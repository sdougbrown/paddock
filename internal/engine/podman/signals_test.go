package podman

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/runstage"
)

func eventually(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startFakeLifecycle returns a fake podman CLI whose inspect reports running
// only once the running marker file exists, logging kill and stop calls.
func startFakeLifecycle(t *testing.T) (bin string, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := "#!/bin/sh\nd=\"${0%/*}\"\ncase \"$1\" in\n" +
		" inspect) [ -e \"$d/running\" ] && echo true || echo false ;;\n" +
		" kill) echo \"$3\" >> \"$d/kills\" ;;\n" +
		" stop) printf '%s\\n' \"$*\" >> \"$d/stops\" ;;\n" +
		"esac\n"
	bin = filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(bin, "__warm").Run(); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func TestForwardSignalsDeliversQueuedSignalsInOrder(t *testing.T) {
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
			bin, dir := startFakeLifecycle(t)
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
			// Every signal and the cancellation's TERM are now queued, and the
			// delivery attempt right after each one has finished and failed:
			// the container is still booting. Only a retry can deliver them.
			if b, _ := os.ReadFile(filepath.Join(dir, "kills")); len(b) != 0 {
				t.Fatalf("delivered %q while the container was booting", b)
			}
			if err := os.WriteFile(filepath.Join(dir, "running"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			var got string
			eventually(t, 10*time.Second, "queued signals to be delivered", func() bool {
				b, _ := os.ReadFile(filepath.Join(dir, "kills"))
				got = strings.Join(strings.Fields(string(b)), " ")
				return got == tt.want
			})
			time.Sleep(3 * pendingRetry)
			b, _ := os.ReadFile(filepath.Join(dir, "kills"))
			if got := strings.Join(strings.Fields(string(b)), " "); got != tt.want {
				t.Errorf("delivered %q, want %q", got, tt.want)
			}
		})
	}
}

// TestForwardSignalsStopsAfterCancel runs forwardSignals to cancellation with
// a running container: the TERM is delivered, and after cancelGrace the
// engine stops the container with stopGrace.
func TestForwardSignalsStopsAfterCancel(t *testing.T) {
	bin, dir := startFakeLifecycle(t)
	if err := os.WriteFile(filepath.Join(dir, "running"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Bin: bin, cancelGrace: 200 * time.Millisecond, stopGrace: 3}
	ctx, cancel := context.WithCancel(t.Context())
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
	cancel()
	eventually(t, 10*time.Second, "TERM to be delivered", func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "kills"))
		return strings.Contains(string(b), "TERM")
	})
	eventually(t, 10*time.Second, "the container to be stopped", func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "stops"))
		return strings.Contains(string(b), "stop")
	})
	b, _ := os.ReadFile(filepath.Join(dir, "stops"))
	if !strings.Contains(string(b), "3") {
		t.Errorf("stop argv %q lacks the stopGrace", b)
	}
}

func TestRemoveSecretsOnceRunning(t *testing.T) {
	const grace = 400 * time.Millisecond
	tests := []struct {
		name      string
		running   bool
		startWait time.Duration
		removedBy time.Duration // earliest time secrets.env may go
	}{
		{"running from the start", true, 5 * time.Second, grace},
		{"never running", false, 500 * time.Millisecond, 500 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, dir := startFakeLifecycle(t)
			if tt.running {
				if err := os.WriteFile(filepath.Join(dir, "running"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			secrets := filepath.Join(dir, "secrets")
			if err := os.Mkdir(secrets, 0o700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(secrets, runstage.SecretsFile)
			if err := os.WriteFile(file, []byte("TOKEN=x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			e := &Engine{Bin: bin, secretsGrace: grace, secretsStartWait: tt.startWait}
			done := make(chan struct{})
			returned := make(chan struct{})
			start := time.Now()
			go func() {
				e.removeSecretsOnceRunning("paddock-test", secrets, done)
				close(returned)
			}()
			defer func() {
				close(done)
				<-returned
			}()
			deadline := start.Add(tt.removedBy)
			eventually(t, 10*time.Second, "secrets.env to be removed", func() bool {
				_, err := os.Stat(file)
				return os.IsNotExist(err)
			})
			if time.Now().Before(deadline) {
				t.Errorf("secrets.env removed before its grace ended (removed by %s)", deadline.Format(time.RFC3339Nano))
			}
		})
	}
}
