package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"clippet/internal/db"
)

// shareReq posts a share-sheet-shaped body: the text fields Android sends,
// then the file part named `file` (what manifest.webmanifest declares).
func (e *env) shareReq(filename string, video []byte, fields []part, opts ...reqOpt) resp {
	e.t.Helper()
	parts := append([]part{}, fields...)
	parts = append(parts, part{name: "file", filename: filename, contentType: "video/mp4", data: video})
	body, ct := multipartBody(e.t, parts...)
	return e.do(http.MethodPost, "/api/share", body,
		append([]reqOpt{withHeader("Content-Type", ct)}, opts...)...)
}

// TestShareTargetCreatesAClipAndRedirects is ROADMAP #27's happy path: the
// browser is navigated, so the answer is a 303 to the clip's share page rather
// than a JSON body, and the clip is an ordinary clip.
func TestShareTargetCreatesAClipAndRedirects(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	// A share sheet sends `title`, `text` and `url` beside the file. The
	// title is taken; the other two are the sharing app's commentary and are
	// ignored like any other unknown field (§20 erratum 137).
	r := e.shareReq("clip.mp4", testVideo(900), []part{
		{name: "title", data: []byte("From my phone")},
		{name: "text", data: []byte("look at this")},
		{name: "url", data: []byte("https://example.invalid/whatever")},
	}, withCookie(cookie))
	if r.code != http.StatusSeeOther {
		t.Fatalf("share = %d %s, want 303", r.code, r.body)
	}
	loc := r.hdr.Get("Location")
	if !strings.HasPrefix(loc, testOrigin+"/") {
		t.Fatalf("Location = %q, want the clip's share page on this origin", loc)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", r.hdr.Get("Cache-Control"))
	}
	id := strings.TrimPrefix(loc, testOrigin+"/")
	c := e.getClip(id)
	if c.OwnerID != owner.ID || c.Title != "From my phone" {
		t.Fatalf("clip = %+v, want the session user's, titled from the share", c)
	}
	if c.Description != "" {
		t.Errorf("description = %q: the share's `text` must not become one", c.Description)
	}
	// It went through the ordinary ingest, so there is a transcode job.
	if _, err := e.db.LatestJobOfKind(context.Background(), id, db.KindTranscode); err != nil {
		t.Fatalf("no transcode job for the shared clip: %v", err)
	}
}

// TestShareTargetTitleEqualToTheFilenameIsDropped: Android fills `title` from
// the file name constantly, and erratum 60's rule applies to this route for
// exactly the reason it applies to the capture one.
func TestShareTargetTitleEqualToTheFilenameIsDropped(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("owner")
	r := e.shareReq("PXL_20260906_223301.mp4", testVideo(900), []part{
		{name: "title", data: []byte("PXL_20260906_223301.mp4")},
	}, withCookie(cookie))
	if r.code != http.StatusSeeOther {
		t.Fatalf("share = %d %s", r.code, r.body)
	}
	id := strings.TrimPrefix(r.hdr.Get("Location"), testOrigin+"/")
	if got := e.getClip(id).Title; got != "PXL_20260906_223301" {
		t.Fatalf("title = %q, want the filename without its extension", got)
	}
}

// TestShareTargetRequiresASession is the whole CSRF and credential story of
// the route: the session is the credential, the ordinary rule is the binding,
// and a signed-out phone gets a page it can act on rather than an envelope.
func TestShareTargetRequiresASession(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	token := e.newToken(owner, "upload")

	t.Run("anonymous JSON client", func(t *testing.T) {
		r := e.shareReq("x.mp4", testVideo(100), nil)
		if r.code != 401 || r.errCode() != "unauthorized" {
			t.Fatalf("%d %s, want 401 unauthorized", r.code, r.body)
		}
	})
	t.Run("anonymous browser gets the interstitial", func(t *testing.T) {
		r := e.shareReq("x.mp4", testVideo(100), nil, withHeader("Accept", "text/html,application/xhtml+xml"))
		if r.code != 401 {
			t.Fatalf("%d, want 401", r.code)
		}
		if ct := r.hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("Content-Type = %q, want text/html", ct)
		}
		body := string(r.body)
		for _, want := range []string{"Sign in to share", `href="/login"`, "nothing was uploaded"} {
			if !strings.Contains(body, want) {
				t.Fatalf("interstitial lacks %q: %s", want, body)
			}
		}
		// The one thing it must not be: a redirect. The browser does not
		// re-POST after a login, so a redirect would drop the file silently.
		if r.hdr.Get("Location") != "" {
			t.Fatalf("the signed-out answer redirects to %q", r.hdr.Get("Location"))
		}
		// No inline script or style: the route carries the page CSP.
		if strings.Contains(body, "<script") || strings.Contains(body, "style=") {
			t.Fatalf("the interstitial carries inline script or style: %s", body)
		}
	})
	t.Run("a capture token", func(t *testing.T) {
		r := e.shareReq("x.mp4", testVideo(100), nil, withBearer(token))
		if r.code != 403 || r.errCode() != "token_not_allowed" {
			t.Fatalf("%d %s, want 403 token_not_allowed", r.code, r.body)
		}
	})
	t.Run("a cross-site POST", func(t *testing.T) {
		r := e.shareReq("x.mp4", testVideo(100), nil, withCookie(cookie),
			withHeader("Origin", "https://evil.example"))
		if r.code != 403 || r.errCode() != "csrf" {
			t.Fatalf("%d %s, want 403 csrf", r.code, r.body)
		}
	})
	t.Run("a share-sheet launch carries no Origin and is admitted", func(t *testing.T) {
		// What Chrome actually sends: a top-level navigation the browser
		// itself started, so `Sec-Fetch-Site: none` and no Origin at all.
		r := e.shareReq("x.mp4", testVideo(100), nil, withCookie(cookie),
			withHeader("Sec-Fetch-Site", "none"), withHeader("Sec-Fetch-Mode", "navigate"))
		if r.code != http.StatusSeeOther {
			t.Fatalf("%d %s, want 303", r.code, r.body)
		}
	})
	// Nothing above but the last one reached the bucket.
	clips := 0
	for _, k := range e.fake.Keys() {
		if strings.HasSuffix(k, "/original/source.mp4") {
			clips++
		}
	}
	if clips != 1 {
		t.Fatalf("%d originals in the bucket, want 1 (only the admitted share)", clips)
	}
}

// TestShareTargetWaitsForReady: the share sheet's caller is a browser about to
// LAND on the share page, so it takes the capture endpoint's ready wait
// (§20 erratum 97) rather than dropping the user on "Processing…".
func TestShareTargetWaitsForReady(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.CaptureReadyWait = 5 * 1e9 })
	_, cookie := e.newUser("owner")
	e.worker.script(func(id string) {
		if err := e.finishTranscode(id); err != nil {
			t.Error(err)
		}
		e.worker.finish(id)
	})
	r := e.shareReq("x.mp4", testVideo(900), nil, withCookie(cookie))
	if r.code != http.StatusSeeOther {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if e.worker.awaited.Load() == 0 {
		t.Fatal("the share target did not wait for the clip to be ready")
	}
	id := strings.TrimPrefix(r.hdr.Get("Location"), testOrigin+"/")
	if got := e.getClip(id); got.State != db.ClipReady {
		t.Fatalf("state = %q, want ready before the redirect", got.State)
	}
}
