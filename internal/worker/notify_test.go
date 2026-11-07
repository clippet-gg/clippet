package worker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/notify"
	"clippet/internal/urls"
)

// webhookToken is the secret half of every test endpoint: no log line and no
// job row this package writes may contain it.
const webhookToken = "T0KEN-must-never-appear-in-a-log"

// hook is an httptest stand-in for Discord: it records what it was posted and
// answers with whatever the test set, changing its answer per attempt.
type hook struct {
	srv *httptest.Server

	mu      sync.Mutex
	bodies  []string
	replies []reply // consumed in order; the last one repeats
	// posted is closed and replaced on every post, so a test waits for one
	// (waitPosts) instead of polling posts().
	posted chan struct{}
}

type reply struct {
	status     int
	body       string
	retryAfter string
	// gate pins the handler until it is closed: a delivery held in flight for
	// as long as the test needs, without a sleep.
	gate chan struct{}
}

func newHook(t *testing.T, replies ...reply) *hook {
	t.Helper()
	h := &hook{replies: replies, posted: make(chan struct{})}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, string(b))
		rep := h.replies[min(len(h.bodies)-1, len(h.replies)-1)]
		close(h.posted)
		h.posted = make(chan struct{})
		h.mu.Unlock()
		if rep.gate != nil {
			<-rep.gate
		}
		if rep.retryAfter != "" {
			w.Header().Set("Retry-After", rep.retryAfter)
		}
		w.WriteHeader(rep.status)
		if rep.body != "" {
			_, _ = w.Write([]byte(rep.body))
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hook) posts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.bodies...)
}

// waitPosts blocks until the hook has been posted at least n times; a post that
// never arrives fails the test at the hang guard.
func (h *hook) waitPosts(t *testing.T, n int) {
	t.Helper()
	guard := time.NewTimer(settleGuard)
	defer guard.Stop()
	for {
		h.mu.Lock()
		got, next := len(h.bodies), h.posted
		h.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-next:
		case <-guard.C:
			t.Fatalf("the hook was posted %d time(s) within %s, want %d", got, settleGuard, n)
		}
	}
}

func (h *hook) endpoint() string { return h.srv.URL + "/api/webhooks/1234567890/" + webhookToken }

// withNotifier rebuilds the harness's worker with a Notifier pointed at h.
// It must run before start(); the harness is otherwise untouched.
func (h *harness) withNotifier(t *testing.T, endpoint string, timeout time.Duration) {
	t.Helper()
	base, err := url.Parse("https://clips.example")
	if err != nil {
		t.Fatal(err)
	}
	h.w.deps.Notifier = notify.New(notify.Deps{Endpoint: endpoint, URLs: urls.New(base, ""), Timeout: timeout})
}

// waitNotify waits for the clip's notify job to satisfy pred with its run
// settled. It used to poll the row for ten seconds of wall time, and a row is
// committed BEFORE the WARN line that reports it (gate 28a); it now waits on the
// settle hook, so every line the attempt writes is in the buffer when it returns
// (§20 erratum 331).
func (h *harness) waitNotify(t *testing.T, clipID string, pred func(db.Job) bool) db.Job {
	t.Helper()
	return h.awaitJob("notify job for "+clipID, func() (db.Job, bool) {
		jobs := h.notifyJobs(t, clipID)
		if len(jobs) == 0 {
			return db.Job{}, false
		}
		return jobs[0], true
	}, pred)
}

func (h *harness) notifyJobs(t *testing.T, clipID string) []db.Job {
	t.Helper()
	j, err := h.db.GetLatestJob(context.Background(), clipID)
	if err != nil || j.Kind != db.KindNotify {
		return nil
	}
	return []db.Job{j}
}

// readyWithNotify uploads a clip, runs it to ready and returns it. The
// notifier must already be installed, so finalize enqueues the announcement.
func (h *harness) readyWithNotify(t *testing.T, unlisted bool) db.Clip {
	t.Helper()
	c, _ := h.newClip("mp4", 100, true)
	if unlisted {
		if _, err := h.db.UpdateClipMeta(context.Background(), c.ID, db.ClipPatch{Unlisted: &unlisted}, h.clock.Now().Unix()); err != nil {
			t.Fatalf("UpdateClipMeta: %v", err)
		}
	}
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	return c
}

// TestNotifyDelivered: a 2xx marks the job done, the post carries the title
// and the share URL, and nothing about the clip moves.
func TestNotifyDelivered(t *testing.T) {
	h := newHarness(t)
	hk := newHook(t, reply{status: 200, body: `{"id":"1"}`})
	h.withNotifier(t, hk.endpoint(), 0)
	c := h.readyWithNotify(t, false)
	j := h.waitNotify(t, c.ID, func(j db.Job) bool { return j.State == db.JobDone })
	if j.Stage != db.JobStageDelivered || j.LastError == nil || !strings.Contains(*j.LastError, "HTTP 200") {
		t.Fatalf("job = %+v", j)
	}
	posts := hk.posts()
	if len(posts) != 1 {
		t.Fatalf("%d posts, want 1", len(posts))
	}
	if !strings.Contains(posts[0], "https://clips.example/"+c.ID) {
		t.Fatalf("post = %s", posts[0])
	}
	if !strings.Contains(posts[0], `"allowed_mentions":{"parse":[]}`) {
		t.Fatalf("post = %s", posts[0])
	}
	got := h.waitClip(c.ID, db.ClipReady)
	if got.FailureReason != nil {
		t.Fatalf("clip failure_reason = %v", *got.FailureReason)
	}
	h.assertLog("INFO", "discord webhook delivered")
	h.assertNoErrorLogs()
	h.assertNoWebhookLeak(t, hk)
}

// TestNotifyRetriesThenSucceeds: a 500 requeues with the crash backoff, and
// the clip is untouched while the job waits.
func TestNotifyRetriesThenSucceeds(t *testing.T) {
	h := newHarness(t)
	hk := newHook(t, reply{status: 500, body: "upstream sad"}, reply{status: 204})
	h.withNotifier(t, hk.endpoint(), 0)
	c := h.readyWithNotify(t, false)
	j := h.waitNotify(t, c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.Attempts == 1 })
	if j.LastError == nil || !strings.Contains(*j.LastError, "HTTP 500") || !strings.Contains(*j.LastError, "upstream sad") {
		t.Fatalf("job = %+v", j)
	}
	if want := h.clock.Now().Unix() + 30; j.RunAfter != want {
		t.Fatalf("run_after = %d, want %d (30s crash backoff)", j.RunAfter, want)
	}
	got, _ := h.db.GetClip(context.Background(), c.ID)
	if got.State != db.ClipReady || got.FailureReason != nil {
		t.Fatalf("clip = %+v", got)
	}
	// Let the backoff elapse; the second attempt lands.
	h.clock.Advance(31 * time.Second)
	h.w.Wake()
	j = h.waitNotify(t, c.ID, func(j db.Job) bool { return j.State == db.JobDone })
	if j.Attempts != 1 || j.Stage != db.JobStageDelivered {
		t.Fatalf("job = %+v", j)
	}
	h.assertLog("WARN", "discord webhook delivery failed; requeued")
	h.assertNoErrorLogs()
	h.assertNoWebhookLeak(t, hk)
}

// TestNotifyRateLimited: a 429's Retry-After becomes the floor under the
// backoff (it is longer than the 30s the crash class would pick).
func TestNotifyRateLimited(t *testing.T) {
	h := newHarness(t)
	hk := newHook(t, reply{status: 429, retryAfter: "120", body: `{"retry_after":120}`}, reply{status: 200})
	h.withNotifier(t, hk.endpoint(), 0)
	c := h.readyWithNotify(t, false)
	j := h.waitNotify(t, c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.Attempts == 1 })
	if want := h.clock.Now().Unix() + 120; j.RunAfter != want {
		t.Fatalf("run_after = %d, want %d (Retry-After honoured over the 30s backoff)", j.RunAfter, want)
	}
	if j.LastError == nil || !strings.Contains(*j.LastError, "rate limited") {
		t.Fatalf("job = %+v", j)
	}
	h.assertNoErrorLogs()
	h.assertNoWebhookLeak(t, hk)
}

// TestNotifyPermanentRejection: a 400 fails the job at once, with the body
// captured, and never retries.
func TestNotifyPermanentRejection(t *testing.T) {
	h := newHarness(t)
	hk := newHook(t, reply{status: 400, body: `{"message":"Cannot send an empty message","code":50006}`})
	h.withNotifier(t, hk.endpoint(), 0)
	c := h.readyWithNotify(t, false)
	j := h.waitNotify(t, c.ID, func(j db.Job) bool { return j.State == db.JobFailed })
	if j.Attempts != 0 {
		t.Fatalf("a permanent rejection must not burn an attempt: %+v", j)
	}
	if j.LastError == nil || !strings.Contains(*j.LastError, "HTTP 400") || !strings.Contains(*j.LastError, "code\":50006") {
		t.Fatalf("job = %+v", j)
	}
	got, _ := h.db.GetClip(context.Background(), c.ID)
	if got.State != db.ClipReady || got.FailureReason != nil {
		t.Fatalf("a rejected announcement changed the clip: %+v", got)
	}
	// The far end answered; it must not be asked again.
	h.clock.Advance(5 * time.Minute)
	h.w.Wake()
	h.waitIdle()
	if n := len(hk.posts()); n != 1 {
		t.Fatalf("%d posts, want exactly 1", n)
	}
	h.assertLog("WARN", "discord webhook rejected; not retrying")
	h.assertNoErrorLogs()
	h.assertNoWebhookLeak(t, hk)
}

// TestNotifyRedirectIsPermanent: a 3xx is never followed and never retried.
func TestNotifyRedirectIsPermanent(t *testing.T) {
	h := newHarness(t)
	elsewhere := newHook(t, reply{status: 200})
	hk := newHook(t, reply{status: 302})
	h.withNotifier(t, hk.endpoint(), 0)
	c := h.readyWithNotify(t, false)
	j := h.waitNotify(t, c.ID, func(j db.Job) bool { return j.State == db.JobFailed })
	if j.LastError == nil || !strings.Contains(*j.LastError, "redirect is not followed") {
		t.Fatalf("job = %+v", j)
	}
	if n := len(elsewhere.posts()); n != 0 {
		t.Fatalf("the redirect target was posted to %d time(s)", n)
	}
	h.assertNoErrorLogs()
}

// TestNotifyGivesUpAfterThreeAttempts: the cap is a constant, so a worker
// configured with generous transcode caps still stops at three.
func TestNotifyGivesUpAfterThreeAttempts(t *testing.T) {
	h := newHarness(t)
	h.cfg.Transcode.MaxAttempts = 99
	hk := newHook(t, reply{status: 503})
	h.withNotifier(t, hk.endpoint(), 0)
	c := h.readyWithNotify(t, false)
	for attempt := 1; attempt < db.NotifyMaxAttempts; attempt++ {
		h.waitNotify(t, c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.Attempts == attempt })
		h.clock.Advance(11 * time.Minute)
		h.w.Wake()
	}
	j := h.waitNotify(t, c.ID, func(j db.Job) bool { return j.State == db.JobFailed })
	if j.Attempts != db.NotifyMaxAttempts {
		t.Fatalf("job = %+v, want %d attempts", j, db.NotifyMaxAttempts)
	}
	if n := len(hk.posts()); n != db.NotifyMaxAttempts {
		t.Fatalf("%d posts, want %d", n, db.NotifyMaxAttempts)
	}
	got, _ := h.db.GetClip(context.Background(), c.ID)
	if got.State != db.ClipReady || got.FailureReason != nil {
		t.Fatalf("clip = %+v", got)
	}
	// The row flips to failed before the giving-up line is written (the same
	// order as the repair counter, gate 26). waitNotify returned at the job's
	// SETTLE, which is after the line (§20 erratum 331), so it is read once.
	h.assertLog("WARN", "discord webhook not delivered; giving up")
	h.assertNoErrorLogs()
	h.assertNoWebhookLeak(t, hk)
}

// TestNotifyTimeoutIsRetryable: an attempt that never answers is requeued,
// and the error text names no URL.
func TestNotifyTimeoutIsRetryable(t *testing.T) {
	h := newHarness(t)
	// The far end never answers while the test runs: the gate is closed by a
	// cleanup registered after newHook's, so it runs before the server's Close.
	gate := make(chan struct{})
	hk := newHook(t, reply{status: 200, gate: gate})
	t.Cleanup(func() { close(gate) })
	h.withNotifier(t, hk.endpoint(), 100*time.Millisecond)
	c := h.readyWithNotify(t, false)
	j := h.waitNotify(t, c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.Attempts == 1 })
	if j.LastError == nil || !strings.Contains(*j.LastError, "delivery failed") {
		t.Fatalf("job = %+v", j)
	}
	h.assertNoErrorLogs()
	h.assertNoWebhookLeak(t, hk)
}

// TestNotifySkippedWhenClipIsGone: the clip was destroyed between the
// enqueue and the claim — the job resolves as a skip, and nothing is posted.
func TestNotifySkippedWhenClipIsGone(t *testing.T) {
	h := newHarness(t)
	hk := newHook(t, reply{status: 200})
	h.withNotifier(t, hk.endpoint(), 0)
	// Drive the pipeline by hand so the notify job is never claimed while the
	// clip still exists.
	c, job := h.newClip("mp4", 100, true)
	claimed, clip, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	_ = job
	h.w.runJob(context.Background(), claimed, clip)
	njob, _, ok, err := h.db.ClaimNotifyJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok || njob.Kind != db.KindNotify {
		t.Fatalf("claim notify: %+v ok=%v err=%v", njob, ok, err)
	}
	if err := h.db.MarkDeleting(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
		t.Fatalf("MarkDeleting: %v", err)
	}
	if err := h.db.DeleteClipRow(context.Background(), c.ID); err != nil {
		t.Fatalf("DeleteClipRow: %v", err)
	}
	h.w.runNotify(context.Background(), njob, clip)
	if n := len(hk.posts()); n != 0 {
		t.Fatalf("a deleted clip was announced (%d posts)", n)
	}
	h.assertLog("INFO", "discord webhook skipped")
	h.assertNoErrorLogs()
}

// TestNotifyNeverEnqueuedForAnUnlistedClip: the per-upload opt-out. Unlisted
// means "do not surface this", so there is no job at all — no API surface, no
// extra flag, nothing to configure.
func TestNotifyNeverEnqueuedForAnUnlistedClip(t *testing.T) {
	h := newHarness(t)
	hk := newHook(t, reply{status: 200})
	h.withNotifier(t, hk.endpoint(), 0)
	c := h.readyWithNotify(t, true)
	h.waitIdle()
	j, err := h.db.GetLatestJob(context.Background(), c.ID)
	if err != nil || j.Kind != db.KindTranscode {
		t.Fatalf("latest job = %+v (%v), want the transcode job and nothing after it", j, err)
	}
	if n := len(hk.posts()); n != 0 {
		t.Fatalf("an unlisted clip was announced (%d posts)", n)
	}
	h.assertNoErrorLogs()
}

// TestNotifySkippedWhenUnlistedSince: listed at finalize, unlisted before the
// job ran. The opt-out is re-checked at delivery time, so it still wins.
func TestNotifySkippedWhenUnlistedSince(t *testing.T) {
	h := newHarness(t)
	hk := newHook(t, reply{status: 200})
	h.withNotifier(t, hk.endpoint(), 0)
	c, _ := h.newClip("mp4", 100, true)
	claimed, clip, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	h.w.runJob(context.Background(), claimed, clip)
	njob, nclip, ok, err := h.db.ClaimNotifyJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok || njob.Kind != db.KindNotify {
		t.Fatalf("claim notify: %+v ok=%v err=%v", njob, ok, err)
	}
	unlisted := true
	if _, err := h.db.UpdateClipMeta(context.Background(), c.ID, db.ClipPatch{Unlisted: &unlisted}, h.clock.Now().Unix()); err != nil {
		t.Fatalf("UpdateClipMeta: %v", err)
	}
	h.w.runNotify(context.Background(), njob, nclip)
	if n := len(hk.posts()); n != 0 {
		t.Fatalf("a since-unlisted clip was announced (%d posts)", n)
	}
	j, _ := h.db.GetLatestJob(context.Background(), c.ID)
	if j.State != db.JobDone || j.Stage != db.JobStageSkipped || j.LastError == nil || !strings.Contains(*j.LastError, "unlisted") {
		t.Fatalf("job = %+v", j)
	}
	h.assertNoErrorLogs()
}

// TestNotifyUnsetEnqueuesNothing is the §15 promise: with no webhook there is
// no client, no job and no outbound request anywhere.
func TestNotifyUnsetEnqueuesNothing(t *testing.T) {
	h := newHarness(t)
	if h.w.deps.Notifier != nil {
		t.Fatal("the default harness must have no notifier")
	}
	c, _ := h.newClip("mp4", 100, true)
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()
	j, err := h.db.GetLatestJob(context.Background(), c.ID)
	if err != nil || j.Kind != db.KindTranscode {
		t.Fatalf("latest job = %+v (%v), want no notify job", j, err)
	}
	kinds, err := h.db.JobKindCounts(context.Background())
	if err != nil || kinds[db.KindNotify] != 0 {
		t.Fatalf("JobKindCounts = %v (%v)", kinds, err)
	}
	if strings.Contains(h.logs.String(), "discord webhook") {
		t.Fatalf("a webhook log line with no webhook configured:\n%s", h.logs.String())
	}
}

// TestNotifyJobOutlivingItsConfiguration: enqueued with a webhook set,
// claimed after the operator removed it. Nothing is posted (there is no
// client) and the job is resolved rather than retried forever.
func TestNotifyJobOutlivingItsConfiguration(t *testing.T) {
	h := newHarness(t)
	hk := newHook(t, reply{status: 200})
	h.withNotifier(t, hk.endpoint(), 0)
	c, _ := h.newClip("mp4", 100, true)
	claimed, clip, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	h.w.runJob(context.Background(), claimed, clip)
	njob, nclip, ok, err := h.db.ClaimNotifyJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok || njob.Kind != db.KindNotify {
		t.Fatalf("claim notify: %+v ok=%v err=%v", njob, ok, err)
	}
	h.w.deps.Notifier = nil // the operator removed DISCORD_WEBHOOK_URL and restarted
	h.w.runNotify(context.Background(), njob, nclip)
	if n := len(hk.posts()); n != 0 {
		t.Fatalf("%d posts with no notifier configured", n)
	}
	j, _ := h.db.GetLatestJob(context.Background(), c.ID)
	if j.State != db.JobDone || j.LastError == nil || !strings.Contains(*j.LastError, "webhook not configured") {
		t.Fatalf("job = %+v", j)
	}
	h.assertNoErrorLogs()
}

// TestNotifyDoesNotBlockTheTranscodeLoop: the two queues are disjoint (§20
// erratum 67). A delivery pinned in flight — a blackholed Discord costs three
// 10s attempts — must not delay the encode of a clip somebody is waiting for,
// even though its job is OLDER and TRANSCODE_CONCURRENCY is 1.
func TestNotifyDoesNotBlockTheTranscodeLoop(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	hk := newHook(t, reply{status: 200, gate: gate})
	h.withNotifier(t, hk.endpoint(), 30*time.Second)

	// Drive clip A to ready by hand so its notify job is queued BEFORE clip
	// B's transcode job exists: under one FIFO the notify wins on id order.
	a, _ := h.newClip("mp4", 100, true)
	claimed, clip, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	h.w.runJob(context.Background(), claimed, clip)
	b, _ := h.newClip("mp4", 100, true)
	njob, err := h.db.GetLatestJob(context.Background(), a.ID)
	if err != nil || njob.Kind != db.KindNotify {
		t.Fatalf("notify job = %+v (%v)", njob, err)
	}
	bjob, err := h.db.GetLatestJob(context.Background(), b.ID)
	if err != nil || bjob.ID <= njob.ID {
		t.Fatalf("precondition: want b's transcode job newer than notify job %d, got %+v", njob.ID, bjob)
	}

	h.start()
	// The delivery is claimed and pinned inside the handler...
	hk.waitPosts(t, 1)
	// ...and clip B transcodes anyway, on the loop the delivery is not on.
	h.waitClip(b.ID, db.ClipReady)
	if j, _ := h.db.GetLatestJob(context.Background(), a.ID); j.ID != njob.ID || j.State != db.JobRunning {
		t.Fatalf("precondition: the delivery must still be in flight, got %+v", j)
	}

	close(gate)
	h.waitNotify(t, a.ID, func(j db.Job) bool { return j.State == db.JobDone })
	h.assertNoErrorLogs()
	h.assertNoWebhookLeak(t, hk)
}

// TestUnsupportedKindIsStillRefused: adding `notify`, `trim` and `gif` to the
// dispatch must not have opened the door to every other kind. The kind used
// here must stay one nothing implements — it is failed permanently at ERROR,
// which is the whole point: a kind a running binary cannot execute needs a
// human, not a retry.
func TestUnsupportedKindIsStillRefused(t *testing.T) {
	h := newHarness(t)
	c, _ := h.newClip("mp4", 100, true)
	job, clip, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
	if err != nil || !ok {
		t.Fatalf("ClaimJob: %v", err)
	}
	job.Kind = "hls"
	h.w.runJob(context.Background(), job, clip)
	got, _ := h.db.GetLatestJob(context.Background(), c.ID)
	if got.State != db.JobFailed || got.LastError == nil || !strings.Contains(*got.LastError, "unsupported job kind: hls") {
		t.Fatalf("job = %+v", got)
	}
}

// assertNoWebhookLeak: neither the endpoint nor its token may appear in any
// log line or in any job row this run wrote.
func (h *harness) assertNoWebhookLeak(t *testing.T, hk *hook) {
	t.Helper()
	logs := h.logs.String()
	for _, needle := range []string{webhookToken, hk.endpoint(), hk.srv.URL} {
		if strings.Contains(logs, needle) {
			t.Fatalf("the logs leak %q:\n%s", needle, logs)
		}
	}
	rows, err := h.db.ListAllClipIDsAndKeys(context.Background())
	if err != nil {
		t.Fatalf("list clips: %v", err)
	}
	for _, r := range rows {
		j, err := h.db.GetLatestJob(context.Background(), r.ID)
		if err != nil || j.LastError == nil {
			continue
		}
		for _, needle := range []string{webhookToken, hk.srv.URL} {
			if strings.Contains(*j.LastError, needle) {
				t.Fatalf("job %d last_error leaks %q: %s", j.ID, needle, *j.LastError)
			}
		}
	}
}
