package applecontainer

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
)

// guestMount is one bind mount into the guest. The run's -v flags and the
// init config's verify list are both derived from the same []guestMount.
type guestMount struct {
	Host, Guest string
	ReadOnly    bool
}

// mountLayout holds the paddock-managed host paths that join the plan's mounts.
type mountLayout struct {
	Dirs           runDirs
	BinDir         string // host dir mounted at initcfg.BinDir; empty for none
	TranscriptDir  string // same path on host and in the guest
	UserSettings   string // host file mounted read-only at UserSettingsAt; empty for none
	UserSettingsAt string
}

// guestMounts lists the plan's mounts followed by the paddock-managed ones.
// Each plan mount must pass plan.Mount.Check, the rule resolve applies, and
// must not target the engine's own transcript or user-settings paths. A
// managed mount whose host path contains ':' is an error, since
// `-v host:guest[:ro]` cannot express it.
func guestMounts(p *plan.Plan, l mountLayout) ([]guestMount, error) {
	engineOwned := []string{l.TranscriptDir}
	if l.UserSettings != "" {
		engineOwned = append(engineOwned, l.UserSettingsAt)
	}
	var ms []guestMount
	for _, m := range p.Mounts {
		if err := m.Check(); err != nil {
			return nil, fmt.Errorf("plan mount %s: %w", m.Host, err)
		}
		if slices.ContainsFunc(engineOwned, func(d string) bool { return atOrUnder(m.Guest, d) }) {
			return nil, fmt.Errorf("plan mount %q targets paddock-managed guest path %s", m.Host, m.Guest)
		}
		ms = append(ms, guestMount{m.Host, m.Guest, m.ReadOnly})
	}
	managed := []guestMount{
		{l.Dirs.State, initcfg.StateDir, true},
		{l.Dirs.Secrets, initcfg.SecretsDir, false},
		{l.Dirs.Log, initcfg.LogDir, false},
	}
	if l.BinDir != "" {
		managed = append(managed, guestMount{l.BinDir, initcfg.BinDir, true})
	}
	managed = append(managed, guestMount{l.TranscriptDir, l.TranscriptDir, false})
	if l.UserSettings != "" {
		managed = append(managed, guestMount{l.UserSettings, l.UserSettingsAt, true})
	}
	for _, m := range managed {
		for _, s := range []string{m.Host, m.Guest} {
			if strings.Contains(s, ":") {
				return nil, fmt.Errorf("mount path %q contains ':'", s)
			}
		}
	}
	return append(ms, managed...), nil
}

// atOrUnder reports whether guest path p is dir or lies under it.
func atOrUnder(p, dir string) bool {
	if dir == "" {
		return false
	}
	p, dir = path.Clean(p), path.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// runSpec is everything `container run` needs. It holds no secret values.
type runSpec struct {
	Name       string
	TTY        bool
	CPUs       int
	Memory     string
	Mounts     []guestMount
	Workdir    string   // guest working directory
	Env        []string // NAME=VALUE passed with -e
	Entrypoint string
	Image      string
	Args       []string // arguments after the image
}

// runArgv builds the argument vector for `container run`.
func runArgv(s runSpec) []string {
	a := []string{"run", "--rm", "-i"}
	if s.TTY {
		a = append(a, "-t")
	}
	a = append(a, "--init", "--name", s.Name,
		"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW")
	if s.CPUs > 0 {
		a = append(a, "-c", strconv.Itoa(s.CPUs))
	}
	if s.Memory != "" {
		a = append(a, "-m", s.Memory)
	}
	a = append(a, "--progress", "none")
	for _, m := range s.Mounts {
		a = append(a, "-v", volume(m.Host, m.Guest, m.ReadOnly))
	}
	a = append(a, "-w", s.Workdir)
	for _, e := range s.Env {
		a = append(a, "-e", e)
	}
	a = append(a, "--entrypoint", s.Entrypoint, s.Image)
	return append(a, s.Args...)
}

func volume(host, guest string, ro bool) string {
	v := host + ":" + guest
	if ro {
		v += ":ro"
	}
	return v
}
