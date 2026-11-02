package bwrap

import (
	"time"

	"github.com/sdougbrown/paddock/internal/runstage"
)

// runSpec is everything the bwrap argv needs. It holds no secret values.
type runSpec struct {
	Name    string // unused in argv; kept for parity with the other engines
	NetNone bool   // unshare the network (policy none)
	Mounts  []runstage.GuestMount
	Workdir string // working directory, on the host and in the namespace
	Argv    []string
}

// runArgv builds the argument vector for bwrap. The host rootfs is bound
// read-only first, then writable binds override it in order. bwrap takes
// separate host and guest path arguments, so paths containing ':' are fine
// here, unlike the container engines' volume syntax.
func (e *Engine) runArgv(s runSpec, home string) []string {
	a := []string{e.bin(), "--die-with-parent"}
	if s.NetNone {
		a = append(a, "--unshare-net")
	}
	a = append(a, "--ro-bind", "/", "/", "--dev-bind", "/dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp")
	a = append(a, "--bind", home, home)
	for _, m := range s.Mounts {
		if m.ReadOnly {
			a = append(a, "--ro-bind", m.Host, m.Guest)
		} else {
			a = append(a, "--bind", m.Host, m.Guest)
		}
	}
	a = append(a, "--chdir", s.Workdir)
	return append(a, s.Argv...)
}

// signalGrace is how long a forwarded signal may stay queued while the
// child starts; bwrap starts in milliseconds, so this only bounds a
// pathological exec delay.
const signalGrace = time.Second
