package applecontainer

import (
	"os"
	"testing"

	"github.com/sdougbrown/paddock/internal/runstage"
)

func TestIsTTY(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()

	if runstage.IsTTY(r) {
		t.Error("pipe reported as a terminal")
	}
	if runstage.IsTTY(null) {
		t.Error("/dev/null reported as a terminal")
	}
	t.Run("terminal", func(t *testing.T) {
		if !runstage.IsTTY(openTerminal(t)) {
			t.Error("pseudo-terminal not reported as a terminal")
		}
	})
}
