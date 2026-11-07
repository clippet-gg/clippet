package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The duplicate answer on the bot's side (§20 erratum 326): the ingest says the
// bound account already held these bytes, and the job posts the existing clip's
// link exactly as a new one — plus, behind the upload form, the private line
// that the form was not applied. The link is never held for a redraw.

func dupIngest(_ context.Context, src IngestSource) (IngestResult, error) {
	_, _ = io.ReadAll(src.Body)
	return IngestResult{ClipID: testClipID, CardReady: true, Existing: true}, nil
}

// dupDroppedIngest is a duplicate whose form had a submit already answered
// "Changes saved.": the hook dropped it.
func dupDroppedIngest(ctx context.Context, src IngestSource) (IngestResult, error) {
	res, err := dupIngest(ctx, src)
	res.SubmitDropped = true
	return res, err
}

// TestBotFormDuplicateSaysNotAppliedAndPostsTheLink: a Form job whose upload
// was a duplicate that dropped a held submit posts SentenceFormNotApplied as an
// EPHEMERAL follow-up and the link as the ordinary public one, and holds
// nothing for a redraw.
func TestBotFormDuplicateSaysNotAppliedAndPostsTheLink(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	job := formJob(f, dupDroppedIngest)
	job.Form = true
	f.bot.Clip(job)
	link, _ := json.Marshal(ClipMessage(f.bot.urls, testClipID))
	calls := f.waitCalls(t, "the line and the link", func(cs []apiCall) bool {
		n := 0
		for _, c := range cs {
			if isFollowup(c.method, c.path) {
				n++
			}
		}
		return n == 2
	})
	var sawLine, sawLink bool
	for _, c := range calls {
		if !isFollowup(c.method, c.path) {
			continue
		}
		switch {
		case bytes.Equal(c.body, link):
			sawLink = true
		case strings.Contains(string(c.body), `"content":"`+SentenceFormNotApplied+`"`) && strings.Contains(string(c.body), `"flags":64`):
			sawLine = true
		default:
			t.Fatalf("an unexpected follow-up: %s", c.body)
		}
	}
	if !sawLine || !sawLink {
		t.Fatalf("line=%v link=%v; calls %v", sawLine, sawLink, describe(calls))
	}
	if f.bot.links.len() != 0 {
		t.Fatal("a duplicate's link was held for a redraw")
	}
	if !strings.Contains(f.logs.String(), "existing=true") {
		t.Fatalf("the job's line does not say existing: %s", f.logs.String())
	}
}

// TestBotFormDuplicateWithNoHeldSubmitIsTheLinkAlone: no submit was held when
// the duplicate was found, so a later one hears SentenceFormNotApplied from its
// own answer; the job says nothing of it (run 8: the line is heard once).
func TestBotFormDuplicateWithNoHeldSubmitIsTheLinkAlone(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	job := formJob(f, dupIngest)
	job.Form = true
	f.bot.Clip(job)
	link, _ := json.Marshal(ClipMessage(f.bot.urls, testClipID))
	f.waitCalls(t, "the link", func(cs []apiCall) bool {
		for _, c := range cs {
			if isFollowup(c.method, c.path) && bytes.Equal(c.body, link) {
				return true
			}
		}
		return false
	})
	for _, c := range f.callsSnapshot() {
		if isFollowup(c.method, c.path) && strings.Contains(string(c.body), SentenceFormNotApplied) {
			t.Fatalf("the job said the line a later submit will say: %s", c.body)
		}
	}
}

// TestBotDuplicateWithoutTheFormIsJustTheLink: the deferred `/upload` (and
// Enrich, which never has the form) edits in the link and says nothing else.
func TestBotDuplicateWithoutTheFormIsJustTheLink(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	f.bot.Clip(okJob(f, dupIngest))
	f.waitEdited(t)
	link, _ := json.Marshal(ClipMessage(f.bot.urls, testClipID))
	for _, c := range f.callsSnapshot() {
		if isFollowup(c.method, c.path) {
			t.Fatalf("a deferred duplicate posted a follow-up: %s", c.body)
		}
		if c.method == http.MethodPatch && !bytes.Equal(c.body, link) {
			t.Fatalf("the edit is %s, want the link alone", c.body)
		}
	}
}
