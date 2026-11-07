package httpapi

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"clippet/internal/auth"
	"clippet/internal/config"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/urls"
)

// Capture-form bounds (§20 erratum 58). A capture tool sends a handful of
// short text fields; both numbers are far above any real client and exist so
// a hostile body cannot make the handler read arbitrarily much before the
// file part (MAX_UPLOAD_BYTES already bounds the body as a whole).
const (
	maxCaptureFields    = 32
	maxCaptureFieldByte = 16 << 10
)

// captureFilePart reports whether name is one of the two accepted names for
// the file part. ShareX's FileFormName is configurable and RePlays/OBS
// scripts differ, so `file` (what clippet.sxcu ships) and `video` (the name
// POST /api/clips uses) are both taken.
func captureFilePart(name string) bool { return name == "file" || name == "video" }

// captureUpload is POST /api/upload (§8.1 Phase 4 row; §20 erratum 58): the
// single-request capture-tool endpoint. It differs from POST /api/clips in
// the multipart grammar and the response only — the auth model, the limits,
// the rate limit and the scratch-first ingest pipeline are literally the same
// code (beginUpload + ingest).
//
// The one behavioural difference is the wait before the 201 (§20 errata 97,
// 212). This route's answer is the link that goes on somebody's clipboard, and
// Discord CACHES the embed it extracts per URL: a link pasted while the clip is
// still `queued` unfurls as a bare link-out card and stays one long after the
// clip is playable. POST /api/clips has no such problem — its caller is a
// browser watching a progress bar — so the wait lives here and only here.
//
// Since §20 erratum 212 what it waits for is CARD-READY and not merely a
// terminal state. The owner's decision of 2026-09-08: a link is given only when
// it is ready for sharing, which means the Discord card is ready. db.CardReady is that predicate.
//
// A byte-identical upload of a live clip this account already holds answers
// 200 with that clip's UploadDTO and `"existing": true` (§20 erratum 326).
func (h *handler) captureUpload(w http.ResponseWriter, r *http.Request) {
	clip, existing, ok := h.capture(w, r)
	if !ok {
		return
	}
	httpx.WriteJSON(w, uploadStatus(existing), uploadDTOFor(clip, existing, h.d.URLs))
}

// uploadStatus is a capture-grammar route's success status: 201 for a clip
// made, 200 for the duplicate answer — nothing was created (§20 erratum 326).
func uploadStatus(existing bool) int {
	if existing {
		return http.StatusOK
	}
	return http.StatusCreated
}

// uploadDTOFor is toUploadDTO with the duplicate answer's flag.
func uploadDTOFor(c db.Clip, existing bool, u urls.Builder) UploadDTO {
	dto := toUploadDTO(c, u)
	dto.Existing = existing
	return dto
}

// capture is the whole capture route but its 201 body, shared with POST
// /in/{token} (§20 erratum 322) so the two cannot drift: the preamble, the
// capture grammar, the title rule, the ingest and the card-ready wait. Every
// refusal is answered on w; ok is false then and the caller writes nothing.
//
// existing is the duplicate answer (§20 erratum 326); the wait applies to it
// too, since the clip it names may still be encoding.
func (h *handler) capture(w http.ResponseWriter, r *http.Request) (clip db.Clip, existing, ok bool) {
	p, _ := auth.FromContext(r.Context())
	mr, ok := h.beginUpload(w, r)
	if !ok {
		return db.Clip{}, false, false
	}
	meta, part, ok := h.readCaptureForm(w, r, mr)
	if !ok {
		return db.Clip{}, false, false
	}
	meta.title = titleUnlessFilename(meta.title, part.FileName())
	clip, existing, ok = h.ingest(w, r, p, meta, part)
	if !ok {
		return db.Clip{}, false, false
	}
	return h.awaitCardReady(r, clip, h.captureWait()), existing, true
}

// captureWait is the budget for this request: CAPTURE_READY_WAIT, and nothing
// else (§20 erratum 212).
//
// The per-request `wait` field is GONE. The owner's decision of 2026-09-08: a
// user may not set up a breaking behaviour at all. A capture tool that
// sent `wait=0` got back a link whose card is a bare link-out one, Discord cached
// that per URL, and the person pasting it never learned why — so the client
// cannot ask for it any more, and applyCaptureField refuses the field with a 400
// naming the setting that replaced it.
//
// The clamp is repeated here only because a Config assembled in a test does not
// go through config.Load.
func (h *handler) captureWait() time.Duration {
	d := h.d.Cfg.CaptureReadyWait
	if d < 0 {
		return 0
	}
	return min(d, config.CaptureReadyWaitMax)
}

// awaitCardReady holds the 201 until the clip's CARD works (db.CardReady), the
// clip has failed, the budget runs out, or the client goes away, and returns the
// clip as the database then has it. It is never an error: a wait that expires
// answers 201 with the state the clip really is in and `card_ready: false`,
// exactly as this route has always answered an expired wait (§20 errata 97, 212).
//
// There is deliberately NO time estimate in that answer (the owner's call,
// 2026-09-08): an estimate is too often a bit of a lie.
//
// The wake-up is the worker's progress registry, not a poll — and the worker
// writes a run's terminal entry AFTER the commit that published it, so the
// re-read below cannot see the state the wait exists to skip past. A clip
// this process is not transcoding (queued behind others, or claimed by
// nobody) simply never signals and the budget ends the wait.
//
// The route is outside http.TimeoutHandler (§8, `long`), so nothing else in
// the stack is counting: the budget and the client's own disconnect are the
// only two things that can end this, and the ceiling on the budget is a
// minute (config.CaptureReadyWaitMax).
func (h *handler) awaitCardReady(r *http.Request, clip db.Clip, budget time.Duration) db.Clip {
	fresh, reread := h.awaitCardReadyCtx(r.Context(), clip, budget)
	if reread {
		httpx.SetLogField(r, "ready_wait", fresh.State)
		httpx.SetLogField(r, "card_ready", db.CardReady(fresh))
	}
	return fresh
}

// awaitCardReadyCtx is that wait with no request around it — the half the
// Discord bot's `/clip` needs, which holds the SAME budget before it edits the
// deferred reply with the share link (§20 erratum 256). One implementation, so
// "the link is handed over when the card works" cannot come to mean two
// different things on two surfaces.
//
// The bool is whether the row was RE-READ (the wait ended in a terminal
// signal rather than in the budget or a disconnect); the capture route puts
// the fresh state in its access log line only then, exactly as it did when
// this was one function.
func (h *handler) awaitCardReadyCtx(ctx context.Context, clip db.Clip, budget time.Duration) (db.Clip, bool) {
	if budget <= 0 || db.CardReady(clip) || clip.State == db.ClipFailed {
		return clip, false
	}
	done, stop := h.d.Worker.AwaitTerminal(clip.ID)
	defer stop()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		return clip, false
	case <-ctx.Done():
		return clip, false
	}
	fresh, err := h.d.DB.GetClip(ctx, clip.ID)
	if err != nil {
		// The clip was answered for already; a re-read that fails (or a clip
		// destroyed inside the wait) just means the caller gets the state the
		// 201 would have carried anyway.
		h.d.Log.Info("capture upload: re-read after the ready wait", "clip", clip.ID, "err", err)
		return clip, false
	}
	return fresh, true
}

// readCaptureForm reads the simple text fields that precede the file part and
// returns the file part itself, still unread (§8.1: `title?`, `description?`,
// `ttl_days?`, `unlisted?` in ANY order BEFORE `file`).
//
//   - a part with a filename ends the loop: it must be named `file` or
//     `video`, else 400 missing_video;
//   - a text part named `title`/`description`/`ttl_days`/`unlisted`/`tags`/
//     `link_style` is applied, with the §8.2 validation POST /api/clips applies
//     to the same value; `wait` is read and REFUSED, because it used to be this route's own
//     ready-wait override and is now the operator's setting alone (§20 erratum
//     212);
//   - any other text part is ignored, so a tool that sends extras still works;
//   - a text part AFTER the file part is never seen: ingest streams the file
//     to scratch and the request is answered without reading further.
func (h *handler) readCaptureForm(w http.ResponseWriter, r *http.Request, mr *multipart.Reader) (uploadMeta, *multipart.Part, bool) {
	var meta uploadMeta
	fields := 0
	for {
		part, err := mr.NextPart()
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				httpx.SetLogField(r, "reject", "too_large")
				h.writeTooLarge(w)
				return meta, nil, false
			}
			writeMissingFile(w, r)
			return meta, nil, false
		}
		name := part.FormName()
		if part.FileName() != "" {
			if !captureFilePart(name) {
				writeMissingFile(w, r)
				return meta, nil, false
			}
			if !h.resolveCaptureTTL(w, r, &meta) {
				return meta, nil, false
			}
			return meta, part, true
		}
		fields++
		if fields > maxCaptureFields {
			httpx.SetLogField(r, "reject", "too_many_fields")
			writeInvalid(w, "too many form fields before the file part")
			return meta, nil, false
		}
		if !captureField(name) {
			continue // unknown text field: ignored, never fatal
		}
		value, ok := readCaptureField(w, part, name)
		if !ok {
			return meta, nil, false
		}
		if !applyCaptureField(w, r, &meta, name, value) {
			return meta, nil, false
		}
	}
}

// captureField reports whether name is a text field this endpoint reads.
//
// `wait` is still in the list although it is no longer a setting: it is read so
// that it can be REFUSED with a sentence naming what replaced it (§20 erratum
// 212). Dropping it from the list would make it one of the unknown fields this
// grammar ignores, and a tool configured with `wait=0` would then go on
// uploading while quietly getting the opposite of what it asked for.
func captureField(name string) bool {
	switch name {
	case "title", "description", "ttl_days", "unlisted", "tags", "link_style", "wait", duplicateField:
		return true
	}
	return false
}

// readCaptureField reads one text part, bounded like the metadata part.
//
// label names the field in the two error messages and MUST be a literal the
// SERVER chose — never part.FormName(). mime/multipart lets a part header run
// to megabytes, so echoing the caller's own name back would put unbounded
// caller-chosen text in an owner-facing error, which is exactly what erratum
// 26 bounded filenames to stop (see also erratum 135). readCaptureForm
// satisfies that by passing `name` only after captureField has restricted it
// to its six literals; replaceFilePart, which reads no field at all, passes
// the constant "form".
func readCaptureField(w http.ResponseWriter, part *multipart.Part, label string) (string, bool) {
	data, err := io.ReadAll(io.LimitReader(part, maxCaptureFieldByte+1))
	if err != nil {
		writeInvalid(w, "could not read the "+label+" field")
		return "", false
	}
	if len(data) > maxCaptureFieldByte {
		writeInvalid(w, "the "+label+" field is larger than 16 KiB")
		return "", false
	}
	return string(data), true
}

// applyCaptureField validates one field into meta with the §8.2 rules and the
// §20 erratum 29 messages. An empty value means "not sent" for every field:
// a capture tool that always emits its arguments must not be forced to send
// a meaningful TTL or title.
func applyCaptureField(w http.ResponseWriter, r *http.Request, meta *uploadMeta, name, value string) bool {
	trimmed := strings.TrimSpace(value)
	switch name {
	case "title":
		clean, err := cleanText(value, maxTitleRunes)
		if err != nil {
			writeInvalidText(w, "title", "at most 200 characters", err)
			return false
		}
		meta.title = clean
	case "description":
		clean, err := cleanText(value, maxDescriptionRunes)
		if err != nil {
			writeInvalidText(w, "description", "at most 2000 characters", err)
			return false
		}
		meta.description = clean
	case "ttl_days":
		if trimmed == "" {
			return true
		}
		n, err := strconv.Atoi(trimmed)
		if err != nil {
			httpx.SetLogField(r, "reject", "invalid_ttl")
			writeInvalidTTL(w)
			return false
		}
		if _, err := config.ParseTTLDays(&n, nil); err != nil {
			httpx.SetLogField(r, "reject", "invalid_ttl")
			writeInvalidTTL(w)
			return false
		}
		meta.TTLDays.days = &n
	case "unlisted":
		if trimmed == "" {
			return true
		}
		b, ok := parseCaptureBool(trimmed)
		if !ok {
			writeInvalid(w, "unlisted must be true or false")
			return false
		}
		meta.Unlisted = &b
	case "tags":
		// A form field cannot carry a JSON array, so the capture grammar
		// spells a tag set the way a person types one: comma-separated
		// ("gaming, clutch"). An empty field means "not sent", like every
		// other capture field (§20 erratum 58).
		if trimmed == "" {
			return true
		}
		tags, err := db.CleanTags(db.SplitTags(value))
		if err != nil {
			httpx.SetLogField(r, "reject", "invalid_tags")
			writeInvalidTags(w, err)
			return false
		}
		meta.tags = tags
	case "link_style":
		// The id grammar for this upload (§20 erratum 250). An empty field means
		// "not sent" like every other capture field, and a value outside the
		// closed set is refused HERE rather than at applyDefaults, so the 400
		// lands before the file has been read — the same treatment `ttl_days`
		// gets, for the same reason. The shipped `clippet.sxcu` sends no such
		// field at all and therefore follows the account's default.
		if trimmed == "" {
			return true
		}
		if !db.ValidLinkStyle(trimmed) {
			httpx.SetLogField(r, "reject", "invalid_link_style")
			writeInvalidLinkStyle(w)
			return false
		}
		meta.LinkStyle = &trimmed
	case duplicateField:
		// The duplicate check's opt-out (§20 erratum 326): `allow` and only
		// `allow` makes this upload a new clip even when the account already
		// holds one of exactly these bytes; empty is "not sent" (the check
		// stays on), and any other value is refused (run 8), before the file
		// is read.
		allow, ok := parseDuplicate(value)
		if !ok {
			httpx.SetLogField(r, "reject", "invalid_duplicate")
			writeInvalid(w, msgInvalidDuplicate)
			return false
		}
		meta.allowDuplicate = allow
	case "wait":
		// REFUSED (§20 erratum 212). It used to override CAPTURE_READY_WAIT for
		// one request, and the owner's decision of 2026-09-08 is that a client may
		// not configure this away: a user may not set up a breaking behaviour
		// at all.
		//
		// It is a 400 rather than a silent ignore, because silence is what makes
		// the old behaviour look like it is still working: a `.sxcu` or a script
		// carrying the field is told once, in a sentence naming the operator
		// setting that replaced it. The shipped clippet.sxcu never sent it.
		if trimmed == "" {
			// An empty field means "not sent" for every other capture field, and
			// a tool that always emits its arguments must not be told off for a
			// value it did not choose.
			return true
		}
		httpx.SetLogField(r, "reject", "wait_removed")
		writeInvalid(w, "wait is no longer accepted: the server's CAPTURE_READY_WAIT decides how long the reply is held")
		return false
	}
	return true
}

// resolveCaptureTTL fills in the fields the form left out — the account's own
// defaults, then the instance's — through the SAME applyDefaults the metadata
// part's absent keys go through (§8.2, ROADMAP #22). Both upload routes
// therefore agree by construction rather than by two matching lines.
func (h *handler) resolveCaptureTTL(w http.ResponseWriter, r *http.Request, meta *uploadMeta) bool {
	return h.applyDefaults(w, r, meta)
}

// parseCaptureBool accepts the spellings a form or a capture tool sends.
func parseCaptureBool(s string) (bool, bool) {
	switch strings.ToLower(s) {
	case "on", "yes", "y":
		return true, true
	case "off", "no", "n":
		return false, true
	}
	b, err := strconv.ParseBool(s)
	return b, err == nil
}

// titleUnlessFilename drops a title that is exactly the file's own name
// (§20 erratum 60). A capture tool whose title argument is filled from the
// filename — ShareX's `{filename}`, RePlays' and most OBS scripts' equivalent
// — says nothing the filename did not already say, and it says it WITH the
// extension, so the share page, `og:title` and the Discord card would all
// read "2026-09-04_18-22-13.mp4". Treating it as absent hands the naming back
// to the ONE fallback both upload routes share (`defaultTitle`, which strips
// the extension). Any other title is a real choice and is kept verbatim,
// extension or not.
func titleUnlessFilename(title, filename string) string {
	if title != "" && title == stripPath(filename) {
		return ""
	}
	return title
}

// writeMissingFile is the §8.3 missing_video answer, worded for the names
// this route accepts.
func writeMissingFile(w http.ResponseWriter, r *http.Request) {
	httpx.SetLogField(r, "reject", "missing_video")
	httpx.WriteError(w, http.StatusBadRequest, "missing_video",
		"a file part named file (or video) with a filename must be sent after the text fields")
}
