package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// userCols is every user SELECT's column list, in scanUser's order. The three
// Discord columns ride along since migration 0019 (§20 erratum 302).
const userCols = "id, username, password_hash, created_at, discord_user_id, discord_username, discord_linked_at"

// CreateUser inserts a user. A duplicate id or username (case-insensitive)
// returns ErrConflict.
func (d *DB) CreateUser(ctx context.Context, id, username, hash string, now int64) (User, error) {
	_, err := d.rw.ExecContext(ctx,
		"INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, ?, ?)",
		id, username, hash, now)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrConflict
		}
		return User{}, err
	}
	return User{ID: id, Username: username, PasswordHash: hash, CreatedAt: now}, nil
}

// CreateFirstUser inserts a user ONLY while the `users` table is empty and
// returns ErrConflict when it was not — the atomic half of `REGISTRATION=first`
// (§20 erratum 106).
//
// The emptiness test rides INSIDE the statement on purpose. `first` promises
// that exactly one account can come into existence out of nothing, and a
// COUNT(*) followed by an INSERT cannot promise that: the handler runs bcrypt
// between the two, so the window is a couple of hundred milliseconds wide and
// a burst of concurrent registrations against a fresh instance would all read
// zero and all be accepted. Here the count is evaluated by the same statement
// that writes the row, on the single-connection writer pool, so the door is
// shut by construction rather than by timing.
//
// A UNIQUE violation cannot happen on this statement — the row goes in only
// when there is nothing to collide with — so ErrConflict has exactly one
// meaning for the caller: somebody else bootstrapped the instance first.
func (d *DB) CreateFirstUser(ctx context.Context, id, username, hash string, now int64) (User, error) {
	n, err := d.exec(ctx, `
INSERT INTO users (id, username, password_hash, created_at)
SELECT ?, ?, ?, ? WHERE (SELECT COUNT(*) FROM users) = 0`, id, username, hash, now)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrConflict
		}
		return User{}, err
	}
	if n == 0 {
		return User{}, ErrConflict
	}
	return User{ID: id, Username: username, PasswordHash: hash, CreatedAt: now}, nil
}

// GetUserByUsername looks a user up case-insensitively (COLLATE NOCASE).
func (d *DB) GetUserByUsername(ctx context.Context, name string) (User, error) {
	return scanUser(d.ro.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE username = ?", name))
}

// GetUserByID returns the user with the given id.
func (d *DB) GetUserByID(ctx context.Context, id string) (User, error) {
	return scanUser(d.ro.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = ?", id))
}

// CountUsers returns the number of registered users.
func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := d.ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

// --- Sign in with Discord (ROADMAP #44, §20 erratum 302) --------------------

// GetUserByDiscordID resolves a Discord user id to the ONE account it is
// linked to, or ErrNotFound. This is the whole of the login-mode callback's
// account rule: found → a session for that account; not found → nothing is
// created, under any REGISTRATION value.
func (d *DB) GetUserByDiscordID(ctx context.Context, discordID string) (User, error) {
	return scanUser(d.ro.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE discord_user_id = ?", discordID))
}

// LinkDiscord records that discordID may open userID's account — an UPDATE
// guarded on the account NOT already holding a link, so a second link is a
// refusal rather than a silent replacement (one explicit state change at a
// time). ErrConflict for BOTH "this account already has a link" (0 rows) and
// "that Discord id is linked to another account" (the partial UNIQUE index);
// a caller that needs to tell them apart re-reads, which the CLI does and the
// callback deliberately does not (it never names the other account).
func (d *DB) LinkDiscord(ctx context.Context, userID, discordID, username string, now int64) error {
	n, err := d.exec(ctx, `
UPDATE users SET discord_user_id = ?, discord_username = ?, discord_linked_at = ?
 WHERE id = ? AND discord_user_id IS NULL`, discordID, username, now, userID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

// UnlinkDiscord removes the account's link and reports whether there was one
// — "not linked" is worth saying out loud and is not an error. Sessions are
// untouched: unlinking removes a way IN, not the browsers already in.
func (d *DB) UnlinkDiscord(ctx context.Context, userID string) (bool, error) {
	n, err := d.exec(ctx, `
UPDATE users SET discord_user_id = NULL, discord_username = '', discord_linked_at = NULL
 WHERE id = ? AND discord_user_id IS NOT NULL`, userID)
	return n > 0, err
}

// TouchDiscordUsername refreshes the display name at a Discord login. Display
// only: the id decides everything, and a failed touch must not fail a login.
func (d *DB) TouchDiscordUsername(ctx context.Context, userID, username string) error {
	_, err := d.exec(ctx, "UPDATE users SET discord_username = ? WHERE id = ? AND discord_user_id IS NOT NULL", username, userID)
	return err
}

// --- sessions ---

// sessionCols is the session half of every session SELECT, in Session field
// order, so GetSession and ListSessions cannot drift apart.
const sessionCols = "s.token_hash, s.user_id, s.created_at, s.expires_at, s.last_seen_at, s.max_expires_at, s.user_agent"

func scanSessionInto(s *Session, sc scanner, tail ...any) error {
	dest := append([]any{&s.TokenHash, &s.UserID, &s.CreatedAt, &s.ExpiresAt, &s.LastSeenAt,
		&s.MaxExpiresAt, &s.UserAgent}, tail...)
	return sc.Scan(dest...)
}

// CreateSession inserts a session row keyed by the hex sha256 of the cookie.
// maxExpiresAt is the ceiling the sliding window may never pass (§20 erratum
// 105); pass 0 for "no ceiling", which only tests and legacy callers do.
func (d *DB) CreateSession(ctx context.Context, tokenHash, userID string, now, expiresAt, maxExpiresAt int64, ua string) error {
	_, err := d.rw.ExecContext(ctx, `
INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen_at, max_expires_at, user_agent)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		tokenHash, userID, now, expiresAt, now, maxExpiresAt, ua)
	if err != nil && isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// GetSession returns the unexpired session and its user, or ErrNotFound.
//
// The ceiling is enforced HERE as well as in TouchSession, and not only as a
// belt on braces: the touch runs at most once a minute per session, so a row
// whose max_expires_at passed inside the current minute would otherwise be
// accepted on every request until the gate next opened. A ceiling of 0 means
// "none recorded" and is skipped (§20 erratum 105).
func (d *DB) GetSession(ctx context.Context, tokenHash string, now int64) (Session, User, error) {
	row := d.ro.QueryRowContext(ctx, `
SELECT `+sessionCols+`, u.id, u.username, u.password_hash, u.created_at
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token_hash = ? AND s.expires_at > ? AND (s.max_expires_at <= 0 OR s.max_expires_at > ?)`,
		tokenHash, now, now)
	var s Session
	var u User
	err := scanSessionInto(&s, row, &u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return Session{}, User{}, ErrNotFound
		}
		return Session{}, User{}, err
	}
	return s, u, nil
}

// TouchSession slides the expiry and records activity, CLAMPED to the row's
// own ceiling (§20 erratum 105): a session may slide, but never past
// `max_expires_at`. The clamp is in SQL rather than in the caller because the
// ceiling is a fact of the row and the caller holds only the request's clock —
// computing it in Go would need a second read and a second round trip, and
// would race a concurrent revocation between them.
func (d *DB) TouchSession(ctx context.Context, tokenHash string, now, expiresAt int64) error {
	n, err := d.exec(ctx, `
UPDATE sessions
   SET last_seen_at = ?,
       expires_at = CASE WHEN max_expires_at > 0 AND ? > max_expires_at THEN max_expires_at ELSE ? END
 WHERE token_hash = ?`, now, expiresAt, expiresAt, tokenHash)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListSessions returns a user's live sessions, newest first — what
// GET /api/auth/sessions renders. The hash comes back with each row because
// the handler needs it to mark the CURRENT one, and for nothing else: it is
// never rendered and never leaves httpapi (§20 erratum 107).
func (d *DB) ListSessions(ctx context.Context, userID string, now int64) ([]Session, error) {
	rows, err := d.ro.QueryContext(ctx, `
SELECT `+sessionCols+`
FROM sessions s
WHERE s.user_id = ? AND s.expires_at > ? AND (s.max_expires_at <= 0 OR s.max_expires_at > ?)
ORDER BY s.created_at DESC, s.token_hash`, userID, now, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var s Session
		if err := scanSessionInto(&s, rows); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteOtherSessions removes every session of userID EXCEPT keepHash and
// returns how many went. It is "log out everywhere but here" (§20 erratum
// 107) and the sweep half of a password change (§20 erratum 108).
//
// The DELETE carries no lifetime predicate on purpose: removing a dead row is
// desirable, and repeating ListSessions' two predicates here would leave it
// behind for the sweeper. The consequence is that the count is rows DELETED,
// which is an upper bound on what GET /api/auth/sessions last showed — it
// also covers every session of this user that expired since the last sweeper
// tick. The receipt is still honest about what the call did; erratum 107 says
// so in as many words.
//
// keepHash is compared, not trusted: a caller passing "" removes the lot,
// which is what `clippet admin sessions revoke` wants and what the handlers
// never do — they always hold their own session's hash.
func (d *DB) DeleteOtherSessions(ctx context.Context, userID, keepHash string) (int64, error) {
	return d.exec(ctx, "DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?", userID, keepHash)
}

// DeleteAllSessions removes every session in the instance and returns the
// count — `clippet admin sessions revoke --all`, the "I have no idea which
// account was compromised" answer.
func (d *DB) DeleteAllSessions(ctx context.Context) (int64, error) {
	return d.exec(ctx, "DELETE FROM sessions")
}

// SetPassword rotates a user's bcrypt hash and, in the SAME transaction,
// deletes every session of that user except keepHash (§20 erratum 108). One
// transaction is the whole point: a rotation that committed while the old
// sessions were still live would leave the thief logged in with the new
// password's blessing, and a sweep that committed first would log the owner
// out of a password change that then failed.
//
// It returns the number of sessions removed. ErrNotFound if the user is gone.
func (d *DB) SetPassword(ctx context.Context, userID, hash, keepHash string) (int64, error) {
	var removed int64
	err := d.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", hash, userID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		res, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?", userID, keepHash)
		if err != nil {
			return err
		}
		removed, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// DeleteSession removes one session (logout). Missing → ErrNotFound.
func (d *DB) DeleteSession(ctx context.Context, tokenHash string) error {
	n, err := d.exec(ctx, "DELETE FROM sessions WHERE token_hash = ?", tokenHash)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteExpiredSessions prunes every row that can no longer authenticate:
// one whose sliding expiry has arrived, and one whose absolute CEILING has
// (§20 erratum 105).
//
// The second half is not redundant. Every read rejects a row past its
// ceiling — GetSession and ListSessions both carry the predicate — and the
// only writer that pulls expires_at down to the ceiling is TouchSession,
// which is reached only for sessions that still resolve. So a row whose
// ceiling has passed is invisible and unusable, and with `expires_at <= ?`
// alone it survived until its OWN expiry arrived: up to SESSION_TTL (720 h by
// default) of dead rows. Migration 0005's backfill
// (`max_expires_at = created_at + 7776000`) creates exactly that population
// on any instance upgraded with sessions older than 90 days, which is what
// makes this worth a predicate rather than a comment.
func (d *DB) DeleteExpiredSessions(ctx context.Context, now int64) (int64, error) {
	return d.exec(ctx,
		"DELETE FROM sessions WHERE expires_at <= ? OR (max_expires_at > 0 AND max_expires_at <= ?)",
		now, now)
}

// --- api tokens ---

const tokenCols = "id, user_id, name, token_hash, prefix, scopes, created_at, last_used_at, revoked_at"

// CreateToken inserts an API token row. Duplicate id/hash → ErrConflict.
func (d *DB) CreateToken(ctx context.Context, t APIToken) error {
	scopes := strings.Join(t.Scopes, ",")
	if scopes == "" {
		scopes = "upload"
	}
	_, err := d.rw.ExecContext(ctx, `
INSERT INTO api_tokens (id, user_id, name, token_hash, prefix, scopes, created_at, last_used_at, revoked_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.Name, t.TokenHash, t.Prefix, scopes, t.CreatedAt, nullInt64(t.LastUsedAt), nullInt64(t.RevokedAt))
	if err != nil && isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func scanToken(s scanner) (APIToken, error) {
	var t APIToken
	var scopes string
	var lastUsed, revoked sql.NullInt64
	if err := s.Scan(&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.Prefix, &scopes, &t.CreatedAt, &lastUsed, &revoked); err != nil {
		if err == sql.ErrNoRows {
			return APIToken{}, ErrNotFound
		}
		return APIToken{}, err
	}
	t.Scopes = splitScopes(scopes)
	t.LastUsedAt, t.RevokedAt = ptrInt64(lastUsed), ptrInt64(revoked)
	return t, nil
}

func splitScopes(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// GetTokenByHash returns a live (unrevoked) token and its user, or ErrNotFound.
func (d *DB) GetTokenByHash(ctx context.Context, hash string) (APIToken, User, error) {
	row := d.ro.QueryRowContext(ctx, `
SELECT t.id, t.user_id, t.name, t.token_hash, t.prefix, t.scopes, t.created_at, t.last_used_at, t.revoked_at,
       u.id, u.username, u.password_hash, u.created_at
FROM api_tokens t JOIN users u ON u.id = t.user_id
WHERE t.token_hash = ? AND t.revoked_at IS NULL`, hash)
	var t APIToken
	var u User
	var scopes string
	var lastUsed, revoked sql.NullInt64
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.Prefix, &scopes, &t.CreatedAt, &lastUsed, &revoked,
		&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return APIToken{}, User{}, ErrNotFound
		}
		return APIToken{}, User{}, err
	}
	t.Scopes = splitScopes(scopes)
	t.LastUsedAt, t.RevokedAt = ptrInt64(lastUsed), ptrInt64(revoked)
	return t, u, nil
}

// ListTokens returns a user's unrevoked tokens, newest first.
func (d *DB) ListTokens(ctx context.Context, userID string) ([]APIToken, error) {
	rows, err := d.ro.QueryContext(ctx,
		"SELECT "+tokenCols+" FROM api_tokens WHERE user_id = ? AND revoked_at IS NULL ORDER BY created_at DESC, id",
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken sets revoked_at on the caller's token. Unknown, foreign or
// already-revoked → ErrNotFound.
func (d *DB) RevokeToken(ctx context.Context, userID, tokenID string, now int64) error {
	n, err := d.exec(ctx,
		"UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL",
		now, tokenID, userID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeUserTokens revokes every live token of one user and returns the
// count — `clippet admin tokens revoke <username> --all` (§20 erratum 109).
// It takes a user id, not a name: the CLI resolves the name first so that a
// pattern can never be smuggled in where an identity is meant.
func (d *DB) RevokeUserTokens(ctx context.Context, userID string, now int64) (int64, error) {
	return d.exec(ctx, "UPDATE api_tokens SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL", now, userID)
}

// TouchToken records last_used_at.
func (d *DB) TouchToken(ctx context.Context, tokenID string, now int64) error {
	n, err := d.exec(ctx, "UPDATE api_tokens SET last_used_at = ? WHERE id = ?", now, tokenID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- admin: user inventory and prune (§20 erratum 86) ----------------------

// UserSummary is one row of `clippet admin users`: who exists, when they
// registered, and how much they own. The three counts are what make the
// prune safe to reason about — a user with a clip or a collection is never
// a candidate, whatever the pattern says.
type UserSummary struct {
	ID          string
	Username    string
	CreatedAt   int64
	Clips       int // every state, tombstones included
	Collections int
	Tokens      int // live (unrevoked) API tokens
	// DiscordID is the linked Discord user's id, "" when the account has no
	// link (§20 erratum 302) — the `discord=` column of `admin users`.
	DiscordID string
	// Owner is the instance's owner flag (migration 0025, §20 erratum 349) —
	// the `owner=` column of `admin users`.
	Owner bool
}

// userSummarySQL is the inventory query. The counts are correlated
// subqueries rather than joins because a join would need three GROUP BYs to
// avoid multiplying rows against each other, and `users` is the smallest
// table in the schema: this is a scan of it with three indexed probes per
// row (`clips_owner`, `collections_owner`, `api_tokens` by user), not a
// query anything is paged through.
const userSummarySQL = `
SELECT u.id, u.username, u.created_at,
       (SELECT COUNT(*) FROM clips       c  WHERE c.owner_id = u.id),
       (SELECT COUNT(*) FROM collections co WHERE co.owner_id = u.id),
       (SELECT COUNT(*) FROM api_tokens  t  WHERE t.user_id  = u.id AND t.revoked_at IS NULL),
       COALESCE(u.discord_user_id, ''),
       u.owner
FROM users u`

func scanUserSummaries(rows *sql.Rows) ([]UserSummary, error) {
	defer rows.Close()
	out := []UserSummary{}
	for rows.Next() {
		var s UserSummary
		if err := rows.Scan(&s.ID, &s.Username, &s.CreatedAt, &s.Clips, &s.Collections, &s.Tokens, &s.DiscordID, &s.Owner); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListUserSummaries returns every user with their counts, oldest first.
func (d *DB) ListUserSummaries(ctx context.Context) ([]UserSummary, error) {
	rows, err := d.ro.QueryContext(ctx, userSummarySQL+" ORDER BY u.created_at, u.id")
	if err != nil {
		return nil, err
	}
	return scanUserSummaries(rows)
}

// pruneWhere is the candidate predicate, shared BY CONSTRUCTION between the
// list and the delete so the rows reported are exactly the rows removed: one
// string, two statements, one transaction.
//
// `username LIKE ?` uses SQLite's default LIKE, which on the NOCASE-collated
// username column is case-insensitive for ASCII — the same rule registration
// uses, so an operator who types the name they see matches it. The two other
// clauses are the safety: a user who owns a clip or a collection is never a
// candidate, because `clips.owner_id` and `collections.owner_id` CASCADE from
// `users` and deleting such a row would drop the clip's row while its objects
// stayed in the bucket forever (a Destroy that never runs). `--older-than`
// then keeps the command off anybody who registered while it was being typed.
const pruneWhere = `
WHERE username LIKE ?
  AND created_at < ?
  AND NOT EXISTS (SELECT 1 FROM clips       WHERE clips.owner_id       = users.id)
  AND NOT EXISTS (SELECT 1 FROM collections WHERE collections.owner_id = users.id)`

// PruneUsers deletes every user whose name matches pattern (SQL LIKE: `%` is
// any run, `_` is one character), who registered before createdBefore, and
// who owns NO clips and NO collections. It returns the rows it removed.
//
// An empty pattern is refused rather than treated as "everything": this is
// the one command in the binary that deletes a user, and a typo that expands
// to the whole table must not be spellable. Matching nothing is not an error
// — it is an empty result and exit 0.
//
// The select and the delete run in ONE transaction on the single-connection
// writer pool, so the report is the truth: nothing can register into the
// candidate set between reading it and deleting it. Sessions and API tokens
// go with each user through their existing ON DELETE CASCADE, which is the
// whole cleanup — there are no objects to reclaim, since a candidate owns no
// clips by definition.
func (d *DB) PruneUsers(ctx context.Context, pattern string, createdBefore int64) ([]UserSummary, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, ErrInvalidPattern
	}
	var out []UserSummary
	err := d.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			userSummarySQL+" WHERE u.id IN (SELECT id FROM users"+pruneWhere+") ORDER BY u.created_at, u.id",
			pattern, createdBefore)
		if err != nil {
			return err
		}
		out, err = scanUserSummaries(rows)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM users"+pruneWhere, pattern, createdBefore)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if int(n) != len(out) {
			// Impossible on the single-connection writer inside a
			// transaction, and worth failing loudly rather than reporting a
			// set that is not the set that went.
			return fmt.Errorf("db: prune deleted %d users but reported %d", n, len(out))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- the owner (migration 0025, §20 erratum 349) ---------------------------

// IsOwner reports whether userID is this instance's owner — the first account
// created on it, or whoever the role passed to since (the migration's two
// triggers, `admin users owner`). An unknown id is simply not the owner. One
// indexed probe by primary key; the callers are the operator pages alone.
func (d *DB) IsOwner(ctx context.Context, userID string) (bool, error) {
	var owner bool
	err := d.ro.QueryRowContext(ctx, "SELECT owner FROM users WHERE id = ?", userID).Scan(&owner)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return owner, err
}

// SetOwner moves the owner role to the account named username (NOCASE, as
// registration matches it), in ONE transaction: the old owner's flag is
// cleared before the new one is set, which is the order the partial unique
// index needs. ErrNotFound when no such account exists — and then nothing has
// changed.
func (d *DB) SetOwner(ctx context.Context, username string) (User, error) {
	var u User
	err := d.tx(ctx, func(tx *sql.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE username = ?", username))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE users SET owner = 0 WHERE owner = 1 AND id <> ?", u.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE users SET owner = 1 WHERE id = ?", u.ID)
		return err
	})
	return u, err
}
