package resolve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/plan"
)

func TestSetupCacheHitAndMiss(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	s := &plan.SetupPlan{CacheHost: dir}
	if hit, err := PrepareSetupCache(s); err != nil || hit {
		t.Fatalf("new dir: hit=%v err=%v", hit, err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg.zip"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if hit, _ := PrepareSetupCache(s); hit {
		t.Error("populated dir without a completion marker counted as a hit")
	}
	if err := CompleteSetupCache(s); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("marker written inside the mounted cache dir: %v", entries)
	}
	if hit, _ := PrepareSetupCache(s); !hit {
		t.Error("completed dir not counted as a hit")
	}
	if err := CompleteSetupCache(s); err != nil {
		t.Fatalf("complete after prepare: %v", err)
	}
}

func TestSetupCachePrepareClearsMarker(t *testing.T) {
	s := &plan.SetupPlan{CacheHost: filepath.Join(t.TempDir(), "c")}
	PrepareSetupCache(s)
	if err := CompleteSetupCache(s); err != nil {
		t.Fatal(err)
	}
	if hit, _ := PrepareSetupCache(s); !hit {
		t.Fatal("want a hit")
	}
	if _, err := os.Lstat(s.CacheHost + ".complete"); !os.IsNotExist(err) {
		t.Fatalf("marker survived prepare: %v", err)
	}
}

func TestSetupCacheMarkerDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "victim")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &plan.SetupPlan{CacheHost: filepath.Join(root, "cache")}
	if err := os.Symlink(target, s.CacheHost+".complete"); err != nil {
		t.Fatal(err)
	}
	if hit, err := PrepareSetupCache(s); err != nil || hit {
		t.Fatalf("planted symlink: hit=%v err=%v", hit, err)
	}
	if err := CompleteSetupCache(s); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("symlink target overwritten: %q", b)
	}
	// A symlink planted after prepare is refused, not followed.
	os.Remove(s.CacheHost + ".complete")
	if err := os.Symlink(target, s.CacheHost+".complete"); err != nil {
		t.Fatal(err)
	}
	if err := CompleteSetupCache(s); err == nil {
		t.Error("marking through a symlink succeeded")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("symlink target overwritten: %q", b)
	}
}

func TestCacheDirNestsLowercasedRef(t *testing.T) {
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "yarn.lock"), []byte("a"), 0o644)
	d, err := CacheDir("/h", "Afresh/Scanner", ws, "yarn.lock")
	if err != nil || !strings.HasPrefix(d, filepath.Join("/h", ".cache", "paddock", "afresh", "scanner")+string(filepath.Separator)) {
		t.Fatalf("CacheDir = %q, %v", d, err)
	}
}

func TestSetupCacheRejectsOverlappingMounts(t *testing.T) {
	for _, guest := range []string{"/var/cache/paddock/setup", "/var/cache/paddock/setup/sub", "/var/cache", "/"} {
		r := &Resolver{Home: t.TempDir()}
		ws := t.TempDir()
		os.WriteFile(filepath.Join(ws, "yarn.lock"), []byte("a"), 0o644)
		p := &plan.Plan{
			Spec: "a/b", Workdir: ws,
			Mounts: []plan.Mount{{Host: "/x", Guest: guest}},
			Setup:  &plan.SetupPlan{CacheKeyFile: "yarn.lock", CacheDir: "/var/cache/paddock/setup"},
		}
		if err := r.setupCache(p); err == nil || !strings.Contains(err.Error(), "overlaps") {
			t.Errorf("mount at %q: err = %v", guest, err)
		}
	}
	r := &Resolver{Home: t.TempDir()}
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "yarn.lock"), []byte("a"), 0o644)
	p := &plan.Plan{
		Spec: "a/b", Workdir: ws,
		Mounts: []plan.Mount{{Host: "/x", Guest: "/var/cache/paddock/setup2"}},
		Setup:  &plan.SetupPlan{CacheKeyFile: "yarn.lock", CacheDir: "/var/cache/paddock/setup"},
	}
	if err := r.setupCache(p); err != nil {
		t.Errorf("sibling path rejected: %v", err)
	}
}
