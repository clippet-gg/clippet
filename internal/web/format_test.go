package web

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The page formatters, tested beside format.go rather than scattered through
// web_test.go: they are pure, so a table is the whole test.
//
// TestFormatDuration reads the golden table (§20 erratum 268) rather than a list
// of its own: internal/notify/reply.go carries a byte-identical copy for the
// Discord bot's follow-up, the two packages do not import each other, and
// humanBytes already showed what unpinned copies do (nit sweep S11).
func TestFormatDuration(t *testing.T) {
	for _, tc := range durationGolden(t) {
		if got := formatDuration(tc.MS); got != tc.Text {
			t.Errorf("formatDuration(%d) = %q, want %q (testdata/duration.tsv)", tc.MS, got, tc.Text)
		}
	}
}

// durationGolden reads ../../testdata/duration.tsv into (ms, text) pairs. Its
// twin lives in internal/notify/reply_test.go and reads the same file.
func durationGolden(t *testing.T) []struct {
	MS   int64
	Text string
} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "duration.tsv"))
	if err != nil {
		t.Fatalf("read the formatDuration golden: %v", err)
	}
	var out []struct {
		MS   int64
		Text string
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 2 {
			t.Fatalf("duration.tsv:%d: %d tab-separated fields, want 2 (%q)", i+1, len(f), line)
		}
		ms, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			t.Fatalf("duration.tsv:%d: %v", i+1, err)
		}
		out = append(out, struct {
			MS   int64
			Text string
		}{ms, f[1]})
	}
	if len(out) < 8 {
		t.Fatalf("the formatDuration golden holds %d rows; it is meant to be the whole table", len(out))
	}
	return out
}

func TestFormatDateAndExpiryText(t *testing.T) {
	if got := formatDate(1_700_000_000); got != "2023-11-14 22:13 UTC" {
		t.Errorf("formatDate = %q", got)
	}
	now := time.Unix(1_700_000_000, 0)
	future, past := int64(1_700_604_800), int64(1_699_999_999)
	for _, tc := range []struct {
		exp  *int64
		want string
	}{
		{nil, "kept forever"},
		{&future, "expires 2023-11-21 22:13 UTC"},
		{&past, "expired 2023-11-14 22:13 UTC (pending deletion)"},
	} {
		if got := expiryText(tc.exp, now); got != tc.want {
			t.Errorf("expiryText = %q, want %q", got, tc.want)
		}
	}
}

// The size beside "Download original" speaks the same binary units the upload
// limit's error and `admin stats` do.
// TestHumanBytes reads the golden table (nit sweep S11) rather than a list of
// its own: this copy of humanBytes and internal/httpapi's are byte-identical on
// purpose, and cmd/clippet's differs in exactly two documented ways, so the
// expectations belong in ONE place all three tests can disagree with.
func TestHumanBytes(t *testing.T) {
	for _, tc := range humanBytesGolden(t) {
		if got := humanBytes(tc.N); got != tc.Page {
			t.Errorf("humanBytes(%d) = %q, want %q (testdata/humanbytes.tsv)", tc.N, got, tc.Page)
		}
	}
}

// humanBytesGolden reads the ONE table all three humanBytes copies are pinned
// against (../../testdata/humanbytes.tsv, nit sweep S11) and returns
// (bytes, page spelling, cli spelling) triples. The file's own header explains
// the two deliberate page/CLI differences; a THIRD difference is a bug in
// whichever copy moved.
func humanBytesGolden(t *testing.T) []struct {
	N         int64
	Page, CLI string
} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "humanbytes.tsv"))
	if err != nil {
		t.Fatalf("read the humanBytes golden: %v", err)
	}
	var out []struct {
		N         int64
		Page, CLI string
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			t.Fatalf("humanbytes.tsv:%d: %d tab-separated fields, want 3 (%q)", i+1, len(f), line)
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			t.Fatalf("humanbytes.tsv:%d: %v", i+1, err)
		}
		out = append(out, struct {
			N         int64
			Page, CLI string
		}{n, f[1], f[2]})
	}
	if len(out) < 8 {
		t.Fatalf("the humanBytes golden holds %d rows; it is meant to be the whole table", len(out))
	}
	return out
}

// humanDuration renders a CEILING — "a session never lives longer than X" —
// so no branch may round UP: a 30-minute SESSION_MAX_TTL that reads "1 hour"
// promises the reader half an hour the setting does not give them. Sub-hour
// values are reachable (config.go loads SESSION_MAX_TTL with no minimum, and
// auth.SessionMaxTTL only substitutes the default for <= 0), which is why the
// minutes branch exists at all.
func TestHumanDurationNeverRoundsUp(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{2160 * time.Hour, "90 days"}, // the shipped default
		{48 * time.Hour, "2 days"},    //
		{47 * time.Hour, "1 day"},     // floors: 1 day 23 h is not 2 days
		{24 * time.Hour, "1 day"},     //
		{23*time.Hour + 59*time.Minute, "23 hours"},
		{23 * time.Hour, "23 hours"},         //
		{2 * time.Hour, "2 hours"},           //
		{time.Hour, "1 hour"},                //
		{90 * time.Minute, "1 hour"},         // floors, and stays singular
		{59 * time.Minute, "59 minutes"},     // used to read "1 hour"
		{30 * time.Minute, "30 minutes"},     // used to read "1 hour"
		{time.Minute, "1 minute"},            //
		{59 * time.Second, "under a minute"}, // 0 minutes would be a lie
		{time.Second, "under a minute"},      //
		{0, "under a minute"},                // never reaches the page: auth substitutes the default
	} {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
