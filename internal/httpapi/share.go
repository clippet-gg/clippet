package httpapi

import (
	"html/template"
	"net/http"
	"strings"

	"clippet/internal/auth"
	"clippet/internal/httpx"
)

// The phone share-sheet target (ROADMAP #27, §20 erratum 137).
//
//	POST /api/share   multipart, session only → 303 to the clip's share page
//
// `/manifest.webmanifest` (served by internal/web) declares this URL as the
// app's `share_target`. On an installed Android PWA that puts Clippet in the
// system share sheet: picking it makes the BROWSER navigate to this route with
// a multipart POST carrying the file — no page of ours is involved, and no
// script of ours runs.
//
// **The credential and the CSRF story.** A share-sheet launch is a top-level
// navigation the browser itself starts, so it carries no `Origin` (there is no
// initiating document) and `Sec-Fetch-Site: none` rather than `cross-site`.
// The binding is therefore the session cookie plus the ordinary §15 rule this
// route runs under, unchanged: `auth.CSRF` refuses a cookie-authenticated
// mutation whose `Origin` is a different origin, or whose `Sec-Fetch-Site` is
// `cross-site`. A hostile page's cross-origin form POST carries both of those
// marks and is refused; a share-sheet launch carries neither and is admitted,
// exactly as a same-origin form POST is. There is no token to add, because
// there is no page of ours to put one on: the browser composes this request
// from the manifest, and any secret the manifest could carry would be a secret
// every reader of the manifest has.
//
// **A logged-out share is an interstitial, not a redirect.** The browser does
// not re-POST after a login, so sending the user to /login would silently drop
// the file they chose. The answer is 401 with a page that says what happened
// and links to the login form, so the honest next step — log in, share again —
// is the one on screen. The body is HTML only for a client that asked for HTML
// (the browser); everything else, the E2E included, gets the ordinary JSON
// envelope. The file itself is never read on that path (§20 erratum 138).
//
// **No service worker.** Nothing here caches, and nothing here is offline. The
// share target is a plain route, the manifest is a static document, and the
// one thing a service worker would add — an install prompt on some Chrome
// versions — is not worth a script that outlives the page, serves stale HTML
// and would need its own answer to "what does it call".
//
// This is the INBOUND direction, and it is the only share machinery this
// product has: erratum 98(c) forbids share TARGETS on the watch page, meaning
// buttons that hand a viewer's clip to a third party. This hands the phone's
// file to Clippet. Nothing leaves.

// shareTarget is POST /api/share. It is captureUpload's grammar, ingest and
// ready-wait — a share is a capture upload that a phone started — with a
// redirect instead of a JSON body, because its caller is a browser being
// navigated rather than a tool reading an answer.
func (h *handler) shareTarget(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	mr, ok := h.beginUpload(w, r)
	if !ok {
		return
	}
	// The manifest's `params` name `title`, `text` and `url`; the capture form
	// reads `title` (and would read a TTL or tags a hand-built form sent) and
	// ignores the rest, which is exactly right — `text` and `url` are the
	// sharing app's commentary, not the clip's title.
	meta, part, ok := h.readCaptureForm(w, r, mr)
	if !ok {
		return
	}
	meta.title = titleUnlessFilename(meta.title, part.FileName())
	// A duplicate (§20 erratum 326) lands on the clip the account already
	// holds, which is exactly where a share of it should land.
	clip, _, ok := h.ingest(w, r, p, meta, part)
	if !ok {
		return
	}
	// The same wait POST /api/upload takes (§20 errata 97, 212), for the same
	// reason and one more: the browser is about to LAND on the share page, and
	// landing on "Processing…" when half a minute would have shown the player is
	// a worse first impression than half a minute of a spinner in the share
	// sheet.
	clip = h.awaitCardReady(r, clip, h.captureWait())
	h.d.Log.Info("share target upload", "clip", clip.ID, "user", p.User.ID, "state", clip.State)
	// 303: the POST is finished and what follows is a GET of a different
	// resource, which is also what stops a reload of the landing page from
	// re-posting the file.
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, h.d.URLs.Page(clip.ID), http.StatusSeeOther)
}

// shareInterstitial is the 401 of POST /api/share for a browser: the one
// failure a phone will actually hit, and the one a JSON envelope would strand.
// It runs as middleware, above the handler, so the body is still unread.
func shareInterstitial(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.FromContext(r.Context()); ok || !wantsHTML(r) {
			next.ServeHTTP(w, r)
			return
		}
		httpx.SetLogField(r, "reject", "share_signed_out")
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		_ = shareSignedOut.Execute(w, nil)
	})
}

// wantsHTML reports whether the caller is a browser navigating rather than a
// tool reading JSON.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// shareSignedOut is that page. It is static, carries no inline script or style
// (the route's CSP is the page CSP) and links only to this origin's own login
// form — there is nothing to fill in and nothing to send.
var shareSignedOut = template.Must(template.New("share").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in to share &middot; Clippet</title><meta name="robots" content="noindex">
<link rel="stylesheet" href="/static/app.css"></head>
<body><main><section class="card">
<h1>Sign in to share</h1>
<p>Clippet did not receive your video, because this browser is not signed in.
Your phone cannot send it again by itself, so nothing was uploaded.</p>
<p><a href="/login">Sign in</a>, then share the video again from the same app.</p>
</section></main></body>
</html>
`))
