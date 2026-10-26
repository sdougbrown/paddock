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
		{"setup with extra args", []string{"setup", "--profile", "p", "make"}, 1, "paddock setup: the setup command comes from the spec"},
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
