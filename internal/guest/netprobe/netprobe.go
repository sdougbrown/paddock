// Package netprobe holds connection probes: TLS handshakes with or without
// a certificate check, and a UDP DNS query. Each takes a Dialer so callers
// can substitute the network.
package netprobe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"syscall"

	"golang.org/x/net/dns/dnsmessage"
)

// ErrClosedInHandshake reports that the peer closed the connection during
// the TLS handshake. Behind paddock's firewall that is the SNI proxy
// refusing the connection.
var ErrClosedInHandshake = errors.New("connection closed during TLS handshake")

// Dialer opens TCP connections. *net.Dialer satisfies it.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Handshake connects to addr and completes a TLS handshake, sending
// serverName as SNI (none when empty, as a client dialing an IP literal
// does). It returns nil once the handshake completes and checks nothing
// about the certificate: a probe that expects to be refused counts any
// completed handshake as a connection.
func Handshake(ctx context.Context, d Dialer, addr, serverName string) (tls.ConnectionState, error) {
	if d == nil {
		d = &net.Dialer{}
	}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return tls.ConnectionState{}, fmt.Errorf("connect %s: %w", addr, err)
	}
	defer raw.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}
	c := tls.Client(raw, &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err := c.HandshakeContext(ctx); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isReset(err) {
			return tls.ConnectionState{}, fmt.Errorf("handshake with %s: %w", addr, ErrClosedInHandshake)
		}
		return tls.ConnectionState{}, fmt.Errorf("handshake with %s: %w", addr, err)
	}
	return c.ConnectionState(), nil
}

// TLS is Handshake for a probe that expects to connect: the leaf
// certificate must also name serverName. The chain is not verified: guest
// images may lack CA roots, and the probe proves the network path, not
// trust.
func TLS(ctx context.Context, d Dialer, addr, serverName string) error {
	st, err := Handshake(ctx, d, addr, serverName)
	if err != nil {
		return err
	}
	if len(st.PeerCertificates) == 0 {
		return fmt.Errorf("handshake with %s: no peer certificate", addr)
	}
	if err := st.PeerCertificates[0].VerifyHostname(serverName); err != nil {
		return fmt.Errorf("handshake with %s: %w", addr, err)
	}
	return nil
}

func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// Resolver looks up addresses. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DNSQuery sends a recursive query for name's A record to the DNS server at
// addr over UDP and returns the size of the first datagram that comes back.
// An error means no answer arrived. ctx must carry a deadline: it bounds the
// wait for the answer.
func DNSQuery(ctx context.Context, d Dialer, addr, name string) (int, error) {
	if d == nil {
		d = &net.Dialer{}
	}
	c, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	q, err := dnsQuery(name)
	if err != nil {
		return 0, err
	}
	if _, err := c.Write(q); err != nil {
		return 0, err
	}
	buf := make([]byte, 512)
	return c.Read(buf)
}

// dnsQuery builds a recursive DNS query for name's A record.
func dnsQuery(name string) ([]byte, error) {
	n, err := dnsmessage.NewName(strings.TrimSuffix(name, ".") + ".")
	if err != nil {
		return nil, fmt.Errorf("dns name %q: %w", name, err)
	}
	return (&dnsmessage.Message{
		Header: dnsmessage.Header{ID: 0x7064, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET,
		}},
	}).Pack()
}
