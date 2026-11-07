package store

// minio_parallel_test.go — the parallel multipart path of PutFile against a
// fake bucket that speaks just enough of the multipart API (§20 erratum 337):
// parts really go up at the same time, each carries a Content-MD5 that
// matches its bytes, the ETag predicted locally is the one the bucket
// computes by the documented rule, and a slow uplink with every part buffer
// in flight is not mistaken for an idle transfer.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/config"
)

// multipartS3 is the fake bucket. slowRead > 0 makes every part body arrive
// in 1 MiB reads with that pause between them — a slow uplink.
type multipartS3 struct {
	t        *testing.T
	slowRead time.Duration
	holdFor  time.Duration // how long a part waits for a second one (see ServeHTTP)

	mu        sync.Mutex
	inflight  int
	maxIn     int
	wake      *sync.Cond
	parts     map[int][]byte
	md5s      map[int]string // the Content-MD5 each part carried, hex
	missing   int            // parts that came without a Content-MD5
	badMD5    int            // parts whose Content-MD5 did not match their bytes
	etag      string         // the completed object's ETag
	object    []byte
	singlePUT int
}

func newMultipartS3(t *testing.T, slowRead time.Duration) (*multipartS3, *httptest.Server) {
	m := &multipartS3{t: t, slowRead: slowRead, holdFor: 2 * time.Second, parts: map[int][]byte{}, md5s: map[int]string{}}
	m.wake = sync.NewCond(&m.mu)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return m, srv
}

// body decodes the request body: aws-chunked when minio-go streams a signed
// payload over plain HTTP, raw otherwise.
func (m *multipartS3) body(r *http.Request) []byte {
	var raw io.Reader = r.Body
	if m.slowRead > 0 {
		raw = &slowReader{r: r.Body, pause: m.slowRead}
	}
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		b, _ := io.ReadAll(raw)
		return b
	}
	br := bufio.NewReaderSize(raw, 1<<20)
	var out []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return out
		}
		hexSize, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		n, err := strconv.ParseInt(hexSize, 16, 64)
		if err != nil || n == 0 {
			return out
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return out
		}
		out = append(out, chunk...)
		_, _ = br.Discard(2) // CRLF
	}
}

func (m *multipartS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		fmt.Fprint(w, `<InitiateMultipartUploadResult><Bucket>clips</Bucket><Key>k</Key><UploadId>up1</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPut && q.Get("partNumber") != "":
		n, _ := strconv.Atoi(q.Get("partNumber"))
		m.mu.Lock()
		m.inflight++
		m.maxIn = max(m.maxIn, m.inflight)
		m.wake.Broadcast()
		// Hold the part until two have been in flight at once, or holdFor
		// passes: on the parallel path the wait ends as soon as the second
		// part arrives, on the sequential path it always times out and maxIn
		// stays 1.
		expired := false
		timer := time.AfterFunc(m.holdFor, func() {
			m.mu.Lock()
			expired = true
			m.wake.Broadcast()
			m.mu.Unlock()
		})
		for m.maxIn < 2 && !expired {
			m.wake.Wait()
		}
		timer.Stop()
		m.mu.Unlock()
		b := m.body(r)
		sum := md5.Sum(b)
		m.mu.Lock()
		m.inflight--
		m.parts[n] = b
		if c := r.Header.Get("Content-Md5"); c == "" {
			m.missing++
		} else if c != base64.StdEncoding.EncodeToString(sum[:]) {
			m.badMD5++
		}
		m.md5s[n] = hex.EncodeToString(sum[:])
		m.mu.Unlock()
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		_, _ = io.Copy(io.Discard, r.Body)
		m.mu.Lock()
		nums := make([]int, 0, len(m.parts))
		for n := range m.parts {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		var cat, obj []byte
		for _, n := range nums {
			s := md5.Sum(m.parts[n])
			cat = append(cat, s[:]...)
			obj = append(obj, m.parts[n]...)
		}
		s := md5.Sum(cat)
		m.etag = hex.EncodeToString(s[:]) + "-" + strconv.Itoa(len(nums))
		m.object = obj
		etag := m.etag
		m.mu.Unlock()
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>clips</Bucket><Key>k</Key><ETag>"%s"</ETag></CompleteMultipartUploadResult>`, etag)
	case r.Method == http.MethodPut:
		b := m.body(r)
		s := md5.Sum(b)
		m.mu.Lock()
		m.singlePUT++
		m.object = b
		m.etag = hex.EncodeToString(s[:])
		if r.Header.Get("Content-Md5") == "" {
			m.missing++
		}
		m.mu.Unlock()
		w.Header().Set("ETag", `"`+hex.EncodeToString(s[:])+`"`)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// slowReader takes up to 1 MiB per Read — reading until it has it, since the
// server's own body reader hands out a few KiB at a time — pausing before
// each: an uplink of about 1 MiB per pause.
type slowReader struct {
	r     io.Reader
	pause time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.pause) // sleep: the slow uplink IS the subject; the watchdog under test runs on the wall clock
	if len(p) > 1<<20 {
		p = p[:1<<20]
	}
	n, err := io.ReadFull(s.r, p)
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	if n > 0 && err == io.EOF {
		err = nil // the next Read reports the end
	}
	return n, err
}

func parallelStore(t *testing.T, srv *httptest.Server, idle time.Duration, threads int) *minioStore {
	t.Helper()
	s, err := New(config.S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "clips", AccessKey: "a", SecretKey: "s",
		ForcePathStyle: true, OpTimeout: 5 * time.Second, IdleTimeout: idle, TransferMax: time.Minute, UploadThreads: threads})
	if err != nil {
		t.Fatal(err)
	}
	return s.(*minioStore)
}

// pipeListener hands the server the far end of an in-memory net.Pipe for
// every connection the client dials. A pipe has NO buffer: a write blocks
// until the reader takes it, so the client's body reads track the server's
// reads exactly. Over loopback TCP the kernel absorbs megabytes of a part
// before the server reads any of it, which would make a slow server look
// like an idle client for reasons that are the test box's and not the code's.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

func (l *pipeListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// pipeStore is parallelStore over pipes to h.
func pipeStore(t *testing.T, h http.Handler, idle time.Duration, threads int) *minioStore {
	t.Helper()
	ln := newPipeListener()
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	cfg := config.S3Config{Endpoint: "http" + "://pipe.invalid", Region: "us-east-1", Bucket: "clips", AccessKey: "a", SecretKey: "s",
		ForcePathStyle: true, OpTimeout: 5 * time.Second, IdleTimeout: idle, TransferMax: time.Minute, UploadThreads: threads}
	c, err := newClientOn(cfg, cfg.Endpoint, &http.Transport{DialContext: ln.dial, ResponseHeaderTimeout: cfg.OpTimeout})
	if err != nil {
		t.Fatal(err)
	}
	return &minioStore{c: c, signer: c, cfg: cfg}
}

func randomTemp(t *testing.T, n int) (string, []byte) {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "obj.bin")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, data
}

// TestPutFileParallelParts: three parts go up at the same time, each with a
// Content-MD5 that matches it, the object reassembles byte for byte, and the
// locally predicted ETag is the one the bucket computed — Verified.
func TestPutFileParallelParts(t *testing.T) {
	fake, srv := newMultipartS3(t, 0)
	m := parallelStore(t, srv, time.Minute, 6)
	size := 2*partSize + 1000
	path, data := randomTemp(t, size)
	var last int64
	var mu sync.Mutex
	res, err := m.PutFile(context.Background(), "clips/x/renditions/main-a.mp4", path, "video/mp4", func(d, _ int64) {
		mu.Lock()
		last = max(last, d)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.maxIn < 2 {
		t.Fatalf("at most %d part(s) were in flight at once; the parallel path did not run", fake.maxIn)
	}
	if len(fake.parts) != 3 || len(fake.parts[1]) != partSize || len(fake.parts[2]) != partSize || len(fake.parts[3]) != 1000 {
		t.Fatalf("parts: %d (sizes %d/%d/%d)", len(fake.parts), len(fake.parts[1]), len(fake.parts[2]), len(fake.parts[3]))
	}
	if fake.missing != 0 || fake.badMD5 != 0 {
		t.Fatalf("Content-MD5: %d part(s) without one, %d wrong", fake.missing, fake.badMD5)
	}
	if !bytes.Equal(fake.object, data) {
		t.Fatal("the reassembled object differs from the file")
	}
	sum := sha256.Sum256(data)
	if res.Size != int64(size) || res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("PutResult = %+v", res)
	}
	if res.ExpectedETag != fake.etag || res.StoredETag != fake.etag || !res.Verified() {
		t.Fatalf("ETag: expected %q stored %q, the bucket computed %q", res.ExpectedETag, res.StoredETag, fake.etag)
	}
	if last != int64(size) {
		t.Fatalf("progress reached %d of %d", last, size)
	}
}

// TestPutFileSinglePutCarriesContentMD5: a small object is one PUT with a
// Content-MD5, and its ETag (the plain MD5) is predicted too.
func TestPutFileSinglePutCarriesContentMD5(t *testing.T) {
	fake, srv := newMultipartS3(t, 0)
	m := parallelStore(t, srv, time.Minute, 6)
	path, data := randomTemp(t, 5000)
	res, err := m.PutFile(context.Background(), "clips/x/poster/poster-a.jpg", path, "image/jpeg", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := md5.Sum(data)
	if fake.singlePUT != 1 || fake.missing != 0 || res.ExpectedETag != hex.EncodeToString(s[:]) || !res.Verified() {
		t.Fatalf("single PUT: puts=%d missing-md5=%d result=%+v", fake.singlePUT, fake.missing, res)
	}
}

// TestPutFileParallelSlowWireIsNotIdle: every part buffer is filled from the
// file at once, then the uplink takes far longer than STORAGE_IDLE_TIMEOUT to
// drain them while never pausing for that long. The file is not read in that
// time, so only the wire can tell the watchdog bytes are moving
// (wireTransport) — without it this upload fails with ErrIdleTimeout.
func TestPutFileParallelSlowWireIsNotIdle(t *testing.T) {
	fake, _ := newMultipartS3(t, 60*time.Millisecond)
	m := pipeStore(t, fake, 400*time.Millisecond, 6)
	path, _ := randomTemp(t, 2*partSize+1000)
	start := time.Now()
	res, err := m.PutFile(context.Background(), "clips/x/renditions/main-a.mp4", path, "video/mp4", nil)
	if err != nil {
		t.Fatalf("PutFile on a slow uplink: %v (after %s)", err, time.Since(start))
	}
	if el := time.Since(start); el < 400*time.Millisecond {
		t.Fatalf("the upload took %s, not longer than the idle timeout: the test proves nothing", el)
	}
	if !res.Verified() {
		t.Fatalf("PutResult = %+v", res)
	}
}

// TestPutFileOneThreadIsSequential: STORAGE_UPLOAD_THREADS=1 is the path
// before erratum 337 — one part at a time — and is still verified.
func TestPutFileOneThreadIsSequential(t *testing.T) {
	fake, srv := newMultipartS3(t, 0)
	fake.holdFor = 200 * time.Millisecond
	m := parallelStore(t, srv, time.Minute, 1)
	path, data := randomTemp(t, partSize+10)
	res, err := m.PutFile(context.Background(), "clips/x/renditions/main-a.mp4", path, "video/mp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fake.maxIn != 1 || !bytes.Equal(fake.object, data) || !res.Verified() || fake.missing != 0 {
		t.Fatalf("one thread: maxIn=%d equal=%v result=%+v missing-md5=%d", fake.maxIn, bytes.Equal(fake.object, data), res, fake.missing)
	}
}
