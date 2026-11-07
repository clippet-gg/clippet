package db

import (
	"context"
	"database/sql"
	"errors"
)

// GuildBinding is one row of `discord_guilds` with the account's name joined
// in: the CLI prints the name, the interaction handler needs the id, and one
// read answers both (ROADMAP #33, §20 erratum 255).
//
// Since §20 erratum 342 it also carries the binding's Scope and the account's
// linked Discord user id (DiscordID, "" when the account has none): on a
// `person` binding the box answers for that one member only, and the join that
// already reaches `users` for the name reads the id in the same row.
type GuildBinding struct {
	GuildID   string
	UserID    string
	Username  string
	CreatedAt int64
	Scope     string
	DiscordID string
}

// The two scopes a binding has (migration 0024, §20 erratum 342). A server
// binding is every member's; a person binding is one member's, the account's
// linked Discord user.
const (
	GuildScopeServer = "server"
	GuildScopePerson = "person"
)

// Person reports whether the binding answers for one member only.
func (g GuildBinding) Person() bool { return g.Scope == GuildScopePerson }

// BindGuild points a Discord server at an account, replacing whatever it
// pointed at before. A guild that is already bound is REBOUND rather than
// refused: "this server now uploads into that account" is the operator's
// decision to change, and making them unbind first would only cost a command.
// The returned bool is whether a binding already existed with another account
// or another scope, so the CLI can say which of the two happened.
//
// created_at is left alone on a rebind — it records when this server was first
// connected, which is the fact an operator reading `admin bot ls` wants.
//
// A user id that does not exist is a FOREIGN KEY failure (foreign_keys(1) is
// in the DSN), which surfaces as a plain error rather than ErrConflict: the
// CLI resolves the username to an id first, so reaching it means the account
// was deleted between the two statements.
//
// BindGuild writes a SERVER binding; BindGuildScope names the scope (§20
// erratum 342). A rebind writes the scope it is given, so a server rebound
// without `--person` is every member's again.
func (d *DB) BindGuild(ctx context.Context, guildID, userID string, now int64) (rebound bool, err error) {
	return d.BindGuildScope(ctx, guildID, userID, GuildScopeServer, now)
}

// BindGuildScope is BindGuild with the scope: GuildScopeServer or
// GuildScopePerson (the column's CHECK refuses anything else). The caller
// checks a person binding's precondition — the account has a Discord user id —
// because the refusal is the CLI's sentence, not a database error.
func (d *DB) BindGuildScope(ctx context.Context, guildID, userID, scope string, now int64) (rebound bool, err error) {
	prev, err := d.GuildBinding(ctx, guildID)
	switch {
	case err == nil:
		rebound = prev.UserID != userID || prev.Scope != scope
	case errors.Is(err, ErrNotFound):
	default:
		return false, err
	}
	_, err = d.rw.ExecContext(ctx, `
INSERT INTO discord_guilds (guild_id, user_id, created_at, scope) VALUES (?, ?, ?, ?)
ON CONFLICT(guild_id) DO UPDATE SET user_id = excluded.user_id, scope = excluded.scope`, guildID, userID, now, scope)
	if err != nil {
		return false, err
	}
	return rebound, nil
}

// UnbindGuild removes a binding. It reports whether there was one: "no such
// binding" is worth saying out loud to an operator who typed the id wrong,
// and it is not an error.
func (d *DB) UnbindGuild(ctx context.Context, guildID string) (bool, error) {
	n, err := d.exec(ctx, "DELETE FROM discord_guilds WHERE guild_id = ?", guildID)
	return n > 0, err
}

// guildCols is both reads' column list, in GuildBinding's field order.
const guildCols = "g.guild_id, g.user_id, u.username, g.created_at, g.scope, COALESCE(u.discord_user_id, '')"

// GuildBinding resolves one guild to its account. ErrNotFound means the guild
// is unbound, which is the state every guild starts in and the one the
// interaction handler answers with a one-line refusal.
func (d *DB) GuildBinding(ctx context.Context, guildID string) (GuildBinding, error) {
	var g GuildBinding
	err := d.ro.QueryRowContext(ctx, `
SELECT `+guildCols+`
FROM discord_guilds g JOIN users u ON u.id = g.user_id
WHERE g.guild_id = ?`, guildID).Scan(&g.GuildID, &g.UserID, &g.Username, &g.CreatedAt, &g.Scope, &g.DiscordID)
	if errors.Is(err, sql.ErrNoRows) {
		return GuildBinding{}, ErrNotFound
	}
	return g, err
}

// ListGuildBindings returns every binding, oldest first — the order they were
// made in, which is how an operator remembers them.
func (d *DB) ListGuildBindings(ctx context.Context) ([]GuildBinding, error) {
	rows, err := d.ro.QueryContext(ctx, `
SELECT `+guildCols+`
FROM discord_guilds g JOIN users u ON u.id = g.user_id
ORDER BY g.created_at ASC, g.guild_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GuildBinding
	for rows.Next() {
		var g GuildBinding
		if err := rows.Scan(&g.GuildID, &g.UserID, &g.Username, &g.CreatedAt, &g.Scope, &g.DiscordID); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
