package ids

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// wordCount is the size of the CURATED list this build ships. It is written
// out rather than derived so that a re-fetched or hand-edited word file fails
// here, beside the entropy the package doc and DESIGN §7 quote for it
// (log2(7772^3) ≈ 38.8 bits, log2(7772^4) ≈ 51.7).
const wordCount = 7772

func TestWordList(t *testing.T) {
	if Words() != wordCount {
		t.Fatalf("Words() = %d, want %d (the curated EFF long list)", Words(), wordCount)
	}
	shape := regexp.MustCompile(`^[a-z]{3,9}$`)
	seen := make(map[string]bool, len(words))
	for _, w := range words {
		if !shape.MatchString(w) {
			t.Fatalf("word %q is outside the curation rule ^[a-z]{3,9}$", w)
		}
		if seen[w] {
			t.Fatalf("word %q appears twice; the entropy claim assumes a set", w)
		}
		seen[w] = true
	}
	if !sort.StringsAreSorted(words) {
		t.Error("the word list is not in ascending order (the published list is; a diff should read as one)")
	}
}

// TestWordSampler pins the rejection constant rather than measuring the
// distribution: 65536 is not a multiple of 7772, so `v % len(words)` alone
// would favour the first 3,360 words by 9 draws to 8, and the ONE thing that
// prevents it is that wordReject is the largest multiple that fits.
func TestWordSampler(t *testing.T) {
	if wordReject%len(words) != 0 {
		t.Fatalf("wordReject = %d is not a multiple of %d", wordReject, len(words))
	}
	if wordReject > 1<<16 {
		t.Fatalf("wordReject = %d does not fit in a two-byte draw", wordReject)
	}
	if (1<<16)-wordReject >= len(words) {
		t.Fatalf("wordReject = %d is not the LARGEST multiple below %d", wordReject, 1<<16)
	}
	if wordReject != 8*wordCount {
		t.Fatalf("wordReject = %d, want %d (8 x %d)", wordReject, 8*wordCount, wordCount)
	}
}

// splitMemorable cuts an id back into its words, lowercased. It mirrors
// validMemorable's scan and is only used by the tests below.
func splitMemorable(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	for i := 0; i < len(id); {
		j := i + 1
		for j < len(id) && isLower(id[j]) {
			j++
		}
		out = append(out, strings.ToLower(id[i:j]))
		i = j
	}
	return out
}

func TestNewMemorableShape(t *testing.T) {
	inList := make(map[string]bool, len(words))
	for _, w := range words {
		inList[w] = true
	}
	for _, n := range []int{MemorableMinWords, MemorableMaxWords} {
		for i := 0; i < 500; i++ {
			id := NewMemorable(n)
			if !Valid(id) {
				t.Fatalf("Valid(NewMemorable(%d) = %q) = false", n, id)
			}
			if k := Kind(id); k != KindMemorable {
				t.Fatalf("Kind(%q) = %q, want %q", id, k, KindMemorable)
			}
			got := splitMemorable(t, id)
			if len(got) != n {
				t.Fatalf("NewMemorable(%d) = %q has %d words", n, id, len(got))
			}
			for _, w := range got {
				if !inList[w] {
					t.Fatalf("NewMemorable(%d) = %q used %q, which is not in the list", n, id, w)
				}
			}
		}
	}
}

// TestNewMemorableClamps: a caller cannot mint an id outside the grammar every
// {id} route accepts.
func TestNewMemorableClamps(t *testing.T) {
	for _, n := range []int{-3, 0, 1, 2, 5, 40} {
		id := NewMemorable(n)
		if !Valid(id) {
			t.Fatalf("NewMemorable(%d) = %q is not a valid id", n, id)
		}
		w := len(splitMemorable(t, id))
		if w < MemorableMinWords || w > MemorableMaxWords {
			t.Fatalf("NewMemorable(%d) drew %d words", n, w)
		}
	}
}

// TestMemorableCoverage draws 100k words and asserts that all but a couple of
// the list is reachable — the modulo maps across the whole list, not across a
// prefix of it. With 100k draws the chance a given word is never seen is
// e^-12.87 ≈ 2.6e-6, so "at most two missing" is a bound a correct sampler
// essentially never trips while a truncated one fails by thousands.
func TestMemorableCoverage(t *testing.T) {
	seen := make(map[string]bool, len(words))
	for i := 0; i < 100_000/MemorableMinWords; i++ {
		for _, w := range splitMemorable(t, NewMemorable(MemorableMinWords)) {
			seen[w] = true
		}
	}
	if len(seen) < len(words)-2 {
		t.Fatalf("saw %d of %d words in 100k draws", len(seen), len(words))
	}
}

// TestMintersRefuseAReservedWord rigs both generators, because neither
// rejection can be reached by a real draw: a compact id is twelve base62
// characters and a memorable one is three words, and no route is either.
func TestMintersRefuseAReservedWord(t *testing.T) {
	realSlug, realWords := randSlug, drawMemorable
	t.Cleanup(func() { randSlug, drawMemorable = realSlug, realWords })

	// Every reserved word once, then a real draw. New must hand back the real
	// one — and must not spin forever on the way.
	queue := append([]string{}, Reserved...)
	queue = append(queue, "SETTINGS", "Login", "AbcAbcAbcAbc") // case-insensitive, and the memorable-shaped compact draw
	i := 0
	randSlug = func() string {
		if i < len(queue) {
			id := queue[i]
			i++
			return id
		}
		return realSlug()
	}
	id := New()
	if i != len(queue) {
		t.Fatalf("New() stopped after %d rigged draws, want all %d put back", i, len(queue))
	}
	if IsReserved(id) || Kind(id) != KindCompact {
		t.Fatalf("New() = %q after the rigged draws", id)
	}

	j := 0
	drawMemorable = func(n int) string {
		if j < len(Reserved) {
			w := Reserved[j]
			j++
			return w
		}
		return realWords(n)
	}
	mem := NewMemorable(MemorableMinWords)
	if j != len(Reserved) {
		t.Fatalf("NewMemorable() stopped after %d rigged draws, want all %d put back", j, len(Reserved))
	}
	if IsReserved(mem) || Kind(mem) != KindMemorable {
		t.Fatalf("NewMemorable() = %q after the rigged draws", mem)
	}
}

func TestIsReserved(t *testing.T) {
	for _, r := range Reserved {
		if !IsReserved(r) {
			t.Errorf("IsReserved(%q) = false", r)
		}
		if !IsReserved(strings.ToUpper(r)) {
			t.Errorf("IsReserved(%q) = false (the check is case-insensitive)", strings.ToUpper(r))
		}
	}
	for _, s := range []string{"", "clip", "clipss", "settingsx", "aZ3kQ9xB2mLp", "PurpleUpdateCharacter"} {
		if IsReserved(s) {
			t.Errorf("IsReserved(%q) = true", s)
		}
	}
	if !sort.StringsAreSorted(Reserved) {
		t.Error("Reserved is not sorted; keep it so a new route lands in an obvious place")
	}
}

// TestNewIsNeverMemorable is the property Kind depends on: no compact id this
// build mints reads as a memorable one. It is a sampling test — the overlap is
// about one draw in 3,000 — so it draws enough to have met a dozen or so.
func TestNewIsNeverMemorable(t *testing.T) {
	for i := 0; i < 50_000; i++ {
		if id := New(); Kind(id) != KindCompact {
			t.Fatalf("New() = %q reports %q", id, Kind(id))
		}
	}
}
