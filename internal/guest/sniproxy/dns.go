package sniproxy

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DNSResolver resolves names by querying fixed DNS servers directly. It
// never reads /etc/hosts, /etc/nsswitch.conf, or /etc/resolv.conf. The
// guest's /etc/hosts maps allowlisted names to an address that only leads
// back to the proxy, so the proxy must not use the system resolver, which
// consults /etc/hosts first.
type DNSResolver struct {
	// Servers are queried in order until one answers.
	Servers []netip.AddrPort
	// Dialer defaults to a *net.Dialer.
	Dialer Dialer
	// Timeout bounds each query to one server; default 5s.
	Timeout time.Duration
}

// LookupNetIP implements Resolver. network is "ip" (A and AAAA), "ip4", or
// "ip6". The answer's addresses are those of the name or of the CNAME chain
// that starts at it.
func (r *DNSResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	var types []dnsmessage.Type
	switch network {
	case "ip":
		types = []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA}
	case "ip4":
		types = []dnsmessage.Type{dnsmessage.TypeA}
	case "ip6":
		types = []dnsmessage.Type{dnsmessage.TypeAAAA}
	default:
		return nil, fmt.Errorf("sniproxy: dns: unsupported network %q", network)
	}
	if len(r.Servers) == 0 {
		return nil, errors.New("sniproxy: dns: no servers")
	}
	name, err := dnsmessage.NewName(strings.TrimSuffix(host, ".") + ".")
	if err != nil {
		return nil, fmt.Errorf("sniproxy: dns: %q: %w", host, err)
	}
	// As with the net package's resolver, one record type failing, even as
	// not found, does not discard the addresses of the other. With no
	// addresses, a not-found answer outranks other failures: the name does
	// not exist, so retrying cannot help. The types are queried concurrently
	// and combined in the order of types.
	type result struct {
		addrs []netip.Addr
		err   error
	}
	results := make([]result, len(types))
	var wg sync.WaitGroup
	for i, t := range types {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].addrs, results[i].err = r.lookup(ctx, name, t)
		}()
	}
	wg.Wait()
	var out []netip.Addr
	var notFound error
	var errs []error
	for _, res := range results {
		var de *net.DNSError
		switch {
		case errors.As(res.err, &de) && de.IsNotFound:
			notFound = res.err
		case res.err != nil:
			errs = append(errs, res.err)
		}
		out = append(out, res.addrs...)
	}
	switch {
	case len(out) > 0:
		return out, nil
	case notFound != nil:
		return nil, notFound
	case len(errs) > 0:
		return nil, errors.Join(errs...)
	}
	return nil, &net.DNSError{Err: "no address", Name: host, IsNotFound: true}
}

// lookup asks each server in turn for name's records of type t and returns
// the first conclusive answer.
func (r *DNSResolver) lookup(ctx context.Context, name dnsmessage.Name, t dnsmessage.Type) ([]netip.Addr, error) {
	var errs []error
	for _, s := range r.Servers {
		addrs, err := r.query(ctx, s, name, t)
		if errors.Is(err, errNXDomain) {
			return nil, &net.DNSError{Err: errNXDomain.Error(), Name: name.String(), Server: s.String(), IsNotFound: true}
		}
		if err == nil {
			return addrs, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", s, err))
	}
	return nil, fmt.Errorf("sniproxy: dns: lookup %s: %w", name, errors.Join(errs...))
}

var errNXDomain = errors.New("no such host")

// query asks server over UDP, and again over TCP when the UDP answer is
// truncated.
func (r *DNSResolver) query(ctx context.Context, server netip.AddrPort, name dnsmessage.Name, t dnsmessage.Type) ([]netip.Addr, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := r.Dialer
	if d == nil {
		d = &net.Dialer{}
	}
	var idb [2]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return nil, err
	}
	id := binary.BigEndian.Uint16(idb[:])
	q := dnsmessage.Question{Name: name, Type: t, Class: dnsmessage.ClassINET}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{q},
	}
	packed, err := msg.Pack()
	if err != nil {
		return nil, err
	}
	resp, err := exchange(ctx, d, "udp", server, packed)
	if err != nil {
		return nil, err
	}
	addrs, truncated, err := parseAnswer(resp, id, q)
	if !truncated {
		return addrs, err
	}
	if resp, err = exchange(ctx, d, "tcp", server, packed); err != nil {
		return nil, err
	}
	// A truncated answer over TCP is incomplete, not empty: an error, so
	// the next server is asked.
	if addrs, truncated, err = parseAnswer(resp, id, q); truncated {
		return nil, errors.New("answer truncated over TCP")
	}
	return addrs, err
}

// exchange sends one query and reads one response. Over TCP both carry the
// two-byte length prefix.
func exchange(ctx context.Context, d Dialer, network string, server netip.AddrPort, query []byte) ([]byte, error) {
	c, err := d.DialContext(ctx, network, server.String())
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
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
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

// parseAnswer checks that resp answers q under id and returns the addresses
// of q's type held by q's name or by a name its CNAME chain leads to.
func parseAnswer(resp []byte, id uint16, q dnsmessage.Question) (addrs []netip.Addr, truncated bool, err error) {
	var p dnsmessage.Parser
	h, err := p.Start(resp)
	if err != nil {
		return nil, false, fmt.Errorf("parse response: %w", err)
	}
	if h.ID != id || !h.Response {
		return nil, false, errors.New("response does not match the query")
	}
	if h.Truncated {
		return nil, true, nil
	}
	qs, err := p.AllQuestions()
	if err != nil {
		return nil, false, fmt.Errorf("parse response: %w", err)
	}
	if len(qs) != 1 || !strings.EqualFold(qs[0].Name.String(), q.Name.String()) || qs[0].Type != q.Type || qs[0].Class != q.Class {
		return nil, false, errors.New("response question does not match the query")
	}
	switch h.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError:
		return nil, false, errNXDomain
	default:
		return nil, false, fmt.Errorf("server returned %v", h.RCode)
	}
	answers, err := p.AllAnswers()
	if err != nil {
		return nil, false, fmt.Errorf("parse response: %w", err)
	}
	chain := map[string]bool{strings.ToLower(q.Name.String()): true}
	// CNAME records may come in any order; repeat until the chain stops
	// growing.
	for grew := true; grew; {
		grew = false
		for _, a := range answers {
			cn, ok := a.Body.(*dnsmessage.CNAMEResource)
			if !ok || a.Header.Class != dnsmessage.ClassINET {
				continue
			}
			from, to := strings.ToLower(a.Header.Name.String()), strings.ToLower(cn.CNAME.String())
			if chain[from] && !chain[to] {
				chain[to] = true
				grew = true
			}
		}
	}
	for _, a := range answers {
		if a.Header.Class != dnsmessage.ClassINET || !chain[strings.ToLower(a.Header.Name.String())] {
			continue
		}
		switch b := a.Body.(type) {
		case *dnsmessage.AResource:
			if q.Type == dnsmessage.TypeA {
				addrs = append(addrs, netip.AddrFrom4(b.A))
			}
		case *dnsmessage.AAAAResource:
			if q.Type == dnsmessage.TypeAAAA {
				addrs = append(addrs, netip.AddrFrom16(b.AAAA))
			}
		}
	}
	return addrs, false, nil
}
