package main

import (
	"bytes"
	"testing"
)

func TestRun(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := run([]string{"version"}, &out, &errb); code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		if got, want := out.String(), "paddock "+version+"\n"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	tests := []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"unknown command", []string{"bogus"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run(tc.args, &out, &errb); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if out.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", out.String())
			}
		})
	}
}
