package autobot

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The 2026-10-12 amendment of §20 erratum 334 (lane `autofix`, the review's audit
// of the live hosted process): each pin here fails with its fix reverted.

// countStore is a SessionStore whose Save answers err and counts the calls.
type countStore struct {
	mu    sync.Mutex
	err   error
	saves int
	last  Session
}

func (c *countStore) Load(context.Context) (Session, error) { return Session{}, nil }
func (c *countStore) Save(_ context.Context, s Session) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saves++
	c.last = s
	return c.err
}
func (c *countStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saves
}
func (c *countStore) setErr(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}

// saveAuto is an Auto with a store and a dirty session, its saveLoop running;
// stop ends the loop (the last save included) and waits for it.
func saveAuto(t *testing.T, store SessionStore, every time.Duration) (*Auto, func()) {
	t.Helper()
	a := New(Config{Mode: ModeGuild, Log: quietLog(), Call: newFakeREST().Call, Sessions: store, SaveEvery: every})
	a.mu.Lock()
	a.session = sessionState{id: "sess-f", resumeURL: "wss://gw.example", seq: 3, hasSeq: true}
	a.dirty = true
	a.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.saveLoop(ctx) }()
	return a, func() {
		cancel()
		<-done
		_ = a.Shutdown(context.Background())
	}
}

// Pin (audit F1): a failing store is NOT asked every tick — the tries back off
// (SaveEvery, doubling), so 300 ms of 2-ms ticks make a handful of Saves, not
// ~150; and a success resets it, so the next change is saved within a tick.
func TestSaveBacksOffOnFailure(t *testing.T) {
	store := &countStore{err: errors.New("plane down")}
	a, stop := saveAuto(t, store, 2*time.Millisecond)
	defer stop()
	time.Sleep(300 * time.Millisecond) // sleep: 150 save ticks with the store failing
	if n := store.count(); n < 3 || n > 12 {
		t.Fatalf("%d Saves in 300 ms of 2-ms ticks with the store failing, want a backed-off handful (≈8)", n)
	}
	a.mu.Lock()
	fails := a.saveFails
	a.mu.Unlock()
	if fails < 3 {
		t.Fatalf("saveFails = %d", fails)
	}
	store.setErr(nil)
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		fails, dirty := a.saveFails, a.dirty
		a.mu.Unlock()
		if fails == 0 && !dirty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the store came back and the session was never saved")
		}
		time.Sleep(2 * time.Millisecond) // sleep: polling the backoff's state; the save loop has no hook
	}
	before := store.count()
	a.acted("900000000000000001")
	time.Sleep(30 * time.Millisecond) // sleep: fifteen ticks after a change
	if store.count() != before+1 {
		t.Fatalf("after a success the next change made %d Saves, want 1 (the backoff reset)", store.count()-before)
	}
}

// Pin (audit F1): a REFUSED session (a 4xx other than 429) is asked once, then
// not again — not even by the last save — until the session changes.
func TestSaveRefusedWaitsForAChange(t *testing.T) {
	store := &countStore{err: ErrSessionRefused}
	a, stop := saveAuto(t, store, 2*time.Millisecond)
	time.Sleep(100 * time.Millisecond) // sleep: fifty save ticks with the store refusing
	if n := store.count(); n != 1 {
		t.Fatalf("%d Saves of a refused session, want 1", n)
	}
	a.acted("900000000000000002")     // a change
	time.Sleep(50 * time.Millisecond) // sleep: twenty-five ticks after the change
	if n := store.count(); n != 2 {
		t.Fatalf("%d Saves after one change, want 2", n)
	}
	stop()
	if n := store.count(); n != 2 {
		t.Fatalf("the last save asked again for a refused, unchanged session (%d Saves)", n)
	}
}

// Pin (audit F2): READY's bot user and application are saved WITH the session,
// and a process that starts by RESUME — and so never sees a READY — has them:
// a press of a reply this process does not hold is the stale-button delete,
// not "acknowledged and dropped".
func TestResumeKeepsIdentity(t *testing.T) {
	store := &memStore{}
	saved := make(chan struct{})
	var gw1 *gwServer
	gw1 = newGW(t, func(c *gwConn) {
		hello(c, 60000)
		c.next(t, opIdentify)
		ready(c, "sess-i", gw1.wsURL(), 4)
		<-saved
		<-c.ctx.Done()
	})
	_, stop1 := hostedAuto(t, gw1, store, newFakeREST(), &fakeHost{bound: map[string]bool{}}, ModePrompt, nil)
	waitSaved(t, store, func(s Session) bool { return s.ID == "sess-i" })
	close(saved)
	stop1()
	if s, _ := store.get(); s.UserID != testSelf || s.AppID != testApp {
		t.Fatalf("the saved session carries user %q app %q, want READY's", s.UserID, s.AppID)
	}

	rest := newFakeREST()
	a := resumeRun(t, store, rest)
	if self, app := a.identity(); self != testSelf || app != testApp {
		t.Fatalf("after a RESUME the identity is (%q, %q)", self, app)
	}
	for _, c := range rest.snapshot() {
		if c.Path == "/users/@me" || c.Path == "/applications/@me" {
			t.Fatalf("asked %s although the session carried the identity", c.Path)
		}
	}
	if ans, start := a.Press(context.Background(), pressBody(testGuild, testPoster, "0")); ans == nil || start == nil {
		t.Fatalf("a press after a RESUME: answer %v start %v, want the stale reply's delete", ans, start != nil)
	}
}

// Pin (audit F2, the fallback): a saved session with no identity in it (one
// written before the amendment) — the bot user and the application are asked
// ONCE each through the REST manners before the RESUME.
func TestResumeFetchesMissingIdentity(t *testing.T) {
	store := &memStore{}
	rest := newFakeREST()
	rest.respond = func(method, path string) Response {
		switch path {
		case "/users/@me":
			return Response{Status: 200, Body: []byte(`{"id":"` + testSelf + `","bot":true}`)}
		case "/applications/@me":
			return Response{Status: 200, Body: []byte(`{"id":"` + testApp + `"}`)}
		}
		return Response{}
	}
	a := resumeRun(t, store, rest)
	if self, app := a.identity(); self != testSelf || app != testApp {
		t.Fatalf("after a RESUME with no saved identity: (%q, %q)", self, app)
	}
	n := map[string]int{}
	for _, c := range rest.snapshot() {
		n[c.Path]++
	}
	if n["/users/@me"] != 1 || n["/applications/@me"] != 1 {
		t.Fatalf("identity calls %v, want one each", n)
	}
}

func waitSaved(t *testing.T, store *memStore, ok func(Session) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, _ := store.get(); ok(s) {
			return
		}
		if time.Now().After(deadline) {
			s, _ := store.get()
			t.Fatalf("not saved: %+v", s)
		}
		time.Sleep(5 * time.Millisecond) // sleep: polling the store; the save loop has no hook
	}
}

// resumeRun starts a hosted Auto on store's session (its resume URL pointed at
// a test gateway that answers RESUMED), waits for the RESUME and answers the
// Auto. The bound guild is testGuild.
func resumeRun(t *testing.T, store *memStore, rest *fakeREST) *Auto {
	t.Helper()
	resumed := make(chan struct{})
	gw := newGW(t, func(c *gwConn) {
		hello(c, 60000)
		c.next(t, opResume)
		c.send(map[string]any{"op": opDispatch, "t": "RESUMED", "s": 9, "d": map[string]any{}})
		close(resumed)
		<-c.ctx.Done()
	})
	store.mu.Lock()
	if store.s.ID == "" {
		store.s = Session{ID: "sess-r", Seq: 8}
	}
	store.s.ResumeURL = gw.wsURL()
	store.mu.Unlock()
	a, stop := hostedAuto(t, gw, store, rest, &fakeHost{bound: map[string]bool{testGuild: true}, link: testLink}, ModePrompt, nil)
	t.Cleanup(stop)
	select {
	case <-resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("no RESUME")
	}
	return a
}

// Pin (audit F5): the per-channel route keys do not grow without end — once
// past pruneAbove, a learn sweeps out every bucket whose reset has passed and
// every route left pointing at nothing.
func TestRestRoutesPruned(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC)}
	r := &rest{now: clock.Now, buckets: map[string]*bucket{}, routes: map[string]string{}}
	h := func(hash string) http.Header {
		return http.Header{"X-Ratelimit-Bucket": {hash}, "X-Ratelimit-Remaining": {"4"}, "X-Ratelimit-Reset-After": {"1"}}
	}
	for i := 0; i < 1000; i++ {
		ch := strconv.Itoa(200000000000000000 + i)
		r.learn("POST /channels/"+ch+"/messages", h("b"+ch))
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(2 * time.Minute)
	clock.mu.Unlock()
	r.learn("POST /channels/299999999999999999/messages", h("bnew"))
	if len(r.routes) > 2 || len(r.buckets) > 2 {
		t.Fatalf("after the resets passed: %d routes, %d buckets, want the new one alone", len(r.routes), len(r.buckets))
	}
}

// Pin (audit F7): an id or token that could bend a REST path is refused before
// any call; Discord's own shapes pass.
func TestRestPathPartsChecked(t *testing.T) {
	a, rest, _, _ := newModeAuto(t, ModeAuto, false)
	ctx := context.Background()
	bad := []func() (Response, error){
		func() (Response, error) { return a.createMessage(ctx, "1/../../guilds/2", nil) },
		func() (Response, error) { return a.deleteMessage(ctx, testChannel, "3?x=1") },
		func() (Response, error) { return a.callback(ctx, "800000000000000001", "a/b", nil) },
		func() (Response, error) { return a.followup(ctx, testApp, "tok#frag", nil) },
		func() (Response, error) { return a.followup(ctx, "app", "itok", nil) },
		func() (Response, error) { return a.channelMessages(ctx, testChannel, "1 2", 50) },
	}
	for i, f := range bad {
		if _, err := f(); err == nil {
			t.Fatalf("case %d: a malformed path part was accepted", i)
		}
	}
	if c := rest.snapshot(); len(c) != 0 {
		t.Fatalf("malformed parts reached the REST: %+v", c)
	}
	if _, err := a.callback(ctx, "800000000000000001", "aW50ZXJhY3Rpb246OjEyMzQ.abc_DEF-9", nil); err != nil {
		t.Fatalf("a token of Discord's shape was refused: %v", err)
	}
}

// panicHost panics on its first Upload and links after.
type panicHost struct {
	fakeHost
	n atomic.Int32
}

func (h *panicHost) Upload(ctx context.Context, g string, at Attachment) (string, error) {
	if h.n.Add(1) == 1 {
		panic("boom")
	}
	return h.fakeHost.Upload(ctx, g, at)
}

// Pin (audit F8): a panic inside one upload ends that job, not the process,
// and the server's queue goes on.
func TestDrainSurvivesAPanic(t *testing.T) {
	a, rest, _, _ := newModeAuto(t, ModeAuto, false)
	h := &panicHost{fakeHost: fakeHost{bound: map[string]bool{testGuild: true}, link: testLink}}
	a.SetHost(h)
	at := Attachment{URL: "https://cdn.discordapp.com/attachments/1/2/a.mp4", ContentType: "video/mp4"}
	for _, id := range []string{"300000000000000011", "300000000000000012"} {
		if !a.enqueue(job{guild: testGuild, channel: testChannel, original: id, att: at, mode: ModeAuto}) {
			t.Fatal("enqueue refused")
		}
	}
	waitIdle(t, a)
	linked := false
	for _, c := range rest.snapshot() {
		if c.Method == http.MethodPost && c.Body["content"] == testLink {
			linked = true
		}
	}
	if !linked || h.n.Load() != 2 {
		t.Fatalf("after a panicking job: uploads=%d linked=%v", h.n.Load(), linked)
	}
}
