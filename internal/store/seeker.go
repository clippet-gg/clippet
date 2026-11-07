package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// errSeekerClosed is what a closed seeker answers: the reader that owned it
// is gone, so a late Read neither streams nor opens a fresh range (§20
// erratum 41).
var errSeekerClosed = errors.New("store: seeker closed")

// NewSeeker returns a lazy io.ReadSeekCloser over one object whose size the
// caller already knows from the database (§8.5). Seek is pure arithmetic —
// no storage call. The first Read after any position change closes the
// previous body and opens exactly one GetRange(key, pos, size-1); subsequent
// Reads stream from it. Reading at or past size returns io.EOF with no call.
//
// Read and Seek are for one goroutine at a time, as any io.ReadSeeker is;
// Close may be called concurrently with them and always wins — it closes the
// open body (unblocking a read that is waiting on the network) and makes
// every later Read/Seek fail instead of opening a new range.
func NewSeeker(ctx context.Context, s Store, key string, size int64) io.ReadSeekCloser {
	return &seeker{ctx: ctx, s: s, key: key, size: size}
}

type seeker struct {
	ctx  context.Context
	s    Store
	key  string
	size int64

	// mu guards every mutable field below. http.ServeContent answers a
	// multi-range request from a goroutine it starts and never joins, which
	// keeps calling Seek/Read after the handler has returned (a client
	// disconnect ends the handler's copy first); Close and the handler's
	// read of the recorded error race that goroutine. The lock is never held
	// across a storage call or a body read, so Close can never wait behind
	// an in-flight GetRange.
	mu      sync.Mutex
	pos     int64         // logical read position
	body    io.ReadCloser // open range body, positioned at pos; nil when closed
	bodyErr error         // sticky error once a body has failed
	closed  bool          // Close was called: no further reads, no new ranges
}

func (k *seeker) Seek(offset int64, whence int) (int64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return 0, errSeekerClosed
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = k.pos + offset
	case io.SeekEnd:
		abs = k.size + offset
	default:
		return 0, fmt.Errorf("store: seeker: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("store: seeker: negative position")
	}
	if abs != k.pos {
		k.closeBodyLocked()
		k.pos = abs
	}
	return abs, nil
}

func (k *seeker) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	k.mu.Lock()
	switch {
	case k.closed:
		k.mu.Unlock()
		return 0, errSeekerClosed
	case k.bodyErr != nil:
		err := k.bodyErr
		k.mu.Unlock()
		return 0, err
	case k.pos >= k.size:
		k.mu.Unlock()
		return 0, io.EOF
	}
	body, pos := k.body, k.pos
	k.mu.Unlock()

	if body == nil {
		var err error
		if body, err = k.openBody(pos); err != nil {
			return 0, err
		}
	}
	n, err := body.Read(p)

	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		// Close ran during the read: the body it closed under us is what
		// ended the read, so err describes the teardown, not the object.
		return 0, errSeekerClosed
	}
	k.pos += int64(n)
	switch {
	case err == io.EOF:
		k.closeBodyLocked()
		if k.pos < k.size {
			// The object is shorter than the database says: row/object drift.
			k.bodyErr = io.ErrUnexpectedEOF
			if n == 0 {
				return 0, k.bodyErr
			}
			return n, nil
		}
		if n == 0 {
			return 0, io.EOF
		}
		return n, nil
	case err != nil:
		k.closeBodyLocked()
		k.bodyErr = err
		return n, err
	}
	return n, nil
}

// openBody opens the one GetRange a read run costs. The lock is released
// across the call — a Close must never queue behind it — so a Close that
// lands meanwhile wins: the fresh body is closed at once and never recorded,
// and no closed seeker leaves an open body or a storage call behind it.
func (k *seeker) openBody(pos int64) (io.ReadCloser, error) {
	body, err := k.s.GetRange(k.ctx, k.key, pos, k.size-1)
	k.mu.Lock()
	defer k.mu.Unlock()
	if err != nil {
		if k.closed {
			return nil, errSeekerClosed
		}
		k.bodyErr = err
		return nil, err
	}
	if k.closed {
		_ = body.Close()
		return nil, errSeekerClosed
	}
	k.body = body
	return body, nil
}

func (k *seeker) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.closed = true
	k.closeBodyLocked()
	return nil
}

func (k *seeker) closeBodyLocked() {
	if k.body != nil {
		_ = k.body.Close()
		k.body = nil
	}
}
