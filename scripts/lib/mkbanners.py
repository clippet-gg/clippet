#!/usr/bin/env python3
"""Draw the Discord bots' profile banners (docs/brand/README.md; web/STYLE.md, "The companion mark").

A Discord bot's profile carries a banner across its top — the developer portal's Bot page states
680 x 240, 17:6 — and Discord draws the bot's round avatar over the banner's lower-left corner. The
banner the owner picked (2026-10-06, candidate a of three) is the WORDMARK ALONE ON THE INK TILE: the
avatar's own ground, the word in paper, the span in the bot's colour, and nothing else — no line of
copy, no second drawing of the mark. This file writes it:

  docs/brand/discord/clippet-banner-680x240.png,       the production bot's banner, the span green
                     clippet-banner-1360x480.png       ...and the same picture at twice the size
  docs/brand/discord/clippet-dev-banner-680x240.png,   the DEV bot's: the same drawing with the GREY
                     clippet-dev-banner-1360x480.png   span — the only difference (erratum 285's rule)
  docs/brand/discord/clippet-auto-banner-680x240.png,  the AUTO bot's: the same drawing with the BLUE
                     clippet-auto-banner-1360x480.png  span — again the only difference

NOTHING HERE IS A DRAWING OF THE BRAND. Every brand piece is read from its one source:

  the wordmark   the three `.wordmark` rules, copied VERBATIM out of web/static/app.css at run time,
                 around the markup web/STYLE.md gives for a mark that is not a link, set in the real
                 web/static/fonts/spline-sans.woff2 and rasterised by Chromium — the engine and the
                 face the product itself is drawn with. If a rule is missing from the stylesheet the
                 run stops; it never falls back to a copy of its own.
  the colours    INK, PAPER, GREEN, GREY and BLUE are scripts/lib/mkicons.py's: the Discord tile's
                 palette, so a banner and the avatar Discord lays over it are one ink and one span.

What this file chooses is exactly two things, both placement: the word's font-size (SIZE — the
product sets the same rules at 16, 21, 32 and 40) and where its INK box sits (CENTRE). The ink box
runs from the ascender tops to the span's bottom edge; its centre is put right of the banner's own
centre because the avatar covers the lower-left corner — roughly the left 250 px of the lower half
at this size, an approximation, since Discord documents neither the popout's width nor the overlap —
and a word centred on the whole banner puts its `c` under the avatar's ring.

    python scripts/lib/mkbanners.py            # rewrite the six files above in place
    python scripts/lib/mkbanners.py OUTDIR     # ...or write them somewhere else, flat
    python scripts/lib/mkbanners.py --check    # do the committed six still match a fresh draw?
    python scripts/lib/mkbanners.py --name NAME --span "#RRGGBB" [--extra FILE] [OUTDIR]
    python scripts/lib/mkbanners.py --card      # the link card: docs/brand/og/clippet-card-1200x630.png
    python scripts/lib/mkbanners.py --gif FACE  # the link card ANIMATED: docs/brand/og/clippet-card-FACE.gif

THE LINK CARD (2026-10-07: the owner asked for a good embed when the link is pasted in Discord, then
for the preview image to be a high-quality gif that starts on that frame and plays one of the mark's
animations — the owner's pick: beam). The still is the wordmark in the middle of
Open Graph's 1200 x 630 on the ink tile. The GIF is the same card with ONE of the living mark's faces
(web/static/app.js FACES, erratum 274) played by the real engine: the page carries app.css whole and
app.js, the mark arrives wearing `data-face` (app.js's `wear` path plays it at boot), Playwright's
clock is PAUSED and stepped 1000/GIF_FPS ms between screenshots, so every frame is the engine's own
drawing at that instant — no re-timing, no interpolation of ours. Rendered at GIF_SCALE x for crisp
edges (Discord scales the picture down), assembled with one palette taken from the whole frame set
and no dither (three flat colours plus antialiased edges fit 256 entries with nothing lost), each
unchanged frame folded into the previous one's duration. It starts and ends on the still — the first
frame is what X, Slack and iMessage show, since they do not animate — and loops: the still held
GIF_HOLD_MS, the face, the still GIF_TAIL_MS. `--check` draws it again and compares the bytes.

The last form draws ONE bot: `--span` is the one thing a bot differs by (green = production, GREY =
dev, BLUE = Auto, a free value for a bot whose colour is not picked yet), and `--extra FILE` drops
FILE's HTML into a full-canvas layer (680 x 240 CSS px, absolutely positioned, above the ground and
under the word) for a design that needs one more element. Nothing is committed for such a bot until
its colour is decided.

THIS IS NOT A `scripts/check.sh` STAGE, unlike mkicons.py and mkcard.py. Those two draw with the
standard library alone; a wordmark set from live CSS needs a browser, so this one needs docker and
the `clippet-visual:1.48.0` image (Dockerfile.visual — scripts/visual.sh builds it), and check.sh
promises to need neither a browser nor that image. Verify by hand instead, after any change to the
wordmark's rules, the face or the five colours: run `--check`, which draws into a temporary folder
and compares each committed file's sha256 with the fresh one. The comparison is over the FILE BYTES:
both the rasteriser and the PNG encoder are the pinned image's Chromium, and two runs on the box
that drew the committed files are byte-identical (measured 2026-10-06). A different CPU or a bumped
image may round an antialiased edge differently; `--check` then names the files, and the eye decides.

Run from Git Bash on Windows or from a POSIX shell. The container is given one temporary folder and
nothing else — not the repository, not a `.env` — and the folder is removed when the run ends.
"""

import argparse
import base64
import hashlib
import io
import os
import re
import shutil
import struct
import subprocess
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from mkicons import BLUE, BRAND, GREEN, GREY, INK, PAPER, STATIC  # noqa: E402

IMAGE = "clippet-visual:1.48.0"      # the pin scripts/visual.sh builds from Dockerfile.visual
W, H = 680, 240                      # Discord's stated banner size (the portal's Bot page), 17:6
SCALES = (1, 2)                      # 680 x 240, and 1360 x 480 for the upload
SIZE = 100                           # the wordmark's font-size in px at 1x
CENTRE = (456, 120)                  # the centre of the word's ink box at 1x: right of the avatar's corner
OUT = os.path.join(BRAND, "discord")


def hexof(rgb):
    return "#%02X%02X%02X" % tuple(rgb)


# The committed bots: name -> the span's colour. Everything else about the pictures is equal.
BOTS = {"clippet": hexof(GREEN), "clippet-dev": hexof(GREY), "clippet-auto": hexof(BLUE)}

# The wordmark as web/STYLE.md writes it where the lockup is not a link.
WORDMARK_HTML = '<span class="wordmark" role="img" aria-label="clippet">cli<span class="pp">pp</span>et</span>'
WORDMARK_RULES = (".wordmark", ".wordmark .pp", ".wordmark .pp::after")


def wordmark_css():
    """(the three rules verbatim, the --sans stack) out of web/static/app.css, or exit: a rule that
    moved is a change to the logo's one source, and a generator that guessed at it would be a second."""
    with open(os.path.join(STATIC, "app.css"), encoding="utf-8") as fh:
        css = fh.read()
    rules = []
    for selector in WORDMARK_RULES:
        m = re.search(r"^" + re.escape(selector) + r" \{[^}]*\}", css, re.M)
        if not m:
            sys.exit("web/static/app.css: no rule `%s { ... }` -- the wordmark's source moved; "
                     "this generator copies it and never guesses it" % selector)
        rules.append(m.group(0))
    sans = re.search(r"--sans: ([^;]+);", css)
    if not sans:
        sys.exit("web/static/app.css: no --sans token")
    return "\n".join(rules), sans.group(1)


PAGE = """<!doctype html>
<meta charset="utf-8">
<title>%(name)s banner</title>
<style>
@font-face { font-family: "Spline Sans"; font-style: normal; font-weight: 300 700; src: url(data:font/woff2;base64,%(font)s) format("woff2"); }
html, body { margin: 0; }
body { width: %(w)dpx; height: %(h)dpx; overflow: hidden; position: relative; background: %(ink)s; font-family: %(sans)s; --text: %(paper)s; --ok: %(span)s; }
/* --- verbatim from web/static/app.css --- */
%(rules)s
/* --- placement only: the size and the position (the script below sets left and top) --- */
.wordmark { position: absolute; left: 0; top: 0; font-size: %(size)dpx; }
.extra { position: absolute; inset: 0; }
</style>
<div class="extra">%(extra)s</div>
%(wordmark)s
<script>
// Put the centre of the word's INK box (ascender tops .. the span's bottom edge, which hangs 0.265em
// under the mark's own box) on CENTRE, and say whether the face that drew it is the product's.
(async function () {
  var size = %(size)d, font = "600 " + size + "px 'Spline Sans'";
  var faces = [];
  try { faces = await document.fonts.load(font, "clippet"); } catch (e) { faces = []; }
  await document.fonts.ready;
  var el = document.querySelector(".wordmark");
  var c = document.createElement("canvas").getContext("2d");
  c.font = font;
  var m = c.measureText("clippet");
  var box = el.getBoundingClientRect();
  var baseline = (size - (m.fontBoundingBoxAscent + m.fontBoundingBoxDescent)) / 2 + m.fontBoundingBoxAscent;
  var inkTop = baseline - m.actualBoundingBoxAscent, inkBottom = size + 0.265 * size;
  el.style.left = Math.round(%(cx)d - box.width / 2) + "px";
  el.style.top = Math.round(%(cy)d - (inkTop + inkBottom) / 2) + "px";
  var ok = faces.length > 0 && faces.every(function (f) { return f.status === "loaded"; });
  document.documentElement.dataset.ready = ok ? "1" : "nofont";
})();
</script>
"""

# The rasteriser, run by node inside the image (its NODE_PATH carries playwright). LCD text and
# hinting are off so the picture does not depend on the container's font configuration.
SHOOT = """const fs = require("fs");
const { chromium } = require("playwright");
(async () => {
  const browser = await chromium.launch({ args: ["--disable-lcd-text", "--font-render-hinting=none"] });
  const pages = fs.readdirSync("/work/pages").filter((f) => f.endsWith(".html")).sort();
  for (const scale of [%(scales)s]) {
    const ctx = await browser.newContext({ viewport: { width: %(w)d, height: %(h)d }, deviceScaleFactor: scale });
    const page = await ctx.newPage();
    for (const f of pages) {
      await page.goto("file:///work/pages/" + f);
      await page.waitForFunction(() => document.documentElement.dataset.ready, null, { timeout: 15000 });
      const ready = await page.evaluate(() => document.documentElement.dataset.ready);
      if (ready !== "1") throw new Error(f + ": Spline Sans did not load (" + ready + ") -- no banner is written from a fallback face");
      const out = "/work/out/" + f.replace(/\\.html$/, "") + "-" + %(w)d * scale + "x" + %(h)d * scale + ".png";
      await page.screenshot({ path: out, clip: { x: 0, y: 0, width: %(w)d, height: %(h)d } });
    }
    await ctx.close();
  }
  await browser.close();
})().catch((e) => { console.error(String(e && e.message || e)); process.exit(1); });
"""


def filenames(name, kind="banner", w=W, h=H, scales=SCALES):
    return ["%s-%s-%dx%d.png" % (name, kind, w * s, h * s) for s in scales]


# The link card (2026-10-07, the owner's ask for a good embed when the link is pasted in
# Discord): the same drawing on the same ink tile, placed for a card instead of a popout — the word
# in the MIDDLE of Open Graph's 1200 x 630 (every reader that draws a large image takes that shape;
# Discord's card scales it to its width). Placement only, as above; the file is
# docs/brand/og/clippet-card-1200x630.png, and clippet-pass/site/static/og.png is a copy of it.
CARD_W, CARD_H = 1200, 630
CARD_SIZE = 176                      # the word is about 3.6em wide: ~630 px, a little over half the card
CARD_CENTRE = (600, 315)
CARD_OUT = os.path.join(BRAND, "og")

# The animated card (the header of this file): the face the owner picked, and how it is rendered.
GIF_FACE = "beam"
GIF_FPS = 30                         # 33 ms a frame; a GIF's clock is hundredths, so 3 = 30 ms
GIF_SCALE = 2                        # 2400 x 1260 pixels: crisp edges after Discord's scaling
GIF_HOLD_MS = 6000                   # the still, before the face (one frame's delay: a longer hold costs no bytes)
GIF_TAIL_MS = 400                    # the still, after it, before the loop restarts
GIF_MAX_MS = 4000                    # the longest a face may run before capture stops
GIF_FACES = ("blink", "smile", "beam", "glance", "wink", "gasp")   # drift is random: never a committed file


def png_size(data):
    if data[:8] != b"\x89PNG\r\n\x1a\n" or data[12:16] != b"IHDR":
        raise ValueError("not a PNG")
    return struct.unpack(">II", data[16:24])


def draw(bots, extra="", kind="banner", w=W, h=H, size=SIZE, centre=CENTRE, scales=SCALES):
    """Return {filename: PNG bytes} for {name: span colour}, rasterised in the visual image."""
    if subprocess.call(["docker", "image", "inspect", IMAGE], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL) != 0:
        sys.exit("no %s image (or no docker): build it the way scripts/visual.sh does --\n"
                 "  docker build -q --build-arg PW_VERSION=1.48.0 -t %s - < Dockerfile.visual" % (IMAGE, IMAGE))
    rules, sans = wordmark_css()
    with open(os.path.join(STATIC, "fonts", "spline-sans.woff2"), "rb") as fh:
        font = base64.b64encode(fh.read()).decode("ascii")
    work = tempfile.mkdtemp(prefix="clippet-banners-")
    try:
        os.makedirs(os.path.join(work, "pages"))
        os.makedirs(os.path.join(work, "out"))
        for name, span in bots.items():
            page = PAGE % {
                "name": name, "font": font, "w": w, "h": h, "sans": sans, "rules": rules,
                "ink": hexof(INK), "paper": hexof(PAPER), "span": span, "extra": extra,
                "wordmark": WORDMARK_HTML, "size": size, "cx": centre[0], "cy": centre[1],
            }
            with open(os.path.join(work, "pages", "%s-%s.html" % (name, kind)), "w", encoding="utf-8", newline="\n") as fh:
                fh.write(page)
        with open(os.path.join(work, "shoot.js"), "w", encoding="utf-8", newline="\n") as fh:
            fh.write(SHOOT % {"w": w, "h": h, "scales": ", ".join(str(s) for s in scales)})
        # A `C:/...` host path and MSYS_NO_PATHCONV=1: from Git Bash neither the mount's container
        # half nor the two in-container paths may be rewritten into Windows ones. Harmless elsewhere.
        env = dict(os.environ, MSYS_NO_PATHCONV="1")
        rc = subprocess.call(["docker", "run", "--rm", "-v", work.replace("\\", "/") + ":/work", "-w", "/pw",
                              IMAGE, "node", "/work/shoot.js"], env=env)
        if rc != 0:
            sys.exit("the rasteriser failed (exit %d): nothing was written" % rc)
        out = {}
        for name in bots:
            for scale, fname in zip(scales, filenames(name, kind, w, h, scales)):
                with open(os.path.join(work, "out", fname), "rb") as fh:
                    data = fh.read()
                if png_size(data) != (w * scale, h * scale):
                    sys.exit("%s: drawn %dx%d, want %dx%d" % ((fname,) + png_size(data) + (w * scale, h * scale)))
                out[fname] = data
        return out
    finally:
        shutil.rmtree(work, ignore_errors=True)
        if os.path.exists(work):
            print("could not remove %s -- remove it by hand" % work)


def write(bots, outdir, extra=""):
    os.makedirs(outdir, exist_ok=True)
    for fname, data in sorted(draw(bots, extra).items()):
        path = os.path.join(outdir, fname)
        with open(path, "wb") as fh:
            fh.write(data)
        print("wrote %s (%d bytes, sha256 %s)" % (path, len(data), hashlib.sha256(data).hexdigest()))
    return 0


def draw_card():
    """The link card: {filename: PNG bytes}, one file."""
    return draw({"clippet": BOTS["clippet"]}, kind="card", w=CARD_W, h=CARD_H, size=CARD_SIZE, centre=CARD_CENTRE, scales=(1,))


# The animated card's page: the still's page with app.css WHOLE (the eyes overlay and the faces'
# classes live there; its own @font-face rules are cut, since a file page cannot fetch /static/fonts/
# and the data: face above is the one that loads) and the mark wearing the face. app.js is added by
# the shooter AFTER the fonts are in and the clock is paused, so its boot — which plays the worn face
# on the first animation frame — runs on the paused clock from its first millisecond.
GIF_PAGE = """<!doctype html>
<meta charset="utf-8">
<title>%(name)s card</title>
<style>
@font-face { font-family: "Spline Sans"; font-style: normal; font-weight: 300 700; src: url(data:font/woff2;base64,%(font)s) format("woff2"); }
/* --- web/static/app.css, whole but its @font-face rules --- */
%(css)s
/* --- placement only --- */
html, body { margin: 0; background: %(ink)s !important; }
body { width: %(w)dpx; height: %(h)dpx; overflow: hidden; position: relative; --text: %(paper)s; --ok: %(span)s; }
.wordmark { position: absolute; left: 0; top: 0; font-size: %(size)dpx; color: %(paper)s; }
</style>
%(wordmark)s
<script>
(async function () {
  var size = %(size)d, font = "600 " + size + "px 'Spline Sans'";
  var faces = [];
  try { faces = await document.fonts.load(font, "clippet"); } catch (e) { faces = []; }
  await document.fonts.ready;
  var el = document.querySelector(".wordmark");
  var c = document.createElement("canvas").getContext("2d");
  c.font = font;
  var m = c.measureText("clippet");
  var box = el.getBoundingClientRect();
  var baseline = (size - (m.fontBoundingBoxAscent + m.fontBoundingBoxDescent)) / 2 + m.fontBoundingBoxAscent;
  var inkTop = baseline - m.actualBoundingBoxAscent, inkBottom = size + 0.265 * size;
  el.style.left = Math.round(%(cx)d - box.width / 2) + "px";
  el.style.top = Math.round(%(cy)d - (inkTop + inkBottom) / 2) + "px";
  var ok = faces.length > 0 && faces.every(function (f) { return f.status === "loaded"; });
  document.documentElement.dataset.ready = ok ? "1" : "nofont";
})();
</script>
"""

# The shooter: the fonts on the real clock, then the clock installed and PAUSED (installed alone it
# kept running through each screenshot — 47 ms a frame, measured — and the face played at double
# speed), then app.js. One screenshot per 1000/FPS ms until the mark has taken its face off and the
# overlay has not changed for six frames.
GIF_SHOOT = """const fs = require("fs");
const { chromium } = require("playwright");
const STEP = 1000 / %(fps)d, MAX = %(max)d;
(async () => {
  const browser = await chromium.launch({ args: ["--disable-lcd-text", "--font-render-hinting=none"] });
  const ctx = await browser.newContext({ viewport: { width: %(w)d, height: %(h)d }, deviceScaleFactor: %(scale)d, hasTouch: false });
  const page = await ctx.newPage();
  page.on("pageerror", (e) => console.error("pageerror: " + e.message));
  await page.goto("file:///work/pages/card.html");
  await page.waitForFunction(() => document.documentElement.dataset.ready, null, { timeout: 15000 });
  const ready = await page.evaluate(() => document.documentElement.dataset.ready);
  if (ready !== "1") throw new Error("Spline Sans did not load (" + ready + ") -- nothing is written from a fallback face");
  if (!(await page.evaluate(() => matchMedia("(hover: hover)").matches))) throw new Error("no hover: the engine would not draw");
  await page.clock.install({ time: new Date("2026-10-07T00:00:00Z") });
  await page.clock.pauseAt(new Date("2026-10-07T00:00:01Z"));
  await page.addScriptTag({ path: "/work/pages/app.js" });
  let n = 0, lastSvg = "", quiet = 0, started = false;
  for (let t = 0; t <= MAX; t += STEP) {
    await page.screenshot({ path: "/work/out/" + String(n).padStart(4, "0") + ".png", clip: { x: 0, y: 0, width: %(w)d, height: %(h)d } });
    const svg = await page.evaluate(() => { const s = document.querySelector("svg.eyes"); return s ? s.outerHTML : ""; });
    const wearing = await page.evaluate(() => document.querySelector(".wordmark").hasAttribute("data-face"));
    if (wearing) started = true;
    if (started && !wearing && svg === lastSvg) quiet++; else quiet = 0;
    lastSvg = svg;
    n++;
    if (started && !wearing && quiet >= 6) break;
    await page.clock.runFor(STEP);
  }
  if (!started) throw new Error("the face never played");
  await browser.close();
})().catch((e) => { console.error(String(e && e.message || e)); process.exit(1); });
"""


def draw_gif(face):
    """The animated link card for one face: GIF bytes, rendered by the engine (see the header)."""
    from PIL import Image  # the host's python carries it (CLAUDE.md "Host tools"); the standard library has no GIF writer
    if face not in GIF_FACES:
        sys.exit("--gif takes one of %s (drift is random and never a committed file)" % ", ".join(GIF_FACES))
    if subprocess.call(["docker", "image", "inspect", IMAGE], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL) != 0:
        sys.exit("no %s image (or no docker): build it the way scripts/visual.sh does" % IMAGE)
    with open(os.path.join(STATIC, "fonts", "spline-sans.woff2"), "rb") as fh:
        font = base64.b64encode(fh.read()).decode("ascii")
    with open(os.path.join(STATIC, "app.css"), encoding="utf-8") as fh:
        css = re.sub(r"@font-face\s*\{[^}]*\}", "", fh.read())
    work = tempfile.mkdtemp(prefix="clippet-cardgif-")
    try:
        os.makedirs(os.path.join(work, "pages"))
        os.makedirs(os.path.join(work, "out"))
        shutil.copy(os.path.join(STATIC, "app.js"), os.path.join(work, "pages", "app.js"))
        page = GIF_PAGE % {
            "name": face, "font": font, "css": css, "w": CARD_W, "h": CARD_H, "ink": hexof(INK), "paper": hexof(PAPER),
            "span": BOTS["clippet"], "size": CARD_SIZE, "cx": CARD_CENTRE[0], "cy": CARD_CENTRE[1],
            "wordmark": WORDMARK_HTML.replace('class="wordmark"', 'class="wordmark" data-face="%s"' % face),
        }
        with open(os.path.join(work, "pages", "card.html"), "w", encoding="utf-8", newline="\n") as fh:
            fh.write(page)
        with open(os.path.join(work, "shoot.js"), "w", encoding="utf-8", newline="\n") as fh:
            fh.write(GIF_SHOOT % {"fps": GIF_FPS, "max": GIF_MAX_MS, "w": CARD_W, "h": CARD_H, "scale": GIF_SCALE})
        env = dict(os.environ, MSYS_NO_PATHCONV="1")
        rc = subprocess.call(["docker", "run", "--rm", "-v", work.replace("\\", "/") + ":/work", "-w", "/pw",
                              IMAGE, "node", "/work/shoot.js"], env=env)
        if rc != 0:
            sys.exit("the rasteriser failed (exit %d): nothing was written" % rc)
        out = os.path.join(work, "out")
        frames = [Image.open(os.path.join(out, f)).convert("RGB") for f in sorted(os.listdir(out)) if f.endswith(".png")]
        rest = frames[0].tobytes()
        moving = [i for i, f in enumerate(frames) if f.tobytes() != rest]
        if not moving:
            sys.exit("%s: the mark never moved" % face)
        seq = frames[moving[0]:moving[-1] + 1]
        # ONE palette from the whole set — every antialiased edge of every frame is in it — and no dither
        w, h = frames[0].size
        union = Image.new("RGB", (w, h * (len(seq) + 1)))
        union.paste(frames[0], (0, 0))
        for i, f in enumerate(seq):
            union.paste(f, (0, h * (i + 1)))
        pal = union.quantize(colors=256, method=Image.Quantize.MEDIANCUT, dither=Image.Dither.NONE)
        q = [frames[0]] + seq + [frames[0]]
        q = [im.quantize(colors=256, palette=pal, dither=Image.Dither.NONE) for im in q]
        durations = [GIF_HOLD_MS] + [round(1000 / GIF_FPS)] * len(seq) + [GIF_TAIL_MS]
        buf = io.BytesIO()
        q[0].save(buf, format="GIF", save_all=True, append_images=q[1:], duration=durations, loop=0, disposal=1, optimize=False)
        return {"clippet-card-%s.gif" % face: buf.getvalue()}, len(seq)
    finally:
        shutil.rmtree(work, ignore_errors=True)
        if os.path.exists(work):
            print("could not remove %s -- remove it by hand" % work)


def check():
    """Compare the committed six banners and the card against a fresh draw, by sha256. 0 = all match."""
    bad = 0
    drawn = [(OUT, fname, data) for fname, data in sorted(draw(BOTS).items())]
    drawn += [(CARD_OUT, fname, data) for fname, data in sorted(draw_card().items())]
    drawn += [(CARD_OUT, fname, data) for fname, data in sorted(draw_gif(GIF_FACE)[0].items())]
    for outdir, fname, data in drawn:
        path = os.path.join(outdir, fname)
        try:
            with open(path, "rb") as fh:
                got = fh.read()
        except OSError as err:
            print("%s: unreadable (%s)" % (path, err))
            bad += 1
            continue
        if got != data:
            print("%s: differs from scripts/lib/mkbanners.py (committed sha256 %s, drawn %s) -- look at both; "
                  "if the wordmark's source changed, re-run the generator (and --card) and commit the files"
                  % (path, hashlib.sha256(got).hexdigest(), hashlib.sha256(data).hexdigest()))
            bad += 1
        else:
            print("%s: matches the generator (sha256 %s)" % (path, hashlib.sha256(got).hexdigest()))
    return 1 if bad else 0


def main(argv):
    ap = argparse.ArgumentParser(description="Draw the Discord bots' profile banners (see the header of this file).")
    ap.add_argument("outdir", nargs="?", help="write somewhere else, flat (default: docs/brand/discord)")
    ap.add_argument("--check", action="store_true", help="compare the committed six and the card against a fresh draw")
    ap.add_argument("--card", action="store_true", help="draw the link card (docs/brand/og/clippet-card-1200x630.png)")
    ap.add_argument("--gif", metavar="FACE", help="draw the link card animated with one face (docs/brand/og/clippet-card-FACE.gif)")
    ap.add_argument("--name", help="draw ONE bot's banner: its file-name stem, e.g. clippet-staging")
    ap.add_argument("--span", help="that bot's span colour, #RRGGBB (needed for a name that is not committed)")
    ap.add_argument("--extra", help="a file of HTML for the full-canvas layer under the word")
    a = ap.parse_args(argv)
    if a.check:
        if a.outdir or a.name or a.span or a.extra or a.card or a.gif:
            ap.error("--check takes nothing else")
        return check()
    if a.gif:
        if a.name or a.span or a.extra or a.card:
            ap.error("--gif takes only an outdir")
        outdir = a.outdir or CARD_OUT
        os.makedirs(outdir, exist_ok=True)
        files, n = draw_gif(a.gif)
        for fname, data in sorted(files.items()):
            path = os.path.join(outdir, fname)
            with open(path, "wb") as fh:
                fh.write(data)
            print("wrote %s (%d bytes, %d moving frames at %d fps, %dx%d, sha256 %s)"
                  % (path, len(data), n, GIF_FPS, CARD_W * GIF_SCALE, CARD_H * GIF_SCALE, hashlib.sha256(data).hexdigest()))
        return 0
    if a.card:
        if a.name or a.span or a.extra:
            ap.error("--card takes only an outdir")
        outdir = a.outdir or CARD_OUT
        os.makedirs(outdir, exist_ok=True)
        for fname, data in sorted(draw_card().items()):
            path = os.path.join(outdir, fname)
            with open(path, "wb") as fh:
                fh.write(data)
            print("wrote %s (%d bytes, sha256 %s)" % (path, len(data), hashlib.sha256(data).hexdigest()))
        return 0
    if (a.span or a.extra) and not a.name:
        ap.error("--span and --extra belong to --name")
    if not a.name:
        return write(BOTS, a.outdir or OUT)
    if not re.fullmatch(r"[a-z0-9]+(-[a-z0-9]+)*", a.name):
        ap.error("--name is a file-name stem: lowercase letters, digits and single hyphens")
    span = a.span or BOTS.get(a.name)
    if not span:
        ap.error("--name %s is not a committed bot: give its --span" % a.name)
    if not re.fullmatch(r"#[0-9A-Fa-f]{6}", span):
        ap.error("--span is #RRGGBB")
    extra = ""
    if a.extra:
        with open(a.extra, encoding="utf-8") as fh:
            extra = fh.read()
    return write({a.name: span}, a.outdir or OUT, extra)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
