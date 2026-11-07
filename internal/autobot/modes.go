package autobot

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"strconv"
	"time"
)

// The words (the owner's design of 2026-10-11 and the brief's strings; the rest
// are the settled lines of the hosted Worker this replaces, reused as written).
const (
	// LabelUpload is Prompt's one button.
	LabelUpload = "Upload to Clippet"
	// LineUploading answers a press, to the presser alone.
	LineUploading = "Uploading…"
	// LineNotice is the one line under each link, unless DISCORD_AUTO_NOTICE=off.
	LineNotice = "Videos posted here become Clippet links."
	// LineNeedsManage is the missing-permission line: to the presser (Prompt),
	// or once a day to the channel (Auto).
	LineNeedsManage = "Clippet Auto needs Manage Messages to remove the original."
	// LineNotYours answers a press by somebody who is neither the poster nor a
	// manager (rescue's NOT_YOURS).
	LineNotYours = "Only the poster or someone with Manage Server can do this."
	// LineBusy answers a press when the server's queue is full (rescue's BUSY).
	LineBusy = "Busy. Try again in a minute."
	// LineFailed answers a press whose upload failed for a reason with no line
	// of its own (rescue's FAILED).
	LineFailed = "That video could not be uploaded."
)

// PressID is the button's custom_id. It carries nothing: the facts of the
// reply are held in memory under the reply's id. Handled in BOTH places a
// press can arrive — the socket (an app with no Interactions Endpoint URL) and
// the instance's §30 endpoint (an app with one) — by the one function Press.
const PressID = "auto:go"

// Discord's numbers.
const (
	interactionComponent = 3
	callbackMessage      = 4
	callbackDeferUpdate  = 6
	flagEphemeral        = 1 << 6
	permAdministrator    = 1 << 3
	permManageGuild      = 1 << 5
	componentRow         = 1
	componentButton      = 2
	buttonPrimary        = 1
)

// permLineEvery is how often Auto's Manage Messages line may be said per server.
const permLineEvery = 24 * time.Hour

// waiting is one Prompt reply under its timer.
type waiting struct {
	guild, channel, original, poster string
	att                              Attachment
	notice                           bool
	at                               time.Time // when the reply was posted
	stop                             func() bool
}

// job is one upload in a server's queue.
type job struct {
	guild, channel, original string
	att                      Attachment
	notice                   bool
	mode                     string
	// press is set for a Prompt press; nil for Auto.
	press *pressRef
}

type pressRef struct {
	appID, token, replyID string
}

type guildQueue struct {
	ch chan job
}

// ---------------------------------------------------------------------------
// A message was posted.

type messageEvent struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	WebhookID string `json:"webhook_id"`
	Author    struct {
		ID  string `json:"id"`
		Bot bool   `json:"bot"`
	} `json:"author"`
	Attachments []Attachment `json:"attachments"`
}

func (a *Auto) onMessage(ctx context.Context, d json.RawMessage) {
	host := a.hostOf()
	if host == nil {
		return
	}
	// The guild FIRST, and nothing else until it is known to be bound.
	var g struct {
		GuildID string `json:"guild_id"`
	}
	if json.Unmarshal(d, &g) != nil || g.GuildID == "" {
		return
	}
	if ok, err := host.Bound(ctx, g.GuildID); err != nil || !ok {
		return
	}
	var m messageEvent
	if json.Unmarshal(d, &m) != nil || m.ID == "" || m.ChannelID == "" {
		return
	}
	self, _ := a.identity()
	if m.Author.Bot || m.WebhookID != "" || m.Author.ID == "" || m.Author.ID == self {
		return
	}
	// Once per message: a backfill never repeats a live one, and the newest
	// id is the saved session's Last (§20 erratum 334; the hosted process only).
	if (a.backfill || a.sessions != nil) && !a.firstSight(m.ID) {
		return
	}
	att, ok := firstApplicable(host, m.Attachments)
	if !ok {
		return // not applicable: ignored, silently
	}
	// A person-scoped binding (§20 erratum 342) consented for one member: any
	// other poster's video is ignored silently — no reply, no fetch, not acted
	// on; the decision says skip=person, never the id (erratum 333 amended).
	if !personAllows(ctx, host, m.GuildID, m.Author.ID) {
		a.obs.Decide(Decision{Event: "video", Guild: m.GuildID, Channel: m.ChannelID, Message: m.ID, Mode: a.mode,
			Outcome: "ignored", Skip: "person"})
		return
	}
	mode, secs, notice := a.mode, a.seconds, a.notice
	var st Settings
	var stErr error
	if mode == ModeGuild {
		st, stErr = a.guildSettings(ctx, m.GuildID)
		// A poster who routes his uploads to his own box (the server's skip
		// list, §20 erratum 334 amended 2026-10-19) is ignored silently: no
		// reply, no fetch, not acted on — the owner's box never takes a
		// friend's video. Only a debug line says so, without the id.
		if stErr == nil && (st.Mode == ModePrompt || st.Mode == ModeAuto) && st.Skip[m.Author.ID] {
			a.obs.Decide(Decision{Event: "video", Guild: m.GuildID, Channel: m.ChannelID, Message: m.ID, Mode: st.Mode,
				Outcome: "ignored", Skip: "person"})
			return
		}
	}
	a.acted(m.ID)
	if mode == ModeGuild {
		if stErr != nil || (st.Mode != ModePrompt && st.Mode != ModeAuto) {
			return
		}
		mode, secs, notice = st.Mode, secondsOf(st), st.Notice
	}
	if mode == ModeAuto {
		outcome := "queued"
		if !a.enqueue(job{guild: m.GuildID, channel: m.ChannelID, original: m.ID, att: att, notice: notice, mode: mode}) {
			outcome = "dropped"
			a.log.Warn("discord auto: queue full; video dropped", "guild_id", m.GuildID, "event", "auto", "outcome", "dropped")
		}
		a.obs.Decide(Decision{Event: "video", Guild: m.GuildID, Channel: m.ChannelID, Message: m.ID, Mode: mode, Outcome: outcome})
		return
	}
	a.offer(ctx, m, att, secs, notice)
}

// errNoGuildSettings: under ModeGuild, a Host that answers no settings.
const errNoGuildSettings = constError("the host answers no per-server settings")

// guildSettings is the server's own Clippet Auto under ModeGuild, read when a
// video is seen AND again when its reply's timer fires or its button is
// pressed: a server switched mid-reply follows the switch.
func (a *Auto) guildSettings(ctx context.Context, guild string) (Settings, error) {
	gs, ok := a.hostOf().(GuildSettings)
	if !ok {
		return Settings{}, errNoGuildSettings
	}
	return gs.Settings(ctx, guild)
}

// secondsOf is a server's Prompt reply lifetime: 5-60 s, else the default.
func secondsOf(st Settings) time.Duration {
	if st.Seconds >= 5 && st.Seconds <= 60 {
		return time.Duration(st.Seconds) * time.Second
	}
	return time.Duration(defaultSeconds) * time.Second
}

func firstApplicable(h Host, atts []Attachment) (Attachment, bool) {
	for _, at := range atts {
		if h.Applicable(at) {
			return at, true
		}
	}
	return Attachment{}, false
}

// offerBody is Prompt's reply: no text, ONE button, a reply to the video that
// pings nobody. `fail_if_not_exists`: a video deleted first gets no reply.
func offerBody(channel, original string) map[string]any {
	return map[string]any{
		"message_reference": map[string]any{"message_id": original, "channel_id": channel, "fail_if_not_exists": true},
		"allowed_mentions":  map[string]any{"parse": []string{}, "replied_user": false},
		"components": []any{map[string]any{"type": componentRow, "components": []any{
			map[string]any{"type": componentButton, "style": buttonPrimary, "label": LabelUpload, "custom_id": PressID},
		}}},
	}
}

// offer posts the reply and arms its timer.
func (a *Auto) offer(ctx context.Context, m messageEvent, att Attachment, secs time.Duration, notice bool) {
	resp, err := a.createMessage(ctx, m.ChannelID, offerBody(m.ChannelID, m.ID))
	if err != nil || !ok2xx(resp) {
		a.log.Info("discord auto: offer", "guild_id", m.GuildID, "event", "offer", "outcome", outcomeOf(resp, err))
		a.obs.Decide(Decision{Event: "video", Guild: m.GuildID, Channel: m.ChannelID, Message: m.ID, Mode: ModePrompt,
			Outcome: "offer_" + outcomeOf(resp, err)})
		return
	}
	var r struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(resp.Body, &r) != nil || r.ID == "" {
		return
	}
	w := &waiting{guild: m.GuildID, channel: m.ChannelID, original: m.ID, poster: m.Author.ID, att: att, notice: notice,
		at: a.now()}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	reply := r.ID
	w.stop = a.afterFunc(secs, func() { a.expire(reply) })
	a.waiting[reply] = w
	a.mu.Unlock()
	a.log.Info("discord auto: offer", "guild_id", m.GuildID, "event", "offer", "outcome", "sent")
	a.obs.Decide(Decision{Event: "video", Guild: m.GuildID, Channel: m.ChannelID, Message: m.ID, Mode: ModePrompt, Outcome: "offered"})
	a.savedAfterAct()
}

// expire deletes a reply nobody pressed. Under ModeGuild the server's settings
// are read again first (expireGuild).
func (a *Auto) expire(reply string) {
	if a.mode == ModeGuild {
		a.mu.Lock()
		w, ok := a.waiting[reply]
		a.mu.Unlock()
		if ok {
			a.goTracked(func() { a.expireGuild(reply, w) })
		}
		return
	}
	a.mu.Lock()
	w, ok := a.waiting[reply]
	if ok {
		delete(a.waiting, reply)
	}
	a.mu.Unlock()
	if !ok {
		return
	}
	a.goTracked(func() {
		resp, err := a.deleteMessage(a.ctx, w.channel, reply)
		a.log.Info("discord auto: offer", "guild_id", w.guild, "event", "offer", "outcome", "expired_"+outcomeOf(resp, err))
		a.obs.Decide(Decision{Event: "offer", Guild: w.guild, Channel: w.channel, Message: w.original, Mode: ModePrompt,
			Outcome: "expired_" + outcomeOf(resp, err)})
	})
}

// expireGuild is a reply's timer under ModeGuild, with the server's settings
// as they are NOW: still Prompt with a longer lifetime, the reply waits out
// the rest of it; switched to Auto, the reply goes and the video is uploaded
// as Auto would have (the notice as Auto's is now); off, or unread, the reply
// goes as it would have.
func (a *Auto) expireGuild(reply string, w *waiting) {
	st, err := a.guildSettings(a.ctx, w.guild)
	a.mu.Lock()
	if cur, still := a.waiting[reply]; !still || cur != w {
		a.mu.Unlock()
		return // pressed meanwhile
	}
	skipped := err == nil && st.Skip[w.poster] // on the skip list since: the reply goes, nothing uploads
	if err == nil && st.Mode == ModePrompt && !skipped && !a.closed {
		if rest := secondsOf(st) - a.now().Sub(w.at); rest > 0 {
			w.stop = a.afterFunc(rest, func() { a.expire(reply) })
			a.mu.Unlock()
			return
		}
	}
	delete(a.waiting, reply)
	a.mu.Unlock()
	resp, derr := a.deleteMessage(a.ctx, w.channel, reply)
	outcome, mode := "expired_"+outcomeOf(resp, derr), ModePrompt
	if err == nil && st.Mode == ModeAuto && !skipped {
		outcome, mode = "auto", ModeAuto
		if !a.enqueue(job{guild: w.guild, channel: w.channel, original: w.original, att: w.att, notice: st.Notice, mode: ModeAuto}) {
			outcome = "auto_dropped"
		}
	}
	a.log.Info("discord auto: offer", "guild_id", w.guild, "event", "offer", "outcome", outcome)
	a.obs.Decide(Decision{Event: "offer", Guild: w.guild, Channel: w.channel, Message: w.original, Mode: mode, Outcome: outcome})
}

// ---------------------------------------------------------------------------
// A press.

type interaction struct {
	ID            string `json:"id"`
	Type          int    `json:"type"`
	Token         string `json:"token"`
	ApplicationID string `json:"application_id"`
	GuildID       string `json:"guild_id"`
	ChannelID     string `json:"channel_id"`
	Data          struct {
		CustomID string `json:"custom_id"`
	} `json:"data"`
	Member *struct {
		Permissions string `json:"permissions"`
		User        struct {
			ID string `json:"id"`
		} `json:"user"`
	} `json:"member"`
	Message *struct {
		ID     string `json:"id"`
		Author struct {
			ID string `json:"id"`
		} `json:"author"`
	} `json:"message"`
}

// IsPress reports whether an interaction body is a press of this package's
// button — the §30 endpoint asks before it routes a component click.
func IsPress(raw []byte) bool {
	var in struct {
		Type int `json:"type"`
		Data struct {
			CustomID string `json:"custom_id"`
		} `json:"data"`
	}
	return json.Unmarshal(raw, &in) == nil && in.Type == interactionComponent && in.Data.CustomID == PressID
}

// Press is THE press function, for both doors. It answers the interaction's
// callback body and, when the press is accepted, a start func the caller runs
// ONCE the answer has been delivered — a press whose answer did not reach
// Discord uploads nothing. A nil answer is a press this package IGNORES (not
// a press of ours, or a server not bound here): the socket's door sends
// nothing at all, and the §30 endpoint, which must answer every interaction,
// answers it with PressIgnored.
func (a *Auto) Press(ctx context.Context, raw []byte) (answer map[string]any, start func()) {
	ack := map[string]any{"type": callbackDeferUpdate}
	var in interaction
	if json.Unmarshal(raw, &in) != nil || in.Type != interactionComponent || in.Data.CustomID != PressID || in.GuildID == "" {
		return nil, nil
	}
	host := a.hostOf()
	if host == nil {
		return nil, nil
	}
	if ok, err := host.Bound(ctx, in.GuildID); err != nil || !ok {
		return nil, nil // an unbound server is ignored
	}
	self, app := a.identity()
	if self == "" || in.Message == nil || in.Message.Author.ID != self || in.Message.ID == "" || in.ApplicationID != app {
		return ack, nil // only our own reply is ever acted on
	}
	reply := in.Message.ID
	a.mu.Lock()
	w, ok := a.waiting[reply]
	a.mu.Unlock()
	if !ok || w.channel != in.ChannelID || w.guild != in.GuildID {
		// A reply this process does not hold (a restart forgot it): it is
		// removed, so a stale button cannot linger.
		return ack, func() {
			a.goTracked(func() { _, _ = a.deleteMessage(a.ctx, in.ChannelID, reply) })
		}
	}
	presser := ""
	perms := ""
	if in.Member != nil {
		presser, perms = in.Member.User.ID, in.Member.Permissions
	}
	if !(presser != "" && presser == w.poster) && !hasManage(perms) {
		return ephemeral(LineNotYours), nil
	}
	// Under ModeGuild, the server's settings as they are at the press: its
	// notice now, and a server switched off since the reply removes it — as
	// does a poster put on the skip list since: his video is his own box's.
	notice, off := w.notice, false
	if a.mode == ModeGuild {
		if st, err := a.guildSettings(ctx, w.guild); err == nil {
			notice, off = st.Notice, (st.Mode != ModePrompt && st.Mode != ModeAuto) || st.Skip[w.poster]
		}
	}
	// A server re-bound to one person since the reply (§20 erratum 342) — the
	// reply goes unless its poster is that person.
	off = off || !personAllows(ctx, host, w.guild, w.poster)
	if off {
		a.mu.Lock()
		if cur, still := a.waiting[reply]; !still || cur != w {
			a.mu.Unlock()
			return ack, nil
		}
		delete(a.waiting, reply)
		a.mu.Unlock()
		w.stop()
		return ack, func() {
			a.goTracked(func() { _, _ = a.deleteMessage(a.ctx, w.channel, reply) })
		}
	}
	if a.queueFull(w.guild) {
		// Said on the press itself; the reply keeps waiting on its timer.
		return ephemeral(LineBusy), nil
	}
	// Claimed: the timer is stopped and nobody else can press it.
	a.mu.Lock()
	if cur, still := a.waiting[reply]; !still || cur != w {
		a.mu.Unlock()
		return ack, nil
	}
	delete(a.waiting, reply)
	a.mu.Unlock()
	w.stop()
	j := job{guild: w.guild, channel: w.channel, original: w.original, att: w.att, notice: notice, mode: ModePrompt,
		press: &pressRef{appID: in.ApplicationID, token: in.Token, replyID: reply}}
	a.obs.Decide(Decision{Event: "press", Guild: w.guild, Channel: w.channel, Message: w.original, Mode: ModePrompt, Outcome: "accepted"})
	return ephemeral(LineUploading), func() {
		if !a.enqueue(j) {
			a.goTracked(func() {
				a.say(j, LineBusy)
				a.log.Warn("discord auto: queue full; press dropped", "guild_id", j.guild, "event", "press", "outcome", "dropped")
				_, _ = a.deleteMessage(a.ctx, j.channel, reply)
			})
		}
	}
}

// PressIgnored is what the §30 endpoint answers for a press Press ignored: a
// deferred update that changes nothing.
func PressIgnored() map[string]any { return map[string]any{"type": callbackDeferUpdate} }

// queueFull reports whether a server's queue has no room.
func (a *Auto) queueFull(guild string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	q, ok := a.queues[guild]
	return ok && len(q.ch) >= cap(q.ch)
}

// onSocketInteraction is the socket's door: answer through the callback
// route, then start.
func (a *Auto) onSocketInteraction(ctx context.Context, d json.RawMessage) {
	var in struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if !IsPress(d) || json.Unmarshal(d, &in) != nil || in.ID == "" || in.Token == "" {
		return
	}
	answer, start := a.Press(ctx, d)
	if answer == nil {
		return // ignored: not even acknowledged
	}
	resp, err := a.callback(ctx, in.ID, in.Token, answer)
	if err != nil || !ok2xx(resp) {
		return // not delivered: nothing uploads
	}
	if start != nil {
		start()
	}
}

func ephemeral(line string) map[string]any {
	return map[string]any{"type": callbackMessage, "data": map[string]any{
		"content": line, "flags": flagEphemeral, "allowed_mentions": noMentions}}
}

// hasManage: Administrator or Manage Server in the guild-computed bitfield.
func hasManage(perms string) bool {
	if perms == "" || len(perms) > 30 {
		return false
	}
	p, ok := new(big.Int).SetString(perms, 10)
	if !ok || p.Sign() < 0 {
		return false
	}
	return p.Bit(3) == 1 || p.Bit(5) == 1
}

// ---------------------------------------------------------------------------
// The queue: one upload at a time per server, at most QueueCap waiting.

func (a *Auto) enqueue(j job) bool {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return false
	}
	q, ok := a.queues[j.guild]
	if !ok {
		q = &guildQueue{ch: make(chan job, a.queueCap)}
		a.queues[j.guild] = q
	}
	select {
	case q.ch <- j:
	default:
		a.mu.Unlock()
		return false
	}
	if !ok {
		a.wg.Add(1)
		go a.drain(j.guild, q)
	}
	a.queued++
	n := a.queued
	a.mu.Unlock()
	a.obs.QueueDepth(n)
	return true
}

// drain runs a server's jobs one at a time and leaves when the queue is empty
// (under the lock enqueue takes, so no job is stranded).
func (a *Auto) drain(guild string, q *guildQueue) {
	defer a.wg.Done()
	for {
		a.mu.Lock()
		var j job
		select {
		case j = <-q.ch:
		default:
			delete(a.queues, guild)
			a.mu.Unlock()
			return
		}
		a.queued--
		n := a.queued
		a.mu.Unlock()
		a.obs.QueueDepth(n)
		if a.ctx.Err() != nil {
			continue // shutting down: the rest are dropped
		}
		a.runRecovered(j)
	}
}

// runRecovered is one job with goTracked's recover: a panic in an upload ends
// that job, not the process, and the server's queue goes on draining.
func (a *Auto) runRecovered(j job) {
	defer func() {
		if p := recover(); p != nil {
			a.log.Error("discord auto: a task panicked", "guild_id", j.guild, "outcome", "panic")
		}
	}()
	a.run(j)
}

// run is one upload, then — only on a link — the link posted, the original
// deleted, and (Prompt) the reply deleted: always in that order, each once.
func (a *Auto) run(j job) {
	// The act's save OFF the queue: a Save may take its 5 s against the plane,
	// and the server's next upload must not wait behind it (the run 15a nit sweep, N2).
	// Refused once Shutdown began, which saves what changed itself.
	defer a.goTracked(a.savedAfterAct)
	ctx := a.ctx
	event := "auto"
	if j.press != nil {
		event = "press"
	}
	host := a.hostOf()
	started := a.now()
	link, err := host.Upload(ctx, j.guild, j.att)
	took := a.now().Sub(started)
	decide := func(outcome string) {
		a.obs.Decide(Decision{Event: "link", Guild: j.guild, Channel: j.channel, Message: j.original, Mode: j.mode,
			Outcome: outcome, Took: took})
	}
	if err != nil {
		if j.press != nil {
			line := LineFailed
			if l, ok := err.(Liner); ok && l.Line() != "" {
				line = l.Line()
			}
			a.say(j, line)
			_, _ = a.deleteMessage(ctx, j.channel, j.press.replyID)
		}
		a.log.Warn("discord auto: upload failed", "guild_id", j.guild, "event", event, "outcome", "upload_failed")
		decide("upload_failed")
		return
	}
	content := link
	if j.notice {
		content += "\n-# " + LineNotice
	}
	resp, err := a.createMessage(ctx, j.channel, map[string]any{"content": content, "allowed_mentions": noMentions})
	if err != nil || !ok2xx(resp) {
		// The original stays: it is never removed before the link is public.
		if j.press != nil {
			a.say(j, link)
		}
		a.log.Warn("discord auto: link not posted", "guild_id", j.guild, "event", event, "outcome", "post_"+outcomeOf(resp, err))
		decide("post_" + outcomeOf(resp, err))
		return
	}
	outcome := "linked"
	del, err := a.deleteMessage(ctx, j.channel, j.original)
	switch {
	case err == nil && (ok2xx(del) || del.Status == http.StatusNotFound):
	case err == nil && del.Status == http.StatusForbidden:
		outcome = "no_manage_messages"
		if j.press != nil {
			a.say(j, LineNeedsManage)
		} else if a.permLineDue(j.guild) {
			_, _ = a.createMessage(ctx, j.channel, map[string]any{"content": LineNeedsManage, "allowed_mentions": noMentions})
		}
	default:
		outcome = "delete_" + outcomeOf(del, err)
	}
	if j.press != nil {
		_, _ = a.deleteMessage(ctx, j.channel, j.press.replyID)
	}
	a.log.Info("discord auto: link", "guild_id", j.guild, "event", event, "outcome", outcome)
	decide(outcome)
}

// say tells the presser one line, privately (a follow-up on the press's token).
func (a *Auto) say(j job, line string) {
	if j.press == nil {
		return
	}
	_, _ = a.followup(a.ctx, j.press.appID, j.press.token, map[string]any{
		"content": line, "flags": flagEphemeral, "allowed_mentions": noMentions})
}

// permLineDue reports (and records) whether Auto may say its Manage Messages
// line in this server now: once per day.
func (a *Auto) permLineDue(guild string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if last, ok := a.permLine[guild]; ok && now.Sub(last) < permLineEvery {
		return false
	}
	a.permLine[guild] = now
	return true
}

// outcomeOf is a REST result as one log word.
func outcomeOf(r Response, err error) string {
	if err != nil {
		return "error"
	}
	switch {
	case ok2xx(r):
		return "ok"
	case r.Status == 0:
		return "none"
	}
	return "http_" + itoa(r.Status)
}

func itoa(n int) string { return strconv.Itoa(n) }
