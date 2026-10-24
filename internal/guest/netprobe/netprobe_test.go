package netprobe_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/guest/guesttest"
	"github.com/sdougbrown/paddock/internal/guest/netprobe"
)

func TestTLSRejectsHostnameMismatch(t *testing.T) {
	cert, _ := guesttest.SelfSigned(t, "other.test")
	ln := guesttest.EchoTLS(t, cert)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := netprobe.TLS(ctx, nil, ln.Addr().String(), "allowed.test")
	if err == nil || !strings.Contains(err.Error(), "allowed.test") {
		t.Fatalf("err = %v; want a hostname mismatch naming allowed.test", err)
	}
}

// A proxy that refuses by resetting the connection mid-handshake is a
// closed handshake too, like one that closes it cleanly.
func TestHandshakeResetIsClosedInHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Read(make([]byte, 512))
		// Linger 0 makes Close send a RST instead of a FIN.
		_ = c.(*net.TCPConn).SetLinger(0)
		c.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = netprobe.Handshake(ctx, nil, ln.Addr().String(), "denied.test")
	if !errors.Is(err, netprobe.ErrClosedInHandshake) {
		t.Fatalf("err = %v; want ErrClosedInHandshake", err)
	}
}
