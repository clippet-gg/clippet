package autobot

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Lane nits22 (the run 15a nit sweep's N1/N2, run 14's [autohost] the backfill's
// one-second double): each pin here fails with its fix reverted.

// sessionAuto is an Auto with a store and a held session, no Run and no
// saveLoop: the flushes are called by hand, so the order is the test's.
func sessionAuto(t *testing.T, store SessionStore, every time.Duration) *Auto {
	t.Helper()
	a := New(Config{Mode: ModeAuto, Log: quietLog(), Call: newFakeREST().Call, Sessions: store, SaveEvery: every})
	a.mu.Lock()
	a.session = sessionState{id: "sess-n", resumeURL: "wss://gw.example", seq: 7, hasSeq: true}
	a.mu.Unlock()
	return a
}

// Pin (N1): Shutdown's save and Run's last save write a session ONCE, in
// either order — the second finds the same session already saved. Before the
// fix flushFinal-then-flushLast wrote it twice (flushLast wrote whenever a
// session was held).
func TestFinalSavesWriteOnce(t *testing.T) {
	for _, order := range [][2]flushWhy{{flushFinal, flushLast}, {flushLast, flushFinal}} {
		store := &countStore{}
		a := sessionAuto(t, store, time.Hour)
		a.acted("900000000000000010")
		a.flush(context.Background(), order[0])
		a.flush(context.Background(), order[1])
		if n := store.count(); n != 1 {
			t.Fatalf("order %v: %d Saves of one unchanged session, want 1", order, n)
		}
		// A sequence number that moved after the first is still saved by the last.
		a.mu.Lock()
		a.session.seq = 8
		a.mu.Unlock()
		a.flush(context.Background(), flushLast)
		if n := store.count(); n != 2 || store.last.Seq != 8 {
			t.Fatalf("order %v: a moved seq after the final save: %d Saves, seq %d — want 2, 8", order, n, store.last.Seq)
		}
		_ = a.Shutdown(context.Background())
	}
}

// Pin (N1, whole): a Shutdown while Run is live writes the session once — not
// once by Shutdown and once more by Run's end.
func TestShutdownWritesTheSessionOnce(t *testing.T) {
	store := &memStore{}
	readied := make(chan struct{})
	var gw *gwServer
	gw = newGW(t, func(c *gwConn) {
		hello(c, 60000)
		c.next(t, opIdentify)
		ready(c, "sess-1", gw.wsURL(), 5)
		close(readied)
		<-c.ctx.Done()
	})
	_, stop := hostedAuto(t, gw, store, newFakeREST(), &fakeHost{bound: map[string]bool{}}, ModePrompt, nil)
	<-readied
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, _ := store.get(); s.ID == "sess-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("READY's session was never saved")
		}
		time.Sleep(5 * time.Millisecond) // sleep: polling the store; the save loop has no hook
	}
	_, before := store.get()
	stop()
	if _, n := store.get(); n != before {
		t.Fatalf("a stop with nothing changed since the save wrote %d more time(s), want 0", n-before)
	}
}

// Pin (the backfill's one-second double): a stop right after a video is seen
// persists `last` before Shutdown returns, even when the debounce holds the
// loop and an upload runs past Shutdown's ctx — the hosted process exits as
// Shutdown returns, and an unsaved `last` is the next start's backfill acting
// on that video again. Before the fix Shutdown returned ctx's error at the
// job wait without saving.
func TestStopRightAfterAnActPersistsLast(t *testing.T) {
	store := &countStore{}
	a := sessionAuto(t, store, time.Hour)
	a.mu.Lock()
	a.lastSave = time.Now() // the debounce holds: no act save, no tick, for an hour
	a.mu.Unlock()
	const id = "900000000000000020"
	a.acted(id)
	block := make(chan struct{})
	defer close(block)
	a.goTracked(func() { <-block }) // an upload that outlives Shutdown's ctx
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := a.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want the deadline (the job outlived it)", err)
	}
	if n := store.count(); n != 1 || store.last.Last != id {
		t.Fatalf("after a stop right after an act: %d Saves, last %q — want 1, %q", n, store.last.Last, id)
	}
}

// blockStore's Save waits for release (or its ctx); it records when each call
// started.
type blockStore struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockStore) Load(context.Context) (Session, error) { return Session{}, nil }
func (b *blockStore) Save(ctx context.Context, _ Session) error {
	b.started <- struct{}{}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pin (N2): the act's save runs OFF the server's upload queue — the next
// upload in the same server starts while the first one's Save is still
// waiting on the plane. Before the fix the queue stalled behind it (up to 5 s).
func TestActSaveDoesNotStallTheQueue(t *testing.T) {
	store := &blockStore{started: make(chan struct{}, 8), release: make(chan struct{})}
	rest := newFakeREST()
	host := &fakeHost{bound: map[string]bool{testGuild: true}, link: testLink}
	a := New(Config{Mode: ModeAuto, Log: quietLog(), Call: rest.Call, Sessions: store, SaveEvery: time.Millisecond})
	a.SetHost(host)
	a.selfID, a.appID = testSelf, testApp
	a.mu.Lock()
	a.session = sessionState{id: "sess-q", resumeURL: "wss://gw.example", seq: 1, hasSeq: true}
	a.mu.Unlock()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	}()
	gate := make(chan struct{})
	host.mu.Lock()
	host.gate = gate
	host.mu.Unlock()
	att := Attachment{URL: "https://cdn.discordapp.com/attachments/1/2/a.mp4", ContentType: "video/mp4", Size: 1000}
	a.acted("900000000000000030")
	if !a.enqueue(job{guild: testGuild, channel: testChannel, original: "900000000000000030", att: att, mode: ModeAuto}) ||
		!a.enqueue(job{guild: testGuild, channel: testChannel, original: "900000000000000031", att: att, mode: ModeAuto}) {
		t.Fatal("enqueue refused")
	}
	gate <- struct{}{} // the first upload ends; its act save starts and blocks on the plane
	select {
	case <-store.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first act's save never started")
	}
	select {
	case gate <- struct{}{}: // the second upload is running: the queue did not wait on the Save
	case <-time.After(2 * time.Second):
		t.Fatal("the server's next upload waited behind the first act's Save (the save ran inline in the queue)")
	}
	close(store.release)
}
