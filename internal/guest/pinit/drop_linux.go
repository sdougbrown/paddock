//go:build linux

package pinit

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/sdougbrown/paddock/internal/guest/verify"
)

// dropAndExec drops to uid/gid with no capabilities and no_new_privs, then
// execs path with argv in workdir and env. It returns only on failure. The
// caller must hold runtime.LockOSThread: the bounding set, capset, and
// no_new_privs apply to the calling thread, which is the one that execs.
func dropAndExec(uid, gid int, workdir, path string, argv, env []string) error {
	for c := 0; ; c++ {
		err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0)
		if errors.Is(err, unix.EINVAL) {
			break
		}
		if err != nil {
			return fmt.Errorf("drop capability %d from the bounding set: %w", c, err)
		}
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("clear ambient capabilities: %w", err)
	}
	// The syscall package applies these to every thread.
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capset: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	status, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		return err
	}
	if _, err := verify.CheckPrivileges(status, uid); err != nil {
		return fmt.Errorf("privilege drop did not hold: %w", err)
	}
	if err := os.Chdir(workdir); err != nil {
		return err
	}
	if err := syscall.Exec(path, argv, env); err != nil {
		return fmt.Errorf("exec %s: %w", path, err)
	}
	return nil
}
