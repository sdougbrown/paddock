// Package sniproxy is a transparent TLS egress proxy that filters on the
// ClientHello server name.
//
// The guest firewall DNATs tcp/443 to the proxy. For each connection the
// proxy reads the ClientHello without answering it, takes the SNI host name,
// and checks it against the allowlist. An allowed name is resolved by the
// proxy itself (in the guest, by a DNSResolver that queries the pinned
// resolvers and never reads /etc/hosts) and dialed on the upstream port; only
// allowlisted names are ever looked up. The client's original
// destination address is never used, so a client cannot pair an allowed SNI
// with an arbitrary IP. The buffered ClientHello is replayed upstream and
// the two connections are spliced without decryption.
//
// A connection that sends no SNI, sends something other than a TLS
// handshake, names a host that is not allowlisted, or does not finish its
// ClientHello within HelloTimeout is closed and recorded as one JSON line in
// the denial log. The denial is written before the client connection
// closes.
//
// The ClientHello is parsed strictly: every length-prefixed field must be
// consumed exactly, no extension may appear twice, and there must be one
// host_name, so the proxy and the upstream cannot read different names.
// A ClientHello carrying encrypted_client_hello (ECH) is refused with
// ReasonECH, because ECH hides the name the client is really asking for
// behind an outer name the proxy would check instead. Chromium sends
// ECH-GREASE by default, so a Chromium-based browser (including headless
// Chrome under Playwright) is refused under the allowlist policy; browsers
// belong on an open-egress profile. curl and the Go, Node, and Python TLS
// stacks do not send ECH unless configured to.
//
// An allowed name's addresses are filtered before dialing: loopback,
// link-local, multicast, private (RFC 1918, ULA), shared (RFC 6598), and
// the pinned resolvers' addresses are never dialed, so a name whose DNS
// points inside the guest's network cannot steer the proxy there.
//
// The only timeouts are the ClientHello read, the upstream dial, and the
// ClientHello replay to the upstream. A spliced connection has no idle
// timeout: an API response stream can stay quiet for long stretches and must
// not be cut.
package sniproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"
)

// Reason classifies a refused connection in the denial log.
type Reason string

// Denial reasons.
const (
	ReasonNotTLS    Reason = "not-tls"   // first record is not a TLS handshake
	ReasonNoSNI     Reason = "no-sni"    // ClientHello without a server_name host name
	ReasonMalformed Reason = "malformed" // unparseable record or ClientHello
	ReasonDenied    Reason = "denied"    // server name not allowlisted
	ReasonECH       Reason = "ech"       // ClientHello carries encrypted_client_hello
	ReasonTimeout   Reason = "timeout"   // ClientHello not complete within HelloTimeout
	ReasonTruncated Reason = "truncated" // client closed before a complete ClientHello
	ReasonUpstream  Reason = "upstream"  // allowed name, but no usable address or the dial failed
)

// Denial is one denial log line.
type Denial struct {
	Time   time.Time `json:"time"`
	SNI    string    `json:"sni"`
	Reason Reason    `json:"reason"`
	Peer   string    `json:"peer"`
}

// Resolver looks up addresses. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Dialer opens upstream connections. *net.Dialer satisfies it.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Config configures a Proxy. Zero values take the documented defaults.
type Config struct {
	Allowlist *Allowlist // required
	// Resolver is required. In the guest it is a DNSResolver: the system
	// resolver reads /etc/hosts, which maps allowlisted names back to the
	// proxy.
	Resolver Resolver
	Dialer   Dialer // default a *net.Dialer with TCP keep-alive
	// ExcludeAddrs are never dialed, in addition to the address classes
	// the package doc lists; in the guest, the pinned resolvers.
	ExcludeAddrs []netip.Addr
	// UpstreamPort is the port dialed on the resolved address; default 443.
	UpstreamPort uint16
	// HelloTimeout bounds reading the ClientHello; default 5s.
	HelloTimeout time.Duration
	// DialTimeout bounds resolving and dialing upstream, and separately the
	// ClientHello replay once connected; default 10s.
	// Each address gets an equal share of what remains after the lookup,
	// so one address that drops SYNs cannot use up the whole budget.
	DialTimeout time.Duration
	// DenyLog receives one JSON Denial per line; nil discards.
	DenyLog io.Writer
	// ErrorLog receives operational messages; nil discards.
	ErrorLog *log.Logger
	// Now is the clock for denial timestamps only; default time.Now.
	Now func() time.Time
}

// Proxy is an SNI-filtering TLS proxy.
type Proxy struct {
	cfg   Config
	logMu sync.Mutex
}

// New validates cfg and fills defaults.
func New(cfg Config) (*Proxy, error) {
	if cfg.Allowlist == nil {
		return nil, errors.New("sniproxy: config needs an allowlist")
	}
	if cfg.Resolver == nil {
		return nil, errors.New("sniproxy: config needs a resolver")
	}
	if cfg.Dialer == nil {
		cfg.Dialer = &net.Dialer{KeepAlive: 30 * time.Second}
	}
	if cfg.UpstreamPort == 0 {
		cfg.UpstreamPort = 443
	}
	if cfg.HelloTimeout <= 0 {
		cfg.HelloTimeout = 5 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.DenyLog == nil {
		cfg.DenyLog = io.Discard
	}
	if cfg.ErrorLog == nil {
		cfg.ErrorLog = log.New(io.Discard, "", 0)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Proxy{cfg: cfg}, nil
}

// Serve accepts connections on ln until ln is closed, handling each in its
// own goroutine. It returns nil once ln is closed.
func (p *Proxy) Serve(ln net.Listener) error {
	var backoff time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			p.cfg.ErrorLog.Printf("accept: %v; retrying in %v", err, backoff)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		go p.handle(c)
	}
}

func (p *Proxy) handle(client net.Conn) {
	defer client.Close()
	peer := client.RemoteAddr().String()

	_ = client.SetReadDeadline(time.Now().Add(p.cfg.HelloTimeout))
	hello, sni, err := readClientHello(client)
	if err != nil {
		var he *helloError
		if errors.As(err, &he) {
			p.deny(peer, sni, he.reason, he.detail)
		} else {
			p.deny(peer, sni, ReasonMalformed, err.Error())
		}
		return
	}
	if !p.cfg.Allowlist.Allows(sni) {
		p.deny(peer, sni, ReasonDenied, "not in allowlist")
		return
	}
	_ = client.SetReadDeadline(time.Time{})

	upstream, err := p.dialUpstream(sni)
	if err != nil {
		p.deny(peer, sni, ReasonUpstream, err.Error())
		return
	}
	defer upstream.Close()
	_ = upstream.SetWriteDeadline(time.Now().Add(p.cfg.DialTimeout))
	if _, err := upstream.Write(hello); err != nil {
		p.cfg.ErrorLog.Printf("%s %s: replay ClientHello: %v", peer, sni, err)
		return
	}
	_ = upstream.SetWriteDeadline(time.Time{})
	splice(client, upstream)
}

// dialUpstream resolves name and dials the first usable address that
// answers, IPv4 addresses first. Each address gets an equal share of the
// time left before the DialTimeout deadline.
func (p *Proxy) dialUpstream(name string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.DialTimeout)
	defer cancel()
	addrs, err := p.cfg.Resolver.LookupNetIP(ctx, "ip", name)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", name, err)
	}
	usable := usableAddrs(addrs, p.cfg.ExcludeAddrs)
	if len(usable) == 0 {
		return nil, fmt.Errorf("resolve %s: no usable address in %v", name, addrs)
	}
	deadline, _ := ctx.Deadline()
	var errs []error
	for i, a := range usable {
		share := time.Until(deadline) / time.Duration(len(usable)-i)
		actx, acancel := context.WithTimeout(ctx, share)
		c, err := p.cfg.Dialer.DialContext(actx, "tcp", netip.AddrPortFrom(a, p.cfg.UpstreamPort).String())
		acancel()
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// sharedAddrSpace is RFC 6598's carrier-grade NAT range, which overlay
// networks such as Tailscale also use.
var sharedAddrSpace = netip.MustParsePrefix("100.64.0.0/10")

// usableAddrs drops addresses an allowlisted name must never steer the
// proxy to (see the package doc) and orders IPv4 before IPv6.
func usableAddrs(addrs, exclude []netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() ||
			a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() ||
			a.IsPrivate() || sharedAddrSpace.Contains(a) || excluded(a, exclude) {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Is4() && !out[j].Is4() })
	return out
}

func excluded(a netip.Addr, exclude []netip.Addr) bool {
	for _, e := range exclude {
		if e.Unmap().WithZone("") == a.WithZone("") {
			return true
		}
	}
	return false
}

func (p *Proxy) deny(peer, sni string, r Reason, detail string) {
	b, err := json.Marshal(Denial{Time: p.cfg.Now().UTC(), SNI: sni, Reason: r, Peer: peer})
	if err == nil {
		b = append(b, '\n')
		p.logMu.Lock()
		_, err = p.cfg.DenyLog.Write(b)
		p.logMu.Unlock()
	}
	if err != nil {
		p.cfg.ErrorLog.Printf("write denial log: %v", err)
	}
	p.cfg.ErrorLog.Printf("deny %s sni=%q reason=%s: %s", peer, sni, r, detail)
}

// splice copies both directions until each side has finished sending. A
// clean end of one direction half-closes the other side's write; an error
// in either direction closes both connections.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			a.Close()
			b.Close()
		})
	}
	pump := func(dst, src net.Conn) {
		defer wg.Done()
		if _, err := io.Copy(dst, src); err != nil {
			closeBoth()
			return
		}
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			if cw.CloseWrite() == nil {
				return
			}
		}
		closeBoth()
	}
	wg.Add(2)
	go pump(b, a)
	go pump(a, b)
	wg.Wait()
}
