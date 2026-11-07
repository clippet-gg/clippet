package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"clippet/internal/auth"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/urls"
	"clippet/web/static"
	"clippet/web/templates"
)

const (
	defaultSSRTimeout = 5 * time.Second
	retryAfterSeconds = "30"
	dateLayout        = "2006-01-02 15:04 UTC"
	viaSession        = "session"
)

// libraryPageSize is how many clips the library renders per page. A variable
// only so a test can shrink it and exercise the pagination link without
// seeding a hundred rows; the number itself lives in internal/db, beside the
// two API caps that have to agree with it.
var libraryPageSize = db.LibraryPageSize

// The library hands db.ProgressRows a WHOLE page of ids at once (clipRows), and
// that call answers ErrLimitReached above db.MaxProgressIDs — at which point
// every row on the page silently loses its stage phrase and the only trace is
// one WARN. The two constants are equal today, and nothing in this package said
// so: an array cannot have a negative length, so MaxProgressIDs dropping under
// LibraryPageSize stops compiling here instead of going quiet in production.
var _ [db.MaxProgressIDs - db.LibraryPageSize]struct{}

// pageNames are the templates that render inside base.html; embed.html is
// standalone (chrome-less, §9.2).
var pageNames = []string{"front", "index", "login", "register", "signin", "settings", "setup", "clip", "collection", "notfound", "unavailable"}

// server holds the parsed templates and the dependencies of every handler.
type server struct {
	d      Deps
	pages  map[string]*template.Template
	embed  *template.Template
	static *staticHandler
	log    *slog.Logger
	reg    regCache
	// patterns is every mux pattern handler() registered, recorded as it
	// registers them (§20 erratum 248). It exists for one test: with a clip id
	// at the ROOT, `/{id}` shadows nothing only because every page path is a
	// literal the mux prefers — and every one of those literals must also be in
	// ids.Reserved, or tomorrow's route could be yesterday's clip id. Deriving
	// the list from the mux rather than from a list somebody remembers to
	// extend is what makes adding a route without adding the word fail a test.
	patterns []string
}

// registrationCacheTTL is how long the PAGE-side registration flag is reused
// (§20 erratum 106's nit). One second: long enough that a crawler hammering
// /login stops paying a COUNT(*) per request, short enough that nobody
// notices the answer is a moment stale.
const registrationCacheTTL = time.Second

// regCache memoises `registration=first`'s "are there any users yet" answer
// for the two ANONYMOUS pages that draw a signup form or a link to one.
//
// It is deliberately not a latch. `clippet admin users prune` can empty the
// users table again, and `first` must re-open when it does — a cache that
// closed forever would need a restart to undo. Only a SUCCESSFUL count is
// stored, so a transient database error does not stick for a second; and the
// age test rejects a negative age, because the clock this reads can step
// backwards and a cache with no expiry is the one failure mode worth
// spending a comparison on.
//
// Nothing here weakens the route: POST /api/auth/register asks the question
// again, uncached, before it creates anything (internal/httpapi's own count
// carries a comment saying it must stay uncached). The cost of the staleness
// is bounded at one second of a form that answers "registration is closed".
//
// It lives on *server, so it is PER MOUNT: a process that ever called New()
// twice would simply have two of them, each re-counting within the same second,
// and nothing about the route's correctness depends on there being one (§20
// erratum 229). serve.go mounts this package once.
type regCache struct {
	mu    sync.Mutex
	at    time.Time
	open  bool
	valid bool
}

// get returns the cached flag when it is fresh.
func (c *regCache) get(now time.Time) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid {
		return false, false
	}
	age := now.Sub(c.at)
	if age < 0 || age >= registrationCacheTTL {
		return false, false
	}
	return c.open, true
}

func (c *regCache) put(now time.Time, open bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at, c.open, c.valid = now, open, true
}

func newServer(d Deps) (*server, error) {
	if d.DB == nil {
		return nil, errors.New("web: Deps.DB is nil")
	}
	if d.Auth == nil {
		return nil, errors.New("web: Deps.Auth is nil")
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	pages, embed, err := parseTemplates(templates.FS)
	if err != nil {
		return nil, err
	}
	st, err := newStaticHandler(static.FS, d.BuildID)
	if err != nil {
		return nil, err
	}
	return &server{d: d, pages: pages, embed: embed, static: st, log: d.Log}, nil
}

// parseTemplates parses base.html once and clones it per page so each page
// may define its own "content" block without name collisions.
func parseTemplates(fsys fs.FS) (map[string]*template.Template, *template.Template, error) {
	base, err := template.New("root").ParseFS(fsys, "base.html")
	if err != nil {
		return nil, nil, fmt.Errorf("web: parse base.html: %w", err)
	}
	pages := make(map[string]*template.Template, len(pageNames))
	for _, n := range pageNames {
		t, err := base.Clone()
		if err != nil {
			return nil, nil, fmt.Errorf("web: clone base for %s: %w", n, err)
		}
		if _, err := t.ParseFS(fsys, n+".html"); err != nil {
			return nil, nil, fmt.Errorf("web: parse %s.html: %w", n, err)
		}
		pages[n] = t
	}
	embed, err := template.New("embed.html").ParseFS(fsys, "embed.html")
	if err != nil {
		return nil, nil, fmt.Errorf("web: parse embed.html: %w", err)
	}
	return pages, embed, nil
}

// handler builds the mux. Every non-embed route carries the §9.1 headers
// (SecurityHeaders(false)); /embed/{id} carries the iframe-safe set.
//
// The Authenticator wraps only the routes whose output depends on the
// principal. /static/ (hot, cacheable, always sent with the cookie) and the
// chrome-less embed (no view count, no controls, ever — §9.2) never read it,
// so they must not cost the Resolver a session lookup per request. The 404
// catch-all keeps it so a logged-in user's 404 page still shows the header
// nav; cookie-less scanner hits cost the Resolver nothing there.
func (s *server) handler() http.Handler {
	https := s.d.URLs.IsHTTPS()
	page := httpx.SecurityHeaders(false, https, s.d.Cfg.MediaOrigin())
	embed := httpx.SecurityHeaders(true, https, s.d.Cfg.MediaOrigin())
	authed := s.d.Auth.Middleware
	stdmux := http.NewServeMux()
	s.patterns = nil
	mux := patternMux{m: stdmux, seen: &s.patterns}
	mux.Handle("/{$}", authed(page(getOnly(s.index))))
	mux.Handle("/login", authed(page(getOnly(s.login))))
	mux.Handle("/register", authed(page(getOnly(s.register))))
	// The bot's one-time login link (§20 erratum 341, signin.go): GET renders
	// and changes nothing, POST consumes. Its own method switch, because the
	// one path answers both; the 404 page whenever the door is shut.
	mux.Handle(auth.SigninLinkPrefix+"{token}", authed(page(http.HandlerFunc(s.signinLink))))
	// The account page (ROADMAP #1, §20 erratum 110). /settings/tokens is kept
	// answering — a 303 to the tokens section — because it is the URL Phase 1
	// shipped, DEPLOY §9 documents and browsers have bookmarked.
	mux.Handle("/settings", authed(page(getOnly(s.settings))))
	mux.Handle("/settings/tokens", authed(page(getOnly(s.settingsTokens))))
	// The first-run wizard (ROADMAP #45, §20 errata 297-299). Beside
	// /settings, and reached the same way a session-less caller reaches that:
	// a redirect to /login carrying `next`. Its one exception is an instance
	// nobody has registered on yet, where there is no account to be.
	mux.Handle("/setup", authed(page(getOnly(s.setup))))
	// The theme control's no-JavaScript half (§20 errata 242 and 288,
	// theme.go): a form POST that sets the theme cookie and bounces back to
	// the page it was pressed on. It is the ONE page route with no
	// Authenticator around it, for the same reason /static and the embed have
	// none: its answer does not depend on the principal. The top bar's glyph
	// posts it from a stranger's share page, and the handler writes one cookie
	// about this browser and reads no row — so a session lookup here would buy
	// nothing but a Resolver hit per press.
	mux.Handle("/settings/theme", page(postOnly(s.setTheme)))
	// The share page's OLD address (§20 erratum 248). Every link already sent
	// names it, so it answers a method-preserving permanent redirect to the id
	// at the root — see clipRedirect.
	mux.Handle("/clips/{id}", authed(page(getOnly(s.clipRedirect))))
	// The collection page (§9.4, §17 #9). It carries the SAME headers as the
	// share page — X-Frame-Options: DENY and the page CSP — because it is a
	// page like any other: nothing frames a list of links, and the owner
	// controls on it are as clickjackable as the share page's.
	mux.Handle("/c/{id}", authed(page(getOnly(s.collection))))
	mux.Handle("/embed/{id}", embed(getOnly(s.embedHandler)))
	mux.Handle("/static/{file...}", page(getOnly(s.staticFile)))
	// The crawler policy. Like /static/ it is the same bytes for everybody, so
	// it never costs the Resolver a session lookup.
	mux.Handle("/robots.txt", page(getOnly(s.robots)))
	// The web app manifest (ROADMAP #27, §20 erratum 137). The same again: one
	// static document, identical for every caller, no session lookup. It is
	// what makes the app installable on Android, and installing is what puts
	// Clippet in the phone's share sheet through the `share_target` it
	// declares.
	mux.Handle("/manifest.webmanifest", page(getOnly(s.manifest)))
	// The icon a client asks for WITHOUT being told to (§20 erratum 304). The
	// same again: one embedded file, identical for every caller, no session
	// lookup. Every page this app renders declares its own `data:` favicon, so
	// a browser looking at a page never comes here; the callers that do are the
	// ones that have only ever been given a URL — bookmark managers, some link
	// previewers, Safari's pinned-tab fallback — and this path answered the 404
	// page before.
	mux.Handle("/favicon.ico", page(getOnly(s.favicon)))
	// THE SHARE PAGE (§20 erratum 248): a clip id is a single segment at the
	// root of the origin. It is registered LAST of the page routes because it
	// is the least specific of them — every literal above wins on the Go 1.22
	// mux's specificity rule whatever the order, and putting it here says so —
	// and it serves exactly what `/clips/{id}` served: the same handler, the
	// same owner/stranger/crawler-hold/NoIndex rules, the same headers. A
	// single segment that is not an id is the 404 page, which is what `/`
	// answered for it before — see share for why that is not `getOnly`.
	mux.Handle("/{id}", authed(page(http.HandlerFunc(s.share))))
	mux.Handle("/", authed(page(http.HandlerFunc(s.notFound))))
	return stdmux
}

// patternMux is http.ServeMux plus a record of what was registered. The Go mux
// cannot be asked what patterns it holds, and the reserved-word test needs
// exactly that (see server.patterns).
type patternMux struct {
	m    *http.ServeMux
	seen *[]string
}

func (p patternMux) Handle(pattern string, h http.Handler) {
	*p.seen = append(*p.seen, pattern)
	p.m.Handle(pattern, h)
}

// share is `GET|HEAD /{id}`, the share page at the root (§20 erratum 248).
//
// It checks the ID BEFORE the method, which is the one place in this package
// where that order is deliberate and it is why this is not `getOnly(s.clip)`.
// `/{id}` matches EVERY single-segment path, so with the method checked first
// a `POST /nope` would answer 405 `Allow: GET, HEAD` — telling a scanner that
// every one-segment path on this site is a route, and changing the answer a
// path that is not a resource has always given. A segment that is not an id is
// therefore the 404 page for every method, exactly as the `/` catch-all
// answered it before; a segment that IS an id gets the ordinary 405, exactly
// as `/clips/{id}` did.
func (s *server) share(w http.ResponseWriter, r *http.Request) {
	if !ids.Valid(r.PathValue("id")) {
		s.notFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.clip(w, r)
}

// clipRedirect answers `/clips/{id}`, the share URL every link sent before §20
// erratum 248 moved the id to the root.
//
// **308 and not 301** because 308 is method-preserving: a 301 permits a client
// to turn a HEAD (or any other method) into a GET, and this route answers HEAD
// — the unfurlers and every uptime checker use it. Permanent, because the move
// is: Discord, X, Slack and Facebook all follow permanent redirects when they
// unfurl, so a card link that was pasted a year ago still resolves to the page
// with the tags on it.
//
// It reads no row. `ids.Valid` decides, and every id it accepts is redirected
// whether or not a clip exists — so the redirect cannot be used to probe which
// ids exist, and the answer to an unknown-but-valid id is the SAME 404 page
// the root form gives, one hop later. An id-shaped segment is therefore the
// only thing this route knows; anything else is the 404 page, never a redirect
// (a bad id must not become somebody's cached permanent redirect) and never a
// 500.
//
// The query string rides along: `/clips/{id}?autoplay=1` keeps its parameters,
// which the share page ignores today (DEPLOY §9) and might not tomorrow.
//
// It is PUBLICLY cacheable, unlike the page it points at, because the mapping
// is a pure function of the path — no session, no row, no clock — EXCEPT on a
// response that is carrying a `Set-Cookie`: this route runs under the Resolver
// (so a logged-in caller's 404 page still has its nav, §9's rule for the
// catch-all), and the Resolver's sliding session touch writes the refreshed
// cookie onto whatever response it is on. A shared cache storing THAT under a
// public rule would hand one person's session to the next caller. So the
// credential decides, exactly as it does on `/original` (§20 erratum 207
// amended): a bare redirect is public, one carrying a cookie is private and
// uncacheable.
func (s *server) clipRedirect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ids.Valid(id) {
		s.notFound(w, r)
		return
	}
	target := "/" + id
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	if w.Header().Get("Set-Cookie") == "" {
		w.Header().Set("Cache-Control", staticCacheControl)
	} else {
		setPrivate(w)
	}
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}

// getOnly answers 405 with Allow for anything but GET/HEAD. The Go 1.22 mux
// cannot do this itself once "/" is registered (§20 item 7).
func getOnly(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	})
}

// page is the data every base.html page shares; page structs embed it.
type page struct {
	DocTitle  string // <title>
	BodyClass string
	Viewer    *db.User // session user; drives the header nav
	NoIndex   bool
	Meta      []Meta
	Canonical string
	CSS, JS   string
	// OEmbed is the discovery link an oEmbed consumer follows (ROADMAP #18,
	// §20 erratum 152): `<link rel="alternate" type="application/json+oembed">`.
	// It is set on the share page and nowhere else, because /oembed describes
	// a clip and there is nothing else on this instance it could describe.
	// Empty renders no element at all.
	OEmbed string
	// Theme is the browser's own choice of look — "light", "dark" or "" for the
	// operating system's — read from the theme cookie (§20 erratum 242,
	// theme.go) and rendered as `data-theme` on <html>.
	Theme string
	// Product is the product's name in prose (productName), rendered as
	// `<body data-product>`. It is there so app.js can rebuild a document title
	// the way the server spelled it — a page title is the server's to word, and
	// the client carrying a second spelling of the name is how the two drift.
	Product string
	// Wordmark puts the product's wordmark alone, linking to `/`, in the header
	// slot of a page a SESSION-LESS visitor may see — the share page and the
	// collection page (§20 erratum 244; web/STYLE.md). A viewer with a session
	// has the full header anyway, so this only changes what a stranger gets:
	// the word `clippet` and a way to the site that served them, no nav, no
	// text beside it. False = a page without a session has no header at all.
	Wordmark bool
	// Badge is INSTANCE_BADGE (§20 erratum 285): the word an instance that is
	// not the production one wears. It is rendered as `data-badge` on <html> —
	// server-rendered with the first byte, exactly like `data-theme`, so the
	// mark paints grey rather than green before any script runs — and as the
	// small tag beside the wordmark. Empty renders NEITHER, and an unbadged
	// page is byte-identical to the one this app served before the variable
	// existed (TestUnbadgedPagesAreByteIdentical pins it).
	Badge string
	// Path is THIS page's own address, and it exists for one element: the
	// theme glyph's hidden `next` (§20 erratum 288), so a browser with no
	// JavaScript comes back to the page it pressed the glyph on instead of
	// landing in Settings. It is the request's path with no query — what a
	// `next` needs and no more — normalised by safeNext here and validated by
	// safeNext again in setTheme, because the value that arrives back is
	// whatever the client sent and not necessarily the one rendered.
	Path string

	// CardColor is the bar Discord paints down the left edge of this page's
	// unfurl (§20 erratum 287; web.CardColor carries the measurement): ONE
	// unqualified <meta name="theme-color"> rendered by base.html AFTER the
	// browser's light/dark pair, because Discord takes the LAST such tag and
	// ignores `media` while a browser takes the FIRST whose media matches. It is
	// set on the pages that CARRY a card and nowhere else — the share page, in
	// both its shapes — so the front door, the library, Settings, the auth pages
	// and the collection page emit the pair alone and are byte-identical to the
	// pages that shipped before it. Empty renders no tag at all.
	CardColor string

	// ComponentEmbed is the share page's own Discord card (§20 erratum 316,
	// CARD_COMPONENTS=on): the JSON BuildComponentEmbed renders, emitted by
	// base.html as the one `<script id="discord:component-embed">` data block in
	// <head>. Set on the share page alone and only when the switch is on; empty
	// renders no element, and every page is then byte-identical to the one
	// served before the switch existed.
	ComponentEmbed template.HTML
}

// docTitle is the `<title>` of a page: the badge word, then the title. An
// instance with no badge is unchanged; a badged one says which instance it is in
// the one place a browser shows on a tab strip of twenty tabs (§20 erratum 285).
func docTitle(badge, title string) string {
	if badge == "" {
		return title
	}
	return badge + " · " + title
}

func (s *server) basePage(r *http.Request, title string) page {
	badge := s.d.Cfg.InstanceBadge
	p := page{
		DocTitle: docTitle(badge, title),
		CSS:      samePath(s.d.URLs.Static("app.css")),
		JS:       samePath(s.d.URLs.Static("app.js")),
		Theme:    themeOf(r),
		Product:  productName,
		Badge:    badge,
		Path:     safeNext(r.URL.Path),
	}
	if u, ok := sessionUser(r); ok {
		p.Viewer = &u
	}
	return p
}

// render executes a base.html page into a buffer first so a template error
// becomes a clean 500 rather than a half-written page. Cache-Control
// defaults to no-cache (the principal-free embed); every base.html page
// pre-sets no-store — private + Vary: Cookie via setPrivate where the HTML
// carries principal-dependent content (§15) — or Retry-After as §9 requires.
func (s *server) render(w http.ResponseWriter, r *http.Request, name string, status int, data any) {
	t, ok := s.pages[name]
	if !ok {
		s.renderFailure(w, r, fmt.Errorf("unknown template %q", name))
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		s.renderFailure(w, r, err)
		return
	}
	s.writeHTML(w, r, status, buf.Bytes())
}

func (s *server) renderEmbed(w http.ResponseWriter, r *http.Request, data any) {
	var buf bytes.Buffer
	if err := s.embed.Execute(&buf, data); err != nil {
		s.renderFailure(w, r, err)
		return
	}
	s.writeHTML(w, r, http.StatusOK, buf.Bytes())
}

// renderFailure is the "cannot happen" path (a template bug): ERROR by the
// §12 doctrine, plain 500, never a 404.
func (s *server) renderFailure(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("web: render", "path", httpx.LogPath(r.URL.Path), "err", err)
	httpx.SetLogField(r, "render_error", err.Error())
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func (s *server) writeHTML(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// robotsTxt is the crawler policy (§20 errata 94, 248). A share link is a
// capability — "unlisted" means do not surface this, and DESIGN.md §9.1 makes
// the link the only authentication — but capability URLs reach crawlers all
// the time (toolbars, referrer chains, paste sites), and nothing in the app
// told one to stay away. The clip, collection and embed spaces are therefore
// closed to general indexing.
//
// Since the id moved to the ROOT (§20 erratum 248) the honest shape of that is
// `Disallow: /` plus an Allow for each of the four addresses that ARE meant to
// be found: the front door, the two auth pages and the static assets a
// rendered page needs. Listing `/clips/` while every real share link is
// `/{id}` would have closed nothing at all. It reads as a closed site because
// this IS a closed site: everything else here is somebody's private link.
//
// The four unfurlers are named individually with an EMPTY Disallow, which in
// robots.txt means "allow everything": a per-agent group replaces the `*`
// group outright for the agents it names, so without these Discord's, X's,
// Slack's and Facebook's crawlers would obey the block above and the product's
// whole point — a link that unfurls as a playable card — would stop working.
// The two mechanisms are also not interchangeable: `meta robots noindex` does
// not stop og: unfurling (the tags are read, the page is simply not indexed),
// and robots.txt does not stop it either for an agent allowed here. Both are
// needed, and neither is a security control — an id is still 71 bits (or 39,
// or 52: erratum 249).
const robotsTxt = `User-agent: *
Disallow: /
Allow: /$
Allow: /login
Allow: /register
Allow: /static/

User-agent: Discordbot
Disallow:

User-agent: Twitterbot
Disallow:

User-agent: facebookexternalhit
Disallow:

User-agent: Slackbot-LinkExpanding
Disallow:
`

// robots serves /robots.txt. It depends on no principal and no row, so it is
// public, cacheable and identical for every caller.
func (s *server) robots(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", staticCacheControl)
	h.Set("Content-Length", strconv.Itoa(len(robotsTxt)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(robotsTxt))
	}
}

// webManifest is the W3C web app manifest (ROADMAP #27, §20 erratum 137).
//
// Every URL in it is ROOT-RELATIVE, like every in-page resource this package
// renders (§9): the manifest is fetched from whatever host the browser is on,
// and hard-coding PUBLIC_BASE_URL would make an install under a second name —
// a LAN address beside the tunnel, say — point its share target at the other
// one. `scope` is the whole site, so the share target and the clip pages it
// redirects to are inside the installed app rather than kicking the user out
// to a browser tab.
//
// `share_target` is the point of the document. `method: POST` with
// `enctype: multipart/form-data` is the only form that can carry a FILE, and
// `files[0].name` is `file` — the same part name `clippet.sxcu` uses, so the
// share sheet and ShareX speak the same grammar. `title`/`text`/`url` are what
// Android's share intent sends alongside; the handler reads `title` and
// ignores the other two (§20 erratum 137).
//
// There is deliberately no `service_worker` and no offline story: nothing here
// caches, and a script that outlives the page would need its own answer to
// "what does it call" (§15's one-egress promise).
//
// `name`/`short_name` carry INSTANCE_BADGE when there is one (§20 erratum 285):
// an installed dev app and an installed production app are two icons on the same
// phone home screen, and the name under the icon is the only thing that tells
// them apart there — the tile itself is the same drawing. Everything else in the
// document is identical, badge or no badge, because an install is still this
// instance and its share target is still its own.
const webManifest = `{
  "name": "%[1]s",
  "short_name": "%[1]s",
  "description": "Upload a clip, get a link that plays inline.",
  "start_url": "/",
  "scope": "/",
  "display": "standalone",
  "background_color": "#ECEDEF",
  "theme_color": "#ECEDEF",
  "icons": [
    { "src": "/static/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any" },
    { "src": "/static/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any" }
  ],
  "share_target": {
    "action": "/api/share",
    "method": "POST",
    "enctype": "multipart/form-data",
    "params": {
      "title": "title",
      "text": "text",
      "url": "url",
      "files": [{ "name": "file", "accept": ["video/*", "audio/*"] }]
    }
  }
}
`

// manifest serves /manifest.webmanifest. Like robots.txt it depends on no
// principal and no row, so it is public, cacheable and identical for every
// caller. The media type is the registered one: Chrome accepts
// application/json too, but a manifest served as JSON is a manifest an
// operator will one day debug for an hour.
func (s *server) manifest(w http.ResponseWriter, r *http.Request) {
	doc := manifestJSON(s.d.Cfg.InstanceBadge)
	h := w.Header()
	h.Set("Content-Type", "application/manifest+json; charset=utf-8")
	h.Set("Cache-Control", staticCacheControl)
	h.Set("Content-Length", strconv.Itoa(len(doc)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(doc))
	}
}

// manifestJSON renders the manifest for this instance's badge. The name is the
// product in PROSE (`Clippet dev`, not `clippet dev`): it is a label under an
// icon, not the wordmark and not the site line Discord draws — §20 erratum 241's
// distinction, applied to the fourth place the product is named. The badge's own
// charset (lowercase, digits, `-`) needs no JSON escaping, which is why one
// Sprintf is the whole renderer.
func manifestJSON(badge string) string {
	name := productName
	if badge != "" {
		name += " " + badge
	}
	return fmt.Sprintf(webManifest, name)
}

// faviconFile is the embedded name /favicon.ico answers with. It is a file of
// web/static like any other, so it already has an ETag and a media type from
// the boot walk — the route is a second address for it, not a second copy.
const faviconFile = "favicon.ico"

// faviconCacheControl is a DAY, where every other static asset gets an hour
// (staticCacheControl). The difference is the URL: a `/static/` URL a template
// renders carries `?v=<build id>`, so a replaced file is a new URL and the hour
// is only a floor on how long a stale one may be kept; `/favicon.ico` is a
// fixed path nobody writes into a page, so a query would buy nothing and an
// upgrade cannot bust it. The icon changes with a release and a release is not
// a daily event, so the trade is a day of staleness in exchange for a request
// every browser-shaped client would otherwise make on every visit.
const faviconCacheControl = "public, max-age=86400"

// favicon serves /favicon.ico (§20 erratum 304). Like robots.txt and the
// manifest it depends on no principal and no row, so it is public, cacheable
// and identical for every caller.
//
// It reuses the static handler's own table rather than reading or embedding a
// second copy: the ETag is the sha256 the boot walk took of these exact bytes
// (a strong validator, and a better one than the build id — two builds of the
// same icon share it), the media type is staticTypes' row, and ServeFileFS
// answers HEAD, Range and the 304 on If-None-Match against the ETag set above.
// A missing name is the 404 page, which can only happen if the embed pattern
// stopped matching the file — and then the page is the honest answer.
func (s *server) favicon(w http.ResponseWriter, r *http.Request) {
	etag, ok := s.static.etags[faviconFile]
	if !ok {
		s.notFound(w, r)
		return
	}
	h := w.Header()
	// Set exactly as staticFile sets it, including the `ok` — a Content-Type
	// key present but EMPTY is a header Go's ServeContent will not fill in, so
	// a missing row here would be worse than no row at all.
	if ct, ok := staticTypes[path.Ext(faviconFile)]; ok {
		h.Set("Content-Type", ct)
	}
	h.Set("Cache-Control", faviconCacheControl)
	h.Set("ETag", etag)
	http.ServeFileFS(w, r, s.static.fsys, faviconFile)
}

// notFound renders the true-404 page: noindex, no card tags (§9.1). The
// page still renders the session nav, so it gets the private headers too.
func (s *server) notFound(w http.ResponseWriter, r *http.Request) {
	if !s.resolved(w, r) {
		return
	}
	setPrivate(w)
	p := s.basePage(r, "Not found · "+productName)
	p.NoIndex = true
	s.render(w, r, "notfound", http.StatusNotFound, p)
}

// setPrivate marks a page that renders principal-dependent content (the
// owner's view count, the session nav) as uncacheable by any shared cache
// and keyed on the credential for one that stores anyway — the HTML twin of
// httpapi's privateNoStore (§15).
func setPrivate(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "private, no-store")
	h.Set("Vary", "Cookie")
}

// resolved rejects a request whose credential could not be checked: the
// Resolver records a DB error instead of rejecting (§14), and a page that
// trusts the resulting "anonymous" would redirect a logged-in user to
// /login or render the owner's page without its owner content. A backend
// failure answers as one: the 503 page.
func (s *server) resolved(w http.ResponseWriter, r *http.Request) bool {
	if err := auth.ResolveError(r.Context()); err != nil {
		s.unavailable(w, r, err)
		return false
	}
	return true
}

// unavailable renders the 503 page for any backend failure: Retry-After,
// no-store, no card tags — never a 404 (§9.1). The error annotates the
// access-log line (WARN for 5xx) rather than producing its own ERROR line.
func (s *server) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	httpx.SetLogField(r, "db_error", err.Error())
	p := s.basePage(r, "Temporarily unavailable · "+productName)
	p.NoIndex = true
	h := w.Header()
	h.Set("Retry-After", retryAfterSeconds)
	h.Set("Cache-Control", "no-store")
	s.render(w, r, "unavailable", http.StatusServiceUnavailable, p)
}

// backendFailure answers a failed DB read. A client that has already gone
// away (the request context is cancelled — serve.go applies no
// TimeoutHandler, so only the SSR child context can hit a deadline) gets
// nothing: there is nobody to deliver a 503 to, and the access-log line
// stays INFO with client_gone=true (§12: disconnects are expected events).
// Everything else, the SSR deadline included, is the 503 page.
func (s *server) backendFailure(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(r.Context().Err(), context.Canceled) {
		httpx.SetLogField(r, "client_gone", true)
		return
	}
	s.unavailable(w, r, err)
}

// ssrContext bounds a page HANDLER's DB reads by SSR_TIMEOUT (§9). It does
// not cover the auth Resolver's session lookup, which runs before the
// handler on the bare request context — accepted and recorded in §20
// erratum 45, shared with the `long` JSON routes.
func (s *server) ssrContext(r *http.Request) (context.Context, context.CancelFunc) {
	d := s.d.Cfg.SSRTimeout
	if d <= 0 {
		d = defaultSSRTimeout
	}
	return context.WithTimeout(r.Context(), d)
}

// lookupClip applies the §9.1 discipline shared by the share and embed
// pages. It has already written the response when ok is false.
func (s *server) lookupClip(w http.ResponseWriter, r *http.Request) (db.Clip, bool) {
	id := r.PathValue("id")
	if !ids.Valid(id) {
		s.notFound(w, r)
		return db.Clip{}, false
	}
	ctx, cancel := s.ssrContext(r)
	defer cancel()
	c, err := s.d.DB.GetClip(ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		s.notFound(w, r)
		return db.Clip{}, false
	case err != nil:
		s.backendFailure(w, r, err)
		return db.Clip{}, false
	case c.State == db.ClipDeleting:
		s.notFound(w, r)
		return db.Clip{}, false
	}
	return c, true
}

// clipView pairs the row with the caller: IsOwner only for a browser
// session whose user owns the clip (§20 errata 19 and 22). A Bearer token
// is an upload credential — the Resolver still turns it into a principal on
// this public page, but it must unlock neither the owner-private view count
// nor the owner controls, exactly as httpapi.clipDTO refuses it on
// GET /api/clips/{id}.
func (s *server) clipView(r *http.Request, c db.Clip) ClipView {
	v := ClipView{Clip: c, URLs: s.d.URLs}
	if u, ok := sessionUser(r); ok {
		v.Viewer = &u
		v.IsOwner = u.ID != "" && u.ID == c.OwnerID
	}
	return v
}

// sessionUser returns the cookie-authenticated user. Token principals never
// unlock the session-only pages (/ and /settings).
func sessionUser(r *http.Request) (db.User, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.Via != viaSession {
		return db.User{}, false
	}
	return p.User, true
}

// redirectToLogin sends a session-less caller to /login?next=<path>. next is
// always one of this package's own route paths, so it needs no escaping and
// the redirect reads exactly as §9 documents it (/login?next=/).
func redirectToLogin(w http.ResponseWriter, r *http.Request, next string) {
	redirectPrivate(w, r, "/login?next="+next)
}

// redirectPrivate is the 303 of a principal-dependent branch: /settings
// bounces a session-less caller to /login (`/` renders the front door instead
// since §20 erratum 246, with the same headers), /login and
// /register bounce a logged-in one to next, and the invalid-cursor bounce
// only exists inside a session. WHICH response a URL produces therefore
// depends on the cookie, so the redirect carries the same headers as the
// pages it stands in for (§20 errata 17 and 37) — otherwise a shared cache
// could store the anonymous bounce and loop a logged-in user through /login.
func redirectPrivate(w http.ResponseWriter, r *http.Request, target string) {
	setPrivate(w)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// safeNext keeps a post-login redirect target on this origin (§20 erratum
// 52). It admits only a single-slash-rooted path that carries no control
// character and no backslash — a browser's URL parser folds `\` to `/` for
// http(s), so `/\evil.example` is an authority, not a path — AND that
// net/http will hand back to the client verbatim.
//
// That last clause is the load-bearing one: for a target with no scheme and
// no host, http.Redirect writes path.Clean(path)+query rather than the string
// it was given, and path.Clean treats `\` as an ordinary path rune while
// collapsing `.` and `..` segments — so it can MANUFACTURE the authority
// prefix out of a value that never literally held it
// (path.Clean(`/./\/evil.example`) == `/\/evil.example`). Validating only the
// input therefore validates a string that is not the one sent. Requiring the
// value to be its own normal form closes that gap once, for every caller,
// instead of chasing the shapes Clean can synthesise.
func safeNext(v string) string {
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return "/"
	}
	for _, c := range v {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	p := v
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	clean := path.Clean(p)
	if strings.HasSuffix(p, "/") && !strings.HasSuffix(clean, "/") {
		clean += "/" // Redirect preserves a trailing slash; so do we.
	}
	if clean != p {
		return "/"
	}
	return v
}

// samePath reduces an absolute Builder URL to its same-origin path+query so
// in-page resources satisfy CSP 'self' whatever host the page is browsed
// under (the card tags keep the absolute form).
func samePath(abs string) string {
	u, err := url.Parse(abs)
	if err != nil {
		return abs
	}
	return u.RequestURI()
}

// formatDate, expiryText, formatDuration, humanBytes and humanDuration live
// in format.go — they are pure formatters with no server state, and the four
// that were here had a fifth stranded in pages.go.

// stateLabel is the status line for non-ready clips (§9.1/§9.2).
func stateLabel(state string) string {
	switch state {
	case db.ClipFailed:
		return statusFailed
	case db.ClipReady:
		return "Ready"
	default:
		return statusProcessing
	}
}

// ttlOption is one <select> entry of the TTL pickers (§8.2 wire values). The
// pickers' grammar is "Delete after …" (§20 erratum 244: the owner's point that it is
// not keep for never, it is keep forever or never delete), so every label completes that
// sentence: `1 day` … `90 days`, and the no-expiry choice is the word `never`,
// last, as the far end of the scale. The wire value is untouched: 0 = never.
type ttlOption struct{ Value, Label string }

var ttlOptions = []ttlOption{
	{"1", "1 day"},
	{"7", "7 days"},
	{"30", "30 days"},
	{"90", "90 days"},
	{"0", "never"},
}

// linkExamples is the hint under the Link control: one EXAMPLE of each id
// grammar, side by side, rather than a sentence describing them (§20 erratum
// 264: for compact and memorable, the owner asked for an example of each
// instead of a description). Each is a share link on THIS instance's own host with the scheme left
// off, so the row stays narrow and the reader sees the shape they will paste.
// Neither is a link and neither is a clip: they are the two shapes.
type linkExamples struct{ Compact, Memorable string }

// The two ids the hint spends. The compact one carries digits, which no
// memorable id can (a memorable word is letters only), so `ids.Kind` can never
// read the example as the other grammar; the memorable one is the three-word
// shape a listed clip is minted in. Both are constants and neither is real:
// nothing resolves them, and an id that HAS been minted must never be printed
// as an example of anything.
const (
	exampleCompactID   = "Xk3v9QpL2mZa"
	exampleMemorableID = "PurpleUpdateCharacter"
)

// linkExamplesOf renders the two of them on the configured public base.
func linkExamplesOf(u urls.Builder) linkExamples {
	return linkExamples{
		Compact:   u.PageShort(exampleCompactID),
		Memorable: u.PageShort(exampleMemorableID),
	}
}

// defaultTTLLabel describes DEFAULT_CLIP_TTL for the "server default" option.
func defaultTTLLabel(secs *int64) string {
	if secs == nil {
		return "never"
	}
	days := *secs / 86400
	if days == 1 {
		return "1 day"
	}
	return strconv.FormatInt(days, 10) + " days"
}
