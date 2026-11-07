package web

import (
	"context"
	"io"
	"regexp"
	"strings"
	"testing"

	"clippet/internal/db"
)

// The web half of bundle W1: the trim control's new home (§20 erratum 221), the
// filmstrip attributes it carries (erratum 220), and the library row's stage
// phrase (erratum 224).

// withFilmstrip publishes a sheet on a clip through the real writer, so the page
// is rendered from a row the worker could have produced.
func (f *fixture) withFilmstrip(c db.Clip) db.Clip {
	f.t.Helper()
	ctx := context.Background()
	job, err := f.db.EnqueueFilmstrip(ctx, c.ID, f.now)
	if err != nil {
		f.t.Fatalf("EnqueueFilmstrip: %v", err)
	}
	if _, _, ok, err := f.db.ClaimMediaJob(ctx, f.now); err != nil || !ok {
		f.t.Fatalf("ClaimMediaJob: ok=%v err=%v", ok, err)
	}
	swap, err := f.db.SetFilmstrip(ctx, job.ID, c.ID, db.FilmstripFields{
		Key: "clips/" + c.ID + "/filmstrip/strip-f8f8f8f8.jpg", Size: 12345, ETag: "strip-etag",
		Tiles: 24, TileW: 160, TileH: 90, IntervalMS: 250,
	}, *c.RenditionETag, f.now)
	if err != nil || !swap.OK {
		f.t.Fatalf("SetFilmstrip: %+v %v", swap, err)
	}
	return f.reload(c.ID)
}

// TestSharePageTrimSitsWithThePlayer is erratum 221's structural half: the trim
// control is rendered BEFORE the owner block, not inside it. The owner's first look
// found it a section and a scroll away from the picture it edits, and this is the
// assertion that keeps it where it now is — a later edit that puts it back inside
// Manage fails here rather than being noticed in a tour.
func TestSharePageTrimSitsWithThePlayer(t *testing.T) {
	f := newFixture(t)
	body := get(f.handler(session(f.owner)), "/"+f.ready.ID).Body.String()

	video := strings.Index(body, "<video")
	trim := strings.Index(body, `data-trim data-trim-url=`)
	ownerBlock := strings.Index(body, `data-owner-controls`)
	if video < 0 || trim < 0 || ownerBlock < 0 {
		t.Fatalf("player=%d trim=%d owner=%d; one of them is missing", video, trim, ownerBlock)
	}
	if !(video < trim && trim < ownerBlock) {
		t.Errorf("order is player=%d trim=%d owner=%d; the trim control must sit between the player "+
			"and the Manage section (§20 erratum 221)", video, trim, ownerBlock)
	}
	// It is a <section class="trim"> now, and its note is one paragraph rather
	// than a span inside a row of controls. `aria-label` is what keeps it a NAMED
	// landmark: out of Manage it lost the <h2> that used to name it, and a
	// <section> with no accessible name is not a region at all.
	mustContain(t, body, `<section class="trim" data-trim data-trim-url="/api/clips/`+f.ready.ID+`/trim"`,
		`aria-label="Trim"`, `<div class="row trim-row">`, `<p class="hint" data-trim-note>`)
	// Manage keeps everything else and gains nothing: the Trim heading is gone
	// from it because the control is not there any more.
	manage := body[ownerBlock:]
	if strings.Contains(manage, "data-trim ") {
		t.Error("the trim control is still inside the owner block")
	}
	if strings.Contains(manage, "<h2>Trim</h2>") {
		t.Error("Manage still carries a Trim heading with no control under it")
	}
	// The handles' words are their own elements, so they can be read at the
	// page's size instead of inside a 1.6rem disc (the owner's first look).
	mustContain(t, body, `<span class="handle-label">From</span>`, `<span class="handle-label">To</span>`)
	assertNoInline(t, "clip with the trim block by the player", body)

	// A stranger still gets none of it.
	anon := get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String()
	mustNotContain(t, anon, "data-trim", "data-scrubber", "data-filmstrip-url", "handle-label")
}

// TestSharePageFilmstripAttributes: the URL is always rendered (the route makes a
// sheet when there is none) and the geometry only when a sheet EXISTS — which is
// what lets the strip paint with the page instead of after a poll.
func TestSharePageFilmstripAttributes(t *testing.T) {
	f := newFixture(t)

	// No sheet yet: the address, and no layout to draw from.
	body := get(f.handler(session(f.owner)), "/"+f.ready.ID).Body.String()
	mustContain(t, body, `data-filmstrip-url="/api/clips/`+f.ready.ID+`/filmstrip.jpg"`)
	mustNotContain(t, body, `data-filmstrip-tiles`, `data-filmstrip-tile-w`, `data-filmstrip-interval-ms`)
	// The three empty <img> elements ship with no src: a page with no client and
	// a clip with no sheet request nothing at all.
	mustContain(t, body, `<div class="strip" data-scrub-strip hidden><img data-scrub-strip-img alt=""></div>`,
		`<span class="handle-tile" data-scrub-tile hidden><img data-scrub-tile-img alt=""></span>`)
	if strings.Contains(body, `data-scrub-strip-img src=`) || strings.Contains(body, `data-scrub-tile-img src=`) {
		t.Error("the filmstrip <img> elements ship with a src; they must be empty until app.js knows there is a sheet")
	}

	// With a sheet: the four measured numbers, so a client can place a tile.
	c := f.withFilmstrip(f.ready)
	body = get(f.handler(session(f.owner)), "/"+c.ID).Body.String()
	mustContain(t, body,
		`data-filmstrip-url="/api/clips/`+c.ID+`/filmstrip.jpg"`,
		`data-filmstrip-tiles="24"`, `data-filmstrip-tile-w="160"`,
		`data-filmstrip-tile-h="90"`, `data-filmstrip-interval-ms="250"`)
	assertNoInline(t, "clip with a filmstrip", body)

	// A visitor's page carries neither: the whole control is the owner's.
	anon := get(f.handler(passAuth{}), "/"+c.ID).Body.String()
	mustNotContain(t, anon, `data-filmstrip-url`, `data-filmstrip-tiles`)
}

// TestSharePageFilmstripNeedsAllFiveColumns: a row carrying part of a layout
// cannot come from this server (one UPDATE writes them together), and half a
// layout would place tiles wrongly. The page then says "no sheet", which sends
// the client to the route for a whole one.
func TestSharePageFilmstripNeedsAllFiveColumns(t *testing.T) {
	f := newFixture(t)
	c := f.withFilmstrip(f.ready)
	for _, tc := range []struct {
		name   string
		break_ func(*db.Clip)
	}{
		{"no key", func(c *db.Clip) { c.FilmstripKey = nil }},
		{"no tile count", func(c *db.Clip) { c.FilmstripTiles = nil }},
		{"no tile width", func(c *db.Clip) { c.FilmstripTileW = nil }},
		{"no tile height", func(c *db.Clip) { c.FilmstripTileH = nil }},
		{"no interval", func(c *db.Clip) { c.FilmstripIntervalMS = nil }},
		{"a zero tile count", func(c *db.Clip) { z := 0; c.FilmstripTiles = &z }},
		{"a zero interval", func(c *db.Clip) { var z int64; c.FilmstripIntervalMS = &z }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broken := c
			tc.break_(&broken)
			var m clipPage
			m.fillFilmstrip(broken, builder(t, testBase))
			if m.FilmstripURL == "" {
				t.Error("the URL is unconditional: the route makes a sheet when there is none")
			}
			if m.HasFilmstrip {
				t.Errorf("a row with %s was drawn as a filmstrip: %+v", tc.name, m)
			}
		})
	}
	// …and the whole row is drawn.
	var m clipPage
	m.fillFilmstrip(c, builder(t, testBase))
	if !m.HasFilmstrip || m.FilmstripTiles != 24 || m.FilmstripTileW != 160 ||
		m.FilmstripTileH != 90 || m.FilmstripIntervalMS != 250 {
		t.Errorf("a complete row = %+v", m)
	}
}

// TestLibraryRowCarriesTheStagePhrase is erratum 224: the striped bar says
// something is happening, and this says WHAT, server-rendered so a reader with no
// JavaScript sees it too.
func TestLibraryRowCarriesTheStagePhrase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// f.processing is mid-encode: give its transcode job the stage it would have.
	job, err := f.db.LatestJobOfKind(ctx, f.processing.ID, db.KindTranscode)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetJobStage(ctx, job.ID, *job.StartedAt, "encode", f.now); err != nil {
		t.Fatal(err)
	}
	body := get(f.handler(session(f.owner)), "/").Body.String()
	mustContain(t, body, `<p class="status" data-status>Processing… · encoding on the server</p>`)
	// A clip nothing has claimed yet says so in the label, with no phrase after
	// it: two words saying one thing is one word too many.
	mustContain(t, body, `<p class="status" data-status>Queued…</p>`)
	assertNoInline(t, "library with stage phrases", body)

	// Every stage of the §8.4 vocabulary has a phrase, and none of them prints a
	// raw stage name — a stage word is this server's vocabulary, not a reader's.
	for _, stage := range []string{"acquire", "probe", "encode", "verify", "poster", "store", "finalize"} {
		if err := f.db.SetJobStage(ctx, job.ID, *job.StartedAt, stage, f.now); err != nil {
			t.Fatal(err)
		}
		got := stageNote(db.ClipProcessing, stage)
		if got == "" {
			t.Errorf("stage %q has no phrase", stage)
		}
		if got == stage {
			t.Errorf("stage %q prints its own name", stage)
		}
		row := get(f.handler(session(f.owner)), "/").Body.String()
		if !strings.Contains(row, "· "+got) {
			t.Errorf("the library row for stage %q does not carry %q", stage, got)
		}
	}
	// An unknown stage yields no phrase rather than an internal identifier.
	if got := stageNote(db.ClipProcessing, "reticulating-splines"); got != "" {
		t.Errorf("an unknown stage rendered %q", got)
	}
	// And a ready row has nothing in progress to describe.
	if got := stageNote(db.ClipReady, "done"); got != "" {
		t.Errorf("a ready clip rendered %q", got)
	}
}

// jsStageNote matches one entry of app.js's `stageNotes` object.
var jsStageNote = regexp.MustCompile(`(?m)^\s*([a-z]+):\s*"([^"]*)",`)

// TestLibraryStageNotesMatchTheClient pins the two renderers of one line against
// each other (§20 erratum 224). The server ships the phrase in the HTML and the
// client's first poll writes over it: if the two lists ever drift, a library row
// changes its words a second after it loads, for no reason a reader could guess.
//
// It reads the SHIPPED script, so a change to either side that forgets the other
// fails here — the same technique the LibraryPageSize pin uses.
func TestLibraryStageNotesMatchTheClient(t *testing.T) {
	f := newFixture(t)
	raw, err := io.ReadAll(get(f.handler(passAuth{}), "/static/app.js").Body)
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	start := strings.Index(js, "const stageNotes = {")
	if start < 0 {
		t.Fatal("app.js has no `stageNotes` object; the server's phrases have nothing to agree with")
	}
	end := strings.Index(js[start:], "};")
	if end < 0 {
		t.Fatal("app.js's `stageNotes` object is not closed")
	}
	pairs := jsStageNote.FindAllStringSubmatch(js[start:start+end], -1)
	if len(pairs) < 8 {
		t.Fatalf("app.js's stageNotes has %d entries; the §8.4 vocabulary has more", len(pairs))
	}
	// Both directions, which is the whole point: the loop below catches a client
	// entry the server does not know (or words differently), and the second loop
	// catches a server phrase the client never says — a row that changes its
	// words a second after it loads, either way round.
	seen := make(map[string]bool, len(pairs))
	for _, m := range pairs {
		stage, phrase := m[1], m[2]
		seen[stage] = true
		if phrase == "" {
			t.Errorf("stage %q: app.js's phrase is empty", stage)
		}
		if got := stageNote(db.ClipProcessing, stage); got != phrase {
			t.Errorf("stage %q: app.js says %q, internal/web says %q", stage, phrase, got)
		}
	}
	for stage, phrase := range stageNotes {
		if !seen[stage] {
			t.Errorf("stage %q: internal/web says %q and app.js's stageNotes has no entry at all", stage, phrase)
		}
	}
}
