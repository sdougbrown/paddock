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
		want string
	}{
		{"run without profile", []string{"run", "--", "claude"}, "paddock run: --profile is required"},
		{"run without command", []string{"run", "--profile", "p"}, "paddock run: a command or --verify is required"},
		{"run verify with command", []string{"run", "--profile", "p", "--verify", "--", "claude"}, "paddock run: --verify takes no command"},
		{"setup without profile", []string{"setup"}, "paddock setup: --profile is required"},
		{"setup with extra args", []string{"setup", "--profile", "p", "make"}, "paddock setup: the setup command comes from the spec"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr %q lacks %q", stderr.String(), tt.want)
			}
		})
	}
}
