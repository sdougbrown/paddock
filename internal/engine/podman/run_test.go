package podman

import (
	"os"
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
	p := &plan.Plan{
		Engine:    "podman",
		Workspace: ws,
		Workdir:   ws,
		Mounts:    []plan.Mount{{Host: ws, Guest: ws}},
		Limits:    plan.Limits{CPUs: 2, Memory: "512M"},
		Network:   plan.Network{Policy: "none"},
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

	// The run's secret-holding directories are removed when the run ends;
	// the root stays, holding the log.
	dirs, err := filepath.Glob(filepath.Join(home, "state", "runs", "paddock-*", "state"))
	if err != nil || len(dirs) != 0 {
		t.Errorf("run state dirs remain: %v, %v", dirs, err)
	}
}
