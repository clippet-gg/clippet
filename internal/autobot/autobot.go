// Package autobot is Clippet Auto ON THE BOX (DESIGN.md §20 erratum 333,
// DEPLOY.md §30.10; run 14 lane `autobox`): a Discord gateway client for an
// application the operator made, and the three modes the owner designed on
// 2026-10-11 — Off (nothing runs), Prompt (one reply, one button,
// `Upload to Clippet`, gone after N seconds unless pressed) and Auto (no reply:
// every applicable video becomes a link and the original is removed).
//
// It is written to be usable by a hosted process too — and since §20 erratum
// 334 it is: cmd/clippet-auto runs it for every server that turned Auto on in
// the official bot's `/settings` (ModeGuild, a per-server mode from the Host;
// the session kept OFF the machine through a SessionStore so a restart
// RESUMEs; a backfill after a fresh IDENTIFY; an Observer for its metrics and
// its one-line-per-decision log). It imports nothing of this repository: no
// handler, no store, no config. Everything it needs comes in through four
// seams —
//
//	Dialer  opens the gateway socket            (internal/notify in production)
//	Caller  makes one Discord REST call         (internal/notify in production)
//	Host    "is this server bound", "would the box take this file", "upload it"
//	        (internal/httpapi in production: the binding, autofetch's rule, the
//	        SAME in-process path POST /in/auto/{token} runs)
//	clock   time, sleeps, timers and jitter     (tests drive their own)
//
// — and internal/notify stays the one package that builds a network client
// (§15): this package never dials, never resolves and never builds a request.
//
// What it reads, in the privacy order of the Worker it replaces
// (clippet-pass/rescue, R2): a dispatch that is not MESSAGE_CREATE or
// INTERACTION_CREATE is dropped unread; of the two, the guild id is read
// first, and a guild NOT BOUND to an account on this instance is dropped with
// nothing else read and nothing logged. Nothing about a message is stored: a
// Prompt reply's few facts (the channel, the original's id, its poster's id
// and its attachment) live in memory until the reply is pressed or expires.
// Log lines carry an event word, the guild id and an outcome — never a token,
// a URL, a file name, a channel or message id, or any content. An Observer
// (the hosted process's, erratum 334) is handed a Decision per act — the
// guild, the channel and the original's id, an outcome and its duration — and
// never a token, a URL, a file name or any content either.
package autobot

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Mode words, as DISCORD_AUTO_MODE spells them.
const (
	ModeOff    = "off"
	ModePrompt = "prompt"
	ModeAuto   = "auto"
	// ModeGuild is the hosted process's (§20 erratum 334): every server has a
	// mode of its own — Off, Prompt or Auto, its seconds and its notice —
	// answered by the Host, which must implement GuildSettings.
	ModeGuild = "guild"
)

// Settings is one server's Clippet Auto, as the hosted plane holds it.
type Settings struct {
	Mode    string // off | prompt | auto
	Seconds int    // Prompt's reply lifetime, 5-60 (10 when out of range)
	Notice  bool
	// Skip is the posters this server's Auto ignores — the members who route
	// their uploads to their own box (§20 erratum 334, amended 2026-10-19):
	// Discord user ids; nil = nobody.
	Skip map[string]bool
}

// GuildSettings is the Host's per-server answer, asked under ModeGuild after
// Bound. An error is "off" for that message.
type GuildSettings interface {
	Settings(ctx context.Context, guildID string) (Settings, error)
}

// Session is what a RESUME needs, kept off the machine by a SessionStore so a
// restarted (or standby) process RESUMEs instead of IDENTIFYing — Discord's
// budget is 1,000 a day, and a RESUME replays every event of the gap. Last is
// the newest message id this process saw: the backfill's starting point.
// UserID and AppID are READY's bot user and application: a process that
// starts by RESUME never sees a READY, and a press is acted on only for a
// reply authored by that user of that application (§20 erratum 334, the
// 2026-10-12 amendment).
type Session struct {
	ID        string `json:"session_id"`
	Seq       int64  `json:"seq"`
	ResumeURL string `json:"resume_gateway_url"`
	Last      string `json:"last,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	AppID     string `json:"app_id,omitempty"`
}

// SessionStore keeps the Session. Load answers the zero Session when there is
// none. Save is called at most once per SaveEvery, on a change; after a
// failure it backs off (SaveEvery doubling, at most 300 × SaveEvery), and a
// Save that answers ErrSessionRefused is not tried again until the session
// changes.
type SessionStore interface {
	Load(ctx context.Context) (Session, error)
	Save(ctx context.Context, s Session) error
}

// ErrSessionRefused is a Save error meaning the store refused this session as
// it is (a 4xx other than 429): asking again with the same session would only
// be refused again.
var ErrSessionRefused = constError("the session store refused the session")

// GatewayEvent is one moment of the socket's life, for an Observer: kind is
// identify | resume | up | down | reconnect | beat | ack (RTT set on ack).
type GatewayEvent struct {
	Kind string
	RTT  time.Duration
}

// Decision is one act, for an Observer: Event is video | offer | press | link
// | backfill; Mode the server's; Took the upload's duration on a link. Skip is
// why a video was ignored (`person`: its poster routes to his own box) — such
// a decision is a debug line that counts nothing; never the poster's id.
type Decision struct {
	Event, Guild, Channel, Message, Mode, Outcome string
	Took                                          time.Duration
	Skip                                          string
}

// Observer is told what happens; it must not block. nil = nobody.
type Observer interface {
	Gateway(e GatewayEvent)
	Decide(d Decision)
	RateLimited()
	QueueDepth(n int)
}

type nopObserver struct{}

func (nopObserver) Gateway(GatewayEvent) {}
func (nopObserver) Decide(Decision)      {}
func (nopObserver) RateLimited()         {}
func (nopObserver) QueueDepth(int)       {}

// Conn is one gateway WebSocket as this package needs it: text frames in and
// out, and a close with a code. A Read error that carries Discord's close code
// implements CloseCoder.
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, p []byte) error
	Close(code int, reason string) error
}

// CloseCoder is a Read error that names the close code the peer sent.
type CloseCoder interface{ CloseCode() int }

// Dialer opens a gateway socket to rawURL (already checked by AllowGateway).
type Dialer func(ctx context.Context, rawURL string) (Conn, error)

// Response is one REST answer: the status, the headers (the rate-limit ones
// are read) and the body.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Caller makes ONE REST call on Discord's API: method, a path under the
// versioned prefix (`/channels/…`), an optional JSON body, and whether the
// bot token goes in Authorization (an interaction callback and a follow-up
// carry none). No retries: rest.go owns the rate limits.
type Caller func(ctx context.Context, method, path string, body []byte, auth bool) (Response, error)

// Attachment is one file on a posted message, as Discord describes it.
type Attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// Host is the instance side.
type Host interface {
	// Bound reports whether the guild is bound to an account on this instance
	// (`admin bot bind`, or the official bot's claim). Only those act.
	Bound(ctx context.Context, guildID string) (bool, error)
	// Applicable is POST /in/auto's own rule: the box would take this file.
	Applicable(a Attachment) bool
	// Upload runs the upload as the bound account and answers the share link.
	// An error carrying a person-readable line implements Liner.
	Upload(ctx context.Context, guildID string, a Attachment) (link string, err error)
}

// Scoped is a Host whose binding may be ONE person's (`admin bot bind
// --person`, §20 erratum 342; erratum 333 amended 2026-10-19): Person answers
// whether the guild's binding is person-scoped and, if so, the bound account's
// linked Discord user id. Auto then acts only on that poster's videos — every
// other poster is ignored as an unbound server is — and a person binding with
// no linked id, or an error, acts for nobody. A Host without it (the hosted
// plane's) is server-scoped everywhere. An allow-one, never Settings.Skip.
type Scoped interface {
	Person(ctx context.Context, guildID string) (person bool, discordID string, err error)
}

// personAllows reports whether Auto may act on poster's video in guild.
func personAllows(ctx context.Context, h Host, guild, poster string) bool {
	s, ok := h.(Scoped)
	if !ok {
		return true
	}
	person, id, err := s.Person(ctx, guild)
	if err != nil {
		return false
	}
	return !person || (id != "" && poster == id)
}

// Liner is an Upload error with a sentence a presser may read.
type Liner interface{ Line() string }

// Config builds an Auto.
type Config struct {
	Mode    string // prompt | auto; anything else is off and New answers nil
	Seconds int    // Prompt's reply lifetime (5-60; 10 when 0)
	Notice  bool   // the line under each link
	Log     *slog.Logger

	Dial         Dialer
	Call         Caller
	AllowGateway func(string) (*url.URL, error)

	// The hosted process's (§20 erratum 334); zero = the box's behaviour.
	Sessions SessionStore // the session kept off the machine; shutdown then keeps it resumable
	Observer Observer
	Backfill bool // after a fresh IDENTIFY, read back each bound server's last minutes

	// Tests only (zero = production).
	SaveEvery      time.Duration // the session's write debounce (1 s)
	BackfillWindow time.Duration // how far back a backfill reads (10 min)
	Now            func() time.Time
	Sleep          func(ctx context.Context, d time.Duration) error
	Jitter         func() float64 // [0,1)
	AfterFunc      func(d time.Duration, f func()) (stop func() bool)
	IdentifyGap    time.Duration // the floor between two IDENTIFYs (5 s)
	IdentifyCap    int           // IDENTIFYs in any 24 h (500; Discord resets the token past 1000)
	HelloWait      time.Duration // a socket with no HELLO by then is dropped (20 s)
	QueueCap       int           // per guild (10)
}

// The defaults.
const (
	defaultSeconds     = 10
	defaultIdentifyGap = 5 * time.Second
	defaultHelloWait   = 20 * time.Second
	defaultQueueCap    = 10
	defaultIdentifyCap = 500
	identifyWindow     = 24 * time.Hour
	// stableAfter is how long a session must live after its socket opened
	// before it counts as healthy and resets the backoff: a session that
	// reached READY and died at once is a failure like any other.
	stableAfter    = 60 * time.Second
	backoffFloor   = time.Second
	backoffCeiling = 5 * time.Minute
	// The hosted process's (§20 erratum 334).
	defaultSaveEvery      = time.Second
	saveBackoffSteps      = 300 // a failing Save backs off to 300 × SaveEvery (5 min) at most
	defaultBackfillWindow = 10 * time.Minute
	// defaultCloseWait bounds how long Shutdown waits for the socket's close.
	defaultCloseWait = 2 * time.Second
	// minHeartbeat floors HELLO's heartbeat_interval: a 1 ms interval would
	// beat at 1 kHz.
	minHeartbeat       = time.Second
	backfillPerChannel = 50
	backfillChannels   = 100             // per server
	backfillAfterReady = 2 * time.Minute // a GUILD_CREATE this long after a fresh READY still backfills
	seenKeep           = 15 * time.Minute
	seenCap            = 5000
)

// Auto is one running Clippet Auto.
type Auto struct {
	mode    string
	seconds time.Duration
	notice  bool
	log     *slog.Logger
	dial    Dialer
	allowGW func(string) (*url.URL, error)
	rest    *rest

	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
	jitter    func() float64
	afterFunc func(d time.Duration, f func()) (stop func() bool)
	idGap     time.Duration
	idCap     int
	helloWait time.Duration
	queueCap  int

	hostMu sync.RWMutex
	host   Host

	// ctx bounds every goroutine Auto starts; Shutdown cancels and waits.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	closed    bool
	halted    string // a non-reconnectable reason; nothing reconnects once set
	selfID    string // the bot user's id, from READY
	appID     string // the application's id, from READY
	waiting   map[string]*waiting
	queues    map[string]*guildQueue
	permLine  map[string]time.Time // Auto's Manage Messages line, per guild per day
	lastIdent time.Time
	idents    []time.Time // the IDENTIFYs sent in the last identifyWindow
	session   sessionState

	// The hosted process's (§20 erratum 334).
	sessions     SessionStore
	obs          Observer
	backfill     bool
	saveEvery    time.Duration
	bfWindow     time.Duration
	dirty        bool                 // the session changed since the last Save
	saveFails    int                  // Saves failed in a row (the backoff's step)
	saveRetryAt  time.Time            // after a failed Save: not tried again before this
	saveRefused  bool                 // the last Save was refused (ErrSessionRefused); cleared by a change
	saved        Session              // what the last successful Save wrote (savedOK): a final save of the same is skipped
	savedOK      bool                 // saved is set
	last         string               // the newest message id seen
	seen         map[string]time.Time // message ids acted on lately (a backfill never repeats a live one)
	bfAfter      string               // the snowflake a pending backfill reads after
	bfReady      time.Time            // when the fresh session's READY came ("" bfAfter: none pending)
	bfDone       map[string]bool      // servers already backfilled for this session
	queued       int                  // jobs waiting, every server
	beatSentNano atomic.Int64

	haltCh    chan struct{} // closed by the first halt: Run's socket ends at once
	connDone  chan struct{} // a socket being opened or open; closed once its close was sent
	closeWait time.Duration // how long Shutdown waits for that close (2 s)
	saveMu    sync.Mutex    // one Save at a time
	lastSave  time.Time     // when the last Save started: one a SaveEvery at most
}

// New returns an Auto, or nil when the mode is off — then nothing is built,
// no goroutine starts and no socket is ever opened.
func New(c Config) *Auto {
	if c.Mode != ModePrompt && c.Mode != ModeAuto && c.Mode != ModeGuild {
		return nil
	}
	a := &Auto{
		mode: c.Mode, notice: c.Notice, log: c.Log, dial: c.Dial, allowGW: c.AllowGateway,
		now: c.Now, sleep: c.Sleep, jitter: c.Jitter, afterFunc: c.AfterFunc,
		idGap: c.IdentifyGap, idCap: c.IdentifyCap, helloWait: c.HelloWait, queueCap: c.QueueCap,
		waiting: map[string]*waiting{}, queues: map[string]*guildQueue{}, permLine: map[string]time.Time{},
		sessions: c.Sessions, obs: c.Observer, backfill: c.Backfill, saveEvery: c.SaveEvery, bfWindow: c.BackfillWindow,
		seen: map[string]time.Time{}, bfDone: map[string]bool{},
		haltCh: make(chan struct{}), closeWait: defaultCloseWait,
	}
	if a.obs == nil {
		a.obs = nopObserver{}
	}
	if a.saveEvery <= 0 {
		a.saveEvery = defaultSaveEvery
	}
	if a.bfWindow <= 0 {
		a.bfWindow = defaultBackfillWindow
	}
	secs := c.Seconds
	if secs <= 0 {
		secs = defaultSeconds
	}
	a.seconds = time.Duration(secs) * time.Second
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.sleep == nil {
		a.sleep = sleepCtx
	}
	if a.jitter == nil {
		a.jitter = rand.Float64
	}
	if a.afterFunc == nil {
		a.afterFunc = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	if a.idGap <= 0 {
		a.idGap = defaultIdentifyGap
	}
	if a.idCap <= 0 {
		a.idCap = defaultIdentifyCap
	}
	if a.helloWait <= 0 {
		a.helloWait = defaultHelloWait
	}
	if a.queueCap <= 0 {
		a.queueCap = defaultQueueCap
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.rest = &rest{call: c.Call, now: a.now, sleep: a.sleep, buckets: map[string]*bucket{}, routes: map[string]string{},
		onUnauthorized: func() { a.halt("rest_401") }, onRateLimited: func() { a.obs.RateLimited() }}
	return a
}

// SetHost hands in the instance side. It is called once, before Run (the
// handler is built after the Auto, exactly as the drop folder's watcher is).
func (a *Auto) SetHost(h Host) {
	a.hostMu.Lock()
	a.host = h
	a.hostMu.Unlock()
}

func (a *Auto) hostOf() Host {
	a.hostMu.RLock()
	defer a.hostMu.RUnlock()
	return a.host
}

// Mode is the running mode (prompt or auto).
func (a *Auto) Mode() string { return a.mode }

// Shutdown stops the socket and every job, bounded by ctx. The socket is
// closed with 1000 (a clean close) by Run's own unwinding, and Shutdown waits
// for that close to be sent (closeWait, 2 s, at most). With a SessionStore,
// the session — `last` included, which moves as a video is SEEN — is saved
// FIRST, synchronously and whatever the debounce, before Shutdown waits for a
// job: a stop while an upload runs past ctx (the hosted process exits when
// Shutdown returns) never leaves the last second unsaved for the next start's
// backfill to act on again. It is saved once more after the jobs only if it
// changed since; Run's own last save (flushLast) skips a session already
// saved, so a stop writes it exactly once when nothing moved.
func (a *Auto) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	a.closed = true
	for id, w := range a.waiting {
		w.stop()
		delete(a.waiting, id)
	}
	sock := a.connDone
	a.mu.Unlock()
	a.cancel()
	if a.sessions != nil {
		a.flush(ctx, flushFinal)
	}
	done := make(chan struct{})
	go func() { defer close(done); a.wg.Wait() }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if sock != nil {
		t := time.NewTimer(a.closeWait)
		select {
		case <-sock:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
	}
	if a.sessions != nil {
		a.flush(ctx, flushFinal)
	}
	return ctx.Err()
}

// goTracked starts f under the WaitGroup unless Shutdown has begun.
func (a *Auto) goTracked(f func()) bool {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return false
	}
	a.wg.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				a.log.Error("discord auto: a task panicked", "outcome", "panic")
			}
		}()
		f()
	}()
	return true
}

// halt stops the gateway for good (until a restart): a close code Discord
// marks non-reconnectable, or a REST 401 on the bot token.
func (a *Auto) halt(reason string) {
	a.mu.Lock()
	first := a.halted == ""
	if first {
		a.halted = reason
		close(a.haltCh) // Run ends its socket now, not at its next loop
	}
	a.mu.Unlock()
	if first {
		a.log.Error("discord auto: halted; restart the instance after fixing the cause (DEPLOY.md §30.10)", "outcome", reason)
	}
}

// Halted is the reason the gateway stopped for good, or "".
func (a *Auto) Halted() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.halted
}

func (a *Auto) identity() (selfID, appID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.selfID, a.appID
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // every value marshalled here is a literal of this package
	}
	return b
}
