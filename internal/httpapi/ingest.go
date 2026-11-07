package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clippet/internal/db"
	"clippet/internal/scratch"
	"clippet/internal/store"
	"clippet/internal/transcode"
)

// The ingest core (DESIGN.md §8.3 steps 5-9; §20 erratum 253).
//
// Everything an upload does once its bytes are readable — draw an id, spool to
// scratch, gate on ffprobe, PutFile, one transaction, hand the spool to the
// worker, wake it — lives here, in ONE function that knows nothing about HTTP.
// Three callers reach it and they must not be able to drift apart:
//
//   - POST /api/clips (upload.go `ingest`) — the browser's multipart body,
//   - POST /api/upload (upload_capture.go) — the capture tool's, through the
//     same `ingest`,
//   - the Discord bot's `/clip` (discord.go `botIngest`) — an attachment
//     fetched from Discord's CDN, with no request and no ResponseWriter
//     anywhere in sight.
//
// The split is by RESPONSIBILITY, not by convenience: the core decides what
// went wrong and says so in a typed `*ingestError`; the caller decides what to
// tell whoever is listening. For the HTTP routes that mapping is
// `writeIngestError`, which writes exactly the statuses, codes, messages and
// access-log fields those routes have always written; for the bot it is one
// plain sentence in a Discord message.
//
// What is NOT here, deliberately: the multipart grammar, the Content-Length
// and MAX_UPLOAD_BYTES gates and the scratch precheck (`beginUpload`, which is
// HTTP by nature — MaxBytesReader needs the ResponseWriter), and the metadata
// defaults (`applyDefaults`, which both grammars run before they get here).

// ingestKind classifies a failure of the core. The zero value is not used: a
// kind is always set, so a `switch` with no default cannot silently write a
// 200.
type ingestKind int

const (
	ingestFailScratch     ingestKind = iota + 1 // the scratch disk failed → 507
	ingestFailTooLarge                          // MaxBytesReader tripped → 413
	ingestFailEmpty                             // zero bytes → 400 empty_video
	ingestFailMalformed                         // the body ended mid-part → 400
	ingestFailGone                              // the client (or the fetch) went away → 400
	ingestFailNotVideo                          // ffprobe says it is not one → 415
	ingestFailProbe                             // ffprobe could not run → 503
	ingestFailStorage                           // the bucket failed → 503
	ingestFailDB                                // the database failed → 503
	ingestFailIDCollision                       // an id was taken, or none could be drawn → 500
	ingestFailQuota                             // the instance is at its storage quota → 507
)

// ingestError is one failure of the core: a class, the cause, and (for the two
// id_collision shapes, which differ only in their wording) the message the
// HTTP routes have always used.
type ingestError struct {
	kind    ingestKind
	err     error
	message string
}

func (e *ingestError) Error() string {
	if e.err == nil {
		return e.message
	}
	return e.err.Error()
}

func (e *ingestError) Unwrap() error { return e.err }

// failIngest builds an ingestError. A nil cause is legal: `empty_video` and
// the exhausted id loop have no error under them, only a verdict.
func failIngest(kind ingestKind, err error, message string) *ingestError {
	return &ingestError{kind: kind, err: err, message: message}
}

// ingestSource is one upload's bytes plus the two facts that come from
// whatever framing carried them: the client's filename and its declared
// content type (either may be empty — `store.SanitizeExt` and
// `store.ContentTypeFor` decide what that means).
//
// Body is read to EOF and never closed here; whoever opened it closes it.
//
// Spooled is the second door (§20 erratum 344): the path of a file that is
// already whole on scratch — the chunked upload's assembled file — used INSTEAD
// of Body. The core hard-links it into the spool (a copy where the filesystem
// cannot link) and hashes it there, and from that point the upload is exactly
// the one-shot's: the same checks, in the same order, by the same code. The
// file at Spooled is never moved or removed here; its owner keeps it until the
// core has answered.
type ingestSource struct {
	Body        io.Reader
	Spooled     string
	Filename    string
	ContentType string
}

// ingestCore runs §8.3 steps 5-9 for one upload and returns the created clip —
// or, when this ACCOUNT already holds a live clip of exactly these bytes, that
// clip with existing = true and nothing written anywhere (§20 erratum 326).
// Every failure is an *ingestError; nothing is written anywhere.
func (h *handler) ingestCore(ctx context.Context, ownerID string, meta uploadMeta, src ingestSource) (clip db.Clip, existing bool, err error) {
	if err := h.storageQuota(ctx); err != nil {
		return db.Clip{}, false, err
	}
	ext := store.SanitizeExt(src.Filename)
	contentType := store.ContentTypeFor(src.ContentType, ext)

	id, err := h.pickClipIDCtx(ctx, meta)
	if err != nil {
		return db.Clip{}, false, err
	}
	// Spool under a name the janitors never treat as a clip dir: no row
	// exists for id yet, and the §12 "row missing → remove" rule must not
	// be able to destroy an in-flight upload that spans a janitor tick.
	dir := scratch.UploadTmpDir(h.d.Cfg.ScratchDir, id)
	srcPath := filepath.Join(dir, sourceFilePrefix+ext)
	var size int64
	var sum string
	if src.Spooled != "" {
		size, sum, err = h.adoptSpool(src.Spooled, dir, srcPath)
	} else {
		size, sum, err = h.spoolBody(ctx, src.Body, dir, srcPath)
	}
	if err != nil {
		h.removeScratch(dir)
		return db.Clip{}, false, err
	}
	// The duplicate check (§20 erratum 326), BEFORE the probe and before any
	// byte reaches the bucket: the bytes are whole on scratch and hashed, and a
	// live clip of this account with the same original already passed every
	// gate below. The spool is discarded — no row, no object, no job, and so no
	// webhook (erratum 319 fires at the ready flip of a NEW clip).
	if !meta.allowDuplicate {
		prior, found, err := h.liveCopy(ctx, ownerID, size, sum)
		if err != nil {
			h.removeScratch(dir)
			return db.Clip{}, false, failIngest(ingestFailDB, err, "")
		}
		if found {
			h.removeScratch(dir)
			h.d.Log.Info("upload deduped", "clip", prior.ID, "bytes", size)
			return prior, true, nil
		}
	}
	probe, err := h.probeSource(ctx, srcPath)
	if err != nil {
		h.removeScratch(dir)
		return db.Clip{}, false, err
	}
	kind, facts := db.ClipKindVideo, db.AudioFacts{}
	switch {
	case probe != nil && probe.Kind == transcode.KindAudio:
		// SOUND (§20 erratum 328). What is stored is never the upload itself:
		// it is the strip — the one audio stream copied into a fresh container
		// with every tag, chapter, picture and encoder string left behind — and
		// the container the strip chose decides the key's extension and the
		// stored media type (the probe decides; the part's type was an opinion).
		st, err := h.stripAudio(ctx, probe, dir, srcPath)
		if err != nil {
			h.removeScratch(dir)
			return db.Clip{}, false, err
		}
		ext, contentType, srcPath, size, sum = st.ext, st.contentType, st.path, st.size, st.sum
		kind = db.ClipKindAudio
		facts = db.AudioFacts{Codec: firstAudioCodec(probe), SampleRate: probe.AudioSampleRate,
			BitDepth: probe.AudioBits, Channels: probe.AudioChannels}
		// The duplicate check again, on the STORED bytes (erratum 326): the strip
		// is reproducible (`-fflags +bitexact`), so two uploads of one file strip
		// to one file, and an audio clip's source_etag is the strip's hash.
		if !meta.allowDuplicate {
			prior, found, err := h.liveCopy(ctx, ownerID, size, sum)
			if err != nil {
				h.removeScratch(dir)
				return db.Clip{}, false, failIngest(ingestFailDB, err, "")
			}
			if found {
				h.removeScratch(dir)
				h.d.Log.Info("upload deduped", "clip", prior.ID, "bytes", size)
				return prior, true, nil
			}
		}
	default:
		contentType = videoContentType(ext, contentType)
	}
	keys := store.KeysFor(h.d.Cfg.S3.KeyPrefix, id, ext)
	if h.d.Store == nil {
		h.removeScratch(dir)
		return db.Clip{}, false, failIngest(ingestFailStorage, errors.New("storage not configured"), "")
	}
	res, err := h.d.Store.PutFile(ctx, keys.Original, srcPath, contentType, nil)
	if err != nil {
		h.abortUpload(keys.Prefix, dir)
		if ingestClientGone(ctx, err) {
			return db.Clip{}, false, failIngest(ingestFailGone, err, "")
		}
		h.d.Log.Warn("upload: store original", "clip", id, "err", err)
		return db.Clip{}, false, failIngest(ingestFailStorage, err, "")
	}
	if res.SHA256 != sum {
		// Two hashes of one file: the spool's (what the duplicate check
		// compares) and the store's (what `source_etag` records). They are the
		// same function over the same bytes, so a difference means the scratch
		// file changed under us — said once, and the store's is what is kept.
		h.d.Log.Warn("upload: the spool's hash and the store's differ", "clip", id)
	}
	title := meta.title
	if title == "" {
		title = defaultTitle(src.Filename)
	}
	clip, _, err = h.d.DB.CreateClipWithJob(ctx, db.NewClip{
		ID: id, OwnerID: ownerID, Title: title, Description: meta.description,
		Unlisted:  meta.unlisted,
		SourceKey: keys.Original, SourceFilename: baseName(src.Filename), SourceSize: size,
		SourceContentType: contentType, SourceETag: res.SHA256,
		TTLSeconds: meta.ttlSeconds, Tags: meta.tags, Now: h.unixNow(),
		Kind: kind, Audio: facts,
	})
	if err != nil {
		cleanup := keys.Prefix
		if errors.Is(err, db.ErrConflict) {
			// The id is taken: the prefix now belongs to the clip that won
			// it, so remove only the object this request wrote (a key is its
			// own prefix, §20.6), never the winner's objects (§20.21).
			cleanup = keys.Original
		}
		h.abortUpload(cleanup, dir)
		if errors.Is(err, db.ErrConflict) && !ingestClientGone(ctx, err) {
			return db.Clip{}, false, failIngest(ingestFailIDCollision, err, "clip id collided; retry the upload")
		}
		return db.Clip{}, false, failIngest(ingestFailDB, err, "")
	}
	// The row exists now: hand the spooled source to the worker under the
	// clip's real dir (scratch-first, §8.3). A rename failure only costs a
	// re-download from the bucket in acquire — never the upload.
	if err := os.Rename(dir, scratch.ClipDir(h.d.Cfg.ScratchDir, id)); err != nil {
		h.d.Log.Warn("upload: move spool to clip dir; worker will re-download", "clip", id, "err", err)
		h.removeScratch(dir)
	}
	h.d.Worker.Wake()
	h.d.Log.Info("upload accepted", "clip", id, "bytes", size, "content_type", contentType, "user", ownerID)
	return clip, false, nil
}

// liveCopy is the duplicate check's one question (§20 erratum 326): does THIS
// owner hold a live clip whose original is exactly these bytes? Asked by size
// first through migration 0020's (owner_id, source_size) index, then by the hex
// sha256 in Go — the same two steps `clippet admin import` takes, which asks
// through liveDigests too, so the verb and the core cannot give two answers.
//
// It is NEVER asked across accounts: there is no global hash index and no route
// that takes a hash, because an answer about another account's bytes would be
// an oracle over its unlisted clips, whose only protection is the id.
func (h *handler) liveCopy(ctx context.Context, ownerID string, size int64, sum string) (db.Clip, bool, error) {
	if sum == "" {
		return db.Clip{}, false, nil
	}
	digests, err := h.liveDigests(ctx, ownerID, size)
	if err != nil {
		return db.Clip{}, false, err
	}
	for _, d := range digests {
		if d.ETag != sum {
			continue
		}
		c, err := h.d.DB.GetClip(ctx, d.ID)
		if errors.Is(err, db.ErrNotFound) {
			continue // swept between the two reads
		}
		if err != nil {
			return db.Clip{}, false, err
		}
		if dedupeMatch(c.State, c.ExpiresAt, h.unixNow()) {
			return c, true, nil
		}
	}
	return db.Clip{}, false, nil
}

// liveDigests is ClipsWithSourceSize narrowed to the clips a duplicate may be
// answered with: dedupeMatch.
func (h *handler) liveDigests(ctx context.Context, ownerID string, size int64) ([]db.SourceDigest, error) {
	all, err := h.d.DB.ClipsWithSourceSize(ctx, ownerID, size)
	if err != nil {
		return nil, err
	}
	now := h.unixNow()
	out := all[:0]
	for _, d := range all {
		if dedupeMatch(d.State, d.ExpiresAt, now) {
			out = append(out, d)
		}
	}
	return out, nil
}

// dedupeMatch is which clip a duplicate is answered with: `queued`,
// `processing` or `ready`, and not past its expiry. `failed` never matches
// (uploading again is the person's only retry), `deleting` never matches (its
// objects are going), and a clip past its expiry that the sweeper has not
// reached yet never matches (a link that dies within SWEEP_INTERVAL would be a
// lie).
func dedupeMatch(state string, expiresAt *int64, now int64) bool {
	switch state {
	case db.ClipQueued, db.ClipProcessing, db.ClipReady:
	default:
		return false
	}
	return expiresAt == nil || *expiresAt > now
}

// storageQuota is the opt-in hard cap (STORAGE_BUDGET_HARD=1; §20 erratum
// 277): with it on, a NEW clip is refused once the bytes this database says are
// stored have reached STORAGE_BUDGET_BYTES.
//
// **This is the ONE refusal site**, and it is here rather than in beginUpload
// for two reasons that are really one: `ingestCore` is exactly the boundary
// "a clip is about to exist" — all three creating callers run through it
// (POST /api/clips, POST /api/upload, the bot's /clip) and nothing else does,
// so a replacement (replace.go, which shares beginUpload but not this function)
// and a trim are not refused by a cap about how many clips an instance holds.
// Replacing the file behind a link the friends already have is not the thing an
// operator meant to stop, and it usually makes the instance SMALLER.
//
// It runs before an id is drawn and before a byte is spooled, so a refused
// upload writes nothing to scratch and nothing to the bucket. The body is then
// left unread, which net/http answers by closing the read side after the reply
// (`doEarlyClose` + `closeWriteAndWait`), so the client reads the 507 rather
// than a reset — the same shape erratum 138 arranged for the refusals above it.
//
// **The number is read LIVE, not from the sweeper's cache** (internal/httpx's
// StorageBudget, which is at most one SWEEP_INTERVAL old and stays what it has
// always been: the advisory reading behind the WARN, /healthz and the panel).
// A cap on a cached reading would refuse for a minute after somebody deleted a
// clip to make room, which is exactly the moment it is being watched. One
// indexed sum over `clips` per upload is the price, and it is paid only when
// the cap is on: with STORAGE_BUDGET_HARD unset — the default and every
// deployment before it — this function reads nothing at all.
//
// A read that FAILS does not refuse: a database that cannot answer is already
// going to fail the insert four steps later with a 503 that says so, and
// turning it into "you are out of space" would send the operator to the wrong
// problem. It is logged and the upload proceeds.
func (h *handler) storageQuota(ctx context.Context) error {
	limit := h.d.Cfg.StorageBudgetBytes
	if !h.d.Cfg.StorageBudgetHard || limit <= 0 {
		return nil
	}
	used, err := h.d.DB.StorageUsed(ctx)
	if err != nil {
		h.d.Log.Warn("upload: could not read the storage quota; the upload is allowed", "err", err)
		return nil
	}
	// AT the line, not one byte past it: a budget of N bytes is N bytes of
	// room, and an instance holding exactly N has none left. (The advisory
	// WARN keeps erratum 157's `used > budget` edge — it fires when the line
	// has been crossed, and /healthz's `over` is what DEPLOY §24's alerts
	// read. The two are one byte apart and each is right about its own
	// question.)
	if used < limit {
		return nil
	}
	h.d.Log.Warn("upload refused: the instance is at its storage quota",
		"used_bytes", used, "budget_bytes", limit)
	return failIngest(ingestFailQuota, nil, quotaRefusal)
}

// quotaRefusal is the one sentence every surface of the cap says — the HTTP
// error message, the Discord bot's reply and the log line's human half — so a
// person who hits it is told the same thing wherever they hit it, and is told
// what to DO about it in the same breath.
const quotaRefusal = "This instance is at its storage quota; delete or expire some clips, or raise the quota."

// pickClipIDCtx draws an id that does not exist yet (§7 collision loop), in the
// grammar THIS upload resolved. It runs after `applyDefaultsCtx`, so `meta`
// already carries the settled `linkStyle` and the settled `unlisted` — which is
// the whole reason the id is minted this late (§20 erratum 250), and what makes
// the Discord bot's `/clip` mint a four-word id when the bound account defaults
// to unlisted, exactly as the web upload does.
func (h *handler) pickClipIDCtx(ctx context.Context, meta uploadMeta) (string, error) {
	for i := 0; i < idPickAttempts; i++ {
		id := newClipID(meta.linkStyle, meta.unlisted)
		exists, err := h.d.DB.ClipExists(ctx, id)
		if err != nil {
			return "", failIngest(ingestFailDB, err, "")
		}
		if !exists {
			return id, nil
		}
	}
	return "", failIngest(ingestFailIDCollision, nil, "could not allocate a clip id; retry the upload")
}

// spoolBody streams body into dst and fsyncs it, mapping the failure classes
// of §8.3 step 5 onto ingestKinds: MaxBytesReader overflow → too_large, disk →
// scratch, zero bytes → empty_video, the reader going away → gone, and a body
// that is merely malformed → 400 invalid_request (§20 erratum 53).
//
// The bytes are hashed as they are written (§20 erratum 326): sum is the hex
// sha256 of exactly what is on scratch, the same function the store applies in
// PutFile, so the duplicate check costs no second read.
func (h *handler) spoolBody(ctx context.Context, body io.Reader, dir, dst string) (n int64, sum string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, "", h.scratchFailed(err)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, "", h.scratchFailed(err)
	}
	hash := sha256.New()
	// The disk first: io.MultiWriter stops at the first writer that fails, and
	// a disk error must surface as one (diskWriter tags it).
	n, err = io.Copy(io.MultiWriter(diskWriter{f}, hash), body)
	if err != nil {
		_ = f.Close()
		var mbe *http.MaxBytesError
		var de *diskError
		switch {
		case errors.As(err, &mbe):
			return 0, "", failIngest(ingestFailTooLarge, err, "")
		case errors.As(err, &de):
			return 0, "", h.scratchFailed(err)
		case ingestClientGone(ctx, err):
			return 0, "", failIngest(ingestFailGone, err, "")
		default:
			// The body arrived whole but does not parse: a part that never
			// reached its closing boundary makes mime/multipart answer
			// io.ErrUnexpectedEOF. The client is still on the connection and
			// reads this reply, so it must not be told it disconnected.
			return 0, "", failIngest(ingestFailMalformed, err, "")
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, "", h.scratchFailed(err)
	}
	if err := f.Close(); err != nil {
		return 0, "", h.scratchFailed(err)
	}
	if n == 0 {
		return 0, "", failIngest(ingestFailEmpty, nil, "")
	}
	return n, hex.EncodeToString(hash.Sum(nil)), nil
}

// adoptSpool is spoolBody's twin for a file already whole on scratch (§20
// erratum 344): the spool's `source.{ext}` becomes a hard link to it — no byte
// is copied, and the original name stays its owner's — or, on a filesystem that
// cannot link, a copy. Then the two numbers spoolBody returns: the size and the
// hex sha256, read once off the disk. Zero bytes is empty_video, as it is there.
func (h *handler) adoptSpool(spooled, dir, dst string) (int64, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, "", h.scratchFailed(err)
	}
	if err := os.Link(spooled, dst); err != nil {
		if err := copyFile(spooled, dst); err != nil {
			return 0, "", h.scratchFailed(err)
		}
	}
	n, sum, err := hashFile(dst)
	if err != nil {
		return 0, "", h.scratchFailed(err)
	}
	if n == 0 {
		return 0, "", failIngest(ingestFailEmpty, nil, "")
	}
	return n, sum, nil
}

// copyFile is adoptSpool's fallback: src copied to a new dst, synced.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// scratchFailed reports a scratch disk error (WARN: environment).
func (h *handler) scratchFailed(err error) error {
	h.d.Log.Warn("upload: scratch write failed", "dir", h.d.Cfg.ScratchDir, "err", err)
	return failIngest(ingestFailScratch, err, "")
}

// probeSource runs the injected ffprobe (30s) and rejects anything without a
// real video stream — nothing reaches the bucket (§8.3 step 6). A bad file is a
// normal input: INFO, never an error. Only file-at-fault failures
// (transcode.ErrPermanent) earn the "not a video" verdict (§11): a disconnect
// is `gone` like the neighbouring steps, and environmental trouble (an ffprobe
// that will not exec, the probe budget on a wedged scratch disk) is a 503 with
// a WARN — never a permanent-sounding verdict on a good file.
//
// It returns the probe so the core can tell sound from picture (§20 erratum
// 328): an AUDIO result carries VideoIndex -1 by construction and is the one
// such result that is not refused. nil with a nil error means no prober is
// wired (a test), and the upload is then treated as the video it always was.
func (h *handler) probeSource(ctx context.Context, path string) (*transcode.ProbeResult, error) {
	if h.d.Probe == nil {
		return nil, nil
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	res, err := h.d.Probe(pctx, path)
	if err == nil && (res == nil || (res.Kind != transcode.KindAudio && res.VideoIndex < 0)) {
		err = &transcode.ErrPermanent{Err: errors.New("no video stream")}
	}
	switch {
	case err == nil:
		return res, nil
	case ingestClientGone(ctx, err):
		return nil, failIngest(ingestFailGone, err, "")
	case transcode.IsPermanent(err):
		return nil, failIngest(ingestFailNotVideo, err, "")
	default:
		h.d.Log.Warn("upload: probe failed", "err", err)
		return nil, failIngest(ingestFailProbe, err, "")
	}
}

// stripTimeout bounds the ingest's strip of one audio upload: a `-c copy`, a
// probe and (on a lossless path) two decodes of the file — seconds for an hour
// of CD audio, and far under it for anything a person records.
const stripTimeout = 10 * time.Minute

// strippedSource is what the strip left on scratch: the file the bucket will
// hold, under the spool's own `source.{ext}` name for the worker's hand-off.
type strippedSource struct {
	path, ext, contentType, sum string
	size                        int64
}

// videoContentType is the stored media type of an upload the probe called a
// VIDEO (§20 erratum 328). A video whose first opinion said sound — an
// `audio/…` part type, or an audio extension (`clip.m4a`) — is stored under the
// type it would have had before audio existed: the extension's VIDEO type, else
// octet-stream. Never an audio type on a video row: the direct-play branch
// serves a playable original under that type. Both the ingest and the replace
// path ask it.
func videoContentType(ext, contentType string) string {
	if !strings.HasPrefix(contentType, "audio/") {
		return contentType
	}
	if ct := store.ContentTypeFor("", ext); strings.HasPrefix(ct, "video/") {
		return ct
	}
	return "application/octet-stream"
}

// stripAudio runs the strip on a spooled audio upload (§20 erratum 328) and
// puts its output where the raw upload was. The raw bytes are removed the
// moment the strip is proven — they are never stored, and never served.
//
// The scratch room is asked for first, because the two files coexist for the
// length of the copy and a disk that fills mid-copy would otherwise make ffmpeg
// exit non-zero, which the gate reads as the FILE's fault: a 415 for an
// upload that was fine. A refusal here is the scratch class (507), the same one
// beginUpload's precheck gives.
func (h *handler) stripAudio(ctx context.Context, p *transcode.ProbeResult, dir, raw string) (strippedSource, error) {
	if h.d.Strip == nil {
		h.d.Log.Warn("upload: an audio upload arrived and no strip is wired; refused")
		return strippedSource{}, failIngest(ingestFailProbe, errors.New("audio strip not configured"), "")
	}
	t, ok := transcode.StripTargetFor(p)
	if !ok {
		return strippedSource{}, failIngest(ingestFailNotVideo,
			&transcode.ErrPermanent{Err: errors.New("no container to strip into")}, "")
	}
	if h.d.FreeBytes != nil {
		if st, err := os.Stat(raw); err == nil {
			need := st.Size() + h.d.Cfg.ScratchMinFree
			if free, ferr := h.d.FreeBytes(dir); ferr == nil && free >= 0 && free < need {
				return strippedSource{}, h.scratchFailed(errors.New("no scratch room to strip the upload"))
			}
		}
	}
	out := filepath.Join(dir, "stripped."+t.Ext)
	sctx, cancel := context.WithTimeout(ctx, stripTimeout)
	defer cancel()
	if _, err := h.d.Strip(sctx, p, raw, out); err != nil {
		switch {
		case ingestClientGone(ctx, err):
			return strippedSource{}, failIngest(ingestFailGone, err, "")
		case transcode.IsPermanent(err):
			h.d.Log.Info("upload: audio refused by the strip", "err", err)
			return strippedSource{}, failIngest(ingestFailNotVideo, err, "")
		default:
			h.d.Log.Warn("upload: audio strip failed", "err", err)
			return strippedSource{}, failIngest(ingestFailProbe, err, "")
		}
	}
	if err := os.Remove(raw); err != nil && !errors.Is(err, os.ErrNotExist) {
		return strippedSource{}, h.scratchFailed(err)
	}
	final := filepath.Join(dir, sourceFilePrefix+t.Ext)
	if err := os.Rename(out, final); err != nil {
		return strippedSource{}, h.scratchFailed(err)
	}
	size, sum, err := hashFile(final)
	if err != nil {
		return strippedSource{}, h.scratchFailed(err)
	}
	return strippedSource{path: final, ext: t.Ext, contentType: t.ContentType, size: size, sum: sum}, nil
}

// hashFile is the size and hex sha256 of a file — spoolBody's two numbers, for
// a file the core wrote itself.
func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(hash.Sum(nil)), nil
}

// firstAudioCodec is the probe's one audio codec name.
func firstAudioCodec(p *transcode.ProbeResult) string {
	if p == nil || len(p.AudioCodecs) == 0 {
		return ""
	}
	return p.AudioCodecs[0]
}

// ingestClientGone reports whether err (or the context) reflects whoever was
// sending the bytes going away, rather than a backend failure. For an HTTP
// route that is the client disconnecting; for the bot it is the shutdown that
// cancelled the fetch.
func ingestClientGone(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled)
}
