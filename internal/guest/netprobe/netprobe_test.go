package netprobe_test

import (
	"context"
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
