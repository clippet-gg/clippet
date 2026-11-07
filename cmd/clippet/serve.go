package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"clippet/internal/auth"
	"clippet/internal/autobot"
	"clippet/internal/chunks"
	"clippet/internal/config"
	"clippet/internal/cost"
	"clippet/internal/db"
	"clippet/internal/drop"
	"clippet/internal/httpapi"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/lifecycle"
	"clippet/internal/notify"
	"clippet/internal/scratch"
	"clippet/internal/store"
	"clippet/internal/sweeper"
	"clippet/internal/transcode"
	"clippet/internal/urls"
	"clippet/internal/views"
	"clippet/internal/web"
	"clippet/internal/worker"
)

// Boot and shutdown budgets (DESIGN.md §8, §8.7, §18.1).
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	// How long ONE ping attempt may take. The COUNT of attempts and the pause
	// between them are configuration — STORAGE_PING_ATTEMPTS and
	// STORAGE_PING_INTERVAL, §20 erratum 283 — because they are the whole of
	// how long a process with an unreachable bucket takes to say so, and that
	// is an operator's call. This one is not: a Ping that has not answered in
	// three seconds is not the kind of slow more waiting fixes, and the
	// control-plane budget an operator DOES set for that is
	// STORAGE_OP_TIMEOUT, inside the store.
	pingTimeout       = 3 * time.Second
	httpDrainBudget   = 15 * time.Second
	workerStopBudget  = 20 * time.Second
	sweeperStopBudget = 5 * time.Second
)

// runServe boots in the §8.7 order — config → DB → reconcile → listener UP
// → background storage ping → worker → sweeper — and runs the §18.1 SIGTERM
// sequence on shutdown. Every fatal condition logs one line and returns 1.
func runServe(cfg config.Config) int {
	log := newLogger(cfg)
	slog.SetDefault(log)

	ffmpeg, ffprobe, ffVersion, err := resolveFFmpeg(cfg.Transcode)
	if err != nil {
		log.Error("boot: ffmpeg", "err", err)
		return 1
	}
	cfg.Transcode.FFmpeg, cfg.Transcode.FFprobe = ffmpeg, ffprobe
	cfg.Transcode.FFmpegVersion = ffVersion
	// What this ffmpeg can actually do, against what `.env` asked for (§20 errata
	// 214–216): an AV1 Full or a CUDA path this binary has no encoder for is fatal
	// HERE rather than three failed clips later, and a tag with no font or no
	// drawtext filter is a warning that switches the tag off.
	font, err := resolveFFCaps(cfg, log)
	if err != nil {
		log.Error("boot: ffmpeg capabilities", "err", err)
		return 1
	}
	cfg.LightTag.Font = font
	if font == "" {
		cfg.LightTag.Text = ""
	}
	// `registration` is on the boot line, every time (ROADMAP #21, §20 erratum 106):
	// `first` means "open only while the users table is empty", and an operator
	// looking at a public origin should not have to reason about which of three
	// modes a `.env` they last edited months ago left behind.
	//
	// `threads` and `hwaccel` are on it for a related reason (§20 errata 215, 216):
	// the thread count is DERIVED from the container's own CPU quota, so it is the
	// one number an operator cannot read out of their own `.env`.
	log.Info("boot", "version", version, "ffmpeg", ffVersion, "listen", cfg.Listen,
		"public_base_url", cfg.PublicBaseURL.String(), "s3_endpoint", cfg.S3.Endpoint, "bucket", cfg.S3.Bucket,
		"registration", cfg.Registration.String(),
		"setup_wizard", uploadFormWord(!cfg.SetupWizardOff), // §20 erratum 335 (d)
		"threads", transcode.ResolveThreads(cfg.Transcode.Threads), "hwaccel", cfg.Transcode.HWAccel,
		"light_tag", cfg.LightTag.Text != "",
		// On by default on both backends, so it is on the boot line for
		// registration's reason: a copy of the database written into the store
		// is a fact an operator should not have to infer from an empty `.env`
		// (§20 errata 343, 348).
		"db_snapshot", onOff(cfg.DBSnapshot))
	// A container cannot see its own port publish, so this warns on the COMBINATION
	// rather than on a detection (§20 erratum 96): trusting a prefix is only safe
	// while nothing outside the proxy can reach the listener. One line, at boot.
	if n := len(cfg.TrustedProxies); n > 0 {
		ps := make([]string, n)
		for i, p := range cfg.TrustedProxies {
			ps[i] = p.String()
		}
		log.Warn("trusted proxies configured: X-Real-IP and X-Forwarded-For from these prefixes are believed - make sure the app port is not published beyond loopback (DEPLOY.md §4)",
			"prefixes", strings.Join(ps, ","))
	}

	warnShippedCredentials(log, cfg)

	dirs := []string{cfg.DataDir, cfg.ScratchDir}
	if cfg.StorageBackend == config.BackendFS {
		// STORAGE_DIR is created HERE and nowhere else in the binary (§20
		// erratum 276). A bind mount whose host side is missing arrives as an
		// empty directory rather than as an error, which is what the store
		// marker below is for; a verb that created the mount point itself
		// would turn a mistyped bind into media written to the container's own
		// filesystem and lost on the next recreate.
		dirs = append(dirs, cfg.StorageDir)
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Error("boot: create dir", "dir", dir, "err", err)
			return 1
		}
	}

	// DATA_DIR's lock, SHARED (§20 erratum 343, datalock.go): every server
	// holds it for its life so `admin restore --from-store`, which takes it
	// exclusive, refuses to replace the database under a running one. Two
	// servers over one database is a supported shape and both hold it; what
	// is refused is booting while a restore holds it. A filesystem that cannot
	// lock is a WARN and a boot.
	switch release, err := lockDataDir(cfg.DataDir, false); {
	case errors.Is(err, errDataLockHeld):
		log.Error("boot: DATA_DIR is held by `admin restore` (its serve.lock is taken exclusive): start the server when the restore is done",
			"dir", cfg.DataDir)
		return 1
	case err != nil:
		log.Warn("boot: DATA_DIR's serve.lock could not be taken: `admin restore` cannot tell this server is running", "err", err)
	default:
		defer release()
	}

	// 1. DB open + migrate + reconcile + janitor (local only, milliseconds).
	database, err := db.Open(dbPath(cfg))
	if err != nil {
		log.Error("boot: db open", "err", err)
		return 1
	}
	bootCtx, cancelBoot := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelBoot()
	if err := database.Migrate(bootCtx); err != nil {
		log.Error("boot: migrate", "err", err)
		_ = database.Close()
		return 1
	}
	now := time.Now().Unix()
	released, requeued, failed, err := database.ReconcileOnStartup(bootCtx, limits(cfg), now)
	if err != nil {
		log.Error("boot: reconcile", "err", err)
		_ = database.Close()
		return 1
	}
	log.Info("boot: reconcile", "released", released, "requeued", requeued, "failed", failed)
	if n, err := database.DeleteExpiredSessions(bootCtx, now); err != nil {
		log.Warn("boot: prune sessions", "err", err)
	} else if n > 0 {
		log.Info("boot: pruned sessions", "n", n)
	}
	scratchJanitor(bootCtx, cfg.ScratchDir, database, log, time.Now())

	// The drop folder's owner (ROADMAP #42 (b), §20 erratum 296), resolved
	// here because this is the first moment the database is open and the
	// listener is not.
	//
	// FATAL when the account does not exist. `.env` names an account, and an
	// account is not a string this process may assume into being: a typo would
	// otherwise leave the folder watched by nobody, every file in it silently
	// ignored, and the only symptom a person waiting for a clip. That is the
	// same class of failure as a DISCORD_APP_ID nobody checked, and it fails
	// the same way — one line, at boot, naming the variable and the fix.
	var dropWatcher *drop.Watcher
	if cfg.DropFolderOwner != "" {
		owner, err := database.GetUserByUsername(bootCtx, cfg.DropFolderOwner)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				log.Error("boot: DROP_FOLDER_OWNER names no account on this instance: register it, or remove the line (DEPLOY.md §34)",
					"owner", cfg.DropFolderOwner)
			} else {
				log.Error("boot: DROP_FOLDER_OWNER could not be resolved", "owner", cfg.DropFolderOwner, "err", err)
			}
			_ = database.Close()
			return 1
		}
		dropWatcher = drop.New(drop.Deps{
			Dir: cfg.StorageDir, Owner: owner.Username, OwnerID: owner.ID,
			Seen:  database,
			Probe: transcode.Probe(cfg.Transcode.FFprobe),
			Log:   log, Now: time.Now,
		})
	}

	// 2. Store (no network) and the HTTP handler graph.
	st, err := store.Open(cfg)
	if err != nil {
		log.Error("boot: store", "err", err)
		_ = database.Close()
		return 1
	}
	storage := &httpx.StorageHealth{}
	storage.SetBackend(cfg.StorageBackend)
	// unmarked is non-nil when the folder store's marker is missing over a
	// database that accounts for stored clips (§20 erratum 348 (c)): the ping
	// loop then records it instead of pinging, so the instance serves degraded
	// exactly as it does when storage is unreachable.
	var unmarked error
	if cfg.StorageBackend == config.BackendFS {
		// The marker says "this directory is the one this instance writes to"
		// (§20 erratum 276). Writing it is best effort: a read-only mount is
		// the store's problem to report through Ping, and it is not worth
		// refusing to boot over a file whose only job is to be MISSING when
		// somebody mounts the wrong share — which is also why it is NOT written
		// over a folder the database says should already hold clips (erratum
		// 348 (c)): written there, it would erase the one signal `admin doctor`
		// has that the share is not mounted.
		unmarked = bootStoreMarker(bootCtx, cfg.StorageDir, database.StorageUsed, log)
		if unmarked != nil {
			// /readyz pings live: without this its first call would ping the
			// empty folder, answer ok and clear the degraded state.
			st = unmarkedStore{Store: st, err: unmarked}
		}
		log.Info("storage backend", "backend", cfg.StorageBackend, "dir", cfg.StorageDir)
	} else {
		log.Info("storage backend", "backend", cfg.StorageBackend, "bucket", cfg.S3.Bucket)
	}
	// How the media is DELIVERED, which is a different fact from where it
	// lives (ROADMAP #34 lane F, §20 erratum 278). It is on the boot line for
	// the reason `registration` is: it changes what every viewer's client
	// does, and an operator reading a log should not have to work out which of
	// two modes a `.env` they edited months ago left behind.
	if cfg.RedirectsMedia() {
		// The origin comes from a REAL presign, never from S3_ENDPOINT: with
		// virtual-host bucket lookup (the default, and what R2 and B2 use) the
		// signed URL carries the BUCKET in the host, and a CSP built from the
		// endpoint would block the player on our own page (§20 erratum 278,
		// amended). Signing is local HMAC, so this opens no connection and is
		// safe here, before the storage ping has even run.
		//
		// FATAL when it fails: the alternative is booting with the narrower
		// policy and a watch page that silently shows nothing.
		origin, err := store.MediaOrigin(bootCtx, st, cfg.MediaPresignTTL)
		if err != nil {
			log.Error("boot: media delivery: the presign origin could not be resolved", "err", err)
			_ = database.Close()
			return 1
		}
		cfg.ResolvedMediaOrigin = origin
		log.Info("media delivery", "mode", cfg.MediaDelivery, "presign_endpoint", cfg.S3.PresignEndpoint,
			"media_origin", origin, "presign_ttl", cfg.MediaPresignTTL)
	} else {
		log.Info("media delivery", "mode", cfg.MediaDelivery)
		// A MEDIA_PRESIGN_* value that was read and then IGNORED. A warning
		// rather than an error, because it is what `.env` looks like while an
		// operator gets ready to flip the switch — but never silence, because
		// "I set it and nothing changed" must have an answer that is not a
		// viewer. `admin doctor`'s `media` row says the same sentence.
		if note := config.MediaNote(cfg, os.Getenv); note != "" {
			log.Warn("media delivery: " + note)
		}
	}
	started := time.Now()
	buildID := version
	if buildID == "" || buildID == "dev" {
		buildID = "dev-" + strconv.FormatInt(started.Unix(), 36)
	}
	u := urls.New(cfg.PublicBaseURL, buildID)
	// The ONE place the outbound clients are built (§15, §17 #4), each only
	// when the operator configured it: the Discord webhook here, the bot and
	// the login below, the clip webhook after them. The startup line names the
	// host and the webhook id — never the token, which is the rest of the path.
	// Per-tag routing (ROADMAP #26) adds NAMED webhooks to the same single
	// client: the §15 tripwire is about how many ways this binary can reach
	// the network, and one client with a map of endpoints is still one. Each
	// route gets its own boot line, naming the tag, the host and the numeric
	// id — never the token, which is the rest of the path.
	var notifier *notify.Notifier
	if cfg.DiscordEnabled() {
		routes := make([]notify.Route, 0, len(cfg.DiscordRoutes))
		for _, r := range cfg.DiscordRoutes {
			routes = append(routes, notify.Route{Name: r.Tag, Endpoint: r.Webhook.URL()})
		}
		notifier = notify.New(notify.Deps{Endpoint: cfg.DiscordWebhook.URL(), Routes: routes, URLs: u})
		if cfg.DiscordWebhook.Configured() {
			log.Info("discord webhook configured", "host", cfg.DiscordWebhook.Host(), "webhook_id", cfg.DiscordWebhook.ID())
		}
		for _, r := range cfg.DiscordRoutes {
			log.Info("discord webhook route configured", "tag", r.Tag,
				"host", r.Webhook.Host(), "webhook_id", r.Webhook.ID())
		}
		// Gated on there BEING rules, and that is exactly right today only
		// because config.Load refuses DISCORD_WEBHOOK_UNMATCHED=skip with no
		// rules and a live default webhook (§20 erratum 160). With that
		// refusal in place the one routes-empty state left here is
		// unmatched=default, which this line would tell an operator nothing
		// about. If the refusal is ever relaxed, widen the guard to
		// `len(cfg.DiscordRoutes) > 0 || cfg.DiscordUnmatched !=
		// config.DiscordUnmatchedDefault` — widening it NOW would be dead
		// code. Whatever happens to the condition, keep the discipline of the
		// lines above: host and webhook_id, never a URL (N072).
		if len(cfg.DiscordRoutes) > 0 {
			log.Info("discord webhook routing", "unmatched", cfg.DiscordUnmatched,
				"default_configured", cfg.DiscordWebhook.Configured())
		}
	}
	// The Discord bot (ROADMAP #33, §20 errata 253-256). A SECOND outbound
	// feature in the one package allowed a client, and only when the operator
	// configured all three variables: with them unset nothing here runs, the
	// interactions route is not registered and no client is built for it. The
	// boot line names the application id and the endpoint URL to paste into the
	// developer portal — never the token, which is the one secret of the three
	// (DEPLOY.md §30).
	var bot *notify.Bot
	if cfg.DiscordBot.Configured() {
		bot = notify.NewBot(notify.BotDeps{
			AppID: cfg.DiscordBot.AppID(), Token: cfg.DiscordBot.Token(),
			Managed:             cfg.DiscordBot.Managed(),
			CommandName:         cfg.DiscordBot.CommandName(),
			UploadCommandName:   cfg.DiscordBot.UploadCommandName(),
			EnrichCommandName:   cfg.DiscordBot.EnrichCommandName(),
			SettingsCommandName: cfg.DiscordBot.SettingsCommandName(),
			URLs:                u, Log: log,
			APIHost:         config.DiscordAPIHost(),
			AllowAttachment: config.ValidAttachmentURL,
			MaxBytes:        cfg.MaxUploadBytes,
		})
		// The boot line names the COMMANDS too (§20 errata 284, 315): on a box
		// that runs two instances they are the one fact saying which of them the
		// interactions in the lines below belong to. `command` stays the LINK
		// command's key (it was the only one); `upload_command` is its twin.
		log.Info("discord bot configured", "app_id", cfg.DiscordBot.AppID(),
			"upload_command", "/"+cfg.DiscordBot.UploadCommandName(),
			"command", "/"+cfg.DiscordBot.CommandName(),
			"enrich_command", cfg.DiscordBot.EnrichCommandName(),
			"settings_command", "/"+cfg.DiscordBot.SettingsCommandName(),
			"upload_form", uploadFormWord(cfg.UploadForm), // §20 erratum 320
			"enrich_upload", uploadFormWord(cfg.EnrichUpload), // §20 erratum 323
			"interactions_url", u.Abs(httpapi.InteractionsPath()),
			"attachment_hosts", strings.Join(config.DiscordCDNHosts(), ","),
			"managed", cfg.DiscordBot.Managed())
	}
	// Clippet Auto on the box (§20 erratum 333, DEPLOY §30.10): with
	// DISCORD_AUTO_MODE off (the default) nothing here runs — no client, no
	// goroutine, no socket. prompt|auto builds the client in the one package
	// allowed one, and the gateway is started with the drop folder, after the
	// worker, below. The boot line names the mode, never the token.
	var auto *autobot.Auto
	if cfg.DiscordAuto.On() {
		ac := notify.NewAutoClient(notify.AutoDeps{Token: cfg.DiscordAuto.Token(), APIHost: config.DiscordAPIHost(),
			AllowGateway: config.ValidGatewayURL})
		auto = autobot.New(autobot.Config{Mode: cfg.DiscordAuto.Mode(), Seconds: cfg.DiscordAuto.Seconds(),
			Notice: cfg.DiscordAuto.Notice(), Log: log, Dial: ac.Dial, Call: ac.Call, AllowGateway: config.ValidGatewayURL})
		log.Info("discord auto configured", "auto", cfg.DiscordAuto, "gateway_host", config.DiscordGatewayHost())
	}
	// Sign in with Discord (ROADMAP #44, §20 erratum 302): a THIRD outbound
	// feature in the one package allowed a client, and only when the operator
	// configured both variables. The boot line names the client id and the
	// redirect URI to paste into the developer portal — never the secret.
	var login *notify.Login
	if cfg.DiscordLogin.Configured() {
		login = notify.NewLogin(notify.LoginDeps{
			ClientID: cfg.DiscordLogin.ClientID(), Secret: cfg.DiscordLogin.Secret(),
			APIHost: config.DiscordAPIHost(),
		})
		log.Info("discord login configured", "client_id", cfg.DiscordLogin.ClientID(),
			"redirect_uri", u.Abs(httpapi.DiscordLoginCallbackPath()))
	}
	// The automatic stuck-job repair's counters (ROADMAP #12, §20 erratum 231):
	// the worker writes them, /healthz reads them, and like the sweeper's
	// storage-budget holder they are constructed here so neither side has to
	// know about the other.
	repairs := &worker.Repairs{}
	// The clip webhook (ROADMAP #54, §20 erratum 319): the fourth outbound
	// feature in the one package allowed a client, built only when both of
	// its variables are set. With them unset nothing is built and no `hook`
	// job is ever enqueued. The boot line names the scheme and the host —
	// never the path, the query or the key (the URL is a credential).
	var hook *notify.Hook
	if cfg.ClipWebhook.Configured() {
		hook = notify.NewHook(notify.HookDeps{Endpoint: cfg.ClipWebhook.URL(), Secret: cfg.ClipWebhook.Secret(), URLs: u})
		log.Info("clip webhook configured", "scheme", cfg.ClipWebhook.Scheme(), "host", cfg.ClipWebhook.Host())
	}
	// The database snapshot in the store (§20 errata 343, 348), built only when
	// on: the bucket, or on `fs` the media folder's `_clippet/`.
	// Its triggers are the clip table's two moments that matter — a card that
	// has become ready (the worker's hook below) and a row destroyed (lifeDeps)
	// — and its own timers; Touch never blocks either caller.
	var snap *dbSnapshotter
	if cfg.DBSnapshot {
		snap = newDBSnapshotter(cfg, database, st, log)
	}
	wkDeps := worker.Deps{DB: database, Store: st, Cfg: cfg, Log: log, Now: time.Now, Notifier: notifier, Hook: hook,
		Repairs: repairs}
	switch {
	case bot != nil && snap != nil:
		wkDeps.CardReady = func(clipID string) {
			snap.Touch()
			bot.CardReady(clipID)
		}
	case bot != nil:
		// The late card (§20 erratum 330): the worker says when a clip's card
		// becomes ready, and the bot redraws a link it posted before it was. A
		// method value, assigned only when the bot exists, so no bot is a nil
		// func and the worker reads nothing for it.
		wkDeps.CardReady = bot.CardReady
	case snap != nil:
		wkDeps.CardReady = func(string) { snap.Touch() }
	}
	wk := worker.New(wkDeps)
	lifeDeps := lifecycle.Deps{DB: database, Store: st, Worker: wk, KeyPrefix: cfg.S3.KeyPrefix,
		ScratchDir: cfg.ScratchDir, Log: log, OpTimeout: cfg.S3.OpTimeout, Busy: wk.Busy}
	if snap != nil {
		lifeDeps.Destroyed = func(string) { snap.Touch() }
	}
	// Constructed before the handler graph (it starts no goroutine until
	// Start) so its Totals() feed /healthz's "sweep" counters (§12).
	// The DISPLAYED storage-budget reading (ROADMAP #24): the sweeper measures
	// it, /healthz, the library page and the settings panel's quota row read
	// it, and nothing anywhere refuses work because of THIS value. The opt-in
	// cap (STORAGE_BUDGET_HARD=1, §20 erratum 277) reads the database live at
	// the upload instead, so a delete makes room at once rather than at the
	// sweeper's next tick.
	budget := &httpx.StorageBudget{}
	// The chunked web upload's sessions (§20 erratum 344): one store, which the
	// API writes and the sweeper expires.
	uploads := chunks.New(filepath.Join(cfg.ScratchDir, chunks.DirName))
	sw := sweeper.New(lifeDeps, cfg).WithBudget(budget).WithUploadSweep(uploads.Expire)
	if snap != nil {
		sw = sw.WithExtraBytes(snap.Bytes) // the snapshot is stored bytes like any other
	}
	// The served-bytes counter (ROADMAP #39, §20 erratum 270): the media routes
	// add the bytes they wrote, /healthz and the settings page read the totals,
	// and its own loop (started below, beside the listener) writes one row per
	// UTC day. Constructed here, with the other shared holders, so no side has
	// to know about the other.
	served := cost.New(database, log)
	apiDeps := httpapi.Deps{
		Cfg: cfg, DB: database, Store: st, Worker: wk, URLs: u,
		Views:   views.NewDeduper(cfg.ViewDedupeTTL, cfg.ViewDedupeMax),
		Log:     log,
		Version: version, Commit: commit, Started: started,
		Probe: transcode.Probe(cfg.Transcode.FFprobe),
		Strip: transcode.StripVerified(cfg.Transcode.FFmpeg, cfg.Transcode.FFprobe, transcode.ResolveThreads(cfg.Transcode.Threads)),
		Destroy: func(ctx context.Context, clipID string) (lifecycle.Result, error) {
			return lifecycle.Destroy(ctx, lifeDeps, clipID)
		},
		FreeBytes:   freeBytes,
		Storage:     storage,
		SweepTotals: sw.Totals,
		Budget:      budget,
		Served:      served,
		Repairs:     repairs.Get,
		Uploads:     uploads,
	}
	if bot != nil {
		// Assigned only when there IS one: a typed nil in an interface field
		// is not nil, and the endpoint's "the bot is off" check is a nil test.
		apiDeps.Bot = bot
	}
	if dropWatcher != nil {
		// Same rule as the bot above, and the same reason: a typed nil in an
		// interface field is not nil. NewHandler wires the upload path into
		// it; the background chain below starts it.
		apiDeps.Drop = dropWatcher
	}
	if login != nil {
		apiDeps.Login = login // the same typed-nil rule
	}
	if auto != nil {
		apiDeps.Auto = auto // the same typed-nil rule
	}
	api := httpapi.NewHandler(apiDeps)
	pages, err := web.New(web.Deps{DB: database, Auth: &auth.Resolver{DB: database, Cfg: cfg, Now: time.Now},
		URLs: u, Cfg: cfg, BuildID: buildID, Log: log, Now: time.Now, Budget: budget, Served: served,
		Health: storage})
	if err != nil {
		log.Error("boot: web", "err", err)
		_ = database.Close()
		return 1
	}
	root := http.NewServeMux()
	root.Handle("/api/", api)
	root.Handle("/healthz", api)
	root.Handle("/readyz", api)
	// oEmbed lives at the path every consumer's discovery link names, which is
	// not under /api/ (ROADMAP #18, §20 erratum 152). It answers JSON and
	// belongs to the API handler; only its address is page-shaped.
	root.Handle("/oembed", api)
	// The inbound URL, POST /in/{token} (§20 erratum 322), is the API's too: a
	// path with no route under /in/ is then the API's JSON 404, the answer a
	// wrong token must be indistinguishable from.
	root.Handle("/in/", api)
	root.Handle("/", pages)
	handler := httpx.RequestID()(httpx.AccessLog(log)(httpx.Recover(log)(root)))
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// 3. Listener UP before any storage contact.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("boot: listen", "addr", cfg.Listen, "err", err)
		_ = database.Close()
		return 1
	}
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	sigCtx, stopSignals := signal.NotifyContext(rootCtx, syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	// The served-bytes flush loop starts with the listener rather than with the
	// background chain below: the first request can arrive before the storage
	// ping has finished, and its bytes belong to a window that is ticking.
	go served.Run(sigCtx)
	log.Info("listening", "addr", ln.Addr().String())

	// Timed backups (§20 erratum 321), OFF unless BACKUP_EVERY and BACKUP_DIR
	// are both set. Beside the listener, not in the storage-gated chain below:
	// a database snapshot needs no bucket, and an unreachable S3_ENDPOINT must
	// not stop it.
	var backupDone chan struct{}
	if cfg.BackupOn() {
		backupDone = make(chan struct{})
		go func() {
			defer close(backupDone)
			newBackupScheduler(cfg, database, log).Run(sigCtx)
		}()
	}

	// 4. Background: storage ping loop → worker → sweeper.
	bgDone := make(chan struct{})
	var snapDone chan struct{}
	if snap != nil {
		snapDone = make(chan struct{})
	}
	go func() {
		defer close(bgDone)
		snapStarted := false
		defer func() {
			if snapDone != nil && !snapStarted {
				close(snapDone) // shut down before it was started: nothing to wait for
			}
		}()
		defer func() {
			if p := recover(); p != nil {
				log.Error("background boot panic", "err", p)
			}
		}()
		if unmarked != nil {
			storage.Set(unmarked)
			log.Warn("storage unreachable; serving degraded", "reason", "the store marker is missing (see the ERROR above)")
		} else {
			pingLoop(sigCtx, st, storage, log, cfg.StoragePingAttempts, cfg.StoragePingInterval)
		}
		if sigCtx.Err() != nil {
			return
		}
		// The database snapshot starts once the bucket has answered (or the
		// ping gave up): its first act is a read of the bucket, and a write it
		// could not make at boot waits a whole DB_SNAPSHOT_EVERY (§20 erratum
		// 343). Its own goroutine, waited for at shutdown beside the backup.
		// Never into a folder that is not this database's (erratum 348 (c)):
		// a snapshot written under an empty mount point is hidden the moment
		// the share is mounted over it.
		if snap != nil && unmarked == nil {
			snapStarted = true
			go func() {
				defer close(snapDone)
				snap.Run(sigCtx)
			}()
		}
		wk.Start(sigCtx)
		log.Info("worker started", "concurrency", cfg.Transcode.Concurrency)
		// The drop folder polls after the worker is up and after the store has
		// answered a ping, because everything it can do ends in an encode and
		// an unreachable store is a refusal it should never have to word
		// (ROADMAP #42 (b), §20 erratum 296). Its own goroutine: the sweeper
		// below blocks until shutdown, and one poll must never wait on the
		// other's tick.
		if dropWatcher != nil {
			go dropWatcher.Run(sigCtx)
		}
		// Clippet Auto's gateway, for the drop folder's reason: everything it
		// does ends in an upload (§20 erratum 333). Its own goroutine; it ends
		// with sigCtx and its jobs are waited for beside the bot's.
		if auto != nil {
			go auto.Run(sigCtx, cfg.DiscordAuto.Token())
		}
		log.Info("sweeper started", "interval", cfg.SweepInterval, "grace", cfg.SweepGrace, "batch", cfg.SweepBatch)
		sw.Start(sigCtx) // blocks until sigCtx is done
	}()

	exit := 0
	select {
	case <-sigCtx.Done():
		log.Info("shutdown: signal received; a second signal exits immediately")
	case err := <-serveErr:
		log.Error("http server stopped", "err", err)
		exit = 1
	}
	// Restore the default disposition now: a second SIGTERM/SIGINT during
	// the drain terminates the process at once instead of being swallowed
	// until the budgets expire (Docker's SIGKILL at stop_grace_period is the
	// backstop either way).
	stopSignals()

	// SIGTERM sequence (§18.1): cancel root ctx; Shutdown(15s) ∥ worker.Stop(20s);
	// then wait ≤5s for the sweeper; then close the DB.
	cancelRoot()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), httpDrainBudget)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Warn("http shutdown incomplete; closing", "err", err)
			_ = srv.Close()
		}
	}()
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), workerStopBudget)
		defer cancel()
		if err := wk.Stop(ctx); err != nil {
			log.Warn("worker stop incomplete", "err", err)
		}
		// The bot's in-flight `/clip` goroutines share the worker's budget:
		// they are the same kind of work (an upload and an encode), they are
		// bounded by the same shutdown, and a job cancelled here still edits
		// its deferred Discord reply to say so rather than leaving a
		// "thinking…" that never resolves (§20 erratum 256).
		if auto != nil {
			if err := auto.Shutdown(ctx); err != nil {
				log.Warn("discord auto: jobs still running at shutdown", "err", err)
			}
		}
		if bot != nil {
			if err := bot.Shutdown(ctx); err != nil {
				log.Warn("discord bot: jobs still running at shutdown", "err", err)
			}
		}
	}()
	wg.Wait()
	// ONE deadline for the sweeper chain and the backup loop together: the
	// backup waits under the same 5 s, not 5 s more (§20 erratum 321).
	stopBy, stopCancel := context.WithTimeout(context.Background(), sweeperStopBudget)
	select {
	case <-bgDone:
	case <-stopBy.Done():
		log.Warn("sweeper did not stop within budget", "budget", sweeperStopBudget)
	}
	if backupDone != nil {
		// A loop that already stopped is asked first: when the sweeper chain
		// overran the shared deadline both cases below are ready, and Go picks
		// one at random — half the time a WARN for a loop that stopped at once.
		select {
		case <-backupDone:
		default:
			select {
			case <-backupDone:
			case <-stopBy.Done():
				log.Warn("backup did not stop within budget", "budget", sweeperStopBudget)
			}
		}
	}
	if snapDone != nil {
		// Under the same deadline, asked first for the same reason (§20
		// erratum 343): a write in flight is cancelled by sigCtx and its scratch
		// copy removed; the database must still be open while it unwinds.
		select {
		case <-snapDone:
		default:
			select {
			case <-snapDone:
			case <-stopBy.Done():
				log.Warn("db snapshot did not stop within budget", "budget", sweeperStopBudget)
			}
		}
	}
	stopCancel()
	// The last served-bytes window, after the HTTP drain and before the database
	// closes (§20 erratum 270): every media response has finished by here, so
	// this is the one moment at which nothing more can be added. Its own loop
	// flushed when the signal arrived; this covers the bytes served during the
	// drain, and both calls are the same idempotent write of whatever is left.
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), sweeperStopBudget)
		defer cancel()
		if err := served.Flush(ctx); err != nil {
			log.Warn("the last served-bytes window could not be written", "err", err)
		}
	}()
	if err := database.Close(); err != nil {
		log.Warn("db close", "err", err)
	}
	log.Info("shutdown complete")
	return exit
}

// bootStoreMarker writes the folder store's marker when it is missing and the
// database accounts for no stored clips — a fresh folder — and refuses to when
// it does (§20 erratum 348 (c)): a marker missing over a database that says
// the folder should hold N bytes of clips is the shape of an unmounted NAS
// share or a mistyped bind, and writing it would erase the one signal `admin
// doctor` reads. It returns the error the ping loop then records, or nil.
//
// A database that cannot answer the sum keeps the old behaviour (the marker
// written, best effort): a boot that refused over a failed aggregate would
// trade a hypothetical wrong folder for a certain outage.
func bootStoreMarker(ctx context.Context, dir string, stored func(context.Context) (int64, error), log *slog.Logger) error {
	present, err := store.HasMarker(dir)
	if err == nil && present {
		return nil
	}
	if err == nil {
		if used, serr := stored(ctx); serr == nil && used > 0 {
			log.Error("boot: STORAGE_DIR has no "+store.MarkerName+" while the database accounts for stored clips: it is probably not the directory this database knows (an unmounted share, a mistyped bind) - the marker was NOT written and the instance serves degraded; check the mount, or if this IS the directory, create an empty file named "+store.MarkerName+" at its root and restart (DEPLOY.md §32.4)",
				"dir", dir, "stored_bytes", used)
			return &store.PingError{Class: store.PingUnavailable,
				Err: fmt.Errorf("store: %s is missing in %s while the database accounts for %d bytes of stored clips", store.MarkerName, dir, used)}
		}
	}
	if werr := store.WriteMarker(dir); werr != nil {
		log.Warn("boot: the store marker could not be written", "dir", dir, "err", werr)
	}
	return nil
}

// unmarkedStore is the folder store while bootStoreMarker refused to write the
// marker (§20 erratum 348 (c)): Ping is the refusal, so /readyz, /healthz and
// /setup's state cell all say degraded for as long as the process runs, and
// every write, delete and byte read answers the same error, so nothing lands
// in (or is "deleted" from) a folder that is not this database's — an upload
// fails, a tier job and the sweeper log and retry, exactly as against an
// unreachable store. Stat, List and PresignGet stay the store's own.
type unmarkedStore struct {
	store.Store
	err error
}

func (u unmarkedStore) Ping(context.Context) error { return u.err }

func (u unmarkedStore) PutFile(context.Context, string, string, string, func(int64, int64)) (store.PutResult, error) {
	return store.PutResult{}, u.err
}

func (u unmarkedStore) Download(context.Context, string, string, func(int64, int64)) error {
	return u.err
}

func (u unmarkedStore) GetRange(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, u.err
}

func (u unmarkedStore) DeleteKey(context.Context, string) error { return u.err }

func (u unmarkedStore) DeletePrefix(context.Context, string) (int, error) { return 0, u.err }

// pingLoop tries store.Ping up to `attempts` times, `interval` apart,
// recording every outcome in the shared cache. A final failure logs at WARN
// and returns — the process keeps serving degraded, never crash-loops.
// Both bounds are STORAGE_PING_ATTEMPTS / STORAGE_PING_INTERVAL (§20 erratum
// 283); the per-attempt budget is pingTimeout and is not configurable.
func pingLoop(ctx context.Context, st store.Store, health *httpx.StorageHealth, log *slog.Logger, attempts int, interval time.Duration) {
	for attempt := 1; attempt <= attempts; attempt++ {
		pctx, cancel := context.WithTimeout(ctx, pingTimeout)
		err := st.Ping(pctx)
		cancel()
		health.Set(err)
		if err == nil {
			log.Info("storage reachable", "attempt", attempt)
			return
		}
		log.Warn("storage ping failed", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
	log.Warn("storage unreachable; serving degraded", "attempts", attempts, "interval", interval)
}

func limits(cfg config.Config) db.Limits {
	return db.Limits{MaxAttempts: cfg.Transcode.MaxAttempts, MaxTransient: cfg.Transcode.MaxTransient}
}

// janitorStaleAge is the §12 mtime clause: a scratch dir untouched for this
// long is removed unless its clip is processing (the worker re-downloads the
// source from the bucket if it ever needs it again).
const janitorStaleAge = 24 * time.Hour

// scratchJanitor applies the full §12 janitor rule once at boot (the sweeper
// repeats it every 10th tick): remove $SCRATCH_DIR/{id} when the clip row is
// missing or in ready|failed|deleting AND the clip has no queued/running job
// left to run against that dir (§20 erratum 89 — it runs right after
// ReconcileOnStartup, which has just requeued whatever the crash interrupted),
// OR when the dir's mtime is older than
// janitorStaleAge and the clip is not processing. Row-less upload spools
// ($SCRATCH_DIR/.upload-{id}, §8.3) are removed unconditionally here: the
// listener is not open yet, so any such dir is residue of a crashed upload.
func scratchJanitor(ctx context.Context, dir string, database *db.DB, log *slog.Logger, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Warn("janitor: read scratch", "dir", dir, "err", err)
		return
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if scratch.IsUploadTmp(e.Name()) {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				log.Warn("janitor: remove", "dir", e.Name(), "err", err)
				continue
			}
			removed++
			continue
		}
		if !ids.Valid(e.Name()) {
			continue
		}
		c, err := database.GetClip(ctx, e.Name())
		remove := false
		switch {
		case errors.Is(err, db.ErrNotFound):
			remove = true
		case err != nil:
			log.Warn("janitor: lookup", "clip", e.Name(), "err", err)
			continue
		case c.State == db.ClipReady || c.State == db.ClipFailed || c.State == db.ClipDeleting:
			// A trim or a gif runs against a `ready` clip and uses this dir,
			// so the state is not the question — the job table is (§20
			// erratum 89). At boot nothing is running yet and reconcile has
			// just requeued what was, which is exactly what this asks about.
			live, lerr := database.HasLiveJob(ctx, e.Name())
			if lerr != nil {
				log.Warn("janitor: jobs", "clip", e.Name(), "err", lerr)
				continue
			}
			remove = !live
		case c.State != db.ClipProcessing:
			info, ierr := e.Info()
			if ierr != nil {
				log.Warn("janitor: stat", "clip", e.Name(), "err", ierr)
				continue
			}
			remove = now.Sub(info.ModTime()) > janitorStaleAge
		}
		if !remove {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			log.Warn("janitor: remove", "clip", e.Name(), "err", err)
			continue
		}
		removed++
	}
	log.Info("janitor", "scanned", len(entries), "removed", removed)
}

var ffmpegVersionRE = regexp.MustCompile(`^ffmpeg version (\S+)`)

// resolveFFmpeg locates both binaries (fatal if missing) and reads the
// ffmpeg version string printed at boot and by /healthz.
func resolveFFmpeg(tc config.TranscodeConfig) (ffmpeg, ffprobe, version string, err error) {
	if ffmpeg, err = exec.LookPath(tc.FFmpeg); err != nil {
		return "", "", "", fmt.Errorf("ffmpeg %q not found: %w", tc.FFmpeg, err)
	}
	if ffprobe, err = exec.LookPath(tc.FFprobe); err != nil {
		return "", "", "", fmt.Errorf("ffprobe %q not found: %w", tc.FFprobe, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffmpeg, "-version").Output()
	if err != nil {
		return "", "", "", fmt.Errorf("ffmpeg -version: %w", err)
	}
	return ffmpeg, ffprobe, parseFFmpegVersion(string(out)), nil
}

func parseFFmpegVersion(out string) string {
	first, _, _ := strings.Cut(out, "\n")
	if m := ffmpegVersionRE.FindStringSubmatch(first); m != nil {
		return m[1]
	}
	return "unknown"
}

// The development credentials printed in this repository. compose fell back to
// them when .env set none until §20 erratum 345 withdrew the bundled MinIO;
// today only the test harness's own bucket server uses them, which is fine on
// loopback and is not fine anywhere else.
const (
	shippedAccessKey = "clippet"
	shippedSecretKey = "clippet-dev-secret"
)

// warnShippedCredentials logs ONE line when the instance is running on the
// PUBLISHED development credentials while its public origin is not loopback
// (§20 erratum 104). Until §20 erratum 345 compose handed them to the app
// whenever `S3_SECRET_KEY=` was empty, so a half-finished switch to R2/B2
// landed here silently; compose falls back to no key any more, and the pair
// now only arrives by being typed. It stays a warning and not a boot failure
// for the reason it always was: the exposure depends on whether the bucket
// server is reachable at all, which this process cannot see.
//
// The reachability half is inferred from PUBLIC_BASE_URL, not detected: a
// container cannot see its own port publish, so this warns on the COMBINATION,
// exactly as the TRUSTED_PROXIES warning above does (erratum 96). A dev stack
// on localhost:8481 stays quiet, which is what keeps the line worth
// reading. The values themselves are never logged.
func warnShippedCredentials(log *slog.Logger, cfg config.Config) {
	if cfg.S3.AccessKey != shippedAccessKey && cfg.S3.SecretKey != shippedSecretKey {
		return
	}
	if cfg.PublicBaseURL == nil || isLoopbackHost(cfg.PublicBaseURL.Hostname()) {
		return
	}
	// COPY? (§20 erratum 345: the MINIO_ROOT_* alternative and the closing
	// "NOTE an EMPTY value in .env counts as unset" removed; nothing added)
	log.Warn("the bucket credentials are the ones published in this repository, and PUBLIC_BASE_URL is not loopback - set S3_ACCESS_KEY and S3_SECRET_KEY in .env and recreate (DEPLOY.md §7)",
		"public_base_url", cfg.PublicBaseURL.String())
}

// isLoopbackHost reports whether a URL host names this machine only. A name
// that is not an address is loopback ONLY when it is exactly "localhost": any
// other hostname resolves somewhere, and this check must not guess where.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsLoopback() || addr.IsUnspecified()
	}
	return false
}

func newLogger(cfg config.Config) *slog.Logger {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// uploadFormWord is DISCORD_UPLOAD_FORM as the boot line prints it (§20
// erratum 320): the word the operator would write.
func uploadFormWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
