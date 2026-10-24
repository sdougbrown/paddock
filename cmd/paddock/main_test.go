package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/plan"
)

func TestPlanCommand(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "internal", "resolve", "testdata", "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PADDOCK_CONFIG_DIR", root)
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}

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
}

func TestPlanCommandNeedsHome(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "internal", "resolve", "testdata", "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PADDOCK_CONFIG_DIR", root)
	t.Setenv("HOME", "")
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
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
	root, err := filepath.Abs(filepath.Join("..", "..", "internal", "resolve", "testdata", "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PADDOCK_CONFIG_DIR", root)
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
			ws := t.TempDir()
			if err := os.Mkdir(filepath.Join(ws, "repo"), 0o755); err != nil {
				t.Fatal(err)
			}
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

func TestPlanCommandRejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"plan", "--bogus"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "paddock plan:") || !strings.Contains(stderr.String(), "bogus") || stdout.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}
