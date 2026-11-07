package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newFSStore returns a filesystem store over a fresh temp directory and the
// directory itself (so a test can look at what is on disk, which is the whole
// point of this backend).
func newFSStore(t *testing.T) (*fsStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewFS(dir, 0)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	fs, ok := s.(*fsStore)
	if !ok {
		t.Fatalf("NewFS returned %T", s)
	}
	return fs, dir
}

func TestFSNewRejectsARelativeDir(t *testing.T) {
	if _, err := NewFS("media", 0); err == nil {
		t.Fatal("NewFS accepted a relative STORAGE_DIR")
	}
	if _, err := NewFS("", 0); err == nil {
		t.Fatal("NewFS accepted an empty STORAGE_DIR")
	}
}

// TestFSPathLaw is the traversal gate (§20 erratum 276). Every key below must
// be refused by the ONE function that maps keys to paths, and — because that
// is what actually protects the disk — by every method that takes a key.
func TestFSPathLaw(t *testing.T) {
	ctx := context.Background()
	s, dir := newFSStore(t)
	src := writeTemp(t, []byte("payload"))

	// A file OUTSIDE the store, which the traversal keys below aim at.
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(outside, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}

	bad := []string{
		"../outside.txt",
		"clips/../../outside.txt",
		"clips/x/../../../outside.txt",
		"/etc/passwd",
		"/",
		"./clips/x",
		"clips/./x",
		"clips//x",
		"",
		"clips\\x",
		"clips/x\x00.mp4",
		"..",
		".",
	}
	for _, key := range bad {
		t.Run("key="+strings.ReplaceAll(key, "/", "_"), func(t *testing.T) {
			if _, err := s.PutFile(ctx, key, src, "video/mp4", nil); err == nil {
				t.Fatalf("PutFile accepted %q", key)
			}
			if err := s.Download(ctx, key, filepath.Join(t.TempDir(), "out"), nil); err == nil {
				t.Fatalf("Download accepted %q", key)
			}
			if _, err := s.GetRange(ctx, key, 0, -1); err == nil {
				t.Fatalf("GetRange accepted %q", key)
			}
			if _, err := s.Stat(ctx, key); err == nil {
				t.Fatalf("Stat accepted %q", key)
			}
		})
	}
	// The empty key is the ONE that List accepts: an empty prefix means
	// "everything", the way it does in S3. Every other bad key is refused
	// there too.
	if _, err := s.List(ctx, ""); err != nil {
		t.Fatalf("List(\"\"): %v", err)
	}
	for _, key := range bad {
		if key == "" {
			continue
		}
		if _, err := s.List(ctx, key); err == nil {
			t.Fatalf("List accepted %q", key)
		}
		if _, err := s.DeletePrefix(ctx, key); err == nil {
			t.Fatalf("DeletePrefix accepted %q", key)
		}
	}
	// Nothing reached the file next door, and nothing was created beside it.
	if b, err := os.ReadFile(outside); err != nil || string(b) != "do not touch" {
		t.Fatalf("the file outside the store was touched: %q %v", b, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the store is not empty after the refusals: %v", entries)
	}
}

// TestFSPathLawTrailingSlash: a prefix is not an object.
func TestFSPathLawTrailingSlash(t *testing.T) {
	ctx := context.Background()
	s, _ := newFSStore(t)
	if _, err := s.Stat(ctx, "clips/x/"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("Stat on a prefix: %v", err)
	}
	if _, err := s.PutFile(ctx, "clips/x/", writeTemp(t, []byte("x")), "", nil); !errors.Is(err, ErrBadKey) {
		t.Fatalf("PutFile on a prefix: %v", err)
	}
}

func TestFSPutStatGetDownload(t *testing.T) {
	ctx := context.Background()
	s, dir := newFSStore(t)
	data := bytes.Repeat([]byte("clippet fs store "), 200000) // ~3.4 MiB: several copy chunks
	src := writeTemp(t, data)
	key := "clips/abc/original/source.mp4"

	var calls int
	var lastDone, lastTotal int64
	prev := int64(-1)
	res, err := s.PutFile(ctx, key, src, "video/mp4", func(done, total int64) {
		calls++
		if done < prev {
			t.Fatalf("progress went backwards: %d after %d", done, prev)
		}
		if done > total {
			t.Fatalf("progress reported %d of %d", done, total)
		}
		prev, lastDone, lastTotal = done, done, total
	})
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	sum := sha256.Sum256(data)
	if res.Size != int64(len(data)) || res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("PutResult = %+v", res)
	}
	if calls < 2 || lastDone != int64(len(data)) || lastTotal != int64(len(data)) {
		t.Fatalf("progress: calls=%d done=%d total=%d", calls, lastDone, lastTotal)
	}

	// It is on disk, at the path the key spells, with the store's mode and no
	// temp file beside it.
	onDisk := filepath.Join(dir, "clips", "abc", "original", "source.mp4")
	got, err := os.ReadFile(onDisk)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("on disk: %d bytes, %v", len(got), err)
	}
	fi, err := os.Stat(onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != fsFileMode {
		t.Fatalf("mode = %v, want %v", fi.Mode().Perm(), os.FileMode(fsFileMode))
	}
	entries, err := os.ReadDir(filepath.Dir(onDisk))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("a temp file was left behind: %v", entries)
	}

	info, err := s.Stat(ctx, key)
	if err != nil || info.Key != key || info.Size != int64(len(data)) {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	if info.ETag != "" {
		t.Fatalf("the fs store reported an ETag (%q); erratum 276 says it does not", info.ETag)
	}
	if time.Since(info.LastModified) > time.Minute {
		t.Fatalf("LastModified = %v", info.LastModified)
	}
	if _, err := s.Stat(ctx, "clips/abc/original/missing.mp4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat of a missing key: %v", err)
	}
	if c := PublicClass(mustStatErr(t, s, "clips/abc/nope.mp4")); c != PublicNotFound {
		t.Fatalf("the public class of a missing key is %q", c)
	}

	// Download round-trips, reports progress and leaves no .part behind.
	out := filepath.Join(t.TempDir(), "nested", "copy.mp4")
	calls, lastDone, lastTotal = 0, 0, 0
	if err := s.Download(ctx, key, out, func(done, total int64) {
		calls++
		lastDone, lastTotal = done, total
	}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	back, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(back, data) {
		t.Fatalf("downloaded %d bytes, %v", len(back), err)
	}
	if calls == 0 || lastDone != int64(len(data)) || lastTotal != int64(len(data)) {
		t.Fatalf("download progress: calls=%d done=%d total=%d", calls, lastDone, lastTotal)
	}
	if _, err := os.Stat(out + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Download left its .part file behind")
	}
	if err := s.Download(ctx, "clips/abc/renditions/nothing.mp4", out, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Download of a missing key: %v", err)
	}
}

func mustStatErr(t *testing.T, s Store, key string) error {
	t.Helper()
	_, err := s.Stat(context.Background(), key)
	if err == nil {
		t.Fatalf("Stat(%q) succeeded", key)
	}
	return err
}

// TestFSPutOverwritesAtomically: the second Put replaces the object and never
// leaves a half-written one under the key.
func TestFSPutOverwritesAtomically(t *testing.T) {
	ctx := context.Background()
	s, dir := newFSStore(t)
	key := "clips/x/renditions/main-a1b2c3d4.mp4"
	if _, err := s.PutFile(ctx, key, writeTemp(t, []byte("first")), "video/mp4", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutFile(ctx, key, writeTemp(t, []byte("second, longer")), "video/mp4", nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "clips", "x", "renditions", "main-a1b2c3d4.mp4"))
	if err != nil || string(got) != "second, longer" {
		t.Fatalf("after the overwrite: %q %v", got, err)
	}
	list, err := s.List(ctx, "clips/x/")
	if err != nil || len(list) != 1 {
		t.Fatalf("List after the overwrite = %+v, %v", list, err)
	}
}

// TestFSGetRangeEdges pins the fs store against the semantics minio.go and the
// Fake already have: end is INCLUSIVE, -1 means EOF, an end past the last byte
// is clamped, and a start at or past the end is an error (S3's 416).
func TestFSGetRangeEdges(t *testing.T) {
	ctx := context.Background()
	s, _ := newFSStore(t)
	f := NewFake()
	data := []byte("0123456789")
	src := writeTemp(t, data)
	key := "clips/r/original/source.bin"
	if _, err := s.PutFile(ctx, key, src, "application/octet-stream", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.PutFile(ctx, key, src, "application/octet-stream", nil); err != nil {
		t.Fatal(err)
	}

	ok := []struct {
		start, end int64
		want       string
	}{
		{0, -1, "0123456789"},
		{0, 0, "0"},
		{0, 3, "0123"},   // INCLUSIVE end
		{3, 5, "345"},    //
		{5, -1, "56789"}, // to EOF
		{9, 9, "9"},
		{0, 99, "0123456789"}, // an end past the object is clamped
		{7, 99, "789"},
	}
	for _, c := range ok {
		got := readAll(t, s, key, c.start, c.end)
		if got != c.want {
			t.Fatalf("fs GetRange(%d,%d) = %q, want %q", c.start, c.end, got, c.want)
		}
		if fake := readAll(t, f, key, c.start, c.end); fake != c.want {
			t.Fatalf("the Fake disagrees on (%d,%d): %q", c.start, c.end, fake)
		}
	}
	// Refused by the fs store. The first five are refused by the Fake too and
	// are cross-checked; `{0,-2}` is the one place the two existing
	// implementations already disagree — minio.go's rangeHeader refuses an end
	// under -1 and the Fake clamps it — and fs follows the REAL backend.
	bad := [][2]int64{{10, -1}, {10, 12}, {11, 11}, {-1, 5}, {5, 3}}
	for _, c := range bad {
		if rc, err := s.GetRange(ctx, key, c[0], c[1]); err == nil {
			rc.Close()
			t.Fatalf("fs GetRange(%d,%d) succeeded", c[0], c[1])
		}
		if rc, err := f.GetRange(ctx, key, c[0], c[1]); err == nil {
			rc.Close()
			t.Fatalf("the Fake accepts (%d,%d) but fs does not", c[0], c[1])
		}
	}
	if rc, err := s.GetRange(ctx, key, 0, -2); err == nil {
		rc.Close()
		t.Fatal("fs GetRange(0,-2) succeeded; minio.go's rangeHeader refuses an end under -1")
	}
	// An EMPTY object has no satisfiable range at all, which is what S3 says.
	empty := "clips/r/meta/probe.json"
	if _, err := s.PutFile(ctx, empty, writeTemp(t, nil), "application/json", nil); err != nil {
		t.Fatal(err)
	}
	if rc, err := s.GetRange(ctx, empty, 0, -1); err == nil {
		rc.Close()
		t.Fatal("GetRange on an empty object succeeded")
	}
	if _, err := s.GetRange(ctx, "clips/r/renditions/absent.mp4", 0, -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRange of a missing key: %v", err)
	}
}

func readAll(t *testing.T, s Store, key string, start, end int64) string {
	t.Helper()
	rc, err := s.GetRange(context.Background(), key, start, end)
	if err != nil {
		t.Fatalf("GetRange(%d,%d): %v", start, end, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read (%d,%d): %v", start, end, err)
	}
	return string(b)
}

// TestFSGetRangeStopsOnACancelledContext: a client that disconnects ends the
// body, as it does on the S3 store and the Fake.
func TestFSGetRangeStopsOnACancelledContext(t *testing.T) {
	s, _ := newFSStore(t)
	key := "clips/c/original/source.bin"
	if _, err := s.PutFile(context.Background(), key, writeTemp(t, bytes.Repeat([]byte("x"), 4096)), "", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rc, err := s.GetRange(ctx, key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	cancel()
	if _, err := rc.Read(make([]byte, 16)); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled read: %v", err)
	}
}

// TestFSSeekerOverTheStore: the media hot path is store.NewSeeker over
// GetRange, so the two have to agree about the inclusive end.
func TestFSSeekerOverTheStore(t *testing.T) {
	ctx := context.Background()
	s, _ := newFSStore(t)
	data := []byte("abcdefghijklmnopqrstuvwxyz")
	key := "clips/s/renditions/main-00000000.mp4"
	if _, err := s.PutFile(ctx, key, writeTemp(t, data), "video/mp4", nil); err != nil {
		t.Fatal(err)
	}
	sk := NewSeeker(ctx, s, key, int64(len(data)))
	defer sk.Close()
	if _, err := sk.Seek(20, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(sk)
	if err != nil || string(tail) != "uvwxyz" {
		t.Fatalf("seeker tail = %q, %v", tail, err)
	}
	if _, err := sk.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(sk)
	if err != nil || string(all) != string(data) {
		t.Fatalf("seeker whole = %q, %v", all, err)
	}
}

// TestFSListIsAStringPrefix: S3 answers `clips/ab` with every key that begins
// with those bytes, and `admin orphans` compares key strings, so this must too.
func TestFSListIsAStringPrefix(t *testing.T) {
	ctx := context.Background()
	s, dir := newFSStore(t)
	keys := []string{
		"clips/ab/original/source.mp4",
		"clips/ab/renditions/main-1.mp4",
		"clips/ab/renditions/social-1.mp4",
		"clips/abcdef/original/source.mp4",
		"clips/zz/meta/probe.json",
		"other/thing.txt",
	}
	for _, k := range keys {
		if _, err := s.PutFile(ctx, k, writeTemp(t, []byte(k)), "", nil); err != nil {
			t.Fatalf("PutFile %s: %v", k, err)
		}
	}
	if err := WriteMarker(dir); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		prefix string
		want   []string
	}{
		{"clips/", []string{"clips/ab/original/source.mp4", "clips/ab/renditions/main-1.mp4", "clips/ab/renditions/social-1.mp4", "clips/abcdef/original/source.mp4", "clips/zz/meta/probe.json"}},
		{"clips/ab/", []string{"clips/ab/original/source.mp4", "clips/ab/renditions/main-1.mp4", "clips/ab/renditions/social-1.mp4"}},
		{"clips/ab", []string{"clips/ab/original/source.mp4", "clips/ab/renditions/main-1.mp4", "clips/ab/renditions/social-1.mp4", "clips/abcdef/original/source.mp4"}},
		{"clips/ab/renditions/social-", []string{"clips/ab/renditions/social-1.mp4"}},
		{"clips/ab/renditions/main-1.mp4", []string{"clips/ab/renditions/main-1.mp4"}},
		{"clips/nothing/", nil},
		{"nosuchtree/at/all/", nil},
		{"", append(append([]string{}, keys...), "")[:0]}, // filled below
	}
	for _, c := range cases {
		if c.prefix == "" {
			continue
		}
		got := listKeys(t, s, c.prefix)
		if !equalStrings(got, c.want) {
			t.Fatalf("List(%q) = %v, want %v", c.prefix, got, c.want)
		}
	}
	// The whole store, and the marker is NOT an object.
	all := listKeys(t, s, "")
	if len(all) != len(keys) {
		t.Fatalf("List(\"\") = %v", all)
	}
	for _, k := range all {
		if k == MarkerName {
			t.Fatal("List reported the store marker as an object")
		}
	}
	// Sizes and times come back.
	one, err := s.List(ctx, "clips/zz/")
	if err != nil || len(one) != 1 || one[0].Size != int64(len("clips/zz/meta/probe.json")) || one[0].LastModified.IsZero() {
		t.Fatalf("List detail = %+v, %v", one, err)
	}
}

// TestFSListReportsLitter: a `.clippet-put-*` file left by a crashed transfer
// is an object as far as List is concerned, which is what lets `admin orphans`
// report it (§20 erratum 276).
func TestFSListReportsLitter(t *testing.T) {
	s, dir := newFSStore(t)
	if err := os.MkdirAll(filepath.Join(dir, "clips", "lit", "poster"), 0o755); err != nil {
		t.Fatal(err)
	}
	litter := filepath.Join(dir, "clips", "lit", "poster", fsPutTemp+"123456")
	if err := os.WriteFile(litter, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := listKeys(t, s, "clips/lit/")
	if len(got) != 1 || !strings.HasSuffix(got[0], "123456") {
		t.Fatalf("List did not report the crashed transfer's temp file: %v", got)
	}
}

func listKeys(t *testing.T, s Store, prefix string) []string {
	t.Helper()
	objs, err := s.List(context.Background(), prefix)
	if err != nil {
		t.Fatalf("List(%q): %v", prefix, err)
	}
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Key)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFSDeletePrefix: the whole tree goes, the emptied directories go with it,
// the root stays, and a single key is its own prefix.
func TestFSDeletePrefix(t *testing.T) {
	ctx := context.Background()
	s, dir := newFSStore(t)
	for _, k := range []string{
		"clips/dd/original/source.mp4",
		"clips/dd/renditions/main-1.mp4",
		"clips/dd/poster/poster-1.jpg",
		"clips/dd/meta/probe.json",
		"clips/ee/original/source.mp4",
	} {
		if _, err := s.PutFile(ctx, k, writeTemp(t, []byte(k)), "", nil); err != nil {
			t.Fatal(err)
		}
	}
	// One key, through the same call the worker uses for a superseded object.
	n, err := s.DeletePrefix(ctx, "clips/dd/poster/poster-1.jpg")
	if err != nil || n != 1 {
		t.Fatalf("DeletePrefix(one key) = %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "clips", "dd", "poster")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the emptied directory was not pruned: %v", err)
	}

	n, err = s.DeletePrefix(ctx, "clips/dd/")
	if err != nil || n != 3 {
		t.Fatalf("DeletePrefix(clip) = %d, %v", n, err)
	}
	if got := listKeys(t, s, "clips/dd/"); len(got) != 0 {
		t.Fatalf("the prefix is not empty: %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "clips", "dd")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the clip's directory was not pruned: %v", err)
	}
	// The neighbour is untouched and the root is still there.
	if got := listKeys(t, s, "clips/ee/"); len(got) != 1 {
		t.Fatalf("the neighbouring clip: %v", got)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("the store root was pruned: %v", err)
	}
	// An absent prefix deletes nothing and is not an error.
	if n, err := s.DeletePrefix(ctx, "clips/nothing/"); n != 0 || err != nil {
		t.Fatalf("DeletePrefix(absent) = %d, %v", n, err)
	}
}

// TestFSDeletePrefixVerifies: the contract minio.go documents — re-list, and
// report ErrPrefixNotEmpty rather than claiming success — is what the sweeper's
// "the objects are gone" rests on.
func TestFSDeletePrefixVerifies(t *testing.T) {
	ctx := context.Background()
	s, dir := newFSStore(t)
	if _, err := s.PutFile(ctx, "clips/vv/original/source.mp4", writeTemp(t, []byte("x")), "", nil); err != nil {
		t.Fatal(err)
	}
	// A sub-directory whose object cannot be removed: make the directory
	// read-only, so the unlink inside it fails.
	stuck := filepath.Join(dir, "clips", "vv", "original")
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o755) })
	_, err := s.DeletePrefix(ctx, "clips/vv/")
	if err == nil {
		// Under a uid with CAP_DAC_OVERRIDE — root in a container, which is
		// what the project's own test image runs as — a read-only directory
		// does not refuse an unlink, so the removal simply succeeded. That is
		// not a failure of the code; it is the test's setup failing to create
		// the situation, and saying so is better than a green line that
		// proved nothing.
		if os.Geteuid() == 0 {
			if left := listKeys(t, s, "clips/vv/"); len(left) != 0 {
				t.Fatalf("DeletePrefix reported success and left %v", left)
			}
			t.Skip("uid 0 with CAP_DAC_OVERRIDE: a read-only directory could not be made to refuse the unlink")
		}
		t.Fatal("DeletePrefix reported success with the object still there")
	}
	if !errors.Is(err, ErrPrefixNotEmpty) && !strings.Contains(err.Error(), "remove") {
		t.Fatalf("DeletePrefix error = %v", err)
	}
}

// TestFSDeletePrefixIsEmptyAfterwards is the half of the same contract that
// holds on every uid: the call's own claim is checked by re-listing, and the
// count it returns is the number of objects it actually removed.
func TestFSDeletePrefixIsEmptyAfterwards(t *testing.T) {
	ctx := context.Background()
	s, _ := newFSStore(t)
	for i := range 12 {
		k := "clips/many/renditions/main-" + string(rune('a'+i)) + ".mp4"
		if _, err := s.PutFile(ctx, k, writeTemp(t, []byte(k)), "video/mp4", nil); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.DeletePrefix(ctx, "clips/many/")
	if err != nil || n != 12 {
		t.Fatalf("DeletePrefix = %d, %v", n, err)
	}
	if left, err := s.List(ctx, "clips/many/"); err != nil || len(left) != 0 {
		t.Fatalf("the prefix is not empty: %+v, %v", left, err)
	}
}

func TestFSPing(t *testing.T) {
	ctx := context.Background()
	s, dir := newFSStore(t)
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	// It leaves nothing behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Ping left a probe file: %v", entries)
	}

	// A missing directory is `bucket missing`, the closed-set member for "the
	// place the objects live is not there" (§20 erratum 31).
	gone, err := NewFS(filepath.Join(dir, "not-mounted"), 0)
	if err != nil {
		t.Fatal(err)
	}
	err = gone.Ping(ctx)
	var pe *PingError
	if !errors.As(err, &pe) || pe.HealthClass() != PingBucketMissing {
		t.Fatalf("Ping of a missing directory = %v", err)
	}
	// A FILE where the directory should be is the same class.
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	notDir, err := NewFS(file, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := notDir.Ping(ctx); !errors.As(err, &pe) || pe.HealthClass() != PingBucketMissing {
		t.Fatalf("Ping of a non-directory = %v", err)
	}
}

func TestFSMarker(t *testing.T) {
	_, dir := newFSStore(t)
	has, err := HasMarker(dir)
	if err != nil || has {
		t.Fatalf("HasMarker on a fresh directory = %v, %v", has, err)
	}
	if err := WriteMarker(dir); err != nil {
		t.Fatal(err)
	}
	has, err = HasMarker(dir)
	if err != nil || !has {
		t.Fatalf("HasMarker after WriteMarker = %v, %v", has, err)
	}
	// It does not rewrite an existing marker (the stamp inside it is the one
	// from the first boot, which is the only thing it is ever read for).
	before, err := os.ReadFile(filepath.Join(dir, MarkerName))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteMarker(dir); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, MarkerName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("WriteMarker rewrote the marker")
	}
}

// TestFSWholeClip drives one clip's whole key set through keys.go — the ONLY
// place a key is built — the way the worker does, then destroys it the way the
// sweeper does.
func TestFSWholeClip(t *testing.T) {
	ctx := context.Background()
	s, _ := newFSStore(t)
	const (
		prefix = "inst/"
		id     = "Ab3XyZ9kLmQp"
		r8     = "0f1e2d3c"
	)
	keys := []string{
		OriginalKey(prefix, id, "mkv"),
		RenditionKey(prefix, id, r8),
		SocialKey(prefix, id, r8),
		PosterKey(prefix, id, r8),
		GIFKey(prefix, id, r8),
		FilmstripKey(prefix, id, r8),
		ProbeKey(prefix, id),
		ReplacementKey(prefix, id, "deadbeef", "mp4"),
	}
	for _, k := range keys {
		if _, err := s.PutFile(ctx, k, writeTemp(t, []byte(k)), ContentTypeFor("", "mp4"), nil); err != nil {
			t.Fatalf("PutFile %s: %v", k, err)
		}
	}
	got := listKeys(t, s, ClipPrefix(prefix, id))
	if len(got) != len(keys) {
		t.Fatalf("the clip prefix holds %d objects, want %d: %v", len(got), len(keys), got)
	}
	n, err := s.DeletePrefix(ctx, ClipPrefix(prefix, id))
	if err != nil || n != len(keys) {
		t.Fatalf("DeletePrefix = %d, %v", n, err)
	}
	if got := listKeys(t, s, ClipPrefix(prefix, id)); len(got) != 0 {
		t.Fatalf("after the destroy: %v", got)
	}
}

// TestFSPutFileCancelled: a cancelled transfer leaves nothing under the key.
func TestFSPutFileCancelled(t *testing.T) {
	s, dir := newFSStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	key := "clips/kk/original/source.mp4"
	if _, err := s.PutFile(ctx, key, writeTemp(t, []byte("payload")), "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("PutFile with a cancelled ctx: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "clips", "kk", "original", "source.mp4")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a cancelled PutFile left an object under the key")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "clips", "kk", "original"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a cancelled PutFile left its temp file: %v", entries)
	}
}

// TestFSSatisfiesTheStoreContract is the compile-time statement that the same
// seven methods answer for a directory.
func TestFSSatisfiesTheStoreContract(t *testing.T) {
	var s Store
	var err error
	if s, err = NewFS(t.TempDir(), 0); err != nil || s == nil {
		t.Fatalf("NewFS: %v", err)
	}
}

// --- the symlink rule (§20 erratum 276(q)) -------------------------------------

// mustSymlink makes a symlink, or skips the test where the platform will not
// (unprivileged Windows). The project's own test image is Linux, where this
// always works.
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this platform will not make a symlink: %v", err)
	}
}

// TestFSRefusesToFollowASymlinkedObject: a symlink planted AS an object is
// LITTER — List reports it so `admin orphans` can see it, every read and write
// refuses it, and DeletePrefix unlinks it without touching what it points at.
func TestFSRefusesToFollowASymlinkedObject(t *testing.T) {
	ctx := context.Background()
	s, dir := newFSStore(t)
	// A real object, so the clip's directory exists.
	real := "clips/sym/original/source.mp4"
	if _, err := s.PutFile(ctx, real, writeTemp(t, []byte("the real one")), "video/mp4", nil); err != nil {
		t.Fatal(err)
	}
	// A file OUTSIDE the store, and a symlink to it under a live clip.
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("not this store's business"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := "clips/sym/renditions/main-deadbeef.mp4"
	if err := os.MkdirAll(filepath.Join(dir, "clips", "sym", "renditions"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, filepath.Join(dir, filepath.FromSlash(link)))

	// List REPORTS it: a report that hid it would call this store clean while
	// a symlink sat under a live clip.
	got := listKeys(t, s, "clips/sym/")
	if !equalStrings(got, []string{real, link}) {
		t.Fatalf("List did not report the symlink as litter: %v", got)
	}
	// Every read and write refuses it, and says why.
	for name, err := range map[string]error{
		"Stat":     errOf2(s.Stat(ctx, link)),
		"GetRange": errOf2(s.GetRange(ctx, link, 0, -1)),
		"Download": s.Download(ctx, link, filepath.Join(t.TempDir(), "out"), nil),
		"PutFile":  errOf2(s.PutFile(ctx, link, writeTemp(t, []byte("x")), "", nil)),
	} {
		if !errors.Is(err, ErrBadKey) {
			t.Fatalf("%s followed the symlink: %v", name, err)
		}
		if !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("%s's refusal does not say why: %v", name, err)
		}
	}
	// The file it points at is untouched by any of that.
	if b, err := os.ReadFile(outside); err != nil || string(b) != "not this store's business" {
		t.Fatalf("the target was written through: %q %v", b, err)
	}
	// DeletePrefix UNLINKS it (os.Remove never follows) and leaves the target.
	n, err := s.DeletePrefix(ctx, link)
	if err != nil || n != 1 {
		t.Fatalf("DeletePrefix(symlink) = %d, %v", n, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(link))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the symlink survived DeletePrefix: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("DeletePrefix removed the symlink's TARGET: %v", err)
	}
	// And the store is clean afterwards, by its own report.
	if got := listKeys(t, s, "clips/sym/"); !equalStrings(got, []string{real}) {
		t.Fatalf("after the unlink: %v", got)
	}
}

// TestFSRefusesASymlinkedDirectoryComponent: the case the lexical path law
// cannot see — `<root>/clips` pointing somewhere else would make every key
// under it read and write outside the store.
func TestFSRefusesASymlinkedDirectoryComponent(t *testing.T) {
	ctx := context.Background()
	s, dir := newFSStore(t)
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "loot.mp4"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, elsewhere, filepath.Join(dir, "clips"))

	key := "clips/loot.mp4"
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrBadKey) {
		t.Fatalf("Stat through a symlinked directory: %v", err)
	}
	if _, err := s.GetRange(ctx, key, 0, -1); !errors.Is(err, ErrBadKey) {
		t.Fatalf("GetRange through a symlinked directory: %v", err)
	}
	if _, err := s.PutFile(ctx, "clips/planted/original/source.mp4", writeTemp(t, []byte("x")), "", nil); !errors.Is(err, ErrBadKey) {
		t.Fatalf("PutFile through a symlinked directory: %v", err)
	}
	if _, err := s.List(ctx, "clips/"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("List through a symlinked directory: %v", err)
	}
	// Nothing was created next door.
	entries, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("something was written outside the store: %v", entries)
	}
}

// TestFSAllowsASymlinkedRoot: STORAGE_DIR itself pointing at a mount is the
// operator's own configuration and an ordinary way to deploy this — the rule
// starts BELOW the root.
func TestFSAllowsASymlinkedRoot(t *testing.T) {
	ctx := context.Background()
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "media")
	mustSymlink(t, real, link)
	s, err := NewFS(link, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping on a symlinked STORAGE_DIR: %v", err)
	}
	key := "clips/root/original/source.mp4"
	if _, err := s.PutFile(ctx, key, writeTemp(t, []byte("fine")), "video/mp4", nil); err != nil {
		t.Fatalf("PutFile on a symlinked STORAGE_DIR: %v", err)
	}
	if got := readAll(t, s, key, 0, -1); got != "fine" {
		t.Fatalf("read back %q", got)
	}
}

func errOf2[T any](_ T, err error) error { return err }

// --- STORAGE_OP_TIMEOUT on the control plane (§20 erratum 276(q)) --------------

// TestFSControlPlaneHonoursTheOpTimeout: a filesystem call cannot be
// cancelled, so the store gives up on it instead. The probe is the timeout
// itself, driven through a directory that exists: a 1 ns deadline expires
// before the goroutine can answer, and the caller gets the bounded TIMEOUT
// class rather than blocking.
func TestFSControlPlaneHonoursTheOpTimeout(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := NewFS(dir, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	perr := s.Ping(ctx)
	var pe *PingError
	if !errors.As(perr, &pe) || pe.HealthClass() != PingTimeout {
		t.Fatalf("Ping under a 1ns op timeout = %v", perr)
	}
	if !strings.Contains(perr.Error(), "did not answer") {
		t.Fatalf("the ping timeout does not say what happened: %v", perr)
	}
	if _, err := s.Stat(ctx, "clips/x/original/source.mp4"); PublicClass(err) != PublicTimeout {
		t.Fatalf("Stat under a 1ns op timeout = %v (class %q)", err, PublicClass(err))
	}
	if _, err := s.List(ctx, "clips/"); PublicClass(err) != PublicTimeout {
		t.Fatalf("List under a 1ns op timeout = %v (class %q)", err, PublicClass(err))
	}
	// A sane timeout does not get in the way of the same three calls.
	ok, err := NewFS(dir, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := ok.Ping(ctx); err != nil {
		t.Fatalf("Ping under a 30s op timeout: %v", err)
	}
	if _, err := ok.List(ctx, "clips/"); err != nil {
		t.Fatalf("List under a 30s op timeout: %v", err)
	}
}

// TestFSRefusesToPresign (§20 erratum 278): a directory has no origin and no
// credential, so there is no URL to hand a viewer. The refusal is a named
// sentinel rather than a panic — the Store contract is total, and `admin`
// verbs hold this interface too — and MEDIA_DELIVERY=redirect on `fs` is
// refused at BOOT, so nothing in a request can reach this.
func TestFSRefusesToPresign(t *testing.T) {
	s, _ := newFSStore(t)
	u, err := s.PresignGet(context.Background(), "clips/x/renditions/r.mp4", time.Hour, "video/mp4", "inline")
	if u != "" {
		t.Errorf("the fs store returned a URL: %q", u)
	}
	if !errors.Is(err, ErrPresignUnsupported) {
		t.Fatalf("err = %v, want ErrPresignUnsupported", err)
	}
	if PublicClass(err) != PublicUnavailable {
		t.Errorf("public class = %q", PublicClass(err))
	}
}

// TestFSPutFileOnAMountThatRefusesModes: a directory that keeps no unix modes
// (vfat/exFAT, SMB without unix extensions) answers PutFile's chmod with
// EPERM or ENOTSUP, and that is not a failed write (erratum 276, amended):
// every object lands, the refusal is logged ONCE for the store, and an error
// that is not a "modes unsupported" one still fails the write.
func TestFSPutFileOnAMountThatRefusesModes(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLog) })
	refuse := syscall.EPERM
	fsChmod = func(name string, _ os.FileMode) error {
		return &fs.PathError{Op: "chmod", Path: name, Err: refuse}
	}
	t.Cleanup(func() { fsChmod = os.Chmod })

	s, dir := newFSStore(t)
	for i, errno := range []syscall.Errno{syscall.EPERM, syscall.ENOTSUP, syscall.EPERM} {
		refuse = errno
		key := "clips/abc/original/source" + string(rune('a'+i)) + ".mp4"
		if _, err := s.PutFile(ctx, key, writeTemp(t, []byte("bytes")), "video/mp4", nil); err != nil {
			t.Fatalf("PutFile with chmod refused (%v): %v", errno, err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key))); err != nil || string(got) != "bytes" {
			t.Fatalf("%s did not land: %q %v", key, got, err)
		}
	}
	if n := strings.Count(logs.String(), "refuses a file mode change"); n != 1 {
		t.Fatalf("the refusal was logged %d times, want once:\n%s", n, logs.String())
	}
	// Anything else is still a failed write, and leaves no temp file behind.
	refuse = syscall.EIO
	if _, err := s.PutFile(ctx, "clips/abc/original/eio.mp4", writeTemp(t, []byte("bytes")), "video/mp4", nil); err == nil {
		t.Fatal("PutFile succeeded although chmod failed with EIO")
	}
	if _, err := os.Stat(filepath.Join(dir, "clips", "abc", "original", "eio.mp4")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an EIO chmod left the object in place: %v", err)
	}
}
