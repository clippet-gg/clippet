package config

import (
	"strings"
	"testing"
)

// The official-bot sentinel (§20 erratum 279): DISCORD_BOT_TOKEN=managed
// passes the "all three or none" rule as itself, is reported honestly, and
// changes nothing about the other two variables' shapes.

func TestDiscordBotManagedSentinel(t *testing.T) {
	m := botEnv(t)
	m[EnvDiscordBotToken] = ManagedToken
	c := mustLoad(t, m)
	b := c.DiscordBot
	if !b.Configured() || !b.Managed() {
		t.Fatalf("managed sentinel: configured=%v managed=%v", b.Configured(), b.Managed())
	}
	if b.Token() != ManagedToken {
		t.Fatalf("Token() = %q, want the sentinel", b.Token())
	}
	if b.AppID() != testAppID || len(b.PublicKey()) == 0 {
		t.Fatal("the other two variables were not carried")
	}
	// The rendering says the word — it is not a secret — and never "redacted",
	// which would send an operator hunting for a token that does not exist.
	for _, s := range []string{b.String(), b.LogValue().String()} {
		if !strings.Contains(s, "managed") || strings.Contains(s, "redacted") {
			t.Fatalf("rendering = %q", s)
		}
		if !strings.Contains(s, testAppID) {
			t.Fatalf("rendering does not name the application: %s", s)
		}
	}
	// A real token is not managed, and neither is a token that merely starts
	// with the word: the sentinel is the exact spelling.
	if mustLoad(t, botEnv(t)).DiscordBot.Managed() {
		t.Fatal("a real token reads as managed")
	}
	m[EnvDiscordBotToken] = "managed-by-clippet-gg-no-registration"
	if mustLoad(t, m).DiscordBot.Managed() {
		t.Fatal("a token-shaped value starting with the word reads as managed")
	}
	// The sentinel alone is still "one of three": the all-or-none rule is
	// untouched.
	if err := loadErr(t, map[string]string{EnvDiscordBotToken: ManagedToken}); !strings.Contains(err.Error(), "all three") {
		t.Fatalf("the sentinel alone = %v", err)
	}
	// And the charset rule is exactly as strict for everything else.
	m[EnvDiscordBotToken] = "manage d"
	if err := loadErr(t, m); !strings.Contains(err.Error(), "characters") {
		t.Fatalf("a near-miss with a space = %v", err)
	}
	if !b.Managed() || (DiscordBot{}).Managed() {
		t.Fatal("the zero value must not be managed")
	}
}
