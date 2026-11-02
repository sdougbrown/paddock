package bwrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
			`cp "$HOME/.claude.json" home-proof.txt; echo ok > wrote.txt; [ -n "$TOKEN" ] && printf '%s' "$TOKEN" > token.txt; timeout 2 bash -c 'exec 3<>/dev/tcp/example.com/443' 2>/dev/null && echo network-up > net.txt || echo no-network > net.txt`},
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
	// The throwaway home is seeded; the run copied the marker into the
	// workspace before cleanup removed the home.
	if b, err := os.ReadFile(filepath.Join(ws, "home-proof.txt")); err != nil || !strings.Contains(string(b), "hasCompletedOnboarding") {
		t.Errorf("seeded .claude.json = %q, %v", b, err)
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

// TestBwrapRealTerm forwards a TERM to a real bwrap run: the exit code is
// 128+SIGTERM, settling empirically that the forwarding path works against
// real bubblewrap.
func TestBwrapRealTerm(t *testing.T) {
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
		Argv:      []string{"sh", "-c", `echo ready > ready.txt; sleep 30 >/dev/null 2>&1 </dev/null`},
	}
	ctx, cancel := context.WithCancel(t.Context())
	codeCh := make(chan int, 1)
	go func() {
		code, err := e.Run(ctx, p, engine.Options{})
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		codeCh <- code
	}()
	// The agent signals ready from inside the namespace; the TERM goes to
	// the paddock process, whose forwarder delivers it to bwrap.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(ws, "ready.txt")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent never signalled ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-codeCh:
		if code != 143 {
			t.Errorf("exit code = %d; want 143 (128+SIGTERM)", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run never returned after cancellation")
	}
}
