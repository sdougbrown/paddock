// Package podman runs plans in rootful podman containers.
package podman

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/sdougbrown/paddock/internal/resolve"
)

// Engine runs plans with the `podman` CLI. The zero value is usable.
//
// Podman must be rootful: paddock-init needs CAP_NET_ADMIN in the guest to
// commit the egress ruleset, and a rootless container never grants it. Set
// Sudo, or PADDOCK_PODMAN_SUDO=1, when the invoking user reaches rootful
// podman through sudo; every podman argv, the watchdog included, then runs
// through `sudo`.
//
// Cleanup after a crash of the paddock process (SIGKILL, power loss of the
// parent) is done by a watchdog that Run starts by re-executing the running
// binary with the internal WatchdogCommand as its first argument. The one
// thing a program embedding Engine must wire is a call to Dispatch with its
// arguments before it handles them; otherwise the watchdog never signals
// readiness and Run fails.
type Engine struct {
	Bin       string // podman CLI; default "podman"
	Sudo      bool   // run every podman command through sudo
	Home      string // default: the user's home directory
	StateRoot string // default: <home>/.local/state/paddock
	CacheRoot string // default: resolve.CacheRoot(home)
	Stdin     *os.File
	Stdout    io.Writer
	Stderr    io.Writer
	Debug     bool
	Getenv    func(string) string

	// Test hooks: a non-empty testImage skips image builds and the
	// paddock-init mount and runs testEntrypoint with testArgs instead.
	testImage        string
	testEntrypoint   string
	testArgs         []string
	selfExe          string          // watchdog executable; default os.Executable()
	watchdogArgs     []string        // arguments that make selfExe run RunWatchdog; default [WatchdogCommand]
	cancelGrace      time.Duration   // how long after ctx is cancelled the host waits before `podman stop`
	stopGrace        int             // seconds that `podman stop` after a cancel gives the guest before killing it
	secretsGrace     time.Duration   // how long a running container may keep secrets.env before the host removes it
	secretsStartWait time.Duration   // how long the host waits for the container to run before it removes secrets.env anyway
	testStartDelay   time.Duration   // pause between preparing the run and starting `podman run`
	signalQueued     func(os.Signal) // called after forwardSignals queues a signal and tries the queue once
}

func (e *Engine) getenv(k string) string {
	if e.Getenv != nil {
		return e.Getenv(k)
	}
	return os.Getenv(k)
}

func (e *Engine) lookup(k string) (string, bool) {
	if e.Getenv != nil {
		v := e.Getenv(k)
		return v, v != ""
	}
	return os.LookupEnv(k)
}

func (e *Engine) defaults() error {
	if e.Stdin == nil {
		e.Stdin = os.Stdin
	}
	if e.Stdout == nil {
		e.Stdout = os.Stdout
	}
	if e.Stderr == nil {
		e.Stderr = os.Stderr
	}
	if e.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		e.Home = h
	}
	if e.StateRoot == "" {
		e.StateRoot = "/var/lib/paddock"
	}
	if e.CacheRoot == "" {
		e.CacheRoot = resolve.CacheRoot(e.Home)
	}
	if e.cancelGrace == 0 {
		e.cancelGrace = 10 * time.Second
	}
	if e.stopGrace == 0 {
		e.stopGrace = 10
	}
	if e.secretsGrace == 0 {
		e.secretsGrace = 30 * time.Second
	}
	if e.secretsStartWait == 0 {
		e.secretsStartWait = 2 * time.Minute
	}
	return nil
}

func (e *Engine) bin() string {
	if e.Bin != "" {
		return e.Bin
	}
	return "podman"
}

// invocation is the base command that runs podman: the binary alone, or
// `sudo` followed by it when Sudo is set.
func (e *Engine) invocation() []string {
	if e.Sudo {
		return []string{"sudo", e.bin()}
	}
	return []string{e.bin()}
}

func (e *Engine) debugf(format string, a ...any) {
	if e.Debug {
		fmt.Fprintf(e.Stderr, "paddock: "+format+"\n", a...)
	}
}

// cliTimeout bounds each short `podman` CLI call.
const cliTimeout = 5 * time.Second

// cliOutput runs one short `podman` CLI call under ctx, bounded by
// cliTimeout, and returns its standard output.
func (e *Engine) cliOutput(ctx context.Context, args ...string) ([]byte, error) {
	return e.cliRun(ctx, cliTimeout, args...)
}

// cliRun runs `podman args...` under ctx, bounded by timeout, and returns
// its standard output. Every `podman` call except build and run goes
// through it. WaitDelay bounds the wait for the output pipe after a timeout
// kill, in case a child of the CLI still holds it.
func (e *Engine) cliRun(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	inv := e.invocation()
	cmd := exec.CommandContext(c, inv[0], append(inv[1:], args...)...)
	cmd.WaitDelay = time.Second
	return cmd.Output()
}
