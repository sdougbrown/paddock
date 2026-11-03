package podman

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
var watchdogCLITimeout = stopTimeout(runstage.OrphanStopGrace)

// watchdogStopTries is how many times the watchdog tries `podman stop`
// before it deletes the container anyway.
const watchdogStopTries = 10

// watchdogRetry is the pause between the watchdog's `podman stop` attempts.
var watchdogRetry = time.Second

// RunWatchdog is the watchdog process body. args are the podman binary, a
// sudo marker ("sudo" or "-"), the container name, the run's root directory
// (which holds the kept log directory), then the directories to remove
// (RunDirs.Removable). It reports ready on
// fd 4, then blocks on fd 3, the read end of a pipe whose write end only the
// parent holds. On EOF without the done byte the parent died, so it stops
// the container and removes the directories. It exits 2 without acting when
// fd 3 is not a pipe.
func RunWatchdog(args []string) int {
	if len(args) < 4 {
		return 2
	}
	e := &Engine{Bin: args[0], Sudo: args[1] == "sudo"}
	name, root, dirs := args[2], args[3], args[4:]
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
		ready.Write([]byte{runstage.WatchdogReady})
		ready.Close()
	}
	data, _ := io.ReadAll(pipe)
	if len(data) > 0 && data[0] == runstage.WatchdogDone {
		return 0
	}
	for range watchdogStopTries {
		if _, err := e.cliRun(context.Background(), watchdogCLITimeout, stopArgs(name, runstage.OrphanStopGrace)...); err == nil {
			break
		}
		time.Sleep(watchdogRetry)
	}
	e.cliRun(context.Background(), watchdogCLITimeout, "rm", "--force", name)
	// Removing the run directories while the container is still running
	// would strand its mounts, so the removal waits for the container to
	// be verifiably gone; when it never is, the failure lands in the kept
	// log directory instead of disappearing.
	if e.isRunning(name) {
		diagnostic := fmt.Sprintf("%s watchdog: %s still runs after stop and rm; the run directories stay\n", time.Now().Format(time.RFC3339), name)
		_ = os.WriteFile(filepath.Join(root, "log", "watchdog.log"), []byte(diagnostic), 0o644)
		return 0
	}
	for _, d := range dirs {
		os.RemoveAll(d)
	}
	return 0
}

// startWatchdog re-executes paddock as a session leader holding the read
// end of a pipe, with the podman binary, the sudo marker, the run root, and
// the removable directories as the watchdog's tail arguments. The handshake
// lives in runstage.
func (e *Engine) startWatchdog(name string, d runstage.RunDirs) (*runstage.WatchdogHandle, error) {
	exe := e.selfExe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, err
		}
	}
	entry := e.watchdogArgs
	if entry == nil {
		entry = []string{WatchdogCommand}
	}
	marker := "-"
	if e.Sudo {
		marker = "sudo"
	}
	return runstage.StartWatchdog(exe, entry, append([]string{e.bin(), marker, name, d.Root}, d.Removable()...))
}
