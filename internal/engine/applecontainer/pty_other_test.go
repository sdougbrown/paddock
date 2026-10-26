//go:build !darwin

package applecontainer

import (
	"os"
	"testing"
)

func openTerminal(t *testing.T) *os.File {
	t.Helper()
	t.Skip("pseudo-terminal setup is implemented for darwin only")
	return nil
}
