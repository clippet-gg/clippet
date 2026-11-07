package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func gets(f *Fake) int {
	n := 0
	for _, c := range f.CallsSnapshot() {
		if len(c) >= 8 && c[:8] == "GetRange" {
			n++
		}
	}
	return n
}

func seekerFixture(t *testing.T) (*Fake, []byte) {
	t.Helper()
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	f := NewFake()
	f.Put("obj", data, "video/mp4")
	return f, data
}

func TestSeekerSeekIsOffline(t *testing.T) {
	f, data := seekerFixture(t)
	rs := NewSeeker(context.Background(), f, "obj", int64(len(data)))
	defer rs.Close()
	// http.ServeContent's size probe: SeekEnd then SeekStart.
	if n, err := rs.Seek(0, io.SeekEnd); err != nil || n != 1000 {
		t.Fatalf("SeekEnd = %d, %v", n, err)
	}
	if n, err := rs.Seek(0, io.SeekStart); err != nil || n != 0 {
		t.Fatalf("SeekStart = %d, %v", n, err)
	}
	if n, err := rs.Seek(10, io.SeekCurrent); err != nil || n != 10 {
		t.Fatalf("SeekCurrent = %d, %v", n, err)
	}
	if _, err := rs.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek accepted")
	}
	if _, err := rs.Seek(0, 7); err == nil {
		t.Fatal("bad whence accepted")
	}
	if gets(f) != 0 {
		t.Fatalf("Seek made %d storage calls", gets(f))
	}
}

func TestSeekerOneGetPerReadRun(t *testing.T) {
	f, data := seekerFixture(t)
	rs := NewSeeker(context.Background(), f, "obj", int64(len(data)))
	defer rs.Close()
	// Full read in small chunks: exactly one GET.
	got, err := io.ReadAll(iotest(rs, 7))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("ReadAll: len=%d err=%v", len(got), err)
	}
	if gets(f) != 1 {
		t.Fatalf("full read made %d GETs, want 1", gets(f))
	}
	// EOF after the run, still no new GET.
	if _, err := rs.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read past end = %v, want EOF", err)
	}
	if gets(f) != 1 {
		t.Fatalf("EOF read made a GET")
	}
	// Seek back and read a byte range: second GET, from the new offset.
	if _, err := rs.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 50)
	if _, err := io.ReadFull(rs, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, data[100:150]) {
		t.Fatal("range bytes mismatch")
	}
	if gets(f) != 2 {
		t.Fatalf("after seek+read: %d GETs, want 2", gets(f))
	}
	// Seek to the current position must not reopen.
	if _, err := rs.Seek(150, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rs, buf); err != nil || !bytes.Equal(buf, data[150:200]) {
		t.Fatalf("continue read: %v", err)
	}
	if gets(f) != 2 {
		t.Fatalf("no-op seek reopened: %d GETs", gets(f))
	}
	// The GET range is offset..size-1.
	calls := f.CallsSnapshot()
	if calls[len(calls)-1] != "GetRange obj" {
		t.Fatalf("last call %q", calls[len(calls)-1])
	}
}

func TestSeekerRangeBoundaries(t *testing.T) {
	f, data := seekerFixture(t)
	rs := NewSeeker(context.Background(), f, "obj", int64(len(data)))
	defer rs.Close()
	// Last byte only.
	if _, err := rs.Seek(999, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 10)
	n, err := rs.Read(b)
	if n != 1 || (err != nil && err != io.EOF) || b[0] != data[999] {
		t.Fatalf("last byte: n=%d err=%v", n, err)
	}
	if _, err := rs.Read(b); err != io.EOF {
		t.Fatalf("after last byte: %v", err)
	}
	// Read after Seek(0, SeekEnd) → EOF, zero GETs for that run.
	before := gets(f)
	if _, err := rs.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Read(b); err != io.EOF {
		t.Fatalf("read at end = %v", err)
	}
	if _, err := rs.Seek(5, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Read(b); err != io.EOF {
		t.Fatalf("read past end = %v", err)
	}
	if gets(f) != before {
		t.Fatalf("reads at/past EOF issued GETs")
	}
	// Zero-length read is a no-op.
	if n, err := rs.Read(nil); n != 0 || err != nil {
		t.Fatalf("zero read = %d, %v", n, err)
	}
}

func TestSeekerClosesPreviousBody(t *testing.T) {
	f, data := seekerFixture(t)
	rs := NewSeeker(context.Background(), f, "obj", int64(len(data)))
	b := make([]byte, 10)
	if _, err := rs.Read(b); err != nil {
		t.Fatal(err)
	}
	// Seeking elsewhere closes the open body; a new run opens a new GET.
	if _, err := rs.Seek(500, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Read(b); err != nil || !bytes.Equal(b, data[500:510]) {
		t.Fatalf("after reseek: %v %v", err, b)
	}
	if gets(f) != 2 {
		t.Fatalf("GETs = %d", gets(f))
	}
	if err := rs.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rs.Close(); err != nil {
		t.Fatal("double close")
	}
}

// A closed seeker stops reading and never opens another range (§20 erratum
// 41). http.ServeContent answers a multi-range request from a goroutine it
// starts and never joins: that goroutine is still calling Seek/Read when an
// aborted copy returns to the handler and the handler closes the seeker, so
// a late read must fail instead of buying a fresh GetRange for a request
// that has already ended.
func TestSeekerClosedStopsReading(t *testing.T) {
	f, data := seekerFixture(t)
	rs := NewSeeker(context.Background(), f, "obj", int64(len(data)))
	b := make([]byte, 10)
	if _, err := rs.Read(b); err != nil {
		t.Fatal(err)
	}
	if err := rs.Close(); err != nil {
		t.Fatal(err)
	}
	before := gets(f)
	// Both a continuing read and one that would have to reopen after a seek.
	if _, err := rs.Read(b); !errors.Is(err, errSeekerClosed) {
		t.Fatalf("read after close = %v", err)
	}
	if _, err := rs.Seek(500, io.SeekStart); !errors.Is(err, errSeekerClosed) {
		t.Fatalf("seek after close = %v", err)
	}
	if _, err := rs.Read(b); !errors.Is(err, errSeekerClosed) {
		t.Fatalf("read after close+seek = %v", err)
	}
	if gets(f) != before {
		t.Fatalf("a closed seeker made %d storage calls", gets(f)-before)
	}
	if err := rs.Close(); err != nil {
		t.Fatal("double close")
	}
}

// Close is safe while a read is in flight: it closes the body under the
// reader (which is what unblocks a read waiting on the network) instead of
// waiting behind it, and the two never touch a field unguarded — run under
// -race, this is the unit-level shape of the media-path race erratum 41
// closed.
func TestSeekerCloseDuringRead(t *testing.T) {
	f, data := seekerFixture(t)
	slow := &slowStore{Store: f, delay: 2 * time.Millisecond}
	rs := NewSeeker(context.Background(), slow, "obj", int64(len(data)))
	done := make(chan struct{})
	go func() {
		defer close(done)
		b := make([]byte, 8)
		for {
			if _, err := rs.Read(b); err != nil {
				return
			}
			if _, err := rs.Seek(0, io.SeekStart); err != nil { // force a reopen per pass
				return
			}
		}
	}()
	time.Sleep(5 * time.Millisecond) // sleep: lets the reopen loop run before Close (a race test; any interleaving must pass)
	if err := rs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader still running after Close")
	}
	before := gets(f)
	if _, err := rs.Read(make([]byte, 8)); !errors.Is(err, errSeekerClosed) {
		t.Fatalf("read after close = %v", err)
	}
	if gets(f) != before {
		t.Fatal("a closed seeker opened a range")
	}
}

// slowStore delays every body read so a Close can land mid-read.
type slowStore struct {
	Store
	delay time.Duration
}

func (s *slowStore) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	rc, err := s.Store.GetRange(ctx, key, start, end)
	if err != nil {
		return nil, err
	}
	return &slowBody{ReadCloser: rc, delay: s.delay}, nil
}

type slowBody struct {
	io.ReadCloser
	delay time.Duration
}

func (b *slowBody) Read(p []byte) (int, error) {
	time.Sleep(b.delay) // sleep: a slow body IS the subject
	return b.ReadCloser.Read(p)
}

func TestSeekerErrors(t *testing.T) {
	f, data := seekerFixture(t)
	// Missing object surfaces ErrNotFound on the first read.
	rs := NewSeeker(context.Background(), f, "missing", 10)
	if _, err := rs.Read(make([]byte, 1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// Sticky: a second read does not retry.
	if _, err := rs.Read(make([]byte, 1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing again: %v", err)
	}
	if gets(f) != 1 {
		t.Fatalf("retried failed GET: %d", gets(f))
	}
	// Object shorter than the DB size → ErrUnexpectedEOF (drift).
	rs = NewSeeker(context.Background(), f, "obj", int64(len(data))+100)
	got, err := io.ReadAll(rs)
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(got) != len(data) {
		t.Fatalf("short object: len=%d err=%v", len(got), err)
	}
	// Cancelled ctx propagates from the body.
	ctx, cancel := context.WithCancel(context.Background())
	rs = NewSeeker(ctx, f, "obj", int64(len(data)))
	if _, err := rs.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := rs.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("after cancel: %v", err)
	}
}

// iotest wraps r so every Read is at most n bytes (like a slow network).
func iotest(r io.Reader, n int) io.Reader { return &smallReader{r: r, n: n} }

type smallReader struct {
	r io.Reader
	n int
}

func (s *smallReader) Read(p []byte) (int, error) {
	if len(p) > s.n {
		p = p[:s.n]
	}
	return s.r.Read(p)
}
