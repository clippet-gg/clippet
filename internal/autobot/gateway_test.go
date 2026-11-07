package autobot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Pin: IDENTIFY carries GUILDS | GUILD_MESSAGES | MESSAGE_CONTENT, and a
// MESSAGE_CREATE on the socket reaches the mode (a Prompt reply is posted).
func TestIdentifyAndDispatch(t *testing.T) {
	gotIdentify := make(chan map[string]any, 1)
	gw := newGW(t, func(c *gwConn) {
		hello(c, 60000)
		gotIdentify <- c.next(t, opIdentify)
		ready(c, "s1", "", 1)
		c.send(map[string]any{"op": opDispatch, "t": "MESSAGE_CREATE", "s": 2, "d": jsonAny(t, videoMessage(testGuild))})
		<-c.ctx.Done()
	})
	rest := newFakeREST()
	_, stop := newGatewayAuto(t, gw, nil, rest)
	defer stop()
	id := <-gotIdentify
	d := id["d"].(map[string]any)
	if int(d["intents"].(float64)) != 1|1<<9|1<<15 || d["token"] != testToken {
		t.Fatalf("IDENTIFY = %v", d)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range rest.snapshot() {
			if c.Method == http.MethodPost && c.Path == "/channels/"+testChannel+"/messages" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond) // sleep: polling the fake REST for the dispatch's reply
	}
	t.Fatalf("the dispatched video got no reply: %+v", rest.snapshot())
}

// Pin: a dropped socket is RESUMEd — the same session id and the last
// sequence number, on the resume URL READY named; no second IDENTIFY.
func TestResumeAfterDrop(t *testing.T) {
	resumed := make(chan map[string]any, 1)
	var gw *gwServer
	gw = newGW(t,
		func(c *gwConn) {
			hello(c, 60000)
			c.next(t, opIdentify)
			ready(c, "sess-1", gw.wsURL(), 1)
			c.send(map[string]any{"op": opDispatch, "t": "GUILD_CREATE", "s": 7, "d": map[string]any{}})
			// A drop that is not a close Discord marks fatal.
			_ = c.ws.Close(websocket.StatusCode(4000), "")
		},
		func(c *gwConn) {
			hello(c, 60000)
			select {
			case f := <-c.in:
				resumed <- f
			case <-time.After(5 * time.Second):
			}
			<-c.ctx.Done()
		})
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	_, stop := newGatewayAuto(t, gw, clock, newFakeREST())
	defer stop()
	select {
	case f := <-resumed:
		if int(f["op"].(float64)) != opResume {
			t.Fatalf("after a drop the client sent op %v, want RESUME", f["op"])
		}
		d := f["d"].(map[string]any)
		if d["session_id"] != "sess-1" || int(d["seq"].(float64)) != 7 || d["token"] != testToken {
			t.Fatalf("RESUME = %v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no second connection after the drop")
	}
}

// Pin: op 7 is a RESUME at once; op 9 (not resumable) is a fresh IDENTIFY
// after 1-5 s — and two IDENTIFYs are never closer than 5 s.
func TestReconnectInvalidAndIdentifySpacing(t *testing.T) {
	idents := make(chan time.Time, 4)
	resumes := make(chan struct{}, 4)
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	var gw *gwServer
	gw = newGW(t,
		func(c *gwConn) { // 1: identify, READY, then op 7
			hello(c, 60000)
			c.next(t, opIdentify)
			idents <- clock.Now()
			ready(c, "sess-1", gw.wsURL(), 1)
			c.send(map[string]any{"op": opReconnect})
			<-c.ctx.Done()
		},
		func(c *gwConn) { // 2: must RESUME, then op 9 false
			hello(c, 60000)
			c.next(t, opResume)
			resumes <- struct{}{}
			c.send(map[string]any{"op": opInvalid, "d": false})
			<-c.ctx.Done()
		},
		func(c *gwConn) { // 3: must IDENTIFY afresh
			hello(c, 60000)
			c.next(t, opIdentify)
			idents <- clock.Now()
			<-c.ctx.Done()
		})
	a, stop := newGatewayAuto(t, gw, clock, newFakeREST())
	defer stop()
	var first, second time.Time
	select {
	case first = <-idents:
	case <-time.After(5 * time.Second):
		t.Fatal("no first IDENTIFY")
	}
	select {
	case <-resumes:
	case <-time.After(5 * time.Second):
		t.Fatal("op 7 was not followed by a RESUME")
	}
	select {
	case second = <-idents:
	case <-time.After(5 * time.Second):
		t.Fatal("op 9 (false) was not followed by an IDENTIFY")
	}
	if gap := second.Sub(first); gap < 5*time.Second {
		t.Fatalf("two IDENTIFYs %v apart, want at least 5 s", gap)
	}
	waited := false
	clock.mu.Lock()
	for _, d := range clock.sleeps {
		if d >= time.Second && d < 5*time.Second {
			waited = true
		}
	}
	clock.mu.Unlock()
	if !waited {
		t.Fatal("op 9 was not waited 1-5 s")
	}
	if a.Halted() != "" {
		t.Fatalf("halted: %s", a.Halted())
	}
}

// Pin: the IDENTIFY floor on its own: two IDENTIFYs in a row wait out the gap.
func TestIdentifyGap(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	a := New(Config{Mode: ModeAuto, Log: quietLog(), Now: clock.Now, Sleep: clock.Sleep})
	ctx := context.Background()
	if err := a.waitIdentify(ctx); err != nil {
		t.Fatal(err)
	}
	t1 := clock.Now()
	clock.mu.Lock()
	clock.now = clock.now.Add(2 * time.Second)
	clock.mu.Unlock()
	if err := a.waitIdentify(ctx); err != nil {
		t.Fatal(err)
	}
	if gap := clock.Now().Sub(t1); gap < 5*time.Second {
		t.Fatalf("the second IDENTIFY came %v after the first", gap)
	}
}

// Pin: 4014 (the Message Content intent off) halts — no reconnect.
func TestFatalCloseHalts(t *testing.T) {
	gw := newGW(t, func(c *gwConn) {
		hello(c, 60000)
		c.next(t, opIdentify)
		_ = c.ws.Close(websocket.StatusCode(4014), "Disallowed intent(s).")
	})
	a, stop := newGatewayAuto(t, gw, &fakeClock{now: time.Unix(1_800_000_000, 0)}, newFakeREST())
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for a.Halted() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond) // sleep: polling the halt flag the run loop sets
	}
	if a.Halted() != "disallowed_intents" {
		t.Fatalf("halted = %q", a.Halted())
	}
	time.Sleep(50 * time.Millisecond) // sleep: giving a wrong reconnect the time to happen
	if n := gw.conns.Load(); n != 1 {
		t.Fatalf("%d connections after a fatal close", n)
	}
}

// Pin: DISCORD_AUTO_MODE=off opens no socket — New builds nothing, and a
// gateway that listens is never connected to. The same harness with prompt
// connects once, so the count is the switch's doing.
func TestOffOpensNoSocket(t *testing.T) {
	var accepted atomic.Int32
	count := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			accepted.Add(1)
			ws, err := websocket.Accept(w, r, nil)
			if err == nil {
				<-r.Context().Done()
				_ = ws.CloseNow()
			}
		}))
	}
	srv := count()
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	start := func(mode string) {
		rest := newFakeREST()
		rest.respond = func(method, path string) Response {
			if path == "/gateway/bot" {
				return Response{Status: 200, Body: []byte(`{"url":"` + wsURL + `","session_start_limit":{"total":1,"remaining":1}}`)}
			}
			return Response{}
		}
		a := New(Config{Mode: mode, Log: quietLog(), Call: rest.Call, Dial: testDial,
			AllowGateway: allowAll, HelloWait: 50 * time.Millisecond})
		if a == nil {
			return // off: nothing to run
		}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		a.Run(ctx, testToken)
	}
	start(ModeOff)
	start("")
	if accepted.Load() != 0 {
		t.Fatalf("off opened %d socket(s)", accepted.Load())
	}
	start(ModePrompt)
	if accepted.Load() == 0 {
		t.Fatal("the harness never connects, so the off count proves nothing")
	}
}

// A 429 is waited out (retry_after) and tried once more; a bucket at 0 is
// not called again before its reset.
func TestRateLimits(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	n := 0
	r := &rest{now: clock.Now, sleep: clock.Sleep, buckets: map[string]*bucket{}, routes: map[string]string{},
		call: func(ctx context.Context, method, path string, body []byte, auth bool) (Response, error) {
			n++
			if n == 1 {
				return Response{Status: 429, Header: http.Header{}, Body: []byte(`{"retry_after":2.5,"global":false}`)}, nil
			}
			h := http.Header{}
			h.Set("X-RateLimit-Bucket", "abc")
			h.Set("X-RateLimit-Remaining", "0")
			h.Set("X-RateLimit-Reset-After", "3")
			return Response{Status: 200, Header: h}, nil
		}}
	resp, err := r.do(context.Background(), "POST", "/channels/1/messages", "k", map[string]any{"x": 1}, true)
	if err != nil || resp.Status != 200 || n != 2 {
		t.Fatalf("status=%d n=%d err=%v", resp.Status, n, err)
	}
	if len(clock.sleeps) != 1 || clock.sleeps[0] != 2500*time.Millisecond {
		t.Fatalf("sleeps = %v, want the 429's 2.5 s", clock.sleeps)
	}
	_, _ = r.do(context.Background(), "POST", "/channels/1/messages", "k", nil, true)
	if len(clock.sleeps) != 2 || clock.sleeps[1] != 3*time.Second {
		t.Fatalf("sleeps = %v, want the exhausted bucket's 3 s", clock.sleeps)
	}
}

// A REST 401 on the bot token halts everything.
func TestUnauthorizedHalts(t *testing.T) {
	a, rest, _, _ := newModeAuto(t, ModeAuto, true)
	rest.respond = func(method, path string) Response { return Response{Status: 401} }
	a.onMessage(context.Background(), videoMessage(testGuild))
	rest.waitCalls(t, 1)
	waitIdle(t, a)
	if a.Halted() != "rest_401" {
		t.Fatalf("halted = %q", a.Halted())
	}
}

// Pin: a heartbeat Discord never ACKs is a zombie: the socket is closed and
// the session RESUMEd on a new one.
func TestZombieHeartbeatResumes(t *testing.T) {
	resumed := make(chan struct{}, 1)
	var gw *gwServer
	gw = newGW(t,
		func(c *gwConn) {
			hello(c, 40)
			c.next(t, opIdentify)
			ready(c, "sess-z", gw.wsURL(), 3)
			<-c.ctx.Done()
		},
		func(c *gwConn) {
			hello(c, 60000)
			c.next(t, opResume)
			resumed <- struct{}{}
			<-c.ctx.Done()
		})
	gw.noAckFlag.Store(true)
	_, stop := newGatewayAuto(t, gw, &fakeClock{now: time.Unix(1_800_000_000, 0)}, newFakeREST())
	defer stop()
	select {
	case <-resumed:
	case <-time.After(10 * time.Second):
		t.Fatal("an un-ACKed heartbeat did not lead to a RESUME")
	}
}

// identifyStorm runs rounds connections, each scripted by round, and answers
// how many IDENTIFYs a day that pace is on the fake clock and how many times
// /gateway/bot was read.
func identifyStorm(t *testing.T, rounds int, round func(c *gwConn, idents *atomic.Int32)) (perDay float64, gwBot int) {
	t.Helper()
	var idents atomic.Int32
	scripts := make([]func(c *gwConn), rounds)
	for i := range scripts {
		scripts[i] = func(c *gwConn) { round(c, &idents) }
	}
	gw := newGW(t, scripts...)
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	rest := newFakeREST()
	start := clock.Now()
	_, stop := newGatewayAuto(t, gw, clock, rest)
	deadline := time.Now().Add(30 * time.Second)
	for idents.Load() < int32(rounds) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond) // sleep: polling the fake gateway's IDENTIFY count
	}
	stop()
	n := idents.Load()
	if n < int32(rounds) {
		t.Fatalf("only %d of %d IDENTIFYs arrived", n, rounds)
	}
	for _, c := range rest.snapshot() {
		if c.Path == "/gateway/bot" {
			gwBot++
		}
	}
	return float64(n) / clock.Now().Sub(start).Hours() * 24, gwBot
}

// Pin (erratum 333(b)): a gateway that answers every IDENTIFY with op 9
// (false) is backed off, not stormed — under Discord's 1000 a day, which
// resets the token — and /gateway/bot (the budget) is re-read each time.
// Without the backoff on op 9 it was ≈17,000 a day and one read.
func TestIdentifyStormBacksOff(t *testing.T) {
	const rounds = 40
	perDay, gwBot := identifyStorm(t, rounds, func(c *gwConn, idents *atomic.Int32) {
		hello(c, 60000)
		c.next(t, opIdentify)
		idents.Add(1)
		c.send(map[string]any{"op": opInvalid, "d": false})
		<-c.ctx.Done()
	})
	t.Logf("%.0f IDENTIFYs a day", perDay)
	if perDay >= 1000 {
		t.Fatalf("an op 9 loop sends %.0f IDENTIFYs a day (Discord resets the token past 1000)", perDay)
	}
	if gwBot < rounds {
		t.Fatalf("/gateway/bot read %d times over %d unhealthy sessions, want before each IDENTIFY", gwBot, rounds)
	}
}

// Pin: a session that reaches READY and is invalidated at once does not
// reset the backoff (it never held), so READY → op 9 is no storm either.
func TestReadyThenInvalidBacksOff(t *testing.T) {
	perDay, _ := identifyStorm(t, 40, func(c *gwConn, idents *atomic.Int32) {
		hello(c, 60000)
		c.next(t, opIdentify)
		idents.Add(1)
		ready(c, "sess-x", "", 1)
		c.send(map[string]any{"op": opInvalid, "d": false})
		<-c.ctx.Done()
	})
	t.Logf("%.0f IDENTIFYs a day", perDay)
	if perDay >= 1000 {
		t.Fatalf("a READY → op 9 loop sends %.0f IDENTIFYs a day", perDay)
	}
}

// Pin: the daily ceiling. With IdentifyCap 3 the fourth IDENTIFY waits until
// the first is 24 h old.
func TestIdentifyCeiling(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	a := New(Config{Mode: ModeAuto, Log: quietLog(), Now: clock.Now, Sleep: clock.Sleep, IdentifyCap: 3})
	ctx := context.Background()
	first := clock.Now()
	for i := 0; i < 3; i++ {
		if w := a.identifyCapWait(); w != 0 {
			t.Fatalf("IDENTIFY %d asked to wait %v under the ceiling", i+1, w)
		}
		if err := a.waitIdentify(ctx); err != nil {
			t.Fatal(err)
		}
	}
	w := a.identifyCapWait()
	if got := clock.Now().Add(w).Sub(first); got != 24*time.Hour {
		t.Fatalf("the fourth IDENTIFY may go %v after the first, want 24h", got)
	}
	clock.mu.Lock()
	clock.now = first.Add(24 * time.Hour)
	clock.mu.Unlock()
	if w := a.identifyCapWait(); w != 0 {
		t.Fatalf("a day later the ceiling still asks for %v", w)
	}
}

// Pin: Run honours the ceiling — with IdentifyCap 2 and a gateway that
// answers op 9, the third IDENTIFY comes a day after the first.
func TestRunWaitsOutTheCeiling(t *testing.T) {
	idents := make(chan time.Time, 4)
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	round := func(c *gwConn) {
		hello(c, 60000)
		c.next(t, opIdentify)
		idents <- clock.Now()
		c.send(map[string]any{"op": opInvalid, "d": false})
		<-c.ctx.Done()
	}
	gw := newGW(t, round, round, round)
	rest := newFakeREST()
	rest.respond = func(method, path string) Response {
		return Response{Status: 200, Body: []byte(`{"url":"` + gw.wsURL() + `","session_start_limit":{"total":1000,"remaining":999,"reset_after":1000}}`)}
	}
	a := New(Config{Mode: ModeAuto, Log: quietLog(), Call: rest.Call, Dial: testDial, AllowGateway: allowAll,
		Now: clock.Now, Sleep: clock.Sleep, Jitter: func() float64 { return 0.999 }, IdentifyCap: 2})
	a.SetHost(&fakeHost{bound: map[string]bool{testGuild: true}, link: testLink})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx, testToken) }()
	defer func() {
		cancel()
		sctx, sc := context.WithTimeout(context.Background(), 5*time.Second)
		defer sc()
		_ = a.Shutdown(sctx)
		<-done
	}()
	var got []time.Time
	for len(got) < 3 {
		select {
		case at := <-idents:
			got = append(got, at)
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d IDENTIFYs", len(got))
		}
	}
	if gap := got[2].Sub(got[0]); gap < 24*time.Hour {
		t.Fatalf("the third IDENTIFY came %v after the first under a ceiling of 2", gap)
	}
}
