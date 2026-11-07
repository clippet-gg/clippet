package db

import (
	"context"
	"strings"
	"testing"
)

// oldSweepCandidatesSQL is the spelling AUDIT #29 replaced, kept here ONLY so
// this file can show the difference it makes: the plan assertion below is
// meaningless unless the old form really fails it.
const oldSweepCandidatesSQL = `
SELECT id FROM clips
WHERE state = 'deleting'
   OR (expires_at IS NOT NULL AND (
        (state IN ('queued','ready','failed') AND expires_at <= ?)
     OR (expires_at <= ? - ?)))
ORDER BY expires_at IS NULL, expires_at, id LIMIT ?`

// explainSweep runs EXPLAIN QUERY PLAN over a sweep statement with the
// arguments RunOnce binds. modernc answers EXPLAIN QUERY PLAN like any other
// query: four columns (id, parent, notused, detail); only the detail is
// interesting.
func explainSweep(t *testing.T, d *DB, query string) string {
	t.Helper()
	rows, err := d.ro.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query,
		int64(1000), int64(1000), int64(3600), 50)
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

// TestSweepCandidatesUsesTheExpiryIndex pins the PLAN, not the rows (AUDIT #29,
// §20 erratum 84's method). The sweep runs on every SWEEP_INTERVAL tick for the
// life of the process, and the rows it returns are the same whichever index the
// planner picks — so an assertion on the answer passes either way, which is
// exactly why this has to be EXPLAIN over the statement SweepCandidates really
// runs.
//
// What must hold: two index searches, ONE of them `clips_state` (the tombstone
// arm, which is what that index is for) and one `clips_expires`, and no full
// table scan. The old spelling nested a second MULTI-INDEX OR inside the
// expiry arm and drove ITS first branch off `clips_state` too, so every tick
// walked the `ready` rows of the whole instance to look at a column
// `clips_state` does not carry:
//
//	MULTI-INDEX OR / INDEX 1 SEARCH clips USING INDEX clips_state (state=?)
//	                INDEX 2 MULTI-INDEX OR / INDEX 1 SEARCH clips USING INDEX clips_state (state=?)
//	                                         INDEX 2 SEARCH clips USING INDEX clips_expires (expires_at<?)
//
// A `clips(expires_at, state)` index changes none of this and was refuted with
// the rest of AUDIT #29's mechanism; the spelling is the whole fix.
const sweepStateSearch = "SEARCH clips USING INDEX clips_state"

func TestSweepCandidatesUsesTheExpiryIndex(t *testing.T) {
	d := openTest(t)
	u := mkUser(t, d, "carol")
	for i := 0; i < 20; i++ {
		mkClip(t, d, u, int64(900+i), nil, false)
	}

	plan := explainSweep(t, d, sweepCandidatesSQL)
	if !strings.Contains(plan, "clips_expires") {
		t.Fatalf("the sweep does not use clips_expires; plan was:\n%s", plan)
	}
	if n := strings.Count(plan, sweepStateSearch); n != 1 {
		t.Fatalf("the sweep walks clips_state %d times, want exactly 1 (the tombstone arm); plan was:\n%s", n, plan)
	}
	// "SCAN clips" without "USING" is the full table scan; every acceptable plan
	// here is a SEARCH through one of the two indexes, or an OR over both.
	for _, line := range strings.Split(plan, "\n") {
		if strings.HasPrefix(line, "SCAN clips") && !strings.Contains(line, "USING") {
			t.Fatalf("the sweep scans the whole clips table; plan was:\n%s", plan)
		}
	}

	// The half that makes the assertion above worth making: the spelling this
	// replaced must really fail it, or the pin is decoration.
	old := explainSweep(t, d, oldSweepCandidatesSQL)
	if n := strings.Count(old, sweepStateSearch); n < 2 {
		t.Fatalf("the spelling AUDIT #29 replaced walks clips_state %d times, so this pin proves nothing.\n--- old ---\n%s\n--- new ---\n%s", n, old, plan)
	}
}

// TestSweepCandidatesRewriteSelectsTheSameRows: the plan changed, the answer
// must not. Both spellings are run over one fixture holding every shape the §12
// predicate distinguishes — a tombstone with and without a TTL, each state
// inside and outside its expiry, and a `processing` clip inside and outside the
// grace window — and their answers are compared id for id, in order.
func TestSweepCandidatesRewriteSelectsTheSameRows(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	const now, grace = int64(10000), int64(3600)
	exp := func(at int64) *int64 { v := at; return &v }
	for _, tc := range []struct {
		state   string
		expires *int64
	}{
		{ClipDeleting, nil},              // a manual DELETE's tombstone: no TTL
		{ClipDeleting, exp(now + 10000)}, // a tombstone whose TTL is still far off
		{ClipReady, nil},                 // no TTL at all: never a candidate
		{ClipReady, exp(now - 1)},        // rule (b)
		{ClipReady, exp(now)},            // the boundary: <= now
		{ClipReady, exp(now + 1)},        // not yet
		{ClipQueued, exp(now - 1)},       // rule (b)
		{ClipQueued, nil},                // never
		{ClipFailed, exp(now - 1)},       // rule (b)
		{ClipProcessing, exp(now - 1)},   // expired but inside the grace window
		{ClipProcessing, exp(now - grace)},
		{ClipProcessing, exp(now - grace - 1)}, // rule (c)
	} {
		c, _ := mkClip(t, d, u, 1, nil, false)
		if _, err := d.exec(ctx, "UPDATE clips SET state = ?, expires_at = ? WHERE id = ?",
			tc.state, nullInt64(tc.expires), c.ID); err != nil {
			t.Fatal(err)
		}
	}

	read := func(query string) []string {
		t.Helper()
		rows, err := d.ro.QueryContext(ctx, query, now, now, grace, 50)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got, want := read(sweepCandidatesSQL), read(oldSweepCandidatesSQL)
	if len(want) == 0 {
		t.Fatal("the fixture produced no candidates at all, so the comparison is vacuous")
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the rewrite changed the candidate set (or its order)\nnew: %v\nold: %v", got, want)
	}
}
