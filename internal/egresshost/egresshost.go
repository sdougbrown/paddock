// Package egresshost defines the egress host names paddock accepts in an
// allowlist. It is a leaf package shared by spec loading, plan resolution,
// and the in-guest SNI proxy, so every layer applies the same rule.
//
// An egress host is one exact DNS name. There are no wildcards: under the
// allowlist policy the agent resolves names only through /etc/hosts, which
// cannot express a wildcard, and a wildcard would let the agent make the
// proxy look up names of its choosing. Names are compared lowercased and
// without one trailing dot.
package egresshost

import (
	"fmt"
	"net/netip"
	"strings"
)

// reservedTLDs are special-use names (RFC 6761) that never resolve on the
// internet. "localhost" in /etc/hosts would also take loopback's name, and
// paddock-init uses a name under "invalid" as the lifeline's denied host, so
// neither can be allowlisted.
var reservedTLDs = []string{"invalid", "localhost"}

// Normalize lowercases name and drops one trailing dot.
func Normalize(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".")
}

// Validate reports whether host is an acceptable egress host.
func Validate(host string) error {
	n := Normalize(host)
	if strings.ContainsAny(n, "*?") {
		return fmt.Errorf("egress host %q: wildcards are not supported; list each host exactly", host)
	}
	if _, err := netip.ParseAddr(n); err == nil {
		return fmt.Errorf("egress host %q: IP addresses are not allowed; the proxy matches TLS server names", host)
	}
	if !Valid(n) {
		return fmt.Errorf("egress host %q is not a hostname", host)
	}
	tld := n[strings.LastIndexByte(n, '.')+1:]
	// No real top-level domain is all digits. Rejecting such names catches
	// IPv4 forms that netip refuses (01.2.3.4, 999.1.1.1, 3232235521).
	if strings.Trim(tld, "0123456789") == "" {
		return fmt.Errorf("egress host %q: IP addresses are not allowed; the proxy matches TLS server names", host)
	}
	for _, r := range reservedTLDs {
		if tld == r {
			return fmt.Errorf("egress host %q: %q is a reserved name that never resolves", host, "."+r)
		}
	}
	return nil
}

// Valid reports whether name, already normalized, is a hostname: lowercase
// LDH labels (plus "_", which appears in real service names), each 1 to 63
// bytes, at most 253 bytes in all.
func Valid(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}
