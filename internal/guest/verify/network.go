package verify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/sdougbrown/paddock/internal/guest/netpolicy"
	"github.com/sdougbrown/paddock/internal/guest/netprobe"
	"github.com/sdougbrown/paddock/internal/guest/sniproxy"
)

// NetworkOptions configures NetworkChecks.
type NetworkOptions struct {
	// AllowedHost is an exact allowlisted name.
	AllowedHost string
	// DeniedHost is a resolvable name that is not allowlisted.
	DeniedHost string
	// DenyLog is the proxy's denial log. When set, the denied-host and
	// raw-IP checks also require a matching denial entry. The log is
	// guest-writable, so an entry corroborates the refusal the probe saw;
	// it does not prove the refusal.
	DenyLog string
	// ProxyPID is the SNI proxy's pid.
	ProxyPID int
	// IPv6Target is dialed to show IPv6 egress is refused; default
	// "[2606:4700:4700::1111]:443".
	IPv6Target string
	// ExternalDNS is a resolver other than the pinned one, queried to show
	// DNS is pinned; default "1.1.1.1:53".
	ExternalDNS string
	// HostsAddr is the address /etc/hosts maps allowlisted names to;
	// default netpolicy.HostsAddr.
	HostsAddr netip.Addr
	// Resolvers are the pinned DNS servers, which only the proxy uid may
	// query. When empty, pinned-dns-refused reads the nameservers from
	// /etc/resolv.conf as it runs, so a missing or empty file fails that
	// check with its cause; withDefaults cannot report an error.
	Resolvers []netip.Addr
	// RawIPTarget is a routable IPv4 address dialed without a name to show
	// raw-IP egress is refused; default "1.1.1.1".
	RawIPTarget string
	// UnlistedNames must not resolve; default a random name under
	// example.invalid and, when AllowedHost is set, one under its parent
	// domain.
	UnlistedNames []string
	// ProtectedFiles hold the guest's name resolution and must refuse the
	// agent's writes; default /etc/hosts, /etc/resolv.conf, and
	// /etc/nsswitch.conf.
	ProtectedFiles []string
	// Timeout bounds each probe; default 10s.
	Timeout time.Duration
	// Dialer and Resolver default to the net package's.
	Dialer   netprobe.Dialer
	Resolver netprobe.Resolver
}

func (o NetworkOptions) withDefaults() NetworkOptions {
	if o.IPv6Target == "" {
		o.IPv6Target = "[2606:4700:4700::1111]:443"
	}
	if o.ExternalDNS == "" {
		o.ExternalDNS = "1.1.1.1:53"
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Dialer == nil {
		o.Dialer = &net.Dialer{}
	}
	if o.Resolver == nil {
		o.Resolver = net.DefaultResolver
	}
	if !o.HostsAddr.IsValid() {
		o.HostsAddr = netpolicy.HostsAddr
	}
	if o.RawIPTarget == "" {
		o.RawIPTarget = "1.1.1.1"
	}
	if o.UnlistedNames == nil {
		o.UnlistedNames = []string{randomLabel() + ".example.invalid"}
		if o.AllowedHost != "" {
			o.UnlistedNames = append(o.UnlistedNames, randomLabel()+"."+parentDomain(o.AllowedHost))
		}
	}
	if o.ProtectedFiles == nil {
		o.ProtectedFiles = []string{"/etc/hosts", netpolicy.ResolvConfPath, "/etc/nsswitch.conf"}
	}
	return o
}

// randomLabel is a DNS label no resolver can have cached.
func randomLabel() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "paddock-" + hex.EncodeToString(b[:])
}

// parentDomain drops the first label of a name of three or more labels and
// returns any shorter name unchanged.
func parentDomain(host string) string {
	if _, rest, ok := strings.Cut(host, "."); ok && strings.Contains(rest, ".") {
		return rest
	}
	return host
}

// NetworkChecks returns the network checks for policy "allowlist", in run
// order. The firewall tamper check runs first so that a firewall the agent
// managed to flush shows up in every later check too.
//
// A raw-IP connection to tcp/443 is DNAT'd to the proxy like any other; it
// is refused because a TLS client dialing an IP literal sends no SNI. The
// agent has no DNS: allowlisted names resolve through /etc/hosts, and every
// other name fails without a query leaving the guest.
func NetworkChecks(o NetworkOptions) []Check {
	o = o.withDefaults()
	checks := []Check{
		{"nft-flush-refused", o.nftFlushRefused},
		{"proxy-kill-refused", o.proxyKillRefused},
	}
	checks = append(checks, o.dnsChecks()...)
	return append(checks,
		Check{"denied-host-refused", o.deniedHostRefused},
		Check{"raw-ip-refused", o.rawIPRefused},
		Check{"raw-ip-other-port-refused", o.rawIPOtherPortRefused},
		Check{"ipv6-refused", o.ipv6Refused},
		Check{"external-dns-refused", o.externalDNSRefused},
		Check{"allowed-host-connects", o.allowedHostConnects},
	)
}

// dnsChecks are the checks that the agent cannot resolve names on its own or
// change how names resolve.
func (o NetworkOptions) dnsChecks() []Check {
	var checks []Check
	for _, f := range o.ProtectedFiles {
		checks = append(checks, Check{"write-refused " + f, func(context.Context) (string, error) {
			return writeDenied(f)
		}})
	}
	return append(checks,
		Check{"pinned-dns-refused", o.pinnedDNSRefused},
		Check{"unlisted-names-unresolvable", o.unlistedNamesUnresolvable},
	)
}

var (
	errUnexpectedSuccess = errors.New("succeeded; the boundary is open")
	errInconclusive      = errors.New("inconclusive: the probe timed out or was cancelled instead of being refused")
)

// inconclusive reports whether err ended a probe by running out of time or
// by cancellation. The paddock rulesets reject blocked traffic at once, so
// a probe that times out was never refused: its packet may have left and
// the answer been lost on the way back.
func inconclusive(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.As(err, &ne) && ne.Timeout()
}

// refusal returns the result of a probe that must fail: err is a pass
// unless it is inconclusive.
func refusal(detail string, err error) (string, error) {
	if inconclusive(err) {
		return detail, errInconclusive
	}
	return detail, nil
}

func (o NetworkOptions) nftFlushRefused(ctx context.Context) (string, error) {
	nft, err := netpolicy.NFTPath()
	if err != nil {
		return "", err
	}
	return flushRefused(ctx, nft, o.Timeout)
}

// flushRefused runs `nft flush ruleset` and passes only when nft ran to a
// normal exit with a non-zero status. A kill by signal or by the timeout
// shows nothing about the agent's permissions.
func flushRefused(ctx context.Context, nft string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, nft, "flush", "ruleset")
	cmd.Env = []string{}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return "nft flush ruleset", errUnexpectedSuccess
	}
	var ee *exec.ExitError
	if ctx.Err() == nil && errors.As(err, &ee) && ee.Exited() && ee.ExitCode() > 0 {
		return "nft flush ruleset failed: " + firstLine(out), nil
	}
	return "nft flush ruleset", fmt.Errorf("did not exit normally (%v); %w", err, errInconclusive)
}

func (o NetworkOptions) proxyKillRefused(context.Context) (string, error) {
	if o.ProxyPID <= 0 {
		return "", errors.New("proxy pid unknown")
	}
	// Signal 0 runs the kernel's permission check without delivering a
	// signal, so a passing run leaves the proxy untouched.
	err := syscall.Kill(o.ProxyPID, 0)
	switch {
	case errors.Is(err, syscall.EPERM):
		return fmt.Sprintf("signal to proxy pid %d: operation not permitted", o.ProxyPID), nil
	case errors.Is(err, syscall.ESRCH):
		return "", fmt.Errorf("proxy pid %d is not running", o.ProxyPID)
	case err == nil:
		return fmt.Sprintf("signal to proxy pid %d", o.ProxyPID), errUnexpectedSuccess
	}
	return "", fmt.Errorf("signal proxy pid %d: %w", o.ProxyPID, err)
}

// deniedHostRefused passes when the denied name's handshake is refused. With
// DenyLog set it also requires a recent denial entry for that name. The log
// is guest-writable, so the entry corroborates the refusal; it does not
// prove the proxy made it.
func (o NetworkOptions) deniedHostRefused(ctx context.Context) (string, error) {
	if o.DeniedHost == "" {
		return "", errors.New("no denied host configured")
	}
	// The agent cannot resolve the denied host, so the probe sends its name
	// as SNI to the address allowlisted names resolve to.
	since := time.Now().Add(-time.Second)
	addr := net.JoinHostPort(o.HostsAddr.String(), "443")
	_, err := o.handshake(ctx, addr, o.DeniedHost)
	if err == nil {
		return "TLS to " + addr + " with SNI " + o.DeniedHost, errUnexpectedSuccess
	}
	detail := fmt.Sprintf("SNI %s via %s refused (%v)", o.DeniedHost, addr, err)
	if inconclusive(err) {
		return detail, errInconclusive
	}
	return o.requireDenial(detail, fmt.Sprintf("%q denial for %s", sniproxy.ReasonDenied, o.DeniedHost), func(d sniproxy.Denial) bool {
		return d.SNI == o.DeniedHost && d.Reason == sniproxy.ReasonDenied && !d.Time.Before(since)
	})
}

// rawIPRefused sends a ClientHello without SNI to a raw IP. With DenyLog
// set it also requires the proxy's no-SNI denial for this connection, so a
// target that refuses no-SNI hellos itself cannot pass for the proxy. The
// log is guest-writable, so the entry corroborates the refusal; it does not
// prove the proxy made it.
func (o NetworkOptions) rawIPRefused(ctx context.Context) (string, error) {
	since := time.Now().Add(-time.Second)
	addr := net.JoinHostPort(o.RawIPTarget, "443")
	local, err := o.handshake(ctx, addr, "")
	if err == nil {
		return "TLS without SNI to " + addr, errUnexpectedSuccess
	}
	detail := fmt.Sprintf("TLS without SNI to %s refused (%v)", addr, err)
	if inconclusive(err) {
		return detail, errInconclusive
	}
	return o.requireDenial(detail, fmt.Sprintf("%q denial for %s", sniproxy.ReasonNoSNI, local), func(d sniproxy.Denial) bool {
		return local != "" && d.Peer == local && d.Reason == sniproxy.ReasonNoSNI && !d.Time.Before(since)
	})
}

// requireDenial passes when DenyLog is unset or holds an entry that match
// accepts; want describes that entry. The entry corroborates the refusal
// the probe already observed. The log is guest-writable, so a match is not
// proof that the proxy refused the connection.
func (o NetworkOptions) requireDenial(detail, want string, match func(sniproxy.Denial) bool) (string, error) {
	if o.DenyLog == "" {
		return detail, nil
	}
	ok, err := hasDenial(o.DenyLog, match)
	if err != nil {
		return detail, fmt.Errorf("read denial log: %w", err)
	}
	if !ok {
		return detail, fmt.Errorf("no %s in %s", want, o.DenyLog)
	}
	return detail + "; denial logged", nil
}

func (o NetworkOptions) rawIPOtherPortRefused(ctx context.Context) (string, error) {
	return o.connectRefused(ctx, net.JoinHostPort(o.RawIPTarget, "80"))
}

func (o NetworkOptions) ipv6Refused(ctx context.Context) (string, error) {
	return o.connectRefused(ctx, o.IPv6Target)
}

func (o NetworkOptions) connectRefused(ctx context.Context, addr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	c, err := o.Dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return refusal(fmt.Sprintf("connect %s refused (%v)", addr, err), err)
	}
	c.Close()
	return "connect " + addr, errUnexpectedSuccess
}

func (o NetworkOptions) externalDNSRefused(ctx context.Context) (string, error) {
	return o.dnsQueryRefused(ctx, o.ExternalDNS)
}

// dnsQueryRefused passes when a UDP query to addr is refused.
func (o NetworkOptions) dnsQueryRefused(ctx context.Context, addr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, min(o.Timeout, 3*time.Second))
	defer cancel()
	n, err := netprobe.DNSQuery(ctx, o.Dialer, addr, "example.com")
	if err != nil {
		return refusal(fmt.Sprintf("no answer from %s (%v)", addr, err), err)
	}
	return fmt.Sprintf("%s answered with %d bytes", addr, n), errUnexpectedSuccess
}

// pinnedDNSRefused passes when neither a UDP query nor a TCP connection to
// any pinned resolver gets through.
func (o NetworkOptions) pinnedDNSRefused(ctx context.Context) (string, error) {
	resolvers := o.Resolvers
	if len(resolvers) == 0 {
		var err error
		if resolvers, err = netpolicy.ReadResolvConf(netpolicy.ResolvConfPath); err != nil {
			return "", err
		}
	}
	var details []string
	for _, r := range resolvers {
		addr := netip.AddrPortFrom(r, 53).String()
		d, err := o.dnsQueryRefused(ctx, addr)
		if err != nil {
			return d, err
		}
		details = append(details, "udp: "+d)
		if d, err = o.connectRefused(ctx, addr); err != nil {
			return d, err
		}
		details = append(details, "tcp: "+d)
	}
	return strings.Join(details, "; "), nil
}

// unlistedNamesUnresolvable passes when every unlisted name fails to resolve
// because the guest refused to send the query. Any answer from a resolver,
// "no such host", SERVFAIL, or REFUSED alike, means the query left the guest
// and so could have carried data out.
func (o NetworkOptions) unlistedNamesUnresolvable(ctx context.Context) (string, error) {
	var details []string
	for _, name := range o.UnlistedNames {
		lctx, cancel := context.WithTimeout(ctx, o.Timeout)
		addrs, err := o.Resolver.LookupNetIP(lctx, "ip", name)
		cancel()
		detail := fmt.Sprintf("%s: %v", name, err)
		switch {
		case err == nil:
			return fmt.Sprintf("%s resolved to %v", name, addrs), errUnexpectedSuccess
		case inconclusive(err):
			return detail, errInconclusive
		case !sendRefused(err):
			return detail, errors.New("the lookup did not fail on a refused send; a resolver may have answered")
		}
		details = append(details, detail)
	}
	return strings.Join(details, "; "), nil
}

// localRefusals are the errors a DNS query send gets when the guest's
// firewall rejects it (EPERM, EACCES) or no route exists (ENETUNREACH).
var localRefusals = []syscall.Errno{syscall.EPERM, syscall.EACCES, syscall.ENETUNREACH}

// sendRefused reports whether err is a lookup failure caused by a refused
// send. The net package's resolver keeps the socket error of a failed
// exchange only as the text of DNSError.Err, so the errno is matched there.
func sendRefused(err error) bool {
	var de *net.DNSError
	if !errors.As(err, &de) || de.IsNotFound || de.IsTimeout {
		return false
	}
	for _, e := range localRefusals {
		if errors.Is(err, e) || strings.HasSuffix(de.Err, ": "+e.Error()) {
			return true
		}
	}
	return false
}

// allowedHostConnects resolves the allowed host the way the agent does,
// requires the /etc/hosts address and nothing else, and completes TLS to
// it by name. Nothing answers on that address, so a completed handshake
// with a certificate naming the host went through the DNAT and the proxy.
func (o NetworkOptions) allowedHostConnects(ctx context.Context) (string, error) {
	if o.AllowedHost == "" {
		return "", errors.New("no allowed host configured")
	}
	lctx, cancel := context.WithTimeout(ctx, o.Timeout)
	addrs, err := o.Resolver.LookupNetIP(lctx, "ip", o.AllowedHost)
	cancel()
	if err != nil {
		return "resolve " + o.AllowedHost, err
	}
	if len(addrs) != 1 || addrs[0].Unmap() != o.HostsAddr {
		return fmt.Sprintf("%s resolved to %v", o.AllowedHost, addrs), fmt.Errorf("want only %s from /etc/hosts", o.HostsAddr)
	}
	if err := o.tls(ctx, net.JoinHostPort(o.AllowedHost, "443"), o.AllowedHost); err != nil {
		return fmt.Sprintf("%s -> %s; TLS", o.AllowedHost, o.HostsAddr), err
	}
	return fmt.Sprintf("%s -> %s (/etc/hosts); TLS through the proxy completed", o.AllowedHost, o.HostsAddr), nil
}

// tls expects a handshake with a certificate naming sni.
func (o NetworkOptions) tls(ctx context.Context, addr, sni string) error {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	return netprobe.TLS(ctx, o.Dialer, addr, sni)
}

// handshake is for probes that expect refusal: any completed handshake is a
// connection, whatever certificate came back. It returns the local address
// of the TCP connection, if one was made, which is the peer the proxy logs.
func (o NetworkOptions) handshake(ctx context.Context, addr, sni string) (local string, err error) {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	d := &localAddrDialer{d: o.Dialer}
	_, err = netprobe.Handshake(ctx, d, addr, sni)
	return d.local, err
}

// localAddrDialer records the local address of the connection it opens.
type localAddrDialer struct {
	d     netprobe.Dialer
	local string
}

func (l *localAddrDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := l.d.DialContext(ctx, network, addr)
	if err == nil {
		l.local = c.LocalAddr().String()
	}
	return c, err
}

// denialLogLimit caps how much of the denial log hasDenial reads.
const denialLogLimit = 4 << 20

// hasDenial reports whether the first denialLogLimit bytes of the denial log
// at path hold an entry match accepts. Entries past the cap are not read.
// The log is guest-writable, so a match corroborates a refusal the probe
// already observed; it does not prove one.
func hasDenial(path string, match func(sniproxy.Denial) bool) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, denialLogLimit))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		var d sniproxy.Denial
		if json.Unmarshal(sc.Bytes(), &d) != nil {
			continue
		}
		if match(d) {
			return true, nil
		}
	}
	return false, sc.Err()
}

func firstLine(b []byte) string {
	line, _, _ := bytes.Cut(bytes.TrimSpace(b), []byte("\n"))
	return string(line)
}
