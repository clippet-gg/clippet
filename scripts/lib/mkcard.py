#!/usr/bin/env python3
"""Draw web/static/card-placeholder.png (DESIGN §9.1, §20 errata 287 and 301).

This is the picture Discord puts on the card of a clip that is still encoding — and on
the card of a FAILED clip for ever, because a failed clip is never held back from a
crawler (internal/web/crawlerhold.go). It is the one card image the product draws
rather than cuts out of somebody's video.

Until erratum 287 it was a flat `#1F2833` slab with nothing on it; 287 made it the display
wordmark centred on the Kept dark ground. Since erratum 301 it is the companion mark's
card: the striped clapper LARGE on the left, the wordmark SMALL under it, on the same dark
ground, and NOTHING else — the card's own title already says `Processing…` or `Processing
failed`, and repeating that in the picture would be the one place in the product where a
message is stated twice (the owner's decision, 2026-09-15: the wordmark, no text). The mark and the word
are stacked, never side by side: web/STYLE.md's "never beside the word" is about the
header's one line, and this is the placement the mark's first design round
chose for the card (frame large, wordmark small, left edge, dark ground).

Same name, same 1200×630. Only the bytes change, so `internal/urls`'s three facts, the
oEmbed document's thumbnail dimensions and every assertion that names the file keep
working untouched.

    python scripts/lib/mkcard.py            # rewrite the file in place
    python scripts/lib/mkcard.py OUTDIR     # ...or write it somewhere else
    python scripts/lib/mkcard.py --check    # does the committed file still match this generator?

Run the first form only when the mark changes. The third is a `scripts/check.sh` stage
(DESIGN §16.1), the one mkicons.py's committed icons already have: it compares DECODED
pixel rows against a fresh draw, never the compressed bytes, which depend on the zlib
the interpreter happens to ship.

The mark's geometry, the rasteriser, the PNG writer and the PNG reader are mkicons.py's,
imported rather than copied — the two generators draw the same mark out of the same font
units, and a second copy of any of them would be the obvious place for them to drift.
"""

import math
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from mkicons import SS, decode, edges_of, encode_rows, flatten, glyph_spans, layer, mark_box, mark_layers, raster  # noqa: E402

W, H = 1200, 630            # the size every link-out card claims (internal/urls)
GROUND = (0x19, 0x18, 0x16)  # --bg, dark: the Kept screening room
TEXT = (0xEC, 0xE9, 0xE2)    # --text, dark: the mark and the letters
GREEN = (0x4C, 0xD6, 0x9A)   # --ok, dark: both spans

# `clippet` in Spline Sans 600 at the wordmark's own spacing — each glyph advanced by
# its width plus the tracking (-50 units = -0.025em) plus the GPOS pair kern. Font
# units, 2000 per em, baseline at y = 0, y DOWN (the export flipped the font's y axis
# so the path pastes into an SVG as is). Exported with fontTools from the Google Fonts
# TTF by web/STYLE.md's glyph-to-path recipe, applied to the whole word.
WORD_PATH = (
    "M591 19Q424 19 308 -52Q192 -124 132 -252Q73 -381 73 -550Q73 -726 134 -852Q196 -978 310 -1045"
    "Q424 -1112 579 -1112Q732 -1112 834 -1058Q936 -1004 986 -911Q1035 -818 1029 -702H764Q762 -747 750 -782"
    "Q738 -817 715 -840Q692 -864 658 -876Q624 -888 579 -888Q458 -889 404 -802Q349 -716 349 -555"
    "Q349 -375 412 -292Q476 -210 601 -207Q639 -206 678 -224Q718 -241 744 -278Q770 -315 768 -371H1032"
    "Q1035 -253 979 -165Q923 -77 822 -29Q722 19 591 19Z"
    "M1175 0V-1559H1452V0Z"
    "M1679 0V-1091H1955L1954 0Z"
    "M1818 -1234Q1746 -1234 1696 -1284Q1646 -1334 1646 -1412Q1646 -1490 1696 -1540Q1747 -1589 1818 -1589"
    "Q1889 -1589 1940 -1540Q1990 -1490 1990 -1412Q1990 -1334 1940 -1284Q1889 -1234 1818 -1234Z"
    "M2738 13Q2645 13 2582 -19Q2520 -51 2486 -98Q2453 -146 2443 -193L2340 -191L2456 -318Q2464 -286 2488 -260"
    "Q2512 -235 2553 -220Q2594 -206 2652 -206Q2758 -206 2821 -288Q2884 -369 2884 -550Q2884 -713 2824 -797"
    "Q2764 -881 2660 -881Q2599 -881 2556 -864Q2512 -847 2487 -824Q2462 -800 2456 -778V-856Q2464 -903 2484 -948"
    "Q2504 -994 2540 -1031Q2575 -1068 2629 -1090Q2683 -1112 2758 -1112Q2887 -1112 2977 -1042Q3067 -972 3114 -848"
    "Q3162 -723 3162 -559Q3162 -419 3129 -312Q3096 -206 3038 -134Q2980 -61 2903 -24Q2826 13 2738 13Z"
    "M2181 403V-1091H2451L2456 -929V-275L2443 -193L2456 1V403Z"
    "M3884 13Q3791 13 3728 -19Q3666 -51 3632 -98Q3599 -146 3589 -193L3486 -191L3602 -318Q3610 -286 3634 -260"
    "Q3658 -235 3699 -220Q3740 -206 3798 -206Q3904 -206 3967 -288Q4030 -369 4030 -550Q4030 -713 3970 -797"
    "Q3910 -881 3806 -881Q3745 -881 3702 -864Q3658 -847 3633 -824Q3608 -800 3602 -778V-856Q3610 -903 3630 -948"
    "Q3650 -994 3686 -1031Q3721 -1068 3775 -1090Q3829 -1112 3904 -1112Q4033 -1112 4123 -1042Q4213 -972 4260 -848"
    "Q4308 -723 4308 -559Q4308 -419 4275 -312Q4242 -206 4184 -134Q4126 -61 4049 -24Q3972 13 3884 13Z"
    "M3327 403V-1091H3597L3602 -929V-275L3589 -193L3602 1V403Z"
    "M4934 15Q4772 15 4656 -50Q4540 -116 4478 -242Q4416 -367 4416 -547Q4416 -729 4482 -855Q4547 -981 4664 -1047"
    "Q4782 -1113 4938 -1113Q5090 -1113 5196 -1054Q5301 -996 5356 -882Q5411 -767 5411 -601Q5411 -574 5410 -550"
    "Q5408 -526 5406 -493H4688Q4698 -343 4762 -273Q4825 -203 4927 -203Q5017 -203 5064 -242Q5111 -281 5123 -337"
    "H5388Q5379 -228 5320 -148Q5262 -69 5164 -27Q5065 15 4934 15Z"
    "M4806 -662H5142Q5138 -774 5088 -832Q5037 -890 4927 -890Q4806 -890 4748 -806Q4691 -723 4686 -572"
    "Q4694 -624 4723 -643Q4752 -662 4806 -662Z"
    "M5893 14Q5760 14 5686 -54Q5612 -121 5612 -255L5610 -774Q5610 -809 5628 -834Q5646 -859 5679 -867"
    "L5610 -1048V-1079L5665 -1404H5884V-334Q5884 -270 5906 -245Q5927 -220 5986 -220Q6033 -220 6069 -221"
    "Q6105 -222 6135 -225V-1Q6092 11 6025 12Q5958 14 5893 14Z"
    "M5481 -867V-1091H6132V-867Z"
)
# The advance of the whole word, in font units: the sum of the seven advances plus six
# trackings plus the kerns (the export printed it).
WORD_ADVANCE = 6204.0
# The word's span: the double p starts at 2039, and its stems run 142 → 1563 from the pp's
# own origin (the 1418 the mark's SPAN_W in mkicons.py spans), 200 units under the descender
# tips (which end 403 below the baseline) and 200 thick. The same geometry the stylesheet
# draws (§20 erratum 271); this file is where the pp outline and its span live since 301.
PP_ORIGIN = 2039.0
SPAN = dict(x1=PP_ORIGIN + 142, x2=PP_ORIGIN + 1563, y1=603.0, h=200.0)
# The word's ink box in font units: the i's dot is the top (-1589), the span's underside
# the bottom (803).
INK_TOP, INK_BOTTOM = -1589.0, SPAN["y1"] + SPAN["h"]

# The layout, in card pixels. The mark's box (1418 × ~1422 font units) stands MARK_H tall on
# the left margin; the word sits one GAP under it at WORD_SIZE, its stems' left edge on the
# mark's left edge; the two together are centred on the card's height. 340 px puts the mark
# at 54 % of the card's height — the same share of the card the 287 wordmark had of its
# width — so a thumbnail still shows a clapper over a green line; 64 px is the word at a
# third of that, a caption and not a second subject.
LEFT = 120.0
MARK_H = 340.0
GAP = 44.0
WORD_SIZE = 64.0


def geometry():
    """The two transforms (font units → SAMPLE px) and the pixel band the ink can touch."""
    x0, y0, x1, y1 = mark_box()
    mu = MARK_H / (y1 - y0)                    # one font unit of the mark, in card px
    wu = WORD_SIZE / 2000.0                     # one font unit of the word, in card px
    word_h = (INK_BOTTOM - INK_TOP) * wu
    top = (H - (MARK_H + GAP + word_h)) / 2.0
    mark_left = LEFT - x0 * mu
    mark_top = top - y0 * mu
    # The word's LEFT is its first stem's left edge: the c's ink starts at x = 73, so the
    # left margin is measured to the letters and not to the glyph box.
    word_left = LEFT - 73.0 * wu
    word_baseline = top + MARK_H + GAP - INK_TOP * wu

    def fm(x, y):
        return ((mark_left + x * mu) * SS, (mark_top + y * mu) * SS)

    def fw(x, y):
        return ((word_left + x * wu) * SS, (word_baseline + y * wu) * SS)

    right = max((x1 - x0) * mu + LEFT, word_left + WORD_ADVANCE * wu)
    band = (max(0, int(top) - 2), min(H, int(math.ceil(top + MARK_H + GAP + word_h)) + 2),
            max(0, int(LEFT) - 2), min(W, int(math.ceil(right)) + 2))
    return fm, fw, band


def index_edges(edges):
    """Bucket the edges by sample ROW so a scanline tests the active few instead of all of
    them: the word is seven glyphs of flattened quadratics, ~2,600 edges."""
    buckets = {}
    for e in edges:
        _, y0, _, y1 = e
        lo, hi = (y0, y1) if y0 < y1 else (y1, y0)
        for row in range(int(math.floor(lo)), int(math.ceil(hi)) + 1):
            buckets.setdefault(row, []).append(e)
    return buckets


def word_layers(fw):
    """The wordmark's two layers: the glyphs (non-zero winding, as a font's outlines need —
    the p's stem and bowl are two overlapping contours) and its span, a capsule."""
    buckets = index_edges(edges_of(flatten(WORD_PATH, fw)))

    def glyphs(y):
        return glyph_spans(buckets.get(int(math.floor(y)), ()), y)

    sx1, sy1 = fw(SPAN["x1"], SPAN["y1"])
    sx2, sy2 = fw(SPAN["x2"], SPAN["y1"] + SPAN["h"])
    sr = (sy2 - sy1) / 2.0
    syc = (sy1 + sy2) / 2.0

    def span(y):
        dy = y - syc
        if abs(dy) > sr:
            return []
        ext = math.sqrt(sr * sr - dy * dy)
        return [(sx1 + sr - ext, sx2 - sr + ext)]

    return [(2, glyphs), (3, span)]


def draw():
    """Return the card as H rows of W RGBA pixels."""
    fm, fw, band = geometry()
    layers = mark_layers(fm) + word_layers(fw)
    colours = {2: TEXT + (255,), 3: GREEN + (255,)}
    return raster(W, H, layers, colours, GROUND + (255,), band=band)


def path_of(root):
    return os.path.normpath(os.path.join(root, "card-placeholder.png"))


def write(root):
    out = encode_rows(W, H, draw())
    with open(path_of(root), "wb") as fh:
        fh.write(out)
    print("wrote %s (%d bytes)" % (path_of(root), len(out)))


def check(root):
    """Compare the committed card's PIXELS against a fresh draw. 0 = it matches."""
    path = path_of(root)
    t0 = time.time()
    try:
        with open(path, "rb") as fh:
            width, height, rows = decode(fh.read())
    except Exception as err:  # unreadable, truncated, or no longer this shape
        print("%s: unreadable (%s)" % (path, err))
        return 1
    want = draw()
    if (width, height) != (W, H):
        print("%s: %dx%d, want %dx%d" % (path, width, height, W, H))
        return 1
    if rows != want:
        n = sum(1 for a, b in zip(rows, want) if a != b)
        print("%s: %d of %d pixel rows differ from scripts/lib/mkcard.py -- "
              "re-run it and commit the file" % (path, n, H))
        return 1
    print("%s: matches the generator (%dx%d, %.2fs)" % (path, W, H, time.time() - t0))
    return 0


def main(argv):
    root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "web", "static")
    if argv and argv[0] == "--check":
        return check(root)
    if argv:
        root = argv[0]
    write(root)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
