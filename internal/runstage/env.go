package runstage

import (
	"fmt"
	"strings"

	"github.com/sdougbrown/paddock/internal/envname"
	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
)

// DefaultPath is the agent's PATH unless the inherited environment or the
// plan's env sets one.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// AgentShell is the agent's SHELL.
const AgentShell = "/bin/bash"

// AgentEnv is the agent's environment. It starts from inherited, then sets
// PATH (DefaultPath only when inherited has none), HOME, USER, LOGNAME, and
// SHELL, then adds extra (the plan's literal env and passthrough variables)
// and secrets. A later entry replaces an earlier one with the same name, in
// the earlier one's position. An invalid extra or secret entry is quoted in
// the error, so secrets must be validated before this call.
func AgentEnv(inherited []string, u initcfg.User, extra, secrets []string) ([]string, error) {
	var env []string
	index := map[string]int{}
	set := func(kv string) {
		name, _, _ := strings.Cut(kv, "=")
		if i, ok := index[name]; ok {
			env[i] = kv
			return
		}
		index[name] = len(env)
		env = append(env, kv)
	}
	for _, kv := range inherited {
		set(kv)
	}
	if _, ok := index["PATH"]; !ok {
		set("PATH=" + DefaultPath)
	}
	set("HOME=" + u.Home)
	set("USER=" + u.Name)
	set("LOGNAME=" + u.Name)
	set("SHELL=" + AgentShell)
	for _, kv := range append(append([]string(nil), extra...), secrets...) {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("an env entry has no NAME=VALUE separator")
		}
		if !envname.Valid(name) {
			return nil, fmt.Errorf("env name %q is not a valid environment name", name)
		}
		set(kv)
	}
	return env, nil
}

// CheckMountTargets rejects the plan's mounts that fail the mount rule or
// target a path the engine mounts itself: engineOwned lists the directory
// roots whose subtrees are engine-owned.
func CheckMountTargets(p *plan.Plan, engineOwned []string) error {
	for _, m := range p.Mounts {
		if err := m.Check(); err != nil {
			return fmt.Errorf("plan mount %s: %w", m.Host, err)
		}
		if AtOrUnder(m.Guest, engineOwned...) {
			return fmt.Errorf("plan mount %q targets a paddock-managed guest path", m.Guest)
		}
	}
	return nil
}
