package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/notify"
	"clippet/internal/urls"
	"clippet/internal/web"
)

// `/signin` — the bot is the door (§20 erratum 341). The interactions are
// signed with a key minted per test (the fake Discord of discord_test.go); the
// link's page is the REAL internal/web handler over the same database.

const (
	signinGuild  = "888888888888888801"
	signinMember = "555555555555555501"
	signinRole   = "666666666666666601"
)

// newSigninEnv is a bot env under the given REGISTRATION, with one bound guild.
func newSigninEnv(t *testing.T, mode config.RegistrationMode) *botEnv {
	t.Helper()
	b := newBotEnv(t, func(d *Deps) { d.Cfg.Registration = mode })
	b.boundGuild(t, "serverowner", signinGuild)
	return b
}

// signinBody is a `/signin` interaction from one member.
func signinBody(guild, userID, username, global string, roles ...string) string {
	if roles == nil {
		roles = []string{}
	}
	body, _ := json.Marshal(map[string]any{
		"type": interactionCommand, "token": "int-token-signin", "guild_id": guild,
		"member": map[string]any{
			"permissions": "2147601473", "roles": roles,
			"user": map[string]any{"id": userID, "username": username, "global_name": global},
		},
		"data": map[string]any{"name": config.SigninCommandName, "type": 1},
	})
	return string(body)
}

// signinLink runs `/signin` and returns the link's PATH (the page's address),
// having checked the whole reply: the owner's sentence, the link on its own line in
// Discord's no-unfurl brackets, ephemeral + SUPPRESS_EMBEDS.
func signinLink(t *testing.T, b *botEnv, body string) string {
	t.Helper()
	r := b.post(body)
	if r.code != http.StatusOK {
		t.Fatalf("/signin = %d: %s", r.code, r.body)
	}
	data, _ := r.json["data"].(map[string]any)
	content, _ := data["content"].(string)
	if flags, _ := data["flags"].(float64); int(flags) != ephemeralFlag|suppressEmbedsFlag {
		t.Fatalf("flags = %v, want %d (ephemeral + SUPPRESS_EMBEDS): %s", data["flags"], ephemeralFlag|suppressEmbedsFlag, r.body)
	}
	head, link, ok := strings.Cut(content, "\n")
	if !ok || head != "Sign in to localhost:8481 · this link works once, for five minutes." {
		t.Fatalf("content = %q", content)
	}
	if !strings.HasPrefix(link, "<"+testOrigin+auth.SigninLinkPrefix+"cs_") || !strings.HasSuffix(link, ">") {
		t.Fatalf("the link line is %q, want <%s%scs_…>", link, testOrigin, auth.SigninLinkPrefix)
	}
	for _, forbidden := range []string{"embeds", "components"} {
		if _, bad := data[forbidden]; bad {
			t.Fatalf("the reply carries %q: %s", forbidden, r.body)
		}
	}
	return strings.TrimPrefix(strings.TrimSuffix(link[1:], ">"), testOrigin)
}

// pagesFor is the real page handler over the bot env's database and config.
func pagesFor(t *testing.T, b *botEnv, now func() time.Time) http.Handler {
	t.Helper()
	base, _ := url.Parse(testOrigin)
	pages, err := web.New(web.Deps{
		DB: b.db, Auth: &auth.Resolver{DB: b.db, Cfg: b.cfg, Now: now},
		URLs: urls.New(base, "test"), Cfg: b.cfg, Log: slog.New(slog.DiscardHandler), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pages
}

func page(h http.Handler, method, path string, hdr ...string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader("next=%2F")
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// rawDB is a second connection to the env's file, for reading the token table
// the db package exposes no listing of (the way nullClipColumns writes).
func rawDB(t *testing.T, b *botEnv) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+b.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

func countSignins(t *testing.T, b *botEnv) int {
	t.Helper()
	var n int
	if err := rawDB(t, b).QueryRowContext(context.Background(), "SELECT COUNT(*) FROM signin_tokens").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The mode table: who `/signin` answers with a link, and the exact refusal for
// everyone else. No refusal leaves a token behind.
func TestSigninModeTable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  config.RegistrationMode
		guild string
		roles []string
		want  string // "" = a link
	}{
		{"members", config.RegistrationMembers, signinGuild, nil, ""},
		{"role present", config.RegistrationMode("role:" + signinRole), signinGuild, []string{"1", signinRole}, ""},
		{"role absent", config.RegistrationMode("role:" + signinRole), signinGuild, []string{"1"}, msgSigninRole},
		{"off (open)", config.RegistrationOpen, signinGuild, nil, msgSigninOff},
		{"off (closed)", config.RegistrationClosed, signinGuild, nil, msgSigninOff},
		{"off (first)", config.RegistrationFirst, signinGuild, nil, msgSigninOff},
		{"unbound", config.RegistrationMembers, "999999999999999999", nil, msgUnbound},
		{"dm", config.RegistrationMembers, "", nil, "Run /signin in a server, not in a DM."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newSigninEnv(t, tc.mode)
			body := signinBody(tc.guild, signinMember, "mallory", "Mallory", tc.roles...)
			if tc.want == "" {
				signinLink(t, b, body)
				if n := countSignins(t, b); n != 1 {
					t.Fatalf("%d token rows, want 1", n)
				}
				return
			}
			r := b.post(body)
			assertEphemeral(t, r, tc.want)
			if c, _ := r.json["data"].(map[string]any)["content"].(string); c != tc.want {
				t.Fatalf("content = %q, want exactly %q", c, tc.want)
			}
			if n := countSignins(t, b); n != 0 {
				t.Fatalf("a refusal left %d token row(s)", n)
			}
		})
	}
}

// The stored row: the token HASHED (the raw value is nowhere in the table),
// the member and the names, five minutes; nothing of it on the log.
func TestSigninStoresAHashAndLogsNothing(t *testing.T) {
	b := newSigninEnv(t, config.RegistrationMembers)
	body := signinBody(signinGuild, signinMember, "mallory", "Mallory M")
	r, logs := b.postLogged(body)
	if r.code != http.StatusOK {
		t.Fatalf("/signin = %d", r.code)
	}
	content := r.json["data"].(map[string]any)["content"].(string)
	raw := content[strings.Index(content, "cs_") : len(content)-1]
	var hash, guild, member, username, display string
	var created, expires int64
	err := rawDB(t, b).QueryRowContext(context.Background(),
		"SELECT token_hash, guild_id, discord_user_id, discord_username, display_name, created_at, expires_at FROM signin_tokens").
		Scan(&hash, &guild, &member, &username, &display, &created, &expires)
	if err != nil {
		t.Fatal(err)
	}
	if hash != auth.HashToken(raw) || hash == raw || guild != signinGuild || member != signinMember ||
		username != "mallory" || display != "Mallory M" || expires-created != 300 {
		t.Fatalf("row = %q %q %q %q %q %d..%d", hash, guild, member, username, display, created, expires)
	}
	for _, leak := range []string{raw, auth.SigninLinkPrefix, signinMember, "mallory"} {
		if strings.Contains(logs+b.logBuf.String(), leak) {
			t.Fatalf("%q reached the log: %s", leak, logs)
		}
	}
	if !strings.Contains(logs, "discord=signin") || !strings.Contains(logs, "signin=link") {
		t.Fatalf("the access line does not say discord=signin signin=link: %s", logs)
	}
	// The global name is the display; without one, the username.
	b2 := newSigninEnv(t, config.RegistrationMembers)
	b2.post(signinBody(signinGuild, signinMember, "mallory", ""))
	if err := rawDB(t, b2).QueryRowContext(context.Background(), "SELECT display_name FROM signin_tokens").Scan(&display); err != nil || display != "mallory" {
		t.Fatalf("display without a global name = %q, %v", display, err)
	}
}

// The whole door: a GET (twice — the crawler, then the member) changes
// nothing; the POST signs in with a session a password login would have
// issued; the second POST is refused; the page logs no token.
func TestSigninLinkGetIsFreeAndPostSignsInOnce(t *testing.T) {
	b := newSigninEnv(t, config.RegistrationMembers)
	path := signinLink(t, b, signinBody(signinGuild, signinMember, "Mal.Lory", "Mallory"))
	pages := pagesFor(t, b, time.Now)
	for i := 0; i < 2; i++ {
		rec := page(pages, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %d = %d: %s", i, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{`Sign in to localhost:8481 as Mallory`, `<button type="submit" class="btn primary">Sign in</button>`,
			`action="` + path + `"`, `method="post"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("the page lacks %q", want)
			}
		}
		if rec.Header().Get("Set-Cookie") != "" {
			t.Fatal("a GET set a cookie")
		}
	}
	if n := countSignins(t, b); n != 1 {
		t.Fatalf("after two GETs %d token rows, want 1 (a GET consumes nothing)", n)
	}
	if n, _ := b.db.CountUsers(context.Background()); n != 1 {
		t.Fatalf("after two GETs %d users, want 1 (a GET creates nothing)", n)
	}
	// A cross-site POST is refused and spends nothing.
	if rec := page(pages, http.MethodPost, path, "Origin", "https://evil.example"); rec.Code != http.StatusForbidden || countSignins(t, b) != 1 {
		t.Fatalf("a cross-site POST = %d, tokens %d", rec.Code, countSignins(t, b))
	}
	rec := page(pages, http.MethodPost, path, "Origin", testOrigin)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("POST = %d %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	got := rec.Result().Cookies()
	if len(got) != 1 {
		t.Fatalf("POST set %d cookies", len(got))
	}
	// The created account: the username normalised, linked by id, no password.
	u, err := b.db.GetUserByDiscordID(context.Background(), signinMember)
	if err != nil || u.Username != "mal_lory" || u.PasswordHash != "" {
		t.Fatalf("created %+v, %v", u, err)
	}
	// The SAME session a password login issues: cookie name, flags and lifetime
	// against the API login's own, and one row on the Signed-in browsers list.
	pw := mustLoginCookie(t, b)
	if got[0].Name != pw.Name || got[0].HttpOnly != pw.HttpOnly || got[0].Secure != pw.Secure ||
		got[0].SameSite != pw.SameSite || got[0].Path != pw.Path || got[0].MaxAge != pw.MaxAge {
		t.Fatalf("the sign-in cookie %+v differs from a password login's %+v", got[0], pw)
	}
	r := b.do(http.MethodGet, "/api/auth/sessions", nil, withCookie(got[0]))
	if r.code != http.StatusOK || len(r.json["sessions"].([]any)) != 1 {
		t.Fatalf("/api/auth/sessions with the sign-in cookie = %d %s", r.code, r.body)
	}
	if n := countSignins(t, b); n != 0 {
		t.Fatalf("%d token rows after the POST", n)
	}
	// Single use: the second POST, and a GET after it, land on /login's line.
	for _, m := range []string{http.MethodPost, http.MethodGet} {
		rec = page(pages, m, path, "Origin", testOrigin)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?discord=link" || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("%s after the POST = %d %q", m, rec.Code, rec.Header().Get("Location"))
		}
	}
	login := page(pages, http.MethodGet, "/login?discord=link")
	if !strings.Contains(login.Body.String(), "That sign-in link expired. Run /signin again.") {
		t.Fatal("/login does not say the link expired")
	}
	// The account has no password: the password login refuses it with its
	// ordinary answer, for any password at all.
	for _, pwd := range []string{"", "anything-long-enough"} {
		r := b.doJSON(http.MethodPost, "/api/auth/login", map[string]string{"username": "mal_lory", "password": pwd})
		if r.code == http.StatusOK || (pwd != "" && r.errCode() != "invalid_credentials") {
			t.Fatalf("the password login on a passwordless account = %d %s", r.code, r.body)
		}
	}
}

// mustLoginCookie is a password login's cookie through the API, on an account
// made for it.
func mustLoginCookie(t *testing.T, b *botEnv) *http.Cookie {
	t.Helper()
	hash, err := auth.HashPassword("a-password-for-the-compare")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.CreateUser(context.Background(), "pwuser-id", "pwuser", hash, b.now()); err != nil {
		t.Fatal(err)
	}
	r := b.doJSON(http.MethodPost, "/api/auth/login", map[string]string{"username": "pwuser", "password": "a-password-for-the-compare"})
	if r.code != http.StatusOK {
		t.Fatalf("the password login = %d %s", r.code, r.body)
	}
	resp := http.Response{Header: r.hdr}
	cs := resp.Cookies()
	if len(cs) != 1 {
		t.Fatalf("the password login set %d cookies", len(cs))
	}
	return cs[0]
}

// A member already linked is OPENED, never duplicated; a member whose Discord
// username equals an existing account's name is NOT linked to it — the id is
// the key — and gets a suffixed account of their own.
func TestSigninOpensTheLinkedAccountAndNeverLinksByName(t *testing.T) {
	b := newSigninEnv(t, config.RegistrationMembers)
	ctx := context.Background()
	owner := mustUser(t, b, "serverowner")
	if err := b.db.LinkDiscord(ctx, owner.ID, signinMember, "", b.now()); err != nil {
		t.Fatal(err)
	}
	pages := pagesFor(t, b, time.Now)
	path := signinLink(t, b, signinBody(signinGuild, signinMember, "someoneelse", ""))
	if rec := page(pages, http.MethodPost, path); rec.Code != http.StatusSeeOther || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("POST = %d", rec.Code)
	}
	if n, _ := b.db.CountUsers(ctx); n != 1 {
		t.Fatalf("%d users after a linked member's sign-in, want 1", n)
	}
	// A stranger named like the owner's account.
	const stranger = "555555555555555502"
	path = signinLink(t, b, signinBody(signinGuild, stranger, "ServerOwner", ""))
	if rec := page(pages, http.MethodPost, path); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST = %d", rec.Code)
	}
	if o, _ := b.db.GetUserByID(ctx, owner.ID); o.DiscordID == nil || *o.DiscordID != signinMember {
		t.Fatal("the owner's link changed")
	}
	u, err := b.db.GetUserByDiscordID(ctx, stranger)
	if err != nil || u.ID == owner.ID || u.Username != "serverowner_2" {
		t.Fatalf("the stranger got %+v, %v; want an account of their own named serverowner_2", u, err)
	}
}

// The link expires five minutes after it was minted (the page's clock moved).
func TestSigninLinkExpiresAfterFiveMinutes(t *testing.T) {
	b := newSigninEnv(t, config.RegistrationMembers)
	path := signinLink(t, b, signinBody(signinGuild, signinMember, "mallory", ""))
	late := func() time.Time { return time.Now().Add(5*time.Minute + time.Second) }
	pages := pagesFor(t, b, late)
	if rec := page(pages, http.MethodGet, path); rec.Header().Get("Location") != "/login?discord=link" {
		t.Fatalf("GET of a 5-minute-old link = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := page(pages, http.MethodPost, path); rec.Header().Get("Location") != "/login?discord=link" || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("POST of a 5-minute-old link = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if n, _ := b.db.CountUsers(context.Background()); n != 1 {
		t.Fatal("an expired link made an account")
	}
}

// Under every mode that is not a Discord one the route is the 404 page, so a
// token minted before a restart into another mode opens nothing — and
// /register stays closed under `members`.
func TestSigninRouteOnlyWhileTheDoorIsOpen(t *testing.T) {
	b := newSigninEnv(t, config.RegistrationMembers)
	path := signinLink(t, b, signinBody(signinGuild, signinMember, "mallory", ""))
	// /register is closed under `members` on both sides: the API and the page.
	if r := b.doJSON(http.MethodPost, "/api/auth/register", map[string]string{"username": "walkin", "password": "long-enough-pw"}); r.code != http.StatusForbidden || r.errCode() != "registration_closed" {
		t.Fatalf("POST /api/auth/register under members = %d %s", r.code, r.body)
	}
	if body := page(pagesFor(t, b, time.Now), http.MethodGet, "/register").Body.String(); !strings.Contains(body, "data-registration-closed") {
		t.Fatal("/register under members draws a form")
	}
	b.cfg.Registration = config.RegistrationOpen
	if rec := page(pagesFor(t, b, time.Now), http.MethodPost, path); rec.Code != http.StatusNotFound || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("POST under REGISTRATION=open = %d, want the 404 page", rec.Code)
	}
	if n := countSignins(t, b); n != 1 {
		t.Fatal("the shut door spent the token")
	}
}

// The command's NAME: one spelling in internal/config, internal/notify's
// registration and this endpoint.
func TestSigninCommandNameMatchesTheRegistration(t *testing.T) {
	if got := notify.SigninCommandName(); got != config.SigninCommandName {
		t.Fatalf("internal/notify registers /%s, internal/config names /%s", got, config.SigninCommandName)
	}
}
