package main

// admin_ops.go — the outage verbs (ROADMAP #10 and #12, DESIGN.md §20 errata
// 144 and 146):
//
//	clippet admin jobs [--kind k] [--state s] [--clip id] [--since D] [--limit n]
//	clippet admin jobs unstick <job-id> | --older-than D
//	clippet admin retry <id>… | --all-failed
//
// "MinIO was down for three hours overnight. Which clips ended `failed`, which
// trims and previews died with them, and can I put them all back on the
// queue?" — `admin stats` said `failed=1` and not WHICH, `admin ls` had no
// filter, and the only retry was POST /api/clips/{id}/retry: session-only,
// CSRF-guarded, one human click per clip.
//
// Two rules from the bundle brief hold throughout: every verb is read-only
// unless its name says otherwise, and every destructive or state-changing verb
// needs an explicit selector with no default that means "everything".

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"clippet/internal/db"
	"clippet/internal/ids"
	"clippet/internal/store"
)

// maxErrorPrint bounds the last_error text one job line carries. A failure
// reason is a bounded §11 class today, but the column is the one place an
// arbitrary library message can end up, and a job listing should stay one
// line per job.
const maxErrorPrint = 160

// --- jobs -------------------------------------------------------------------

func (a *admin) jobsUsage() {
	fmt.Fprintf(a.env.stderr, "usage: clippet admin jobs [--kind %s] [--state queued|running|done|failed|cancelled]\n", jobKindList)
	fmt.Fprintln(a.env.stderr, "                          [--clip <id>] [--since D] [--limit N]")
	fmt.Fprintln(a.env.stderr, "       clippet admin jobs unstick <job-id>")
	fmt.Fprintln(a.env.stderr, "       clippet admin jobs unstick --older-than D")
	fmt.Fprintln(a.env.stderr, "  `unstick` needs a selector: either one job id or --older-than. There is no default.")
}

func (a *admin) jobs(args []string) int {
	if len(args) > 0 && args[0] == "unstick" {
		return a.jobsUnstick(args[1:])
	}
	fs := a.flags("jobs")
	kind := fs.String("kind", "", "only this job kind: "+jobKindList)
	state := fs.String("state", "", "only this job state: queued|running|done|failed|cancelled")
	clip := fs.String("clip", "", "only jobs of this clip")
	since := fs.Duration("since", 0, "only jobs touched within this long (e.g. 24h)")
	limit := fs.Int("limit", db.DefaultJobListLimit, "at most this many rows, newest first")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		a.jobsUsage()
		return 2
	}
	if *kind != "" && !validJobKind(*kind) {
		fmt.Fprintf(a.env.stderr, "admin jobs: --kind %q is not a job kind (%s)\n", *kind, jobKindList)
		return 2
	}
	if *state != "" && !validJobState(*state) {
		fmt.Fprintf(a.env.stderr, "admin jobs: --state %q is not a job state (queued|running|done|failed|cancelled)\n", *state)
		return 2
	}
	if *clip != "" && !ids.Valid(*clip) {
		fmt.Fprintf(a.env.stderr, "admin jobs: --clip %q is not a clip id\n", *clip)
		return 2
	}
	if *limit < 1 {
		fmt.Fprintln(a.env.stderr, "admin jobs: --limit must be at least 1")
		return 2
	}
	now := a.env.now()
	q := db.JobQuery{Kind: *kind, State: *state, ClipID: *clip, Limit: *limit}
	if *since > 0 {
		q.Since = now.Add(-*since).Unix()
	}
	rows, err := a.db.ListJobs(a.ctx, q)
	if err != nil {
		return a.fail("jobs", err)
	}
	for _, j := range rows {
		a.printJob(j, now)
	}
	fmt.Fprintf(a.env.stdout, "%d job(s)%s\n", len(rows), jobFilterSuffix(*kind, *state, *clip, *since))
	// The per-kind/state shape of the WHOLE table, whatever the filter showed:
	// it is what turns "12 rows scrolled past" into "there are 4 failed
	// transcodes and nothing else is wrong".
	counts, err := a.db.JobKindStateCounts(a.ctx)
	if err != nil {
		return a.fail("jobs", err)
	}
	for _, k := range jobKinds {
		byState := counts[k]
		fmt.Fprintf(a.env.stdout, "totals:  %-10s queued=%d running=%d done=%d failed=%d cancelled=%d\n", k,
			byState[db.JobQueued], byState[db.JobRunning], byState[db.JobDone], byState[db.JobFailed], byState[db.JobCancelled])
		delete(counts, k)
	}
	// Anything the worker does not know about (a kind from a newer binary that
	// ran against this file) is still counted rather than silently dropped.
	for k, byState := range counts {
		total := 0
		for _, n := range byState {
			total += n
		}
		fmt.Fprintf(a.env.stdout, "totals:  %-10s %d row(s) of an UNKNOWN kind\n", k, total)
	}
	return 0
}

// printJob is the one job line format `admin jobs` and `admin jobs unstick`
// both use. `age` is measured from started_at for a running job and from
// run_after for a queued one — the two numbers an operator is actually
// deciding on.
func (a *admin) printJob(j db.Job, now time.Time) {
	age := "-"
	switch {
	case j.State == db.JobRunning && j.StartedAt != nil:
		age = "running_for=" + dur(now.Unix()-*j.StartedAt)
	case j.State == db.JobQueued:
		age = "queued_for=" + dur(now.Unix()-j.RunAfter)
	}
	fmt.Fprintf(a.env.stdout,
		"#%-6d clip=%s kind=%-9s state=%-9s stage=%-10s attempts=%d transient=%d %s run_after=%s started=%s finished=%s checkpoint=%v error=%s\n",
		j.ID, j.ClipID, j.Kind, j.State, j.Stage, j.Attempts, j.TransientFailures, age,
		rfc(j.RunAfter), rfcp(j.StartedAt), rfcp(j.FinishedAt), j.CheckpointJSON != nil, quoteErr(j.LastError))
}

// dur renders a whole number of seconds as a duration; a negative value (the
// container clock steps backwards — DEPLOY §8) reads as 0s rather than as a
// negative age nobody expects.
func dur(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	return (time.Duration(seconds) * time.Second).String()
}

// quoteErr renders last_error as a quoted, bounded, single-line token. It is
// %q on purpose: the column is the one place a library message ends up, and a
// value carrying a newline must not be able to forge a second output line in
// something an operator greps.
func quoteErr(p *string) string {
	if p == nil {
		return `""`
	}
	s := *p
	if len(s) > maxErrorPrint {
		s = s[:maxErrorPrint] + "…"
	}
	return fmt.Sprintf("%q", s)
}

func jobFilterSuffix(kind, state, clip string, since time.Duration) string {
	var parts []string
	for _, p := range []struct{ k, v string }{{"kind", kind}, {"state", state}, {"clip", clip}} {
		if p.v != "" {
			parts = append(parts, p.k+"="+p.v)
		}
	}
	if since > 0 {
		parts = append(parts, "since="+since.String())
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " ") + ")"
}

// --- jobs unstick -----------------------------------------------------------

// jobsUnstick repairs a job left `running` by a worker that is no longer
// there. `jobRun.fail()`'s `case err != nil:` branch logs ERROR, cleans up its
// keys and RETURNS — leaving the job `running` and, for a transcode, the clip
// `processing`; the only thing that ever repaired such a row was a process
// restart. §11 promises "always claimable or retryable, never stuck", and this
// is that promise without a restart.
//
// THE AGE FLOOR IS A CONSERVATIVE HEURISTIC, NOT A PROOF, and saying so is
// part of the design. The boot repair can be sure nobody is running a job
// because nothing is running at all; this CLI is a second process and cannot
// see the worker's in-memory registry, so the only evidence it has is age, and
// TRANSCODE_HARD_TIMEOUT is the largest bound it can name — but that timeout
// covers ONE ENCODE (transcode.EncodeOptions.HardTimeout), not a job's
// lifetime. A run is acquire + probe + encode + verify + poster + store +
// checkpoint, acquiring a large original is bounded by STORAGE_TRANSFER_MAX
// instead (6 h by default), and a trim, gif or notify job is bounded by
// nothing related to it at all. So a live job CAN be older than the floor:
// the floor is the age past which a running row is very probably abandoned,
// and `admin jobs --state running` (the `running_for=` column) is the number
// to read first. What the floor rules out is the common shape of the mistake —
// repairing a job an ffmpeg is still writing, which lets a second worker claim
// it and reach CancelJob on the row underneath the older run. A shorter
// --older-than is therefore RAISED to the floor and the raise is printed,
// never silently obeyed, and a threshold that cannot even cover an acquire
// says so on its own line.
func (a *admin) jobsUnstick(args []string) int {
	fs := a.flags("jobs unstick")
	olderThan := fs.Duration("older-than", 0, "repair EVERY running job started longer ago than this (mutually exclusive with <job-id>)")
	positional, err := parsePositionals(fs, args, 1)
	if err != nil {
		a.jobsUsage()
		return 2
	}
	jobArg := ""
	if len(positional) > 0 {
		jobArg = strings.TrimSpace(positional[0])
	}
	if (jobArg == "") == (*olderThan <= 0) {
		a.jobsUsage()
		return 2
	}
	// The selector is validated before anything is printed, so a usage error
	// stays a usage error and nothing else.
	//
	// ParseInt, not Sscanf: Sscanf("41x", "%d") succeeds with 41, so a `#41`,
	// `41,` or `41x` pasted out of a listing would repair a job the operator
	// did not name — and this is the one verb here that writes. The whole
	// argument must be a positive decimal, the way every sibling filter is an
	// exact member of a closed set.
	//
	// The digit sweep comes BEFORE ParseInt because ParseInt is not that
	// closed set on its own: it accepts a leading sign, so `+41` parses as 41.
	// The consequence was nil — `+41` repairs exactly the job the operator
	// named — but a selector whose spelling is "the number `admin jobs` prints
	// after '#'" should be that number and nothing else.
	var jobID int64
	if jobArg != "" {
		digits := true
		for _, r := range jobArg {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		id, err := strconv.ParseInt(jobArg, 10, 64)
		if !digits || err != nil || id <= 0 {
			fmt.Fprintf(a.env.stderr, "admin jobs unstick: %q is not a job id (the number `admin jobs` prints after '#')\n", jobArg)
			return 2
		}
		jobID = id
	}
	floor := a.cfg.Transcode.HardTimeout
	threshold := *olderThan
	if threshold < floor {
		if threshold > 0 {
			fmt.Fprintf(a.env.stdout, "note:    --older-than %s raised to the TRANSCODE_HARD_TIMEOUT floor of %s (a younger job may still have a live worker on it)\n", threshold, floor)
		}
		threshold = floor
	}
	// The floor bounds an ENCODE. A transcode still acquiring its original is
	// bounded by STORAGE_TRANSFER_MAX instead, so when the threshold is below
	// that the operator is told which running rows the age test cannot speak
	// for — before any of them is repaired, and on its own greppable line.
	if threshold < a.cfg.S3.TransferMax {
		fmt.Fprintf(a.env.stdout, "note:    %s does not bound a job's whole run: downloading a large original is bounded by STORAGE_TRANSFER_MAX (%s), so read `admin jobs --state running` (running_for=) before repairing\n",
			threshold, a.cfg.S3.TransferMax)
	}
	notAfter := a.env.now().Add(-threshold).Unix()
	now := a.env.now().Unix()
	lim := db.Limits{MaxAttempts: a.cfg.Transcode.MaxAttempts, MaxTransient: a.cfg.Transcode.MaxTransient}

	if jobArg != "" {
		res, err := a.db.UnstickJob(a.ctx, jobID, notAfter, lim, now)
		switch {
		case errors.Is(err, db.ErrNotFound):
			fmt.Fprintf(a.env.stdout, "#%d: no such job\n", jobID)
			return 1
		case errors.Is(err, db.ErrStateMismatch):
			fmt.Fprintf(a.env.stdout, "#%d: not running; nothing to repair\n", jobID)
			return 1
		case errors.Is(err, db.ErrNotStale):
			fmt.Fprintf(a.env.stdout, "#%d: running for less than %s, so a worker may still be on it; nothing was changed\n", jobID, threshold)
			return 1
		case err != nil:
			return a.fail("jobs unstick", err)
		}
		a.printUnstuck(res)
		fmt.Fprintf(a.env.stdout, "unstuck: 1 job(s) running since before %s\n", rfc(notAfter))
		return 0
	}

	results, err := a.db.UnstickStaleJobs(a.ctx, notAfter, lim, now)
	if err != nil {
		return a.fail("jobs unstick", err)
	}
	for _, res := range results {
		a.printUnstuck(res)
	}
	fmt.Fprintf(a.env.stdout, "unstuck: %d job(s) running since before %s\n", len(results), rfc(notAfter))
	return 0
}

// printUnstuck reports what one repair did, including the clip's state after
// it — the §5 half an operator needs, because only a transcode job moves a
// clip and every other kind must leave it exactly as it was.
func (a *admin) printUnstuck(res db.UnstickResult) {
	fmt.Fprintf(a.env.stdout, "unstuck: #%d clip=%s kind=%s started=%s -> job %s, clip %s\n",
		res.JobID, res.ClipID, res.Kind, rfc(res.StartedAt), res.Action, res.ClipStateAfter)
}

// --- retry ------------------------------------------------------------------

func (a *admin) retryUsage() {
	fmt.Fprintln(a.env.stderr, "usage: clippet admin retry <id> [<id>…]")
	fmt.Fprintln(a.env.stderr, "       clippet admin retry --all-failed")
	fmt.Fprintln(a.env.stderr, "  only a FAILED clip is re-queued, and only while its original is still in the bucket.")
	fmt.Fprintln(a.env.stderr, "  a selector is required: ids, or --all-failed. There is no default.")
}

// retry is the CLI half of POST /api/clips/{id}/retry: it re-queues a FAILED
// clip's transcode. Two guards, both from §5/§11:
//
//   - db.RetryClip's UPDATE is predicated on `state = 'failed'`, so a ready,
//     queued or processing clip is never re-queued — the invariant a second
//     process must not be able to break;
//   - the ORIGINAL must still be in the bucket. A retry re-derives everything
//     from the upload, so a clip whose original is gone would be re-queued only
//     to fail again, and the honest answer is to say so now.
//
// The retry is picked up by the worker's next 5 s poll: the CLI is a separate
// process and does not (and must not) reach into the running worker.
func (a *admin) retry(args []string) int {
	fs := a.flags("retry")
	allFailed := fs.Bool("all-failed", false, "re-queue EVERY failed clip (mutually exclusive with <id>…)")
	var idsArgs []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			a.retryUsage()
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		idsArgs, rest = append(idsArgs, fs.Arg(0)), fs.Args()[1:]
	}
	if (len(idsArgs) == 0) == !*allFailed {
		a.retryUsage()
		return 2
	}
	targets := idsArgs
	if *allFailed {
		rows, err := a.db.ListAllClipIDsAndKeys(a.ctx)
		if err != nil {
			return a.fail("retry", err)
		}
		for _, r := range rows {
			if r.State == db.ClipFailed {
				targets = append(targets, r.ID)
			}
		}
		if len(targets) == 0 {
			fmt.Fprintln(a.env.stdout, "retry:   no failed clips")
			return 0
		}
	}
	st, err := a.store()
	if err != nil {
		return a.fail("retry", err)
	}
	requeued, skipped := 0, 0
	rc := 0
	for _, id := range targets {
		switch a.retryOne(st, id) {
		case 0:
			requeued++
		default:
			skipped++
			rc = 1
		}
	}
	// A partial batch still exits 1: an operator running --all-failed in a
	// script wants to know that something did not go back on the queue.
	fmt.Fprintf(a.env.stdout, "retry:   %d re-queued, %d skipped (the worker claims them on its next poll)\n", requeued, skipped)
	return rc
}

// retryOne re-queues one clip and prints one line about it. 0 = re-queued.
func (a *admin) retryOne(st store.Store, id string) int {
	if !ids.Valid(id) {
		fmt.Fprintf(a.env.stdout, "%s: not found\n", id)
		return 1
	}
	c, err := a.db.GetClip(a.ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		fmt.Fprintf(a.env.stdout, "%s: not found\n", id)
		return 1
	case err != nil:
		fmt.Fprintf(a.env.stdout, "%s: %v\n", id, err)
		return 1
	}
	if c.State != db.ClipFailed {
		fmt.Fprintf(a.env.stdout, "%s: not failed (state=%s); only a failed clip is re-queued\n", id, c.State)
		return 1
	}
	if _, err := st.Stat(a.ctx, c.SourceKey); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(a.env.stdout, "%s: the original is missing from the bucket; a retry would fail again (delete the clip instead)\n", id)
			return 1
		}
		fmt.Fprintf(a.env.stdout, "%s: cannot check the original: %v\n", id, err)
		return 1
	}
	err = a.db.RetryClip(a.ctx, id, a.env.now().Unix())
	switch {
	case errors.Is(err, db.ErrNotFound):
		fmt.Fprintf(a.env.stdout, "%s: not found\n", id)
		return 1
	case errors.Is(err, db.ErrStateMismatch):
		// It stopped being failed between the read and the write: the clip is
		// already on its way, which is the outcome the caller wanted.
		fmt.Fprintf(a.env.stdout, "%s: no longer failed; left alone\n", id)
		return 1
	case err != nil:
		fmt.Fprintf(a.env.stdout, "%s: %v\n", id, err)
		return 1
	}
	fmt.Fprintf(a.env.stdout, "%s: re-queued (transcode reset: attempts=0, checkpoint cleared)\n", id)
	return 0
}
