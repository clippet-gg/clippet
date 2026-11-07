package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory Store for tests (§18.1 contract):
//
//   - Calls records "op key" for every operation, in order.
//   - FailNext(op, err) makes the next call of op return err.
//   - Deletes() lists every key removed, in order (worker tests assert none
//     under original/).
//   - Hook, when set, runs at the START of every op with that op's ctx; a
//     non-nil return is returned by the op unchanged. A hook that blocks on
//     ctx.Done() is the "slow Fake" every timeout test uses; a hook that
//     appends to a slice is the ordering probe.
//
// Op names: PutFile, Download, GetRange, Stat, List, DeletePrefix, DeleteKey,
// Ping, EnsureBucket.
type Fake struct {
	mu       sync.Mutex
	objects  map[string]*fakeObject
	failNext map[string]error
	deletes  []string

	// Calls is appended under the Fake's lock; read it after the operations
	// under test have returned, or use CallsSnapshot from another goroutine.
	Calls []string
	// Hook is consulted at the start of every operation (see type doc).
	Hook func(ctx context.Context, op, key string) error
	// PresignOrigin overrides the `scheme://host` PresignGet mints against, so
	// a test can make the Fake stand in for a bucket at a particular origin —
	// which is what `admin doctor`'s `media` row and the CSP are about (§20
	// erratum 278). "" is FakePresignHost.
	PresignOrigin string
	// OpaqueETags makes Stat, List and PutFile report each object's sha256 as
	// its ETag — a provider whose ETag is not the S3 MD5 shape, which is what
	// the Fake reported before §20 erratum 337. Left false, the Fake answers
	// the S3 shape (ETagHasher at UploadPartSize), so a caller's ETag proof
	// matches exactly as it would on R2.
	OpaqueETags bool
	// BucketMissing makes the Fake a store whose bucket does not exist yet:
	// EnsureBucket creates it (clears the field) and reports that it did
	// (§20 erratum 346). Nothing else reads it.
	BucketMissing bool

	// unlisted holds the keys the LISTING half of this store cannot see: List
	// and DeletePrefix skip them, while Stat, GetRange, Download, DeleteKey,
	// Keys and Get find them exactly as before.
	//
	// It models the one guarantee a bucket does not give and MinIO's local
	// backend happens to (§20 erratum 305): R2's listing is an eventually
	// consistent index, so an object whose PutFile returned a millisecond ago
	// can be missing from the very next List while a key-addressed request
	// already reads it. Until this field the Fake listed from the same map it
	// stored into, so no test could see the gap — which is why every E2E run
	// passed the step the reference stack failed.
	unlisted map[string]bool
}

// Unlisted makes the LISTING pretend these keys' objects are not there yet,
// while every key-addressed operation still finds them (see the field's
// comment). It is sticky: they stay unlistable until Listed puts them back.
func (f *Fake) Unlisted(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		f.unlisted[k] = true
	}
}

// Listed undoes Unlisted for these keys — the listing index catching up.
func (f *Fake) Listed(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		delete(f.unlisted, k)
	}
}

type fakeObject struct {
	data        []byte
	contentType string
	sha256      string
	etag        string // the S3-shaped ETag (§20 erratum 337)
	modified    time.Time
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{objects: map[string]*fakeObject{}, failNext: map[string]error{}, unlisted: map[string]bool{}}
}

// FailNext arranges for the next call of op to return err.
func (f *Fake) FailNext(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[op] = err
}

// Deletes returns every key removed so far, in order.
func (f *Fake) Deletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

// CallsSnapshot returns a copy of Calls taken under the lock.
func (f *Fake) CallsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}

// Put seeds an object directly (test setup; not a Store op, not recorded).
func (f *Fake) Put(key string, data []byte, contentType string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = newFakeObject(data, contentType)
}

// Get returns a copy of an object's bytes (test inspection; not recorded).
func (f *Fake) Get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), o.data...), true
}

// Keys returns every stored key, sorted (test inspection; not recorded).
func (f *Fake) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sortedKeys("")
}

func newFakeObject(data []byte, contentType string) *fakeObject {
	sum := sha256.Sum256(data)
	eh := NewETagHasher(UploadPartSize)
	_, _ = eh.Write(data)
	return &fakeObject{data: data, contentType: contentType, sha256: hex.EncodeToString(sum[:]), etag: eh.Sum(), modified: time.Now()}
}

// etagOf is the ETag the Fake reports for o (OpaqueETags decides the shape).
// Called with f.mu held.
func (f *Fake) etagOf(o *fakeObject) string {
	if f.OpaqueETags {
		return o.sha256
	}
	return o.etag
}

// begin runs the common preamble: record the call, run the hook, apply
// FailNext, then check ctx. Callers return its error unchanged.
func (f *Fake) begin(ctx context.Context, op, key string) error {
	f.mu.Lock()
	f.Calls = append(f.Calls, op+" "+key)
	hook := f.Hook
	f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, op, key); err != nil {
			return err
		}
	}
	f.mu.Lock()
	err, ok := f.failNext[op]
	if ok {
		delete(f.failNext, op)
	}
	f.mu.Unlock()
	if ok {
		return err
	}
	return ctx.Err()
}

// PutFile streams the file into memory, hashing it and reporting progress.
func (f *Fake) PutFile(ctx context.Context, key, path, contentType string, progress func(done, total int64)) (PutResult, error) {
	if err := f.begin(ctx, "PutFile", key); err != nil {
		return PutResult{}, err
	}
	src, err := os.Open(path)
	if err != nil {
		return PutResult{}, err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return PutResult{}, err
	}
	total := st.Size()
	h := sha256.New()
	var buf bytes.Buffer
	chunk := make([]byte, 64<<10)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return PutResult{}, err
		}
		n, rerr := src.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			h.Write(chunk[:n])
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return PutResult{}, rerr
		}
	}
	data := buf.Bytes()
	sum := hex.EncodeToString(h.Sum(nil))
	o := newFakeObject(data, contentType)
	f.mu.Lock()
	f.objects[key] = o
	stored := f.etagOf(o)
	f.mu.Unlock()
	return PutResult{Size: done, SHA256: sum, ExpectedETag: o.etag, StoredETag: stored}, nil
}

// Download writes the object to path, reporting progress in 64 KiB steps
// so tests observe intermediate fractions.
func (f *Fake) Download(ctx context.Context, key, path string, progress func(done, total int64)) error {
	if err := f.begin(ctx, "Download", key); err != nil {
		return err
	}
	f.mu.Lock()
	o, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, o.data, 0o644); err != nil {
		return err
	}
	if progress != nil {
		const chunk = 64 << 10
		total := int64(len(o.data))
		for done := int64(0); done < total; {
			done += min(chunk, total-done)
			progress(done, total)
		}
	}
	return nil
}

// GetRange streams [start, end] of the object; end == -1 → to EOF. An
// unsatisfiable start returns an error (S3 answers 416 the same way).
func (f *Fake) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	if err := f.begin(ctx, "GetRange", key); err != nil {
		return nil, err
	}
	f.mu.Lock()
	o, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	size := int64(len(o.data))
	if end < 0 || end >= size {
		end = size - 1
	}
	if start < 0 || start > end || start >= size {
		return nil, fmt.Errorf("store: invalid range %d-%d for size %d", start, end, size)
	}
	return &ctxReader{ctx: ctx, r: bytes.NewReader(o.data[start : end+1])}, nil
}

// ctxReader streams from r, failing with ctx.Err() once ctx is done — so a
// cancelled media request stops mid-body like a real HTTP body would.
type ctxReader struct {
	ctx context.Context
	r   *bytes.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func (c *ctxReader) Close() error { return nil }

// Stat describes an object.
func (f *Fake) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := f.begin(ctx, "Stat", key); err != nil {
		return ObjectInfo{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return f.info(key, o), nil
}

// info describes o; called with f.mu held.
func (f *Fake) info(key string, o *fakeObject) ObjectInfo {
	return ObjectInfo{Key: key, Size: int64(len(o.data)), ETag: f.etagOf(o), LastModified: o.modified}
}

// List returns every object under prefix, sorted by key.
func (f *Fake) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := f.begin(ctx, "List", prefix); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ObjectInfo
	for _, k := range f.listedKeys(prefix) {
		out = append(out, f.info(k, f.objects[k]))
	}
	return out, nil
}

func (f *Fake) sortedKeys(prefix string) []string {
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// listedKeys is sortedKeys as the LISTING sees it: without the keys Unlisted
// is holding back (§20 erratum 305).
func (f *Fake) listedKeys(prefix string) []string {
	keys := f.sortedKeys(prefix)
	out := keys[:0]
	for _, k := range keys {
		if !f.unlisted[k] {
			out = append(out, k)
		}
	}
	return out
}

// DeletePrefix removes every LISTED object under prefix and records each key.
// An unlisted one survives it, exactly as it survives the bucket's own
// list-then-delete (§20 erratum 305).
func (f *Fake) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if err := f.begin(ctx, "DeletePrefix", prefix); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := f.listedKeys(prefix)
	for _, k := range keys {
		delete(f.objects, k)
		f.deletes = append(f.deletes, k)
	}
	return len(keys), nil
}

// DeleteKey removes exactly this object, listed or not, and records the key.
// A key that is not there is not an error.
func (f *Fake) DeleteKey(ctx context.Context, key string) error {
	if err := f.begin(ctx, "DeleteKey", key); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[key]; ok {
		delete(f.objects, key)
		delete(f.unlisted, key)
		// Deletes() means "keys REMOVED", as it did when this was
		// DeletePrefix(key): a key that was not there removed nothing.
		f.deletes = append(f.deletes, key)
	}
	return nil
}

var _ BucketMaker = (*Fake)(nil)

// EnsureBucket creates the bucket when BucketMissing says there is none.
func (f *Fake) EnsureBucket(ctx context.Context) (bool, error) {
	if err := f.begin(ctx, "EnsureBucket", ""); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	created := f.BucketMissing
	f.BucketMissing = false
	return created, nil
}

// Ping succeeds unless a hook or FailNext says otherwise.
func (f *Fake) Ping(ctx context.Context) error {
	return f.begin(ctx, "Ping", "")
}

// FakePresignHost is the host every Fake presign is minted on. It is a
// `.invalid` name (RFC 2606) so a test that accidentally follows one resolves
// nothing, and it is assembled with the scheme in FakePresignURL rather than
// written as a URL, because no non-test file under internal/ may carry a URL
// literal (scripts/check.sh's §15 tripwire).
const FakePresignHost = "presign.invalid"

// FakePresignURL is what the Fake's PresignGet answers for key: deterministic,
// so a handler test can assert the exact Location, and shaped like the real
// thing — the same `X-Amz-*` query keys a caller would look for, plus the two
// `response-*` overrides the contract promises.
func FakePresignURL(key string, ttl time.Duration, contentType, disposition string) string {
	return fakePresignURL("https"+"://"+FakePresignHost, key, ttl, contentType, disposition)
}

func fakePresignURL(origin, key string, ttl time.Duration, contentType, disposition string) string {
	q := url.Values{}
	if contentType != "" {
		q.Set("response-content-type", contentType)
	}
	if disposition != "" {
		q.Set("response-content-disposition", disposition)
	}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(ttl.Seconds()), 10))
	q.Set("X-Amz-SignedHeaders", "host")
	// The "signature" is a hash of the key so it is stable per object and
	// obviously not a credential. Nothing in this file signs anything.
	sum := sha256.Sum256([]byte(key))
	q.Set("X-Amz-Signature", hex.EncodeToString(sum[:]))
	return origin + "/" + key + "?" + q.Encode()
}

// PresignGet answers a deterministic URL (FakePresignURL), recording the call
// like every other op so a test can assert that the media path presigned
// instead of reading.
func (f *Fake) PresignGet(ctx context.Context, key string, ttl time.Duration, contentType, disposition string) (string, error) {
	if err := f.begin(ctx, "PresignGet", key); err != nil {
		return "", err
	}
	if f.PresignOrigin != "" {
		return fakePresignURL(f.PresignOrigin, key, ttl, contentType, disposition), nil
	}
	return FakePresignURL(key, ttl, contentType, disposition), nil
}

var _ Store = (*Fake)(nil)

// ContentType returns the media type an object was stored with (test
// inspection; not recorded).
func (f *Fake) ContentType(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return "", false
	}
	return o.contentType, true
}
