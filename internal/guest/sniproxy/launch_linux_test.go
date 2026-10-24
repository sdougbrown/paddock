package sniproxy_test

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/sdougbrown/paddock/internal/guest/guesttest"
	"github.com/sdougbrown/paddock/internal/guest/sniproxy"
)

const netnsEnv = "SNIPROXY_TEST_NETNS"

// inNetNS reports whether the test runs in its own network namespace. If
// not, it runs the test again in a re-exec of the test binary inside a new
// one, fails if that run does not pass, and returns false; the caller then
// returns. It skips without the capabilities a namespace needs.
func inNetNS(t *testing.T) bool {
	t.Helper()
	if os.Getenv(netnsEnv) == "1" {
		return true
	}
	if !hasCaps(t, capNetAdmin, capSysAdmin) {
		t.Skip("needs CAP_SYS_ADMIN and CAP_NET_ADMIN for a network namespace")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), netnsEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("run in a new network namespace: %v\n%s", err, out)
	}
	return false
}

const (
	capNetAdmin = 12
	capSysAdmin = 21
)

// hasCaps reports whether the process's effective capability set holds
// every one of caps.
func hasCaps(t *testing.T, caps ...uint) bool {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "CapEff:"); ok {
			eff, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range caps {
				if eff&(1<<c) == 0 {
					return false
				}
			}
			return true
		}
	}
	t.Fatal("no CapEff line in /proc/self/status")
	return false
}

// upLoopback brings lo up and adds aliases to it as lo:1, lo:2, and so on.
// A new network namespace has only a down lo, and the proxy refuses
// loopback upstreams, so the upstream needs an address of its own. lo
// already up means this is not a fresh namespace, so it changes nothing.
func upLoopback(t *testing.T, aliases ...netip.Addr) {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	var flags struct {
		name  [syscall.IFNAMSIZ]byte
		flags uint16
		_     [22]byte
	}
	copy(flags.name[:], "lo")
	if err := ioctl(fd, syscall.SIOCGIFFLAGS, unsafe.Pointer(&flags)); err != nil {
		t.Fatalf("get lo flags: %v", err)
	}
	if flags.flags&syscall.IFF_UP != 0 {
		t.Fatal("lo is already up: not a fresh network namespace")
	}
	flags.flags |= syscall.IFF_UP
	if err := ioctl(fd, syscall.SIOCSIFFLAGS, unsafe.Pointer(&flags)); err != nil {
		t.Fatalf("bring lo up: %v", err)
	}
	for i, a := range aliases {
		var addr struct {
			name [syscall.IFNAMSIZ]byte
			addr syscall.RawSockaddrInet4
			_    [8]byte
		}
		copy(addr.name[:], fmt.Sprintf("lo:%d", i+1))
		addr.addr.Family = syscall.AF_INET
		addr.addr.Addr = a.As4()
		if err := ioctl(fd, syscall.SIOCSIFADDR, unsafe.Pointer(&addr)); err != nil {
			t.Fatalf("add %s to lo: %v", a, err)
		}
	}
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// TestLaunchAllowedNameEndToEnd: the child resolves an allowlisted name
// through its own DNS client, at the configured resolver, and splices a TLS
// session to the upstream it names. The resolver is not on 127.0.0.1, where
// the system resolver's default or a local nameserver would also find it.
// An allowlisted name that resolves to the resolver itself is refused: the
// child never dials its resolvers as an upstream.
func TestLaunchAllowedNameEndToEnd(t *testing.T) {
	requireRootLinux(t)
	if !inNetNS(t) {
		return
	}
	upstream := netip.MustParseAddr("203.0.113.10")
	resolver := netip.MustParseAddr("198.51.100.53")
	upLoopback(t, upstream, resolver)
	queries := sniproxy.ServeDNSAt(t, netip.AddrPortFrom(resolver, 53), map[string]netip.Addr{
		"allowed.test.":  upstream,
		"resolver.test.": resolver,
	})

	cert, pool := guesttest.SelfSigned(t, "allowed.test")
	up, err := tls.Listen("tcp4", netip.AddrPortFrom(upstream, 443).String(), &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { up.Close() })
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if line, err := bufio.NewReader(c).ReadString('\n'); err == nil {
			_, _ = c.Write([]byte("echo " + line))
		}
	}()

	o := launchOpts(t)
	o.Path = exePath(t)
	o.Args = []string{childArg}
	o.Config.Allow = []string{"allowed.test", "resolver.test"}
	o.Config.Resolvers = []netip.Addr{resolver}
	errLog, err := os.CreateTemp(t.TempDir(), "err")
	if err != nil {
		t.Fatal(err)
	}
	defer errLog.Close()
	o.ErrorLog = errLog
	cmd, err := sniproxy.Launch(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	raw, err := net.DialTimeout("tcp", o.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	c := tls.Client(raw, &tls.Config{ServerName: "allowed.test", RootCAs: pool})
	if _, err := c.Write([]byte("hello\n")); err != nil {
		b, _ := os.ReadFile(o.DenyLog.Name())
		t.Fatalf("through the proxy: %v; denials: %q", err, b)
	}
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || got != "echo hello\n" {
		t.Fatalf("got %q, %v; want the upstream's echo", got, err)
	}
	if !slices.Contains(queries(), "allowed.test. TypeA") {
		t.Errorf("resolver saw %q; want the child's A query for allowed.test", queries())
	}

	// A dial to the resolver's :443 would succeed through this listener's
	// backlog and splice, so the upstream denial comes only from the child
	// excluding its resolver.
	trap, err := net.Listen("tcp4", netip.AddrPortFrom(resolver, 443).String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { trap.Close() })
	raw2, err := net.DialTimeout("tcp", o.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw2.Close()
	_ = raw2.SetDeadline(time.Now().Add(5 * time.Second))
	_ = tls.Client(raw2, &tls.Config{ServerName: "resolver.test"}).Handshake()
	waitDenial(t, o, `"sni":"resolver.test","reason":"upstream"`)
}
