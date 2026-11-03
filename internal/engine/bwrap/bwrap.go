package bwrap

import (
	"github.com/sdougbrown/paddock/internal/engine"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sdougbrown/paddock/internal/initcfg"
)

// Engine runs plans with the `bwrap` CLI. The zero value is usable.
//
// Tier-0 isolation: namespaces only, the host rootfs read-only, the agent
// running as the invoking user. Work isolation, not process isolation;
// the egress policy is limited to host (unchanged network) and none
// (no network), and limits are not enforced. There is no paddock-init
// and no image: argv runs directly against the host toolchain. The
// watchdog's job falls to --die-with-parent plus a parent-death signal:
// when the paddock process dies the kernel kills bwrap (PDEATHSIG on
// Linux), and bwrap kills the agent.
type Engine struct {
	Bin       string                       // bwrap CLI; default "bwrap"
	Home      string                       // the host home the transcript dir lives under; default: the user's home directory
	StateRoot string                       // default: <home>/.local/state/paddock
	hostUser  func() (initcfg.User, error) // default: runstage.HostUser; tests inject
	Stdin     *os.File
	Stdout    io.Writer
	Stderr    io.Writer
	Debug     bool
	Getenv    func(string) string

	cancelGrace    time.Duration // how long after ctx is cancelled the host waits before SIGKILL
	testStartDelay time.Duration // pause between preparing the run and starting bwrap
	signalQueued   func(os.Signal)
}

func (e *Engine) getenv(k string) string {
	return engine.Env(e.Getenv, k)
}

func (e *Engine) lookup(k string) (string, bool) {
	return engine.LookupEnv(e.Getenv, k)
}

func (e *Engine) defaults() error {
	engine.DefaultStreams(&e.Stdin, &e.Stdout, &e.Stderr)
	if e.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		e.Home = h
	}
	if e.StateRoot == "" {
		e.StateRoot = filepath.Join(e.Home, ".local", "state", "paddock")
	}
	if e.cancelGrace == 0 {
		e.cancelGrace = 10 * time.Second
	}
	return nil
}

func (e *Engine) bin() string {
	if e.Bin != "" {
		return e.Bin
	}
	return "bwrap"
}

func (e *Engine) debugf(format string, a ...any) {
	if e.Debug {
		engine.Debugf(e.Stderr, format, a...)
	}
}
