package applecontainer

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/plan"
)

// TestContainerRealInit runs plans through the real base image with the real
// paddock-init as the entrypoint: the network commit, the privilege drop,
// and the exec of the agent or of the verify role. It builds paddock-init
// for linux and needs the base image, so it also needs PADDOCK_BUILD_TESTS=1;
// the allowlist cases reach api.anthropic.com.
func TestContainerRealInit(t *testing.T) {
	if os.Getenv("PADDOCK_CONTAINER_TESTS") != "1" || os.Getenv("PADDOCK_BUILD_TESTS") != "1" {
		t.Skip("set PADDOCK_CONTAINER_TESTS=1 and PADDOCK_BUILD_TESTS=1 to run paddock-init in a VM")
	}
	initBin := filepath.Join(t.TempDir(), "paddock-init")
	build := exec.Command("go", "build", "-o", initBin, "../../../cmd/paddock")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build paddock-init: %v\n%s", err, out)
	}

	run := func(t *testing.T, policy string, hosts []string, verify bool, argv ...string) (int, string, string) {
		t.Helper()
		home, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ws := filepath.Join(home, "ws")
		if err := os.Mkdir(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		e := &Engine{
			Home:    home,
			Stdout:  &out,
			Stderr:  &out,
			Getenv:  func(k string) string { return map[string]string{"PADDOCK_INIT": initBin}[k] },
			selfExe: os.Args[0],
		}
		p := &plan.Plan{
			Engine:    "apple-container",
			Workspace: ws,
			Workdir:   ws,
			Mounts:    []plan.Mount{{Host: ws, Guest: ws}},
			Network:   plan.Network{Policy: policy, Hosts: hosts},
			Argv:      argv,
		}
		code, err := e.Run(context.Background(), p, engine.Options{Verify: verify})
		if err != nil {
			t.Fatalf("Run: %v\n%s", err, out.String())
		}
		return code, out.String(), ws
	}

	t.Run("verify allowlist", func(t *testing.T) {
		code, out, ws := run(t, "allowlist", []string{"api.anthropic.com"}, true)
		if code != 0 {
			t.Fatalf("exit %d, want 0; output:\n%s", code, out)
		}
		uid, gid := strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid())
		for _, want := range []string{
			"PASS privileges-dropped: uid " + uid + ", gid " + gid + ",",
			"PASS allowed-host-connects:",
			"PASS denied-host-refused:",
			"PASS writable " + ws + ":",
		} {
			if !strings.Contains(out, "\n"+want) && !strings.HasPrefix(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("agent runs dropped in the workdir", func(t *testing.T) {
		code, out, ws := run(t, "none", nil, false, "sh", "-c",
			`id -u; id -G; pwd; grep -E '^(CapEff|CapBnd|NoNewPrivs):' /proc/self/status; touch ran; exit 7`)
		if code != 7 {
			t.Fatalf("exit %d, want 7; output:\n%s", code, out)
		}
		for _, want := range []string{
			strconv.Itoa(os.Getuid()) + "\n" + strconv.Itoa(os.Getgid()) + "\n" + ws + "\n",
			"CapEff:\t0000000000000000\n",
			"CapBnd:\t0000000000000000\n",
			"NoNewPrivs:\t1\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if _, err := os.Stat(filepath.Join(ws, "ran")); err != nil {
			t.Errorf("agent did not run in the workspace: %v", err)
		}
	})

	// The proxy cannot resolve the only allowlisted name, so the lifeline
	// fails, the commit fails, and paddock-init must exit 125 without
	// starting the agent.
	t.Run("failed commit never starts the agent", func(t *testing.T) {
		code, out, ws := run(t, "allowlist", []string{"paddock-lifeline-test.example.com"}, false, "touch", "ran")
		if code != 125 {
			t.Fatalf("exit %d, want 125; output:\n%s", code, out)
		}
		if !strings.Contains(out, "paddock-init: netpolicy: lifeline:") {
			t.Errorf("output lacks the lifeline failure:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(ws, "ran")); !os.IsNotExist(err) {
			t.Errorf("agent ran after a failed commit (stat: %v)", err)
		}
	})
}
