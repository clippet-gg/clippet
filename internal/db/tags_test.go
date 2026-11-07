package db

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	"clippet/internal/ids"
)

// TestCleanTagGrammar is the §20 erratum 81 grammar, one row per rule. The
// accepted shape is ^[a-z0-9][a-z0-9-]{0,31}$ after a trim and an ASCII-only
// lower-casing, and nothing else is accepted — in particular nothing that a
// Unicode case fold would have turned into an ASCII letter.
func TestCleanTagGrammar(t *testing.T) {
	long32 := strings.Repeat("a", 32)
	cases := []struct {
		in   string
		want string // "" = rejected
	}{
		{"gaming", "gaming"},
		{"GAMING", "gaming"},
		{"  clutch  ", "clutch"},
		{"CS2", "cs2"},
		{"1v5", "1v5"},
		{"best-of-2026", "best-of-2026"},
		{"a", "a"},
		{"0", "0"},
		{long32, long32},
		{"a-", "a-"}, // a trailing dash is fine; only a LEADING one is not
		{long32 + "a", ""},
		{"", ""},
		{"   ", ""},
		{"-lead", ""}, // must start with a letter or a digit
		{"has space", ""},
		{"comma,tag", ""},
		{"slash/tag", ""},
		{"dot.tag", ""},
		{"under_score", ""},
		{"hash#tag", ""},
		{"emoji🙂", ""},
		{"café", ""},
		{"K", ""}, // KELVIN SIGN: a Unicode fold would make this "k"
		{"İ", ""}, // LATIN CAPITAL I WITH DOT ABOVE: folds to "i̇"
		{"tab\there", ""},
		{"new\nline", ""},
		{"nul\x00", ""},
	}
	for _, c := range cases {
		got, err := CleanTag(c.in)
		if c.want == "" {
			if err == nil {
				t.Errorf("CleanTag(%q) = %q, want a rejection", c.in, got)
				continue
			}
			var bad *BadTagError
			if !errors.As(err, &bad) {
				t.Errorf("CleanTag(%q) error %T, want *BadTagError", c.in, err)
				continue
			}
			// The message must NAME the offending value: a rejected list is
			// useless to a person who cannot see which word was wrong.
			if !strings.Contains(err.Error(), "tag ") {
				t.Errorf("CleanTag(%q) message %q does not name the tag", c.in, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("CleanTag(%q) = %q, %v; want %q, nil", c.in, got, err, c.want)
		}
	}
}

// TestCleanTagsCollapsesAndSorts: duplicates (after normalisation) collapse,
// the result is sorted, and an empty input is an empty non-nil set.
func TestCleanTagsCollapsesAndSorts(t *testing.T) {
	got, err := CleanTags([]string{"Zulu", "alpha", "ZULU", " alpha ", "mike"})
	if err != nil {
		t.Fatalf("CleanTags: %v", err)
	}
	if !slices.Equal(got, []string{"alpha", "mike", "zulu"}) {
		t.Fatalf("CleanTags = %v", got)
	}
	empty, err := CleanTags(nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("CleanTags(nil) = %v, %v; want an empty non-nil slice", empty, err)
	}
}

// TestCleanTagsCap: the cap counts SURVIVING tags, so twelve copies of one
// word are one tag, while eleven distinct ones are a rejection that blames no
// single tag.
func TestCleanTagsCap(t *testing.T) {
	dupes := make([]string, 12)
	for i := range dupes {
		dupes[i] = "same"
	}
	if got, err := CleanTags(dupes); err != nil || !slices.Equal(got, []string{"same"}) {
		t.Fatalf("CleanTags(12 dupes) = %v, %v", got, err)
	}
	many := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}
	_, err := CleanTags(many)
	var too *TooManyTagsError
	if !errors.As(err, &too) {
		t.Fatalf("CleanTags(11) error %v, want *TooManyTagsError", err)
	}
	if !strings.Contains(err.Error(), "10") {
		t.Errorf("cap message %q does not name the cap", err)
	}
	if _, err := CleanTags(many[:10]); err != nil {
		t.Errorf("CleanTags(10): %v", err)
	}
}

func TestSplitTags(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"gaming, clutch", []string{"gaming", " clutch"}},
		{"gaming,", []string{"gaming"}},
		{",,", nil},
		{"", nil},
		{"   ", nil},
	}
	for _, c := range cases {
		got := SplitTags(c.in)
		if len(got) != len(c.want) {
			t.Errorf("SplitTags(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("SplitTags(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

func TestSetAndReadClipTags(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	c, _ := mkClip(t, d, u, 1000, nil, false)

	if tags, err := d.TagsOf(ctx, c.ID); err != nil || len(tags) != 0 || tags == nil {
		t.Fatalf("TagsOf on a fresh clip = %v, %v; want an empty non-nil slice", tags, err)
	}
	if err := d.SetClipTags(ctx, c.ID, []string{"Clutch", "gaming", "clutch"}); err != nil {
		t.Fatalf("SetClipTags: %v", err)
	}
	tags, err := d.TagsOf(ctx, c.ID)
	if err != nil || !slices.Equal(tags, []string{"clutch", "gaming"}) {
		t.Fatalf("TagsOf = %v, %v", tags, err)
	}
	// Replacement, not merge.
	if err := d.SetClipTags(ctx, c.ID, []string{"solo"}); err != nil {
		t.Fatalf("SetClipTags replace: %v", err)
	}
	if tags, _ := d.TagsOf(ctx, c.ID); !slices.Equal(tags, []string{"solo"}) {
		t.Fatalf("after replace = %v", tags)
	}
	// Clearing.
	if err := d.SetClipTags(ctx, c.ID, nil); err != nil {
		t.Fatalf("SetClipTags clear: %v", err)
	}
	if tags, _ := d.TagsOf(ctx, c.ID); len(tags) != 0 {
		t.Fatalf("after clear = %v", tags)
	}
	// A bad tag never reaches the table.
	if err := d.SetClipTags(ctx, c.ID, []string{"ok", "NOT OK"}); err == nil {
		t.Fatal("SetClipTags with a bad tag: want an error")
	}
	if tags, _ := d.TagsOf(ctx, c.ID); len(tags) != 0 {
		t.Fatalf("a rejected set wrote rows: %v", tags)
	}
}

func TestSetClipTagsOnMissingOrDeletingClip(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	c, _ := mkClip(t, d, u, 1000, nil, false)
	if err := d.SetClipTags(ctx, "AAAAAAAAAAAA", []string{"x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetClipTags on a missing clip = %v, want ErrNotFound", err)
	}
	if err := d.MarkDeleting(ctx, c.ID, 2000); err != nil {
		t.Fatal(err)
	}
	if err := d.SetClipTags(ctx, c.ID, []string{"x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetClipTags on a tombstone = %v, want ErrNotFound", err)
	}
}

// TestCreateClipWithTags: the tag set is written in the SAME transaction as
// the row, so a clip is never briefly visible without the tags it was
// uploaded with — and an invalid one fails the whole create.
func TestCreateClipWithTags(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	id := "TagsAtBirth"[:0] + "AaBbCcDdEeFf"
	c, _, err := d.CreateClipWithJob(ctx, NewClip{
		ID: id, OwnerID: u.ID, Title: "t", SourceKey: "k", SourceFilename: "f.mp4",
		SourceSize: 1, SourceContentType: "video/mp4", SourceETag: "e",
		Tags: []string{"Gaming", "gaming", "clutch"}, Now: 1000,
	})
	if err != nil {
		t.Fatalf("CreateClipWithJob: %v", err)
	}
	if tags, _ := d.TagsOf(ctx, c.ID); !slices.Equal(tags, []string{"clutch", "gaming"}) {
		t.Fatalf("tags at birth = %v", tags)
	}
	_, _, err = d.CreateClipWithJob(ctx, NewClip{
		ID: "BbCcDdEeFfGg", OwnerID: u.ID, Title: "t", SourceKey: "k", SourceFilename: "f.mp4",
		SourceSize: 1, SourceContentType: "video/mp4", SourceETag: "e",
		Tags: []string{"no good"}, Now: 1000,
	})
	var bad *BadTagError
	if !errors.As(err, &bad) {
		t.Fatalf("CreateClipWithJob with a bad tag = %v, want *BadTagError", err)
	}
	if ok, _ := d.ClipExists(ctx, "BbCcDdEeFfGg"); ok {
		t.Fatal("a rejected tag still created the clip")
	}
}

func TestTagsForClipsIsOneQuery(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	a, _ := mkClip(t, d, u, 1000, nil, false)
	b, _ := mkClip(t, d, u, 1001, nil, false)
	c, _ := mkClip(t, d, u, 1002, nil, false)
	if err := d.SetClipTags(ctx, a.ID, []string{"x", "y"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetClipTags(ctx, b.ID, []string{"y"}); err != nil {
		t.Fatal(err)
	}
	got, err := d.TagsForClips(ctx, []string{a.ID, b.ID, c.ID})
	if err != nil {
		t.Fatalf("TagsForClips: %v", err)
	}
	if !slices.Equal(got[a.ID], []string{"x", "y"}) || !slices.Equal(got[b.ID], []string{"y"}) {
		t.Fatalf("TagsForClips = %v", got)
	}
	if _, ok := got[c.ID]; ok {
		t.Errorf("a clip with no tags must be absent from the map, got %v", got[c.ID])
	}
	if m, err := d.TagsForClips(ctx, nil); err != nil || len(m) != 0 {
		t.Errorf("TagsForClips(nil) = %v, %v", m, err)
	}
}

// TestListClipsTagFilter: the filter narrows the SAME visibility set — it
// never reveals somebody else's unlisted clip — and paginates.
func TestListClipsTagFilter(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	me := mkUser(t, d, "carol")
	other := mkUser(t, d, "friend")

	mine1, _ := mkClip(t, d, me, 1000, nil, false)
	mine2, _ := mkClip(t, d, me, 1001, nil, false)
	myHidden, _ := mkClip(t, d, me, 1002, nil, true)
	theirs, _ := mkClip(t, d, other, 1003, nil, false)
	theirHidden, _ := mkClip(t, d, other, 1004, nil, true)
	untagged, _ := mkClip(t, d, me, 1005, nil, false)
	for _, id := range []string{mine1.ID, mine2.ID, myHidden.ID, theirs.ID, theirHidden.ID} {
		if err := d.SetClipTags(ctx, id, []string{"pick"}); err != nil {
			t.Fatal(err)
		}
	}

	got, next, err := d.ListClips(ctx, ListQuery{ViewerID: me.ID, Tag: "pick"})
	if err != nil || next != "" {
		t.Fatalf("ListClips: %v, next=%q", err, next)
	}
	want := []string{theirs.ID, myHidden.ID, mine2.ID, mine1.ID} // created_at DESC
	if len(got) != len(want) {
		t.Fatalf("filtered list = %d rows, want %d", len(got), len(want))
	}
	for i, c := range got {
		if c.ID != want[i] {
			t.Fatalf("row %d = %s, want %s", i, c.ID, want[i])
		}
		if c.ID == theirHidden.ID || c.ID == untagged.ID {
			t.Fatalf("row %d leaked %s", i, c.ID)
		}
	}
	// A tag nobody carries is an empty page, not an error.
	if got, _, err := d.ListClips(ctx, ListQuery{ViewerID: me.ID, Tag: "nobody"}); err != nil || len(got) != 0 {
		t.Fatalf("unknown tag = %d rows, %v", len(got), err)
	}
}

// TestListClipsTagCursor: a cursor is minted UNDER its filter and can only be
// spent under the same one — the alternative is a page that silently skips
// rows (§20 erratum 81).
func TestListClipsTagCursor(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	var tagged []string
	for i := 0; i < 3; i++ {
		c, _ := mkClip(t, d, u, int64(1000+i), nil, false)
		if err := d.SetClipTags(ctx, c.ID, []string{"pick"}); err != nil {
			t.Fatal(err)
		}
		tagged = append([]string{c.ID}, tagged...) // newest first
		if _, err := mkClipNoTags(t, d, u, int64(1500+i)); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	cursor := ""
	for i := 0; i < 5; i++ {
		page, next, err := d.ListClips(ctx, ListQuery{ViewerID: u.ID, Limit: 1, Cursor: cursor, Tag: "pick"})
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, c := range page {
			seen = append(seen, c.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if !slices.Equal(seen, tagged) {
		t.Fatalf("paged through %v, want %v", seen, tagged)
	}
	// The last cursor of the FILTERED list is not a cursor for the whole
	// library, and vice versa.
	_, tagCursor, err := d.ListClips(ctx, ListQuery{ViewerID: u.ID, Limit: 1, Tag: "pick"})
	if err != nil || tagCursor == "" {
		t.Fatalf("mint a filtered cursor: %v %q", err, tagCursor)
	}
	if _, _, err := d.ListClips(ctx, ListQuery{ViewerID: u.ID, Limit: 1, Cursor: tagCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("a filtered cursor spent unfiltered = %v, want ErrInvalidCursor", err)
	}
	_, plainCursor, err := d.ListClips(ctx, ListQuery{ViewerID: u.ID, Limit: 1})
	if err != nil || plainCursor == "" {
		t.Fatalf("mint a plain cursor: %v %q", err, plainCursor)
	}
	if _, _, err := d.ListClips(ctx, ListQuery{ViewerID: u.ID, Limit: 1, Cursor: plainCursor, Tag: "pick"}); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("a plain cursor spent filtered = %v, want ErrInvalidCursor", err)
	}
	// A cursor whose tag is not even a legal tag is not a cursor at all.
	if _, _, err := d.ListClips(ctx, ListQuery{ViewerID: u.ID, Cursor: encodeCursorRaw("1000:AaBbCcDdEeFf:NOT OK")}); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("a cursor with an illegal tag = %v, want ErrInvalidCursor", err)
	}
}

// TestClipDeleteCascadesTags proves the FK cascade of migration 0004: the
// tags of a destroyed clip go with the row, with no sweeper step at all.
func TestClipDeleteCascadesTags(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	c, _ := mkClip(t, d, u, 1000, nil, false)
	if err := d.SetClipTags(ctx, c.ID, []string{"keepme"}); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkDeleting(ctx, c.ID, 2000); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteClipRow(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.ro.QueryRowContext(ctx, "SELECT COUNT(*) FROM clip_tags WHERE clip_id = ?", c.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d clip_tags rows survived the clip (PRAGMA foreign_keys is not on?)", n)
	}
}

// mkClipNoTags is mkClip without the t.Fatalf, for loops that want the error.
func mkClipNoTags(t *testing.T, d *DB, owner User, now int64) (Clip, error) {
	t.Helper()
	c, _, err := d.CreateClipWithJob(context.Background(), NewClip{
		ID: idFor(now), OwnerID: owner.ID, Title: "t", SourceKey: "k", SourceFilename: "f.mp4",
		SourceSize: 1, SourceContentType: "video/mp4", SourceETag: "e", Now: now,
	})
	return c, err
}

// idFor makes a deterministic 12-character slug from a number.
func idFor(n int64) string {
	s := "Z"
	for i := int64(0); i < 11; i++ {
		s += string(ids.Alphabet[(n+i)%int64(len(ids.Alphabet))])
	}
	return s
}

// encodeCursorRaw builds a cursor from an arbitrary payload (the tests that
// prove a hostile or stale cursor is refused).
func encodeCursorRaw(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// TestListClipsTagFilterUsesTheTagIndex pins the PLAN, not the result:
// migration 0004 adds `clip_tags_tag` on `(tag, clip_id)` for exactly one
// query — the `?tag=` filter — and a predicate the planner cannot drive from
// that index leaves it dead and full-scans `clips` instead (§20 erratum 84).
// The uncorrelated IN-list is what makes the index reachable, so the guard has
// to be EXPLAIN QUERY PLAN over the statement ListClips really runs; asserting
// on the rows would pass either way.
func TestListClipsTagFilterUsesTheTagIndex(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	for i := 0; i < 20; i++ {
		c, _ := mkClip(t, d, u, int64(1000+i), nil, false)
		if i%2 == 0 {
			if err := d.SetClipTags(ctx, c.ID, []string{"pick"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// modernc's driver answers EXPLAIN QUERY PLAN like any other query: four
	// columns (id, parent, notused, detail); only the detail is interesting.
	rows, err := d.ro.QueryContext(ctx, "EXPLAIN QUERY PLAN "+listClipsSQL(true, listFilter{Tag: "pick"}, false),
		u.ID, "pick", 0, int64(0), int64(0), "", 51)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int64
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "clip_tags_tag") {
		t.Fatalf("the tag filter does not use clip_tags_tag; plan was:\n%s", joined)
	}
	// A correlated EXISTS is the failure this test exists for, and its plan
	// reads `SCAN clips` with a CORRELATED SCALAR SUBQUERY.
	if strings.Contains(joined, "CORRELATED") {
		t.Fatalf("the tag filter is evaluated once per clips row; plan was:\n%s", joined)
	}
}

// TestCleanTagCountsRunesNotBytes: the cap is 32 RUNES and the message says
// "characters", so a 20-character multi-byte tag must not be told it is
// longer than 32 characters. It is still rejected -- the grammar allows only
// [a-z0-9-] -- but with the message that names its real problem (§20
// erratum 87).
func TestCleanTagCountsRunesNotBytes(t *testing.T) {
	tag := strings.Repeat("é", 20) // 20 runes, 40 bytes
	_, err := CleanTag(tag)
	var bad *BadTagError
	if !errors.As(err, &bad) {
		t.Fatalf("CleanTag(20 runes of e-acute) = %v, want a BadTagError", err)
	}
	if strings.Contains(bad.Reason, "longer than") {
		t.Fatalf("a 20-character tag was rejected for LENGTH: %q", bad.Reason)
	}
	if !strings.Contains(bad.Reason, "a-z") {
		t.Fatalf("reason = %q, want the grammar message", bad.Reason)
	}
	// The length rule itself still holds, counted in runes.
	if _, err := CleanTag(strings.Repeat("a", 32)); err != nil {
		t.Fatalf("32 ASCII characters must be legal: %v", err)
	}
	if _, err := CleanTag(strings.Repeat("a", 33)); err == nil ||
		!strings.Contains(err.Error(), "longer than 32 characters") {
		t.Fatalf("33 characters = %v, want the length message", err)
	}
}
