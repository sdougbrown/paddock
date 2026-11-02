package applecontainer

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"encoding/json"
	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
	"github.com/sdougbrown/paddock/internal/resolve"
	"github.com/sdougbrown/paddock/internal/runstage"
)

var _ engine.Engine = (*Engine)(nil)

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
		e.StateRoot = filepath.Join(e.Home, ".local", "state", "paddock")
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

// Run implements engine.Engine.
func (e *Engine) Run(ctx context.Context, p *plan.Plan, o engine.Options) (int, error) {
	if err := e.defaults(); err != nil {
		return 1, err
	}
	return runstage.ContainerRun(ctx, p, o.Verify, e.containerOps())
}

// containerOps carries the container CLI's shape into the shared container
// sequence.
func (e *Engine) containerOps() runstage.ContainerOps {
	return runstage.ContainerOps{
		EngineName:       config.EngineAppleContainer,
		RunLabel:         "container run",
		TestImage:        e.testImage,
		TestEntrypoint:   e.testEntrypoint,
		TestArgs:         e.testArgs,
		TestStartDelay:   e.testStartDelay,
		Getenv:           e.getenv,
		Lookup:           e.lookup,
		Home:             e.Home,
		StateRoot:        e.StateRoot,
		CacheRoot:        e.CacheRoot,
		Stdin:            e.Stdin,
		Stdout:           e.Stdout,
		Stderr:           e.Stderr,
		CancelGrace:      e.cancelGrace,
		StopGrace:        e.stopGrace,
		SecretsGrace:     e.secretsGrace,
		SecretsStartWait: e.secretsStartWait,
		SignalQueued:     e.signalQueued,
		Debugf:           e.debugf,
		SignalContainer:  e.signalContainer,
		StopContainer:    e.stopContainer,
		IsRunning:        e.isRunning,
		EnsureImages: func(ctx context.Context, p *plan.Plan) (string, string, string, error) {
			image, err := e.ensureImages(ctx, p)
			if err != nil {
				return "", "", "", err
			}
			src, err := runstage.LocateInitBinary(e.getenv, os.Executable)
			if err != nil {
				return "", "", "", err
			}
			binDir, err := runstage.StageInitBinary(e.CacheRoot, src)
			if err != nil {
				return "", "", "", err
			}
			return image, binDir, initcfg.InitPath, nil
		},
		Watchdog: func(name string, dirs runstage.RunDirs) (runstage.Finisher, error) {
			return e.startWatchdog(name, dirs)
		},
		Argv: func(s runstage.ContainerArgv) []string {
			return runArgv(runSpec{
				Name:       s.Name,
				TTY:        s.TTY,
				CPUs:       s.CPUs,
				Memory:     s.Memory,
				Mounts:     s.Mounts,
				Workdir:    s.Workdir,
				Env:        s.Env,
				Entrypoint: s.Entrypoint,
				Image:      s.Image,
				Args:       s.Args,
			})
		},
	}
}

// supervisor builds the shared supervision for this run: the engine supplies
// the CLI's send, stop, and running checks; the queue, retry cadence,
// cancellation escalation, and secrets cleanup are runstage's.
func (e *Engine) supervisor() *runstage.Supervisor {
	return runstage.NewSupervisor(runstage.SupervisorConfig{
		Send:             e.signalContainer,
		Stop:             e.stopContainer,
		IsRunning:        e.isRunning,
		SecretsStartWait: e.secretsStartWait,
		SecretsGrace:     e.secretsGrace,
		CancelGrace:      e.cancelGrace,
		StopGrace:        e.stopGrace,
		Debugf:           e.debugf,
		SignalQueued:     e.signalQueued,
	})
}

// cli runs `container args...` with its output discarded, bounded by
// cliTimeout. It never uses the run's ctx: kill goes through it after that
// ctx is cancelled.
func (e *Engine) cli(args ...string) error {
	_, err := e.cliOutput(context.Background(), args...)
	return err
}

// orphanStopGrace is the stop grace, in seconds, for a container whose
// paddock side is gone: the `container run` client died (Run) or paddock
// itself died (the watchdog). Nothing is left to wait on the guest, so
// cleanup is prompt. A cancellation uses Engine.stopGrace instead: it is the
// last step of an escalation that has already tried SIGTERM and waited
// cancelGrace, and the caller chooses how long it lasts.
const orphanStopGrace = 2

// stopArgs is the `container stop` argv that stops name, killing it after
// grace seconds. Run and the watchdog both stop containers with it.
func stopArgs(name string, grace int) []string {
	return []string{"stop", "--time", strconv.Itoa(grace), name}
}

// stopTimeout bounds a `container stop` whose grace is grace seconds:
// cliTimeout plus the grace.
func stopTimeout(grace int) time.Duration {
	return cliTimeout + time.Duration(grace)*time.Second
}

// stopContainer asks the container to stop, killing it after secs seconds.
// Like cli, it never uses the run's ctx.
func (e *Engine) stopContainer(name string, secs int) error {
	_, err := e.cliRun(context.Background(), stopTimeout(secs), stopArgs(name, secs)...)
	return err
}

// signalContainer delivers sig to the guest and reports whether it was
// delivered. A kill that succeeds while the VM is still booting can be
// acknowledged and dropped, so delivery counts only when the container was
// observed running immediately before the kill.
func (e *Engine) signalContainer(name string, sig os.Signal) bool {
	n, ok := runstage.SignalName(sig)
	if !ok {
		return false
	}
	if !e.isRunning(name) {
		e.debugf("%s not running; %s stays pending", name, n)
		return false
	}
	e.debugf("forwarding %s to %s", n, name)
	if err := e.cli("kill", "--signal", n, name); err != nil {
		e.debugf("container kill: %v", err)
		return false
	}
	return true
}

func (e *Engine) isRunning(name string) bool {
	out, err := e.cliOutput(context.Background(), "ls", "--format", "json")
	if err != nil {
		return false
	}
	var list []struct {
		Configuration struct {
			ID string `json:"id"`
		} `json:"configuration"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if json.Unmarshal(out, &list) != nil {
		return false
	}
	for _, c := range list {
		if c.Configuration.ID == name {
			return c.Status.State == "running"
		}
	}
	return false
}

// startWatchdog re-executes paddock as a session leader holding the read
// end of a pipe, with the run's container binary and name as the watchdog's
// tail arguments. The handshake lives in runstage.
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
	return runstage.StartWatchdog(exe, entry, append([]string{e.bin(), name}, d.Removable()...))
}
