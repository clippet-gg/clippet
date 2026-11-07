package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/worker"
)

func TestProgress(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")

	queued := e.seedClip(owner, clipOpts{})
	r := e.do("GET", "/api/clips/"+queued.ID+"/progress", nil)
	if r.code != 200 || r.str("state") != "queued" || r.str("stage") != "queued" || r.json["percent"] != float64(0) ||
		r.json["indeterminate"] != false || r.json["error"] != nil || r.str("updated_at") == "" || r.str("id") != queued.ID {
		t.Fatalf("queued: %d %s", r.code, r.body)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", r.hdr.Get("Cache-Control"))
	}

	// processing: 0, then 98 once a checkpoint exists; stage from the job.
	job := e.claim(queued.ID)
	now := e.now()
	if err := e.db.SetJobStage(context.Background(), job.ID, *job.StartedAt, "encode", now); err != nil {
		t.Fatal(err)
	}
	r = e.do("GET", "/api/clips/"+queued.ID+"/progress", nil)
	if r.str("state") != "processing" || r.str("stage") != "encode" || r.json["percent"] != float64(0) {
		t.Fatalf("processing: %s", r.body)
	}
	if err := e.db.SaveCheckpoint(context.Background(), job.ID, *job.StartedAt, `{"ok":true}`, now); err != nil {
		t.Fatal(err)
	}
	if r = e.do("GET", "/api/clips/"+queued.ID+"/progress", nil); r.json["percent"] != float64(98) {
		t.Fatalf("checkpointed: %s", r.body)
	}

	// The worker registry wins when present.
	e.worker.progress[queued.ID] = worker.Progress{State: "processing", Stage: "poster", Percent: 250, Indeterminate: true, UpdatedAt: time.Unix(1_700_000_000, 0)}
	r = e.do("GET", "/api/clips/"+queued.ID+"/progress", nil)
	if r.str("stage") != "poster" || r.json["percent"] != float64(100) || r.json["indeterminate"] != true || r.str("updated_at") != "2023-11-14T22:13:20Z" {
		t.Fatalf("registry: %s", r.body)
	}
	delete(e.worker.progress, queued.ID)

	// ready: 100, done; failed: error carries failure_reason.
	ready, _, _ := e.readyClip(owner, clipOpts{})
	r = e.do("GET", "/api/clips/"+ready.ID+"/progress", nil)
	if r.str("state") != "ready" || r.str("stage") != "done" || r.json["percent"] != float64(100) || r.json["error"] != nil {
		t.Fatalf("ready: %s", r.body)
	}
	// Since Phase 4 the NEWEST job of a ready clip is routinely a trim (or a
	// notify, or a gif), and this DTO's subject is the clip's own encode: the
	// fallback reads the transcode job BY KIND, so a running trim lends the
	// clip neither its stage nor its updated_at (§20 erratum 89).
	readyRow := e.getClip(ready.ID)
	if _, err := e.db.EnqueueTrim(context.Background(), ready.ID,
		db.MarshalTrimParams(db.TrimParams{StartMS: 0, EndMS: 2000}), e.now()); err != nil {
		t.Fatal(err)
	}
	tj := e.claimTrim(ready.ID)
	if err := e.db.SetJobStage(context.Background(), tj.ID, *tj.StartedAt, "encode", readyRow.UpdatedAt+100); err != nil {
		t.Fatal(err)
	}
	r = e.do("GET", "/api/clips/"+ready.ID+"/progress", nil)
	if r.str("state") != "ready" || r.str("stage") != "done" || r.json["percent"] != float64(100) ||
		r.str("updated_at") != rfc3339(readyRow.UpdatedAt) {
		t.Fatalf("a running trim leaked into the clip's progress: %s", r.body)
	}

	failed := e.failedClip(owner, clipOpts{})
	r = e.do("GET", "/api/clips/"+failed.ID+"/progress", nil)
	if r.str("state") != "failed" || r.str("error") != "bad file" {
		t.Fatalf("failed: %s", r.body)
	}

	// 404: unknown, malformed, deleting — never "done".
	for _, p := range []string{strings.Repeat("P", 12), "nope", e.deletingClip(owner).ID} {
		if r = e.do("GET", "/api/clips/"+p+"/progress", nil, withCookie(cookie)); r.code != 404 || r.errCode() != "not_found" {
			t.Fatalf("%s: %d %s", p, r.code, r.body)
		}
	}
}

func TestViewBeacon(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	path := "/api/clips/" + c.ID + "/view"
	views := func() float64 {
		r := e.do("GET", "/api/clips/"+c.ID, nil, withCookie(cookie))
		v, _ := r.json["views"].(float64)
		return v
	}
	ua := withHeader("User-Agent", "player/1")

	if r := e.do("POST", path, nil, ua); r.code != 204 {
		t.Fatalf("first beacon: %d %s", r.code, r.body)
	}
	if views() != 1 {
		t.Fatalf("views = %v after first beacon", views())
	}
	// Same viewer inside the window: 204 but not counted.
	if r := e.do("POST", path, nil, ua); r.code != 204 || views() != 1 {
		t.Fatalf("dedupe: %d views=%v", r.code, views())
	}
	// A DIFFERENT User-Agent from the same address is the same viewer (§20
	// erratum 103): the header is written by the caller, so counting it would
	// let any link holder run the owner's private counter up at the view
	// limiter's rate. Three invented User-Agents, still one view.
	for _, agent := range []string{"player/2", "player/3", ""} {
		if r := e.do("POST", path, nil, withHeader("User-Agent", agent)); r.code != 204 || views() != 1 {
			t.Fatalf("ua %q: %d views=%v, want 204 and 1 (the UA is not part of the key)", agent, r.code, views())
		}
	}
	// A different ADDRESS still counts -- that is the whole key now.
	if r := e.do("POST", path, nil, ua, func(req *http.Request) { req.RemoteAddr = "198.51.100.9:1" }); r.code != 204 || views() != 2 {
		t.Fatalf("other ip: %d views=%v", r.code, views())
	}
	// Forwarding headers are ignored without TRUSTED_PROXIES: still the same viewer.
	if r := e.do("POST", path, nil, ua, withHeader("X-Forwarded-For", "203.0.113.77")); r.code != 204 || views() != 2 {
		t.Fatalf("spoofed xff: %d views=%v", r.code, views())
	}
	// 404 for unknown/deleting; GET is 405.
	if r := e.do("POST", "/api/clips/"+strings.Repeat("V", 12)+"/view", nil); r.code != 404 {
		t.Fatalf("unknown: %d", r.code)
	}
	if r := e.do("POST", "/api/clips/"+e.deletingClip(owner).ID+"/view", nil); r.code != 404 {
		t.Fatalf("deleting: %d", r.code)
	}
	if r := e.do("GET", path, nil); r.code != 405 || r.hdr.Get("Allow") != "POST" {
		t.Fatalf("GET view: %d Allow=%q", r.code, r.hdr.Get("Allow"))
	}
}

// TestViewDedupeIPv6Slash64 pins §20 erratum 292 at the beacon: two IPv6
// addresses in one /64 are ONE viewer inside the dedupe window, and a second
// /64 is a second viewer -- the same rule one IPv4 address always had.
func TestViewDedupeIPv6Slash64(t *testing.T) {
	e := newEnv(t)
	owner, cookie := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	path := "/api/clips/" + c.ID + "/view"
	views := func() float64 {
		r := e.do("GET", "/api/clips/"+c.ID, nil, withCookie(cookie))
		v, _ := r.json["views"].(float64)
		return v
	}
	if r := e.do("POST", path, nil, withRemote("[2001:db8:1:2::1]:1")); r.code != 204 || views() != 1 {
		t.Fatalf("first v6 beacon: %d views=%v", r.code, views())
	}
	if r := e.do("POST", path, nil, withRemote("[2001:db8:1:2:ffff:ffff:ffff:ffff]:1")); r.code != 204 || views() != 1 {
		t.Fatalf("same /64: %d views=%v, want 1 (one dedupe key per /64)", r.code, views())
	}
	if r := e.do("POST", path, nil, withRemote("[2001:db8:1:3::1]:1")); r.code != 204 || views() != 2 {
		t.Fatalf("other /64: %d views=%v, want 2", r.code, views())
	}
}

func TestViewRateLimit(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.newUser("owner")
	c, _, _ := e.readyClip(owner, clipOpts{})
	path := "/api/clips/" + c.ID + "/view"
	for i := 0; i < viewsPerMinute; i++ {
		if r := e.do("POST", path, nil); r.code != 204 {
			t.Fatalf("beacon %d: %d %s", i, r.code, r.body)
		}
	}
	r := e.do("POST", path, nil)
	if r.code != 429 || r.errCode() != "rate_limited" || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("61st beacon: %d %s", r.code, r.body)
	}
}
