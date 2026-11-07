package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"clippet/internal/db"
	"clippet/internal/transcode"
)

// §20 erratum 293: every ENDING a run writes — FailAttempt, ReleaseJob,
// RequeueWithBackoff and CompleteJob — is pinned to the attempt that holds the
// row, the way erratum 291 pinned the three writes a run makes while it is
// still going (the stage, the checkpoint, the cancel). Erratum 291 left these
// four on `state = 'running'` alone and said why: on the TRANSCODE path every
// one of them now sits behind the pinned checkpoint, so a repaired row is met
// there first. The media kinds have no checkpoint at all, so nothing preceded
// their endings, and a run whose row an operator had unstuck could still spend
// the NEW attempt's counter through fail(), take its job off the queue through
// release() or noRoom(), or mark it `done` through complete().

// attemptEndings are the four a run can take with the row it believes it
// holds. Each kind builds them over its own run type; the test then calls each
// one through a handle whose row a repair has taken away.
type attemptEndings struct {
	complete func()
	release  func()
	fail     func()
	noRoom   func()
}

// jobFacts renders a job row by value, so "unchanged" is a string comparison
// rather than a list of fields somebody will forget to extend.
func jobFacts(j db.Job) string {
	s := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	i := func(p *int64) int64 {
		if p == nil {
			return -1
		}
		return *p
	}
	return fmt.Sprintf("state=%s stage=%s attempts=%d transient=%d run_after=%d updated=%d checkpoint=%s last_error=%s started=%d finished=%d",
		j.State, j.Stage, j.Attempts, j.TransientFailures, j.RunAfter, j.UpdatedAt,
		s(j.CheckpointJSON), s(j.LastError), i(j.StartedAt), i(j.FinishedAt))
}

// jobOfKind reads the clip's one job of this kind back from the database: a
// run's handle is a COPY of the row it claimed, so nothing but a re-read can
// say whether a refused ending wrote anything.
func jobOfKind(t *testing.T, h *harness, clipID, kind string) db.Job {
	t.Helper()
	j, err := h.db.LatestJobOfKind(context.Background(), clipID, kind)
	if err != nil {
		t.Fatalf("LatestJobOfKind(%s, %s): %v", clipID, kind, err)
	}
	return j
}

// TestEveryKindsEndingRefusesAStaleHandle is the pin, kind by kind. Each case
// enqueues its job on a ready clip, claims it, has an operator `unstick` it
// (the second hand erratum 291 names: the CLI, a second process, the boot
// reconcile), re-claims it as a NEW attempt, and then drives all four endings
// through the OLD handle. Every one must refuse and leave the row byte for
// byte as the repair and the re-claim left it; the new attempt's own ending
// must still land.
func TestEveryKindsEndingRefusesAStaleHandle(t *testing.T) {
	cases := []struct {
		kind    string
		enqueue func(h *harness, c db.Clip)
		build   func(h *harness, job db.Job, c db.Clip) attemptEndings
	}{
		{
			kind:    db.KindTrim,
			enqueue: func(h *harness, c db.Clip) { h.enqueueTrim(c.ID, 0, 2000) },
			build: func(h *harness, job db.Job, c db.Clip) attemptEndings {
				r := newTrimRun(h.w, context.Background(), &run{}, job, c)
				return attemptEndings{
					complete: func() { r.complete(db.JobStageSkipped, "skipped: the pin test") },
					release:  func() { r.release() },
					fail:     func() { r.fail(db.FailCrash, errors.New("boom")) },
					noRoom:   func() { r.noRoom(errNoScratchRoom) },
				}
			},
		},
		{
			kind:    db.KindGIF,
			enqueue: func(h *harness, c db.Clip) { h.enqueueGIF(c.ID) },
			build: func(h *harness, job db.Job, c db.Clip) attemptEndings {
				r := newGIFRun(h.w, context.Background(), &run{}, job, c)
				return attemptEndings{
					complete: func() { r.complete(db.JobStageSkipped, "skipped: the pin test") },
					release:  func() { r.release() },
					fail:     func() { r.fail(db.FailCrash, errors.New("boom")) },
					noRoom:   func() { r.noRoom(errNoScratchRoom) },
				}
			},
		},
		{
			kind: db.KindSocial,
			enqueue: func(h *harness, c db.Clip) {
				if _, err := h.db.EnqueueSocial(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
					h.t.Fatalf("EnqueueSocial: %v", err)
				}
			},
			build: func(h *harness, job db.Job, c db.Clip) attemptEndings {
				r := newSocialRun(h.w, context.Background(), &run{}, job, c)
				return attemptEndings{
					complete: func() { r.complete(db.JobStageSkipped, "skipped: the pin test") },
					release:  func() { r.release() },
					fail:     func() { r.fail(db.FailCrash, errors.New("boom")) },
					noRoom:   func() { r.noRoom(errNoScratchRoom) },
				}
			},
		},
		{
			kind:    db.KindPoster,
			enqueue: func(h *harness, c db.Clip) { h.enqueuePoster(c.ID, 1) },
			build: func(h *harness, job db.Job, c db.Clip) attemptEndings {
				r := newPosterRun(h.w, context.Background(), &run{}, job, c)
				return attemptEndings{
					complete: func() { r.complete(db.JobStageSkipped, "skipped: the pin test") },
					release:  func() { r.release() },
					fail:     func() { r.fail(db.FailCrash, errors.New("boom")) },
					noRoom:   func() { r.noRoom(errNoScratchRoom) },
				}
			},
		},
		{
			kind:    db.KindFilmstrip,
			enqueue: func(h *harness, c db.Clip) { h.enqueueFilmstrip(c.ID) },
			build: func(h *harness, job db.Job, c db.Clip) attemptEndings {
				r := newFilmstripRun(h.w, context.Background(), &run{}, job, c)
				return attemptEndings{
					complete: func() { r.complete(db.JobStageSkipped, "skipped: the pin test") },
					release:  func() { r.release() },
					fail:     func() { r.fail(db.FailCrash, errors.New("boom")) },
					noRoom:   func() { r.noRoom(errNoScratchRoom) },
				}
			},
		},
		{
			kind: db.KindFull,
			enqueue: func(h *harness, c db.Clip) {
				if _, err := h.db.EnqueueFull(context.Background(), c.ID, h.clock.Now().Unix()); err != nil {
					h.t.Fatalf("EnqueueFull: %v", err)
				}
			},
			build: func(h *harness, job db.Job, c db.Clip) attemptEndings {
				r := newFullRun(h.w, context.Background(), &run{}, job, c)
				return attemptEndings{
					complete: func() { r.complete(db.JobStageSkipped, "skipped: the pin test") },
					release:  func() { r.release() },
					fail:     func() { r.fail(db.FailCrash, errors.New("boom")) },
					noRoom:   func() { r.noRoom(errNoScratchRoom) },
				}
			},
		},
		{
			kind:    db.KindReplace,
			enqueue: func(h *harness, c db.Clip) { h.enqueueReplace(c.ID, 2048) },
			build: func(h *harness, job db.Job, c db.Clip) attemptEndings {
				r := newReplaceRun(h.w, context.Background(), &run{}, job, c)
				return attemptEndings{
					complete: func() { r.complete(db.JobStageSkipped, "skipped: the pin test") },
					release:  func() { r.release() },
					fail:     func() { r.fail(db.FailCrash, errors.New("boom")) },
					noRoom:   func() { r.noRoom(errNoScratchRoom) },
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			h := newHarness(t)
			// A ready clip with no worker running, so this test owns the order
			// in which the job is claimed, repaired and claimed again.
			c := h.gifReady()
			tc.enqueue(h, c)

			old := h.claimKind(tc.kind)
			if old.StartedAt == nil {
				t.Fatalf("a claimed %s job has no stamp: %+v", tc.kind, old)
			}
			// The operator's unstick: the row is old enough by its own stamp,
			// so the repair takes it, spends the one crash attempt a
			// checkpointless job owes, and requeues it with a backoff.
			if _, err := h.db.UnstickJob(context.Background(), old.ID, *old.StartedAt, h.w.lim, h.clock.Now().Unix()); err != nil {
				t.Fatalf("UnstickJob: %v", err)
			}
			// Past the crash backoff, so the queue can hand it to a NEW attempt.
			h.clock.Advance(2 * time.Minute)
			cur := h.claimKind(tc.kind)
			if cur.ID != old.ID {
				t.Fatalf("re-claim took job %d, want the repaired %d", cur.ID, old.ID)
			}
			if cur.StartedAt == nil || *cur.StartedAt == *old.StartedAt {
				t.Fatalf("the new attempt's stamp is %v, want one of its own (old %d)", cur.StartedAt, *old.StartedAt)
			}

			before := jobOfKind(t, h, c.ID, tc.kind)
			if before.State != db.JobRunning || before.Attempts != 1 {
				t.Fatalf("the re-claimed row = %s, want running with the repair's one crash attempt", jobFacts(before))
			}
			clipBefore := h.clip(c.ID)

			// The OLD handle. Every ending refuses; nothing moves.
			endings := tc.build(h, old, c)
			for _, e := range []struct {
				name string
				call func()
			}{
				{"complete", endings.complete},
				{"release", endings.release},
				{"fail", endings.fail},
				{"noRoom", endings.noRoom},
			} {
				e.call()
				if got := jobOfKind(t, h, c.ID, tc.kind); jobFacts(got) != jobFacts(before) {
					t.Fatalf("%s through the stale handle changed the row:\n before %s\n after  %s",
						e.name, jobFacts(before), jobFacts(got))
				}
			}
			if got := h.clip(c.ID); got.State != clipBefore.State || got.UpdatedAt != clipBefore.UpdatedAt {
				t.Fatalf("a refused ending moved the clip: %s/%d, want %s/%d",
					got.State, got.UpdatedAt, clipBefore.State, clipBefore.UpdatedAt)
			}
			// Each refusal is the lost-attempt line, at INFO, and none of them
			// is an error: a repair somebody chose to run is an expected ending.
			h.assertLog("INFO", "job row is no longer this attempt's")
			h.assertNoErrorLogs()

			// The CURRENT handle still ends its own row.
			tc.build(h, cur, c).complete()
			if got := jobOfKind(t, h, c.ID, tc.kind); got.State != db.JobDone {
				t.Fatalf("the current attempt's complete() left the row %s, want done", got.State)
			}
		})
	}
}

// TestATrimWhoseRowWasRepairedEndsAsALostAttempt is the same pin seen from a
// LIVE run — erratum 291's TestARunWhoseRowWasRepairedEndsAsALostAttempt for a
// kind that has no checkpoint to meet the repair at. The unstick happens from
// inside the cut; the run then fails on its own and must record nothing,
// because the row it would record it on is the new attempt's.
func TestATrimWhoseRowWasRepairedEndsAsALostAttempt(t *testing.T) {
	h := newHarness(t)
	c := h.readyTrimmable()
	h.runner.encodeFn = func(context.Context, transcode.Plan, string, string, func(float64, bool)) error {
		j, err := h.db.LatestJobOfKind(context.Background(), c.ID, db.KindTrim)
		if err != nil {
			return err
		}
		if _, err := h.db.UnstickJob(context.Background(), j.ID, *j.StartedAt, h.w.lim, h.clock.Now().Unix()); err != nil {
			return err
		}
		return &transcode.ErrPermanent{Err: errors.New("the cut failed after the repair")}
	}

	h.enqueueTrim(c.ID, 0, 2000)
	h.waitLog("trim job row is no longer this attempt's; outputs discarded, row untouched")
	h.waitIdle()

	job, err := h.db.LatestJobOfKind(context.Background(), c.ID, db.KindTrim)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != db.JobQueued || job.Attempts != 1 || job.TransientFailures != 0 ||
		job.LastError == nil || *job.LastError != db.UnstickReason {
		t.Fatalf("the repaired row after the lost attempt = %s, want queued with the repair's ONE crash attempt and its reason", jobFacts(job))
	}
	after := h.clip(c.ID)
	if after.State != db.ClipReady || after.FailureReason != nil {
		t.Fatalf("a lost trim attempt moved the clip: %s/%v", after.State, after.FailureReason)
	}
	if _, ok := h.store.Get(*after.RenditionKey); !ok {
		t.Error("the clip lost the rendition it was playing")
	}
	h.assertNoErrorLogs()
}
