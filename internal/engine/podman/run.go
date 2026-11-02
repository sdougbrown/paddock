package podman

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
	if p.Engine != "" && p.Engine != config.EnginePodman {
		return 1, fmt.Errorf("plan engine %q is not %s", p.Engine, config.EnginePodman)
	}
	if p.Workdir == "" {
		return 1, errors.New("plan has no workdir")
	}
	secrets, err := runstage.ResolveSecrets(p.Env, e.lookup)
	if err != nil {
		return 1, err
	}
	u, err := runstage.HostUser()
	if err != nil {
		return 1, fmt.Errorf("host user: %w", err)
	}
	// The guest HOME is the host's $HOME, from which avenor derives the
	// Claude transcript path.
	u.Home = e.Home

	image, binDir, entry := e.testImage, "", e.testEntrypoint
	if image == "" {
		if image, err = e.ensureImages(ctx, p); err != nil {
			return 1, err
		}
		src, err := runstage.LocateInitBinary(e.getenv, os.Executable)
		if err != nil {
			return 1, err
		}
		if binDir, err = runstage.StageInitBinary(e.CacheRoot, src); err != nil {
			return 1, err
		}
		entry = initcfg.InitPath
	}

	name, err := runstage.NewRunName()
	if err != nil {
		return 1, err
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	dirs, err := runstage.CreateRunDirs(e.StateRoot, name)
	if err != nil {
		return 1, err
	}
	var wd *watchdog
	defer func() {
		dirs.Remove()
		if wd != nil {
			wd.finish()
		}
	}()
	if wd, err = e.startWatchdog(name, dirs); err != nil {
		return 1, err
	}

	tdir := runstage.TranscriptDir(e.Home, p.Workdir)
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		return 1, err
	}
	var pass []string
	for _, k := range runstage.PassthroughEnv {
		if v := e.getenv(k); v != "" {
			pass = append(pass, k+"="+v)
		}
	}
	layout := runstage.MountLayout{Dirs: dirs, BinDir: binDir, TranscriptDir: tdir}
	if len(p.Claude.ManagedSettings) > 0 {
		layout.UserSettings = filepath.Join(dirs.Settings, runstage.UserSettingsFile)
		layout.UserSettingsAt = runstage.UserSettingsPath(u.Home)
	}
	mounts, err := runstage.GuestMounts(p, layout)
	if err != nil {
		return 1, err
	}
	cfg, err := runstage.BuildInitConfig(p, runstage.InitInput{User: u, Verify: o.Verify, TranscriptDir: tdir, Mounts: mounts, Passthrough: pass})
	if err != nil {
		return 1, err
	}
	if err := initcfg.Write(filepath.Join(dirs.State, filepath.Base(initcfg.ConfigPath)), cfg); err != nil {
		return 1, err
	}
	if err := runstage.CopySeedFiles(dirs.State, p.Claude.SeedFiles); err != nil {
		return 1, err
	}
	if layout.UserSettings != "" {
		if err := os.WriteFile(layout.UserSettings, p.Claude.ManagedSettings, 0o644); err != nil {
			return 1, err
		}
	}
	if secrets != nil {
		if err := runstage.WriteSecrets(dirs.Secrets, secrets); err != nil {
			return 1, err
		}
	}

	argv := e.runArgv(runSpec{
		Name:       name,
		TTY:        runstage.IsTTY(e.Stdin),
		CPUs:       p.Limits.CPUs,
		Memory:     p.Limits.Memory,
		Mounts:     mounts,
		Workdir:    p.Workdir,
		Env:        pass,
		Entrypoint: entry,
		Image:      image,
		Args:       e.testArgs,
	})
	e.debugf("%s", strings.Join(argv, " "))

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr

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
		return 1, fmt.Errorf("podman run: %w", err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.forwardSignals(ctx, name, sigs, done)
	}()
	if secrets != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.removeSecretsOnceRunning(name, dirs.Secrets, done)
		}()
	}

	waitErr := cmd.Wait()
	close(done)
	wg.Wait()

	code := 0
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		code = exitErr.ExitCode()
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
			// The client died without the container's own exit; stop it.
			e.stopContainer(name, orphanStopGrace)
		}
	default:
		return 1, waitErr
	}
	return code, nil
}

var signalNames = map[os.Signal]string{
	syscall.SIGTERM: "TERM",
	syscall.SIGINT:  "INT",
	syscall.SIGHUP:  "HUP",
}

// pendingRetry is how often an undelivered signal or stop is retried.
const pendingRetry = 200 * time.Millisecond

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

// forwardSignals delivers signals received by paddock to the guest process,
// because `podman run` does not forward them reliably across its conmon
// parent. A signal that cannot yet be delivered, such as one that arrives
// while the container is booting, is queued, and the queue is retried in
// arrival order every pendingRetry until it is empty or done closes. When
// ctx is cancelled the guest gets SIGTERM, and `podman stop` with stopGrace
// follows after cancelGrace.
func (e *Engine) forwardSignals(ctx context.Context, name string, sigs <-chan os.Signal, done <-chan struct{}) {
	var pending runstage.SignalQueue
	send := func(s os.Signal) bool { return e.signalContainer(name, s) }
	queue := func(s os.Signal) {
		pending.Push(s)
		pending.Deliver(send)
		if e.signalQueued != nil {
			e.signalQueued(s)
		}
	}
	var stopAt <-chan time.Time
	stopDue, stopped := false, false
	cancelled := ctx.Done()
	retry := time.NewTicker(pendingRetry)
	defer retry.Stop()
	for {
		select {
		case s := <-sigs:
			if _, ok := signalNames[s]; ok {
				queue(s)
			}
		case <-cancelled:
			cancelled = nil
			queue(syscall.SIGTERM)
			stopAt = time.After(e.cancelGrace)
		case <-stopAt:
			stopAt, stopDue = nil, true
		case <-retry.C:
			pending.Deliver(send)
		case <-done:
			return
		}
		if stopDue && !stopped {
			e.debugf("stopping %s after cancellation", name)
			stopped = e.stopContainer(name, e.stopGrace) == nil
		}
	}
}

// signalContainer delivers s to the guest process and reports whether it was
// delivered. A kill that succeeds while the container is still booting can be
// acknowledged and dropped, so delivery counts only when the container was
// observed running immediately before the kill.
func (e *Engine) signalContainer(name string, s os.Signal) bool {
	n, ok := signalNames[s]
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

// removeSecretsOnceRunning polls until the container is running, then waits
// up to secretsGrace for paddock-init to unlink secrets.env itself before
// removing it. If the container is not running within secretsStartWait, it
// removes secrets.env anyway, so a slow or failed boot cannot leave it on the
// host for the whole run. When done closes it returns at once; Run then
// removes the secrets dir.
func (e *Engine) removeSecretsOnceRunning(name, dir string, done <-chan struct{}) {
	file := filepath.Join(dir, runstage.SecretsFile)
	startBy := time.Now().Add(e.secretsStartWait)
	for !e.isRunning(name) {
		if time.Now().After(startBy) {
			os.Remove(file)
			return
		}
		select {
		case <-done:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	grace := time.Now().Add(e.secretsGrace)
	for time.Now().Before(grace) {
		if _, err := os.Stat(file); err != nil {
			return
		}
		select {
		case <-done:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	os.Remove(file)
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
