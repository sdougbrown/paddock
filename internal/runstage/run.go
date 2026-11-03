package runstage

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"

	"github.com/sdougbrown/paddock/internal/initcfg"
)

// IsTTY reports whether f is a terminal; /dev/null is a character device but not one.
func IsTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(st, null)
}

func HostUser() (initcfg.User, error) {
	u, err := user.Current()
	if err != nil {
		return initcfg.User{}, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return initcfg.User{}, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return initcfg.User{}, err
	}
	return initcfg.User{Name: u.Username, UID: uid, GID: gid, Home: u.HomeDir}, nil
}

// PassthroughEnv names the invoking environment's variables that reach the
// guest. Run passes the set ones to paddock-init both in the init config,
// where the plan's env overrides them, and with the run's -e flags.
var PassthroughEnv = []string{"TERM", "COLORTERM", "LANG"}

// SignalQueue holds the signals that could not be delivered yet, in arrival
// order. A signal equal to the last one queued is not queued again.
type SignalQueue []os.Signal

func (q *SignalQueue) Push(s os.Signal) {
	if n := len(*q); n > 0 && (*q)[n-1] == s {
		return
	}
	*q = append(*q, s)
}

// Deliver sends queued signals in order with send, stopping at the first one
// send reports undelivered; that one and the rest stay queued.
func (q *SignalQueue) Deliver(send func(os.Signal) bool) {
	for len(*q) > 0 && send((*q)[0]) {
		*q = (*q)[1:]
	}
}

// ExitCode turns a child's wait error into the exit code the CLI reports:
// the child's own code, 128+signal when it died to a signal, or 1 with the
// wait error when the wait itself failed. The engines share it so signal
// deaths report identically everywhere.
func ExitCode(waitErr error) (int, error) {
	code := 0
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		code = exitErr.ExitCode()
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
	default:
		return 1, waitErr
	}
	return code, nil
}
