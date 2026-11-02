package applecontainer

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
	"github.com/sdougbrown/paddock/internal/runstage"
)

func testSpec() runSpec {
	return runSpec{
		Name:       "paddock-ab12cd34",
		CPUs:       4,
		Memory:     "6G",
		Mounts:     testMounts(),
		Workdir:    "/w",
		Env:        []string{"TERM=xterm", "COLORTERM=truecolor"},
		Entrypoint: initcfg.InitPath,
		Image:      "paddock/x:1",
	}
}

func testMounts() []runstage.GuestMount {
	ms, err := runstage.GuestMounts(&plan.Plan{Mounts: []plan.Mount{
		{Host: "/w", Guest: "/w"},
		{Host: "/w/repo", Guest: "/w/repo", ReadOnly: true},
		{Host: "/skills", Guest: "/skills", ReadOnly: true},
	}}, runstage.MountLayout{
		Dirs:          runstage.RunDirs{State: "/run/s", Secrets: "/run/x", Log: "/run/l"},
		BinDir:        "/cache/bin/h",
		TranscriptDir: "/home/u/.claude/projects/-w",
	})
	if err != nil {
		panic(err)
	}
	return ms
}

func TestRunArgv(t *testing.T) {
	want := "run --rm -i --init --name paddock-ab12cd34 --cap-add NET_ADMIN --cap-add NET_RAW -c 4 -m 6G --progress none " +
		"-v /w:/w -v /w/repo:/w/repo:ro -v /skills:/skills:ro " +
		"-v /run/s:/run/paddock:ro -v /run/x:/run/paddock-secrets -v /run/l:/run/paddock-log " +
		"-v /cache/bin/h:/opt/paddock:ro -v /home/u/.claude/projects/-w:/home/u/.claude/projects/-w " +
		"-w /w -e TERM=xterm -e COLORTERM=truecolor --entrypoint /opt/paddock/paddock-init paddock/x:1 -- claude -p"
	s := testSpec()
	s.Args = []string{"--", "claude", "-p"}
	if got := runArgv(s); !slices.Equal(got, strings.Fields(want)) {
		t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, strings.Fields(want))
	}
}

func TestRunArgvTTYOnlyWhenRequested(t *testing.T) {
	s := testSpec()
	if slices.Contains(runArgv(s), "-t") {
		t.Fatal("-t present without a TTY")
	}
	s.TTY = true
	if !slices.Contains(runArgv(s), "-t") {
		t.Fatal("-t missing with a TTY")
	}
}

func TestRunArgvMountsUserSettingsReadOnly(t *testing.T) {
	s := testSpec()
	if strings.Contains(strings.Join(runArgv(s), " "), "settings.json") {
		t.Fatal("settings mount present without a settings file")
	}
	var err error
	s.Mounts, err = runstage.GuestMounts(&plan.Plan{}, runstage.MountLayout{
		UserSettings: "/run/s/user-settings.json", UserSettingsAt: "/home/u/.claude/settings.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(runArgv(s), "/run/s/user-settings.json:/home/u/.claude/settings.json:ro") {
		t.Fatalf("argv lacks the read-only file mount: %v", runArgv(s))
	}
}

func TestRunArgvOmitsUnsetLimits(t *testing.T) {
	s := testSpec()
	s.CPUs, s.Memory = 0, ""
	a := runArgv(s)
	if slices.Contains(a, "-c") || slices.Contains(a, "-m") {
		t.Fatalf("limits not omitted: %v", a)
	}
}

func TestSecretsStayOutOfArgvAndInitConfig(t *testing.T) {
	const secret = "s3cr3t-token-value"
	p := &plan.Plan{
		Workspace: "/w", Workdir: "/w", Argv: []string{"claude"},
		Env: []plan.EnvVar{
			{Name: "TOKEN", Source: plan.SourceEnv, Ref: "HOST_TOKEN"},
			{Name: "LIT", Source: plan.SourceLiteral, Value: "1"},
		},
		Network: plan.Network{Policy: "none"},
	}
	lookup := func(k string) (string, bool) { return secret, k == "HOST_TOKEN" }
	data, err := runstage.ResolveSecrets(p.Env, lookup)
	if err != nil || string(data) != "TOKEN="+secret+"\n" {
		t.Fatalf("ResolveSecrets = %q, %v", data, err)
	}
	cfg, err := runstage.BuildInitConfig(p, runstage.InitInput{User: testUser, TranscriptDir: "/t", Mounts: testMounts()})
	if err != nil {
		t.Fatal(err)
	}
	s := testSpec()
	s.Workdir = p.Workspace
	all := strings.Join(runArgv(s), "\x00") + "\x00" + strings.Join(cfg.Env, "\x00")
	if strings.Contains(all, secret) {
		t.Fatal("secret value leaked into argv or init config env")
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBaseBuildContextHoldsOnlyTheDockerfile(t *testing.T) {
	bin, _ := fakeContainer(t)
	if _, err := (&Engine{Bin: bin, Stderr: io.Discard}).ensureImages(t.Context(), &plan.Plan{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "contexts"))
	if err != nil || strings.TrimSpace(string(got)) != "Dockerfile" {
		t.Errorf("base build context = %q, %v; the base tag hashes only the Dockerfile", got, err)
	}
}

func TestPruneKeepsSpecsWithSimilarRefs(t *testing.T) {
	plans := map[string]*plan.Plan{}
	for _, spec := range []string{"x/y", "x-y"} {
		df := writeTree(t, map[string]string{"Dockerfile": "ARG PADDOCK_PARENT\nFROM ${PADDOCK_PARENT}\nLABEL spec=" + spec + "\n"})
		plans[spec] = &plan.Plan{Image: plan.ImagePlan{Layers: []plan.ImageLayer{{Spec: spec, Dockerfile: filepath.Join(df, "Dockerfile")}}}}
	}
	bin, calls := fakeContainer(t)
	e := &Engine{Bin: bin, Stderr: io.Discard}
	for _, spec := range []string{"x/y", "x-y", "x/y"} {
		if _, err := e.ensureImages(t.Context(), plans[spec]); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range calls() {
		if len(c) > 1 && c[0] == "image" && c[1] == "rm" {
			t.Errorf("removed %q", c)
		}
	}
	if b := buildCalls(calls()); len(b) != 3 {
		t.Errorf("builds = %d, want the base and one layer per spec", len(b))
	}
}

// fakeContainer writes a `container` stand-in that logs each invocation's
// arguments (unit-separator joined, one per line), lists the tags in a file
// for `image ls`, and appends the -t tag of each `build` to that file. The
// command exits 1 for any subcommand named by failOn.
func fakeContainer(t *testing.T, existing ...string) (bin string, calls func() [][]string) {
	t.Helper()
	dir := t.TempDir()
	log, tags := filepath.Join(dir, "log"), filepath.Join(dir, "tags")
	if err := os.WriteFile(tags, []byte(strings.Join(existing, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$1\" = __warm ] && exit 0\nprintf '%s\\037' \"$@\" >> " + log + "\necho >> " + log + "\n" +
		"[ -e " + dir + "/fail-$1 ] && exit 1\n" +
		"[ -e " + dir + "/fail-$1-$2 ] && exit 1\n" +
		"[ -e " + dir + "/slow-$1 ] && exec sleep 30\n" +
		"[ -e " + dir + "/slow-once-$1 ] && rm " + dir + "/slow-once-$1 && exec sleep 30\n" +
		"case \"$1\" in\n image) if [ \"$2\" = ls ]; then [ $# -eq 3 ] && [ \"$3\" = --quiet ] || exit 2; cat " + tags + "; fi ;;\n build) for a; do c=$a; done; ls -A \"$c\" | tr '\\n' ' ' >> " + filepath.Join(dir, "contexts") + "; echo >> " + filepath.Join(dir, "contexts") + "\n  while [ $# -gt 0 ]; do [ \"$1\" = -t ] && echo \"$2\" >> " + tags + "; shift; done ;;\nesac\n"
	bin = filepath.Join(dir, "container")
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

func failOn(t *testing.T, bin, sub string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(bin), "fail-"+sub), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildCalls(calls [][]string) (builds [][]string) {
	for _, c := range calls {
		if c[0] == "build" {
			builds = append(builds, c)
		}
	}
	return builds
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestEnsureImagesChainsParentsAndSkipsExisting(t *testing.T) {
	d1 := writeTree(t, map[string]string{"Dockerfile": "ARG PADDOCK_PARENT\nFROM ${PADDOCK_PARENT}\n"})
	d2 := writeTree(t, map[string]string{"Dockerfile": "ARG PADDOCK_PARENT\nFROM ${PADDOCK_PARENT}\nRUN true\n"})
	p := &plan.Plan{Image: plan.ImagePlan{Layers: []plan.ImageLayer{
		{Spec: "a", Dockerfile: filepath.Join(d1, "Dockerfile")},
		{Spec: "a/b", Dockerfile: filepath.Join(d2, "Dockerfile")},
	}}}
	bin, calls := fakeContainer(t)
	e := &Engine{Bin: bin, Stderr: io.Discard}
	last, err := e.ensureImages(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	builds := buildCalls(calls())
	if len(builds) != 3 {
		t.Fatalf("builds = %q, want base and two layers", builds)
	}
	var tags []string
	for _, b := range builds {
		tags = append(tags, flagValue(b, "-t"))
	}
	if !strings.HasPrefix(tags[0], "paddock/base:") || !strings.HasPrefix(tags[1], "paddock/a-") || tags[2] != last {
		t.Fatalf("tags = %v, last = %s", tags, last)
	}
	if flagValue(builds[0], "--build-arg") != "" {
		t.Errorf("base build has a parent: %q", builds[0])
	}
	for i := 1; i < 3; i++ {
		if got, want := flagValue(builds[i], "--build-arg"), "PADDOCK_PARENT="+tags[i-1]; got != want {
			t.Errorf("layer %d build-arg = %q, want %q", i, got, want)
		}
	}

	bin2, calls2 := fakeContainer(t, tags...)
	e = &Engine{Bin: bin2, Stderr: io.Discard}
	if again, err := e.ensureImages(t.Context(), p); err != nil || again != last {
		t.Fatalf("second run = %q, %v", again, err)
	}
	if b := buildCalls(calls2()); len(b) != 0 {
		t.Errorf("existing tags rebuilt: %q", b)
	}

	bin3, calls3 := fakeContainer(t, tags[:2]...)
	e = &Engine{Bin: bin3, Stderr: io.Discard}
	if _, err := e.ensureImages(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if b := buildCalls(calls3()); len(b) != 1 || flagValue(b[0], "-t") != tags[2] || flagValue(b[0], "--build-arg") != "PADDOCK_PARENT="+tags[1] {
		t.Errorf("partial rebuild = %q", b)
	}
}

var testUser = initcfg.User{Name: "u", UID: 501, GID: 20, Home: "/Users/u"}

func TestInitMountsMatchRunArgv(t *testing.T) {
	for _, bin := range []string{"", "/cache/bin/h"} {
		p := &plan.Plan{
			Workspace: "/w", Workdir: "/w", Argv: []string{"x"}, Network: plan.Network{Policy: "none"},
			Mounts: []plan.Mount{{Host: "/w", Guest: "/w"}, {Host: "/s", Guest: "/s", ReadOnly: true}},
		}
		mounts, err := runstage.GuestMounts(p, runstage.MountLayout{
			Dirs: runstage.RunDirs{State: "/run/s", Secrets: "/run/x", Log: "/run/l"}, BinDir: bin, TranscriptDir: "/t",
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := runstage.BuildInitConfig(p, runstage.InitInput{User: testUser, TranscriptDir: "/t", Mounts: mounts})
		if err != nil {
			t.Fatal(err)
		}
		var fromArgv []initcfg.Mount
		a := runArgv(runSpec{Mounts: mounts})
		for i, v := range a {
			if v != "-v" {
				continue
			}
			f := strings.Split(a[i+1], ":")
			fromArgv = append(fromArgv, initcfg.Mount{Path: f[1], ReadOnly: len(f) == 3 && f[2] == "ro"})
		}
		if !slices.Equal(fromArgv, cfg.Mounts) {
			t.Errorf("bin %q: argv mounts %v, init mounts %v", bin, fromArgv, cfg.Mounts)
		}
		if has := slices.Contains(cfg.Mounts, initcfg.Mount{Path: initcfg.BinDir, ReadOnly: true}); has != (bin != "") {
			t.Errorf("bin %q: init config lists BinDir = %v", bin, has)
		}
	}
}

func TestWorkdirDrivesArgvTranscriptAndTrust(t *testing.T) {
	p := &plan.Plan{
		Workspace: "/run/x", Workdir: "/run/x/review", Argv: []string{"claude"},
		Network: plan.Network{Policy: "none"},
	}
	tdir := runstage.TranscriptDir("/Users/u", p.Workdir)
	if want := "/Users/u/.claude/projects/-run-x-review"; tdir != want {
		t.Errorf("transcript dir = %q, want %q", tdir, want)
	}
	cfg, err := runstage.BuildInitConfig(p, runstage.InitInput{User: testUser, TranscriptDir: tdir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workdir != "/run/x/review" {
		t.Errorf("workdir = %q", cfg.Workdir)
	}
	var cj struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(cfg.Claude.ClaudeJSON, &cj); err != nil {
		t.Fatal(err)
	}
	if !cj.Projects["/run/x/review"].Trusted || len(cj.Projects) != 1 {
		t.Errorf("projects = %+v", cj.Projects)
	}
	s := testSpec()
	s.Workdir = p.Workdir
	a := runArgv(s)
	if i := slices.Index(a, "-w"); i < 0 || a[i+1] != "/run/x/review" {
		t.Errorf("argv lacks -w /run/x/review: %v", a)
	}
}

func TestEnsureImagesFailures(t *testing.T) {
	d := writeTree(t, map[string]string{"Dockerfile": "FROM x\n"})
	p := &plan.Plan{Image: plan.ImagePlan{Layers: []plan.ImageLayer{{Spec: "a", Dockerfile: filepath.Join(d, "Dockerfile")}}}}

	bin, calls := fakeContainer(t)
	failOn(t, bin, "build")
	e := &Engine{Bin: bin, Stderr: io.Discard}
	if _, err := e.ensureImages(t.Context(), p); err == nil {
		t.Fatal("want an error when build fails")
	}
	if b := buildCalls(calls()); len(b) != 1 {
		t.Errorf("builds after a failed base build = %q, want only the base attempt", b)
	}

	bin, calls = fakeContainer(t)
	failOn(t, bin, "image")
	e = &Engine{Bin: bin, Stderr: io.Discard}
	if _, err := e.ensureImages(t.Context(), p); err == nil {
		t.Fatal("want an error when image ls fails")
	}
	if b := buildCalls(calls()); len(b) != 0 {
		t.Errorf("builds after a failed image ls = %q", b)
	}
}

func TestEnsureImagesStopsWhenCancelled(t *testing.T) {
	d := writeTree(t, map[string]string{"Dockerfile": "FROM x\n"})
	p := &plan.Plan{Image: plan.ImagePlan{Layers: []plan.ImageLayer{{Spec: "a", Dockerfile: filepath.Join(d, "Dockerfile")}}}}

	bin, calls := fakeContainer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (&Engine{Bin: bin, Stderr: io.Discard}).ensureImages(ctx, p); err == nil {
		t.Fatal("want an error from a cancelled context")
	}
	if c := calls(); len(c) != 0 {
		t.Errorf("container calls after cancellation = %q", c)
	}

	bin, _ = fakeContainer(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(bin), "slow-build"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := (&Engine{Bin: bin, Stderr: io.Discard}).ensureImages(ctx, p); err == nil {
		t.Fatal("want an error when the context ends during a build")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("ensureImages returned %v after the context ended", d)
	}
}

func TestImageExistsMatchesImageLsOutput(t *testing.T) {
	// Output shape of `container image ls -q` from Apple container 1.6.0, plus
	// a docker.io/-prefixed name as printed after a build.
	bin, _ := fakeContainer(t,
		"alpine:3.20",
		"alpine:3.22",
		"node:22.15-bookworm-slim",
		"paddock/afresh-afresh-scanner-app:fb710fee1985",
		"paddock/afresh:bf490c6a3900",
		"paddock/base:2887019daf15",
		"docker.io/paddock/built:0123456789ab",
		"example.com/paddock/elsewhere:0123456789ab",
	)
	e := &Engine{Bin: bin, Stderr: io.Discard}
	for _, tc := range []struct {
		tag  string
		want bool
	}{
		{"paddock/afresh:bf490c6a3900", true},
		{"paddock/built:0123456789ab", true},
		{"paddock/afresh:000000000000", false},
		{"fresh:bf490c6a3900", false},
		{"app:fb710fee1985", false},
		{"paddock/elsewhere:0123456789ab", false},
	} {
		got, err := e.imageExists(t.Context(), tc.tag)
		if err != nil || got != tc.want {
			t.Errorf("imageExists(%q) = %v, %v; want %v", tc.tag, got, err, tc.want)
		}
	}
}

func TestEnsureImagesPrunesSupersededTags(t *testing.T) {
	d := writeTree(t, map[string]string{"Dockerfile": "ARG PADDOCK_PARENT\nFROM ${PADDOCK_PARENT}\n"})
	p := &plan.Plan{Image: plan.ImagePlan{Layers: []plan.ImageLayer{{Spec: "afresh", Dockerfile: filepath.Join(d, "Dockerfile")}}}}

	// Learn the current tags from a first run against an empty store.
	bin, calls := fakeContainer(t)
	if _, err := (&Engine{Bin: bin, Stderr: io.Discard}).ensureImages(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	builds := buildCalls(calls())
	base, layer := flagValue(builds[0], "-t"), flagValue(builds[1], "-t")
	repo, _, _ := strings.Cut(layer, ":")

	bin, calls = fakeContainer(t,
		base, layer,
		"docker.io/"+repo+":oldlayer11111",
		"paddock/base:oldbase000000",
		"docker.io/paddock/base:oldbase111111",
		repo+":oldlayer00000",
		"paddock/afresh:abcdef012345",
		"paddock/other:abcdef012345",
		"alpine:3.22",
		"example.com/paddock/base:notours00000",
	)
	e := &Engine{Bin: bin, Stderr: io.Discard}
	if got, err := e.ensureImages(t.Context(), p); err != nil || got != layer {
		t.Fatalf("ensureImages = %q, %v", got, err)
	}
	var removed []string
	for _, c := range calls() {
		if len(c) == 3 && c[0] == "image" && c[1] == "rm" {
			removed = append(removed, c[2])
		}
	}
	want := []string{
		"docker.io/" + repo + ":oldlayer11111",
		"paddock/base:oldbase000000",
		"docker.io/paddock/base:oldbase111111",
		repo + ":oldlayer00000",
	}
	slices.Sort(removed)
	slices.Sort(want)
	if !slices.Equal(removed, want) {
		t.Errorf("removed = %q, want %q", removed, want)
	}

	// A failed removal does not fail the run.
	bin, calls = fakeContainer(t, base, layer, "paddock/base:oldbase000000")
	failOn(t, bin, "image-rm")
	if got, err := (&Engine{Bin: bin, Stderr: io.Discard}).ensureImages(t.Context(), p); err != nil || got != layer {
		t.Fatalf("ensureImages with failing rm = %q, %v", got, err)
	}
	if !slices.ContainsFunc(calls(), func(c []string) bool {
		return slices.Equal(c, []string{"image", "rm", "paddock/base:oldbase000000"})
	}) {
		t.Errorf("no removal attempted: %q", calls())
	}
}
