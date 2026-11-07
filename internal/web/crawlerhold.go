package web

import (
	"net/http"
	"strings"

	"clippet/internal/db"
	"clippet/internal/httpx"
)

// The crawler hold (DESIGN.md §9.1, §20 erratum 213).
//
// A link-preview crawler fetches a URL ONCE and caches what it extracted, per
// URL, for its own lifetime — the measured fact CAPTURE_READY_WAIT exists for
// (§20 erratum 97). So the worst possible answer to a crawler asking about a clip
// that is still encoding is a 200 carrying the link-out card: it is correct at
// that instant, it is wrong a minute later, and the crawler will not come back to
// find out. The pasted link then stays a bare link for as long as that cache
// lives, whatever the clip becomes.
//
// The answer is the one §9.1 already gives for a backend outage, for exactly the
// same reason — "so a bad first fetch is not remembered": **503 with Retry-After
// and no card tags at all**. A crawler that honours Retry-After comes back; one
// that does not has cached nothing. Humans are unaffected and get the processing
// page they always got.

// crawlerRetryAfter is the hold's Retry-After, in seconds — and it is literally
// the outage page's, not a second 30 that happens to agree. Long enough that a
// crawler's retry is not a poll, short enough that a clip whose encode takes a
// minute is picked up on the second look; §9.1 chose it for the outage this hold
// borrows its whole doctrine from, and the two moving apart would mean nothing.
const crawlerRetryAfter = retryAfterSeconds

// linkPreviewCrawlers is the User-Agent table: the unfurlers this product exists
// to feed, which are the same four /robots.txt names them individually plus
// Telegram's.
//
// Each entry is a lower-cased SUBSTRING of the agent's own token, because every
// one of these appends a version and a URL of its own ("Discordbot/2.0
// (+…)") and two of them change that suffix between releases. The tokens
// themselves have been stable for years.
//
// It is a TABLE and not a regexp so that the list is greppable, testable, and
// obviously the whole of the rule: anything not named here is a human as far as
// this hold is concerned, which is the safe direction — a missed crawler gets
// today's behaviour, and a human never gets a 503.
var linkPreviewCrawlers = []string{
	"discordbot",          // Discord
	"twitterbot",          // X
	"facebookexternalhit", // Facebook, Instagram, WhatsApp
	"slackbot",            // Slack (both Slackbot-LinkExpanding and Slack-ImgProxy's parent)
	"telegrambot",         // Telegram
}

// isLinkPreviewCrawler reports whether this User-Agent is one of the unfurlers.
func isLinkPreviewCrawler(ua string) bool {
	if ua == "" {
		return false
	}
	lower := strings.ToLower(ua)
	for _, name := range linkPreviewCrawlers {
		if strings.Contains(lower, name) {
			return true
		}
	}
	return false
}

// holdCrawler reports whether this request must be held, and is the whole of the
// rule: a link-preview crawler asking about a clip whose card is still coming.
//
// A FAILED clip is never held (db.CardPending): its link-out card is the card it
// will have for ever, and withholding that would leave the paste with no card at
// all. A READY clip is never held either — since erratum 206 `ready` means
// card-ready, so there is nothing to wait for.
func holdCrawler(r *http.Request, c db.Clip) bool {
	return db.CardPending(c) && isLinkPreviewCrawler(r.UserAgent())
}

// cardNoteText is the one line a human gets while the card is still coming (§20
// erratum 213).
//
// It says three things and stops: WHAT is happening, WHERE (the owner's question
// of 2026-09-08: what processing is and where it happens), and the consequence
// that matters — the link is not worth pasting yet. No countdown and no
// estimate: an estimate is too often a bit of a lie.
const cardNoteText = "Encoding on the server — the Discord card is not ready yet."

// cardNote is that line, or "" when there is nothing to say: a card-ready clip
// has its card, and a failed one has its own line on the page already.
func cardNote(c db.Clip) string {
	if db.CardPending(c) {
		return cardNoteText
	}
	return ""
}

// crawlerHold writes that answer: 503, Retry-After, no-store, and the same body
// the outage page uses — which is exactly why it is reused, since it carries not
// one `og:` tag for a crawler to remember.
func (s *server) crawlerHold(w http.ResponseWriter, r *http.Request, c db.Clip) {
	httpx.SetLogField(r, "crawler_hold", c.State)
	p := s.basePage(r, "Still encoding · "+productName)
	p.NoIndex = true
	h := w.Header()
	h.Set("Retry-After", crawlerRetryAfter)
	h.Set("Cache-Control", "no-store")
	s.render(w, r, "unavailable", http.StatusServiceUnavailable, p)
}
