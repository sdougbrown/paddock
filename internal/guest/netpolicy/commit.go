package netpolicy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/sdougbrown/paddock/internal/guest/netprobe"
)

// CommitOptions supplies the steps Commit sequences.
type CommitOptions struct {
	// Apply loads a ruleset. Nil means Apply (nft -f -).
	Apply func(ctx context.Context, ruleset string) error
	// StartProxy starts the SNI proxy listening on Policy.ProxyAddr as
	// Policy.ProxyUID. It runs while deny-all is in force. Required for
	// mode allowlist.
	StartProxy func(ctx context.Context) error
	// Lifeline checks the committed allowlist from inside the guest.
	// Required for mode allowlist.
	Lifeline func(ctx context.Context) error
}

// Commit puts p into force and returns nil only when it holds.
//
// For mode allowlist the order is: deny-all, StartProxy, the allowlist
// ruleset, Lifeline. Each ruleset load is atomic, so the guest is never open
// between steps. If any step fails, Commit loads deny-all again and returns
// the error; the caller must not start the agent. Mode none loads deny-all.
// Mode host loads nothing. Every deny-all load ignores ctx's cancellation:
// deny-all is the state a cancelled commit must end in.
//
// Commit must run before any other guest process opens a connection.
// `flush ruleset` keeps conntrack entries, so a connection opened earlier
// would keep passing the established-flow rules, past the tcp/443 DNAT.
func Commit(ctx context.Context, p Policy, o CommitOptions) error {
	apply := o.Apply
	if apply == nil {
		apply = Apply
	}
	switch p.Mode {
	case ModeHost:
		return nil
	case ModeNone:
		if err := apply(context.WithoutCancel(ctx), DenyAll()); err != nil {
			return fmt.Errorf("netpolicy: apply deny-all: %w", err)
		}
		return nil
	case ModeAllowlist:
	default:
		_, err := Ruleset(p)
		return failClosed(ctx, apply, err)
	}
	if o.StartProxy == nil || o.Lifeline == nil {
		return failClosed(ctx, apply, errors.New("netpolicy: allowlist commit needs StartProxy and Lifeline"))
	}
	rules, err := Ruleset(p)
	if err != nil {
		return failClosed(ctx, apply, err)
	}
	if err := apply(context.WithoutCancel(ctx), DenyAll()); err != nil {
		return fmt.Errorf("netpolicy: apply deny-all: %w", err)
	}
	if err := o.StartProxy(ctx); err != nil {
		return failClosed(ctx, apply, fmt.Errorf("netpolicy: start proxy: %w", err))
	}
	if err := apply(ctx, rules); err != nil {
		return failClosed(ctx, apply, fmt.Errorf("netpolicy: apply allowlist: %w", err))
	}
	if err := o.Lifeline(ctx); err != nil {
		return failClosed(ctx, apply, fmt.Errorf("netpolicy: lifeline: %w", err))
	}
	return nil
}

// failClosed loads deny-all and returns cause, joined with any error from
// the deny-all load itself.
func failClosed(ctx context.Context, apply func(context.Context, string) error, cause error) error {
	if err := apply(context.WithoutCancel(ctx), DenyAll()); err != nil {
		return errors.Join(cause, fmt.Errorf("netpolicy: fail-closed deny-all also failed: %w", err))
	}
	return cause
}

// Lifeline is the post-commit check of an allowlist policy, run from inside
// the guest as root. Root has no DNS under the allowlist ruleset, so the
// probes take the agent's path: the allowed host resolves through /etc/hosts
// to HostsAddr and completes a TLS handshake through the proxy, while a
// denied server name and a ClientHello without one, sent to the same
// address, are refused, and a DNS query to each pinned resolver gets no
// answer.
type Lifeline struct {
	// AllowedHost is an exact allowlisted name, listed in /etc/hosts.
	AllowedHost string
	// DeniedHost is a name that is not allowlisted.
	DeniedHost string
	// HostsAddr is the address /etc/hosts maps allowlisted names to; zero
	// means the package's HostsAddr.
	HostsAddr netip.Addr
	// Resolvers are the pinned DNS servers, queried on port 53.
	Resolvers []netip.Addr
	// Timeout bounds each probe; zero means 10s.
	Timeout time.Duration
	// Dialer defaults to the net package's.
	Dialer netprobe.Dialer
}

// Check runs the probes and returns every failure.
func (l Lifeline) Check(ctx context.Context) error {
	if l.AllowedHost == "" || l.DeniedHost == "" {
		return errors.New("lifeline needs an allowed and a denied host")
	}
	if len(l.Resolvers) == 0 {
		return errors.New("lifeline needs the pinned resolvers")
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	hosts := l.HostsAddr
	if !hosts.IsValid() {
		hosts = HostsAddr
	}
	// connects expects a handshake and a certificate naming sni; refused
	// expects no handshake at all.
	connects := func(addr, sni string) error {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return netprobe.TLS(pctx, l.Dialer, addr, sni)
	}
	refused := func(addr, sni string) bool {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_, err := netprobe.Handshake(pctx, l.Dialer, addr, sni)
		return err != nil
	}
	var errs []error
	if err := connects(hostPort(l.AllowedHost), l.AllowedHost); err != nil {
		if errors.Is(err, netprobe.ErrClosedInHandshake) {
			err = fmt.Errorf("%w (the proxy refused it or is not serving; check the denial log and that the proxy runs as the proxy uid)", err)
		}
		errs = append(errs, fmt.Errorf("allowed host %s did not connect: %w", l.AllowedHost, err))
	}
	if !refused(hostPort(hosts.String()), l.DeniedHost) {
		errs = append(errs, fmt.Errorf("denied host %s connected", l.DeniedHost))
	}
	if !refused(hostPort(hosts.String()), "") {
		errs = append(errs, fmt.Errorf("raw-IP connection to %s:443 without SNI connected", hosts))
	}
	for _, r := range l.Resolvers {
		addr := netip.AddrPortFrom(r, 53).String()
		qctx, cancel := context.WithTimeout(ctx, min(timeout, 3*time.Second))
		n, err := netprobe.DNSQuery(qctx, l.Dialer, addr, "example.com")
		cancel()
		if err == nil {
			errs = append(errs, fmt.Errorf("DNS query to %s was answered (%d bytes); only the proxy uid may query it", addr, n))
		}
	}
	// A cancelled context fails every later probe, and a failed probe
	// reads as a refusal, so the refusals above prove nothing.
	if err := ctx.Err(); err != nil {
		errs = append(errs, fmt.Errorf("lifeline interrupted: %w", err))
	}
	return errors.Join(errs...)
}

func hostPort(host string) string {
	return net.JoinHostPort(host, strconv.Itoa(443))
}
