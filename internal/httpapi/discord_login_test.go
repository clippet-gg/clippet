package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/notify"
)

// "Sign in with Discord" (ROADMAP #44, §20 erratum 302): the callback matrix,
// the link and unlink routes, the OFF state, the cookie's inheritance and the
// secrets' absence — everything past the exchange, which internal/notify's
// own tests prove against httptest.

const (
	testLoginClientID = "876543210987654321"
	testLoginSecret   = "aBcDeFgHiJkLmNoPqRsTuVwXyZ012345"
	testDiscordUser   = "111111111111111111"
	testCode          = "the-code-discord-minted"
)

// fakeLogin is httpapi.IdentityProvider with a script: what the next exchange
// answers, and a record of what it was asked.
type fakeLogin struct {
	mu    sync.Mutex
	calls []fakeCall
	id    notify.DiscordIdentity
	err   error
}

type fakeCall struct{ code, verifier, redirectURI string }

func (f *fakeLogin) Identify(_ context.Context, code, verifier, redirectURI string) (notify.DiscordIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{code, verifier, redirectURI})
	if f.err != nil {
		return notify.DiscordIdentity{}, f.err
	}
	return f.id, nil
}

func (f *fakeLogin) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeLogin) last() fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

// testLoginConfig builds a real config.DiscordLogin through config.Load, so
// no test here passes on a value the loader would refuse.
func testLoginConfig(t *testing.T) config.DiscordLogin {
	t.Helper()
	env := map[string]string{
		"PUBLIC_BASE_URL":                  testOrigin,
		"S3_ENDPOINT":                      "http://minio:9000",
		"S3_BUCKET":                        "b",
		"S3_ACCESS_KEY":                    "a",
		"S3_SECRET_KEY":                    "s",
		config.EnvDiscordLoginClientID:     testLoginClientID,
		config.EnvDiscordLoginClientSecret: testLoginSecret,
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg.DiscordLogin
}

type loginEnv struct {
	*env
	login *fakeLogin
}

func newLoginEnv(t *testing.T, mutate ...func(*Deps)) *loginEnv {
	t.Helper()
	fake := &fakeLogin{id: notify.DiscordIdentity{ID: testDiscordUser, Username: "carol_on_discord", GlobalName: "Carol"}}
	all := append([]func(*Deps){func(d *Deps) {
		d.Cfg.DiscordLogin = testLoginConfig(t)
		d.Login = fake
	}}, mutate...)
	return &loginEnv{env: newEnv(t, all...), login: fake}
}

func (e *loginEnv) flowCookieName() string { return e.cfg.SessionCookie + auth.FlowCookieSuffix }

// decode reads a flow cookie value under this instance's key.
func (e *loginEnv) decode(v string) (flow, bool) { return decodeFlow(flowKey(e.cfg.DiscordLogin), v) }

// forged builds a flow cookie the way an attacker would (erratum 302(h)): a
// fresh nonce — whose state they can derive, it is a public function — and a
// payload of their choosing, signed under `key` (nil: no MAC at all, the shape
// the build before the fix accepted).
func (e *loginEnv) forged(key []byte, f flow) *http.Cookie {
	f.nonce = newNonce()
	payload := base64.RawURLEncoding.EncodeToString([]byte(f.mode + "\x00" + f.userID + "\x00" + f.next))
	v := f.nonce + "." + payload
	if key != nil {
		v += "." + flowMAC(key, v)
	}
	return &http.Cookie{Name: e.flowCookieName(), Value: v}
}

// cookieNamed reads one Set-Cookie off a response by name.
func cookieNamed(r resp, name string) *http.Cookie {
	res := http.Response{Header: r.hdr}
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// start runs GET /start and returns the flow cookie and the authorize URL.
func (e *loginEnv) start(next string, opts ...reqOpt) (*http.Cookie, *url.URL) {
	e.t.Helper()
	path := discordLoginStartPath
	if next != "" {
		path += "?next=" + url.QueryEscape(next)
	}
	r := e.do("GET", path, nil, opts...)
	if r.code != 302 {
		e.t.Fatalf("start: %d %s", r.code, r.body)
	}
	c := cookieNamed(r, e.flowCookieName())
	if c == nil {
		e.t.Fatalf("start set no flow cookie: %v", r.hdr)
	}
	u, err := url.Parse(r.hdr.Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	return c, u
}

// stateOf derives the state Discord would echo for a flow cookie.
func stateOf(c *http.Cookie) string {
	nonce, _, _ := strings.Cut(c.Value, ".")
	return flowState(nonce)
}

// callback runs GET /callback with the query and cookie given.
func (e *loginEnv) callback(q url.Values, c *http.Cookie, opts ...reqOpt) resp {
	e.t.Helper()
	all := append([]reqOpt{withCookie(c)}, opts...)
	return e.do("GET", discordLoginCallbackPath+"?"+q.Encode(), nil, all...)
}

func good(c *http.Cookie) url.Values {
	return url.Values{"code": {testCode}, "state": {stateOf(c)}}
}

// expectBounce asserts a 303 to target with the flow cookie CLEARED on the
// response and no session issued.
func (e *loginEnv) expectBounce(r resp, target string) {
	e.t.Helper()
	if r.code != 303 || r.hdr.Get("Location") != target {
		e.t.Fatalf("want 303 %s, got %d %s (%s)", target, r.code, r.hdr.Get("Location"), r.body)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		e.t.Fatalf("Cache-Control = %q", r.hdr.Get("Cache-Control"))
	}
	e.expectFlowCleared(r)
	if e.sessionCookie(r) != nil {
		e.t.Fatal("a refusal issued a session")
	}
}

// expectBounceKeeping is expectBounce for a request that CARRIED a session:
// the Resolver's sliding touch may re-issue that cookie on the response, and
// the assertion is that no OTHER session was issued.
func (e *loginEnv) expectBounceKeeping(r resp, target string, sess *http.Cookie) {
	e.t.Helper()
	if r.code != 303 || r.hdr.Get("Location") != target {
		e.t.Fatalf("want 303 %s, got %d %s (%s)", target, r.code, r.hdr.Get("Location"), r.body)
	}
	e.expectFlowCleared(r)
	if c := e.sessionCookie(r); c != nil && c.Value != sess.Value {
		e.t.Fatal("a refusal issued a NEW session")
	}
}

func (e *loginEnv) expectFlowCleared(r resp) {
	e.t.Helper()
	c := cookieNamed(r, e.flowCookieName())
	if c == nil || c.MaxAge >= 0 || c.Value != "" {
		e.t.Fatalf("the flow cookie was not cleared on the response: %+v", c)
	}
}

// TestDiscordLoginOffState: with the two variables unset nothing exists.
func TestDiscordLoginOffState(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", discordLoginStartPath}, {"GET", discordLoginCallbackPath + "?code=x&state=y"},
		{"POST", discordLoginPath}, {"DELETE", discordLoginPath},
	} {
		r := e.doJSON(tc.method, tc.path, nil)
		if r.code != 404 || r.errCode() != "not_found" {
			t.Fatalf("%s %s with the login off: %d %s", tc.method, tc.path, r.code, r.body)
		}
	}
}

// TestDiscordLoginStart: the authorize URL, the flow cookie, and the three
// refusals a start can make.
func TestDiscordLoginStart(t *testing.T) {
	e := newLoginEnv(t)
	c, u := e.start("/settings")
	if u.Scheme != "https" || u.Host != config.DiscordAPIHost() || u.Path != "/oauth2/authorize" {
		t.Fatalf("authorize URL = %s", u)
	}
	q := u.Query()
	nonce, _, _ := strings.Cut(c.Value, ".")
	for k, want := range map[string]string{
		"response_type": "code", "client_id": testLoginClientID, "scope": "identify",
		"redirect_uri": testOrigin + discordLoginCallbackPath, "code_challenge_method": "S256",
		"state": flowState(nonce), "code_challenge": flowChallenge(nonce),
	} {
		if q.Get(k) != want {
			t.Fatalf("authorize %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if len(q.Get("state")) != 43 || q.Get("state") == q.Get("code_challenge") {
		t.Fatalf("state %q / challenge %q", q.Get("state"), q.Get("code_challenge"))
	}
	if _, ok := q["prompt"]; ok {
		t.Fatal("the authorize URL carries a prompt")
	}
	if len(nonce) != 43 || !c.HttpOnly || c.Path != "/" || c.SameSite != http.SameSiteLaxMode || c.Secure || c.MaxAge != 600 {
		t.Fatalf("flow cookie %+v", c)
	}
	f, ok := e.decode(c.Value)
	if !ok || f.mode != flowLogin || f.userID != "" || f.next != "/settings" {
		t.Fatalf("decoded flow %+v %v", f, ok)
	}
	// The verifier never appears in the URL the browser is sent to.
	if strings.Contains(u.String(), nonce) {
		t.Fatal("the authorize URL carries the verifier")
	}

	// next is held to safeNext.
	c, _ = e.start("//evil.example/x")
	if f, _ := e.decode(c.Value); f.next != "/" {
		t.Fatalf("next //evil → %q", f.next)
	}
	c, _ = e.start("")
	if f, _ := e.decode(c.Value); f.next != "/" {
		t.Fatalf("no next → %q", f.next)
	}

	// Signed in → straight to next, no flow.
	_, sess := e.newUser("signedin")
	r := e.do("GET", discordLoginStartPath+"?next=/settings", nil, withCookie(sess))
	if r.code != 303 || r.hdr.Get("Location") != "/settings" || cookieNamed(r, e.flowCookieName()) != nil {
		t.Fatalf("signed-in start: %d %s %v", r.code, r.hdr.Get("Location"), r.hdr)
	}
	// A cross-site navigation may not launch a flow.
	r = e.do("GET", discordLoginStartPath, nil, withHeader("Sec-Fetch-Site", "cross-site"))
	if r.code != 303 || r.hdr.Get("Location") != "/login" || cookieNamed(r, e.flowCookieName()) != nil {
		t.Fatalf("cross-site start: %d %s %v", r.code, r.hdr.Get("Location"), r.hdr)
	}
	// same-site, none and an absent header all launch.
	for _, v := range []string{"same-origin", "none", ""} {
		r = e.do("GET", discordLoginStartPath, nil, withHeader("Sec-Fetch-Site", v))
		if r.code != 302 {
			t.Fatalf("Sec-Fetch-Site %q: %d", v, r.code)
		}
	}
}

// TestDiscordLoginCallbackRefusals: every refusal before the exchange, each
// with the cookie cleared and the provider never asked.
func TestDiscordLoginCallbackRefusals(t *testing.T) {
	e := newLoginEnv(t)
	c, _ := e.start("/")
	before, _ := e.db.CountUsers(context.Background())

	// No cookie at all.
	e.expectBounce(e.callback(good(c), nil), "/login?discord=expired")
	// A cookie that is not the shape this binary writes.
	e.expectBounce(e.callback(good(c), &http.Cookie{Name: e.flowCookieName(), Value: "garbage"}), "/login?discord=expired")
	e.expectBounce(e.callback(good(c), &http.Cookie{Name: e.flowCookieName(), Value: strings.Repeat("a", 43) + ".!!!"}), "/login?discord=expired")
	// The wrong state — somebody else's, or none.
	e.expectBounce(e.callback(url.Values{"code": {testCode}, "state": {flowState(strings.Repeat("b", 43))}}, c), "/login?discord=expired")
	e.expectBounce(e.callback(url.Values{"code": {testCode}}, c), "/login?discord=expired")
	// Discord's own refusal.
	e.expectBounce(e.callback(url.Values{"error": {"access_denied"}, "state": {stateOf(c)}}, c), "/login?discord=denied")
	// No code, or an absurd one.
	e.expectBounce(e.callback(url.Values{"state": {stateOf(c)}}, c), "/login?discord=expired")
	e.expectBounce(e.callback(url.Values{"state": {stateOf(c)}, "code": {strings.Repeat("c", maxCodeBytes+1)}}, c), "/login?discord=expired")
	if e.login.count() != 0 {
		t.Fatalf("a refused callback reached the provider %d time(s)", e.login.count())
	}
	if after, _ := e.db.CountUsers(context.Background()); after != before {
		t.Fatal("a refused callback changed the users table")
	}
}

// TestDiscordLoginUnlinkedCreatesNothing: a Discord account that is not linked
// opens nothing and creates nothing, under every REGISTRATION value.
func TestDiscordLoginUnlinkedCreatesNothing(t *testing.T) {
	for _, mode := range []config.RegistrationMode{config.RegistrationOpen, config.RegistrationFirst, config.RegistrationClosed} {
		e := newLoginEnv(t, func(d *Deps) { d.Cfg.Registration = mode })
		before, _ := e.db.CountUsers(context.Background())
		c, _ := e.start("/")
		r, logs := e.logged("GET", discordLoginCallbackPath+"?"+good(c).Encode(), nil, withCookie(c))
		e.expectBounce(r, "/login?discord=unlinked")
		if e.login.count() != 1 {
			t.Fatalf("%s: the provider was asked %d times", mode, e.login.count())
		}
		if after, _ := e.db.CountUsers(context.Background()); after != before {
			t.Fatalf("%s: an unlinked Discord login created an account", mode)
		}
		if !strings.Contains(logs, "discord_login=unlinked") || !strings.Contains(logs, "client_ip=") {
			t.Fatalf("%s: the access line does not carry the verdict and the address: %s", mode, logs)
		}
	}
}

// TestDiscordLoginLinkedOpensTheAccount: the happy path — a session exactly
// as login issues one, the display name refreshed, the flow cookie cleared,
// and a replay refused.
func TestDiscordLoginLinkedOpensTheAccount(t *testing.T) {
	e := newLoginEnv(t)
	u, _ := e.newUser("carol")
	if err := e.db.LinkDiscord(context.Background(), u.ID, testDiscordUser, "old_name", e.now()); err != nil {
		t.Fatal(err)
	}
	c, _ := e.start("/settings")
	q := good(c)
	r, logs := e.logged("GET", discordLoginCallbackPath+"?"+q.Encode(), nil, withCookie(c))
	if r.code != 303 || r.hdr.Get("Location") != "/settings" {
		t.Fatalf("linked login: %d %s %s", r.code, r.hdr.Get("Location"), r.body)
	}
	e.expectFlowCleared(r)
	sess := e.sessionCookie(r)
	// Login's exact flags (TestRegisterLoginLogoutMe pins the same set).
	if sess == nil || sess.Value == "" || !sess.HttpOnly || sess.Path != "/" || sess.SameSite != http.SameSiteLaxMode || sess.Secure || sess.MaxAge != 3600 {
		t.Fatalf("session cookie: %+v", sess)
	}
	me := e.do("GET", "/api/auth/me", nil, withCookie(sess))
	if me.code != 200 || me.str("via") != "session" || me.json["user"].(map[string]any)["username"] != "carol" {
		t.Fatalf("me after a Discord login: %d %s", me.code, me.body)
	}
	call := e.login.last()
	nonce, _, _ := strings.Cut(c.Value, ".")
	if call.code != testCode || call.verifier != nonce || call.redirectURI != testOrigin+discordLoginCallbackPath {
		t.Fatalf("the provider was asked %+v", call)
	}
	row, _ := e.db.GetUserByID(context.Background(), u.ID)
	if row.DiscordUsername != "carol_on_discord" {
		t.Fatalf("the display name was not refreshed: %+v", row)
	}
	if !strings.Contains(logs, "login=discord") || !strings.Contains(logs, "user=carol") || !strings.Contains(logs, "client_ip=") {
		t.Fatalf("the access line: %s", logs)
	}
	// The SAME callback URL again: the browser no longer holds the cookie
	// (it was cleared above), so a replay is refused without an exchange.
	e.expectBounce(e.callback(q, nil), "/login?discord=expired")
	if e.login.count() != 1 {
		t.Fatalf("a replayed callback reached the provider (%d calls)", e.login.count())
	}
	// …and even a browser that kept the cookie cannot reuse it against a
	// SECOND exchange for a different next: the nonce is the same, the
	// state matches, and the provider IS asked — which is why the cookie is
	// cleared in the first place. This pins that the clear is on every
	// outcome, not that a kept cookie is refused.
	c2, _ := e.start("/")
	r2 := e.callback(good(c2), c2)
	if r2.code != 303 || r2.hdr.Get("Location") != "/" {
		t.Fatalf("second login: %d %s", r2.code, r2.hdr.Get("Location"))
	}
	e.expectFlowCleared(r2)
}

// TestDiscordLoginExchangeFailure: the provider's error lands as one line,
// scrubbed, and no session.
func TestDiscordLoginExchangeFailure(t *testing.T) {
	e := newLoginEnv(t)
	e.login.err = errors.New("HTTP 400 body={\"error\":\"invalid_grant\"}")
	c, _ := e.start("/")
	r, logs := e.logged("GET", discordLoginCallbackPath+"?"+good(c).Encode(), nil, withCookie(c))
	e.expectBounce(r, "/login?discord=failed")
	if !strings.Contains(logs, "discord_login=exchange_failed") {
		t.Fatalf("access line: %s", logs)
	}
	if !strings.Contains(e.logBuf.String(), "invalid_grant") {
		t.Fatalf("the app log does not carry the cause: %s", e.logBuf.String())
	}
}

// TestDiscordLink: the link POST, the link-mode callback in all its outcomes,
// and unlink.
func TestDiscordLink(t *testing.T) {
	e := newLoginEnv(t)
	reg := e.doJSON("POST", "/api/auth/register", map[string]string{"username": "owner", "password": "correct horse"})
	if reg.code != 201 {
		t.Fatalf("register: %d %s", reg.code, reg.body)
	}
	owner := e.sessionCookie(reg)
	ownerID := reg.json["user"].(map[string]any)["id"].(string)
	ownerRow, err := e.db.GetUserByID(context.Background(), ownerID)
	if err != nil {
		t.Fatal(err)
	}
	tok := e.newToken(ownerRow, auth.ScopeUpload)

	// Guards on the POST.
	if r := e.doJSON("POST", discordLoginPath, map[string]string{"current_password": "x"}); r.code != 401 {
		t.Fatalf("anonymous link: %d", r.code)
	}
	if r := e.doJSON("POST", discordLoginPath, map[string]string{"current_password": "x"}, withBearer(tok)); r.code != 403 || r.errCode() != "token_not_allowed" {
		t.Fatalf("token link: %d %s", r.code, r.body)
	}
	if r := e.do("POST", discordLoginPath, strings.NewReader("current_password=x"), withCookie(owner)); r.code != 415 {
		t.Fatalf("form link: %d", r.code)
	}
	if r := e.doJSON("POST", discordLoginPath, map[string]string{"current_password": "x"}, withCookie(owner), withHeader("Origin", "https://evil.example")); r.code != 403 || r.errCode() != "csrf" {
		t.Fatalf("cross-origin link: %d %s", r.code, r.body)
	}
	if r := e.doJSON("POST", discordLoginPath, map[string]string{}, withCookie(owner)); r.code != 400 {
		t.Fatalf("empty password: %d %s", r.code, r.body)
	}
	r, logs := e.logged("POST", discordLoginPath, strings.NewReader(`{"current_password":"wrong password"}`), withCookie(owner), withJSON())
	if r.code != 401 || r.errCode() != "invalid_credentials" || cookieNamed(r, e.flowCookieName()) != nil {
		t.Fatalf("wrong password: %d %s %v", r.code, r.body, r.hdr)
	}
	if !strings.Contains(logs, "discord_link=bad_password") || !strings.Contains(logs, "client_ip=") {
		t.Fatalf("bad-password access line: %s", logs)
	}

	// The real thing: a link-mode cookie bound to the owner, and the URL.
	r = e.doJSON("POST", discordLoginPath, map[string]string{"current_password": "correct horse"}, withCookie(owner))
	if r.code != 200 {
		t.Fatalf("link: %d %s", r.code, r.body)
	}
	c := cookieNamed(r, e.flowCookieName())
	if c == nil {
		t.Fatalf("no flow cookie: %v", r.hdr)
	}
	f, ok := e.decode(c.Value)
	if !ok || f.mode != flowLink || f.userID != ownerID || f.next != "/settings" {
		t.Fatalf("link flow %+v %v", f, ok)
	}
	nonce, _, _ := strings.Cut(c.Value, ".")
	u, err := url.Parse(r.str("url"))
	if err != nil || u.Host != config.DiscordAPIHost() || u.Query().Get("state") != flowState(nonce) || u.Query().Get("scope") != "identify" {
		t.Fatalf("link url: %q %v", r.str("url"), err)
	}

	// Link-mode callback without a session, and with somebody else's.
	e.expectBounce(e.callback(good(c), c), "/settings")
	_, other := e.newUser("other")
	e.expectBounceKeeping(e.callback(good(c), c, withCookie(other)), "/settings", other)
	if e.login.count() != 0 {
		t.Fatal("a link callback without the right session reached the provider")
	}
	// With the owner's session: linked.
	r, logs = e.logged("GET", discordLoginCallbackPath+"?"+good(c).Encode(), nil, withCookie(c), withCookie(owner))
	if r.code != 303 || r.hdr.Get("Location") != "/settings?discord=linked" {
		t.Fatalf("link callback: %d %s %s", r.code, r.hdr.Get("Location"), r.body)
	}
	e.expectFlowCleared(r)
	if !strings.Contains(logs, "discord_linked=owner") || !strings.Contains(logs, "discord_id="+testDiscordUser) {
		t.Fatalf("link access line: %s", logs)
	}
	row, _ := e.db.GetUserByID(context.Background(), ownerID)
	if row.DiscordID == nil || *row.DiscordID != testDiscordUser || row.DiscordUsername != "carol_on_discord" {
		t.Fatalf("the row after the link: %+v", row)
	}
	// A second link on a linked account: 409 on the POST, no cookie.
	r = e.doJSON("POST", discordLoginPath, map[string]string{"current_password": "correct horse"}, withCookie(owner))
	if r.code != 409 || r.errCode() != "discord_linked" || cookieNamed(r, e.flowCookieName()) != nil {
		t.Fatalf("relink: %d %s", r.code, r.body)
	}

	// …and a Discord LOGIN with that id now opens the owner's account.
	lc, _ := e.start("/")
	r = e.callback(good(lc), lc)
	if r.code != 303 || r.hdr.Get("Location") != "/" || e.sessionCookie(r) == nil {
		t.Fatalf("login after link: %d %s", r.code, r.hdr.Get("Location"))
	}
	me := e.do("GET", "/api/auth/me", nil, withCookie(e.sessionCookie(r)))
	if me.json["user"].(map[string]any)["username"] != "owner" {
		t.Fatalf("the Discord login opened %s", me.body)
	}

	// Taken: another account's link callback for the SAME Discord id.
	reg2 := e.doJSON("POST", "/api/auth/register", map[string]string{"username": "second", "password": "correct horse"})
	second := e.sessionCookie(reg2)
	r = e.doJSON("POST", discordLoginPath, map[string]string{"current_password": "correct horse"}, withCookie(second))
	c2 := cookieNamed(r, e.flowCookieName())
	r = e.callback(good(c2), c2, withCookie(second))
	if r.code != 303 || r.hdr.Get("Location") != "/settings?discord=taken" {
		t.Fatalf("taken: %d %s", r.code, r.hdr.Get("Location"))
	}
	if strings.Contains(string(r.body), "owner") || strings.Contains(r.hdr.Get("Location"), "owner") {
		t.Fatal("the taken refusal named the other account")
	}
	row2, _ := e.db.GetUserByUsername(context.Background(), "second")
	if row2.DiscordID != nil {
		t.Fatalf("the taken id was linked anyway: %+v", row2)
	}

	// Unlink: session + CSRF, 204 then 404; the session survives.
	if r := e.doJSON("DELETE", discordLoginPath, nil); r.code != 401 {
		t.Fatalf("anonymous unlink: %d", r.code)
	}
	if r := e.doJSON("DELETE", discordLoginPath, nil, withCookie(owner), withHeader("Sec-Fetch-Site", "cross-site")); r.code != 403 {
		t.Fatalf("cross-site unlink: %d", r.code)
	}
	if r := e.doJSON("DELETE", discordLoginPath, nil, withCookie(second)); r.code != 404 || r.errCode() != "not_linked" {
		t.Fatalf("unlink of an unlinked account: %d %s", r.code, r.body)
	}
	r, logs = e.logged("DELETE", discordLoginPath, nil, withCookie(owner), withJSON())
	if r.code != 204 || !strings.Contains(logs, "discord_unlinked=owner") {
		t.Fatalf("unlink: %d %s / %s", r.code, r.body, logs)
	}
	row, _ = e.db.GetUserByID(context.Background(), ownerID)
	if row.DiscordID != nil {
		t.Fatalf("still linked: %+v", row)
	}
	if me := e.do("GET", "/api/auth/me", nil, withCookie(owner)); me.code != 200 {
		t.Fatalf("the unlink touched the session: %d", me.code)
	}
	// The id is free: a Discord login for it now opens nothing.
	lc, _ = e.start("/")
	e.expectBounce(e.callback(good(lc), lc), "/login?discord=unlinked")
}

// TestDiscordLinkCookieIsUnforgeable (erratum 302(h)): the link intent is
// proven by the cookie's MAC, not by its words. A thief holding the owner's
// SESSION cookie — and so able to pass step 6 — cannot mint the link cookie
// the password POST mints: a hand-built one, one signed under a guessed key,
// a real cookie with its payload swapped, and a real link cookie re-bound to
// another account are all refused as no flow, with no exchange and no row
// written. Login-mode forgeries are refused the same way.
func TestDiscordLinkCookieIsUnforgeable(t *testing.T) {
	e := newLoginEnv(t)
	reg := e.doJSON("POST", "/api/auth/register", map[string]string{"username": "alice", "password": "correct horse"})
	if reg.code != 201 {
		t.Fatalf("register: %d %s", reg.code, reg.body)
	}
	alice := e.sessionCookie(reg)
	aliceID := reg.json["user"].(map[string]any)["id"].(string)
	link := flow{mode: flowLink, userID: aliceID, next: "/settings"}

	// The exact repro: a live session for alice plus a cookie built from the
	// nonce alone (no MAC — the pre-fix shape), a good state and a code.
	forged := e.forged(nil, link)
	e.expectBounceKeeping(e.callback(good(forged), forged, withCookie(alice)), "/login?discord=expired", alice)
	// …signed under a wrong key (a guess at the derivation, or another
	// instance's secret).
	wrong := e.forged(flowKey(config.DiscordLogin{}), link)
	e.expectBounceKeeping(e.callback(good(wrong), wrong, withCookie(alice)), "/login?discord=expired", alice)
	// …a REAL login-mode cookie (minted by an anonymous start) with its
	// payload swapped for a link payload: the MAC is over the payload too.
	real, _ := e.start("/")
	nonce, rest, _ := strings.Cut(real.Value, ".")
	_, mac, _ := strings.Cut(rest, ".")
	swapped := &http.Cookie{Name: e.flowCookieName(), Value: nonce + "." + base64.RawURLEncoding.EncodeToString([]byte("link\x00"+aliceID+"\x00/settings")) + "." + mac}
	e.expectBounceKeeping(e.callback(good(swapped), swapped, withCookie(alice)), "/login?discord=expired", alice)
	// …and a real LINK cookie, minted for bob with bob's password, re-bound
	// to alice: the nonce is in the MAC, so is the bound id.
	reg2 := e.doJSON("POST", "/api/auth/register", map[string]string{"username": "bob", "password": "correct horse"})
	bob := e.sessionCookie(reg2)
	r := e.doJSON("POST", discordLoginPath, map[string]string{"current_password": "correct horse"}, withCookie(bob))
	if r.code != 200 {
		t.Fatalf("bob's link POST: %d %s", r.code, r.body)
	}
	bobs := cookieNamed(r, e.flowCookieName())
	nonce, rest, _ = strings.Cut(bobs.Value, ".")
	_, mac, _ = strings.Cut(rest, ".")
	rebound := &http.Cookie{Name: e.flowCookieName(), Value: nonce + "." + base64.RawURLEncoding.EncodeToString([]byte("link\x00"+aliceID+"\x00/settings")) + "." + mac}
	e.expectBounceKeeping(e.callback(good(rebound), rebound, withCookie(alice)), "/login?discord=expired", alice)
	// A login-mode forgery is no flow either.
	fl := e.forged(nil, flow{mode: flowLogin, next: "/"})
	e.expectBounce(e.callback(good(fl), fl), "/login?discord=expired")

	if n := e.login.count(); n != 0 {
		t.Fatalf("a forged cookie reached the provider %d time(s)", n)
	}
	for _, name := range []string{"alice", "bob"} {
		row, _ := e.db.GetUserByUsername(context.Background(), name)
		if row.DiscordID != nil {
			t.Fatalf("a forged cookie wrote a link on %s: %+v", name, row)
		}
	}
	// The genuine article still works: what the POST mints, signed under the
	// instance's key, is exactly what a forger cannot produce.
	r = e.doJSON("POST", discordLoginPath, map[string]string{"current_password": "correct horse"}, withCookie(alice))
	genuine := cookieNamed(r, e.flowCookieName())
	if r := e.callback(good(genuine), genuine, withCookie(alice)); r.code != 303 || r.hdr.Get("Location") != "/settings?discord=linked" {
		t.Fatalf("the genuine link cookie: %d %s", r.code, r.hdr.Get("Location"))
	}
	if e.login.count() != 1 {
		t.Fatalf("the genuine link asked the provider %d times", e.login.count())
	}
}

// TestDiscordLoginCookieInheritsHostPrefix: on an https origin whose session
// cookie carries `__Host-`, the flow cookie does too, and is Secure.
func TestDiscordLoginCookieInheritsHostPrefix(t *testing.T) {
	base, _ := url.Parse("https://clips.example")
	e := newLoginEnv(t, func(d *Deps) {
		d.Cfg.PublicBaseURL = base
		d.Cfg.SessionCookie = "__Host-clippet_session"
		d.URLs = urlsFor("https://clips.example")
	})
	r := e.do("GET", discordLoginStartPath, nil)
	if r.code != 302 {
		t.Fatalf("start: %d", r.code)
	}
	c := cookieNamed(r, "__Host-clippet_session_discord")
	if c == nil || !c.Secure || c.Path != "/" || c.Domain != "" {
		t.Fatalf("flow cookie on https: %+v", c)
	}
	if u, _ := url.Parse(r.hdr.Get("Location")); u.Query().Get("redirect_uri") != "https://clips.example"+discordLoginCallbackPath {
		t.Fatalf("redirect_uri = %q", u.Query().Get("redirect_uri"))
	}
}

// TestDiscordLoginNeverLeaksASecret: no response body, no header and no log
// line carries the secret, the code, the state, the verifier or a token.
func TestDiscordLoginNeverLeaksASecret(t *testing.T) {
	e := newLoginEnv(t)
	u, _ := e.newUser("carol")
	_ = e.db.LinkDiscord(context.Background(), u.ID, testDiscordUser, "x", e.now())
	c, _ := e.start("/")
	nonce, _, _ := strings.Cut(c.Value, ".")
	e.login.err = errors.New("HTTP 400 body=refused " + testLoginSecret) // a provider that misbehaves
	r, logs := e.logged("GET", discordLoginCallbackPath+"?"+good(c).Encode(), nil, withCookie(c))
	// The Location may carry only the closed-map reason.
	all := string(r.body) + "\n" + r.hdr.Get("Location") + "\n" + logs
	for _, leak := range []string{testCode, nonce, stateOf(c)} {
		if strings.Contains(all, leak) {
			t.Fatalf("a response or the access log carries %q:\n%s", leak, all)
		}
	}
	// The app log carries the provider's cause; the secret in it is the
	// provider's failure to scrub (internal/notify's own test pins that it
	// does), and the handler adds nothing of its own.
	if strings.Contains(logs, testLoginSecret) {
		t.Fatalf("the access log carries the secret: %s", logs)
	}
}

// TestSafeNextMatchesTheWebRule pins this package's copy of internal/web's
// safeNext against the same table (§20 erratum 52), since the two packages
// cannot share the function.
func TestSafeNextMatchesTheWebRule(t *testing.T) {
	for in, want := range map[string]string{
		"":                "/",
		"/":               "/",
		"/settings":       "/settings",
		"/c/abc?x=1":      "/c/abc?x=1",
		"/dir/":           "/dir/",
		"//evil.example":  "/",
		"/\\evil.example": "/",
		"/./\\/evil":      "/",
		"/a/../b":         "/",
		"/a\x00b":         "/",
		"https://x/":      "/",
		"settings":        "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDiscordLoginPathsAreSpelledOnce: the exported accessors and the route
// constants agree, and the start path is the one internal/web links to.
func TestDiscordLoginPathsAreSpelledOnce(t *testing.T) {
	if DiscordLoginCallbackPath() != discordLoginCallbackPath || DiscordLoginStartPath() != discordLoginStartPath {
		t.Fatal("the accessors do not spell the constants")
	}
	if discordLoginStartPath != "/api/auth/discord/start" || discordLoginCallbackPath != "/api/auth/discord/callback" || discordLoginPath != "/api/auth/discord" {
		t.Fatalf("paths: %s %s %s", discordLoginStartPath, discordLoginCallbackPath, discordLoginPath)
	}
}
