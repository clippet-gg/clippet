package autobot

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Discord's REST manners (Discord's "Rate Limits" page): every route belongs
// to a bucket the answer names (X-RateLimit-Bucket); a bucket with
// X-RateLimit-Remaining 0 is not called again before X-RateLimit-Reset-After;
// a 429 says how long to wait (`retry_after`, seconds) and whether the limit
// is global. This file keeps those — per route key (the method, the path and
// its MAJOR parameter: the channel, or the interaction/webhook) — and waits
// rather than hammering: a 429 is waited out once and the call tried once
// more; a second 429, or a wait longer than maxRateWait, gives the 429 back.

// maxRateWait is the longest this client sleeps for a rate limit before it
// gives the answer back instead: a Prompt reply lives 5-60 s.
const maxRateWait = 60 * time.Second

type bucket struct {
	remaining int
	resetAt   time.Time
}

type rest struct {
	call  Caller
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	mu          sync.Mutex
	buckets     map[string]*bucket // bucket hash → state
	routes      map[string]string  // route key → bucket hash
	globalUntil time.Time
	lastPrune   time.Time

	onUnauthorized func()
	onRateLimited  func()
}

// do makes one call under the manners. key is the route key.
func (r *rest) do(ctx context.Context, method, path, key string, body any, auth bool) (Response, error) {
	var raw []byte
	if body != nil {
		raw = mustJSON(body)
	}
	for attempt := 0; ; attempt++ {
		if err := r.waitFor(ctx, key); err != nil {
			return Response{}, err
		}
		resp, err := r.call(ctx, method, path, raw, auth)
		if err != nil {
			return resp, err
		}
		r.learn(key, resp.Header)
		if resp.Status == http.StatusUnauthorized && auth && r.onUnauthorized != nil {
			// The bot token itself is refused: stop everything, never loop.
			r.onUnauthorized()
		}
		if resp.Status != http.StatusTooManyRequests {
			return resp, nil
		}
		if r.onRateLimited != nil {
			r.onRateLimited()
		}
		wait, global := retryAfter(resp)
		r.mu.Lock()
		until := r.now().Add(wait)
		if global {
			r.globalUntil = until
		} else if h := r.routes[key]; h != "" {
			r.buckets[h] = &bucket{remaining: 0, resetAt: until}
		} else {
			r.routes[key] = "route:" + key
			r.buckets["route:"+key] = &bucket{remaining: 0, resetAt: until}
		}
		r.mu.Unlock()
		if attempt > 0 || wait > maxRateWait {
			return resp, nil
		}
	}
}

// waitFor sleeps out a global limit and the route's exhausted bucket.
func (r *rest) waitFor(ctx context.Context, key string) error {
	r.mu.Lock()
	now := r.now()
	var wait time.Duration
	if r.globalUntil.After(now) {
		wait = r.globalUntil.Sub(now)
	}
	if b := r.buckets[r.routes[key]]; b != nil && b.remaining <= 0 && b.resetAt.After(now) {
		wait = max(wait, b.resetAt.Sub(now))
	}
	r.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	if wait > maxRateWait {
		wait = maxRateWait
	}
	return r.sleep(ctx, wait)
}

// learn records the bucket a route answered from.
func (r *rest) learn(key string, h http.Header) {
	hash := h.Get("X-RateLimit-Bucket")
	rem := h.Get("X-RateLimit-Remaining")
	after := h.Get("X-RateLimit-Reset-After")
	if hash == "" || rem == "" || after == "" {
		return
	}
	n, err1 := strconv.Atoi(rem)
	secs, err2 := strconv.ParseFloat(after, 64)
	if err1 != nil || err2 != nil || secs < 0 {
		return
	}
	r.mu.Lock()
	now := r.now()
	r.prune(now)
	r.routes[key] = hash
	r.buckets[hash] = &bucket{remaining: n, resetAt: now.Add(time.Duration(secs * float64(time.Second)))}
	r.mu.Unlock()
}

// pruneAbove is the route count past which learn sweeps out what no longer
// constrains a call, at most once per pruneEvery: the keys are per channel,
// and a long-lived process touches channels without end.
const (
	pruneAbove = 256
	pruneEvery = time.Minute
)

// prune drops every bucket whose reset has passed — waitFor ignores those
// already — and every route that points to no bucket. Under r.mu.
func (r *rest) prune(now time.Time) {
	if len(r.routes) <= pruneAbove || now.Sub(r.lastPrune) < pruneEvery {
		return
	}
	r.lastPrune = now
	for h, b := range r.buckets {
		if !b.resetAt.After(now) {
			delete(r.buckets, h)
		}
	}
	for k, h := range r.routes {
		if r.buckets[h] == nil {
			delete(r.routes, k)
		}
	}
}

// retryAfter reads a 429: the body's retry_after and global, else the
// Retry-After header; one second when neither says.
func retryAfter(resp Response) (time.Duration, bool) {
	var b struct {
		RetryAfter float64 `json:"retry_after"`
		Global     bool    `json:"global"`
	}
	global := resp.Header.Get("X-RateLimit-Global") == "true" || resp.Header.Get("X-RateLimit-Scope") == "global"
	if json.Unmarshal(resp.Body, &b) == nil && b.RetryAfter > 0 {
		return time.Duration(b.RetryAfter * float64(time.Second)), global || b.Global
	}
	if s, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil && s > 0 {
		return time.Duration(s * float64(time.Second)), global
	}
	return time.Second, global
}

func ok2xx(r Response) bool { return r.Status >= 200 && r.Status < 300 }

// ---------------------------------------------------------------------------
// The calls, and nothing else. Every channel message this package posts says
// `allowed_mentions: {parse: []}`: it never pings anybody.

var noMentions = map[string]any{"parse": []string{}}

// gatewayBot is GET /gateway/bot.
func (a *Auto) gatewayBot(ctx context.Context) (gatewayInfo, error) {
	resp, err := a.rest.do(ctx, http.MethodGet, "/gateway/bot", "GET /gateway/bot", nil, true)
	if err != nil {
		return gatewayInfo{}, err
	}
	if !ok2xx(resp) {
		return gatewayInfo{}, statusError(resp.Status)
	}
	var g gatewayInfo
	if err := json.Unmarshal(resp.Body, &g); err != nil || g.URL == "" {
		return gatewayInfo{}, errMalformed
	}
	return g, nil
}

// channelMessages is GET /channels/{id}/messages?after=…&limit=… (the
// backfill, §20 erratum 334): the channel's messages after a snowflake,
// newest first, at most limit.
func (a *Auto) channelMessages(ctx context.Context, channel, after string, limit int) (Response, error) {
	if !validSnowflake(channel) || !validSnowflake(after) {
		return Response{}, errBadPathPart
	}
	return a.rest.do(ctx, http.MethodGet, "/channels/"+channel+"/messages?after="+after+"&limit="+strconv.Itoa(limit),
		"GET /channels/"+channel+"/messages", nil, true)
}

type gatewayInfo struct {
	URL               string `json:"url"`
	SessionStartLimit struct {
		Total      int   `json:"total"`
		Remaining  int   `json:"remaining"`
		ResetAfter int64 `json:"reset_after"` // milliseconds
	} `json:"session_start_limit"`
}

// Every id that goes into a REST path is checked first (defence in depth: they
// are Discord's own, but a path is built from them): a snowflake, and an
// interaction token with nothing in it that could end or bend a path.
const errBadPathPart = constError("an id in the path is malformed")

func validToken(s string) bool {
	if len(s) < 1 || len(s) > 1024 || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == '/' || c == '\\' || c == '?' || c == '#' || c == '%' {
			return false
		}
	}
	return true
}

func (a *Auto) createMessage(ctx context.Context, channel string, body map[string]any) (Response, error) {
	if !validSnowflake(channel) {
		return Response{}, errBadPathPart
	}
	return a.rest.do(ctx, http.MethodPost, "/channels/"+channel+"/messages", "POST /channels/"+channel+"/messages", body, true)
}

func (a *Auto) deleteMessage(ctx context.Context, channel, message string) (Response, error) {
	if !validSnowflake(channel) || !validSnowflake(message) {
		return Response{}, errBadPathPart
	}
	return a.rest.do(ctx, http.MethodDelete, "/channels/"+channel+"/messages/"+message,
		"DELETE /channels/"+channel+"/messages/:id", nil, true)
}

// callback answers an interaction (no token: the interaction token is the
// credential, and it is in the path).
func (a *Auto) callback(ctx context.Context, id, token string, body map[string]any) (Response, error) {
	if !validSnowflake(id) || !validToken(token) {
		return Response{}, errBadPathPart
	}
	return a.rest.do(ctx, http.MethodPost, "/interactions/"+id+"/"+token+"/callback",
		"POST /interactions/:id/:token/callback", body, false)
}

// followup posts a follow-up message on an interaction (no token either).
func (a *Auto) followup(ctx context.Context, appID, token string, body map[string]any) (Response, error) {
	if !validSnowflake(appID) || !validToken(token) {
		return Response{}, errBadPathPart
	}
	return a.rest.do(ctx, http.MethodPost, "/webhooks/"+appID+"/"+token, "POST /webhooks/"+appID+"/:token", body, false)
}

type statusError int

func (e statusError) Error() string { return "discord answered " + strconv.Itoa(int(e)) }

type constError string

func (e constError) Error() string { return string(e) }

const errMalformed = constError("discord's answer could not be read")
