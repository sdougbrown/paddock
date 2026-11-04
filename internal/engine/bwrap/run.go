package bwrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

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
	if p.Engine != "" && p.Engine != config.EngineBwrap {
		return 1, fmt.Errorf("plan engine %q is not %s", p.Engine, config.EngineBwrap)
	}
	if p.Workdir == "" {
		return 1, errors.New("plan has no workdir")
	}
	// The config layer rejects allowlist policies and limits for this
	// engine; the shared check covers plans built by another path.
	if err := config.CheckEngineCapabilities(config.EngineBwrap, p.Network.Policy, config.Limits{CPUs: p.Limits.CPUs, Memory: p.Limits.Memory}); err != nil {
		return 1, err
	}
	if o.Verify {
		return 1, errors.New("verify is not supported by the bwrap engine: there is no paddock-init to run the guest checks")
	}
	tdir := runstage.TranscriptDir(e.Home, p.Workdir)
	if err := runstage.CheckMountTargets(p, []string{tdir}); err != nil {
		return 1, err
	}
	hostUser := e.hostUser
	if hostUser == nil {
		hostUser = runstage.HostUser
	}
	u, err := hostUser()
	if err != nil {
		return 1, fmt.Errorf("host user: %w", err)
	}
	if u.UID <= 0 {
		return 1, fmt.Errorf("the bwrap engine needs an unprivileged invoking user; uid %d would give the agent root", u.UID)
	}
	secrets, err := runstage.ResolveSecrets(p.Env, e.lookup)
	if err != nil {
		return 1, err
	}
	name, err := runstage.NewRunName()
	if err != nil {
		return 1, err
	}
	dirs, err := runstage.CreateRunDirs(e.StateRoot, name)
	if err != nil {
		return 1, err
	}
	defer dirs.Remove()
	home := filepath.Join(dirs.Root, "home")
	// The transcript directory binds at its host path, so it must exist
	// before bwrap starts; the home's projects symlink points at it.
	// The config's transcript dir is the symlink target's parent, so the
	// per-project dir under it is the bound transcript dir.
	st, err := runstage.Stage(p, u, e.getenv, e.Home, filepath.Dir(tdir), dirs, nil, false)
	if err != nil {
		return 1, err
	}
	u.Home = home
	st.Cfg.User = u
	// The shared seeding writes managed settings at the guest's managed
	// path, which the read-only host rootfs must not receive: the engine
	// pins user-scope settings with the read-only bind instead.
	st.Cfg.Claude.ManagedSettings = nil
	fs := initcfg.NewHomeFS("/", st.Cfg)
	fs.SeedDir = runstage.StagedSeedDir(dirs)
	settings := st.Settings
	if err := initcfg.PrepareHome(fs, st.Cfg); err != nil {
		return 1, err
	}
	var mounts []runstage.GuestMount
	for _, m := range p.Mounts {
		mounts = append(mounts, runstage.GuestMount{Host: m.Host, Guest: m.Guest, ReadOnly: m.ReadOnly})
	}
	// The transcript dir binds read-write at its host path, so writes through
	// the home's projects symlink reach the caller's path.
	mounts = append(mounts, runstage.GuestMount{Host: tdir, Guest: tdir})
	// A rw workspace lets the agent write project-scope settings that would
	// override the pinned user scope on a later run. Where project-scope
	// settings files exist at staging, the managed settings bind over them
	// read-only; where they do not, the current run's policy is loaded at
	// startup and cannot be changed mid-run. Residual risk 13 covers what
	// remains.
	if settings != "" {
		for _, name := range []string{"settings.json", "settings.local.json"} {
			project := filepath.Join(p.Workdir, ".claude", name)
			if _, err := os.Stat(project); err == nil {
				mounts = append(mounts, runstage.GuestMount{Host: settings, Guest: project, ReadOnly: true})
			}
		}
	}
	argv := e.runArgv(runSpec{
		NetNone: p.Network.Policy == initcfg.PolicyNone,
		Mounts:  mounts,
		Workdir: p.Workdir,
		Argv:    p.Argv,
	}, home, settings)
	e.debugf("%s", strings.Join(argv, " "))

	// Notify before the start, so a signal that lands while bwrap is
	// starting waits in the channel for the forwarder.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	// From here the child is the run's process: the supervisor may deliver
	// signals to it and kill it on escalation.
	running := false
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr
	cmd.SysProcAttr = parentDeathAttr()
	// Tier-0 inherits the curated passthrough set (TERM, COLORTERM, LANG —
	// the same lines a container run forwards) plus only PATH from the
	// invoking environment: the host toolchain lives on it, and everything
	// else (tokens, broker URLs, shell history) stays out of the run.
	path, hasPath := e.lookup("PATH")
	inherited := append([]string{}, st.Pass...)
	if hasPath {
		inherited = append(inherited, "PATH="+path)
	}
	lines, err := secretsLines(secrets)
	if err != nil {
		return 1, err
	}
	env, err := initcfg.AgentEnv(inherited, u, st.Cfg.Env, lines)
	if err != nil {
		return 1, err
	}
	cmd.Env = env
	cmd.Dir = p.Workdir

	if e.testStartDelay > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(e.testStartDelay):
		}
	}
	if err := ctx.Err(); err != nil {
		return 1, err
	}
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("bwrap: %w", err)
	}
	// From here the child is the run's process: the supervisor may deliver
	// signals to it and kill it on escalation.
	running = true

	// The bwrap child is the run's own process: a signal is delivered
	// directly, and the cancellation escalation kills it after the grace.
	sup := runstage.NewSupervisor(runstage.SupervisorConfig{
		Send: func(name string, sig os.Signal) bool {
			if running {
				return cmd.Process.Signal(sig) == nil
			}
			return false
		},
		Stop: func(name string, grace int) error {
			return cmd.Process.Kill()
		},
		IsRunning:    func(name string) bool { return running },
		CancelGrace:  e.cancelGrace,
		Debugf:       e.debugf,
		SignalQueued: e.signalQueued,
	})
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sup.ForwardSignals(ctx, name, sigs, done)
	}()

	waitErr := cmd.Wait()
	close(done)
	wg.Wait()
	// The run's home never carries state a later run needs, so it does not
	// accumulate on disk.
	os.RemoveAll(home)

	code, err := runstage.ExitCode(waitErr)
	if err != nil {
		return 1, err
	}
	return code, nil
}

// secretsLines parses the resolved secrets file through the shared
// initcfg parser, so the container engines and bwrap agree on what a
// valid secrets file is.
func secretsLines(secrets []byte) ([]string, error) {
	if secrets == nil {
		return nil, nil
	}
	return initcfg.ParseSecrets(secrets)
}
