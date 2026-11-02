package podman

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

	"github.com/sdougbrown/paddock/internal/runstage"
)

// startTestWatchdog re-executes the test binary as the watchdog for the run
// directories it creates, and waits for its ready byte. It returns the write
// end of the watchdog's pipe, a channel that receives the watchdog's exit,
// and the run directories. The watchdog runs in its own process group, which
// cleanup kills, hung fake CLI included.
func startTestWatchdog(t *testing.T, bin string, sudo bool) (*os.File, chan error, runstage.RunDirs) {
	t.Helper()
	dirs, err := runstage.CreateRunDirs(t.TempDir(), "paddock-test")
	if err != nil {
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
	marker := "-"
	if sudo {
		marker = "sudo"
	}
	cmd := exec.Command(os.Args[0], WatchdogCommand, bin, marker, "paddock-test", dirs.Root)
	cmd.Args = append(cmd.Args, dirs.Removable()...)
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
	return w, exited, dirs
}

func TestWatchdogDoneByteSkipsCleanup(t *testing.T) {
	bin, calls := fakePodman(t)
	w, exited, dirs := startTestWatchdog(t, bin, false)
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
		t.Errorf("watchdog called the podman CLI: %q", c)
	}
	if _, err := os.Stat(dirs.State); err != nil {
		t.Errorf("run dir: %v", err)
	}
}

// TestWatchdogBoundsHungCLICalls runs the watchdog after its parent's pipe
// closes without the done byte, with a podman CLI whose stop or rm hangs or
// fails. Each call must time out, the stop is retried, the rm is attempted,
// and the run's directories go only when the container is verifiably gone.
func TestWatchdogBoundsHungCLICalls(t *testing.T) {
	tests := []struct {
		name     string
		hang     []string // fakePodman marker files
		fail     []string
		stops    int
		rm       int
		dirsKept bool
		wantDiag bool
	}{
		{"first stop and rm hang", []string{"slow-once-stop", "slow-once-rm"}, nil, 2, 1, true, true},
		{"every stop hangs", []string{"slow-stop"}, nil, watchdogStopTries, 1, false, false},
		{"stop and rm fail", nil, []string{"fail-stop", "fail-rm"}, watchdogStopTries, 1, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, calls := fakePodman(t)
			dir := filepath.Dir(bin)
			for _, f := range append(tt.hang, tt.fail...) {
				if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			w, exited, dirs := startTestWatchdog(t, bin, false)
			w.Close()

			select {
			case err := <-exited:
				exited <- err
				if err != nil {
					t.Fatalf("watchdog: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("watchdog still running after 15s; calls: %q", calls())
			}
			_, statErr := os.Stat(dirs.State)
			if kept := statErr == nil; kept != tt.dirsKept {
				t.Errorf("state dir kept = %v, want %v (stat: %v)", kept, tt.dirsKept, statErr)
			}
			diag := filepath.Join(dirs.Root, "log", "watchdog.log")
			_, diagErr := os.Stat(diag)
			if tt.wantDiag && diagErr != nil {
				t.Errorf("no watchdog.log after failed cleanup: %v", diagErr)
			}
			if !tt.wantDiag && diagErr == nil {
				t.Errorf("watchdog.log written without a failed cleanup")
			}
			var stops, rms int
			for _, c := range calls() {
				switch c[0] {
				case "stop":
					stops++
				case "rm":
					rms++
				}
			}
			if stops != tt.stops || rms != tt.rm {
				t.Errorf("got %d stops and %d rms, want %d and %d; calls: %q", stops, rms, tt.stops, tt.rm, calls())
			}
		})
	}
}

// TestWatchdogRefusesNonPipe starts the watchdog with /dev/null on fd 3,
// which reads as EOF at once. It must exit 2 without signalling ready,
// calling the podman CLI, or removing the directory.
func TestWatchdogRefusesNonPipe(t *testing.T) {
	bin, calls := fakePodman(t)
	dirs, err := runstage.CreateRunDirs(t.TempDir(), "paddock-test")
	if err != nil {
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
	cmd := exec.Command(os.Args[0], WatchdogCommand, bin, "-", "paddock-test", dirs.Root)
	cmd.Args = append(cmd.Args, dirs.Removable()...)
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
		t.Errorf("watchdog called the podman CLI: %q", c)
	}
	if _, err := os.Stat(dirs.State); err != nil {
		t.Errorf("run dir: %v", err)
	}
}

func TestRunWatchdogNeedsFullArgv(t *testing.T) {
	for _, args := range [][]string{nil, {"bin"}, {"bin", "sudo"}, {"bin", "sudo", "name"}} {
		if code := RunWatchdog(args); code != 2 {
			t.Errorf("RunWatchdog(%q) = %d, want 2", args, code)
		}
	}
}

// TestDispatch checks that Dispatch hands WatchdogCommand to RunWatchdog
// and leaves every other argument list to the caller.
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

// TestStartWatchdogFailsWhenNotReady points the watchdog at a program that
// never writes the ready byte. startWatchdog must fail after
// watchdogReadyTimeout, name the entry arguments, and reap the child.
func TestStartWatchdogFailsWhenNotReady(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "not-paddock")
	script := "#!/bin/sh\n[ \"$1\" = warm ] && exit 0\necho $$ > \"${0%/*}/pid\"\nprintf '%s\\n' \"$@\" > \"${0%/*}/args\"\nexec sleep 30\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Keep a slow first exec out of the timed readiness wait.
	if err := exec.Command(exe, "warm").Run(); err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { watchdogReadyTimeout = d }(watchdogReadyTimeout)
	watchdogReadyTimeout = 2 * time.Second
	dirs, err := runstage.CreateRunDirs(t.TempDir(), "paddock-test")
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Bin: "podman", selfExe: exe, watchdogArgs: []string{"custom-watchdog"}}
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
	if want := `the program must dispatch the arguments ["custom-watchdog"] to podman.RunWatchdog`; !strings.Contains(err.Error(), "did not become ready") || !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the readiness failure and the missing dispatch %q", err, want)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	want := []string{"custom-watchdog", "podman", "-", "paddock-test", dirs.Root}
	if len(lines) < len(want) {
		t.Fatalf("watchdog argv = %q; want at least %q", lines, want)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("watchdog argv[%d] = %q, want %q (full: %q)", i, lines[i], w, lines)
		}
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
