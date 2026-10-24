package initcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		Version:  Version,
		User:     User{Name: "agent", UID: 1000, GID: 1000, Home: "/home/agent"},
		ProxyUID: 990,
		ProxyGID: 990,
		Workdir:  "/workspace",
		Argv:     []string{"claude", "--print"},
		Env:      []string{"TERM=xterm-256color"},
		Secrets:  SecretsPath,
		Network: Network{
			Policy:    "allowlist",
			Hosts:     []string{"api.anthropic.com"},
			ProxyPort: 3128,
			DenyLog:   LogDir + "/deny.log",
			ErrorLog:  LogDir + "/error.log",
		},
		Claude: ClaudeHome{
			SeedFiles:     []string{"settings.json"},
			Skills:        "/opt/skills",
			Agents:        "/opt/agents",
			TranscriptDir: "/home/agent/.claude/projects/-workspace",
		},
		Mounts: []Mount{{Path: StateDir, ReadOnly: true}},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{name: "valid", mutate: func(c *Config) {}},
		{name: "valid none policy", mutate: func(c *Config) {
			c.Network = Network{Policy: "none"}
		}},
		{name: "valid host policy", mutate: func(c *Config) {
			c.Network = Network{Policy: "host"}
		}},
		{name: "valid verify without argv", mutate: func(c *Config) {
			c.Argv = nil
			c.Verify = true
		}},
		{name: "wrong version", wantErr: true, mutate: func(c *Config) {
			c.Version = Version + 1
		}},
		{name: "empty user name", wantErr: true, mutate: func(c *Config) {
			c.User.Name = ""
		}},
		{name: "zero user uid", wantErr: true, mutate: func(c *Config) {
			c.User.UID = 0
		}},
		{name: "zero user gid", wantErr: true, mutate: func(c *Config) {
			c.User.GID = 0
		}},
		{name: "relative user home", wantErr: true, mutate: func(c *Config) {
			c.User.Home = "home/agent"
		}},
		{name: "relative workdir", wantErr: true, mutate: func(c *Config) {
			c.Workdir = "workspace"
		}},
		{name: "valid absolute claude paths", mutate: func(c *Config) {
			c.Claude.Skills, c.Claude.Agents, c.Claude.TranscriptDir = "/s", "/a", "/t"
		}},
		{name: "relative claude skills", wantErr: true, mutate: func(c *Config) {
			c.Claude.Skills = "skills"
		}},
		{name: "relative claude agents", wantErr: true, mutate: func(c *Config) {
			c.Claude.Agents = "agents"
		}},
		{name: "relative claude transcript dir", wantErr: true, mutate: func(c *Config) {
			c.Claude.TranscriptDir = "t"
		}},
		{name: "empty argv without verify", wantErr: true, mutate: func(c *Config) {
			c.Argv = nil
		}},
		{name: "unknown policy", wantErr: true, mutate: func(c *Config) {
			c.Network.Policy = "open"
		}},
		{name: "allowlist without hosts", wantErr: true, mutate: func(c *Config) {
			c.Network.Hosts = nil
		}},
		{name: "allowlist zero proxy port", wantErr: true, mutate: func(c *Config) {
			c.Network.ProxyPort = 0
		}},
		{name: "allowlist without deny log", wantErr: true, mutate: func(c *Config) {
			c.Network.DenyLog = ""
		}},
		{name: "allowlist zero proxy uid", wantErr: true, mutate: func(c *Config) {
			c.ProxyUID = 0
		}},
		{name: "allowlist proxy uid equals user uid", wantErr: true, mutate: func(c *Config) {
			c.ProxyUID = c.User.UID
		}},
		{name: "allowlist zero proxy gid", wantErr: true, mutate: func(c *Config) {
			c.ProxyGID = 0
		}},
		{name: "root home", wantErr: true, mutate: func(c *Config) {
			c.User.Home = "/"
		}},
		{name: "root home unclean", wantErr: true, mutate: func(c *Config) {
			c.User.Home = "/a/.."
		}},
		{name: "proxy port too large", wantErr: true, mutate: func(c *Config) {
			c.Network.ProxyPort = 65536
		}},
		{name: "empty seed file", wantErr: true, mutate: func(c *Config) {
			c.Claude.SeedFiles = []string{""}
		}},
		{name: "dot seed file", wantErr: true, mutate: func(c *Config) {
			c.Claude.SeedFiles = []string{"."}
		}},
		{name: "dotdot seed file", wantErr: true, mutate: func(c *Config) {
			c.Claude.SeedFiles = []string{".."}
		}},
		{name: "seed file with slash", wantErr: true, mutate: func(c *Config) {
			c.Claude.SeedFiles = []string{"sub/settings.json"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			err := c.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.HasPrefix(err.Error(), "initcfg: ") {
				t.Errorf("error %q lacks initcfg: prefix", err)
			}
		})
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "init.json")
	want := validConfig()

	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	// Unknown fields must be rejected on read.
	var raw map[string]any
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	raw["bogus"] = 1
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Read(path); err == nil {
		t.Error("Read accepted a config with an unknown field")
	}
}

func TestReadRejectsTrailingData(t *testing.T) {
	data, err := json.Marshal(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"concatenated config": append(append(append([]byte{}, data...), '\n'), data...),
		"trailing garbage":    append(append([]byte{}, data...), " x"...),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "init.json")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(path); err == nil || !strings.Contains(err.Error(), "after the JSON object") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
