package bwrap

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/sdougbrown/paddock/internal/runstage"
)

// pendingRetry is how often an undelivered signal is retried.
const pendingRetry = 200 * time.Millisecond

// forwardSignals delivers signals received by paddock to the bwrap child.
// bwrap execs the agent as its own child, and the child is in the same
// session, so delivery is a direct process signal — no queue of CLI kills
// like the container engines need. When ctx is cancelled the child gets
// SIGTERM, and SIGKILL follows after cancelGrace.
func (e *Engine) forwardSignals(ctx context.Context, cmd *exec.Cmd, sigs <-chan os.Signal, done <-chan struct{}) {
	var pending runstage.SignalQueue
	send := func(s os.Signal) bool { return cmd.Process.Signal(s) == nil }
	queue := func(s os.Signal) {
		pending.Push(s)
		pending.Deliver(send)
		if e.signalQueued != nil {
			e.signalQueued(s)
		}
	}
	var killAt <-chan time.Time
	killDue, killed := false, false
	cancelled := ctx.Done()
	retry := time.NewTicker(pendingRetry)
	defer retry.Stop()
	for {
		select {
		case s := <-sigs:
			if s == syscall.SIGTERM || s == syscall.SIGINT || s == syscall.SIGHUP {
				queue(s)
			}
		case <-cancelled:
			cancelled = nil
			queue(syscall.SIGTERM)
			killAt = time.After(e.cancelGrace)
		case <-killAt:
			killAt, killDue = nil, true
		case <-retry.C:
			pending.Deliver(send)
		case <-done:
			return
		}
		if killDue && !killed {
			e.debugf("killing the bwrap child after cancellation")
			killed = cmd.Process.Kill() == nil
		}
	}
}
