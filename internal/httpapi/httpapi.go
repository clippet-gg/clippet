// Package httpapi is the JSON API (DESIGN.md §8, §18.2): auth, API tokens,
// the streaming scratch-first upload, clip CRUD, progress polling, view
// beacons, media serving through the lazy seeker, and the health probes.
//
// Every route is registered as a full path (the handler is mounted at /api/,
// /healthz and /readyz) and wrapped by Methods so a method mismatch answers
// 405 + Allow + the JSON envelope. http.TimeoutHandler (15s) guards only the
// short JSON routes — never the upload, the media streams or owner DELETE.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"clippet/internal/auth"
	"clippet/internal/autobot"
	"clippet/internal/chunks"
	"clippet/internal/config"
	"clippet/internal/cost"
	"clippet/internal/db"
	"clippet/internal/drop"
	"clippet/internal/httpx"
	"clippet/internal/lifecycle"
	"clippet/internal/notify"
	"clippet/internal/ratelimit"
	"clippet/internal/store"
	"clippet/internal/transcode"
	"clippet/internal/urls"
	"clippet/internal/views"
	"clippet/internal/worker"
)

// Deps wires the API. Probe/Destroy/FreeBytes are injected so tests never
// touch ffprobe, storage lifecycle or statfs.
type Deps struct {
	Cfg     config.Config
	DB      *db.DB
	Store   store.Store
	Worker  worker.Worker
	URLs    urls.Builder
	Views   *views.Deduper
	Log     *slog.Logger
	Version string
	// Commit is the git object name this binary was built from — `main.commit`,
	// set by the Dockerfile's `COMMIT` build argument (§20 erratum 303). It is
	// SEPARATE from Version because the two answer different questions:
	// `CLIPPET_VERSION` is what the operator chose to call the release, and this
	// is what that release is made of. Empty is the ordinary state of a local
	// build, and GET /api/instance publishes empty — and anything that is not
	// forty hex characters — as JSON `null`.
	Commit  string
	Started time.Time

	// Probe is transcode.Probe(cfg.Transcode.FFprobe) in production.
	Probe func(ctx context.Context, path string) (*transcode.ProbeResult, error)
	// Strip is transcode.StripVerified(FFMPEG, FFPROBE, threads) in production: the
	// strip an AUDIO upload takes before a byte of it reaches the bucket (§20
	// erratum 328). Nil refuses every audio upload (a 503, never a silent
	// unstripped store) — the fail-closed direction for a wiring mistake.
	Strip func(ctx context.Context, p *transcode.ProbeResult, in, out string) (transcode.StripTarget, error)
	// Destroy is a closure over lifecycle.Destroy in production.
	Destroy func(ctx context.Context, clipID string) (lifecycle.Result, error)
	// FreeBytes is statfs(dir).free in production.
	FreeBytes func(dir string) (int64, error)
	// DestroyBudget is the inline budget for owner DELETE; 0 = 60s.
	DestroyBudget time.Duration
	// AutoFetchBudget is how long POST /in/auto/{token} holds its answer
	// before 202 (§20 erratum 324); 0 = 100s, under a proxy's 125 s read
	// timeout. Tests only.
	AutoFetchBudget time.Duration
	// Storage is the shared last-Ping cache: /readyz Sets after its live
	// ping, /healthz Gets; serve.go's boot loop Sets too.
	Storage *httpx.StorageHealth
	// SweepTotals returns the sweeper's lifetime (destroyed, errors) counters
	// (§12). nil → the "sweep" object is omitted from /healthz.
	SweepTotals func() (destroyed, errors int64)
	// Budget is the shared storage-budget reading the sweeper publishes
	// (ROADMAP #24, §20 erratum 157). nil, no STORAGE_BUDGET_BYTES set, or no
	// measurement taken yet → the "storage_budget" object is omitted from
	// /healthz entirely, because there is nothing true to say.
	Budget *httpx.StorageBudget
	// Bot is the Discord bot's OUTBOUND half (*notify.Bot in production, a
	// recorder in tests): what POST /api/discord/interactions hands a deferred
	// `/clip` to once it has answered Discord (ROADMAP #33, §20 erratum 256).
	// nil — the state DISCORD_APP_ID and its two companions being unset
	// produces — is off, and then the route is not registered at all.
	//
	// It is an interface rather than *notify.Bot so this package's tests can
	// drive the endpoint with no client anywhere: internal/notify is the only
	// package allowed to build one (§15).
	Bot DiscordBot
	// Login is "Sign in with Discord"'s OUTBOUND half (*notify.Login in
	// production, a fake in tests): the code exchange and the identity read the
	// callback performs (ROADMAP #44, §20 erratum 302). nil — the state
	// DISCORD_LOGIN_CLIENT_ID and its secret being unset produces — is off, and
	// then the three paths under /api/auth/discord are not registered at all.
	// An interface for Bot's reason: internal/notify is the only package
	// allowed to build a client (§15).
	Login IdentityProvider
	// Served is the per-day served-bytes counter (ROADMAP #39, §20 erratum
	// 270): every media response adds the bytes it wrote, /healthz reads the
	// totals back. nil is safe and is what a test handler gets — Add on a nil
	// counter does nothing, Totals answers zero, and the `storage` object's
	// served keys are then absent rather than a zero somebody could read as
	// "nothing has been served".
	Served *cost.Counter
	// Repairs reads the automatic stuck-job repair's lifetime counters, which
	// the worker publishes (ROADMAP #12, §20 erratum 231). nil → the "repair"
	// object is omitted from /healthz. It is COUNTS only: the SETTING behind
	// them is operator business and lives in `admin doctor`, because /healthz
	// is anonymous and names no configured value (§15).
	Repairs func() worker.RepairStatus
	// Drop is the drop folder's watcher (ROADMAP #42 (b), §20 erratum 296),
	// built by `clippet serve` when DROP_FOLDER_OWNER names an account that
	// exists. nil is off, and off is every instance that has not set it.
	//
	// It is handed IN rather than built here for the reason the bot is: the
	// watcher runs on its own schedule and `serve` starts and stops it with
	// everything else in the background chain. What it cannot do for itself is
	// reach the ingest core, so NewHandler wires that in below — once, at
	// construction, before anything runs.
	Drop DropWatcher
	// Auto is Clippet Auto on the box (§20 erratum 333, DEPLOY §30.10):
	// *autobot.Auto in production, built by serve when DISCORD_AUTO_MODE is
	// prompt or auto; nil is off. NewHandler hands it the instance side
	// (autobox.go) as it hands the drop watcher its ingest, and the
	// interactions endpoint routes its button's presses to it.
	Auto AutoBot
	// Uploads is the chunked web upload's session store (§20 erratum 344),
	// built by `clippet serve` over $SCRATCH_DIR/.chunks and shared with the
	// sweeper, which expires it. nil — a test's handler — is one of the
	// handler's own over the same directory.
	Uploads *chunks.Store
}

// AutoBot is Clippet Auto as this package needs it: the instance side handed
// in once, and the ONE press function for presses arriving at the §30
// endpoint (the socket's presses go to the same function inside autobot).
type AutoBot interface {
	SetHost(autobot.Host)
	Press(ctx context.Context, raw []byte) (answer map[string]any, start func())
}

// DropWatcher is the drop folder's watcher as this package needs it: the
// account it ingests as, and somewhere to hand the upload path (*drop.Watcher
// in production, a recorder in tests). The wiring is one-directional — the
// watcher calls in, and nothing here ever calls the watcher.
//
// The owner's id comes from the watcher rather than from Cfg because it is not
// configuration: `.env` names an ACCOUNT and `clippet serve` resolves it
// against the database at boot, refusing to start when there is no such
// account. A Config field holding an id nothing validated would be the one
// place that fact could rot.
type DropWatcher interface {
	OwnerID() string
	SetIngest(drop.Ingest)
}

// DiscordBot is what the interactions endpoint hands a deferred `/clip` to:
// the fetch, the ingest and the follow-up edit, in a goroutine of the Bot's
// own (internal/notify's *Bot implements it). One method, because the endpoint
// has exactly one thing to say once it has answered Discord. Fetch is the
// Clippet Auto URL's job (§20 erratum 324): the same fetcher with no
// interaction to answer, its outcome on a buffered channel. Redraw is the
// upload form's late title (§20 erratum 325): the bot redraws the card of the
// link it posted for that clip, if it holds one; it never blocks. HoldReply is
// the late card's (§20 erratum 330): a type-4 reply that posts the link to a clip
// whose card is not ready yet is held, so the bot redraws it once the worker says
// the card is; it never blocks either.
type DiscordBot interface {
	Clip(job notify.ClipJob)
	Fetch(att notify.Attachment, ingest notify.Ingest) (<-chan notify.FetchOutcome, error)
	Redraw(clipID string)
	HoldReply(token, clipID string)
}

// Defaults for zero-valued Deps.Cfg fields (§4); config.Load always fills
// them, tests may build a bare Config.
const (
	defaultScratchDir     = "/scratch"
	defaultMaxUploadBytes = 4 << 30
	defaultRateAuthPerMin = 10
	defaultRateUploadHour = 60
	defaultRateUploadIP   = 120
	defaultRateMaxKeys    = 10000
	defaultViewDedupeTTL  = 30 * time.Minute
	defaultViewDedupeMax  = 50000
	defaultDestroyBudget  = 60 * time.Second
	// defaultAutoFetchBudget is POST /in/auto/{token}'s hold (§20 erratum
	// 324): under the 125 s a proxy in front of the box waits for an answer.
	defaultAutoFetchBudget = 100 * time.Second
	viewsPerMinute         = 60
	// pollsPerMinute bounds GET /api/clips/{id}/progress per client address
	// (§20 erratum 211, amended). That route used to carry no limiter at all,
	// which was defensible while only a QUEUED or PROCESSING clip polled it — such
	// a clip is not shareable, so the traffic was the owner's own tab. Since the
	// quality tiers a READY clip polls it too, from every viewer of a public share
	// link whose better tier is still encoding, and an unlimited anonymous DB read
	// per clip is the one thing erratum 87 says a per-clip public route must not
	// be.
	//
	// It is its OWN bucket and not the `view` one, which is what the batched
	// sibling shares, for a reason that is about the product and not about the
	// numbers: the view bucket is also what counts VIEWS and serves preview GIFs,
	// so putting a poll on it would make one office or one NAT'd household of
	// viewers spend their view counts on polling. 120/min is twice what a single
	// tab can issue at the fastest poll interval below, so a handful of viewers
	// behind one address still poll and still get counted.
	pollsPerMinute = 120
	// interactionsPerMinute bounds POST /api/discord/interactions per client
	// address (§20 erratum 256). Discord signs every real interaction, so what
	// this bounds is the cost of an UNSIGNED flood — a hex decode and one
	// ed25519.Verify, both cheap, both worth a ceiling anyway — plus the
	// database read a signed one costs.
	//
	// Its own bucket rather than the `auth` one, for the reason the poll
	// bucket is its own: the auth bucket is 10/min/IP by default and is what
	// login and register spend, and Discord's interactions arrive from a
	// SMALL set of egress addresses that a busy server would exhaust in a
	// minute — taking every human behind the same proxy's login attempts with
	// it. Sixty a minute is far above what a Discord server's `/clip` traffic
	// looks like (the work is an upload, not a click) and far below what makes
	// an unsigned flood interesting.
	interactionsPerMinute = 60
	// claimsPerMinute bounds GET /api/bot/claim per client address (§20
	// erratum 279(h)). It is its OWN bucket and not the `bot` one, for a reason
	// that is about who spends it: the claim endpoint is anonymous and public
	// — anyone who can reach the box can GET it — while the interactions
	// route's tokens are what the OFFICIAL bot's `/clip`s spend, and on a box
	// that trusts its proxy every server's `/clip` shares ONE of those buckets
	// (the forwarder pins a fixed X-Real-IP sentinel; DEPLOY §30.8). A
	// stranger draining the claim endpoint must not turn real `/clip`s into
	// "not available". Thirty a minute is far above what the claim page (one
	// GET per claim) and the weekly re-proof ever ask.
	claimsPerMinute = 30
)

// NewHandler returns the API router. It is mounted at /api/, /healthz and
// /readyz by serve.go, so patterns here are full paths.
func NewHandler(d Deps) http.Handler {
	if d.Storage == nil {
		d.Storage = &httpx.StorageHealth{}
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Worker == nil {
		d.Worker = worker.NewNoop()
	}
	d.Cfg = withDefaults(d.Cfg)
	if d.Views == nil {
		d.Views = views.NewDeduper(d.Cfg.ViewDedupeTTL, d.Cfg.ViewDedupeMax)
	}
	if d.DestroyBudget <= 0 {
		d.DestroyBudget = defaultDestroyBudget
	}
	if d.AutoFetchBudget <= 0 {
		d.AutoFetchBudget = defaultAutoFetchBudget
	}
	h := newHandler(d)
	// The drop folder's watcher gets the upload path here and nowhere else
	// (§20 erratum 296). It has to be THIS handler's — a watcher with an
	// ingest of its own would carry its own rate limiters, and "the same gates
	// as an HTTP upload" would stop being true of the one gate nobody would
	// notice: the account's upload budget is a bucket, and two buckets are
	// twice the budget.
	if d.Drop != nil {
		d.Drop.SetIngest(h.dropIngest(d.Drop.OwnerID()))
	}
	// Clippet Auto gets the instance side here, for the drop folder's reason:
	// its uploads are THIS handler's, with this handler's buckets.
	if d.Auto != nil {
		d.Auto.SetHost(autoHost{h: h})
	}
	return h.routes()
}

// newHandler assembles the handler NewHandler mounts. It is split out for one
// reason: a test needs the *handler itself — routes() records what it
// registered (handler.patterns, §20 erratum 248) and NewHandler hands back only
// the mux.
func newHandler(d Deps) *handler {
	h := &handler{
		d:           d,
		now:         time.Now,
		auth:        &auth.Resolver{DB: d.DB, Cfg: d.Cfg, Now: time.Now},
		authLimit:   ratelimit.New(float64(d.Cfg.RateAuthPerMin)/60, d.Cfg.RateAuthPerMin, d.Cfg.RateMaxKeys),
		uploadLimit: ratelimit.New(float64(d.Cfg.RateUploadPerHour)/3600, d.Cfg.RateUploadPerHour, d.Cfg.RateMaxKeys),
		// The second upload bucket (§20 erratum 97). RATE_UPLOAD_PER_HOUR is
		// per ACCOUNT, and accounts are free — register is bounded only by
		// the auth limiter at 10/min/IP, so one address can mint hundreds an
		// hour, each carrying its own upload budget. Requiring BOTH buckets
		// puts a ceiling back on the address.
		uploadIPLimit: ratelimit.New(float64(d.Cfg.RateUploadPerHourPerIP)/3600, d.Cfg.RateUploadPerHourPerIP, d.Cfg.RateMaxKeys),
		viewLimit:     ratelimit.New(viewsPerMinute/60.0, viewsPerMinute, d.Cfg.RateMaxKeys),
		pollLimit:     ratelimit.New(pollsPerMinute/60.0, pollsPerMinute, d.Cfg.RateMaxKeys),
		botLimit:      ratelimit.New(interactionsPerMinute/60.0, interactionsPerMinute, d.Cfg.RateMaxKeys),
		claimLimit:    ratelimit.New(claimsPerMinute/60.0, claimsPerMinute, d.Cfg.RateMaxKeys),
		forms:         newFormTable(),
		autoInflight:  newInflight(autoFetchInflightMax),
		uploads:       d.Uploads,
	}
	if h.uploads == nil {
		h.uploads = chunks.New(filepath.Join(d.Cfg.ScratchDir, chunks.DirName))
	}
	return h
}

func withDefaults(c config.Config) config.Config {
	if c.ScratchDir == "" {
		c.ScratchDir = defaultScratchDir
	}
	if c.MaxUploadBytes <= 0 {
		c.MaxUploadBytes = defaultMaxUploadBytes
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = auth.DefaultSessionTTL
	}
	if c.SessionCookie == "" {
		c.SessionCookie = auth.DefaultCookieName
	}
	if c.RateAuthPerMin <= 0 {
		c.RateAuthPerMin = defaultRateAuthPerMin
	}
	if c.RateUploadPerHour <= 0 {
		c.RateUploadPerHour = defaultRateUploadHour
	}
	if c.RateUploadPerHourPerIP <= 0 {
		c.RateUploadPerHourPerIP = defaultRateUploadIP
	}
	if c.UploadChunkBytes <= 0 {
		c.UploadChunkBytes = config.DefaultUploadChunkBytes
	}
	if c.RateMaxKeys <= 0 {
		c.RateMaxKeys = defaultRateMaxKeys
	}
	if c.ViewDedupeTTL <= 0 {
		c.ViewDedupeTTL = defaultViewDedupeTTL
	}
	if c.ViewDedupeMax <= 0 {
		c.ViewDedupeMax = defaultViewDedupeMax
	}
	return c
}

type handler struct {
	d    Deps
	now  func() time.Time
	auth *auth.Resolver

	authLimit, uploadLimit, uploadIPLimit, viewLimit, pollLimit, botLimit, claimLimit *ratelimit.Limiter

	// patterns is every mux pattern routes() registered, recorded as it
	// registers them (§20 erratum 248). The Go mux cannot be asked what it
	// holds, and one test needs to know: with a clip id at the ROOT of the
	// origin, the first segment of every route this app serves has to be in
	// ids.Reserved, or tomorrow's route could be yesterday's clip id.
	// internal/web keeps the same record for the same reason.
	patterns []string

	// setupProbe is POST /api/setup/check's single-flight + 10 s cache
	// (ROADMAP #45, §20 erratum 297). It lives on the handler so it is per
	// MOUNT, exactly as internal/web's regCache is: a process that called
	// NewHandler twice would simply have two, each probing at most once every
	// ten seconds, and nothing about the route's correctness depends on there
	// being one. It is a CACHE, not a limiter — nobody is ever refused.
	setupProbe setupProbe

	// forms is the upload form's table (§20 erratum 320): per mount, in
	// memory, fifteen minutes an entry. Lost on a restart, as the jobs are.
	forms *formTable

	// autoInflight counts the Clippet Auto URL's running fetch jobs per
	// account (§20 erratum 327): at most autoFetchInflightMax at once.
	autoInflight *inflight

	// uploads is the chunked web upload's sessions (§20 erratum 344).
	uploads *chunks.Store
}

// Methods restricts next to the given methods (HEAD is implied by GET and
// listed in Allow, RFC 9110 §10.2.1) and answers anything else with 405 +
// Allow + the JSON envelope (§8.1). With a "/" catch-all registered, the
// ServeMux itself never produces a 405, so known paths are registered
// method-agnostically and guarded here.
func Methods(next http.Handler, methods ...string) http.Handler {
	allowed := make(map[string]bool, len(methods)+1)
	list := make([]string, 0, len(methods)+1)
	for _, m := range methods {
		if allowed[m] {
			continue
		}
		allowed[m] = true
		list = append(list, m)
		if m == http.MethodGet && !allowed[http.MethodHead] {
			allowed[http.MethodHead] = true
			list = append(list, http.MethodHead)
		}
	}
	allow := strings.Join(list, ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Method] {
			w.Header().Set("Allow", allow)
			httpx.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method "+r.Method+" not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}
