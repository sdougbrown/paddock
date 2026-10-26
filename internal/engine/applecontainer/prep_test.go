package applecontainer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/images"
	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
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

func testMounts() []guestMount {
	ms, err := guestMounts(&plan.Plan{Mounts: []plan.Mount{
		{Host: "/w", Guest: "/w"},
		{Host: "/w/repo", Guest: "/w/repo", ReadOnly: true},
		{Host: "/skills", Guest: "/skills", ReadOnly: true},
	}}, mountLayout{
		Dirs:          runDirs{State: "/run/s", Secrets: "/run/x", Log: "/run/l"},
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
	s.Mounts, err = guestMounts(&plan.Plan{}, mountLayout{
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
	data, err := resolveSecrets(p.Env, lookup)
	if err != nil || string(data) != "TOKEN="+secret+"\n" {
		t.Fatalf("resolveSecrets = %q, %v", data, err)
	}
	cfg, err := buildInitConfig(p, initInput{User: testUser, TranscriptDir: "/t", Mounts: testMounts()})
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

func TestResolveSecretsRejectsLineBreaks(t *testing.T) {
	env := []plan.EnvVar{{Name: "A", Source: plan.SourceEnv, Ref: "X"}}
	_, err := resolveSecrets(env, func(string) (string, bool) { return "a\nB=c", true })
	if err == nil || strings.Contains(err.Error(), "a\nB") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveSecretsFailsOnUnsetSource(t *testing.T) {
	env := []plan.EnvVar{{Name: "A", Source: plan.SourceEnv, Ref: "X"}}
	if b, err := resolveSecrets(env, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatalf("resolveSecrets with an unset variable = %q, nil", b)
	}
}

func TestTranscriptDirEncoding(t *testing.T) {
	// Expected names come from running the project-dir function of Claude
	// Code 2.1.303 (s0 in its bundle) in node on the same inputs.
	emoji := "\U0001F600"
	cases := map[string]string{
		"/private/tmp/claude-501/-Users-douglasbrown--botfiles/x": "-private-tmp-claude-501--Users-douglasbrown--botfiles-x",
		"/Users/me/Code/my.repo_v2":                               "-Users-me-Code-my-repo-v2",
		"/Users/u/Code/my_repo.v2":                                "-Users-u-Code-my-repo-v2",
		"/w/caf\u00e9 " + emoji + "x":                             "-w-caf----x",
		"/w/" + strings.Repeat("b", 197):                          "-w-" + strings.Repeat("b", 197),
		"/w/" + strings.Repeat("b", 198):                          "-w-" + strings.Repeat("b", 197) + "-jtl6k9",
		"/w/" + strings.Repeat("a", 250):                          "-w-" + strings.Repeat("a", 197) + "-9arrx5",
		"/w/" + strings.Repeat(emoji, 110):                        "-w-" + strings.Repeat("-", 197) + "-16fk1b",
		"/w/" + strings.Repeat("c", 300):                          "-w-" + strings.Repeat("c", 197) + "-klqcw7",
	}
	for ws, enc := range cases {
		got := transcriptDir("/home/u", ws)
		if want := filepath.Join("/home/u/.claude/projects", enc); got != want {
			t.Errorf("transcriptDir(%q) = %q, want %q", ws, got, want)
		}
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

func TestImageHash(t *testing.T) {
	ctx := writeTree(t, map[string]string{"a.txt": "a", "sub/b.txt": "b"})
	base, err := imageHash([]byte("FROM x"), ctx, "parent:1")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := imageHash([]byte("FROM x"), ctx, "parent:1")
	if base != again {
		t.Fatal("hash is not stable")
	}
	mut := func(name string, h string, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if h == base {
			t.Errorf("hash did not change with %s", name)
		}
	}
	h, err := imageHash([]byte("FROM y"), ctx, "parent:1")
	mut("dockerfile", h, err)
	h, err = imageHash([]byte("FROM x"), ctx, "parent:2")
	mut("parent", h, err)
	if err := os.WriteFile(filepath.Join(ctx, "sub", "b.txt"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err = imageHash([]byte("FROM x"), ctx, "parent:1")
	mut("context content", h, err)
	if err := os.Chmod(filepath.Join(ctx, "a.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	h2, err := imageHash([]byte("FROM x"), ctx, "parent:1")
	if err != nil || h2 == h {
		t.Errorf("hash did not change with file mode (err=%v)", err)
	}
}

func TestLayerTag(t *testing.T) {
	re := regexp.MustCompile(`^paddock/afresh-scanner-[0-9a-f]{8}:0123456789ab$`)
	if got := layerTag("Afresh/Scanner", "0123456789abcdef"); !re.MatchString(got) {
		t.Fatalf("layerTag = %q", got)
	}
	repos := map[string]string{}
	for _, spec := range []string{"x/y", "x-y", "X/Y", "base", "x"} {
		repo, _, _ := strings.Cut(layerTag(spec, "0123456789abcdef"), ":")
		if prev, dup := repos[repo]; dup || repo == "paddock/base" {
			t.Errorf("specs %q and %q share the repository %s", prev, spec, repo)
		}
		repos[repo] = spec
	}
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

func TestImageHashFollowsSymlinkedContext(t *testing.T) {
	real := writeTree(t, map[string]string{"a.txt": "a"})
	link := filepath.Join(t.TempDir(), "ctx")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	before, err := imageHash([]byte("FROM x"), link, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(link, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := imageHash([]byte("FROM x"), link, "p")
	if err != nil || after == before {
		t.Fatalf("hash unchanged after editing through a symlinked context (err=%v)", err)
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

func TestBuildInitConfig(t *testing.T) {
	p := &plan.Plan{
		Workspace: "/w",
		Workdir:   "/w",
		Argv:      []string{"claude", "-p"},
		Mounts:    []plan.Mount{{Host: "/w", Guest: "/w"}, {Host: "/s", Guest: "/s", ReadOnly: true}},
		Env: []plan.EnvVar{
			{Name: "LIT", Source: plan.SourceLiteral, Value: "1"},
			{Name: "TOK", Source: plan.SourceFile, Ref: "/x"},
		},
		Network: plan.Network{Policy: "allowlist", Hosts: []string{"api.anthropic.com"}},
		Claude: plan.ClaudePlan{
			ManagedSettings: []byte(`{"a":1}`),
			SeedFiles:       []string{"/h/.claude/remote-settings.json"},
			Skills:          "/s",
			Agents:          "/ag",
		},
	}
	mounts, err := guestMounts(p, mountLayout{
		Dirs:          runDirs{State: "/run/s", Secrets: "/run/x", Log: "/run/l"},
		BinDir:        "/cache/bin/h",
		TranscriptDir: "/t",
		UserSettings:  "/run/set/user-settings.json", UserSettingsAt: testUser.Home + "/.claude/settings.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := buildInitConfig(p, initInput{User: testUser, TranscriptDir: "/t", Mounts: mounts, Passthrough: []string{"TERM=xterm"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Secrets != initcfg.SecretsPath {
		t.Errorf("Secrets = %q", cfg.Secrets)
	}
	if want := []string{"LIT=1", "TERM=xterm"}; !slices.Equal(cfg.Env, want) {
		t.Errorf("Env = %v, want %v", cfg.Env, want)
	}
	n := cfg.Network
	if n.Policy != "allowlist" || !slices.Equal(n.Hosts, []string{"api.anthropic.com"}) {
		t.Errorf("policy %q hosts %v", n.Policy, n.Hosts)
	}
	if n.ProxyPort != 15443 || n.DenyLog != "/run/paddock-log/deny.jsonl" || n.ErrorLog != "/run/paddock-log/proxy.log" || cfg.ProxyUID != 970 {
		t.Errorf("network = %+v uid %d", n, cfg.ProxyUID)
	}
	if c := cfg.Claude; string(c.ManagedSettings) != `{"a":1}` || c.Skills != "/s" || c.Agents != "/ag" {
		t.Errorf("managed settings %s, skills %q, agents %q", c.ManagedSettings, c.Skills, c.Agents)
	}
	if want := []string{"remote-settings.json"}; !slices.Equal(cfg.Claude.SeedFiles, want) {
		t.Errorf("SeedFiles = %v", cfg.Claude.SeedFiles)
	}
	if !strings.Contains(string(cfg.Claude.ClaudeJSON), `"lastOnboardingVersion":"`+images.ClaudeCodeVersion+`"`) {
		t.Errorf("ClaudeJSON = %s", cfg.Claude.ClaudeJSON)
	}
	if cfg.Claude.TranscriptDir != "/t" || cfg.Workdir != "/w" {
		t.Errorf("claude = %+v workdir %q", cfg.Claude, cfg.Workdir)
	}
	if len(cfg.Mounts) != 8 || cfg.Mounts[1] != (initcfg.Mount{Path: "/s", ReadOnly: true}) {
		t.Errorf("Mounts = %+v", cfg.Mounts)
	}
	if last := cfg.Mounts[len(cfg.Mounts)-1]; last != (initcfg.Mount{Path: testUser.Home + "/.claude/settings.json", ReadOnly: true}) {
		t.Errorf("last mount = %+v", last)
	}
}

func TestInitMountsMatchRunArgv(t *testing.T) {
	for _, bin := range []string{"", "/cache/bin/h"} {
		p := &plan.Plan{
			Workspace: "/w", Workdir: "/w", Argv: []string{"x"}, Network: plan.Network{Policy: "none"},
			Mounts: []plan.Mount{{Host: "/w", Guest: "/w"}, {Host: "/s", Guest: "/s", ReadOnly: true}},
		}
		mounts, err := guestMounts(p, mountLayout{
			Dirs: runDirs{State: "/run/s", Secrets: "/run/x", Log: "/run/l"}, BinDir: bin, TranscriptDir: "/t",
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := buildInitConfig(p, initInput{User: testUser, TranscriptDir: "/t", Mounts: mounts})
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

func TestBuildInitConfigNoSecretsWhenAllLiteral(t *testing.T) {
	p := &plan.Plan{
		Workspace: "/w", Workdir: "/w", Argv: []string{"x"}, Network: plan.Network{Policy: "none"},
		Env: []plan.EnvVar{{Name: "A", Source: plan.SourceLiteral, Value: "1"}},
	}
	cfg, err := buildInitConfig(p, initInput{User: testUser, TranscriptDir: "/t"})
	if err != nil || cfg.Secrets != "" {
		t.Fatalf("Secrets = %q, err = %v", cfg.Secrets, err)
	}
}

func TestStageInitBinary(t *testing.T) {
	src := filepath.Join(t.TempDir(), "paddock-init")
	if err := os.WriteFile(src, []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	dir, err := stageInitBinary(cache, src)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "paddock-init" {
		t.Fatalf("staged dir holds %v", entries)
	}
	if st, _ := os.Stat(filepath.Join(dir, "paddock-init")); st.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", st.Mode())
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "paddock-init")); string(got) != "binary" {
		t.Errorf("staged content = %q", got)
	}
	again, err := stageInitBinary(cache, src)
	if err != nil || again != dir {
		t.Fatalf("restage = %q, %v", again, err)
	}
	if err := os.WriteFile(src, []byte("rebuilt"), 0o644); err != nil {
		t.Fatal(err)
	}
	next, err := stageInitBinary(cache, src)
	if err != nil || next == dir {
		t.Fatalf("stage after a change = %q, %v", next, err)
	}
	if got, _ := os.ReadFile(filepath.Join(next, "paddock-init")); string(got) != "rebuilt" {
		t.Errorf("restaged content = %q", got)
	}
}

func TestCopySeedFilesRejectsCollisions(t *testing.T) {
	a := writeTree(t, map[string]string{"s.json": "1"})
	b := writeTree(t, map[string]string{"s.json": "2"})
	err := copySeedFiles(t.TempDir(), []string{filepath.Join(a, "s.json"), filepath.Join(b, "s.json")})
	if err == nil {
		t.Fatal("want collision error")
	}
}

func TestCreateRunDirsAreOwnerOnly(t *testing.T) {
	d, err := createRunDirs(t.TempDir(), "paddock-x")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{d.Root, d.State, d.Secrets, d.Log, d.Settings} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", p, st, err)
		}
	}
	d.remove()
	for _, p := range []string{d.State, d.Secrets, d.Settings} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived remove", p)
		}
	}
	if _, err := os.Stat(d.Log); err != nil {
		t.Error("log dir was removed")
	}
}

func TestCreateRunDirsRefusesAnExistingRun(t *testing.T) {
	root := t.TempDir()
	if _, err := createRunDirs(root, "paddock-x"); err != nil {
		t.Fatal(err)
	}
	if _, err := createRunDirs(root, "paddock-x"); err == nil {
		t.Error("a second run with the same name shared its directories")
	}
}

func TestWorkdirDrivesArgvTranscriptAndTrust(t *testing.T) {
	p := &plan.Plan{
		Workspace: "/run/x", Workdir: "/run/x/review", Argv: []string{"claude"},
		Network: plan.Network{Policy: "none"},
	}
	tdir := transcriptDir("/Users/u", p.Workdir)
	if want := "/Users/u/.claude/projects/-run-x-review"; tdir != want {
		t.Errorf("transcript dir = %q, want %q", tdir, want)
	}
	cfg, err := buildInitConfig(p, initInput{User: testUser, TranscriptDir: tdir})
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

func TestBuildInitConfigRequiresWorkdir(t *testing.T) {
	p := &plan.Plan{Workspace: "/w", Argv: []string{"x"}, Network: plan.Network{Policy: "none"}}
	if _, err := buildInitConfig(p, initInput{User: testUser, TranscriptDir: "/t"}); err == nil {
		t.Fatal("want an error for an empty workdir")
	}
}

// TestBuildInitConfigVerifyWithoutArgv builds the init config for
// `paddock run --verify`: no argv, Verify set. The config must carry Verify
// and pass the guest's validation, which rejects an empty argv otherwise.
func TestBuildInitConfigVerifyWithoutArgv(t *testing.T) {
	p := &plan.Plan{Workspace: "/w", Workdir: "/w", Network: plan.Network{Policy: "none"}}
	cfg, err := buildInitConfig(p, initInput{User: testUser, TranscriptDir: "/t", Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Verify || len(cfg.Argv) != 0 {
		t.Errorf("Verify = %v, Argv = %q; want true and empty", cfg.Verify, cfg.Argv)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestBuildInitConfigPlanEnvBeatsPassthrough(t *testing.T) {
	p := &plan.Plan{
		Workspace: "/w", Workdir: "/w", Argv: []string{"x"}, Network: plan.Network{Policy: "none"},
		Env: []plan.EnvVar{{Name: "TERM", Source: plan.SourceLiteral, Value: "dumb"}},
	}
	cfg, err := buildInitConfig(p, initInput{
		User: testUser, TranscriptDir: "/t",
		Passthrough: []string{"TERM=xterm", "COLORTERM=truecolor", "LANG=C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"TERM=dumb", "COLORTERM=truecolor", "LANG=C"}; !slices.Equal(cfg.Env, want) {
		t.Errorf("Env = %v, want %v", cfg.Env, want)
	}
}

func TestSplitEnvKeepsNonLiteralsOutOfInitConfig(t *testing.T) {
	env := []plan.EnvVar{
		{Name: "LIT", Source: plan.SourceLiteral, Value: "1"},
		{Name: "F", Source: plan.SourceFile, Ref: "/x"},
		{Name: "E", Source: plan.SourceEnv, Ref: "HOST_E"},
	}
	lits, refs := splitEnv(env)
	if len(lits) != 1 || lits[0].Name != "LIT" || len(refs) != 2 || refs[0].Name != "F" || refs[1].Name != "E" {
		t.Fatalf("splitEnv = %v, %v", lits, refs)
	}
	p := &plan.Plan{Workspace: "/w", Workdir: "/w", Argv: []string{"x"}, Env: env, Network: plan.Network{Policy: "none"}}
	cfg, err := buildInitConfig(p, initInput{User: testUser, TranscriptDir: "/t"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"LIT=1"}; !slices.Equal(cfg.Env, want) {
		t.Errorf("Env = %v, want %v", cfg.Env, want)
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

func TestImageHashHashesSymlinksByTarget(t *testing.T) {
	ctx := writeTree(t, map[string]string{"keep": "k"})
	outside := writeTree(t, map[string]string{"one": "1", "two": "2"})
	link := filepath.Join(ctx, "link")
	if err := os.Symlink(filepath.Join(outside, "one"), link); err != nil {
		t.Fatal(err)
	}
	hash := func() string {
		t.Helper()
		h, err := imageHash([]byte("FROM x"), ctx, "p")
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := hash()
	if err := os.WriteFile(filepath.Join(outside, "one"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hash() != base {
		t.Error("editing a link target's content changed the hash")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "two"), link); err != nil {
		t.Fatal(err)
	}
	if hash() == base {
		t.Error("retargeting a symlink did not change the hash")
	}
}

func TestCopySeedFiles(t *testing.T) {
	src := writeTree(t, map[string]string{"remote-settings.json": `{"a":1}`})
	state := t.TempDir()
	if err := copySeedFiles(state, []string{filepath.Join(src, "remote-settings.json")}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(state, filepath.Base(initcfg.SeedDir), "remote-settings.json")
	st, err := os.Stat(dst)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("seed file: %v, %v", st, err)
	}
	if got, _ := os.ReadFile(dst); string(got) != `{"a":1}` {
		t.Errorf("content = %q", got)
	}
	if st, err := os.Stat(filepath.Dir(dst)); err != nil || !st.IsDir() {
		t.Errorf("seed dir: %v, %v", st, err)
	}
}

func TestWriteSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := writeSecrets(dir, []byte("A=b\n")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, filepath.Base(initcfg.SecretsPath))
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file: %v, %v", st, err)
	}
	if got, _ := os.ReadFile(p); string(got) != "A=b\n" {
		t.Errorf("content = %q", got)
	}
}

func TestGuestMountsRejectManagedGuestPaths(t *testing.T) {
	for _, guest := range []string{"/run/paddock", "/run/paddock/seed", "/run/paddock-secrets", "/run/paddock-log/x", "/opt/paddock", "/opt/paddock/../paddock/bin"} {
		p := &plan.Plan{Mounts: []plan.Mount{{Host: "/h", Guest: guest}}}
		if _, err := guestMounts(p, mountLayout{}); err == nil || !strings.Contains(err.Error(), "paddock-managed") {
			t.Errorf("guest %q: err = %v", guest, err)
		}
	}
	p := &plan.Plan{Mounts: []plan.Mount{{Host: "/h", Guest: "/opt/paddock2"}, {Host: "/h", Guest: "/run/paddocks"}}}
	if _, err := guestMounts(p, mountLayout{}); err != nil {
		t.Errorf("sibling paths rejected: %v", err)
	}
}

func TestGuestMountsRejectTranscriptAndSettingsPaths(t *testing.T) {
	l := mountLayout{TranscriptDir: "/h/.claude/projects/-w", UserSettings: "/s/user-settings.json", UserSettingsAt: "/h/.claude/settings.json"}
	for _, guest := range []string{"/h/.claude/projects/-w", "/h/.claude/projects/-w/sub", "/h/.claude/settings.json"} {
		p := &plan.Plan{Mounts: []plan.Mount{{Host: "/x", Guest: guest}}}
		if _, err := guestMounts(p, l); err == nil || !strings.Contains(err.Error(), "paddock-managed") {
			t.Errorf("guest %q: err = %v", guest, err)
		}
	}
	p := &plan.Plan{Mounts: []plan.Mount{{Host: "/x", Guest: "/h/.claude/projects/-w2"}, {Host: "/y", Guest: "/h/.claude/settings.json.d"}}}
	if _, err := guestMounts(p, l); err != nil {
		t.Errorf("sibling paths rejected: %v", err)
	}
	if _, err := guestMounts(&plan.Plan{Mounts: []plan.Mount{{Host: "/x", Guest: "/h/.claude/settings.json"}}}, mountLayout{TranscriptDir: "/t"}); err != nil {
		t.Errorf("settings path without a settings mount: %v", err)
	}
}

func TestGuestMountsRejectColons(t *testing.T) {
	for _, m := range []plan.Mount{{Host: "/h:x", Guest: "/g"}, {Host: "/h", Guest: "/g:x"}} {
		p := &plan.Plan{Mounts: []plan.Mount{m}}
		if _, err := guestMounts(p, mountLayout{TranscriptDir: "/t"}); err == nil || !strings.Contains(err.Error(), "':'") {
			t.Errorf("mount %+v: err = %v", m, err)
		}
	}
	if _, err := guestMounts(&plan.Plan{}, mountLayout{TranscriptDir: "/t", BinDir: "/bin:x"}); err == nil {
		t.Error("a managed host path with ':' was accepted")
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

func TestLocateInitBinary(t *testing.T) {
	exe := func() (string, error) { return "/opt/bin/paddock", nil }
	set := func(k string) string {
		if k == "PADDOCK_INIT" {
			return "/custom/paddock-init"
		}
		return ""
	}
	unset := func(string) string { return "" }

	if got, err := locateInitBinary(set, exe); err != nil || got != "/custom/paddock-init" {
		t.Errorf("with $PADDOCK_INIT: %q, %v", got, err)
	}
	if got, err := locateInitBinary(unset, exe); err != nil || got != "/opt/bin/paddock-init" {
		t.Errorf("beside the executable: %q, %v", got, err)
	}
	failing := func() (string, error) { return "", errors.New("no executable") }
	if _, err := locateInitBinary(unset, failing); err == nil {
		t.Error("want the executable lookup error")
	}
}

func TestNewRunName(t *testing.T) {
	re := regexp.MustCompile(`^paddock-[0-9a-f]{16}$`)
	a, err := newRunName()
	if err != nil || !re.MatchString(a) {
		t.Fatalf("newRunName() = %q, %v", a, err)
	}
	if b, _ := newRunName(); b == a {
		t.Errorf("two run names are both %q", a)
	}
}

func TestBuildInitConfigRejectsSeedOverManagedSettings(t *testing.T) {
	p := &plan.Plan{
		Workspace: "/w", Workdir: "/w", Argv: []string{"claude"},
		Network: plan.Network{Policy: initcfg.PolicyNone},
		Claude:  plan.ClaudePlan{SeedFiles: []string{"/h/.claude/settings.json"}},
	}
	if _, err := buildInitConfig(p, initInput{User: testUser}); err != nil {
		t.Fatalf("settings.json seed without managed settings: %v", err)
	}
	p.Claude.ManagedSettings = []byte(`{}`)
	if _, err := buildInitConfig(p, initInput{User: testUser}); err == nil || !strings.Contains(err.Error(), "settings.json") {
		t.Errorf("settings.json seed with managed settings: err = %v", err)
	}
}
