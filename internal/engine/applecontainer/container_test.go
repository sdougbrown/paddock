package applecontainer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/plan"
)

// The container tests run alpine with a shell entrypoint instead of
// paddock-init. They re-execute this test binary: with PADDOCK_ENGINE_HELPER
// set it runs Engine.Run as the paddock process under test, and with
// __watchdog as its first argument it is the watchdog.

const fakeSecret = "tok-not-a-real-secret"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == WatchdogCommand {
		os.Exit(RunWatchdog(os.Args[2:]))
	}
	if os.Getenv("PADDOCK_ENGINE_HELPER") == "1" {
		os.Exit(helperMain())
	}
	os.Exit(m.Run())
}

func helperMain() int {
	home := os.Getenv("HELPER_HOME")
	ws := os.Getenv("HELPER_WORKSPACE")
	e := &Engine{
		Home:           home,
		testImage:      "alpine:3.22",
		testEntrypoint: "/bin/sh",
		testArgs:       []string{"-c", os.Getenv("HELPER_SCRIPT")},
		selfExe:        os.Args[0],
		secretsGrace:   time.Second,
	}
	p := &plan.Plan{
		Engine:    "apple-container",
		Workspace: ws,
		Workdir:   ws,
		Mounts:    []plan.Mount{{Host: ws, Guest: ws}},
		Env:       []plan.EnvVar{{Name: "TOKEN", Source: plan.SourceEnv, Ref: "HELPER_TOKEN"}},
		Network:   plan.Network{Policy: "none"},
		Argv:      []string{"unused"},
	}
	code, err := e.Run(context.Background(), p, engine.Options{})
	if err != nil {
		os.Stderr.WriteString("helper: " + err.Error() + "\n")
		return 1
	}
	return code
}

type helper struct {
	cmd  *exec.Cmd
	home string
	out  bytes.Buffer
}

func startHelper(t *testing.T, script string) *helper {
	t.Helper()
	if os.Getenv("PADDOCK_CONTAINER_TESTS") != "1" {
		t.Skip("set PADDOCK_CONTAINER_TESTS=1 to run container tests")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(home, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &helper{home: home}
	h.cmd = exec.Command(os.Args[0])
	h.cmd.Env = append(os.Environ(), "PADDOCK_ENGINE_HELPER=1", "HELPER_HOME="+home,
		"HELPER_WORKSPACE="+ws, "HELPER_SCRIPT="+script, "HELPER_TOKEN="+fakeSecret)
	h.cmd.Stdout, h.cmd.Stderr = &h.out, &h.out
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.cmd.Process.Kill()
		h.cmd.Wait()
		if name := h.name(); name != "" {
			exec.Command("container", "delete", "--force", name).Run()
		}
	})
	return h
}

func (h *helper) wait(t *testing.T) int {
	t.Helper()
	err := h.cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	}
	t.Fatalf("wait: %v", err)
	return -1
}

// name returns the container name from the run directory the helper created.
func (h *helper) name() string {
	entries, _ := os.ReadDir(filepath.Join(h.home, ".local", "state", "paddock", "runs"))
	for _, e := range entries {
		return e.Name()
	}
	return ""
}

func (h *helper) runDir() string {
	return filepath.Join(h.home, ".local", "state", "paddock", "runs", h.name())
}

func eventually(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func listed(name string, all bool) (present, running bool) {
	args := []string{"ls", "--format", "json"}
	if all {
		args = append(args, "-a")
	}
	out, err := exec.Command("container", args...).Output()
	if err != nil {
		return false, false
	}
	var list []struct {
		Configuration struct{ ID string }
		Status        struct{ State string }
	}
	if json.Unmarshal(out, &list) != nil {
		return false, false
	}
	for _, c := range list {
		if c.Configuration.ID == name {
			return true, c.Status.State == "running"
		}
	}
	return false, false
}

func waitRunning(t *testing.T, h *helper) string {
	t.Helper()
	var name string
	eventually(t, 60*time.Second, "container to run", func() bool {
		name = h.name()
		_, running := listed(name, false)
		return name != "" && running
	})
	return name
}

func TestContainerExitCodePropagates(t *testing.T) {
	h := startHelper(t, "exit 7")
	if code := h.wait(t); code != 7 {
		t.Fatalf("exit code %d, want 7; output:\n%s", code, h.out.String())
	}
	if _, err := os.Stat(filepath.Join(h.runDir(), "secrets")); !os.IsNotExist(err) {
		t.Error("secrets dir left behind after exit")
	}
	if _, err := os.Stat(filepath.Join(h.runDir(), "state")); !os.IsNotExist(err) {
		t.Error("state dir left behind after exit")
	}
}

func TestContainerSIGTERMReachesGuest(t *testing.T) {
	h := startHelper(t, "exec sleep 300")
	waitRunning(t, h)
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := h.wait(t); code != 143 {
		t.Fatalf("exit code %d, want 143; output:\n%s", code, h.out.String())
	}
}

func TestContainerSIGTERMBeforeRunningStillExits143(t *testing.T) {
	h := startHelper(t, "exec sleep 300")
	eventually(t, 30*time.Second, "run dir to appear", func() bool { return h.name() != "" })
	if _, running := listed(h.name(), false); running {
		t.Skip("container was already running when the signal was sent")
	}
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := h.wait(t); code != 143 {
		t.Fatalf("exit code %d, want 143; output:\n%s", code, h.out.String())
	}
}

func TestContainerWatchdogStopsAfterSIGKILL(t *testing.T) {
	h := startHelper(t, "exec sleep 300")
	name := waitRunning(t, h)
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	h.cmd.Wait()
	eventually(t, 20*time.Second, "container to disappear", func() bool {
		present, _ := listed(name, true)
		return !present
	})
	eventually(t, 5*time.Second, "state dirs to be removed", func() bool {
		_, e1 := os.Stat(filepath.Join(h.runDir(), "state"))
		_, e2 := os.Stat(filepath.Join(h.runDir(), "secrets"))
		_, e3 := os.Stat(filepath.Join(h.runDir(), "settings"))
		return os.IsNotExist(e1) && os.IsNotExist(e2) && os.IsNotExist(e3)
	})
}

func TestContainerSecretsRemovedWhileRunning(t *testing.T) {
	script := "cat /run/paddock-secrets/secrets.env > /run/paddock-log/seen && touch /run/paddock-log/read; exec sleep 300"
	h := startHelper(t, script)
	name := waitRunning(t, h)
	file := filepath.Join(h.runDir(), "secrets", "secrets.env")
	eventually(t, 10*time.Second, "guest to read secrets.env", func() bool {
		_, err := os.Stat(filepath.Join(h.runDir(), "log", "read"))
		return err == nil
	})
	eventually(t, 10*time.Second, "secrets.env to be removed", func() bool {
		_, err := os.Stat(file)
		return os.IsNotExist(err)
	})
	if _, running := listed(name, false); !running {
		t.Fatal("container stopped before the check")
	}
	seen, err := os.ReadFile(filepath.Join(h.runDir(), "log", "seen"))
	if err != nil || strings.TrimSpace(string(seen)) != "TOKEN="+fakeSecret {
		t.Fatalf("guest did not read secrets.env: %q, %v", seen, err)
	}
	if out, _ := exec.Command("container", "inspect", name).Output(); strings.Contains(string(out), fakeSecret) {
		t.Fatal("secret value visible in container inspect")
	}
}

// TestContainerBuildImages builds the base image and one layer on top of it,
// then checks that a second call reuses both tags. It takes minutes on a
// cold cache, so it also needs PADDOCK_BUILD_TESTS=1.
func TestContainerBuildImages(t *testing.T) {
	if os.Getenv("PADDOCK_CONTAINER_TESTS") != "1" || os.Getenv("PADDOCK_BUILD_TESTS") != "1" {
		t.Skip("set PADDOCK_CONTAINER_TESTS=1 and PADDOCK_BUILD_TESTS=1 to build images")
	}
	dir := writeTree(t, map[string]string{
		"Dockerfile": "ARG PADDOCK_PARENT\nFROM ${PADDOCK_PARENT}\nRUN echo layered > /layer\n",
	})
	p := &plan.Plan{Image: plan.ImagePlan{Layers: []plan.ImageLayer{
		{Spec: "paddock-test/layer", Dockerfile: filepath.Join(dir, "Dockerfile"), Context: dir},
	}}}
	e := &Engine{Stderr: os.Stderr}
	start := time.Now()
	tag, err := e.ensureImages(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ensureImages: %s in %s", tag, time.Since(start).Round(time.Second))
	t.Cleanup(func() { exec.Command("container", "image", "delete", tag).Run() })
	out, err := exec.Command("container", "run", "--rm", tag, "cat", "/layer").Output()
	if err != nil || strings.TrimSpace(string(out)) != "layered" {
		t.Fatalf("layer content = %q, %v", out, err)
	}
	start = time.Now()
	again, err := e.ensureImages(p)
	if err != nil || again != tag || time.Since(start) > 5*time.Second {
		t.Fatalf("second call: %q, %v, %s", again, err, time.Since(start))
	}
}
