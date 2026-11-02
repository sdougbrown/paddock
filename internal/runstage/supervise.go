package runstage

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// signalNames maps the signals Run forwards to the guest to their CLI names.
var signalNames = map[os.Signal]string{
	syscall.SIGTERM: "TERM",
	syscall.SIGINT:  "INT",
	syscall.SIGHUP:  "HUP",
}

// PendingRetry is how often an undelivered signal or stop is retried.
const PendingRetry = 200 * time.Millisecond

// Supervisor drives one guest run's supervision against a container
// runtime's CLI, parameterized by the three operations that differ per
// engine: delivering a signal, stopping the container, and reporting
// whether it runs. Everything else — the signal queue, the retry cadence,
// the cancellation escalation, and the secrets cleanup — is shared.
type Supervisor struct {
	// Send delivers sig to the guest named name and reports whether it was
	// delivered. A signal that arrives while the guest is still booting can
	// be acknowledged and dropped by the runtime, so Send counts a delivery
	// only when the guest was observed running immediately before.
	Send func(name string, sig os.Signal) bool
	// Stop asks the guest named name to stop, killing it after grace
	// seconds. Like a kill after cancellation, it runs without the run's
	// context.
	Stop func(name string, grace int) error
	// IsRunning reports whether name is running. An unreachable runtime
	// reports not running.
	IsRunning func(name string) bool
	// SecretsStartWait is how long the host waits for the guest to run
	// before removing secrets.env anyway.
	SecretsStartWait time.Duration
	// SecretsGrace is how long a running guest may keep secrets.env before
	// the host removes it.
	SecretsGrace time.Duration
	// CancelGrace is how long after ctx is cancelled the host waits before
	// Stop follows the queued SIGTERM.
	CancelGrace time.Duration
	// StopGrace is the stop grace, in seconds, for a cancellation.
	StopGrace int
	// Debugf is optional; a non-nil one receives progress notes.
	Debugf func(format string, args ...any)

	queued func(os.Signal)
}

// SignalQueued registers a callback fired after a signal is queued and one
// delivery attempt has been made. Tests use it to observe the queue.
func (s *Supervisor) SignalQueued(f func(os.Signal)) { s.queued = f }

func (s *Supervisor) debugf(format string, a ...any) {
	if s.Debugf != nil {
		s.Debugf(format, a...)
	}
}

// ForwardSignals delivers signals received by paddock to the guest process,
// because the runtime client does not forward them. A signal that cannot yet
// be delivered, such as one that arrives while the guest is booting, is
// queued, and the queue is retried in arrival order every pendingRetry until
// it is empty or done closes. When ctx is cancelled the guest gets SIGTERM,
// and Stop with StopGrace follows after CancelGrace.
func (s *Supervisor) ForwardSignals(ctx context.Context, name string, sigs <-chan os.Signal, done <-chan struct{}) {
	var pending SignalQueue
	send := func(sig os.Signal) bool { return s.Send(name, sig) }
	queue := func(sig os.Signal) {
		pending.Push(sig)
		pending.Deliver(send)
		if s.queued != nil {
			s.queued(sig)
		}
	}
	var stopAt <-chan time.Time
	stopDue, stopped := false, false
	cancelled := ctx.Done()
	retry := time.NewTicker(PendingRetry)
	defer retry.Stop()
	for {
		select {
		case sig := <-sigs:
			if _, ok := signalNames[sig]; ok {
				queue(sig)
			}
		case <-cancelled:
			cancelled = nil
			queue(syscall.SIGTERM)
			stopAt = time.After(s.CancelGrace)
		case <-stopAt:
			stopAt, stopDue = nil, true
		case <-retry.C:
			pending.Deliver(send)
		case <-done:
			return
		}
		if stopDue && !stopped {
			s.debugf("stopping %s after cancellation", name)
			stopped = s.Stop(name, s.StopGrace) == nil
		}
	}
}

// SignalName returns the CLI signal name for sig, and whether sig is one
// ForwardSignals forwards.
func SignalName(sig os.Signal) (string, bool) {
	n, ok := signalNames[sig]
	return n, ok
}

// RemoveSecretsOnceRunning polls until the guest is running, then waits up
// to SecretsGrace for paddock-init to unlink the secrets file at path's
// directory before removing it. If the guest is not running within
// SecretsStartWait, it removes the file anyway, so a slow or failed boot
// cannot leave it on the host for the whole run. When done closes it returns
// at once; the caller then removes the secrets dir.
func (s *Supervisor) RemoveSecretsOnceRunning(name, dir string, done <-chan struct{}) {
	file := filepath.Join(dir, SecretsFile)
	startBy := time.Now().Add(s.SecretsStartWait)
	for !s.IsRunning(name) {
		if time.Now().After(startBy) {
			os.Remove(file)
			return
		}
		select {
		case <-done:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	grace := time.Now().Add(s.SecretsGrace)
	for time.Now().Before(grace) {
		if _, err := os.Stat(file); err != nil {
			return
		}
		select {
		case <-done:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	os.Remove(file)
}
