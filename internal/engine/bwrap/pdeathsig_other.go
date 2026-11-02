//go:build !linux

package bwrap

import "syscall"

// parentDeathAttr is a no-op off Linux: there is no parent-death signal to
// set, so an orphaned bwrap run there relies on --die-with-parent alone.
func parentDeathAttr() *syscall.SysProcAttr {
	return nil
}
