package sniproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/sdougbrown/paddock/internal/guest/dnsclient"
)

// dnsServer is a fake DNS server on loopback, answering over UDP and TCP on
// the same port.
type dnsServer struct {
	addr netip.AddrPort
	// listen, when valid, is the address to serve on instead of a free
	// loopback port.
	listen netip.AddrPort
	// answer builds the response to q; tcp reports the transport.
	answer func(q dnsmessage.Question, tcp bool) (dnsmessage.Header, []dnsmessage.Resource)
	// badID makes every response carry the wrong ID.
	badID bool
	// stray sends, ahead of each UDP response, two datagrams that do not
	// answer the query: one with the wrong ID and one with the wrong
	// question. Both carry strayAddr for the queried name.
	stray bool
	// dropFirst is how many UDP datagrams to discard unanswered before
	// serving, like a guest network that is not forwarding yet.
	dropFirst int

	mu       sync.Mutex
	asked    []string
	received int
	tcpAsked int
	udp      arrivals
}

// arrivals records when each UDP query datagram reached a fake server, so a
// test can tell a resend, which waits out dnsclient.ResendInterval, from a
// duplicate send, which does not.
type arrivals struct {
	mu sync.Mutex
	at map[string][]time.Time
}

func (a *arrivals) record(t *testing.T, query []byte) {
	var m dnsmessage.Message
	if err := m.Unpack(query); err != nil || len(m.Questions) != 1 {
		return
	}
	key := m.Questions[0].Name.String() + " " + m.Questions[0].Type.String()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.at == nil {
		a.at = map[string][]time.Time{}
	}
	a.at[key] = append(a.at[key], time.Now())
}

// checkSpaced fails t if two datagrams for one question arrived less than
// half a resend interval apart.
func (a *arrivals) checkSpaced(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for q, times := range a.at {
		for i := 1; i < len(times); i++ {
			if gap := times[i].Sub(times[i-1]); gap < dnsclient.ResendInterval/2 {
				t.Errorf("query %q sent twice %v apart; a resend waits %v", q, gap, dnsclient.ResendInterval)
			}
		}
	}
}

func startDNS(t *testing.T, answer func(dnsmessage.Question, bool) (dnsmessage.Header, []dnsmessage.Resource)) *dnsServer {
	t.Helper()
	return serveDNS(t, &dnsServer{answer: answer})
}

// serveDNS starts s, which must be fully configured: its fields are read by
// the server goroutines.
func serveDNS(t *testing.T, s *dnsServer) *dnsServer {
	t.Helper()
	// The UDP and TCP sockets share a port; another process may hold the
	// TCP side of the UDP port picked, so try again.
	var pc net.PacketConn
	var ln net.Listener
	listen := "127.0.0.1:0"
	if s.listen.IsValid() {
		listen = s.listen.String()
	}
	for range 20 {
		var err error
		if pc, err = net.ListenPacket("udp4", listen); err != nil {
			t.Fatal(err)
		}
		s.addr = pc.LocalAddr().(*net.UDPAddr).AddrPort()
		if ln, err = net.Listen("tcp4", s.addr.String()); err == nil {
			break
		}
		pc.Close()
		pc = nil
		if s.listen.IsValid() {
			t.Fatal(err)
		}
	}
	if pc == nil {
		t.Fatal("no port free for both UDP and TCP")
	}
	t.Cleanup(func() { pc.Close() })
	t.Cleanup(func() { ln.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			s.udp.record(t, buf[:n])
			s.mu.Lock()
			s.received++
			drop := s.received <= s.dropFirst
			s.mu.Unlock()
			if drop {
				continue
			}
			if s.stray {
				for _, b := range strayResponses(t, buf[:n]) {
					_, _ = pc.WriteTo(b, from)
				}
			}
			if resp := s.respond(t, buf[:n], false); resp != nil {
				_, _ = pc.WriteTo(resp, from)
			}
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var lb [2]byte
				if _, err := io.ReadFull(c, lb[:]); err != nil {
					return
				}
				q := make([]byte, binary.BigEndian.Uint16(lb[:]))
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				if resp := s.respond(t, q, true); resp != nil {
					_, _ = c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(resp))), resp...))
				}
			}()
		}
	}()
	return s
}

func (s *dnsServer) respond(t *testing.T, query []byte, tcp bool) []byte {
	var m dnsmessage.Message
	if err := m.Unpack(query); err != nil || len(m.Questions) != 1 {
		t.Errorf("fake dns: bad query: %v", err)
		return nil
	}
	q := m.Questions[0]
	s.mu.Lock()
	s.asked = append(s.asked, q.Name.String()+" "+q.Type.String())
	if tcp {
		s.tcpAsked++
	}
	s.mu.Unlock()
	h, answers := s.answer(q, tcp)
	h.ID, h.Response = m.ID, true
	if s.badID {
		h.ID++
	}
	resp := dnsmessage.Message{Header: h, Questions: m.Questions, Answers: answers}
	b, err := resp.Pack()
	if err != nil {
		t.Errorf("fake dns: pack: %v", err)
		return nil
	}
	return b
}

const strayAddr = "198.51.100.66"

func strayResponses(t *testing.T, query []byte) [][]byte {
	var m dnsmessage.Message
	if err := m.Unpack(query); err != nil || len(m.Questions) != 1 {
		return nil
	}
	q := m.Questions[0]
	answers := []dnsmessage.Resource{rr(q.Name.String(), a4(strayAddr))}
	other := q
	other.Name = dnsmessage.MustNewName("other.test.")
	var out [][]byte
	for _, msg := range []dnsmessage.Message{
		{Header: dnsmessage.Header{ID: m.ID + 1, Response: true}, Questions: m.Questions, Answers: answers},
		{Header: dnsmessage.Header{ID: m.ID, Response: true}, Questions: []dnsmessage.Question{other}, Answers: answers},
	} {
		b, err := msg.Pack()
		if err != nil {
			t.Errorf("fake dns: pack: %v", err)
			return nil
		}
		out = append(out, b)
	}
	return out
}

// ServeDNSAt starts a fake DNS server on addr for the launch tests, which
// cannot reach the unexported fake. It answers A queries for the names in
// a and returns the questions it was asked.
func ServeDNSAt(t *testing.T, addr netip.AddrPort, a map[string]netip.Addr) func() []string {
	t.Helper()
	s := serveDNS(t, &dnsServer{listen: addr, answer: func(q dnsmessage.Question, _ bool) (dnsmessage.Header, []dnsmessage.Resource) {
		if ip, ok := a[q.Name.String()]; ok && q.Type == dnsmessage.TypeA {
			return dnsmessage.Header{}, []dnsmessage.Resource{rr(q.Name.String(), &dnsmessage.AResource{A: ip.As4()})}
		}
		return dnsmessage.Header{}, nil
	}})
	return s.queries
}

// queries returns the distinct questions asked, sorted: the resolver asks
// the record types concurrently, so arrival order is not defined, and a
// query that is answered late is sent again.
func (s *dnsServer) queries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.asked)
	slices.Sort(out)
	return slices.Compact(out)
}

func rr(name string, body dnsmessage.ResourceBody) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Class: dnsmessage.ClassINET, TTL: 60},
		Body:   body,
	}
}

func a4(s string) *dnsmessage.AResource {
	return &dnsmessage.AResource{A: netip.MustParseAddr(s).As4()}
}

func a6(s string) *dnsmessage.AAAAResource {
	return &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr(s).As16()}
}

func lookup(t *testing.T, servers []netip.AddrPort, network, host string) ([]netip.Addr, error) {
	t.Helper()
	r := &DNSResolver{Servers: servers, Timeout: 2 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.LookupNetIP(ctx, network, host)
}

// TestDNSResolverIgnoresHostsFile: the guest's /etc/hosts maps allowlisted
// names to an address that leads back to the proxy. "localhost" is in every
// hosts file, so a resolver that consulted it would return loopback without
// asking the server.
func TestDNSResolverIgnoresHostsFile(t *testing.T) {
	s := startDNS(t, func(q dnsmessage.Question, _ bool) (dnsmessage.Header, []dnsmessage.Resource) {
		if q.Type == dnsmessage.TypeA {
			return dnsmessage.Header{}, []dnsmessage.Resource{rr(q.Name.String(), a4("203.0.113.5"))}
		}
		return dnsmessage.Header{}, nil
	})
	got, err := lookup(t, []netip.AddrPort{s.addr}, "ip", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	if want := []netip.Addr{netip.MustParseAddr("203.0.113.5")}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if want := []string{"localhost. TypeA", "localhost. TypeAAAA"}; !slices.Equal(s.queries(), want) {
		t.Errorf("server saw %q, want %q", s.queries(), want)
	}
}

func TestDNSResolverFollowsCNAMEChainOnly(t *testing.T) {
	s := startDNS(t, func(q dnsmessage.Question, _ bool) (dnsmessage.Header, []dnsmessage.Resource) {
		return dnsmessage.Header{}, []dnsmessage.Resource{
			rr("edge.test.", a4("203.0.113.7")),
			rr("api.test.", &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName("edge.test.")}),
			rr("unrelated.test.", a4("203.0.113.66")),
		}
	})
	got, err := lookup(t, []netip.AddrPort{s.addr}, "ip4", "API.test")
	if err != nil {
		t.Fatal(err)
	}
	if want := []netip.Addr{netip.MustParseAddr("203.0.113.7")}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestDNSResolverCNAMEChains: a chain of several hops is followed whatever
// the order of its records, and a chain that does not start at the queried
// name yields nothing.
func TestDNSResolverCNAMEChains(t *testing.T) {
	cname := func(from, to string) dnsmessage.Resource {
		return rr(from, &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(to)})
	}
	for _, tc := range []struct {
		name    string
		answers []dnsmessage.Resource
		want    []netip.Addr
	}{
		{"three hops, last hop first", []dnsmessage.Resource{
			rr("d.test.", a4("203.0.113.8")),
			cname("c.test.", "d.test."),
			cname("b.test.", "c.test."),
			cname("a.test.", "b.test."),
		}, []netip.Addr{netip.MustParseAddr("203.0.113.8")}},
		{"chain from another name", []dnsmessage.Resource{
			rr("d.test.", a4("203.0.113.8")),
			cname("y.test.", "d.test."),
			cname("x.test.", "y.test."),
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startDNS(t, func(dnsmessage.Question, bool) (dnsmessage.Header, []dnsmessage.Resource) {
				return dnsmessage.Header{}, tc.answers
			})
			got, err := lookup(t, []netip.AddrPort{s.addr}, "ip4", "a.test")
			if tc.want == nil {
				var de *net.DNSError
				if !errors.As(err, &de) || !de.IsNotFound {
					t.Fatalf("got %v, %v; want a not-found DNSError", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDNSResolverNXDomain(t *testing.T) {
	s := startDNS(t, func(dnsmessage.Question, bool) (dnsmessage.Header, []dnsmessage.Resource) {
		return dnsmessage.Header{RCode: dnsmessage.RCodeNameError}, nil
	})
	_, err := lookup(t, []netip.AddrPort{s.addr}, "ip", "missing.test")
	var de *net.DNSError
	if !errors.As(err, &de) || !de.IsNotFound {
		t.Fatalf("err = %v; want a not-found DNSError", err)
	}
}

// TestDNSResolverNXDomainOutranksFailure: with no addresses, a not-found
// answer for one type is the result even when the other type failed.
func TestDNSResolverNXDomainOutranksFailure(t *testing.T) {
	for _, nx := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		t.Run(nx.String(), func(t *testing.T) {
			s := startDNS(t, func(q dnsmessage.Question, _ bool) (dnsmessage.Header, []dnsmessage.Resource) {
				if q.Type == nx {
					return dnsmessage.Header{RCode: dnsmessage.RCodeNameError}, nil
				}
				return dnsmessage.Header{RCode: dnsmessage.RCodeServerFailure}, nil
			})
			_, err := lookup(t, []netip.AddrPort{s.addr}, "ip", "missing.test")
			var de *net.DNSError
			if !errors.As(err, &de) || !de.IsNotFound {
				t.Fatalf("err = %v; want a not-found DNSError", err)
			}
		})
	}
}

func TestDNSResolverKeepsAddressesWhenOneTypeFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rcode   dnsmessage.RCode
		checkV6 func(error) bool
	}{
		{"server failure", dnsmessage.RCodeServerFailure, func(err error) bool {
			return err != nil && strings.Contains(err.Error(), "ServerFailure")
		}},
		{"name error", dnsmessage.RCodeNameError, func(err error) bool {
			var de *net.DNSError
			return errors.As(err, &de) && de.IsNotFound
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startDNS(t, func(q dnsmessage.Question, _ bool) (dnsmessage.Header, []dnsmessage.Resource) {
				if q.Type == dnsmessage.TypeAAAA {
					return dnsmessage.Header{RCode: tc.rcode}, nil
				}
				return dnsmessage.Header{}, []dnsmessage.Resource{rr(q.Name.String(), a4("203.0.113.6"))}
			})
			got, err := lookup(t, []netip.AddrPort{s.addr}, "ip", "v4only.test")
			if err != nil {
				t.Fatal(err)
			}
			if want := []netip.Addr{netip.MustParseAddr("203.0.113.6")}; !slices.Equal(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
			if _, err := lookup(t, []netip.AddrPort{s.addr}, "ip6", "v4only.test"); !tc.checkV6(err) {
				t.Errorf("AAAA only: err = %v; want the AAAA failure", err)
			}
		})
	}
}

// TestDNSResolverNoAddress: a name that exists but has no A or AAAA records
// is not found, not an empty success.
func TestDNSResolverNoAddress(t *testing.T) {
	s := startDNS(t, func(dnsmessage.Question, bool) (dnsmessage.Header, []dnsmessage.Resource) {
		return dnsmessage.Header{}, nil
	})
	got, err := lookup(t, []netip.AddrPort{s.addr}, "ip", "empty.test")
	var de *net.DNSError
	if !errors.As(err, &de) || !de.IsNotFound {
		t.Fatalf("got %v, %v; want a not-found DNSError", got, err)
	}
}

func TestDNSResolverRetriesTruncatedOverTCP(t *testing.T) {
	s := startDNS(t, func(q dnsmessage.Question, tcp bool) (dnsmessage.Header, []dnsmessage.Resource) {
		if !tcp {
			return dnsmessage.Header{Truncated: true}, nil
		}
		return dnsmessage.Header{}, []dnsmessage.Resource{rr(q.Name.String(), a4("203.0.113.8"))}
	})
	got, err := lookup(t, []netip.AddrPort{s.addr}, "ip4", "big.test")
	if err != nil {
		t.Fatal(err)
	}
	if want := []netip.Addr{netip.MustParseAddr("203.0.113.8")}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A TCP answer that is itself truncated is incomplete; the next server is
// asked instead of returning no addresses.
func TestDNSResolverTruncatedOverTCPTriesNextServer(t *testing.T) {
	truncated := startDNS(t, func(dnsmessage.Question, bool) (dnsmessage.Header, []dnsmessage.Resource) {
		return dnsmessage.Header{Truncated: true}, nil
	})
	good := startDNS(t, func(q dnsmessage.Question, _ bool) (dnsmessage.Header, []dnsmessage.Resource) {
		return dnsmessage.Header{}, []dnsmessage.Resource{rr(q.Name.String(), a4("203.0.113.12"))}
	})
	got, err := lookup(t, []netip.AddrPort{truncated.addr, good.addr}, "ip4", "big.test")
	if err != nil {
		t.Fatal(err)
	}
	if want := []netip.Addr{netip.MustParseAddr("203.0.113.12")}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if want := []string{"big.test. TypeA"}; !slices.Equal(truncated.queries(), want) {
		t.Errorf("truncating server saw %q, want %q", truncated.queries(), want)
	}
	truncated.udp.checkSpaced(t)
	truncated.mu.Lock()
	defer truncated.mu.Unlock()
	if truncated.tcpAsked != 1 {
		t.Errorf("truncating server saw %d TCP queries, want 1 after the truncated UDP answer", truncated.tcpAsked)
	}
}

// The server answers every query with both record types; each lookup keeps
// only the types its network asks for.
func TestDNSResolverMixedAnswers(t *testing.T) {
	s := startDNS(t, func(q dnsmessage.Question, _ bool) (dnsmessage.Header, []dnsmessage.Resource) {
		return dnsmessage.Header{}, []dnsmessage.Resource{
			rr(q.Name.String(), a6("2001:db8::7")),
			rr(q.Name.String(), a4("203.0.113.7")),
		}
	})
	v4, v6 := netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8::7")
	for _, tt := range []struct {
		network string
		want    []netip.Addr
	}{
		{"ip", []netip.Addr{v4, v6}},
		{"ip4", []netip.Addr{v4}},
		{"ip6", []netip.Addr{v6}},
	} {
		got, err := lookup(t, []netip.AddrPort{s.addr}, tt.network, "dual.test")
		if err != nil {
			t.Fatalf("%s: %v", tt.network, err)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.network, got, tt.want)
		}
	}
}

// TestDNSResolverReadsLargeUDPAnswer: a UDP answer larger than the
// EDNS-sized 1232 bytes, sent without the truncated bit, is read whole.
func TestDNSResolverReadsLargeUDPAnswer(t *testing.T) {
	var want []netip.Addr
	var records []dnsmessage.Resource
	for i := range 100 {
		a := netip.AddrFrom4([4]byte{203, 0, 113, byte(i)})
		want = append(want, a)
		records = append(records, rr("many.test.", &dnsmessage.AResource{A: a.As4()}))
	}
	s := startDNS(t, func(q dnsmessage.Question, tcp bool) (dnsmessage.Header, []dnsmessage.Resource) {
		if tcp {
			t.Error("asked over TCP; the UDP answer was not truncated")
		}
		return dnsmessage.Header{}, records
	})
	got, err := lookup(t, []netip.AddrPort{s.addr}, "ip4", "many.test")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %d addresses, want %d", len(got), len(want))
	}
}

// TestDNSResolverIgnoresStrayResponses: a UDP datagram that does not
// answer the query, by ID or by question, is skipped, and the answer that
// follows it is used.
func TestDNSResolverIgnoresStrayResponses(t *testing.T) {
	s := serveDNS(t, &dnsServer{stray: true, answer: func(q dnsmessage.Question, _ bool) (dnsmessage.Header, []dnsmessage.Resource) {
		return dnsmessage.Header{}, []dnsmessage.Resource{rr(q.Name.String(), a4("203.0.113.9"))}
	}})
	got, err := lookup(t, []netip.AddrPort{s.addr}, "ip4", "x.test")
	if err != nil {
		t.Fatal(err)
	}
	if want := []netip.Addr{netip.MustParseAddr("203.0.113.9")}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestDNSResolverMismatchedServerTimesOut: a server whose every response
// carries the wrong ID never answers, so its query times out and the next
// server is asked.
func TestDNSResolverMismatchedServerTimesOut(t *testing.T) {
	answer := func(q dnsmessage.Question, _ bool) (dnsmessage.Header, []dnsmessage.Resource) {
		return dnsmessage.Header{}, []dnsmessage.Resource{rr(q.Name.String(), a4("203.0.113.9"))}
	}
	bad := serveDNS(t, &dnsServer{answer: answer, badID: true})
	good := startDNS(t, answer)
	r := &DNSResolver{Servers: []netip.AddrPort{bad.addr}, Timeout: 200 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.LookupNetIP(ctx, "ip4", "x.test"); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err = %v; want the per-query timeout", err)
	}
	r.Servers = []netip.AddrPort{bad.addr, good.addr}
	got, err := r.LookupNetIP(ctx, "ip4", "x.test")
	if err != nil || len(got) != 1 {
		t.Fatalf("with a second server: %v, %v", got, err)
	}
}

// TestDNSResolverResendsLostQuery: a query that the network drops is sent
// again, so a server that only starts hearing datagrams after about two
// seconds still answers within the timeout.
func TestDNSResolverResendsLostQuery(t *testing.T) {
	s := serveDNS(t, &dnsServer{dropFirst: 4, answer: func(q dnsmessage.Question, _ bool) (dnsmessage.Header, []dnsmessage.Resource) {
		return dnsmessage.Header{}, []dnsmessage.Resource{rr(q.Name.String(), a4("203.0.113.9"))}
	}})
	r := &DNSResolver{Servers: []netip.AddrPort{s.addr}, Timeout: 5 * time.Second}
	got, err := r.LookupNetIP(context.Background(), "ip4", "x.test")
	if err != nil {
		t.Fatal(err)
	}
	if want := []netip.Addr{netip.MustParseAddr("203.0.113.9")}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	s.udp.checkSpaced(t)
}

// TestDNSResolverSilentServerFailsAtTimeout: resending does not stretch the
// per-query timeout. The timeout falls just after the first resend, so a
// read that ran a whole further interval would end past the upper bound.
func TestDNSResolverSilentServerFailsAtTimeout(t *testing.T) {
	const margin = 50 * time.Millisecond
	timeout := dnsclient.ResendInterval + margin
	r := &DNSResolver{Servers: []netip.AddrPort{silentUDP(t)}, Timeout: timeout}
	start := time.Now()
	_, err := r.LookupNetIP(context.Background(), "ip4", "x.test")
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err = %v; want a timeout", err)
	}
	if d := time.Since(start); d < timeout || d >= 2*dnsclient.ResendInterval-margin {
		t.Errorf("lookup took %v; want the %v timeout", d, timeout)
	}
}

// TestDNSResolverResendsPastStrayDatagrams: strays that keep arriving do not
// postpone the resend, so the query is still answered long before the
// timeout.
func TestDNSResolverResendsPastStrayDatagrams(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var sent arrivals
	done := make(chan struct{})
	t.Cleanup(func() { close(done); pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for n := 1; ; n++ {
			size, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			query := slices.Clone(buf[:size])
			sent.record(t, query)
			switch {
			case n == 1:
			case n == 2:
				go func() {
					for {
						for _, b := range strayResponses(t, query) {
							_, _ = pc.WriteTo(b, from)
						}
						select {
						case <-done:
							return
						case <-time.After(dnsclient.ResendInterval / 5):
						}
					}
				}()
			default:
				var m dnsmessage.Message
				if err := m.Unpack(query); err != nil {
					t.Errorf("fake dns: bad query: %v", err)
					return
				}
				q := m.Questions[0]
				resp, err := (&dnsmessage.Message{
					Header:    dnsmessage.Header{ID: m.ID, Response: true},
					Questions: m.Questions,
					Answers:   []dnsmessage.Resource{rr(q.Name.String(), a4("203.0.113.9"))},
				}).Pack()
				if err != nil {
					t.Errorf("fake dns: pack: %v", err)
					return
				}
				_, _ = pc.WriteTo(resp, from)
			}
		}
	}()
	r := &DNSResolver{Servers: []netip.AddrPort{pc.LocalAddr().(*net.UDPAddr).AddrPort()}, Timeout: 10 * dnsclient.ResendInterval}
	start := time.Now()
	got, err := r.LookupNetIP(context.Background(), "ip4", "x.test")
	if err != nil {
		t.Fatal(err)
	}
	if want := []netip.Addr{netip.MustParseAddr("203.0.113.9")}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if d := time.Since(start); d > 5*dnsclient.ResendInterval {
		t.Errorf("lookup took %v; want it answered by the third send", d)
	}
	sent.checkSpaced(t)
}

// silentUDP listens on loopback and never replies.
func silentUDP(t *testing.T) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).AddrPort()
}

func TestDNSResolverHungServerFallsThrough(t *testing.T) {
	hung := silentUDP(t)
	good := startDNS(t, func(q dnsmessage.Question, _ bool) (dnsmessage.Header, []dnsmessage.Resource) {
		if q.Type == dnsmessage.TypeAAAA {
			return dnsmessage.Header{}, []dnsmessage.Resource{rr(q.Name.String(), a6("2001:db8::7"))}
		}
		return dnsmessage.Header{}, []dnsmessage.Resource{rr(q.Name.String(), a4("203.0.113.7"))}
	})
	r := &DNSResolver{Servers: []netip.AddrPort{hung, good.addr}, Timeout: 200 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := r.LookupNetIP(ctx, "ip", "x.test")
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8::7")}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v (A first, then AAAA)", got, want)
	}
}

func TestDNSResolverSingleHungServerFailsBeforeContextDeadline(t *testing.T) {
	hung := silentUDP(t)
	r := &DNSResolver{Servers: []netip.AddrPort{hung}, Timeout: 200 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := r.LookupNetIP(ctx, "ip", "x.test")
	if err == nil {
		t.Fatal("no error from a server that never replies")
	}
	if ctx.Err() != nil {
		t.Errorf("parent context ended (%v) before the lookup failed: %v", ctx.Err(), err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("lookup took %v; the per-query timeout is 200ms and the types run concurrently", d)
	}
}

func TestChildConfigResolver(t *testing.T) {
	if _, err := (ChildConfig{Allow: []string{"a.test"}}).resolver(); err == nil {
		t.Error("no resolvers: no error")
	}
	if _, err := (ChildConfig{Resolvers: []netip.Addr{netip.MustParseAddr("::ffff:0.0.0.0")}}).resolver(); err == nil {
		t.Error("mapped unspecified resolver: no error")
	}
	if _, err := (ChildConfig{Resolvers: []netip.Addr{netip.MustParseAddr("0.0.0.0")}}).resolver(); err == nil {
		t.Error("unspecified resolver: no error")
	}
	if _, err := (ChildConfig{Resolvers: []netip.Addr{netip.MustParseAddr("fd00::1")}}).resolver(); err == nil {
		t.Error("IPv6 resolver: no error")
	}
	r, err := ChildConfig{Resolvers: []netip.Addr{netip.MustParseAddr("192.168.64.1"), netip.MustParseAddr("::ffff:10.0.0.2")}}.resolver()
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.AddrPort{netip.MustParseAddrPort("192.168.64.1:53"), netip.MustParseAddrPort("10.0.0.2:53")}
	if !slices.Equal(r.Servers, want) {
		t.Errorf("servers %v, want %v", r.Servers, want)
	}
}
