package main

import (
	"strings"
	"testing"
)

// `admin bot bind --person` and the unlink guard (migration 0024, §20 erratum
// 342). --no-register throughout, as in admin_bot_test.go.

const personDiscordID = "444444444444444444"

func TestAdminBotBindPerson(t *testing.T) {
	fx := newAdminFixture(t)

	// Refused while the account has no Discord user id, in the sentence that
	// names the next command — and nothing is stored.
	code, out := fx.run(t, "bot", "bind", testGuild, "carol", "--person", "--no-register")
	want := "admin bot bind: carol has no Discord user id - run `clippet admin users link carol <discord-id>` first"
	if code != 1 || !strings.Contains(out, want) {
		t.Fatalf("bind --person without an id: %d %q", code, out)
	}
	if _, out = fx.run(t, "bot", "ls"); !strings.Contains(out, "0 binding(s)") {
		t.Fatalf("the refused bind stored a row: %q", out)
	}

	if code, out = fx.run(t, "users", "link", "carol", personDiscordID); code != 0 {
		t.Fatalf("users link: %d %q", code, out)
	}
	code, out = fx.run(t, "bot", "bind", testGuild, "carol", "--person", "--no-register")
	if code != 0 || !strings.Contains(out, "guild "+testGuild+" bound to carol (") || !strings.Contains(out, ") (person)\n") {
		t.Fatalf("bind --person: %d %q", code, out)
	}
	code, out = fx.run(t, "bot", "ls")
	if code != 0 || !lsRowEndsWith(out, testGuild, "  person") {
		t.Fatalf("ls does not show the person scope: %d %q", code, out)
	}

	// The account's link is held while the person binding names it.
	code, out = fx.run(t, "users", "unlink", "carol")
	want = "admin users unlink: carol has a person binding (guild " + testGuild + ") - run `clippet admin bot unbind " + testGuild + "` first"
	if code != 1 || !strings.Contains(out, want) {
		t.Fatalf("unlink under a person binding: %d %q", code, out)
	}

	// A rebind without --person makes the server every member's again, and
	// says it changed; the link may then go.
	code, out = fx.run(t, "bot", "bind", testGuild, "carol", "--no-register")
	if code != 0 || !strings.Contains(out, "rebound to carol") || strings.Contains(out, "(person)") {
		t.Fatalf("rebind as server: %d %q", code, out)
	}
	if _, out = fx.run(t, "bot", "ls"); !lsRowEndsWith(out, testGuild, "  server") {
		t.Fatalf("ls after the server rebind: %q", out)
	}
	if code, out = fx.run(t, "users", "unlink", "carol"); code != 0 || !strings.Contains(out, "unlinked from Discord user") {
		t.Fatalf("unlink under a server binding: %d %q", code, out)
	}
}

// TestAdminUsersUnlinkAfterUnbind: the refusal's own advice works.
func TestAdminUsersUnlinkAfterUnbind(t *testing.T) {
	fx := newAdminFixture(t)
	if code, out := fx.run(t, "users", "link", "carol", personDiscordID); code != 0 {
		t.Fatalf("users link: %d %q", code, out)
	}
	if code, out := fx.run(t, "bot", "bind", testGuild, "carol", "--person", "--no-register"); code != 0 {
		t.Fatalf("bind --person: %d %q", code, out)
	}
	if code, out := fx.run(t, "bot", "unbind", testGuild); code != 0 {
		t.Fatalf("unbind: %d %q", code, out)
	}
	if code, out := fx.run(t, "users", "unlink", "carol"); code != 0 || !strings.Contains(out, "unlinked from Discord user") {
		t.Fatalf("unlink after unbind: %d %q", code, out)
	}
}

// lsRowEndsWith finds the guild's row in `bot ls` and checks its last column.
func lsRowEndsWith(out, guild, suffix string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, guild+"  ") {
			return strings.HasSuffix(line, suffix)
		}
	}
	return false
}
