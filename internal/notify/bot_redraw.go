package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The bot's follow-ups beyond the link (§20 errata 325, 330):
//
//   - **"Uploading…"** A modal answers `/upload`, so Discord shows no
//     "thinking…", and a dismissed form is silent: Discord sends the app
//     nothing when a modal is closed. A job with ClipJob.Uploading posts one
//     EPHEMERAL follow-up — only the person who ran the command sees it — and
//     deletes it when the job ends.
//   - **The redraw.** Discord draws (and caches, for about half an hour) a
//     link's card from the page as it was when it first read it, and it never
//     reads that URL again by itself. Two things can make that card wrong after
//     the link is posted: the upload form's late title (erratum 325), and a
//     card that was not READY yet (erratum 330: the bot waits its card budget,
//     then posts the link anyway — an audiogram or a long video's Light copy
//     routinely outruns it, and Discord then reads a page with no player). In
//     both cases the bot edits its own message from the link to the same link
//     plus a version query — `?v=2`, then `?v=3`, … — a different URL, so
//     Discord fetches the page again and draws the card as it is now. Measured
//     2026-10-06 on a channel webhook (the kind of webhook a follow-up is): the
//     PATCH's own answer had no embed, the new URL's card was there 1.8 s later,
//     one fresh GET of the page.
//
// A link is HELD for a redraw only while something may still change its card:
// a form job's link (the title), and any link posted with IngestResult.CardReady
// false — `/upload` with or without the form, an old `/clip file:`, Enrich's
// upload, and `/clip link:` or Enrich on a clip whose card is not ready yet. The
// worker says when a clip's card becomes ready (worker.Deps.CardReady →
// Bot.CardReady); a link whose card WAS ready at post, and a duplicate's answer
// (erratum 326), are never held, so nothing redraws them.
//
// Everything uses only the interaction-token routes the reply already uses, so
// an instance on the official bot (DISCORD_BOT_TOKEN=managed) does exactly what
// a self-hosted one does. The token and the message id are held here and nowhere
// else, in memory, bounded, for the token's fifteen minutes; a restart forgets
// them (the clip is unchanged; there is then no redraw, and a later paste of the
// link draws the card). Nothing typed into the form ever reaches this file: a
// redraw takes a clip id, and its log line is that id, a reason word and an
// outcome word.

// SentenceUploading is the ephemeral line a form job posts while it runs — the
// same line Clippet Auto uses.
const SentenceUploading = "Uploading…"

const (
	// redrawFirst is the version the first redraw of a clip's link carries: the
	// bare link is version 1, so the first edit is `?v=2`. Each later redraw of
	// any held link of the same clip takes the next number (redrawQuery), so no
	// two edits ever name a URL Discord may already have read.
	redrawFirst = 2
	// linkTableMax bounds the links held for a redraw, as formTableMax bounds
	// the forms in internal/httpapi; a full table holds nothing more (no
	// redraw for that link, never an error).
	linkTableMax = 256
	// linkLife is the interaction token's life: past it no edit can be made.
	linkLife = 15 * time.Minute
	// flagEphemeral is Discord's EPHEMERAL message flag, 1 << 6.
	flagEphemeral = 1 << 6
	// originalMessage is the message id of an interaction's own reply — a
	// deferral's edited "thinking…", or `/clip link:`'s type-4 answer — on the
	// interaction's webhook: `/messages/@original`.
	originalMessage = "@original"
)

// redrawQuery is what the redraw appends to the share link for version v. The
// page ignores it (the same page, the same canonical, the same og:url and card
// links, for any `v`), and every parser of share links reads past it.
func redrawQuery(v int) string { return "?v=" + strconv.Itoa(v) }

// redrawReason is why a held link may be redrawn: a bit set.
type redrawReason uint8

const (
	reasonCard  redrawReason = 1 << iota // the card was not ready when the link was posted
	reasonTitle                          // the upload form changed the title after the post
)

// String is the reason as the log line's word.
func (r redrawReason) String() string {
	switch r {
	case reasonCard:
		return "card"
	case reasonTitle:
		return "title"
	case reasonCard | reasonTitle:
		return "card+title"
	}
	return "none"
}

// ephemeralPayload is a follow-up only the person who ran the command sees.
type ephemeralPayload struct {
	Content         string          `json:"content"`
	Flags           int             `json:"flags"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

// postedLink is one held link: the interaction token (a SECRET for fifteen
// minutes, never logged), the message's id once it exists, and what may still
// redraw it.
type postedLink struct {
	clipID    string
	token     string
	messageID string // "" while the post is in flight; originalMessage for an interaction's own reply
	expires   time.Time
	owed      redrawReason // the redraws this link may still get, each at most once
	wanted    redrawReason // asked for while the post was in flight: the job redraws once it lands
}

// clipLinks is one clip's held links and the last version any of them was
// redrawn to (1 = none yet).
type clipLinks struct {
	links   []*postedLink
	version int
}

// linkTable holds the posted links by clip id. A clip may have more than one —
// the upload's own link and a `/clip link:` of it, say — and a redraw edits
// every one of them that is owed it.
type linkTable struct {
	mu sync.Mutex
	m  map[string]*clipLinks
	n  int
}

func newLinkTable() *linkTable { return &linkTable{m: map[string]*clipLinks{}} }

// begin holds a link that is about to be posted (messageID "", BEFORE the post:
// an ask that lands while it is in flight is then not lost) or that is being
// posted right now as an interaction's own reply (originalMessage). nil when
// the table is full or nothing is owed — that link is simply not redrawn.
func (t *linkTable) begin(clipID, token, messageID string, owed redrawReason, expires, now time.Time) *postedLink {
	if owed == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked(now)
	if t.n >= linkTableMax {
		return nil
	}
	c := t.m[clipID]
	if c == nil {
		c = &clipLinks{version: redrawFirst - 1}
		t.m[clipID] = c
	}
	e := &postedLink{clipID: clipID, token: token, messageID: messageID, expires: expires, owed: owed}
	c.links = append(c.links, e)
	t.n++
	return e
}

// sweepLocked drops every expired link.
func (t *linkTable) sweepLocked(now time.Time) {
	for _, c := range t.m {
		for _, e := range c.links {
			if !now.Before(e.expires) {
				t.removeLocked(e)
			}
		}
	}
}

// removeLocked drops one link (a no-op for one already gone), and the clip's
// row with its last link.
func (t *linkTable) removeLocked(e *postedLink) {
	c := t.m[e.clipID]
	if c == nil {
		return
	}
	for i, x := range c.links {
		if x == e {
			c.links = append(c.links[:i], c.links[i+1:]...)
			t.n--
			break
		}
	}
	if len(c.links) == 0 {
		delete(t.m, e.clipID)
	}
}

// nextVersionLocked is the version the next redraw of clipID's links carries.
func (t *linkTable) nextVersionLocked(clipID string) int {
	c := t.m[clipID]
	if c == nil {
		return redrawFirst // unreachable: a link being redrawn is in the table
	}
	c.version++
	return c.version
}

// redrawTask is one edit to make, decided under the lock.
type redrawTask struct {
	token, messageID string
	version          int
	reason           redrawReason
}

// posted records the link's message id once Discord has it. When a redraw was
// asked for while the post was in flight it reports that edit — the caller
// makes it now. An empty id (no id in Discord's answer) forgets the link.
func (t *linkTable) posted(e *postedLink, id string) (redrawTask, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == "" {
		t.removeLocked(e)
		return redrawTask{}, false
	}
	e.messageID = id
	wanted := e.wanted
	e.wanted = 0
	if e.owed == 0 {
		t.removeLocked(e)
	}
	if wanted == 0 {
		return redrawTask{}, false
	}
	return redrawTask{token: e.token, messageID: id, version: t.nextVersionLocked(e.clipID), reason: wanted}, true
}

// forget drops a link (the post failed).
func (t *linkTable) forget(e *postedLink) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeLocked(e)
}

// take is a redraw's one locked step: for every held link of clipID owed this
// reason, the edit to make now — or, for a link still in flight, a mark so the
// job edits once it lands (later). Each reason redraws a link once: it is
// struck off as it is taken, and a link owed nothing more leaves the table.
func (t *linkTable) take(clipID string, reason redrawReason, now time.Time) (tasks []redrawTask, later bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.m[clipID]
	if c == nil {
		return nil, false
	}
	for _, e := range append([]*postedLink(nil), c.links...) {
		if !now.Before(e.expires) {
			t.removeLocked(e)
			continue
		}
		if e.owed&reason == 0 {
			continue
		}
		e.owed &^= reason
		if e.messageID == "" {
			e.wanted |= reason
			later = true
			continue
		}
		tasks = append(tasks, redrawTask{token: e.token, messageID: e.messageID,
			version: t.nextVersionLocked(clipID), reason: reason})
		if e.owed == 0 {
			t.removeLocked(e)
		}
	}
	return tasks, later
}

// len is for the tests.
func (t *linkTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n
}

// deliverLink is a job's success: the link, held for a redraw when one may be
// owed (owed != 0) — posted as a new follow-up after the upload form, or edited
// into the deferred reply. A link owed nothing goes out through deliver, as it
// always did.
func (b *Bot) deliverLink(ctx context.Context, job ClipJob, clipID string, owed redrawReason, started time.Time) {
	msg := ClipMessage(b.urls, clipID)
	if owed == 0 {
		b.deliver(ctx, job, msg, clipID)
		return
	}
	held := b.links.begin(clipID, job.Token, "", owed, started.Add(linkLife), time.Now())
	id, err := originalMessage, error(nil)
	if job.Followup {
		id, err = b.followupLink(ctx, job.Token, msg, clipID)
	} else {
		err = b.editOriginal(ctx, job.Token, msg)
	}
	if err != nil {
		if held != nil {
			b.links.forget(held)
		}
		line := "discord bot: the deferred reply could not be edited"
		if job.Followup {
			line = "discord bot: the follow-up could not be posted"
		}
		b.log.Warn(line, "err", b.scrubJob(err.Error(), job.Token))
		return
	}
	if !job.Followup {
		// The reply IS the link now: the panic recovery checks linkIDs and must
		// never edit a failure sentence over it (§20 erratum 320 (g)).
		b.linkIDs.hold(job.Token, originalMessage, time.Now())
	}
	if held == nil {
		return
	}
	if task, now := b.links.posted(held, id); now {
		// The card became ready, or the form changed the title, while the post
		// was in flight: Discord may have read the page before. Redraw it,
		// here, in the job.
		b.redraw(clipID, task)
	}
}

// HoldReply holds the link an interaction is answering with RIGHT NOW — the
// type-4 reply of `/clip link:` or Enrich on a link, whose message is the
// interaction's own (`@original`) — when that clip's card is not ready yet (§20
// erratum 330): once the worker says it is, the reply is redrawn. The caller
// calls it before its response is written. It never blocks.
func (b *Bot) HoldReply(token, clipID string) {
	now := time.Now()
	b.links.begin(clipID, token, originalMessage, reasonCard, now.Add(linkLife), now)
}

// Redraw is the upload form's late submit (§20 erratum 325): the form's write
// CHANGED the title of a clip whose link this bot may already have posted.
// When it has, the message's content becomes the same link plus the next
// version query, so Discord draws the card again; when the post is still in
// flight, the job does it once Discord answers; otherwise nothing happens
// (the card is drawn after the write anyway, or there is nothing to edit).
//
// It never blocks — the interactions handler calls it before its response is
// flushed (internal/httpapi's property 5) — and it never fails anybody: a
// failed edit is one log line with the clip id, the reason and an outcome word.
func (b *Bot) Redraw(clipID string) { b.redrawFor(clipID, reasonTitle) }

// CardReady is the worker's word that clipID's card has just become ready
// (§20 erratum 330; worker.Deps.CardReady): every held link of that clip that
// was posted before its card was ready is redrawn, once. A clip nothing of the
// bot's is waiting on — almost every clip — costs one map lookup and no log
// line at INFO. It never blocks the worker.
func (b *Bot) CardReady(clipID string) { b.redrawFor(clipID, reasonCard) }

// redrawFor is Redraw and CardReady: the edits are made in one goroutine of
// the Bot's own, counted for Shutdown.
func (b *Bot) redrawFor(clipID string, reason redrawReason) {
	tasks, later := b.links.take(clipID, reason, time.Now())
	if later {
		b.log.Info("discord bot: card redraw", "clip", clipID, "reason", reason.String(), "outcome", "after_post")
	}
	if len(tasks) == 0 {
		if !later {
			// A card that became ready with nothing held is every web upload's
			// ordinary case: not worth an INFO line each.
			level := slog.LevelInfo
			if reason == reasonCard {
				level = slog.LevelDebug
			}
			b.log.Log(context.Background(), level, "discord bot: card redraw", "clip", clipID,
				"reason", reason.String(), "outcome", "none")
		}
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.log.Info("discord bot: card redraw", "clip", clipID, "reason", reason.String(), "outcome", "restart")
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				b.log.Error("discord bot: card redraw", "clip", clipID, "reason", reason.String(), "outcome", "panic")
			}
		}()
		for _, t := range tasks {
			b.redraw(clipID, t)
		}
	}()
}

// redraw PATCHes one held message: `content` only — the link plus the task's
// version query — and the empty allowed_mentions list every message here
// carries. No embed, no component, no flag: Discord drops the old card and
// draws the new URL's. One retry on the retryable set, as every edit here (a
// 404 included: a type-4 reply redrawn within a second of its answer may not
// exist at Discord yet).
func (b *Bot) redraw(clipID string, t redrawTask) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(b.ctx), 2*b.callTimeout+botEditRetryWait)
	defer cancel()
	msg := ClipMessage(b.urls, clipID)
	msg.Content += redrawQuery(t.version)
	body, _ := json.Marshal(msg) // plain strings: cannot fail
	path := b.followupPath(t.token) + "/messages/" + t.messageID
	err := b.call(ctx, http.MethodPatch, path, body, false)
	var re *retryableError
	if err != nil && errors.As(err, &re) {
		select {
		case <-time.After(botEditRetryWait):
			err = b.call(ctx, http.MethodPatch, path, body, false)
		case <-ctx.Done():
		}
	}
	if err != nil {
		b.log.Warn("discord bot: card redraw", "clip", clipID, "reason", t.reason.String(), "outcome", "failed")
		return
	}
	b.log.Info("discord bot: card redraw", "clip", clipID, "reason", t.reason.String(), "outcome", "edited")
}

// notApplied posts SentenceFormNotApplied as an ephemeral follow-up (§20
// erratum 326): the upload form's duplicate. A line that will not post is one
// log line; the link still goes out.
func (b *Bot) notApplied(ctx context.Context, job ClipJob) {
	if _, err := b.followup(ctx, job.Token, ephemeralPayload{
		Content: SentenceFormNotApplied, Flags: flagEphemeral,
		AllowedMentions: allowedMentions{Parse: []string{}},
	}); err != nil {
		b.log.Info("discord bot: the form-not-applied line was not posted",
			"err", b.scrubJob(err.Error(), job.Token))
	}
}

// postUploading posts SentenceUploading as an ephemeral follow-up beside the
// job and hands back the message's id on a buffered channel ("" when it was
// not posted). The job's upload never waits on it.
func (b *Bot) postUploading(ctx context.Context, job ClipJob) <-chan string {
	out := make(chan string, 1)
	go func() {
		id := ""
		defer func() {
			if p := recover(); p != nil {
				b.log.Error("discord bot: the uploading line panicked", "guild", job.GuildID)
			}
			out <- id
		}()
		var err error
		id, err = b.followup(ctx, job.Token, ephemeralPayload{
			Content: SentenceUploading, Flags: flagEphemeral,
			AllowedMentions: allowedMentions{Parse: []string{}},
		})
		if err != nil {
			b.log.Info("discord bot: the uploading line was not posted",
				"err", b.scrubJob(err.Error(), job.Token))
		}
	}()
	return out
}

// deleteUploading waits for postUploading and deletes the line it posted. It
// runs once the job's last message is out; a failure is one log line.
func (b *Bot) deleteUploading(job ClipJob, line *heldLine) {
	id := line.wait()
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(b.ctx), b.callTimeout)
	defer cancel()
	if err := b.call(ctx, http.MethodDelete, b.followupPath(job.Token)+"/messages/"+id, nil, false); err != nil {
		b.log.Info("discord bot: the uploading line was not deleted",
			"err", b.scrubJob(err.Error(), job.Token))
	}
}
