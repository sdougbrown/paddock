package bwrap

import (
	"syscall"
	"testing"
)

// TestRunKillsBwrapOnParentDeath pins the parent-death protection: when the
// paddock host process dies the kernel kills bwrap, whose --die-with-parent
// then kills the agent. Without the attr both survive as orphans.
func TestRunKillsBwrapOnParentDeath(t *testing.T) {
	attr := parentDeathAttr()
	if attr == nil {
		t.Fatal("the run's child process must carry a parent-death attr")
	}
	if attr.Pdeathsig != syscall.SIGKILL {
		t.Errorf("Pdeathsig = %v, want SIGKILL", attr.Pdeathsig)
	}
}
