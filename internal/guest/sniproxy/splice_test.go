package sniproxy

import (
	"net"
	"testing"
	"time"
)

// tcpPair returns the two ends of a loopback TCP connection.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	local, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		local.Close()
		remote.Close()
	})
	return local, remote
}

// An error in one direction closes both connections, so splice returns even
// while the other side keeps its sending side open.
func TestSpliceErrorClosesBoth(t *testing.T) {
	client, clientPeer := tcpPair(t)
	upstream, upstreamPeer := tcpPair(t)
	done := make(chan struct{})
	go func() {
		splice(clientPeer, upstream)
		close(done)
	}()
	// Reset the upstream: closing with linger 0 sends an RST.
	if err := upstreamPeer.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	upstreamPeer.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("splice did not return after the upstream reset")
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("client connection still open after splice returned")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("client connection still open after splice returned")
	}
}
