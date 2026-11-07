package db

import (
	"context"
	"database/sql"
)

// The two link grammars (ROADMAP #35, §20 errata 249, 250). They are spelled
// here, beside the column that stores one of them, so `internal/httpapi` (the
// wire value and the account default), `internal/ids` (the minter) and this
// package cannot drift into three vocabularies. `ids.Kind` reports the same
// two words for an id that already exists.
const (
	LinkStyleCompact   = "compact"
	LinkStyleMemorable = "memorable"
)

// ValidLinkStyle reports whether s is one of the two grammars — the closed set
// a `link_style` field and the account default are both held to.
func ValidLinkStyle(s string) bool {
	return s == LinkStyleCompact || s == LinkStyleMemorable
}

// UserDefaults are one account's upload preferences (ROADMAP #22 and #35, §20
// errata 155, 250): what an upload gets when it says nothing about the field.
//
// TTLDays keeps the three states migration 0011 stores, in the wire unit of
// §8.2: nil = "no preference, follow DEFAULT_CLIP_TTL", 0 = the account chose
// keep-forever, 1|7|30|90 = that many days. Unlisted has two states and is a
// plain bool. LinkStyle has two states and no absence either (migration 0015's
// column is NOT NULL DEFAULT 'compact'), so the zero value of this struct —
// which is what a caller gets for an account whose row has gone — reads as
// `compact`, the shipped default, through LinkStyleOr below.
//
// No field is a permission and none can widen anything: the resolution order
// is FIELD SENT > USER DEFAULT > instance default, so a capture tool that
// always sends `ttl_days` and `unlisted` behaves exactly as it did before this
// table had columns, and one that sends no `link_style` at all — the shipped
// `clippet.sxcu` sends none — follows the account.
type UserDefaults struct {
	TTLDays   *int
	Unlisted  bool
	LinkStyle string
}

// LinkStyleOr returns the account's grammar, or `compact` for the zero value
// and for anything outside the closed set. The second case is a value some
// other tool wrote into the column: an upload resolves it to the shipped
// default rather than failing over somebody's SQL, exactly as applyDefaults
// treats an illegal `default_ttl_days`.
func (d UserDefaults) LinkStyleOr() string {
	if ValidLinkStyle(d.LinkStyle) {
		return d.LinkStyle
	}
	return LinkStyleCompact
}

// GetUserDefaults reads one account's defaults. A user that does not exist →
// ErrNotFound, so a caller can never silently resolve a deleted account's
// upload against the zero value.
func (d *DB) GetUserDefaults(ctx context.Context, userID string) (UserDefaults, error) {
	var (
		out      UserDefaults
		ttl      sql.NullInt64
		unlisted int64
		style    string
	)
	err := d.ro.QueryRowContext(ctx,
		"SELECT default_ttl_days, default_unlisted, default_link_style FROM users WHERE id = ?",
		userID).Scan(&ttl, &unlisted, &style)
	if err != nil {
		if err == sql.ErrNoRows {
			return UserDefaults{}, ErrNotFound
		}
		return UserDefaults{}, err
	}
	if ttl.Valid {
		n := int(ttl.Int64)
		out.TTLDays = &n
	}
	out.Unlisted = unlisted != 0
	out.LinkStyle = style
	return out, nil
}

// SetUserDefaults replaces one account's defaults. The value is written as
// given — the WIRE unit is validated by the handler (config.ParseTTLDays,
// db.ValidLinkStyle), which is the one place §8.2's closed sets live; this
// statement's job is to keep the three TTL states distinct all the way to the
// column. An empty LinkStyle is written as `compact` rather than as an empty
// string, because the column has no absent state and a blank one would read
// back as neither grammar. A user that does not exist → ErrNotFound.
func (d *DB) SetUserDefaults(ctx context.Context, userID string, def UserDefaults) error {
	var ttl any
	if def.TTLDays != nil {
		ttl = int64(*def.TTLDays)
	}
	n, err := d.exec(ctx,
		"UPDATE users SET default_ttl_days = ?, default_unlisted = ?, default_link_style = ? WHERE id = ?",
		ttl, boolInt(def.Unlisted), def.LinkStyleOr(), userID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
