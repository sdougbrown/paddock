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
	"os/user"
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
	if e.secretsGrace == 0 {
		e.secretsGrace = 30 * time.Second
	}
	return nil
}

// isTTY reports whether f is a terminal; /dev/null is a character device but not one.
func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(st, null)
}

func hostUser() (initcfg.User, error) {
	u, err := user.Current()
	if err != nil {
		return initcfg.User{}, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return initcfg.User{}, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return initcfg.User{}, err
	}
	return initcfg.User{Name: u.Username, UID: uid, GID: gid, Home: u.HomeDir}, nil
}

// passthroughEnv names the invoking environment's variables that reach the
// guest. Run passes the set ones to paddock-init both in the init config,
// where the plan's env overrides them, and with `container run -e`.
var passthroughEnv = []string{"TERM", "COLORTERM", "LANG"}

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
	secrets, err := resolveSecrets(p.Env, e.lookup)
	if err != nil {
		return 1, err
	}
	u, err := hostUser()
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
		src, err := locateInitBinary(e.getenv, os.Executable)
		if err != nil {
			return 1, err
		}
		if binDir, err = stageInitBinary(e.CacheRoot, src); err != nil {
			return 1, err
		}
		entry = initcfg.InitPath
	}

	name, err := newRunName()
	if err != nil {
		return 1, err
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	dirs, err := createRunDirs(e.StateRoot, name)
	if err != nil {
		return 1, err
	}
	var wd *watchdog
	defer func() {
		dirs.remove()
		if wd != nil {
			wd.finish()
		}
	}()
	if wd, err = e.startWatchdog(name, dirs); err != nil {
		return 1, err
	}

	tdir := transcriptDir(e.Home, p.Workdir)
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		return 1, err
	}
	var pass []string
	for _, k := range passthroughEnv {
		if v := e.getenv(k); v != "" {
			pass = append(pass, k+"="+v)
		}
	}
	layout := mountLayout{Dirs: dirs, BinDir: binDir, TranscriptDir: tdir}
	if len(p.Claude.ManagedSettings) > 0 {
		layout.UserSettings = filepath.Join(dirs.Settings, userSettingsFile)
		layout.UserSettingsAt = userSettingsPath(u.Home)
	}
	mounts, err := guestMounts(p, layout)
	if err != nil {
		return 1, err
	}
	cfg, err := buildInitConfig(p, initInput{User: u, Verify: o.Verify, TranscriptDir: tdir, Mounts: mounts, Passthrough: pass})
	if err != nil {
		return 1, err
	}
	if err := initcfg.Write(filepath.Join(dirs.State, "init.json"), cfg); err != nil {
		return 1, err
	}
	if err := copySeedFiles(dirs.State, p.Claude.SeedFiles); err != nil {
		return 1, err
	}
	if layout.UserSettings != "" {
		if err := os.WriteFile(layout.UserSettings, p.Claude.ManagedSettings, 0o644); err != nil {
			return 1, err
		}
	}
	if secrets != nil {
		if err := writeSecrets(dirs.Secrets, secrets); err != nil {
			return 1, err
		}
	}

	argv := runArgv(runSpec{
		Name:       name,
		TTY:        isTTY(e.Stdin),
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

	if err := ctx.Err(); err != nil {
		return 1, err
	}
	time.Sleep(e.testStartDelay)
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("container run: %w", err)
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
			e.stopContainer(name, 2)
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

// stopContainer asks the container to stop, killing it after secs seconds.
// Like cli, it never uses the run's ctx.
func (e *Engine) stopContainer(name string, secs int) error {
	_, err := e.cliRun(context.Background(), stopTimeout(secs), stopArgs(name, secs)...)
	return err
}

// forwardSignals delivers signals received by paddock to the guest process,
// because `container run` does not forward them. A signal that cannot yet be
// delivered, such as one that arrives while the VM is booting, stays pending
// and is retried every pendingRetry until it is delivered or done closes. When ctx is cancelled the
// guest gets SIGTERM, and `container stop` follows after cancelGrace.
func (e *Engine) forwardSignals(ctx context.Context, name string, sigs <-chan os.Signal, done <-chan struct{}) {
	var pending os.Signal
	var stopAt <-chan time.Time
	stopDue, stopped := false, false
	cancelled := ctx.Done()
	retry := time.NewTicker(pendingRetry)
	defer retry.Stop()
	for {
		select {
		case s := <-sigs:
			if e.signalContainer(name, s) {
				pending = nil
			} else if _, ok := signalNames[s]; ok {
				pending = s
			}
		case <-cancelled:
			cancelled = nil
			if !e.signalContainer(name, syscall.SIGTERM) {
				pending = syscall.SIGTERM
			}
			stopAt = time.After(e.cancelGrace)
		case <-stopAt:
			stopAt, stopDue = nil, true
		case <-retry.C:
			if pending != nil && e.signalContainer(name, pending) {
				pending = nil
			}
		case <-done:
			return
		}
		if stopDue && !stopped {
			e.debugf("stopping %s after cancellation", name)
			stopped = e.stopContainer(name, 10) == nil
		}
	}
}

// signalContainer delivers s to the guest process and reports whether it was
// delivered. A kill that succeeds while the VM is still booting can be
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
		e.debugf("container kill: %v", err)
		return false
	}
	return true
}

// removeSecretsOnceRunning polls until the container is running, then waits
// up to secretsGrace for paddock-init to unlink secrets.env itself before
// removing it. It gives up waiting for the container after 30 seconds or when
// done closes.
func (e *Engine) removeSecretsOnceRunning(name, dir string, done <-chan struct{}) {
	file := filepath.Join(dir, secretsFile)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return
		case <-time.After(200 * time.Millisecond):
		}
		if e.isRunning(name) {
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
			return
		}
	}
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
func (e *Engine) startWatchdog(name string, d runDirs) (*watchdog, error) {
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
	args := slices.Concat(entry, []string{e.bin(), name}, d.removable())
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
