package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clippet/internal/autobot"
	"clippet/internal/autorule"
	"clippet/internal/notify"
)

// The hosted Clippet Auto (§20 erratum 334), against a fake plane (httptest),
// a fake box and the TypeScript's own seals. Nothing here reaches a Discord,
// Fly or Cloudflare host.

const (
	tGuild = "1419000000000000002"
	tHost  = "name.example.test"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type vector struct {
	RecipientPrivate json.RawMessage `json:"recipient_private"`
	Guild, Host      string
	Plaintext, Blob  string
}

func readVector(t *testing.T, name string) vector {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var v vector
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// testOpener opens with the shared vector's TEST recipient key.
func testOpener(t *testing.T) *opener {
	t.Helper()
	o, err := newOpener(string(readVector(t, "seal-vector.json").RecipientPrivate))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func flip(s string, i int) string {
	c := byte('A')
	if s[i] == 'A' {
		c = 'B'
	}
	return s[:i] + string(c) + s[i+1:]
}

// Pin: a URL sealed by clippet-pass forward/src/claim/seal.ts opens here —
// the shared vector (sealWith, fixed ephemeral key) and a fresh seal()
// (random ephemeral key and IV), both checked in; the wrong server, the wrong
// host, a flipped byte or a cut blob open to nothing.
func TestSealFromTypeScriptOpens(t *testing.T) {
	o := testOpener(t)
	for _, name := range []string{"seal-vector.json", "seal-fresh.json"} {
		v := readVector(t, name)
		got, ok := o.open(v.Blob, v.Guild, v.Host)
		if !ok || got != v.Plaintext {
			t.Fatalf("%s: open = %q %v, want %q", name, got, ok, v.Plaintext)
		}
		if _, ok := o.open(v.Blob, "900000000000000002", v.Host); ok {
			t.Fatalf("%s: opened for another server", name)
		}
		if _, ok := o.open(v.Blob, v.Guild, "other.example.org"); ok {
			t.Fatalf("%s: opened for another host", name)
		}
		for _, i := range []int{0, 1, 5, 70, 90, len(v.Blob) - 1} {
			if _, ok := o.open(flip(v.Blob, i), v.Guild, v.Host); ok {
				t.Fatalf("%s: a flip at %d opened", name, i)
			}
		}
		for _, cut := range []int{1, 10, 100, len(v.Blob) - 20} {
			if _, ok := o.open(v.Blob[:cut], v.Guild, v.Host); ok {
				t.Fatalf("%s: cut to %d opened", name, cut)
			}
		}
		if _, ok := o.open(v.Blob+"=", v.Guild, v.Host); ok {
			t.Fatalf("%s: a padded blob opened", name)
		}
	}
	if _, ok := o.open(strings.Repeat("A", 1025), tGuild, tHost); ok {
		t.Fatal("an over-long blob opened")
	}
}

// Pin: AUTO_SEAL_PRIVATE is a P-256 private JWK whose x/y are d's own public
// half; a public-only, another curve, a wrong x or junk is refused.
func TestOpenerKeyShape(t *testing.T) {
	var k map[string]string
	if err := json.Unmarshal(readVector(t, "seal-vector.json").RecipientPrivate, &k); err != nil {
		t.Fatal(err)
	}
	mut := func(f func(map[string]string)) string {
		c := map[string]string{}
		for a, b := range k {
			c[a] = b
		}
		f(c)
		b, _ := json.Marshal(c)
		return string(b)
	}
	for _, bad := range []string{"", "{", mut(func(m map[string]string) { delete(m, "d") }),
		mut(func(m map[string]string) { m["crv"] = "P-384" }), mut(func(m map[string]string) { m["x"] = k["y"] }),
		mut(func(m map[string]string) { m["d"] = k["d"][:42] })} {
		if _, err := newOpener(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// Pin: the instruction's exact shape — the hosted encoder's bytes go through
// THE decoder POST /in/auto/{token} runs (autorule.Decode, called by
// internal/httpapi/autofetch.go) and come back field for field.
func TestInstructionDecodesOnTheBox(t *testing.T) {
	a := autobot.Attachment{ID: "1", Filename: "clip ünï.mp4", ContentType: "video/mp4", Size: 123456,
		URL: "https://cdn.discordapp.com/attachments/1419000000000000003/1419000000000000004/clip.mp4?ex=1&is=2&hm=3"}
	body := instructionBody(tGuild, a)
	in, err := autorule.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("the box's decoder refused the hosted instruction %s: %v", body, err)
	}
	want := autorule.Instruction{GuildID: tGuild, URL: a.URL, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size}
	if in != want {
		t.Fatalf("decoded %+v, want %+v", in, want)
	}
	if !autorule.Applicable(in, 1<<30) || len(body) > autorule.MaxBody {
		t.Fatalf("the box would not take it: %s", body)
	}
}

// fakePlane is the claim Worker's /auto routes, in memory.
type fakePlane struct {
	mu      sync.Mutex
	records map[string]guildRecord
	session []byte
	gets    map[string]int
	status  int // when set, every answer
}

func (p *fakePlane) Plane(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gets == nil {
		p.gets = map[string]int{}
	}
	p.gets[method+" "+path]++
	if p.status != 0 {
		return p.status, nil, nil
	}
	switch {
	case method == http.MethodGet && strings.HasPrefix(path, "/auto/guild/"):
		r, ok := p.records[strings.TrimPrefix(path, "/auto/guild/")]
		if !ok {
			return 404, nil, nil
		}
		b, _ := json.Marshal(r)
		return 200, b, nil
	case method == http.MethodGet && path == "/auto/session":
		if p.session == nil {
			return 404, nil, nil
		}
		return 200, p.session, nil
	case method == http.MethodPut && path == "/auto/session":
		p.session = append([]byte(nil), body...)
		return 204, nil, nil
	case method == http.MethodGet && path == "/auto/guilds":
		var ids []string
		for id := range p.records {
			ids = append(ids, id)
		}
		b, _ := json.Marshal(map[string]any{"guilds": ids})
		return 200, b, nil
	}
	return 404, nil, nil
}

func (p *fakePlane) count(k string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gets[k]
}

// fakeBox answers one status and body, and records the URL and the bytes.
type fakeBox struct {
	mu     sync.Mutex
	status int
	body   string
	urls   []string
	sent   [][]byte
}

func (b *fakeBox) Box(_ context.Context, rawURL string, instruction []byte, _ time.Duration) (int, []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.urls = append(b.urls, rawURL)
	b.sent = append(b.sent, instruction)
	return b.status, []byte(b.body)
}

// liveRecord is the fresh fixture's server, on, with its seal.
func liveRecord(t *testing.T, mode string) guildRecord {
	v := readVector(t, "seal-fresh.json")
	return guildRecord{On: true, Mode: mode, S: 15, N: true, U: v.Blob, Host: v.Host}
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func testHost(t *testing.T, plane *fakePlane, box *fakeBox) (*planeHost, *clock, *metrics) {
	m := newMetrics(quiet())
	h := newPlaneHost(plane, box, testOpener(t), m, quiet())
	c := &clock{now: time.Date(2026, 10, 11, 23, 0, 0, 0, time.UTC)}
	h.now = c.Now
	return h, c, m
}

var video = autobot.Attachment{ID: "1", Filename: "a.mp4", ContentType: "video/mp4", Size: 1000,
	URL: "https://cdn.discordapp.com/attachments/1419000000000000003/1419000000000000004/a.mp4"}

// Pin: the per-server settings are cached 60 s — one plane read inside the
// minute, a fresh one after it, and a change in /settings is then seen.
func TestSettingsCached60s(t *testing.T) {
	plane := &fakePlane{records: map[string]guildRecord{tGuild: liveRecord(t, autobot.ModePrompt)}}
	h, c, _ := testHost(t, plane, &fakeBox{})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if ok, _ := h.Bound(ctx, tGuild); !ok {
			t.Fatal("not bound")
		}
		c.add(10 * time.Second)
	}
	s, _ := h.Settings(ctx, tGuild)
	if n := plane.count("GET /auto/guild/" + tGuild); n != 1 || s.Mode != autobot.ModePrompt || s.Seconds != 15 || !s.Notice {
		t.Fatalf("reads=%d settings=%+v, want one read inside the minute", n, s)
	}
	plane.mu.Lock()
	r := plane.records[tGuild]
	r.Mode, r.N = autobot.ModeAuto, false
	plane.records[tGuild] = r
	plane.mu.Unlock()
	c.add(15 * time.Second) // 65 s after the first read
	s, _ = h.Settings(ctx, tGuild)
	if n := plane.count("GET /auto/guild/" + tGuild); n != 2 || s.Mode != autobot.ModeAuto || s.Notice {
		t.Fatalf("reads=%d settings=%+v, want the change seen after a minute", n, s)
	}
	// Off (404) is cached too, and a plane that stops answering keeps the
	// last answer it gave.
	plane.mu.Lock()
	delete(plane.records, tGuild)
	plane.mu.Unlock()
	c.add(61 * time.Second)
	if ok, _ := h.Bound(ctx, tGuild); ok {
		t.Fatal("a 404 still bound")
	}
	plane.mu.Lock()
	plane.status = 500
	plane.mu.Unlock()
	c.add(61 * time.Second)
	if ok, err := h.Bound(ctx, tGuild); ok || err != nil {
		t.Fatalf("a failing plane: ok=%v err=%v, want the last answer (off)", ok, err)
	}
}

// Pin (review F3): a plane failure is remembered for planeRetry — after one
// failure, five Bound calls inside 10 s make ONE plane call (a plane outage
// does not hold a goroutine per message), the first call past 10 s asks again,
// and a server the plane has never answered for stays an error, not "on".
func TestPlaneFailureCached10s(t *testing.T) {
	for _, known := range []bool{false, true} {
		plane := &fakePlane{records: map[string]guildRecord{tGuild: liveRecord(t, autobot.ModePrompt)}}
		h, c, _ := testHost(t, plane, &fakeBox{})
		ctx := context.Background()
		k := "GET /auto/guild/" + tGuild
		base := 0
		if known {
			if ok, _ := h.Bound(ctx, tGuild); !ok {
				t.Fatal("not bound")
			}
			c.add(61 * time.Second)
			base = 1
		}
		plane.mu.Lock()
		plane.status = 503
		plane.mu.Unlock()
		for i := 0; i < 6; i++ { // the failure, then five more inside 10 s
			ok, err := h.Bound(ctx, tGuild)
			if known && (!ok || err != nil) {
				t.Fatalf("known: ok=%v err=%v, want the last answer (on)", ok, err)
			}
			if !known && (ok || err == nil) {
				t.Fatalf("never answered: ok=%v err=%v, want an error", ok, err)
			}
			c.add(time.Second)
		}
		if n := plane.count(k) - base; n != 1 {
			t.Fatalf("known=%v: %d plane calls inside 10 s of a failure, want 1", known, n)
		}
		c.add(5 * time.Second) // 11 s after the failure
		_, _ = h.Bound(ctx, tGuild)
		if n := plane.count(k) - base; n != 2 {
			t.Fatalf("known=%v: %d plane calls past 10 s, want 2", known, n)
		}
		plane.mu.Lock()
		plane.status = 0
		plane.mu.Unlock()
		c.add(11 * time.Second)
		if ok, err := h.Bound(ctx, tGuild); !ok || err != nil {
			t.Fatalf("known=%v: the plane back: ok=%v err=%v", known, ok, err)
		}
	}
}

// Pin: a press's upload opens the seal for (guild, route host), POSTs the
// instruction to that URL, and answers the box's link — only a root link on
// the route's own host.
func TestUploadDispatchesToTheBox(t *testing.T) {
	v := readVector(t, "seal-fresh.json")
	plane := &fakePlane{records: map[string]guildRecord{tGuild: liveRecord(t, autobot.ModeAuto)}}
	box := &fakeBox{status: 201, body: `{"id":"aZ3kQ9xB2mLp","url":"https://` + tHost + `/aZ3kQ9xB2mLp"}`}
	h, _, _ := testHost(t, plane, box)
	link, err := h.Upload(context.Background(), tGuild, video)
	if err != nil || link != "https://"+tHost+"/aZ3kQ9xB2mLp" {
		t.Fatalf("Upload = %q, %v", link, err)
	}
	if len(box.urls) != 1 || box.urls[0] != v.Plaintext {
		t.Fatalf("box URLs = %v", box.urls)
	}
	if in, err := autorule.Decode(bytes.NewReader(box.sent[0])); err != nil || in.GuildID != tGuild || in.URL != video.URL {
		t.Fatalf("sent %s (%v)", box.sent[0], err)
	}
	for _, foreign := range []string{`{"url":"https://evil.example/aZ3kQ9xB2mLp"}`, `{"url":"https://` + tHost + `/a/b"}`,
		`{"url":"https://` + tHost + `/x?y=1"}`, `{"url":"look at this"}`} {
		box.body = foreign
		if _, err := h.Upload(context.Background(), tGuild, video); err == nil || err.(autobot.Liner).Line() != autobot.LineFailed {
			t.Fatalf("posted a link the box chose: %s (%v)", foreign, err)
		}
	}
}

// Pin: a 429 from the box is its own line for the presser, never a retry.
func TestBox429IsTheLimitLine(t *testing.T) {
	plane := &fakePlane{records: map[string]guildRecord{tGuild: liveRecord(t, autobot.ModePrompt)}}
	box := &fakeBox{status: 429}
	h, _, _ := testHost(t, plane, box)
	_, err := h.Upload(context.Background(), tGuild, video)
	l, ok := err.(autobot.Liner)
	if !ok || l.Line() != lineLimit {
		t.Fatalf("a 429 answered %v, want %q", err, lineLimit)
	}
	if len(box.urls) != 1 {
		t.Fatalf("the box was called %d times", len(box.urls))
	}
	for status, want := range map[int]string{0: lineNoAnswer, 202: lineWorking, 404: lineURLGone, 413: lineTooLarge,
		415: lineNotPlayable, 507: lineFull, 503: lineNoAnswer, 400: autobot.LineFailed} {
		box.status = status
		if _, err := h.Upload(context.Background(), tGuild, video); err.(autobot.Liner).Line() != want {
			t.Fatalf("%d answered %v, want %q", status, err, want)
		}
	}
	// A sound is accepted too since §20 erratum 328: the 415 line names both.
	if lineNotPlayable != "That file is not a video or sound Clippet can play." {
		t.Fatalf("the 415 line = %q", lineNotPlayable)
	}
	box.status, box.body = 403, `{"error":"not_bound"}`
	if _, err := h.Upload(context.Background(), tGuild, video); err.(autobot.Liner).Line() != lineWrongAccount {
		t.Fatalf("not_bound answered %v", err)
	}
}

// Pin: a seal that opens to a URL off the route's host — or does not open —
// is never sent anywhere.
func TestUploadRefusesForeignURL(t *testing.T) {
	r := liveRecord(t, autobot.ModeAuto)
	r.Host = "other.clippet.gg" // the route moved: the seal no longer opens
	plane := &fakePlane{records: map[string]guildRecord{tGuild: r}}
	box := &fakeBox{status: 201}
	h, _, _ := testHost(t, plane, box)
	if _, err := h.Upload(context.Background(), tGuild, video); err.(autobot.Liner).Line() != lineURLGone || len(box.urls) != 0 {
		t.Fatalf("err=%v box=%v", err, box.urls)
	}
	// A seal that OPENS for the route, but whose URL names another host (the
	// plane never seals one; a fixture sealed by forward's seal() all the same):
	// refused, and nothing is sent anywhere.
	m := readVector(t, "seal-mismatch.json")
	plane.mu.Lock()
	plane.records[tGuild] = guildRecord{On: true, Mode: autobot.ModeAuto, S: 10, N: true, U: m.Blob, Host: m.Host}
	plane.mu.Unlock()
	h2, _, _ := testHost(t, plane, box)
	if got, ok := h2.open.open(m.Blob, m.Guild, m.Host); !ok || got != m.Plaintext {
		t.Fatalf("the mismatch fixture did not open: %q %v", got, ok)
	}
	if _, err := h2.Upload(context.Background(), tGuild, video); err.(autobot.Liner).Line() != lineURLGone || len(box.urls) != 0 {
		t.Fatalf("a URL off the route's host: err=%v box=%v", err, box.urls)
	}
	for _, s := range []string{"https://" + tHost + ":443/in/auto/ca_" + strings.Repeat("a", 43), "http://" + tHost + "/in/auto/ca_" + strings.Repeat("a", 43),
		"https://" + tHost + "/in/auto/ca_" + strings.Repeat("a", 42), "https://" + tHost + "/in/ci_" + strings.Repeat("a", 43),
		"https://" + tHost + "/in/auto/ca_" + strings.Repeat("a", 43) + "?x", "https://u@" + tHost + "/in/auto/ca_" + strings.Repeat("a", 43)} {
		if urlShapeOK(s, tHost) {
			t.Fatalf("shape accepted %s", s)
		}
	}
	if !urlShapeOK("https://"+tHost+"/in/auto/ca_"+strings.Repeat("a", 43), tHost) {
		t.Fatal("the good shape was refused")
	}
}

// Pin: the session round-trips through the plane's /auto/session.
func TestPlaneSessions(t *testing.T) {
	plane := &fakePlane{}
	s := planeSessions{plane: plane, m: newMetrics(quiet())}
	if got, err := s.Load(context.Background()); err != nil || got != (autobot.Session{}) {
		t.Fatalf("no session: %+v %v", got, err)
	}
	want := autobot.Session{ID: "sess-1", Seq: 42, ResumeURL: "wss://gateway-us-east1-b.discord.gg", Last: "1419000000000000009"}
	if err := s.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load(context.Background()); err != nil || got != want {
		t.Fatalf("read back %+v %v", got, err)
	}
	plane.status = 401
	if _, err := s.Load(context.Background()); err == nil {
		t.Fatal("a 401 read as no session")
	}
}

// Pin: /healthz is 200 only while the session is READY or RESUMED and no
// heartbeat waits for its ACK past the grace; 503 before READY, after a drop,
// and with a beat unanswered.
func TestHealthz(t *testing.T) {
	m := newMetrics(quiet())
	c := &clock{now: time.Date(2026, 10, 11, 23, 0, 0, 0, time.UTC)}
	m.now = c.Now
	srv := httptest.NewServer(handler(m))
	defer srv.Close()
	code := func() int {
		r, err := srv.Client().Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		return r.StatusCode
	}
	if code() != 503 {
		t.Fatal("healthy before READY")
	}
	m.Gateway(autobot.GatewayEvent{Kind: "up"})
	if code() != 200 {
		t.Fatal("not healthy after READY")
	}
	m.Gateway(autobot.GatewayEvent{Kind: "beat"})
	c.add(5 * time.Second)
	if code() != 200 {
		t.Fatal("a beat in flight 5 s is not yet unhealthy")
	}
	c.add(6 * time.Second)
	if code() != 503 {
		t.Fatal("healthy with a beat unanswered 11 s")
	}
	m.Gateway(autobot.GatewayEvent{Kind: "ack", RTT: 40 * time.Millisecond})
	if code() != 200 {
		t.Fatal("not healthy after the ACK")
	}
	m.Gateway(autobot.GatewayEvent{Kind: "down"})
	if code() != 503 {
		t.Fatal("healthy while disconnected")
	}
}

// Pin: /metrics carries every series the runbook's panels read, by name.
func TestMetricsNames(t *testing.T) {
	m := newMetrics(quiet())
	m.Gateway(autobot.GatewayEvent{Kind: "identify"})
	m.Decide(autobot.Decision{Event: "video", Mode: "auto"})
	m.Decide(autobot.Decision{Event: "link", Outcome: "linked", Took: 1500 * time.Millisecond})
	srv := httptest.NewServer(handler(m))
	defer srv.Close()
	r, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	out := string(b)
	for _, name := range []string{"auto_gateway_up", "auto_identifies_24h", "auto_resumes_total", "auto_reconnects_total",
		"auto_heartbeat_latency_ms", "auto_guilds_bound", `auto_videos_seen_total{mode="auto"}`, `auto_uploads_total{outcome="linked"}`,
		`auto_dispatch_ms_bucket{le="2500"} 1`, "auto_dispatch_ms_count 1", "auto_queue_depth", "auto_rest_429_total", "auto_plane_errors_total",
		"auto_identifies_24h 1"} {
		if !strings.Contains(out, name) {
			t.Fatalf("/metrics lacks %s:\n%s", name, out)
		}
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("content type %q", r.Header.Get("Content-Type"))
	}
}

// Pin: a decision is ONE JSON line with ts, level, event, guild, channel,
// msg_id, outcome, ms — and nothing of a token, a URL or a file name.
func TestDecisionLogLine(t *testing.T) {
	var buf bytes.Buffer
	m := newMetrics(newLogger(&buf, "json"))
	m.Decide(autobot.Decision{Event: "link", Guild: tGuild, Channel: "1419000000000000003", Message: "1419000000000000005",
		Mode: "auto", Outcome: "linked", Took: 2300 * time.Millisecond})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines: %q", lines)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"level": "INFO", "event": "link", "guild": tGuild, "channel": "1419000000000000003",
		"msg_id": "1419000000000000005", "outcome": "linked", "ms": float64(2300)} {
		if got[k] != want {
			t.Fatalf("%s = %v, want %v (%s)", k, got[k], want, lines[0])
		}
	}
	if _, ok := got["ts"]; !ok {
		t.Fatalf("no ts: %s", lines[0])
	}
}

// Pin: the configuration is the environment's seven names; a missing secret
// or a plain-http dead-man is a boot error; String never prints a secret.
func TestLoadEnv(t *testing.T) {
	env := map[string]string{"DISCORD_AUTO_TOKEN": "tok.SECRET", "AUTO_PLANE_URL": "https://bot.clippet.gg/claim",
		"AUTO_PLANE_SECRET": "plane.SECRET", "AUTO_SEAL_PRIVATE": `{"kty":"EC"}`}
	get := func(k string) string { return env[k] }
	c, err := loadEnv(get)
	if err != nil || c.listen != ":9091" || c.logFormat != "json" {
		t.Fatalf("%+v %v", c, err)
	}
	if s := c.String(); strings.Contains(s, "SECRET") || strings.Contains(s, "EC") {
		t.Fatalf("String printed a secret: %s", s)
	}
	for k, v := range map[string]string{"DISCORD_AUTO_TOKEN": "", "AUTO_PLANE_SECRET": "", "AUTO_SEAL_PRIVATE": "",
		"AUTO_LOG": "xml", "AUTO_DEADMAN_URL": "http" + "://hc-ping.example/x", "AUTO_LISTEN": "9091"} {
		saved := env[k]
		env[k] = v
		if _, err := loadEnv(get); err == nil || !strings.Contains(err.Error(), k) {
			t.Fatalf("%s=%q: %v", k, v, err)
		}
		env[k] = saved
	}
}

// The real notify client against an httptest plane: the bearer goes on the
// plane call, never on the box call, and neither follows a redirect.
func TestHostedClientHeaders(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Clone()
		mu.Unlock()
		if r.URL.Path == "/in/auto/redirect" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"url":"x"}`))
	}))
	defer srv.Close()
	hc, err := notify.NewHostedClient(notify.HostedDeps{PlaneURL: srv.URL + "/claim", PlaneSecret: "plane.SECRET", AllowHTTP: true, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	if st, _, err := hc.Plane(context.Background(), http.MethodGet, "/auto/session", nil); err != nil || st != 201 {
		t.Fatalf("plane %d %v", st, err)
	}
	if st, body := hc.Box(context.Background(), srv.URL+"/in/auto/x", []byte(`{}`), time.Second); st != 201 || string(body) != `{"url":"x"}` {
		t.Fatalf("box %d %s", st, body)
	}
	if st, _ := hc.Box(context.Background(), srv.URL+"/in/auto/redirect", []byte(`{}`), time.Second); st != 302 {
		t.Fatalf("a redirect was followed: %d", st)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["/claim/auto/session"].Get("Authorization") != "Bearer plane.SECRET" {
		t.Fatalf("plane headers %v", seen["/claim/auto/session"])
	}
	box := seen["/in/auto/x"]
	if box.Get("Authorization") != "" || box.Get("User-Agent") != notify.BoxUserAgent || box.Get("Content-Type") != "application/json" {
		t.Fatalf("box headers %v", box)
	}
	if _, ok := seen["/elsewhere"]; ok {
		t.Fatal("the redirect was followed")
	}
}

// ---------------------------------------------------------------------------
// The 2026-10-12 amendment (lane `autofix`, the review's audit of the live
// process): each pin fails with its fix reverted.

// Pin (audit F1): the plane refusing the session (a 4xx other than 429) is
// autobot.ErrSessionRefused — not tried again until the session changes —;
// a 429, a 5xx or no answer is a failure autobot retries with its backoff.
func TestPlaneSessionRefusal(t *testing.T) {
	for status, refused := range map[int]bool{400: true, 401: true, 404: true, 413: true, 429: false, 500: false, 503: false} {
		plane := &fakePlane{status: status}
		err := planeSessions{plane: plane, m: newMetrics(quiet())}.Save(context.Background(), autobot.Session{ID: "s"})
		if err == nil || errors.Is(err, autobot.ErrSessionRefused) != refused {
			t.Fatalf("PUT answered %d: err %v, refused=%v want %v", status, err, errors.Is(err, autobot.ErrSessionRefused), refused)
		}
	}
}

// gatedPlane holds every GET /auto/guild/ until gate closes.
type gatedPlane struct {
	*fakePlane
	gate    chan struct{}
	arrived atomic.Int32
}

func (g *gatedPlane) Plane(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	if strings.HasPrefix(path, "/auto/guild/") {
		g.arrived.Add(1)
		<-g.gate
	}
	return g.fakePlane.Plane(ctx, method, path, body)
}

// Pin (audit F3): one plane GET per server at a time — twenty messages of a
// server never answered, all arriving while the first GET is under way, make
// ONE plane call, and all twenty get its answer.
func TestPlaneOneCallInFlight(t *testing.T) {
	plane := &gatedPlane{fakePlane: &fakePlane{records: map[string]guildRecord{tGuild: liveRecord(t, autobot.ModeAuto)}},
		gate: make(chan struct{})}
	h, _, _ := testHost(t, plane.fakePlane, &fakeBox{})
	h.plane = plane
	var wg sync.WaitGroup
	var bound atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := h.Bound(context.Background(), tGuild); ok && err == nil {
				bound.Add(1)
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for plane.arrived.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond) // sleep: waiting for the first GET to reach the plane
	}
	time.Sleep(100 * time.Millisecond) // sleep: the other nineteen arrive behind it
	close(plane.gate)
	wg.Wait()
	if n := plane.count("GET /auto/guild/" + tGuild); n != 1 || bound.Load() != 20 {
		t.Fatalf("%d plane calls for 20 concurrent messages (%d answered bound), want 1 and 20", n, bound.Load())
	}
}

// Pin (audit F6): with a fresh GET /auto/guilds list, a server NOT on it is off
// with no plane call of its own; a server on it is asked as before; a list
// older than listFresh filters nothing.
func TestGuildListPrefilters(t *testing.T) {
	plane := &fakePlane{records: map[string]guildRecord{tGuild: liveRecord(t, autobot.ModePrompt)}}
	h, c, m := testHost(t, plane, &fakeBox{})
	ctx := context.Background()
	const other = "1419000000000000099"
	h.refreshList(ctx)
	m.mu.Lock()
	gauge := m.guilds
	m.mu.Unlock()
	if gauge != 1 {
		t.Fatalf("auto_guilds_bound = %d", gauge)
	}
	for i := 0; i < 5; i++ {
		if ok, err := h.Bound(ctx, other); ok || err != nil {
			t.Fatalf("an unlisted server: ok=%v err=%v", ok, err)
		}
	}
	if n := plane.count("GET /auto/guild/" + other); n != 0 {
		t.Fatalf("an unlisted server made %d plane calls, want 0", n)
	}
	if ok, _ := h.Bound(ctx, tGuild); !ok || plane.count("GET /auto/guild/"+tGuild) != 1 {
		t.Fatal("a listed server was not asked")
	}
	if n := plane.count("GET /auto/guilds"); n != 1 {
		t.Fatalf("the list was read %d times inside its minute, want 1", n)
	}
	// The other server turns Auto on: its messages a minute later re-read the
	// LIST once (never one GET each) and it is seen — within the minute, as
	// before the filter.
	plane.mu.Lock()
	plane.records[other] = liveRecord(t, autobot.ModeAuto)
	plane.mu.Unlock()
	c.add(listRecheck)
	for i := 0; i < 3; i++ {
		if ok, err := h.Bound(ctx, other); !ok || err != nil {
			t.Fatalf("a server that turned Auto on, a minute later: ok=%v err=%v", ok, err)
		}
	}
	if l, g := plane.count("GET /auto/guilds"), plane.count("GET /auto/guild/"+other); l != 2 || g != 1 {
		t.Fatalf("list reads %d, record reads %d; want 2 and 1", l, g)
	}
	// A list older than listFresh filters nothing.
	const third = "1419000000000000077"
	c.add(listFresh + time.Second)
	_, _ = h.Bound(ctx, third)
	if n := plane.count("GET /auto/guild/" + third); n != 1 {
		t.Fatalf("a stale list still filtered (%d calls), want the server asked", n)
	}
}

// listPlane answers GET /auto/guilds with one fixed body.
type listPlane struct{ body string }

func (p listPlane) Plane(_ context.Context, method, path string, _ []byte) (int, []byte, error) {
	if method == http.MethodGet && path == "/auto/guilds" {
		return 200, []byte(p.body), nil
	}
	return 404, nil, nil
}

// Pin (lane autoroute): auto_guilds_bound counts `routable` when GET
// /auto/guilds carries it — a server whose route is disabled is not bound —
// and `guilds` when it does not (the old shape); the list the pre-filter
// keeps is `guilds` in both.
func TestGuildsGaugeCountsRoutable(t *testing.T) {
	const a, b, c = "1419000000000000011", "1419000000000000012", "1419000000000000013"
	for _, tc := range []struct {
		name, body string
		gauge      int
	}{
		{"old shape", `{"guilds":["` + a + `","` + b + `","` + c + `"]}`, 3},
		{"routable", `{"guilds":["` + a + `","` + b + `","` + c + `"],"routable":["` + a + `"]}`, 1},
		{"none routable", `{"guilds":["` + a + `"],"routable":[]}`, 0},
	} {
		m := newMetrics(quiet())
		ids, ok := boundGuilds(context.Background(), listPlane{tc.body}, m)
		m.mu.Lock()
		gauge := m.guilds
		m.mu.Unlock()
		if !ok || gauge != tc.gauge {
			t.Fatalf("%s: ok=%v auto_guilds_bound=%d, want %d", tc.name, ok, gauge, tc.gauge)
		}
		if tc.name != "none routable" && len(ids) != 3 {
			t.Fatalf("%s: the list is %v, want guilds", tc.name, ids)
		}
	}
}

// Pin (§20 erratum 347): AUTO_TEST_ENDPOINTS=1 is refused unless the token is
// not a Discord token, the plane is a .test name and no dead-man is set; any
// value but 1 is refused; on, it is the first line the process logs.
func TestTestEndpointsGuard(t *testing.T) {
	realTok := base64.RawStdEncoding.EncodeToString([]byte("1234567890123456789")) + ".GaBcDe.abcdefghijklmnopqrstuvwxyz0123456"
	env := map[string]string{"DISCORD_AUTO_TOKEN": "e2e-autohost-token.not.discord", "AUTO_PLANE_URL": "https://plane.clippet.test/claim",
		"AUTO_PLANE_SECRET": "plane.SECRET", "AUTO_SEAL_PRIVATE": `{"kty":"EC"}`, "AUTO_TEST_ENDPOINTS": "1"}
	get := func(k string) string { return env[k] }
	c, err := loadEnv(get)
	if err != nil || !c.testEndpoints {
		t.Fatalf("a test process: %+v %v", c, err)
	}
	if !strings.Contains(c.String(), "test_endpoints=true") {
		t.Fatalf("String does not say the knob is on: %s", c)
	}
	for _, tc := range []struct{ k, v, want string }{
		{"AUTO_TEST_ENDPOINTS", "yes", "AUTO_TEST_ENDPOINTS must be 1 or unset"},
		{"AUTO_TEST_ENDPOINTS", "0", "AUTO_TEST_ENDPOINTS must be 1 or unset"},
		{"DISCORD_AUTO_TOKEN", realTok, "needs a DISCORD_AUTO_TOKEN that is not a Discord token"},
		{"DISCORD_AUTO_TOKEN", strings.ReplaceAll(base64.URLEncoding.EncodeToString([]byte("12345678901234567")), "=", "") + ".x.y", "not a Discord token"},
		{"AUTO_PLANE_URL", "https://bot.clippet.gg/claim", "needs an AUTO_PLANE_URL under the reserved .test domain"},
		{"AUTO_PLANE_URL", "https://plane.clippet.test.example/claim", "under the reserved .test domain"},
		{"AUTO_PLANE_URL", "https://10.0.0.1/claim", "under the reserved .test domain"},
		{"AUTO_DEADMAN_URL", "https://hc-ping.example/x", "refuses AUTO_DEADMAN_URL"},
	} {
		saved, had := env[tc.k]
		env[tc.k] = tc.v
		if _, err := loadEnv(get); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s=%q: %v, want %q", tc.k, tc.v, err, tc.want)
		}
		if had {
			env[tc.k] = saved
		} else {
			delete(env, tc.k)
		}
	}
	// Off, the same production values load exactly as before.
	delete(env, "AUTO_TEST_ENDPOINTS")
	env["DISCORD_AUTO_TOKEN"], env["AUTO_PLANE_URL"] = realTok, "https://bot.clippet.gg/claim"
	if c, err := loadEnv(get); err != nil || c.testEndpoints {
		t.Fatalf("knob off: %+v %v", c, err)
	}
	// The WARN, first, and only when on.
	var buf bytes.Buffer
	announce(newLogger(&buf, "json"), envConfig{testEndpoints: true, listen: ":9091", logFormat: "json"})
	first := strings.SplitN(buf.String(), "\n", 2)[0]
	if !strings.Contains(first, `"level":"WARN"`) || !strings.Contains(first, testEndpointsWarn) {
		t.Fatalf("the first line is not the knob's WARN: %s", first)
	}
	buf.Reset()
	announce(newLogger(&buf, "json"), envConfig{listen: ":9091", logFormat: "json"})
	if strings.Contains(buf.String(), "WARN") || strings.Contains(buf.String(), "AUTO_TEST_ENDPOINTS") {
		t.Fatalf("the knob's line without the knob: %s", buf.String())
	}
}

// Pin: the token shape the guard refuses — a Discord token's first segment is
// the base64 of the bot's user id.
func TestDiscordTokenShape(t *testing.T) {
	id := "222222222222222222"
	for tok, want := range map[string]bool{
		base64.RawStdEncoding.EncodeToString([]byte(id)) + ".GhIjKl.abcdefghijklmnopqrstuvwxyz0123": true,
		base64.StdEncoding.EncodeToString([]byte(id)) + ".GhIjKl.abcdefghijklmnopqrstuvwxyz0123":    true,
		base64.RawURLEncoding.EncodeToString([]byte(id)) + ".x.y":                                   true,
		base64.RawStdEncoding.EncodeToString([]byte("abcdefghijklmnopq")) + ".x.y":                  false,
		base64.RawStdEncoding.EncodeToString([]byte("1234")) + ".x.y":                               false,
		base64.RawStdEncoding.EncodeToString([]byte(id)) + ".x":                                     false,
		base64.RawStdEncoding.EncodeToString([]byte(id)) + "..y":                                    false,
		"e2e-autohost-token.not.discord":                                                            false,
		"tok.SECRET":                                                                                false,
		"managed":                                                                                   false,
		"":                                                                                          false,
	} {
		if got := discordTokenShape(tok); got != want {
			t.Fatalf("discordTokenShape(%q) = %v, want %v", tok, got, want)
		}
	}
}
