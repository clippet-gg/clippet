package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"clippet/internal/db"
)

// §20 erratum 165. db.ReconcileOnStartup words a job the process did not
// survive as db.MsgInterruptedByRestart, for EVERY kind, and the media status
// routes hand jobs.last_error straight to their reader: the owner for the
// poster, the trim and the replacement, and — through previewGIF's 503 — an
// anonymous caller for the preview. Erratum 80's doctrine is that those
// surfaces carry a fixed owner vocabulary, so an operator's sentence arriving
// by the one path that does not run through internal/worker was a hole in it.
// The routes now map anything outside the kind's own vocabulary onto that
// kind's generic sentence, and the raw text stays in the operator log.
func TestARestartNeverSpeaksToTheOwnerInAnOperatorsWords(t *testing.T) {
	ctx := context.Background()
	fail := func(t *testing.T, e *env, job db.Job, clipID, text string) {
		t.Helper()
		if _, err := e.db.FailAttempt(ctx, job.ID, db.ClaimStamp(job), clipID, text, db.FailPermanent, db.Limits{}, e.now()); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("poster", func(t *testing.T) {
		e := newEnv(t)
		owner, cookie := e.newUser("owner")
		c, _, _ := e.readyClip(owner, clipOpts{})
		if r := e.doJSON("POST", "/api/clips/"+c.ID+"/poster", posterBody(1), withCookie(cookie)); r.code != 202 {
			t.Fatalf("POST: %d %s", r.code, r.body)
		}
		job := e.claimPoster(c.ID)
		fail(t, e, job, c.ID, db.MsgInterruptedByRestart)
		r := e.do("GET", "/api/clips/"+c.ID+"/poster", nil, withCookie(cookie))
		if r.code != 200 || r.str("state") != "failed" {
			t.Fatalf("failed status = %d %s", r.code, r.body)
		}
		if got := r.str("error"); got != "the card image could not be changed" {
			t.Fatalf("error = %q, want the poster's own generic sentence", got)
		}
		// The row itself is unchanged: the operator still reads the truth.
		row, _ := e.db.LatestJobOfKind(ctx, c.ID, db.KindPoster)
		if row.LastError == nil || *row.LastError != db.MsgInterruptedByRestart {
			t.Fatalf("the job row was rewritten: %v", row.LastError)
		}
	})

	t.Run("trim", func(t *testing.T) {
		e := newEnv(t)
		owner, cookie := e.newUser("owner")
		c, _, _ := e.readyClip(owner, clipOpts{})
		if r := e.doJSON("POST", "/api/clips/"+c.ID+"/trim", trimBody(1000, 3000), withCookie(cookie)); r.code != 202 {
			t.Fatalf("POST: %d %s", r.code, r.body)
		}
		job := e.claimTrim(c.ID)
		fail(t, e, job, c.ID, db.MsgInterruptedByRestart)
		r := e.do("GET", "/api/clips/"+c.ID+"/trim", nil, withCookie(cookie))
		if r.code != 200 || r.str("state") != "failed" {
			t.Fatalf("failed status = %d %s", r.code, r.body)
		}
		if got := r.str("error"); got != "the trim failed" {
			t.Fatalf("error = %q, want the trim's own generic sentence", got)
		}
	})

	t.Run("replace", func(t *testing.T) {
		e := newEnv(t)
		owner, cookie := e.newUser("owner")
		c, _, _ := e.readyClip(owner, clipOpts{})
		if rr := e.replaceReq(c.ID, "file", "new.mp4", testVideo(300), withCookie(cookie)); rr.code != 202 {
			t.Fatalf("replace: %d %s", rr.code, rr.body)
		}
		job, clip, ok, err := e.db.ClaimMediaJob(ctx, e.now())
		if err != nil || !ok || clip.ID != c.ID || job.Kind != db.KindReplace {
			t.Fatalf("ClaimMediaJob = %+v %v %v", job, ok, err)
		}
		fail(t, e, job, c.ID, db.MsgInterruptedByRestart)
		r := e.do(http.MethodGet, "/api/clips/"+c.ID+"/replace", nil, withCookie(cookie))
		if r.code != 200 || r.str("state") != "failed" {
			t.Fatalf("failed status = %d %s", r.code, r.body)
		}
		if got := r.str("error"); got != "the replacement failed" {
			t.Fatalf("error = %q, want the replacement's own generic sentence", got)
		}
	})

	// The gif has no closed vocabulary — its last_error is publicReason's
	// redacted "stage: message", which is what an operator and a viewer both
	// need — so the ONE text it drops is the one db itself writes. The route
	// then says only its own sentence, to a caller who is not signed in at all.
	t.Run("preview gif", func(t *testing.T) {
		e := newEnv(t)
		owner, _ := e.newUser("owner")
		c, _, _ := e.readyClip(owner, clipOpts{})
		if r := e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil); r.code != 202 {
			t.Fatalf("first GET = %d", r.code)
		}
		job := e.claimGIF(c.ID)
		fail(t, e, job, c.ID, db.MsgInterruptedByRestart)
		r := e.do("GET", "/api/clips/"+c.ID+"/preview.gif", nil)
		if r.code != 503 || r.errCode() != "gif_failed" {
			t.Fatalf("GET after a failed job = %d %s", r.code, r.body)
		}
		msg, _ := r.json["message"].(string)
		if msg != "the preview could not be made" {
			t.Fatalf("message = %q, want the route's own sentence and nothing else", msg)
		}
		if strings.Contains(msg, "restart") {
			t.Fatalf("an anonymous caller was told about the server's restart: %q", msg)
		}
	})
}
