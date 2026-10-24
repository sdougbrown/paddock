package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/guest/sniproxy"
)

// A denial log larger than denialLogLimit is read up to the cap: entries
// before it are found, entries after it are not, and no error is returned.
func TestHasDenialReadsUpToCap(t *testing.T) {
	line := func(sni string) string {
		b, err := json.Marshal(sniproxy.Denial{Time: time.Now(), SNI: sni, Reason: sniproxy.ReasonDenied, Peer: "192.168.64.2:1"})
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	match := func(d sniproxy.Denial) bool { return d.SNI == "target.test" }
	var b strings.Builder
	b.WriteString(line("target.test"))
	filler := line("other.test")
	for b.Len() <= denialLogLimit+len(filler) {
		b.WriteString(filler)
	}
	b.WriteString(line("target.test"))
	path := filepath.Join(t.TempDir(), "denials.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Size() <= denialLogLimit {
		t.Fatalf("log is %d bytes; want more than %d", info.Size(), denialLogLimit)
	}
	ok, err := hasDenial(path, match)
	if err != nil {
		t.Fatalf("hasDenial: %v", err)
	}
	if !ok {
		t.Error("entry within the cap not found")
	}

	var tail strings.Builder
	for tail.Len() <= denialLogLimit {
		tail.WriteString(filler)
	}
	tail.WriteString(line("target.test"))
	other := filepath.Join(t.TempDir(), "tail.jsonl")
	if err := os.WriteFile(other, []byte(tail.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err = hasDenial(other, match)
	if err != nil {
		t.Fatalf("hasDenial past cap: %v", err)
	}
	if ok {
		t.Error("entry past the cap was read")
	}
}

// The denial log path is guest-writable, so it may hold a FIFO, which must
// fail at once instead of blocking the open until a writer appears.
func TestHasDenialRejectsNonRegularFile(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "denials.jsonl")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := hasDenial(fifo, func(sniproxy.Denial) bool { return true })
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("err = %v; want not a regular file", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hasDenial blocked on a FIFO")
	}
}
