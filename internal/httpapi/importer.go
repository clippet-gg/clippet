package httpapi

// importer.go — the app's own upload path for a process with no listener:
// `clippet admin import <dir>` (ROADMAP #51, DESIGN.md §20 erratum 307).
//
// It is `dropIngest`'s sibling and `botIngest`'s cousin, and it exists for the
// reason both of those do: erratum 253's ingest core is the ONE place an upload
// becomes a clip, and a verb that walked a folder with a pipeline of its own
// would be a second answer to every question the core already answers — the
// quota, the ffprobe gate, the account's defaults, the title, the scratch-first
// spool, the atomic PutFile, the one transaction.
//
// What is different here, and each difference is deliberate:
//
//   - **There is no HTTP process.** `clippet admin import` runs in a container
//     of its own beside a live instance (`docker compose run --rm … app`), so
//     `Deps.Worker` is the no-op: nothing to wake. The clip is created queued,
//     exactly as an upload is, and the SERVING instance's worker claims it on
//     its next poll — the same poll that picks a job up after a restart.
//   - **It does not spend the account's hourly upload budget**
//     (`RATE_UPLOAD_PER_HOUR`). That bucket bounds what an ACCOUNT can push
//     over the network in an hour; this is an operator at the box's own command
//     line, moving a folder they already own onto their own instance, once. Two
//     honest reasons beside that one: a budget that a one-shot process mints
//     fresh on every run — which is what a per-process limiter is — is a knob
//     nobody can act on (erratum 292's lesson), and stopping an import of five
//     hundred old clips at sixty would leave the operator running it nine more
//     times. Every gate that is about this INSTANCE's health is kept: the
//     scratch precheck, MAX_UPLOAD_BYTES, the storage quota, the probe.
//   - **It classifies its refusals for an exit code** rather than for a text
//     file. The rule is the drop folder's own (erratum 296 (l)), pointed at a
//     different reader: if fixing something here and running the verb again
//     could change the answer, the refusal is this INSTANCE's and the command
//     exits non-zero; if the answer would be the same tomorrow, it is the
//     file's, it is printed on its own line, and the run is a success.

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"clippet/internal/db"
	"clippet/internal/worker"
)

// Importer is the upload path as `clippet admin import` sees it. Build one per
// process; it holds the same handler the API would have built, minus a mux.
type Importer struct{ h *handler }

// NewImporter assembles that handler. It registers no route and starts
// nothing: `routes()` is never called, so nothing here can answer a request
// even if something handed it a connection.
//
// The defaults are NewHandler's own, applied here for the same reason they are
// applied there — a Config a caller assembled by hand has zero values that §4
// gives meanings to.
func NewImporter(d Deps) *Importer {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Worker == nil {
		d.Worker = worker.NewNoop()
	}
	d.Cfg = withDefaults(d.Cfg)
	return &Importer{h: newHandler(d)}
}

// ImportSource is one file on its way in: the bytes, its name, and the size the
// filesystem reported. There is no declared content type — nobody declared one
// — so the ingest core derives it from the extension, exactly as it does for a
// dropped file or a capture tool that sent none.
//
// Body is read to EOF and never closed here; the caller opened it.
type ImportSource struct {
	Body     io.Reader
	Filename string
	Size     int64
}

// ImportResult is the clip that now exists and what it is made of. SHA256 is
// the hash of the ORIGINAL as the store computed it while writing — the same
// string that is now in `clips.source_etag`, which is how a second run over the
// same folder recognises this file without importing it again.
//
// Existing says the ingest core answered with a live clip this account already
// held, byte for byte (§20 erratum 326) — the verb's own `seen`, reached by the
// core rather than by the verb's pre-check (a clip that appeared in between).
type ImportResult struct {
	ClipID   string
	Bytes    int64
	SHA256   string
	Existing bool
}

// ImportRefusal is one file that did not become a clip.
//
// Tag is the short word the command prints (`refused <tag> <name>`) so a
// refusal can be grepped and counted; Why is the sentence beside it. Instance
// says which arm it is: true when this box, rather than this file, is the
// reason — and those are the ones that make the command exit 1, because fixing
// them and running it again is exactly what the operator should do.
type ImportRefusal struct {
	Tag      string
	Why      string
	Instance bool
}

func (e *ImportRefusal) Error() string { return e.Why }

// Import streams one file through the ingest core as `ownerID` and returns the
// clip it became. Every failure is an *ImportRefusal.
func (im *Importer) Import(ctx context.Context, ownerID string, src ImportSource) (ImportResult, error) {
	h := im.h
	// The scratch precheck, asked with the size the filesystem already
	// reported — so a file too big for the working disk is refused before a
	// byte is copied, which is what the 507 promise is on every other path.
	if free, ok := h.scratchHasRoom(declaredScratchNeed(src.Size, h.d.Cfg.ScratchMinFree)); !ok {
		h.d.Log.Warn("import: not enough scratch for the file",
			"file", src.Filename, "bytes", src.Size, "free", free)
		return ImportResult{}, &ImportRefusal{Tag: "scratch", Instance: true,
			Why: "this instance has no room on its working disk right now"}
	}
	var meta uploadMeta
	if err := h.applyDefaultsCtx(ctx, ownerID, &meta); err != nil {
		return ImportResult{}, &ImportRefusal{Tag: "database", Instance: true,
			Why: "this instance could not read the account's upload defaults"}
	}
	// No title: `defaultTitle` inside the core makes it the filename without
	// its extension, which is erratum 60's rule for every other upload and the
	// only thing anybody named this file either.
	clip, existing, err := h.ingestCore(ctx, ownerID, meta, ingestSource{
		Body:     capReader(src.Body, h.d.Cfg.MaxUploadBytes),
		Filename: src.Filename,
	})
	if err != nil {
		return ImportResult{}, im.refusal(err)
	}
	return ImportResult{ClipID: clip.ID, Bytes: clip.SourceSize, SHA256: clip.SourceETag, Existing: existing}, nil
}

// refusal turns the core's verdict into the line the operator reads and the
// exit code the shell reads, exactly as `writeIngestError` turns it into a
// status, `botIngestError` into a Discord reply and `dropIngestError` into a
// text file beside the video.
func (im *Importer) refusal(err error) error {
	var ie *ingestError
	if !errors.As(err, &ie) {
		im.h.d.Log.Warn("import: ingest failed", "err", err)
		return &ImportRefusal{Tag: "instance", Instance: true, Why: "this instance could not make a clip out of it"}
	}
	switch ie.kind {
	case ingestFailTooLarge:
		return &ImportRefusal{Tag: "too-large", Why: "it is larger than this instance accepts (MAX_UPLOAD_BYTES)"}
	case ingestFailNotVideo:
		return &ImportRefusal{Tag: "not-a-video", Why: "this instance cannot read a video or audio stream out of it"}
	case ingestFailEmpty:
		return &ImportRefusal{Tag: "empty", Why: "it is empty"}
	case ingestFailQuota:
		// The instance's, not the file's: make room or raise the quota and the
		// same file goes through. The sentence is the ONE sentence every
		// surface of the cap says.
		return &ImportRefusal{Tag: "quota", Instance: true, Why: quotaRefusal}
	case ingestFailScratch:
		return &ImportRefusal{Tag: "scratch", Instance: true, Why: "this instance has no room on its working disk right now"}
	case ingestFailStorage:
		return &ImportRefusal{Tag: "storage", Instance: true, Why: "this instance could not write it to the store"}
	case ingestFailDB:
		return &ImportRefusal{Tag: "database", Instance: true, Why: "this instance could not record it"}
	case ingestFailProbe:
		return &ImportRefusal{Tag: "probe", Instance: true, Why: "this instance could not run ffprobe on it"}
	}
	// Malformed and gone: a local file that ended before its size said it
	// would, or a cancelled run. Neither is a verdict about the video.
	im.h.d.Log.Warn("import: ingest failed", "kind", ie.kind, "err", ie)
	return &ImportRefusal{Tag: "instance", Instance: true, Why: "this instance could not make a clip out of it"}
}

// Duplicates asks the one question that makes the verb idempotent: does this
// account already own a clip whose original is exactly these bytes? It is
// answered by SIZE first (migration 0020) and the caller compares the hash only
// when a size matched — see internal/db/imports.go.
//
// It is the ingest core's own question (§20 erratum 326): the same live-clip
// predicate (dedupeMatch) through the same liveDigests, so `seen` here and the
// duplicate answer of an upload are one answer. A failed clip, an expired one
// the sweeper has not reached and a `deleting` one are not "already here".
func (im *Importer) Duplicates(ctx context.Context, ownerID string, size int64) ([]db.SourceDigest, error) {
	return im.h.liveDigests(ctx, ownerID, size)
}
