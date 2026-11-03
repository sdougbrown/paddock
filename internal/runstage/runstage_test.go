package runstage

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sdougbrown/paddock/images"
	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
)

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

var testUser = initcfg.User{Name: "u", UID: 501, GID: 20, Home: "/Users/u"}

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
		got := TranscriptDir("/home/u", ws)
		if want := filepath.Join("/home/u/.claude/projects", enc); got != want {
			t.Errorf("TranscriptDir(%q) = %q, want %q", ws, got, want)
		}
	}
}

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
	mounts, err := GuestMounts(p, MountLayout{
		Dirs:          RunDirs{State: "/run/s", Secrets: "/run/x", Log: "/run/l"},
		BinDir:        "/cache/bin/h",
		TranscriptDir: "/t",
		UserSettings:  "/run/set/user-settings.json", UserSettingsAt: testUser.Home + "/.claude/settings.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := BuildInitConfig(p, InitInput{User: testUser, TranscriptDir: "/t", Mounts: mounts, Passthrough: []string{"TERM=xterm"}})
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

func TestBuildInitConfigNoSecretsWhenAllLiteral(t *testing.T) {
	p := &plan.Plan{
		Workspace: "/w", Workdir: "/w", Argv: []string{"x"}, Network: plan.Network{Policy: "none"},
		Env: []plan.EnvVar{{Name: "A", Source: plan.SourceLiteral, Value: "1"}},
	}
	cfg, err := BuildInitConfig(p, InitInput{User: testUser, TranscriptDir: "/t"})
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
	dir, err := StageInitBinary(cache, src)
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
	again, err := StageInitBinary(cache, src)
	if err != nil || again != dir {
		t.Fatalf("restage = %q, %v", again, err)
	}
	if err := os.WriteFile(src, []byte("rebuilt"), 0o644); err != nil {
		t.Fatal(err)
	}
	next, err := StageInitBinary(cache, src)
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
	err := CopySeedFiles(RunDirs{State: t.TempDir()}, []string{filepath.Join(a, "s.json"), filepath.Join(b, "s.json")})
	if err == nil {
		t.Fatal("want collision error")
	}
}

func TestCreateRunDirsAreOwnerOnly(t *testing.T) {
	d, err := CreateRunDirs(t.TempDir(), "paddock-x")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{d.Root, d.State, d.Secrets, d.Log, d.Settings} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", p, st, err)
		}
	}
	d.Remove()
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
	if _, err := CreateRunDirs(root, "paddock-x"); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateRunDirs(root, "paddock-x"); err == nil {
		t.Error("a second run with the same name shared its directories")
	}
}

func TestBuildInitConfigRequiresWorkdir(t *testing.T) {
	p := &plan.Plan{Workspace: "/w", Argv: []string{"x"}, Network: plan.Network{Policy: "none"}}
	if _, err := BuildInitConfig(p, InitInput{User: testUser, TranscriptDir: "/t"}); err == nil {
		t.Fatal("want an error for an empty workdir")
	}
}

// TestBuildInitConfigVerifyWithoutArgv builds the init config for
// `paddock run --verify`: no argv, Verify set. The config must carry Verify
// and pass the guest's validation, which rejects an empty argv otherwise.
func TestBuildInitConfigVerifyWithoutArgv(t *testing.T) {
	p := &plan.Plan{Workspace: "/w", Workdir: "/w", Network: plan.Network{Policy: "none"}}
	cfg, err := BuildInitConfig(p, InitInput{User: testUser, TranscriptDir: "/t", Verify: true})
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
	cfg, err := BuildInitConfig(p, InitInput{
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
	lits, refs := SplitEnv(env)
	if len(lits) != 1 || lits[0].Name != "LIT" || len(refs) != 2 || refs[0].Name != "F" || refs[1].Name != "E" {
		t.Fatalf("SplitEnv = %v, %v", lits, refs)
	}
	p := &plan.Plan{Workspace: "/w", Workdir: "/w", Argv: []string{"x"}, Env: env, Network: plan.Network{Policy: "none"}}
	cfg, err := BuildInitConfig(p, InitInput{User: testUser, TranscriptDir: "/t"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"LIT=1"}; !slices.Equal(cfg.Env, want) {
		t.Errorf("Env = %v, want %v", cfg.Env, want)
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

	if got, err := LocateInitBinary(set, exe); err != nil || got != "/custom/paddock-init" {
		t.Errorf("with $PADDOCK_INIT: %q, %v", got, err)
	}
	if got, err := LocateInitBinary(unset, exe); err != nil || got != "/opt/bin/paddock-init" {
		t.Errorf("beside the executable: %q, %v", got, err)
	}
	failing := func() (string, error) { return "", errors.New("no executable") }
	if _, err := LocateInitBinary(unset, failing); err == nil {
		t.Error("want the executable lookup error")
	}
}

func TestNewRunName(t *testing.T) {
	re := regexp.MustCompile(`^paddock-[0-9a-f]{16}$`)
	a, err := NewRunName()
	if err != nil || !re.MatchString(a) {
		t.Fatalf("NewRunName() = %q, %v", a, err)
	}
	if b, _ := NewRunName(); b == a {
		t.Errorf("two run names are both %q", a)
	}
}

func TestBuildInitConfigRejectsSeedOverManagedSettings(t *testing.T) {
	p := &plan.Plan{
		Workspace: "/w", Workdir: "/w", Argv: []string{"claude"},
		Network: plan.Network{Policy: initcfg.PolicyNone},
		Claude:  plan.ClaudePlan{SeedFiles: []string{"/h/.claude/settings.json"}},
	}
	if _, err := BuildInitConfig(p, InitInput{User: testUser}); err != nil {
		t.Fatalf("settings.json seed without managed settings: %v", err)
	}
	p.Claude.ManagedSettings = []byte(`{}`)
	if _, err := BuildInitConfig(p, InitInput{User: testUser}); err == nil || !strings.Contains(err.Error(), "settings.json") {
		t.Errorf("settings.json seed with managed settings: err = %v", err)
	}
}

func TestCopySeedFiles(t *testing.T) {
	src := writeTree(t, map[string]string{"remote-settings.json": `{"a":1}`})
	state := RunDirs{State: t.TempDir()}
	if err := CopySeedFiles(state, []string{filepath.Join(src, "remote-settings.json")}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(state.State, filepath.Base(initcfg.SeedDir), "remote-settings.json")
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
	if err := WriteSecrets(dir, []byte("A=b\n")); err != nil {
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
		if _, err := GuestMounts(p, MountLayout{}); err == nil || !strings.Contains(err.Error(), "paddock-managed") {
			t.Errorf("guest %q: err = %v", guest, err)
		}
	}
	p := &plan.Plan{Mounts: []plan.Mount{{Host: "/h", Guest: "/opt/paddock2"}, {Host: "/h", Guest: "/run/paddocks"}}}
	if _, err := GuestMounts(p, MountLayout{}); err != nil {
		t.Errorf("sibling paths rejected: %v", err)
	}
}

func TestGuestMountsRejectTranscriptAndSettingsPaths(t *testing.T) {
	l := MountLayout{TranscriptDir: "/h/.claude/projects/-w", UserSettings: "/s/user-settings.json", UserSettingsAt: "/h/.claude/settings.json"}
	for _, guest := range []string{"/h/.claude/projects/-w", "/h/.claude/projects/-w/sub", "/h/.claude/settings.json"} {
		p := &plan.Plan{Mounts: []plan.Mount{{Host: "/x", Guest: guest}}}
		if _, err := GuestMounts(p, l); err == nil || !strings.Contains(err.Error(), "paddock-managed") {
			t.Errorf("guest %q: err = %v", guest, err)
		}
	}
	p := &plan.Plan{Mounts: []plan.Mount{{Host: "/x", Guest: "/h/.claude/projects/-w2"}, {Host: "/y", Guest: "/h/.claude/settings.json.d"}}}
	if _, err := GuestMounts(p, l); err != nil {
		t.Errorf("sibling paths rejected: %v", err)
	}
	if _, err := GuestMounts(&plan.Plan{Mounts: []plan.Mount{{Host: "/x", Guest: "/h/.claude/settings.json"}}}, MountLayout{TranscriptDir: "/t"}); err != nil {
		t.Errorf("settings path without a settings mount: %v", err)
	}
}

func TestGuestMountsRejectColons(t *testing.T) {
	for _, m := range []plan.Mount{{Host: "/h:x", Guest: "/g"}, {Host: "/h", Guest: "/g:x"}} {
		p := &plan.Plan{Mounts: []plan.Mount{m}}
		if _, err := GuestMounts(p, MountLayout{TranscriptDir: "/t"}); err == nil || !strings.Contains(err.Error(), "':'") {
			t.Errorf("mount %+v: err = %v", m, err)
		}
	}
	if _, err := GuestMounts(&plan.Plan{}, MountLayout{TranscriptDir: "/t", BinDir: "/bin:x"}); err == nil {
		t.Error("a managed host path with ':' was accepted")
	}
}

func TestResolveSecretsRejectsLineBreaks(t *testing.T) {
	env := []plan.EnvVar{{Name: "A", Source: plan.SourceEnv, Ref: "X"}}
	_, err := ResolveSecrets(env, func(string) (string, bool) { return "a\nB=c", true })
	if err == nil || strings.Contains(err.Error(), "a\nB") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveSecretsFailsOnUnsetSource(t *testing.T) {
	env := []plan.EnvVar{{Name: "A", Source: plan.SourceEnv, Ref: "X"}}
	if b, err := ResolveSecrets(env, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatalf("ResolveSecrets with an unset variable = %q, nil", b)
	}
}

// TestSignalQueueDeliversInOrder queues INT then TERM (twice) while the
// container is booting. They go out as INT then TERM, once each, and a
// failed send leaves the rest queued for the next retry.
func TestSignalQueueDeliversInOrder(t *testing.T) {
	var q SignalQueue
	var sent []os.Signal
	booting := func(os.Signal) bool { return false }
	running := func(s os.Signal) bool { sent = append(sent, s); return true }

	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGTERM} {
		q.Push(s)
		q.Deliver(booting)
	}
	if want := []os.Signal{syscall.SIGINT, syscall.SIGTERM}; !slices.Equal(q, want) {
		t.Fatalf("queue = %v, want %v", q, want)
	}

	first := true
	q.Deliver(func(s os.Signal) bool {
		if !first {
			return false
		}
		first = false
		return running(s)
	})
	if want := []os.Signal{syscall.SIGTERM}; !slices.Equal(q, want) {
		t.Fatalf("after one delivery, queue = %v, want %v", q, want)
	}
	q.Deliver(running)
	if len(q) != 0 {
		t.Errorf("queue = %v after delivery, want empty", q)
	}
	if want := []os.Signal{syscall.SIGINT, syscall.SIGTERM}; !slices.Equal(sent, want) {
		t.Errorf("sent %v, want %v", sent, want)
	}

	q.Push(syscall.SIGHUP)
	q.Deliver(running)
	q.Push(syscall.SIGHUP)
	q.Deliver(running)
	if want := []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGHUP}; !slices.Equal(sent, want) {
		t.Errorf("a repeated signal after delivery was dropped: sent %v, want %v", sent, want)
	}
}

// TestSignalQueueKeepsLaterSignalsBehindAFailure queues INT, TERM, INT. The
// second INT is not a consecutive duplicate, so it stays. When TERM cannot be
// delivered, delivery stops there and keeps TERM and the second INT queued.
func TestSignalQueueKeepsLaterSignalsBehindAFailure(t *testing.T) {
	var q SignalQueue
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGINT} {
		q.Push(s)
	}
	if want := []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGINT}; !slices.Equal(q, want) {
		t.Fatalf("queue = %v, want %v", q, want)
	}
	var tried []os.Signal
	q.Deliver(func(s os.Signal) bool {
		tried = append(tried, s)
		return s != syscall.SIGTERM
	})
	if want := []os.Signal{syscall.SIGINT, syscall.SIGTERM}; !slices.Equal(tried, want) {
		t.Errorf("tried %v, want %v", tried, want)
	}
	if want := []os.Signal{syscall.SIGTERM, syscall.SIGINT}; !slices.Equal(q, want) {
		t.Errorf("queue = %v, want %v", q, want)
	}
}
