// Package netpolicy builds and applies the guest's nftables ruleset.
//
// The ruleset is one inet table named "paddock", loaded with `nft -f -` as a
// single atomic transaction that starts with `flush ruleset`, so the guest
// moves between whole policies and is never left half-configured.
//
// Policy modes:
//
//   - "allowlist": input and output default to drop for IPv4 and IPv6.
//     Loopback and established flows pass. DNS (udp and tcp 53) is open to
//     the proxy uid only, and only to the IPv4 resolvers from
//     /etc/resolv.conf: every other uid, root included, has no DNS, so a
//     lookup cannot carry data out of the guest. Port 53 from any other uid
//     is rejected before the established-flow rule: conntrack does not
//     record which socket owns a flow, so a UDP query from a port the proxy
//     just used would otherwise match the proxy's entry and pass. Every IPv4 tcp/443 connection from any uid other
//     than the proxy uid is DNAT'd to the proxy's listen address (the guest's
//     own interface address; Apple container mounts /proc/sys read-only, so
//     route_localnet is unavailable and a 127.0.0.0/8 target would
//     blackhole). Only the proxy uid may open real tcp/443 connections; the
//     proxy only dials addresses an allowlisted name resolves to. Everything
//     else ends in a reject, so callers fail fast instead of timing out; the
//     chain policy stays drop.
//   - "none": loopback only; other output is rejected.
//   - "host": no rules; the guest keeps the engine's network unchanged.
//
// Under "allowlist", other uids reach allowlisted names through /etc/hosts,
// which maps each exact allowlisted host to HostsAddr. HostsAddr is routed
// out of the guest interface, so the nat chain's loopback exemption does not
// apply and the tcp/443 DNAT sends the connection to the proxy. The proxy
// reads the name from the ClientHello and resolves it itself.
//
// A raw-IP tcp/443 connection is DNAT'd to the proxy like any other; the
// proxy refuses it because a TLS client sends no server_name for an IP
// literal. Raw-IP connections to any other port hit the reject rule.
//
// Commit sequences the allowlist mode fail-closed: deny-all first, then the
// proxy starts and is checked to run as the proxy uid, then the allowlist
// ruleset, then the lifeline check, and on any failure back to deny-all.
package netpolicy

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/sdougbrown/paddock/internal/guest/sniproxy"
)

// Mode is a network policy mode.
type Mode string

// Policy modes, matching the profile's network.policy values.
const (
	ModeAllowlist Mode = "allowlist"
	ModeNone      Mode = "none"
	ModeHost      Mode = "host"
)

// TableName is the nftables table paddock owns.
const TableName = "paddock"

// HostsAddr is the address /etc/hosts gives every allowlisted name. It is in
// TEST-NET-1 (RFC 5737), so it is never a real destination: nothing answers
// it, and only the tcp/443 DNAT to the proxy makes a connection to it work.
// It must not be an address of the guest itself, which the kernel routes
// over loopback, past the DNAT.
var HostsAddr = netip.AddrFrom4([4]byte{192, 0, 2, 1})

// Policy is the input to ruleset generation.
type Policy struct {
	Mode Mode
	// ProxyUID is the uid the SNI proxy runs as. It is exempt from the
	// tcp/443 DNAT and is the only uid allowed to open tcp/443 itself.
	ProxyUID int
	// ProxyAddr is the proxy's listen address: an IPv4 address on a guest
	// interface (not loopback) and the proxy port.
	ProxyAddr netip.AddrPort
	// Resolvers are the only DNS servers the proxy uid may query; IPv4
	// only. No other uid may query any.
	Resolvers []netip.Addr
}

// Ruleset returns the nftables ruleset text for p. Mode "host" returns an
// empty string: there is nothing to load.
func Ruleset(p Policy) (string, error) {
	switch p.Mode {
	case ModeHost:
		return "", nil
	case ModeNone:
		return DenyAll(), nil
	case ModeAllowlist:
		return allowlist(p)
	}
	return "", fmt.Errorf("netpolicy: unknown policy mode %q (want allowlist, none, or host)", p.Mode)
}

// DenyAll returns the loopback-only ruleset. It is both policy "none" and
// the fail-closed state Commit falls back to.
func DenyAll() string {
	var b strings.Builder
	b.WriteString("flush ruleset\n")
	fmt.Fprintf(&b, "table inet %s {\n", TableName)
	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority filter; policy drop;\n")
	b.WriteString("\t\tiif \"lo\" accept\n")
	b.WriteString("\t}\n")
	writeForward(&b)
	b.WriteString("\tchain output {\n")
	b.WriteString("\t\ttype filter hook output priority filter; policy drop;\n")
	b.WriteString("\t\toif \"lo\" accept\n")
	b.WriteString("\t\treject with icmpx admin-prohibited\n")
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String()
}

// allowlist returns the allowlist ruleset. guesttest.Net simulates what it
// lets the agent reach, for tests that run without nft: IPv4 tcp/443 goes to
// the proxy and every other connection is refused. A change to that routing
// must change guesttest.Net with it.
func allowlist(p Policy) (string, error) {
	if err := p.validateAllowlist(); err != nil {
		return "", err
	}
	proxyIP := p.ProxyAddr.Addr().String()
	port := p.ProxyAddr.Port()

	var b strings.Builder
	b.WriteString("flush ruleset\n")
	fmt.Fprintf(&b, "table inet %s {\n", TableName)

	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority filter; policy drop;\n")
	b.WriteString("\t\tiif \"lo\" accept\n")
	b.WriteString("\t\tct state established,related accept\n")
	b.WriteString("\t}\n")

	writeForward(&b)

	b.WriteString("\tchain output {\n")
	b.WriteString("\t\ttype filter hook output priority filter; policy drop;\n")
	b.WriteString("\t\toif \"lo\" accept\n")
	// Before the established accept: see the package doc.
	fmt.Fprintf(&b, "\t\tmeta skuid != %d udp dport 53 reject with icmpx admin-prohibited\n", p.ProxyUID)
	fmt.Fprintf(&b, "\t\tmeta skuid != %d tcp dport 53 reject with icmpx admin-prohibited\n", p.ProxyUID)
	b.WriteString("\t\tct state established,related accept\n")
	for _, r := range p.Resolvers {
		addr := r.Unmap().String()
		fmt.Fprintf(&b, "\t\tmeta skuid %d ip daddr %s udp dport 53 accept\n", p.ProxyUID, addr)
		fmt.Fprintf(&b, "\t\tmeta skuid %d ip daddr %s tcp dport 53 accept\n", p.ProxyUID, addr)
	}
	// A DNAT'd packet keeps the output interface it was first routed to
	// (eth0), so the loopback rule above does not match it; this rule does.
	fmt.Fprintf(&b, "\t\tip daddr %s tcp dport %d accept\n", proxyIP, port)
	fmt.Fprintf(&b, "\t\tmeta skuid %d tcp dport 443 accept\n", p.ProxyUID)
	b.WriteString("\t\treject with icmpx admin-prohibited\n")
	b.WriteString("\t}\n")

	// The symbolic priority "dstnat" is defined for the prerouting hook only;
	// -100 is its value and is the correct priority for nat on output.
	b.WriteString("\tchain nat_output {\n")
	b.WriteString("\t\ttype nat hook output priority -100; policy accept;\n")
	b.WriteString("\t\toif \"lo\" accept\n")
	fmt.Fprintf(&b, "\t\tmeta skuid %d accept\n", p.ProxyUID)
	fmt.Fprintf(&b, "\t\ttcp dport 443 dnat ip to %s:%d\n", proxyIP, port)
	b.WriteString("\t}\n")

	b.WriteString("}\n")
	return b.String(), nil
}

func writeForward(b *strings.Builder) {
	b.WriteString("\tchain forward {\n")
	b.WriteString("\t\ttype filter hook forward priority filter; policy drop;\n")
	b.WriteString("\t}\n")
}

func (p Policy) validateAllowlist() error {
	if p.ProxyUID <= 0 {
		return fmt.Errorf("netpolicy: proxy uid must be a non-root uid, got %d", p.ProxyUID)
	}
	a := p.ProxyAddr.Addr()
	if !p.ProxyAddr.IsValid() || p.ProxyAddr.Port() == 0 {
		return fmt.Errorf("netpolicy: proxy address %q is not a valid address and port", p.ProxyAddr)
	}
	if err := checkProxyAddr(a); err != nil {
		return err
	}
	if p.ProxyAddr.Port() == 443 {
		return fmt.Errorf("netpolicy: proxy port must not be 443, the port it intercepts")
	}
	if len(p.Resolvers) == 0 {
		return fmt.Errorf("netpolicy: allowlist policy needs at least one DNS resolver")
	}
	for _, r := range p.Resolvers {
		if !sniproxy.UsableResolver(r) {
			return fmt.Errorf("netpolicy: resolver address %q is not a usable IPv4 address", r)
		}
	}
	return nil
}

// checkProxyAddr reports whether a can be a DNAT target for the proxy: an
// IPv4 unicast address that is not loopback.
func checkProxyAddr(a netip.Addr) error {
	a = a.Unmap()
	switch {
	case !a.Is4():
		return fmt.Errorf("netpolicy: proxy address %s is not IPv4", a)
	case a.IsLoopback():
		return fmt.Errorf("netpolicy: proxy address %s is loopback; DNAT to 127.0.0.0/8 needs route_localnet, which the guest cannot set", a)
	case a.IsUnspecified(), a.IsMulticast(), a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return fmt.Errorf("netpolicy: proxy address %s is not a unicast address", a)
	}
	return nil
}
