package db

import (
	"context"
	"testing"
)

// TestSweepCandidatesPutsTheUntimedTombstonesLast pins the ORDER, which is the
// whole of §20 erratum 101's database half. SQLite sorts NULL first, so the
// plain `ORDER BY expires_at` this query used to carry returned every
// `deleting` tombstone with no TTL — a manual DELETE, the one candidate class
// that piles up — ahead of every clip whose TTL had actually run out. The
// sweeper walks the batch in order and synchronously, at up to
// STORAGE_OP_TIMEOUT per Destroy against a 55 s tick, so a couple of
// blackholing prefixes at the head starved TTL expiry entirely.
func TestSweepCandidatesPutsTheUntimedTombstonesLast(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "a")

	// Two tombstones with no TTL: `deleting`, expires_at NULL.
	var tombstones []string
	for i := 0; i < 2; i++ {
		c, _ := mkClip(t, d, u, 100, nil, false)
		if err := d.MarkDeleting(ctx, c.ID, 100); err != nil {
			t.Fatal(err)
		}
		tombstones = append(tombstones, c.ID)
	}
	// Three clips whose TTL has run out, oldest expiry first.
	ttl := int64(10)
	var expired []string
	for i := 0; i < 3; i++ {
		c, _ := mkClip(t, d, u, int64(200+i), &ttl, false)
		expired = append(expired, c.ID)
	}

	got, err := d.SweepCandidates(ctx, 1000, 3600, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("%d candidates, want 5: %v", len(got), got)
	}
	// The TTL-driven work comes first, in expiry order…
	for i, want := range expired {
		if got[i] != want {
			t.Fatalf("position %d = %s, want the expiring clip %s (order was %v)", i, got[i], want, got)
		}
	}
	// …and the untimed tombstones come last, so a failing one can never
	// crowd out an expiry.
	tail := map[string]bool{got[3]: true, got[4]: true}
	for _, id := range tombstones {
		if !tail[id] {
			t.Fatalf("tombstone %s is not in the tail of %v", id, got)
		}
	}
}

// The order is TOTAL: two rows with the same expires_at are broken by id, so
// a batch is the same list every tick rather than whatever the scan happened
// to produce. Without that, a candidate at the batch boundary could be
// skipped and re-skipped forever.
func TestSweepCandidatesOrderIsStable(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	u := mkUser(t, d, "a")
	ttl := int64(10)
	for i := 0; i < 6; i++ {
		mkClip(t, d, u, 200, &ttl, false) // identical created_at → identical expires_at
	}
	first, err := d.SweepCandidates(ctx, 1000, 3600, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 4 {
		t.Fatalf("%d candidates, want the batch of 4", len(first))
	}
	for i := 0; i < 3; i++ {
		again, err := d.SweepCandidates(ctx, 1000, 3600, 4)
		if err != nil {
			t.Fatal(err)
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("batch %d differs at %d: %v vs %v", i, j, again, first)
			}
		}
	}
}
