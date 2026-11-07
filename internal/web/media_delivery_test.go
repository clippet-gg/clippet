package web

import (
	"context"
	"strings"
	"testing"

	"clippet/internal/config"
	"clippet/internal/cost"
	"clippet/internal/httpx"
)

// MEDIA_DELIVERY=redirect on the pages (§20 erratum 278). Two things change and
// nothing else: the served figure says it is an estimate, and the CSP names the
// origin a media route now redirects to.

// TestServedRowSaysEstimatedInRedirectMode: bytes that leave the bucket never
// pass this app, so the counter is charged each redirect's whole object. That
// is an UPPER bound, and erratum 270's law — a number nobody measured is absent
// rather than a zero — means a number measured DIFFERENTLY has to say so where
// it is read.
func TestServedRowSaysEstimatedInRedirectMode(t *testing.T) {
	f, _ := countingFixture(t)
	f.budget = &httpx.StorageBudget{}
	f.budget.Set(25_585_254, 0)
	if err := f.db.AddBytesServed(context.Background(), cost.Day(fixtureNow(f)), 1_288_490_189); err != nil {
		t.Fatal(err)
	}

	// proxy (the shipped default): no qualifier at all — the app wrote every
	// byte it counted.
	panel := panelOf(t, get(f.handler(session(f.owner)), "/settings").Body.String())
	if strings.Contains(panel, "estimated") {
		t.Errorf("proxy mode qualifies the served figure:\n%s", panel)
	}

	f.mediaDelivery = config.DeliveryRedirect
	f.mediaPresignEndpoint = "https://clips.example.test"
	panel = panelOf(t, get(f.handler(session(f.owner)), "/settings").Body.String())
	if !strings.Contains(panel, "estimated (redirect)") {
		t.Fatalf("the served row does not say the figure is an estimate:\n%s", panel)
	}
	// It qualifies the FIGURE, not the money: it sits in the control cell
	// beside the byte count, the way the quota row's "of" does.
	row := panel[strings.Index(panel, ">served this month<"):]
	if j := strings.Index(row, "</div>"); j > 0 {
		row = row[:j]
	}
	if !strings.Contains(row, "1.2 GiB</span> estimated (redirect)") {
		t.Errorf("the qualifier is not beside the figure:\n%s", row)
	}
	// The stored row is a different source and is untouched: it comes from the
	// database, not from anything a viewer did.
	stored := panel[strings.Index(panel, ">stored<"):strings.Index(panel, ">served this month<")]
	if strings.Contains(stored, "estimated") {
		t.Errorf("the stored row was qualified too:\n%s", stored)
	}
}

// TestPagesNameTheMediaOriginInCSP: CSP re-checks a redirect TARGET by host, so
// the watch page's own `<video>` would be blocked — silently — unless the
// bucket's origin is in media-src and img-src. Both the ordinary pages and the
// embed carry it, and neither carries it in `proxy` mode.
func TestPagesNameTheMediaOriginInCSP(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/" + f.ready.ID, "/embed/" + f.ready.ID} {
		if csp := get(f.handler(passAuth{}), path).Header().Get("Content-Security-Policy"); strings.Contains(csp, "clips.example.test") {
			t.Errorf("proxy mode names a media origin on %s: %s", path, csp)
		}
	}
	f.mediaDelivery = config.DeliveryRedirect
	f.mediaPresignEndpoint = "https://clips.example.test"
	for _, path := range []string{"/" + f.ready.ID, "/embed/" + f.ready.ID} {
		csp := get(f.handler(passAuth{}), path).Header().Get("Content-Security-Policy")
		if n := strings.Count(csp, "https://clips.example.test"); n != 2 {
			t.Errorf("%s: the media origin appears %d times, want 2 (media-src and img-src): %s", path, n, csp)
		}
		if !strings.Contains(csp, "script-src 'self';") || strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s: redirect mode widened more than the media: %s", path, csp)
		}
	}
	// The page's own markup is unchanged: the card and the player still point
	// at this app's stable URLs, and the redirect happens behind them.
	body := get(f.handler(passAuth{}), "/"+f.ready.ID).Body.String()
	if strings.Contains(body, "clips.example.test") {
		t.Error("a bucket origin reached the page's markup; the URLs must not move")
	}
	if !strings.Contains(body, "/api/clips/"+f.ready.ID+"/video.mp4") {
		t.Error("the player lost its stable src")
	}
}
