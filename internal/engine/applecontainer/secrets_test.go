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
// be removed. The survival check runs half a second before that, so a
// remover delayed by test load does not fail it.
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
			defer close(done)
			returned := make(chan time.Duration, 1)
			start := time.Now()
			go func() {
				e.removeSecretsOnceRunning("paddock-test", secrets, done)
				returned <- time.Since(start)
			}()
			if tt.runningAt > 0 {
				time.Sleep(tt.runningAt)
				setLs(running)
			}
			// Half a second of slack before the earliest allowed removal.
			time.Sleep(tt.removedBy - 500*time.Millisecond - time.Since(start))
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("secrets.env gone at %s, before %s: %v", time.Since(start), tt.removedBy, err)
			}
			select {
			case d := <-returned:
				if d < tt.removedBy || d > tt.removedBy+3*time.Second {
					t.Errorf("returned after %s; want between %s and %s", d, tt.removedBy, tt.removedBy+3*time.Second)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("removeSecretsOnceRunning still waiting after 10s")
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Errorf("secrets.env still present: %v", err)
			}
		})
	}
}
