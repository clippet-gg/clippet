// Package config reads Clippet's configuration from environment variables
// exactly once at boot (DESIGN.md §4). Every value has the documented default;
// an invalid value fails the process with a one-line reason — never a guess.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3Config configures the S3-compatible object store (§4, §18.4).
type S3Config struct {
	Endpoint, Region, Bucket, AccessKey, SecretKey, KeyPrefix string
	ForcePathStyle                                            bool
	OpTimeout, IdleTimeout, TransferMax                       time.Duration

	// UploadThreads is STORAGE_UPLOAD_THREADS (§20 erratum 337): how many
	// 16 MiB parts one multipart PutFile uploads at the same time. 6 by
	// default (memory: about that many × 16 MiB per running upload); 1 is the
	// sequential path every upload took before the erratum. S3 only — `fs`
	// has no parts and ignores it.
	UploadThreads int

	// PresignEndpoint is MEDIA_PRESIGN_ENDPOINT: the bare origin a VIEWER
	// reaches this bucket at, which is not always the origin this process
	// reaches it at (§20 erratum 278). SigV4 signs the HOST, so a link minted
	// against `Endpoint` is only valid at `Endpoint` — a stack whose app talks
	// to the bucket over a private name (the shipped MinIO is reached by
	// SERVICE NAME over the compose network) has to sign with the public one,
	// or every redirect a viewer follows 403s. It is never empty after Load in
	// redirect mode: the loader fills it in from Endpoint, which is the common
	// case (R2, B2 and S3 are each one host for both), so the store never has
	// to decide. In `proxy` mode it is cleared, so "configured" can never be
	// mistaken for "in use".
	PresignEndpoint string
}

// The two accepted values of MEDIA_DELIVERY (ROADMAP #34 lane F, §20 erratum
// 278). `proxy` is the default and is byte-for-byte every deployment before
// it: the app streams every media byte itself. `redirect` answers the same
// routes, after the same gates, with a 302 to a freshly presigned URL on the
// bucket's own origin — so the bytes never cross whatever sits in front of
// the app.
const (
	DeliveryProxy    = "proxy"
	DeliveryRedirect = "redirect"
)

// The bounds on MEDIA_PRESIGN_TTL (§20 erratum 278). A `<video>` element keeps
// issuing ranged GETs against the SAME src for as long as the page is open, so
// a signature that expires mid-watch breaks seeking on a page nobody reloaded:
// the floor is a sanity bound and the default is an hour, which is longer than
// anybody watches one clip. The ceiling is the vendors' own maximum for a
// SigV4 query signature (7 days) cut down to a day, because a link that
// outlives the afternoon is a link somebody can paste.
const (
	DefaultMediaPresignTTL = time.Hour
	MinMediaPresignTTL     = time.Minute
	MaxMediaPresignTTL     = 24 * time.Hour
)

// MediaVars are the two variables that describe how media is DELIVERED rather
// than where it is stored. They are listed once, here, because two places need
// the same list: the loader's "set while `proxy` is in force" note and
// `admin doctor`'s `media` row.
var MediaVars = []string{"MEDIA_PRESIGN_TTL", "MEDIA_PRESIGN_ENDPOINT"}

// The two accepted values of STORAGE_BACKEND (ROADMAP #37, §20 erratum 276).
// `s3` is the default and is every deployment before it; `fs` is a plain
// directory, which is the smallest true self-host this product can offer and
// the shape a mounted NAS already wants.
const (
	BackendS3 = "s3"
	BackendFS = "fs"
)

// S3Vars are the four variables that describe a BUCKET, as opposed to the ones
// that describe the key space (`S3_KEY_PREFIX`) or the clock
// (`STORAGE_*_TIMEOUT`). They are required on `s3` and refused on `fs`, and
// they are listed once, here, because three places need the same list: the
// loader's rule, `admin doctor`'s report and the `admin storage migrate`
// preflight.
var S3Vars = []string{"S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY"}

// MigrateS3Vars are the four variables that describe the DESTINATION bucket of
// `admin storage migrate --to s3` on an instance already on `s3` (§20 erratum
// 335): bucket to bucket. They are required together, and they are read by
// LoadForMigrate ONLY — Load never looks at them, so `clippet serve` and every
// other verb neither use them nor mention them. The three optional ones beside
// them are MIGRATE_S3_REGION, MIGRATE_S3_FORCE_PATH_STYLE and
// MIGRATE_S3_KEY_PREFIX.
var MigrateS3Vars = []string{"MIGRATE_S3_ENDPOINT", "MIGRATE_S3_BUCKET", "MIGRATE_S3_ACCESS_KEY", "MIGRATE_S3_SECRET_KEY"}

// TranscodeConfig configures the worker and ffmpeg (§4, §10).
type TranscodeConfig struct {
	Concurrency               int
	Preset                    string
	CRF, MaxFPS, MaxW, MaxH   int
	StallTimeout, HardTimeout time.Duration
	MaxAttempts, MaxTransient int
	FFmpeg, FFprobe           string

	// MaxBitrate is TRANSCODE_MAX_BITRATE in BITS PER SECOND: the VBV
	// ceiling of the MAIN rendition (`-maxrate`, with `-bufsize` twice it).
	// 0 — the default — renders neither option, so CRF alone decides the
	// bitrate exactly as it always has (§20 erratum 124). It is the cheap
	// half of §17 #3: a ceiling on the file a player streams, where the
	// social rendition is a second, smaller file for the card.
	MaxBitrate int64

	// FullCodec is FULL_CODEC: which encoder the FULL tier uses (§20 erratum
	// 204). `h264` — the default — is the only codec every browser, every
	// Discord client and every embedded player decodes, so it is what an
	// operator who has not thought about it gets; `av1` roughly halves the
	// bytes of the one stored copy of a source the browser could not play, at
	// the cost of a slower encode and of viewers on hardware older than about
	// 2019. It bounds NOTHING else: the LIGHT copy a card fetches is H.264
	// whatever this says (erratum 121's measured fact), and a playable original
	// is never re-encoded at all.
	FullCodec string

	// Threads is TRANSCODE_THREADS (§20 erratum 215): how many threads one
	// encode may use. 0 — the default — is AUTO, and auto reads the
	// container's own CFS quota, because `nproc` inside a `cpus: 1.0`
	// container still reports every core the HOST has: x264 then spawns frame
	// threads for sixteen cores and is throttled to one, which measured 25 %
	// slower than pinning it. transcode.ResolveThreads turns this into the
	// `-threads` / `-filter_threads` pair every encode carries.
	Threads int

	// HWAccel is TRANSCODE_HWACCEL (§20 erratum 216): "" — the default —
	// renders the CPU argv this package has always rendered; `cuda` selects
	// the NVENC argv, which only the GPU image has an encoder for
	// (`Dockerfile` target `gpu`, compose profile `gpu`). It is a config value
	// and not a build flag because most of the filter graph is CPU-only: a
	// plan that needs a tone map, a deinterlace or a pre-rotate falls back to
	// the CPU argv on the very same binary.
	HWAccel string

	// FFmpegVersion is NOT read from the environment: serve.go fills it from
	// `ffmpeg -version` after resolving FFmpeg (the same way it overwrites
	// FFmpeg/FFprobe with their resolved paths) and /healthz reports it
	// (§8.1). Living here keeps httpapi.Deps exactly §18.2.
	FFmpegVersion string
}

// LightTagConfig is the burned-in corner line of the LIGHT copy (§20 erratum
// 214).
//
// Text is LIGHT_TAG_TEXT: a one-line template in which `{light}` is the card
// copy's own quality ("720p30") and `{best}` is the best tier's ("1440p60").
// `off` — the one word this variable refuses to treat as text — switches it
// off, and so does an empty value on an install that sets the variable to
// nothing at all.
//
// Font is NOT read from the environment. serve.go resolves it from the fonts
// the image ships, exactly as it resolves FFmpeg, and clears Text when it finds
// neither a font nor a `drawtext` filter: a missing font must cost a clip its
// tag and never its encode.
type LightTagConfig struct {
	Text string
	Font string
}

// LightTagOff is the LIGHT_TAG_TEXT value that means "no tag".
//
// It is a WORD rather than the empty string because config.Load reads through
// `getenv func(string) string`, which cannot tell a variable that is unset from
// one set to nothing — and compose's own `${VAR:-default}` interpolation
// collapses those two cases before the process ever sees them (the trap
// `S3_ACCESS_KEY` is documented for). An operator turns the tag off the way
// they turn a TTL off, with a word they can read in `.env` (§20 erratum 214).
const LightTagOff = "off"

// DefaultLightTagText is the shipped template. It names the preview quality
// first because that is what the reader is looking at, and the real quality
// second because that is what the link is for; it is deliberately one short
// line, since it is burned into the picture.
const DefaultLightTagText = "{light} preview · {best} on Clippet"

// Config is the fully validated process configuration.
type Config struct {
	Listen              string
	PublicBaseURL       *url.URL
	DataDir, ScratchDir string

	S3        S3Config
	Transcode TranscodeConfig

	// MigrateS3 is the destination bucket of a bucket-to-bucket `admin storage
	// migrate` (§20 erratum 335): the seven MIGRATE_S3_* variables, filled by
	// LoadForMigrate and by nothing else. It carries no timeout of its own —
	// the verb copies S3's — and it is the zero value on every other path.
	MigrateS3 S3Config

	// StorageBackend is STORAGE_BACKEND: `s3` (what every version before
	// ROADMAP #37 did) or `fs`, a plain directory under StorageDir (§20
	// erratum 276). Unset, it follows the four bucket variables (S3Vars): none
	// set means `fs`, any set means `s3` (erratum 345: the default stack is the
	// folder, and every `.env` written before it named its bucket without this
	// key — and a bucket missing one of its four lines is still a bucket, so the
	// error names that line). On `fs` the four bucket variables are
	// REFUSED rather than ignored — see the loader's s3Block — and S3Config
	// then carries only the key prefix and the timeouts, which are
	// about the key space and the clock rather than about a bucket.
	StorageBackend string

	// StorageDir is STORAGE_DIR: the absolute directory the media lives in on
	// `fs`. Required there, optional on `s3` (where it is what `admin storage
	// migrate --to fs` copies into), and validated whenever it is set: it may
	// not be `/`, and it may not be, contain or sit inside DATA_DIR or
	// SCRATCH_DIR.
	StorageDir string

	// DropFolderOwner is DROP_FOLDER_OWNER: the ONE account that owns
	// whatever is dropped at the ROOT of StorageDir (ROADMAP #42 (b), §20
	// erratum 296, amended by 296 (m)). Empty is the default and the feature
	// is off; a name turns it on, on EITHER backend.
	//
	// What it needs is a DIRECTORY, not a backend. On `fs` that directory is
	// the media store itself and is already required, so nothing about that
	// deployment changes. On `s3` StorageDir is optional — it is what `admin
	// storage migrate --to fs` copies into — and naming an owner is what makes
	// it required, because a folder to watch is the one thing the watcher
	// cannot do without.
	//
	// The name is validated here for SHAPE only. Whether such an account
	// exists is a question for the database, which this package never opens:
	// `clippet serve` resolves it at boot and refuses to start if it does not
	// (the same class of failure as a bad DISCORD_APP_ID, and for the same
	// reason — a feature that is on and silently ingests nothing for nobody is
	// the worst possible state for it to be in).
	DropFolderOwner string

	// Timed backups of the DATABASE (§20 erratum 321). BackupEvery is
	// BACKUP_EVERY, BackupDir is BACKUP_DIR (a clean absolute container path),
	// BackupKeep is BACKUP_KEEP. The feature is ON only when both of the first
	// two are set — see BackupOn — and it is set nowhere the project ships:
	// every compose file passes the three through empty.
	BackupEvery time.Duration
	BackupDir   string
	BackupKeep  int

	// The database snapshot that follows the clips into the store (§20
	// errata 343, 348). DBSnapshot is DB_SNAPSHOT=on|off, ON by default on
	// both backends: on `s3` the copy lands in the bucket, on `fs` under the
	// media folder's own `_clippet/` — so backing up that folder is the whole
	// backup. DBSnapshotEvery is DB_SNAPSHOT_EVERY, the debounce: at most one
	// write per interval, 10 minutes by default.
	DBSnapshot      bool
	DBSnapshotEvery time.Duration

	// MediaDelivery is MEDIA_DELIVERY: `proxy` (the default, and what every
	// version before ROADMAP #34 lane F did) or `redirect` (§20 erratum 278).
	// It changes nothing about WHICH URLs exist, what a card points at, or
	// which gate runs — only what the media routes answer once every gate has
	// passed: the bytes, or a 302 to a freshly presigned URL on the bucket's
	// own origin. `redirect` on StorageBackend `fs` is a boot error: a
	// directory has no URL to redirect to.
	MediaDelivery string

	// MediaPresignTTL is MEDIA_PRESIGN_TTL: how long one of those signatures
	// is valid. It is read once per request — the link is minted AFTER the
	// gate, never stored and never written into a card tag — so it bounds the
	// window a copied media URL keeps working, not the lifetime of a clip.
	MediaPresignTTL time.Duration

	// ResolvedMediaOrigin is the `scheme://host[:port]` a presigned media URL
	// actually lands on, and is the ONE thing MediaOrigin() answers with
	// (§20 erratum 278, amended). It is NOT read from the environment: the
	// loader cannot know it, because it depends on minio-go's bucket-lookup
	// rule — virtual-host lookup puts the BUCKET in the host and path-style
	// does not. So it is filled in after the store opens, by `serve` and by
	// `admin doctor`, from store.MediaOrigin, which signs one throwaway key and
	// reads the origin off the result (local HMAC, no network call).
	//
	// Empty in `proxy` mode and empty until that call is made. A handler built
	// with it empty in `redirect` mode would emit the old, narrower CSP, so
	// serve.go treats a failure to resolve it as fatal at boot rather than
	// shipping a policy that blocks the player.
	ResolvedMediaOrigin string

	// DefaultTTLSeconds is DEFAULT_CLIP_TTL resolved to seconds; nil = never.
	DefaultTTLSeconds *int64

	SweepInterval, SweepGrace time.Duration
	SweepBatch                int

	// StoragePingAttempts / StoragePingInterval are STORAGE_PING_ATTEMPTS and
	// STORAGE_PING_INTERVAL: the shape of the BOOT ping loop in cmd/clippet's
	// runServe (§8.7). The listener is already up when that loop starts — the
	// process serves degraded rather than crash-looping on a bucket that is
	// slow to appear — so what these two decide is only how long it keeps
	// asking before it gives up and logs "storage unreachable": at the
	// defaults, 10 attempts of up to 3s each with 3s between them, so about a
	// minute. They are a property of THIS process and not of the store, which
	// is why they live here and not in S3Config: `fs` has a Ping too.
	//
	// An operator whose bucket is slow to appear raises the attempts; one
	// whose bucket is definitely up lowers them, so a genuinely dead endpoint
	// is in the log in seconds instead of a minute. The defaults are what
	// every release before this knob did, and TestDefaults pins both
	// (§20 erratum 283).
	StoragePingAttempts int
	StoragePingInterval time.Duration

	MaxUploadBytes, ScratchMinFree int64

	// UploadChunkBytes is UPLOAD_CHUNK_BYTES: the size of one chunk of the
	// chunked web upload (§20 erratum 344), the server's word on how a file
	// larger than one chunk is cut. The default is 50 MiB, under the 100 MB a
	// Free or Pro Cloudflare zone accepts in one request body; the knob exists
	// for a zone whose cap is different, and is "the edge's cap minus headroom".
	UploadChunkBytes int64

	// SocialMaxBytes is SOCIAL_MAX_BYTES: the rendition size at or above
	// which a clip gets its own dedicated social rendition (§17 #3, §20
	// erratum 121). A rendition under it that is also ≤ 30 fps is left alone
	// — `/social.mp4` serves the main rendition, exactly as it always has —
	// because a small, calm file is already the file a card wants. 0 turns
	// the feature OFF entirely: no social job is ever enqueued, whatever the
	// frame rate.
	SocialMaxBytes int64

	// CaptureReadyWait is CAPTURE_READY_WAIT (§20 errata 97, 212): how long
	// POST /api/upload may hold its 201 waiting for the clip to become
	// CARD-READY (or `failed`), so the link a capture tool puts on the
	// clipboard already unfurls as a player card. 0 = never wait; the
	// hard ceiling is CaptureReadyWaitMax. Since erratum 212 it is the ONLY
	// knob on that wait — the per-request `wait` field is gone — and its
	// default is 30s rather than 8s, because what the reply now waits for is
	// the file Discord fetches and not merely a playable clip.
	CaptureReadyWait time.Duration

	// LightTag is LIGHT_TAG_TEXT and the font resolved for it (§20 erratum
	// 214): the one small corner line burned into the LIGHT copy — the file a
	// card fetches — naming the preview quality and the real one. It is the
	// single amendment to the no-watermark rule (the owner's call, 2026-09-08) and it is
	// never on the Original or the Full.
	LightTag LightTagConfig

	SessionTTL    time.Duration
	SessionCookie string

	// SessionMaxTTL is SESSION_MAX_TTL (§20 erratum 105): the ceiling on the
	// sliding window, measured from the session's CREATION. A session may
	// slide, but never past `created_at + SessionMaxTTL`, so an actively used
	// stolen cookie still dies on a schedule its holder cannot extend.
	SessionMaxTTL time.Duration

	// Registration is REGISTRATION (§20 erratum 106): open | first | closed,
	// and since §20 erratum 341 members | role:<id> — the bot's `/signin` as the
	// door, the web form closed. `first` is the shipped default — open while the
	// users table is empty, closed the moment an account exists.
	Registration RegistrationMode

	// LibraryScope is LIBRARY_SCOPE (§20 erratum 113): who the library and
	// GET /api/clips list. LibraryScopeShared (the default, and what every
	// deployment before this variable existed did) lists every account's
	// listed clips plus the caller's own unlisted ones; LibraryScopeOwn lists
	// the caller's own clips only. It narrows a result set and never widens
	// one, and it changes NO other route: a share link is still the auth.
	LibraryScope string

	// InstanceBadge is INSTANCE_BADGE (§20 erratum 285): one lowercase word
	// this instance wears so a person can never mistake it for the production
	// one — `dev`, `staging`, `lab-test`. Empty (the default, and what every
	// deployment before it did) changes NOTHING anywhere: no attribute, no tag,
	// no title prefix, no rename of anything.
	//
	// Non-empty and the mark goes MONOCHROME — the wordmark's green span, and a
	// face's mouth with it, paint `--line-strong` instead — the word sits beside
	// the wordmark, `<title>` gains it as a prefix, and the two strings Discord
	// renders as the logo (`og:site_name`, the oEmbed `provider_name`) become
	// `clippet <badge>`. Green span = production, grey span = dev, which is the
	// rule the two Discord application icons already set (2026-09-15).
	//
	// It is deliberately NOT on `/healthz`: what it exists for is a person's eye,
	// and the anonymous surface stays exactly the size it was.
	InstanceBadge string

	// CardComponents is CARD_COMPONENTS (§20 erratum 316): `on` makes the
	// share page declare its own Discord card — a "component embed", the
	// `<script id="discord:component-embed">` data block Discord's link
	// previews read — beside the §9.1 card tags, which stay exactly as they are
	// and are what Discord falls back to. `on` is the default since the owner's
	// real-channel test (2026-10-05); `off` renders every page byte for byte as
	// before the variable existed — the switch for a Discord that changes the
	// format, or a Pass guard that does not know the element yet.
	CardComponents bool

	// SetupWizardOff is SETUP_WIZARD=off (§20 erratum 335 (d)); `on`, the
	// default and the zero value, serves `/setup` exactly as before the
	// variable existed. Off answers every view of it but the steps-only one
	// with the ordinary 404, the first-run door included. The desktop app sets
	// it on every instance it runs, because the app does the setup there
	// (the owner's call, 2026-11-02); a server older than the variable ignores it, and an
	// app older than it leaves the default. Held inverted so a Config built in
	// code keeps the wizard.
	SetupWizardOff bool

	// UploadForm is DISCORD_UPLOAD_FORM (§20 erratum 320): `on` (the default)
	// makes `/upload` answer with a form — Title, Delete after, Tags — while the
	// upload already runs, and the clip's link is posted later as a follow-up;
	// `off` answers "thinking…" exactly as before the variable existed. It is
	// inert without a bot (nothing reads it), so it is no orphan, and it is
	// allowed on a managed instance: the official bot's forwarder relays the
	// box's answer unchanged.
	UploadForm bool

	// EnrichUpload is DISCORD_ENRICH_UPLOAD (§20 erratum 323): `off` (the
	// default) keeps Enrich answering links only, exactly as before the variable
	// existed; `on` lets Enrich on a message with a video and no link of ours
	// upload that video through `/upload`'s path, for the message's poster or a
	// member with Manage Server. Off by default because Enrich has its own row
	// under Discord's Server Settings → Integrations: a server that limited
	// `/upload` to a role has not limited Enrich. Parsed and placed exactly as
	// UploadForm: inert without a bot, allowed on a managed instance.
	EnrichUpload bool

	TrustedProxies []netip.Prefix

	ViewDedupeTTL time.Duration
	ViewDedupeMax int

	RateAuthPerMin, RateUploadPerHour, RateMaxKeys int

	// RateUploadPerHourPerIP is RATE_UPLOAD_PER_HOUR_PER_IP (§20 erratum
	// 97): the second bucket the upload routes require, keyed on the
	// client IP. RATE_UPLOAD_PER_HOUR alone bounds an ACCOUNT, and
	// accounts are free.
	RateUploadPerHourPerIP int

	SSRTimeout          time.Duration
	LogLevel, LogFormat string

	// DiscordWebhook is DISCORD_WEBHOOK_URL, validated and split (§17 #4).
	// The zero value means "unset": no notify job is ever enqueued and nothing
	// is ever posted to Discord.
	DiscordWebhook DiscordWebhook

	// DiscordRoutes is DISCORD_WEBHOOK_ROUTES (ROADMAP #26, §20 erratum 158):
	// an ORDERED list of tag→webhook rules. A ready clip is posted to the
	// first rule whose tag it carries; the order is the operator's, spelled
	// in the variable, so priority is something they can read off the line
	// they wrote. Empty = no routing, and DiscordWebhook alone decides. The
	// tag `default` is reserved: it already names DiscordWebhook's own
	// channel everywhere a route is named (§20 erratum 160).
	DiscordRoutes []DiscordRoute

	// DiscordUnmatched is DISCORD_WEBHOOK_UNMATCHED: what happens to a clip
	// no route matched. `default` (the default, and what every deployment
	// before routing existed did) posts it to DISCORD_WEBHOOK_URL; `skip`
	// posts it nowhere, which is how an operator says "only tagged clips are
	// announced" — and is refused at boot when DISCORD_WEBHOOK_ROUTES is
	// empty, because with no rules to match it means "announce nothing at
	// all" while DISCORD_WEBHOOK_URL still looks live (§20 erratum 160).
	DiscordUnmatched string

	// DiscordBot is DISCORD_APP_ID + DISCORD_PUBLIC_KEY + DISCORD_BOT_TOKEN
	// (ROADMAP #33, §20 errata 253-255): the Discord application whose slash
	// commands this instance answers. The zero value means "unset" — the
	// interactions route is not registered at all, no client is built for it
	// and no command is ever registered. All three or none (discordbot.go).
	// DISCORD_COMMAND_NAME rides along as a fourth, OPTIONAL variable: it names
	// the command (default DefaultCommandName) and is a boot error with the bot
	// off, or on a managed instance under any other name (§20 erratum 284).
	// DISCORD_UPLOAD_COMMAND_NAME is its twin for the upload command (default
	// DefaultUploadCommandName), under the same rules, and the two must differ
	// (§20 erratum 315: `/upload file:` and `/clip link:`).
	DiscordBot DiscordBot

	// DiscordAuto is Clippet Auto on the box (§20 erratum 333, DEPLOY §30.10):
	// DISCORD_AUTO_MODE off|prompt|auto, its token, the Prompt seconds and the
	// notice switch (discordauto.go). The zero value is off: no socket.
	DiscordAuto DiscordAuto

	// DiscordLogin is DISCORD_LOGIN_CLIENT_ID + DISCORD_LOGIN_CLIENT_SECRET
	// (ROADMAP #44, §20 erratum 302): the instance's OWN Discord application,
	// used only for "Sign in with Discord". The zero value means "unset" — no
	// route under /api/auth/discord, no button on /login, no client. Both or
	// none (discordlogin.go); independent of the bot's three.
	DiscordLogin DiscordLogin

	// ClipWebhook is CLIP_WEBHOOK_URL + CLIP_WEBHOOK_SECRET (ROADMAP #54, §20
	// erratum 319): one operator-named URL every clip that becomes ready is
	// POSTed to, signed. The zero value means "unset" — no `hook` job is ever
	// enqueued and no request is ever built for it. Both or none
	// (clipwebhook.go).
	ClipWebhook ClipWebhook

	// StorageBudgetBytes is STORAGE_BUDGET_BYTES (ROADMAP #24, §20 errata 157
	// and 277): an ADVISORY line the operator wants to know about crossing.
	// 0 (the default) is off. It is never a cap — no upload is refused
	// because of it, ever, UNLESS StorageBudgetHard below is set, which is the
	// one opt-in that makes it one. README's "no tiers, no per-user quotas"
	// stays literally true either way: this is one line for the whole
	// instance, chosen by whoever runs it, and there is still nothing per
	// account.
	StorageBudgetBytes int64

	// StoragePriceMicros and EgressPriceMicros are STORAGE_PRICE_PER_GB_MONTH
	// and EGRESS_PRICE_PER_GB in MILLIONTHS of the operator's own currency unit
	// (ROADMAP #39, §20 erratum 270). nil is UNSET, and unset means the cost
	// line is ABSENT everywhere rather than zero: an operator who has typed no
	// price must never be shown a 0.00 they could read as "this is free".
	//
	// The unit is the operator's, not this app's: the app never names a
	// currency, never asks a provider what anything costs and never makes a
	// call of any kind to find out (§15). It multiplies two numbers the
	// operator typed by two numbers it already keeps.
	//
	// Micros rather than a float because a price IS a decimal the operator
	// wrote (`0.015`) and six fractional digits hold every per-GB price any
	// provider quotes; the parse is exact, so the configured value round-trips.
	StoragePriceMicros, EgressPriceMicros *int64

	// StoragePricing is STORAGE_PRICING (§20 erratum 277): the name of a
	// provider PRESET — a dated row of list prices held in `internal/cost`,
	// never fetched from anywhere — or "" for the custom case, which is the
	// shipped default and means the two prices above and nothing else.
	//
	// The names live here rather than in internal/cost because the loader has
	// to validate one and cost imports this package; PricingPresets is the set
	// the boot error names, and a cost test asserts the table holds exactly
	// these rows, so neither half can grow a name the other does not have.
	//
	// A typed price OVERRIDES its half of the preset (cost.PricesOf); the
	// preset's free-allowance rule stays whatever the price is, because an
	// allowance is a billing rule and not a number the operator retyped.
	StoragePricing string

	// StorageBudgetHard is STORAGE_BUDGET_HARD (§20 erratum 277): whether
	// StorageBudgetBytes is a CAP as well as a line. false (the default, and
	// what every version before it did) leaves the budget purely advisory.
	//
	// true refuses a NEW clip with 507 once the stored bytes reach the budget.
	// It is opt-in for the reason the field above states: an operator who set a
	// budget to be warned must not discover that it silently stopped their
	// friends uploading. It is a boot error without a budget to cap — a cap of
	// zero would refuse everything, which nobody means by "1".
	StorageBudgetHard bool

	// JobsUnstickAfter is JOBS_UNSTICK_AFTER as the operator wrote it (§20
	// erratum 230): how long a job may be `running` before the app repairs
	// it by itself, exactly as `clippet admin jobs unstick --older-than`
	// does by hand. 0 turns the automatic repair OFF; an unset variable is
	// TRANSCODE_HARD_TIMEOUT + UnstickMargin, which is why it is not a
	// plain default anybody can read off this line.
	//
	// It is the RAW value: the effective schedule — the floor, the tick, and
	// whether the value is one a live encode of ANOTHER process could still be
	// inside — is UnstickPlan(), which the worker logs at boot and
	// `admin doctor` prints, so one function answers "what will actually
	// happen".
	JobsUnstickAfter time.Duration
}

// UnstickMargin is how far past TRANSCODE_HARD_TIMEOUT the DEFAULT automatic
// repair age sits (§20 erratum 230). The hard timeout is the longest a live
// ENCODE may last, and the margin is there because a job is more than its
// encode (a transcode also acquires, probes, verifies, posters and stores), so
// the shipped default repairs nothing that could still plausibly be running
// anywhere — including in a second process this one cannot see into.
const UnstickMargin = 15 * time.Minute

// MinUnstickAfter is the absolute floor on JOBS_UNSTICK_AFTER when it is set at
// all (§20 erratum 230). It is deliberately NOT TRANSCODE_HARD_TIMEOUT, and the
// reason is the whole difference between this repair and the CLI's:
//
//   - `admin jobs unstick` is a SECOND PROCESS with no way to see the run
//     registry, so age is its only evidence and it floors `--older-than` at the
//     hard timeout (§20 erratum 146).
//   - the automatic repair runs INSIDE the worker and knows, exactly, every job
//     this process has claimed and not yet finished. For its own jobs age is not
//     evidence at all — it never repairs one, at any age — so the age only has
//     to cover the process that is NOT this one: the one that died (whose jobs
//     nobody is running by definition) and a second instance somebody pointed at
//     the same database (which DESIGN §1 and DEPLOY §18 do not support).
//
// So the floor here is a sanity bound rather than a safety argument: half a
// minute is under any real deployment's setting and above the "0 means off"
// confusion of a sub-second value. A value below TRANSCODE_HARD_TIMEOUT is
// obeyed and WARNED about — at boot and in `admin doctor` — because it is the
// operator's call to make and the second-process case is the one it costs.
const MinUnstickAfter = 30 * time.Second

// The bounds on how often the automatic repair looks (§20 erratum 230). The
// pass is one index-free scan of the `running` rows — a handful of rows on any
// instance this design targets — so the cadence is chosen for how quickly a
// repair is NOTICED, not for what it costs: a quarter of the age, never more
// than five minutes (an operator watching a stuck queue should not have to wait
// a sixth of an hour) and never less than five seconds (a ticker of zero
// panics, and a test instance may set the age to seconds).
const (
	minUnstickEvery = 5 * time.Second
	maxUnstickEvery = 5 * time.Minute
)

// UnstickPlan is the EFFECTIVE automatic-repair schedule of a configuration:
// what the operator asked for, what will actually happen, and what is worth
// telling them about the difference. One function so the boot log line,
// `admin doctor` and the loop itself cannot disagree about any of it.
type UnstickPlan struct {
	// Configured is JOBS_UNSTICK_AFTER as written (0 = off).
	Configured time.Duration
	// After is the age a running job must reach before this process repairs it,
	// never below Floor. 0 means the feature is off and no loop runs at all.
	After time.Duration
	// Floor is MinUnstickAfter, the sanity bound a positive value is raised to.
	Floor time.Duration
	// HardTimeout is TRANSCODE_HARD_TIMEOUT, carried so the one function that
	// resolves the schedule is also the one that knows whether the operator has
	// chosen an age a live encode of ANOTHER process could still be inside.
	HardTimeout time.Duration
	// Every is the tick.
	Every time.Duration
}

// On reports whether the automatic repair runs at all.
func (p UnstickPlan) On() bool { return p.After > 0 }

// Raised reports whether the sanity floor moved the operator's value.
func (p UnstickPlan) Raised() bool { return p.On() && p.Configured < p.Floor }

// UnderHardTimeout reports whether the effective age is one a live encode could
// still be inside. It is never true of THIS process's jobs — they are excluded
// by claim, not by age — so what it warns about is a SECOND process sharing the
// database, whose live job this one cannot see and would repair.
func (p UnstickPlan) UnderHardTimeout() bool { return p.On() && p.After < p.HardTimeout }

// UnstickPlan resolves JOBS_UNSTICK_AFTER.
func (c Config) UnstickPlan() UnstickPlan {
	p := UnstickPlan{Configured: c.JobsUnstickAfter, Floor: MinUnstickAfter, HardTimeout: c.Transcode.HardTimeout}
	if p.Configured <= 0 {
		return p // off; After and Every stay 0 and no loop is started
	}
	p.After = max(p.Configured, p.Floor)
	p.Every = min(max(p.After/4, minUnstickEvery), maxUnstickEvery)
	return p
}

// DiscordRoute is one DISCORD_WEBHOOK_ROUTES rule: clips carrying Tag are
// posted to Webhook. Tag is NOT secret and is what a log line, a job's
// params_json and the DEPLOY docs name a route by; Webhook is the secret and
// is reachable only through its URL() accessor (§20 erratum 158).
type DiscordRoute struct {
	Tag     string
	Webhook DiscordWebhook
}

// The two values of DISCORD_WEBHOOK_UNMATCHED.
const (
	DiscordUnmatchedDefault = "default"
	DiscordUnmatchedSkip    = "skip"
)

// DiscordEnabled reports whether ANY webhook is configured — the default, a
// route, or both. It is the bit the ready transaction takes: with it false no
// notify job is ever enqueued and internal/notify builds no client for Discord
// (§15).
func (c Config) DiscordEnabled() bool {
	return c.DiscordWebhook.Configured() || len(c.DiscordRoutes) > 0
}

// DiscordWebhook is a validated Discord webhook URL.
//
// **The value is a SECRET**: a webhook's token is the last segment of its
// path, and anyone holding it can post to the channel forever. So the raw URL
// is reachable only through URL(), and the String/LogValue pair deliberately
// render the harmless half (host + numeric id) — a Config printed with %v, or
// handed to slog, can never leak the token.
type DiscordWebhook struct {
	raw  string // "" = not configured
	host string
	id   string
}

// Configured reports whether DISCORD_WEBHOOK_URL was set.
func (w DiscordWebhook) Configured() bool { return w.raw != "" }

// URL returns the raw webhook URL — the secret. Only internal/notify may put
// it on the wire; nothing may put it in a log line, an error or a DB row.
func (w DiscordWebhook) URL() string { return w.raw }

// Host is the validated Discord host (not secret).
func (w DiscordWebhook) Host() string { return w.host }

// ID is the numeric webhook id from the path (not secret; the token is).
func (w DiscordWebhook) ID() string { return w.id }

// String is the redacted rendering: never the token.
func (w DiscordWebhook) String() string {
	if w.raw == "" {
		return "(unset)"
	}
	return w.host + "/" + w.id + "/(token redacted)"
}

// LogValue makes slog print the redacted form even if the whole value is
// logged by accident.
func (w DiscordWebhook) LogValue() slog.Value { return slog.StringValue(w.String()) }

// discordHosts is the closed set of hosts DISCORD_WEBHOOK_URL may name. They
// are stored as bare host names, never as URL literals: §15 / §20 erratum 27
// forbid an `http(s)://` literal anywhere in the binary, and the whole point
// of the allowlist is that the no-call-home guarantee stays greppable.
var discordHosts = map[string]bool{
	"discord.com":        true,
	"discordapp.com":     true,
	"ptb.discord.com":    true,
	"canary.discord.com": true,
}

// discordPathPrefix is the only path shape a webhook URL may have:
// /api/webhooks/{numeric id}/{token}. The versioned form (/api/v10/webhooks/…)
// is deliberately rejected — the unversioned route is what Discord's own
// "Copy Webhook URL" button hands you, and pinning the shape keeps the
// allowlist meaningful.
const discordPathPrefix = "/api/webhooks/"

// The two values of LIBRARY_SCOPE (§20 erratum 113). `shared` is the default
// because it is what every deployment did before the variable existed, and a
// configuration change must never be the thing that silently hides clips an
// operator's household could see yesterday.
const (
	LibraryScopeShared = "shared"
	LibraryScopeOwn    = "own"
)

// ErrInvalidTTL is returned by ParseTTLDays for any value outside
// {0, 1, 7, 30, 90}; httpapi maps it to 400 invalid_ttl.
var ErrInvalidTTL = errors.New("ttl_days must be 0, 1, 7, 30 or 90")

// allowedTTLDays is the closed set of accepted TTL values (§8.2).
var allowedTTLDays = map[int]bool{0: true, 1: true, 7: true, 30: true, 90: true}

// The two accepted values of FULL_CODEC (§20 erratum 204). They are spelled
// here rather than imported from internal/transcode because config imports
// nothing of ours by design (§4), and transcode.NormalizeFullCodec is what
// turns a stored value back into an encoder.
const (
	FullCodecH264 = "h264"
	FullCodecAV1  = "av1"
)

// The two accepted values of TRANSCODE_HWACCEL (§20 erratum 216), spelled here
// for the same reason FULL_CODEC's are: config imports nothing of ours.
// HWAccelNone is the empty string, which is what "render the CPU argv this
// package has always rendered" has to be — the variable's absence and its
// off-switch are one value.
const (
	HWAccelNone = ""
	HWAccelCUDA = "cuda"
)

// MaxTranscodeThreads bounds TRANSCODE_THREADS. It is far above any machine
// this runs on and exists so a typo is a boot error rather than a thousand
// x264 threads: past the core count the setting only costs memory, and x264's
// own frame-thread ceiling is 128.
const MaxTranscodeThreads = 128

// x264 presets accepted for TRANSCODE_PRESET.
var x264Presets = map[string]bool{
	"ultrafast": true, "superfast": true, "veryfast": true, "faster": true, "fast": true,
	"medium": true, "slow": true, "slower": true, "veryslow": true, "placebo": true,
}

// ParseTTLDays applies the §8.2 wire rule shared by POST and PATCH:
// v == nil → def (a copy); *v == 0 → never (nil); 1|7|30|90 → n·86400;
// anything else → ErrInvalidTTL. Callers implement "absent means leave
// unchanged" for PATCH by passing the clip's current value as def.
func ParseTTLDays(v *int, def *int64) (*int64, error) {
	if v == nil {
		if def == nil {
			return nil, nil
		}
		d := *def
		return &d, nil
	}
	if !allowedTTLDays[*v] {
		return nil, ErrInvalidTTL
	}
	if *v == 0 {
		return nil, nil
	}
	secs := int64(*v) * 86400
	return &secs, nil
}

// Load reads and validates every variable of §4 through getenv (normally
// os.Getenv). EVERY invalid value is reported, as an *Errors holding one error
// per variable, each naming its variable and its rule (§20 erratum 104); the
// process must treat any error as fatal.
//
// All of them, not just the first: the loader already kept running every
// lookup after a failure, and compose restarts the container on a loop, so an
// operator with three typos in .env used to learn about them one restart at a
// time. Nothing else about the messages changed.
func Load(getenv func(string) string) (Config, error) {
	return load(getenv, false)
}

// LoadForMigrate is Load with ONE rule relaxed, for the one verb that needs
// it: `clippet admin storage migrate` copies between the two backends, so it
// is the only process that may have STORAGE_BACKEND=fs and the four bucket
// variables set at once (§20 erratum 276).
//
// It is a separate entry point rather than a variable an operator can set,
// because that makes the relaxation UNFORGEABLE: `clippet serve` cannot reach
// it, so an instance that is serving on `fs` provably has no bucket
// configured, and §15's sentence about outbound destinations stays true
// without an operator having to check anything. cmd/clippet/main.go routes
// exactly `admin storage migrate` here and everything else through Load.
func LoadForMigrate(getenv func(string) string) (Config, error) {
	return load(getenv, true)
}

func load(getenv func(string) string, migrating bool) (Config, error) {
	l := &loader{getenv: getenv, migrating: migrating}
	var c Config

	c.Listen = l.str("CLIPPET_LISTEN", ":8080")
	c.PublicBaseURL = l.baseURL("PUBLIC_BASE_URL")
	c.DataDir = l.str("DATA_DIR", "/data")
	c.ScratchDir = l.str("SCRATCH_DIR", "/scratch")

	// Read BEFORE the bucket block, which it governs (§20 erratum 276). Unset,
	// the backend is whatever the four bucket variables say (erratum 345): none
	// set is the folder store, any set is a bucket — so a `.env` from before the
	// folder became the default keeps its bucket with no new line, and one that
	// lost a bucket line is told which line, not told to drop the other three.
	backendDefault := BackendFS
	for _, v := range S3Vars {
		if strings.TrimSpace(l.raw(v)) != "" {
			backendDefault = BackendS3
			break
		}
	}
	c.StorageBackend = l.oneOf("STORAGE_BACKEND", backendDefault, BackendS3, BackendFS)
	c.StorageDir = l.storageDir("STORAGE_DIR", c)
	l.s3Block(&c)
	c.S3.Region = l.str("S3_REGION", "us-east-1")
	c.S3.ForcePathStyle = l.boolean("S3_FORCE_PATH_STYLE", false)
	c.S3.KeyPrefix = l.keyPrefix("S3_KEY_PREFIX")
	c.S3.OpTimeout = l.duration("STORAGE_OP_TIMEOUT", 30*time.Second)
	c.S3.IdleTimeout = l.duration("STORAGE_IDLE_TIMEOUT", 2*time.Minute)
	c.S3.TransferMax = l.duration("STORAGE_TRANSFER_MAX", 6*time.Hour)
	// intRange, so 0 or 100 is a boot error naming the variable rather than a
	// clamp (§4). The ceiling bounds the memory one upload may take (32 ×
	// 16 MiB = 512 MiB, a quarter of the app's default mem_limit).
	c.S3.UploadThreads = l.intRange("STORAGE_UPLOAD_THREADS", 6, 1, 32)
	if migrating {
		l.migrateS3Block(&c)
	}
	// The boot ping loop's shape (§20 erratum 283). Not part of S3Config: the
	// loop is serve.go's and runs against whichever backend is configured.
	// intMin, so an attempt count under one is a boot error naming the variable
	// rather than a guess (§4): a process that pinged zero times would report
	// `storage unreachable` before it had asked anything.
	c.StoragePingAttempts = l.intMin("STORAGE_PING_ATTEMPTS", 10, 1)
	c.StoragePingInterval = l.duration("STORAGE_PING_INTERVAL", 3*time.Second)

	// How media is DELIVERED, read AFTER the block that says where it lives,
	// because two of its three rules depend on that (§20 erratum 278).
	l.mediaBlock(&c)

	// The drop folder (ROADMAP #42 (b), §20 erratum 296), read after the
	// backend for the reason the bucket block is: its one rule is about which
	// backend is in force.
	c.DropFolderOwner = l.dropFolderOwner("DROP_FOLDER_OWNER", c)

	// Timed backups (§20 erratum 321), read after SCRATCH_DIR and STORAGE_DIR,
	// which BACKUP_DIR may not overlap.
	l.backupBlock(&c)
	// The database snapshot in the bucket (§20 erratum 343), after
	// STORAGE_BACKEND, which decides whether it can be on at all.
	l.snapshotBlock(&c)

	c.DefaultTTLSeconds = l.ttl("DEFAULT_CLIP_TTL")
	c.SweepInterval = l.duration("SWEEP_INTERVAL", 60*time.Second)
	c.SweepGrace = l.duration("SWEEP_GRACE", time.Hour)
	c.SweepBatch = l.intMin("SWEEP_BATCH", 50, 1)
	c.MaxUploadBytes = l.int64Range("MAX_UPLOAD_BYTES", DefaultMaxUploadBytes, 1, maxUploadCeiling)
	c.ScratchMinFree = l.int64Min("SCRATCH_MIN_FREE_BYTES", 512<<20, 0)
	c.UploadChunkBytes = l.int64Range("UPLOAD_CHUNK_BYTES", DefaultUploadChunkBytes, MinUploadChunkBytes, MaxUploadChunkBytes)
	c.SocialMaxBytes = l.int64Min("SOCIAL_MAX_BYTES", 25<<20, 0)
	c.CaptureReadyWait = l.waitDuration("CAPTURE_READY_WAIT", 30*time.Second, CaptureReadyWaitMax)
	c.LightTag.Text = l.lightTagText("LIGHT_TAG_TEXT")

	c.Transcode.Concurrency = l.intRange("TRANSCODE_CONCURRENCY", 1, 1, MaxTranscodeConcurrency)
	c.Transcode.Preset = l.preset("TRANSCODE_PRESET", "veryfast")
	c.Transcode.CRF = l.intRange("TRANSCODE_CRF", 23, 0, 51)
	c.Transcode.MaxFPS = l.intMin("TRANSCODE_MAX_FPS", 60, 1)
	c.Transcode.MaxW = l.evenDim("TRANSCODE_MAX_WIDTH", 1280)
	c.Transcode.MaxH = l.evenDim("TRANSCODE_MAX_HEIGHT", 720)
	c.Transcode.StallTimeout = l.duration("TRANSCODE_STALL_TIMEOUT", 5*time.Minute)
	c.Transcode.HardTimeout = l.duration("TRANSCODE_HARD_TIMEOUT", 6*time.Hour)
	c.Transcode.MaxAttempts = l.intMin("TRANSCODE_MAX_ATTEMPTS", 3, 1)
	c.Transcode.MaxTransient = l.intMin("TRANSCODE_MAX_TRANSIENT", 10, 1)
	c.Transcode.MaxBitrate = l.offOrRange("TRANSCODE_MAX_BITRATE", 0, minBitrate, maxBitrate)
	// Read AFTER TRANSCODE_HARD_TIMEOUT, whose value is its default (§20
	// erratum 230). A variable whose default is another variable cannot be
	// spelled in compose.yaml, so compose passes it through EMPTY and this
	// line is the only place the coupling exists.
	c.JobsUnstickAfter = l.offDuration("JOBS_UNSTICK_AFTER", c.Transcode.HardTimeout+UnstickMargin)
	c.Transcode.FullCodec = l.oneOf("FULL_CODEC", FullCodecH264, FullCodecH264, FullCodecAV1)
	c.Transcode.Threads = l.intRange("TRANSCODE_THREADS", 0, 0, MaxTranscodeThreads)
	c.Transcode.HWAccel = l.hwaccel("TRANSCODE_HWACCEL")
	c.Transcode.FFmpeg = l.str("FFMPEG_PATH", "ffmpeg")
	c.Transcode.FFprobe = l.str("FFPROBE_PATH", "ffprobe")

	c.SessionTTL = l.duration("SESSION_TTL", 720*time.Hour)
	c.SessionMaxTTL = l.duration("SESSION_MAX_TTL", DefaultSessionMaxTTL)
	c.SessionCookie = l.cookieName("SESSION_COOKIE_NAME", "clippet_session")
	l.cookiePrefix("SESSION_COOKIE_NAME", c.SessionCookie, c.PublicBaseURL)
	c.Registration = l.registration("REGISTRATION")
	c.LibraryScope = l.oneOf("LIBRARY_SCOPE", LibraryScopeShared, LibraryScopeShared, LibraryScopeOwn)
	c.InstanceBadge = l.instanceBadge("INSTANCE_BADGE")
	c.CardComponents = l.oneOf("CARD_COMPONENTS", "on", "on", "off") == "on"
	c.SetupWizardOff = l.oneOf("SETUP_WIZARD", "on", "on", "off") == "off"
	c.UploadForm = l.oneOf(EnvDiscordUploadForm, "on", "on", "off") == "on"
	c.EnrichUpload = l.oneOf(EnvDiscordEnrichUpload, "off", "on", "off") == "on"
	c.TrustedProxies = l.prefixes("TRUSTED_PROXIES")

	c.ViewDedupeTTL = l.duration("VIEW_DEDUPE_TTL", 30*time.Minute)
	c.ViewDedupeMax = l.intMin("VIEW_DEDUPE_MAX", 50000, 1)
	c.RateAuthPerMin = l.intMin("RATE_AUTH_PER_MIN", 10, 1)
	c.RateUploadPerHour = l.intMin("RATE_UPLOAD_PER_HOUR", 60, 1)
	c.RateUploadPerHourPerIP = l.intMin("RATE_UPLOAD_PER_HOUR_PER_IP", 120, 1)
	c.RateMaxKeys = l.intMin("RATE_LIMIT_MAX_KEYS", 10000, 1)

	c.DiscordWebhook = l.discordWebhook("DISCORD_WEBHOOK_URL")
	c.DiscordRoutes = l.discordRoutes("DISCORD_WEBHOOK_ROUTES")
	c.DiscordUnmatched = l.oneOf("DISCORD_WEBHOOK_UNMATCHED", DiscordUnmatchedDefault,
		DiscordUnmatchedDefault, DiscordUnmatchedSkip)
	// `skip` means "announce only the clips a rule matched". With no rules
	// nothing can ever match, so the pair announces NOTHING while
	// DISCORD_WEBHOOK_URL still looks configured — one typo in
	// DISCORD_WEBHOOK_ROUTES and every post stops with a boot log that says
	// "discord webhook configured". Silence is the worst possible failure for
	// an integration, so this fails at boot rather than at 3am (§4). With no
	// default webhook either, Discord is simply off and the variable is inert.
	if c.DiscordUnmatched == DiscordUnmatchedSkip && len(c.DiscordRoutes) == 0 && c.DiscordWebhook.Configured() {
		l.fail("DISCORD_WEBHOOK_UNMATCHED", "%q means \"announce only clips a DISCORD_WEBHOOK_ROUTES rule matched\", but no rules are configured, so no clip would ever be announced; add the rules, or unset DISCORD_WEBHOOK_URL to turn Discord off deliberately",
			DiscordUnmatchedSkip)
	}

	// The bot (ROADMAP #33, §20 errata 253-255). Read as ONE value: its three
	// variables are all-or-nothing, and the reason lives in discordBot.
	c.DiscordBot = l.discordBot()
	// Clippet Auto on the box (§20 erratum 333): four variables, one value, and
	// prompt|auto needs the bot above.
	c.DiscordAuto = l.discordAuto(c.DiscordBot)
	// Sign in with Discord (ROADMAP #44, §20 erratum 302). Two variables, read
	// as ONE value for the bot's reason: both or none.
	c.DiscordLogin = l.discordLogin()
	// The clip webhook (ROADMAP #54, §20 erratum 319). Two variables, read as
	// ONE value for the login's reason: both or none.
	c.ClipWebhook = l.clipWebhook()

	c.StorageBudgetBytes = l.int64Min("STORAGE_BUDGET_BYTES", 0, 0)
	// The two optional prices (ROADMAP #39, §20 erratum 270). Unset is the
	// default and means "no cost line anywhere"; either one may be set alone,
	// because a friend on R2 pays for storage and nothing for egress.
	c.StoragePriceMicros = l.price("STORAGE_PRICE_PER_GB_MONTH")
	c.EgressPriceMicros = l.price("EGRESS_PRICE_PER_GB")

	// The provider preset and the opt-in cap (§20 erratum 277). Unset is the
	// custom case for the first and OFF for the second, which is every
	// deployment that existed before them.
	c.StoragePricing = l.oneOf("STORAGE_PRICING", "", PricingPresets...)
	c.StorageBudgetHard = l.boolean("STORAGE_BUDGET_HARD", false)
	// A cap needs a line to cap at. With no budget, `1` could only mean
	// "refuse at zero bytes", which would refuse every upload on a fresh
	// instance — a silent, total outage from one variable nobody meant that
	// way. It fails at boot instead, naming both variables (§4).
	if c.StorageBudgetHard && c.StorageBudgetBytes <= 0 {
		l.fail("STORAGE_BUDGET_HARD", "1 makes STORAGE_BUDGET_BYTES a cap, but no budget is set: set STORAGE_BUDGET_BYTES to the line you want uploads refused at, or leave STORAGE_BUDGET_HARD at 0 (DEPLOY.md §25)")
	}

	c.SSRTimeout = l.duration("SSR_TIMEOUT", 5*time.Second)
	c.LogLevel = l.oneOf("LOG_LEVEL", "info", "debug", "info", "warn", "error")
	c.LogFormat = l.oneOf("LOG_FORMAT", "text", "text", "json")

	if len(l.errs) > 0 {
		return Config{}, &Errors{Errs: l.errs}
	}
	return c, nil
}

// Errors is what Load returns when one or more variables are invalid: every
// failure, in the order the loader met them (§20 erratum 104). Callers that
// only want a message can use Error(); main prints one line per entry under a
// single `clippet: config:` header, and errors.Is/As reach through Unwrap.
type Errors struct{ Errs []error }

func (e *Errors) Error() string {
	parts := make([]string, len(e.Errs))
	for i, err := range e.Errs {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "; ")
}

func (e *Errors) Unwrap() []error { return e.Errs }

// loader accumulates validation errors; every lookup still runs after one
// fails (the zero values it returns are discarded with the Config), which is
// what makes reporting all of them possible.
type loader struct {
	getenv func(string) string
	errs   []error
	// migrating is LoadForMigrate's one relaxation: on STORAGE_BACKEND=fs the
	// four bucket variables are READ instead of refused, because `admin
	// storage migrate` has to hold both stores open at once (§20 erratum 276).
	migrating bool
}

func (l *loader) fail(name, format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf("%s: %s", name, fmt.Sprintf(format, args...)))
}

func (l *loader) raw(name string) string {
	return strings.TrimSpace(l.getenv(name))
}

func (l *loader) str(name, def string) string {
	if v := l.raw(name); v != "" {
		return v
	}
	return def
}

// cookieName validates a cookie name as an RFC 6265 token (§20 erratum
// 28). net/http drops a Set-Cookie whose name holds a separator (a space,
// '"', ';', ',', '=', ':', '/', …) silently: register and login would answer
// success and set no cookie, nobody could stay logged in, and nothing would
// be logged — the class of misconfiguration §4 says must fail at boot.
func (l *loader) cookieName(name, def string) string {
	v := l.str(name, def)
	for _, r := range v {
		if !isCookieTokenRune(r) {
			l.fail(name, "must be an RFC 6265 token (letters, digits and !#$%%&'*+-.^_`|~; no spaces, quotes, ';', ',', '=', ':' or '/'), got %q", v)
			return def
		}
	}
	return v
}

// cookiePrefix rejects a __Host-/__Secure- cookie name on a plain-http
// PUBLIC_BASE_URL (§20 erratum 35). §14 sets the cookie's Secure flag from
// the base URL's scheme alone, and RFC 6265bis makes a browser DISCARD a
// prefixed cookie that arrives without Secure — so the pair boots, answers
// 200 to login, inserts session rows, logs nothing and leaves every browser
// permanently logged out: erratum 28's failure mode, reached through the
// browser's rule instead of net/http's parser.
func (l *loader) cookiePrefix(name, value string, base *url.URL) {
	if base == nil || base.Scheme == "https" {
		return
	}
	for _, p := range []string{"__Host-", "__Secure-"} {
		if strings.HasPrefix(value, p) {
			l.fail(name, "the %s prefix requires a https PUBLIC_BASE_URL (browsers discard such a cookie when it is set without Secure), got %q on %s", p, value, base.Scheme)
			return
		}
	}
}

// isCookieTokenRune reports whether r may appear in a cookie-name (an RFC
// 9110 token: printable ASCII minus the separators).
func isCookieTokenRune(r rune) bool {
	switch {
	case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", r)
}

func (l *loader) required(name string) string {
	v := l.raw(name)
	if v == "" {
		l.fail(name, "required")
	}
	return v
}

func (l *loader) boolean(name string, def bool) bool {
	v := strings.ToLower(l.raw(name))
	switch v {
	case "":
		return def
	case "true", "1":
		return true
	case "false", "0":
		return false
	}
	l.fail(name, "must be true/false/1/0, got %q", v)
	return def
}

func (l *loader) duration(name string, def time.Duration) time.Duration {
	v := l.raw(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(name, "invalid duration %q (use Go syntax: 60s, 5m, 720h)", v)
		return def
	}
	if d <= 0 {
		l.fail(name, "must be positive, got %s", d)
		return def
	}
	return d
}

// boundedDuration reads a positive duration with a floor AND a ceiling, and
// refuses outside either rather than clamping (§20 erratum 278's
// MEDIA_PRESIGN_TTL). Clamping is the wrong move for this one: a value under
// the floor is somebody who meant minutes and typed seconds, and silently
// serving them an hour would hide the typo until a viewer's seek broke.
func (l *loader) boundedDuration(name string, def, minimum, maximum time.Duration) time.Duration {
	v := l.raw(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(name, "invalid duration %q (use Go syntax: 15m, 1h, 24h)", v)
		return def
	}
	if d < minimum || d > maximum {
		l.fail(name, "must be between %s and %s, got %s", minimum, maximum, d)
		return def
	}
	return d
}

// CaptureReadyWaitMax is the hard ceiling on how long POST /api/upload will
// hold its 201 (§20 erratum 97) — the bound on CAPTURE_READY_WAIT at boot and
// on the request's own `wait` field. A minute is already far past the point
// where a capture tool's user thinks the upload has hung, and the wait holds a
// connection open outside the TimeoutHandler, so it must not be operator-
// settable to "forever".
const CaptureReadyWaitMax = 60 * time.Second

// waitDuration reads a duration whose ZERO is meaningful, bounded above by
// maximum. `duration` refuses 0 — and rightly so for every other §4 duration,
// where a zero sweep interval or a zero timeout is a misconfiguration — but 0
// is precisely how an operator turns CAPTURE_READY_WAIT off.
func (l *loader) waitDuration(name string, def, maximum time.Duration) time.Duration {
	v := l.raw(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(name, "invalid duration %q (use Go syntax: 8s, 500ms, or 0 to switch it off)", v)
		return def
	}
	if d < 0 {
		l.fail(name, "must not be negative, got %s", d)
		return def
	}
	if d > maximum {
		l.fail(name, "must be <= %s, got %s", maximum, d)
		return def
	}
	return d
}

// offDuration reads a duration whose ZERO switches a feature off, like
// waitDuration, and which has no ceiling — the shape of JOBS_UNSTICK_AFTER
// (§20 erratum 230), where "off" is a deliberate operator choice and the only
// bound that matters is the FLOOR, applied against another variable in
// UnstickPlan rather than here.
func (l *loader) offDuration(name string, def time.Duration) time.Duration {
	v := l.raw(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(name, "invalid duration %q (use Go syntax: 8h, 30m, or 0 to switch it off)", v)
		return def
	}
	if d < 0 {
		l.fail(name, "must not be negative, got %s", d)
		return def
	}
	return d
}

// MaxTranscodeConcurrency bounds TRANSCODE_CONCURRENCY (§20 erratum 104).
// Every slot is a live ffmpeg with its own encoder buffers on a machine whose
// sweeper and HTTP server share the same process, and DEPLOY §18 advises
// raising the knob for a bigger box -- so the failure mode of a typo (a
// trailing zero on 4) is the OOM killer taking the whole instance, HTTP
// included, rather than one encode. Sixteen is past any machine this design
// targets (the rule of thumb is min(cores, RAM_GiB)) and turns that typo into
// a boot error that names the variable.
const MaxTranscodeConcurrency = 16

// DefaultMaxUploadBytes is MAX_UPLOAD_BYTES unset (4 GiB, §4). The hosted
// Clippet Auto (§20 erratum 334) cannot read a box's own value, so it applies
// this one and the box answers its own 413 past a lower one.
const DefaultMaxUploadBytes = 4 << 30

// The chunked web upload's chunk (§20 erratum 344): 50 MiB by default, never
// under 5 MiB (a smaller one only multiplies the requests) and never over
// 95 MB, which keeps one chunk plus its headers under the 100 MB the edge
// accepts on its two cheapest plans, for a value somebody wrote meaning "as big
// as it goes". 95 MB is already fewer than fifty requests for a 4 GiB file.
const (
	DefaultUploadChunkBytes = 50 << 20
	MinUploadChunkBytes     = 5 << 20
	MaxUploadChunkBytes     = 95_000_000
)

// maxUploadCeiling bounds MAX_UPLOAD_BYTES (§20 erratum 33). §4 documents the
// knob as "no arbitrary caps — this is your disk", so an operator meaning
// "unlimited" plausibly writes math.MaxInt64; the handler then adds the 1 MiB
// multipart slack, and an int64 that wraps negative makes http.MaxBytesReader
// (n < 0 → 0) answer 413 to every upload. 4 EiB is past any disk and leaves
// the whole arithmetic headroom.
const maxUploadCeiling = 1 << 62

// minBitrate / maxBitrate bound TRANSCODE_MAX_BITRATE when it is set at all
// (§20 erratum 124). The floor exists because a ceiling below it is not a
// setting but a mistake — 50 kbps of 720p is a slideshow x264 will spend the
// whole encode failing to hit — and a value like `2500` from an operator
// thinking in kilobits must fail at boot rather than silently ruin every
// clip. The roof is past any sane rendition and keeps the `-bufsize`
// doubling inside int64.
const (
	minBitrate = 100_000
	maxBitrate = 1 << 34
)

// offOrRange reads an integer that is either 0 ("off") or inside [minimum,
// maximum]. It is the shape of a knob whose zero is a SETTING rather than a
// bound, so the range check must not reject it — the same distinction
// waitDuration draws for CAPTURE_READY_WAIT.
func (l *loader) offOrRange(name string, def, minimum, maximum int64) int64 {
	n := l.int64Min(name, def, 0)
	if n == 0 {
		return 0
	}
	if n < minimum || n > maximum {
		l.fail(name, "must be 0 (off) or between %d and %d, got %d", minimum, maximum, n)
		return def
	}
	return n
}

// The STORAGE_PRICING preset names (§20 erratum 277). They are the VALUES the
// variable takes, so they live with the loader that validates them; the dated
// list prices and the free-allowance rules behind each one are a table in
// internal/cost, which imports this package and not the other way round.
//
// `local` is a real row and not an absence: a box with its own disks has a
// provider bill of zero, which is a number an operator can be shown, and it is
// what the owner asked for: a zero printed in the cost cell. An unset variable is the
// CUSTOM case and shows no cost at all unless a price was typed.
const (
	PricingLocal = "local"
	PricingR2    = "r2"
	PricingB2    = "b2"
	PricingS3    = "s3"
)

// PricingPresets is the accepted set, in the order the boot error prints it.
// A cost test asserts the preset table holds exactly these names, so a row
// added on one side and not the other fails the build rather than the boot.
var PricingPresets = []string{PricingLocal, PricingR2, PricingB2, PricingS3}

// PriceScale is how many parts a price is held in: MICROS, a millionth of the
// operator's currency unit (§20 erratum 270). It is exported because the one
// package that does the arithmetic (internal/cost) must divide by exactly this.
const PriceScale = 1_000_000

// MaxPriceFractionDigits is how far a price may be written out. Six is a
// millionth of a currency unit per GB, which is three orders of magnitude
// finer than anything a provider quotes (R2's storage is 0.015 a GB-month,
// B2's egress 0.01 a GB) and is exactly what PriceScale holds — a seventh
// digit could not be stored, so it is refused rather than silently dropped.
const MaxPriceFractionDigits = 6

// MaxPricePerGB bounds the whole part of a price. It is a TYPO GUARD and not a
// statement about currencies: a million units per GB is past any real price in
// any currency (a GB-month at that rate would cost more than the disk), while a
// misplaced decimal point or a pasted thousands separator lands far above it.
// Without a bound the arithmetic would also have to defend itself against a
// price with twenty digits in it.
const MaxPricePerGB = 1_000_000

// price reads one optional per-GB price — STORAGE_PRICE_PER_GB_MONTH or
// EGRESS_PRICE_PER_GB (ROADMAP #39, §20 erratum 270) — into millionths of the
// operator's own currency unit. nil is UNSET and is the default.
//
// Unset is not zero, here or anywhere downstream: with no price the cost line
// is absent from /healthz, from `admin stats` and from the settings page, so an
// operator who has configured nothing is never shown a number that looks like a
// bill for nothing. `0` IS a number, and a deliberate one — it is how a friend
// on R2 says "egress costs me nothing" and gets a 0.00 that MEANS zero.
//
// The grammar is a plain decimal, which is what a price list is written in:
// digits, at most one point, at most MaxPriceFractionDigits after it. No sign
// (a negative price is a typo, not a discount), no exponent, no separator, no
// currency symbol — the app never names a currency, and a `$` in the value
// would be the operator telling it one it cannot use.
func (l *loader) price(name string) *int64 {
	v := l.raw(name)
	if v == "" {
		return nil
	}
	micros, reason := parsePrice(v)
	if reason != "" {
		l.fail(name, "%s; write it as a plain decimal number of your own currency per GB, with at most %d digits after the point (0.015, 0.36, 0) — or leave it unset, which removes the cost line entirely",
			reason, MaxPriceFractionDigits)
		return nil
	}
	return &micros
}

// parsePrice turns a decimal price into micros, or returns the reason it is
// not one. The reason ECHOES the value: a price is the operator's own number,
// not a credential, and a rejection that does not show what was read makes a
// stray space impossible to find.
func parsePrice(v string) (int64, string) {
	whole, frac, hasPoint := strings.Cut(v, ".")
	if hasPoint && frac == "" {
		return 0, fmt.Sprintf("%q ends in a decimal point", v)
	}
	if whole == "" && !hasPoint {
		return 0, fmt.Sprintf("%q is not a number", v)
	}
	if whole == "" {
		whole = "0" // ".015" is a price somebody typed; it means 0.015
	}
	if !isDigits(whole) || (hasPoint && !isDigits(frac)) {
		return 0, fmt.Sprintf("%q is not a plain decimal number (no sign, no exponent, no separator and no currency symbol)", v)
	}
	if len(frac) > MaxPriceFractionDigits {
		return 0, fmt.Sprintf("%q has %d digits after the point", v, len(frac))
	}
	// Leading zeros are not magnitude: `00000015` is fifteen, and the length
	// check below is a guard against a pasted separator or a misplaced point,
	// not against a padded number. Trimmed here rather than inside the check so
	// that ParseInt sees the same string the check measured (§20 erratum 275).
	if trimmed := strings.TrimLeft(whole, "0"); trimmed != whole {
		whole = trimmed
		if whole == "" {
			whole = "0" // "000" and "0.015" are both a zero whole part
		}
	}
	if len(whole) > len(strconv.Itoa(MaxPricePerGB)) {
		return 0, fmt.Sprintf("%q is above %d per GB", v, MaxPricePerGB)
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || units > MaxPricePerGB {
		return 0, fmt.Sprintf("%q is above %d per GB", v, MaxPricePerGB)
	}
	micros := units * PriceScale
	if frac != "" {
		// Right-pad to exactly MaxPriceFractionDigits so "0.5" and "0.500000"
		// are the same price.
		padded := frac + strings.Repeat("0", MaxPriceFractionDigits-len(frac))
		n, err := strconv.ParseInt(padded, 10, 64)
		if err != nil {
			return 0, fmt.Sprintf("%q is not a plain decimal number", v)
		}
		micros += n
	}
	return micros, ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// int64Range is int64Min with an upper bound.
func (l *loader) int64Range(name string, def, minimum, maximum int64) int64 {
	n := l.int64Min(name, def, minimum)
	if n > maximum {
		l.fail(name, "must be <= %d, got %d", maximum, n)
		return def
	}
	return n
}

func (l *loader) int64Min(name string, def, minimum int64) int64 {
	v := l.raw(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		l.fail(name, "invalid integer %q", v)
		return def
	}
	if n < minimum {
		l.fail(name, "must be >= %d, got %d", minimum, n)
		return def
	}
	return n
}

func (l *loader) intMin(name string, def, minimum int) int {
	return int(l.int64Min(name, int64(def), int64(minimum)))
}

func (l *loader) intRange(name string, def, lo, hi int) int {
	n := l.intMin(name, def, lo)
	if n > hi {
		l.fail(name, "must be <= %d, got %d", hi, n)
		return def
	}
	return n
}

func (l *loader) evenDim(name string, def int) int {
	n := l.intMin(name, def, 2)
	if n%2 != 0 {
		l.fail(name, "must be even, got %d", n)
		return def
	}
	return n
}

func (l *loader) oneOf(name, def string, allowed ...string) string {
	v := strings.ToLower(l.raw(name))
	if v == "" {
		return def
	}
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	l.fail(name, "must be one of %s, got %q", strings.Join(allowed, "|"), v)
	return def
}

// hwaccel reads TRANSCODE_HWACCEL (§20 erratum 216). Empty and the word `none`
// both mean the CPU argv; `cuda` is the only hardware path this build renders.
// oneOf is not used because its message would read "must be one of |cuda".
func (l *loader) hwaccel(name string) string {
	switch v := strings.ToLower(l.raw(name)); v {
	case "", "none":
		return HWAccelNone
	case HWAccelCUDA:
		return HWAccelCUDA
	default:
		l.fail(name, "must be empty (the CPU argv) or %q, got %q", HWAccelCUDA, v)
		return HWAccelNone
	}
}

// lightTagText reads LIGHT_TAG_TEXT (§20 erratum 214): the template burned into
// the LIGHT copy, `off` for no tag at all, absent for the shipped default.
//
// The text is a picture, not markup, so the only thing refused is a value that
// cannot be one line: a control character (a newline included) would make
// `drawtext` render a second row over the frame, or nothing at all.
func (l *loader) lightTagText(name string) string {
	v := l.raw(name)
	switch {
	case v == "":
		return DefaultLightTagText
	case strings.EqualFold(v, LightTagOff):
		return ""
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			l.fail(name, "must be one line of text (found a control character); use %q to switch the tag off", LightTagOff)
			return DefaultLightTagText
		}
	}
	if len([]rune(v)) > MaxLightTagRunes {
		l.fail(name, "must be at most %d characters, got %d", MaxLightTagRunes, len([]rune(v)))
		return DefaultLightTagText
	}
	return v
}

// instanceBadge reads INSTANCE_BADGE (§20 erratum 285): the word an instance
// that is not the production one wears, or "" for the production one.
//
// The rule is narrow on purpose, because the value is rendered in four places
// at once — an HTML attribute, a tag beside the wordmark, the `<title>` prefix
// and the site line Discord draws as the logo — and a word a person READS is
// the whole point of it. Lowercase letters, digits and `-`, at most
// MaxInstanceBadgeRunes of them: `dev`, `staging`, `lab-test`. That set needs
// no escaping anywhere it lands, which is a property worth having by
// construction rather than by four correct template calls; and the length keeps
// the tag a tag rather than a second wordmark.
//
// Anything else is a boot error naming the variable and the rule (§4), never a
// silent trim or a lowercasing: an operator who wrote `Dev` meant something,
// and quietly rendering `dev` for it is how a page comes to disagree with the
// file that configured it.
func (l *loader) instanceBadge(name string) string {
	v := l.raw(name)
	if v == "" {
		return ""
	}
	if len([]rune(v)) > MaxInstanceBadgeRunes {
		l.fail(name, "must be at most %d characters, got %d (%q) — it is a word beside the wordmark, not a second wordmark", MaxInstanceBadgeRunes, len([]rune(v)), v)
		return ""
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			l.fail(name, "must be lowercase letters, digits and '-' only (a word a person reads, such as \"dev\", \"staging\" or \"lab-test\"), got %q", v)
			return ""
		}
	}
	return v
}

// MaxInstanceBadgeRunes bounds INSTANCE_BADGE. Twelve: `staging` and `lab-test`
// fit, and the tag stays one short word beside a 21 px wordmark in a 45 px
// header band rather than a line that wraps the header on a phone.
const MaxInstanceBadgeRunes = 12

// MaxLightTagRunes bounds LIGHT_TAG_TEXT. The line is burned into a 1280×720
// frame at about 2 % of its height, so eighty characters already runs off the
// edge; the bound is there to make that a boot error rather than a corner of
// every card nobody can read.
const MaxLightTagRunes = 80

func (l *loader) preset(name, def string) string {
	v := strings.ToLower(l.raw(name))
	if v == "" {
		return def
	}
	if !x264Presets[v] {
		l.fail(name, "unknown libx264 preset %q", v)
		return def
	}
	return v
}

// RegistrationMode is the value of REGISTRATION (§20 erratum 106).
type RegistrationMode string

// The three registration modes. `first` is the DEFAULT and the reason this
// stopped being a bool: `open` on a public origin means anyone who finds the
// hostname has an account the moment the documented tunnel comes up, and
// `closed` out of the box leaves a fresh install with no way in at all. The
// first account bootstraps the instance and closes the door behind it; an
// operator who wants friends to sign up sets `open` deliberately, and
// `clippet admin users create` adds one at any time without reopening
// anything.
const (
	RegistrationOpen   RegistrationMode = "open"
	RegistrationFirst  RegistrationMode = "first"
	RegistrationClosed RegistrationMode = "closed"
)

// DefaultSessionMaxTTL is the SESSION_MAX_TTL default: ninety days from the
// session's creation, whatever the sliding SESSION_TTL does (§20 erratum 105).
const DefaultSessionMaxTTL = 2160 * time.Hour

// The two Discord modes (§20 erratum 341): the door is the bot's `/signin` in
// a bound server, and the web form is CLOSED exactly as under `closed`.
// `members` lets any member of a bound server in; `role:<id>` only a member
// whose signed interaction carries that one role. RegistrationRolePrefix is
// the spelling before the id; the mode's value is the whole string.
const RegistrationMembers RegistrationMode = "members"

// RegistrationRolePrefix is an untyped string on purpose: it is compared with
// and cut from plain strings as well as modes.
const RegistrationRolePrefix = "role:"

// AlwaysOpen reports whether registration is open regardless of how many
// accounts exist. `first` answers false here and is resolved against the
// users table by the caller (the register handler and the two auth pages).
// The two Discord modes answer false too: /register stays closed under them.
func (m RegistrationMode) AlwaysOpen() bool { return m == RegistrationOpen }

// DiscordSignIn reports whether `/signin` may mint a login link (§20 erratum
// 341): `members` or `role:<id>`. Every other mode refuses the command before
// a token exists.
func (m RegistrationMode) DiscordSignIn() bool {
	return m == RegistrationMembers || m.SignInRole() != ""
}

// SignInRole is the role id of `role:<id>`, "" for every other mode. The id
// was validated as a snowflake at boot.
func (m RegistrationMode) SignInRole() string {
	if s := string(m); strings.HasPrefix(s, RegistrationRolePrefix) {
		return s[len(RegistrationRolePrefix):]
	}
	return ""
}

// NeedsFirstUser reports whether this mode's answer depends on the users
// table being empty.
func (m RegistrationMode) NeedsFirstUser() bool { return m == RegistrationFirst }

// String renders the mode for a log line; the zero value reads as the
// default, which is what a Config built by hand in a test gets.
func (m RegistrationMode) String() string {
	if m == "" {
		return string(RegistrationFirst)
	}
	return string(m)
}

func (l *loader) registration(name string) RegistrationMode {
	// `role:<id>` is the one value that is not a word (§20 erratum 341): the
	// prefix is matched case-insensitively like the words, and the id must be a
	// Discord snowflake — a role NAME pasted here would gate on nothing at all.
	if v := l.raw(name); len(v) >= len(RegistrationRolePrefix) && strings.EqualFold(v[:len(RegistrationRolePrefix)], RegistrationRolePrefix) {
		id := strings.TrimSpace(v[len(RegistrationRolePrefix):])
		if !isSnowflake(id) {
			l.fail(name, "role:<id> must name a Discord ROLE id: %d-%d digits (Discord → Developer Mode → right-click the role → Copy Role ID), got %q",
				minSnowflakeDigits, maxSnowflakeDigits, id)
			return RegistrationFirst
		}
		return RegistrationMode(RegistrationRolePrefix + id)
	}
	return RegistrationMode(l.oneOf(name, string(RegistrationFirst),
		string(RegistrationOpen), string(RegistrationFirst), string(RegistrationClosed), string(RegistrationMembers)))
}

// baseURL validates PUBLIC_BASE_URL: absolute, http or https, a host, and no
// path other than an optional trailing slash (which is trimmed).
func (l *loader) baseURL(name string) *url.URL {
	v := l.required(name)
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil {
		l.fail(name, "invalid URL %q: %v", v, err)
		return nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		l.fail(name, "scheme must be http or https, got %q", u.Scheme)
		return nil
	}
	if u.Host == "" || u.Hostname() == "" {
		l.fail(name, "missing host in %q", v)
		return nil
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		l.fail(name, "must be a bare origin scheme://host[:port], got %q", v)
		return nil
	}
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		l.fail(name, "must not have a path (got %q)", u.Path)
		return nil
	}
	u.Path, u.RawPath = "", ""
	return u
}

// endpoint validates S3_ENDPOINT as a bare origin scheme://host[:port].
// store.New hands only the host to minio-go, so a path, query, fragment or
// userinfo would be dropped silently and surface as 403/404 on every object
// call — fail fast at boot instead (§4). A trailing slash is tolerated and
// trimmed; the returned value is normalised to scheme://host[:port].
func (l *loader) endpoint(name string) string {
	return l.endpointOf(name, l.required(name))
}

// optionalEndpoint is endpoint without the requirement: the shape is still
// validated, an empty value is simply not an error. It exists for the one
// caller that has both stores in hand (LoadForMigrate on `fs`).
func (l *loader) optionalEndpoint(name string) string {
	return l.endpointOf(name, l.raw(name))
}

func (l *loader) endpointOf(name, v string) string {
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		l.fail(name, "must be a full URL with scheme and host, got %q", v)
		return ""
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(u.Path, "/") != "" {
		l.fail(name, "must be a bare origin scheme://host[:port] (no path, query, fragment or userinfo), got %q", v)
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// s3Block reads the four variables that describe a BUCKET, under whichever of
// the three rules STORAGE_BACKEND has put in force (ROADMAP #37, §20 erratum
// 276).
//
// On `fs` they are REFUSED, not ignored, and that is the point of the feature
// rather than pedantry. §15's guarantee is "the only outbound destinations
// this binary has are the S3 endpoint, Discord, and the one URL in
// CLIP_WEBHOOK_URL", and it is enforced HERE — by the endpoint validator, the
// Discord allowlist and the clip webhook being a value the operator wrote —
// because a promise an operator cannot check is not a promise. On `fs` the
// media path has no outbound destination at all, so the sentence gets
// STRONGER: with the Discord features and the clip webhook unset, a
// `STORAGE_BACKEND=fs` instance opens no outbound socket whatsoever. A
// leftover `S3_ENDPOINT=` line in `.env` would leave a reader unable to tell
// that by reading the file, so it fails at boot naming itself.
//
// The relaxation for `admin storage migrate` is LoadForMigrate's, and it is a
// separate ENTRY POINT rather than a variable precisely so that `serve` cannot
// reach it.
func (l *loader) s3Block(c *Config) {
	switch {
	case c.StorageBackend != BackendFS:
		if strings.TrimSpace(l.raw("S3_ENDPOINT")) == "" && strings.TrimSpace(l.raw("S3_BUCKET")) != "" {
			// The upgrade trap (§20 erratum 348 (d)): `.env.example` shipped
			// an active `S3_BUCKET=clips` for the bundled bucket server, whose
			// endpoint compose supplied. That server is gone (erratum 345), so
			// such a `.env` reaches here with a bucket and no endpoint, and a
			// bare "required" sends its owner nowhere.
			l.fail("S3_ENDPOINT", "required: S3_BUCKET is set but S3_ENDPOINT is not. A .env from before the bundled bucket server was withdrawn (DESIGN.md §20 erratum 345) names S3_BUCKET alone: move those clips with DEPLOY.md §32.6, or set S3_ENDPOINT to your own bucket's endpoint")
		} else {
			c.S3.Endpoint = l.endpoint("S3_ENDPOINT")
		}
		c.S3.Bucket = l.required("S3_BUCKET")
		c.S3.AccessKey = l.required("S3_ACCESS_KEY")
		c.S3.SecretKey = l.required("S3_SECRET_KEY")
	case l.migrating:
		c.S3.Endpoint = l.optionalEndpoint("S3_ENDPOINT")
		c.S3.Bucket = l.raw("S3_BUCKET")
		c.S3.AccessKey = l.raw("S3_ACCESS_KEY")
		c.S3.SecretKey = l.raw("S3_SECRET_KEY")
	default:
		for _, name := range S3Vars {
			if l.raw(name) != "" {
				l.fail(name, "STORAGE_BACKEND=fs takes no S3_* variable: the media lives in STORAGE_DIR and this instance has no bucket, so remove the line (an instance serving on `fs` opens no outbound connection for media at all - DEPLOY.md §32)")
			}
		}
	}
}

// migrateS3Block reads the seven MIGRATE_S3_* variables (§20 erratum 335): the
// destination bucket of `admin storage migrate --to s3` on an instance that is
// already on a bucket. Their shapes are validated the way the S3_* ones are;
// the rule that the four bucket variables come TOGETHER is the verb's
// (MigrateS3Missing), because `--to fs` ignores all seven and must not fail on
// a half-written set it never reads.
//
// MIGRATE_S3_KEY_PREFIX unset is S3_KEY_PREFIX's own value: the database rows
// name every object by its whole key, prefix included, so a destination under
// a different prefix holds objects the switched instance's rows do not name.
// A prefix SET to a different value is not refused: writing it is the
// operator's consent (a copy kept beside another instance's keys, say).
func (l *loader) migrateS3Block(c *Config) {
	c.MigrateS3.Endpoint = l.optionalEndpoint("MIGRATE_S3_ENDPOINT")
	c.MigrateS3.Bucket = l.raw("MIGRATE_S3_BUCKET")
	c.MigrateS3.AccessKey = l.raw("MIGRATE_S3_ACCESS_KEY")
	c.MigrateS3.SecretKey = l.raw("MIGRATE_S3_SECRET_KEY")
	c.MigrateS3.Region = l.str("MIGRATE_S3_REGION", "us-east-1")
	c.MigrateS3.ForcePathStyle = l.boolean("MIGRATE_S3_FORCE_PATH_STYLE", false)
	c.MigrateS3.KeyPrefix = c.S3.KeyPrefix
	if l.raw("MIGRATE_S3_KEY_PREFIX") != "" {
		c.MigrateS3.KeyPrefix = l.keyPrefix("MIGRATE_S3_KEY_PREFIX")
	}
}

// MigrateS3Set reports whether any of the four MIGRATE_S3_* bucket variables
// is set: the verb's question "is this a bucket-to-bucket run".
func (c Config) MigrateS3Set() bool {
	m := c.MigrateS3
	return m.Endpoint != "" || m.Bucket != "" || m.AccessKey != "" || m.SecretKey != ""
}

// MigrateS3Missing names the MigrateS3Vars that are empty, sorted: the verb's
// question "is it whole".
func (c Config) MigrateS3Missing() []string {
	var missing []string
	for _, kv := range []struct{ name, v string }{
		{"MIGRATE_S3_ACCESS_KEY", c.MigrateS3.AccessKey},
		{"MIGRATE_S3_BUCKET", c.MigrateS3.Bucket},
		{"MIGRATE_S3_ENDPOINT", c.MigrateS3.Endpoint},
		{"MIGRATE_S3_SECRET_KEY", c.MigrateS3.SecretKey},
	} {
		if kv.v == "" {
			missing = append(missing, kv.name)
		}
	}
	return missing
}

// mediaBlock reads the three variables that describe how media is DELIVERED,
// under whichever rule STORAGE_BACKEND and MEDIA_DELIVERY have put in force
// (ROADMAP #34 lane F, §20 erratum 278).
//
// Three rules, and each of them is a thing an operator would otherwise
// discover from a viewer:
//
//   - `redirect` on `fs` is a boot ERROR. A directory has no URL to redirect
//     to, and there is no sensible fallback: silently proxying would leave an
//     operator who set the variable believing the bytes had left their box.
//   - A `MEDIA_PRESIGN_*` variable set while `proxy` is in force is a NOTE and
//     not an error, because it is what an operator has in `.env` while they
//     are getting ready to flip the switch, and refusing it would make the
//     switch a two-edit change. The note is printed at boot and by
//     `admin doctor`, so "I set it and nothing happened" has an answer.
//   - A plain-http presign origin under an https PUBLIC_BASE_URL is a boot
//     ERROR. The redirect is followed by the VIEWER'S BROWSER from an https
//     page, which makes it active mixed content: Chrome and Firefox block it
//     outright and the player shows nothing. Discord's proxy would follow it
//     happily, so the card would work and the watch page would not — the worst
//     shape a misconfiguration can have.
func (l *loader) mediaBlock(c *Config) {
	c.MediaDelivery = l.oneOf("MEDIA_DELIVERY", DeliveryProxy, DeliveryProxy, DeliveryRedirect)
	c.MediaPresignTTL = l.boundedDuration("MEDIA_PRESIGN_TTL", DefaultMediaPresignTTL, MinMediaPresignTTL, MaxMediaPresignTTL)
	c.S3.PresignEndpoint = l.optionalEndpoint("MEDIA_PRESIGN_ENDPOINT")

	if c.MediaDelivery == DeliveryRedirect && c.StorageBackend == BackendFS {
		l.fail("MEDIA_DELIVERY", "%q needs a bucket: STORAGE_BACKEND=%s keeps the media in a directory, which has no URL to redirect a viewer to, so this instance can only serve the bytes itself (%q - DEPLOY.md §33)",
			DeliveryRedirect, BackendFS, DeliveryProxy)
		return
	}
	if c.MediaDelivery != DeliveryRedirect {
		// The presign origin is meaningless in `proxy` mode, and carrying it
		// forward would let one later reader mistake "configured" for "in use".
		c.S3.PresignEndpoint = ""
		return
	}
	// From here the mode IS redirect, so the endpoint has to resolve to
	// something a viewer can reach.
	if c.S3.PresignEndpoint == "" {
		c.S3.PresignEndpoint = c.S3.Endpoint
	}
	if strings.HasPrefix(c.S3.PresignEndpoint, "http:") &&
		c.PublicBaseURL != nil && c.PublicBaseURL.Scheme == "https" {
		l.fail("MEDIA_PRESIGN_ENDPOINT", "is a plain-http origin (%s) while PUBLIC_BASE_URL is https (%s): a browser refuses to follow an https page's media to plain http (active mixed content) and the player stays blank, so give the bucket an https origin or set MEDIA_DELIVERY=%s (DEPLOY.md §33)",
			c.S3.PresignEndpoint, c.PublicBaseURL, DeliveryProxy)
	}
}

// MediaNote is the one thing worth saying about a MEDIA_PRESIGN_* variable an
// instance is NOT using, or "" when there is nothing to say (§20 erratum 278).
// It is computed from the raw environment rather than from the parsed Config,
// because the whole point is a value that was read and then ignored; both the
// boot line and `admin doctor` render it, so they cannot disagree.
func MediaNote(c Config, getenv func(string) string) string {
	if c.MediaDelivery == DeliveryRedirect {
		return ""
	}
	var set []string
	for _, name := range MediaVars {
		if strings.TrimSpace(getenv(name)) != "" {
			set = append(set, name)
		}
	}
	if len(set) == 0 {
		return ""
	}
	return strings.Join(set, " and ") + " is set but MEDIA_DELIVERY=" + DeliveryProxy +
		", so it is ignored and this instance serves every media byte itself (DEPLOY.md §33)"
}

// RedirectsMedia reports whether the media routes answer a 302 instead of the
// bytes. One predicate, so the handler, the CSP and the cost line cannot
// disagree about which mode is in force.
func (c Config) RedirectsMedia() bool { return c.MediaDelivery == DeliveryRedirect }

// MediaOrigin is the origin a viewer fetches media from, or "" when that is
// this app's own origin. It is what the CSP has to allow beyond 'self'
// (§20 erratum 278): CSP re-checks a redirect TARGET by host, so a media route
// that 302s off-origin is blocked on our own watch and embed pages until the
// bucket's origin is named — from configuration, never from a literal.
//
// It reads ResolvedMediaOrigin and NOT S3.PresignEndpoint, because those two
// are different strings on every deployment DEPLOY §33 is written for: with
// virtual-host bucket lookup (`S3_FORCE_PATH_STYLE=false`, the default here and
// what R2 and B2 are configured with) the signed URL carries the BUCKET in the
// host. store.MediaOrigin asks the signer instead of guessing, and `serve` and
// `admin doctor` both fill this field in from it once the store is open —
// see the field's own comment.
func (c Config) MediaOrigin() string {
	if !c.RedirectsMedia() {
		return ""
	}
	return c.ResolvedMediaOrigin
}

// dropFolderOwner validates DROP_FOLDER_OWNER (ROADMAP #42 (b), §20 erratum
// 296, amended by 296 (m)): the one account that owns whatever arrives at the
// root of STORAGE_DIR. Empty is the default and means the watcher never starts.
//
// Two rules, and the first is the one that keeps the feature honest. It used to
// be "REFUSED on `s3`", on the reading that a bucket has no root anybody can
// drop a file into. That is true of the bucket and was never the whole
// question: what the watcher needs is a DIRECTORY on this box, and `s3` can
// have one — STORAGE_DIR is already a legal (optional) value there, because it
// is what `admin storage migrate --to fs` copies into. So the rule is now about
// the thing that is actually required: **a media directory**. Naming an owner
// without one is refused, naming the variable that supplies it — which on `fs`
// is required anyway, so only a bucket deployment can reach this arm.
//
// The second is SHAPE, and it stops at the shape: the character set is
// register's own (`^[A-Za-z0-9_]{3,32}$`, spelled here because internal/config
// may not import internal/httpapi and §3's import graph is the reason), so a
// value that could not be a username at all fails beside every other bad
// variable in one report instead of at the boot after it. Whether the ACCOUNT
// exists is not a question this package can ask — it opens no database — and
// `clippet serve` refuses to start when it does not.
func (l *loader) dropFolderOwner(name string, c Config) string {
	v := l.raw(name)
	if v == "" {
		return ""
	}
	// On `fs` an empty STORAGE_DIR has already failed with its own sentence
	// ("required when STORAGE_BACKEND=fs"), and two messages about one missing
	// variable in the same report is noise, so only a bucket reaches this.
	if c.StorageBackend != BackendFS && c.StorageDir == "" {
		l.fail(name, "names the account that owns whatever is dropped at the ROOT of the media DIRECTORY, and this instance has not been given one: set STORAGE_DIR to a directory bound into the container (on STORAGE_BACKEND=%s it is the media store itself and is already required; on %s it is a folder of its own and the clips still go to the bucket) (DEPLOY.md §34)",
			BackendFS, BackendS3)
		return ""
	}
	if !dropOwnerRE.MatchString(v) {
		l.fail(name, "must be an account name: 3-32 characters of letters, digits or underscore")
		return ""
	}
	return v
}

// dropOwnerRE is register's own username rule, written again here because §3's
// import graph forbids internal/config importing internal/httpapi. A value this
// accepts may still name no account; serve refuses to boot on that.
var dropOwnerRE = regexp.MustCompile(`^[A-Za-z0-9_]{3,32}$`)

// storageDir validates STORAGE_DIR (ROADMAP #37, §20 erratum 276). It is
// required on `fs` and optional on `s3` — where it is what `admin storage
// migrate --to fs` copies INTO — and the rules below apply whenever it is set,
// because a value that would be wrong on `fs` is worth refusing before the
// operator flips the switch rather than after.
//
// The overlap rules are the ones with teeth. SCRATCH_DIR is walked by the boot
// janitor and the sweeper, which REMOVE a working directory whose clip is gone
// (§12) — a media store inside it would be deleted a clip at a time by the
// app's own housekeeping. DATA_DIR holds the SQLite file and is what `admin
// backup <dir> --rotate` and every backup recipe in DEPLOY §15 treat as "the
// small, precious one"; the media directory is the large, replaceable-only-
// from-a-backup one, and a deployment that cannot tell an operator which is
// which has already lost the argument about what to copy where.
//
// The path is a CONTAINER path (POSIX) or, for the desktop app's server on
// Windows, a drive-letter or UNC path held slashed (absDir): `path` rather than
// `filepath` either way, so the answer does not depend on the machine the
// test runs on.
func (l *loader) storageDir(name string, c Config) string {
	v := l.raw(name)
	if v == "" {
		if c.StorageBackend == BackendFS {
			l.fail(name, "required when STORAGE_BACKEND=%s: it is the absolute directory the media lives in (DEPLOY.md §32)", BackendFS)
		}
		return ""
	}
	clean, ok := l.absDir(name, v)
	if !ok {
		return ""
	}
	for _, other := range []struct{ name, dir string }{{"DATA_DIR", c.DataDir}, {"SCRATCH_DIR", c.ScratchDir}} {
		if other.dir == "" {
			continue
		}
		od := dirForm(other.dir)
		switch {
		case sameDir(clean, od):
			l.fail(name, "is %s, which is also %s=%s: the media store and that directory must be two places (DEPLOY.md §32)", clean, other.name, od)
		case pathWithin(clean, od):
			l.fail(name, "is %s, which sits INSIDE %s=%s: put it beside that directory, not under it (DEPLOY.md §32)", clean, other.name, od)
		case pathWithin(od, clean):
			l.fail(name, "is %s, which CONTAINS %s=%s: put it beside that directory, not around it (DEPLOY.md §32)", clean, other.name, od)
		}
	}
	return clean
}

// pathWithin reports whether child sits under parent. Both are already clean
// absolute paths in dirForm; the separator is appended so `/datax` is not
// inside `/data`. A Windows pair (drive letter or UNC) compares without case,
// as Windows does.
func pathWithin(child, parent string) bool {
	if parent == "/" {
		return child != "/"
	}
	if isWindowsPath(child) || isWindowsPath(parent) {
		child, parent = strings.ToLower(child), strings.ToLower(parent)
	}
	return strings.HasPrefix(child, parent+"/")
}

// sameDir is equality in dirForm, without case for a Windows path.
func sameDir(a, b string) bool {
	if isWindowsPath(a) || isWindowsPath(b) {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// isDrivePath reports a Windows drive-letter absolute path: `C:/...` or
// `C:\...`. The desktop app runs the server on Windows with STORAGE_DIR on the
// PC's own disk (run 15, R15B-1), so the shape rule takes that form as well as
// the container's POSIX one. `C:` alone is drive-relative and is not one.
func isDrivePath(v string) bool {
	if len(v) < 3 || v[1] != ':' || (v[2] != '/' && v[2] != '\\') {
		return false
	}
	c := v[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// isUNCPath reports a Windows UNC path that names a server AND a share:
// `\\server\share`, `\\server\share\sub`, or the same with forward slashes
// (`//server/share/sub`). The desktop app takes a NAS share in the same field
// as a folder on the PC (run 15d, lane uncpath). A bare `\\server` names no
// share, and `\\.\` and `\\?\` are device paths, not shares: none is one.
func isUNCPath(v string) bool {
	if len(v) < 5 || !isSlash(v[0]) || !isSlash(v[1]) || isSlash(v[2]) {
		return false
	}
	server, rest, _ := strings.Cut(strings.ReplaceAll(v[2:], `\`, "/"), "/")
	share, _, _ := strings.Cut(rest, "/")
	return server != "." && server != "?" && share != "" && share != "." && share != ".."
}

func isSlash(b byte) bool { return b == '/' || b == '\\' }

// isWindowsPath is a drive-letter or a UNC path: the two forms Windows
// compares without case.
func isWindowsPath(v string) bool { return isDrivePath(v) || isUNCPath(v) }

// dirForm is the comparable form of a directory: a drive-letter path with its
// backslashes turned to slashes and cleaned after the drive (`C:\x\..\y` ->
// `C:/y`), a UNC path slashed and cleaned KEEPING its two leading slashes
// (`\\nas\clips\.\x` -> `//nas/clips/x`; path.Clean alone folds them to one,
// which is another path), anything else path.Clean. Go's os package on
// Windows takes the slashed forms as they are.
func dirForm(v string) string {
	if isDrivePath(v) {
		n := strings.ReplaceAll(v, `\`, "/")
		return n[:2] + path.Clean(n[2:])
	}
	if isUNCPath(v) {
		n := strings.ReplaceAll(v, `\`, "/")
		return "/" + path.Clean(n[1:])
	}
	return path.Clean(v)
}

// absDir is the SHAPE half of a directory variable: an absolute, clean path
// that is not the root — POSIX (`/media`), a Windows drive-letter path
// (`C:/media` or `C:\media`, returned slashed) or a Windows UNC path
// (`\\nas\clips` or `//nas/clips`, returned slashed with its two leading
// slashes; the share itself is allowed, it is no filesystem's root). Anything
// else that starts with two separators, a bare `\\nas` among them, is not
// absolute. STORAGE_DIR and BACKUP_DIR share it, and so share its three
// sentences word for word. It reports false after failing.
func (l *loader) absDir(name, v string) (string, bool) {
	n := v
	switch {
	case isWindowsPath(v):
		n = strings.ReplaceAll(v, `\`, "/")
	case !strings.HasPrefix(v, "/") || (len(v) > 1 && isSlash(v[1])):
		l.fail(name, "must be an absolute path, got %q", v)
		return "", false
	}
	clean := dirForm(n)
	if isUNCPath(n) && !isUNCPath(clean) {
		// `//nas/clips/..` cleans to a path with no share left in it.
		l.fail(name, "must be an absolute path, got %q", v)
		return "", false
	}
	if clean != strings.TrimRight(n, "/") && clean != n {
		l.fail(name, "must be a clean absolute path with no `.` or `..` segment, got %q (did you mean %q?)", v, clean)
		return "", false
	}
	if clean == "/" || (isDrivePath(n) && clean == n[:2]+"/") {
		l.fail(name, "must not be the root of the filesystem")
		return "", false
	}
	return clean, true
}

// BackupEveryFloor is the shortest BACKUP_EVERY the loader accepts. A snapshot
// name has one-second resolution, so anything above a second is correct; ten
// is far below any interval a person means, and short enough for the E2E to
// watch three runs (§20 erratum 321).
const BackupEveryFloor = 10 * time.Second

// BackupOn reports whether timed backups run: BACKUP_EVERY and BACKUP_DIR are
// both set. The loader refuses one without the other, so a loaded Config is
// either fully on or fully off.
func (c Config) BackupOn() bool { return c.BackupEvery > 0 && c.BackupDir != "" }

// DBSnapshotEveryFloor is the shortest DB_SNAPSHOT_EVERY the loader accepts
// (§20 erratum 343): short enough for a test stack to watch a debounce, long
// enough that no setting turns the bucket into a write per change.
const DBSnapshotEveryFloor = 5 * time.Second

// snapshotBlock reads the two snapshot variables (§20 errata 343, 348).
// DB_SNAPSHOT is honoured on both backends. On `fs` it was forced off until
// erratum 348, on the reasoning that the media folder sits beside the database
// — but the database lives in DATA_DIR (a Docker volume on the shipped stack),
// not in the folder people back up, so the default folder install had no
// backup of its database at all while everything suggested the folder was the
// whole instance. The copy under the folder's `_clippet/` closes that.
func (l *loader) snapshotBlock(c *Config) {
	c.DBSnapshot = l.oneOf("DB_SNAPSHOT", "on", "on", "off") == "on"
	c.DBSnapshotEvery = l.duration("DB_SNAPSHOT_EVERY", 10*time.Minute)
	if c.DBSnapshotEvery < DBSnapshotEveryFloor {
		l.fail("DB_SNAPSHOT_EVERY", "must be at least 5s, got %s", c.DBSnapshotEvery)
		c.DBSnapshotEvery = 10 * time.Minute
	}
}

// backupBlock reads the three timed-backup variables (§20 erratum 321).
//
// Both or neither: a half-configured backup that silently does nothing is the
// worst state a backup can be in, so one of the pair without the other is a
// boot error naming the missing one. BACKUP_KEEP is read ONLY when both are
// set — an operator who comments out the two lines to turn backups off must
// not get a boot error from the third.
//
// BACKUP_DIR's path rules run whenever it is set, the half-pair case included.
// It may not overlap SCRATCH_DIR (the janitor removes things there) or
// STORAGE_DIR (the drop watcher ingests files at its root). DATA_DIR equal to
// or around it is allowed: rotation only ever touches the names it mints, and
// `clippet.db` is not one.
func (l *loader) backupBlock(c *Config) {
	// Literal names, not constants: check.sh stage 03 greps l.<kind>("NAME"
	// to prove every variable read here reaches the container.
	everyRaw, dirRaw := l.raw("BACKUP_EVERY"), l.raw("BACKUP_DIR")
	if dirRaw != "" {
		c.BackupDir = l.backupDir("BACKUP_DIR", dirRaw, *c)
	}
	if everyRaw != "" {
		d := l.duration("BACKUP_EVERY", 0)
		if d > 0 && d < BackupEveryFloor {
			l.fail("BACKUP_EVERY", "must be at least 10s, got %s", d)
			d = 0
		}
		c.BackupEvery = d
	}
	switch {
	case everyRaw != "" && dirRaw == "":
		l.fail("BACKUP_EVERY", "is set and BACKUP_DIR is not: timed backups need both (DEPLOY.md §15.6)")
	case dirRaw != "" && everyRaw == "":
		l.fail("BACKUP_DIR", "is set and BACKUP_EVERY is not: timed backups need both (DEPLOY.md §15.6)")
	case everyRaw != "" && dirRaw != "":
		c.BackupKeep = l.intMin("BACKUP_KEEP", 30, 1)
	}
}

// backupDir validates BACKUP_DIR: absDir's shape, then the overlap rules with
// sentences of its own (storageDir's speak of the media store).
func (l *loader) backupDir(name, v string, c Config) string {
	clean, ok := l.absDir(name, v)
	if !ok {
		return ""
	}
	for _, other := range []struct{ name, dir string }{{"SCRATCH_DIR", c.ScratchDir}, {"STORAGE_DIR", c.StorageDir}} {
		if other.dir == "" {
			continue
		}
		od := dirForm(other.dir)
		switch {
		case sameDir(clean, od):
			l.fail(name, "is %s, which is also %s=%s: the snapshots need a directory of their own (DEPLOY.md §15.6)", clean, other.name, od)
			return ""
		case pathWithin(clean, od):
			l.fail(name, "is %s, which sits INSIDE %s=%s: put it beside that directory, not under it (DEPLOY.md §15.6)", clean, other.name, od)
			return ""
		case pathWithin(od, clean):
			l.fail(name, "is %s, which CONTAINS %s=%s: put it beside that directory, not around it (DEPLOY.md §15.6)", clean, other.name, od)
			return ""
		}
	}
	return clean
}

// discordWebhook validates DISCORD_WEBHOOK_URL (§17 #4). Empty = the feature
// is off, which is the default: nothing is posted to Discord.
//
// Three reasons the rules are this narrow, all of them worth the strictness:
//
//   - The §15 no-call-home guarantee has to stay AUDITABLE. "This variable can
//     only ever point at Discord" is a claim an operator can check; "this
//     variable points wherever it was pasted" is not. (The clip webhook,
//     erratum 319, is the one destination the operator names outright, in a
//     variable of its own, and that is exactly why it is not this one.)
//   - A typo must not leak share links. A webhook URL is pasted from a chat
//     client into a `.env`; a mistyped host would quietly start posting every
//     ready clip's link to somebody else's server, and nothing in the app
//     would look wrong.
//   - The token is IN THE PATH. Plain http would hand the webhook's
//     credential to every hop on the way, so https is not negotiable.
//
// Every rejection names the rule that failed and NEVER echoes the value —
// error strings reach logs, and this one is a secret.
func (l *loader) discordWebhook(name string) DiscordWebhook {
	v := l.raw(name)
	if v == "" {
		return DiscordWebhook{}
	}
	return l.parseDiscordURL(name, "", v)
}

// parseDiscordURL is the validation itself, shared by DISCORD_WEBHOOK_URL and
// by every entry of DISCORD_WEBHOOK_ROUTES so a routed webhook can never be
// held to a looser rule than the default one (§20 erratum 158). `where` names
// the entry inside a multi-value variable ("" for a single-value one) and is
// never the value: a tag is not a secret, a URL is.
func (l *loader) parseDiscordURL(name, where, v string) DiscordWebhook {
	fail := func(format string, args ...any) {
		l.fail(name, where+format, args...)
	}
	u, err := url.Parse(v)
	if err != nil {
		fail("is not a valid URL (the value is a secret and is not echoed here)")
		return DiscordWebhook{}
	}
	if u.Scheme != "https" {
		fail("scheme must be https (the webhook token is in the path, so plain http would leak it); got %q", u.Scheme)
		return DiscordWebhook{}
	}
	// ASCII case alone (§20 erratum 327): strings.ToLower is Unicode, and a
	// dotted capital I (U+0130) folds to an ASCII "i", so a lookalike host
	// would pass this lookup and then be dialled in its IDNA form — the trap
	// ValidAttachmentURL closes. A non-ASCII byte now fails the lookup.
	host := asciiLower(u.Host)
	if !discordHosts[host] {
		fail("host must be exactly one of %s (no port, no other host)", strings.Join(sortedKeys(discordHosts), ", "))
		return DiscordWebhook{}
	}
	if u.User != nil {
		fail("must not contain userinfo (user:password@)")
		return DiscordWebhook{}
	}
	// A trailing "?" parses as ForceQuery with an EMPTY RawQuery, and a
	// trailing "#" leaves Fragment empty too, so neither is caught by the two
	// parsed fields alone — and both survive into the raw value that
	// internal/notify appends "?wait=true" to, where they would rename that
	// parameter ("?wait") or swallow it into a fragment. The raw string is
	// therefore checked as well; a real webhook URL contains neither character.
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || strings.ContainsAny(v, "?#") {
		fail("must not carry a query string or fragment (not even a trailing '?' or '#')")
		return DiscordWebhook{}
	}
	id, ok := webhookID(u.Path)
	if !ok {
		fail("path must be %s{id}/{token} with a numeric id and a token of [A-Za-z0-9_.-]", discordPathPrefix)
		return DiscordWebhook{}
	}
	return DiscordWebhook{raw: v, host: host, id: id}
}

// routeSeparator and routeAssign spell DISCORD_WEBHOOK_ROUTES:
// `<tag>=<url>;<tag>=<url>`. A semicolon separates the rules because a comma
// is already TRUSTED_PROXIES' separator and a webhook URL's charset contains
// neither; '=' assigns because a tag's grammar cannot contain one, so the
// FIRST '=' is unambiguously the split point whatever the URL holds.
const (
	routeSeparator = ";"
	routeAssign    = "="
)

// maxRouteTagRunes mirrors the tag grammar of internal/db (`^[a-z0-9][a-z0-9-]{0,31}$`).
// The rule is restated here rather than imported so that internal/config
// depends on nothing but the standard library, which is what lets it stay the
// package every other one reads its rules from;
// TestCleanRouteTagIsTheTagGrammar pins the two against each other by calling
// db.CleanTag from the TEST binary, whose imports are not the package's.
const maxRouteTagRunes = 32

// reservedRouteTag is notify.DefaultRoute — the name DISCORD_WEBHOOK_URL's own
// channel carries in a boot line, in a delivery line and in a queued job's
// params_json. It is spelled out here for the same reason the tag grammar is:
// internal/config depends on nothing but the standard library. The two
// spellings are pinned against each other by TestReservedRouteTagIsNotifys in
// routes_test.go, which may import internal/notify because a test binary's
// imports are not the package's.
const reservedRouteTag = "default"

// discordRoutes parses DISCORD_WEBHOOK_ROUTES into ordered rules. Empty (the
// default) is no routing at all.
//
// Every URL goes through parseDiscordURL, so a routed webhook is held to the
// same https + host-allowlist + path-shape rule as DISCORD_WEBHOOK_URL and the
// §15 "S3 and Discord, nothing else" claim is unchanged by this feature. Every
// TAG goes through the tag grammar, because a rule whose tag no clip could
// ever carry is a rule that silently never fires — which is exactly the class
// of misconfiguration §4 says must fail at boot rather than at 3am.
//
// The ORDER is preserved and duplicates are refused: "first match wins" is
// only a rule an operator can reason about if the list they wrote is the list
// that is walked, and if a tag appears once.
//
// NOTHING a rejection says is taken from the value. A rule is located by its
// 1-based POSITION, never by echoing the text that failed: `strings.Cut`
// splits on the first `=`, so an operator who types the rule backwards
// (`<url>=<tag>`) hands the whole webhook — token included — to the "tag"
// half, and echoing it would print a live credential to stderr and into
// `docker compose logs app`, where it persists and is shipped wherever logs
// are shipped. Only a tag that has already PASSED the grammar (at most 32
// characters of a-z, 0-9 and -, so no URL) is ever named.
func (l *loader) discordRoutes(name string) []DiscordRoute {
	v := l.raw(name)
	if v == "" {
		return nil
	}
	var out []DiscordRoute
	seen := map[string]bool{}
	rule := 0
	for _, part := range strings.Split(v, routeSeparator) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue // a trailing ';' is a typing habit, not a mistake
		}
		rule++
		rawTag, rawURL, ok := strings.Cut(part, routeAssign)
		if !ok {
			l.fail(name, "rule %d must be <tag>%s<url>, with the rules separated by %q (the URLs are secrets and are not echoed here)",
				rule, routeAssign, routeSeparator)
			return nil
		}
		tag, tagErr := cleanRouteTag(rawTag)
		if tagErr != "" {
			l.fail(name, "rule %d: its tag %s (the rule is not echoed here — a rule typed <url>%s<tag> would print a webhook token)",
				rule, tagErr, routeAssign)
			return nil
		}
		if tag == reservedRouteTag {
			// `default` is the NAME internal/notify gives DISCORD_WEBHOOK_URL's
			// own channel, so a rule claiming it can never be honoured: the
			// notifier keeps the first endpoint registered under a name, and
			// that is always the default webhook. Left accepted, the boot log
			// would report the route as live while every clip carrying the tag
			// went to the default channel instead — the one outcome routing
			// exists to prevent — so it fails at boot rather than at 3am (§4).
			l.fail(name, "rule %d: %q is reserved — it is the name DISCORD_WEBHOOK_URL's own channel already has in the logs and in a queued job, so it cannot also be a route tag; rename the tag",
				rule, reservedRouteTag)
			return nil
		}
		if seen[tag] {
			l.fail(name, "tag %q appears twice; the first matching rule wins, so a repeated tag has no meaning", tag)
			return nil
		}
		seen[tag] = true
		w := l.parseDiscordURL(name, "the webhook for tag "+strconv.Quote(tag)+" ", strings.TrimSpace(rawURL))
		if !w.Configured() {
			return nil // parseDiscordURL already recorded the reason
		}
		out = append(out, DiscordRoute{Tag: tag, Webhook: w})
	}
	return out
}

// cleanRouteTag applies the tag grammar to a rule's tag and returns the
// normalised value, or a reason. Normalisation is db.CleanTag's: trim, then
// lower-case ASCII letters and NOTHING else — a Unicode fold would map
// characters that look nothing like a-z onto a-z, and an operator's rule must
// match the tag their users actually type.
func cleanRouteTag(s string) (string, string) {
	b := []byte(strings.TrimSpace(s))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	tag := string(b)
	if tag == "" {
		return "", "is empty"
	}
	if len([]rune(tag)) > maxRouteTagRunes {
		return "", "is longer than " + strconv.Itoa(maxRouteTagRunes) + " characters"
	}
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return "", "must be 1-" + strconv.Itoa(maxRouteTagRunes) +
				" characters of a-z, 0-9 and -, starting with a letter or a digit"
		}
	}
	return tag, ""
}

// webhookID checks the path shape and returns the (non-secret) numeric id.
// Exactly two segments follow the prefix: the id and the token. Anything
// longer is one of Discord's alternate endpoints (`/slack`, `/github`), which
// take a different body than the one §17 #4 sends.
//
// p is url.URL.Path — the DECODED path — so a token written `%20` on the way
// in is checked as the space it decodes to. The charset is Discord's own
// (URL-safe base64-ish); requiring it keeps the configured spelling and the
// spelling net/url puts on the wire identical, which is what lets
// internal/notify scrub the token out of an echoed error by a plain string
// match.
func webhookID(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, discordPathPrefix)
	if !ok {
		return "", false
	}
	id, token, ok := strings.Cut(rest, "/")
	if !ok || id == "" || !isWebhookToken(token) {
		return "", false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return "", false
		}
	}
	return id, true
}

// isWebhookToken reports whether s is a non-empty [A-Za-z0-9_.-] run — one
// path segment, nothing that percent-encodes, nothing that could carry a '/'.
func isWebhookToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// sortedKeys renders a set deterministically for an error message.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// keyPrefix normalizes S3_KEY_PREFIX: empty stays empty; otherwise a single
// trailing slash is guaranteed and the value is restricted to the key charset.
func (l *loader) keyPrefix(name string) string {
	v := strings.TrimLeft(l.raw(name), "/")
	if v == "" {
		return ""
	}
	v = strings.TrimRight(v, "/") + "/"
	// `.` is refused for the same reason `..` is, and it took ROADMAP #37 to
	// make it bite: on `fs` a key becomes a PATH, and store/fs.go's path law
	// refuses a `.` segment outright — so `S3_KEY_PREFIX=./` used to load
	// clean, pass `/readyz` (a ping takes no key) and then fail every upload,
	// list, stat and sweep with ErrBadKey. §4's rule is that a configuration
	// like that dies at boot, not at the first call.
	if strings.Contains(v, "..") || hasDotSegment(v) || !isKeyCharset(v) {
		l.fail(name, "must match [A-Za-z0-9_./-] and contain no '.' or '..' path segment, got %q", v)
		return ""
	}
	return v
}

// hasDotSegment reports whether any `/`-separated segment is exactly ".".
// (".." is caught by the substring test beside it, which is deliberately
// wider: `a..b` is refused too.)
func hasDotSegment(v string) bool {
	for _, seg := range strings.Split(v, "/") {
		if seg == "." {
			return true
		}
	}
	return false
}

func isKeyCharset(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '/', c == '-':
		default:
			return false
		}
	}
	return true
}

// ttl parses DEFAULT_CLIP_TTL: never | 0 | 1d | 7d | 30d | 90d | 1 | 7 | 30 | 90.
func (l *loader) ttl(name string) *int64 {
	v := strings.ToLower(l.raw(name))
	switch v {
	case "", "never", "0":
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
	if err != nil || !allowedTTLDays[n] {
		l.fail(name, "must be never, 0, 1d, 7d, 30d, 90d (or bare 1|7|30|90), got %q", v)
		return nil
	}
	if n == 0 {
		return nil
	}
	secs := int64(n) * 86400
	return &secs
}

// prefixes parses TRUSTED_PROXIES: comma-separated CIDRs; a bare IP is
// accepted as a single-address prefix. Empty → trust nothing.
func (l *loader) prefixes(name string) []netip.Prefix {
	v := l.raw(name)
	if v == "" {
		return nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			a, aerr := netip.ParseAddr(part)
			if aerr != nil {
				l.fail(name, "invalid CIDR %q", part)
				return nil
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		out = append(out, p.Masked())
	}
	return out
}
