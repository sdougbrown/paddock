package podman

import (
	"context"
	"os"
	"strings"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
	"github.com/sdougbrown/paddock/internal/runstage"
)

var _ engine.Engine = (*Engine)(nil)

// Run implements engine.Engine.
func (e *Engine) Run(ctx context.Context, p *plan.Plan, o engine.Options) (int, error) {
	if err := e.defaults(); err != nil {
		return 1, err
	}
	return runstage.ContainerRun(ctx, p, o.Verify, e.containerOps())
}

// containerOps carries the podman CLI's shape into the shared container
// sequence.
func (e *Engine) containerOps() runstage.ContainerOps {
	return runstage.ContainerOps{
		EngineName:       config.EnginePodman,
		RunLabel:         "podman run",
		TestImage:        e.testImage,
		TestEntrypoint:   e.testEntrypoint,
		TestArgs:         e.testArgs,
		TestStartDelay:   e.testStartDelay,
		Getenv:           e.getenv,
		Lookup:           e.lookup,
		Home:             e.Home,
		StateRoot:        e.StateRoot,
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
			return e.runArgv(runSpec{
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

// cli runs `podman args...` with its output discarded, bounded by
// cliTimeout. It never uses the run's ctx: kill goes through it after that
// ctx is cancelled.
func (e *Engine) cli(args ...string) error {
	_, err := e.cliOutput(context.Background(), args...)
	return err
}

// stopContainer asks the container to stop, killing it after secs seconds.
// Like cli, it never uses the run's ctx.
func (e *Engine) stopContainer(name string, secs int) error {
	_, err := e.cliRun(context.Background(), stopTimeout(secs), stopArgs(name, secs)...)
	return err
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

// signalContainer delivers sig to the guest and reports whether it was
// delivered. A kill that succeeds while the container is still booting can
// be acknowledged and dropped, so delivery counts only when the container
// was observed running immediately before the kill.
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
		e.debugf("podman kill: %v", err)
		return false
	}
	return true
}

// isRunning reports whether name is a running container. A missing container
// is not running; an inspect failure is reported as not running too, since
// the callers only use the answer to gate signals and secret cleanup.
func (e *Engine) isRunning(name string) bool {
	out, err := e.cliOutput(context.Background(), "inspect", "--format", "{{.State.Running}}", name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}
