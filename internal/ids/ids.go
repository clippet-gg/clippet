// Package ids generates the unguessable public identifiers Clippet uses for
// clips, users and API tokens (DESIGN.md §7, §20 errata 248-249).
//
// There are TWO id grammars, and both are minted here.
//
// **Compact** — the default, and the only grammar until §20 erratum 249 — is
// 12 characters drawn from the 62-symbol alphabet A–Z a–z 0–9 (≈71.4 bits of
// entropy). They are produced with crypto/rand and rejection sampling so every
// symbol is equally likely: a random byte is accepted only when it is below
// 248 (= 4·62), then reduced modulo 62.
//
// **Memorable** is three or four words from the EFF long word list, each
// capitalised and joined without separators (PurpleUpdateCharacter). It is
// drawn from the same crypto/rand with the same rejection sampling, over the
// CURATED list in eff_large_wordlist.txt: every entry of the published 7,776
// that is not `^[a-z]{3,9}$` is dropped — four hyphenated entries — leaving
// **7,772 words**. That is log2(7772³) ≈ **38.8 bits** for three words and
// log2(7772⁴) ≈ **51.7 bits** for four. The owner's rule (2026-09-09) decides
// which: three words are public-clip grade, four are unlisted grade, and
// compact needs no rate limit at all.
//
// Word list: EFF large wordlist, CC BY 3.0.
//
// **Neither grammar can collide with a route.** Reserved (reserved.go) is the
// first path segment of every route the app registers, and no minter here
// returns an id that equals one of them, case-insensitively. A compact id
// could never have — none of those words is twelve base62 characters — and a
// memorable id could not either, being three words where a route is one; the
// check is a handful of byte comparisons per mint, and it is what makes "an id
// is never a route" a property rather than an observation.
//
// **The two grammars are told apart by shape** (Kind), so nothing about the
// choice is stored on the clip. They overlap in principle — `AbcAbcAbcAbc` is
// twelve base62 characters AND four capitalised triples — so New refuses any
// draw that reads as memorable. Exactly eleven twelve-character shapes qualify
// (3+3+3+3, and the ten ways three words of 3–9 characters sum to 12), each
// drawn with probability (26/62)^12, so about one compact draw in 3,000 is put
// back and the grammar loses 0.0005 of a bit. Kind is therefore exact for every
// id THIS build mints; an id minted before erratum 249 is compact and carries
// that same one-in-3,000 chance of reading as memorable, which costs one
// true-but-irrelevant line on its owner's own page and nothing else.
package ids

import (
	"crypto/rand"
	"encoding/base64"
)

// Alphabet is the compact alphabet, in the order symbols are indexed by the
// rejection sampler. It is exported so tests can assert coverage.
const Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// SlugLen is the length of every COMPACT id produced by New.
const SlugLen = 12

// rejectAbove is the largest multiple of len(Alphabet) that fits in a byte.
// Bytes ≥ this value are discarded so that b % 62 is unbiased.
const rejectAbove = 248

// The two grammar names, as they travel on the wire (`link_style`) and as
// Kind reports them.
const (
	KindCompact   = "compact"
	KindMemorable = "memorable"
)

// New returns a fresh 12-character COMPACT id.
//
// The loop is the two invariants of the package doc, not a retry: a draw that
// equals a route, or that reads as a memorable id, is put back. Both are
// vanishingly rare — the first cannot happen with any route this app has — so
// this is one draw in practice.
func New() string {
	for {
		if id := randSlug(); !IsReserved(id) && !validMemorable(id) {
			return id
		}
	}
}

// randSlug draws ONE compact candidate. It is a package variable so the two
// rejections above it can be proven with a rigged generator: a draw that comes
// back reserved, or shaped like a memorable id, must be put back, and nothing
// short of rigging the draw can reach either arm (a real one has a
// one-in-3,000 chance of the second and none at all of the first).
var randSlug = func() string { return Rand(SlugLen) }

// Rand returns n unbiased random characters from Alphabet.
func Rand(n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/2+8) // headroom for rejected bytes
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			// crypto/rand.Read is documented never to fail on supported
			// platforms; there is no sane way to continue without entropy.
			panic("ids: crypto/rand failed: " + err.Error())
		}
		for _, b := range buf {
			if b >= rejectAbove {
				continue
			}
			out = append(out, Alphabet[int(b)%len(Alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out)
}

// Valid reports whether s is a well-formed id in EITHER grammar: exactly 12
// characters of [A-Za-z0-9], or three to four capitalised lowercase words
// joined without separators. Every {id} route checks it before touching the
// database, and both scratch janitors use it to tell a clip's working
// directory from anything else on the disk — which is why the leading dot of
// `.upload-{id}` still keeps a spool outside both grammars.
func Valid(s string) bool { return validCompact(s) || validMemorable(s) }

// Kind reports which grammar s is in: KindCompact, KindMemorable, or "" for a
// string that is neither. Memorable is tested FIRST because it is the narrower
// shape (see the package doc on the overlap New refuses to mint into).
func Kind(s string) string {
	switch {
	case validMemorable(s):
		return KindMemorable
	case validCompact(s):
		return KindCompact
	}
	return ""
}

// validCompact is the original `^[A-Za-z0-9]{12}$`.
func validCompact(s string) bool {
	if len(s) != SlugLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isAlnum(s[i]) {
			return false
		}
	}
	return true
}

// validMemorable is `^(?:[A-Z][a-z]{2,8}){3,4}$`, scanned rather than matched:
// an uppercase letter opens a word and the lowercase run that follows closes
// it, so the split into words is unique and one pass decides it with no
// allocation and no regexp.
func validMemorable(s string) bool {
	if len(s) < MemorableMinWords*minWordLen || len(s) > MemorableMaxWords*maxWordLen {
		return false
	}
	words := 0
	for i := 0; i < len(s); {
		if !isUpper(s[i]) {
			return false
		}
		j := i + 1
		for j < len(s) && isLower(s[j]) {
			j++
		}
		if n := j - i; n < minWordLen || n > maxWordLen {
			return false
		}
		words++
		if words > MemorableMaxWords {
			return false
		}
		i = j
	}
	return words >= MemorableMinWords
}

func isAlnum(c byte) bool {
	return isUpper(c) || isLower(c) || (c >= '0' && c <= '9')
}

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLower(c byte) bool { return c >= 'a' && c <= 'z' }

// TokenPrefix is the fixed prefix of every raw API token.
const TokenPrefix = "ck_"

// Token returns a raw API token: "ck_" followed by 43 base64url characters
// encoding 32 random bytes (46 characters total). Only its sha256 is stored.
func Token() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("ids: crypto/rand failed: " + err.Error())
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
}

// InboundPrefix is the fixed prefix of every inbound URL's secret (§20
// erratum 322). It differs from TokenPrefix on purpose: the Resolver accepts
// only `Bearer ck_…`, and the stored hash covers the prefix, so a value of
// one kind can never be read as the other.
const InboundPrefix = "ci_"

// InboundTokenLen is the length of a raw inbound-URL secret: the prefix and
// 43 base64url characters (46).
const InboundTokenLen = len(InboundPrefix) + 43

// InboundToken returns a raw inbound-URL secret: "ci_" followed by 43
// base64url characters encoding 32 random bytes. Only its sha256 is stored.
func InboundToken() string { return pathSecret(InboundPrefix) }

// IsInboundToken reports whether s has the SHAPE of an inbound-URL secret:
// "ci_" and exactly 43 characters of [A-Za-z0-9_-]. It says nothing about
// whether the value is live; a value of any other shape is answered without a
// database read.
func IsInboundToken(s string) bool { return isPathSecret(s, InboundPrefix) }

// AutoFetchPrefix is the fixed prefix of every Clippet Auto URL's secret (§20
// erratum 324): the credential in the path of POST /in/auto/{token}. A third
// prefix for a third kind of row, for InboundPrefix's reason — the stored hash
// covers the prefix, so no value of one kind is ever read as another.
const AutoFetchPrefix = "ca_"

// AutoFetchTokenLen is the length of a raw Clippet Auto URL secret (46).
const AutoFetchTokenLen = len(AutoFetchPrefix) + 43

// AutoFetchToken returns a raw Clippet Auto URL secret: "ca_" followed by 43
// base64url characters encoding 32 random bytes. Only its sha256 is stored.
func AutoFetchToken() string { return pathSecret(AutoFetchPrefix) }

// IsAutoFetchToken reports whether s has the SHAPE of a Clippet Auto URL's
// secret: "ca_" and exactly 43 characters of [A-Za-z0-9_-].
func IsAutoFetchToken(s string) bool { return isPathSecret(s, AutoFetchPrefix) }

// SigninPrefix is the fixed prefix of every one-time login link's secret (§20
// erratum 341): the token in GET|POST /login/discord/{token}, minted by the
// bot's `/signin`. A fourth prefix for a fourth kind of row, for InboundPrefix's
// reason — and so httpx.LogPath recognises it in any path it appears in.
const SigninPrefix = "cs_"

// SigninTokenLen is the length of a raw login-link secret (46).
const SigninTokenLen = len(SigninPrefix) + 43

// SigninToken returns a raw login-link secret: "cs_" followed by 43 base64url
// characters encoding 32 random bytes (256 bits). Only its sha256 is stored.
func SigninToken() string { return pathSecret(SigninPrefix) }

// IsSigninToken reports whether s has the SHAPE of a login-link secret.
func IsSigninToken(s string) bool { return isPathSecret(s, SigninPrefix) }

// pathSecret is a secret that travels in a URL path: prefix and 43 base64url
// characters encoding 32 random bytes.
func pathSecret(prefix string) string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("ids: crypto/rand failed: " + err.Error())
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw[:])
}

// isPathSecret is the shape of a pathSecret with this prefix.
func isPathSecret(s, prefix string) bool {
	if len(s) != len(prefix)+43 || s[:len(prefix)] != prefix {
		return false
	}
	for i := len(prefix); i < len(s); i++ {
		if c := s[i]; !isAlnum(c) && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
