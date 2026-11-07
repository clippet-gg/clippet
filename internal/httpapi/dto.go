package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/urls"
)

// ClipDTO is the §8.2 wire shape. Views is set only for the owner and the
// key is omitted for everyone else.
type ClipDTO struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Description   string  `json:"description"`
	Unlisted      bool    `json:"unlisted"`
	State         string  `json:"state"`
	FailureReason *string `json:"failure_reason"`
	Views         *int64  `json:"views,omitempty"`
	// LinkStyle is which grammar `id` is in — `compact` or `memorable` (§20
	// errata 249, 251, 264). It is DERIVED from the id (ids.Kind) and stored
	// nowhere: the id IS the fact, and a column that could disagree with it
	// would be a second, worse one. Always present, never null, and not
	// owner-private — anyone holding the link can read the id it describes.
	// It is a WIRE fact and no page spends it: erratum 251's one owner-only
	// line was withdrawn by 264, and the field stays because the shape of an id
	// is something a client (a capture tool, a backup script) may want named
	// rather than re-derived, and it is free to send.
	LinkStyle string `json:"link_style"`
	// Kind is `video` or `audio` (§20 erratum 328), always present. Audio is the
	// audio clip's stream — codec, sample rate, bit depth (null for a lossy
	// codec), channels — and absent on a video.
	Kind  string    `json:"kind"`
	Audio *AudioDTO `json:"audio,omitempty"`
	// SourceDurationMS and Trim are the trim contract, and they are OWNER-ONLY
	// for the same reason `views` is: together they say "this clip is a cut of
	// something longer", which is the owner's editing history and nobody
	// else's. Both are ABSENT (never null) for every other caller — and, like
	// `views`, absent rather than null is also how "there is nothing to say"
	// is spelled: `trim` is missing on a clip that is the whole original, and
	// `source_duration_ms` is missing on a row from before migration 0003 that
	// has already been trimmed, which is exactly the clip whose windows the
	// API stops bounding and the worker's probe decides (§20 erratum 68).
	SourceDurationMS *int64        `json:"source_duration_ms,omitempty"`
	Trim             *TrimStateDTO `json:"trim,omitempty"`
	// Tags is the clip's tag set, sorted, ALWAYS present and never null: `[]`
	// is how "no tags" is spelled, so a client can render the list without a
	// null check and a round-trip of GET into PATCH keeps meaning what it said
	// (§20 erratum 81). Unlike `views`, tags are not owner-private — they are
	// part of what the clip IS, and this DTO is already owner-or-link gated.
	Tags       []string   `json:"tags"`
	CreatedAt  string     `json:"created_at"`
	UpdatedAt  string     `json:"updated_at"`
	ReadyAt    *string    `json:"ready_at"`
	TTLDays    int        `json:"ttl_days"`
	ExpiresAt  *string    `json:"expires_at"`
	DurationMS *int64     `json:"duration_ms"`
	Width      *int       `json:"width"`
	Height     *int       `json:"height"`
	FPS        *string    `json:"fps"`
	HasAudio   *bool      `json:"has_audio"`
	Poster     *PosterDTO `json:"poster"`
	// GIFURL is the preview GIF's URL once one exists, and null until then
	// (§17 #6). It is a plain nullable string rather than a member of `urls`
	// on purpose: everything in `urls` is an address that always answers,
	// while this one is a FACT — "this clip has a preview" — and a client
	// that wants to make one asks the route, which is what null means.
	GIFURL *string `json:"gif_url"`
	// Existing is true on POST /api/clips' duplicate answer only (§20 erratum
	// 326): the account already held a live clip of exactly these bytes, this is
	// that clip, and nothing was created. Absent on every other answer.
	Existing bool `json:"existing,omitempty"`
	// Filmstrip is the trim scrubber's sprite sheet ONCE ONE EXISTS, and null
	// until then (§20 erratum 220). It is an object rather than a URL for the
	// reason GIFURL is a URL rather than an entry in `urls`, one step further
	// on: the address alone is useless to a client, which cannot place a single
	// tile without knowing how many there are, how wide one is and how much clip
	// time it covers. So the fact and the geometry travel together, and null is
	// how "no sheet for this rendition — ask the route and it will make one" is
	// spelled. It is NOT owner-private: the route it describes is public like
	// `poster.jpg`, and thumbnails of a clip the caller may watch frame by frame
	// say nothing the video does not.
	Filmstrip *FilmstripDTO `json:"filmstrip"`
	Source    SourceDTO     `json:"source"`
	Owner     OwnerDTO      `json:"owner"`
	URLs      URLsDTO       `json:"urls"`
	// Tiers is every quality tier this clip has or could have, best first. It is
	// ALWAYS present, never null and always the same three entries, so a client
	// can render the list without a null check or a length check (`tags`' rule,
	// erratum 81). On a clip that is not ready yet the entries carry no `url` and
	// the two derived tiers read `pending` — nothing has been decided (§20
	// erratum 205, amended). CardTier names the one a crawler ends up fetching,
	// and is empty until there is one.
	//
	// It is what closes N079 ("no DTO says whether a clip has a social file"):
	// the answer is a `light` entry whose state is `ready`, and the same list
	// says whether a better copy exists, is encoding, failed, was never needed,
	// or has yet to be decided (§20 erratum 205).
	Tiers    []TierDTO `json:"tiers"`
	CardTier string    `json:"card_tier"`
}

// TierDTO is one quality tier (§20 erratum 205).
//
// Every field except `tier`, `state` and `playable` is nullable, and each null
// means the same thing: nobody has measured that yet. `url` is null for a tier
// that has no file — one that is encoding, failed, skipped or still pending — so
// a client never has an address that answers 404 or 409. `bytes` is null for a
// tier whose object this DTO cannot size, which today is only the Original of a
// legacy row.
//
// `playable` says whether a BROWSER plays this tier's file. It is true for every
// Full and every Light, by construction, and it is the probe's verdict for the
// Original — false there means the upload is a download and not a stream (HEVC,
// ProRes, a 10-bit capture), which is exactly when a Full exists beside it.
// AudioDTO is an audio clip's one stream as it is stored (§20 erratum 328).
type AudioDTO struct {
	Codec      string `json:"codec"`
	SampleRate *int   `json:"sample_rate"`
	BitDepth   *int   `json:"bit_depth"`
	Channels   *int   `json:"channels"`
}

// kindOf is the clip's kind on the wire; a row before migration 0021 is a video.
func kindOf(c db.Clip) string {
	if c.IsAudio() {
		return db.ClipKindAudio
	}
	return db.ClipKindVideo
}

// audioDTOOf is the audio block, nil for a video.
func audioDTOOf(c db.Clip) *AudioDTO {
	if !c.IsAudio() {
		return nil
	}
	return &AudioDTO{Codec: c.AudioCodec, SampleRate: c.SampleRate, BitDepth: c.BitDepth, Channels: c.AudioChannels}
}

type TierDTO struct {
	Tier     string  `json:"tier"`
	URL      *string `json:"url"`
	Width    *int    `json:"width"`
	Height   *int    `json:"height"`
	FPS      *string `json:"fps"`
	Bytes    *int64  `json:"bytes"`
	Codec    string  `json:"codec"`
	Playable bool    `json:"playable"`
	State    string  `json:"state"`
	// Label is the quality control's own words for an AUDIO tier (§20 erratum
	// 328) — `Full · FLAC 24-bit` — which no width, height or rate could spell.
	// Absent on every video tier, whose label the client builds from the numbers.
	Label string `json:"label,omitempty"`
}

// TrimStateDTO is the window of the ORIGINAL the clip's current rendition was
// cut from (§20 erratum 68). It is what a second trim measures itself against
// and what the share page pre-fills its inputs with.
type TrimStateDTO struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
}

// FilmstripDTO is the trim scrubber's sheet: where it is and how to read it.
// Every number is MEASURED from the produced JPEG (§20 erratum 220), so a client
// may compute a tile's offset as `-(floor(ms / interval_ms) * tile_width)` and
// trust that the frame it lands on is the frame the player would seek to.
//
// `tiles` is the count in the sheet, laid out in ONE row; `interval_ms` is what
// one tile covers on the RENDITION's timeline — the same timeline the player's
// clock runs on, and the reason a caller holding a moment on the ORIGINAL's
// timeline subtracts the applied cut's start first (erratum 68's coordinate
// system, subtracted exactly as the poster's chosen frame is).
type FilmstripDTO struct {
	URL        string `json:"url"`
	Tiles      int    `json:"tiles"`
	TileWidth  int    `json:"tile_width"`
	TileHeight int    `json:"tile_height"`
	IntervalMS int64  `json:"interval_ms"`
}

// PosterDTO carries the poster's measured dimensions.
type PosterDTO struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// SourceDTO describes the original upload.
type SourceDTO struct {
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// OwnerDTO is the public owner reference.
type OwnerDTO struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// URLsDTO holds the absolute routes built from PUBLIC_BASE_URL.
//
// `social` is the LIGHT tier's original name and stays an ALIAS of `light`
// (§20 erratum 210): it is what every card tag points at and what every client
// written before the tiers reads, and both keys carry the same URL's object.
// `video` is "the best playable tier", which is what that route has always
// served and is now sometimes the upload itself.
type URLsDTO struct {
	Page     string `json:"page"`
	Embed    string `json:"embed"`
	Video    string `json:"video"`
	Social   string `json:"social"`
	Light    string `json:"light"`
	Original string `json:"original"`
	Poster   string `json:"poster"`
	Progress string `json:"progress"`
}

// UploadDTO is the §8.1 answer of POST /api/upload: exactly what a capture
// tool needs and nothing more. `url` is the share page — the link ShareX
// pastes, and the one that unfurls as a player card.
type UploadDTO struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	EmbedURL string `json:"embed_url"`
	// Kind is `video` or `audio` (§20 erratum 328).
	Kind string `json:"kind"`
	// State is the CLIP's state — `ready`, `queued`, `processing` or `failed`.
	// It says nothing about the card, and §20 erratum 212 did not change it.
	State string `json:"state"`
	// CardReady says whether the link in `url` unfurls as a PLAYER card right
	// now (db.CardReady, §20 erratum 212). The reply is HELD until it is true,
	// for up to CAPTURE_READY_WAIT; at the ceiling the link is returned anyway
	// with this false, because a capture tool cannot be held for minutes. There
	// is deliberately no estimate of how much longer it will be.
	CardReady bool    `json:"card_ready"`
	ExpiresAt *string `json:"expires_at"`
	// Tiers is the clip's quality tiers, exactly as the ClipDTO renders them
	// (§20 errata 205, 212): always three entries, best first, never null. A
	// capture tool that wants to say "1440p60, card at 720p30" has the numbers
	// without a second request.
	Tiers []TierDTO `json:"tiers"`
	// Existing is true on the duplicate answer only (§20 erratum 326): this
	// account already held a live clip of exactly these bytes, this is that
	// clip, and nothing was created. Absent on every other answer.
	Existing bool `json:"existing,omitempty"`
}

// CollectionDTO is the §8.2 wire shape of a collection (§17 #9). It carries
// no owner block and no counter beyond `clip_count`: a collection is an
// ordered list of links, and the page it describes shows no view counts, no
// author and no engagement of any kind.
//
// `clips` is populated only by GET /api/collections/{cid} — the list route
// answers with the shells, because a library of 500 collections would
// otherwise render every clip in every one of them.
//
// It is a POINTER to a slice, and that is the whole reason (§20 erratum 82,
// amended). The field carries three states, not two: "the members, in order"
// on the single route that renders them, "this collection has NO members" on the
// same route, and "this body is a shell, ask the other route" everywhere
// else. A plain `[]ClipDTO` with `omitempty` can only spell two of them — an
// empty collection would omit `clips` exactly as a shell does, so a client
// reading GET /api/collections/{cid} could not tell "empty" from "not
// answered here" and §8.1's "with clips" would be a lie for the one
// collection that most needs the key. With a pointer the empty case is a
// non-nil pointer to an empty slice → `"clips":[]`, which is how empty is
// spelled everywhere else in this API (`tags`, the list routes), and the
// shells leave it nil → omitted.
type CollectionDTO struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	URL       string     `json:"url"`
	ClipCount int        `json:"clip_count"`
	Clips     *[]ClipDTO `json:"clips,omitempty"`
	CreatedAt string     `json:"created_at"`
	UpdatedAt string     `json:"updated_at"`
}

// collectionList is the body of GET /api/collections.
type collectionList struct {
	Collections []CollectionDTO `json:"collections"`
}

// toCollectionDTO renders the shell (no clips).
func toCollectionDTO(c db.Collection, u urls.Builder) CollectionDTO {
	return CollectionDTO{
		ID: c.ID, Title: c.Title, URL: u.Collection(c.ID), ClipCount: c.ClipCount,
		CreatedAt: rfc3339(c.CreatedAt), UpdatedAt: rfc3339(c.UpdatedAt),
	}
}

// UserDTO is the §8.2 user shape.
type UserDTO struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	CreatedAt string `json:"created_at"`
}

// TokenDTO is one row of GET /api/auth/tokens; the raw token is never here.
type TokenDTO struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at"`
}

// SessionDTO is one row of GET /api/auth/sessions (§20 erratum 107).
//
// There is deliberately NO id and no handle of any kind. A session's only
// identity is the hex sha256 of its cookie — that IS the security of the
// scheme — and a per-session revoke would need a name for it that a page,
// and therefore an attacker's page, could then send. The affordance the
// incident actually needs is "end every session but this one", which needs no
// id at all; so the list is a statement of fact and `current` is the only
// thing a reader has to match against the browser in front of them.
//
// `user_agent` is the string that browser sent, stored capped at 256 bytes
// and rendered as text; it is the caller's own data and reaches nobody else.
type SessionDTO struct {
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at"`
	ExpiresAt  string `json:"expires_at"`
	Current    bool   `json:"current"`
	UserAgent  string `json:"user_agent"`
}

type sessionList struct {
	Sessions []SessionDTO `json:"sessions"`
}

func toSessionDTO(s db.Session, current bool) SessionDTO {
	return SessionDTO{
		CreatedAt:  rfc3339(s.CreatedAt),
		LastSeenAt: rfc3339(s.LastSeenAt),
		ExpiresAt:  rfc3339(s.ExpiresAt),
		Current:    current,
		UserAgent:  s.UserAgent,
	}
}

// TrimStartDTO is the 202 of POST /api/clips/{id}/trim: the job that was
// queued, and nothing else — every fact about the clip is still true, and
// the caller polls GET …/trim for the rest (§17 #5).
type TrimStartDTO struct {
	JobID int64  `json:"job_id"`
	State string `json:"state"`
}

// TrimStatusDTO is the 200 of GET /api/clips/{id}/trim: the clip's latest
// trim job. `state` is the JOB's (queued|running|done|failed|cancelled),
// never the clip's — the clip is `ready` for the whole of it. `cancelled` is
// the terminal a poller must also stop on: it is what the worker records when
// the clip stopped being ready under a running trim (§11's lost race).
//
// `start_ms`/`end_ms` are what was ASKED for; `applied_start_ms`/
// `applied_end_ms` are what the run actually cut, and appear only once it is
// done. They differ by design — a stream copy starts at the keyframe it can
// start at, and a window running past the video stream is clamped to it — and
// both are on the ORIGINAL's timeline (§20 erratum 68).
//
// A `done` job can report `applied_start_ms`/`applied_end_ms: null` too: a
// later replacement retires `trim_start_ms`/`trim_end_ms` outright
// (`db.SwapSource`), the same way it retires a chosen poster frame (N110,
// roadmap-run nit sweep — see PosterStatusDTO). `start_ms`/`end_ms` keep
// naming the window that was asked for on a clip's retired original.
type TrimStatusDTO struct {
	JobID          int64   `json:"job_id"`
	State          string  `json:"state"`
	Stage          string  `json:"stage"`
	Percent        int     `json:"percent"`
	Error          *string `json:"error"`
	StartMS        int64   `json:"start_ms"`
	EndMS          int64   `json:"end_ms"`
	AppliedStartMS *int64  `json:"applied_start_ms"`
	AppliedEndMS   *int64  `json:"applied_end_ms"`
}

// ProgressDTO is the §8.4 polling shape.
//
// `tiers` is the quality tiers' per-tier state (§20 erratum 205), and it is what
// lets the watch page of a READY clip keep polling for a better copy: `ready`
// with `full: encoding` is the ordinary shape of a clip somebody can already
// watch while its faithful tier finishes. BOTH progress routes carry it (the
// batched one since erratum 205), and it is `[]`, never null and never absent,
// so a poller needs no null check — which is why the field has no `omitempty`:
// a tag that drops an empty list would be a third shape to handle, and the
// promise here is that there are only two.
type ProgressDTO struct {
	ID            string            `json:"id"`
	State         string            `json:"state"`
	Stage         string            `json:"stage"`
	Percent       int               `json:"percent"`
	Indeterminate bool              `json:"indeterminate"`
	Error         *string           `json:"error"`
	UpdatedAt     string            `json:"updated_at"`
	Tiers         []ProgressTierDTO `json:"tiers"`
}

// ProgressTierDTO is one tier's state on the polling route: the tier's name and
// one of ready | encoding | failed | skipped | pending (§20 erratum 205,
// amended: `pending` is a clip whose tiers nobody has decided yet, which is
// exactly the clip a poller is watching). It carries no numbers on purpose —
// a poller that wants the dimensions and the URLs asks GET /api/clips/{id},
// which is the one place a tier is fully described.
type ProgressTierDTO struct {
	Tier  string `json:"tier"`
	State string `json:"state"`
}

const secondsPerDay = 86400

// ToDTO renders a clip for the wire; isOwner=false → Views nil → key absent.
// tags is the clip's tag set (nil renders as `[]`, never as null).
func ToDTO(c db.Clip, owner db.User, u urls.Builder, isOwner bool, tags []string) ClipDTO {
	if tags == nil {
		tags = []string{}
	}
	dto := ClipDTO{
		ID:            c.ID,
		Title:         c.Title,
		Description:   c.Description,
		Unlisted:      c.Unlisted,
		State:         c.State,
		LinkStyle:     linkStyleOf(c.ID),
		Kind:          kindOf(c),
		Audio:         audioDTOOf(c),
		FailureReason: c.FailureReason,
		Tags:          tags,
		CreatedAt:     rfc3339(c.CreatedAt),
		UpdatedAt:     rfc3339(c.UpdatedAt),
		ReadyAt:       rfc3339Ptr(c.ReadyAt),
		ExpiresAt:     rfc3339Ptr(c.ExpiresAt),
		DurationMS:    c.DurationMS,
		Width:         c.Width,
		Height:        c.Height,
		HasAudio:      c.HasAudio,
		Source:        SourceDTO{Filename: c.SourceFilename, Size: c.SourceSize, ContentType: c.SourceContentType},
		Owner:         OwnerDTO{ID: owner.ID, Username: owner.Username},
		URLs: URLsDTO{
			Page: u.Page(c.ID), Embed: u.Embed(c.ID), Video: u.Video(c.ID),
			Social: u.Social(c.ID), Light: u.Light(c.ID), Original: u.Original(c.ID),
			Poster: u.Poster(c.ID), Progress: u.Progress(c.ID),
		},
	}
	dto.Tiers, dto.CardTier = tiersOf(c, u)
	if c.TTLSeconds != nil {
		dto.TTLDays = int(*c.TTLSeconds / secondsPerDay)
	}
	if c.FPSNum != nil && c.FPSDen != nil {
		fps := strconv.Itoa(*c.FPSNum) + "/" + strconv.Itoa(*c.FPSDen)
		dto.FPS = &fps
	}
	if c.PosterWidth != nil && c.PosterHeight != nil {
		dto.Poster = &PosterDTO{Width: *c.PosterWidth, Height: *c.PosterHeight}
	}
	if c.GIFKey != nil {
		gif := u.GIF(c.ID)
		dto.GIFURL = &gif
	}
	if fs, ok := filmstripDTO(c, u); ok {
		dto.Filmstrip = &fs
	}
	if isOwner {
		v := c.Views
		dto.Views = &v
		dto.SourceDurationMS = c.SourceDurationMS
		if c.TrimStartMS != nil && c.TrimEndMS != nil {
			dto.Trim = &TrimStateDTO{StartMS: *c.TrimStartMS, EndMS: *c.TrimEndMS}
		}
	}
	return dto
}

// toUploadDTO renders the capture-tool 201 (§8.1). No owner-private field is
// in it, so the same body is safe for a token principal.
func toUploadDTO(c db.Clip, u urls.Builder) UploadDTO {
	tiers, _ := tiersOf(c, u)
	return UploadDTO{
		ID: c.ID, URL: u.Page(c.ID), EmbedURL: u.Embed(c.ID), Kind: kindOf(c),
		State: c.State, CardReady: db.CardReady(c), ExpiresAt: rfc3339Ptr(c.ExpiresAt),
		Tiers: tiers,
	}
}

func toUserDTO(u db.User) UserDTO {
	return UserDTO{ID: u.ID, Username: u.Username, CreatedAt: rfc3339(u.CreatedAt)}
}

func toTokenDTO(t db.APIToken) TokenDTO {
	scopes := t.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return TokenDTO{ID: t.ID, Name: t.Name, Prefix: t.Prefix, Scopes: scopes,
		CreatedAt: rfc3339(t.CreatedAt), LastUsedAt: rfc3339Ptr(t.LastUsedAt)}
}

// rfc3339 renders unix seconds as RFC3339 UTC.
func rfc3339(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

func rfc3339Ptr(unix *int64) *string {
	if unix == nil {
		return nil
	}
	s := rfc3339(*unix)
	return &s
}

// validID is ids.Valid (the §7 check every {id} route runs first). Since §20
// erratum 249 it accepts BOTH grammars — twelve base62 characters, or three to
// four capitalised words — so every route, every janitor and this one line
// agree about what an id is by construction.
func validID(id string) bool { return ids.Valid(id) }

// linkStyleOf names the grammar an id is in for the DTO, falling back to
// `compact` for anything ids.Kind cannot place. Nothing that reaches here can
// be unplaceable — a stored clip id was minted by one of the two minters — so
// the fallback exists only so the field is always one of the two words a
// client may switch on.
func linkStyleOf(id string) string {
	if k := ids.Kind(id); k != "" {
		return k
	}
	return ids.KindCompact
}

// --- request decoding ---

// maxJSONBody bounds every JSON request body.
const maxJSONBody = 64 << 10

// isJSONRequest reports whether Content-Type is application/json (any
// parameters, e.g. charset, are fine).
func isJSONRequest(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// requireJSON answers 415 unsupported_media_type unless the body is JSON
// (§14: forms cannot send it cross-origin without a preflight).
func requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isJSONRequest(r) {
			httpx.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// decodeJSON reads one JSON object into v (≤ 64 KiB, nothing trailing).
// On failure it writes 400 invalid_request and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
			return false
		}
		writeInvalid(w, jsonShapeMessage(err, "body must be a JSON object"))
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		writeInvalid(w, "unexpected data after the JSON object")
		return false
	}
	return true
}

// jsonShapeMessage names the field whose value has the wrong JSON type
// (§20 erratum 30): {"unlisted":"yes"} is an object, and telling the
// client it is not sends it after the wrong bug. Everything else — a syntax
// error, a truncated body, a document that is not an object at all (the
// same error type with no field path) — gets generic.
func jsonShapeMessage(err error, generic string) string {
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) && ute.Field != "" {
		return "field " + ute.Field + " must be " + jsonKind(ute.Type) + ", got " + ute.Value
	}
	return generic
}

// jsonKind renders the JSON type a Go destination accepts, with its article.
func jsonKind(t reflect.Type) string {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return "a value"
	}
	switch t.Kind() {
	case reflect.Bool:
		return "a boolean"
	case reflect.String:
		return "a string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "an array"
	case reflect.Map, reflect.Struct:
		return "an object"
	default:
		return "a value"
	}
}

// ttlDays decodes the §8.2 TTL field preserving the three cases: absent or
// null (days nil), an integer (days set), anything else (invalid → 400
// invalid_ttl rather than a generic decode error).
type ttlDays struct {
	days    *int
	invalid bool
}

func (t *ttlDays) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		t.invalid = true
		return nil
	}
	t.days = &n
	return nil
}

// resolve applies config.ParseTTLDays with def for the absent case.
func (t ttlDays) resolve(def *int64) (*int64, error) {
	if t.invalid {
		return nil, config.ErrInvalidTTL
	}
	return config.ParseTTLDays(t.days, def)
}

// present reports whether a TTL value (valid or not) was supplied.
func (t ttlDays) present() bool { return t.invalid || t.days != nil }

func writeInvalidTTL(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusBadRequest, "invalid_ttl", config.ErrInvalidTTL.Error())
}

// writeInvalidLinkStyle is the ONE answer for a `link_style` outside the closed
// set, on all four routes that take one — both uploads, the phone share target
// and the account settings (§20 erratum 250). The message names the two legal
// values, as `invalid_ttl`'s does: a client that sent `words` cannot guess what
// the vocabulary is from a refusal that will not say.
func writeInvalidLinkStyle(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusBadRequest, "invalid_link_style",
		"link_style must be "+db.LinkStyleCompact+" or "+db.LinkStyleMemorable)
}

// writeInvalidTags is the ONE answer for every tag-grammar failure on every
// route that accepts tags (§20 erratum 81): 400 `invalid_tags`, with a message
// that NAMES the offending value — a rejected list is useless to a person who
// cannot see which word in it was wrong. db.BadTagError quotes the value, so
// the message is safe to echo whatever the client sent.
func writeInvalidTags(w http.ResponseWriter, err error) {
	httpx.WriteError(w, http.StatusBadRequest, "invalid_tags", err.Error())
}

// cleanTagList is the shared validation for every write path that accepts a
// list of tags (PATCH, the upload metadata, the capture form): normalise, and
// answer 400 invalid_tags naming the offender if anything is wrong.
func cleanTagList(w http.ResponseWriter, in []string) ([]string, bool) {
	tags, err := db.CleanTags(in)
	if err != nil {
		writeInvalidTags(w, err)
		return nil, false
	}
	return tags, true
}

// Text limits (§8.3, §8.1).
const (
	maxTitleRunes       = 200
	maxDescriptionRunes = 2000
	maxTokenNameRunes   = 64
)

// The two rules a text field can break, told apart so the 400 names the
// right one (§20 erratum 29): a five-character title holding a NUL is not
// "too long".
var (
	errTextTooLong = errors.New("too long")
	errTextControl = errors.New("control character or invalid UTF-8")
	// errTextEmpty is what a field that is REQUIRED and came in blank answers.
	// cleanText itself is happy with "" (a cleared description is a legitimate
	// value); the callers that require a value used to spell the check as
	// `err != nil || name == ""` and hand writeInvalidText a nil error, which
	// prints the length rule. This sentinel is that same case, named, so a
	// caller outside this package can tell the two apart (§20 erratum 308).
	errTextEmpty = errors.New("empty")
)

// cleanText trims s and validates it: valid UTF-8, at most max runes, no
// control characters (tab, newline and CR allowed). The error says which
// rule failed.
func cleanText(s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) {
		return "", errTextControl
	}
	if utf8.RuneCountInString(s) > max {
		return "", errTextTooLong
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' {
			return "", errTextControl
		}
	}
	return s, nil
}

// writeInvalidText answers 400 invalid_request for a rejected text field:
// the field's length rule ("at most 200 characters", "1-64 characters")
// when that is what failed — or when the caller found the trimmed value
// empty (err nil) — and the control-character rule otherwise.
func writeInvalidText(w http.ResponseWriter, field, lengthRule string, err error) {
	if errors.Is(err, errTextControl) {
		writeInvalid(w, field+" must not contain control characters")
		return
	}
	writeInvalid(w, field+" must be "+lengthRule)
}
