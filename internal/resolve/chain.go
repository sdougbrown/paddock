// Package resolve turns a profile, a spec chain, and a workspace into a Plan,
// enforcing the trust and never-widen rules.
package resolve

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/egresshost"
)

// Trust is the provenance level of a spec.
type Trust int

const (
	// TrustOperator marks specs under the operator-owned config root.
	TrustOperator Trust = iota
	// TrustRepository marks specs committed to a repository.
	TrustRepository
)

func (t Trust) String() string {
	if t == TrustOperator {
		return "operator"
	}
	return "repository"
}

// SpecSource supplies specs and the config-root files they reference.
type SpecSource interface {
	// LoadSpec returns the spec for ref; a missing spec wraps fs.ErrNotExist.
	LoadSpec(ref string) (*config.Spec, Trust, error)
	// Path maps a config-root-relative path to an absolute host path.
	Path(rel string) (string, error)
	ReadFile(rel string) ([]byte, error)
}

// RootSource loads operator-trust specs from a config root.
type RootSource struct{ Root config.Root }

func (s RootSource) LoadSpec(ref string) (*config.Spec, Trust, error) {
	spec, err := s.Root.LoadSpec(ref)
	return spec, TrustOperator, err
}

func (s RootSource) Path(rel string) (string, error) { return s.Root.Path(rel) }

func (s RootSource) ReadFile(rel string) ([]byte, error) {
	p, err := s.Root.Path(rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// LoadedSpec is one spec in a chain.
type LoadedSpec struct {
	Ref   string
	Spec  *config.Spec
	Trust Trust
}

// LoadChain loads ref and its extends ancestors and returns them root first.
// It rejects cycles and missing parents and applies the per-spec trust rules.
func LoadChain(src SpecSource, ref string) ([]LoadedSpec, error) {
	var leafFirst []LoadedSpec
	seen := map[string]bool{}
	var path []string
	for cur, child := ref, ""; cur != ""; {
		path = append(path, cur)
		if seen[cur] {
			return nil, fmt.Errorf("spec extends cycle: %s", strings.Join(path, " -> "))
		}
		seen[cur] = true
		spec, trust, err := src.LoadSpec(cur)
		if err != nil {
			if child != "" {
				return nil, fmt.Errorf("spec %q extends %q: %w", child, cur, err)
			}
			return nil, fmt.Errorf("spec %q: %w", cur, err)
		}
		leafFirst = append(leafFirst, LoadedSpec{Ref: cur, Spec: spec, Trust: trust})
		child, cur = cur, spec.Extends
	}
	chain := make([]LoadedSpec, len(leafFirst))
	for i, ls := range leafFirst {
		chain[len(leafFirst)-1-i] = ls
	}
	for i, ls := range chain {
		var parent *LoadedSpec
		if i > 0 {
			parent = &chain[i-1]
		}
		if err := checkSpec(ls, parent); err != nil {
			return nil, err
		}
	}
	return chain, nil
}

// checkSpec applies the rules that hold for a spec on its own: no env
// credentials, and repository specs define no egress or managed settings and
// extend an operator parent.
//
// Repository-trust specs must additionally be blocked, before paddock loads
// them, from declaring context.readonly host mounts, setup.command,
// setup.cache_dir and setup.cache_key, managed-settings allow rules and hooks
// (the managed_settings rule already blocks these), and image
// Dockerfiles: each runs repository-chosen code or exposes host paths outside
// the operator's review.
func checkSpec(ls LoadedSpec, parent *LoadedSpec) error {
	for _, name := range sortedKeys(ls.Spec.Env) {
		kind, _, err := ParseSource(ls.Spec.Env[name])
		if err != nil {
			return fmt.Errorf("spec %q: env.%s: %w", ls.Ref, name, err)
		}
		if kind != "literal" {
			article := "a"
			if kind == "env" {
				article = "an"
			}
			return fmt.Errorf("spec %q: env.%s: specs may only set literal env values, not %s %s source", ls.Ref, name, article, kind)
		}
	}
	if ls.Trust == TrustRepository {
		if len(ls.Spec.Egress) > 0 {
			return fmt.Errorf("spec %q: [egress]: repository-trust specs may not define egress sets", ls.Ref)
		}
		if ls.Spec.Claude.ManagedSettings != "" {
			return fmt.Errorf("spec %q: claude.managed_settings: repository-trust specs may not set managed settings", ls.Ref)
		}
		if parent == nil || parent.Trust != TrustOperator {
			return fmt.Errorf("spec %q: repository-trust specs must extend an operator-trust spec", ls.Ref)
		}
	}
	return nil
}

// Layer is one image layer from a spec, with absolute paths.
type Layer struct{ Spec, Dockerfile, Context string }

// Setup is the merged setup configuration.
type Setup struct {
	Command  []string
	CacheKey string
	CacheDir string
}

// Merged is the combined contents of a spec chain.
type Merged struct {
	Layers          []Layer
	Egress          map[string][]string
	Env             map[string]string
	ContextRO       []string
	Setup           Setup
	ManagedSettings []byte
}

// Merge combines a chain root first. A child adds contents and never
// redefines a parent's egress set, env name, or setup field.
func Merge(src SpecSource, chain []LoadedSpec) (*Merged, error) {
	m := &Merged{Egress: map[string][]string{}, Env: map[string]string{}}
	var settings map[string]any
	for _, ls := range chain {
		s := ls.Spec
		if s.Image.Dockerfile != "" {
			df, err := src.Path(s.Image.Dockerfile)
			if err != nil {
				return nil, fmt.Errorf("spec %q: image.dockerfile: %w", ls.Ref, err)
			}
			ctx := filepath.Dir(df)
			if s.Image.Context != "" {
				if ctx, err = src.Path(s.Image.Context); err != nil {
					return nil, fmt.Errorf("spec %q: image.context: %w", ls.Ref, err)
				}
			}
			m.Layers = append(m.Layers, Layer{Spec: ls.Ref, Dockerfile: df, Context: ctx})
		}
		for _, name := range sortedKeys(s.Egress) {
			if _, dup := m.Egress[name]; dup {
				return nil, fmt.Errorf("spec %q: egress.%s: already defined by a parent spec", ls.Ref, name)
			}
			for _, host := range s.Egress[name] {
				if err := egresshost.Validate(host); err != nil {
					return nil, fmt.Errorf("spec %q: egress.%s: %w", ls.Ref, name, err)
				}
			}
			m.Egress[name] = s.Egress[name]
		}
		for _, name := range sortedKeys(s.Env) {
			if _, dup := m.Env[name]; dup {
				return nil, fmt.Errorf("spec %q: env.%s: already defined by a parent spec", ls.Ref, name)
			}
			m.Env[name] = s.Env[name]
		}
		m.ContextRO = append(m.ContextRO, s.Context.Readonly...)
		if err := mergeSetup(&m.Setup, s.Setup, ls.Ref); err != nil {
			return nil, err
		}
		if s.Claude.ManagedSettings != "" {
			data, err := src.ReadFile(s.Claude.ManagedSettings)
			if err != nil {
				return nil, fmt.Errorf("spec %q: claude.managed_settings: %w", ls.Ref, err)
			}
			child, err := decodeObject(data)
			if err != nil {
				return nil, fmt.Errorf("spec %q: claude.managed_settings %s: %w", ls.Ref, s.Claude.ManagedSettings, err)
			}
			if settings == nil {
				settings = map[string]any{}
			}
			if err := mergeObjects(settings, child, ""); err != nil {
				return nil, fmt.Errorf("spec %q: claude.managed_settings %s: %w", ls.Ref, s.Claude.ManagedSettings, err)
			}
		}
	}
	if settings != nil {
		data, err := encodeJSON(settings)
		if err != nil {
			return nil, err
		}
		m.ManagedSettings = data
	}
	return m, nil
}

func mergeSetup(dst *Setup, s config.SpecSetup, ref string) error {
	if len(s.Command) > 0 {
		if len(dst.Command) > 0 {
			return fmt.Errorf("spec %q: setup.command: already defined by a parent spec", ref)
		}
		dst.Command = s.Command
	}
	if s.CacheKey != "" {
		if dst.CacheKey != "" {
			return fmt.Errorf("spec %q: setup.cache_key: already defined by a parent spec", ref)
		}
		dst.CacheKey = s.CacheKey
	}
	if s.CacheDir != "" {
		if dst.CacheDir != "" {
			return fmt.Errorf("spec %q: setup.cache_dir: already defined by a parent spec", ref)
		}
		dst.CacheDir = s.CacheDir
	}
	return nil
}

// SpecExists reports whether ref names a spec. A spec that is missing is
// not found; one that exists but fails to load or validate is an error.
func SpecExists(src SpecSource, ref string) (bool, error) {
	_, _, err := src.LoadSpec(ref)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("spec %q: %w", ref, err)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
