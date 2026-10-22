package resolve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/plan"
)

func TestSetupCacheHitAndMiss(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	s := &plan.SetupPlan{CacheHost: dir}
	hit, release, err := PrepareSetupCache(s)
	if err != nil || hit {
		t.Fatalf("new dir: hit=%v err=%v", hit, err)
	}
	release()
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg.zip"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	hit, release, _ = PrepareSetupCache(s)
	release()
	if hit {
		t.Error("populated dir without a completion marker counted as a hit")
	}
	if err := CompleteSetupCache(s); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("marker written inside the mounted cache dir: %v", entries)
	}
	hit, release, _ = PrepareSetupCache(s)
	defer release()
	if !hit {
		t.Error("completed dir not counted as a hit")
	}
	if err := CompleteSetupCache(s); err != nil {
		t.Fatalf("complete after prepare: %v", err)
	}
}

func TestSetupCachePrepareClearsMarker(t *testing.T) {
	s := &plan.SetupPlan{CacheHost: filepath.Join(t.TempDir(), "c")}
	_, release, _ := PrepareSetupCache(s)
	if err := CompleteSetupCache(s); err != nil {
		t.Fatal(err)
	}
	release()
	hit, release, _ := PrepareSetupCache(s)
	defer release()
	if !hit {
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
	hit, release, err := PrepareSetupCache(s)
	defer release()
	if err != nil || hit {
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

func TestPrepareSetupCacheSerializes(t *testing.T) {
	s := &plan.SetupPlan{CacheHost: filepath.Join(t.TempDir(), "c")}
	_, release, err := PrepareSetupCache(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(s.CacheHost + ".lock"); err != nil {
		t.Fatalf("lock file beside the cache dir: %v", err)
	}
	if entries, _ := os.ReadDir(s.CacheHost); len(entries) != 0 {
		t.Fatalf("lock written inside the mounted cache dir: %v", entries)
	}
	type result struct {
		release func()
		err     error
	}
	second := make(chan result, 1)
	go func() {
		_, rel, err := PrepareSetupCache(s)
		second <- result{rel, err}
	}()
	select {
	case <-second:
		t.Fatal("second prepare returned while the first held the lock")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	release()
	select {
	case r := <-second:
		if r.err != nil {
			t.Fatal(r.err)
		}
		r.release()
	case <-time.After(5 * time.Second):
		t.Fatal("second prepare still blocked after release")
	}
}

func TestPrepareSetupCacheWithoutCacheTakesNoLock(t *testing.T) {
	_, release, err := PrepareSetupCache(&plan.SetupPlan{})
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestSetupCacheGuards(t *testing.T) {
	r, ws := testResolver(t, "", errors.New("no remote"))
	if err := os.WriteFile(filepath.Join(ws, "yarn.lock"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	useProfile(t, r, "[workspace]\nmode = \"rw\"\n")
	r.Source = fakeSource{specs: map[string]fakeSpec{
		"nocache": op("[setup]\ncommand = [\"true\"]\n"),
		"onlydir": op("[setup]\ncommand = [\"true\"]\ncache_dir = \"/var/cache/paddock/x\"\n"),
		"onlykey": op("[setup]\ncommand = [\"true\"]\ncache_key = \"yarn.lock\"\n"),
	}}
	p, err := r.Resolve(Options{Profile: "p", Spec: "nocache", Phase: PhaseSetup})
	if err != nil {
		t.Fatal(err)
	}
	if p.Setup.CacheHost != "" || len(p.Mounts) != 1 {
		t.Errorf("no cache fields: host %q, mounts %+v", p.Setup.CacheHost, p.Mounts)
	}
	for _, spec := range []string{"onlydir", "onlykey"} {
		_, err := r.Resolve(Options{Profile: "p", Spec: spec, Phase: PhaseSetup})
		if err == nil || !strings.Contains(err.Error(), "must be set together") {
			t.Errorf("%s: err = %v", spec, err)
		}
	}
}
