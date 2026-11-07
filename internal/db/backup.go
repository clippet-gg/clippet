package db

// backup.go — the two halves of the operator's backup story (DESIGN.md §20
// erratum 83): taking a consistent snapshot of a LIVE database, and reading
// one back without touching it.
//
// There is no third half: restoring is copying the file back while the app is
// stopped, which needs no code and is documented in DEPLOY.md §15.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Backup writes a consistent snapshot of the whole database to dst, which must
// NOT exist or be an empty regular file (SQLite refuses to overwrite, and so
// do we — a backup command that can destroy the previous backup is a footgun).
//
// The mechanism is `VACUUM INTO`, which SQLite runs inside a read transaction:
// on a WAL database that means a snapshot as of one instant, including
// everything committed to the -wal file, taken without blocking the writer and
// without a `.db-wal`/`.db-shm` pair of its own. It is therefore safe to run
// against the file a live `clippet serve` is writing to — from this process or,
// as the admin CLI does, from a second one.
//
// It runs on the rw pool: that pool holds a single connection, so a backup and
// this process's own writes serialise instead of fighting over the file, and
// `VACUUM` (which cannot run inside a transaction) is never wrapped in one.
func (d *DB) Backup(ctx context.Context, dst string) error {
	// dst may exist only as an EMPTY regular file — the 0600 part the admin
	// verb creates first so the snapshot is never world-readable (§20 erratum
	// 321 (k)); SQLite's `VACUUM INTO` writes into such a file and refuses any
	// other. A file with a byte in it is refused here as before: nothing is
	// ever overwritten.
	if fi, err := os.Lstat(dst); err == nil {
		if !fi.Mode().IsRegular() || fi.Size() != 0 {
			return fmt.Errorf("db: backup: %s already exists", dst)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("db: backup: %s: %w", dst, err)
	}
	// The destination is a bound parameter, not string-formatted SQL: a path
	// with a quote in it is data, not syntax.
	if _, err := d.rw.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("db: backup into %s: %w", dst, err)
	}
	return nil
}

// maxIntegrityLines bounds how much of a failing integrity_check is reported.
const maxIntegrityLines = 10

// IntegrityCheck runs `PRAGMA integrity_check` against the OPEN database and
// returns its verdict verbatim ("ok" when the pages are sound). It is
// Inspect's first read, reachable for a database that is already open — which
// is what `clippet admin doctor` needs (§20 erratum 143): the file it is
// asked about is the live one, so it cannot be opened read-only a second time
// while a `serve` process is replaying the WAL.
//
// It reads on the ro pool, so it never queues behind the single writer.
func (d *DB) IntegrityCheck(ctx context.Context) (string, error) {
	return integrityCheck(ctx, d.ro)
}

// integrityCheck is the shared body: run the pragma, join the complaints and
// bound how many of them are reported. A sound database answers with the
// single line "ok"; a corrupt one answers with up to 100 complaints, which is
// a wall of text an operator does not need in order to decide not to restore
// this file.
func integrityCheck(ctx context.Context, q querier) (string, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "", fmt.Errorf("integrity_check: %w", err)
	}
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return "", fmt.Errorf("integrity_check: %w", err)
		}
		lines = append(lines, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("integrity_check: %w", err)
	}
	if len(lines) == 0 {
		return "", errors.New("integrity_check returned nothing")
	}
	if len(lines) > maxIntegrityLines {
		return fmt.Sprintf("%s; …and %d more", strings.Join(lines[:maxIntegrityLines], "; "), len(lines)-maxIntegrityLines), nil
	}
	return strings.Join(lines, "; "), nil
}

// Snapshot is what Inspect could read out of a database file. Integrity is the
// verbatim result of `PRAGMA integrity_check` ("ok" when the file is sound).
type Snapshot struct {
	Path      string
	Size      int64
	Integrity string
	Applied   []int
	Users     int
	Clips     int
	Jobs      int
}

// OK reports whether the file is a sound Clippet database: SQLite says the
// pages are consistent and at least the first migration is recorded.
func (s Snapshot) OK() bool { return s.Integrity == "ok" && len(s.Applied) > 0 }

// Inspect opens the database file at path READ-ONLY and reports what it holds.
// It is the dry run of a restore: nothing is written, no journal mode is
// changed, no migration is applied, and a file that is not a database (or is a
// truncated one) comes back as an error rather than a panic.
//
// It expects a file with no live `-wal` sidecar — a backup taken by Backup, or
// the database of a stopped app. Pointed at a database a running app is
// writing to, SQLite may refuse the read-only open because replaying the WAL
// needs write access; that is the correct answer, not something to work around.
func Inspect(ctx context.Context, path string) (Snapshot, error) {
	s := Snapshot{Path: path}
	fi, err := os.Stat(path)
	if err != nil {
		return s, fmt.Errorf("db: inspect: %w", err)
	}
	if fi.IsDir() {
		return s, fmt.Errorf("db: inspect: %s is a directory", path)
	}
	s.Size = fi.Size()

	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return s, fmt.Errorf("db: inspect: open: %w", err)
	}
	ro.SetMaxOpenConns(1)
	defer ro.Close()

	// integrity_check is the first read: on a file that is not SQLite at all
	// this is where "file is not a database" surfaces, and it is the answer an
	// operator actually wants before they copy this file over a live one.
	s.Integrity, err = integrityCheck(ctx, ro)
	if err != nil {
		return s, fmt.Errorf("db: inspect: %w", err)
	}

	var hasMigrations int
	if err := ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'").Scan(&hasMigrations); err != nil {
		return s, fmt.Errorf("db: inspect: read schema: %w", err)
	}
	if hasMigrations == 0 {
		return s, errors.New("db: inspect: no schema_migrations table — this is not a Clippet database")
	}

	verRows, err := ro.QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return s, fmt.Errorf("db: inspect: read schema_migrations: %w", err)
	}
	for verRows.Next() {
		var v int
		if err := verRows.Scan(&v); err != nil {
			verRows.Close()
			return s, fmt.Errorf("db: inspect: read schema_migrations: %w", err)
		}
		s.Applied = append(s.Applied, v)
	}
	verRows.Close()
	if err := verRows.Err(); err != nil {
		return s, fmt.Errorf("db: inspect: read schema_migrations: %w", err)
	}

	for _, q := range []struct {
		sql string
		out *int
	}{
		{"SELECT COUNT(*) FROM users", &s.Users},
		{"SELECT COUNT(*) FROM clips", &s.Clips},
		{"SELECT COUNT(*) FROM jobs", &s.Jobs},
	} {
		if err := ro.QueryRowContext(ctx, q.sql).Scan(q.out); err != nil {
			return s, fmt.Errorf("db: inspect: %s: %w", q.sql, err)
		}
	}
	return s, nil
}
