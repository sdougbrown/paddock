package resolve

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
)

// Phases a command runs in.
const (
	PhaseRun   = "run"
	PhaseSetup = "setup"
)

// Options selects what to resolve.
type Options struct {
	Profile   string
	Spec      string // explicit spec ref; empty selects automatically
	Workspace string // empty means the current directory
	Phase     string // empty means PhaseRun
	Argv      []string
}

// Resolver builds Plans from a config root.
type Resolver struct {
	Root   config.Root
	Source SpecSource
	Home   string
	Cwd    func() (string, error)
	Origin func(dir string) (string, error)
}

// New returns a Resolver reading operator specs from root.
func New(root config.Root, home string) *Resolver {
	return &Resolver{Root: root, Source: RootSource{Root: root}, Home: home, Cwd: os.Getwd, Origin: GitOrigin}
}

// Resolve produces the Plan for one command.
func (r *Resolver) Resolve(o Options) (*plan.Plan, error) {
	phase := o.Phase
	if phase == "" {
		phase = PhaseRun
	}
	if phase != PhaseRun && phase != PhaseSetup {
		return nil, fmt.Errorf("phase %q must be %q or %q", phase, PhaseRun, PhaseSetup)
	}
	prof, err := r.Root.LoadProfile(o.Profile)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", o.Profile, err)
	}
	if phase == PhaseSetup && prof.Workspace.Mode != "rw" {
		return nil, fmt.Errorf("phase setup: setup needs a read-write workspace; profile %q workspace.mode is %s", o.Profile, modeName(prof.Workspace.Mode))
	}
	ws, err := r.workspace(o.Workspace)
	if err != nil {
		return nil, err
	}
	ref, err := r.selectSpec(o, prof, ws)
	if err != nil {
		return nil, err
	}
	chain, err := LoadChain(r.Source, ref)
	if err != nil {
		return nil, err
	}
	merged, err := Merge(r.Source, chain)
	if err != nil {
		return nil, err
	}

	p := &plan.Plan{Profile: o.Profile, Spec: ref, Engine: prof.Engine, Phase: phase, Workspace: ws, Workdir: r.workdir(ws)}
	p.SpecChain = []string{}
	for _, ls := range chain {
		p.SpecChain = append(p.SpecChain, ls.Ref)
	}
	p.Image.Layers = []plan.ImageLayer{}
	for _, l := range merged.Layers {
		p.Image.Layers = append(p.Image.Layers, plan.ImageLayer{Spec: l.Spec, Dockerfile: l.Dockerfile, Context: l.Context})
	}
	if p.Mounts, err = r.mounts(prof, merged, ws, phase); err != nil {
		return nil, err
	}
	if p.Env, err = r.env(prof, merged, phase); err != nil {
		return nil, err
	}
	if p.Network, err = network(prof, merged, phase); err != nil {
		return nil, err
	}
	p.Limits = plan.Limits{CPUs: prof.Limits.CPUs, Memory: prof.Limits.Memory}
	p.Claude = plan.ClaudePlan{SeedFiles: []string{}}
	if phase == PhaseRun {
		if p.Claude, err = r.claude(prof, merged, p.Mounts); err != nil {
			return nil, err
		}
	}
	if len(merged.Setup.Command) > 0 {
		if p.Setup, err = setupPlan(ref, merged.Setup); err != nil {
			return nil, err
		}
	}
	switch phase {
	case PhaseSetup:
		if p.Setup == nil {
			return nil, fmt.Errorf("phase setup: spec %q defines no [setup] command", ref)
		}
		if len(o.Argv) > 0 {
			return nil, errors.New("phase setup: the setup command comes from the spec; do not pass a command")
		}
		if err := r.setupCache(p); err != nil {
			return nil, err
		}
		p.Argv = append([]string{}, p.Setup.Command...)
	default:
		p.Argv = append([]string{}, o.Argv...)
	}
	return p, nil
}

// workdir returns the process's symlink-resolved current directory when it is
// the workspace or below it, else the workspace.
func (r *Resolver) workdir(ws string) string {
	cwd, err := r.Cwd()
	if err != nil {
		return ws
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return ws
	}
	rel, err := filepath.Rel(ws, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ws
	}
	return real
}

func (r *Resolver) workspace(dir string) (string, error) {
	if dir == "" {
		cwd, err := r.Cwd()
		if err != nil {
			return "", fmt.Errorf("workspace: %w", err)
		}
		dir = cwd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("workspace %q: %w", dir, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("workspace %q: %w", dir, err)
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return "", fmt.Errorf("workspace %q is not a directory", dir)
	}
	return real, nil
}

// selectSpec picks the spec: --spec, else the origin remote's
// <owner>/<repo> if such a spec exists, else the profile default.
func (r *Resolver) selectSpec(o Options, prof *config.Profile, ws string) (string, error) {
	if o.Spec != "" {
		return o.Spec, nil
	}
	tried := []string{"no --spec"}
	url, err := r.Origin(ws)
	switch {
	case err != nil:
		tried = append(tried, "no git origin in "+ws)
	default:
		ref, ok := ParseOrigin(url)
		if !ok {
			tried = append(tried, fmt.Sprintf("origin %q is not owner/repo", redactOrigin(url)))
			break
		}
		found, err := SpecExists(r.Source, ref)
		if err != nil {
			return "", err
		}
		if found {
			return ref, nil
		}
		tried = append(tried, fmt.Sprintf("no spec %q for origin %q", ref, redactOrigin(url)))
	}
	if prof.DefaultSpec != "" {
		return prof.DefaultSpec, nil
	}
	tried = append(tried, fmt.Sprintf("profile %q has no default_spec", o.Profile))
	return "", fmt.Errorf("no spec selected: %s", strings.Join(tried, "; "))
}

// modeName renders a workspace mode for error messages; an unset mode is ro.
func modeName(m string) string {
	if m == "" {
		return "ro"
	}
	return m
}

// mounts lists the plan's bind mounts. The workspace mount follows the
// profile's mode in both phases; its readonly subpaths stay read-only.
func (r *Resolver) mounts(prof *config.Profile, m *Merged, ws, phase string) ([]plan.Mount, error) {
	mounts := []plan.Mount{{Host: ws, Guest: ws, ReadOnly: prof.Workspace.Mode != "rw"}}
	for _, sub := range prof.Workspace.Readonly {
		clean := filepath.Clean(sub)
		if filepath.IsAbs(sub) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("workspace.readonly: %q must be a relative path inside the workspace", sub)
		}
		p, err := readonlySubpath(ws, clean)
		if err != nil {
			return nil, fmt.Errorf("workspace.readonly: %q: %w", sub, err)
		}
		mounts = append(mounts, plan.Mount{Host: p, Guest: p, ReadOnly: true})
	}
	add := func(key string, paths []string) error {
		for _, raw := range paths {
			p, err := ExpandHome(raw, r.Home)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			if strings.Contains(p, ":") {
				return fmt.Errorf("%s: %q must not contain ':'", key, p)
			}
			if containsPath(p, ws) {
				return fmt.Errorf("%s: %q equals or contains the workspace %s", key, p, ws)
			}
			if initcfg.Reserved(p) {
				return fmt.Errorf("%s: %q is a paddock-managed guest path", key, p)
			}
			if !hasMount(mounts, p) {
				mounts = append(mounts, plan.Mount{Host: p, Guest: p, ReadOnly: true})
			}
		}
		return nil
	}
	if err := add("mounts.readonly", prof.Mounts.Readonly); err != nil {
		return nil, err
	}
	if err := add("context.readonly", m.ContextRO); err != nil {
		return nil, err
	}
	return mounts, nil
}

// containsPath reports whether dir equals or is an ancestor of target. A dir
// that exists is compared through its symlink-resolved form.
func containsPath(dir, target string) bool {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	rel, err := filepath.Rel(dir, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// readonlySubpath resolves ws/rel through symlinks and requires an existing
// directory that still lies inside ws, so a symlink cannot redirect the
// read-only mount outside the workspace.
func readonlySubpath(ws, rel string) (string, error) {
	p, err := filepath.EvalSymlinks(filepath.Join(ws, rel))
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return "", errors.New("is not a directory")
	}
	back, err := filepath.Rel(ws, p)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", errors.New("resolves outside the workspace")
	}
	return p, nil
}

func hasMount(ms []plan.Mount, p string) bool {
	for _, m := range ms {
		if m.Guest == p {
			return true
		}
	}
	return false
}

// env lists the plan's environment. The setup phase runs dependency installs,
// so it receives literals only: file and env sources are credentials.
func (r *Resolver) env(prof *config.Profile, m *Merged, phase string) ([]plan.EnvVar, error) {
	var out []plan.EnvVar
	for _, name := range sortedKeys(prof.Env) {
		kind, ref, err := ParseSource(prof.Env[name])
		if err != nil {
			return nil, fmt.Errorf("env.%s: %w", name, err)
		}
		if phase == PhaseSetup && kind != plan.SourceLiteral {
			continue
		}
		v := plan.EnvVar{Name: name, Source: kind}
		switch kind {
		case plan.SourceLiteral:
			v.Value = ref
		case plan.SourceFile:
			if v.Ref, err = ExpandHome(ref, r.Home); err != nil {
				return nil, fmt.Errorf("env.%s: %w", name, err)
			}
		default:
			v.Ref = ref
		}
		out = append(out, v)
	}
	for _, name := range sortedKeys(m.Env) {
		if _, dup := prof.Env[name]; dup {
			return nil, fmt.Errorf("env.%s: set by both the profile and the spec chain", name)
		}
		out = append(out, plan.EnvVar{Name: name, Source: plan.SourceLiteral, Value: m.Env[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if out == nil {
		out = []plan.EnvVar{}
	}
	return out, nil
}

func network(prof *config.Profile, m *Merged, phase string) (plan.Network, error) {
	policy := prof.Network.Policy
	if policy == "" {
		policy = initcfg.PolicyNone
	}
	n := plan.Network{Policy: policy, Sets: []string{}, Hosts: []string{}}
	if policy != initcfg.PolicyAllowlist {
		return n, nil
	}
	names, key := prof.Network.Run, "network.run"
	if phase == PhaseSetup {
		names, key = prof.Network.Setup, "network.setup"
	}
	if len(names) == 0 {
		return n, fmt.Errorf("%s: allowlist policy needs at least one egress set", key)
	}
	hosts := map[string]bool{}
	for _, name := range names {
		set, ok := m.Egress[name]
		if !ok {
			return n, fmt.Errorf("%s: unknown egress set %q (defined: %s)", key, name, strings.Join(sortedKeys(m.Egress), ", "))
		}
		n.Sets = append(n.Sets, name)
		for _, h := range set {
			hosts[h] = true
		}
	}
	if len(hosts) == 0 {
		return n, fmt.Errorf("%s: egress sets %s define no hosts", key, strings.Join(names, ", "))
	}
	n.Hosts = sortedKeys(hosts)
	return n, nil
}

func (r *Resolver) claude(prof *config.Profile, m *Merged, mounts []plan.Mount) (plan.ClaudePlan, error) {
	c := plan.ClaudePlan{SeedFiles: []string{}}
	if m.ManagedSettings != nil {
		c.ManagedSettings = json.RawMessage(m.ManagedSettings)
	}
	for _, f := range prof.Claude.SeedFiles {
		p, err := ExpandHome(f, r.Home)
		if err != nil {
			return c, fmt.Errorf("claude.seed_files: %w", err)
		}
		c.SeedFiles = append(c.SeedFiles, p)
	}
	for _, t := range []struct {
		key, raw string
		dst      *string
	}{{"claude.skills", prof.Claude.Skills, &c.Skills}, {"claude.agents", prof.Claude.Agents, &c.Agents}} {
		if t.raw == "" {
			continue
		}
		p, err := ExpandHome(t.raw, r.Home)
		if err != nil {
			return c, fmt.Errorf("%s: %w", t.key, err)
		}
		if !underReadonlyMount(mounts, p) {
			return c, fmt.Errorf("%s: %s is not under a read-only mount", t.key, p)
		}
		*t.dst = p
	}
	return c, nil
}

// underReadonlyMount reports whether the deepest mount holding p is
// read-only, both for p as written and for p with its symlinks resolved, so a
// link inside a read-only mount cannot lead into a writable one. Mounts are
// same-path, so resolving on the host matches what the guest sees.
func underReadonlyMount(mounts []plan.Mount, p string) bool {
	if !deepestMountReadonly(mounts, p, func(s string) string { return s }) {
		return false
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return true
	}
	resolve := func(s string) string {
		if r, err := filepath.EvalSymlinks(s); err == nil {
			return r
		}
		return s
	}
	return deepestMountReadonly(mounts, real, resolve)
}

// deepestMountReadonly reports whether the longest mount path holding p, after
// passing each mount's host path through form, is read-only.
func deepestMountReadonly(mounts []plan.Mount, p string, form func(string) string) bool {
	best, ro := -1, false
	for _, m := range mounts {
		g := form(m.Host)
		if (p == g || strings.HasPrefix(p, g+string(filepath.Separator))) && len(g) > best {
			best, ro = len(g), m.ReadOnly
		}
	}
	return ro
}

// guestCacheRoot is the guest directory every setup.cache_dir must lie under,
// so a spec cannot place its read-write cache mount over other guest paths.
const guestCacheRoot = "/var/cache/paddock/"

func setupPlan(ref string, s Setup) (*plan.SetupPlan, error) {
	sp := &plan.SetupPlan{Command: s.Command, CacheKeyFile: s.CacheKey, CacheDir: s.CacheDir}
	if d := s.CacheDir; d != "" && (!path.IsAbs(d) || path.Clean(d) != d || !strings.HasPrefix(d, guestCacheRoot) || d == guestCacheRoot) {
		return nil, fmt.Errorf("spec %q: setup.cache_dir: %q must be a clean absolute path strictly under %s", ref, d, guestCacheRoot)
	}
	if s.CacheKey != "" {
		clean := filepath.Clean(s.CacheKey)
		if filepath.IsAbs(s.CacheKey) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("spec %q: setup.cache_key: %q must be a relative path inside the workspace", ref, s.CacheKey)
		}
	}
	return sp, nil
}
