// Package dnsclient sends one packed DNS query and reads one response. It is
// a leaf package shared by the SNI proxy's resolver and the network probes,
// so both use the same wire handling.
package dnsclient

import (
	"context"
	"encoding/binary"
	"io"
	"net"
)

// Dialer opens connections. *net.Dialer satisfies it.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Exchange sends query to server over network ("udp" or "tcp") and returns
// the response. Over TCP both carry the two-byte length prefix. Over UDP, as
// in the net package's resolver, a datagram that matches rejects is skipped:
// a stray or forged datagram does not fail the query, which waits for its
// answer until ctx's deadline. A nil matches accepts the first datagram.
func Exchange(ctx context.Context, d Dialer, network, server string, query []byte, matches func([]byte) bool) ([]byte, error) {
	c, err := d.DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if network == "udp" {
		if _, err := c.Write(query); err != nil {
			return nil, err
		}
		buf := make([]byte, 1232)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return nil, err
			}
			if matches == nil || matches(buf[:n]) {
				return buf[:n], nil
			}
		}
	}
	framed := binary.BigEndian.AppendUint16(nil, uint16(len(query)))
	if _, err := c.Write(append(framed, query...)); err != nil {
		return nil, err
	}
	var lb [2]byte
	if _, err := io.ReadFull(c, lb[:]); err != nil {
		return nil, err
	}
	buf := make([]byte, binary.BigEndian.Uint16(lb[:]))
	if _, err := io.ReadFull(c, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
