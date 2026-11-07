package autobot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The fakes: a Discord REST that records every call, a Host, timers fired by
// hand, and an in-process gateway (a WebSocket server in the test). Nothing
// here reaches a Discord host.

const (
	testGuild   = "100000000000000001"
	otherGuild  = "100000000000000002"
	testChannel = "200000000000000001"
	testMessage = "300000000000000001"
	testPoster  = "400000000000000001"
	testSelf    = "500000000000000001"
	testApp     = "600000000000000001"
	testReply   = "700000000000000001"
	testLink    = "https://clips.example/aZ3kQ9xB2mLp"
	testToken   = "tok.EN-must-never-appear_0123456789"
)

type call struct {
	Method, Path string
	Body         map[string]any
	Auth         bool
}

type fakeREST struct {
	mu      sync.Mutex
	calls   []call
	respond func(method, path string) Response
	notify  chan struct{}
}

func newFakeREST() *fakeREST {
	return &fakeREST{notify: make(chan struct{}, 1024)}
}

func (f *fakeREST) Call(ctx context.Context, method, path string, body []byte, auth bool) (Response, error) {
	var m map[string]any
	if body != nil {
		_ = json.Unmarshal(body, &m)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{Method: method, Path: path, Body: m, Auth: auth})
	resp := f.respond
	f.mu.Unlock()
	f.notify <- struct{}{}
	if resp != nil {
		if r := resp(method, path); r.Status != 0 {
			if r.Header == nil {
				r.Header = http.Header{}
			}
			return r, nil
		}
	}
	if method == http.MethodPost && strings.HasSuffix(path, "/messages") {
		return Response{Status: 200, Header: http.Header{}, Body: []byte(`{"id":"` + testReply + `"}`)}, nil
	}
	if method == http.MethodDelete {
		return Response{Status: 204, Header: http.Header{}}, nil
	}
	return Response{Status: 200, Header: http.Header{}, Body: []byte(`{}`)}, nil
}

func (f *fakeREST) snapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// waitCalls blocks until at least n calls were recorded.
func (f *fakeREST) waitCalls(t *testing.T, n int) []call {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if c := f.snapshot(); len(c) >= n {
			return c
		}
		select {
		case <-f.notify:
		case <-deadline:
			t.Fatalf("waited for %d REST calls, got %d: %+v", n, len(f.snapshot()), f.snapshot())
		}
	}
}

type fakeHost struct {
	mu      sync.Mutex
	bound   map[string]bool
	uploads []string
	link    string
	err     error
	gate    chan struct{} // when set, Upload waits for it
}

func (h *fakeHost) Bound(_ context.Context, g string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bound[g], nil
}

func (h *fakeHost) Applicable(a Attachment) bool {
	return a.URL != "" && strings.HasPrefix(a.ContentType, "video/")
}

func (h *fakeHost) Upload(ctx context.Context, g string, a Attachment) (string, error) {
	h.mu.Lock()
	h.uploads = append(h.uploads, g)
	gate := h.gate
	h.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if h.err != nil {
		return "", h.err
	}
	return h.link, nil
}

func (h *fakeHost) uploadCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.uploads)
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped atomic.Bool
}

type fakeTimers struct {
	mu sync.Mutex
	ts []*fakeTimer
}

func (ft *fakeTimers) AfterFunc(d time.Duration, f func()) func() bool {
	t := &fakeTimer{d: d, f: f}
	ft.mu.Lock()
	ft.ts = append(ft.ts, t)
	ft.mu.Unlock()
	return func() bool { return !t.stopped.Swap(true) }
}

func (ft *fakeTimers) all() []*fakeTimer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return append([]*fakeTimer(nil), ft.ts...)
}

// fire runs a timer's func as time.AfterFunc would, unless it was stopped.
func (t *fakeTimer) fire() {
	if !t.stopped.Load() {
		t.f()
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newModeAuto is an Auto whose gateway is not running: READY's identity is
// set by hand, and events are handed in directly.
func newModeAuto(t *testing.T, mode string, notice bool) (*Auto, *fakeREST, *fakeHost, *fakeTimers) {
	t.Helper()
	rest := newFakeREST()
	host := &fakeHost{bound: map[string]bool{testGuild: true}, link: testLink}
	timers := &fakeTimers{}
	a := New(Config{Mode: mode, Seconds: 10, Notice: notice, Log: quietLog(), Call: rest.Call,
		AfterFunc: timers.AfterFunc, Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }})
	if a == nil {
		t.Fatal("New returned nil for an on mode")
	}
	a.SetHost(host)
	a.selfID, a.appID = testSelf, testApp
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	})
	return a, rest, host, timers
}

func videoMessage(guild string) json.RawMessage {
	return json.RawMessage(`{"id":"` + testMessage + `","channel_id":"` + testChannel + `","guild_id":"` + guild +
		`","author":{"id":"` + testPoster + `"},"content":"look","attachments":[{"id":"1","filename":"a.mp4",` +
		`"url":"https://cdn.discordapp.com/attachments/1/2/a.mp4","size":1000,"content_type":"video/mp4"}]}`)
}

func pressBody(guild, presser, perms string) []byte {
	return []byte(`{"id":"800000000000000001","type":3,"token":"itok","application_id":"` + testApp +
		`","guild_id":"` + guild + `","channel_id":"` + testChannel + `","data":{"custom_id":"` + PressID +
		`","component_type":2},"member":{"permissions":"` + perms + `","user":{"id":"` + presser +
		`"}},"message":{"id":"` + testReply + `","author":{"id":"` + testSelf + `"}}}`)
}

// waitIdle blocks until no server queue is running.
func waitIdle(t *testing.T, a *Auto) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		n := len(a.queues)
		a.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond) // sleep: polling the queue map; the drain goroutine has no hook
	}
	t.Fatal("the queue never drained")
}

// ---------------------------------------------------------------------------
// The gateway in the test.

// gwServer is a WebSocket server that runs script for each connection, in
// order; the frames a client sends arrive on that connection's channel.
type gwServer struct {
	srv       *httptest.Server
	conns     atomic.Int32
	scripts   []func(c *gwConn)
	noAckFlag atomic.Bool
	t         *testing.T
}

type gwConn struct {
	ws  *websocket.Conn
	in  chan map[string]any
	ctx context.Context
}

func (c *gwConn) send(v any) {
	b, _ := json.Marshal(v)
	_ = c.ws.Write(c.ctx, websocket.MessageText, b)
}

// next is the next frame the client sent with op, skipping heartbeats unless
// op is the heartbeat.
func (c *gwConn) next(t *testing.T, op int) map[string]any {
	t.Helper()
	for {
		select {
		case f, ok := <-c.in:
			if !ok {
				t.Fatalf("the client closed before sending op %d", op)
			}
			if int(f["op"].(float64)) == op {
				return f
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no op %d from the client", op)
		}
	}
}

func newGW(t *testing.T, scripts ...func(c *gwConn)) *gwServer {
	g := &gwServer{scripts: scripts, t: t}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(g.conns.Add(1)) - 1
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		c := &gwConn{ws: ws, in: make(chan map[string]any, 64), ctx: ctx}
		go func() {
			defer close(c.in)
			for {
				_, p, err := ws.Read(ctx)
				if err != nil {
					return
				}
				var m map[string]any
				if json.Unmarshal(p, &m) == nil {
					if op, _ := m["op"].(float64); int(op) == opHeartbeat && !g.noAckFlag.Load() {
						c.send(map[string]any{"op": opHeartbeatAck})
						continue // acknowledged, never queued
					}
					select {
					case c.in <- m:
					default: // a test that stopped reading
					}
				}
			}
		}()
		if n < len(g.scripts) {
			g.scripts[n](c)
		} else {
			<-ctx.Done() // hold any extra connection open until the test ends
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gwServer) wsURL() string { return "ws" + strings.TrimPrefix(g.srv.URL, "http") }

// testConn adapts a client socket to Conn.
type testConn struct{ c *websocket.Conn }

type testClosed struct{ code int }

func (e *testClosed) Error() string  { return "closed" }
func (e *testClosed) CloseCode() int { return e.code }

func (c *testConn) Read(ctx context.Context) ([]byte, error) {
	_, p, err := c.c.Read(ctx)
	if err != nil {
		if code := websocket.CloseStatus(err); code != -1 {
			return nil, &testClosed{code: int(code)}
		}
		return nil, err
	}
	return p, nil
}
func (c *testConn) Write(ctx context.Context, p []byte) error {
	return c.c.Write(ctx, websocket.MessageText, p)
}
func (c *testConn) Close(code int, reason string) error {
	return c.c.Close(websocket.StatusCode(code), reason)
}

func testDial(ctx context.Context, raw string) (Conn, error) {
	c, _, err := websocket.Dial(ctx, raw, nil)
	if err != nil {
		return nil, err
	}
	return &testConn{c: c}, nil
}

// fakeClock is a clock whose sleeps return at once and move it forward.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	// Yield, so a heartbeat loop on a fake clock does not spin a core.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Millisecond):
	}
	return nil
}

// newGatewayAuto runs an Auto against gw; the REST answers /gateway/bot with
// gw's URL. It returns the Auto and a stop func.
func newGatewayAuto(t *testing.T, gw *gwServer, clock *fakeClock, rest *fakeREST) (*Auto, func()) {
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
	cfg := Config{Mode: ModePrompt, Log: quietLog(), Call: rest.Call, Dial: testDial,
		AllowGateway: func(s string) (*url.URL, error) { return url.Parse(s) },
		// A beat no sooner than a second: the scripts below answer no heartbeat.
		Jitter: func() float64 { return 0.999 }}
	if clock != nil {
		cfg.Now, cfg.Sleep = clock.Now, clock.Sleep
	}
	a := New(cfg)
	a.SetHost(&fakeHost{bound: map[string]bool{testGuild: true}, link: testLink})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx, testToken) }()
	return a, func() {
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = a.Shutdown(sctx)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	}
}

func hello(c *gwConn, intervalMS int) {
	c.send(map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": intervalMS}})
}

func ready(c *gwConn, session, resumeURL string, seq int) {
	c.send(map[string]any{"op": opDispatch, "t": "READY", "s": seq, "d": map[string]any{
		"session_id": session, "resume_gateway_url": resumeURL,
		"user": map[string]any{"id": testSelf}, "application": map[string]any{"id": testApp}}})
}

func allowAll(s string) (*url.URL, error) { return url.Parse(s) }

func jsonAny(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
