package applecontainer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRemoveSecretsOnceRunning drives removeSecretsOnceRunning with a fake
// CLI whose `container ls` lists the container as running from the start,
// only after a delay, or never. secrets.env must survive until the matching
// wait ends (secretsGrace once running, secretsStartWait otherwise), then
// be removed.
func TestRemoveSecretsOnceRunning(t *testing.T) {
	const running = `[{"configuration":{"id":"paddock-test"},"status":{"state":"running"}}]`
	const grace = time.Second
	tests := []struct {
		name      string
		runningAt time.Duration // when ls starts listing it as running; <0 never
		startWait time.Duration
		removedBy time.Duration // earliest time secrets.env may go
	}{
		{"running from the start", 0, 5 * time.Second, grace},
		{"running after a delay", 600 * time.Millisecond, 5 * time.Second, 600*time.Millisecond + grace},
		{"never running", -1, 1500 * time.Millisecond, 1500 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			ls := filepath.Join(dir, "ls.json")
			setLs := func(s string) {
				if err := os.WriteFile(ls+".tmp", []byte(s), 0o644); err != nil {
					t.Error(err)
				}
				if err := os.Rename(ls+".tmp", ls); err != nil {
					t.Error(err)
				}
			}
			setLs("[]")
			if tt.runningAt == 0 {
				setLs(running)
			}
			bin := filepath.Join(dir, "container")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat \"${0%/*}/ls.json\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			secrets := filepath.Join(dir, "secrets")
			if err := os.Mkdir(secrets, 0o700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(secrets, secretsFile)
			if err := os.WriteFile(file, []byte("TOKEN=x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			e := &Engine{Bin: bin, secretsGrace: grace, secretsStartWait: tt.startWait}
			done := make(chan struct{})
			returned := make(chan time.Duration, 1)
			start := time.Now()
			go func() {
				e.removeSecretsOnceRunning("paddock-test", secrets, done)
				returned <- time.Since(start)
			}()
			// Registered after TempDir, so this runs before the directory is
			// removed: stop the remover and any pending flip to running.
			flipped := make(chan struct{})
			var flip *time.Timer
			if tt.runningAt > 0 {
				flip = time.AfterFunc(tt.runningAt, func() {
					defer close(flipped)
					setLs(running)
				})
			}
			t.Cleanup(func() {
				if flip != nil && !flip.Stop() {
					<-flipped
				}
				close(done)
			})
			// secrets.env must be present whenever it is checked before the
			// earliest allowed removal. The time is read after each Stat, so a
			// check delayed by test load cannot fail a correct remover.
			deadline := start.Add(tt.removedBy - 50*time.Millisecond)
			for time.Now().Before(deadline) {
				_, err := os.Stat(file)
				if at := time.Now(); err != nil && at.Before(deadline) {
					t.Fatalf("secrets.env gone at %s, before %s: %v", at.Sub(start), tt.removedBy, err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			eventually(t, 10*time.Second, "secrets.env to be removed", func() bool {
				_, err := os.Stat(file)
				return os.IsNotExist(err)
			})
			// The remover's own clock: it must not finish early, and finishing
			// long after the expected wait means it took the wrong path.
			select {
			case d := <-returned:
				if d < tt.removedBy || d > tt.removedBy+3*time.Second {
					t.Errorf("returned after %s; want between %s and %s", d, tt.removedBy, tt.removedBy+3*time.Second)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("removeSecretsOnceRunning did not return after removing secrets.env")
			}
		})
	}
}
