package applecontainer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestWatchdogBoundsHungCLICalls runs the watchdog after its parent's pipe
// closes without the done byte, with a container CLI whose first stop and
// first delete hang. Each call must time out, so the stop is retried and the
// run's directories are still removed.
func TestWatchdogBoundsHungCLICalls(t *testing.T) {
	bin, calls := fakeContainer(t)
	for _, sub := range []string{"stop", "delete"} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(bin), "slow-once-"+sub), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	cmd := exec.Command(os.Args[0], WatchdogCommand, bin, "paddock-test", dir)
	cmd.Env = append(os.Environ(), "HELPER_WATCHDOG_CLI_TIMEOUT=500ms")
	cmd.ExtraFiles = []*os.File{r, readyW}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	readyW.Close()
	readyR.SetReadDeadline(time.Now().Add(5 * time.Second))
	var b [1]byte
	if n, _ := readyR.Read(b[:]); n != 1 || b[0] != watchdogReady {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal("watchdog did not become ready")
	}
	w.Close()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("watchdog: %v", err)
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-exited
		t.Fatalf("watchdog still running after 10s; calls: %q", calls())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("run dir not removed: %v", err)
	}
	var stops, deletes int
	for _, c := range calls() {
		switch c[0] {
		case "stop":
			stops++
		case "delete":
			deletes++
		}
	}
	if stops != 2 || deletes != 1 {
		t.Errorf("got %d stops and %d deletes, want 2 and 1; calls: %q", stops, deletes, calls())
	}
}
