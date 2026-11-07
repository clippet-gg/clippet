package web

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"clippet/internal/db"
)

// The watch page's quality selector (§20 errata 205, 211).
//
// Three properties run through every test here:
//   - it is EVERY viewer's control, not the owner's: choosing what to stream is
//     a viewer's decision, so an anonymous page carries it too;
//   - it is server-rendered, so a browser with no JavaScript shows which tier is
//     playing and the <video> src is that tier's own URL;
//   - a tier with no FILE is never an option. What a missing tier gets is one
//     short line, and nothing to click.

// tierClip is `f.ready` with the tier columns of one shape or another. The
// fixture's own clip is a legacy-shaped row — a rendition and no tier columns —
// which is the FULL-tier case.
func tierClip(f *fixture, mutate func(*db.Clip)) db.Clip {
	c := f.ready
	if mutate != nil {
		mutate(&c)
	}
	return c
}

func i(v int) *int        { return &v }
func i64v(v int64) *int64 { return &v }
func s(v string) *string  { return &v }

func TestQualitySelectorOffersOnlyTiersThatExist(t *testing.T) {
	f := newFixture(t)
	srv := serverFor(t, f)

	t.Run("one tier: no control, and the player is on it", func(t *testing.T) {
		body := renderClip(t, srv, srv.clipModel(ClipView{Clip: f.ready, URLs: builder(t, testBase)}))
		mustNotContain(t, body, `data-quality-select`)
		mustContain(t, body, `src="/api/clips/`+f.ready.ID+`/video.mp4"`)
	})

	t.Run("original and light: two options, best first", func(t *testing.T) {
		c := tierClip(f, func(c *db.Clip) {
			// A playable upload with a card copy beside it: the shape a 1080p60
			// faststart mp4 produces (no Full at all).
			c.RenditionKey, c.RenditionSize, c.RenditionETag = nil, nil, nil
			c.OriginalPlayable = true
			c.OriginalWidth, c.OriginalHeight = i(1920), i(1080)
			c.OriginalFPSNum, c.OriginalFPSDen = i(60), i(1)
			c.OriginalCodec = "h264"
			c.SourceSize = 38 << 20
			c.SocialKey, c.SocialSize, c.SocialETag = s("sk"), i64v(2<<20), s("se")
			c.SocialWidth, c.SocialHeight = i(1280), i(720)
			c.SocialFPSNum, c.SocialFPSDen = i(30), i(1)
			c.FullState, c.LightState = db.TierSkipped, db.TierReady
		})
		m := srv.clipModel(ClipView{Clip: c, URLs: builder(t, testBase)})
		if len(m.Tiers) != 2 || !m.HasTierChoice {
			t.Fatalf("tiers = %+v HasTierChoice=%v", m.Tiers, m.HasTierChoice)
		}
		if m.Tiers[0].Tier != db.TierNameOriginal || !m.Tiers[0].Selected {
			t.Fatalf("the first option must be the best tier, selected: %+v", m.Tiers[0])
		}
		// REAL NUMBERS, which is the whole of the owner's ask.
		if got := m.Tiers[0].Label; got != "Original 1920×1080 60fps · 38.0 MiB" {
			t.Fatalf("original label = %q", got)
		}
		if got := m.Tiers[1].Label; got != "Light 1280×720 30fps · 2.0 MiB" {
			t.Fatalf("light label = %q", got)
		}
		// The player starts on the Original, at /original — the upload itself.
		if m.VideoURL != "/api/clips/"+c.ID+"/original" {
			t.Fatalf("the player's src = %q, want the upload", m.VideoURL)
		}
		body := renderClip(t, srv, m)
		mustContain(t, body,
			`<div class="quality" data-quality>`,
			`<select name="quality" data-quality-select>`,
			`<option value="/api/clips/`+c.ID+`/original" data-tier="original" selected="selected">`,
			`<option value="/api/clips/`+c.ID+`/light.mp4" data-tier="light">`,
			`src="/api/clips/`+c.ID+`/original"`)
		mustNotContain(t, body, `data-tier-note`)
		assertNoInline(t, "quality selector", body)
	})

	t.Run("a remuxed original is the Original tier, at /video.mp4", func(t *testing.T) {
		c := tierClip(f, func(c *db.Clip) {
			c.OriginalPlayable, c.OriginalRemuxed = true, true
			c.OriginalWidth, c.OriginalHeight = i(2560), i(1440)
			c.OriginalFPSNum, c.OriginalFPSDen = i(60), i(1)
			c.FullState, c.LightState = db.TierSkipped, db.TierSkipped
		})
		m := srv.clipModel(ClipView{Clip: c, URLs: builder(t, testBase)})
		if len(m.Tiers) != 1 || m.Tiers[0].Tier != db.TierNameOriginal {
			t.Fatalf("tiers = %+v, want one original", m.Tiers)
		}
		if m.Tiers[0].URL != "/api/clips/"+c.ID+"/video.mp4" {
			t.Fatalf("a remuxed original plays at %q, want /video.mp4", m.Tiers[0].URL)
		}
		// The remux carries no `full` tier: not one frame was re-encoded.
		for _, tier := range m.Tiers {
			if tier.Tier == db.TierNameFull {
				t.Fatal("a remux was offered as a Full tier")
			}
		}
	})

	t.Run("a better tier still encoding: one line, no option", func(t *testing.T) {
		c := tierClip(f, func(c *db.Clip) {
			c.RenditionKey, c.RenditionSize, c.RenditionETag = nil, nil, nil
			c.SocialKey, c.SocialSize, c.SocialETag = s("sk"), i64v(2<<20), s("se")
			c.SocialWidth, c.SocialHeight = i(1280), i(720)
			c.SocialFPSNum, c.SocialFPSDen = i(30), i(1)
			c.OriginalWidth, c.OriginalHeight = i(2560), i(1440)
			c.FullState, c.LightState = db.TierEncoding, db.TierReady
		})
		m := srv.clipModel(ClipView{Clip: c, URLs: builder(t, testBase)})
		if m.TierNote != "Full quality still encoding." {
			t.Fatalf("TierNote = %q", m.TierNote)
		}
		if len(m.Tiers) != 1 || m.Tiers[0].Tier != db.TierNameLight {
			t.Fatalf("tiers = %+v, want only the light copy", m.Tiers)
		}
		// This is the ONE case where a ready clip polls (§20 erratum 211).
		if m.ProgressURL == "" {
			t.Fatal("a clip whose better tier is encoding must carry a ProgressURL")
		}
		body := renderClip(t, srv, m)
		mustContain(t, body, `data-tier-note>Full quality still encoding.`, `data-progress-url`)
		mustNotContain(t, body, `data-quality-select`) // one tier is not a choice
		assertNoInline(t, "encoding note", body)
	})

	t.Run("a failed Full says so and stops polling", func(t *testing.T) {
		c := tierClip(f, func(c *db.Clip) {
			c.SocialKey, c.SocialSize, c.SocialETag = s("sk"), i64v(1<<20), s("se")
			c.FullState, c.LightState = db.TierFailed, db.TierReady
			c.RenditionKey, c.RenditionSize, c.RenditionETag = nil, nil, nil
		})
		m := srv.clipModel(ClipView{Clip: c, URLs: builder(t, testBase)})
		if m.TierNote != "Full quality could not be encoded." {
			t.Fatalf("TierNote = %q", m.TierNote)
		}
		if m.ProgressURL != "" {
			t.Fatal("a clip whose better tier failed must stop polling for it")
		}
	})

	t.Run("a failed Light is the owner's line and nobody else's", func(t *testing.T) {
		c := tierClip(f, func(c *db.Clip) {
			c.SocialKey, c.SocialSize, c.SocialETag = nil, nil, nil
			c.FullState, c.LightState = db.TierReady, db.TierFailed
		})
		u := builder(t, testBase)
		owner := srv.clipModel(ClipView{Clip: c, URLs: u, IsOwner: true})
		if owner.TierNote != "Card copy could not be made." {
			t.Fatalf("the owner's TierNote = %q", owner.TierNote)
		}
		if owner.ProgressURL != "" {
			t.Fatal("a failed tier must stop the poll")
		}
		body := renderClip(t, srv, owner)
		mustContain(t, body, `data-tier-note>Card copy could not be made.`)
		assertNoInline(t, "failed light note", body)
		// The card still works — it falls back to the play object — so a viewer
		// is missing nothing and is told nothing (§20 erratum 211).
		if stranger := srv.clipModel(ClipView{Clip: c, URLs: u}); stranger.TierNote != "" {
			t.Fatalf("a stranger was told %q", stranger.TierNote)
		}
	})

	t.Run("a clip that is not ready has no selector at all", func(t *testing.T) {
		m := srv.clipModel(ClipView{Clip: f.processing, URLs: builder(t, testBase)})
		if len(m.Tiers) != 0 || m.HasTierChoice || m.TierNote != "" {
			t.Fatalf("a %s clip has a selector: %+v", f.processing.State, m.Tiers)
		}
	})
}

func TestQualitySelectorIsEveryViewersControl(t *testing.T) {
	f := newFixture(t)
	// The fixture's ready clip with two real tiers, written onto the row so both
	// pages are rendered through the real handler.
	f.setTiers(f.ready.ID, `original_playable = 1, original_width = 1920, original_height = 1080,
		original_fps_num = 60, original_fps_den = 1, original_codec = 'h264',
		rendition_key = NULL, rendition_size = NULL, rendition_etag = NULL,
		social_key = 'sk', social_size = 1048576, social_etag = 'se',
		social_width = 1280, social_height = 720, social_fps_num = 30, social_fps_den = 1,
		full_state = 'skipped', light_state = 'ready'`)
	owner := get(f.handler(session(f.owner)), "/"+f.ready.ID).Body.String()
	anon := get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String()
	for name, body := range map[string]string{"owner": owner, "anonymous": anon} {
		if !strings.Contains(body, `data-quality-select`) {
			t.Errorf("the %s page has no quality selector", name)
		}
		if !strings.Contains(body, `data-tier="light"`) {
			t.Errorf("the %s page does not offer the light copy", name)
		}
		assertNoInline(t, name+" quality page", body)
	}
	// An anonymous page still carries nothing owner-private.
	mustNotContain(t, anon, `data-views`, `data-owner-controls`)
}

// setTiers writes tier columns straight onto a clip row. The db package exposes
// no such write on purpose — these columns belong to a transcode's commit — and
// what this test needs is a ROW of a given shape rather than a run that produces
// one.
func (f *fixture) setTiers(id, sets string) {
	f.t.Helper()
	raw, err := sql.Open("sqlite", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		f.t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(context.Background(), "UPDATE clips SET "+sets+" WHERE id = ?", id); err != nil {
		f.t.Fatalf("set tier columns: %v", err)
	}
}

func TestTierLabel(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		tier                 string
		w, h, fpsNum, fpsDen *int
		bytes                *int64
		want                 string
	}{
		{"everything known", db.TierNameFull, i(1920), i(1080), i(60), i(1), i64v(40 << 20),
			"Full 1920×1080 60fps · 40.0 MiB"},
		{"a fractional rate is not rounded", db.TierNameLight, i(1280), i(720), i(30000), i(1001), nil,
			"Light 1280×720 29.97fps"},
		{"no numbers at all", db.TierNameOriginal, nil, nil, nil, nil, nil, "Original"},
		{"dims only", db.TierNameOriginal, i(640), i(360), nil, nil, nil, "Original 640×360"},
		{"a zero size is not a size", db.TierNameLight, i(640), i(360), i(30), i(1), i64v(0),
			"Light 640×360 30fps"},
	} {
		if got := tierLabel(tc.tier, tc.w, tc.h, tc.fpsNum, tc.fpsDen, tc.bytes); got != tc.want {
			t.Errorf("%s: tierLabel = %q, want %q", tc.name, got, tc.want)
		}
	}
}
