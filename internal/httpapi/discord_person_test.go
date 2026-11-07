package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"clippet/internal/db"
)

// A PERSON binding (migration 0024, §20 erratum 342): the box consented for
// one member of the server, the bound account's linked Discord user, and
// guildBinding refuses every other invoker before anything is fetched.

const (
	personGuild  = "222222222222222222"
	personLinked = "444444444444444444"
	personOther  = "555555555555555555"
)

// asMember lays a member object over an interaction body built by the helpers
// in discord_test.go; id "" leaves the member without a user.
func asMember(t *testing.T, body, id string) string {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{"permissions": "0"}
	if id != "" {
		m["user"] = map[string]any{"id": id, "username": "member" + id[:4]}
	}
	v["member"] = m
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// personBound binds personGuild to an account linked to personLinked, in the
// scope asked for.
func (b *botEnv) personBound(t *testing.T, scope string) db.User {
	t.Helper()
	ctx := context.Background()
	u, _ := b.newUser("friend")
	if err := b.db.LinkDiscord(ctx, u.ID, personLinked, "friend_d", b.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.BindGuildScope(ctx, personGuild, u.ID, scope, b.now()); err != nil {
		t.Fatal(err)
	}
	return u
}

func personUpload(t *testing.T, member string) string {
	t.Helper()
	body := clipBody(personGuild, "clip.mp4", "video/mp4", "https://cdn.discordapp.com/x/clip.mp4", 16)
	if member == "-" {
		return body
	}
	return asMember(t, body, member)
}

// TestPersonBindingRefusesEveryOtherInvoker: another member, a member with no
// user, and a body with no member at all each get the one private line and
// hand over no job; the log says why and names nobody.
func TestPersonBindingRefusesEveryOtherInvoker(t *testing.T) {
	b := newBotEnv(t)
	b.personBound(t, db.GuildScopePerson)
	for _, member := range []string{personOther, "", "-"} {
		r, logs := b.postLogged(personUpload(t, member))
		assertEphemeral(t, r, msgNotYours)
		if !strings.Contains(logs, "discord_guild=not_yours") {
			t.Fatalf("member %q: the log does not say not_yours: %s", member, logs)
		}
		if strings.Contains(logs, personOther) || strings.Contains(logs, personLinked) {
			t.Fatalf("member %q: a log line carries a Discord id: %s", member, logs)
		}
	}
	if b.bot.count() != 0 {
		t.Fatal("a person binding handed another member's upload to the bot")
	}
}

// TestPersonBindingPassesTheLinkedMember: the account's own Discord user goes
// on exactly as on a server binding — the upload is deferred and handed over.
func TestPersonBindingPassesTheLinkedMember(t *testing.T) {
	b := newBotEnv(t)
	b.personBound(t, db.GuildScopePerson)
	r := b.post(personUpload(t, personLinked))
	if got, _ := r.json["type"].(float64); int(got) != callbackDeferredMessage {
		t.Fatalf("the linked member's /upload answered %s", r.body)
	}
	if b.bot.count() != 1 {
		t.Fatalf("the linked member's /upload handed over %d jobs, want 1", b.bot.count())
	}
}

// TestServerBindingPassesEveryMember: a server binding never reads the invoker
// — every member, with or without a user object, goes on as before 0024.
func TestServerBindingPassesEveryMember(t *testing.T) {
	b := newBotEnv(t)
	b.personBound(t, db.GuildScopeServer)
	for i, member := range []string{personLinked, personOther, "", "-"} {
		r := b.post(personUpload(t, member))
		if got, _ := r.json["type"].(float64); int(got) != callbackDeferredMessage {
			t.Fatalf("member %q on a server binding answered %s", member, r.body)
		}
		if b.bot.count() != i+1 {
			t.Fatalf("member %q: %d jobs, want %d", member, b.bot.count(), i+1)
		}
	}
}
