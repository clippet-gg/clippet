package db

import (
	"context"
	"errors"
	"testing"
)

// The account link behind "Sign in with Discord" (ROADMAP #44, §20 erratum
// 302; migration 0019): one Discord id opens at most one account, an account
// holds at most one id, a second link is a refusal and never a replacement,
// and unlinking touches no session.

func TestDiscordLinkRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	alice := mkUser(t, d, "alice")
	bob := mkUser(t, d, "bob")
	const id = "555555555555555555"

	if _, err := d.GetUserByDiscordID(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unlinked id reads as %v, want ErrNotFound", err)
	}
	u, err := d.GetUserByID(ctx, alice.ID)
	if err != nil || u.DiscordID != nil || u.DiscordUsername != "" || u.DiscordLinkedAt != nil {
		t.Fatalf("a fresh account carries a link: %+v %v", u, err)
	}
	if err := d.LinkDiscord(ctx, alice.ID, id, "alice#0", 1000); err != nil {
		t.Fatalf("LinkDiscord: %v", err)
	}
	u, err = d.GetUserByDiscordID(ctx, id)
	if err != nil || u.ID != alice.ID || u.DiscordID == nil || *u.DiscordID != id || u.DiscordUsername != "alice#0" ||
		u.DiscordLinkedAt == nil || *u.DiscordLinkedAt != 1000 {
		t.Fatalf("GetUserByDiscordID after the link: %+v %v", u, err)
	}
	// The same fields by every other read of the row.
	byName, _ := d.GetUserByUsername(ctx, "alice")
	byID, _ := d.GetUserByID(ctx, alice.ID)
	for name, got := range map[string]User{"GetUserByUsername": byName, "GetUserByID": byID} {
		if got.DiscordID == nil || *got.DiscordID != id || got.DiscordUsername != "alice#0" {
			t.Fatalf("%s does not carry the link: %+v", name, got)
		}
	}

	// A second link on an account that has one is refused (0 rows), and the
	// row is untouched.
	if err := d.LinkDiscord(ctx, alice.ID, "666666666666666666", "other", 2000); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second link on a linked account: %v, want ErrConflict", err)
	}
	u, _ = d.GetUserByID(ctx, alice.ID)
	if *u.DiscordID != id || *u.DiscordLinkedAt != 1000 {
		t.Fatalf("the refused link changed the row: %+v", u)
	}
	// The same id on ANOTHER account is refused by the partial UNIQUE index.
	if err := d.LinkDiscord(ctx, bob.ID, id, "bob", 3000); !errors.Is(err, ErrConflict) {
		t.Fatalf("a taken id on another account: %v, want ErrConflict", err)
	}
	if u, _ := d.GetUserByID(ctx, bob.ID); u.DiscordID != nil {
		t.Fatalf("bob got the taken id: %+v", u)
	}

	// The display name refreshes; the id does not move.
	if err := d.TouchDiscordUsername(ctx, alice.ID, "alice_renamed"); err != nil {
		t.Fatal(err)
	}
	u, _ = d.GetUserByDiscordID(ctx, id)
	if u.DiscordUsername != "alice_renamed" || *u.DiscordID != id {
		t.Fatalf("after the touch: %+v", u)
	}
	// A touch on an UNLINKED account is a no-op, never a stray name.
	if err := d.TouchDiscordUsername(ctx, bob.ID, "stray"); err != nil {
		t.Fatal(err)
	}
	if u, _ := d.GetUserByID(ctx, bob.ID); u.DiscordUsername != "" {
		t.Fatalf("a touch on an unlinked account wrote a name: %+v", u)
	}

	// Unlink: reports whether there was one, clears all three, keeps sessions.
	if err := d.CreateSession(ctx, "sess-a", alice.ID, 10, 20, 30, "ua"); err != nil {
		t.Fatal(err)
	}
	had, err := d.UnlinkDiscord(ctx, alice.ID)
	if err != nil || !had {
		t.Fatalf("UnlinkDiscord = %v, %v; want true", had, err)
	}
	u, _ = d.GetUserByID(ctx, alice.ID)
	if u.DiscordID != nil || u.DiscordUsername != "" || u.DiscordLinkedAt != nil {
		t.Fatalf("the unlink left something behind: %+v", u)
	}
	if _, _, err := d.GetSession(ctx, "sess-a", 15); err != nil {
		t.Fatalf("the unlink touched a session: %v", err)
	}
	had, err = d.UnlinkDiscord(ctx, alice.ID)
	if err != nil || had {
		t.Fatalf("a second unlink = %v, %v; want false", had, err)
	}
	// …and the id is free again, for whoever links it next.
	if err := d.LinkDiscord(ctx, bob.ID, id, "bob", 4000); err != nil {
		t.Fatalf("relinking a freed id: %v", err)
	}
	if u, _ := d.GetUserByDiscordID(ctx, id); u.ID != bob.ID {
		t.Fatalf("the freed id opens %q, want bob", u.Username)
	}

	// The inventory carries the id, or "" for an unlinked account.
	rows, err := d.ListUserSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Username] = r.DiscordID
	}
	if got["alice"] != "" || got["bob"] != id {
		t.Fatalf("ListUserSummaries discord ids: %v", got)
	}
}

// A deleted account takes its link with it — the columns are on the row, so
// there is no cascade question and the id is free the moment the row is gone.
func TestDiscordLinkGoesWithItsAccount(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "leaving")
	const id = "777777777777777777"
	if err := d.LinkDiscord(ctx, u.ID, id, "x", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.rw.ExecContext(ctx, "DELETE FROM users WHERE id = ?", u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetUserByDiscordID(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the link outlived its account: %v", err)
	}
	other := mkUser(t, d, "next")
	if err := d.LinkDiscord(ctx, other.ID, id, "x", 2); err != nil {
		t.Fatalf("the freed id could not be linked again: %v", err)
	}
}
