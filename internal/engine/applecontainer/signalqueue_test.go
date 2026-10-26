package applecontainer

import (
	"os"
	"slices"
	"syscall"
	"testing"
)

// TestSignalQueueDeliversInOrder queues INT then TERM (twice) while the
// container is booting. They go out as INT then TERM, once each, and a
// failed send leaves the rest queued for the next retry.
func TestSignalQueueDeliversInOrder(t *testing.T) {
	var q signalQueue
	var sent []os.Signal
	booting := func(os.Signal) bool { return false }
	running := func(s os.Signal) bool { sent = append(sent, s); return true }

	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGTERM} {
		q.push(s)
		q.deliver(booting)
	}
	if want := []os.Signal{syscall.SIGINT, syscall.SIGTERM}; !slices.Equal(q, want) {
		t.Fatalf("queue = %v, want %v", q, want)
	}

	first := true
	q.deliver(func(s os.Signal) bool {
		if !first {
			return false
		}
		first = false
		return running(s)
	})
	if want := []os.Signal{syscall.SIGTERM}; !slices.Equal(q, want) {
		t.Fatalf("after one delivery, queue = %v, want %v", q, want)
	}
	q.deliver(running)
	if len(q) != 0 {
		t.Errorf("queue = %v after delivery, want empty", q)
	}
	if want := []os.Signal{syscall.SIGINT, syscall.SIGTERM}; !slices.Equal(sent, want) {
		t.Errorf("sent %v, want %v", sent, want)
	}

	q.push(syscall.SIGHUP)
	q.deliver(running)
	q.push(syscall.SIGHUP)
	q.deliver(running)
	if want := []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGHUP}; !slices.Equal(sent, want) {
		t.Errorf("a repeated signal after delivery was dropped: sent %v, want %v", sent, want)
	}
}
