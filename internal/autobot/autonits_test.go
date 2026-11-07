package autobot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Lane `autonits15` (the run 14 nit sweep): each pin here fails with its
// change reverted.

func msgWithID(guild, id string) json.RawMessage {
	return json.RawMessage(strings.Replace(string(videoMessage(guild)), `"id":"`+testMessage+`"`, `"id":"`+id+`"`, 1))
}

// guildAuto is a ModeGuild Auto (no gateway) over host, with store when given.
func guildAuto(t *testing.T, host *guildHost, store SessionStore, every time.Duration) (*Auto, *fakeREST, *fakeTimers) {
	t.Helper()
	rest := newFakeREST()
	timers := &fakeTimers{}
	a := New(Config{Mode: ModeGuild, Log: quietLog(), Call: rest.Call, AfterFunc: timers.AfterFunc,
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }, Sessions: store, SaveEvery: every})
	a.SetHost(host)
	a.selfID, a.appID = testSelf, testApp
	t.Cleanup(func() { _ = a.Shutdown(context.Background()) })
	return a, rest, timers
}

func oneGuild(st Settings) *guildHost {
	return &guildHost{fakeHost: fakeHost{bound: map[string]bool{testGuild: true}, link: testLink},
		st: map[string]Settings{testGuild: st}}
}

// Pin (row 2): an acted message's `last` is saved as its act completes — not
// a debounce later — so a process that dies a moment after does not act on it
// again from the backfill; bounded: a second act inside SaveEvery saves nothing
// more. Auto (the job's end) and Prompt (the reply posted) both.
func TestActSavesLastAtOnce(t *testing.T) {
	for _, mode := range []string{ModeAuto, ModePrompt} {
		store := &countStore{}
		a, _, _ := guildAuto(t, oneGuild(Settings{Mode: mode}), store, time.Hour)
		a.onMessage(context.Background(), msgWithID(testGuild, "300000000000000041"))
		waitIdle(t, a)
		if n := store.count(); n != 1 {
			t.Fatalf("%s: %d Saves after one act with the loop an hour away, want 1", mode, n)
		}
		store.mu.Lock()
		last := store.last.Last
		store.mu.Unlock()
		if last != "300000000000000041" {
			t.Fatalf("%s: the saved last is %q", mode, last)
		}
		a.onMessage(context.Background(), msgWithID(testGuild, "300000000000000042"))
		waitIdle(t, a)
		if n := store.count(); n != 1 {
			t.Fatalf("%s: a second act inside SaveEvery made %d Saves, want still 1", mode, n)
		}
	}
}

// Pin (row 2): Shutdown saves a session changed since the last Save, once.
func TestShutdownSavesWhatChanged(t *testing.T) {
	store := &countStore{}
	a, _, _ := guildAuto(t, oneGuild(Settings{Mode: ModeAuto}), store, time.Hour)
	a.acted("300000000000000051")
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	n, last := store.saves, store.last.Last
	store.mu.Unlock()
	if n != 1 || last != "300000000000000051" {
		t.Fatalf("Shutdown saved %d times, last %q", n, last)
	}
	_ = a.Shutdown(context.Background())
	if store.count() != 1 {
		t.Fatal("a second Shutdown saved an unchanged session")
	}
}

// slowStore's Save takes took, fails, and records when each began and ended.
type slowStore struct {
	took       time.Duration
	mu         sync.Mutex
	starts     []time.Time
	ends       []time.Time
	sessionNil Session
}

func (s *slowStore) Load(context.Context) (Session, error) { return s.sessionNil, nil }
func (s *slowStore) Save(context.Context, Session) error {
	s.mu.Lock()
	s.starts = append(s.starts, time.Now())
	s.mu.Unlock()
	time.Sleep(s.took) // sleep: a plane that answers slowly
	s.mu.Lock()
	s.ends = append(s.ends, time.Now())
	s.mu.Unlock()
	return errors.New("plane timed out")
}

// Pin (row 5, R1): the backoff counts from the moment the failed Save
// RETURNED — a plane that times out slower than the backoff does not get its
// retries back-to-back. With 10 ms ticks and 40 ms Saves, the third failure's
// 40 ms wait separates its end from the fourth try's start.
func TestSaveBackoffCountsFromTheAnswer(t *testing.T) {
	store := &slowStore{took: 40 * time.Millisecond}
	_, stop := saveAuto(t, store, 10*time.Millisecond)
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		store.mu.Lock()
		n := len(store.starts)
		store.mu.Unlock()
		if n >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d Saves", n)
		}
		time.Sleep(5 * time.Millisecond) // sleep: polling the store; the save loop has no hook
	}
	store.mu.Lock()
	gap := store.starts[3].Sub(store.ends[2])
	store.mu.Unlock()
	if gap < 38*time.Millisecond {
		t.Fatalf("the fourth try began %v after the third failure returned, want the 40 ms backoff", gap)
	}
}

// Pin (row 3): a server switched Prompt → Auto while a reply waits: when its
// timer fires the reply goes and the video is uploaded as Auto would, with the
// notice as it is now.
func TestReplyTimerFollowsASwitchToAuto(t *testing.T) {
	host := oneGuild(Settings{Mode: ModePrompt, Seconds: 30})
	a, rest, timers := guildAuto(t, host, nil, 0)
	a.onMessage(context.Background(), videoMessage(testGuild))
	ts := timers.all()
	if len(ts) != 1 {
		t.Fatalf("timers = %d", len(ts))
	}
	host.st[testGuild] = Settings{Mode: ModeAuto, Notice: true}
	ts[0].fire()
	deadline := time.Now().Add(5 * time.Second)
	for host.uploadCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond) // sleep: polling the upload the fired timer queues
	}
	waitIdle(t, a)
	var replyGone, linked, originalGone bool
	for _, c := range rest.snapshot() {
		switch {
		case c.Method == http.MethodDelete && strings.HasSuffix(c.Path, "/"+testReply):
			replyGone = true
		case c.Method == http.MethodPost && c.Body["content"] == testLink+"\n-# "+LineNotice:
			linked = true
		case c.Method == http.MethodDelete && strings.HasSuffix(c.Path, "/"+testMessage):
			originalGone = true
		}
	}
	if !replyGone || !linked || !originalGone || host.uploadCount() != 1 {
		t.Fatalf("reply gone %v, linked with the notice %v, original gone %v, uploads %d: %+v",
			replyGone, linked, originalGone, host.uploadCount(), rest.snapshot())
	}
}

// Pin (row 3): a reply whose server lengthened its seconds waits out the rest
// of the new lifetime instead of going at the old one.
func TestReplyTimerFollowsNewSeconds(t *testing.T) {
	host := oneGuild(Settings{Mode: ModePrompt, Seconds: 10})
	a, rest, timers := guildAuto(t, host, nil, 0)
	a.onMessage(context.Background(), videoMessage(testGuild))
	host.st[testGuild] = Settings{Mode: ModePrompt, Seconds: 60}
	timers.all()[0].fire()
	deadline := time.Now().Add(5 * time.Second)
	for len(timers.all()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond) // sleep: polling the re-armed timer
	}
	ts := timers.all()
	if len(ts) != 2 || ts[1].d <= 10*time.Second || ts[1].d > 60*time.Second {
		t.Fatalf("timers after the fire: %+v, want a second one for the rest of 60 s", ts)
	}
	for _, c := range rest.snapshot() {
		if c.Method == http.MethodDelete {
			t.Fatalf("the reply was removed at the old lifetime: %+v", c)
		}
	}

}

// Pin (row 3): a press reads the server's settings at the press — the notice
// as it is now; and a server switched off since removes the reply, uploading
// nothing.
func TestPressFollowsTheSwitch(t *testing.T) {
	host := oneGuild(Settings{Mode: ModePrompt, Notice: false})
	a, rest, _ := guildAuto(t, host, nil, 0)
	a.onMessage(context.Background(), videoMessage(testGuild))
	host.st[testGuild] = Settings{Mode: ModePrompt, Notice: true}
	ans, start := a.Press(context.Background(), pressBody(testGuild, testPoster, "0"))
	if ans == nil || start == nil {
		t.Fatalf("press: %v", ans)
	}
	start()
	waitIdle(t, a)
	linked := false
	for _, c := range rest.snapshot() {
		if c.Method == http.MethodPost && c.Body["content"] == testLink+"\n-# "+LineNotice {
			linked = true
		}
	}
	if !linked {
		t.Fatalf("the link was posted without the notice the server has now: %+v", rest.snapshot())
	}

	host2 := oneGuild(Settings{Mode: ModePrompt})
	b, rest2, _ := guildAuto(t, host2, nil, 0)
	b.onMessage(context.Background(), videoMessage(testGuild))
	host2.st[testGuild] = Settings{Mode: ModeOff}
	ans, start = b.Press(context.Background(), pressBody(testGuild, testPoster, "0"))
	if ans == nil || ans["type"] != callbackDeferUpdate || start == nil {
		t.Fatalf("a press in a server switched off: %v", ans)
	}
	start()
	deadline := time.Now().Add(5 * time.Second)
	for {
		gone := false
		for _, c := range rest2.snapshot() {
			if c.Method == http.MethodDelete && strings.HasSuffix(c.Path, "/"+testReply) {
				gone = true
			}
		}
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reply stayed: %+v", rest2.snapshot())
		}
		time.Sleep(5 * time.Millisecond) // sleep: polling the delete the press starts
	}
	if host2.uploadCount() != 0 {
		t.Fatal("a press in a server switched off uploaded")
	}
}

// beatObs counts heartbeats sent.
type beatObs struct {
	nopObserver
	beats atomic.Int32
}

func (o *beatObs) Gateway(e GatewayEvent) {
	if e.Kind == "beat" {
		o.beats.Add(1)
	}
}

// Pin (row 4): HELLO's heartbeat_interval is floored at 1 s — a 1 ms interval
// does not beat at 1 kHz.
func TestHelloIntervalFloored(t *testing.T) {
	readied := make(chan struct{})
	var gw *gwServer
	gw = newGW(t, func(c *gwConn) {
		hello(c, 1)
		c.next(t, opIdentify)
		ready(c, "sess-h", gw.wsURL(), 1)
		close(readied)
		<-c.ctx.Done()
	})
	obs := &beatObs{}
	rest := newFakeREST()
	rest.respond = func(method, path string) Response {
		if path == "/gateway/bot" {
			return Response{Status: 200, Body: []byte(`{"url":"` + gw.wsURL() + `","session_start_limit":{"total":1000,"remaining":999,"reset_after":1000}}`)}
		}
		return Response{}
	}
	a := New(Config{Mode: ModePrompt, Log: quietLog(), Call: rest.Call, Dial: testDial, AllowGateway: allowAll,
		Jitter: func() float64 { return 0 }, Observer: obs})
	a.SetHost(&fakeHost{bound: map[string]bool{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx, testToken) }()
	defer func() { cancel(); _ = a.Shutdown(context.Background()); <-done }()
	<-readied
	time.Sleep(300 * time.Millisecond) // sleep: 300 beats' worth at the interval HELLO asked for
	if n := obs.beats.Load(); n > 2 {
		t.Fatalf("%d heartbeats in 300 ms after HELLO asked for 1 ms, want the 1 s floor", n)
	}
}

// Pin (row 4): a REST 401 on the bot token ends the socket at once, not at the
// run loop's next turn (a minute of heartbeats later).
func TestRest401EndsTheSocketAtOnce(t *testing.T) {
	closed := make(chan struct{})
	var gw *gwServer
	gw = newGW(t, func(c *gwConn) {
		hello(c, 60000)
		c.next(t, opIdentify)
		ready(c, "sess-u", gw.wsURL(), 1)
		var m map[string]any
		_ = json.Unmarshal(videoMessage(testGuild), &m)
		c.send(map[string]any{"op": opDispatch, "t": "MESSAGE_CREATE", "s": 2, "d": m})
		for range c.in { // until the client closes
		}
		close(closed)
	})
	rest := newFakeREST()
	rest.respond = func(method, path string) Response {
		if method == http.MethodPost && strings.HasSuffix(path, "/messages") {
			return Response{Status: 401}
		}
		return Response{}
	}
	a, stop := newGatewayAuto(t, gw, nil, rest)
	defer stop()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatalf("the socket stayed open after a 401 (halted %q)", a.Halted())
	}
	if a.Halted() != "rest_401" {
		t.Fatalf("halted = %q", a.Halted())
	}
}

// slowCloseConn's Close takes took and records its code once it returned.
type slowCloseConn struct {
	Conn
	took time.Duration
	code *atomic.Int32
}

func (c slowCloseConn) Close(code int, reason string) error {
	time.Sleep(c.took) // sleep: a close handshake that takes its time
	err := c.Conn.Close(code, reason)
	c.code.Store(int32(code))
	return err
}

// shutdownRun runs a box Auto (no store) whose socket's Close takes took, and
// answers it once READY came.
func shutdownRun(t *testing.T, took time.Duration, code *atomic.Int32) (*Auto, chan struct{}) {
	t.Helper()
	readied := make(chan struct{})
	var gw *gwServer
	gw = newGW(t, func(c *gwConn) {
		hello(c, 60000)
		c.next(t, opIdentify)
		ready(c, "sess-s", gw.wsURL(), 1)
		close(readied)
		<-c.ctx.Done()
	})
	rest := newFakeREST()
	rest.respond = func(method, path string) Response {
		if path == "/gateway/bot" {
			return Response{Status: 200, Body: []byte(`{"url":"` + gw.wsURL() + `","session_start_limit":{"total":1000,"remaining":999,"reset_after":1000}}`)}
		}
		return Response{}
	}
	a := New(Config{Mode: ModePrompt, Log: quietLog(), Call: rest.Call, AllowGateway: allowAll, Jitter: func() float64 { return 0.999 },
		Dial: func(ctx context.Context, raw string) (Conn, error) {
			c, err := testDial(ctx, raw)
			if err != nil {
				return nil, err
			}
			return slowCloseConn{Conn: c, took: took, code: code}, nil
		}})
	a.SetHost(&fakeHost{bound: map[string]bool{}})
	done := make(chan struct{})
	go func() { defer close(done); a.Run(context.Background(), testToken) }()
	select {
	case <-readied:
	case <-time.After(5 * time.Second):
		t.Fatal("no READY")
	}
	time.Sleep(20 * time.Millisecond) // sleep: READY read on the client's side
	return a, done
}

// Pin (row 4): Shutdown returns once the clean 1000 close was sent, and waits
// for it no longer than closeWait.
func TestShutdownWaitsForTheClose(t *testing.T) {
	var code atomic.Int32
	a, done := shutdownRun(t, 300*time.Millisecond, &code)
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := code.Load(); got != 1000 {
		t.Fatalf("Shutdown returned before the close was sent (code %d)", got)
	}
	<-done

	var code2 atomic.Int32
	b, done2 := shutdownRun(t, 1500*time.Millisecond, &code2)
	b.closeWait = 200 * time.Millisecond
	began := time.Now()
	_ = b.Shutdown(context.Background())
	if took := time.Since(began); took > time.Second {
		t.Fatalf("Shutdown waited %v for a close that hangs, want closeWait", took)
	}
	<-done2
}
