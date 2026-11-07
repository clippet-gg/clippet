package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"clippet/internal/ids"
)

// TestBackupOfAQuietDatabaseIsExact: the simple half. A snapshot of a database
// nobody is writing to holds exactly the rows the original holds, on the same
// schema, and opens read-only afterwards.
func TestBackupOfAQuietDatabaseIsExact(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "carol")
	for range 3 {
		mkClip(t, d, u, 1000, nil, false)
	}
	dst := filepath.Join(t.TempDir(), "backup.db")
	if err := d.Backup(ctx, dst); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	s, err := Inspect(ctx, dst)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !s.OK() || s.Integrity != "ok" {
		t.Fatalf("snapshot not sound: %+v", s)
	}
	if s.Users != 1 || s.Clips != 3 || s.Jobs != 3 {
		t.Fatalf("row counts = users %d clips %d jobs %d, want 1/3/3", s.Users, s.Clips, s.Jobs)
	}
	applied, err := d.AppliedVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Applied) != len(applied) {
		t.Fatalf("schema applied=%v, source has %v", s.Applied, applied)
	}
	if s.Size <= 0 {
		t.Fatalf("size = %d", s.Size)
	}
	// VACUUM INTO produces a single file: no -wal, no -shm to carry around.
	for _, side := range []string{dst + "-wal", dst + "-shm"} {
		if _, err := os.Stat(side); err == nil {
			t.Errorf("%s exists beside the backup", side)
		}
	}
	// And it refuses to write over an existing file.
	if err := d.Backup(ctx, dst); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second Backup to the same path: %v", err)
	}
}

// TestBackupWithAConcurrentWriter is the property that makes `admin backup`
// usable on a running instance: the snapshot is taken as of ONE instant while
// another connection keeps committing, so it is internally consistent — never
// half a transaction — and its counts land between what was there when the
// backup started and what was there when it finished.
func TestBackupWithAConcurrentWriter(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "carol")
	const before = 5
	for range before {
		mkClip(t, d, u, 1000, nil, false)
	}

	stop := make(chan struct{})
	started := make(chan struct{})
	var wg sync.WaitGroup
	var written int
	var mu sync.Mutex
	wg.Add(1)
	go func() {
		defer wg.Done()
		first := true
		for {
			select {
			case <-stop:
				return
			default:
			}
			id := ids.New()
			if _, _, err := d.CreateClipWithJob(ctx, NewClip{
				ID: id, OwnerID: u.ID, Title: "t " + id,
				SourceKey: "clips/" + id + "/original/source.mp4", SourceFilename: "clip.mp4",
				SourceSize: 123, SourceContentType: "video/mp4", SourceETag: "etag-" + id, Now: 1000,
			}); err != nil {
				return // the test is finishing; the assertions below own the verdict
			}
			mu.Lock()
			written++
			mu.Unlock()
			if first {
				close(started)
				first = false
			}
		}
	}()

	// Let the writer get going, then snapshot underneath it.
	<-started
	dst := filepath.Join(t.TempDir(), "hot-backup.db")
	err := d.Backup(ctx, dst)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Backup under load: %v", err)
	}
	mu.Lock()
	after := before + written
	mu.Unlock()

	s, err := Inspect(ctx, dst)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s.Integrity != "ok" || !s.OK() {
		t.Fatalf("hot snapshot not sound: %+v", s)
	}
	if s.Clips < before || s.Clips > after {
		t.Fatalf("snapshot has %d clips, want between %d and %d", s.Clips, before, after)
	}
	// Consistency, not just a count: CreateClipWithJob writes the clip and its
	// job in one transaction, so a snapshot that caught a torn write would
	// show a different number of each.
	if s.Jobs != s.Clips {
		t.Fatalf("snapshot has %d clips but %d jobs — a transaction was torn", s.Clips, s.Jobs)
	}
	if s.Users != 1 {
		t.Fatalf("snapshot has %d users", s.Users)
	}
}

// TestInspectRefusesWhatIsNotABackup: every way a restore-check can go wrong
// comes back as an error, never a panic and never a false "ok".
func TestInspectRefusesWhatIsNotABackup(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte("this is not a database, it is a sentence"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(ctx, junk); err == nil {
		t.Fatal("Inspect accepted a text file")
	}

	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// An empty file is a valid (zero-page) SQLite database, so integrity_check
	// passes; it is rejected for having no schema_migrations table.
	if _, err := Inspect(ctx, empty); err == nil || !strings.Contains(err.Error(), "not a Clippet database") {
		t.Fatalf("Inspect on an empty file: %v", err)
	}

	if _, err := Inspect(ctx, filepath.Join(dir, "does-not-exist.db")); err == nil {
		t.Fatal("Inspect accepted a missing file")
	}
	if _, err := Inspect(ctx, dir); err == nil {
		t.Fatal("Inspect accepted a directory")
	}

	// A truncated backup: the header is intact, the rest is not.
	d := openTest(t)
	u := mkUser(t, d, "carol")
	mkClip(t, d, u, 1000, nil, false)
	full := filepath.Join(dir, "full.db")
	if err := d.Backup(ctx, full); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(dir, "cut.db")
	if err := os.WriteFile(cut, body[:len(body)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Inspect(ctx, cut)
	if err == nil && s.OK() {
		t.Fatalf("Inspect called a truncated backup sound: %+v", s)
	}
}
