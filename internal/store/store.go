// Package store abstracts where a clip's objects live (DESIGN.md §6, §18.1).
// The Store interface is implemented by minio.go (Stream D) for an
// S3-compatible bucket, by fs.go for a plain directory (ROADMAP #37, §20
// erratum 276) and by Fake for tests. Open picks the first two apart on
// STORAGE_BACKEND; nothing above this package knows which one answered.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"clippet/internal/config"
)

// Open builds the Store this configuration selects (§20 erratum 276). It is
// the ONE place that decision is made — `serve`, `admin` and `doctor` all come
// through here — so adding a third backend later touches this function and
// nothing above it.
func Open(cfg config.Config) (Store, error) {
	if cfg.StorageBackend == config.BackendFS {
		// STORAGE_OP_TIMEOUT travels to both backends: it is about the clock
		// rather than about a bucket, and a hung mount is exactly what it
		// bounds on `fs` (§20 erratum 276(q)). The other two storage timeouts
		// have no meaning there and DEPLOY §7 says so.
		return NewFS(cfg.StorageDir, cfg.S3.OpTimeout)
	}
	return New(cfg.S3)
}

// MediaOriginProbeKey is the key MediaOrigin signs. It deliberately does not
// exist and is never fetched: only the URL's ORIGIN is read off the result, and
// a presign is local HMAC with no network call of any kind, so a key nothing
// has ever written is exactly as good as a real one and cannot race a sweeper.
const MediaOriginProbeKey = "clippet-media-origin-probe"

// MediaOrigin is the `scheme://host[:port]` a presigned media URL for this
// store ACTUALLY lands on (§20 erratum 278, amended). It is what the CSP must
// allow beyond 'self', and what `admin doctor`'s `media` row reports.
//
// It is derived from a real presign rather than from the configured endpoint
// because the two DIFFER whenever the bucket lookup is virtual-host — which is
// `S3_FORCE_PATH_STYLE=false`, the binary's own default and what DEPLOY §7's R2
// and B2 stanzas and §33.3's recipe all set. minio-go then prefixes the BUCKET
// to the host, so a CSP built from `S3_ENDPOINT` names `acct.example` while the
// redirect lands on `bucket.acct.example`. CSP host matching is exact, so the
// watch page and the embed player would be blocked by our own policy —
// silently, with nothing in the response to explain it — on every deployment
// §33 exists for. (It worked only on the bundled MinIO, which compose pins to
// path style, which is why the E2E could not see it.)
//
// Asking the SIGNER is immune to that and to any future lookup rule. The
// alternative — prepending the bucket here when ForcePathStyle is false —
// duplicates minio-go's rule in a second place and would rot the first time the
// library changed it.
//
// ttl is passed through only because a signature needs one; nothing about the
// origin depends on it.
func MediaOrigin(ctx context.Context, s Store, ttl time.Duration) (string, error) {
	raw, err := s.PresignGet(ctx, MediaOriginProbeKey, ttl, "", "")
	if err != nil {
		return "", err
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("store: media origin: the presign produced no usable origin (%q): %v", raw, err)
	}
	return u.Scheme + "://" + u.Host, nil
}

// BucketMaker is implemented by a backend that has a bucket to create: the
// minio-go store. `clippet admin objects init` asks for it (§20 erratum 346)
// and says there is nothing to do when the store is a directory, which has no
// bucket and so does not implement it.
//
// EnsureBucket creates the configured bucket when it is missing and reports
// whether it did. It sets no policy of any kind: a new bucket is private, and
// one that already exists is left exactly as it was.
type BucketMaker interface {
	EnsureBucket(ctx context.Context) (created bool, err error)
}

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

// PutResult reports what PutFile stored. SHA256 (hex) is computed while
// uploading and becomes the object's ETag in the database.
//
// ExpectedETag and StoredETag are §20 erratum 337's proof of a write: the
// S3-shaped ETag predicted from the bytes in the SAME pass that computed
// SHA256 (etag.go), and the ETag the provider answered the write with. A
// backend with no ETag (`fs`, erratum 276) leaves both empty, and Verified is
// then false — which sends a caller that needs certainty to a read-back.
type PutResult struct {
	Size   int64
	SHA256 string

	ExpectedETag string
	StoredETag   string
}

// Verified reports whether the provider's ETag for this write is the one the
// bytes predict (ETagMatches). False is not a failure: it means only that the
// write has not been proved this way.
func (r PutResult) Verified() bool { return ETagMatches(r.ExpectedETag, r.StoredETag) }

// Sentinel errors.
var (
	// ErrNotFound: the key does not exist.
	ErrNotFound = errors.New("store: object not found")
	// ErrPrefixNotEmpty: DeletePrefix re-listed the prefix and found objects.
	ErrPrefixNotEmpty = errors.New("store: prefix not empty after delete")
	// ErrIdleTimeout: no bytes moved for S3Config.IdleTimeout (transient class).
	ErrIdleTimeout = errors.New("store: transfer idle timeout")
	// ErrPresignUnsupported: this backend cannot mint a URL for an object
	// (§20 erratum 278). A directory has no origin, so `fs` answers every
	// PresignGet with this — which is why MEDIA_DELIVERY=redirect on `fs` is
	// refused at BOOT and can never be reached from a request.
	ErrPresignUnsupported = errors.New("store: this backend cannot presign a URL")
)

// Ping failure classes (§20 erratum 31). They are the ONLY storage detail
// the public probes may print: §15 says /healthz exposes no config values,
// and a real ping failure is a net/http *url.Error whose text carries the
// whole request URL — S3_ENDPOINT (for R2 the account id) and the bucket.
const (
	PingUnreachable   = "unreachable"    // dial/DNS/TLS/transport failure
	PingTimeout       = "timeout"        // the ctx deadline (STORAGE_OP_TIMEOUT) expired
	PingDenied        = "denied"         // the credentials were rejected (401/403)
	PingBucketMissing = "bucket missing" // the bucket does not exist
	PingUnavailable   = "unavailable"    // anything else, including unclassified
)

// PingError is what Store.Ping returns: the full error for the operator
// (logs, admin) and a bounded Class for the anonymous /healthz and /readyz
// probes, which render HealthClass() and never Error().
type PingError struct {
	Class string
	Err   error
}

func (e *PingError) Error() string { return e.Err.Error() }
func (e *PingError) Unwrap() error { return e.Err }

// HealthClass satisfies the interface httpx.StorageHealth looks for.
func (e *PingError) HealthClass() string {
	if e.Class == "" {
		return PingUnavailable
	}
	return e.Class
}

// Public failure classes for a transfer or control-plane call (§20 erratum
// 90). They are the SAME idea as the ping classes above, for the other text a
// storage failure reaches an anonymous reader through: the worker writes a
// failed clip's last reason into `clips.failure_reason`, which GET
// /api/clips/{id}, GET …/progress and the share page all render, and a failed
// preview's reason into the gif route's 503. A minio-go transfer failure is a
// net/http *url.Error whose text carries the whole request URL — S3_ENDPOINT
// (for R2 the account id), the bucket and the key layout — so the public text
// gets the class and the log gets the error.
//
// It is a CLOSED set on purpose: a reader learns that storage was the problem
// and nothing about where the storage is.
const (
	PublicUnavailable = "storage unavailable" // refused, denied, unreachable, anything unclassified
	PublicNotFound    = "object missing"      // the key is not in the bucket
	PublicTimeout     = "storage timeout"     // an idle transfer, or a deadline that expired
)

// StorageError is what a failed transfer or control-plane call returns: the
// full error for the operator (logs, admin) and a bounded Class for any text
// that reaches an anonymous reader. Unwrap keeps `errors.Is(err,
// ErrNotFound)` and friends working exactly as before.
type StorageError struct {
	Class string
	Err   error
}

func (e *StorageError) Error() string { return e.Err.Error() }
func (e *StorageError) Unwrap() error { return e.Err }

// PublicClass is the bounded word for this failure.
func (e *StorageError) PublicClass() string {
	if e.Class == "" {
		return PublicUnavailable
	}
	return e.Class
}

// publicClassifier is implemented by an error that knows how to describe
// itself to an anonymous reader.
type publicClassifier interface{ PublicClass() string }

// PublicClass renders the bounded class of a storage failure, or "" when err
// does not classify itself. "" is the signal that the caller must fall back to
// its own redaction: the leak is closed by default, never by enumerating the
// error shapes that would have leaked.
func PublicClass(err error) string {
	var c publicClassifier
	if errors.As(err, &c) {
		return c.PublicClass()
	}
	return ""
}

// Store is the bucket contract. Every method honours ctx; transfers are
// bounded by the implementation's progress-based idle watchdog, never by an
// absolute body deadline (§8.5, §18.4).
type Store interface {
	// PutFile uploads the file at path under key with contentType, calling
	// progress(done, total) as bytes move (progress may be nil). progress may
	// be called from a goroutine other than the caller's.
	PutFile(ctx context.Context, key, path, contentType string, progress func(done, total int64)) (PutResult, error)
	// Download writes the object to path, calling progress(done, total) as
	// bytes arrive (progress may be nil; total ≤ 0 when the size is unknown).
	Download(ctx context.Context, key, path string, progress func(done, total int64)) error
	// GetRange streams bytes [start, end] (end inclusive; end == -1 → to EOF).
	GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error)
	// Stat describes an object. Admin/tests only — never in the media hot path.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// List returns every object under prefix.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	// DeletePrefix removes every object and incomplete multipart upload under
	// prefix and verifies the prefix is empty afterwards. It LISTS first, so
	// it removes what the backend is willing to enumerate and nothing else.
	DeletePrefix(ctx context.Context, prefix string) (deleted int, err error)
	// DeleteKey removes EXACTLY this object, and any incomplete multipart
	// upload of it, WITHOUT consulting a listing. A key that is not there is
	// not an error.
	//
	// It exists because DeletePrefix cannot remove what a listing does not yet
	// show (§20 erratum 305). A bucket answers a key-addressed delete from the
	// object itself; its listing is an index, and R2's is eventually
	// consistent, so an object written a millisecond ago can be missing from
	// the very next List — and the keys a run cleans up after itself are
	// exactly the ones it wrote a millisecond ago. Cleanup by key is therefore
	// the only cleanup that is certain to have happened.
	DeleteKey(ctx context.Context, key string) error
	// Ping checks the bucket is reachable (BucketExists).
	Ping(ctx context.Context) error
	// PresignGet returns a URL that fetches the object directly from the
	// backend's own origin for ttl, with the response Content-Type and
	// Content-Disposition the CALLER names rather than whatever the object
	// was stored with (§20 erratum 278). It makes NO network call — a SigV4
	// query signature is local HMAC over the request line — so §15's list of
	// outbound destinations is unchanged by it.
	//
	// A backend with no origin returns ErrPresignUnsupported.
	PresignGet(ctx context.Context, key string, ttl time.Duration, contentType, disposition string) (string, error)
}
