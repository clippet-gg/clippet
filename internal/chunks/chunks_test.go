package chunks

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), DirName))
}

func open(t *testing.T, st *Store, size, chunk int64) Session {
	t.Helper()
	s, err := st.Open(Session{Owner: "u1", Size: size, ChunkBytes: chunk, Name: "a.mp4"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 3)
	}
	return b
}

func put(t *testing.T, st *Store, s Session, file []byte, i int) (Session, bool, error) {
	t.Helper()
	off := int64(i) * s.ChunkBytes
	part := file[off : off+s.ChunkLen(i)]
	return st.Put(s.Owner, s.ID, i, int64(len(part)), bytes.NewReader(part))
}

func TestSessionArithmetic(t *testing.T) {
	s := Session{Size: 25, ChunkBytes: 10}
	if s.Chunks() != 3 || s.ChunkLen(0) != 10 || s.ChunkLen(2) != 5 || s.ChunkLen(3) != 0 {
		t.Fatalf("chunks %d, lens %d %d %d", s.Chunks(), s.ChunkLen(0), s.ChunkLen(2), s.ChunkLen(3))
	}
	if exact := (Session{Size: 20, ChunkBytes: 10}); exact.Chunks() != 2 || exact.ChunkLen(1) != 10 {
		t.Fatalf("an exact multiple: %d chunks, last %d", exact.Chunks(), exact.ChunkLen(1))
	}
	s.Received = 3
	if s.ReceivedBytes() != 25 {
		t.Fatalf("received bytes %d", s.ReceivedBytes())
	}
}

func TestValidID(t *testing.T) {
	for _, bad := range []string{"", "..", "ABCDEF0123456789abcdef0123456789", "0123456789abcdef0123456789abcde", "0123456789abcdef0123456789abcdeg", "../0123456789abcdef0123456789ab"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
	if !ValidID("0123456789abcdef0123456789abcdef") {
		t.Error("a 32-hex id is not valid")
	}
}

// TestPutInOrderIdempotentAndFinish: chunks land in order, a repeated chunk is
// 200 without reading its body, an out-of-order one is refused with the session
// to resume from, and Finish hands over exactly the file.
func TestPutInOrderIdempotentAndFinish(t *testing.T) {
	st := newStore(t)
	file := payload(25)
	s := open(t, st, 25, 10)
	if len(s.ID) != 32 || s.Received != 0 {
		t.Fatalf("opened %+v", s)
	}
	if got, _, err := put(t, st, s, file, 1); !errors.Is(err, ErrOutOfOrder) || got.Received != 0 {
		t.Fatalf("chunk 1 first: %v, received %d", err, got.Received)
	}
	if got, already, err := put(t, st, s, file, 0); err != nil || already || got.Received != 1 {
		t.Fatalf("chunk 0: %v already=%v received=%d", err, already, got.Received)
	}
	// The same chunk again: answered, and its body is never read.
	got, already, err := st.Put("u1", s.ID, 0, 10, readerThatFails{t})
	if err != nil || !already || got.Received != 1 {
		t.Fatalf("chunk 0 again: %v already=%v received=%d", err, already, got.Received)
	}
	if _, _, err := st.Put("u1", s.ID, 1, 9, bytes.NewReader(file[10:19])); !errors.Is(err, ErrLength) {
		t.Fatalf("a short declared length: %v", err)
	}
	if _, _, err := st.Put("u1", s.ID, 3, 0, bytes.NewReader(nil)); !errors.Is(err, ErrIndex) {
		t.Fatalf("index past the last chunk: %v", err)
	}
	if _, _, err := st.Put("u1", s.ID, 1, 10, bytes.NewReader(file[10:15])); !errors.Is(err, ErrShort) {
		t.Fatalf("a body that ends early: %v", err)
	}
	if _, err := st.Finish("u1", s.ID, func(Session, string) (Result, error) { t.Fatal("fn on an incomplete upload"); return Result{}, nil }); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Finish while incomplete: %v", err)
	}
	for i := 1; i < 3; i++ {
		if _, _, err := put(t, st, s, file, i); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	calls := 0
	done, err := st.Finish("u1", s.ID, func(sess Session, data string) (Result, error) {
		calls++
		b, err := os.ReadFile(data)
		if err != nil || !bytes.Equal(b, file) {
			t.Fatalf("the assembled file differs (%v): %d bytes", err, len(b))
		}
		return Result{Clip: "AAAAAAAAAAAA"}, nil
	})
	if err != nil || done.Clip != "AAAAAAAAAAAA" || !done.Done() {
		t.Fatalf("Finish: %v %+v", err, done)
	}
	if _, err := os.Stat(st.DataPath(s.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the data file outlived the finish: %v", err)
	}
	// A second Finish answers the first one's result and never runs fn again.
	again, err := st.Finish("u1", s.ID, func(Session, string) (Result, error) { calls++; return Result{}, nil })
	if err != nil || again.Clip != "AAAAAAAAAAAA" || calls != 1 {
		t.Fatalf("Finish again: %v %+v, fn ran %d times", err, again, calls)
	}
	// Every chunk of a finished upload reads as received.
	if _, already, err := put(t, st, s, file, 2); err != nil || !already {
		t.Fatalf("a chunk after the finish: %v already=%v", err, already)
	}
}

// TestFinishFailureKeepsTheFile: a failed ingest leaves the file, so the same
// Finish can be tried again.
func TestFinishFailureKeepsTheFile(t *testing.T) {
	st := newStore(t)
	file := payload(7)
	s := open(t, st, 7, 10)
	if _, _, err := put(t, st, s, file, 0); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("storage down")
	if _, err := st.Finish("u1", s.ID, func(Session, string) (Result, error) { return Result{}, boom }); !errors.Is(err, boom) {
		t.Fatalf("Finish: %v", err)
	}
	got, err := st.Get("u1", s.ID)
	if err != nil || got.Done() || got.Received != 1 || got.Finishing {
		t.Fatalf("after a failed finish: %v %+v", err, got)
	}
	if done, err := st.Finish("u1", s.ID, func(Session, string) (Result, error) { return Result{Clip: "BBBBBBBBBBBB", Existing: true}, nil }); err != nil || !done.Existing {
		t.Fatalf("the retry: %v %+v", err, done)
	}
}

// TestOwnerOnly: another account's session is not found, by every method.
func TestOwnerOnly(t *testing.T) {
	st := newStore(t)
	s := open(t, st, 5, 10)
	if _, err := st.Get("u2", s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get by another owner: %v", err)
	}
	if _, _, err := st.Put("u2", s.ID, 0, 5, bytes.NewReader(payload(5))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Put by another owner: %v", err)
	}
	if err := st.Delete("u2", s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete by another owner: %v", err)
	}
	if _, err := st.Get("u1", "0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of an unknown id: %v", err)
	}
	if err := st.Delete("u1", s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get("u1", s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
}

// TestDeleteWhileFinishing: the abandon is noted and the session goes when the
// finish returns; the clip it made is made.
func TestDeleteWhileFinishing(t *testing.T) {
	st := newStore(t)
	s := open(t, st, 3, 10)
	if _, _, err := put(t, st, s, payload(3), 0); err != nil {
		t.Fatal(err)
	}
	done, err := st.Finish("u1", s.ID, func(Session, string) (Result, error) {
		if got, _ := st.Get("u1", s.ID); !got.Finishing {
			t.Error("Get during the finish does not say finishing")
		}
		if err := st.Delete("u1", s.ID); err != nil {
			t.Errorf("Delete during the finish: %v", err)
		}
		return Result{Clip: "CCCCCCCCCCCC"}, nil
	})
	if err != nil || done.Clip != "CCCCCCCCCCCC" {
		t.Fatalf("Finish: %v %+v", err, done)
	}
	if _, err := os.Stat(filepath.Join(st.Root(), s.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the abandoned session outlived its finish: %v", err)
	}
}

// TestExpire: a session older than MaxAge goes, finished or not; a younger one
// stays; a busy one waits; litter that is not a session goes once it is old.
func TestExpire(t *testing.T) {
	st := newStore(t)
	base := time.Unix(1_800_000_000, 0)
	st.now = func() time.Time { return base }
	old := open(t, st, 5, 10)
	busy := open(t, st, 5, 10)
	st.now = func() time.Time { return base.Add(MaxAge - time.Hour) }
	young := open(t, st, 5, 10)
	litter := filepath.Join(st.Root(), "not-a-session")
	if err := os.MkdirAll(litter, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(litter, base, base); err != nil {
		t.Fatal(err)
	}
	later := base.Add(MaxAge + time.Minute)
	st.now = func() time.Time { return later }
	if _, err := st.Get("u1", old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired session still answers Get: %v", err)
	}
	sl := st.slot(busy.ID)
	sl.mu.Lock()
	if n := st.Expire(later); n != 2 {
		t.Fatalf("Expire removed %d, want 2 (the old session and the litter)", n)
	}
	sl.mu.Unlock()
	for _, gone := range []string{old.ID, "not-a-session"} {
		if _, err := os.Stat(filepath.Join(st.Root(), gone)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived Expire: %v", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(st.Root(), young.ID)); err != nil {
		t.Fatalf("the young session went: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.Root(), busy.ID)); err != nil {
		t.Fatalf("the busy session went while locked: %v", err)
	}
	if n := st.Expire(later); n != 1 {
		t.Fatalf("the second Expire removed %d, want the busy one", n)
	}
	// No directory at all is not an error.
	if n := New(filepath.Join(t.TempDir(), "nothing")).Expire(later); n != 0 {
		t.Fatalf("Expire on a missing root: %d", n)
	}
}

type readerThatFails struct{ t *testing.T }

func (r readerThatFails) Read([]byte) (int, error) {
	r.t.Fatal("the body of an already-received chunk was read")
	return 0, nil
}
