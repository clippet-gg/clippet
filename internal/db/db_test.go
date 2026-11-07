package db

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"clippet/internal/ids"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "clippet.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d
}

// applySkipped is the ending the four "upgrades an older database" tests share.
//
// Each of them builds its older database as EVERY embedded migration except
// one, because the clip and user helpers write whatever columns the CURRENT
// schema has and a database frozen before that file could not hold a fixture at
// all. Since AUDIT #23 that shape is exactly what `Migrate` refuses: a file
// below max(applied) can only be applied out of order, and no real database is
// ever in this state (it is reached here by applying the files by hand). So the
// tests assert the refusal — which makes each of them a second pin of #23, on a
// database that looks like the mistake it is there to catch — and then apply the
// one file the way Migrate applies each, so the migration itself is still what
// the rest of the test is about.
func applySkipped(t *testing.T, d *DB, version int) {
	t.Helper()
	ctx := context.Background()
	err := d.Migrate(ctx)
	if !errors.Is(err, ErrMigrationOutOfOrder) {
		t.Fatalf("Migrate over a database missing only %04d = %v, want ErrMigrationOutOfOrder (AUDIT #23)", version, err)
	}
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range files {
		if m.version != version {
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		return
	}
	t.Fatalf("no embedded migration with version %04d", version)
}

func mkUser(t *testing.T, d *DB, name string) User {
	t.Helper()
	u, err := d.CreateUser(context.Background(), ids.New(), name, "$2a$12$hash", 1000)
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", name, err)
	}
	return u
}

func mkClip(t *testing.T, d *DB, owner User, now int64, ttl *int64, unlisted bool) (Clip, Job) {
	t.Helper()
	id := ids.New()
	c, j, err := d.CreateClipWithJob(context.Background(), NewClip{
		ID: id, OwnerID: owner.ID, Title: "t " + id, Description: "d", Unlisted: unlisted,
		SourceKey: "clips/" + id + "/original/source.mp4", SourceFilename: "clip.mp4",
		SourceSize: 123, SourceContentType: "video/mp4", SourceETag: "etag-" + id,
		TTLSeconds: ttl, Now: now,
	})
	if err != nil {
		t.Fatalf("CreateClipWithJob: %v", err)
	}
	return c, j
}

func i64(v int64) *int64 { return &v }

func ready() ReadyFields {
	// A transcode always measures the ORIGINAL it rendered, so every ready
	// row a test builds carries its source length too (§20 erratum 68).
	src := int64(6000)
	return ReadyFields{
		RenditionKey: "r", RenditionSize: 10, RenditionETag: "re",
		PosterKey: "p", PosterSize: 5, PosterETag: "pe", PosterWidth: 1280, PosterHeight: 720,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		SourceDurationMS: &src,
	}
}

func TestOpenAndMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "clippet.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate again: %v", err)
	}
	vs, err := d.AppliedVersions(ctx)
	if err != nil || !slices.Equal(vs, embeddedVersions(t)) {
		t.Fatalf("AppliedVersions = %v, %v; want %v", vs, err, embeddedVersions(t))
	}
	if err := d.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Reopen: still one version, migration not re-applied.
	d, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate after reopen: %v", err)
	}
	vs, _ = d.AppliedVersions(ctx)
	if !slices.Equal(vs, embeddedVersions(t)) {
		t.Fatalf("AppliedVersions after reopen = %v", vs)
	}
}

// embeddedVersions is every migration shipped in the binary, in order. The
// migrate tests compare against it rather than a literal so adding a
// migration does not mean editing three assertions (and so a migration that
// is embedded but never applied is a failure).
func embeddedVersions(t *testing.T) []int {
	t.Helper()
	files, err := migrationFiles()
	if err != nil {
		t.Fatalf("migrationFiles: %v", err)
	}
	out := make([]int, len(files))
	for i, m := range files {
		out[i] = m.version
	}
	return out
}

func TestOpenBadDir(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing", "x.db")); err == nil {
		t.Fatal("Open in a missing directory should fail")
	}
}

func TestUsersSessionsTokens(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "Carol")
	if _, err := d.CreateUser(ctx, ids.New(), "carol", "h", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("case-insensitive duplicate username: err = %v, want ErrConflict", err)
	}
	if _, err := d.CreateUser(ctx, u.ID, "other", "h", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate id: err = %v, want ErrConflict", err)
	}
	got, err := d.GetUserByUsername(ctx, "CAROL")
	if err != nil || got.ID != u.ID {
		t.Fatalf("GetUserByUsername(CAROL) = %+v, %v", got, err)
	}
	if _, err := d.GetUserByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	if got, err := d.GetUserByID(ctx, u.ID); err != nil || got.Username != "Carol" {
		t.Fatalf("GetUserByID: %+v, %v", got, err)
	}
	if n, _ := d.CountUsers(ctx); n != 1 {
		t.Fatalf("CountUsers = %d", n)
	}

	// Sessions.
	if err := d.CreateSession(ctx, "h1", u.ID, 100, 200, 0, "ua"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := d.CreateSession(ctx, "h1", u.ID, 100, 200, 0, "ua"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate session: %v", err)
	}
	s, su, err := d.GetSession(ctx, "h1", 150)
	if err != nil || s.UserID != u.ID || su.ID != u.ID || s.UserAgent != "ua" || s.ExpiresAt != 200 {
		t.Fatalf("GetSession = %+v, %+v, %v", s, su, err)
	}
	if _, _, err := d.GetSession(ctx, "h1", 200); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session should be ErrNotFound, got %v", err)
	}
	if err := d.TouchSession(ctx, "h1", 190, 300); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	if s, _, _ = d.GetSession(ctx, "h1", 250); s.ExpiresAt != 300 || s.LastSeenAt != 190 {
		t.Fatalf("after touch: %+v", s)
	}
	if err := d.TouchSession(ctx, "nope", 1, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TouchSession unknown: %v", err)
	}
	_ = d.CreateSession(ctx, "h2", u.ID, 100, 120, 0, "")
	if n, err := d.DeleteExpiredSessions(ctx, 150); err != nil || n != 1 {
		t.Fatalf("DeleteExpiredSessions = %d, %v", n, err)
	}
	if err := d.DeleteSession(ctx, "h1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if err := d.DeleteSession(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteSession twice: %v", err)
	}

	// Tokens.
	tok := APIToken{ID: ids.New(), UserID: u.ID, Name: "sharex", TokenHash: "th1", Prefix: "abcdefgh", Scopes: []string{"upload"}, CreatedAt: 10}
	if err := d.CreateToken(ctx, tok); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if err := d.CreateToken(ctx, tok); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate token: %v", err)
	}
	gt, gu, err := d.GetTokenByHash(ctx, "th1")
	if err != nil || gt.ID != tok.ID || gu.ID != u.ID || len(gt.Scopes) != 1 || gt.Scopes[0] != "upload" {
		t.Fatalf("GetTokenByHash = %+v, %+v, %v", gt, gu, err)
	}
	if err := d.TouchToken(ctx, tok.ID, 42); err != nil {
		t.Fatalf("TouchToken: %v", err)
	}
	list, err := d.ListTokens(ctx, u.ID)
	if err != nil || len(list) != 1 || list[0].LastUsedAt == nil || *list[0].LastUsedAt != 42 {
		t.Fatalf("ListTokens = %+v, %v", list, err)
	}
	if err := d.RevokeToken(ctx, "someone-else", tok.ID, 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RevokeToken foreign: %v", err)
	}
	if err := d.RevokeToken(ctx, u.ID, tok.ID, 50); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if err := d.RevokeToken(ctx, u.ID, tok.ID, 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RevokeToken twice: %v", err)
	}
	if _, _, err := d.GetTokenByHash(ctx, "th1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token still resolvable: %v", err)
	}
	if list, _ := d.ListTokens(ctx, u.ID); len(list) != 0 {
		t.Fatalf("revoked token listed: %+v", list)
	}
}

func TestCreateClipWithJob(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	c, j := mkClip(t, d, u, 5000, i64(86400), false)
	if c.State != ClipQueued || c.SourceETag != "etag-"+c.ID || c.ExpiresAt == nil || *c.ExpiresAt != 5000+86400 ||
		c.TTLSeconds == nil || *c.TTLSeconds != 86400 || c.CreatedAt != 5000 || c.Views != 0 {
		t.Fatalf("clip = %+v", c)
	}
	if j.ClipID != c.ID || j.Kind != KindTranscode || j.State != JobQueued || j.Stage != JobStageQueued || j.RunAfter != 5000 || j.Attempts != 0 {
		t.Fatalf("job = %+v", j)
	}
	if got, err := d.GetClip(ctx, c.ID); err != nil || got.SourceETag != c.SourceETag || got.HasAudio != nil {
		t.Fatalf("GetClip = %+v, %v", got, err)
	}
	if ok, _ := d.ClipExists(ctx, c.ID); !ok {
		t.Fatal("ClipExists false")
	}
	if ok, _ := d.ClipExists(ctx, "nope"); ok {
		t.Fatal("ClipExists true for unknown")
	}
	// never-TTL clip
	c2, _ := mkClip(t, d, u, 5000, nil, true)
	if c2.ExpiresAt != nil || c2.TTLSeconds != nil || !c2.Unlisted {
		t.Fatalf("never clip = %+v", c2)
	}
	// Atomicity: a duplicate id inserts neither row.
	_, _, err := d.CreateClipWithJob(ctx, NewClip{ID: c.ID, OwnerID: u.ID, Title: "dup", SourceKey: "k", SourceFilename: "f", SourceContentType: "video/mp4", SourceETag: "e", Now: 1})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate clip: %v", err)
	}
	q, r, _ := d.QueueStats(ctx)
	if q != 2 || r != 0 {
		t.Fatalf("QueueStats = %d,%d; want 2,0", q, r)
	}
	// Unknown owner → FK error: a plain DB error (httpapi → 503), NOT
	// ErrConflict (which httpapi reports as a 500 id_collision), not silent.
	_, _, err = d.CreateClipWithJob(ctx, NewClip{ID: ids.New(), OwnerID: "ghost", Title: "x", SourceKey: "k", SourceFilename: "f", SourceContentType: "v", SourceETag: "e", Now: 1})
	if err == nil {
		t.Fatal("foreign-key violation should fail")
	}
	if errors.Is(err, ErrConflict) {
		t.Fatalf("foreign-key violation reported as ErrConflict: %v", err)
	}
	if q, r, _ := d.QueueStats(ctx); q != 2 || r != 0 {
		t.Fatalf("QueueStats after FK failure = %d,%d; want 2,0", q, r)
	}
}

func TestIncrementViewsGuard(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	c, _ := mkClip(t, d, u, 1, nil, false)
	if err := d.IncrementViews(ctx, c.ID); err != nil {
		t.Fatalf("IncrementViews: %v", err)
	}
	if err := d.IncrementViews(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if err := d.MarkDeleting(ctx, c.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := d.IncrementViews(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("view on a tombstone: %v, want ErrNotFound", err)
	}
	if gc, _ := d.GetClip(ctx, c.ID); gc.Views != 1 {
		t.Fatalf("views = %d, want 1", gc.Views)
	}
}

// TestApplyMigrationSkipsUnderLock: the per-file transaction re-checks
// schema_migrations, so a process that lost the boot race (the other one
// applied the file between AppliedVersions and BEGIN IMMEDIATE) skips
// instead of failing on CREATE TABLE.
func TestApplyMigrationSkipsUnderLock(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	files, err := migrationFiles()
	if err != nil || len(files) == 0 {
		t.Fatalf("migrationFiles: %v %d", err, len(files))
	}
	if err := d.applyMigration(ctx, files[0]); err != nil {
		t.Fatalf("re-applying an applied migration must be a no-op, got %v", err)
	}
	versions, _ := d.AppliedVersions(ctx)
	if len(versions) != len(files) || versions[0] != files[0].version {
		t.Fatalf("versions = %v", versions)
	}
}

func TestClaimJobCouplingAndExclusivity(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	const n = 40
	want := map[int64]bool{}
	for i := 0; i < n; i++ {
		_, j := mkClip(t, d, u, int64(100+i), nil, false)
		want[j.ID] = true
	}
	// A deleting clip's job must never be claimed.
	del, delJob := mkClip(t, d, u, 50, nil, false)
	if err := d.MarkDeleting(ctx, del.ID, 60); err != nil {
		t.Fatalf("MarkDeleting: %v", err)
	}
	// A job whose run_after is in the future must not be claimed either.
	fut, futJob := mkClip(t, d, u, 10, nil, false)
	if _, err := d.exec(ctx, "UPDATE jobs SET run_after = 999999 WHERE id = ?", futJob.ID); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	claimed := map[int64]int{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, c, ok, err := d.ClaimJob(ctx, 1000)
				if err != nil {
					t.Errorf("ClaimJob: %v", err)
					return
				}
				if !ok {
					return
				}
				if j.State != JobRunning || c.State != ClipProcessing || c.ID != j.ClipID || j.StartedAt == nil {
					t.Errorf("claimed job/clip not coupled: %+v / %+v", j, c)
				}
				mu.Lock()
				claimed[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != n {
		t.Fatalf("claimed %d jobs, want %d", len(claimed), n)
	}
	for id, cnt := range claimed {
		if cnt != 1 || !want[id] {
			t.Fatalf("job %d claimed %d times (expected %v)", id, cnt, want[id])
		}
	}
	if claimed[delJob.ID] != 0 || claimed[futJob.ID] != 0 {
		t.Fatal("deleting or future job was claimed")
	}
	if got, _ := d.GetClip(ctx, fut.ID); got.State != ClipQueued {
		t.Fatalf("future clip state = %s", got.State)
	}
	q, r, _ := d.QueueStats(ctx)
	if q != 1 || r != n {
		t.Fatalf("QueueStats = %d,%d; want 1,%d", q, r, n)
	}
	// Ordering: the oldest job is claimed first.
	_, j := mkClip(t, d, u, 1, nil, false)
	_, j2 := mkClip(t, d, u, 2, nil, false)
	if _, err := d.exec(ctx, "UPDATE jobs SET run_after = 0 WHERE id = ?", futJob.ID); err != nil {
		t.Fatal(err)
	}
	first, _, ok, _ := d.ClaimJob(ctx, 2000)
	if !ok || first.ID != futJob.ID {
		t.Fatalf("first claim = %d, want oldest %d", first.ID, futJob.ID)
	}
	second, _, _, _ := d.ClaimJob(ctx, 2000)
	third, _, _, _ := d.ClaimJob(ctx, 2000)
	if second.ID != j.ID || third.ID != j2.ID {
		t.Fatalf("claim order %d,%d want %d,%d", second.ID, third.ID, j.ID, j2.ID)
	}
}

func claim(t *testing.T, d *DB, now int64) (Job, Clip) {
	t.Helper()
	j, c, ok, err := d.ClaimJob(context.Background(), now)
	if err != nil || !ok {
		t.Fatalf("ClaimJob: ok=%v err=%v", ok, err)
	}
	return j, c
}

func TestFailAttemptTable(t *testing.T) {
	ctx := context.Background()
	lim := Limits{MaxAttempts: 3, MaxTransient: 10}

	t.Run("permanent", func(t *testing.T) {
		d := openTest(t)
		u := mkUser(t, d, "a")
		mkClip(t, d, u, 1, nil, false)
		j, c := claim(t, d, 10)
		out, err := d.FailAttempt(ctx, j.ID, ClaimStamp(j), c.ID, "no_video_stream", FailPermanent, lim, 20)
		if err != nil || out != OutcomeFailed {
			t.Fatalf("out=%v err=%v", out, err)
		}
		gj, _ := d.GetLatestJob(ctx, c.ID)
		gc, _ := d.GetClip(ctx, c.ID)
		if gj.State != JobFailed || gj.Attempts != 0 || gj.TransientFailures != 0 || gj.LastError == nil || *gj.LastError != "no_video_stream" {
			t.Fatalf("job = %+v", gj)
		}
		if gc.State != ClipFailed || gc.FailureReason == nil || *gc.FailureReason != "no_video_stream" {
			t.Fatalf("clip = %+v", gc)
		}
	})

	t.Run("crash", func(t *testing.T) {
		d := openTest(t)
		u := mkUser(t, d, "a")
		mkClip(t, d, u, 1, nil, false)
		wantBackoff := []int64{30, 60}
		var now int64 = 100
		for i := 0; i < 2; i++ {
			j, c := claim(t, d, now)
			out, err := d.FailAttempt(ctx, j.ID, ClaimStamp(j), c.ID, "panic", FailCrash, lim, now)
			if err != nil || out != OutcomeRequeued {
				t.Fatalf("attempt %d: out=%v err=%v", i+1, out, err)
			}
			gj, _ := d.GetLatestJob(ctx, c.ID)
			gc, _ := d.GetClip(ctx, c.ID)
			if gj.State != JobQueued || gj.Attempts != i+1 || gj.TransientFailures != 0 || gj.RunAfter != now+wantBackoff[i] || gj.StartedAt != nil {
				t.Fatalf("attempt %d: job = %+v", i+1, gj)
			}
			if gc.State != ClipQueued {
				t.Fatalf("attempt %d: clip state = %s", i+1, gc.State)
			}
			now = gj.RunAfter
		}
		j, c := claim(t, d, now)
		out, err := d.FailAttempt(ctx, j.ID, ClaimStamp(j), c.ID, "panic", FailCrash, lim, now)
		if err != nil || out != OutcomeFailed {
			t.Fatalf("third: out=%v err=%v", out, err)
		}
		gj, _ := d.GetLatestJob(ctx, c.ID)
		gc, _ := d.GetClip(ctx, c.ID)
		if gj.State != JobFailed || gj.Attempts != 3 || gc.State != ClipFailed || gc.FailureReason == nil || *gc.FailureReason != "panic" {
			t.Fatalf("job = %+v clip = %+v", gj, gc)
		}
	})

	t.Run("transient", func(t *testing.T) {
		d := openTest(t)
		u := mkUser(t, d, "a")
		mkClip(t, d, u, 1, nil, false)
		// 1m·2^(n−1) capped at 30m: 60,120,240,480,960,1800,1800,1800,1800
		wantBackoff := []int64{60, 120, 240, 480, 960, 1800, 1800, 1800, 1800}
		var now int64 = 100
		for i := 0; i < 9; i++ {
			j, c := claim(t, d, now)
			out, err := d.FailAttempt(ctx, j.ID, ClaimStamp(j), c.ID, "storage", FailTransient, lim, now)
			if err != nil || out != OutcomeRequeued {
				t.Fatalf("attempt %d: out=%v err=%v", i+1, out, err)
			}
			gj, _ := d.GetLatestJob(ctx, c.ID)
			if gj.State != JobQueued || gj.TransientFailures != i+1 || gj.Attempts != 0 || gj.RunAfter != now+wantBackoff[i] {
				t.Fatalf("attempt %d: job = %+v (want run_after %d)", i+1, gj, now+wantBackoff[i])
			}
			now = gj.RunAfter
		}
		j, c := claim(t, d, now)
		out, err := d.FailAttempt(ctx, j.ID, ClaimStamp(j), c.ID, "storage", FailTransient, lim, now)
		if err != nil || out != OutcomeFailed {
			t.Fatalf("tenth: out=%v err=%v", out, err)
		}
		gj, _ := d.GetLatestJob(ctx, c.ID)
		gc, _ := d.GetClip(ctx, c.ID)
		if gj.State != JobFailed || gj.TransientFailures != 10 || gj.Attempts != 0 || gc.State != ClipFailed {
			t.Fatalf("job = %+v clip = %+v", gj, gc)
		}
	})

	t.Run("not running", func(t *testing.T) {
		d := openTest(t)
		u := mkUser(t, d, "a")
		c, j := mkClip(t, d, u, 1, nil, false)
		if _, err := d.FailAttempt(ctx, j.ID, ClaimStamp(j), c.ID, "x", FailCrash, lim, 5); !errors.Is(err, ErrStateMismatch) {
			t.Fatalf("queued job: %v, want ErrStateMismatch", err)
		}
		if _, err := d.FailAttempt(ctx, 9999, 0, c.ID, "x", FailCrash, lim, 5); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown job: %v, want ErrNotFound", err)
		}
		// Cancelled by MarkDeleting while running → mismatch, nothing changes.
		claim(t, d, 5)
		_ = d.MarkDeleting(ctx, c.ID, 6)
		if _, err := d.FailAttempt(ctx, j.ID, ClaimStamp(j), c.ID, "x", FailCrash, lim, 7); !errors.Is(err, ErrStateMismatch) {
			t.Fatalf("cancelled job: %v", err)
		}
		gj, _ := d.GetLatestJob(ctx, c.ID)
		gc, _ := d.GetClip(ctx, c.ID)
		if gj.State != JobCancelled || gc.State != ClipDeleting || gj.Attempts != 0 {
			t.Fatalf("job = %+v clip = %+v", gj, gc)
		}
	})

	t.Run("zero limits use defaults", func(t *testing.T) {
		d := openTest(t)
		u := mkUser(t, d, "a")
		mkClip(t, d, u, 1, nil, false)
		j, c := claim(t, d, 5)
		out, err := d.FailAttempt(ctx, j.ID, ClaimStamp(j), c.ID, "x", FailCrash, Limits{}, 5)
		if err != nil || out != OutcomeRequeued {
			t.Fatalf("out=%v err=%v", out, err)
		}
	})
}

func TestBackoff(t *testing.T) {
	cases := []struct {
		kind FailKind
		n    int
		want time.Duration
	}{
		{FailCrash, 1, 30 * time.Second}, {FailCrash, 2, time.Minute}, {FailCrash, 5, 8 * time.Minute},
		{FailCrash, 6, 10 * time.Minute}, {FailCrash, 60, 10 * time.Minute},
		{FailTransient, 1, time.Minute}, {FailTransient, 5, 16 * time.Minute}, {FailTransient, 6, 30 * time.Minute},
		{FailTransient, 100, 30 * time.Minute}, {FailPermanent, 3, 0}, {FailCrash, 0, 30 * time.Second},
	}
	for _, c := range cases {
		if got := Backoff(c.kind, c.n); got != c.want {
			t.Errorf("Backoff(%d, %d) = %s, want %s", c.kind, c.n, got, c.want)
		}
	}
}

func TestReleaseJob(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	// Give it some history so we can see counters are untouched.
	if _, err := d.exec(ctx, "UPDATE jobs SET attempts = 1, transient_failures = 2 WHERE id = ?", j.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.SetJobStage(ctx, j.ID, *j.StartedAt, "encode", 11); err != nil {
		t.Fatalf("SetJobStage: %v", err)
	}
	if err := d.ReleaseJob(ctx, j.ID, ClaimStamp(j), c.ID, 20); err != nil {
		t.Fatalf("ReleaseJob: %v", err)
	}
	gj, _ := d.GetLatestJob(ctx, c.ID)
	gc, _ := d.GetClip(ctx, c.ID)
	if gj.State != JobQueued || gj.Attempts != 1 || gj.TransientFailures != 2 || gj.RunAfter != 20 || gj.Stage != JobStageQueued || gj.StartedAt != nil {
		t.Fatalf("job = %+v", gj)
	}
	if gc.State != ClipQueued {
		t.Fatalf("clip state = %s", gc.State)
	}
	if err := d.ReleaseJob(ctx, j.ID, ClaimStamp(j), c.ID, 21); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("release a queued job: %v", err)
	}
	if err := d.ReleaseJob(ctx, 12345, 0, c.ID, 21); !errors.Is(err, ErrNotFound) {
		t.Fatalf("release unknown job: %v", err)
	}
	if err := d.SetJobStage(ctx, 12345, 1, "x", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetJobStage unknown: %v", err)
	}
}

func TestFinalizeTranscode(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 30)
	if err != nil || !ok {
		t.Fatalf("Finalize: ok=%v err=%v", ok, err)
	}
	gc, _ := d.GetClip(ctx, c.ID)
	gj, _ := d.GetLatestJob(ctx, c.ID)
	if gc.State != ClipReady || gc.RenditionKey == nil || *gc.RenditionKey != "r" || gc.PosterWidth == nil || *gc.PosterWidth != 1280 ||
		gc.HasAudio == nil || !*gc.HasAudio || gc.ReadyAt == nil || *gc.ReadyAt != 30 || gc.DurationMS == nil || *gc.DurationMS != 6000 ||
		gc.FPSNum == nil || *gc.FPSNum != 30 || gc.FPSDen == nil || *gc.FPSDen != 1 || gc.SocialKey != nil {
		t.Fatalf("clip = %+v", gc)
	}
	if gj.State != JobDone || gj.Stage != "done" || gj.FinishedAt == nil {
		t.Fatalf("job = %+v", gj)
	}
	// Second finalize (clip no longer processing) is a no-op returning false.
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 31); err != nil || ok {
		t.Fatalf("re-finalize: ok=%v err=%v", ok, err)
	}
}

func TestFinalizeAfterMarkDeleting(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	mkClip(t, d, u, 1, nil, false)
	j, c := claim(t, d, 10)
	if err := d.MarkDeleting(ctx, c.ID, 20); err != nil {
		t.Fatalf("MarkDeleting: %v", err)
	}
	ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 30)
	if err != nil || ok {
		t.Fatalf("Finalize after MarkDeleting: ok=%v err=%v; want false", ok, err)
	}
	gc, _ := d.GetClip(ctx, c.ID)
	gj, _ := d.GetLatestJob(ctx, c.ID)
	if gc.State != ClipDeleting || gc.RenditionKey != nil || gj.State != JobCancelled {
		t.Fatalf("clip = %+v job = %+v", gc, gj)
	}
	// The run's own cancel arrives after MarkDeleting already ended the row:
	// the attempt-pinned CancelJob reports a row that is not this attempt's any
	// more and changes nothing (§20 erratum 291).
	if err := d.CancelJob(ctx, j.ID, *j.StartedAt, 31); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("CancelJob on a row MarkDeleting ended: %v, want ErrStateMismatch", err)
	}
	if gj, _ = d.GetLatestJob(ctx, c.ID); gj.State != JobCancelled || gj.FinishedAt == nil || *gj.FinishedAt != 20 {
		t.Fatalf("a refused cancel changed the row: %+v", gj)
	}
	// Tombstone → row delete; DeleteClipRow refuses non-deleting rows.
	if err := d.DeleteClipRow(ctx, c.ID); err != nil {
		t.Fatalf("DeleteClipRow: %v", err)
	}
	if _, err := d.GetClip(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if _, err := d.GetLatestJob(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("jobs did not cascade: %v", err)
	}
	if err := d.DeleteClipRow(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if err := d.MarkDeleting(ctx, c.ID, 40); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkDeleting gone: %v", err)
	}
	c2, _ := mkClip(t, d, u, 1, nil, false)
	if err := d.DeleteClipRow(ctx, c2.ID); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("DeleteClipRow on queued: %v", err)
	}
}

func TestReconcileOnStartup(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	lim := Limits{MaxAttempts: 3, MaxTransient: 10}

	// running + checkpoint → released, counters unchanged
	mkClip(t, d, u, 1, nil, false)
	jA, cA := claim(t, d, 10)
	if err := d.SaveCheckpoint(ctx, jA.ID, *jA.StartedAt, `{"rendition_key":"r"}`, 11); err != nil {
		t.Fatal(err)
	}
	if _, err := d.exec(ctx, "UPDATE jobs SET attempts = 2, transient_failures = 3 WHERE id = ?", jA.ID); err != nil {
		t.Fatal(err)
	}
	// running without checkpoint → FailCrash (requeued at attempts 1)
	mkClip(t, d, u, 2, nil, false)
	jB, cB := claim(t, d, 10)
	// running without checkpoint at attempts 2 → FailCrash → failed at cap 3
	mkClip(t, d, u, 3, nil, false)
	jC, cC := claim(t, d, 10)
	if _, err := d.exec(ctx, "UPDATE jobs SET attempts = 2 WHERE id = ?", jC.ID); err != nil {
		t.Fatal(err)
	}
	// clip stuck processing with no running job → queued
	cD, _ := mkClip(t, d, u, 4, nil, false)
	if _, err := d.exec(ctx, "UPDATE clips SET state = 'processing' WHERE id = ?", cD.ID); err != nil {
		t.Fatal(err)
	}
	// untouched queued clip
	cE, _ := mkClip(t, d, u, 5, nil, false)

	released, requeued, failed, err := d.ReconcileOnStartup(ctx, lim, 100)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if released != 1 || requeued != 2 || failed != 1 {
		t.Fatalf("released=%d requeued=%d failed=%d; want 1,2,1", released, requeued, failed)
	}
	gA, _ := d.GetLatestJob(ctx, cA.ID)
	if gA.State != JobQueued || gA.Attempts != 2 || gA.TransientFailures != 3 || gA.RunAfter != 100 || gA.CheckpointJSON == nil {
		t.Fatalf("A = %+v", gA)
	}
	gB, _ := d.GetLatestJob(ctx, cB.ID)
	if gB.State != JobQueued || gB.Attempts != 1 || gB.RunAfter != 130 || gB.LastError == nil || *gB.LastError != "interrupted by restart" {
		t.Fatalf("B = %+v", gB)
	}
	gC, _ := d.GetLatestJob(ctx, cC.ID)
	ccC, _ := d.GetClip(ctx, cC.ID)
	if gC.State != JobFailed || gC.Attempts != 3 || ccC.State != ClipFailed {
		t.Fatalf("C = %+v / %+v", gC, ccC)
	}
	for _, id := range []string{cA.ID, cB.ID, cD.ID, cE.ID} {
		if gc, _ := d.GetClip(ctx, id); gc.State != ClipQueued {
			t.Fatalf("clip %s state = %s, want queued", id, gc.State)
		}
	}
	// Idempotent: nothing running now.
	released, requeued, failed, _ = d.ReconcileOnStartup(ctx, lim, 200)
	if released+requeued+failed != 0 {
		t.Fatalf("second reconcile changed things: %d %d %d", released, requeued, failed)
	}
	_ = jB
}

// TestReconcileOrphanRepair: a clip left processing with no running job
// must come out of boot claimable or retryable, never stuck in queued
// with nothing to claim.
func TestReconcileOrphanRepair(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	lim := Limits{MaxAttempts: 3, MaxTransient: 10}
	setClipProcessing := func(id string) {
		t.Helper()
		if _, err := d.exec(ctx, "UPDATE clips SET state = 'processing' WHERE id = ?", id); err != nil {
			t.Fatal(err)
		}
	}
	setJob := func(id int64, state string, extra string) {
		t.Helper()
		if _, err := d.exec(ctx, "UPDATE jobs SET state = ?"+extra+" WHERE id = ?", state, id); err != nil {
			t.Fatal(err)
		}
	}

	// (1) latest job failed → the clip completes the transition to failed
	// with the job's last_error; the owner can retry.
	cFailed, jFailed := mkClip(t, d, u, 1, nil, false)
	setJob(jFailed.ID, JobFailed, ", last_error = 'ffmpeg exited 1'")
	setClipProcessing(cFailed.ID)
	// (2) latest job failed without last_error → generic reason
	cFailedNoErr, jFailedNoErr := mkClip(t, d, u, 2, nil, false)
	setJob(jFailedNoErr.ID, JobFailed, "")
	setClipProcessing(cFailedNoErr.ID)
	// (3) latest job done (checkpointed) → job requeued, checkpoint kept
	cDone, jDone := mkClip(t, d, u, 3, nil, false)
	setJob(jDone.ID, JobDone, ", checkpoint_json = '{\"k\":1}', finished_at = 9, attempts = 1")
	setClipProcessing(cDone.ID)
	// (4) latest job cancelled → job requeued
	cCancelled, jCancelled := mkClip(t, d, u, 4, nil, false)
	setJob(jCancelled.ID, JobCancelled, ", finished_at = 9")
	setClipProcessing(cCancelled.ID)
	// (5) no transcode job at all → failed "orphaned at boot"
	cNoJob, jNoJob := mkClip(t, d, u, 5, nil, false)
	if _, err := d.exec(ctx, "DELETE FROM jobs WHERE id = ?", jNoJob.ID); err != nil {
		t.Fatal(err)
	}
	setClipProcessing(cNoJob.ID)
	// (6) latest job already queued → clip simply goes back to queued
	cQueued, _ := mkClip(t, d, u, 6, nil, false)
	setClipProcessing(cQueued.ID)

	released, requeued, failed, err := d.ReconcileOnStartup(ctx, lim, 100)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if released != 0 || requeued != 3 || failed != 3 {
		t.Fatalf("released=%d requeued=%d failed=%d; want 0,3,3", released, requeued, failed)
	}

	gc, _ := d.GetClip(ctx, cFailed.ID)
	if gc.State != ClipFailed || gc.FailureReason == nil || *gc.FailureReason != "ffmpeg exited 1" {
		t.Fatalf("failed-job clip = %+v", gc)
	}
	gc, _ = d.GetClip(ctx, cFailedNoErr.ID)
	if gc.State != ClipFailed || gc.FailureReason == nil || *gc.FailureReason != orphanReason {
		t.Fatalf("failed-job (no last_error) clip = %+v", gc)
	}
	gc, _ = d.GetClip(ctx, cNoJob.ID)
	if gc.State != ClipFailed || gc.FailureReason == nil || *gc.FailureReason != orphanReason {
		t.Fatalf("no-job clip = %+v", gc)
	}
	gj, _ := d.GetLatestJob(ctx, cDone.ID)
	if gj.State != JobQueued || gj.Stage != JobStageQueued || gj.RunAfter != 100 || gj.CheckpointJSON == nil ||
		gj.Attempts != 1 || gj.StartedAt != nil || gj.FinishedAt != nil {
		t.Fatalf("done-job requeued = %+v", gj)
	}
	gj, _ = d.GetLatestJob(ctx, cCancelled.ID)
	if gj.State != JobQueued || gj.RunAfter != 100 || gj.FinishedAt != nil {
		t.Fatalf("cancelled-job requeued = %+v", gj)
	}
	for _, id := range []string{cDone.ID, cCancelled.ID, cQueued.ID} {
		if gc, _ := d.GetClip(ctx, id); gc.State != ClipQueued {
			t.Fatalf("clip %s = %s, want queued", id, gc.State)
		}
	}
	// Every requeued clip is claimable: exactly three claims succeed.
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		j, c, ok, err := d.ClaimJob(ctx, 100)
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		seen[c.ID] = true
		if c.State != ClipProcessing || j.State != JobRunning {
			t.Fatalf("claim %d = %+v / %+v", i, j, c)
		}
	}
	if !seen[cDone.ID] || !seen[cCancelled.ID] || !seen[cQueued.ID] {
		t.Fatalf("claimed set = %v", seen)
	}
	if _, _, ok, _ := d.ClaimJob(ctx, 100); ok {
		t.Fatal("a fourth claim succeeded: a failed clip leaked a claimable job")
	}
	// Every failed clip is retryable — including the one with no job.
	for _, id := range []string{cFailed.ID, cFailedNoErr.ID, cNoJob.ID} {
		if err := d.RetryClip(ctx, id, 200); err != nil {
			t.Fatalf("RetryClip(%s): %v", id, err)
		}
		if _, _, ok, err := d.ClaimJob(ctx, 200); err != nil || !ok {
			t.Fatalf("retried %s not claimable: ok=%v err=%v", id, ok, err)
		}
	}
	// Idempotent.
	released, requeued, failed, _ = d.ReconcileOnStartup(ctx, lim, 300)
	if released+requeued+failed != 6 { // the six running jobs claimed above → FailCrash requeues
		t.Fatalf("second reconcile: %d %d %d", released, requeued, failed)
	}
	released, requeued, failed, _ = d.ReconcileOnStartup(ctx, lim, 400)
	if released+requeued+failed != 0 {
		t.Fatalf("third reconcile changed things: %d %d %d", released, requeued, failed)
	}
}

func TestSweepCandidates(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	const now, grace = 10_000, 3600
	mk := func(state string, expires *int64) string {
		c, _ := mkClip(t, d, u, 1, nil, false)
		if _, err := d.exec(ctx, "UPDATE clips SET state = ?, expires_at = ? WHERE id = ?", state, nullInt64(expires), c.ID); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	want := map[string]bool{}
	want[mk(ClipDeleting, nil)] = true                // (a) tombstone, even without expiry
	want[mk(ClipQueued, i64(now))] = true             // (b) at expiry
	want[mk(ClipReady, i64(now-1))] = true            // (b)
	want[mk(ClipFailed, i64(now-5))] = true           // (b)
	want[mk(ClipProcessing, i64(now-grace))] = true   // (c) exactly at grace
	want[mk(ClipProcessing, i64(now-grace-1))] = true // (c) past grace
	skip := []string{
		mk(ClipReady, i64(now+1)),            // not yet expired
		mk(ClipReady, nil),                   // never
		mk(ClipProcessing, i64(now)),         // processing within grace
		mk(ClipProcessing, i64(now-grace+1)), // processing within grace by 1s
		mk(ClipQueued, nil),                  // never
	}
	got, err := d.SweepCandidates(ctx, now, grace, 50)
	if err != nil {
		t.Fatalf("SweepCandidates: %v", err)
	}
	gotSet := map[string]bool{}
	for _, id := range got {
		gotSet[id] = true
	}
	for id := range want {
		if !gotSet[id] {
			t.Errorf("candidate %s missing", id)
		}
	}
	for _, id := range skip {
		if gotSet[id] {
			t.Errorf("clip %s should not be a candidate", id)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d candidates, want %d", len(got), len(want))
	}
	if lim, _ := d.SweepCandidates(ctx, now, grace, 2); len(lim) != 2 {
		t.Errorf("limit not honored: %d", len(lim))
	}
	// ForceExpire moves a never clip into the candidate set.
	id := skip[1]
	if err := d.ForceExpire(ctx, id, now-2*grace); err != nil {
		t.Fatalf("ForceExpire: %v", err)
	}
	got, _ = d.SweepCandidates(ctx, now, grace, 50)
	found := false
	for _, g := range got {
		found = found || g == id
	}
	if !found {
		t.Error("force-expired clip not a candidate")
	}
	if err := d.ForceExpire(ctx, "nope", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("ForceExpire unknown: %v", err)
	}
}

func TestRetryClip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	c, j := mkClip(t, d, u, 1, nil, false)
	if err := d.RetryClip(ctx, c.ID, 5); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("retry a queued clip: %v", err)
	}
	if err := d.RetryClip(ctx, "nope", 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry unknown: %v", err)
	}
	cj, _ := claim(t, d, 10)
	if err := d.SaveCheckpoint(ctx, cj.ID, *cj.StartedAt, "{}", 11); err != nil {
		t.Fatal(err)
	}
	if _, err := d.exec(ctx, "UPDATE jobs SET attempts = 2, transient_failures = 4 WHERE id = ?", j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.FailAttempt(ctx, cj.ID, ClaimStamp(cj), c.ID, "bad", FailPermanent, Limits{}, 12); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryClip(ctx, c.ID, 50); err != nil {
		t.Fatalf("RetryClip: %v", err)
	}
	gc, _ := d.GetClip(ctx, c.ID)
	gj, _ := d.GetLatestJob(ctx, c.ID)
	if gc.State != ClipQueued || gc.FailureReason != nil {
		t.Fatalf("clip = %+v", gc)
	}
	if gj.ID != j.ID || gj.State != JobQueued || gj.Stage != JobStageQueued || gj.Attempts != 0 || gj.TransientFailures != 0 ||
		gj.CheckpointJSON != nil || gj.RunAfter != 50 || gj.LastError != nil || gj.FinishedAt != nil {
		t.Fatalf("job = %+v", gj)
	}
	if _, _, ok, _ := d.ClaimJob(ctx, 50); !ok {
		t.Fatal("retried job not claimable")
	}
}

func TestListClipsVisibilityAndCursor(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	me := mkUser(t, d, "me")
	other := mkUser(t, d, "other")
	var mine, theirs, theirsUnlisted, myUnlisted, deleting Clip
	// Same created_at for a pair to exercise the id tie-break.
	mine, _ = mkClip(t, d, me, 100, nil, false)
	theirs, _ = mkClip(t, d, other, 100, nil, false)
	theirsUnlisted, _ = mkClip(t, d, other, 101, nil, true)
	myUnlisted, _ = mkClip(t, d, me, 102, nil, true)
	deleting, _ = mkClip(t, d, other, 103, nil, false)
	_ = d.MarkDeleting(ctx, deleting.ID, 104)
	for i := 0; i < 5; i++ {
		mkClip(t, d, other, int64(200+i), nil, false)
	}

	all, next, err := d.ListClips(ctx, ListQuery{ViewerID: me.ID, Limit: 100})
	if err != nil || next != "" {
		t.Fatalf("ListClips: next=%q err=%v", next, err)
	}
	seen := map[string]bool{}
	for i, c := range all {
		seen[c.ID] = true
		if i > 0 {
			prev := all[i-1]
			if c.CreatedAt > prev.CreatedAt || (c.CreatedAt == prev.CreatedAt && c.ID < prev.ID) {
				t.Fatalf("not ordered: %+v after %+v", c, prev)
			}
		}
	}
	if !seen[mine.ID] || !seen[theirs.ID] || !seen[myUnlisted.ID] {
		t.Fatalf("missing visible clips: %v", seen)
	}
	if seen[theirsUnlisted.ID] || seen[deleting.ID] {
		t.Fatalf("hidden clip visible: %v", seen)
	}
	if len(all) != 8 {
		t.Fatalf("len = %d, want 8", len(all))
	}

	// Paginate with limit 3 and collect the same set in the same order.
	var paged []Clip
	cursor := ""
	pages := 0
	for {
		page, nc, err := d.ListClips(ctx, ListQuery{ViewerID: me.ID, Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		paged = append(paged, page...)
		pages++
		if nc == "" {
			break
		}
		cursor = nc
	}
	if pages != 3 || len(paged) != len(all) {
		t.Fatalf("pages=%d len=%d", pages, len(paged))
	}
	for i := range all {
		if paged[i].ID != all[i].ID {
			t.Fatalf("page order differs at %d: %s vs %s", i, paged[i].ID, all[i].ID)
		}
	}
	// Garbage cursors → ErrInvalidCursor, never a generic error.
	for _, bad := range []string{"!!!", "bm90YWN1cnNvcg", "MTIzOmJhZC1pZA", "eDphWjNrUTl4QjJtTHA"} {
		if _, _, err := d.ListClips(ctx, ListQuery{ViewerID: me.ID, Cursor: bad}); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("cursor %q: err = %v, want ErrInvalidCursor", bad, err)
		}
	}
	// Default and max limits.
	if page, _, _ := d.ListClips(ctx, ListQuery{ViewerID: me.ID}); len(page) != 8 {
		t.Errorf("default limit page len = %d", len(page))
	}
	if _, _, err := d.ListClips(ctx, ListQuery{ViewerID: me.ID, Limit: 1000}); err != nil {
		t.Errorf("oversized limit: %v", err)
	}
	// Anonymous-style viewer (empty id) sees only listed clips.
	anon, _, _ := d.ListClips(ctx, ListQuery{Limit: 100})
	for _, c := range anon {
		if c.Unlisted {
			t.Errorf("unlisted clip %s visible to empty viewer", c.ID)
		}
	}
}

func TestUpdateClipMeta(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	c, _ := mkClip(t, d, u, 100, i64(86400), false)
	title := "renamed"
	got, err := d.UpdateClipMeta(ctx, c.ID, ClipPatch{Title: &title}, 200)
	if err != nil || got.Title != "renamed" || got.UpdatedAt != 200 || got.ExpiresAt == nil || *got.ExpiresAt != 100+86400 {
		t.Fatalf("title patch: %+v %v", got, err)
	}
	// TTL absent → unchanged (already checked); *nil → never
	var never *int64
	got, err = d.UpdateClipMeta(ctx, c.ID, ClipPatch{TTLSeconds: &never}, 300)
	if err != nil || got.ExpiresAt != nil || got.TTLSeconds != nil {
		t.Fatalf("ttl never: %+v %v", got, err)
	}
	// value → recomputed from now
	seven := i64(7 * 86400)
	unl := true
	desc := "new desc"
	got, err = d.UpdateClipMeta(ctx, c.ID, ClipPatch{TTLSeconds: &seven, Unlisted: &unl, Description: &desc}, 400)
	if err != nil || got.ExpiresAt == nil || *got.ExpiresAt != 400+7*86400 || *got.TTLSeconds != 7*86400 || !got.Unlisted || got.Description != "new desc" {
		t.Fatalf("ttl 7d: %+v %v", got, err)
	}
	if _, err := d.UpdateClipMeta(ctx, "nope", ClipPatch{Title: &title}, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	_ = d.MarkDeleting(ctx, c.ID, 500)
	if _, err := d.UpdateClipMeta(ctx, c.ID, ClipPatch{Title: &title}, 501); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting: %v", err)
	}
	if (ClipPatch{Title: &title, TTLSeconds: &seven}).String() != "patch{title,ttl}" {
		t.Error("ClipPatch.String")
	}
}

// TouchClip is the §8.1 keep-alive: one UPDATE, expires_at = now +
// ttl_seconds, in every state but deleting — and a no-op that changes
// nothing at all (updated_at included) for a clip kept forever.
func TestTouchClip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")

	// A clip with a TTL: the expiry is recomputed from the touch's now, not
	// extended from the old value — a clip touched a day late gets a full
	// TTL, not a day and a TTL.
	c, _ := mkClip(t, d, u, 100, i64(7*86400), false)
	got, err := d.TouchClip(ctx, c.ID, 1000)
	if err != nil || got.ExpiresAt == nil || *got.ExpiresAt != 1000+7*86400 || got.UpdatedAt != 1000 {
		t.Fatalf("touch: %+v %v", got, err)
	}
	if got.TTLSeconds == nil || *got.TTLSeconds != 7*86400 {
		t.Fatalf("touch changed ttl_seconds: %+v", got.TTLSeconds)
	}
	// The rescue erratum 15 exists for: a clip already past its expiry is
	// touched back into the future, no state change.
	if err := d.ForceExpire(ctx, c.ID, 500); err != nil {
		t.Fatal(err)
	}
	got, err = d.TouchClip(ctx, c.ID, 2000)
	if err != nil || got.ExpiresAt == nil || *got.ExpiresAt != 2000+7*86400 || got.State != ClipQueued {
		t.Fatalf("rescue touch: %+v %v", got, err)
	}
	// Every non-deleting state is touchable: a queued or processing clip has
	// a TTL like any other.
	if _, _, ok, err := d.ClaimJob(ctx, 2000); err != nil || !ok {
		t.Fatalf("ClaimJob: %v %v", ok, err)
	}
	got, err = d.TouchClip(ctx, c.ID, 3000)
	if err != nil || got.State != ClipProcessing || got.ExpiresAt == nil || *got.ExpiresAt != 3000+7*86400 {
		t.Fatalf("processing touch: %+v %v", got, err)
	}

	// Kept forever: a no-op that still returns the row. Nothing at all
	// changes — expires_at stays NULL and updated_at is not stamped.
	never, _ := mkClip(t, d, u, 100, nil, false)
	got, err = d.TouchClip(ctx, never.ID, 4000)
	if err != nil || got.ExpiresAt != nil || got.TTLSeconds != nil {
		t.Fatalf("touch never: %+v %v", got, err)
	}
	if got.UpdatedAt != never.UpdatedAt {
		t.Fatalf("touch never stamped updated_at: %d, want %d", got.UpdatedAt, never.UpdatedAt)
	}

	// A touch never resurrects a tombstone, and never invents a clip.
	if _, err := d.TouchClip(ctx, "nope", 5000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := d.TouchClip(ctx, "ZZZZZZZZZZZZ", 5000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown well-formed id: %v", err)
	}
	if err := d.MarkDeleting(ctx, c.ID, 5000); err != nil {
		t.Fatal(err)
	}
	if _, err := d.TouchClip(ctx, c.ID, 6000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting: %v", err)
	}
	after, err := d.GetClip(ctx, c.ID)
	if err != nil || after.State != ClipDeleting || after.ExpiresAt == nil || *after.ExpiresAt != 3000+7*86400 {
		t.Fatalf("a tombstoned clip was modified by a touch: %+v %v", after, err)
	}
}

func TestListAllClipIDsAndKeys(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	c, _ := mkClip(t, d, u, 1, nil, false)
	j, _ := claim(t, d, 2)
	_, _, _ = d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 3)
	c2, _ := mkClip(t, d, u, 1, nil, false)
	rows, err := d.ListAllClipIDsAndKeys(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	byID := map[string]ClipKeysRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if r := byID[c.ID]; r.State != ClipReady || r.RenditionKey == nil || *r.RenditionKey != "r" || r.PosterKey == nil || r.SourceKey != c.SourceKey {
		t.Fatalf("ready row = %+v", r)
	}
	if r := byID[c2.ID]; r.State != ClipQueued || r.RenditionKey != nil || r.PosterKey != nil {
		t.Fatalf("queued row = %+v", r)
	}
}

// TestSingleWriterNoBusy hammers the writer pool from 50 goroutines: with
// _txlock=immediate and MaxOpenConns(1) no SQLITE_BUSY can surface.
func TestSingleWriterNoBusy(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	u := mkUser(t, d, "a")
	c, _ := mkClip(t, d, u, 1, nil, false)
	const writers = 50
	var wg sync.WaitGroup
	errs := make(chan error, writers*2)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := d.IncrementViews(ctx, c.ID); err != nil {
				errs <- err
			}
			title := "t"
			if _, err := d.UpdateClipMeta(ctx, c.ID, ClipPatch{Title: &title}, int64(i)); err != nil {
				errs <- err
			}
			// Interleave reads on the ro pool.
			if _, err := d.GetClip(ctx, c.ID); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write: %v", err)
	}
	got, _ := d.GetClip(ctx, c.ID)
	if got.Views != writers {
		t.Fatalf("views = %d, want %d", got.Views, writers)
	}
}

func TestPrefixed(t *testing.T) {
	if got, want := prefixed("j", "a, b,\nc"), "j.a, j.b,\nj.c"; got != want {
		t.Fatalf("prefixed = %q, want %q", got, want)
	}
}

// TestHasLiveJob is the fact the scratch janitor reads instead of inferring
// who owns $SCRATCH_DIR/{id} from the clip's state (§12, §20 erratum 89):
// `queued` and `running` are live, every terminal state is not, and a READY
// clip is live again the moment somebody asks for a trim.
func TestHasLiveJob(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	owner := mkUser(t, d, "janitor")
	live := func(id string) bool {
		t.Helper()
		got, err := d.HasLiveJob(ctx, id)
		if err != nil {
			t.Fatalf("HasLiveJob(%s): %v", id, err)
		}
		return got
	}

	c, j := mkClip(t, d, owner, 1000, nil, false)
	if !live(c.ID) {
		t.Error("a queued transcode is live")
	}
	if _, _, ok, err := d.ClaimJob(ctx, 1000); err != nil || !ok {
		t.Fatalf("ClaimJob: %v %v", ok, err)
	}
	if !live(c.ID) {
		t.Error("a running transcode is live")
	}
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, c.ID, ready(), false, 1001); err != nil || !ok {
		t.Fatalf("FinalizeTranscode: %v %v", ok, err)
	}
	if live(c.ID) {
		t.Error("a done job is not live; nothing owns the scratch dir any more")
	}

	// The case the janitor exists for: the clip is `ready` and stays ready,
	// but a trim is queued against it and will use that very dir.
	tj, err := d.EnqueueTrim(ctx, c.ID, MarshalTrimParams(TrimParams{StartMS: 0, EndMS: 2000}), 1002)
	if err != nil {
		t.Fatal(err)
	}
	if !live(c.ID) {
		t.Error("a queued trim of a ready clip is live")
	}
	tj, _, ok, err := d.ClaimMediaJob(ctx, 1003)
	if err != nil || !ok {
		t.Fatalf("ClaimMediaJob: %v %v", ok, err)
	}
	if !live(c.ID) {
		t.Error("a running trim of a ready clip is live")
	}
	if _, err := d.FailAttempt(ctx, tj.ID, ClaimStamp(tj), c.ID, "no window", FailPermanent, Limits{}, 1004); err != nil {
		t.Fatal(err)
	}
	if live(c.ID) {
		t.Error("a failed job is not live")
	}

	// A cancelled job is terminal too, and an id with no rows at all is not
	// live (which is what makes a row-less scratch dir removable).
	c2, j2 := mkClip(t, d, owner, 2000, nil, false)
	cj, _, ok, err := d.ClaimJob(ctx, 2000)
	if err != nil || !ok || cj.ID != j2.ID {
		t.Fatalf("ClaimJob: %v %v", ok, err)
	}
	if err := d.CancelJob(ctx, cj.ID, *cj.StartedAt, 2001); err != nil {
		t.Fatal(err)
	}
	if live(c2.ID) {
		t.Error("a cancelled job is not live")
	}
	if live("aZ3kQ9xB2mLp") {
		t.Error("no rows, nothing live")
	}
}
