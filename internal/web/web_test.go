package web

import (
	"context"
	"errors"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/cost"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/urls"
)

const testBase = "http://example.test:8481"

// stubAuth injects a fixed principal (the §18.1 pattern for owner-path tests).
type stubAuth struct{ p auth.Principal }

func (s stubAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), s.p)))
	})
}

// passAuth never sets a principal: the anonymous path, independent of the
// real Resolver's implementation.
type passAuth struct{}

func (passAuth) Middleware(next http.Handler) http.Handler { return next }

// errAuth simulates a Resolver DB failure while checking the credential:
// no principal, a recorded ResolveError (§14).
type errAuth struct{ err error }

func (e errAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.WithResolveError(r.Context(), e.err)))
	})
}

// countingAuth records how many requests reached the Authenticator.
type countingAuth struct{ n *int }

func (c countingAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*c.n++
		next.ServeHTTP(w, r)
	})
}

func session(u db.User) auth.Authenticator {
	return stubAuth{auth.Principal{User: u, Via: "session", SessionHash: "h"}}
}

// sessionWithHash is session() for a test that seeds real session ROWS and
// needs the page to recognise one of them as the caller's (§20 erratum 107).
func sessionWithHash(u db.User, hash string) auth.Authenticator {
	return stubAuth{auth.Principal{User: u, Via: "session", SessionHash: hash}}
}

func bearer(u db.User) auth.Authenticator {
	return stubAuth{auth.Principal{User: u, Via: "token", TokenID: "t", Scopes: []string{"upload"}}}
}

// fixture is a temp DB with one clip per state, all owned by owner except
// otherListed / otherUnlisted (owned by other).
type fixture struct {
	t                                           *testing.T
	db                                          *db.DB
	owner, other                                db.User
	ready, processing, failed, deleting, queued db.Clip
	otherListed, otherUnlisted                  db.Clip
	now                                         int64
	registration                                config.RegistrationMode
	defaultTTL                                  *int64
	ssrTimeout                                  time.Duration
	// libraryScope is LIBRARY_SCOPE (§20 erratum 113); "" is config.Load's
	// default, `shared`.
	libraryScope string
	// budget is the storage-budget holder the library page reads (ROADMAP
	// #24, §20 erratum 157). nil = no holder wired, which is what every test
	// that does not care about it gets.
	budget *httpx.StorageBudget
	// served is the per-day served-bytes counter the settings page's Storage
	// panel reads (ROADMAP #39, §20 erratum 270); nil = no counter, and the
	// panel is not rendered at all, which is what every test that does not care
	// about it gets. storagePrice/egressPrice are the two optional prices in
	// micros, nil = unset.
	served                    *cost.Counter
	storagePrice, egressPrice *int64
	// storagePricing is STORAGE_PRICING and budgetHard is STORAGE_BUDGET_HARD
	// (§20 erratum 277); "" and false are the shipped defaults.
	storagePricing string
	budgetHard     bool
	// dropFolderOwner is DROP_FOLDER_OWNER (§20 erratum 296); "" is the
	// shipped default and the Storage panel then has no drop row at all.
	dropFolderOwner string
	// mediaDelivery is MEDIA_DELIVERY and mediaPresignEndpoint the origin it
	// redirects to (§20 erratum 278); "" is the shipped `proxy`, where the
	// served row carries no qualifier and the CSP names no media origin.
	mediaDelivery, mediaPresignEndpoint string
	// instanceBadge is INSTANCE_BADGE (§20 erratum 285); "" is the production
	// instance and every instance before the variable existed, where no page
	// carries the attribute, the tag, the title prefix or the longer site name.
	instanceBadge string
	// discordLogin is DISCORD_LOGIN_CLIENT_ID + _SECRET (§20 erratum 302); the
	// zero value is the shipped OFF state, where /login carries no Discord
	// button and /settings no Discord panel.
	discordLogin config.DiscordLogin
	// cardComponents is CARD_COMPONENTS (§20 erratum 316); false is the shipped
	// default, where no page carries the component-embed data block.
	cardComponents bool
	// discordBot is the Discord bot configuration; its zero value is OFF. A managed
	// one (DISCORD_BOT_TOKEN=managed) draws the Clippet Auto URL kind (§20 erratum 324).
	discordBot config.DiscordBot
	// setupWizardOff is SETUP_WIZARD=off (§20 erratum 335 (d)); the zero value
	// is the shipped default, where /setup is the wizard.
	setupWizardOff bool
	// path is the SQLite file, for the one thing a fixture cannot do through the
	// db package: write a column that belongs to a worker's commit (tiers_test).
	path string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "clippet.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	f := &fixture{t: t, db: d, path: path, now: 1_700_000_000,
		registration: config.RegistrationOpen, ssrTimeout: 5 * time.Second}
	f.owner = f.user("carol")
	f.other = f.user("friend")

	// ClaimJob takes the oldest queued job, so every clip is driven to its
	// state right after insertion; the clip that stays queued comes last.
	var job db.Job
	f.ready, job = f.create(f.owner, "E2E std", "a <b>desc</b>", false, nil)
	f.claim(f.ready)
	sourceMS := int64(6000) // the original this rendition was made from
	ok, _, err := d.FinalizeTranscode(ctx, job.ID, f.ready.ID, db.ReadyFields{
		RenditionKey: "r", RenditionSize: 10, RenditionETag: "re",
		PosterKey: "p", PosterSize: 5, PosterETag: "pe", PosterWidth: 640, PosterHeight: 360,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		SourceDurationMS: &sourceMS,
	}, false, f.now)
	if err != nil || !ok {
		t.Fatalf("FinalizeTranscode: %v %v", ok, err)
	}
	f.ready = f.reload(f.ready.ID)

	ttl := int64(7 * 86400)
	f.processing, _ = f.create(f.owner, "Mid encode", "", false, &ttl)
	f.claim(f.processing)
	f.processing = f.reload(f.processing.ID)

	f.failed, _ = f.create(f.owner, "Broken upload", "", false, nil)
	job = f.claim(f.failed)
	if _, err := d.FailAttempt(ctx, job.ID, db.ClaimStamp(job), f.failed.ID, "ffmpeg exited 1: boom", db.FailPermanent, db.Limits{}, f.now); err != nil {
		t.Fatalf("FailAttempt: %v", err)
	}
	f.failed = f.reload(f.failed.ID)

	f.deleting, _ = f.create(f.owner, "Going away", "", false, nil)
	if err := d.MarkDeleting(ctx, f.deleting.ID, f.now); err != nil {
		t.Fatalf("MarkDeleting: %v", err)
	}
	f.deleting = f.reload(f.deleting.ID)

	f.otherListed, _ = f.create(f.other, "Friend public", "", false, nil)
	f.otherUnlisted, _ = f.create(f.other, "Friend secret", "", true, nil)
	f.queued, _ = f.create(f.owner, "Just uploaded", "", true, nil)
	return f
}

func (f *fixture) user(name string) db.User {
	f.t.Helper()
	u, err := f.db.CreateUser(context.Background(), ids.New(), name, "$2a$12$hash", f.now)
	if err != nil {
		f.t.Fatalf("CreateUser: %v", err)
	}
	return u
}

func (f *fixture) create(owner db.User, title, desc string, unlisted bool, ttl *int64) (db.Clip, db.Job) {
	f.t.Helper()
	id := ids.New()
	c, j, err := f.db.CreateClipWithJob(context.Background(), db.NewClip{
		ID: id, OwnerID: owner.ID, Title: title, Description: desc, Unlisted: unlisted,
		SourceKey: "clips/" + id + "/original/source.mp4", SourceFilename: "clip.mp4",
		SourceSize: 123, SourceContentType: "video/mp4", SourceETag: "etag", TTLSeconds: ttl, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("CreateClipWithJob: %v", err)
	}
	f.now++
	return c, j
}

func (f *fixture) claim(c db.Clip) db.Job {
	f.t.Helper()
	j, got, ok, err := f.db.ClaimJob(context.Background(), f.now)
	if err != nil || !ok || got.ID != c.ID {
		f.t.Fatalf("ClaimJob: got %s ok=%v err=%v, want %s", got.ID, ok, err, c.ID)
	}
	return j
}

func (f *fixture) reload(id string) db.Clip {
	f.t.Helper()
	c, err := f.db.GetClip(context.Background(), id)
	if err != nil {
		f.t.Fatalf("GetClip: %v", err)
	}
	return c
}

func (f *fixture) handler(a auth.Authenticator) http.Handler {
	return f.handlerBase(a, testBase)
}

// handlerBase builds the handler against an explicit PUBLIC_BASE_URL (the
// unfurl regression needs both an http and an https base).
func (f *fixture) handlerBase(a auth.Authenticator, base string) http.Handler {
	f.t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		f.t.Fatal(err)
	}
	h, err := New(Deps{
		DB: f.db, Auth: a, URLs: urls.New(u, "b1"),
		Cfg: config.Config{SSRTimeout: f.ssrTimeout, Registration: f.registration, DefaultTTLSeconds: f.defaultTTL,
			SessionCookie: "clippet_session", LibraryScope: f.libraryScope,
			StoragePriceMicros: f.storagePrice, EgressPriceMicros: f.egressPrice,
			StoragePricing: f.storagePricing, StorageBudgetHard: f.budgetHard,
			DropFolderOwner:     f.dropFolderOwner,
			MediaDelivery:       f.mediaDelivery,
			ResolvedMediaOrigin: f.mediaPresignEndpoint,
			InstanceBadge:       f.instanceBadge,
			DiscordLogin:        f.discordLogin,
			CardComponents:      f.cardComponents,
			DiscordBot:          f.discordBot,
			SetupWizardOff:      f.setupWizardOff,
			S3:                  config.S3Config{PresignEndpoint: f.mediaPresignEndpoint}},
		BuildID: "b1",
		Now:     func() time.Time { return time.Unix(f.now, 0) },
		Budget:  f.budget,
		Served:  f.served,
	})
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	return h
}

func do(h http.Handler, method, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func get(h http.Handler, path string) *httptest.ResponseRecorder { return do(h, http.MethodGet, path) }

func mustContain(t *testing.T, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(body, s) {
			t.Errorf("body lacks %q", s)
		}
	}
}

func mustNotContain(t *testing.T, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(body, s) {
			t.Errorf("body contains %q", s)
		}
	}
}

var (
	// A real attribute has a quote right after its '='. html/template
	// escapes quotes in user text to &#34; / &#39;, so a description that
	// contains style="x" or onclick="y" can never match these (§16.2
	// guards the templates, not the users' words).
	styleAttrRE = regexp.MustCompile(`(?i)\bstyle\s*=\s*["']`)
	onAttrRE    = regexp.MustCompile(`(?i)\son[a-z]+\s*=\s*["']`)
	scriptRE    = regexp.MustCompile(`(?is)<script\b[^>]*>(.*?)</script>`)
	// componentEmbedOpenTag is the exact element base.html renders for §20
	// erratum 316.
	componentEmbedOpenTag = `<script id="discord:component-embed" type="application/vnd.discord.component-embed+json">`
	scriptSrcRE           = regexp.MustCompile(`(?i)<script\s+src="/static/app\.js\?v=b1"\s+defer></script>`)
)

// assertNoInline is the §9/§15 CSP invariant: no style attributes, no
// <style>, no inline script bodies, no on* handlers.
func assertNoInline(t *testing.T, name, body string) {
	t.Helper()
	lower := strings.ToLower(body)
	if styleAttrRE.MatchString(body) {
		t.Errorf("%s: contains a style attribute", name)
	}
	if strings.Contains(lower, "<style") {
		t.Errorf("%s: contains <style", name)
	}
	if onAttrRE.MatchString(body) {
		t.Errorf("%s: contains an on*= handler", name)
	}
	scripts := scriptRE.FindAllStringSubmatch(body, -1)
	// The ONE body a page may carry is the share page's component embed (§20
	// erratum 316): a JSON data block of a non-JavaScript type, recognised by
	// its exact opening tag and nothing looser, at most once.
	dataBlocks := 0
	for _, m := range scripts {
		if strings.HasPrefix(m[0], componentEmbedOpenTag) {
			dataBlocks++
			continue
		}
		if strings.TrimSpace(m[1]) != "" {
			t.Errorf("%s: inline script body %q", name, m[1])
		}
	}
	if dataBlocks > 1 {
		t.Errorf("%s: %d component-embed data blocks, want at most one", name, dataBlocks)
	}
	if strings.Count(lower, "<script") != len(scripts) || !scriptSrcRE.MatchString(body) {
		t.Errorf("%s: expected exactly the external app.js script tags, got %d", name, strings.Count(lower, "<script"))
	}
}

func assertPageHeaders(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCache string) {
	t.Helper()
	h := rec.Header()
	if rec.Code != wantStatus {
		t.Errorf("status %d, want %d", rec.Code, wantStatus)
	}
	if ct := h.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type %q", ct)
	}
	if cc := h.Get("Cache-Control"); cc != wantCache {
		t.Errorf("Cache-Control %q, want %q", cc, wantCache)
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "strict-origin-when-cross-origin" {
		t.Errorf("missing §15 headers: %v", h)
	}
}

// assertPrivateRedirect: a 303 whose target depends on the session is as
// principal-dependent as the page it replaces, so it carries erratum 17's
// headers (§20 erratum 37). A shared cache that stored the anonymous bounce
// would loop a logged-in user through /login.
func assertPrivateRedirect(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("redirect to %q: Cache-Control %q, want %q", rec.Header().Get("Location"), cc, "private, no-store")
	}
	if v := rec.Header().Get("Vary"); v != "Cookie" {
		t.Errorf("redirect to %q: Vary %q, want %q", rec.Header().Get("Location"), v, "Cookie")
	}
}

func assertNonEmbedHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("Content-Security-Policy") != httpx.CSPPage("") {
		t.Errorf("non-embed page headers: %v", rec.Header())
	}
}

// ---- share page --------------------------------------------------------

func TestSharePageReadyAnonymous(t *testing.T) {
	f := newFixture(t)
	rec := get(f.handler(passAuth{}), "/"+f.ready.ID)
	// The share page renders principal-dependent content (owner views, nav):
	// private + no-store + Vary: Cookie, like the owner clip JSON (§15).
	assertPageHeaders(t, rec, 200, "private, no-store")
	if v := rec.Header().Get("Vary"); v != "Cookie" {
		t.Errorf("Vary = %q, want Cookie", v)
	}
	assertNonEmbedHeaders(t, rec)
	body := rec.Body.String()
	mustContain(t, body,
		`<title>E2E std · Clippet</title>`,
		`<h1>E2E std</h1>`,
		`a &lt;b&gt;desc&lt;/b&gt;`, // escaped in the body
		`<meta property="og:title" content="E2E std">`,
		`<meta property="og:video" content="`+testBase+`/api/clips/`+f.ready.ID+`/social.mp4">`,
		`<meta name="twitter:player" content="`+testBase+`/embed/`+f.ready.ID+`">`,
		`<meta name="twitter:card" content="player">`,
		`<meta property="og:image:width" content="640">`,
		`<link rel="canonical" href="`+testBase+`/`+f.ready.ID+`">`,
		`<video controls playsinline preload="metadata" poster="/api/clips/`+f.ready.ID+`/poster.jpg" src="/api/clips/`+f.ready.ID+`/video.mp4"`,
		`data-page-url="`+testBase+`/`+f.ready.ID+`"`,
		`<dd data-duration-ms="6000">1280×720 · 0:06</dd>`,
		`<dd data-expiry>kept forever</dd>`,
	)
	// The share row is PUBLIC (§20 erratum 98): a link to copy, the iframe
	// snippet that puts this clip on a forum or a wiki, and the clip itself as
	// a file. A download is not a call to action — it hands over the clip the
	// viewer came for — and there is no share target of any kind beside it.
	// The iframe is sized from the RENDITION (1280×720 here), not from a
	// fixed box, so the embed matches the clip's aspect.
	mustContain(t, body,
		`<button type="button" class="btn" data-copy-link>Copy link</button>`,
		`<button type="button" class="btn" data-copy-embed data-embed-code="&lt;iframe src=&#34;`+testBase+`/embed/`+f.ready.ID+
			`&#34; width=&#34;1280&#34; height=&#34;720&#34; frameborder=&#34;0&#34; allowfullscreen allow=&#34;autoplay&#34;&gt;&lt;/iframe&gt;">Copy embed</button>`,
		`<a class="btn" href="/api/clips/`+f.ready.ID+`/video.mp4?download=1" download>Download</a>`,
		`<span class="copied" data-copied hidden>copied</span>`,
	)
	// …and the OWNER's half of that row is not: the source file is behind the
	// owner gate on /original (§8.1), so a visitor is offered neither the link
	// nor the size of it, and gets no owner controls at all.
	mustNotContain(t, body, `data-views`, `data-owner-controls`, `data-original-link`, `/original`,
		`data-progress-url`, `data-copy-hint`, `data-owner-error`, `noindex`, `og:video:secure_url`)
	// The owner block's <noscript> line goes with it (§20 erratum 229): a visitor
	// has nothing on this page that needs a client, so a note about it would be
	// one more sentence saying nothing.
	mustNotContain(t, body, `<noscript>`)
	// The one brand a watch page shows (§20 erratum 244, the owner's amendment of
	// the no-CTA rule): the wordmark alone in the header slot, linking home.
	// No nav, no sign-up, no text beside it.
	mustContain(t, body, `<header class="top">`,
		`<a class="wordmark" href="/" aria-label="clippet">cli<span class="pp">pp</span>et</a>`)
	mustNotContain(t, body, `<nav>`, `data-logout`, `href="/settings"`, `Log in`, `Sign up`)
	// The Phase 4 #2 lifetime controls are the owner's alone: a visitor is
	// handed neither the keep-alive URL nor a way to change the lifetime.
	mustNotContain(t, body, `data-touch-url`, `data-keep-forever`, `data-copy-note`, `data-expires`)
	// And so is #5's trim: a visitor gets no cut controls and no trim URL.
	// (`data-duration-ms` on the facts <dd> is NOT in that list: it is the
	// same number the line already prints, and it is what lets a trim
	// re-render the duration in place — owner-only markup it is not.)
	mustNotContain(t, body, `data-trim`, `data-trim-url`, `data-trim-start`, `data-trim-end`,
		`data-trim-go`, `data-trim-status`, `data-source-duration-ms`, `data-trim-note`)
	// #6's preview GIF is owner-only too, and deliberately not rendered as an
	// <img> on a watch page: it is made on demand, and a visitor's page must
	// not pull megabytes nobody asked for (the URL itself stays public).
	mustNotContain(t, body, `data-gif`, `data-gif-url`, `data-gif-go`, `data-gif-img`, `preview.gif`)
	// And so is the re-upload (ROADMAP #31): a visitor is never offered a way
	// to change the file, and never even learns the route exists.
	mustNotContain(t, body, `data-replace`, `data-replace-url`, `data-replace-file`, `/replace`)
	assertNoInline(t, "clip ready anon", body)
}

func TestSharePageOwnerSeesViewsAndControls(t *testing.T) {
	f := newFixture(t)
	if err := f.db.IncrementViews(context.Background(), f.ready.ID); err != nil {
		t.Fatal(err)
	}
	rec := get(f.handler(session(f.owner)), "/"+f.ready.ID)
	assertPageHeaders(t, rec, 200, "private, no-store")
	body := rec.Body.String()
	if n := strings.Count(body, "data-views="); n != 1 {
		t.Fatalf("data-views occurrences = %d, want exactly 1", n)
	}
	mustContain(t, body, `<span data-views="1">1</span>`, `data-owner-controls`, `data-copy-link`, `data-edit`, `data-delete`, `name="ttl_days"`, `value="E2E std"`,
		`data-logout`) // the session shows the header nav
	// The N109 <noscript> treatment reaches this block too (§20 erratum 229):
	// ONE short line at the top of Manage, because the controls under it send
	// JSON to routes that take no form submission — and it names the exception
	// rather than over-claiming, since Replace really does post without a client.
	mustContain(t, body, `<noscript><p class="hint">Editing this clip needs JavaScript; replacing the video still works.</p></noscript>`)
	if n := strings.Count(body, "<noscript>"); n != 1 {
		t.Errorf("<noscript> occurrences on the owner's page = %d, want exactly 1", n)
	}
	mustNotContain(t, body, `data-retry`, `data-copy-hint`)
	// The owner gets everything a visitor gets PLUS the upload itself, with
	// its size beside the link so "download the original" is a decision rather
	// than a surprise (the fixture's source is 123 bytes).
	mustContain(t, body, `data-copy-embed`, `href="/api/clips/`+f.ready.ID+`/video.mp4?download=1" download`,
		`<a href="/api/clips/`+f.ready.ID+`/original?download=1" download data-original-link>Download original (123 bytes)</a>`)
	// f.ready is kept forever: the keep-alive URL is still rendered (the
	// lifetime can change without a reload) but there is no countdown, so
	// the page carries no data-expires and both lifetime hints ship hidden —
	// copying such a link touches nothing.
	mustContain(t, body, `data-touch-url="/api/clips/`+f.ready.ID+`/touch"`,
		`data-keep-forever hidden`, `data-copy-note hidden`, `<dd data-expiry>kept forever</dd>`)
	mustNotContain(t, body, `data-expires`)
	// #5's trim control: the route, the ORIGINAL's length as the bound of both
	// number inputs, the CURRENT window as their values, the sentence that
	// says which timeline those numbers are on, and a status line that starts
	// hidden. This clip has never been cut, so the window is the whole upload.
	mustContain(t, body, `data-trim data-trim-url="/api/clips/`+f.ready.ID+`/trim"`,
		`data-source-duration-ms="6000"`, `data-trim-start-ms="0"`, `data-trim-end-ms="6000"`,
		`max="6.0" step="0.1" value="0.0" data-trim-start`, `max="6.0" step="0.1" value="6.0" data-trim-end`,
		`data-trim-go`, `data-trim-status hidden`,
		`Seconds on the original upload (0:00–0:06); the cut is the whole of it.`)
	// No trim has ever run on this clip, so the page must NOT ask the browser
	// to rejoin one: `data-trim-live` is absent, and its absence is what keeps
	// GET …/trim (a 404 for a never-trimmed clip) off every owner page view.
	mustNotContain(t, body, `data-trim-live`)
	// #6's GIF control: both routes, a button, a download link and an <img>
	// that all start hidden — the preview does not exist until it is asked
	// for, and this clip has none.
	mustContain(t, body, `data-gif data-gif-url="/api/clips/`+f.ready.ID+`/preview.gif" data-gif-post-url="/api/clips/`+f.ready.ID+`/gif"`,
		`data-gif-go`, `data-gif-download download hidden`, `data-gif-status hidden`, `data-gif-img alt="" hidden`)
	mustNotContain(t, body, `data-gif-ready`)
	// The re-upload (ROADMAP #31, §20 erratum 135). It is a real <form> with a
	// real action, so it degrades to a plain POST without app.js, and it names
	// the file part `file` — the name both upload endpoints already take.
	mustContain(t, body,
		`data-replace data-replace-url="/api/clips/`+f.ready.ID+`/replace" action="/api/clips/`+f.ready.ID+`/replace"`,
		`method="post" enctype="multipart/form-data"`,
		`<input type="file" name="file"`, `data-replace-file`, `data-replace-go`, `data-replace-status hidden`)
	// Cancel ships HIDDEN: app.js reveals it for the upload only, so a browser
	// without JavaScript is never offered a control that does nothing, and an
	// owner is never shown one once the job is the queue's (N091).
	mustContain(t, body, `data-replace-cancel hidden>Cancel</button>`)
	// The owner block's fields carry a name and a server-agreeing maxlength
	// (N045, N048). The names are inert — app.js POSTs JSON — and exist so
	// every field has an id or a name; 329 is db.MaxTagsPerClip x
	// db.MaxTagRunes + 9 commas, the widest tag string the API accepts.
	mustContain(t, body, `name="tags" maxlength="329"`, `name="trim_start"`, `name="trim_end"`)
	mustNotContain(t, body, `maxlength="400"`)
	// The card tags never depend on the viewer.
	mustContain(t, body, `<meta name="twitter:card" content="player">`)
	assertNoInline(t, "clip ready owner", body)
}

// TestSharePageShowsAnExistingPreview: once a preview exists the page says
// so in the markup (data-gif-ready), so the owner sees it without a request
// to the API — and the anonymous page still shows no <img> at all.
func TestSharePageShowsAnExistingPreview(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	job, err := f.db.EnqueueGIF(ctx, f.ready.ID, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := f.db.ClaimMediaJob(ctx, f.now); err != nil || !ok {
		t.Fatalf("ClaimMediaJob: ok=%v err=%v", ok, err)
	}
	swap, err := f.db.SetGIF(ctx, job.ID, f.ready.ID, db.GIFFields{
		Key: "clips/x/renditions/preview-r8r8r8r8.gif", Size: 4096, ETag: "ge", Width: 480, Height: 270,
	}, *f.ready.RenditionETag, f.now)
	if err != nil || !swap.OK {
		t.Fatalf("SetGIF: %+v %v", swap, err)
	}

	body := get(f.handler(session(f.owner)), "/"+f.ready.ID).Body.String()
	mustContain(t, body, `data-gif-ready`, `data-gif-img alt="" hidden`)
	assertNoInline(t, "clip with a preview", body)

	anon := get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String()
	mustNotContain(t, anon, `data-gif`, `preview.gif`)
}

// TestSharePageTrimNeedsAReadyClipWithRoomToCut: the control is offered only
// where every window it could send would be accepted. A clip that is not
// ready has nothing to trim, and one at or under the API's 500 ms floor has
// no window left after the cut — rendering a button whose only possible
// answer is a 400 would be worse than rendering none.
func TestSharePageTrimNeedsAReadyClipWithRoomToCut(t *testing.T) {
	f := newFixture(t)
	h := f.handler(session(f.owner))
	for _, c := range []db.Clip{f.processing, f.queued, f.failed} {
		body := get(h, "/"+c.ID).Body.String()
		mustContain(t, body, `data-owner-controls`) // the rest of the owner block is there
		// Neither the trim nor the GIF nor the re-upload control is offered
		// for a clip with no rendition to work from: all three answer 409
		// not_ready, and a button whose only answer is an error is not one.
		mustNotContain(t, body, `data-trim`, `data-gif`, `data-replace`)
	}

	short, job := f.create(f.owner, "Blink", "", false, nil)
	// The fixture leaves one clip queued on purpose, so the oldest claimable
	// job is not ours: claim until it is (this fixture is this test's alone).
	for i := 0; i < 5; i++ {
		_, got, ok, err := f.db.ClaimJob(context.Background(), f.now)
		if err != nil || !ok {
			t.Fatalf("ClaimJob: ok=%v err=%v", ok, err)
		}
		if got.ID == short.ID {
			break
		}
	}
	ok, _, err := f.db.FinalizeTranscode(context.Background(), job.ID, short.ID, db.ReadyFields{
		RenditionKey: "r2", RenditionETag: "re2", PosterKey: "p2", PosterETag: "pe2", PosterWidth: 640, PosterHeight: 360,
		Width: 1280, Height: 720, DurationMS: 400, FPSNum: 30, FPSDen: 1,
	}, false, f.now)
	if err != nil || !ok {
		t.Fatalf("FinalizeTranscode: %v %v", ok, err)
	}
	body := get(h, "/"+short.ID).Body.String()
	mustContain(t, body, `data-owner-controls`, `<video`)
	mustNotContain(t, body, `data-trim`)
}

// The owner's own upload token is an upload credential, not a browser
// session (§20 errata 19 and 22): on the share page it unlocks nothing — no
// private view count, no owner controls, no header nav — exactly as the
// JSON route refuses it. The page is still the public share page.
func TestSharePageOwnerTokenIsNotOwner(t *testing.T) {
	f := newFixture(t)
	if err := f.db.IncrementViews(context.Background(), f.ready.ID); err != nil {
		t.Fatal(err)
	}
	rec := get(f.handler(bearer(f.owner)), "/"+f.ready.ID)
	assertPageHeaders(t, rec, 200, "private, no-store")
	body := rec.Body.String()
	// The public share row is still there (it is public), but nothing owner-
	// only is: no view count, no controls, and no link to the original.
	mustNotContain(t, body, `data-views`, `data-owner-controls`, `data-edit`, `data-delete`, `data-retry`, `data-logout`, `data-trim`,
		`data-original-link`, `/original`, `data-touch-url`, `data-keep-forever`, `data-copy-note`, `data-expires`)
	mustContain(t, body, `<h1>E2E std</h1>`, `<video`, `<meta name="twitter:card" content="player">`, `data-copy-link`, `data-copy-embed`)
	assertNoInline(t, "clip ready owner token", body)
}

func TestSharePageOtherUserIsNotOwner(t *testing.T) {
	f := newFixture(t)
	body := get(f.handler(session(f.other)), "/"+f.ready.ID).Body.String()
	mustNotContain(t, body, `data-views`, `data-owner-controls`, `data-trim`, `data-original-link`, `/original`,
		`data-touch-url`, `data-keep-forever`, `data-copy-note`, `data-expires`)
	// A logged-in stranger is a viewer like any other: the share row is theirs
	// too, and copying the link from it touches nothing (§20 erratum 62).
	mustContain(t, body, `<video`, `data-logout`, `data-copy-link`, `data-copy-embed`,
		`href="/api/clips/`+f.ready.ID+`/video.mp4?download=1" download`)
}

func TestSharePageProcessingAndQueued(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	for _, c := range []db.Clip{f.processing, f.queued} {
		rec := get(h, "/"+c.ID)
		assertPageHeaders(t, rec, 200, "private, no-store")
		body := rec.Body.String()
		mustContain(t, body,
			`<h1>`+c.Title+`</h1>`,
			`<meta name="twitter:card" content="summary_large_image">`,
			`<meta property="og:image" content="`+testBase+`/static/card-placeholder.png?v=b1">`,
			`<meta property="og:image:width" content="1200">`,
			`data-progress-url="/api/clips/`+c.ID+`/progress"`,
			`data-progress-bar`,
			`data-status>Processing…</p>`,
		)
		// A visitor gets no share row at all while a clip encodes: there is no
		// rendition to download and no player to embed yet, and the whole row
		// is about a clip that exists (§20 erratum 98).
		mustNotContain(t, body, `<video`, `og:video`, `twitter:player`, `data-views`, `data-copy-link`, `data-copy-hint`,
			`data-copy-embed`, `video.mp4?download=1`, `data-original-link`, `data-quality`)
		assertNoInline(t, "clip "+c.State, body)
		// The owner gets the copy button plus the "plain link until ready"
		// hint — and the original, which is durable from the 201 on, so a
		// clip that is still encoding can already hand its source back.
		owner := get(f.handler(session(f.owner)), "/"+c.ID).Body.String()
		mustContain(t, owner, `data-owner-controls`, `data-copy-link`, `data-copy-hint`, `data-progress-bar`,
			`<a href="/api/clips/`+c.ID+`/original?download=1" download data-original-link>Download original (123 bytes)</a>`)
		// …but nothing that needs a rendition: it does not exist yet. The
		// `?download=1` on the ORIGINAL link is not one of those — /original is a
		// public player URL for a playable upload now, so the owner's save asks
		// for the attachment explicitly (§20 errata 98, 207) — so the negative is
		// about the RENDITION's download and the quality selector.
		mustNotContain(t, owner, `data-copy-embed`, `video.mp4?download=1`, `data-quality`)
		assertNoInline(t, "clip "+c.State+" owner", owner)
	}
	if body := get(h, "/"+f.processing.ID).Body.String(); !strings.Contains(body, "2023-11-21") {
		t.Errorf("expiry date missing: %s", body)
	}
	// f.processing has a 7-day TTL, so its owner gets the live lifetime
	// controls: a countdown to reset (data-expires, which is what makes the
	// copy button touch) and a visible one-click "keep forever". A clip is
	// touchable in every state — its TTL runs while it encodes.
	owner := get(f.handler(session(f.owner)), "/"+f.processing.ID).Body.String()
	mustContain(t, owner, `data-owner-controls data-touch-url="/api/clips/`+f.processing.ID+`/touch" data-expires`,
		`<button type="button" class="linkish" data-keep-forever>Keep forever</button>`,
		`<span class="hint" data-copy-note>`)
	mustNotContain(t, owner, `data-keep-forever hidden`, `data-copy-note hidden`)
}

// TestSharePageProcessingKeepsTheOwnerBlock pins the shape the client's
// ready-transition depends on (§20 erratum 95). The owner block does NOT wait
// for the encode — none of these fields describes the rendition — so an owner
// who clicks through from the upload can title, describe and tag a clip while
// it is still being made. That is the whole reason app.js may not reload the
// page out from under them when the encode lands: the poller checks every
// control here against the value the server rendered it with, and offers a
// link instead of reloading when any of them has been typed in.
//
// If a later change ever hides this block while processing, THIS test fails
// first — which is the point: it must not be possible to "fix" the lost-typing
// bug by taking the typing away.
func TestSharePageProcessingKeepsTheOwnerBlock(t *testing.T) {
	f := newFixture(t)
	for _, c := range []db.Clip{f.processing, f.queued} {
		body := get(f.handler(session(f.owner)), "/"+c.ID).Body.String()
		mustContain(t, body,
			// the poller runs: this page WILL see the encode finish
			`data-progress-url="/api/clips/`+c.ID+`/progress"`,
			// …and the fields it must not throw away, every one of them
			// carrying the server-rendered default the check compares against
			`data-owner-controls`,
			`<form class="panel" data-edit>`,
			`<input type="text" name="title" maxlength="200" value="`+c.Title+`" id="o-title" required>`,
			`<textarea name="description" maxlength="2000" rows="2" id="o-desc">`,
			`<select name="ttl_days" id="o-ttl">`,
			`<input type="checkbox" name="unlisted"`,
			`data-tags-edit data-api-url="/api/clips/`+c.ID+`"`,
			`data-tags-input`,
		)
		// The player and the on-demand extras are the parts that DO wait.
		mustNotContain(t, body, `<video`, `data-gif`, `data-trim`)
		assertNoInline(t, "processing owner block "+c.State, body)
	}
}

// ownerSelects returns the `<select …>…</select>` blocks of the share page's
// owner section, with the section itself as the search window (the page has
// no other select, and pinning the window keeps the assertion honest if one
// is ever added outside it).
func ownerSelects(t *testing.T, body string) []string {
	t.Helper()
	i := strings.Index(body, "data-owner-controls")
	if i < 0 {
		t.Fatal("no owner block on the page")
	}
	rest := body[i:]
	if j := strings.Index(rest, "</section>"); j >= 0 {
		rest = rest[:j]
	}
	var out []string
	for {
		a := strings.Index(rest, "<select")
		if a < 0 {
			return out
		}
		rest = rest[a:]
		b := strings.Index(rest, "</select>")
		if b < 0 {
			t.Fatalf("unterminated <select> in the owner block: %.80s", rest)
		}
		out = append(out, rest[:b+len("</select>")])
		rest = rest[b:]
	}
}

// TestSharePageOwnerSelectHasNoDefault pins the markup precondition the
// client's dirty check depends on (§20 erratum 95(b)).
//
// No `<option>` in the owner block carries a `selected` attribute — the TTL
// select's first option ("no change") is the no-op and is deliberately not
// marked — and a single-select must still have ONE option selected, so the
// browser selects index 0 with `defaultSelected === false`. A dirty check
// written as `option.selected !== option.defaultSelected` is therefore TRUE
// on a page nobody has touched, and the ready-transition reload never fires
// for an owner. app.js compares `selectedIndex` against the index of the
// option a browser would select instead; this test is the other half of that
// rule, so adding a `selected` attribute here — which would be fine on its
// own — cannot silently change what the client compares against.
//
// `disabled` is pinned for the same reason (§20 erratum 103): with no
// `selected` attribute anywhere, a browser selects the first option that is
// NOT disabled, so a disabled option 0 would move the default to index 1.
// app.js implements that rule now, but the two halves are only in agreement
// while this markup is what the rule was written against.
func TestSharePageOwnerSelectHasNoDefault(t *testing.T) {
	f := newFixture(t)
	for _, c := range []db.Clip{f.processing, f.ready} {
		body := get(f.handler(session(f.owner)), "/"+c.ID).Body.String()
		sels := ownerSelects(t, body)
		if len(sels) == 0 {
			t.Fatalf("%s: the owner block renders no <select>; erratum 95(b)'s rule has no subject", c.State)
		}
		for _, s := range sels {
			if !strings.Contains(s, `name="ttl_days"`) {
				t.Errorf("%s: an unexpected owner <select>: %s", c.State, s)
			}
			if strings.Contains(s, "selected") {
				t.Errorf("%s: an owner <select> option carries a selected attribute; "+
					"app.js's dirty rule assumes none does: %s", c.State, s)
			}
			if strings.Contains(s, "multiple") {
				t.Errorf("%s: an owner <select> is `multiple`; the dirty rule's other branch applies: %s", c.State, s)
			}
			if strings.Contains(s, "disabled") {
				t.Errorf("%s: an owner <select> option is disabled; the browser's default then moves off index 0 "+
					"and app.js's fallback is what has to agree with it: %s", c.State, s)
			}
		}
	}
}

// TestSharePageFailed: the failure REASON is owner-only on the page.
// internal/worker's reason() prefixes the internal stage name onto ffmpeg's
// stderr, and a friend arriving from a Discord link can neither act on that
// nor understand it — they get one plain sentence. The DTO is unchanged
// (§8.2 exposes failure_reason to any clip-id holder on purpose); what is
// split here is the RENDERING, not the data.
func TestSharePageFailed(t *testing.T) {
	f := newFixture(t)
	body := get(f.handler(passAuth{}), "/"+f.failed.ID).Body.String()
	mustContain(t, body, `data-status>This clip could not be processed.</p>`,
		`<meta property="og:description" content="Processing failed">`, `summary_large_image`)
	mustNotContain(t, body, `ffmpeg`, `boom`, `Processing failed:`,
		`<video`, `data-progress-url`, `data-retry`, `data-copy-link`, `data-copy-embed`, `download=1`, `/original`,
		`data-quality`)
	assertNoInline(t, "clip failed anon", body)
	// A logged-in stranger is not the owner either.
	other := get(f.handler(session(f.other)), "/"+f.failed.ID).Body.String()
	mustNotContain(t, other, `ffmpeg`, `boom`)
	// The owner keeps the technical line — they are the one who can act on it,
	// with the operator — and the retry button beside the copy button rather
	// than in the danger zone: retrying is the opposite of deleting.
	owner := get(f.handler(session(f.owner)), "/"+f.failed.ID).Body.String()
	mustContain(t, owner, `Processing failed: ffmpeg exited 1: boom`,
		`data-retry`, `data-delete`, `data-views="0"`, `data-copy-link`, `data-copy-hint`,
		// …and the source, which a failed clip still has: "give me my original
		// back" is exactly the request a failed encode produces (§8.1 serves
		// /original in every state but `deleting`).
		`<a href="/api/clips/`+f.failed.ID+`/original?download=1" download data-original-link>Download original (123 bytes)</a>`)
	if i, j := strings.Index(owner, `data-retry`), strings.Index(owner, `data-delete`); i < 0 || j < 0 || i > j {
		t.Errorf("Retry is not above Delete (retry at %d, delete at %d)", i, j)
	}
	assertNoInline(t, "clip failed owner", owner)
}

// User text that looks like inline markup is escaped by html/template
// everywhere it lands (title, description, meta content, form values) and
// must not trip the CSP guards, which match real attributes only.
func TestSharePageEscapesHostileText(t *testing.T) {
	f := newFixture(t)
	hostile := `style="color:red" onclick="x()" <style>p{}</style><script>alert(1)</script>`
	c, _ := f.create(f.owner, `style="t"`, hostile, false, nil)
	for name, a := range map[string]auth.Authenticator{"anon": passAuth{}, "owner": session(f.owner)} {
		body := get(f.handler(a), "/"+c.ID).Body.String()
		mustContain(t, body, `style=&#34;color:red&#34;`, `onclick=&#34;x()&#34;`, `&lt;script&gt;alert(1)&lt;/script&gt;`, `<title>style=&#34;t&#34; · Clippet</title>`)
		mustNotContain(t, body, hostile, `<script>alert`, `<style>p`)
		assertNoInline(t, "hostile "+name, body)
	}
	// The same text inside <meta content="…"> on a ready clip.
	if _, err := f.db.UpdateClipMeta(context.Background(), f.ready.ID, db.ClipPatch{Description: &hostile}, f.now); err != nil {
		t.Fatal(err)
	}
	body := get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String()
	// The duration leads the card description since §20 erratum 287; the owner's
	// own words are escaped exactly as they always were.
	mustContain(t, body, `<meta property="og:description" content="0:06 · style=&#34;color:red&#34; onclick=&#34;x()&#34; &lt;style&gt;p{}&lt;/style&gt;&lt;script&gt;alert(1)&lt;/script&gt;">`)
	assertNoInline(t, "hostile ready", body)
	assertNoInline(t, "hostile index", get(f.handler(session(f.owner)), "/").Body.String())
	assertNoInline(t, "hostile embed", get(f.handler(passAuth{}), "/embed/"+c.ID).Body.String())
	// The guards still catch the real thing.
	for _, bad := range []string{`<p style="color:red">`, `<p STYLE = 'x'>`, `<img onerror="x()">`, `<b onclick='y'>`} {
		if !styleAttrRE.MatchString(bad) && !onAttrRE.MatchString(bad) {
			t.Errorf("guard misses %q", bad)
		}
	}
}

func TestSharePageTrue404Cases(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	for _, tc := range []struct{ name, path string }{
		{"unknown id", "/" + ids.New()},
		{"deleting tombstone", "/" + f.deleting.ID},
		{"short id", "/abc"},
		{"long id", "/abcdefghijklm"},
		{"bad charset", "/abcdefghijk-"},
		{"trailing slash", "/" + f.ready.ID + "/"},
		{"unknown path", "/nope"},
		{"embed unknown", "/embed/" + ids.New()},
		{"embed deleting", "/embed/" + f.deleting.ID},
		{"embed bad id", "/embed/abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(h, tc.path)
			if rec.Code != 404 {
				t.Fatalf("status %d", rec.Code)
			}
			if rec.Header().Get("Retry-After") != "" {
				t.Error("404 carries Retry-After")
			}
			// The 404 page renders the session nav too: private, never shared.
			if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
				t.Errorf("404 Cache-Control %q", cc)
			}
			body := rec.Body.String()
			mustContain(t, body, `<meta name="robots" content="noindex">`, `Not found`)
			mustNotContain(t, body, `og:`, `twitter:`, `<video`)
			assertNoInline(t, "404 "+tc.name, body)
		})
	}
}

func TestSharePage503OnClosedDB(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/" + f.ready.ID, "/embed/" + f.ready.ID} {
		rec := get(h, path)
		if rec.Code != 503 {
			t.Fatalf("%s: status %d, want 503 (a backend failure must never be a 404)", path, rec.Code)
		}
		hd := rec.Header()
		if hd.Get("Retry-After") != "30" || hd.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: headers %v", path, hd)
		}
		body := rec.Body.String()
		mustNotContain(t, body, `og:`, `twitter:`, `<video`)
		mustContain(t, body, `Temporarily unavailable`)
		assertNoInline(t, "503 "+path, body)
	}
	// Session pages degrade the same way.
	rec := get(f.handler(session(f.owner)), "/")
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "30" {
		t.Errorf("index on closed db: %d %v", rec.Code, rec.Header())
	}
	rec = get(f.handler(session(f.owner)), "/settings")
	if rec.Code != 503 {
		t.Errorf("settings on closed db: %d", rec.Code)
	}
}

// A Resolver DB error must never masquerade as an auth state: the pages
// answer their 503, not a redirect to /login or an anonymous render of
// credentialed content (auth.ResolveError's contract; §8 conventions).
func TestPagesAnswer503OnResolveError(t *testing.T) {
	f := newFixture(t)
	h := f.handler(errAuth{errors.New("sqlite: disk I/O error")})
	for name, path := range map[string]string{
		"index":    "/",
		"settings": "/settings",
		"clip":     "/" + f.ready.ID,
		"404":      "/nope",
	} {
		t.Run(name, func(t *testing.T) {
			rec := get(h, path)
			if rec.Code != 503 {
				t.Fatalf("%s: %d, want 503 (never a redirect or an anonymous render)", path, rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Fatalf("%s: redirected to %q", path, loc)
			}
			hd := rec.Header()
			if hd.Get("Retry-After") != "30" || hd.Get("Cache-Control") != "no-store" {
				t.Errorf("%s: headers %v", path, hd)
			}
			body := rec.Body.String()
			mustContain(t, body, `Temporarily unavailable`)
			mustNotContain(t, body, `og:`, `twitter:`, `<video`)
		})
	}
	// The login page itself keeps rendering (a resolver error must not lock
	// the user out of the form).
	if rec := get(h, "/login"); rec.Code != 200 {
		t.Fatalf("login on resolve error: %d", rec.Code)
	}
}

// A client that disconnects during the SSR read gets no 503 (nobody is
// listening) and the access log stays INFO: §12 treats disconnects as
// expected events, not backend failures.
func TestBackendFailureClientGoneIsNot503(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		auth auth.Authenticator
		path string
	}{
		"clip":     {passAuth{}, "/" + f.ready.ID},
		"embed":    {passAuth{}, "/embed/" + f.ready.ID},
		"index":    {session(f.owner), "/"},
		"settings": {session(f.owner), "/settings"},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			f.handler(tc.auth).ServeHTTP(rec, req)
			if rec.Code == 503 || rec.Header().Get("Retry-After") != "" || rec.Body.Len() != 0 {
				t.Fatalf("client gone answered %d %v %q", rec.Code, rec.Header(), rec.Body.String())
			}
		})
	}
}

func TestSharePage503OnDeadline(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for _, path := range []string{"/" + f.ready.ID, "/embed/" + f.ready.ID} {
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 503 || rec.Header().Get("Retry-After") != "30" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %v", path, rec.Code, rec.Header())
		}
		mustNotContain(t, rec.Body.String(), `og:`)
	}
	// A zero SSRTimeout falls back to the 5s default rather than an instant deadline.
	f.ssrTimeout = 0
	if rec := get(f.handler(passAuth{}), "/"+f.ready.ID); rec.Code != 200 {
		t.Fatalf("default timeout: %d", rec.Code)
	}
}

// ---- embed ---------------------------------------------------------------

func TestEmbedReady(t *testing.T) {
	f := newFixture(t)
	rec := get(f.handler(session(f.owner)), "/embed/"+f.ready.ID)
	assertPageHeaders(t, rec, 200, "no-cache")
	h := rec.Header()
	if _, has := h["X-Frame-Options"]; has {
		t.Errorf("embed carries X-Frame-Options %q", h.Get("X-Frame-Options"))
	}
	if csp := h.Get("Content-Security-Policy"); csp != httpx.CSPEmbed("") || !strings.Contains(csp, "frame-ancestors *") {
		t.Errorf("embed CSP %q", csp)
	}
	body := rec.Body.String()
	mustContain(t, body, `<title>E2E std</title>`, `<body class="embed"`, `<video controls playsinline preload="metadata" poster="/api/clips/`+f.ready.ID+`/poster.jpg" src="/api/clips/`+f.ready.ID+`/video.mp4" data-view-url="/api/clips/`+f.ready.ID+`/view">`)
	// No view count ever (even for the owner), no chrome, no card tags — and
	// with no query the player behaves exactly as it always did. The ONE link
	// is the wordmark overlay (§20 erratum 246): to the share page, out of
	// the iframe, relative so it resolves against this origin.
	mustNotContain(t, body, `data-views`, `data-owner-controls`, `data-logout`, `<h1`, `og:`, `twitter:`, ` autoplay`, ` muted`, ` loop`)
	mustContain(t, body, embedMark(f.ready.ID))
	mustNotContain(t, body, `href="http`, `<header`, `<nav`)
	if n := strings.Count(body, "<a "); n != 1 {
		t.Errorf("the embed carries %d links, want exactly the overlay", n)
	}
	assertNoInline(t, "embed ready", body)
}

// embedMark is the wordmark overlay the embed carries (§20 erratum 246):
// the same live-text lockup as every page, linking to the clip's share page
// with target="_top" because an iframe cannot navigate itself out.
// embedMark is the overlay this FIXTURE renders (§20 erratum 287): testBase is
// `http://example.test:8481`, which is not clippet.gg, so the mark is the host
// as plain text in the same face -- no span, and therefore no living mark and no
// badge grey on such an instance. embedLockup is the other half of the rule.
func embedMark(id string) string {
	return `<a class="wordmark embed-mark" href="/` + id + `" target="_top" rel="noopener">example.test</a>`
}

// embedLockup is the overlay on the product's OWN domain and any subdomain of it:
// the wordmark intact, with the host's prefix and `.gg` in the same face and size
// and the green span still under the double p.
func embedLockup(id, prefix string) string {
	return `<a class="wordmark embed-mark" href="/` + id + `" target="_top" rel="noopener" aria-label="` +
		prefix + `clippet.gg">` + prefix + `cli<span class="pp">pp</span>et.gg</a>`
}

// TestEmbedPlayerParams is §17 #8: the three flags map to the <video>
// attributes of the same names, and autoplay implies muted because no
// browser will start an unmuted video by itself — `?autoplay=1` alone would
// otherwise render a player that silently does nothing.
func TestEmbedPlayerParams(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	for _, tc := range []struct {
		query   string
		want    []string
		absent  []string
		preload string
	}{
		{query: "", absent: []string{" autoplay", " muted", " loop"}, preload: "metadata"},
		{query: "?loop=1", want: []string{" loop"}, absent: []string{" autoplay", " muted"}, preload: "metadata"},
		{query: "?muted=1", want: []string{" muted"}, absent: []string{" autoplay", " loop"}, preload: "metadata"},
		{query: "?autoplay=1", want: []string{" autoplay", " muted"}, absent: []string{" loop"}, preload: "auto"},
		{query: "?autoplay=1&loop=1", want: []string{" autoplay", " muted", " loop"}, preload: "auto"},
		{query: "?autoplay&muted&loop", want: []string{" autoplay", " muted", " loop"}, preload: "auto"},
		{query: "?autoplay=0&muted=1&loop=nope", want: []string{" muted"}, absent: []string{" autoplay", " loop"}, preload: "metadata"},
	} {
		t.Run("embed"+tc.query, func(t *testing.T) {
			body := get(h, "/embed/"+f.ready.ID+tc.query).Body.String()
			mustContain(t, body, append(tc.want, `preload="`+tc.preload+`"`)...)
			if len(tc.absent) > 0 {
				mustNotContain(t, body, tc.absent...)
			}
			// Nothing else about the markup moves, and the CSP still has no
			// inline anything to allow.
			mustContain(t, body, `<video controls playsinline`, `src="/api/clips/`+f.ready.ID+`/video.mp4"`)
			assertNoInline(t, "embed"+tc.query, body)
		})
	}
	// The share page ignores the same query: it is the watch page, and an
	// autoplaying video is not what a person opening a link asked for.
	body := get(h, "/"+f.ready.ID+"?autoplay=1&loop=1").Body.String()
	mustNotContain(t, body, " autoplay", " muted", " loop")
}

func TestEmbedOptionsResolve(t *testing.T) {
	if got := (embedOptions{Autoplay: true}).resolve(); !got.Muted {
		t.Error("autoplay must imply muted")
	}
	if got := (embedOptions{Loop: true}).resolve(); got.Muted || got.Autoplay {
		t.Errorf("loop alone changed something else: %+v", got)
	}
	if got := (embedOptions{Autoplay: true}).Preload(); got != "auto" {
		t.Errorf("autoplay preload = %q, want auto", got)
	}
	if got := (embedOptions{}).Preload(); got != "metadata" {
		t.Errorf("default preload = %q, want metadata", got)
	}
}

func TestEmbedNonReadyStates(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	for _, tc := range []struct {
		clip db.Clip
		line string
	}{
		{f.processing, "Processing…"},
		{f.queued, "Processing…"},
		{f.failed, "Processing failed"},
	} {
		rec := get(h, "/embed/"+tc.clip.ID)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d (the resource exists → 200)", tc.clip.State, rec.Code)
		}
		body := rec.Body.String()
		// The status frame carries the overlay too (§20 erratum 246): a
		// poster-less black box is exactly where a viewer wonders whose
		// player it is.
		mustContain(t, body, `<p class="embed-status" data-status>`+tc.line+`</p>`, embedMark(tc.clip.ID))
		mustNotContain(t, body, `<video`, `data-views`)
		if tc.clip.State == db.ClipFailed {
			mustNotContain(t, body, `data-progress-url`, `boom`)
		} else {
			mustContain(t, body, `data-progress-url="/api/clips/`+tc.clip.ID+`/progress"`)
		}
		assertNoInline(t, "embed "+tc.clip.State, body)
	}
}

func TestParseEmbedOptions(t *testing.T) {
	for q, want := range map[string]embedOptions{
		"":                          {},
		"autoplay":                  {Autoplay: true},
		"autoplay=1&muted=true":     {Autoplay: true, Muted: true},
		"loop=0&muted=false":        {},
		"autoplay=yes&loop=on":      {Autoplay: true, Loop: true},
		"autoplay=nope&loop=&x=1":   {Loop: true},
		"muted=1&muted=0":           {Muted: true},
		"AUTOPLAY=1":                {},
		"autoplay=1&muted=1&loop=1": {Autoplay: true, Muted: true, Loop: true},
	} {
		v, err := url.ParseQuery(q)
		if err != nil {
			t.Fatal(err)
		}
		if got := parseEmbedOptions(v); got != want {
			t.Errorf("%q → %+v, want %+v", q, got, want)
		}
	}
}

// ---- HEAD / methods ------------------------------------------------------

func TestHeadRequests(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	for _, path := range []string{"/embed/" + f.ready.ID, "/" + f.ready.ID, "/login", "/static/app.js"} {
		rec := do(h, http.MethodHead, path)
		if rec.Code != 200 {
			t.Errorf("HEAD %s: %d", path, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("HEAD %s: body %d bytes", path, rec.Body.Len())
		}
		if rec.Header().Get("Content-Length") == "" || rec.Header().Get("Content-Length") == "0" {
			t.Errorf("HEAD %s: Content-Length %q", path, rec.Header().Get("Content-Length"))
		}
	}
	if rec := do(h, http.MethodHead, "/embed/"+f.ready.ID); rec.Header().Get("X-Frame-Options") != "" {
		t.Error("HEAD embed carries X-Frame-Options")
	}
}

// The Authenticator (a DB session lookup with the real Resolver) runs only
// where the page depends on the principal: never for static assets or the
// chrome-less embed.
func TestAuthenticatorSkipsStaticAndEmbed(t *testing.T) {
	f := newFixture(t)
	n := 0
	h := f.handler(countingAuth{&n})
	for _, path := range []string{"/static/app.js", "/static/app.css?v=b1", "/static/card-placeholder.png", "/embed/" + f.ready.ID, "/robots.txt"} {
		n = 0
		if rec := get(h, path); rec.Code != 200 || n != 0 {
			t.Errorf("%s: status %d, authenticator ran %d times (want 0)", path, rec.Code, n)
		}
	}
	for _, path := range []string{"/", "/login", "/register", "/settings", "/settings/tokens", "/" + f.ready.ID, "/nope"} {
		n = 0
		get(h, path)
		if n != 1 {
			t.Errorf("%s: authenticator ran %d times (want 1)", path, n)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	h := f.handler(session(f.owner))
	for _, path := range []string{"/", "/login", "/register", "/settings", "/settings/tokens", "/" + f.ready.ID, "/embed/" + f.ready.ID, "/static/app.js", "/robots.txt"} {
		for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
			rec := do(h, m, path)
			if rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("%s %s: %d Allow=%q", m, path, rec.Code, rec.Header().Get("Allow"))
			}
		}
	}
	if rec := do(h, http.MethodPost, "/nope"); rec.Code != 404 {
		t.Errorf("POST unknown path: %d", rec.Code)
	}
}

// ---- index -----------------------------------------------------------------

// TestIndexWithoutSession: `/` without a session is the FRONT DOOR (ROADMAP
// #36, §20 erratum 246) — a 200 with the wordmark, one line and a way in —
// where it was `303 /login?next=/` before. Every non-session principal gets
// it, a Bearer token included (a token is not a browser). /settings still
// bounces to /login exactly as it did.
func TestIndexWithoutSession(t *testing.T) {
	f := newFixture(t)
	for name, a := range map[string]auth.Authenticator{
		"no principal":        passAuth{},
		"skeleton resolver":   &auth.Resolver{DB: f.db, Cfg: config.Config{SessionCookie: "clippet_session"}, Now: time.Now},
		"token principal":     bearer(f.owner),
		"principal via empty": stubAuth{auth.Principal{User: f.owner}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := get(f.handler(a), "/")
			// The same headers the redirect it replaces carried: which page
			// `/` renders depends on the cookie, so private + Vary: Cookie.
			assertPageHeaders(t, rec, 200, "private, no-store")
			if v := rec.Header().Get("Vary"); v != "Cookie" {
				t.Errorf("Vary %q, want Cookie", v)
			}
			assertNonEmbedHeaders(t, rec)
			assertFrontPage(t, rec.Body.String(), true)
			rec = get(f.handler(a), "/settings")
			if rec.Code != 303 || rec.Header().Get("Location") != "/login?next=/settings" {
				t.Fatalf("settings: %d %q", rec.Code, rec.Header().Get("Location"))
			}
			assertPrivateRedirect(t, rec)
		})
	}
}

// assertFrontPage pins what the front door shows and, as tightly, what it
// does not: no header slot, no nav, no session control, no card tags, no
// noindex, no feature list — the wordmark, the line, Log in, and "Create an
// account" only while registration is open.
func assertFrontPage(t *testing.T, body string, open bool) {
	t.Helper()
	mustContain(t, body,
		`<title>Clippet</title>`,
		`<section class="auth front">`,
		`<span class="wordmark display" role="img" aria-label="clippet">cli<span class="pp">pp</span>et</span>`,
		`<p class="line">Upload a clip. Get a link.</p>`,
		`<a class="btn primary" href="/login">Log in</a>`)
	mustNotContain(t, body, `<header`, `<nav`, `data-logout`, `data-upload`, `data-clip-list`,
		`og:`, `twitter:`, `name="robots"`, `data-registration-closed`, `<h1`, `<img`)
	if open {
		mustContain(t, body, `<a href="/register">Create an account</a>`)
	} else {
		mustNotContain(t, body, `Create an account`, `href="/register`)
	}
	assertNoInline(t, "front page", body)
}

// TestFrontPage: the register link follows REGISTRATION as the login page's
// does; a session lands on the library exactly as before; HEAD works; and a
// resolver failure is still the 503 page rather than a front door shown to
// somebody who is logged in.
func TestFrontPage(t *testing.T) {
	f := newFixture(t)
	for _, mode := range []config.RegistrationMode{config.RegistrationOpen, config.RegistrationClosed, config.RegistrationFirst} {
		f.registration = mode
		body := get(f.handler(passAuth{}), "/").Body.String()
		// `first` is closed here: the fixture already has two users.
		assertFrontPage(t, body, mode == config.RegistrationOpen)
	}
	f.registration = config.RegistrationOpen

	rec := get(f.handler(session(f.owner)), "/")
	assertPageHeaders(t, rec, 200, "no-store")
	body := rec.Body.String()
	mustContain(t, body, `data-upload`, `data-clip-list`, `data-logout`, `<header class="top">`)
	mustNotContain(t, body, `class="auth front"`, `Upload a clip. Get a link.`)

	rec = do(f.handler(passAuth{}), http.MethodHead, "/")
	if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD /: %d, %d body bytes, Content-Length %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if rec := do(f.handler(passAuth{}), m, "/"); rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s / without a session: %d Allow=%q", m, rec.Code, rec.Header().Get("Allow"))
		}
	}
	if rec := get(f.handler(errAuth{errors.New("sqlite: disk I/O error")}), "/"); rec.Code != 503 {
		t.Errorf("a resolver failure at /: %d, want the 503 page", rec.Code)
	}
}

func TestIndexLibrary(t *testing.T) {
	f := newFixture(t)
	rec := get(f.handler(session(f.owner)), "/")
	assertPageHeaders(t, rec, 200, "no-store")
	assertNonEmbedHeaders(t, rec)
	body := rec.Body.String()
	mustContain(t, body,
		// The form's own action is the CAPTURE endpoint — the no-JS path,
		// whose grammar a plain browser form fits once the file field is last
		// — and data-upload-api is where app.js posts the §8.3 metadata-first
		// document instead (§20 erratum 139).
		`data-upload`, `action="/api/upload"`, `data-upload-api="/api/clips"`,
		`name="video"`, `name="ttl_days"`, `data-upload-cancel`, `data-upload-retry`,
		`<label for="u-ttl">Delete after</label>`,
		`<option value="">server default · never</option>`,
		`<option value="90">90 days</option>`, `<option value="0">never</option>`,
		`<label for="bulk-ttl">Delete after</label>`, `<option value="">no change</option>`,
		`name="unlisted"`, `data-upload-bar`, `data-clip-list`, `data-logout`, `href="/settings"`,
		// own ready clip: poster thumb + private view count
		`data-clip-id="`+f.ready.ID+`" data-state="ready"`,
		`<img src="/api/clips/`+f.ready.ID+`/poster.jpg"`,
		// own processing clip: badge + progress
		`data-clip-id="`+f.processing.ID+`" data-state="processing" data-progress-url="/api/clips/`+f.processing.ID+`/progress"`,
		`data-badge>processing</span>`,
		`data-expiry>expires 2023-11-21`,
		// own queued unlisted clip is listed with its tag
		// The unlisted marker is a FLAG, not a tag chip: chips are links into
		// the library filter and this one is not clickable at all.
		`data-clip-id="`+f.queued.ID+`"`, `<span class="flag">unlisted</span>`,
		// friend's listed clip appears with the owner name and no views
		`data-clip-id="`+f.otherListed.ID+`"`, `by friend`,
		`by carol`, `kept forever`,
	)
	mustNotContain(t, body, f.otherUnlisted.ID, f.deleting.ID, `Friend secret`)
	// The served markup is SINGLE-file: `multiple` is app.js's own, because a
	// browser without it would post a second file part the server never reads
	// (§20 errata 58, 139). The file field is also the LAST NAMED field in the
	// form, which is what makes the no-JS POST to /api/upload carry the text
	// fields first — the order that capture grammar requires.
	mustNotContain(t, body, `multiple`)
	assertFileFieldLast(t, body)
	// Views: exactly one span per own clip (ready, processing, failed, queued), none on the friend's.
	if n := strings.Count(body, `data-views="`); n != 4 {
		t.Errorf("data-views count = %d, want 4 (own clips only)", n)
	}
	if i := strings.Index(body, `data-clip-id="`+f.otherListed.ID+`"`); i >= 0 {
		row := body[i:]
		if j := strings.Index(row, "</li>"); j >= 0 {
			row = row[:j]
		}
		mustNotContain(t, row, `data-views`)
	}
	assertNoInline(t, "index", body)

	// The friend sees their own unlisted clip but not the owner's, and views only on theirs.
	body = get(f.handler(session(f.other)), "/").Body.String()
	mustContain(t, body, f.otherUnlisted.ID, `Friend secret`)
	mustNotContain(t, body, f.queued.ID)
	if n := strings.Count(body, `data-views="`); n != 2 {
		t.Errorf("friend data-views count = %d, want 2", n)
	}
}

// assertFileFieldLast pins the one property the no-JS upload turns on: EVERY
// named control of the upload form appears BEFORE the file input.
//
// `readCaptureForm` RETURNS at the file part (§20 erratum 58), so a field left
// after it is not "read late", it is never read at all — and the two that used
// to sit there were `ttl_days` and `unlisted`, which means a clip the owner had
// marked private came out listed in the shared library and announced to the
// Discord webhook. The old assertion compared the file field against `tags`
// only, which the broken markup satisfied: it must name every field.
func assertFileFieldLast(t *testing.T, body string) {
	t.Helper()
	form := body
	if i := strings.Index(form, `<form class="panel upload" data-upload`); i >= 0 {
		form = form[i:]
		if j := strings.Index(form, `</form>`); j >= 0 {
			form = form[:j]
		}
	} else {
		t.Fatalf("no upload form in the page")
	}
	file := strings.Index(form, `name="video"`)
	if file < 0 {
		t.Fatalf("the upload form has no file field")
	}
	for _, name := range []string{"title", "description", "tags", "ttl_days", "unlisted"} {
		at := strings.Index(form, `name="`+name+`"`)
		switch {
		case at < 0:
			t.Errorf("the upload form has no %s field", name)
		case at > file:
			t.Errorf("%s is at %d, after the file field at %d: a field after the file part is never read", name, at, file)
		}
	}
}

func TestIndexDefaultTTLLabelAndCursor(t *testing.T) {
	f := newFixture(t)
	ttl := int64(7 * 86400)
	f.defaultTTL = &ttl
	h := f.handler(session(f.owner))
	body := get(h, "/").Body.String()
	mustContain(t, body, `server default · 7 days`)
	// With an account default the no-op option names THAT, which is what an
	// upload that sends no lifetime gets (run 8): never the server's.
	one := int64(86400)
	oneDay, never := 1, 0
	for days, want := range map[*int]string{&oneDay: `<option value="">default · 1 day</option>`, &never: `<option value="">default · never</option>`} {
		if err := f.db.SetUserDefaults(context.Background(), f.owner.ID, db.UserDefaults{TTLDays: days}); err != nil {
			t.Fatal(err)
		}
		body := get(h, "/").Body.String()
		mustContain(t, body, want)
		if strings.Contains(body, `server default · 7 days`) {
			t.Errorf("an account default of %d day(s) still offers the server's lifetime as the default", *days)
		}
	}
	if got := defaultTTLLabel(&one); got != "1 day" {
		t.Errorf("1 day label = %q", got)
	}
	if rec := get(h, "/?cursor=garbage"); rec.Code != 303 || rec.Header().Get("Location") != "/" {
		t.Errorf("garbage cursor: %d %q", rec.Code, rec.Header().Get("Location"))
	} else {
		assertPrivateRedirect(t, rec)
	}
}

func TestIndexEmptyHintHiddenWhenOthersHaveClips(t *testing.T) {
	f := newFixture(t)
	lonely := f.user("lonely")
	body := get(f.handler(session(lonely)), "/").Body.String()
	// Others' listed clips still show; a brand-new user of a fresh server sees the empty hint.
	mustContain(t, body, `data-empty hidden="hidden"`)
}

// ---- login / register ----------------------------------------------------

// TestRegisterClosedNamesTheHost (§20 erratum 339; the owner's sentence, verbatim):
// the closed notice names the instance's own host from PUBLIC_BASE_URL — no
// scheme, the port kept when the URL carries one.
func TestRegisterClosedNamesTheHost(t *testing.T) {
	f := newFixture(t)
	f.registration = config.RegistrationClosed
	for _, tc := range []struct{ base, host string }{
		{"https://sweatycasuals.clippet.gg", "sweatycasuals.clippet.gg"},
		{"https://clips.example", "clips.example"},
		{"http://localhost:8481", "localhost:8481"},
	} {
		body := get(f.handlerBase(passAuth{}, tc.base), "/register").Body.String()
		mustContain(t, body, `<p class="notice" data-registration-closed>Registration is closed. Ask whoever runs `+tc.host+` for an account.</p>`)
		mustNotContain(t, body, `Ask the operator for an account.`)
		// …and since erratum 341 the two lines that still said "the operator":
		// /login's closed line, and /register's no-JavaScript hint (open only).
		body = get(f.handlerBase(passAuth{}, tc.base), "/login").Body.String()
		mustContain(t, body, `<p class="alt" data-registration-closed>Accounts are made by whoever runs `+tc.host+`.</p>`)
		mustNotContain(t, body, `the operator`)
	}
	f.registration = config.RegistrationOpen
	for _, tc := range []struct{ base, host string }{
		{"https://sweatycasuals.clippet.gg", "sweatycasuals.clippet.gg"},
		{"http://localhost:8481", "localhost:8481"},
	} {
		body := get(f.handlerBase(passAuth{}, tc.base), "/register").Body.String()
		mustContain(t, body, `<noscript><p class="hint">Creating an account needs JavaScript in this browser. Ask whoever runs `+tc.host+` to make one for you.</p></noscript>`)
		mustNotContain(t, body, `the operator`)
		// /settings' three CLI lines, the last to say "the operator" (the owner's
		// call, 2026-11-02): the password's two, and linking Discord.
		g := newFixture(t)
		g.discordLogin = testDiscordLogin(t)
		body = get(g.handlerBase(session(g.owner), tc.base), "/settings").Body.String()
		mustContain(t, body,
			`<noscript><p class="hint">Changing the password needs JavaScript in this browser. Whoever runs `+tc.host+` can change it from the CLI.</p></noscript>`,
			`<noscript><p class="hint">Linking needs JavaScript in this browser. Whoever runs `+tc.host+` can link it from the CLI.</p></noscript>`,
			`<span class="rs">no reset — whoever runs `+tc.host+` changes it from the CLI</span>`)
		mustNotContain(t, body, `The operator`, `the operator`)
	}
}

func TestLoginAndRegisterPages(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	rec := get(h, "/login")
	assertPageHeaders(t, rec, 200, "no-store")
	body := rec.Body.String()
	mustContain(t, body, `data-auth="login"`, `action="/api/auth/login"`, `name="next" value="/"`, `href="/register?next=%2F"`)
	mustNotContain(t, body, `data-logout`)
	assertNoInline(t, "login", body)

	// next is validated in the form net/http will actually send: Redirect
	// runs path.Clean over a scheme-less target, and Clean treats "\" as an
	// ordinary path rune, so "." / ".." segments can MANUFACTURE the
	// "/\"-authority prefix a literal-prefix check never sees (erratum 52).
	nexts := map[string]string{
		"/settings/tokens":        "/settings/tokens",
		"//evil.example":          "/",
		"/\\evil.example":         "/",
		"https://evil.example/":   "/",
		"/ok?x=1":                 "/ok?x=1",
		"/bad\nheader":            "/",
		"":                        "/",
		"/aZ3kQ9xB2mLp":           "/aZ3kQ9xB2mLp",
		"javascript:alert(1)":     "/",
		"/%2F%2Fevil.example/x/y": "/%2F%2Fevil.example/x/y",
		"/./\\/evil.example":      "/",
		"/a/../\\evil.example":    "/",
		"/.\\/evil.example":       "/",
		"/x/..//evil.example":     "/",
		"/ok?x=a\\b":              "/",
		"/x/../ok":                "/",
		"/ok/":                    "/ok/",
	}
	for q, want := range nexts {
		body := get(h, "/login?next="+url.QueryEscape(q)).Body.String()
		if !strings.Contains(body, `name="next" value="`+want+`"`) {
			t.Errorf("next=%q: hidden input not %q in %s", q, want, body)
		}
	}
	// The Location a logged-in caller is handed must stay on this origin
	// after net/http's own path.Clean rewriting — which the hidden input
	// cannot see, and which is where the /\ authority came from.
	logged := f.handler(session(f.owner))
	for q, want := range nexts {
		rec := get(logged, "/login?next="+url.QueryEscape(q))
		loc := rec.Header().Get("Location")
		if rec.Code != 303 || loc != want {
			t.Errorf("next=%q: logged-in /login %d Location=%q want %q", q, rec.Code, loc, want)
		}
		if strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, "/\\") {
			t.Errorf("next=%q: Location %q is an authority, not a path", q, loc)
		}
	}

	body = get(h, "/register").Body.String()
	mustContain(t, body, `data-auth="register"`, `action="/api/auth/register"`, `pattern="[A-Za-z0-9_]{3,32}"`, `minlength="8"`, `href="/login?next=%2F"`)
	mustNotContain(t, body, `data-registration-closed`)
	assertNoInline(t, "register", body)

	f.registration = config.RegistrationClosed
	body = get(f.handler(passAuth{}), "/register").Body.String()
	mustContain(t, body, `data-registration-closed`, `Registration is closed`)
	mustNotContain(t, body, `data-auth="register"`)
	assertNoInline(t, "register closed", body)

	// The login page reads the same flag. With registration closed the only
	// thing "Create an account" could reach is /register's own "closed"
	// notice, so the link is replaced by the sentence that is actually true.
	body = get(f.handler(passAuth{}), "/login").Body.String()
	mustContain(t, body, `data-auth="login"`, `data-registration-closed`,
		`Accounts are made by whoever runs example.test:8481.`)
	mustNotContain(t, body, `Create an account`, `href="/register`)
	assertNoInline(t, "login with registration closed", body)

	// Already logged in → straight to next.
	rec = get(f.handler(session(f.owner)), "/login?next=/settings/tokens")
	if rec.Code != 303 || rec.Header().Get("Location") != "/settings/tokens" {
		t.Errorf("logged-in /login: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	assertPrivateRedirect(t, rec)
	rec = get(f.handler(session(f.owner)), "/register?next=//evil")
	if rec.Code != 303 || rec.Header().Get("Location") != "/" {
		t.Errorf("logged-in /register: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	assertPrivateRedirect(t, rec)
}

// ---- settings page (ROADMAP #1; §20 errata 107, 110, 111) -------------------

func TestSettingsPage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	used := f.now - 60
	for _, tk := range []db.APIToken{
		{ID: ids.New(), UserID: f.owner.ID, Name: "ShareX <desk>", TokenHash: "tokenhash-one", Prefix: "abcdefgh", Scopes: []string{"upload"}, CreatedAt: f.now, LastUsedAt: &used},
		{ID: ids.New(), UserID: f.owner.ID, Name: "old", TokenHash: "tokenhash-two", Prefix: "zzzzzzzz", Scopes: []string{"upload"}, CreatedAt: f.now - 5},
		{ID: ids.New(), UserID: f.other.ID, Name: "friend token", TokenHash: "tokenhash-three", Prefix: "ffffffff", Scopes: []string{"upload"}, CreatedAt: f.now},
	} {
		if err := f.db.CreateToken(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	rec := get(f.handler(session(f.owner)), "/settings")
	// The page renders the owner's username, token prefixes and session list,
	// so it carries the private headers of every principal-dependent page.
	assertPageHeaders(t, rec, 200, "private, no-store")
	body := rec.Body.String()
	mustContain(t, body, `data-token-mint`, `action="/api/auth/tokens"`, `ShareX &lt;desk&gt;`, `<code>ck_abcdefgh…</code>`, `<code>ck_zzzzzzzz…</code>`, `data-revoke`, `data-new-token`, `<td class="m">never</td>`)
	// The clippet.sxcu affordance (§20 erratum 59): the anchor ships hidden
	// with its download name, and app.js fills its href from the ONE mint
	// response that carries the raw token — the page never renders a token
	// and the server never writes the file.
	mustContain(t, body, `data-sxcu`, `data-sxcu-download`, `download="clippet.sxcu"`, `/api/upload`)
	// The password form, with the autocomplete tokens a password manager needs
	// to offer the right thing in each box (ROADMAP #1).
	mustContain(t, body, `data-password-form`, `action="/api/auth/password"`,
		`autocomplete="current-password"`, `autocomplete="new-password"`)
	// The mint form's scope checkboxes (§20 erratum 111), upload pre-ticked.
	mustContain(t, body, `name="scopes" value="upload" checked`, `name="scopes" value="read"`)
	// The token NAME is a label for a token, not a value a browser profile has
	// or should offer, and it was the one field on this page Chrome's audit
	// flagged for a missing autocomplete attribute (N027).
	mustContain(t, body, `name="name" maxlength="64" autocomplete="off"`)
	// Both forms POST to JSON-only routes, so without JavaScript each is a
	// 405/415 dead end with no way back. They say so rather than leaving the
	// reader on a raw envelope; requireJSON is NOT relaxed (§20 erratum 178).
	if strings.Count(body, "<noscript>") != 2 {
		t.Errorf("want a <noscript> in each of the two /settings forms, got %d", strings.Count(body, "<noscript>"))
	}
	mustContain(t, body, `needs JavaScript in this browser`)
	mustNotContain(t, body, `friend token`, `ffffffff`, `tokenhash-`, `href="blob:`)
	if strings.Index(body, "abcdefgh") > strings.Index(body, "zzzzzzzz") {
		t.Error("tokens not newest first")
	}
	assertNoInline(t, "settings", body)
	// A session with no tokens gets the empty hint.
	body = get(f.handler(session(f.user("newbie"))), "/settings").Body.String()
	mustContain(t, body, `data-empty>No tokens yet.`)
}

// The session list is the page's own half of §20 erratum 107: it renders one
// row per live session, marks the caller's, and prints NO handle of any kind —
// the token hash is the credential's shadow and never leaves the DB layer.
func TestSettingsSessionList(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mine, theirs := "hash-of-this-browser", "hash-of-the-other-laptop"
	for _, sess := range []struct {
		hash, ua string
		created  int64
	}{
		{mine, "Mozilla/5.0 (this browser)", f.now},
		{theirs, "Mozilla/5.0 (the other laptop)", f.now - 600},
	} {
		if err := f.db.CreateSession(ctx, sess.hash, f.owner.ID, sess.created, f.now+3600, f.now+7200, sess.ua); err != nil {
			t.Fatal(err)
		}
	}
	// Another user's session must not appear on this page at all.
	if err := f.db.CreateSession(ctx, "hash-of-a-stranger", f.other.ID, f.now, f.now+3600, f.now+7200, "someone else"); err != nil {
		t.Fatal(err)
	}
	rec := get(f.handler(sessionWithHash(f.owner, mine)), "/settings")
	if rec.Code != 200 {
		t.Fatalf("settings: %d", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, `data-session-list`, `data-logout-others`,
		`Mozilla/5.0 (this browser)`, `Mozilla/5.0 (the other laptop)`, `this browser</span>`)
	// No hash, and nobody else's session.
	mustNotContain(t, body, mine, theirs, "hash-of-a-stranger", "someone else")
	if n := strings.Count(body, `<tr class="current">`); n != 1 {
		t.Errorf("exactly one row must be marked current, got %d", n)
	}
	assertNoInline(t, "settings sessions", body)
}

// /settings/tokens is the Phase 1 URL; it stays answering as a 303 to the
// tokens section rather than 404ing a documented, bookmarked path (§20
// erratum 110). The redirect is principal-dependent (a session-less caller
// lands on /login from the page it points at), so it carries the private
// headers every other bounce in this package carries.
func TestSettingsTokensRedirects(t *testing.T) {
	f := newFixture(t)
	rec := get(f.handler(session(f.owner)), "/settings/tokens")
	if rec.Code != 303 || rec.Header().Get("Location") != "/settings#tokens" {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Location"))
	}
	assertPrivateRedirect(t, rec)
	// Anonymously too: the shim does not need a session to say where the page
	// moved, and /settings then bounces to /login as it always did.
	rec = get(f.handler(passAuth{}), "/settings/tokens")
	if rec.Code != 303 || rec.Header().Get("Location") != "/settings#tokens" {
		t.Fatalf("anonymous: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// ---- static ----------------------------------------------------------------

func TestStaticAssets(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	for _, tc := range []struct{ name, ct string }{
		{"app.js", "text/javascript"},
		{"app.css", "text/css"},
		{"card-placeholder.png", "image/png"},
	} {
		rec := get(h, "/static/"+tc.name+"?v=b1")
		if rec.Code != 200 {
			t.Fatalf("%s: %d", tc.name, rec.Code)
		}
		hd := rec.Header()
		if hd.Get("Cache-Control") != "public, max-age=3600" {
			t.Errorf("%s: Cache-Control %q", tc.name, hd.Get("Cache-Control"))
		}
		if !strings.HasPrefix(hd.Get("Content-Type"), tc.ct) {
			t.Errorf("%s: Content-Type %q", tc.name, hd.Get("Content-Type"))
		}
		if hd.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: nosniff missing", tc.name)
		}
		etag := hd.Get("ETag")
		if !strings.HasPrefix(etag, `"`) || len(etag) < 10 {
			t.Errorf("%s: ETag %q", tc.name, etag)
		}
		if rec := do(h, http.MethodGet, "/static/"+tc.name, "If-None-Match", etag); rec.Code != 304 {
			t.Errorf("%s: If-None-Match → %d", tc.name, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", tc.name)
		}
	}
	// The link-out card's picture, pinned against the CONSTANTS rather than
	// against literals: internal/httpapi's oEmbed document builds its
	// thumbnail_url from the same three facts and never fetches it, so a
	// rename that left this test green would ship a thumbnail that 404s.
	img, err := png.Decode(get(h, "/static/"+urls.PlaceholderCard).Body)
	if err != nil {
		t.Fatalf("placeholder decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != urls.PlaceholderWidth || b.Dy() != urls.PlaceholderHeight {
		t.Fatalf("placeholder is %dx%d, want %dx%d", b.Dx(), b.Dy(), urls.PlaceholderWidth, urls.PlaceholderHeight)
	}
	if placeholderWidth != strconv.Itoa(urls.PlaceholderWidth) || placeholderHeight != strconv.Itoa(urls.PlaceholderHeight) {
		t.Fatalf("the og:image dimensions (%s x %s) do not match the file", placeholderWidth, placeholderHeight)
	}
	for _, path := range []string{"/static/nope.js", "/static/embed.go", "/static/", "/static/sub/app.js", "/static/../internal/web/web.go"} {
		rec := get(h, path)
		if rec.Code != 404 && rec.Code/100 != 3 { // ".." is cleaned by the mux with a redirect
			t.Errorf("%s: %d", path, rec.Code)
		}
		if rec.Code == 404 && !strings.Contains(rec.Body.String(), "Not found") {
			t.Errorf("%s: not the HTML 404 page", path)
		}
	}
	// The faces are versioned like every other static URL: app.css writes the
	// token, the handler substitutes the build id once at boot, and the ETag
	// is taken from the substituted bytes (so the conditional request above
	// still answers 304).
	css := get(h, "/static/app.css").Body.String()
	for _, want := range []string{
		`url("/static/fonts/spline-sans.woff2?v=b1")`,
		`url("/static/fonts/spline-sans-mono.woff2?v=b1")`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css does not carry %q — the faces are not cache-busted", want)
		}
	}
	if strings.Contains(css, staticVersionToken) {
		t.Errorf("app.css still carries %q as served", staticVersionToken)
	}

	js, _ := io.ReadAll(get(h, "/static/app.js").Body)
	for _, forbidden := range []string{`setAttribute("style"`, "innerHTML", "http://", "https://"} {
		if strings.Contains(string(js), forbidden) {
			t.Errorf("app.js contains %q", forbidden)
		}
	}
	// The library's page size is one number in four places; three are derived
	// from db.LibraryPageSize, and this is the fourth. app.js cannot import
	// Go, so the copy is pinned here instead: if the constant moves and this
	// literal does not, the poll would silently truncate the page it batches.
	if want := "const libraryPollMax = " + strconv.Itoa(db.LibraryPageSize) + ";"; !strings.Contains(string(js), want) {
		t.Errorf("app.js does not carry %q — the library page size has drifted between Go and JS", want)
	}
}

// A wrong current password is NOT a dead session (§20 erratum 108, amending
// erratum 95). `sessionEnded` used to read EVERY 401 as a dead credential,
// which was true while no page action could 401 for any other reason;
// PATCH /api/auth/password is the first route in the product that answers 401
// to a LIVE session, so the settings form's most likely failure would have
// rendered "your session ended — log in again" beside a link that bounces
// straight back to /settings. There is no JS engine in this suite, so the
// guard is asserted where it lives — the same shape as the forbidden-string
// scan above.
func TestSessionEndedIgnoresAWrongPassword(t *testing.T) {
	f := newFixture(t)
	raw, _ := io.ReadAll(get(f.handler(passAuth{}), "/static/app.js").Body)
	js := string(raw)
	const decl = "function sessionEnded("
	i := strings.Index(js, decl)
	if i < 0 {
		t.Fatal("app.js no longer defines sessionEnded")
	}
	body := js[i:]
	if j := strings.Index(body, "\n  }"); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "invalid_credentials") {
		t.Errorf("sessionEnded must not treat a 401 invalid_credentials as a dead session:\n%s", body)
	}
}

// ---- helpers ---------------------------------------------------------------

// TestFormatting covers the page helpers that are NOT pure number formatters
// (those moved to format_test.go with format.go): the URL trimmer, the facts
// line, the TTL label and the embed snippet.
func TestFormatting(t *testing.T) {
	if samePath("http://h:1/static/app.css?v=x") != "/static/app.css?v=x" || samePath("/rel") != "/rel" {
		t.Error("samePath")
	}
	six := int64(6000)
	for _, tc := range []struct {
		w, h *int
		d    *int64
		want string
	}{
		{ip(1280), ip(720), &six, "1280×720 · 0:06"},
		{ip(1280), ip(720), nil, "1280×720"},
		{nil, nil, &six, "0:06"},
		{ip(1280), nil, nil, ""},
	} {
		if got := videoFacts(db.Clip{Width: tc.w, Height: tc.h, DurationMS: tc.d}); got != tc.want {
			t.Errorf("videoFacts(%v,%v,%v) = %q, want %q", tc.w, tc.h, tc.d, got, tc.want)
		}
	}
	if defaultTTLLabel(nil) != "never" {
		t.Error("defaultTTLLabel(nil)")
	}
	// The embed snippet is sized from the rendition, so a 9:16 phone capture
	// gets a 9:16 box; a row with no measurements falls back to 640×360.
	const url = "https://x.test/embed/abc"
	for _, tc := range []struct {
		w, h *int
		want string
	}{
		{ip(1280), ip(720), `<iframe src="` + url + `" width="1280" height="720" frameborder="0" allowfullscreen allow="autoplay"></iframe>`},
		{ip(720), ip(1280), `<iframe src="` + url + `" width="720" height="1280" frameborder="0" allowfullscreen allow="autoplay"></iframe>`},
		{nil, nil, `<iframe src="` + url + `" width="640" height="360" frameborder="0" allowfullscreen allow="autoplay"></iframe>`},
		{ip(1280), nil, `<iframe src="` + url + `" width="640" height="360" frameborder="0" allowfullscreen allow="autoplay"></iframe>`},
		{ip(0), ip(0), `<iframe src="` + url + `" width="640" height="360" frameborder="0" allowfullscreen allow="autoplay"></iframe>`},
	} {
		if got := embedCode(url, tc.w, tc.h); got != tc.want {
			t.Errorf("embedCode(%v,%v) = %q, want %q", tc.w, tc.h, got, tc.want)
		}
	}
}

// A ready row that somehow lacks its measurements renders no "Video" fact
// at all rather than an empty " · " entry.
func TestSharePageOmitsVideoFactsWithoutDims(t *testing.T) {
	f := newFixture(t)
	s, err := newServer(Deps{DB: f.db, Auth: passAuth{}, URLs: builder(t, testBase)})
	if err != nil {
		t.Fatal(err)
	}
	c := f.ready
	c.Width, c.Height, c.DurationMS = nil, nil, nil
	req := httptest.NewRequest(http.MethodGet, "/"+c.ID, nil)
	data := s.clipModel(ClipView{Clip: c, URLs: s.d.URLs})
	data.page = s.basePage(req, "x")
	rec := httptest.NewRecorder()
	s.render(rec, req, "clip", http.StatusOK, data)
	body := rec.Body.String()
	mustContain(t, body, `<video`)
	mustNotContain(t, body, `<dt>Video</dt>`)
	// …and the embed snippet still names a box, because a snippet with no
	// width and height is a snippet nobody can paste.
	mustContain(t, body, `width=&#34;640&#34; height=&#34;360&#34;`)
	if with := get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String(); !strings.Contains(with, `<dt>Video</dt><dd data-duration-ms="6000">1280×720 · 0:06</dd>`) {
		t.Error("measured row lacks the Video fact")
	}
}

func TestNewRejectsMissingDeps(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Fatal("nil DB accepted")
	}
	f := newFixture(t)
	if _, err := New(Deps{DB: f.db}); err == nil {
		t.Fatal("nil Auth accepted")
	}
}

// TestSharePageTrimControlSpeaksTheOriginalsTimeline: after a cut, the inputs
// are pre-filled with the window the clip HAS and still bounded by the whole
// ORIGINAL — so the footage that cut removed is one edit away, not gone (§20
// erratum 68) — and the facts line carries the new duration for the JS to
// re-render in place.
func TestSharePageTrimControlSpeaksTheOriginalsTimeline(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	job, err := f.db.EnqueueTrim(ctx, f.ready.ID, db.MarshalTrimParams(db.TrimParams{StartMS: 4000, EndMS: 6000}), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := f.db.ClaimMediaJob(ctx, f.now); err != nil || !ok {
		t.Fatalf("ClaimMediaJob: ok=%v err=%v", ok, err)
	}
	if _, ok, _, err := f.db.SwapRendition(ctx, job.ID, f.ready.ID, db.ReadyFields{
		RenditionKey: "r2", RenditionETag: "re2", PosterKey: "p2", PosterETag: "pe2", PosterWidth: 640, PosterHeight: 360,
		Width: 1280, Height: 720, DurationMS: 2000, FPSNum: 30, FPSDen: 1,
	}, db.TrimParams{StartMS: 4000, EndMS: 6000}, f.now); err != nil || !ok {
		t.Fatalf("SwapRendition: ok=%v err=%v", ok, err)
	}

	body := get(f.handler(session(f.owner)), "/"+f.ready.ID).Body.String()
	mustContain(t, body,
		`data-source-duration-ms="6000"`, `data-trim-start-ms="4000"`, `data-trim-end-ms="6000"`,
		`max="6.0" step="0.1" value="4.0" data-trim-start`, `max="6.0" step="0.1" value="6.0" data-trim-end`,
		`Seconds on the original upload (0:00–0:06); the cut is 0:04–0:06.`,
		`<dd data-duration-ms="2000">1280×720 · 0:02</dd>`)
}

// TestSharePageTrimControlWithoutAMeasuredOriginal: a row from before
// migration 0003 that has already been cut has no source_duration_ms, and the
// control then offers NO maximum — exactly as the API stops bounding such a
// clip and lets the worker's probe of the original decide.
func TestSharePageTrimControlWithoutAMeasuredOriginal(t *testing.T) {
	f := newFixture(t)
	s, err := newServer(Deps{DB: f.db, Auth: passAuth{}, URLs: builder(t, testBase)})
	if err != nil {
		t.Fatal(err)
	}
	c := f.ready
	c.SourceDurationMS = nil
	two := int64(2000)
	c.DurationMS = &two
	req := httptest.NewRequest(http.MethodGet, "/"+c.ID, nil)
	data := s.clipModel(ClipView{Clip: c, URLs: s.d.URLs, IsOwner: true})
	data.page = s.basePage(req, "x")
	rec := httptest.NewRecorder()
	s.render(rec, req, "clip", http.StatusOK, data)
	body := rec.Body.String()
	mustContain(t, body, `data-trim `, `data-trim-start-ms="0"`, `data-trim-end-ms="2000"`,
		`Seconds on the original upload, not on this cut.`)
	mustNotContain(t, body, `max="`, `data-source-duration-ms`)
}

// TestSharePageAdvertisesALiveTrim is the other half: when a trim really IS
// queued or running, the control carries `data-trim-live` and the owner's
// page rejoins the poll. The server answers the question so the browser never
// has to ask it (§20 erratum 87).
func TestSharePageAdvertisesALiveTrim(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.db.EnqueueTrim(ctx, f.ready.ID, `{"start_ms":0,"end_ms":3000}`, 1000); err != nil {
		t.Fatal(err)
	}
	body := get(f.handler(session(f.owner)), "/"+f.ready.ID).Body.String()
	mustContain(t, body, `data-trim-live`)
	// It is owner-only, like the control it sits on.
	anon := get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String()
	mustNotContain(t, anon, `data-trim-live`, `data-trim-url`)
}
