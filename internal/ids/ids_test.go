package ids

import (
	"math"
	"regexp"
	"strings"
	"testing"
)

func TestNewShape(t *testing.T) {
	for i := 0; i < 1000; i++ {
		id := New()
		if len(id) != SlugLen {
			t.Fatalf("New() = %q, len %d, want %d", id, len(id), SlugLen)
		}
		for _, c := range id {
			if !strings.ContainsRune(Alphabet, c) {
				t.Fatalf("New() = %q contains %q outside the alphabet", id, c)
			}
		}
		if !Valid(id) {
			t.Fatalf("Valid(New()=%q) = false", id)
		}
	}
}

func TestRandLengths(t *testing.T) {
	for _, n := range []int{-1, 0, 1, 2, 8, 12, 13, 100, 1000} {
		got := Rand(n)
		want := n
		if want < 0 {
			want = 0
		}
		if len(got) != want {
			t.Errorf("Rand(%d) len = %d, want %d", n, len(got), want)
		}
	}
}

func TestValid(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"aZ3kQ9xB2mLp", true},
		{"AAAAAAAAAAAA", true},
		{"000000000000", true},
		{"", false},
		{"aZ3kQ9xB2mL", false},   // 11
		{"aZ3kQ9xB2mLpQ", false}, // 13
		{"aZ3kQ9xB2mL-", false},  // dash
		{"aZ3kQ9xB2mL_", false},  // underscore
		{"aZ3kQ9xB2mL ", false},  // space
		{"aZ3kQ9xB2m/p", false},  // slash
		{"aZ3kQ9xB2mLé", false},  // non-ascii (2 bytes → 13 bytes)
		{"aZ3kQ9xB2m..", false},  // traversal-ish
		// The memorable grammar (§20 erratum 249): three or four capitalised
		// words of 3-9 characters each, joined without separators.
		{"PurpleUpdateCharacter", true},
		{"AbcAbcAbc", true},                          // the shortest three-word id
		{"AbcAbcAbcAbc", true},                       // four words — and also a legal compact id
		{"UnwiredUnwiredUnwiredUnwired", true},       // four nine-character words
		{"PurpleUpdated", false},                     // two words (twelve characters would be a legal COMPACT id)
		{"PurpleUpdateCharacterPurpleUpdate", false}, // five
		{"purpleUpdateCharacter", false},             // the first word is not capitalised
		{"PurpleUPdateCharacter", false},             // an upper inside a word opens a 2-char one
		{"Purple-Update-Character", false},           // separators
		{"AbAbcAbc", false},                          // a two-character word
		{"AbcdefghijAbcAbc", false},                  // a ten-character word
		{"Purple Update Character", false},           // spaces
	}
	for _, c := range cases {
		if got := Valid(c.in); got != c.want {
			t.Errorf("Valid(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestKind pins which grammar an id reports, including the deliberate overlap:
// a string that is BOTH twelve base62 characters and four capitalised triples
// reads as memorable, which is exactly the draw New refuses to mint.
func TestKind(t *testing.T) {
	cases := map[string]string{
		"aZ3kQ9xB2mLp":          KindCompact,
		"AAAAAAAAAAAA":          KindCompact,
		"PurpleUpdateCharacter": KindMemorable,
		"AbcAbcAbc":             KindMemorable,
		"AbcAbcAbcAbc":          KindMemorable, // twelve base62 characters too
		"":                      "",
		"not-an-id":             "",
		"PurpleUpdated":         "",
	}
	for in, want := range cases {
		k := Kind(in)
		if k != want {
			t.Errorf("Kind(%q) = %q, want %q", in, k, want)
		}
		if (k != "") != Valid(in) {
			t.Errorf("Kind(%q) = %q but Valid(%q) = %v", in, k, in, Valid(in))
		}
	}
}

// TestDistribution draws 100k slugs and checks that every symbol appears and
// that the frequency spread passes a loose chi-square sanity bound.
func TestDistribution(t *testing.T) {
	const draws = 100_000
	counts := make(map[rune]int, len(Alphabet))
	for i := 0; i < draws; i++ {
		for _, c := range New() {
			counts[c]++
		}
	}
	if len(counts) != len(Alphabet) {
		t.Fatalf("saw %d distinct symbols, want %d", len(counts), len(Alphabet))
	}
	for _, c := range Alphabet {
		if counts[c] == 0 {
			t.Errorf("symbol %q never drawn", c)
		}
	}
	total := float64(draws * SlugLen)
	expected := total / float64(len(Alphabet))
	var chi2 float64
	for _, c := range Alphabet {
		d := float64(counts[c]) - expected
		chi2 += d * d / expected
	}
	// 61 degrees of freedom: mean 61, sd ≈ 11. A bound of 150 is > 8 sd
	// above the mean, so a correct sampler essentially never trips it while
	// a modulo-biased one (which skews the first 8 symbols) always does.
	if chi2 > 150 || math.IsNaN(chi2) {
		t.Fatalf("chi-square %.1f exceeds sanity bound 150 (biased sampler?)", chi2)
	}
}

func TestToken(t *testing.T) {
	re := regexp.MustCompile(`^ck_[A-Za-z0-9_-]{43}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok := Token()
		if !re.MatchString(tok) {
			t.Fatalf("Token() = %q does not match %s", tok, re)
		}
		if len(tok) != 46 {
			t.Fatalf("Token() len = %d, want 46", len(tok))
		}
		if seen[tok] {
			t.Fatalf("Token() repeated %q", tok)
		}
		seen[tok] = true
	}
}

// The inbound URL's secret (§20 erratum 322): its own prefix, the capture
// token's length, and a shape check that admits exactly what InboundToken
// mints.
func TestInboundToken(t *testing.T) {
	re := regexp.MustCompile(`^ci_[A-Za-z0-9_-]{43}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok := InboundToken()
		if !re.MatchString(tok) || len(tok) != InboundTokenLen || InboundTokenLen != 46 {
			t.Fatalf("InboundToken() = %q (len %d)", tok, len(tok))
		}
		if !IsInboundToken(tok) {
			t.Fatalf("IsInboundToken(%q) = false on a minted value", tok)
		}
		if seen[tok] {
			t.Fatalf("InboundToken() repeated %q", tok)
		}
		seen[tok] = true
	}
	body := strings.Repeat("A", 43)
	for _, bad := range []string{
		"", "ci_", "ci_" + body[:42], "ci_" + body + "A", "ck_" + body, "CI_" + body,
		"ci_" + body[:42] + "=", "ci_" + body[:42] + ".", "ci_" + body[:42] + "/", "ci_" + body[:42] + "%",
		Token(),
	} {
		if IsInboundToken(bad) {
			t.Errorf("IsInboundToken(%q) = true", bad)
		}
	}
	if !IsInboundToken("ci_" + strings.Repeat("-", 21) + strings.Repeat("_", 22)) {
		t.Error("IsInboundToken refused the base64url symbols - and _")
	}
}

// The Clippet Auto URL's secret (§20 erratum 324): `ca_` and 43 base64url
// characters, 46 in all; no value of either other kind passes its shape, and
// it passes neither of theirs.
func TestAutoFetchToken(t *testing.T) {
	re := regexp.MustCompile(`^ca_[A-Za-z0-9_-]{43}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok := AutoFetchToken()
		if !re.MatchString(tok) || len(tok) != AutoFetchTokenLen || AutoFetchTokenLen != 46 {
			t.Fatalf("AutoFetchToken() = %q (len %d)", tok, len(tok))
		}
		if !IsAutoFetchToken(tok) || IsInboundToken(tok) {
			t.Fatalf("the shapes disagree on a minted value %q", tok)
		}
		if seen[tok] {
			t.Fatalf("AutoFetchToken() repeated %q", tok)
		}
		seen[tok] = true
	}
	body := strings.Repeat("A", 43)
	for _, bad := range []string{
		"", "ca_", "ca_" + body[:42], "ca_" + body + "A", "ci_" + body, "ck_" + body, "CA_" + body,
		"ca_" + body[:42] + "=", "ca_" + body[:42] + "/", "ca_" + body[:42] + "%",
		Token(), InboundToken(),
	} {
		if IsAutoFetchToken(bad) {
			t.Errorf("IsAutoFetchToken(%q) = true", bad)
		}
	}
}
