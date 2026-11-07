package notify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"clippet/internal/urls"
)

// The upload form's "Uploading…" and its redraw (§20 erratum 325).

const testClipID = "abc123XYZ789"

// Discord's answers in these tests: the ephemeral line is message 901, the
// link 902 — decided by the body, so concurrency cannot swap them.
const (
	upMsgID   = "901"
	linkMsgID = "902"
)

// recordAPI makes the fake API record every call and answer with decide's
// status and body.
func recordAPI(f *botFixture, decide func(method, path string, body []byte) (int, string)) {
	f.answerWith(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		f.mu.Lock()
		f.calls = append(f.calls, apiCall{method: r.Method, path: r.URL.Path,
			auth: r.Header.Get("Authorization"), body: body})
		f.mu.Unlock()
		code, answer := decide(r.Method, r.URL.Path, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, answer)
	})
}

// discordLike answers as Discord does: a POST gets a message (901 for the
// ephemeral line, 902 for anything else), an edit gets the message, a delete
// gets 204.
func discordLike(method, _ string, body []byte) (int, string) {
	switch method {
	case http.MethodPost:
		if strings.Contains(string(body), `"flags":64`) {
			return 200, `{"id":"` + upMsgID + `","flags":64}`
		}
		return 200, `{"id":"` + linkMsgID + `"}`
	case http.MethodDelete:
		return 204, ""
	}
	return 200, `{"id":"` + linkMsgID + `"}`
}

func (f *botFixture) waitCalls(t *testing.T, what string, ok func([]apiCall) bool) []apiCall {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls := f.callsSnapshot()
		if ok(calls) {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("never saw %s; calls %v; log:\n%s", what, describe(calls), f.logs.String())
		}
		time.Sleep(10 * time.Millisecond) // sleep: the conditions read the bot's own state and log, which no signal reports
	}
}

func describe(calls []apiCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.method+" "+c.path+" "+string(c.body))
	}
	return out
}

func indexOf(calls []apiCall, ok func(apiCall) bool) int {
	for i, c := range calls {
		if ok(c) {
			return i
		}
	}
	return -1
}

func count(calls []apiCall, ok func(apiCall) bool) int {
	n := 0
	for _, c := range calls {
		if ok(c) {
			n++
		}
	}
	return n
}

func isMethod(m string) func(apiCall) bool { return func(c apiCall) bool { return c.method == m } }

func isUploadingPost(c apiCall) bool {
	return c.method == http.MethodPost && strings.Contains(string(c.body), `"flags":64`)
}

func isPublicPost(c apiCall) bool {
	return c.method == http.MethodPost && !strings.Contains(string(c.body), `"flags":64`)
}

func uploadingJob(f *botFixture, ingest Ingest) ClipJob {
	j := formJob(f, ingest)
	j.Uploading = true
	return j
}

func okIngest(context.Context, IngestSource) (IngestResult, error) {
	return IngestResult{ClipID: testClipID, CardReady: true}, nil
}

func (f *botFixture) hook() string {
	return botAPIPath + "/webhooks/" + testAppID + "/" + testIntToken
}

// TestBotUploadingIsPrivateAndGoneAfterTheLink: one ephemeral follow-up with
// exactly the word, deleted by its own id once the link is out — no
// Authorization anywhere, no PATCH.
func TestBotUploadingIsPrivateAndGoneAfterTheLink(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	recordAPI(f, discordLike)
	f.bot.Clip(uploadingJob(f, okIngest))
	calls := f.waitCalls(t, "the delete", func(c []apiCall) bool { return count(c, isMethod(http.MethodDelete)) > 0 })

	up := indexOf(calls, isUploadingPost)
	if up < 0 || count(calls, isUploadingPost) != 1 {
		t.Fatalf("want one Uploading line: %v", describe(calls))
	}
	if got, want := string(calls[up].body), `{"content":"Uploading…","flags":64,"allowed_mentions":{"parse":[]}}`; got != want {
		t.Fatalf("the line is\n%s\nwant\n%s", got, want)
	}
	if calls[up].path != f.hook() {
		t.Fatalf("the line went to %s", calls[up].path)
	}
	link := indexOf(calls, isPublicPost)
	del := indexOf(calls, isMethod(http.MethodDelete))
	if link < 0 || string(calls[link].body) != `{"content":"https://clips.example/`+testClipID+`","allowed_mentions":{"parse":[]}}` {
		t.Fatalf("the link: %v", describe(calls))
	}
	if del < link {
		t.Fatalf("the line was deleted before the link was out: %v", describe(calls))
	}
	if calls[del].path != f.hook()+"/messages/"+upMsgID {
		t.Fatalf("deleted %s, want the line's own id", calls[del].path)
	}
	for _, c := range calls {
		if c.auth != "" || c.method == http.MethodPatch {
			t.Fatalf("%s %s (auth %q) — no credential and no edit here", c.method, c.path, c.auth)
		}
	}
}

// TestBotUploadingIsGoneAfterAFailureSentence: the job's end is the failure
// line too.
func TestBotUploadingIsGoneAfterAFailureSentence(t *testing.T) {
	f := newBotFixture(t, http.StatusForbidden, "")
	recordAPI(f, discordLike)
	f.bot.Clip(uploadingJob(f, okIngest))
	calls := f.waitCalls(t, "the delete", func(c []apiCall) bool { return count(c, isMethod(http.MethodDelete)) > 0 })
	fail := indexOf(calls, isPublicPost)
	if fail < 0 || !strings.Contains(string(calls[fail].body), SentenceFetch) {
		t.Fatalf("no failure sentence: %v", describe(calls))
	}
	if del := indexOf(calls, isMethod(http.MethodDelete)); del < fail || calls[del].path != f.hook()+"/messages/"+upMsgID {
		t.Fatalf("the delete: %v", describe(calls))
	}
}

// TestBotUploadingThatWillNotPostChangesNothing: the upload goes on exactly as
// before, and nothing is deleted.
func TestBotUploadingThatWillNotPostChangesNothing(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	recordAPI(f, func(m, p string, body []byte) (int, string) {
		if strings.Contains(string(body), `"flags":64`) {
			return http.StatusBadRequest, `{"message":"no"}`
		}
		return discordLike(m, p, body)
	})
	f.bot.Clip(uploadingJob(f, okIngest))
	f.waitCalls(t, "the link", func(c []apiCall) bool { return count(c, isPublicPost) > 0 })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = f.bot.Shutdown(ctx)
	calls := f.callsSnapshot()
	if count(calls, isMethod(http.MethodDelete)) != 0 || count(calls, isPublicPost) != 1 {
		t.Fatalf("a refused line changed the job: %v", describe(calls))
	}
	if strings.Contains(f.logs.String(), testIntToken) {
		t.Fatal("the interaction token reached the log")
	}
}

// TestBotUploadingOnlyOnAFormJob: the deferred path (form off, table full) is
// untouched — its "thinking…" is the feedback — and a form job without the
// flag posts the link alone.
func TestBotUploadingOnlyOnAFormJob(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	recordAPI(f, discordLike)
	job := okJob(f, okIngest)
	job.Uploading = true // ignored without Followup
	f.bot.Clip(job)
	f.waitCalls(t, "the edit", func(c []apiCall) bool { return count(c, isMethod(http.MethodPatch)) > 0 })
	g := newBotFixture(t, http.StatusOK, "video bytes")
	recordAPI(g, discordLike)
	g.bot.Clip(formJob(g, okIngest))
	g.waitCalls(t, "the link", func(c []apiCall) bool { return count(c, isPublicPost) > 0 })
	for _, x := range []*botFixture{f, g} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = x.bot.Shutdown(ctx)
		cancel()
	}
	if c := f.callsSnapshot(); len(c) != 1 || c[0].method != http.MethodPatch {
		t.Fatalf("a deferred job made %v", describe(c))
	}
	if c := g.callsSnapshot(); len(c) != 1 || !isPublicPost(c[0]) {
		t.Fatalf("a form job without the flag made %v", describe(c))
	}
}

// managedBot replaces the fixture's bot with one on the official bot's
// sentinel (DISCORD_BOT_TOKEN=managed): the same interaction-token routes.
func managedBot(t *testing.T, f *botFixture) {
	t.Helper()
	base, _ := url.Parse("https://clips.example")
	f.bot = NewBot(BotDeps{
		AppID: testAppID, Token: "managed", Managed: true,
		URLs:      urls.New(base, "test"),
		Log:       slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		APIHost:   strings.TrimPrefix(f.api.URL, "http://"),
		APIScheme: "http", AllowAttachment: func(raw string) (*url.URL, error) { return url.Parse(raw) },
		MaxBytes: 1 << 20, CallTimeout: 2 * time.Second, FetchTimeout: 2 * time.Second, JobBudget: 5 * time.Second,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.bot.Shutdown(ctx)
	})
}

// TestBotRedrawEditsThePostedLinkOnce: the follow-up's content becomes the
// link plus `?v=2` — content and the empty mention list, nothing else — on the
// message the link was posted as; a second ask edits nothing. The same on a
// managed instance.
func TestBotRedrawEditsThePostedLinkOnce(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprintf("managed=%v", managed), func(t *testing.T) {
			f := newBotFixture(t, http.StatusOK, "video bytes")
			if managed {
				managedBot(t, f)
			}
			recordAPI(f, discordLike)
			f.bot.Clip(uploadingJob(f, okIngest))
			f.waitCalls(t, "the link", func(c []apiCall) bool { return count(c, isPublicPost) > 0 })
			f.bot.Redraw(testClipID)
			calls := f.waitCalls(t, "the redraw", func(c []apiCall) bool { return count(c, isMethod(http.MethodPatch)) > 0 })
			p := calls[indexOf(calls, isMethod(http.MethodPatch))]
			if p.path != f.hook()+"/messages/"+linkMsgID || p.auth != "" {
				t.Fatalf("the redraw went to %s (auth %q)", p.path, p.auth)
			}
			if got, want := string(p.body), `{"content":"https://clips.example/`+testClipID+`?v=2","allowed_mentions":{"parse":[]}}`; got != want {
				t.Fatalf("the redraw is\n%s\nwant\n%s", got, want)
			}
			f.waitCalls(t, "outcome=edited", func([]apiCall) bool { return strings.Contains(f.logs.String(), "outcome=edited") })
			f.bot.Redraw(testClipID)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = f.bot.Shutdown(ctx)
			if n := count(f.callsSnapshot(), isMethod(http.MethodPatch)); n != 1 {
				t.Fatalf("%d edits, want one redraw per upload", n)
			}
			if !strings.Contains(f.logs.String(), "outcome=none") || strings.Contains(f.logs.String(), testIntToken) {
				t.Fatalf("the second ask is not logged as none, or the token leaked:\n%s", f.logs.String())
			}
		})
	}
}

// TestBotRedrawWithNothingPostedIsNothing: no link of a form job for that clip
// (none yet, or a deferred job's) → no call at all, one log line.
func TestBotRedrawWithNothingPostedIsNothing(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	recordAPI(f, discordLike)
	f.bot.Redraw(testClipID)
	f.bot.Clip(okJob(f, okIngest)) // deferred: its reply is @original, never redrawn
	f.waitCalls(t, "the edit", func(c []apiCall) bool { return count(c, isMethod(http.MethodPatch)) > 0 })
	f.bot.Redraw(testClipID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = f.bot.Shutdown(ctx)
	if c := f.callsSnapshot(); len(c) != 1 {
		t.Fatalf("calls %v, want only the deferred edit", describe(c))
	}
	if n := strings.Count(f.logs.String(), "outcome=none"); n != 2 {
		t.Fatalf("%d none lines, want 2:\n%s", n, f.logs.String())
	}
}

// TestBotRedrawDuringThePostWaitsForIt: a submit landing while the link's
// POST is in flight is not lost — the job edits once Discord answers.
func TestBotRedrawDuringThePostWaitsForIt(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	arrived := make(chan struct{})
	release := make(chan struct{})
	recordAPI(f, func(m, p string, body []byte) (int, string) {
		if m == http.MethodPost && !strings.Contains(string(body), `"flags":64`) {
			close(arrived)
			<-release
		}
		return discordLike(m, p, body)
	})
	f.bot.Clip(formJob(f, okIngest))
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the link was never posted")
	}
	f.bot.Redraw(testClipID)
	if !strings.Contains(f.logs.String(), "outcome=after_post") {
		t.Fatalf("not deferred to the post:\n%s", f.logs.String())
	}
	close(release)
	calls := f.waitCalls(t, "the redraw", func(c []apiCall) bool { return count(c, isMethod(http.MethodPatch)) > 0 })
	if p := calls[indexOf(calls, isMethod(http.MethodPatch))]; p.path != f.hook()+"/messages/"+linkMsgID ||
		!strings.Contains(string(p.body), testClipID+`?v=2"`) {
		t.Fatalf("the redraw: %s %s", p.path, p.body)
	}
}

// TestBotRedrawFailureIsOneLogLine: a refused edit shows nothing to anybody;
// the log names the clip and the outcome, never the token.
func TestBotRedrawFailureIsOneLogLine(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	recordAPI(f, func(m, p string, body []byte) (int, string) {
		if m == http.MethodPatch {
			return http.StatusBadRequest, `{"message":"bad ` + p + `"}`
		}
		return discordLike(m, p, body)
	})
	f.bot.Clip(formJob(f, okIngest))
	f.waitCalls(t, "the link", func(c []apiCall) bool { return count(c, isPublicPost) > 0 })
	f.waitCalls(t, "the id held", func([]apiCall) bool { return f.bot.links.len() == 1 && linkHeld(f.bot) })
	f.bot.Redraw(testClipID)
	f.waitCalls(t, "outcome=failed", func([]apiCall) bool { return strings.Contains(f.logs.String(), "outcome=failed") })
	logged := f.logs.String()
	if !strings.Contains(logged, "clip="+testClipID) || strings.Contains(logged, testIntToken) {
		t.Fatalf("the failure line:\n%s", logged)
	}
	if n := count(f.callsSnapshot(), isMethod(http.MethodPatch)); n != 1 {
		t.Fatalf("%d edits; a 400 is not retried", n)
	}
}

func linkHeld(b *Bot) bool {
	b.links.mu.Lock()
	defer b.links.mu.Unlock()
	c, ok := b.links.m[testClipID]
	return ok && len(c.links) == 1 && c.links[0].messageID != ""
}

// TestLinkTableIsBoundedAndExpires: 256 links at most (a full table holds
// nothing more), and none past the token's fifteen minutes.
func TestLinkTableIsBoundedAndExpires(t *testing.T) {
	tb := newLinkTable()
	t0 := time.Unix(1_800_000_000, 0)
	var first *postedLink
	for i := 0; i < linkTableMax; i++ {
		e := tb.begin(fmt.Sprintf("clip%d", i), "tok", "", reasonTitle, t0.Add(linkLife), t0)
		if e == nil {
			t.Fatalf("entry %d refused", i)
		}
		if i == 0 {
			first = e
		}
	}
	if tb.begin("one-more", "tok", "", reasonTitle, t0.Add(linkLife), t0) != nil {
		t.Fatal("a full table took another link")
	}
	tb.posted(first, "902")
	if tasks, later := tb.take("clip0", reasonTitle, t0.Add(linkLife)); len(tasks) != 0 || later {
		t.Fatalf("at fifteen minutes: %v later=%v", tasks, later)
	}
	// Beginning past the life sweeps every expired entry.
	fresh := tb.begin("fresh", "tok", "", reasonTitle, t0.Add(2*linkLife), t0.Add(linkLife))
	if fresh == nil || tb.len() != 1 {
		t.Fatalf("the sweep left %d", tb.len())
	}
	// An answer with no id holds nothing.
	tb.posted(fresh, "")
	if tb.len() != 0 {
		t.Fatal("a link without an id was held")
	}
	// A link owed nothing is never held.
	if tb.begin("ready", "tok", originalMessage, 0, t0.Add(3*linkLife), t0.Add(linkLife)) != nil || tb.len() != 0 {
		t.Fatal("a link owed no redraw was held")
	}
}

// TestMessageIDAcceptsOnlyASnowflake: the id goes into a path.
func TestMessageIDAcceptsOnlyASnowflake(t *testing.T) {
	for raw, want := range map[string]string{
		`{"id":"1234567890123456789"}`:   "1234567890123456789",
		`{"id":"../x"}`:                  "",
		`{"id":12}`:                      "",
		`{}`:                             "",
		`not json`:                       "",
		`{"id":"123456789012345678901"}`: "",
	} {
		if got := messageID([]byte(raw)); got != want {
			t.Errorf("messageID(%s) = %q, want %q", raw, got, want)
		}
	}
}
