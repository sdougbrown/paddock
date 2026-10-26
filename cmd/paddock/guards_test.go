package main

import (
	"bytes"
	"strings"
	"testing"
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
		{"setup unknown flag", []string{"setup", "--bogus"}, 2, "flag provided but not defined: -bogus"},
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
