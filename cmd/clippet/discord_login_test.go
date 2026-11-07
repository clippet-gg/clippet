package main

import (
	"context"
	"strings"
	"testing"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
)

// The operator's half of "Sign in with Discord" (ROADMAP #44, §20 erratum
// 302): `admin users link|unlink`, the `discord=` column, and the doctor's
// `login` row.

const (
	testDiscordID    = "111111111111111111"
	testDiscordOther = "222222222222222222"
)

func TestAdminUsersLinkUnlink(t *testing.T) {
	fx := newAdminFixture(t)
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser(context.Background(), ids.New(), "friend", "h", 2); err != nil {
		t.Fatal(err)
	}
	d.Close()

	// Usage: no arguments, one argument, a bad id, an unknown user.
	if code, out := fx.run(t, "users", "link"); code != 2 || !strings.Contains(out, "usage") {
		t.Fatalf("link with no args: %d %q", code, out)
	}
	if code, out := fx.run(t, "users", "link", "carol"); code != 2 || !strings.Contains(out, "usage") {
		t.Fatalf("link with one arg: %d %q", code, out)
	}
	if code, out := fx.run(t, "users", "link", "carol", "not-an-id"); code != 2 || !strings.Contains(out, "not a Discord user id") {
		t.Fatalf("link with a bad id: %d %q", code, out)
	}
	if code, out := fx.run(t, "users", "link", "nobody", testDiscordID); code != 1 || !strings.Contains(out, "no such user") {
		t.Fatalf("link of an unknown user: %d %q", code, out)
	}
	// The listing before: nobody linked.
	if code, out := fx.run(t, "users"); code != 0 || strings.Count(out, "discord=-") != 2 {
		t.Fatalf("users before: %d %q", code, out)
	}
	// The link.
	code, out := fx.run(t, "users", "link", "carol", testDiscordID)
	if code != 0 || !strings.Contains(out, "linked ") || !strings.Contains(out, "carol") || !strings.Contains(out, "discord="+testDiscordID) {
		t.Fatalf("link: %d %q", code, out)
	}
	if code, out := fx.run(t, "users"); code != 0 || !strings.Contains(out, "carol  created=") || !strings.Contains(out, "discord="+testDiscordID) || !strings.Contains(out, "discord=-") {
		t.Fatalf("users after the link: %d %q", code, out)
	}
	// The same id on another account is refused, NAMING the first (the CLI is
	// the one surface that may).
	if code, out := fx.run(t, "users", "link", "friend", testDiscordID); code != 1 || !strings.Contains(out, "already linked to carol") {
		t.Fatalf("link of a taken id: %d %q", code, out)
	}
	// A second link on a linked account is refused and points at unlink.
	if code, out := fx.run(t, "users", "link", "carol", testDiscordOther); code != 1 || !strings.Contains(out, "already linked to Discord user "+testDiscordID) || !strings.Contains(out, "users unlink carol") {
		t.Fatalf("second link: %d %q", code, out)
	}
	d, _ = openForTest(fx.cfg)
	u, _ := d.GetUserByUsername(context.Background(), "carol")
	d.Close()
	if u.DiscordID == nil || *u.DiscordID != testDiscordID {
		t.Fatalf("the refusals changed the row: %+v", u)
	}
	// Unlink: usage, unknown user, a user with no link, then the real one.
	if code, out := fx.run(t, "users", "unlink"); code != 2 || !strings.Contains(out, "usage") {
		t.Fatalf("unlink with no args: %d %q", code, out)
	}
	if code, out := fx.run(t, "users", "unlink", "nobody"); code != 1 || !strings.Contains(out, "no such user") {
		t.Fatalf("unlink of an unknown user: %d %q", code, out)
	}
	if code, out := fx.run(t, "users", "unlink", "friend"); code != 0 || !strings.Contains(out, "friend: no Discord link") {
		t.Fatalf("unlink of an unlinked user: %d %q", code, out)
	}
	if code, out := fx.run(t, "users", "unlink", "carol"); code != 0 || !strings.Contains(out, "carol unlinked from Discord user "+testDiscordID) {
		t.Fatalf("unlink: %d %q", code, out)
	}
	// …and the id is free for the other account.
	if code, out := fx.run(t, "users", "link", "friend", testDiscordID); code != 0 || !strings.Contains(out, "friend") {
		t.Fatalf("relink on the other account: %d %q", code, out)
	}
	if code, out := fx.run(t, "users"); code != 0 || strings.Count(out, "discord="+testDiscordID) != 1 {
		t.Fatalf("users at the end: %d %q", code, out)
	}
}

// The doctor's `login` row: off by default, on with the redirect URI to paste
// and never the secret, a warning on a plain-http public origin.
func TestDoctorLoginRow(t *testing.T) {
	fx := newDoctorFixture(t)
	code, out := fx.run(t)
	if code != 0 {
		t.Fatalf("doctor: %d\n%s", code, out)
	}
	if v := verdictOf(out, "login"); v != vOK {
		t.Fatalf("login row off = %s:\n%s", v, out)
	}
	if !strings.Contains(out, "discord login: off (DISCORD_LOGIN_CLIENT_ID and DISCORD_LOGIN_CLIENT_SECRET are unset)") || !strings.Contains(out, "/api/auth/discord/start is not served") {
		t.Fatalf("the off row:\n%s", out)
	}

	const secret = "s3cr3t-LoginValueNeverPrinted-01"
	fx.env[config.EnvDiscordLoginClientID] = "876543210987654321"
	fx.env[config.EnvDiscordLoginClientSecret] = secret
	code, out = fx.run(t)
	if code != 0 || verdictOf(out, "login") != vOK {
		t.Fatalf("login row on: %d %s\n%s", code, verdictOf(out, "login"), out)
	}
	if !strings.Contains(out, "discord login: on - client 876543210987654321, secret (redacted)") ||
		!strings.Contains(out, "Redirect URI (paste this into the Developer Portal → OAuth2 → Redirects, byte for byte): https://clips.example/api/auth/discord/callback") {
		t.Fatalf("the on row:\n%s", out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("doctor printed the client secret:\n%s", out)
	}
	// A plain-http PUBLIC origin: Discord will refuse the redirect.
	fx.env["PUBLIC_BASE_URL"] = "http://clips.example"
	_, out = fx.run(t)
	if verdictOf(out, "login") != vWarn || !strings.Contains(out, "plain-http redirect URI only on localhost") {
		t.Fatalf("plain-http public origin: %s\n%s", verdictOf(out, "login"), out)
	}
	// …but loopback is fine: a lane stack and a laptop.
	fx.env["PUBLIC_BASE_URL"] = "http://localhost:8530"
	_, out = fx.run(t)
	if verdictOf(out, "login") != vOK || !strings.Contains(out, "http://localhost:8530/api/auth/discord/callback") {
		t.Fatalf("loopback origin: %s\n%s", verdictOf(out, "login"), out)
	}
	// One variable alone is a config failure the doctor REPORTS, naming both,
	// and the secret is still redacted from every line.
	delete(fx.env, config.EnvDiscordLoginClientID)
	code, out = fx.run(t)
	if code != 1 || !strings.Contains(out, "DISCORD_LOGIN_CLIENT_ID") || !strings.Contains(out, "DISCORD_LOGIN_CLIENT_SECRET") || strings.Contains(out, secret) {
		t.Fatalf("half-configured: %d\n%s", code, out)
	}
}

// The link's row survives a prune of OTHER users and goes with its own.
func TestAdminUsersPruneTakesTheLink(t *testing.T) {
	fx := newAdminFixture(t)
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	u, err := d.CreateUser(context.Background(), ids.New(), "tmp_user", "h", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.LinkDiscord(context.Background(), u.ID, testDiscordID, "x", 3); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if code, out := fx.run(t, "users", "prune", "tmp_%", "--older-than", "1s", "--no-clips"); code != 0 || !strings.Contains(out, "pruned 1 user") {
		t.Fatalf("prune: %d %q", code, out)
	}
	d, _ = openForTest(fx.cfg)
	defer d.Close()
	if _, err := d.GetUserByDiscordID(context.Background(), testDiscordID); err != db.ErrNotFound {
		t.Fatalf("the link outlived the prune: %v", err)
	}
}

// The `login` row's `/signin` line (§20 erratum 341): it names the mode; a
// Discord mode with no bot is a warning (nobody new can get in that way).
func TestDoctorSigninRow(t *testing.T) {
	fx := newDoctorFixture(t)
	_, out := fx.run(t)
	if !strings.Contains(out, "/signin: off (REGISTRATION=first;") {
		t.Fatalf("the off line:\n%s", out)
	}
	fx.env["REGISTRATION"] = "members"
	_, out = fx.run(t)
	if verdictOf(out, "login") != vWarn || !strings.Contains(out, "/signin: REGISTRATION=members but the Discord bot is off") {
		t.Fatalf("members without a bot: %s\n%s", verdictOf(out, "login"), out)
	}
	fx.env[config.EnvDiscordAppID] = "123456789012345678"
	fx.env[config.EnvDiscordPublicKey] = strings.Repeat("ab", 32)
	fx.env[config.EnvDiscordBotToken] = "managed"
	_, out = fx.run(t)
	if verdictOf(out, "login") != vOK || !strings.Contains(out, "/signin: on - REGISTRATION=members: any member of a bound server") {
		t.Fatalf("members with the bot: %s\n%s", verdictOf(out, "login"), out)
	}
	fx.env["REGISTRATION"] = "role:666666666666666601"
	_, out = fx.run(t)
	if verdictOf(out, "login") != vOK || !strings.Contains(out, "/signin: on - REGISTRATION=role:666666666666666601: a member of a bound server holding that role") {
		t.Fatalf("role with the bot: %s\n%s", verdictOf(out, "login"), out)
	}
}
