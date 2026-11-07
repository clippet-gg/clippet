package config

import (
	"strings"
	"testing"

	// Both imports are the TEST BINARY's, not the package's: internal/config
	// depends on the standard library alone (§18.1), which is what lets every
	// other package read its rules from here. They are imported so the two
	// restated constants — the tag grammar and the reserved route name — are
	// pinned against the real ones rather than against a literal.
	"clippet/internal/db"
	"clippet/internal/notify"
)

// A pair of well-formed webhook URLs that pass the §17 #4 allowlist. They are
// obviously fake — the id is zeros and the token says what it is — so nothing
// in this file could ever post to a real channel if it were run against the
// network, which it never is.
const (
	hookA = "https://discord.com/api/webhooks/100000000000000001/not-a-real-token-aaa"
	hookB = "https://canary.discord.com/api/webhooks/100000000000000002/not-a-real-token-bbb"
	hookC = "https://discordapp.com/api/webhooks/100000000000000003/not-a-real-token-ccc"
)

// TestDiscordRoutesParse: the shape, the order and the normalisation.
func TestDiscordRoutesParse(t *testing.T) {
	c, err := Load(env(map[string]string{
		"DISCORD_WEBHOOK_URL":    hookA,
		"DISCORD_WEBHOOK_ROUTES": " Clutch=" + hookB + " ; fails =" + hookC + " ; ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.DiscordRoutes) != 2 {
		t.Fatalf("routes = %+v, want 2", c.DiscordRoutes)
	}
	// ORDER is the operator's priority and is preserved verbatim; the tag is
	// lower-cased and trimmed exactly as db.CleanTag would.
	if c.DiscordRoutes[0].Tag != "clutch" || c.DiscordRoutes[1].Tag != "fails" {
		t.Errorf("tags = %q, %q", c.DiscordRoutes[0].Tag, c.DiscordRoutes[1].Tag)
	}
	if c.DiscordRoutes[0].Webhook.URL() != hookB || c.DiscordRoutes[1].Webhook.URL() != hookC {
		t.Errorf("routes carry the wrong endpoints")
	}
	if c.DiscordRoutes[0].Webhook.Host() != "canary.discord.com" || c.DiscordRoutes[0].Webhook.ID() != "100000000000000002" {
		t.Errorf("route webhook not split: %+v", c.DiscordRoutes[0].Webhook)
	}
	if !c.DiscordEnabled() {
		t.Error("DiscordEnabled is false with a default and two routes")
	}
	if c.DiscordUnmatched != DiscordUnmatchedDefault {
		t.Errorf("DISCORD_WEBHOOK_UNMATCHED default = %q", c.DiscordUnmatched)
	}
}

// TestDiscordRoutesAloneEnableTheFeature: routes with no DISCORD_WEBHOOK_URL
// is a legal configuration ("only tagged clips are announced"), and it is what
// the notifier is built from.
func TestDiscordRoutesAloneEnableTheFeature(t *testing.T) {
	c, err := Load(env(map[string]string{"DISCORD_WEBHOOK_ROUTES": "clutch=" + hookA}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DiscordWebhook.Configured() {
		t.Error("a default webhook appeared from nowhere")
	}
	if !c.DiscordEnabled() {
		t.Error("DiscordEnabled is false although a route is configured")
	}
}

// TestDiscordDisabledByDefault is the §15 promise: with neither variable set,
// nothing is configured and nothing will ever construct a client.
func TestDiscordDisabledByDefault(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DiscordEnabled() || len(c.DiscordRoutes) != 0 {
		t.Errorf("Discord is on with nothing configured: %+v", c.DiscordRoutes)
	}
}

// TestDiscordRoutesRejections: every rejection names the RULE and never the
// value, because a routes value is a list of secrets.
func TestDiscordRoutesRejections(t *testing.T) {
	cases := []struct {
		name, value, want string
	}{
		{"no assignment", hookA, "<tag>"},
		{"empty tag", "=" + hookA, "is empty"},
		{"illegal tag", "not ok=" + hookA, "must be 1-32 characters"},
		{"tag starting with a dash", "-x=" + hookA, "must be 1-32 characters"},
		{"a rule typed backwards", hookA + "=clutch", "rule 1"},
		{"the second rule is the one at fault", "clutch=" + hookA + ";" + hookB + "=x", "rule 2"},
		{"duplicate tag", "clutch=" + hookA + ";clutch=" + hookB, "appears twice"},
		{"plain http", "clutch=" + strings.Replace(hookA, "https", "http", 1), "must be https"},
		{"foreign host", "clutch=https://evil.example/api/webhooks/1/t", "host must be exactly one of"},
		{"bad path", "clutch=https://discord.com/api/v10/webhooks/1/t", "path must be"},
		{"userinfo", "clutch=https://u:p@discord.com/api/webhooks/1/tok", "userinfo"},
		{"query", "clutch=" + hookA + "?x=1", "query string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(env(map[string]string{"DISCORD_WEBHOOK_ROUTES": c.value}))
			if err == nil {
				t.Fatal("accepted an illegal DISCORD_WEBHOOK_ROUTES")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name the rule %q", err, c.want)
			}
			// The whole point: no secret in the message. The token of every
			// fixture above is the same recognisable string.
			if strings.Contains(err.Error(), "not-a-real-token") {
				t.Errorf("the error echoes a webhook URL: %q", err)
			}
		})
	}
}

// TestDiscordRoutesRejectionsEchoNothing makes §20 erratum 158's sentence
// literal: a routes value is a list of SECRETS, so no rejection quotes any part
// of it. The case that matters is the rule typed BACKWARDS — strings.Cut splits
// on the first '=', so the whole webhook lands in the "tag" half — because a
// message that echoed it would print a live token to stderr and from there into
// `docker compose logs app`, where it persists and is shipped with the logs.
func TestDiscordRoutesRejectionsEchoNothing(t *testing.T) {
	for _, value := range []string{
		hookA + "=clutch",                      // the rule typed backwards
		"clutch=" + hookA + ";" + hookB + "=x", // …as the second rule
		hookA,                                  // no '=' at all
		"not ok=" + hookA,                      // an illegal tag beside a real URL
		"=" + hookA,                            // an empty tag beside one
		strings.Repeat("a", 33) + "=" + hookA,  // an over-long tag beside one
	} {
		_, err := Load(env(map[string]string{"DISCORD_WEBHOOK_ROUTES": value}))
		if err == nil {
			t.Fatalf("accepted %q", value)
		}
		msg := err.Error()
		// The token, the credential-bearing path, and the id that precedes the
		// token: no fragment of any of them may reach the message.
		for _, secret := range []string{"not-a-real-token", "discord.com/api", "/webhooks/", "1000000000000000"} {
			if strings.Contains(msg, secret) {
				t.Errorf("the rejection of %q echoes %q: %s", value, secret, msg)
			}
		}
	}
}

// TestDiscordRoutesRefuseTheReservedTag: `default` satisfies the tag grammar
// but already NAMES DISCORD_WEBHOOK_URL's own channel in the boot log, in a
// delivery line and in a queued job's params_json. Accepted, the rule would be
// logged as configured and then silently dropped, and every clip carrying the
// tag would be posted to the default channel — the one outcome routing exists
// to prevent (§20 erratum 160).
func TestDiscordRoutesRefuseTheReservedTag(t *testing.T) {
	for _, value := range []string{
		"default=" + hookB,
		" DEFAULT =" + hookB, // the grammar lower-cases and trims first
		"clutch=" + hookA + ";default=" + hookB,
	} {
		_, err := Load(env(map[string]string{
			"DISCORD_WEBHOOK_URL":    hookA,
			"DISCORD_WEBHOOK_ROUTES": value,
		}))
		if err == nil {
			t.Fatalf("accepted a route tagged %q in %q", reservedRouteTag, value)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("the rejection of %q does not say the tag is reserved: %v", value, err)
		}
		if strings.Contains(err.Error(), "not-a-real-token") {
			t.Errorf("the rejection echoes a webhook URL: %v", err)
		}
	}
}

// TestReservedRouteTagIsNotifys pins the spelling this package restates against
// the one internal/notify defines. The constant is duplicated so that
// internal/config keeps depending on the standard library alone; a TEST binary's
// imports are not the package's, so the pin costs the package nothing.
func TestReservedRouteTagIsNotifys(t *testing.T) {
	if reservedRouteTag != notify.DefaultRoute {
		t.Fatalf("reservedRouteTag = %q, notify.DefaultRoute = %q", reservedRouteTag, notify.DefaultRoute)
	}
}

// TestUnmatchedSkipWithNoRulesIsRefused: `skip` means "announce only clips a
// rule matched", so with NO rules it means "announce nothing" — while the boot
// log still says "discord webhook configured". One typo in
// DISCORD_WEBHOOK_ROUTES would silently stop every announcement, which is the
// class of misconfiguration §4 says must fail at boot (§20 erratum 160).
func TestUnmatchedSkipWithNoRulesIsRefused(t *testing.T) {
	_, err := Load(env(map[string]string{
		"DISCORD_WEBHOOK_URL":       hookA,
		"DISCORD_WEBHOOK_UNMATCHED": "skip",
	}))
	if err == nil {
		t.Fatal("accepted DISCORD_WEBHOOK_UNMATCHED=skip with a webhook and no rules")
	}
	if !strings.Contains(err.Error(), "DISCORD_WEBHOOK_ROUTES") {
		t.Errorf("the rejection does not name the variable to fix: %v", err)
	}
	// With rules it is exactly what an operator means, and it is kept.
	c, err := Load(env(map[string]string{
		"DISCORD_WEBHOOK_URL":       hookA,
		"DISCORD_WEBHOOK_UNMATCHED": "skip",
		"DISCORD_WEBHOOK_ROUTES":    "clutch=" + hookB,
	}))
	if err != nil || c.DiscordUnmatched != DiscordUnmatchedSkip {
		t.Fatalf("skip alongside rules: %v / %q", err, c.DiscordUnmatched)
	}
	// With Discord off entirely the variable is inert: a leftover line in a
	// .env must not fail the boot of an instance that posts nowhere.
	if _, err := Load(env(map[string]string{"DISCORD_WEBHOOK_UNMATCHED": "skip"})); err != nil {
		t.Errorf("skip with Discord off failed the boot: %v", err)
	}
}

// TestDiscordUnmatched pins the closed set.
func TestDiscordUnmatched(t *testing.T) {
	c, err := Load(env(map[string]string{"DISCORD_WEBHOOK_UNMATCHED": "SKIP"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DiscordUnmatched != DiscordUnmatchedSkip {
		t.Errorf("DISCORD_WEBHOOK_UNMATCHED=SKIP → %q", c.DiscordUnmatched)
	}
	if _, err := Load(env(map[string]string{"DISCORD_WEBHOOK_UNMATCHED": "maybe"})); err == nil {
		t.Error("accepted DISCORD_WEBHOOK_UNMATCHED=maybe")
	}
}

// TestStorageBudgetBytes: off by default, a non-negative integer otherwise.
func TestStorageBudgetBytes(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil || c.StorageBudgetBytes != 0 {
		t.Fatalf("default STORAGE_BUDGET_BYTES = %d (%v)", c.StorageBudgetBytes, err)
	}
	c, err = Load(env(map[string]string{"STORAGE_BUDGET_BYTES": "214748364800"}))
	if err != nil || c.StorageBudgetBytes != 214748364800 {
		t.Fatalf("STORAGE_BUDGET_BYTES = %d (%v)", c.StorageBudgetBytes, err)
	}
	if _, err := Load(env(map[string]string{"STORAGE_BUDGET_BYTES": "-1"})); err == nil {
		t.Error("accepted a negative STORAGE_BUDGET_BYTES")
	}
	if _, err := Load(env(map[string]string{"STORAGE_BUDGET_BYTES": "lots"})); err == nil {
		t.Error("accepted a non-numeric STORAGE_BUDGET_BYTES")
	}
}

// TestCleanRouteTagIsTheTagGrammar pins this package's restated rule against
// internal/db's CleanTag — DIFFERENTIALLY, by calling both (N064).
//
// The grammar is duplicated so that internal/config keeps depending on the
// standard library alone (§18.1). A hand-written table of literals is not a
// pin on that duplicate: a change to db.CleanTag — '_' becoming legal, say, or
// MaxTagRunes moving — would fail no test in this package, the two spellings
// would silently diverge, and an operator's DISCORD_WEBHOOK_ROUTES rule would
// stop matching the tags users can actually type. So this test IMPORTS
// internal/db and asserts the two agree on every input, which a test file may
// do because a test binary's imports are not the package's — the same argument
// config.go already makes for TestReservedRouteTagIsNotifys and
// internal/notify. internal/db imports nothing of ours but internal/ids, so
// there is no cycle.
func TestCleanRouteTagIsTheTagGrammar(t *testing.T) {
	// The cap is named in both packages; if one moves, the whole grammar has.
	if maxRouteTagRunes != db.MaxTagRunes {
		t.Fatalf("maxRouteTagRunes = %d but db.MaxTagRunes = %d", maxRouteTagRunes, db.MaxTagRunes)
	}

	// The literal table stays: it says what the grammar IS, in one place a
	// reader can check, rather than only that two copies agree with each other.
	good := map[string]string{
		"clutch":                      "clutch",
		"CLUTCH":                      "clutch",
		"  spaced  ":                  "spaced",
		"a":                           "a",
		"0":                           "0",
		"a-b-c":                       "a-b-c",
		"9lives":                      "9lives",
		strings.Repeat("a", 32):       strings.Repeat("a", 32),
		strings.Repeat("a", 31) + "-": strings.Repeat("a", 31) + "-",
	}
	for in, want := range good {
		got, reason := cleanRouteTag(in)
		if reason != "" || got != want {
			t.Errorf("cleanRouteTag(%q) = %q, %q; want %q", in, got, reason, want)
		}
	}
	bad := []string{"", "   ", "-lead", "has space", "has_underscore", "üni", "UPPER CASE",
		strings.Repeat("a", 33), "slash/es", "dot.s", "e:mo"}
	for _, in := range bad {
		if got, reason := cleanRouteTag(in); reason == "" {
			t.Errorf("cleanRouteTag(%q) accepted it as %q", in, got)
		}
	}

	// The differential half: every input above, plus one tag per ASCII byte in
	// three positions (so the whole accepted alphabet and every rejected
	// neighbour of it is covered), plus the shapes the two loops treat
	// specially — a leading '-', the length boundary, multi-byte runes whose
	// Unicode fold would land on a-z, and the separators
	// DISCORD_WEBHOOK_ROUTES itself splits on.
	var inputs []string
	for in := range good {
		inputs = append(inputs, in)
	}
	inputs = append(inputs, bad...)
	for c := 0; c < 128; c++ {
		inputs = append(inputs, string(rune(c)), "a"+string(rune(c)), string(rune(c))+"a")
	}
	inputs = append(inputs,
		"-", "--", "a-", "-a", "a--b",
		strings.Repeat("a", 31), strings.Repeat("a", 32), strings.Repeat("a", 33),
		strings.Repeat("é", 20), strings.Repeat("é", 33), "Ｋ", "K", "İ",
		"tag;other", "tag=url", "\ttab\t", "\nnl\n", " a ", "a b",
	)
	for _, in := range inputs {
		got, reason := cleanRouteTag(in)
		want, err := db.CleanTag(in)
		if (reason == "") != (err == nil) {
			t.Errorf("cleanRouteTag(%q) reason=%q but db.CleanTag err=%v: the two grammars disagree on acceptance", in, reason, err)
			continue
		}
		if err == nil && got != want {
			t.Errorf("cleanRouteTag(%q) = %q but db.CleanTag = %q: the two normalisations disagree", in, got, want)
		}
	}
}
