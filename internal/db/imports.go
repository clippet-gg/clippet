package db

// imports.go — the one question `clippet admin import` asks (ROADMAP #51,
// DESIGN.md §20 erratum 307; migration 0020).
//
// The import verb is idempotent by CONTENT, and the content is already
// remembered: `clips.source_etag` is the hex sha256 of the original, computed
// by the store while it wrote the bytes at ingest. There is no `import_seen`
// table and there is deliberately no second memory to keep in step — a clip
// that has been deleted is gone, and importing that file again is the right
// answer rather than a row pointing at nothing.
//
// The lookup is by SIZE first (0020's index) because size is free and the hash
// is a read: a file whose size matches nothing this account owns cannot be a
// duplicate, so the verb streams it straight through the upload path instead of
// reading it twice.

import (
	"context"
	"database/sql"
)

// SourceDigest is one clip's identity as an importer sees it: the id to print,
// and the sha256 of the original to compare a file against. State and
// ExpiresAt are what the duplicate check of an upload (§20 erratum 326) needs
// to tell a live clip from a failed or an expired one; the ingest core and the
// import verb both read them through internal/httpapi's one predicate.
type SourceDigest struct {
	ID        string
	ETag      string
	State     string
	ExpiresAt *int64
}

// ClipsWithSourceSize lists this owner's clips whose ORIGINAL is exactly this
// many bytes, newest first.
//
// `deleting` rows are left out: those objects are on their way to being
// removed, and saying "already here" about one would be a lie by the time the
// operator read it.
func (d *DB) ClipsWithSourceSize(ctx context.Context, ownerID string, size int64) ([]SourceDigest, error) {
	rows, err := d.ro.QueryContext(ctx, `
SELECT id, source_etag, state, expires_at FROM clips
WHERE owner_id = ? AND source_size = ? AND state <> 'deleting'
ORDER BY created_at DESC, id`, ownerID, size)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceDigest
	for rows.Next() {
		var s SourceDigest
		var exp sql.NullInt64
		if err := rows.Scan(&s.ID, &s.ETag, &s.State, &exp); err != nil {
			return nil, err
		}
		if exp.Valid {
			v := exp.Int64
			s.ExpiresAt = &v
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
