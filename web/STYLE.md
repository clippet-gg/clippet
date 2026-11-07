# Clippet — the visual system "Kept" (one page)

Chosen by the owner in the 2026-09-09 design pass (proposal F, the system named Kept; logo variation 2). This page is
the whole system; `web/static/app.css` is its one implementation and carries the colour law in its
header comment. DESIGN.md §20 erratum 240 binds this page; errata 241–243 record the three contracts
it changed (`og:site_name`, the theme cookie, `font-src`).

## The idea

A clip is the part you kept. The interface is built from the one object that moment is made of —
**the rail** — a thin track that fills while the server works, grows two handles when you trim, drains
while a link ages, and turns green at exactly one moment: when the clip is shareable. Around it the page
is a quiet room (paper by day, a warm screening room at night), so the video's own colour is the
loudest thing on any screen.

## Tokens

| token | light (daylight studio) | dark (screening room) | role |
|---|---|---|---|
| `--bg` | `#ECEDEF` | `#191816` | the page ground; also `<meta name="theme-color">` and the manifest |
| `--surface` | `#FFFFFF` | `#211F1C` | panels, rows, forms |
| `--surface-2` | `#F4F5F6` | `#2A2825` | fields, the filmstrip trough, the drop frame |
| `--line` | `#D5D7DB` | `#34312D` | rules between rows, the empty rail |
| `--line-strong` | `#878A90` | `#787269` | field edges, the dashed queued rail, the grey lamp |
| `--text` / `--primary` | `#17181C` | `#ECE9E2` | ink, and the one solid button |
| `--primary-text` | `#FFFFFF` | `#191816` | text on the solid button |
| `--muted` | `#5B5F67` | `#A9A59C` | secondary text, row labels |
| `--faint` | `#666971` | `#959087` | hints, timestamps |
| `--time` | `#D98E12` | `#F0B24A` | amber as a fill: working rail, handles, lifetime rail, the pulsing lamp |
| `--time-text` / `--warn` | `#8A5400` | `#F0B24A` | amber as text: `23 days left`, the token-shown-once notice |
| `--marker` | `rgba(217,142,18,.42)` | `rgba(240,178,74,.45)` | the stroke under a link |
| `--ok` | `#0E7C4F` | `#4CD69A` | green: a full rail, the ready lamp, `saved`, `copied`, the wordmark's span |
| `--danger` | `#B3261E` | `#F0665C` | delete, revoke, failed — text and a hairline, never a fill |
| `--media` | `#0A0A0A` | `#000000` | the player's own black |
| `--dim` | `rgba(236,237,239,.74)` | `rgba(25,24,22,.74)` | the trimmed-away ends of the filmstrip |
| `--focus` | `#17181C` | `#ECE9E2` | the focus ring: the text colour, never a hue |

Three-state theme, and the order is load-bearing: the complete light palette on bare `:root`; the same
tokens again under `@media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) }` (the
system's choice, unless the cookie pinned light); and once more under `:root[data-theme="dark"]` (the
cookie's choice). No colour has its only definition inside a media or attribute block. `body` paints
`var(--bg)` explicitly. The page handlers read the `theme` cookie (`light` | `dark`; anything else =
follow the system) and render `data-theme` on `<html>`, so the choice paints with the first byte and
there is no flash and no inline script; Settings has the three-way control (System / Light / Dark),
app.js writes the cookie and applies the attribute at once, and `POST /settings/theme` is the same
control without JavaScript (DEPLOY.md §28) — anonymously since §20 erratum 288, because the glyph
below is on pages nobody has signed in to, and it comes back to the page it was pressed on.

**The top bar carries the same choice as one glyph** (§20 erratum 288, the owner's ask: a light/dark
glyph in the top bar): last in the bar, on every page that HAS a bar — the library,
Settings, a stranger's share or collection page — in its own `.bar-end` slot after the nav, never
inside it (it navigates nowhere, and a stranger's page has no nav). 16 px, the Kept line style: 1.5 px
strokes, `currentColor`, `--muted`, `--text` on hover, the standard focus ring, no button chrome and no
label. **A sun on a page painted dark, a moon on a page painted light, each naming the theme it HANDS
YOU** (`aria-label` and `title`: `Light theme`, `Dark theme`) — a toggle labelled with the state it is
in is one half its readers press the wrong way. The page renders BOTH buttons always and the stylesheet
draws exactly one, through the same three states in the same order as the token blocks above: the bare
rule hides the sun, `@media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) }` shows it,
`:root[data-theme="dark"]` shows it again. That is what makes it right in all three states with no
script — the server never learns what the visitor's machine prefers. Pressing it while the cookie says
`system` PINS light or dark; **`System` is reachable from Settings only**, because a bar has two states
to swap between and "ask the machine" is a preference, not a toggle. The pages that are their own front
door — the front door, login, register, the message pages — have no bar and so no glyph. A stranger's
page keeps erratum 244's rule exactly: no nav, no sign-up, no CTA and no brand but the wordmark. A
control that changes how this browser paints is none of those.

## The colour law

- **amber = happening**: encoding, uploading, playing, the time a link has left, a trim being chosen.
  It fills rails, handles and the lamp beside a stage phrase — never a button.
- **green = done**: shareable, saved, copied, kept forever, the card edge, the wordmark's span. A green
  thing on screen always means "you can paste this".
- **red = loss**: delete, revoke, failed. Text and a hairline, never a fill.
- **ink = every button**. One solid ink button per screen (Upload, Copy link, Log in, Register); the
  rest are hairline. Grey is waiting. Where the next step depends on state, the ink moves with it: the
  upload card's Pick a file is solid until a file is picked, then Upload is (§20 erratum 313), and the
  frame's foot gives the picked file's name a line of its own above the stage phrase (erratum 314).
- A hue never fills a control, colours a link, tints a hover or draws a focus ring.
- Blue (`#2FA8E8`) is not a UI colour. It is the span of the Auto bot — on its Discord icon and its banner —
  and it is the blue of a hyperlink: both of that bot's jobs end in a link (the owner's call, 2026-10-06). No page,
  button or state in the product wears it.

## Type

One family, self-hosted (`web/static/fonts/`, OFL, latin subset, two variable woff2 files, ~94 KB,
`font-display: swap`, `font-src 'self'`): **Spline Sans** for everything a person reads or presses,
**Spline Sans Mono** for things a machine measured or a person copies (timecodes, sizes, prefixes, the
state cell). Stacks: `"Spline Sans", "Segoe UI", Roboto, system-ui, sans-serif` and
`"Spline Sans Mono", ui-monospace, Consolas, Menlo, monospace`.

| step | size / lh | weight | use |
|---|---|---|---|
| display | 30 / 1.08, −0.03em (24 on a phone) | 700 | the clip title on the watch page |
| wordmark | 21 (header) · 40 (auth, message and front pages) · 16 (the embed overlay) | 600, −0.025em | `clippet` |
| title | 22 / 1.2 | 600 | page titles (Settings, a collection) |
| heading | 17 / 1.3 | 600 | panel heads (Details, Password) |
| body | 15 / 1.5 | 400 | prose, inputs; 14 on buttons |
| small | 13 / 1.45 | 400 | row labels, meta, hints |
| micro | 12 / 1.4 | 400–500 | the state cell (mono), timecodes on handles, durations on thumbnails |

Sentence case throughout. Prose capped at 64ch. Numbers that must line up are mono, never tracked.

## The one grammar: `label | control | state`

Every editable thing on every page is one row, `<div class="row"><span class="rl">label</span><span class="rc">control</span><span class="rs">state</span></div>`:
a 3-column grid (`9rem | 1fr | auto`) with a hairline between rows, one column under 780 px. The label
is a `<label for>` where the control has an id. The state cell is mono, right-aligned, and always
rendered — empty when there is nothing to say — so `saved`, `copied`, `23 days left`, `8–72 characters`
and `click again to confirm` always land in the same place. Rows live in a `.panel` with an optional
`.panel-head` (a heading and one short hint). The library's clip rows are `.clip-row`, not `.row`:
they are a list, not the grammar.

**A control never leaves its own track** (§20 erratum 288): `.rc > input, .rc > textarea` carry
`min-width: 0`, because a flex item's automatic minimum size is its CONTENT's and an input's content
width is its `size` — about 260 px — so without it an input refuses to shrink and is painted outside
its column whatever the grid says. It did, on the register panel, under the hint describing it (the owner,
2026-09-16, asked whether the hint placement was intentional). **And the auth
panel is the one container where the three columns do not fit**: `.auth` is 460 px, and `9rem` of label
plus a state cell of up to 26ch leaves the control about 70 px. So `.auth .row` takes the ≤780 px form
at EVERY width — label above, control full width, the hint under the control and left-aligned,
`.rs:empty` hidden — which is the grammar's own answer for a narrow box and not a special case: login
and register then read the same at every size, and the hint sits under the field it describes.

Two kinds of things exist on the owner's watch page: **actions** (Copy link, Copy embed, Download, Make
GIF, Use this frame, Replace video — one strip of one shape, one of them ink; Trim on the filmstrip
itself) and **facts** (title, description, tags, collections, lifetime, unlisted — rows in one panel with
ONE Save that fans out to the three endpoints and says `saved` only after all of them landed). Delete
is its own last panel: a red word, "click again to confirm" in the state cell.

**A closed set of two or three is a `.seg` radio group**, never a `<select>`: Theme (System/Light/Dark)
and Links (Compact/Memorable, on the upload form and in Settings — erratum 250). Radios, so a form
without JavaScript posts the value like any other field; the checked one is the ink fill, and the row's
hint wraps under the control (`.row .hint { flex: 1 1 100% }`) where a sentence is needed rather than a
state word. A `<select>` stays what a longer closed set uses (Delete after). **Where a closed set is a
set of SHAPES, the hint shows the shapes** (erratum 264 — the owner's call: show an example of each instead of
that copy): the Links hint is `Compact: {host}/Xk3v9QpL2mZa · Memorable: {host}/PurpleUpdateCharacter`, each
in `<code>` because a link is a thing a person copies, and the host is the configured public one
rendered by the server, never a literal in a template. Settings keeps the word list's credit after the
examples; a credit is an attribution, not copy.

**A list of like rows keeps the grammar and adds three things** (the claim page's server lists, the design pass of
2026-09-16 on the hosted side, the owner's picks). A row may be the `<summary>` of a `<details>` — `name | state |
chevron`, the chevron where the state cell's end would be — whose body holds that row's own controls; sibling rows share
one `name` so opening one closes the rest, and a collapsed row exposes no button, which is how one ink control per screen
holds on a list of twenty servers BY CONSTRUCTION and not by rule. A list of like rows whose STATE CELL CAN WRAP goes
two-across above 780 px (an odd last row spans both columns); a row carrying a host does not go two-across — a host is a
name, and a name wraps as words, never as letters (a name of two words may break between them; the host stays whole with
its state under it). A value a person copies — a token, a command — is shown WHOLE, `pre.mono` wrapping at any character
rather than clipping, and selects whole: `user-select: all`, its hint reading `click selects all`, so a copy is one click
and never a partial line.

**An ordered list is a WALK, and there is one** (§20 erratum 297): the `<ol class="walk">` on `/setup`, where
each step is a thing to do in an order that matters and the numbers are the point. Everywhere else a sequence
is rows in the grammar, a `.seg` or prose; a numbered list of facts is a table with its numbering showing.
Each step of a walk is two lines — the instruction in terms of the artifact and its property, then, under it
in the hint face, a note naming somebody else's current labels — because a vendor's menus rot on the vendor's
schedule; the date those labels were read is printed ONCE, `labels read 2026-09-13`, in a small right-aligned
line under the list, in the state cell's face (errata 309-311), never on every step. The list itself has no
label and runs full width in the walk's body, outside the row grammar (erratum 311): an ordered list is its
own name, and a row labelled `steps` around a list of steps says it twice.

**Labels are nouns, and a refusal is a state** (§20 errata 309, 311, the owner's walks of `/setup`). A label column
holds the names of things — `backend`, `where`, `clips` — never the joints of a paragraph (`first`, `no`,
`instead`, `why`); a column that has to be read top to bottom to learn what any row is cannot be scanned. A
pick the page will not honour would be a red state word in that control's own state cell and one
red-hairline callout under the controls that caused it — never a panel further down headed by a negation,
and never a rule stated before it bites. `/setup` honours every pick since erratum 348 (the owner's rule: name the
risk, never refuse): the folder behind a tunnel walks, and its risk is the walk's closing hint line. A verdict on a pick is a state word (`on now`, the option's own one fact), and a
panel is headed by what it is, never by a place or a mood. **When the panels are done in order, the heads
say so** (errata 310, 311 — the owner's second walk, then the blank-sheet mock the owner chose): numbered — a 22 px
mono chip, hairline, no fill, before the h2 — and named for the step: `1 · Pick a storage backend`,
`2 · Cloudflare R2` (the backend's own name), `3 · Apply`; a panel that is not a step (the facts, headed
`Now`) carries no number; and a step not reached yet renders as its head alone, muted and dashed (`2 · Set
up`, `3 · Apply`), so the whole route is visible before the first pick and nothing below the pick pretends
to be ready. **A fact about the instance that changes what the reader must do next is a callout with its
fact as the bold lead; a verdict on a control is still a state word** (erratum 311). The STOP is that
callout — **`5 clips stay where they are.`** then what does not happen and what comes first — never a row
(`clips | … | stop` gave a sentence a label it did not need and a state cell nobody asked for, erratum
310) and never a red line in the panel head (310's `.hint.danger` went with it): a state word is a verdict
on a control, and the STOP is a verdict on nothing.

**Three small components, and no new token** (erratum 311; the `/setup` block of `web/static/app.css`).
*The numbered head* — `.setup .panel-head .n`: a 22 px mono chip (hairline, no fill) before a panel's h2,
which is laid out as a flex row on this page; and `.panel.later`, a dashed, head-only, muted panel for a
step that is not reachable yet. *The radio cards* — `.choices` / `.choice`: a grid of `<label>`s each
holding a radio, an ink dot, a name and a state cell (the option's one fact · `on now`);
two across above 780 px, one below, the state cell under the name at 600; ink border and an ink dot on the
chosen one, no fill, no hue. Radios, so the form GET posts `store` with JavaScript off. This is the one
place a `<select>` was the wrong control: six choices whose facts ARE the decision, and a closed `<select>`
hid the one thing a first-time reader needs to compare — the seg rule above (a closed set of two or three)
and the select rule (a longer closed set of mere values) stand; a closed set whose FACTS decide it is cards.
*The callout* — `.callout`: `.notice`'s shape (10/14 padding, 4 px radius, 64ch) with a `--danger`
hairline and a bold red lead sentence; used once, by the STOP (the refusal that was its second use went
with erratum 348). Red is loss — the clips a switch strands — a hairline, never a fill.

**No template contains a link to another origin.** Not a provider's console, not a documentation page, not
this product's own site (the one exception is the official bot's claim page on the hosted side, whose
wordmark is a door back into the product — recorded above). A page of this app links within this app and
nowhere else: `check.sh` fails the merge on a literal `http(s)://` under `web/` or `internal/`, comments
included, and the reason is not only the tripwire — a URL in a template is a promise about somebody else's
site that nothing here can keep. Where a walk needs one, it names the artifact in words and the .md files
carry the address (§20 erratum 298's two placeholder grammars, DEPLOY §35).

**Choosing a lifetime is always `Delete after` + a completion** (erratum 244 — the owner's reading: it is not keep
for never, it is keep forever or never delete): the upload form, Settings, the bulk bar and the
owner's row all read `Delete after` and their options finish the sentence — `1 day` … `90 days`,
`never` last, `server default · 30 days` and `no change` where the form has a no-op (the owner's row and
the bulk bar both rest on it, and Set sends nothing for it). A lifetime
already decided is a fact in its own words: `expires 2 Oct`, `23 days left`, `kept forever`; and
`Keep forever` stays an action. Never "keep for never".

## The rail

One class, `.rail`, 4 px, and its `<i>` is the fill. The states are classes only; app.js writes the
fill width (`el.style.width`) and never a style attribute.

| state | class | look | where |
|---|---|---|---|
| queued | `.queued` | dashed track, no fill | a library row nothing has claimed yet |
| working | `.working` | amber fill with a slow sweep (`.indeterminate` slides) | a library row encoding; the upload frame; the owner's processing page |
| left | `.left` | amber, draining from the upload towards the expiry | the Delete-after row |
| full | `.full` | green, 100 % | kept forever |
| done | `.done` | grey, 100 % | reserved: a ready row carries NO rail today, because a tick at the card frame would be a guess (the DTO carries no poster seconds, erratum 128) |

The lamp (`.lamp`) is the rail's dot: green = shareable (inside Copy link), amber and breathing beside
a stage phrase (`.on`), grey while the card is not ready (`.off`). If a rail appears anywhere that does
not encode time, it is wrong.

**The filmstrip rail** is the same object grown up: a 72 px trough (56 on a phone) welded under the
player, the sheet of frames end to end, the trimmed-away ends dimmed (`.cut`, two positioned divs),
the kept span outlined in amber (`.chosen`), two tall amber handles carrying mono `From`/`To` timecode
tags, and a tile of the handle's own frame on hover or drag. app.js positions all of it from the two
number boxes, which stay the API's truth.

## The wordmark and the favicon

**One surface deviates on purpose (the owner's call, 2026-09-15):** the official bot's claim page at `bot.clippet.gg/claim` (the
hosted side's, the one page of the product that is about Discord) paints the wordmark's span and its done word `connected` in
Discord's blurple instead of `--ok` — `#5865F2` in light (Discord's own hex, the owner's call: 4.61 : 1 on white and 4.22 : 1 on
`--surface-2`, a known and chosen trade; `#4F5CEE` is what would clear it) and `#7C86F7` in dark (ours, for the same
contrast on the charcoal); nothing else there is blue, and the geometry is this sheet's. On its Pass portal a setting's
state `on` is `--ok` green (`.rs .on`) and `off` stays muted (the owner's call, 2026-10-19). That page's header mark is also
the ONE wordmark whose `href` is not `/`: it links to the product's public site (the Worker's `SITE_URL`), because the
claim page is a door into the product and not a place of its own (erratum 244(b) stands for every page the product
serves itself). Here and everywhere else
the span is green. The wordmark IS the logo; there is no separate mark and no tile beside the word. `clippet` in Spline
Sans 600, lowercase, tracked −0.025em; the two stems of the double p are the handles and a green span
joins them at the descender tips — live text, one pseudo-element, and an empty SVG overlay app.js
fills while the cursor is near:
`<a class="wordmark" href="/" aria-label="clippet">cli<span class="pp">pp</span>et</a>` with
`.pp::after { left: .071em; width: .7105em; bottom: -.265em; height: .1em; background: var(--ok) }`
(measured from the glyphs: the span runs from the first stem's left edge to the second stem's right
edge, 1421 units = 0.71em, 200 units under the descender tips, 200 thick — the same gap the favicon draws;
until §20 erratum 271 the sheet said `-.165em`, which put the span's top edge on the tips). 21 px in the header, 40 px
on the auth and message pages. **The span is in no box the page can measure** — it is an absolutely
positioned pseudo-element hanging below the mark's own border box — so **every container that stacks
text under a DISPLAY mark owes it that overhang plus air**: `.auth` and `.message` give the mark's
grid cell (and, on a badged page, its `.lockup`) `margin-bottom: 0.5em`, which leaves about 21 px
under the span on the 404, 27 on the front door and 31 above the auth panels. Without it the 404
left ONE pixel and the green line read as an underline on "Not found" (§20 erratum 288 (e); the owner,
2026-09-16: the Not Found heading sat too close to the logo). The air is opened on the CONTAINER, never on
the mark: the geometry above is erratum 271's and nothing about it may move. **The mark is alive under a hovering pointer** (ROADMAP #38, §20
erratum 267): app.js puts `<svg class="eyes" viewBox="0 -1727 2300 2000">` inside the header's and
the auth card's `.pp` at boot — `.pp` is a 1 em flex item, so font units map 1:1 at any size — and it
paints nothing until the pointer MOVES inside the mark's tracking area (the header bar; the whole page for the display mark),
when two ink pupils, masked to the p's own counters, swim up, converge on the pointer and roll after
it, and leave when it leaves. Not the embed, not the message pages; nothing without a pointer that
hovers (`@media (hover: hover)` and a `matchMedia` check); under `prefers-reduced-motion: reduce`
nothing is injected and the mark is exactly the one above. **The mark reacts** (§20 erratum 274): the same
overlay carries lids and a mouth — the span redrawn as a 200-unit round stroke, clipped from the stems, drawn
only while it moves, the span itself hidden under it so the swap is invisible — and app.js plays one of eight
named faces at the moments the app already knows, on BOTH marks wherever the moment happens (`FACE_OF` in
app.js is the whole table): an upload landing `blink`s, a clip in the queue `smile`s, the worker taking it
`blink`s, a ready clip `smile`s, a failed one `sulk`s, a copied link `beam`s (the first copy of a tab `smile`s),
a save `beam`s, a delete `gasp`s, the library after a login `smile`s, the 404's mark arrives wearing a `gasp`
(`data-face`). Never two faces at once on one mark: a face that arrives mid-face queues, two deep. **The one
exception to the colour law:** `sulk`'s mouth is `--danger` — red is loss, and a failed clip is the one moment
the green span must not read as done; the red is the stroke (`.wordmark.sad`), never a repaint of the span.
**The cadence:** one ambient timer per open page draws every 45–90 s from `glance` and `drift`, alternating
(never the same twice), and about one draw in twelve is a `wink` — a few times an hour; it pauses while the eyes
are out under the pointer, while any face plays and while the tab is hidden, and never fires within 5 s of a
meaningful face. Pupil radius 150 font units on both marks. **The embed wears one face**: `smile` on `ready`,
no eyes, nothing else, ever. All of it under the follow's gates — a pointer that hovers, no reduced motion —
and exact rest after every face. Where the lockup is not a link it is a `<span class="wordmark display"
role="img" aria-label="clippet">`: an `aria-label` is only authoritative on an element whose role takes
a name from the author, and a bare span has no role — a link (the header, the 404, the embed) already
does. **A stranger's page carries the wordmark alone**: the share page and
the collection page render the header slot with the wordmark and nothing else when there is no
session — the same lockup, 21 px, linking to `/` — so a viewer knows whose player this is and can
get to it (erratum 244, the owner's amendment of the no-CTA rule). No nav, no "sign up", no text beside
it. **The embed carries it as one overlay** (erratum 246, the owner's ask: simple branding on the
embed, so that someone who likes the player knows what to try): `<a class="wordmark
embed-mark" href="/{id}" target="_top" rel="noopener">`, 16 px, top-LEFT (10 px down, 12 px in —
clear of the native controls' bottom band at any card size, 400 × 225 included; it was top-RIGHT until
erratum 287 put the clip's duration in the top-right of every poster, and at a card's own size the two
printed on top of each other), on the poster and while paused, gone while playing (app.js sets `data-playing` on
`body.embed` from the video's `play`/`pause`/`ended`; one rule in app.css hides it). Its ground is
always the player's black, so it uses the dark palette's text and green outright (`#ECE9E2`, `#4CD69A`)
and a shadow — the one shadow in the system, because the ink rule cannot apply on top of a
picture. It links to the clip's share page, relative, out of the iframe (`target="_top"`); the share
page's header wordmark then carries the viewer to `/`. No text beside it, no play count, no "sign up".
**It spells the instance's own public HOST** (erratum 287, the owner's ask: the baked-in text names the
host, follows the design language and is readable on any background without becoming more
obtrusive), because a viewer who likes this player can
type what they just read. On `clippet.gg` and any subdomain of it the host CONTAINS the wordmark, so
the lockup is intact — `{sub.}` + `cli<span class="pp">pp</span>et` + `.gg`, one face, one size, the
green span still under the two stems. On any other host it is the host as plain text in the same
style and there is no span at all: a self-hoster's `clips.example.com` is not this product's wordmark,
and the brand's green under two letters of somebody else's domain would be the logo claiming a name it
does not have. Such an overlay therefore has no `.pp`, so it wears no face and the badge's grey span
(erratum 285) has nothing to paint there — no loss, since it already names the box. It is also the one
form that can be LONG, so it alone carries a width: `.embed-mark:not(:has(.pp))` is an inline-block
bounded to `calc(80% - 12px)` and ellipsized (erratum 300 (h) on 287), which keeps a ~40-character
domain clear of the duration plate burned into the top-right of the same picture at a card's own
400 × 225. The selector excludes the lockup deliberately: `.pp::after` draws the green span BELOW the
text box, so clipping the overflow of a mark that has one would cut the span off it. That bound is
horizontal, and it is written that way on purpose: an `overflow` clips at the PADDING box, so the rule
also carries `padding-bottom: 5px`. `.wordmark` is `line-height: 1`, so the mark's content box is
exactly its 16 px while the mark PAINTS 21 — 2 px of descender below the box (p, q, y, g, j) and 3 px
of shadow below THEM — and a bound with no room under it takes the descender tails and the whole
shadow, which is the next thing this paragraph calls the readability. A width bound may never be paid
for in ink. The SHADOW is two layers: `#ECE9E2` on white is 1.21:1, so the letters are carried by the
dark edge under them rather than by their own colour. `0 1px 1px` at 85 % is that hard edge and
`0 1px 2px` at 60 % deepens and widens it — the pixel directly under a glyph goes from ≈#494949
(7.6:1) to ≈#2B2B2B (11.8:1) over white, and over black the whole shadow is invisible. Both layers
point DOWN, so a glyph's top edge on a white frame is still ≈1.1:1: a mark that cleared a bright frame
outright would need a scrim, a plate or a text-stroke, and a logo is recognised rather than read. The
one thing on a picture that must be READ is the duration below, which is why that one gets a plate. No
scrim, no plate, no size change here.
**The front door** at `/` is what a stranger reaches from there (erratum 246): the display wordmark,
the one line "Upload a clip. Get a link." (erratum 264), one ink `Log in`, `Create an account`
while registration is open, and nothing else — no header slot, no feature list, no marketing voice.
Discord's `og:site_name` and the oEmbed `provider_name` were the
wordmark's spelling until erratum 287; they are now the instance's own public HOST (`clippet.gg`,
`dev.clippet.gg`, `clips.example.com`) — Discord renders that line as text and nobody can click it, so
the best thing it can say is the one thing a reader could type. In prose and titles the product is
still "Clippet".

**The card has one more surface, and it obeys the colour law** (erratum 287). Discord paints a thin
rule down the left edge of an unfurl from the page's `theme-color`, and a share page carries one
unqualified tag after the browser's light/dark pair, LAST, carrying the clip's state: green `#0E7C4F`
ready, amber `#D98E12` encoding, red `#F0665C` failed — "green = you can paste this", "amber =
happening", "red = loss", on the one surface that law had never reached. It adds no token and it
changes nothing a browser sees: a browser takes the FIRST `theme-color` whose media matches and never
reaches this one, Discord takes the last and ignores `media` (measured, not documented). Only a page
that carries a clip's card gets it.

**A green span is production; a grey span is not** (erratum 285 — the rule the production and dev Discord application
icons already set). An instance carrying `INSTANCE_BADGE` — `dev`, `staging`, `lab-test` — renders
`data-badge` on `<html>`, server-side like `data-theme`, and four rules in app.css paint the span
`var(--line-strong)` instead of `var(--ok)`, a face's mouth with it, and the embed overlay's span and
mouth `#837F77` — the dark palette's `--faint`, because its ground is the player's black whatever the
page theme, so it spends that palette outright as it already does for green and text, and that
palette's `--line-strong` (`#514D47`) is too dark on black to read as a grey rather than as a missing
line. **Colour only**: the geometry, the weight, the tracking, the pupils,
the eight faces and the overlay's box are exactly the ones above, so a dev page and a production page
differ in one channel and nothing moves. `sulk` stays `--danger` everywhere — red is loss, and a failed
clip is not something an environment recolours, which is why the two mouth rules are written
`:not(.sad)`: without it the badge's selector outweighs `sulk`'s and a badged instance repeals the
colour law's one exception. The favicon is drawn rather than styled, so it
belongs to the tab and stays green. **After the mark goes ONE word**, `<span class="instance">dev</span>`:
mono 11 / 500, uppercase, lightly tracked, `--muted` on `--surface-2`, a 3 px radius and 2 px 6 px of
padding, on the same line as the mark and 8 px after it — in the header and on the front door, login,
register, the 404 and the 503 alike, which is the one placement everywhere. (It is after the word and
not under it because the span is centred on the double p and not on the word, so a tag centred under
the word disagrees with the only axis the logo has — the owner's call, 2026-09-15.) Beside a DISPLAY mark it rides
in a `.lockup`, a `1fr auto 1fr` grid rendered only on a badged page, with the mark in the middle
column: the outer columns are equal, so the mark stays exactly where the container had centred it and
the tag hangs off the third. On a phone the
header bar is already full, so a badged bar makes `Log out` unwrappable and lets the tag itself
ellipsise at the extreme, rather than scroll the page sideways or grow a second line. It is
`.instance` and not `.badge`, because `.badge` is already the state chip on a poster frame. **Never on
the embed**: the overlay is 16 px of live text over a picture on somebody else's page, "no text beside
it" outranks this, and the grey span is its whole signal there. A badged instance also prefixes every
`<title>` with the word and spells `og:site_name` and the oEmbed `provider_name` `clippet dev`, because
that line is the logo in Discord and a card from the dev box must not read as a card from production.
An instance with no badge renders none of it and is byte-identical to the pages described above.

**The companion mark** (ROADMAP #43, errata 301 and 306; `docs/brand/`) is the wordmark's partner for the
surfaces the word cannot reach — the favicon, the manifest and app icons, the Discord application icons,
the link-out placeholder — and it is NEVER set beside the word: no lockup, no tile in the header, the
rule above stands. One surface is the owner's exception (2026-10-06): a Discord bot's profile banner carries
the wordmark alone on the ink tile (`docs/brand/discord/*-banner-*.png`, drawn by
`scripts/lib/mkbanners.py`), and Discord draws the bot's avatar — the mark — over the banner's corner.
That adjacency is Discord's layout, not a lockup of ours: nothing we draw sets the two together. The
mark is the striped clapper: the "mitre" frame — two stems 272 wide at the wordmark's
own pitch of 1146, the right one grown up to meet a lid 230 thick (the p's bowl stroke) lifted 9° at the
left hinge — with three slashed 40° cuts through the lid, over the wordmark's own green span, 1418 × 200,
one gap of 200 under the stems: every number the wordmark's, in font units. The cuts are HOLES in the
lid's one `fill-rule="evenodd"` path, never paper polygons laid over ink, because an overlay leaves a
hairline at every size that is not a whole multiple of the drawing. **There is no size rule**: this one
drawing is every size, the tab's 16 px included (erratum 306 revoked 301's "stripes at 64 and above, a
plain mitre at 32 and 16" on the owner's call and deleted the mitre from the repository; the generator's
supersampling is what makes the mark read small, and no icon is ever drawn by hand). The ink is the
theme's text colour and the green the theme's one green; on a surface with no
theme — the ink tile of an app icon or a Discord avatar — the mark is `#ECEDEF` and the span `#4CD69A`
on `#17181C`; the DEV application's icon wears the grey span (`#837F77`, erratum 285's rule) and the AUTO
application's the blue (`#2FA8E8`, the one place blue is drawn), each its only difference — three
application icons, one drawing. **The favicon** is the mark itself: the unit geometry of `docs/brand/mark.svg`
(the two stems, the lid's one even-odd path with its three cuts, the span) in a square viewBox of the
mark's own box with a sixteenth of the square as inset, carrying its own
`prefers-color-scheme` rule, percent-encoded in full into a `data:` URI in `base.html` and `embed.html`
(erratum 103: no request, `img-src 'self' data:`). That is the icon a PAGE declares, and it is the only
one that follows the browser's light or dark chrome. `web/static/favicon.ico` is the icon for clients
that never read a page — bookmark tools, some link previewers, Safari's pinned-tab fallback, which ask
`/favicon.ico` unprompted: one ICO of three PNG entries, that same mark at 16, 32 and 48,
all ink on transparent with the green span and NO tile, and — an ICO carrying no
colour-scheme rule — the light theme's ink at every size (erratum 304). **The manifest icons** (`icon-192.png`,
`icon-512.png`) are the striped mark in paper on a rounded ink tile, transparent outside it — an app
icon needs a tile; the header does not. **The link-out card** (`card-placeholder.png`, 1200 × 630 — the
picture Discord puts on a clip that is still encoding and on a failed one for ever) is the mark large on
the left, 340 px tall, with the wordmark small under it at 64 px, `#ECE9E2` and `#4CD69A` on the dark
ground, and no text at all: the card's own title already says `Processing…` or `Processing failed`, and
a picture that repeated it would be the one place in the product where a message is stated twice
(erratum 287). All of it — the nine PNGs (`icon-192.png`, `icon-512.png`, `icon-dev-192.png` and the three
application icons at 512 and 1024), `card-placeholder.png`, `favicon.ico`, `docs/brand/mark.svg`, the favicon
line in both templates — is DRAWN by `scripts/lib/mkicons.py` (the card by `scripts/lib/mkcard.py`, which
imports its rasteriser rather than carrying a second one) and diffed against the generator by `check.sh`;
change the geometry there, never in a file.

**Glyph-to-path recipe** (done once; re-run only if the face or weight changes): `pip install fonttools`;
fetch the family's CSS from the Google Fonts service with a plain curl user agent (it then names TTF
files) and download the 600 and 700 cuts; for each `p`,
`TTFont(path).getGlyphSet()[cmap[ord('p')]].draw(TransformPen(SVGPathPen(gs), (1,0,0,-1,dx,0)))` — flip
y, offset by the glyph origin; advance = `glyph.width + tracking + GPOS pair kern` (pp kerns −8 at 600,
−6 at 700; tracking −0.025em = −50 units). Coordinates stay in font units, baseline at y = 0; place
with one `<g transform="translate(tx ty) scale(s)">` per size. Then re-measure the span (stem edges,
descender) and re-render the favicon and `mkicons.py`.

## What app.js positions, and how

Everything geometric is a percentage or a pixel written to `element.style` from a measurement, never a
style attribute (CSP `style-src 'self'`): the rail fills (`setBar`), the library rows' rail state
(`setRail`), the lifetime rail and its state cell (`paintLifetime`: fraction = time left over the
upload-to-expiry span, from the two `<time datetime>` instants the facts list carries), the filmstrip's
strip inset, both cuts, the kept outline, the handle positions, the handle tags' timecodes and the
tiles (`paintScrubber`, `paintFilm`, `paintTile`). Reveal-on-client is `hidden` toggled (the scrubber,
the lifetime rail, the bulk bar, Cancel/Try again, the row actions); a drag is a class (`.dropping`,
`.dragging`); a state is a class (`.picked`, `.outside`, `.queued`/`.working`). The theme is a cookie
plus `data-theme` on `<html>`.

## One number on a picture

**The poster carries the clip's length, burned into its top-right corner** (erratum 287; the owner's ask: a
large length readout in the thumbnail's top right, since it is only useful before play). `{m:ss}` in Spline Sans Mono 500 — this sheet already gives that face exactly this job,
"things a machine measured", and a duration is a timecode — at 4.5 % of the picture's height with a
14 px floor, on a plate of the dark palette's ground at 72 %, inset 12 px at 720p-equivalent and
scaled with the frame.

It is the one documented exception to "the interface never writes on the video", and it is not the
LIGHT tag's kind of exception: that one is a brand line and needed erratum 214's amendment of *no
watermarks*; this carries no brand, no name and no logo, and is one measurement of the file it is
drawn on — the same relationship a video player's own "3:42" has to its thumbnail. It is on
`poster.jpg` alone and it is gone the moment anybody presses play.

Two rules of this sheet apply and are why it looks the way it does. **On a picture the dark palette is
spent outright**, whatever the page theme — the rule erratum 246 set for the embed overlay — so the
text is `#ECE9E2` and the plate the dark `--bg`. And a number a reader is asked to READ needs real
contrast, not a shadow: composited over white the plate reads 5.8:1 and over black 15.5:1, where the
overlay's bare text would be 1.21:1 on white before its shadow does anything. The overlay is a logo
and may not grow a box; this is a number and must have one.

## What may never appear

- a coloured link (links are ink with an amber marker stroke that fills on hover; nav, buttons, tags,
  thumbnails and the wordmark opt out with `background-image: none`),
- a coloured button fill (amber "primary", green "success"), a green or amber focus ring,
- a shadow (the embed overlay's two-layer text shadow is the one exception, errata 246 and 287: it sits
  on a picture, where no ground can be chosen for it), a wash, a tinted hover, a gradient that is not
  the marker stroke or the rail's sweep,
- a tile, badge or icon beside the wordmark — the ONE exception is the instance tag of erratum 285,
  which is a word and exists on no production page at all; a wordmark drawn as a path in the header,
- the companion mark beside the word, or on any page: it is an icon for the surfaces the word cannot
  reach (the tab, the app icon, the Discord avatar, the placeholder card), not a lockup (erratum 301),
- an inline style, an inline script, a style attribute written from JavaScript, an external font or
  stylesheet, a request to any origin but the stack's — no request leaves the origin, except, on a
  stack delivering media by redirect, the signed media requests to the ONE origin the page's own CSP
  names (erratum 282),
- a rail on something that does not encode time; a lamp that is not a state,
- a call to action, a sign-up nudge, a "made with" line or any brand but the wordmark alone on a watch
  page (the wordmark sits in the header slot, never as a footer credit); any writing burned into a
  picture that is not the one number above,
- all-caps, a second display face, a countdown estimate of an encode.

## Proving it

`scripts/visual.sh` photographs every page in both themes at 1280 and 390 with the official Playwright
container — the embed at 400 × 225 as well, and once more paused after a play when headless playback
works — and asserts zero console errors, zero CSP violations, zero foreign requests, no horizontal
overflow at 390, the faces loaded from `/static/fonts/` (on the embed too, since it draws the overlay),
the ground painted the theme asked for, the embed overlay's box inside the frame and above the
controls band, and the overlay's own TEXT against the host this stack is pointed at — the lockup form
on clippet.gg, plain text elsewhere, each with the accessible name it earns (erratum 287; the pass
reads that host off the app's public base and never off the origin the container reaches it by, and
says out loud where an instance's host cannot carry a mark for the embed's one smile to play on);
`.e2e/visual/index.html` is the contact sheet. It reads both media-delivery shapes
(erratum 282): it reads which one the stack is in off the app's own CSP before the first shot and
prints it, confirms that against what the media routes actually serve once a clip is ready, allows
exactly the signed media requests to that origin, and on a redirect stack requires the watch and
embed pages to have made one — and no signature ever reaches its log. It carries `domTheme` (§20
erratum 288) for the two claims a photograph cannot make: that the bar draws exactly ONE of its two
theme buttons in each of the three theme states, on a light system and a dark one, on a stranger's
share page and on the library — cross-checked against the ground the page actually painted, so a
pinned cookie is seen beating the machine — and that no control on either auth panel is drawn outside
its own `.rc` or its own `.row`, at 1280 and at 390. It also measures the air under the display mark's
span on the four pages that stack text below it (at least 12 px, erratum 288 (e)). None of it costs a
frame: the `library`, `register`, `front` and `notfound` shots in both themes already photograph all
of it. E2E steps 77–79 pin the favicon, the theme-color pair AND the card's own bar last and unqualified after
it (erratum 287), the self-hosted faces, `og:site_name`/`provider_name` as this instance's host, the
theme cookie and the Settings round trip; steps 80–81 pin the front door and the embed overlay (erratum 246); steps 82–84 pin the share link at
the root of the origin and the two id grammars (errata 248–252), and assert that the withdrawn line
about a memorable unlisted link renders for nobody (erratum 264). The instance badge (erratum 285) is
read from the stack rather than assumed: the pass asks the app's own DOM for `data-badge` before the
first shot and prints it, and on a BADGED stack it adds `domBadge` — the attribute, the tag's metrics
and its 8 px after the word, the lockup keeping the mark on the container's centre at 1280 and at 390
with no sideways scroll, the span's and a synthetic mouth's computed colour against `--line-strong`,
that same mouth with `sad` on it against `--danger`, and the embed's one grey with no tag — plus the
two frames `badge-header` and `badge-front`. On an unbadged stack both are
skipped and the pass is the 103 shots it takes since ROADMAP #45 (the wizard's chooser and its first
walk, at both widths in both themes), §20 erratum 311 (the STOP, the same four) and erratum 313 (the upload
card after a pick, one frame); step 77 brings its own
badged container instead.

**What the harness pins at shot time** (lane `speed2`; a harness-side change of what the page does while it is
photographed, never of what it is). A page that carries the clip's player (the watch pages, the embeds) is shot
with Playwright's `animations: "disabled"`: Chromium's loading spinner inside its own media controls turns for as
long as the pinned build (no H.264) keeps trying, and no two takes caught it at one angle — disabled, an infinite
animation sits at its start and a finite one at its end, so the frame is the same every time. A face frame (the
wordmark's expressions) is shot on a paused fake clock run exactly to the face's hold, with a SEEDED
`Math.random`, because the drift's direction is drawn at random and two takes of one hold differed by a few
pixels of pupil; the frame is about the face's shape at its hold. Only the face FRAMES are seeded: `domFaces`'
ambient band keeps the real draw, because its randomness is part of what that check tests. What stays unpinned is
named in `VISUAL_PROVE=1`'s verdict (`MOVING_REGIONS` in `scripts/lib/visual.js`): the spinner on any other
player frame, settings' served-this-month counter and last-seen minute, the owner's first watch view before its
filmstrip exists, and the collection row's moving frame; a shot outside those that differs between two takes
fails the measurement by name.
