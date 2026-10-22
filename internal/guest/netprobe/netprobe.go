// Package netprobe holds the connection probes shared by the netpolicy
// lifeline and the in-guest verify checks.
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

// LookupIPv4 returns the first IPv4 address host resolves to.
func LookupIPv4(ctx context.Context, r Resolver, host string) (netip.Addr, error) {
	if r == nil {
		r = net.DefaultResolver
	}
	addrs, err := r.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, a := range addrs {
		if a.Unmap().Is4() {
			return a.Unmap(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("resolve %s: no IPv4 address", host)
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
	if _, err := c.Write(dnsQuery(name)); err != nil {
		return 0, err
	}
	buf := make([]byte, 512)
	return c.Read(buf)
}

// dnsQuery builds a recursive DNS query for name's A record.
func dnsQuery(name string) []byte {
	q := []byte{0x70, 0x64, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for _, label := range strings.Split(name, ".") {
		q = append(q, byte(len(label)))
		q = append(q, label...)
	}
	return append(q, 0x00, 0x00, 0x01, 0x00, 0x01)
}
