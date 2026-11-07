package transcode

import (
	"testing"
)

// PlanFilmstrip is pure arithmetic and is tested as such (§20 erratum 220). The
// two properties every row asserts are the ones the client depends on: the LAST
// tile lies inside the clip, and two tiles are never closer together than
// FilmstripMinIntervalMS — one tile per 20 ms of a short clip is the same
// picture several times, which is a wider file and no more information.
func TestPlanFilmstrip(t *testing.T) {
	for _, tc := range []struct {
		name        string
		durationMS  int64
		width       int
		tiles       int
		tileW       int
		intervalMS  int64
		sheetWidth  int
		wantNoClamp bool
	}{
		{name: "a six second 720p rendition", durationMS: 6000, width: 1280,
			tiles: 24, tileW: 160, intervalMS: 250, sheetWidth: 3840},
		{name: "a two minute rendition still gets 24", durationMS: 120_000, width: 1280,
			tiles: 24, tileW: 160, intervalMS: 5000, sheetWidth: 3840},
		{name: "a one second clip gets ten tiles a tenth apart", durationMS: 1000, width: 1280,
			tiles: 10, tileW: 160, intervalMS: 100, sheetWidth: 1600},
		{name: "a clip shorter than one interval gets one tile", durationMS: 80, width: 1280,
			tiles: 1, tileW: 160, intervalMS: 80, sheetWidth: 160},
		{name: "a narrow rendition keeps its own width", durationMS: 6000, width: 96,
			tiles: 24, tileW: 96, intervalMS: 250, sheetWidth: 2304},
		{name: "an odd width is made even", durationMS: 6000, width: 97,
			tiles: 24, tileW: 96, intervalMS: 250, sheetWidth: 2304},
		{name: "an unmeasured rendition falls back to bounded values", durationMS: 0, width: 1280,
			tiles: 12, tileW: 160, intervalMS: 1000, sheetWidth: 1920},
		{name: "a negative duration is the same fallback", durationMS: -5, width: 0,
			tiles: 12, tileW: 160, intervalMS: 1000, sheetWidth: 1920},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := PlanFilmstrip(tc.durationMS, tc.width)
			if p.Tiles != tc.tiles || p.TileWidth != tc.tileW || p.IntervalMS != tc.intervalMS {
				t.Fatalf("PlanFilmstrip(%d, %d) = %+v, want tiles=%d tileW=%d interval=%d",
					tc.durationMS, tc.width, p, tc.tiles, tc.tileW, tc.intervalMS)
			}
			if got := p.SheetWidth(); got != tc.sheetWidth {
				t.Errorf("SheetWidth = %d, want %d", got, tc.sheetWidth)
			}
			if p.Tiles > FilmstripTiles {
				t.Errorf("tiles = %d, past the %d cap", p.Tiles, FilmstripTiles)
			}
			if p.IntervalMS < 1 {
				t.Errorf("interval = %d ms", p.IntervalMS)
			}
			// The last tile is a moment the clip HAS. Everything a client does
			// with this plan reads floor(ms / interval) and clamps to tiles-1,
			// so a plan whose last tile sat past the end would put a black pad
			// under a handle naming a real frame.
			if tc.durationMS > 0 && int64(p.Tiles-1)*p.IntervalMS >= tc.durationMS {
				t.Errorf("the last tile is at %d ms of a %d ms clip",
					int64(p.Tiles-1)*p.IntervalMS, tc.durationMS)
			}
			if tc.durationMS >= int64(FilmstripTiles)*FilmstripMinIntervalMS && p.IntervalMS < FilmstripMinIntervalMS {
				t.Errorf("interval = %d ms, under the %d ms floor", p.IntervalMS, FilmstripMinIntervalMS)
			}
		})
	}
}

// TestFilmstripArgs pins the argv: ONE pass, the video stream only, the sampling
// rate as a rational (never a rounded decimal), the tile grid, and the protocol
// whitelist in front of the input it governs — the last of which is what stops a
// crafted playlist in the bucket from reaching anything but a file.
func TestFilmstripArgs(t *testing.T) {
	p := PlanFilmstrip(6000, 1280)
	args := p.Args("/scratch/x/stripsrc.mp4", "/scratch/x/strip.jpg")
	want := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-y",
		"-protocol_whitelist", "file", "-i", "/scratch/x/stripsrc.mp4",
		"-map", "0:v:0", "-an", "-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1",
		"-vf", "fps=1000/250,scale=160:-2:flags=lanczos,tile=24x1",
		"-frames:v", "1", "-update", "1", "-q:v", "5", "-f", "image2", "/scratch/x/strip.jpg",
	}
	if len(args) != len(want) {
		t.Fatalf("argv = %q\nwant %q", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q\nfull: %q", i, args[i], want[i], args)
		}
	}
}

// TestMeasureFilmstripRefusesWhatWeDidNotWrite: every mismatch on our own output
// is a permanent bad_output, never a retry — a second identical pass produces
// the same file (§12).
func TestMeasureFilmstripRefusesWhatWeDidNotWrite(t *testing.T) {
	p := PlanFilmstrip(6000, 1280)
	if _, err := MeasureFilmstrip("/no/such/sheet.jpg", p); !IsPermanent(err) {
		t.Errorf("a missing sheet: err = %v, want permanent", err)
	}
}
