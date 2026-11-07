// The HOSTED Clippet Auto's three other destinations (§20 erratum 334,
// cmd/clippet-auto), built here because this is the one package allowed to
// build an outbound client (§15, §20 errata 66/96). Discord itself is
// AutoClient's (auto.go), unchanged.
//
//	the plane     AUTO_PLANE_URL (the claim Worker): GET /auto/guild/{id},
//	              GET|PUT /auto/session, GET /auto/guilds — the bearer
//	              AUTO_PLANE_SECRET, 10 s, 64 KiB read (the list 1 MiB)
//	a box         the server's own Clippet Auto URL, opened from its seal for
//	              one upload: POST the instruction, TWO headers (content-type,
//	              user-agent `clippet-auto/1`), never an Authorization, 110 s
//	              (the box holds its answer up to 100 s), 8 KiB read and only
//	              for the statuses whose body is used
//	the dead-man  AUTO_DEADMAN_URL, a bare GET, 10 s
//
// All three go through the hosted transport (newHostedTransport): 64 KiB of
// headers at most, and no dial to an address that is not public — save, under
// AUTO_TEST_ENDPOINTS=1 alone, a name under the reserved `.test` domain (§20
// erratum 347: the end-to-end bundle's private network).
// No redirect is followed anywhere (this package's one client). Every
// transport failure is ONE error with no URL in it: a box URL is a
// credential, and so is the plane's path in a log line read by the wrong eye.
package notify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// The hosted client's limits.
const (
	planeCallTimeout = 10 * time.Second
	planeAnswerMax   = 64 << 10
	// BoxCallTimeout is the one deadline over a box's headers AND body.
	BoxCallTimeout = 110 * time.Second
	boxAnswerMax   = 8 << 10
	pingTimeout    = 10 * time.Second
	// BoxUserAgent names the hosted Clippet Auto to a box (an edge in front of
	// a box may challenge a request with no user agent).
	BoxUserAgent = "clippet-auto/1"
)

// boxReadStatuses are the answers whose body the hosted process reads: the
// link (201, 200) and the box's own envelope (403, 502).
var boxReadStatuses = map[int]bool{200: true, 201: true, 403: true, 502: true}

// errHostedCall is every transport failure, worded once.
var errHostedCall = errors.New("the call did not complete")

// HostedDeps configures a HostedClient.
type HostedDeps struct {
	PlaneURL    string // the claim Worker's base, e.g. its /claim path; https
	PlaneSecret string // SECRET: the bearer for the plane's /auto routes
	// AllowHTTP admits a plain-http plane (tests only).
	AllowHTTP bool
	// AllowPrivate lets the hosted transport dial a loopback or private
	// address (tests only: an httptest server is on loopback).
	AllowPrivate bool
	// AllowTestNames lets a name under the reserved `.test` domain (RFC 6761:
	// it never resolves on the public internet) reach a private address, and
	// no other name: cmd/clippet-auto sets it for AUTO_TEST_ENDPOINTS=1 alone,
	// the end-to-end bundle's private network (§20 erratum 347). Every other
	// name keeps the refusal, whatever it resolves to.
	AllowTestNames bool
	Transport      http.RoundTripper // tests only
}

// The hosted transport's limits (audit F4, §20 erratum 334's 2026-10-12
// amendment): a box is anybody's server, so its headers are capped far below
// net/http's 10 MiB default, and every phase before the body has a deadline
// of its own under the call's.
const (
	hostedHeaderMax       = 64 << 10
	hostedTLSHandshake    = 10 * time.Second
	hostedResponseHeaders = BoxCallTimeout
	hostedDialTimeout     = 10 * time.Second
	// planeListMax is GET /auto/guilds' read cap (audit F10): the plane lists
	// up to 10,000 servers, ≈ 22 bytes each — past 64 KiB at ≈ 2,900.
	planeListMax = 1 << 20
)

// newHostedTransport is the hosted client's own transport: no proxy, the
// caps above, and a dialer that refuses any address that is not public —
// loopback, private (RFC 1918 and IPv6 ULA, which is Fly's private network),
// link-local, CGNAT, unspecified or multicast — for the plane and a box
// alike, AFTER resolution, so a name that resolves to one is refused too.
func newHostedTransport(allowPrivate, allowTestNames bool) *http.Transport {
	d := &net.Dialer{Timeout: hostedDialTimeout, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		d.Control = refuseNonPublic
	}
	dial := d.DialContext
	if !allowPrivate && allowTestNames {
		open := &net.Dialer{Timeout: hostedDialTimeout, KeepAlive: 30 * time.Second}
		dial = testNameDial(open.DialContext, d.DialContext)
	}
	return &http.Transport{
		DialContext:            dial,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           32,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    hostedTLSHandshake,
		ResponseHeaderTimeout:  hostedResponseHeaders,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: hostedHeaderMax,
	}
}

var errNotPublic = errors.New("the address is not public")

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// testNameDial is AllowTestNames' dial: a `.test` name goes to open (no
// address check), every other address to guarded (refuseNonPublic). The name
// is read BEFORE resolution — the one thing about it no resolver can steer.
func testNameDial(open, guarded dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, err := net.SplitHostPort(addr); err == nil && TestName(host) {
			return open(ctx, network, addr)
		}
		return guarded(ctx, network, addr)
	}
}

// TestName reports whether host is a name under the reserved `.test` domain
// (RFC 6761 §6.2: no public resolver answers for it). An IP literal never is.
func TestName(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return len(h) > len(".test") && strings.HasSuffix(h, ".test") && !strings.HasPrefix(h, ".") &&
		!strings.Contains(h, "..")
}

// cgnat is RFC 6598's shared address space, which netip does not call private;
// nat64 (RFC 6052's well-known prefix) reaches any IPv4 address, private ones
// included, through a translator; doc6 is IPv6's documentation range (RFC 3849).
var (
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
	doc6  = netip.MustParsePrefix("2001:db8::/32")
)

// refuseNonPublic is the dialer's Control: it sees the resolved address.
func refuseNonPublic(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return errNotPublic
	}
	if !publicAddr(ap.Addr()) {
		return errNotPublic
	}
	return nil
}

func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast() &&
		!a.IsUnspecified() && !a.IsMulticast() && !cgnat.Contains(a) && !nat64.Contains(a) && !doc6.Contains(a) && !(a.Is4() && a.As4()[0] == 0)
}

// HostedClient makes the hosted process's plane, box and dead-man calls.
type HostedClient struct {
	plane  *url.URL
	secret string
	client *http.Client
}

// NewHostedClient returns the client, or an error naming the field.
func NewHostedClient(d HostedDeps) (*HostedClient, error) {
	u, err := url.Parse(strings.TrimRight(d.PlaneURL, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		!(u.Scheme == "https" || (d.AllowHTTP && u.Scheme == "http")) {
		return nil, errors.New("AUTO_PLANE_URL must be an https URL with no userinfo, query or fragment")
	}
	if d.PlaneSecret == "" {
		return nil, errors.New("AUTO_PLANE_SECRET is required")
	}
	var tr http.RoundTripper = d.Transport
	if tr == nil {
		tr = newHostedTransport(d.AllowPrivate, d.AllowTestNames)
	}
	return &HostedClient{plane: u, secret: d.PlaneSecret, client: newHTTPClient(0, tr)}, nil
}

// Plane makes one call on the plane: method, a path under AUTO_PLANE_URL
// (`/auto/guild/…`), an optional JSON body. It answers the status and at most
// 64 KiB of the body.
func (c *HostedClient) Plane(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	cctx, cancel := context.WithTimeout(ctx, planeCallTimeout)
	defer cancel()
	u := *c.plane
	u.Path = c.plane.Path + path
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(cctx, method, u.String(), rd)
	if err != nil {
		return 0, nil, errHostedCall
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	req.Header.Set("User-Agent", BoxUserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, errHostedCall
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()
	limit := int64(planeAnswerMax)
	if path == "/auto/guilds" {
		limit = planeListMax
	}
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	return resp.StatusCode, answer, nil
}

// Box posts one instruction to a box's Clippet Auto URL (already opened from
// its seal and checked against the server's route by the caller). It answers
// the status — 0 for no answer within BoxCallTimeout, whatever came first —
// and the body for the statuses whose body is read, at most 8 KiB (a longer
// one is read as none).
func (c *HostedClient) Box(ctx context.Context, rawURL string, instruction []byte, timeout time.Duration) (int, []byte) {
	if timeout <= 0 {
		timeout = BoxCallTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, rawURL, bytes.NewReader(instruction))
	if err != nil {
		return 0, nil
	}
	req.Header = http.Header{"Content-Type": {"application/json"}, "User-Agent": {BoxUserAgent}}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if !boxReadStatuses[resp.StatusCode] {
		return resp.StatusCode, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, boxAnswerMax+1))
	if err != nil {
		if cctx.Err() != nil {
			return 0, nil // the body stalled past the deadline: no answer
		}
		return resp.StatusCode, nil
	}
	if len(b) > boxAnswerMax {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, b
}

// Ping is the dead-man's GET: true when it answered 2xx.
func (c *HostedClient) Ping(ctx context.Context, rawURL string) bool {
	cctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", BoxUserAgent)
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	_ = resp.Body.Close()
	return resp.StatusCode/100 == 2
}
