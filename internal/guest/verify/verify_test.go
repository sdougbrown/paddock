package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/guest/guesttest"
	"github.com/sdougbrown/paddock/internal/guest/netpolicy"
	"github.com/sdougbrown/paddock/internal/guest/netprobe"
	"github.com/sdougbrown/paddock/internal/guest/sniproxy"
)

func TestRunAndWrite(t *testing.T) {
	checks := []Check{
		{"passes", func(context.Context) (string, error) { return "fine", nil }},
		{"fails", func(context.Context) (string, error) { return "probe", errors.New("broken") }},
		{"panics", func(context.Context) (string, error) { panic("boom") }},
	}
	results := Run(context.Background(), checks)
	var out bytes.Buffer
	if Write(&out, results) {
		t.Fatal("Write reported all passed")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	wantPrefix := []string{"PASS passes: fine (", "FAIL fails: probe: broken (", "FAIL panics: check panicked: boom"}
	if len(lines) != len(wantPrefix) {
		t.Fatalf("got %d lines:\n%s", len(lines), out.String())
	}
	for i, p := range wantPrefix {
		if !strings.HasPrefix(lines[i], p) {
			t.Errorf("line %d = %q; want prefix %q", i, lines[i], p)
		}
	}
	if !Write(&bytes.Buffer{}, results[:1]) {
		t.Fatal("Write reported failure for an all-pass list")
	}
}

func testResolver() guesttest.Resolver {
	// allowed.test resolves the way /etc/hosts makes it resolve.
	return guesttest.Resolver{"allowed.test": {netpolicy.HostsAddr}}
}

// dnsView is the agent's resolver: names in hosts resolve, and any other
// lookup fails without an answer while DNS is blocked, or gets "no such
// host" back from a resolver when it is open.
type dnsView struct {
	hosts guesttest.Resolver
	open  bool
}

func (v dnsView) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if _, ok := v.hosts[host]; ok || v.open {
		return v.hosts.LookupNetIP(ctx, network, host)
	}
	return nil, &net.DNSError{Err: "connect: operation not permitted", Name: host, Server: "198.51.100.53:53"}
}

// networkResults runs the probes that need no real kernel or guest files
// (everything but the nft, signal, and file checks) against a simulated
// guest.
func networkResults(t *testing.T, open bool, denyLog bool) map[string]Result {
	t.Helper()
	g := guesttest.StartGuest(t, "allowed.test", testResolver())
	g.Net.Open = open
	o := NetworkOptions{
		AllowedHost: "allowed.test",
		DeniedHost:  "denied.test",
		IPv6Target:  "[2001:db8::1]:443",
		ExternalDNS: "198.51.100.53:53",
		Resolvers:   []netip.Addr{netip.MustParseAddr("198.51.100.54")},
		RawIPTarget: "198.51.100.10",
		Timeout:     3 * time.Second,
		Dialer:      g.Net,
		Resolver:    dnsView{hosts: g.Resolver, open: open},
	}
	if denyLog {
		o.DenyLog = g.DenyLog
	}
	var checks []Check
	for _, c := range NetworkChecks(o) {
		if c.Name != "nft-flush-refused" && c.Name != "proxy-kill-refused" && !strings.HasPrefix(c.Name, "write-refused ") {
			checks = append(checks, c)
		}
	}
	out := map[string]Result{}
	for _, r := range Run(context.Background(), checks) {
		out[r.Name] = r
	}
	return out
}

var probeChecks = []string{
	"pinned-dns-refused", "unlisted-names-unresolvable", "denied-host-refused", "raw-ip-refused",
	"raw-ip-other-port-refused", "ipv6-refused", "external-dns-refused",
}

func TestNetworkChecksEnforced(t *testing.T) {
	got := networkResults(t, false, true)
	for _, name := range append(probeChecks, "allowed-host-connects") {
		r, ok := got[name]
		if !ok {
			t.Fatalf("missing check %s", name)
		}
		if !r.Pass {
			t.Errorf("%s failed: %s", name, r.Detail)
		}
	}
	if d := got["denied-host-refused"].Detail; !strings.Contains(d, "denial logged") {
		t.Errorf("denied-host detail %q does not report the denial entry", d)
	}
}

func TestParentDomain(t *testing.T) {
	for host, want := range map[string]string{
		"":                "",
		"example.com":     "example.com",
		"api.example.com": "example.com",
		"a.b.example.com": "b.example.com",
	} {
		if got := parentDomain(host); got != want {
			t.Errorf("parentDomain(%q) = %q; want %q", host, got, want)
		}
	}
}

// closedDialer models a guest with no proxy: the firewall rejects every
// connection, including the tcp/443 the allowlist ruleset would redirect.
type closedDialer struct{ open *guesttest.Net }

func (d closedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.open != nil {
		return d.open.DialContext(ctx, network, addr)
	}
	if network == "udp" {
		return nil, &net.OpError{Op: "write", Net: network, Err: syscall.EPERM}
	}
	return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.EHOSTUNREACH}
}

// closedResults runs the closed-network checks that need no real kernel or
// guest files against a simulated guest.
func closedResults(t *testing.T, open bool) (names []string, results map[string]Result) {
	t.Helper()
	g := guesttest.StartGuest(t, "allowed.test", testResolver())
	g.Net.Open = open
	o := NetworkOptions{
		IPv6Target:  "[2001:db8::1]:443",
		ExternalDNS: "198.51.100.53:53",
		Resolvers:   []netip.Addr{netip.MustParseAddr("198.51.100.54")},
		RawIPTarget: "198.51.100.10",
		Timeout:     3 * time.Second,
		Dialer:      closedDialer{},
		Resolver:    dnsView{hosts: g.Resolver, open: open},
	}
	if open {
		o.Dialer = closedDialer{open: g.Net}
	}
	var checks []Check
	for _, c := range ClosedNetworkChecks(o) {
		names = append(names, c.Name)
		if c.Name != "nft-flush-refused" && !strings.HasPrefix(c.Name, "write-refused ") {
			checks = append(checks, c)
		}
	}
	results = map[string]Result{}
	for _, r := range Run(context.Background(), checks) {
		results[r.Name] = r
	}
	return names, results
}

var closedProbeChecks = []string{
	"egress-refused", "ipv6-refused", "external-dns-refused", "pinned-dns-refused", "unlisted-names-unresolvable",
}

func TestClosedNetworkChecksEnforced(t *testing.T) {
	names, got := closedResults(t, false)
	want := []string{
		"nft-flush-refused", "egress-refused", "ipv6-refused", "external-dns-refused",
		"write-refused /etc/hosts", "write-refused " + netpolicy.ResolvConfPath, "write-refused /etc/nsswitch.conf",
		"pinned-dns-refused", "unlisted-names-unresolvable",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("checks = %v; want %v", names, want)
	}
	for _, name := range closedProbeChecks {
		r, ok := got[name]
		if !ok {
			t.Fatalf("missing check %s", name)
		}
		if !r.Pass {
			t.Errorf("%s failed: %s", name, r.Detail)
		}
	}
}

func TestClosedNetworkChecksDetectOpenBoundary(t *testing.T) {
	_, got := closedResults(t, true)
	for _, name := range closedProbeChecks {
		if got[name].Pass {
			t.Errorf("%s passed with the firewall open: %s", name, got[name].Detail)
		}
	}
}

func TestNetworkChecksDetectOpenBoundary(t *testing.T) {
	got := networkResults(t, true, false)
	for _, name := range probeChecks {
		if got[name].Pass {
			t.Errorf("%s passed with the firewall open: %s", name, got[name].Detail)
		}
	}
}

// An allowed host that resolves anywhere but the /etc/hosts address did not
// take the agent's path, even if TLS to it works.
func TestAllowedHostMustResolveThroughHosts(t *testing.T) {
	g := guesttest.StartGuest(t, "allowed.test", guesttest.Resolver{"allowed.test": {netip.MustParseAddr("203.0.113.10")}})
	o := NetworkOptions{AllowedHost: "allowed.test", Timeout: 3 * time.Second, Dialer: g.Net, Resolver: g.Resolver}.withDefaults()
	if d, err := o.allowedHostConnects(context.Background()); err == nil || !strings.Contains(err.Error(), "/etc/hosts") {
		t.Fatalf("detail %q, err %v; want a failure naming /etc/hosts", d, err)
	}
}

func TestWriteDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	open := filepath.Join(dir, "open")
	if err := os.WriteFile(open, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeDenied(open); err == nil {
		t.Error("writable file passed")
	}
	// A read-only file in a writable directory can still be replaced.
	ro := filepath.Join(dir, "ro")
	if err := os.WriteFile(ro, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := writeDenied(ro); err == nil {
		t.Error("replaceable file passed")
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	// Its owner can chmod the file or the directory back to writable.
	if d, err := writeDenied(ro); err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("file and directory owned by the caller: %s: err = %v", d, err)
	}
	if got := readFileString(t, ro); got != "x" {
		t.Errorf("file changed: %q", got)
	}
	if _, err := writeDenied(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file passed")
	}
}

func TestWriteDeniedNotOwned(t *testing.T) {
	const hosts = "/etc/hosts"
	st, err := os.Stat(hosts)
	if err != nil {
		t.Skip(err)
	}
	if uid := st.Sys().(*syscall.Stat_t).Uid; os.Geteuid() == 0 || int(uid) == os.Geteuid() {
		t.Skip("needs a non-root caller that does not own " + hosts)
	}
	if d, err := writeDenied(hosts); err != nil {
		t.Errorf("%s: %s: %v", hosts, d, err)
	}
	// A link to it in the caller's directory can be replaced: by a new
	// file while the directory is writable, and after a chmod once not.
	dir := t.TempDir()
	link := filepath.Join(dir, "hosts")
	if err := os.Symlink(hosts, link); err != nil {
		t.Fatal(err)
	}
	if _, err := writeDenied(link); !errors.Is(err, errUnexpectedSuccess) {
		t.Errorf("writable directory: err = %v; want %v", err, errUnexpectedSuccess)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if _, err := writeDenied(link); err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("directory owned by the caller: err = %v", err)
	}
}

func readFileString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDeniedHostNeedsLogEntry(t *testing.T) {
	g := guesttest.StartGuest(t, "allowed.test", testResolver())
	empty := t.TempDir() + "/other.jsonl"
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	o := NetworkOptions{DeniedHost: "denied.test", DenyLog: empty, Timeout: 3 * time.Second, Dialer: g.Net, Resolver: g.Resolver}.withDefaults()
	if _, err := o.deniedHostRefused(context.Background()); err == nil {
		t.Fatal("denied-host check passed without a denial entry")
	}
}

// A denial entry from before the probe ran does not corroborate its refusal.
func TestDenialMustBeRecent(t *testing.T) {
	g := guesttest.StartGuest(t, "allowed.test", testResolver())
	stale := filepath.Join(t.TempDir(), "stale.jsonl")
	appendDenial := func(d sniproxy.Denial) {
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(stale, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-5 * time.Second)
	appendDenial(sniproxy.Denial{Time: old, SNI: "denied.test", Reason: sniproxy.ReasonDenied})
	// The raw-IP entry needs the probe's own local address, so the dialer
	// writes it, backdated, as soon as the connection exists.
	dialer := guesttest.DialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := g.Net.DialContext(ctx, network, addr)
		if err == nil {
			appendDenial(sniproxy.Denial{Time: old, Peer: c.LocalAddr().String(), Reason: sniproxy.ReasonNoSNI})
		}
		return c, err
	})
	o := NetworkOptions{DeniedHost: "denied.test", RawIPTarget: "198.51.100.10", DenyLog: stale, Timeout: 3 * time.Second, Dialer: dialer}.withDefaults()
	if d, err := o.deniedHostRefused(context.Background()); err == nil || !strings.Contains(err.Error(), " in "+stale) {
		t.Errorf("denied host with a stale entry: %s: err = %v; want no denial", d, err)
	}
	if d, err := o.rawIPRefused(context.Background()); err == nil || !strings.Contains(err.Error(), " in "+stale) {
		t.Errorf("raw IP with a stale entry: %s: err = %v; want no denial", d, err)
	}
}

// A recent no-SNI entry for another connection does not corroborate this
// probe's refusal.
func TestRawIPDenialMustMatchPeer(t *testing.T) {
	g := guesttest.StartGuest(t, "allowed.test", testResolver())
	other := filepath.Join(t.TempDir(), "other.jsonl")
	b, err := json.Marshal(sniproxy.Denial{Time: time.Now(), Peer: "192.0.2.99:40000", Reason: sniproxy.ReasonNoSNI})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	o := NetworkOptions{RawIPTarget: "198.51.100.10", DenyLog: other, Timeout: 3 * time.Second, Dialer: g.Net}.withDefaults()
	if d, err := o.rawIPRefused(context.Background()); err == nil || !strings.Contains(err.Error(), " in "+other) {
		t.Errorf("entry for another peer: %s: err = %v; want no denial", d, err)
	}
}

func TestProxyKillRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may signal any process")
	}
	o := NetworkOptions{ProxyPID: 1}
	if _, err := o.proxyKillRefused(context.Background()); err != nil {
		t.Fatalf("signal to pid 1 as non-root: %v", err)
	}
	o.ProxyPID = os.Getpid()
	if _, err := o.proxyKillRefused(context.Background()); err == nil {
		t.Fatal("check passed for a process the caller can signal")
	}
	o.ProxyPID = 0
	if _, err := o.proxyKillRefused(context.Background()); err == nil {
		t.Fatal("check passed with no proxy pid")
	}
}

func TestRawIPNeedsLogEntry(t *testing.T) {
	g := guesttest.StartGuest(t, "allowed.test", testResolver())
	o := NetworkOptions{RawIPTarget: "198.51.100.10", DenyLog: g.DenyLog, Timeout: 3 * time.Second, Dialer: g.Net}.withDefaults()
	if d, err := o.rawIPRefused(context.Background()); err != nil || !strings.Contains(d, "denial logged") {
		t.Fatalf("with the proxy's log: %s: %v", d, err)
	}
	// A refusal the proxy did not log came from somewhere else.
	empty := t.TempDir() + "/other.jsonl"
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	o.DenyLog = empty
	if _, err := o.rawIPRefused(context.Background()); err == nil {
		t.Fatal("raw-ip check passed without a no-sni denial entry")
	}
}

// A probe that times out was never refused, so it cannot pass.
func TestRefusalProbesTimeoutIsInconclusive(t *testing.T) {
	hang := guesttest.DialFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: ctx.Err()}
	})
	refuse := guesttest.DialFunc(func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EHOSTUNREACH}
	})
	o := NetworkOptions{DeniedHost: "denied.test", Timeout: 100 * time.Millisecond, Dialer: hang}.withDefaults()
	if _, err := o.connectRefused(context.Background(), "198.51.100.10:80"); !errors.Is(err, errInconclusive) {
		t.Errorf("hanging connect: err = %v; want inconclusive", err)
	}
	o.Dialer = refuse
	if d, err := o.connectRefused(context.Background(), "198.51.100.10:80"); err != nil {
		t.Errorf("refused connect: %s: %v", d, err)
	}
	// A socket deadline, as when a dropped SYN is never answered, is a
	// net.Error timeout that wraps no context error.
	o.Dialer = guesttest.DialFunc(func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	})
	if _, err := o.connectRefused(context.Background(), "198.51.100.10:80"); !errors.Is(err, errInconclusive) {
		t.Errorf("connect past its deadline: err = %v; want inconclusive", err)
	}

	// A UDP server that never answers: the query is sent and times out.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	o.Dialer = &net.Dialer{}
	if _, err := o.dnsQueryRefused(context.Background(), pc.LocalAddr().String()); !errors.Is(err, errInconclusive) {
		t.Errorf("unanswered DNS query: err = %v; want inconclusive", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.Dialer = hang
	if _, err := o.deniedHostRefused(ctx); !errors.Is(err, errInconclusive) {
		t.Errorf("cancelled denied-host probe: err = %v; want inconclusive", err)
	}
}

func TestUnlistedNamesNeedRefusedSend(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantPass bool
	}{
		{"firewall refused", &net.DNSError{Err: "write udp 192.168.64.2:4000->192.168.64.1:53: write: operation not permitted", Name: "x"}, true},
		{"no route", &net.DNSError{Err: "dial udp 192.168.64.1:53: connect: network is unreachable", Name: "x"}, true},
		{"nxdomain", &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}, false},
		{"servfail", &net.DNSError{Err: "server misbehaving", Name: "x", IsTemporary: true}, false},
		{"refused rcode", &net.DNSError{Err: "server misbehaving", Name: "x"}, false},
		{"port unreachable", &net.DNSError{Err: "read udp 192.168.64.2:4000->192.168.64.1:53: read: connection refused", Name: "x"}, false},
		{"remote network unreachable", &net.DNSError{Err: "read udp 192.168.64.2:4000->192.168.64.1:53: read: network is unreachable", Name: "x"}, false},
		{"timeout", &net.DNSError{Err: "i/o timeout", Name: "x", IsTimeout: true}, false},
		{"context", context.DeadlineExceeded, false},
		{"other", errors.New("resolver broke"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := NetworkOptions{UnlistedNames: []string{"x.example.invalid"}, Resolver: failingResolver{tt.err}}.withDefaults()
			d, err := o.unlistedNamesUnresolvable(context.Background())
			if (err == nil) != tt.wantPass {
				t.Errorf("pass = %v (%s: %v); want %v", err == nil, d, err, tt.wantPass)
			}
		})
	}
}

type failingResolver struct{ err error }

func (r failingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, r.err
}

func TestFlushRefused(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name, body string
		timeout    time.Duration
		wantErr    error
	}{
		{"refused", "echo 'Operation not permitted' >&2; exit 1", 10 * time.Second, nil},
		{"flushed", "exit 0", 10 * time.Second, errUnexpectedSuccess},
		{"killed", "kill -9 $$", 10 * time.Second, errInconclusive},
		{"timed out", "exec sleep 5", 300 * time.Millisecond, errInconclusive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := flushRefused(context.Background(), script(strings.ReplaceAll(tt.name, " ", "-"), tt.body), tt.timeout)
			if tt.wantErr == nil && err != nil || tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("%s: err = %v; want %v", d, err, tt.wantErr)
			}
		})
	}
}

// Only an error the guest itself raises shows the traffic was refused. A
// refusal from the far end, or a failure unrelated to the network, means
// the packet may have left or the probe never ran.
func TestRefusalNeedsLocalError(t *testing.T) {
	failWith := func(errno syscall.Errno) guesttest.DialFunc {
		return func(_ context.Context, network, _ string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: network, Err: errno}
		}
	}
	for _, tt := range []struct {
		errno    syscall.Errno
		connect  bool
		dnsQuery bool
	}{
		{syscall.EHOSTUNREACH, true, false},
		{syscall.EACCES, true, true},
		{syscall.EPERM, true, true},
		{syscall.ENETUNREACH, true, true},
		{syscall.ECONNREFUSED, false, false},
		{syscall.EMFILE, false, false},
	} {
		t.Run(tt.errno.Error(), func(t *testing.T) {
			o := NetworkOptions{Timeout: time.Second, Dialer: failWith(tt.errno)}.withDefaults()
			check := func(name string, want bool, d string, err error) {
				t.Helper()
				if want && err != nil || !want && !errors.Is(err, errNotRefused) {
					t.Errorf("%s: %s: err = %v; want pass %v", name, d, err, want)
				}
			}
			d, err := o.connectRefused(context.Background(), "198.51.100.10:80")
			check("connect", tt.connect, d, err)
			d, err = o.dnsQueryRefused(context.Background(), "198.51.100.53:53")
			check("dns query", tt.dnsQuery, d, err)
		})
	}

	// An error on the read after a successful send came back as an ICMP
	// error, which a remote router sends too.
	o := NetworkOptions{Timeout: time.Second, Dialer: guesttest.DialFunc(func(context.Context, string, string) (net.Conn, error) {
		c, peer := net.Pipe()
		t.Cleanup(func() { peer.Close() })
		go func() { _, _ = io.Copy(io.Discard, peer) }()
		return readFailConn{c, &net.OpError{Op: "read", Net: "udp", Err: syscall.ENETUNREACH}}, nil
	})}.withDefaults()
	if d, err := o.dnsQueryRefused(context.Background(), "198.51.100.53:53"); !errors.Is(err, errNotRefused) {
		t.Errorf("read failed after the send: %s: err = %v; want %v", d, err, errNotRefused)
	}

	// A UDP port with nothing behind it answers with port unreachable.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	o = NetworkOptions{Timeout: time.Second}.withDefaults()
	if d, err := o.dnsQueryRefused(context.Background(), addr); !errors.Is(err, errNotRefused) {
		t.Errorf("closed UDP port: %s: err = %v; want %v", d, err, errNotRefused)
	}
}

type readFailConn struct {
	net.Conn
	err error
}

func (c readFailConn) Read([]byte) (int, error) { return 0, c.err }

// A handshake probe passes only when the connection closes during the
// handshake, as the proxy closes it, or is refused locally. A TLS alert
// comes from a server that read the ClientHello.
func TestHandshakeRefusalNeedsClose(t *testing.T) {
	serve := func(handle func(net.Conn)) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				handle(c)
				c.Close()
			}
		}()
		return ln.Addr().String()
	}
	alert := serve(func(c net.Conn) {
		buf := make([]byte, 4096)
		_, _ = c.Read(buf)
		// A fatal handshake_failure alert record.
		_, _ = c.Write([]byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x28})
	})
	closes := serve(func(c net.Conn) {
		buf := make([]byte, 4096)
		_, _ = c.Read(buf)
	})
	to := func(addr string) guesttest.DialFunc {
		return func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	}
	for _, tt := range []struct {
		name   string
		dialer netprobe.Dialer
		pass   bool
	}{
		{"alert", to(alert), false},
		{"closed", to(closes), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := NetworkOptions{DeniedHost: "denied.test", RawIPTarget: "198.51.100.10", Timeout: 3 * time.Second, Dialer: tt.dialer}.withDefaults()
			for name, probe := range map[string]func(context.Context) (string, error){
				"denied host": o.deniedHostRefused,
				"raw ip":      o.rawIPRefused,
			} {
				d, err := probe(context.Background())
				if tt.pass && err != nil || !tt.pass && !errors.Is(err, errNotRefused) {
					t.Errorf("%s: %s: err = %v; want pass %v", name, d, err, tt.pass)
				}
			}
		})
	}
}
