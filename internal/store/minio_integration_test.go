//go:build integration

package store

// Integration tests against a real S3-compatible bucket: a throwaway RustFS,
// the server the test harness runs since MinIO's images left Docker Hub (run 22;
// the same digest pin as compose.e2e-bucket.yaml, an amd64 + arm64 index). The
// file keeps its name because it tests minioStore, the minio-go client.
//
//	docker network create clippet-it
//	docker run -d --rm --name clippet-it-s3 --network clippet-it \
//	  -e RUSTFS_ACCESS_KEY=itroot -e RUSTFS_SECRET_KEY=it-secret-12345 \
//	  -e RUSTFS_CHECK_UPDATE=false -e RUSTFS_CONSOLE_ENABLE=false -e RUSTFS_OBS_LOG_DIRECTORY= \
//	  rustfs/rustfs:1.0.1@sha256:1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c \
//	  rustfs --address :9000 /data
//	docker build --target testdeps -t clippet-it-deps .
//	docker run --rm --network clippet-it -v "$(pwd)":/src:ro -v clippet-gobuild:/root/.cache/go-build -w /src \
//	  -e CLIPPET_TEST_S3_ENDPOINT=http://clippet-it-s3:9000 \
//	  -e CLIPPET_TEST_S3_ACCESS_KEY=itroot -e CLIPPET_TEST_S3_SECRET_KEY=it-secret-12345 \
//	  clippet-it-deps go test -tags integration -run Integration -v ./internal/store/
//	docker rm -f clippet-it-s3; docker network rm clippet-it; docker image rm clippet-it-deps
//
// (Git Bash: "$(pwd -W)" and MSYS_NO_PATHCONV=1.) RUSTFS_CHECK_UPDATE=false
// stops RustFS asking its vendor for the latest version at start.
//
// Skipped (not failed) when CLIPPET_TEST_S3_ENDPOINT is unset. The bucket
// (CLIPPET_TEST_S3_BUCKET, default clippet-it) is created if missing; every
// test works under its own random prefix and removes it.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"clippet/internal/config"
	"clippet/internal/ids"
)

func itStore(t *testing.T) (*minioStore, config.S3Config) {
	t.Helper()
	ep := os.Getenv("CLIPPET_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("CLIPPET_TEST_S3_ENDPOINT not set")
	}
	bucket := os.Getenv("CLIPPET_TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "clippet-it"
	}
	cfg := config.S3Config{
		Endpoint: ep, Region: "us-east-1", Bucket: bucket,
		AccessKey: os.Getenv("CLIPPET_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("CLIPPET_TEST_S3_SECRET_KEY"),
		ForcePathStyle: true, OpTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute, TransferMax: 10 * time.Minute,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := s.(*minioStore)
	ctx := context.Background()
	exists, err := m.c.BucketExists(ctx, bucket)
	if err != nil {
		t.Fatalf("BucketExists: %v", err)
	}
	if !exists {
		if err := m.c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatalf("MakeBucket: %v", err)
		}
	}
	return m, cfg
}

func itPrefix(t *testing.T, m *minioStore) string {
	t.Helper()
	prefix := "it/clips/" + ids.New() + "/"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = m.DeletePrefix(ctx, prefix)
	})
	return prefix
}

func randomFile(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, data
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestIntegrationPing(t *testing.T) {
	m, cfg := itStore(t)
	ctx := context.Background()
	if err := m.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	bad := *m
	bad.cfg.Bucket = cfg.Bucket + "-does-not-exist"
	if err := bad.Ping(ctx); err == nil {
		t.Fatal("Ping on a missing bucket succeeded")
	}
}

func TestIntegrationSmallObjectRoundTrip(t *testing.T) {
	m, _ := itStore(t)
	ctx := context.Background()
	prefix := itPrefix(t, m)
	key := prefix + "original/source.mp4"
	path, data := randomFile(t, 100<<10)

	var last [2]int64
	calls := 0
	res, err := m.PutFile(ctx, key, path, "video/mp4", func(done, total int64) {
		calls++
		if done < last[0] {
			t.Errorf("progress went backwards: %d → %d", last[0], done)
		}
		last = [2]int64{done, total}
	})
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if res.Size != int64(len(data)) || res.SHA256 != sha(data) {
		t.Fatalf("PutResult = %+v", res)
	}
	if calls == 0 || last != [2]int64{int64(len(data)), int64(len(data))} {
		t.Fatalf("progress: calls=%d last=%v", calls, last)
	}

	info, err := m.Stat(ctx, key)
	if err != nil || info.Size != int64(len(data)) || info.Key != key || info.ETag == "" || info.LastModified.IsZero() {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	if _, err := m.Stat(ctx, prefix+"missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat missing = %v", err)
	}

	ranges := []struct {
		start, end int64
		want       []byte
	}{
		{0, -1, data},
		{10, 19, data[10:20]},
		{int64(len(data)) - 5, -1, data[len(data)-5:]},
		{0, 0, data[:1]},
		{0, 1 << 30, data}, // past EOF: S3 clamps
	}
	for _, r := range ranges {
		rc, err := m.GetRange(ctx, key, r.start, r.end)
		if err != nil {
			t.Fatalf("GetRange(%d,%d): %v", r.start, r.end, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(got, r.want) {
			t.Fatalf("GetRange(%d,%d) = %d bytes, %v; want %d bytes", r.start, r.end, len(got), err, len(r.want))
		}
	}
	if _, err := m.GetRange(ctx, key, int64(len(data)), -1); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRange at EOF = %v, want an unsatisfiable-range error", err)
	}
	if _, err := m.GetRange(ctx, prefix+"missing", 0, -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRange missing = %v, want ErrNotFound (eager)", err)
	}

	out := filepath.Join(t.TempDir(), "sub", "dl.bin")
	var dlDone, dlTotal int64
	if err := m.Download(ctx, key, out, func(d, tot int64) { dlDone, dlTotal = d, tot }); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatal("Download content differs")
	}
	if dlDone != int64(len(data)) || dlTotal != int64(len(data)) {
		t.Fatalf("Download progress = %d/%d, want %d/%d", dlDone, dlTotal, len(data), len(data))
	}
	if _, err := os.Stat(out + ".part"); !os.IsNotExist(err) {
		t.Fatal(".part left behind")
	}
	if err := m.Download(ctx, prefix+"missing", out+"2", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Download missing = %v", err)
	}
	if _, err := os.Stat(out + "2.part"); !os.IsNotExist(err) {
		t.Fatal(".part left behind after a failed download")
	}

	list, err := m.List(ctx, prefix)
	if err != nil || len(list) != 1 || list[0].Key != key || list[0].Size != int64(len(data)) {
		t.Fatalf("List = %+v, %v", list, err)
	}
	// A key is its own prefix.
	if n, err := m.DeletePrefix(ctx, key); err != nil || n != 1 {
		t.Fatalf("DeletePrefix(key) = %d, %v", n, err)
	}
	if _, err := m.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after delete = %v", err)
	}
	if n, err := m.DeletePrefix(ctx, prefix); err != nil || n != 0 {
		t.Fatalf("DeletePrefix(empty) = %d, %v", n, err)
	}
}

func TestIntegrationLargeMultipart(t *testing.T) {
	m, _ := itStore(t)
	ctx := context.Background()
	prefix := itPrefix(t, m)
	key := prefix + "renditions/main-abcdefgh.mp4"
	size := 40 << 20 // 3 parts of 16 MiB
	path, data := randomFile(t, size)

	var maxDone int64
	res, err := m.PutFile(ctx, key, path, "video/mp4", func(done, total int64) {
		if total != int64(size) {
			t.Errorf("total = %d", total)
		}
		if done > maxDone {
			maxDone = done
		}
	})
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if res.Size != int64(size) || res.SHA256 != sha(data) {
		t.Fatalf("PutResult = %+v, want size %d sha %s", res, size, sha(data))
	}
	if maxDone != int64(size) {
		t.Fatalf("progress reached %d, want %d", maxDone, size)
	}
	info, err := m.Stat(ctx, key)
	if err != nil || info.Size != int64(size) {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	// §20 erratum 337: 40 MiB is the PARALLEL multipart path (UploadThreads
	// unset → 6) with a Content-MD5 per part. Whether MinIO's ETag follows the
	// S3 MD5 rule is a FACT to report, not a requirement: no document this
	// project has read establishes it, and the migrate's proof falls back to a
	// read-back when it does not match. The bytes reading back equal (below)
	// is the requirement.
	t.Logf("erratum 337 fact: multipart expected=%s put-returned=%s stat=%s match=%v",
		res.ExpectedETag, res.StoredETag, info.ETag, ETagMatches(res.ExpectedETag, info.ETag))
	small := prefix + "poster/poster-abcdefgh.jpg"
	sp, sdata := randomFile(t, 70_000)
	sres, err := m.PutFile(ctx, small, sp, "image/jpeg", nil)
	if err != nil {
		t.Fatalf("PutFile small: %v", err)
	}
	sinfo, err := m.Stat(ctx, small)
	if err != nil || sinfo.Size != int64(len(sdata)) {
		t.Fatalf("Stat small = %+v, %v", sinfo, err)
	}
	t.Logf("erratum 337 fact: single PUT expected=%s put-returned=%s stat=%s match=%v",
		sres.ExpectedETag, sres.StoredETag, sinfo.ETag, ETagMatches(sres.ExpectedETag, sinfo.ETag))
	// A range across the part boundary.
	rc, err := m.GetRange(ctx, key, 16<<20-100, 16<<20+99)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, data[16<<20-100:16<<20+100]) {
		t.Fatalf("cross-part range: %d bytes, %v", len(got), err)
	}
	out := filepath.Join(t.TempDir(), "dl.bin")
	if err := m.Download(ctx, key, out, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatal("Download content differs")
	}
}

// countIncomplete lists in-progress multipart uploads under prefix (exact
// key or directory prefix, depending on what the server supports).
func countIncomplete(t *testing.T, m *minioStore, prefix string) int {
	t.Helper()
	n := 0
	for u := range m.c.ListIncompleteUploads(context.Background(), m.cfg.Bucket, prefix, true) {
		if u.Err != nil {
			t.Fatal(u.Err)
		}
		n++
	}
	return n
}

// abandonUpload starts a multipart upload for key and uploads one part,
// then walks away — the state an aborted worker attempt can leave.
func abandonUpload(t *testing.T, m *minioStore, key string) {
	t.Helper()
	ctx := context.Background()
	core := minio.Core{Client: m.c}
	uploadID, err := core.NewMultipartUpload(ctx, m.cfg.Bucket, key, minio.PutObjectOptions{})
	if err != nil {
		t.Fatalf("NewMultipartUpload: %v", err)
	}
	part := make([]byte, 5<<20)
	if _, err := core.PutObjectPart(ctx, m.cfg.Bucket, key, uploadID, 1, bytes.NewReader(part), int64(len(part)), minio.PutObjectPartOptions{}); err != nil {
		t.Fatalf("PutObjectPart: %v", err)
	}
}

// TestIntegrationDeletePrefixAbortsIncomplete: an abandoned multipart upload
// is invisible to a plain list but is aborted by DeletePrefix — by exact key
// on every provider, and by clip prefix where the server lists uploads by
// prefix (AWS/R2/B2; MinIO does not, see abortIncomplete).
func TestIntegrationDeletePrefixAbortsIncomplete(t *testing.T) {
	m, _ := itStore(t)
	ctx := context.Background()
	prefix := itPrefix(t, m)
	small, _ := randomFile(t, 1024)
	for _, k := range []string{prefix + "original/source.mp4", prefix + "poster/poster-a.jpg"} {
		if _, err := m.PutFile(ctx, k, small, "application/octet-stream", nil); err != nil {
			t.Fatal(err)
		}
	}
	abandoned := prefix + "renditions/main-zzzzzzzz.mp4"
	abandonUpload(t, m, abandoned)
	if n := countIncomplete(t, m, abandoned); n != 1 {
		t.Fatalf("incomplete uploads for the exact key = %d, want 1", n)
	}
	byPrefix := countIncomplete(t, m, prefix) == 1
	if !byPrefix {
		t.Logf("server does not list multipart uploads by directory prefix (MinIO); the clip-wide abort is exercised on AWS/R2/B2 only")
	}

	n, err := m.DeletePrefix(ctx, prefix)
	if err != nil || n != 2 {
		t.Fatalf("DeletePrefix = %d, %v", n, err)
	}
	if list, err := m.List(ctx, prefix); err != nil || len(list) != 0 {
		t.Fatalf("List after delete = %v, %v", list, err)
	}
	if byPrefix {
		if n := countIncomplete(t, m, prefix); n != 0 {
			t.Fatalf("incomplete uploads survived the prefix-wide DeletePrefix: %d", n)
		}
	}
	// The worker's per-attempt path (§20 erratum 305): DeleteKey removes the
	// object without listing anything, and still aborts the incomplete upload
	// for that EXACT key, which is the one form every provider implements.
	if err := m.DeleteKey(ctx, abandoned); err != nil {
		t.Fatalf("DeleteKey = %v", err)
	}
	if n := countIncomplete(t, m, abandoned); n != 0 {
		t.Fatalf("incomplete upload survived DeleteKey: %d", n)
	}
	// And it removes a real object, and is not an error on a key that is gone.
	live := prefix + "renditions/main-yyyyyyyy.mp4"
	if _, err := m.PutFile(ctx, live, small, "video/mp4", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteKey(ctx, live); err != nil {
		t.Fatalf("DeleteKey(live) = %v", err)
	}
	if _, err := m.Stat(ctx, live); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after DeleteKey = %v, want ErrNotFound", err)
	}
	if err := m.DeleteKey(ctx, live); err != nil {
		t.Fatalf("DeleteKey on a key that is already gone = %v, want nil", err)
	}
}

// TestIntegrationPutFileCancelAbortsUpload: a multipart PutFile cut by its
// caller's ctx leaves no incomplete upload behind (abortUpload runs under
// its own ctx; minio-go's built-in abort would fail on the dead one).
func TestIntegrationPutFileCancelAbortsUpload(t *testing.T) {
	m, _ := itStore(t)
	prefix := itPrefix(t, m)
	key := prefix + "renditions/main-cancelled.mp4"
	path, _ := randomFile(t, 40<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := m.PutFile(ctx, key, path, "video/mp4", func(done, total int64) {
		if done > 17<<20 { // part 1 is complete, part 2 is in flight
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PutFile = %v, want context.Canceled", err)
	}
	if n := countIncomplete(t, m, key); n != 0 {
		t.Fatalf("incomplete upload left behind after a cancelled PutFile: %d", n)
	}
	if _, err := m.Stat(context.Background(), key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after cancelled PutFile = %v, want ErrNotFound", err)
	}
}
