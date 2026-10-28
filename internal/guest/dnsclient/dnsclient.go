// Package dnsclient sends one packed DNS query and reads one response. It is
// a leaf package shared by the SNI proxy's resolver and the network probes,
// so both use the same wire handling.
package dnsclient

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"time"
)

// ResendInterval is how long Exchange waits for a UDP answer before it sends
// the query again. A guest's network drops datagrams for the first seconds
// after it boots, so a query sent once can be lost for good.
const ResendInterval = 500 * time.Millisecond

// Dialer opens connections. *net.Dialer satisfies it.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Exchange sends query to server over network ("udp" or "tcp") and returns
// the response. Over TCP both carry the two-byte length prefix. Over UDP, as
// in the net package's resolver, a datagram that matches rejects is skipped:
// a stray or forged datagram does not fail the query, which waits for its
// answer until ctx's deadline. The query is sent again every ResendInterval
// until an answer arrives. A nil matches accepts the first datagram.
func Exchange(ctx context.Context, d Dialer, network, server string, query []byte, matches func([]byte) bool) ([]byte, error) {
	c, err := d.DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	dl, hasDeadline := ctx.Deadline()
	if hasDeadline {
		_ = c.SetDeadline(dl)
	}
	if network == "udp" {
		// A datagram longer than buf is cut short without an error, so buf
		// holds the largest DNS message.
		buf := make([]byte, 65535)
		for {
			if _, err := c.Write(query); err != nil {
				return nil, err
			}
			resendAt := time.Now().Add(ResendInterval)
			if hasDeadline && dl.Before(resendAt) {
				resendAt = dl
			}
			_ = c.SetReadDeadline(resendAt)
			for {
				n, err := c.Read(buf)
				if errors.Is(err, os.ErrDeadlineExceeded) && (!hasDeadline || time.Now().Before(dl)) {
					break
				}
				if err != nil {
					return nil, err
				}
				if matches == nil || matches(buf[:n]) {
					return buf[:n], nil
				}
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
