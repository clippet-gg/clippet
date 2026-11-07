package clientip

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func prefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestResolve(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8", "127.0.0.1/32", "fd00::/8")
	tests := []struct {
		name    string
		remote  string
		hdr     map[string]string
		trusted []netip.Prefix
		want    string
	}{
		{"untrusted remote ignores headers", "203.0.113.9:4444",
			map[string]string{"X-Real-IP": "198.51.100.1", "X-Forwarded-For": "198.51.100.2"}, trusted, "203.0.113.9"},
		{"trust nothing by default", "10.1.1.1:1",
			map[string]string{"X-Real-IP": "198.51.100.1"}, nil, "10.1.1.1"},
		{"trusted uses X-Real-IP", "10.1.1.1:1",
			map[string]string{"X-Real-IP": "198.51.100.1", "X-Forwarded-For": "198.51.100.2"}, trusted, "198.51.100.1"},
		{"garbage X-Real-IP falls through to XFF", "10.1.1.1:1",
			map[string]string{"X-Real-IP": "not-an-ip", "X-Forwarded-For": "198.51.100.2"}, trusted, "198.51.100.2"},
		{"XFF right-to-left skipping trusted hops", "10.1.1.1:1",
			map[string]string{"X-Forwarded-For": "198.51.100.7, 203.0.113.5, 10.2.2.2, 10.3.3.3"}, trusted, "203.0.113.5"},
		{"XFF all trusted falls back to remote", "10.1.1.1:1",
			map[string]string{"X-Forwarded-For": "10.2.2.2, 10.3.3.3"}, trusted, "10.1.1.1"},
		{"XFF garbage falls back to remote", "10.1.1.1:1",
			map[string]string{"X-Forwarded-For": "203.0.113.5, garbage"}, trusted, "10.1.1.1"},
		{"XFF garbage beyond trusted hop falls back", "10.1.1.1:1",
			map[string]string{"X-Forwarded-For": "garbage, 10.2.2.2"}, trusted, "10.1.1.1"},
		{"no headers on trusted remote", "127.0.0.1:9", nil, trusted, "127.0.0.1"},
		{"ipv6 remote with port", "[2001:db8::1]:443", map[string]string{"X-Real-IP": "198.51.100.1"}, trusted, "2001:db8::"},
		{"trusted ipv6 proxy", "[fd00::1]:443", map[string]string{"X-Real-IP": "2001:db8::2"}, trusted, "2001:db8::"},
		{"remote without port", "10.1.1.1", map[string]string{"X-Real-IP": "198.51.100.3"}, trusted, "198.51.100.3"},
		{"ipv4-mapped remote unmapped", "[::ffff:10.1.1.1]:5", map[string]string{"X-Real-IP": "198.51.100.4"}, trusted, "198.51.100.4"},
		{"XFF with port-suffixed entry is garbage", "10.1.1.1:1",
			map[string]string{"X-Forwarded-For": "203.0.113.5:1234"}, trusted, "203.0.113.5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			for k, v := range tc.hdr {
				r.Header.Set(k, v)
			}
			got := Resolve(r, tc.trusted)
			if got.String() != tc.want {
				t.Fatalf("Resolve = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveMultipleXFFHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.2:1"
	r.Header.Add("X-Forwarded-For", "203.0.113.5")
	r.Header.Add("X-Forwarded-For", "10.9.9.9")
	if got := Resolve(r, prefixes(t, "10.0.0.0/8")); got.String() != "203.0.113.5" {
		t.Fatalf("Resolve = %q", got)
	}
}

func TestResolveUnparseableRemote(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "pipe"
	r.Header.Set("X-Real-IP", "198.51.100.1")
	got := Resolve(r, prefixes(t, "0.0.0.0/0"))
	if got.IsValid() {
		t.Fatalf("Resolve = %q, want invalid", got)
	}
	if got.String() == "" {
		t.Fatal("invalid addr must still render a stable key")
	}
}

// TestResolveFoldsIPv6ToSlash64 pins §20 erratum 292: two IPv6 addresses in
// one /64 resolve to the same Addr (the /64 base), two /64s do not, and IPv4
// -- plain or v4-mapped -- is exactly what it was.
func TestResolveFoldsIPv6ToSlash64(t *testing.T) {
	get := func(remote string) netip.Addr {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		return Resolve(r, nil)
	}
	for remote, want := range map[string]string{
		"[2001:db8:1:2:3:4:5:6]:443":           "2001:db8:1:2::",
		"[2001:db8:1:2:ffff:ffff:ffff:ffff]:1": "2001:db8:1:2::",
		"[2001:db8:1:2::]:1":                   "2001:db8:1:2::",
		"[2001:db8:1:3::1]:1":                  "2001:db8:1:3::",
		"203.0.113.9:4444":                     "203.0.113.9",
		"[::ffff:203.0.113.9]:4444":            "203.0.113.9",
		"[fe80::1%eth0]:5":                     "fe80::",
	} {
		if got := get(remote); got.String() != want {
			t.Errorf("Resolve(%s) = %q, want %q", remote, got, want)
		}
	}
	one, two := get("[2001:db8:1:2:3:4:5:6]:443"), get("[2001:db8:1:2:ffff:ffff:ffff:ffff]:1")
	if one != two {
		t.Fatalf("one /64, two keys: %q vs %q", one, two)
	}
	if other := get("[2001:db8:1:3::1]:1"); other == one {
		t.Fatalf("two /64s, one key: %q", one)
	}
	if v4a, v4b := get("203.0.113.9:1"), get("203.0.113.10:1"); v4a == v4b {
		t.Fatalf("IPv4 must be unchanged by the fold: %q == %q", v4a, v4b)
	}
	if got := get("pipe"); got.IsValid() {
		t.Fatalf("an unparseable peer must stay invalid through the fold, got %q", got)
	}
}

// TestResolveFoldAfterProxyCheck pins the ORDER: the trusted-proxy test runs
// on the exact peer address and only the resolved client is folded. A proxy
// trusted as a /128 would otherwise never match (its own /64 base is not in
// fd00::1/128), the headers would be ignored, and the proxy's address would
// become every viewer's key.
func TestResolveFoldAfterProxyCheck(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "[fd00::1]:443"
	r.Header.Set("X-Real-IP", "2001:db8:aa:bb:cc:dd:ee:ff")
	if got := Resolve(r, prefixes(t, "fd00::1/128")); got.String() != "2001:db8:aa:bb::" {
		t.Fatalf("Resolve = %q, want the client's /64 base 2001:db8:aa:bb:: (the /128 proxy must still match)", got)
	}
	// And a trusted hop inside X-Forwarded-For is matched exactly too.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "[fd00::1]:443"
	r.Header.Set("X-Forwarded-For", "2001:db8:aa:bb::7, fd00::2")
	if got := Resolve(r, prefixes(t, "fd00::1/128", "fd00::2/128")); got.String() != "2001:db8:aa:bb::" {
		t.Fatalf("Resolve = %q, want 2001:db8:aa:bb::", got)
	}
}
