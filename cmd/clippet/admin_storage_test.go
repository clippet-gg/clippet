package main

// admin_storage_test.go — `admin storage migrate` bucket to bucket, the
// manifest, the stop file and the byte cap (§20 erratum 335).

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/store"
)

// b2bFixture is an `s3` instance (the source Fake) with a second bucket named
// by MIGRATE_S3_* (the destination Fake) and one ready clip of four objects.
type b2bFixture struct {
	*fsFixture
	dst      *store.Fake
	id       string
	keys     []string
	opened   []config.S3Config
	stateDir string
	// timeout and objectTimeout are handed to the env as they are (§20
	// erratum 350); 0 is the production shape.
	timeout, objectTimeout time.Duration
}

func newB2BFixture(t *testing.T) *b2bFixture {
	t.Helper()
	fx := newFSFixture(t)
	fx.cfg.StorageBackend = config.BackendS3
	fx.cfg.S3.Endpoint = "http://minio:9000"
	fx.cfg.S3.Bucket = "clips"
	fx.cfg.S3.AccessKey = "ak"
	fx.cfg.S3.SecretKey = "sk"
	fx.cfg.MigrateS3 = config.S3Config{
		Endpoint: "http://other:9000", Bucket: "next", AccessKey: "ak2", SecretKey: "sk2", Region: "us-east-1",
	}
	if code := runMigrate(fx.cfg); code != 0 {
		t.Fatal("migrate")
	}
	id, original, rendition, poster := seedOneClip(t, fx.cfg)
	meta := "clips/" + id + "/meta/probe.json"
	fx.fake.Put(original, bytes.Repeat([]byte("o"), 3), "video/mp4")
	fx.fake.Put(rendition, bytes.Repeat([]byte("r"), 4), "video/mp4")
	fx.fake.Put(poster, bytes.Repeat([]byte("p"), 2), "image/jpeg")
	fx.fake.Put(meta, []byte("{}"), "application/json")
	return &b2bFixture{fsFixture: fx, dst: store.NewFake(), id: id,
		keys: []string{original, rendition, poster, meta}, stateDir: t.TempDir()}
}

// run drives the verb with a constructor that hands back the SOURCE Fake for
// the `clips` bucket and the DESTINATION Fake for any other.
func (fx *b2bFixture) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	fx.out.Reset()
	fx.errOut.Reset()
	code := runAdminWith(append([]string{"storage", "migrate"}, args...), fx.cfg, adminEnv{
		stdout: &fx.out, stderr: &fx.errOut,
		openStore: func(c config.Config) (store.Store, error) {
			if c.StorageBackend == config.BackendFS {
				return store.NewFS(c.StorageDir, c.S3.OpTimeout)
			}
			fx.opened = append(fx.opened, c.S3)
			if c.S3.Bucket == "clips" {
				return fx.fake, nil
			}
			return fx.dst, nil
		},
		now:     func() time.Time { return fx.now },
		timeout: fx.timeout, objectTimeout: fx.objectTimeout,
	})
	return code, fx.out.String() + fx.errOut.String()
}

func countOps(calls []string, op string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, op+" ") {
			n++
		}
	}
	return n
}

// TestMigrateBucketToBucket: `--to s3` on an `s3` instance copies into the
// MIGRATE_S3_* bucket, names it without a key, and never touches the source.
func TestMigrateBucketToBucket(t *testing.T) {
	fx := newB2BFixture(t)
	code, out := fx.run(t, "--to", "s3")
	if code != 0 {
		t.Fatalf("bucket to bucket: %d\n%s", code, out)
	}
	for _, want := range []string{
		"migrate: from s3 (clips at http://minio:9000) to s3 (next at http://other:9000)",
		"migrate: plan clips=1 objects=4 bytes=11",
		"migrate: done copied=4 skipped=0 bytes=11",
		"failed=0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"ak2", "sk2"} {
		if strings.Contains(out, secret) {
			t.Fatalf("the output carries a key (%s):\n%s", secret, out)
		}
	}
	if strings.Contains(out, ".clippet-store") {
		t.Fatalf("a bucket destination was given the directory marker:\n%s", out)
	}
	for _, k := range fx.keys {
		if _, ok := fx.dst.Get(k); !ok {
			t.Fatalf("%s did not arrive", k)
		}
	}
	if len(fx.fake.Deletes()) != 0 || countOps(fx.fake.CallsSnapshot(), "PutFile") != 0 {
		t.Fatalf("the source was written to: %v", fx.fake.CallsSnapshot())
	}
	// The destination store was opened with the MIGRATE_S3_* bucket and S3's
	// clock, never with the source's keys.
	last := fx.opened[len(fx.opened)-1]
	if last.Bucket != "next" || last.AccessKey != "ak2" || last.OpTimeout == 0 {
		t.Fatalf("the destination was opened as %+v", last)
	}
}

// TestMigrateBucketToBucketKeyPrefix: a MIGRATE_S3_KEY_PREFIX of its own
// re-roots every key under it at the destination.
func TestMigrateBucketToBucketKeyPrefix(t *testing.T) {
	fx := newB2BFixture(t)
	fx.cfg.MigrateS3.KeyPrefix = "moved/"
	if code, out := fx.run(t, "--to", "s3"); code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	for _, k := range fx.keys {
		if _, ok := fx.dst.Get("moved/" + k); !ok {
			t.Fatalf("moved/%s did not arrive: %v", k, fx.dst.Keys())
		}
	}
}

// TestMigrateBucketToBucketUploadThreads: the destination bucket uploads with
// the instance's STORAGE_UPLOAD_THREADS, as it takes S3's clock (§20 erratum
// 337) — MIGRATE_S3_* has no knob of its own.
func TestMigrateBucketToBucketUploadThreads(t *testing.T) {
	fx := newB2BFixture(t)
	fx.cfg.S3.UploadThreads = 3
	if code, out := fx.run(t, "--to", "s3"); code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	var dst *config.S3Config
	for i := range fx.opened {
		if fx.opened[i].Bucket == "next" {
			dst = &fx.opened[i]
		}
	}
	if dst == nil {
		t.Fatal("the destination was never opened")
	}
	if dst.UploadThreads != 3 {
		t.Fatalf("the destination was opened with UploadThreads=%d, want 3", dst.UploadThreads)
	}
}

// TestMigrateBucketToBucketRefusals: none set is today's refusal; some set
// names the missing ones; `--to fs` ignores all of them.
func TestMigrateBucketToBucketRefusals(t *testing.T) {
	fx := newB2BFixture(t)
	fx.cfg.MigrateS3 = config.S3Config{}
	code, out := fx.run(t, "--to", "s3")
	if code != 2 || !strings.Contains(out, "admin storage migrate: --to s3 is the store this instance already uses (STORAGE_BACKEND=s3); there is nothing to copy") {
		t.Fatalf("none set: %d\n%s", code, out)
	}
	fx.cfg.MigrateS3 = config.S3Config{Endpoint: "http://other:9000", Bucket: "next"}
	code, out = fx.run(t, "--to", "s3")
	want := "admin storage migrate: --to s3 needs MIGRATE_S3_ENDPOINT, MIGRATE_S3_BUCKET, MIGRATE_S3_ACCESS_KEY, MIGRATE_S3_SECRET_KEY in this environment; MIGRATE_S3_ACCESS_KEY, MIGRATE_S3_SECRET_KEY unset"
	if code != 2 || !strings.Contains(out, want) {
		t.Fatalf("some set: %d\n%s", code, out)
	}
	// --to fs on the same half-written set copies into the directory.
	code, out = fx.run(t, "--to", "fs")
	if code != 0 || !strings.Contains(out, "to fs (") || !strings.Contains(out, "done copied=4") {
		t.Fatalf("--to fs with a half MIGRATE_S3_* set: %d\n%s", code, out)
	}
}

// TestMigrateManifestRoundTrip: the first run writes one line per verified
// object; the second skips every one of them WITHOUT hashing (no GetRange on
// either store); a third with no manifest hashes again.
func TestMigrateManifestRoundTrip(t *testing.T) {
	fx := newB2BFixture(t)
	man := filepath.Join(fx.stateDir, "move.manifest")
	code, out := fx.run(t, "--to", "s3", "--manifest", man)
	if code != 0 || !strings.Contains(out, "done copied=4 skipped=0") {
		t.Fatalf("first run: %d\n%s", code, out)
	}
	raw, err := os.ReadFile(man)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("manifest has %d lines:\n%s", len(lines), raw)
	}
	for _, l := range lines {
		f := strings.Split(l, "\t")
		if len(f) != 3 || len(f[2]) != 64 {
			t.Fatalf("manifest line %q", l)
		}
	}
	srcBefore, dstBefore := countOps(fx.fake.CallsSnapshot(), "GetRange"), countOps(fx.dst.CallsSnapshot(), "GetRange")

	code, out = fx.run(t, "--to", "s3", "--manifest", man)
	if code != 0 || !strings.Contains(out, "done copied=0 skipped=4") {
		t.Fatalf("second run: %d\n%s", code, out)
	}
	if n := strings.Count(out, "(in the manifest)"); n != 4 {
		t.Fatalf("%d manifest skips:\n%s", n, out)
	}
	if !strings.Contains(out, "migrate: skip "+fx.keys[0]+" size=3 (in the manifest)\n") {
		t.Fatalf("the skip line's shape:\n%s", out)
	}
	if s, d := countOps(fx.fake.CallsSnapshot(), "GetRange")-srcBefore, countOps(fx.dst.CallsSnapshot(), "GetRange")-dstBefore; s != 0 || d != 0 {
		t.Fatalf("the manifest run hashed: %d source + %d destination GetRange", s, d)
	}
	if raw2, _ := os.ReadFile(man); !bytes.Equal(raw, raw2) {
		t.Fatalf("a manifest skip appended a line:\n%s", raw2)
	}

	// A destination object whose size changed is NOT trusted on the
	// manifest's word: it is hashed, found wrong, and copied again.
	fx.dst.Put(fx.keys[1], []byte("rr"), "video/mp4")
	code, out = fx.run(t, "--to", "s3", "--manifest", man)
	if code != 0 || !strings.Contains(out, "done copied=1 skipped=3") {
		t.Fatalf("a changed destination: %d\n%s", code, out)
	}

	// Without --manifest the run hashes as it always did.
	code, out = fx.run(t, "--to", "s3")
	if code != 0 || !strings.Contains(out, "already at the destination, verified") || strings.Contains(out, "(in the manifest)") {
		t.Fatalf("no manifest: %d\n%s", code, out)
	}

	// A line for a key the plan no longer holds is ignored.
	f, err := os.OpenFile(man, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("clips/Qq1Ww2Ee3Rr4/original/source.mp4\t9\t" + strings.Repeat("a", 64) + "\n")
	_ = f.Close()
	if code, out = fx.run(t, "--to", "s3", "--manifest", man); code != 0 {
		t.Fatalf("a stale line: %d\n%s", code, out)
	}
}

// TestMigrateManifestRefusals: an unreadable or malformed manifest is fatal
// (exit 2) before anything is planned — never a silent trust.
func TestMigrateManifestRefusals(t *testing.T) {
	fx := newB2BFixture(t)
	for name, body := range map[string]string{
		"two fields":  "clips/x/a.mp4\t3\n",
		"bad size":    "clips/x/a.mp4\tthree\t" + strings.Repeat("a", 64) + "\n",
		"bad sha":     "clips/x/a.mp4\t3\tnot-a-hash\n",
		"partial end": "clips/x/a.mp4\t3\t" + strings.Repeat("a", 64) + "\nclips/x/b.mp4\t4\tabc",
	} {
		man := filepath.Join(fx.stateDir, strings.ReplaceAll(name, " ", "-"))
		if err := os.WriteFile(man, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out := fx.run(t, "--to", "s3", "--manifest", man)
		if code != 2 || !strings.Contains(out, "admin storage migrate: --manifest "+man+": ") {
			t.Fatalf("%s: %d\n%s", name, code, out)
		}
		if strings.Contains(out, "migrate: plan") {
			t.Fatalf("%s: planned past a bad manifest:\n%s", name, out)
		}
	}
	// A directory where the file should be is unreadable.
	code, out := fx.run(t, "--to", "s3", "--manifest", fx.stateDir)
	if code != 2 || !strings.Contains(out, "--manifest "+fx.stateDir+": ") {
		t.Fatalf("unreadable: %d\n%s", code, out)
	}
	// A missing file is an empty manifest.
	if code, out := fx.run(t, "--to", "s3", "--manifest", filepath.Join(fx.stateDir, "none")); code != 0 {
		t.Fatalf("missing manifest: %d\n%s", code, out)
	}
}

// TestMigrateStopFileBetweenObjects: a stop file that appears DURING an object
// lets that object finish, then stops before the next: exit 3, nothing
// removed, no marker.
func TestMigrateStopFileBetweenObjects(t *testing.T) {
	fx := newB2BFixture(t)
	stop := filepath.Join(fx.stateDir, "move.stop")
	fx.dst.Hook = func(ctx context.Context, op, key string) error {
		if op == "PutFile" {
			_ = os.WriteFile(stop, nil, 0o600)
		}
		return nil
	}
	code, out := fx.run(t, "--to", "s3", "--stop-file", stop)
	if code != 3 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	first := strconv.FormatInt(planOrderFirstSize(t, fx), 10)
	if !strings.Contains(out, "migrate: stopped reason=stop-file copied=1 skipped=0 bytes="+first+" ("+first+" B) failed=0 remaining=3") {
		t.Fatalf("the stopped line:\n%s", out)
	}
	if strings.Contains(out, "migrate: done") {
		t.Fatalf("a stopped run printed done:\n%s", out)
	}
	if len(fx.dst.Deletes()) != 0 || len(fx.fake.Deletes()) != 0 {
		t.Fatal("a stop removed something")
	}
	// A stop file already there stops before the first object.
	fx.dst.Hook = nil
	code, out = fx.run(t, "--to", "s3", "--stop-file", stop)
	if code != 3 || !strings.Contains(out, "reason=stop-file copied=0 skipped=0 bytes=0 (0 B) failed=0 remaining=4") {
		t.Fatalf("a standing stop file: %d\n%s", code, out)
	}
	// Gone, the run goes on.
	_ = os.Remove(stop)
	if code, out = fx.run(t, "--to", "s3", "--stop-file", stop); code != 0 {
		t.Fatalf("after the stop file went: %d\n%s", code, out)
	}
}

// TestMigrateMaxBytes: the run stops BEFORE the object that would take placed
// + skipped bytes past N; skipped bytes count, so the copy stays a mirror of
// at most N.
func TestMigrateMaxBytes(t *testing.T) {
	fx := newB2BFixture(t)
	// The objects in plan order are 3, 4, 2, 2 bytes (original, rendition,
	// poster, meta — the Fake lists sorted, and keys.go names them so).
	code, out := fx.run(t, "--to", "s3", "--max-bytes", "1")
	if code != 3 || !strings.Contains(out, "migrate: stopped reason=max-bytes copied=0 skipped=0 bytes=0 (0 B) failed=0 remaining=4") {
		t.Fatalf("--max-bytes 1: %d\n%s", code, out)
	}
	if len(fx.dst.Keys()) != 0 {
		t.Fatalf("an object past the cap was placed: %v", fx.dst.Keys())
	}
	// Exactly the first object's bytes: it fits, the next does not.
	first := strconv.FormatInt(planOrderFirstSize(t, fx), 10)
	code, out = fx.run(t, "--to", "s3", "--max-bytes", first)
	if code != 3 || !strings.Contains(out, "reason=max-bytes copied=1 skipped=0") || !strings.Contains(out, "remaining=3") {
		t.Fatalf("--max-bytes %s: %d\n%s", first, code, out)
	}
	// Re-run with a cap one byte under the whole plan (11 bytes): the placed
	// object is SKIPPED and its bytes COUNT, so the run stops before the last
	// object. Were skipped bytes not counted, everything after the first would
	// fit and the run would finish.
	code, out = fx.run(t, "--to", "s3", "--max-bytes", "10")
	if code != 3 || !strings.Contains(out, "reason=max-bytes copied=2 skipped=1") || !strings.Contains(out, "remaining=1") {
		t.Fatalf("skipped bytes do not count: %d\n%s", code, out)
	}
	// A cap at the whole plan completes.
	code, out = fx.run(t, "--to", "s3", "--max-bytes", "11")
	if code != 0 || !strings.Contains(out, "done copied=1 skipped=3") {
		t.Fatalf("--max-bytes 11: %d\n%s", code, out)
	}
	// A negative cap is a usage error.
	if code, out = fx.run(t, "--to", "s3", "--max-bytes", "-1"); code != 2 {
		t.Fatalf("--max-bytes -1: %d\n%s", code, out)
	}
}

// planOrderFirstSize is the size of the object the plan copies first.
func planOrderFirstSize(t *testing.T, fx *b2bFixture) int64 {
	t.Helper()
	objs, err := fx.fake.List(context.Background(), store.ClipPrefix(fx.cfg.S3.KeyPrefix, fx.id))
	if err != nil || len(objs) == 0 {
		t.Fatalf("list: %v", err)
	}
	return objs[0].Size
}

// TestMigrateETagProof: the three verdicts of §20 erratum 337. A destination
// whose ETag is the S3 shape proves a copy and a skip WITHOUT reading the
// destination back; one whose ETag is shaped otherwise (OpaqueETags — a
// provider with no documented rule) falls back to the full read-back, and the
// line says so.
func TestMigrateETagProof(t *testing.T) {
	fx := newB2BFixture(t)
	dstReads := func() int { return countOps(fx.dst.CallsSnapshot(), "GetRange") }
	srcReads := func() int { return countOps(fx.fake.CallsSnapshot(), "GetRange") }

	// (a) a copy proved by the ETag: no read-back.
	code, out := fx.run(t, "--to", "s3")
	if code != 0 || !strings.Contains(out, "done copied=4 skipped=0") {
		t.Fatalf("first run: %d\n%s", code, out)
	}
	for _, k := range fx.keys {
		if lineWithPrefix(out, "migrate: copied "+k+" size=") == "" ||
			!strings.HasSuffix(lineWithPrefix(out, "migrate: copied "+k+" size="), " (etag proof)") {
			t.Fatalf("%s was not copied on the etag proof:\n%s", k, out)
		}
	}
	if n := dstReads(); n != 0 {
		t.Fatalf("an etag-proved copy read the destination back %d time(s)", n)
	}

	// (b) a skip proved by the ETag: the source is read once (sha256 + the
	// expected ETag in one pass), the destination not at all.
	s0, d0 := srcReads(), dstReads()
	code, out = fx.run(t, "--to", "s3")
	if code != 0 || !strings.Contains(out, "done copied=0 skipped=4") {
		t.Fatalf("second run: %d\n%s", code, out)
	}
	if n := strings.Count(out, ") (etag proof)\n"); n != 4 || !strings.Contains(out, "(already at the destination, verified sha256=") {
		t.Fatalf("%d etag-proof skips:\n%s", n, out)
	}
	if s, d := srcReads()-s0, dstReads()-d0; s != 4 || d != 0 {
		t.Fatalf("an etag-proved skip read the source %d and the destination %d time(s); want 4 and 0", s, d)
	}

	// (c) a provider whose ETag is not the S3 shape: every skip is read back.
	fx.dst.OpaqueETags = true
	d0 = dstReads()
	code, out = fx.run(t, "--to", "s3")
	if code != 0 || !strings.Contains(out, "done copied=0 skipped=4") {
		t.Fatalf("opaque skips: %d\n%s", code, out)
	}
	if n := strings.Count(out, "(already at the destination, verified sha256="); n != 4 || strings.Count(out, ") (read back)\n") != 4 || strings.Contains(out, "(etag proof)") {
		t.Fatalf("opaque skips were not read back:\n%s", out)
	}
	if d := dstReads() - d0; d != 4 {
		t.Fatalf("opaque skips read the destination %d time(s), want 4", d)
	}

	// (d) …and a copy is read back too.
	if err := fx.dst.DeleteKey(context.Background(), fx.keys[1]); err != nil {
		t.Fatal(err)
	}
	code, out = fx.run(t, "--to", "s3")
	if code != 0 || !strings.Contains(out, "done copied=1 skipped=3") {
		t.Fatalf("opaque copy: %d\n%s", code, out)
	}
	if l := lineWithPrefix(out, "migrate: copied "+fx.keys[1]+" size=4 sha256="); !strings.HasSuffix(l, " (read back)") {
		t.Fatalf("the opaque copy's line: %q\n%s", l, out)
	}
}

// TestMigrateProgressLines: the copy prints `migrate: progress <key>
// <bytes>/<size>` — on the write leg into a bucket, on the read leg into a
// directory — and with a clock that never moves, exactly the end line.
func TestMigrateProgressLines(t *testing.T) {
	fx := newB2BFixture(t)
	code, out := fx.run(t, "--to", "s3")
	if code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	sizes := map[string]int{fx.keys[0]: 3, fx.keys[1]: 4, fx.keys[2]: 2, fx.keys[3]: 2}
	for k, n := range sizes {
		want := "migrate: progress " + k + " " + strconv.Itoa(n) + "/" + strconv.Itoa(n) + "\n"
		if strings.Count(out, want) != 1 {
			t.Fatalf("want one %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "migrate: progress "); n != 4 {
		t.Fatalf("%d progress lines, want 4:\n%s", n, out)
	}
	// A skip moves no bytes and prints none.
	if _, out = fx.run(t, "--to", "s3"); strings.Contains(out, "migrate: progress ") {
		t.Fatalf("a skip printed progress:\n%s", out)
	}
}

// TestMigrateProgressThrottle: at most one line a second while bytes move,
// never a repeat of the same number, and the end line once.
func TestMigrateProgressThrottle(t *testing.T) {
	var buf bytes.Buffer
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	now := t0
	p := newMigrateProgress(&buf, "clips/k/x.mp4", 1000, func() time.Time { return now })
	step := func(ms int, done int64) {
		now = t0.Add(time.Duration(ms) * time.Millisecond)
		p.update(done, 1000)
	}
	step(100, 10)
	step(900, 90) // under a second since the leg began: nothing
	step(1000, 100)
	step(1500, 150) // under a second since the last line: nothing
	step(1999, 199)
	step(2000, 200)
	step(3500, 200) // a second on, but no new bytes: nothing
	step(3600, 360)
	p.finish()
	p.finish() // the end line once
	want := "migrate: progress clips/k/x.mp4 100/1000\n" +
		"migrate: progress clips/k/x.mp4 200/1000\n" +
		"migrate: progress clips/k/x.mp4 360/1000\n" +
		"migrate: progress clips/k/x.mp4 1000/1000\n"
	if buf.String() != want {
		t.Fatalf("lines:\n%s\nwant:\n%s", buf.String(), want)
	}
	// A leg whose last line was already the whole size prints no end line.
	buf.Reset()
	now = t0
	q := newMigrateProgress(&buf, "k", 5, func() time.Time { return now })
	now = t0.Add(2 * time.Second)
	q.update(5, 5)
	q.finish()
	if buf.String() != "migrate: progress k 5/5\n" {
		t.Fatalf("a finished leg repeated its end line:\n%s", buf.String())
	}
}

// TestMigrateFinishMarkerFallback is §20 erratum 348 (c) seen from the
// migrate's side: after a copy TO a directory the database accounts for every
// copied clip, so `clippet serve` will NOT write the marker there — the line
// for a marker the migrate could not write sends the operator to the file,
// never to a boot that refuses to do it.
func TestMigrateFinishMarkerFallback(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &admin{cfg: config.Config{StorageDir: filepath.Join(blocker, "media")}, env: adminEnv{stdout: &out, stderr: &out}}
	if code := a.migrateFinish(config.BackendFS, 1); code != 0 {
		t.Fatalf("migrateFinish: %d", code)
	}
	got := out.String()
	if !strings.Contains(got, "the store marker could not be written") ||
		!strings.Contains(got, "create an empty file named .clippet-store at its root before `clippet serve` (DEPLOY.md §32.4)") {
		t.Fatalf("the fallback line: %q", got)
	}
	if strings.Contains(got, "writes it at its next boot") {
		t.Fatalf("the fallback line still promises the boot write: %q", got)
	}
}

// TestMigrateOutlivesTheInvocationBudget: the verb is NOT bounded by the admin
// invocation's whole-run deadline (§20 erratum 350). DEPLOY §32.3's 44.9 GiB
// cannot copy in ten minutes; here the budget is 5 ms and four objects take
// 20 ms each to read, and every one of them still arrives verified.
func TestMigrateOutlivesTheInvocationBudget(t *testing.T) {
	fx := newB2BFixture(t)
	fx.timeout = 5 * time.Millisecond
	fx.fake.Hook = func(ctx context.Context, op, key string) error {
		if op != "Download" {
			return nil
		}
		select {
		case <-time.After(20 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	code, out := fx.run(t, "--to", "s3")
	if code != 0 {
		t.Fatalf("a run longer than the invocation budget = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "migrate: done copied=4 skipped=0 bytes=11") || !strings.Contains(out, "failed=0") {
		t.Fatalf("not every object arrived:\n%s", out)
	}
}

// TestMigrateObjectDeadlineFiresOnAHungObject: each object still carries a
// deadline of its own (§20 erratum 350) — a read that never returns fails THAT
// object, and the run goes on to the next one and ends.
func TestMigrateObjectDeadlineFiresOnAHungObject(t *testing.T) {
	fx := newB2BFixture(t)
	fx.objectTimeout = 50 * time.Millisecond
	hung := fx.keys[1]
	fx.fake.Hook = func(ctx context.Context, op, key string) error {
		if op == "Download" && key == hung {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	type result struct {
		code int
		out  string
	}
	done := make(chan result, 1)
	go func() {
		code, out := fx.run(t, "--to", "s3")
		done <- result{code, out}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a hung object held the run: no per-object deadline fired")
	}
	if r.code != 1 {
		t.Fatalf("a hung object = %d, want 1\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "migrate: FAILED "+hung+": reading it from the source") ||
		!strings.Contains(r.out, "copied=3") || !strings.Contains(r.out, "failed=1") {
		t.Fatalf("the hung object did not fail alone:\n%s", r.out)
	}
}
