package main

import (
	"strings"
	"testing"
)

// `clippet admin bot` (ROADMAP #33, §20 erratum 255). Every test here uses
// --no-register: publishing the command is an outbound call to Discord, which
// internal/notify's own tests drive against an httptest server and which the
// CLI must never make from a unit test.

const testGuild = "222222222222222222"

func TestAdminBotBindLsUnbind(t *testing.T) {
	fx := newAdminFixture(t)

	code, out := fx.run(t, "bot", "ls")
	if code != 0 || !strings.Contains(out, "0 binding(s)") {
		t.Fatalf("ls on a fresh instance: %d %q", code, out)
	}
	// The bot is off in this fixture, and `ls` says so — a binding that does
	// nothing is exactly the thing an operator needs telling about.
	if !strings.Contains(out, "discord bot: off") {
		t.Fatalf("ls does not report the off state: %q", out)
	}

	code, out = fx.run(t, "bot", "bind", testGuild, "carol", "--no-register")
	if code != 0 || !strings.Contains(out, "bound to carol") {
		t.Fatalf("bind: %d %q", code, out)
	}
	if !strings.Contains(out, "NOT published") {
		t.Fatalf("--no-register does not say the command was not published: %q", out)
	}

	code, out = fx.run(t, "bot", "ls")
	if code != 0 || !strings.Contains(out, testGuild) || !strings.Contains(out, "carol") || !strings.Contains(out, "1 binding(s)") {
		t.Fatalf("ls after bind: %d %q", code, out)
	}

	code, out = fx.run(t, "bot", "unbind", testGuild)
	if code != 0 || !strings.Contains(out, "unbound") {
		t.Fatalf("unbind: %d %q", code, out)
	}
	code, out = fx.run(t, "bot", "unbind", testGuild)
	if code != 0 || !strings.Contains(out, "no binding") {
		t.Fatalf("unbinding twice: %d %q", code, out)
	}
	code, out = fx.run(t, "bot", "ls")
	if code != 0 || !strings.Contains(out, "0 binding(s)") {
		t.Fatalf("ls after unbind: %d %q", code, out)
	}
}

func TestAdminBotUsageErrors(t *testing.T) {
	fx := newAdminFixture(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no verb", []string{"bot"}, "usage"},
		{"an unknown verb", []string{"bot", "wat"}, "usage"},
		{"bind with no account", []string{"bot", "bind", testGuild}, "usage"},
		{"bind with a bad guild id", []string{"bot", "bind", "not-a-snowflake", "carol", "--no-register"}, "not a Discord server id"},
		{"bind to an account that does not exist", []string{"bot", "bind", testGuild, "nobody", "--no-register"}, "no such user"},
		{"unbind with no id", []string{"bot", "unbind"}, "usage"},
		{"register with a bad guild id", []string{"bot", "register", "12"}, "not a Discord server id"},
		{"register an unbound guild", []string{"bot", "register", testGuild}, "is not bound"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out := fx.run(t, c.args...)
			if code == 0 {
				t.Fatalf("%v exited 0: %q", c.args, out)
			}
			if !strings.Contains(out, c.want) {
				t.Fatalf("%v = %q, want it to mention %q", c.args, out, c.want)
			}
		})
	}
}

// TestAdminBotRegisterNeedsTheBotToBeOn: with the three variables unset there
// is nothing to authenticate with, and the CLI says which ones are missing
// rather than attempting a call it cannot make.
func TestAdminBotRegisterNeedsTheBotToBeOn(t *testing.T) {
	fx := newAdminFixture(t)
	if code, out := fx.run(t, "bot", "bind", testGuild, "carol", "--no-register"); code != 0 {
		t.Fatalf("bind: %d %q", code, out)
	}
	code, out := fx.run(t, "bot", "register")
	if code != 2 || !strings.Contains(out, "DISCORD_APP_ID") {
		t.Fatalf("register with the bot off: %d %q", code, out)
	}
	// And a bind that WOULD register says the same thing rather than dying in
	// a way that hides the binding it just made.
	if code, out := fx.run(t, "bot", "bind", "333333333333333333", "carol"); code != 2 || !strings.Contains(out, "DISCORD_APP_ID") {
		t.Fatalf("bind without --no-register on an off bot: %d %q", code, out)
	}
	if code, out := fx.run(t, "bot", "ls"); code != 0 || !strings.Contains(out, "2 binding(s)") {
		t.Fatalf("the binding was not stored despite the failed registration: %d %q", code, out)
	}
}

// TestAdminUsageListsTheBotVerbs keeps `admin help` honest: every subcommand
// an operator can run is in it (DEPLOY §30 points at it).
func TestAdminUsageListsTheBotVerbs(t *testing.T) {
	fx := newAdminFixture(t)
	code, out := fx.run(t, "help")
	if code != 0 {
		t.Fatalf("help: %d", code)
	}
	for _, want := range []string{"bot bind", "bot unbind", "bot ls", "bot register"} {
		if !strings.Contains(out, want) {
			t.Fatalf("`admin help` does not list %q:\n%s", want, out)
		}
	}
}
