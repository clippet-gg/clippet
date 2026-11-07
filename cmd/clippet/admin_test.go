package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/cost"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
)

// adminFixture: a migrated temp database with one user, a ready clip X and a
// queued clip Y, plus a Fake bucket holding their objects, an orphan prefix
// Z (no row) and one foreign key.
type adminFixture struct {
	cfg     config.Config
	fake    *store.Fake
	x, y, z string
	now     time.Time
	out     bytes.Buffer
	errOut  bytes.Buffer
	xPoster string
	// timeout is handed to the env as adminEnv.timeout (§20 erratum 350); 0
	// is the production shape.
	timeout time.Duration
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	cfg, err := config.Load(testEnv(filepath.Join(t.TempDir(), "data")))
	if err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(cfg); code != 0 {
		t.Fatal("migrate")
	}
	fx := &adminFixture{cfg: cfg, fake: store.NewFake(), now: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)}
	d, err := openForTest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	u, err := d.CreateUser(ctx, ids.New(), "carol", "h", 1)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(title string) db.Clip {
		id := ids.New()
		k := store.KeysFor("", id, "mp4")
		c, _, err := d.CreateClipWithJob(ctx, db.NewClip{ID: id, OwnerID: u.ID, Title: title, SourceKey: k.Original,
			SourceFilename: "f.mp4", SourceSize: 3, SourceContentType: "video/mp4", SourceETag: "e", Now: fx.now.Unix()})
		if err != nil {
			t.Fatal(err)
		}
		fx.fake.Put(k.Original, []byte("src"), "video/mp4")
		return c
	}
	x := mk("ready clip")
	j, cc, ok, err := d.ClaimJob(ctx, fx.now.Unix())
	if err != nil || !ok || cc.ID != x.ID {
		t.Fatalf("claim: %v %v", ok, err)
	}
	fx.xPoster = store.PosterKey("", x.ID, "r8r8r8r8")
	rend := store.RenditionKey("", x.ID, "r8r8r8r8")
	sourceMS := int64(6000) // the original this rendition was made from (§20 erratum 68)
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, x.ID, db.ReadyFields{RenditionKey: rend, RenditionSize: 4, RenditionETag: "re",
		PosterKey: fx.xPoster, PosterSize: 2, PosterETag: "pe", PosterWidth: 1280, PosterHeight: 720, Width: 1280, Height: 720,
		DurationMS: 6000, FPSNum: 30, FPSDen: 1, HasAudio: true, SourceDurationMS: &sourceMS}, false, fx.now.Unix()); err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
	fx.fake.Put(rend, []byte("rend"), "video/mp4")
	fx.fake.Put(fx.xPoster, []byte("pj"), "image/jpeg")
	y := mk("queued clip")
	fx.x, fx.y = x.ID, y.ID
	fx.z = ids.New()
	fx.fake.Put(store.OriginalKey("", fx.z, "mp4"), []byte("orphan"), "video/mp4")
	fx.fake.Put(store.RenditionKey("", fx.z, "zzzzzzzz"), []byte("orphan-rend"), "video/mp4")
	fx.fake.Put("clips/not-a-valid-id/x.bin", []byte("?"), "")
	return fx
}

func (fx *adminFixture) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	fx.out.Reset()
	fx.errOut.Reset()
	code := runAdminWith(args, fx.cfg, adminEnv{
		stdout:    &fx.out,
		stderr:    &fx.errOut,
		openStore: func(config.Config) (store.Store, error) { return fx.fake, nil },
		now:       func() time.Time { return fx.now },
		timeout:   fx.timeout,
	})
	return code, fx.out.String() + fx.errOut.String()
}

func (fx *adminFixture) clip(t *testing.T, id string) db.Clip {
	t.Helper()
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, err := d.GetClip(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAdminUsageAndNoDatabase(t *testing.T) {
	cfg, _ := config.Load(testEnv(t.TempDir()))
	var out, errOut bytes.Buffer
	env := adminEnv{stdout: &out, stderr: &errOut, openStore: func(config.Config) (store.Store, error) { return store.NewFake(), nil }, now: time.Now}
	if code := runAdminWith(nil, cfg, env); code != 2 || !strings.Contains(errOut.String(), "usage") {
		t.Fatalf("no args: %d %q", code, errOut.String())
	}
	if code := runAdminWith([]string{"help"}, cfg, env); code != 0 || !strings.Contains(out.String(), "expire <id>") {
		t.Fatalf("help: %d %q", code, out.String())
	}
	errOut.Reset()
	if code := runAdminWith([]string{"bogus"}, cfg, env); code != 2 || !strings.Contains(errOut.String(), "unknown subcommand") {
		t.Fatalf("bogus: %d %q", code, errOut.String())
	}
	// Never creates the database: a data dir without clippet.db is refused.
	errOut.Reset()
	if code := runAdminWith([]string{"stats"}, cfg, env); code != 2 || !strings.Contains(errOut.String(), "no database") {
		t.Fatalf("no db: %d %q", code, errOut.String())
	}
	// runAdmin (the production entry) shares the behaviour; main_test asserts the same via run().
	if code := runAdmin([]string{"stats"}, cfg); code != 2 {
		t.Fatalf("runAdmin without db = %d", code)
	}
}

func TestAdminExpire(t *testing.T) {
	fx := newAdminFixture(t)
	code, out := fx.run(t, "expire", fx.x, "--ago", "2h")
	if code != 0 || !strings.Contains(out, "expired "+fx.x+" expires_at=2026-08-31T10:00:00Z") {
		t.Fatalf("expire --ago: %d %q", code, out)
	}
	if c := fx.clip(t, fx.x); c.ExpiresAt == nil || *c.ExpiresAt != fx.now.Add(-2*time.Hour).Unix() {
		t.Fatalf("expires_at not written: %+v", c.ExpiresAt)
	}
	code, out = fx.run(t, "expire", fx.x, "--at", "2026-01-01T00:00:00Z")
	if code != 0 || !strings.Contains(out, "expires_at=2026-01-01T00:00:00Z") {
		t.Fatalf("expire --at: %d %q", code, out)
	}
	if c := fx.clip(t, fx.x); *c.ExpiresAt != time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("expires_at = %d", *c.ExpiresAt)
	}
	// Default = now.
	code, out = fx.run(t, "expire", fx.y)
	if code != 0 || !strings.Contains(out, "expires_at=2026-08-31T12:00:00Z") {
		t.Fatalf("expire default: %d %q", code, out)
	}
	// The id may follow the flags.
	code, out = fx.run(t, "expire", "--ago", "1h", fx.y)
	if code != 0 || !strings.Contains(out, "expires_at=2026-08-31T11:00:00Z") {
		t.Fatalf("expire with the id last: %d %q", code, out)
	}
	for _, args := range [][]string{
		{"expire"},
		{"expire", fx.x, "--ago", "1h", "--at", "2026-01-01T00:00:00Z"},
		{"expire", fx.x, "--at", "yesterday"},
		{"expire", fx.x, "--ago", "soon"},
		{"expire", fx.x, "extra"},
		{"expire", "--ago", "1h", fx.x, "extra"},
		{"expire", fx.x, "--ago", "1h", "extra"},
	} {
		if code, _ := fx.run(t, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
	if code, out := fx.run(t, "expire", "aZ3kQ9xB2mLp", "--ago", "2h"); code != 1 || !strings.Contains(out, "not found") {
		t.Fatalf("unknown id: %d %q", code, out)
	}
	if code, out := fx.run(t, "expire", "not-an-id"); code != 1 || !strings.Contains(out, "not found") {
		t.Fatalf("invalid id: %d %q", code, out)
	}
}

func TestAdminLs(t *testing.T) {
	fx := newAdminFixture(t)
	code, out := fx.run(t, "ls")
	if code != 0 || !strings.Contains(out, fx.x) || !strings.Contains(out, fx.y) || !strings.Contains(out, "2 clips") {
		t.Fatalf("ls: %d %q", code, out)
	}
	code, out = fx.run(t, "ls", fx.x)
	for _, want := range []string{"clip:      " + fx.x, "state:     ready", "rendition: clips/" + fx.x + "/renditions/main-r8r8r8r8.mp4",
		"poster:    " + fx.xPoster, "dims=1280x720", "media:     1280x720 fps=30/1 duration_ms=6000 has_audio=true",
		"trim:      (none: the whole original, 6000 ms)", "job:       #", "state=done"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls %s: missing %q in\n%s", fx.x, want, out)
		}
	}
	if code != 0 {
		t.Fatalf("ls id: %d", code)
	}
	if code, out := fx.run(t, "ls", fx.y); code != 0 || !strings.Contains(out, "state:     queued") || !strings.Contains(out, "rendition: (none)") {
		t.Fatalf("ls queued: %d %q", code, out)
	}
	code, out = fx.run(t, "ls", fx.x, "--stat")
	if code != 0 || strings.Count(out, "bucket:    present") != 3 {
		t.Fatalf("ls --stat: %d %q", code, out)
	}
	if code, out := fx.run(t, "ls", "--stat", fx.x); code != 0 || strings.Count(out, "bucket:    present") != 3 {
		t.Fatalf("ls --stat with the id last: %d %q", code, out)
	}
	// A missing object shows up as MISSING (and does not fail the command).
	if _, err := fx.fake.DeletePrefix(context.Background(), fx.xPoster); err != nil {
		t.Fatal(err)
	}
	if code, out := fx.run(t, "ls", fx.x, "--stat"); code != 0 || !strings.Contains(out, "MISSING "+fx.xPoster) {
		t.Fatalf("ls --stat missing: %d %q", code, out)
	}
	if code, out := fx.run(t, "ls", "aZ3kQ9xB2mLp"); code != 1 || !strings.Contains(out, "not found") {
		t.Fatalf("ls unknown: %d %q", code, out)
	}
	if code, _ := fx.run(t, "ls", fx.x, "extra"); code != 2 {
		t.Fatalf("ls extra arg: %d", code)
	}
	if code, _ := fx.run(t, "ls", "--bogus"); code != 2 {
		t.Fatalf("ls unknown flag: %d", code)
	}
}

func TestAdminStats(t *testing.T) {
	fx := newAdminFixture(t)
	code, out := fx.run(t, "stats")
	for _, want := range []string{"users:   1", "clips:   2 (queued=1 processing=0 ready=1 failed=0 deleting=0)", "jobs:    queued=1 running=0",
		"bucket:  objects=7 bytes=30 (0.0 MiB) clip_prefixes=3"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats: missing %q in\n%s", want, out)
		}
	}
	if code != 0 {
		t.Fatalf("stats: %d", code)
	}
	if code, out := fx.run(t, "stats", "--no-storage"); code != 0 || strings.Contains(out, "bucket:") {
		t.Fatalf("stats --no-storage: %d %q", code, out)
	}
	fx.fake.FailNext("List", errors.New("bucket down"))
	if code, out := fx.run(t, "stats"); code != 1 || !strings.Contains(out, "bucket:  error: ") || !strings.Contains(out, "users:   1") {
		t.Fatalf("stats with storage error: %d %q", code, out)
	}
}

func TestAdminOrphans(t *testing.T) {
	fx := newAdminFixture(t)
	zPrefix := "clips/" + fx.z + "/"
	// Fresh objects (the Fake stamps LastModified = now) are skipped by default.
	code, out := fx.run(t, "orphans")
	if code != 0 || !strings.Contains(out, "recent  "+zPrefix) || strings.Contains(out, "orphan  ") {
		t.Fatalf("orphans default: %d %q", code, out)
	}
	fx.now = time.Now().Add(2 * time.Hour) // the fixture's clock moves past --min-age
	code, out = fx.run(t, "orphans")
	if code != 0 || !strings.Contains(out, "orphan  "+zPrefix+" objects=2") || !strings.Contains(out, "foreign clips/not-a-valid-id/x.bin") ||
		!strings.Contains(out, "orphans: prefixes=1 deleted=0 recent=0 missing=0 foreign=1") {
		t.Fatalf("orphans: %d %q", code, out)
	}
	if _, ok := fx.fake.Get(store.OriginalKey("", fx.z, "mp4")); !ok {
		t.Fatal("orphans without --delete removed objects")
	}
	// A row whose object is gone is reported, never deleted.
	if _, err := fx.fake.DeletePrefix(context.Background(), fx.xPoster); err != nil {
		t.Fatal(err)
	}
	code, out = fx.run(t, "orphans", "--delete")
	if code != 0 || !strings.Contains(out, "deleted 2 objects") || !strings.Contains(out, "missing "+fx.x+" state=ready key="+fx.xPoster) ||
		!strings.Contains(out, "orphans: prefixes=1 deleted=1 recent=0 missing=1 foreign=1") {
		t.Fatalf("orphans --delete: %d %q", code, out)
	}
	if _, ok := fx.fake.Get(store.OriginalKey("", fx.z, "mp4")); ok {
		t.Fatal("orphan prefix survived --delete")
	}
	for _, key := range []string{store.OriginalKey("", fx.x, "mp4"), store.OriginalKey("", fx.y, "mp4"), "clips/not-a-valid-id/x.bin"} {
		if _, ok := fx.fake.Get(key); !ok {
			t.Fatalf("%s was deleted although a row references it / it is foreign", key)
		}
	}
	if code, _ := fx.run(t, "orphans", "--min-age", "bogus"); code != 2 {
		t.Fatalf("bad flag: %d", code)
	}
	fx.fake.FailNext("List", errors.New("bucket down"))
	if code, _ := fx.run(t, "orphans"); code != 1 {
		t.Fatalf("storage error: %d", code)
	}
}

// TestAdminOrphansDeleteOutlivesTheInvocationBudget: `orphans --delete` is not
// bounded by the invocation's whole-run deadline (§20 erratum 350) — a large
// store's clean-up is one DeletePrefix after another, each under the store's
// own per-operation timeout. Here the budget is 5 ms and the delete takes 20.
func TestAdminOrphansDeleteOutlivesTheInvocationBudget(t *testing.T) {
	fx := newAdminFixture(t)
	fx.now = time.Now().Add(2 * time.Hour)
	fx.timeout = 5 * time.Millisecond
	fx.fake.Hook = func(ctx context.Context, op, key string) error {
		if op != "DeletePrefix" {
			return nil
		}
		select {
		case <-time.After(20 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	code, out := fx.run(t, "orphans", "--delete")
	if code != 0 || !strings.Contains(out, "deleted 2 objects") || !strings.Contains(out, "orphans: prefixes=1 deleted=1") {
		t.Fatalf("orphans --delete past the invocation budget: %d %q", code, out)
	}
}

// --- the key list both subcommands share (a nit sweep, round 6) ----------

// TestClipObjectKeysListsEveryDerivedObject: `ls --stat` and `orphans` ask
// ONE function which keys a row references. They used to build that list
// separately and `orphans` forgot social_key — dead while the column was
// always NULL, and a silent hole the moment Phase 4 filled it. This pins the
// list itself; TestAdminReportsAMissingPreview proves both callers use it.
func TestClipObjectKeysListsEveryDerivedObject(t *testing.T) {
	p := func(s string) *string { return &s }
	c := db.Clip{SourceKey: "src", RenditionKey: p("rend"), PosterKey: p("post"), SocialKey: p("soc"), GIFKey: p("gif")}
	got := clipObjectKeys(c)
	want := []string{"src", "rend", "post", "soc", "gif"}
	if len(got) != len(want) {
		t.Fatalf("clipObjectKeys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("clipObjectKeys = %v, want %v", got, want)
		}
	}
	// A row with nothing derived yet lists only the original.
	if only := clipObjectKeys(db.Clip{SourceKey: "src"}); len(only) != 1 || only[0] != "src" {
		t.Fatalf("a bare row lists %v", only)
	}
	// And the db-side projection agrees, since it IS the same function.
	if row := db.KeysOf(c); len(row.Keys()) != len(want) {
		t.Fatalf("db.KeysOf(...).Keys() = %v", row.Keys())
	}
}

// TestAdminReportsAMissingPreview drives the same drift through both
// subcommands: a clip whose gif_key points at an object the bucket no longer
// holds is MISSING in `ls --stat` and `missing` in `orphans`.
func TestAdminReportsAMissingPreview(t *testing.T) {
	fx := newAdminFixture(t)
	gifKey := store.GIFKey("", fx.x, "g8g8g8g8")
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	job, err := d.EnqueueGIF(ctx, fx.x, fx.now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := d.ClaimMediaJob(ctx, fx.now.Unix()); err != nil || !ok {
		t.Fatalf("ClaimMediaJob: ok=%v err=%v", ok, err)
	}
	c, err := d.GetClip(ctx, fx.x)
	if err != nil {
		t.Fatal(err)
	}
	swap, err := d.SetGIF(ctx, job.ID, fx.x, db.GIFFields{Key: gifKey, Size: 9, ETag: "ge", Width: 480, Height: 270}, *c.RenditionETag, fx.now.Unix())
	if err != nil || !swap.OK {
		t.Fatalf("SetGIF: %+v %v", swap, err)
	}
	d.Close()

	// The object exists: both subcommands are quiet about it.
	fx.fake.Put(gifKey, []byte("GIF89a-x"), "image/gif")
	code, out := fx.run(t, "ls", fx.x, "--stat")
	if code != 0 || !strings.Contains(out, "gif:       "+gifKey) || !strings.Contains(out, "present "+gifKey) {
		t.Fatalf("ls --stat with a preview: %d %q", code, out)
	}
	// Now it does not.
	if _, err := fx.fake.DeletePrefix(ctx, gifKey); err != nil {
		t.Fatal(err)
	}
	if code, out := fx.run(t, "ls", fx.x, "--stat"); code != 0 || !strings.Contains(out, "MISSING "+gifKey) {
		t.Fatalf("ls --stat with a missing preview: %d %q", code, out)
	}
	fx.now = time.Now().Add(2 * time.Hour)
	if code, out := fx.run(t, "orphans"); code != 0 || !strings.Contains(out, "missing "+fx.x+" state=ready key="+gifKey) {
		t.Fatalf("orphans with a missing preview: %d %q", code, out)
	}
}

// --- backup / restore-check ------------------------------------------------

// TestAdminBackupAndRestoreCheck drives the operator's backup story end to
// end: a snapshot of the live database, a validator that reads it back, and
// the two refusals that keep the pair honest (never overwrite, never call a
// non-database sound).
func TestAdminBackupAndRestoreCheck(t *testing.T) {
	fx := newAdminFixture(t)
	dir := t.TempDir()
	dst := filepath.Join(dir, "clippet-backup.db")

	code, out := fx.run(t, "backup", dst)
	if code != 0 || !strings.Contains(out, "backup:  "+dst+" bytes=") || !strings.Contains(out, "schema:  applied=1,2") ||
		!strings.Contains(out, "source:  "+dbPath(fx.cfg)) {
		t.Fatalf("backup: %d %q", code, out)
	}
	// The snapshot holds every bcrypt password hash, every session token hash
	// and every API token hash, so it is written 0600 and says so (§20 erratum
	// 96). Windows has no POSIX mode to assert; the message is asserted there too.
	if !strings.Contains(out, "warning: this file contains password and token hashes") {
		t.Fatalf("backup output does not warn about the hashes: %q", out)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Fatalf("backup mode = %#o, want 0600", got)
		}
	}
	// The snapshot holds the fixture's rows, and restore-check reports them.
	code, out = fx.run(t, "restore-check", dst)
	if code != 0 || !strings.Contains(out, "integrity: ok") || !strings.Contains(out, "rows:      users=1 clips=2 jobs=2") ||
		!strings.Contains(out, "schema:    applied=1,2") {
		t.Fatalf("restore-check: %d %q", code, out)
	}
	// It never overwrites, and it leaves no `.part-…` file behind when it
	// refuses: the destination directory still holds exactly one file.
	if code, out := fx.run(t, "backup", dst); code != 1 || !strings.Contains(out, "refusing to overwrite") {
		t.Fatalf("backup over an existing file: %d %q", code, out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(dst) {
		t.Fatalf("backup directory holds %v", entries)
	}
	// A path whose directory does not exist fails cleanly, and leaves nothing.
	if code, out := fx.run(t, "backup", filepath.Join(dir, "nope", "b.db")); code != 1 || !strings.Contains(out, "admin backup:") {
		t.Fatalf("backup into a missing directory: %d %q", code, out)
	}

	// restore-check refuses everything that is not a Clippet database.
	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte("not a database, just a sentence"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := fx.run(t, "restore-check", junk); code != 1 || !strings.Contains(out, "admin restore-check:") ||
		strings.Contains(out, "integrity: ok") {
		t.Fatalf("restore-check on junk: %d %q", code, out)
	}
	if code, _ := fx.run(t, "restore-check", filepath.Join(dir, "missing.db")); code != 1 {
		t.Fatal("restore-check accepted a missing file")
	}
	for _, args := range [][]string{{"backup"}, {"backup", dst, "extra"}, {"restore-check"}, {"restore-check", dst, "extra"}, {"backup", ""}} {
		if code, _ := fx.run(t, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
	if code, out := fx.run(t, "help"); code != 0 || !strings.Contains(out, "backup <path>") || !strings.Contains(out, "restore-check <path>") {
		t.Fatalf("help does not list the new subcommands: %d %q", code, out)
	}
}

// TestRestoreCheckNeedsNoConfiguration: the subcommand is dispatched before
// config.Load and before any database is opened, which is what lets an
// operator validate a backup in a throwaway container that has no `.env`, no
// bucket and no database of its own (§20 erratum 83).
func TestRestoreCheckNeedsNoConfiguration(t *testing.T) {
	fx := newAdminFixture(t)
	dst := filepath.Join(t.TempDir(), "backup.db")
	if code, out := fx.run(t, "backup", dst); code != 0 {
		t.Fatalf("backup: %d %q", code, out)
	}
	var out, errOut bytes.Buffer
	blank := func(string) string { return "" } // every required variable unset
	if code := run([]string{"admin", "restore-check", dst}, blank, &out, &errOut); code != 0 {
		t.Fatalf("restore-check with no config: %d %q %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "integrity: ok") || !strings.Contains(out.String(), "rows:      users=1 clips=2") {
		t.Fatalf("restore-check output: %q", out.String())
	}
	// Every other admin subcommand still fails on configuration first.
	out.Reset()
	errOut.Reset()
	if code := run([]string{"admin", "stats"}, blank, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "PUBLIC_BASE_URL") {
		t.Fatalf("admin stats with no config: %d %q", code, errOut.String())
	}
}

// TestAdminStatsShowsTheSchema: the applied-migration list is how an operator
// (and E2E step 22) sees which schema a live database is on.
func TestAdminStatsShowsTheSchema(t *testing.T) {
	fx := newAdminFixture(t)
	code, out := fx.run(t, "stats", "--no-storage")
	if code != 0 || !strings.Contains(out, "schema:  applied=1,2") {
		t.Fatalf("stats: %d %q", code, out)
	}
	// The per-kind line is an INVENTORY: a kind missing from it is work an
	// operator cannot see at all.
	for _, kind := range []string{"transcode=", "notify=", "trim=", "gif=", "social=", "replace=", "poster="} {
		if !strings.Contains(out, kind) {
			t.Fatalf("stats does not count %s jobs: %q", strings.TrimSuffix(kind, "="), out)
		}
	}
	// …and the inventory alone would still pass if a counter were wired to the
	// wrong column, so the kinds the fixture is KNOWN not to queue carry an
	// exact count too (N056). newAdminFixture creates two clips and drives one
	// through FinalizeTranscode; no media job of any of these four kinds is
	// ever enqueued, so a non-zero here is a miswiring and not a fixture drift.
	for _, want := range []string{"gif=0", "poster=0", "replace=0", "social=0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats: want %s in %q", want, out)
		}
	}
	// The transcode counter is the other half of the same claim: a number the
	// fixture determines, not a constant.
	if !strings.Contains(out, "transcode=2") {
		t.Fatalf("stats: want transcode=2 (one per clip the fixture created) in %q", out)
	}
}

// `--no-storage` skips the BUCKET LIST and nothing else: the `storage:` line
// is a database sum, which is why DEPLOY tells an operator holding placeholder
// S3 values to run this form. The flag help now says which of the two it means
// (N068); the flag itself is NOT renamed — it is published in DEPLOY and
// DESIGN, and an alias pair would be worse than the mismatch.
func TestAdminStatsNoStorageStillPrintsTheDatabaseSum(t *testing.T) {
	fx := newAdminFixture(t)
	code, out := fx.run(t, "stats", "--no-storage")
	if code != 0 || !strings.Contains(out, "storage: used=") {
		t.Fatalf("stats --no-storage must still print the storage line: %d %q", code, out)
	}
	if strings.Contains(out, "bucket:") {
		t.Fatalf("stats --no-storage listed the bucket: %q", out)
	}
	if code, out = fx.run(t, "stats", "--help"); !strings.Contains(out, "skip the BUCKET listing") {
		t.Fatalf("the flag help must name what it skips: %d %q", code, out)
	}
}

// --- users / users prune ---------------------------------------------------

// TestAdminUsers drives the inventory and the prune through the real
// dispatch: the counts an operator reads, the three guards that make the
// destructive verb hard to type by accident, and the two users it must
// refuse to remove (§20 erratum 86; DEPLOY §8).
func TestAdminUsers(t *testing.T) {
	fx := newAdminFixture(t)
	ctx := context.Background()
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	// carol (the fixture's user) owns clips X and Y. Add three throwaways of
	// the shape an E2E run leaves behind, one of which owns a collection.
	old := fx.now.Add(-48 * time.Hour).Unix()
	for _, name := range []string{"e2e-a", "e2e-b", "e2e-owner"} {
		if _, err := d.CreateUser(ctx, ids.New(), name, "h", old); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := d.GetUserByUsername(ctx, "e2e-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateCollection(ctx, ids.New(), owner.ID, "Best of", old); err != nil {
		t.Fatal(err)
	}
	fresh, err := d.CreateUser(ctx, ids.New(), "e2e-fresh", "h", fx.now.Add(-time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	d.Close()

	code, out := fx.run(t, "users")
	if code != 0 || !strings.Contains(out, "5 user(s)") {
		t.Fatalf("users: %d %q", code, out)
	}
	if !strings.Contains(out, "carol  created=") || !strings.Contains(out, "clips=2 collections=0 tokens=0") {
		t.Fatalf("the owner row must show its clips: %q", out)
	}
	if !strings.Contains(out, "e2e-owner  created=") || !strings.Contains(out, "clips=0 collections=1 tokens=0") {
		t.Fatalf("the collection owner's row: %q", out)
	}

	// The guards. Each of these must change nothing and exit 2.
	for _, args := range [][]string{
		{"users", "prune"}, // no pattern
		{"users", "prune", "--older-than", "24h", "--no-clips"},       // still no pattern
		{"users", "prune", "  ", "--older-than", "24h", "--no-clips"}, // a blank one
		{"users", "prune", "e2e-%", "--no-clips"},                     // no --older-than
		{"users", "prune", "e2e-%", "--older-than", "24h"},            // no --no-clips
		{"users", "prune", "e2e-%", "--older-than", "0s", "--no-clips"},
	} {
		if code, out := fx.run(t, args...); code != 2 || !strings.Contains(out, "usage: clippet admin users prune") {
			t.Fatalf("%v: %d %q", args, code, out)
		}
	}
	if code, out := fx.run(t, "users"); code != 0 || !strings.Contains(out, "5 user(s)") {
		t.Fatalf("a refused prune removed somebody: %d %q", code, out)
	}

	// The real thing: only e2e-a and e2e-b own nothing AND are old enough.
	code, out = fx.run(t, "users", "prune", "e2e-%", "--older-than", "24h", "--no-clips")
	if code != 0 || !strings.Contains(out, "pruned 2 user(s) matching \"e2e-%\"") {
		t.Fatalf("prune: %d %q", code, out)
	}
	if !strings.Contains(out, "e2e-a  created=") || !strings.Contains(out, "e2e-b  created=") {
		t.Fatalf("the prune must report the rows it removed: %q", out)
	}
	code, out = fx.run(t, "users")
	if code != 0 || !strings.Contains(out, "3 user(s)") {
		t.Fatalf("after the prune: %d %q", code, out)
	}
	for _, want := range []string{"carol", "e2e-owner", "e2e-fresh"} {
		if !strings.Contains(out, want+"  created=") {
			t.Errorf("%s was pruned and must not have been: %q", want, out)
		}
	}
	// A pattern that matches nothing is exit 0 and an empty report.
	if code, out := fx.run(t, "users", "prune", "nobody-%", "--older-than", "24h", "--no-clips"); code != 0 ||
		!strings.Contains(out, "pruned 0 user(s)") {
		t.Fatalf("a prune that matches nothing: %d %q", code, out)
	}
	_ = fresh
}

// --- the storage-cost line (ROADMAP #39, §20 erratum 270) -------------------

// TestAdminStatsServedAndCost: `served:` is always printed (zero is the healthy
// answer and is printed as one), the `cost:` line exists only when a price
// does, and the numbers are the ones the arithmetic gives.
func TestAdminStatsServedAndCost(t *testing.T) {
	fx := newAdminFixture(t)
	ctx := context.Background()
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Two days inside the fixture's month and one outside it.
	for _, row := range []struct {
		day   string
		bytes int64
	}{
		{cost.Day(fx.now), 2 * cost.GB},
		{cost.Day(fx.now.AddDate(0, 0, -1)), cost.GB},
		{cost.Day(fx.now.AddDate(0, -2, 0)), 900 * cost.GB},
	} {
		if err := d.AddBytesServed(ctx, row.day, row.bytes); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()

	code, out := fx.run(t, "stats", "--no-storage")
	if code != 0 {
		t.Fatalf("stats: %d %q", code, out)
	}
	for _, want := range []string{
		"served:  today=2000000000 (1.9 GiB) month=3000000000 (2.8 GiB)",
		"bytes sent to viewers", "400-day history",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stats is missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "cost:") {
		t.Errorf("an unpriced instance printed a cost line:\n%s", out)
	}

	// Priced: both halves, each an estimate, with the variables named.
	storage, egress := int64(15_000), int64(10_000)
	fx.cfg.StoragePriceMicros, fx.cfg.EgressPriceMicros = &storage, &egress
	code, out = fx.run(t, "stats", "--no-storage")
	if code != 0 {
		t.Fatalf("stats priced: %d %q", code, out)
	}
	for _, want := range []string{
		"cost:    stored ≈0.00 /month", // the fixture stores a handful of bytes
		"served ≈0.03 this month",      // 3 GB at 0.01
		"an ESTIMATE", "STORAGE_PRICE_PER_GB_MONTH", "EGRESS_PRICE_PER_GB", "never a bill",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the priced stats line is missing %q in\n%s", want, out)
		}
	}

	// One price alone leaves the other half saying so, rather than a 0.00.
	fx.cfg.EgressPriceMicros = nil
	if _, out = fx.run(t, "stats", "--no-storage"); !strings.Contains(out, "served unpriced") {
		t.Errorf("the unpriced half must say so:\n%s", out)
	}

	// A PRESET names the list and the date it was read instead of the two
	// variables (§20 erratum 277). B2's allowance is 3× what is STORED and
	// this fixture stores twelve bytes, so the allowance is thirty-six bytes
	// and the 3 GB month is billed in full at B2's 0.01 — the arithmetic of
	// the allowance itself is a table in internal/cost.
	fx.cfg.StoragePriceMicros, fx.cfg.EgressPriceMicros = nil, nil
	fx.cfg.StoragePricing = "b2"
	_, out = fx.run(t, "stats", "--no-storage")
	for _, want := range []string{"cost:    stored ≈0.00 /month", "served ≈0.03 this month",
		"an ESTIMATE", "B2 list prices as of 2026-09-13 (USD)", "never a bill"} {
		if !strings.Contains(out, want) {
			t.Errorf("the preset stats line is missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "STORAGE_PRICE_PER_GB_MONTH") {
		t.Errorf("a preset instance still attributes the two variables:\n%s", out)
	}
}

// TestAdminStatsQuotaLine (§20 erratum 277): with a budget set, the HEADLINE of
// the storage block is how much room is left and which of the two things the
// budget is. The machine-readable `storage:` line below it keeps the raw
// `used=`/`budget=` pair that four E2E steps and DEPLOY §25 already parse — the
// same human/raw pair `disk:` prints two lines further down.
func TestAdminStatsQuotaLine(t *testing.T) {
	fx := newAdminFixture(t)

	// No budget: no quota line at all, and `storage:` says the feature is off.
	_, out := fx.run(t, "stats", "--no-storage")
	if strings.Contains(out, "quota:") {
		t.Errorf("a quota line with STORAGE_BUDGET_BYTES off:\n%s", out)
	}
	if !strings.Contains(out, "budget=off") {
		t.Errorf("the storage line does not say the budget is off:\n%s", out)
	}

	// Advisory: the line, the percentage, and the promise that nothing is
	// refused — with the clause that names the one way it could be.
	fx.cfg.StorageBudgetBytes = 200 * cost.GB
	_, out = fx.run(t, "stats", "--no-storage")
	for _, want := range []string{"quota:   ", " of 186.3 GiB (0 %)", "advisory (STORAGE_BUDGET_HARD=0",
		"no upload is ever refused because of it"} {
		if !strings.Contains(out, want) {
			t.Errorf("the advisory quota line is missing %q in\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "quota:"), strings.Index(out, "storage:"); i < 0 || j < 0 || i > j {
		t.Errorf("the quota line is not the headline (quota at %d, storage at %d):\n%s", i, j, out)
	}

	// Armed, and over the line: both lines change their verdict, and the
	// `storage:` line keeps the OVER word and the raw pair.
	fx.cfg.StorageBudgetBytes = 1
	fx.cfg.StorageBudgetHard = true
	_, out = fx.run(t, "stats", "--no-storage")
	for _, want := range []string{"HARD (STORAGE_BUDGET_HARD=1: a new clip is refused with 507 at or past the line)",
		"budget=1 ", "OVER (STORAGE_BUDGET_HARD=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the armed quota line is missing %q in\n%s", want, out)
		}
	}

	// Advisory and over: the sentence the E2E greps for is still there, with
	// the new clause after it.
	fx.cfg.StorageBudgetHard = false
	_, out = fx.run(t, "stats", "--no-storage")
	if !strings.Contains(out, "OVER (advisory only: no upload is ever refused, unless STORAGE_BUDGET_HARD=1)") {
		t.Errorf("the advisory OVER verdict:\n%s", out)
	}
}

// --- collections (§20 erratum 275) ------------------------------------------

// TestAdminCollections: the inventory lists every collection with its owner, the
// filter narrows it, and `rm` removes the ROW and not one clip — which is the
// whole reason the verb exists (`users prune` skips an account that owns a
// collection, and nothing else could clear one).
func TestAdminCollections(t *testing.T) {
	fx := newAdminFixture(t)
	ctx := context.Background()
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	carol, err := d.GetUserByUsername(ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	// Registered two days ago, so the prune at the foot of this test can reach
	// it the moment its collection is gone.
	other, err := d.CreateUser(ctx, ids.New(), "e2e_1789149541", "h", fx.now.Add(-48*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	mine, err := d.CreateCollection(ctx, ids.New(), carol.ID, "Best of\tlast night", fx.now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := d.CreateCollection(ctx, ids.New(), other.ID, "left by a run that died", fx.now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	// The clip in it is what must survive the rm.
	if err := d.SetCollectionClips(ctx, mine.ID, carol.ID, []string{fx.x}, fx.now.Unix()); err != nil {
		t.Fatal(err)
	}
	d.Close()

	// Usage: no verb, an unknown verb, and `rm` with no id are all exit 2.
	for _, args := range [][]string{{"collections"}, {"collections", "burn"}, {"collections", "rm"}, {"collections", "ls", "extra"}} {
		if code, out := fx.run(t, args...); code != 2 || !strings.Contains(out, "usage: clippet admin collections ls") {
			t.Fatalf("%v: %d %q", args, code, out)
		}
	}

	code, out := fx.run(t, "collections", "ls")
	if code != 0 || !strings.Contains(out, "2 collections\n") {
		t.Fatalf("collections ls: %d %q", code, out)
	}
	if !strings.Contains(out, mine.ID) || !strings.Contains(out, theirs.ID) {
		t.Fatalf("the listing names neither collection: %q", out)
	}
	// The title is quoted, so a tab in one cannot break the row it is in.
	if !strings.Contains(out, `"Best of\tlast night"`) {
		t.Fatalf("the title is not quoted into its own row: %q", out)
	}
	if !strings.Contains(out, "carol") || !strings.Contains(out, "e2e_1789149541") {
		t.Fatalf("the listing does not name the owners: %q", out)
	}
	// One row per line, and each row carries its owner's own clip count.
	for _, want := range []string{"1     ", "0     "} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing is missing a clip count %q:\n%s", want, out)
		}
	}

	// --owner narrows it, and names the filter on the count line.
	code, out = fx.run(t, "collections", "ls", "--owner", "e2e_1789149541")
	if code != 0 || !strings.Contains(out, "1 collections (owner=e2e_1789149541)") || strings.Contains(out, mine.ID) {
		t.Fatalf("collections ls --owner: %d %q", code, out)
	}
	if code, out := fx.run(t, "collections", "ls", "--owner", "nobody"); code != 1 || !strings.Contains(out, "no such user") {
		t.Fatalf("an unknown owner: %d %q", code, out)
	}

	// An unknown id refuses and removes nothing.
	if code, out := fx.run(t, "collections", "rm", "AAAAAAAAAAAA"); code != 1 || !strings.Contains(out, "AAAAAAAAAAAA: not found") {
		t.Fatalf("rm of an unknown id: %d %q", code, out)
	}
	// So does the right id with the WRONG owner.
	if code, out := fx.run(t, "collections", "rm", theirs.ID, "--owner", "carol"); code != 1 ||
		!strings.Contains(out, "owned by e2e_1789149541, not carol") {
		t.Fatalf("rm with a mismatched owner: %d %q", code, out)
	}
	if code, out := fx.run(t, "collections", "ls"); code != 0 || !strings.Contains(out, "2 collections") {
		t.Fatalf("a refused rm removed something: %d %q", code, out)
	}

	// The real thing: the target is printed first, then removed.
	code, out = fx.run(t, "collections", "rm", theirs.ID, "--owner", "e2e_1789149541")
	if code != 0 || !strings.Contains(out, "collection: "+theirs.ID) || !strings.Contains(out, "owner=e2e_1789149541") {
		t.Fatalf("rm: %d %q", code, out)
	}
	if !strings.Contains(out, "removed collection "+theirs.ID) {
		t.Fatalf("rm said nothing about what it did: %q", out)
	}
	// …and the one with a clip in it, without --owner.
	code, out = fx.run(t, "collections", "rm", mine.ID)
	if code != 0 || !strings.Contains(out, "clips=1") || !strings.Contains(out, "1 clip(s) untouched") {
		t.Fatalf("rm of a collection with a member: %d %q", code, out)
	}
	if code, out := fx.run(t, "collections", "ls"); code != 0 || !strings.Contains(out, "no collections") {
		t.Fatalf("after both rms: %d %q", code, out)
	}
	// THE CLIP IS STILL THERE. This is the assertion the verb lives or dies by.
	if c := fx.clip(t, fx.x); c.ID != fx.x || c.State != db.ClipReady {
		t.Fatalf("removing a collection moved its clip: %+v", c)
	}
	if code, out := fx.run(t, "ls"); code != 0 || !strings.Contains(out, fx.x) {
		t.Fatalf("the clip left the listing: %d %q", code, out)
	}
	// And with no collection left, the account it stranded can be pruned.
	if code, out := fx.run(t, "users", "prune", "e2e_%", "--older-than", "1s", "--no-clips"); code != 0 ||
		!strings.Contains(out, "pruned 1 user(s)") {
		t.Fatalf("the stranded account is still unprunable: %d %q", code, out)
	}
}

// TestBackupPartIsPrivateFromCreationAndGoneOnFailure: the snapshot's `.part-`
// file is 0600 from the moment it exists — before `VACUUM INTO` has written a
// byte into it — and a run whose VACUUM fails for any reason (not only a
// cancel) removes it (§20 erratum 321 (k); D-security row 10). The failure is
// forced by writing a byte into the part before the VACUUM: SQLite writes only
// into a missing or EMPTY file, so the VACUUM refuses it, which is an error
// that is not a cancel.
func TestBackupPartIsPrivateFromCreationAndGoneOnFailure(t *testing.T) {
	fx := newAdminFixture(t)
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dir := t.TempDir()
	var out bytes.Buffer
	a := &admin{cfg: fx.cfg, db: d, ctx: context.Background(), env: adminEnv{stdout: &out, stderr: &out}}

	// The good run: the part exists, empty and 0600, when the VACUUM starts.
	seen := ""
	a.beforeVacuum = func(part string) {
		fi, err := os.Stat(part)
		switch {
		case err != nil:
			seen = "no part before the VACUUM: " + err.Error()
		case fi.Size() != 0:
			seen = fmt.Sprintf("the part already holds %d bytes", fi.Size())
		case runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600:
			seen = fmt.Sprintf("the part was created %#o, want 0600", fi.Mode().Perm())
		default:
			seen = "ok"
		}
	}
	good := filepath.Join(dir, "good.db")
	if _, code := a.writeSnapshot(good); code != 0 {
		t.Fatalf("snapshot failed: %s", out.String())
	}
	if seen != "ok" {
		t.Fatal(seen)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(good); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("the finished snapshot: %v %v", fi, err)
		}
	}

	// The failed run: a VACUUM error that is not a cancel leaves nothing.
	a.beforeVacuum = func(part string) {
		if err := os.WriteFile(part, []byte("x"), 0o600); err != nil {
			t.Error(err)
		}
	}
	out.Reset()
	bad := filepath.Join(dir, "bad.db")
	if _, code := a.writeSnapshot(bad); code == 0 {
		t.Fatal("a VACUUM into a non-empty part succeeded")
	}
	for _, e := range mustReadDir(t, dir) {
		if e.Name() != "good.db" {
			t.Fatalf("a failed snapshot left %s (%s)", e.Name(), strings.TrimSpace(out.String()))
		}
	}
}

// TestAdminUsersOwner: `admin users` names the owner on its row (owner=yes,
// the LAST column), the first account the fixture made is it, and `admin
// users owner <name>` moves the role — refusing an unknown name and a missing
// one, and changing nothing then (§20 erratum 349).
func TestAdminUsersOwner(t *testing.T) {
	fx := newAdminFixture(t)
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser(context.Background(), ids.New(), "friend", "h", 2); err != nil {
		t.Fatal(err)
	}
	d.Close()
	row := func(out, name string) string {
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) > 1 && f[1] == name {
				return l
			}
		}
		return ""
	}
	code, out := fx.run(t, "users")
	if code != 0 || !strings.HasSuffix(row(out, "carol"), " discord=- owner=yes") || !strings.HasSuffix(row(out, "friend"), " owner=no") {
		t.Fatalf("users: %d %q", code, out)
	}
	if code, out := fx.run(t, "users", "owner", "Friend"); code != 0 || !strings.Contains(out, "owner: friend is this instance's owner") {
		t.Fatalf("users owner friend: %d %q", code, out)
	}
	_, out = fx.run(t, "users")
	if !strings.HasSuffix(row(out, "carol"), " owner=no") || !strings.HasSuffix(row(out, "friend"), " owner=yes") {
		t.Fatalf("after the move: %q", out)
	}
	if code, out := fx.run(t, "users", "owner", "nobody"); code != 1 || !strings.Contains(out, `no account named "nobody"`) {
		t.Fatalf("users owner nobody: %d %q", code, out)
	}
	if code, out := fx.run(t, "users", "owner"); code != 2 || !strings.Contains(out, "usage: clippet admin users owner") {
		t.Fatalf("users owner with no name: %d %q", code, out)
	}
	if _, out := fx.run(t, "users"); !strings.HasSuffix(row(out, "friend"), " owner=yes") {
		t.Fatalf("a refused move changed the owner: %q", out)
	}
}
