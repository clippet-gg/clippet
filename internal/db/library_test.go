package db

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// TestCleanSearchGrammar pins the whole of `?q=`'s normalisation and its
// rejections, including the hostile shapes a search box invites: the LIKE
// metacharacters, a 10 KB paste, and a non-ASCII case pair that a full
// Unicode fold would silently merge.
func TestCleanSearchGrammar(t *testing.T) {
	long := strings.Repeat("a", MaxSearchRunes+1)
	for _, tc := range []struct {
		name, in, want string
		wantErr        bool
	}{
		{"plain", "clutch", "clutch", false},
		{"trimmed and lowered", "  CluTch  ", "clutch", false},
		{"empty is not a filter", "   ", "", false},
		{"percent survives as text", "50%", "50%", false},
		{"underscore survives as text", "a_b", "a_b", false},
		{"backslash survives as text", `a\b`, `a\b`, false},
		{"at the cap", strings.Repeat("a", MaxSearchRunes), strings.Repeat("a", MaxSearchRunes), false},
		{"over the cap", long, "", true},
		{"a 10 KB paste", strings.Repeat("x", 10<<10), "", true},
		{"control character", "a\x00b", "", true},
		{"newline", "a\nb", "", true},
		{"invalid utf-8", "a\xffb", "", true},
		// The ASCII-only fold: a K is a k, a KELVIN SIGN is not (erratum 81's
		// rule, applied to this field).
		{"ascii fold", "K", "k", false},
		{"kelvin is left alone", "K", "K", false},
		{"runes are counted, not bytes", strings.Repeat("é", MaxSearchRunes), strings.Repeat("é", MaxSearchRunes), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CleanSearch(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidSearch) {
					t.Fatalf("CleanSearch(%q) err = %v, want ErrInvalidSearch", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CleanSearch(%q) = %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("CleanSearch(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEscapeLike: the three characters LIKE gives meaning to are quoted, and
// nothing else is touched.
func TestEscapeLike(t *testing.T) {
	for in, want := range map[string]string{
		"plain": "plain",
		"50%":   `50\%`,
		"a_b":   `a\_b`,
		`a\b`:   `a\\b`,
		"%_%":   `\%\_\%`,
	} {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestListClipsSearch: `?q=` matches a substring of the title OR of the
// description, case-insensitively, and the metacharacters are text.
func TestListClipsSearch(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	titles := map[string]string{
		"clutch":  "The CLUTCH round",
		"percent": "Nailed 50% of them",
		"under":   "snake_case name",
		"other":   "nothing to see",
	}
	byName := map[string]string{}
	now := int64(1000)
	for name, title := range titles {
		c, _ := mkClip(t, d, u, now, nil, false)
		if _, err := d.UpdateClipMeta(ctx, c.ID, ClipPatch{Title: &title}, now); err != nil {
			t.Fatal(err)
		}
		byName[name] = c.ID
		now++
	}
	// One clip whose DESCRIPTION carries the needle and whose title does not.
	desc := "a note about the clutch"
	c, _ := mkClip(t, d, u, now, nil, false)
	if _, err := d.UpdateClipMeta(ctx, c.ID, ClipPatch{Description: &desc}, now); err != nil {
		t.Fatal(err)
	}
	byName["desc"] = c.ID

	search := func(q string) []string {
		t.Helper()
		clean, err := CleanSearch(q)
		if err != nil {
			t.Fatalf("CleanSearch(%q): %v", q, err)
		}
		got, _, err := d.ListClips(ctx, ListQuery{ViewerID: u.ID, Q: clean})
		if err != nil {
			t.Fatalf("ListClips(q=%q): %v", q, err)
		}
		out := make([]string, len(got))
		for i, c := range got {
			out[i] = c.ID
		}
		return out
	}
	has := func(ids []string, want ...string) {
		t.Helper()
		if len(ids) != len(want) {
			t.Fatalf("got %d rows, want %d (%v)", len(ids), len(want), ids)
		}
		for _, w := range want {
			found := false
			for _, id := range ids {
				if id == w {
					found = true
				}
			}
			if !found {
				t.Fatalf("row %s missing from %v", w, ids)
			}
		}
	}
	has(search("clutch"), byName["clutch"], byName["desc"])
	has(search("CLUTCH"), byName["clutch"], byName["desc"])
	// A LIKE metacharacter is a character. Without ESCAPE, "50%" would match
	// every row and "_" would match any single one.
	has(search("50%"), byName["percent"])
	has(search("e_c"), byName["under"])
	has(search("no such clip"))
}

// TestListClipsSearchPlanScansOnlyClips: the `%needle%` LIKE cannot use an
// index in any database, which is why no migration adds one — but it must
// stay a scan of ONE table. A predicate that dragged clip_tags or jobs into
// the plan would be a join nobody asked for.
func TestListClipsSearchPlanScansOnlyClips(t *testing.T) {
	d := openTest(t)
	u := mkUser(t, d, "carol")
	for i := 0; i < 5; i++ {
		mkClip(t, d, u, int64(1000+i), nil, false)
	}
	plan := explain(t, d, listClipsSQL(true, listFilter{Q: "x"}, false),
		u.ID, "%x%", "%x%", 0, int64(0), int64(0), "", 51)
	if strings.Contains(plan, "clip_tags") || strings.Contains(plan, "jobs") {
		t.Fatalf("the search filter touches another table; plan was:\n%s", plan)
	}
	if !strings.Contains(plan, "clips") {
		t.Fatalf("the search filter does not read clips at all; plan was:\n%s", plan)
	}
}

// TestListClipsTagFilterKeepsItsIndexUnderTheNewFilters: erratum 84's index
// must still be reachable now that `?q=` and `?mine=1` can be applied beside
// `?tag=`. A predicate order that pushed the tag IN-list behind a scan would
// undo that erratum silently.
func TestListClipsTagFilterKeepsItsIndexUnderTheNewFilters(t *testing.T) {
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
	for _, tc := range []struct {
		name string
		f    listFilter
		args []any
	}{
		{"tag + mine", listFilter{Tag: "pick", Mine: true}, []any{u.ID, "pick", u.ID, 0, int64(0), int64(0), "", 51}},
		{"tag + q", listFilter{Tag: "pick", Q: "t"}, []any{u.ID, "pick", "%t%", "%t%", 0, int64(0), int64(0), "", 51}},
		{"tag + mine + q", listFilter{Tag: "pick", Mine: true, Q: "t"},
			[]any{u.ID, "pick", u.ID, "%t%", "%t%", 0, int64(0), int64(0), "", 51}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := explain(t, d, listClipsSQL(true, tc.f, false), tc.args...)
			if !strings.Contains(plan, "clip_tags_tag") {
				t.Fatalf("the tag filter lost its index; plan was:\n%s", plan)
			}
			if strings.Contains(plan, "CORRELATED") {
				t.Fatalf("the tag filter became correlated; plan was:\n%s", plan)
			}
		})
	}
}

// explain runs EXPLAIN QUERY PLAN over the exact statement ListClips runs and
// returns the details, one per line.
func explain(t *testing.T, d *DB, query string, args ...any) string {
	t.Helper()
	rows, err := d.ro.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
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
	return strings.Join(plan, "\n")
}

// TestListClipsMineAndScopeNarrowIdentically: `?mine=1` and LIBRARY_SCOPE=own
// are the same predicate, and NEITHER can reach another user's clip — listed
// or not. The pair is the visibility matrix in one table.
func TestListClipsMineAndScopeNarrowIdentically(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	me := mkUser(t, d, "carol")
	them := mkUser(t, d, "friend")
	mine, _ := mkClip(t, d, me, 1000, nil, false)
	mineUnlisted, _ := mkClip(t, d, me, 1001, nil, true)
	theirs, _ := mkClip(t, d, them, 1002, nil, false)
	theirSecret, _ := mkClip(t, d, them, 1003, nil, true)

	ids := func(q ListQuery) map[string]bool {
		t.Helper()
		got, _, err := d.ListClips(ctx, q)
		if err != nil {
			t.Fatalf("ListClips: %v", err)
		}
		out := map[string]bool{}
		for _, c := range got {
			out[c.ID] = true
		}
		return out
	}
	shared := ids(ListQuery{ViewerID: me.ID})
	for _, want := range []string{mine.ID, mineUnlisted.ID, theirs.ID} {
		if !shared[want] {
			t.Fatalf("the shared library is missing %s", want)
		}
	}
	if shared[theirSecret.ID] {
		t.Fatal("the shared library leaked another user's unlisted clip")
	}
	for _, tc := range []struct {
		name string
		q    ListQuery
	}{
		{"?mine=1", ListQuery{ViewerID: me.ID, Mine: true}},
		{"LIBRARY_SCOPE=own", ListQuery{ViewerID: me.ID, ScopeOwn: true}},
		{"both at once", ListQuery{ViewerID: me.ID, Mine: true, ScopeOwn: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(tc.q)
			if len(got) != 2 || !got[mine.ID] || !got[mineUnlisted.ID] {
				t.Fatalf("got %v, want exactly my own two clips", got)
			}
		})
	}
	// And the OTHER user under scope=own sees theirs, never mine.
	got := ids(ListQuery{ViewerID: them.ID, ScopeOwn: true})
	if len(got) != 2 || !got[theirs.ID] || !got[theirSecret.ID] {
		t.Fatalf("the friend's own library = %v", got)
	}
}

// TestCursorShapesAndBinding: the three cursor shapes round-trip, the two
// legacy ones are still MINTED (not merely decoded), and a cursor spent under
// a different filter is refused.
func TestCursorShapesAndBinding(t *testing.T) {
	id := "AaBbCcDdEeFf"
	for _, tc := range []struct {
		name    string
		f       listFilter
		wantRaw string
	}{
		{"unfiltered keeps the two-field shape", listFilter{}, "1000:" + id},
		{"a tag alone keeps the three-field shape", listFilter{Tag: "gaming"}, "1000:" + id + ":gaming"},
		{"mine alone", listFilter{Mine: true}, "1000:" + id + ":mine=1"},
		{"a search alone", listFilter{Q: "clutch"}, "1000:" + id + ":q=clutch"},
		{"all three", listFilter{Tag: "gaming", Q: "a b", Mine: true}, "1000:" + id + ":mine=1&q=a+b&t=gaming"},
		{"a search with a colon", listFilter{Q: "a:b"}, "1000:" + id + ":q=a%3Ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := encodeCursor(1000, id, tc.f)
			if got := decodeRaw(t, cur); got != tc.wantRaw {
				t.Fatalf("cursor payload = %q, want %q", got, tc.wantRaw)
			}
			at, gotID, gotF, err := decodeCursor(cur)
			if err != nil || at != 1000 || gotID != id || gotF != tc.f {
				t.Fatalf("decodeCursor = %d %q %+v %v", at, gotID, gotF, err)
			}
		})
	}
	// Forged and corrupted tokens are refused rather than trusted.
	for _, payload := range []string{
		"1000:" + id + ":NOT OK",        // a tag that is not a tag
		"1000:" + id + ":mine=2",        // a switch that is not on
		"1000:" + id + ":owner=someone", // a key this version never mints
		"1000:" + id + ":q=",            // an empty search is not a filter
		"1000:" + id + ":q=" + strings.Repeat("a", MaxSearchRunes+1),
		"1000:" + id + ":q=A",           // not normalised: we minted lower case
		"1000:" + id + ":mine=1&mine=1", // one value per key
	} {
		if _, _, _, err := decodeCursor(encodeCursorRaw(payload)); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("decodeCursor(%q) err = %v, want ErrInvalidCursor", payload, err)
		}
	}
}

// decodeRaw returns the cursor's decoded payload, so a test can pin the exact
// SHAPE a filter mints and not merely that it round-trips.
func decodeRaw(t *testing.T, cursor string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	return string(raw)
}

// TestListClipsCursorCarriesEveryFilter: a page token minted under one filter
// is only spendable under that same filter — the erratum 81 rule, now over
// three filters rather than one.
func TestListClipsCursorCarriesEveryFilter(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	for i := 0; i < 3; i++ {
		c, _ := mkClip(t, d, u, int64(1000+i), nil, false)
		if err := d.SetClipTags(ctx, c.ID, []string{"gaming"}); err != nil {
			t.Fatal(err)
		}
	}
	full := ListQuery{ViewerID: u.ID, Limit: 1, Tag: "gaming", Q: "t", Mine: true}
	_, next, err := d.ListClips(ctx, full)
	if err != nil || next == "" {
		t.Fatalf("ListClips: %v, cursor %q", err, next)
	}
	// The same filter continues; every partial one is refused.
	if _, _, err := d.ListClips(ctx, ListQuery{ViewerID: u.ID, Limit: 1, Tag: "gaming", Q: "t", Mine: true, Cursor: next}); err != nil {
		t.Fatalf("the cursor did not continue its own list: %v", err)
	}
	for _, q := range []ListQuery{
		{ViewerID: u.ID, Limit: 1, Cursor: next},
		{ViewerID: u.ID, Limit: 1, Tag: "gaming", Cursor: next},
		{ViewerID: u.ID, Limit: 1, Q: "t", Mine: true, Cursor: next},
		{ViewerID: u.ID, Limit: 1, Tag: "gaming", Q: "other", Mine: true, Cursor: next},
	} {
		if _, _, err := d.ListClips(ctx, q); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("a cursor spent under %+v err = %v, want ErrInvalidCursor", q, err)
		}
	}
	// The SCOPE is not part of the binding: flipping LIBRARY_SCOPE must not
	// invalidate every outstanding page token.
	scoped := full
	scoped.ScopeOwn = true
	scoped.Cursor = next
	if _, _, err := d.ListClips(ctx, scoped); err != nil {
		t.Fatalf("LIBRARY_SCOPE invalidated a live cursor: %v", err)
	}
}

// TestListClipsSearchPages: a filtered list pages one row at a time and the
// pages hold exactly the matching set, in the library's own order.
func TestListClipsSearchPages(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	var want []string
	for i := 0; i < 3; i++ {
		c, _ := mkClip(t, d, u, int64(1000+i), nil, false)
		title := "needle " + string(rune('a'+i))
		if _, err := d.UpdateClipMeta(ctx, c.ID, ClipPatch{Title: &title}, 1000); err != nil {
			t.Fatal(err)
		}
		want = append([]string{c.ID}, want...) // newest first
	}
	mkClip(t, d, u, 1100, nil, false) // a row the filter must not reach

	var got []string
	cursor := ""
	for i := 0; ; i++ {
		if i > 5 {
			t.Fatal("the search did not terminate within 6 pages")
		}
		page, next, err := d.ListClips(ctx, ListQuery{ViewerID: u.ID, Limit: 1, Q: "needle", Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page {
			got = append(got, c.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("paged %v, want %v", got, want)
	}
}

// TestAddClipTag: the one add-one tag verb. It adds, it is idempotent, it
// respects the clip's own cap, and it never touches another tag.
func TestAddClipTag(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	c, _ := mkClip(t, d, u, 1000, nil, false)
	if err := d.SetClipTags(ctx, c.ID, []string{"gaming"}); err != nil {
		t.Fatal(err)
	}
	if err := d.AddClipTag(ctx, c.ID, "clutch", 2000); err != nil {
		t.Fatalf("AddClipTag: %v", err)
	}
	tags, err := d.TagsOf(ctx, c.ID)
	if err != nil || strings.Join(tags, ",") != "clutch,gaming" {
		t.Fatalf("tags = %v (%v), want the old one AND the new one", tags, err)
	}
	// Idempotent, and a no-op does not stamp the clip.
	before := mustGet(t, d, c.ID).UpdatedAt
	if err := d.AddClipTag(ctx, c.ID, "clutch", 3000); err != nil {
		t.Fatalf("AddClipTag (again): %v", err)
	}
	if got := mustGet(t, d, c.ID).UpdatedAt; got != before {
		t.Fatalf("a no-op add stamped updated_at (%d → %d)", before, got)
	}
	// The cap is the CLIP's ten (erratum 81), enforced here too.
	full := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	if err := d.SetClipTags(ctx, c.ID, full); err != nil {
		t.Fatal(err)
	}
	if err := d.AddClipTag(ctx, c.ID, "k", 4000); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("the eleventh tag err = %v, want ErrLimitReached", err)
	}
	// An eleventh that is ALREADY there is still a no-op, not a cap failure.
	if err := d.AddClipTag(ctx, c.ID, "a", 4000); err != nil {
		t.Fatalf("adding an existing tag to a full clip: %v", err)
	}
	// A tombstone never gains a tag.
	if err := d.MarkDeleting(ctx, c.ID, 5000); err != nil {
		t.Fatal(err)
	}
	if err := d.AddClipTag(ctx, c.ID, "z", 5000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tagging a tombstone err = %v, want ErrNotFound", err)
	}
	if err := d.AddClipTag(ctx, "NoSuchClipId", "z", 5000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tagging a missing clip err = %v, want ErrNotFound", err)
	}
}

// TestAddClipToCollection: an append that does not disturb the order, is
// idempotent, refuses a stranger's clip and a stranger's collection, and
// stops at the membership cap.
func TestAddClipToCollection(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	me := mkUser(t, d, "carol")
	them := mkUser(t, d, "friend")
	first, _ := mkClip(t, d, me, 1000, nil, false)
	second, _ := mkClip(t, d, me, 1001, nil, false)
	third, _ := mkClip(t, d, me, 1002, nil, false)
	theirs, _ := mkClip(t, d, them, 1003, nil, false)
	col, err := d.CreateCollection(ctx, "CoLLeCtIoN1", me.ID, "best of", 1000)
	if err != nil {
		t.Fatal(err)
	}
	// The order the PUT chose stays the order; the append lands after it.
	if err := d.SetCollectionClips(ctx, col.ID, me.ID, []string{third.ID, first.ID}, 1000); err != nil {
		t.Fatal(err)
	}
	if err := d.AddClipToCollection(ctx, col.ID, me.ID, second.ID, 2000); err != nil {
		t.Fatalf("AddClipToCollection: %v", err)
	}
	members, err := d.CollectionClips(ctx, col.ID)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range members {
		order = append(order, c.ID)
	}
	if strings.Join(order, ",") != strings.Join([]string{third.ID, first.ID, second.ID}, ",") {
		t.Fatalf("membership = %v, want the PUT's order with the append at the end", order)
	}
	// Idempotent, and it does not move an existing member.
	if err := d.AddClipToCollection(ctx, col.ID, me.ID, third.ID, 3000); err != nil {
		t.Fatalf("re-adding a member: %v", err)
	}
	members, _ = d.CollectionClips(ctx, col.ID)
	if len(members) != 3 || members[0].ID != third.ID {
		t.Fatalf("a re-add changed the membership: %v", members)
	}
	// A stranger's clip, and a clip that is not there, are one answer.
	var notYours *NotYourClipError
	if err := d.AddClipToCollection(ctx, col.ID, me.ID, theirs.ID, 3000); !errors.As(err, &notYours) {
		t.Fatalf("a stranger's clip err = %v, want NotYourClipError", err)
	}
	if err := d.AddClipToCollection(ctx, col.ID, me.ID, "NoSuchClipId", 3000); !errors.As(err, &notYours) {
		t.Fatalf("a missing clip err = %v, want NotYourClipError", err)
	}
	// A stranger's collection is 404-shaped, exactly as every other
	// collection mutation is (erratum 82).
	if err := d.AddClipToCollection(ctx, col.ID, them.ID, first.ID, 3000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a stranger's collection err = %v, want ErrNotFound", err)
	}
}

// TestProgressRows: one query, the transcode job's stage (never a later
// job's), and unknown or tombstoned ids simply absent.
func TestProgressRows(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	queued, _ := mkClip(t, d, u, 1000, nil, false)
	gone, _ := mkClip(t, d, u, 1001, nil, false)
	if err := d.MarkDeleting(ctx, gone.ID, 1002); err != nil {
		t.Fatal(err)
	}
	rows, err := d.ProgressRows(ctx, []string{queued.ID, gone.ID, "NoSuchClipId"})
	if err != nil {
		t.Fatalf("ProgressRows: %v", err)
	}
	if len(rows) != 1 || rows[0].ClipID != queued.ID {
		t.Fatalf("rows = %+v, want only the live clip", rows)
	}
	if rows[0].State != ClipQueued || rows[0].Stage != JobStageQueued {
		t.Fatalf("row = %+v, want a queued clip on its transcode job", rows[0])
	}
	// A clip with no ids at all, and a batch over the cap.
	if got, err := d.ProgressRows(ctx, nil); err != nil || got != nil {
		t.Fatalf("ProgressRows(nil) = %v, %v", got, err)
	}
	too := make([]string, MaxProgressIDs+1)
	for i := range too {
		too[i] = queued.ID
	}
	if _, err := d.ProgressRows(ctx, too); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("an over-long batch err = %v, want ErrLimitReached", err)
	}
}

func mustGet(t *testing.T, d *DB, id string) Clip {
	t.Helper()
	c, err := d.GetClip(context.Background(), id)
	if err != nil {
		t.Fatalf("GetClip: %v", err)
	}
	return c
}
