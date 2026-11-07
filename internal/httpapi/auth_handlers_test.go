package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"clippet/internal/config"
)

func TestRegisterLoginLogoutMe(t *testing.T) {
	e := newEnv(t)

	// Register: 201, user DTO, session cookie with the §14 flags.
	r := e.doJSON("POST", "/api/auth/register", map[string]string{"username": "Carol", "password": "correct horse"})
	if r.code != 201 {
		t.Fatalf("register: %d %s", r.code, r.body)
	}
	user, _ := r.json["user"].(map[string]any)
	if user["username"] != "Carol" || user["id"] == "" || user["created_at"] == "" {
		t.Fatalf("register body: %s", r.body)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("register Cache-Control = %q", r.hdr.Get("Cache-Control"))
	}
	c := e.sessionCookie(r)
	if c == nil || c.Value == "" || !c.HttpOnly || c.Path != "/" || c.SameSite != http.SameSiteLaxMode || c.Secure || c.MaxAge != 3600 {
		t.Fatalf("session cookie: %+v", c)
	}

	// me via the cookie.
	r = e.do("GET", "/api/auth/me", nil, withCookie(c))
	if r.code != 200 || r.str("via") != "session" {
		t.Fatalf("me: %d %s", r.code, r.body)
	}
	if u := r.json["user"].(map[string]any); u["username"] != "Carol" {
		t.Fatalf("me user: %v", u)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatal("me must be no-store")
	}

	// Anonymous me → 401.
	if r = e.do("GET", "/api/auth/me", nil); r.code != 401 || r.errCode() != "unauthorized" {
		t.Fatalf("anonymous me: %d %s", r.code, r.body)
	}

	// Duplicate username (case-insensitive) → 409.
	r = e.doJSON("POST", "/api/auth/register", map[string]string{"username": "carol", "password": "another pass"})
	if r.code != 409 || r.errCode() != "username_taken" {
		t.Fatalf("dup register: %d %s", r.code, r.body)
	}

	// Login: wrong password and unknown user are indistinguishable 401s and
	// the unknown-user path runs the dummy bcrypt compare.
	before := dummyCompares.Load()
	bad := e.doJSON("POST", "/api/auth/login", map[string]string{"username": "carol", "password": "wrong password"})
	unknown := e.doJSON("POST", "/api/auth/login", map[string]string{"username": "nobody", "password": "wrong password"})
	if bad.code != 401 || unknown.code != 401 || string(bad.body) != string(unknown.body) || bad.errCode() != "invalid_credentials" {
		t.Fatalf("login failures: %d %s / %d %s", bad.code, bad.body, unknown.code, unknown.body)
	}
	if dummyCompares.Load() != before+1 {
		t.Fatalf("dummy compares = %d, want %d", dummyCompares.Load(), before+1)
	}
	if e.sessionCookie(bad) != nil {
		t.Fatal("failed login set a cookie")
	}

	// Login success: case-insensitive username, fresh cookie.
	r = e.doJSON("POST", "/api/auth/login", map[string]string{"username": "CAROL", "password": "correct horse"})
	if r.code != 200 || r.json["user"].(map[string]any)["username"] != "Carol" {
		t.Fatalf("login: %d %s", r.code, r.body)
	}
	c2 := e.sessionCookie(r)
	if c2 == nil || c2.Value == c.Value {
		t.Fatal("login did not issue a fresh session")
	}

	// Logout: 204, cookie cleared, the session row is gone.
	r = e.do("POST", "/api/auth/logout", nil, withCookie(c2))
	if r.code != 204 {
		t.Fatalf("logout: %d %s", r.code, r.body)
	}
	if cleared := e.sessionCookie(r); cleared == nil || cleared.MaxAge != -1 || cleared.Value != "" {
		t.Fatalf("logout cookie: %+v", cleared)
	}
	if r = e.do("GET", "/api/auth/me", nil, withCookie(c2)); r.code != 401 {
		t.Fatalf("me after logout: %d", r.code)
	}
	// The first session is still valid.
	if r = e.do("GET", "/api/auth/me", nil, withCookie(c)); r.code != 200 {
		t.Fatalf("me with first session: %d", r.code)
	}
	// Logout needs a session; anonymous → 401.
	if r = e.do("POST", "/api/auth/logout", nil); r.code != 401 {
		t.Fatalf("anonymous logout: %d", r.code)
	}
}

func TestRegisterValidation(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name string
		body any
		ct   string
		code int
		err  string
	}{
		{"short username", map[string]string{"username": "ab", "password": "long enough"}, "application/json", 400, "invalid_request"},
		{"bad username chars", map[string]string{"username": "car-ol", "password": "long enough"}, "application/json", 400, "invalid_request"},
		{"short password", map[string]string{"username": "carol", "password": "short"}, "application/json", 400, "invalid_request"},
		{"73-byte password", map[string]string{"username": "carol", "password": strings.Repeat("a", 73)}, "application/json", 400, "invalid_request"},
		{"not an object", "[1,2]", "application/json", 400, "invalid_request"},
		{"trailing garbage", `{"username":"carol","password":"long enough"} x`, "application/json", 400, "invalid_request"},
		{"empty body", "", "application/json", 400, "invalid_request"},
		{"form encoded", "username=carol&password=longenough", "application/x-www-form-urlencoded", 415, "unsupported_media_type"},
		{"no content type", `{"username":"carol","password":"long enough"}`, "", 415, "unsupported_media_type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r resp
			if tc.ct == "application/json" {
				r = e.doJSON("POST", "/api/auth/register", tc.body)
			} else {
				opts := []reqOpt{}
				if tc.ct != "" {
					opts = append(opts, withHeader("Content-Type", tc.ct))
				}
				r = e.do("POST", "/api/auth/register", strings.NewReader(tc.body.(string)), opts...)
			}
			if r.code != tc.code || r.errCode() != tc.err {
				t.Fatalf("got %d %s, want %d %s", r.code, r.body, tc.code, tc.err)
			}
		})
	}
	// 72 bytes is the maximum accepted.
	r := e.doJSON("POST", "/api/auth/register", map[string]string{"username": "carol", "password": strings.Repeat("a", 72)})
	if r.code != 201 {
		t.Fatalf("72-byte password: %d %s", r.code, r.body)
	}
	// Login with a non-JSON body is 415 too; malformed JSON is 400.
	if r = e.do("POST", "/api/auth/login", strings.NewReader("x")); r.code != 415 {
		t.Fatalf("login non-json: %d", r.code)
	}
	if r = e.doJSON("POST", "/api/auth/login", `{"username":""}`); r.code != 400 {
		t.Fatalf("login empty: %d", r.code)
	}
}

func TestRegistrationClosed(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.Registration = config.RegistrationClosed })
	r := e.doJSON("POST", "/api/auth/register", map[string]string{"username": "carol", "password": "correct horse"})
	if r.code != 403 || r.errCode() != "registration_closed" {
		t.Fatalf("closed: %d %s", r.code, r.body)
	}
}

func TestAuthRateLimit(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.RateAuthPerMin = 2 })
	body := map[string]string{"username": "nobody", "password": "wrong password"}
	for i := 0; i < 2; i++ {
		if r := e.doJSON("POST", "/api/auth/login", body); r.code != 401 {
			t.Fatalf("attempt %d: %d", i, r.code)
		}
	}
	r := e.doJSON("POST", "/api/auth/login", body)
	if r.code != 429 || r.errCode() != "rate_limited" || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("limited: %d %s Retry-After=%q", r.code, r.body, r.hdr.Get("Retry-After"))
	}
	// Register shares the same per-IP auth limiter (§14): the exhausted IP
	// is refused there too, before any handler work.
	r = e.doJSON("POST", "/api/auth/register", map[string]string{"username": "carol", "password": "long enough"})
	if r.code != 429 || r.errCode() != "rate_limited" || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("register limited: %d %s Retry-After=%q", r.code, r.body, r.hdr.Get("Retry-After"))
	}
	// Another IP is unaffected.
	r = e.doJSON("POST", "/api/auth/login", body, func(req *http.Request) { req.RemoteAddr = "198.51.100.1:1" })
	if r.code != 401 {
		t.Fatalf("other ip: %d", r.code)
	}
}

// TestAuthRateLimitIPv6Slash64 pins §20 erratum 292 at the route: two IPv6
// addresses in one /64 spend the same auth bucket -- a machine rotating its
// interface identifier gets no fresh budget -- and a neighbouring /64 is
// another client. IPv4 is keyed as before.
func TestAuthRateLimitIPv6Slash64(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Cfg.RateAuthPerMin = 2 })
	body := map[string]string{"username": "nobody", "password": "wrong password"}
	for i := 0; i < 2; i++ {
		if r := e.doJSON("POST", "/api/auth/login", body, withRemote("[2001:db8:1:2::1]:1")); r.code != 401 {
			t.Fatalf("attempt %d: %d", i, r.code)
		}
	}
	r := e.doJSON("POST", "/api/auth/login", body, withRemote("[2001:db8:1:2:dead:beef:0:9]:1"))
	if r.code != 429 || r.errCode() != "rate_limited" {
		t.Fatalf("same /64, different address: %d %s, want 429 (one bucket per /64)", r.code, r.body)
	}
	if r = e.doJSON("POST", "/api/auth/login", body, withRemote("[2001:db8:1:3::1]:1")); r.code != 401 {
		t.Fatalf("other /64: %d, want 401 (its own bucket)", r.code)
	}
	if r = e.doJSON("POST", "/api/auth/login", body, withRemote("192.0.2.11:1")); r.code != 401 {
		t.Fatalf("ipv4 neighbour of the default peer: %d, want 401 (IPv4 is keyed whole)", r.code)
	}
}

func TestBearerOnMeAndCSRFOnLogout(t *testing.T) {
	e := newEnv(t)
	u, cookie := e.newUser("carol")
	tok := e.newToken(u, "upload")

	r := e.do("GET", "/api/auth/me", nil, withBearer(tok))
	if r.code != 200 || r.str("via") != "token" {
		t.Fatalf("me via token: %d %s", r.code, r.body)
	}
	// Bearer wins over the cookie.
	other, otherCookie := e.newUser("other")
	r = e.do("GET", "/api/auth/me", nil, withBearer(tok), withCookie(otherCookie))
	if r.code != 200 || r.json["user"].(map[string]any)["id"] != u.ID || r.str("via") != "token" {
		t.Fatalf("precedence: %s", r.body)
	}
	_ = other
	// A token may not log out (session-only route).
	if r = e.do("POST", "/api/auth/logout", nil, withBearer(tok)); r.code != 403 || r.errCode() != "token_not_allowed" {
		t.Fatalf("token logout: %d %s", r.code, r.body)
	}
	// Cookie + foreign Origin → 403 csrf; same Origin passes.
	r = e.do("POST", "/api/auth/logout", nil, withCookie(cookie), withHeader("Origin", "https://evil.example"))
	if r.code != 403 || r.errCode() != "csrf" {
		t.Fatalf("csrf: %d %s", r.code, r.body)
	}
	r = e.do("POST", "/api/auth/logout", nil, withCookie(cookie), withHeader("Origin", testOrigin))
	if r.code != 204 {
		t.Fatalf("same-origin logout: %d %s", r.code, r.body)
	}
}

func TestSecureCookieOnHTTPS(t *testing.T) {
	e := newEnv(t, func(d *Deps) {
		d.Cfg.PublicBaseURL = mustURL("https://clips.example")
		d.URLs = urlsFor("https://clips.example")
	})
	r := e.doJSON("POST", "/api/auth/register", map[string]string{"username": "carol", "password": "correct horse"})
	if c := e.sessionCookie(r); r.code != 201 || c == nil || !c.Secure {
		t.Fatalf("https cookie: %d %+v", r.code, c)
	}
}
