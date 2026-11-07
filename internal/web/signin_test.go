package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
)

// The bot's one-time login link on the pages (§20 erratum 341). The whole
// flow — the interaction, the GET, the POST, the session — is driven end to
// end in internal/httpapi's discord_signin_test.go; these are the page's own
// rules.

// mintLink stores a live link for the fixture's clock and returns its path.
func (f *fixture) mintLink(t *testing.T, guild string) string {
	t.Helper()
	raw := ids.SigninToken()
	if err := f.db.CreateSigninToken(context.Background(), db.SigninToken{
		Hash: auth.HashToken(raw), GuildID: guild, DiscordID: "555555555555555501",
		Username: "mallory", DisplayName: "Mallory <M>", CreatedAt: f.now, ExpiresAt: f.now + db.SigninTTLSeconds,
	}); err != nil {
		t.Fatal(err)
	}
	return auth.SigninLinkPrefix + raw
}

func (f *fixture) openDoor(t *testing.T) {
	t.Helper()
	f.discordBot = testBotConfig(t, "managed")
	f.registration = config.RegistrationMembers
	if _, err := f.db.BindGuild(context.Background(), "222222222222222222", f.owner.ID, f.now); err != nil {
		t.Fatal(err)
	}
}

// The route exists only while the door is open: the bot AND a Discord mode.
func TestSigninLinkNeedsTheDoor(t *testing.T) {
	f := newFixture(t)
	path := f.mintLink(t, "222222222222222222")
	for _, tc := range []struct {
		name string
		set  func()
	}{
		{"no bot, members", func() { f.registration = config.RegistrationMembers }},
		{"bot, open", func() { f.discordBot = testBotConfig(t, "managed"); f.registration = config.RegistrationOpen }},
		{"bot, closed", func() { f.registration = config.RegistrationClosed }},
	} {
		tc.set()
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			if rec := do(f.handler(passAuth{}), m, path); rec.Code != http.StatusNotFound {
				t.Errorf("%s: %s = %d, want the 404 page", tc.name, m, rec.Code)
			}
		}
	}
}

// The page: the host, the Discord name (escaped), one primary button, a plain
// form to itself; no script, no cookie, noindex, private, and no referrer to
// another origin. A shape that is not a token, or a dead one, goes to /login's
// line without a read.
func TestSigninLinkPage(t *testing.T) {
	f := newFixture(t)
	f.openDoor(t)
	path := f.mintLink(t, "222222222222222222")
	rec := get(f.handler(passAuth{}), path)
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, `<h1>Sign in to example.test:8481 as Mallory &lt;M&gt;</h1>`,
		`<form class="panel" data-signin action="`+path+`" method="post">`,
		`<button type="submit" class="btn primary">Sign in</button>`, `noindex`)
	if strings.Count(body, `class="btn primary"`) != 1 {
		t.Error("the page has more than one solid button")
	}
	assertNoInline(t, "signin", body)
	if got := rec.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("Referrer-Policy = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q", got)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Error("the GET set a cookie")
	}
	for _, p := range []string{auth.SigninLinkPrefix + "nope", auth.SigninLinkPrefix + ids.SigninToken()} {
		rec = get(f.handler(passAuth{}), p)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?discord=link" {
			t.Errorf("GET %s = %d %q", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	// The fixture's clock five minutes on: the same link is dead.
	f.now += db.SigninTTLSeconds
	if rec = get(f.handler(passAuth{}), path); rec.Header().Get("Location") != "/login?discord=link" {
		t.Errorf("GET of a five-minute-old link = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// A cross-site POST is refused before the token is read: it would sign the
// victim's browser into the forger's account.
func TestSigninLinkRefusesACrossSitePost(t *testing.T) {
	f := newFixture(t)
	f.openDoor(t)
	path := f.mintLink(t, "222222222222222222")
	for _, hdr := range [][]string{{"Origin", "https://evil.example"}, {"Sec-Fetch-Site", "cross-site"}} {
		if rec := do(f.handler(passAuth{}), http.MethodPost, path, hdr...); rec.Code != http.StatusForbidden || rec.Header().Get("Set-Cookie") != "" {
			t.Errorf("%v: POST = %d", hdr, rec.Code)
		}
	}
	if rec := get(f.handler(passAuth{}), path); rec.Code != 200 {
		t.Fatalf("the refused POSTs spent the token: GET = %d", rec.Code)
	}
}

// An operator who unbinds the server inside the five minutes has said no.
func TestSigninLinkNeedsTheServerStillBound(t *testing.T) {
	f := newFixture(t)
	f.openDoor(t)
	path := f.mintLink(t, "333333333333333333") // never bound
	rec := do(f.handler(passAuth{}), http.MethodPost, path, "Origin", testBase)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?discord=link" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("POST for an unbound server = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := f.db.GetUserByDiscordID(context.Background(), "555555555555555501"); err == nil {
		t.Fatal("an account was made for an unbound server's link")
	}
}

// /login's line and its dead-link notice exist only while the door is open.
func TestLoginPageSigninLine(t *testing.T) {
	f := newFixture(t)
	const line = `<p class="alt" data-signin-hint>Members of the connected server: /signin in Discord.</p>`
	const notice = "That sign-in link expired. Run /signin again."
	for _, tc := range []struct {
		name string
		set  func()
		want bool
	}{
		{"neither", func() {}, false},
		{"mode without bot", func() { f.registration = config.RegistrationMembers }, false},
		{"bot without mode", func() { f.registration = config.RegistrationClosed; f.discordBot = testBotConfig(t, "managed") }, false},
		{"members + bot", func() { f.registration = config.RegistrationMembers }, true},
		{"role + bot", func() { f.registration = config.RegistrationMode("role:666666666666666601") }, true},
	} {
		tc.set()
		body := get(f.handler(passAuth{}), "/login?discord=link").Body.String()
		if strings.Contains(body, line) != tc.want || strings.Contains(body, notice) != tc.want {
			t.Errorf("%s: line=%v notice=%v, want %v", tc.name, strings.Contains(body, line), strings.Contains(body, notice), tc.want)
		}
		if tc.want {
			// The web form is closed under both modes: no "Create an account".
			mustContain(t, body, `data-registration-closed`)
			mustNotContain(t, body, `href="/register`)
			assertNoInline(t, "login "+tc.name, body)
		}
	}
}

// The account name a Discord username becomes: §14's charset, lowercase, 3-32.
func TestSigninUsername(t *testing.T) {
	for in, want := range map[string]string{
		"mallory":                                "mallory",
		"Mal.Lory":                               "mal_lory",
		"ab":                                     "ab_",
		"x":                                      "x__",
		"é.é":                                    "___",
		"user_123":                               "user_123",
		strings.Repeat("a", 40):                  strings.Repeat("a", 32),
		"Discord User With Spaces And More Text": "discord_user_with_spaces_and_mor",
	} {
		if got := signinUsername(in); got != want {
			t.Errorf("signinUsername(%q) = %q, want %q", in, got, want)
		}
	}
}
