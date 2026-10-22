package applecontainer

import (
	"fmt"
	"io"
	"os"
	"time"
)

// Engine runs plans with the `container` CLI. The zero value is usable.
//
// Cleanup after a crash of the paddock process (SIGKILL, power loss of the
// parent) is done by a watchdog that Run starts by re-executing the running
// binary with WatchdogCommand as its first argument. The program embedding
// Engine must dispatch that argument to RunWatchdog; otherwise the watchdog
// never signals readiness and Run fails.
type Engine struct {
	Bin       string // container CLI; default "container"
	Home      string // default: the user's home directory
	StateRoot string // default: <home>/.local/state/paddock
	CacheRoot string // default: <home>/.cache/paddock
	Stdin     *os.File
	Stdout    io.Writer
	Stderr    io.Writer
	Debug     bool
	Getenv    func(string) string

	// Test hooks: a non-empty testImage skips image builds and the
	// paddock-init mount and runs testEntrypoint with testArgs instead.
	testImage      string
	testEntrypoint string
	testArgs       []string
	selfExe        string        // watchdog executable; default os.Executable()
	cancelGrace    time.Duration // how long after ctx is cancelled the host waits before `container stop`
	secretsGrace   time.Duration // how long a running container may keep secrets.env before the host removes it
}

func (e *Engine) bin() string {
	if e.Bin != "" {
		return e.Bin
	}
	return "container"
}

func (e *Engine) debugf(format string, a ...any) {
	if e.Debug {
		fmt.Fprintf(e.Stderr, "paddock: "+format+"\n", a...)
	}
}
