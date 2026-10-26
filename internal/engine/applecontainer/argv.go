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

// guestMounts lists the plan's mounts followed by the paddock-managed ones. A
// plan mount whose guest path is paddock-managed is an error, and so is any
// path containing ':', which `-v host:guest[:ro]` cannot express.
//
// These checks repeat resolve's mount rules (resolver.go, Resolver.mounts) at
// the point where the engine builds `container run` arguments, and also cover
// the workspace mount and the paddock-managed mounts, which resolve does not
// check. A new mount rule belongs in both places.
func guestMounts(p *plan.Plan, l mountLayout) ([]guestMount, error) {
	ms, err := listMounts(p, l)
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		for _, s := range []string{m.Host, m.Guest} {
			if strings.Contains(s, ":") {
				return nil, fmt.Errorf("mount path %q contains ':'", s)
			}
		}
	}
	return ms, nil
}

func listMounts(p *plan.Plan, l mountLayout) ([]guestMount, error) {
	managed := []string{l.TranscriptDir}
	if l.UserSettings != "" {
		managed = append(managed, l.UserSettingsAt)
	}
	var ms []guestMount
	for _, m := range p.Mounts {
		if initcfg.Reserved(m.Guest) || slices.ContainsFunc(managed, func(d string) bool { return atOrUnder(m.Guest, d) }) {
			return nil, fmt.Errorf("plan mount %q targets paddock-managed guest path %s", m.Host, m.Guest)
		}
		ms = append(ms, guestMount{m.Host, m.Guest, m.ReadOnly})
	}
	ms = append(ms,
		guestMount{l.Dirs.State, initcfg.StateDir, true},
		guestMount{l.Dirs.Secrets, initcfg.SecretsDir, false},
		guestMount{l.Dirs.Log, initcfg.LogDir, false},
	)
	if l.BinDir != "" {
		ms = append(ms, guestMount{l.BinDir, initcfg.BinDir, true})
	}
	ms = append(ms, guestMount{l.TranscriptDir, l.TranscriptDir, false})
	if l.UserSettings != "" {
		ms = append(ms, guestMount{l.UserSettings, l.UserSettingsAt, true})
	}
	return ms, nil
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
