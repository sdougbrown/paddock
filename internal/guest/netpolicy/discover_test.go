package netpolicy

import (
	"net/netip"
	"testing"
)

func TestRouteSourceRejectsLoopback(t *testing.T) {
	if _, err := RouteSource(netip.MustParseAddr("127.0.0.1")); err == nil {
		t.Fatal("RouteSource(127.0.0.1) returned nil; want a loopback rejection")
	}
}

func TestProxyAddrForNeedsResolver(t *testing.T) {
	if _, err := ProxyAddrFor(nil, 443); err == nil {
		t.Fatal("ProxyAddrFor with no resolvers returned nil")
	}
}
