package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"clippet/internal/db"
)

// The watch page's three owner-and-viewer additions (ROADMAP #5, #6, #8, #29;
// §20 errata 132-134). Two properties run through every test here:
//   - what a STRANGER receives grows by nothing an owner uses. The card-image
//     control and the scrubber are inside the owner block, and the diff of an
//     anonymous page is empty of both.
//   - nothing that needs JavaScript renders as if it worked without it. The
//     scrubber, the "= now" buttons and the Loop toggle are all server-
//     rendered `hidden`; the number boxes, the UTC dates and the player are
//     what a reader with no client gets, exactly as before.

// TestSharePageDatesAreMachineReadable is ROADMAP #8's server half: the
// absolute UTC sentence is unchanged and is now inside a <time datetime>, so
// a client can re-render it in the viewer's own locale and put a countdown
// under it — and a reader without one still sees the sentence.
func TestSharePageDatesAreMachineReadable(t *testing.T) {
	f := newFixture(t)
	s := serverFor(t, f)
	// A ready clip with a countdown, rendered through the real template set.
	c := f.ready
	soon := f.now + 7*86400
	c.ExpiresAt = &soon
	body := renderClip(t, s, s.clipModel(ClipView{Clip: c, URLs: builder(t, testBase), IsOwner: true}))
	// The instant, as RFC3339 UTC, on both dates.
	mustContain(t, body,
		`<time datetime="`+rfc3339(c.CreatedAt)+`" data-local>`+formatDate(c.CreatedAt)+`</time>`,
		`<time datetime="`+rfc3339(*c.ExpiresAt)+`" data-local data-countdown>`+formatDate(*c.ExpiresAt)+`</time>`)
	// The English is OUTSIDE the element: the <time> carries a date and the
	// sentence carries the meaning.
	mustContain(t, body, `<dd data-expiry>expires <time`)
	// …and the UTC text itself is still the fallback, unchanged.
	mustContain(t, body, formatDate(*c.ExpiresAt))
	assertNoInline(t, "clip with a countdown", body)

	// A clip kept forever names no instant, so it gets no <time> and — this
	// is the point — no countdown either.
	kept := get(f.handler(session(f.owner)), "/"+f.ready.ID).Body.String()
	mustContain(t, kept, `<dd data-expiry>kept forever</dd>`)
	mustNotContain(t, kept, `data-countdown`)

	// An expiry the sweeper has not reached yet keeps its own sentence, split
	// the same way.
	past := f.ready
	gone := f.now - 60
	past.ExpiresAt = &gone
	m := s.clipModel(ClipView{Clip: past, URLs: builder(t, testBase), IsOwner: true})
	if m.ExpiryPrefix != "expired " || m.ExpirySuffix != " (pending deletion)" || m.ExpiryISO == "" {
		t.Fatalf("a pending-deletion lifetime = %q + <time %q> + %q", m.ExpiryPrefix, m.ExpiryISO, m.ExpirySuffix)
	}
}

// TestSharePagePlayerExtras is ROADMAP #29's server half: the Loop toggle and
// the keyboard legend belong to every viewer of a ready clip (they are player
// chrome, not owner data and not a call to action), and both are inert until
// a client reveals them.
func TestSharePagePlayerExtras(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name string
		auth string
	}{{"owner", "owner"}, {"anonymous", "anon"}} {
		t.Run(tc.name, func(t *testing.T) {
			h := f.handler(session(f.owner))
			if tc.auth == "anon" {
				h = f.handler(passAuth{})
			}
			body := get(h, "/"+f.ready.ID).Body.String()
			mustContain(t, body, `<div class="player-extras" data-player-extras hidden>`,
				`data-loop aria-pressed="false"`, `space play`)
			assertNoInline(t, tc.name+" player extras", body)
		})
	}
	// A clip that is not ready has no player, so it has no player controls.
	body := get(f.handler(session(f.owner)), "/"+f.processing.ID).Body.String()
	mustNotContain(t, body, `data-player-extras`, `data-loop`)

	// The EMBED never gets them: its autoplay/muted/loop are the embedder's
	// choice in the URL (§20 erratum 77), and a viewer preference must not
	// override a contract other people's pages depend on.
	embed := get(f.handler(passAuth{}), "/embed/"+f.ready.ID).Body.String()
	mustNotContain(t, embed, `data-player-extras`, `data-loop`)
	// The wordmark it carries is the overlay and nothing else (§20 erratum
	// 246, the owner's amendment of 244's "an iframe is someone else's page"):
	// one link, to the share page, out of the iframe; no header slot, no nav,
	// no second link.
	mustContain(t, embed, embedMark(f.ready.ID))
	mustNotContain(t, embed, `class="wordmark" href="/"`, `<header`, `<nav`)
	if n := strings.Count(embed, "<a "); n != 1 {
		t.Errorf("the embed carries %d links, want exactly the overlay", n)
	}
}

// TestSharePageScrubberIsOwnerOnlyAndInert is ROADMAP #6's server half.
func TestSharePageScrubberIsOwnerOnlyAndInert(t *testing.T) {
	f := newFixture(t)

	owner := get(f.handler(session(f.owner)), "/"+f.ready.ID).Body.String()
	mustContain(t, owner,
		`<div class="scrubber" data-scrubber hidden>`,
		`data-scrub-track`, `data-scrub-range`,
		`data-scrub-handle="start" role="slider" aria-label="Trim start"`,
		`data-scrub-handle="end" role="slider" aria-label="Trim end"`,
		`data-scrub-set="start" hidden`, `data-scrub-set="end" hidden`,
		// The number boxes are still there: they are the API's truth and the
		// only half of this control that works with scripting off.
		`data-trim-start>`, `data-trim-end>`)
	assertNoInline(t, "clip with a scrubber", owner)

	// A stranger gets none of it — the whole control lives in the owner block.
	anon := get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String()
	mustNotContain(t, anon, "data-scrubber", "data-scrub-", "data-trim", "data-poster")
}

// TestSharePageCardImageControl is ROADMAP #5's server half: the control is
// the owner's, it says whether a frame was CHOSEN or picked automatically,
// and it carries the cut's start so the client can turn the player's clock
// into the API's timeline.
func TestSharePageCardImageControl(t *testing.T) {
	f := newFixture(t)
	s := serverFor(t, f)

	auto := s.clipModel(ClipView{Clip: f.ready, URLs: builder(t, testBase), IsOwner: true})
	if !auto.CanPoster || auto.PosterChosen || auto.PosterSeconds != "" {
		t.Fatalf("an automatic card image = %+v, want CanPoster with no choice", auto)
	}
	if auto.PosterNote != "Card image: automatic." {
		t.Errorf("note = %q", auto.PosterNote)
	}

	chosen := f.ready
	at := 2.5
	chosen.PosterSeconds = &at
	m := s.clipModel(ClipView{Clip: chosen, URLs: builder(t, testBase), IsOwner: true})
	if !m.PosterChosen || m.PosterSeconds != "2.500" {
		t.Fatalf("a chosen card image = %+v", m)
	}
	if !strings.Contains(m.PosterNote, "0:03") && !strings.Contains(m.PosterNote, "0:02") {
		t.Errorf("note = %q, want the moment in clock form", m.PosterNote)
	}

	body := renderClip(t, s, m)
	mustContain(t, body, `data-poster data-poster-url="/api/clips/`, `data-poster-status-url="/api/clips/`,
		`data-cut-start-ms="0"`, `data-poster-seconds="2.500"`, `data-poster-go>Use this frame`)
	assertNoInline(t, "clip with a chosen card image", body)

	// A clip too short to point at gets no control rather than one whose
	// every request would be a 400.
	tiny := f.ready
	fifty := int64(50)
	tiny.DurationMS = &fifty
	if m := s.clipModel(ClipView{Clip: tiny, URLs: builder(t, testBase), IsOwner: true}); m.CanPoster {
		t.Error("a 50 ms clip offers a card-image choice it could never honour")
	}
	// A clip that is not ready has no rendition to take a frame from.
	if m := s.clipModel(ClipView{Clip: f.processing, URLs: builder(t, testBase), IsOwner: true}); m.CanPoster {
		t.Error("a processing clip offers a card-image choice")
	}
}

// A poster job that is queued or running is advertised in the markup, so the
// owner's browser rejoins it without a GET …/poster that would be a 404 on
// every clip nobody has chosen a frame for (§20 erratum 87's rule).
func TestSharePageAdvertisesALivePosterJob(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.db.EnqueuePoster(ctx, f.ready.ID, `{"seconds":1}`, f.now); err != nil {
		t.Fatal(err)
	}
	body := get(f.handler(session(f.owner)), "/"+f.ready.ID).Body.String()
	mustContain(t, body, `data-poster-live`)

	// …and once it is terminal the page stops advertising it.
	job, _, _, err := f.db.ClaimMediaJob(ctx, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != db.KindPoster {
		t.Fatalf("claimed %s, want %s", job.Kind, db.KindPoster)
	}
	if err := f.db.CompleteJob(ctx, job.ID, db.ClaimStamp(job), "done", "", f.now); err != nil {
		t.Fatal(err)
	}
	body = get(f.handler(session(f.owner)), "/"+f.ready.ID).Body.String()
	mustNotContain(t, body, `data-poster-live`)
}

// serverFor builds a bare server for the model-level assertions above.
func serverFor(t *testing.T, f *fixture) *server {
	t.Helper()
	s, err := newServer(Deps{DB: f.db, Auth: passAuth{}, URLs: builder(t, testBase),
		Now: func() time.Time { return time.Unix(f.now, 0).UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// renderClip renders one clipPage through the real template set.
func renderClip(t *testing.T, s *server, m clipPage) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	m.page = s.basePage(req, "x")
	rec := httptest.NewRecorder()
	s.render(rec, req, "clip", http.StatusOK, m)
	return rec.Body.String()
}

// The card-image control carries the CUT's start, and it reads it itself
// rather than borrowing the trim control's: a clip can be too short to trim
// and still be a cut of something longer, and a client that added zero to the
// player's clock would then send a moment the API refuses.
func TestCardImageControlCarriesTheCutStart(t *testing.T) {
	f := newFixture(t)
	s := serverFor(t, f)
	c := f.ready
	// A 400 ms cut of an original this row has never measured: the trim
	// control does not render (every window it could send is a 400), and the
	// card-image control must still know where the cut begins.
	start, end, dur := int64(6000), int64(6400), int64(400)
	c.TrimStartMS, c.TrimEndMS, c.DurationMS, c.SourceDurationMS = &start, &end, &dur, nil

	m := s.clipModel(ClipView{Clip: c, URLs: builder(t, testBase), IsOwner: true})
	if m.CanTrim {
		t.Fatalf("a 400 ms clip offers a trim whose every window would be a 400")
	}
	if m.TrimStartMS != 0 {
		t.Fatalf("the trim control filled TrimStartMS = %d although it does not render", m.TrimStartMS)
	}
	if !m.CanPoster {
		t.Fatal("a 400 ms cut has a frame to choose and no control to choose it with")
	}
	if m.PosterCutStartMS != 6000 {
		t.Fatalf("PosterCutStartMS = %d, want the cut's own start", m.PosterCutStartMS)
	}
	mustContain(t, renderClip(t, s, m), `data-cut-start-ms="6000"`)
}

// TestExpiredUnsweptClipPageIsTheDocumentedState pins a nit sweep's "an expired,
// unswept clip's page answered 200": that answer is the design, not a cache or
// a clock fault. Until the sweeper tombstones the row the share page renders
// with the lifetime sentence "expired <date> (pending deletion)" (§20 erratum
// 133); only the `deleting` state is the §9.1 404. The 404 an E2E step saw came
// from the sweep having destroyed the row, never from the page's own rule.
func TestExpiredUnsweptClipPageIsTheDocumentedState(t *testing.T) {
	f := newFixture(t)
	if err := f.db.ForceExpire(context.Background(), f.ready.ID, f.now-60); err != nil {
		t.Fatal(err)
	}
	rec := get(f.handler(passAuth{}), "/"+f.ready.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("an expired, unswept clip's share page = %d, want 200 (pending deletion)", rec.Code)
	}
	mustContain(t, rec.Body.String(), `data-expiry>expired <time`, `</time> (pending deletion)</dd>`)
}
