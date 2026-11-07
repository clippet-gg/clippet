package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// Sign in with Discord's configuration (ROADMAP #44, §20 erratum 302): both or
// none, the shapes, the redaction, and its independence from the bot.

const (
	testLoginClientID = "876543210987654321"
	testLoginSecret   = "aBcDeFgHiJkLmNoPqRsTuVwXyZ012345"
)

func loginEnv() map[string]string {
	return map[string]string{
		EnvDiscordLoginClientID:     testLoginClientID,
		EnvDiscordLoginClientSecret: testLoginSecret,
	}
}

func TestDiscordLoginOffByDefault(t *testing.T) {
	c := mustLoad(t, nil)
	if c.DiscordLogin.Configured() {
		t.Fatal("the login is configured with neither variable set")
	}
	if c.DiscordLogin.ClientID() != "" || c.DiscordLogin.Secret() != "" {
		t.Fatalf("the zero value carries something: %+v", c.DiscordLogin)
	}
	if s := c.DiscordLogin.String(); s != "(unset)" {
		t.Fatalf("String() = %q, want (unset)", s)
	}
}

func TestDiscordLoginBothOrNone(t *testing.T) {
	for _, only := range []string{EnvDiscordLoginClientID, EnvDiscordLoginClientSecret} {
		err := loadErr(t, map[string]string{only: loginEnv()[only]})
		msg := err.Error()
		for _, name := range []string{EnvDiscordLoginClientID, EnvDiscordLoginClientSecret} {
			if !strings.Contains(msg, name) {
				t.Fatalf("with only %s set the error does not name %s: %v", only, name, err)
			}
		}
		if strings.Contains(msg, testLoginSecret) {
			t.Fatalf("the error echoes the secret: %v", err)
		}
	}
}

func TestDiscordLoginShapes(t *testing.T) {
	c := mustLoad(t, loginEnv())
	if !c.DiscordLogin.Configured() || c.DiscordLogin.ClientID() != testLoginClientID || c.DiscordLogin.Secret() != testLoginSecret {
		t.Fatalf("loaded: %+v", c.DiscordLogin)
	}
	for _, tc := range []struct {
		name, key, value string
	}{
		{"a short id", EnvDiscordLoginClientID, "12345"},
		{"a non-numeric id", EnvDiscordLoginClientID, "12345678901234567x"},
		{"a URL as the id", EnvDiscordLoginClientID, "discord.com/oauth2/authorize"},
		{"a short secret", EnvDiscordLoginClientSecret, "tooshort"},
		{"a secret with a space", EnvDiscordLoginClientSecret, "aBcDeFgHiJkLmNoPq RsTuVwXyZ012345"},
		{"a secret past the ceiling", EnvDiscordLoginClientSecret, strings.Repeat("a", maxClientSecretChars+1)},
	} {
		over := loginEnv()
		over[tc.key] = tc.value
		err := loadErr(t, over)
		if !strings.Contains(err.Error(), tc.key) {
			t.Fatalf("%s: the error does not name %s: %v", tc.name, tc.key, err)
		}
		if tc.key == EnvDiscordLoginClientSecret && strings.Contains(err.Error(), tc.value) {
			t.Fatalf("%s: the error echoes the secret value: %v", tc.name, err)
		}
	}
}

// The secret never renders: not through String, not through slog, not through
// a %v of the whole Config.
func TestDiscordLoginNeverPrintsTheSecret(t *testing.T) {
	c := mustLoad(t, loginEnv())
	for name, s := range map[string]string{
		"String":   c.DiscordLogin.String(),
		"LogValue": c.DiscordLogin.LogValue().String(),
		"%v":       fmt.Sprintf("%v", c.DiscordLogin),
		"%+v":      fmt.Sprintf("%+v", c),
	} {
		if strings.Contains(s, testLoginSecret) {
			t.Fatalf("%s leaks the secret: %s", name, s)
		}
	}
	if s := c.DiscordLogin.String(); s != "client "+testLoginClientID+", secret (redacted)" {
		t.Fatalf("String() = %q", s)
	}
	var sb strings.Builder
	slog.New(slog.NewTextHandler(&sb, nil)).Info("x", "login", c.DiscordLogin)
	if strings.Contains(sb.String(), testLoginSecret) || !strings.Contains(sb.String(), "redacted") {
		t.Fatalf("slog rendering: %s", sb.String())
	}
}

// Independent of the bot: the login on its own, the bot on its own, and both
// together with the SAME application id all load.
func TestDiscordLoginIsIndependentOfTheBot(t *testing.T) {
	c := mustLoad(t, loginEnv())
	if c.DiscordBot.Configured() {
		t.Fatal("the login configured the bot")
	}
	both := botEnv(t)
	for k, v := range loginEnv() {
		both[k] = v
	}
	both[EnvDiscordLoginClientID] = testAppID // the own-bot instance reuses its application
	c = mustLoad(t, both)
	if !c.DiscordBot.Configured() || !c.DiscordLogin.Configured() || c.DiscordLogin.ClientID() != testAppID {
		t.Fatalf("both: bot=%v login=%v", c.DiscordBot, c.DiscordLogin)
	}
	managed := botEnv(t)
	managed[EnvDiscordBotToken] = ManagedToken
	for k, v := range loginEnv() {
		managed[k] = v
	}
	c = mustLoad(t, managed)
	if !c.DiscordBot.Managed() || !c.DiscordLogin.Configured() {
		t.Fatalf("managed + login: bot=%v login=%v", c.DiscordBot, c.DiscordLogin)
	}
}
