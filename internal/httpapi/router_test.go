package httpapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/httpx"
)

// pageHeaders asserts the §15 page header set on an API response.
func pageHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	if h.Get("X-Frame-Options") != "DENY" || h.Get("Content-Security-Policy") != httpx.CSPPage("") ||
		h.Get("Referrer-Policy") != "strict-origin-when-cross-origin" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%s: security headers: %v", what, h)
	}
}

func TestRouterEnvelopesAndMethods(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("owner")

	// Unknown API paths → 404 JSON; trailing-slash variants too.
	for _, p := range []string{"/api/nope", "/api/clips/", "/api/clips/abc/def/ghi", "/api/auth"} {
		if r := e.do("GET", p, nil, withCookie(cookie)); r.code != 404 || r.errCode() != "not_found" {
			t.Fatalf("%s: %d %s", p, r.code, r.body)
		}
	}
	// Method mismatches list every allowed method and carry the page
	// header set like every other API response.
	tests := []struct{ method, path, allow string }{
		{"PUT", "/api/clips/" + strings.Repeat("a", 12), "GET, HEAD, PATCH, DELETE"},
		{"DELETE", "/api/clips", "GET, HEAD, POST"},
		{"GET", "/api/auth/register", "POST"},
		{"PATCH", "/api/auth/tokens", "GET, HEAD, POST"},
		{"POST", "/api/clips/" + strings.Repeat("a", 12) + "/progress", "GET, HEAD"},
	}
	for _, tc := range tests {
		r := e.do(tc.method, tc.path, nil, withCookie(cookie))
		if r.code != 405 || r.hdr.Get("Allow") != tc.allow || r.errCode() != "method_not_allowed" {
			t.Fatalf("%s %s: %d Allow=%q %s", tc.method, tc.path, r.code, r.hdr.Get("Allow"), r.body)
		}
		pageHeaders(t, tc.method+" "+tc.path, r.hdr)
	}
	// Security headers on API responses, the catch-all included; JSON
	// content type on errors.
	r := e.do("GET", "/api/nope", nil)
	if r.hdr.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("404 headers: %v", r.hdr)
	}
	pageHeaders(t, "catch-all 404", r.hdr)
	pageHeaders(t, "me 401", e.do("GET", "/api/auth/me", nil).hdr)

	// 204s carry no Content-Type: there is no body to label.
	r = e.do("POST", "/api/auth/logout", nil, withCookie(cookie))
	if r.code != 204 || r.hdr.Get("Content-Type") != "" || r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("logout 204: %d %v", r.code, r.hdr)
	}
}

func TestJSONRoutesAreTimeBounded(t *testing.T) {
	old := jsonTimeout
	jsonTimeout = 50 * time.Millisecond
	defer func() { jsonTimeout = old }()
	e := newEnv(t)
	e.fake.Hook = func(ctx context.Context, op, key string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	start := time.Now()
	r := e.do("GET", "/readyz", nil)
	if r.code != 503 || r.errCode() != "timeout" || r.hdr.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("timeout: %d %q %s", r.code, r.hdr.Get("Content-Type"), r.body)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("not bounded: %s", time.Since(start))
	}
	// The expiry 503 is uncacheable and carries the page header set even
	// though the inner handler never ran to completion.
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("timeout 503 Cache-Control = %q", r.hdr.Get("Cache-Control"))
	}
	pageHeaders(t, "timeout 503", r.hdr)
}

func TestCfgDefaults(t *testing.T) {
	c := withDefaults(config.Config{})
	if c.ScratchDir != defaultScratchDir || c.MaxUploadBytes != defaultMaxUploadBytes || c.SessionCookie == "" || c.SessionTTL == 0 ||
		c.RateAuthPerMin != defaultRateAuthPerMin || c.RateUploadPerHour != defaultRateUploadHour ||
		c.RateUploadPerHourPerIP != defaultRateUploadIP || c.RateMaxKeys != defaultRateMaxKeys ||
		c.ViewDedupeTTL != defaultViewDedupeTTL || c.ViewDedupeMax != defaultViewDedupeMax {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestClientDisconnectIsNotABackendError(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The DB read fails only because the client is gone: a 4xx (logged at
	// INFO by AccessLog), never a 503.
	r := e.do("GET", "/api/clips/"+c.ID+"/video.mp4", nil, func(req *http.Request) { *req = *req.WithContext(ctx) })
	if r.code != 400 || r.errCode() != "client_disconnected" {
		t.Fatalf("disconnected: %d %s", r.code, r.body)
	}
}

// TestCancelledRequestKeepsTheHandlerAnswer pins §20 erratum 54: on a short
// JSON route a client that goes away must not collapse the handler's own
// envelope into http.TimeoutHandler's bodyless 503, which the access log
// then records at WARN as a server fault — §12 puts context.Canceled
// anywhere at INFO, and a still-reading client with a truncated body is owed
// the 4xx the handler wrote.
func TestCancelledRequestKeepsTheHandlerAnswer(t *testing.T) {
	gone := func(req *http.Request) {
		ctx, cancel := context.WithCancel(req.Context())
		cancel()
		*req = *req.WithContext(ctx)
	}
	t.Run("backend call failed only because the client is gone", func(t *testing.T) {
		e := newEnv(t)
		owner, cookie := e.newUser("owner")
		c, _, _ := e.readyClip(owner, clipOpts{})
		r, logs := e.logged("GET", "/api/clips/"+c.ID, nil, withCookie(cookie), gone)
		if r.code != 400 || r.errCode() != "client_disconnected" {
			t.Fatalf("cancelled GET: %d %s", r.code, r.body)
		}
		if !strings.Contains(logs, "client_disconnected=true") {
			t.Errorf("access log lost the disconnect: %s", logs)
		}
		if !strings.Contains(logs, "level=INFO") || strings.Contains(logs, "level=WARN") {
			t.Errorf("access log records a server fault: %s", logs)
		}
	})
	t.Run("truncated body keeps the decoder's envelope", func(t *testing.T) {
		e := newEnv(t)
		body := &errAfterReader{data: []byte(`{"username":"probe`), err: io.ErrUnexpectedEOF}
		r, logs := e.logged("POST", "/api/auth/register", body, withJSON(), gone)
		if r.code != 400 || r.errCode() != "invalid_request" {
			t.Fatalf("truncated body: %d %s", r.code, r.body)
		}
		if r.hdr.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Errorf("Content-Type %q on a %d-byte body", r.hdr.Get("Content-Type"), len(r.body))
		}
		if strings.Contains(logs, "level=WARN") {
			t.Errorf("access log records a server fault: %s", logs)
		}
	})
}

func TestCredentialWithDBDownIs503(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	tok := e.newToken(owner, "upload")
	_ = e.db.Close()
	// The Resolver records the failure and passes through anonymously
	// (§14); the API's gate turns that into 503 unavailable + Retry-After
	// on every credentialed chain — a presented credential is never
	// silently downgraded to anonymous.
	for _, tc := range []struct {
		method, path string
		opt          reqOpt
	}{
		{"GET", "/api/auth/me", withCookie(cookie)},
		{"GET", "/api/auth/me", withBearer(tok)},
		{"GET", "/api/clips/" + c.ID + "/original", withCookie(cookie)},
		{"GET", "/api/clips/" + c.ID, withCookie(cookie)},
	} {
		r := e.do(tc.method, tc.path, nil, tc.opt)
		if r.code != 503 || r.errCode() != "unavailable" || r.hdr.Get("Retry-After") != "5" || r.hdr.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s: %d %s %v", tc.method, tc.path, r.code, r.body, r.hdr)
		}
	}
	if strings.Contains(e.logBuf.String(), "level=ERROR") {
		t.Fatalf("a database outage is not an ERROR line: %s", e.logBuf.String())
	}
}
