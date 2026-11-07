// The Discord bot's outbound half (ROADMAP #33, DESIGN.md §20 errata
// 253-256, 268, 286, 315).
//
// It lives in this package for one reason: internal/notify is the ONLY place
// in the binary allowed to build an HTTP client (§15, §20 errata 66/96, and
// scripts/check.sh's tripwire, which excludes exactly this directory). The
// interactions ENDPOINT — the signature check, the guild lookup, the replies
// Discord reads off our HTTP response — is inbound and lives in
// internal/httpapi; everything here is a call this binary makes.
//
// Six calls, and no others:
//
//	GET    <cdn host>/…                                  the attachment (`Range: bytes=0-`, erratum 340)
//	PATCH  discord.com/api/v10/webhooks/{app}/{token}/messages/@original  the deferred reply (and its byte count)
//	POST   discord.com/api/v10/webhooks/{app}/{token}                    the link (and "Uploading…") after the upload form
//	PATCH  discord.com/api/v10/webhooks/{app}/{token}/messages/{id}      a late title or a late card: the link redrawn; "Uploading…"'s count
//	DELETE discord.com/api/v10/webhooks/{app}/{token}/messages/{id}      "Uploading…" gone when the job ends
//	PUT    discord.com/api/v10/applications/{app}/guilds/{guild}/commands
//
// The middle four carry no credential at all: an interaction token IS the
// authentication for its own follow-up, which is why the bot token is sent on
// exactly one route (the registration) and never leaves this file otherwise —
// and why all four work unchanged for an instance on the official bot.
// A job uses the @original edit OR the follow-up routes (§20 errata 320, 325):
// after a deferral there is a "thinking…" to edit; after the upload form (a
// modal answers the command, so no message was created) every message the job
// ends in is a NEW follow-up message on the command's token, and the last two
// routes touch only messages this binary itself posted (the redraw of §20
// erratum 330 also edits a deferred or type-4 reply, as `messages/@original`).
//
// **Two secrets, both scrubbed.** The bot token (DISCORD_BOT_TOKEN) and the
// interaction token, which authenticates anyone holding it to edit that
// interaction's message for fifteen minutes. Both are removed from every
// string that leaves this package, by the same longest-first replacement the
// webhook endpoint gets (§20 errata 63, 84).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"clippet/internal/urls"
)

// Budgets. Each bounds ONE call, except jobBudget, which bounds a whole
// `/clip` from the moment the endpoint deferred it. The interaction token is
// valid for 15 minutes and Discord's own deadline for a deferred reply is the
// same, so the ceiling here is not Discord's — it is ours, and it exists so a
// goroutine cannot outlive the reason it was started.
const (
	botCallTimeout  = 10 * time.Second
	botFetchTimeout = 5 * time.Minute
	botJobBudget    = 12 * time.Minute
	// botEditRetryWait is the one pause before the single retry of the
	// follow-up edit. The endpoint answers `type: 5` (deferred) on the HTTP
	// response Discord is still reading when this goroutine starts, so a very
	// fast job can PATCH a message Discord has not created yet and get
	// "Unknown Message". One retry, one second, and the log says so.
	botEditRetryWait = time.Second
	// botAPIPath is the versioned API prefix. v10 is the current stable
	// version and the one both routes below are documented under.
	botAPIPath = "/api/v10"
	// botCommand is the DEFAULT name of the LINK command, `/clip link:` — what
	// an instance publishes unless DISCORD_COMMAND_NAME says otherwise (§20
	// erratum 284). Since erratum 315 the bot publishes TWO commands, this one
	// and botUploadCommand (the owner's call, 2026-10-05: /upload makes a clip,
	// /clip is the formal way to show one). Both values are spelled here and in
	// internal/config, and pinned against both by a test in internal/httpapi,
	// because §3 forbids the imports that would make them one constant.
	botCommand = "clip"
	// botUploadCommand is the DEFAULT name of the UPLOAD command, `/upload
	// file:` — what the one command of errata 253-286 became under erratum 315,
	// renamed and otherwise unchanged (DISCORD_UPLOAD_COMMAND_NAME renames it).
	botUploadCommand = "upload"
	// botEnrichCommand is the DEFAULT name of the MESSAGE command — a message's
	// Apps menu → "Enrich" (§20 erratum 315's addendum; DISCORD_ENRICH_COMMAND_NAME
	// renames it). It posts the same message as `/clip link:` for the first link
	// to a clip of this instance in the message it was used on.
	botEnrichCommand = "Enrich"
	// botSettingsCommand is the DEFAULT name of the SETTINGS command (§20
	// erratum 318; DISCORD_SETTINGS_COMMAND_NAME renames it): an ephemeral panel
	// whose one row opens the bound account's link defaults in a modal. The
	// interaction never leaves internal/httpapi; this file only publishes it.
	botSettingsCommand = "settings"
	// botSigninCommand is `/signin` (§20 erratum 341): a one-time login link for
	// the member who runs it. A FIXED name (internal/config's SigninCommandName,
	// pinned against this by a test in internal/httpapi): there is no variable.
	botSigninCommand = "signin"
	// botFileOption is the attachment option's name — what a user types after
	// `/upload`. It is the command's ONLY option and it is REQUIRED (§20
	// erratum 286), so Discord refuses an empty `/upload` in its own client and
	// the endpoint never has to.
	botFileOption = "file"
	// botLinkOption is the LINK command's only option, a STRING and REQUIRED
	// (§20 erratum 315): a share link of this instance, parsed by the endpoint
	// and never fetched.
	botLinkOption = "link"
	// maxDescription is Discord's limit on a command's or an option's
	// description: 100 characters, enforced by the registration call. Past it
	// the PUT is rejected and the server ends up with NO `/clip` at all, so the
	// description that names this instance's host is built to fit rather than
	// sent to find out.
	maxDescription = 100
)

// BotDeps configures a Bot.
//
// APIBase and AllowAttachment are injected for the reason Deps.Endpoint is a
// plain string: the ALLOWLIST lives in internal/config and is tested there, so
// it must not also be the thing that decides where a unit test may point. In
// production serve.go passes config.DiscordAPIHost() and
// config.ValidAttachmentURL; the tests here drive httptest servers and never
// the network.
type BotDeps struct {
	// AppID is the application snowflake (not secret).
	AppID string
	// Token is the bot token (SECRET), used only by Register.
	Token string
	// Managed says the token is config.ManagedToken — this instance answers
	// the OFFICIAL bot (§20 erratum 279): Register refuses rather than send
	// the sentinel to Discord, and nothing is scrubbed for it (it is a word,
	// not a credential). Everything else — the deferral, the fetch, the
	// follow-up edit — is identical, because none of it ever needed the token.
	Managed bool
	// CommandName is the LINK command Register publishes, without the slash
	// (DISCORD_COMMAND_NAME; §20 errata 284, 315). Empty means botCommand, the
	// default — this package validates nothing, because internal/config already
	// refused everything Discord would: it is passed in for the reason APIHost
	// is, so the rule has exactly one home.
	CommandName string
	// UploadCommandName is the UPLOAD command, the same way
	// (DISCORD_UPLOAD_COMMAND_NAME; §20 erratum 315). Empty means
	// botUploadCommand.
	UploadCommandName string
	// EnrichCommandName is the MESSAGE command, the same way
	// (DISCORD_ENRICH_COMMAND_NAME; §20 erratum 315's addendum). Empty means
	// botEnrichCommand.
	EnrichCommandName string
	// SettingsCommandName is the SETTINGS command, the same way
	// (DISCORD_SETTINGS_COMMAND_NAME; §20 erratum 318). Empty means
	// botSettingsCommand.
	SettingsCommandName string
	URLs                urls.Builder
	Log                 *slog.Logger
	// APIHost is the Discord API host ("" is not allowed; serve.go passes
	// config.DiscordAPIHost()). A test passes an httptest server's host.
	APIHost string
	// APIScheme is "https" in production. A test passes "http" for its
	// httptest server — this is the only knob that can make this package
	// speak plain HTTP, and nothing but a test sets it.
	APIScheme string
	// AllowAttachment is the egress gate on an attachment URL
	// (config.ValidAttachmentURL in production). nil means nothing may be
	// fetched, which is the safe default for a Bot built without one.
	AllowAttachment func(string) (*url.URL, error)
	// MaxBytes is MAX_UPLOAD_BYTES: an attachment larger than this is refused
	// before a byte is fetched, and the fetch is bounded by it anyway.
	MaxBytes int64
	// Timeouts (tests only; 0 = the constants above).
	CallTimeout, FetchTimeout, JobBudget time.Duration
	// Transport overrides the client's transport (tests only).
	Transport http.RoundTripper
}

// Bot makes the three calls above and owns the goroutines that run a `/clip`
// after its interaction has been answered.
type Bot struct {
	appID       string
	token       string   // SECRET
	managed     bool     // the token is the sentinel: Register refuses (erratum 279)
	command     string   // the LINK command's name, without the slash (errata 284, 315)
	upload      string   // the UPLOAD command's name, without the slash (erratum 315)
	enrich      string   // the MESSAGE command's name (erratum 315's addendum)
	settingsCmd string   // the SETTINGS command's name, without the slash (erratum 318)
	secrets     []string // every spelling of the bot token, longest first
	urls        urls.Builder
	log         *slog.Logger
	scheme      string
	host        string
	allow       func(string) (*url.URL, error)

	maxBytes                             int64
	callTimeout, fetchTimeout, jobBudget time.Duration
	client                               *http.Client

	// ctx/cancel bound every running job to the process: Shutdown cancels it
	// and then waits, and a job that was cancelled says so in the message it
	// leaves behind rather than disappearing.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// mu guards `closed`, which is raised by Shutdown so no job can be added
	// to the WaitGroup while it is being waited on.
	mu     sync.Mutex
	closed bool
	// links is what a redraw needs (§20 errata 325, 330): per held link, the
	// interaction token and the message's id. Memory only, bounded, gone on a
	// restart.
	links *linkTable
	// linkIDs is the link follow-up's message id per interaction token (§20
	// erratum 320 (g)): a second link follow-up on one token edits, never
	// posts. Memory only, bounded, gone on a restart.
	linkIDs *linkIDTable
	// tick is the count's ticker (§20 erratum 340): newTicker in production; a
	// test swaps in a channel it feeds before the first job starts.
	tick func(time.Duration) (<-chan time.Time, func())
}

// NewBot returns a Bot, or nil when the application is not configured — the
// OFF state, in which no client is built at all.
func NewBot(d BotDeps) *Bot {
	if d.AppID == "" || d.Token == "" || d.APIHost == "" {
		return nil
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	scheme := d.APIScheme
	if scheme == "" {
		scheme = "https"
	}
	secrets := tokenSpellings(d.Token)
	if d.Managed {
		secrets = nil // a word, not a credential: scrubbing it would eat honest text
	}
	command := d.CommandName
	if command == "" {
		command = botCommand
	}
	upload := d.UploadCommandName
	if upload == "" {
		upload = botUploadCommand
	}
	enrich := d.EnrichCommandName
	if enrich == "" {
		enrich = botEnrichCommand
	}
	settingsCmd := d.SettingsCommandName
	if settingsCmd == "" {
		settingsCmd = botSettingsCommand
	}
	b := &Bot{
		appID: d.AppID, token: d.Token, managed: d.Managed, command: command, upload: upload, enrich: enrich, settingsCmd: settingsCmd, secrets: secrets,
		urls: d.URLs, log: log, scheme: scheme, host: d.APIHost,
		allow: d.AllowAttachment, maxBytes: d.MaxBytes,
		callTimeout:  orDuration(d.CallTimeout, botCallTimeout),
		fetchTimeout: orDuration(d.FetchTimeout, botFetchTimeout),
		jobBudget:    orDuration(d.JobBudget, botJobBudget),
		links:        newLinkTable(),
		linkIDs:      newLinkIDTable(),
		tick:         newTicker,
	}
	// ONE client, exactly as the Notifier has one: the §15 tripwire is about
	// how many ways this binary can reach the network. Redirects are not
	// followed, for erratum 27's reason — a 3xx off an allowlisted host is
	// the laundering the allowlist exists to prevent, and on the CDN route it
	// would also be the way to make this binary fetch an arbitrary URL.
	//
	// Its own Timeout is ZERO and every budget is a CONTEXT instead, because
	// this client carries two very different calls: a JSON API request that
	// must not hang for ten seconds, and the download of an attachment that
	// may be gigabytes. http.Client.Timeout covers reading the body too, so
	// one value for both would either cut a large fetch off or let a wedged
	// API call sit. call() and fetchAndIngest each bound their own.
	b.client = newHTTPClient(0, d.Transport)
	b.ctx, b.cancel = context.WithCancel(context.Background())
	return b
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// newHTTPClient is the ONE construction site for an outbound client in this
// binary (§15, §20 erratum 66). Both the webhook notifier and the bot build
// theirs here so the policy — never following a redirect off the host the
// allowlist approved — is written once. A zero timeout means "the caller's
// context is the only budget", which is what the bot needs and what the
// notifier, whose every call is one small POST, does not use.
func newHTTPClient(timeout time.Duration, tr http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// AppID is the application this bot answers for (not secret).
func (b *Bot) AppID() string { return b.appID }

// CommandName is the LINK command this bot publishes, without the slash
// (§20 errata 284, 315). The CLI prints it where it used to print a literal
// `/clip`.
func (b *Bot) CommandName() string { return b.command }

// UploadCommandName is the UPLOAD command this bot publishes, without the
// slash (§20 erratum 315).
func (b *Bot) UploadCommandName() string { return b.upload }

// EnrichCommandName is the MESSAGE command this bot publishes (§20 erratum
// 315's addendum).
func (b *Bot) EnrichCommandName() string { return b.enrich }

// SettingsCommandName is the SETTINGS command this bot publishes, without the
// slash (§20 erratum 318).
func (b *Bot) SettingsCommandName() string { return b.settingsCmd }

// --- the ingest hand-off ----------------------------------------------------

// IngestSource is one attachment on its way into the library: the bytes, and
// the two facts Discord states about them.
type IngestSource struct {
	Body        io.Reader
	Filename    string
	ContentType string
	Size        int64
}

// IngestResult is what the app made of it.
type IngestResult struct {
	ClipID string
	// CardReady is whether the clip's CARD works yet (db.CardReady). False
	// means the link is posted anyway — Discord never reads that URL again by
	// itself — and held so the bot redraws it once the worker says the card is
	// ready (§20 erratum 330); it is logged, never shown.
	CardReady bool
	// Existing says the bound account already held a live clip of exactly
	// these bytes and ClipID is that clip: nothing was created (§20 erratum
	// 326). Its link is posted exactly as a new one would be; a Form job also
	// tells the person, privately, that the form was not applied.
	Existing bool
	// SubmitDropped, on a Form job's duplicate, says a submit that had already
	// been answered "Changes saved." was dropped: the job then tells that person,
	// privately, that the form was not applied. A submit that arrives later
	// hears it from its own answer, so the line is said once (run 8).
	SubmitDropped bool
}

// Ingest is the app's own upload path, handed in by the caller. It is a
// callback rather than an import because internal/httpapi owns the ingest core
// and importing it here would be a cycle (httpapi → notify → httpapi).
type Ingest func(ctx context.Context, src IngestSource) (IngestResult, error)

// IngestError is a failure the person who ran the command may be told about,
// in one plain sentence chosen by the app. Anything else is reported as the
// generic sentence, because an unclassified error is exactly the one whose
// text nobody has checked for a path, a bucket name or a credential.
type IngestError struct {
	Sentence string
	Err      error
}

func (e *IngestError) Error() string {
	if e.Err == nil {
		return e.Sentence
	}
	return e.Sentence + ": " + e.Err.Error()
}

func (e *IngestError) Unwrap() error { return e.Err }

// ClipJob is one `/upload` that has been deferred and now has to be finished
// (the name is history: until §20 erratum 315 the upload command was `/clip`).
type ClipJob struct {
	// Token is the interaction token — a SECRET for fifteen minutes: it
	// authenticates the follow-up edit, and anyone holding it can rewrite
	// this reply.
	Token string
	// URL is the attachment, as Discord resolved it.
	URL string
	// Filename, ContentType and Size are Discord's own metadata for it.
	Filename, ContentType string
	Size                  int64
	// GuildID is for the log line only (not secret).
	GuildID string
	// Command is the command the job's log lines name (run 8): `/upload`,
	// `/clip` (an old `/clip file:`) or `Enrich`. Empty reads as `/upload`.
	// The lines' messages keep their `/clip` wording, which the E2E greps.
	Command string
	// Ingest is the app's upload path — the whole of a job's work since §20
	// erratum 286 withdrew the `link` option, which was the one kind of job that
	// had none.
	Ingest Ingest
	// Followup says the command was answered with the upload form, a modal,
	// rather than deferred (§20 erratum 320): there is no "thinking…" message to
	// edit, so EVERY message this job ends in — the link, a refusal sentence,
	// the restart sentence, the panic's sentence — is posted as a new follow-up
	// message on the command's token. False is the deferred path, unchanged.
	Followup bool
	// Uploading, on a Followup job, posts SentenceUploading as an EPHEMERAL
	// follow-up the moment the job starts — the only feedback the person gets
	// while the upload runs, since the modal replaced the "thinking…" (§20
	// erratum 325) — and deletes it when the job ends, after the link or the
	// failure sentence is out. Neither call can change the job: a line that
	// will not post is skipped, one that will not delete is left.
	Uploading bool
	// Failed, when set, is called when the job ends without a clip — on any
	// error, before the failure is logged, and from the panic recovery unless
	// the token already holds the link (§20 erratum 320 (g)) — so
	// the upload form's table learns that a later submit has nothing to apply
	// to (§20 erratum 320). It is called at most twice (runClip's error path,
	// then the recovery if deliver panics), so it must be idempotent, and it
	// must not block. nil is allowed.
	Failed func()
	// Form says the job is behind the upload form (§20 erratum 320). When the
	// upload turns out to be a duplicate (IngestResult.Existing, §20 erratum
	// 326), the form's fields are never applied to the existing clip, and —
	// when a submit already answered was dropped (IngestResult.SubmitDropped) —
	// the person gets SentenceFormNotApplied as an ephemeral follow-up; the
	// link is posted unchanged and is never held for a redraw.
	Form bool
}

// command is the job's command for a log line.
func (job ClipJob) command() string {
	if job.Command == "" {
		return "/" + botUploadCommand
	}
	return job.Command
}

// failed calls job.Failed, nil-safe and panic-safe: the hook is the endpoint's
// and a fault inside it must not cost the person their message.
func (b *Bot) failed(job ClipJob) {
	if job.Failed == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			b.log.Error("discord bot: the failure hook panicked", "guild", job.GuildID, "err", p)
		}
	}()
	job.Failed()
}

// The sentences the bot can leave in place of a link. They are deliberately
// plain, short and free of jargon: whoever ran the command is looking at a
// chat message, not at a log. None of them names a path, a bucket, a variable
// or an error — the operator's detail goes to the app log, where it belongs
// (§11, §20 erratum 256).
// They are exported because the INGEST side chooses some of them
// (internal/httpapi's botIngest maps the ingest core's verdict onto them), and
// the whole set is worth reading in one place.
const (
	// SentenceFetch — the attachment could not be read from Discord's CDN
	// (a refused host, a redirect, a non-200, a short body).
	SentenceFetch = "That attachment could not be fetched from Discord."
	// SentenceTooLarge — past MAX_UPLOAD_BYTES, by Discord's own size or by
	// the bytes that actually arrived.
	SentenceTooLarge = "That file is larger than this Clippet accepts."
	// SentenceNotVideo — ffprobe (or the content type) says it is not one.
	SentenceNotVideo = "That file is not a video or audio Clippet can play."
	// SentenceBusy — the bound account's upload budget is spent (§14).
	SentenceBusy = "That account has hit its upload limit — try again in a bit."
	// SentenceFull — the scratch disk has no room for it.
	SentenceFull = "This Clippet is out of disk space right now."
	// SentenceQuota — the instance is at the storage quota its operator set
	// (STORAGE_BUDGET_HARD=1; §20 erratum 277). It is a DELIBERATE line and
	// not a fault, so it says what to do about it rather than apologising —
	// and it is the same sentence the 507 carries on the upload routes, so
	// two people hitting it two ways compare notes and hear one thing.
	SentenceQuota = "This instance is at its storage quota; delete or expire some clips, or raise the quota."
	// SentenceFailed — anything else. Deliberately vague: an unclassified
	// failure is the one whose text nobody has checked for a path or a
	// credential, and the operator's detail is in the app log.
	SentenceFailed = "That clip could not be uploaded."
	// SentenceFormNotApplied — the upload form's duplicate (§20 erratum 326;
	// the owner's words): the file was already a clip of the bound account, its
	// link is posted, and what was typed into the form is not applied to it.
	// Ephemeral: only the person who ran the command sees it.
	SentenceFormNotApplied = "Already in your library. The form was not applied."
)

// SentenceRestart — the process was shutting down mid-job. It names the command,
// so on a renamed instance (§20 erratum 284) it is built from the configured
// name by sentenceRestart. The job is an UPLOAD, so since §20 erratum 315 the
// command it names is the upload command (DISCORD_UPLOAD_COMMAND_NAME); this is
// the default instance's sentence, and what the tests pin.
var SentenceRestart = sentenceRestart(botUploadCommand)

// sentenceRestart is the channel-visible restart sentence for the command this
// instance publishes: a person told to run a command must be told the one that
// exists here.
func sentenceRestart(command string) string {
	return "Clippet was restarting — run /" + command + " again."
}

// Clip finishes a deferred `/clip` in a goroutine of its own, bounded by the
// process: Shutdown cancels every one of them and waits.
//
// It never blocks the caller — the endpoint has three seconds to answer
// Discord and has already spent some of them — and it never returns an error,
// because there is nobody left to return one to: the outcome IS the message it
// edits into place.
func (b *Bot) Clip(job ClipJob) {
	// The counter is raised under the same lock Shutdown closes with. Without
	// it, an interaction that arrives during the drain — the HTTP server's 15 s
	// and the worker's 20 s overlap, so it can — would call wg.Add(1) beside
	// wg.Wait(), which is the one WaitGroup misuse the runtime panics on. A job
	// started after the shutdown could not finish anyway: its context is
	// already cancelled.
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.log.Warn("discord bot: /clip arrived during shutdown and was not started", "guild", job.GuildID,
			"command", job.command())
		b.failed(job) // no clip will come of it: a form submit must not answer "Changes saved."
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				// A panic here would take the process down with it and leave
				// the interaction hanging for fifteen minutes. One log line,
				// one honest message, and the rest of the app lives.
				// A token that already holds the link (§20 erratum 320 (g)): the
				// panic came after it (the in-job redraw), so the clip exists —
				// the form's table is not told "no clip", and no sentence goes
				// under a working link.
				linked := b.linkIDs.get(job.Token, time.Now()) != ""
				if !linked {
					b.failed(job)
				}
				b.log.Error("discord bot: /clip panicked", "guild", job.GuildID, "command", job.command(), "err", p)
				if !linked {
					b.deliver(context.WithoutCancel(b.ctx), job, newPayload(SentenceFailed), "")
				}
			}
		}()
		b.runClip(job)
	}()
}

// Attachment is one Discord attachment a Clippet Auto URL named (§20 erratum
// 324): Discord's CDN link and its own metadata, as Clippet Auto read them off
// the message. Tag is the api_tokens row id, the one thing about the request
// the job's log lines may name.
type Attachment struct {
	URL, Filename, ContentType string
	Size                       int64
	Tag                        string
}

// FetchOutcome is how a Fetch job ended: the ingest's result, or the error.
type FetchOutcome struct {
	Result IngestResult
	Err    error
}

// ErrClosed is Fetch's answer once Shutdown has begun: no job is started.
var ErrClosed = errors.New("notify: the bot is shutting down")

// Fetch is a Clippet Auto URL's job (§20 erratum 324): the SAME fetcher a
// deferred `/upload` runs — fetchAndIngest, behind the same allowlist, size
// guard and no-redirect client — with no interaction to answer. The caller
// reads one outcome off the returned channel, or stops reading: it is
// buffered, so a job that outlives its request never blocks.
//
// It runs under the Bot's own context and job budget, never the request's, in
// a goroutine counted in the Bot's WaitGroup under the lock Shutdown closes
// with (as Clip), so Shutdown waits for it; once Shutdown has begun it is
// refused with ErrClosed. A panic is recovered as Clip recovers one.
//
// Its log lines carry Tag, the clip id, an outcome word and the duration —
// never the URL (a signed CDN link is a bearer link to the file), the file
// name, the guild or an error's text.
func (b *Bot) Fetch(att Attachment, ingest Ingest) (<-chan FetchOutcome, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrClosed
	}
	b.wg.Add(1)
	b.mu.Unlock()
	out := make(chan FetchOutcome, 1)
	go func() {
		defer b.wg.Done()
		started := time.Now()
		defer func() {
			if p := recover(); p != nil {
				// The value's Go TYPE only (§20 erratum 327): enough to tell a
				// nil map from an index out of range, and never the value's
				// text, which could carry the URL or the file name.
				b.log.Error("discord bot: autofetch panicked", "autofetch", att.Tag, "outcome", "panic",
					"panic_type", fmt.Sprintf("%T", p),
					"took", time.Since(started).Round(time.Millisecond).String())
				out <- FetchOutcome{Err: errors.New("the job panicked")}
			}
		}()
		ctx, cancel := context.WithTimeout(b.ctx, b.jobBudget)
		defer cancel()
		// No count (§20 erratum 340): nobody is waiting on a line. The fetch's
		// own log line is the shared function's, so Auto gets it too.
		res, err := b.fetchAndIngest(ctx, ClipJob{URL: att.URL, Filename: att.Filename,
			ContentType: att.ContentType, Size: att.Size, Ingest: ingest}, nil)
		took := time.Since(started).Round(time.Millisecond).String()
		if err != nil {
			b.log.Warn("discord bot: autofetch failed", "autofetch", att.Tag,
				"outcome", fetchOutcomeWord(ctx, err), "took", took)
		} else {
			b.log.Info("discord bot: autofetch", "autofetch", att.Tag, "clip", res.ClipID,
				"card_ready", res.CardReady, "outcome", "ok", "took", took)
		}
		out <- FetchOutcome{Result: res, Err: err}
	}()
	return out, nil
}

// fetchOutcomeWord is a Fetch failure as one closed word for the log: the
// sentence the app chose, never the error's own text.
func fetchOutcomeWord(ctx context.Context, err error) string {
	var ie *IngestError
	if errors.As(err, &ie) {
		switch ie.Sentence {
		case SentenceFetch:
			return "fetch"
		case SentenceTooLarge:
			return "too_large"
		case SentenceNotVideo:
			return "not_video"
		case SentenceBusy:
			return "busy"
		case SentenceFull:
			return "full"
		case SentenceQuota:
			return "quota"
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return "restart"
	}
	return "failed"
}

// Shutdown cancels every running job and waits for them, bounded by ctx. It is
// joined to the worker's own Stop budget in serve.go: a `/clip` mid-fetch when
// SIGTERM arrives leaves a "run it again" message rather than silence.
func (b *Bot) Shutdown(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	done := make(chan struct{})
	go func() { defer close(done); b.wg.Wait() }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runClip is the whole of a deferred `/clip`: the work, then the edit.
func (b *Bot) runClip(job ClipJob) {
	ctx, cancel := context.WithTimeout(b.ctx, b.jobBudget)
	defer cancel()
	started := time.Now()
	var line *heldLine
	if job.Followup && job.Uploading {
		// Posted beside the fetch, never before it: the first follow-up after a
		// modal can meet Discord's one-second 404 and its retry, and the upload
		// must not wait on a courtesy line. Deleted by this deferred call on
		// every way out — the link, a failure sentence, a panic's (which the
		// recovery in Clip posts after this has run).
		line = newHeldLine(b.postUploading(ctx, job))
		defer b.deleteUploading(job, line)
	}
	// The count (§20 erratum 340): the line above — or, on the deferred path,
	// the "thinking…" — reads `Uploading… 120 of 263 MB` while the bytes land.
	res, err := b.fetchAndIngest(ctx, job, b.progressFor(job, line))
	if err != nil {
		// The edit must survive the cancellation that may have caused the
		// failure, or a shutdown would fail silently — which is the one
		// outcome the person who ran the command cannot act on.
		editCtx, editCancel := context.WithTimeout(context.WithoutCancel(ctx), b.callTimeout)
		defer editCancel()
		sentence := b.sentenceFor(ctx, err)
		// The form's table first (§20 erratum 320): every way a job ends without
		// a clip lands here — a refused host, a CDN non-200 and a declared size
		// all return before the ingest is ever called — and the log line below
		// is what a test waits on, so the table is settled by then.
		b.failed(job)
		b.log.Warn("discord bot: /clip failed", "guild", job.GuildID, "command", job.command(),
			"file", job.Filename, "err", b.scrubJob(err.Error(), job.Token))
		b.deliver(editCtx, job, newPayload(sentence), "")
		return
	}
	b.log.Info("discord bot: /clip", "guild", job.GuildID, "command", job.command(), "clip", res.ClipID,
		"card_ready", res.CardReady, "existing", res.Existing,
		"took", time.Since(started).Round(time.Millisecond).String())
	if res.Existing {
		// The duplicate (§20 erratum 326): the existing clip's link, posted as a
		// new one would be but never held for a redraw — the form is not applied
		// to a clip that was already there, so nothing will change its title,
		// and its card is the one that clip already has (erratum 330).
		if job.Followup && job.Form && res.SubmitDropped {
			b.notApplied(ctx, job)
		}
		b.deliver(ctx, job, b.reply(res), res.ClipID)
		return
	}
	// The message (§20 errata 315, 316): the share link alone, which Discord
	// unfurls into the card the clip's page declares. The SAME message `/clip
	// link:` answers with, built by the same function. It is held for a redraw
	// when its card may still change (§20 errata 325, 330): a form job's title,
	// and a card that was not ready when the budget ran out.
	b.deliverLink(ctx, job, res.ClipID, owedFor(job, res), started)
}

// owedFor is what may still redraw a job's link (§20 errata 325, 330).
func owedFor(job ClipJob, res IngestResult) redrawReason {
	var owed redrawReason
	if job.Followup {
		owed |= reasonTitle
	}
	if !res.CardReady {
		owed |= reasonCard
	}
	return owed
}

// deliver is the ONE way a job's message leaves (§20 erratum 320): an edit of
// the deferred reply, or — after the upload form, where no reply exists — a
// new follow-up message on the command's token. The link, every failure
// sentence, the restart sentence and the panic's sentence all go through it.
// clipID names the clip when msg is that clip's LINK ("" for a sentence): a
// link follow-up is the one held per token (§20 erratum 320 (g)).
func (b *Bot) deliver(ctx context.Context, job ClipJob, msg any, clipID string) {
	if !job.Followup {
		b.editMessage(ctx, job.Token, msg)
		return
	}
	var err error
	if clipID != "" {
		_, err = b.followupLink(ctx, job.Token, msg, clipID)
	} else {
		_, err = b.followup(ctx, job.Token, msg)
	}
	if err != nil {
		b.log.Warn("discord bot: the follow-up could not be posted",
			"err", b.scrubJob(err.Error(), job.Token))
	}
}

// sentenceFor picks the message the reply becomes. A cancelled job is a
// restart (the only thing that cancels one), and an app-classified failure
// carries its own sentence; anything else is the generic one.
func (b *Bot) sentenceFor(ctx context.Context, err error) string {
	var ie *IngestError
	switch {
	case errors.As(err, &ie) && ie.Sentence != "":
		return ie.Sentence
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return sentenceRestart(b.upload)
	}
	return SentenceFailed
}

// fetchAndIngest GETs the attachment from an allowlisted CDN host and streams
// it straight into the app's ingest — no temporary file of its own: the ingest
// core spools to scratch exactly as an upload does, with the same size guard
// and the same ffprobe gate.
//
// The GET measures itself (§20 erratum 340): it asks `Range: bytes=0-` — the
// whole file, so a 206 streams exactly as a 200 does, and the status says
// whether Discord's CDN would serve parts — the body is counted, and ONE
// `discord fetch` line is logged when the fetch ends, success or not. A non-nil
// progress is the person's line: edited with the count every botProgressEvery
// while the body runs, and stopped before this returns.
func (b *Bot) fetchAndIngest(ctx context.Context, job ClipJob, progress progressEdit) (IngestResult, error) {
	if job.Ingest == nil { // unreachable: the endpoint always passes one
		return IngestResult{}, errors.New("no ingest configured")
	}
	if b.allow == nil {
		return IngestResult{}, &IngestError{Sentence: SentenceFetch, Err: errors.New("no attachment allowlist configured")}
	}
	u, err := b.allow(job.URL)
	if err != nil {
		return IngestResult{}, &IngestError{Sentence: SentenceFetch, Err: err}
	}
	if b.maxBytes > 0 && job.Size > b.maxBytes {
		return IngestResult{}, &IngestError{Sentence: SentenceTooLarge,
			Err: fmt.Errorf("attachment declares %d bytes", job.Size)}
	}
	fctx, cancel := context.WithTimeout(ctx, b.fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return IngestResult{}, &IngestError{Sentence: SentenceFetch, Err: errors.New("request could not be built")}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", "bytes=0-")
	fl := fetchLine{started: time.Now()}
	defer func() { b.logFetch(fl) }()
	resp, err := b.client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			// The process is shutting down (the only thing that cancels a
			// job's context): a restart, not Discord's fault (§20 erratum
			// 327). The callers word a bare cancellation as the restart.
			return IngestResult{}, fmt.Errorf("the attachment fetch was interrupted: %w", context.Canceled)
		}
		return IngestResult{}, &IngestError{Sentence: SentenceFetch, Err: errors.New(cause(err))}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()
	fl.ttfb, fl.status, fl.proto = time.Since(fl.started), resp.StatusCode, resp.Proto
	fl.acceptRanges = resp.Header.Get("Accept-Ranges")
	whole := resp.StatusCode == http.StatusOK ||
		// A 206 to `bytes=0-` is the whole file from its first byte; one that
		// starts anywhere else is not the file, and is refused like any status.
		resp.StatusCode == http.StatusPartialContent && strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes 0-")
	if !whole {
		// Includes the 3xx this client refuses to follow: an attachment that
		// redirects off the allowlisted host is not one we fetch.
		return IngestResult{}, &IngestError{Sentence: SentenceFetch,
			Err: fmt.Errorf("HTTP %d from the attachment host", resp.StatusCode)}
	}
	fl.meter = newFetchMeter(resp.Body)
	body := io.Reader(fl.meter)
	if b.maxBytes > 0 {
		// The second half of the size guard: a declared size is a claim, and
		// the ingest core's own MAX_UPLOAD_BYTES check is the enforcement on
		// an upload route. Here the limit is applied to the STREAM, so an
		// attachment that lies about its length stops at the same ceiling and
		// the ingest sees a short read rather than a full disk.
		body = io.LimitReader(fl.meter, b.maxBytes+1)
	}
	total := job.Size
	if total <= 0 {
		total = resp.ContentLength // -1 when unknown: then there is no "of" to say
	}
	if progress != nil && total > 0 {
		stop := b.watchFetch(ctx, fl.meter, total, progress)
		defer stop()
	}
	res, err := job.Ingest(ctx, IngestSource{
		Body: body, Filename: job.Filename, ContentType: job.ContentType, Size: job.Size,
	})
	fl.clipID = res.ClipID
	return res, err
}

// --- the API calls ----------------------------------------------------------

// editMessage rewrites the deferred reply with a whole message body: a
// one-sentence payload (a refusal, a restart) or the Message a successful
// upload ends in (§20 errata 315, 316). Whatever happens it is one attempt
// plus at most one retry, and a failure is a log line: there is no third
// place to put the outcome of a Discord message that will not send.
func (b *Bot) editMessage(ctx context.Context, token string, msg any) {
	if err := b.editOriginal(ctx, token, msg); err != nil {
		b.log.Warn("discord bot: the deferred reply could not be edited",
			"err", b.scrubJob(err.Error(), token))
	}
}

// editOriginal is PATCH /webhooks/{app}/{interaction token}/messages/@original
// — the documented way to replace a deferred reply.
//
// It sends `content` and an EMPTY allowed_mentions parse list, exactly as the webhook payload
// does and for the same reason: the content is a link this time, but the rule
// that a message this binary sends can never ping a server is not one to make
// conditional (§17 #4). Both shapes it is handed — payload and Message —
// carry that list by construction.
//
// No Authorization header: the interaction token in the path is the
// credential. The bot token is not sent here and must not be.
func (b *Bot) editOriginal(ctx context.Context, token string, msg any) error {
	body, err := json.Marshal(msg)
	if err != nil { // unreachable: both shapes are plain strings and ints
		return errors.New("payload could not be encoded")
	}
	path := botAPIPath + "/webhooks/" + b.appID + "/" + url.PathEscape(token) + "/messages/@original"
	err = b.call(ctx, http.MethodPatch, path, body, false)
	if err == nil {
		return nil
	}
	var re *retryableError
	if !errors.As(err, &re) {
		return err
	}
	// One retry. The deferred reply is created by Discord as it reads our
	// HTTP response, and this goroutine started before that response was
	// flushed, so a very fast job can genuinely arrive first.
	b.log.Info("discord bot: retrying the deferred edit", "after", botEditRetryWait.String(),
		"reason", b.scrubJob(err.Error(), token))
	select {
	case <-time.After(botEditRetryWait):
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.call(ctx, http.MethodPatch, path, body, false)
}

// followup is POST /webhooks/{app}/{interaction token} — "Create Followup
// Message", the way a message is posted after a MODAL answered the command
// (§20 erratum 320). The same body as the edit (the link alone, or one
// sentence; an empty allowed_mentions list), public, no flags.
//
// No Authorization header: the interaction token in the path is the
// credential, exactly as for the edit. One retry on the same retryable set as
// editOriginal — the 404 matters here too: a fast failure can arrive before
// Discord has processed the modal answer the HTTP response carried.
//
// It returns the new message's id when Discord's answer names one ("" when it
// does not — a follow-up is still posted; only what needs the id, the redraw
// and the delete of §20 erratum 325, is then skipped).
//
// This is the follow-up for every line that is NOT the link (the ephemeral
// lines of errata 325/326, a failure sentence); the link goes through
// followupLink, which holds its id per token.
func (b *Bot) followup(ctx context.Context, token string, msg any) (string, error) {
	return b.postFollowup(ctx, token, msg, "")
}

// followupLink is followup for a clip's LINK (§20 erratum 320 (g), the box's
// half): when this token already carries a posted link — its id held in
// linkIDs — the same body is PATCHed onto that message and nothing new is
// posted; otherwise it is posted, and the id Discord answers with is held for
// the token's life. One link per interaction, whatever re-enters.
func (b *Bot) followupLink(ctx context.Context, token string, msg any, clipID string) (string, error) {
	if id := b.linkIDs.get(token, time.Now()); id != "" {
		body, err := json.Marshal(msg)
		if err != nil { // unreachable: every shape is plain strings and ints
			return "", errors.New("payload could not be encoded")
		}
		b.log.Info("discord bot: the link is already posted on this interaction; editing it", "clip", clipID)
		return id, b.editHeld(ctx, token, b.followupPath(token)+"/messages/"+id, body)
	}
	id, err := b.postFollowup(ctx, token, msg, clipID)
	if err == nil {
		b.linkIDs.hold(token, id, time.Now())
	}
	return id, err
}

// postFollowup is the POST itself, with ONE retry on the retryable set — the
// 404 matters here too: a fast failure can arrive before Discord has processed
// the modal answer the HTTP response carried. The retry classes (§20 erratum
// 320 (g)):
//   - HTTP 429, 5xx or 404: Discord answered and the body did not land — the
//     retry posts it, as before;
//   - a TRANSPORT failure (no answer read: the response may have been lost
//     after Discord stored the message): still one retry, since a lost link is
//     worse than a rare duplicate, but under its own WARN line saying so.
//     Discord's webhooks take no idempotency key, so the earlier message
//     cannot be asked for by key.
//
// clipID is the link's clip ("" for any other line), for that line only.
func (b *Bot) postFollowup(ctx context.Context, token string, msg any, clipID string) (string, error) {
	body, err := json.Marshal(msg)
	if err != nil { // unreachable: every shape is plain strings and ints
		return "", errors.New("payload could not be encoded")
	}
	path := b.followupPath(token)
	answer, err := b.callBody(ctx, http.MethodPost, path, body, false)
	if err == nil {
		return messageID(answer), nil
	}
	var re *retryableError
	if !errors.As(err, &re) {
		return "", err
	}
	reason := b.scrubJob(err.Error(), token)
	switch {
	case re.lost && clipID != "":
		b.log.Warn("discord bot: follow-up response lost; retrying — the link may appear twice",
			"clip", clipID, "after", botEditRetryWait.String(), "reason", reason)
	case re.lost:
		b.log.Warn("discord bot: follow-up response lost; retrying — the line may appear twice",
			"after", botEditRetryWait.String(), "reason", reason)
	default:
		b.log.Info("discord bot: retrying the follow-up", "after", botEditRetryWait.String(), "reason", reason)
	}
	select {
	case <-time.After(botEditRetryWait):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	answer, err = b.callBody(ctx, http.MethodPost, path, body, false)
	if err != nil {
		return "", err
	}
	return messageID(answer), nil
}

// editHeld PATCHes a message this binary posted on the token, with one retry
// on the retryable set, as every edit here.
func (b *Bot) editHeld(ctx context.Context, token, path string, body []byte) error {
	err := b.call(ctx, http.MethodPatch, path, body, false)
	var re *retryableError
	if err == nil || !errors.As(err, &re) {
		return err
	}
	b.log.Info("discord bot: retrying the link edit", "after", botEditRetryWait.String(),
		"reason", b.scrubJob(err.Error(), token))
	select {
	case <-time.After(botEditRetryWait):
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.call(ctx, http.MethodPatch, path, body, false)
}

// followupPath is the interaction's own webhook: POST here posts a follow-up,
// and `/messages/{id}` under it names one this binary posted.
func (b *Bot) followupPath(token string) string {
	return botAPIPath + "/webhooks/" + b.appID + "/" + url.PathEscape(token)
}

// messageID reads the `id` of the Message object Discord answers a follow-up
// with. Only a snowflake is accepted — it goes into a path — so anything else
// reads as no id.
func messageID(answer []byte) string {
	var m struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(answer, &m) != nil || !snowflake(m.ID) {
		return ""
	}
	return m.ID
}

// snowflake says s is a Discord id: 1 to 20 decimal digits.
func snowflake(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Register (re)registers this instance's FIVE commands for one guild, in one
// PUT — `/upload`, `/clip`, the message command "Enrich" and `/settings`, or
// whatever DISCORD_UPLOAD_COMMAND_NAME, DISCORD_COMMAND_NAME,
// DISCORD_ENRICH_COMMAND_NAME and DISCORD_SETTINGS_COMMAND_NAME renamed them to
// (§20 errata 284, 315, 318), and `/signin` under its fixed name (erratum 341)
// — registered whatever REGISTRATION says, since the command itself answers
// "off" when the mode does not let anyone in. A self-hosted application never publishes
// `/pass`: there is no Pass without the official plane (run 4's panel contract
// §1).
//
// PUT replaces the application's whole command set IN THAT GUILD, which is
// what this should do: these are the only commands, and an operator running
// `admin bot register` after an upgrade expects the guild to end up with
// exactly what this build defines — which is also what retires an older
// build's one-command `/clip file:` there. GUILD commands (rather than global ones)
// appear the instant this call returns, where a global command takes up to an
// hour to propagate — that is the whole reason the CLI registers per guild.
//
// This is the ONE call that carries the bot token — and therefore the one call
// a MANAGED instance must never make: there is no token, and the command is
// published once, globally, by the official application's owner.
func (b *Bot) Register(ctx context.Context, guildID string) error {
	if b.managed {
		return ErrManaged
	}
	body, err := json.Marshal([]botCommandDef{b.uploadCommand(), b.clipCommand(), b.enrichCommand(), b.settingsCommand(), signinCommand()})
	if err != nil { // unreachable: the definitions are literals
		return errors.New("command definition could not be encoded")
	}
	path := botAPIPath + "/applications/" + b.appID + "/guilds/" + url.PathEscape(guildID) + "/commands"
	return b.call(ctx, http.MethodPut, path, body, true)
}

// ErrManaged is Register's answer on a managed instance (DISCORD_BOT_TOKEN is
// the sentinel, §20 erratum 279). The CLI prints it as the whole explanation.
var ErrManaged = errors.New("this instance answers the official Clippet bot (DISCORD_BOT_TOKEN=managed): /upload, /clip, Enrich and /settings are published once, globally, by that application, and nothing is registered from here")

// botCommandDef is one application command, in Discord's shape. `description`
// is omitted when empty because a MESSAGE command may not carry one ("not
// allowed when creating message commands"); both chat commands always do.
// There is no `contexts` and no `integration_types` here on purpose: Discord
// reads both "only for globally-scoped commands", and every command this file
// registers is a GUILD command — guild-only by construction. The official
// application's GLOBAL body (the forwarder's register step) carries them.
//
// DefaultMemberPermissions is set on `/settings` alone (§20 erratum 318): "32"
// is MANAGE_GUILD, so Discord shows the command only to members who hold it
// (or ADMINISTRATOR), and a server's admins may re-scope it under Server
// Settings → Integrations. Discord reads it as a STRING bitfield; the other
// commands omit it, which is "everyone", as they always were.
type botCommandDef struct {
	Name                     string             `json:"name"`
	Type                     int                `json:"type"`
	Description              string             `json:"description,omitempty"`
	Options                  []botCommandOption `json:"options,omitempty"`
	DefaultMemberPermissions string             `json:"default_member_permissions,omitempty"`
}

// botCommandOption is one command option. Type 11 is ATTACHMENT, type 3 STRING.
type botCommandOption struct {
	Name        string `json:"name"`
	Type        int    `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	// FileTypes narrows an ATTACHMENT option to Discord's own presets (§20
	// erratum 328): `/upload` takes `video` and `audio`. Discord matches it on
	// the file's extension only; ffprobe is still the gate.
	FileTypes []string `json:"file_types,omitempty"`
}

// uploadCommand is `/upload`, with ONE option — the video — and it is
// `required` (§20 erratum 286). Until §20 erratum 315 this was `/clip`; it is
// the same command under its own name, so that `/clip` could become the command
// that SHOWS a clip.
//
// Marking the one option REQUIRED means Discord will not send an empty
// `/upload` at all, so the mistake is caught in the client rather than answered
// a second later by an ephemeral line nobody enjoys. The endpoint keeps its own
// refusal for a body that arrives without an attachment anyway — defence in
// depth, and the answer for anything that is not Discord's own picker.
//
// The description is what a Discord user reads in the command picker, so it
// says what happens, in one line, with no CTA (§15 anti-features).
func (b *Bot) uploadCommand() botCommandDef {
	return botCommandDef{
		Name: b.upload, Type: commandTypeChatInput,
		Description: uploadDescription(b.urls.Host()),
		Options: []botCommandOption{{
			Name: botFileOption, Type: optionTypeAttachment,
			Description: "The video or audio to upload", Required: true,
			FileTypes: []string{"video", "audio"},
		}},
	}
}

// clipCommand is `/clip`, with ONE option — a share link of this instance —
// and it is `required` (§20 erratum 315). Erratum 286 withdrew a `link` option
// that sat BESIDE the file on the upload command, where it asked somebody to
// type a link back at the place it had come from; this one is a command of its
// own, for a different act: a person pastes a link to SHOW a clip, and the
// answer is the same message an upload ends in — the card, with the instance's
// name and the clip's state above it.
//
// The link is parsed by the endpoint and never fetched (internal/urls'
// ParseShare, a pure function): this command adds no egress at all.
func (b *Bot) clipCommand() botCommandDef {
	return botCommandDef{
		Name: b.command, Type: commandTypeChatInput,
		Description: clipDescription,
		Options: []botCommandOption{{
			Name: botLinkOption, Type: optionTypeString,
			Description: linkOptionDescription, Required: true,
		}},
	}
}

// enrichCommand is the MESSAGE command (type 3; §20 erratum 315's addendum):
// a message's Apps menu → "Enrich". It has no description (Discord refuses
// one) and no options — the message it is used on IS its input, delivered in
// the interaction's `data.resolved.messages`. No default_member_permissions:
// anybody may use it, as anybody may use `/clip link:`.
func (b *Bot) enrichCommand() botCommandDef {
	return botCommandDef{Name: b.enrich, Type: commandTypeMessage}
}

// settingsCommand is `/settings` (§20 erratum 318; run 4's panel contract §1):
// no options, guild-only by construction (a guild command), and visible only to
// members who may manage the server. The endpoint re-checks that permission on
// every interaction it answers, because a server's admins may widen it.
func (b *Bot) settingsCommand() botCommandDef {
	return botCommandDef{
		Name: b.settingsCmd, Type: commandTypeChatInput,
		Description:              settingsDescription,
		DefaultMemberPermissions: permManageGuild,
	}
}

// signinCommand is `/signin` (§20 erratum 341): no options, guild-only by
// construction, and visible to everybody — REGISTRATION decides who it lets
// in, re-checked on every interaction, and a server's admins may narrow who
// sees it under Server Settings → Integrations like any other command.
func signinCommand() botCommandDef {
	return botCommandDef{Name: botSigninCommand, Type: commandTypeChatInput, Description: signinDescription}
}

// signinDescription is what the command picker says under `/signin`.
const signinDescription = "Sign in to this Clippet's website with a one-time link."

// settingsDescription is the contract's description for `/settings`, verbatim
// (run 4's panel contract §1).
const settingsDescription = "This server's Clippet: the connection and the link defaults."

// permManageGuild is Discord's MANAGE_GUILD bit (1 << 5) as the decimal string
// `default_member_permissions` takes.
const permManageGuild = "32"

// The link command's two descriptions — the settled words (§20 erratum 315), and
// no host in them: "this Clippet" is the instance the command belongs to.
const (
	clipDescription       = "Post a clip of this Clippet as a card."
	linkOptionDescription = "A link to a clip on this Clippet"
)

// The upload command description's three pieces. Found on 2026-09-11, at the
// first live `/clip`: Discord refuses a too-big attachment CLIENT-SIDE ("Max file
// size is 500.00 MB"), before a byte reaches this app, so the bot cannot answer
// that moment — and the command picker is one of the only two places our words
// appear BEFORE it. So the description carries the way out: this instance's
// host, which is where a file Discord will not take is uploaded instead. The
// lead is the settled sentence of §20 erratum 315 ("a clip" since run 10: a clip can be
// sound, erratum 328, and the homepage says "Upload a clip."); the "bigger files" clause is
// erratum 268(a)'s, unchanged.
const (
	descLead    = "Upload a clip. You get a link that plays in Discord."
	descHost    = " Bigger files: "
	descNoHost  = " Bigger files go on the site."
	descMaxHost = maxDescription - len(descLead) - len(descHost) // 33
)

// uploadDescription names the host when it fits in Discord's hundred
// characters and says the same thing without it when it does not.
//
// The fallback is not hypothetical tidiness: PUBLIC_BASE_URL is whatever the
// operator's domain is, a long one is legal, and a description one character
// over the limit is a registration the API REJECTS — which would leave the
// server with no `/clip` at all rather than with a slightly vague one. The
// sentence is therefore built to fit, and the operator re-runs `admin bot
// register` after changing PUBLIC_BASE_URL (DEPLOY §30), since the description
// is baked into the command Discord stores rather than read per interaction.
func uploadDescription(host string) string {
	if host == "" || len(host) > descMaxHost {
		return descLead + descNoHost
	}
	return descLead + descHost + host
}

// The Discord enum values this file states. They are spelled here rather than
// imported from internal/httpapi (which parses the interaction carrying them)
// because §3 forbids the import; the option types are pinned by a test. (The
// STRING type went with erratum 286's `link` option and came back with erratum
// 315's `/clip link:`.)
const (
	commandTypeChatInput = 1  // CHAT_INPUT, a slash command
	commandTypeMessage   = 3  // MESSAGE, a message's Apps-menu entry
	optionTypeString     = 3  // STRING
	optionTypeAttachment = 11 // ATTACHMENT
)

// ClipCommandName and the accessor below are what this package REGISTERS, for
// the one test that pins them against what internal/httpapi READS. DESIGN §3
// forbids an import between those two packages, so the values are stated twice
// and checked once — the failure they guard against is a rename here that makes
// every `/clip` in every server answer "Clippet does not know that command".
//
// Since §20 erratum 284 the command's NAME is configurable, so this is the
// DEFAULT — and the pin is a three-way one: this value, internal/config's
// DefaultCommandName (which the loader hands to both sides) and what the
// endpoint answers when nothing is configured must all be the same word.
func ClipCommandName() string { return botCommand }

// UploadCommandName is the upload command's DEFAULT name, for the same pin —
// its twin since §20 erratum 315 (internal/config's DefaultUploadCommandName).
func UploadCommandName() string { return botUploadCommand }

// ClipOptionType is Discord's ATTACHMENT option type, as registered — the type
// of the UPLOAD command's only option (§20 errata 286, 315; the name is history).
func ClipOptionType() int { return optionTypeAttachment }

// LinkOptionType is Discord's STRING option type, as registered — the type of
// the LINK command's only option (§20 erratum 315).
func LinkOptionType() int { return optionTypeString }

// LinkOptionName is the LINK command's option name, as registered — the name
// the endpoint finds the link by (§20 erratum 315).
func LinkOptionName() string { return botLinkOption }

// EnrichCommandName is the message command's DEFAULT name, for the pin (§20
// erratum 315's addendum; internal/config's DefaultEnrichCommandName).
func EnrichCommandName() string { return botEnrichCommand }

// SettingsCommandName is the settings command's DEFAULT name, for the pin (§20
// erratum 318; internal/config's DefaultSettingsCommandName).
func SettingsCommandName() string { return botSettingsCommand }

// SigninCommandName is `/signin`'s FIXED name, for the pin (§20 erratum 341;
// internal/config's SigninCommandName).
func SigninCommandName() string { return botSigninCommand }

// MessageCommandType is Discord's MESSAGE command type, as registered — what
// the endpoint routes the message command by.
func MessageCommandType() int { return commandTypeMessage }

// retryableError marks a call whose failure is worth exactly one more attempt.
// lost says no answer was read at all (a transport failure): the request may
// still have landed at Discord, which matters for a POST (§20 erratum 320 (g)).
type retryableError struct {
	err  error
	lost bool
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// call makes one API request and classifies the answer. `auth` sends the bot
// token; only Register sets it.
//
// Nothing it returns names a URL or a token: the endpoint is built from an
// allowlisted host and two ids, the *url.Error wrapper (whose message embeds
// the URL, interaction token and all) is unwrapped to its cause, and every
// string is scrubbed of the bot token on the way out. The caller scrubs the
// interaction token, which is the one secret this function is not given a list
// for.
func (b *Bot) call(ctx context.Context, method, path string, body []byte, auth bool) error {
	_, err := b.callBody(ctx, method, path, body, auth)
	return err
}

// botAnswerMax bounds the success body callBody keeps: a Message object is a
// few kilobytes, and only its id is read.
const botAnswerMax = 64 << 10

// callBody is call, keeping the start of a 2xx answer's body (§20 erratum
// 325: the follow-up's message id).
func (b *Bot) callBody(ctx context.Context, method, path string, body []byte, auth bool) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, b.callTimeout)
	defer cancel()
	endpoint := (&url.URL{Scheme: b.scheme, Host: b.host, Path: path}).String()
	req, err := http.NewRequestWithContext(cctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("request could not be built")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bot "+b.token)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		// lost only when the CALLER did not end it: a cancelled or expired
		// context is not a response lost in transit (the request may never have
		// left), and must not be logged as one.
		return nil, &retryableError{err: errors.New(b.scrub(cause(err))), lost: ctx.Err() == nil}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()
	code := resp.StatusCode
	if code/100 == 2 {
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, botAnswerMax))
		return answer, nil
	}
	snippet := b.botBodySnippet(resp.Body)
	switch {
	case code == http.StatusTooManyRequests, code/100 == 5, code == http.StatusNotFound:
		return nil, &retryableError{err: fmt.Errorf("HTTP %d %s", code, snippet)}
	default:
		return nil, fmt.Errorf("HTTP %d %s", code, snippet)
	}
}

// botBodySnippet renders a rejection body for the log, in the order the
// notifier's does: read a window, scrub, and only then cut (a cut secret
// cannot be matched by any later scrub).
func (b *Bot) botBodySnippet(r io.Reader) string {
	s := truncateBytes(b.scrub(readWindow(r)), bodySnippet)
	if s == "" {
		return "(empty body)"
	}
	return "body=" + s
}

// tokenSpellings enumerates every form a bare token can take in a string on
// its way out: as configured, and percent-encoded the two ways net/url encodes
// a path segment and a query value. secretSpellings does this for a webhook
// URL, whose token is its last path segment; these two are not, so they get
// their own list rather than a URL's.
func tokenSpellings(tok string) []string {
	if tok == "" {
		return nil
	}
	out := []string{tok}
	for _, s := range []string{url.PathEscape(tok), url.QueryEscape(tok)} {
		if s != tok {
			out = append(out, s)
		}
	}
	sortLongestFirst(out)
	return out
}

// scrub removes every spelling of the BOT token from a string on its way out.
func (b *Bot) scrub(s string) string {
	for _, secret := range b.secrets {
		s = strings.ReplaceAll(s, secret, redacted)
	}
	return s
}

// scrubJob removes the bot token AND one interaction token. The interaction
// token is per-job, so it cannot live in b.secrets; it is scrubbed exactly the
// same way, because for fifteen minutes it is exactly as good as a credential.
func (b *Bot) scrubJob(s, token string) string {
	s = b.scrub(s)
	if token == "" {
		return s
	}
	for _, spelling := range tokenSpellings(token) {
		s = strings.ReplaceAll(s, spelling, redacted)
	}
	return s
}
