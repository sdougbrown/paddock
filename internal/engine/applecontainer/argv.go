package applecontainer

import (
	"fmt"
	"strconv"

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
// plan mount whose guest path is paddock-managed is an error.
func guestMounts(p *plan.Plan, l mountLayout) ([]guestMount, error) {
	var ms []guestMount
	for _, m := range p.Mounts {
		if initcfg.Reserved(m.Guest) {
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
