package store

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"strconv"
	"testing"
)

// referenceETag is the documented rule written out the long way, with no
// code shared with etag.go: one PUT (size ≤ part) → hex MD5; otherwise hex
// MD5 of the concatenated binary part MD5s, `-N`.
func referenceETag(data []byte, part int) string {
	if len(data) <= part {
		s := md5.Sum(data)
		return hex.EncodeToString(s[:])
	}
	var cat []byte
	n := 0
	for off := 0; off < len(data); off += part {
		end := min(off+part, len(data))
		s := md5.Sum(data[off:end])
		cat = append(cat, s[:]...)
		n++
	}
	s := md5.Sum(cat)
	return hex.EncodeToString(s[:]) + "-" + strconv.Itoa(n)
}

// TestExpectedETagAgainstTheFormula: single, exactly N parts, N parts plus a
// remainder — and the part boundary itself, where a one-part object is a
// single PUT (§20 erratum 337).
func TestExpectedETagAgainstTheFormula(t *testing.T) {
	const part = 1024
	data := make([]byte, 5*part+333)
	for i := range data {
		data[i] = byte(i*7 + i/part)
	}
	for _, c := range []struct {
		name string
		size int
		want string // the shape's suffix
	}{
		{"empty", 0, ""},
		{"single", 700, ""},
		{"exactly one part is a single PUT", part, ""},
		{"one byte past one part", part + 1, "-2"},
		{"exactly three parts", 3 * part, "-3"},
		{"five parts plus a remainder", 5*part + 333, "-6"},
	} {
		b := data[:c.size]
		got, err := ExpectedETag(bytes.NewReader(b), int64(c.size), part)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if want := referenceETag(b, part); got != want {
			t.Errorf("%s: ExpectedETag = %s, the formula says %s", c.name, got, want)
		}
		if c.want != "" && got[len(got)-len(c.want):] != c.want {
			t.Errorf("%s: %s does not end in %s", c.name, got, c.want)
		}
		// Written in uneven pieces, the hasher agrees with itself.
		h := NewETagHasher(part)
		for off := 0; off < len(b); off += 517 {
			_, _ = h.Write(b[off:min(off+517, len(b))])
		}
		if h.Sum() != got {
			t.Errorf("%s: chunked writes gave %s, one read %s", c.name, h.Sum(), got)
		}
	}
	// A known literal: the ETag of "hello" from one PUT is its MD5.
	if got, _ := ExpectedETag(bytes.NewReader([]byte("hello")), 5, 0); got != "5d41402abc4b2a76b9719d911017c592" {
		t.Errorf("hello: %s", got)
	}
	if _, err := ExpectedETag(bytes.NewReader(data[:10]), 11, part); err == nil {
		t.Error("a short stream was given an ETag")
	}
}

// TestETagMatches: quotes and case are spelling; empty is never evidence.
func TestETagMatches(t *testing.T) {
	for _, c := range []struct {
		e, r string
		want bool
	}{
		{"abc-2", `"ABC-2"`, true},
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"", "", false},
		{"abc", "", false},
		{"", "abc", false},
	} {
		if got := ETagMatches(c.e, c.r); got != c.want {
			t.Errorf("ETagMatches(%q, %q) = %v", c.e, c.r, got)
		}
	}
	if (PutResult{ExpectedETag: "x", StoredETag: `"x"`}).Verified() != true || (PutResult{}).Verified() {
		t.Error("PutResult.Verified")
	}
}

// TestFakeETagShapes: the Fake answers the S3 shape by default — so a caller's
// proof path is testable — and a sha256 under OpaqueETags, a provider whose
// ETag is shaped some other way.
func TestFakeETagShapes(t *testing.T) {
	ctx := context.Background()
	data := bytes.Repeat([]byte("e"), 3000)
	f := NewFake()
	f.Put("k", data, "video/mp4")
	info, err := f.Stat(ctx, "k")
	if err != nil || info.ETag != referenceETag(data, UploadPartSize) {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	res, err := f.PutFile(ctx, "p", writeTemp(t, data), "video/mp4", nil)
	if err != nil || !res.Verified() || res.ExpectedETag != info.ETag {
		t.Fatalf("PutFile = %+v, %v", res, err)
	}
	o := NewFake()
	o.OpaqueETags = true
	res, err = o.PutFile(ctx, "p", writeTemp(t, data), "video/mp4", nil)
	if err != nil || res.Verified() || res.ExpectedETag != info.ETag {
		t.Fatalf("opaque PutFile = %+v, %v", res, err)
	}
	if oi, _ := o.Stat(ctx, "p"); oi.ETag != res.SHA256 || ETagMatches(res.ExpectedETag, oi.ETag) {
		t.Fatalf("opaque Stat = %+v", oi)
	}
}
