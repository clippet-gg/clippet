package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// One link per interaction (§20 erratum 320 (g), the box's half): the link
// follow-up's id is held per token, a second link follow-up on that token
// edits it, and the retry classes are told apart in the log.

const linkClip = "abc123XYZ789"

func linkCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func heldLinkBody(t *testing.T, f *botFixture) []byte {
	t.Helper()
	b, err := json.Marshal(ClipMessage(f.bot.urls, linkClip))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func countCalls(calls []apiCall, method, path string) int {
	n := 0
	for _, c := range calls {
		if c.method == method && c.path == path {
			n++
		}
	}
	return n
}

// (a) A link follow-up on a token that already holds the link's id: exactly
// one PATCH of that message, the same body, and no POST.
func TestBotLinkFollowupOnAHeldTokenEditsNeverPosts(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	f.bot.linkIDs.hold(testIntToken, "777", time.Now())
	job := formJob(f, nil)
	f.bot.deliver(linkCtx(t), job, ClipMessage(f.bot.urls, linkClip), linkClip)
	calls := f.callsSnapshot()
	if n := countCalls(calls, http.MethodPost, f.hook()); n != 0 {
		t.Fatalf("%d follow-up POSTs on a token whose link is held, want 0: %+v", n, calls)
	}
	if len(calls) != 1 || calls[0].method != http.MethodPatch || calls[0].path != f.hook()+"/messages/777" {
		t.Fatalf("calls %+v, want exactly one PATCH of %s/messages/777", calls, f.hook())
	}
	if !bytes.Equal(calls[0].body, heldLinkBody(t, f)) {
		t.Fatalf("the edit's body is\n%s\nwant the link's\n%s", calls[0].body, heldLinkBody(t, f))
	}
	if !strings.Contains(f.logs.String(), "the link is already posted on this interaction; editing it") {
		t.Fatalf("no line for the edit: %s", f.logs.String())
	}
}

// (a) The same through both ways a link leaves — deliver (a link owed no
// redraw) then deliverLink (one held for a redraw): one POST, then one PATCH
// of the id the POST was answered with.
func TestBotSecondLinkFollowupEditsTheFirst(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	job := formJob(f, nil)
	f.bot.deliver(linkCtx(t), job, ClipMessage(f.bot.urls, linkClip), linkClip)
	f.bot.deliverLink(linkCtx(t), job, linkClip, reasonCard, time.Now())
	calls := f.callsSnapshot()
	if n := countCalls(calls, http.MethodPost, f.hook()); n != 1 {
		t.Fatalf("%d follow-up POSTs, want 1: %+v", n, calls)
	}
	if n := countCalls(calls, http.MethodPatch, f.hook()+"/messages/1"); n != 1 {
		t.Fatalf("%d PATCHes of the posted link, want 1: %+v", n, calls)
	}
	if len(calls) != 2 || !bytes.Equal(calls[1].body, calls[0].body) {
		t.Fatalf("the edit does not carry the post's body: %+v", calls)
	}
}

// retryHandler answers the follow-up POSTs: the first with first(), every
// later one with the id `id`, counting them.
type retryHandler struct {
	mu    sync.Mutex
	posts int
	other []string
}

func (h *retryHandler) count() (int, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.posts, append([]string(nil), h.other...)
}

func (h *retryHandler) serve(t *testing.T, first func(http.ResponseWriter, *http.Request), id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isFollowup(r.Method, r.URL.Path) {
			h.mu.Lock()
			h.other = append(h.other, r.Method+" "+r.URL.Path)
			h.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"9"}`)
			return
		}
		_, _ = io.ReadAll(r.Body)
		h.mu.Lock()
		h.posts++
		n := h.posts
		h.mu.Unlock()
		if n == 1 {
			first(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"`+id+`"}`)
	}
}

// (b) 503 then 200: Discord answered, the body did not land — two POSTs, and
// the second's id is the one held.
func TestBotLinkFollowupRetryAfter503HoldsTheRetrysID(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	h := &retryHandler{}
	f.answerWith(h.serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"message":"upstream"}`)
	}, "42"))
	f.bot.deliver(linkCtx(t), formJob(f, nil), ClipMessage(f.bot.urls, linkClip), linkClip)
	posts, other := h.count()
	if posts != 2 || len(other) != 0 {
		t.Fatalf("%d POSTs and %v, want 2 POSTs and nothing else", posts, other)
	}
	if id := f.bot.linkIDs.get(testIntToken, time.Now()); id != "42" {
		t.Fatalf("held id %q, want the retry's 42", id)
	}
	logged := f.logs.String()
	if !strings.Contains(logged, "retrying the follow-up") || strings.Contains(logged, "response lost") {
		t.Fatalf("a 503's retry is not the ordinary retry line: %s", logged)
	}
}

// (c) A transport failure on the POST — the connection closed with no answer,
// the message possibly stored — still retries once, under its own WARN line
// (the clip id, never the token), and the retry's id is held.
func TestBotLinkFollowupLostResponseRetriesUnderItsOwnLine(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	h := &retryHandler{}
	f.answerWith(h.serve(t, func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test server cannot hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close() // the request was read; no answer ever comes
	}, "43"))
	f.bot.deliver(linkCtx(t), formJob(f, nil), ClipMessage(f.bot.urls, linkClip), linkClip)
	posts, other := h.count()
	if posts != 2 || len(other) != 0 {
		t.Fatalf("%d POSTs and %v, want 2 POSTs (one retry) and nothing else", posts, other)
	}
	logged := f.logs.String()
	if !strings.Contains(logged, "follow-up response lost; retrying — the link may appear twice") {
		t.Fatalf("no lost-response line: %s", logged)
	}
	if !strings.Contains(logged, "clip="+linkClip) {
		t.Fatalf("the lost-response line does not name the clip: %s", logged)
	}
	if strings.Contains(logged, testIntToken) {
		t.Fatalf("the INTERACTION token reached the log:\n%s", logged)
	}
	if id := f.bot.linkIDs.get(testIntToken, time.Now()); id != "43" {
		t.Fatalf("held id %q, want the retry's 43", id)
	}
}

// (d) The holder's cap and expiry.
func TestLinkIDTableCapAndExpiry(t *testing.T) {
	tab := newLinkIDTable()
	t0 := time.Unix(1_800_000_000, 0)
	for i := 0; i <= linkIDMax; i++ {
		tab.hold("tok-"+strconv.Itoa(i), strconv.Itoa(i+1), t0.Add(time.Duration(i)*time.Millisecond))
	}
	if n := tab.len(); n != linkIDMax {
		t.Fatalf("%d entries, want the cap %d", n, linkIDMax)
	}
	now := t0.Add(2 * time.Second)
	if id := tab.get("tok-0", now); id != "" {
		t.Fatalf("the oldest entry survived a full table: %q", id)
	}
	if id := tab.get("tok-1", now); id != "2" {
		t.Fatalf("the second oldest is %q, want 2", id)
	}
	if id := tab.get("tok-"+strconv.Itoa(linkIDMax), now); id != strconv.Itoa(linkIDMax+1) {
		t.Fatalf("the newest is %q", id)
	}
	// Re-holding a held token never evicts.
	tab.hold("tok-1", "99", now)
	if n := tab.len(); n != linkIDMax {
		t.Fatalf("re-holding changed the size to %d", n)
	}
	// Expiry: the token's fifteen minutes from the hold.
	e := newLinkIDTable()
	e.hold("tok", "5", t0)
	if id := e.get("tok", t0.Add(linkLife-time.Nanosecond)); id != "5" {
		t.Fatalf("an entry inside the token's life reads %q", id)
	}
	if id := e.get("tok", t0.Add(linkLife)); id != "" || e.len() != 0 {
		t.Fatalf("an entry past the token's life reads %q (len %d)", id, e.len())
	}
	// A full table of expired entries is swept, not evicted one by one.
	tab.hold("late", "7", t0.Add(linkLife+time.Hour))
	if n := tab.len(); n != 1 {
		t.Fatalf("after the sweep %d entries, want 1", n)
	}
	// No id holds nothing.
	e.hold("tok", "", t0)
	if e.len() != 0 {
		t.Fatal("an empty id was held")
	}
}

// (e) The ephemeral lines are their own messages: a held link id never turns
// "Uploading…" or the not-applied line into an edit, and posting them holds
// nothing.
func TestBotEphemeralFollowupsIgnoreTheLinkHolder(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	job := formJob(f, nil)
	f.bot.notApplied(linkCtx(t), job)
	if id := <-f.bot.postUploading(linkCtx(t), job); id != "1" {
		t.Fatalf("the uploading line's id is %q", id)
	}
	if n := f.bot.linkIDs.len(); n != 0 {
		t.Fatalf("an ephemeral line was held as the link (%d entries)", n)
	}
	f.bot.linkIDs.hold(testIntToken, "777", time.Now())
	f.bot.notApplied(linkCtx(t), job)
	<-f.bot.postUploading(linkCtx(t), job)
	calls := f.callsSnapshot()
	if n := countCalls(calls, http.MethodPost, f.hook()); n != 4 || len(calls) != 4 {
		t.Fatalf("calls %+v, want four ephemeral POSTs and nothing else", calls)
	}
	if id := f.bot.linkIDs.get(testIntToken, time.Now()); id != "777" {
		t.Fatalf("an ephemeral line moved the held link id to %q", id)
	}
}

// A panic on a token that already holds the link — the in-job redraw is the
// only thing that runs after it — posts no failure sentence under that
// working link, and does not tell the form's table "no clip": the clip exists.
// (TestBotFollowupPanicDeliversByPost is the token with none.)
func TestBotPanicAfterTheLinkPostsNoSentence(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	f.bot.linkIDs.hold(testIntToken, "777", time.Now())
	var failed atomic.Bool
	job := formJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		panic("boom")
	})
	job.Failed = func() { failed.Store(true) }
	f.bot.Clip(job)
	f.quiesce(t)
	if failed.Load() {
		t.Fatal("the panic told the form's table \"no clip\" although the link is out")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = f.bot.Shutdown(ctx)
	if !strings.Contains(f.logs.String(), "/clip panicked") {
		t.Fatalf("the panic is not logged: %s", f.logs.String())
	}
	for _, c := range f.callsSnapshot() {
		if isFollowup(c.method, c.path) || c.method == http.MethodPatch {
			t.Fatalf("a message went out under the held link: %s %s %s", c.method, c.path, c.body)
		}
	}
}

// The twin with no link out: the panic is a failed upload, so the form's
// table IS told "no clip" (a later submit has nothing to apply to) and the
// failure sentence goes out.
func TestBotPanicBeforeTheLinkTellsTheTable(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	var failed atomic.Bool
	job := formJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		panic("boom")
	})
	job.Failed = func() { failed.Store(true) }
	f.bot.Clip(job)
	f.quiesce(t)
	if !failed.Load() {
		t.Fatal("a panic with no link out did not tell the form's table \"no clip\"")
	}
	if !strings.Contains(f.logs.String(), "/clip panicked") {
		t.Fatalf("the panic is not logged: %s", f.logs.String())
	}
	sent := false
	for _, c := range f.callsSnapshot() {
		sent = sent || (isFollowup(c.method, c.path) && strings.Contains(string(c.body), SentenceFailed))
	}
	if !sent {
		t.Fatal("a panic with no link out posted no failure sentence")
	}
}

// redrawPanics wraps the fixture's log handler and panics on the redraw's own
// "edited" line — a panic inside the in-job redraw, after the link is out.
type redrawPanics struct{ slog.Handler }

func (h redrawPanics) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "discord bot: card redraw" {
		edited := false
		r.Attrs(func(a slog.Attr) bool {
			edited = edited || (a.Key == "outcome" && a.Value.String() == "edited")
			return true
		})
		if edited {
			panic("redraw boom")
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h redrawPanics) WithAttrs(as []slog.Attr) slog.Handler {
	return redrawPanics{h.Handler.WithAttrs(as)}
}

func (h redrawPanics) WithGroup(g string) slog.Handler { return redrawPanics{h.Handler.WithGroup(g)} }

// The deferred twin: the reply edited into the link (card not ready, held for
// a redraw), the card turns ready while that edit is in flight, and the in-job
// redraw panics. The recovery must not edit `@original` to the failure
// sentence: the reply stays the link.
func TestBotDeferredPanicAfterTheLinkKeepsTheLink(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	f.bot.log = slog.New(redrawPanics{f.bot.log.Handler()})
	arrived := make(chan struct{})
	release := make(chan struct{})
	var arriveOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	recordAPI(f, func(m, p string, body []byte) (int, string) {
		if m == http.MethodPatch && string(body) == linkBody("") {
			arriveOnce.Do(func() { close(arrived) })
			<-release
		}
		return discordLike(m, p, body)
	})
	var failed atomic.Bool
	job := okJob(f, lateIngest)
	job.Failed = func() { failed.Store(true) }
	f.bot.Clip(job)
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the link was never edited into the reply")
	}
	f.bot.CardReady(testClipID)
	releaseOnce.Do(func() { close(release) })
	f.quiesce(t)
	if failed.Load() {
		t.Fatal("the redraw's panic told the table \"no clip\" although the link is out")
	}
	calls := f.shutdown(t)
	if !strings.Contains(f.logs.String(), "/clip panicked") {
		t.Fatalf("the panic is not logged: %s", f.logs.String())
	}
	orig := f.hook() + "/messages/@original"
	var last string
	for _, c := range calls {
		if strings.Contains(string(c.body), SentenceFailed) || isFollowup(c.method, c.path) {
			t.Fatalf("the failure sentence went out over the link: %v", describe(calls))
		}
		if c.method == http.MethodPatch && c.path == orig {
			last = string(c.body)
		}
	}
	if last != linkBody("?v=2") {
		t.Fatalf("the reply's last edit is %q, want the redrawn link: %v", last, describe(calls))
	}
}
