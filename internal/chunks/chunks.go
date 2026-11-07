// Package chunks keeps the upload sessions of the chunked web upload (DESIGN.md
// §20 erratum 344): one file sent as a run of bounded requests, because the edge
// in front of a Pass name refuses any single request body over its plan's cap.
//
// A session is a directory under $SCRATCH_DIR/.chunks/{id}: `session.json` (what
// was opened, how many chunks are in, and — once finished — which clip it made)
// and `data`, the file being assembled, written chunk by chunk at its own offset.
// Nothing about a session is in the database and nothing is only in memory: a
// restart of the box between two chunks loses nothing, and GET answers what the
// disk says.
//
// The name `.chunks` is outside both janitors' rules on purpose — it is neither
// an `ids.Valid` clip dir nor a `.upload-` spool, so the boot janitor (which
// removes every spool unconditionally) leaves a half-sent upload alone. What
// removes a session is Expire, which the sweeper calls: 24 hours after it was
// opened, finished or not.
//
// The package knows nothing about HTTP and nothing about clips. Finish hands the
// assembled file to a function of the caller's — the ingest core, the same one
// the one-shot upload runs — and records what it answered.
package chunks

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DirName is the sessions' directory under SCRATCH_DIR.
const DirName = ".chunks"

// MaxAge is how long a session lives after it was opened (§20 erratum 344).
const MaxAge = 24 * time.Hour

const (
	metaName = "session.json"
	dataName = "data"
	idBytes  = 16 // 32 hex characters
)

// The errors a caller maps onto its answers.
var (
	ErrNotFound   = errors.New("chunks: no such upload")
	ErrOutOfOrder = errors.New("chunks: chunk out of order")
	ErrIndex      = errors.New("chunks: chunk index out of range")
	ErrLength     = errors.New("chunks: chunk length does not match")
	ErrShort      = errors.New("chunks: the chunk's body ended early")
	ErrIncomplete = errors.New("chunks: upload is missing chunks")
	ErrDone       = errors.New("chunks: upload is finished")
	ErrFinishing  = errors.New("chunks: upload is finishing")
)

// DiskError is a write-side failure of the scratch disk: the caller answers it
// as the scratch class, never as the client's fault.
type DiskError struct{ Err error }

func (e *DiskError) Error() string { return "chunks: scratch: " + e.Err.Error() }
func (e *DiskError) Unwrap() error { return e.Err }

// Session is one upload's record, exactly what session.json holds.
type Session struct {
	ID         string `json:"id"`
	Owner      string `json:"owner"`
	Size       int64  `json:"size"`
	ChunkBytes int64  `json:"chunk_bytes"`
	// Received counts the chunks that are whole on disk: chunks 0 … Received-1.
	Received int    `json:"received"`
	Name     string `json:"name,omitempty"`
	Type     string `json:"type,omitempty"`
	// Meta is the caller's metadata document, kept as it was sent and read
	// again at Finish.
	Meta    json.RawMessage `json:"meta,omitempty"`
	Created int64           `json:"created"`

	// Clip and Existing are set once Finish succeeded: the clip the ingest
	// answered, and whether it was a byte-identical clip the account held.
	Clip     string `json:"clip,omitempty"`
	Existing bool   `json:"existing,omitempty"`

	// Finishing is never stored: it is this process's word that a Finish is
	// running right now.
	Finishing bool `json:"-"`
}

// Chunks is how many chunks the file is.
func (s Session) Chunks() int {
	if s.ChunkBytes <= 0 || s.Size <= 0 {
		return 0
	}
	return int((s.Size + s.ChunkBytes - 1) / s.ChunkBytes)
}

// ChunkLen is chunk i's exact length: ChunkBytes, the last one the remainder.
func (s Session) ChunkLen(i int) int64 {
	if i < 0 || i >= s.Chunks() {
		return 0
	}
	if rest := s.Size - int64(i)*s.ChunkBytes; rest < s.ChunkBytes {
		return rest
	}
	return s.ChunkBytes
}

// ReceivedBytes is how many bytes of the file are whole on disk.
func (s Session) ReceivedBytes() int64 {
	if s.Received >= s.Chunks() {
		return s.Size
	}
	return int64(s.Received) * s.ChunkBytes
}

// Done says Finish succeeded.
func (s Session) Done() bool { return s.Clip != "" }

// Result is what Finish's function answered.
type Result struct {
	Clip     string
	Existing bool
}

// Store is the sessions under one directory. Its methods are safe for
// concurrent use; the work on ONE session is serialised by that session's own
// lock, so two requests for different uploads never wait on each other.
type Store struct {
	root string
	now  func() time.Time

	mu    sync.Mutex
	slots map[string]*slot
}

// slot is one session's in-process state: its lock, and the two facts that
// are only true while this process is running.
type slot struct {
	mu        sync.Mutex
	finishing bool
	abandoned bool
}

// New is a store over root ($SCRATCH_DIR/.chunks). The directory is made on
// the first Open.
func New(root string) *Store {
	return &Store{root: root, now: time.Now, slots: map[string]*slot{}}
}

// Root is the directory the sessions live in.
func (st *Store) Root() string { return st.root }

// ValidID says s is an id Open minted: 32 lower-case hex characters. It is
// checked before any path is built from s.
func ValidID(s string) bool {
	if len(s) != 2*idBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (st *Store) slot(id string) *slot {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.slots[id]
	if !ok {
		s = &slot{}
		st.slots[id] = s
	}
	return s
}

func (st *Store) forget(id string) {
	st.mu.Lock()
	delete(st.slots, id)
	st.mu.Unlock()
}

func (st *Store) dir(id string) string { return filepath.Join(st.root, id) }

// DataPath is the file a session assembles. A caller reads it only inside
// Finish's function.
func (st *Store) DataPath(id string) string { return filepath.Join(st.dir(id), dataName) }

// Open makes a session for s (its ID, Received, Created and result fields are
// the store's to set) and returns it as stored.
func (st *Store) Open(s Session) (Session, error) {
	if s.Size <= 0 || s.ChunkBytes <= 0 || s.Owner == "" {
		return Session{}, fmt.Errorf("chunks: a session needs an owner, a size and a chunk size")
	}
	var raw [idBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Session{}, err
	}
	s.ID = hex.EncodeToString(raw[:])
	s.Received, s.Clip, s.Existing, s.Finishing = 0, "", false, false
	s.Created = st.now().Unix()
	dir := st.dir(s.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Session{}, &DiskError{err}
	}
	f, err := os.OpenFile(filepath.Join(dir, dataName), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		_ = os.RemoveAll(dir)
		return Session{}, &DiskError{err}
	}
	if err := f.Close(); err != nil {
		_ = os.RemoveAll(dir)
		return Session{}, &DiskError{err}
	}
	if err := writeMeta(dir, s); err != nil {
		_ = os.RemoveAll(dir)
		return Session{}, err
	}
	return s, nil
}

// Get is owner's session id. Another account's session is ErrNotFound, never
// a different answer: an upload id says nothing to anybody but its owner.
func (st *Store) Get(owner, id string) (Session, error) {
	if !ValidID(id) {
		return Session{}, ErrNotFound
	}
	s, err := readMeta(st.dir(id))
	if err != nil {
		return Session{}, err
	}
	if s.Owner != owner || st.expired(s) {
		return Session{}, ErrNotFound
	}
	sl := st.slot(id)
	st.mu.Lock()
	s.Finishing = sl.finishing
	st.mu.Unlock()
	return s, nil
}

func (st *Store) expired(s Session) bool {
	return st.now().Sub(time.Unix(s.Created, 0)) > MaxAge
}

// Put writes chunk index of owner's session id from body, which must carry
// exactly length bytes — the chunk's own length (ChunkLen). An index already
// received is (session, true, nil) and body is never read: a repeated chunk is
// the client retrying an answer it lost, and the bytes are already there. An
// index past the next one is ErrOutOfOrder; the session comes back with it so
// the caller can say where to resume.
func (st *Store) Put(owner, id string, index int, length int64, body io.Reader) (Session, bool, error) {
	if _, err := st.Get(owner, id); err != nil {
		return Session{}, false, err
	}
	sl := st.slot(id)
	sl.mu.Lock()
	defer sl.mu.Unlock()
	// Again under the lock: another Put of this session may have landed while
	// this one waited.
	s, err := st.Get(owner, id)
	if err != nil {
		return Session{}, false, err
	}
	if index < 0 || index >= s.Chunks() {
		return s, false, ErrIndex
	}
	if index < s.Received {
		return s, true, nil
	}
	if s.Done() {
		return s, false, ErrDone
	}
	if index > s.Received {
		return s, false, ErrOutOfOrder
	}
	if length != s.ChunkLen(index) {
		return s, false, ErrLength
	}
	f, err := os.OpenFile(st.DataPath(id), os.O_WRONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Session{}, false, ErrNotFound
		}
		return s, false, &DiskError{err}
	}
	if _, err := f.Seek(int64(index)*s.ChunkBytes, io.SeekStart); err != nil {
		_ = f.Close()
		return s, false, &DiskError{err}
	}
	n, err := io.Copy(diskWriter{f}, io.LimitReader(body, length))
	if err != nil {
		_ = f.Close()
		var de *DiskError
		if errors.As(err, &de) {
			return s, false, de
		}
		return s, false, fmt.Errorf("%w: %v", ErrShort, err)
	}
	if n != length {
		_ = f.Close()
		return s, false, ErrShort
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return s, false, &DiskError{err}
	}
	if err := f.Close(); err != nil {
		return s, false, &DiskError{err}
	}
	// Only now is the chunk counted: a crash between the write and this line
	// leaves Received where it was, and the same chunk is simply sent again.
	s.Received++
	if err := writeMeta(st.dir(id), s); err != nil {
		s.Received--
		return s, false, err
	}
	return s, false, nil
}

// Finish hands the whole file to fn and records what it answered. A session
// already finished answers its recorded result without calling fn again (a
// client that lost the first answer asks twice); one still missing chunks is
// ErrIncomplete. fn receives the session and the data file's path; it may link
// or copy the file but must not move or remove it — on success the store
// removes it, and on failure it is kept, so the same Finish can be tried again.
func (st *Store) Finish(owner, id string, fn func(s Session, data string) (Result, error)) (Session, error) {
	if _, err := st.Get(owner, id); err != nil {
		return Session{}, err
	}
	sl := st.slot(id)
	sl.mu.Lock()
	defer sl.mu.Unlock()
	s, err := st.Get(owner, id)
	if err != nil {
		return Session{}, err
	}
	if s.Done() {
		return s, nil
	}
	if s.Received < s.Chunks() {
		return s, ErrIncomplete
	}
	data := st.DataPath(id)
	// The file is exactly Size bytes: every chunk was written to its own length
	// at its own offset, and a truncate costs nothing when that is already so.
	if err := os.Truncate(data, s.Size); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Session{}, ErrNotFound
		}
		return s, &DiskError{err}
	}
	st.mu.Lock()
	sl.finishing = true
	st.mu.Unlock()
	res, ferr := fn(s, data)
	st.mu.Lock()
	sl.finishing = false
	abandoned := sl.abandoned
	st.mu.Unlock()
	if ferr != nil {
		if abandoned {
			st.remove(id)
		}
		return s, ferr
	}
	s.Clip, s.Existing = res.Clip, res.Existing
	if abandoned {
		st.remove(id)
		return s, nil
	}
	// The clip exists whatever happens below. The record first: if it will not
	// write, the file stays, and a repeated Finish runs fn again — which the
	// ingest's duplicate check answers with this same clip. A file that will
	// not go goes with the session at its expiry.
	if err := writeMeta(st.dir(id), s); err == nil {
		_ = os.Remove(data)
	}
	return s, nil
}

// Delete abandons owner's session id. A session whose Finish is running is
// marked and removed when it returns — the clip it is making is made, exactly
// as a one-shot upload cancelled after its last byte is.
func (st *Store) Delete(owner, id string) error {
	if _, err := st.Get(owner, id); err != nil {
		return err
	}
	sl := st.slot(id)
	st.mu.Lock()
	if sl.finishing {
		sl.abandoned = true
		st.mu.Unlock()
		return nil
	}
	st.mu.Unlock()
	sl.mu.Lock()
	defer sl.mu.Unlock()
	st.mu.Lock()
	if sl.finishing { // it started while this waited for the lock
		sl.abandoned = true
		st.mu.Unlock()
		return nil
	}
	st.mu.Unlock()
	st.remove(id)
	return nil
}

func (st *Store) remove(id string) {
	_ = os.RemoveAll(st.dir(id))
	st.forget(id)
}

// Expire removes every session opened more than MaxAge before now, and any
// directory under the root that is not a session at all once it is that old.
// A session whose lock is held (a chunk being written, a Finish running) is
// left for the next call. It returns how many it removed.
func (st *Store) Expire(now time.Time) int {
	entries, err := os.ReadDir(st.root)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(st.root, name)
		if !ValidID(name) || !e.IsDir() {
			if info, ierr := e.Info(); ierr == nil && now.Sub(info.ModTime()) > MaxAge {
				if os.RemoveAll(path) == nil {
					removed++
				}
			}
			continue
		}
		sl := st.slot(name)
		if !sl.mu.TryLock() {
			continue
		}
		created := time.Time{}
		if s, err := readMeta(path); err == nil {
			created = time.Unix(s.Created, 0)
		} else if info, ierr := e.Info(); ierr == nil {
			created = info.ModTime()
		}
		if !created.IsZero() && now.Sub(created) > MaxAge {
			if os.RemoveAll(path) == nil {
				removed++
			}
			sl.mu.Unlock()
			st.forget(name)
			continue
		}
		sl.mu.Unlock()
	}
	return removed
}

// diskWriter tags a write error as the disk's, so Put can tell it from the
// client's body ending early inside io.Copy.
type diskWriter struct{ f *os.File }

func (d diskWriter) Write(p []byte) (int, error) {
	n, err := d.f.Write(p)
	if err != nil {
		return n, &DiskError{err}
	}
	return n, nil
}

func readMeta(dir string) (Session, error) {
	b, err := os.ReadFile(filepath.Join(dir, metaName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Session{}, ErrNotFound
		}
		return Session{}, &DiskError{err}
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return Session{}, ErrNotFound
	}
	return s, nil
}

// writeMeta replaces session.json whole: a temporary file, synced, renamed.
func writeMeta(dir string, s Session) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, metaName+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return &DiskError{err}
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return &DiskError{err}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return &DiskError{err}
	}
	if err := f.Close(); err != nil {
		return &DiskError{err}
	}
	if err := os.Rename(tmp, filepath.Join(dir, metaName)); err != nil {
		return &DiskError{err}
	}
	return nil
}
