package applecontainer

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestWatchdogBoundsHungCLICalls runs the watchdog after its parent's pipe
// closes without the done byte, with a container CLI whose stop or delete
// hangs. Each call must time out, so the stop is retried, the delete is
// attempted, and the run's directories are still removed.
func TestWatchdogBoundsHungCLICalls(t *testing.T) {
	tests := []struct {
		name   string
		hang   []string // fakeContainer marker files
		stops  int
		delete int
	}{
		{"first stop and delete hang", []string{"slow-once-stop", "slow-once-delete"}, 2, 1},
		{"every stop hangs", []string{"slow-stop"}, 10, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, calls := fakeContainer(t)
			for _, f := range tt.hang {
				if err := os.WriteFile(filepath.Join(filepath.Dir(bin), f), nil, 0o644); err != nil {
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
			cmd.Env = append(os.Environ(), "HELPER_WATCHDOG_CLI_TIMEOUT=300ms", "HELPER_WATCHDOG_RETRY=50ms")
			cmd.ExtraFiles = []*os.File{r, readyW}
			// Its own process group, so cleanup also kills a hung fake CLI.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			t.Cleanup(func() {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-exited
			})
			r.Close()
			readyW.Close()
			readyR.SetReadDeadline(time.Now().Add(5 * time.Second))
			var b [1]byte
			if n, _ := readyR.Read(b[:]); n != 1 || b[0] != watchdogReady {
				t.Fatal("watchdog did not become ready")
			}
			w.Close()

			select {
			case err := <-exited:
				exited <- err
				if err != nil {
					t.Fatalf("watchdog: %v", err)
				}
			case <-time.After(10 * time.Second):
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
			if stops != tt.stops || deletes != tt.delete {
				t.Errorf("got %d stops and %d deletes, want %d and %d; calls: %q", stops, deletes, tt.stops, tt.delete, calls())
			}
		})
	}
}
