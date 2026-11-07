package main

// snapshot.go — the database snapshot follows the clips into the store
// (DESIGN.md §20 errata 343, 348, DEPLOY §15).
//
// The store holds bytes without names: every clip's id, title, owner and TTL
// lives only in clippet.db on the box. So a box keeps a copy of that file IN
// the store, under the instance's reserved prefix, and the store alone can
// bring the instance back on another machine (`admin restore --from-store`,
// admin_restore.go). On a bucket that is the bucket; on `fs` (erratum 348) it
// is the media folder's own `_clippet/`, the same relative key — the database
// itself lives in DATA_DIR, not in the folder people back up, so without this
// copy a folder home's backup held every clip's bytes and none of their names.
// The folder store's `.clippet-store` marker sits at the folder's ROOT and the
// snapshot under `_clippet/`: two names that cannot meet.
//
//	<S3_KEY_PREFIX>_clippet/db.sqlite.meta     {written_at, commit, clips, bytes, sha256, born}
//	<S3_KEY_PREFIX>_clippet/db.sqlite          the copy, written through erratum 321's path
//	<S3_KEY_PREFIX>_clippet/db.sqlite.sha256   lowercase hex of the file, written LAST
//
// The rules, each of them small:
//
//   - One goroutine, one write at a time, never on the request path: the
//     triggers (a clip's card becoming ready — erratum 330's hook — and a clip's
//     row being destroyed) only Touch, a non-blocking send into a channel of
//     one, so a burst of a thousand costs the caller a thousand failed selects.
//   - Debounced: at most one upload per DB_SNAPSHOT_EVERY, with a TRAILING
//     write, so the last change of a burst is never lost. A day with no tracked
//     change still gets one (snapshotIdle) — a user, a token or a setting is not
//     tracked, and a day is the most such a change waits.
//   - Once at boot when the bucket's copy is older than the debounce, or there
//     is none, or the last process stopped with a change still pending (the
//     status file's `pending`).
//   - The copy is erratum 321's: `(*admin).writeSnapshot` (`VACUUM INTO` a
//     `.part-` file, 0600, renamed) into SCRATCH_DIR, under a dotted name the
//     scratch janitors never read; then db.Inspect (the integrity check
//     `restore-check` runs) on the copy; then PutFile, the parallel-parts path;
//     then the scratch copy is removed.
//   - Never another database's: the meta carries `born` (db.Born, the first
//     migration's time), and a write is REFUSED when the bucket's meta names a
//     different one — a fresh PC booted against the bucket before its restore,
//     or a probe on the same key prefix, would otherwise replace the one copy
//     that can bring the instance back with an empty database.
//   - A failure is one WARN and a retry one interval on; never a crash, never a
//     refusal of anything else. What happened is written to
//     DATA_DIR/snapshot-status.json for `admin doctor`, another process.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
)

const (
	// snapshotStatusName is the status file under DATA_DIR, beside
	// backup-status.json and for its reason: doctor is another process.
	snapshotStatusName = "snapshot-status.json"
	// snapshotIdle is the most an untracked change waits for an upload.
	snapshotIdle = 24 * time.Hour
	// snapshotContentType is the registered media type of a SQLite file.
	snapshotContentType = "application/vnd.sqlite3"
	// snapshotSmallMax bounds a read of the meta or the sha256 sidecar: both
	// are a few hundred bytes, and a reader never trusts a size it was handed.
	snapshotSmallMax = 64 << 10
)

// snapshotMeta is the JSON beside the snapshot.
type snapshotMeta struct {
	WrittenAt string `json:"written_at"`
	Commit    string `json:"commit"`
	Clips     int    `json:"clips"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
	Born      int64  `json:"born"`
}

// snapshotStatus is DATA_DIR/snapshot-status.json. LastOK and Bytes describe
// the last upload; FailedAt and Reason the last failure when it is newer;
// Pending says the process stopped with a change not yet uploaded.
type snapshotStatus struct {
	LastOK   string `json:"last_ok,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	FailedAt string `json:"failed_at,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Pending  bool   `json:"pending,omitempty"`
}

// readSnapshotStatus reads the status file; missing or malformed is absent.
func readSnapshotStatus(dataDir string) (snapshotStatus, bool) {
	raw, err := os.ReadFile(filepath.Join(dataDir, snapshotStatusName))
	if err != nil {
		return snapshotStatus{}, false
	}
	var st snapshotStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return snapshotStatus{}, false
	}
	return st, true
}

// errForeignSnapshot ends a write whose bucket copy is another database's.
var errForeignSnapshot = errors.New("the store's snapshot belongs to another database")

// dbSnapshotter is the loop. now and after are injectable so the tests can
// drive the debounce on a fake clock; write is the upload itself (nil = the
// real one, writeOnce).
type dbSnapshotter struct {
	cfg   config.Config
	db    *db.DB
	st    store.Store
	log   *slog.Logger
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
	write func(ctx context.Context) (int64, error)

	kick   chan struct{}
	bytes  atomic.Int64
	status snapshotStatus
}

func newDBSnapshotter(cfg config.Config, database *db.DB, st store.Store, log *slog.Logger) *dbSnapshotter {
	return &dbSnapshotter{cfg: cfg, db: database, st: st, log: log, now: time.Now, after: time.After,
		kick: make(chan struct{}, 1)}
}

// Touch marks the database changed. It never blocks: a send into a channel of
// one that is already full is the same news twice.
func (s *dbSnapshotter) Touch() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Bytes is the size of the bucket's copy as this process last knew it — the
// storage line counts it like any stored byte.
func (s *dbSnapshotter) Bytes() int64 { return s.bytes.Load() }

// Run boots and runs the loop until ctx is done.
func (s *dbSnapshotter) Run(ctx context.Context) {
	every := s.cfg.DBSnapshotEvery
	last, dirty := s.boot(ctx)
	for {
		var due <-chan time.Time
		if dirty {
			due = s.after(max(last.Add(every).Sub(s.now()), 0))
		}
		idle := s.after(snapshotIdle)
		select {
		case <-ctx.Done():
			if dirty {
				// The trailing write this process will not make: the next boot
				// makes it instead, however recent the bucket's copy is.
				s.status.Pending = true
				s.saveStatus()
			}
			return
		case <-s.kick:
			dirty = true
		case <-idle:
			dirty = true
		case <-due:
			start := s.now()
			if s.attempt(ctx, start) {
				dirty = false
			}
			last = start
		}
	}
}

// boot reads what the bucket and the status file say and decides whether the
// first upload is due now.
func (s *dbSnapshotter) boot(ctx context.Context) (last time.Time, dirty bool) {
	if st, ok := readSnapshotStatus(s.cfg.DataDir); ok {
		s.status = st
	}
	dirty = s.status.Pending
	s.status.Pending = false
	sctx, cancel := context.WithTimeout(ctx, s.cfg.S3.OpTimeout)
	defer cancel()
	prefix := s.cfg.S3.KeyPrefix
	switch info, err := s.st.Stat(sctx, store.SnapshotSumKey(prefix)); {
	case err == nil:
		last = info.LastModified
		if dbi, err := s.st.Stat(sctx, store.SnapshotKey(prefix)); err == nil {
			s.bytes.Store(dbi.Size)
		}
	case errors.Is(err, store.ErrNotFound):
	default:
		s.log.Warn("db snapshot: the bucket could not be read at boot; writing one now", "err", err)
	}
	if last.IsZero() || s.now().Sub(last) >= s.cfg.DBSnapshotEvery {
		dirty, last = true, time.Time{}
	}
	s.log.Info("db snapshot on", "key", store.SnapshotKey(prefix), "every", s.cfg.DBSnapshotEvery,
		"due_now", dirty)
	return last, dirty
}

// attempt is one upload, a panic turned into a failure. It reports success.
func (s *dbSnapshotter) attempt(ctx context.Context, start time.Time) (ok bool) {
	defer func() {
		if p := recover(); p != nil {
			s.failed(start, fmt.Errorf("panic: %v", p))
			ok = false
		}
	}()
	write := s.write
	if write == nil {
		write = s.writeOnce
	}
	runCtx, cancel := context.WithTimeout(ctx, adminTimeout)
	defer cancel()
	n, err := write(runCtx)
	if err != nil {
		if ctx.Err() != nil {
			s.log.Info("db snapshot cancelled", "reason", "the server is shutting down")
			return false
		}
		s.failed(start, err)
		return false
	}
	s.bytes.Store(n)
	s.status = snapshotStatus{LastOK: rfcTime(start), Bytes: n}
	s.saveStatus()
	s.log.Info("db snapshot written", "key", store.SnapshotKey(s.cfg.S3.KeyPrefix), "bytes", n,
		"took", s.now().Sub(start).Round(time.Millisecond))
	return true
}

func (s *dbSnapshotter) failed(start time.Time, err error) {
	s.status.FailedAt, s.status.Reason = rfcTime(start), err.Error()
	s.saveStatus()
	s.log.Warn("db snapshot failed", "err", err, "retry", rfcTime(start.Add(s.cfg.DBSnapshotEvery)))
}

func (s *dbSnapshotter) saveStatus() {
	if err := writeJSONAtomic(s.cfg.DataDir, snapshotStatusName, s.status); err != nil {
		s.log.Warn("db snapshot status not written", "err", err)
	}
}

// writeOnce is the upload: the owner check, the consistent copy into scratch,
// its integrity check, then the meta, the copy and the sidecar, in that order.
// It returns the copy's size.
func (s *dbSnapshotter) writeOnce(ctx context.Context) (int64, error) {
	prefix := s.cfg.S3.KeyPrefix
	born, err := s.db.Born(ctx)
	if err != nil {
		return 0, err
	}
	if err := checkSnapshotOwner(ctx, s.st, prefix, born); err != nil {
		return 0, err
	}
	base := filepath.Join(s.cfg.ScratchDir, ".db-snapshot-"+ids.New())
	file := base + ".sqlite"
	defer func() {
		// The copy, and whatever SQLite left beside it while Inspect read it.
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(file + suffix)
		}
	}()
	var stderr bytes.Buffer
	a := &admin{cfg: s.cfg, db: s.db, ctx: ctx, env: adminEnv{stdout: io.Discard, stderr: &stderr, now: s.now}}
	size, code := a.writeSnapshot(file)
	if code != 0 {
		return 0, fmt.Errorf("the copy: %s", lastLine(stderr.String()))
	}
	sum, err := fileSHA256(file)
	if err != nil {
		return 0, err
	}
	insp, err := db.Inspect(ctx, file)
	if err != nil {
		return 0, err
	}
	if !insp.OK() {
		return 0, fmt.Errorf("the copy failed its integrity check (%s)", insp.Integrity)
	}
	meta := snapshotMeta{WrittenAt: rfcTime(s.now()), Commit: commit, Clips: int(insp.Clips), Bytes: size, SHA256: sum, Born: born}
	if err := putSmall(ctx, s.st, base+".meta", store.SnapshotMetaKey(prefix), mustJSON(meta), "application/json"); err != nil {
		return 0, err
	}
	res, err := s.st.PutFile(ctx, store.SnapshotKey(prefix), file, snapshotContentType, nil)
	if err != nil {
		return 0, err
	}
	if res.SHA256 != "" && res.SHA256 != sum {
		return 0, fmt.Errorf("the bytes sent are not the bytes hashed (%s, want %s)", res.SHA256, sum)
	}
	if err := putSmall(ctx, s.st, base+".sha256", store.SnapshotSumKey(prefix), []byte(sum+"\n"), "text/plain"); err != nil {
		return 0, err
	}
	return size, nil
}

// checkSnapshotOwner refuses a write over another database's snapshot. No meta
// is an unclaimed slot; a meta that names no `born` (or does not parse) was not
// written by this design and is overwritten.
func checkSnapshotOwner(ctx context.Context, st store.Store, prefix string, born int64) error {
	raw, err := readSmall(ctx, st, store.SnapshotMetaKey(prefix))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the snapshot's meta: %w", err)
	}
	var m snapshotMeta
	if json.Unmarshal(raw, &m) != nil || m.Born == 0 || m.Born == born {
		return nil
	}
	return fmt.Errorf("%w (born %s, this one %s): not overwritten - two instances share this store (bucket or media folder) and S3_KEY_PREFIX, "+
		"or this is a new database that `admin restore --from-store` should have replaced (DEPLOY.md §15)",
		errForeignSnapshot, rfcTime(time.Unix(m.Born, 0)), rfcTime(time.Unix(born, 0)))
}

// readSmall reads a small object whole, refusing one larger than
// snapshotSmallMax.
func readSmall(ctx context.Context, st store.Store, key string) ([]byte, error) {
	rc, err := st.GetRange(ctx, key, 0, -1)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, snapshotSmallMax+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > snapshotSmallMax {
		return nil, fmt.Errorf("%s is larger than %d bytes", key, snapshotSmallMax)
	}
	return raw, nil
}

// putSmall writes data to a scratch file and puts it under key; the file is
// removed either way.
func putSmall(ctx context.Context, st store.Store, file, key string, data []byte, contentType string) error {
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return err
	}
	defer os.Remove(file)
	_, err := st.PutFile(ctx, key, file, contentType, nil)
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err) // a struct of strings and numbers always marshals
	}
	return append(raw, '\n')
}

// snapshotAgo words how long ago t was, for doctor and the restore: "just
// now", "12 s ago", "2 min ago", "5 h ago", "3 days ago".
func snapshotAgo(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0:
		return "in the future (" + rfcTime(t) + ")"
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d s ago", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
}

// onOff is the boot line's word for a switch.
func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// trimHex is the sidecar's content as a reader compares it.
func trimHex(raw []byte) string { return strings.ToLower(strings.TrimSpace(string(raw))) }
