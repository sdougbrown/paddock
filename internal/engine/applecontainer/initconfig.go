package applecontainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

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

// splitEnv separates plan env into literals, which go into the init config,
// and secret refs (file or env sources), whose values are resolved on the host
// and delivered only through secrets.env.
func splitEnv(env []plan.EnvVar) (literals, secretRefs []plan.EnvVar) {
	for _, e := range env {
		if e.Source == plan.SourceLiteral {
			literals = append(literals, e)
		} else {
			secretRefs = append(secretRefs, e)
		}
	}
	return literals, secretRefs
}

// buildInitConfig assembles the init config for p. Literal env values go into
// Env, with the passthrough variables added only where the plan does not set
// the same name; any file or env source makes Secrets point at secrets.env.
// An empty Plan.Workdir is an error.
func buildInitConfig(p *plan.Plan, in initInput) (*initcfg.Config, error) {
	if p.Workdir == "" {
		return nil, errors.New("plan has no workdir")
	}
	cfg := &initcfg.Config{
		Version:  initcfg.Version,
		User:     in.User,
		ProxyUID: proxyID,
		ProxyGID: proxyID,
		Workdir:  p.Workdir,
		Argv:     p.Argv,
		Verify:   in.Verify,
		Network: initcfg.Network{
			Policy:    p.Network.Policy,
			Hosts:     p.Network.Hosts,
			ProxyPort: proxyPort,
			DenyLog:   initcfg.LogDir + "/" + initcfg.DenyLogName,
			ErrorLog:  initcfg.LogDir + "/" + initcfg.ErrorLogName,
		},
	}
	literals, secretRefs := splitEnv(p.Env)
	if len(secretRefs) > 0 {
		cfg.Secrets = initcfg.SecretsPath
	}
	planned := map[string]bool{}
	for _, e := range literals {
		planned[e.Name] = true
		cfg.Env = append(cfg.Env, e.Name+"="+e.Value)
	}
	for _, kv := range in.Passthrough {
		if name, _, _ := strings.Cut(kv, "="); !planned[name] {
			cfg.Env = append(cfg.Env, kv)
		}
	}

	claudeJSON, err := json.Marshal(map[string]any{
		"hasCompletedOnboarding": true,
		"lastOnboardingVersion":  images.ClaudeCodeVersion,
		"projects": map[string]any{
			p.Workdir: map[string]any{"hasTrustDialogAccepted": true},
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
