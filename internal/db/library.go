package db

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The library's narrowing filters (ROADMAP #2/#4, §20 errata 113–115).
//
// Everything here NARROWS the visibility set `listClipsSQL` already builds —
// every listed clip plus the viewer's own unlisted ones — and none of it can
// widen it, exactly as the tag filter cannot (§20 erratum 81). The predicates
// are appended inside that WHERE, never beside it.

// MaxSearchRunes bounds `?q=`. Sixty-four characters is longer than any clip
// title a person types into a search box and short enough that the pattern
// handed to SQLite can never itself be the load: the scan is over the rows,
// not over the needle.
const MaxSearchRunes = 64

// ErrInvalidSearch is a `?q=` that cannot be a search: too long, not valid
// UTF-8, or carrying a control character. httpapi maps it to 400
// invalid_request and the library page bounces like an illegal `?tag=`.
var ErrInvalidSearch = errors.New("db: invalid search query")

// CleanSearch normalises `?q=`: trim, then ASCII-lower-case, and reject
// anything that is not a plausible search term.
//
// The lower-casing is ASCII-only, for erratum 81's reason applied to a
// different field: a full Unicode fold maps characters that look nothing like
// a–z onto a–z (U+212A KELVIN SIGN → "k"), so a search for one word would
// silently match a different one. SQLite's own `lower()` — which the SQL side
// of this comparison uses — is ASCII-only too, so folding more here would only
// make the two halves disagree.
//
// An empty (or all-space) value is NOT an error: it is the absence of a
// filter, which is what an empty search box means.
func CleanSearch(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", ErrInvalidSearch
	}
	q := strings.TrimSpace(s)
	if q == "" {
		return "", nil
	}
	if utf8.RuneCountInString(q) > MaxSearchRunes {
		return "", ErrInvalidSearch
	}
	for _, r := range q {
		if unicode.IsControl(r) {
			return "", ErrInvalidSearch
		}
	}
	return asciiLower(q), nil
}

// asciiLower lower-cases A–Z and nothing else (see CleanSearch).
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// searchSQL is the `?q=` predicate: a substring of the title OR of the
// description, case-insensitive for ASCII.
//
// `lower(col) LIKE ?` over a pattern this package lower-cased is the whole
// mechanism, and there is no index behind it on purpose. A `%needle%` LIKE
// cannot use a B-tree in any database — the leading wildcard means every row
// is a candidate — so an index on `title` would be dead weight and an
// expression index on `lower(title)` would be dead weight with a migration
// attached. The bound is the scan itself: it runs INSIDE the visibility
// clause, over `clips` alone (never over `clip_tags` or `jobs`), on a table a
// personal instance keeps in the low tens of thousands of rows, and it stops
// at LIMIT+1 matches. `TestListClipsSearchPlan` pins that the statement scans
// exactly one table.
//
// ESCAPE '\' is not decoration: `%` and `_` are LIKE metacharacters, so a
// search for "50%" without it would match every row. escapeLike quotes all
// three (including the escape character itself) before the pattern is built.
const searchSQL = " AND (lower(title) LIKE ? ESCAPE '\\' OR lower(description) LIKE ? ESCAPE '\\')"

// ownerFilterSQL narrows the list to one owner. It is the SAME predicate for
// `?mine=1` and for LIBRARY_SCOPE=own — the difference between them is where
// the decision came from, not what it does — and it is applied at most once
// even when both are in force.
const ownerFilterSQL = " AND owner_id = ?"

// searchPattern turns a cleaned query into the LIKE pattern.
func searchPattern(q string) string { return "%" + escapeLike(q) + "%" }

// escapeLike quotes the three characters LIKE gives meaning to, using the
// backslash that searchSQL declares as its ESCAPE.
func escapeLike(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if r == '\\' || r == '%' || r == '_' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// listFilter is the set of narrowing predicates ONE request asked for, and
// therefore the thing a page cursor is minted under (§20 erratum 115).
//
// LIBRARY_SCOPE is deliberately NOT in it. A filter is a choice the reader
// made and can unmake with the next link, so a cursor that outlives it must
// be refused; the scope is the instance's configuration, identical for every
// request the reader can make, and refusing every outstanding cursor because
// the operator edited .env would turn a settings change into a wall of
// "invalid cursor". Narrowing the set under a live cursor is safe on its own
// terms: the order is total over (created_at DESC, id ASC) and the cursor
// names a position in it, so removing rows can only shorten a page, never
// skip past one.
type listFilter struct {
	Tag  string
	Q    string
	Mine bool
}

func (f listFilter) empty() bool { return f == listFilter{} }

// tagOnly reports whether the legacy three-field cursor shape says everything
// about this filter.
func (f listFilter) tagOnly() bool { return f.Tag != "" && f.Q == "" && !f.Mine }

// encode renders the filter as the cursor's key=value field. url.Values.Encode
// sorts its keys, so the same filter always mints the same token, and it
// percent-escapes, so no value can carry the ':' the cursor splits on.
func (f listFilter) encode() string {
	v := url.Values{}
	if f.Tag != "" {
		v.Set("t", f.Tag)
	}
	if f.Q != "" {
		v.Set("q", f.Q)
	}
	if f.Mine {
		v.Set("mine", "1")
	}
	return v.Encode()
}

// decodeFilter reads the cursor's filter field back. A legacy token carries a
// bare tag there (the tag grammar has no '=', so the two shapes cannot be
// confused); anything else must be the key=value form, with the exact keys
// this version mints and values that survive their own validators — a cursor
// is our own token, so a value that does not round-trip is a forged or
// corrupted one and gets no benefit of the doubt.
func decodeFilter(s string) (listFilter, error) {
	if !strings.Contains(s, "=") {
		clean, err := CleanTag(s)
		if err != nil || clean != s {
			return listFilter{}, ErrInvalidCursor
		}
		return listFilter{Tag: s}, nil
	}
	vals, err := url.ParseQuery(s)
	if err != nil {
		return listFilter{}, ErrInvalidCursor
	}
	var f listFilter
	for key, got := range vals {
		if len(got) != 1 {
			return listFilter{}, ErrInvalidCursor
		}
		switch key {
		case "t":
			clean, err := CleanTag(got[0])
			if err != nil || clean != got[0] {
				return listFilter{}, ErrInvalidCursor
			}
			f.Tag = clean
		case "q":
			clean, err := CleanSearch(got[0])
			if err != nil || clean != got[0] || clean == "" {
				return listFilter{}, ErrInvalidCursor
			}
			f.Q = clean
		case "mine":
			if got[0] != "1" {
				return listFilter{}, ErrInvalidCursor
			}
			f.Mine = true
		default:
			return listFilter{}, ErrInvalidCursor
		}
	}
	if f.empty() {
		return listFilter{}, ErrInvalidCursor
	}
	return f, nil
}

// --- batched progress (ROADMAP #15, §20 erratum 116) ---

// LibraryPageSize is how many clips the library renders on one page, and it
// is the single home for that number. Three other constants were independently
// spelled 100 with the same reasoning in their comments — the batched progress
// read, the bulk-action cap, and the SSR page size — and nothing failed if one
// of them drifted: a bulk selection of a full page would simply start 400ing.
// They are all derived from this one now. The fourth spelling, `libraryPollMax`
// in web/static/app.js, cannot import Go and is pinned by a scripts/check.sh
// grep instead.
const LibraryPageSize = 100

// MaxProgressIDs bounds one batched progress read. It is the library's page
// size: the batch exists to replace that page's N polls with one, so asking
// for more than a page could hold is asking for something no page needs.
const MaxProgressIDs = LibraryPageSize

// ProgressRow is the DATABASE half of one clip's §8.4 progress: the clip's
// own state and the stage of its latest TRANSCODE job (never merely its
// newest job — since Phase 4 that is routinely a notify, a trim or a gif;
// §20 erratum 89). The worker's in-memory registry is layered over it by the
// handler, exactly as it is for the single-clip route.
type ProgressRow struct {
	ClipID, State string
	UpdatedAt     int64
	FailureReason *string
	// Stage is "" when the clip has no transcode job at all; JobUpdatedAt is
	// then 0 and Checkpointed false.
	Stage        string
	JobUpdatedAt int64
	Checkpointed bool
	// The quality tiers' two stored words and whether each tier's OBJECT exists
	// (§20 erratum 205). They are here rather than derived by the handler for
	// erratum 116's reason: the batched poll and the single poll must reconcile
	// the same facts through the same function (db.TierStatesOf), and this route
	// reads a projection rather than a whole clip.
	FullState, LightState string
	HasFull, HasLight     bool
}

// ProgressRows reads the progress facts of up to MaxProgressIDs clips in ONE
// query. Unknown ids and `deleting` tombstones are simply ABSENT from the
// result — the batch analogue of the single route's 404, which a poller must
// read as "gone", never as "done" (§8.4).
func (d *DB) ProgressRows(ctx context.Context, clipIDs []string) ([]ProgressRow, error) {
	if len(clipIDs) == 0 {
		return nil, nil
	}
	if len(clipIDs) > MaxProgressIDs {
		return nil, ErrLimitReached
	}
	args := make([]any, len(clipIDs))
	for i, id := range clipIDs {
		args[i] = id
	}
	// The LEFT JOIN's ON is a scalar subquery rather than a correlated
	// EXISTS for erratum 84's reason: it is driven from jobs_clip
	// (clip_id, id DESC), one seek per clip, and the outer side is the clips
	// primary key. At most 100 rows either way, but the shape is the one the
	// indexes were built for.
	q := `
SELECT c.id, c.state, c.updated_at, c.failure_reason,
       COALESCE(j.stage, ''), COALESCE(j.updated_at, 0), (j.checkpoint_json IS NOT NULL),
       c.full_state, c.light_state,
       (c.rendition_key IS NOT NULL AND c.original_remuxed = 0), (c.social_key IS NOT NULL)
FROM clips c
LEFT JOIN jobs j ON j.id = (SELECT id FROM jobs WHERE clip_id = c.id AND kind = 'transcode' ORDER BY id DESC LIMIT 1)
WHERE c.state <> 'deleting' AND c.id IN ` + placeholders(len(args))
	rows, err := d.ro.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProgressRow
	for rows.Next() {
		var r ProgressRow
		var reason sql.NullString
		var checkpointed, hasFull, hasLight int64
		if err := rows.Scan(&r.ClipID, &r.State, &r.UpdatedAt, &reason, &r.Stage, &r.JobUpdatedAt, &checkpointed,
			&r.FullState, &r.LightState, &hasFull, &hasLight); err != nil {
			return nil, err
		}
		r.FailureReason = ptrStr(reason)
		r.Checkpointed = checkpointed != 0
		r.HasFull, r.HasLight = hasFull != 0, hasLight != 0
		out = append(out, r)
	}
	return out, rows.Err()
}
