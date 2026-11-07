package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modernc.org/sqlite"
)

// healthClasses is the CLOSED set §20 erratum 99 lets /healthz and /readyz
// print. Nothing outside it may ever reach an anonymous prober.
var healthClasses = map[string]bool{
	"ok": true, "timeout": true, "locked": true, "readonly": true,
	"io": true, "full": true, "corrupt": true, "unavailable": true,
}

// TestHealthClass covers every branch HealthClass can be driven into without
// a real driver error: nil, both context errors (wrapped and bare, since a
// db call always wraps), and the DEFAULT that closes the leak. Anything the
// switch does not recognise must come back "unavailable" and must not carry
// one byte of the message.
func TestHealthClass(t *testing.T) {
	secret := "/data/clippet.db"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "ok"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"canceled", context.Canceled, "timeout"},
		{"wrapped deadline", fmt.Errorf("db: ping: %w", context.DeadlineExceeded), "timeout"},
		{"wrapped canceled", fmt.Errorf("db: get clip: %w", context.Canceled), "timeout"},
		{"sql.ErrConnDone", sql.ErrConnDone, "unavailable"},
		{"sql.ErrNoRows", sql.ErrNoRows, "unavailable"},
		{"ErrNotFound", ErrNotFound, "unavailable"},
		{"stranger", errors.New("boom"), "unavailable"},
		{"names the data dir", fmt.Errorf("unable to open database file %s", secret), "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HealthClass(tc.err)
			if got != tc.want {
				t.Fatalf("HealthClass(%v) = %q, want %q", tc.err, got, tc.want)
			}
			if !healthClasses[got] {
				t.Fatalf("HealthClass returned %q, outside the closed set", got)
			}
			if strings.Contains(got, secret) {
				t.Fatalf("HealthClass leaked the path: %q", got)
			}
		})
	}
}

// openTestAt is openTest with the file path handed back, so a test can open
// a second, differently-configured pool on the SAME database.
func openTestAt(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clippet.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d, path
}

// probe opens a throwaway pool on path with extra DSN pragmas and no busy
// wait, so a lock conflict answers immediately instead of after 5 s.
func probe(t *testing.T, path, extra string) *sql.DB {
	t.Helper()
	p, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)"+extra)
	if err != nil {
		t.Fatalf("open probe pool: %v", err)
	}
	p.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestHealthClassOnRealDriverErrors drives HealthClass with errors modernc
// actually produced, which is the half a hand-built error cannot prove:
// *sqlite.Error's fields are unexported, so the primary-code mask and the
// errors.As reach through db.go's own fmt.Errorf wrapping are only tested
// against the real thing. SQLITE_FULL has no portable trigger in a unit test;
// it is covered by TestHealthClass's closed-set assertion and by the switch's
// shape. SQLITE_IOERR proper has none either, but the arm it shares with
// SQLITE_CANTOPEN does: a database under a directory that does not exist.
func TestHealthClassOnRealDriverErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown statement error stays unavailable and leaks nothing", func(t *testing.T) {
		d, _ := openTestAt(t)
		_, err := d.ro.QueryContext(ctx, "SELECT 1 FROM no_such_table_zz")
		if err == nil {
			t.Fatal("querying a missing table succeeded")
		}
		var se *sqlite.Error
		if !errors.As(err, &se) {
			t.Fatalf("driver error is not *sqlite.Error: %T %v", err, err)
		}
		// The premise: the driver's own message DOES name the statement.
		if !strings.Contains(err.Error(), "no_such_table_zz") {
			t.Fatalf("premise broken -- the raw error no longer names the table: %v", err)
		}
		if got := HealthClass(err); got != "unavailable" {
			t.Fatalf("HealthClass(no such table) = %q, want unavailable", got)
		}
		if got := HealthClass(fmt.Errorf("db: list clips: %w", err)); got != "unavailable" {
			t.Fatalf("HealthClass(wrapped) = %q, want unavailable", got)
		}
	})

	t.Run("a locked database is locked", func(t *testing.T) {
		d, path := openTestAt(t)
		// The rw pool's DSN carries _txlock=immediate, so this tx takes the
		// write lock at BEGIN and holds it for the length of the subtest.
		tx, err := d.rw.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin immediate: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = probe(t, path, "").ExecContext(ctx, "CREATE TABLE zz_locked (x)")
		if err == nil {
			t.Fatal("a second writer got the lock")
		}
		if got := HealthClass(err); got != "locked" {
			t.Fatalf("HealthClass(%v) = %q, want locked", err, got)
		}
	})

	t.Run("a read-only database is readonly", func(t *testing.T) {
		_, path := openTestAt(t)
		_, err := probe(t, path, "&_pragma=query_only(1)").ExecContext(ctx, "CREATE TABLE zz_ro (x)")
		if err == nil {
			t.Fatal("a query_only connection accepted a write")
		}
		if got := HealthClass(err); got != "readonly" {
			t.Fatalf("HealthClass(%v) = %q, want readonly", err, got)
		}
	})

	// SQLITE_CANTOPEN (14) used to fall through to the default and read
	// "unavailable", which tells an operator nothing; folded into the "io"
	// arm it says "look at DATA_DIR's mount and permissions". Still inside
	// erratum 99's closed set, so the wire contract is unchanged.
	t.Run("a database that cannot be opened is io", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no-such-dir", "clippet.db")
		d, err := Open(path)
		if err == nil {
			_ = d.Close()
			t.Fatal("Open succeeded under a directory that does not exist")
		}
		var se *sqlite.Error
		if !errors.As(err, &se) {
			t.Fatalf("driver error is not *sqlite.Error: %T %v", err, err)
		}
		if got := HealthClass(err); got != "io" {
			t.Fatalf("HealthClass(%v) = %q, want io", err, got)
		}
		if got := HealthClass(err); strings.Contains(got, path) || strings.Contains(got, "no-such-dir") {
			t.Fatalf("HealthClass leaked the path: %q", got)
		}
	})

	t.Run("a file that is not a database is corrupt", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "garbage.db")
		junk := []byte(strings.Repeat("not a sqlite file at all; ", 32))
		if err := os.WriteFile(path, junk, 0o644); err != nil {
			t.Fatal(err)
		}
		d, err := Open(path)
		if err == nil {
			_ = d.Close()
			t.Fatal("Open accepted a non-database file")
		}
		if got := HealthClass(err); got != "corrupt" {
			t.Fatalf("HealthClass(%v) = %q, want corrupt", err, got)
		}
		// Open's wrapping names the file path; the class must not.
		if got := HealthClass(err); strings.Contains(got, path) || strings.Contains(got, "garbage") {
			t.Fatalf("HealthClass leaked the path: %q", got)
		}
	})
}
