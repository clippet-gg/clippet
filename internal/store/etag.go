package store

// etag.go — the S3-shaped ETag a bucket SHOULD report for bytes we hashed
// ourselves (§20 erratum 337).
//
// The rule, as Cloudflare's R2 documentation states it for its S3 API and as
// AWS documents it for plaintext and SSE-S3 objects: an object written by ONE
// PUT has the hex MD5 of its bytes as its ETag; an object written by a
// multipart upload has the hex MD5 of the CONCATENATED BINARY MD5s of its
// parts, followed by `-N` for N parts, where every part but the last is the
// same size. Backblaze B2's and MinIO's rules are not established by any
// document this project has read, so nothing here ASSUMES a provider follows
// it: a caller compares what this computes with what the provider reports, and
// a provider whose ETag is shaped any other way simply never matches. That is
// the safe side — `admin storage migrate` then reads the object back in full,
// exactly as it did before this erratum.
//
// MD5 is fine for this job and for nothing more: it detects a part that
// arrived different from what was sent, which is all a copy between two of
// the operator's own stores is defending against. The sha256 PutFile has
// always computed is still the object's identity in the database.

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
)

// UploadPartSize is the part size PutFile uploads with, and therefore the part
// size an ETag has to be predicted at: an object of up to this many bytes goes
// up in ONE PUT, anything larger in parts of exactly this size (the last one
// shorter). It is the same 16 MiB the store has always used (§18.4).
const UploadPartSize = partSize

// ETagHasher computes the expected S3-shaped ETag of the bytes written to it,
// for an upload that splits at partSize. It is an io.Writer so it can sit in
// the SAME pass that already computes an object's sha256 (an io.MultiWriter
// beside it): predicting the ETag never costs a second read.
type ETagHasher struct {
	partSize int64
	cur      hash.Hash
	curN     int64
	sums     []byte // the binary MD5s of the parts finished so far
	parts    int
	total    int64
}

// NewETagHasher returns a hasher for uploads split at partSize (≤ 0 means
// UploadPartSize).
func NewETagHasher(partSize int64) *ETagHasher {
	if partSize <= 0 {
		partSize = UploadPartSize
	}
	return &ETagHasher{partSize: partSize, cur: md5.New()}
}

// Write never fails.
func (e *ETagHasher) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		// A part is closed when the NEXT byte arrives, never when it fills:
		// an object of exactly one part size is a single PUT (minio-go's own
		// threshold is "size <= part size"), and its ETag is the plain MD5.
		if e.curN == e.partSize {
			e.sums = e.cur.Sum(e.sums)
			e.cur.Reset()
			e.curN = 0
			e.parts++
		}
		k := min(int64(len(p)), e.partSize-e.curN)
		e.cur.Write(p[:k])
		e.curN += k
		e.total += k
		p = p[k:]
	}
	return n, nil
}

// Sum is the expected ETag: lower-case hex, no quotes, `-N` on a multipart
// object.
func (e *ETagHasher) Sum() string {
	if e.total <= e.partSize {
		return hex.EncodeToString(e.cur.Sum(nil))
	}
	all := e.cur.Sum(append([]byte(nil), e.sums...))
	whole := md5.Sum(all)
	return hex.EncodeToString(whole[:]) + "-" + strconv.Itoa(e.parts+1)
}

// ExpectedETag reads r to the end and returns the ETag an upload of those
// bytes split at partSize should be given (§20 erratum 337). size is the
// length the caller expects; a stream of any other length is an error, so a
// short read can never be mistaken for a smaller object.
func ExpectedETag(r io.Reader, size, partSize int64) (string, error) {
	h := NewETagHasher(partSize)
	n, err := io.Copy(h, r)
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("store: expected etag: read %d bytes of %d", n, size)
	}
	return h.Sum(), nil
}

// NormalizeETag strips the quotes an HTTP ETag travels in and lower-cases it,
// so two spellings of the same value compare equal.
func NormalizeETag(s string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(s), `"`))
}

// ETagMatches reports whether a provider's ETag is the one predicted. An empty
// prediction or an empty report never matches: "no ETag" (the `fs` store,
// erratum 276) is not evidence of anything.
func ETagMatches(expected, reported string) bool {
	e, r := NormalizeETag(expected), NormalizeETag(reported)
	return e != "" && r != "" && e == r
}
