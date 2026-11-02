package podman

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/plan"
)

// TestRunPropagatesExitCodeAndArgv runs a plan through the fake podman CLI,
// which logs the run argv and exits 7. The exit code must propagate, and the
// run argv must carry the caps, mounts, workdir, entrypoint, and image.
func TestRunPropagatesExitCodeAndArgv(t *testing.T) {
	bin, calls := fakePodman(t, "paddock/base:fake")
	home := t.TempDir()
	e := &Engine{
		Bin:            bin,
		Home:           home,
		StateRoot:      filepath.Join(home, "state"),
		testImage:      "paddock/base:fake",
		testEntrypoint: "/bin/sh",
		testArgs:       []string{"-c", "exit 7"},
	}
	ws := filepath.Join(home, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUN_TEST_TOKEN", "s3cr3t-value")
	p := &plan.Plan{
		Engine:    "podman",
		Workspace: ws,
		Workdir:   ws,
		Mounts:    []plan.Mount{{Host: ws, Guest: ws}},
		Limits:    plan.Limits{CPUs: 2, Memory: "512M"},
		Network:   plan.Network{Policy: "none"},
		Env:       []plan.EnvVar{{Name: "TOKEN", Source: plan.SourceEnv, Ref: "RUN_TEST_TOKEN"}},
		Argv:      []string{"unused"},
	}
	code, err := e.Run(t.Context(), p, engine.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d; want 7", code)
	}

	var run []string
	for _, c := range calls() {
		if len(c) > 0 && c[0] == "run" {
			run = c
		}
	}
	if run == nil {
		t.Fatalf("no podman run call logged: %q", calls())
	}
	join := strings.Join(run, " ")
	for _, want := range []string{
		"--cap-add NET_ADMIN", "--cap-add NET_RAW", "--cpus 2", "-m 512M",
		"--init", "--name paddock-", "-w " + ws,
		"-v " + ws + ":" + ws,
		"--entrypoint /bin/sh", "paddock/base:fake",
	} {
		if !strings.Contains(join, want) {
			t.Errorf("run argv %q does not contain %q", join, want)
		}
	}

	// The resolved secret value reaches the guest only through secrets.env:
	// it must not appear anywhere in the run argv.
	var runArgv []string
	for _, c := range calls() {
		if len(c) > 0 && c[0] == "run" {
			runArgv = c
		}
	}
	if strings.Contains(strings.Join(runArgv, " "), "s3cr3t-value") {
		t.Errorf("the secret value leaked into the run argv: %q", runArgv)
	}

	// The run's secret-holding directories are removed when the run ends;
	// the root stays, holding the log.
	dirs, err := filepath.Glob(filepath.Join(home, "state", "runs", "paddock-*", "state"))
	if err != nil || len(dirs) != 0 {
		t.Errorf("run state dirs remain: %v, %v", dirs, err)
	}
}

// TestRunKilledBySignalPropagatesAndStops runs a plan whose `podman run`
// client dies by signal: the exit code is 128+signal and the container is
// stopped, since nothing is left to wait on the guest.
func TestRunKilledBySignalPropagatesAndStops(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := "#!/bin/sh\nd=\"${0%/*}\"\n[ \"$1\" = __warm ] && exit 0\n" +
		"case \"$1\" in\n run) printf '%s\\037' \"$@\" >> " + log + "; echo >> " + log + "; kill -TERM $$ ;;\n" +
		" stop) echo stopped >> \"${0%/*}/stops\" ;;\nesac\n"
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(bin, "__warm").Run(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	e := &Engine{
		Bin:            bin,
		Home:           home,
		StateRoot:      filepath.Join(home, "state"),
		testImage:      "paddock/base:fake",
		testEntrypoint: "/bin/sh",
		testArgs:       []string{"-c", "sleep 30"},
	}
	ws := filepath.Join(home, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	p := &plan.Plan{
		Engine:    "podman",
		Workspace: ws,
		Workdir:   ws,
		Mounts:    []plan.Mount{{Host: ws, Guest: ws}},
		Network:   plan.Network{Policy: "none"},
		Argv:      []string{"unused"},
	}
	code, err := e.Run(t.Context(), p, engine.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 143 {
		t.Errorf("exit code = %d; want 143 (128+SIGTERM)", code)
	}
	b, err := os.ReadFile(filepath.Join(dir, "stops"))
	if err != nil || !strings.Contains(string(b), "stopped") {
		t.Errorf("the container was not stopped after the client died by signal: %q, %v", b, err)
	}
}
