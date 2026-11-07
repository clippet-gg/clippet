// Package clientip resolves the real client address behind trusted proxies
// (DESIGN.md §13). Forwarding headers are honoured ONLY when the TCP peer is
// inside one of the TRUSTED_PROXIES prefixes; otherwise they are ignored, so
// a direct client can neither spoof another address nor collapse every
// viewer onto one.
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Resolve returns the client address for r. It parses RemoteAddr; only when
// that peer lies inside one of trusted are the headers consulted:
// X-Real-IP first (if present and parseable), else X-Forwarded-For walked
// from the right, skipping hops inside trusted prefixes and taking the first
// untrusted address. Garbage or exhausted headers fall back to RemoteAddr.
// An unparseable RemoteAddr yields the zero (invalid) Addr, whose String()
// is a stable "invalid IP" bucket for callers that only need a key.
//
// An IPv6 client is returned as the base of its /64 (§20 erratum 292): every
// caller keys on the result -- the auth and view limiters, the view dedupe --
// and a /64 is what an ISP hands one household or one phone, inside which a
// machine may use any of 2^64 addresses (the interface identifier is chosen
// by the host, and privacy extensions rotate it by the hour). Keyed on the
// whole address, one machine was 2^64 limiter buckets and 2^64 "viewers";
// keyed on the /64 it is one of each, which is what one IPv4 address already
// was. The fold happens AFTER the trusted-proxy test, which runs on the exact
// peer, so a proxy trusted as fd00::1/128 is still matched by fd00::1.
func Resolve(r *http.Request, trusted []netip.Prefix) netip.Addr {
	return fold(resolve(r, trusted))
}

// fold returns the /64 base of an IPv6 address and any other address (IPv4;
// the invalid zero Addr) unchanged. parseAddr already unmapped ::ffff:a.b.c.d
// to plain IPv4, so the Is4In6 test is a guard against a caller that did not.
func fold(a netip.Addr) netip.Addr {
	if !a.Is6() || a.Is4In6() {
		return a
	}
	return netip.PrefixFrom(a, 64).Masked().Addr()
}

// resolve is Resolve before the fold: the exact address of the client.
func resolve(r *http.Request, trusted []netip.Prefix) netip.Addr {
	remote := parseAddr(r.RemoteAddr)
	if !remote.IsValid() || !inPrefixes(remote, trusted) {
		return remote
	}
	if real := parseAddr(r.Header.Get("X-Real-IP")); real.IsValid() {
		return real
	}
	if fwd := forwardedFor(r.Header.Values("X-Forwarded-For"), trusted); fwd.IsValid() {
		return fwd
	}
	return remote
}

// forwardedFor walks the joined X-Forwarded-For lists from the right. Every
// hop inside trusted was appended by one of our proxies and is skipped; the
// first untrusted hop is the client. An unparseable entry ends the walk (it
// was written by something we do not control), leaving the caller to fall
// back to RemoteAddr.
func forwardedFor(headers []string, trusted []netip.Prefix) netip.Addr {
	var hops []string
	for _, h := range headers {
		for _, part := range strings.Split(h, ",") {
			if part = strings.TrimSpace(part); part != "" {
				hops = append(hops, part)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a := parseAddr(hops[i])
		if !a.IsValid() {
			return netip.Addr{}
		}
		if !inPrefixes(a, trusted) {
			return a
		}
	}
	return netip.Addr{}
}

// parseAddr accepts "ip", "ip:port", "[v6]:port" or "[v6]" and returns the
// unmapped address (IPv4-in-IPv6 becomes plain IPv4 so prefix checks and
// dedupe keys treat ::ffff:1.2.3.4 and 1.2.3.4 alike).
func parseAddr(s string) netip.Addr {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap().WithZone("")
}

func inPrefixes(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
