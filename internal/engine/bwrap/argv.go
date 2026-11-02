package bwrap

import (
	"github.com/sdougbrown/paddock/internal/runstage"
)

// runSpec is everything the bwrap argv needs. It holds no secret values.
type runSpec struct {
	NetNone bool // unshare the network (policy none)
	Mounts  []runstage.GuestMount
	Workdir string // working directory, on the host and in the namespace
	Argv    []string
}

// runArgv builds the argument vector for bwrap. The host rootfs is bound
// read-only first, then writable binds override it in order, so the later
// read-only settings bind pins the file inside the writable home. bwrap
// takes separate host and guest path arguments, so paths containing ':' are
// fine here, unlike the container engines' volume syntax.
func (e *Engine) runArgv(s runSpec, home, settings string) []string {
	// Unprivileged bwrap sets no_new_privs itself, as it must for an
	// unprivileged user namespace; there is no flag for it.
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
	if settings != "" {
		// Bind the settings file read-only over the writable home, so the
		// agent cannot rewrite its own enforcement.
		a = append(a, "--ro-bind", settings, runstage.UserSettingsPath(home))
	}
	a = append(a, "--chdir", s.Workdir)
	return append(a, s.Argv...)
}
