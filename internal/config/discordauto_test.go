package config

import (
	"strings"
	"testing"
)

// Clippet Auto's four variables (§20 erratum 333).

const testAutoToken = "AUTO.token-0123456789_abcdefghij"

func autoEnv(t *testing.T, mode string) map[string]string {
	t.Helper()
	m := botEnv(t)
	m[EnvDiscordAutoMode] = mode
	m[EnvDiscordAutoToken] = testAutoToken
	return m
}

func TestDiscordAutoOffByDefault(t *testing.T) {
	a := mustLoad(t, nil).DiscordAuto
	if a.On() || a.Mode() != AutoModeOff || a.Seconds() != 10 {
		t.Fatalf("default = %+v", a)
	}
	// With the bot on and Auto unset, still off.
	if mustLoad(t, botEnv(t)).DiscordAuto.On() {
		t.Fatal("the bot alone turned Auto on")
	}
}

func TestDiscordAutoModes(t *testing.T) {
	for _, mode := range []string{"prompt", "auto", "PROMPT"} {
		a := mustLoad(t, autoEnv(t, mode)).DiscordAuto
		if !a.On() || a.Mode() != strings.ToLower(mode) || a.Token() != testAutoToken || !a.Notice() || a.Seconds() != 10 {
			t.Fatalf("%s: %+v", mode, a)
		}
		for _, s := range []string{a.String(), a.LogValue().String()} {
			if strings.Contains(s, testAutoToken) || !strings.Contains(s, "redacted") {
				t.Fatalf("rendering = %q", s)
			}
		}
	}
	m := autoEnv(t, "prompt")
	m[EnvDiscordAutoSeconds] = "60"
	m[EnvDiscordAutoNotice] = "off"
	a := mustLoad(t, m).DiscordAuto
	if a.Seconds() != 60 || a.Notice() {
		t.Fatalf("seconds/notice = %d/%v", a.Seconds(), a.Notice())
	}
}

// Pin: off written out is a deliberate off — the token may stay and nothing
// reads it; a mode unset with any of the three set is an orphan.
func TestDiscordAutoOffAndOrphans(t *testing.T) {
	a := mustLoad(t, autoEnv(t, "off")).DiscordAuto
	if a.On() || a.Token() != "" {
		t.Fatalf("off: %+v", a)
	}
	for _, env := range []string{EnvDiscordAutoToken, EnvDiscordAutoSeconds, EnvDiscordAutoNotice} {
		m := botEnv(t)
		m[env] = map[string]string{EnvDiscordAutoToken: testAutoToken, EnvDiscordAutoSeconds: "10", EnvDiscordAutoNotice: "on"}[env]
		if err := loadErr(t, m); !strings.Contains(err.Error(), env) || !strings.Contains(err.Error(), EnvDiscordAutoMode) {
			t.Fatalf("%s orphan: %v", env, err)
		}
	}
}

func TestDiscordAutoRefusals(t *testing.T) {
	cases := map[string]func(m map[string]string){
		EnvDiscordAutoMode:    func(m map[string]string) { m[EnvDiscordAutoMode] = "always" },
		EnvDiscordAutoToken:   func(m map[string]string) { delete(m, EnvDiscordAutoToken) },
		EnvDiscordAutoSeconds: func(m map[string]string) { m[EnvDiscordAutoSeconds] = "4" },
		EnvDiscordAutoNotice:  func(m map[string]string) { m[EnvDiscordAutoNotice] = "maybe" },
	}
	for env, mut := range cases {
		m := autoEnv(t, "auto")
		mut(m)
		if err := loadErr(t, m); !strings.Contains(err.Error(), env) {
			t.Fatalf("%s: %v", env, err)
		}
	}
	m := autoEnv(t, "auto")
	m[EnvDiscordAutoSeconds] = "61"
	if err := loadErr(t, m); !strings.Contains(err.Error(), "<= 60") {
		t.Fatalf("61 s: %v", err)
	}
	// The official bot's sentinel is never Auto's token, and a bad token is
	// named, never echoed.
	m = autoEnv(t, "prompt")
	m[EnvDiscordAutoToken] = ManagedToken
	if err := loadErr(t, m); !strings.Contains(err.Error(), EnvDiscordAutoToken) {
		t.Fatalf("managed token: %v", err)
	}
	m[EnvDiscordAutoToken] = "not a token with spaces 0123456789"
	if err := loadErr(t, m); strings.Contains(err.Error(), "spaces 0123456789") {
		t.Fatalf("the token was echoed: %v", err)
	}
}

// Pin: prompt|auto needs §30's bot — own or managed.
func TestDiscordAutoNeedsTheBot(t *testing.T) {
	m := map[string]string{EnvDiscordAutoMode: "prompt", EnvDiscordAutoToken: testAutoToken}
	if err := loadErr(t, m); !strings.Contains(err.Error(), EnvDiscordAppID) {
		t.Fatalf("no bot: %v", err)
	}
	m = autoEnv(t, "auto")
	m[EnvDiscordBotToken] = ManagedToken
	if !mustLoad(t, m).DiscordAuto.On() {
		t.Fatal("Auto beside the official bot was refused")
	}
}

// Pin: the gateway rule — Discord's gateway host or a regional one, wss only.
func TestValidGatewayURL(t *testing.T) {
	for _, ok := range []string{"wss://gateway.discord.gg", "wss://gateway.discord.gg/", "wss://GATEWAY.discord.gg",
		"wss://gateway-us-east1-b.discord.gg"} {
		if _, err := ValidGatewayURL(ok); err != nil {
			t.Errorf("%s refused", ok)
		}
	}
	for _, bad := range []string{"ws://gateway.discord.gg", "https://gateway.discord.gg", "wss://gateway.discord.gg:443",
		"wss://u:p@gateway.discord.gg", "wss://evil.example", "wss://gateway.discord.gg.evil.example",
		"wss://gateway-x.discord.gg.evil", "wss://xgateway.discord.gg", "wss://gateway.discord.gg/other",
		"wss://gatewаy.discord.gg", "wss://discord.gg"} {
		if _, err := ValidGatewayURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
