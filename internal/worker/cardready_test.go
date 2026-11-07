package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/transcode"
)

// The card-ready hook (§20 erratum 330): the worker tells Deps.CardReady when a
// clip's card has just become ready — at the ready flip, when the Light copy's
// job settles, after a repair that settles it — and never for a clip whose card
// is not ready. Without a hook nothing is read.

// cardHook records the ids the hook was called with.
type cardHook struct {
	mu  sync.Mutex
	ids []string
}

func (c *cardHook) call(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, id)
}

func (c *cardHook) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ids...)
}

// wait re-reads the calls at every job settle: the card-ready call is made
// before the job that made it settles (runJob, repairOnce).
func (c *cardHook) wait(t *testing.T, h *harness, id string) {
	t.Helper()
	h.until("the card-ready hook called for "+id, func() bool {
		for _, got := range c.calls() {
			if got == id {
				return true
			}
		}
		return false
	})
}

func withCardHook(h *harness) *cardHook {
	c := &cardHook{}
	h.w.deps.CardReady = c.call
	return c
}

// TestCardReadyHookAtTheReadyFlip: an upload whose card works at the flip —
// the common case, and the audiogram's (its card copy is part of the run) —
// calls the hook with the clip's id once the transcode is done.
func TestCardReadyHookAtTheReadyFlip(t *testing.T) {
	h := newHarness(t)
	hook := withCardHook(h)
	c, _ := h.newClip("mkv", 1024, true)
	h.start()
	ready := h.waitClip(c.ID, db.ClipReady)
	hook.wait(t, h, c.ID)
	h.waitIdle()
	if !db.CardReady(h.clip(c.ID)) {
		t.Fatalf("premise: the clip is not card-ready: %+v", ready)
	}
	h.assertNoErrorLogs()
}

// TestCardReadyHookWhenTheLightCopySettles: a ready clip whose card copy is
// still promised (`light_state = encoding`, erratum 235) is NOT card-ready;
// the `social` job's ending — here its skip, erratum 237 — makes it so, and
// that ending calls the hook. Nothing before it does.
func TestCardReadyHookWhenTheLightCopySettles(t *testing.T) {
	ctx := context.Background()
	h := newHarnessCfg(t, func(c *config.Config) { c.SocialMaxBytes = 0 })
	hook := withCardHook(h)
	c := h.gifReady()
	if err := h.db.SetTierState(ctx, c.ID, db.TierNameLight, db.TierEncoding, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if db.CardReady(h.clip(c.ID)) {
		t.Fatal("premise: a promised card copy means the card is not ready")
	}
	if _, err := h.db.EnqueueSocial(ctx, c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.waitSocial(c.ID, jobDone)
	hook.wait(t, h, c.ID)
	h.waitIdle()
	if got := hook.calls(); len(got) != 1 {
		t.Fatalf("hook calls %v, want the social job's one", got)
	}
}

// TestCardReadyHookNeverForACardThatIsNotReady: a clip that FAILED has no card
// to redraw, and its run's ending does not call the hook.
func TestCardReadyHookNeverForACardThatIsNotReady(t *testing.T) {
	p := defaultProbeResult()
	p.CodedW, p.CodedH, p.DisplayW, p.DisplayH = 1920, 1080, 1920, 1080
	h := tierHarness(t, p)
	hook := withCardHook(h)
	h.runner.tiersFn = func(context.Context, transcode.Plan, transcode.Plan, string, string, string) error {
		return &transcode.ErrPermanent{Err: errFullBoom}
	}
	c, _ := h.newClip("mkv", 4096, true)
	h.start()
	h.waitClip(c.ID, db.ClipFailed)
	h.waitIdle()
	if got := hook.calls(); len(got) != 0 {
		t.Fatalf("the hook was called for a failed clip: %v", got)
	}
}

// TestCardReadyHookAfterARepairSettlesTheCardCopy: a `social` job left running
// with no worker is repaired by the automatic repair; once the repair fails it
// at its cap, `light_state` is settled (erratum 236) and the card is ready —
// and the repair calls the hook.
func TestCardReadyHookAfterARepairSettlesTheCardCopy(t *testing.T) {
	ctx := context.Background()
	h := socialByHandHarness(t)
	hook := withCardHook(h)
	c := h.gifReady()
	if err := h.db.SetTierState(ctx, c.ID, db.TierNameLight, db.TierEncoding, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.EnqueueSocial(ctx, c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10 && len(hook.calls()) == 0; i++ {
		h.clock.Advance(time.Hour) // past any backoff the last repair scheduled
		h.claimKind(db.KindSocial)
		h.clock.Advance(2 * time.Hour)
		if n := h.w.repairOnce(ctx, h.repairLog(), time.Hour); n != 1 {
			t.Fatalf("round %d: repaired %d\nlogs:\n%s", i, n, h.logs.String())
		}
		if !db.CardReady(h.clip(c.ID)) && len(hook.calls()) != 0 {
			t.Fatalf("round %d: the hook was called while the card is not ready", i)
		}
	}
	if got := hook.calls(); len(got) != 1 || got[0] != c.ID || !db.CardReady(h.clip(c.ID)) {
		t.Fatalf("hook calls %v, card ready %v; want one call once the repair settled the copy", got, db.CardReady(h.clip(c.ID)))
	}
}

// TestCardReadyHookWithoutABotReadsNothing: no hook (no bot) is a no-op — not
// even the re-read: this worker has no database, and a read would panic.
func TestCardReadyHookWithoutABotReadsNothing(t *testing.T) {
	w := newWorker(Deps{})
	w.cardReady(context.Background(), "aZ3kQ9xB2mLp")
}
