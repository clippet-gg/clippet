package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
)

func TestPrincipalRoundTrip(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("empty ctx has a principal")
	}
	p := Principal{User: db.User{ID: "u1", Username: "carol"}, Via: "token", TokenID: "t1", Scopes: []string{"upload"}}
	ctx := WithPrincipal(context.Background(), p)
	got, ok := FromContext(ctx)
	if !ok || got.User.ID != "u1" || got.Via != "token" || got.TokenID != "t1" || len(got.Scopes) != 1 {
		t.Fatalf("FromContext = %+v, %v", got, ok)
	}
	// Nested overwrite wins.
	ctx2 := WithPrincipal(ctx, Principal{User: db.User{ID: "u2"}, Via: "session"})
	if got, _ := FromContext(ctx2); got.User.ID != "u2" || got.Via != "session" {
		t.Fatalf("overwrite = %+v", got)
	}
	if got, _ := FromContext(ctx); got.User.ID != "u1" {
		t.Fatal("parent ctx mutated")
	}
}

func TestResolverMiddlewarePassThrough(t *testing.T) {
	called := false
	var r = &Resolver{}
	h := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		called = true
		if _, ok := FromContext(req.Context()); ok {
			t.Error("nil-DB middleware set a principal")
		}
		w.WriteHeader(204)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer ck_x")
	h.ServeHTTP(rec, req)
	if !called || rec.Code != 204 {
		t.Fatalf("pass-through: called=%v code=%d", called, rec.Code)
	}
	var _ Authenticator = r
}

// stubAuth is the 3-line Authenticator Stream E's owner-path tests use.
type stubAuth struct{ p Principal }

func (s stubAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), s.p)))
	})
}

func TestStubAuthenticatorInjects(t *testing.T) {
	var a Authenticator = stubAuth{Principal{User: db.User{ID: "owner"}, Via: "session"}}
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok || p.User.ID != "owner" {
			t.Errorf("principal = %+v, %v", p, ok)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil || !strings.HasPrefix(h, "$2a$12$") {
		t.Fatalf("HashPassword = %q, %v", h, err)
	}
	if !CheckPassword(h, "correct horse") || CheckPassword(h, "wrong") || CheckPassword("not a hash", "x") {
		t.Fatal("CheckPassword")
	}
	if _, err := HashPassword(strings.Repeat("a", 73)); err == nil {
		t.Fatal("73-byte password accepted")
	}
}

func TestTokensAndHashes(t *testing.T) {
	raw, hash := NewSessionToken()
	if len(raw) != 43 || hash != HashToken(raw) || len(hash) != 64 {
		t.Fatalf("NewSessionToken = %q (%d), %q", raw, len(raw), hash)
	}
	raw2, _ := NewSessionToken()
	if raw2 == raw {
		t.Fatal("two session tokens collided")
	}
	if HashToken("abc") != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("HashToken(abc) = %s", HashToken("abc"))
	}
	tok := ids.Token()
	if p := TokenPrefixOf(tok); len(p) != 8 || !strings.HasPrefix(tok, "ck_"+p) {
		t.Fatalf("TokenPrefixOf(%s) = %q", tok, p)
	}
}

func TestSessionCookieFlags(t *testing.T) {
	for _, https := range []bool{false, true} {
		rec := httptest.NewRecorder()
		SetSessionCookie(rec, config.Config{SessionCookie: "sess", SessionTTL: 2 * time.Hour}, https, "raw-value", 2*time.Hour)
		cs := rec.Result().Cookies()
		if len(cs) != 1 {
			t.Fatalf("https=%v: %d cookies", https, len(cs))
		}
		c := cs[0]
		if c.Name != "sess" || c.Value != "raw-value" || c.Path != "/" || !c.HttpOnly ||
			c.SameSite != http.SameSiteLaxMode || c.MaxAge != 7200 || c.Secure != https {
			t.Fatalf("https=%v: cookie %+v", https, c)
		}
		rec = httptest.NewRecorder()
		ClearSessionCookie(rec, config.Config{SessionCookie: "sess"}, https)
		c = rec.Result().Cookies()[0]
		if c.Name != "sess" || c.Value != "" || c.MaxAge != -1 || c.Secure != https || !c.HttpOnly || c.Path != "/" {
			t.Fatalf("https=%v: cleared cookie %+v", https, c)
		}
	}
	// Defaults for a bare config.
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, config.Config{}, false, "v", DefaultSessionTTL)
	if c := rec.Result().Cookies()[0]; c.Name != DefaultCookieName || c.MaxAge != int(DefaultSessionTTL/time.Second) {
		t.Fatalf("default cookie %+v", c)
	}
}

func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

type fixture struct {
	d       *db.DB
	res     *Resolver
	user    db.User
	other   db.User
	cookie  string
	token   string
	tokenID string
	now     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	d := testDB(t)
	now := time.Unix(1_700_000_000, 0)
	f := &fixture{d: d, now: now}
	f.res = &Resolver{DB: d, Cfg: config.Config{SessionCookie: "sess", SessionTTL: time.Hour}, Now: func() time.Time { return f.now }}
	var err error
	if f.user, err = d.CreateUser(ctx, ids.New(), "carol", "h", now.Unix()); err != nil {
		t.Fatal(err)
	}
	if f.other, err = d.CreateUser(ctx, ids.New(), "other", "h", now.Unix()); err != nil {
		t.Fatal(err)
	}
	if f.cookie, _, err = CreateSession(ctx, d, f.res.Cfg, f.user.ID, "UA", now); err != nil {
		t.Fatal(err)
	}
	f.token = ids.Token()
	f.tokenID = ids.New()
	err = d.CreateToken(ctx, db.APIToken{ID: f.tokenID, UserID: f.other.ID, Name: "t", TokenHash: HashToken(f.token),
		Prefix: TokenPrefixOf(f.token), Scopes: []string{"upload"}, CreatedAt: now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) resolve(t *testing.T, mutate func(*http.Request)) (int, Principal, bool) {
	t.Helper()
	var got Principal
	var found bool
	h := f.res.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, found = FromContext(r.Context())
		w.WriteHeader(204)
	}))
	req := httptest.NewRequest("GET", "/", nil)
	mutate(req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, got, found
}

func TestResolverPrecedenceAndPopulation(t *testing.T) {
	f := newFixture(t)
	withCookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "sess", Value: f.cookie}) }
	withToken := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.token) }

	code, p, ok := f.resolve(t, withCookie)
	if code != 204 || !ok || p.Via != ViaSession || p.User.ID != f.user.ID || p.SessionHash != HashToken(f.cookie) ||
		p.TokenID != "" || p.Scopes != nil {
		t.Fatalf("cookie: %d %+v %v", code, p, ok)
	}
	code, p, ok = f.resolve(t, withToken)
	if code != 204 || !ok || p.Via != ViaToken || p.User.ID != f.other.ID || p.TokenID != f.tokenID ||
		p.SessionHash != "" || len(p.Scopes) != 1 || p.Scopes[0] != "upload" {
		t.Fatalf("token: %d %+v %v", code, p, ok)
	}
	// Both present: Bearer wins.
	_, p, ok = f.resolve(t, func(r *http.Request) { withCookie(r); withToken(r) })
	if !ok || p.Via != ViaToken || p.User.ID != f.other.ID {
		t.Fatalf("both: %+v", p)
	}
	// Invalid Bearer falls back to the cookie; invalid cookie → anonymous.
	_, p, ok = f.resolve(t, func(r *http.Request) { withCookie(r); r.Header.Set("Authorization", "Bearer ck_nope") })
	if !ok || p.Via != ViaSession {
		t.Fatalf("bad bearer + cookie: %+v %v", p, ok)
	}
	_, _, ok = f.resolve(t, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "sess", Value: "nope"}) })
	if ok {
		t.Fatal("bad cookie resolved a principal")
	}
	_, _, ok = f.resolve(t, func(r *http.Request) { r.Header.Set("Authorization", "Basic abc") })
	if ok {
		t.Fatal("non-Bearer scheme resolved a principal")
	}
	// Revoked token is anonymous; expired session is anonymous.
	if err := f.d.RevokeToken(context.Background(), f.other.ID, f.tokenID, f.now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok = f.resolve(t, withToken); ok {
		t.Fatal("revoked token resolved")
	}
	f.now = f.now.Add(2 * time.Hour)
	if _, _, ok = f.resolve(t, withCookie); ok {
		t.Fatal("expired session resolved")
	}
}

func TestResolverTouchesAtMostOncePerMinute(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	withCookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "sess", Value: f.cookie}) }
	withToken := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.token) }

	f.now = f.now.Add(10 * time.Minute)
	f.resolve(t, withCookie)
	s, _, err := f.d.GetSession(ctx, HashToken(f.cookie), f.now.Unix())
	if err != nil || s.LastSeenAt != f.now.Unix() || s.ExpiresAt != f.now.Add(time.Hour).Unix() {
		t.Fatalf("first touch: %+v %v", s, err)
	}
	f.now = f.now.Add(30 * time.Second)
	f.resolve(t, withCookie)
	if s, _, _ = f.d.GetSession(ctx, HashToken(f.cookie), f.now.Unix()); s.LastSeenAt == f.now.Unix() {
		t.Fatal("session touched twice within a minute")
	}
	f.now = f.now.Add(31 * time.Second)
	f.resolve(t, withCookie)
	if s, _, _ = f.d.GetSession(ctx, HashToken(f.cookie), f.now.Unix()); s.LastSeenAt != f.now.Unix() {
		t.Fatal("session not touched after a minute")
	}

	f.resolve(t, withToken)
	tok, _, err := f.d.GetTokenByHash(ctx, HashToken(f.token))
	if err != nil || tok.LastUsedAt == nil || *tok.LastUsedAt != f.now.Unix() {
		t.Fatalf("token touch: %+v %v", tok, err)
	}
	f.now = f.now.Add(10 * time.Second)
	f.resolve(t, withToken)
	if tok, _, _ = f.d.GetTokenByHash(ctx, HashToken(f.token)); *tok.LastUsedAt == f.now.Unix() {
		t.Fatal("token touched twice within a minute")
	}
}

// The session slides for the browser too (§4; §20 erratum 25): when the
// once-a-minute touch extends expires_at, the response re-issues the cookie
// — same value, fresh Max-Age — so a jar that honours Max-Age keeps the
// session exactly as long as the DB does. Nothing else ever sets it.
func TestResolverReissuesCookieOnTouch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	withCookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "sess", Value: f.cookie}) }
	withToken := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.token) }
	cookies := func(mutate func(*http.Request)) []*http.Cookie {
		t.Helper()
		h := f.res.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
		req := httptest.NewRequest("GET", "/", nil)
		mutate(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result().Cookies()
	}

	// 50 minutes into a 1h session: the touch fires and the cookie comes
	// back with the full TTL and the login-time flags.
	f.now = f.now.Add(50 * time.Minute)
	cs := cookies(withCookie)
	if len(cs) != 1 {
		t.Fatalf("touch: %d Set-Cookie headers, want 1", len(cs))
	}
	if c := cs[0]; c.Name != "sess" || c.Value != f.cookie || c.MaxAge != 3600 || !c.HttpOnly || c.Path != "/" ||
		c.SameSite != http.SameSiteLaxMode || c.Secure {
		t.Fatalf("re-issued cookie %+v", c)
	}
	// Within the minute nothing is touched and nothing is re-issued.
	f.now = f.now.Add(30 * time.Second)
	if cs := cookies(withCookie); len(cs) != 0 {
		t.Fatalf("untouched request re-issued %d cookies", len(cs))
	}
	// Past the login-time Max-Age the row is still valid (it slid) and so
	// is the cookie the browser holds; the next touch re-issues it again.
	f.now = f.now.Add(11 * time.Minute)
	if cs := cookies(withCookie); len(cs) != 1 || cs[0].Value != f.cookie || cs[0].MaxAge != 3600 {
		t.Fatalf("second touch: %+v", cs)
	}
	s, _, err := f.d.GetSession(ctx, HashToken(f.cookie), f.now.Unix())
	if err != nil || s.ExpiresAt != f.now.Add(time.Hour).Unix() {
		t.Fatalf("row after second touch: %+v %v", s, err)
	}
	// A token principal and an anonymous request never get a cookie.
	if cs := cookies(withToken); len(cs) != 0 {
		t.Fatalf("token request got %d cookies", len(cs))
	}
	if cs := cookies(func(*http.Request) {}); len(cs) != 0 {
		t.Fatalf("anonymous request got %d cookies", len(cs))
	}
	// Secure follows PUBLIC_BASE_URL's scheme, as at login.
	f.res.Cfg.PublicBaseURL = &url.URL{Scheme: "https", Host: "clips.example"}
	f.now = f.now.Add(time.Minute)
	if cs := cookies(withCookie); len(cs) != 1 || !cs[0].Secure {
		t.Fatalf("https re-issue: %+v", cs)
	}
}

// Logout on a request whose touch queued a refresh must clear the cookie,
// not re-issue it: ClearSessionCookie drops the pending Set-Cookie for the
// session name and leaves every other cookie alone.
func TestClearSessionCookieDropsPendingRefresh(t *testing.T) {
	cfg := config.Config{SessionCookie: "sess", SessionTTL: time.Hour}
	rec := httptest.NewRecorder()
	rec.Header().Add("Set-Cookie", "other=1; Path=/")
	SetSessionCookie(rec, cfg, false, "raw", SessionTTL(cfg))
	ClearSessionCookie(rec, cfg, false)
	var sess []*http.Cookie
	other := 0
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case "sess":
			sess = append(sess, c)
		case "other":
			other++
		}
	}
	if len(sess) != 1 || sess[0].Value != "" || sess[0].MaxAge != -1 || other != 1 {
		t.Fatalf("Set-Cookie after clear: %v", rec.Header().Values("Set-Cookie"))
	}
}

func TestResolverDBErrorPassesThroughAnonymously(t *testing.T) {
	f := newFixture(t)
	_ = f.d.Close()
	var resolveErr error
	h := f.res.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolveErr = ResolveError(r.Context())
		if _, ok := FromContext(r.Context()); ok {
			t.Error("a principal was resolved from a closed database")
		}
		w.WriteHeader(204)
	}))
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "sess", Value: f.cookie}) },
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.token) },
	} {
		resolveErr = nil
		req := httptest.NewRequest("GET", "/", nil)
		mutate(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		// The Resolver never rejects (§14): the route family decides the
		// shape of the 503 from ResolveError.
		if rec.Code != 204 || resolveErr == nil {
			t.Fatalf("closed db: code=%d resolveErr=%v", rec.Code, resolveErr)
		}
	}
	// No credential → no lookup → no error even with the DB closed.
	code, _, ok := f.resolve(t, func(*http.Request) {})
	if code != 204 || ok {
		t.Fatalf("anonymous with closed db: %d ok=%v", code, ok)
	}
	if ResolveError(context.Background()) != nil {
		t.Fatal("ResolveError on a bare context")
	}
}

func TestTouchGateBounded(t *testing.T) {
	g := newTouchGate(3)
	now := time.Unix(1_700_000_000, 0)
	for _, k := range []string{"a", "b", "c"} {
		if !g.due(k, now) {
			t.Fatalf("%s not due on first sight", k)
		}
	}
	if g.due("a", now.Add(30*time.Second)) {
		t.Fatal("a due again within a minute")
	}
	if !g.due("d", now) || len(g.last) != 1 {
		t.Fatalf("cap reset: len=%d", len(g.last))
	}
	if !g.due("a", now.Add(61*time.Second)) {
		t.Fatal("a not due after a minute")
	}
}

func serve(h http.Handler, req *http.Request) (int, map[string]any) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
}

func withPrincipal(req *http.Request, p Principal) *http.Request {
	return req.WithContext(WithPrincipal(req.Context(), p))
}

func TestRequireMiddleware(t *testing.T) {
	session := Principal{User: db.User{ID: "u"}, Via: ViaSession}
	token := Principal{User: db.User{ID: "u"}, Via: ViaToken, Scopes: []string{"upload"}}
	anon := httptest.NewRequest("GET", "/", nil)

	if code, body := serve(RequireUser(okHandler()), anon); code != 401 || body["error"] != "unauthorized" {
		t.Fatalf("RequireUser anon: %d %v", code, body)
	}
	if code, _ := serve(RequireUser(okHandler()), withPrincipal(anon, token)); code != 204 {
		t.Fatalf("RequireUser token: %d", code)
	}
	if code, _ := serve(RequireSession(okHandler()), anon); code != 401 {
		t.Fatalf("RequireSession anon: %d", code)
	}
	if code, body := serve(RequireSession(okHandler()), withPrincipal(anon, token)); code != 403 || body["error"] != "token_not_allowed" {
		t.Fatalf("RequireSession token: %d %v", code, body)
	}
	if code, _ := serve(RequireSession(okHandler()), withPrincipal(anon, session)); code != 204 {
		t.Fatalf("RequireSession session: %d", code)
	}
	if code, _ := serve(RequireScope("upload")(okHandler()), anon); code != 401 {
		t.Fatalf("RequireScope anon: %d", code)
	}
	if code, _ := serve(RequireScope("upload")(okHandler()), withPrincipal(anon, token)); code != 204 {
		t.Fatalf("RequireScope token with scope: %d", code)
	}
	if code, body := serve(RequireScope("admin")(okHandler()), withPrincipal(anon, token)); code != 403 || body["error"] != "insufficient_scope" {
		t.Fatalf("RequireScope token lacking scope: %d %v", code, body)
	}
	if code, _ := serve(RequireScope("admin")(okHandler()), withPrincipal(anon, session)); code != 204 {
		t.Fatalf("RequireScope session: %d", code)
	}
}

func TestCSRFTable(t *testing.T) {
	const origin = "http://localhost:8481"
	session := Principal{User: db.User{ID: "u"}, Via: ViaSession}
	token := Principal{User: db.User{ID: "u"}, Via: ViaToken}
	tests := []struct {
		name   string
		method string
		via    *Principal
		hdr    map[string]string
		want   int
	}{
		{"no headers pass", "POST", &session, nil, 204},
		{"same Origin pass", "POST", &session, map[string]string{"Origin": origin}, 204},
		{"same Origin with default port pass", "POST", &session, map[string]string{"Origin": "http://localhost:8481"}, 204},
		{"foreign Origin 403", "POST", &session, map[string]string{"Origin": "https://evil.example"}, 403},
		{"null Origin 403", "POST", &session, map[string]string{"Origin": "null"}, 403},
		{"Sec-Fetch-Site cross-site 403", "PATCH", &session, map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"Sec-Fetch-Site same-origin pass", "DELETE", &session, map[string]string{"Sec-Fetch-Site": "same-origin"}, 204},
		{"Sec-Fetch-Site none pass", "PUT", &session, map[string]string{"Sec-Fetch-Site": "none"}, 204},
		{"Bearer + foreign Origin pass", "POST", &token, map[string]string{"Origin": "https://evil.example"}, 204},
		{"anonymous + foreign Origin pass", "POST", nil, map[string]string{"Origin": "https://evil.example"}, 204},
		{"GET never checked", "GET", &session, map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, 204},
	}
	h := CSRF(origin)(okHandler())
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/", nil)
			for k, v := range tc.hdr {
				req.Header.Set(k, v)
			}
			if tc.via != nil {
				req = withPrincipal(req, *tc.via)
			}
			code, body := serve(h, req)
			if code != tc.want {
				t.Fatalf("code = %d %v, want %d", code, body, tc.want)
			}
			if code == 403 && body["error"] != "csrf" {
				t.Fatalf("body = %v", body)
			}
		})
	}
}

func TestSameOrigin(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"http://a.example", "http://a.example:80", true},
		{"https://a.example", "https://a.example:443", true},
		{"HTTPS://A.example", "https://a.example", true},
		{"http://a.example", "https://a.example", false},
		{"http://a.example:8080", "http://a.example", false},
		{"http://a.example/", "http://a.example", true},
		{"null", "http://a.example", false},
		{"", "http://a.example", false},
	}
	for _, tc := range tests {
		if got := sameOrigin(tc.a, tc.b); got != tc.want {
			t.Errorf("sameOrigin(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}
