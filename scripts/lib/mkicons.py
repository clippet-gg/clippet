#!/usr/bin/env python3
"""Draw the companion mark's assets (ROADMAP #27 and #43, DESIGN §20 errata 137, 240, 301, 306).

The wordmark is the logo and stays alone in the header (web/STYLE.md). This file draws the
COMPANION MARK — the striped clapper — for the surfaces the word cannot reach, and it is the
one place the mark's geometry lives:

  web/static/icon-192.png, icon-512.png    the manifest icons: the mark in paper on a rounded
                                           ink tile, transparent outside it (an installed app
                                           needs a tile; the header does not)
  web/static/icon-dev-192.png              icon-192 with the GREY span of the dev application's
                                           icon below — the same drawing, tile and size; the
                                           Discord card's thumbnail on an instance that wears
                                           INSTANCE_BADGE (§20 erratum 316)
  web/static/favicon.ico                   what `GET /favicon.ico` answers (§20 erratum 304): one
                                           ICO holding PNG entries at 16, 32 and 48 px — the same
                                           striped mark at each of them (§20 erratum 306) — ink on
                                           transparent, the span green, no tile
  docs/brand/discord/clippet-512.png,      the production Discord application icon: the mark
                     clippet-1024.png      on a full ink tile, the span green
  docs/brand/discord/clippet-dev-512.png,  the DEV application's icon: the same drawing with a
                     clippet-dev-1024.png  GREY span — the only difference, and the rule the
                                           instance badge (erratum 285) repeats
  docs/brand/discord/clippet-auto-512.png, the AUTO application's icon: the same drawing with a
                     clippet-auto-1024.png BLUE span — again the only difference
  docs/brand/mark.svg                      the unit geometry, one element per piece, striped
  web/templates/base.html, embed.html      the favicon: that SAME unit geometry with its
                                           colour-scheme rule, percent-encoded in full into the
                                           `data:` URI

The mark (eight design rounds with the owner, 2026-09-16/17):
the "mitre" frame — two stems 272 wide at the wordmark's pitch of 1146, the right one grown up to
meet a lid 230 thick lifted 9° at the left hinge — with THREE slashed 40° cuts through the lid as
HOLES in one even-odd path (never paper polygons laid over ink: round 6's hairline lesson), over
the wordmark's own green span, 1418 × 200, with the wordmark's own 200 gap under the stems. Font
units, 2000 per em, y DOWN, so every number below is the wordmark's. There is NO size rule: this
one geometry is every icon this repository draws, the tab's 16 px included (§20 erratum 306 — the
"stripes at 64 and above, a plain mitre at 32 and 16" of erratum 301 is revoked, and the plain
mitre is gone from the repository). What makes the mark read at 16 is the supersampling below,
not a second drawing.

Everything is generated rather than drawn by hand so it is reproducible and dependency-free
(zlib and struct are stdlib: no Pillow, no rasteriser, nothing to install on a host that has no
Node and no Go), and so the shape can be changed in one place.

    python scripts/lib/mkicons.py            # rewrite every file above in place
    python scripts/lib/mkicons.py OUTDIR     # ...or write the rasters and SVGs somewhere else, flat
    python scripts/lib/mkicons.py --check    # do the committed files still match this generator?

Run the first form only when the mark changes. The third is a `scripts/check.sh` stage (DESIGN
§16.1): it compares the DECODED pixels of the committed PNGs against a fresh draw, never the
compressed bytes, because those depend on the zlib the interpreter happens to ship — a version
bump would otherwise fail the gate without the mark having moved at all — and the SVGs and the two
templates' favicon URIs as text. The ICO is read the same way: its directory is walked, each
entry's PNG is DECODED and its pixels compared, and the container bytes are never compared.
"""

import math
import os
import re
import struct
import sys
import zlib
from urllib.parse import quote

INK = (0x17, 0x18, 0x1C)     # the tile: the light theme's ink (--text / --primary)
PAPER = (0xEC, 0xED, 0xEF)   # the mark on the tile: the light theme's ground (--bg)
GREEN = (0x4C, 0xD6, 0x9A)   # the span on ink: green on ink (--ok of the dark theme, i.e. --tile-ok)
GREY = (0x83, 0x7F, 0x77)    # the DEV span: the dark palette's --faint, as the embed's badged overlay
BLUE = (0x2F, 0xA8, 0xE8)    # the AUTO application's span — the blue of a hyperlink, because both of that
                             # bot's jobs end in a link; not a UI colour (owner, 2026-10-06)
SS = 4                       # supersampling factor; the edges are antialiased by averaging

# --- the mark, in font units ------------------------------------------------------------------
STEM, PITCH = 272, 1146          # the wordmark's stems and their pitch
SPAN_W, SPAN_H, GAP = 1418, 200, 200   # the span and the gap under the stems, the wordmark's own
LID, LID_LEN, TILT = 230, 1436, 9      # the lid: the p's bowl stroke thick, turned 9° at the left hinge
STEM_TOP, STEM_BOTTOM = 230, 800       # the left stem hangs from the lid to the gap
# The right stem grows up to meet the lifted lid, its top cut on the lid's own angle.
RIGHT_STEM_TOP = ((PITCH, 18), (PITCH + STEM, 5))
# The three cuts: parallelograms leaning 193 over the lid's 230 (40° off the vertical), each 104
# wide, one every 260, all inside the lid's flat run so no hole ever crosses the outline (a hole
# subpath that crosses the outline fills OUTSIDE it under even-odd).
CUT_X0, CUT_LEAN, CUT_W, CUT_PITCH, CUTS = 378, 193, 104, 260, 3
SPAN_Y = STEM_BOTTOM + GAP             # 1000: the span's top, one gap under the stems
# The viewBox every raster shares: the mark's own box (1418 wide; 1422 tall, because the lid's
# turned end rises 222 above the frame) centred in a 2400 square — chosen.html's own symbol.
BOX, BOX_X, BOX_Y = 2400, -491, -713
TILE_RADIUS = 7.0 / 32.0               # the manifest tile's corner, unchanged since the pp tile
ARC_STEPS = 24                         # segments per quarter arc / semicircle when flattening
# The mark's margin inside a FAVICON square — a sixteenth of the square on every side, which is the
# inset the two literal small mitres had before erratum 306 deleted them. Both favicons frame the
# mark by it: the ICO's entries through `favicon_map`, the `data:` SVG through `favicon_box`, so the
# tab's drawing and the file's are the same picture.
ICO_MARGIN = 1.0 / 16.0


def lid_path():
    """The lid's `d`: its outline, then the three cuts as subpaths (even-odd makes them holes)."""
    d = "M0 %d A%d %d 0 0 1 %d 0 H%d A%d %d 0 0 1 %d %d Z" % (
        LID, LID, LID, LID, LID_LEN - LID, LID, LID, LID_LEN, LID)
    for i in range(CUTS):
        x = CUT_X0 + i * CUT_PITCH
        d += " M%d %d L%d 0 L%d 0 L%d %d Z" % (x, LID, x + CUT_LEAN, x + CUT_LEAN + CUT_W, x + CUT_W, LID)
    return d


def _arc(cx, cy, r, a0, a1, steps=ARC_STEPS):
    return [(cx + r * math.cos(a0 + (a1 - a0) * k / steps), cy + r * math.sin(a0 + (a1 - a0) * k / steps))
            for k in range(steps + 1)]


def _rotate(points, deg, cx, cy):
    a = math.radians(deg)
    c, s = math.cos(a), math.sin(a)
    return [(cx + (x - cx) * c - (y - cy) * s, cy + (x - cx) * s + (y - cy) * c) for x, y in points]


def _capsule(x1, y1, x2, y2):
    r = (y2 - y1) / 2.0
    cy = (y1 + y2) / 2.0
    return _arc(x2 - r, cy, r, -math.pi / 2, math.pi / 2) + _arc(x1 + r, cy, r, math.pi / 2, 3 * math.pi / 2)


def mark_shapes():
    """The mark as [(value, [polygon, ...])] in font units, one entry per PIECE, painted in this
    order, each piece EVEN-ODD on its own: 2 = ink (the left stem; the right stem; the lid with
    its three holes), 3 = the span. One entry per piece matters: the right stem's top runs a few
    units up into the lid on purpose (chosen.html's own geometry, so the joint can never open),
    and two pieces in one even-odd fill would cancel where they overlap. `value` is the sample
    byte the rasteriser writes; the caller maps it to a colour."""
    left = [(0, STEM_TOP), (STEM, STEM_TOP), (STEM, STEM_BOTTOM), (0, STEM_BOTTOM)]
    (rx0, ry0), (rx1, ry1) = RIGHT_STEM_TOP
    right = [(rx0, ry0), (rx1, ry1), (rx1, STEM_BOTTOM), (rx0, STEM_BOTTOM)]
    outline = (_arc(LID, LID, LID, math.pi, 1.5 * math.pi)
               + _arc(LID_LEN - LID, LID, LID, 1.5 * math.pi, 2 * math.pi))
    lid = [_rotate(outline, -TILT, 0, LID)]
    for i in range(CUTS):
        x = CUT_X0 + i * CUT_PITCH
        lid.append(_rotate([(x, LID), (x + CUT_LEAN, 0), (x + CUT_LEAN + CUT_W, 0), (x + CUT_W, LID)], -TILT, 0, LID))
    span = _capsule(0, SPAN_Y, SPAN_W, SPAN_Y + SPAN_H)
    return [(2, [left]), (2, [right]), (2, lid), (3, [span])]


# --- the SVG files ----------------------------------------------------------------------------
SVG_NS = "http://www.w3.org/2000/svg"   # the namespace, which the favicon URI percent-encodes in full

# The favicon's colours: the light theme's ink and green, the dark theme's under its own rule.
FAVICON_STYLE = "<style>.p{fill:#17181C}.s{fill:#0E7C4F}@media(prefers-color-scheme:dark){.p{fill:#ECE9E2}.s{fill:#4CD69A}}</style>"


def mark_elements(fills):
    """The mark as SVG elements in UNIT space, one per piece, in paint order — the one drawing
    `docs/brand/mark.svg` and the tab's `data:` favicon are both made of (§20 erratum 306).

    `fills` gives each piece its paint attribute: `p` the ink (the two stems and the lid),
    `s` the span — a fill for the brand file, a class under a colour-scheme rule for the favicon.
    The lid is ONE `fill-rule="evenodd"` path whose three cuts are subpaths, so they are holes and
    never paper laid over ink; the stems stay their own elements because the right one runs a few
    units up into the lid on purpose and a single even-odd fill would cancel that overlap."""
    (rx0, ry0), (rx1, ry1) = RIGHT_STEM_TOP
    return [
        '<rect %s x="0" y="%d" width="%d" height="%d"/>' % (fills["p"], STEM_TOP, STEM, STEM_BOTTOM - STEM_TOP),
        '<path %s d="M%d %dL%d %dV%dH%dZ"/>' % (fills["p"], rx0, ry0, rx1, ry1, STEM_BOTTOM, rx0),
        '<path %s fill-rule="evenodd" transform="rotate(-%d 0 %d)" d="%s"/>' % (fills["p"], TILT, LID, lid_path()),
        '<rect %s x="0" y="%d" width="%d" height="%d" rx="%d"/>' % (fills["s"], SPAN_Y, SPAN_W, SPAN_H, SPAN_H // 2),
    ]


def favicon_box():
    """The favicon SVG's viewBox, `(x, y, side)`: the mark's own box centred in a square with the
    same 1/16 inset `favicon_map` gives the ICO's entries, so the tab and the file frame the mark
    identically. Whole units — half a unit in 1624 is a thousandth of a pixel at 16 px, and short
    numbers keep the percent-encoded URI short."""
    x0, y0, x1, y1 = mark_box()
    side = max(x1 - x0, y1 - y0) / (1.0 - 2.0 * ICO_MARGIN)
    return (round((x0 + x1 - side) / 2.0), round((y0 + y1 - side) / 2.0), round(side))


def favicon_svg():
    """The mark itself with its own prefers-color-scheme rule: what the tab draws, at whatever
    size the client asks for it (§20 erratum 306 — there is no small drawing any more)."""
    x, y, side = favicon_box()
    return ('<svg xmlns="%s" viewBox="%d %d %d %d">%s%s</svg>'
            % (SVG_NS, x, y, side, side, FAVICON_STYLE,
               "".join(mark_elements({"p": 'class="p"', "s": 'class="s"'}))))


def favicon_uri():
    """The favicon as the templates carry it: percent-encoded in full (`:` and `/` included, so
    no scheme literal appears in the template — check.sh's outbound-URL tripwire reads it)."""
    return "data:image/svg+xml," + quote(favicon_svg(), safe="")


def brand_svgs():
    """{filename: text} for docs/brand: the unit geometry, striped — the only mark there is. Ink is
    `currentColor` so the file takes the theme's text colour; the span is the tile green."""
    fills = {"p": 'fill="currentColor"', "s": 'fill="#%02X%02X%02X"' % GREEN}
    mark = (
        '<svg xmlns="%s" viewBox="%d %d %d %d">\n' % (SVG_NS, BOX_X, BOX_Y, BOX, BOX)
        + '  <!-- clippet, the companion mark: the striped clapper (docs/brand/README.md). Font units of the\n'
        + '       wordmark, 2000 per em: stems %d at pitch %d, the lid %d thick turned %d degrees at the left\n' % (STEM, PITCH, LID, TILT)
        + '       hinge with three cuts as holes in its own path, the span %d x %d one gap of %d under the stems. -->\n' % (SPAN_W, SPAN_H, GAP)
        + "".join("  %s\n" % el for el in mark_elements(fills))
        + '</svg>\n')
    return {"mark.svg": mark}


# --- the rasteriser ---------------------------------------------------------------------------
_TOKENS = re.compile(r"[MLQHVZA]|-?\d+(?:\.\d+)?")


def _arc_to(x0, y0, r, large, sweep, x1, y1, steps=ARC_STEPS):
    """The points of an SVG `A r r 0 <large> <sweep> x1 y1` from (x0, y0), the endpoint form
    converted to a centre and two angles (SVG 1.1 F.6.5), CIRCULAR only — every arc this file
    draws has rx == ry and no x-axis rotation, and an ellipse would be a shape nobody has asked
    the mark for. The start point is NOT repeated; the caller already holds it."""
    hx, hy = (x0 - x1) / 2.0, (y0 - y1) / 2.0
    d = hx * hx + hy * hy
    if d == 0.0:
        return []
    r = max(r, math.sqrt(d))           # F.6.6: a radius too small for the chord is grown to fit
    k = math.sqrt(max(0.0, (r * r - d) / d))
    if large == sweep:
        k = -k
    cx, cy = k * hy + (x0 + x1) / 2.0, -k * hx + (y0 + y1) / 2.0
    a0 = math.atan2(y0 - cy, x0 - cx)
    a1 = math.atan2(y1 - cy, x1 - cx)
    if sweep and a1 < a0:
        a1 += 2 * math.pi
    if not sweep and a1 > a0:
        a1 -= 2 * math.pi
    return _arc(cx, cy, r, a0, a1, steps)[1:]


def flatten(path, f):
    """Turn an SVG path (M/L/Q/H/V/A/Z, absolute) into closed polygons in sample space. f maps
    (x, y) font units → sample px. Used by mkcard.py for the wordmark's glyph outlines; the `A`
    case (circular arcs) is the one erratum 304 added to read the literal small mitres, which
    erratum 306 deleted — nothing in either generator draws an arc through this reader today."""
    polys, cur, pt, start = [], [], (0.0, 0.0), (0.0, 0.0)
    toks = _TOKENS.findall(path)
    i = 0

    def num():
        nonlocal i
        v = float(toks[i])
        i += 1
        return v

    while i < len(toks):
        c = toks[i]
        i += 1
        if c == "M":
            if len(cur) > 2:
                polys.append(cur)
            pt = (num(), num())
            start = pt
            cur = [f(*pt)]
        elif c == "L":
            pt = (num(), num())
            cur.append(f(*pt))
        elif c == "H":
            pt = (num(), pt[1])
            cur.append(f(*pt))
        elif c == "V":
            pt = (pt[0], num())
            cur.append(f(*pt))
        elif c == "Q":
            cx, cy = num(), num()
            x1, y1 = num(), num()
            x0, y0 = pt
            for k in range(1, 13):
                t = k / 12.0
                u = 1.0 - t
                cur.append(f(u * u * x0 + 2 * u * t * cx + t * t * x1,
                             u * u * y0 + 2 * u * t * cy + t * t * y1))
            pt = (x1, y1)
        elif c == "A":
            rx, _ry, _rot, large, sweep = num(), num(), num(), num(), num()
            x1, y1 = num(), num()
            cur += [f(*p) for p in _arc_to(pt[0], pt[1], rx, int(large), int(sweep), x1, y1)]
            pt = (x1, y1)
        elif c == "Z":
            if len(cur) > 2:
                polys.append(cur)
            cur = []
            pt = start
    if len(cur) > 2:
        polys.append(cur)
    return polys


def edges_of(polys):
    out = []
    for poly in polys:
        for k in range(len(poly)):
            x0, y0 = poly[k]
            x1, y1 = poly[(k + 1) % len(poly)]
            if y0 != y1:
                out.append((x0, y0, x1, y1))
    return out


def _crossings(edges, y):
    xs = []
    for x0, y0, x1, y1 in edges:
        lo, hi = (y0, y1) if y0 < y1 else (y1, y0)
        if lo <= y < hi:
            xs.append((x0 + (y - y0) * (x1 - x0) / (y1 - y0), 1 if y1 > y0 else -1))
    xs.sort()
    return xs


def glyph_spans(edges, y):
    """Non-zero winding spans on the sample row centred at y (the glyph outlines)."""
    spans, wind, at = [], 0, 0.0
    for x, d in _crossings(edges, y):
        if wind == 0:
            at = x
        wind += d
        if wind == 0:
            spans.append((at, x))
    return spans


def evenodd_spans(edges, y):
    """Even-odd spans on the sample row centred at y: the mark's rule, so the lid's cuts are holes."""
    xs = [x for x, _ in _crossings(edges, y)]
    return [(xs[k], xs[k + 1]) for k in range(0, len(xs) - 1, 2)]


def paint(row, spans, value, n):
    """Write `value` over the sample columns whose centres fall inside the spans."""
    for xa, xb in spans:
        a = max(0, int(math.ceil(xa - 0.5)))
        b = min(n, int(math.ceil(xb - 0.5)))
        if b > a:
            row[a:b] = bytes([value]) * (b - a)


def layer(value, edges, spans=evenodd_spans):
    """One layer for `raster`: (value, fn) where fn(y) is the spans to paint on sample row y."""
    return (value, lambda y: spans(edges, y))


def mark_layers(f):
    """The mark's layers in sample space, for `raster`; f maps font units → sample px."""
    return [layer(value, edges_of([[f(*p) for p in poly] for poly in polys])) for value, polys in mark_shapes()]


def mark_box():
    """The mark's bounding box in font units, (x0, y0, x1, y1): 0..1418 by about -222..1200."""
    pts = [p for _, polys in mark_shapes() for poly in polys for p in poly]
    return (min(x for x, _ in pts), min(y for _, y in pts), max(x for x, _ in pts), max(y for _, y in pts))


def raster(width, height, layers, colours, flat, band=None, tile=0.0):
    """Return height rows of width RGBA pixels, supersampled SS times per axis.

    `layers` are (value, fn) painted in order, fn(y) the spans on sample row y (`layer` above
    builds one from an edge list, even-odd unless told otherwise); `colours` maps a sample
    value to RGBA; `flat` is the RGBA of a sample no layer touched. `band` = (y0, y1, x0, x1) in
    PIXELS bounds the region any layer can reach: outside it a pixel is `flat` and costs nothing,
    which is what keeps a 1200 × 630 card under a second. `tile` > 0 paints a rounded square of
    that corner radius (in pixels) with value 1 first, and outside it a sample is `flat` — the
    manifest icons are transparent outside their tile.
    """
    n = width * SS
    y0, y1, x0, x1 = band if band else (0, height, 0, width)
    flat_px = bytes(flat)
    flat_row = flat_px * width
    radius = tile * SS
    rows = []
    k = SS * SS
    for py in range(height):
        if py < y0 or py >= y1:
            rows.append(flat_row)
            continue
        subrows = []
        for sy in range(SS):
            y = py * SS + sy + 0.5
            row = bytearray(n)
            if tile:
                if y < radius:
                    inset = radius - math.sqrt(max(0.0, radius * radius - (radius - y) ** 2))
                elif y > n - radius:
                    inset = radius - math.sqrt(max(0.0, radius * radius - (y - (n - radius)) ** 2))
                else:
                    inset = 0.0
                paint(row, [(inset, n - inset)], 1, n)
            for value, spans in layers:
                paint(row, spans(y), value, n)
            subrows.append(row)
        out = bytearray(flat_px * x0)
        for px in range(x0, x1):
            a, b = px * SS, px * SS + SS
            blk = b"".join(r[a:b] for r in subrows)
            acc = [0, 0, 0, 0]
            for v, c in colours.items():
                m = blk.count(v)
                if m:
                    for i in range(4):
                        acc[i] += c[i] * m
            m = blk.count(0)
            if m:
                for i in range(4):
                    acc[i] += flat[i] * m
            out += bytes((acc[0] // k, acc[1] // k, acc[2] // k, acc[3] // k))
        out += flat_px * (width - x1)
        rows.append(bytes(out))
    return rows


def unit_map(size):
    """f(x, y): font units → sample px for a `size` square carrying the 2400-unit box."""
    s = size * SS / float(BOX)

    def f(x, y):
        return ((x - BOX_X) * s, (y - BOX_Y) * s)

    return f


def draw(size, span=GREEN, tile=True):
    """One square raster of the mark: the manifest icon (a rounded ink tile, transparent outside
    it) or, with tile=False, the Discord application icon (the ink tile edge to edge)."""
    layers = mark_layers(unit_map(size))
    colours = {1: INK + (255,), 2: PAPER + (255,), 3: span + (255,)}
    if tile:
        return raster(size, size, layers, colours, (0, 0, 0, 0), tile=TILE_RADIUS * size)
    return raster(size, size, layers, colours, INK + (255,))


# --- the favicon: one ICO, one drawing at three sizes -------------------------------------------
#
# The favicon slot has NO TILE — the `data:` URI in the templates is ink and green on nothing, and
# a raster that put a tile behind the same mark would be a second favicon. So every entry below is
# ink on transparent with the span green, and the ink is the LIGHT theme's: an ICO cannot carry a
# colour-scheme rule the way the SVG does, and the tab strip a client renders one in is the one
# place the SVG already wins (every current browser prefers the declared `data:` URI). What asks
# for this file asks for the path unprompted — bookmark tools, link previewers, Safari's pinned
# tab fallback — and those draw it on their own chrome.
#
# Since §20 erratum 306 all three entries are the SAME striped mark through the rasteriser, framed
# by ICO_MARGIN above: 16 and 32 were the plain mitre until the owner revoked the size rule, and what
# carries the drawing down there is SS × SS supersampling, never hand-placed pixels.
#
# The sizes the file holds, smallest first — what a directory entry's `width`/`height` byte says.
ICO_SIZES = (16, 32, 48)


def favicon_map(size):
    """f(x, y): font units → sample px for the striped mark inside a favicon square — the mark's
    OWN box under ICO_MARGIN's 1/16 inset. `unit_map`'s 2400-unit box is the MANIFEST tile's
    framing, where the mark is inset because it sits on a tile; drawn to that box here every entry
    would be visibly smaller than the square it is asked for."""
    x0, y0, x1, y1 = mark_box()
    s = (size - 2.0 * size * ICO_MARGIN) * SS / max(x1 - x0, y1 - y0)

    def f(x, y):
        return ((x - x0) * s + (size * SS - (x1 - x0) * s) / 2.0,
                (y - y0) * s + (size * SS - (y1 - y0) * s) / 2.0)

    return f


def draw_favicon(size, span=GREEN):
    """One entry of the ICO: the striped mark, ink on transparent, the span green — the same
    drawing at every size (§20 erratum 306)."""
    return raster(size, size, mark_layers(favicon_map(size)),
                  {2: INK + (255,), 3: span + (255,)}, (0, 0, 0, 0))


def encode_ico(entries):
    """Return a whole ICO file for [(size, PNG bytes)]: the 6-byte ICONDIR, one 16-byte directory
    entry each, then the payloads.

    PNG-IN-ICO, and no BMP/DIB entry at all. Every current browser reads it and so does every
    Windows since Vista, it needs nothing but the PNG writer above (a DIB entry would be a second
    encoder, upside down, with an AND mask), and it is a third of the size. `planes`/`bitCount`
    are written as 1/32 — what the PNG is — although a reader of a PNG entry takes the truth from
    the IHDR; `colourCount` is 0, which means "not a palette"."""
    head = struct.pack("<HHH", 0, 1, len(entries))
    at = len(head) + 16 * len(entries)
    table, blobs = b"", b""
    for size, png in entries:
        table += struct.pack("<BBBBHHII", size & 0xFF, size & 0xFF, 0, 0, 1, 32, len(png), at)
        at += len(png)
        blobs += png
    return head + table + blobs


def decode_ico(data):
    """Return [(width, height, PNG bytes)] for an ICO this script wrote.

    As narrow as `decode`: it reads the one container shape `encode_ico` produces and refuses
    anything else, so a file that stopped being that shape is reported rather than compared as
    if it were."""
    if len(data) < 6:
        raise ValueError("too short for an ICONDIR")
    reserved, kind, count = struct.unpack("<HHH", data[:6])
    if (reserved, kind) != (0, 1):
        raise ValueError("not an icon ICONDIR: %r" % (data[:4],))
    if count == 0 or len(data) < 6 + 16 * count:
        raise ValueError("%d entries, and a directory too short for them" % count)
    out = []
    for i in range(count):
        w, h, _colours, _res, _planes, _bits, length, at = struct.unpack(
            "<BBBBHHII", data[6 + 16 * i:22 + 16 * i])
        blob = data[at:at + length]
        if len(blob) != length:
            raise ValueError("entry %d runs past the end of the file" % i)
        if blob[:8] != b"\x89PNG\r\n\x1a\n":
            raise ValueError("entry %d is not PNG-in-ICO" % i)
        out.append((w or 256, h or 256, blob))
    return out


def favicon_ico():
    """The whole `web/static/favicon.ico`, drawn."""
    return encode_ico([(size, encode_rows(size, size, draw_favicon(size))) for size in ICO_SIZES])


# --- PNG ---------------------------------------------------------------------------------------
def encode_rows(width, height, rows):
    """Return a whole PNG file for the given RGBA rows: 8-bit RGBA, no interlace, one IDAT.

    It is the only PNG WRITER in this repository, and scripts/lib/mkcard.py (the 1200x630
    link-out placeholder, DESIGN §20 erratum 287) imports it rather than carrying a second one:
    `decode` below understands exactly the shape this produces, and two writers would be two
    shapes for one reader to drift against.
    """
    raw = b"".join(b"\x00" + r for r in rows)

    def chunk(tag, data):
        return (struct.pack(">I", len(data)) + tag + data
                + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF))

    out = b"\x89PNG\r\n\x1a\n"
    out += chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0))
    out += chunk(b"IDAT", zlib.compress(raw, 9))
    out += chunk(b"IEND", b"")
    return out


def decode(data):
    """Return (width, height, rows) for a PNG this script wrote.

    Deliberately narrow: it understands the one shape `encode_rows` produces (8-bit RGBA,
    filter 0, no interlace) and refuses anything else, so a file that stopped being that
    shape is reported rather than silently compared as if it were.
    """
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError("not a PNG")
    width = height = None
    idat, i = [], 8
    while i + 8 <= len(data):
        length = struct.unpack(">I", data[i:i + 4])[0]
        tag, body = data[i + 4:i + 8], data[i + 8:i + 8 + length]
        if tag == b"IHDR":
            width, height, depth, colour, comp, filt, interlace = struct.unpack(">IIBBBBB", body)
            if (depth, colour, comp, filt, interlace) != (8, 6, 0, 0, 0):
                raise ValueError("not 8-bit RGBA/filter 0/non-interlaced: %r"
                                 % ((depth, colour, comp, filt, interlace),))
        elif tag == b"IDAT":
            idat.append(body)
        i += 12 + length
    if width is None:
        raise ValueError("no IHDR")
    raw = zlib.decompress(b"".join(idat))
    stride = width * 4
    rows = []
    for y in range(height):
        at = y * (stride + 1)
        if raw[at] != 0:
            raise ValueError("row %d uses filter %d, not 0" % (y, raw[at]))
        rows.append(raw[at + 1:at + 1 + stride])
    return width, height, rows


# --- the files ---------------------------------------------------------------------------------
ROOT = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
STATIC = os.path.join(ROOT, "web", "static")
BRAND = os.path.join(ROOT, "docs", "brand")
TEMPLATES = [os.path.join(ROOT, "web", "templates", n) for n in ("base.html", "embed.html")]
_ICON_HREF = re.compile(r'(<link rel="icon" href=")data:image/svg\+xml,[^"]*(")')


def ico_path(outdir=None):
    return os.path.join(outdir or STATIC, "favicon.ico")


def rasters(outdir=None):
    """[(path, size, span, tile)] — every PNG this generator owns, or all of them flat in outdir."""
    items = [
        (os.path.join(STATIC, "icon-192.png"), 192, GREEN, True),
        (os.path.join(STATIC, "icon-512.png"), 512, GREEN, True),
        (os.path.join(STATIC, "icon-dev-192.png"), 192, GREY, True),
        (os.path.join(BRAND, "discord", "clippet-512.png"), 512, GREEN, False),
        (os.path.join(BRAND, "discord", "clippet-1024.png"), 1024, GREEN, False),
        (os.path.join(BRAND, "discord", "clippet-dev-512.png"), 512, GREY, False),
        (os.path.join(BRAND, "discord", "clippet-dev-1024.png"), 1024, GREY, False),
        (os.path.join(BRAND, "discord", "clippet-auto-512.png"), 512, BLUE, False),
        (os.path.join(BRAND, "discord", "clippet-auto-1024.png"), 1024, BLUE, False),
    ]
    if outdir:
        items = [(os.path.join(outdir, os.path.basename(p)), s, c, t) for p, s, c, t in items]
    return items


def svgs(outdir=None):
    return {os.path.join(outdir or BRAND, name): text for name, text in brand_svgs().items()}


def _read(path):
    with open(path, "rb") as fh:
        return fh.read()


def _write(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as fh:
        fh.write(data)
    print("wrote %s (%d bytes)" % (path, len(data)))


def write(outdir=None):
    for path, size, span, tile in rasters(outdir):
        _write(path, encode_rows(size, size, draw(size, span, tile)))
    _write(ico_path(outdir), favicon_ico())
    for path, text in svgs(outdir).items():
        _write(path, text.encode("utf-8"))
    if outdir:
        return
    href = favicon_uri()
    for path in TEMPLATES:
        text = _read(path).decode("utf-8")
        new, n = _ICON_HREF.subn(lambda m: m.group(1) + href + m.group(2), text)
        if n != 1:
            print("%s: found %d favicon links, want 1 -- not touched" % (path, n))
            continue
        if new != text:
            _write(path, new.encode("utf-8"))
        else:
            print("%s: the favicon is already this generator's" % path)


def _check_ico():
    """The favicon, entry by entry: the sizes the directory claims, then the DECODED pixels of
    each PNG payload against a fresh draw. The container bytes are never compared, for the same
    reason the PNGs' are not — a different zlib would fail the gate with the mark unmoved."""
    path = ico_path()
    try:
        entries = decode_ico(_read(path))
    except (OSError, ValueError, zlib.error) as err:
        print("%s: unreadable (%s)" % (path, err))
        return 1
    got = [(w, h) for w, h, _ in entries]
    if got != [(s, s) for s in ICO_SIZES]:
        print("%s: entries %s, want %s" % (path, got, [(s, s) for s in ICO_SIZES]))
        return 1
    for (size, (_w, _h, blob)) in zip(ICO_SIZES, entries):
        try:
            width, height, rows = decode(blob)
        except (ValueError, zlib.error) as err:
            print("%s: the %d px entry is not a PNG this script wrote (%s)" % (path, size, err))
            return 1
        want = draw_favicon(size)
        if (width, height) != (size, size):
            print("%s: the %d px entry is %dx%d" % (path, size, width, height))
            return 1
        if rows != want:
            n = sum(1 for a, b in zip(rows, want) if a != b)
            print("%s: the %d px entry differs from scripts/lib/mkicons.py in %d of %d pixel rows "
                  "-- re-run it and commit every file it writes" % (path, size, n, size))
            return 1
    print("%s: matches the generator (%s px, PNG-in-ICO)"
          % (path, ", ".join(str(s) for s in ICO_SIZES)))
    return 0


def check():
    """Compare every committed file against a fresh draw. 0 = they all match."""
    bad = 0
    for path, size, span, tile in rasters():
        try:
            width, height, rows = decode(_read(path))
        except (OSError, ValueError, zlib.error) as err:
            print("%s: unreadable (%s)" % (path, err))
            bad += 1
            continue
        want = draw(size, span, tile)
        if (width, height) != (size, size):
            print("%s: %dx%d, want %dx%d" % (path, width, height, size, size))
            bad += 1
        elif rows != want:
            n = sum(1 for a, b in zip(rows, want) if a != b)
            print("%s: %d of %d pixel rows differ from scripts/lib/mkicons.py -- "
                  "re-run it and commit every file it writes" % (path, n, size))
            bad += 1
        else:
            print("%s: matches the generator (%dx%d)" % (path, size, size))
    bad += _check_ico()
    for path, text in svgs().items():
        try:
            got = _read(path).decode("utf-8")
        except OSError as err:
            print("%s: unreadable (%s)" % (path, err))
            bad += 1
            continue
        if got != text:
            print("%s: differs from scripts/lib/mkicons.py -- re-run it and commit the file" % path)
            bad += 1
        else:
            print("%s: matches the generator" % path)
    href = favicon_uri()
    for path in TEMPLATES:
        try:
            text = _read(path).decode("utf-8")
        except OSError as err:
            print("%s: unreadable (%s)" % (path, err))
            bad += 1
            continue
        if len(_ICON_HREF.findall(text)) != 1 or ('<link rel="icon" href="%s"' % href) not in text:
            print("%s: the favicon data URI is not this generator's striped mark -- "
                  "re-run scripts/lib/mkicons.py and commit the template" % path)
            bad += 1
        else:
            print("%s: the favicon is the generator's (%d bytes)" % (path, len(href)))
    return 1 if bad else 0


def main(argv):
    if argv and argv[0] == "--check":
        return check()
    write(argv[0] if argv else None)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
