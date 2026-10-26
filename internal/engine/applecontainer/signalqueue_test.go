package applecontainer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
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

// TestSignalQueueKeepsLaterSignalsBehindAFailure queues INT, TERM, INT. The
// second INT is not a consecutive duplicate, so it stays. When TERM cannot be
// delivered, delivery stops there and keeps TERM and the second INT queued.
func TestSignalQueueKeepsLaterSignalsBehindAFailure(t *testing.T) {
	var q signalQueue
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGINT} {
		q.push(s)
	}
	if want := []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGINT}; !slices.Equal(q, want) {
		t.Fatalf("queue = %v, want %v", q, want)
	}
	var tried []os.Signal
	q.deliver(func(s os.Signal) bool {
		tried = append(tried, s)
		return s != syscall.SIGTERM
	})
	if want := []os.Signal{syscall.SIGINT, syscall.SIGTERM}; !slices.Equal(tried, want) {
		t.Errorf("tried %v, want %v", tried, want)
	}
	if want := []os.Signal{syscall.SIGTERM, syscall.SIGINT}; !slices.Equal(q, want) {
		t.Errorf("queue = %v, want %v", q, want)
	}
}

// TestForwardSignalsDeliversQueuedSignalsInOrder runs forwardSignals against
// a fake CLI whose container is booting, then running. Signals and the
// cancellation's TERM that arrive during boot reach the guest in arrival
// order once it runs, through the retry alone, with no new signal.
func TestForwardSignalsDeliversQueuedSignalsInOrder(t *testing.T) {
	const running = `[{"configuration":{"id":"paddock-test"},"status":{"state":"running"}}]`
	tests := []struct {
		name   string
		sigs   []os.Signal
		cancel bool
		want   string
	}{
		{"INT then TERM", []os.Signal{syscall.SIGINT, syscall.SIGTERM}, false, "INT TERM"},
		{"INT then cancel", []os.Signal{syscall.SIGINT}, true, "INT TERM"},
		{"TERM then cancel", []os.Signal{syscall.SIGTERM}, true, "TERM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			ls := filepath.Join(dir, "ls.json")
			if err := os.WriteFile(ls, []byte("[]"), 0o644); err != nil {
				t.Fatal(err)
			}
			kills := filepath.Join(dir, "kills")
			bin := filepath.Join(dir, "container")
			script := "#!/bin/sh\nd=\"${0%/*}\"\ncase \"$1\" in\n ls) cat \"$d/ls.json\" ;;\n kill) echo \"$3\" >> \"$d/kills\" ;;\nesac\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			queued := make(chan os.Signal, 8)
			e := &Engine{Bin: bin, cancelGrace: time.Hour, stopGrace: 1, signalQueued: func(s os.Signal) { queued <- s }}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sigs := make(chan os.Signal, 4)
			done := make(chan struct{})
			returned := make(chan struct{})
			go func() {
				e.forwardSignals(ctx, "paddock-test", sigs, done)
				close(returned)
			}()
			defer func() {
				close(done)
				<-returned
			}()
			for _, s := range tt.sigs {
				sigs <- s
			}
			// A select with both a signal and the cancellation ready picks
			// either, so cancel only once the loop has queued the signals.
			for range tt.sigs {
				<-queued
			}
			if tt.cancel {
				cancel()
				<-queued
			}
			// Every signal and the cancellation's TERM are now queued, and
			// none could be delivered: the container is still booting.
			if b, _ := os.ReadFile(kills); len(b) != 0 {
				t.Fatalf("delivered %q while the container was booting", b)
			}
			if err := os.WriteFile(ls+".tmp", []byte(running), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(ls+".tmp", ls); err != nil {
				t.Fatal(err)
			}
			var got string
			eventually(t, 10*time.Second, "queued signals to be delivered", func() bool {
				b, _ := os.ReadFile(kills)
				got = strings.Join(strings.Fields(string(b)), " ")
				return got == tt.want
			})
			time.Sleep(3 * pendingRetry)
			b, _ := os.ReadFile(kills)
			if got := strings.Join(strings.Fields(string(b)), " "); got != tt.want {
				t.Errorf("delivered %q, want %q", got, tt.want)
			}
		})
	}
}
