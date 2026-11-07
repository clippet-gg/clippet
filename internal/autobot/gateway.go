package autobot

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// The gateway (Discord's "Gateway" page, API v10, JSON, no compression):
//
//	GET /gateway/bot              the URL and the IDENTIFY budget (logged)
//	dial                          `?v=10&encoding=json`
//	HELLO (op 10)                 the heartbeat interval, 1 s at least; none within
//	                              HelloWait → dropped
//	IDENTIFY (op 2) | RESUME (6)  RESUME whenever a session is held
//	heartbeat (op 1)              the first after interval × jitter, then every
//	                              interval; a beat with the last one un-ACKed (op
//	                              11) is a zombie: closed and RESUMEd
//	op 1 from Discord             a beat at once
//	op 7 RECONNECT                closed (4000, so the session survives) and RESUMEd at once
//	op 9 INVALID SESSION          1-5 s, then RESUME when `d` is true, else IDENTIFY
//	a dropped socket              RESUMEd, with the backoff below
//	4004 4010 4011 4012 4013 4014 halted: Discord marks these non-reconnectable
//	                              (4014 = the Message Content intent is not on)
//	a REST 401 on the bot token   halted, and the socket closed at once
//	shutdown                     closed with 1000 — or, with a SessionStore (the
//	                              hosted process, erratum 334), closed 4000 and the
//	                              session saved, so the next start RESUMEs
//
// Never more than one IDENTIFY per IdentifyGap (5 s), and never more than
// IdentifyCap (500) in any 24 h — past it the client waits for the window to
// roll; Discord resets the bot token past 1000. The budget from /gateway/bot
// is read before the first IDENTIFY and before every IDENTIFY that follows a
// session that did not hold; it is logged and, at zero, waited out. Failures
// back off 1 s → 5 min, doubling; an op 9 or op 7 on a session that did not
// hold is a failure too. A session holds when it reached READY or RESUMED and
// lived 60 s; only that resets the backoff.

// Opcodes.
const (
	opDispatch     = 0
	opHeartbeat    = 1
	opIdentify     = 2
	opResume       = 6
	opReconnect    = 7
	opInvalid      = 9
	opHello        = 10
	opHeartbeatAck = 11
)

// Intents is GUILDS | GUILD_MESSAGES | MESSAGE_CONTENT: the server's channels
// exist, a message was posted, and its attachments are readable. Message
// Content is the one privileged intent and must be ON in the portal.
const Intents = 1<<0 | 1<<9 | 1<<15

// The close codes Discord marks non-reconnectable.
var fatalClose = map[int]string{
	4004: "authentication_failed",
	4010: "invalid_shard",
	4011: "sharding_required",
	4012: "invalid_api_version",
	4013: "invalid_intents",
	4014: "disallowed_intents",
}

// Close codes that end the session but not the bot: identify afresh.
var sessionGone = map[int]bool{4007: true, 4009: true}

// closeResumable is the code this client closes with when it means to RESUME:
// a 1000 or 1001 would end the session on Discord's side.
const closeResumable = 4000

type sessionState struct {
	id        string
	resumeURL string
	seq       int64
	hasSeq    bool
}

type frame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

// Run holds the gateway until ctx ends, Shutdown is called, or the bot is
// halted. It is the ONE place a socket is opened.
func (a *Auto) Run(ctx context.Context, token string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-a.ctx.Done():
			cancel()
		case <-a.haltCh: // a REST 401 halts the socket at once
			cancel()
		case <-ctx.Done():
		}
	}()
	if a.sessions != nil {
		a.loadSession(ctx)
		a.fetchIdentity(ctx)
		saved := make(chan struct{})
		go func() { defer close(saved); a.saveLoop(ctx) }()
		// Run's ctx ends first, so the loop's last save runs and is waited.
		defer func() { cancel(); <-saved }()
	}
	failures := 0
	openFails := 0
	sockets := 0
	gwURL := ""
	backoff := func() bool {
		failures++
		d := backoffFloor << min(failures-1, 20)
		if d > backoffCeiling {
			d = backoffCeiling
		}
		return a.sleep(ctx, d) == nil
	}
	for ctx.Err() == nil && a.Halted() == "" {
		sess := a.sessionSnapshot()
		target := sess.resumeURL
		if !sess.resumes() {
			// This socket will IDENTIFY: the daily ceiling first.
			if w := a.identifyCapWait(); w > 0 {
				a.log.Warn("discord auto: daily IDENTIFY ceiling reached; waiting", "outcome", "identify_budget",
					"ceiling", a.idCap, "wait", w.Round(time.Second).String())
				if a.sleep(ctx, w) != nil {
					return
				}
				continue
			}
		}
		if sess.id == "" || target == "" {
			if gwURL == "" {
				info, err := a.gatewayBot(ctx)
				if err != nil {
					a.log.Warn("discord auto: gateway lookup failed", "outcome", "gateway_bot")
					if a.Halted() != "" || !backoff() {
						return
					}
					continue
				}
				l := info.SessionStartLimit
				a.log.Info("discord auto: identify budget", "remaining", l.Remaining, "total", l.Total,
					"reset_after", (time.Duration(l.ResetAfter) * time.Millisecond).Round(time.Second).String())
				if l.Total > 0 && l.Remaining <= 0 {
					a.log.Warn("discord auto: identify budget spent; waiting for Discord's reset", "outcome", "budget")
					if a.sleep(ctx, time.Duration(l.ResetAfter)*time.Millisecond) != nil {
						return
					}
					continue
				}
				gwURL = info.URL
			}
			target = gwURL
		}
		u, err := a.allowGW(target)
		if err != nil {
			if sess.id != "" && target == sess.resumeURL {
				a.forgetSession()
				continue
			}
			a.halt("gateway_host")
			return
		}
		dialURL := u.Scheme + "://" + u.Host + "/?v=10&encoding=json"
		sock, open := a.socketOpening()
		if !open {
			return // Shutdown has begun
		}
		conn, err := a.dial(ctx, dialURL)
		if err != nil {
			a.socketClosed(sock)
			if ctx.Err() != nil {
				return
			}
			openFails++
			a.log.Warn("discord auto: gateway did not open", "outcome", "dial")
			if openFails >= 3 {
				// Three failed opens in a row: the resume URL may be the cause.
				a.forgetSession()
				gwURL = ""
				openFails = 0
			}
			if !backoff() {
				return
			}
			continue
		}
		openFails = 0
		sockets++
		if sockets > 1 {
			a.obs.Gateway(GatewayEvent{Kind: "reconnect"})
		}
		opened := a.now()
		res := a.runConn(ctx, conn, token) // the socket is closed when it returns
		a.socketClosed(sock)
		// A session held only when it reached READY or RESUMED AND lived
		// stableAfter: one that dies at once (op 9 after READY, a drop) is a
		// failure, so a loop of them backs off to one cycle per 5 min instead
		// of one per IDENTIFY gap — Discord resets the token past 1000
		// IDENTIFYs a day.
		held := res.healthy && a.now().Sub(opened) >= stableAfter
		if held {
			failures = 0
		} else {
			// Re-read /gateway/bot (and its IDENTIFY budget) before the next
			// IDENTIFY.
			gwURL = ""
		}
		switch res.kind {
		case endShutdown:
			return
		case endFatal:
			a.halt(res.reason)
			return
		case endReconnect:
			a.log.Info("discord auto: reconnect", "outcome", res.reason)
			if !held && !backoff() {
				return
			}
		case endInvalid:
			if !res.resumable {
				a.forgetSession()
			}
			// Discord's own instruction: a random 1-5 s.
			if a.sleep(ctx, time.Second+time.Duration(a.jitter()*float64(4*time.Second))) != nil {
				return
			}
			if !held && !backoff() {
				return
			}
		default:
			a.log.Warn("discord auto: gateway dropped", "outcome", res.reason)
			if res.forget {
				a.forgetSession()
			}
			if !backoff() {
				return
			}
		}
	}
}

// socketOpening marks a socket about to be dialled, so Shutdown can wait for
// its close; false once Shutdown has begun (no socket is opened after it).
func (a *Auto) socketOpening() (chan struct{}, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, false
	}
	ch := make(chan struct{})
	a.connDone = ch
	return ch, true
}

// socketClosed says the socket of socketOpening is closed (or never opened).
func (a *Auto) socketClosed(ch chan struct{}) {
	a.mu.Lock()
	if a.connDone == ch {
		a.connDone = nil
	}
	a.mu.Unlock()
	close(ch)
}

type endKind int

const (
	endDropped endKind = iota
	endShutdown
	endFatal
	endReconnect
	endInvalid
)

type connEnd struct {
	kind      endKind
	reason    string
	healthy   bool // READY or RESUMED was seen on this socket
	resumable bool // op 9's `d`
	forget    bool // the session is gone on Discord's side
}

// runConn drives one socket from HELLO to its end.
func (a *Auto) runConn(parent context.Context, conn Conn, token string) (end connEnd) {
	ctx, cancel := context.WithCancel(parent)
	var wmu sync.Mutex
	write := func(v any) error {
		wmu.Lock()
		defer wmu.Unlock()
		return conn.Write(ctx, mustJSON(v))
	}
	var hbWG sync.WaitGroup
	var zombie atomic.Bool
	defer func() {
		cancel()
		hbWG.Wait()
		a.obs.Gateway(GatewayEvent{Kind: "down"})
		switch {
		case end.kind == endShutdown && a.sessions == nil:
			_ = conn.Close(1000, "")
		default:
			// With a SessionStore a shutdown is a restart: the session is
			// kept on Discord's side for the next start's RESUME.
			_ = conn.Close(closeResumable, "")
		}
	}()

	// HELLO, within its deadline.
	hctx, hcancel := context.WithTimeout(ctx, a.helloWait)
	raw, err := conn.Read(hctx)
	hcancel()
	if err != nil {
		return a.readEnd(parent, err, false)
	}
	var hello frame
	var hd struct {
		Interval int64 `json:"heartbeat_interval"`
	}
	if json.Unmarshal(raw, &hello) != nil || hello.Op != opHello || json.Unmarshal(hello.D, &hd) != nil || hd.Interval <= 0 {
		return connEnd{kind: endDropped, reason: "no_hello"}
	}
	interval := max(time.Duration(hd.Interval)*time.Millisecond, minHeartbeat)

	var acked atomic.Bool
	acked.Store(true)
	beat := func() error {
		s := a.sessionSnapshot()
		var d any
		if s.hasSeq {
			d = s.seq
		}
		a.beatSentNano.Store(time.Now().UnixNano())
		a.obs.Gateway(GatewayEvent{Kind: "beat"})
		return write(map[string]any{"op": opHeartbeat, "d": d})
	}
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		wait := time.Duration(a.jitter() * float64(interval))
		for {
			// Real time always: the beat is Discord's clock, not the
			// backoff's, and a test's fake clock must not spin it.
			if sleepCtx(ctx, wait) != nil {
				return
			}
			if !acked.Load() {
				zombie.Store(true)
				cancel() // the read below ends; the socket is closed resumable
				return
			}
			acked.Store(false)
			if beat() != nil {
				return
			}
			wait = interval
		}
	}()

	// IDENTIFY or RESUME.
	sess := a.sessionSnapshot()
	fresh := !sess.resumes()
	if !fresh {
		err = write(map[string]any{"op": opResume, "d": map[string]any{
			"token": token, "session_id": sess.id, "seq": sess.seq}})
		a.obs.Gateway(GatewayEvent{Kind: "resume"})
	} else {
		if a.waitIdentify(ctx) != nil {
			return a.readEnd(parent, ctx.Err(), false)
		}
		err = write(map[string]any{"op": opIdentify, "d": map[string]any{
			"token":      token,
			"intents":    Intents,
			"properties": map[string]string{"os": "linux", "browser": "clippet", "device": "clippet"},
		}})
		a.obs.Gateway(GatewayEvent{Kind: "identify"})
	}
	if err != nil {
		return a.readEnd(parent, err, false)
	}

	healthy := false
	for {
		raw, err := conn.Read(ctx)
		if err != nil {
			if zombie.Load() && parent.Err() == nil {
				return connEnd{kind: endDropped, reason: "zombie", healthy: healthy}
			}
			return a.readEnd(parent, err, healthy)
		}
		var f frame
		if json.Unmarshal(raw, &f) != nil {
			continue
		}
		if f.S != nil {
			a.setSeq(*f.S)
		}
		switch f.Op {
		case opDispatch:
			switch f.T {
			case "READY":
				a.onReady(f.D)
				healthy = true
				if fresh {
					a.armBackfill()
				}
				a.obs.Gateway(GatewayEvent{Kind: "up"})
			case "RESUMED":
				healthy = true
				a.log.Info("discord auto: resumed", "outcome", "resumed")
				a.obs.Gateway(GatewayEvent{Kind: "up"})
			default:
				a.dispatch(f.T, f.D)
			}
		case opHeartbeat:
			if beat() != nil {
				return a.readEnd(parent, errors.New("write"), healthy)
			}
		case opHeartbeatAck:
			acked.Store(true)
			if sent := a.beatSentNano.Load(); sent > 0 {
				a.obs.Gateway(GatewayEvent{Kind: "ack", RTT: time.Duration(time.Now().UnixNano() - sent)})
			}
		case opReconnect:
			return connEnd{kind: endReconnect, reason: "op7", healthy: healthy}
		case opInvalid:
			var resumable bool
			_ = json.Unmarshal(f.D, &resumable)
			return connEnd{kind: endInvalid, reason: "op9", healthy: healthy, resumable: resumable}
		}
	}
}

// readEnd classifies a socket that ended under a Read or Write.
func (a *Auto) readEnd(parent context.Context, err error, healthy bool) connEnd {
	if parent.Err() != nil {
		return connEnd{kind: endShutdown, reason: "shutdown", healthy: healthy}
	}
	var cc CloseCoder
	if errors.As(err, &cc) {
		code := cc.CloseCode()
		if why, ok := fatalClose[code]; ok {
			return connEnd{kind: endFatal, reason: why, healthy: healthy}
		}
		if sessionGone[code] {
			return connEnd{kind: endDropped, reason: "session_gone", healthy: healthy, forget: true}
		}
	}
	return connEnd{kind: endDropped, reason: "dropped", healthy: healthy}
}

// waitIdentify keeps IDENTIFYs at least idGap apart, then stamps this one.
func (a *Auto) waitIdentify(ctx context.Context) error {
	a.mu.Lock()
	last := a.lastIdent
	a.mu.Unlock()
	if !last.IsZero() {
		if wait := a.idGap - a.now().Sub(last); wait > 0 {
			if err := a.sleep(ctx, wait); err != nil {
				return err
			}
		}
	}
	a.mu.Lock()
	a.lastIdent = a.now()
	a.idents = append(a.idents, a.lastIdent)
	a.mu.Unlock()
	return nil
}

// identifyCapWait is how long until one more IDENTIFY fits under the daily
// ceiling (idCap in any identifyWindow), or 0 when it fits now.
func (a *Auto) identifyCapWait() time.Duration {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	keep := a.idents[:0]
	for _, t := range a.idents {
		if now.Sub(t) < identifyWindow {
			keep = append(keep, t)
		}
	}
	a.idents = keep
	if len(a.idents) < a.idCap {
		return 0
	}
	return a.idents[len(a.idents)-a.idCap].Add(identifyWindow).Sub(now)
}

// resumes reports whether a socket opened now would RESUME (rather than
// IDENTIFY): a session and a sequence number are held.
func (s sessionState) resumes() bool { return s.id != "" && s.hasSeq }

func (a *Auto) onReady(d json.RawMessage) {
	var r struct {
		SessionID string `json:"session_id"`
		ResumeURL string `json:"resume_gateway_url"`
		User      struct {
			ID string `json:"id"`
		} `json:"user"`
		Application struct {
			ID string `json:"id"`
		} `json:"application"`
	}
	if json.Unmarshal(d, &r) != nil {
		return
	}
	a.mu.Lock()
	a.session.id = r.SessionID
	a.session.resumeURL = r.ResumeURL
	a.dirty = true
	a.selfID = r.User.ID
	a.appID = r.Application.ID
	a.mu.Unlock()
	a.log.Info("discord auto: ready", "mode", a.mode, "app_id", r.Application.ID, "outcome", "ready")
}

func (a *Auto) setSeq(s int64) {
	a.mu.Lock()
	// A new sequence number alone does not make the session dirty: every
	// event of every server moves it, and a store write per second is a KV
	// bill (§20 erratum 334). It is saved with the next change that matters —
	// READY, a video acted on — and as Run ends; a RESUME from an older one
	// only replays more, which the dedupe and the unbound rule drop.
	a.session.seq, a.session.hasSeq = s, true
	a.mu.Unlock()
}

func (a *Auto) sessionSnapshot() sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.session
}

func (a *Auto) forgetSession() {
	a.mu.Lock()
	a.session = sessionState{}
	a.dirty = true
	a.mu.Unlock()
}

// dispatch hands the two events this package reads to their handlers, each in
// a tracked goroutine so the read loop never waits on a REST call.
func (a *Auto) dispatch(t string, d json.RawMessage) {
	switch t {
	case "MESSAGE_CREATE":
		a.goTracked(func() { a.onMessage(a.ctx, d) })
	case "INTERACTION_CREATE":
		a.goTracked(func() { a.onSocketInteraction(a.ctx, d) })
	case "GUILD_CREATE":
		if a.backfillDue() {
			a.goTracked(func() { a.onGuildCreate(a.ctx, d) })
		}
	}
}
