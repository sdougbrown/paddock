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
	"github.com/sdougbrown/paddock/internal/envname"
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
	// The config layer rejects allowlist profiles and limits for this
	// engine; the checks here cover plans built by another path.
	if p.Network.Policy == initcfg.PolicyAllowlist {
		return 1, fmt.Errorf("network policy %q cannot be enforced by the bwrap engine", p.Network.Policy)
	}
	if p.Limits.CPUs != 0 || p.Limits.Memory != "" {
		return 1, fmt.Errorf("limits: the bwrap engine cannot enforce cpu or memory limits; remove them or use a container engine")
	}
	if o.Verify {
		return 1, errors.New("verify is not supported by the bwrap engine: there is no paddock-init to run the guest checks")
	}
	tdir := runstage.TranscriptDir(e.Home, p.Workdir)
	if err := planMountTargets(p, tdir); err != nil {
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
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		return 1, fmt.Errorf("transcript dir: %w", err)
	}

	var pass []string
	for _, k := range runstage.PassthroughEnv {
		if v := e.getenv(k); v != "" {
			pass = append(pass, k+"="+v)
		}
	}
	cfg, err := runstage.BuildInitConfig(p, runstage.InitInput{
		User: u,
		// ~/.claude/projects must hold the per-project dir: point the
		// symlink at its parent, so <enc> under it is the bound transcript
		// dir and Claude's files land at the canonical TranscriptDir path.
		TranscriptDir: filepath.Dir(tdir),
		Passthrough:   pass,
	})
	if err != nil {
		return 1, err
	}
	u.Home = home
	cfg.User = u
	// The shared seeding writes managed settings at the guest's managed
	// path, which the read-only host rootfs must not receive: the engine
	// pins user-scope settings with the read-only bind instead, and stages
	// the seed files into the run's state directory for the seeding to copy.
	managed := cfg.Claude.ManagedSettings
	cfg.Claude.ManagedSettings = nil
	if err := runstage.CopySeedFiles(dirs.State, p.Claude.SeedFiles); err != nil {
		return 1, err
	}
	fs := initcfg.NewHomeFS("/", cfg)
	fs.SeedDir = filepath.Join(dirs.State, filepath.Base(initcfg.SeedDir))
	var settings string
	if len(managed) > 0 {
		settings = filepath.Join(dirs.Settings, runstage.UserSettingsFile)
		if err := os.WriteFile(settings, managed, 0o644); err != nil {
			return 1, err
		}
	}
	if err := initcfg.PrepareHome(fs, cfg); err != nil {
		return 1, err
	}
	mounts, err := planMounts(p)
	if err != nil {
		return 1, err
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
	if len(managed) > 0 {
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
	// Tier-0 inherits only PATH from the invoking environment: the host
	// toolchain lives on it, and everything else (tokens, broker URLs,
	// shell history) stays out of the run.
	path, hasPath := os.LookupEnv("PATH")
	inherited := []string{}
	if hasPath {
		inherited = append(inherited, "PATH="+path)
	}
	lines, err := secretsLines(secrets)
	if err != nil {
		return 1, err
	}
	env, err := initcfg.AgentEnv(inherited, u, cfg.Env, lines)
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

	code := 0
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		code = exitErr.ExitCode()
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
	default:
		return 1, waitErr
	}
	return code, nil
}

// planMountTargets rejects plan mounts that would collide with the
// transcript directory the engine mounts itself.
func planMountTargets(p *plan.Plan, tdir string) error {
	for _, m := range p.Mounts {
		if runstage.AtOrUnder(m.Guest, tdir) {
			return fmt.Errorf("plan mount %s targets the transcript directory the engine mounts itself", m.Host)
		}
	}
	return nil
}

// secretsLines splits the resolved secrets file content into KEY=VALUE
// entries for the child environment. A line without a separator, or with a
// name that is not a valid environment name, is an error that names only
// the line and the name: a value may contain anything, and it must never
// reach an error message.
func secretsLines(secrets []byte) ([]string, error) {
	if secrets == nil {
		return nil, nil
	}
	var out []string
	for i, line := range strings.Split(strings.TrimRight(string(secrets), "\n"), "\n") {
		name, _, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("secrets file line %d has no NAME=VALUE separator", i+1)
		}
		if !envname.Valid(name) {
			return nil, fmt.Errorf("secrets file line %d: %q is not a valid environment name", i+1, name)
		}
		out = append(out, line)
	}
	return out, nil
}

// planMounts validates and copies the plan's mounts.
func planMounts(p *plan.Plan) ([]runstage.GuestMount, error) {
	var ms []runstage.GuestMount
	for _, m := range p.Mounts {
		if err := m.Check(); err != nil {
			return nil, fmt.Errorf("plan mount %s: %w", m.Host, err)
		}
		ms = append(ms, runstage.GuestMount{Host: m.Host, Guest: m.Guest, ReadOnly: m.ReadOnly})
	}
	return ms, nil
}
