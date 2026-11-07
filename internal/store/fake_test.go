package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFakePutGetStatListDelete(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	data := []byte("hello clippet, this is an original")
	path := writeTemp(t, data)
	var progressCalls int
	var lastDone, lastTotal int64
	res, err := f.PutFile(ctx, "clips/x/original/source.mp4", path, "video/mp4", func(done, total int64) {
		progressCalls++
		lastDone, lastTotal = done, total
	})
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	sum := sha256.Sum256(data)
	if res.Size != int64(len(data)) || res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("PutResult = %+v", res)
	}
	if progressCalls == 0 || lastDone != int64(len(data)) || lastTotal != int64(len(data)) {
		t.Fatalf("progress: calls=%d done=%d total=%d", progressCalls, lastDone, lastTotal)
	}
	info, err := f.Stat(ctx, "clips/x/original/source.mp4")
	if err != nil || info.Size != int64(len(data)) || info.ETag != res.ExpectedETag || !res.Verified() || info.Key != "clips/x/original/source.mp4" {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	if _, err := f.Stat(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat missing: %v", err)
	}
	f.Put("clips/x/renditions/main-a.mp4", []byte("rend"), "video/mp4")
	f.Put("clips/y/original/source.mp4", []byte("other"), "video/mp4")
	list, err := f.List(ctx, "clips/x/")
	if err != nil || len(list) != 2 || list[0].Key != "clips/x/original/source.mp4" || list[1].Key != "clips/x/renditions/main-a.mp4" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	// Download (with the progress hook reaching done == total)
	out := filepath.Join(t.TempDir(), "sub", "dl.mp4")
	var dlDone, dlTotal int64
	if err := f.Download(ctx, "clips/x/original/source.mp4", out, func(d, tot int64) { dlDone, dlTotal = d, tot }); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(out); string(got) != string(data) {
		t.Fatalf("Download content = %q", got)
	}
	if dlDone != int64(len(data)) || dlTotal != int64(len(data)) {
		t.Fatalf("Download progress = %d/%d, want %d/%d", dlDone, dlTotal, len(data), len(data))
	}
	if err := f.Download(ctx, "nope", out, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Download missing: %v", err)
	}
	// GetRange
	rc, err := f.GetRange(ctx, "clips/x/original/source.mp4", 6, 12)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "clippet" {
		t.Fatalf("GetRange 6-12 = %q", b)
	}
	rc, _ = f.GetRange(ctx, "clips/x/original/source.mp4", 30, -1)
	b, _ = io.ReadAll(rc)
	if string(b) != "inal" {
		t.Fatalf("GetRange 30- = %q", b)
	}
	rc, _ = f.GetRange(ctx, "clips/x/original/source.mp4", 0, 1_000_000)
	b, _ = io.ReadAll(rc)
	if len(b) != len(data) {
		t.Fatalf("GetRange 0-huge len = %d", len(b))
	}
	if _, err := f.GetRange(ctx, "clips/x/original/source.mp4", int64(len(data)), -1); err == nil {
		t.Fatal("GetRange at EOF should error")
	}
	if _, err := f.GetRange(ctx, "missing", 0, -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRange missing: %v", err)
	}
	// DeletePrefix
	n, err := f.DeletePrefix(ctx, "clips/x/")
	if err != nil || n != 2 {
		t.Fatalf("DeletePrefix = %d, %v", n, err)
	}
	if got := f.Deletes(); !reflect.DeepEqual(got, []string{"clips/x/original/source.mp4", "clips/x/renditions/main-a.mp4"}) {
		t.Fatalf("Deletes = %v", got)
	}
	if list, _ := f.List(ctx, "clips/x/"); len(list) != 0 {
		t.Fatalf("prefix not empty after delete: %v", list)
	}
	if keys := f.Keys(); !reflect.DeepEqual(keys, []string{"clips/y/original/source.mp4"}) {
		t.Fatalf("Keys = %v", keys)
	}
	if err := f.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	// Calls order.
	want := []string{
		"PutFile clips/x/original/source.mp4", "Stat clips/x/original/source.mp4", "Stat missing", "List clips/x/",
		"Download clips/x/original/source.mp4", "Download nope",
		"GetRange clips/x/original/source.mp4", "GetRange clips/x/original/source.mp4", "GetRange clips/x/original/source.mp4",
		"GetRange clips/x/original/source.mp4", "GetRange missing",
		"DeletePrefix clips/x/", "List clips/x/", "Ping ",
	}
	if !reflect.DeepEqual(f.CallsSnapshot(), want) {
		t.Fatalf("Calls = %q\nwant  %q", f.Calls, want)
	}
}

func TestFakeFailNext(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	boom := errors.New("boom")
	f.FailNext("Ping", boom)
	if err := f.Ping(ctx); !errors.Is(err, boom) {
		t.Fatalf("first Ping = %v, want boom", err)
	}
	if err := f.Ping(ctx); err != nil {
		t.Fatalf("second Ping = %v, want nil", err)
	}
	f.FailNext("PutFile", boom)
	if _, err := f.PutFile(ctx, "k", writeTemp(t, []byte("x")), "v", nil); !errors.Is(err, boom) {
		t.Fatalf("PutFile = %v", err)
	}
	if _, ok := f.Get("k"); ok {
		t.Fatal("failed PutFile stored the object")
	}
	f.FailNext("DeletePrefix", boom)
	if _, err := f.DeletePrefix(ctx, "k"); !errors.Is(err, boom) {
		t.Fatalf("DeletePrefix = %v", err)
	}
	// The call is still recorded even when it fails.
	if calls := f.CallsSnapshot(); len(calls) != 4 || calls[3] != "DeletePrefix k" {
		t.Fatalf("Calls = %v", calls)
	}
}

func TestFakeHookOrderingProbe(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	var mu sync.Mutex
	var seen []string
	f.Hook = func(ctx context.Context, op, key string) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, op+":"+key)
		return nil
	}
	f.Put("p/a", []byte("a"), "")
	_ = f.Ping(ctx)
	_, _ = f.List(ctx, "p/")
	_, _ = f.DeletePrefix(ctx, "p/")
	if want := []string{"Ping:", "List:p/", "DeletePrefix:p/"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("hook saw %v, want %v", seen, want)
	}
	// A hook error is returned unchanged and nothing happens.
	hookErr := errors.New("hook says no")
	f.Hook = func(context.Context, string, string) error { return hookErr }
	f.Put("p/b", []byte("b"), "")
	if n, err := f.DeletePrefix(ctx, "p/"); !errors.Is(err, hookErr) || n != 0 {
		t.Fatalf("DeletePrefix with failing hook = %d, %v", n, err)
	}
	if _, ok := f.Get("p/b"); !ok {
		t.Fatal("object deleted despite hook error")
	}
}

// TestFakeSlowHook is the timeout idiom every lifecycle/httpapi test uses: a
// hook blocking on ctx.Done() makes the op return ctx.Err() when the caller's
// deadline passes.
func TestFakeSlowHook(t *testing.T) {
	f := NewFake()
	f.Hook = func(ctx context.Context, op, key string) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := f.DeletePrefix(ctx, "p/")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("slow hook did not honour ctx")
	}
	// A cancelled ctx without a hook is also rejected up front.
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	f.Hook = nil
	if err := f.Ping(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ping with cancelled ctx = %v", err)
	}
}

func TestFakeGetRangeHonoursCtx(t *testing.T) {
	f := NewFake()
	f.Put("k", make([]byte, 1<<20), "")
	ctx, cancel := context.WithCancel(context.Background())
	rc, err := f.GetRange(ctx, "k", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1024)
	if _, err := rc.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}
	cancel()
	if _, err := rc.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel = %v", err)
	}
}

func TestFakeConcurrentSafe(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "k" + string(rune('a'+i))
			f.Put(key, []byte("x"), "")
			_, _ = f.Stat(ctx, key)
			_, _ = f.List(ctx, "k")
			_, _ = f.DeletePrefix(ctx, key)
			_ = f.CallsSnapshot()
			_ = f.Deletes()
		}(i)
	}
	wg.Wait()
	if len(f.Deletes()) != 20 {
		t.Fatalf("deletes = %d", len(f.Deletes()))
	}
}

// TestFakePresignIsDeterministic (§20 erratum 278): a handler test asserts the
// exact Location a media redirect answers with, so the Fake's URL has to be a
// pure function of what was asked for — and it has to LOOK like the real
// thing, or an assertion written against it proves nothing.
func TestFakePresignIsDeterministic(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	a, err := f.PresignGet(ctx, "clips/x/renditions/r.mp4", time.Hour, "video/mp4", "inline")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.PresignGet(ctx, "clips/x/renditions/r.mp4", time.Hour, "video/mp4", "inline")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("two presigns of the same object differ:\n%s\n%s", a, b)
	}
	if a != FakePresignURL("clips/x/renditions/r.mp4", time.Hour, "video/mp4", "inline") {
		t.Fatalf("PresignGet does not agree with FakePresignURL: %s", a)
	}
	u, err := url.Parse(a)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != FakePresignHost {
		t.Errorf("host %q, want %q", u.Host, FakePresignHost)
	}
	for _, k := range []string{"X-Amz-Algorithm", "X-Amz-Expires", "X-Amz-Signature", "response-content-type", "response-content-disposition"} {
		if u.Query().Get(k) == "" {
			t.Errorf("the fake presign has no %s: %s", k, a)
		}
	}
	// A different object gets a different URL, and the call is recorded like
	// every other op so a test can assert the media path presigned.
	other, err := f.PresignGet(ctx, "clips/y/renditions/r.mp4", time.Hour, "video/mp4", "inline")
	if err != nil {
		t.Fatal(err)
	}
	if other == a {
		t.Error("two different objects presign identically")
	}
	var n int
	for _, c := range f.CallsSnapshot() {
		if strings.HasPrefix(c, "PresignGet ") {
			n++
		}
	}
	if n != 3 {
		t.Errorf("recorded %d PresignGet calls, want 3: %v", n, f.CallsSnapshot())
	}
	// FailNext works on it, so a handler test can drive the 503 branch.
	f.FailNext("PresignGet", errors.New("nope"))
	if _, err := f.PresignGet(ctx, "clips/x/renditions/r.mp4", time.Hour, "video/mp4", "inline"); err == nil {
		t.Error("FailNext did not reach PresignGet")
	}
}

// TestDeleteKeyIgnoresTheListing is the store half of the same erratum: the
// contract DeleteKey adds is that it removes the object and not what a listing
// says exists, and that a key already gone is not an error.
func TestDeleteKeyIgnoresTheListing(t *testing.T) {
	f := NewFake()
	f.Put("clips/x/renditions/main-abcd1234.mp4", []byte("bytes"), "video/mp4")
	f.Unlisted("clips/x/renditions/main-abcd1234.mp4")

	if n, err := f.DeletePrefix(context.Background(), "clips/x/"); err != nil || n != 0 {
		t.Fatalf("DeletePrefix over an unlisted object = (%d, %v), want (0, nil)", n, err)
	}
	if _, ok := f.Get("clips/x/renditions/main-abcd1234.mp4"); !ok {
		t.Fatal("the unlisted object must survive a list-driven delete; the Fake is not modelling the bucket")
	}
	if err := f.DeleteKey(context.Background(), "clips/x/renditions/main-abcd1234.mp4"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	if _, ok := f.Get("clips/x/renditions/main-abcd1234.mp4"); ok {
		t.Fatal("DeleteKey must remove the object whatever the listing shows")
	}
	if err := f.DeleteKey(context.Background(), "clips/x/renditions/main-abcd1234.mp4"); err != nil {
		t.Fatalf("DeleteKey on a key that is already gone = %v, want nil", err)
	}
}
