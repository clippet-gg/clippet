package web

import (
	"net/http"
	"strings"
	"testing"

	"clippet/internal/db"
)

// The crawler hold (§20 erratum 213). Two claims, and both of them are about
// what is NOT in the answer: a crawler asking about a clip that has no card yet
// gets 503 and not one `og:` tag, and a human asking the same question gets
// exactly the page they always got.

// TestCrawlerTable is the whole rule, agent by agent. It is a table because the
// list IS the feature: a name missing from it means today's behaviour (a 200 a
// crawler then caches), and a name that should not be there means a 503 for a
// person.
func TestCrawlerTable(t *testing.T) {
	for _, ua := range []string{
		"Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)",
		"Discordbot/2.0",
		"discordbot/2.0",
		"Twitterbot/1.0",
		"facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)",
		"Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)",
		"Slackbot 1.0 (+https://api.slack.com/robots)",
		"TelegramBot (like TwitterBot)",
	} {
		if !isLinkPreviewCrawler(ua) {
			t.Errorf("%q is not recognised as a link-preview crawler", ua)
		}
	}
	for _, ua := range []string{
		"",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Version/18.0 Mobile/15E148 Safari/604.1",
		"curl/8.11.0",
		"Googlebot/2.1 (+http://www.google.com/bot.html)", // a search crawler, not an unfurler
		"Clippet-Notifier/1.0",
		"discord",     // the app, not the bot
		"twitter/1.0", // the app, not the bot
	} {
		if isLinkPreviewCrawler(ua) {
			t.Errorf("%q is treated as a link-preview crawler", ua)
		}
	}
}

// TestHoldCrawlerOnlyWhileTheCardIsComing pins the OTHER half of the predicate,
// as a table over the clip states. The two that must never be held are the ones
// a regression would break silently: a ready clip (the card works) and a failed
// one (the link-out card is the card it will have for ever).
func TestHoldCrawlerOnlyWhileTheCardIsComing(t *testing.T) {
	f := newFixture(t)
	bot := "Discordbot/2.0"
	for _, tc := range []struct {
		name string
		clip db.Clip
		hold bool
	}{
		{name: "queued", clip: f.queued, hold: true},
		{name: "processing", clip: f.processing, hold: true},
		{name: "ready", clip: f.ready},
		{name: "failed", clip: f.failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httpRequestWithUA(bot)
			if got := holdCrawler(r, tc.clip); got != tc.hold {
				t.Fatalf("holdCrawler(%s) = %v, want %v", tc.clip.State, got, tc.hold)
			}
			// A human is never held, in any state.
			if holdCrawler(httpRequestWithUA("Mozilla/5.0"), tc.clip) {
				t.Fatalf("a human was held on a %s clip", tc.clip.State)
			}
		})
	}
}

func httpRequestWithUA(ua string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("User-Agent", ua)
	return r
}

// TestCrawlerHoldOverHTTP is the answer itself, through the real handler: 503,
// Retry-After: 30, no-store, and — the point of the whole erratum — NOT ONE card
// tag for a crawler to remember.
func TestCrawlerHoldOverHTTP(t *testing.T) {
	f := newFixture(t)
	h := f.handler(passAuth{})
	rec := do(h, http.MethodGet, "/"+f.queued.ID, "User-Agent", "Discordbot/2.0")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a crawler got %d for a queued clip, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != crawlerRetryAfter {
		t.Errorf("Retry-After = %q, want %q", got, crawlerRetryAfter)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"og:", "twitter:", f.queued.Title} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the hold page carries %q — a crawler must have nothing to cache:\n%s", forbidden, body)
		}
	}
	if !strings.Contains(body, robotsMeta) {
		t.Error("the hold page is not noindex")
	}

	// A HUMAN gets the processing page, with its card tags, exactly as before.
	human := do(h, http.MethodGet, "/"+f.queued.ID, "User-Agent",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/140.0 Safari/537.36")
	if human.Code != http.StatusOK {
		t.Fatalf("a human got %d for a queued clip, want 200", human.Code)
	}
	mustContain(t, human.Body.String(),
		`<meta property="og:title" content="Just uploaded">`,
		// …and the one short line erratum 213 asks for.
		`data-card-note`, cardNoteText)

	// And a crawler asking about a READY clip gets the player card it came for.
	ready := do(h, http.MethodGet, "/"+f.ready.ID, "User-Agent", "Discordbot/2.0")
	if ready.Code != http.StatusOK {
		t.Fatalf("a crawler got %d for a ready clip, want 200", ready.Code)
	}
	mustContain(t, ready.Body.String(), `name="twitter:card" content="player"`)
	if strings.Contains(ready.Body.String(), "data-card-note") {
		t.Error("a card-ready clip still carries the not-ready line")
	}

	// A FAILED clip is never held: its link-out card is the only card it will
	// ever get, and it must not carry the "still encoding" line either.
	failed := do(h, http.MethodGet, "/"+f.failed.ID, "User-Agent", "Discordbot/2.0")
	if failed.Code != http.StatusOK {
		t.Fatalf("a crawler got %d for a failed clip, want 200 with the link-out card", failed.Code)
	}
	mustContain(t, failed.Body.String(), `name="twitter:card" content="summary_large_image"`)
	if strings.Contains(failed.Body.String(), "data-card-note") {
		t.Error("a failed clip carries the not-ready line; it has its own")
	}
}
