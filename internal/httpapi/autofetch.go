package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"clippet/internal/auth"
	"clippet/internal/autorule"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/notify"
)

// POST /in/auto/{token} — the Clippet Auto URL (§20 erratum 324; run 7 lane
// `onepress`; the owner's pick (1b): Auto does the whole job).
//
// Clippet Auto, the second Discord app, offers a button under a video. When
// the poster or a manager presses it, the app sends THIS instance a small JSON
// instruction naming that Discord attachment, and the instance fetches the
// video ITSELF — through the very fetcher `/upload` and Enrich use
// (notify.Bot.Fetch → fetchAndIngest: Discord's two CDN hosts only, https, no
// redirect, the size cap on the stream) — into the account the URL belongs
// to, and answers the share link. No video byte ever passes through the app.
//
// It is NOT a general "fetch this URL". The one field that names an address is
// `url`, and it must be an ATTACHMENT on Discord's CDN — config's allowlist
// first, then this route's own path rule, before anything else is looked at
// and before any socket: the two CDN hosts also serve avatars, icons and
// stickers, and here the URL is chosen by whoever holds the credential rather
// than signed by Discord. And the instruction acts only for a server bound to
// THIS URL's account (`admin bot bind`, or the official bot's claim).
//
// The credential is erratum 322's machinery with its own shape and scope
// (scopedCredential: `ca_`, `autofetch` exactly), so a miss is the missing
// route's answer byte for byte, an inbound URL cannot make the box fetch, and
// this URL cannot carry a file body. The route's two upload limiters run after
// the credential and are the ONLY charge: attachmentIngest is told not to
// spend the account's bucket a second time.

// maxAutoFetchBody bounds the instruction: five short fields (autorule's).
const maxAutoFetchBody = autorule.MaxBody

// maxAutoFetchFilename is the longest file name kept. A longer one is CLIPPED
// to it, never refused (§20 erratum 327): Clippet Auto passes Discord's own
// name, and a presser cannot shorten it.
const maxAutoFetchFilename = 255

// autoFetchInflightMax is how many Clippet Auto fetch jobs one account may
// have running at once (§20 erratum 327). A press is one video; two at once is
// more than a person pressing buttons makes, and a leaked URL can no longer
// start a whole hour's bucket of downloads in one burst.
const autoFetchInflightMax = 2

// The refusals' words (§20 erratum 324; the review's COPY 7).
const (
	msgAutoFetchInvalid     = "instruction must be JSON: guild_id, url, filename, content_type, size"
	msgAutoFetchHost        = "url is not a Discord attachment"
	msgAutoFetchNotBound    = "that server is not connected to this account"
	msgAutoFetchNotVideo    = "that file is not a video or audio file"
	msgAutoFetchUnavailable = "Discord is not configured on this instance"
	msgAutoFetchFetch       = "the attachment could not be fetched from Discord"
	msgAutoFetchFailed      = "the upload failed"
	msgAutoFetchRestarting  = "the instance is restarting; retry shortly"
)

// autoFetchInstruction is the whole body (§20 erratum 334: the type, its
// grammar and its decoder live in internal/autorule, the one place the hosted
// Clippet Auto encodes it from).
type autoFetchInstruction = autorule.Instruction

// clipFilename cuts a name to at most max bytes on a rune boundary, keeping a
// short extension (up to 16 bytes, the dot included) so the type a name says
// survives the cut. A name within max is returned as it is.
func clipFilename(name string, max int) string {
	if len(name) <= max {
		return name
	}
	ext := ""
	if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i <= 16 {
		ext = name[i:]
	}
	stem := name[:len(name)-len(ext)]
	cut := max - len(ext)
	for cut > 0 && !utf8.RuneStart(stem[cut]) {
		cut--
	}
	return stem[:cut] + ext
}

// discordAttachmentURL is the whole URL rule (autorule.AttachmentURL): config's
// allowlist, then the attachment path, whose last segment may not be `.` or
// `..` in any spelling.
func discordAttachmentURL(raw string) (*url.URL, bool) { return autorule.AttachmentURL(raw) }

// autofetch is the route's handler, past the credential and both limiters.
func (h *handler) autofetch(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if !isJSONRequest(r) {
		httpx.SetLogField(r, "reject", "not_json")
		httpx.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	in, err := autorule.Decode(http.MaxBytesReader(w, r.Body, maxAutoFetchBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.SetLogField(r, "reject", "too_large")
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
			return
		}
		httpx.SetLogField(r, "reject", "invalid")
		writeInvalid(w, msgAutoFetchInvalid)
		return
	}
	// The address, before anything else is looked at: no socket is ever
	// opened for a URL that is not a Discord attachment.
	u, ok := discordAttachmentURL(in.URL)
	if !ok {
		httpx.SetLogField(r, "reject", "attachment_host")
		httpx.WriteError(w, http.StatusBadRequest, "attachment_host", msgAutoFetchHost)
		return
	}
	// The server must be bound to THIS URL's account. Unbound and bound to
	// another account are one answer: the holder learns nothing about which.
	binding, err := h.d.DB.GuildBinding(r.Context(), in.GuildID)
	switch {
	case errors.Is(err, db.ErrNotFound) || (err == nil && binding.UserID != p.User.ID):
		httpx.SetLogField(r, "reject", "not_bound")
		httpx.WriteError(w, http.StatusForbidden, "not_bound", msgAutoFetchNotBound)
		return
	case err != nil:
		writeDBError(w, r, err)
		return
	}
	if !isMedia(discordAttachment{Filename: in.Filename, ContentType: in.ContentType}) {
		httpx.SetLogField(r, "reject", "not_video")
		httpx.WriteError(w, http.StatusUnsupportedMediaType, "not_video", msgAutoFetchNotVideo)
		return
	}
	if in.Size > h.d.Cfg.MaxUploadBytes {
		httpx.SetLogField(r, "reject", "too_large")
		h.writeTooLarge(w)
		return
	}
	if h.d.Bot == nil {
		httpx.SetLogField(r, "reject", "unavailable")
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", msgAutoFetchUnavailable)
		return
	}
	// The account's running jobs (§20 erratum 327): the last refusal, so a
	// refused instruction never holds a slot. The bucket's own 429 words.
	if !h.autoInflight.acquire(p.User.ID) {
		httpx.SetLogField(r, "reject", "inflight")
		w.Header().Set("Retry-After", "10")
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; retry later")
		return
	}
	_ = h.d.DB.TouchToken(r.Context(), p.TokenID, h.unixNow())

	deadline := time.Now().Add(h.d.AutoFetchBudget)
	cardWait := func() time.Duration { return max(0, min(h.captureWait(), time.Until(deadline))) }
	ch, err := h.fetchAttachment(p.User.ID, u, in, p.TokenID, false, cardWait)
	if err != nil {
		h.autoInflight.release(p.User.ID)
		httpx.SetLogField(r, "reject", "restarting")
		w.Header().Set("Retry-After", "5")
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", msgAutoFetchRestarting)
		return
	}
	// The slot is held until the JOB ends, not the request: a relay reads the
	// one outcome, frees the slot and hands it on (buffered, so a request that
	// stopped waiting leaves nothing blocked).
	relay := make(chan notify.FetchOutcome, 1)
	go func(job <-chan notify.FetchOutcome, user string) {
		out := <-job
		h.autoInflight.release(user)
		relay <- out
	}(ch, p.User.ID)
	httpx.SetLogField(r, "auto", "upload")
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case out := <-relay:
		h.writeAutoFetchOutcome(w, r, out)
	case <-timer.C:
		// Not done inside the hold: the job carries on under the Bot's own
		// budget and its clip lands in the library.
		httpx.SetLogField(r, "auto_wait", "expired")
		httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"state": "working"})
	case <-r.Context().Done():
		// The client hung up; the job finishes on its own, nobody is told.
		// The access line says 400 client_disconnected, as every route's
		// hang-up does, rather than a 200 nothing was written for (§20
		// erratum 327).
		writeClientGone(w, r)
	}
}

// fetchAttachment is the job both Clippet Auto doors run (§20 errata 324,
// 333): the bot's fetcher (Discord's two CDN hosts, https, no redirect, the
// size cap on the stream) into the account, the instruction's declared size
// as the truth. charge says whether THIS is where the account's upload bucket
// is spent: false for POST /in/auto/{token}, whose route limiters already
// charged it; true for the box's own gateway, which has no route.
func (h *handler) fetchAttachment(userID string, u *url.URL, in autoFetchInstruction, tag string, charge bool,
	cardWait func() time.Duration) (<-chan notify.FetchOutcome, error) {
	ingest := h.attachmentIngest(userID, charge, in.Duplicate == duplicateAllow, cardWait, nil)
	declared := in.Size
	return h.d.Bot.Fetch(notify.Attachment{URL: u.String(), Filename: clipFilename(in.Filename, maxAutoFetchFilename),
		ContentType: in.ContentType, Size: in.Size, Tag: tag},
		func(ctx context.Context, src notify.IngestSource) (notify.IngestResult, error) {
			// The declared size is the truth (§20 erratum 327): it chose the
			// early 413 and the scratch room, so a stream longer than it is
			// refused as too large, exactly as one past MAX_UPLOAD_BYTES is.
			src.Body = capReader(src.Body, declared)
			return ingest(ctx, src)
		})
}

// writeAutoFetchOutcome answers a job that ended inside the hold: 201 with
// /api/upload's UploadDTO (always JSON) — 200 and `"existing": true` for a
// duplicate (§20 erratum 326) — or one refusal envelope chosen by the
// sentence the fetcher or the ingest chose — never an error's own text.
func (h *handler) writeAutoFetchOutcome(w http.ResponseWriter, r *http.Request, out notify.FetchOutcome) {
	if out.Err == nil {
		clip, err := h.d.DB.GetClip(context.WithoutCancel(r.Context()), out.Result.ClipID)
		if err != nil {
			writeDBError(w, r, err)
			return
		}
		httpx.SetLogField(r, "clip", clip.ID)
		if out.Result.Existing {
			httpx.SetLogField(r, "existing", true)
		}
		httpx.WriteJSON(w, uploadStatus(out.Result.Existing), uploadDTOFor(clip, out.Result.Existing, h.d.URLs))
		return
	}
	var ie *notify.IngestError
	sentence := ""
	if errors.As(out.Err, &ie) {
		sentence = ie.Sentence
	}
	switch {
	case sentence == notify.SentenceFetch:
		httpx.SetLogField(r, "reject", "fetch")
		httpx.WriteError(w, http.StatusBadGateway, "fetch", msgAutoFetchFetch)
	case sentence == notify.SentenceTooLarge:
		httpx.SetLogField(r, "reject", "too_large")
		h.writeTooLarge(w)
	case sentence == notify.SentenceNotVideo:
		httpx.SetLogField(r, "reject", "not_video")
		httpx.WriteError(w, http.StatusUnsupportedMediaType, "not_video", msgAutoFetchNotVideo)
	case sentence == notify.SentenceFull:
		httpx.SetLogField(r, "reject", "scratch")
		httpx.WriteError(w, http.StatusInsufficientStorage, "insufficient_scratch", "not enough scratch space for this upload")
	case sentence == notify.SentenceQuota:
		httpx.SetLogField(r, "reject", "storage_quota")
		httpx.WriteError(w, http.StatusInsufficientStorage, "storage_quota", quotaRefusal)
	case errors.Is(out.Err, context.Canceled) || isGone(out.Err):
		httpx.SetLogField(r, "reject", "restarting")
		w.Header().Set("Retry-After", "5")
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", msgAutoFetchRestarting)
	default:
		httpx.SetLogField(r, "reject", "failed")
		httpx.WriteError(w, http.StatusInternalServerError, "failed", msgAutoFetchFailed)
	}
}

// SentenceBusy has no arm above (§20 erratum 327): this route's ingest is told
// not to charge the bucket (charge=false), so it cannot be chosen here; were it
// ever, it would be the generic 500, which the stub-bot test pins.

// inflight counts running jobs per key, at most max each.
type inflight struct {
	mu  sync.Mutex
	max int
	n   map[string]int
}

func newInflight(max int) *inflight { return &inflight{max: max, n: map[string]int{}} }

// acquire takes a slot for key, or reports false when key has max running.
func (f *inflight) acquire(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n[key] >= f.max {
		return false
	}
	f.n[key]++
	return true
}

// release gives one slot back; the key leaves the map at zero.
func (f *inflight) release(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n[key] <= 1 {
		delete(f.n, key)
		return
	}
	f.n[key]--
}

// isGone reports the ingest core's own word for a cancelled ingest.
func isGone(err error) bool {
	var ie *ingestError
	return errors.As(err, &ie) && ie.kind == ingestFailGone
}
