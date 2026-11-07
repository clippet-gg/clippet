package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
)

// registered creates a real account through the API (bcrypt and all) and
// returns its session cookie. The account-security routes re-verify the
// password, so the seeded `$2a$12$notahash` of newUser is not usable here.
func registered(t *testing.T, e *env, name, password string) *http.Cookie {
	t.Helper()
	r := e.doJSON("POST", "/api/auth/register", map[string]string{"username": name, "password": password})
	if r.code != 201 {
		t.Fatalf("register %s: %d %s", name, r.code, r.body)
	}
	c := e.sessionCookie(r)
	if c == nil {
		t.Fatalf("register %s set no cookie", name)
	}
	return c
}

// PATCH /api/auth/password (§20 erratum 108): the current password is
// re-verified, the hash rotates, every OTHER session dies, and the caller's
// own survives.
func TestChangePassword(t *testing.T) {
	e := newEnv(t)
	here := registered(t, e, "carol", "correct horse")
	// A second and third browser of the same account, both live.
	laptop := loginAs(t, e, "carol", "correct horse")
	phone := loginAs(t, e, "carol", "correct horse")

	// The wrong current password is 401 and changes nothing.
	r := e.doJSON("PATCH", "/api/auth/password",
		map[string]string{"current_password": "not it", "new_password": "a whole new thing"}, withCookie(here))
	if r.code != 401 || r.errCode() != "invalid_credentials" {
		t.Fatalf("wrong current: %d %s", r.code, r.body)
	}
	if r = e.do("GET", "/api/auth/me", nil, withCookie(laptop)); r.code != 200 {
		t.Fatalf("a failed change must not revoke anything: %d", r.code)
	}
	// A short new password is 400, and again nothing changes.
	r = e.doJSON("PATCH", "/api/auth/password",
		map[string]string{"current_password": "correct horse", "new_password": "short"}, withCookie(here))
	if r.code != 400 || r.errCode() != "invalid_request" {
		t.Fatalf("short new: %d %s", r.code, r.body)
	}
	// The same password twice is refused as a no-op that would still log the
	// other browsers out.
	r = e.doJSON("PATCH", "/api/auth/password",
		map[string]string{"current_password": "correct horse", "new_password": "correct horse"}, withCookie(here))
	if r.code != 400 {
		t.Fatalf("same password: %d %s", r.code, r.body)
	}

	// The real change.
	r = e.doJSON("PATCH", "/api/auth/password",
		map[string]string{"current_password": "correct horse", "new_password": "a whole new thing"}, withCookie(here))
	if r.code != 200 {
		t.Fatalf("change: %d %s", r.code, r.body)
	}
	if n, _ := r.json["sessions_revoked"].(float64); n != 2 {
		t.Fatalf("sessions_revoked = %v, want 2 (%s)", r.json["sessions_revoked"], r.body)
	}
	// The caller's own cookie still works; the other two are dead.
	if r = e.do("GET", "/api/auth/me", nil, withCookie(here)); r.code != 200 {
		t.Fatalf("the changing browser must stay signed in: %d", r.code)
	}
	for name, c := range map[string]*http.Cookie{"laptop": laptop, "phone": phone} {
		if r = e.do("GET", "/api/auth/me", nil, withCookie(c)); r.code != 401 {
			t.Errorf("%s: %d, want 401 (the stolen cookie must stop resolving)", name, r.code)
		}
	}
	// The old password no longer logs in; the new one does.
	if r = e.doJSON("POST", "/api/auth/login",
		map[string]string{"username": "carol", "password": "correct horse"}); r.code != 401 {
		t.Errorf("old password: %d, want 401", r.code)
	}
	if r = e.doJSON("POST", "/api/auth/login",
		map[string]string{"username": "carol", "password": "a whole new thing"}); r.code != 200 {
		t.Errorf("new password: %d %s", r.code, r.body)
	}
	// A change never echoes a credential, in either direction.
	if strings.Contains(string(r.body), "a whole new thing") {
		t.Error("the login response echoed the password")
	}
}

// loginAs opens another browser on an existing account.
func loginAs(t *testing.T, e *env, name, password string) *http.Cookie {
	t.Helper()
	r := e.doJSON("POST", "/api/auth/login", map[string]string{"username": name, "password": password})
	if r.code != 200 {
		t.Fatalf("login %s: %d %s", name, r.code, r.body)
	}
	c := e.sessionCookie(r)
	if c == nil {
		t.Fatalf("login %s set no cookie", name)
	}
	return c
}

// The password route's auth class: session only, CSRF-guarded, JSON only, and
// held by the AUTH limiter.
func TestChangePasswordAuthClass(t *testing.T) {
	e := newEnv(t)
	here := registered(t, e, "carol", "correct horse")
	u, err := e.db.GetUserByUsername(context.Background(), "carol")
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]string{"current_password": "correct horse", "new_password": "a whole new thing"}

	if r := e.doJSON("PATCH", "/api/auth/password", body); r.code != 401 || r.errCode() != "unauthorized" {
		t.Errorf("anonymous: %d", r.code)
	}
	// A token — of ANY scope — is refused: a capture credential must never be
	// able to rotate the password that would revoke it.
	for _, scopes := range [][]string{{auth.ScopeUpload}, {auth.ScopeRead}, {auth.ScopeUpload, auth.ScopeRead}} {
		tok := e.newToken(u, scopes...)
		r := e.doJSON("PATCH", "/api/auth/password", body, withBearer(tok))
		if r.code != 403 || r.errCode() != "token_not_allowed" {
			t.Errorf("token %v: %d %s", scopes, r.code, r.body)
		}
	}
	// CSRF: a cross-site Origin is rejected before anything happens.
	r := e.doJSON("PATCH", "/api/auth/password", body, withCookie(here), withHeader("Origin", "https://evil.example"))
	if r.code != 403 || r.errCode() != "csrf" {
		t.Errorf("cross-site: %d %s", r.code, r.body)
	}
	// Non-JSON is 415, so an HTML form cannot post here cross-origin.
	if r = e.do("PATCH", "/api/auth/password", strings.NewReader("x"), withCookie(here)); r.code != 415 {
		t.Errorf("non-json: %d", r.code)
	}
	// And every one of those refusals left the password alone.
	if r = e.doJSON("POST", "/api/auth/login", map[string]string{"username": "carol", "password": "correct horse"}); r.code != 200 {
		t.Errorf("the password must be unchanged: %d %s", r.code, r.body)
	}
}

// The password route shares the per-IP AUTH limiter with login and register
// (§14): it takes a password and runs a bcrypt compare, so it must not be a
// free oracle for a thief who already holds the cookie.
func TestChangePasswordUsesTheAuthLimiter(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.RateAuthPerMin = 2 })
	here := registered(t, e, "carol", "correct horse") // spends one token
	body := map[string]string{"current_password": "guess", "new_password": "a whole new thing"}
	if r := e.doJSON("PATCH", "/api/auth/password", body, withCookie(here)); r.code != 401 {
		t.Fatalf("first guess: %d %s", r.code, r.body)
	}
	r := e.doJSON("PATCH", "/api/auth/password", body, withCookie(here))
	if r.code != 429 || r.errCode() != "rate_limited" {
		t.Fatalf("third auth-class request from this IP: %d %s", r.code, r.body)
	}
}

// GET/DELETE /api/auth/sessions (§20 erratum 107).
func TestSessionListAndRevoke(t *testing.T) {
	e := newEnv(t)
	here := registered(t, e, "carol", "correct horse")
	laptop := loginAs(t, e, "carol", "correct horse")
	// Another account's session must never appear in this list.
	stranger := registered(t, e, "stranger", "correct horse")

	r := e.do("GET", "/api/auth/sessions", nil, withCookie(here))
	if r.code != 200 {
		t.Fatalf("list: %d %s", r.code, r.body)
	}
	rows, _ := r.json["sessions"].([]any)
	if len(rows) != 2 {
		t.Fatalf("sessions = %v, want 2 (%s)", r.json["sessions"], r.body)
	}
	current := 0
	for _, row := range rows {
		m, _ := row.(map[string]any)
		for _, key := range []string{"created_at", "last_seen_at", "expires_at", "current", "user_agent"} {
			if _, ok := m[key]; !ok {
				t.Errorf("session row lacks %q: %v", key, m)
			}
		}
		// No handle of any kind reaches the wire — the hash IS the credential's
		// shadow (§20 erratum 107).
		for _, forbidden := range []string{"id", "token", "token_hash", "hash"} {
			if _, ok := m[forbidden]; ok {
				t.Errorf("session row carries %q: %v", forbidden, m)
			}
		}
		if b, _ := m["current"].(bool); b {
			current++
		}
	}
	if current != 1 {
		t.Errorf("exactly one row must be current, got %d", current)
	}
	// The response is principal-dependent JSON: never storable by a cache.
	if cc := r.hdr.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q", cc)
	}
	if !strings.Contains(r.hdr.Get("Vary"), "Cookie") {
		t.Errorf("Vary = %q", r.hdr.Get("Vary"))
	}
	if strings.Contains(string(r.body), here.Value) || strings.Contains(string(r.body), laptop.Value) {
		t.Fatal("the session list echoed a raw cookie value")
	}

	// Log out everywhere else.
	r = e.do("DELETE", "/api/auth/sessions", nil, withCookie(here), withJSON())
	if r.code != 200 {
		t.Fatalf("revoke: %d %s", r.code, r.body)
	}
	if n, _ := r.json["sessions_revoked"].(float64); n != 1 {
		t.Fatalf("sessions_revoked = %v, want 1", r.json["sessions_revoked"])
	}
	if r = e.do("GET", "/api/auth/me", nil, withCookie(here)); r.code != 200 {
		t.Errorf("the calling browser must stay signed in: %d", r.code)
	}
	if r = e.do("GET", "/api/auth/me", nil, withCookie(laptop)); r.code != 401 {
		t.Errorf("the other browser: %d, want 401", r.code)
	}
	if r = e.do("GET", "/api/auth/me", nil, withCookie(stranger)); r.code != 200 {
		t.Errorf("another ACCOUNT's session must survive: %d", r.code)
	}
	// A second revoke is a clean zero, not an error.
	r = e.do("DELETE", "/api/auth/sessions", nil, withCookie(here), withJSON())
	if r.code != 200 {
		t.Fatalf("second revoke: %d %s", r.code, r.body)
	}
	if n, _ := r.json["sessions_revoked"].(float64); n != 0 {
		t.Errorf("second revoke removed %v", r.json["sessions_revoked"])
	}
}

func TestSessionsAuthClass(t *testing.T) {
	e := newEnv(t)
	here := registered(t, e, "carol", "correct horse")
	u, err := e.db.GetUserByUsername(context.Background(), "carol")
	if err != nil {
		t.Fatal(err)
	}
	read := e.newToken(u, auth.ScopeRead)
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		if r := e.do(m, "/api/auth/sessions", nil, withJSON()); r.code != 401 {
			t.Errorf("%s anonymous: %d", m, r.code)
		}
		// Not even a read token: the session list is the account's own
		// security surface, and erratum 111 widened exactly two READ routes.
		r := e.do(m, "/api/auth/sessions", nil, withBearer(read), withJSON())
		if r.code != 403 || r.errCode() != "token_not_allowed" {
			t.Errorf("%s read token: %d %s", m, r.code, r.body)
		}
	}
	r := e.do("DELETE", "/api/auth/sessions", nil, withCookie(here), withJSON(),
		withHeader("Sec-Fetch-Site", "cross-site"))
	if r.code != 403 || r.errCode() != "csrf" {
		t.Errorf("cross-site DELETE: %d %s", r.code, r.body)
	}
}

// REGISTRATION=first (§20 erratum 106): open while the users table is empty,
// closed the moment an account exists.
func TestRegistrationFirstClosesAfterTheFirstAccount(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.Registration = config.RegistrationFirst })
	r := e.doJSON("POST", "/api/auth/register", map[string]string{"username": "carol", "password": "correct horse"})
	if r.code != 201 {
		t.Fatalf("the FIRST account must be accepted: %d %s", r.code, r.body)
	}
	r = e.doJSON("POST", "/api/auth/register", map[string]string{"username": "friend", "password": "correct horse"})
	if r.code != 403 || r.errCode() != "registration_closed" {
		t.Fatalf("the second: %d %s, want 403 registration_closed", r.code, r.body)
	}
	// An account made by the operator (the CLI's `users create`, modelled here
	// as a direct insert) does not reopen the door either.
	if _, err := e.db.CreateUser(context.Background(), "u2", "byhand", "$2a$12$notahash", e.now()); err != nil {
		t.Fatal(err)
	}
	r = e.doJSON("POST", "/api/auth/register", map[string]string{"username": "another", "password": "correct horse"})
	if r.code != 403 {
		t.Fatalf("still closed: %d %s", r.code, r.body)
	}
	// The zero value of the mode is `first`, so a Config built by hand behaves
	// like a shipped one rather than like `open`.
	e2 := newEnv(t, func(d *Deps) { d.Cfg.Registration = "" })
	if r = e2.doJSON("POST", "/api/auth/register", map[string]string{"username": "carol", "password": "correct horse"}); r.code != 201 {
		t.Fatalf("zero mode, empty table: %d %s", r.code, r.body)
	}
	if r = e2.doJSON("POST", "/api/auth/register", map[string]string{"username": "friend", "password": "correct horse"}); r.code != 403 {
		t.Fatalf("zero mode, one user: %d %s", r.code, r.body)
	}
}

// A BURST of concurrent registrations against an empty table yields exactly
// ONE account (§20 erratum 106).
//
// This is the guarantee `first` exists to make, and it is not one a
// check-then-insert can keep: `registrationOpen` reads COUNT(*), the handler
// then spends a couple of hundred milliseconds in bcrypt, and only after that
// inserts — a window wide enough that every request in a burst reads zero and
// every one of them succeeds, with nothing but the per-IP auth limiter
// bounding how many. The emptiness test therefore rides inside
// `db.CreateFirstUser`'s INSERT, and the losers get the same 403
// `registration_closed` a late registration has always got.
func TestRegistrationFirstIsAtomicUnderABurst(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.Registration = config.RegistrationFirst })
	const n = 8
	codes := make([]int, n)
	errs := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r := e.doJSON("POST", "/api/auth/register", map[string]string{
				"username": fmt.Sprintf("racer%d", i), "password": "bootstrap-pass",
			})
			codes[i], errs[i] = r.code, r.errCode()
		}(i)
	}
	close(start) // every goroutine is parked on this: they go together
	wg.Wait()
	created := 0
	for i, code := range codes {
		switch {
		case code == 201:
			created++
		case code == 403 && errs[i] == "registration_closed":
		default:
			t.Errorf("racer%d: %d %s, want 201 or 403 registration_closed", i, code, errs[i])
		}
	}
	if created != 1 {
		t.Errorf("%d of %d concurrent registrations were accepted against an EMPTY users table; "+
			"`first` promises exactly one", created, n)
	}
	got, err := e.db.CountUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("users table holds %d rows, want 1", got)
	}
}

// REGISTRATION=open stays open however many accounts exist — the operator's
// deliberate "my friends are signing up this week" setting.
func TestRegistrationOpenStaysOpen(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.Registration = config.RegistrationOpen })
	for _, name := range []string{"one", "two", "three"} {
		if r := e.doJSON("POST", "/api/auth/register",
			map[string]string{"username": name, "password": "correct horse"}); r.code != 201 {
			t.Fatalf("%s: %d %s", name, r.code, r.body)
		}
	}
}

// A successful registration is on the record with the resolved client
// address (ROADMAP #21): on a public origin this is the only route that mints a
// credential out of nothing.
func TestRegistrationIsLogged(t *testing.T) {
	e := newEnv(t)
	_, line := e.logged("POST", "/api/auth/register",
		strings.NewReader(`{"username":"carol","password":"correct horse"}`), withJSON())
	if !strings.Contains(line, "registered=carol") {
		t.Errorf("the access log does not name the new account: %s", line)
	}
	if !strings.Contains(line, "client_ip=192.0.2.10") {
		t.Errorf("the access log does not carry the resolved address: %s", line)
	}
	if strings.Contains(line, "correct horse") {
		t.Fatalf("the access log echoed the password: %s", line)
	}
}

// Every authentication REFUSAL carries the resolved client address too, not
// only the 201 (ROADMAP #21, extended). The access log's own `remote` field is
// the raw socket address, which behind the TRUSTED_PROXIES deployment this
// project documents is the proxy — so without this field a scanner probing
// /register or /login is unattributable.
func TestAuthRefusalsAreLoggedWithTheResolvedAddress(t *testing.T) {
	closed := newEnv(t, func(d *Deps) { d.Cfg.Registration = config.RegistrationClosed })
	_, line := closed.logged("POST", "/api/auth/register",
		strings.NewReader(`{"username":"carol","password":"correct horse"}`), withJSON())
	if !strings.Contains(line, "registration=closed") || !strings.Contains(line, "client_ip=192.0.2.10") {
		t.Errorf("a refused registration does not name the caller: %s", line)
	}

	e := newEnv(t)
	e.newUser("carol")
	for name, body := range map[string]string{
		"unknown_user": `{"username":"nobody","password":"correct horse"}`,
		"bad_password": `{"username":"carol","password":"wrong horse"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r, line := e.logged("POST", "/api/auth/login", strings.NewReader(body), withJSON())
			if r.code != 401 {
				t.Fatalf("login = %d %s", r.code, r.body)
			}
			if !strings.Contains(line, "login="+name) || !strings.Contains(line, "client_ip=192.0.2.10") {
				t.Errorf("a refused login does not name the caller: %s", line)
			}
			if strings.Contains(line, "horse") {
				t.Fatalf("the access log echoed the password: %s", line)
			}
		})
	}
}

// The read scope (§20 errata 111–112): what it opens, and everything it does
// not. This is the matrix the E2E runs live.
func TestReadScopeMatrix(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("carol")
	clip := e.seedClip(u, clipOpts{title: "mine"})
	read := e.newToken(u, auth.ScopeRead)
	up := e.newToken(u, auth.ScopeUpload)
	both := e.newToken(u, auth.ScopeUpload, auth.ScopeRead)

	// GET /api/clips: session yes, read yes, upload-only no.
	for name, tc := range map[string]struct {
		opt  reqOpt
		want int
	}{
		"session":     {withCookie(cookie), 200},
		"read token":  {withBearer(read), 200},
		"both token":  {withBearer(both), 200},
		"upload only": {withBearer(up), 403},
		"anonymous":   {func(*http.Request) {}, 401},
	} {
		r := e.do("GET", "/api/clips", nil, tc.opt)
		if r.code != tc.want {
			t.Errorf("GET /api/clips as %s: %d, want %d (%s)", name, r.code, tc.want, r.body)
		}
		if tc.want == 403 && r.errCode() != "token_not_allowed" {
			t.Errorf("GET /api/clips as %s: error %q, want token_not_allowed", name, r.errCode())
		}
	}
	// GET /api/collections is the other widened read.
	if r := e.do("GET", "/api/collections", nil, withBearer(read)); r.code != 200 {
		t.Errorf("GET /api/collections with a read token: %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/api/collections", nil, withBearer(up)); r.code != 403 {
		t.Errorf("GET /api/collections with an upload token: %d %s", r.code, r.body)
	}
	// …and NOTHING else. Every owner mutation, and the owner-only reads that
	// erratum 19 kept for sessions, still refuse a read token.
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/clips/" + clip.ID + "/retry"},
		{"PATCH", "/api/clips/" + clip.ID},
		{"DELETE", "/api/clips/" + clip.ID},
		{"POST", "/api/clips/" + clip.ID + "/touch"},
		{"GET", "/api/clips/" + clip.ID + "/original"},
		{"GET", "/api/clips/" + clip.ID + "/trim"},
		{"POST", "/api/collections"},
		{"POST", "/api/auth/tokens"},
		{"GET", "/api/auth/tokens"},
	} {
		r := e.do(tc.method, tc.path, strings.NewReader(`{}`), withBearer(read), withJSON())
		if r.code != 403 || r.errCode() != "token_not_allowed" {
			t.Errorf("%s %s with a read token: %d %s", tc.method, tc.path, r.code, r.body)
		}
	}
	// A read token may not upload: the upload scope is a separate grant.
	if r := e.do("POST", "/api/upload", strings.NewReader("x"), withBearer(read),
		withHeader("Content-Type", "multipart/form-data; boundary=zz")); r.code != 403 || r.errCode() != "insufficient_scope" {
		t.Errorf("POST /api/upload with a read token: %d %s", r.code, r.body)
	}

	// §20 erratum 112: the owner-private fields. A read token of the OWNER
	// sees them on the owner's own clip; an upload-only token does not.
	r := e.do("GET", "/api/clips/"+clip.ID, nil, withBearer(read))
	if r.code != 200 || !r.has("views") {
		t.Errorf("read token on the owner's clip: %d has_views=%v (%s)", r.code, r.has("views"), r.body)
	}
	r = e.do("GET", "/api/clips/"+clip.ID, nil, withBearer(up))
	if r.code != 200 || r.has("views") {
		t.Errorf("upload token must see the PUBLIC DTO: %d has_views=%v", r.code, r.has("views"))
	}
	// …and a read token belonging to somebody ELSE sees the public DTO too.
	other, _ := e.newUser("stranger")
	if r = e.do("GET", "/api/clips/"+clip.ID, nil, withBearer(e.newToken(other, auth.ScopeRead))); r.has("views") {
		t.Errorf("a stranger's read token unlocked the owner's view count: %s", r.body)
	}
}

// Minting with scopes (§20 erratum 111): the default is unchanged, the set is
// validated, and a read-only token gets no ShareX file.
func TestMintTokenScopes(t *testing.T) {
	e := newEnv(t)
	_, cookie := e.newUser("carol")

	// No `scopes` key at all → ["upload"], exactly as before.
	r := e.doJSON("POST", "/api/auth/tokens", map[string]any{"name": "sharex"}, withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("default mint: %d %s", r.code, r.body)
	}
	if got := scopesOf(r); len(got) != 1 || got[0] != "upload" {
		t.Fatalf("default scopes = %v, want [upload]", got)
	}
	if _, ok := r.json["sxcu"]; !ok || r.json["sxcu"] == nil {
		t.Error("an upload token must still carry its .sxcu (§20 erratum 59)")
	}

	// Read only: no ShareX destination, because it would 403 on the first
	// screenshot.
	r = e.doJSON("POST", "/api/auth/tokens",
		map[string]any{"name": "backup", "scopes": []string{"read"}}, withCookie(cookie))
	if r.code != 201 {
		t.Fatalf("read mint: %d %s", r.code, r.body)
	}
	if got := scopesOf(r); len(got) != 1 || got[0] != "read" {
		t.Fatalf("read scopes = %v", got)
	}
	if r.json["sxcu"] != nil {
		t.Errorf("a read-only token must not carry a .sxcu: %s", r.body)
	}

	// Both, in the canonical order however they were sent, duplicates gone.
	r = e.doJSON("POST", "/api/auth/tokens",
		map[string]any{"name": "everything", "scopes": []string{"read", "upload", "read"}}, withCookie(cookie))
	if got := scopesOf(r); len(got) != 2 || got[0] != "upload" || got[1] != "read" {
		t.Fatalf("both scopes = %v, want [upload read]", got)
	}

	// An unknown scope, and an empty set, are 400 — never stored and ignored.
	for _, bad := range []any{[]string{"admin"}, []string{}, []string{"upload", "root"}} {
		r = e.doJSON("POST", "/api/auth/tokens", map[string]any{"name": "bad", "scopes": bad}, withCookie(cookie))
		if r.code != 400 || r.errCode() != "invalid_request" {
			t.Errorf("scopes %v: %d %s", bad, r.code, r.body)
		}
	}

	// The list route renders what was minted.
	r = e.do("GET", "/api/auth/tokens", nil, withCookie(cookie))
	if r.code != 200 || !strings.Contains(string(r.body), `"read"`) {
		t.Fatalf("token list: %d %s", r.code, r.body)
	}
}

func scopesOf(r resp) []string {
	raw, _ := r.json["scopes"].([]any)
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		v, _ := s.(string)
		out = append(out, v)
	}
	return out
}

// The §20 erratum 105 ceiling, end to end through the Resolver: a session is
// dead SESSION_MAX_TTL after it was CREATED, however hard it has been used —
// which is the whole point, since sliding alone made an actively used stolen
// cookie immortal.
func TestSessionCeilingEndsAnActiveSession(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(d *Deps) {
		d.Cfg.SessionTTL = time.Hour
		d.Cfg.SessionMaxTTL = 2 * time.Hour
	})
	u, _ := e.newUser("carol") // newUser opens one session of its own
	before, err := e.db.ListSessions(ctx, u.ID, e.now())
	if err != nil {
		t.Fatal(err)
	}

	// A session created just now resolves, and its cookie carries the
	// SESSION_TTL it was given (below the ceiling, so nothing is clamped).
	raw, ttl, err := auth.CreateSession(ctx, e.db, e.cfg, u.ID, "ua", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ttl != time.Hour {
		t.Fatalf("fresh cookie ttl = %s, want 1h", ttl)
	}
	if r := e.do("GET", "/api/auth/me", nil,
		withCookie(&http.Cookie{Name: e.cfg.SessionCookie, Value: raw})); r.code != 200 {
		t.Fatalf("fresh session: %d", r.code)
	}

	// A session created THREE hours ago on the same two-hour ceiling: the row
	// is present and its ceiling has passed, so the Resolver refuses it.
	old, _, err := auth.CreateSession(ctx, e.db, e.cfg, u.ID, "ua", time.Now().Add(-3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/api/auth/me", nil,
		withCookie(&http.Cookie{Name: e.cfg.SessionCookie, Value: old})); r.code != 401 {
		t.Fatalf("a session past its ceiling: %d, want 401", r.code)
	}
	// It is the CEILING doing that and not a sweep: the row is still in the
	// table, and it simply no longer counts as live. Two sessions were opened
	// after `before` was read (the fresh one and the expired one); only the
	// fresh one shows up.
	rows, err := e.db.ListSessions(ctx, u.ID, e.now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(before)+1 {
		t.Fatalf("live sessions = %d, want %d (only the fresh one was added)", len(rows), len(before)+1)
	}

	// With SESSION_MAX_TTL BELOW SESSION_TTL, even a brand-new session is
	// short — and the cookie says so rather than outliving the row it names.
	e2 := newEnv(t, func(d *Deps) {
		d.Cfg.SessionTTL = 720 * time.Hour
		d.Cfg.SessionMaxTTL = time.Hour
	})
	u2, _ := e2.newUser("carol")
	if _, ttl, err = auth.CreateSession(ctx, e2.db, e2.cfg, u2.ID, "ua", time.Now()); err != nil {
		t.Fatal(err)
	}
	if ttl != time.Hour {
		t.Fatalf("clamped cookie ttl = %s, want 1h", ttl)
	}
}
