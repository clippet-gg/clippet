package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"clippet/internal/autobot"
	"clippet/internal/autorule"
	"clippet/internal/config"
)

// The plane-backed autobot.Host (§20 erratum 334). Every server's switch lives
// in the claim plane's KV `r:<guild>` = {mode, s, n, u}, written by the
// official bot's `/settings`; this process reads it through the claim Worker's
// `GET /auto/guild/{id}` (bearer AUTO_PLANE_SECRET) and caches each answer —
// on or off — settingsTTL (60 s), so a change in `/settings` is seen within a
// minute. A plane that does not answer keeps the last answer it gave; a server
// never asked before is off until it does. A failure is remembered too, for
// planeRetry (10 s): during a plane outage every message in every server would
// otherwise ask again at once and hold a goroutine for the call's 10 s budget,
// so a failing plane is asked at most once per server per planeRetry. Since
// the 2026-10-12 amendment one GET per server is ever under way, and a server
// not on GET /auto/guilds' list (polled every 5 min, and re-read at most once a
// minute when an unlisted server speaks) is off with no GET of its own.
//
//   - Bound      the plane says Auto is on there (Prompt or Auto) with a route
//   - Settings   that answer's mode, seconds, notice and skip list
//   - Applicable POST /in/auto's own rule (internal/autorule) under
//     MAX_UPLOAD_BYTES' default; the box applies its own cap again
//   - Upload     open the sealed URL for (guild, route host), check its shape
//     against the route, POST the instruction, read the link —
//     the box fetches the file from Discord's CDN itself; no
//     video byte ever passes through here

const (
	settingsTTL = 60 * time.Second
	planeRetry  = 10 * time.Second
)

// The words a presser reads (the settled lines of run 7's one press, reused
// as written; `Busy.` and the
// others are internal/autobot's own).
const (
	lineURLGone      = "This server's Clippet Auto URL no longer works. Set it again in /settings."
	lineWrongAccount = "This server's Clippet Auto URL is from the wrong account. Set it again in /settings."
	lineTooLarge     = "That file is larger than this Clippet accepts."
	lineNotPlayable  = "That file is not a video or sound Clippet can play."
	lineNoAnswer     = "This server's Clippet did not answer."
	lineLimit        = "This server's Clippet has hit its upload limit. Try again later."
	lineFull         = "This server's Clippet is full."
	lineWorking      = "Still uploading. The link will not be posted; look in the Clippet's library."
)

// lineErr is an Upload refusal a presser may read (autobot.Liner).
type lineErr string

func (e lineErr) Error() string { return string(e) }
func (e lineErr) Line() string  { return string(e) }

// planeCaller is notify.HostedClient's plane call; boxCaller its box call.
type planeCaller interface {
	Plane(ctx context.Context, method, path string, body []byte) (int, []byte, error)
}
type boxCaller interface {
	Box(ctx context.Context, rawURL string, instruction []byte, timeout time.Duration) (int, []byte)
}

// guildRecord is GET /auto/guild/{id}'s 200. Skip (the 2026-10-19 amendment)
// is the server's members who route their uploads to their own box, as
// Discord user ids (at most 200, the plane's own ceiling); absent or empty =
// nobody skipped. Each answer REPLACES the set — skip is the set as the plane
// holds it now, never merged with an older one.
type guildRecord struct {
	On   bool     `json:"on"`
	Mode string   `json:"mode"`
	S    int      `json:"s"`
	N    bool     `json:"n"`
	U    string   `json:"u"`
	Host string   `json:"host"`
	Skip []string `json:"skip,omitempty"`

	skip map[string]bool // Skip as a set, built once per answer; nil = nobody
}

type cached struct {
	rec     *guildRecord // nil: off
	at      time.Time    // when the plane gave rec
	known   bool         // the plane has answered at least once
	retryAt time.Time    // after a failure: not asked again before this
}

type planeHost struct {
	plane      planeCaller
	box        boxCaller
	open       *opener
	m          *metrics
	log        *slog.Logger
	now        func() time.Time
	boxTimeout time.Duration // 0 = notify.BoxCallTimeout

	mu       sync.Mutex
	cache    map[string]cached
	inflight map[string]chan struct{} // a plane GET under way, per server: one at a time
	listed   map[string]bool          // GET /auto/guilds' last answer: the servers with Auto on
	listAt   time.Time                // when it came (zero: never)
	listTry  time.Time                // when it was last asked, answered or not
	listing  bool                     // a re-read for an unlisted server is under way
}

// GET /auto/guilds pre-filters Bound (§20 erratum 334, the 2026-10-12
// amendment, audit F6): a server NOT on a list younger than listFresh (three of
// its 5-minute polls) makes no plane call of its own. So that a server that
// has just turned Auto on is still seen within the minute, a message from an
// unlisted server re-reads the LIST when it was last asked listRecheck ago or
// more — one list call a minute at most for the whole process, where every
// unlisted server used to make one GET a minute of its own. An older list, or
// none, and every server is asked as before.
const (
	listFresh   = 15 * time.Minute
	listRecheck = settingsTTL
)

func newPlaneHost(plane planeCaller, box boxCaller, o *opener, m *metrics, log *slog.Logger) *planeHost {
	return &planeHost{plane: plane, box: box, open: o, m: m, log: log, now: time.Now, cache: map[string]cached{},
		inflight: map[string]chan struct{}{}}
}

var guildID = regexp.MustCompile(`^[0-9]{17,20}$`)

// record is the server's switch: nil when off.
//
// One plane GET per server at a time (audit F3): a caller that finds one
// under way takes the last answer the plane gave, or — for a server never
// answered — waits for that call's answer, so a chatty server during a plane
// outage makes one call, not one per message.
func (p *planeHost) record(ctx context.Context, guild string) (*guildRecord, error) {
	if !guildID.MatchString(guild) {
		return nil, nil
	}
	now := p.now()
	p.mu.Lock()
	c := p.cache[guild]
	switch {
	case !p.listAt.IsZero() && now.Sub(p.listAt) < listFresh && !p.listed[guild]:
		recheck := !p.listing && now.Sub(p.listTry) >= listRecheck
		if recheck {
			p.listing = true
		}
		p.mu.Unlock()
		if !recheck {
			return nil, nil
		}
		p.refreshList(ctx)
		p.mu.Lock()
		p.listing = false
		p.mu.Unlock()
		return p.record(ctx, guild) // listTry is now: no second re-read
	case c.known && now.Sub(c.at) < settingsTTL:
		p.mu.Unlock()
		return c.rec, nil
	case now.Before(c.retryAt):
		p.mu.Unlock()
		return p.stale(c)
	}
	if ch, busy := p.inflight[guild]; busy {
		p.mu.Unlock()
		if c.known {
			return c.rec, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, errPlane
		}
		p.mu.Lock()
		c = p.cache[guild]
		p.mu.Unlock()
		return p.stale(c)
	}
	ch := make(chan struct{})
	p.inflight[guild] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.inflight, guild)
		p.mu.Unlock()
		close(ch)
	}()
	status, body, err := p.plane.Plane(ctx, http.MethodGet, "/auto/guild/"+guild, nil)
	var rec *guildRecord
	switch {
	case err == nil && status == http.StatusOK:
		var r guildRecord
		if json.Unmarshal(body, &r) != nil || !r.On || (r.Mode != autobot.ModePrompt && r.Mode != autobot.ModeAuto) ||
			r.U == "" || r.Host == "" {
			return p.failed(guild, c, now, "guild_record")
		}
		if len(r.Skip) > 0 {
			r.skip = make(map[string]bool, len(r.Skip))
			for _, id := range r.Skip {
				r.skip[id] = true
			}
		}
		rec = &r
	case err == nil && status == http.StatusNotFound:
		rec = nil
	default:
		return p.failed(guild, c, now, "guild")
	}
	p.mu.Lock()
	p.cache[guild] = cached{rec: rec, at: now, known: true}
	p.mu.Unlock()
	return rec, nil
}

// failed counts a plane failure, remembers it for planeRetry (so a failing
// plane is asked at most once per server per planeRetry) and answers the last
// answer the plane gave, or an error when there is none.
func (p *planeHost) failed(guild string, c cached, now time.Time, what string) (*guildRecord, error) {
	p.planeError(what)
	c.retryAt = now.Add(planeRetry)
	p.mu.Lock()
	p.cache[guild] = c
	p.mu.Unlock()
	return p.stale(c)
}

// stale answers the last answer the plane gave, or an error when there is none.
func (p *planeHost) stale(c cached) (*guildRecord, error) {
	if c.known {
		return c.rec, nil
	}
	return nil, errPlane
}

var errPlane = lineErr("the plane did not answer")

func (p *planeHost) planeError(what string) {
	p.m.planeError()
	p.log.Warn("plane", "event", "plane", "outcome", what)
}

func (p *planeHost) Bound(ctx context.Context, guild string) (bool, error) {
	rec, err := p.record(ctx, guild)
	return rec != nil, err
}

func (p *planeHost) Settings(ctx context.Context, guild string) (autobot.Settings, error) {
	rec, err := p.record(ctx, guild)
	if err != nil || rec == nil {
		return autobot.Settings{Mode: autobot.ModeOff}, err
	}
	return autobot.Settings{Mode: rec.Mode, Seconds: rec.S, Notice: rec.N, Skip: rec.skip}, nil
}

// placeholderGuild stands in for the grammar's guild id in Applicable: the
// binding is the box's answer, not part of the rule.
const placeholderGuild = "100000000000000000"

func (p *planeHost) Applicable(a autobot.Attachment) bool {
	return autorule.Applicable(instructionOf(placeholderGuild, a), config.DefaultMaxUploadBytes)
}

// instructionOf is the attachment as POST /in/auto/{token} takes it.
func instructionOf(guild string, a autobot.Attachment) autorule.Instruction {
	return autorule.Instruction{GuildID: guild, URL: a.URL, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size}
}

// instructionBody is the bytes a box decodes (autorule.Decode, the route's
// own decoder — the pin drives it with these bytes).
func instructionBody(guild string, a autobot.Attachment) []byte {
	b, _ := json.Marshal(instructionOf(guild, a))
	return b
}

// autoPath is a Clippet Auto URL's path: /in/auto/ca_ and 43 base64url.
var autoPath = regexp.MustCompile(`^/in/auto/ca_[A-Za-z0-9_-]{43}$`)

// urlShapeOK is the Clippet Auto URL's whole shape, the claim plane's own rule
// (forward/src/claim/panel.ts autoURLShape) re-checked against the server's
// CURRENT route host: https, exactly that host (a host with a port has none),
// no userinfo, port, query or fragment, and the string already in the form a
// URL prints.
func urlShapeOK(s, host string) bool {
	if len(s) > 320 || host == "" || strings.Contains(host, ":") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || u.String() != s {
		return false
	}
	return u.Scheme == "https" && u.User == nil && u.Port() == "" && u.RawQuery == "" && !u.ForceQuery &&
		u.Fragment == "" && u.Host == strings.ToLower(host) && autoPath.MatchString(u.EscapedPath())
}

// linkPath is a share link's path: a root id.
var linkPath = regexp.MustCompile(`^/[A-Za-z0-9_-]{1,64}$`)

// linkOK: only a root share link of the server's own host is ever posted,
// never text a box chose (run 7's review item 18).
func linkOK(s, host string) bool {
	if len(s) > 512 || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || u.String() != s {
		return false
	}
	return u.Scheme == "https" && u.Host == strings.ToLower(host) && u.User == nil && u.Port() == "" &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && linkPath.MatchString(u.EscapedPath())
}

func (p *planeHost) Upload(ctx context.Context, guild string, a autobot.Attachment) (string, error) {
	rec, err := p.record(ctx, guild)
	if err != nil || rec == nil {
		return "", lineErr(autobot.LineFailed)
	}
	target, ok := p.open.open(rec.U, guild, rec.Host)
	if !ok || !urlShapeOK(target, rec.Host) {
		return "", lineErr(lineURLGone)
	}
	status, body := p.box.Box(ctx, target, instructionBody(guild, a), p.boxTimeout)
	switch status {
	case http.StatusCreated, http.StatusOK:
		var r struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(body, &r) != nil || !linkOK(r.URL, rec.Host) {
			return "", lineErr(autobot.LineFailed)
		}
		return r.URL, nil
	case http.StatusAccepted:
		return "", lineErr(lineWorking)
	case 0:
		return "", lineErr(lineNoAnswer)
	}
	return "", lineErr(boxLine(status, body))
}

// boxLine is a box's refusal as the presser's line (rescue's boxLine): a 403
// is "wrong account" only in the box's own envelope (`{"error":"not_bound"}`)
// — an edge challenge answers 403 too; a 502 in the box's envelope
// (`{"error":"fetch"}`) is the CDN, not the box, so it is the failed line.
func boxLine(status int, body []byte) string {
	var env struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	switch status {
	case http.StatusForbidden:
		if env.Error == "not_bound" {
			return lineWrongAccount
		}
		return autobot.LineFailed
	case http.StatusBadGateway:
		if env.Error == "fetch" {
			return autobot.LineFailed
		}
	case http.StatusNotFound:
		return lineURLGone
	case http.StatusRequestEntityTooLarge:
		return lineTooLarge
	case http.StatusUnsupportedMediaType:
		return lineNotPlayable
	case http.StatusTooManyRequests:
		return lineLimit
	case http.StatusInsufficientStorage:
		return lineFull
	}
	if status >= 500 {
		return lineNoAnswer
	}
	return autobot.LineFailed
}

// ---------------------------------------------------------------------------
// The session, kept on the plane (`as:` one record, 4 KiB): read once at
// start, written on every change at most once a second.

type planeSessions struct {
	plane planeCaller
	m     *metrics
}

func (s planeSessions) Load(ctx context.Context) (autobot.Session, error) {
	status, body, err := s.plane.Plane(ctx, http.MethodGet, "/auto/session", nil)
	switch {
	case err == nil && status == http.StatusNotFound:
		return autobot.Session{}, nil
	case err == nil && status == http.StatusOK:
		var out autobot.Session
		if json.Unmarshal(body, &out) != nil {
			s.m.planeError()
			return autobot.Session{}, errPlane
		}
		return out, nil
	}
	s.m.planeError()
	return autobot.Session{}, errPlane
}

// Save answers autobot.ErrSessionRefused for a 4xx other than 429 — the plane
// refused this session as it is (a 400 shape, a 404 route not deployed, a 401
// bearer), so autobot does not ask again until the session changes; any other
// failure is errPlane, which autobot retries with its backoff (audit F1).
func (s planeSessions) Save(ctx context.Context, sess autobot.Session) error {
	b, _ := json.Marshal(sess)
	status, _, err := s.plane.Plane(ctx, http.MethodPut, "/auto/session", b)
	if err == nil && status/100 == 2 {
		return nil
	}
	s.m.planeError()
	if err == nil && status/100 == 4 && status != http.StatusTooManyRequests {
		return autobot.ErrSessionRefused
	}
	return errPlane
}

// refreshList reads GET /auto/guilds: the auto_guilds_bound gauge, and the
// list Bound pre-filters with (listFresh). A failure keeps the last list,
// which ages out of use after listFresh.
func (p *planeHost) refreshList(ctx context.Context) {
	p.mu.Lock()
	p.listTry = p.now()
	p.mu.Unlock()
	ids, ok := boundGuilds(ctx, p.plane, p.m)
	if !ok {
		return
	}
	listed := make(map[string]bool, len(ids))
	for _, id := range ids {
		listed[id] = true
	}
	p.mu.Lock()
	p.listed, p.listAt = listed, p.now()
	p.mu.Unlock()
}

// boundGuilds reads GET /auto/guilds for the auto_guilds_bound gauge and the
// pre-filter's list. The gauge counts `routable` — the servers whose route
// works — when the answer carries it (the plane's additive field), else
// `guilds` as before; the list it answers is `guilds` either way, so a
// server whose route is down is still asked for its own record.
func boundGuilds(ctx context.Context, plane planeCaller, m *metrics) ([]string, bool) {
	status, body, err := plane.Plane(ctx, http.MethodGet, "/auto/guilds", nil)
	var r struct {
		Guilds   []string  `json:"guilds"`
		Routable *[]string `json:"routable"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &r) != nil {
		m.planeError()
		return nil, false
	}
	if r.Routable != nil {
		m.setGuilds(len(*r.Routable))
	} else {
		m.setGuilds(len(r.Guilds))
	}
	return r.Guilds, true
}
