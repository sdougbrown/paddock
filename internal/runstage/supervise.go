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
	ops SupervisorConfig

	queued func(os.Signal)
}

// SupervisorConfig carries the engine-specific operations and the knobs the
// shared supervision needs. NewSupervisor wires them, so the field set has a
// single owner.
type SupervisorConfig struct {
	Send             func(name string, sig os.Signal) bool
	Stop             func(name string, grace int) error
	IsRunning        func(name string) bool
	SecretsStartWait time.Duration
	SecretsGrace     time.Duration
	CancelGrace      time.Duration
	StopGrace        int
	Debugf           func(format string, args ...any)
	SignalQueued     func(os.Signal)
}

// NewSupervisor builds the shared supervision from the engine's operations.
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	return &Supervisor{ops: cfg}
}

func (s *Supervisor) debugf(format string, a ...any) {
	if s.ops.Debugf != nil {
		s.ops.Debugf(format, a...)
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
	send := func(sig os.Signal) bool { return s.ops.Send(name, sig) }
	queue := func(sig os.Signal) {
		pending.Push(sig)
		pending.Deliver(send)
		if s.ops.SignalQueued != nil {
			s.ops.SignalQueued(sig)
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
			stopAt = time.After(s.ops.CancelGrace)
		case <-stopAt:
			stopAt, stopDue = nil, true
		case <-retry.C:
			pending.Deliver(send)
		case <-done:
			return
		}
		if stopDue && !stopped {
			s.debugf("stopping %s after cancellation", name)
			stopped = s.ops.Stop(name, s.ops.StopGrace) == nil
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
	startBy := time.Now().Add(s.ops.SecretsStartWait)
	for !s.ops.IsRunning(name) {
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
	grace := time.Now().Add(s.ops.SecretsGrace)
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
