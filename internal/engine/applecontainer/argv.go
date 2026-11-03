package applecontainer

import (
	"strconv"

	"github.com/sdougbrown/paddock/internal/runstage"
)

// runSpec is everything `container run` needs. It holds no secret values.
type runSpec struct {
	Name       string
	TTY        bool
	CPUs       int
	Memory     string
	Mounts     []runstage.GuestMount
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
		a = append(a, "-v", runstage.Volume(m.Host, m.Guest, m.ReadOnly))
	}
	a = append(a, "-w", s.Workdir)
	for _, e := range s.Env {
		a = append(a, "-e", e)
	}
	a = append(a, "--entrypoint", s.Entrypoint, s.Image)
	return append(a, s.Args...)
}
