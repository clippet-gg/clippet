package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
)

// failClip drives the fixture's queued clip Y to `failed` the way the worker
// would, so the recovery verbs have a real subject.
func (fx *adminFixture) failClip(t *testing.T, id string) db.Job {
	t.Helper()
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	j, c, ok, err := d.ClaimJob(ctx, fx.now.Unix())
	if err != nil || !ok || c.ID != id {
		t.Fatalf("claim %s: %v %v", id, ok, err)
	}
	if out, err := d.FailAttempt(ctx, j.ID, db.ClaimStamp(j), id, "the file is not a video", db.FailPermanent, db.Limits{}, fx.now.Unix()); err != nil || out != db.OutcomeFailed {
		t.Fatalf("fail: %v %v", out, err)
	}
	return j
}

// claimY claims the fixture's queued clip and leaves the job `running`.
func (fx *adminFixture) claimY(t *testing.T) db.Job {
	t.Helper()
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	j, _, ok, err := d.ClaimJob(context.Background(), fx.now.Unix())
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	return j
}

// saveCheckpoint is §10.3 step 8 for the fixture: the outputs are uploaded and
// their keys recorded on the JOB, before any column names them.
func (fx *adminFixture) saveCheckpoint(t *testing.T, j db.Job, checkpointJSON string) {
	t.Helper()
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.SaveCheckpoint(context.Background(), j.ID, *j.StartedAt, checkpointJSON, fx.now.Unix()); err != nil {
		t.Fatal(err)
	}
}

// enqueueGIF puts a live (queued) gif job on a ready clip — a kind that DOES
// write objects, and so is inside the live-job interlock.
func (fx *adminFixture) enqueueGIF(t *testing.T, clipID string) {
	t.Helper()
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.EnqueueGIF(context.Background(), clipID, fx.now.Unix()); err != nil {
		t.Fatalf("EnqueueGIF: %v", err)
	}
}

// finalizeY is §10.3 step 9 for the fixture: the checkpointed retry publishes
// the keys it already uploaded, which is what makes deleting them fatal.
func (fx *adminFixture) finalizeY(t *testing.T, jobID int64, rendition, poster string) {
	t.Helper()
	fx.finalizeYNotifying(t, jobID, rendition, poster, false)
}

// finalizeYNotifying is finalizeY with the one bit that decides whether the
// same commit queues the Discord announcement (§10.3 step 9, notify half).
func (fx *adminFixture) finalizeYNotifying(t *testing.T, jobID int64, rendition, poster string, notify bool) {
	t.Helper()
	d, err := openForTest(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ok, _, err := d.FinalizeTranscode(context.Background(), jobID, fx.y, db.ReadyFields{
		RenditionKey: rendition, RenditionSize: 4, RenditionETag: "re",
		PosterKey: poster, PosterSize: 2, PosterETag: "pe", PosterWidth: 1280, PosterHeight: 720,
		Width: 1280, Height: 720, DurationMS: 6000, FPSNum: 30, FPSDen: 1, HasAudio: true,
	}, notify, fx.now.Unix())
	if err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
}

func TestAdminJobsListsAndFilters(t *testing.T) {
	fx := newAdminFixture(t)
	code, out := fx.run(t, "jobs")
	if code != 0 {
		t.Fatalf("jobs: %d %q", code, out)
	}
	// Two clips → two transcode jobs; X's is done, Y's is queued.
	if !strings.Contains(out, "2 job(s)") {
		t.Fatalf("jobs count:\n%s", out)
	}
	if !strings.Contains(out, "clip="+fx.x) || !strings.Contains(out, "clip="+fx.y) {
		t.Fatalf("jobs did not name both clips:\n%s", out)
	}
	if !strings.Contains(out, "totals:  transcode  queued=1 running=0 done=1 failed=0 cancelled=0") {
		t.Fatalf("per-kind totals:\n%s", out)
	}
	// Every kind is on the summary even with no rows, so "no gif job exists"
	// is an answer rather than a silence.
	for _, kind := range []string{"notify", "trim", "gif"} {
		if !strings.Contains(out, "totals:  "+kind) {
			t.Fatalf("totals line for %s is missing:\n%s", kind, out)
		}
	}

	code, out = fx.run(t, "jobs", "--state", "queued")
	if code != 0 || !strings.Contains(out, "1 job(s) (state=queued)") || strings.Contains(out, "clip="+fx.x) {
		t.Fatalf("--state queued: %d\n%s", code, out)
	}
	code, out = fx.run(t, "jobs", "--clip", fx.x)
	if code != 0 || !strings.Contains(out, "1 job(s) (clip="+fx.x+")") {
		t.Fatalf("--clip: %d\n%s", code, out)
	}
	// The queued job carries the age an operator decides on.
	if code, out := fx.run(t, "jobs", "--state", "queued"); code != 0 || !strings.Contains(out, "queued_for=") {
		t.Fatalf("queued age:\n%s", out)
	}
	// A typo is a usage error, never an empty listing.
	for _, args := range [][]string{
		{"jobs", "--state", "redy"},
		{"jobs", "--kind", "transcod"},
		{"jobs", "--clip", "not-an-id"},
		{"jobs", "--limit", "0"},
		{"jobs", "stray"},
	} {
		if code, _ := fx.run(t, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
}

func TestAdminJobsErrorTextIsQuotedAndBounded(t *testing.T) {
	fx := newAdminFixture(t)
	fx.failClip(t, fx.y)
	code, out := fx.run(t, "jobs", "--state", "failed")
	if code != 0 {
		t.Fatalf("jobs --state failed: %d\n%s", code, out)
	}
	if !strings.Contains(out, `error="the file is not a video"`) {
		t.Fatalf("last_error is not quoted:\n%s", out)
	}
	// One line per job, whatever the error contains.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "#") && strings.Count(line, "error=") != 1 {
			t.Fatalf("a job line carries more than one error field: %q", line)
		}
	}
}

func TestAdminLsStateAndOwnerFilters(t *testing.T) {
	fx := newAdminFixture(t)
	code, out := fx.run(t, "ls", "--state", "ready")
	if code != 0 || !strings.Contains(out, fx.x) || strings.Contains(out, fx.y) ||
		!strings.Contains(out, "1 clips (state=ready)") {
		t.Fatalf("ls --state ready: %d\n%s", code, out)
	}
	code, out = fx.run(t, "ls", "--state", "failed")
	if code != 0 || !strings.Contains(out, "no clips (state=failed)") {
		t.Fatalf("ls --state failed on a healthy instance: %d\n%s", code, out)
	}
	fx.failClip(t, fx.y)
	code, out = fx.run(t, "ls", "--state", "failed")
	if code != 0 || !strings.Contains(out, fx.y) || !strings.Contains(out, "1 clips (state=failed)") {
		t.Fatalf("ls --state failed: %d\n%s", code, out)
	}
	code, out = fx.run(t, "ls", "--owner", "carol")
	if code != 0 || !strings.Contains(out, "2 clips (owner=carol)") {
		t.Fatalf("ls --owner: %d\n%s", code, out)
	}
	if code, out := fx.run(t, "ls", "--owner", "nobody"); code != 1 || !strings.Contains(out, "no such user") {
		t.Fatalf("ls --owner for an unknown user: %d\n%s", code, out)
	}
	// A typo in --state is a usage error; a filter cannot be combined with an id.
	for _, args := range [][]string{
		{"ls", "--state", "readyy"},
		{"ls", fx.x, "--state", "ready"},
		{"ls", fx.x, "--owner", "carol"},
	} {
		if code, _ := fx.run(t, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
}

func TestAdminRetryOnlyRequeuesAFailedClipWhoseOriginalExists(t *testing.T) {
	fx := newAdminFixture(t)
	// A READY clip is never re-queued: the §5 invariant a second process must
	// not be able to break.
	code, out := fx.run(t, "retry", fx.x)
	if code != 1 || !strings.Contains(out, "not failed (state=ready)") {
		t.Fatalf("retry a ready clip: %d\n%s", code, out)
	}
	if c := fx.clip(t, fx.x); c.State != db.ClipReady {
		t.Fatalf("a refused retry moved the clip to %s", c.State)
	}
	// A queued one is refused too.
	if code, out := fx.run(t, "retry", fx.y); code != 1 || !strings.Contains(out, "not failed (state=queued)") {
		t.Fatalf("retry a queued clip: %d\n%s", code, out)
	}

	fx.failClip(t, fx.y)
	code, out = fx.run(t, "retry", fx.y)
	if code != 0 || !strings.Contains(out, fx.y+": re-queued") || !strings.Contains(out, "retry:   1 re-queued, 0 skipped") {
		t.Fatalf("retry a failed clip: %d\n%s", code, out)
	}
	if c := fx.clip(t, fx.y); c.State != db.ClipQueued || c.FailureReason != nil {
		t.Fatalf("clip after retry: state=%s reason=%v", c.State, c.FailureReason)
	}

	// The original is what a retry re-derives everything from: without it the
	// retry would only fail again, so it is refused and says so.
	fx.failClip(t, fx.y)
	if _, err := fx.fake.DeletePrefix(context.Background(), store.OriginalKey("", fx.y, "mp4")); err != nil {
		t.Fatal(err)
	}
	code, out = fx.run(t, "retry", fx.y)
	if code != 1 || !strings.Contains(out, "the original is missing from the bucket") {
		t.Fatalf("retry without an original: %d\n%s", code, out)
	}
	if c := fx.clip(t, fx.y); c.State != db.ClipFailed {
		t.Fatalf("a refused retry moved the clip to %s", c.State)
	}
}

func TestAdminRetrySelectorsAndAllFailed(t *testing.T) {
	fx := newAdminFixture(t)
	// A selector is required, and it is one OR the other.
	for _, args := range [][]string{
		{"retry"},
		{"retry", fx.y, "--all-failed"},
	} {
		if code, _ := fx.run(t, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
	// Nothing failed: a clean zero, not an error.
	if code, out := fx.run(t, "retry", "--all-failed"); code != 0 || !strings.Contains(out, "no failed clips") {
		t.Fatalf("--all-failed with nothing failed: %d\n%s", code, out)
	}
	fx.failClip(t, fx.y)
	code, out := fx.run(t, "retry", "--all-failed")
	if code != 0 || !strings.Contains(out, fx.y+": re-queued") || !strings.Contains(out, "retry:   1 re-queued, 0 skipped") {
		t.Fatalf("--all-failed: %d\n%s", code, out)
	}
	if c := fx.clip(t, fx.y); c.State != db.ClipQueued {
		t.Fatalf("clip after --all-failed: %s", c.State)
	}
	// An unknown id is one answer and exit 1, exactly as `expire`'s is.
	if code, out := fx.run(t, "retry", "aZ3kQ9xB2mLp"); code != 1 || !strings.Contains(out, "not found") {
		t.Fatalf("unknown id: %d\n%s", code, out)
	}
	if code, out := fx.run(t, "retry", "not-an-id"); code != 1 || !strings.Contains(out, "not found") {
		t.Fatalf("malformed id: %d\n%s", code, out)
	}
}

func TestAdminJobsUnstickNeedsASelectorAndHonoursTheAgeFloor(t *testing.T) {
	fx := newAdminFixture(t)
	j := fx.claimY(t)

	for _, args := range [][]string{
		{"jobs", "unstick"},
		{"jobs", "unstick", "7", "--older-than", "8h"},
		{"jobs", "unstick", "not-a-number"},
		{"jobs", "unstick", "0"},
		// Trailing garbage is a usage error, never job #1: a `#41`, `41,` or
		// `41x` pasted out of a listing must not repair a job nobody named.
		{"jobs", "unstick", itoa(j.ID) + "x"},
		{"jobs", "unstick", "#" + itoa(j.ID)},
		{"jobs", "unstick", itoa(j.ID) + ","},
		{"jobs", "unstick", "1e9"},
		{"jobs", "unstick", "-1"},
		// strconv.ParseInt accepts a leading sign, so `+41` used to parse as
		// the job it names. The consequence was nil — it repairs exactly that
		// job — but the selector is spelled "the number `admin jobs` prints
		// after '#'", and that number carries no sign (N039). The full-width
		// pair makes the same claim about the digit set: ASCII 0-9 only.
		{"jobs", "unstick", "+" + itoa(j.ID)},
		{"jobs", "unstick", "＋" + itoa(j.ID)},
		{"jobs", "unstick", "４１"},
	} {
		if code, _ := fx.run(t, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
	// …and none of those touched the job they merely started with.
	if c := fx.clip(t, fx.y); c.State != db.ClipProcessing {
		t.Fatalf("a rejected selector moved the clip to %s", c.State)
	}

	// The clock has not moved, so the job was claimed "now": the floor refuses
	// it, and refusing must change nothing.
	code, out := fx.run(t, "jobs", "unstick", itoa(j.ID))
	if code != 1 || !strings.Contains(out, "a worker may still be on it") {
		t.Fatalf("a fresh job: %d\n%s", code, out)
	}
	if c := fx.clip(t, fx.y); c.State != db.ClipProcessing {
		t.Fatalf("a refused unstick moved the clip to %s", c.State)
	}
	// A short --older-than is RAISED to TRANSCODE_HARD_TIMEOUT, out loud.
	code, out = fx.run(t, "jobs", "unstick", "--older-than", "1s")
	if code != 0 || !strings.Contains(out, "raised to the TRANSCODE_HARD_TIMEOUT floor of 6h") ||
		!strings.Contains(out, "unstuck: 0 job(s)") {
		t.Fatalf("--older-than under the floor: %d\n%s", code, out)
	}
	if c := fx.clip(t, fx.y); c.State != db.ClipProcessing {
		t.Fatalf("a sub-floor sweep repaired a live job: %s", c.State)
	}

	// Move the clock past the floor: now it is repairable, and the clip follows
	// because this is a transcode job (§5).
	fx.now = fx.now.Add(7 * time.Hour)
	code, out = fx.run(t, "jobs", "unstick", itoa(j.ID))
	if code != 0 || !strings.Contains(out, "job requeued, clip queued") || !strings.Contains(out, "unstuck: 1 job(s)") {
		t.Fatalf("unstick a stale job: %d\n%s", code, out)
	}
	if c := fx.clip(t, fx.y); c.State != db.ClipQueued {
		t.Fatalf("clip after unstick: %s", c.State)
	}
	// Repeating it is a clean refusal.
	if code, out := fx.run(t, "jobs", "unstick", itoa(j.ID)); code != 1 || !strings.Contains(out, "not running") {
		t.Fatalf("second unstick: %d\n%s", code, out)
	}
	if code, out := fx.run(t, "jobs", "unstick", "999999"); code != 1 || !strings.Contains(out, "no such job") {
		t.Fatalf("unknown job: %d\n%s", code, out)
	}
}

func TestAdminJobsUnstickSweepsEveryStaleJob(t *testing.T) {
	fx := newAdminFixture(t)
	fx.claimY(t)
	fx.now = fx.now.Add(7 * time.Hour)
	code, out := fx.run(t, "jobs", "unstick", "--older-than", "6h")
	if code != 0 || !strings.Contains(out, "unstuck: 1 job(s) running since before") {
		t.Fatalf("sweep: %d\n%s", code, out)
	}
	if code, out := fx.run(t, "jobs", "unstick", "--older-than", "6h"); code != 0 || !strings.Contains(out, "unstuck: 0 job(s)") {
		t.Fatalf("second sweep: %d\n%s", code, out)
	}
}

// The floor bounds ONE ENCODE, not a job's whole run, and an instance that
// lowered TRANSCODE_HARD_TIMEOUT can hold live jobs older than it — a large
// original is still being acquired under STORAGE_TRANSFER_MAX. The command
// says so rather than claiming a proof it does not have.
func TestAdminJobsUnstickNamesWhatTheFloorCannotBound(t *testing.T) {
	fx := newAdminFixture(t)
	fx.cfg.Transcode.HardTimeout = 30 * time.Minute
	code, out := fx.run(t, "jobs", "unstick", "--older-than", "45m")
	if code != 0 {
		t.Fatalf("sweep: %d\n%s", code, out)
	}
	if !strings.Contains(out, "STORAGE_TRANSFER_MAX (6h0m0s)") || !strings.Contains(out, "running_for=") {
		t.Fatalf("the caution does not name the bound the floor is not:\n%s", out)
	}
	// At the shipped defaults the floor and the transfer bound are the same 6h,
	// and a caution that printed on every run would be noise, not a caution.
	quiet := newAdminFixture(t)
	if code, out := quiet.run(t, "jobs", "unstick", "--older-than", "8h"); code != 0 || strings.Contains(out, "STORAGE_TRANSFER_MAX") {
		t.Fatalf("the caution printed at the defaults: %d\n%s", code, out)
	}
}

func TestAdminStatsReportsFreeDiskAndTheQueueAges(t *testing.T) {
	fx := newAdminFixture(t)
	// The fixture's SCRATCH_DIR is the image default (/scratch), which exists
	// on the stack and not in a test container; point it somewhere real so the
	// disk line is exercised rather than degrading to "unavailable".
	fx.cfg.ScratchDir = t.TempDir()
	code, out := fx.run(t, "stats", "--no-storage")
	if code != 0 {
		t.Fatalf("stats: %d\n%s", code, out)
	}
	if !strings.Contains(out, "disk:    data_free=") || !strings.Contains(out, "scratch_free=") ||
		!strings.Contains(out, "min_free=") || !strings.Contains(out, "same_device=") {
		t.Fatalf("the disk line is missing or incomplete:\n%s", out)
	}
	if !strings.Contains(out, "SCRATCH_MIN_FREE_BYTES=") {
		t.Fatalf("the human disk line is missing:\n%s", out)
	}
	if !strings.Contains(out, "queue:   failed_24h=0 oldest_queued_age_s=") || !strings.Contains(out, "longest_running_age_s=") {
		t.Fatalf("the queue line is missing:\n%s", out)
	}
}

func TestAdminOrphansReportsAndDeletesExtraObjectsUnderALiveClip(t *testing.T) {
	fx := newAdminFixture(t)
	// A superseded rendition and poster of the READY clip: exactly what a trim
	// leaves behind when the process dies between the commit and the
	// best-effort delete.
	stale := store.RenditionKey("", fx.x, "oldoldol")
	stalePoster := store.PosterKey("", fx.x, "oldoldol")
	fx.fake.Put(stale, []byte("superseded"), "video/mp4")
	fx.fake.Put(stalePoster, []byte("old"), "image/jpeg")
	// The probe JSON is the ONE unreferenced key that is legitimate.
	fx.fake.Put(store.ProbeKey("", fx.x), []byte("{}"), "application/json")

	// Fresh objects are skipped: an in-flight job uploads its outputs BEFORE
	// the commit that starts referencing them, so a young unreferenced object
	// under a live clip is usually a job that is still running.
	code, out := fx.run(t, "orphans")
	if code != 0 || !strings.Contains(out, "younger than --min-age") || !strings.Contains(out, "extra=0 extra_bytes=0") {
		t.Fatalf("fresh extras must be skipped: %d\n%s", code, out)
	}
	// A skipped extra is still PRINTED on a line that starts `extra   <id> `:
	// the E2E's step 70 (70-web.sh (f)) asserts that prefix ABSENT for a live
	// clip, and that absence is only age-proof because a young object lands
	// on the same prefix as a counted one.
	if !strings.Contains(out, "extra   "+fx.x+" state=ready key="+stale+" bytes=10 age=") {
		t.Fatalf("a young extra is not printed under the clip's `extra   <id> ` prefix:\n%s", out)
	}

	// The Fake stamps LastModified with the real wall clock, so the fixture's
	// frozen clock has to move to the real one to age anything out.
	fx.now = time.Now().Add(2 * time.Hour)
	code, out = fx.run(t, "orphans")
	if code != 0 {
		t.Fatalf("orphans: %d\n%s", code, out)
	}
	if !strings.Contains(out, "extra   "+fx.x+" state=ready key="+stale+" bytes=10") {
		t.Fatalf("the superseded rendition is not reported:\n%s", out)
	}
	if !strings.Contains(out, "extra   "+fx.x+" subtotal objects=2 bytes=13") {
		t.Fatalf("the per-clip subtotal is wrong:\n%s", out)
	}
	if !strings.Contains(out, "extra=2 extra_bytes=13 extra_deleted=0") {
		t.Fatalf("the summary counters:\n%s", out)
	}
	if strings.Contains(out, store.ProbeKey("", fx.x)) {
		t.Fatalf("meta/probe.json is the one legitimate unreferenced key and must not be reported:\n%s", out)
	}
	// Reporting removes nothing.
	if _, ok := fx.fake.Get(stale); !ok {
		t.Fatal("orphans without --delete removed an extra object")
	}

	code, out = fx.run(t, "orphans", "--delete")
	if code != 0 || !strings.Contains(out, "extra_deleted=2") {
		t.Fatalf("orphans --delete: %d\n%s", code, out)
	}
	for _, k := range []string{stale, stalePoster} {
		if _, ok := fx.fake.Get(k); ok {
			t.Fatalf("%s survived --delete", k)
		}
	}
	// The keys the row DOES reference are untouched, every time.
	for _, k := range []string{store.RenditionKey("", fx.x, "r8r8r8r8"), fx.xPoster, store.OriginalKey("", fx.x, "mp4"), store.ProbeKey("", fx.x)} {
		if _, ok := fx.fake.Get(k); !ok {
			t.Fatalf("%s was deleted although the row references it (or it is the probe JSON)", k)
		}
	}
}

// A CHECKPOINTED job's outputs are referenced by the checkpoint and by no
// column (§10.3 step 8 writes them BEFORE FinalizeTranscode writes the
// columns, and no failure path clears the checkpoint), so to the third report
// they look exactly like garbage — of any age, because the jobs this happens
// to are the ones that have been stuck for hours, which is the population
// DEPLOY §24.3 exists for. Deleting them and then running the documented
// recovery would flip the clip to `ready` on keys that no longer exist.
func TestAdminOrphansNeverDeletesTheOutputsOfALiveJob(t *testing.T) {
	fx := newAdminFixture(t)
	j := fx.claimY(t) // clip Y: transcode running, clip processing
	rend := store.RenditionKey("", fx.y, "cccccccc")
	poster := store.PosterKey("", fx.y, "cccccccc")
	fx.fake.Put(rend, []byte("rend"), "video/mp4")
	fx.fake.Put(poster, []byte("pj"), "image/jpeg")
	fx.saveCheckpoint(t, j, `{"rendition_key":"`+rend+`","poster_key":"`+poster+`"}`)

	// Hours later: the age at which an operator actually finds a stuck job, and
	// the reason --min-age cannot be the guard for this class.
	fx.now = time.Now().Add(2 * time.Hour)
	code, out := fx.run(t, "orphans", "--delete", "--min-age", "1s")
	if code != 0 {
		t.Fatalf("orphans --delete: %d\n%s", code, out)
	}
	for _, k := range []string{rend, poster} {
		if !strings.Contains(out, "extra   "+fx.y+" state=processing key="+k) {
			t.Fatalf("the object is not reported at all:\n%s", out)
		}
		if _, ok := fx.fake.Get(k); !ok {
			t.Fatalf("%s was deleted although a live job's checkpoint names it", k)
		}
	}
	if !strings.Contains(out, "the clip has a live job") {
		t.Fatalf("the skip does not say why:\n%s", out)
	}
	// Nothing was confirmed, so nothing is counted or subtotalled either.
	if !strings.Contains(out, "extra=0 extra_bytes=0 extra_deleted=0") || strings.Contains(out, "subtotal") {
		t.Fatalf("a skipped object was counted as a finding:\n%s", out)
	}
	// The guard is not permanent: once the job reaches a terminal state and the
	// columns are the only reference left, the sweep works as before. (Ending
	// the job the way the worker does also proves the keys are still there to
	// finalize onto — the whole point of the interlock.)
	fx.finalizeY(t, j.ID, rend, poster)
	code, out = fx.run(t, "orphans", "--delete", "--min-age", "1s")
	if code != 0 || !strings.Contains(out, "extra=0") || strings.Contains(out, "live job") {
		t.Fatalf("after the job finished: %d\n%s", code, out)
	}
	if strings.Contains(out, "missing "+fx.y) {
		t.Fatalf("the finalized clip references an object that is not in the bucket:\n%s", out)
	}
}

// The live-job interlock is about OBJECTS, so it excludes the one kind that
// never writes any (§20 erratum 150, amended). A queued notify job is the
// longest-lived non-terminal row this system produces — NOTIFY_MAX_ATTEMPTS
// backoffs against a Discord that is down — and while it counted, a clip's
// leftovers were shielded from the sweep by an announcement that cannot
// reference them.
func TestAdminOrphansSweepsAClipWhoseOnlyLiveJobIsANotify(t *testing.T) {
	fx := newAdminFixture(t)
	j := fx.claimY(t)
	rend := store.RenditionKey("", fx.y, "nnnnnnnn")
	poster := store.PosterKey("", fx.y, "nnnnnnnn")
	fx.fake.Put(rend, []byte("rend"), "video/mp4")
	fx.fake.Put(poster, []byte("pj"), "image/jpeg")
	// The transcode finishes AND queues the announcement in one commit, so Y
	// is ready with a done transcode and a queued notify.
	fx.finalizeYNotifying(t, j.ID, rend, poster, true)
	// A superseded rendition of the same clip: exactly what a trim that died
	// after its commit leaves behind.
	stale := store.RenditionKey("", fx.y, "oldoldol")
	fx.fake.Put(stale, []byte("superseded"), "video/mp4")

	fx.now = time.Now().Add(2 * time.Hour)
	code, out := fx.run(t, "orphans", "--delete", "--min-age", "1s")
	if code != 0 {
		t.Fatalf("orphans --delete: %d\n%s", code, out)
	}
	if strings.Contains(out, "the clip has a live job") {
		t.Fatalf("a queued notify job shielded the clip from the sweep:\n%s", out)
	}
	if !strings.Contains(out, "extra   "+fx.y+" state=ready key="+stale) {
		t.Fatalf("the superseded rendition is not reported:\n%s", out)
	}
	if _, ok := fx.fake.Get(stale); ok {
		t.Fatal("the superseded rendition survived --delete")
	}
	// The keys the row references are untouched, notify job or not.
	for _, k := range []string{rend, poster} {
		if _, ok := fx.fake.Get(k); !ok {
			t.Fatalf("%s was deleted although the row references it", k)
		}
	}
	// The exclusion is exactly one kind wide: give the same clip a live GIF
	// job and the interlock is back.
	fx.enqueueGIF(t, fx.y)
	other := store.RenditionKey("", fx.y, "otherrrr")
	fx.fake.Put(other, []byte("also superseded"), "video/mp4")
	code, out = fx.run(t, "orphans", "--delete", "--min-age", "1s")
	if code != 0 || !strings.Contains(out, "the clip has a live job") {
		t.Fatalf("a live gif job must still shield the clip: %d\n%s", code, out)
	}
	if _, ok := fx.fake.Get(other); !ok {
		t.Fatal("an object was deleted under a clip with a live gif job")
	}
}

func TestAdminOrphansNeverDeletesAnExtraOriginal(t *testing.T) {
	fx := newAdminFixture(t)
	// An original under a live clip that no column names — a leftover from an
	// upload whose extension changed, say. The upload is sacred (§11): report
	// it, never remove it.
	rogue := "clips/" + fx.x + "/original/source.mov"
	fx.fake.Put(rogue, []byte("sacred"), "video/quicktime")
	fx.now = time.Now().Add(2 * time.Hour)
	code, out := fx.run(t, "orphans", "--delete")
	if code != 0 || !strings.Contains(out, "extra   "+fx.x+" state=ready key="+rogue) {
		t.Fatalf("orphans --delete: %d\n%s", code, out)
	}
	if !strings.Contains(out, "refusing to delete an original; left alone") {
		t.Fatalf("the refusal is not reported:\n%s", out)
	}
	if _, ok := fx.fake.Get(rogue); !ok {
		t.Fatal("an object under original/ was deleted")
	}
	if !strings.Contains(out, "extra=1 extra_bytes=6 extra_deleted=0") {
		t.Fatalf("summary:\n%s", out)
	}
}

func TestAdminBackupIntoADirectoryRotates(t *testing.T) {
	fx := newAdminFixture(t)
	dir := t.TempDir()
	// A file this command did not mint: rotation must never look at it.
	decoy := filepath.Join(dir, "my-own-copy.db")
	if err := os.WriteFile(decoy, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	var names []string
	for i := 0; i < 3; i++ {
		fx.now = fx.now.Add(time.Duration(i+1) * time.Minute)
		code, out := fx.run(t, "backup", dir, "--rotate", "2")
		if code != 0 {
			t.Fatalf("backup %d: %d\n%s", i, code, out)
		}
		name := "clippet-" + fx.now.UTC().Format(snapshotStamp) + ".db"
		names = append(names, name)
		if !strings.Contains(out, filepath.Join(dir, name)) {
			t.Fatalf("backup %d did not mint the dated name:\n%s", i, out)
		}
	}
	// The newest two survive; the oldest was removed and said so.
	for _, want := range names[1:] {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Fatalf("%s should have been kept: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, names[0])); !os.IsNotExist(err) {
		t.Fatalf("%s should have been rotated away: %v", names[0], err)
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Fatalf("rotation removed a file it did not mint: %v", err)
	}
	fx.now = fx.now.Add(time.Minute) // a fresh second, so the name is a fresh one
	_, out := fx.run(t, "backup", dir, "--rotate", "2")
	if !strings.Contains(out, "untouched=1") {
		t.Fatalf("the summary does not report the files left alone:\n%s", out)
	}
	// 0600, like every other snapshot: it holds every password hash.
	fi, err := os.Stat(filepath.Join(dir, names[2]))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %#o, want 0600", fi.Mode().Perm())
	}
}

func TestAdminBackupIntoADirectoryStalePartFiles(t *testing.T) {
	fx := newAdminFixture(t)
	dir := t.TempDir()
	stale := filepath.Join(dir, "clippet-20260101T000000Z.db.part-abc123")
	fresh := filepath.Join(dir, "clippet-20260102T000000Z.db.part-def456")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := fx.now.Add(-3 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	code, out := fx.run(t, "backup", dir, "--rotate", "5")
	if code != 0 {
		t.Fatalf("backup: %d\n%s", code, out)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a stranded partial older than an hour should be removed: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a partial younger than an hour may still be being written: %v", err)
	}
	if !strings.Contains(out, "stale_parts=1") {
		t.Fatalf("summary:\n%s", out)
	}
}

func TestAdminBackupDirectoryGuards(t *testing.T) {
	fx := newAdminFixture(t)
	dir := t.TempDir()
	// --rotate needs a directory: a single named file is never rotated.
	if code, out := fx.run(t, "backup", filepath.Join(dir, "one.db"), "--rotate", "3"); code != 2 ||
		!strings.Contains(out, "needs a DIRECTORY argument") {
		t.Fatalf("--rotate on a file: %d\n%s", code, out)
	}
	if code, _ := fx.run(t, "backup", dir, "--rotate", "-1"); code != 2 {
		t.Fatal("a negative --rotate must be a usage error")
	}
	// Without --rotate a directory still works and deletes nothing.
	code, out := fx.run(t, "backup", dir)
	if code != 0 || !strings.Contains(out, "rotate:  off") {
		t.Fatalf("backup into a directory without --rotate: %d\n%s", code, out)
	}
	// Two snapshots in the same second: the never-overwrite rule holds even
	// though the name is derived.
	code, out = fx.run(t, "backup", dir)
	if code != 1 || !strings.Contains(out, "already exists") {
		t.Fatalf("a second snapshot in the same second: %d\n%s", code, out)
	}
	// …and it left no `.part-` file behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".part-") {
			t.Fatalf("a refused snapshot left %s behind", e.Name())
		}
	}
}

// The CLI's copy against the same golden table the two web-facing ones read
// (nit sweep S11). It uses the `cli` column, whose two differences from the
// `page` column are deliberate and documented in the file itself — so a change
// that made this renderer agree with the others, or diverge further, has to be
// spelled out there rather than in three separate test bodies.
func TestHumanBytes(t *testing.T) {
	for _, tc := range humanBytesGolden(t) {
		if got := humanBytes(tc.N); got != tc.CLI {
			t.Errorf("humanBytes(%d) = %q, want %q (testdata/humanbytes.tsv)", tc.N, got, tc.CLI)
		}
	}
}

// humanBytesGolden reads the ONE table all three humanBytes copies are pinned
// against (../../testdata/humanbytes.tsv, nit sweep S11) and returns
// (bytes, page spelling, cli spelling) triples. The file's own header explains
// the two deliberate page/CLI differences; a THIRD difference is a bug in
// whichever copy moved.
func humanBytesGolden(t *testing.T) []struct {
	N         int64
	Page, CLI string
} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "humanbytes.tsv"))
	if err != nil {
		t.Fatalf("read the humanBytes golden: %v", err)
	}
	var out []struct {
		N         int64
		Page, CLI string
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			t.Fatalf("humanbytes.tsv:%d: %d tab-separated fields, want 3 (%q)", i+1, len(f), line)
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			t.Fatalf("humanbytes.tsv:%d: %v", i+1, err)
		}
		out = append(out, struct {
			N         int64
			Page, CLI string
		}{n, f[1], f[2]})
	}
	if len(out) < 8 {
		t.Fatalf("the humanBytes golden holds %d rows; it is meant to be the whole table", len(out))
	}
	return out
}

// TestAdminUsageNamesTheNewVerbs keeps the help text and the dispatch table in
// step: a verb that is dispatched but undocumented is a verb nobody runs.
func TestAdminUsageNamesTheNewVerbs(t *testing.T) {
	var b strings.Builder
	adminUsage(&b)
	for _, verb := range []string{"doctor", "jobs unstick", "retry <id>", "--rotate N", "--state s", "--all-failed"} {
		if !strings.Contains(b.String(), verb) {
			t.Errorf("admin usage does not mention %q", verb)
		}
	}
}

// TestAdminNewVerbsRefuseWithoutADatabase pins that the recovery verbs join
// the existing ones in never creating the file they read.
func TestAdminNewVerbsRefuseWithoutADatabase(t *testing.T) {
	fx := newAdminFixture(t)
	cfg := fx.cfg
	cfg.DataDir = t.TempDir() // a data dir with no clippet.db in it
	env := adminEnv{stdout: &fx.out, stderr: &fx.errOut,
		openStore: func(config.Config) (store.Store, error) { return fx.fake, nil },
		now:       func() time.Time { return fx.now }}
	for _, args := range [][]string{{"jobs"}, {"jobs", "unstick", "--older-than", "7h"}, {"retry", ids.New()}} {
		if code := runAdminWith(args, cfg, env); code != 2 {
			t.Errorf("%v without a database: %d, want 2", args, code)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "clippet.db")); !os.IsNotExist(err) {
		t.Fatal("an admin verb created the database")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// `--clip <id>` (§20 erratum 290, AUDIT #4) bounds every report AND the delete
// to one clip's prefix. The E2E's step 54 runs `orphans --delete` on the
// shared stack every gate; until this flag that call was bucket-wide, so a
// stranger's leftover anywhere under clips/ was the suite's to remove. The
// pin plants three things a bucket-wide sweep would take — an extra under clip
// X, an extra under a second live clip Y, and the fixture's row-less prefix Z
// — and asks for X alone: X's extra goes, Y's and Z's stay, the summary counts
// only X's, and nothing outside X's prefix is even listed.
func TestAdminOrphansClipScopesEveryReportAndTheDelete(t *testing.T) {
	fx := newAdminFixture(t)
	j := fx.claimY(t)
	yRend := store.RenditionKey("", fx.y, "yyyyyyyy")
	yPoster := store.PosterKey("", fx.y, "yyyyyyyy")
	fx.fake.Put(yRend, []byte("rend"), "video/mp4")
	fx.fake.Put(yPoster, []byte("pj"), "image/jpeg")
	fx.finalizeY(t, j.ID, yRend, yPoster) // Y: ready, its transcode done, no live job
	xStale := store.RenditionKey("", fx.x, "oldoldol")
	yStale := store.RenditionKey("", fx.y, "oldoldol")
	fx.fake.Put(xStale, []byte("superseded"), "video/mp4")
	fx.fake.Put(yStale, []byte("also superseded"), "video/mp4")
	zRend := store.RenditionKey("", fx.z, "zzzzzzzz") // the fixture's row-less prefix
	fx.now = time.Now().Add(2 * time.Hour)

	code, out := fx.run(t, "orphans", "--clip", fx.x, "--delete", "--min-age", "1s")
	if code != 0 {
		t.Fatalf("orphans --clip: %d\n%s", code, out)
	}
	if !strings.Contains(out, "scope   clips/"+fx.x+"/ (--clip") {
		t.Fatalf("the report does not name the prefix it was scoped to:\n%s", out)
	}
	if !strings.Contains(out, "extra   "+fx.x+" state=ready key="+xStale) || !strings.Contains(out, "extra=1 extra_bytes=10 extra_deleted=1") {
		t.Fatalf("X's extra is not the one (and only) finding:\n%s", out)
	}
	if _, ok := fx.fake.Get(xStale); ok {
		t.Fatal("X's superseded rendition survived --clip X --delete")
	}
	// Nothing outside clips/X/ is listed, let alone removed: Y's extra, Z's
	// row-less prefix and the foreign key all sit outside the listing.
	for _, s := range []string{fx.y, fx.z, "not-a-valid-id", "orphan ", "foreign "} {
		if strings.Contains(out, s) {
			t.Fatalf("a --clip %s report mentions %q, which is outside that prefix:\n%s", fx.x, s, out)
		}
	}
	if !strings.Contains(out, "orphans: prefixes=0 deleted=0 recent=0 missing=0 foreign=0") {
		t.Fatalf("the summary counts something outside the scope:\n%s", out)
	}
	for _, k := range []string{yStale, zRend, store.OriginalKey("", fx.z, "mp4"), "clips/not-a-valid-id/x.bin"} {
		if _, ok := fx.fake.Get(k); !ok {
			t.Fatalf("%s was deleted by a delete scoped to clip %s", k, fx.x)
		}
	}
	// The scope is the whole story: asked for Z, the row-less prefix IS the
	// finding and X and Y are not mentioned; asked for Y, its extra goes.
	code, out = fx.run(t, "orphans", "--clip", fx.z, "--delete", "--min-age", "1s")
	if code != 0 || !strings.Contains(out, "orphans: prefixes=1 deleted=1 ") || strings.Contains(out, fx.x) || strings.Contains(out, fx.y) {
		t.Fatalf("orphans --clip Z: %d\n%s", code, out)
	}
	if _, ok := fx.fake.Get(zRend); ok {
		t.Fatal("Z's row-less prefix survived --clip Z --delete")
	}
	if _, ok := fx.fake.Get(yStale); !ok {
		t.Fatal("Y's extra was deleted by a delete scoped to clip Z")
	}
	// A --clip that is not a clip id is a usage error, and so is a positional.
	if code, _ := fx.run(t, "orphans", "--clip", "../../etc"); code != 2 {
		t.Fatalf("--clip with a path: %d", code)
	}
	if code, _ := fx.run(t, "orphans", "--clip", fx.x, "extra"); code != 2 {
		t.Fatalf("--clip with a positional: %d", code)
	}
}
