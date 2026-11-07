package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/cost"
	"clippet/internal/httpx"
	"clippet/internal/store"
)

// TestCSPFollowsTheBucketLookup is the regression for §20 erratum 278's
// amendment, and the reason `MediaOrigin()` is resolved from a real presign
// rather than read off `S3_ENDPOINT`.
//
// With virtual-host bucket lookup — `S3_FORCE_PATH_STYLE=false`, the binary's
// own default and what DEPLOY §7's R2 and B2 stanzas and §33.3's recipe set —
// minio-go prefixes the BUCKET to the host. A CSP built from the endpoint then
// names `acct.example` while the redirect lands on `clips.acct.example`, and
// CSP host matching is exact, so the watch page and the embed player would be
// blocked by our own policy on every deployment §33 exists for. It worked only
// on the bundled MinIO, which compose pins to path style — which is exactly why
// the E2E could not see it.
//
// The whole chain is exercised: a real minio client (still no network — a SigV4
// query signature is local HMAC and the region is pinned), through
// store.MediaOrigin, into Config, out of CSPPage/CSPEmbed.
func TestCSPFollowsTheBucketLookup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pathStyle  bool
		wantOrigin string
	}{
		{"virtual-host (S3_FORCE_PATH_STYLE=false: R2, B2, S3)", false, "https://clips.acct.example.test"},
		{"path style (S3_FORCE_PATH_STYLE=true: the bundled MinIO)", true, "https://acct.example.test"},
	} {
		s, err := store.New(config.S3Config{
			Endpoint: "https://acct.example.test", Region: "auto", Bucket: "clips",
			AccessKey: "AKIAEXAMPLE", SecretKey: "s3cr3t-example-key",
			ForcePathStyle: tc.pathStyle, OpTimeout: time.Second,
		})
		if err != nil {
			t.Fatalf("%s: New: %v", tc.name, err)
		}
		origin, err := store.MediaOrigin(context.Background(), s, time.Hour)
		if err != nil {
			t.Fatalf("%s: MediaOrigin: %v", tc.name, err)
		}
		if origin != tc.wantOrigin {
			t.Errorf("%s: origin = %q, want %q", tc.name, origin, tc.wantOrigin)
		}
		// …and that is what reaches the policy, on the page and on the embed.
		cfg := config.Config{MediaDelivery: config.DeliveryRedirect, ResolvedMediaOrigin: origin}
		for _, csp := range []string{httpx.CSPPage(cfg.MediaOrigin()), httpx.CSPEmbed(cfg.MediaOrigin())} {
			if !strings.Contains(csp, "media-src 'self' "+tc.wantOrigin+";") ||
				!strings.Contains(csp, "img-src 'self' data: "+tc.wantOrigin+";") {
				t.Errorf("%s: the policy does not allow the origin the redirect lands on: %s", tc.name, csp)
			}
		}
		// A signed Location for a REAL key lands on that same origin — the
		// equality `90-media.sh` now asserts against the CSP header too.
		raw, err := s.PresignGet(context.Background(), "clips/abcdefghijkl/renditions/main-1a2b3c4d.mp4", time.Hour, "video/mp4", "inline")
		if err != nil {
			t.Fatalf("%s: PresignGet: %v", tc.name, err)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := u.Scheme + "://" + u.Host; got != tc.wantOrigin {
			t.Errorf("%s: a real Location lands on %q, not on %q", tc.name, got, tc.wantOrigin)
		}
	}
}

// The media redirect (ROADMAP #34 lane F, §20 erratum 278). Every test here is
// about ONE claim: the routes, the gates and the URLs are unchanged, and the
// only thing that moves is what a route answers once every gate has passed.

// redirectMode is the mutator that puts a handler in MEDIA_DELIVERY=redirect
// with a presign origin of its own — the shape a real deployment has, where
// the app reaches the bucket privately and a viewer reaches it publicly.
func redirectMode(d *Deps) {
	d.Cfg.MediaDelivery = config.DeliveryRedirect
	d.Cfg.MediaPresignTTL = 42 * time.Minute
	d.Cfg.S3.PresignEndpoint = "https://clips.example.test"
	// What `serve` fills in from store.MediaOrigin once the store is open: the
	// origin the Fake's own presigns land on (§20 erratum 278, amended).
	d.Cfg.ResolvedMediaOrigin = "https://" + store.FakePresignHost
}

// TestMediaRedirectsAfterEveryGate is the erratum's law in one test: the card
// and media routes answer 302 to a freshly presigned URL, on GET and HEAD
// alike, with no representation headers and no storage read — and every gate
// that used to run still runs BEFORE the link is minted.
func TestMediaRedirectsAfterEveryGate(t *testing.T) {
	e := newEnv(t, redirectMode)
	owner, ownerCookie := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{})
	base := "/api/clips/" + c.ID

	for _, route := range []string{"/video.mp4", "/social.mp4", "/poster.jpg"} {
		for _, method := range []string{"GET", "HEAD"} {
			r := e.do(method, base+route, nil)
			if r.code != http.StatusFound {
				t.Fatalf("%s %s: %d, want 302", method, route, r.code)
			}
			if len(r.body) != 0 {
				t.Errorf("%s %s: %d bytes of body on a 302", method, route, len(r.body))
			}
			loc, err := url.Parse(r.hdr.Get("Location"))
			if err != nil || loc.Host != store.FakePresignHost {
				t.Fatalf("%s %s: Location = %q (%v)", method, route, r.hdr.Get("Location"), err)
			}
			q := loc.Query()
			if q.Get("X-Amz-Signature") == "" || q.Get("X-Amz-Expires") != strconv.Itoa(int((42*time.Minute).Seconds())) {
				t.Errorf("%s %s: the Location is not a fresh presign at the configured TTL: %s", method, route, loc)
			}
			if q.Get("response-content-disposition") != "inline" {
				t.Errorf("%s %s: disposition = %q, want inline", method, route, q.Get("response-content-disposition"))
			}
			if r.hdr.Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s: Cache-Control = %q (a signed Location must never be stored)", method, route, r.hdr.Get("Cache-Control"))
			}
			// No representation headers: they describe bytes this response does
			// not carry (erratum 85's rule, applied to a third status).
			for _, hname := range []string{"ETag", "Last-Modified", "Accept-Ranges", "Content-Disposition", "Content-Type"} {
				if v := r.hdr.Get(hname); v != "" {
					t.Errorf("%s %s: the 302 carries %s: %q", method, route, hname, v)
				}
			}
		}
	}

	// The right OBJECT is signed for each route, and the response type comes
	// from the ROW rather than from whatever the object was stored with.
	for _, tc := range []struct{ route, frag, ctype string }{
		{"/video.mp4", "renditions/main-", "video/mp4"},
		{"/social.mp4", "renditions/main-", "video/mp4"}, // no social copy: it falls back
		{"/poster.jpg", "poster/poster-", "image/jpeg"},
	} {
		loc := e.do("GET", base+tc.route, nil).hdr.Get("Location")
		if !strings.Contains(loc, tc.frag) {
			t.Errorf("%s signed %q, want the key under %s", tc.route, loc, tc.frag)
		}
		if got := mustQuery(t, loc).Get("response-content-type"); got != tc.ctype {
			t.Errorf("%s response-content-type = %q, want %q", tc.route, got, tc.ctype)
		}
	}

	// Not one byte was read out of the store. That is the whole point.
	if e.gets() != 0 {
		t.Fatalf("redirect mode read the store %d times: %v", e.gets(), e.fake.CallsSnapshot())
	}

	// Range and conditional headers are IGNORED by a 302 — the target answers
	// them — so no 206 or 304 may be invented here.
	for _, hh := range [][2]string{
		{"Range", "bytes=0-99"},
		{"If-None-Match", `"` + sha256hex(rendition) + `"`},
		{"If-Range", `"` + sha256hex(rendition) + `"`},
	} {
		if r := e.do("GET", base+"/video.mp4", nil, withHeader(hh[0], hh[1])); r.code != http.StatusFound {
			t.Errorf("%s: %d, want 302", hh[0], r.code)
		}
	}

	// `?download=1` carries erratum 98's presentation rule in the query, which
	// is the only place it can travel in this mode.
	q := mustQuery(t, e.do("GET", base+"/video.mp4?download=1", nil).hdr.Get("Location"))
	if d := q.Get("response-content-disposition"); !strings.HasPrefix(d, "attachment;") || !strings.Contains(d, ".mp4") {
		t.Errorf("?download=1 disposition = %q", d)
	}

	// EVERY gate still runs, and runs before a link exists.
	unknown := "/api/clips/" + strings.Repeat("a", 12) + "/video.mp4"
	if r := e.do("GET", unknown, nil); r.code != 404 || r.hdr.Get("Location") != "" {
		t.Errorf("unknown clip: %d Location=%q", r.code, r.hdr.Get("Location"))
	}
	proc := e.processingClip(owner, clipOpts{})
	if r := e.do("GET", "/api/clips/"+proc.ID+"/video.mp4", nil); r.code != 409 || r.hdr.Get("Location") != "" {
		t.Errorf("not-ready clip: %d Location=%q", r.code, r.hdr.Get("Location"))
	}
	del := e.deletingClip(owner)
	if r := e.do("GET", "/api/clips/"+del.ID+"/video.mp4", nil); r.code != 404 || r.hdr.Get("Location") != "" {
		t.Errorf("deleting clip: %d Location=%q", r.code, r.hdr.Get("Location"))
	}

	// `/original` is the one object with a real identity gate, so it is NEVER
	// redirected: a presigned URL cannot carry an owner check.
	orig := "/api/clips/" + c.ID + "/original"
	if r := e.do("GET", orig, nil); r.code != 401 {
		t.Errorf("anonymous original: %d, want 401", r.code)
	}
	_, otherCookie := e.newUser("other")
	if r := e.do("GET", orig, nil, withCookie(otherCookie)); r.code != 403 {
		t.Errorf("stranger's original: %d, want 403", r.code)
	}
	r := e.do("GET", orig, nil, withCookie(ownerCookie))
	if r.code != 200 || r.hdr.Get("Location") != "" {
		t.Fatalf("owner's original: %d Location=%q, want 200 and no redirect", r.code, r.hdr.Get("Location"))
	}
	if r.hdr.Get("ETag") == "" || !strings.HasPrefix(r.hdr.Get("Content-Disposition"), "attachment") {
		t.Errorf("the original lost its proxied headers: %v", r.hdr)
	}
	if e.gets() == 0 {
		t.Error("the original did not read the store")
	}
}

// TestGIFAndFilmstripStayProxied (§20 erratum 278): the two media routes whose
// STATUS this app's own JavaScript polls with `fetch` are NEVER redirected. A
// `fetch` follows a cross-origin 302 into a CORS check, so the response would be
// unreadable — the GIF button would poll out and the trim scrubber would never
// get its sheet — unless `connect-src` were widened and the bucket served CORS
// headers, which is exactly what a BYO bucket is promised it never needs.
func TestGIFAndFilmstripStayProxied(t *testing.T) {
	e := newEnv(t, redirectMode)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	c = e.withGIF(c, gifKey(e, c.ID))
	e.withFilmstrip(c, filmstripKeyFor(e, c.ID))
	base := "/api/clips/" + c.ID

	for _, tc := range []struct {
		route string
		body  []byte
		ctype string
	}{{"/preview.gif", []byte(gifBytes), "image/gif"}, {"/filmstrip.jpg", []byte(filmstripBytes), "image/jpeg"}} {
		r := e.do("GET", base+tc.route, nil, withCookie(cookie))
		if r.code != 200 || r.hdr.Get("Location") != "" {
			t.Fatalf("%s: %d Location=%q, want 200 and no redirect", tc.route, r.code, r.hdr.Get("Location"))
		}
		if string(r.body) != string(tc.body) || r.hdr.Get("Content-Type") != tc.ctype || r.hdr.Get("ETag") == "" {
			t.Errorf("%s: the proxied answer lost its bytes or its headers: %d %v", tc.route, len(r.body), r.hdr)
		}
		// The HEAD the poll actually makes is the same answer, still not a 302.
		if r := e.do("HEAD", base+tc.route, nil, withCookie(cookie)); r.code != 200 || r.hdr.Get("Location") != "" {
			t.Errorf("HEAD %s: %d Location=%q", tc.route, r.code, r.hdr.Get("Location"))
		}
	}
	if presigns(e) != 0 {
		t.Errorf("the GIF or the filmstrip presigned something: %v", e.fake.CallsSnapshot())
	}
	// …while the three that carry the card and the player still redirect.
	for _, route := range []string{"/video.mp4", "/social.mp4", "/poster.jpg"} {
		if r := e.do("GET", base+route, nil); r.code != http.StatusFound {
			t.Errorf("%s: %d, want 302", route, r.code)
		}
	}
}

// TestMediaRedirectUnlistedAndDeleted: the two clip states an operator worries
// about when media leaves the app. Unlisted is unchanged — the key is as
// unguessable as the link, and the link IS the capability (§9.1) — and a
// deleted clip stops redirecting the instant the row is gone, because the link
// is minted AFTER the lookup and never before.
func TestMediaRedirectUnlistedAndDeleted(t *testing.T) {
	e := newEnv(t, redirectMode)
	owner, ownerCookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{unlisted: true})
	base := "/api/clips/" + c.ID
	if r := e.do("GET", base+"/video.mp4", nil); r.code != http.StatusFound {
		t.Fatalf("an unlisted clip's media: %d, want 302", r.code)
	}
	if r := e.do("DELETE", base, nil, withCookie(ownerCookie)); r.code != 204 && r.code != 202 {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	before := presigns(e)
	r := e.do("GET", base+"/video.mp4", nil)
	if r.code != 404 || r.hdr.Get("Location") != "" {
		t.Fatalf("a deleted clip: %d Location=%q, want 404 and no presign", r.code, r.hdr.Get("Location"))
	}
	if presigns(e) != before {
		t.Error("a deleted clip's media route still minted a link")
	}
}

// presigns counts the links the Fake has been asked for.
func presigns(e *env) int {
	n := 0
	for _, call := range e.fake.CallsSnapshot() {
		if strings.HasPrefix(call, "PresignGet ") {
			n++
		}
	}
	return n
}

// TestMediaRedirectChargesTheCounter (§20 errata 270, 278): bytes that leave
// the bucket never pass this app, so a 302 attributes the whole object as an
// UPPER bound rather than leaving the counter reading zero — which erratum 270
// was written to prevent ("unset is ABSENT, never zero").
func TestMediaRedirectChargesTheCounter(t *testing.T) {
	var counter *cost.Counter
	e := newEnv(t, redirectMode, func(d *Deps) {
		counter = cost.New(d.DB, d.Log)
		d.Served = counter
	})
	owner, _ := e.newUser("owner")
	c, rendition, poster := e.readyClip(owner, clipOpts{})
	base := "/api/clips/" + c.ID

	if r := e.do("GET", base+"/video.mp4", nil); r.code != http.StatusFound {
		t.Fatalf("video: %d", r.code)
	}
	if r := e.do("GET", base+"/poster.jpg", nil); r.code != http.StatusFound {
		t.Fatalf("poster: %d", r.code)
	}
	want := int64(len(rendition) + len(poster))
	if got := counter.Pending(""); got != want {
		t.Fatalf("the counter is at %d, want %d (one rendition and one poster, whole)", got, want)
	}
	// A HEAD is charged NOTHING — the same answer `proxy` mode gives, where a
	// HEAD writes no body at all. "Whatever follows it fetches the object
	// anyway" is true of a browser and false of every request that is only a
	// HEAD: an uptime monitor, `curl -I`, a link checker. Charging those would
	// put bytes nobody received into a cost line.
	for i := 0; i < 3; i++ {
		if r := e.do("HEAD", base+"/video.mp4", nil); r.code != http.StatusFound {
			t.Fatalf("head: %d", r.code)
		}
	}
	if got := counter.Pending(""); got != want {
		t.Fatalf("three HEADs charged %d bytes; a HEAD must cost nothing", got-want)
	}
	// A refusal charges nothing: no object leaves.
	if r := e.do("GET", "/api/clips/"+strings.Repeat("a", 12)+"/video.mp4", nil); r.code != 404 {
		t.Fatalf("unknown: %d", r.code)
	}
	if got := counter.Pending(""); got != want {
		t.Errorf("a 404 charged the counter: %d", got)
	}
}

// TestMediaRedirectFailureIs503 (§20 erratum 278): a presign that fails is a
// storage failure like any other — 503 in the bounded envelope, never a 302 to
// nothing, and never the endpoint in a body an anonymous caller reads (§15).
func TestMediaRedirectFailureIs503(t *testing.T) {
	e := newEnv(t, redirectMode)
	owner, _ := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	e.fake.FailNext("PresignGet", errors.New("signing failed for host clips.example.test"))
	r := e.do("GET", "/api/clips/"+c.ID+"/video.mp4", nil)
	if r.code != 503 || r.hdr.Get("Location") != "" {
		t.Fatalf("presign failure: %d Location=%q", r.code, r.hdr.Get("Location"))
	}
	if r.str("error") != "storage_unavailable" {
		t.Errorf("error code = %q", r.str("error"))
	}
	if strings.Contains(string(r.body), "clips.example.test") {
		t.Errorf("the 503 body names the endpoint: %s", r.body)
	}
}

// TestProxyModeIsUntouched: with the shipped default nothing about the media
// routes moves — no presign is ever minted and the bytes are still the app's.
func TestProxyModeIsUntouched(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, rendition, _ := e.readyClip(owner, clipOpts{})
	r := e.do("GET", "/api/clips/"+c.ID+"/video.mp4", nil)
	if r.code != 200 || string(r.body) != string(rendition) || r.hdr.Get("ETag") == "" {
		t.Fatalf("proxy mode: %d len=%d", r.code, len(r.body))
	}
	if presigns(e) != 0 {
		t.Fatalf("proxy mode presigned something: %v", e.fake.CallsSnapshot())
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query()
}
