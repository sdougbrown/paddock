package applecontainer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRemoveSecretsOnceRunning drives removeSecretsOnceRunning with a fake
// CLI whose `container ls` reports the container running or never lists it.
// Either way secrets.env must be gone soon after the matching wait.
func TestRemoveSecretsOnceRunning(t *testing.T) {
	tests := []struct {
		name string
		ls   string // `container ls --format json` output
	}{
		{"running, guest never unlinks", `[{"configuration":{"id":"paddock-test"},"status":{"state":"running"}}]`},
		{"never running", `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "container")
			if err := os.WriteFile(filepath.Join(dir, "ls.json"), []byte(tt.ls), 0o644); err != nil {
				t.Fatal(err)
			}
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
			e := &Engine{Bin: bin, secretsGrace: 300 * time.Millisecond, secretsStartWait: 300 * time.Millisecond}
			returned := make(chan struct{})
			done := make(chan struct{})
			defer close(done)
			go func() {
				e.removeSecretsOnceRunning("paddock-test", secrets, done)
				close(returned)
			}()
			select {
			case <-returned:
			case <-time.After(10 * time.Second):
				t.Fatal("removeSecretsOnceRunning still waiting after 10s")
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Errorf("secrets.env still present: %v", err)
			}
		})
	}
}
