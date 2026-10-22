package netpolicy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
)

// ResolvConfPath is the guest's resolver configuration.
const ResolvConfPath = "/etc/resolv.conf"

// ParseResolvConf returns the IPv4 nameserver addresses listed in r, in
// order, without duplicates. Lines that are not valid nameserver entries
// are skipped.
//
// IPv6 nameservers are skipped too. The allowlist ruleset passes no ICMPv6
// neighbour discovery, so an IPv6 resolver can stop answering once its
// neighbour entry expires, and a link-local one needs its zone to be
// dialed. Apple container lists a single IPv4 resolver, the host gateway.
func ParseResolvConf(r io.Reader) ([]netip.Addr, error) {
	var out []netip.Addr
	seen := map[netip.Addr]bool{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(f[1])
		if err != nil {
			continue
		}
		a = a.Unmap()
		if !a.Is4() || a.IsUnspecified() || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out, sc.Err()
}

// ReadResolvConf parses the resolv.conf at path and errors if it lists no
// IPv4 nameserver.
func ReadResolvConf(path string) ([]netip.Addr, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("netpolicy: %w", err)
	}
	defer f.Close()
	addrs, err := ParseResolvConf(f)
	if err != nil {
		return nil, fmt.Errorf("netpolicy: read %s: %w", path, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("netpolicy: %s lists no IPv4 nameserver", path)
	}
	return addrs, nil
}

// RouteSource returns the IPv4 address the guest uses as the source for
// traffic to dst: the address of the interface its route goes out of. It
// connects a UDP socket, which selects a route without sending a packet.
func RouteSource(dst netip.Addr) (netip.Addr, error) {
	dst = dst.Unmap()
	if !dst.Is4() {
		return netip.Addr{}, fmt.Errorf("netpolicy: route source needs an IPv4 destination, got %s", dst)
	}
	c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(dst, 53)))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("netpolicy: find route to %s: %w", dst, err)
	}
	defer c.Close()
	src := c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	if err := checkProxyAddr(src); err != nil {
		return netip.Addr{}, err
	}
	return src, nil
}

// ProxyAddrFor picks the proxy listen address for the given resolvers: the
// route source toward the first resolver, on port.
func ProxyAddrFor(resolvers []netip.Addr, port uint16) (netip.AddrPort, error) {
	if len(resolvers) == 0 {
		return netip.AddrPort{}, fmt.Errorf("netpolicy: no resolver to route toward")
	}
	src, err := RouteSource(resolvers[0])
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(src, port), nil
}
