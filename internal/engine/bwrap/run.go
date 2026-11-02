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
	// The config layer rejects allowlist profiles for this engine; the check
	// here covers plans built by another path.
	if p.Network.Policy == initcfg.PolicyAllowlist {
		return 1, fmt.Errorf("network policy %q cannot be enforced by the bwrap engine", p.Network.Policy)
	}
	if o.Verify {
		return 1, errors.New("verify is not supported by the bwrap engine: there is no paddock-init to run the guest checks")
	}
	secrets, err := runstage.ResolveSecrets(p.Env, e.lookup)
	if err != nil {
		return 1, err
	}
	dirs, err := runstage.CreateRunDirs(e.StateRoot, mustRunName())
	if err != nil {
		return 1, err
	}
	defer dirs.Remove()
	home := filepath.Join(dirs.Root, "home")
	tdir := runstage.TranscriptDir(e.Home, p.Workdir)

	var pass []string
	for _, k := range runstage.PassthroughEnv {
		if v := e.getenv(k); v != "" {
			pass = append(pass, k+"="+v)
		}
	}
	cfg, err := runstage.BuildInitConfig(p, runstage.InitInput{
		User:          runstageHostUser(home),
		TranscriptDir: tdir,
		Passthrough:   pass,
	})
	if err != nil {
		return 1, err
	}
	if err := prepareHome(home, p.Claude, cfg, tdir); err != nil {
		return 1, err
	}
	mounts, err := planMounts(p)
	if err != nil {
		return 1, err
	}
	mounts = append(mounts,
		runstage.GuestMount{Host: home, Guest: home},
		runstage.GuestMount{Host: tdir, Guest: tdir},
	)
	argv := e.runArgv(runSpec{
		NetNone: p.Network.Policy == initcfg.PolicyNone,
		Mounts:  mounts,
		Workdir: p.Workdir,
		Argv:    p.Argv,
	}, home)
	e.debugf("%s", strings.Join(argv, " "))

	// Notify before the start, so a signal that lands while bwrap is
	// starting waits in the channel for the forwarder.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr
	cmd.Env = childEnv(cfg, secrets, home)
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
	if secrets != nil {
		// The values are already in the child's environment; the artifact
		// goes, like the container engines' post-init cleanup.
		os.Remove(filepath.Join(dirs.Secrets, runstage.SecretsFile))
	}

	done := make(chan struct{})
	go e.forwardSignals(ctx, cmd, sigs, done)

	waitErr := cmd.Wait()
	close(done)

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

// childEnv builds the child's environment: the invoking environment, the
// plan's literal env and passthrough variables (as the init config would
// have staged them), the resolved secrets, and HOME pointing at the
// throwaway home.
func childEnv(cfg *initcfg.Config, secrets []byte, home string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, cfg.Env...)
	if secrets != nil {
		env = append(env, strings.Split(strings.TrimRight(string(secrets), "\n"), "\n")...)
	}
	return append(env, "HOME="+home)
}

// runstageHostUser returns the invoking user with the guest's throwaway
// home, as the init config's user block. The uid and gid are the host's, so
// files written to the workspace keep host ownership.
func runstageHostUser(home string) initcfg.User {
	u, err := runstage.HostUser()
	if err != nil {
		// The container engines fail the run on this; here the user block
		// only feeds the init config's validation, so a broken lookup is a
		// zero user, which the validation rejects.
		return initcfg.User{Home: home}
	}
	u.Home = home
	return u
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

// mustRunName defers to runstage.NewRunName; the error path is a broken
// entropy source, which the run reports as a failure to start.
func mustRunName() string {
	n, err := runstage.NewRunName()
	if err != nil {
		panic(err)
	}
	return n
}
