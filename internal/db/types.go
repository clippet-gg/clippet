package db

import "database/sql"

// User is a row of users.
//
// The three Discord fields are the account's "Sign in with Discord" link
// (ROADMAP #44, §20 erratum 302; migration 0019): DiscordID is the Discord
// user's snowflake as text, nil when the account is not linked; the username
// is display only, refreshed at every Discord login; LinkedAt is when the link
// was made. They are read by GetUserBy* and left empty by GetSession's and
// GetTokenByHash's explicit joins — a request's Principal does not need them,
// and the one page that does (Settings) reads the row by id.
type User struct {
	ID, Username, PasswordHash string
	CreatedAt                  int64
	DiscordID                  *string
	DiscordUsername            string
	DiscordLinkedAt            *int64
}

// Session is a row of sessions (the cookie value is never stored; TokenHash
// is its hex sha256).
type Session struct {
	TokenHash, UserID                string
	CreatedAt, ExpiresAt, LastSeenAt int64
	// MaxExpiresAt is the ceiling the sliding window may never pass (§20
	// erratum 105), fixed at creation and never moved by a touch. 0 = no
	// ceiling recorded (a row an older binary wrote into a migrated file);
	// it is read as "unbounded", never as "already expired".
	MaxExpiresAt int64
	UserAgent    string
}

// APIToken is a row of api_tokens; Scopes is the comma-separated column split.
type APIToken struct {
	ID, UserID, Name, TokenHash, Prefix string
	Scopes                              []string
	CreatedAt                           int64
	LastUsedAt, RevokedAt               *int64
}

// HasScope reports whether the row carries scope. auth.Principal has the
// request-time answer (a session implicitly holds every scope); this is the
// ROW's own, for callers that hold a token and no request — the mint's `.sxcu`
// decision and the admin listing.
func (t APIToken) HasScope(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Clip is a row of clips. Pointer fields are NULL until finalize (or never).
//
// SocialWidth/SocialHeight (migration 0007) are the MEASURED dimensions of
// the social rendition: they are what `og:video:width` and the
// `twitter:player` dims state, because `/social.mp4` is the file a crawler
// fetches, and NULL — no social rendition — falls back to the main
// rendition's dims, which is exactly what that route then serves (§17 #3,
// §20 erratum 123).
type Clip struct {
	ID, OwnerID, Title, Description string
	Unlisted                        bool
	State                           string
	FailureReason                   *string
	SourceKey, SourceFilename       string
	SourceSize                      int64
	SourceContentType, SourceETag   string
	RenditionKey                    *string
	RenditionSize                   *int64
	RenditionETag                   *string
	PosterKey                       *string
	PosterSize                      *int64
	PosterETag                      *string
	PosterWidth, PosterHeight       *int
	PosterSeconds                   *float64 // the OWNER's chosen frame, in seconds on the ORIGINAL's timeline; nil = automatic (§20 erratum 127)
	SocialKey                       *string
	SocialSize                      *int64
	SocialETag                      *string
	SocialWidth, SocialHeight       *int
	SocialFPSNum, SocialFPSDen      *int // the LIGHT copy's MEASURED rate (migration 0012), for the quality selector's label
	GIFKey                          *string
	GIFSize                         *int64
	GIFETag                         *string
	GIFWidth, GIFHeight             *int
	// The trim scrubber's filmstrip (migration 0014, §20 erratum 220): one
	// JPEG sprite sheet of the CURRENT rendition, its tiles laid out in a
	// single row. The seven are NULL together — nobody has asked for a
	// filmstrip since the last cut — and every measurement comes from the
	// produced JPEG rather than from the plan, for poster_width/poster_height's
	// reason (erratum 92): a scrubber that placed tiles by a plan the encoder
	// did not follow would show the wrong frame under the handle.
	FilmstripKey                   *string
	FilmstripSize                  *int64
	FilmstripETag                  *string
	FilmstripTiles                 *int
	FilmstripTileW, FilmstripTileH *int
	FilmstripIntervalMS            *int64
	DurationMS                     *int64
	Width, Height, FPSNum, FPSDen  *int
	HasAudio                       *bool
	// SourceDurationMS is the ORIGINAL's container duration, and
	// TrimStartMS/TrimEndMS are the window of it the current rendition was cut
	// from (both NULL = the whole original). Together they are the trim
	// contract (§17 #5, §20 erratum 68): every window an owner sends is
	// measured on the original's timeline, which never moves, rather than on
	// the rendition's, which shrinks with every cut. SourceDurationMS is NULL
	// only while the original has not been measured yet — a row that predates
	// migration 0003 and has already been trimmed, until its next trim.
	SourceDurationMS       *int64
	TrimStartMS, TrimEndMS *int64
	Views                  int64
	TTLSeconds, ExpiresAt  *int64
	CreatedAt, UpdatedAt   int64
	ReadyAt                *int64

	// The quality tiers (migration 0012, §20 errata 200–211).
	//
	// OriginalPlayable is the probe's verdict on the UPLOAD: its container,
	// codec, pixel format, colour, field order, audio shape and pixel aspect
	// are all something a browser plays as they stand
	// (transcode.OriginalPlayability). OriginalRemuxed says the coded packets
	// were rewrapped into a faststart mp4 to be served — `-c copy`, not one
	// coded byte changed — and that rewrap is what `rendition_key` then points
	// at, so a remuxed clip has NO Full tier.
	//
	// OriginalWidth/Height/FPS/Codec describe the upload itself and never the
	// rendition. They are what the quality selector labels the Original tier
	// with, and — because a Full is encoded at the source's own size — what it
	// labels a Full that has not finished yet with. All four are NULL/"" for a
	// row nothing has probed since 0012.
	OriginalPlayable, OriginalRemuxed bool
	OriginalWidth, OriginalHeight     *int
	OriginalFPSNum, OriginalFPSDen    *int
	OriginalCodec                     string

	// FullState and LightState are TierState* ("" = unknown, TierSkipped,
	// TierEncoding, TierReady, TierFailed) and FullCodec is which encoder
	// produced the Full. The Full tier's OBJECT is rendition_key — there is no
	// second key for it, deliberately (migration 0012's header) — so these two
	// words are the whole of what the row has to add.
	FullState, FullCodec string
	LightState           string

	// Kind is ClipKindVideo or ClipKindAudio (migration 0021, §20 erratum 328), and the
	// four after it describe an audio clip's one stream as it was stored: the
	// codec, the sample rate in Hz, the bit depth (nil for a lossy codec) and
	// the channel count. All four are nil on a video row.
	Kind          string
	AudioCodec    string
	SampleRate    *int
	BitDepth      *int
	AudioChannels *int

	// Peaks is an audio clip's waveform, transcode.EncodePeaks' hex (migration
	// 0022, §20 erratum 329); "" on a video and on any row without one.
	Peaks string
}

// Clip kinds as stored in clips.kind (migration 0021).
const (
	ClipKindVideo = "video"
	ClipKindAudio = "audio"
)

// IsAudio reports whether the clip is sound (§20 erratum 328). An empty kind —
// a Clip value built by hand in a test — is a video, as every row before 0021.
func (c Clip) IsAudio() bool { return c.Kind == ClipKindAudio }

// Tier state values, as stored in clips.full_state / clips.light_state and
// reported by ClipDTO.tiers and GET …/progress. They mirror
// transcode.TierState* and are spelled here so internal/db imports nothing of
// internal/transcode (the dependency runs the other way).
const (
	// TierUnknown is the zero value: a row written before migration 0012, or a
	// clip whose tiers have not been decided yet (queued, processing, failed).
	// It is never shown to anybody as a state — the DTO derives what it can
	// from the keys instead.
	TierUnknown  = ""
	TierSkipped  = "skipped"
	TierEncoding = "encoding"
	TierReady    = "ready"
	TierFailed   = "failed"
	// TierPending is the word for "nobody has decided this tier yet", and it is
	// the one state that is NEVER stored: it is derived, for a tier with no
	// object and no stored word on a clip that is still queued or processing
	// (ReconcileClipTier). It exists because `skipped` means "not needed — a
	// finished answer, not a gap" everywhere else in this API, and answering
	// that for a Full a clip may be about to get is how a client is told to stop
	// waiting for a tier that is coming (§20 erratum 205, amended).
	TierPending = "pending"
)

// Job is a row of jobs (one row per attempt series).
type Job struct {
	ID                          int64
	ClipID, Kind, State, Stage  string
	Attempts, TransientFailures int
	RunAfter                    int64
	ParamsJSON                  string
	CheckpointJSON              *string
	LastError                   *string
	CreatedAt, UpdatedAt        int64
	StartedAt, FinishedAt       *int64
}

// Clip/job states and job kinds as stored.
const (
	ClipQueued     = "queued"
	ClipProcessing = "processing"
	ClipReady      = "ready"
	ClipFailed     = "failed"
	ClipDeleting   = "deleting"

	JobQueued    = "queued"
	JobRunning   = "running"
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"

	// JobStageQueued is the stage of a job that is waiting to be claimed
	// (fresh, retried, requeued or released); the worker overwrites it at
	// its first stage transition. Head of the §8.4 vocabulary.
	JobStageQueued = "queued"

	// KindTranscode is the ONLY kind coupled to clips.state: ClaimJob,
	// FailAttempt and ReleaseJob move the clip alongside the job for it and
	// for nothing else (§5).
	KindTranscode = "transcode"
	// KindNotify posts a ready clip to the operator's Discord webhook (§17
	// #4). It runs against a clip that is already `ready` and leaves it
	// `ready`: a notify job never writes clips.state, clips.failure_reason
	// or the clip's failure counters, whatever happens to the delivery.
	KindNotify = "notify"
	// KindTrim re-derives the rendition from the retained original over a
	// sub-window of it (§17 #5). Like notify it runs against a `ready` clip
	// and leaves it `ready` — the clip stays playable on its old rendition
	// for the whole run and is swapped onto the new one by a single UPDATE
	// (SwapRendition) or not at all.
	KindTrim = "trim"
	// KindGIF renders the bounded preview GIF of a ready clip's CURRENT
	// rendition (§17 #6). Same rule again: it runs against a `ready` clip,
	// leaves it `ready`, and commits with one guarded UPDATE (SetGIF) that
	// writes only the gif_* columns.
	KindGIF = "gif"
	// KindSocial renders the dedicated social rendition of a ready clip's
	// CURRENT rendition (§17 #3). The same rule once more: it runs against a
	// `ready` clip, leaves it `ready`, and commits with one guarded UPDATE
	// (SetSocial) that writes only the social_* columns — so a social
	// rendition that never arrives costs the clip nothing at all, and
	// `/social.mp4` goes on serving the main rendition.
	KindSocial = "social"
	// KindReplace re-derives EVERYTHING from a NEW original the owner
	// uploaded under the same clip id (ROADMAP #31, §20 erratum 135). It is
	// the media loop's fourth kind and follows the same rule as the rest of
	// them: it runs against a `ready` clip, leaves it `ready` — a viewer
	// never sees the clip leave that state, so the share page and the card
	// keep working throughout — and commits with ONE guarded UPDATE
	// (SwapSource) that swaps the source, the rendition and the poster
	// together. It is the only kind that writes clips.source_*.
	KindReplace = "replace"
	// KindFull encodes the FULL tier of a clip whose original the browser
	// cannot play (§20 errata 200–202). It is the media loop's SEVENTH kind and
	// follows the same rule as the rest of them: it runs against a clip that is
	// already `ready` — ready on its Light copy, which the transcode run made
	// first so the share card exists as early as possible — leaves it `ready`,
	// and commits with ONE guarded UPDATE (SetFull) that points
	// `rendition_key` at the faithful copy. A Full that never arrives costs the
	// clip nothing except quality: the Light copy goes on playing and the
	// selector says the better tier failed.
	KindFull = "full"
	// KindPoster re-takes a ready clip's card image from ONE frame of its
	// current rendition — the frame the owner chose (ROADMAP #5, §20 errata
	// 127-130). The media rule once more: it runs against a `ready` clip,
	// leaves it `ready`, and commits with one guarded UPDATE (SetPoster)
	// that writes only the poster columns.
	KindPoster = "poster"
	// KindFilmstrip renders the trim scrubber's thumbnail sheet from a ready
	// clip's CURRENT rendition (§20 erratum 220). The media rule once more: it
	// runs against a `ready` clip, leaves it `ready`, and commits with one
	// guarded UPDATE (SetFilmstrip) that writes only the filmstrip_* columns —
	// so a filmstrip that never arrives costs the clip nothing at all and the
	// scrubber simply keeps its plain track.
	KindFilmstrip = "filmstrip"
	// KindHook POSTs a ready clip to the operator's clip webhook (ROADMAP #54,
	// §20 erratum 319). It is notify's twin and follows notify's rule: it runs
	// against a clip that is already `ready`, leaves it `ready`, and never
	// writes clips.state, clips.failure_reason or the clip's failure counters
	// whatever happens to the delivery. Its params_json is `{}`: the row is a
	// pointer to the clip, and the URL and the key live only in the process.
	KindHook = "hook"

	// JobStageDelivered / JobStageSkipped are the terminal stages of a
	// notify job (CompleteJob): delivered, or resolved without a post.
	JobStageDelivered = "delivered"
	JobStageSkipped   = "skipped"

	// MsgInterruptedByRestart is what ReconcileOnStartup writes into
	// jobs.last_error for a running job the process did not survive. It is
	// the OPERATOR's sentence and the only text this package ever puts in
	// that column: every other value is written by internal/worker, which
	// words each media kind's endings from a fixed vocabulary (§20 erratum
	// 80). It is exported so the routes that show last_error to a person who
	// is not an operator can recognise it (§20 erratum 165) — never so
	// anybody else may write it.
	MsgInterruptedByRestart = "interrupted by restart"
)

// clipCols is the SELECT list every Clip scan uses (order matters).
const clipCols = `id, owner_id, title, description, unlisted, state, failure_reason,
source_key, source_filename, source_size, source_content_type, source_etag,
rendition_key, rendition_size, rendition_etag,
poster_key, poster_size, poster_etag, poster_width, poster_height, poster_seconds,
social_key, social_size, social_etag, social_width, social_height, social_fps_num, social_fps_den,
gif_key, gif_size, gif_etag, gif_width, gif_height,
filmstrip_key, filmstrip_size, filmstrip_etag, filmstrip_tiles, filmstrip_tile_w, filmstrip_tile_h, filmstrip_interval_ms,
duration_ms, width, height, fps_num, fps_den, has_audio,
source_duration_ms, trim_start_ms, trim_end_ms,
views, ttl_seconds, expires_at, created_at, updated_at, ready_at,
original_playable, original_remuxed, original_width, original_height,
original_fps_num, original_fps_den, original_codec,
full_state, full_codec, light_state,
kind, audio_codec, sample_rate, bit_depth, channels, peaks`

// jobCols is the SELECT list every Job scan uses (order matters).
const jobCols = `id, clip_id, kind, state, stage, attempts, transient_failures, run_after,
params_json, checkpoint_json, last_error, created_at, updated_at, started_at, finished_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanClip(s scanner) (Clip, error) {
	var (
		c                                     Clip
		unlisted                              int64
		failureReason                         sql.NullString
		rKey, rETag, pKey, pETag, sKey, sETag sql.NullString
		rSize, pSize, pW, pH, sSize           sql.NullInt64
		sW, sH, sFPSN, sFPSD                  sql.NullInt64
		posterSec                             sql.NullFloat64
		gKey, gETag                           sql.NullString
		gSize, gW, gH                         sql.NullInt64
		fKey, fETag                           sql.NullString
		fSize, fTiles, fTileW, fTileH, fIvl   sql.NullInt64
		durMS, w, h, fpsN, fpsD, hasAudio     sql.NullInt64
		srcDurMS, trimStart, trimEnd          sql.NullInt64
		ttl, expires, readyAt                 sql.NullInt64
		origPlayable, origRemuxed             int64
		origW, origH, origFPSN, origFPSD      sql.NullInt64
		audioCodec, peaks                     sql.NullString
		sampleRate, bitDepth, channels        sql.NullInt64
	)
	err := s.Scan(
		&c.ID, &c.OwnerID, &c.Title, &c.Description, &unlisted, &c.State, &failureReason,
		&c.SourceKey, &c.SourceFilename, &c.SourceSize, &c.SourceContentType, &c.SourceETag,
		&rKey, &rSize, &rETag,
		&pKey, &pSize, &pETag, &pW, &pH, &posterSec,
		&sKey, &sSize, &sETag, &sW, &sH, &sFPSN, &sFPSD,
		&gKey, &gSize, &gETag, &gW, &gH,
		&fKey, &fSize, &fETag, &fTiles, &fTileW, &fTileH, &fIvl,
		&durMS, &w, &h, &fpsN, &fpsD, &hasAudio,
		&srcDurMS, &trimStart, &trimEnd,
		&c.Views, &ttl, &expires, &c.CreatedAt, &c.UpdatedAt, &readyAt,
		&origPlayable, &origRemuxed, &origW, &origH,
		&origFPSN, &origFPSD, &c.OriginalCodec,
		&c.FullState, &c.FullCodec, &c.LightState,
		&c.Kind, &audioCodec, &sampleRate, &bitDepth, &channels, &peaks,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return Clip{}, ErrNotFound
		}
		return Clip{}, err
	}
	c.Unlisted = unlisted != 0
	c.FailureReason = ptrStr(failureReason)
	c.RenditionKey, c.RenditionSize, c.RenditionETag = ptrStr(rKey), ptrInt64(rSize), ptrStr(rETag)
	c.PosterKey, c.PosterSize, c.PosterETag = ptrStr(pKey), ptrInt64(pSize), ptrStr(pETag)
	c.PosterWidth, c.PosterHeight = ptrInt(pW), ptrInt(pH)
	c.PosterSeconds = ptrFloat64(posterSec)
	c.SocialKey, c.SocialSize, c.SocialETag = ptrStr(sKey), ptrInt64(sSize), ptrStr(sETag)
	c.SocialWidth, c.SocialHeight = ptrInt(sW), ptrInt(sH)
	c.SocialFPSNum, c.SocialFPSDen = ptrInt(sFPSN), ptrInt(sFPSD)
	c.OriginalPlayable, c.OriginalRemuxed = origPlayable != 0, origRemuxed != 0
	c.OriginalWidth, c.OriginalHeight = ptrInt(origW), ptrInt(origH)
	c.OriginalFPSNum, c.OriginalFPSDen = ptrInt(origFPSN), ptrInt(origFPSD)
	c.GIFKey, c.GIFSize, c.GIFETag = ptrStr(gKey), ptrInt64(gSize), ptrStr(gETag)
	c.GIFWidth, c.GIFHeight = ptrInt(gW), ptrInt(gH)
	c.FilmstripKey, c.FilmstripSize, c.FilmstripETag = ptrStr(fKey), ptrInt64(fSize), ptrStr(fETag)
	c.FilmstripTiles, c.FilmstripTileW, c.FilmstripTileH = ptrInt(fTiles), ptrInt(fTileW), ptrInt(fTileH)
	c.FilmstripIntervalMS = ptrInt64(fIvl)
	c.DurationMS = ptrInt64(durMS)
	c.Width, c.Height, c.FPSNum, c.FPSDen = ptrInt(w), ptrInt(h), ptrInt(fpsN), ptrInt(fpsD)
	c.HasAudio = ptrBool(hasAudio)
	c.SourceDurationMS = ptrInt64(srcDurMS)
	c.TrimStartMS, c.TrimEndMS = ptrInt64(trimStart), ptrInt64(trimEnd)
	c.TTLSeconds, c.ExpiresAt, c.ReadyAt = ptrInt64(ttl), ptrInt64(expires), ptrInt64(readyAt)
	c.AudioCodec, c.Peaks = audioCodec.String, peaks.String
	c.SampleRate, c.BitDepth, c.AudioChannels = ptrInt(sampleRate), ptrInt(bitDepth), ptrInt(channels)
	return c, nil
}

func scanJob(s scanner) (Job, error) {
	var (
		j                     Job
		checkpoint, lastError sql.NullString
		startedAt, finishedAt sql.NullInt64
	)
	err := s.Scan(
		&j.ID, &j.ClipID, &j.Kind, &j.State, &j.Stage, &j.Attempts, &j.TransientFailures, &j.RunAfter,
		&j.ParamsJSON, &checkpoint, &lastError, &j.CreatedAt, &j.UpdatedAt, &startedAt, &finishedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return Job{}, ErrNotFound
		}
		return Job{}, err
	}
	j.CheckpointJSON, j.LastError = ptrStr(checkpoint), ptrStr(lastError)
	j.StartedAt, j.FinishedAt = ptrInt64(startedAt), ptrInt64(finishedAt)
	return j, nil
}

func scanUser(s scanner) (User, error) {
	var u User
	var discordID sql.NullString
	var linkedAt sql.NullInt64
	if err := s.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt, &discordID, &u.DiscordUsername, &linkedAt); err != nil {
		if err == sql.ErrNoRows {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	if discordID.Valid {
		id := discordID.String
		u.DiscordID = &id
	}
	u.DiscordLinkedAt = ptrInt64(linkedAt)
	return u, nil
}
