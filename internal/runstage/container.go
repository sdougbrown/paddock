package runstage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
)

// Finisher is a started watchdog; Finish tells it the run ended normally.
type Finisher interface{ Finish() }

// ContainerOps carries the runtime-specific operations of a container
// engine. Everything else in a container run — staging, mounts, the init
// config, supervision, watchdog handshake, exit codes — is shared, in
// ContainerRun.
type ContainerOps struct {
	// EngineName is the plan engine this runtime serves; the plan check and
	// the start error name it.
	EngineName string
	// RunLabel prefixes start errors, e.g. "podman run".
	RunLabel string
	// Test hooks: a non-empty TestImage skips image builds and the
	// paddock-init mount, and runs TestEntrypoint with TestArgs instead.
	TestImage, TestEntrypoint string
	TestArgs                  []string
	TestStartDelay            time.Duration
	// Getenv and Lookup resolve the invoking environment.
	Getenv           func(string) string
	Lookup           func(string) (string, bool)
	Home             string
	StateRoot        string
	CacheRoot        string
	Stdin            *os.File
	Stdout           io.Writer
	Stderr           io.Writer
	CancelGrace      time.Duration
	StopGrace        int
	SecretsGrace     time.Duration
	SecretsStartWait time.Duration
	SignalQueued     func(os.Signal)
	Debugf           func(format string, args ...any)
	// SignalContainer, StopContainer, and IsRunning are the runtime's
	// supervision operations.
	SignalContainer func(name string, sig os.Signal) bool
	StopContainer   func(name string, grace int) error
	IsRunning       func(name string) bool
	// EnsureImages builds the image chain and stages the paddock-init
	// binary. It returns the image tag, the staged binary directory, and
	// the entrypoint path.
	EnsureImages func(ctx context.Context, p *plan.Plan) (image, binDir, entry string, err error)
	// Watchdog starts the crash watchdog for name; Finish on the returned
	// handle tells it the run ended normally.
	Watchdog func(name string, dirs RunDirs) (Finisher, error)
	// Argv builds the full command — the runtime binary first — for the
	// staged run.
	Argv func(s ContainerArgv) []string
}

// ContainerArgv is everything the runtime's argv builder needs. It holds no
// secret values.
type ContainerArgv struct {
	Name       string
	TTY        bool
	CPUs       int
	Memory     string
	Mounts     []GuestMount
	Workdir    string
	Env        []string
	Entrypoint string
	Image      string
	Args       []string
}

// hostUser returns the invoking user.
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

// orphanStopGrace is the stop grace, in seconds, for a container whose
// paddock side died without waiting on it.
const orphanStopGrace = 2

// isTTY reports whether f is a terminal; /dev/null is a character device but
// not one.
func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(st, null)
}

// ContainerRun runs a container-engine plan to completion: staging, image
// chain, watchdog, supervision, and exit-code propagation. The runtime's
// shape comes from ops; the sequence is shared.
func ContainerRun(ctx context.Context, p *plan.Plan, verify bool, ops ContainerOps) (int, error) {
	if p.Engine != "" && p.Engine != ops.EngineName {
		return 1, fmt.Errorf("plan engine %q is not %s", p.Engine, ops.EngineName)
	}
	if p.Workdir == "" {
		return 1, errors.New("plan has no workdir")
	}
	secrets, err := ResolveSecrets(p.Env, ops.Lookup)
	if err != nil {
		return 1, err
	}
	u, err := hostUser()
	if err != nil {
		return 1, fmt.Errorf("host user: %w", err)
	}
	// The guest HOME is the host's $HOME, from which avenor derives the
	// Claude transcript path.
	u.Home = ops.Home

	image, binDir, entry := ops.TestImage, "", ops.TestEntrypoint
	if image == "" {
		if image, binDir, entry, err = ops.EnsureImages(ctx, p); err != nil {
			return 1, err
		}
	}

	name, err := NewRunName()
	if err != nil {
		return 1, err
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	dirs, err := CreateRunDirs(ops.StateRoot, name)
	if err != nil {
		return 1, err
	}
	var wd Finisher
	defer func() {
		dirs.Remove()
		if wd != nil {
			wd.Finish()
		}
	}()
	if wd, err = ops.Watchdog(name, dirs); err != nil {
		return 1, err
	}

	tdir := TranscriptDir(ops.Home, p.Workdir)
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		return 1, err
	}
	var pass []string
	for _, k := range PassthroughEnv {
		if v := ops.Getenv(k); v != "" {
			pass = append(pass, k+"="+v)
		}
	}
	layout := MountLayout{Dirs: dirs, BinDir: binDir, TranscriptDir: tdir}
	if len(p.Claude.ManagedSettings) > 0 {
		layout.UserSettings = filepath.Join(dirs.Settings, UserSettingsFile)
		layout.UserSettingsAt = UserSettingsPath(u.Home)
	}
	mounts, err := GuestMounts(p, layout)
	if err != nil {
		return 1, err
	}
	cfg, err := BuildInitConfig(p, InitInput{User: u, Verify: verify, TranscriptDir: tdir, Mounts: mounts, Passthrough: pass})
	if err != nil {
		return 1, err
	}
	if err := initcfg.Write(filepath.Join(dirs.State, filepath.Base(initcfg.ConfigPath)), cfg); err != nil {
		return 1, err
	}
	if err := CopySeedFiles(dirs.State, p.Claude.SeedFiles); err != nil {
		return 1, err
	}
	if layout.UserSettings != "" {
		if err := os.WriteFile(layout.UserSettings, p.Claude.ManagedSettings, 0o644); err != nil {
			return 1, err
		}
	}
	if secrets != nil {
		if err := WriteSecrets(dirs.Secrets, secrets); err != nil {
			return 1, err
		}
	}

	argv := ops.Argv(ContainerArgv{
		Name:       name,
		TTY:        isTTY(ops.Stdin),
		CPUs:       p.Limits.CPUs,
		Memory:     p.Limits.Memory,
		Mounts:     mounts,
		Workdir:    p.Workdir,
		Env:        pass,
		Entrypoint: entry,
		Image:      image,
		Args:       ops.TestArgs,
	})
	ops.Debugf("%s", strings.Join(argv, " "))

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = ops.Stdin, ops.Stdout, ops.Stderr

	if ops.TestStartDelay > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(ops.TestStartDelay):
		}
	}
	if err := ctx.Err(); err != nil {
		return 1, err
	}
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("%s: %w", ops.RunLabel, err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sup := NewSupervisor(SupervisorConfig{
			Send:             ops.SignalContainer,
			Stop:             ops.StopContainer,
			IsRunning:        ops.IsRunning,
			SecretsStartWait: ops.SecretsStartWait,
			SecretsGrace:     ops.SecretsGrace,
			CancelGrace:      ops.CancelGrace,
			StopGrace:        ops.StopGrace,
			Debugf:           ops.Debugf,
			SignalQueued:     ops.SignalQueued,
		})
		sup.ForwardSignals(ctx, name, sigs, done)
	}()
	if secrets != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sup := NewSupervisor(SupervisorConfig{
				IsRunning:        ops.IsRunning,
				SecretsStartWait: ops.SecretsStartWait,
				SecretsGrace:     ops.SecretsGrace,
			})
			sup.RemoveSecretsOnceRunning(name, dirs.Secrets, done)
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
			// The client died without the container's own exit; stop it
			// promptly: nothing is left to wait on the guest.
			ops.StopContainer(name, orphanStopGrace)
		}
	default:
		return 1, waitErr
	}
	return code, nil
}
