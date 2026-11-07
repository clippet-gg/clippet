package transcode

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
)

// The card image the OWNER chose (ROADMAP #5, §20 errata 127-129).
//
// Poster (poster.go) is a SEARCH: a thumbnail pass over a bounded window, a
// seek ladder behind it, and a near-black test that rejects candidates until
// one is worth putting on a card. Every part of that exists because nobody
// said which frame they wanted.
//
// PosterAt is the opposite, and its whole design is the absence of those
// three things. Somebody said which frame they wanted:
//
//   - there is no ladder. A fallback would publish a DIFFERENT frame under
//     the answer "your frame was used", which is worse than a plain refusal
//     the owner can act on by picking again;
//   - there is no near-black rejection. "This frame is too dark to be a card
//     image" is a judgement, and it is not this program's to make about a
//     decision a person took while looking at the picture. The result still
//     REPORTS NearBlack, so the run can log it and the operator can see why
//     a card looks the way it does;
//   - the seek is exact and the answer is the timestamp that was asked for,
//     so the value the clip stores and the frame it shows are the same fact.
//
// What it keeps from Poster is the measurement: the JPEG is decoded, and its
// real dimensions (which og:image:width/height read) come from the same pass
// that proves there is a picture in it at all.

// ErrNoFrameAt is the one CLIENT-class failure of a chosen frame: ffmpeg
// seeked past the last picture in the file and wrote nothing. It is the
// poster's version of erratum 78's ErrWindowEmpty — a request and a file
// that disagree, which is a 4xx-shaped fact and never this server's fault —
// so a run that meets it fails permanently at INFO with a plain sentence and
// no ffmpeg text anywhere near the owner.
var ErrNoFrameAt = errors.New("no picture at the requested moment")

// PosterAt writes the frame at `seconds` of `in` to `out` and measures it.
//
// `seconds` is on the timeline of the FILE it is given — the rendition — so
// a caller holding a choice on the original's timeline subtracts the cut's
// start before calling. A negative, NaN or infinite value is a programming
// error and is refused permanently rather than silently seeking to zero.
//
// `label` is the CLIP's duration, burned into the frame's top-right corner (§20
// erratum 287) — the clip's, not this moment's, because the label answers "how
// long is this clip" and a frame chosen at 0:03 of a 0:42 clip is still a frame
// of a 0:42 clip. It is drawn after the measurement, for the reason Poster draws
// it after the near-black test, and it never fails the pass.
func PosterAt(ctx context.Context, ffmpeg, in, out string, seconds float64, label PosterLabel) (PosterResult, error) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return PosterResult{}, permanent("poster_failed: %v is not a moment in this clip", seconds)
	}
	// The candidate is rendered beside the poster and moved onto it only
	// once it has been measured, exactly as Poster does: a frame that does
	// not decode never becomes the card image, and a stale file from a
	// previous attempt is never mistaken for this one's.
	tmp := out + ".cand"
	_ = os.Remove(out)
	_ = os.Remove(tmp)
	defer os.Remove(tmp)

	// No PosterBox (§20 erratum 219): the chosen frame is taken from the file
	// `/video.mp4` serves, which carries no burned-in line of any kind, so there
	// is nothing here for the box to be about and the frame is written at the
	// size it was decoded at — as it always has been.
	_, se, runErr := runCapture(ctx, ffmpeg, PosterArgs(in, tmp, seconds, PosterBox{})...)
	if ctx.Err() != nil {
		return PosterResult{}, ctx.Err()
	}
	if runErr != nil {
		// ffmpeg writes nothing at all for a seek past the last frame, which
		// is the ordinary way this fails and is the client's, not ours. The
		// stderr tail rides along for the LOG; the owner reads the sentence
		// ErrNoFrameAt carries and never this.
		return PosterResult{}, &ErrPermanent{Err: fmt.Errorf("%w at %.3fs: %s", ErrNoFrameAt, seconds, exitReason("ffmpeg", runErr, se))}
	}
	frame, err := jpegStats(tmp)
	if err != nil {
		return PosterResult{}, &ErrPermanent{Err: fmt.Errorf("poster_failed: %w", err)}
	}
	if err := os.Rename(tmp, out); err != nil {
		return PosterResult{}, fmt.Errorf("poster unmovable: %w", err)
	}
	drawPosterLabel(ctx, ffmpeg, out, label, frame.Height)
	return PosterResult{Width: frame.Width, Height: frame.Height, Seconds: seconds, NearBlack: frame.nearBlack()}, nil
}
