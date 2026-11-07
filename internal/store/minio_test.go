package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"clippet/internal/config"
)

// These tests cover the parts of minio.go that do not need a bucket: client
// construction, range-header rendering, the idle watchdog, the progress
// hook, the GetRange body wrapper and error mapping. The bucket-facing
// behaviour is exercised by minio_integration_test.go (-tags integration)
// and by E2E.

func TestNewMinioStore(t *testing.T) {
	cfg := config.S3Config{Endpoint: "http://minio:9000", Region: "us-east-1", Bucket: "clips",
		AccessKey: "a", SecretKey: "b", ForcePathStyle: true, OpTimeout: 30 * time.Second}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := s.(*minioStore); !ok {
		t.Fatalf("New returned %T", s)
	}
	https, err := New(config.S3Config{Endpoint: "https://acct.r2.cloudflarestorage.com", Region: "auto", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil || https == nil {
		t.Fatalf("New(https): %v", err)
	}
	for _, ep := range []string{"", "minio:9000", "://x"} {
		if _, err := New(config.S3Config{Endpoint: ep}); err == nil {
			t.Errorf("New(%q) accepted a bad endpoint", ep)
		}
	}
}

func TestRangeHeader(t *testing.T) {
	cases := []struct {
		start, end int64
		want       string
		bad        bool
	}{
		{0, -1, "", false},
		{5, -1, "bytes=5-", false},
		{0, 99, "bytes=0-99", false},
		{7, 7, "bytes=7-7", false},
		{10, 5, "", true},
		{-1, 5, "", true},
		{0, -2, "", true},
	}
	for _, c := range cases {
		got, err := rangeHeader(c.start, c.end)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("rangeHeader(%d, %d) = %q, %v; want %q bad=%v", c.start, c.end, got, err, c.want, c.bad)
		}
	}
}

func TestWatchdogFiresWhenIdle(t *testing.T) {
	ctx, wd := newWatchdog(context.Background(), 30*time.Millisecond)
	defer wd.stop()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not fire")
	}
	if !wd.Fired() {
		t.Fatal("Fired = false after the ctx was cancelled by the timer")
	}
}

// TestWatchdogTouchKeepsAlive, on the fake clock (clock_test.go): a touch every
// 20 ms for 600 ms against a 200 ms idle timeout never fires, and the first
// 200 ms of silence after the last touch fires exactly once. A watchdog that
// ignored the injected clock fails the second half: its wall timer has not run
// at the instant the fake one says it must.
func TestWatchdogTouchKeepsAlive(t *testing.T) {
	const idle = 200 * time.Millisecond
	fc := newFakeClock()
	ctx, wd := newWatchdogOn(context.Background(), idle, fc)
	for i := 0; i < 30; i++ {
		wd.touch()
		if n := fc.Advance(20 * time.Millisecond); n != 0 {
			t.Fatalf("touch %d: the idle timer fired %d time(s) although bytes kept moving", i, n)
		}
	}
	if ctx.Err() != nil || wd.Fired() {
		t.Fatal("watchdog fired although bytes kept moving")
	}
	wd.touch()
	if n := fc.Advance(idle - time.Nanosecond); n != 0 {
		t.Fatalf("the idle timer fired %d time(s) before the idle timeout", n)
	}
	if n := fc.Advance(time.Nanosecond); n != 1 || ctx.Err() == nil || !wd.Fired() {
		t.Fatalf("at the idle timeout: %d timer(s) ran, ctx.Err()=%v, Fired=%v; want 1, canceled, true (the watchdog ignored the injected clock?)", n, ctx.Err(), wd.Fired())
	}
	wd.stop()

	// stop on a live watchdog releases the ctx and does not count as fired.
	ctx, wd = newWatchdogOn(context.Background(), idle, fc)
	wd.touch()
	wd.stop()
	if ctx.Err() == nil {
		t.Fatal("stop did not release the ctx")
	}
	if wd.Fired() {
		t.Fatal("stop must not count as fired")
	}
}

func TestWatchdogDisabled(t *testing.T) {
	ctx, wd := newWatchdog(context.Background(), 0)
	time.Sleep(30 * time.Millisecond) // sleep: a negative check (a disabled watchdog never fires); load can only lengthen it
	if ctx.Err() != nil {
		t.Fatal("disabled watchdog cancelled the ctx")
	}
	wd.touch() // no timer: must not panic
	wd.pause()
	wd.stop()
	if ctx.Err() == nil || wd.Fired() {
		t.Fatalf("after stop: err=%v fired=%v", ctx.Err(), wd.Fired())
	}
}

// TestWatchdogPauseSuspendsCountdown: a paused watchdog never fires, however
// long it waits; the next touch re-arms it.
func TestWatchdogPauseSuspendsCountdown(t *testing.T) {
	ctx, wd := newWatchdog(context.Background(), 50*time.Millisecond)
	defer wd.stop()
	wd.pause()
	time.Sleep(200 * time.Millisecond) // sleep: a negative check (paused, it never fires); load can only lengthen it
	if ctx.Err() != nil || wd.Fired() {
		t.Fatal("paused watchdog fired")
	}
	wd.touch()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("re-armed watchdog did not fire")
	}
	if !wd.Fired() {
		t.Fatal("Fired = false after the re-armed countdown ended")
	}
}

// stallReader blocks until ctx is done, like a black-holed S3 body.
type stallReader struct{ ctx context.Context }

func (s stallReader) Read([]byte) (int, error) {
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// gatedReader serves data instantly while ctx is alive and fails with
// ctx.Err() afterwards, like a minio.Object whose transfer ctx was cancelled.
type gatedReader struct {
	ctx context.Context
	r   io.Reader
}

func (c gatedReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// TestRangeBodyPausedConsumerIsNotIdle: the watchdog counts only time spent
// waiting on S3, so a consumer that stops reading for longer than the idle
// timeout (a browser with a full media buffer) resumes without an error.
func TestRangeBodyPausedConsumerIsNotIdle(t *testing.T) {
	wctx, wd := newWatchdog(context.Background(), 30*time.Millisecond)
	body := &rangeBody{r: gatedReader{wctx, strings.NewReader("clippet")}, obj: io.NopCloser(nil), wd: wd, key: "k"}
	defer body.Close()
	wd.pause() // as GetRange leaves it after the first byte
	buf := make([]byte, 3)
	if n, err := body.Read(buf); n != 3 || err != nil {
		t.Fatalf("first Read = %d, %v", n, err)
	}
	time.Sleep(150 * time.Millisecond) // sleep: > idle, with no Read in flight — the pause IS the subject
	rest, err := io.ReadAll(body)
	if err != nil || string(rest) != "ppet" {
		t.Fatalf("after the pause: %q, %v; want \"ppet\", nil", rest, err)
	}
	if wd.Fired() {
		t.Fatal("watchdog fired while no Read was waiting on S3")
	}
}

func TestRangeBodyIdleTimeout(t *testing.T) {
	wctx, wd := newWatchdog(context.Background(), 30*time.Millisecond)
	body := &rangeBody{r: stallReader{wctx}, obj: io.NopCloser(nil), wd: wd, key: "clips/x/renditions/main-a.mp4"}
	start := time.Now()
	_, err := body.Read(make([]byte, 16))
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("Read = %v, want ErrIdleTimeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("idle watchdog did not cut the stalled read")
	}
	if !strings.Contains(err.Error(), "clips/x/renditions/main-a.mp4") {
		t.Fatalf("error lacks the key: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestRangeBodyCallerCancelIsNotIdle(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	wctx, wd := newWatchdog(parent, time.Minute)
	body := &rangeBody{r: stallReader{wctx}, obj: io.NopCloser(nil), wd: wd, key: "k"}
	defer body.Close()
	go func() {
		time.Sleep(20 * time.Millisecond) // sleep: lets the Read block first; load only delays the cancel, never changes the verdict
		cancel()
	}()
	_, err := body.Read(make([]byte, 16))
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("Read = %v, want context.Canceled", err)
	}
}

func TestRangeBodyMapsNotFoundAndTouches(t *testing.T) {
	_, wd := newWatchdog(context.Background(), 0)
	body := &rangeBody{r: errReader{minio.ErrorResponse{Code: minio.NoSuchKey}}, obj: io.NopCloser(nil), wd: wd, key: "k"}
	if _, err := body.Read(make([]byte, 4)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read = %v, want ErrNotFound", err)
	}
	_ = body.Close()

	// A clean EOF passes through untouched.
	_, wd2 := newWatchdog(context.Background(), 0)
	body2 := &rangeBody{r: strings.NewReader("abc"), obj: io.NopCloser(nil), wd: wd2, key: "k"}
	defer body2.Close()
	got, err := io.ReadAll(body2)
	if err != nil || string(got) != "abc" {
		t.Fatalf("ReadAll = %q, %v", got, err)
	}
}

func TestProgressHook(t *testing.T) {
	_, wd := newWatchdog(context.Background(), 0)
	defer wd.stop()
	var calls [][2]int64
	h := &progressHook{wd: wd, total: 10, fn: func(done, total int64) { calls = append(calls, [2]int64{done, total}) }}
	for _, n := range []int{4, 4, 4, 0} { // the last chunk overshoots (a retried part) and an empty read reports nothing
		if got, err := h.Read(make([]byte, n)); got != n || err != nil {
			t.Fatalf("Read(%d) = %d, %v", n, got, err)
		}
	}
	want := [][2]int64{{4, 10}, {8, 10}, {10, 10}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}
	// nil callback is fine.
	h2 := &progressHook{wd: wd, total: 1}
	if _, err := h2.Read([]byte{1}); err != nil {
		t.Fatal(err)
	}
}

func TestMapErr(t *testing.T) {
	if mapErr("stat", "k", nil) != nil {
		t.Fatal("nil must map to nil")
	}
	if err := mapErr("stat", "k", minio.ErrorResponse{Code: minio.NoSuchKey}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("NoSuchKey = %v", err)
	}
	denied := minio.ErrorResponse{Code: minio.AccessDenied, Message: "Access Denied."}
	if err := mapErr("stat", "k", denied); errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "Access Denied") {
		t.Fatalf("AccessDenied = %v", err)
	}
	if err := mapErr("get", "k", context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Canceled = %v", err)
	}
}

func TestTransferErr(t *testing.T) {
	m := &minioStore{cfg: config.S3Config{IdleTimeout: 2 * time.Minute, TransferMax: 6 * time.Hour}}
	// Idle watchdog wins over everything.
	ctx, wd := newWatchdog(context.Background(), time.Millisecond)
	<-ctx.Done()
	err := m.transferErr("put", "k", context.Background(), wd, context.Canceled)
	if !errors.Is(err, ErrIdleTimeout) || !strings.Contains(err.Error(), "2m0s") {
		t.Fatalf("fired = %v", err)
	}
	// Absolute cap: the caller's ctx is alive, the transfer ctx expired.
	_, quiet := newWatchdog(context.Background(), 0)
	defer quiet.stop()
	err = m.transferErr("put", "k", context.Background(), quiet, context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "STORAGE_TRANSFER_MAX") {
		t.Fatalf("transfer max = %v", err)
	}
	// The caller's own deadline: reported plainly.
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err = m.transferErr("put", "k", parent, quiet, context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "STORAGE_TRANSFER_MAX") {
		t.Fatalf("caller deadline = %v", err)
	}
	// Ordinary storage errors map like everywhere else.
	err = m.transferErr("download", "k", context.Background(), quiet, minio.ErrorResponse{Code: minio.NoSuchKey})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("NoSuchKey = %v", err)
	}
}

func TestWatchedReaderTouches(t *testing.T) {
	ctx, wd := newWatchdog(context.Background(), 150*time.Millisecond)
	defer wd.stop()
	// A reader that yields one byte every 40ms for 400ms keeps the watchdog quiet.
	r := &tickReader{n: 10, every: 40 * time.Millisecond}
	got, err := io.ReadAll(&watchedReader{r: r, wd: wd})
	if err != nil || len(got) != 10 {
		t.Fatalf("ReadAll = %d bytes, %v", len(got), err)
	}
	if ctx.Err() != nil {
		t.Fatal("watchdog fired although bytes kept moving")
	}
}

type tickReader struct {
	n     int
	every time.Duration
}

func (r *tickReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.every) // sleep: a trickling S3 body IS the subject
	r.n--
	p[0] = 'x'
	return 1, nil
}

// --- the real code paths against a fake S3 endpoint --------------------------

// fakeS3 emulates just enough S3 for the transfer paths: GET answers the
// headers plus the first `first` bytes of a `size`-byte object and then
// stalls until the client goes away (a black-holed body) — or, when first
// covers the whole object, completes normally; PUT drains the body and
// answers 200 (the single-PUT path).
func fakeS3(t *testing.T, size, first int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			n, _ := io.Copy(io.Discard, r.Body)
			w.Header().Set("ETag", `"put-etag"`)
			w.Header().Set("X-Body-Bytes", strconv.FormatInt(n, 10))
			w.WriteHeader(http.StatusOK)
		case http.MethodGet, http.MethodHead:
			w.Header().Set("Content-Length", strconv.Itoa(size))
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("ETag", `"get-etag"`)
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodHead {
				return
			}
			_, _ = w.Write(bytes.Repeat([]byte("x"), first))
			w.(http.Flusher).Flush()
			if first < size {
				<-r.Context().Done()
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fakeS3Store(t *testing.T, srv *httptest.Server, idle time.Duration) *minioStore {
	t.Helper()
	s, err := New(config.S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "clips", AccessKey: "a", SecretKey: "s",
		ForcePathStyle: true, OpTimeout: 5 * time.Second, IdleTimeout: idle, TransferMax: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return s.(*minioStore)
}

// TestGetRangeIdleWatchdogEndsStalledBody: bytes stop flowing mid-body →
// the read errors with ErrIdleTimeout after IdleTimeout, with the bytes
// received so far delivered.
func TestGetRangeIdleWatchdogEndsStalledBody(t *testing.T) {
	m := fakeS3Store(t, fakeS3(t, 1000, 10), 200*time.Millisecond)
	rc, err := m.GetRange(context.Background(), "clips/x/renditions/main-a.mp4", 0, -1)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	defer rc.Close()
	start := time.Now()
	got, err := io.ReadAll(rc)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("ReadAll = %d bytes, %v; want ErrIdleTimeout", len(got), err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d bytes before the stall, want 10", len(got))
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("watchdog took %s", el)
	}
}

// TestGetRangePausedConsumerIsNotIdle: the same body through the real
// GetRange path — the consumer sits on the stream for longer than
// IdleTimeout without reading, then drains it without an error.
func TestGetRangePausedConsumerIsNotIdle(t *testing.T) {
	m := fakeS3Store(t, fakeS3(t, 64, 64), 100*time.Millisecond)
	rc, err := m.GetRange(context.Background(), "clips/x/renditions/main-a.mp4", 0, -1)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	defer rc.Close()
	var one [1]byte
	if n, err := rc.Read(one[:]); n != 1 || err != nil {
		t.Fatalf("first Read = %d, %v", n, err)
	}
	time.Sleep(400 * time.Millisecond) // sleep: the consumer pauses (the subject); S3 is fine
	rest, err := io.ReadAll(rc)
	if err != nil || len(rest) != 63 {
		t.Fatalf("ReadAll after the pause = %d bytes, %v; want 63, nil", len(rest), err)
	}
}

// TestDownloadIdleWatchdog: a stalled Download fails with ErrIdleTimeout and
// leaves neither the file nor its .part behind.
func TestDownloadIdleWatchdog(t *testing.T) {
	m := fakeS3Store(t, fakeS3(t, 1000, 10), 200*time.Millisecond)
	out := filepath.Join(t.TempDir(), "sub", "source.mp4")
	err := m.Download(context.Background(), "clips/x/original/source.mp4", out, nil)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("Download = %v, want ErrIdleTimeout", err)
	}
	for _, p := range []string{out, out + ".part"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left behind (%v)", p, err)
		}
	}
}

// TestPutFileSinglePut: a small object goes up in one PUT; the result carries
// the file's sha256 and progress reaches the total.
func TestPutFileSinglePut(t *testing.T) {
	m := fakeS3Store(t, fakeS3(t, 0, 0), time.Minute)
	data := bytes.Repeat([]byte("clippet"), 10000)
	path := writeTemp(t, data)
	var last, total int64
	res, err := m.PutFile(context.Background(), "clips/x/poster/poster-a.jpg", path, "image/jpeg", func(d, tot int64) { last, total = d, tot })
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	sum := sha256.Sum256(data)
	if res.Size != int64(len(data)) || res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("PutResult = %+v", res)
	}
	if last != int64(len(data)) || total != int64(len(data)) {
		t.Fatalf("progress ended at %d/%d, want %d", last, total, len(data))
	}
	if _, err := m.PutFile(context.Background(), "k", filepath.Join(t.TempDir(), "missing"), "x", nil); err == nil {
		t.Fatal("PutFile of a missing file succeeded")
	}
}

// TestPutFileHonoursCallerCtx: a cancelled caller ctx is reported as
// context.Canceled, not as an idle timeout.
func TestPutFileHonoursCallerCtx(t *testing.T) {
	m := fakeS3Store(t, fakeS3(t, 0, 0), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := m.PutFile(ctx, "clips/x/poster/poster-a.jpg", writeTemp(t, []byte("x")), "image/jpeg", nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("PutFile with a dead ctx = %v", err)
	}
}

// TestPingClassifiesWithoutLeakingTheEndpoint: Ping's error keeps every
// detail for the operator's log, but its HealthClass — the ONLY thing the
// anonymous /healthz and /readyz print (§20 erratum 31) — is a bounded word
// that names neither S3_ENDPOINT nor S3_BUCKET.
func TestPingClassifiesWithoutLeakingTheEndpoint(t *testing.T) {
	var status atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(srv.Close)
	m := fakeS3Store(t, srv, time.Minute)

	status.Store(http.StatusOK)
	if err := m.Ping(context.Background()); err != nil {
		t.Fatalf("Ping of a healthy bucket: %v", err)
	}
	for _, c := range []struct {
		name  string
		code  int
		class string
	}{
		{"missing bucket", http.StatusNotFound, PingBucketMissing},
		{"bad credentials", http.StatusForbidden, PingDenied},
		{"unauthorized", http.StatusUnauthorized, PingDenied},
		{"other S3 error", http.StatusConflict, PingUnavailable},
	} {
		status.Store(int64(c.code))
		err := m.Ping(context.Background())
		var pe *PingError
		if !errors.As(err, &pe) {
			t.Fatalf("%s: Ping = %v (%T), want a *PingError", c.name, err, err)
		}
		if pe.HealthClass() != c.class {
			t.Errorf("%s: class = %q, want %q", c.name, pe.HealthClass(), c.class)
		}
		if strings.Contains(pe.HealthClass(), srv.URL) || strings.Contains(pe.HealthClass(), "clips") {
			t.Errorf("%s: class %q leaks the endpoint or the bucket", c.name, pe.HealthClass())
		}
	}
}

// TestPingClass pins the classes for the error shapes minio-go returns; the
// transport failure is the leaky one (*url.Error prints the request URL).
func TestPingClass(t *testing.T) {
	leaky := &url.Error{Op: "Get", URL: "https://acc0unt.r2.cloudflarestorage.com/myclips/?location=",
		Err: errors.New("dial tcp 1.2.3.4:443: connect: connection refused")}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"transport", leaky, PingUnreachable},
		{"deadline", context.DeadlineExceeded, PingTimeout},
		{"cancelled", context.Canceled, PingTimeout},
		{"no such bucket", minio.ErrorResponse{Code: "NoSuchBucket", StatusCode: http.StatusNotFound}, PingBucketMissing},
		{"denied", minio.ErrorResponse{Code: minio.AccessDenied, StatusCode: http.StatusForbidden}, PingDenied},
		{"bad key", minio.ErrorResponse{Code: "InvalidAccessKeyId", StatusCode: http.StatusForbidden}, PingDenied},
		{"other S3", minio.ErrorResponse{Code: "InternalError", StatusCode: http.StatusInternalServerError}, PingUnavailable},
		{"unknown", errors.New("something else entirely"), PingUnavailable},
	} {
		if got := pingClass(c.err); got != c.want {
			t.Errorf("%s: pingClass = %q, want %q", c.name, got, c.want)
		}
	}
	// The operator keeps the detail the probe must not print.
	pe := &PingError{Class: pingClass(leaky), Err: fmt.Errorf("store: ping: %w", leaky)}
	if !strings.Contains(pe.Error(), "acc0unt") || strings.Contains(pe.HealthClass(), "acc0unt") {
		t.Fatalf("error %q / class %q", pe.Error(), pe.HealthClass())
	}
	if (&PingError{Err: errors.New("x")}).HealthClass() != PingUnavailable {
		t.Fatal("an empty class must render as unavailable")
	}
}

// TestPresignGet (§20 erratum 278) pins the four things the media redirect
// depends on: the URL is on the bucket's PUBLIC origin, it carries a SigV4
// query signature with the expiry we asked for, it carries both `response-*`
// overrides so the bucket answers with the row's type rather than the object's
// — and minting it makes NO network call, which is what keeps §15's outbound
// list unchanged in redirect mode.
func TestPresignGet(t *testing.T) {
	// Neither host resolves from a test runner, which is the point: if
	// presigning contacted anything, this would fail rather than pass quietly.
	// TestPresignMakesNoNetworkCall below is the sharper form of the same claim
	// — an unroutable `.invalid` endpoint, timed.
	cfg := config.S3Config{
		Endpoint: "http://minio:9000", PresignEndpoint: "https://clips.example.test",
		Region: "us-east-1", Bucket: "clips", AccessKey: "AKIAEXAMPLE", SecretKey: "s3cr3t-example-key",
		ForcePathStyle: true, OpTimeout: time.Second,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := s.(*minioStore)
	if m.signer == m.c {
		t.Fatal("a separate presign endpoint did not get its own signing client")
	}
	const key = "clips/abcdefghijkl/renditions/social-1a2b3c4d.mp4"
	raw, err := m.PresignGet(context.Background(), key, 42*time.Minute, "video/mp4", "inline")
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	if u.Scheme != "https" || u.Host != "clips.example.test" {
		t.Errorf("presigned on %s://%s, want the PUBLIC origin (SigV4 signs the host)", u.Scheme, u.Host)
	}
	if !strings.Contains(u.Path, key) {
		t.Errorf("path %q does not carry the key", u.Path)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"X-Amz-Algorithm":              "AWS4-HMAC-SHA256",
		"X-Amz-Expires":                strconv.Itoa(int((42 * time.Minute).Seconds())),
		"response-content-type":        "video/mp4",
		"response-content-disposition": "inline",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if len(q.Get("X-Amz-Signature")) != 64 {
		t.Errorf("X-Amz-Signature = %q, want 64 hex characters", q.Get("X-Amz-Signature"))
	}
	if !strings.Contains(q.Get("X-Amz-Credential"), cfg.AccessKey) {
		t.Errorf("X-Amz-Credential = %q, want the access key id", q.Get("X-Amz-Credential"))
	}
	// The SECRET is never in the URL — a signature is HMAC over it, not it.
	if strings.Contains(raw, cfg.SecretKey) {
		t.Error("the presigned URL carries the secret key")
	}
	// A download asks for the attachment disposition, and nothing else moves.
	att, err := m.PresignGet(context.Background(), key, time.Hour, "video/mp4", `attachment; filename="x.mp4"`)
	if err != nil {
		t.Fatalf("PresignGet(attachment): %v", err)
	}
	if got, err := url.Parse(att); err != nil {
		t.Fatal(err)
	} else if got.Query().Get("response-content-disposition") != `attachment; filename="x.mp4"` {
		t.Errorf("download disposition = %q", got.Query().Get("response-content-disposition"))
	}
	// With one origin for both, the signer IS the client — `proxy` mode and
	// every R2/B2/S3 deployment allocate nothing extra.
	same, err := New(config.S3Config{Endpoint: "https://acct.r2.cloudflarestorage.com",
		PresignEndpoint: "https://acct.r2.cloudflarestorage.com", Region: "auto", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if ms := same.(*minioStore); ms.signer != ms.c {
		t.Error("one origin built two clients")
	}
	// A malformed presign endpoint is refused at construction, like the main one.
	if _, err := New(config.S3Config{Endpoint: "http://minio:9000", PresignEndpoint: "nonsense", Region: "us-east-1"}); err == nil {
		t.Error("New accepted a malformed MEDIA_PRESIGN_ENDPOINT")
	}
}

// TestPresignMakesNoNetworkCall is §15's guarantee, measured (§20 erratum 278).
// A SigV4 query signature is HMAC over the canonical request, and `newClient`
// pins Region from configuration so minio-go never asks the bucket where it
// lives — so a presign against a host that cannot resolve must still succeed,
// instantly. If either of those ever stops being true this hangs on DNS and
// then fails, instead of quietly adding a destination to the app's egress.
func TestPresignMakesNoNetworkCall(t *testing.T) {
	s, err := New(config.S3Config{
		Endpoint: "http://this-host-does-not-resolve.invalid:9000", Region: "us-east-1",
		Bucket: "clips", AccessKey: "AKIAEXAMPLE", SecretKey: "s3cr3t-example-key",
		ForcePathStyle: true, OpTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	raw, err := s.PresignGet(context.Background(), "clips/x/renditions/main-1a2b3c4d.mp4", time.Hour, "video/mp4", "inline")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("PresignGet against an unroutable host: %v", err)
	}
	if !strings.Contains(raw, "this-host-does-not-resolve.invalid:9000") || !strings.Contains(raw, "X-Amz-Signature=") {
		t.Errorf("presigned %q", raw)
	}
	if elapsed > time.Second {
		t.Errorf("signing took %s: something went to the network", elapsed)
	}
	// And MediaOrigin, which is what the CSP is built from, reads the origin
	// straight off it — same call, same absence of a socket.
	origin, err := MediaOrigin(context.Background(), s, time.Hour)
	if err != nil {
		t.Fatalf("MediaOrigin: %v", err)
	}
	if origin != "http://this-host-does-not-resolve.invalid:9000" {
		t.Errorf("MediaOrigin = %q", origin)
	}
}

// TestMediaOriginFollowsTheBucketLookup is the store half of §20 erratum 278's
// amendment: with virtual-host lookup the BUCKET is in the host, and asking the
// signer is the only way to know it without duplicating minio-go's rule.
func TestMediaOriginFollowsTheBucketLookup(t *testing.T) {
	base := config.S3Config{Endpoint: "https://acct.example.test", Region: "auto", Bucket: "clips",
		AccessKey: "AKIAEXAMPLE", SecretKey: "s3cr3t-example-key", OpTimeout: time.Second}
	for _, tc := range []struct {
		pathStyle bool
		want      string
	}{
		{false, "https://clips.acct.example.test"}, // R2, B2, S3 — and the binary's own default
		{true, "https://acct.example.test"},        // the bundled MinIO, which compose pins
	} {
		cfg := base
		cfg.ForcePathStyle = tc.pathStyle
		s, err := New(cfg)
		if err != nil {
			t.Fatalf("New(path_style=%v): %v", tc.pathStyle, err)
		}
		got, err := MediaOrigin(context.Background(), s, time.Hour)
		if err != nil {
			t.Fatalf("MediaOrigin(path_style=%v): %v", tc.pathStyle, err)
		}
		if got != tc.want {
			t.Errorf("S3_FORCE_PATH_STYLE=%v: origin = %q, want %q", tc.pathStyle, got, tc.want)
		}
		// The probe key is never a real object and never reached: only its
		// origin is read, so nothing can race a sweeper over it.
		if !strings.Contains(MediaOriginProbeKey, "probe") {
			t.Errorf("the probe key does not look like one: %q", MediaOriginProbeKey)
		}
	}
	// A presign endpoint of its own is signed the same way — the bucket still
	// lands in the host when the lookup is virtual-host.
	cfg := base
	cfg.PresignEndpoint = "https://cdn.example.test"
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := MediaOrigin(context.Background(), s, time.Hour); err != nil || got != "https://clips.cdn.example.test" {
		t.Errorf("MEDIA_PRESIGN_ENDPOINT + virtual-host: %q, %v", got, err)
	}
}

// TestMediaOriginRefusesABackendWithoutOne: `fs` cannot sign, so it cannot
// answer this either — and the caller must see the refusal rather than an
// empty origin that would quietly narrow the CSP.
func TestMediaOriginRefusesABackendWithoutOne(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFS(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if origin, err := MediaOrigin(context.Background(), fs, time.Hour); err == nil || origin != "" {
		t.Fatalf("MediaOrigin on fs = %q, %v", origin, err)
	}
}

// TestEnsureBucket (§20 erratum 346): a missing bucket is created with one PUT
// and reported as created; an existing one is reported as such with no PUT; a
// bucket someone else owns is an error; and no request ever names a policy —
// init makes a bucket and leaves its access exactly as the provider made it.
func TestEnsureBucket(t *testing.T) {
	var head atomic.Int64
	var put atomic.Int64 // the status a PUT answers; 0 = 200
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(int(head.Load()))
		case http.MethodPut:
			if code := put.Load(); code != 0 {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(int(code))
				fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>BucketAlreadyExists</Code><Message>taken</Message><BucketName>clips</BucketName></Error>`)
				return
			}
			head.Store(http.StatusOK)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	m := fakeS3Store(t, srv, time.Minute)
	ctx := context.Background()

	head.Store(http.StatusNotFound)
	created, err := m.EnsureBucket(ctx)
	if err != nil || !created {
		t.Fatalf("EnsureBucket on a missing bucket = %v, %v; want created", created, err)
	}
	created, err = m.EnsureBucket(ctx)
	if err != nil || created {
		t.Fatalf("EnsureBucket on an existing bucket = %v, %v; want not created, no error", created, err)
	}
	puts := 0
	mu.Lock()
	for _, s := range seen {
		if strings.HasPrefix(s, "PUT ") {
			puts++
		}
		if strings.Contains(s, "policy") {
			t.Errorf("EnsureBucket sent a policy request: %s", s)
		}
	}
	mu.Unlock()
	if puts != 1 {
		t.Fatalf("EnsureBucket made %d PUTs, want exactly 1: %v", puts, seen)
	}
	// Somebody else's bucket of that name: an error, never a silent success.
	head.Store(http.StatusNotFound)
	put.Store(http.StatusConflict)
	if created, err := m.EnsureBucket(ctx); err == nil || created {
		t.Fatalf("EnsureBucket on a name owned elsewhere = %v, %v; want an error", created, err)
	}
	// A HEAD that fails outright is an error too, and nothing is created.
	head.Store(http.StatusForbidden)
	put.Store(0)
	if created, err := m.EnsureBucket(ctx); err == nil || created {
		t.Fatalf("EnsureBucket with a refused HEAD = %v, %v; want an error", created, err)
	}
}
