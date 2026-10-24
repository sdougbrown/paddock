package sniproxy_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
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
// addresses and their dial deadlines. Addresses in refuse fail at once;
// addresses in timeout fail at once with context.DeadlineExceeded, as a host
// that drops SYNs does once the dial's share of DialTimeout runs out.
type routeDialer struct {
	target    string
	refuse    map[string]bool
	timeout   map[string]bool
	mu        sync.Mutex
	dialed    []string
	deadlines []time.Time
	// failWrite makes every successful dial return a connection whose
	// Write fails.
	failWrite bool
	// stall makes every dial return a connection that nothing reads, so
	// writes to it block until their deadline.
	stall bool
}

func (d *routeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	deadline, _ := ctx.Deadline()
	d.deadlines = append(d.deadlines, deadline)
	fail, stall := d.failWrite, d.stall
	d.mu.Unlock()
	if d.refuse[addr] {
		return nil, errors.New("connection refused")
	}
	if d.timeout[addr] {
		return nil, context.DeadlineExceeded
	}
	if stall {
		c, _ := net.Pipe()
		return c, nil
	}
	var nd net.Dialer
	c, err := nd.DialContext(ctx, network, d.target)
	if err != nil || !fail {
		return c, err
	}
	return failWriteConn{c}, nil
}

// failWriteConn is a connection whose Write always fails.
type failWriteConn struct{ net.Conn }

func (failWriteConn) Write([]byte) (int, error) {
	return 0, errors.New("write refused")
}

// failingWriter is a denial log whose Write always fails.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("disk full")
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

// lookupRecorder is a resolver that records the names it is asked for.
type lookupRecorder struct {
	guesttest.Resolver
	mu    sync.Mutex
	names []string
}

func (r *lookupRecorder) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	r.names = append(r.names, host)
	r.mu.Unlock()
	return r.Resolver.LookupNetIP(ctx, network, host)
}

func (r *lookupRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

type harness struct {
	addr     string
	dialer   *routeDialer
	resolver *lookupRecorder
	log      *syncBuffer
	pool     *x509.CertPool
}

func startProxy(t *testing.T, mutate func(*sniproxy.Config)) *harness {
	t.Helper()
	cert, pool := guesttest.SelfSigned(t, "allowed.test", "second.test")
	up := guesttest.EchoTLS(t, cert)
	allow, err := sniproxy.NewAllowlist([]string{
		"allowed.test", "second.test", "loop.test", "gone.test", "flaky.test", "slow.test", "private.test", "resolver.test", "refused.test",
		"unspecified.test", "multicast.test", "mapped.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		dialer: &routeDialer{
			target:  up.Addr().String(),
			refuse:  map[string]bool{"203.0.113.30:443": true, "203.0.113.50:443": true, "203.0.113.51:443": true},
			timeout: map[string]bool{"203.0.113.40:443": true},
		},
		resolver: &lookupRecorder{Resolver: guesttest.Resolver{
			"allowed.test": {netip.MustParseAddr("2001:db8::10"), netip.MustParseAddr("203.0.113.10")},
			"second.test":  {netip.MustParseAddr("203.0.113.11")},
			"loop.test":    {netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1"), netip.MustParseAddr("169.254.1.1")},
			"flaky.test":   {netip.MustParseAddr("203.0.113.30"), netip.MustParseAddr("203.0.113.31")},
			"slow.test":    {netip.MustParseAddr("203.0.113.40"), netip.MustParseAddr("203.0.113.41")},
			"private.test": {
				netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("172.16.0.1"), netip.MustParseAddr("192.168.64.1"),
				netip.MustParseAddr("fd00::5"), netip.MustParseAddr("100.100.1.1"),
			},
			"resolver.test":    {netip.MustParseAddr("198.51.100.53")},
			"refused.test":     {netip.MustParseAddr("203.0.113.50"), netip.MustParseAddr("203.0.113.51")},
			"unspecified.test": {netip.MustParseAddr("0.0.0.0"), netip.MustParseAddr("::")},
			"multicast.test":   {netip.MustParseAddr("224.0.0.251"), netip.MustParseAddr("ff0e::1")},
			"mapped.test":      {netip.MustParseAddr("::ffff:100.64.1.1"), netip.MustParseAddr("::ffff:198.51.100.53")},
		}},
		log:  &syncBuffer{},
		pool: pool,
	}
	cfg := sniproxy.Config{
		Allowlist:    allow,
		Resolver:     h.resolver,
		ExcludeAddrs: []netip.Addr{netip.MustParseAddr("198.51.100.53")},
		Dialer:       h.dialer,
		HelloTimeout: 200 * time.Millisecond,
		DenyLog:      h.log,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h.addr = guesttest.ServeProxy(t, cfg)
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
// so the second address still has time to connect. The shares are read from
// the dial deadlines, so the test does not wait for one to run out.
func TestProxyDialsNextAddressAfterTimeout(t *testing.T) {
	const timeout = 10 * time.Second
	h := startProxy(t, func(c *sniproxy.Config) { c.DialTimeout = timeout })
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
	h.dialer.mu.Lock()
	first, second := h.dialer.deadlines[0], h.dialer.deadlines[1]
	h.dialer.mu.Unlock()
	if first.IsZero() || second.IsZero() {
		t.Fatalf("dial deadlines %v, %v; want both set", first, second)
	}
	// The first address gets half the time left; the second gets the rest,
	// up to the overall deadline.
	if gap := second.Sub(first); gap < timeout/5 {
		t.Fatalf("first dial deadline %v is only %v before the second's %v; want about %v", first, gap, second, timeout/2)
	}
}

// If the ClientHello cannot be replayed upstream, the client is closed with
// nothing sent and no denial is logged: the connection was allowed.
func TestProxyReplayWriteFailureClosesClient(t *testing.T) {
	h := startProxy(t, nil)
	h.dialer.mu.Lock()
	h.dialer.failWrite = true
	h.dialer.mu.Unlock()
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	go tlsHello("allowed.test")(t, c, h)
	n, err := io.Copy(io.Discard, c)
	if err != nil && !errors.Is(err, net.ErrClosed) && !isConnReset(err) {
		t.Fatalf("read: %v", err)
	}
	if n != 0 {
		t.Fatalf("proxy sent %d bytes after a failed replay", n)
	}
	if d := h.log.denials(t); len(d) != 0 {
		t.Fatalf("denials = %v; want none for an allowed connection", d)
	}
	if calls := h.dialer.calls(); len(calls) != 1 {
		t.Fatalf("dialed %v; want one dial", calls)
	}
}

// An upstream that accepts the connection but never reads cannot hold the
// ClientHello replay, and with it the client, open past DialTimeout.
func TestProxyReplayStallClosesClient(t *testing.T) {
	h := startProxy(t, func(c *sniproxy.Config) { c.DialTimeout = 300 * time.Millisecond })
	h.dialer.mu.Lock()
	h.dialer.stall = true
	h.dialer.mu.Unlock()
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	go tlsHello("allowed.test")(t, c, h)
	n, err := io.Copy(io.Discard, c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("client read timed out; the stalled replay held the connection open")
	}
	if n != 0 {
		t.Fatalf("proxy sent %d bytes after a stalled replay", n)
	}
	if d := h.log.denials(t); len(d) != 0 {
		t.Fatalf("denials = %v; want none for an allowed connection", d)
	}
}

// An upstream that answers and then ends the stream, by FIN or by RST,
// makes the proxy close the client promptly rather than leaving it to its
// read deadline.
func TestProxyUpstreamEndClosesClient(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reset bool
	}{{"close", false}, {"reset", true}} {
		t.Run(tt.name, func(t *testing.T) {
			h := startProxy(t, nil)
			cert, pool := guesttest.SelfSigned(t, "allowed.test")
			h.dialer.target = abruptUpstream(t, cert, tt.reset)
			h.pool = pool
			raw, err := net.Dial("tcp", h.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
			c := tls.Client(raw, &tls.Config{ServerName: "allowed.test", RootCAs: pool})
			if err := c.Handshake(); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Write([]byte("hi\n")); err != nil {
				t.Fatal(err)
			}
			_ = raw.SetReadDeadline(time.Now().Add(2 * time.Second))
			data, err := io.ReadAll(c)
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatal("client read timed out; the proxy did not close the stream")
			}
			// A FIN leaves the reply intact. A reset may discard it.
			if !tt.reset && string(data) != "echo hi\n" {
				t.Fatalf("client read %q; want the upstream reply", data)
			}
		})
	}
}

// A client that ends its sending side cleanly still receives the upstream's
// reply: the proxy half-closes toward the upstream instead of tearing down
// both directions.
func TestProxyClientHalfCloseKeepsReply(t *testing.T) {
	h := startProxy(t, nil)
	cert, pool := guesttest.SelfSigned(t, "allowed.test")
	h.dialer.target = replyAfterEOFUpstream(t, cert)
	raw, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	c := tls.Client(raw, &tls.Config{ServerName: "allowed.test", RootCAs: pool})
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := raw.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("reading reply: %v", err)
	}
	if string(data) != "got 5 bytes\n" {
		t.Fatalf("client read %q; want the reply sent after the upstream saw EOF", data)
	}
}

// replyAfterEOFUpstream serves one TLS connection: it reads until the client
// ends its stream with close_notify and its TCP connection then reaches EOF,
// and only then replies with the byte count and closes. It returns its
// address.
func replyAfterEOFUpstream(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
		tc := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
		n, err := io.Copy(io.Discard, tc)
		if err != nil {
			return
		}
		// close_notify ends the TLS stream; the half-close must still reach
		// the TCP connection.
		if _, err := raw.Read(make([]byte, 1)); err != io.EOF {
			return
		}
		_, _ = fmt.Fprintf(tc, "got %d bytes\n", n)
		_ = tc.Close()
	}()
	return ln.Addr().String()
}

// abruptUpstream serves one TLS connection: it answers the first line and
// then closes, sending a reset when reset is set. It returns its address.
func abruptUpstream(t *testing.T, cert tls.Certificate, reset bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		tc := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
		line, err := bufio.NewReader(tc).ReadString('\n')
		if err == nil {
			_, _ = tc.Write([]byte("echo " + line))
		}
		if tcp, ok := raw.(*net.TCPConn); ok && reset {
			_ = tcp.SetLinger(0)
		}
		_ = raw.Close()
	}()
	return ln.Addr().String()
}

// The denial log failing must not stop the proxy from serving later
// connections.
func TestProxyServesAfterDenyLogFailure(t *testing.T) {
	h := startProxy(t, func(c *sniproxy.Config) { c.DenyLog = failingWriter{} })
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	tlsHello("evil.test")(t, c, h)
	_, _ = io.Copy(io.Discard, c)
	c.Close()
	got, err := h.roundTrip(t, "allowed.test")
	if err != nil {
		t.Fatalf("allowed connection after a failed denial write: %v", err)
	}
	if got != "echo hi\n" {
		t.Fatalf("got %q", got)
	}
}

// Denial timestamps come from Config.Now.
func TestDenialTimeUsesConfigNow(t *testing.T) {
	fixed := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	h := startProxy(t, func(c *sniproxy.Config) {
		c.Now = func() time.Time { return fixed }
	})
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	tlsHello("evil.test")(t, c, h)
	_, _ = io.Copy(io.Discard, c)
	d := h.log.denials(t)
	if len(d) != 1 {
		t.Fatalf("denials = %v; want one", d)
	}
	if !d[0].Time.Equal(fixed) {
		t.Fatalf("denial time = %v; want injected %v", d[0].Time, fixed)
	}
}

func TestProxyRefusals(t *testing.T) {
	tests := []struct {
		name       string
		send       func(t *testing.T, c net.Conn, h *harness)
		wantSNI    string
		wantReason sniproxy.Reason
		wantDials  []string
	}{
		{"denied name", tlsHello("evil.test"), "evil.test", sniproxy.ReasonDenied, nil},
		{"subdomain of allowed", tlsHello("sub.allowed.test"), "sub.allowed.test", sniproxy.ReasonDenied, nil},
		// A reserved name cannot be allowlisted, but it is still a hostname:
		// the lifeline's denied host relies on a ReasonDenied line naming it.
		{"reserved name", tlsHello("paddock-lifeline-denied.invalid"), "paddock-lifeline-denied.invalid", sniproxy.ReasonDenied, nil},
		{"no sni", tlsHello(""), "", sniproxy.ReasonNoSNI, nil},
		// ECH is refused even under an allowlisted outer name.
		{"ech", func(t *testing.T, c net.Conn, _ *harness) { _, _ = c.Write(sniproxy.ECHHello(t, "allowed.test")) },
			"allowed.test", sniproxy.ReasonECH, nil},
		{"ip literal", tlsHello("203.0.113.10"), "", sniproxy.ReasonNoSNI, nil},
		{"http", writeBytes("GET / HTTP/1.1\r\nHost: allowed.test\r\n\r\n"), "", sniproxy.ReasonNotTLS, nil},
		{"silent client", func(*testing.T, net.Conn, *harness) {}, "", sniproxy.ReasonTimeout, nil},
		{"loopback upstream", tlsHello("loop.test"), "loop.test", sniproxy.ReasonUpstream, nil},
		{"unresolvable", tlsHello("gone.test"), "gone.test", sniproxy.ReasonUpstream, nil},
		{"private upstream", tlsHello("private.test"), "private.test", sniproxy.ReasonUpstream, nil},
		{"resolver upstream", tlsHello("resolver.test"), "resolver.test", sniproxy.ReasonUpstream, nil},
		{"unspecified upstream", tlsHello("unspecified.test"), "unspecified.test", sniproxy.ReasonUpstream, nil},
		{"multicast upstream", tlsHello("multicast.test"), "multicast.test", sniproxy.ReasonUpstream, nil},
		{"mapped shared and resolver upstream", tlsHello("mapped.test"), "mapped.test", sniproxy.ReasonUpstream, nil},
		{"every upstream refuses", tlsHello("refused.test"), "refused.test", sniproxy.ReasonUpstream,
			[]string{"203.0.113.50:443", "203.0.113.51:443"}},
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
			if got, want := strings.Join(h.dialer.calls(), ","), strings.Join(tt.wantDials, ","); got != want {
				t.Fatalf("dialed %q; want %q", got, want)
			}
			// Only an allowlisted name is ever resolved.
			lookups := h.resolver.calls()
			if tt.wantReason != sniproxy.ReasonUpstream && len(lookups) != 0 {
				t.Fatalf("looked up %v for a connection refused as %q", lookups, tt.wantReason)
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
