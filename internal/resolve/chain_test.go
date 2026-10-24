package resolve

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/plan"
)

type fakeSpec struct {
	trust Trust
	toml  string
}

type fakeSource struct {
	specs map[string]fakeSpec
	files map[string]string
}

func (f fakeSource) LoadSpec(ref string) (*config.Spec, Trust, error) {
	fs_, ok := f.specs[ref]
	if !ok {
		return nil, 0, fs.ErrNotExist
	}
	var s config.Spec
	if err := toml.Unmarshal([]byte(fs_.toml), &s); err != nil {
		return nil, 0, err
	}
	return &s, fs_.trust, nil
}

func (f fakeSource) Path(rel string) (string, error) { return "/cfg/" + rel, nil }

func (f fakeSource) ReadFile(rel string) ([]byte, error) {
	c, ok := f.files[rel]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(c), nil
}

func op(t string) fakeSpec   { return fakeSpec{TrustOperator, t} }
func repo(t string) fakeSpec { return fakeSpec{TrustRepository, t} }

func TestChainRules(t *testing.T) {
	base := op("[egress]\nanthropic = [\"api.anthropic.com\"]\n[env]\nA = \"1\"\n[setup]\ncommand = [\"make\"]\n")
	cached := op("[setup]\ncommand = [\"make\"]\ncache_key = \"yarn.lock\"\ncache_dir = \"/var/cache/x\"\n")
	tests := []struct {
		name    string
		specs   map[string]fakeSpec
		wantErr string
	}{
		{"repository spec with egress", map[string]fakeSpec{"base": base, "leaf": repo("extends = \"base\"\n[egress]\nx = [\"x.example.com\"]\n")}, "may not define egress"},
		{"repository spec with managed settings", map[string]fakeSpec{"base": base, "leaf": repo("extends = \"base\"\n[claude]\nmanaged_settings = \"m.json\"\n")}, "claude.managed_settings: repository-trust specs may not set managed settings"},
		{"repository spec without parent", map[string]fakeSpec{"leaf": repo("")}, "must extend an operator-trust"},
		{"repository spec extending repository spec", map[string]fakeSpec{"base": base, "mid": repo("extends = \"base\"\n"), "leaf": repo("extends = \"mid\"\n")}, "must extend an operator-trust"},
		{"repository spec extending operator spec", map[string]fakeSpec{"base": base, "leaf": repo("extends = \"base\"\n[context]\nreadonly = [\"/x\"]\n")}, ""},
		{"file credential", map[string]fakeSpec{"leaf": op("[env]\nT = \"file:/x\"\n")}, "env.T: specs may only set literal env values, not a file source"},
		{"env credential", map[string]fakeSpec{"leaf": op("[env]\nT = \"env:X\"\n")}, "env.T: specs may only set literal env values, not an env source"},
		{"literal env", map[string]fakeSpec{"leaf": op("[env]\nT = \"1\"\n")}, ""},
		{"cycle", map[string]fakeSpec{"a": op("extends = \"b\"\n"), "b": op("extends = \"c\"\n"), "c": op("extends = \"a\"\n")}, "a -> b -> c -> a"},
		{"self cycle", map[string]fakeSpec{"a": op("extends = \"a\"\n")}, "a -> a"},
		{"missing parent", map[string]fakeSpec{"leaf": op("extends = \"ghost\"\n")}, "extends \"ghost\""},
		{"invalid egress pattern", map[string]fakeSpec{"leaf": op("[egress]\nbad = [\"api.anthropic.com\", \"*.com\"]\n")}, "egress.bad"},
		{"redefined egress set", map[string]fakeSpec{"base": base, "leaf": op("extends = \"base\"\n[egress]\nanthropic = [\"evil.example.com\"]\n")}, "egress.anthropic"},
		{"redefined env name", map[string]fakeSpec{"base": base, "leaf": op("extends = \"base\"\n[env]\nA = \"2\"\n")}, "env.A"},
		{"redefined setup command", map[string]fakeSpec{"base": base, "leaf": op("extends = \"base\"\n[setup]\ncommand = [\"other\"]\n")}, "setup.command"},
		{"redefined setup cache_key", map[string]fakeSpec{"base": cached, "leaf": op("extends = \"base\"\n[setup]\ncache_key = \"other.lock\"\n")}, "setup.cache_key: already defined"},
		{"redefined setup cache_dir", map[string]fakeSpec{"base": cached, "leaf": op("extends = \"base\"\n[setup]\ncache_dir = \"/var/cache/y\"\n")}, "setup.cache_dir: already defined"},
		{"child adds egress set, env, context path", map[string]fakeSpec{"base": base, "leaf": op("extends = \"base\"\n[egress]\nmore = [\"m.example.com\"]\n[env]\nB = \"2\"\n[context]\nreadonly = [\"/notes\"]\n")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := fakeSource{specs: tc.specs}
			chain, err := LoadChain(src, "leaf")
			if _, isCycle := tc.specs["a"]; isCycle {
				chain, err = LoadChain(src, "a")
			}
			if err == nil {
				_, err = Merge(src, chain)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestChainOrderAndMergedContents(t *testing.T) {
	src := fakeSource{specs: map[string]fakeSpec{
		"base": op("[egress]\nanthropic = [\"api.anthropic.com\"]\n[env]\nA = \"1\"\n[context]\nreadonly = [\"/notes\"]\n[setup]\ncommand = [\"make\"]\ncache_key = \"yarn.lock\"\ncache_dir = \"/var/cache/x\"\n"),
		"mid":  op("extends = \"base\"\n[egress]\nmore = [\"m.example.com\"]\n[env]\nB = \"2\"\n"),
		"leaf": repo("extends = \"mid\"\n[context]\nreadonly = [\"/docs\"]\n"),
	}}
	chain, err := LoadChain(src, "leaf")
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, ls := range chain {
		refs = append(refs, ls.Ref)
	}
	if got := strings.Join(refs, ","); got != "base,mid,leaf" {
		t.Fatalf("chain = %s; want root first: base,mid,leaf", got)
	}
	m, err := Merge(src, chain)
	if err != nil {
		t.Fatal(err)
	}
	wantEgress := map[string][]string{"anthropic": {"api.anthropic.com"}, "more": {"m.example.com"}}
	if !reflect.DeepEqual(m.Egress, wantEgress) {
		t.Errorf("egress = %v; want %v", m.Egress, wantEgress)
	}
	if want := map[string]string{"A": "1", "B": "2"}; !reflect.DeepEqual(m.Env, want) {
		t.Errorf("env = %v; want %v", m.Env, want)
	}
	if want := []string{"/notes", "/docs"}; !reflect.DeepEqual(m.ContextRO, want) {
		t.Errorf("context readonly = %v; want %v", m.ContextRO, want)
	}
	if want := (Setup{Command: []string{"make"}, CacheKey: "yarn.lock", CacheDir: "/var/cache/x"}); !reflect.DeepEqual(m.Setup, want) {
		t.Errorf("setup = %+v; want %+v", m.Setup, want)
	}
	if len(m.Layers) != 0 || m.ManagedSettings != nil {
		t.Errorf("layers = %v, managed settings = %s; want none", m.Layers, m.ManagedSettings)
	}
}

func TestMergeImageLayers(t *testing.T) {
	src := fakeSource{specs: map[string]fakeSpec{
		"base": op("[image]\ndockerfile = \"images/base/Dockerfile\"\n"),
		"leaf": op("extends = \"base\"\n[image]\ndockerfile = \"images/leaf/Dockerfile\"\ncontext = \"shared\"\n"),
	}}
	chain, err := LoadChain(src, "leaf")
	if err != nil {
		t.Fatal(err)
	}
	m, err := Merge(src, chain)
	if err != nil {
		t.Fatal(err)
	}
	want := []Layer{
		{Spec: "base", Dockerfile: "/cfg/images/base/Dockerfile", Context: "/cfg/images/base"},
		{Spec: "leaf", Dockerfile: "/cfg/images/leaf/Dockerfile", Context: "/cfg/shared"},
	}
	if !reflect.DeepEqual(m.Layers, want) {
		t.Errorf("layers = %+v; want %+v", m.Layers, want)
	}
}

func TestSpecExists(t *testing.T) {
	src := fakeSource{specs: map[string]fakeSpec{
		"ok":      op("[env]\nA = \"1\"\n"),
		"garbled": op("[env\n"),
	}}
	if found, err := SpecExists(src, "ok"); !found || err != nil {
		t.Errorf("ok: found %v, err %v", found, err)
	}
	if found, err := SpecExists(src, "ghost"); found || err != nil {
		t.Errorf("ghost: found %v, err %v; want not found, no error", found, err)
	}
	if found, err := SpecExists(src, "garbled"); found || err == nil || !strings.Contains(err.Error(), `spec "garbled"`) {
		t.Errorf("garbled: found %v, err %v; want an error", found, err)
	}
}

func TestManagedSettingsMerge(t *testing.T) {
	tests := []struct {
		name          string
		parent, child string
		want          string
		wantErr       string
	}{
		{"objects merge", `{"a":{"x":1}}`, `{"a":{"y":2},"b":true}`, `{"a":{"x":1,"y":2},"b":true}`, ""},
		{"arrays union, parent first", `{"d":["a","b"]}`, `{"d":["c","a"]}`, `{"d":["a","b","c"]}`, ""},
		{"equal scalars", `{"k":"v","n":1}`, `{"k":"v","n":1}`, `{"k":"v","n":1}`, ""},
		{"conflicting scalar", `{"k":"v"}`, `{"k":"w"}`, "", "k: scalar already set"},
		{"nested conflicting scalar", `{"a":{"k":1}}`, `{"a":{"k":2}}`, "", "a.k"},
		{"type conflict", `{"a":[1]}`, `{"a":"x"}`, "", "a: type conflict"},
		{"not an object", `{}`, `[1]`, "", "JSON object"},
		{"concatenated objects", `{"a":1}{"b":2}`, `{}`, "", "after the JSON object"},
		{"trailing garbage", `{"a":1} x`, `{}`, "", "after the JSON object"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := fakeSource{
				specs: map[string]fakeSpec{
					"p": op("[claude]\nmanaged_settings = \"p.json\"\n"),
					"c": op("extends = \"p\"\n[claude]\nmanaged_settings = \"c.json\"\n"),
				},
				files: map[string]string{"p.json": tc.parent, "c.json": tc.child},
			}
			chain, err := LoadChain(src, "c")
			if err != nil {
				t.Fatal(err)
			}
			m, err := Merge(src, chain)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(m.ManagedSettings) != tc.want {
				t.Fatalf("got %s want %s", m.ManagedSettings, tc.want)
			}
		})
	}
}

func TestParseOrigin(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://github.com/afresh/scanner.git", "afresh/scanner"},
		{"https://github.com/afresh/scanner", "afresh/scanner"},
		{"https://github.com/afresh/scanner/", "afresh/scanner"},
		{"https://user:tok@github.com/afresh/scanner.git", "afresh/scanner"},
		{"ssh://git@github.com/afresh/scanner.git", "afresh/scanner"},
		{"ssh://git@github.com:22/afresh/scanner.git", "afresh/scanner"},
		{"git@github.com:afresh/scanner.git", "afresh/scanner"},
		{"git@github.com:afresh/scanner", "afresh/scanner"},
		{"github.com:afresh/scanner.git", "afresh/scanner"},
		{"https://github.com/scanner.git", ""},
		{"https://gitlab.com/a/b/c.git", ""},
		{"/local/path/repo.git", ""},
		{"git@github.com:../etc", ""},
		{"", ""},
	}
	for _, tc := range tests {
		got, ok := ParseOrigin(tc.url)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("ParseOrigin(%q) = %q, %v; want %q", tc.url, got, ok, tc.want)
		}
	}
}

func TestResolveValue(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	lookup := func(k string) (string, bool) {
		v, ok := map[string]string{"SET": "from-env", "EMPTY": ""}[k]
		return v, ok
	}
	tests := []struct {
		name    string
		v       plan.EnvVar
		want    string
		wantErr string
	}{
		{"literal", plan.EnvVar{Name: "L", Source: "literal", Value: "a b"}, "a b", ""},
		{"file trims surrounding whitespace", plan.EnvVar{Name: "F", Source: "file", Ref: write("ok", "  tok-123\n\n")}, "tok-123", ""},
		{"file interior newline", plan.EnvVar{Name: "F", Source: "file", Ref: write("wrapped", "abc\ndef\n")}, "", "inside the value"},
		{"file interior space", plan.EnvVar{Name: "F", Source: "file", Ref: write("space", "abc def")}, "", "inside the value"},
		{"file interior control", plan.EnvVar{Name: "F", Source: "file", Ref: write("ctl", "abc\x00def")}, "", "inside the value"},
		{"file empty", plan.EnvVar{Name: "F", Source: "file", Ref: write("empty", " \n")}, "", "empty"},
		{"file missing", plan.EnvVar{Name: "F", Source: "file", Ref: filepath.Join(dir, "none")}, "", "env.F"},
		{"env set", plan.EnvVar{Name: "E", Source: "env", Ref: "SET"}, "from-env", ""},
		{"env unset", plan.EnvVar{Name: "E", Source: "env", Ref: "UNSET"}, "", "unset"},
		{"env empty", plan.EnvVar{Name: "E", Source: "env", Ref: "EMPTY"}, "", "unset or empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveValue(tc.v, lookup)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				if strings.Contains(err.Error(), "def") {
					t.Fatalf("error leaks value: %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestParseSourceAndExpandHome(t *testing.T) {
	for raw, want := range map[string]string{"file:/x": "file", "env:X": "env", "plain": "literal", "https://x": "literal"} {
		if k, _, err := ParseSource(raw); err != nil || k != want {
			t.Errorf("ParseSource(%q) = %q, %v", raw, k, err)
		}
	}
	for _, raw := range []string{"file:", "env:"} {
		if _, _, err := ParseSource(raw); err == nil {
			t.Errorf("ParseSource(%q) should fail", raw)
		}
	}
	tests := []struct {
		in, want string
		fail     bool
	}{
		{"~/a/b", "/h/a/b", false}, {"~", "/h", false}, {"/abs/../x", "/x", false},
		{"rel/path", "", true}, {"~other/x", "", true}, {"", "", true},
	}
	for _, tc := range tests {
		got, err := ExpandHome(tc.in, "/h")
		if (err != nil) != tc.fail || got != tc.want {
			t.Errorf("ExpandHome(%q) = %q, %v", tc.in, got, err)
		}
	}
}
