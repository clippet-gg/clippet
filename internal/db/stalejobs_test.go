package db

import (
	"context"
	"testing"
)

// StaleRunningJobs is the candidate list of the AUTOMATIC repair (§20 erratum
// 231). What is pinned here is the property a merge broke once before, when
// `admin jobs` came to know four of seven kinds: the predicate has no kind in
// it at all.

// insertJob puts one job row in whatever state a test needs, without going
// through a claim — the point is the population, not how it came about.
func insertJob(t *testing.T, d *DB, clipID, kind, state string, startedAt *int64) int64 {
	t.Helper()
	res, err := d.rw.ExecContext(context.Background(), `
INSERT INTO jobs (clip_id, kind, state, attempts, transient_failures, run_after, created_at, updated_at, started_at)
VALUES (?, ?, ?, 0, 0, 0, 1000, 1000, ?)`, clipID, kind, state, startedAt)
	if err != nil {
		t.Fatalf("insert %s job: %v", kind, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStaleRunningJobsCoversEveryKindAndOnlyRunningRows(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "carol")
	c, seed := mkClip(t, d, u, 1000, nil, false)
	// The clip's own transcode job is queued; nothing may see it.
	old, young := int64(1000), int64(9000)

	// Every kind that exists today. The list is HERE and nowhere in the
	// implementation: a kind added tomorrow is covered by the query without
	// anybody editing it, which is exactly the invariant being pinned.
	kinds := []string{KindTranscode, KindNotify, KindTrim, KindGIF, KindSocial, KindReplace, KindPoster}
	want := map[int64]string{}
	for _, k := range kinds {
		want[insertJob(t, d, c.ID, k, JobRunning, &old)] = k
		// …and a young one of the same kind, which the age test must exclude.
		insertJob(t, d, c.ID, k, JobRunning, &young)
	}
	// Terminal and queued rows are not candidates however old they are.
	for _, st := range []string{JobQueued, JobDone, JobFailed, JobCancelled} {
		insertJob(t, d, c.ID, KindTranscode, st, &old)
	}
	// A `running` row ClaimJob never stamped is stale by definition: the row is
	// already inconsistent and the repair is the point (`staleEnough`).
	nullStarted := insertJob(t, d, c.ID, KindGIF, JobRunning, nil)
	want[nullStarted] = KindGIF

	got, err := d.StaleRunningJobs(ctx, 5000, 0)
	if err != nil {
		t.Fatalf("StaleRunningJobs: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d (one per kind plus the NULL started_at row)", len(got), len(want))
	}
	seen := map[string]bool{}
	last := int64(0)
	for _, j := range got {
		k, ok := want[j.ID]
		if !ok {
			t.Fatalf("job #%d (%s, started %v) is not a candidate", j.ID, j.Kind, j.StartedAt)
		}
		if j.Kind != k {
			t.Errorf("job #%d came back as %q, want %q", j.ID, j.Kind, k)
		}
		if j.ID <= last {
			t.Errorf("candidates are not in id order: %d after %d", j.ID, last)
		}
		last = j.ID
		seen[j.Kind] = true
	}
	for _, k := range kinds {
		if !seen[k] {
			t.Errorf("kind %q was not among the candidates: the predicate is not kind-generic", k)
		}
	}
	if seed.State != JobQueued {
		t.Fatalf("the clip's own transcode job is %s; this test assumed queued", seed.State)
	}

	// The limit truncates, and 0 means the default rather than "none".
	if got, err := d.StaleRunningJobs(ctx, 5000, 3); err != nil || len(got) != 3 {
		t.Fatalf("StaleRunningJobs(limit 3) = %d rows (%v)", len(got), err)
	}
	// A threshold before every started_at names nobody.
	if got, err := d.StaleRunningJobs(ctx, 999, 0); err != nil || len(got) != 1 {
		// …except the NULL started_at row, which is stale at every threshold.
		t.Fatalf("StaleRunningJobs at a threshold older than every row = %d rows (%v), want only the NULL one", len(got), err)
	}
}
