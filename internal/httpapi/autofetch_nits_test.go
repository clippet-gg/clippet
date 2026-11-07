package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/httpx"
	"clippet/internal/notify"
)

// POST /in/auto/{token} after run 8's box nits (§20 erratum 327): every arm of
// the outcome switch, the declared size as the truth, the account's running
// jobs capped, a restart mid-GET, the long file name clipped and the second
// bucket keyed by the URL.

// outcomeBot is a recorderBot whose Fetch ends with a chosen outcome, or is
// refused as a shutting-down bot's is.
type outcomeBot struct {
	recorderBot
	out    notify.FetchOutcome
	closed bool
}

func (b *outcomeBot) Fetch(att notify.Attachment, ing notify.Ingest) (<-chan notify.FetchOutcome, error) {
	_, _ = b.recorderBot.Fetch(att, ing)
	if b.closed {
		return nil, notify.ErrClosed
	}
	ch := make(chan notify.FetchOutcome, 1)
	ch <- b.out
	return ch, nil
}

// Each sentence the fetcher or the ingest can end in, and the refusal the
// route answers it with. SentenceBusy has no arm of its own (the route's
// ingest never charges the bucket): were it ever chosen it is the generic 500.
func TestAutoFetchEveryOutcomeArm(t *testing.T) {
	sentence := func(s string) notify.FetchOutcome {
		return notify.FetchOutcome{Err: &notify.IngestError{Sentence: s, Err: errors.New("detail")}}
	}
	cases := []struct {
		name         string
		bot          *outcomeBot
		code         int
		errCode, msg string
		retryAfter   string
	}{
		{"closed", &outcomeBot{closed: true}, 503, "unavailable", msgAutoFetchRestarting, "5"},
		{"fetch", &outcomeBot{out: sentence(notify.SentenceFetch)}, 502, "fetch", msgAutoFetchFetch, ""},
		{"stream too large", &outcomeBot{out: sentence(notify.SentenceTooLarge)}, 413, "too_large", "", ""},
		{"probe: not a video", &outcomeBot{out: sentence(notify.SentenceNotVideo)}, 415, "not_video", msgAutoFetchNotVideo, ""},
		{"scratch", &outcomeBot{out: sentence(notify.SentenceFull)}, 507, "insufficient_scratch", "not enough scratch space for this upload", ""},
		{"quota", &outcomeBot{out: sentence(notify.SentenceQuota)}, 507, "storage_quota", quotaRefusal, ""},
		{"restart (cancelled)", &outcomeBot{out: notify.FetchOutcome{Err: context.Canceled}}, 503, "unavailable", msgAutoFetchRestarting, "5"},
		{"restart (the core's gone)", &outcomeBot{out: notify.FetchOutcome{Err: &ingestError{kind: ingestFailGone}}}, 503, "unavailable", msgAutoFetchRestarting, "5"},
		{"busy is unreachable: the generic 500", &outcomeBot{out: sentence(notify.SentenceBusy)}, 500, "failed", msgAutoFetchFailed, ""},
		{"anything else", &outcomeBot{out: notify.FetchOutcome{Err: errors.New("x")}}, 500, "failed", msgAutoFetchFailed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, func(d *Deps) { d.Bot = tc.bot })
			u, _ := e.newUser("carol")
			raw := e.newAutoFetch(u)
			e.bind(afGuild, u)
			r := e.autoFetch(raw, afBody())
			if r.code != tc.code || r.errCode() != tc.errCode || (tc.msg != "" && r.str("message") != tc.msg) {
				t.Fatalf("%d %s, want %d %s %q", r.code, r.body, tc.code, tc.errCode, tc.msg)
			}
			if got := r.hdr.Get("Retry-After"); got != tc.retryAfter {
				t.Fatalf("Retry-After %q, want %q", got, tc.retryAfter)
			}
			if tc.bot.fetchCount() != 1 {
				t.Fatalf("%d fetch call(s)", tc.bot.fetchCount())
			}
		})
	}
}

// A declared size smaller than the stream: the stream stops at the declared
// size and the request is the 413 the early check would have given.
func TestAutoFetchDeclaredSizeBoundsTheStream(t *testing.T) {
	stub := newAFStub(t, testVideo(4000))
	e, _ := afEnv(t, stub)
	u, _ := e.newUser("carol")
	raw := e.newAutoFetch(u)
	e.bind(afGuild, u)
	r := e.autoFetch(raw, afBody(func(m map[string]any) { m["size"] = 100 }))
	if r.code != 413 || r.errCode() != "too_large" {
		t.Fatalf("a 4000-byte stream declared as 100: %d %s", r.code, r.body)
	}
	if n := e.countRows("clips"); n != 0 {
		t.Fatalf("%d clip row(s) from a lying size", n)
	}
	// The truth (or more) is accepted.
	if r := e.autoFetch(raw, afBody(func(m map[string]any) { m["size"] = 4000 })); r.code != 201 {
		t.Fatalf("the true size: %d %s", r.code, r.body)
	}
}

// At most two running jobs per account: a third press while two are still
// downloading is the bucket's 429; once they end, presses pass again. Another
// account is not affected.
func TestAutoFetchCapsAnAccountsRunningJobs(t *testing.T) {
	stub := newAFStub(t, testVideo(4000))
	e, _ := afEnv(t, stub, func(d *Deps) { d.AutoFetchBudget = 100 * time.Millisecond })
	u, _ := e.newUser("carol")
	raw := e.newAutoFetch(u)
	e.bind(afGuild, u)
	other, _ := e.newUser("other")
	oraw := e.newAutoFetch(other)
	e.bind("444444444444444444", other)
	slow := func(m map[string]any) { m["url"] = strings.Replace(afCDN, "clip.mp4", "slow.mp4", 1) }
	for i := range 2 {
		if r := e.autoFetch(raw, afBody(slow)); r.code != 202 {
			t.Fatalf("press %d: %d %s", i+1, r.code, r.body)
		}
	}
	r := e.autoFetch(raw, afBody(slow))
	if r.code != 429 || r.errCode() != "rate_limited" || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("a third running job: %d %v %s", r.code, r.hdr, r.body)
	}
	if hits := stub.count(); hits != 2 {
		t.Fatalf("the CDN saw %d request(s); the refused press fetched", hits)
	}
	if r := e.autoFetch(oraw, afBody(slow, func(m map[string]any) { m["guild_id"] = "444444444444444444" })); r.code != 202 {
		t.Fatalf("another account's press: %d %s", r.code, r.body)
	}
	stub.free()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := e.autoFetch(raw, afBody(func(m map[string]any) { m["filename"] = "again.mp4" }))
		if r.code == 201 || r.code == 200 { // 200: the same bytes are a duplicate of the slow jobs' clip
			break
		}
		if r.code != 429 || time.Now().After(deadline) {
			t.Fatalf("after the two jobs ended: %d %s", r.code, r.body)
		}
		time.Sleep(20 * time.Millisecond) // sleep: the limiter frees the slot when the two jobs end, which no signal reports
	}
}

// A restart while the CDN GET is in flight answers the restart's 503, not the
// CDN's 502.
func TestAutoFetchRestartDuringTheGetIsA503(t *testing.T) {
	stub := newAFStub(t, testVideo(4000))
	e, bot := afEnv(t, stub)
	u, _ := e.newUser("carol")
	raw := e.newAutoFetch(u)
	e.bind(afGuild, u)
	done := make(chan resp, 1)
	go func() {
		done <- e.autoFetch(raw, afBody(func(m map[string]any) { m["url"] = strings.Replace(afCDN, "clip.mp4", "slow.mp4", 1) }))
	}()
	stub.waitHits(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = bot.Shutdown(ctx) }()
	select {
	case r := <-done:
		if r.code != 503 || r.errCode() != "unavailable" || r.str("message") != msgAutoFetchRestarting || r.hdr.Get("Retry-After") != "5" {
			t.Fatalf("a restart mid-GET: %d %v %s", r.code, r.hdr, r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no answer after the restart")
	}
	wg.Wait()
}

// A file name past 255 bytes is clipped, never refused: Clippet Auto passes
// Discord's own name. The extension survives, and the cut is on a rune.
func TestAutoFetchClipsALongFileName(t *testing.T) {
	stub := newAFStub(t, testVideo(4000))
	e, _ := afEnv(t, stub)
	u, _ := e.newUser("carol")
	raw := e.newAutoFetch(u)
	e.bind(afGuild, u)
	long := strings.Repeat("é", 150) + ".mp4" // 304 bytes
	r := e.autoFetch(raw, afBody(func(m map[string]any) { m["filename"] = long }))
	if r.code != 201 {
		t.Fatalf("a 304-byte name: %d %s", r.code, r.body)
	}
	if c := e.getClip(r.str("id")); c.Title != strings.Repeat("é", 125) {
		t.Fatalf("title %q (%d bytes)", c.Title, len(c.Title))
	}
}

func TestClipFilename(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"clip.mp4", 255, "clip.mp4"},
		{strings.Repeat("a", 255), 255, strings.Repeat("a", 255)},
		{strings.Repeat("a", 300) + ".mp4", 255, strings.Repeat("a", 251) + ".mp4"},
		{strings.Repeat("a", 300), 255, strings.Repeat("a", 255)},
		{strings.Repeat("é", 150) + ".webm", 255, strings.Repeat("é", 125) + ".webm"},
		{"a." + strings.Repeat("x", 300), 255, ("a." + strings.Repeat("x", 300))[:255]},
		{strings.Repeat("€", 100), 10, "€€€"},
	} {
		got := clipFilename(tc.in, tc.max)
		if got != tc.want || len(got) > tc.max {
			t.Errorf("clipFilename(%d bytes, %d) = %q (%d bytes), want %q", len(tc.in), tc.max, got, len(got), tc.want)
		}
	}
}

// The second bucket is the URL's, not the address's: every press comes from
// Clippet Auto's egress, so two accounts' URLs pressed from one address each
// get their own bucket.
func TestAutoFetchSecondBucketIsKeyedByTheURL(t *testing.T) {
	rec := &recorderBot{}
	e := newEnv(t, func(d *Deps) { d.Bot = rec; d.Cfg.RateUploadPerHourPerIP = 1 })
	a, _ := e.newUser("alice")
	b, _ := e.newUser("bob")
	araw, braw := e.newAutoFetch(a), e.newAutoFetch(b)
	e.bind(afGuild, a)
	e.bind("444444444444444444", b)
	if r := e.autoFetch(araw, afBody()); r.code == 429 {
		t.Fatalf("alice's first press: %s", r.body)
	}
	if r := e.autoFetch(braw, afBody(func(m map[string]any) { m["guild_id"] = "444444444444444444" })); r.code == 429 {
		t.Fatalf("bob's first press from the same address was refused by alice's bucket: %s", r.body)
	}
	if r := e.autoFetch(araw, afBody()); r.code != 429 {
		t.Fatalf("alice's second press with a bucket of one: %d %s", r.code, r.body)
	}
}

// A hung-up client's access line is the 400 every route's hang-up writes,
// never a 200 nothing was written for.
func TestAutoFetchHangUpIsNotA200(t *testing.T) {
	stub := newAFStub(t, testVideo(4000))
	e, _ := afEnv(t, stub)
	u, _ := e.newUser("carol")
	raw := e.newAutoFetch(u)
	e.bind(afGuild, u)
	body, _ := json.Marshal(afBody(func(m map[string]any) { m["url"] = strings.Replace(afCDN, "clip.mp4", "slow.mp4", 1) }))
	ctx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	var logs bytes.Buffer
	h := httpx.AccessLog(slog.New(slog.NewTextHandler(&logs, nil)))(e.h)
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/in/auto/"+raw, bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(w, req)
	}()
	stub.waitHits(t, 1)
	cancel()
	<-done
	if w.Code != 400 || !strings.Contains(w.Body.String(), "client_disconnected") {
		t.Fatalf("a hang-up answered %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(logs.String(), "status=400") || !strings.Contains(logs.String(), "client_disconnected=true") {
		t.Fatalf("the access line: %s", logs.String())
	}
}
