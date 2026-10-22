// Package initcfg is the init configuration the host writes and paddock-init reads inside the guest.
package initcfg

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

const Version = 1

// Guest paths shared by the host engine and paddock-init.
const (
	BinDir              = "/opt/paddock" // ro mount holding the paddock-init binary
	InitPath            = "/opt/paddock/paddock-init"
	StateDir            = "/run/paddock" // ro mount: init.json and seed/
	ConfigPath          = "/run/paddock/init.json"
	SeedDir             = "/run/paddock/seed"
	SecretsDir          = "/run/paddock-secrets" // rw mount: secrets.env, unlinked by init after reading
	SecretsPath         = "/run/paddock-secrets/secrets.env"
	LogDir              = "/run/paddock-log" // rw mount: proxy denial and error logs
	ManagedSettingsPath = "/etc/claude-code/managed-settings.json"
)

// File names inside LogDir.
const (
	DenyLogName  = "deny.jsonl" // proxy denial records
	ErrorLogName = "proxy.log"  // proxy stderr
)

// Config is the full init configuration for one guest run.
type Config struct {
	Version  int        `json:"version"`
	User     User       `json:"user"`
	ProxyUID int        `json:"proxy_uid"`
	ProxyGID int        `json:"proxy_gid"`
	Workdir  string     `json:"workdir"`
	Argv     []string   `json:"argv"`
	Env      []string   `json:"env,omitempty"`     // non-secret NAME=VALUE for the agent
	Secrets  string     `json:"secrets,omitempty"` // guest path of secrets.env
	Network  Network    `json:"network"`
	Claude   ClaudeHome `json:"claude"`
	Mounts   []Mount    `json:"mounts,omitempty"` // guest mounts, for the verify ro checks
	Verify   bool       `json:"verify"`
}

// User is the unprivileged guest account the agent runs as.
type User struct {
	Name string `json:"name"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	Home string `json:"home"`
}

// Network describes the guest's egress policy.
type Network struct {
	Policy    string   `json:"policy"` // "allowlist" | "none" | "host"
	Hosts     []string `json:"hosts,omitempty"`
	ProxyPort int      `json:"proxy_port,omitempty"`
	DenyLog   string   `json:"deny_log,omitempty"`
	ErrorLog  string   `json:"error_log,omitempty"`
}

// ClaudeHome describes the Claude Code state paddock-init prepares for the agent.
type ClaudeHome struct {
	ManagedSettings json.RawMessage `json:"managed_settings,omitempty"`
	ClaudeJSON      json.RawMessage `json:"claude_json,omitempty"`    // seed for ~/.claude.json
	SeedFiles       []string        `json:"seed_files,omitempty"`     // base names under SeedDir copied into ~/.claude/
	Skills          string          `json:"skills,omitempty"`         // symlink target for ~/.claude/skills
	Agents          string          `json:"agents,omitempty"`         // symlink target for ~/.claude/agents
	TranscriptDir   string          `json:"transcript_dir,omitempty"` // ~/.claude/projects/<enc>, already mounted
}

// Mount is a guest mount point, listed so verify can check its read-only state.
type Mount struct {
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only"`
}

// Validate reports whether the config is internally consistent.
func (c *Config) Validate() error {
	if c.Version != Version {
		return fmt.Errorf("initcfg: version %d, want %d", c.Version, Version)
	}
	if c.User.Name == "" {
		return fmt.Errorf("initcfg: user name is empty")
	}
	if c.User.UID <= 0 || c.User.GID <= 0 {
		return fmt.Errorf("initcfg: invalid user uid/gid %d/%d", c.User.UID, c.User.GID)
	}
	if !path.IsAbs(c.User.Home) {
		return fmt.Errorf("initcfg: user home %q is not absolute", c.User.Home)
	}
	if path.Clean(c.User.Home) == "/" {
		return fmt.Errorf("initcfg: user home must not be the filesystem root")
	}
	if !path.IsAbs(c.Workdir) {
		return fmt.Errorf("initcfg: workdir %q is not absolute", c.Workdir)
	}
	if len(c.Argv) == 0 && !c.Verify {
		return fmt.Errorf("initcfg: argv is empty and verify is off")
	}
	for _, f := range c.Claude.SeedFiles {
		if f == "" || f == "." || f == ".." || strings.Contains(f, "/") {
			return fmt.Errorf("initcfg: seed file %q is not a base name", f)
		}
	}
	switch c.Network.Policy {
	case "allowlist":
		if len(c.Network.Hosts) == 0 {
			return fmt.Errorf("initcfg: allowlist policy has no hosts")
		}
		if c.Network.ProxyPort <= 0 || c.Network.ProxyPort > 65535 {
			return fmt.Errorf("initcfg: allowlist policy has invalid proxy port %d", c.Network.ProxyPort)
		}
		if c.Network.DenyLog == "" {
			return fmt.Errorf("initcfg: allowlist policy has no deny log")
		}
		if c.ProxyUID <= 0 {
			return fmt.Errorf("initcfg: allowlist policy has invalid proxy uid %d", c.ProxyUID)
		}
		if c.ProxyGID <= 0 {
			return fmt.Errorf("initcfg: allowlist policy has invalid proxy gid %d", c.ProxyGID)
		}
		if c.ProxyUID == c.User.UID {
			return fmt.Errorf("initcfg: proxy uid equals user uid %d", c.ProxyUID)
		}
	case "none", "host":
	default:
		return fmt.Errorf("initcfg: unknown network policy %q", c.Network.Policy)
	}
	return nil
}

// Write validates c and writes it to path as indented JSON with mode 0600.
func Write(path string, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("initcfg: marshal: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// Read loads the config at path, rejecting unknown fields, and validates it.
func Read(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("initcfg: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("initcfg: decode %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("initcfg: decode %s: unexpected data after the JSON object", path)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
