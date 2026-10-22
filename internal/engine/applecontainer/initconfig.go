package applecontainer

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"

	"github.com/sdougbrown/paddock/images"
	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
)

const (
	proxyID   = 970
	proxyPort = 15443
)

// initInput carries the host-derived values the init config needs.
type initInput struct {
	User          initcfg.User
	Verify        bool
	TranscriptDir string
	// Mounts is the full guest mount list, as passed to `container run`.
	Mounts []guestMount
	// Passthrough lists the invoking environment's TERM/COLORTERM/LANG as NAME=VALUE.
	Passthrough []string
}

// userSettingsFile is the name of the merged settings file in the run's settings dir.
const userSettingsFile = "user-settings.json"

// userSettingsPath is where the merged settings file is mounted read-only in
// the guest: Claude Code's user scope, which merges with project and local
// scopes and is not shadowed by server-managed settings.
func userSettingsPath(home string) string { return path.Join(home, ".claude", "settings.json") }

// workdir is the plan's working directory, defaulting to the workspace.
func workdir(p *plan.Plan) string {
	if p.Workdir != "" {
		return p.Workdir
	}
	return p.Workspace
}

// buildInitConfig assembles the init config for p. Literal env values go into
// Env; any file or env source makes Secrets point at secrets.env.
func buildInitConfig(p *plan.Plan, in initInput) (*initcfg.Config, error) {
	cfg := &initcfg.Config{
		Version:  initcfg.Version,
		User:     in.User,
		ProxyUID: proxyID,
		ProxyGID: proxyID,
		Workdir:  workdir(p),
		Argv:     p.Argv,
		Verify:   in.Verify,
		Network: initcfg.Network{
			Policy:    p.Network.Policy,
			Hosts:     p.Network.Hosts,
			ProxyPort: proxyPort,
			DenyLog:   initcfg.LogDir + "/deny.jsonl",
			ErrorLog:  initcfg.LogDir + "/proxy.log",
		},
	}
	for _, e := range p.Env {
		if e.Source == plan.SourceLiteral {
			cfg.Env = append(cfg.Env, e.Name+"="+e.Value)
		} else {
			cfg.Secrets = initcfg.SecretsPath
		}
	}
	cfg.Env = append(cfg.Env, in.Passthrough...)

	claudeJSON, err := json.Marshal(map[string]any{
		"hasCompletedOnboarding": true,
		"lastOnboardingVersion":  images.ClaudeCodeVersion,
		"projects": map[string]any{
			workdir(p): map[string]any{"hasTrustDialogAccepted": true},
		},
	})
	if err != nil {
		return nil, err
	}
	cfg.Claude = initcfg.ClaudeHome{
		ManagedSettings: p.Claude.ManagedSettings,
		ClaudeJSON:      claudeJSON,
		Skills:          p.Claude.Skills,
		Agents:          p.Claude.Agents,
		TranscriptDir:   in.TranscriptDir,
	}
	for _, f := range p.Claude.SeedFiles {
		cfg.Claude.SeedFiles = append(cfg.Claude.SeedFiles, filepath.Base(f))
	}
	for _, m := range in.Mounts {
		cfg.Mounts = append(cfg.Mounts, initcfg.Mount{Path: m.Guest, ReadOnly: m.ReadOnly})
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("init config: %w", err)
	}
	return cfg, nil
}
