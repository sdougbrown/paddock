package verify

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sdougbrown/paddock/internal/guest/guesttest"
)

// A read-only mount passes the read-only mount check, for a directory and
// for a file on it.
func TestWriteRefusedReadOnlyMount(t *testing.T) {
	if os.Geteuid() != 0 || !guesttest.HasCaps(t, guesttest.CapSysAdmin) {
		t.Skip("mounting needs root with CAP_SYS_ADMIN")
	}
	dir := t.TempDir()
	if err := syscall.Mount("tmpfs", dir, "tmpfs", 0, "size=64k"); err != nil {
		t.Fatalf("mount tmpfs: %v", err)
	}
	t.Cleanup(func() { syscall.Unmount(dir, 0) })
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("", dir, "", syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
		t.Fatalf("remount read-only: %v", err)
	}
	for _, p := range []string{dir, file} {
		if d, err := writeRefused(p); err != nil {
			t.Errorf("%s: %s: %v", p, d, err)
		}
	}
}
