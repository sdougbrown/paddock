package pinit

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

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
// and checks that it carries the agent's uid and gid, every read-only mount,
// the writable home and transcript dirs and read-write mounts other than
// paddock's log dir, and the proxy fields only when a proxy runs.
func TestVerifyArgv(t *testing.T) {
	cfg := &initcfg.Config{
		User: initcfg.User{UID: 501, GID: 20, Home: "/Users/a"},
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
			{Path: initcfg.LogDir, ReadOnly: false},
		},
	}
	t.Run("no transcript dir", func(t *testing.T) {
		bare := *cfg
		bare.Claude = initcfg.ClaudeHome{}
		bare.Mounts = nil
		argv, err := verifyArgv(&bare, &network{})
		if err != nil {
			t.Fatal(err)
		}
		var p verifyPlan
		if err := json.Unmarshal([]byte(argv[2]), &p); err != nil {
			t.Fatal(err)
		}
		if want := []string{"/Users/a", "/Users/a/.claude"}; !sameSet(p.Writable, want) {
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
			if p.Policy != initcfg.PolicyAllowlist || p.UID != 501 || p.GID != 20 || p.DenyLog != tt.wantLog {
				t.Errorf("policy %q uid %d gid %d deny log %q", p.Policy, p.UID, p.GID, p.DenyLog)
			}
			if want := []string{"/opt/paddock", "/Users/a/.botfiles/skills"}; !sameSet(p.ReadOnly, want) {
				t.Errorf("ReadOnly = %q; want %q", p.ReadOnly, want)
			}
			if want := []string{"/Users/a", "/Users/a/.claude", "/Users/a/.claude/projects/-ws", "/Users/a/ws"}; !sameSet(p.Writable, want) {
				t.Errorf("Writable = %q; want %q", p.Writable, want)
			}
			if p.ProxyPID != tt.wantPID || p.AllowedHost != tt.wantAllowed || p.DeniedHost != tt.wantDenied {
				t.Errorf("proxy fields: pid %d allowed %q denied %q; want %d %q %q",
					p.ProxyPID, p.AllowedHost, p.DeniedHost, tt.wantPID, tt.wantAllowed, tt.wantDenied)
			}
		})
	}
}

// sameSet reports whether a and b hold the same strings in any order. Each
// listed directory is its own independent check, so order is not part of
// the verify plan's contract.
func sameSet(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
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

func TestVerifyMain(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	plan := func(p verifyPlan) string {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	tests := []struct {
		name     string
		args     []string
		want     int
		wantLine string
	}{
		{"no argument", nil, ExitInit, ""},
		{"two arguments", []string{"{}", "{}"}, ExitInit, ""},
		{"malformed JSON", []string{"{"}, ExitInit, ""},
		{"unknown policy", []string{plan(verifyPlan{Policy: "bogus"})}, 1, `FAIL network: unknown network policy "bogus"`},
		{"missing writable dir", []string{plan(verifyPlan{Policy: initcfg.PolicyHost, Writable: []string{missing}})}, 1, "FAIL writable " + missing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var code int
			out := captureStdout(t, func() { code = verifyMain(tt.args) })
			if code != tt.want {
				t.Errorf("verifyMain = %d, want %d; output %q", code, tt.want, out)
			}
			if tt.wantLine != "" && !strings.Contains(out, tt.wantLine) {
				t.Errorf("output %q; want a line containing %q", out, tt.wantLine)
			}
			if tt.wantLine == "" && out != "" {
				t.Errorf("output %q; want none before the checks run", out)
			}
		})
	}
}

// captureStdout returns what f writes to os.Stdout.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()
	read := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()
	f()
	w.Close()
	return <-read
}

func TestNetworkStop(t *testing.T) {
	(*network)(nil).stop()
	(&network{}).stop()

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	n := &network{proxy: cmd}
	done := make(chan struct{})
	go func() {
		n.stop()
		// A second stop finds the process gone and must not wait again.
		n.stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("stop did not return")
	}
	if cmd.ProcessState == nil {
		t.Fatal("stop did not reap the proxy")
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("proxy ended with %v; want killed by SIGKILL", cmd.ProcessState)
	}
}
