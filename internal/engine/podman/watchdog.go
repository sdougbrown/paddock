package podman

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"time"

	"github.com/sdougbrown/paddock/internal/runstage"
)

// WatchdogCommand is the internal first argument under which Run
// re-executes the running program as the watchdog. It is not a user-facing
// subcommand; Dispatch handles it.
const WatchdogCommand = "__watchdog-podman"

// Dispatch is the one entry a program embedding Engine wires: it calls
// Dispatch with its arguments (without the program name) before handling
// them itself. When they start with WatchdogCommand, Dispatch runs the
// watchdog and returns its exit code with handled true; otherwise it
// returns handled false and the program carries on.
func Dispatch(args []string) (code int, handled bool) {
	if len(args) == 0 || args[0] != WatchdogCommand {
		return 0, false
	}
	return RunWatchdog(args[1:]), true
}

// watchdogDone is the byte the parent writes to tell the watchdog the run
// ended normally and nothing needs cleaning up.
const watchdogDone = 'x'

// watchdogReady is the byte the watchdog writes on fd 4 once it holds the pipe on fd 3.
const watchdogReady = 'r'

// watchdogCLITimeout bounds each `podman` call the watchdog makes, so a hung
// podman cannot keep it from removing the run's directories. It is the stop
// timeout for orphanStopGrace.
var watchdogCLITimeout = stopTimeout(orphanStopGrace)

// watchdogStopTries is how many times the watchdog tries `podman stop`
// before it deletes the container anyway.
const watchdogStopTries = 10

// watchdogRetry is the pause between the watchdog's `podman stop` attempts.
var watchdogRetry = time.Second

// RunWatchdog is the watchdog process body. args are the podman binary, a
// sudo marker ("sudo" or "-"), the container name, then the directories to
// remove (RunDirs.Removable). It reports ready on
// fd 4, then blocks on fd 3, the read end of a pipe whose write end only the
// parent holds. On EOF without the done byte the parent died, so it stops
// the container and removes the directories. It exits 2 without acting when
// fd 3 is not a pipe.
func RunWatchdog(args []string) int {
	if len(args) < 3 {
		return 2
	}
	e := &Engine{Bin: args[0], Sudo: args[1] == "sudo"}
	name, dirs := args[2], args[3:]
	// Only a pipe from startWatchdog may trigger cleanup: anything else on
	// fd 3, such as /dev/null, would read as EOF at once.
	pipe := os.NewFile(3, "watchdog-pipe")
	if pipe == nil {
		return 2
	}
	if st, err := pipe.Stat(); err != nil || st.Mode()&os.ModeNamedPipe == 0 {
		return 2
	}
	if ready := os.NewFile(4, "watchdog-ready"); ready != nil {
		ready.Write([]byte{watchdogReady})
		ready.Close()
	}
	data, _ := io.ReadAll(pipe)
	if len(data) > 0 && data[0] == watchdogDone {
		return 0
	}
	for range watchdogStopTries {
		if _, err := e.cliRun(context.Background(), watchdogCLITimeout, stopArgs(name, orphanStopGrace)...); err == nil {
			break
		}
		time.Sleep(watchdogRetry)
	}
	e.cliRun(context.Background(), watchdogCLITimeout, "rm", "--force", name)
	for _, d := range dirs {
		os.RemoveAll(d)
	}
	return 0
}

// watchdog is the handle on the running watchdog process.
type watchdog struct {
	w   *os.File
	cmd *exec.Cmd
}

// watchdogReadyTimeout is how long startWatchdog waits for the watchdog to
// signal that it is holding the pipe.
var watchdogReadyTimeout = 5 * time.Second

// startWatchdog re-executes paddock as a session leader holding the read end
// of a pipe; only this process holds the write end. It returns once the
// watchdog has written its ready byte, and fails if that takes longer than
// watchdogReadyTimeout.
func (e *Engine) startWatchdog(name string, d runstage.RunDirs) (*watchdog, error) {
	exe := e.selfExe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, err
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	entry := e.watchdogArgs
	if entry == nil {
		entry = []string{WatchdogCommand}
	}
	marker := "-"
	if e.Sudo {
		marker = "sudo"
	}
	args := slices.Concat(entry, []string{e.bin(), marker, name}, d.Removable())
	cmd := exec.Command(exe, args...)
	cmd.ExtraFiles = []*os.File{r, readyW}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	r.Close()
	readyW.Close()
	if err != nil {
		w.Close()
		readyR.Close()
		return nil, fmt.Errorf("start watchdog: %w", err)
	}
	defer readyR.Close()
	readyR.SetReadDeadline(time.Now().Add(watchdogReadyTimeout))
	var b [1]byte
	if n, _ := readyR.Read(b[:]); n != 1 || b[0] != watchdogReady {
		cmd.Process.Kill()
		cmd.Wait()
		w.Close()
		return nil, fmt.Errorf("watchdog %s %q did not become ready within %s; the program must dispatch the arguments %q to podman.RunWatchdog", exe, entry, watchdogReadyTimeout, entry)
	}
	return &watchdog{w: w, cmd: cmd}, nil
}

// finish tells the watchdog the run ended normally and reaps it.
func (w *watchdog) finish() {
	w.w.Write([]byte{watchdogDone})
	w.w.Close()
	w.cmd.Wait()
}
