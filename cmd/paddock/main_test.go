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

// TestFlagHelpAndParseErrors checks plan, run, and setup: -h exits 0 and
// prints usage, an unknown flag exits 2, and the flag package's own report
// is the only output.
func TestFlagHelpAndParseErrors(t *testing.T) {
	for _, cmd := range []string{"plan", "run", "setup"} {
		for _, tc := range []struct {
			arg  string
			code int
			want string
		}{
			{"-h", 0, "Usage of " + cmd + ":"},
			{"--bogus", 2, "flag provided but not defined: -bogus"},
		} {
			t.Run(cmd+" "+tc.arg, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				code := run([]string{cmd, tc.arg}, &stdout, &stderr)
				out := stderr.String()
				if code != tc.code || !strings.Contains(out, tc.want) || !strings.Contains(out, "Usage of "+cmd+":") ||
					strings.Contains(out, "paddock "+cmd+":") || stdout.Len() != 0 {
					t.Errorf("exit %d (want %d), stdout %q, stderr %q", code, tc.code, stdout.String(), out)
				}
			})
		}
	}
}
