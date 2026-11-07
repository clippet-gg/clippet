package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// The guild binding (ROADMAP #33, §20 erratum 255): one Discord server → one
// account, written by the operator and read once per interaction.

func TestGuildBindingRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	alice := mkUser(t, d, "alice")
	bob := mkUser(t, d, "bob")
	const guild = "222222222222222222"

	if _, err := d.GuildBinding(ctx, guild); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unbound guild reads as %v, want ErrNotFound", err)
	}
	rebound, err := d.BindGuild(ctx, guild, alice.ID, 1000)
	if err != nil || rebound {
		t.Fatalf("BindGuild = %v, %v; want a fresh binding", rebound, err)
	}
	g, err := d.GuildBinding(ctx, guild)
	if err != nil || g.UserID != alice.ID || g.Username != "alice" || g.CreatedAt != 1000 {
		t.Fatalf("GuildBinding = %+v, %v", g, err)
	}
	// Rebinding to the SAME account is not a change and says so.
	if again, err := d.BindGuild(ctx, guild, alice.ID, 2000); err != nil || again {
		t.Fatalf("re-binding to the same account = %v, %v; want no change", again, err)
	}
	// Rebinding to ANOTHER account is the operator changing their mind, and
	// the original created_at is kept: it records when the server was first
	// connected.
	if moved, err := d.BindGuild(ctx, guild, bob.ID, 3000); err != nil || !moved {
		t.Fatalf("re-binding to another account = %v, %v; want rebound=true", moved, err)
	}
	g, err = d.GuildBinding(ctx, guild)
	if err != nil || g.UserID != bob.ID || g.CreatedAt != 1000 {
		t.Fatalf("after the rebind: %+v, %v", g, err)
	}

	// A second guild, and the listing.
	const other = "333333333333333333"
	if _, err := d.BindGuild(ctx, other, alice.ID, 500); err != nil {
		t.Fatal(err)
	}
	rows, err := d.ListGuildBindings(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ListGuildBindings = %+v, %v", rows, err)
	}
	if rows[0].GuildID != other || rows[1].GuildID != guild {
		t.Fatalf("the listing is not oldest-first: %+v", rows)
	}

	had, err := d.UnbindGuild(ctx, guild)
	if err != nil || !had {
		t.Fatalf("UnbindGuild = %v, %v", had, err)
	}
	if had, err := d.UnbindGuild(ctx, guild); err != nil || had {
		t.Fatalf("unbinding twice = %v, %v; want false and no error", had, err)
	}
	if _, err := d.GuildBinding(ctx, guild); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the unbound guild reads as %v", err)
	}
}

// TestGuildBindingCascadesWithItsAccount pins the schema choice: the binding
// follows `clips.owner_id`'s ON DELETE CASCADE, so deleting an account leaves
// no row pointing at a user that is gone — and the guild is simply unbound.
func TestGuildBindingCascadesWithItsAccount(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "leaving")
	const guild = "444444444444444444"
	if _, err := d.BindGuild(ctx, guild, u.ID, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := d.rw.ExecContext(ctx, "DELETE FROM users WHERE id = ?", u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GuildBinding(ctx, guild); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after the account was deleted the binding reads as %v, want ErrNotFound", err)
	}
	rows, err := d.ListGuildBindings(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("ListGuildBindings = %+v, %v; want none", rows, err)
	}
}

// TestBindGuildRefusesAnAccountThatDoesNotExist: foreign_keys(1) is in the DSN
// and the CLI resolves the username first, so this is the belt-and-braces
// half — a binding must never point at nothing.
func TestBindGuildRefusesAnAccountThatDoesNotExist(t *testing.T) {
	d := openTest(t)
	if _, err := d.BindGuild(context.Background(), "555555555555555555", "u_nobody", 1); err == nil {
		t.Fatal("a binding to a user that does not exist was accepted")
	}
}

// TestMigrate0016UpgradesAnOlderDatabase applies every migration EXCEPT 0016
// (by version, not by position — a later feature adds files to that
// directory), proves the table is genuinely new, and then upgrades.
func TestMigrate0016UpgradesAnOlderDatabase(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.rw.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	skipped := 0
	for _, m := range files {
		// 0024 alters the table 0016 makes (§20 erratum 342), so it waits
		// for 0016 and is applied, in order, after it.
		if m.version == 16 || m.version >= 24 {
			if m.version == 16 {
				skipped++
			}
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if skipped != 1 {
		t.Fatalf("0016 is missing from the embedded migrations: %d files", len(files))
	}
	u := mkUser(t, d, "upgrader")
	// The table does not exist yet, and that failure IS the proof it is new.
	if _, err := d.ListGuildBindings(ctx); err == nil {
		t.Fatal("discord_guilds existed before 0016")
	}
	applySkipped(t, d, 16)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate after 0016: %v", err)
	}
	if _, err := d.BindGuild(ctx, "666666666666666666", u.ID, 42); err != nil {
		t.Fatalf("BindGuild on the upgraded database: %v", err)
	}
	rows, err := d.ListGuildBindings(ctx)
	if err != nil || len(rows) != 1 || rows[0].Username != "upgrader" {
		t.Fatalf("the upgraded database reads back %+v (%v)", rows, err)
	}
}

// --- the scope (migration 0024, §20 erratum 342) ---------------------------

// TestGuildBindingScope pins the column's life: a bind is a server binding, a
// person bind says so, a rebind writes the scope it is given (and a scope
// change on the same account is a change), and the read carries the account's
// linked Discord id beside it.
func TestGuildBindingScope(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	bob := mkUser(t, d, "bob")
	const guild = "777777777777777777"
	const discordID = "888888888888888888"

	if _, err := d.BindGuild(ctx, guild, bob.ID, 10); err != nil {
		t.Fatal(err)
	}
	g, err := d.GuildBinding(ctx, guild)
	if err != nil || g.Scope != GuildScopeServer || g.Person() || g.DiscordID != "" {
		t.Fatalf("a plain bind = %+v, %v; want a server binding and no linked id", g, err)
	}
	if err := d.LinkDiscord(ctx, bob.ID, discordID, "bob_d", 11); err != nil {
		t.Fatal(err)
	}
	rebound, err := d.BindGuildScope(ctx, guild, bob.ID, GuildScopePerson, 12)
	if err != nil || !rebound {
		t.Fatalf("server -> person on the same account = %v, %v; want rebound", rebound, err)
	}
	g, err = d.GuildBinding(ctx, guild)
	if err != nil || g.Scope != GuildScopePerson || !g.Person() || g.DiscordID != discordID || g.CreatedAt != 10 {
		t.Fatalf("after --person: %+v, %v", g, err)
	}
	if again, err := d.BindGuildScope(ctx, guild, bob.ID, GuildScopePerson, 13); err != nil || again {
		t.Fatalf("the same person bind twice = %v, %v; want no change", again, err)
	}
	rows, err := d.ListGuildBindings(ctx)
	if err != nil || len(rows) != 1 || rows[0].Scope != GuildScopePerson || rows[0].DiscordID != discordID {
		t.Fatalf("ListGuildBindings = %+v, %v", rows, err)
	}
	// BindGuild is a server bind: the same command without --person makes the
	// server every member's again.
	if back, err := d.BindGuild(ctx, guild, bob.ID, 14); err != nil || !back {
		t.Fatalf("person -> server = %v, %v; want rebound", back, err)
	}
	if g, _ = d.GuildBinding(ctx, guild); g.Scope != GuildScopeServer {
		t.Fatalf("after the plain rebind: %+v", g)
	}
	// The CHECK keeps the two words the only two.
	if _, err := d.BindGuildScope(ctx, guild, bob.ID, "everyone", 15); err == nil {
		t.Fatal("a third scope was accepted")
	}
}

// TestMigrate0024KeepsTheBindingsItFinds builds a database through 0023, writes
// a binding the way 0016 left the table (three columns), and upgrades: the row
// survives as a server binding, which is what every binding before 0024 was.
func TestMigrate0024KeepsTheBindingsItFinds(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.rw.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range files {
		if m.version >= 24 {
			found = found || m.version == 24
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if !found {
		t.Fatal("0024 is missing from the embedded migrations")
	}
	u := mkUser(t, d, "before")
	if _, err := d.rw.ExecContext(ctx,
		"INSERT INTO discord_guilds (guild_id, user_id, created_at) VALUES (?, ?, ?)", "999999999999999999", u.ID, 77); err != nil {
		t.Fatalf("a 0016-shaped row: %v", err)
	}
	if _, err := d.rw.ExecContext(ctx, "SELECT scope FROM discord_guilds"); err == nil {
		t.Fatal("scope existed before 0024")
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	g, err := d.GuildBinding(ctx, "999999999999999999")
	if err != nil || g.Scope != GuildScopeServer || g.UserID != u.ID || g.CreatedAt != 77 {
		t.Fatalf("the upgraded row = %+v, %v; want the same binding, scope server", g, err)
	}
}
