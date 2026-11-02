package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/sdougbrown/paddock/internal/envname"
	"github.com/sdougbrown/paddock/internal/initcfg"
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
const (
	EngineAppleContainer = "apple-container"
	EnginePodman         = "podman"
	EngineBwrap          = "bwrap"
)

// engines lists every supported engine name.
var engines = []string{EngineAppleContainer, EnginePodman, EngineBwrap}

var memoryRe = regexp.MustCompile(`^[0-9]+[KkMmGg]$`)

func (p *Profile) validate() error {
	if p.Hooks != nil {
		return fmt.Errorf("[hooks]: host hooks are not supported in v1")
	}
	if !slices.Contains(engines, p.Engine) {
		return fmt.Errorf("engine: %q is not supported (want one of %q)", p.Engine, engines)
	}
	if p.Engine == EngineBwrap {
		// Tier-0 isolation: namespaces only, no CAP_NET_ADMIN and no cgroup
		// enforcement. Fail closed here, where the error names the profile.
		if p.Network.Policy == initcfg.PolicyAllowlist {
			return fmt.Errorf("network.policy: the bwrap engine cannot enforce an egress allowlist; use \"host\" or \"none\"")
		}
		if p.Limits.CPUs != 0 || p.Limits.Memory != "" {
			return fmt.Errorf("[limits]: the bwrap engine cannot enforce cpu or memory limits; remove them or use a container engine")
		}
	}
	switch p.Workspace.Mode {
	case "", "ro", "rw":
	default:
		return fmt.Errorf("workspace.mode: %q must be \"rw\" or \"ro\"", p.Workspace.Mode)
	}
	if p.Network.Policy != "" && !initcfg.ValidPolicy(p.Network.Policy) {
		return fmt.Errorf("network.policy: %q must be one of %q", p.Network.Policy, initcfg.Policies())
	}
	if err := validateSetNames("network.run", p.Network.Run); err != nil {
		return err
	}
	if err := validateSetNames("network.setup", p.Network.Setup); err != nil {
		return err
	}
	if p.DefaultSpec != "" && !ValidRef(p.DefaultSpec) {
		return fmt.Errorf("default_spec: invalid spec ref %q", p.DefaultSpec)
	}
	if err := validateEnvNames(p.Env); err != nil {
		return err
	}
	if p.Limits.CPUs < 0 {
		return fmt.Errorf("limits.cpus: must not be negative")
	}
	if p.Limits.Memory != "" && !memoryRe.MatchString(p.Limits.Memory) {
		return fmt.Errorf("limits.memory: %q must look like 512M or 6G", p.Limits.Memory)
	}
	return nil
}

// validateEnvNames reports the first invalid env name in sorted order.
func validateEnvNames(env map[string]string) error {
	for _, n := range slices.Sorted(maps.Keys(env)) {
		if !envname.Valid(n) {
			return fmt.Errorf("env.%s: invalid variable name", n)
		}
	}
	return nil
}

// validateSetNames checks that every egress set a profile references has a
// name a spec's [egress] table could define.
func validateSetNames(key string, names []string) error {
	for _, n := range names {
		if !nameRe.MatchString(n) {
			return fmt.Errorf("%s: invalid egress set name %q", key, n)
		}
	}
	return nil
}
