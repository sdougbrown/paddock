package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeRoot(t *testing.T, files map[string]string) Root {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Root{Dir: dir}
}

func TestResolveRoot(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		home string
		want string
		fail bool
	}{
		{"explicit", map[string]string{"PADDOCK_CONFIG_DIR": "/x", "XDG_CONFIG_HOME": "/y"}, "/h", "/x", false},
		{"xdg", map[string]string{"XDG_CONFIG_HOME": "/y"}, "/h", "/y/paddock", false},
		{"home", nil, "/h", "/h/.config/paddock", false},
		{"nothing", nil, "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ResolveRoot(func(k string) string { return tc.env[k] }, tc.home)
			if (err != nil) != tc.fail || r.Dir != tc.want {
				t.Fatalf("got %q, %v", r.Dir, err)
			}
		})
	}
}

func TestLoadProfile(t *testing.T) {
	const ok = `engine = "apple-container"
[workspace]
mode = "rw"
readonly = ["repo"]
[network]
policy = "allowlist"
run = ["anthropic"]
[limits]
cpus = 4
memory = "6G"
`
	tests := []struct {
		name    string
		body    string
		wantErr string
		check   func(t *testing.T, p *Profile)
	}{
		{"valid", ok, "", func(t *testing.T, p *Profile) {
			if p.Limits.CPUs != 4 {
				t.Fatalf("limits.cpus = %d, want 4", p.Limits.CPUs)
			}
			if !slices.Equal(p.Workspace.Readonly, []string{"repo"}) || !slices.Equal(p.Network.Run, []string{"anthropic"}) {
				t.Fatalf("workspace.readonly = %v, network.run = %v", p.Workspace.Readonly, p.Network.Run)
			}
		}},
		{"valid env, mounts, and claude", ok + "[env]\nA = \"1\"\n[mounts]\nreadonly = [\"~/x\"]\n[claude]\nseed_files = [\"CLAUDE.md\"]\nskills = \"~/s\"\nagents = \"~/a\"\n", "", func(t *testing.T, p *Profile) {
			if p.Env["A"] != "1" || !slices.Equal(p.Mounts.Readonly, []string{"~/x"}) {
				t.Fatalf("env = %v, mounts.readonly = %v", p.Env, p.Mounts.Readonly)
			}
			if !slices.Equal(p.Claude.SeedFiles, []string{"CLAUDE.md"}) || p.Claude.Skills != "~/s" || p.Claude.Agents != "~/a" {
				t.Fatalf("claude = %+v", p.Claude)
			}
		}},
		{"valid default_spec", "default_spec = \"owner/repo\"\n" + ok, "", nil},
		{"valid zero cpus", strings.Replace(ok, "cpus = 4", "cpus = 0", 1), "", func(t *testing.T, p *Profile) {
			if p.Limits.CPUs != 0 {
				t.Fatalf("limits.cpus = %d, want 0", p.Limits.CPUs)
			}
		}},
		{"valid ro mode", strings.Replace(ok, `"rw"`, `"ro"`, 1), "", func(t *testing.T, p *Profile) {
			if p.Workspace.Mode != "ro" {
				t.Fatalf("workspace.mode = %q, want \"ro\"", p.Workspace.Mode)
			}
		}},
		{"valid no optional tables", "engine = \"apple-container\"\n", "", func(t *testing.T, p *Profile) {
			if p.Workspace.Mode != "" || p.Network.Policy != "" {
				t.Fatalf("workspace.mode = %q, network.policy = %q, want both empty", p.Workspace.Mode, p.Network.Policy)
			}
		}},
		{"valid none policy", strings.Replace(ok, "allowlist", "none", 1), "", func(t *testing.T, p *Profile) {
			if p.Network.Policy != "none" {
				t.Fatalf("network.policy = %q, want \"none\"", p.Network.Policy)
			}
		}},
		{"valid host policy", strings.Replace(ok, "allowlist", "host", 1), "", func(t *testing.T, p *Profile) {
			if p.Network.Policy != "host" {
				t.Fatalf("network.policy = %q, want \"host\"", p.Network.Policy)
			}
		}},
		{"valid lowercase memory unit", strings.Replace(ok, "6G", "512m", 1), "", func(t *testing.T, p *Profile) {
			if p.Limits.Memory != "512m" {
				t.Fatalf("limits.memory = %q, want \"512m\"", p.Limits.Memory)
			}
		}},
		{"negative cpus", strings.Replace(ok, "cpus = 4", "cpus = -1", 1), "limits.cpus", nil},
		{"traversing default_spec", "default_spec = \"../x\"\n" + ok, "default_spec", nil},
		{"unknown top-level key", ok + "bogus = 1\n", "bogus", nil},
		{"unknown nested key", ok + "[network.extra]\nx = 1\n", "network.extra", nil},
		{"hooks table", ok + "[hooks]\npre = \"x\"\n", "host hooks are not supported", nil},
		{"empty hooks table", ok + "[hooks]\n", "host hooks are not supported", nil},
		{"bad engine", strings.Replace(ok, "apple-container", "docker", 1), "engine", nil},
		{"bad mode", strings.Replace(ok, `"rw"`, `"wx"`, 1), "workspace.mode", nil},
		{"bad policy", strings.Replace(ok, "allowlist", "open", 1), "network.policy", nil},
		{"bad run set name", strings.Replace(ok, `run = ["anthropic"]`, `run = ["-bad"]`, 1), "network.run", nil},
		{"bad setup set name", strings.Replace(ok, `run = ["anthropic"]`, "run = [\"anthropic\"]\nsetup = [\"a/b\"]", 1), "network.setup", nil},
		{"bad memory", strings.Replace(ok, "6G", "lots", 1), "limits.memory", nil},
		{"unit-less memory", strings.Replace(ok, "6G", "512", 1), "limits.memory", nil},
		{"bad env name", ok + "[env]\n\"A-B\" = \"x\"\n", "env.A-B", nil},
		{"syntax error", "engine = \n", "p.toml", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := writeRoot(t, map[string]string{"profiles/p.toml": tc.body})
			p, err := r.LoadProfile("p")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("got %+v, %v", p, err)
				}
				if tc.check != nil {
					tc.check(t, p)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "p.toml") {
				t.Fatalf("want error containing %q and file name, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoadSpec(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
		check   func(t *testing.T, s *Spec)
	}{
		{"valid", "extends = \"afresh\"\n[egress]\na = [\"x.example.com\"]\n[context]\nreadonly = [\"~/x\"]\n", "", func(t *testing.T, s *Spec) {
			if s.Extends != "afresh" || !slices.Equal(s.Egress["a"], []string{"x.example.com"}) || !slices.Equal(s.Context.Readonly, []string{"~/x"}) {
				t.Fatalf("extends = %q, egress = %v, context.readonly = %v", s.Extends, s.Egress, s.Context.Readonly)
			}
		}},
		{"valid image", "[image]\ndockerfile = \"Dockerfile\"\ncontext = \"..\"\n", "", func(t *testing.T, s *Spec) {
			if s.Image.Dockerfile != "Dockerfile" || s.Image.Context != ".." {
				t.Fatalf("image = %+v", s.Image)
			}
		}},
		{"valid image without context", "[image]\ndockerfile = \"Dockerfile\"\n", "", nil},
		{"valid setup", "[setup]\ncommand = [\"yarn\", \"install\"]\ncache_key = \"yarn.lock\"\ncache_dir = \"/cache\"\n", "", func(t *testing.T, s *Spec) {
			if !slices.Equal(s.Setup.Command, []string{"yarn", "install"}) || s.Setup.CacheKey != "yarn.lock" || s.Setup.CacheDir != "/cache" {
				t.Fatalf("setup = %+v", s.Setup)
			}
		}},
		{"unknown key", "wat = 1\n", "wat", nil},
		{"rw mount key", "[mounts]\nreadwrite = [\"/x\"]\n", "mounts", nil},
		{"hooks table", "[hooks]\npre = \"x\"\n", "host hooks are not supported", nil},
		{"env file source", "[env]\nT = \"file:/x\"\n", "env.T", nil},
		{"env env source", "[env]\nT = \"env:X\"\n", "env.T", nil},
		{"literal env", "[env]\nT = \"1\"\n", "", nil},
		{"bad env name", "[env]\n\"A-B\" = \"x\"\n", "env.A-B", nil},
		{"bad egress host", "[egress]\na = [\"x/y\"]\n", "egress.a", nil},
		{"bad egress set name", "[egress]\n\"-bad\" = [\"x.example.com\"]\n", "egress.", nil},
		{"cache without command", "[setup]\ncache_key = \"yarn.lock\"\n", "setup", nil},
		{"context without dockerfile", "[image]\ncontext = \"x\"\n", "image.context", nil},
		{"bad extends", "extends = \"../x\"\n", "extends", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := writeRoot(t, map[string]string{"projects/o/r.toml": tc.body})
			s, err := r.LoadSpec("o/r")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if tc.check != nil {
					tc.check(t, s)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "r.toml") {
				t.Fatalf("want error containing %q and file name, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoadSpecMissing(t *testing.T) {
	r := writeRoot(t, nil)
	if _, err := r.LoadSpec("nope"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	if _, err := r.LoadSpec("a/b/c"); err == nil {
		t.Fatal("want error for three-segment ref")
	} else if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid ref reported as missing: %v", err)
	}
	if _, err := r.LoadProfile("../x"); err == nil {
		t.Fatal("want error for traversing profile name")
	}
}

func TestLoadProfileRejectsSpecStyleName(t *testing.T) {
	r := writeRoot(t, map[string]string{"profiles/a/b.toml": "engine = \"apple-container\"\n"})
	if _, err := r.LoadProfile("a/b"); err == nil || !strings.Contains(err.Error(), "invalid profile name") {
		t.Fatalf("want invalid profile name error, got %v", err)
	}
}

func TestRootPath(t *testing.T) {
	r := Root{Dir: "/cfg"}
	if got, err := r.Path("images/a/Dockerfile"); err != nil || got != filepath.FromSlash("/cfg/images/a/Dockerfile") {
		t.Errorf("Path(images/a/Dockerfile) = %q, %v", got, err)
	}
	for _, rel := range []string{"/etc/passwd", "../x", "a/../../x", ""} {
		if _, err := r.Path(rel); err == nil {
			t.Errorf("Path(%q) succeeded, want error", rel)
		}
	}
}

func TestDecodeErrorLocation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"wrong type", "engine = \"apple-container\"\n[limits]\ncpus = \"four\"\n", "p.toml:3:8: limits.cpus: "},
		{"unknown key", "engine = \"apple-container\"\nbogus = 1\n", "p.toml:2:1: bogus: unknown key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := writeRoot(t, map[string]string{"profiles/p.toml": tc.body})
			_, err := r.LoadProfile("p")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}
