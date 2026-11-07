package notify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fetch measures itself and the person's line counts the bytes (§20 erratum
// 340). The CDN and Discord's API are the fixture's httptest servers; the
// count's ticker is a channel the test feeds, so the five-second cadence is
// pinned without waiting five seconds: an edit can only follow a tick, and the
// watcher asks for exactly botProgressEvery.

// fakeTicks swaps the bot's ticker for an unbuffered channel. A send that
// completes means the watcher took that tick, so the previous tick's edit has
// finished (the watcher edits on its own loop, one at a time).
type fakeTicks struct {
	ch chan time.Time
	mu sync.Mutex
	d  []time.Duration
}

func installTicks(f *botFixture) *fakeTicks {
	ft := &fakeTicks{ch: make(chan time.Time)}
	f.bot.tick = func(d time.Duration) (<-chan time.Time, func()) {
		ft.mu.Lock()
		ft.d = append(ft.d, d)
		ft.mu.Unlock()
		return ft.ch, func() {}
	}
	return ft
}

func (ft *fakeTicks) asked() []time.Duration {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return append([]time.Duration(nil), ft.d...)
}

func (ft *fakeTicks) send(t *testing.T) {
	t.Helper()
	select {
	case ft.ch <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never took the tick")
	}
}

// fetchLines are the `discord fetch` lines of a log.
func fetchLines(logs string) []string {
	var out []string
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, `msg="discord fetch"`) {
			out = append(out, l)
		}
	}
	return out
}

// TestFetchLineOn200And206: one line per fetch, its fields, the Range asked,
// and never the URL. A 206 streams exactly as a 200; a 206 that does not start
// at the first byte is refused like any other status, and still logs its line.
func TestFetchLineOn200And206(t *testing.T) {
	cases := []struct {
		name, contentRange, acceptRanges string
		status                           int
		ok                               bool
	}{
		{"200", "", "", http.StatusOK, true},
		{"206", "bytes 0-10/11", "bytes", http.StatusPartialContent, true},
		{"206 elsewhere", "bytes 5-10/11", "bytes", http.StatusPartialContent, false},
		{"403", "", "", http.StatusForbidden, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBotFixture(t, http.StatusOK, "unused")
			var rng string
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rng = r.Header.Get("Range")
				if tc.acceptRanges != "" {
					w.Header().Set("Accept-Ranges", tc.acceptRanges)
				}
				if tc.contentRange != "" {
					w.Header().Set("Content-Range", tc.contentRange)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "video bytes")
			}))
			t.Cleanup(cdn.Close)
			var body []byte
			job := okJob(f, func(_ context.Context, src IngestSource) (IngestResult, error) {
				body, _ = io.ReadAll(src.Body)
				return IngestResult{ClipID: "abc123XYZ789", CardReady: true}, nil
			})
			job.URL = cdn.URL + "/" + testAttachment + "?ex=1&is=2&hm=sig"
			_, err := f.bot.fetchAndIngest(context.Background(), job, nil)
			if rng != "bytes=0-" {
				t.Fatalf("the GET asked Range %q, want bytes=0-", rng)
			}
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			var ie *IngestError
			if !tc.ok && (!errors.As(err, &ie) || ie.Sentence != SentenceFetch) {
				t.Fatalf("a refused status is %v, want the fetch sentence", err)
			}
			if tc.ok && string(body) != "video bytes" {
				t.Fatalf("the ingest read %q", body)
			}
			lines := fetchLines(f.logs.String())
			if len(lines) != 1 {
				t.Fatalf("%d fetch lines, want one:\n%s", len(lines), f.logs.String())
			}
			l := lines[0]
			want := []string{"level=INFO", "status=" + strconv.Itoa(tc.status), "proto=HTTP/1.1",
				"accept_ranges=" + quoteEmpty(tc.acceptRanges), "seconds=", "mb_s=", "ttfb_ms="}
			if tc.ok {
				want = append(want, "bytes=11", "clip=abc123XYZ789")
			} else {
				want = append(want, "bytes=0")
			}
			for _, w := range want {
				if !strings.Contains(l, w) {
					t.Fatalf("the fetch line lacks %q: %s", w, l)
				}
			}
			for _, leak := range []string{cdn.URL, strings.TrimPrefix(cdn.URL, "http://"), "attachments/", "hm=", "sig", "clip.mp4", testGuildID, testIntToken} {
				if strings.Contains(l, leak) {
					t.Fatalf("the fetch line carries %q: %s", leak, l)
				}
			}
		})
	}
}

func quoteEmpty(s string) string {
	if s == "" {
		return `""`
	}
	return s
}

func TestProgressSentence(t *testing.T) {
	for _, c := range []struct {
		got, total int64
		want       string
	}{
		{120_400_000, 263_900_000, "Uploading… 120 of 263 MB"},
		{0, 263_000_000, "Uploading… 0 of 263 MB"},
		{263_000_000, 263_000_000, "Uploading… 263 of 263 MB"},
		{300_000_000, 263_000_000, "Uploading… 263 of 263 MB"}, // a size that lied: never past the total
	} {
		if got := progressSentence(c.got, c.total); got != c.want {
			t.Errorf("progressSentence(%d, %d) = %q, want %q", c.got, c.total, got, c.want)
		}
	}
}

// TestWatchFetchCadenceAndSkip: the watcher asks for a five-second ticker,
// edits only on a tick, and a refused edit (a 429 among them) makes it rest
// one tick — no retry. Each edit blocks until the test answers it, so a tick
// that edited would hold the next send: the skip is proven by the next tick
// being taken with no edit in between.
func TestWatchFetchCadenceAndSkip(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "unused")
	ft := installTicks(f)
	m := newFetchMeter(strings.NewReader(strings.Repeat("x", 2_500_000)))
	type editCall struct {
		content string
		reply   chan error
	}
	calls := make(chan editCall)
	edit := func(ctx context.Context, content string) error {
		c := editCall{content, make(chan error)}
		select { // a failed test's stop() must not hang on an unread edit
		case calls <- c:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case err := <-c.reply:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	stop := f.bot.watchFetch(context.Background(), m, 4_000_000, edit)
	defer stop()
	if d := ft.asked(); len(d) != 1 || d[0] != 5*time.Second {
		t.Fatalf("the watcher asked for tickers %v, want one of 5s", d)
	}
	if _, err := io.CopyN(io.Discard, m, 2_000_000); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-calls:
		t.Fatalf("an edit before the first tick: %q", c.content)
	default:
	}
	next := func() editCall {
		t.Helper()
		select {
		case c := <-calls:
			return c
		case <-time.After(5 * time.Second):
			t.Fatal("no edit after the tick")
		}
		return editCall{}
	}
	ft.send(t)
	c := next()
	if c.content != "Uploading… 2 of 4 MB" {
		t.Fatalf("the edit is %q", c.content)
	}
	c.reply <- nil
	ft.send(t)
	c = next()
	c.reply <- &retryableError{err: errors.New("HTTP 429")}
	// The tick after a refusal rests: the watcher takes the NEXT tick without
	// having asked for an edit in between.
	ft.send(t)
	select {
	case c := <-calls:
		t.Fatalf("the tick after a 429 edited: %q", c.content)
	case ft.ch <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never took the tick after the rest")
	}
	c = next()
	c.reply <- nil
	// The body ends: the watcher stops by itself and asks nothing more.
	_, _ = io.Copy(io.Discard, m)
	stop()
	select {
	case c := <-calls:
		t.Fatalf("an edit after the body ended: %q", c.content)
	default:
	}
}

// progressAPI is a fake Discord API that answers the follow-up POST with an id
// and puts every PATCH on a channel; patchStatus picks each PATCH's answer.
type progressAPI struct {
	patches chan apiCall
	posted  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	nPatch  int
	answer  func(n int) int // the n-th PATCH's status (1-based)
}

func installProgressAPI(f *botFixture, answer func(int) int) *progressAPI {
	p := &progressAPI{patches: make(chan apiCall, 64), posted: make(chan struct{}), answer: answer}
	f.answerWith(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := apiCall{method: r.Method, path: r.URL.Path, body: body}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			_, _ = io.WriteString(w, `{"id":"900000000000000001"}`)
			if strings.Contains(string(body), SentenceUploading) {
				p.once.Do(func() { close(p.posted) })
			}
		case http.MethodPatch:
			p.mu.Lock()
			p.nPatch++
			n := p.nPatch
			p.mu.Unlock()
			if st := p.answer(n); st != http.StatusOK {
				w.WriteHeader(st)
				_, _ = io.WriteString(w, `{"message":"You are being rate limited.","retry_after":5}`)
			} else {
				_, _ = io.WriteString(w, `{"id":"900000000000000001"}`)
			}
			p.patches <- c
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return p
}

// bigCDN serves n bytes of a declared-size attachment.
func bigCDN(t *testing.T, n int) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(n))
		_, _ = w.Write(bytes.Repeat([]byte{'v'}, n))
	}))
	t.Cleanup(s.Close)
	return s
}

// halfIngest reads `first` bytes, says so, waits for `resume`, reads the rest.
func halfIngest(first int, half, resume chan struct{}) Ingest {
	return func(_ context.Context, src IngestSource) (IngestResult, error) {
		if _, err := io.CopyN(io.Discard, src.Body, int64(first)); err != nil {
			return IngestResult{}, err
		}
		close(half)
		<-resume
		_, _ = io.Copy(io.Discard, src.Body)
		return IngestResult{ClipID: "abc123XYZ789", CardReady: true}, nil
	}
}

// tickUntilPatch feeds ticks until a PATCH lands (a tick before Discord named
// the follow-up's id has nothing to edit and passes).
func tickUntilPatch(t *testing.T, ft *fakeTicks, p *progressAPI) apiCall {
	t.Helper()
	for i := 0; i < 200; i++ {
		ft.send(t)
		select {
		case c := <-p.patches:
			return c
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("no PATCH after 200 ticks")
	return apiCall{}
}

// TestUploadingLineCountsTheBytes: a form job's private `Uploading…` is edited
// by its own id to the exact sentence, whole megabytes of the declared size; a
// 429 on one edit fails nothing; the link still posts and the line is still
// deleted.
func TestUploadingLineCountsTheBytes(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "unused")
	f.bot.maxBytes = 1 << 30
	ft := installTicks(f)
	p := installProgressAPI(f, func(n int) int {
		if n == 2 {
			return http.StatusTooManyRequests
		}
		return http.StatusOK
	})
	cdn := bigCDN(t, 4_000_000)
	half, resume := make(chan struct{}), make(chan struct{})
	job := formJob(f, halfIngest(2_500_000, half, resume))
	job.Uploading = true
	job.URL, job.Size = cdn.URL+"/"+testAttachment, 4_000_000
	f.bot.Clip(job)
	<-half
	<-p.posted
	hook := "/api/v10/webhooks/" + testAppID + "/" + testIntToken
	c := tickUntilPatch(t, ft, p)
	if c.path != hook+"/messages/900000000000000001" {
		t.Fatalf("the count edited %s", c.path)
	}
	if got := content(t, c); got != "Uploading… 2 of 4 MB" {
		t.Fatalf("the count reads %q", got)
	}
	ft.send(t) // the second edit: answered 429
	select {
	case <-p.patches:
	case <-time.After(5 * time.Second):
		t.Fatal("no second edit")
	}
	close(resume)
	f.quiesce(t)
	calls := f.callsSnapshot()
	var link, deleted bool
	for _, c := range calls {
		if c.method == http.MethodPost && strings.Contains(string(c.body), "abc123XYZ789") {
			link = true
		}
		if c.method == http.MethodDelete && c.path == hook+"/messages/900000000000000001" {
			deleted = true
		}
		if c.method == http.MethodPatch && strings.HasSuffix(c.path, "@original") {
			t.Fatalf("a form job PATCHed @original")
		}
	}
	if !link || !deleted {
		t.Fatalf("link posted %v, line deleted %v; calls %d; log:\n%s", link, deleted, len(calls), f.logs.String())
	}
	if !strings.Contains(f.logs.String(), `msg="discord bot: /clip"`) {
		t.Fatalf("the job did not end in its link line:\n%s", f.logs.String())
	}
	if n := len(fetchLines(f.logs.String())); n != 1 {
		t.Fatalf("%d fetch lines, want one", n)
	}
}

// TestDeferredCountEditsOriginal: with the form off the "thinking…" reply is
// the line, edited as `@original`; the link is still the last edit.
func TestDeferredCountEditsOriginal(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "unused")
	f.bot.maxBytes = 1 << 30
	ft := installTicks(f)
	p := installProgressAPI(f, func(int) int { return http.StatusOK })
	cdn := bigCDN(t, 3_000_000)
	half, resume := make(chan struct{}), make(chan struct{})
	job := okJob(f, halfIngest(1_000_000, half, resume))
	job.URL, job.Size = cdn.URL+"/"+testAttachment, 3_000_000
	f.bot.Clip(job)
	<-half
	ft.send(t)
	var c apiCall
	select {
	case c = <-p.patches:
	case <-time.After(5 * time.Second):
		t.Fatal("no edit after the tick")
	}
	if !strings.HasSuffix(c.path, "/messages/@original") {
		t.Fatalf("the deferred count edited %s", c.path)
	}
	if got := content(t, c); got != "Uploading… 1 of 3 MB" {
		t.Fatalf("the count reads %q", got)
	}
	close(resume)
	f.quiesce(t)
	calls := f.callsSnapshot()
	last := calls[len(calls)-1]
	if !strings.HasSuffix(last.path, "/messages/@original") || !strings.Contains(content(t, last), "abc123XYZ789") {
		t.Fatalf("the last call is %s %s %s, want the link's @original edit", last.method, last.path, last.body)
	}
}

// TestAutoFetchLogsAndNeverEdits: Clippet Auto shares the fetch, so it gets the
// line; nobody waits on a message, so no ticker is ever asked for and Discord's
// API is never called.
func TestAutoFetchLogsAndNeverEdits(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	f.bot.tick = func(time.Duration) (<-chan time.Time, func()) {
		t.Error("Clippet Auto's fetch asked for the count's ticker")
		return make(chan time.Time), func() {}
	}
	ch, err := f.bot.Fetch(fetchAtt(f), func(_ context.Context, src IngestSource) (IngestResult, error) {
		_, _ = io.ReadAll(src.Body)
		return IngestResult{ClipID: "abc123XYZ789", CardReady: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out := <-ch; out.Err != nil {
		t.Fatal(out.Err)
	}
	f.quiesce(t)
	if n := len(f.callsSnapshot()); n != 0 {
		t.Fatalf("Clippet Auto called Discord's API %d time(s)", n)
	}
	lines := fetchLines(f.logs.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "bytes=11") || !strings.Contains(lines[0], "clip=abc123XYZ789") ||
		strings.Contains(lines[0], f.cdn.URL) {
		t.Fatalf("Auto's fetch lines: %q", lines)
	}
}
