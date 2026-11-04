package runstage

import (
	"fmt"
	"path"
	"strings"

	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
)

// GuestMount is one bind mount into the guest. The run's -v flags and the
// init config's verify list are both derived from the same []GuestMount.
type GuestMount struct {
	Host, Guest string
	ReadOnly    bool
}

// MountLayout holds the paddock-managed host paths that join the plan's mounts.
type MountLayout struct {
	Dirs           RunDirs
	BinDir         string // host dir mounted at initcfg.BinDir; empty for none
	TranscriptDir  string // same path on host and in the guest
	UserSettings   string // host file mounted read-only at UserSettingsAt; empty for none
	UserSettingsAt string
}

// GuestMounts lists the plan's mounts followed by the paddock-managed ones.
// Each plan mount must pass plan.Mount.Check, the rule resolve applies, and
// must not target the engine's own transcript or user-settings paths. A
// managed mount whose host path contains ':' is an error, since
// `-v host:guest[:ro]` cannot express it.
func GuestMounts(p *plan.Plan, l MountLayout) ([]GuestMount, error) {
	engineOwned := []string{l.TranscriptDir}
	if l.UserSettings != "" {
		engineOwned = append(engineOwned, l.UserSettingsAt)
	}
	if err := CheckMountTargets(p, engineOwned); err != nil {
		return nil, err
	}
	var ms []GuestMount
	for _, m := range p.Mounts {
		ms = append(ms, GuestMount{m.Host, m.Guest, m.ReadOnly})
	}
	managed := []GuestMount{
		{l.Dirs.State, initcfg.StateDir, true},
		{l.Dirs.Secrets, initcfg.SecretsDir, false},
		{l.Dirs.Log, initcfg.LogDir, false},
	}
	if l.BinDir != "" {
		managed = append(managed, GuestMount{l.BinDir, initcfg.BinDir, true})
	}
	managed = append(managed, GuestMount{l.TranscriptDir, l.TranscriptDir, false})
	if l.UserSettings != "" {
		managed = append(managed, GuestMount{l.UserSettings, l.UserSettingsAt, true})
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

// AtOrUnder reports whether guest path p is one of dirs or lies under one
// of them.
func AtOrUnder(p string, dirs ...string) bool {
	p, dirs = path.Clean(p), cleanDirs(dirs)
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

func cleanDirs(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, path.Clean(d))
	}
	return out
}

func Volume(host, guest string, ro bool) string {
	v := host + ":" + guest
	if ro {
		v += ":ro"
	}
	return v
}
