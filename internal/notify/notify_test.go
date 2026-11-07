package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"clippet/internal/urls"
)

// token is the secret half of every endpoint below: no Result, no error and
// no rendered string this package produces may contain it.
const token = "T0KEN-must-never-appear-anywhere"

func base(t *testing.T) urls.Builder {
	t.Helper()
	u, err := url.Parse("https://clips.example")
	if err != nil {
		t.Fatal(err)
	}
	return urls.New(u, "")
}

// newTestNotifier points a Notifier at srv with a webhook-shaped path. The
// host allowlist lives in config (and is tested there), which is exactly why
// Deps takes a plain endpoint: these tests never touch the network.
func newTestNotifier(t *testing.T, srv *httptest.Server, timeout time.Duration) *Notifier {
	t.Helper()
	return New(Deps{Endpoint: srv.URL + "/api/webhooks/1234567890/" + token, URLs: base(t), Timeout: timeout})
}

// recorder is one httptest server that records what it was sent and answers
// with whatever the test set. The recorded slices are read from the test
// goroutine after the handler ran on another, so they are mutex-guarded (the
// -race pass is part of the gate).
type recorder struct {
	srv     *httptest.Server
	mu      sync.Mutex
	got     []sent
	status  int
	body    string
	headers map[string]string
	// hang holds every answer until the CLIENT gives up (its per-attempt
	// timeout closes the connection, which cancels the request context): a
	// server that never answers, with no sleep.
	hang bool
}

type sent struct {
	method, query, contentType, userAgent, body string
}

func newRecorder(t *testing.T, status int, body string) *recorder {
	t.Helper()
	r := &recorder{status: status, body: body, headers: map[string]string{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, sent{method: req.Method, query: req.URL.RawQuery,
			contentType: req.Header.Get("Content-Type"), userAgent: req.Header.Get("User-Agent"), body: string(b)})
		hang, status, body, headers := r.hang, r.status, r.body, r.headers
		r.mu.Unlock()
		if hang {
			<-req.Context().Done()
			return
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *recorder) sent() []sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sent(nil), r.got...)
}

func TestUnsetEndpointConstructsNothing(t *testing.T) {
	if n := New(Deps{URLs: base(t)}); n != nil {
		t.Fatalf("New with no endpoint = %+v, want nil (no http.Client, ever)", n)
	}
}

// TestDeliverSuccess: a 2xx is done, and the request on the wire is the
// contract — POST ?wait=true, JSON, allowed_mentions.parse = [].
func TestDeliverSuccess(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
	}{{200, `{"id":"1"}`}, {204, ""}} {
		rec := newRecorder(t, c.status, c.body)
		n := newTestNotifier(t, rec.srv, 0)
		res := n.Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "My clip")
		if !res.OK || res.Retry || res.Status != c.status {
			t.Fatalf("status %d: %+v", c.status, res)
		}
		reqs := rec.sent()
		if len(reqs) != 1 {
			t.Fatalf("status %d: %d requests", c.status, len(reqs))
		}
		req := reqs[0]
		if req.method != http.MethodPost {
			t.Errorf("method = %s", req.method)
		}
		if req.query != "wait=true" {
			t.Errorf("query = %q, want wait=true", req.query)
		}
		if req.contentType != "application/json" {
			t.Errorf("Content-Type = %q", req.contentType)
		}
		if req.userAgent != userAgent {
			t.Errorf("User-Agent = %q", req.userAgent)
		}
		var got payload
		if err := json.Unmarshal([]byte(req.body), &got); err != nil {
			t.Fatalf("body %q: %v", req.body, err)
		}
		if got.Content != "My clip\nhttps://clips.example/aZ3kQ9xB2mLp" {
			t.Errorf("content = %q", got.Content)
		}
		if got.AllowedMentions.Parse == nil || len(got.AllowedMentions.Parse) != 0 {
			t.Errorf("allowed_mentions.parse = %#v, want an empty list", got.AllowedMentions.Parse)
		}
		// `null` would mean "the default", i.e. everything may be pinged.
		if !strings.Contains(req.body, `"allowed_mentions":{"parse":[]}`) {
			t.Errorf("raw body must carry parse:[] verbatim: %s", req.body)
		}
	}
}

// TestDeliverClassification is the whole retry/permanent table.
func TestDeliverClassification(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		body           string
		retryAfter     string
		wantOK         bool
		wantRetry      bool
		wantRetryAfter time.Duration
		wantDetail     string
	}{
		{name: "200", status: 200, wantOK: true},
		{name: "201", status: 201, wantOK: true},
		{name: "204", status: 204, wantOK: true},
		{name: "429 without Retry-After", status: 429, wantRetry: true, wantDetail: "rate limited: HTTP 429"},
		{name: "429 seconds", status: 429, retryAfter: "42", wantRetry: true, wantRetryAfter: 42 * time.Second, wantDetail: "rate limited"},
		{name: "429 fractional", status: 429, retryAfter: "1.5", wantRetry: true, wantRetryAfter: 1500 * time.Millisecond},
		{name: "429 absurd is capped", status: 429, retryAfter: "999999", wantRetry: true, wantRetryAfter: maxRetryAfter},
		{name: "429 garbage", status: 429, retryAfter: "soon", wantRetry: true},
		{name: "500", status: 500, body: "oops", wantRetry: true, wantDetail: "server error: HTTP 500"},
		{name: "503", status: 503, wantRetry: true, wantDetail: "server error: HTTP 503"},
		{name: "400", status: 400, body: `{"message":"Cannot send an empty message","code":50006}`, wantDetail: "rejected: HTTP 400"},
		{name: "401", status: 401, wantDetail: "rejected: HTTP 401"},
		{name: "403", status: 403, wantDetail: "rejected: HTTP 403"},
		{name: "404 unknown webhook", status: 404, body: `{"message":"Unknown Webhook","code":10015}`, wantDetail: "rejected: HTTP 404"},
	}
	for _, c := range cases {
		rec := newRecorder(t, c.status, c.body)
		if c.retryAfter != "" {
			rec.headers["Retry-After"] = c.retryAfter
		}
		n := newTestNotifier(t, rec.srv, 0)
		res := n.Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "t")
		if res.OK != c.wantOK || res.Retry != c.wantRetry || res.Status != c.status {
			t.Errorf("%s: %+v", c.name, res)
		}
		if res.RetryAfter != c.wantRetryAfter {
			t.Errorf("%s: RetryAfter = %s, want %s", c.name, res.RetryAfter, c.wantRetryAfter)
		}
		if c.wantDetail != "" && !strings.Contains(res.Detail, c.wantDetail) {
			t.Errorf("%s: Detail = %q, want it to contain %q", c.name, res.Detail, c.wantDetail)
		}
		if c.body != "" && !c.wantOK && !strings.Contains(res.Detail, c.body) {
			t.Errorf("%s: Detail = %q, want the response body captured", c.name, res.Detail)
		}
	}
}

// TestRejectionBodyIsBounded: at most bodySnippet bytes reach the job row.
func TestRejectionBodyIsBounded(t *testing.T) {
	rec := newRecorder(t, 400, strings.Repeat("x", 4000))
	n := newTestNotifier(t, rec.srv, 0)
	res := n.Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "t")
	if res.OK || res.Retry {
		t.Fatalf("%+v", res)
	}
	if len(res.Detail) > bodySnippet+80 {
		t.Fatalf("Detail is %d bytes, want ≈%d: %q", len(res.Detail), bodySnippet, res.Detail)
	}
}

// TestRedirectIsNotFollowed: a 3xx is permanent and the redirect target is
// never contacted — the host allowlist would be worthless otherwise.
func TestRedirectIsNotFollowed(t *testing.T) {
	elsewhere := newRecorder(t, 200, "")
	rec := newRecorder(t, http.StatusFound, "")
	rec.headers["Location"] = elsewhere.srv.URL + "/api/webhooks/9/other"
	n := newTestNotifier(t, rec.srv, 0)
	res := n.Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "t")
	if res.OK || res.Retry || res.Status != http.StatusFound {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(res.Detail, "redirect is not followed") {
		t.Errorf("Detail = %q", res.Detail)
	}
	if n := len(elsewhere.sent()); n != 0 {
		t.Fatalf("the redirect target was contacted %d time(s)", n)
	}
}

// TestTimeoutIsRetryable: a server that never answers within the per-attempt
// budget is a retry, and the error names no URL.
func TestTimeoutIsRetryable(t *testing.T) {
	rec := newRecorder(t, 200, "")
	rec.hang = true
	n := newTestNotifier(t, rec.srv, 50*time.Millisecond)
	res := n.Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "t")
	if res.OK || !res.Retry || res.Status != 0 {
		t.Fatalf("%+v", res)
	}
	if strings.Contains(res.Detail, token) || strings.Contains(res.Detail, rec.srv.URL) {
		t.Fatalf("the timeout error leaks the endpoint: %q", res.Detail)
	}
}

// TestNetworkErrorIsRetryable uses a server that is already closed, so the
// dial fails locally — no network, no flakiness.
func TestNetworkErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	n := New(Deps{Endpoint: srv.URL + "/api/webhooks/1/" + token, URLs: base(t)})
	srv.Close()
	res := n.Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "t")
	if res.OK || !res.Retry {
		t.Fatalf("%+v", res)
	}
	if strings.Contains(res.Detail, token) {
		t.Fatalf("the dial error leaks the token: %q", res.Detail)
	}
}

// TestCancelledContextIsRetryable: shutdown mid-attempt is not a rejection.
func TestCancelledContextIsRetryable(t *testing.T) {
	rec := newRecorder(t, 200, "")
	n := newTestNotifier(t, rec.srv, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := n.Deliver(ctx, "", "aZ3kQ9xB2mLp", "t")
	if res.OK || !res.Retry {
		t.Fatalf("%+v", res)
	}
	if strings.Contains(res.Detail, token) {
		t.Fatalf("leak: %q", res.Detail)
	}
}

// TestEndpointNeverEscapes: whatever the server says, the token must not come
// back out — not through a body echo, not through an error.
func TestEndpointNeverEscapes(t *testing.T) {
	rec := newRecorder(t, 400, "your url is https://x/api/webhooks/1234567890/"+token+" which is wrong")
	n := newTestNotifier(t, rec.srv, 0)
	res := n.Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "t")
	if strings.Contains(res.Detail, token) {
		t.Fatalf("an echoed body leaked the token: %q", res.Detail)
	}
	if !strings.Contains(res.Detail, redacted) {
		t.Fatalf("Detail = %q, want the token replaced", res.Detail)
	}
}

// TestTokenStraddlingTheSnippetCutIsStillScrubbed: the body is cut to
// bodySnippet bytes for the log, but the scrub runs FIRST, on a wider window.
// Cutting first would leave the token's prefix in the output, where nothing
// can match it any more.
func TestTokenStraddlingTheSnippetCutIsStillScrubbed(t *testing.T) {
	// Land the token across the 200-byte boundary: it starts at byte 190.
	body := strings.Repeat("x", 190) + token + " trailing"
	rec := newRecorder(t, 400, body)
	n := newTestNotifier(t, rec.srv, 0)
	res := n.Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "t")
	for i := len(token); i >= 8; i-- {
		if strings.Contains(res.Detail, token[:i]) {
			t.Fatalf("a token straddling the snippet cut survived as %q: %s", token[:i], res.Detail)
		}
	}
	if !strings.Contains(res.Detail, redacted) {
		t.Fatalf("Detail = %q, want the token replaced", res.Detail)
	}
	if len(res.Detail) > bodySnippet+80 {
		t.Fatalf("Detail is %d bytes, want ≈%d", len(res.Detail), bodySnippet)
	}
}

// TestEscapedTokenIsScrubbed: net/url normalises what it puts on the wire, so
// a token that has to be percent-encoded in a path comes BACK in its escaped
// spelling — which a ReplaceAll of the configured spelling does not match.
// config rejects such a token now (§20 erratum 63), which is why this is a
// unit test and not a live path; Deps takes a plain string and cannot rely on
// the caller having validated it.
func TestEscapedTokenIsScrubbed(t *testing.T) {
	const raw = "tok en"
	wire := url.PathEscape(raw) // "tok%20en" — what the far end actually sees
	if wire == raw {
		t.Fatal("precondition: the token must be one net/url re-escapes")
	}
	// The far end echoes the request URL back the way it received it.
	rec := newRecorder(t, 400, "")
	endpoint := rec.srv.URL + "/api/webhooks/1234567890/" + raw
	rec.body = "your url is " + rec.srv.URL + "/api/webhooks/1234567890/" + wire + "?wait=true which is wrong"
	n := New(Deps{Endpoint: endpoint, URLs: base(t)})
	res := n.Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "t")
	if strings.Contains(res.Detail, raw) || strings.Contains(res.Detail, wire) {
		t.Fatalf("an echoed body leaked the escaped token: %q", res.Detail)
	}
	if !strings.Contains(res.Detail, redacted) {
		t.Fatalf("Detail = %q, want the token replaced", res.Detail)
	}
	for _, spelling := range []string{raw, wire, url.QueryEscape(raw), endpoint, endpoint + waitQuery} {
		got := n.scrub("far end says: " + spelling + " is wrong")
		if strings.Contains(got, raw) || strings.Contains(got, wire) {
			t.Errorf("scrub(%q) = %q", spelling, got)
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("scrub(%q) = %q, want the secret replaced", spelling, got)
		}
	}
	// The ?wait=true form is what the request actually carries, so an echo of
	// the whole request URL must not leak either.
	if got := n.scrub("POST " + endpoint + waitQuery + " failed"); strings.Contains(got, raw) {
		t.Errorf("the ?wait=true form leaked: %q", got)
	}
	// Longest first: the replacement of the short token must not eat half of
	// the endpoint and leave the rest of it standing.
	if got := n.scrub(endpoint); got != redacted {
		t.Errorf("scrub(endpoint) = %q, want exactly %q", got, redacted)
	}
}

// TestContent pins the payload text: one line of title, one line of URL, no
// mention syntax honoured, no markdown of any kind honoured, no control
// characters.
func TestContent(t *testing.T) {
	const page = "https://clips.example/aZ3kQ9xB2mLp"
	cases := []struct{ name, title, want string }{
		{"plain", "My best clip", "My best clip\n" + page},
		{"empty title", "", page},
		{"blank title", "   ", page},
		{"newline becomes a space", "line1\nline2", "line1 line2\n" + page},
		{"tab and CR too", "a\tb\rc", "a b c\n" + page},
		{"other control characters", "a\x00\x07b", "a  b\n" + page},
		{"header is escaped", "# gg", `\# gg` + "\n" + page},
		{"quote is escaped", "> quoted", `\> quoted` + "\n" + page},
		{"block quote is escaped", ">>> quoted", `\>\>\> quoted` + "\n" + page},
		{"bullet is escaped", "- item", `\- item` + "\n" + page},
		{"star bullet is escaped", "* item", `\* item` + "\n" + page},
		{"code fence is escaped", "```js", "\\`\\`\\`js\n" + page},
		{"an interior # is escaped too", "clip #4", `clip \#4` + "\n" + page},
		{"a mention is left to allowed_mentions", "@everyone gg", "@everyone gg\n" + page},

		// The masked link: `[text](url)` renders as a link labelled `text` in
		// WEBHOOK-authored content, so an unescaped '[' lets a title lie about
		// where it points, under the operator's identity.
		{"masked link", "[click](https://evil.example)", `\[click\](https://evil.example)` + "\n" + page},
		{"masked link mid-title", "gg [click](https://evil.example) wp", `gg \[click\](https://evil.example) wp` + "\n" + page},
		{"a backslash cannot re-open the bracket", `\[click](https://evil.example)`, `\\\[click\](https://evil.example)` + "\n" + page},
		{"a lone trailing backslash is doubled", `gg\`, `gg\\` + "\n" + page},
		{"spoiler", "||hidden||", `\|\|hidden\|\|` + "\n" + page},
		{"bold", "**bold**", `\*\*bold\*\*` + "\n" + page},
		{"backtick span", "a `code` b", "a \\`code\\` b\n" + page},
		{"underscore and tilde", "a_b~c", `a\_b\~c` + "\n" + page},
		{"angle-bracketed URL", "<https://x.example>", `<https://x.example\>` + "\n" + page},
		{"a hyphen mid-title is left alone", "my-best-clip", "my-best-clip\n" + page},

		// Invisible characters have no place in a one-line title: a bidi
		// override reverses what the line reads as, and a zero-width character
		// rides along invisibly into every copy of the message.
		{"RTL override", "clip\u202egpj.exe", "clip gpj.exe\n" + page},
		{"RTL/LTR marks and isolates", "a\u200fb\u2066c\u2069d", "a b c d\n" + page},
		{"zero-width space and joiner", "a\u200bb\u200dc", "a b c\n" + page},
		{"BOM", "a\ufeffb", "a b\n" + page},
	}
	for _, c := range cases {
		if got := Content(c.title, page); got != c.want {
			t.Errorf("%s: Content(%q) = %q, want %q", c.name, c.title, got, c.want)
		}
	}
	// The 2000-rune limit is enforced by trimming the TITLE; the URL survives.
	long := Content(strings.Repeat("é", 5000), page)
	if n := len([]rune(long)); n != maxContent {
		t.Fatalf("long content is %d runes, want %d", n, maxContent)
	}
	if !strings.HasSuffix(long, "\n"+page) {
		t.Fatal("the share URL must survive the trim")
	}
	// A URL longer than the whole budget leaves the URL alone.
	if got := Content("title", strings.Repeat("u", maxContent+10)); !strings.HasPrefix(got, "uuu") {
		t.Fatalf("oversized URL: %q", got[:20])
	}
}

// TestContentClampIsCountedAfterEscaping: escaping ADDS runes, so a title that
// fits before it can overflow after it. The clamp runs last, never cuts the
// URL, and never ends the title on a dangling backslash — which would escape
// the newline and pull the URL onto the title's line.
func TestContentClampIsCountedAfterEscaping(t *testing.T) {
	const page = "https://clips.example/aZ3kQ9xB2mLp"
	titles := []string{
		strings.Repeat("[", 1999),          // 1 rune in, 2 runes out, every one of them
		strings.Repeat("[", maxContent*2),  //
		strings.Repeat(`\`, 1999),          // the escape character itself
		strings.Repeat("a[", 1500),         // the cut lands mid-pair on odd offsets
		strings.Repeat("|", 1999) + "tail", //
		strings.Repeat("é", 1999) + "[",    // multi-byte runes before the pair
	}
	for i, title := range titles {
		got := Content(title, page)
		if n := utf8.RuneCountInString(got); n > maxContent {
			t.Fatalf("title %d: content is %d runes, want ≤ %d", i, n, maxContent)
		}
		if !strings.HasSuffix(got, "\n"+page) {
			t.Fatalf("title %d: the share URL must survive the clamp: %q", i, got[max(0, len(got)-80):])
		}
		head := strings.TrimSuffix(got, "\n"+page)
		slashes := len(head) - len(strings.TrimRight(head, `\`))
		if slashes%2 == 1 {
			t.Fatalf("title %d: the clamped title ends on a dangling backslash, which escapes the newline: %q", i, head[max(0, len(head)-20):])
		}
	}
	// The clamp still bites: 1999 '[' cannot fit in what is left of 2000 runes
	// once each one is two runes wide.
	got := Content(strings.Repeat("[", 1999), page)
	if n := strings.Count(got, "["); n >= 1999 {
		t.Fatalf("nothing was clamped: %d brackets survived", n)
	}
	if n := utf8.RuneCountInString(got); n < maxContent-2 {
		t.Fatalf("the clamp left %d runes on the table", maxContent-n)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"-5", 0},
		{"3", 3 * time.Second},
		{" 3 ", 3 * time.Second},
		{"0.25", 250 * time.Millisecond},
		{"0.5", 500 * time.Millisecond},
		{"-1", 0},
		{"100000", maxRetryAfter},
		// Every one of these used to reach time.Duration(secs * 1e9) and come
		// back NEGATIVE (the conversion is implementation-defined once the
		// float is out of int64 range), which then rendered into a WARN line
		// and into jobs.last_error and became a "floor" under the backoff.
		{"1e10", maxRetryAfter},       // > 9.2e9 s: overflows a Duration
		{"4000000000", maxRetryAfter}, // 4e9 s: fits an int64 of seconds, not of nanoseconds
		{"1e300", maxRetryAfter},
		{"Inf", maxRetryAfter},
		{"+Inf", maxRetryAfter},
		{"-Inf", 0},
		{"NaN", 0}, // fails every comparison, including `<= 0`
		{"tomorrow", 0},
		{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	}
	for _, c := range cases {
		got := retryAfter(c.in)
		if got != c.want {
			t.Errorf("retryAfter(%q) = %s, want %s", c.in, got, c.want)
		}
		// The invariant behind the table: whatever a hostile far end sends,
		// what leaves this function is a sane, non-negative, capped delay.
		if got < 0 || got > maxRetryAfter {
			t.Errorf("retryAfter(%q) = %s, outside [0, %s]", c.in, got, maxRetryAfter)
		}
	}
	// An HTTP-date in the future is honoured and capped.
	if got := retryAfter(time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)); got <= 0 || got > 2*time.Minute {
		t.Errorf("HTTP-date Retry-After = %s", got)
	}
	if got := retryAfter(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)); got != maxRetryAfter {
		t.Errorf("far-future HTTP-date = %s, want the cap", got)
	}
}

func TestCauseStripsTheURL(t *testing.T) {
	wrapped := &url.Error{Op: "Post", URL: "https://discord.com/api/webhooks/1/" + token, Err: errTest{}}
	if got := cause(wrapped); got != "boom" {
		t.Fatalf("cause = %q, want the inner error", got)
	}
	if !strings.Contains(wrapped.Error(), token) {
		t.Fatal("precondition: *url.Error is supposed to embed the URL")
	}
}

type errTest struct{}

func (errTest) Error() string { return "boom" }

// TestTokenCutAtTheWindowEdgeIsStruck: a receiver that misbehaves on purpose.
// The read window ends inside a token — after a run of leading whitespace the
// trim removes, or as the last of many echoes the scrub shrinks — so the token's
// cut-off prefix would land inside the snippet where no whole-token scrub can
// match it (run 8, the hook lane's residual gaps).
func TestTokenCutAtTheWindowEdgeIsStruck(t *testing.T) {
	for name, body := range map[string]string{
		"leading whitespace": strings.Repeat(" ", bodyWindow-10) + token + " trailing",
		"repeated echoes":    "x" + strings.Repeat(token, 30),
	} {
		rec := newRecorder(t, 400, body)
		n := newTestNotifier(t, rec.srv, 0)
		res := n.Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "t")
		for i := len(token); i >= 6; i-- {
			if strings.Contains(res.Detail, token[:i]) {
				t.Fatalf("%s: a cut-off token survived as %q: %s", name, token[:i], res.Detail)
			}
		}
	}
	// An honest body that fits the window is never struck at its end.
	rec := newRecorder(t, 400, "missing T0KEN")
	res := newTestNotifier(t, rec.srv, 0).Deliver(context.Background(), "", "aZ3kQ9xB2mLp", "t")
	if !strings.Contains(res.Detail, "missing T0KEN") {
		t.Fatalf("a body inside the window was struck: %s", res.Detail)
	}
}
