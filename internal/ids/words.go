package ids

import (
	"crypto/rand"
	_ "embed"
	"strings"
)

// wordFile is the CURATED EFF long word list, one word per line, shipped
// inside the binary (§20 erratum 249). The curation rule is `^[a-z]{3,9}$`
// applied to the published 7,776 entries — it drops the four hyphenated ones
// and nothing else — and it is re-asserted by TestWordList on every run, so a
// hand-edited file fails the build's tests rather than quietly changing what a
// link looks like.
//
// Word list: the EFF large wordlist is CC BY 3.0; THIRD_PARTY.md at the root
// carries the attribution, and the credit also lives here, in the package doc,
// and beside the control on /settings.
//
//go:embed eff_large_wordlist.txt
var wordFile string

// The word-count policy of a memorable id (§20 errata 249, 250). Three words
// for a listed clip, four when the clip is uploaded unlisted (the owner's
// rule, 2026-09-09: three words are public-clip grade, four unlisted grade).
// They are also the bounds validMemorable accepts, so an id minted here and an
// id a route will answer for are the same set by construction.
const (
	MemorableMinWords = 3
	MemorableMaxWords = 4
)

// minWordLen and maxWordLen bound ONE capitalised word: the curation rule
// `^[a-z]{3,9}$` gives 3–9 characters, and capitalising the first changes no
// length. They are what validMemorable measures a word against.
const (
	minWordLen = 3
	maxWordLen = 9
)

// words is the parsed list. It is built once at init from wordFile.
var words = parseWords(wordFile)

// wordReject is the largest multiple of len(words) that fits in 16 bits.
// A two-byte draw at or above it is discarded so that v % len(words) is
// unbiased — the base62 sampler's rejection rule, one width up (7,772 needs
// 13 bits, so a byte cannot carry an index).
var wordReject = (1 << 16) / len(words) * len(words)

func parseWords(raw string) []string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if w := strings.TrimSpace(l); w != "" {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		// The list is embedded from this package's own directory, so an empty
		// one is a build that shipped without it: fail loudly at init rather
		// than mint ids from an empty alphabet at request time.
		panic("ids: the embedded word list is empty")
	}
	return out
}

// Words is how many words the shipped list holds, for the tests and the docs
// that quote the entropy (log2(Words^n) bits for an n-word id).
func Words() int { return len(words) }

// NewMemorable returns a fresh memorable id of n words: each word capitalised,
// joined without separators (PurpleUpdateCharacter). n outside
// [MemorableMinWords, MemorableMaxWords] is clamped into it — a caller cannot
// mint an id no route would accept.
//
// The reserved-word check is New's, for New's reason: it cannot fire on a
// three-word id, and it exists so that "no minter returns a route" holds for
// every minter rather than for the one where it was plausible.
func NewMemorable(n int) string {
	if n < MemorableMinWords {
		n = MemorableMinWords
	}
	if n > MemorableMaxWords {
		n = MemorableMaxWords
	}
	for {
		if id := drawMemorable(n); !IsReserved(id) {
			return id
		}
	}
}

// drawMemorable is randSlug's twin: a package variable so the reserved-word
// rejection above it can be proven with a rigged generator, since a real draw
// can never reach that arm.
var drawMemorable = drawWords

// drawWords is Rand's loop over a 16-bit draw: fill a buffer from crypto/rand,
// walk it two bytes at a time, discard any pair at or above wordReject, and
// take the rest modulo the list length. The headroom over 2n bytes is the same
// idea as Rand's — about 5% of pairs are rejected — and the outer loop refills
// on the (rare) exhaustion.
func drawWords(n int) string {
	var b strings.Builder
	b.Grow(n * maxWordLen)
	buf := make([]byte, 2*(n+n/2+4))
	got := 0
	for got < n {
		if _, err := rand.Read(buf); err != nil {
			panic("ids: crypto/rand failed: " + err.Error())
		}
		for i := 0; i+1 < len(buf); i += 2 {
			v := int(buf[i])<<8 | int(buf[i+1])
			if v >= wordReject {
				continue
			}
			w := words[v%len(words)]
			b.WriteByte(w[0] - ('a' - 'A'))
			b.WriteString(w[1:])
			got++
			if got == n {
				break
			}
		}
	}
	return b.String()
}
