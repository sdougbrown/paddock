package sniproxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/sdougbrown/paddock/internal/egresshost"
)

const (
	recordTypeHandshake      = 0x16
	handshakeTypeClientHello = 0x01
	extensionServerName      = 0x0000
	serverNameTypeHostName   = 0x00

	recordHeaderLen = 5
	maxRecordLen    = 1 << 14 // plaintext TLS record limit
	maxHelloLen     = 1 << 16 // cap on a ClientHello reassembled across records
)

// helloError is a refusal to proxy, with the reason logged to the denial log.
type helloError struct {
	reason Reason
	detail string
}

func (e *helloError) Error() string { return string(e.reason) + ": " + e.detail }

func refuse(r Reason, format string, args ...any) error {
	return &helloError{reason: r, detail: fmt.Sprintf(format, args...)}
}

// readClientHello reads TLS records from r until it holds one complete
// ClientHello, reassembling it if the client split it across records. It
// reads exactly the records it needs and nothing after them. It returns
// every byte read, for replay to the upstream, and the SNI host name,
// lowercased without a trailing dot. Errors are *helloError.
func readClientHello(r io.Reader) (raw []byte, sni string, err error) {
	var hs []byte
	for {
		var hdr [recordHeaderLen]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return raw, "", ioRefusal(err, len(raw) == 0)
		}
		raw = append(raw, hdr[:]...)
		if hdr[0] != recordTypeHandshake || hdr[1] != 0x03 {
			return raw, "", refuse(ReasonNotTLS, "record starts %#02x %#02x", hdr[0], hdr[1])
		}
		n := int(binary.BigEndian.Uint16(hdr[3:]))
		if n == 0 || n > maxRecordLen {
			return raw, "", refuse(ReasonMalformed, "record length %d", n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return raw, "", ioRefusal(err, false)
		}
		raw = append(raw, body...)
		hs = append(hs, body...)
		if len(hs) < 4 {
			continue
		}
		if hs[0] != handshakeTypeClientHello {
			return raw, "", refuse(ReasonMalformed, "handshake type %d is not ClientHello", hs[0])
		}
		msgLen := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
		if msgLen > maxHelloLen {
			return raw, "", refuse(ReasonMalformed, "ClientHello length %d over %d", msgLen, maxHelloLen)
		}
		if len(hs) < 4+msgLen {
			continue
		}
		sni, err := parseSNI(hs[4 : 4+msgLen])
		return raw, sni, err
	}
}

func ioRefusal(err error, empty bool) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return refuse(ReasonTimeout, "ClientHello not received in time")
	}
	if empty && errors.Is(err, io.EOF) {
		return refuse(ReasonTruncated, "connection closed before any data")
	}
	return refuse(ReasonTruncated, "connection ended inside the ClientHello: %v", err)
}

// cursor reads big-endian fields from a byte slice; any out-of-range read
// marks it bad and returns zero values from then on.
type cursor struct {
	b   []byte
	bad bool
}

func (c *cursor) take(n int) []byte {
	if c.bad || n < 0 || n > len(c.b) {
		c.bad = true
		return nil
	}
	out := c.b[:n]
	c.b = c.b[n:]
	return out
}

func (c *cursor) u8() int {
	b := c.take(1)
	if b == nil {
		return 0
	}
	return int(b[0])
}

func (c *cursor) u16() int {
	b := c.take(2)
	if b == nil {
		return 0
	}
	return int(binary.BigEndian.Uint16(b))
}

// parseSNI returns the first host_name in the server_name extension of a
// ClientHello body (the handshake message without its 4-byte header).
func parseSNI(body []byte) (string, error) {
	c := &cursor{b: body}
	c.take(2)  // legacy_version
	c.take(32) // random
	c.take(c.u8())
	c.take(c.u16()) // cipher_suites
	c.take(c.u8())  // compression_methods
	if c.bad {
		return "", refuse(ReasonMalformed, "ClientHello truncated before extensions")
	}
	if len(c.b) == 0 {
		return "", refuse(ReasonNoSNI, "ClientHello has no extensions")
	}
	exts := &cursor{b: c.take(c.u16())}
	if c.bad {
		return "", refuse(ReasonMalformed, "extensions length exceeds ClientHello")
	}
	for len(exts.b) > 0 {
		typ := exts.u16()
		data := exts.take(exts.u16())
		if exts.bad {
			return "", refuse(ReasonMalformed, "extension length exceeds extensions block")
		}
		if typ != extensionServerName {
			continue
		}
		sn := &cursor{b: data}
		list := &cursor{b: sn.take(sn.u16())}
		if sn.bad {
			return "", refuse(ReasonMalformed, "server_name list length")
		}
		for len(list.b) > 0 {
			nameType := list.u8()
			name := list.take(list.u16())
			if list.bad {
				return "", refuse(ReasonMalformed, "server_name entry length")
			}
			if nameType != serverNameTypeHostName {
				continue
			}
			host := egresshost.Normalize(string(name))
			if !egresshost.Valid(host) {
				return "", refuse(ReasonMalformed, "server_name is not a hostname")
			}
			return host, nil
		}
		return "", refuse(ReasonNoSNI, "server_name extension has no host_name")
	}
	return "", refuse(ReasonNoSNI, "ClientHello has no server_name extension")
}
