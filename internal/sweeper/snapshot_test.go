package sweeper

import (
	"context"
	"testing"
	"time"

	"clippet/internal/httpx"
	"clippet/internal/store"
)

// TestSweepLeavesTheReservedPrefix pins §20 erratum 343: the database snapshot
// under `_clippet/` is never a clip's, so a tick that destroys every expired
// clip — and the Destroyed hook it fires per row, the snapshot's trigger — leaves
// all three of its objects where they are.
func TestSweepLeavesTheReservedPrefix(t *testing.T) {
	fx := newFixture(t)
	reserved := []string{store.SnapshotKey(""), store.SnapshotSumKey(""), store.SnapshotMetaKey("")}
	for _, k := range reserved {
		fx.fake.Put(k, []byte("instance"), "")
	}
	a, b := fx.ready(), fx.failed()
	fx.expire(a, time.Second)
	fx.expire(b, time.Second)

	s := fx.sweeper(fx.cfg())
	var destroyed []string
	s.deps.Destroyed = func(id string) { destroyed = append(destroyed, id) }
	if st := s.RunOnce(context.Background()); st.Destroyed != 2 {
		t.Fatalf("Stats = %+v, want two destroyed", st)
	}
	for _, k := range reserved {
		if _, ok := fx.fake.Get(k); !ok {
			t.Fatalf("the sweep removed %s", k)
		}
	}
	if len(destroyed) != 2 {
		t.Fatalf("the Destroyed hook fired %d times for two destroyed rows: %v", len(destroyed), destroyed)
	}
}

// TestBudgetCountsTheExtraBytes: the storage reading is the clips' bytes plus
// WithExtraBytes' answer — the snapshot, counted like any stored byte.
func TestBudgetCountsTheExtraBytes(t *testing.T) {
	fx := newFixture(t)
	budget := &httpx.StorageBudget{}
	s := fx.sweeper(fx.cfg()).WithBudget(budget)
	s.checkBudget(context.Background())
	base := budget.Get().Used
	s = s.WithExtraBytes(func() int64 { return 1234 })
	s.checkBudget(context.Background())
	if got := budget.Get().Used; got != base+1234 {
		t.Fatalf("the reading is %d, want %d (the clips' %d + the snapshot's 1234)", got, base+1234, base)
	}
}
