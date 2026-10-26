//go:build linux

package pinit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/initcfg"
)

// dropStage selects what a re-executed test binary does in TestDropAndExec.
const dropStage = "PADDOCK_DROP_STAGE"

// TestDropAndExec runs the real privilege drop: a root child of this test
// binary calls dropAndExec, which execs a copy of the binary as the verify
// role. The verify role must pass its privileges-dropped and writable
// checks and see the workdir as its working directory and 65534 as its
// only group. It needs root.
func TestDropAndExec(t *testing.T) {
	switch os.Getenv(dropStage) {
	case "drop":
		runtime.LockOSThread()
		err := dropAndExec(65534, 65534, os.Getenv("DROP_WORKDIR"), os.Getenv("DROP_EXE"),
			[]string{os.Getenv("DROP_EXE"), "-test.run=^TestDropAndExec$"},
			[]string{dropStage + "=verify", "DROP_PLAN=" + os.Getenv("DROP_PLAN")})
		fmt.Fprintln(os.Stderr, "dropAndExec:", err)
		os.Exit(3)
	case "verify":
		wd, err := os.Getwd()
		fmt.Printf("cwd %s %v\n", wd, err)
		groups, err := os.Getgroups()
		fmt.Printf("groups %v %v\n", groups, err)
		os.Exit(verifyMain([]string{os.Getenv("DROP_PLAN")}))
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	// The exec'd copy runs as 65534, so it and its directories must be
	// readable by others.
	dir := t.TempDir()
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(dir, "pinit.test")
	copyExecutable(t, exe)
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(work, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	plan, err := json.Marshal(verifyPlan{Policy: initcfg.PolicyHost, UID: 65534, Writable: []string{work}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDropAndExec$")
	cmd.Env = append(os.Environ(), dropStage+"=drop", "DROP_WORKDIR="+work, "DROP_EXE="+exe, "DROP_PLAN="+string(plan))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("verify role after the drop: %v\n%s", err, out)
	}
	lines := strings.Split(string(out), "\n")
	for _, want := range []string{
		"cwd " + work + " <nil>",
		"groups [65534] <nil>",
		"PASS privileges-dropped: uid 65534, all capability sets empty, NoNewPrivs 1",
		"PASS writable " + work + ":",
	} {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, want) }) {
			t.Errorf("no output line starts with %q:\n%s", want, out)
		}
	}
}

func copyExecutable(t *testing.T, dst string) {
	t.Helper()
	src, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
