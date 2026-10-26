package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/plan"
)

func TestRunAndSetupArgumentGuards(t *testing.T) {
	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"run without profile", []string{"run", "--", "claude"}, 1, "paddock run: --profile is required"},
		{"run without command", []string{"run", "--profile", "p"}, 1, "paddock run: a command or --verify is required"},
		{"run verify with command", []string{"run", "--profile", "p", "--verify", "--", "claude"}, 1, "paddock run: --verify takes no command"},
		{"run profile without value", []string{"run", "--profile"}, 2, "flag needs an argument: -profile"},
		{"setup without profile", []string{"setup"}, 1, "paddock setup: --profile is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != tt.code {
				t.Errorf("exit %d, want %d", code, tt.code)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr %q lacks %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestRunAndSetupReportHomeLookupError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("PADDOCK_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, args := range [][]string{{"run", "--profile", "p", "--", "claude"}, {"setup", "--profile", "p"}} {
		t.Run(args[0], func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			if want := "paddock " + args[0] + ": home directory: "; !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr %q lacks %q", stderr.String(), want)
			}
		})
	}
}

// TestSetupRejectsACommand passes a command to setup. The resolver's
// setup-phase guard rejects it, as it does for `paddock plan --phase setup`.
func TestSetupRejectsACommand(t *testing.T) {
	ws := planFixture(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"setup", "--profile", "fixer", "--spec", "afresh/scanner", "--workspace", ws, "make"}, &stdout, &stderr)
	if want := "paddock setup: phase setup: the setup command comes from the spec"; code != 1 || !strings.Contains(stderr.String(), want) {
		t.Errorf("exit %d, stderr %q; want 1 and %q", code, stderr.String(), want)
	}
}

// recordingEngine records the plan and options Run receives.
type recordingEngine struct {
	plan *plan.Plan
	opts engine.Options
}

func (r *recordingEngine) Run(_ context.Context, p *plan.Plan, o engine.Options) (int, error) {
	r.plan, r.opts = p, o
	return 0, nil
}

// TestRunPassesVerifyToEngine checks that `paddock run --verify` reaches
// the engine as Options.Verify with no argv, and a command does not.
func TestRunPassesVerifyToEngine(t *testing.T) {
	ws := planFixture(t)
	rec := &recordingEngine{}
	defer func(f func(string) (engine.Engine, error)) { newEngine = f }(newEngine)
	newEngine = func(string) (engine.Engine, error) { return rec, nil }
	for _, tc := range []struct {
		args   []string
		verify bool
		argv   string
	}{
		{[]string{"--verify"}, true, ""},
		{[]string{"--", "claude"}, false, "claude"},
	} {
		var stdout, stderr bytes.Buffer
		args := append([]string{"run", "--profile", "fixer", "--spec", "afresh", "--workspace", ws}, tc.args...)
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%q: exit %d, stderr %q", tc.args, code, stderr.String())
		}
		if rec.opts.Verify != tc.verify || strings.Join(rec.plan.Argv, " ") != tc.argv {
			t.Errorf("%q: engine got Verify %v, argv %q; want %v, %q", tc.args, rec.opts.Verify, rec.plan.Argv, tc.verify, tc.argv)
		}
	}
}
