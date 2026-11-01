package main

import (
	"bytes"
	"strings"
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

	t.Run("unknown command", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := run([]string{"bogus"}, &out, &errb); code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
		if out.Len() != 0 || !strings.Contains(errb.String(), "unknown command \"bogus\"") {
			t.Fatalf("stdout %q, stderr %q", out.String(), errb.String())
		}
	})
}
