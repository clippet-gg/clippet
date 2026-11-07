package transcode

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
)

// PosterAt refuses a moment that is not a moment before it spends an ffmpeg
// on it. Every one of these is a programming error rather than a request —
// the handler bounds what a person can send — so the refusal is permanent,
// and it is deliberately NOT ErrNoFrameAt: that sentence says "this file has
// no picture there", which is a fact about a file and not about a number.
func TestPosterAtRefusesAnImpossibleMoment(t *testing.T) {
	for _, seconds := range []float64{-0.001, -5, math.NaN(), math.Inf(1), math.Inf(-1)} {
		out := filepath.Join(t.TempDir(), "poster.jpg")
		_, err := PosterAt(context.Background(), "ffmpeg-that-is-never-run", "in.mp4", out, seconds, PosterLabel{})
		if err == nil {
			t.Fatalf("PosterAt(%v) succeeded; want a refusal", seconds)
		}
		if !IsPermanent(err) {
			t.Errorf("PosterAt(%v) = %v, want a permanent error: no retry makes %v a moment", seconds, err, seconds)
		}
		if errors.Is(err, ErrNoFrameAt) {
			t.Errorf("PosterAt(%v) reported ErrNoFrameAt, which is a sentence about a FILE", seconds)
		}
	}
}
