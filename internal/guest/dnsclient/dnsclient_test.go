package dnsclient

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// A cancelled ctx ends a UDP exchange at once, not at the next resend, and
// is reported as a cancellation.
func TestExchangeUDPStopsWhenCancelled(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	received := make(chan struct{})
	go func() {
		buf := make([]byte, 1500)
		if _, _, err := pc.ReadFrom(buf); err == nil {
			close(received)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-received
		cancel()
	}()
	start := time.Now()
	_, err = Exchange(ctx, &net.Dialer{}, "udp", pc.LocalAddr().String(), []byte("q"), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
	if d := time.Since(start); d > ResendInterval/2 {
		t.Errorf("Exchange took %v to notice the cancellation; the resend interval is %v", d, ResendInterval)
	}
}
