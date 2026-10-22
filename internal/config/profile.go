package config

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/sdougbrown/paddock/internal/envname"
)

// Profile is a role profile: what a command may access.
type Profile struct {
	Engine      string            `toml:"engine"`
	DefaultSpec string            `toml:"default_spec"`
	Workspace   ProfileWorkspace  `toml:"workspace"`
	Network     ProfileNetwork    `toml:"network"`
	Env         map[string]string `toml:"env"`
	Mounts      ProfileMounts     `toml:"mounts"`
	Limits      Limits            `toml:"limits"`
	Claude      ProfileClaude     `toml:"claude"`
	Hooks       *map[string]any   `toml:"hooks"`
}

type ProfileWorkspace struct {
	Mode     string   `toml:"mode"`
	Readonly []string `toml:"readonly"`
}

type ProfileNetwork struct {
	Policy string   `toml:"policy"`
	Run    []string `toml:"run"`
	Setup  []string `toml:"setup"`
}

type ProfileMounts struct {
	Readonly []string `toml:"readonly"`
}

type Limits struct {
	CPUs   int    `toml:"cpus"`
	Memory string `toml:"memory"`
}

type ProfileClaude struct {
	SeedFiles []string `toml:"seed_files"`
	Skills    string   `toml:"skills"`
	Agents    string   `toml:"agents"`
}

// Engines supported in v1.
const EngineAppleContainer = "apple-container"

var memoryRe = regexp.MustCompile(`^[0-9]+[KkMmGg]?$`)

func (p *Profile) validate() error {
	if p.Hooks != nil {
		return fmt.Errorf("[hooks]: host hooks are not supported in v1")
	}
	if p.Engine != EngineAppleContainer {
		return fmt.Errorf("engine: %q is not supported (want %q)", p.Engine, EngineAppleContainer)
	}
	switch p.Workspace.Mode {
	case "", "ro", "rw":
	default:
		return fmt.Errorf("workspace.mode: %q must be \"rw\" or \"ro\"", p.Workspace.Mode)
	}
	switch p.Network.Policy {
	case "", "allowlist", "none", "host":
	default:
		return fmt.Errorf("network.policy: %q must be \"allowlist\", \"none\", or \"host\"", p.Network.Policy)
	}
	if p.DefaultSpec != "" && !ValidRef(p.DefaultSpec) {
		return fmt.Errorf("default_spec: invalid spec ref %q", p.DefaultSpec)
	}
	names := make([]string, 0, len(p.Env))
	for n := range p.Env {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !envname.Valid(n) {
			return fmt.Errorf("env.%s: invalid variable name", n)
		}
	}
	if p.Limits.CPUs < 0 {
		return fmt.Errorf("limits.cpus: must not be negative")
	}
	if p.Limits.Memory != "" && !memoryRe.MatchString(p.Limits.Memory) {
		return fmt.Errorf("limits.memory: %q must look like 512M or 6G", p.Limits.Memory)
	}
	return nil
}
