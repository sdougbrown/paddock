package pinit

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/guest/verify"
	"github.com/sdougbrown/paddock/internal/initcfg"
)

func TestLifelineHosts(t *testing.T) {
	tests := []struct {
		hosts           []string
		allowed, errSub string
	}{
		{[]string{"registry.npmjs.org", "api.anthropic.com"}, "api.anthropic.com", ""},
		{[]string{"a.test", "api.anthropic.com"}, "api.anthropic.com", ""},
		{[]string{"z.example.org", "b.test", "A.test"}, "a.test", ""},
		{[]string{"example.com", "example.net", "example.org"}, "example.com", ""},
		{[]string{"*.anthropic.com"}, "", "wildcards are not supported"},
		{nil, "", "allowlist is empty"},
	}
	for _, tt := range tests {
		a, d, err := lifelineHosts(tt.hosts)
		if tt.errSub != "" {
			if err == nil || !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("%v: err = %v; want containing %q", tt.hosts, err, tt.errSub)
			}
			continue
		}
		if err != nil || a != tt.allowed || d != lifelineDeniedHost {
			t.Errorf("%v: got %q %q %v; want %q %q", tt.hosts, a, d, err, tt.allowed, lifelineDeniedHost)
		}
	}
	// The denied host must stay refusable whatever the operator allowlists.
	if _, _, err := lifelineHosts([]string{lifelineDeniedHost}); err == nil {
		t.Errorf("%s could be allowlisted", lifelineDeniedHost)
	}
}

// TestVerifyArgv decodes the verify role's argument the way verifyMain does
// and checks that it carries every read-only mount, the writable home and
// transcript dirs, and the proxy fields only when a proxy runs.
func TestVerifyArgv(t *testing.T) {
	cfg := &initcfg.Config{
		User: initcfg.User{UID: 501, Home: "/Users/a"},
		Network: initcfg.Network{
			Policy:  initcfg.PolicyAllowlist,
			DenyLog: "/run/paddock-log/deny.jsonl",
		},
		Claude: initcfg.ClaudeHome{TranscriptDir: "/Users/a/.claude/projects/-ws"},
		Mounts: []initcfg.Mount{
			{Path: "/opt/paddock", ReadOnly: true},
			{Path: "/Users/a/ws", ReadOnly: false},
			{Path: "/Users/a/.botfiles/skills", ReadOnly: true},
			{Path: "/Users/a/.claude/projects/-ws", ReadOnly: false},
		},
	}
	t.Run("no transcript dir", func(t *testing.T) {
		bare := *cfg
		bare.Claude = initcfg.ClaudeHome{}
		argv, err := verifyArgv(&bare, &network{})
		if err != nil {
			t.Fatal(err)
		}
		var p verifyPlan
		if err := json.Unmarshal([]byte(argv[2]), &p); err != nil {
			t.Fatal(err)
		}
		if want := []string{"/Users/a", "/Users/a/.claude"}; !slices.Equal(p.Writable, want) {
			t.Errorf("Writable = %q; want %q", p.Writable, want)
		}
	})
	proxied := &network{proxy: &exec.Cmd{Process: &os.Process{Pid: 4242}}, allowed: "api.anthropic.com", denied: lifelineDeniedHost}
	tests := []struct {
		name                             string
		net                              *network
		wantPID                          int
		wantAllowed, wantDenied, wantLog string
	}{
		{"with proxy", proxied, 4242, "api.anthropic.com", lifelineDeniedHost, "/run/paddock-log/deny.jsonl"},
		{"without proxy", &network{}, 0, "", "", "/run/paddock-log/deny.jsonl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argv, err := verifyArgv(cfg, tt.net)
			if err != nil {
				t.Fatal(err)
			}
			if len(argv) != 3 || argv[0] != initcfg.InitPath || argv[1] != VerifyRole {
				t.Fatalf("argv = %q", argv)
			}
			var p verifyPlan
			if err := json.Unmarshal([]byte(argv[2]), &p); err != nil {
				t.Fatal(err)
			}
			if p.Policy != initcfg.PolicyAllowlist || p.UID != 501 || p.DenyLog != tt.wantLog {
				t.Errorf("policy %q uid %d deny log %q", p.Policy, p.UID, p.DenyLog)
			}
			if want := []string{"/opt/paddock", "/Users/a/.botfiles/skills"}; !slices.Equal(p.ReadOnly, want) {
				t.Errorf("ReadOnly = %q; want %q", p.ReadOnly, want)
			}
			if want := []string{"/Users/a", "/Users/a/.claude", "/Users/a/.claude/projects/-ws"}; !slices.Equal(p.Writable, want) {
				t.Errorf("Writable = %q; want %q", p.Writable, want)
			}
			if p.ProxyPID != tt.wantPID || p.AllowedHost != tt.wantAllowed || p.DeniedHost != tt.wantDenied {
				t.Errorf("proxy fields: pid %d allowed %q denied %q; want %d %q %q",
					p.ProxyPID, p.AllowedHost, p.DeniedHost, tt.wantPID, tt.wantAllowed, tt.wantDenied)
			}
		})
	}
}

func TestNetworkChecksHostAndUnknown(t *testing.T) {
	tests := []struct {
		policy   string
		wantPass bool
		wantSub  string
	}{
		{initcfg.PolicyHost, true, "policy host, no network checks apply"},
		{"bogus", false, `unknown network policy "bogus"`},
		{"", false, "unknown network policy"},
	}
	for _, tt := range tests {
		t.Run(tt.policy, func(t *testing.T) {
			var buf strings.Builder
			results := verify.Run(context.Background(), networkChecks(verifyPlan{Policy: tt.policy}))
			if ok := verify.Write(&buf, results); ok != tt.wantPass {
				t.Errorf("pass = %v, want %v; output %q", ok, tt.wantPass, buf.String())
			}
			if len(results) != 1 || !strings.Contains(buf.String(), tt.wantSub) {
				t.Errorf("results %d, output %q; want one line containing %q", len(results), buf.String(), tt.wantSub)
			}
		})
	}
}
