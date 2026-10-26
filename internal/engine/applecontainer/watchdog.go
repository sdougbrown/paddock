package applecontainer

import (
	"context"
	"io"
	"os"
	"time"
)

// WatchdogCommand is the hidden subcommand under which paddock re-executes
// itself as the watchdog.
const WatchdogCommand = "__watchdog"

// watchdogDone is the byte the parent writes to tell the watchdog the run
// ended normally and nothing needs cleaning up.
const watchdogDone = 'x'

// watchdogReady is the byte the watchdog writes on fd 4 once it holds the pipe on fd 3.
const watchdogReady = 'r'

// watchdogCLITimeout bounds each `container` call the watchdog makes, so a
// hung container service cannot keep it from removing the run's directories.
var watchdogCLITimeout = cliTimeout + 2*time.Second

// watchdogRetry is the pause between the watchdog's `container stop` attempts.
var watchdogRetry = time.Second

// RunWatchdog is the watchdog process body. args are the container binary,
// the container name, then the directories to remove (runDirs.removable). It
// reports ready on fd 4, then blocks on fd 3, the read end of a pipe whose
// write end only the parent holds. On EOF without the done byte the parent
// died, so it stops the container and removes the directories.
func RunWatchdog(args []string) int {
	if len(args) < 2 {
		return 2
	}
	e, name, dirs := &Engine{Bin: args[0]}, args[1], args[2:]
	pipe := os.NewFile(3, "watchdog-pipe")
	if pipe == nil {
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
	for range 10 {
		if _, err := e.cliRun(context.Background(), watchdogCLITimeout, "stop", "--time", "2", name); err == nil {
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
