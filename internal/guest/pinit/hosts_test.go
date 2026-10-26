package pinit

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteHosts(t *testing.T) {
	addr := netip.MustParseAddr("192.0.2.1")
	in := "127.0.0.1 localhost\n" +
		"# comment kept\n" +
		"192.168.64.145 guest-name\n" +
		"10.0.0.9 API.anthropic.com. mirror.internal # pinned elsewhere\n" +
		"10.0.0.8 registry.npmjs.org\n" +
		"192.0.2.1 stale.example\n" +
		"::ffff:192.0.2.1 mapped.example\n"
	path := filepath.Join(t.TempDir(), "hosts")
	writeFile(t, path, in, 0o600)
	hosts := []string{"API.anthropic.com.", "registry.npmjs.org", "api.anthropic.com"}
	if err := writeHosts(path, hosts, addr); err != nil {
		t.Fatal(err)
	}
	want := "127.0.0.1 localhost\n" +
		"# comment kept\n" +
		"192.168.64.145 guest-name\n" +
		"10.0.0.9\tmirror.internal\n" +
		"192.0.2.1\tapi.anthropic.com\n" +
		"192.0.2.1\tregistry.npmjs.org\n"
	if got := readFile(t, path); got != want {
		t.Errorf("hosts:\n%s\nwant:\n%s", got, want)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("mode: %v %v", st, err)
	}
	// A second run gives the same file.
	if err := writeHosts(path, hosts, addr); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != want {
		t.Errorf("second run:\n%s", got)
	}
}

func TestWriteHostsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := writeHosts(path, []string{"a.test"}, netip.MustParseAddr("192.0.2.1")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "192.0.2.1\ta.test\n" {
		t.Errorf("got %q", got)
	}
}

func TestWriteHostsRejectsInvalidHost(t *testing.T) {
	for _, bad := range []string{"*.example.com", "192.0.2.5", "lifeline.invalid", "not a host"} {
		path := filepath.Join(t.TempDir(), "hosts")
		writeFile(t, path, "127.0.0.1 localhost\n", 0o644)
		if err := writeHosts(path, []string{"a.test", bad}, netip.MustParseAddr("192.0.2.1")); err == nil {
			t.Errorf("%q: no error", bad)
		}
		if got := readFile(t, path); got != "127.0.0.1 localhost\n" {
			t.Errorf("%q: hosts changed:\n%s", bad, got)
		}
	}
}
