package bwrap

import "syscall"

// parentDeathAttr kills bwrap when the paddock process dies: bwrap's
// --die-with-parent then kills the agent. Without it a killed or crashed
// host process orphans both.
func parentDeathAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
