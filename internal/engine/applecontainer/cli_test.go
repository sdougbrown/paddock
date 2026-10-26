package applecontainer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCLIRunReturnsWhenAChildHoldsOutput times out a CLI that leaves a
// background child holding its stdout. cliRun must return soon after the
// timeout rather than when that child exits.
func TestCLIRunReturnsWhenAChildHoldsOutput(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child")
	bin := filepath.Join(dir, "container")
	script := "#!/bin/sh\n[ \"$1\" = warm ] && exit 0\nsleep 30 &\necho $! > \"${0%/*}/child\"\nexec sleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The first exec of a new script can be slow on a loaded host; run it
	// once so the timed call below measures only cliRun.
	if err := exec.Command(bin, "warm").Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	start := time.Now()
	if _, err := (&Engine{Bin: bin}).cliRun(t.Context(), 2*time.Second, "ls"); err == nil {
		t.Fatal("cliRun succeeded; want a timeout error")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("cliRun returned after %s; want about the timeout plus WaitDelay", d)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("the CLI never started its background child: %v", err)
	}
}
