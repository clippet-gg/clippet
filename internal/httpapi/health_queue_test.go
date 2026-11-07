package httpapi

import (
	"context"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/worker"
)

// TestHealthzQueueBlockIsAdditiveAndAnonymous pins §20 erratum 145: the queue
// block keeps `queued` and `running` exactly as Phase 1 defined them and gains
// three fields, all of them counts or ages, none of them naming a clip, a user
// or a key.
func TestHealthzQueueBlockIsAdditiveAndAnonymous(t *testing.T) {
	ctx := context.Background()
	d := testDB(t)
	fake := store.NewFake()
	cfg := config.Config{Transcode: config.TranscodeConfig{FFmpegVersion: "7.1.1"}}
	h := NewHandler(Deps{Cfg: cfg, DB: d, Store: fake, Version: "test", Started: time.Now(), Storage: &httpx.StorageHealth{}})

	code, body := get(h, "/healthz")
	if code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	q, ok := body["queue"].(map[string]any)
	if !ok {
		t.Fatalf("no queue block: %v", body)
	}
	for _, key := range []string{"queued", "running", "failed_24h", "oldest_queued_age_s", "longest_running_age_s"} {
		v, present := q[key]
		if !present {
			t.Fatalf("queue.%s is missing: %v", key, q)
		}
		if v != float64(0) {
			t.Fatalf("queue.%s on an empty instance = %v, want 0", key, v)
		}
	}
	if len(q) != 5 {
		t.Fatalf("the queue block has %d fields (%v); the block is a CLOSED set of anonymous numbers", len(q), q)
	}

	// One queued transcode of a real clip, waiting a full minute.
	u, err := d.CreateUser(ctx, ids.New(), "carol", "h", 1)
	if err != nil {
		t.Fatal(err)
	}
	id := ids.New()
	now := time.Now().Unix()
	if _, _, err := d.CreateClipWithJob(ctx, db.NewClip{ID: id, OwnerID: u.ID, Title: "queued clip",
		SourceKey: "clips/" + id + "/original/source.mp4", SourceFilename: "f.mp4", SourceSize: 3,
		SourceContentType: "video/mp4", SourceETag: "e", Now: now - 60}); err != nil {
		t.Fatal(err)
	}
	_, body = get(h, "/healthz")
	q = body["queue"].(map[string]any)
	if q["queued"] != float64(1) {
		t.Fatalf("queued = %v, want 1", q["queued"])
	}
	if age, _ := q["oldest_queued_age_s"].(float64); age < 55 {
		t.Fatalf("oldest_queued_age_s = %v, want about 60", age)
	}
	// Nothing in the block identifies the clip that produced those numbers.
	for k, v := range q {
		if s, isStr := v.(string); isStr {
			t.Fatalf("queue.%s = %q — the block carries numbers only", k, s)
		}
	}
	if _, present := body["clips"]; present {
		t.Fatalf("healthz grew a clip-shaped field: %v", body)
	}
}

// TestHealthzRepairBlock pins §20 erratum 231: /healthz reports what the
// automatic stuck-job repair has DONE and never what it is configured to do —
// the route is anonymous, and a configured value on it would be the §15 line
// `admin doctor` exists to hold.
func TestHealthzRepairBlock(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{Transcode: config.TranscodeConfig{FFmpegVersion: "7.1.1", HardTimeout: 6 * time.Hour},
		JobsUnstickAfter: 6*time.Hour + config.UnstickMargin}

	// No reader wired: the block is absent rather than a row of zeros claiming
	// something about a feature nobody published.
	h := NewHandler(Deps{Cfg: cfg, DB: d, Store: store.NewFake(), Version: "test", Started: time.Now(),
		Storage: &httpx.StorageHealth{}})
	_, body := get(h, "/healthz")
	if _, present := body["repair"]; present {
		t.Fatalf("the repair block is present with no counters wired: %v", body)
	}

	counters := &worker.Repairs{}
	h = NewHandler(Deps{Cfg: cfg, DB: d, Store: store.NewFake(), Version: "test", Started: time.Now(),
		Storage: &httpx.StorageHealth{}, Repairs: counters.Get})
	_, body = get(h, "/healthz")
	r, ok := body["repair"].(map[string]any)
	if !ok {
		t.Fatalf("no repair block: %v", body)
	}
	want := []string{"repaired", "released", "requeued", "failed", "errors"}
	for _, key := range want {
		if v, present := r[key]; !present || v != float64(0) {
			t.Fatalf("repair.%s = %v (present=%v), want 0 before anything is repaired", key, v, present)
		}
	}
	if len(r) != len(want) {
		t.Fatalf("the repair block has %d fields (%v); it is a CLOSED set of anonymous counts", len(r), r)
	}

	// Three repairs of the three shapes; the route reports them and nothing else.
	for _, action := range []string{db.UnstickReleased, db.UnstickRequeued, db.UnstickFailed} {
		counters.Record(action)
	}
	_, body = get(h, "/healthz")
	r = body["repair"].(map[string]any)
	for key, want := range map[string]float64{"repaired": 3, "released": 1, "requeued": 1, "failed": 1, "errors": 0} {
		if r[key] != want {
			t.Errorf("repair.%s = %v, want %v", key, r[key], want)
		}
	}
	// Nothing on this route names the setting behind it.
	for _, forbidden := range []string{"unstick", "after", "jobs_unstick_after", "every"} {
		if _, present := r[forbidden]; present {
			t.Errorf("repair.%s is a configured value on an anonymous route (§15)", forbidden)
		}
	}
}
