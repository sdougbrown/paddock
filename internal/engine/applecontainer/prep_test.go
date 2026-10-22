package applecontainer

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	return guestMounts(&plan.Plan{Mounts: []plan.Mount{
		{Host: "/w", Guest: "/w"},
		{Host: "/w/repo", Guest: "/w/repo", ReadOnly: true},
		{Host: "/skills", Guest: "/skills", ReadOnly: true},
	}}, mountLayout{
		Dirs:          runDirs{State: "/run/s", Secrets: "/run/x", Log: "/run/l"},
		BinDir:        "/cache/bin/h",
		TranscriptDir: "/home/u/.claude/projects/-w",
	})
}

func TestRunArgv(t *testing.T) {
	want := "run --rm -i --init --name paddock-ab12cd34 --cap-add NET_ADMIN --cap-add NET_RAW -c 4 -m 6G --progress none " +
		"-v /w:/w -v /w/repo:/w/repo:ro -v /skills:/skills:ro " +
		"-v /run/s:/run/paddock:ro -v /run/x:/run/paddock-secrets -v /run/l:/run/paddock-log " +
		"-v /cache/bin/h:/opt/paddock:ro -v /home/u/.claude/projects/-w:/home/u/.claude/projects/-w " +
		"-w /w -e TERM=xterm -e COLORTERM=truecolor --entrypoint /opt/paddock/paddock-init paddock/x:1"
	if got := runArgv(testSpec()); !slices.Equal(got, strings.Fields(want)) {
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
	s.Mounts = guestMounts(&plan.Plan{}, mountLayout{
		UserSettings: "/run/s/user-settings.json", UserSettingsAt: "/home/u/.claude/settings.json",
	})
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
		Workspace: "/w", Argv: []string{"claude"},
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

func TestTranscriptDirEncoding(t *testing.T) {
	cases := map[string]string{
		"/private/tmp/claude-501/-Users-douglasbrown--botfiles/x": "-private-tmp-claude-501--Users-douglasbrown--botfiles-x",
		"/Users/me/Code/my.repo_v2":                               "-Users-me-Code-my-repo-v2",
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
	for _, spec := range []string{"afresh/scanner", "Afresh/Scanner"} {
		if got := layerTag(spec, "0123456789abcdef"); got != "paddock/afresh-scanner:0123456789ab" {
			t.Fatalf("layerTag(%q) = %q", spec, got)
		}
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
// for `image ls`, and appends the -t tag of each `build` to that file.
func fakeContainer(t *testing.T, existing ...string) (bin string, calls func() [][]string) {
	t.Helper()
	dir := t.TempDir()
	log, tags := filepath.Join(dir, "log"), filepath.Join(dir, "tags")
	if err := os.WriteFile(tags, []byte(strings.Join(existing, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\037' \"$@\" >> " + log + "\necho >> " + log + "\n" +
		"case \"$1\" in\n image) cat " + tags + " ;;\n build) while [ $# -gt 0 ]; do [ \"$1\" = -t ] && echo \"$2\" >> " + tags + "; shift; done ;;\nesac\n"
	bin = filepath.Join(dir, "container")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
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
	last, err := e.ensureImages(p)
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
	if !strings.HasPrefix(tags[0], "paddock/base:") || !strings.HasPrefix(tags[1], "paddock/a:") || tags[2] != last {
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
	if again, err := e.ensureImages(p); err != nil || again != last {
		t.Fatalf("second run = %q, %v", again, err)
	}
	if b := buildCalls(calls2()); len(b) != 0 {
		t.Errorf("existing tags rebuilt: %q", b)
	}

	bin3, calls3 := fakeContainer(t, tags[:2]...)
	e = &Engine{Bin: bin3, Stderr: io.Discard}
	if _, err := e.ensureImages(p); err != nil {
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
		},
	}
	mounts := guestMounts(p, mountLayout{
		Dirs:          runDirs{State: "/run/s", Secrets: "/run/x", Log: "/run/l"},
		BinDir:        "/cache/bin/h",
		TranscriptDir: "/t",
		UserSettings:  "/run/set/user-settings.json", UserSettingsAt: testUser.Home + "/.claude/settings.json",
	})
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
	if n.ProxyPort != 15443 || n.DenyLog != "/run/paddock-log/deny.jsonl" || n.ErrorLog != "/run/paddock-log/proxy.log" || cfg.ProxyUID != 970 {
		t.Errorf("network = %+v uid %d", n, cfg.ProxyUID)
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
			Workspace: "/w", Argv: []string{"x"}, Network: plan.Network{Policy: "none"},
			Mounts: []plan.Mount{{Host: "/w", Guest: "/w"}, {Host: "/s", Guest: "/s", ReadOnly: true}},
		}
		mounts := guestMounts(p, mountLayout{
			Dirs: runDirs{State: "/run/s", Secrets: "/run/x", Log: "/run/l"}, BinDir: bin, TranscriptDir: "/t",
		})
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
		Workspace: "/w", Argv: []string{"x"}, Network: plan.Network{Policy: "none"},
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
	again, err := stageInitBinary(cache, src)
	if err != nil || again != dir {
		t.Fatalf("restage = %q, %v", again, err)
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
	for _, p := range []string{d.Root, d.State, d.Secrets, d.Log} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", p, st, err)
		}
	}
	d.remove()
	if _, err := os.Stat(d.Secrets); !os.IsNotExist(err) {
		t.Error("secrets dir survived remove")
	}
	if _, err := os.Stat(d.Log); err != nil {
		t.Error("log dir was removed")
	}
}

func TestWorkdirDrivesArgvTranscriptAndTrust(t *testing.T) {
	p := &plan.Plan{
		Workspace: "/run/x", Workdir: "/run/x/review", Argv: []string{"claude"},
		Network: plan.Network{Policy: "none"},
	}
	tdir := transcriptDir("/Users/u", workdir(p))
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
	s.Workdir = workdir(p)
	a := runArgv(s)
	if i := slices.Index(a, "-w"); i < 0 || a[i+1] != "/run/x/review" {
		t.Errorf("argv lacks -w /run/x/review: %v", a)
	}
}

func TestWorkdirDefaultsToWorkspace(t *testing.T) {
	if got := workdir(&plan.Plan{Workspace: "/w"}); got != "/w" {
		t.Errorf("workdir = %q", got)
	}
}
