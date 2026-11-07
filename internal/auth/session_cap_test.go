package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/ids"
)

// The §20 erratum 105 ceiling through the Resolver: a session slides while it
// is used, but neither the row nor the re-issued cookie ever reaches past
// `created_at + SESSION_MAX_TTL`. Sliding alone made an actively used stolen
// SESSION_TTL=720h cookie immortal, which is the whole reason for the cap.
func TestSlidingSessionStopsAtTheCeiling(t *testing.T) {
	ctx := context.Background()
	d := testDB(t)
	now := time.Unix(1_700_000_000, 0)
	cfg := config.Config{SessionCookie: "sess", SessionTTL: time.Hour, SessionMaxTTL: 3 * time.Hour}
	res := &Resolver{DB: d, Cfg: cfg, Now: func() time.Time { return now }}
	u, err := d.CreateUser(ctx, ids.New(), "carol", "h", now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	raw, ttl, err := CreateSession(ctx, d, cfg, u.ID, "UA", now)
	if err != nil {
		t.Fatal(err)
	}
	if ttl != time.Hour {
		t.Fatalf("fresh cookie ttl = %s, want SESSION_TTL", ttl)
	}
	created := now

	// One request per half hour, forever: exactly the traffic pattern that
	// used to keep a session alive indefinitely.
	touch := func() []*http.Cookie {
		t.Helper()
		h := res.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: "sess", Value: raw})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result().Cookies()
	}
	ceiling := created.Add(3 * time.Hour).Unix()
	for i := 0; i < 4; i++ {
		now = now.Add(30 * time.Minute)
		cs := touch()
		if len(cs) != 1 {
			t.Fatalf("touch %d re-issued %d cookies", i, len(cs))
		}
		s, _, err := d.GetSession(ctx, HashToken(raw), now.Unix())
		if err != nil {
			t.Fatalf("touch %d: %v", i, err)
		}
		want := now.Add(time.Hour).Unix()
		if want > ceiling {
			want = ceiling
		}
		if s.ExpiresAt != want {
			t.Errorf("touch %d: expires_at = %d, want %d (ceiling %d)", i, s.ExpiresAt, want, ceiling)
		}
		// The browser is told the same thing the row holds: a cookie that
		// outlived its row would be erratum 25's bug in reverse.
		if wantAge := int(want - now.Unix()); cs[0].MaxAge != wantAge {
			t.Errorf("touch %d: cookie Max-Age = %d, want %d", i, cs[0].MaxAge, wantAge)
		}
	}
	// Past the ceiling the session is simply gone, whatever it does next.
	now = created.Add(3*time.Hour + time.Second)
	h := res.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); ok {
			t.Error("a session past its ceiling still resolved")
		}
		w.WriteHeader(204)
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "sess", Value: raw})
	h.ServeHTTP(httptest.NewRecorder(), req)
}

// SESSION_MAX_TTL below SESSION_TTL clamps even a BRAND-NEW session, and the
// cookie says so.
func TestCreateSessionClampsToTheCeiling(t *testing.T) {
	ctx := context.Background()
	d := testDB(t)
	now := time.Unix(1_700_000_000, 0)
	cfg := config.Config{SessionCookie: "sess", SessionTTL: 720 * time.Hour, SessionMaxTTL: time.Hour}
	u, err := d.CreateUser(ctx, ids.New(), "carol", "h", now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	raw, ttl, err := CreateSession(ctx, d, cfg, u.ID, "UA", now)
	if err != nil {
		t.Fatal(err)
	}
	if ttl != time.Hour {
		t.Fatalf("cookie ttl = %s, want the ceiling (1h)", ttl)
	}
	s, _, err := d.GetSession(ctx, HashToken(raw), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if s.ExpiresAt != now.Add(time.Hour).Unix() || s.MaxExpiresAt != now.Add(time.Hour).Unix() {
		t.Fatalf("row = %+v", s)
	}
}

// The defaults a bare config.Config falls back to (§20 erratum 105).
func TestSessionMaxTTLDefault(t *testing.T) {
	if got := SessionMaxTTL(config.Config{}); got != DefaultSessionMaxTTL {
		t.Errorf("SessionMaxTTL(zero) = %s, want %s", got, DefaultSessionMaxTTL)
	}
	if got := SessionMaxTTL(config.Config{SessionMaxTTL: 5 * time.Hour}); got != 5*time.Hour {
		t.Errorf("SessionMaxTTL = %s", got)
	}
	if DefaultSessionMaxTTL != config.DefaultSessionMaxTTL {
		t.Error("auth and config disagree about the SESSION_MAX_TTL default")
	}
}

// A non-positive lifetime must never spell "delete this cookie": a session in
// its last second is still a session.
func TestSetSessionCookieFloorsMaxAge(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, config.Config{SessionCookie: "sess"}, false, "v", -time.Hour)
	cs := rec.Result().Cookies()
	if len(cs) != 1 || cs[0].MaxAge != 1 {
		t.Fatalf("cookies = %+v", cs)
	}
}

// The scope vocabulary (§20 erratum 111) and the owner-read rule of erratum
// 112: a session holds everything, a read token unlocks the owner-private
// fields of its own rows, an upload-only token does not.
func TestScopesAndOwnerReader(t *testing.T) {
	if !ValidScope(ScopeUpload) || !ValidScope(ScopeRead) || ValidScope("admin") || ValidScope("") {
		t.Error("ValidScope admits the wrong set")
	}
	for name, tc := range map[string]struct {
		p                 Principal
		read, up, ownerRd bool
	}{
		"session":     {Principal{Via: ViaSession}, true, true, true},
		"read token":  {Principal{Via: ViaToken, Scopes: []string{ScopeRead}}, true, false, true},
		"upload only": {Principal{Via: ViaToken, Scopes: []string{ScopeUpload}}, false, true, false},
		"both":        {Principal{Via: ViaToken, Scopes: []string{ScopeUpload, ScopeRead}}, true, true, true},
		"no scopes":   {Principal{Via: ViaToken}, false, false, false},
	} {
		if got := tc.p.HasScope(ScopeRead); got != tc.read {
			t.Errorf("%s: HasScope(read) = %v", name, got)
		}
		if got := tc.p.HasScope(ScopeUpload); got != tc.up {
			t.Errorf("%s: HasScope(upload) = %v", name, got)
		}
		if got := tc.p.OwnerReader(); got != tc.ownerRd {
			t.Errorf("%s: OwnerReader() = %v", name, got)
		}
	}
}

// RequireSessionOrScope is RequireSession widened by one scope and by nothing
// else — and its refusal keeps erratum 19's `token_not_allowed` code.
func TestRequireSessionOrScope(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h := RequireSessionOrScope(ScopeRead)(next)
	for name, tc := range map[string]struct {
		p    *Principal
		want int
	}{
		"anonymous":   {nil, 401},
		"session":     {&Principal{Via: ViaSession}, 204},
		"read token":  {&Principal{Via: ViaToken, Scopes: []string{ScopeRead}}, 204},
		"upload only": {&Principal{Via: ViaToken, Scopes: []string{ScopeUpload}}, 403},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		if tc.p != nil {
			req = req.WithContext(WithPrincipal(req.Context(), *tc.p))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: %d, want %d", name, rec.Code, tc.want)
		}
		if tc.want == 403 && !strings.Contains(rec.Body.String(), `"token_not_allowed"`) {
			t.Errorf("%s: body %s, want the erratum 19 error code", name, rec.Body.String())
		}
		if tc.want != 204 && rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: a rejection must be no-store", name)
		}
	}
}
