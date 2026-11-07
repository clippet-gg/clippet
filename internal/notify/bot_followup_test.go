package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The upload form's delivery (§20 erratum 320): after `/upload` answered with a
// modal there is no "thinking…" message to edit, so every message a job ends in
// is a NEW follow-up message on the command's token — and the endpoint's table
// is told when a job ends without a clip.

// isFollowup says a request is "Create Followup Message": POST to the
// interaction's webhook itself, not to one of its messages.
func isFollowup(method, path string) bool {
	return method == http.MethodPost && strings.HasPrefix(path, botAPIPath+"/webhooks/") &&
		!strings.Contains(path, "/messages/")
}

func (f *botFixture) waitFollowed(t *testing.T) apiCall {
	t.Helper()
	select {
	case <-f.followed:
	case <-time.After(5 * time.Second):
		t.Fatalf("no follow-up message was posted; log:\n%s", f.logs.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if isFollowup(c.method, c.path) {
			return c
		}
	}
	t.Fatal("no follow-up recorded")
	return apiCall{}
}

func (f *botFixture) callsSnapshot() []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apiCall(nil), f.calls...)
}

func formJob(f *botFixture, ingest Ingest) ClipJob {
	j := okJob(f, ingest)
	j.Followup = true
	return j
}

func TestBotFollowupPostsTheLinkAsANewMessage(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	f.bot.Clip(formJob(f, func(_ context.Context, src IngestSource) (IngestResult, error) {
		_, _ = io.ReadAll(src.Body)
		return IngestResult{ClipID: "abc123XYZ789", CardReady: true}, nil
	}))
	post := f.waitFollowed(t)
	if want := "/api/v10/webhooks/" + testAppID + "/" + testIntToken; post.path != want {
		t.Fatalf("the follow-up went to %s, want %s", post.path, want)
	}
	if post.auth != "" {
		t.Fatalf("the follow-up carried an Authorization header: %q", post.auth)
	}
	// The same body the edit sends: ClipMessage's, the link alone.
	want, _ := json.Marshal(ClipMessage(f.bot.urls, "abc123XYZ789"))
	if !bytes.Equal(post.body, want) {
		t.Fatalf("the follow-up body is\n%s\nwant ClipMessage's\n%s", post.body, want)
	}
	for _, c := range f.callsSnapshot() {
		if c.method == http.MethodPatch {
			t.Fatalf("a form job PATCHed %s: there is no @original after a modal", c.path)
		}
	}
}

// TestBotFollowupTellsTheTableBeforeTheFailureLine: a CDN non-200 returns
// before the ingest is ever called, and the table must still learn it — before
// the `/clip failed` line, which is the line the E2E waits on.
func TestBotFollowupTellsTheTableBeforeTheFailureLine(t *testing.T) {
	f := newBotFixture(t, http.StatusForbidden, `{"message":"expired"}`)
	var calls atomic.Int32
	var logAtCall string
	job := formJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		t.Error("the ingest ran for a fetch that failed")
		return IngestResult{}, nil
	})
	job.Failed = func() {
		calls.Add(1)
		logAtCall = f.logs.String()
	}
	f.bot.Clip(job)
	if c := content(t, f.waitFollowed(t)); c != SentenceFetch {
		t.Fatalf("the follow-up is %q, want %q", c, SentenceFetch)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("Failed was called %d times, want once", n)
	}
	if strings.Contains(logAtCall, "/clip failed") {
		t.Fatal("Failed ran after the failure line was logged")
	}
	if !strings.Contains(f.logs.String(), "/clip failed") {
		t.Fatalf("no failure line: %s", f.logs.String())
	}
}

func TestBotFailedIsNotCalledOnSuccessAndMayBeNil(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	var called atomic.Bool
	job := okJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		return IngestResult{ClipID: "abc123XYZ789"}, nil
	})
	job.Failed = func() { called.Store(true) }
	f.bot.Clip(job)
	f.waitEdited(t)
	if called.Load() {
		t.Fatal("Failed was called for a job that produced a clip")
	}
	// nil hook, failing job: no panic, the sentence still goes out.
	g := newBotFixture(t, http.StatusForbidden, "")
	g.bot.Clip(okJob(g, func(context.Context, IngestSource) (IngestResult, error) { return IngestResult{}, nil }))
	if c := content(t, g.waitEdited(t)); c != SentenceFetch {
		t.Fatalf("the reply is %q", c)
	}
}

func TestBotFollowupRetriesOnceOnARetryableAnswer(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	var mu sync.Mutex
	var posts []string
	done := make(chan struct{})
	f.answerWith(func(w http.ResponseWriter, r *http.Request) {
		if !isFollowup(r.Method, r.URL.Path) {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		mu.Lock()
		posts = append(posts, r.URL.Path)
		n := len(posts)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusNotFound) // Discord has not processed the modal yet
			_, _ = io.WriteString(w, `{"message":"Unknown Webhook"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"1"}`)
		close(done)
	})
	f.bot.Clip(formJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		return IngestResult{ClipID: "abc123XYZ789"}, nil
	}))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("no retry; log:\n%s", f.logs.String())
	}
	if !strings.Contains(f.logs.String(), "retrying the follow-up") {
		t.Fatalf("the retry is not logged: %s", f.logs.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = f.bot.Shutdown(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 2 {
		t.Fatalf("%d follow-up POSTs, want 2 (one retry)", len(posts))
	}
}

// TestBotFollowupPanicDeliversByPost: the panic recovery used to PATCH
// @original directly; after a modal that would lose the only message the
// channel ever gets.
func TestBotFollowupPanicDeliversByPost(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	var called atomic.Bool
	job := formJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		panic("boom")
	})
	job.Failed = func() { called.Store(true) }
	f.bot.Clip(job)
	if c := content(t, f.waitFollowed(t)); c != SentenceFailed {
		t.Fatalf("the panic's follow-up is %q, want %q", c, SentenceFailed)
	}
	if !called.Load() {
		t.Fatal("the panic did not tell the table")
	}
	for _, c := range f.callsSnapshot() {
		if c.method == http.MethodPatch {
			t.Fatal("the panic recovery PATCHed @original for a form job")
		}
	}
}

func TestBotFollowupScrubsTheInteractionToken(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	var once sync.Once
	hit := make(chan struct{})
	f.answerWith(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(hit) })
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"bad request for `+r.URL.Path+`"}`)
	})
	f.bot.Clip(formJob(f, func(context.Context, IngestSource) (IngestResult, error) {
		return IngestResult{ClipID: "abc123XYZ789"}, nil
	}))
	<-hit
	f.quiesce(t) // the warning is the job goroutine's own last word
	logged := f.logs.String()
	if !strings.Contains(logged, "the follow-up could not be posted") {
		t.Fatalf("no failure line: %s", logged)
	}
	if strings.Contains(logged, testIntToken) {
		t.Fatalf("the INTERACTION token reached the log:\n%s", logged)
	}
}

func TestBotFollowupShutdownSentenceIsAFollowup(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	started := make(chan struct{})
	f.bot.Clip(formJob(f, func(ctx context.Context, _ IngestSource) (IngestResult, error) {
		close(started)
		<-ctx.Done()
		return IngestResult{}, ctx.Err()
	}))
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.bot.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if c := content(t, f.waitFollowed(t)); c != SentenceRestart {
		t.Fatalf("the restart follow-up is %q", c)
	}
}

// TestBotJobRefusedDuringShutdownTellsTheTable: a job that never starts has no
// clip either, so a form submit must not answer "Changes saved." for it.
func TestBotJobRefusedDuringShutdownTellsTheTable(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.bot.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	var called atomic.Bool
	job := formJob(f, func(context.Context, IngestSource) (IngestResult, error) { return IngestResult{}, nil })
	job.Failed = func() { called.Store(true) }
	f.bot.Clip(job)
	if !called.Load() {
		t.Fatal("a job refused during shutdown did not tell the table")
	}
}
