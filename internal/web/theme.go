package web

import (
	"net/http"
	"strings"
)

// The theme choice (§20 erratum 242; web/STYLE.md). Both themes are first
// class and the default is the operating system's own preference, read by the
// stylesheet's `prefers-color-scheme` block. A person who wants the other one
// says so ONCE, in Settings, and the answer is a cookie — not a column: it is a
// fact about this browser, not about the account, exactly like the muted and
// loop preferences app.js keeps in localStorage. The page handlers read it and
// render it as `data-theme` on <html>, which is what makes the choice paint
// with the first byte of the page instead of after a script runs: there is no
// flash and no inline script (CSP script-src 'self' has no room for one).
const (
	themeCookie = "theme"
	themeLight  = "light"
	themeDark   = "dark"
	themeSystem = "system"
	// A year. The cookie is re-set on every change, so a person who keeps a
	// preference keeps it; one who never changes it again falls back to the
	// system after a year, which is the right default to fall back to.
	themeMaxAge = 365 * 24 * 3600
)

// themeOf reads the cookie. Anything but the two spellings the control writes
// is "follow the system", so a stale or hand-edited value can only ever produce
// the default look.
func themeOf(r *http.Request) string {
	c, err := r.Cookie(themeCookie)
	if err != nil {
		return ""
	}
	switch c.Value {
	case themeLight, themeDark:
		return c.Value
	}
	return ""
}

// themeHome is where a theme POST that names no page of its own comes back to:
// the Settings control's own row. It is the answer for the three-way picker,
// which is rendered on exactly that page.
const themeHome = "/settings#theme"

// setTheme answers POST /settings/theme, the no-JavaScript half of BOTH theme
// controls — the Settings picker (§20 erratum 242) and the top bar's glyph
// (erratum 288). app.js enhances the same two forms and never calls this.
// It takes a form (`theme=system|light|dark`), so a browser with no script can
// submit it; `form-action 'self'` in the CSP covers it (internal/httpx
// middleware.go's cspBase). `SameSite=Lax` and `Secure` iff the public origin
// is https — the session cookie's own rules. It is deliberately NOT HttpOnly:
// the client writes it too.
//
// It is ANONYMOUS, and the route is not wrapped in the Authenticator at all
// (server.go): the glyph is in the header of a stranger's share page, and this
// handler writes one cookie about ONE BROWSER and reads no row, no session and
// no database — so there is nothing here for a principal to change and nothing
// for a resolve error to say. A cross-site POST can therefore do exactly what
// it could before: change a visitor's theme, once, in their own browser.
func (s *server) setTheme(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	want := strings.TrimSpace(r.PostForm.Get("theme"))
	c := &http.Cookie{
		Name:     themeCookie,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		Secure:   s.d.URLs.IsHTTPS(),
	}
	switch want {
	case themeLight, themeDark:
		c.Value = want
		c.MaxAge = themeMaxAge
	case themeSystem, "":
		c.MaxAge = -1 // delete: the system decides again
	default:
		http.Error(w, "theme must be system, light or dark", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, c)
	redirectPrivate(w, r, themeNext(r))
}

// themeNext is the page the browser is sent back to. The glyph can be pressed
// on any page that carries the header bar, so the form names its own page in a
// hidden field (page.Path, rendered by base.html) and the answer is that page
// rather than Settings — a control that teleported a viewer off the clip they
// were watching would be a worse control than no control.
//
// The value goes through safeNext, the same gate the login form's `next` does
// (§20 erratum 52): a single-slash-rooted path on this origin, in its own
// normal form, or "/". It is never trusted because it is ours — a cross-site
// POST carries whatever it likes — and never used for anything but a redirect.
func themeNext(r *http.Request) string {
	if v := r.PostForm.Get("next"); v != "" {
		return safeNext(v)
	}
	return themeHome
}

// postOnly is getOnly's twin for the one form route.
func postOnly(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	})
}
