package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// The trim scrubber's filmstrip (§20 erratum 220; ROADMAP #6's open half): ONE
// JPEG holding a row of small thumbnails, evenly spaced across the whole of a
// clip's CURRENT rendition, so the owner dragging a trim handle sees the frame
// they are naming instead of a number.
//
// It is a sprite SHEET and not N objects on purpose. A scrubber wants every
// tile at once — a handle can be anywhere on the track a millisecond after the
// page loads — and twenty-four separate requests for twenty-four 8 KB images
// is twenty-four round trips, twenty-four cache entries and twenty-four rows
// in an access log, against one object the browser fetches once and positions
// with `background-position`. One object is also one key, which is what lets
// the clip delete, `admin orphans` and the trim's supersede list cover it
// without learning anything new.
//
// The layout is a single ROW, never a grid. A grid halves the pixel width of a
// wide sheet, and it costs the client a second coordinate to compute for every
// tile; a row costs one, and the widest sheet this produces (24 × 160 px) is
// 3840 pixels, which is a perfectly ordinary JPEG.
//
// The pass reads the RENDITION, not the original, for the preview GIF's
// reasons exactly (§17 #6): the rendition is normalised and bounded, and a
// filmstrip of the clip people are watching is the only honest one — the
// original may hold footage an applied cut removed.

const (
	// FilmstripTiles is how many thumbnails a filmstrip holds when the clip is
	// long enough to space them out. Twenty-four across a track that is a few
	// hundred pixels wide gives a tile every 10-20 px of travel, which is the
	// point at which dragging a handle stops feeling like guessing.
	FilmstripTiles = 24
	// FilmstripTileWidth is one tile's width in pixels. A tile is a hint, not
	// a picture: 160 px reads clearly at the size a track shows it and keeps
	// the whole sheet under a few hundred kilobytes.
	FilmstripTileWidth = 160
	// FilmstripMinIntervalMS is the closest two tiles may be in clip time. A
	// clip shorter than FilmstripTiles × this gets proportionally fewer tiles
	// rather than the same frame repeated: two tiles 20 ms apart are the same
	// picture twice, which is a wider file and no more information.
	FilmstripMinIntervalMS = 100
	// FilmstripQuality is the JPEG quantiser (ffmpeg `-q:v`, 2 = best). Five is
	// a step below the poster's 3: nothing on a 160 px tile survives the
	// difference, and the sheet holds two dozen of them.
	FilmstripQuality = 5
	// FilmstripMaxBytes is the "this is not a filmstrip" line. A 3840 × 284
	// sheet at quality 5 is a couple of hundred kilobytes; four megabytes can
	// only be reached by an input the bounds above did not predict, and the
	// run is failed permanently rather than published.
	FilmstripMaxBytes = 4 << 20
	// filmstripScaleFlags matches the rendition's own scaler (§10.3).
	filmstripScaleFlags = "lanczos"
	// FilmstripHardTimeout bounds the pass. Unlike the poster's single seek
	// this one DECODES the rendition — `fps` samples a decoded stream, and a
	// keyframe-only decode would put the same picture in several tiles of a
	// short clip, which is a worse filmstrip than a slower one. A clip's
	// rendition decodes in seconds; three minutes is the "this is not
	// happening" line, past which the job fails and the scrubber keeps its
	// plain track (DEPLOY §22).
	FilmstripHardTimeout = 180 * time.Second
	// filmstripFallbackTiles / filmstripFallbackIntervalMS are what an
	// UNMEASURED rendition gets: a row's worth of tiles a second apart. A ready
	// clip always carries a duration (FinalizeTranscode writes the whole set)
	// and the job skips a clip that does not, so this is the shape of a
	// hand-written row rather than of anything the server produces — it exists
	// so the plan is total and bounded whatever it is handed.
	filmstripFallbackTiles      = 12
	filmstripFallbackIntervalMS = 1000
)

// ErrNoTiles is the one CLIENT-class failure of a filmstrip: ffmpeg read the
// file and the tile filter emitted no complete sheet, which means there was no
// decodable picture in it at all. It is ErrNoFrameAt's sibling — a file that
// does not hold what the request needs, never this server's fault — so a run
// that meets it fails permanently at INFO and the scrubber keeps its plain
// track.
var ErrNoTiles = errors.New("no thumbnails could be taken from this clip")

// FilmstripPlan is the sheet one rendition gets: how many tiles, how wide each
// one is, and how much clip time one tile covers. It is pure and table-tested;
// nothing about a filmstrip depends on anything an uploader says.
type FilmstripPlan struct {
	Tiles      int   // tiles across (the sheet is always one row)
	TileWidth  int   // px; the height follows from the rendition's aspect
	IntervalMS int64 // what one tile covers, on the RENDITION's timeline
}

// PlanFilmstrip derives the sheet from the rendition's measured duration (ms)
// and width.
//
// Two rules and nothing else. Tiles are spaced so the LAST one still lies
// inside the clip — tile i is the frame at i × IntervalMS, and
// (Tiles−1) × IntervalMS is strictly below the duration by construction — and
// a clip too short to hold FilmstripTiles of them at
// FilmstripMinIntervalMS gets fewer tiles rather than duplicates. A rendition
// narrower than FilmstripTileWidth keeps its own width: a filmstrip is never
// upscaled, for the preview GIF's reason.
func PlanFilmstrip(durationMS int64, width int) FilmstripPlan {
	p := FilmstripPlan{Tiles: FilmstripTiles, TileWidth: FilmstripTileWidth}
	if durationMS <= 0 {
		p.Tiles, p.IntervalMS = filmstripFallbackTiles, filmstripFallbackIntervalMS
	} else {
		if room := int(durationMS / FilmstripMinIntervalMS); room < p.Tiles {
			p.Tiles = room
		}
		if p.Tiles < 1 {
			p.Tiles = 1
		}
		p.IntervalMS = durationMS / int64(p.Tiles)
		if p.IntervalMS < 1 {
			p.IntervalMS = 1
		}
	}
	if width > 0 && width < p.TileWidth {
		p.TileWidth = width
	}
	// An odd width would make `scale=W:-2` pick the even height of an odd
	// width — legal, but the rendition is always even (§10.3), so this only
	// ever fires on a hand-written row.
	if p.TileWidth%2 != 0 {
		p.TileWidth--
	}
	if p.TileWidth < 2 {
		p.TileWidth = 2
	}
	return p
}

// SheetWidth is how wide the produced JPEG must be. It is the assertion the
// verify step makes: `scale=W:-2` emits exactly W and `tile=Nx1` emits exactly
// N of them, so anything else came out of a file we did not write.
func (p FilmstripPlan) SheetWidth() int { return p.Tiles * p.TileWidth }

// filterChain samples one frame per IntervalMS, scales it to the tile width on
// an even height, and lays Tiles of them out in a single row.
//
// The sampling rate is written as a RATIONAL — tiles per interval — rather than
// as a decimal, so the times ffmpeg picks are exactly i × IntervalMS with no
// rounding of our own in front of it: `fps=1000/250` is four frames a second
// and says so, while `fps=4.000000` is the same number one float conversion
// later.
//
// `tile` pads an incomplete last sheet with its default colour, which is why a
// rendition whose picture stops before its container does still yields a sheet
// of the planned width (with dark tiles at the end) rather than nothing at all.
func (p FilmstripPlan) filterChain() string {
	return "fps=1000/" + strconv.FormatInt(p.IntervalMS, 10) +
		",scale=" + strconv.Itoa(p.TileWidth) + ":-2:flags=" + filmstripScaleFlags +
		",tile=" + strconv.Itoa(p.Tiles) + "x1"
}

// Args renders the one pass. `-frames:v 1` takes the FIRST complete sheet the
// tile filter emits — the one holding tiles 0..Tiles−1 — and stops, so a long
// rendition costs the decode it costs and not one frame more.
func (p FilmstripPlan) Args(in, out string) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-y"}
	args = append(args, inputArgs(in)...)
	return append(args,
		"-map", "0:v:0", "-an", "-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1",
		"-vf", p.filterChain(),
		"-frames:v", "1", "-update", "1", "-q:v", strconv.Itoa(FilmstripQuality), "-f", "image2", out,
	)
}

// FilmstripResult is the MEASURED description of a produced sheet: the numbers
// the client positions tiles from. Every one is read back out of the JPEG
// rather than copied from the plan, for poster_width/poster_height's reason
// (§20 erratum 92) — a scrubber that placed tiles by a plan the encoder did not
// follow would show the wrong frame under the handle.
type FilmstripResult struct {
	Tiles        int
	TileW, TileH int
	IntervalMS   int64
	Bytes        int64
}

// MakeFilmstrip runs the pass and measures its output.
//
// It carries no stall watchdog and no progress reporting, for CopyTrim's and
// the poster's reason: the pass emits ONE output frame, so there is nothing to
// watch tick. Its bound is the caller's context, which the runner gives a hard
// timeout of its own (FilmstripHardTimeout).
func MakeFilmstrip(ctx context.Context, ffmpeg string, p FilmstripPlan, in, out string) (FilmstripResult, error) {
	// The sheet is written beside its destination and moved onto it only once
	// it has been measured, exactly as Poster does: a JPEG that does not decode
	// never becomes a filmstrip, and a stale file from a previous attempt is
	// never mistaken for this one's.
	tmp := out + ".cand"
	_ = os.Remove(out)
	_ = os.Remove(tmp)
	defer os.Remove(tmp)

	_, se, runErr := runCapture(ctx, ffmpeg, p.Args(in, tmp)...)
	if ctx.Err() != nil {
		return FilmstripResult{}, ctx.Err()
	}
	if runErr != nil {
		// ffmpeg writes nothing at all when the tile filter never fills a
		// sheet, which is the ordinary way this fails and is the FILE's, not
		// ours. The stderr tail rides along for the log; nobody outside the
		// operator ever reads it.
		return FilmstripResult{}, &ErrPermanent{Err: fmt.Errorf("%w: %s", ErrNoTiles, exitReason("ffmpeg", runErr, se))}
	}
	res, err := MeasureFilmstrip(tmp, p)
	if err != nil {
		return FilmstripResult{}, err
	}
	if err := os.Rename(tmp, out); err != nil {
		return FilmstripResult{}, fmt.Errorf("filmstrip unmovable: %w", err)
	}
	return res, nil
}

// MeasureFilmstrip decodes the produced sheet and applies the verify-stage
// assertions: it must be a JPEG, it must be exactly as wide as Tiles tiles of
// the planned width, and it must be inside the byte budget. Any mismatch is a
// permanent bad_output — our own output, which cannot be wrong by design (§12)
// — and never a retry, because a second identical pass produces the same file.
//
// It is exported so a test can measure a sheet without re-running ffmpeg.
func MeasureFilmstrip(path string, p FilmstripPlan) (FilmstripResult, error) {
	st, err := os.Stat(path)
	if err != nil {
		return FilmstripResult{}, &ErrPermanent{Err: fmt.Errorf("bad_output: the filmstrip is unreadable: %w", err)}
	}
	if st.Size() > FilmstripMaxBytes {
		return FilmstripResult{}, &ErrPermanent{Err: fmt.Errorf(
			"output_too_large: the filmstrip is %d bytes, past the %d budgeted for it", st.Size(), FilmstripMaxBytes)}
	}
	sheet, err := jpegStats(path)
	if err != nil {
		return FilmstripResult{}, &ErrPermanent{Err: fmt.Errorf("bad_output: %w", err)}
	}
	if sheet.Width != p.SheetWidth() {
		return FilmstripResult{}, &ErrPermanent{Err: fmt.Errorf(
			"bad_output: the filmstrip is %d px wide, want %d tiles of %d", sheet.Width, p.Tiles, p.TileWidth)}
	}
	if sheet.Height <= 0 {
		return FilmstripResult{}, &ErrPermanent{Err: fmt.Errorf("bad_output: the filmstrip is %d px tall", sheet.Height)}
	}
	return FilmstripResult{Tiles: p.Tiles, TileW: p.TileWidth, TileH: sheet.Height,
		IntervalMS: p.IntervalMS, Bytes: st.Size()}, nil
}
