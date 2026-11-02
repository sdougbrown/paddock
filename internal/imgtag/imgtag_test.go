package imgtag

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestHash(t *testing.T) {
	ctx := writeTree(t, map[string]string{"a.txt": "a", "sub/b.txt": "b"})
	base, err := Hash([]byte("FROM x"), ctx, "parent:1")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Hash([]byte("FROM x"), ctx, "parent:1")
	if base != again {
		t.Fatal("hash is not stable")
	}
	mut := func(name string, h string, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if h == base {
			t.Errorf("hash did not change with %s", name)
		}
	}
	h, err := Hash([]byte("FROM y"), ctx, "parent:1")
	mut("dockerfile", h, err)
	h, err = Hash([]byte("FROM x"), ctx, "parent:2")
	mut("parent", h, err)
	if err := os.WriteFile(filepath.Join(ctx, "sub", "b.txt"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err = Hash([]byte("FROM x"), ctx, "parent:1")
	mut("context content", h, err)
	if err := os.Chmod(filepath.Join(ctx, "a.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	h2, err := Hash([]byte("FROM x"), ctx, "parent:1")
	if err != nil || h2 == h {
		t.Errorf("hash did not change with file mode (err=%v)", err)
	}
}

func TestLayerTag(t *testing.T) {
	re := regexp.MustCompile(`^paddock/afresh-scanner-[0-9a-f]{8}:0123456789ab$`)
	if got := LayerTag("Afresh/Scanner", "0123456789abcdef"); !re.MatchString(got) {
		t.Fatalf("LayerTag = %q", got)
	}
	repos := map[string]string{}
	for _, spec := range []string{"x/y", "x-y", "X/Y", "base", "x"} {
		repo, _, _ := strings.Cut(LayerTag(spec, "0123456789abcdef"), ":")
		if prev, dup := repos[repo]; dup || repo == "paddock/base" {
			t.Errorf("specs %q and %q share the repository %s", prev, spec, repo)
		}
		repos[repo] = spec
	}
}

func TestHashFollowsSymlinkedContext(t *testing.T) {
	real := writeTree(t, map[string]string{"a.txt": "a"})
	link := filepath.Join(t.TempDir(), "ctx")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	before, err := Hash([]byte("FROM x"), link, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(link, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := Hash([]byte("FROM x"), link, "p")
	if err != nil || after == before {
		t.Fatalf("hash unchanged after editing through a symlinked context (err=%v)", err)
	}
}

func TestHashHashesSymlinksByTarget(t *testing.T) {
	ctx := writeTree(t, map[string]string{"keep": "k"})
	outside := writeTree(t, map[string]string{"one": "1", "two": "2"})
	link := filepath.Join(ctx, "link")
	if err := os.Symlink(filepath.Join(outside, "one"), link); err != nil {
		t.Fatal(err)
	}
	hash := func() string {
		t.Helper()
		h, err := Hash([]byte("FROM x"), ctx, "p")
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := hash()
	if err := os.WriteFile(filepath.Join(outside, "one"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hash() != base {
		t.Error("editing a link target's content changed the hash")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "two"), link); err != nil {
		t.Fatal(err)
	}
	if hash() == base {
		t.Error("retargeting a symlink did not change the hash")
	}
}
