package sniproxy

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// captureHello runs a crypto/tls client handshake against a pipe and
// returns the raw bytes of its first flight (the ClientHello records).
func captureHello(t *testing.T, cfg *tls.Config) []byte {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tls.Client(client, cfg).Handshake()
	}()
	var buf bytes.Buffer
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	hdr := make([]byte, recordHeaderLen)
	if _, err := io.ReadFull(server, hdr); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[3:]))
	if _, err := io.ReadFull(server, body); err != nil {
		t.Fatal(err)
	}
	buf.Write(hdr)
	buf.Write(body)
	server.Close()
	client.Close()
	<-done
	return buf.Bytes()
}

// refragment splits the handshake payload of a single-record ClientHello
// into records of at most size bytes.
func refragment(t *testing.T, rec []byte, size int) []byte {
	t.Helper()
	payload := rec[recordHeaderLen:]
	var out []byte
	for len(payload) > 0 {
		n := min(size, len(payload))
		out = append(out, recordTypeHandshake, 0x03, 0x01, byte(n>>8), byte(n))
		out = append(out, payload[:n]...)
		payload = payload[n:]
	}
	return out
}

// sniExt builds a server_name extension with one entry.
func sniExt(nameType byte, name string) []byte {
	return sniExtHosts(nameType, name)
}

// sniExtHosts builds a server_name extension with one entry per name.
func sniExtHosts(nameType byte, names ...string) []byte {
	var entries []byte
	for _, name := range names {
		entries = append(entries, nameType, byte(len(name)>>8), byte(len(name)))
		entries = append(entries, name...)
	}
	list := append([]byte{byte(len(entries) >> 8), byte(len(entries))}, entries...)
	return ext(extensionServerName, list)
}

// ext builds one extension of type typ around data.
func ext(typ int, data []byte) []byte {
	return append([]byte{byte(typ >> 8), byte(typ), byte(len(data) >> 8), byte(len(data))}, data...)
}

// handBuiltHello wraps exts in a minimal ClientHello record. extLen
// overrides the extensions length field when non-negative.
func handBuiltHello(t *testing.T, exts []byte, extLen int) []byte {
	t.Helper()
	if extLen < 0 {
		extLen = len(exts)
	}
	body := []byte{0x03, 0x03}
	body = append(body, make([]byte, 32)...)
	body = append(body, 0x00)                   // session_id
	body = append(body, 0x00, 0x02, 0x13, 0x01) // cipher_suites
	body = append(body, 0x01, 0x00)             // compression_methods
	body = append(body, byte(extLen>>8), byte(extLen))
	body = append(body, exts...)
	msg := append([]byte{handshakeTypeClientHello, 0x00, byte(len(body) >> 8), byte(len(body))}, body...)
	return append([]byte{recordTypeHandshake, 0x03, 0x01, byte(len(msg) >> 8), byte(len(msg))}, msg...)
}

func reasonOf(err error) Reason {
	var he *helloError
	if errors.As(err, &he) {
		return he.reason
	}
	return ""
}

func TestReadClientHello(t *testing.T) {
	withSNI := captureHello(t, &tls.Config{ServerName: "API.Example.test", InsecureSkipVerify: true})
	tls12 := captureHello(t, &tls.Config{ServerName: "old.example.test", InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12})
	noSNI := captureHello(t, &tls.Config{InsecureSkipVerify: true})
	ipLiteral := captureHello(t, &tls.Config{ServerName: "192.0.2.7", InsecureSkipVerify: true})

	truncatedBody := append([]byte(nil), withSNI[:len(withSNI)-10]...)
	wrongHandshake := append([]byte(nil), withSNI...)
	wrongHandshake[recordHeaderLen] = 0x02 // ServerHello
	badLength := []byte{recordTypeHandshake, 0x03, 0x01, 0xff, 0xff}
	zeroLength := []byte{recordTypeHandshake, 0x03, 0x01, 0x00, 0x00}
	badExt := handBuiltHello(t, []byte{0x00, 0x00, 0x00, 0x10}, 0x0100)
	otherNameType := handBuiltHello(t, sniExt(0x01, "a.example.test"), -1)
	badChars := handBuiltHello(t, sniExt(serverNameTypeHostName, "a b.example.test"), -1)
	handOK := handBuiltHello(t, sniExt(serverNameTypeHostName, "Hand.Example.Test."), -1)
	// One record holding only a handshake header that announces a
	// ClientHello one byte over the cap; the body is never sent.
	overCap := maxHelloLen + 1
	helloTooLong := []byte{recordTypeHandshake, 0x03, 0x01, 0x00, 0x04, handshakeTypeClientHello,
		byte(overCap >> 16), byte(overCap >> 8), byte(overCap)}
	shortHello := []byte{recordTypeHandshake, 0x03, 0x01, 0x00, 0x08, handshakeTypeClientHello, 0x00, 0x00, 0x04, 0x03, 0x03, 0x00, 0x00}

	pub := sniExt(serverNameTypeHostName, "public.example.test")
	// An ECH payload is opaque here; Chromium's ECH-GREASE is random bytes
	// under the same extension type.
	echPayload := ext(extensionEncryptedClientHello, []byte{0x00, 0x00, 0x01, 0x00, 0x01, 0x07, 0x00, 0x00, 0x00})
	echAfterSNI := handBuiltHello(t, append(append([]byte(nil), pub...), echPayload...), -1)
	echBeforeSNI := handBuiltHello(t, append(append([]byte(nil), echPayload...), pub...), -1)
	twoSNIExts := handBuiltHello(t, append(append([]byte(nil), pub...), sniExt(serverNameTypeHostName, "other.example.test")...), -1)
	twoHostNames := handBuiltHello(t, sniExtHosts(serverNameTypeHostName, "public.example.test", "other.example.test"), -1)
	groups := ext(0x000a, []byte{0x00, 0x02, 0x00, 0x1d})
	twoOtherExts := handBuiltHello(t, append(append(append([]byte(nil), groups...), groups...), pub...), -1)
	sniTrailing := ext(extensionServerName, append(pub[4:], 0x00))
	sniDataTrailing := handBuiltHello(t, sniTrailing, -1)
	bodyTrailing := handBuiltHello(t, append(append([]byte(nil), pub...), 0x00), len(pub))
	recordTrailing := append(handBuiltHello(t, pub, -1), 0x00)
	recordTrailing[3], recordTrailing[4] = byte((len(recordTrailing)-recordHeaderLen)>>8), byte(len(recordTrailing)-recordHeaderLen)

	tests := []struct {
		name       string
		in         []byte
		wantSNI    string
		wantReason Reason
	}{
		{"tls13 sni", withSNI, "api.example.test", ""},
		{"tls12 sni", tls12, "old.example.test", ""},
		{"fragmented", refragment(t, withSNI, 64), "api.example.test", ""},
		{"one-byte records", refragment(t, withSNI, 1), "api.example.test", ""},
		{"no sni", noSNI, "", ReasonNoSNI},
		{"ip literal sends no sni", ipLiteral, "", ReasonNoSNI},
		{"http", []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"), "", ReasonNotTLS},
		{"ssh", []byte("SSH-2.0-OpenSSH_9.6\r\n"), "", ReasonNotTLS},
		{"empty", nil, "", ReasonTruncated},
		{"partial header", withSNI[:3], "", ReasonTruncated},
		{"truncated body", truncatedBody, "", ReasonTruncated},
		{"not a client hello", wrongHandshake, "", ReasonMalformed},
		{"record too long", badLength, "", ReasonMalformed},
		{"record empty", zeroLength, "", ReasonMalformed},
		{"hello over the cap", helloTooLong, "", ReasonMalformed},
		{"short hello", shortHello, "", ReasonMalformed},
		{"corrupt extensions", badExt, "", ReasonMalformed},
		{"server_name without host_name", otherNameType, "", ReasonNoSNI},
		{"server_name not a hostname", badChars, "", ReasonMalformed},
		{"hand-built", handOK, "hand.example.test", ""},
		{"ech after sni", echAfterSNI, "public.example.test", ReasonECH},
		{"ech before sni", echBeforeSNI, "public.example.test", ReasonECH},
		{"two server_name extensions", twoSNIExts, "", ReasonMalformed},
		{"two host_names", twoHostNames, "", ReasonMalformed},
		{"duplicate extension", twoOtherExts, "", ReasonMalformed},
		{"server_name data past its list", sniDataTrailing, "", ReasonMalformed},
		{"bytes past the extensions", bodyTrailing, "", ReasonMalformed},
		{"bytes past the ClientHello in its record", recordTrailing, "", ReasonMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, sni, err := readClientHello(bytes.NewReader(tt.in))
			if got := reasonOf(err); got != tt.wantReason {
				t.Fatalf("reason = %q (err %v); want %q", got, err, tt.wantReason)
			}
			if sni != tt.wantSNI {
				t.Fatalf("sni = %q; want %q", sni, tt.wantSNI)
			}
			if err == nil && !bytes.Equal(raw, tt.in) {
				t.Fatalf("raw replay differs from input (%d vs %d bytes)", len(raw), len(tt.in))
			}
		})
	}
}

func TestReadClientHelloStopsAtHello(t *testing.T) {
	hello := captureHello(t, &tls.Config{ServerName: "a.example.test", InsecureSkipVerify: true})
	trailing := []byte{0x17, 0x03, 0x03, 0x00, 0x01, 0xaa}
	r := bytes.NewReader(append(append([]byte(nil), hello...), trailing...))
	raw, _, err := readClientHello(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, hello) {
		t.Fatal("raw differs from the ClientHello")
	}
	rest, _ := io.ReadAll(r)
	if !bytes.Equal(rest, trailing) {
		t.Fatalf("read past the ClientHello: %d trailing bytes left, want %d", len(rest), len(trailing))
	}
}

func TestReadClientHelloTimeout(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	_ = server.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	go func() { _, _ = client.Write([]byte{recordTypeHandshake, 0x03}) }()
	_, _, err := readClientHello(server)
	if got := reasonOf(err); got != ReasonTimeout {
		t.Fatalf("reason = %q (err %v); want timeout", got, err)
	}
}
