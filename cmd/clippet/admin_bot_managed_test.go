package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"

	"clippet/internal/config"
	"clippet/internal/httpapi"
	"clippet/internal/ids"
)

// The managed instance (DISCORD_BOT_TOKEN=managed, §20 erratum 279): the CLI
// never tries to publish a command, and `admin doctor`'s bot row says so and
// lists the bound servers.

const managedGuild = "444444444444444444"

func managedEnv(t *testing.T, base func(string) string) func(string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	extra := map[string]string{
		config.EnvDiscordAppID:     "123456789012345678",
		config.EnvDiscordPublicKey: hex.EncodeToString(pub),
		config.EnvDiscordBotToken:  config.ManagedToken,
	}
	return func(k string) string {
		if v, ok := extra[k]; ok {
			return v
		}
		return base(k)
	}
}

func TestAdminBotManagedNeverRegisters(t *testing.T) {
	fx := newAdminFixture(t)
	cfg, err := config.Load(managedEnv(t, testEnv(fx.cfg.DataDir)))
	if err != nil {
		t.Fatal(err)
	}
	fx.cfg = cfg

	// register is refused in words, before anything could reach Discord.
	code, out := fx.run(t, "bot", "register")
	if code != 2 || !strings.Contains(out, "managed") || !strings.Contains(out, "§30.8") {
		t.Fatalf("register on a managed instance: %d %q", code, out)
	}
	code, out = fx.run(t, "bot", "register", managedGuild)
	if code != 2 || !strings.Contains(out, "managed") {
		t.Fatalf("register <guild> on a managed instance: %d %q", code, out)
	}

	// bind WITHOUT --no-register stores the binding and publishes nothing —
	// and says which, in one line, naming the claim page's check.
	code, out = fx.run(t, "bot", "bind", managedGuild, "carol")
	if code != 0 || !strings.Contains(out, "bound to carol") {
		t.Fatalf("bind: %d %q", code, out)
	}
	if !strings.Contains(out, "nothing to publish") || !strings.Contains(out, "origin check") {
		t.Fatalf("bind on a managed instance does not explain itself: %q", out)
	}
	if strings.Contains(out, "NOT published") || strings.Contains(out, "admin bot register") {
		t.Fatalf("bind on a managed instance still points at register: %q", out)
	}
	// --no-register is accepted and says the same thing.
	if code, out := fx.run(t, "bot", "bind", "555555555555555555", "carol", "--no-register"); code != 0 || !strings.Contains(out, "nothing to publish") {
		t.Fatalf("bind --no-register: %d %q", code, out)
	}
	code, out = fx.run(t, "bot", "ls")
	if code != 0 || !strings.Contains(out, "2 binding(s)") || !strings.Contains(out, "discord bot: managed") {
		t.Fatalf("ls: %d %q", code, out)
	}
	// The managed line states the mode and names no command: a list here drifted once (§20 erratum 318).
	if !strings.Contains(out, "discord bot: managed (this instance answers the official bot; nothing is registered from here)") {
		t.Fatalf("ls: the managed line changed: %q", out)
	}
	// help lists the managed behaviour.
	if code, out := fx.run(t, "bot"); code != 2 || !strings.Contains(out, "managed") {
		t.Fatalf("usage does not mention the managed case: %d %q", code, out)
	}
}

func TestDoctorManagedBotRowListsTheBoundServers(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.env[config.EnvDiscordAppID] = "123456789012345678"
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	fx.env[config.EnvDiscordPublicKey] = hex.EncodeToString(pub)
	fx.env[config.EnvDiscordBotToken] = config.ManagedToken
	cfg, err := config.Load(func(k string) string { return fx.env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(cfg); code != 0 {
		t.Fatal("migrate")
	}

	// Nothing bound yet: the row says how to bind.
	code, out := fx.run(t)
	if code != 0 {
		t.Fatalf("doctor: %d\n%s", code, out)
	}
	if v := verdictOf(out, "bot"); v != vOK {
		t.Fatalf("bot = %s on a managed instance with an https origin:\n%s", v, out)
	}
	for _, want := range []string{"token managed", "publishes no command", "§30.8", httpapi.BotClaimPath(), "bound servers: none yet", "admin bot bind"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the managed bot row lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "paste this into the Developer Portal") {
		t.Fatalf("a managed instance is told to paste an endpoint URL:\n%s", out)
	}
	if strings.Contains(out, "redacted") {
		t.Fatalf("the sentinel is not a secret and must not be called redacted:\n%s", out)
	}
	if !strings.Contains(out, "doctor:  19 checks") {
		t.Fatalf("the check count moved:\n%s", out)
	}

	// Bind one server and the row names it, with the account's name.
	d, err := openForTest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	u, err := d.CreateUser(context.Background(), ids.New(), "carol", "h", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.BindGuild(context.Background(), managedGuild, u.ID, 1); err != nil {
		t.Fatal(err)
	}
	d.Close()
	_, out = fx.run(t)
	if !strings.Contains(out, "bound server(s): "+managedGuild+" → carol") {
		t.Fatalf("the managed bot row does not list the binding:\n%s", out)
	}
}
