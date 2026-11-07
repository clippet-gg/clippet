# The companion mark

Clippet, the wordmark and the striped clapper are trademarks of the Licensor; the licence grants no trademark
rights — a fork must rename and rebrand.

The wordmark is the logo and stays alone in the header; this mark is its companion for the surfaces the word
cannot reach — the favicon, the app and manifest icons, the Discord application icons, the placeholder card — and
it is never set beside the word as a lockup (`web/STYLE.md`, "The wordmark and the favicon"; ROADMAP #43;
DESIGN.md §20 errata 301 and 306). The mark is the striped clapper: the "mitre" frame of two stems 272 wide at the
wordmark's pitch of 1146, the right stem grown up to meet a lid 230 thick lifted 9° at the left hinge, with three
slashed 40° cuts through the lid, over the wordmark's own green span (1418 × 200) at the wordmark's own 200 gap
under the stems — every number the wordmark's, in font units of 2000 per em. The cuts are HOLES in the lid's one
`fill-rule="evenodd"` path, never paper polygons laid over ink, because an overlay leaves a hairline at every
size that is not a whole multiple of the drawing. **There is no size rule** (erratum 306; the owner's decision of 2026-09-18: no plain clapper anywhere in
the repository, the striped mark reads fine small): this one drawing is every size, the tab's
16 px included — erratum 301's plain mitre at 32 and 16 is revoked and deleted, and what carries the mark down
there is the generator's 4 × 4 supersampling, never hand-placed pixels. The favicon a PAGE declares is that same
unit geometry, in the mark's own box with a sixteenth of the square as inset, carrying its own
`prefers-color-scheme` rule. `web/static/favicon.ico`, which `GET /favicon.ico` answers for the clients that ask
without reading a page (DESIGN.md §20 erratum 304), is the same drawing in three PNG entries at 16, 32 and 48,
all ink on transparent with the green span and no tile. An ICO carries no colour-scheme rule, so it is the LIGHT
theme's ink at every size. The ink is the theme's text colour and
the green is the theme's one green (`--ok`); on a surface with no theme — the Discord tile — the mark is
`#ECEDEF` and the span `#4CD69A` on the `#17181C` ink tile, and the DEV application's icon wears a grey span
(`#837F77`), which is the only difference between the two (the rule `INSTANCE_BADGE` repeats on the wordmark:
green span = production, grey span = dev). The AUTO application's icon wears a blue span (`#2FA8E8`) and differs
in nothing else: blue span = the Auto bot. A dev copy of the Auto bot wears the dev icon: grey outranks blue, and
the name tells the two apart.

| file | what it is |
|---|---|
| `mark.svg` | the unit geometry, striped, one element per piece (`currentColor` ink, the tile green) — the only mark there is |
| `discord/…`, the Polar tile | the Discord applications' icons (production: green span; dev: grey; Auto: blue) and bot banners, and the 16:9 product tile — drawn by the same generators from this one geometry, uploaded by hand in the developer portal (DEPLOY.md §7.1); kept outside this repository with the hosted side's assets |

**An audio clip's waveform is not a brand asset** (DESIGN.md §20 erratum 329). The poster and the audiogram
of an audio clip are a drawing OF THE CLIP — its own peaks as rounded bars on the ink tile, grey, and amber
where played — made by the server (`internal/transcode/waveform.go`) for each clip; nothing of it lives here
and nothing of it is the brand's. The one brand element on it is the mark in the poster's bottom-left
corner, and that is the manifest icon `web/static/icon-192.png` from its one source (this generator),
halved and laid on as it is — never redrawn. The audiogram itself carries no mark.

Every file here is DRAWN by `scripts/lib/mkicons.py` (the manifest icons and the favicon URI in the two templates
with them; the placeholder card by `scripts/lib/mkcard.py`) and diffed against it by `scripts/check.sh`; change the
geometry there, re-run it, and commit what it writes; never hand-export. The six banners are the one exception:
they are drawn by `scripts/lib/mkbanners.py` from the wordmark's own CSS, in the visual image's browser, and are not
diffed by `check.sh` — `python scripts/lib/mkbanners.py --check` compares them by hand.
