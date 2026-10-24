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

// The log is append-only, so the entries a probe just caused are at its
// end. A log larger than denialLogLimit is read from its last
// denialLogLimit bytes: an entry there is found however large the log grew,
// an entry before them is not, and no error is returned.
func TestHasDenialReadsTail(t *testing.T) {
	line := func(sni string) string {
		b, err := json.Marshal(sniproxy.Denial{Time: time.Now(), SNI: sni, Reason: sniproxy.ReasonDenied, Peer: "192.168.64.2:1"})
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	var b strings.Builder
	b.WriteString(line("head.test"))
	filler := line("other.test")
	for b.Len() <= denialLogLimit+len(filler) {
		b.WriteString(filler)
	}
	b.WriteString(line("tail.test"))
	path := filepath.Join(t.TempDir(), "denials.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Size() <= denialLogLimit+int64(len(filler)) {
		t.Fatalf("log is %d bytes; want more than %d", info.Size(), denialLogLimit+len(filler))
	}
	for sni, want := range map[string]bool{"tail.test": true, "head.test": false, "other.test": true} {
		ok, err := hasDenial(path, func(d sniproxy.Denial) bool { return d.SNI == sni })
		if err != nil {
			t.Fatalf("hasDenial(%s): %v", sni, err)
		}
		if ok != want {
			t.Errorf("hasDenial(%s) = %v; want %v", sni, ok, want)
		}
	}
	// An entry that starts exactly at the cap is read whole.
	exact := line("exact.test")
	rest := exact
	for len(rest)+len(filler) < denialLogLimit-1 {
		rest += filler
	}
	rest += strings.Repeat(" ", denialLogLimit-len(rest)-1) + "\n"
	at := filepath.Join(t.TempDir(), "at.jsonl")
	if err := os.WriteFile(at, []byte(line("head.test")+rest), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := hasDenial(at, func(d sniproxy.Denial) bool { return d.SNI == "exact.test" }); err != nil || !ok {
		t.Errorf("entry at the cap: ok = %v, err = %v; want found", ok, err)
	}
	// A cut that lands mid-entry skips the partial line rather than parsing it.
	var partial strings.Builder
	partial.WriteString(`{"sni":"cut.test","reason":"denied"}` + strings.Repeat(" ", denialLogLimit) + "\n")
	partial.WriteString(line("tail.test"))
	cut := filepath.Join(t.TempDir(), "cut.jsonl")
	if err := os.WriteFile(cut, []byte(partial.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := hasDenial(cut, func(d sniproxy.Denial) bool { return d.SNI == "tail.test" }); err != nil || !ok {
		t.Errorf("after a cut entry: ok = %v, err = %v; want the tail entry", ok, err)
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
