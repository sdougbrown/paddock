package initcfg

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileInPlaceKeepsInode checks the bind-mount fallback: the file's
// content and mode change, the inode does not, so a mount that pins the file
// keeps working.
func TestWriteFileInPlaceKeepsInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileInPlace(path, []byte("10.0.0.1 api.example.com\n"), 0o644); err != nil {
		t.Fatalf("writeFileInPlace: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("writeFileInPlace replaced the inode; a bind mount would break")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "10.0.0.1 api.example.com\n" {
		t.Errorf("content = %q, %v", b, err)
	}
	if after.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v; want 0644", after.Mode().Perm())
	}
}
