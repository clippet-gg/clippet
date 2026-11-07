package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/notify"
)

// POST /in/auto/{token}, the Clippet Auto URL (§20 erratum 324).

const (
	afGuild = "333333333333333333"
	afCDN   = "https://cdn.discordapp.com/attachments/111111111111111111/222222222222222222/clip.mp4?ex=1&is=2&hm=3"
)

// newAutoFetch inserts a Clippet Auto row for u and returns its raw secret.
func (e *env) newAutoFetch(u db.User, scopes ...string) string {
	e.t.Helper()
	if len(scopes) == 0 {
		scopes = []string{auth.ScopeAutoFetch}
	}
	raw := ids.AutoFetchToken()
	err := e.db.CreateToken(context.Background(), db.APIToken{ID: ids.New(), UserID: u.ID, Name: "auto",
		TokenHash: auth.HashToken(raw), Prefix: auth.TokenPrefixOf(raw), Scopes: scopes, CreatedAt: e.now()})
	if err != nil {
		e.t.Fatal(err)
	}
	return raw
}

func (e *env) bind(guild string, u db.User) {
	e.t.Helper()
	if _, err := e.db.BindGuild(context.Background(), guild, u.ID, e.now()); err != nil {
		e.t.Fatal(err)
	}
}

// afBody is one instruction; mutate edits the map before it is encoded.
func afBody(mutate ...func(map[string]any)) map[string]any {
	m := map[string]any{"guild_id": afGuild, "url": afCDN, "filename": "clip.mp4", "content_type": "video/mp4", "size": 4000}
	for _, f := range mutate {
		f(m)
	}
	return m
}

func (e *env) autoFetch(raw string, body any, opts ...reqOpt) resp {
	e.t.Helper()
	return e.doJSON(http.MethodPost, "/in/auto/"+raw, body, opts...)
}

// afStub is Discord's CDN for a real notify.Bot: /attachments/… serves the
// video; a path containing "redirect" answers 302 to another of its own
// paths; one containing "slow" holds until released.
type afStub struct {
	srv     *httptest.Server
	mu      sync.Mutex
	hits    []string
	release chan struct{}
	once    sync.Once
	// hit is closed and replaced on every request (guarded by mu): waitHits
	// blocks on it instead of polling count().
	hit chan struct{}
}

// free lets every held request go (once; the cleanup calls it too).
func (s *afStub) free() { s.once.Do(func() { close(s.release) }) }

func newAFStub(t *testing.T, body []byte) *afStub {
	s := &afStub{release: make(chan struct{}), hit: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits = append(s.hits, r.URL.Path)
		close(s.hit)
		s.hit = make(chan struct{})
		s.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "redirect"):
			http.Redirect(w, r, "/attachments/elsewhere.mp4", http.StatusFound)
			return
		case strings.Contains(r.URL.Path, "slow"):
			<-s.release
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.srv.Close)
	// Registered after Close so it runs first: a held request must be let go before
	// the server is closed, or Close waits for it forever.
	t.Cleanup(s.free)
	return s
}

// waitHits blocks until the stub has been asked at least n times; the guard is
// a hang guard, never a budget.
func (s *afStub) waitHits(t *testing.T, n int) {
	t.Helper()
	guard := time.NewTimer(time.Minute)
	defer guard.Stop()
	for {
		s.mu.Lock()
		got, next := len(s.hits), s.hit
		s.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-next:
		case <-guard.C:
			t.Fatalf("the CDN stub was asked %d time(s), want %d", got, n)
		}
	}
}

func (s *afStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hits)
}

// afEnv is an env with a REAL notify.Bot whose allowlist is the production
// gate first and then points the CDN's host at the stub, as a test may.
func afEnv(t *testing.T, stub *afStub, mutate ...func(*Deps)) (*env, *notify.Bot) {
	host := strings.TrimPrefix(stub.srv.URL, "http://")
	var bot *notify.Bot
	e := newEnv(t, append([]func(*Deps){func(d *Deps) {
		bot = notify.NewBot(notify.BotDeps{
			AppID: "123456789012345678", Token: config.ManagedToken, Managed: true,
			URLs: d.URLs, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			APIHost: host, APIScheme: "http",
			AllowAttachment: func(raw string) (*url.URL, error) {
				u, err := config.ValidAttachmentURL(raw)
				if err != nil {
					return nil, err
				}
				u.Scheme, u.Host = "http", host
				return u, nil
			},
			MaxBytes: 1 << 20, CallTimeout: 2 * time.Second, FetchTimeout: 5 * time.Second, JobBudget: 10 * time.Second,
		})
		d.Bot = bot
	}}, mutate...)...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bot.Shutdown(ctx)
	})
	// Let any held CDN request go before the Shutdown above waits on its job.
	t.Cleanup(stub.free)
	return e, bot
}

func TestCleanScopesAutoFetch(t *testing.T) {
	for _, in := range [][]string{{"autofetch"}, {"autofetch", "autofetch"}} {
		if got, err := CleanScopes(in); err != nil || !reflect.DeepEqual(got, []string{"autofetch"}) {
			t.Errorf("CleanScopes(%v) = %v, %v", in, got, err)
		}
	}
	for _, bad := range [][]string{{"autofetch", "inbound"}, {"inbound", "autofetch"}, {"autofetch", "upload"},
		{"read", "autofetch"}, {"Autofetch"}, {"autofetch", ""}} {
		if got, err := CleanScopes(bad); err == nil {
			t.Errorf("CleanScopes(%v) = %v, want an error", bad, got)
		}
	}
	if auth.ValidScope(auth.ScopeAutoFetch) || slices.Contains(auth.Scopes, auth.ScopeAutoFetch) {
		t.Error("autofetch leaked into the Bearer scope set")
	}
	for _, s := range [][]string{{"autofetch", "upload"}, {"autofetch", "autofetch"}, {}} {
		if IsAutoFetchSet(s) {
			t.Errorf("IsAutoFetchSet(%v) = true; the set test is exact", s)
		}
	}
}

// The browser mint: `["autofetch"]` alone is a Clippet Auto URL — `ca_`, its
// url on PUBLIC_BASE_URL under a hostile Host, sxcu null.
func TestCreateTokenAutoFetch(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")
	r := e.doJSON(http.MethodPost, "/api/auth/tokens", map[string]any{"name": "Discord server", "scopes": []string{"autofetch"}},
		withCookie(cookie), func(r *http.Request) { r.Host = "evil.example" })
	if r.code != 201 {
		t.Fatalf("mint: %d %s", r.code, r.body)
	}
	raw := r.str("token")
	if !ids.IsAutoFetchToken(raw) || r.str("url") != testOrigin+"/in/auto/"+raw || r.json["sxcu"] != nil || !r.has("sxcu") {
		t.Fatalf("autofetch mint: %s", r.body)
	}
	if r.str("prefix") != raw[3:11] || !reflect.DeepEqual(r.json["scopes"], []any{"autofetch"}) {
		t.Fatalf("row: %s", r.body)
	}
	r = e.doJSON(http.MethodPost, "/api/auth/tokens", map[string]any{"name": "x", "scopes": []string{"autofetch", "inbound"}}, withCookie(cookie))
	if r.code != 400 || r.str("message") != ScopesRule {
		t.Fatalf("mixed: %d %s", r.code, r.body)
	}
}

// Every miss on BOTH path routes is POST /api/nope byte for byte, for three
// body framings, with a cookie and a Bearer token riding along: the wrong
// shape, the other route's secret, revoked, unknown, `ck_`, a deleted
// account's row and hand-written rows holding either path scope beside
// another. A `ca_` value is no Bearer credential, and GET on /in/auto/{x}
// answers one 405 for every x.
func TestAutoFetchMissIsAMissingRouteOnBothRoutes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	u, cookie := e.newUser("carol")
	revoked := e.newAutoFetch(u)
	toks, _ := e.db.ListTokens(ctx, u.ID)
	if err := e.db.RevokeToken(ctx, u.ID, toks[0].ID, e.now()); err != nil {
		t.Fatal(err)
	}
	doomed, _ := e.newUser("doomed")
	orphan := e.newAutoFetch(doomed)
	if _, err := e.db.PruneUsers(ctx, "doomed", e.now()+10); err != nil {
		t.Fatal(err)
	}
	bearer := e.newToken(u, "upload")
	liveInbound := e.newInbound(u)
	liveAuto := e.newAutoFetch(u)
	auto := map[string]string{
		"bad shape":             "ca_short",
		"too long":              ids.AutoFetchToken() + "A",
		"unknown":               ids.AutoFetchToken(),
		"revoked":               revoked,
		"deleted user":          orphan,
		"capture token":         bearer,
		"live ci_ on /in/auto/": liveInbound,
		"autofetch+upload row":  e.newAutoFetch(u, "autofetch", "upload"),
		"autofetch+inbound row": e.newAutoFetch(u, "autofetch", "inbound"),
		"inbound-shaped row":    "ci_" + liveAuto[3:],
		"percent-encoded":       "ca_%41" + strings.Repeat("A", 42),
	}
	in := map[string]string{
		"live ca_ on /in/":     liveAuto,
		"autofetch row as ci_": e.newInbound(u, "autofetch"),
		"inbound+autofetch":    e.newInbound(u, "inbound", "autofetch"),
	}
	bodies := map[string]func() (io.Reader, reqOpt){
		"small": func() (io.Reader, reqOpt) { return strings.NewReader(`{"x":1}`), func(*http.Request) {} },
		"large": func() (io.Reader, reqOpt) { return bytes.NewReader(make([]byte, 300<<10)), func(*http.Request) {} },
		"chunked": func() (io.Reader, reqOpt) {
			return io.NopCloser(strings.NewReader(`{"x":1}`)), func(r *http.Request) {
				r.ContentLength = -1
				r.TransferEncoding = []string{"chunked"}
			}
		},
	}
	for bname, mk := range bodies {
		body, opt := mk()
		want := e.do(http.MethodPost, "/api/nope", body, withJSON(), opt)
		if want.code != 404 || want.str("message") != "no such route" {
			t.Fatalf("the catch-all itself: %d %s", want.code, want.body)
		}
		check := func(path, name string) {
			body, opt := mk()
			got := e.do(http.MethodPost, path, body, withJSON(), withCookie(cookie), withBearer(bearer), opt)
			if got.code != want.code || !bytes.Equal(got.body, want.body) || !reflect.DeepEqual(got.hdr, want.hdr) {
				t.Errorf("%s / %s body: got %d %v %s; want %d %v %s", name, bname, got.code, got.hdr, got.body,
					want.code, want.hdr, want.body)
			}
		}
		for name, raw := range auto {
			check("/in/auto/"+raw, name)
		}
		for name, raw := range in {
			check("/in/"+raw, name)
		}
	}
	if used, _ := e.db.StorageUsed(ctx); used != 0 {
		t.Fatalf("a miss stored %d bytes", used)
	}
	// Not a Bearer credential anywhere.
	if r := e.do(http.MethodGet, "/api/auth/me", nil, withBearer(liveAuto)); r.code != 401 {
		t.Fatalf("Bearer ca_… on /api/auth/me: %d", r.code)
	}
	// GET and HEAD answer the same for every value (no token oracle).
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		var first resp
		for i, raw := range []string{liveAuto, ids.AutoFetchToken(), "nope", revoked} {
			r := e.do(m, "/in/auto/"+raw, nil)
			if r.code != 405 || r.hdr.Get("Allow") != "POST" {
				t.Errorf("%s /in/auto/%s: %d %v", m, raw, r.code, r.hdr)
			}
			if i == 0 {
				first = r
			} else if !bytes.Equal(r.body, first.body) || !reflect.DeepEqual(r.hdr, first.hdr) {
				t.Errorf("%s /in/auto/ answers differ by value", m)
			}
		}
	}
}

// Every refusal past the credential, in the designed order, with no fetch:
// 415 not JSON → 413 body → 400 invalid → 400 attachment_host → 403
// not_bound → 415 not_video → 413 too_large → 503 unavailable. Each pair below
// breaks two rules at once and must be answered by the earlier one.
func TestAutoFetchRefusalsInOrderFetchNothing(t *testing.T) {
	rec := &recorderBot{}
	e := newEnv(t, func(d *Deps) { d.Bot = rec })
	u, _ := e.newUser("carol")
	other, _ := e.newUser("other")
	raw := e.newAutoFetch(u)
	e.bind(afGuild, u)
	e.bind("444444444444444444", other)
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	del := func(k string) func(map[string]any) { return func(m map[string]any) { delete(m, k) } }

	if r := e.do(http.MethodPost, "/in/auto/"+raw, strings.NewReader(`{}`), withHeader("Content-Type", "text/plain")); r.code != 415 || r.errCode() != "unsupported_media_type" {
		t.Fatalf("not JSON: %d %s", r.code, r.body)
	}
	if r := e.autoFetch(raw, afBody(set("filename", strings.Repeat("a", 5000)))); r.code != 413 || r.errCode() != "too_large" {
		t.Fatalf("body over 4 KiB: %d %s", r.code, r.body)
	}
	for name, body := range map[string]any{
		"guild not a snowflake": afBody(set("guild_id", "abc"), set("url", "http://evil.test/x")),
		"short guild":           afBody(set("guild_id", "123")),
		"no url":                afBody(del("url")),
		"no filename":           afBody(set("filename", "")),
		"control in filename":   afBody(set("filename", "a\nb.mp4")),
		"long content type":     afBody(set("content_type", strings.Repeat("v", 128))),
		"size zero":             afBody(set("size", 0)),
		"unknown field":         afBody(set("bucket", "x")),
		"not an object":         `[1]`,
		"trailing data":         `{"guild_id":"333333333333333333","url":"x","filename":"a.mp4","content_type":"","size":1} {}`,
	} {
		if r := e.autoFetch(raw, body); r.code != 400 || r.errCode() != "invalid_request" || r.str("message") != msgAutoFetchInvalid {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
	for name, u := range map[string]string{
		"http":              strings.Replace(afCDN, "https:", "http:", 1),
		"another host":      strings.Replace(afCDN, "cdn.discordapp.com", "cdn.discordapp.com.evil.test", 1),
		"a port":            strings.Replace(afCDN, "cdn.discordapp.com", "cdn.discordapp.com:8443", 1),
		"userinfo":          strings.Replace(afCDN, "cdn.discordapp.com", "x@cdn.discordapp.com", 1),
		"an avatar":         "https://cdn.discordapp.com/avatars/111111111111111111/abc.png",
		"an emoji":          "https://cdn.discordapp.com/emojis/111111111111111111.png",
		"short ids":         "https://cdn.discordapp.com/attachments/1/2/clip.mp4",
		"a deeper path":     "https://cdn.discordapp.com/attachments/111111111111111111/222222222222222222/a/clip.mp4",
		"dot-dot":           "https://cdn.discordapp.com/attachments/111111111111111111/222222222222222222/..",
		"encoded dot-dot":   "https://cdn.discordapp.com/attachments/111111111111111111/222222222222222222/%2e%2E",
		"encoded slash":     "https://cdn.discordapp.com/attachments/111111111111111111/222222222222222222/a%2Fb.mp4",
		"unbound AND off":   "https://example.com/attachments/111111111111111111/222222222222222222/clip.mp4",
		"media host avatar": "https://media.discordapp.net/stickers/111111111111111111.png",
		"U+0130 raw":        strings.Replace(afCDN, "cdn.discordapp.com", "cdn.dİscordapp.com", 1),
		"U+0130 encoded":    strings.Replace(afCDN, "cdn.discordapp.com", "cdn.d%C4%B0scordapp.com", 1),
	} {
		body := afBody(set("url", u))
		if name == "unbound AND off" {
			body["guild_id"] = "555555555555555555"
		}
		if r := e.autoFetch(raw, body); r.code != 400 || r.errCode() != "attachment_host" || r.str("message") != msgAutoFetchHost {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
	// The media host's own attachment path is accepted as far as the binding.
	media := strings.Replace(afCDN, "cdn.discordapp.com", "media.discordapp.net", 1)
	unbound := e.autoFetch(raw, afBody(set("url", media), set("guild_id", "555555555555555555"), set("content_type", "image/png"), set("filename", "a.png")))
	other2 := e.autoFetch(raw, afBody(set("guild_id", "444444444444444444")))
	if unbound.code != 403 || unbound.errCode() != "not_bound" || unbound.str("message") != msgAutoFetchNotBound {
		t.Fatalf("unbound: %d %s", unbound.code, unbound.body)
	}
	if other2.code != unbound.code || !bytes.Equal(other2.body, unbound.body) {
		t.Fatalf("bound to another account answers differently: %s vs %s", other2.body, unbound.body)
	}
	if r := e.autoFetch(raw, afBody(set("filename", "a.png"), set("content_type", "image/png"), set("size", 1<<40))); r.code != 415 || r.errCode() != "not_video" || r.str("message") != msgAutoFetchNotVideo {
		t.Fatalf("not a video: %d %s", r.code, r.body)
	}
	if r := e.autoFetch(raw, afBody(set("size", e.cfg.MaxUploadBytes+1))); r.code != 413 || r.errCode() != "too_large" ||
		!strings.Contains(r.str("message"), "larger than the server accepts") {
		t.Fatalf("too large: %d %s", r.code, r.body)
	}
	if rec.fetchCount() != 0 {
		t.Fatalf("a refusal reached the fetcher %d time(s)", rec.fetchCount())
	}
	// A video typed by extension alone passes the video test.
	if r := e.autoFetch(raw, afBody(set("content_type", ""))); r.code != 500 || rec.fetchCount() != 1 {
		t.Fatalf("an untyped .mp4 did not reach the fetcher: %d %s", r.code, r.body)
	}
	// No bot: the last refusal.
	nb := newEnv(t)
	nu, _ := nb.newUser("carol")
	nraw := nb.newAutoFetch(nu)
	nb.bind(afGuild, nu)
	if r := nb.autoFetch(nraw, afBody()); r.code != 503 || r.errCode() != "unavailable" || r.str("message") != msgAutoFetchUnavailable {
		t.Fatalf("no bot: %d %s", r.code, r.body)
	}
	toks, _ := nb.db.ListTokens(context.Background(), nu.ID)
	if toks[0].LastUsedAt != nil {
		t.Fatal("a refusal wrote last_used_at")
	}
}

// The whole path through a REAL notify.Bot: one CDN hit, a clip in the URL's
// account titled by the file name, 201 with /api/upload's UploadDTO as JSON
// whatever Accept says, last_used_at written; the access line carries the row
// id and the outcome word and never the guild, the URL or the file name.
func TestAutoFetchWholePath(t *testing.T) {
	stub := newAFStub(t, testVideo(4000))
	e, _ := afEnv(t, stub)
	u, _ := e.newUser("carol")
	raw := e.newAutoFetch(u)
	e.bind(afGuild, u)
	body, _ := json.Marshal(afBody(func(m map[string]any) { m["filename"] = "Sunset run.mp4" }))
	r, logs := e.logged(http.MethodPost, "/in/auto/"+raw, bytes.NewReader(body), withJSON(), withHeader("Accept", "text/plain"))
	if r.code != 201 || r.hdr.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("hit: %d %v %s", r.code, r.hdr, r.body)
	}
	want := map[string]bool{"id": true, "url": true, "embed_url": true, "kind": true, "state": true, "card_ready": true, "expires_at": true, "tiers": true}
	if len(r.json) != len(want) || r.str("url") != testOrigin+"/"+r.str("id") {
		t.Fatalf("not /api/upload's answer: %s", r.body)
	}
	if c := e.getClip(r.str("id")); c.OwnerID != u.ID || c.Title != "Sunset run" {
		t.Fatalf("clip owner %q title %q", c.OwnerID, c.Title)
	}
	if stub.count() != 1 {
		t.Fatalf("the CDN served %d request(s), want 1", stub.count())
	}
	toks, _ := e.db.ListTokens(context.Background(), u.ID)
	if toks[0].LastUsedAt == nil {
		t.Fatal("last_used_at not written")
	}
	if !strings.Contains(logs, "/in/{token}") || !strings.Contains(logs, "autofetch="+toks[0].ID) || !strings.Contains(logs, "auto=upload") {
		t.Fatalf("access line: %s", logs)
	}
	for _, secret := range []string{raw, raw[3:], afGuild, "cdn.discordapp", "Sunset", "111111111111111111"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("the access line carries %q: %s", secret, logs)
		}
	}
}

// The route's two limiters are the ONLY charge (review item 3). With a bucket
// of two, after one request of each kind a second request still passes the
// limiter and a third is 429 — so every kind spent exactly one token.
func TestAutoFetchChargesTheAccountOnce(t *testing.T) {
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	cases := map[string]func(map[string]any){
		"201":             nil,
		"202":             set("url", strings.Replace(afCDN, "clip.mp4", "slow.mp4", 1)),
		"fetch 502":       set("url", strings.Replace(afCDN, "clip.mp4", "redirect.mp4", 1)),
		"invalid":         set("guild_id", "x"),
		"attachment_host": set("url", "https://cdn.discordapp.com/avatars/1/2.png"),
		"not_bound":       set("guild_id", "555555555555555555"),
		"not_video":       func(m map[string]any) { m["content_type"], m["filename"] = "image/png", "a.png" },
		"too_large":       set("size", 1<<40),
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newAFStub(t, testVideo(4000))
			e, _ := afEnv(t, stub, func(d *Deps) {
				d.Cfg.RateUploadPerHour = 2
				if name == "202" {
					d.AutoFetchBudget = 100 * time.Millisecond // the slow job outlives the hold
				}
			})
			u, _ := e.newUser("carol")
			raw := e.newAutoFetch(u)
			e.bind(afGuild, u)
			var first resp
			if mut == nil {
				first = e.autoFetch(raw, afBody())
			} else {
				first = e.autoFetch(raw, afBody(mut))
			}
			if first.code == 429 {
				t.Fatalf("the first request was refused by the bucket: %s", first.body)
			}
			if name == "202" && first.code != 202 {
				t.Fatalf("the slow job answered %d %s, want 202", first.code, first.body)
			}
			if r := e.autoFetch(raw, afBody(set("guild_id", "x"))); r.code != 400 {
				t.Fatalf("after %s the account had no token left (%d %s): it was charged twice", name, r.code, r.body)
			}
			if r := e.autoFetch(raw, afBody(set("guild_id", "x"))); r.code != 429 {
				t.Fatalf("after %s and one more the bucket is not empty (%d): it was not charged", name, r.code)
			}
		})
	}
}

// A CDN redirect is a fetch failure: 502 `fetch`, and the redirect is not
// followed (one hit, the redirect itself).
func TestAutoFetchRedirectIsNotFollowed(t *testing.T) {
	stub := newAFStub(t, testVideo(4000))
	e, _ := afEnv(t, stub)
	u, _ := e.newUser("carol")
	raw := e.newAutoFetch(u)
	e.bind(afGuild, u)
	r := e.autoFetch(raw, afBody(func(m map[string]any) { m["url"] = strings.Replace(afCDN, "clip.mp4", "redirect.mp4", 1) }))
	if r.code != 502 || r.errCode() != "fetch" || r.str("message") != msgAutoFetchFetch {
		t.Fatalf("redirect: %d %s", r.code, r.body)
	}
	if stub.count() != 1 {
		t.Fatalf("the CDN saw %d request(s); a redirect was followed", stub.count())
	}
}

// A job still running at the hold's end answers 202 {"state":"working"}
// exactly, and the job carries on: its clip lands in the library.
func TestAutoFetchSlowJobAnswers202AndLands(t *testing.T) {
	stub := newAFStub(t, testVideo(4000))
	e, _ := afEnv(t, stub, func(d *Deps) { d.AutoFetchBudget = 200 * time.Millisecond })
	u, _ := e.newUser("carol")
	raw := e.newAutoFetch(u)
	e.bind(afGuild, u)
	r := e.autoFetch(raw, afBody(func(m map[string]any) {
		m["url"], m["filename"] = strings.Replace(afCDN, "clip.mp4", "slow.mp4", 1), "slow.mp4"
	}))
	if r.code != 202 || string(r.body) != `{"state":"working"}`+"\n" {
		t.Fatalf("slow: %d %q", r.code, r.body)
	}
	stub.free()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		clips, _, err := e.db.ListClips(context.Background(), db.ListQuery{ViewerID: u.ID, Mine: true, Limit: 10})
		if err == nil && len(clips) == 1 && clips[0].Title == "slow" {
			return
		}
		time.Sleep(20 * time.Millisecond) // sleep: the job lands after its 202 with no signal the test can hold; the row is the subject
	}
	t.Fatal("the job did not land after its 202")
}

// A client that hangs up after the job started: the handler returns at once,
// the job finishes on its own — its outcome is buffered, so nothing blocks —
// and Shutdown, which waits for every job, returns: no goroutine is left.
// A client already gone when the request arrives: the database reads on its
// context (the credential's, then the binding's) refuse, so nothing starts.
// The handler has no check of its own for this case.
func TestAutoFetchClientGone(t *testing.T) {
	stub := newAFStub(t, testVideo(4000))
	e, bot := afEnv(t, stub)
	u, _ := e.newUser("carol")
	raw := e.newAutoFetch(u)
	e.bind(afGuild, u)
	body, _ := json.Marshal(afBody(func(m map[string]any) { m["url"] = strings.Replace(afCDN, "clip.mp4", "slow.mp4", 1) }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/in/auto/"+raw, bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		e.h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	stub.waitHits(t, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return when its client hung up")
	}
	stub.free()
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if err := bot.Shutdown(sctx); err != nil {
		t.Fatalf("a job outlived its hung-up client: %v", err)
	}

	rec := &recorderBot{}
	e2 := newEnv(t, func(d *Deps) { d.Bot = rec })
	u2, _ := e2.newUser("carol")
	raw2 := e2.newAutoFetch(u2)
	e2.bind(afGuild, u2)
	gone, cancel2 := context.WithCancel(context.Background())
	cancel2()
	body2, _ := json.Marshal(afBody())
	req := httptest.NewRequest(http.MethodPost, "/in/auto/"+raw2, bytes.NewReader(body2)).WithContext(gone)
	req.Header.Set("Content-Type", "application/json")
	e2.h.ServeHTTP(httptest.NewRecorder(), req)
	if rec.fetchCount() != 0 {
		t.Fatal("a job started for a client already gone")
	}
}
