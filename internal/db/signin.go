package db

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// The bot's one-time login links (§20 erratum 341, migration 0023): `/signin`
// mints a row, the link's page reads it, its POST consumes it.

// SigninTTLSeconds is how long a login link lives: five minutes, the number the
// bot's reply states ("this link works once, for five minutes").
const SigninTTLSeconds = 300

// SigninToken is one minted login link, as the interaction proved it. Hash is
// the hex sha256 of the raw token; the raw value is never stored.
type SigninToken struct {
	Hash        string
	GuildID     string
	DiscordID   string
	Username    string // the member's Discord username (a new account's name source)
	DisplayName string // the page's "as <name>"
	CreatedAt   int64
	ExpiresAt   int64
}

// CreateSigninToken stores a minted link and, in the same transaction, removes
// every row already past its expiry: minting is the only moment the table
// grows, so it is the moment it is tidied, and no sweeper needs to know it.
func (d *DB) CreateSigninToken(ctx context.Context, t SigninToken) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM signin_tokens WHERE expires_at <= ?", t.CreatedAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO signin_tokens (token_hash, guild_id, discord_user_id, discord_username, display_name, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, t.Hash, t.GuildID, t.DiscordID, t.Username, t.DisplayName, t.CreatedAt, t.ExpiresAt)
		if err != nil && isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
}

const signinCols = "token_hash, guild_id, discord_user_id, discord_username, display_name, created_at, expires_at"

func scanSignin(s scanner) (SigninToken, error) {
	var t SigninToken
	err := s.Scan(&t.Hash, &t.GuildID, &t.DiscordID, &t.Username, &t.DisplayName, &t.CreatedAt, &t.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SigninToken{}, ErrNotFound
	}
	return t, err
}

// PeekSigninToken reads a LIVE link without touching it — the GET that renders
// the page, which Discord's crawler may make too (erratum 316). ErrNotFound for
// an unknown, used or expired link alike.
func (d *DB) PeekSigninToken(ctx context.Context, hash string, now int64) (SigninToken, error) {
	return scanSignin(d.ro.QueryRowContext(ctx,
		"SELECT "+signinCols+" FROM signin_tokens WHERE token_hash = ? AND expires_at > ?", hash, now))
}

// ConsumeSigninToken takes a link: the row is read and DELETED in one
// transaction on the single-connection writer, so of two POSTs of one link
// exactly one sees it. An expired row is deleted too and answers ErrNotFound,
// as an unknown or already-used one does.
func (d *DB) ConsumeSigninToken(ctx context.Context, hash string, now int64) (SigninToken, error) {
	var t SigninToken
	err := d.tx(ctx, func(tx *sql.Tx) error {
		var err error
		t, err = scanSignin(tx.QueryRowContext(ctx, "SELECT "+signinCols+" FROM signin_tokens WHERE token_hash = ?", hash))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM signin_tokens WHERE token_hash = ?", hash)
		return err
	})
	if err != nil {
		return SigninToken{}, err
	}
	// Checked AFTER the commit, so an expired row's DELETE is kept: the
	// transaction would have rolled it back had this been its error.
	if t.ExpiresAt <= now {
		return SigninToken{}, ErrNotFound
	}
	return t, nil
}

// maxUsernameBytes is §14's username ceiling (3-32 of [A-Za-z0-9_]).
const maxUsernameBytes = 32

// signinNameCandidate is the n-th name tried for a new Discord account: the
// base itself, then `base_2`, `base_3`, … — the base cut short so the suffix
// always fits inside 32 bytes. The separator is `_` because §14's charset has
// no `-`: a name the password form could not type would be no name here.
func signinNameCandidate(base string, n int) string {
	if n <= 1 {
		return base
	}
	suffix := "_" + strconv.Itoa(n)
	if len(base)+len(suffix) > maxUsernameBytes {
		base = base[:maxUsernameBytes-len(suffix)]
	}
	return base + suffix
}

// maxSigninNameTries bounds the suffix search. A hundred accounts already named
// after one Discord username is a server nobody has.
const maxSigninNameTries = 100

// CreateDiscordUser makes the account an unlinked member's first `/signin`
// opens (§20 erratum 341): named from base (already normalised to §14's rule by
// the caller), suffixed `_2`, `_3`, … on a collision, with NO password — the
// empty hash bcrypt refuses every password against, so the password login
// answers it with its ordinary wrong-password line — and linked to discordID in
// the SAME statement that creates it. A username match is never a link: the
// name is only a name, and the id is the key.
//
// ErrConflict means discordID is already linked to an account (another sign-in
// of the same member won a race): the caller re-reads by id and opens that one.
func (d *DB) CreateDiscordUser(ctx context.Context, id, base, discordID, discordUsername string, now int64) (User, error) {
	var u User
	err := d.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE discord_user_id = ?", discordID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrConflict
		}
		for try := 1; try <= maxSigninNameTries; try++ {
			name := signinNameCandidate(base, try)
			var taken int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE username = ?", name).Scan(&taken); err != nil {
				return err
			}
			if taken > 0 {
				continue
			}
			linked := now
			_, err := tx.ExecContext(ctx, `
INSERT INTO users (id, username, password_hash, created_at, discord_user_id, discord_username, discord_linked_at)
VALUES (?, ?, '', ?, ?, ?, ?)`, id, name, now, discordID, discordUsername, now)
			if err != nil {
				if isUniqueViolation(err) {
					return ErrConflict
				}
				return err
			}
			did := discordID
			u = User{ID: id, Username: name, CreatedAt: now, DiscordID: &did, DiscordUsername: discordUsername, DiscordLinkedAt: &linked}
			return nil
		}
		return errors.New("db: no free username for a Discord sign-in after " + strconv.Itoa(maxSigninNameTries) + " tries")
	})
	if err != nil {
		return User{}, err
	}
	return u, nil
}
