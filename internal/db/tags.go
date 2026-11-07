package db

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Tag grammar (§17 #9, §20 erratum 81). A tag is a short, lower-case,
// URL-safe word: it appears in `?tag=` on two routes and as a chip that links
// to one, so the set of characters it may hold is the set that survives a URL
// and a filename without encoding.
const (
	// MaxTagRunes is the longest tag (the grammar's 1 + 31).
	MaxTagRunes = 32
	// MaxTagsPerClip is the cap the write paths enforce.
	MaxTagsPerClip = 10
)

// BadTagError names the tag that failed the grammar. httpapi turns it into
// 400 invalid_tags with this message; the offending value is quoted because
// it is client text that lands in a JSON string.
type BadTagError struct {
	Tag    string
	Reason string
}

func (e *BadTagError) Error() string {
	return "tag " + quoteTag(e.Tag) + " " + e.Reason
}

// TooManyTagsError is the cap failure. It is separate from BadTagError
// because no single tag is at fault.
type TooManyTagsError struct{ Max int }

func (e *TooManyTagsError) Error() string {
	return "a clip may carry at most " + strconv.Itoa(e.Max) + " tags"
}

// quoteTag renders a rejected tag for a human message: Go-quoted (so a
// control character or a quote cannot break out of the JSON string) and
// truncated, because the value is client-controlled and may be a megabyte.
func quoteTag(s string) string {
	const max = 48
	r := []rune(s)
	if len(r) > max {
		s = string(r[:max]) + "…"
	}
	return strconv.Quote(s)
}

// CleanTag normalises one tag and reports whether it is valid.
//
// Normalisation is deliberately minimal and NOT Unicode-aware: trim the
// surrounding space, then lower-case ASCII letters and nothing else. A
// case-folding pass over the whole of Unicode would map characters that look
// nothing like a-z onto a-z (U+212A KELVIN SIGN → "k", U+0130 → "i̇"), so two
// visibly different tags would silently become one row. Everything that is not
// already `[a-z0-9-]` after that is simply rejected, and the rejection names
// the value.
//
// The accepted shape is ^[a-z0-9][a-z0-9-]{0,31}$: it must start with a letter
// or a digit (so a tag can never be mistaken for a flag or a path segment
// beginning with `-`) and is at most 32 characters long.
func CleanTag(s string) (string, error) {
	raw := strings.TrimSpace(s)
	b := []byte(raw)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	tag := string(b)
	if tag == "" {
		return "", &BadTagError{Tag: s, Reason: "is empty"}
	}
	// RUNES, not bytes. The cap is named in runes and the message says
	// "characters", so counting bytes made a 20-CHARACTER multi-byte tag
	// (twenty "é" is forty bytes) fail with "is longer than 32 characters" —
	// a message about a limit it is nowhere near. Such a tag is rejected
	// either way, by the grammar loop below, and THAT is the message it
	// should carry: the characters are what is wrong with it, not the length.
	if utf8.RuneCountInString(tag) > MaxTagRunes {
		return "", &BadTagError{Tag: s, Reason: "is longer than " + strconv.Itoa(MaxTagRunes) + " characters"}
	}
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return "", &BadTagError{Tag: s,
				Reason: "must be 1-" + strconv.Itoa(MaxTagRunes) + " characters of a-z, 0-9 and -, starting with a letter or a digit"}
		}
	}
	return tag, nil
}

// CleanTags normalises a whole set: every element through CleanTag,
// duplicates collapsed (first occurrence wins), at most MaxTagsPerClip
// SURVIVING tags, sorted. The result is never nil — an empty input is an
// empty set, which is what "this clip has no tags" is spelled as everywhere.
//
// The cap is applied AFTER collapsing duplicates on purpose: sending the same
// tag twelve times is a sloppy client, not a client asking for twelve tags.
func CleanTags(in []string) ([]string, error) {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		tag, err := CleanTag(raw)
		if err != nil {
			return nil, err
		}
		if seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	if len(out) > MaxTagsPerClip {
		return nil, &TooManyTagsError{Max: MaxTagsPerClip}
	}
	sort.Strings(out)
	return out, nil
}

// SplitTags parses the comma-separated spelling the capture form and the
// share-page editor use ("gaming, clutch"). Empty segments are dropped, so a
// trailing comma and a wholly empty field both mean "no tags" rather than a
// 400 — a tool that always emits its arguments must not be forced to invent
// one (the §20 erratum 58 rule, applied to this field).
func SplitTags(s string) []string {
	out := make([]string, 0, 8)
	for _, part := range strings.Split(s, ",") {
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return out
}

// SetClipTags replaces a clip's whole tag set in one transaction: an empty
// slice clears it. The tags are re-normalised here rather than trusted, so the
// stored form is the grammar's form whatever the caller did. A missing or
// deleting clip → ErrNotFound (a tag never lands on a tombstone).
func (d *DB) SetClipTags(ctx context.Context, clipID string, tags []string) error {
	clean, err := CleanTags(tags)
	if err != nil {
		return err
	}
	return d.tx(ctx, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, "SELECT state FROM clips WHERE id = ?", clipID).Scan(&state); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if state == ClipDeleting {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM clip_tags WHERE clip_id = ?", clipID); err != nil {
			return err
		}
		for _, tag := range clean {
			if _, err := tx.ExecContext(ctx, "INSERT INTO clip_tags (clip_id, tag) VALUES (?, ?)", clipID, tag); err != nil {
				return err
			}
		}
		return nil
	})
}

// TagsOf returns one clip's tags, sorted. Never nil.
func (d *DB) TagsOf(ctx context.Context, clipID string) ([]string, error) {
	rows, err := d.ro.QueryContext(ctx, "SELECT tag FROM clip_tags WHERE clip_id = ? ORDER BY tag", clipID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TagsForClips returns the tags of every listed clip in ONE query, keyed by
// clip id, so rendering a page of the library costs one round trip rather
// than one per row. Clips with no tags are absent from the map; callers
// substitute the empty set.
func (d *DB) TagsForClips(ctx context.Context, clipIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(clipIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(clipIDs))
	for i, id := range clipIDs {
		args[i] = id
	}
	q := "SELECT clip_id, tag FROM clip_tags WHERE clip_id IN " +
		placeholders(len(args)) + " ORDER BY clip_id, tag"
	rows, err := d.ro.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, err
		}
		out[id] = append(out[id], tag)
	}
	return out, rows.Err()
}

// tagFilterSQL is the predicate ListClips adds for a tag filter: an IN-list
// over the `(tag, clip_id)` index (`clip_tags_tag`) rather than a join, so no
// row can be returned twice and the visibility and cursor clauses still apply
// to whatever the planner drives from.
//
// It is an IN-list and NOT a correlated `EXISTS (… WHERE t.clip_id = clips.id
// AND t.tag = ?)` on purpose. A correlated EXISTS is evaluated once per
// candidate `clips` row and its only usable index is the PRIMARY KEY
// `(clip_id, tag)` — the `(tag, clip_id)` index migration 0004 adds for
// exactly this query is then dead, and the filter degrades into a full scan of
// `clips` with a subquery probe per row. The uncorrelated form is a LIST
// SUBQUERY: one covering-index range scan of `clip_tags_tag` on `tag = ?`
// yields the whole id set, and the outer query looks those ids up in the
// `clips` PK. `TestListClipsTagFilterUsesTheTagIndex` pins the plan.
const tagFilterSQL = " AND clips.id IN (SELECT clip_id FROM clip_tags WHERE tag = ?)"
