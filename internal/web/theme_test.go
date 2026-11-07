package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"clippet/internal/auth"
)

// postForm submits an ordinary HTML form, which is what both theme controls are
// without JavaScript. Every other POST this package serves is JSON, so there is
// no shared helper to borrow.
func postForm(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// setCookie returns the one Set-Cookie line naming `name`, or "".
func setCookie(rec *httptest.ResponseRecorder, name string) string {
	for _, v := range rec.Result().Header.Values("Set-Cookie") {
		if strings.HasPrefix(v, name+"=") {
			return v
		}
	}
	return ""
}

// TestThemeGlyphIsInTheBar: the top bar carries EXACTLY the two theme buttons —
// one per theme, each naming the theme it hands you — and it carries them on
// every page that has a bar, a stranger's share page included (§20 erratum 288).
// A page with no bar at all carries neither, and nothing else on the page does.
func TestThemeGlyphIsInTheBar(t *testing.T) {
	f := newFixture(t)
	col := f.mkCollection(f.owner, "Best of 2026", f.ready)
	for _, c := range []struct {
		name, path string
		session    bool
		bar        bool
	}{
		{"the library", "/", true, true},
		{"settings", "/settings", true, true},
		{"the owner's share page", "/" + f.ready.ID, true, true},
		{"a stranger's share page", "/" + f.ready.ID, false, true},
		{"a stranger's collection page", "/c/" + col.ID, false, true},
		{"the 404 with a session", "/AAAAAAAAAAAA", true, true},
		// The pages that are their own front door have no header slot by
		// design (§20 errata 244 and 246), so they have no glyph either.
		{"the front door", "/", false, false},
		{"login", "/login", false, false},
		{"register", "/register", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var a auth.Authenticator = passAuth{}
			if c.session {
				a = session(f.owner)
			}
			body := get(f.handler(a), c.path).Body.String()
			want := 0
			if c.bar {
				want = 2
			}
			if n := strings.Count(body, `data-theme-to=`); n != want {
				t.Fatalf("%s carries %d theme buttons, want %d", c.path, n, want)
			}
			if !c.bar {
				mustNotContain(t, body, `action="/settings/theme"`)
				return
			}
			mustContain(t, body,
				`<form class="theme-glyph" action="/settings/theme" method="post" data-theme-glyph>`,
				`name="theme" value="light" data-theme-to="light" aria-label="Light theme" title="Light theme"`,
				`name="theme" value="dark" data-theme-to="dark" aria-label="Dark theme" title="Dark theme"`,
				`<input type="hidden" name="next" value="`+c.path+`">`,
			)
			// It is in the bar's END SLOT, which is the last thing in the bar,
			// and it is NOT inside the nav (it navigates nowhere; and a
			// stranger's page has no nav at all — §20 erratum 244).
			head := body[:strings.Index(body, "</header>")]
			if !strings.Contains(head, "data-theme-glyph") {
				t.Errorf("%s: the glyph is not in the header", c.path)
			}
			end := head[strings.Index(head, `<div class="bar-end">`):]
			if strings.Count(end, `data-theme-to=`) != 2 {
				t.Errorf("%s: the glyph is not in the bar's end slot", c.path)
			}
			if i := strings.Index(head, "</nav>"); i >= 0 && strings.Contains(head[:i], "data-theme-to=") {
				t.Errorf("%s: the glyph is inside the nav", c.path)
			}
			if c.session != strings.Contains(head, "<nav>") {
				t.Errorf("%s: the nav is rendered for the wrong viewer (§20 erratum 244)", c.path)
			}
			// No page may carry a second copy of it, and no inline anything.
			assertNoInline(t, c.path, body)
		})
	}
}

// The glyph is a control on a page, so it must not have taught the page a new
// way to spell the theme: `data-theme` is still the cookie's answer and only
// the cookie's (§20 erratum 242), and the hidden `next` is this page's own path
// and never a query, a host or a fragment somebody sent.
func TestThemeGlyphDoesNotChangeTheRenderedTheme(t *testing.T) {
	f := newFixture(t)
	h := f.handler(session(f.owner))
	for _, c := range []struct{ cookie, want string }{
		{"dark", `<html lang="en" data-theme="dark">`},
		{"light", `<html lang="en" data-theme="light">`},
		{"purple", `<html lang="en">`},
		{"", `<html lang="en">`},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.cookie != "" {
			req.AddCookie(&http.Cookie{Name: "theme", Value: c.cookie})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if !strings.Contains(body, c.want) {
			t.Errorf("theme=%q: the page does not open %q", c.cookie, c.want)
		}
		if strings.Count(body, "data-theme-to=") != 2 {
			t.Errorf("theme=%q: the glyph is not the two buttons", c.cookie)
		}
	}
	// A page whose address carries a query: the `next` is the path, no more —
	// a `next` needs the page and nothing a client typed after it.
	body := get(h, "/?after=abc").Body.String()
	mustContain(t, body, `<input type="hidden" name="next" value="/">`)
}

// TestSetThemeIsAnonymous: the route answers a browser with no session, because
// the glyph is in the bar of a stranger's share page (§20 erratum 288). It sets
// the same cookie it always did and comes back to the page that posted it.
func TestSetThemeIsAnonymous(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	rec := postForm(h, "/settings/theme", url.Values{"theme": {"dark"}, "next": {"/" + f.ready.ID}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("anonymous POST: %d, want 303 (never a redirect to /login)", rec.Code)
	}
	sc := setCookie(rec, "theme")
	for _, want := range []string{"theme=dark", "Path=/", "Max-Age=31536000", "SameSite=Lax"} {
		if !strings.Contains(sc, want) {
			t.Errorf("Set-Cookie %q lacks %q", sc, want)
		}
	}
	if strings.Contains(sc, "HttpOnly") {
		t.Errorf("Set-Cookie %q is HttpOnly: app.js writes this cookie too", sc)
	}
	if loc := rec.Header().Get("Location"); loc != "/"+f.ready.ID {
		t.Errorf("Location %q, want the page it was pressed on", loc)
	}
	// …and `system` still deletes it, anonymously too.
	rec = postForm(h, "/settings/theme", url.Values{"theme": {"system"}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(setCookie(rec, "theme"), "Max-Age=0") {
		t.Errorf("anonymous theme=system: %d %q", rec.Code, setCookie(rec, "theme"))
	}
}

// Where it comes back to. The hidden field is ours, but the value that arrives
// is the client's, so it goes through safeNext exactly as a login `next` does —
// and a form with no field at all is the Settings picker, which belongs on
// Settings.
func TestSetThemeComesBackToTheSamePage(t *testing.T) {
	f := newFixture(t)
	h := f.handler(session(f.owner))
	for _, c := range []struct{ next, want string }{
		{"", themeHome},
		{"/settings#theme", "/settings#theme"},
		{"/" + f.ready.ID, "/" + f.ready.ID},
		{"/c/xyz", "/c/xyz"},
		{"//evil.example/", "/"},
		{`/\evil.example`, "/"},
		{"/./\\/evil.example", "/"},
		{"https://evil.example/", "/"},
		{"/settings\r\nX-Evil: 1", "/"},
	} {
		form := url.Values{"theme": {"light"}}
		if c.next != "" {
			form.Set("next", c.next)
		}
		rec := postForm(h, "/settings/theme", form)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("next=%q: %d, want 303", c.next, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != c.want {
			t.Errorf("next=%q: Location %q, want %q", c.next, loc, c.want)
		}
	}
}

// A value neither control ever writes is a 400, signed in or out — unchanged by
// erratum 288 — and GET is still not a page.
func TestSetThemeRefusesWhatNoControlWrites(t *testing.T) {
	f := newFixture(t)
	for _, a := range []auth.Authenticator{passAuth{}, session(f.owner)} {
		h := f.handler(a)
		rec := postForm(h, "/settings/theme", url.Values{"theme": {"neon"}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("theme=neon: %d, want 400", rec.Code)
		}
		if sc := setCookie(rec, "theme"); sc != "" {
			t.Errorf("theme=neon set a cookie: %q", sc)
		}
		rec = do(h, http.MethodGet, "/settings/theme")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
			t.Errorf("GET /settings/theme: %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
		}
	}
}

// …and it costs no session lookup at all: the route is the one page route
// outside the Authenticator (§20 erratum 288), because its answer is the same
// for everybody.
func TestSetThemeCostsNoSessionLookup(t *testing.T) {
	f := newFixture(t)
	n := 0
	h := f.handler(countingAuth{&n})
	if rec := postForm(h, "/settings/theme", url.Values{"theme": {"dark"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings/theme: %d", rec.Code)
	}
	if n != 0 {
		t.Errorf("the Authenticator ran %d times for a theme POST, want 0", n)
	}
}
