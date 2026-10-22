package sniproxy_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/guest/guesttest"
	"github.com/sdougbrown/paddock/internal/guest/sniproxy"
)

// routeDialer sends every dial to target and records the requested
// addresses. Addresses in refuse fail at once; addresses in hang block until
// the dial's context ends, like a host that drops SYNs.
type routeDialer struct {
	target string
	refuse map[string]bool
	hang   map[string]bool
	mu     sync.Mutex
	dialed []string
}

func (d *routeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	d.mu.Unlock()
	if d.refuse[addr] {
		return nil, errors.New("connection refused")
	}
	if d.hang[addr] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, d.target)
}

func (d *routeDialer) calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dialed...)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) denials(t *testing.T) []sniproxy.Denial {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sniproxy.Denial
	for _, line := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if line == "" {
			continue
		}
		var d sniproxy.Denial
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("denial log line %q: %v", line, err)
		}
		out = append(out, d)
	}
	return out
}

type harness struct {
	addr   string
	dialer *routeDialer
	log    *syncBuffer
	pool   *x509.CertPool
}

func startProxy(t *testing.T, mutate func(*sniproxy.Config)) *harness {
	t.Helper()
	cert, pool := guesttest.SelfSigned(t, "allowed.test", "second.test")
	up := guesttest.EchoTLS(t, cert)
	allow, err := sniproxy.NewAllowlist([]string{
		"allowed.test", "second.test", "loop.test", "gone.test", "flaky.test", "slow.test", "private.test", "resolver.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		dialer: &routeDialer{
			target: up.Addr().String(),
			refuse: map[string]bool{"203.0.113.30:443": true},
			hang:   map[string]bool{"203.0.113.40:443": true},
		},
		log:  &syncBuffer{},
		pool: pool,
	}
	cfg := sniproxy.Config{
		Allowlist: allow,
		Resolver: guesttest.Resolver{
			"allowed.test": {netip.MustParseAddr("2001:db8::10"), netip.MustParseAddr("203.0.113.10")},
			"second.test":  {netip.MustParseAddr("203.0.113.11")},
			"loop.test":    {netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1"), netip.MustParseAddr("169.254.1.1")},
			"flaky.test":   {netip.MustParseAddr("203.0.113.30"), netip.MustParseAddr("203.0.113.31")},
			"slow.test":    {netip.MustParseAddr("203.0.113.40"), netip.MustParseAddr("203.0.113.41")},
			"private.test": {
				netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("172.16.0.1"), netip.MustParseAddr("192.168.64.1"),
				netip.MustParseAddr("fd00::5"), netip.MustParseAddr("100.100.1.1"),
			},
			"resolver.test": {netip.MustParseAddr("198.51.100.53")},
		},
		ExcludeAddrs: []netip.Addr{netip.MustParseAddr("198.51.100.53")},
		Dialer:       h.dialer,
		HelloTimeout: 200 * time.Millisecond,
		DenyLog:      h.log,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := sniproxy.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = p.Serve(ln) }()
	h.addr = ln.Addr().String()
	return h
}

// roundTrip completes a TLS handshake through the proxy with serverName
// and exchanges one line.
func (h *harness) roundTrip(t *testing.T, serverName string) (string, error) {
	t.Helper()
	raw, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	c := tls.Client(raw, &tls.Config{ServerName: serverName, RootCAs: h.pool})
	if err := c.Handshake(); err != nil {
		return "", err
	}
	if _, err := c.Write([]byte("hi\n")); err != nil {
		return "", err
	}
	return bufio.NewReader(c).ReadString('\n')
}

func TestProxyAllowsAndSplices(t *testing.T) {
	h := startProxy(t, nil)
	for _, name := range []string{"allowed.test", "second.test"} {
		got, err := h.roundTrip(t, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != "echo hi\n" {
			t.Fatalf("%s: got %q", name, got)
		}
	}
	// The proxy dials the address the name resolves to, IPv4 first, never
	// the client's destination (the proxy's own listener here).
	want := []string{"203.0.113.10:443", "203.0.113.11:443"}
	if got := h.dialer.calls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dialed %v; want %v", got, want)
	}
	if d := h.log.denials(t); len(d) != 0 {
		t.Fatalf("unexpected denials %v", d)
	}
}

func TestProxyFallsBackAcrossAddresses(t *testing.T) {
	h := startProxy(t, nil)
	cert, pool := guesttest.SelfSigned(t, "flaky.test")
	up := guesttest.EchoTLS(t, cert)
	h.dialer.target = up.Addr().String()
	h.pool = pool
	if _, err := h.roundTrip(t, "flaky.test"); err != nil {
		t.Fatal(err)
	}
	want := "203.0.113.30:443,203.0.113.31:443"
	if got := strings.Join(h.dialer.calls(), ","); got != want {
		t.Fatalf("dialed %s; want %s", got, want)
	}
}

// A first address that never answers gets only its share of DialTimeout,
// so the second address still has time to connect.
func TestProxyDialsNextAddressAfterTimeout(t *testing.T) {
	h := startProxy(t, func(c *sniproxy.Config) { c.DialTimeout = 2 * time.Second })
	cert, pool := guesttest.SelfSigned(t, "slow.test")
	up := guesttest.EchoTLS(t, cert)
	h.dialer.target = up.Addr().String()
	h.pool = pool
	if _, err := h.roundTrip(t, "slow.test"); err != nil {
		t.Fatal(err)
	}
	want := "203.0.113.40:443,203.0.113.41:443"
	if got := strings.Join(h.dialer.calls(), ","); got != want {
		t.Fatalf("dialed %s; want %s", got, want)
	}
}

func TestProxyRefusals(t *testing.T) {
	tests := []struct {
		name       string
		send       func(t *testing.T, c net.Conn, h *harness)
		wantSNI    string
		wantReason sniproxy.Reason
	}{
		{"denied name", tlsHello("evil.test"), "evil.test", sniproxy.ReasonDenied},
		{"subdomain of allowed", tlsHello("sub.allowed.test"), "sub.allowed.test", sniproxy.ReasonDenied},
		{"no sni", tlsHello(""), "", sniproxy.ReasonNoSNI},
		{"ip literal", tlsHello("203.0.113.10"), "", sniproxy.ReasonNoSNI},
		{"http", writeBytes("GET / HTTP/1.1\r\nHost: allowed.test\r\n\r\n"), "", sniproxy.ReasonNotTLS},
		{"silent client", func(*testing.T, net.Conn, *harness) {}, "", sniproxy.ReasonTimeout},
		{"loopback upstream", tlsHello("loop.test"), "loop.test", sniproxy.ReasonUpstream},
		{"unresolvable", tlsHello("gone.test"), "gone.test", sniproxy.ReasonUpstream},
		{"private upstream", tlsHello("private.test"), "private.test", sniproxy.ReasonUpstream},
		{"resolver upstream", tlsHello("resolver.test"), "resolver.test", sniproxy.ReasonUpstream},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startProxy(t, nil)
			c, err := net.Dial("tcp", h.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			go tt.send(t, c, h)
			// The proxy closes the client without sending anything back.
			n, err := io.Copy(io.Discard, c)
			if err != nil && !errors.Is(err, net.ErrClosed) && !isConnReset(err) {
				t.Fatalf("read: %v", err)
			}
			if n != 0 {
				t.Fatalf("proxy sent %d bytes to a refused client", n)
			}
			// The denial is logged before the client connection closes.
			d := h.log.denials(t)
			if len(d) != 1 {
				t.Fatalf("denials = %v; want one", d)
			}
			if d[0].SNI != tt.wantSNI || d[0].Reason != tt.wantReason {
				t.Fatalf("denial = %+v; want sni %q reason %q", d[0], tt.wantSNI, tt.wantReason)
			}
			if d[0].Peer != c.LocalAddr().String() {
				t.Fatalf("peer = %q; want %q", d[0].Peer, c.LocalAddr())
			}
			if d[0].Time.IsZero() {
				t.Fatal("denial has no time")
			}
			if calls := h.dialer.calls(); len(calls) != 0 {
				t.Fatalf("dialed %v for a refused connection", calls)
			}
		})
	}
}

func isConnReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET)
}

func tlsHello(serverName string) func(*testing.T, net.Conn, *harness) {
	return func(_ *testing.T, c net.Conn, _ *harness) {
		_ = tls.Client(c, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}).Handshake()
	}
}

func writeBytes(s string) func(*testing.T, net.Conn, *harness) {
	return func(_ *testing.T, c net.Conn, _ *harness) { _, _ = c.Write([]byte(s)) }
}

func TestDenialJSONShape(t *testing.T) {
	b, err := json.Marshal(sniproxy.Denial{Time: time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC), SNI: "evil.test", Reason: sniproxy.ReasonDenied, Peer: "192.168.64.48:55390"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"time":"2026-10-22T17:00:00Z","sni":"evil.test","reason":"denied","peer":"192.168.64.48:55390"}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
}

func TestNewRequiresAllowlistAndResolver(t *testing.T) {
	if _, err := sniproxy.New(sniproxy.Config{Resolver: guesttest.Resolver{}}); err == nil {
		t.Error("New without allowlist succeeded")
	}
	allow, err := sniproxy.NewAllowlist([]string{"allowed.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sniproxy.New(sniproxy.Config{Allowlist: allow}); err == nil {
		t.Error("New without resolver succeeded")
	}
}
