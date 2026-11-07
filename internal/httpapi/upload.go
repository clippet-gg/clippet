package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"clippet/internal/auth"
	"clippet/internal/autorule"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/ids"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// Upload limits and budgets (§8.3, §15).
const (
	maxMetadataBytes = 16 << 10
	uploadSlackBytes = 1 << 20 // multipart framing headroom over MAX_UPLOAD_BYTES
	probeTimeout     = 30 * time.Second
	cleanupTimeout   = 10 * time.Second
	idPickAttempts   = 3
	sourceFilePrefix = "source."
	untitled         = "Untitled"
)

// uploadLimit is MAX_UPLOAD_BYTES plus the multipart framing headroom,
// saturating rather than wrapping (§20 erratum 33): config.Load bounds the
// variable, and a negative limit would make http.MaxBytesReader (which
// clamps n < 0 to 0) answer 413 to every upload, the first metadata part
// included.
func uploadLimit(maxUpload int64) int64 {
	limit := maxUpload + uploadSlackBytes
	if limit < maxUpload {
		return math.MaxInt64
	}
	return limit
}

// writeTooLarge is the 413 every upload path answers with (§20 erratum 97).
//
// The message names a SIZE, never `MAX_UPLOAD_BYTES`. web/static/app.js
// renders `message` verbatim to whoever is uploading, and the person watching
// a 5 GiB clip bounce has no idea what an environment variable of ours is,
// cannot change it, and — as the old wording never said the number — could not
// even tell how far over the line they were. The error CODE and the status are
// unchanged: a client keys on those.
func (h *handler) writeTooLarge(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusRequestEntityTooLarge, "too_large",
		"this file is larger than the server accepts (limit "+humanBytes(h.d.Cfg.MaxUploadBytes)+")")
}

// byteUnits are the binary units humanBytes renders, matching the admin CLI's
// own MiB arithmetic — MAX_UPLOAD_BYTES's default of 4294967296 is 4.0 GiB
// exactly, and calling it "4.3 GB" would only invite the operator to compare
// two different numbers.
var byteUnits = [...]string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

// humanBytes renders a byte count with one decimal in the largest unit that
// leaves it under 1024.
func humanBytes(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " bytes"
	}
	div, exp := int64(1024), 0
	for m := n / 1024; m >= 1024 && exp < len(byteUnits)-1; m /= 1024 {
		div *= 1024
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + byteUnits[exp]
}

// uploadMeta is the JSON of the first multipart part.
type uploadMeta struct {
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	Unlisted    *bool     `json:"unlisted"`
	TTLDays     ttlDays   `json:"ttl_days"`
	Tags        *[]string `json:"tags"`
	// LinkStyle picks THIS upload's id grammar (ROADMAP #35, §20 erratum 250),
	// mirroring `unlisted` exactly: a pointer, so an absent key means "follow
	// the account", and a value outside the closed set is a 400 rather than a
	// silent fallback to something the caller did not ask for.
	LinkStyle *string `json:"link_style"`

	title, description string
	ttlSeconds         *int64
	tags               []string
	// unlisted is the RESOLVED visibility: the field when the caller sent one,
	// the ACCOUNT's default otherwise (ROADMAP #22, §20 erratum 155). It is
	// filled by applyDefaults, which both upload routes run, so `ingest` never
	// has to know which grammar the request arrived in — and a client that
	// sends the field explicitly is unaffected by any preference.
	unlisted bool
	// linkStyle is the RESOLVED grammar, settled by the same applyDefaults and
	// read by pickClipID — which runs AFTER it, so a memorable id knows whether
	// the clip is unlisted and can be drawn with four words instead of three
	// (§20 erratum 250).
	linkStyle string
	// allowDuplicate is the opt-out of the duplicate check (§20 erratum 326):
	// the multipart field `duplicate=allow`, on both upload grammars. With it
	// the upload makes a new clip even when this account already holds one of
	// exactly these bytes. The bot's own paths never set it.
	allowDuplicate bool
}

// duplicateField is the opt-out's field name, and duplicateAllow the one value
// that means it (§20 erratum 326). Empty is "not sent": the check runs. Any
// other value is a 400 on both multipart grammars since run 8 (it used to be
// read as "check on", so a typo silently kept the check), in the shape of the
// grammar's other field sentences — the Clippet Auto instruction already
// refused one.
const (
	duplicateField = "duplicate"
	duplicateAllow = autorule.DuplicateAllow

	msgInvalidDuplicate = "duplicate must be allow"
)

// parseDuplicate reads the field: (allow, ok). Surrounding whitespace is
// ignored, as on every other capture field.
func parseDuplicate(value string) (bool, bool) {
	switch strings.TrimSpace(value) {
	case "":
		return false, true
	case duplicateAllow:
		return true, true
	}
	return false, false
}

// upload is POST /api/clips: streaming multipart, metadata part first, the
// video spooled to scratch, ffprobe gate, PutFile, then one transaction
// inserting the clip and its job. 201 only after the original is durable.
//
// A byte-identical upload of a live clip this account already holds is 200
// with THAT clip and `"existing": true` — never 201, because nothing was
// created; the metadata part's fields are not applied to it (§20 erratum 326).
func (h *handler) upload(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	mr, ok := h.beginUpload(w, r)
	if !ok {
		return
	}
	meta, ok := h.readMetadata(w, r, mr)
	if !ok {
		return
	}
	part, ok := h.nextVideoPart(w, r, mr, &meta)
	if !ok {
		return
	}
	clip, existing, ok := h.ingest(w, r, p, meta, part)
	if !ok {
		return
	}
	if existing {
		tags, ok := h.tagsOf(w, r, clip.ID)
		if !ok {
			return
		}
		dto := ToDTO(clip, p.User, h.d.URLs, true, tags)
		dto.Existing = true
		httpx.WriteJSON(w, http.StatusOK, dto)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, ToDTO(clip, p.User, h.d.URLs, true, meta.tags))
}

// beginUpload runs the preamble both upload routes share (§8.3 steps 1–3;
// §20 erratum 58): the multipart check, the Content-Length gates and the
// bounded body reader. The limits live here exactly once, so POST /api/upload
// cannot drift from POST /api/clips.
//
// A body with no declared length — `Transfer-Encoding: chunked`, which
// net/http reports as ContentLength = −1 — is gated too (§20 erratum 90).
// The scratch check gets the conservative need below, because the only honest
// upper bound on a chunked body is MAX_UPLOAD_BYTES itself and refusing every
// upload whenever free space is under that would be worse than useless. The
// 413 is deliberately NOT given the same treatment: MaxBytesReader is the real
// enforcement of MAX_UPLOAD_BYTES and it stops the body at the limit whatever
// the framing, while a pre-emptive 413 on an undeclared body could only be a
// guess about a length nobody stated.
func (h *handler) beginUpload(w http.ResponseWriter, r *http.Request) (*multipart.Reader, bool) {
	if !isMultipart(r) {
		httpx.WriteError(w, http.StatusUnsupportedMediaType, "not_multipart", "Content-Type must be multipart/form-data")
		return nil, false
	}
	limit := uploadLimit(h.d.Cfg.MaxUploadBytes)
	if r.ContentLength > limit {
		httpx.SetLogField(r, "reject", "too_large")
		h.writeTooLarge(w)
		return nil, false
	}
	// A negative ContentLength is net/http for "the client declared none".
	need := declaredScratchNeed(r.ContentLength, h.d.Cfg.ScratchMinFree)
	if r.ContentLength < 0 {
		need = chunkedScratchNeed(h.d.Cfg.MaxUploadBytes, h.d.Cfg.ScratchMinFree)
	}
	if free, ok := h.scratchHasRoom(need); !ok {
		httpx.SetLogField(r, "scratch_free", free)
		httpx.SetLogField(r, "reject", "insufficient_scratch")
		httpx.WriteError(w, http.StatusInsufficientStorage, "insufficient_scratch", "not enough scratch space for this upload")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.WriteError(w, http.StatusUnsupportedMediaType, "not_multipart", "Content-Type must be multipart/form-data")
		return nil, false
	}
	// Every gate has passed and the body is about to be streamed to scratch,
	// so this connection is worth keeping: take off the `Connection: close`
	// the route's outermost middleware pre-set for a body it might have had to
	// refuse unread (§20 erratum 138). Every exit above this line keeps it.
	bodyWillBeRead(w)
	return mr, true
}

// ingest is the tail both upload routes share (§8.3 steps 5–9; §17): spool to
// scratch, ffprobe gate, PutFile, one transaction, hand the spool to the
// worker, wake it. Every failure is answered on w; the caller only chooses
// the 201 body.
//
// The work itself is `ingestCore` (ingest.go), which knows nothing about HTTP
// — the Discord bot ingests through the same function with no request and no
// ResponseWriter (§20 erratum 256). What is left here is the half that is
// about THIS request: turning the core's verdict into the status, code,
// message and access-log fields these two routes have always written.
//
// existing is the duplicate answer (§20 erratum 326): clip is the account's
// own live clip of exactly these bytes, and nothing was created.
func (h *handler) ingest(w http.ResponseWriter, r *http.Request, p auth.Principal, meta uploadMeta, part *multipart.Part) (clip db.Clip, existing, ok bool) {
	clip, existing, err := h.ingestCore(r.Context(), p.User.ID, meta, ingestSource{
		Body: part, Filename: part.FileName(), ContentType: part.Header.Get("Content-Type"),
	})
	if err != nil {
		h.writeIngestError(w, r, err)
		return db.Clip{}, false, false
	}
	httpx.SetLogField(r, "clip", clip.ID)
	if existing {
		httpx.SetLogField(r, "existing", true)
	}
	return clip, existing, true
}

// writeIngestError is the ONE mapping from an ingest failure to an HTTP
// answer. Every arm is the exact status, error code, message and log field the
// route wrote before the core was split out; the arms are in the order the
// pipeline meets them.
func (h *handler) writeIngestError(w http.ResponseWriter, r *http.Request, err error) {
	var ie *ingestError
	if !errors.As(err, &ie) {
		// Unreachable: ingestCore returns nothing else. A 503 rather than a
		// panic, because an upload is the one request where the client has
		// already spent its bytes.
		writeDBError(w, r, err)
		return
	}
	switch ie.kind {
	case ingestFailTooLarge:
		httpx.SetLogField(r, "reject", "too_large")
		h.writeTooLarge(w)
	case ingestFailScratch:
		httpx.SetLogField(r, "scratch_error", ie.Error())
		httpx.WriteError(w, http.StatusInsufficientStorage, "insufficient_scratch", "scratch disk error")
	case ingestFailQuota:
		// 507 for the second time in this switch, and correctly: the
		// condition is "this server has nowhere to put it", which is what the
		// status means. The CODE is what tells the two apart — a full scratch
		// disk is the operator's problem and clears itself, a crossed quota is
		// a deliberate line somebody drew (§20 erratum 277).
		httpx.SetLogField(r, "reject", "storage_quota")
		httpx.WriteError(w, http.StatusInsufficientStorage, "storage_quota", ie.message)
	case ingestFailEmpty:
		httpx.SetLogField(r, "reject", "empty_video")
		httpx.WriteError(w, http.StatusBadRequest, "empty_video", "the video part is empty")
	case ingestFailMalformed:
		httpx.SetLogField(r, "reject", "malformed_multipart")
		httpx.SetLogField(r, "multipart_error", ie.Error())
		writeInvalid(w, "the multipart body ended before the video part was complete")
	case ingestFailGone:
		httpx.SetLogField(r, "client_disconnected", true)
		httpx.WriteError(w, http.StatusBadRequest, "upload_interrupted", "client disconnected during upload")
	case ingestFailNotVideo:
		httpx.SetLogField(r, "reject", "not_a_video")
		httpx.SetLogField(r, "probe_error", ie.Error())
		httpx.WriteError(w, http.StatusUnsupportedMediaType, "not_a_video", "the file is not a playable video or audio file")
	case ingestFailProbe:
		httpx.SetLogField(r, "probe_error", ie.Error())
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", "could not inspect the upload; retry shortly")
	case ingestFailStorage:
		writeStorageError(w, r, ie)
	case ingestFailIDCollision:
		httpx.SetLogField(r, "reject", "id_collision")
		httpx.WriteError(w, http.StatusInternalServerError, "id_collision", ie.message)
	default: // ingestFailDB
		writeDBError(w, r, ie)
	}
}

func isMultipart(r *http.Request) bool {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "multipart/form-data" && params["boundary"] != ""
}

// declaredScratchNeed is the §8.3 precheck for a body that states its length:
// 1.5×Content-Length + SCRATCH_MIN_FREE_BYTES. The half is the framing plus
// headroom; the operator's floor is what the WORKER will find still free once
// this upload's bytes are on the disk, which is the whole point of the pair
// (worker.AcquireScratchNeed is the other half, §20 erratum 90).
func declaredScratchNeed(contentLength, minFree int64) int64 {
	return contentLength + contentLength/2 + minFree
}

// chunkedScratchNeed is the precheck for a body that states no length at all.
// Half of MAX_UPLOAD_BYTES is a deliberate compromise: the true upper bound is
// the whole of MAX_UPLOAD_BYTES (4 GiB by default), and reserving that would
// refuse every capture-tool upload on a scratch volume sized for real clips,
// while reserving nothing is what let a chunked body walk past the operator's
// one lever entirely.
func chunkedScratchNeed(maxUpload, minFree int64) int64 {
	return maxUpload/2 + minFree
}

// scratchHasRoom applies the §8.3 precheck: free ≥ need. A statfs failure is
// logged and does not block — the spool itself reports a real disk problem as
// 507. The reading is returned so each caller can put it in its own refusal:
// the access log for an upload route, one log line for the bot, which has an
// attachment's declared size and the same reason to check it.
func (h *handler) scratchHasRoom(need int64) (free int64, ok bool) {
	if h.d.FreeBytes == nil {
		return 0, true
	}
	free, err := h.d.FreeBytes(h.d.Cfg.ScratchDir)
	if err != nil {
		h.d.Log.Warn("upload: scratch statfs", "dir", h.d.Cfg.ScratchDir, "err", err)
		return 0, true
	}
	return free, free >= need
}

// readMetadata reads part 1, which must be named "metadata" (§8.3 step 4).
func (h *handler) readMetadata(w http.ResponseWriter, r *http.Request, mr *multipart.Reader) (uploadMeta, bool) {
	var meta uploadMeta
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "metadata" {
		httpx.SetLogField(r, "reject", "metadata_first")
		httpx.WriteError(w, http.StatusBadRequest, "metadata_first", "the first multipart part must be named metadata")
		return meta, false
	}
	data, err := io.ReadAll(io.LimitReader(part, maxMetadataBytes+1))
	if err != nil {
		writeInvalid(w, "could not read the metadata part")
		return meta, false
	}
	if len(data) > maxMetadataBytes {
		writeInvalid(w, "metadata part larger than 16 KiB")
		return meta, false
	}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &meta); err != nil {
			writeInvalid(w, jsonShapeMessage(err, "metadata is not a JSON object"))
			return meta, false
		}
	}
	if !h.checkMeta(w, r, &meta) {
		return meta, false
	}
	return meta, true
}

// checkMeta is the metadata document's every rule after the JSON itself: the
// title, the description, the tags, then the defaults. The one-shot upload's
// metadata part and the chunked upload's open both run it (§20 erratum 344), so
// the two doors cannot accept different documents.
func (h *handler) checkMeta(w http.ResponseWriter, r *http.Request, meta *uploadMeta) bool {
	var err error
	if meta.Title != nil {
		if meta.title, err = cleanText(*meta.Title, maxTitleRunes); err != nil {
			writeInvalidText(w, "title", "at most 200 characters", err)
			return false
		}
	}
	if meta.Description != nil {
		if meta.description, err = cleanText(*meta.Description, maxDescriptionRunes); err != nil {
			writeInvalidText(w, "description", "at most 2000 characters", err)
			return false
		}
	}
	// Tags travel with the upload so a capture-driven clip is filed the moment
	// it exists, rather than needing a second request (§20 erratum 81). An
	// absent key and `[]` are the same thing here — a new clip has no tag set
	// to leave alone — so there is no pointer dance as there is for the TTL.
	if meta.Tags != nil {
		if meta.tags, err = db.CleanTags(*meta.Tags); err != nil {
			httpx.SetLogField(r, "reject", "invalid_tags")
			writeInvalidTags(w, err)
			return false
		}
	}
	return h.applyDefaults(w, r, meta)
}

// applyDefaults settles the two fields an upload may leave out, for BOTH
// upload routes, in one place (ROADMAP #22, §20 erratum 155).
//
// The order is `field sent > account default > instance default`, and the
// first clause is what makes this safe to add to a shipped API: a capture
// tool that sends `ttl_days` and `unlisted` — which the `.sxcu` this app
// mints does — behaves exactly as it did before accounts had preferences.
// Only silence is filled in.
//
// It lives here rather than in each route's parser because the two grammars
// (a JSON metadata part and a form field) are the only difference between the
// routes, and a default that applied to one and not the other would be a
// difference the user could feel.
func (h *handler) applyDefaults(w http.ResponseWriter, r *http.Request, meta *uploadMeta) bool {
	p, _ := auth.FromContext(r.Context())
	switch err := h.applyDefaultsCtx(r.Context(), p.User.ID, meta); {
	case err == nil:
		return true
	case errors.Is(err, errInvalidLinkStyle):
		httpx.SetLogField(r, "reject", "invalid_link_style")
		writeInvalidLinkStyle(w)
	default:
		httpx.SetLogField(r, "reject", "invalid_ttl")
		writeInvalidTTL(w)
	}
	return false
}

// applyDefaultsCtx is that resolution with no request around it — the half the
// Discord bot needs, which uploads as the bound account and must get that
// account's own TTL and visibility rather than the instance's (§20 erratum
// 256). It returns config.ErrInvalidTTL for a `ttl_days` the CALLER sent that
// is not one of the five legal values; a bad value in the account row is
// warned about and falls back, exactly as it always did.
func (h *handler) applyDefaultsCtx(ctx context.Context, userID string, meta *uploadMeta) error {
	def := h.userDefaults(ctx, userID)
	fallback := h.d.Cfg.DefaultTTLSeconds
	if def.TTLDays != nil {
		// The account HAS a preference, including "0 = keep forever", which is
		// a decision and not an absence — ParseTTLDays turns it into the nil
		// that means never. An account default is validated on the way in, so
		// this cannot fail on a row this build wrote; a value from a hand-
		// edited database falls back to the instance rather than 500ing an
		// upload over somebody's SQL.
		if secs, err := config.ParseTTLDays(def.TTLDays, nil); err == nil {
			fallback = secs
		} else {
			h.d.Log.Warn("account default ttl is not a legal value; using the instance default",
				"user", userID, "ttl_days", *def.TTLDays)
		}
	}
	secs, err := meta.TTLDays.resolve(fallback)
	if err != nil {
		return err
	}
	meta.ttlSeconds = secs
	meta.unlisted = def.Unlisted
	if meta.Unlisted != nil {
		meta.unlisted = *meta.Unlisted
	}
	// The id grammar, by the same order and for the same reasons (§20 erratum
	// 250). The capture endpoint and the phone share target send no field at
	// all — the shipped `clippet.sxcu` has no argument for it — so this is what
	// makes them follow the account's own choice rather than the instance's,
	// and it is what makes the Discord bot's `/clip` follow the BOUND
	// account's: the bot sends no field either (§20 erratum 256).
	meta.linkStyle = def.LinkStyleOr()
	if meta.LinkStyle != nil {
		if !db.ValidLinkStyle(*meta.LinkStyle) {
			return errInvalidLinkStyle
		}
		meta.linkStyle = *meta.LinkStyle
	}
	return nil
}

// errInvalidLinkStyle is what applyDefaultsCtx reports for a `link_style` the
// CALLER sent that is outside the closed set — the link-grammar twin of
// config.ErrInvalidTTL, and the reason applyDefaults switches on the error it
// gets back rather than calling every failure a TTL (§20 errata 250, 256). It
// is reachable only from the JSON metadata part: the capture grammar refuses a
// bad value at the field, before the file is read, and the bot sends none.
var errInvalidLinkStyle = errors.New("invalid link_style")

// nextVideoPart reads part 2, which must be named "video" and carry a
// filename (§8.3 step 5). One text part may come between the metadata and the
// video: `duplicate` (§20 erratum 326), the duplicate check's opt-out, read
// into meta. Any other part there is still missing_video.
func (h *handler) nextVideoPart(w http.ResponseWriter, r *http.Request, mr *multipart.Reader, meta *uploadMeta) (*multipart.Part, bool) {
	part, err := mr.NextPart()
	if err == nil && part.FormName() == duplicateField && part.FileName() == "" {
		value, ok := readCaptureField(w, part, duplicateField)
		if !ok {
			return nil, false
		}
		allow, ok := parseDuplicate(value)
		if !ok {
			httpx.SetLogField(r, "reject", "invalid_duplicate")
			writeInvalid(w, msgInvalidDuplicate)
			return nil, false
		}
		meta.allowDuplicate = allow
		part, err = mr.NextPart()
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			h.writeTooLarge(w)
			return nil, false
		}
		httpx.SetLogField(r, "reject", "missing_video")
		httpx.WriteError(w, http.StatusBadRequest, "missing_video", "a video part with a filename must follow metadata")
		return nil, false
	}
	if part.FormName() != "video" || part.FileName() == "" {
		httpx.SetLogField(r, "reject", "missing_video")
		httpx.WriteError(w, http.StatusBadRequest, "missing_video", "a video part with a filename must follow metadata")
		return nil, false
	}
	return part, true
}

// newClipID draws a fresh clip id in the grammar this upload resolved (§20
// erratum 250). A package variable so a test can pin the id an upload picks:
// the id_collision path needs a row to appear under that id between pickClipID
// and CreateClipWithJob (§20.21).
//
// The word COUNT is decided here and not in internal/ids, because it is a
// product rule about clips rather than a fact about the generator: three words
// for a listed clip (≈38.8 bits — public-clip grade), four when the clip is
// uploaded UNLISTED (≈51.7 — unlisted grade, where the link is the only
// authentication). The owner's rule of 2026-09-09: an unlisted link stays safe
// even if the rate limit breaks. A clip flipped to unlisted LATER keeps the id it has (erratum 251):
// an id is an address, and changing it would break every link already sent.
var newClipID = func(style string, unlisted bool) string {
	if style != db.LinkStyleMemorable {
		return ids.New()
	}
	if unlisted {
		return ids.NewMemorable(ids.MemorableMaxWords)
	}
	return ids.NewMemorable(ids.MemorableMinWords)
}

// diskError tags a write-side failure so spoolVideo can tell "the disk
// failed" (507) from "the client stopped sending" (400) inside io.Copy.
type diskError struct{ err error }

func (e *diskError) Error() string { return "scratch write: " + e.err.Error() }
func (e *diskError) Unwrap() error { return e.err }

// diskWriter wraps the scratch file without ReaderFrom so io.Copy uses its
// generic loop and every write error passes through Write.
type diskWriter struct{ f *os.File }

func (d diskWriter) Write(p []byte) (int, error) {
	n, err := d.f.Write(p)
	if err != nil {
		return n, &diskError{err}
	}
	return n, nil
}

// spoolVideo streams the part into src and fsyncs it. It maps the failure
// classes of §8.3 step 5: MaxBytesReader overflow → 413, disk → 507, zero
// bytes → 400 empty_video, client gone → 400 upload_interrupted, and a body
// that is merely malformed → 400 invalid_request (§20 erratum 53).
//
// The spooling itself is `spoolBody` (ingest.go), which the bot's ingest uses
// too; what is left here is the HTTP answer, through the one mapping every
// ingest failure goes through. `replace` (ROADMAP #31) calls this, which is
// why it stays a function of its own rather than folding into ingestCore.
func (h *handler) spoolVideo(w http.ResponseWriter, r *http.Request, part *multipart.Part, dir, src string) (int64, bool) {
	n, _, err := h.spoolBody(r.Context(), part, dir, src)
	if err != nil {
		h.writeIngestError(w, r, err)
		return 0, false
	}
	return n, true
}

// probeGate runs the injected ffprobe (30s) and rejects anything without a
// real video stream with 415 — nothing reaches the bucket (§8.3 step 6). A
// bad file is a normal input: INFO, never an error. Only file-at-fault
// failures (transcode.ErrPermanent) earn the 415 (§11): a client disconnect
// is upload_interrupted like the neighbouring steps, and environmental
// trouble (ffprobe exec failure, the probe budget on a wedged scratch disk)
// is a 503 with a WARN — never a permanent-sounding verdict on a good file.
//
// The probe itself is `probeSource` (ingest.go); this is its HTTP answer, and
// `replace` calls it for spoolVideo's reason.
func (h *handler) probeGate(w http.ResponseWriter, r *http.Request, src string) (*transcode.ProbeResult, bool) {
	p, err := h.probeSource(r.Context(), src)
	if err != nil {
		h.writeIngestError(w, r, err)
		return nil, false
	}
	return p, true
}

// abortUpload removes whatever reached the bucket under the clip prefix
// (best effort, fresh 10s context — the request may already be cancelled)
// and the scratch directory.
func (h *handler) abortUpload(prefix, dir string) {
	if h.d.Store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if _, err := h.d.Store.DeletePrefix(ctx, prefix); err != nil {
			h.d.Log.Warn("upload: cleanup prefix", "prefix", prefix, "err", err)
		}
	}
	h.removeScratch(dir)
}

func (h *handler) removeScratch(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		h.d.Log.Warn("upload: remove scratch", "dir", dir, "err", err)
	}
}

// clientGone reports whether err (or the request) reflects the client
// disconnecting rather than a backend failure. It is the request-shaped
// spelling of ingest.go's ingestClientGone, which the ingest core (with no
// request in sight) asks the same question with.
func clientGone(r *http.Request, err error) bool {
	return ingestClientGone(r.Context(), err)
}

// Filename bounds (§20 erratum 26). A multipart part header may carry a
// filename of several MiB (Go's mime/multipart allows 10 MiB of part
// headers); unbounded, it was stored, echoed in every ClipDTO — the shared
// library included — and put into a Content-Disposition no client accepts.
// 255 is the usual filesystem limit; a short extension survives the cut so
// the download name keeps its type.
const (
	maxFilenameRunes = 255
	maxExtRunes      = 16
)

// stripPath reduces a client-supplied filename to its last path segment
// (both slash styles) without control characters or surrounding space,
// capped at maxFilenameRunes; "" when nothing usable remains.
func stripPath(filename string) string {
	if i := strings.LastIndexAny(filename, `\/`); i >= 0 {
		filename = filename[i+1:]
	}
	filename = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, filename)
	return truncateName(strings.TrimSpace(filename), maxFilenameRunes)
}

// truncateName shortens name to at most max runes, keeping its extension
// when that is a plausible one (at most maxExtRunes runes).
func truncateName(name string, max int) string {
	if utf8.RuneCountInString(name) <= max {
		return name
	}
	ext := path.Ext(name)
	if utf8.RuneCountInString(ext) > maxExtRunes {
		ext = ""
	}
	runes := []rune(name)
	head := strings.TrimSpace(string(runes[:max-utf8.RuneCountInString(ext)]))
	return head + ext
}

// baseName is what source_filename stores: stripPath, or source.bin when
// the client sent nothing usable.
func baseName(filename string) string {
	if name := stripPath(filename); name != "" {
		return name
	}
	return sourceFilePrefix + store.DefaultExt
}

// defaultTitle is the sanitized filename sans extension, capped at 200
// runes, or "Untitled" — unless the stem carries a capture tool's timestamp,
// in which case it is that timestamp as a date and a time (captureTitle,
// §20 erratum 317(a)). It is the ONE fallback: every route that creates a
// clip without a typed title reaches it through the ingest core.
func defaultTitle(filename string) string {
	name := stripPath(filename)
	name = strings.TrimSuffix(name, path.Ext(name))
	name = strings.TrimSpace(name)
	if t, ok := captureTitle(name); ok {
		name = t
	}
	if utf8.RuneCountInString(name) > maxTitleRunes {
		runes := []rune(name)
		name = strings.TrimSpace(string(runes[:maxTitleRunes]))
	}
	if name == "" {
		return untitled
	}
	return name
}

// captureStamp finds a date followed by a clock in a file's stem — the way
// capture tools name what they record: OBS `2025-06-24 00-23-54`, ShareX
// `2025-06-24_00-23-54`, Game Bar and NVIDIA `Arc Raiders 2026.01.06 -
// 00.15.37.07`. Groups: 1 the date, 2 year, 3 its first separator, 4 month,
// 5 its second separator, 6 day; 7 the separator before the clock; 8 hour,
// 9 the clock's separator, 10 minute, 11 the seconds' separator, 12 seconds.
// The character before the date (when there is one) is consumed so that a
// date cannot start inside a longer run of digits; the separators' agreement,
// the ranges and the character after the clock are checked in captureTitle,
// which RE2 cannot express.
var captureStamp = regexp.MustCompile(
	`(?:^|[^0-9])((\d{4})([-._])(\d{2})([-._])(\d{2}))( - |_-_|[ _-])(\d{2})([-._])(\d{2})(?:([-._])(\d{2}))?`)

// captureTitle is §20 erratum 317(a)'s rule (the owner's pick (1a), the
// review's wording): when the stem carries a date `YYYY-MM-DD`, `YYYY.MM.DD` or
// `YYYY_MM_DD` followed by a clock `HH-MM-SS`, `HH.MM.SS`, `HH_MM_SS` or
// `HH-MM` (between them a space, `_`, `-` or ` - `), the title is
// `YYYY-MM-DD HH:MM` — seconds dropped, the date's separators made hyphens,
// the clock as the file wrote it (no timezone). Text before the date is kept
// in front, its underscores made spaces and its runs of spaces collapsed;
// everything after the clock (`.DVR`, a trim range, a `(1)`) is dropped.
// ok is false when the stem has no such timestamp, and the caller keeps the
// stem byte for byte.
func captureTitle(stem string) (string, bool) {
	for _, m := range captureStamp.FindAllStringSubmatchIndex(stem, -1) {
		g := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return stem[m[2*i]:m[2*i+1]]
		}
		num := func(i int) int { n, _ := strconv.Atoi(g(i)); return n }
		if g(3) != g(5) { // 2025-06.24 is not a date
			continue
		}
		seconds := g(12) != ""
		if seconds && g(11) != g(9) { // 00.15-37: not one clock
			continue
		}
		if !seconds && g(9) != "-" { // the bare HH-MM form is hyphenated only
			continue
		}
		if mo, d, h, mi := num(4), num(6), num(8), num(10); mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 {
			continue
		}
		if seconds && num(12) > 59 {
			continue
		}
		if end := m[1]; end < len(stem) && stem[end] >= '0' && stem[end] <= '9' {
			continue // the clock runs on into more digits: not a clock
		}
		stamp := g(2) + "-" + g(4) + "-" + g(6) + " " + g(8) + ":" + g(10)
		prefix := strings.Join(strings.Fields(strings.ReplaceAll(stem[:m[2]], "_", " ")), " ")
		if prefix == "" {
			return stamp, true
		}
		return prefix + " " + stamp, true
	}
	return "", false
}
