package store

// fs.go — the filesystem-backed Store (ROADMAP #37, DESIGN.md §20 erratum 276).
//
// The case this exists for: "I have a VPS with a disk. Why am I running an
// object-storage server to put files on it?" — and the NAS case, where the
// share is already mounted on the host and binding it into one container is
// the whole deployment. `STORAGE_BACKEND=fs` + `STORAGE_DIR` selects it and
// the MinIO pair goes away (`compose.fs.yaml`, DEPLOY.md §32).
//
// Three properties it inherits from minio.go rather than inventing:
//
//   - PutFile is ATOMIC at the destination key. A temp file in the SAME
//     directory, fsync'd, renamed over the key, then the directory fsync'd
//     (everywhere but Windows, which has no directory fsync — erratum 336):
//     a crash mid-copy leaves a `.clippet-put-*` file, never a truncated
//     object under the name the database is about to point at.
//   - DeletePrefix VERIFIES. It re-lists the prefix afterwards and returns
//     ErrPrefixNotEmpty if anything is left, exactly as the S3 store does —
//     the sweeper's whole "the objects are gone" claim rests on it (§12).
//   - Every failure carries a bounded PUBLIC class (§20 erratum 90), because a
//     failed clip's reason is rendered to anonymous readers and a filesystem
//     error names the path, which on `fs` is the operator's disk layout.
//
// And two it cannot inherit, both recorded in erratum 276:
//
//   - CONTENT TYPE IS NOT STORED. `Store` never hands one back — ObjectInfo
//     has no content-type field and GetRange returns bytes — and every media
//     route sets `Content-Type` from the DATABASE (§15's content-type
//     discipline: `clips.source_content_type` for the original, a fixed type
//     for every derived object). So a sidecar file per object would be written
//     by this code and read by nothing, which is a worse answer than none.
//   - ETAG IS EMPTY. The S3 store reports the provider's ETag and the Fake
//     reports the sha256 it holds in memory; hashing a 4 GiB original inside
//     `admin ls --stat` to invent one here would make a report cost minutes.
//     PutFile still returns the sha256 it computed on the way in — which is
//     what the database stores and what every caller actually uses.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// fsDirMode/fsFileMode are what a fresh directory and object get. The
	// image runs as uid 10001 and a NAS share is usually mounted with an
	// owner of the operator's choosing (DEPLOY §32), so nothing here widens
	// the group or other bits beyond read.
	fsDirMode  = 0o755
	fsFileMode = 0o644
	// fsChunk is the copy buffer, which is also the progress granularity:
	// PutFile and Download call progress once per chunk, the way minio-go
	// calls its hook once per slice handed to the wire.
	fsChunk = 1 << 20
	// fsPutTemp is the prefix of the temp file PutFile renames from. It is
	// deliberately visible to List (see List's doc): a crashed transfer's
	// leftovers are litter in the operator's directory, and `admin orphans`
	// is where litter is reported.
	fsPutTemp = ".clippet-put-"
	// fsPingTemp is the probe file Ping creates and removes.
	fsPingTemp = ".clippet-ping-"
	// fsDefaultOpTimeout guards a zero opTimeout (a store built by hand in a
	// test); config.Load never produces one. It is minio.go's own default.
	fsDefaultOpTimeout = 30 * time.Second
)

// MarkerName is the file that says "this directory is the one that instance
// has been writing to" (§20 erratum 276). `clippet serve` writes it at boot
// and `admin storage migrate --to fs` writes it after a copy; `admin doctor`
// WARNs when it is absent on an instance whose database already accounts for
// stored bytes, which is the shape a mis-mounted bind takes: an empty
// directory where the media used to be, and an app that answers 404 for every
// clip it still lists.
const MarkerName = ".clippet-store"

// markerText is what the marker holds. It is for the person who finds it, not
// for this code: nothing parses it, and its ABSENCE is the whole signal.
const markerText = "clippet: this directory is a Clippet media store (STORAGE_BACKEND=fs).\n" +
	"Back it up together with the database (DEPLOY.md §32); neither half is any use alone.\n"

// ErrBadKey is what the path law refuses: a key that is not a legal relative
// path under STORAGE_DIR. It is never a condition of the store — it means a
// caller built a key somewhere other than keys.go.
var ErrBadKey = errors.New("store: key is not a legal path")

type fsStore struct {
	root string
	// opTimeout is STORAGE_OP_TIMEOUT, honoured by the three control-plane
	// calls (Ping, Stat, List) exactly as minio.go honours it — see
	// withOpTimeout. 0 means fsDefaultOpTimeout.
	opTimeout time.Duration
	// modeRefused is set the first time the directory refuses PutFile's
	// chmod (see fsModeUnsupported), so that refusal is logged once per
	// store and not once per object.
	modeRefused atomic.Bool
}

var _ Store = (*fsStore)(nil)

// fsChmod is os.Chmod, a variable so a test can play a mount that refuses a
// mode change — vfat/exFAT, SMB without unix extensions — which no test
// container can mount.
var fsChmod = os.Chmod

// fsModeUnsupported reports whether a chmod error means "this filesystem does
// not keep unix modes" rather than a broken write: EPERM/EACCES (vfat and exFAT
// answer EPERM unless mounted `quiet`), ENOTSUP/ENOSYS, and EINVAL. It is
// syncDir's forgiveness applied to the mode, for the same deployment: a
// mounted share (a nit sweep's `[local, verifier]` row; erratum 276, amended).
func fsModeUnsupported(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, errors.ErrUnsupported) ||
		errors.Is(err, syscall.EINVAL)
}

// NewFS returns the Store over dir, with opTimeout bounding its control-plane
// calls. It performs NO I/O: the directory is checked by Ping, so a missing
// mount is a `/readyz` failure and an `admin doctor` line rather than a crash
// loop, exactly as an unreachable bucket is.
//
// It does not create the directory either. `clippet serve` does that at boot
// beside DATA_DIR and SCRATCH_DIR; nothing else in the binary may, because a
// verb that silently created a missing mount point would write the instance's
// media into the container's own filesystem and lose it on the next recreate.
func NewFS(dir string, opTimeout time.Duration) (Store, error) {
	if dir == "" {
		return nil, errors.New("store: STORAGE_DIR is empty")
	}
	if !strings.HasPrefix(dir, "/") && !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("store: STORAGE_DIR %q is not an absolute path", dir)
	}
	return &fsStore{root: filepath.Clean(dir), opTimeout: opTimeout}, nil
}

// withOpTimeout runs one control-plane syscall under STORAGE_OP_TIMEOUT.
//
// A filesystem call cannot be cancelled: `os.Stat` on a hung NFS mount blocks
// in the kernel until the mount answers or is unmounted, and no context
// reaches it. So the call runs in a goroutine and this returns when it
// finishes OR when the deadline expires, whichever is first — and when the
// deadline wins the goroutine is LEAKED until the filesystem answers. That is
// the honest cost, and it is the right trade in the three places it is used:
// `/readyz`'s ping has to answer, `admin stats` must not hang an operator's
// terminal, and a stuck List must not hold a sweeper tick forever. The channel
// is buffered so an abandoned goroutine never blocks on its send.
//
// Transfers are deliberately NOT wrapped: PutFile/Download check ctx between
// chunks and a GetRange body checks it per Read, which is where a stalled
// share shows up as progress that stops. There is no equivalent of
// STORAGE_IDLE_TIMEOUT or STORAGE_TRANSFER_MAX on this backend, and DEPLOY §7
// and §32.2 say so rather than implying the S3 guarantees carry over.
func (s *fsStore) withOpTimeout(ctx context.Context, fn func() error) error {
	d := s.opTimeout
	if d <= 0 {
		d = fsDefaultOpTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// --- the path law ------------------------------------------------------------

// keyPath is the ONE place a key becomes a path. Everything below goes through
// it, so the rules are in one readable list rather than spread over seven
// methods:
//
//   - a NUL or a backslash is refused (a backslash is a path separator on the
//     other half of the platforms Go builds for, and no key ours mints holds
//     one);
//   - an ABSOLUTE key is refused — `/etc/passwd` must not become `/etc/passwd`;
//   - an EMPTY segment is refused, so `clips//x` cannot be a second spelling of
//     `clips/x`;
//   - `.` and `..` segments are refused outright, which is the traversal rule;
//   - and, belt and braces after all of that, a result that does not sit under
//     the root is refused — the check that would catch a rule the four above
//     ever stop covering.
//
// The empty key is the ONE key that maps to the root itself: List("") means
// "everything", the way an empty S3 prefix does. Object methods refuse it
// through objectPath.
//
// keys.go is already the only place a key is BUILT (§20 erratum 82) and the
// only user-influenced text in one is a sanitised `[a-z0-9]{1,8}` extension —
// so this is a second wall in front of a key space that is already safe, which
// is the right number of walls for a function that turns a string into a path.
func (s *fsStore) keyPath(key string) (string, error) {
	if strings.ContainsRune(key, 0) {
		return "", fmt.Errorf("%w: it holds a NUL byte", ErrBadKey)
	}
	if strings.ContainsRune(key, '\\') {
		return "", fmt.Errorf("%w: %q holds a backslash", ErrBadKey, key)
	}
	if strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("%w: %q is absolute", ErrBadKey, key)
	}
	trimmed := strings.TrimSuffix(key, "/")
	if trimmed == "" {
		if key == "" {
			return s.root, nil
		}
		return "", fmt.Errorf("%w: %q names no object", ErrBadKey, key)
	}
	segs := strings.Split(trimmed, "/")
	for _, seg := range segs {
		switch seg {
		case "":
			return "", fmt.Errorf("%w: %q has an empty segment", ErrBadKey, key)
		case ".", "..":
			return "", fmt.Errorf("%w: %q has a %q segment", ErrBadKey, key, seg)
		}
	}
	p := filepath.Join(append([]string{s.root}, segs...)...)
	if p != s.root && !strings.HasPrefix(p, s.root+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q resolves outside STORAGE_DIR", ErrBadKey, key)
	}
	return p, nil
}

// objectPath is keyPath for a key that must name one object: the empty key and
// a trailing slash are prefixes, and a prefix is not something to open.
//
// It is LEXICAL only. Every caller that then opens, reads, writes or stats the
// path goes through safePath instead; the two places that want the lexical
// answer are DeletePrefix (which unlinks a symlink rather than refusing it) and
// keyOf's inverse.
func (s *fsStore) objectPath(key string) (string, error) {
	if key == "" || strings.HasSuffix(key, "/") {
		return "", fmt.Errorf("%w: %q is a prefix, not an object", ErrBadKey, key)
	}
	return s.keyPath(key)
}

// safePath is objectPath plus THE SYMLINK RULE: nothing under STORAGE_DIR is
// ever followed through a symlink (§20 erratum 276(q)).
//
// The path law above is purely lexical, which is the right shape for a key
// space keys.go mints — but a lexical check says nothing about what the
// FILESYSTEM does with the path, and a symlink is the filesystem's own
// traversal. Two consequences, both reachable by anything that can write into
// the store:
//
//   - a symlink planted AS an object is followed by a read and a write, so it
//     serves and stores bytes that are not in this store at all;
//   - a symlinked directory COMPONENT (`<root>/clips` → somewhere else) makes
//     every key under it read and write outside the root, which is exactly
//     what the path law exists to prevent.
//
// So: every component below the root is Lstat'd, and a symlink anywhere along
// the way is ErrBadKey. The ROOT itself is never checked — `STORAGE_DIR=/media`
// pointing at a mount is the operator's own configuration and a perfectly
// ordinary way to deploy this. A component that does not exist yet ends the
// walk: PutFile is about to create it.
//
// It is a check and not a resolution, so it is not free of TOCTOU — a symlink
// that appears between this and the open is not caught. That is acceptable
// because writing into STORAGE_DIR already implies the ability to replace any
// object in it: the rule exists so that the store's own reports and the path
// law agree with each other, not to defend against an attacker who already has
// the directory.
func (s *fsStore) safePath(key string) (string, error) {
	p, err := s.objectPath(key)
	if err != nil {
		return "", err
	}
	if err := s.noSymlinks(key, p, true); err != nil {
		return "", err
	}
	return p, nil
}

// noSymlinks walks the components of p below the root. last says whether the
// FINAL component must also be a real file: DeletePrefix passes false, because
// `os.Remove` unlinks a symlink without following it and removing litter is
// the point.
func (s *fsStore) noSymlinks(key, p string, last bool) error {
	if p == s.root {
		return nil // the root is the operator's own configuration; see safePath
	}
	rel, ok := s.keyOf(p)
	if !ok {
		return fmt.Errorf("%w: %q resolves outside STORAGE_DIR", ErrBadKey, key)
	}
	segs := strings.Split(rel, "/")
	if !last {
		segs = segs[:len(segs)-1]
	}
	cur := s.root
	for _, seg := range segs {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing below this exists yet
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %q passes through a symlink at %s, which this store never follows", ErrBadKey, key, cur)
		}
	}
	return nil
}

// keyOf is keyPath backwards: the key of a path under the root, with the
// platform separator turned back into the `/` a key uses.
func (s *fsStore) keyOf(p string) (string, bool) {
	rel, err := filepath.Rel(s.root, p)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// --- the seven methods --------------------------------------------------------

// PutFile copies the file at path to key: a temp file in the destination's own
// directory, fsync'd and renamed, then the directory fsync'd. contentType is
// accepted and NOT stored — see the file header.
func (s *fsStore) PutFile(ctx context.Context, key, path, contentType string, progress func(done, total int64)) (PutResult, error) {
	dst, err := s.safePath(key)
	if err != nil {
		return PutResult{}, fmt.Errorf("store: put %s: %w", key, err)
	}
	src, err := os.Open(path)
	if err != nil {
		return PutResult{}, fmt.Errorf("store: put %s: %w", key, err)
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return PutResult{}, fmt.Errorf("store: put %s: %w", key, err)
	}
	total := st.Size()

	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, fsDirMode); err != nil {
		return PutResult{}, fsErr("put", key, err)
	}
	tmp, err := os.CreateTemp(dir, fsPutTemp+"*")
	if err != nil {
		return PutResult{}, fsErr("put", key, err)
	}
	tmpName := tmp.Name()
	h := sha256.New()
	done, err := fsCopy(ctx, io.MultiWriter(tmp, h), src, total, progress)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// CreateTemp makes 0600; the object's mode is the store's, not the
		// temp file's, and it is set before the rename so no reader ever sees
		// the key with the narrow one.
		//
		// A directory that keeps no unix modes refuses the change, and that
		// is not a failed write (erratum 276, amended): the bytes are in place
		// and fsync'd, and on such a mount every file has the mode the mount
		// options give it whatever we ask. The refusal is logged ONCE per
		// store and the write goes on.
		if cerr := fsChmod(tmpName, fsFileMode); cerr != nil {
			if !fsModeUnsupported(cerr) {
				err = cerr
			} else if s.modeRefused.CompareAndSwap(false, true) {
				slog.Warn("storage: this directory refuses a file mode change; objects keep the mode the mount gives them",
					"dir", s.root, "err", cerr)
			}
		}
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return PutResult{}, fsErr("put", key, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return PutResult{}, fsErr("put", key, err)
	}
	if err := syncDir(dir); err != nil {
		return PutResult{}, fsErr("put", key, err)
	}
	return PutResult{Size: done, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// Download writes the object to path, through `path + ".part"` and a rename so
// a failed transfer never leaves a truncated file under the final name — the
// same contract minio.go's Download keeps, for the same reason (the worker
// re-downloads a source it has lost and then feeds it to ffmpeg).
func (s *fsStore) Download(ctx context.Context, key, path string, progress func(done, total int64)) error {
	src, err := s.safePath(key)
	if err != nil {
		return fmt.Errorf("store: download %s: %w", key, err)
	}
	f, err := os.Open(src)
	if err != nil {
		return fsErr("download", key, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fsErr("download", key, err)
	}
	if st.IsDir() {
		return notFound("download", key)
	}
	if err := os.MkdirAll(filepath.Dir(path), fsDirMode); err != nil {
		return fmt.Errorf("store: download %s: %w", key, err)
	}
	tmp := path + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("store: download %s: %w", key, err)
	}
	_, err = fsCopy(ctx, out, f, st.Size(), progress)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fsErr("download", key, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: download %s: %w", key, err)
	}
	return nil
}

// GetRange streams bytes [start, end] — end INCLUSIVE, end == -1 → to EOF —
// which is the media hot path: store.NewSeeker turns every `Range:` header
// into exactly one of these (§8.5).
//
// The edges are the S3 ones, deliberately, because the Fake and minio.go are
// what the rest of the code has been written against: an `end` past the last
// byte is CLAMPED (S3 answers the bytes it has), and a `start` at or past the
// end is an ERROR (S3 answers 416, and minio.go surfaces it from GetRange
// itself rather than from the first Read). An empty object therefore has no
// satisfiable range at all, which is also what S3 says.
func (s *fsStore) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	p, err := s.safePath(key)
	if err != nil {
		return nil, fmt.Errorf("store: get %s: %w", key, err)
	}
	if start < 0 || end < -1 || (end >= 0 && start > end) {
		return nil, fmt.Errorf("store: get %s: invalid range %d-%d", key, start, end)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fsErr("get", key, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fsErr("get", key, err)
	}
	if st.IsDir() {
		_ = f.Close()
		return nil, notFound("get", key)
	}
	size := st.Size()
	last := end
	if last < 0 || last >= size {
		last = size - 1
	}
	if start > last {
		_ = f.Close()
		return nil, &StorageError{Class: PublicUnavailable,
			Err: fmt.Errorf("store: get %s: unsatisfiable range %d-%d for size %d", key, start, end, size)}
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fsErr("get", key, err)
	}
	return &fsBody{ctx: ctx, f: f, r: io.LimitReader(f, last-start+1), key: key}, nil
}

// Stat describes an object, under STORAGE_OP_TIMEOUT. ETag is empty here — see
// the file header.
func (s *fsStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	var info ObjectInfo
	err := s.withOpTimeout(ctx, func() error {
		p, perr := s.safePath(key)
		if perr != nil {
			return fmt.Errorf("store: stat %s: %w", key, perr)
		}
		fi, serr := os.Stat(p)
		if serr != nil {
			return fsErr("stat", key, serr)
		}
		if fi.IsDir() {
			return notFound("stat", key)
		}
		info = ObjectInfo{Key: key, Size: fi.Size(), LastModified: fi.ModTime()}
		return nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return ObjectInfo{}, fsErr("stat", key, err)
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	return info, nil
}

// List returns every object under prefix, sorted by key.
//
// A prefix is a STRING prefix, not a directory: S3 answers `clips/ab` with
// every key that begins with those bytes, and `admin orphans` compares its
// results against key strings, so this must too. The walk is BOUNDED to the
// directory the prefix names — `clips/x/renditions/main-` walks
// `<root>/clips/x/renditions` and nothing else — so listing one clip's prefix
// never costs a walk of the whole store.
//
// Three things it reports that an operator might not expect, all on purpose:
// a `.clippet-put-*` file left by a crashed transfer (litter, and `admin
// orphans` is where litter is reported), any file a person dropped into the
// directory by hand (the same), and — since §20 erratum 276(q) — any entry
// that is NOT a regular file: a symlink, a fifo, a socket, a device. Those are
// litter of a sharper kind, because safePath refuses to follow them: if List
// hid them, an `admin orphans` run would report a clean store while a symlink
// sat under a live clip's prefix serving nothing and surviving every destroy.
// A directory is not an entry (S3 has none) and the store marker at the root
// is not an object; those two are the only things skipped.
//
// It runs under STORAGE_OP_TIMEOUT, like minio.go's List: a walk of a hung
// mount must not hold a sweeper tick forever.
func (s *fsStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	err := s.withOpTimeout(ctx, func() error {
		var lerr error
		out, lerr = s.list(ctx, prefix)
		return lerr
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fsErr("list", prefix, err)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *fsStore) list(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	// The prefix itself goes through the path law first — a prefix with no `/`
	// in it would otherwise reach the walk below without ever being looked at.
	if _, err := s.keyPath(prefix); err != nil {
		return nil, fmt.Errorf("store: list %s: %w", prefix, err)
	}
	dirKey := ""
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		dirKey = prefix[:i+1]
	}
	base, err := s.keyPath(dirKey)
	if err != nil {
		return nil, fmt.Errorf("store: list %s: %w", prefix, err)
	}
	// The directory the walk starts from is held to the symlink rule too: a
	// walk that began inside a symlinked component would list somebody else's
	// filesystem as this store's contents.
	if err := s.noSymlinks(prefix, base, true); err != nil {
		return nil, fmt.Errorf("store: list %s: %w", prefix, err)
	}
	if _, err := os.Stat(base); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // an absent prefix is an empty listing, as in S3
		}
		return nil, fsErr("list", prefix, err)
	}
	var out []ObjectInfo
	// filepath.WalkDir never follows a symlinked directory, so the walk itself
	// stays inside the store; a symlink it meets arrives here as an entry.
	walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // something was removed under us; it is not in the listing
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			return nil
		}
		key, ok := s.keyOf(p)
		if !ok || !strings.HasPrefix(key, prefix) || key == MarkerName {
			return nil
		}
		info, ierr := d.Info() // Lstat's answer: a symlink's own size, never its target's
		if ierr != nil {
			if errors.Is(ierr, os.ErrNotExist) {
				return nil
			}
			return ierr
		}
		out = append(out, ObjectInfo{Key: key, Size: info.Size(), LastModified: info.ModTime()})
		return nil
	})
	if walkErr != nil {
		return nil, fsErr("list", prefix, walkErr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// DeletePrefix removes everything under prefix and VERIFIES the prefix is
// empty afterwards (the S3 store's contract, §6). A caller that already knows
// the key calls DeleteKey instead (§20 erratum 305): this walks the directory
// to decide what to remove, and the bucket store walks a listing, which is the
// half of the contract that can lag.
//
// It also prunes the directories it empties, up to but never including the
// root: S3 has no directories, so the rest of the app never learns that a
// clip's prefix was one, and a store that kept an empty `clips/<id>/poster`
// tree per deleted clip would grow inodes forever.
func (s *fsStore) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	objs, err := s.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	deleted := 0
	dirs := map[string]bool{}
	for _, o := range objs {
		if cerr := ctx.Err(); cerr != nil {
			return deleted, cerr
		}
		// objectPath, not safePath: `os.Remove` UNLINKS a symlink instead of
		// following it, so a planted one is removable litter rather than a
		// permanent resident (§20 erratum 276(q)). The parent components are
		// still held to the symlink rule, so nothing is ever removed from
		// outside the store.
		p, perr := s.objectPath(o.Key)
		if perr == nil {
			perr = s.noSymlinks(o.Key, p, false)
		}
		if perr != nil {
			return deleted, fmt.Errorf("store: delete prefix %s: %w", prefix, perr)
		}
		if rerr := os.Remove(p); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return deleted, fsErr("remove", o.Key, rerr)
		}
		deleted++
		dirs[filepath.Dir(p)] = true
	}
	s.pruneDirs(dirs)
	left, err := s.List(ctx, prefix)
	if err != nil {
		return deleted, err
	}
	if len(left) > 0 {
		return deleted, fmt.Errorf("store: delete prefix %s: %d objects remain: %w", prefix, len(left), ErrPrefixNotEmpty)
	}
	return deleted, nil
}

// DeleteKey unlinks exactly this object and prunes the directories it empties
// (§20 erratum 305). A missing file is success, which is the S3 answer too.
//
// A directory has no lagging index, so on `fs` this is DeletePrefix of one key
// with the walk and the re-list taken out; it is here because the CONTRACT is
// the store's, not the backend's, and the worker must be able to say "remove
// this key" without a listing standing between the call and the object.
// objectPath rather than safePath, and the same noSymlinks rule on the parents,
// for the reason DeletePrefix gives (§20 erratum 276(q)).
func (s *fsStore) DeleteKey(ctx context.Context, key string) error {
	return s.withOpTimeout(ctx, func() error {
		p, err := s.objectPath(key)
		if err == nil {
			err = s.noSymlinks(key, p, false)
		}
		if err != nil {
			return fmt.Errorf("store: delete key %s: %w", key, err)
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fsErr("remove", key, err)
		}
		s.pruneDirs(map[string]bool{filepath.Dir(p): true})
		return nil
	})
}

// pruneDirs removes each directory that is now empty and walks up while its
// parent is too. os.Remove refuses a non-empty directory, which is the whole
// guard: the loop stops at the first one that still holds something, and the
// prefix check stops it at the root whatever happens.
func (s *fsStore) pruneDirs(dirs map[string]bool) {
	for d := range dirs {
		for d != s.root && strings.HasPrefix(d, s.root+string(filepath.Separator)) {
			if err := os.Remove(d); err != nil {
				break
			}
			d = filepath.Dir(d)
		}
	}
}

// Ping answers "is the store there and can this process write to it": the
// directory exists, is a directory, and takes a probe file that is removed
// again. It is the `fs` answer to BucketExists, and like it the classes it
// reports are the closed §20 erratum 31 set, because /healthz and /readyz
// render them to anonymous callers and a filesystem error text names the path.
// A missing or non-directory STORAGE_DIR is `bucket missing`: the set is
// closed on purpose and that is its member for "the place the objects live is
// not there".
//
// It runs under STORAGE_OP_TIMEOUT (withOpTimeout), because a hung mount is
// precisely what it exists to report and `os.Stat` on one blocks in the kernel:
// without the deadline the boot ping loop and `/readyz` would block with it.
func (s *fsStore) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return &PingError{Class: PingTimeout, Err: fmt.Errorf("store: ping: %w", err)}
	}
	err := s.withOpTimeout(ctx, func() error { return s.ping() })
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &PingError{Class: PingTimeout,
			Err: fmt.Errorf("store: ping: STORAGE_DIR %s did not answer within %s: %w", s.root, s.opTimeoutOrDefault(), err)}
	}
	return err
}

// PresignGet always refuses (§20 erratum 278). A directory has no origin and
// no credential to sign with, so there is no URL to hand a viewer — which is
// why MEDIA_DELIVERY=redirect on STORAGE_BACKEND=fs is a BOOT error and this
// can never be reached from a request. It is here, returning a named sentinel
// rather than panicking, because the Store contract has to be total: `admin`
// verbs and tests hold this interface too.
func (s *fsStore) PresignGet(ctx context.Context, key string, ttl time.Duration, contentType, disposition string) (string, error) {
	return "", &StorageError{Class: PublicUnavailable,
		Err: fmt.Errorf("store: presign %s: %w (the media lives in a directory, not in a bucket - DEPLOY.md §33)", key, ErrPresignUnsupported)}
}

func (s *fsStore) opTimeoutOrDefault() time.Duration {
	if s.opTimeout > 0 {
		return s.opTimeout
	}
	return fsDefaultOpTimeout
}

func (s *fsStore) ping() error {
	fi, err := os.Stat(s.root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return &PingError{Class: PingBucketMissing,
			Err: fmt.Errorf("store: ping: STORAGE_DIR %s does not exist", s.root)}
	case err != nil:
		return &PingError{Class: fsPingClass(err), Err: fmt.Errorf("store: ping: %w", err)}
	case !fi.IsDir():
		return &PingError{Class: PingBucketMissing,
			Err: fmt.Errorf("store: ping: STORAGE_DIR %s is not a directory", s.root)}
	}
	f, err := os.CreateTemp(s.root, fsPingTemp+"*")
	if err != nil {
		return &PingError{Class: fsPingClass(err),
			Err: fmt.Errorf("store: ping: STORAGE_DIR %s is not writable: %w", s.root, err)}
	}
	name := f.Name()
	cerr := f.Close()
	// The probe then takes the rest of the path a real object takes in
	// PutFile — a rename inside the directory and the directory's own sync —
	// so a store whose writes fail AFTER the file is made (Windows' refused
	// directory fsync before §20 erratum 336) says so here instead of reading
	// `writable` while every upload fails.
	var werr error
	if cerr == nil {
		moved := name + "-renamed"
		if werr = os.Rename(name, moved); werr == nil {
			name = moved
			werr = syncDir(s.root)
		}
	}
	if rerr := os.Remove(name); rerr != nil {
		return &PingError{Class: fsPingClass(rerr),
			Err: fmt.Errorf("store: ping: the probe file %s could not be removed: %w", name, rerr)}
	}
	if cerr != nil {
		return &PingError{Class: fsPingClass(cerr), Err: fmt.Errorf("store: ping: %w", cerr)}
	}
	if werr != nil {
		return &PingError{Class: fsPingClass(werr),
			Err: fmt.Errorf("store: ping: STORAGE_DIR %s is not writable: %w", s.root, werr)}
	}
	return nil
}

// --- the marker ---------------------------------------------------------------

// WriteMarker creates MarkerName in dir unless it is already there. It is
// called by `clippet serve` at boot and by `admin storage migrate --to fs`,
// and by nothing else: a verb that wrote it would make the absence this
// exists to notice impossible to observe.
func WriteMarker(dir string) error {
	p := filepath.Join(dir, MarkerName)
	if _, err := os.Stat(p); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stamp := markerText + "written " + time.Now().UTC().Format(time.RFC3339) + "\n"
	return os.WriteFile(p, []byte(stamp), fsFileMode)
}

// HasMarker reports whether MarkerName is in dir.
func HasMarker(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, MarkerName))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, err
}

// --- plumbing -----------------------------------------------------------------

// fsBody is the GetRange body: the bounded reader plus its file. Read checks
// the caller's ctx first, so a cancelled media request stops mid-body the way
// a real HTTP body does (the Fake's ctxReader does the same).
type fsBody struct {
	ctx context.Context
	f   *os.File
	r   io.Reader
	key string
}

func (b *fsBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF {
		err = fsErr("get", b.key, err)
	}
	return n, err
}

func (b *fsBody) Close() error { return b.f.Close() }

// fsCopy is the one copy loop PutFile and Download share: ctx-aware, chunked,
// and feeding progress(done, total) once per chunk with done clamped to total
// — the same contract minio.go's progressHook keeps for a retried part.
func fsCopy(ctx context.Context, w io.Writer, r io.Reader, total int64, progress func(done, total int64)) (int64, error) {
	buf := make([]byte, fsChunk)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return done, werr
			}
			done += int64(n)
			if progress != nil {
				progress(min(done, total), total)
			}
		}
		if rerr == io.EOF {
			return done, nil
		}
		if rerr != nil {
			return done, rerr
		}
	}
}

// syncDir — the directory fsync after PutFile's rename — lives in
// fs_sync_other.go (every platform but Windows: the body it always had) and
// fs_sync_windows.go (a no-op: Windows has no directory fsync; §20 erratum 336).

// fsErr wraps a filesystem failure with its bounded public class, the way
// mapErr does for minio-go. A missing file becomes ErrNotFound so
// `errors.Is(err, store.ErrNotFound)` keeps working across both backends —
// the sweeper, `admin orphans` and the media routes all branch on it.
func fsErr(op, key string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return notFound(op, key)
	}
	return &StorageError{Class: fsPublicClass(err), Err: fmt.Errorf("store: %s %s: %w", op, key, err)}
}

func notFound(op, key string) error {
	return &StorageError{Class: PublicNotFound, Err: fmt.Errorf("store: %s %s: %w", op, key, ErrNotFound)}
}

// fsPublicClass buckets a filesystem failure into one of the three §20 erratum
// 90 classes. Like minio.go's publicClass it names only what KIND of trouble
// storage was in: the full error, which holds the path, stays with the
// operator's log.
func fsPublicClass(err error) string {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, os.ErrNotExist):
		return PublicNotFound
	case errors.Is(err, context.DeadlineExceeded):
		return PublicTimeout
	}
	return PublicUnavailable
}

// fsPingClass is fsPublicClass for the ping vocabulary (§20 erratum 31).
func fsPingClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return PingTimeout
	case errors.Is(err, os.ErrNotExist):
		return PingBucketMissing
	case errors.Is(err, os.ErrPermission):
		return PingDenied
	}
	return PingUnavailable
}
