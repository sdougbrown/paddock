package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const droppedStatus = `Name:	sh
Uid:	501	501	501	501
Gid:	20	20	20	20
Groups:	20
NoNewPrivs:	1
CapInh:	0000000000000000
CapPrm:	0000000000000000
CapEff:	0000000000000000
CapBnd:	0000000000000000
CapAmb:	0000000000000000
`

func TestCheckPrivileges(t *testing.T) {
	tests := []struct {
		name, status string
		uid, gid     int
		want         string
	}{
		{"dropped", droppedStatus, 501, 20, ""},
		{"bounding set kept", strings.Replace(droppedStatus, "CapBnd:	0000000000000000", "CapBnd:	00000000a80435fb", 1), 501, 20, "CapBnd=00000000a80435fb"},
		{"no_new_privs unset", strings.Replace(droppedStatus, "NoNewPrivs:	1", "NoNewPrivs:	0", 1), 501, 20, "NoNewPrivs=0"},
		{"missing ambient", strings.Replace(droppedStatus, "CapAmb:	0000000000000000\n", "", 1), 501, 20, "CapAmb="},
		{"wrong uid", droppedStatus, 1000, 20, "Uid=501 501 501 501"},
		{"effective uid 0", strings.Replace(droppedStatus, "Uid:	501	501	501	501", "Uid:	501	0	501	501", 1), 501, 20, "Uid=501 0 501 501"},
		{"saved uid 0", strings.Replace(droppedStatus, "Uid:	501	501	501	501", "Uid:	501	501	0	501", 1), 501, 20, "Uid=501 501 0 501"},
		{"no supplementary groups", strings.Replace(droppedStatus, "Groups:	20", "Groups:", 1), 501, 20, ""},
		{"wrong gid", droppedStatus, 501, 21, "Gid=20 20 20 20"},
		{"effective gid 0", strings.Replace(droppedStatus, "Gid:	20	20	20	20", "Gid:	20	0	20	20", 1), 501, 20, "Gid=20 0 20 20"},
		{"filesystem gid 0", strings.Replace(droppedStatus, "Gid:	20	20	20	20", "Gid:	20	20	20	0", 1), 501, 20, "Gid=20 20 20 0"},
		{"root supplementary group", strings.Replace(droppedStatus, "Groups:	20", "Groups:	0 20", 1), 501, 20, "Groups=0 20"},
		{"missing groups", strings.Replace(droppedStatus, "Groups:	20\n", "", 1), 501, 20, "Groups="},
		{"filesystem uid 0", strings.Replace(droppedStatus, "Uid:	501	501	501	501", "Uid:	501	501	501	0", 1), 501, 20, "Uid=501 501 501 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CheckPrivileges([]byte(tt.status), tt.uid, tt.gid)
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLocalChecks(t *testing.T) {
	dir := t.TempDir()
	status := filepath.Join(dir, "status")
	if err := os.WriteFile(status, []byte(droppedStatus), 0o600); err != nil {
		t.Fatal(err)
	}
	// A writable directory listed as read-only must fail its check.
	results := Run(context.Background(), LocalChecks(LocalOptions{
		UID:        501,
		GID:        20,
		StatusPath: status,
		ReadOnly:   []string{dir},
		Writable:   []string{dir},
	}))
	want := map[string]bool{"privileges-dropped": true, "readonly-mount " + dir: false, "writable " + dir: true}
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d", len(results), len(want))
	}
	for _, r := range results {
		if r.Pass != want[r.Name] {
			t.Errorf("%s: pass = %v (%s)", r.Name, r.Pass, r.Detail)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("checks left files behind: %v", entries)
	}
}

// A write refused for any reason but EROFS fails the read-only mount check.
func TestWriteRefusedNotEROFS(t *testing.T) {
	t.Run("exists", func(t *testing.T) {
		// The exclusive create fails with EEXIST, which root gets too.
		dir := t.TempDir()
		probe := filepath.Join(dir, fmt.Sprintf(".paddock-verify-%d", os.Getpid()))
		if err := os.WriteFile(probe, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if d, err := writeRefused(dir); err == nil || !strings.Contains(err.Error(), "refused, but not with EROFS") {
			t.Errorf("%s: err = %v; want the not-EROFS failure", d, err)
		}
	})
	t.Run("permission", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file permissions")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		results := Run(context.Background(), LocalChecks(LocalOptions{
			UID:        501,
			GID:        20,
			StatusPath: filepath.Join(dir, "status"),
			ReadOnly:   []string{dir},
		}))
		for _, r := range results {
			if r.Name == "readonly-mount "+dir {
				if r.Pass {
					t.Fatalf("permission-denied directory passed as read-only mount: %s", r.Detail)
				}
				if !strings.Contains(r.Detail, "refused, but not with EROFS") {
					t.Errorf("detail = %q; want the not-EROFS failure", r.Detail)
				}
				return
			}
		}
		t.Fatal("no readonly-mount result")
	})
}

// A protected file reached through a symlink can be replaced in the
// directory of the link or of the file it resolves to.
func TestReplaceDirsFollowsLinks(t *testing.T) {
	root := t.TempDir()
	linkDir, fileDir := filepath.Join(root, "link"), filepath.Join(root, "real")
	for _, d := range []string{linkDir, fileDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(fileDir, "resolv.conf")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "resolv.conf")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(fileDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := replaceDirs(link)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{linkDir, real}; !slices.Equal(got, want) {
		t.Errorf("replaceDirs(link) = %v; want %v", got, want)
	}
	if got, err := replaceDirs(file); err != nil || len(got) != 1 {
		t.Errorf("replaceDirs(file) = %v, %v; want one directory", got, err)
	}

	// Every link in a chain can be replaced, so each one's directory counts.
	hopDir := filepath.Join(root, "hop")
	if err := os.Mkdir(hopDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, filepath.Join(hopDir, "resolv.conf")); err != nil {
		t.Fatal(err)
	}
	chain := filepath.Join(linkDir, "chain")
	if err := os.Symlink("../hop/resolv.conf", chain); err != nil {
		t.Fatal(err)
	}
	realHop, err := filepath.EvalSymlinks(hopDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err = replaceDirs(chain)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{linkDir, realHop, real}; !slices.Equal(got, want) {
		t.Errorf("replaceDirs(chain) = %v; want %v", got, want)
	}
}

// A symlink loop ends in an error instead of walking forever.
func TestReplaceDirsStopsOnLoop(t *testing.T) {
	loop := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := replaceDirs(loop)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "too many levels of symbolic links") {
			t.Errorf("err = %v; want too many levels of symbolic links", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replaceDirs did not stop on a symlink loop")
	}
}
