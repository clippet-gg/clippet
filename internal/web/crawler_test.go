package web

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"clippet/internal/auth"
	"clippet/internal/httpx"
)

const robotsMeta = `<meta name="robots" content="noindex">`

// TestUnlistedPagesAreNoIndex: "unlisted" means do not surface this (§8.2),
// and a search result is exactly the surfacing it rules out. A listed clip is
// unchanged. The collection page is ALWAYS noindex: §9.4 lists a collection in
// one place, the owner's own library, so there is no "listed" collection for a
// search engine to have been meant to find.
func TestUnlistedPagesAreNoIndex(t *testing.T) {
	f := newFixture(t)
	for name, a := range map[string]auth.Authenticator{"anonymous": passAuth{}, "owner": session(f.owner)} {
		t.Run(name, func(t *testing.T) {
			// f.queued is the fixture's unlisted clip; f.ready is listed.
			if body := get(f.handler(a), "/"+f.queued.ID).Body.String(); !strings.Contains(body, robotsMeta) {
				t.Error("unlisted clip page carries no noindex")
			}
			if body := get(f.handler(a), "/"+f.ready.ID).Body.String(); strings.Contains(body, robotsMeta) {
				t.Error("listed clip page carries noindex")
			}
			col := f.mkCollection(f.owner, "Best of 2026", f.ready)
			if body := get(f.handler(a), "/c/"+col.ID).Body.String(); !strings.Contains(body, robotsMeta) {
				t.Error("collection page carries no noindex")
			}
		})
	}

	// The card tags are untouched: /robots.txt lets the unfurlers through and
	// `meta robots` does not stop og: reading in the first place, so an
	// unlisted clip still pastes into Discord as the card it always did (§9.1
	// renders unlisted and listed identically, and still does).
	mustContain(t, get(f.handler(passAuth{}), "/"+f.queued.ID).Body.String(),
		robotsMeta,
		`<meta property="og:title" content="Just uploaded">`,
		`<meta property="og:site_name" content="example.test">`)

	// The pages that always carried it still do.
	for _, path := range []string{"/nope", "/aaaaaaaaaaaa"} {
		if body := get(f.handler(passAuth{}), path).Body.String(); !strings.Contains(body, robotsMeta) {
			t.Errorf("%s carries no noindex", path)
		}
	}
}

// TestRobotsTxt: the crawler policy the app never had. Since the id moved to
// the root (§20 erratum 248) the whole site is closed to general indexing and
// the four addresses meant to be found are allowed back — a share link is a
// capability, and capability URLs reach crawlers routinely — while the four
// unfurlers are named individually with an EMPTY Disallow, which in robots.txt
// means "allow everything". Without those groups the `*` block would apply to
// them too and the product's whole point would stop working.
func TestRobotsTxt(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	rec := get(h, "/robots.txt")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	hd := rec.Header()
	if ct := hd.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type %q", ct)
	}
	if cc := hd.Get("Cache-Control"); cc != staticCacheControl {
		t.Errorf("Cache-Control %q, want %q", cc, staticCacheControl)
	}
	body := rec.Body.String()
	mustContain(t, body,
		"User-agent: *\n", "Disallow: /\n", "Allow: /$\n", "Allow: /login\n", "Allow: /register\n",
		"Allow: /static/\n",
		"User-agent: Discordbot\n", "User-agent: Twitterbot\n",
		"User-agent: facebookexternalhit\n", "User-agent: Slackbot-LinkExpanding\n")
	// The general block is the WHOLE site, not a list of prefixes no share link
	// has used since erratum 248.
	if strings.Contains(body, "Disallow: /c") {
		t.Error("robots.txt still names a /clips/ or /c/ prefix, which is no longer where a share link lives")
	}
	// Each unfurler group is followed by a BARE `Disallow:` — the "allow
	// everything" spelling. A path there would block the card instead.
	for _, bot := range []string{"Discordbot", "Twitterbot", "facebookexternalhit", "Slackbot-LinkExpanding"} {
		head := "User-agent: " + bot + "\n"
		i := strings.Index(body, head)
		if i < 0 {
			t.Fatalf("%s group missing", bot)
		}
		line, _, _ := strings.Cut(body[i+len(head):], "\n")
		if line != "Disallow:" {
			t.Errorf("%s is followed by %q, want a bare \"Disallow:\"", bot, line)
		}
	}
	// It carries the page headers like every other route, HEAD works, and
	// anything else is the 405 the rest of the package answers.
	assertNonEmbedHeaders(t, rec)
	if hd.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("robots.txt: nosniff missing")
	}
	headRec := do(h, http.MethodHead, "/robots.txt")
	if headRec.Code != 200 || headRec.Body.Len() != 0 || headRec.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD /robots.txt: %d body=%d len=%q", headRec.Code, headRec.Body.Len(), headRec.Header().Get("Content-Length"))
	}
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		if rec := do(h, m, "/robots.txt"); rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s /robots.txt: %d Allow=%q", m, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

// TestPageHeadersCarryTheNewDirectives wires httpx's header set to real pages:
// the three CSP directives default-src does not cover, and HSTS that follows
// the PUBLIC origin's scheme (TLS is terminated upstream, so the request never
// carries the fact) rather than being emitted unconditionally.
func TestPageHeadersCarryTheNewDirectives(t *testing.T) {
	f := newFixture(t)
	plain := f.handler(passAuth{})
	secure := f.handlerBase(passAuth{}, "https://example.test")
	for _, path := range []string{"/" + f.ready.ID, "/login", "/robots.txt", "/embed/" + f.ready.ID} {
		rec := get(plain, path)
		csp := rec.Header().Get("Content-Security-Policy")
		for _, want := range []string{"object-src 'none'", "base-uri 'none'", "form-action 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP lacks %q (%s)", path, want, csp)
			}
		}
		// A local http dev stack must be unaffected.
		if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
			t.Errorf("%s on an http origin: HSTS %q, want absent", path, got)
		}
		if got := get(secure, path).Header().Get("Strict-Transport-Security"); got != httpx.HSTS {
			t.Errorf("%s on an https origin: HSTS %q, want %q", path, got, httpx.HSTS)
		}
	}
}

// TestStylesheetWrapsUnbreakableText is the served-asset grep TestStaticAssets
// already runs over app.js: there is no CSS parser here and inventing one
// would be worse than the rule it checked. A 200-character no-space title is
// the realistic case (ShareX and RePlays default the title to the recording
// file name); without the wrap it sets the DOCUMENT's width and the player
// ends up off the side of a page that scrolls sideways. The RENDER half is
// asserted too: the title reaches the <h1> whole.
func TestStylesheetWrapsUnbreakableText(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("Replay_2026-09-06_17-14-22_Overwatch_", 6)[:200]
	c, _ := f.create(f.owner, long, "", false, nil)
	body := get(f.handler(passAuth{}), "/"+c.ID).Body.String()
	mustContain(t, body, "<h1>"+long+"</h1>", "<title>"+long+" · Clippet</title>")
	assertNoInline(t, "long title", body)

	css, err := io.ReadAll(get(f.handler(passAuth{}), "/static/app.css").Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"overflow-wrap: anywhere", "word-break: break-word"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}
