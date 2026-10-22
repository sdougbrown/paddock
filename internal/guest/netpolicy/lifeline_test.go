package netpolicy

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/guest/guesttest"
	"github.com/sdougbrown/paddock/internal/guest/netprobe"
)

// testResolvers stands in for the pinned resolver; the simulated network
// answers it only with the firewall open.
var testResolvers = []netip.Addr{netip.MustParseAddr("198.51.100.53")}

func TestLifeline(t *testing.T) {
	// The allowed host resolves the way /etc/hosts makes it resolve.
	resolver := guesttest.Resolver{"allowed.test": {HostsAddr}}
	tests := []struct {
		name    string
		open    bool
		proxy   string // overrides the proxy address
		allowed string
		want    []string
	}{
		{name: "enforced", allowed: "allowed.test"},
		{name: "open firewall", open: true, allowed: "allowed.test", want: []string{
			"denied host denied.test connected", "raw-IP connection", "DNS query to 198.51.100.53:53 was answered"}},
		{name: "proxy down", proxy: "127.0.0.1:1", allowed: "allowed.test", want: []string{"allowed host allowed.test did not connect"}},
		{name: "allowed host unresolvable", allowed: "missing.test", want: []string{"allowed host missing.test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := guesttest.StartGuest(t, "allowed.test", resolver)
			g.Net.Open = tt.open
			if tt.proxy != "" {
				g.Net.Proxy = tt.proxy
			}
			err := Lifeline{
				AllowedHost: tt.allowed,
				DeniedHost:  "denied.test",
				Resolvers:   testResolvers,
				Timeout:     3 * time.Second,
				Dialer:      g.Net,
			}.Check(context.Background())
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("lifeline passed")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestLifelineNeedsResolvers(t *testing.T) {
	err := Lifeline{AllowedHost: "a.test", DeniedHost: "b.test"}.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pinned resolvers") {
		t.Fatalf("err = %v; want a missing-resolvers error", err)
	}
}

func TestLifelineRefusedAllowedHostIsDiagnosable(t *testing.T) {
	resolver := guesttest.Resolver{"allowed.test": {HostsAddr}, "other.test": {HostsAddr}}
	// The proxy allows only other.test, so allowed.test is refused by the
	// proxy rather than failing to connect.
	g := guesttest.StartGuest(t, "other.test", resolver)
	err := Lifeline{AllowedHost: "allowed.test", DeniedHost: "other.test", Resolvers: testResolvers, Timeout: 3 * time.Second, Dialer: g.Net}.Check(context.Background())
	if !errors.Is(err, netprobe.ErrClosedInHandshake) || !strings.Contains(err.Error(), "proxy uid") {
		t.Fatalf("err = %v; want a closed-in-handshake error naming the proxy uid", err)
	}
}
