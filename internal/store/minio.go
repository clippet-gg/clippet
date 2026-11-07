package store

// minio.go — the minio-go backed Store (DESIGN.md §6, §8.5, §18.4).
//
// Timeouts, in one place:
//
//   - Control-plane calls (List, Remove, Stat, Ping, incomplete-upload
//     listing and abort) each run under cfg.OpTimeout.
//   - The transport's ResponseHeaderTimeout (= cfg.OpTimeout, set in New)
//     bounds the header wait of every request, never a body.
//   - Transfers are PROGRESS-based: PutFile and Download abort when no bytes
//     have moved for cfg.IdleTimeout (→ ErrIdleTimeout) under an absolute
//     cfg.TransferMax deadline. GetRange bodies carry the idle watchdog only,
//     armed solely while a Read is waiting on S3 — a consumer that stops
//     reading (a browser with a full media buffer) never trips it; the
//     caller's ctx (a client disconnect) is the only other thing that ends
//     a media stream (§8.5).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"clippet/internal/config"
)

const (
	// partSize is the multipart part size for PutFile (§18.4). Objects up
	// to this size go up in one PUT.
	partSize = 16 << 20
	// removeBatch is the S3 multi-delete maximum (§6).
	removeBatch = 1000
	// defaultOpTimeout guards a zero S3Config.OpTimeout (a config built by
	// hand in tests); config.Load never produces one.
	defaultOpTimeout = 30 * time.Second
	// abortTimeout bounds the best-effort multipart abort after a failed
	// PutFile (its own ctx: the transfer ctx is usually what failed).
	abortTimeout = 10 * time.Second
	// defaultUploadThreads guards a zero S3Config.UploadThreads (a config built
	// by hand in tests); config.Load's default is the same 6 (§20 erratum 337).
	defaultUploadThreads = 6
)

// New builds the minio-go backed Store from cfg. It performs no network I/O;
// the first contact with the bucket is Ping (§8.7).
func New(cfg config.S3Config) (Store, error) {
	client, err := newClient(cfg, cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	m := &minioStore{c: client, signer: client, cfg: cfg}
	// The SIGNER is a second client whenever a viewer reaches the bucket at a
	// different origin from the one this process uses (§20 erratum 278). SigV4
	// signs the HOST, so the two cannot share a signature. It exists purely to
	// carry that host, never opens a connection, and is the SAME client
	// wherever the two origins agree — which is every deployment on R2, B2 or
	// S3, and the reason `proxy` mode allocates nothing extra.
	if p := cfg.PresignEndpoint; p != "" && p != cfg.Endpoint {
		signer, err := newClient(cfg, p)
		if err != nil {
			return nil, err
		}
		m.signer = signer
	}
	return m, nil
}

// newClient builds one minio client against endpoint. Region is pinned from
// the configuration so nothing is ever PROBED: with no region minio-go asks
// the bucket where it lives before it can sign, which would turn a presign —
// local HMAC, by design (§15) — into a network call on the media hot path.
func newClient(cfg config.S3Config, endpoint string) (*minio.Client, error) {
	return newClientOn(cfg, endpoint, nil)
}

// newClientOn is newClient over base, the HTTP transport under wireTransport.
// nil — every production caller — is minio-go's default transport with the
// header wait bounded; a test hands in one whose connections it controls.
func newClientOn(cfg config.S3Config, endpoint string, base http.RoundTripper) (*minio.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("store: invalid S3 endpoint %q", endpoint)
	}
	secure := u.Scheme == "https"
	lookup := minio.BucketLookupDNS
	if cfg.ForcePathStyle {
		lookup = minio.BucketLookupPath
	}
	if base == nil {
		transport, err := minio.DefaultTransport(secure)
		if err != nil {
			return nil, fmt.Errorf("store: transport: %w", err)
		}
		// Bounds the header wait of every request; never a body (§18.4).
		transport.ResponseHeaderTimeout = cfg.OpTimeout
		base = transport
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
		Transport:    wireTransport{base: base},
	})
	if err != nil {
		return nil, fmt.Errorf("store: minio client: %w", err)
	}
	return client, nil
}

type minioStore struct {
	c *minio.Client
	// signer mints the presigned URLs of MEDIA_DELIVERY=redirect. It is `c`
	// unless MEDIA_PRESIGN_ENDPOINT named a different public origin for the
	// same bucket (§20 erratum 278).
	signer *minio.Client
	cfg    config.S3Config
}

var _ Store = (*minioStore)(nil)

// opCtx bounds one control-plane call by cfg.OpTimeout.
func (m *minioStore) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	d := m.cfg.OpTimeout
	if d <= 0 {
		d = defaultOpTimeout
	}
	return context.WithTimeout(ctx, d)
}

// transferCtx applies the absolute cfg.TransferMax cap to one transfer
// (0 = no cap).
func (m *minioStore) transferCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.cfg.TransferMax <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, m.cfg.TransferMax)
}

// Ping checks that the configured bucket exists and is reachable, under
// cfg.OpTimeout.
func (m *minioStore) Ping(ctx context.Context) error {
	ctx, cancel := m.opCtx(ctx)
	defer cancel()
	ok, err := m.c.BucketExists(ctx, m.cfg.Bucket)
	if err != nil {
		return &PingError{Class: pingClass(err), Err: fmt.Errorf("store: ping: %w", err)}
	}
	if !ok {
		return &PingError{Class: PingBucketMissing, Err: fmt.Errorf("store: bucket %q does not exist", m.cfg.Bucket)}
	}
	return nil
}

var _ BucketMaker = (*minioStore)(nil)

// EnsureBucket creates the bucket when BucketExists says it is missing (§20
// erratum 346), under cfg.OpTimeout. It never touches a policy: the harness's
// old `bucket-init` ran `mc anonymous set none` on a bucket it had just made,
// which on a new bucket is already the state. A bucket this account already
// owns is not an error (a second init racing the first); one somebody else owns
// is (BucketAlreadyExists), because then the name is not ours to write into.
func (m *minioStore) EnsureBucket(ctx context.Context) (bool, error) {
	ctx, cancel := m.opCtx(ctx)
	defer cancel()
	ok, err := m.c.BucketExists(ctx, m.cfg.Bucket)
	if err != nil {
		return false, fmt.Errorf("store: bucket %q: %w", m.cfg.Bucket, err)
	}
	if ok {
		return false, nil
	}
	if err := m.c.MakeBucket(ctx, m.cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == "BucketAlreadyOwnedByYou" {
			return false, nil
		}
		return false, fmt.Errorf("store: create bucket %q: %w", m.cfg.Bucket, err)
	}
	return true, nil
}

// pingClass buckets a BucketExists failure into one of the §20 erratum 31
// classes. The full error keeps every detail for the operator; only this
// word reaches the public probes.
func pingClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return PingTimeout
	}
	if resp := minio.ToErrorResponse(err); resp.Code != "" || resp.StatusCode != 0 {
		switch {
		case resp.Code == "NoSuchBucket", resp.StatusCode == http.StatusNotFound:
			return PingBucketMissing
		case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusUnauthorized,
			resp.Code == "AccessDenied", resp.Code == "InvalidAccessKeyId", resp.Code == "SignatureDoesNotMatch":
			return PingDenied
		}
		return PingUnavailable
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return PingTimeout
		}
		return PingUnreachable
	}
	return PingUnavailable
}

// PutFile uploads the file at path under key, hashing it with sha256 on the
// way out and predicting its S3-shaped ETag in the same pass (§20 erratum
// 337). Objects up to partSize are buffered so the request body is seekable
// and minio-go can retry the single PUT; larger files stream through the
// multipart path in 16 MiB parts and are read exactly once, in order.
//
// The multipart path is PARALLEL since erratum 337: `ConcurrentStreamParts`
// fills S3Config.UploadThreads buffers of partSize from the stream one after
// another and uploads them at the same time, which is what takes a large
// `admin storage migrate` to R2 off one stream's throughput. It works from any
// io.Reader, so the sha256/MD5 TeeReader is untouched. MEMORY: about
// UploadThreads × 16 MiB per running upload (96 MiB at the default 6), held
// only while it runs. UploadThreads = 1 is minio-go's sequential path, which
// is the store before this erratum.
//
// `SendContentMd5` puts a Content-MD5 on every part (and on the single PUT),
// so the provider REFUSES a part that arrived different from what was sent,
// at write time. The part ETags that come back are those MD5s, which is what
// makes ExpectedETag predictable.
//
// progress reports bytes read from the file. On the parallel path that runs
// up to UploadThreads parts ahead of the wire, so the idle watchdog is ALSO
// touched by the wire itself (wireTransport): a slow uplink with every buffer
// in flight is progress, not idleness.
func (m *minioStore) PutFile(ctx context.Context, key, path, contentType string, progress func(done, total int64)) (PutResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return PutResult{}, fmt.Errorf("store: put %s: %w", key, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return PutResult{}, fmt.Errorf("store: put %s: %w", key, err)
	}
	total := st.Size()

	h := sha256.New()
	eh := NewETagHasher(partSize)
	both := io.MultiWriter(h, eh)
	var body io.Reader
	if total <= partSize {
		buf, err := io.ReadAll(f)
		if err != nil {
			return PutResult{}, fmt.Errorf("store: put %s: read: %w", key, err)
		}
		_, _ = both.Write(buf)
		total = int64(len(buf))
		body = bytes.NewReader(buf) // seekable → retryable single PUT
	} else {
		// io.TeeReader exposes only Read, so minio-go never sees ReadAt or
		// Seek and reads the file exactly once, in order.
		body = io.TeeReader(f, both)
	}

	threads := m.cfg.UploadThreads
	if threads <= 0 {
		threads = defaultUploadThreads
	}
	tctx, cancelMax := m.transferCtx(ctx)
	defer cancelMax()
	wctx, wd := newWatchdog(tctx, m.cfg.IdleTimeout)
	defer wd.stop()
	wctx = context.WithValue(wctx, wireWatchdogKey{}, wd)
	hook := &progressHook{wd: wd, total: total, fn: progress}
	info, err := m.c.PutObject(wctx, m.cfg.Bucket, key, body, total, minio.PutObjectOptions{
		ContentType:           contentType,
		PartSize:              partSize,
		Progress:              hook,
		SendContentMd5:        true,
		ConcurrentStreamParts: threads > 1,
		NumThreads:            uint(threads),
	})
	if err != nil {
		if total > partSize {
			m.abortUpload(key)
		}
		return PutResult{}, m.transferErr("put", key, ctx, wd, err)
	}
	return PutResult{Size: total, SHA256: hex.EncodeToString(h.Sum(nil)),
		ExpectedETag: eh.Sum(), StoredETag: NormalizeETag(info.ETag)}, nil
}

// abortUpload aborts any multipart upload left behind for key after a
// failed PutFile, best-effort under a fresh ctx. minio-go aborts on error
// itself but reuses the transfer ctx, so when that ctx is what failed
// (cancel, idle watchdog, TransferMax) its abort fails silently and the
// parts would linger — invisible to a plain list and billed on R2/B2. By
// exact key this works on every provider, including MinIO (see
// abortIncomplete).
func (m *minioStore) abortUpload(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), abortTimeout)
	defer cancel()
	_ = m.c.RemoveIncompleteUpload(ctx, m.cfg.Bucket, key)
}

// Download streams the object into path (parent directories are created),
// writing to path+".part" and renaming on success so a failed transfer
// never leaves a truncated file under the final name. progress(done, total)
// is fed as bytes arrive (§10.3 step 1's acquire fraction).
func (m *minioStore) Download(ctx context.Context, key, path string, progress func(done, total int64)) error {
	tctx, cancelMax := m.transferCtx(ctx)
	defer cancelMax()
	wctx, wd := newWatchdog(tctx, m.cfg.IdleTimeout)
	defer wd.stop()

	obj, err := m.c.GetObject(wctx, m.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return mapErr("download", key, err)
	}
	defer obj.Close()
	total := int64(-1)
	if progress != nil {
		st, serr := obj.Stat()
		if serr != nil {
			return m.transferErr("download", key, ctx, wd, serr)
		}
		total = st.Size
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("store: download %s: %w", key, err)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("store: download %s: %w", key, err)
	}
	_, err = io.Copy(f, &watchedReader{r: obj, wd: wd, fn: progress, total: total})
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return m.transferErr("download", key, ctx, wd, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: download %s: %w", key, err)
	}
	return nil
}

// GetRange streams bytes [start, end] (end == -1 → to EOF) with exactly one
// GET. The request is issued here, not lazily on the first Read, so a
// missing key (ErrNotFound) or an unsatisfiable range surfaces from GetRange
// itself, exactly like the Fake. The body is bounded by the idle watchdog
// (armed only while a Read waits on S3, see rangeBody.Read) and the
// caller's ctx — there is no absolute deadline (§8.5).
func (m *minioStore) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	hdr, err := rangeHeader(start, end)
	if err != nil {
		return nil, fmt.Errorf("store: get %s: %w", key, err)
	}
	opts := minio.GetObjectOptions{}
	if hdr != "" {
		opts.Set("Range", hdr)
	}
	wctx, wd := newWatchdog(ctx, m.cfg.IdleTimeout)
	obj, err := m.c.GetObject(wctx, m.cfg.Bucket, key, opts)
	if err != nil {
		wd.stop()
		return nil, mapErr("get", key, err)
	}
	body := &rangeBody{obj: obj, wd: wd, key: key}
	// minio.Object issues the GET on its first Read; a one-byte read makes
	// that happen now (the byte is handed back through the MultiReader).
	var first [1]byte
	n, err := obj.Read(first[:])
	if err != nil && err != io.EOF {
		_ = body.Close()
		return nil, body.mapErr(err)
	}
	wd.pause() // re-armed by each Read that goes to S3
	body.r = io.MultiReader(bytes.NewReader(first[:n]), obj)
	return body, nil
}

// PresignGet mints a SigV4 query-signed GET for key, valid for ttl, with the
// response headers the caller names (§20 erratum 278).
//
// Three things about it are load-bearing:
//
//   - **No network call.** A query signature is HMAC over the canonical
//     request; minio-go contacts nothing as long as the region is known, and
//     newClient pins it from the configuration for exactly this reason. §15's
//     "outbound traffic goes to S3_ENDPOINT and to Discord" therefore survives
//     redirect mode intact: the app opens no new destination, the VIEWER does.
//   - **The response type comes from the ROW, not from the object.** The media
//     routes have always set Content-Type from the database rather than trusting
//     what the bucket stored (§15's content-type discipline), and the
//     `response-content-type` override is how that rule survives when the app
//     is no longer the one writing the header. MinIO, R2, B2 and AWS S3 all
//     honour the `response-*` overrides on a presigned GET.
//   - **The disposition carries erratum 98's presentation rule.** `?download=1`
//     asks for a save rather than for different bytes, and in redirect mode the
//     query string is the only place that ask can travel to.
func (m *minioStore) PresignGet(ctx context.Context, key string, ttl time.Duration, contentType, disposition string) (string, error) {
	params := url.Values{}
	if contentType != "" {
		params.Set("response-content-type", contentType)
	}
	if disposition != "" {
		params.Set("response-content-disposition", disposition)
	}
	u, err := m.signer.PresignedGetObject(ctx, m.cfg.Bucket, key, ttl, params)
	if err != nil {
		return "", mapErr("presign", key, err)
	}
	return u.String(), nil
}

// Stat describes an object (admin/tests only; never in the media path).
func (m *minioStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	ctx, cancel := m.opCtx(ctx)
	defer cancel()
	info, err := m.c.StatObject(ctx, m.cfg.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, mapErr("stat", key, err)
	}
	return toInfo(info), nil
}

// List returns every object under prefix (recursive), under one OpTimeout.
func (m *minioStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	ctx, cancel := m.opCtx(ctx)
	defer cancel()
	var out []ObjectInfo
	for o := range m.c.ListObjects(ctx, m.cfg.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, mapErr("list", prefix, o.Err)
		}
		out = append(out, toInfo(o))
	}
	return out, nil
}

// DeletePrefix removes everything under prefix (§6): list → multi-delete in
// batches of ≤1000 → abort every incomplete multipart upload under the
// prefix (invisible to a plain list, billed on R2/B2) → re-list; anything
// left → ErrPrefixNotEmpty. Every sub-call has its own OpTimeout. deleted
// counts objects removed before any error.
//
// It is a LISTING that decides what this removes, and a listing is an index
// rather than the objects (§20 erratum 305). A caller that knows the key it
// wants gone — every cleanup a run does of its own uploads — calls DeleteKey
// instead, which asks the object.
func (m *minioStore) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	objs, err := m.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for start := 0; start < len(objs); start += removeBatch {
		end := min(start+removeBatch, len(objs))
		n, err := m.removeBatch(ctx, objs[start:end])
		deleted += n
		if err != nil {
			return deleted, err
		}
	}
	if err := m.abortIncomplete(ctx, prefix); err != nil {
		return deleted, err
	}
	left, err := m.List(ctx, prefix)
	if err != nil {
		return deleted, err
	}
	if len(left) > 0 {
		return deleted, fmt.Errorf("store: delete prefix %s: %d objects remain: %w", prefix, len(left), ErrPrefixNotEmpty)
	}
	return deleted, nil
}

// DeleteKey removes exactly this object and any incomplete multipart upload of
// it, asking neither ListObjects nor anything else what exists (§20 erratum
// 305). S3 and R2 both answer DELETE for a key that is not there with 204, so a
// key already gone is success — which is what the contract wants: after this
// call the key does not exist.
//
// This is the delete a RUN uses on the outputs of its own attempt. It was
// DeletePrefix(key) — a key is its own prefix — and that spelling made every
// such cleanup depend on the bucket's listing having caught up with a PutFile
// that had returned milliseconds earlier. On the bundled MinIO it always had;
// on R2, whose listing is eventually consistent, it sometimes had not, and the
// object was then left behind by a delete that reported success (the fifteenth
// gate's leaked rendition). abortIncomplete is kept because an aborted PutFile
// of a multi-part object leaves an upload no object delete removes, and it is
// called with the EXACT key, which is the one form MinIO also implements.
func (m *minioStore) DeleteKey(ctx context.Context, key string) error {
	rctx, cancel := m.opCtx(ctx)
	err := m.c.RemoveObject(rctx, m.cfg.Bucket, key, minio.RemoveObjectOptions{})
	cancel()
	if err != nil {
		return mapErr("remove", key, err)
	}
	return m.abortIncomplete(ctx, key)
}

// removeBatch multi-deletes one batch (≤ removeBatch keys) under one
// OpTimeout and reports how many of them were removed.
func (m *minioStore) removeBatch(ctx context.Context, batch []ObjectInfo) (int, error) {
	ctx, cancel := m.opCtx(ctx)
	defer cancel()
	ch := make(chan minio.ObjectInfo, len(batch))
	for _, o := range batch {
		ch <- minio.ObjectInfo{Key: o.Key}
	}
	close(ch)
	failed := 0
	var first error
	for e := range m.c.RemoveObjects(ctx, m.cfg.Bucket, ch, minio.RemoveObjectsOptions{}) {
		failed++
		if first == nil {
			first = mapErr("remove", e.ObjectName, e.Err)
		}
	}
	if first == nil && ctx.Err() != nil {
		// The error channel closed because the ctx ended, not because the
		// batch finished; report it rather than counting a phantom success.
		first = fmt.Errorf("store: remove batch: %w", ctx.Err())
		failed = len(batch)
	}
	return len(batch) - failed, first
}

// abortIncomplete aborts every in-progress multipart upload under prefix.
//
// Provider note: AWS, R2 and B2 list multipart uploads by prefix, so a
// whole clip prefix is swept here. MinIO deliberately implements
// ListMultipartUploads for an exact object name only (its erasure backend
// documents "we do not support prefix based listing"), so on MinIO this
// finds uploads only when prefix IS the key — the worker's per-attempt
// DeletePrefix(key) — and a clip-wide sweep relies on PutFile's own
// exact-key abort (abortUpload) plus MinIO's stale-upload expiry
// (MINIO_API_STALE_UPLOADS_EXPIRY, 24h by default). Verified against
// minio/minio:RELEASE.2025-09-07 by the -tags integration test.
func (m *minioStore) abortIncomplete(ctx context.Context, prefix string) error {
	lctx, cancel := m.opCtx(ctx)
	keys := map[string]bool{}
	var order []string
	for u := range m.c.ListIncompleteUploads(lctx, m.cfg.Bucket, prefix, true) {
		if u.Err != nil {
			cancel()
			return mapErr("list incomplete uploads", prefix, u.Err)
		}
		if u.Key != "" && !keys[u.Key] {
			keys[u.Key] = true
			order = append(order, u.Key)
		}
	}
	cancel()
	for _, key := range order {
		actx, acancel := m.opCtx(ctx)
		err := m.c.RemoveIncompleteUpload(actx, m.cfg.Bucket, key)
		acancel()
		if err != nil {
			return mapErr("abort incomplete upload", key, err)
		}
	}
	return nil
}

// transferErr classifies a failed PutFile/Download: the idle watchdog wins
// (ErrIdleTimeout), then the absolute TransferMax deadline (when the
// caller's own ctx is still alive), else the mapped storage error.
func (m *minioStore) transferErr(op, key string, parent context.Context, wd *watchdog, err error) error {
	switch {
	case wd.Fired():
		return &StorageError{Class: PublicTimeout,
			Err: fmt.Errorf("store: %s %s: no progress for %s: %w", op, key, m.cfg.IdleTimeout, ErrIdleTimeout)}
	case parent.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		return &StorageError{Class: PublicTimeout,
			Err: fmt.Errorf("store: %s %s: exceeded STORAGE_TRANSFER_MAX %s: %w", op, key, m.cfg.TransferMax, err)}
	}
	return mapErr(op, key, err)
}

// mapErr wraps a minio error, translating a missing object into ErrNotFound
// (both the explicit NoSuchKey code and the bare 404 minio-go synthesises
// for HEAD responses arrive as NoSuchKey). Every result carries its bounded
// PublicClass (§20 erratum 90): the wrapped text names the key, and for a
// transport failure minio-go's own error names the endpoint and the bucket,
// none of which belongs in a public failure_reason.
func mapErr(op, key string, err error) error {
	if err == nil {
		return nil
	}
	if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
		return &StorageError{Class: PublicNotFound, Err: fmt.Errorf("store: %s %s: %w", op, key, ErrNotFound)}
	}
	return &StorageError{Class: publicClass(err), Err: fmt.Errorf("store: %s %s: %w", op, key, err)}
}

// publicClass buckets a transfer/control-plane failure into one of the three
// §20 erratum 90 classes. Like pingClass, it names only what KIND of trouble
// storage was in; the full error keeps every detail for the operator.
func publicClass(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return PublicNotFound
	case errors.Is(err, ErrIdleTimeout), errors.Is(err, context.DeadlineExceeded):
		return PublicTimeout
	}
	if resp := minio.ToErrorResponse(err); resp.Code != "" || resp.StatusCode != 0 {
		if resp.Code == minio.NoSuchKey || resp.Code == "NoSuchBucket" || resp.StatusCode == http.StatusNotFound {
			return PublicNotFound
		}
		return PublicUnavailable
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return PublicTimeout
	}
	return PublicUnavailable
}

func toInfo(o minio.ObjectInfo) ObjectInfo {
	return ObjectInfo{Key: o.Key, Size: o.Size, ETag: o.ETag, LastModified: o.LastModified}
}

// rangeHeader renders [start, end] (end == -1 → to EOF) as an HTTP Range
// header; "" means the whole object. It deliberately avoids
// GetObjectOptions.SetRange, whose (0, -1) case means "last byte".
func rangeHeader(start, end int64) (string, error) {
	switch {
	case start < 0 || end < -1:
		return "", fmt.Errorf("invalid range %d-%d", start, end)
	case end == -1 && start == 0:
		return "", nil
	case end == -1:
		return fmt.Sprintf("bytes=%d-", start), nil
	case start > end:
		return "", fmt.Errorf("invalid range %d-%d", start, end)
	}
	return fmt.Sprintf("bytes=%d-%d", start, end), nil
}

// watchdog cancels its context when the countdown has run for idle without
// a touch(). It starts armed; pause() suspends the countdown until the next
// touch() so a body can be watched only while it is actually waiting on S3.
// idle <= 0 disables the timer; stop() always releases the context.
type watchdog struct {
	idle   time.Duration
	fired  atomic.Bool
	cancel context.CancelFunc

	mu    sync.Mutex // guards timer and armed against the AfterFunc
	timer clockTimer
	armed bool
}

// clock is the idle watchdog's time source, the store's seam of the same shape
// as transcode's Clock (§20 erratum 331): nil — every production caller — is
// the wall clock. A test supplies one it advances by hand, so "bytes kept
// moving faster than the idle timeout" is an Advance and never a sleep that a
// scheduler stall could outlast.
type clock interface {
	AfterFunc(d time.Duration, f func()) clockTimer
}

// clockTimer is the part of *time.Timer the watchdog uses.
type clockTimer interface {
	Stop() bool
	Reset(d time.Duration) bool
}

// wallClock is the production clock.
type wallClock struct{}

func (wallClock) AfterFunc(d time.Duration, f func()) clockTimer { return time.AfterFunc(d, f) }

func newWatchdog(parent context.Context, idle time.Duration) (context.Context, *watchdog) {
	return newWatchdogOn(parent, idle, nil)
}

// newWatchdogOn is newWatchdog on clock c (nil = the wall clock).
func newWatchdogOn(parent context.Context, idle time.Duration, c clock) (context.Context, *watchdog) {
	if c == nil {
		c = wallClock{}
	}
	ctx, cancel := context.WithCancel(parent)
	w := &watchdog{idle: idle, cancel: cancel}
	if idle > 0 {
		w.armed = true
		w.timer = c.AfterFunc(idle, w.expire)
	}
	return ctx, w
}

// expire runs when the countdown ends; a pause() that won the race makes it
// a no-op, so a Read that returned bytes just before is never blamed.
func (w *watchdog) expire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.armed {
		return
	}
	w.fired.Store(true)
	w.cancel()
}

// touch records progress: the idle countdown restarts (and is armed).
func (w *watchdog) touch() {
	if w.timer == nil || w.fired.Load() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed = true
	w.timer.Reset(w.idle)
}

// pause suspends the countdown until the next touch.
func (w *watchdog) pause() {
	if w.timer == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed = false
	w.timer.Stop()
}

// stop disarms the timer and cancels the context.
func (w *watchdog) stop() {
	w.pause()
	w.cancel()
}

// Fired reports whether the idle timeout expired.
func (w *watchdog) Fired() bool { return w.fired.Load() }

// progressHook is handed to minio-go as PutObjectOptions.Progress: minio
// calls Read with every byte slice as it leaves for the wire (a retried
// part re-sends, so done is clamped to total). It feeds the caller's
// progress callback and the idle watchdog.
type progressHook struct {
	wd    *watchdog
	total int64
	sent  atomic.Int64
	fn    func(done, total int64)
}

func (p *progressHook) Read(b []byte) (int, error) {
	n := len(b)
	if n == 0 {
		return 0, nil
	}
	p.wd.touch()
	done := p.sent.Add(int64(n))
	if p.fn != nil {
		p.fn(min(done, p.total), p.total)
	}
	return n, nil
}

// wireWatchdogKey carries a PutFile's watchdog on its request context to
// wireTransport (§20 erratum 337).
type wireWatchdogKey struct{}

// wireTransport touches the watchdog of the PutFile a request belongs to for
// every byte of request BODY the HTTP client sends. Without it the parallel
// multipart path would look idle to the watchdog whenever every part buffer
// is in flight — progressHook sees the FILE being read, and the file is not
// read again until a part finishes — so a 16 MiB part on a slow uplink would
// trip STORAGE_IDLE_TIMEOUT while bytes were moving the whole time. A request
// without the key (every other call) passes through untouched.
type wireTransport struct{ base http.RoundTripper }

func (t wireTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	wd, ok := r.Context().Value(wireWatchdogKey{}).(*watchdog)
	if !ok || r.Body == nil || r.Body == http.NoBody {
		return t.base.RoundTrip(r)
	}
	// A RoundTripper must not modify the request it was handed: a shallow
	// copy carries the wrapped body.
	r2 := r.WithContext(r.Context())
	r2.Body = &wireBody{rc: r.Body, wd: wd}
	return t.base.RoundTrip(r2)
}

// wireBody is a request body that touches a watchdog as it is read.
type wireBody struct {
	rc io.ReadCloser
	wd *watchdog
}

func (b *wireBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.wd.touch()
	}
	return n, err
}

func (b *wireBody) Close() error { return b.rc.Close() }

// watchedReader touches the watchdog on every successful read (Download)
// and feeds the caller's progress callback.
type watchedReader struct {
	r     io.Reader
	wd    *watchdog
	fn    func(done, total int64)
	total int64
	done  int64
}

func (w *watchedReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		w.wd.touch()
		w.done += int64(n)
		if w.fn != nil {
			w.fn(w.done, w.total)
		}
	}
	return n, err
}

// rangeBody is the GetRange body: a watched, error-mapping reader over the
// minio object. Close stops the watchdog and releases the object (and its
// goroutine) exactly once.
type rangeBody struct {
	r    io.Reader
	obj  io.Closer
	wd   *watchdog
	key  string
	once sync.Once
}

// Read arms the watchdog only for the time spent waiting on S3: no bytes
// from S3 for IdleTimeout ends the stream (§8.5), while a consumer that
// stops calling Read — http.ServeContent behind a browser whose media
// buffer is full — leaves the countdown paused for as long as it likes.
func (b *rangeBody) Read(p []byte) (int, error) {
	b.wd.touch()
	n, err := b.r.Read(p)
	b.wd.pause()
	if err != nil && err != io.EOF {
		err = b.mapErr(err)
	}
	return n, err
}

func (b *rangeBody) mapErr(err error) error {
	if b.wd.Fired() {
		return &StorageError{Class: PublicTimeout,
			Err: fmt.Errorf("store: get %s: no progress for %s: %w", b.key, b.wd.idle, ErrIdleTimeout)}
	}
	return mapErr("get", b.key, err)
}

func (b *rangeBody) Close() error {
	var err error
	b.once.Do(func() {
		b.wd.stop()
		err = b.obj.Close()
	})
	return err
}
