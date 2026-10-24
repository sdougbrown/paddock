package sniproxy

import (
	"errors"
	"fmt"
	"slices"

	"github.com/sdougbrown/paddock/internal/egresshost"
)

// Allowlist matches TLS server names against exact egress hosts (see
// package egresshost). Matching is case-insensitive and ignores one
// trailing dot.
type Allowlist struct {
	hosts map[string]bool
}

// NewAllowlist compiles hosts. An empty list is an error: a proxy that
// allows nothing is a misconfiguration, not a policy (use network policy
// "none" for that).
func NewAllowlist(hosts []string) (*Allowlist, error) {
	if len(hosts) == 0 {
		return nil, errors.New("sniproxy: allowlist is empty")
	}
	a := &Allowlist{hosts: map[string]bool{}}
	for _, h := range hosts {
		if err := egresshost.Validate(h); err != nil {
			return nil, fmt.Errorf("sniproxy: allowlist: %w", err)
		}
		a.hosts[egresshost.Normalize(h)] = true
	}
	return a, nil
}

// Allows reports whether name is allowlisted.
func (a *Allowlist) Allows(name string) bool {
	return a.hosts[egresshost.Normalize(name)]
}

// Hosts returns the normalized hosts, sorted.
func (a *Allowlist) Hosts() []string {
	out := make([]string, 0, len(a.hosts))
	for h := range a.hosts {
		out = append(out, h)
	}
	slices.Sort(out)
	return out
}
