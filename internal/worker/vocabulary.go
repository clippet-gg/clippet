package worker

import (
	"slices"

	"clippet/internal/db"
)

// vocabulary.go — what a PERSON who is not an operator is told about a job that
// ended badly (§20 erratum 80's fixed sentence sets, and erratum 165's mapping).
//
// It lived at the end of registry.go, whose subject is the in-memory progress
// map, because that file already carried the package's other view-facing helper
// (StagePercent). StagePercent stays there — it is genuinely about the progress
// bands — and this is a file of its own now that the "what is a person told"
// concern has three parts (nit sweep S04). A pure move: no behaviour, no
// signature and no sentence changed.

// ownerVocabulary is the FIXED sentence set each media kind's runs write into
// jobs.last_error (§20 erratum 80). One list per kind, because every kind's
// sentences name that kind's own work: telling the owner of a card image that
// "the trim failed" would send them after the wrong thing.
//
// `gif` and the three kinds nobody outside this package reads (transcode,
// social, notify) are deliberately absent — a gif's last_error is
// publicReason's redacted "stage: message", an open set by design, and the
// other three never reach a person who is not an operator.
var ownerVocabulary = map[string][]string{
	db.KindPoster:  {msgNoFrame, msgFrameMedia, msgFrameDisk, msgFrameStorage, msgFrameInternal},
	db.KindTrim:    {msgNoWindow, msgTooLarge, msgBadOutput, msgMedia, msgDisk, msgStorage, msgInternal},
	db.KindReplace: {msgReplaceMedia, msgReplaceTooLarge, msgReplaceBadOutput, msgReplaceDisk, msgReplaceStorage, msgReplaceInternal},
}

// ownerFallback is the sentence a kind falls back to when its last_error is
// not one of its own — the "nothing anybody anticipated" ending, which is
// exactly what an unrecognised text is.
var ownerFallback = map[string]string{
	db.KindPoster:  msgFrameInternal,
	db.KindTrim:    msgInternal,
	db.KindReplace: msgReplaceInternal,
	db.KindGIF:     "",
	// A filmstrip's endings reach an ANONYMOUS caller through the route's 503,
	// exactly as a gif's do, so it takes the gif's open-set treatment rather
	// than a closed vocabulary of its own: publicReason has already redacted
	// what a run wrote, and db.IsOperatorReason drops the two sentences
	// internal/db writes when it repairs a job nobody is running (§20 errata
	// 165, 220).
	db.KindFilmstrip: "",
	// A full-tier failure reaches an owner as a TIER STATE — the tier DTOs
	// render `failed`, no route reads that job's last_error — so the empty
	// string here is a DECISION and not the accident a missing key would be.
	// A future route that does read it is given silence on purpose, and has to
	// come to this line to change its mind.
	db.KindFull: "",
}

// OwnerError renders the failure text of a `failed` job of kind as the routes
// that show it to a person who is NOT an operator may say it (§20 erratum
// 165). "" means "say nothing beyond the route's own sentence".
//
// The gap it closes: internal/db writes jobs.last_error too, in the two places
// that repair a job nobody is running — boot reconciliation
// (db.MsgInterruptedByRestart) and `admin jobs unstick` (db.UnstickReason) —
// and those sentences are an operator's, written for every kind alike. Two
// crash-class restarts of a running poster job with no checkpoint are enough
// to drive it terminal (PosterMaxAttempts is 2), and GET …/poster,
// GET …/trim and GET …/replace hand job.LastError straight to the owner while
// previewGIF prints it to an ANONYMOUS caller. So a fixed vocabulary was
// leaking operator language through the two paths that do not run through
// internal/worker at all.
//
// The mapping lives here rather than in db because the vocabulary does: db
// would otherwise have to know sentences it never writes. And it is a
// vocabulary test, not a test for that one string — a kind with a closed set
// answers with its own fallback for ANY text outside the set, so the next
// operator sentence somebody puts in a job row is covered before it is
// written. A kind with no closed set (gif) is given the complementary test —
// db.IsOperatorReason, which lives beside those sentences so a third one added
// there is covered without anybody having to remember this end.
func OwnerError(kind, lastError string) string {
	if lastError == "" {
		return ""
	}
	if vocab, closed := ownerVocabulary[kind]; closed {
		if slices.Contains(vocab, lastError) {
			return lastError
		}
		return ownerFallback[kind]
	}
	if db.IsOperatorReason(lastError) {
		return ownerFallback[kind]
	}
	return lastError
}
