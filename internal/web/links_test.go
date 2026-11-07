package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"clippet/internal/auth"
	"clippet/internal/db"
	"clippet/internal/ids"
)

// The share link at the root, its old address, and the two rules that keep the
// one from eating the other (ROADMAP #35, §20 errata 248-251).

// refreshingAuth is the Resolver's sliding session touch as a test double: it
// sets the principal AND writes the refreshed cookie onto the response, which
// is the case `clipRedirect`'s Cache-Control has to notice (§20 erratum 248,
// following 207's rule for `/original`). The real Resolver does exactly this,
// before the handler runs.
type refreshingAuth struct{ u db.User }

func (a refreshingAuth) Middleware(next http.Handler) http.Handler {
	inner := session(a.u).Middleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "clippet_session", Value: "refreshed", Path: "/"})
		inner.ServeHTTP(w, r)
	})
}

// createWithID inserts a clip under an id the test chose, which is the only way
// to put a MEMORABLE id in front of the router: the fixture's own helper mints
// compact ones like every ordinary upload.
func (f *fixture) createWithID(id string, owner db.User, title string, unlisted bool) db.Clip {
	f.t.Helper()
	c, _, err := f.db.CreateClipWithJob(context.Background(), db.NewClip{
		ID: id, OwnerID: owner.ID, Title: title, Unlisted: unlisted,
		SourceKey: "clips/" + id + "/original/source.mp4", SourceFilename: "clip.mp4",
		SourceSize: 123, SourceContentType: "video/mp4", SourceETag: "etag", Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("CreateClipWithJob(%s): %v", id, err)
	}
	f.now++
	return c
}

// TestShareURLIsTheIDAtTheRoot: `/{id}` serves exactly what `/clips/{id}` used
// to — the same page, the same card tags, the same canonical, the same headers
// — and `og:url` and the canonical now name the root form, which is the whole
// point of having a domain.
func TestShareURLIsTheIDAtTheRoot(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})

	rec := get(h, "/"+f.ready.ID)
	if rec.Code != 200 {
		t.Fatalf("GET /{id} = %d", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body,
		`<link rel="canonical" href="`+testBase+`/`+f.ready.ID+`">`,
		`<meta property="og:url" content="`+testBase+`/`+f.ready.ID+`">`,
		`data-page-url="`+testBase+`/`+f.ready.ID+`"`,
		`<h1>E2E std</h1>`)
	if strings.Contains(body, testBase+"/clips/") {
		t.Error("the share page still renders a /clips/ URL of its own")
	}
	// The §9.1 discipline is untouched at the new address.
	if rec := do(h, http.MethodHead, "/"+f.ready.ID); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("HEAD /{id} = %d body=%d", rec.Code, rec.Body.Len())
	}
	if rec := get(h, "/"+f.deleting.ID); rec.Code != 404 {
		t.Errorf("a tombstone at the root = %d, want 404", rec.Code)
	}
	if rec := get(h, "/"+ids.New()); rec.Code != 404 {
		t.Errorf("an unknown id at the root = %d, want 404", rec.Code)
	}
	// A single segment that is not an id is the 404 PAGE — not a redirect, not
	// a 500 — for every method, exactly as the catch-all answered it before.
	// `/sitemap.xml` is the dotted case (it was `/favicon.ico` until §20
	// erratum 304 made that one a route): a segment neither id grammar admits
	// is still the page, and a literal route is still the only exception.
	for _, path := range []string{"/nope", "/not-an-id", "/aZ3kQ9xB2mL", "/sitemap.xml"} {
		if rec := get(h, path); rec.Code != 404 {
			t.Errorf("GET %s = %d, want the 404 page", path, rec.Code)
		}
		if rec := do(h, http.MethodPost, path); rec.Code != 404 {
			t.Errorf("POST %s = %d, want 404 (405 would say every one-segment path is a route)", path, rec.Code)
		}
	}
	// An id-shaped segment DOES answer 405 for a method it has not got, exactly
	// as /clips/{id} always did.
	rec = do(h, http.MethodPost, "/"+f.ready.ID)
	if rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST /{id} = %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

// TestLegacyClipURLRedirects: every link already sent still works, with a
// method-preserving permanent redirect and no database read at all.
func TestLegacyClipURLRedirects(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})

	for _, m := range []string{http.MethodGet, http.MethodHead} {
		rec := do(h, m, "/clips/"+f.ready.ID)
		if rec.Code != http.StatusPermanentRedirect {
			t.Errorf("%s /clips/{id} = %d, want 308 (a 301 lets a client turn a HEAD into a GET)", m, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/"+f.ready.ID {
			t.Errorf("%s /clips/{id} → %q, want %q", m, loc, "/"+f.ready.ID)
		}
	}
	// It is a pure function of the path: publicly cacheable, and the same
	// answer for an id no clip has, so it reports on nobody's existence.
	rec := get(h, "/clips/"+f.ready.ID)
	if cc := rec.Header().Get("Cache-Control"); cc != staticCacheControl {
		t.Errorf("the redirect's Cache-Control = %q, want %q", cc, staticCacheControl)
	}
	// …EXCEPT when the Resolver's sliding touch put a cookie on this very
	// response. A shared cache storing THAT publicly would hand one person's
	// session to the next caller (§20 erratum 207's rule for /original).
	cookieRec := get(f.handler(refreshingAuth{f.owner}), "/clips/"+f.ready.ID)
	if cookieRec.Code != http.StatusPermanentRedirect {
		t.Fatalf("the redirect under a refreshing session = %d", cookieRec.Code)
	}
	if cc := cookieRec.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("a redirect carrying Set-Cookie is %q, want private, no-store", cc)
	}
	if v := cookieRec.Header().Get("Vary"); v != "Cookie" {
		t.Errorf("a redirect carrying Set-Cookie varies on %q, want Cookie", v)
	}
	unknown := ids.New()
	if rec := get(h, "/clips/"+unknown); rec.Code != http.StatusPermanentRedirect {
		t.Errorf("/clips/{unknown id} = %d, want 308 — the redirect must not probe existence", rec.Code)
	}
	// …and the hop lands on the SAME 404 the root form gives directly, so the
	// two spellings cannot be told apart by a scanner either.
	direct := get(h, "/"+unknown)
	hop := get(h, "/clips/"+unknown)
	hopped := get(h, hop.Header().Get("Location"))
	if direct.Code != 404 || hopped.Code != 404 || direct.Body.String() != hopped.Body.String() {
		t.Errorf("the redirect's destination differs from the root form: %d vs %d", direct.Code, hopped.Code)
	}
	// The query string rides along; a segment that is not an id is the 404 page
	// rather than somebody's cached permanent redirect.
	rec = get(h, "/clips/"+f.ready.ID+"?autoplay=1&loop=1")
	if loc := rec.Header().Get("Location"); loc != "/"+f.ready.ID+"?autoplay=1&loop=1" {
		t.Errorf("the redirect dropped the query: %q", loc)
	}
	for _, bad := range []string{"/clips/not-an-id", "/clips/aZ3kQ9xB2mL", "/clips/" + f.ready.ID + "/extra"} {
		if rec := get(h, bad); rec.Code != 404 {
			t.Errorf("GET %s = %d, want 404", bad, rec.Code)
		}
	}
}

// TestMemorableIDsAreRoutedLikeCompactOnes: the second grammar is not a special
// case anywhere — the same page at the root, the same redirect from the old
// address, the same embed.
func TestMemorableIDsAreRoutedLikeCompactOnes(t *testing.T) {
	f := newFixture(t)
	id := ids.NewMemorable(ids.MemorableMinWords)
	c := f.createWithID(id, f.owner, "Words", false)
	h := f.handler(passAuth{})

	if rec := get(h, "/"+c.ID); rec.Code != 200 {
		t.Fatalf("GET /{memorable id} = %d", rec.Code)
	}
	mustContain(t, get(h, "/"+c.ID).Body.String(),
		`<link rel="canonical" href="`+testBase+`/`+c.ID+`">`)
	if rec := get(h, "/clips/"+c.ID); rec.Code != http.StatusPermanentRedirect ||
		rec.Header().Get("Location") != "/"+c.ID {
		t.Errorf("/clips/{memorable id} = %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := get(h, "/embed/"+c.ID); rec.Code != 200 {
		t.Errorf("GET /embed/{memorable id} = %d", rec.Code)
	}
}

// TestLinkControlShowsAnExampleOfEachGrammar (§20 erratum 264 — for
// compact and memorable, an example of each instead of a description): the hint
// under the Link radios on BOTH pages is one example link per grammar, on the
// configured public host and with no scheme; Settings keeps the word list's
// credit after it. The old sentence is gone from both.
func TestLinkControlShowsAnExampleOfEachGrammar(t *testing.T) {
	// The examples are the two shapes, and each must really be in the grammar
	// it is offered as — a hint that lied would be worse than no hint.
	if got := ids.Kind(exampleCompactID); got != ids.KindCompact {
		t.Errorf("the compact example reads as %q", got)
	}
	if got := ids.Kind(exampleMemorableID); got != ids.KindMemorable {
		t.Errorf("the memorable example reads as %q", got)
	}

	f := newFixture(t)
	h := f.handler(session(f.owner))
	const hint = `Compact: <code>example.test:8481/` + exampleCompactID +
		`</code> · Memorable: <code>example.test:8481/` + exampleMemorableID + `</code>`
	for _, path := range []string{"/", "/settings"} {
		body := get(h, path).Body.String()
		mustContain(t, body, hint)
		mustNotContain(t, body, "Easier to say", "Memorable links read like",
			// The host comes from PUBLIC_BASE_URL, never a literal, and the
			// example is text rather than a link.
			"clippet.gg/", `href="`+"example.test:8481/"+exampleCompactID)
	}
	// Settings, and only Settings, carries the credit — beside the control that
	// spends the list.
	mustContain(t, get(h, "/settings").Body.String(),
		hint+`. Word list: EFF large wordlist, CC BY 3.0.`)
}

// TestNoMemorableGuessLineAnywhere (§20 erratum 264, withdrawing 251): the
// share page says NOTHING about how guessable a memorable id is — not to its
// owner, not to a stranger, listed or unlisted, in either grammar. The row
// erratum 251 shipped is gone, and so is the element app.js used to reveal.
func TestNoMemorableGuessLineAnywhere(t *testing.T) {
	const line = "Memorable links are easier to guess"
	f := newFixture(t)
	memUnlisted := f.createWithID(ids.NewMemorable(ids.MemorableMaxWords), f.owner, "Four words", true)
	memListed := f.createWithID(ids.NewMemorable(ids.MemorableMinWords), f.owner, "Three words", false)

	for _, c := range []struct{ what, id string }{
		{"a memorable unlisted clip", memUnlisted.ID},
		{"a memorable listed clip", memListed.ID},
		{"a compact clip", f.ready.ID},
	} {
		// The owner, a stranger, another account and a Bearer token: nobody.
		for name, a := range map[string]auth.Authenticator{
			"owner": session(f.owner), "anonymous": passAuth{},
			"other user": session(f.other), "token": bearer(f.owner),
		} {
			body := get(f.handler(a), "/"+c.id).Body.String()
			if strings.Contains(body, line) {
				t.Errorf("%s: %s carries the withdrawn sentence", name, c.what)
			}
			if strings.Contains(body, "data-link-note") {
				t.Errorf("%s: %s still carries the withdrawn row", name, c.what)
			}
		}
	}
}

// routedFirstSegments is every LITERAL first path segment the page mux
// registers, derived from the mux itself.
func routedFirstSegments(t *testing.T, patterns []string) []string {
	t.Helper()
	var out []string
	for _, p := range patterns {
		seg := strings.TrimPrefix(p, "/")
		if i := strings.IndexByte(seg, '/'); i >= 0 {
			seg = seg[:i]
		}
		if seg == "" || strings.HasPrefix(seg, "{") {
			continue // "/", "/{$}" and "/{id}" are not literals
		}
		out = append(out, seg)
	}
	return out
}

// pageServer builds the server directly so the test can read the patterns it
// registered; f.handler returns the finished mux and nothing else.
func pageServer(t *testing.T, f *fixture, a auth.Authenticator) *server {
	t.Helper()
	s, err := newServer(Deps{DB: f.db, Auth: a, URLs: builder(t, testBase)})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return s
}

// TestEveryRoutedSegmentIsReserved is the invariant a clip id at the root
// depends on (§20 erratum 248): no minted id may equal a route, and the list
// the minter checks is derived from ONE place. Adding a route without adding
// its word to ids.Reserved fails HERE rather than shadowing somebody's clip
// three months later.
func TestEveryRoutedSegmentIsReserved(t *testing.T) {
	f := newFixture(t)
	s := pageServer(t, f, passAuth{})
	s.handler()
	if len(s.patterns) == 0 {
		t.Fatal("the mux recorded no patterns")
	}
	for _, seg := range routedFirstSegments(t, s.patterns) {
		if !ids.IsReserved(seg) {
			t.Errorf("route segment %q is not in ids.Reserved: an id could be minted that shadows it", seg)
		}
	}
}

// TestRootIDDoesNotShadowALiteralRoute is the precedence proof, as a table over
// EVERY pattern the page mux registers. `/{id}` matches every single-segment
// path, so the only thing keeping `/login` a login form is the Go 1.22 mux's
// specificity rule — and the only thing keeping THAT true is a test that fails
// when a route is added and not listed here.
func TestRootIDDoesNotShadowALiteralRoute(t *testing.T) {
	f := newFixture(t)
	col := f.mkCollection(f.owner, "Best of 2026", f.ready)
	cases := []struct {
		pattern string
		path    string
		auth    auth.Authenticator
		code    int
		sub     string // a marker only THIS route's answer carries
	}{
		{"/{$}", "/", session(f.owner), 200, "data-upload"},
		{"/login", "/login", passAuth{}, 200, `data-auth="login"`},
		{"/register", "/register", passAuth{}, 200, `data-auth="register"`},
		// §20 erratum 341: the bot's one-time login link. This fixture runs no
		// bot, so the door is shut and the route is the 404 page (signin_test.go
		// covers it open) — but it is never the share page.
		{"/login/discord/{token}", "/login/discord/cs_x", passAuth{}, 404, ""},
		{"/settings", "/settings", session(f.owner), 200, "Upload defaults"},
		{"/settings/tokens", "/settings/tokens", session(f.owner), 303, ""},
		{"/settings/theme", "/settings/theme", session(f.owner), 405, ""},
		{"/setup", "/setup", session(f.owner), 200, "Where do clips live?"},
		{"/clips/{id}", "/clips/" + f.ready.ID, passAuth{}, 308, ""},
		{"/c/{id}", "/c/" + col.ID, passAuth{}, 200, "Best of 2026"},
		{"/embed/{id}", "/embed/" + f.ready.ID, passAuth{}, 200, `<body class="embed"`},
		{"/static/{file...}", "/static/app.css", passAuth{}, 200, ""},
		{"/robots.txt", "/robots.txt", passAuth{}, 200, "User-agent: *"},
		{"/manifest.webmanifest", "/manifest.webmanifest", passAuth{}, 200, "share_target"},
		// §20 erratum 304. The marker is the ICO's first PNG payload, which is
		// the one string only this route's answer carries.
		{"/favicon.ico", "/favicon.ico", passAuth{}, 200, "\x89PNG\r\n\x1a\n"},
		{"/{id}", "/" + f.ready.ID, passAuth{}, 200, `<link rel="canonical"`},
		{"/", "/nope/deeper", passAuth{}, 404, ""},
	}
	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.pattern] = true
		rec := do(f.handler(c.auth), http.MethodGet, c.path)
		if rec.Code != c.code {
			t.Errorf("GET %s (%s) = %d, want %d", c.path, c.pattern, rec.Code, c.code)
			continue
		}
		if c.sub != "" && !strings.Contains(rec.Body.String(), c.sub) {
			t.Errorf("GET %s (%s) is not its own page: %q missing", c.path, c.pattern, c.sub)
		}
		// None of them is the share page by accident.
		if c.pattern != "/{id}" && strings.Contains(rec.Body.String(), `data-clip-page data-clip-id="`+f.ready.ID) {
			t.Errorf("GET %s (%s) rendered the share page", c.path, c.pattern)
		}
	}
	s := pageServer(t, f, passAuth{})
	s.handler()
	for _, p := range s.patterns {
		if !covered[p] {
			t.Errorf("the mux registers %q and this table does not exercise it — add a row, "+
				"and its first segment to ids.Reserved", p)
		}
	}
}
