package db

// drop.go — the drop folder's memory (ROADMAP #42 (b), DESIGN.md §20 erratum
// 296; migration 0018).
//
// Three statements, none of them on any hot path: the watcher asks once per
// candidate file it is about to ingest, writes once per decision it takes, and
// prunes on the ticks that wrote. Nothing else in the product reads this table.
//
// Everything is keyed by (name, size, mtime) — what a directory listing already
// knows about a file — and 0018 says why that is the right key and what it is
// deliberately not good enough to recognise.

import "context"

// DropSeen reports whether this exact file has already been decided about:
// ingested into a clip, or ingested and FAILED. Both are decisions, and the
// watcher acts on neither again.
func (d *DB) DropSeen(ctx context.Context, name string, size, mtime int64) (bool, error) {
	var n int
	err := d.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM drop_seen WHERE name = ? AND size = ? AND mtime = ?`,
		name, size, mtime).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkDropSeen records the decision. clipID is the clip the file became, or ""
// when the ingest failed — stored as NULL, because "no clip" is an absence and
// an empty string in an id column is a value nobody could read.
//
// It UPSERTS rather than inserting: the one way the same key is written twice
// is a crash between this write and the staged file's removal, and the row that
// wins should be the newer decision rather than an error nobody can act on.
func (d *DB) MarkDropSeen(ctx context.Context, name string, size, mtime, at int64, clipID string) error {
	var id any
	if clipID != "" {
		id = clipID
	}
	_, err := d.rw.ExecContext(ctx, `
INSERT INTO drop_seen (name, size, mtime, at, clip_id) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(name, size, mtime) DO UPDATE SET at = excluded.at, clip_id = excluded.clip_id`,
		name, size, mtime, at, id)
	return err
}

// PruneDropSeen deletes every decision taken strictly before `at` and returns
// how many rows went. The watcher calls it on the ticks that wrote a row, with
// now minus its retention window; on a folder nobody has dropped into it is
// never called at all.
func (d *DB) PruneDropSeen(ctx context.Context, at int64) (int, error) {
	res, err := d.rw.ExecContext(ctx, `DELETE FROM drop_seen WHERE at < ?`, at)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
