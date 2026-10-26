package applecontainer

import (
	"os"
	"testing"
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

	if isTTY(r) {
		t.Error("pipe reported as a terminal")
	}
	if isTTY(null) {
		t.Error("/dev/null reported as a terminal")
	}
	t.Run("terminal", func(t *testing.T) {
		if !isTTY(openTerminal(t)) {
			t.Error("pseudo-terminal not reported as a terminal")
		}
	})
}
