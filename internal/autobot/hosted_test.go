package autobot

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The hosted process's seams (§20 erratum 334): the session kept off the
// machine, the backfill window, the per-server settings, the observer. Nothing
// here reaches a Discord host.

type memStore struct {
	mu    sync.Mutex
	s     Session
	saves int
}

func (m *memStore) Load(context.Context) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s, nil
}

func (m *memStore) Save(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s = s
	m.saves++
	return nil
}

func (m *memStore) get() (Session, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s, m.saves
}

// closeRec wraps a test socket and records the code it was closed with.
type closeRec struct {
	Conn
	code *atomic.Int32
}

func (c closeRec) Close(code int, reason string) error {
	c.code.Store(int32(code))
	return c.Conn.Close(code, reason)
}

// hostedAuto runs an Auto with a SessionStore against gw; stop shuts it down
// and waits for Run.
func hostedAuto(t *testing.T, gw *gwServer, store SessionStore, rest *fakeREST, host Host, mode string, closed *atomic.Int32) (*Auto, func()) {
	t.Helper()
	base := rest.respond
	rest.respond = func(method, path string) Response {
		if path == "/gateway/bot" {
			return Response{Status: 200, Body: []byte(`{"url":"` + gw.wsURL() + `","session_start_limit":{"total":1000,"remaining":999,"reset_after":1000}}`)}
		}
		if base != nil {
			return base(method, path)
		}
		return Response{}
	}
	a := New(Config{Mode: mode, Log: quietLog(), Call: rest.Call,
		Dial: func(ctx context.Context, raw string) (Conn, error) {
			c, err := testDial(ctx, raw)
			if err != nil || closed == nil {
				return c, err
			}
			return closeRec{Conn: c, code: closed}, nil
		},
		AllowGateway: allowAll, Jitter: func() float64 { return 0.999 },
		Sessions: store, Backfill: true, SaveEvery: 10 * time.Millisecond})
	a.SetHost(host)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx, testToken) }()
	return a, func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = a.Shutdown(sctx)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return")
		}
	}
}

// firstFrame is the first frame (heartbeats aside) the client sent.
func firstFrame(t *testing.T, c *gwConn) map[string]any {
	t.Helper()
	select {
	case f := <-c.in:
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("the client sent nothing")
	}
	return nil
}

// Pin: the session is written to the store on READY and read back by the
// next process, which RESUMEs (op 6, the saved id and sequence) instead of
// IDENTIFYing; a sequence number alone writes nothing until Run ends, when the
// latest is saved; a shutdown with a store closes RESUMABLE (4000), never 1000.
func TestSessionSavedThenResumed(t *testing.T) {
	store := &memStore{}
	firstSaved := make(chan struct{})
	var gw1 *gwServer
	gw1 = newGW(t, func(c *gwConn) {
		hello(c, 60000)
		c.next(t, opIdentify)
		ready(c, "sess-9", gw1.wsURL(), 5)
		<-firstSaved
		c.send(map[string]any{"op": opDispatch, "t": "TYPING_START", "s": 12, "d": map[string]any{}})
		<-c.ctx.Done()
	})
	var closed atomic.Int32
	_, stop := hostedAuto(t, gw1, store, newFakeREST(), &fakeHost{bound: map[string]bool{}}, ModePrompt, &closed)
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, _ := store.get()
		if s.ID == "sess-9" && s.ResumeURL == gw1.wsURL() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session was not saved: %+v", s)
		}
		time.Sleep(5 * time.Millisecond) // sleep: polling the store; the save loop has no hook
	}
	close(firstSaved)
	time.Sleep(100 * time.Millisecond) // sleep: ten save periods with only a sequence number moving
	if s, n := store.get(); s.Seq != 5 || n != 1 {
		t.Fatalf("a sequence number alone was saved (%d saves, seq %d): a write per event is a KV bill", n, s.Seq)
	}
	stop()
	if s, _ := store.get(); s.Seq != 12 {
		t.Fatalf("the last save as Run ended has seq %d, want 12", s.Seq)
	}
	if got := closed.Load(); got != closeResumable {
		t.Fatalf("a shutdown with a store closed the socket %d, want %d (1000 would end the session on Discord's side)", got, closeResumable)
	}

	// The next process, the same store: its first frame is a RESUME.
	first := make(chan map[string]any, 1)
	gw2 := newGW(t, func(c *gwConn) {
		hello(c, 60000)
		first <- firstFrame(t, c)
		<-c.ctx.Done()
	})
	store.mu.Lock()
	store.s.ResumeURL = gw2.wsURL() // the resume URL READY named, now this test's second server
	store.mu.Unlock()
	_, stop2 := hostedAuto(t, gw2, store, newFakeREST(), &fakeHost{bound: map[string]bool{}}, ModePrompt, nil)
	defer stop2()
	f := <-first
	if int(f["op"].(float64)) != opResume {
		t.Fatalf("a restarted process sent op %v, want RESUME (6): %v", f["op"], f)
	}
	d := f["d"].(map[string]any)
	if d["session_id"] != "sess-9" || int(d["seq"].(float64)) != 12 {
		t.Fatalf("RESUME = %v", d)
	}
}

// guildCreate is a GUILD_CREATE with one text channel and one voice channel.
func guildCreate(guild string) map[string]any {
	return map[string]any{"id": guild, "channels": []any{
		map[string]any{"id": testChannel, "type": 0},
		map[string]any{"id": "200000000000000009", "type": 4}, // a category: never read
	}}
}

// backfillRun runs one fresh IDENTIFY whose READY is followed by gc, with
// last as the store's newest message, and answers the REST calls it makes.
func backfillRun(t *testing.T, last string, gc map[string]any) []call {
	t.Helper()
	store := &memStore{s: Session{Last: last}}
	rest := newFakeREST()
	rest.respond = func(method, path string) Response {
		if method == http.MethodGet && strings.HasPrefix(path, "/channels/"+testChannel+"/messages?") {
			// Discord's answer: newest first, no guild_id on the messages.
			msg := strings.Replace(string(videoMessage(testGuild)), `,"guild_id":"`+testGuild+`"`, "", 1)
			return Response{Status: 200, Body: []byte("[" + msg + "]")}
		}
		return Response{}
	}
	gw := newGW(t, func(c *gwConn) {
		hello(c, 60000)
		c.next(t, opIdentify)
		ready(c, "sess-b", "", 1)
		c.send(map[string]any{"op": opDispatch, "t": "GUILD_CREATE", "s": 2, "d": gc})
		<-c.ctx.Done()
	})
	_, stop := hostedAuto(t, gw, store, rest, &fakeHost{bound: map[string]bool{testGuild: true}, link: testLink}, ModePrompt, nil)
	defer stop()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range rest.snapshot() {
			if c.Method == http.MethodPost && c.Path == "/channels/"+testChannel+"/messages" {
				return rest.snapshot()
			}
		}
		time.Sleep(5 * time.Millisecond) // sleep: polling the fake REST for the backfilled offer
	}
	return rest.snapshot()
}

func afterOf(t *testing.T, calls []call) string {
	t.Helper()
	for _, c := range calls {
		if c.Method == http.MethodGet && strings.HasPrefix(c.Path, "/channels/"+testChannel+"/messages?") {
			u, err := url.Parse(c.Path)
			if err != nil {
				t.Fatal(err)
			}
			if u.Query().Get("limit") != "50" {
				t.Fatalf("backfill limit = %q, want 50", u.Query().Get("limit"))
			}
			return u.Query().Get("after")
		}
	}
	t.Fatalf("no backfill read: %+v", calls)
	return ""
}

func snowTime(t *testing.T, s string) time.Time {
	t.Helper()
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return time.UnixMilli(int64(n>>22) + discordEpochMs)
}

// Pin: after a fresh IDENTIFY a bound server's channels are read back — at
// most 50 each, AFTER the newest message seen but never more than 10 minutes
// back — and what is found goes through the live path (here: Prompt's reply).
func TestBackfillWindow(t *testing.T) {
	// The last message seen is an hour old: the window caps the read at 10 min.
	old := snowflakeAt(time.Now().Add(-time.Hour))
	calls := backfillRun(t, old, guildCreate(testGuild))
	at := snowTime(t, afterOf(t, calls))
	if d := time.Since(at); d < 9*time.Minute+50*time.Second || d > 10*time.Minute+10*time.Second {
		t.Fatalf("an hour-old last message read back %v, want the 10-minute window", d)
	}
	offered := false
	for _, c := range calls {
		if c.Method == http.MethodPost && c.Path == "/channels/"+testChannel+"/messages" && c.Body["components"] != nil {
			offered = true
		}
		if strings.Contains(c.Path, "200000000000000009") {
			t.Fatalf("a category was read: %s", c.Path)
		}
	}
	if !offered {
		t.Fatalf("the backfilled video got no reply: %+v", calls)
	}

	// The last message seen is a minute old: the read starts right after it.
	recent := snowflakeAt(time.Now().Add(-time.Minute))
	if got := afterOf(t, backfillRun(t, recent, guildCreate(testGuild))); got != recent {
		t.Fatalf("after = %s, want the last message seen %s", got, recent)
	}
}

// Pin: a server that is not bound is never read back.
func TestBackfillUnboundNotRead(t *testing.T) {
	calls := backfillRun(t, "", guildCreate(otherGuild))
	for _, c := range calls {
		if strings.HasPrefix(c.Path, "/channels/") {
			t.Fatalf("an unbound server was read: %+v", c)
		}
	}
}

// guildHost is a Host with per-server settings.
type guildHost struct {
	fakeHost
	st map[string]Settings
}

func (h *guildHost) Settings(_ context.Context, g string) (Settings, error) {
	return h.st[g], nil
}

// Pin: under ModeGuild each server's own mode, seconds and notice apply.
func TestGuildSettingsPerServer(t *testing.T) {
	rest := newFakeREST()
	timers := &fakeTimers{}
	host := &guildHost{fakeHost: fakeHost{bound: map[string]bool{testGuild: true, otherGuild: true}, link: testLink},
		st: map[string]Settings{testGuild: {Mode: ModePrompt, Seconds: 30, Notice: true}, otherGuild: {Mode: ModeAuto, Seconds: 0, Notice: false}}}
	// The box-wide notice ON: the server's own (off) must win.
	a := New(Config{Mode: ModeGuild, Notice: true, Log: quietLog(), Call: rest.Call, AfterFunc: timers.AfterFunc,
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }})
	a.SetHost(host)
	a.selfID, a.appID = testSelf, testApp
	defer func() { _ = a.Shutdown(context.Background()) }()

	a.onMessage(context.Background(), videoMessage(testGuild))
	ts := timers.all()
	if len(ts) != 1 || ts[0].d != 30*time.Second {
		t.Fatalf("Prompt's reply timer = %+v, want one of 30 s (the server's own seconds)", ts)
	}
	a.onMessage(context.Background(), videoMessage(otherGuild))
	waitIdle(t, a)
	linked := false
	for _, c := range rest.snapshot() {
		if c.Method == http.MethodPost && c.Body["content"] == testLink {
			linked = true // the bare link: this server's notice is off
		}
		if c.Body["components"] != nil && strings.Contains(c.Path, testChannel) && len(timers.all()) != 1 {
			t.Fatal("Auto posted a reply")
		}
	}
	if !linked || host.uploadCount() != 1 {
		t.Fatalf("Auto's server: linked=%v uploads=%d: %+v", linked, host.uploadCount(), rest.snapshot())
	}
	// Off in a bound server: nothing.
	host.st[testGuild] = Settings{Mode: ModeOff}
	before := len(rest.snapshot())
	a.mu.Lock()
	a.seen = map[string]time.Time{}
	a.mu.Unlock()
	a.onMessage(context.Background(), videoMessage(testGuild))
	if len(rest.snapshot()) != before {
		t.Fatalf("a server switched off was acted on: %+v", rest.snapshot()[before:])
	}
}

// recObs records what an Observer is told.
type recObs struct {
	mu        sync.Mutex
	kinds     []string
	decisions []Decision
	limited   int
}

func (o *recObs) Gateway(e GatewayEvent) {
	o.mu.Lock()
	o.kinds = append(o.kinds, e.Kind)
	o.mu.Unlock()
}
func (o *recObs) Decide(d Decision) {
	o.mu.Lock()
	o.decisions = append(o.decisions, d)
	o.mu.Unlock()
}
func (o *recObs) RateLimited()   { o.mu.Lock(); o.limited++; o.mu.Unlock() }
func (o *recObs) QueueDepth(int) {}

// Pin: the observer hears a link's decision with the ids and its duration,
// and a 429 from Discord's REST.
func TestObserverDecisionsAndLimits(t *testing.T) {
	a, rest, _, _ := newModeAuto(t, ModeAuto, true)
	o := &recObs{}
	a.obs = o
	var n atomic.Int32
	rest.respond = func(method, path string) Response {
		if method == http.MethodDelete && n.Add(1) == 1 {
			return Response{Status: 429, Body: []byte(`{"retry_after":0.001}`)}
		}
		return Response{}
	}
	a.onMessage(context.Background(), videoMessage(testGuild))
	waitIdle(t, a)
	o.mu.Lock()
	defer o.mu.Unlock()
	var link *Decision
	for i := range o.decisions {
		if o.decisions[i].Event == "link" {
			link = &o.decisions[i]
		}
	}
	if link == nil || link.Guild != testGuild || link.Channel != testChannel || link.Message != testMessage || link.Outcome != "linked" || link.Mode != ModeAuto {
		t.Fatalf("decisions = %+v", o.decisions)
	}
	if o.limited != 1 {
		t.Fatalf("429s seen = %d, want 1", o.limited)
	}
}
