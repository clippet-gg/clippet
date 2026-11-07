package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"clippet/internal/store"
)

func TestMediaRoutes(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, rendition, poster := e.readyClip(owner, clipOpts{})
	base := "/api/clips/" + c.ID
	etag := `"` + sha256hex(rendition) + `"`

	// 200: full body, Content-Length from the DB, ETag, Accept-Ranges,
	// Cache-Control without immutable, nosniff; exactly one GetRange.
	r := e.do("GET", base+"/video.mp4", nil)
	if r.code != 200 || string(r.body) != string(rendition) {
		t.Fatalf("video 200: %d len=%d", r.code, len(r.body))
	}
	if r.hdr.Get("Content-Type") != "video/mp4" || r.hdr.Get("ETag") != etag || r.hdr.Get("Accept-Ranges") != "bytes" ||
		r.hdr.Get("Content-Length") != strconv.Itoa(len(rendition)) || r.hdr.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("video headers: %v", r.hdr)
	}
	if cc := r.hdr.Get("Cache-Control"); cc != "public, max-age=3600" || strings.Contains(cc, "immutable") {
		t.Fatalf("Cache-Control = %q", cc)
	}
	if r.hdr.Get("Last-Modified") == "" {
		t.Fatal("Last-Modified missing")
	}
	// Media carries the media header set (§15): no page framing headers.
	if _, has := r.hdr["X-Frame-Options"]; has || r.hdr.Get("Content-Security-Policy") != "" || r.hdr.Get("Referrer-Policy") == "" {
		t.Fatalf("media carries page headers: %v", r.hdr)
	}
	if e.gets() != 1 {
		t.Fatalf("GetRange calls = %d, want 1: %v", e.gets(), e.fake.CallsSnapshot())
	}

	// 206: single range, Content-Range, one more GET.
	r = e.do("GET", base+"/video.mp4", nil, withHeader("Range", "bytes=100-199"))
	if r.code != 206 || string(r.body) != string(rendition[100:200]) || r.hdr.Get("Content-Range") != "bytes 100-199/"+strconv.Itoa(len(rendition)) ||
		r.hdr.Get("Content-Length") != "100" {
		t.Fatalf("206: %d %v len=%d", r.code, r.hdr, len(r.body))
	}
	if e.gets() != 2 {
		t.Fatalf("GetRange calls = %d, want 2", e.gets())
	}
	// Open-ended range.
	r = e.do("GET", base+"/video.mp4", nil, withHeader("Range", "bytes=4000-"))
	if r.code != 206 || len(r.body) != 96 || r.hdr.Get("Content-Range") != "bytes 4000-4095/4096" {
		t.Fatalf("open-ended 206: %d %v len=%d", r.code, r.hdr.Get("Content-Range"), len(r.body))
	}

	// 304 on If-None-Match with ZERO storage calls.
	before := e.gets()
	r = e.do("GET", base+"/video.mp4", nil, withHeader("If-None-Match", etag))
	if r.code != 304 || len(r.body) != 0 || e.gets() != before {
		t.Fatalf("304: %d len=%d gets=%d", r.code, len(r.body), e.gets()-before)
	}
	if r.hdr.Get("ETag") != etag {
		t.Fatalf("304 ETag = %q", r.hdr.Get("ETag"))
	}
	// A stale ETag still serves the body.
	if r = e.do("GET", base+"/video.mp4", nil, withHeader("If-None-Match", `"stale"`)); r.code != 200 {
		t.Fatalf("stale etag: %d", r.code)
	}

	// 416: every shape ServeContent rejects answers the JSON envelope with
	// Content-Range: bytes */size, no-store and zero GETs — beyond the end,
	// malformed, all-out-of-bounds multi-range, and beyond the end with a
	// matching If-Range (which makes the Range binding).
	for _, tc := range []struct{ rng, ifRange string }{
		{"bytes=99999-", ""},
		{"bytes=abc-", ""},
		{"bytes=5000-,6000-", ""},
		{"bytes=99999-", etag},
	} {
		before = e.gets()
		opts := []reqOpt{withHeader("Range", tc.rng)}
		if tc.ifRange != "" {
			opts = append(opts, withHeader("If-Range", tc.ifRange))
		}
		r = e.do("GET", base+"/video.mp4", nil, opts...)
		if r.code != 416 || r.errCode() != "range_not_satisfiable" || r.hdr.Get("Content-Type") != "application/json; charset=utf-8" ||
			r.hdr.Get("Content-Range") != "bytes */4096" || r.hdr.Get("Cache-Control") != "no-store" || e.gets() != before {
			t.Fatalf("416 for Range %q If-Range %q: %d %v %s gets=%d", tc.rng, tc.ifRange, r.code, r.hdr, r.body, e.gets()-before)
		}
		if strings.Contains(string(r.body), "invalid range") {
			t.Fatalf("stdlib text leaked into the envelope: %s", r.body)
		}
	}
	// If-Range with a stale validator ignores the Range (200), never 416.
	if r = e.do("GET", base+"/video.mp4", nil, withHeader("Range", "bytes=99999-"), withHeader("If-Range", `"stale"`)); r.code != 200 {
		t.Fatalf("If-Range stale: %d", r.code)
	}
	// A matching If-Range with a satisfiable Range is a normal 206.
	if r = e.do("GET", base+"/video.mp4", nil, withHeader("Range", "bytes=0-9"), withHeader("If-Range", etag)); r.code != 206 || len(r.body) != 10 {
		t.Fatalf("If-Range match: %d len=%d", r.code, len(r.body))
	}

	// HEAD: headers incl. Content-Length, no body, zero GETs.
	before = e.gets()
	r = e.do("HEAD", base+"/video.mp4", nil)
	if r.code != 200 || len(r.body) != 0 || r.hdr.Get("Content-Length") != "4096" || r.hdr.Get("ETag") != etag || e.gets() != before {
		t.Fatalf("HEAD: %d len=%d CL=%q gets=%d", r.code, len(r.body), r.hdr.Get("Content-Length"), e.gets()-before)
	}

	// social.mp4 serves the rendition bytes and ETag (COALESCE); poster is JPEG.
	r = e.do("GET", base+"/social.mp4", nil)
	if r.code != 200 || string(r.body) != string(rendition) || r.hdr.Get("ETag") != etag || r.hdr.Get("Content-Type") != "video/mp4" {
		t.Fatalf("social: %d %v", r.code, r.hdr)
	}
	r = e.do("GET", base+"/poster.jpg", nil)
	if r.code != 200 || string(r.body) != string(poster) || r.hdr.Get("Content-Type") != "image/jpeg" || r.hdr.Get("ETag") != `"`+sha256hex(poster)+`"` {
		t.Fatalf("poster: %d %v", r.code, r.hdr)
	}

	// Method mismatch: POST on a media route is 405 with Allow: GET, HEAD,
	// still under the media header set.
	r = e.do("POST", base+"/video.mp4", nil)
	if r.code != 405 || r.hdr.Get("Allow") != "GET, HEAD" || r.errCode() != "method_not_allowed" || r.hdr.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("405: %d %q %s %v", r.code, r.hdr.Get("Allow"), r.body, r.hdr)
	}
}

// TestUnfurlRegressionSocialMP4 is the media half of the unfurl regression
// gate (medal #7; the tag-inventory half is internal/web.TestUnfurlRegression):
// the stable social.mp4 route the cards point at must answer a plain GET
// with 200 video/mp4 and a ranged GET with 206 — Discord/X fetch it exactly
// like this before rendering a playable card.
func TestUnfurlRegressionSocialMP4(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{})
	social := "/api/clips/" + c.ID + "/social.mp4"

	r := e.do("GET", social, nil)
	if r.code != 200 || r.hdr.Get("Content-Type") != "video/mp4" || string(r.body) != string(rendition) ||
		r.hdr.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("social 200: %d %v len=%d", r.code, r.hdr, len(r.body))
	}
	r = e.do("GET", social, nil, withHeader("Range", "bytes=0-99"))
	if r.code != 206 || len(r.body) != 100 || r.hdr.Get("Content-Range") != "bytes 0-99/"+strconv.Itoa(len(rendition)) {
		t.Fatalf("social 206: %d %v len=%d", r.code, r.hdr, len(r.body))
	}
	// Phase 1 rows have NULL social columns, so the 200/206 above also prove
	// the §8.5 COALESCE fallback to the rendition — never a 404/503.
}

func TestMediaNotReadyAndGone(t *testing.T) {
	e := newEnv(t)
	owner, ownerCookie := e.newUser("owner")
	// Order matters: clips that claim a job come before the one left queued.
	processing := e.processingClip(owner, clipOpts{})
	failed := e.failedClip(owner, clipOpts{})
	queued := e.seedClip(owner, clipOpts{})
	deleting := e.deletingClip(owner)

	for _, id := range []string{processing.ID, queued.ID, failed.ID} {
		for _, route := range []string{"/video.mp4", "/social.mp4", "/poster.jpg"} {
			r := e.do("GET", "/api/clips/"+id+route, nil)
			if r.code != 409 || r.errCode() != "not_ready" {
				t.Fatalf("%s %s: %d %s", id, route, r.code, r.body)
			}
			if r.hdr.Get("Cache-Control") != "no-store" {
				t.Fatalf("409 must be no-store: %q", r.hdr.Get("Cache-Control"))
			}
		}
	}
	// The original is owner-only: send the owner's cookie so the 404 is
	// what is being tested (an anonymous caller gets 401 before lookup).
	for _, route := range []string{"/video.mp4", "/social.mp4", "/poster.jpg", "/original"} {
		r := e.do("GET", "/api/clips/"+deleting.ID+route, nil, withCookie(ownerCookie))
		if r.code != 404 || r.errCode() != "not_found" {
			t.Fatalf("deleting %s: %d %s", route, r.code, r.body)
		}
		if r = e.do("GET", "/api/clips/"+strings.Repeat("N", 12)+route, nil, withCookie(ownerCookie)); r.code != 404 {
			t.Fatalf("unknown %s: %d", route, r.code)
		}
		if r = e.do("GET", "/api/clips/bad-id"+route, nil, withCookie(ownerCookie)); r.code != 404 {
			t.Fatalf("malformed %s: %d", route, r.code)
		}
	}
	if e.gets() != 0 {
		t.Fatalf("storage touched for non-ready clips: %v", e.fake.CallsSnapshot())
	}
	if strings.Contains(e.logBuf.String(), "level=ERROR") {
		t.Fatalf("not-ready clips are not an error: %s", e.logBuf.String())
	}
}

func TestMediaReadyRowWithNullKeys(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, _, poster := e.readyClip(owner, clipOpts{})
	base := "/api/clips/" + c.ID
	e.nullClipColumns(c.ID, "rendition_key", "rendition_size", "rendition_etag")

	// A ready row missing the rendition triple is an invariant violation:
	// 503 unavailable (never 404, never a misleading not_ready), logged at
	// ERROR, zero storage calls. social.mp4 falls back to the same NULLs.
	for _, route := range []string{"/video.mp4", "/social.mp4"} {
		e.logBuf.Reset()
		r := e.do("GET", base+route, nil)
		if r.code != 503 || r.errCode() != "unavailable" || r.hdr.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %s %v", route, r.code, r.body, r.hdr)
		}
		logs := e.logBuf.String()
		if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "NULL") || !strings.Contains(logs, "route="+route[1:]) {
			t.Fatalf("%s not logged at ERROR: %s", route, logs)
		}
	}
	// The poster triple is intact: still served.
	if r := e.do("GET", base+"/poster.jpg", nil); r.code != 200 || string(r.body) != string(poster) {
		t.Fatalf("poster: %d", r.code)
	}
	if e.gets() != 1 {
		t.Fatalf("GetRange calls = %d, want 1 (poster only): %v", e.gets(), e.fake.CallsSnapshot())
	}
}

// Derived media dates from `updated_at`, so a cache holding pre-trim bytes
// cannot be told 304 by date alone. A trim (SwapRendition), a GIF regenerate
// (SetGIF) and a filmstrip (SetFilmstrip) all replace bytes under these stable
// URLs and all stamp `updated_at`, while `ready_at` stays where the transcode
// put it (§20 errata 70, 76 and 220). The original keeps `created_at`: its bytes
// never change.
func TestMediaLastModifiedFollowsUpdatedAt(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	c = e.withGIF(c, gifKey(e, c.ID))
	c = e.withFilmstrip(c, filmstripKeyFor(e, c.ID))
	base := "/api/clips/" + c.ID
	derived := []string{"/video.mp4", "/social.mp4", "/poster.jpg", "/preview.gif", "/filmstrip.jpg"}

	// Before any swap the date is the row's, and a date-only revalidation
	// still costs nothing.
	was := time.Unix(c.UpdatedAt, 0).UTC()
	for _, route := range derived {
		// (A 304 carries no Last-Modified — net/http drops it next to an
		// ETag — so the header is read off the 200.)
		if r := e.do("GET", base+route, nil); r.hdr.Get("Last-Modified") != was.Format(http.TimeFormat) {
			t.Fatalf("%s Last-Modified = %q, want %q", route, r.hdr.Get("Last-Modified"), was.Format(http.TimeFormat))
		}
		if r := e.do("GET", base+route, nil, withHeader("If-Modified-Since", was.Format(http.TimeFormat))); r.code != 304 {
			t.Fatalf("%s before the swap: %d, want 304", route, r.code)
		}
	}

	// The swap: new bytes under the same keys an hour later. Written here
	// directly because the point is the ROW a trim leaves behind — ready_at
	// untouched, updated_at moved (the real writers are covered in
	// internal/db) — and the bytes the fake store holds are irrelevant to
	// which validator the route hands out.
	e.setClipColumn(c.ID, "updated_at", c.UpdatedAt+3600)
	now := time.Unix(c.UpdatedAt+3600, 0).UTC()
	for _, route := range derived {
		r := e.do("GET", base+route, nil, withHeader("If-Modified-Since", was.Format(http.TimeFormat)))
		if r.code != 200 {
			t.Errorf("%s after the swap: %d, want 200 — the bytes changed", route, r.code)
		}
		if got := r.hdr.Get("Last-Modified"); got != now.Format(http.TimeFormat) {
			t.Errorf("%s Last-Modified = %q, want %q", route, got, now.Format(http.TimeFormat))
		}
		if r := e.do("GET", base+route, nil, withHeader("If-Modified-Since", now.Format(http.TimeFormat))); r.code != 304 {
			t.Errorf("%s revalidating the new date: %d, want 304", route, r.code)
		}
	}
	// The original is not derived: it still dates from the upload, whatever
	// the row's updated_at says.
	created := time.Unix(c.CreatedAt, 0).UTC()
	r := e.do("GET", base+"/original", nil, withCookie(cookie),
		withHeader("If-Modified-Since", created.Format(http.TimeFormat)))
	if r.code != 304 {
		t.Errorf("original after the swap: %d, want 304", r.code)
	}
}

func TestOriginalRoute(t *testing.T) {
	e := newEnv(t)
	owner, ownerCookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")
	src := testVideo(2222)
	c := e.processingClip(owner, clipOpts{source: src})
	path := "/api/clips/" + c.ID + "/original"
	etag := `"` + c.SourceETag + `"`

	// Owner while processing: 200 (never 409), ETag = source_etag, attachment.
	r := e.do("GET", path, nil, withCookie(ownerCookie))
	if r.code != 200 || string(r.body) != string(src) || r.hdr.Get("ETag") != etag || etag != `"`+sha256hex(src)+`"` {
		t.Fatalf("original: %d %v", r.code, r.hdr)
	}
	if cd := r.hdr.Get("Content-Disposition"); cd != `attachment; filename=orig.mp4` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if r.hdr.Get("Content-Type") != "video/mp4" || r.hdr.Get("Content-Length") != "2222" {
		t.Fatalf("headers: %v", r.hdr)
	}
	// Owner-only bytes are never "public": a shared cache must not replay
	// them to the next caller of the URL, and the credential is a cache key.
	if cc := r.hdr.Get("Cache-Control"); cc != "private, max-age=3600" || strings.Contains(cc, "public") {
		t.Fatalf("original Cache-Control = %q", cc)
	}
	if r.hdr.Get("Vary") != "Cookie, Authorization" {
		t.Fatalf("original Vary = %q", r.hdr.Get("Vary"))
	}
	// 304 with zero storage calls, still private.
	before := e.gets()
	r = e.do("GET", path, nil, withCookie(ownerCookie), withHeader("If-None-Match", etag))
	if r.code != 304 || e.gets() != before || r.hdr.Get("Cache-Control") != "private, max-age=3600" {
		t.Fatalf("original 304: %d gets=%d %v", r.code, e.gets()-before, r.hdr)
	}
	// Range works on the original too.
	if r = e.do("GET", path, nil, withCookie(ownerCookie), withHeader("Range", "bytes=0-9")); r.code != 206 || len(r.body) != 10 {
		t.Fatalf("original 206: %d len=%d", r.code, len(r.body))
	}
	// Non-owner 403, anonymous 401; nothing read from storage.
	before = e.gets()
	if r = e.do("GET", path, nil, withCookie(otherCookie)); r.code != 403 || r.errCode() != "forbidden" {
		t.Fatalf("other: %d %s", r.code, r.body)
	}
	if r = e.do("GET", path, nil); r.code != 401 {
		t.Fatalf("anonymous: %d", r.code)
	}
	if e.gets() != before {
		t.Fatal("storage touched for a forbidden request")
	}
	// Failed clips still serve their original to the owner.
	f := e.failedClip(owner, clipOpts{})
	if r = e.do("GET", "/api/clips/"+f.ID+"/original", nil, withCookie(ownerCookie)); r.code != 200 {
		t.Fatalf("failed original: %d %s", r.code, r.body)
	}
	// A Bearer token cannot download the original: owner routes require a
	// browser session in Phase 1 (§14 errata 19), so a token gets 403
	// token_not_allowed — the upload credential is not a download credential.
	tok := e.newToken(owner, "upload")
	if r = e.do("GET", path, nil, withBearer(tok)); r.code != 403 || r.errCode() != "token_not_allowed" {
		t.Fatalf("token original: %d %s", r.code, r.body)
	}
}

// TestOriginalErrorsAreNotCacheable pins §20 erratum 55. `/original` is the
// one route under the Resolver that `timeout` does not preset, so its 403 and
// 404 answered with no cache directive at all — while the sliding session
// cookie (erratum 25, whose safety argument is that every Resolver-covered
// route is `no-store` or `private`) rides on them. RFC 9110 §15.5.5 lets a
// shared cache store a 404 keyed on the URL alone and replay it — credential
// included — to the next caller of that URL.
func TestOriginalErrorsAreNotCacheable(t *testing.T) {
	e := newEnv(t)
	owner, ownerCookie := e.newUser("owner")
	_, otherCookie := e.newUser("other")
	c := e.processingClip(owner, clipOpts{})

	// Each cookie's FIRST request is the one that slides the session, so the
	// credential really is on the response under test.
	tests := []struct {
		name   string
		path   string
		cookie *http.Cookie
		code   int
	}{
		{"unknown clip", "/api/clips/" + strings.Repeat("a", 12) + "/original", ownerCookie, 404},
		{"not the owner", "/api/clips/" + c.ID + "/original", otherCookie, 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := e.do("GET", tc.path, nil, withCookie(tc.cookie))
			if r.code != tc.code {
				t.Fatalf("%d %s", r.code, r.body)
			}
			if e.sessionCookie(r) == nil {
				t.Fatal("no sliding cookie on this response: the case proves nothing")
			}
			if cc := r.hdr.Get("Cache-Control"); cc != "private, no-store" {
				t.Errorf("Cache-Control = %q on a credentialed %d", cc, tc.code)
			}
			if v := r.hdr.Get("Vary"); v != "Cookie, Authorization" {
				t.Errorf("Vary = %q", v)
			}
		})
	}
}

func TestMediaStorageErrors(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{})
	base := "/api/clips/" + c.ID

	// A transfer that ends short — the seeker reports io.ErrUnexpectedEOF
	// for a short object and a store body does the same for a dropped
	// link — is WARN, never ERROR: a flaky bucket link must not page anyone.
	e.fake.FailNext("GetRange", io.ErrUnexpectedEOF)
	r := e.do("GET", base+"/video.mp4", nil)
	if r.code != 200 || len(r.body) != 0 {
		t.Fatalf("truncated: %d len=%d", r.code, len(r.body))
	}
	if logs := e.logBuf.String(); !strings.Contains(logs, "level=WARN") || strings.Contains(logs, "level=ERROR") {
		t.Fatalf("truncated transfer not WARN: %s", logs)
	}
	e.logBuf.Reset()
	e.fake.Put(*c.RenditionKey, rendition[:100], "video/mp4")
	r = e.do("GET", base+"/video.mp4", nil)
	if r.code != 200 || len(r.body) != 100 || r.hdr.Get("Content-Length") != "4096" {
		t.Fatalf("short object: %d len=%d CL=%q", r.code, len(r.body), r.hdr.Get("Content-Length"))
	}
	if logs := e.logBuf.String(); !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "short") || strings.Contains(logs, "level=ERROR") {
		t.Fatalf("short object not WARN: %s", logs)
	}
	e.logBuf.Reset()

	// Object missing behind a ready row: the status is committed (200) but
	// the drift is logged at ERROR — the one ERROR on this path.
	if _, err := e.fake.DeletePrefix(context.Background(), *c.RenditionKey); err != nil {
		t.Fatal(err)
	}
	r = e.do("GET", base+"/video.mp4", nil)
	if r.code != 200 || len(r.body) != 0 {
		t.Fatalf("drift: %d len=%d", r.code, len(r.body))
	}
	logs := e.logBuf.String()
	if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "drift") {
		t.Fatalf("drift not logged at ERROR: %s", logs)
	}
	e.logBuf.Reset()

	// A client disconnect mid-stream is not an error at all: the request
	// context is cancelled the moment the body GET starts, so the first
	// Read fails with context.Canceled.
	ctx, cancel := context.WithCancel(context.Background())
	e.fake.Hook = func(hctx context.Context, op, key string) error {
		if op == "GetRange" {
			cancel()
		}
		return nil
	}
	r = e.do("GET", base+"/poster.jpg", nil, func(req *http.Request) { *req = *req.WithContext(ctx) })
	e.fake.Hook = nil
	if r.code != 200 || len(r.body) != 0 {
		t.Fatalf("disconnect: %d len=%d", r.code, len(r.body))
	}
	if strings.Contains(e.logBuf.String(), "level=ERROR") || strings.Contains(e.logBuf.String(), "level=WARN") {
		t.Fatalf("client disconnect logged above INFO: %s", e.logBuf.String())
	}
	e.logBuf.Reset()

	// Any other storage error is WARN.
	e.fake.FailNext("GetRange", store.ErrIdleTimeout)
	r = e.do("GET", base+"/poster.jpg", nil)
	if !strings.Contains(e.logBuf.String(), "level=WARN") || strings.Contains(e.logBuf.String(), "level=ERROR") {
		t.Fatalf("storage error not WARN: %s", e.logBuf.String())
	}

	// Nil store → 503 storage_unavailable (never 404).
	e2 := newEnv(t, func(d *Deps) { d.Store = nil })
	owner2, _ := e2.newUser("o")
	c2, _, _ := e2.readyClip(owner2, clipOpts{})
	if r = e2.do("GET", "/api/clips/"+c2.ID+"/video.mp4", nil); r.code != 503 || r.errCode() != "storage_unavailable" {
		t.Fatalf("nil store: %d %s", r.code, r.body)
	}
}

// A viewer pressing play as the clip is deleted or expires: the row
// snapshot predates the body GET, so the object is legitimately gone by the
// time GetRange runs. §12 classifies that lost race as INFO — a green run
// emits zero ERROR lines. Only a live row still referencing the missing key
// is drift (covered in TestMediaStorageErrors).
func TestMediaDeleteRaceIsInfoNotError(t *testing.T) {
	for name, destroy := range map[string]func(e *env, id string){
		"row deleted": func(e *env, id string) {
			if _, err := e.lifecycleDestroy(context.Background(), id); err != nil {
				e.t.Errorf("destroy: %v", err)
			}
		},
		"tombstoned": func(e *env, id string) {
			if err := e.db.MarkDeleting(context.Background(), id, 1); err != nil {
				e.t.Errorf("mark deleting: %v", err)
			}
			if _, err := e.fake.DeletePrefix(context.Background(), store.ClipPrefix(e.cfg.S3.KeyPrefix, id)); err != nil {
				e.t.Errorf("delete prefix: %v", err)
			}
		},
		// The exact race the fix names: a concurrent retry mints fresh
		// per-attempt keys and sweeps the old object, so the LIVE (non-
		// deleting) row no longer references the key being streamed.
		// confirmDrift must read that as a lost race, not drift — otherwise
		// every retry-during-stream would emit a spurious ERROR (§12).
		"re-keyed by a concurrent retry": func(e *env, id string) {
			c := e.getClip(id)
			e.setClipColumn(id, "rendition_key", store.RenditionKey(e.cfg.S3.KeyPrefix, id, "n3wn3wn3"))
			if _, err := e.fake.DeletePrefix(context.Background(), *c.RenditionKey); err != nil {
				e.t.Errorf("delete old rendition: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			owner, _ := e.newUser("owner")
			c, _, _ := e.readyClip(owner, clipOpts{})
			e.logBuf.Reset()
			e.fake.Hook = func(_ context.Context, op, _ string) error {
				if op == "GetRange" {
					e.fake.Hook = nil
					destroy(e, c.ID)
				}
				return nil
			}
			r := e.do("GET", "/api/clips/"+c.ID+"/video.mp4", nil)
			if r.code != 200 || len(r.body) != 0 {
				t.Fatalf("race: %d len=%d", r.code, len(r.body))
			}
			logs := e.logBuf.String()
			if strings.Contains(logs, "level=ERROR") || strings.Contains(logs, "level=WARN") {
				t.Fatalf("lost race logged above INFO: %s", logs)
			}
			if !strings.Contains(logs, "lost race") {
				t.Fatalf("lost race not logged at INFO: %s", logs)
			}
		})
	}
}

// confirmDrift re-reads the row on a detached context to tell drift from a
// lost race; when that re-read itself fails (the DB is unavailable) the
// outcome is WARN "row re-read failed", never the ERROR drift verdict — a
// transient DB blip on a viewer's stream must not page anyone (§12).
func TestMediaDriftRecheckFailureIsWarnNotError(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	e.logBuf.Reset()
	e.fake.Hook = func(_ context.Context, op, _ string) error {
		if op == "GetRange" {
			e.fake.Hook = nil
			// The object vanishes and the DB goes away before the recheck.
			if _, err := e.fake.DeletePrefix(context.Background(), *c.RenditionKey); err != nil {
				e.t.Errorf("delete rendition: %v", err)
			}
			_ = e.db.Close()
		}
		return nil
	}
	r := e.do("GET", "/api/clips/"+c.ID+"/video.mp4", nil)
	if r.code != 200 || len(r.body) != 0 {
		t.Fatalf("recheck-fail: %d len=%d", r.code, len(r.body))
	}
	logs := e.logBuf.String()
	if strings.Contains(logs, "level=ERROR") {
		t.Fatalf("re-read failure must not be ERROR drift: %s", logs)
	}
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "row re-read failed") {
		t.Fatalf("re-read failure not WARN: %s", logs)
	}
}

// A Range header may list at most maxRanges range-specs (§20 erratum 23).
// Each non-adjacent range costs one GetRange, so without the cap an
// anonymous request could buy thousands of sequential storage GETs with a
// kilobyte of header; at the cap the §8.5 one-GET-per-range cost still
// applies, one over it answers 416 with zero storage calls.
func TestMediaRangeCountCap(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{})
	base := "/api/clips/" + c.ID
	size := len(rendition)
	// n one-byte ranges, descending with a gap, so the seeker can never
	// reuse an open body: n ranges = n GetRange calls.
	descending := func(n int) string {
		specs := make([]string, 0, n)
		for i := 0; i < n; i++ {
			off := strconv.Itoa(size - 1 - 2*i)
			specs = append(specs, off+"-"+off)
		}
		return "bytes=" + strings.Join(specs, ",")
	}
	// Adjacent ascending one-byte ranges: the seeker streams them from one
	// body, but the cap is on the count, not the cost.
	ascending := func(n int) string {
		specs := make([]string, 0, n)
		for i := 0; i < n; i++ {
			specs = append(specs, strconv.Itoa(i)+"-"+strconv.Itoa(i))
		}
		return "bytes=" + strings.Join(specs, ",")
	}

	r := e.do("GET", base+"/video.mp4", nil, withHeader("Range", descending(maxRanges)))
	if r.code != 206 || !strings.HasPrefix(r.hdr.Get("Content-Type"), "multipart/byteranges") {
		t.Fatalf("%d ranges: %d %q %s", maxRanges, r.code, r.hdr.Get("Content-Type"), r.body)
	}
	if e.gets() != maxRanges {
		t.Fatalf("GetRange calls = %d, want %d (one per non-adjacent range): %v", e.gets(), maxRanges, e.fake.CallsSnapshot())
	}

	before := e.gets()
	for _, rng := range []string{descending(maxRanges + 1), descending(1000), ascending(1000), "bytes=0-0, 2-2 ,4-4,6-6,8-8,10-10,12-12,14-14,16-16"} {
		for _, method := range []string{"GET", "HEAD"} {
			r := e.do(method, base+"/video.mp4", nil, withHeader("Range", rng))
			if r.code != 416 || r.hdr.Get("Content-Range") != "bytes */"+strconv.Itoa(size) || r.hdr.Get("Cache-Control") != "no-store" {
				t.Fatalf("%s %d ranges: %d %v %s", method, rangeCount(rng), r.code, r.hdr, r.body)
			}
			if method == "GET" && (r.errCode() != "range_not_satisfiable" || !strings.Contains(r.str("message"), "at most 8 ranges")) {
				t.Fatalf("envelope: %s", r.body)
			}
			if e.gets() != before {
				t.Fatalf("%s %d ranges touched storage: %v", method, rangeCount(rng), e.fake.CallsSnapshot())
			}
		}
	}
	// The cap counts range-specs, not commas: empty specs are ignored by
	// ServeContent and by the count alike.
	if r := e.do("GET", base+"/video.mp4", nil, withHeader("Range", "bytes=0-0,,,,,,,,,,,,")); r.code != 206 {
		t.Fatalf("empty specs: %d %s", r.code, r.body)
	}
	// The owner's original is capped the same way.
	if r := e.do("GET", base+"/original", nil, withCookie(cookie), withHeader("Range", descending(maxRanges+1))); r.code != 416 {
		t.Fatalf("original: %d %s", r.code, r.body)
	}
}

// A multi-range response is written by a goroutine http.ServeContent starts
// and never joins: when the copy into the client ends early (a disconnect,
// a seek-away, any write error) serveObject returns while that goroutine is
// still inside the seeker. Closing and classifying under it must be safe —
// no torn state, no fresh GetRange for a request that has already ended
// (§20 erratum 41; the data race itself is what the -race CI pass catches).
func TestMediaMultiRangeAbortedCopy(t *testing.T) {
	var slow *slowStore
	e := newEnv(t, func(d *Deps) {
		slow = &slowStore{Store: d.Store, delay: 20 * time.Millisecond}
		d.Store = slow
	})
	owner, _ := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{})
	size := len(rendition)

	// Ranges long enough that the copy of ONE of them spans many reads, so
	// the write into the client fails while the goroutine is mid-range —
	// i.e. inside the seeker, not blocked on the pipe.
	specs := make([]string, 0, maxRanges)
	for i := 0; i < maxRanges; i++ { // non-adjacent: one GetRange per range
		end := size - 1 - 300*i
		specs = append(specs, strconv.Itoa(end-255)+"-"+strconv.Itoa(end))
	}
	req := httptest.NewRequest("GET", "/api/clips/"+c.ID+"/video.mp4", nil)
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("Range", "bytes="+strings.Join(specs, ","))
	w := &failAfterWriter{hdr: http.Header{}, budget: 1}
	e.h.ServeHTTP(w, req) // must return while the byteranges goroutine still reads

	after := e.gets()
	time.Sleep(200 * time.Millisecond) // sleep: a negative check (no GET after the handler returned); load can only lengthen it
	if e.gets() != after {
		t.Fatalf("a storage GET was issued after the handler returned: %v", e.fake.CallsSnapshot())
	}
	if e.gets() > maxRanges {
		t.Fatalf("GetRange calls = %d, want at most %d", e.gets(), maxRanges)
	}
	if strings.Contains(e.logBuf.String(), "level=ERROR") {
		t.Fatalf("an aborted copy is not an error: %s", e.logBuf.String())
	}
}

// failAfterWriter accepts budget bytes and then fails every write, like a
// client that went away mid-response.
type failAfterWriter struct {
	hdr     http.Header
	budget  int
	written int
}

func (w *failAfterWriter) Header() http.Header { return w.hdr }
func (w *failAfterWriter) WriteHeader(int)     {}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.written >= w.budget {
		return 0, errors.New("connection reset by peer")
	}
	w.written += len(p)
	return len(p), nil
}

// slowStore hands out bodies that dribble a range out in small, slow reads,
// so the aborted copy above leaves a read in flight rather than a goroutine
// parked on the pipe.
type slowStore struct {
	store.Store
	delay time.Duration
}

func (s *slowStore) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	rc, err := s.Store.GetRange(ctx, key, start, end)
	if err != nil {
		return nil, err
	}
	return &slowBody{ReadCloser: rc, delay: s.delay}, nil
}

type slowBody struct {
	io.ReadCloser
	delay time.Duration
}

const slowBodyChunk = 32

func (b *slowBody) Read(p []byte) (int, error) {
	time.Sleep(b.delay) // sleep: a slow body IS the subject
	if len(p) > slowBodyChunk {
		p = p[:slowBodyChunk]
	}
	return b.ReadCloser.Read(p)
}

// The two range gates stand aside when a precondition pre-empts the Range
// (§20 erratum 43). RFC 9110 §13.1.5 makes a server IGNORE the Range when
// the If-Range validator does not match (→ the full representation), and
// §13.2.2 resolves If-Match / If-None-Match before Range is looked at (→ 412
// / 304) — so a request that must be answered 200/304/412 must not become a
// 416 just because a gate saw the header first. Where the Range really is
// binding the gates still bite.
func TestMediaRangeGatesRespectPreconditions(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{})
	base := "/api/clips/" + c.ID
	etag := `"` + sha256hex(rendition) + `"`
	modTime := time.Unix(c.UpdatedAt, 0).UTC() // what the derived routes date from

	specs := make([]string, 0, maxRanges+1)
	for i := 0; i <= maxRanges; i++ {
		off := strconv.Itoa(len(rendition) - 1 - 2*i)
		specs = append(specs, off+"-"+off)
	}
	nine := "bytes=" + strings.Join(specs, ",") // one over the cap

	for _, tc := range []struct {
		name string
		hdrs [][2]string
		want int
		gets int
	}{
		// Ignored Range → the full representation, at the usual one GET.
		{"stale If-Range + zero suffix", [][2]string{{"Range", "bytes=-0"}, {"If-Range", `"stale"`}}, 200, 1},
		{"stale If-Range + too many ranges", [][2]string{{"Range", nine}, {"If-Range", `"stale"`}}, 200, 1},
		{"stale If-Range date + too many ranges", [][2]string{{"Range", nine}, {"If-Range", modTime.Add(-time.Hour).Format(http.TimeFormat)}}, 200, 1},
		// 304 / 412 before the Range is looked at, zero GETs.
		{"If-None-Match match + zero suffix", [][2]string{{"Range", "bytes=-0"}, {"If-None-Match", etag}}, 304, 0},
		{"If-None-Match star + too many ranges", [][2]string{{"Range", nine}, {"If-None-Match", "*"}}, 304, 0},
		{"If-None-Match weak match + zero suffix", [][2]string{{"Range", "bytes=-0"}, {"If-None-Match", `W/` + etag}}, 304, 0},
		{"If-None-Match list + too many ranges", [][2]string{{"Range", nine}, {"If-None-Match", `"other", ` + etag}}, 304, 0},
		{"If-Modified-Since + zero suffix", [][2]string{{"Range", "bytes=-0"}, {"If-Modified-Since", modTime.Format(http.TimeFormat)}}, 304, 0},
		{"If-Match fails + zero suffix", [][2]string{{"Range", "bytes=-0"}, {"If-Match", `"stale"`}}, 412, 0},
		{"If-Match garbage + too many ranges", [][2]string{{"Range", nine}, {"If-Match", "garbage"}}, 412, 0},
		{"If-Unmodified-Since fails + too many ranges", [][2]string{{"Range", nine}, {"If-Unmodified-Since", modTime.Add(-2 * time.Second).Format(http.TimeFormat)}}, 412, 0},
		// Binding after all: the gates answer 416 with zero storage calls.
		{"no precondition + zero suffix", [][2]string{{"Range", "bytes=-0"}}, 416, 0},
		{"matching If-Range + zero suffix", [][2]string{{"Range", "bytes=-0"}, {"If-Range", etag}}, 416, 0},
		{"matching If-Range + too many ranges", [][2]string{{"Range", nine}, {"If-Range", etag}}, 416, 0},
		{"matching If-Range date + zero suffix", [][2]string{{"Range", "bytes=-0"}, {"If-Range", modTime.Format(http.TimeFormat)}}, 416, 0},
		{"If-None-Match miss + too many ranges", [][2]string{{"Range", nine}, {"If-None-Match", `"stale"`}}, 416, 0},
		{"If-Match match + zero suffix", [][2]string{{"Range", "bytes=-0"}, {"If-Match", etag}}, 416, 0},
		// A weak If-Range validator may not be used at all: the Range is
		// ignored and the full representation served, never 416.
		{"weak If-Range + too many ranges", [][2]string{{"Range", nine}, {"If-Range", `W/` + etag}}, 200, 1},
	} {
		before := e.gets()
		opts := make([]reqOpt, 0, len(tc.hdrs))
		for _, h := range tc.hdrs {
			opts = append(opts, withHeader(h[0], h[1]))
		}
		r := e.do("GET", base+"/video.mp4", nil, opts...)
		if r.code != tc.want || e.gets()-before != tc.gets {
			t.Errorf("%s: %d (want %d), %d GETs (want %d): %s", tc.name, r.code, tc.want, e.gets()-before, tc.gets, r.body)
			continue
		}
		if tc.want == 200 && len(r.body) != len(rendition) {
			t.Errorf("%s: ignored Range must serve the whole body, got %d bytes", tc.name, len(r.body))
		}
	}
	// The owner's original follows the same rule.
	if r := e.do("GET", base+"/original", nil, withCookie(cookie), withHeader("Range", nine), withHeader("If-Range", `"stale"`)); r.code != 200 {
		t.Fatalf("original: %d %s", r.code, r.body)
	}
}

// A Range header naming a unit this server does not implement must be
// IGNORED (RFC 9110 §14.2 MUST), and `bytes` is case-insensitive (§14.1) —
// http.ServeContent honours neither, and its 416 arrives dressed as this
// package's own answer (§20 erratum 44).
func TestMediaRangeUnit(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{})
	base := "/api/clips/" + c.ID
	size := strconv.Itoa(len(rendition))

	// Unknown units: full representation, one GET, no 416.
	for _, rng := range []string{"items=0-1", "seconds=0-1", "BYTESX=0-1", "chars=0-9,20-29", "items=" + strings.Repeat("0-1,", 50) + "0-1"} {
		before := e.gets()
		r := e.do("GET", base+"/video.mp4", nil, withHeader("Range", rng))
		if r.code != 200 || len(r.body) != len(rendition) || r.hdr.Get("Content-Length") != size || e.gets()-before != 1 {
			t.Fatalf("Range %q: %d len=%d CL=%q gets=%d", rng, r.code, len(r.body), r.hdr.Get("Content-Length"), e.gets()-before)
		}
		if r.hdr.Get("Content-Range") != "" {
			t.Fatalf("Range %q: 200 must carry no Content-Range: %v", rng, r.hdr)
		}
	}
	// A "bytes" unit in any case is honoured, not merely ignored.
	for _, rng := range []string{"bytes=0-9", "BYTES=0-9", "Bytes=0-9"} {
		r := e.do("GET", base+"/video.mp4", nil, withHeader("Range", rng))
		if r.code != 206 || len(r.body) != 10 || r.hdr.Get("Content-Range") != "bytes 0-9/"+size {
			t.Fatalf("Range %q: %d len=%d %q", rng, r.code, len(r.body), r.hdr.Get("Content-Range"))
		}
	}
	// The gates still apply to a case-variant unit.
	if r := e.do("GET", base+"/video.mp4", nil, withHeader("Range", "BYTES=-0")); r.code != 416 {
		t.Fatalf("BYTES=-0: %d %s", r.code, r.body)
	}
	// A malformed ranges-specifier (no "=") is still rejected, as before.
	if r := e.do("GET", base+"/video.mp4", nil, withHeader("Range", "bytes 0-1")); r.code != 416 {
		t.Fatalf("bytes 0-1: %d %s", r.code, r.body)
	}
	// Same on the owner's original and on HEAD.
	if r := e.do("HEAD", base+"/video.mp4", nil, withHeader("Range", "items=0-1")); r.code != 200 || r.hdr.Get("Content-Length") != size {
		t.Fatalf("HEAD items: %d %v", r.code, r.hdr)
	}
	if r := e.do("GET", base+"/original", nil, withCookie(cookie), withHeader("Range", "items=0-1")); r.code != 200 {
		t.Fatalf("original items: %d %s", r.code, r.body)
	}
}

func TestNormalizeRangeUnit(t *testing.T) {
	for in, want := range map[string]string{
		"":                "",
		"bytes=0-1":       "bytes=0-1",
		"BYTES=0-1":       "bytes=0-1",
		"Bytes=0-1, 5-6":  "bytes=0-1, 5-6",
		"bytes=":          "bytes=",
		"items=0-1":       "",
		"bytes 0-1":       "bytes 0-1", // no "=": an invalid specifier, left as is
		"garbage":         "garbage",
		"bytes =0-1":      "", // BWS is not a bytes unit either
		"=0-1":            "",
		"bytesx=0-1":      "",
		"bytes=0-1,-0":    "bytes=0-1,-0",
		"BYTES=-0":        "bytes=-0",
		"bytes=99999999-": "bytes=99999999-",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if in != "" {
			r.Header.Set("Range", in)
		}
		got := normalizeRangeUnit(r)
		if got != want {
			t.Errorf("normalizeRangeUnit(%q) = %q, want %q", in, got, want)
		}
		if r.Header.Get("Range") != want {
			t.Errorf("normalizeRangeUnit(%q) left Range = %q, want %q", in, r.Header.Get("Range"), want)
		}
	}
}

func TestScanETagAndListMatch(t *testing.T) {
	for in, want := range map[string]string{
		`"abc"`:            `"abc"`,
		`W/"abc"`:          `W/"abc"`,
		`"abc", "def"`:     `"abc"`,
		`  "abc" trailing`: `"abc"`,
		`abc`:              ``,
		`"abc`:             ``,
		``:                 ``,
		`W/`:               ``,
		"\"a\x01b\"":       ``,
	} {
		if got, _ := scanETag(in); got != want {
			t.Errorf("scanETag(%q) = %q, want %q", in, got, want)
		}
	}
	tag := `"abc"`
	for _, tc := range []struct {
		list           string
		strong, expect bool
	}{
		{`"abc"`, true, true},
		{`"abc"`, false, true},
		{`W/"abc"`, true, false},   // a weak tag never matches strongly
		{`W/"abc"`, false, true},   // …but does weakly
		{`*`, true, true},          // any representation
		{`"x", "abc"`, true, true}, // walks the list
		{`"x","y"`, true, false},
		{`garbage`, true, false},
		{``, true, false},
		{`,,`, true, false},
	} {
		if got := etagListMatches(tc.list, tag, tc.strong); got != tc.expect {
			t.Errorf("etagListMatches(%q, strong=%v) = %v", tc.list, tc.strong, got)
		}
	}
}

func TestRangeCount(t *testing.T) {
	for in, want := range map[string]int{
		"": 0, "garbage": 0, "bytes=0-1": 1, "bytes=0-1,5-6": 2, "bytes=0-1, 5-6 ,": 2, "bytes=,,,": 0, "bytes=-500": 1,
	} {
		if got := rangeCount(in); got != want {
			t.Errorf("rangeCount(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestAttachmentName(t *testing.T) {
	tests := []struct{ filename, key, want string }{
		{"clip.mp4", "p/x/original/source.mp4", "clip.mp4"},
		{`C:\dir\my "clip".mp4`, "k/source.mp4", "my clip.mp4"},
		{"", "p/x/original/source.bin", "source.bin"},
		{"..", "p/x/original/source.mov", "source.mov"},
		{"\x01\x02", "k/source.mp4", "source.mp4"},
		{strings.Repeat("A", 300_000) + ".mp4", "k/source.mp4", strings.Repeat("A", 251) + ".mp4"}, // §20 erratum 26
	}
	for _, tc := range tests {
		if got := attachmentName(tc.filename, tc.key); got != tc.want {
			t.Errorf("attachmentName(%.30q…) = %.30q…, want %.30q…", tc.filename, got, tc.want)
		}
	}
}

// §8.1 promises `Content-Disposition: attachment; filename="<sanitized>"`.
// mime.FormatMediaType, handed a non-ASCII name alone, emits the RFC 5987
// extended form ONLY — no plain `filename=` at all — so a client that does
// not implement it (curl -OJ, wget --content-disposition, download managers)
// gets no name and saves the URL's last segment: a file called `original`,
// extension and all. RFC 6266 §4.3 says send both (§20 erratum 50).
func TestContentDispositionCarriesAnASCIIFallback(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		// Pure ASCII is unchanged: one plain parameter, quoted when needed.
		{"clip.mp4", "attachment; filename=clip.mp4"},
		{"me <b>.mp4", `attachment; filename="me <b>.mp4"`},
		{"a b c.mp4", `attachment; filename="a b c.mp4"`},
		// Non-ASCII: both, fallback first, extension preserved, and the
		// extended value percent-encoded down to RFC 5987's attr-char set
		// (%, ' and * are NOT attr-chars).
		{"ünïcode.mp4", `attachment; filename=_n_code.mp4; filename*=utf-8''%C3%BCn%C3%AFcode.mp4`},
		{"vidéo été — clip.mp4", `attachment; filename="vid_o _t_ _ clip.mp4"; filename*=utf-8''vid%C3%A9o%20%C3%A9t%C3%A9%20%E2%80%94%20clip.mp4`},
		{"100%’s*.mp4", `attachment; filename=100%_s*.mp4; filename*=utf-8''100%25%E2%80%99s%2A.mp4`},
	} {
		if got := contentDisposition(tc.name); got != tc.want {
			t.Errorf("contentDisposition(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
	// Every non-ASCII name still carries a PLAIN parameter — the one an
	// RFC-5987-unaware client reads — and it keeps the source's extension.
	// (mime.ParseMediaType cannot see this: it prefers filename* and decodes
	// it into params["filename"], which is exactly the reader this is for.)
	for _, name := range []string{"ünïcode.mp4", "vidéo été — clip.mp4", "日本語.mp4", "ünïcödé"} {
		cd := contentDisposition(name)
		plain, ok := "", false
		for _, param := range strings.Split(cd, "; ") {
			if v, cut := strings.CutPrefix(param, "filename="); cut {
				plain, ok = strings.Trim(v, `"`), true
			}
		}
		switch {
		case !ok || plain == "":
			t.Errorf("%q: no plain filename= in %q", name, cd)
		case strings.ContainsFunc(plain, func(r rune) bool { return r < 0x20 || r > 0x7e }):
			t.Errorf("%q: filename fallback is not ASCII: %q", name, plain)
		case path.Ext(plain) != path.Ext(name):
			t.Errorf("%q: fallback lost the extension: %q", name, plain)
		}
		if !strings.Contains(cd, "filename*=utf-8''") {
			t.Errorf("%q: the exact name is gone: %q", name, cd)
		}
	}
}

// A suffix range of zero length selects no bytes: RFC 9110 §14.1.2 makes it
// unsatisfiable, but http.ServeContent answers 206 with an inverted
// "Content-Range: bytes {size}-{size-1}/{size}" under a cacheable
// Cache-Control. §8.5 says unsatisfiable → 416, zero GETs (§20 erratum 38).
func TestMediaZeroSuffixRange(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{})
	base := "/api/clips/" + c.ID
	size := strconv.Itoa(len(rendition))

	before := e.gets()
	for _, rng := range []string{"bytes=-0", "bytes=-00", "bytes=- 0", "bytes=-0,-0", "bytes=0-9,-0", "bytes=-0,100-199"} {
		for _, method := range []string{"GET", "HEAD"} {
			r := e.do(method, base+"/video.mp4", nil, withHeader("Range", rng))
			if r.code != 416 || r.hdr.Get("Content-Range") != "bytes */"+size || r.hdr.Get("Cache-Control") != "no-store" {
				t.Fatalf("%s Range %q: %d %v %s", method, rng, r.code, r.hdr, r.body)
			}
			if method == "GET" && (r.errCode() != "range_not_satisfiable" || !strings.Contains(r.str("message"), "zero length")) {
				t.Fatalf("envelope for %q: %s", rng, r.body)
			}
			if e.gets() != before {
				t.Fatalf("Range %q touched storage: %v", rng, e.fake.CallsSnapshot())
			}
		}
	}
	// Neighbouring shapes still behave: a real suffix range serves, and a
	// zero-length spec is only the SUFFIX one (bytes=0-0 is one byte).
	if r := e.do("GET", base+"/video.mp4", nil, withHeader("Range", "bytes=-100")); r.code != 206 || len(r.body) != 100 {
		t.Fatalf("bytes=-100: %d len=%d", r.code, len(r.body))
	}
	if r := e.do("GET", base+"/video.mp4", nil, withHeader("Range", "bytes=0-0")); r.code != 206 || len(r.body) != 1 {
		t.Fatalf("bytes=0-0: %d len=%d", r.code, len(r.body))
	}
	// The owner's original applies the same gate.
	if r := e.do("GET", base+"/original", nil, withCookie(cookie), withHeader("Range", "bytes=-0")); r.code != 416 {
		t.Fatalf("original bytes=-0: %d", r.code)
	}
}

// TestRangeErrorDropsTheObjectHeaders: a 416 is an ERROR body, not the
// object, so it must not keep the headers that describe the object (§20
// erratum 85). Content-Disposition is the one with teeth -- /original is
// served `attachment; filename="..."`, and a download client that hit a 416
// would otherwise write the JSON envelope to disk under the clip's own
// filename. ETag and Last-Modified go with it: they are the object's
// validators, and no cache may revalidate the object against an error.
//
// Both entry points are covered: the gates in serveObject (too many ranges,
// a zero suffix -- checked BEFORE ServeContent) and the stdlib 416 that
// objectErrorWriter intercepts (a range past the end).
func TestRangeErrorDropsTheObjectHeaders(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	src := testVideo(2222)
	c := e.processingClip(owner, clipOpts{source: src})
	path := "/api/clips/" + c.ID + "/original"

	// The happy path really does set all three, or this test proves nothing.
	full := e.do("GET", path, nil, withCookie(cookie))
	if full.code != 200 || full.hdr.Get("Content-Disposition") == "" || full.hdr.Get("ETag") == "" {
		t.Fatalf("the 200 must carry the headers the 416 has to drop: %d %v", full.code, full.hdr)
	}

	for _, tc := range []struct{ name, rng string }{
		{"gate: zero suffix", "bytes=-0"},
		{"gate: too many ranges", "bytes=0-1,2-3,4-5,6-7,8-9,10-11,12-13,14-15,16-17"},
		{"stdlib: past the end", "bytes=99999-"},
		{"stdlib: malformed", "bytes=abc-"},
	} {
		r := e.do("GET", path, nil, withCookie(cookie), withHeader("Range", tc.rng))
		if r.code != 416 || r.errCode() != "range_not_satisfiable" {
			t.Fatalf("%s: %d %s", tc.name, r.code, r.body)
		}
		for _, h := range []string{"Content-Disposition", "ETag", "Last-Modified"} {
			if v := r.hdr.Get(h); v != "" {
				t.Errorf("%s: the 416 kept %s: %q", tc.name, h, v)
			}
		}
		// …and still says what a 416 has to say.
		if r.hdr.Get("Content-Range") != "bytes */2222" || r.hdr.Get("Cache-Control") != "no-store" ||
			r.hdr.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Errorf("%s: 416 headers = %v", tc.name, r.hdr)
		}
	}
}

// TestPreconditionFailedDropsTheObjectHeaders is erratum 85 applied to the
// OTHER status net/http writes out of serveObject (§20 erratum 103). A
// failing `If-Match` or `If-Unmodified-Since` used to answer 412 carrying
// everything serveObject had already set -- the object ETag, its Content-Type,
// `Cache-Control: public, max-age=3600` and, on /original, its
// `Content-Disposition: attachment` -- because the writer intercepted 416
// only. The max-age is what makes it more than untidy: an empty 412 under an
// explicit max-age is storable, and a cache that stored it could then
// revalidate the OBJECT against an error body.
//
// Both routes are checked, because only /original sets a disposition, and
// both preconditions are, because net/http reaches the same 412 from two
// different clauses.
func TestPreconditionFailedDropsTheObjectHeaders(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{source: testVideo(2222)})
	past := time.Unix(1, 0).UTC().Format(http.TimeFormat)

	for _, route := range []string{"/api/clips/" + c.ID + "/video.mp4", "/api/clips/" + c.ID + "/original"} {
		full := e.do("GET", route, nil, withCookie(cookie))
		if full.code != 200 || full.hdr.Get("ETag") == "" {
			t.Fatalf("%s: the 200 must carry what the 412 has to drop: %d %v", route, full.code, full.hdr)
		}
		for _, tc := range []struct{ name, key, value string }{
			{"If-Match", "If-Match", `"nope"`},
			{"If-Unmodified-Since", "If-Unmodified-Since", past},
		} {
			r := e.do("GET", route, nil, withCookie(cookie), withHeader(tc.key, tc.value))
			if r.code != 412 || r.errCode() != "precondition_failed" {
				t.Fatalf("%s %s: %d %s", route, tc.name, r.code, r.body)
			}
			for _, h := range []string{"Content-Disposition", "ETag", "Last-Modified"} {
				if v := r.hdr.Get(h); v != "" {
					t.Errorf("%s %s: the 412 kept %s: %q", route, tc.name, h, v)
				}
			}
			if cc := r.hdr.Get("Cache-Control"); cc != "no-store" {
				t.Errorf("%s %s: 412 Cache-Control = %q, want no-store", route, tc.name, cc)
			}
			if ct := r.hdr.Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("%s %s: 412 Content-Type = %q", route, tc.name, ct)
			}
			// A 412 is a precondition failure, not a range one.
			if cr := r.hdr.Get("Content-Range"); cr != "" {
				t.Errorf("%s %s: 412 carries Content-Range %q", route, tc.name, cr)
			}
		}
	}
}

// ---- ?download=1 on video.mp4 (§20 erratum 98) --------------------------

// The parameter is an opt-in for PRESENTATION, not for different bytes: the
// same object, through the same serveObject, with the Content-Disposition
// that makes a browser save the clip instead of navigating to it. Everything
// a cache or a player keys on has to be identical to the plain GET's, because
// the two answers ARE the same representation — which is also why the route
// gains no Vary: the query string is already part of the cache key.
func TestVideoDownloadParameter(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{title: "My Clip! (final)"})
	base := "/api/clips/" + c.ID + "/video.mp4"

	plain := e.do("GET", base, nil)
	if plain.code != 200 || string(plain.body) != string(rendition) {
		t.Fatalf("plain video.mp4: %d len=%d", plain.code, len(plain.body))
	}
	if cd := plain.hdr.Get("Content-Disposition"); cd != "" {
		t.Fatalf("a plain video.mp4 must not be an attachment: %q", cd)
	}
	dl := e.do("GET", base+"?download=1", nil)
	if dl.code != 200 || string(dl.body) != string(rendition) {
		t.Fatalf("?download=1: %d len=%d", dl.code, len(dl.body))
	}
	if cd := dl.hdr.Get("Content-Disposition"); cd != "attachment; filename=my-clip-final.mp4" {
		t.Fatalf("?download=1 Content-Disposition = %q", cd)
	}
	// One header added, none changed.
	for _, h := range []string{"ETag", "Last-Modified", "Cache-Control", "Content-Type",
		"Content-Length", "Accept-Ranges", "X-Content-Type-Options", "Vary"} {
		if got, want := dl.hdr.Get(h), plain.hdr.Get(h); got != want {
			t.Errorf("?download=1 %s = %q, want %q (the plain GET's)", h, got, want)
		}
	}
	if v := dl.hdr.Get("Vary"); v != "" {
		t.Errorf("?download=1 must add no Vary (the query string is its own cache key): %q", v)
	}
	// Any other value is IGNORED, never an error: this parameter is reached
	// from a link on the share page, and a link that can 400 is a broken link.
	for _, q := range []string{"?download=0", "?download", "?download=yes", "?download=true",
		"?download=11", "?download=%201", "?download=1x", "?nonsense=1"} {
		r := e.do("GET", base+q, nil)
		if r.code != 200 || r.hdr.Get("Content-Disposition") != "" {
			t.Errorf("%s: %d Content-Disposition=%q, want 200 with none", q, r.code, r.hdr.Get("Content-Disposition"))
		}
	}
	// It changes nothing about the OTHER routes, which have no such parameter.
	for _, route := range []string{"/social.mp4", "/poster.jpg"} {
		r := e.do("GET", "/api/clips/"+c.ID+route+"?download=1", nil)
		if r.code != 200 || r.hdr.Get("Content-Disposition") != "" {
			t.Errorf("%s?download=1: %d Content-Disposition=%q", route, r.code, r.hdr.Get("Content-Disposition"))
		}
	}
}

// Range, revalidation and the 416 are §8.5's, not the parameter's: a save is
// resumable exactly as a stream is, and a conditional GET still costs one
// round trip and zero storage calls.
func TestVideoDownloadKeepsRangeAndConditionals(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{title: "ranged"})
	base := "/api/clips/" + c.ID + "/video.mp4"
	etag := `"` + sha256hex(rendition) + `"`

	plainRange := e.do("GET", base, nil, withHeader("Range", "bytes=10-19"))
	dlRange := e.do("GET", base+"?download=1", nil, withHeader("Range", "bytes=10-19"))
	if plainRange.code != 206 || dlRange.code != 206 {
		t.Fatalf("ranged: plain %d, download %d", plainRange.code, dlRange.code)
	}
	if string(dlRange.body) != string(rendition[10:20]) {
		t.Errorf("ranged ?download=1 body = %q", dlRange.body)
	}
	if got, want := dlRange.hdr.Get("Content-Range"), plainRange.hdr.Get("Content-Range"); got != want || want == "" {
		t.Errorf("ranged Content-Range = %q, want %q", got, want)
	}
	if got, want := dlRange.hdr.Get("ETag"), etag; got != want {
		t.Errorf("ranged ETag = %q, want %q", got, want)
	}
	if cd := dlRange.hdr.Get("Content-Disposition"); cd != "attachment; filename=ranged.mp4" {
		t.Errorf("ranged ?download=1 Content-Disposition = %q", cd)
	}
	// 304 with zero storage calls, and the SAME validator as without it.
	before := e.gets()
	nm := e.do("GET", base+"?download=1", nil, withHeader("If-None-Match", etag))
	if nm.code != 304 || e.gets() != before {
		t.Fatalf("?download=1 If-None-Match: %d gets=%d", nm.code, e.gets()-before)
	}
	// 412 is unchanged too.
	if r := e.do("GET", base+"?download=1", nil, withHeader("If-Match", `"nope"`)); r.code != 412 {
		t.Errorf("?download=1 If-Match: %d, want 412", r.code)
	}
	// And a 416 still drops the disposition with the other object headers
	// (§20 erratum 85): a download client must never save the JSON error
	// envelope to disk under the clip's own name.
	bad := e.do("GET", base+"?download=1", nil, withHeader("Range", "bytes=999999-"))
	if bad.code != 416 || bad.hdr.Get("Content-Disposition") != "" || bad.hdr.Get("ETag") != "" {
		t.Errorf("416 under ?download=1: %d %v", bad.code, bad.hdr)
	}
}

// A title is user text and lands in a response header, so the exact string
// matters: quotes, semicolons, path separators and non-ASCII all have to come
// out as a bare RFC 2045 token that needs no quoting and no RFC 5987 form.
func TestVideoDownloadFilenameIsSafe(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	for _, tc := range []struct{ title, want string }{
		{`"; rm -rf /; x="`, `attachment; filename=rm-rf-x.mp4`},
		{`../../etc/passwd`, `attachment; filename=etc-passwd.mp4`},
		{`C:\Clips\me\My Clip.mp4`, `attachment; filename=c-clips-me-my-clip-mp4.mp4`},
		{"line\r\nbreak: X-Evil\x00", `attachment; filename=line-break-x-evil.mp4`},
		{`Ünïcodé — 日本語`, `attachment; filename=n-cod.mp4`},
		{strings.Repeat("a", 300), `attachment; filename=` + strings.Repeat("a", maxSlugRunes) + `.mp4`},
	} {
		c, _, _ := e.readyClip(owner, clipOpts{title: tc.title})
		r := e.do("GET", "/api/clips/"+c.ID+"/video.mp4?download=1", nil)
		if r.code != 200 || r.hdr.Get("Content-Disposition") != tc.want {
			t.Errorf("title %.30q: %d Content-Disposition = %q, want %q", tc.title, r.code, r.hdr.Get("Content-Disposition"), tc.want)
		}
	}
	// Nothing sluggable at all: the id, which every clip has and which is
	// already the safest string in the system.
	c, _, _ := e.readyClip(owner, clipOpts{title: "🎬 🎬 🎬"})
	r := e.do("GET", "/api/clips/"+c.ID+"/video.mp4?download=1", nil)
	if want := `attachment; filename=` + c.ID + `.mp4`; r.hdr.Get("Content-Disposition") != want {
		t.Errorf("emoji title: Content-Disposition = %q, want %q", r.hdr.Get("Content-Disposition"), want)
	}
}

func TestTitleSlug(t *testing.T) {
	for _, tc := range []struct{ title, want string }{
		{"clip", "clip"},
		{"My Clip! (final)", "my-clip-final"},
		{"UPPER CASE", "upper-case"},
		{"already-hyphenated", "already-hyphenated"},
		{"a — b", "a-b"}, // a '-' is part of the separator run, never doubled
		{"--lead--trail--", "lead-trail"},
		{"   ", "FB"}, // nothing survives
		{"🎬", "FB"},
		{"", "FB"},
		{"2026", "2026"},
		{"v1.2.3", "v1-2-3"},
		{`"quoted"; semi`, "quoted-semi"},
		{strings.Repeat("ab ", 40), strings.Repeat("ab-", 19) + "ab"}, // capped, and never ON a dash
		{strings.Repeat("a", maxSlugRunes+5), strings.Repeat("a", maxSlugRunes)},
	} {
		got := titleSlug(tc.title, "FB")
		if got != tc.want {
			t.Errorf("titleSlug(%.40q) = %q, want %q", tc.title, got, tc.want)
		}
		if len(got) > maxSlugRunes {
			t.Errorf("titleSlug(%.40q) is %d bytes, over the %d cap", tc.title, len(got), maxSlugRunes)
		}
		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("titleSlug(%.40q) = %q: a slug never begins or ends with a dash", tc.title, got)
		}
	}
}
