package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
)

// backupFixture: a migrated database, a BACKUP_DIR with a decoy in it, and a
// scheduler whose log is captured as JSON, one record per line.
type backupFixture struct {
	cfg  config.Config
	db   *db.DB
	dir  string
	logs bytes.Buffer
	s    *backupScheduler
}

func newBackupFixture(t *testing.T, keep string) *backupFixture {
	t.Helper()
	root := t.TempDir()
	fx := &backupFixture{dir: filepath.Join(root, "backups")}
	if err := os.MkdirAll(fx.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := testEnv(filepath.Join(root, "data"))
	cfg, err := config.Load(func(k string) string {
		switch k {
		case "BACKUP_EVERY":
			return "1h"
		case "BACKUP_DIR":
			return fx.dir
		case "BACKUP_KEEP":
			return keep
		case "SCRATCH_DIR":
			return filepath.Join(root, "scratch")
		}
		return base(k)
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(cfg); code != 0 {
		t.Fatal("migrate")
	}
	d, err := openForTest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	fx.cfg, fx.db = cfg, d
	fx.s = newBackupScheduler(cfg, d, slog.New(slog.NewJSONHandler(&fx.logs, nil)))
	return fx
}

// records returns the captured log records, in order.
func (fx *backupFixture) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(fx.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func (fx *backupFixture) status(t *testing.T) backupStatus {
	t.Helper()
	st, ok := readBackupStatus(fx.cfg.DataDir)
	if !ok {
		t.Fatal("no status file")
	}
	return st
}

func snapshotsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if snapshotRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestFirstBackupRun(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	every := time.Hour
	dir := t.TempDir()
	if got := firstBackupRun(filepath.Join(dir, "missing"), every, now); !got.Equal(now) {
		t.Fatalf("a missing directory runs at once (and fails): %v", got)
	}
	if got := firstBackupRun(dir, every, now); !got.Equal(now) {
		t.Fatalf("an empty directory runs at once: %v", got)
	}
	touch := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	touch("clippet-20261006T100000Z.db") // two hours old
	touch("not-a-snapshot.db")
	if got := firstBackupRun(dir, every, now); !got.Equal(now) {
		t.Fatalf("a snapshot older than BACKUP_EVERY runs at once: %v", got)
	}
	touch("clippet-20261006T113000Z.db") // half an hour old
	if want, got := now.Add(30*time.Minute), firstBackupRun(dir, every, now); !got.Equal(want) {
		t.Fatalf("one interval after the newest: got %v want %v", got, want)
	}
	// A name in the future is ignored, never allowed to push the schedule out.
	touch("clippet-20991231T000000Z.db")
	if want, got := now.Add(30*time.Minute), firstBackupRun(dir, every, now); !got.Equal(want) {
		t.Fatalf("a future-named snapshot moved the schedule: got %v want %v", got, want)
	}
}

func TestBackupRunsWriteRotateAndRecord(t *testing.T) {
	fx := newBackupFixture(t, "2")
	decoy := filepath.Join(fx.dir, "README")
	if err := os.WriteFile(decoy, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	fx.s.now = func() time.Time { return start }
	next := fx.s.boot()
	if !next.Equal(start) {
		t.Fatalf("an empty directory runs at once: %v", next)
	}
	if st := fx.status(t); st.Dir != fx.dir || st.Last != nil || st.Next != rfcTime(start) {
		t.Fatalf("the boot status: %+v", st)
	}
	for i := 0; i < 3; i++ {
		at := start.Add(time.Duration(i) * time.Hour)
		fx.s.now = func() time.Time { return at }
		fx.s.runOnce(context.Background(), at)
	}
	snaps := snapshotsIn(t, fx.dir)
	if len(snaps) != 2 || snaps[0] != "clippet-20261006T130000Z.db" || snaps[1] != "clippet-20261006T140000Z.db" {
		t.Fatalf("keep=2 over three runs: %v", snaps)
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Fatalf("the decoy was touched: %v", err)
	}
	for _, s := range snaps {
		fi, err := os.Stat(filepath.Join(fx.dir, s))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", s, fi.Mode(), err)
		}
	}
	// Exactly one record per run, after the boot line.
	recs := fx.records(t)
	if len(recs) != 4 || recs[0]["msg"] != "backup scheduled" {
		t.Fatalf("records: %v", recs)
	}
	for _, r := range recs[1:] {
		if r["msg"] != "backup written" || r["level"] != "INFO" {
			t.Fatalf("a run's record: %v", r)
		}
		for _, k := range []string{"file", "bytes", "kept", "removed", "took", "next"} {
			if _, ok := r[k]; !ok {
				t.Fatalf("the record has no %q: %v", k, r)
			}
		}
	}
	if recs[3]["file"] != filepath.Join(fx.dir, "clippet-20261006T140000Z.db") || recs[3]["removed"] != float64(1) {
		t.Fatalf("the third run: %v", recs[3])
	}
	// The status file: its exact shape, 0600, and the last run.
	raw, err := os.ReadFile(filepath.Join(fx.cfg.DataDir, backupStatusName))
	if err != nil {
		t.Fatal(err)
	}
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	if len(shape) != 3 || shape["dir"] != fx.dir || shape["next"] != "2026-10-06T15:00:00Z" {
		t.Fatalf("status shape: %s", raw)
	}
	last, _ := shape["last"].(map[string]any)
	if len(last) != 5 || last["at"] != "2026-10-06T14:00:00Z" || last["result"] != "ok" ||
		last["file"] != filepath.Join(fx.dir, "clippet-20261006T140000Z.db") || last["reason"] != "" || last["bytes"].(float64) <= 0 {
		t.Fatalf("status last: %s", raw)
	}
	if fi, err := os.Stat(filepath.Join(fx.cfg.DataDir, backupStatusName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("status mode: %v %v", fi.Mode(), err)
	}
	// Each snapshot restores.
	var out, errOut bytes.Buffer
	for _, s := range snaps {
		if code := restoreCheck([]string{filepath.Join(fx.dir, s)}, adminEnv{stdout: &out, stderr: &errOut}); code != 0 {
			t.Fatalf("restore-check %s: %s%s", s, out.String(), errOut.String())
		}
	}
	// A restart carries the last result over and anchors on the newest file.
	fx.s.now = func() time.Time { return start.Add(150 * time.Minute) }
	if next := fx.s.boot(); !next.Equal(start.Add(3 * time.Hour)) {
		t.Fatalf("a restart reset the clock: %v", next)
	}
	if st := fx.status(t); st.Last == nil || st.Last.Result != "ok" {
		t.Fatalf("a restart lost the last result: %+v", st)
	}
}

func TestBackupMissingDirFailsAndCreatesNothing(t *testing.T) {
	fx := newBackupFixture(t, "")
	if err := os.Remove(fx.dir); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	fx.s.now = func() time.Time { return at }
	fx.s.boot()
	fx.s.runOnce(context.Background(), at)
	if _, err := os.Lstat(fx.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the loop created BACKUP_DIR (or a file by its name): %v", err)
	}
	recs := fx.records(t)
	r := recs[len(recs)-1]
	want := "BACKUP_DIR " + fx.dir + " is not a directory"
	if r["msg"] != "backup failed" || r["level"] != "ERROR" || r["err"] != want || r["next"] != "2026-10-06T13:00:00Z" {
		t.Fatalf("the failed record: %v", r)
	}
	if _, ok := r["file"]; ok {
		t.Fatalf("no snapshot was written, so no file field: %v", r)
	}
	st := fx.status(t)
	if st.Last == nil || st.Last.Result != "failed" || st.Last.Reason != want {
		t.Fatalf("status: %+v", st.Last)
	}
}

// TestBackupSkipNeverHidesAFailure (must 5): a run after a failed one finds
// the failed run's `.part-` file and is skipped; the status still says failed.
func TestBackupSkipNeverHidesAFailure(t *testing.T) {
	fx := newBackupFixture(t, "")
	at := time.Now().UTC().Truncate(time.Second)
	fx.s.now = func() time.Time { return at }
	fx.s.boot()
	fx.s.status.Last = &backupLast{At: rfcTime(at), Result: "failed", Reason: "admin backup: disk full"}
	part := filepath.Join(fx.dir, "clippet-20261006T120000Z.db.part-abc123")
	if err := os.WriteFile(part, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.logs.Reset()
	later := at.Add(time.Minute)
	fx.s.runOnce(context.Background(), later)
	recs := fx.records(t)
	if len(recs) != 1 || recs[0]["msg"] != "backup skipped" || recs[0]["level"] != "INFO" ||
		recs[0]["reason"] != "an unfinished snapshot under an hour old is in the directory" || recs[0]["next"] != rfcTime(later.Add(time.Hour)) {
		t.Fatalf("the skip record: %v", recs)
	}
	if snaps := snapshotsIn(t, fx.dir); len(snaps) != 0 {
		t.Fatalf("a skipped run wrote %v", snaps)
	}
	st := fx.status(t)
	if st.Last == nil || st.Last.Result != "failed" || st.Next != rfcTime(later.Add(time.Hour)) {
		t.Fatalf("the skip replaced the last result or did not move next: %+v %q", st.Last, st.Next)
	}
	// An hour on, the part is stale: the run proceeds and rotation removes it.
	old := at.Add(-2 * time.Hour)
	if err := os.Chtimes(part, old, old); err != nil {
		t.Fatal(err)
	}
	fx.s.runOnce(context.Background(), later)
	if st := fx.status(t); st.Last.Result != "ok" {
		t.Fatalf("the run after the stale part: %+v", st.Last)
	}
	if _, err := os.Stat(part); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rotation left the stale part: %v", err)
	}
}

func TestBackupFailureReasons(t *testing.T) {
	fx := newBackupFixture(t, "")
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	fx.s.now = func() time.Time { return at }
	fx.s.boot()
	// Two runs in the same second: the verb's own refusal, verbatim.
	fx.s.runOnce(context.Background(), at)
	fx.s.runOnce(context.Background(), at)
	st := fx.status(t)
	if st.Last.Result != "failed" || !strings.HasPrefix(st.Last.Reason, "admin backup: ") || !strings.Contains(st.Last.Reason, "already exists") {
		t.Fatalf("the verb's stderr line: %+v", st.Last)
	}
	// A panic inside a run is a failed run, and the loop goes on.
	fx.s.db = nil
	fx.s.runOnce(context.Background(), at.Add(time.Hour))
	st = fx.status(t)
	if st.Last.Result != "failed" || !strings.HasPrefix(st.Last.Reason, "panic: ") {
		t.Fatalf("a recovered panic: %+v", st.Last)
	}
}

func TestBackupLastLine(t *testing.T) {
	if got := lastLine("rotate:  keep x\n\nadmin backup rotate: boom\n\n"); got != "admin backup rotate: boom" {
		t.Fatalf("lastLine = %q", got)
	}
	if got := lastLine(""); got != "" {
		t.Fatalf("lastLine(empty) = %q", got)
	}
}

// TestBackupFailureReasonWording (must 4): a written snapshot whose rotation
// could not remove an older file gets its own sentence — the verb's last
// combined line would be the `rotate:` summary — and every other failure is
// the verb's last STDERR line. (A real removal failure cannot be staged in the
// test image, which runs as root; the outcome it produces is driven here.)
func TestBackupFailureReasonWording(t *testing.T) {
	got := backupFailureReason(backupOutcome{File: "/b/clippet-x.db", RemoveErrs: 2}, 1, "")
	if got != "the snapshot was written, but 2 older file(s) could not be removed" {
		t.Fatalf("remove failure: %q", got)
	}
	got = backupFailureReason(backupOutcome{}, 1, "admin backup: db: backup into /b/x: disk full\n")
	if got != "admin backup: db: backup into /b/x: disk full" {
		t.Fatalf("stderr line: %q", got)
	}
	// reportBackup failing after the file is in place: not a removal failure.
	got = backupFailureReason(backupOutcome{File: "/b/clippet-x.db"}, 1, "admin backup: no such table\n")
	if got != "admin backup: no such table" {
		t.Fatalf("a written file with no removal error: %q", got)
	}
}

// TestBackupLoopStopsOnCancel: Run returns promptly when the server shuts down
// between runs.
func TestBackupLoopStopsOnCancel(t *testing.T) {
	fx := newBackupFixture(t, "")
	// A fresh snapshot, so the first run is an hour away and the loop waits.
	name := "clippet-" + time.Now().UTC().Format(snapshotStamp) + ".db"
	if err := os.WriteFile(filepath.Join(fx.dir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); fx.s.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not stop on cancel")
	}
}

// TestBackupCancelInterruptsVacuum records whether a cancelled context stops a
// VACUUM INTO in flight (the design asked the builder to find out). It builds
// a database large enough that an uncancelled snapshot takes measurably long,
// then cancels one shortly after it starts.
func TestBackupCancelInterruptsVacuum(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("builds a large database: the pass without the race detector runs it")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "big.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE b(x BLOB);
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 64)
		INSERT INTO b SELECT randomblob(512*1024) FROM n`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	t0 := time.Now()
	if err := d.Backup(context.Background(), filepath.Join(dir, "full.db")); err != nil {
		t.Fatal(err)
	}
	full := time.Since(t0)
	if full < 30*time.Millisecond {
		t.Skipf("an uncancelled snapshot took only %s: too fast to observe an interrupt", full)
	}
	ctx, cancel := context.WithCancel(context.Background())
	at := full / 10
	time.AfterFunc(at, cancel)
	t0 = time.Now()
	err = d.Backup(ctx, filepath.Join(dir, "cut.db"))
	took := time.Since(t0)
	t.Logf("uncancelled VACUUM INTO: %s; cancelled after %s: returned in %s with err=%v", full, at, took, err)
	if err == nil || took > full/2 {
		t.Fatalf("a cancel did not interrupt the VACUUM in flight: %s of %s, err=%v", took, full, err)
	}
	// The verb's own half: a snapshot cancelled mid-write removes its `.part-`
	// (run 8), so a clean SIGTERM leaves nothing for the next run to skip on.
	out := t.TempDir()
	ctx2, cancel2 := context.WithCancel(context.Background())
	time.AfterFunc(at, cancel2)
	var stderr bytes.Buffer
	a := &admin{db: d, ctx: ctx2, env: adminEnv{stdout: &stderr, stderr: &stderr}}
	if _, code := a.writeSnapshot(filepath.Join(out, "cut.db")); code == 0 {
		t.Skip("the snapshot finished before the cancel landed")
	}
	if left := mustReadDir(t, out); len(left) != 0 {
		t.Fatalf("a cancelled snapshot left %s", left[0].Name())
	}
}

// TestBackupSkipRetriesWithinTheHour: a skip (a fresh `.part-`) is retried
// when that part turns stale — min(BACKUP_EVERY, an hour) — never a whole
// daily interval on; a run that went ahead is one interval on (run 8).
func TestBackupSkipRetriesWithinTheHour(t *testing.T) {
	fx := newBackupFixture(t, "")
	fx.s.cfg.BackupEvery = 24 * time.Hour
	at := time.Now().UTC().Truncate(time.Second)
	fx.s.now = func() time.Time { return at }
	fx.s.boot()
	part := filepath.Join(fx.dir, "clippet-20261006T120000Z.db.part-abc123")
	if err := os.WriteFile(part, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if next := fx.s.runOnce(context.Background(), at); !next.Equal(at.Add(time.Hour)) {
		t.Fatalf("a skip with a daily interval is next due %v, want an hour on (%v)", next, at.Add(time.Hour))
	}
	if st := fx.status(t); st.Next != rfcTime(at.Add(time.Hour)) {
		t.Fatalf("the status after the skip: %q", st.Next)
	}
	fx.s.cfg.BackupEvery = 20 * time.Minute
	if next := fx.s.runOnce(context.Background(), at); !next.Equal(at.Add(20 * time.Minute)) {
		t.Fatalf("a skip with a 20-minute interval: %v", next)
	}
	if err := os.Remove(part); err != nil {
		t.Fatal(err)
	}
	fx.s.cfg.BackupEvery = 24 * time.Hour
	if next := fx.s.runOnce(context.Background(), at); !next.Equal(at.Add(24 * time.Hour)) {
		t.Fatalf("a written run: %v", next)
	}
}

// TestBackupCancelledRunIsNotAFailure: a run the shutdown cancels logs
// `backup cancelled` at INFO — never `backup failed` at ERROR — and leaves the
// status's last result alone (run 8).
func TestBackupCancelledRunIsNotAFailure(t *testing.T) {
	fx := newBackupFixture(t, "")
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	fx.s.now = func() time.Time { return at }
	fx.s.boot()
	fx.s.runOnce(context.Background(), at)
	fx.logs.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fx.s.runOnce(ctx, at.Add(time.Hour))
	recs := fx.records(t)
	if len(recs) != 1 || recs[0]["msg"] != "backup cancelled" || recs[0]["level"] != "INFO" {
		t.Fatalf("a cancelled run's record: %v", recs)
	}
	if st := fx.status(t); st.Last == nil || st.Last.Result != "ok" {
		t.Fatalf("a cancelled run replaced the last result: %+v", st.Last)
	}
	for _, e := range mustReadDir(t, fx.dir) {
		if partRE.MatchString(e.Name()) {
			t.Fatalf("a cancelled run left %s", e.Name())
		}
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
