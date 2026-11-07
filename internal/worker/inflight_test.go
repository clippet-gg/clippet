package worker

import (
	"context"
	"strings"
	"sync"
	"testing"

	"clippet/internal/store"
)

// TestCancelledRunRemovesItsUploadWhateverTheListingSays is the pin for §20
// erratum 305 — the fifteenth gate's "expire-while-in-flight" failure, where a
// destroyed clip left ONE object behind: the rendition its cancelled run had
// just uploaded.
//
// The interleaving, in the order this test replays it:
//
//  1. the run uploads the rendition; the bucket has the object and its LISTING
//     does not show it yet (Fake.Unlisted — R2's listing is an eventually
//     consistent index, the bundled MinIO's is not, which is why no E2E run
//     ever saw this);
//  2. the destroy lands while the NEXT upload is starting: MarkDeleting
//     tombstones the clip and cancels its job, Worker.Cancel cancels the run;
//  3. the run's exit path removes what this attempt wrote — and used to remove
//     it with DeletePrefix(key), which asks the listing, so the object stayed
//     and the INFO line said the outputs were discarded;
//  4. lifecycle.Destroy then deletes the prefix — by listing, so it misses the
//     object too — and deletes the row over the top of it.
//
// The assertion is the invariant the erratum states: when Cancel returns, the
// RUN has removed the object, whatever any listing says.
func TestCancelledRunRemovesItsUploadWhateverTheListingSays(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)

	var mu sync.Mutex
	var rendition string
	atPoster := make(chan struct{})
	var once sync.Once
	h.store.Hook = func(ctx context.Context, op, key string) error {
		if op != "PutFile" {
			return nil
		}
		switch {
		case renditionRE.MatchString(key):
			// The object lands; the listing is behind. Unlisted is sticky, so
			// setting it before the write is the same statement as after it.
			mu.Lock()
			rendition = key
			mu.Unlock()
			h.store.Unlisted(key)
		case posterRE.MatchString(key):
			// The rendition is in the bucket and nothing after it is: this is
			// the instant the reference stack was destroyed at. Hold the run
			// here until Cancel below cuts the transfer, which is what an
			// aborted PutFile returns.
			once.Do(func() { close(atPoster) })
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	h.start()
	<-atPoster

	// lifecycle.Destroy steps 1 and 2, in its order and with its arguments.
	if err := h.db.MarkDeleting(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatalf("MarkDeleting: %v", err)
	}
	if !h.w.Cancel(context.Background(), c.ID) {
		t.Fatalf("Cancel returned false; the run did not exit:\n%s", h.logs.String())
	}

	mu.Lock()
	key := rendition
	mu.Unlock()
	if key == "" {
		t.Fatalf("the run never uploaded a rendition:\n%s", h.logs.String())
	}
	if _, ok := h.store.Get(key); ok {
		t.Fatalf("Cancel returned with %s still in the bucket: the run must remove what it wrote, "+
			"by key, before its goroutine exits (§20 erratum 305)\n%s", key, h.logs.String())
	}

	// Step 3 of the destroy, with the listing still behind: it must find
	// nothing of this attempt left to miss.
	prefix := store.ClipPrefix("", c.ID)
	if _, err := h.store.DeletePrefix(context.Background(), prefix); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	var left []string
	for _, k := range h.store.Keys() {
		if strings.HasPrefix(k, prefix) {
			left = append(left, k)
		}
	}
	if len(left) != 0 {
		t.Fatalf("the clip's prefix still holds %v after the destroy", left)
	}
	h.assertLog("INFO", "outputs discarded")
	h.assertNoErrorLogs()
}
