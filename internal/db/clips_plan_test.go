package db

import (
	"context"
	"strings"
	"testing"
)

// explainListClips runs EXPLAIN QUERY PLAN over the EXACT statement ListClips
// runs for one shape and returns the plan lines joined. modernc answers
// EXPLAIN QUERY PLAN like any other query: four columns (id, parent, notused,
// detail); only the detail is interesting.
func explainListClips(t *testing.T, d *DB, viewerID, tag string) string {
	t.Helper()
	ctx := context.Background()
	var args []any
	if viewerID != "" {
		args = append(args, viewerID)
	}
	if tag != "" {
		args = append(args, tag)
	}
	args = append(args, 0, int64(0), int64(0), "", 51)
	rows, err := d.ro.QueryContext(ctx, "EXPLAIN QUERY PLAN "+listClipsSQL(viewerID != "", listFilter{Tag: tag}, false), args...)
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

// TestListClipsAnonymousUsesTheListedIndex pins the PLAN, not the result (§20
// erratum 100). Migration 0001 creates `clips_listed ON clips(created_at DESC,
// id) WHERE unlisted = 0` for exactly one query -- the library list -- and the
// `(unlisted = 0 OR owner_id = ?)` disjunction is a predicate no partial index
// can drive, so the anonymous page (the hot one: every crawler, every unfurl
// fetch, every logged-out visit) was a full scan of `clips` plus a sort of the
// whole table. Asserting on the rows would pass either way, which is why this
// has to be EXPLAIN over the statement ListClips really runs.
func TestListClipsAnonymousUsesTheListedIndex(t *testing.T) {
	d := openTest(t)
	u := mkUser(t, d, "carol")
	for i := 0; i < 20; i++ {
		mkClip(t, d, u, int64(1000+i), nil, i%3 == 0)
	}
	plan := explainListClips(t, d, "", "")
	if !strings.Contains(plan, "clips_listed") {
		t.Fatalf("the anonymous list does not use clips_listed; plan was:\n%s", plan)
	}
	// The index is (created_at DESC, id) and the ORDER BY matches it, so the
	// planner must not need a sort at all. This half is what a future
	// "harmless" edit to the ORDER BY would break first.
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("the anonymous list sorts the whole table; plan was:\n%s", plan)
	}
}

// TestListClipsAnonymousSeesTheSameRows: the plan changed, the answer must not.
// With no viewer, `(unlisted = 0 OR owner_id = ?)` and `unlisted = 0` select
// the same rows -- owner_id is a user id and is never "" -- and that
// equivalence is the whole argument for erratum 100.
func TestListClipsAnonymousSeesTheSameRows(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	var listed []string
	for i := 0; i < 9; i++ {
		c, _ := mkClip(t, d, u, int64(1000+i), nil, i%3 == 0)
		if i%3 != 0 {
			listed = append([]string{c.ID}, listed...) // newest first
		}
	}
	got, next, err := d.ListClips(ctx, ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Fatalf("next cursor = %q on a single page", next)
	}
	if len(got) != len(listed) {
		t.Fatalf("%d clips, want %d (the unlisted ones must stay hidden)", len(got), len(listed))
	}
	for i, c := range got {
		if c.ID != listed[i] {
			t.Fatalf("position %d = %s, want %s (newest first)", i, c.ID, listed[i])
		}
		if c.Unlisted {
			t.Fatalf("an unlisted clip reached the anonymous list: %s", c.ID)
		}
	}
}
