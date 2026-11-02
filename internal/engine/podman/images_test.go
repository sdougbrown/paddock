package podman

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/plan"
)

// buildAwarePodman returns a fake podman that tracks built tags in local
// storage: the image ls output reflects every `build -t` it has served.
func buildAwarePodman(t *testing.T) (bin string, calls func() [][]string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	names := filepath.Join(dir, "names")
	if err := os.WriteFile(names, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nd=\"${0%/*}\"\n[ \"$1\" = __warm ] && exit 0\nprintf '%s\\037' \"$@\" >> " + log + "\necho >> " + log + "\n" +
		"case \"$1\" in\n image) if [ \"$2\" = ls ]; then\n" +
		"  printf '[' > \"$d/out\"\n  first=1\n  while read -r n; do\n    [ -n \"$n\" ] || continue\n    [ $first = 1 ] || printf ',' >> \"$d/out\"\n    printf '{\"Names\":[\"%s\"]}' \"$n\" >> \"$d/out\"\n    first=0\n  done < \"$d/names\"\n  printf ']' >> \"$d/out\"\n  cat \"$d/out\"\nfi ;;\n" +
		" build) prev=\"\"; for a in \"$@\"; do if [ \"$prev\" = -t ]; then echo \"localhost/$a\" >> \"$d/names\"; fi; prev=\"$a\"; done ;;\n" +
		"esac\n"
	bin = filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
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

// TestEnsureImagesBuildsChainOnce runs the chain twice against a fake podman
// whose storage reflects its builds: the first pass builds the base and each
// layer in order, the second pass builds nothing, and the returned tag is
// the last layer's.
func TestEnsureImagesBuildsChainOnce(t *testing.T) {
	bin, calls := buildAwarePodman(t)
	e := &Engine{Bin: bin}
	dfs := t.TempDir()
	baseDf := filepath.Join(dfs, "Dockerfile")
	if err := os.WriteFile(baseDf, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	layerA := filepath.Join(dfs, "df-a")
	if err := os.WriteFile(layerA, []byte("FROM paddock-base\nRUN a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	layerB := filepath.Join(dfs, "df-b")
	if err := os.WriteFile(layerB, []byte("FROM paddock-a\nRUN b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &plan.Plan{Image: plan.ImagePlan{Layers: []plan.ImageLayer{
		{Spec: "team/app-a", Dockerfile: layerA},
		{Spec: "team/app-b", Dockerfile: layerB, Context: filepath.Dir(layerB)},
	}}}

	tag, err := e.ensureImages(t.Context(), p)
	if err != nil {
		t.Fatalf("ensureImages: %v", err)
	}
	if !strings.HasPrefix(tag, "paddock/team-app-b-") {
		t.Errorf("last tag = %q; want a team/app layer tag", tag)
	}
	builds := 0
	for _, c := range calls() {
		if c[0] == "build" {
			builds++
		}
	}
	if builds != 3 {
		t.Errorf("build calls = %d; want 3 (base + two layers): %q", builds, calls())
	}
	// The second pass reuses every tag: no new build calls.
	tag2, err := e.ensureImages(t.Context(), p)
	if err != nil || tag2 != tag {
		t.Fatalf("second ensureImages = %q, %v; want %q, nil", tag2, err, tag)
	}
	builds = 0
	for _, c := range calls() {
		if c[0] == "build" {
			builds++
		}
	}
	if builds != 3 {
		t.Errorf("build calls after reuse = %d; want 3 (the log is cumulative)", builds)
	}
}
