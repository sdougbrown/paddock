package applecontainer

import (
	"context"
	"io"
	"os"
	"time"
)

// WatchdogCommand is the internal first argument under which Run
// re-executes the running program as the watchdog. It is not a user-facing
// subcommand; Dispatch handles it.
const WatchdogCommand = "__watchdog"

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

// watchdogStopGrace is how many seconds the watchdog's `container stop`
// gives the guest before killing it.
const watchdogStopGrace = 2

// watchdogCLITimeout bounds each `container` call the watchdog makes, so a
// hung container service cannot keep it from removing the run's directories.
// It is the stop timeout for watchdogStopGrace.
var watchdogCLITimeout = stopTimeout(watchdogStopGrace)

// watchdogStopTries is how many times the watchdog tries `container stop`
// before it deletes the container anyway.
const watchdogStopTries = 10

// watchdogRetry is the pause between the watchdog's `container stop` attempts.
var watchdogRetry = time.Second

// RunWatchdog is the watchdog process body. args are the container binary,
// the container name, then the directories to remove (runDirs.removable). It
// reports ready on fd 4, then blocks on fd 3, the read end of a pipe whose
// write end only the parent holds. On EOF without the done byte the parent
// died, so it stops the container and removes the directories. It exits 2
// without acting when fd 3 is not a pipe.
func RunWatchdog(args []string) int {
	if len(args) < 2 {
		return 2
	}
	e, name, dirs := &Engine{Bin: args[0]}, args[1], args[2:]
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
		if _, err := e.cliRun(context.Background(), watchdogCLITimeout, stopArgs(name, watchdogStopGrace)...); err == nil {
			break
		}
		time.Sleep(watchdogRetry)
	}
	e.cliRun(context.Background(), watchdogCLITimeout, "delete", "--force", name)
	for _, d := range dirs {
		os.RemoveAll(d)
	}
	return 0
}
