package netpolicy

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func basePolicy() Policy {
	return Policy{
		Mode:      ModeAllowlist,
		ProxyUID:  970,
		ProxyAddr: netip.MustParseAddrPort("192.168.64.48:15443"),
		Resolvers: []netip.Addr{netip.MustParseAddr("192.168.64.1")},
	}
}

func TestRulesetGolden(t *testing.T) {
	two := basePolicy()
	two.Resolvers = []netip.Addr{netip.MustParseAddr("192.168.64.1"), netip.MustParseAddr("10.0.0.2")}
	none := basePolicy()
	none.Mode = ModeNone

	tests := []struct {
		name   string
		policy Policy
	}{
		{"allowlist", basePolicy()},
		{"allowlist-two-resolvers", two},
		{"none", none},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Ruleset(tt.policy)
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", tt.name+".nft")
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("ruleset mismatch (run with -update to accept)\n--- got\n%s\n--- want\n%s", got, want)
			}
		})
	}
}

func TestRulesetHostIsEmpty(t *testing.T) {
	p := basePolicy()
	p.Mode = ModeHost
	got, err := Ruleset(p)
	if err != nil || got != "" {
		t.Fatalf("Ruleset(host) = %q, %v; want empty, nil", got, err)
	}
}

func TestRulesetFailsClosedShape(t *testing.T) {
	for _, mode := range []Mode{ModeAllowlist, ModeNone} {
		p := basePolicy()
		p.Mode = mode
		got, err := Ruleset(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(got, "flush ruleset\n") {
			t.Errorf("%s: ruleset does not start with flush ruleset", mode)
		}
		for _, hook := range []string{"input", "forward", "output"} {
			want := "type filter hook " + hook + " priority filter; policy drop;"
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q", mode, want)
			}
		}
	}
}

func TestRulesetInvalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Policy)
		want   string
	}{
		{"unknown mode", func(p *Policy) { p.Mode = "open" }, "unknown policy mode"},
		{"root proxy uid", func(p *Policy) { p.ProxyUID = 0 }, "non-root uid"},
		{"loopback proxy", func(p *Policy) { p.ProxyAddr = netip.MustParseAddrPort("127.0.0.1:15443") }, "route_localnet"},
		{"ipv6 proxy", func(p *Policy) { p.ProxyAddr = netip.MustParseAddrPort("[fd00::2]:15443") }, "not IPv4"},
		{"unspecified proxy", func(p *Policy) { p.ProxyAddr = netip.MustParseAddrPort("0.0.0.0:15443") }, "not a unicast"},
		{"zero port", func(p *Policy) { p.ProxyAddr = netip.MustParseAddrPort("192.168.64.48:0") }, "not a valid address"},
		{"port 443", func(p *Policy) { p.ProxyAddr = netip.MustParseAddrPort("192.168.64.48:443") }, "must not be 443"},
		{"no resolvers", func(p *Policy) { p.Resolvers = nil }, "at least one DNS resolver"},
		{"bad resolver", func(p *Policy) { p.Resolvers = []netip.Addr{{}} }, "not a usable IPv4"},
		{"ipv6 resolver", func(p *Policy) { p.Resolvers = []netip.Addr{netip.MustParseAddr("fd00::1")} }, "not a usable IPv4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := basePolicy()
			tt.mutate(&p)
			_, err := Ruleset(p)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v; want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseResolvConf(t *testing.T) {
	in := `# generated
search example.internal
nameserver 192.168.64.1
nameserver 192.168.64.1
nameserver fe80::1%eth0 ; link-local
nameserver fd00::1
nameserver not-an-ip
nameserver 0.0.0.0
options ndots:1
nameserver ::ffff:10.0.0.2
`
	got, err := ParseResolvConf(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Addr{
		netip.MustParseAddr("192.168.64.1"),
		netip.MustParseAddr("10.0.0.2"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestReadResolvConfNeedsIPv4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver fd00::1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadResolvConf(path); err == nil || !strings.Contains(err.Error(), "no IPv4 nameserver") {
		t.Fatalf("err = %v; want no IPv4 nameserver", err)
	}
}

// outputChain returns the trimmed rules of the allowlist output chain.
func outputChain(t *testing.T, p Policy) []string {
	t.Helper()
	got, err := Ruleset(p)
	if err != nil {
		t.Fatal(err)
	}
	_, chain, ok := strings.Cut(got, "\tchain output {\n")
	if !ok {
		t.Fatal("no output chain")
	}
	chain, _, _ = strings.Cut(chain, "\t}\n")
	var rules []string
	for _, line := range strings.Split(chain, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rules = append(rules, line)
		}
	}
	return rules
}

// TestRulesetDNSOnlyForProxy guards against DNS tunnelling: every rule that
// opens port 53 is limited to the proxy uid, and port 53 from any other uid
// is rejected.
func TestRulesetDNSOnlyForProxy(t *testing.T) {
	p := basePolicy()
	p.Resolvers = []netip.Addr{netip.MustParseAddr("192.168.64.1"), netip.MustParseAddr("10.0.0.2")}
	accepts, rejects := 0, 0
	for _, rule := range outputChain(t, p) {
		if !strings.Contains(rule, "dport 53") {
			continue
		}
		switch {
		case strings.HasPrefix(rule, "meta skuid 970 ") && strings.HasSuffix(rule, " accept"):
			accepts++
		case strings.HasPrefix(rule, "meta skuid != 970 ") && strings.Contains(rule, " reject"):
			rejects++
		default:
			t.Errorf("DNS rule neither limited to the proxy uid nor a reject for other uids: %q", rule)
		}
	}
	if accepts != 4 || rejects != 2 {
		t.Errorf("found %d proxy DNS accepts and %d DNS rejects; want 4 and 2", accepts, rejects)
	}
}

// TestRulesetRejectsOtherDNSBeforeEstablished: conntrack keeps the proxy's
// UDP query flows established for a while after the proxy closes their
// sockets. Another uid that binds a freed source port matches such an
// entry, so its port-53 traffic must be rejected before the established
// accept.
func TestRulesetRejectsOtherDNSBeforeEstablished(t *testing.T) {
	rules := outputChain(t, basePolicy())
	pos := func(want string) int {
		for i, r := range rules {
			if r == want {
				return i
			}
		}
		t.Fatalf("output chain has no %q:\n%s", want, strings.Join(rules, "\n"))
		return -1
	}
	est := pos("ct state established,related accept")
	for _, proto := range []string{"udp", "tcp"} {
		if r := pos("meta skuid != 970 " + proto + " dport 53 reject with icmpx admin-prohibited"); r > est {
			t.Errorf("%s port-53 reject (rule %d) comes after the established accept (rule %d)", proto, r, est)
		}
	}
}
