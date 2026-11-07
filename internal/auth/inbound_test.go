package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"clippet/internal/db"
	"clippet/internal/ids"
)

// The inbound URL's principal fails CLOSED (§20 erratum 322). POST /in/{token}
// installs a token principal holding only `inbound`; every existing gate must
// refuse it, so composing it with any of them by mistake opens nothing.
func TestInboundPrincipalFailsClosed(t *testing.T) {
	p := Principal{User: db.User{ID: "u1"}, Via: ViaToken, TokenID: "t1", Scopes: []string{ScopeInbound}}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	run := func(h http.Handler) int {
		req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(WithPrincipal(context.Background(), p))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := run(RequireSession(ok)); c != http.StatusForbidden {
		t.Errorf("RequireSession = %d, want 403", c)
	}
	for _, s := range Scopes {
		if c := run(RequireScope(s)(ok)); c != http.StatusForbidden {
			t.Errorf("RequireScope(%s) = %d, want 403", s, c)
		}
		if c := run(RequireSessionOrScope(s)(ok)); c != http.StatusForbidden {
			t.Errorf("RequireSessionOrScope(%s) = %d, want 403", s, c)
		}
		if p.HasScope(s) {
			t.Errorf("HasScope(%s) = true", s)
		}
	}
	if p.OwnerReader() {
		t.Error("OwnerReader = true for the inbound principal")
	}
	// The Bearer scope set stays closed: `inbound` is not a scope a Bearer
	// token may be minted with.
	if ValidScope(ScopeInbound) {
		t.Error("ValidScope(inbound) = true")
	}
	for _, s := range Scopes {
		if s == ScopeInbound {
			t.Error("Scopes lists inbound")
		}
	}
}

// The Resolver never turns an inbound URL into a principal: a `ci_` value as
// a Bearer header is anonymous, and a row holding `inbound` is not found even
// when its exact hash is handed to resolveToken.
func TestResolverRefusesAnInboundRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	raw := ids.InboundToken()
	if err := f.d.CreateToken(ctx, db.APIToken{ID: ids.New(), UserID: f.user.ID, Name: "in",
		TokenHash: HashToken(raw), Prefix: TokenPrefixOf(raw), Scopes: []string{ScopeInbound},
		CreatedAt: f.now.Unix()}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.resolve(t, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+raw) }); ok {
		t.Fatal("Bearer ci_… resolved a principal")
	}
	f.res.init()
	if _, found, err := f.res.resolveToken(ctx, raw, f.now); found || err != nil {
		t.Fatalf("resolveToken on an inbound row: found=%v err=%v", found, err)
	}
	if p := TokenPrefixOf(raw); len(p) != 8 || raw[:11] != "ci_"+p {
		t.Fatalf("TokenPrefixOf(%s) = %q", raw, p)
	}
}

// The Clippet Auto URL (§20 erratum 324) is the same kind of credential: its
// principal fails closed exactly as the inbound one does, a `ca_` value as a
// Bearer header is anonymous, and a row holding `autofetch` — alone, or beside
// any other scope, as a hand-written row might — is not found by
// resolveToken even with its exact hash.
func TestAutoFetchPrincipalFailsClosedAndTheResolverRefusesIt(t *testing.T) {
	p := Principal{User: db.User{ID: "u1"}, Via: ViaToken, TokenID: "t1", Scopes: []string{ScopeAutoFetch}}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	run := func(h http.Handler) int {
		req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(WithPrincipal(context.Background(), p))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := run(RequireSession(ok)); c != http.StatusForbidden {
		t.Errorf("RequireSession = %d, want 403", c)
	}
	for _, s := range Scopes {
		if c := run(RequireScope(s)(ok)); c != http.StatusForbidden {
			t.Errorf("RequireScope(%s) = %d, want 403", s, c)
		}
		if c := run(RequireSessionOrScope(s)(ok)); c != http.StatusForbidden {
			t.Errorf("RequireSessionOrScope(%s) = %d, want 403", s, c)
		}
	}
	if p.OwnerReader() || ValidScope(ScopeAutoFetch) {
		t.Error("the autofetch principal reads, or autofetch is a Bearer scope")
	}

	f := newFixture(t)
	ctx := context.Background()
	for _, scopes := range [][]string{{ScopeAutoFetch}, {ScopeAutoFetch, ScopeUpload}, {ScopeRead, ScopeAutoFetch},
		{ScopeInbound, ScopeAutoFetch}} {
		raw := ids.AutoFetchToken()
		if err := f.d.CreateToken(ctx, db.APIToken{ID: ids.New(), UserID: f.user.ID, Name: "auto",
			TokenHash: HashToken(raw), Prefix: TokenPrefixOf(raw), Scopes: scopes,
			CreatedAt: f.now.Unix()}); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := f.resolve(t, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+raw) }); ok {
			t.Fatalf("Bearer ca_… resolved a principal (%v)", scopes)
		}
		f.res.init()
		if _, found, err := f.res.resolveToken(ctx, raw, f.now); found || err != nil {
			t.Fatalf("resolveToken on a %v row: found=%v err=%v", scopes, found, err)
		}
		if px := TokenPrefixOf(raw); len(px) != 8 || raw[:11] != "ca_"+px {
			t.Fatalf("TokenPrefixOf(%s) = %q", raw, px)
		}
	}
	// A ck_-shaped row holding autofetch beside upload (only a hand-written row
	// could) is refused by the same rule, through the Bearer door it would use.
	raw := ids.Token()
	if err := f.d.CreateToken(ctx, db.APIToken{ID: ids.New(), UserID: f.user.ID, Name: "mixed",
		TokenHash: HashToken(raw), Prefix: TokenPrefixOf(raw), Scopes: []string{ScopeUpload, ScopeAutoFetch},
		CreatedAt: f.now.Unix()}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.resolve(t, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+raw) }); ok {
		t.Fatal("a ck_ row holding autofetch resolved a principal")
	}
}
