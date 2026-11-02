// Package applecontainer runs plans in Apple `container` VMs.
package applecontainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

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
	if p.Engine != "" && p.Engine != config.EngineAppleContainer {
		return 1, fmt.Errorf("plan engine %q is not %s", p.Engine, config.EngineAppleContainer)
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

	argv := runArgv(runSpec{
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
	e.debugf("%s %v", e.bin(), argv)

	cmd := exec.Command(e.bin(), argv...)
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
		return 1, fmt.Errorf("container run: %w", err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.supervisor().ForwardSignals(ctx, name, sigs, done)
	}()
	if secrets != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.supervisor().RemoveSecretsOnceRunning(name, dirs.Secrets, done)
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

// cli runs `container args...` with its output discarded, bounded by
// cliTimeout. It never uses the run's ctx: kill goes through it after that
// ctx is cancelled.
func (e *Engine) cli(args ...string) error {
	_, err := e.cliOutput(context.Background(), args...)
	return err
}

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

// orphanStopGrace is the stop grace, in seconds, for a container whose
// paddock side is gone: the `container run` client died (Run) or paddock
// itself died (the watchdog). Nothing is left to wait on the guest, so
// cleanup is prompt. A cancellation uses Engine.stopGrace instead: it is the
// last step of an escalation that has already tried SIGTERM and waited
// cancelGrace, and the caller chooses how long it lasts.
const orphanStopGrace = 2

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
	sup := &runstage.Supervisor{
		Send:             e.signalContainer,
		Stop:             e.stopContainer,
		IsRunning:        e.isRunning,
		SecretsStartWait: e.secretsStartWait,
		SecretsGrace:     e.secretsGrace,
		CancelGrace:      e.cancelGrace,
		StopGrace:        e.stopGrace,
		Debugf:           e.debugf,
	}
	sup.SignalQueued(e.signalQueued)
	return sup
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

// watchdog is the handle on the running watchdog process.
type watchdog struct {
	w   *os.File
	cmd *exec.Cmd
}

// watchdogReadyTimeout is how long startWatchdog waits for the watchdog to
// signal that it is holding the pipe.
var watchdogReadyTimeout = 5 * time.Second

// startWatchdog re-executes paddock as a session leader holding the read end
// of a pipe; only this process holds the write end. It returns once the
// watchdog has written its ready byte, and fails if that takes longer than
// watchdogReadyTimeout.
func (e *Engine) startWatchdog(name string, d runstage.RunDirs) (*watchdog, error) {
	exe := e.selfExe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, err
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	entry := e.watchdogArgs
	if entry == nil {
		entry = []string{WatchdogCommand}
	}
	args := slices.Concat(entry, []string{e.bin(), name}, d.Removable())
	cmd := exec.Command(exe, args...)
	cmd.ExtraFiles = []*os.File{r, readyW}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	r.Close()
	readyW.Close()
	if err != nil {
		w.Close()
		readyR.Close()
		return nil, fmt.Errorf("start watchdog: %w", err)
	}
	defer readyR.Close()
	readyR.SetReadDeadline(time.Now().Add(watchdogReadyTimeout))
	var b [1]byte
	if n, _ := readyR.Read(b[:]); n != 1 || b[0] != watchdogReady {
		cmd.Process.Kill()
		cmd.Wait()
		w.Close()
		return nil, fmt.Errorf("watchdog %s %q did not become ready within %s; the program must dispatch the arguments %q to applecontainer.RunWatchdog", exe, entry, watchdogReadyTimeout, entry)
	}
	return &watchdog{w: w, cmd: cmd}, nil
}

// finish tells the watchdog the run ended normally and reaps it.
func (w *watchdog) finish() {
	w.w.Write([]byte{watchdogDone})
	w.w.Close()
	w.cmd.Wait()
}
