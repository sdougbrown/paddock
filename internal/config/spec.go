package config

import (
	"fmt"
	"sort"

	"github.com/sdougbrown/paddock/internal/egresshost"
)

// Spec is a project spec layer: what a command's environment contains.
type Spec struct {
	Extends string              `toml:"extends"`
	Image   SpecImage           `toml:"image"`
	Egress  map[string][]string `toml:"egress"`
	Setup   SpecSetup           `toml:"setup"`
	Context SpecContext         `toml:"context"`
	Claude  SpecClaude          `toml:"claude"`
	Env     map[string]string   `toml:"env"`
	Hooks   *map[string]any     `toml:"hooks"`
}

type SpecImage struct {
	Dockerfile string `toml:"dockerfile"`
	Context    string `toml:"context"`
}

type SpecSetup struct {
	Command  []string `toml:"command"`
	CacheKey string   `toml:"cache_key"`
	CacheDir string   `toml:"cache_dir"`
}

type SpecContext struct {
	Readonly []string `toml:"readonly"`
}

type SpecClaude struct {
	ManagedSettings string `toml:"managed_settings"`
}

func (s *Spec) validate() error {
	if s.Hooks != nil {
		return fmt.Errorf("[hooks]: host hooks are not supported in v1")
	}
	if s.Extends != "" && !ValidRef(s.Extends) {
		return fmt.Errorf("extends: invalid spec ref %q", s.Extends)
	}
	if s.Image.Dockerfile == "" && s.Image.Context != "" {
		return fmt.Errorf("image.context: set without image.dockerfile")
	}
	setNames := make([]string, 0, len(s.Egress))
	for n := range s.Egress {
		setNames = append(setNames, n)
	}
	sort.Strings(setNames)
	for _, n := range setNames {
		if !nameRe.MatchString(n) {
			return fmt.Errorf("egress.%s: invalid set name", n)
		}
		for _, h := range s.Egress[n] {
			if err := egresshost.Validate(h); err != nil {
				return fmt.Errorf("egress.%s: %w", n, err)
			}
		}
	}
	if len(s.Setup.Command) == 0 && (s.Setup.CacheKey != "" || s.Setup.CacheDir != "") {
		return fmt.Errorf("setup: cache_key/cache_dir set without command")
	}
	envNames := make([]string, 0, len(s.Env))
	for n := range s.Env {
		envNames = append(envNames, n)
	}
	sort.Strings(envNames)
	for _, n := range envNames {
		if !validEnvName(n) {
			return fmt.Errorf("env.%s: invalid variable name", n)
		}
	}
	return nil
}
