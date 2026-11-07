package autobot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// The hosted process's half of this package (§20 erratum 334, cmd/clippet-auto):
// the session kept off the machine, and the backfill after a fresh IDENTIFY.
// Neither runs on the box (no SessionStore, Backfill false there).
//
//   - The session (`session_id`, `seq`, `resume_gateway_url` and the newest
//     message id acted on) is written to the SessionStore when it changes in a
//     way that matters — READY, a session forgotten, an applicable video seen —
//     at most once per SaveEvery (by the loop, or as an act completes, off the
//     server's upload queue), by Shutdown FIRST (synchronously, whatever the
//     debounce) and again after the jobs only when it changed, and once more as
//     Run ends when it differs from the last saved (the latest seq) — so a stop
//     writes it once; it is read once as Run starts. A restarted or standby process
//     therefore RESUMEs, and Discord replays the gap's events. A sequence
//     number alone never triggers a write: every event of every server moves
//     it, and a write a second is 86,400 KV writes a day.
//   - A fresh IDENTIFY (no session, or a RESUME refused) replays nothing, so
//     each bound server's channels — as its GUILD_CREATE lists them, read only
//     after Bound says yes — are read back with GET /channels/{id}/messages
//     ?after=, from the newest message seen (or the window's start, whichever
//     is later), 50 per channel, BackfillWindow (10 min) at most. Each message
//     found goes through the very path a live one takes; a message id already
//     acted on is never acted on twice.

// discordEpochMs is the first millisecond of 2015, Discord's epoch.
const discordEpochMs = 1420070400000

// snowflakeAt is the smallest snowflake of the millisecond t.
func snowflakeAt(t time.Time) string {
	ms := t.UnixMilli() - discordEpochMs
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatUint(uint64(ms)<<22, 10)
}

// snowAfter reports whether snowflake x is later than y (decimal strings of
// up to 20 digits, compared by length, then by digits).
func snowAfter(x, y string) bool {
	if len(x) != len(y) {
		return len(x) > len(y)
	}
	return x > y
}

func (a *Auto) loadSession(ctx context.Context) {
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s, err := a.sessions.Load(lctx)
	if err != nil {
		a.log.Warn("discord auto: the saved session could not be read; IDENTIFY instead", "outcome", "session_load")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.Last != "" && validSnowflake(s.Last) {
		a.last = s.Last
	}
	// READY's identity, kept with the session: a RESUME never sees a READY
	// (§20 erratum 334, the 2026-10-12 amendment).
	if a.selfID == "" && validSnowflake(s.UserID) {
		a.selfID = s.UserID
	}
	if a.appID == "" && validSnowflake(s.AppID) {
		a.appID = s.AppID
	}
	if s.ID == "" || s.ResumeURL == "" || a.session.id != "" {
		a.log.Info("discord auto: no saved session", "outcome", "session_none")
		return
	}
	a.session = sessionState{id: s.ID, resumeURL: s.ResumeURL, seq: s.Seq, hasSeq: true}
	a.log.Info("discord auto: saved session read; RESUME", "outcome", "session_loaded")
}

// fetchIdentity is the fallback for a saved session with no identity in it
// (one saved before the 2026-10-12 amendment): a process about to RESUME never
// sees a READY, so the bot user and the application are asked once, through
// the REST manners, before the gateway opens. Without them every press would
// be acknowledged and dropped.
func (a *Auto) fetchIdentity(ctx context.Context) {
	if self, app := a.identity(); self != "" && app != "" {
		return
	}
	if !a.sessionSnapshot().resumes() {
		return // a fresh IDENTIFY: READY names both in a moment
	}
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	idOf := func(path string) string {
		resp, err := a.rest.do(fctx, http.MethodGet, path, "GET "+path, nil, true)
		if err != nil || !ok2xx(resp) {
			return ""
		}
		var r struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(resp.Body, &r) != nil || !validSnowflake(r.ID) {
			return ""
		}
		return r.ID
	}
	user, app := idOf("/users/@me"), idOf("/applications/@me")
	a.mu.Lock()
	if a.selfID == "" && user != "" {
		a.selfID = user
	}
	if a.appID == "" && app != "" {
		a.appID = app
	}
	ok := a.selfID != "" && a.appID != ""
	if ok {
		a.dirty = true // saved with the session from now on
	}
	a.mu.Unlock()
	if !ok {
		a.log.Warn("discord auto: the bot's identity could not be read; presses wait for a READY", "outcome", "identity")
	}
}

// saveLoop writes the session whenever it changed, every saveEvery (real
// time: the debounce is the plane's, not a test clock's), and once more when
// ctx ends.
func (a *Auto) saveLoop(ctx context.Context) {
	for sleepCtx(ctx, a.saveWait()) == nil {
		a.flush(ctx, flushTick)
	}
	fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.flush(fctx, flushLast)
}

// saveWait is the loop's next sleep: saveEvery, or less when an act's own
// Save started under saveEvery ago (the next tick lands when one more Save
// is allowed).
func (a *Auto) saveWait() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastSave.IsZero() {
		return a.saveEvery
	}
	if d := time.Until(a.lastSave.Add(a.saveEvery)); d > 0 && d < a.saveEvery {
		return d
	}
	return a.saveEvery
}

// Why a flush runs.
type flushWhy int

const (
	flushTick  flushWhy = iota // the loop's: dirty, outside the backoff, one a saveEvery
	flushAct                   // an act's end: as a tick, and only when no Save is under way
	flushFinal                 // Shutdown's: whatever the backoff, as flushLast
	flushLast                  // Run's end: when a session is held and differs from the last saved (the latest seq)
)

// savedAfterAct saves the session as an acted message's act completes (the
// reply posted, or the upload's job over), so a process that dies a moment
// later does not leave `last` unsaved and the next start's backfill act on it
// again — bounded as the loop is: one Save a saveEvery, none in a backoff,
// none while another is under way.
func (a *Auto) savedAfterAct() {
	if a.sessions == nil || a.ctx.Err() != nil {
		return // shutting down: Shutdown saves what changed
	}
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()
	a.flush(ctx, flushAct)
}

// flush saves the session when it is dirty, or on flushLast/flushFinal (the
// last saves, with the latest sequence number) when there is one to save that
// differs from what the last successful Save wrote — so Shutdown's save and
// Run's, whichever comes second, never write the same session twice.
//
// A failed Save backs off (§20 erratum 334, the 2026-10-12 amendment): the
// next try waits saveEvery, then twice that, doubling to 300 × saveEvery (1 s
// → 5 min in production), counted from the moment the failed Save RETURNED,
// and a success resets it; one WARN per failed try, never one per tick. A Save
// the store REFUSED (ErrSessionRefused: a 4xx other than 429) is not tried
// again — not even by the last save — until the session changes (READY, a
// session forgotten, a video acted on).
func (a *Auto) flush(ctx context.Context, why flushWhy) {
	if why == flushAct {
		if !a.saveMu.TryLock() {
			return // a Save is under way; the loop saves what it misses
		}
	} else {
		a.saveMu.Lock()
	}
	defer a.saveMu.Unlock()
	now := time.Now()
	a.mu.Lock()
	if a.saveRefused && !a.dirty {
		a.mu.Unlock()
		return
	}
	s := Session{ID: a.session.id, ResumeURL: a.session.resumeURL, Last: a.last, UserID: a.selfID, AppID: a.appID}
	if a.session.hasSeq {
		s.Seq = a.session.seq
	}
	final := why == flushLast || why == flushFinal
	if !a.dirty && !(final && a.session.id != "" && !(a.savedOK && s == a.saved)) {
		a.mu.Unlock()
		return
	}
	if (why == flushTick || why == flushAct) &&
		(now.Before(a.saveRetryAt) || (!a.lastSave.IsZero() && now.Sub(a.lastSave) < a.saveEvery)) {
		a.mu.Unlock()
		return
	}
	a.dirty = false
	a.lastSave = now
	a.mu.Unlock()
	err := a.sessions.Save(ctx, s)
	now = time.Now() // the backoff counts from the answer, not the ask
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case err == nil:
		a.saveFails, a.saveRetryAt, a.saveRefused = 0, time.Time{}, false
		a.saved, a.savedOK = s, true
	case errors.Is(err, ErrSessionRefused):
		a.saveFails, a.saveRetryAt, a.saveRefused = 0, time.Time{}, true
		a.log.Warn("discord auto: the session was refused; not saved again until it changes", "outcome", "session_refused")
	default:
		a.dirty = true
		a.saveFails++
		d := a.saveEvery << min(a.saveFails-1, 20)
		if ceiling := a.saveEvery * saveBackoffSteps; d > ceiling || d <= 0 {
			d = ceiling
		}
		a.saveRetryAt = now.Add(d)
		a.log.Warn("discord auto: the session could not be saved", "outcome", "session_save",
			"retry_in", d.Round(time.Millisecond).String())
	}
}

func validSnowflake(s string) bool {
	if len(s) < 1 || len(s) > 20 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// firstSight records a message id and reports whether it is new. Ids older
// than seenKeep are forgotten.
func (a *Auto) firstSight(id string) bool {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.seen[id]; ok {
		return false
	}
	if len(a.seen) >= seenCap {
		for k, t := range a.seen {
			if now.Sub(t) > seenKeep || len(a.seen) >= seenCap {
				delete(a.seen, k)
			}
		}
	}
	a.seen[id] = now
	return true
}

// acted records an applicable video's message id as the session's Last and
// makes the session dirty: the one live event that is worth a write.
func (a *Auto) acted(id string) {
	if a.sessions == nil && !a.backfill {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last == "" || snowAfter(id, a.last) {
		a.last = id
	}
	if a.sessions != nil {
		a.dirty = true
	}
}

// armBackfill is called on the READY of a fresh IDENTIFY.
func (a *Auto) armBackfill() {
	if !a.backfill {
		return
	}
	now := a.now()
	after := snowflakeAt(now.Add(-a.bfWindow))
	a.mu.Lock()
	if a.last != "" && snowAfter(a.last, after) {
		after = a.last
	}
	a.bfAfter, a.bfReady, a.bfDone = after, now, map[string]bool{}
	a.mu.Unlock()
}

// backfillDue reports whether a GUILD_CREATE now belongs to a fresh session's
// start.
func (a *Auto) backfillDue() bool {
	if !a.backfill {
		return false
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bfAfter != "" && now.Sub(a.bfReady) < backfillAfterReady
}

// The channel types a video can be posted in: text, voice (its chat),
// announcement, the three thread kinds, stage (its chat).
var backfillTypes = map[int]bool{0: true, 2: true, 5: true, 10: true, 11: true, 12: true, 13: true}

func (a *Auto) onGuildCreate(ctx context.Context, d json.RawMessage) {
	host := a.hostOf()
	if host == nil {
		return
	}
	// The guild FIRST (a GUILD_CREATE's own id), and nothing else until it is
	// known to be bound.
	var g struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(d, &g) != nil || !validSnowflake(g.ID) {
		return
	}
	if ok, err := host.Bound(ctx, g.ID); err != nil || !ok {
		return
	}
	a.mu.Lock()
	after := a.bfAfter
	if after == "" || a.bfDone[g.ID] {
		a.mu.Unlock()
		return
	}
	a.bfDone[g.ID] = true
	a.mu.Unlock()
	var full struct {
		Channels []struct {
			ID   string `json:"id"`
			Type int    `json:"type"`
		} `json:"channels"`
		Threads []struct {
			ID   string `json:"id"`
			Type int    `json:"type"`
		} `json:"threads"`
	}
	if json.Unmarshal(d, &full) != nil {
		return
	}
	var chans []string
	for _, c := range append(full.Channels, full.Threads...) {
		if backfillTypes[c.Type] && validSnowflake(c.ID) && len(chans) < backfillChannels {
			chans = append(chans, c.ID)
		}
	}
	found, read := 0, 0
	for _, ch := range chans {
		if ctx.Err() != nil {
			return
		}
		resp, err := a.channelMessages(ctx, ch, after, backfillPerChannel)
		if err != nil || !ok2xx(resp) {
			continue // no Read Message History there, or gone: skipped
		}
		read++
		var msgs []map[string]json.RawMessage
		if json.Unmarshal(resp.Body, &msgs) != nil {
			continue
		}
		gid, _ := json.Marshal(g.ID)
		// Discord answers newest first: act oldest first.
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			m["guild_id"] = gid
			raw, err := json.Marshal(m)
			if err != nil {
				continue
			}
			found++
			a.onMessage(ctx, raw)
		}
	}
	a.log.Info("discord auto: backfill", "guild_id", g.ID, "event", "backfill", "outcome", "read",
		"channels", read, "messages", found)
	a.obs.Decide(Decision{Event: "backfill", Guild: g.ID, Outcome: "messages_" + strconv.Itoa(found)})
}
