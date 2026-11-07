package worker

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/notify"
	"clippet/internal/urls"
)

// The clip webhook's worker half (ROADMAP #54, §20 erratum 319). The receiver
// is the notify tests' httptest stand-in (`hook`, notify_test.go); the type
// name is theirs, not this feature's.

const (
	clipHookToken  = "clip-hook-T0KEN-must-never-appear"
	clipHookSecret = "clip-hook-secret-0123456789abcdef!!"
)

func clipHookEndpoint(hk *hook) string { return hk.srv.URL + "/in?k=" + clipHookToken }

// withClipHook installs a Hook pointed at hk. It must run before start().
func (h *harness) withClipHook(t *testing.T, hk *hook, timeout time.Duration) {
	t.Helper()
	base, err := url.Parse("https://clips.example")
	if err != nil {
		t.Fatal(err)
	}
	h.w.deps.Hook = notify.NewHook(notify.HookDeps{Endpoint: clipHookEndpoint(hk), Secret: clipHookSecret,
		URLs: urls.New(base, ""), Timeout: timeout})
}

// hookJob returns the clip's one `hook` job, or the zero Job.
func (h *harness) hookJob(t *testing.T, clipID string) db.Job {
	t.Helper()
	jobs, err := h.db.ListJobs(context.Background(), db.JobQuery{ClipID: clipID, Kind: db.KindHook})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) == 0 {
		return db.Job{}
	}
	return jobs[0]
}

// waitHook waits, on the settle hook, for the clip's hook job to match pred
// with its run settled (awaitJob).
func (h *harness) waitHook(t *testing.T, clipID string, pred func(db.Job) bool) db.Job {
	t.Helper()
	return h.awaitJob("hook job for "+clipID, func() (db.Job, bool) {
		j := h.hookJob(t, clipID)
		return j, j.ID != 0
	}, pred)
}

func (h *harness) assertNoClipHookLeak(t *testing.T, hk *hook) {
	t.Helper()
	logs := h.logs.String()
	for _, needle := range []string{clipHookToken, clipHookSecret, clipHookEndpoint(hk)} {
		if strings.Contains(logs, needle) {
			t.Fatalf("the logs leak %q:\n%s", needle, logs)
		}
	}
	jobs, _ := h.db.ListJobs(context.Background(), db.JobQuery{Kind: db.KindHook})
	for _, j := range jobs {
		if j.ParamsJSON != "{}" {
			t.Fatalf("a hook job stores %q", j.ParamsJSON)
		}
		if j.LastError != nil && (strings.Contains(*j.LastError, clipHookToken) || strings.Contains(*j.LastError, clipHookSecret)) {
			t.Fatalf("a hook job row leaks: %q", *j.LastError)
		}
	}
}

// TestClipHookDelivered: one signed document per new clip, the unlisted one
// included and marked, the clip untouched, no Discord line anywhere.
func TestClipHookDelivered(t *testing.T) {
	h := newHarness(t)
	hk := newHook(t, reply{status: 200})
	h.withClipHook(t, hk, 0)
	c := h.readyWithNotify(t, true) // unlisted
	j := h.waitHook(t, c.ID, func(j db.Job) bool { return j.State == db.JobDone })
	if j.Stage != db.JobStageDelivered || j.LastError == nil || *j.LastError != "delivered: HTTP 200" || j.Attempts != 0 {
		t.Fatalf("job = %+v", j)
	}
	posts := hk.posts()
	if len(posts) != 1 {
		t.Fatalf("%d posts, want 1", len(posts))
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(posts[0]), &doc); err != nil {
		t.Fatalf("body %q: %v", posts[0], err)
	}
	if doc["event"] != "clip.ready" || doc["id"] != c.ID || doc["url"] != "https://clips.example/"+c.ID ||
		doc["owner"] != "carol" || doc["unlisted"] != true {
		t.Fatalf("doc = %v", doc)
	}
	got := h.waitClip(c.ID, db.ClipReady)
	if got.FailureReason != nil {
		t.Fatalf("clip failure_reason = %v", *got.FailureReason)
	}
	h.assertLog("INFO", "clip webhook delivered")
	if strings.Contains(h.logs.String(), "discord webhook") {
		t.Fatalf("a hook run wrote a Discord line:\n%s", h.logs.String())
	}
	h.assertNoErrorLogs()
	h.assertNoClipHookLeak(t, hk)
}

// TestClipHookRetriesThenGivesUp: five attempts on the crash backoff, WARN
// every time, the clip ready throughout.
func TestClipHookRetriesThenGivesUp(t *testing.T) {
	h := newHarness(t)
	h.cfg.Transcode.MaxAttempts = 99
	hk := newHook(t, reply{status: 503, body: "down " + clipHookToken})
	h.withClipHook(t, hk, 0)
	c := h.readyWithNotify(t, false)
	for attempt := 1; attempt < db.HookMaxAttempts; attempt++ {
		h.waitHook(t, c.ID, func(j db.Job) bool { return j.State == db.JobQueued && j.Attempts == attempt })
		h.clock.Advance(11 * time.Minute)
		h.w.Wake()
	}
	j := h.waitHook(t, c.ID, func(j db.Job) bool { return j.State == db.JobFailed })
	if j.Attempts != db.HookMaxAttempts {
		t.Fatalf("job = %+v, want %d attempts", j, db.HookMaxAttempts)
	}
	if n := len(hk.posts()); n != db.HookMaxAttempts {
		t.Fatalf("%d posts, want %d", n, db.HookMaxAttempts)
	}
	got, _ := h.db.GetClip(context.Background(), c.ID)
	if got.State != db.ClipReady || got.FailureReason != nil {
		t.Fatalf("clip = %+v", got)
	}
	h.assertLog("WARN", "clip webhook delivery failed; requeued")
	h.assertLog("WARN", "clip webhook not delivered; giving up")
	h.assertNoErrorLogs()
	h.assertNoClipHookLeak(t, hk)
}

// TestClipHookRejected: a 4xx fails once and is never asked again.
func TestClipHookRejected(t *testing.T) {
	h := newHarness(t)
	hk := newHook(t, reply{status: 404, body: "no such hook"})
	h.withClipHook(t, hk, 0)
	c := h.readyWithNotify(t, false)
	j := h.waitHook(t, c.ID, func(j db.Job) bool { return j.State == db.JobFailed })
	if j.Attempts != 0 || j.LastError == nil || !strings.Contains(*j.LastError, "rejected: HTTP 404") {
		t.Fatalf("job = %+v", j)
	}
	h.clock.Advance(10 * time.Minute)
	h.w.Wake()
	h.waitIdle()
	if n := len(hk.posts()); n != 1 {
		t.Fatalf("%d posts, want 1", n)
	}
	h.assertLog("WARN", "clip webhook rejected; not retrying")
	h.assertNoErrorLogs()
	h.assertNoClipHookLeak(t, hk)
}

// TestClipHookSkips: gone, deleting, and a configuration that went away all
// resolve as `done` with the reason, and nothing is sent.
func TestClipHookSkips(t *testing.T) {
	for _, tc := range []struct {
		name, note string
		before     func(h *harness, clipID string)
	}{
		{"deleted", "skipped: clip deleted", func(h *harness, id string) {
			_ = h.db.MarkDeleting(context.Background(), id, h.clock.Now().Unix())
			_ = h.db.DeleteClipRow(context.Background(), id)
		}},
		{"deleting", "skipped: clip is being deleted", func(h *harness, id string) {
			_ = h.db.MarkDeleting(context.Background(), id, h.clock.Now().Unix())
		}},
		{"unconfigured", "skipped: webhook not configured", func(h *harness, _ string) {
			h.w.deps.Hook = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			hk := newHook(t, reply{status: 200})
			h.withClipHook(t, hk, 0)
			c, _ := h.newClip("mp4", 100, true)
			claimed, clip, ok, err := h.db.ClaimJob(context.Background(), h.clock.Now().Unix())
			if err != nil || !ok {
				t.Fatalf("claim: %v", err)
			}
			h.w.runJob(context.Background(), claimed, clip)
			job, jclip, ok, err := h.db.ClaimNotifyJob(context.Background(), h.clock.Now().Unix())
			if err != nil || !ok || job.Kind != db.KindHook {
				t.Fatalf("claim hook: %+v ok=%v err=%v", job, ok, err)
			}
			tc.before(h, c.ID)
			h.w.runJob(context.Background(), job, jclip)
			if n := len(hk.posts()); n != 0 {
				t.Fatalf("%d posts", n)
			}
			// MarkDeleting cancels the clip's jobs, so only the unconfigured
			// case still owns its row to record the note in.
			if tc.name == "unconfigured" {
				j := h.hookJob(t, c.ID)
				if j.State != db.JobDone || j.Stage != db.JobStageSkipped || j.LastError == nil || *j.LastError != tc.note {
					t.Fatalf("job = %+v", j)
				}
			}
			h.assertLog("INFO", "clip webhook skipped")
			h.assertNoErrorLogs()
		})
	}
}

// TestClipHookUnsetEnqueuesNothing: off means no job, no line, no request.
func TestClipHookUnsetEnqueuesNothing(t *testing.T) {
	h := newHarness(t)
	if h.w.deps.Hook != nil {
		t.Fatal("the default harness must have no hook")
	}
	c, _ := h.newClip("mp4", 100, true)
	h.start()
	h.waitClip(c.ID, db.ClipReady)
	h.waitIdle()
	kinds, err := h.db.JobKindCounts(context.Background())
	if err != nil || kinds[db.KindHook] != 0 {
		t.Fatalf("JobKindCounts = %v (%v)", kinds, err)
	}
	if strings.Contains(h.logs.String(), "clip webhook") {
		t.Fatalf("a clip webhook line with no webhook configured:\n%s", h.logs.String())
	}
}

// TestClipHookShutdownReleases: a delivery cut short by Stop() goes back to
// queued with its counters untouched.
func TestClipHookShutdownReleases(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	hk := newHook(t, reply{status: 200, gate: gate})
	// Registered AFTER newHook, so it runs BEFORE the server's Close, which
	// would otherwise wait for a handler pinned on the gate.
	t.Cleanup(func() { close(gate) })
	h.withClipHook(t, hk, 30*time.Second)
	c := h.readyWithNotify(t, false)
	hk.waitPosts(t, 1)
	_ = h.w.Stop(context.Background())
	j := h.waitHook(t, c.ID, func(j db.Job) bool { return j.State == db.JobQueued })
	if j.Attempts != 0 || j.TransientFailures != 0 {
		t.Fatalf("a released delivery spent its budget: %+v", j)
	}
	h.assertLog("INFO", "hook job released")
	h.assertNoErrorLogs()
}
