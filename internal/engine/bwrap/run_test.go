package bwrap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
	"github.com/sdougbrown/paddock/internal/runstage"
)

// fakeBwrap returns a fake bwrap CLI that logs its argv and dumps the child
// environment to a file, then exits with the status in FAKE_EXIT.
func fakeBwrap(t *testing.T) (bin string, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := "#!/bin/sh\nd=\"${0%/*}\"\n[ \"$1\" = __warm ] && exit 0\nmkdir -p \"$d/calls\"\nprintf '%s ' \"$@\" > \"$d/calls/$$.log\"\nenv > \"$d/env\"\ncp -a \"$HOME/\" \"$d/claude-dump\" 2>/dev/null || true\nexit ${FAKE_EXIT:-0}\n"
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

// envLines splits an env dump into full lines, so assertions cannot match a
// suffix of a longer variable's value.
func envLines(t *testing.T, dump string) []string {
	t.Helper()
	return strings.Split(strings.TrimRight(dump, "\n"), "\n")
}

func hasEnvLine(lines []string, kv string) bool {
	for _, l := range lines {
		if l == kv {
			return true
		}
	}
	return false
}

func TestRunStagesHomeAndDeliversSecretsOffArgv(t *testing.T) {
	bin, dir := fakeBwrap(t)
	home := t.TempDir()
	ws := filepath.Join(home, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BWRAP_TEST_TOKEN", "s3cr3t-value")
	seedFile := filepath.Join(home, "CLAUDE.md")
	if err := os.WriteFile(seedFile, []byte("context"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := testPlan(home, ws)
	p.Claude = plan.ClaudePlan{
		ManagedSettings: []byte(`{"deny":true}`),
		SeedFiles:       []string{seedFile},
	}
	e := &Engine{Bin: bin, Home: home, StateRoot: filepath.Join(home, "state")}
	code, err := e.Run(t.Context(), p, engine.Options{})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v", code, err)
	}
	var argv string
	for _, f := range argvLogs(t, dir) {
		argv += f + " "
	}
	for _, want := range []string{"--unshare-net", "--bind ", "--chdir " + ws, "claude -p"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q lacks %q", argv, want)
		}
	}
	if strings.Contains(argv, "s3cr3t-value") {
		t.Errorf("the secret value leaked into the argv: %q", argv)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	lines := envLines(t, string(env))
	if !hasEnvLine(lines, "TOKEN=s3cr3t-value") {
		t.Errorf("the resolved secret never reached the child env as its own line: %q", lines)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "BWRAP_TEST_TOKEN=") {
			t.Errorf("the invoking environment leaked into the child env: %q", lines)
		}
	}
	// Managed settings pin: the staged settings file binds read-only over
	// the home's user-scope settings, so the agent cannot rewrite its own
	// enforcement.
	if !strings.Contains(argv, "--ro-bind ") || !strings.Contains(argv, "user-settings.json") {
		t.Errorf("argv %q lacks the read-only managed settings pin", argv)
	}
	// The fake snapshots the seeded home during the run: onboarding marker,
	// seed file, and the transcript symlink at the caller's host path.
	if b, err := os.ReadFile(filepath.Join(dir, "claude-dump", ".claude", "CLAUDE.md")); err != nil || string(b) != "context" {
		t.Errorf("seeded CLAUDE.md = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "claude-dump", ".claude.json")); err != nil || !strings.Contains(string(b), "hasCompletedOnboarding") {
		t.Errorf(".claude.json = %q, %v", b, err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "claude-dump", ".claude", "projects")); err != nil || target != filepath.Dir(runstage.TranscriptDir(home, ws)) {
		t.Errorf("projects symlink = %q, %v; want %q", target, err, filepath.Dir(runstage.TranscriptDir(home, ws)))
	}
	// The guest resolves ~/.claude/projects/<enc> to the canonical path.
	enc := filepath.Base(runstage.TranscriptDir(home, ws))
	projects := filepath.Join(dir, "claude-dump", ".claude", "projects", enc)
	if got, err := filepath.EvalSymlinks(projects); err != nil || got != runstage.TranscriptDir(home, ws) {
		t.Errorf("~/.claude/projects/%s = %q, %v; want %q", enc, got, err, runstage.TranscriptDir(home, ws))
	}
}

func TestRunFailsClosedAndCleansUp(t *testing.T) {
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
	p2 := testPlan(home, ws)
	p2.Limits = plan.Limits{CPUs: 4}
	if _, err := e.Run(t.Context(), p2, engine.Options{}); err == nil || !strings.Contains(err.Error(), "limits") {
		t.Errorf("limits plan: err = %v; want a fail-closed error", err)
	}
	if _, err := e.Run(t.Context(), testPlan(home, ws), engine.Options{Verify: true}); err == nil || !strings.Contains(err.Error(), "verify") {
		t.Errorf("verify: err = %v; want an unsupported error", err)
	}
	// A plan mount must not collide with the engine's own transcript mount,
	// at the encoded path the engine derives.
	p3 := testPlan(home, ws)
	tdir := runstage.TranscriptDir(home, ws)
	p3.Mounts = append(p3.Mounts, plan.Mount{Host: tdir, Guest: tdir})
	if _, err := e.Run(t.Context(), p3, engine.Options{}); err == nil || !strings.Contains(err.Error(), "transcript") {
		t.Errorf("transcript mount: err = %v; want a collision error", err)
	}
}

func TestForwardSignalsDeliverToTheChild(t *testing.T) {
	dir := t.TempDir()
	// The fake marks itself ready after the trap is installed, logs the
	// TERM, and exits 0.
	script := "#!/bin/sh\nd=\"${0%/*}\"\n[ \"$1\" = __warm ] && exit 0\ntrap 'echo TERM >> \"$d/signals\"; exit 0' TERM\necho ready > \"$d/ready\"\nsleep 30 >/dev/null 2>&1 </dev/null &\nwait\n"
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
	// The trap must be installed before the TERM arrives.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake never signalled ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
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

// TestRunKillsChildAfterGrace runs against a child that ignores TERM: after
// the cancellation's grace, the escalation SIGKILLs it and the run reports
// 137 (128+SIGKILL).
func TestRunKillsChildAfterGrace(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nd=\"${0%/*}\"\n[ \"$1\" = __warm ] && exit 0\ntrap '' TERM\necho ready > \"$d/ready\"\nsleep 30 >/dev/null 2>&1 </dev/null &\nwait\n"
	bin := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(bin, "__warm").Run(); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Bin: bin, Home: dir, StateRoot: filepath.Join(dir, "state"), cancelGrace: 200 * time.Millisecond}
	ws := filepath.Join(dir, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
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
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake never signalled ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 137 {
			t.Errorf("exit code = %d; want 137 (128+SIGKILL)", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned after cancellation")
	}
}

// TestRunCancelBeforeStartNeverStarts cancels Run's context during the start
// delay, before bwrap starts. Run must return the cancellation promptly
// without ever launching the sandbox.
func TestRunCancelBeforeStartNeverStarts(t *testing.T) {
	t.Setenv("BWRAP_TEST_TOKEN", "x")
	bin, dir := fakeBwrap(t)
	home := t.TempDir()
	ws := filepath.Join(home, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Engine{
		Bin:            bin,
		Home:           home,
		StateRoot:      filepath.Join(home, "state"),
		testStartDelay: 20 * time.Second,
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		// secrets.env is the last file Run writes before the start delay.
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	code, err := e.Run(ctx, testPlan(home, ws), engine.Options{})
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Run error = %v; want the cancellation", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Run returned after %s; want promptly after the cancel", d)
	}
	if code != 1 {
		t.Errorf("exit code = %d; want 1", code)
	}
	// bwrap never ran: the fake's argv log is empty.
	if b, _ := os.ReadFile(filepath.Join(dir, "argv")); len(b) != 0 {
		t.Errorf("bwrap started despite the cancellation: %q", b)
	}
}

// argvLogs collects the fake's per-invocation logs, in name order.
func argvLogs(t *testing.T, dir string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "calls", "*.log"))
	var out []string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		out = append(out, strings.TrimSuffix(string(b), "\n"))
	}
	return out
}

// TestSecretsLinesNeverQuotesValues pins the trust rule that secret values
// never reach errors: a value containing a newline must split into an error
// that names the line, not the fragment.
func TestSecretsLinesNeverQuotesValues(t *testing.T) {
	lines, err := secretsLines([]byte("OK=yes\nBROKEN=frag\nment\nALSO=ok"))
	if err == nil {
		t.Fatal("a line without a separator must be rejected")
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("the error must name the line: %v", err)
	}
	if strings.Contains(err.Error(), "fragment") {
		t.Errorf("the error must not quote the secret material: %v", err)
	}
	lines, err = secretsLines([]byte("TOKEN=v\nBAD NAME=x\n"))
	if err == nil {
		t.Fatal("an invalid environment name must be rejected")
	}
	if strings.Contains(err.Error(), "=x") {
		t.Errorf("the error must not quote the value: %v", err)
	}
	if lines != nil {
		t.Errorf("a rejected file must produce no lines")
	}
}

// TestRunFailsClosedAsRoot pins the tier-0 privilege guard: an invoking
// root must fail before any bwrap invocation, and the error must say why.
func TestRunFailsClosedAsRoot(t *testing.T) {
	fake, dir := fakeBwrap(t)
	e := &Engine{Bin: fake, Home: dir, StateRoot: dir}
	root := initcfg.User{Name: "root", UID: 0, GID: 0, Home: "/root"}
	e.hostUser = func() (initcfg.User, error) { return root, nil }
	code, err := e.Run(t.Context(), testPlan(dir, dir), engine.Options{})
	if code == 0 || err == nil {
		t.Fatalf("a root invocation must fail closed: code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "unprivileged") {
		t.Errorf("the error must name the privilege rule: %v", err)
	}
	for _, f := range argvLogs(t, dir) {
		if strings.Contains(f, "bwrap") {
			t.Errorf("no bwrap invocation may happen as root: %q", f)
		}
	}
}
