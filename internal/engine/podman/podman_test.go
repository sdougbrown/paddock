package podman

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if d, err := time.ParseDuration(os.Getenv("HELPER_WATCHDOG_CLI_TIMEOUT")); err == nil {
		watchdogCLITimeout = d
	}
	if d, err := time.ParseDuration(os.Getenv("HELPER_WATCHDOG_RETRY")); err == nil {
		watchdogRetry = d
	}
	if code, handled := Dispatch(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// fakePodman returns a fake podman CLI that logs its calls, and a function
// that returns the logged calls. The image ls output is a JSON array file
// the test fills with the tags the fake should report.
func fakePodman(t *testing.T, tags ...string) (bin string, calls func() [][]string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	names := make([]string, 0, len(tags))
	for _, tag := range tags {
		names = append(names, "localhost/"+tag)
	}
	blob, _ := json.Marshal([]struct {
		Names []string `json:"Names"`
	}{{names}})
	lsFile := filepath.Join(dir, "image-ls.json")
	if err := os.WriteFile(lsFile, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$1\" = __warm ] && exit 0\nprintf '%s\\037' \"$@\" >> " + log + "\necho >> " + log + "\n" +
		"[ -e " + dir + "/fail-$1 ] && exit 1\n" +
		"[ -e " + dir + "/slow-$1 ] && exec sleep 30\n" +
		"[ -e " + dir + "/slow-once-$1 ] && rm " + dir + "/slow-once-$1 && exec sleep 30\n" +
		"case \"$1\" in\n image) if [ \"$2\" = ls ]; then cat " + lsFile + "; fi ;;\n" +
		" inspect) printf 'true\\n' ;;\n" +
		" run) want=0; nextimg=0; imgdone=0; ep=\"\"\n" +
		"  while [ $# -gt 0 ]; do\n" +
		"    a=\"$1\"\n" +
		"    if [ \"$want\" = 1 ]; then ep=$a; want=0; nextimg=1; shift; continue; fi\n" +
		"    if [ \"$a\" = --entrypoint ]; then want=1; shift; continue; fi\n" +
		"    if [ \"$nextimg\" = 1 ]; then nextimg=0; imgdone=1; shift; continue; fi\n" +
		"    if [ \"$imgdone\" = 1 ]; then break; fi\n" +
		"    shift\n" +
		"  done\n" +
		"  exec \"$ep\" \"$@\" ;;\n" +
		"esac\n"
	bin = filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The first exec of a new script can be slow on a loaded host. One
	// unlogged run keeps that out of the calls tests time or count.
	if err := exec.Command(bin, "__warm").Run(); err != nil {
		t.Fatal(err)
	}
	return bin, func() [][]string {
		b, _ := os.ReadFile(log)
		var out [][]string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				out = append(out, strings.Split(strings.TrimSuffix(l, "\x1f"), "\x1f"))
			}
		}
		return out
	}
}

func TestImageExistsMatchesLocalhostPrefix(t *testing.T) {
	bin, _ := fakePodman(t, "paddock/base:abc123")
	e := &Engine{Bin: bin}
	if have, err := e.imageExists(t.Context(), "paddock/base:abc123"); err != nil || !have {
		t.Errorf("imageExists = %v, %v; want true, nil", have, err)
	}
	if have, err := e.imageExists(t.Context(), "paddock/base:other"); err != nil || have {
		t.Errorf("imageExists = %v, %v; want false, nil", have, err)
	}
}

func TestPruneSupersededKeepsChainAndOtherRepos(t *testing.T) {
	bin, calls := fakePodman(t, "paddock/base:old", "paddock/afresh-scanner-app-deadbeef:old", "other/repo:1")
	e := &Engine{Bin: bin}
	e.pruneSuperseded(t.Context(), []string{"paddock/base:new", "paddock/afresh-scanner-app-deadbeef:new"})
	var removed []string
	for _, c := range calls() {
		if c[0] == "image" && c[1] == "rm" {
			removed = append(removed, c[2])
		}
	}
	if len(removed) != 2 {
		t.Fatalf("removed %q; want the two superseded chain tags", removed)
	}
	for _, r := range removed {
		if !strings.Contains(r, "paddock/") {
			t.Errorf("removed %q; other repositories must never be touched", r)
		}
	}
}

func TestInspectRunning(t *testing.T) {
	bin, _ := fakePodman(t)
	e := &Engine{Bin: bin}
	if !e.isRunning("paddock-abc") {
		t.Error("isRunning = false; want true (fake inspect always says true)")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(bin), "fail-inspect"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if e.isRunning("paddock-abc") {
		t.Error("isRunning = true; want false on inspect failure")
	}
}
