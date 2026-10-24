package sniproxy_test

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/guest/sniproxy"
)

const childArg = "-sniproxy-test-child"

// TestMain lets the test binary stand in for the proxy child: Launch execs
// it with childArg.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == childArg {
		fmt.Fprintln(os.Stderr, "child:", sniproxy.RunChild())
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func launchOpts(t *testing.T) sniproxy.LaunchOptions {
	t.Helper()
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	deny, err := os.OpenFile(filepath.Join(t.TempDir(), "deny.log"), os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deny.Close() })
	return sniproxy.LaunchOptions{
		Path:     "/nonexistent",
		UID:      65534,
		GID:      65534,
		Listener: ln,
		DenyLog:  deny,
		Config: sniproxy.ChildConfig{
			Allow:     []string{"allowed.test"},
			Resolvers: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		},
	}
}

func TestLaunchValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*sniproxy.LaunchOptions)
		wantErr string
	}{
		{"uid 0", func(o *sniproxy.LaunchOptions) { o.UID = 0 }, "as root"},
		{"nil listener", func(o *sniproxy.LaunchOptions) { o.Listener = nil }, "listener"},
		{"nil denial log", func(o *sniproxy.LaunchOptions) { o.DenyLog = nil }, "denial log"},
		{"invalid allowlist", func(o *sniproxy.LaunchOptions) { o.Config.Allow = []string{"bad host/x"} }, ""},
		{"no resolvers", func(o *sniproxy.LaunchOptions) { o.Config.Resolvers = nil }, "no resolvers"},
		{"IPv6 resolver", func(o *sniproxy.LaunchOptions) {
			o.Config.Resolvers = []netip.Addr{netip.MustParseAddr("fd00::1")}
		}, "IPv4"},
		{"mapped unspecified resolver", func(o *sniproxy.LaunchOptions) {
			o.Config.Resolvers = []netip.Addr{netip.MustParseAddr("::ffff:0.0.0.0")}
		}, "IPv4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := launchOpts(t)
			tc.mutate(&o)
			cmd, err := sniproxy.Launch(o)
			if err == nil {
				_ = cmd.Process.Kill()
				t.Fatal("Launch succeeded")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v; want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func requireRootLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("needs root on linux to drop to another uid")
	}
}

// exePath copies the test binary somewhere the child's uid can execute it:
// a directory of its own directly under os.TempDir, since t.TempDir's
// parents are private to the test's uid.
func exePath(t *testing.T) string {
	t.Helper()
	src, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "sniproxy-child-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "child")
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestLaunchServesPassedListener(t *testing.T) {
	requireRootLinux(t)
	o := launchOpts(t)
	o.Path = exePath(t)
	o.Args = []string{childArg}
	errLog, err := os.CreateTemp(t.TempDir(), "err")
	if err != nil {
		t.Fatal(err)
	}
	defer errLog.Close()
	o.ErrorLog = errLog
	// A supplementary group the child must not inherit, so the empty
	// Groups check holds even where the test runs with none.
	groups, err := syscall.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setgroups([]int{4242}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Setgroups(groups) })

	cmd, err := sniproxy.Launch(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	checkChildIDs(t, cmd.Process.Pid)
	// The parent may close its copy; the child keeps serving.
	addr := o.Listener.Addr().String()
	o.Listener.Close()

	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	// The handshake fails: the child denies the name, which proves the
	// connection reached the child, and the denial lands in the log on fd 4.
	_ = tls.Client(c, &tls.Config{ServerName: "denied.test"}).Handshake()
	c.Close()

	deadline := time.Now().Add(3 * time.Second)
	for {
		b, err := os.ReadFile(o.DenyLog.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"sni":"denied.test"`) {
			break
		}
		if time.Now().After(deadline) {
			eb, _ := os.ReadFile(errLog.Name())
			t.Fatalf("no denial for denied.test in %q; error log: %q", b, eb)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Errorf("child not running: %v", err)
	}
}

// checkChildIDs reads the running child's credentials from /proc: every
// uid and gid is 65534, and it has no supplementary groups.
func checkChildIDs(t *testing.T, pid int) {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Uid": "65534 65534 65534 65534", "Gid": "65534 65534 65534 65534", "Groups": ""}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if w, check := want[k]; ok && check {
			if got := strings.Join(strings.Fields(v), " "); got != w {
				t.Errorf("child %s: %q, want %q", k, got, w)
			}
			delete(want, k)
		}
	}
	for k := range want {
		t.Errorf("no %s line in /proc/%d/status", k, pid)
	}
}

func TestLaunchKillsChildThatNeverReports(t *testing.T) {
	requireRootLinux(t)
	o := launchOpts(t)
	o.Path = "/bin/sleep"
	o.Args = []string{"30"}
	o.ReadyTimeout = 300 * time.Millisecond
	start := time.Now()
	cmd, err := sniproxy.Launch(o)
	if err == nil {
		_ = cmd.Process.Kill()
		t.Fatal("Launch succeeded for a child that never prints ready")
	}
	if !strings.Contains(err.Error(), "not ready after") {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Launch took %v with a 300ms timeout", d)
	}
}

func TestLaunchReportsChildThatExitsBeforeReady(t *testing.T) {
	requireRootLinux(t)
	o := launchOpts(t)
	o.Path = "/bin/true"
	cmd, err := sniproxy.Launch(o)
	if err == nil {
		_ = cmd.Process.Kill()
		t.Fatal("Launch succeeded for a child that exits at once")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("err = %v", err)
	}
}
