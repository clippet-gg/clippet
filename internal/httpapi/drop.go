package httpapi

// drop.go — the drop folder's half of the upload path (ROADMAP #42 (b),
// DESIGN.md §20 erratum 296).
//
// internal/drop watches the root of the media directory and decides WHICH files
// are ready; this file is what it hands them to. It is `botIngest`'s twin and
// deliberately so: both are the app's own upload with no request and no
// ResponseWriter anywhere in sight, and the whole point of erratum 253's ingest
// core is that neither of them is allowed a second copy of the pipeline.
//
// The gates the folder therefore inherits, unchanged, from `POST /api/clips`:
//
//   - the ACCOUNT's upload budget (RATE_UPLOAD_PER_HOUR), keyed exactly as the
//     upload routes key it. The folder mints no exemption — a capture tool
//     saving thirty clips a minute into it spends the same tokens it would
//     spend posting them;
//   - the scratch precheck (SCRATCH_MIN_FREE_BYTES), asked with the size the
//     filesystem already told the watcher, so a file too big for the working
//     disk is refused before a byte is copied;
//   - MAX_UPLOAD_BYTES, through the same capped reader the bot's attachment
//     travels down;
//   - the storage quota (STORAGE_BUDGET_HARD), the ffprobe gate, and the
//     account's own defaults — lifetime, link grammar and unlisted-or-not —
//     because `applyDefaultsCtx` runs here exactly as it does for a `/clip`.
//
// What comes BACK is a sentence, not a status: the watcher's only reader is a
// `<name>.failed.txt` beside the file, so every refusal that is the person's to
// act on says what to do about it and everything else is one generic line with
// the detail in the operator's log. That is `notify.SentenceX`'s reasoning
// applied to a text file instead of to a Discord message, and the sentences are
// the bot's own where they say the same thing.
//
// Two of those gates are not about the file at all, and they come back wrapped
// in `*drop.NotNow` (erratum 296 (l)): the hour's upload budget and the working
// disk's free space are facts about this INSTANCE in this minute, and the same
// file offered a minute later sails through. The watcher records a decision for
// everything else and none for those, so dropping a folder of a hundred clips
// drains at the rate the budget refills instead of stranding its tail behind a
// hundred text files nobody asked for. The rule for which arm is which: if the
// answer would still be no after the person went away and came back, it is the
// file's; otherwise it is ours.

import (
	"context"
	"errors"

	"clippet/internal/drop"
)

// dropIngest is the upload path as the drop folder's goroutine sees it: the
// bound account, its own defaults, its own budget.
//
// There is no card-ready wait here (the bot's `/clip` has one so the link it
// posts unfurls as a player): nobody is holding a message open on this path.
// The file is gone from the folder the moment the clip exists, and the library
// shows it encoding like any other upload.
func (h *handler) dropIngest(ownerID string) drop.Ingest {
	return func(ctx context.Context, src drop.Source) (drop.Result, error) {
		if ok, _ := h.uploadLimit.Allow("u:" + ownerID); !ok {
			return drop.Result{}, &drop.NotNow{Err: errors.New("this account has reached its upload limit for the hour (RATE_UPLOAD_PER_HOUR)")}
		}
		if free, ok := h.scratchHasRoom(declaredScratchNeed(src.Size, h.d.Cfg.ScratchMinFree)); !ok {
			h.d.Log.Warn("drop folder: not enough scratch for the dropped file",
				"file", src.Filename, "bytes", src.Size, "free", free)
			return drop.Result{}, &drop.NotNow{Err: errors.New("this instance has no room on its working disk right now")}
		}
		var meta uploadMeta
		if err := h.applyDefaultsCtx(ctx, ownerID, &meta); err != nil {
			// A database that could not answer this second is this instance's
			// minute too, not the file's: it is asked again rather than
			// remembered as a verdict.
			return drop.Result{}, &drop.NotNow{Err: errors.New("this instance could not read the account's upload defaults")}
		}
		// No title: `defaultTitle` inside the core makes it the filename
		// without its extension, which is erratum 60's rule for every other
		// upload and the only thing anybody typed here either.
		clip, existing, err := h.ingestCore(ctx, ownerID, meta, ingestSource{
			Body:     capReader(src.Body, h.d.Cfg.MaxUploadBytes),
			Filename: src.Filename,
		})
		if err != nil {
			return drop.Result{}, h.dropIngestError(err)
		}
		if existing {
			// The duplicate (§20 erratum 326): the file is already a clip of
			// this account. The watcher leaves it where it is and says where
			// the clip is.
			return drop.Result{ClipID: clip.ID, Existing: true, Link: h.d.URLs.Page(clip.ID)}, nil
		}
		return drop.Result{ClipID: clip.ID}, nil
	}
}

// dropIngestError turns the ingest core's verdict into the sentence that will
// be sitting in a text file beside the video, exactly as `writeIngestError`
// turns it into a status and `botIngestError` into a Discord reply.
//
// Every arm that is the PERSON's to act on says what to do; everything else is
// one generic sentence, because the alternative is a filesystem error's own
// text — which names a path, and on `fs` a path is the operator's disk layout —
// in a file anyone who can reach the folder can read. The detail is in the log.
func (h *handler) dropIngestError(err error) error {
	var ie *ingestError
	if !errors.As(err, &ie) {
		h.d.Log.Warn("drop folder: ingest failed", "err", err)
		return errors.New("this instance could not make a clip out of it")
	}
	switch ie.kind {
	case ingestFailTooLarge:
		return errors.New("it is larger than this instance accepts (MAX_UPLOAD_BYTES)")
	case ingestFailNotVideo:
		return errors.New("this instance cannot read a video or audio stream out of it")
	case ingestFailEmpty:
		return errors.New("it is empty")
	case ingestFailQuota:
		return errors.New(quotaRefusal) //nolint:staticcheck // ST1005: a sentence for a person, written verbatim beside the video
	case ingestFailScratch:
		// The same momentary fact as the precheck above, met a little later
		// with the real size: a pause, not a decision about the file.
		return &drop.NotNow{Err: errors.New("this instance has no room on its working disk right now")}
	}
	h.d.Log.Warn("drop folder: ingest failed", "kind", ie.kind, "err", ie)
	return errors.New("this instance could not make a clip out of it")
}
