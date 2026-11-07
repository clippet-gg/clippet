package main

// admin_objects_test.go — `clippet admin objects <id>` (§20 erratum 281).

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/ids"
	"clippet/internal/store"
)

// TestAdminObjects: the listing, the count, an empty prefix, a bad id, and a
// store that fails.
func TestAdminObjects(t *testing.T) {
	fx := newAdminFixture(t)
	// The ready clip X has three objects: its original, its rendition, its poster.
	code, out := fx.run(t, "objects", fx.x)
	if code != 0 {
		t.Fatalf("objects %s: %d\n%s", fx.x, code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 || lines[3] != "3 object(s)" {
		t.Fatalf("objects %s printed %d lines, want 3 + a count:\n%s", fx.x, len(lines), out)
	}
	// One `<key> <size> <modified RFC3339>` line per object, sorted by key.
	prev := ""
	for _, ln := range lines[:3] {
		f := strings.Fields(ln)
		if len(f) != 3 {
			t.Fatalf("object line %q has %d fields, want 3", ln, len(f))
		}
		if !strings.HasPrefix(f[0], "clips/"+fx.x+"/") {
			t.Fatalf("object line %q is not under the clip prefix", ln)
		}
		if f[0] <= prev {
			t.Fatalf("the listing is not sorted by key: %q after %q", f[0], prev)
		}
		prev = f[0]
		if _, err := time.Parse(time.RFC3339, f[2]); err != nil {
			t.Fatalf("object line %q: %v", ln, err)
		}
	}
	// --count is the same number and nothing else, before OR after the id.
	for _, args := range [][]string{{"objects", fx.x, "--count"}, {"objects", "--count", fx.x}} {
		code, out := fx.run(t, args...)
		if code != 0 || strings.TrimSpace(out) != "3" {
			t.Fatalf("%v: %d %q", args, code, out)
		}
	}
	// An id with no objects at all: `0 object(s)` / `0`, and exit 0. A prefix
	// that is empty is an ANSWER, not a failure.
	empty := ids.New()
	if code, out := fx.run(t, "objects", empty); code != 0 || strings.TrimSpace(out) != "0 object(s)" {
		t.Fatalf("objects on an empty prefix: %d %q", code, out)
	}
	if code, out := fx.run(t, "objects", empty, "--count"); code != 0 || strings.TrimSpace(out) != "0" {
		t.Fatalf("objects --count on an empty prefix: %d %q", code, out)
	}
	// A malformed id is a usage error.
	for _, args := range [][]string{
		{"objects"},
		{"objects", "not-an-id"},
		{"objects", ""},
		{"objects", "clips/" + fx.x},
		{"objects", fx.x, "extra"},
		{"objects", fx.x, "--bogus"},
	} {
		if code, out := fx.run(t, args...); code != 2 || !strings.Contains(out, "usage: clippet admin objects") {
			t.Errorf("%v: code %d, want 2 with usage\n%s", args, code, out)
		}
	}
	// A store failure is exit 1 and the store's own sentence.
	fx.fake.FailNext("List", errors.New("bucket down"))
	if code, out := fx.run(t, "objects", fx.x); code != 1 || !strings.Contains(out, "admin objects: ") || !strings.Contains(out, "bucket down") {
		t.Fatalf("objects with a store error: %d %q", code, out)
	}
}

// TestAdminObjectsNeedsNoRowAndNoDatabase is the reason the verb exists: the
// E2E's `wait_gone` asks it about a clip that has just been destroyed, so the
// row is gone — and `admin objects` must answer anyway. It is asserted twice
// over: on the fixture's ROW-LESS orphan prefix, and on a configuration whose
// database file has never been created at all (which every other verb but
// `doctor` and `restore-check` refuses with exit 2).
func TestAdminObjectsNeedsNoRowAndNoDatabase(t *testing.T) {
	fx := newAdminFixture(t)
	// fx.z is two objects under a prefix with no clip row.
	if code, out := fx.run(t, "objects", fx.z, "--count"); code != 0 || strings.TrimSpace(out) != "2" {
		t.Fatalf("objects on a row-less prefix: %d %q", code, out)
	}
	if code, out := fx.run(t, "objects", fx.z); code != 0 || !strings.Contains(out, "2 object(s)") {
		t.Fatalf("objects listing on a row-less prefix: %d %q", code, out)
	}
	// Now with no database file anywhere. `stats` is the control: it refuses.
	cfg, err := config.Load(testEnv(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	fake := store.NewFake()
	id := ids.New()
	fake.Put(store.OriginalKey("", id, "mp4"), []byte("src"), "video/mp4")
	var out, errOut bytes.Buffer
	env := adminEnv{stdout: &out, stderr: &errOut,
		openStore: func(config.Config) (store.Store, error) { return fake, nil }, now: time.Now}
	if code := runAdminWith([]string{"stats"}, cfg, env); code != 2 || !strings.Contains(errOut.String(), "no database") {
		t.Fatalf("the control: stats without a database = %d %q", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := runAdminWith([]string{"objects", id, "--count"}, cfg, env); code != 0 || strings.TrimSpace(out.String()) != "1" {
		t.Fatalf("objects without a database: %d %q %q", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(dbPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("admin objects created a database at %s", dbPath(cfg))
	}
}

// TestAdminObjectsHonoursKeyPrefix: the verb lists the same keys the app
// WRITES, so a bucket shared with something else answers under S3_KEY_PREFIX
// and not beside it. The harness and the verb agree on `clips/<id>/` for
// exactly this reason.
func TestAdminObjectsHonoursKeyPrefix(t *testing.T) {
	getenv := testEnv(filepath.Join(t.TempDir(), "data"))
	cfg, err := config.Load(func(k string) string {
		if k == "S3_KEY_PREFIX" {
			return "tenant-a"
		}
		return getenv(k)
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3.KeyPrefix != "tenant-a/" {
		t.Fatalf("key prefix = %q", cfg.S3.KeyPrefix)
	}
	fake := store.NewFake()
	id := ids.New()
	fake.Put(store.OriginalKey(cfg.S3.KeyPrefix, id, "mp4"), []byte("mine"), "video/mp4")
	fake.Put(store.OriginalKey("", id, "mp4"), []byte("somebody else's"), "video/mp4")
	var out, errOut bytes.Buffer
	env := adminEnv{stdout: &out, stderr: &errOut,
		openStore: func(config.Config) (store.Store, error) { return fake, nil }, now: time.Now}
	if code := runAdminWith([]string{"objects", id}, cfg, env); code != 0 {
		t.Fatalf("objects with a key prefix: %d %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "tenant-a/clips/"+id+"/original/source.mp4") || !strings.Contains(out.String(), "1 object(s)") {
		t.Fatalf("the listing is not the prefixed one:\n%s", out.String())
	}
}

// TestAdminObjectsOnFS: the same verb over a DIRECTORY. `fs` is the backend
// with no bucket at all, and the listing is the store's answer either way —
// which is the whole claim the harness now rests on.
func TestAdminObjectsOnFS(t *testing.T) {
	fx := newFSFixture(t)
	id := ids.New()
	st, err := store.NewFS(fx.media, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "x.bin")
	if err := os.WriteFile(src, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{store.OriginalKey("", id, "mp4"), store.ProbeKey("", id)} {
		if _, err := st.PutFile(ctx, key, src, "application/octet-stream", nil); err != nil {
			t.Fatal(err)
		}
	}
	// No `runMigrate` in this test on purpose: there is no database here at all.
	code, out := fx.admin(t, "objects", id)
	if code != 0 || !strings.Contains(out, "2 object(s)") ||
		!strings.Contains(out, "clips/"+id+"/meta/probe.json 10 ") ||
		!strings.Contains(out, "clips/"+id+"/original/source.mp4 10 ") {
		t.Fatalf("objects on a directory: %d\n%s", code, out)
	}
	if fx.openedFS != 1 || fx.openedS3Count != 0 {
		t.Fatalf("the verb opened fs=%d s3=%d stores", fx.openedFS, fx.openedS3Count)
	}
	if code, out := fx.admin(t, "objects", ids.New(), "--count"); code != 0 || strings.TrimSpace(out) != "0" {
		t.Fatalf("objects --count on an unused directory prefix: %d %q", code, out)
	}
}

// --- the write side and the root-relative read (§20 erratum 346) -------------

// objectsCfg is a configuration with the given S3_KEY_PREFIX and no database.
func objectsCfg(t *testing.T, keyPrefix string) config.Config {
	t.Helper()
	getenv := testEnv(filepath.Join(t.TempDir(), "data"))
	cfg, err := config.Load(func(k string) string {
		if k == "S3_KEY_PREFIX" {
			return keyPrefix
		}
		return getenv(k)
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// objectsRun runs `admin <args>` over fake with stdin, and reports how many
// times the store was opened — a refusal must not open it at all.
func objectsRun(t *testing.T, cfg config.Config, fake store.Store, stdin io.Reader, args ...string) (code int, stdout, stderr string, opened int) {
	t.Helper()
	var out, errOut bytes.Buffer
	env := adminEnv{stdout: &out, stderr: &errOut, stdin: stdin, now: time.Now,
		openStore: func(config.Config) (store.Store, error) { opened++; return fake, nil }}
	code = runAdminWith(args, cfg, env)
	return code, out.String(), errOut.String(), opened
}

// TestAdminObjectsInit: a missing bucket is created and says so, an existing
// one says so, a store failure is exit 1, and an argument is a usage error.
func TestAdminObjectsInit(t *testing.T) {
	cfg := objectsCfg(t, "")
	fake := store.NewFake()
	fake.BucketMissing = true
	if code, out, errOut, _ := objectsRun(t, cfg, fake, nil, "objects", "init"); code != 0 || out != "bucket clips created\n" {
		t.Fatalf("init on a missing bucket: %d %q %q", code, out, errOut)
	}
	if code, out, _, _ := objectsRun(t, cfg, fake, nil, "objects", "init"); code != 0 || out != "bucket clips already exists\n" {
		t.Fatalf("init on an existing bucket: %d %q", code, out)
	}
	fake.FailNext("EnsureBucket", errors.New("bucket down"))
	if code, _, errOut, _ := objectsRun(t, cfg, fake, nil, "objects", "init"); code != 1 || !strings.Contains(errOut, "admin objects init: bucket down") {
		t.Fatalf("init with a store error: %d %q", code, errOut)
	}
	if code, _, errOut, opened := objectsRun(t, cfg, fake, nil, "objects", "init", "clips"); code != 2 || opened != 0 || !strings.Contains(errOut, "usage: clippet admin objects") {
		t.Fatalf("init with an argument: %d opened=%d %q", code, opened, errOut)
	}
}

// TestAdminObjectsPut: stdin is stored under the key AS GIVEN — root-relative,
// with S3_KEY_PREFIX not prepended — and every malformed key, a missing stdin
// and a missing key are refused before the store is opened.
func TestAdminObjectsPut(t *testing.T) {
	cfg := objectsCfg(t, "tenant-a")
	fake := store.NewFake()
	code, out, errOut, _ := objectsRun(t, cfg, fake, strings.NewReader("planted\n"), "objects", "put", "e2e-x-1/clips/a/original/x.bin")
	if code != 0 || out != "stored e2e-x-1/clips/a/original/x.bin 8\n" {
		t.Fatalf("put: %d %q %q", code, out, errOut)
	}
	if b, ok := fake.Get("e2e-x-1/clips/a/original/x.bin"); !ok || string(b) != "planted\n" {
		t.Fatalf("put stored %q (%v), want the stdin bytes at the root-relative key", b, ok)
	}
	if _, ok := fake.Get("tenant-a/e2e-x-1/clips/a/original/x.bin"); ok {
		t.Fatal("put prepended S3_KEY_PREFIX to a root-relative key")
	}
	// An existing object is replaced, and an empty body is an object too.
	if code, out, _, _ := objectsRun(t, cfg, fake, strings.NewReader(""), "objects", "put", "e2e-x-1/clips/a/original/x.bin"); code != 0 || out != "stored e2e-x-1/clips/a/original/x.bin 0\n" {
		t.Fatalf("put over an existing key: %d %q", code, out)
	}
	for _, key := range []string{"/abs", "trailing/", "a//b", "a/../b", "./a", "..", "a b", "a\nb", "clé", "a?b"} {
		code, _, errOut, opened := objectsRun(t, cfg, fake, strings.NewReader("x"), "objects", "put", key)
		if code != 2 || opened != 0 || !strings.Contains(errOut, "admin objects put: refused: ") || !strings.Contains(errOut, "is not a key") {
			t.Errorf("put %q: %d opened=%d %q, want a refusal before the store", key, code, opened, errOut)
		}
	}
	if code, _, errOut, opened := objectsRun(t, cfg, fake, nil, "objects", "put", "a/b"); code != 2 || opened != 0 || !strings.Contains(errOut, "no stdin") {
		t.Fatalf("put without stdin: %d opened=%d %q", code, opened, errOut)
	}
	for _, args := range [][]string{{"objects", "put"}, {"objects", "put", "a", "b"}, {"objects", "put", "--prefix", "a/"}} {
		if code, _, errOut, opened := objectsRun(t, cfg, fake, strings.NewReader("x"), args...); code != 2 || opened != 0 || !strings.Contains(errOut, "usage: clippet admin objects") {
			t.Errorf("%v: %d opened=%d %q, want usage", args, code, opened, errOut)
		}
	}
	fake.FailNext("PutFile", errors.New("bucket down"))
	if code, _, errOut, _ := objectsRun(t, cfg, fake, strings.NewReader("x"), "objects", "put", "a/b"); code != 1 || !strings.Contains(errOut, "admin objects put: bucket down") {
		t.Fatalf("put with a store error: %d %q", code, errOut)
	}
}

// TestAdminObjectsRmKey: one key goes, the key is root-relative, a key that is
// not there is not an error, and a malformed key is refused untouched.
func TestAdminObjectsRmKey(t *testing.T) {
	cfg := objectsCfg(t, "")
	fake := store.NewFake()
	fake.Put("clips/e2eStrangerA/original/stranger.bin", []byte("s"), "application/octet-stream")
	fake.Put("clips/e2eStrangerA/original/keep.bin", []byte("k"), "application/octet-stream")
	if code, out, errOut, _ := objectsRun(t, cfg, fake, nil, "objects", "rm", "clips/e2eStrangerA/original/stranger.bin"); code != 0 || out != "removed clips/e2eStrangerA/original/stranger.bin\n" {
		t.Fatalf("rm: %d %q %q", code, out, errOut)
	}
	if got := fake.Keys(); len(got) != 1 || got[0] != "clips/e2eStrangerA/original/keep.bin" {
		t.Fatalf("after rm the store holds %v, want only the other key", got)
	}
	if code, _, _, _ := objectsRun(t, cfg, fake, nil, "objects", "rm", "clips/nothing/here.bin"); code != 0 {
		t.Fatalf("rm of a missing key: %d, want 0", code)
	}
	for _, key := range []string{"/abs", "clips/", "a//b", "../x", "a b"} {
		code, _, errOut, opened := objectsRun(t, cfg, fake, nil, "objects", "rm", key)
		if code != 2 || opened != 0 || !strings.Contains(errOut, "admin objects rm: refused: ") {
			t.Errorf("rm %q: %d opened=%d %q, want a refusal before the store", key, code, opened, errOut)
		}
	}
	for _, args := range [][]string{{"objects", "rm"}, {"objects", "rm", "a", "b"}, {"objects", "rm", "a/b", "--prefix", "e2e-x-1/"}, {"objects", "rm", "--bogus"}} {
		if code, _, errOut, opened := objectsRun(t, cfg, fake, nil, args...); code != 2 || opened != 0 || !strings.Contains(errOut, "usage: clippet admin objects") {
			t.Errorf("%v: %d opened=%d %q, want usage", args, code, opened, errOut)
		}
	}
	if len(fake.Deletes()) != 1 {
		t.Fatalf("deletes = %v, want the one rm and nothing from a refusal", fake.Deletes())
	}
	fake.FailNext("DeleteKey", errors.New("bucket down"))
	if code, _, errOut, _ := objectsRun(t, cfg, fake, nil, "objects", "rm", "a/b"); code != 1 || !strings.Contains(errOut, "admin objects rm: bucket down") {
		t.Fatalf("rm with a store error: %d %q", code, errOut)
	}
}

// TestAdminObjectsRmPrefix: the recursive form removes a probe's prefix and
// nothing beside it, and refuses — before the store is even opened — an empty
// prefix, the root, a dot-path, anything not in the harness's
// e2e-<word>-<epoch>/ shape, and a prefix that overlaps this instance's own key
// space in either direction.
func TestAdminObjectsRmPrefix(t *testing.T) {
	cfg := objectsCfg(t, "")
	fake := store.NewFake()
	for _, k := range []string{"e2e-o1b-1761400000/clips/a/original/source.mp4", "e2e-o1b-1761400000/clips/a/poster/p.jpg",
		"e2e-o1b-17614000001/clips/b/original/source.mp4", "clips/live/original/source.mp4"} {
		fake.Put(k, []byte("x"), "application/octet-stream")
	}
	code, out, errOut, _ := objectsRun(t, cfg, fake, nil, "objects", "rm", "--prefix", "e2e-o1b-1761400000/")
	if code != 0 || out != "removed 2 object(s) under e2e-o1b-1761400000/\n" {
		t.Fatalf("rm --prefix: %d %q %q", code, out, errOut)
	}
	if got := strings.Join(fake.Keys(), " "); got != "clips/live/original/source.mp4 e2e-o1b-17614000001/clips/b/original/source.mp4" {
		t.Fatalf("after rm --prefix the store holds %q", got)
	}
	// Empty already: 0, and exit 0.
	if code, out, _, _ := objectsRun(t, cfg, fake, nil, "objects", "rm", "--prefix", "e2e-o1b-1761400000/"); code != 0 || out != "removed 0 object(s) under e2e-o1b-1761400000/\n" {
		t.Fatalf("rm --prefix on an empty prefix: %d %q", code, out)
	}
	refusals := []struct{ prefix, why string }{
		{"", "the whole bucket"},
		{"/", "the whole bucket"},
		{".", "the whole bucket"},
		{"./", "the whole bucket"},
		{"..", "the whole bucket"},
		{"clips/", "not a test probe's prefix"},
		{"_clippet/", "not a test probe's prefix"},
		{"doctor/", "not a test probe's prefix"},
		{"e2e-o1b-1761400000", "not a test probe's prefix"},
		{"e2e-o1b-1761400000/clips/", "not a test probe's prefix"},
		{"e2e-O1b-1761400000/", "not a test probe's prefix"},
		{"e2e-o1b-/", "not a test probe's prefix"},
		{"e2e--1/", "not a test probe's prefix"},
		{"x-e2e-o1b-1/", "not a test probe's prefix"},
		{"/e2e-o1b-1/", "not a test probe's prefix"},
	}
	for _, r := range refusals {
		code, _, errOut, opened := objectsRun(t, cfg, fake, nil, "objects", "rm", "--prefix", r.prefix)
		if code != 2 || opened != 0 || !strings.Contains(errOut, "admin objects rm --prefix: refused: ") || !strings.Contains(errOut, r.why) {
			t.Errorf("rm --prefix %q: %d opened=%d %q, want a refusal (%s) before the store", r.prefix, code, opened, errOut, r.why)
		}
	}
	// This instance's own key space, both ways round.
	for _, kp := range []string{"e2e-o1b-1761400000", "e2e-o1b-1761400000/tenant"} {
		own := objectsCfg(t, kp)
		code, _, errOut, opened := objectsRun(t, own, fake, nil, "objects", "rm", "--prefix", "e2e-o1b-1761400000/")
		if code != 2 || opened != 0 || !strings.Contains(errOut, "overlaps this instance's own key space") {
			t.Errorf("rm --prefix with S3_KEY_PREFIX=%s: %d opened=%d %q, want the key-space refusal", kp, code, opened, errOut)
		}
	}
	// …and a prefix BESIDE a non-empty S3_KEY_PREFIX is allowed: that is where probes put theirs.
	beside := objectsCfg(t, "tenant-a")
	if code, _, errOut, _ := objectsRun(t, beside, fake, nil, "objects", "rm", "--prefix", "e2e-o1b-17614000001/"); code != 0 {
		t.Fatalf("rm --prefix beside S3_KEY_PREFIX=tenant-a: %d %q", code, errOut)
	}
	for _, c := range fake.CallsSnapshot() {
		if strings.HasPrefix(c, "DeletePrefix ") && c != "DeletePrefix e2e-o1b-1761400000/" && c != "DeletePrefix e2e-o1b-17614000001/" {
			t.Fatalf("a refused prefix reached the store: %q", c)
		}
	}
	fake.FailNext("DeletePrefix", errors.New("bucket down"))
	if code, _, errOut, _ := objectsRun(t, cfg, fake, nil, "objects", "rm", "--prefix", "e2e-o1b-1/"); code != 1 || !strings.Contains(errOut, "admin objects rm: bucket down") {
		t.Fatalf("rm --prefix with a store error: %d %q", code, errOut)
	}
}

// TestAdminObjectsPrefixList: the root-relative read form lists and counts
// under the prefix as given (S3_KEY_PREFIX not prepended), and refuses a
// malformed prefix or one combined with an id.
func TestAdminObjectsPrefixList(t *testing.T) {
	cfg := objectsCfg(t, "tenant-a")
	fake := store.NewFake()
	fake.Put("e2e-o1b-1/clips/a/original/source.mp4", []byte("0123"), "video/mp4")
	fake.Put("e2e-o1b-1/clips/a/poster/p.jpg", []byte("01"), "image/jpeg")
	fake.Put("tenant-a/e2e-o1b-1/clips/z/original/source.mp4", []byte("no"), "video/mp4")
	code, out, errOut, _ := objectsRun(t, cfg, fake, nil, "objects", "--prefix", "e2e-o1b-1/")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != 0 || len(lines) != 3 || lines[2] != "2 object(s)" ||
		!strings.HasPrefix(lines[0], "e2e-o1b-1/clips/a/original/source.mp4 4 ") || !strings.HasPrefix(lines[1], "e2e-o1b-1/clips/a/poster/p.jpg 2 ") {
		t.Fatalf("objects --prefix: %d %q %q", code, out, errOut)
	}
	for _, args := range [][]string{{"objects", "--prefix", "e2e-o1b-1/", "--count"}, {"objects", "--count", "--prefix", "e2e-o1b-1/"}} {
		if code, out, _, _ := objectsRun(t, cfg, fake, nil, args...); code != 0 || out != "2\n" {
			t.Fatalf("%v: %d %q", args, code, out)
		}
	}
	if code, out, _, _ := objectsRun(t, cfg, fake, nil, "objects", "--prefix", "doctor/", "--count"); code != 0 || out != "0\n" {
		t.Fatalf("objects --prefix on an empty prefix: %d %q", code, out)
	}
	for _, p := range []string{"/x/", "a//", "../", "a b/"} {
		if code, _, errOut, opened := objectsRun(t, cfg, fake, nil, "objects", "--prefix", p); code != 2 || opened != 0 || !strings.Contains(errOut, "refused: ") {
			t.Errorf("objects --prefix %q: %d opened=%d %q, want a refusal", p, code, opened, errOut)
		}
	}
	if code, _, errOut, opened := objectsRun(t, cfg, fake, nil, "objects", ids.New(), "--prefix", "e2e-o1b-1/"); code != 2 || opened != 0 || !strings.Contains(errOut, "usage: clippet admin objects") {
		t.Fatalf("objects <id> --prefix: %d opened=%d %q, want usage", code, opened, errOut)
	}
}

// TestAdminObjectsWritesOnFS: on a directory, init has nothing to create and
// says so without opening anything, and put / rm / rm --prefix / --prefix work
// exactly as the read form does there.
func TestAdminObjectsWritesOnFS(t *testing.T) {
	fx := newFSFixture(t)
	if code, out := fx.admin(t, "objects", "init"); code != 0 || !strings.Contains(out, "nothing to create: this instance keeps its media in a directory") {
		t.Fatalf("init on fs: %d %q", code, out)
	}
	if fx.openedFS != 0 || fx.openedS3Count != 0 {
		t.Fatalf("init on fs opened fs=%d s3=%d stores", fx.openedFS, fx.openedS3Count)
	}
	open := func(c config.Config) (store.Store, error) { return store.NewFS(c.StorageDir, c.S3.OpTimeout) }
	var out, errOut bytes.Buffer
	run := func(stdin io.Reader, args ...string) int {
		out.Reset()
		errOut.Reset()
		return runAdminWith(args, fx.cfg, adminEnv{stdout: &out, stderr: &errOut, stdin: stdin, openStore: open, now: time.Now})
	}
	if code := run(strings.NewReader("abc"), "objects", "put", "e2e-fs-1/clips/a/original/source.mp4"); code != 0 {
		t.Fatalf("put on fs: %d %q", code, errOut.String())
	}
	if code := run(strings.NewReader("de"), "objects", "put", "e2e-fs-1/clips/a/poster/p.jpg"); code != 0 {
		t.Fatalf("put on fs: %d %q", code, errOut.String())
	}
	if b, err := os.ReadFile(filepath.Join(fx.media, "e2e-fs-1", "clips", "a", "original", "source.mp4")); err != nil || string(b) != "abc" {
		t.Fatalf("put on fs wrote %q (%v)", b, err)
	}
	if code := run(nil, "objects", "--prefix", "e2e-fs-1/", "--count"); code != 0 || out.String() != "2\n" {
		t.Fatalf("--prefix --count on fs: %d %q %q", code, out.String(), errOut.String())
	}
	if code := run(nil, "objects", "rm", "e2e-fs-1/clips/a/poster/p.jpg"); code != 0 {
		t.Fatalf("rm on fs: %d %q", code, errOut.String())
	}
	if code := run(nil, "objects", "rm", "--prefix", "e2e-fs-1/"); code != 0 || out.String() != "removed 1 object(s) under e2e-fs-1/\n" {
		t.Fatalf("rm --prefix on fs: %d %q %q", code, out.String(), errOut.String())
	}
	if code := run(nil, "objects", "--prefix", "e2e-fs-1/", "--count"); code != 0 || out.String() != "0\n" {
		t.Fatalf("--prefix --count on fs after the removal: %d %q", code, out.String())
	}
	if code := run(nil, "objects", "rm", "--prefix", ""); code != 2 || !strings.Contains(errOut.String(), "the whole bucket") {
		t.Fatalf("rm --prefix '' on fs: %d %q", code, errOut.String())
	}
}
