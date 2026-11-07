package main

// storage_fs_test.go — the CLI half of the filesystem store (ROADMAP #37, §20
// erratum 276): `admin doctor`'s directory questions, `admin stats`'
// reconciled line, `admin orphans` over a directory, and the migrate verb.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// fsFixture is a whole `fs` instance on temp directories: DATA_DIR, SCRATCH_DIR
// and STORAGE_DIR as three siblings (which is what the validator insists on),
// stub ffmpeg binaries, and no S3_* variable anywhere — the state the backend
// exists to make reachable.
type fsFixture struct {
	env                     map[string]string
	cfg                     config.Config
	data, scratch, media    string
	out, errOut             bytes.Buffer
	now                     time.Time
	fake                    *store.Fake
	openedFS, openedS3Count int
	// probe is the ffprobe `admin import` asks about a candidate file (§20
	// erratum 307). nil is production's own answer — build one from
	// FFPROBE_PATH — and the fixtures that drive `import` set one.
	probe func(ctx context.Context, path string) (*transcode.ProbeResult, error)
}

func newFSFixture(t *testing.T) *fsFixture {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	scratch := filepath.Join(root, "scratch")
	media := filepath.Join(root, "media")
	for _, d := range []string{data, scratch, media} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ffmpeg, ffprobe := stubFFmpeg(t, root)
	fx := &fsFixture{
		data: data, scratch: scratch, media: media,
		now:  time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		fake: store.NewFake(),
		env: map[string]string{
			"PUBLIC_BASE_URL": "https://clips.example",
			"STORAGE_BACKEND": "fs",
			"STORAGE_DIR":     media,
			"DATA_DIR":        data,
			"SCRATCH_DIR":     scratch,
			"FFMPEG_PATH":     ffmpeg,
			"FFPROBE_PATH":    ffprobe,
		},
	}
	cfg, err := config.Load(fx.getenv)
	if err != nil {
		t.Fatalf("Load(fs): %v", err)
	}
	fx.cfg = cfg
	return fx
}

func (fx *fsFixture) getenv(k string) string { return fx.env[k] }

// doctor runs the preflight with the REAL store constructor, so the directory
// checks probe the directory rather than a Fake.
func (fx *fsFixture) doctor(t *testing.T) (int, string) {
	t.Helper()
	fx.out.Reset()
	fx.errOut.Reset()
	code := runDoctor(nil, fx.getenv, adminEnv{
		stdout: &fx.out, stderr: &fx.errOut, now: func() time.Time { return fx.now }, getenv: fx.getenv,
	})
	return code, fx.out.String() + fx.errOut.String()
}

// admin runs a CLI verb against the same configuration, with a store
// constructor that dispatches on the backend exactly as store.Open does — so a
// migration test really has two different stores.
func (fx *fsFixture) admin(t *testing.T, args ...string) (int, string) {
	t.Helper()
	fx.out.Reset()
	fx.errOut.Reset()
	code := runAdminWith(args, fx.cfg, adminEnv{
		stdout: &fx.out, stderr: &fx.errOut,
		openStore: func(c config.Config) (store.Store, error) {
			if c.StorageBackend == config.BackendFS {
				fx.openedFS++
				return store.NewFS(c.StorageDir, c.S3.OpTimeout)
			}
			fx.openedS3Count++
			return fx.fake, nil
		},
		now:   func() time.Time { return fx.now },
		probe: fx.probe,
	})
	return code, fx.out.String() + fx.errOut.String()
}

// TestDoctorOnFS: the `store` row asks the directory's questions, the `bucket`
// row is skipped with its reason, and the count is the new one.
func TestDoctorOnFS(t *testing.T) {
	fx := newFSFixture(t)
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	code, out := fx.doctor(t)
	if code != 0 {
		t.Fatalf("doctor on fs: %d\n%s", code, out)
	}
	for _, check := range doctorChecks {
		if v := verdictOf(out, check); v == "" {
			t.Fatalf("check %q did not appear:\n%s", check, out)
		}
	}
	if v := verdictOf(out, "store"); v != vOK {
		t.Fatalf("store = %s\n%s", v, out)
	}
	if v := verdictOf(out, "bucket"); v != vSkip {
		t.Fatalf("bucket on fs = %s (it must SKIP, with a reason)\n%s", v, out)
	}
	if !strings.Contains(out, "doctor:  19 checks, 18 ok, 0 warn, 0 fail, 1 skipped") {
		t.Fatalf("summary line:\n%s", out)
	}
	for _, want := range []string{
		"STORAGE_DIR=" + fx.media,
		"probe file created and removed",
		"free=",
		"SCRATCH_DIR=" + fx.scratch,
		store.MarkerName,
		"no bucket credential at all",
		"there is no bucket",
		"outbound destination",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the fs report does not say %q:\n%s", want, out)
		}
	}
	// The probe cleaned up after itself: doctor is read-only but for the probe.
	entries, err := os.ReadDir(fx.media)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("doctor left something in STORAGE_DIR: %v", entries)
	}
}

// TestDoctorOnFSMissingDirectory: a bind mount whose host side is not there.
func TestDoctorOnFSMissingDirectory(t *testing.T) {
	fx := newFSFixture(t)
	if err := os.Remove(fx.media); err != nil {
		t.Fatal(err)
	}
	_, out := fx.doctor(t)
	if v := verdictOf(out, "store"); v != vWarn {
		t.Fatalf("store with no directory = %s\n%s", v, out)
	}
	if !strings.Contains(out, "BIND MOUNT") {
		t.Fatalf("the warning does not name the likely cause:\n%s", out)
	}
	// A FILE where the directory should be is a failure, not a warning.
	if err := os.WriteFile(fx.media, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := fx.doctor(t)
	if code != 1 || verdictOf(out, "store") != vFail {
		t.Fatalf("store with a file in the way: %d\n%s", code, out)
	}
}

// TestDoctorOnFSMarkerMissing: the directory is there and writable, the
// database accounts for stored clips, and the marker is not there — which is
// what a mis-mounted share looks like from inside the container.
func TestDoctorOnFSMarkerMissing(t *testing.T) {
	fx := newFSFixture(t)
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	seedOneClip(t, fx.cfg)
	_, out := fx.doctor(t)
	if v := verdictOf(out, "store"); v != vWarn {
		t.Fatalf("store with no marker and stored clips = %s\n%s", v, out)
	}
	if !strings.Contains(out, "is MISSING while the database accounts for") {
		t.Fatalf("the warning does not say what it noticed:\n%s", out)
	}
	// With the marker there it is a fact, not a warning.
	if err := store.WriteMarker(fx.media); err != nil {
		t.Fatal(err)
	}
	_, out = fx.doctor(t)
	if v := verdictOf(out, "store"); v != vOK {
		t.Fatalf("store with the marker = %s\n%s", v, out)
	}
}

// TestDoctorOnS3KeepsItsShape: the same fifteen rows on the default backend,
// with `store` a one-liner and `bucket` doing the work.
func TestDoctorOnS3KeepsItsShape(t *testing.T) {
	fx := newDoctorFixture(t)
	cfg, err := config.Load(func(k string) string { return fx.env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(cfg); code != 0 {
		t.Fatal("migrate")
	}
	code, out := fx.run(t)
	if code != 0 {
		t.Fatalf("doctor: %d\n%s", code, out)
	}
	if v := verdictOf(out, "store"); v != vOK {
		t.Fatalf("store on s3 = %s\n%s", v, out)
	}
	if !strings.Contains(out, "STORAGE_BACKEND=s3") || !strings.Contains(out, "STORAGE_DIR is unused") {
		t.Fatalf("the s3 store row:\n%s", out)
	}
	if v := verdictOf(out, "bucket"); v != vOK {
		t.Fatalf("bucket on s3 = %s\n%s", v, out)
	}
}

// seedOneClip inserts one READY clip with non-zero sizes, so StorageUsed is
// non-zero and `admin stats`/`orphans` have something to describe. It is the
// same sequence newAdminFixture uses, minus the Fake.
func seedOneClip(t *testing.T, cfg config.Config) (id, original, rendition, poster string) {
	t.Helper()
	d, err := openForTest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC).Unix()
	u, err := d.CreateUser(ctx, ids.New(), "carol", "h", now)
	if err != nil {
		t.Fatal(err)
	}
	id = ids.New()
	keys := store.KeysFor(cfg.S3.KeyPrefix, id, "mp4")
	c, _, err := d.CreateClipWithJob(ctx, db.NewClip{ID: id, OwnerID: u.ID, Title: "fs clip",
		SourceKey: keys.Original, SourceFilename: "f.mp4", SourceSize: 3,
		SourceContentType: "video/mp4", SourceETag: "e", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	j, cc, ok, err := d.ClaimJob(ctx, now)
	if err != nil || !ok || cc.ID != c.ID {
		t.Fatalf("claim: %v %v", ok, err)
	}
	rendition = store.RenditionKey(cfg.S3.KeyPrefix, id, "r8r8r8r8")
	poster = store.PosterKey(cfg.S3.KeyPrefix, id, "r8r8r8r8")
	sourceMS := int64(6000)
	if ok, _, err := d.FinalizeTranscode(ctx, j.ID, id, db.ReadyFields{
		RenditionKey: rendition, RenditionSize: 4, RenditionETag: "re",
		PosterKey: poster, PosterSize: 2, PosterETag: "pe", PosterWidth: 1280, PosterHeight: 720,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30, FPSDen: 1, HasAudio: true,
		SourceDurationMS: &sourceMS}, false, now); err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
	return id, keys.Original, rendition, poster
}

// TestAdminStatsOnFS: the budget line and the free-disk figure describe ONE
// place, and the listing line says `store:` rather than lying about a bucket.
func TestAdminStatsOnFS(t *testing.T) {
	fx := newFSFixture(t)
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	id, original, rendition, poster := seedOneClip(t, fx.cfg)
	writeFSObject(t, fx.media, original, []byte("source bytes"))
	writeFSObject(t, fx.media, rendition, []byte("rendition bytes"))
	writeFSObject(t, fx.media, poster, []byte("jpg"))

	code, out := fx.admin(t, "stats")
	if code != 0 {
		t.Fatalf("admin stats on fs: %d\n%s", code, out)
	}
	line := lineWithPrefix(out, "storage:")
	if line == "" {
		t.Fatalf("no storage: line\n%s", out)
	}
	for _, want := range []string{"used=", "budget=", "backend=fs", "dir=" + fx.media, "free=", "size="} {
		if !strings.Contains(line, want) {
			t.Fatalf("the storage: line has no %q: %s", want, line)
		}
	}
	store := lineWithPrefix(out, "store:")
	if store == "" || !strings.Contains(store, "objects=3") || !strings.Contains(store, "clip_prefixes=1") {
		t.Fatalf("the store: line: %q\n%s", store, out)
	}
	if lineWithPrefix(out, "bucket:") != "" {
		t.Fatalf("an fs instance printed a bucket: line\n%s", out)
	}
	if !strings.Contains(out, "disk:") {
		t.Fatalf("the disk: line went missing\n%s", out)
	}

	// `admin ls --stat` carries the same label, for the same reason: there is
	// no bucket to name. It is the very row erratum 276(l) called "the one
	// place this feature lied" when it argued `stats` had to change.
	code, out = fx.admin(t, "ls", id, "--stat")
	if code != 0 {
		t.Fatalf("admin ls --stat on fs: %d\n%s", code, out)
	}
	if !strings.Contains(out, "store:     present ") {
		t.Fatalf("admin ls --stat on fs does not use the store: label\n%s", out)
	}
	if strings.Contains(out, "bucket:") {
		t.Fatalf("admin ls --stat on fs printed a bucket: row\n%s", out)
	}
	if !strings.Contains(out, "etag=(not stored)") {
		t.Fatalf("admin ls --stat does not spell the absent ETag\n%s", out)
	}
}

// TestAdminOrphansOnFS: the third report walks List, so a planted stray file
// and a planted row-less prefix are both reported — unchanged behaviour over a
// directory, which is the claim worth proving.
func TestAdminOrphansOnFS(t *testing.T) {
	fx := newFSFixture(t)
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	id, original, rendition, poster := seedOneClip(t, fx.cfg)
	writeFSObject(t, fx.media, original, []byte("source bytes"))
	writeFSObject(t, fx.media, rendition, []byte("rendition bytes"))
	writeFSObject(t, fx.media, poster, []byte("jpg"))
	// `orphans` ages objects against the CLI's clock, which is the fixture's
	// fixed `now`; every planted file is stamped plantedAt, an hour before it
	// (§20 erratum 331). No step reads the wall clock, so no step can see a
	// planted file as younger than --min-age 0 — the flake this test had twice
	// (09-13 and 09-17) when both ends were wall-clock reads.

	// (a) clean.
	code, out := fx.admin(t, "orphans", "--min-age", "0")
	if code != 0 || !strings.Contains(out, "prefixes=0") || !strings.Contains(out, "extra=0") || !strings.Contains(out, "missing=0") {
		t.Fatalf("a clean fs store: %d\n%s", code, out)
	}
	// (b) a stray FILE under a live clip.
	stray := "clips/" + id + "/renditions/main-deadbeef.mp4"
	writeFSObject(t, fx.media, stray, []byte("nobody references this"))
	code, out = fx.admin(t, "orphans", "--min-age", "0")
	if code != 0 || !strings.Contains(out, "extra=1") || !strings.Contains(out, stray) {
		t.Fatalf("a planted stray file: %d\n%s", code, out)
	}
	// (c) a stray PREFIX with no row.
	writeFSObject(t, fx.media, "clips/Zz9Yy8Xx7Ww6/original/source.mp4", []byte("row-less"))
	code, out = fx.admin(t, "orphans", "--min-age", "0")
	if code != 0 || !strings.Contains(out, "prefixes=1") || !strings.Contains(out, "clips/Zz9Yy8Xx7Ww6/") {
		t.Fatalf("a planted orphan prefix: %d\n%s", code, out)
	}
	// (d) --delete removes the stray rendition and the orphan prefix, and the
	// directory is really empty of them afterwards.
	code, out = fx.admin(t, "orphans", "--min-age", "0", "--delete")
	if code != 0 || !strings.Contains(out, "extra_deleted=1") || !strings.Contains(out, "deleted=1") {
		t.Fatalf("orphans --delete: %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(fx.media, filepath.FromSlash(stray))); !os.IsNotExist(err) {
		t.Fatalf("the stray file survived --delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fx.media, "clips", "Zz9Yy8Xx7Ww6")); !os.IsNotExist(err) {
		t.Fatalf("the orphan prefix survived --delete: %v", err)
	}
	// The clip's own objects are untouched.
	code, out = fx.admin(t, "orphans", "--min-age", "0")
	if code != 0 || !strings.Contains(out, "missing=0") {
		t.Fatalf("after the sweep: %d\n%s", code, out)
	}
}

// TestAdminStorageMigrateUsage: the verb refuses everything but a real move.
func TestAdminStorageMigrateUsage(t *testing.T) {
	fx := newFSFixture(t)
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	for _, args := range [][]string{
		{"storage"},
		{"storage", "nonsense"},
		{"storage", "migrate"},
		{"storage", "migrate", "--to", "gcs"},
		{"storage", "migrate", "--to", "fs"}, // this instance already IS fs
		{"storage", "migrate", "--to", "s3", "extra"},
	} {
		code, out := fx.admin(t, args...)
		if code != 2 {
			t.Fatalf("%v = %d\n%s", args, code, out)
		}
	}
	// `--to s3` on an fs instance whose environment carries no bucket names
	// exactly what is missing.
	code, out := fx.admin(t, "storage", "migrate", "--to", "s3")
	if code != 2 || !strings.Contains(out, "S3_BUCKET") {
		t.Fatalf("--to s3 with no bucket configured: %d\n%s", code, out)
	}
}

// TestAdminStorageMigrateToFS is the move ROADMAP #37 is for: a bucket
// instance copies every live clip into a directory, verifies it, writes the
// marker and touches nothing at the source.
func TestAdminStorageMigrateToFS(t *testing.T) {
	fx := newFSFixture(t)
	// The instance is configured for S3 (the Fake) with STORAGE_DIR set, which
	// is the state an operator is in the moment before they flip the switch.
	fx.cfg.StorageBackend = config.BackendS3
	fx.cfg.S3.Endpoint = "http://minio:9000"
	fx.cfg.S3.Bucket = "clips"
	fx.cfg.S3.AccessKey = "ak"
	fx.cfg.S3.SecretKey = "sk"
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	id, original, rendition, poster := seedOneClip(t, fx.cfg)
	fx.fake.Put(original, bytes.Repeat([]byte("o"), 3), "video/mp4")
	fx.fake.Put(rendition, bytes.Repeat([]byte("r"), 4), "video/mp4")
	fx.fake.Put(poster, bytes.Repeat([]byte("p"), 2), "image/jpeg")
	// One object under the clip prefix that no column names: it is under a
	// live clip, so a migration carries it (the destination must be a copy of
	// the prefix, not a copy of the columns).
	fx.fake.Put("clips/"+id+"/meta/probe.json", []byte("{}"), "application/json")
	// And a row-less prefix, which a migration must NOT carry: `admin orphans`
	// is the verb for those.
	fx.fake.Put("clips/Qq1Ww2Ee3Rr4/original/source.mp4", []byte("orphan"), "video/mp4")

	// (a) --dry-run prints the plan and copies nothing.
	code, out := fx.admin(t, "storage", "migrate", "--to", "fs", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run: %d\n%s", code, out)
	}
	for _, want := range []string{
		"migrate: from s3 (clips at http://minio:9000) to fs (" + fx.media + ")",
		"plan clips=1 objects=4 bytes=11",
		"plan " + id + " state=ready objects=4",
		"the source is NEVER written to",
		"--dry-run: nothing was copied",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the plan does not say %q:\n%s", want, out)
		}
	}
	if entries, _ := os.ReadDir(fx.media); len(entries) != 0 {
		t.Fatalf("--dry-run wrote something: %v", entries)
	}

	// (b) the real run.
	code, out = fx.admin(t, "storage", "migrate", "--to", "fs")
	if code != 0 {
		t.Fatalf("migrate: %d\n%s", code, out)
	}
	if !strings.Contains(out, "done copied=4 skipped=0 bytes=11") || !strings.Contains(out, "failed=0") {
		t.Fatalf("the summary:\n%s", out)
	}
	if !strings.Contains(out, store.MarkerName+" written in "+fx.media) {
		t.Fatalf("the marker was not written:\n%s", out)
	}
	// Into a directory: the read leg's progress line (§20 erratum 337), and a
	// directory has no ETag, so every copy is read back.
	if !strings.Contains(out, "migrate: progress "+original+" 3/3\n") ||
		!strings.HasSuffix(lineWithPrefix(out, "migrate: copied "+original+" size=3 "), " (read back)") ||
		strings.Contains(out, "(etag proof)") {
		t.Fatalf("the progress or verdict lines into a directory:\n%s", out)
	}
	if got := readFSObject(t, fx.media, original); string(got) != "ooo" {
		t.Fatalf("the original arrived as %q", got)
	}
	if got := readFSObject(t, fx.media, rendition); string(got) != "rrrr" {
		t.Fatalf("the rendition arrived as %q", got)
	}
	if _, err := os.Stat(filepath.Join(fx.media, "clips", "Qq1Ww2Ee3Rr4")); !os.IsNotExist(err) {
		t.Fatal("the migration carried a row-less prefix; `admin orphans` owns those")
	}
	// The SOURCE is untouched: every key is still in the Fake and nothing was
	// deleted from it.
	if len(fx.fake.Deletes()) != 0 {
		t.Fatalf("the migration deleted from the source: %v", fx.fake.Deletes())
	}
	if _, ok := fx.fake.Get(original); !ok {
		t.Fatal("the original is gone from the source")
	}

	// (c) a second run copies nothing and still verifies what is there.
	code, out = fx.admin(t, "storage", "migrate", "--to", "fs")
	if code != 0 || !strings.Contains(out, "done copied=0 skipped=4") {
		t.Fatalf("the second run: %d\n%s", code, out)
	}
	if !strings.Contains(out, "already at the destination, verified") {
		t.Fatalf("the second run did not re-verify:\n%s", out)
	}

	// (d) a destination object that does NOT match the source is caught by the
	// FULL hash and COPIED AGAIN — the summary's own remedy ("run this again")
	// has to be true, and a same-size corrupt copy used to be a permanent
	// failure no re-run could clear.
	corrupt := filepath.Join(fx.media, filepath.FromSlash(rendition))
	if err := os.WriteFile(corrupt, []byte("xxxx"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = fx.admin(t, "storage", "migrate", "--to", "fs")
	if code != 0 {
		t.Fatalf("after a corrupted destination copy: %d\n%s", code, out)
	}
	if !strings.Contains(out, "does not match the source") || !strings.Contains(out, "copying it again") ||
		!strings.Contains(out, "done copied=1 skipped=3") || !strings.Contains(out, "failed=0") {
		t.Fatalf("a same-size mismatch was not repaired:\n%s", out)
	}
	if got := readFSObject(t, fx.media, rendition); string(got) != "rrrr" {
		t.Fatalf("the corrupt copy was not replaced: %q", got)
	}

	// (e) the same thing corrupted at its TAIL. The first cut of the verifier
	// compared the first 64 KiB and would have called this "verified"; the
	// whole-object hash is the reason it does not.
	big := bytes.Repeat([]byte("m"), 200_000)
	fx.fake.Put(rendition, big, "video/mp4")
	code, out = fx.admin(t, "storage", "migrate", "--to", "fs")
	if code != 0 || !strings.Contains(out, "done copied=1 skipped=3") {
		t.Fatalf("re-copying the grown rendition: %d\n%s", code, out)
	}
	tail := append([]byte(nil), big...)
	tail[len(tail)-1] = 'X' // one byte, far past any sample window, same size
	if err := os.WriteFile(corrupt, tail, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = fx.admin(t, "storage", "migrate", "--to", "fs")
	if code != 0 {
		t.Fatalf("after a TAIL corruption: %d\n%s", code, out)
	}
	if !strings.Contains(out, "does not match the source") || !strings.Contains(out, "copying it again") {
		t.Fatalf("a tail corruption was reported as verified (the head-only check is back):\n%s", out)
	}
	if got := readFSObject(t, fx.media, rendition); !bytes.Equal(got, big) {
		t.Fatalf("the tail-corrupt copy was not replaced (%d bytes, last=%q)", len(got), got[len(got)-1:])
	}
}

// TestMainRoutesTheMigrateVerbThroughLoadForMigrate: the relaxation is
// reachable from `admin storage migrate` and from nothing else (§20 erratum
// 276). `serve` on the same environment must refuse to start.
func TestMainRoutesTheMigrateVerbThroughLoadForMigrate(t *testing.T) {
	if !isStorageMigrate([]string{"storage", "migrate", "--to", "s3"}) {
		t.Fatal("the migrate verb is not recognised")
	}
	for _, args := range [][]string{
		{"storage"}, {"storage", "ls"}, {"stats"}, nil, {"migrate"},
	} {
		if isStorageMigrate(args) {
			t.Fatalf("%v was taken for the migrate verb", args)
		}
	}
	root := t.TempDir()
	media := filepath.Join(root, "media")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	envm := map[string]string{
		"PUBLIC_BASE_URL": "http://localhost:8481",
		"STORAGE_BACKEND": "fs",
		"STORAGE_DIR":     media,
		"DATA_DIR":        filepath.Join(root, "data"),
		"SCRATCH_DIR":     filepath.Join(root, "scratch"),
		"S3_ENDPOINT":     "http://minio:9000",
		"S3_BUCKET":       "clips",
		"S3_ACCESS_KEY":   "ak",
		"S3_SECRET_KEY":   "sk",
	}
	getenv := func(k string) string { return envm[k] }
	var out, errOut bytes.Buffer
	// `migrate` (the schema one) is an ordinary verb and dies on the leftover
	// bucket variables, naming all four.
	if code := run([]string{"migrate"}, getenv, &out, &errOut); code != 2 {
		t.Fatalf("a plain verb with both stores configured: %d\n%s", code, errOut.String())
	}
	for _, name := range config.S3Vars {
		if !strings.Contains(errOut.String(), name) {
			t.Fatalf("the refusal does not name %s:\n%s", name, errOut.String())
		}
	}
	// The migrate verb gets past the loader (it fails later, on there being no
	// database, which is proof enough that the configuration parsed).
	errOut.Reset()
	code := run([]string{"admin", "storage", "migrate", "--to", "s3"}, getenv, &out, &errOut)
	if strings.Contains(errOut.String(), "takes no S3_* variable") {
		t.Fatalf("the migrate verb was refused by the fs rule:\n%s", errOut.String())
	}
	if code == 0 {
		t.Fatalf("the migrate verb succeeded without a database:\n%s", out.String())
	}
}

// --- small helpers ------------------------------------------------------------

// plantedAt is the mtime of every object writeFSObject plants: an hour before
// the fixture's clock (newFSFixture's `now`), whatever the wall clock says.
var plantedAt = time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC)

func writeFSObject(t *testing.T, root, key string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Planted at plantedAt, an hour before the fixture's own clock: `orphans`
	// ages an object against the CLI's clock and skips one younger than
	// --min-age even at 0. It used to be stamped from the WALL clock and the
	// verb run on a fresh time.Now(), and a file written moments before the
	// verb ran read as 21 s YOUNGER than the clock under the race build (the
	// fifteenth gate, 2026-09-17). Both ends now come from the injected clock
	// (§20 erratum 331, the clock-read class): the age is exact.
	if err := os.Chtimes(p, plantedAt, plantedAt); err != nil {
		t.Fatal(err)
	}
}

func readFSObject(t *testing.T, root, key string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("reading %s: %v", key, err)
	}
	return b
}

func lineWithPrefix(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
