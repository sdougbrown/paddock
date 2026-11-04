//go:build !linux

package bwrap

import "testing"

// TestRunKillsBwrapOnParentDeath documents the off-Linux degradation: there
// is no parent-death signal, so the run relies on --die-with-parent alone.
func TestRunKillsBwrapOnParentDeath(t *testing.T) {
	if attr := parentDeathAttr(); attr != nil {
		t.Errorf("off Linux the parent-death attr must be nil, got %+v", attr)
	}
}
