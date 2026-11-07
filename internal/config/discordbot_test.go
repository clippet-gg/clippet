package config

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
)

// The Discord bot's configuration (ROADMAP #33, §20 errata 253-255): all three
// or none, the shapes, the redaction, and the two new egress hosts.

const (
	testAppID    = "123456789012345678"
	testBotToken = "MTIzNDU2Nzg5MDEyMzQ1Njc4.GhIjKl.mNoPqRsTuVwXyZ0123456789abcdef"
)

// testPubKey is a real Ed25519 public key in the portal's spelling.
func testPubKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(pub)
}

func botEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		EnvDiscordAppID:     testAppID,
		EnvDiscordPublicKey: testPubKey(t),
		EnvDiscordBotToken:  testBotToken,
	}
}

func TestDiscordBotOffByDefault(t *testing.T) {
	c := mustLoad(t, nil)
	if c.DiscordBot.Configured() {
		t.Fatal("the bot is configured with none of its variables set")
	}
	if c.DiscordBot.AppID() != "" || c.DiscordBot.Token() != "" || len(c.DiscordBot.PublicKey()) != 0 {
		t.Fatalf("the zero value carries something: %+v", c.DiscordBot)
	}
	if s := c.DiscordBot.String(); s != "(unset)" {
		t.Fatalf("String() = %q, want (unset)", s)
	}
}

func TestDiscordBotAllThreeOrNone(t *testing.T) {
	full := botEnv(t)
	for _, missing := range []string{EnvDiscordAppID, EnvDiscordPublicKey, EnvDiscordBotToken} {
		part := map[string]string{}
		for k, v := range full {
			if k != missing {
				part[k] = v
			}
		}
		err := loadErr(t, part)
		// The message must name all three, because the operator's next action
		// is to look at the one they forgot.
		for _, name := range []string{EnvDiscordAppID, EnvDiscordPublicKey, EnvDiscordBotToken} {
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("without %s the error does not name %s: %v", missing, name, err)
			}
		}
		if strings.Contains(err.Error(), testBotToken) {
			t.Fatalf("the error echoed the bot token: %v", err)
		}
	}
	// Exactly one set is the same failure, not a different one.
	if err := loadErr(t, map[string]string{EnvDiscordBotToken: testBotToken}); !strings.Contains(err.Error(), "all three") {
		t.Fatalf("one variable alone = %v", err)
	}
}

func TestDiscordBotValidatesEachShape(t *testing.T) {
	good := botEnv(t)
	cases := []struct {
		name, key, value, want string
	}{
		{"a URL in the app id", EnvDiscordAppID, "https://discord.com/x", "digits"},
		{"a short app id", EnvDiscordAppID, "1234", "digits"},
		{"a public key that is not hex", EnvDiscordPublicKey, strings.Repeat("z", 64), "hex"},
		{"a public key of the wrong length", EnvDiscordPublicKey, strings.Repeat("ab", 16), "hex"},
		{"a token with a space", EnvDiscordBotToken, "not a token but long enough to pass", "characters"},
		{"a token that is too short", EnvDiscordBotToken, "short", "characters"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := map[string]string{}
			for k, v := range good {
				m[k] = v
			}
			m[c.key] = c.value
			err := loadErr(t, m)
			if !strings.Contains(err.Error(), c.key) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s = %v, want a message naming %s and %q", c.name, err, c.key, c.want)
			}
			if strings.Contains(err.Error(), c.value) && c.key == EnvDiscordBotToken {
				t.Fatalf("the rejection echoed the token: %v", err)
			}
		})
	}
}

func TestDiscordBotConfiguredCarriesTheKeyAndRedactsTheToken(t *testing.T) {
	m := botEnv(t)
	c := mustLoad(t, m)
	b := c.DiscordBot
	if !b.Configured() {
		t.Fatal("the bot is not configured with all three set")
	}
	if b.AppID() != testAppID {
		t.Fatalf("AppID = %q", b.AppID())
	}
	if len(b.PublicKey()) != ed25519.PublicKeySize {
		t.Fatalf("PublicKey is %d bytes, want %d", len(b.PublicKey()), ed25519.PublicKeySize)
	}
	if hex.EncodeToString(b.PublicKey()) != b.PublicKeyHex() {
		t.Fatal("PublicKeyHex does not spell PublicKey")
	}
	if b.Token() != testBotToken {
		t.Fatal("Token() does not return the configured token")
	}
	// The redaction: neither rendering may carry the token, and both must
	// still identify WHICH application is live (erratum 63's discipline).
	for _, s := range []string{b.String(), b.LogValue().String()} {
		if strings.Contains(s, testBotToken) {
			t.Fatalf("a rendering carries the token: %s", s)
		}
		if !strings.Contains(s, testAppID) {
			t.Fatalf("a rendering does not name the application: %s", s)
		}
		if !strings.Contains(s, "redacted") {
			t.Fatalf("a rendering does not say the token was redacted: %s", s)
		}
	}
	// An uppercase public key is what the portal shows on some pages; it is
	// the same key.
	upper := map[string]string{}
	for k, v := range m {
		upper[k] = v
	}
	upper[EnvDiscordPublicKey] = strings.ToUpper(m[EnvDiscordPublicKey])
	if got := mustLoad(t, upper).DiscordBot.PublicKeyHex(); got != strings.ToLower(m[EnvDiscordPublicKey]) {
		t.Fatalf("an upper-case key loaded as %q", got)
	}
}

// TestAttachmentAllowlist is the §15 gate on the ONE new egress the bot adds.
func TestAttachmentAllowlist(t *testing.T) {
	ok := []string{
		"https://cdn.discordapp.com/attachments/1/2/clip.mp4",
		"https://media.discordapp.net/attachments/1/2/clip.mp4?ex=1&is=2&hm=abc",
		"https://CDN.discordapp.com/attachments/1/2/clip.mp4",
	}
	for _, raw := range ok {
		if _, err := ValidAttachmentURL(raw); err != nil {
			t.Fatalf("%s was refused: %v", raw, err)
		}
	}
	bad := []string{
		"",
		"http://cdn.discordapp.com/a.mp4", // plain http
		"https://cdn.discordapp.com.evil.example/a.mp4", // a lookalike
		"https://discord.com/api/webhooks/1/tok",        // the API host is not a CDN
		"https://cdn.discordapp.com:8443/a.mp4",         // a port
		"https://user:pw@cdn.discordapp.com/a.mp4",      // userinfo
		"https://minio:9000/clips/x",                    // the compose network
		"http://host.docker.internal/x",
		"file:///etc/passwd",
		"//cdn.discordapp.com/a.mp4",
		// U+0130 folds to an ASCII "i" under strings.ToLower; net/http would
		// dial the IDNA form, a different domain (erratum 324 (c)).
		"https://cdn.dİscordapp.com/attachments/12345678901234567/12345678901234567/a.mp4",
		"https://cdn.d%C4%B0scordapp.com/attachments/12345678901234567/12345678901234567/a.mp4",
		"https://medİa.dİscordapp.net/attachments/12345678901234567/12345678901234567/a.mp4",
	}
	for _, raw := range bad {
		if u, err := ValidAttachmentURL(raw); err == nil {
			t.Fatalf("%s was allowed as %s", raw, u)
		}
	}
}

// TestDiscordAPIHostIsAlreadyAnAllowedWebhookHost pins the claim erratum 254
// makes: the bot's API host is one the §15 destination list already carries,
// so the bot adds two CDN hosts and no new NAME.
func TestDiscordAPIHostIsAlreadyAnAllowedWebhookHost(t *testing.T) {
	if !discordHosts[DiscordAPIHost()] {
		t.Fatalf("the bot's API host %q is not in the webhook allowlist %v", DiscordAPIHost(), sortedKeys(discordHosts))
	}
	hosts := DiscordCDNHosts()
	if len(hosts) != 2 {
		t.Fatalf("the attachment allowlist is %v, want exactly the two CDN hosts", hosts)
	}
	for _, h := range hosts {
		if discordHosts[h] {
			t.Fatalf("%q is in both allowlists; the webhook and the attachment rules are different rules", h)
		}
	}
}
