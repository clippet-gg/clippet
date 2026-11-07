package web

// The unfurl regression gate (medal #7, wired into scripts/check.sh via the
// test image's `go test -tags ffmpeg ./...` step): the RENDERED share page
// must carry the full §9.1 tag inventory with PUBLIC_BASE_URL-absolute
// URLs, og:video:secure_url present iff the base is https, and a non-ready
// (link-out) clip must carry zero og:video* / twitter:player* tags. The
// social.mp4 half of the gate (200 + 206) lives in
// internal/httpapi.TestUnfurlRegressionSocialMP4.

import (
	"strings"
	"testing"
)

// unfurlTags is the §9.1 ready-clip checklist as it must appear in the
// rendered <head> for the fixture clip (title "E2E std", description
// "a <b>desc</b>" — HTML-escaped by the template — poster 640×360,
// rendition 1280×720).
func unfurlTags(base, id string, https bool) []string {
	poster := base + "/api/clips/" + id + "/poster.jpg"
	social := base + "/api/clips/" + id + "/social.mp4"
	// `{m:ss} · {the owner's words}` since §20 erratum 287; the fixture clip is 6 s
	// and has no Light copy, so there is no quality clause on this card.
	desc := "0:06 · a &lt;b&gt;desc&lt;/b&gt;"
	tags := []string{
		`<meta property="og:title" content="E2E std">`,
		`<meta property="og:description" content="` + desc + `">`,
		`<meta property="og:type" content="video.other">`,
		`<meta property="og:url" content="` + base + `/` + id + `">`,
		`<meta property="og:site_name" content="` + builderHost(base) + `">`,
		`<meta property="og:image" content="` + poster + `">`,
		`<meta property="og:image:type" content="image/jpeg">`,
		`<meta property="og:image:width" content="640">`,
		`<meta property="og:image:height" content="360">`,
		`<meta property="og:video" content="` + social + `">`,
		`<meta property="og:video:type" content="video/mp4">`,
		`<meta property="og:video:width" content="1280">`,
		`<meta property="og:video:height" content="720">`,
		`<meta name="twitter:card" content="player">`,
		`<meta name="twitter:title" content="E2E std">`,
		`<meta name="twitter:description" content="` + desc + `">`,
		`<meta name="twitter:image" content="` + poster + `">`,
		`<meta name="twitter:player" content="` + base + `/embed/` + id + `">`,
		`<meta name="twitter:player:width" content="1280">`,
		`<meta name="twitter:player:height" content="720">`,
		`<meta name="twitter:player:stream" content="` + social + `">`,
		`<meta name="twitter:player:stream:content_type" content="video/mp4">`,
		`<meta name="description" content="` + desc + `">`,
		`<link rel="canonical" href="` + base + `/` + id + `">`,
	}
	if https {
		tags = append(tags, `<meta property="og:video:secure_url" content="`+social+`">`)
	}
	return tags
}

func TestUnfurlRegression(t *testing.T) {
	f := newFixture(t)
	id := f.ready.ID

	for name, tc := range map[string]struct {
		base  string
		https bool
	}{
		"http":  {testBase, false},
		"https": {"https://clips.example.com", true},
	} {
		t.Run(name, func(t *testing.T) {
			rec := get(f.handlerBase(passAuth{}, tc.base), "/"+id)
			if rec.Code != 200 {
				t.Fatalf("share page: %d", rec.Code)
			}
			head, _, ok := strings.Cut(rec.Body.String(), "</head>")
			if !ok {
				t.Fatal("no </head> in the rendered page")
			}
			for _, tag := range unfurlTags(tc.base, id, tc.https) {
				if !strings.Contains(head, tag) {
					t.Errorf("head lacks %s", tag)
				}
			}
			if !tc.https && strings.Contains(head, "og:video:secure_url") {
				t.Error("og:video:secure_url emitted on an http base (violates the OG spec)")
			}
			// Every card URL is absolute — nothing in the <head> points at a
			// bare same-origin path (unfurlers resolve nothing).
			for _, frag := range []string{`content="/api/`, `content="/`, `content="/embed/`, `content="/static/`} {
				if strings.Contains(head, frag) {
					t.Errorf("relative card URL in head: %s", frag)
				}
			}
		})
	}

	// Link-out states carry the placeholder card and ZERO video/player tags.
	for _, c := range []struct{ id, state string }{
		{f.processing.ID, "processing"},
		{f.queued.ID, "queued"},
		{f.failed.ID, "failed"},
	} {
		t.Run("linkout "+c.state, func(t *testing.T) {
			rec := get(f.handler(passAuth{}), "/"+c.id)
			if rec.Code != 200 {
				t.Fatalf("share page: %d", rec.Code)
			}
			body := rec.Body.String()
			mustNotContain(t, body, `og:video`, `twitter:player`)
			mustContain(t, body,
				`<meta property="og:type" content="website">`,
				`<meta name="twitter:card" content="summary_large_image">`,
				`<meta property="og:image" content="`+testBase+`/static/card-placeholder.png?v=b1">`,
				`<meta property="og:image:type" content="image/png">`,
				`<meta property="og:image:width" content="1200">`,
				`<meta property="og:image:height" content="630">`,
			)
		})
	}
}
