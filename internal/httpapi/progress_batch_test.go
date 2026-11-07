package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/worker"
)

func batchOf(t *testing.T, r resp) map[string]ProgressDTO {
	t.Helper()
	var body batchProgress
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("%v: %s", err, r.body)
	}
	return body.Progress
}

// TestBatchProgressAnswersEveryLiveID and drops the ones that are gone: an
// absent key is the batch spelling of the single route's 404, and it must
// never be read as "done".
func TestBatchProgressAnswersEveryLiveID(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	queued := e.seedClip(owner, clipOpts{createdAt: 1000})
	gone := e.seedClip(owner, clipOpts{createdAt: 1001})
	if err := e.db.MarkDeleting(context.Background(), gone.ID, 1002); err != nil {
		t.Fatal(err)
	}
	unknown := ids.New()

	r := e.do("GET", "/api/clips/progress?ids="+strings.Join([]string{queued.ID, gone.ID, unknown}, ","), nil)
	if r.code != 200 {
		t.Fatalf("batch progress = %d %s", r.code, r.body)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", r.hdr.Get("Cache-Control"))
	}
	got := batchOf(t, r)
	if len(got) != 1 {
		t.Fatalf("progress = %+v, want only the live clip", got)
	}
	p, ok := got[queued.ID]
	if !ok || p.State != db.ClipQueued || p.Stage != db.JobStageQueued || p.Percent != 0 {
		t.Fatalf("queued clip = %+v", p)
	}
	if _, ok := got[gone.ID]; ok {
		t.Fatal("a tombstoned clip answered in a batch")
	}
}

// TestBatchProgressMatchesTheSingleRoute: the two routes must agree about the
// same clip in every state, registry entry or not — they are one DTO, and a
// client that switches between them must not see the answer change.
func TestBatchProgressMatchesTheSingleRoute(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	ctx := context.Background()

	queued := e.seedClip(owner, clipOpts{createdAt: 1000})
	running := e.seedClip(owner, clipOpts{createdAt: 1001})
	e.claim(queued.ID)  // oldest first: this claims `queued`'s job
	e.claim(running.ID) // …and this one's
	ready := e.seedClip(owner, clipOpts{createdAt: 1002})
	if err := e.finishTranscode(ready.ID); err != nil {
		t.Fatal(err)
	}
	failed := e.seedClip(owner, clipOpts{createdAt: 1003})
	job := e.claim(failed.ID)
	if _, err := e.db.FailAttempt(ctx, job.ID, db.ClaimStamp(job), failed.ID, "ffmpeg exited 1", db.FailPermanent, db.Limits{}, 1004); err != nil {
		t.Fatal(err)
	}
	// One clip with a live registry entry: the batch must layer it exactly as
	// the single route does, and must NOT let the job row show through.
	e.worker.progress[running.ID] = worker.Progress{
		State: db.ClipProcessing, Stage: "encode", Percent: 42, UpdatedAt: time.Unix(1_700_000_000, 0),
	}

	all := []string{queued.ID, running.ID, ready.ID, failed.ID}
	batch := batchOf(t, e.do("GET", "/api/clips/progress?ids="+strings.Join(all, ","), nil))
	if len(batch) != len(all) {
		t.Fatalf("batch = %+v, want %d entries", batch, len(all))
	}
	for _, id := range all {
		r := e.do("GET", "/api/clips/"+id+"/progress", nil)
		if r.code != 200 {
			t.Fatalf("single progress %s = %d %s", id, r.code, r.body)
		}
		// Compared as JSON: the DTO holds a pointer, and it is the WIRE that
		// has to be identical, not the address the two handlers allocated.
		want := strings.TrimSpace(string(r.body))
		gotJSON, err := json.Marshal(batch[id])
		if err != nil {
			t.Fatal(err)
		}
		if string(gotJSON) != want {
			t.Fatalf("%s: batch %s != single %s", id, gotJSON, want)
		}
	}
	if p := batch[running.ID]; p.Percent != 42 || p.Stage != "encode" {
		t.Fatalf("the registry entry did not win: %+v", p)
	}
	if p := batch[failed.ID]; p.State != db.ClipFailed || p.Error == nil {
		t.Fatalf("a failed clip must carry its reason: %+v", p)
	}
	if p := batch[ready.ID]; p.State != db.ClipReady || p.Percent != 100 || p.Stage != "done" {
		t.Fatalf("a ready clip = %+v", p)
	}
}

// TestBatchProgressRequestShape: the bound, the vocabulary, and what a
// malformed id gets — a 400 the caller cannot miss rather than a silent
// omission that reads as "this clip is gone".
func TestBatchProgressRequestShape(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c := e.seedClip(owner, clipOpts{createdAt: 1000})

	tooMany := make([]string, db.MaxProgressIDs+1)
	for i := range tooMany {
		tooMany[i] = ids.New()
	}
	for _, q := range []string{
		"",
		"?ids=",
		"?ids=%20",
		"?ids=not-an-id",
		"?ids=" + c.ID + ",not-an-id",
		"?ids=" + strings.Join(tooMany, ","),
	} {
		r := e.do("GET", "/api/clips/progress"+q, nil)
		if r.code != 400 || r.errCode() != "invalid_request" {
			t.Fatalf("ids%q: %d %s", q, r.code, r.body)
		}
	}
	// Duplicates collapse into one entry rather than costing a second row.
	r := e.do("GET", "/api/clips/progress?ids="+c.ID+","+c.ID, nil)
	if r.code != 200 || len(batchOf(t, r)) != 1 {
		t.Fatalf("duplicate ids: %d %s", r.code, r.body)
	}
	// Exactly the cap is fine.
	atCap := make([]string, db.MaxProgressIDs)
	for i := range atCap {
		atCap[i] = ids.New()
	}
	atCap[0] = c.ID
	if r = e.do("GET", "/api/clips/progress?ids="+strings.Join(atCap, ","), nil); r.code != 200 {
		t.Fatalf("a request AT the cap: %d %s", r.code, r.body)
	}
	// It is a GET-only literal path, not a clip id.
	if r = e.do("POST", "/api/clips/progress?ids="+c.ID, nil); r.code != 405 {
		t.Fatalf("POST /api/clips/progress = %d", r.code)
	}
}

// TestBatchProgressIsPublicAndNarrowsNothing: it answers the same caller the
// single route answers, and it carries no owner-private field.
func TestBatchProgressIsPublicAndNarrowsNothing(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	hidden := e.seedClip(owner, clipOpts{createdAt: 1000, unlisted: true})

	r := e.do("GET", "/api/clips/progress?ids="+hidden.ID, nil)
	if r.code != 200 {
		t.Fatalf("anonymous batch = %d %s", r.code, r.body)
	}
	if _, ok := batchOf(t, r)[hidden.ID]; !ok {
		t.Fatal("an unlisted clip's progress is public on the single route and must stay so here")
	}
	for _, forbidden := range []string{`"views"`, `"title"`, `"owner"`, `"source"`} {
		if strings.Contains(string(r.body), forbidden) {
			t.Fatalf("the progress batch carries %s: %s", forbidden, r.body)
		}
	}
}
