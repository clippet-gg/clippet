// Package db owns every SQL statement Clippet runs against its SQLite file
// (DESIGN.md §5). Two pools share one file: rw (one connection, BEGIN
// IMMEDIATE) serialises every write in-process so SQLITE_BUSY between
// goroutines cannot occur; ro (four connections) serves reads.
//
// All timestamps are unix seconds UTC. Nullable columns map to pointer fields.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Sentinel errors. httpapi maps ErrNotFound → 404, ErrConflict → 409/500
// (per route), ErrStateMismatch → 409, ErrInvalidCursor → 400 invalid_cursor;
// every other error is a backend failure → 503.
var (
	ErrNotFound      = errors.New("db: not found")
	ErrConflict      = errors.New("db: conflict")
	ErrStateMismatch = errors.New("db: state mismatch")
	ErrInvalidCursor = errors.New("db: invalid cursor")
	// ErrInvalidPattern is admin-only: PruneUsers refuses an empty name
	// pattern rather than reading it as "every user" (§20 erratum 86).
	ErrInvalidPattern = errors.New("db: empty pattern")
	// ErrMigrationOutOfOrder is Migrate's refusal to apply a migration file
	// whose version is BELOW the highest version already applied (AUDIT #23).
	// Files are applied in ascending order and recorded by version, so a file
	// that fills a HOLE — 0006, 0009 and 0010 are holes today — runs AFTER
	// every later migration on an existing database and BEFORE all of them on
	// a fresh one. Two instances of the same build would then hold two
	// different schemas, arrived at by two different orders, with nothing said
	// out loud. A fresh database has max(applied)=0 and can never meet this.
	ErrMigrationOutOfOrder = errors.New("db: migration out of order")
)

// Extended SQLite result codes for the two constraint classes that mean
// "this id/key already exists". Every other SQLITE_CONSTRAINT (FOREIGN KEY,
// CHECK, NOT NULL, …) is a backend failure, never a conflict.
const (
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
)

// DB wraps the two connection pools.
type DB struct {
	rw *sql.DB // single writer; every write goes here
	ro *sql.DB // readers
}

// dsnPragmas is the §5 DSN. `synchronous(FULL)`, not NORMAL (§20 erratum
// 100): under WAL, NORMAL survives an application crash but may roll back
// COMMITTED transactions after a host power loss, which is the exact inverse
// of what §8.3 and §11 promise — the original object is fsynced to the
// bucket before the 201, so a lost clip row leaves an object with no row,
// findable only by `admin orphans` after its 1 h min-age. FULL costs one
// fsync per commit; this application's write rate is tens of commits per
// minute (an upload, a state flip, a session touch), so the durability is
// bought with a cost nothing here can measure.
const dsnPragmas = "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)"

// Open opens (creating if needed) the SQLite file at path with the §5 DSN and
// verifies WAL mode. It does not migrate; call Migrate next.
func Open(path string) (*DB, error) {
	dsn := "file:" + path + "?" + dsnPragmas
	rw, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("db: open rw: %w", err)
	}
	rw.SetMaxOpenConns(1)
	rw.SetMaxIdleConns(1)
	rw.SetConnMaxLifetime(0)

	ro, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("db: open ro: %w", err)
	}
	ro.SetMaxOpenConns(4)
	ro.SetMaxIdleConns(4)
	ro.SetConnMaxLifetime(0)

	d := &DB{rw: rw, ro: ro}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mode string
	if err := rw.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("db: read journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		_ = d.Close()
		return nil, fmt.Errorf("db: journal_mode is %q, want wal", mode)
	}
	if err := ro.PingContext(ctx); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("db: ping ro: %w", err)
	}
	return d, nil
}

// Close closes both pools.
func (d *DB) Close() error {
	return errors.Join(d.rw.Close(), d.ro.Close())
}

// Ping is the /healthz and /readyz DB liveness probe: one trivial statement
// on the reader pool. It deliberately never touches rw — that pool has a
// single connection, so a ping there would queue behind whatever write
// transaction (or admin-CLI file lock, up to busy_timeout) is in flight and
// could turn a live process into a failed compose healthcheck (§8.1).
func (d *DB) Ping(ctx context.Context) error {
	var one int
	if err := d.ro.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("db: ping ro: %w", err)
	}
	return nil
}

// Migrate creates schema_migrations and applies every unapplied embedded
// migration in ascending version order, one transaction per file. It is
// idempotent. AppliedVersions reports what is present afterwards.
//
// It refuses, before applying anything, a file that would be applied OUT OF
// ORDER: an unapplied version below max(applied) (ErrMigrationOutOfOrder,
// AUDIT #23). The check is made over the whole list first, so a database is
// never left half-migrated by it.
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.rw.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		return fmt.Errorf("db: create schema_migrations: %w", err)
	}
	applied, err := d.AppliedVersions(ctx)
	if err != nil {
		return err
	}
	have := make(map[int]bool, len(applied))
	for _, v := range applied {
		have[v] = true
	}
	files, err := migrationFiles()
	if err != nil {
		return err
	}
	if err := checkMigrationOrder(files, have, maxOf(applied)); err != nil {
		return err
	}
	for _, m := range files {
		if have[m.version] {
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// maxOf is the highest applied version, or 0 on a fresh database.
// AppliedVersions is ascending, so this is its last element.
func maxOf(applied []int) int {
	if len(applied) == 0 {
		return 0
	}
	return applied[len(applied)-1]
}

// checkMigrationOrder reports the FIRST unapplied file whose version is below
// maxApplied, which is a file this database can only apply out of order
// (ErrMigrationOutOfOrder). It names the file and the version it would have
// landed behind, because the operator's fix is to renumber the file above
// max(applied) — never to delete a row from schema_migrations.
func checkMigrationOrder(files []migration, have map[int]bool, maxApplied int) error {
	for _, m := range files {
		if have[m.version] || m.version >= maxApplied {
			continue
		}
		return fmt.Errorf("%w: %s would be applied after %04d, which is already applied; renumber it above %04d",
			ErrMigrationOutOfOrder, m.name, maxApplied, maxApplied)
	}
	return nil
}

// AppliedVersions lists applied migration versions in ascending order.
func (d *DB) AppliedVersions(ctx context.Context) ([]int, error) {
	rows, err := d.rw.QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("db: read schema_migrations: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Born is when this database was first migrated: the applied_at of its
// lowest migration, unix seconds (§20 erratum 343). It is the database's
// identity for the bucket snapshot — `VACUUM INTO` and a restore both carry it,
// and two databases created apart never share it — so a server never
// overwrites a snapshot another database wrote (a fresh PC booted against the
// bucket before its restore, a probe on the same key prefix).
func (d *DB) Born(ctx context.Context) (int64, error) {
	var born int64
	err := d.rw.QueryRowContext(ctx, "SELECT applied_at FROM schema_migrations ORDER BY version LIMIT 1").Scan(&born)
	if err != nil {
		return 0, fmt.Errorf("db: read schema_migrations: %w", err)
	}
	return born, nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func migrationFiles() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("db: read migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("db: migration %q lacks NNNN_ prefix", name)
		}
		v, err := strconv.Atoi(prefix)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("db: migration %q has a bad version prefix", name)
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("db: duplicate migration version %d", out[i].version)
		}
	}
	return out, nil
}

// applyMigration applies one file inside BEGIN IMMEDIATE and re-checks
// schema_migrations under that lock, so two processes migrating the same
// fresh file concurrently (`clippet migrate` racing `serve`) serialise: the
// loser sees the version already recorded and skips instead of failing on
// CREATE TABLE.
func (d *DB) applyMigration(ctx context.Context, m migration) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = ?", m.version).Scan(&n); err != nil {
			return fmt.Errorf("db: check %s: %w", m.name, err)
		}
		if n > 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("db: apply %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
			m.version, time.Now().Unix()); err != nil {
			return fmt.Errorf("db: record %s: %w", m.name, err)
		}
		return nil
	})
}

// tx runs fn inside one BEGIN IMMEDIATE transaction on the writer pool.
func (d *DB) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// isUniqueViolation reports whether err is a PRIMARY KEY or UNIQUE
// violation (modernc enables extended result codes, so Code() carries the
// extended code). A FOREIGN KEY or CHECK failure is NOT a unique violation:
// callers map only this to ErrConflict; everything else stays a DB error.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqliteConstraintPrimaryKey, sqliteConstraintUnique:
			return true
		}
	}
	return false
}

// Extended SQLite result codes whose PRIMARY code decides a health class
// (§20 erratum 99). Only the primary code is read: the extended codes fan
// out into dozens of values and every one of them means the same thing to
// an anonymous prober.
const (
	sqlitePrimaryMask = 0xff
	sqliteBusy        = 5  // SQLITE_BUSY
	sqliteLocked      = 6  // SQLITE_LOCKED
	sqliteReadOnly    = 8  // SQLITE_READONLY
	sqliteIOErr       = 10 // SQLITE_IOERR
	sqliteCorrupt     = 11 // SQLITE_CORRUPT
	sqliteCantOpen    = 14 // SQLITE_CANTOPEN
	sqliteFull        = 13 // SQLITE_FULL
	sqliteNotADB      = 26 // SQLITE_NOTADB
)

// HealthClass renders the bounded CLASS of a database failure for the
// anonymous /healthz and /readyz probes (§20 erratum 99). It is the DB half
// of what erratum 31 did for storage: a modernc/SQLite error can name
// DATA_DIR's file path or the failing statement, and §15 says /healthz
// exposes no configuration values, so the probes print a class out of this
// closed set — timeout, locked, readonly, io, full, corrupt, unavailable —
// and log the full error instead. The leak is closed by DEFAULT: anything
// not recognised is "unavailable", never the message.
func HealthClass(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & sqlitePrimaryMask {
		case sqliteBusy, sqliteLocked:
			return "locked"
		case sqliteReadOnly:
			return "readonly"
		case sqliteIOErr, sqliteCantOpen:
			// SQLITE_CANTOPEN is folded into "io" rather than left to the
			// default: the file under DATA_DIR could not be opened at all,
			// which is a mount or a permission an operator can act on, and
			// "unavailable" tells them nothing about where to look. It stays
			// inside the closed set erratum 99 pins, so nothing new can reach
			// the prober.
			return "io"
		case sqliteFull:
			return "full"
		case sqliteCorrupt, sqliteNotADB:
			return "corrupt"
		}
	}
	return "unavailable"
}

// affected returns the rows affected by a write, or the error.
func affected(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// exec runs a single write on the writer pool and returns rows affected.
func (d *DB) exec(ctx context.Context, query string, args ...any) (int64, error) {
	return affected(d.rw.ExecContext(ctx, query, args...))
}

// Nullable helpers.

func nullStr(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

func nullInt64(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

func nullFloat64(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}

func ptrFloat64(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

func ptrStr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	s := n.String
	return &s
}

func ptrInt64(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func ptrInt(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func ptrBool(n sql.NullInt64) *bool {
	if !n.Valid {
		return nil
	}
	v := n.Int64 != 0
	return &v
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// emptyNull writes an empty string as SQL NULL. Since the quality tiers a
// rendition commit can legitimately have NO rendition object — a playable
// original that needed no remux is streamed as it was uploaded (§20 erratum
// 201) — and the difference between NULL and ” is the difference between
// "there is no such object" and "there is one, at the empty key". Every reader
// of these columns tests for NULL.
func emptyNull(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// zeroNull writes a non-positive number as SQL NULL, for the same reason
// emptyNull exists: a size or a dimension nobody measured is absent, not zero.
func zeroNull(n int64) sql.NullInt64 {
	if n <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: n, Valid: true}
}
