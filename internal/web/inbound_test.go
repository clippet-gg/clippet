package web

import (
	"context"
	"strings"
	"testing"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
)

// The tokens panel and the inbound URL (§20 erratum 322): the Kind control is
// the mint form's FIRST row, its hint shows both shapes on this instance's own
// host, the words the form switches to are in the template, and a row whose
// scopes are `inbound` is listed as ci_… while every other row stays ck_….
func TestSettingsTokensPanelInbound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tk := range []db.APIToken{
		{ID: ids.New(), UserID: f.owner.ID, Name: "deck", TokenHash: "h-in", Prefix: "inbnd123", Scopes: []string{"inbound"}, CreatedAt: f.now},
		{ID: ids.New(), UserID: f.owner.ID, Name: "sharex", TokenHash: "h-ck", Prefix: "token123", Scopes: []string{"upload"}, CreatedAt: f.now - 1},
	} {
		if err := f.db.CreateToken(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	body := get(f.handler(session(f.owner)), "/settings").Body.String()
	mustContain(t, body,
		`<span class="rl">Kind</span>`,
		`<span class="seg" role="radiogroup" aria-label="Kind">`,
		`<input type="radio" name="kind" value="token" checked data-token-kind><span>Token</span>`,
		`<input type="radio" name="kind" value="inbound" data-token-kind><span>Inbound URL</span>`,
		`Token: <code>Bearer ck_…</code> · Inbound URL: <code>example.test:8481/in/ci_…</code>`,
		`placeholder="ShareX on desktop" data-placeholder-inbound="Stream deck"`,
		`data-token-scopes`,
		`data-label-inbound="Create URL" data-label-autofetch="Create URL">Create token</button>`,
		`data-once-inbound="Copy this URL now — it is shown only once. Anyone who has it can upload as you." data-once-autofetch=`,
		`>Copy this token now — it is shown only once.</p>`,
		`<code>ci_inbnd123…</code>`, `<td class="m">inbound</td>`, `<code>ck_token123…</code>`,
		`click Revoke again to confirm — tools using it stop at once`,
		`<h2>API tokens</h2><span class="hint">a token lets a capture tool upload as you (POST /api/upload); it cannot change your password or make another token</span>`)
	form := body[strings.Index(body, "data-token-mint"):]
	if k, n := strings.Index(form, `aria-label="Kind"`), strings.Index(form, `id="t-name"`); k < 0 || n < 0 || k > n {
		t.Errorf("the Kind row is not the mint form's first row (kind at %d, name at %d)", k, n)
	}
	assertNoInline(t, "settings", body)
}

// The Clippet Auto URL (§20 erratum 324): the Kind control's third option and
// the hint's third part are drawn ONLY on an instance that answers the
// official bot (DISCORD_BOT_TOKEN=managed); its words are in the template; a
// row whose scopes are `autofetch` is listed as ca_….
func TestSettingsTokensPanelAutoFetch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.db.CreateToken(ctx, db.APIToken{ID: ids.New(), UserID: f.owner.ID, Name: "my server", TokenHash: "h-ca",
		Prefix: "autof123", Scopes: []string{"autofetch"}, CreatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	option := `<input type="radio" name="kind" value="autofetch" data-token-kind><span>Clippet Auto URL</span>`
	hint := ` · Clippet Auto URL: <code>example.test:8481/in/auto/ca_…</code>`
	words := []string{
		`data-placeholder-autofetch="the server it is for"`,
		`data-label-autofetch="Create URL"`,
		`data-once-autofetch="Copy this URL now — it is shown only once. It goes into /settings → Clippet Auto in your Discord server. Anyone who has it can upload Discord videos as you."`,
		`<code>ca_autof123…</code>`, `<td class="m">autofetch</td>`,
	}
	// No bot, and a bot with its own token: not drawn.
	body := get(f.handler(session(f.owner)), "/settings").Body.String()
	mustNotContain(t, body, option, hint)
	mustContain(t, body, words...)
	f.discordBot = testBotConfig(t, "MTIzNDU2Nzg5MDEyMzQ1Njc4.GhIjKl.mNoPqRsTuVwXyZ0123456789abcdef")
	body = get(f.handler(session(f.owner)), "/settings").Body.String()
	mustNotContain(t, body, option, hint)
	// Managed: drawn, after Inbound URL.
	f.discordBot = testBotConfig(t, "managed")
	body = get(f.handler(session(f.owner)), "/settings").Body.String()
	mustContain(t, body, option,
		`Token: <code>Bearer ck_…</code> · Inbound URL: <code>example.test:8481/in/ci_…</code>`+hint+`</span>`)
	mustContain(t, body, words...)
	if i, j := strings.Index(body, `value="inbound" data-token-kind`), strings.Index(body, option); i < 0 || j < i {
		t.Errorf("the Clippet Auto option is not after Inbound URL (%d, %d)", i, j)
	}
	assertNoInline(t, "settings", body)
}

// testBotConfig is a real config.DiscordBot through config.Load with the
// given DISCORD_BOT_TOKEN.
func testBotConfig(t *testing.T, token string) config.DiscordBot {
	t.Helper()
	env := map[string]string{
		"PUBLIC_BASE_URL": testBase, "S3_ENDPOINT": "http://minio:9000", "S3_BUCKET": "b",
		"S3_ACCESS_KEY": "a", "S3_SECRET_KEY": "s",
		config.EnvDiscordAppID:     "987654321098765432",
		config.EnvDiscordPublicKey: strings.Repeat("ab", 32),
		config.EnvDiscordBotToken:  token,
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg.DiscordBot
}
