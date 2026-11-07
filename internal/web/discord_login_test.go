package web

import (
	"context"
	"strings"
	"testing"

	"clippet/internal/config"
)

// "Sign in with Discord" on the pages (ROADMAP #44, §20 erratum 302): the
// button on /login and the panel on /settings exist exactly when the
// instance's own application is configured; the notices are a closed map.

func testDiscordLogin(t *testing.T) config.DiscordLogin {
	t.Helper()
	env := map[string]string{
		"PUBLIC_BASE_URL":                  testBase,
		"S3_ENDPOINT":                      "http://minio:9000",
		"S3_BUCKET":                        "b",
		"S3_ACCESS_KEY":                    "a",
		"S3_SECRET_KEY":                    "s",
		config.EnvDiscordLoginClientID:     "876543210987654321",
		config.EnvDiscordLoginClientSecret: "aBcDeFgHiJkLmNoPqRsTuVwXyZ012345",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg.DiscordLogin
}

func TestLoginPageDiscordButton(t *testing.T) {
	f := newFixture(t)
	// OFF: no button, no start path anywhere, whatever the query says.
	body := get(f.handler(passAuth{}), "/login?discord=unlinked").Body.String()
	mustNotContain(t, body, "Sign in with Discord", discordLoginStart, "data-discord-start")
	// …and no notice either: with the feature off there is nothing that
	// could have sent somebody here with one.
	mustNotContain(t, body, "data-discord-notice")

	f.discordLogin = testDiscordLogin(t)
	h := f.handler(passAuth{})
	rec := get(h, "/login?next=/settings")
	assertPageHeaders(t, rec, 200, "no-store")
	body = rec.Body.String()
	mustContain(t, body, `<a class="btn" href="/api/auth/discord/start?next=%2Fsettings" data-discord-start>Sign in with Discord</a>`)
	// A hairline button, not the screen's one solid one; a plain anchor, not a form.
	mustNotContain(t, body, `class="btn primary" href="/api/auth/discord`, `action="/api/auth/discord/start"`)
	mustNotContain(t, body, "data-discord-notice")
	assertNoInline(t, "login with discord", body)

	// next is held to the same rule as the form's hidden input.
	body = get(h, "/login?next=//evil.example").Body.String()
	mustContain(t, body, `href="/api/auth/discord/start?next=%2F"`)

	// The closed notice map.
	for reason, want := range map[string]string{
		"expired":  "That Discord sign-in expired. Try again.",
		"denied":   "Discord sign-in cancelled.",
		"failed":   "Discord did not answer. Try again.",
		"unlinked": "No account here is linked to that Discord. Log in with your password, then link it in Settings.",
	} {
		body := get(h, "/login?discord="+reason).Body.String()
		mustContain(t, body, `<p class="error" data-discord-notice>`+want+`</p>`)
	}
	body = get(h, "/login?discord=<script>alert(1)</script>").Body.String()
	mustNotContain(t, body, "data-discord-notice", "<script>alert")
	// Signed in → still straight to next.
	rec = get(f.handler(session(f.owner)), "/login?next=/settings")
	if rec.Code != 303 || rec.Header().Get("Location") != "/settings" {
		t.Fatalf("logged-in /login: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSettingsDiscordPanel(t *testing.T) {
	f := newFixture(t)
	// OFF: no panel.
	body := get(f.handler(session(f.owner)), "/settings?discord=linked").Body.String()
	mustNotContain(t, body, `id="discord"`, "data-discord-link", "data-discord-unlink", "data-discord-notice")

	f.discordLogin = testDiscordLogin(t)
	h := f.handler(session(f.owner))
	body = get(h, "/settings").Body.String()
	// Not linked: the password row and the Link button; after the sessions panel.
	mustContain(t, body, `id="discord"`, `<form data-discord-link action="/api/auth/discord" method="post">`,
		`name="current_password"`, `>Link Discord</button>`, `<span class="rs">not linked</span>`, `<noscript>`)
	mustNotContain(t, body, "data-discord-unlink", "linked as")
	if i, j := strings.Index(body, `id="sessions"`), strings.Index(body, `id="discord"`); i < 0 || j < i {
		t.Fatalf("the Discord panel is not after the sessions panel (%d, %d)", i, j)
	}
	if i, j := strings.Index(body, `id="discord"`), strings.Index(body, `id="defaults"`); j < i {
		t.Fatalf("the Discord panel is not before the defaults panel (%d, %d)", i, j)
	}
	assertNoInline(t, "settings not linked", body)

	// Linked: the row names the Discord user and the date, and offers Unlink.
	if err := f.db.LinkDiscord(context.Background(), f.owner.ID, "111111111111111111", "carol_on_discord", f.now); err != nil {
		t.Fatal(err)
	}
	body = get(h, "/settings").Body.String()
	mustContain(t, body, `linked as <strong>carol_on_discord</strong> · since `+formatDate(f.now),
		`data-discord-unlink data-confirm-text="Really unlink">Unlink</button>`, `data-discord-unlink-hint hidden`)
	mustNotContain(t, body, "data-discord-link", "not linked")
	// Never the Discord id on the page: the CLI is the only surface that names it.
	mustNotContain(t, body, "111111111111111111")
	assertNoInline(t, "settings linked", body)

	// The notices.
	body = get(h, "/settings?discord=linked").Body.String()
	mustContain(t, body, `<p class="ok pad" data-discord-notice>Discord linked.`)
	for reason, want := range map[string]string{
		"taken":   "That Discord account is already linked to another account here.",
		"denied":  "Discord sign-in cancelled; nothing was linked.",
		"failed":  "Discord did not answer. Try again.",
		"expired": "That Discord link expired. Try again.",
	} {
		body := get(h, "/settings?discord="+reason).Body.String()
		mustContain(t, body, `<p class="error pad" data-discord-notice>`+want+`</p>`)
	}
	body = get(h, "/settings?discord=nonsense").Body.String()
	mustNotContain(t, body, "data-discord-notice>")

	// A link the CLI made carries no Discord username until the first Discord
	// login refreshes it: the row says `linked`, never an empty name.
	if _, err := f.db.UnlinkDiscord(context.Background(), f.owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.LinkDiscord(context.Background(), f.owner.ID, "111111111111111111", "", f.now); err != nil {
		t.Fatal(err)
	}
	body = get(h, "/settings").Body.String()
	mustContain(t, body, `<span class="rc">linked · since `+formatDate(f.now))
	mustNotContain(t, body, "<strong></strong>", "linked as")
}

// The start path this package links to is the one internal/httpapi serves
// (its own test pins the same spelling).
func TestDiscordLoginStartPathSpelling(t *testing.T) {
	if discordLoginStart != "/api/auth/discord/start" {
		t.Fatalf("discordLoginStart = %q", discordLoginStart)
	}
}
