package bwrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/plan"
)

// fakeBwrap returns a fake bwrap CLI that logs its argv and dumps the child
// environment to a file, then exits with the status in FAKE_EXIT.
func fakeBwrap(t *testing.T) (bin string, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := "#!/bin/sh\nd=\"${0%/*}\"\n[ \"$1\" = __warm ] && exit 0\nprintf '%s ' \"$@\" >> \"$d/argv\"\necho >> \"$d/argv\"\nenv > \"$d/env\"\nexit ${FAKE_EXIT:-0}\n"
	bin = filepath.Join(dir, "bwrap")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(bin, "__warm").Run(); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func testPlan(home, ws string) *plan.Plan {
	return &plan.Plan{
		Engine:    "bwrap",
		Workspace: ws,
		Workdir:   ws,
		Mounts:    []plan.Mount{{Host: ws, Guest: ws}},
		Network:   plan.Network{Policy: "none"},
		Env:       []plan.EnvVar{{Name: "TOKEN", Source: plan.SourceEnv, Ref: "BWRAP_TEST_TOKEN"}},
		Argv:      []string{"claude", "-p"},
	}
}

func TestRunStagesHomeAndDeliversSecretsOffArgv(t *testing.T) {
	bin, dir := fakeBwrap(t)
	home := t.TempDir()
	ws := filepath.Join(home, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BWRAP_TEST_TOKEN", "s3cr3t-value")
	e := &Engine{Bin: bin, Home: home, StateRoot: filepath.Join(home, "state")}
	code, err := e.Run(t.Context(), testPlan(home, ws), engine.Options{})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v", code, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "argv"))
	argv := string(b)
	for _, want := range []string{"--unshare-net", "--bind ", "--chdir " + ws, "claude -p"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q lacks %q", argv, want)
		}
	}
	if strings.Contains(argv, "s3cr3t-value") {
		t.Errorf("the secret value leaked into the argv: %q", argv)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	if !strings.Contains(string(env), "TOKEN=s3cr3t-value") {
		t.Errorf("the secret value never reached the child env: %q", env)
	}
	if !strings.Contains(string(env), "HOME=") || !strings.Contains(string(env), filepath.Join(home, "state", "runs")) {
		t.Errorf("HOME must point at the throwaway home: %q", env)
	}
	// The throwaway home is seeded: onboarding marker, transcript dir.
	runs, _ := filepath.Glob(filepath.Join(home, "state", "runs", "paddock-*", "home", ".claude.json"))
	if len(runs) != 1 {
		t.Fatalf("seeded .claude.json not found in the throwaway home: %v", runs)
	}
}

func TestRunAllowlistFailsClosed(t *testing.T) {
	bin, _ := fakeBwrap(t)
	home := t.TempDir()
	ws := filepath.Join(home, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Bin: bin, Home: home, StateRoot: filepath.Join(home, "state")}
	p := testPlan(home, ws)
	p.Network = plan.Network{Policy: "allowlist", Hosts: []string{"api.anthropic.com"}}
	if _, err := e.Run(t.Context(), p, engine.Options{}); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("allowlist plan: err = %v; want a fail-closed error", err)
	}
	if _, err := e.Run(t.Context(), testPlan(home, ws), engine.Options{Verify: true}); err == nil || !strings.Contains(err.Error(), "verify") {
		t.Errorf("verify: err = %v; want an unsupported error", err)
	}
}

func TestForwardSignalsDeliverToTheChild(t *testing.T) {
	dir := t.TempDir()
	// The fake traps TERM, logs it, and exits 0.
	script := "#!/bin/sh\nd=\"${0%/*}\"\n[ \"$1\" = __warm ] && exit 0\ntrap 'echo TERM >> \"$d/signals\"; exit 0' TERM\nsleep 30 >/dev/null 2>&1 </dev/null &\nwait\n"
	bin := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(bin, "__warm").Run(); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Bin: bin, Home: dir, StateRoot: filepath.Join(dir, "state")}
	ws := filepath.Join(dir, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	p := testPlan(dir, ws)
	p.Argv = []string{"sleep", "30"}
	p.Env = nil
	done := make(chan int, 1)
	go func() {
		code, err := e.Run(ctx, p, engine.Options{})
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- code
	}()
	// Wait for the trap to be installed; bwrap execs instantly, so a short
	// sleep is enough, and the assertion below retries anyway.
	time.Sleep(300 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d; want 0 after the trapped TERM", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "signals"))
	if !strings.Contains(string(b), "TERM") {
		t.Errorf("the forwarded TERM never reached the child: %q", b)
	}
}
