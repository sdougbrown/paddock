// Package guesttest provides test fixtures for the guest network packages:
// a self-signed certificate, a TLS echo server, a fake resolver, and a
// simulated guest network that routes connections the way the allowlist
// ruleset does. Tests only; nothing here touches the real network.
package guesttest

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/guest/sniproxy"
)

// SelfSigned returns a certificate for names and a pool trusting it.
func SelfSigned(t testing.TB, names ...string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// EchoTLS starts a TLS server on loopback that echoes one line per
// connection. It closes when the test ends.
func EchoTLS(t testing.TB, cert tls.Certificate) net.Listener {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadString('\n')
				if err == nil {
					_, _ = c.Write([]byte("echo " + line))
				}
			}()
		}
	}()
	return ln
}

// UDPEcho starts a UDP server on loopback that answers every datagram with
// its own bytes. It closes when the test ends.
func UDPEcho(t testing.TB) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().String()
}

// Resolver maps names to addresses.
type Resolver map[string][]netip.Addr

// LookupNetIP satisfies sniproxy.Resolver and the resolver interfaces of the guest probes.
func (r Resolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	addrs, ok := r[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	var out []netip.Addr
	for _, a := range addrs {
		if network == "ip4" && !a.Is4() || network == "ip6" && !a.Is6() {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// Net simulates the agent's view of the guest network. With Open false it
// follows the allowlist ruleset, which redirects IPv4 only: IPv4 tcp/443
// goes to Proxy, and every other connection, IPv6 tcp/443 included, is
// refused. With Open true (a missing firewall) tcp goes straight to Upstream
// and udp to UDPUpstream.
type Net struct {
	Resolver    Resolver
	Proxy       string
	Upstream    string
	UDPUpstream string
	Open        bool
}

// DialContext satisfies the dialer interfaces of the guest probes and sniproxy.Dialer.
func (n *Net) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	ap, err := n.resolve(ctx, addr)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: err}
	}
	var d net.Dialer
	switch {
	case n.Open && network == "udp":
		return d.DialContext(ctx, "udp", n.UDPUpstream)
	case n.Open:
		return d.DialContext(ctx, "tcp", n.Upstream)
	case network == "tcp" && ap.Port() == 443 && ap.Addr().Is4():
		return d.DialContext(ctx, "tcp", n.Proxy)
	}
	return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
}

func (n *Net) resolve(ctx context.Context, addr string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return netip.AddrPort{}, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(a, uint16(p)), nil
	}
	addrs, err := n.Resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if len(addrs) == 0 {
		return netip.AddrPort{}, errors.New("no address for " + host)
	}
	return netip.AddrPortFrom(addrs[0], uint16(p)), nil
}

// Guest is a running sniproxy behind a simulated network.
type Guest struct {
	Net      *Net
	Resolver Resolver
	DenyLog  string
}

// StartGuest runs a sniproxy allowing allowed (which must resolve in
// resolver) with an upstream echo server presenting a certificate for
// allowed, and returns the simulated network around it. The denial log is a
// file in the test's temp dir.
func StartGuest(t testing.TB, allowed string, resolver Resolver) *Guest {
	t.Helper()
	cert, _ := SelfSigned(t, allowed)
	up := EchoTLS(t, cert)
	allow, err := sniproxy.NewAllowlist([]string{allowed})
	if err != nil {
		t.Fatal(err)
	}
	logPath := t.TempDir() + "/deny.jsonl"
	logFile, err := openAppend(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	upstream := up.Addr().String()
	p, err := sniproxy.New(sniproxy.Config{
		Allowlist: allow,
		Resolver:  resolver,
		Dialer: dialFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", upstream)
		}),
		DenyLog: logFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = p.Serve(ln) }()
	return &Guest{
		Net:      &Net{Resolver: resolver, Proxy: ln.Addr().String(), Upstream: upstream, UDPUpstream: UDPEcho(t)},
		Resolver: resolver,
		DenyLog:  logPath,
	}
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (f dialFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
}
