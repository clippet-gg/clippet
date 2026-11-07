package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The clip webhook (ROADMAP #54, §20 erratum 319).

const (
	hookToken  = "hk-T0KEN-in-the-query-1234567890"
	hookSecret = "s3cret-s3cret-s3cret-s3cret-s3cret-0123"
)

type hookCapture struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	hdrs   []http.Header
	status int
	body   string
	// hang: as the recorder's, the answer waits for the client to give up.
	hang   bool
	header map[string]string
}

func newHookCapture(t *testing.T, status int, body string) *hookCapture {
	t.Helper()
	c := &hookCapture{status: status, body: body, header: map[string]string{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, b)
		c.hdrs = append(c.hdrs, r.Header.Clone())
		status, body, hang, hdr := c.status, c.body, c.hang, c.header
		c.mu.Unlock()
		if hang {
			<-r.Context().Done()
			return
		}
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *hookCapture) endpoint() string { return c.srv.URL + "/in?k=" + hookToken }

func newTestHook(t *testing.T, endpoint string, timeout time.Duration) *Hook {
	t.Helper()
	h := NewHook(HookDeps{Endpoint: endpoint, Secret: hookSecret, URLs: base(t), Timeout: timeout})
	if h == nil {
		t.Fatal("NewHook returned nil for a configured pair")
	}
	return h
}

func i64(v int64) *int64 { return &v }

func testEvent() ClipEvent {
	return ClipEvent{ID: "Z6eQucmYEyFQ", Title: `ace <on> "the" last & round`, Owner: "carol",
		DurationMS: i64(6000), CreatedAt: 1791264797, ExpiresAt: nil, Unlisted: false}
}

func TestHookUnsetConstructsNothing(t *testing.T) {
	if NewHook(HookDeps{}) != nil {
		t.Fatal("an empty endpoint built a Hook")
	}
	if NewHook(HookDeps{Endpoint: "http://h.example/x"}) != nil {
		t.Fatal("a Hook was built with no key: it would send unsigned documents")
	}
}

// TestHookBodyIsTheContract: exactly the nine fields, in order, with the
// public API's time layout and nulls where the row has none.
func TestHookBodyIsTheContract(t *testing.T) {
	h := newTestHook(t, "http://h.example/in", 0)
	b, err := h.Body(testEvent())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"event":"clip.ready","id":"Z6eQucmYEyFQ","url":"https://clips.example/Z6eQucmYEyFQ","title":"ace <on> \"the\" last & round","duration_ms":6000,"owner":"carol","created_at":"2026-10-06T05:33:17Z","expires_at":null,"unlisted":false}`
	if string(b) != want {
		t.Fatalf("body\n got %s\nwant %s", b, want)
	}
	ev := testEvent()
	ev.DurationMS, ev.ExpiresAt, ev.Unlisted = nil, i64(1791869597), true
	b, _ = h.Body(ev)
	for _, frag := range []string{`"duration_ms":null`, `"expires_at":"2026-10-13T05:33:17Z"`, `"unlisted":true`} {
		if !bytes.Contains(b, []byte(frag)) {
			t.Fatalf("body lacks %s: %s", frag, b)
		}
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil || len(keys) != 9 {
		t.Fatalf("body has %d keys (%v), want exactly 9", len(keys), err)
	}
}

// TestHookDeliverSignsTheExactBytes: the headers, and a signature a receiver
// recomputes over the bytes it received; another key does not verify.
func TestHookDeliverSignsTheExactBytes(t *testing.T) {
	c := newHookCapture(t, 200, "ok")
	res := newTestHook(t, c.endpoint(), 0).Deliver(context.Background(), testEvent())
	if !res.OK || res.Status != 200 || res.Detail != "delivered: HTTP 200" {
		t.Fatalf("res = %+v", res)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) != 1 {
		t.Fatalf("%d requests", len(c.bodies))
	}
	hd, body := c.hdrs[0], c.bodies[0]
	if hd.Get("Content-Type") != "application/json" || hd.Get("User-Agent") != "Clippet" {
		t.Fatalf("headers = %v", hd)
	}
	sig := hd.Get("X-Clippet-Signature")
	m := hmac.New(sha256.New, []byte(hookSecret))
	m.Write(body)
	if want := "sha256=" + hex.EncodeToString(m.Sum(nil)); sig != want {
		t.Fatalf("signature %q, want %q", sig, want)
	}
	m = hmac.New(sha256.New, []byte("another-key-another-key-another-key!"))
	m.Write(body)
	if sig == "sha256="+hex.EncodeToString(m.Sum(nil)) {
		t.Fatal("the signature verifies with another key")
	}
	if len(sig) != len("sha256=")+64 || strings.ToLower(sig) != sig {
		t.Fatalf("signature is not 64 lowercase hex: %q", sig)
	}
	if bytes.Contains(body, []byte(hookToken)) || bytes.Contains(body, []byte(hookSecret)) {
		t.Fatalf("the body carries the URL's token or the key: %s", body)
	}
}

func TestHookClassification(t *testing.T) {
	cases := []struct {
		status    int
		ok, retry bool
		detail    string
	}{
		{204, true, false, "delivered: HTTP 204"},
		{400, false, false, "rejected: HTTP 400"},
		{404, false, false, "rejected: HTTP 404"},
		{301, false, false, "rejected: HTTP 301 (a redirect is not followed)"},
		{429, false, true, "rate limited: HTTP 429 retry_after=7s"},
		{500, false, true, "server error: HTTP 500"},
		{503, false, true, "server error: HTTP 503"},
	}
	for _, tc := range cases {
		c := newHookCapture(t, tc.status, "nope")
		if tc.status == 429 {
			c.header["Retry-After"] = "7"
		}
		if tc.status == 301 {
			c.header["Location"] = "/elsewhere"
		}
		res := newTestHook(t, c.endpoint(), 0).Deliver(context.Background(), testEvent())
		if res.OK != tc.ok || res.Retry != tc.retry || res.Status != tc.status || !strings.HasPrefix(res.Detail, tc.detail) {
			t.Errorf("%d: %+v", tc.status, res)
		}
		c.mu.Lock()
		n := len(c.bodies)
		c.mu.Unlock()
		if n != 1 {
			t.Errorf("%d: %d requests reached the receiver (a redirect must not be followed)", tc.status, n)
		}
	}
}

func TestHookTimeoutAndRefusalAreRetryable(t *testing.T) {
	c := newHookCapture(t, 200, "")
	c.hang = true
	res := newTestHook(t, c.endpoint(), 50*time.Millisecond).Deliver(context.Background(), testEvent())
	if !res.Retry || res.OK || strings.Contains(res.Detail, hookToken) {
		t.Fatalf("timeout: %+v", res)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	res = newTestHook(t, "http://"+addr+"/in?k="+hookToken, 0).Deliver(context.Background(), testEvent())
	if !res.Retry || res.OK || !strings.HasPrefix(res.Detail, "delivery failed: ") || strings.Contains(res.Detail, hookToken) || strings.Contains(res.Detail, addr+"/in") {
		t.Fatalf("refused: %+v", res)
	}
}

// TestHookScrubsEverySpellingOfTheURL: a receiver that echoes the whole URL,
// its query and its token gets every one of them struck from Detail.
func TestHookScrubsEverySpellingOfTheURL(t *testing.T) {
	c := newHookCapture(t, 400, "")
	ep := c.endpoint()
	c.body = "bad url " + ep + " query k=" + hookToken + " token " + hookToken + " key " + hookSecret
	res := newTestHook(t, ep, 0).Deliver(context.Background(), testEvent())
	for _, leak := range []string{ep, hookToken, hookSecret, "k=" + hookToken} {
		if strings.Contains(res.Detail, leak) {
			t.Fatalf("Detail leaks %q: %s", leak, res.Detail)
		}
	}
	if !strings.Contains(res.Detail, "bad url") || !strings.Contains(res.Detail, "token") {
		t.Fatalf("Detail lost the honest text: %s", res.Detail)
	}
}

// TestHookShortPartsAreNotNeedles: the fixture's own path `/in` is a word an
// honest line contains; only the whole URL and the long parts are struck.
func TestHookShortPartsAreNotNeedles(t *testing.T) {
	n := hookNeedles("http://127.0.0.1:9/in?k="+hookToken, hookSecret)
	for _, s := range n {
		if s == "/in" || s == "in" || s == "/" || len(s) < minHookNeedle {
			t.Fatalf("needle %q is shorter than the floor: %v", s, n)
		}
	}
	c := newHookCapture(t, 404, "no route /in here; try /index")
	res := newTestHook(t, c.srv.URL+"/in", 0).Deliver(context.Background(), testEvent())
	if !strings.Contains(res.Detail, "no route /in here; try /index") {
		t.Fatalf("honest text was struck: %s", res.Detail)
	}
	// A long token echoed ALONE — without the URL around it — is struck.
	long := "segment-token-0123456789abcdef"
	c = newHookCapture(t, 404, "unknown "+long)
	res = newTestHook(t, c.srv.URL+"/hooks/"+long, 0).Deliver(context.Background(), testEvent())
	if strings.Contains(res.Detail, long) || !strings.Contains(res.Detail, "unknown") {
		t.Fatalf("a long path token was not struck: %s", res.Detail)
	}
}

// TestHookLongURLStraddlingTheCutIsScrubbed: the URL has no length cap, so the
// read window grows with it (bodyWindow's reason).
func TestHookLongURLStraddlingTheCutIsScrubbed(t *testing.T) {
	c := newHookCapture(t, 400, "")
	ep := c.srv.URL + "/in?k=" + strings.Repeat("x", 1500)
	c.body = strings.Repeat("a", bodySnippet-60) + ep
	res := newTestHook(t, ep, 0).Deliver(context.Background(), testEvent())
	if strings.Contains(res.Detail, "xxxxxxxx") {
		t.Fatalf("a part of the URL survived the cut: %s", res.Detail)
	}
}

// TestHookURLCutAtTheWindowEdgeIsStruck: a receiver that misbehaves on
// purpose, the Notifier's two residual gaps on the hook's window (run 8): a
// run of leading whitespace the trim removes after the read, and an echo
// repeated so the scrub shrinks the text and the cut-off last echo moves into
// the snippet.
func TestHookURLCutAtTheWindowEdgeIsStruck(t *testing.T) {
	c := newHookCapture(t, 400, "")
	ep := c.srv.URL + "/in?k=" + strings.Repeat("x", 1500)
	for name, body := range map[string]string{
		"leading whitespace": strings.Repeat(" ", 400) + ep,
		"repeated echoes":    ep + ep + ep,
	} {
		c.mu.Lock()
		c.body = body
		c.mu.Unlock()
		res := newTestHook(t, ep, 0).Deliver(context.Background(), testEvent())
		if strings.Contains(res.Detail, "xxxxxxxx") || strings.Contains(res.Detail, "/in?k=") {
			t.Fatalf("%s: a cut-off URL survived: %s", name, res.Detail)
		}
	}
}
