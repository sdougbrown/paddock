package podman

import (
	"strconv"
	"time"

	"github.com/sdougbrown/paddock/internal/runstage"
)

// runSpec is everything `podman run` needs. It holds no secret values.
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

// runArgv builds the argument vector for `podman run`.
func (e *Engine) runArgv(s runSpec) []string {
	a := append(e.invocation(), "run", "--rm", "-i")
	if s.TTY {
		a = append(a, "-t")
	}
	a = append(a, "--init", "--name", s.Name,
		"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW")
	if s.CPUs > 0 {
		a = append(a, "--cpus", strconv.Itoa(s.CPUs))
	}
	if s.Memory != "" {
		a = append(a, "-m", s.Memory)
	}
	for _, m := range s.Mounts {
		a = append(a, "-v", runstage.Volume(m.Host, m.Guest, m.ReadOnly))
	}
	a = append(a, "-w", s.Workdir)
	for _, env := range s.Env {
		a = append(a, "-e", env)
	}
	a = append(a, "--entrypoint", s.Entrypoint, s.Image)
	return append(a, s.Args...)
}

// orphanStopGrace is the stop grace, in seconds, for a container whose
// paddock side is gone: the `podman run` client died (Run) or paddock
// itself died (the watchdog). Nothing is left to wait on the container, so
// cleanup is prompt. A cancellation uses Engine.stopGrace instead: it is the
// last step of an escalation that has already tried SIGTERM and waited
// cancelGrace, and the caller chooses how long it lasts.
const orphanStopGrace = 2

// stopArgs is the `podman stop` argv that stops name, killing it after
// grace seconds. Run and the watchdog both stop containers with it.
func stopArgs(name string, grace int) []string {
	return []string{"stop", "-t", strconv.Itoa(grace), name}
}

// stopTimeout bounds a `podman stop` whose grace is grace seconds:
// cliTimeout plus the grace.
func stopTimeout(grace int) time.Duration {
	return cliTimeout + time.Duration(grace)*time.Second
}
