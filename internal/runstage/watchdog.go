package runstage

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// The parent-side watchdog machinery, shared by the engines whose run
// outlives the paddock process: the child re-executes paddock with the
// engine's watchdog entry arguments, holds the read end of a pipe, and
// cleans up when the write end closes without the done byte. Each engine
// owns its own Dispatch command and RunWatchdog body; this is everything
// both engines share around that handshake.

// WatchdogDone is the byte the parent writes to tell the watchdog the run
// ended normally and nothing needs cleaning up.
const WatchdogDone = 'x'

// WatchdogReady is the byte the watchdog writes on fd 4 once it holds the
// pipe on fd 3.
const WatchdogReady = 'r'

// WatchdogReadyTimeout is how long StartWatchdog waits for the watchdog to
// signal that it is holding the pipe. Tests may shorten it.
var WatchdogReadyTimeout = 5 * time.Second

// WatchdogHandle is the handle on the running watchdog process.
type WatchdogHandle struct {
	w   *os.File
	cmd *exec.Cmd
}

// Finish tells the watchdog the run ended normally and reaps it.
func (w *WatchdogHandle) Finish() {
	w.w.Write([]byte{WatchdogDone})
	w.w.Close()
	w.cmd.Wait()
}

// StartWatchdog re-executes exe as a session leader with entry and tail as
// its arguments, holding the read end of a pipe; only this process holds the
// write end. The caller builds the full watchdog argv: entry is the
// engine's WatchdogCommand (a test may substitute its own), and tail is
// everything after it — the runtime binary, its markers, the container
// name, and the directories to remove. It returns once the watchdog has
// written its ready byte, and fails if that takes longer than
// WatchdogReadyTimeout, naming the missing Dispatch.
func StartWatchdog(exe string, entry, tail []string) (*WatchdogHandle, error) {
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
	cmd := exec.Command(exe, append(append([]string{}, entry...), tail...)...)
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
	readyR.SetReadDeadline(time.Now().Add(WatchdogReadyTimeout))
	var b [1]byte
	if n, _ := readyR.Read(b[:]); n != 1 || b[0] != WatchdogReady {
		cmd.Process.Kill()
		cmd.Wait()
		w.Close()
		return nil, fmt.Errorf("watchdog %s %q did not become ready within %s; the program must dispatch the arguments %q to the engine's RunWatchdog", exe, entry, WatchdogReadyTimeout, entry)
	}
	return &WatchdogHandle{w: w, cmd: cmd}, nil
}
