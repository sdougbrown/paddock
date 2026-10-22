package config

import (
	"errors"
	"os"
	"path/filepath"
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
	}{
		{"valid", ok, ""},
		{"unknown top-level key", ok + "bogus = 1\n", "bogus"},
		{"unknown nested key", ok + "[network.extra]\nx = 1\n", "network.extra"},
		{"hooks table", ok + "[hooks]\npre = \"x\"\n", "host hooks are not supported"},
		{"empty hooks table", ok + "[hooks]\n", "host hooks are not supported"},
		{"bad engine", strings.Replace(ok, "apple-container", "docker", 1), "engine"},
		{"bad mode", strings.Replace(ok, `"rw"`, `"wx"`, 1), "workspace.mode"},
		{"bad policy", strings.Replace(ok, "allowlist", "open", 1), "network.policy"},
		{"bad memory", strings.Replace(ok, "6G", "lots", 1), "limits.memory"},
		{"bad env name", ok + "[env]\n\"A-B\" = \"x\"\n", "env.A-B"},
		{"syntax error", "engine = \n", "p.toml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := writeRoot(t, map[string]string{"profiles/p.toml": tc.body})
			p, err := r.LoadProfile("p")
			if tc.wantErr == "" {
				if err != nil || p.Limits.CPUs != 4 {
					t.Fatalf("got %+v, %v", p, err)
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
	}{
		{"valid", "extends = \"afresh\"\n[egress]\na = [\"x.example.com\"]\n[context]\nreadonly = [\"~/x\"]\n", ""},
		{"unknown key", "wat = 1\n", "wat"},
		{"rw mount key", "[mounts]\nreadwrite = [\"/x\"]\n", "mounts"},
		{"hooks table", "[hooks]\npre = \"x\"\n", "host hooks are not supported"},
		{"env file source", "[env]\nT = \"file:/x\"\n", "env.T"},
		{"env env source", "[env]\nT = \"env:X\"\n", "env.T"},
		{"literal env", "[env]\nT = \"1\"\n", ""},
		{"bad egress host", "[egress]\na = [\"x/y\"]\n", "egress.a"},
		{"cache without command", "[setup]\ncache_key = \"yarn.lock\"\n", "setup"},
		{"context without dockerfile", "[image]\ncontext = \"x\"\n", "image.context"},
		{"bad extends", "extends = \"../x\"\n", "extends"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := writeRoot(t, map[string]string{"projects/o/r.toml": tc.body})
			_, err := r.LoadSpec("o/r")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
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
	}
	if _, err := r.LoadProfile("../x"); err == nil {
		t.Fatal("want error for traversing profile name")
	}
}

func TestRootPath(t *testing.T) {
	r := Root{Dir: "/cfg"}
	for rel, wantErr := range map[string]bool{"images/a/Dockerfile": false, "/etc/passwd": true, "../x": true, "a/../../x": true, "": true} {
		_, err := r.Path(rel)
		if (err != nil) != wantErr {
			t.Errorf("Path(%q) err=%v, wantErr=%v", rel, err, wantErr)
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
