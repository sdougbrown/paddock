package bwrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/plan"
)

// TestBwrapReal runs a plan through real bubblewrap: workspace writes, the
// seeded throwaway home, secrets in the child env, and no network under the
// none policy. It needs bwrap on PATH; set PADDOCK_BWRAP_TESTS=1.
func TestBwrapReal(t *testing.T) {
	if os.Getenv("PADDOCK_BWRAP_TESTS") != "1" {
		t.Skip("set PADDOCK_BWRAP_TESTS=1 to run plans through real bubblewrap")
	}
	home := t.TempDir()
	ws := filepath.Join(home, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Home: home, StateRoot: filepath.Join(home, "state")}
	p := &plan.Plan{
		Engine:    "bwrap",
		Workspace: ws,
		Workdir:   ws,
		Mounts:    []plan.Mount{{Host: ws, Guest: ws}},
		Network:   plan.Network{Policy: "none"},
		Env:       []plan.EnvVar{{Name: "TOKEN", Source: plan.SourceEnv, Ref: "BWRAP_REAL_TOKEN"}},
		Argv: []string{"bash", "-c",
			`echo ok > wrote.txt; [ -n "$TOKEN" ] && printf '%s' "$TOKEN" > token.txt; timeout 2 bash -c 'exec 3<>/dev/tcp/example.com/443' 2>/dev/null && echo network-up > net.txt || echo no-network > net.txt`},
	}
	t.Setenv("BWRAP_REAL_TOKEN", "real-secret")
	code, err := e.Run(t.Context(), p, engine.Options{})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v", code, err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "wrote.txt")); err != nil || string(b) != "ok\n" {
		t.Errorf("wrote.txt = %q, %v; the agent could not write the workspace", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "token.txt")); err != nil || string(b) != "real-secret" {
		t.Errorf("token.txt = %q, %v; the secret value never reached the child env", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "net.txt")); err != nil || string(b) != "no-network\n" {
		t.Errorf("net.txt = %q, %v; policy none must refuse egress", b, err)
	}
	// The throwaway home is seeded and the transcript dir exists at the
	// host path the caller derives.
	runs, _ := filepath.Glob(filepath.Join(home, "state", "runs", "paddock-*", "home", ".claude.json"))
	if len(runs) != 1 {
		t.Errorf("seeded .claude.json not found: %v", runs)
	}
	tdir := filepath.Join(home, ".claude", "projects")
	if _, err := os.Stat(tdir); err != nil {
		t.Errorf("transcript dir missing at the host path: %v", err)
	}
	// The secrets artifact does not survive the run.
	secrets, _ := filepath.Glob(filepath.Join(home, "state", "runs", "paddock-*", "secrets", "*"))
	if len(secrets) != 0 {
		t.Errorf("secrets artifact left behind: %v", secrets)
	}
}
