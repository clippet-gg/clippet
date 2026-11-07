package web

import (
	"net/url"
	"strings"
	"testing"

	"clippet/internal/db"
	"clippet/internal/urls"
)

func builder(t *testing.T, base string) urls.Builder {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return urls.New(u, "b1")
}

func ip(v int) *int { return &v }

// readyClip has poster dims deliberately different from the rendition dims
// so a tag that copies the wrong pair is caught (spec §2: "we shipped that
// bug").
func readyClip() db.Clip {
	ms := int64(6200) // 6.2s -> video:duration 6, and it must ROUND, not truncate
	return db.Clip{
		ID: "aZ3kQ9xB2mLp", OwnerID: "u1", Title: "E2E std", Description: "a <b>desc</b>", State: db.ClipReady,
		PosterWidth: ip(640), PosterHeight: ip(360), Width: ip(1280), Height: ip(720), DurationMS: &ms,
	}
}

// metaMap keys every tag by attr+key and fails on duplicates.
func metaMap(t *testing.T, ms []Meta) map[string]string {
	t.Helper()
	out := make(map[string]string, len(ms))
	for _, m := range ms {
		if m.Attr != "property" && m.Attr != "name" {
			t.Errorf("tag %s has attr %q", m.Key, m.Attr)
		}
		k := m.Attr + " " + m.Key
		if _, dup := out[k]; dup {
			t.Errorf("duplicate tag %s", k)
		}
		out[k] = m.Content
	}
	return out
}

func assertExact(t *testing.T, got []Meta, want map[string]string) {
	t.Helper()
	gm := metaMap(t, got)
	for k, v := range want {
		if gv, ok := gm[k]; !ok {
			t.Errorf("missing %s", k)
		} else if gv != v {
			t.Errorf("%s = %q, want %q", k, gv, v)
		}
	}
	for k := range gm {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected tag %s = %q", k, gm[k])
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d tags, want %d", len(got), len(want))
	}
}

func wantReady(base string, secure bool) map[string]string {
	id := "aZ3kQ9xB2mLp"
	poster := base + "/api/clips/" + id + "/poster.jpg"
	social := base + "/api/clips/" + id + "/social.mp4"
	host := builderHost(base)
	// The card's description is `{m:ss} · {the owner's words} · {the clause}`
	// since §20 erratum 287; readyClip() is 6 s and has no Light copy, so the
	// clause is absent here and the duration is not.
	desc := "0:06 · a <b>desc</b>"
	m := map[string]string{
		"property og:title":        "E2E std",
		"property og:description":  desc,
		"property og:type":         "video.other",
		"property og:url":          base + "/" + id,
		"property og:site_name":    host,
		"property og:image":        poster,
		"property og:image:type":   "image/jpeg",
		"property og:image:width":  "640",
		"property og:image:height": "360",
		"property og:image:alt":    "A still frame from the clip: E2E std",
		"property og:video":        social,
		"property og:video:url":    social,
		"property og:video:type":   "video/mp4",
		"property og:video:width":  "1280",
		"property og:video:height": "720",
		"property video:duration":  "6",

		"name twitter:card":                       "player",
		"name twitter:title":                      "E2E std",
		"name twitter:description":                desc,
		"name twitter:image":                      poster,
		"name twitter:image:alt":                  "A still frame from the clip: E2E std",
		"name twitter:player":                     base + "/embed/" + id,
		"name twitter:player:width":               "1280",
		"name twitter:player:height":              "720",
		"name twitter:player:stream":              social,
		"name twitter:player:stream:content_type": "video/mp4",
		"name description":                        desc,
	}
	if secure {
		m["property og:video:secure_url"] = social
	}
	return m
}

// builderHost is the site line for a base URL: its hostname, no scheme and no
// port (§20 erratum 287).
func builderHost(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	return u.Hostname()
}

func TestBuildMetaReadyHTTP(t *testing.T) {
	got := BuildMeta(ClipView{Clip: readyClip(), URLs: builder(t, "http://example.test:8481")})
	assertExact(t, got, wantReady("http://example.test:8481", false))
	for _, m := range got {
		if m.Key == "og:video:secure_url" {
			t.Fatal("og:video:secure_url emitted on http (violates the OG spec)")
		}
	}
}

func TestBuildMetaReadyHTTPS(t *testing.T) {
	got := BuildMeta(ClipView{Clip: readyClip(), URLs: builder(t, "https://clips.example.com")})
	assertExact(t, got, wantReady("https://clips.example.com", true))
}

func TestBuildMetaPosterDimsNotVideoDims(t *testing.T) {
	m := metaMap(t, BuildMeta(ClipView{Clip: readyClip(), URLs: builder(t, "http://h")}))
	if m["property og:image:width"] != "640" || m["property og:image:height"] != "360" {
		t.Fatalf("og:image dims = %s×%s, want the poster's 640×360", m["property og:image:width"], m["property og:image:height"])
	}
	if m["property og:video:width"] != "1280" || m["name twitter:player:width"] != "1280" {
		t.Fatalf("video/player dims must be the rendition's 1280×720: %v", m)
	}
}

// A ready row whose poster was never measured must omit og:image:width /
// og:image:height rather than borrow the rendition's — the spec §2 bug.
func TestBuildMetaNoPosterDimsOmitsImageDims(t *testing.T) {
	c := readyClip()
	c.PosterWidth, c.PosterHeight = nil, nil
	got := metaMap(t, BuildMeta(ClipView{Clip: c, URLs: builder(t, "https://clips.example.com")}))
	for _, k := range []string{"property og:image:width", "property og:image:height"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s = %q emitted for an unmeasured poster", k, v)
		}
	}
	want := wantReady("https://clips.example.com", true)
	delete(want, "property og:image:width")
	delete(want, "property og:image:height")
	assertExact(t, BuildMeta(ClipView{Clip: c, URLs: builder(t, "https://clips.example.com")}), want)
	// One missing half is treated the same: never a half pair.
	c.PosterWidth = ip(640)
	if _, ok := metaMap(t, BuildMeta(ClipView{Clip: c, URLs: builder(t, "https://h")}))["property og:image:width"]; ok {
		t.Error("og:image:width emitted without og:image:height")
	}
}

func TestBuildMetaDescriptionIsSingleLine(t *testing.T) {
	c := readyClip()
	c.Description = "  first line\n\tsecond\r\n\n  third   line "
	m := metaMap(t, BuildMeta(ClipView{Clip: c, URLs: builder(t, "http://h")}))
	for _, k := range []string{"property og:description", "name twitter:description", "name description"} {
		if m[k] != "0:06 · first line second third line" {
			t.Errorf("%s = %q", k, m[k])
		}
	}
	c.Description = " \n\t "
	m = metaMap(t, BuildMeta(ClipView{Clip: c, URLs: builder(t, "http://h")}))
	// Whitespace-only words are no words: the duration stands alone (§20
	// erratum 287), with no empty part and no dangling separator.
	if m["property og:description"] != "0:06" {
		t.Errorf("whitespace-only description = %q, want the duration alone", m["property og:description"])
	}
}

// TestBuildMetaEmptyDescriptionNeverBlank: a card's description row is never
// empty. Since §20 erratum 287 a ready clip always has at least its duration to
// say, so §9.1's default sentence is what is left when a row has nothing at all --
// no words, no measured duration and no quality clause.
func TestBuildMetaEmptyDescriptionNeverBlank(t *testing.T) {
	c := readyClip()
	c.Description = ""
	m := metaMap(t, BuildMeta(ClipView{Clip: c, URLs: builder(t, "http://h")}))
	for _, k := range []string{"property og:description", "name twitter:description", "name description"} {
		if m[k] != "0:06" {
			t.Errorf("%s = %q, want the duration", k, m[k])
		}
	}
	c.DurationMS = nil
	m = metaMap(t, BuildMeta(ClipView{Clip: c, URLs: builder(t, "http://h")}))
	for _, k := range []string{"property og:description", "name twitter:description", "name description"} {
		if m[k] != defaultDescription {
			t.Errorf("%s = %q, want %q", k, m[k], defaultDescription)
		}
	}
}

func TestBuildMetaLinkOut(t *testing.T) {
	base := "http://example.test:8481"
	id := "aZ3kQ9xB2mLp"
	img := base + "/static/card-placeholder.png?v=b1"
	for _, tc := range []struct{ state, desc, alt string }{
		{db.ClipQueued, "Processing…", "The Clippet wordmark: this clip is still encoding"},
		{db.ClipProcessing, "Processing…", "The Clippet wordmark: this clip is still encoding"},
		{db.ClipFailed, "Processing failed", "The Clippet wordmark: this clip could not be encoded"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			reason := "ffmpeg exited 1"
			c := db.Clip{ID: id, Title: "Pending clip", Description: "will be ignored", State: tc.state, FailureReason: &reason,
				// Stale-looking media columns must not leak into a link-out card.
				Width: ip(1280), Height: ip(720), PosterWidth: ip(1280), PosterHeight: ip(720)}
			got := BuildMeta(ClipView{Clip: c, URLs: builder(t, base)})
			assertExact(t, got, map[string]string{
				"property og:title":        "Pending clip",
				"property og:description":  tc.desc,
				"property og:type":         "website",
				"property og:url":          base + "/" + id,
				"property og:site_name":    "example.test",
				"property og:image":        img,
				"property og:image:type":   "image/png",
				"property og:image:width":  "1200",
				"property og:image:height": "630",
				"property og:image:alt":    tc.alt,
				"name twitter:card":        "summary_large_image",
				"name twitter:title":       "Pending clip",
				"name twitter:description": tc.desc,
				"name twitter:image":       img,
				"name twitter:image:alt":   tc.alt,
				"name description":         tc.desc,
			})
			for _, m := range got {
				if strings.HasPrefix(m.Key, "og:video") || strings.HasPrefix(m.Key, "twitter:player") {
					t.Errorf("link-out card carries %s", m.Key)
				}
			}
		})
	}
}

func TestBuildMetaHTTPSLinkOutHasNoSecureURL(t *testing.T) {
	c := readyClip()
	c.State = db.ClipProcessing
	for _, m := range BuildMeta(ClipView{Clip: c, URLs: builder(t, "https://h")}) {
		if m.Key == "og:video:secure_url" {
			t.Fatal("secure_url on a link-out card")
		}
	}
}

func TestBuildMetaUnlistedIdentical(t *testing.T) {
	for _, state := range []string{db.ClipReady, db.ClipProcessing} {
		listed := readyClip()
		listed.State = state
		unlisted := listed
		unlisted.Unlisted = true
		b := builder(t, "https://h")
		a, c := metaMap(t, BuildMeta(ClipView{Clip: listed, URLs: b})), metaMap(t, BuildMeta(ClipView{Clip: unlisted, URLs: b}))
		if len(a) != len(c) {
			t.Fatalf("%s: unlisted changes the tag set", state)
		}
		for k, v := range a {
			if c[k] != v {
				t.Fatalf("%s: unlisted changes %s", state, k)
			}
		}
	}
}

func TestBuildMetaIgnoresViewer(t *testing.T) {
	b := builder(t, "https://h")
	anon := BuildMeta(ClipView{Clip: readyClip(), URLs: b})
	owner := BuildMeta(ClipView{Clip: readyClip(), URLs: b, Viewer: &db.User{ID: "u1"}, IsOwner: true})
	if len(anon) != len(owner) {
		t.Fatal("viewer changes the tag set")
	}
	for i := range anon {
		if anon[i] != owner[i] {
			t.Fatalf("viewer changes tag %d", i)
		}
	}
}

// ---- collection cards (§9.4, §17 #9) ------------------------------------

func collectionClip(id string, state string, poster bool) db.Clip {
	c := db.Clip{ID: id, OwnerID: "u1", Title: "clip " + id, State: state}
	if poster {
		key := "p"
		c.PosterKey, c.PosterWidth, c.PosterHeight = &key, ip(640), ip(360)
	}
	return c
}

// TestBuildCollectionMetaIsAlwaysLinkOut: a collection is several clips, so
// there is no single video to unfurl. The card is a link-out card in every
// case — never a player, never og:video, never twitter:player.
func TestBuildCollectionMetaIsAlwaysLinkOut(t *testing.T) {
	b := builder(t, "https://clips.example.com")
	col := db.Collection{ID: "CoLLeCtIoN12", OwnerID: "u1", Title: "Best of 2026", ClipCount: 2}
	v := CollectionView{
		Collection: col, URLs: b,
		Clips: []db.Clip{
			collectionClip("AAAAAAAAAAAA", db.ClipProcessing, false),
			collectionClip("BBBBBBBBBBBB", db.ClipReady, true),
			collectionClip("CCCCCCCCCCCC", db.ClipReady, true),
		},
	}
	assertExact(t, BuildCollectionMeta(v), map[string]string{
		"property og:title":       "Best of 2026",
		"property og:description": defaultDescription,
		"property og:type":        "website",
		"property og:url":         "https://clips.example.com/c/CoLLeCtIoN12",
		"property og:site_name":   "clips.example.com",
		// the FIRST ready member's poster, not the first member's
		"property og:image":        "https://clips.example.com/api/clips/BBBBBBBBBBBB/poster.jpg",
		"property og:image:type":   "image/jpeg",
		"property og:image:width":  "640",
		"property og:image:height": "360",
		"name twitter:card":        "summary_large_image",
		"name twitter:title":       "Best of 2026",
		"name twitter:description": defaultDescription,
		"name twitter:image":       "https://clips.example.com/api/clips/BBBBBBBBBBBB/poster.jpg",
		"name description":         defaultDescription,
	})
}

// TestBuildCollectionMetaWithoutAPicture: no ready member means no image, and
// the card degrades to `summary` rather than borrowing the CLIP placeholder,
// which says "Processing…" and would be a lie about a list.
func TestBuildCollectionMetaWithoutAPicture(t *testing.T) {
	b := builder(t, "https://clips.example.com")
	col := db.Collection{ID: "CoLLeCtIoN12", OwnerID: "u1", Title: "Nothing yet"}
	for _, clips := range [][]db.Clip{
		nil,
		{collectionClip("AAAAAAAAAAAA", db.ClipProcessing, false)},
		{collectionClip("AAAAAAAAAAAA", db.ClipFailed, false)},
		// a `ready` row whose poster column is somehow NULL is still not a
		// picture: the card omits the image rather than emitting a URL that
		// would 404 for a crawler.
		{collectionClip("AAAAAAAAAAAA", db.ClipReady, false)},
	} {
		got := metaMap(t, BuildCollectionMeta(CollectionView{Collection: col, URLs: b, Clips: clips}))
		if got["name twitter:card"] != "summary" {
			t.Errorf("%d clips: twitter:card = %q, want summary", len(clips), got["name twitter:card"])
		}
		for _, k := range []string{"property og:image", "property og:image:width", "name twitter:image"} {
			if v, ok := got[k]; ok {
				t.Errorf("%d clips: %s = %q, want absent", len(clips), k, v)
			}
		}
		if got["property og:type"] != "website" || got["property og:title"] != "Nothing yet" {
			t.Errorf("%d clips: %v", len(clips), got)
		}
	}
}

// TestBuildCollectionMetaHasNoPlayerTags is the anti-feature check, stated as
// absence: a collection card must never carry a video or a player.
func TestBuildCollectionMetaHasNoPlayerTags(t *testing.T) {
	b := builder(t, "https://clips.example.com")
	v := CollectionView{
		Collection: db.Collection{ID: "CoLLeCtIoN12", Title: "T"}, URLs: b,
		Clips: []db.Clip{collectionClip("BBBBBBBBBBBB", db.ClipReady, true)},
	}
	for _, m := range BuildCollectionMeta(v) {
		if strings.HasPrefix(m.Key, "og:video") || strings.HasPrefix(m.Key, "twitter:player") {
			t.Errorf("collection card carries %s", m.Key)
		}
	}
}

// TestBuildMetaVideoDimsDescribeTheSocialFile is §20 erratum 123: `og:video`
// points at `/social.mp4`, so `og:video:width/height` and the
// `twitter:player` dims must describe THAT file. With a social rendition on
// the row they are its measured dims; without one the route serves the main
// rendition and the tags say so.
func TestBuildMetaVideoDimsDescribeTheSocialFile(t *testing.T) {
	base := "https://clips.example.com"
	c := readyClip()
	c.SocialKey, c.SocialSize, c.SocialETag = sp("clips/x/renditions/social-abcd1234.mp4"), i64p(99), sp("se")
	c.SocialWidth, c.SocialHeight = ip(854), ip(480)

	want := wantReady(base, true)
	for _, k := range []string{"property og:video:width", "name twitter:player:width"} {
		want[k] = "854"
	}
	for _, k := range []string{"property og:video:height", "name twitter:player:height"} {
		want[k] = "480"
	}
	// …and the DESCRIPTION now states both qualities, because a card that plays a
	// smaller copy than the link leads to has to say so (§20 erratum 214).
	desc := want["property og:description"] + " · Preview 480p · Full 720p at this link"
	for _, k := range []string{"property og:description", "name twitter:description", "name description"} {
		want[k] = desc
	}
	assertExact(t, BuildMeta(ClipView{Clip: c, URLs: builder(t, base)}), want)

	// Half a pair is not a measurement: a row with only one of the two falls
	// back to the rendition's, which is what the route then serves — and with no
	// measured card shape there is no quality line either.
	c.SocialHeight = nil
	assertExact(t, BuildMeta(ClipView{Clip: c, URLs: builder(t, base)}), wantReady(base, true))
}

// TestCardDescriptionStatesBothQualities is §20 erratum 214's other half. The
// card PLAYS the Light copy and the link leads to something better, and a reader
// who is not told that concludes the product is a 720p product — the owner's
// call of 2026-09-08, the same decision the burned-in tag comes from. So the description
// carries both real numbers, and carries nothing extra when there is nothing to
// explain.
func TestCardDescriptionStatesBothQualities(t *testing.T) {
	base := "https://clips.example.com"
	// A 1440p60 upload the browser plays, with a 720p30 card copy: the shape this
	// whole feature exists for.
	c := readyClip()
	c.Description = ""
	c.Width, c.Height, c.FPSNum, c.FPSDen = ip(2560), ip(1440), ip(60), ip(1)
	c.OriginalPlayable = true
	c.SocialKey, c.SocialSize, c.SocialETag = sp("clips/x/renditions/social-abcd1234.mp4"), i64p(99), sp("se")
	c.SocialWidth, c.SocialHeight, c.SocialFPSNum, c.SocialFPSDen = ip(1280), ip(720), ip(30), ip(1)

	desc := descriptionOf(t, c, base)
	if want := "0:06 · Preview 720p30 · Original 1440p60 at this link"; desc != want {
		t.Fatalf("description = %q, want %q — with no words of the owner's the length and the numbers are the description", desc, want)
	}
	// The owner's own words sit between the duration and the clause: the length
	// is what a reader decides on (§20 erratum 287) and the clause is a footnote on
	// the file (erratum 214, whose ordering of those two is unchanged).
	c.Description = "Ace clutch"
	if got, want := descriptionOf(t, c, base), "0:06 · Ace clutch · Preview 720p30 · Original 1440p60 at this link"; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
	// A source no browser plays reads "Full", which is what the page calls it too.
	c.OriginalPlayable = false
	if got := descriptionOf(t, c, base); !strings.Contains(got, "Full 1440p60") {
		t.Fatalf("description = %q, want the Full tier named", got)
	}
	// A trimmed clip's upload is not what the link leads to either.
	c.OriginalPlayable, c.TrimStartMS = true, i64p(1000)
	if got := descriptionOf(t, c, base); !strings.Contains(got, "Full 1440p60") {
		t.Fatalf("trimmed: description = %q, want the Full tier named", got)
	}
	c.TrimStartMS = nil

	// NOTHING to explain, three ways: no card copy at all; a card copy of the
	// same shape as the clip (it got one for its SIZE); an unmeasured row.
	c.Description = "Ace clutch"
	noLine := c
	noLine.SocialKey = nil
	if got := descriptionOf(t, noLine, base); got != "0:06 · Ace clutch" {
		t.Errorf("no card copy: description = %q, want the duration and the owner's words", got)
	}
	same := c
	same.Width, same.Height, same.FPSNum, same.FPSDen = ip(1280), ip(720), ip(30), ip(1)
	if got := descriptionOf(t, same, base); got != "0:06 · Ace clutch" {
		t.Errorf("same shape: description = %q, want the duration and the owner's words", got)
	}
	unmeasured := c
	unmeasured.SocialWidth = nil
	if got := descriptionOf(t, unmeasured, base); got != "0:06 · Ace clutch" {
		t.Errorf("unmeasured: description = %q, want the duration and the owner's words", got)
	}
	// A clip that is not ready has no player card at all, so no quality line.
	pending := c
	pending.State = db.ClipProcessing
	if got := descriptionOf(t, pending, base); got != statusProcessing {
		t.Errorf("processing: description = %q, want the link-out card's own line", got)
	}
	// The tag INVENTORY is unchanged by any of this: values only.
	if n := len(BuildMeta(ClipView{Clip: c, URLs: builder(t, base)})); n != 28 {
		t.Errorf("the https card carries %d tags, want 28 — erratum 214 changes values, never the set", n)
	}
}

// descriptionOf reads the card's description back out of the rendered tags, and
// checks the three that must always agree.
func descriptionOf(t *testing.T, c db.Clip, base string) string {
	t.Helper()
	m := map[string]string{}
	for _, tag := range BuildMeta(ClipView{Clip: c, URLs: builder(t, base)}) {
		m[tag.Attr+" "+tag.Key] = tag.Content
	}
	og, tw, plain := m["property og:description"], m["name twitter:description"], m["name description"]
	if og != tw || og != plain {
		t.Fatalf("the three descriptions disagree: og=%q twitter=%q description=%q", og, tw, plain)
	}
	return og
}

func sp(s string) *string { return &s }

func i64p(v int64) *int64 { return &v }

// The §9.1 inventory grew by three in §20 erratum 103 and by one more in
// erratum 177, and the count is part of the contract: 27 tags on an http
// origin, 28 on https (23 and 24 before erratum 103, 26 and 27 between the
// two). wantReady is the golden, so this only has to pin the number the golden
// encodes -- a test that asserted the tags without the count would let one
// more in unnoticed.
func TestReadyInventoryIsTwentySevenTags(t *testing.T) {
	http := BuildMeta(ClipView{Clip: readyClip(), URLs: builder(t, "http://example.test:8481")})
	if len(http) != 27 {
		t.Errorf("http ready card = %d tags, want 27", len(http))
	}
	https := BuildMeta(ClipView{Clip: readyClip(), URLs: builder(t, "https://clips.example.com")})
	if len(https) != 28 {
		t.Errorf("https ready card = %d tags, want 28 (27 + og:video:secure_url)", len(https))
	}
}

// The two alt sentences describe the same picture, so they must BE the same
// sentence: a card that told Discord one thing about its poster and X another
// would be worse than the single tag erratum 103 shipped (§20 erratum 177).
func TestImageAltIsTheSameForBothCards(t *testing.T) {
	m := metaMap(t, BuildMeta(ClipView{Clip: readyClip(), URLs: builder(t, "https://clips.example.com")}))
	og, tw := m["property og:image:alt"], m["name twitter:image:alt"]
	if og == "" || og != tw {
		t.Fatalf("og:image:alt = %q, twitter:image:alt = %q; want one non-empty sentence in both", og, tw)
	}
	if !strings.Contains(og, "E2E std") {
		t.Fatalf("image alt %q does not name the clip", og)
	}
}

// og:video and og:video:url are the SAME OGP property, so they must carry the
// same URL -- and that URL is the stable social route, never video.mp4.
func TestBuildMetaVideoURLMatchesOGVideo(t *testing.T) {
	m := metaMap(t, BuildMeta(ClipView{Clip: readyClip(), URLs: builder(t, "https://clips.example.com")}))
	social := "https://clips.example.com/api/clips/aZ3kQ9xB2mLp/social.mp4"
	if m["property og:video"] != social || m["property og:video:url"] != social {
		t.Fatalf("og:video = %q, og:video:url = %q, want both %q", m["property og:video"], m["property og:video:url"], social)
	}
}

// video:duration is whole seconds, ROUNDED, and is omitted entirely for a row
// that has no duration: a `0` would be a claim about the clip.
func TestBuildMetaVideoDuration(t *testing.T) {
	for _, tc := range []struct {
		ms   int64
		want string
	}{{6200, "6"}, {5600, "6"}, {1, "0"}, {900, "1"}, {60000, "60"}} {
		c := readyClip()
		ms := tc.ms
		c.DurationMS = &ms
		got := metaMap(t, BuildMeta(ClipView{Clip: c, URLs: builder(t, "http://h")}))
		if got["property video:duration"] != tc.want {
			t.Errorf("%dms -> video:duration %q, want %q", tc.ms, got["property video:duration"], tc.want)
		}
	}
	for _, missing := range []*int64{nil, new(int64)} {
		c := readyClip()
		c.DurationMS = missing
		for _, m := range BuildMeta(ClipView{Clip: c, URLs: builder(t, "http://h")}) {
			if m.Key == "video:duration" {
				t.Errorf("video:duration = %q emitted for a row with no duration", m.Content)
			}
		}
	}
}

// The alt text names the clip, collapses whitespace (a title may hold a
// newline) and degrades to a description of the picture when there is no
// usable title at all.
func TestImageAlt(t *testing.T) {
	for _, tc := range []struct{ title, want string }{
		{"E2E std", "A still frame from the clip: E2E std"},
		{"  ace\n clutch ", "A still frame from the clip: ace clutch"},
		{"   ", "A still frame from a clip"},
		{"", "A still frame from a clip"},
	} {
		if got := imageAlt(tc.title); got != tc.want {
			t.Errorf("imageAlt(%q) = %q, want %q", tc.title, got, tc.want)
		}
	}
}

// The link-out card gained the READY card's two ALT tags in §20 erratum 294 and
// nothing else: og:video:url and video:duration stay absent, because a clip with
// no video to point at and no measured duration has nothing to put behind
// either, and a tag whose content would be invented is worse than no tag.
func TestLinkOutCardGainedOnlyItsAlts(t *testing.T) {
	c := readyClip()
	c.State = db.ClipQueued
	for _, m := range BuildMeta(ClipView{Clip: c, URLs: builder(t, "https://clips.example.com")}) {
		switch m.Key {
		case "og:video:url", "video:duration":
			t.Errorf("the link-out card emitted %s", m.Key)
		}
	}
}

// Every card BuildMeta renders, in every state, describes its picture to a
// reader who cannot see it (§20 erratum 294, carrying errata 103 and 177's one
// accessibility affordance to the card that had neither). The two tags are one
// sentence, as they are on the ready card: a card that told Discord one thing
// about its image and X another would be worse than the single tag.
func TestEveryCardCarriesBothAlts(t *testing.T) {
	for _, tc := range []struct{ state, want string }{
		{db.ClipQueued, "The Clippet wordmark: this clip is still encoding"},
		{db.ClipProcessing, "The Clippet wordmark: this clip is still encoding"},
		{db.ClipFailed, "The Clippet wordmark: this clip could not be encoded"},
		{db.ClipReady, "A still frame from the clip: E2E std"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			c := readyClip()
			c.State = tc.state
			for _, base := range []string{"http://example.test:8481", "https://clips.example.com"} {
				m := metaMap(t, BuildMeta(ClipView{Clip: c, URLs: builder(t, base)}))
				og, tw := m["property og:image:alt"], m["name twitter:image:alt"]
				if og != tc.want {
					t.Errorf("%s: og:image:alt = %q, want %q", base, og, tc.want)
				}
				if tw != og {
					t.Errorf("%s: twitter:image:alt = %q, want og:image:alt %q", base, tw, og)
				}
			}
		})
	}
}

// The link-out sentence names the PICTURE — which since §20 erratum 287(f) is
// the wordmark on the dark ground — and never the clip: the placeholder is not
// a frame from it, so claiming it was would be a lie about the picture, and the
// title is on the card already. The state it DOES carry is the one a reader came
// for, and it follows the description's own two lines rather than inventing a
// third for `queued`.
func TestPlaceholderAltNamesThePictureAndTheState(t *testing.T) {
	c := db.Clip{ID: "aZ3kQ9xB2mLp", Title: "Ace clutch", State: db.ClipProcessing}
	for _, state := range []string{db.ClipQueued, db.ClipProcessing, db.ClipFailed} {
		c.State = state
		m := metaMap(t, BuildMeta(ClipView{Clip: c, URLs: builder(t, "https://clips.example.com")}))
		alt := m["property og:image:alt"]
		if strings.Contains(alt, c.Title) {
			t.Errorf("%s: the alt %q names the clip, which is not what the picture shows", state, alt)
		}
		if !strings.Contains(alt, productName) {
			t.Errorf("%s: the alt %q does not name what the picture is", state, alt)
		}
		if strings.Contains(alt, "clips.example.com") {
			t.Errorf("%s: the alt %q carries the host", state, alt)
		}
	}
	if placeholderAlt(db.ClipQueued) != placeholderAlt(db.ClipProcessing) {
		t.Error("queued and processing say different things while their descriptions say the same")
	}
	if placeholderAlt(db.ClipFailed) == placeholderAlt(db.ClipProcessing) {
		t.Error("a failed clip is still described as encoding")
	}
}

// The link-out inventory is an EXACT set too, and its count is now 16 — the 14
// of every state before §20 erratum 294 plus the two alt tags. Unlike the ready
// card it does not move with the scheme: it carries no og:video:secure_url and
// no URL of its own beyond the placeholder.
func TestLinkOutInventoryIsSixteenTags(t *testing.T) {
	for _, state := range []string{db.ClipQueued, db.ClipProcessing, db.ClipFailed} {
		c := readyClip()
		c.State = state
		for _, base := range []string{"http://example.test:8481", "https://clips.example.com"} {
			if n := len(BuildMeta(ClipView{Clip: c, URLs: builder(t, base)})); n != 16 {
				t.Errorf("%s on %s: %d tags, want 16", state, base, n)
			}
		}
	}
}

// TestShortQualityRoundsANearWholeRate pins the card's quality clause: a rate
// within 0.1 of a whole number prints the whole number, never a decimal
// (a nit from lane compcard: `Original 1440p60.01`, `Preview 674p29.97`).
func TestShortQualityRoundsANearWholeRate(t *testing.T) {
	for _, tc := range []struct {
		num, den int
		want     string
	}{
		{6001, 100, "1440p60"},
		{30000, 1001, "1440p30"},
		{24000, 1001, "1440p24"},
		{60, 1, "1440p60"},
		{5, 2, "1440p2.5"}, // not near a whole number: stays as it was
	} {
		w, h, n, d := 2560, 1440, tc.num, tc.den
		if got := shortQuality(&w, &h, &n, &d); got != tc.want {
			t.Errorf("shortQuality at %d/%d = %q, want %q", tc.num, tc.den, got, tc.want)
		}
	}
}
