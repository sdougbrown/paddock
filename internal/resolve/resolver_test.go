package resolve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/plan"
)

func testResolver(t *testing.T, origin string, originErr error) (*Resolver, string) {
	t.Helper()
	home := t.TempDir()
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := New(config.Root{Dir: filepath.Join("testdata", "root")}, home)
	r.Cwd = func() (string, error) { return ws, nil }
	r.Origin = func(string) (string, error) { return origin, originErr }
	return r, ws
}

func TestSpecSelection(t *testing.T) {
	tests := []struct {
		name       string
		spec       string
		origin     string
		originErr  error
		wantSpec   string
		wantErrSub string
	}{
		{"flag wins over origin", "afresh", "git@github.com:afresh/scanner.git", nil, "afresh", ""},
		{"origin match wins over default", "", "https://github.com/afresh/scanner.git", nil, "afresh/scanner", ""},
		{"origin without spec falls back to default", "", "https://github.com/other/repo.git", nil, "afresh", ""},
		{"no origin falls back to default", "", "", errors.New("no remote"), "afresh", ""},
		{"unparseable origin falls back to default", "", "/tmp/x.git", nil, "afresh", ""},
		{"unknown explicit spec", "ghost", "", nil, "", "ghost"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := testResolver(t, tc.origin, tc.originErr)
			p, err := r.Resolve(Options{Profile: "fixer", Spec: tc.spec})
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("want error containing %q, got %v", tc.wantErrSub, err)
				}
				return
			}
			if err != nil || p.Spec != tc.wantSpec {
				t.Fatalf("got %v, %v; want spec %q", p, err, tc.wantSpec)
			}
		})
	}
}

func TestSpecSelectionSurfacesBrokenOriginSpec(t *testing.T) {
	r, _ := testResolver(t, "https://github.com/o/r.git", nil)
	r.Source = fakeSource{specs: map[string]fakeSpec{"o/r": op("[env\n")}}
	_, err := r.Resolve(Options{Profile: "fixer"})
	if err == nil || !strings.Contains(err.Error(), `spec "o/r"`) || strings.Contains(err.Error(), "no spec selected") {
		t.Fatalf("want the broken spec's error, got %v", err)
	}
}

func TestSpecSelectionNoDefault(t *testing.T) {
	r, _ := testResolver(t, "", errors.New("no remote"))
	_, err := r.Resolve(Options{Profile: "nodefault"})
	if err == nil || !strings.Contains(err.Error(), "default_spec") || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("want error naming what was tried, got %v", err)
	}
}

func TestResolvePlan(t *testing.T) {
	r, ws := testResolver(t, "", errors.New("no remote"))
	realWS, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.Resolve(Options{Profile: "fixer", Spec: "afresh/scanner", Argv: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Workspace != realWS {
		t.Errorf("workspace = %q, want symlink-resolved cwd %q", p.Workspace, realWS)
	}
	if got := strings.Join(p.SpecChain, ","); got != "afresh,afresh/scanner" {
		t.Errorf("chain = %s", got)
	}
	if strings.Join(p.Argv, " ") != "claude" {
		t.Errorf("argv = %q", p.Argv)
	}
	if p.Profile != "fixer" || p.Engine != "apple-container" || p.Limits != (plan.Limits{CPUs: 4, Memory: "6G"}) {
		t.Errorf("profile %q engine %q limits %+v", p.Profile, p.Engine, p.Limits)
	}
	absRoot, err := filepath.Abs(filepath.Join("testdata", "root"))
	if err != nil {
		t.Fatal(err)
	}
	wantDF := filepath.Join(absRoot, "images", "afresh", "Dockerfile")
	wantLayers := []plan.ImageLayer{{Spec: "afresh", Dockerfile: wantDF, Context: filepath.Dir(wantDF)}}
	if len(p.Image.Layers) != 1 || p.Image.Layers[0] != wantLayers[0] {
		t.Errorf("layers = %+v, want %+v", p.Image.Layers, wantLayers)
	}
	wantMounts := []plan.Mount{
		{Host: realWS, Guest: realWS, ReadOnly: false},
		{Host: filepath.Join(realWS, "repo"), Guest: filepath.Join(realWS, "repo"), ReadOnly: true},
		{Host: r.Home + "/.botfiles/skills", Guest: r.Home + "/.botfiles/skills", ReadOnly: true},
		{Host: r.Home + "/.botfiles/agents", Guest: r.Home + "/.botfiles/agents", ReadOnly: true},
		{Host: r.Home + "/Code/_notes/plans", Guest: r.Home + "/Code/_notes/plans", ReadOnly: true},
	}
	if len(p.Mounts) != len(wantMounts) {
		t.Fatalf("mounts = %+v", p.Mounts)
	}
	for i, m := range wantMounts {
		if p.Mounts[i] != m {
			t.Errorf("mount %d = %+v, want %+v", i, p.Mounts[i], m)
		}
	}
	if got := strings.Join(p.Network.Hosts, ","); got != "api.anthropic.com,downloads.claude.ai" {
		t.Errorf("run hosts = %s", got)
	}
	var token *plan.EnvVar
	for i := range p.Env {
		if p.Env[i].Name == "CLAUDE_CODE_OAUTH_TOKEN" {
			token = &p.Env[i]
		}
	}
	if token == nil || token.Source != "file" || token.Ref != r.Home+"/.secrets/claude.token" || token.Value != "" {
		t.Errorf("token env = %+v", token)
	}
	if _, err := json.Marshal(p); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(ws, "yarn.lock"), []byte("lock v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sp, err := r.Resolve(Options{Profile: "fixer", Spec: "afresh/scanner", Phase: "setup"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sp.Argv, " "); got != "yarn install --immutable" {
		t.Errorf("setup argv = %q", got)
	}
	if got := strings.Join(sp.Network.Sets, ","); got != "anthropic,registries" {
		t.Errorf("setup sets = %s", got)
	}
	if got := strings.Join(sp.Network.Hosts, ","); got != "api.anthropic.com,downloads.claude.ai,registry.npmjs.org,registry.yarnpkg.com" {
		t.Errorf("setup hosts = %s", got)
	}
	if got := strings.Join(p.Network.Sets, ","); got != "anthropic" {
		t.Errorf("run sets = %s", got)
	}
}

func TestResolveErrors(t *testing.T) {
	tests := []struct {
		name    string
		opts    Options
		profile string
		wantErr string
	}{
		{"unknown egress set", Options{Spec: "afresh"}, "badset", "unknown egress set \"nope\""},
		{"relative mount", Options{Spec: "afresh"}, "relmount", "must be absolute"},
		{"skills not under ro mount", Options{Spec: "afresh"}, "skills", "not under a read-only mount"},
		{"setup phase without setup command", Options{Spec: "afresh", Phase: "setup"}, "fixer", "no [setup] command"},
		{"setup with a read-only workspace", Options{Spec: "afresh/scanner", Phase: "setup"}, "rowsro", "setup needs a read-write workspace; profile \"rowsro\" workspace.mode is ro"},
		{"setup with caller argv", Options{Spec: "afresh/scanner", Phase: "setup", Argv: []string{"sh"}}, "rows", "do not pass a command"},
		{"workspace is a file", Options{Spec: "afresh", Workspace: "FILE"}, "fixer", "is not a directory"},
		{"bad phase", Options{Spec: "afresh", Phase: "build"}, "fixer", "phase"},
		{"profile env collides with spec env", Options{Spec: "afresh"}, "envclash", "both the profile and the spec chain"},
		{"workspace readonly escapes", Options{Spec: "afresh"}, "wsescape", "inside the workspace"},
		{"missing workspace", Options{Spec: "afresh", Workspace: "/nonexistent/dir"}, "fixer", "workspace"},
		{"unknown profile key", Options{Spec: "afresh"}, "unknownkey", "bogus"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := testResolver(t, "", errors.New("none"))
			tc.opts.Profile = tc.profile
			if tc.opts.Workspace == "FILE" {
				f := filepath.Join(t.TempDir(), "f")
				if err := os.WriteFile(f, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				tc.opts.Workspace = f
			}
			_, err := r.Resolve(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestCacheDir(t *testing.T) {
	ws := t.TempDir()
	write := func(c string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, "yarn.lock"), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a")
	d1, err := CacheDir("/h", "afresh/scanner", ws, "yarn.lock")
	if err != nil {
		t.Fatal(err)
	}
	// sha256("a") = ca978112ca1bbdca...
	if want := "/h/.cache/paddock/afresh/scanner/ca978112ca1bbdca"; d1 != want {
		t.Errorf("dir = %s, want %s", d1, want)
	}
	if d2, _ := CacheDir("/h", "afresh/scanner", ws, "yarn.lock"); d2 != d1 {
		t.Errorf("same key gave %s and %s", d1, d2)
	}
	write("b")
	if d3, _ := CacheDir("/h", "afresh/scanner", ws, "yarn.lock"); d3 == d1 {
		t.Error("changed key kept the same dir")
	}
	if _, err := CacheDir("/h", "afresh/scanner", ws, "missing.lock"); err == nil || !strings.Contains(err.Error(), "cache_key") {
		t.Errorf("missing key file: %v", err)
	}
}

func TestSetupPhasePlan(t *testing.T) {
	r, ws := testResolver(t, "", errors.New("no remote"))
	if _, err := r.Resolve(Options{Profile: "rows", Spec: "afresh/scanner", Phase: "setup"}); err == nil || !strings.Contains(err.Error(), "cache_key") {
		t.Fatalf("missing cache key file: err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "yarn.lock"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(ws)
	setup, err := r.Resolve(Options{Profile: "rows", Spec: "afresh/scanner", Phase: "setup"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.Resolve(Options{Profile: "rows", Spec: "afresh/scanner"})
	if err != nil {
		t.Fatal(err)
	}
	want := plan.Mount{Host: real, Guest: real}
	if setup.Mounts[0] != want || run.Mounts[0] != want {
		t.Errorf("workspace mounts: setup %+v, run %+v, want %+v", setup.Mounts[0], run.Mounts[0], want)
	}
	if sub := setup.Mounts[1]; sub.Guest != filepath.Join(real, "repo") || !sub.ReadOnly {
		t.Errorf("readonly subpath mount = %+v", sub)
	}
	wantHost := filepath.Join(r.Home, ".cache", "paddock", "afresh", "scanner", "ca978112ca1bbdca")
	wantCache := plan.Mount{Host: wantHost, Guest: "/var/cache/paddock/setup"}
	if !slices.Contains(setup.Mounts, wantCache) || setup.Setup.CacheHost != wantHost {
		t.Errorf("mounts = %+v, cache host = %s; want cache mount %+v", setup.Mounts, setup.Setup.CacheHost, wantCache)
	}
	for _, m := range run.Mounts {
		if m.Guest == "/var/cache/paddock/setup" {
			t.Error("run phase mounts the setup cache")
		}
	}
}

func TestWorkdir(t *testing.T) {
	r, ws := testResolver(t, "", errors.New("no remote"))
	real, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(real, "review")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(sub, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, cwd string
		want      string
	}{
		{"equal", real, real},
		{"below", sub, sub},
		{"outside", outside, real},
		{"symlinked cwd resolves", link, sub},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r.Cwd = func() (string, error) { return tc.cwd, nil }
			p, err := r.Resolve(Options{Profile: "fixer", Spec: "afresh", Workspace: real})
			if err != nil {
				t.Fatal(err)
			}
			if p.Workspace != real || p.Workdir != tc.want {
				t.Errorf("workspace %q workdir %q, want workdir %q", p.Workspace, p.Workdir, tc.want)
			}
		})
	}
	r.Cwd = func() (string, error) { return "", errors.New("gone") }
	p, err := r.Resolve(Options{Profile: "fixer", Spec: "afresh", Workspace: real})
	if err != nil || p.Workdir != real {
		t.Errorf("unreadable cwd: workdir %q, err %v", p.Workdir, err)
	}
}

func TestSetupCacheKeyFromWorkdir(t *testing.T) {
	r, ws := testResolver(t, "", errors.New("no remote"))
	real, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(real, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := []byte("nested lock")
	if err := os.WriteFile(filepath.Join(sub, "yarn.lock"), lock, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := Options{Profile: "rows", Spec: "afresh/scanner", Workspace: real, Phase: "setup"}

	r.Cwd = func() (string, error) { return sub, nil }
	p, err := r.Resolve(opts)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(lock)
	wantHost := filepath.Join(r.Home, ".cache", "paddock", "afresh", "scanner", hex.EncodeToString(sum[:])[:16])
	if p.Setup.CacheHost != wantHost {
		t.Errorf("cache host = %s, want %s", p.Setup.CacheHost, wantHost)
	}

	r.Cwd = func() (string, error) { return real, nil }
	if _, err := r.Resolve(opts); err == nil || !strings.Contains(err.Error(), "setup.cache_key") {
		t.Errorf("cwd at workspace root: err = %v, want setup.cache_key error", err)
	}
}

func TestSetupPhaseDropsCredentials(t *testing.T) {
	r, ws := testResolver(t, "", errors.New("no remote"))
	if err := os.WriteFile(filepath.Join(ws, "yarn.lock"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	names := func(p *plan.Plan) string {
		var n []string
		for _, e := range p.Env {
			n = append(n, e.Name)
		}
		return strings.Join(n, ",")
	}
	run, err := r.Resolve(Options{Profile: "fixer", Spec: "afresh/scanner"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(run); !strings.Contains(got, "CLAUDE_CODE_OAUTH_TOKEN") || !strings.Contains(got, "HOST_NAME") {
		t.Fatalf("run env = %s", got)
	}
	setup, err := r.Resolve(Options{Profile: "fixer", Spec: "afresh/scanner", Phase: "setup"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range setup.Env {
		if e.Source != plan.SourceLiteral {
			t.Errorf("setup env keeps credential %+v", e)
		}
	}
	if !strings.Contains(names(setup), "MODE") {
		t.Errorf("setup env lost literals: %s", names(setup))
	}
	if !slices.Contains(setup.Env, plan.EnvVar{Name: "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", Source: plan.SourceLiteral, Value: "1"}) {
		t.Errorf("setup env lost the spec env: %+v", setup.Env)
	}
}

func TestWorkspaceReadonlySymlinks(t *testing.T) {
	r, ws := testResolver(t, "", errors.New("no remote"))
	real, _ := filepath.EvalSymlinks(ws)
	repoDir := filepath.Join(real, "repo")
	resolve := func() (*plan.Plan, error) {
		return r.Resolve(Options{Profile: "fixer", Spec: "afresh"})
	}

	if err := os.Remove(repoDir); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(); err == nil || !strings.Contains(err.Error(), "workspace.readonly") {
		t.Errorf("missing subpath: err = %v", err)
	}

	if err := os.WriteFile(repoDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(); err == nil || !strings.Contains(err.Error(), "workspace.readonly") {
		t.Errorf("file subpath: err = %v", err)
	}
	os.Remove(repoDir)

	if err := os.Symlink(t.TempDir(), repoDir); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(); err == nil || !strings.Contains(err.Error(), "workspace.readonly") || !strings.Contains(err.Error(), "outside the workspace") {
		t.Errorf("escaping symlink: err = %v", err)
	}
	os.Remove(repoDir)

	target := filepath.Join(real, "elsewhere")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, repoDir); err != nil {
		t.Fatal(err)
	}
	p, err := resolve()
	if err != nil {
		t.Fatalf("inside symlink: %v", err)
	}
	if m := p.Mounts[1]; m != (plan.Mount{Host: target, Guest: target, ReadOnly: true}) {
		t.Errorf("mount = %+v, want the resolved path", m)
	}
}

// useProfile points r at a config root holding one profile, "p", with the
// given TOML body.
func useProfile(t *testing.T, r *Resolver, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profiles", "p.toml"), []byte("engine = \"apple-container\"\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Root = config.Root{Dir: dir}
}

func TestMountPathRules(t *testing.T) {
	real := func(ws string) string {
		r, err := filepath.EvalSymlinks(ws)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	tests := []struct {
		name    string
		path    func(ws string) string
		spec    bool // path comes from the spec's context.readonly
		wantErr string
	}{
		{"profile equals workspace", func(ws string) string { return real(ws) }, false, "mounts.readonly"},
		{"profile contains workspace", func(ws string) string { return filepath.Dir(real(ws)) }, false, "mounts.readonly"},
		{"profile root", func(string) string { return "/" }, false, "mounts.readonly"},
		{"spec equals workspace", func(ws string) string { return real(ws) }, true, "context.readonly"},
		{"spec contains workspace", func(ws string) string { return filepath.Dir(real(ws)) }, true, "context.readonly"},
		{"profile colon", func(string) string { return "/opt/a:b" }, false, "mounts.readonly"},
		{"spec colon", func(string) string { return "/opt/a:b" }, true, "context.readonly"},
		{"profile paddock state", func(string) string { return "/run/paddock" }, false, "mounts.readonly"},
		{"profile below paddock secrets", func(string) string { return "/run/paddock-secrets/x" }, false, "mounts.readonly"},
		{"spec paddock log", func(string) string { return "/run/paddock-log" }, true, "context.readonly"},
		{"spec below paddock bin", func(string) string { return "/opt/paddock/bin" }, true, "context.readonly"},
		{"profile sibling of paddock bin", func(string) string { return "/opt/paddock2" }, false, ""},
		{"profile below workspace", func(ws string) string { return filepath.Join(real(ws), "repo") }, false, ""},
		{"spec below workspace", func(ws string) string { return filepath.Join(real(ws), "repo") }, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := fakeSource{specs: map[string]fakeSpec{"s": op("")}}
			r, ws := testResolver(t, "", errors.New("no remote"))
			r.Source = src
			path := tc.path(ws)
			if tc.spec {
				src.specs["s"] = op("[context]\nreadonly = [" + strconv.Quote(path) + "]\n")
				useProfile(t, r, "")
			} else {
				useProfile(t, r, "[mounts]\nreadonly = ["+strconv.Quote(path)+"]\n")
			}
			_, err := r.Resolve(Options{Profile: "p", Spec: "s", Workspace: ws})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestAllowlistPhaseNeedsEgressSets(t *testing.T) {
	tests := []struct {
		name, body string
		phase      string
		wantErr    string
	}{
		{"run empty", "[network]\npolicy = \"allowlist\"\nsetup = [\"anthropic\"]\n", "", "network.run"},
		{"setup empty", "[network]\npolicy = \"allowlist\"\nrun = [\"anthropic\"]\n", plan.PhaseSetup, "network.setup"},
		{"setup empty is fine in run phase", "[network]\npolicy = \"allowlist\"\nrun = [\"anthropic\"]\n", "", ""},
		{"sets with no hosts", "[network]\npolicy = \"allowlist\"\nrun = [\"empty\"]\n", "", "define no hosts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, ws := testResolver(t, "", errors.New("no remote"))
			useProfile(t, r, "[workspace]\nmode = \"rw\"\n"+tc.body)
			if err := os.WriteFile(filepath.Join(ws, "yarn.lock"), []byte("a"), 0o644); err != nil {
				t.Fatal(err)
			}
			r.Source = fakeSource{specs: map[string]fakeSpec{
				"s": op("[egress]\nanthropic = [\"api.anthropic.com\"]\nempty = []\n"),
			}}
			_, err := r.Resolve(Options{Profile: "p", Spec: "s", Phase: tc.phase})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestSetupPhaseHasNoClaudeState(t *testing.T) {
	r, ws := testResolver(t, "", errors.New("no remote"))
	if err := os.WriteFile(filepath.Join(ws, "yarn.lock"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := r.Resolve(Options{Profile: "fixer", Spec: "afresh/scanner"})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Claude.SeedFiles) == 0 || run.Claude.ManagedSettings == nil || run.Claude.Skills == "" || run.Claude.Agents == "" {
		t.Fatalf("run claude plan unexpectedly empty: %+v", run.Claude)
	}
	setup, err := r.Resolve(Options{Profile: "fixer", Spec: "afresh/scanner", Phase: plan.PhaseSetup})
	if err != nil {
		t.Fatal(err)
	}
	c := setup.Claude
	if len(c.SeedFiles) != 0 || c.SeedFiles == nil || c.ManagedSettings != nil || c.Skills != "" || c.Agents != "" {
		t.Errorf("setup claude plan = %+v, want empty", c)
	}
}

func TestOriginErrorsHideCredentials(t *testing.T) {
	tests := []struct {
		origin  string
		want    string
		notWant string
	}{
		{"https://user:tok@github.com/o/r/extra", "https://github.com/o/r/extra", "tok"},
		{"https://user:tok@github.com/o/r", "no spec \"o/r\" for origin \"https://github.com/o/r\"", "tok"},
		{"tok@github.com:a/b/c", "(scp-style remote)", "tok"},
		{"https://user:tok@[bad", "(unparseable URL)", "tok"},
	}
	for _, tc := range tests {
		t.Run(tc.origin, func(t *testing.T) {
			r, _ := testResolver(t, tc.origin, nil)
			_, err := r.Resolve(Options{Profile: "nodefault"})
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.notWant) {
				t.Fatalf("want error containing %q and not %q, got %v", tc.want, tc.notWant, err)
			}
		})
	}
}

func TestImageDockerfileEscapingRootIsRejected(t *testing.T) {
	r, _ := testResolver(t, "", errors.New("no remote"))
	r.Source = RootSource{Root: r.Root}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects", "esc.toml"), []byte("[image]\ndockerfile = \"../../Dockerfile\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Source = RootSource{Root: config.Root{Dir: dir}}
	_, err := r.Resolve(Options{Profile: "fixer", Spec: "esc"})
	if err == nil || !strings.Contains(err.Error(), "image.dockerfile") || !strings.Contains(err.Error(), `"esc"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestSetupPlanValidation(t *testing.T) {
	tests := []struct {
		name string
		s    Setup
		want string
	}{
		{"cache_dir inside root", Setup{CacheDir: "/var/cache/paddock/setup/yarn"}, ""},
		{"cache_dir shadowing /etc/hosts", Setup{CacheDir: "/etc/hosts"}, "setup.cache_dir"},
		{"cache_dir is the root itself", Setup{CacheDir: "/var/cache/paddock"}, "setup.cache_dir"},
		{"cache_dir is the root with slash", Setup{CacheDir: "/var/cache/paddock/"}, "setup.cache_dir"},
		{"cache_dir escapes with ..", Setup{CacheDir: "/var/cache/paddock/../../../etc"}, "setup.cache_dir"},
		{"cache_dir sibling prefix", Setup{CacheDir: "/var/cache/paddock-evil/x"}, "setup.cache_dir"},
		{"cache_dir relative", Setup{CacheDir: "var/cache/paddock/x"}, "setup.cache_dir"},
		{"cache_key escapes", Setup{CacheKey: "../../lock"}, "setup.cache_key"},
		{"cache_key absolute", Setup{CacheKey: "/etc/passwd"}, "setup.cache_key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := setupPlan("a/b", tc.s)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `"a/b"`) {
				t.Fatalf("err = %v, want one naming spec and %s", err, tc.want)
			}
		})
	}
}

func TestClaudeDirsHaveNoLinksBelowTheirMount(t *testing.T) {
	r, ws := testResolver(t, "", errors.New("no remote"))
	real, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	ro := t.TempDir()
	for _, d := range []string{filepath.Join(ro, "skills"), filepath.Join(real, "writable")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(t.TempDir(), "alias")
	links := map[string]string{
		filepath.Join(ro, "agents"):           filepath.Join(real, "writable"),
		filepath.Join(ro, "dangling"):         filepath.Join(real, "later"),
		filepath.Join(real, "repo", "skills"): filepath.Join(ro, "skills"),
		alias:                                 ro,
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, key, mount, path, wantErr string
	}{
		{"real directory in a read-only mount", "skills", ro, filepath.Join(ro, "skills"), ""},
		{"missing directory in a read-only mount", "skills", ro, filepath.Join(ro, "none", "skills"), ""},
		{"read-only mount reached through a link", "skills", alias, filepath.Join(alias, "skills"), ""},
		{"link into the rw workspace", "skills", ro, filepath.Join(ro, "agents"), "not under a read-only mount"},
		{"path below a link into the rw workspace", "skills", ro, filepath.Join(ro, "agents", "x"), "not under a read-only mount"},
		{"dangling link into the rw workspace", "skills", ro, filepath.Join(ro, "dangling"), "not under a read-only mount"},
		{"link in a read-only subpath", "skills", ro, filepath.Join(real, "repo", "skills"), "not under a read-only mount"},
		{"rw workspace", "skills", ro, filepath.Join(real, "writable"), "not under a read-only mount"},
		{"agents real directory in a read-only mount", "agents", ro, filepath.Join(ro, "skills"), ""},
		{"agents link into the rw workspace", "agents", ro, filepath.Join(ro, "agents"), "claude.agents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useProfile(t, r, fmt.Sprintf("[workspace]\nmode = \"rw\"\nreadonly = [\"repo\"]\n[mounts]\nreadonly = [%q]\n[claude]\n%s = %q\n", tc.mount, tc.key, tc.path))
			_, err := r.Resolve(Options{Profile: "p", Spec: "afresh"})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestWorkspaceModeDefaultsToReadonly(t *testing.T) {
	r, ws := testResolver(t, "", errors.New("no remote"))
	real, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	useProfile(t, r, "[workspace]\nreadonly = [\"repo\"]\n")
	p, err := r.Resolve(Options{Profile: "p", Spec: "afresh"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Mounts[0] != (plan.Mount{Host: real, Guest: real, ReadOnly: true}) {
		t.Errorf("workspace mount = %+v, want read-only", p.Mounts[0])
	}
}

func TestSeedFilesNeedDistinctNames(t *testing.T) {
	r, _ := testResolver(t, "", errors.New("no remote"))
	useProfile(t, r, "[claude]\nseed_files = [\"~/a/remote-settings.json\", \"~/b/remote-settings.json\"]\n")
	if _, err := r.Resolve(Options{Profile: "p", Spec: "afresh"}); err == nil || !strings.Contains(err.Error(), "claude.seed_files") {
		t.Errorf("colliding seed file names: err = %v", err)
	}
	useProfile(t, r, "[claude]\nseed_files = [\"~/a/remote-settings.json\", \"~/b/other.json\"]\n")
	if _, err := r.Resolve(Options{Profile: "p", Spec: "afresh"}); err != nil {
		t.Errorf("distinct seed file names: %v", err)
	}
}
