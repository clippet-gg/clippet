package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"clippet/internal/db"
)

// GET /api/bot/claim — the official bot's origin proof (§20 erratum 279).
// One boolean per guild id, only when the bot is on, and never a name.

func TestBotClaimAnswersOneBooleanPerGuild(t *testing.T) {
	b := newBotEnv(t)
	const guild = "222222222222222222"
	const other = "333333333333333333"

	// Unbound: 404 with an EMPTY body — exactly one bit, no hint.
	r := b.do(http.MethodGet, botClaimPath+"?guild_id="+guild, nil)
	if r.code != http.StatusNotFound || len(r.body) != 0 {
		t.Fatalf("unbound: %d %q", r.code, r.body)
	}
	if cc := r.hdr.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store: the answer changes on bind/unbind", cc)
	}

	// Bound: 200 with the boolean and the application id this box verifies
	// for — and nothing about WHO it is bound to.
	u := b.boundGuild(t, "owner", guild)
	r = b.do(http.MethodGet, botClaimPath+"?guild_id="+guild, nil)
	if r.code != http.StatusOK {
		t.Fatalf("bound: %d %s", r.code, r.body)
	}
	if r.json["bound"] != true || r.json["app_id"] != b.cfg.DiscordBot.AppID() {
		t.Fatalf("bound body = %s", r.body)
	}
	// The two fields, since 2026-10-12 (§20 erratum 318 amended) the bound
	// account's link defaults, and since §20 erratum 342 the scope — and
	// nothing else.
	if len(r.json) != 4 || r.json["scope"] != "server" {
		t.Fatalf("the answer carries other than the four fields: %s", r.body)
	}
	def, ok := r.json["defaults"].(map[string]any)
	if !ok || len(def) != 3 {
		t.Fatalf("defaults = %v", r.json["defaults"])
	}
	// A fresh account: no term of its own, so the instance's (0 = never when
	// DEFAULT_CLIP_TTL is unset), listed, compact.
	wantDays := float64(instanceDefaultTTLDays(b.cfg.DefaultTTLSeconds))
	if def["ttl_days"] != wantDays || def["unlisted"] != false || def["link"] != "compact" {
		t.Fatalf("a fresh account's defaults = %v, want ttl_days %v, listed, compact", def, wantDays)
	}
	// The account's own choices come back as set.
	days := 7
	if err := b.db.SetUserDefaults(context.Background(), u.ID, db.UserDefaults{TTLDays: &days, Unlisted: true, LinkStyle: db.LinkStyleMemorable}); err != nil {
		t.Fatal(err)
	}
	r = b.do(http.MethodGet, botClaimPath+"?guild_id="+guild, nil)
	def, _ = r.json["defaults"].(map[string]any)
	if r.code != http.StatusOK || def["ttl_days"] != float64(7) || def["unlisted"] != true || def["link"] != "memorable" {
		t.Fatalf("set defaults: %d %s", r.code, r.body)
	}
	if s := string(r.body); strings.Contains(s, u.ID) || strings.Contains(s, "owner") {
		t.Fatalf("the answer names the account: %s", s)
	}
	if cc := r.hdr.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q on the 200", cc)
	}

	// Another guild is still unbound: the binding is per guild.
	if r = b.do(http.MethodGet, botClaimPath+"?guild_id="+other, nil); r.code != http.StatusNotFound {
		t.Fatalf("other guild: %d", r.code)
	}

	// A malformed id is a 400 naming the rule, not a 404 that would read as
	// "not bound".
	for _, bad := range []string{"", "12", "not-a-snowflake", strings.Repeat("1", 26), "222222222222222222 "} {
		r = b.do(http.MethodGet, botClaimPath+"?guild_id="+strings.ReplaceAll(bad, " ", "%20"), nil)
		if bad == "222222222222222222 " {
			// Trailing whitespace is trimmed: the same id.
			if r.code != http.StatusOK {
				t.Fatalf("trimmed id: %d", r.code)
			}
			continue
		}
		if r.code != http.StatusBadRequest || r.json["error"] != "invalid_guild_id" {
			t.Fatalf("guild_id=%q: %d %s", bad, r.code, r.body)
		}
	}

	// Only GET: the route has no side effect and takes none.
	if r = b.do(http.MethodPost, botClaimPath+"?guild_id="+guild, strings.NewReader("{}"), withJSON()); r.code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", r.code)
	}
}

func TestBotClaimRouteAbsentWhenTheBotIsOff(t *testing.T) {
	e := newEnv(t)
	r := e.do(http.MethodGet, botClaimPath+"?guild_id=222222222222222222", nil)
	if r.code != http.StatusNotFound || r.json["error"] != "not_found" {
		t.Fatalf("with the bot off the route must not exist: %d %s", r.code, r.body)
	}
}

// TestBotClaimPathIsUnderAReservedSegment pins the path choice: `api` is a
// reserved first segment already, so no minted id can shadow this route and
// no new reserved word was needed.
func TestBotClaimPathIsUnderAReservedSegment(t *testing.T) {
	if !strings.HasPrefix(botClaimPath, "/api/") {
		t.Fatalf("botClaimPath = %q", botClaimPath)
	}
	if BotClaimPath() != botClaimPath {
		t.Fatal("BotClaimPath() does not spell the constant")
	}
}

// TestClaimDefaultsResolvesTheTerm pins the one rule claimDefaults adds: an
// account with no term of its own answers the instance's DEFAULT_CLIP_TTL in
// days, an illegal stored term (a hand-edited 5) answers the instance's days as
// an upload would, and an illegal stored grammar answers the shipped one.
func TestClaimDefaultsResolvesTheTerm(t *testing.T) {
	thirty := int64(30 * 86400)
	never := 0
	five := 5
	cases := []struct {
		def      db.UserDefaults
		instance *int64
		want     map[string]any
	}{
		{db.UserDefaults{}, &thirty, map[string]any{"ttl_days": 30, "unlisted": false, "link": "compact"}},
		{db.UserDefaults{}, nil, map[string]any{"ttl_days": 0, "unlisted": false, "link": "compact"}},
		{db.UserDefaults{TTLDays: &never, Unlisted: true, LinkStyle: "memorable"}, &thirty, map[string]any{"ttl_days": 0, "unlisted": true, "link": "memorable"}},
		{db.UserDefaults{TTLDays: &five}, &thirty, map[string]any{"ttl_days": 30, "unlisted": false, "link": "compact"}},
		{db.UserDefaults{LinkStyle: "nonsense"}, &thirty, map[string]any{"ttl_days": 30, "unlisted": false, "link": "compact"}},
	}
	for i, c := range cases {
		got := claimDefaults(c.def, c.instance)
		if len(got) != 3 || got["ttl_days"] != c.want["ttl_days"] || got["unlisted"] != c.want["unlisted"] || got["link"] != c.want["link"] {
			t.Fatalf("case %d: %v, want %v", i, got, c.want)
		}
	}
}

// --- a person binding (migration 0024, §20 erratum 342) --------------------

// TestBotClaimPersonAnswers is the route's whole table: a server binding
// answers as before plus "scope":"server" with or without a user id; a person
// binding answers 200 — scope person, no defaults — only for the account's own
// linked id, and the same empty 404 an unbound guild answers to every other
// ask; a user_id that is present and not a snowflake is a 400 on any binding.
func TestBotClaimPersonAnswers(t *testing.T) {
	b := newBotEnv(t)
	ctx := context.Background()
	const guild = "222222222222222222"
	const linked = "444444444444444444"
	const other = "555555555555555555"
	claim := func(query string) resp {
		t.Helper()
		return b.do(http.MethodGet, botClaimPath+"?guild_id="+guild+query, nil)
	}
	u := b.boundGuild(t, "friend", guild)
	if err := b.db.LinkDiscord(ctx, u.ID, linked, "friend_d", b.now()); err != nil {
		t.Fatal(err)
	}

	// A SERVER binding: today's answer plus the scope, whoever is asked about.
	for _, q := range []string{"", "&user_id=" + linked, "&user_id=" + other} {
		r := claim(q)
		if r.code != http.StatusOK || r.json["bound"] != true || r.json["scope"] != "server" || r.json["defaults"] == nil {
			t.Fatalf("server binding, ask %q: %d %s", q, r.code, r.body)
		}
	}

	// A PERSON binding.
	if _, err := b.db.BindGuildScope(ctx, guild, u.ID, db.GuildScopePerson, b.now()); err != nil {
		t.Fatal(err)
	}
	r := claim("&user_id=" + linked)
	if r.code != http.StatusOK || r.json["bound"] != true || r.json["app_id"] != b.cfg.DiscordBot.AppID() || r.json["scope"] != "person" {
		t.Fatalf("person binding, the linked id: %d %s", r.code, r.body)
	}
	if len(r.json) != 3 {
		t.Fatalf("a person answer carries other than bound, app_id and scope (no defaults): %s", r.body)
	}
	if s := string(r.body); strings.Contains(s, linked) || strings.Contains(s, u.ID) || strings.Contains(s, "friend") {
		t.Fatalf("the answer names the member or the account: %s", s)
	}
	for _, q := range []string{"", "&user_id=" + other} {
		if r := claim(q); r.code != http.StatusNotFound || len(r.body) != 0 {
			t.Fatalf("person binding, ask %q: %d %q; want an empty 404", q, r.code, r.body)
		}
	}

	// A malformed user_id is named, not read as "not you" — on either scope.
	for _, bad := range []string{"", "12", "not-a-snowflake", strings.Repeat("4", 26)} {
		r := claim("&user_id=" + bad)
		if r.code != http.StatusBadRequest || r.json["error"] != "invalid_user_id" {
			t.Fatalf("user_id=%q: %d %s", bad, r.code, r.body)
		}
	}
	if _, err := b.db.BindGuild(ctx, guild, u.ID, b.now()); err != nil {
		t.Fatal(err)
	}
	if r := claim("&user_id=nope"); r.code != http.StatusBadRequest || r.json["error"] != "invalid_user_id" {
		t.Fatalf("server binding, bad user_id: %d %s", r.code, r.body)
	}

	// A person binding whose account lost its link (unlinked by hand, under
	// the CLI's guard) answers no one.
	if _, err := b.db.BindGuildScope(ctx, guild, u.ID, db.GuildScopePerson, b.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.UnlinkDiscord(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if r := claim("&user_id=" + linked); r.code != http.StatusNotFound || len(r.body) != 0 {
		t.Fatalf("person binding, account unlinked: %d %q", r.code, r.body)
	}
}

// TestBotClaimLogsTheAnswerNeverTheID: the access log's `bot_claim` field is
// one of four words, and the asked id never reaches a log line.
func TestBotClaimLogsTheAnswerNeverTheID(t *testing.T) {
	b := newBotEnv(t)
	ctx := context.Background()
	const guild = "222222222222222222"
	const linked = "444444444444444444"
	const other = "555555555555555555"
	field := func(query string) string {
		t.Helper()
		_, logs := b.logged(http.MethodGet, botClaimPath+"?guild_id="+guild+query, nil)
		if strings.Contains(logs, linked) || strings.Contains(logs, other) {
			t.Fatalf("a log line carries the asked id: %s", logs)
		}
		return logs
	}
	if l := field(""); !strings.Contains(l, "bot_claim=unbound") {
		t.Fatalf("unbound: %s", l)
	}
	u := b.boundGuild(t, "friend", guild)
	if l := field("&user_id=" + other); !strings.Contains(l, "bot_claim=bound") {
		t.Fatalf("server: %s", l)
	}
	if err := b.db.LinkDiscord(ctx, u.ID, linked, "friend_d", b.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.BindGuildScope(ctx, guild, u.ID, db.GuildScopePerson, b.now()); err != nil {
		t.Fatal(err)
	}
	if l := field("&user_id=" + linked); !strings.Contains(l, "bot_claim=person") {
		t.Fatalf("person: %s", l)
	}
	if l := field("&user_id=" + other); !strings.Contains(l, "bot_claim=not_person") {
		t.Fatalf("not_person: %s", l)
	}
}
