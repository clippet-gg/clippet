package httpapi

import (
	"net/http"
	"testing"
)

// GET /api/bot/claim has a bucket of its OWN (§20 erratum 279(h)): it is
// anonymous and public, and the interactions route's `bot` bucket is what the
// official bot's `/clip`s spend — behind a fixed proxy sentinel, ONE bucket for
// every connected server on the box. Neither may drain the other. Every
// request here comes from the same client address (the test transport's
// RemoteAddr), which is exactly the shared-key case the rule exists for.

func TestBotClaimHasItsOwnBucket(t *testing.T) {
	t.Run("draining the claim bucket leaves an interaction admitted", func(t *testing.T) {
		b := newBotEnv(t)
		path := botClaimPath + "?guild_id=222222222222222222"
		for i := 0; i < claimsPerMinute; i++ {
			if r := b.do(http.MethodGet, path, nil); r.code != http.StatusNotFound {
				t.Fatalf("claim GET %d: %d %s (want 404 unbound)", i, r.code, r.body)
			}
		}
		r := b.do(http.MethodGet, path, nil)
		if r.code != http.StatusTooManyRequests || r.errCode() != "rate_limited" || r.hdr.Get("Retry-After") == "" {
			t.Fatalf("claim GET %d: %d %s, want 429 rate_limited with Retry-After", claimsPerMinute+1, r.code, r.body)
		}
		// The interactions route still has every one of its tokens.
		if p := b.post(`{"type":1}`); p.code != http.StatusOK || p.json["type"] != float64(1) {
			t.Fatalf("a signed PING after the claim bucket drained: %d %s (the claim endpoint spent the bot bucket)", p.code, p.body)
		}
	})

	t.Run("draining the interactions bucket leaves the claim endpoint answering", func(t *testing.T) {
		b := newBotEnv(t)
		for i := 0; i < interactionsPerMinute; i++ {
			if p := b.post(`{"type":1}`); p.code != http.StatusOK {
				t.Fatalf("PING %d: %d %s", i, p.code, p.body)
			}
		}
		if p := b.post(`{"type":1}`); p.code != http.StatusTooManyRequests || p.errCode() != "rate_limited" {
			t.Fatalf("PING %d: %d %s, want 429 rate_limited", interactionsPerMinute+1, p.code, p.body)
		}
		// The claim endpoint still answers — the bound guild with the boolean,
		// an unbound one with its empty 404 — from its own bucket.
		b.boundGuild(t, "owner", "222222222222222222")
		if r := b.do(http.MethodGet, botClaimPath+"?guild_id=222222222222222222", nil); r.code != http.StatusOK || r.json["bound"] != true {
			t.Fatalf("claim GET after the bot bucket drained: %d %s (the interactions route spent the claim bucket)", r.code, r.body)
		}
		if r := b.do(http.MethodGet, botClaimPath+"?guild_id=333333333333333333", nil); r.code != http.StatusNotFound {
			t.Fatalf("unbound claim GET after the bot bucket drained: %d %s", r.code, r.body)
		}
	})
}

// TestClaimBucketIsSmallerThanTheBotBucket pins the arithmetic the docs quote:
// the claim endpoint's ceiling is its own number and below the interactions
// route's, so a public GET can never be the larger spender on a box.
func TestClaimBucketIsSmallerThanTheBotBucket(t *testing.T) {
	if claimsPerMinute >= interactionsPerMinute {
		t.Fatalf("claimsPerMinute = %d, interactionsPerMinute = %d: the anonymous endpoint must have the smaller budget", claimsPerMinute, interactionsPerMinute)
	}
	if claimsPerMinute != 30 {
		t.Fatalf("claimsPerMinute = %d; API.md and DEPLOY §30.8 say 30", claimsPerMinute)
	}
}
