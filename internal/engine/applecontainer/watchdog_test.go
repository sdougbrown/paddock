package applecontainer

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startTestWatchdog re-executes the test binary as the watchdog for a
// container named paddock-test with dir to remove, and waits for its ready
// byte. It returns the write end of the watchdog's pipe and a channel that
// receives the watchdog's exit. The watchdog runs in its own process group,
// which cleanup kills, hung fake CLI included.
func startTestWatchdog(t *testing.T, bin, dir string) (*os.File, chan error) {
	t.Helper()
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		w.Close()
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
	return w, exited
}

// TestWatchdogDoneByteSkipsCleanup tells the watchdog the run ended normally.
// It exits 0 without calling the container CLI or removing the directory.
func TestWatchdogDoneByteSkipsCleanup(t *testing.T) {
	bin, calls := fakeContainer(t)
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	w, exited := startTestWatchdog(t, bin, dir)
	if _, err := w.Write([]byte{watchdogDone}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	select {
	case err := <-exited:
		exited <- err
		if err != nil {
			t.Fatalf("watchdog: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watchdog still running after 10s")
	}
	if c := calls(); len(c) != 0 {
		t.Errorf("watchdog called the container CLI: %q", c)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("run dir: %v", err)
	}
}

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
		{"every stop hangs", []string{"slow-stop"}, watchdogStopTries, 1},
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
			w, exited := startTestWatchdog(t, bin, dir)
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

// TestStartWatchdogFailsWhenNotReady points the watchdog at a program that
// never writes the ready byte. startWatchdog must fail after
// watchdogReadyTimeout, name the entry arguments, and reap the child.
func TestStartWatchdogFailsWhenNotReady(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "not-paddock")
	script := "#!/bin/sh\necho $$ > \"${0%/*}/pid\"\nprintf '%s\\n' \"$@\" > \"${0%/*}/args\"\nexec sleep 30\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { watchdogReadyTimeout = d }(watchdogReadyTimeout)
	watchdogReadyTimeout = 2 * time.Second
	dirs, err := createRunDirs(t.TempDir(), "paddock-test")
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Bin: "container", selfExe: exe, watchdogArgs: []string{"custom-watchdog"}}
	start := time.Now()
	wd, err := e.startWatchdog("paddock-test", dirs)
	if err == nil {
		wd.finish()
		t.Fatal("startWatchdog succeeded; want a not-ready error")
	}
	// The wait must be the configured timeout: not cut short, and not much
	// longer than it, with room for oversleeping under -race load.
	if d, lo, hi := time.Since(start), watchdogReadyTimeout*9/10, watchdogReadyTimeout+3*time.Second; d < lo || d > hi {
		t.Errorf("startWatchdog returned after %s; want between %s and %s", d, lo, hi)
	}
	if want := `the program must dispatch the arguments ["custom-watchdog"] to applecontainer.RunWatchdog`; !strings.Contains(err.Error(), "did not become ready") || !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the readiness failure and the missing dispatch %q", err, want)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil || !strings.HasPrefix(string(args), "custom-watchdog\ncontainer\npaddock-test\n") {
		t.Errorf("watchdog argv = %q, %v; want the entry arguments, the CLI, then the name", args, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("watchdog child %d still exists after startWatchdog returned (kill 0: %v)", pid, err)
	}
}

// TestWatchdogRefusesNonPipe starts the watchdog with /dev/null on fd 3,
// which reads as EOF at once. It must exit 2 without signalling ready,
// calling the container CLI, or removing the directory.
func TestWatchdogRefusesNonPipe(t *testing.T) {
	bin, calls := fakeContainer(t)
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	cmd := exec.Command(os.Args[0], WatchdogCommand, bin, "paddock-test", dir)
	cmd.ExtraFiles = []*os.File{null, readyW}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	readyW.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-exited:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-exited
		t.Fatal("watchdog still running after 10s")
	}
	var ee *exec.ExitError
	if !errors.As(waitErr, &ee) || ee.ExitCode() != 2 {
		t.Errorf("watchdog exit: %v; want exit status 2", waitErr)
	}
	if b, _ := io.ReadAll(readyR); len(b) != 0 {
		t.Errorf("watchdog wrote %q on the ready pipe", b)
	}
	if c := calls(); len(c) != 0 {
		t.Errorf("watchdog called the container CLI: %q", c)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("run dir: %v", err)
	}
}

func TestRunWatchdogNeedsBinAndName(t *testing.T) {
	for _, args := range [][]string{nil, {"bin"}} {
		if code := RunWatchdog(args); code != 2 {
			t.Errorf("RunWatchdog(%q) = %d, want 2", args, code)
		}
	}
}

// TestDispatch checks that Dispatch hands WatchdogCommand to RunWatchdog
// and leaves every other argument list to the caller. The watchdog's own
// tests re-execute this binary through Dispatch in TestMain.
func TestDispatch(t *testing.T) {
	if code, handled := Dispatch([]string{WatchdogCommand}); !handled || code != 2 {
		t.Errorf("Dispatch(%q) = %d, %v; want RunWatchdog's 2, true", WatchdogCommand, code, handled)
	}
	for _, args := range [][]string{nil, {}, {"run", WatchdogCommand}, {"plan"}, {"-" + WatchdogCommand}} {
		if code, handled := Dispatch(args); handled || code != 0 {
			t.Errorf("Dispatch(%q) = %d, %v; want 0, false", args, code, handled)
		}
	}
}
