package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/plan"
)

// planFixture points PADDOCK_CONFIG_DIR at the resolve testdata root and
// returns a workspace holding the "repo" directory the fixer profile mounts
// read-only.
func planFixture(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "internal", "resolve", "testdata", "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PADDOCK_CONFIG_DIR", root)
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestPlanCommand(t *testing.T) {
	ws := planFixture(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"plan", "--profile", "fixer", "--spec", "afresh", "--workspace", ws, "--", "claude"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	var p plan.Plan
	if err := json.Unmarshal(stdout.Bytes(), &p); err != nil {
		t.Fatalf("stdout is not a plan: %v\n%s", err, stdout.String())
	}
	if strings.Join(p.Argv, " ") != "claude" || p.Phase != "run" {
		t.Errorf("argv %q phase %q", p.Argv, p.Phase)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"plan", "--", "claude"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "--profile is required") {
		t.Errorf("missing profile: exit %d, stderr %q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"plan", "--profile", "fixer", "--spec", "afresh", "--workspace", ws, "claude", "-run", "X"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dashed args: exit %d, stderr %q", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &p); err != nil {
		t.Fatalf("stdout is not a plan: %v\n%s", err, stdout.String())
	}
	if strings.Join(p.Argv, " ") != "claude -run X" {
		t.Errorf("argv %q, want %q", p.Argv, []string{"claude", "-run", "X"})
	}
}

func TestPlanCommandNeedsHome(t *testing.T) {
	ws := planFixture(t)
	t.Setenv("HOME", "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"plan", "--profile", "fixer", "--spec", "afresh", "--workspace", ws}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "home directory") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestPlanCommandSelectsSpecFromOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tests := []struct {
		name, profile, origin string
		wantSpec, wantErr     string
	}{
		{"origin matches an operator spec", "fixer", "git@github.com:afresh/scanner.git", "afresh/scanner", ""},
		{"origin without a spec falls back to the default", "fixer", "https://github.com/other/repo.git", "afresh", ""},
		{"origin without a spec and no default", "nodefault", "https://user:secret@github.com/other/repo.git", "", `no spec "other/repo" for origin "https://github.com/other/repo.git"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := planFixture(t)
			for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", tc.origin}} {
				if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"plan", "--profile", tc.profile, "--workspace", ws}, &stdout, &stderr)
			if tc.wantErr != "" {
				if code == 0 || !strings.Contains(stderr.String(), tc.wantErr) || strings.Contains(stderr.String(), "secret") {
					t.Fatalf("exit %d, stderr %q; want an error containing %q without the credential", code, stderr.String(), tc.wantErr)
				}
				return
			}
			if code != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr.String())
			}
			var p plan.Plan
			if err := json.Unmarshal(stdout.Bytes(), &p); err != nil {
				t.Fatalf("stdout is not a plan: %v", err)
			}
			if p.Spec != tc.wantSpec {
				t.Errorf("spec = %q, want %q", p.Spec, tc.wantSpec)
			}
		})
	}
}

// exitStub reports a fixed exit code and records the argv it saw, so tests
// can observe passthrough.
type exitStub struct {
	code int
	seen *[]string
}

func (s exitStub) Run(_ context.Context, p *plan.Plan, _ engine.Options) (int, error) {
	if s.seen != nil {
		*s.seen = p.Argv
	}
	return s.code, nil
}

// TestSandboxedExitCodePassesThrough checks run returns the sandboxed
// command's own exit code rather than a paddock-level one.
func TestSandboxedExitCodePassesThrough(t *testing.T) {
	orig := newEngine
	t.Cleanup(func() { newEngine = orig })
	newEngine = func(string) (engine.Engine, error) { return exitStub{code: 3}, nil }
	ws := planFixture(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--profile", "fixer", "--spec", "afresh", "--workspace", ws, "--", "claude"}, &stdout, &stderr)
	if code != 3 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("exit %d, want 3 with silent streams (stdout %q, stderr %q)", code, stdout.String(), stderr.String())
	}
}

// TestSetupExitCodePassesThrough checks setup returns the setup command's
// own exit code rather than a paddock-level one.
func TestSetupExitCodePassesThrough(t *testing.T) {
	orig := newEngine
	t.Cleanup(func() { newEngine = orig })
	newEngine = func(string) (engine.Engine, error) { return exitStub{code: 3}, nil }
	ws := planFixture(t)
	if err := os.WriteFile(filepath.Join(ws, "yarn.lock"), []byte("#"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"setup", "--profile", "fixer", "--spec", "afresh/scanner", "--workspace", ws}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit %d, want 3 (stderr %q)", code, stderr.String())
	}
}

// TestRunPassesThroughDashedArgs checks that arguments after the command
// name reach the command instead of parsing as paddock's flags.
func TestRunPassesThroughDashedArgs(t *testing.T) {
	orig := newEngine
	t.Cleanup(func() { newEngine = orig })
	var gotArgv []string
	newEngine = func(string) (engine.Engine, error) { return exitStub{seen: &gotArgv}, nil }
	ws := planFixture(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--profile", "fixer", "--spec", "afresh", "--workspace", ws, "go", "test", "-run", "X"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %q)", code, stderr.String())
	}
	if want := []string{"go", "test", "-run", "X"}; !slices.Equal(gotArgv, want) {
		t.Errorf("argv %q, want %q", gotArgv, want)
	}
}

// TestTopLevelHelp checks paddock, --help, and -h: each exits 0 and prints
// the command list on stdout.
func TestTopLevelHelp(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bare", nil},
		{"--help", []string{"--help"}},
		{"-h", []string{"-h"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			out := stdout.String()
			if code != 0 || !strings.Contains(out, "Available Commands:") ||
				!strings.Contains(out, "setup") || !strings.Contains(out, "version") || stderr.Len() != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out, stderr.String())
			}
		})
	}
}

// TestFlagHelpAndParseErrors checks plan, run, and setup: -h exits 0 and
// prints help on stdout, an unknown flag exits 2 with cobra's error relayed
// under the command's name, and nothing lands on the other stream.
func TestFlagHelpAndParseErrors(t *testing.T) {
	for _, cmd := range []string{"plan", "run", "setup"} {
		t.Run(cmd+" -h", func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{cmd, "-h"}, &stdout, &stderr)
			out := stdout.String()
			if code != 0 || !strings.Contains(out, "Usage:") || !strings.Contains(out, "--profile") || stderr.Len() != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out, stderr.String())
			}
		})
		t.Run(cmd+" --bogus", func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{cmd, "--bogus"}, &stdout, &stderr)
			out := stderr.String()
			if code != 2 || !strings.Contains(out, "unknown flag: --bogus") ||
				!strings.Contains(out, "paddock "+cmd+":") || stdout.Len() != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), out)
			}
		})
	}
}
