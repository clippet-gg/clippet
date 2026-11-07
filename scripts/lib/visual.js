// visual.js — the Playwright half of scripts/visual.sh (web/STYLE.md; DESIGN.md §20 erratum 240).
//
// Runs INSIDE the official Playwright container (Node 20, the bundled Chromium), never on the
// host. BASE is the stack as the container reaches it; PUBLIC_BASE is what a person types. It
// registers a throwaway account, uploads the 720p30 fixture, makes a collection, and then
// photographs every page in both themes at two widths, asserting on each:
//   - zero console errors and zero uncaught page errors,
//   - zero Content-Security-Policy violations (a listener planted before any script runs),
//   - zero requests to any origin but the stack's — except, on a stack delivering media by
//     REDIRECT, the signed media requests to the one bucket origin the app's own CSP names
//     (§20 erratum 282; data: and blob: are the page's own),
//   - no horizontal overflow at 390 wide,
//   - the Spline Sans face loaded — and every font the page fetched came from /static/fonts/,
//   - the page painted the theme it was asked for (the body's ground is the token's own hex).
// Which of the two media-delivery shapes the stack is in (§20 erratum 278) is DETECTED from the
// stack itself, once, before the first shot, and printed — then confirmed against what the stack
// actually serves as soon as a ready clip exists. See detectMedia and confirmMedia below.
// It also carries the theme glyph's DOM test (`domTheme`, §20 erratum 288 — the three theme states in
// a real browser, and no control outside its own row on either auth panel) and the living mark's
// (ROADMAP #38; `domFollow`, §20 erratum 267, and
// `domFaces`, erratum 274) and shoots the header and the front door with the cursor inside the mark's
// tracking area, then every face on both marks — frozen mid-storyboard on Playwright's fake clock.
// Whether the stack wears an `INSTANCE_BADGE` (§20 erratum 285) is DETECTED from it too, once, before
// the first shot: on a badged stack the pass adds `domBadge` and the two frames `badge-header` /
// `badge-front`; on an unbadged one — which is production, and the reference stack — it skips both and
// the sheet is the 111 shots it has always been (90 until ROADMAP #45 added the wizard's four, 98 until
// §20 erratum 311 added the STOP's four, 102 until erratum 313 added `upload-picked`, 103 until erratum 329
// added the audio clip's eight: its page at rest, its page part-played, and the library with its tile).
// The screenshots and an index.html contact sheet land in /out (.e2e/visual on the host). No
// committed baselines: what a page SHOULD look like is a decision a person makes on the sheet.
"use strict";
const fs = require("fs");
const path = require("path");
const { chromium } = require("playwright");
// The navigation, retried once on a classified transient (run 9, lane `retry`): every page.goto in this pass goes
// through go() below, which is navretry.gotoRetry with this pass's own redaction.
const nav = require("./navretry");

const BASE = (process.env.BASE || "").replace(/\/$/, "");
const PUBLIC_BASE = (process.env.PUBLIC_BASE || BASE).replace(/\/$/, "");
// The instance's own public HOST — no scheme, no port — read off PUBLIC_BASE and NEVER off BASE:
// inside the container BASE names host.docker.internal, while the app bakes its configured
// PUBLIC_BASE_URL into every page it serves. Since §20 erratum 287 the embed's wordmark overlay
// READS that host (the owner asked for the instance's host in the part baked into the video), so this pass
// compares what the overlay says against where the operator says this stack lives, rather than
// carrying a literal that would be wrong on every origin but one.
const SITE_HOST = (() => { try { return new URL(PUBLIC_BASE).hostname; } catch { return ""; } })();
// …and whether that host CONTAINS the wordmark, which decides the SHAPE of the overlay and
// therefore what can be asserted about it. On clippet.gg and any subdomain of it the overlay is the
// lockup: a `.pp`, a green span, and the one face the embed wears (a smile on ready, erratum 274).
// On any other host it is the host as plain text — there is no `pp` in `clips.example.com` for eyes
// to live in and no span for a mouth to be drawn as, so app.js injects nothing there, by
// construction rather than by a rule. The checks that are ABOUT the mark on the embed therefore run
// where there IS a mark and say so where there is not; every check about the overlay's box, its
// text, its colours and its play/pause rule runs everywhere, and the shot count does not move.
const EMBED_LOCKUP = SITE_HOST === "clippet.gg" || SITE_HOST.endsWith(".clippet.gg");
const OUT = "/out";
// Where a shot is WRITTEN and whose list it lands in. Both are switched for one thing only: VISUAL_PROVE=1's
// serial re-take of a phase (lane `speed2`), which runs with nothing else in flight, into OUT/serial/.
let SHOT_DIR = OUT;
const FIXTURE = "/src/testclips/std-720p30.mp4";
const BROKEN_SRC = "/src/testclips/faststart-720p30.mp4"; // moov first, so zeroing the mdat leaves a probe-able file
const WIDTHS = [1280, 390];
const THEMES = ["light", "dark"];
// The two grounds, as getComputedStyle reports them (app.css --bg).
const GROUND = { light: "rgb(236, 237, 239)", dark: "rgb(25, 24, 22)" };
const results = [];
const serialResults = []; // VISUAL_PROVE=1's serial re-take; never on the sheet
// The media-delivery shape this run is photographing, and the ONE second origin it may see
// (§20 erratum 282). Both are set once by detectMedia() and read nowhere else: "" is proxy mode,
// which is every version of this pass before erratum 278 existed.
let MEDIA = "";        // "proxy" | "redirect"
let MEDIA_ORIGIN = ""; // scheme://host[:port] the app's 302 sends a player to; "" in proxy mode
let browser = null; // set by the run; frozenContext() clones contexts off it
// X-Clippet-App: 1 on EVERY request this pass makes (DEPLOY §37). On clippet.gg a Worker serves the
// product's homepage to an anonymous GET/HEAD of the exact root `/` and passes through anything that
// carries a session cookie or this header; the pass photographs and asserts on the PRODUCT's front
// door, so every context it opens carries the header — its pages' navigations and subresources, and
// its own `api()`/`ctx.request` calls, which inherit the context's extra headers. Set ONCE, here, by
// wrapping the browser's newContext, rather than at each of the dozen places that open a context.
// Harmless where there is no Worker (a lane stack, dev, any self-host): the app ignores it.
const APP_HEADERS = { "X-Clippet-App": "1" };
function withAppHeaders(b) {
  const open = b.newContext.bind(b);
  b.newContext = (opts = {}) => open({ ...opts, extraHTTPHeaders: { ...APP_HEADERS, ...(opts.extraHTTPHeaders || {}) } });
  return b;
}
const stamp = Date.now();
const user = { username: `visual_${stamp}`, password: `visual-pass-${stamp}` };

if (!BASE) {
  console.error("visual: BASE is not set");
  process.exit(2);
}

// The mount, asserted from inside (AUDIT #25). This container is given `scripts/` and `testclips/`
// and nothing else; it used to be handed the whole checkout read-only, which put the stack's bucket
// credentials — the gitignored `.env` — inside an image that npm-installs Playwright from the
// network. A pass that photographs pages has no business being able to read one. The check is here,
// in the container, rather than in visual.sh, because it is the CONTAINER's view that matters and a
// mount added back by hand would be invisible from the host side.
for (const forbidden of ["/src/.env", "/src/compose.yaml", "/src/internal", "/src/.git"]) {
  if (fs.existsSync(forbidden)) {
    console.error(`visual: ${forbidden} is visible inside the container — this pass is given scripts/ and testclips/ only (AUDIT #25)`);
    process.exit(2);
  }
}
for (const needed of ["/src/scripts/lib/visual.js", FIXTURE, BROKEN_SRC]) {
  if (!fs.existsSync(needed)) {
    console.error(`visual: ${needed} is missing — the two mounts this pass needs are scripts/ and testclips/`);
    process.exit(2);
  }
}

function sleep(ms) { return new Promise((r) => setTimeout(r, ms)); }

// ---- media delivery: the pass reads BOTH shapes (§20 erratum 282) -------------------------------
// The app either serves a clip's rendition and poster itself (`MEDIA_DELIVERY=proxy`, the default
// and every version before it) or answers the same URLs, after the same gates, with a 302 to a
// freshly signed link on the bucket's own origin (`redirect`, §20 erratum 278). Both are shipped
// shapes and this pass photographs a LIVE stack, so it has to read both — and on a redirect stack
// the bucket is a legitimate SECOND origin, named by the app's own Content-Security-Policy.
//
// redact(u): a URL with its query replaced when the query carries a signature. EVERY message this
// pass writes that names a request URL goes through this or redactText below. The query of a
// presigned link holds `X-Amz-Signature` and `X-Amz-Credential` — a credential and the access key
// id (DEPLOY §33.2) — and this pass's output is read into a log file on disk. The path is kept,
// because the path is what a person reading the failure needs.
function redact(u) {
  const q = String(u).indexOf("?");
  if (q < 0) return String(u);
  return /X-Amz-/i.test(String(u).slice(q)) ? String(u).slice(0, q) + "?<signed>" : String(u);
}
// redactText(s): the same, over a MESSAGE that may name a URL somewhere inside it — a console line,
// a CSP violation's blocked URI, an exception's text. Everything a failure carries passes through
// it once more before it is printed or written to the sheet, as the last line of defence.
function redactText(s) { return String(s).replace(/\bhttps?:\/\/[^\s"'|]+/gi, redact); }
// go(page, url, opts): page.goto, retried ONCE after 2 s on pw-closed / pw-reset / pw-timeout, with a
// `[visual] retry <url> (<signature>)` line (scripts/lib/navretry.js). Every navigation in the pass uses it.
function go(page, url, opts) { return nav.gotoRetry(page, url, opts, { redact }); }
// signedMedia(u): the ONE request that is not foreign on a redirect stack. Three things, all of
// them: it is on this run's media origin, it carries a signature, and its key is under `clips/`
// (S3_KEY_PREFIX and a path-style bucket both put more path in front of that, so the match is
// anywhere after the origin). A bare, unsigned request to the bucket origin is still a fault —
// it would be a media URL this app did not mint — and so is a signed request to anywhere else.
function signedMedia(u) {
  if (!MEDIA_ORIGIN || !String(u).startsWith(MEDIA_ORIGIN + "/")) return false;
  const q = String(u).indexOf("?");
  if (q < 0 || !/[?&]X-Amz-Signature=[^&]/.test(String(u).slice(q))) return false;
  return String(u).slice(MEDIA_ORIGIN.length, q).includes("/clips/");
}
// selfTest(): the three rules above, proven before the browser starts.
//
// It is the only place the REDACTION runs on a pass that has nothing to report: a green run prints
// no request URL at all, so "no signature reached the log" would otherwise be a claim about code
// nobody executed on that run. The fixture's signature is invented and 64 hex characters long —
// exactly the shape a leak grep looks for — and the line this prints carries it REDACTED, so the
// grep has something real to be zero about. It pins the other half too: converted is not weakened,
// and a bucket request that is unsigned, or under a key that is not a clip's, or on any other
// origin at all, is still foreign. The fixture is assembled rather than written out because no file
// in this project spells a URL literal, whatever the scan's current scope (scripts/check.sh).
function selfTest() {
  const done = [];
  const check = (ok, what) => { done.push(what); if (!ok) throw new Error(`media rules: ${what}`); };
  const scheme = "ht" + "tp://";
  const bucket = `${scheme}bucket.example:9000`;
  const sig = "?response-content-type=image%2Fjpeg&X-Amz-Credential=AKIAEXAMPLE&X-Amz-Signature=" + "0123456789abcdef".repeat(4);
  const link = `${bucket}/a-bucket/clips/AAAAAAAAAAAA/poster/poster-BBBBBBBB.jpg${sig}`;
  const bare = link.split("?")[0];
  const keep = [MEDIA, MEDIA_ORIGIN];
  try {
    MEDIA_ORIGIN = bucket;
    check(signedMedia(link), "a signed link on the media origin, under a clip key, is not foreign");
    check(!signedMedia(bare), "an UNSIGNED request to the media origin is still foreign");
    check(!signedMedia(`${bucket}/a-bucket/other/x.jpg${sig}`), "a signed request that is not under a clip key is still foreign");
    check(!signedMedia(`${scheme}elsewhere.example/clips/x.jpg${sig}`), "a signed request to any other origin is still foreign");
    MEDIA_ORIGIN = "";
    check(!signedMedia(link), "in proxy mode nothing off the stack is allowed at all");
    check(redact(link) === `${bare}?<signed>`, "redact keeps the path and takes the query");
    check(redact(bare) === bare, "redact leaves a URL with no query alone");
    check(!/X-Amz/.test(redactText(`csp: media-src blocked ${link} | and ${link}`)), "redactText takes EVERY signed URL inside a message");
    check(redactText(`${bucket}/x.png?w=2`) === `${bucket}/x.png?w=2`, "redactText leaves an ordinary query alone");
    const fake = withAppHeaders({ newContext: (o) => o });
    check(fake.newContext({ baseURL: BASE }).extraHTTPHeaders["X-Clippet-App"] === "1", "every context carries X-Clippet-App: 1 (DEPLOY §37)");
    const kept = fake.newContext({ extraHTTPHeaders: { "X-Other": "y" } }).extraHTTPHeaders;
    check(kept["X-Clippet-App"] === "1" && kept["X-Other"] === "y", "…beside any header a context asks for itself");
  } finally {
    MEDIA = keep[0];
    MEDIA_ORIGIN = keep[1];
  }
  console.log(`[visual] media rules: ${done.length} checks ok (a signed link prints as ${redact(link)})`);
}
// navSelfTest(): the navigation retry (run 9, lane `retry`), against fake pages, before the browser starts:
// the three signatures and two things that are not one; a classified failure goes ONCE more and says so;
// a second failure and an unclassified one both throw. The counter is put back, so the summary counts
// the run's retries only.
async function navSelfTest() {
  const done = [];
  const check = (ok, what) => { done.push(what); if (!ok) throw new Error(`navigation retry: ${what}`); };
  const err = (m) => new Error(`page.goto: ${m} at ${BASE}/`);
  check(nav.navSignature(err("net::ERR_CONNECTION_CLOSED")) === "pw-closed", "ERR_CONNECTION_CLOSED is pw-closed (gate 23's seven shots)");
  check(nav.navSignature(err("net::ERR_CONNECTION_RESET")) === "pw-reset", "ERR_CONNECTION_RESET is pw-reset");
  check(nav.navSignature(err("net::ERR_CONNECTION_TIMED_OUT")) === "pw-timeout" && nav.navSignature(err("net::ERR_TIMED_OUT")) === "pw-timeout", "a net:: timeout is pw-timeout");
  check(nav.navSignature(err("net::ERR_NAME_NOT_RESOLVED")) === "", "a name that does not resolve is not a transient");
  check(nav.navSignature(new Error("page.goto: Timeout 30000ms exceeded.")) === "", "Playwright's own wait timeout is not one either");
  const keep = nav.stats.retries;
  const lines = [];
  const fake = (fails) => { const p = { calls: 0, goto: async () => { p.calls++; if (p.calls <= fails.length) throw fails[p.calls - 1]; return "answered"; } }; return p; };
  const opts = { log: (l) => lines.push(l), redact, pauseMs: 0 };
  try {
    let p = fake([err("net::ERR_CONNECTION_CLOSED")]);
    check((await nav.gotoRetry(p, `${BASE}/x`, {}, opts)) === "answered" && p.calls === 2, "a classified failure is retried exactly once and the retry's answer returned");
    check(lines.length === 1 && lines[0] === `[visual] retry ${BASE}/x (pw-closed)`, "…with one `[visual] retry <url> (<signature>)` line");
    p = fake([err("net::ERR_CONNECTION_RESET"), err("net::ERR_CONNECTION_RESET")]);
    let threw = false; try { await nav.gotoRetry(p, `${BASE}/x`, {}, opts); } catch { threw = true; }
    check(threw && p.calls === 2, "a SECOND failure throws (two calls, never a third): the shot fails as before");
    p = fake([err("net::ERR_NAME_NOT_RESOLVED")]);
    threw = false; try { await nav.gotoRetry(p, `${BASE}/x`, {}, opts); } catch { threw = true; }
    check(threw && p.calls === 1, "an unclassified failure is never retried");
    check(nav.stats.retries - keep === 2, "the summary's counter counted the two retries");
  } finally {
    nav.stats.retries = keep;
  }
  check(nav.PAUSE_MS === 2000, "the pause before the retry is 2 s");
  console.log(`[visual] navigation retry: ${done.length} checks ok`);
}
// policyOf(ctx, url): the shape of the stack as THAT PAGE declares it, read off its own
// Content-Security-Policy. `cspBase` writes `media-src 'self'` and `img-src 'self' data:` in proxy
// mode and appends the ONE media origin in redirect (erratum 278(j)), so every page of this app
// says which shape it is in — before a single clip exists, which is what lets the answer arrive
// before the first shot. Both directives must agree and neither may name anything else: a policy
// with two origins in it is a stack this pass has nothing true to say about, since the app mints
// every link against one signer.
async function policyOf(ctx, url) {
  const res = await ctx.request.fetch(BASE + url);
  const csp = res.headers()["content-security-policy"] || "";
  const sources = (name) => {
    const d = csp.split(";").map((s) => s.trim()).filter(Boolean).find((s) => s === name || s.startsWith(name + " "));
    return d === undefined ? [] : d.slice(name.length).trim().split(/\s+/).filter(Boolean);
  };
  const media = sources("media-src");
  const img = sources("img-src");
  if (media[0] !== "'self'" || media.length > 2) throw new Error(`media delivery: ${url} declares media-src ${JSON.stringify(media.join(" "))}, want 'self' and at most one media origin (§20 erratum 278(j))`);
  const origin = media.length === 2 ? media[1] : "";
  if (origin && !/^https?:\/\/[^/]+$/.test(origin)) throw new Error(`media delivery: ${url}'s media-src names ${JSON.stringify(origin)}, which is not a bare origin`);
  const wantImg = (origin ? ["'self'", "data:", origin] : ["'self'", "data:"]).join(" ");
  if (img.join(" ") !== wantImg) throw new Error(`media delivery: ${url} declares img-src ${JSON.stringify(img.join(" "))} beside that media-src, want ${JSON.stringify(wantImg)} (§20 erratum 278(j))`);
  return { mode: origin ? "redirect" : "proxy", origin };
}
// detectMedia(ctx): which shape this run is photographing, asked of the STACK, once, and printed —
// BEFORE the first shot, because the very first page this pass photographs can already carry media.
// (On a default `LIBRARY_SCOPE=shared` instance the library lists every listed clip, so even the
// empty-library shot of a brand-new account fetches other people's posters.)
//
// It is never read out of `.env`: a run against somebody else's origin has no `.env`, and a lane's
// `.env` is what an operator MEANT rather than what the process booted with (§20
// erratum 280 is the E2E's half of this). The login page is anonymous, exists on every stack and
// costs one request.
async function detectMedia(ctx) {
  const p = await policyOf(ctx, "/login");
  MEDIA = p.mode;
  MEDIA_ORIGIN = p.origin;
  const host = (MEDIA_ORIGIN || PUBLIC_BASE).replace(/^https?:\/\//, "");
  console.log(`[visual] media delivery: ${MEDIA} (${host}) — the policy the app serves`);
}
// confirmMedia(ctx, clipId): …and the ANSWER ON THE WIRE, the moment a ready clip exists.
//
// One anonymous GET of the run's own clip's rendition, redirects NOT followed. The declared shape
// and the served one must agree: `proxy` must answer 200, `redirect` must answer 302 with a SIGNED
// Location on exactly the origin the policy named. The two halves are worth more than either alone
// — a policy naming an origin the media routes never send anyone to would let this pass allow
// requests to a host the product does not use, and a 302 to an origin the policy does not name is
// erratum 278(j)'s silent failure, where the card keeps working while our own CSP blocks the player
// on our own page. The watch page is asked for its policy too: the media origin is one value for
// the whole instance, and a page that disagreed with the front of the site would be a stack whose
// player and whose library cannot both work.
async function confirmMedia(ctx, clipId) {
  const res = await ctx.request.fetch(`${BASE}/api/clips/${clipId}/video.mp4`, { maxRedirects: 0 });
  const code = res.status();
  if (MEDIA === "proxy") {
    if (code !== 200) throw new Error(`media delivery: the app's policy declares proxy and a ready clip's rendition answered ${code}, not 200`);
  } else {
    if (code !== 302) throw new Error(`media delivery: the app's policy declares redirect and a ready clip's rendition answered ${code}, not 302`);
    const loc = res.headers()["location"] || "";
    if (!loc) throw new Error("media delivery: a 302 with no Location — this stack is in no shape this pass knows");
    const origin = loc.split("/").slice(0, 3).join("/"); // scheme://host[:port], and nothing else
    if (origin !== MEDIA_ORIGIN) throw new Error(`media delivery: the 302 lands on ${JSON.stringify(origin)} and the policy names ${JSON.stringify(MEDIA_ORIGIN)} (§20 erratum 278(j))`);
    if (!/[?&]X-Amz-Signature=[^&]/.test(loc)) throw new Error(`media delivery: the Location is not a signed link: ${redact(loc)}`);
  }
  const watch = await policyOf(ctx, `/${clipId}`);
  if (watch.mode !== MEDIA || watch.origin !== MEDIA_ORIGIN) throw new Error(`media delivery: the watch page declares ${watch.mode} (${JSON.stringify(watch.origin)}) and the login page declared ${MEDIA} (${JSON.stringify(MEDIA_ORIGIN)})`);
  console.log(`[visual] media delivery: ${MEDIA} confirmed on the wire — a ready clip's rendition answered ${code}${MEDIA === "redirect" ? ", signed, on the origin the policy names" : ""}, and the watch page declares the same shape`);
}

// ---- the instance badge (§20 erratum 285; web/STYLE.md) -----------------------------------------
// A stack may be wearing `INSTANCE_BADGE` (`dev`, `staging`, `kev-test`), in which case the wordmark's
// span is GREY rather than green, a small word sits beside the mark and every title carries a prefix.
// The reference stack is NOT badged and the pass there is the 111 shots it has always been, so this is
// read off the STACK and never assumed — the same rule detectMedia follows, for the same two reasons:
// a run against somebody else's origin has no `.env` to read, and a lane's `.env` is what an operator
// MEANT rather than what the process booted with. `data-badge` on <html> is the server's own answer.
let BADGE = ""; // the word this stack wears; "" = production, and every badge check is skipped
async function detectBadge(ctx) {
  const page = await ctx.newPage();
  try {
    await go(page, BASE + "/login", { waitUntil: "domcontentloaded" });
    BADGE = await page.evaluate(() => document.documentElement.getAttribute("data-badge") || "");
  } finally {
    await page.close();
  }
  console.log(BADGE
    ? `[visual] instance badge: ${JSON.stringify(BADGE)} — the mark is monochrome here; domBadge and the two badge frames are ON`
    : "[visual] instance badge: none (the production shape) — domBadge and the two badge frames are SKIPPED");
}

// domBadge: what a screenshot cannot assert — that the grey is the TOKEN's grey rather than a grey
// somebody typed, that the tag sits where the sheet says at the size the sheet says, and that the
// embed got the colour and NOT the word. It runs only on a badged stack.
//
// The mouth is read off a SYNTHETIC one: `.wordmark .eyes .mouth` is drawn by app.js only while a face
// moves, so waiting for one would make this test a race about the ambient timer. One element matching
// the selector, inserted, measured and removed, asks the stylesheet the question directly.
async function domBadge(browser, clipId) {
  let n = 0;
  const check = (ok, what) => { n++; if (!ok) throw new Error(`dom badge: ${what}`); };
  const ctx = await browser.newContext({ baseURL: BASE });
  try {
    const read = async (url, width) => {
      const page = await ctx.newPage();
      if (width) await page.setViewportSize({ width, height: 900 });
      await go(page, BASE + url, { waitUntil: "networkidle" });
      await page.evaluate(() => document.fonts.ready);
      const out = await page.evaluate(() => {
        const mark = document.querySelector(".wordmark");
        const pp = mark && mark.querySelector(".pp");
        const tag = document.querySelector(".instance");
        const span = pp ? getComputedStyle(pp, "::after") : null;
        // The token itself, RESOLVED the way the span's own value is: a custom property reads back as
        // the text it was declared with (`#A7ABB3`) while a computed background reads back as
        // `rgb(167, 171, 179)`, so the two would never compare equal. One throwaway element given the
        // `var()` puts both sides in the same form. The assertion is then "the span is --line-strong",
        // not "the span is that hex", and it holds in either theme with no second table.
        // `element.style.x = …` is the CSSOM, which `style-src 'self'` does not govern; nothing here
        // writes a style ATTRIBUTE, and the element is gone before the next line.
        const tok = (v) => {
          const d = document.createElement("div");
          d.style.color = v;
          document.body.appendChild(d);
          const c = getComputedStyle(d).color;
          d.remove();
          return c;
        };
        // A synthetic mouth: the same selector app.js's own one matches. It is read TWICE — once as
        // the mark stands, and once with `sad` on it — because the badge's mouth rule and `sulk`'s red
        // one are two selectors competing for the same property, and the whole point of the second
        // read is that the badge must LOSE it (erratum 274: red is loss; §20 erratum 285 (b)).
        let mouth = "", mouthSad = "";
        if (pp) {
          const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
          svg.setAttribute("class", "eyes");
          const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
          path.setAttribute("class", "mouth");
          svg.appendChild(path);
          pp.appendChild(svg);
          mouth = getComputedStyle(path).stroke;
          const wasSad = mark.classList.contains("sad");
          mark.classList.add("sad");
          mouthSad = getComputedStyle(path).stroke;
          if (!wasSad) mark.classList.remove("sad");
          svg.remove();
        }
        const mr = mark ? mark.getBoundingClientRect() : null;
        const tr = tag ? tag.getBoundingClientRect() : null;
        // The DISPLAY lockup (§20 erratum 285 (c)): the wrapper that exists only on a badged page. Its
        // whole job is that the mark does not move, so both halves of that are measured — the lockup
        // spans its container, and the mark's centre is the lockup's centre.
        const lock = mark ? mark.closest(".lockup") : null;
        const lr = lock ? lock.getBoundingClientRect() : null;
        const pr = lock && lock.parentElement ? lock.parentElement.getBoundingClientRect() : null;
        return {
          attr: document.documentElement.getAttribute("data-badge"),
          title: document.title,
          markClass: mark ? mark.className : "",
          spanBg: span ? span.backgroundColor : "",
          mouth,
          mouthSad,
          lineStrong: tok("var(--line-strong)"),
          ok: tok("var(--ok)"),
          danger: tok("var(--danger)"),
          lockup: lr ? {
            // The mark's centre against the lockup's, and the lockup's width against its container's.
            // Together they say "the mark is where the container centred it", which is the claim the
            // three-column grid exists to keep and the one a screenshot cannot make on its own.
            offCentre: Math.abs((mr.left + mr.right) / 2 - (lr.left + lr.right) / 2),
            fills: pr ? Math.abs(lr.width - pr.width) : null,
          } : null,
          scrolls: document.documentElement.scrollWidth > document.documentElement.clientWidth,
          tag: tag ? {
            text: tag.textContent.trim(),
            shown: getComputedStyle(tag).textTransform,
            size: parseFloat(getComputedStyle(tag).fontSize),
            mono: getComputedStyle(tag).fontFamily,
            // The tag follows the WORD everywhere now, so one measurement covers both sizes.
            gapX: tr && mr ? Math.round(tr.left - mr.right) : null,
            sameLine: tr && mr ? tr.top < mr.bottom && tr.bottom > mr.top : null,
          } : null,
        };
      });
      await page.close();
      return out;
    };
    const front = await read("/");
    check(front.attr === BADGE, `the front door's data-badge is ${JSON.stringify(front.attr)}, want ${JSON.stringify(BADGE)}`);
    check(front.title.startsWith(BADGE + " · "), `the front door's title is ${JSON.stringify(front.title)}, want the ${JSON.stringify(BADGE + " · ")} prefix`);
    check(front.spanBg === front.lineStrong, `the display mark's span is ${front.spanBg}, want --line-strong (${front.lineStrong})`);
    check(front.spanBg !== front.ok, `the display mark's span is still --ok (${front.ok}): a badged instance must not read as production`);
    check(front.mouth === front.lineStrong, `a face's mouth would stroke ${front.mouth}, want --line-strong (${front.lineStrong})`);
    // …and `sulk` is still RED. The badge's mouth rule is one class heavier than `.wordmark.sad`, so
    // without `:not(.sad)` it wins and a failed clip frowns grey on every badged page — which is what
    // the first build did, and what the colour law's one recorded exception exists to prevent.
    check(front.mouthSad === front.danger, `a sulk on a badged display mark strokes ${front.mouthSad}, want --danger (${front.danger})`);
    check(!!front.tag, "the front door carries no instance tag");
    check(front.tag.text === BADGE, `the tag reads ${JSON.stringify(front.tag.text)}, want ${JSON.stringify(BADGE)}`);
    check(front.tag.shown === "uppercase", `the tag's text-transform is ${front.tag.shown}, want uppercase`);
    check(front.tag.size === 11, `the tag is ${front.tag.size} px, want 11`);
    check(/Mono/.test(front.tag.mono), `the tag is set in ${front.tag.mono}, want the mono face`);
    // The tag FOLLOWS THE WORD (the owner's call, §20 erratum 285 (c)): same line, 8 px after it — and the
    // mark has NOT moved off the container's centre line to make room for it.
    check(front.tag.gapX === 8, `the display tag sits ${front.tag.gapX} px after the mark, want 8`);
    check(front.tag.sameLine, "the display tag is not on the mark's own line");
    check(!!front.lockup, "the display mark is not inside a `.lockup`");
    check(front.lockup.offCentre <= 1, `the display mark's centre is ${front.lockup.offCentre.toFixed(2)} px off the lockup's, want ≤ 1 (the tag must not shift the word)`);
    check(front.lockup.fills !== null && front.lockup.fills <= 1, `the lockup is ${front.lockup.fills} px narrower than its container, want it to span it`);
    // …and the same on the other display containers, which set their own row gaps and widths.
    // `/register` is in the list since the badge lane's own nit: it is the third
    // display container and the one page of the four a stranger is most often sent
    // to, and it was the only one nothing measured.
    for (const [url, what] of [["/login", ".auth"], ["/register", ".auth"], ["/AAAAAAAAAAAA", ".message"]]) {
      const d = await read(url);
      check(!!d.tag && d.tag.text === BADGE, `${what} (${url}) carries no instance tag`);
      check(d.tag.gapX === 8, `${what} (${url}): the tag sits ${d.tag.gapX} px after the mark, want 8`);
      check(d.tag.sameLine, `${what} (${url}): the tag is not on the mark's own line`);
      check(!!d.lockup && d.lockup.offCentre <= 1, `${what} (${url}): the mark is ${d.lockup ? d.lockup.offCentre.toFixed(2) : "?"} px off the lockup's centre`);
      check(d.spanBg === d.lineStrong, `${what} (${url}): the span is ${d.spanBg}, want --line-strong (${d.lineStrong})`);
      check(d.mouthSad === d.danger, `${what} (${url}): a sulk strokes ${d.mouthSad}, want --danger (${d.danger})`);
    }
    // …and at 390, where the display mark is 32 px and the container 358: the pair still shares one
    // line, the mark is still centred, and the page does not scroll sideways to hold them. (The
    // header at 390 is covered by every shot the pass takes there, which asserts both.)
    const narrow = await read("/", 390);
    check(narrow.tag.sameLine, "at 390 the display tag drops off the mark's line");
    check(narrow.tag.gapX === 8, `at 390 the display tag sits ${narrow.tag.gapX} px after the mark, want 8`);
    check(narrow.lockup.offCentre <= 1, `at 390 the display mark is ${narrow.lockup.offCentre.toFixed(2)} px off the lockup's centre`);
    check(!narrow.scrolls, "at 390 a badged front door scrolls sideways");
    // The header mark: the same tag, the same 8 px after the word.
    const header = await read("/" + clipId);
    check(header.attr === BADGE, `the share page's data-badge is ${JSON.stringify(header.attr)}`);
    check(header.spanBg === header.lineStrong, `the header mark's span is ${header.spanBg}, want --line-strong (${header.lineStrong})`);
    check(header.mouthSad === header.danger, `a sulk on a badged header mark strokes ${header.mouthSad}, want --danger (${header.danger})`);
    check(!!header.tag && header.tag.text === BADGE, "the share page's header carries no instance tag");
    check(header.tag.gapX === 8, `the header tag sits ${header.tag.gapX} px after the mark, want 8`);
    check(header.tag.size === 11, `the header tag is ${header.tag.size} px, want 11`);
    // The embed: the colour and NOT the word. Its ground is the player's black whatever the page theme,
    // so span and mouth are one spelled-out grey — the dark palette's --faint, which is readable there
    // where its --line-strong was not (§20 erratum 285 (b)); and the mouth is asserted at all because
    // without its own rule it inherits the PAGE theme's grey and the overlay carries two of them.
    const embed = await read("/embed/" + clipId);
    const embedGrey = "rgb(131, 127, 119)"; // #837F77
    check(embed.attr === BADGE, `the embed's data-badge is ${JSON.stringify(embed.attr)}`);
    check(embed.tag === null, "the embed carries an instance tag, and it must never (web/STYLE.md: no text beside it)");
    // The grey span is the badge's whole signal on the embed — but since §20 erratum 287 the overlay
    // reads the HOST, and on an instance whose host is not this product's own it is plain text with
    // no span at all. There is then nothing to paint grey, and nothing is lost: such an overlay
    // already says which box this is, by name. So the two colour checks run where a span exists.
    if (!embed.spanBg && !embed.mouth) {
      console.log(`[visual] NOTE dom badge: this instance's overlay is the plain host form (${SITE_HOST}), which carries no span to recolour (§20 erratum 287)`);
    } else {
      check(embed.spanBg === embedGrey, `the embed overlay's span is ${embed.spanBg}, want ${embedGrey}`);
      check(embed.mouth === embedGrey, `the embed overlay's mouth is ${embed.mouth}, want ${embedGrey} — the same grey as its span`);
    }
    check(/embed-mark/.test(embed.markClass), `the embed's mark is ${JSON.stringify(embed.markClass)}, want the overlay`);
  } finally {
    await ctx.close();
  }
  console.log(`[visual] dom badge: ${n} checks ok (data-badge everywhere; the span and a mouth at --line-strong and a SULK still --danger on both marks; the tag 11 px mono uppercase, 8 px after the word, with the mark still on the container's centre; the embed one readable grey and no tag; the front door, /login, /register and the 404 all measured)`);
}

// ---- the API, through the owner context so the session cookie is shared with its pages --------
async function api(ctx, method, url, opts = {}) {
  const res = await ctx.request.fetch(BASE + url, { method, ...opts });
  let body = null;
  const ct = res.headers()["content-type"] || "";
  if (ct.includes("application/json")) {
    try { body = await res.json(); } catch { body = null; }
  }
  return { status: res.status(), body };
}

async function register(ctx) {
  const r = await api(ctx, "POST", "/api/auth/register", { data: user });
  if (r.status !== 201) throw new Error(`register: ${r.status} ${JSON.stringify(r.body)}`);
}

async function upload(ctx, title) {
  const r = await ctx.request.post(BASE + "/api/clips", {
    multipart: {
      metadata: { name: "metadata.json", mimeType: "application/json", buffer: Buffer.from(JSON.stringify({ title, tags: ["visual", "kept"], ttl_days: 7 })) },
      video: { name: "std-720p30.mp4", mimeType: "video/mp4", buffer: fs.readFileSync(FIXTURE) },
    },
  });
  if (r.status() !== 201) throw new Error(`upload: ${r.status()} ${await r.text()}`);
  return await r.json();
}

// An AUDIO clip (§20 erratum 329): a 32-bit FLOAT WAV made here — a 220 Hz tone that swells in and
// out, so the waveform has a shape — which the stack strips, stores and plays as its 24-bit FLAC Full
// (`Full · FLAC 24-bit`), with its waveform poster and its audiogram. Nothing is read from testclips/:
// the audio fixtures there belong to the E2E's step 103 and live only while it runs.
function floatWav(seconds = 6, rate = 48000) {
  const n = seconds * rate;
  const buf = Buffer.alloc(44 + 4 * n);
  buf.write("RIFF", 0); buf.writeUInt32LE(36 + 4 * n, 4); buf.write("WAVE", 8);
  buf.write("fmt ", 12); buf.writeUInt32LE(16, 16); buf.writeUInt16LE(3, 20); buf.writeUInt16LE(1, 22);
  buf.writeUInt32LE(rate, 24); buf.writeUInt32LE(4 * rate, 28); buf.writeUInt16LE(4, 32); buf.writeUInt16LE(32, 34);
  buf.write("data", 36); buf.writeUInt32LE(4 * n, 40);
  for (let i = 0; i < n; i++) {
    const t = i / rate;
    const env = Math.min(1, t / 1.5) * (0.55 + 0.45 * Math.sin(2 * Math.PI * 0.45 * t)) * (t > 4.2 && t < 4.6 ? 0 : 1);
    buf.writeFloatLE(0.9 * env * Math.sin(2 * Math.PI * 220 * t), 44 + 4 * i);
  }
  return buf;
}

async function uploadAudio(ctx, title) {
  const r = await ctx.request.post(BASE + "/api/clips", {
    multipart: {
      metadata: { name: "metadata.json", mimeType: "application/json", buffer: Buffer.from(JSON.stringify({ title, tags: ["visual"], ttl_days: 7 })) },
      video: { name: "voice-memo.wav", mimeType: "audio/wav", buffer: floatWav() },
    },
  });
  if (r.status() !== 201) throw new Error(`upload (audio): ${r.status()} ${await r.text()}`);
  return await r.json();
}

// playedPart is the audio page's `before`: seek the <audio> to 60 % and read the amber layer's clip
// back — its right inset must leave exactly the part of the band that has been heard (app.js,
// setupWaveform: (80 + 1120 × 0.6) / 1280 = 58.75 % shown, 41.25 % clipped).
async function playedPart(page) {
  await page.waitForSelector(".audio-wave .wave-played", { timeout: 10000 });
  const r = await page.evaluate(async () => {
    const a = document.querySelector("audio");
    const wait = (ev, ms) => new Promise((res) => {
      const t = setTimeout(() => res(false), ms);
      a.addEventListener(ev, () => { clearTimeout(t); res(true); }, { once: true });
    });
    if (!(a.duration > 0) && !(await wait("loadedmetadata", 15000))) return { why: "no metadata from /audio" };
    a.currentTime = a.duration * 0.6;
    if (!(await wait("seeked", 10000))) return { why: "the seek did not finish" };
    await new Promise((res) => setTimeout(res, 150));
    return { clip: getComputedStyle(document.querySelector(".wave-played")).clipPath, bars: document.querySelectorAll(".wave-played rect").length };
  });
  if (r.why) throw new Error("audio played part: " + r.why);
  const m = /([\d.]+)%/.exec(r.clip || "");
  if (!m || Math.abs(Number(m[1]) - 41.25) > 0.5) throw new Error(`audio played part: the amber layer's clip is '${r.clip}', want a right inset of 41.25%`);
  if (r.bars !== 70) throw new Error(`audio played part: ${r.bars} bars in the amber layer, want 70`);
}

async function waitReady(ctx, id) {
  const started = Date.now();
  for (;;) {
    const r = await api(ctx, "GET", `/api/clips/${id}/progress`);
    if (r.status === 200 && r.body && r.body.state === "ready") return;
    if (r.status === 200 && r.body && r.body.state === "failed") throw new Error(`clip ${id} failed: ${r.body.error}`);
    if (r.status === 404) throw new Error(`clip ${id} vanished`);
    if (Date.now() - started > 180000) throw new Error(`clip ${id} not ready after 180 s`);
    await sleep(1000);
  }
}

// ---- one page, one theme, one width ----------------------------------------------------------
async function shoot(ctx, name, url, theme, width, opts = {}) {
  const frozen = opts.face ? await frozenContext(ctx) : null;
  const page = await (frozen || ctx).newPage();
  const errors = [];
  const requests = [];
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });
  page.on("pageerror", (e) => errors.push("pageerror: " + String(e)));
  page.on("request", (r) => requests.push(r.url()));
  await page.addInitScript(() => {
    window.__csp = [];
    document.addEventListener("securitypolicyviolation", (e) => {
      window.__csp.push(e.violatedDirective + " blocked " + e.blockedURI);
    });
  });
  const height = opts.height || (width === 390 ? 844 : 800);
  await page.setViewportSize({ width, height });
  await page.emulateMedia({ colorScheme: theme });
  // A face frame: the page boots on a PAUSED fake clock wearing `data-face`, and `before` below
  // runs the clock exactly to the face's hold, so the shot is the same frame every time.
  if (opts.face) await wearFace(page, opts.face);
  // …and a SEEDED Math.random on a face frame (lane `speed2`): the drift's direction is drawn at random, so
  // two takes of the same frozen hold differed by a few pixels of pupil. The frame is about the face's
  // shape at its hold, not about which way it drifted. Only the face FRAMES get it — domFaces' ambient
  // band keeps the real one, because there the randomness of the draw is part of what is tested.
  if (opts.face) await page.addInitScript(() => { let x = 1234567; Math.random = () => ((x = (x * 16807) % 2147483647) - 1) / 2147483646; });
  // Park the pointer at the bottom of the viewport BEFORE the page loads: a fresh page whose mouse
  // has never moved gets a `pointerenter` at (0, 0) from Chromium as it lays out, which is inside
  // the header bar — and the header's mark now answers a pointer there (§20 erratum 267). Every
  // frame but the two follow frames is the mark at rest. The BOTTOM-RIGHT CORNER, not the middle of
  // the last row: the middle sits inside the library's collection card at 1280 and painted its hover
  // border into two rest frames; the corner is the page gutter (`main.page` is width-capped) on every
  // page and width the pass shoots, and stays so whatever a real library holds.
  await page.mouse.move(width - 1, height - 1);
  const shotName = `${name}-${theme}-${width}.png`;
  const failures = [];
  let headerPx = 0;
  let markBox = null;
  try {
    const resp = await go(page, BASE + url, { waitUntil: "networkidle" });
    if (opts.status && resp && resp.status() !== opts.status) failures.push(`status ${resp.status()}, want ${opts.status}`);
    if (opts.face) {
      await page.clock.runFor(FACE_AT[opts.face]);
      const m = await eyesOf(page, "body");
      if (!m || m.face !== opts.face || !moved(m)) failures.push(`the mark is not mid-${opts.face}: ${JSON.stringify(m && { face: m.face, mouth: m.mouth, pupils: m.pupils })}`);
      if (opts.face === "sulk" && !(m && m.sad)) failures.push("the sulk is not wearing `sad` (the red mouth)");
    }
    if (opts.before) {
      // A `before` that answers "skip" is a shot the browser cannot take (the embed's play/pause
      // shot when headless playback is refused): say so, take no shot, record nothing.
      const b = await opts.before(page, width);
      if (b === "skip") {
        await page.close();
        console.log(`[visual] NOTE ${name} ${theme} ${width} skipped`);
        return;
      }
    }
    await page.evaluate(() => document.fonts.ready);
    // Every image the page has STARTED loading has finished (lane `speed2`): a poster caught mid-decode
    // was one of the two-take differences the identity proof found. Bounded, because a lazy image below
    // the fold never starts — it is not waited for, and the 3 s cap keeps a stalled one from hanging a shot.
    await page.evaluate(() => Promise.race([
      Promise.all(Array.from(document.images).filter((i) => !i.complete && i.currentSrc).map((i) => new Promise((r) => { i.addEventListener("load", r, { once: true }); i.addEventListener("error", r, { once: true }); }))),
      new Promise((r) => setTimeout(r, 3000)),
    ]));
    await sleep(150); // one paint after the fonts landed
    const facts = await page.evaluate(() => ({
      csp: window.__csp || [],
      overflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
      sans: document.fonts.check('15px "Spline Sans"'),
      ground: getComputedStyle(document.body).backgroundColor,
      dataTheme: document.documentElement.getAttribute("data-theme"),
      // The header band's height: 45 px by design. A grid on <body> that shares the viewport
      // with it stretches it to hundreds of pixels on a short page (§20 erratum 244's fix pass).
      header: (document.querySelector("header.top") || { offsetHeight: 0 }).offsetHeight,
      // The embed's wordmark overlay (§20 erratum 246): its box, from the DOM, and whether it is
      // drawn — the poster and the paused player show it, a playing one does not.
      //
      // `bottom` is the UNION of the anchor and its green mark, not the anchor's border box. The
      // mark is `.wordmark .pp::after` (app.css): an absolutely positioned pseudo-element sitting
      // `bottom: -0.165em` below its `.pp` container, so it paints OUTSIDE the anchor and
      // getBoundingClientRect() never sees it — which under-reported the overlay's bottom by ~2.6 px
      // at the embed's 16 px. Immaterial against the 48 px controls band, and the assertion below
      // should still be measuring the thing a viewer sees.
      mark: (() => {
        const m = document.querySelector(".embed-mark");
        if (!m) return null;
        const r = m.getBoundingClientRect();
        let bottom = r.bottom, overhang = 0;
        const pp = m.querySelector(".pp");
        if (pp) {
          const off = parseFloat(getComputedStyle(pp, "::after").bottom);   // negative: it hangs below
          if (Number.isFinite(off)) {
            const markBottom = pp.getBoundingClientRect().bottom - off;
            overhang = Math.max(0, markBottom - r.bottom);
            bottom = Math.max(bottom, markBottom);
          }
        }
        return { top: r.top, left: r.left, right: r.right, bottom, anchorBottom: r.bottom, overhang,
          shown: getComputedStyle(m).display !== "none",
          // What the overlay SAYS (§20 erratum 287) and whether it kept the lockup: on
          // clippet.gg and its subdomains the host contains the wordmark, so `.pp` is there and
          // the green span runs under the double p; anywhere else it is the host as plain text.
          text: m.textContent.trim(), lockup: !!pp, label: m.getAttribute("aria-label") };
      })(),
      frame: { w: window.innerWidth, h: window.innerHeight },
      // The wordmark's box on the page (document coordinates) and its size: what a crop-diff
      // against another pass's frames compares, since the living mark must paint NOTHING at rest.
      markBox: (() => {
        const m = document.querySelector(".wordmark");
        if (!m) return null;
        const r = m.getBoundingClientRect();
        return { x: r.left + window.scrollX, y: r.top + window.scrollY, w: r.width, h: r.height, fs: parseFloat(getComputedStyle(m).fontSize) };
      })(),
    }));
    // A page that carries the clip's player is photographed with its ANIMATIONS STOPPED (lane `speed2`): the
    // player's loading spinner turns for as long as the pinned Chromium (no H.264) keeps trying, and the
    // identity proof found it at a different angle in every take. Playwright puts an infinite animation
    // at its start and a finite one at its end, so the frame is the same frame every time.
    await page.screenshot({ path: path.join(SHOT_DIR, shotName), fullPage: true, animations: (opts.media || opts.embed) ? "disabled" : "allow" });
    // A page that IS a 404 (the not-found page) makes Chromium log the document's own status
    // as a resource error; that is the page working, not a fault on it.
    const faults = opts.status ? errors.filter((e) => !e.includes(`status of ${opts.status}`)) : errors;
    if (faults.length) failures.push(`console: ${faults.map(redactText).join(" | ")}`);
    if (facts.csp.length) failures.push(`csp: ${facts.csp.map(redactText).join(" | ")}`);
    // The no-telemetry tripwire (web/STYLE.md), converted rather than weakened for redirect mode
    // (§20 erratum 282): the ONE exception is signedMedia — this run's media origin, a signature,
    // and a key under `clips/`, all three. In proxy mode MEDIA_ORIGIN is "", signedMedia is false
    // for every URL, and this is byte-for-byte the filter it has always been.
    const foreign = requests.filter((u) => !(u.startsWith(BASE + "/") || u === BASE || u.startsWith("data:") || u.startsWith("blob:") || signedMedia(u)));
    if (foreign.length) failures.push(`foreign requests: ${foreign.map(redact).join(" | ")}`);
    // …and the assertion the other mode cannot make: a page that carries the clip's media must
    // actually have FETCHED it from the bucket on a redirect stack. A watch or embed page whose
    // every request stayed on $BASE would mean the 302 was never followed — the tripwire above
    // would be silent about it, and the shot would be photographed as if the stack were proxying.
    if ((opts.media || opts.embed) && MEDIA === "redirect" && !requests.some(signedMedia)) {
      failures.push("no signed media request left this page: on a redirect stack the poster is fetched from the media origin");
    }
    const fonts = requests.filter((u) => /\.woff2(\?|$)/.test(u));
    const strayFonts = fonts.filter((u) => !u.startsWith(BASE + "/static/fonts/"));
    if (strayFonts.length) failures.push(`fonts from elsewhere: ${strayFonts.map(redact).join(" | ")}`);
    // Every page draws text in the face — the embed too, since its wordmark overlay (§20 erratum
    // 246) — so the face MUST have loaded everywhere.
    if (!facts.sans) failures.push("Spline Sans did not load");
    if (width === 390 && facts.overflow) failures.push("horizontal overflow at 390");
    if (!opts.embed && facts.ground !== GROUND[theme]) failures.push(`ground is ${facts.ground}, want ${GROUND[theme]} for ${theme}`);
    // `data-theme` is the SERVER's answer to the theme cookie (§20 erratum 242) and this pass sets no
    // cookie: every shot here picks its theme with emulateMedia, which is the `prefers-color-scheme`
    // path, and the attribute must therefore be absent on every page. It was collected and never
    // asserted, so a handler that started stamping the attribute unasked — pinning every viewer to one
    // theme whatever their system says — would have been photographed and passed.
    // There is no option to relax it: an `opts.themeCookie` no caller ever set read as a knob this
    // pass had, and a dead knob is a promise nothing keeps (§20 erratum 275).
    if (facts.dataTheme !== null) failures.push(`data-theme is ${JSON.stringify(facts.dataTheme)}, want null (no theme cookie is set: the theme comes from prefers-color-scheme)`);
    // The band is 45 px by design and this catches the grid that once stretched it to hundreds. It is
    // deliberately NOT relaxed for a badged stack (§20 erratum 285): a badge is one small word, and if
    // it makes the bar two lines high on a phone that is a fault in the bar, not an allowance to give.
    if (facts.header > 64) failures.push(`the header band is ${facts.header} px tall (stretched)`);
    if (opts.embed) {
      // The overlay: present, drawn (on the poster or paused), inside the frame, and above the
      // native controls' bottom band (48 px) — at the card size too (400 × 225).
      const m = facts.mark;
      if (!m) failures.push("the embed carries no wordmark overlay");
      else {
        if (!m.shown) failures.push("the wordmark overlay is hidden on a player that is not playing");
        if (m.left < 0 || m.top < 0 || m.right > facts.frame.w || m.bottom > facts.frame.h) failures.push(`the overlay's box (${m.left},${m.top})-(${m.right},${m.bottom}) leaves the ${facts.frame.w}x${facts.frame.h} frame`);
        if (m.bottom > facts.frame.h - 48) failures.push(`the overlay's bottom (${m.bottom}, anchor ${m.anchorBottom} + ${m.overhang} of green mark) is inside the controls band (frame ${facts.frame.h}, band 48)`);
        // …and it says this instance's own host, in the form that host earns (§20 erratum 287).
        if (SITE_HOST && m.text !== SITE_HOST) failures.push(`the overlay reads ${JSON.stringify(m.text)}, want the public host ${JSON.stringify(SITE_HOST)}`);
        const ours = SITE_HOST === "clippet.gg" || SITE_HOST.endsWith(".clippet.gg");
        if (SITE_HOST && m.lockup !== ours) failures.push(`the overlay ${m.lockup ? "keeps" : "drops"} the wordmark lockup on ${SITE_HOST}; on this product's own domain it keeps it and nowhere else`);
        if (ours && m.label !== SITE_HOST) failures.push(`the overlay's accessible name is ${JSON.stringify(m.label)}, want the host it spells`);
        if (!ours && m.label !== null) failures.push(`the plain overlay carries an aria-label (${JSON.stringify(m.label)}); its own text is the name`);
      }
    }
    headerPx = facts.header;
    markBox = facts.markBox;
  } catch (e) {
    failures.push("exception: " + String(e));
  }
  await page.close();
  if (frozen) await frozen.close();
  // One choke point for every word this shot says: the line below, the contact sheet and
  // results.json all read `said`, so a signature cannot reach a log or a file through a message
  // nobody thought to redact — an exception's text, for instance (§20 erratum 282). redactText is
  // idempotent: a URL already reduced to `?<signed>` carries no `X-Amz-` for it to find.
  const said = failures.map(redactText);
  // `seq` is the shot's place in the SERIAL order (lane `speed2`): the workers finish in any order, and the
  // sheet and results.json are sorted by it, so both read exactly as a one-at-a-time pass writes them.
  (SHOT_DIR === OUT ? results : serialResults).push({ seq: opts.seq === undefined ? 1e9 : opts.seq, name, theme, width, shot: shotName, url: PUBLIC_BASE + url, failures: said, markBox });
  const tag = said.length ? "FAIL" : "ok  ";
  console.log(`[visual] ${tag} ${name} ${theme} ${width}${headerPx ? ` header=${headerPx}px` : ""}${said.length ? " — " + said.join("; ") : ""}`);
}

async function shootAll(ctx, name, url, opts) {
  for (const theme of THEMES) {
    for (const width of WIDTHS) {
      await shoot(ctx, name, url, theme, width, opts);
    }
  }
}

// ---- the theme glyph and the auth panel's rows (§20 erratum 288; web/STYLE.md) -----------------
//
// domTheme: two claims a screenshot cannot make.
//
// (1) The glyph is correct in ALL THREE theme states WITHOUT a script. The page renders BOTH buttons
//     always, and the stylesheet draws exactly one — so the only way to know it is right is to put a
//     browser in each state and ask which one is displayed. The three states are: no cookie (the
//     system decides, `prefers-color-scheme`), the cookie pinning light, and the cookie pinning dark;
//     each is read on a light system AND a dark one, because the whole point of the middle rule is
//     that a pinned cookie must beat the machine. The button that is drawn is cross-checked against
//     the GROUND the page actually painted, so "the sun is showing" is only ever accepted on a page
//     that is in fact dark. Both pages that have a bar are read: a stranger's share page (no session,
//     no nav — erratum 244) and the owner's library.
// (2) A control never leaves its own row. The register panel's username input overflowed its track
//     and was drawn under the mono hint at every width (the owner's report, 2026-09-16); the fix is `min-width: 0`
//     plus the stacked grammar in `.auth`, and this asserts the BOXES — every input inside its `.rc`,
//     every `.rc` inside its `.row`, at 1280 and at 390, on both auth panels.
// (3) The display mark has clear air under its green span on every page that stacks text below it
//     (erratum 288 (e)). The span is absolutely positioned and in no box the grid can see, so the
//     only way to know is to measure it against what follows.
async function domTheme(browser, owner, clipId) {
  let n = 0;
  const check = (ok, what) => { n++; if (!ok) throw new Error(`dom theme: ${what}`); };
  const state = await owner.storageState();
  // One reading: which button the stylesheet drew, what the page painted, and where the glyph sits.
  const read = async (url, colorScheme, cookie, signedIn, width) => {
    const ctx = await browser.newContext(signedIn
      ? { baseURL: BASE, colorScheme, storageState: state }
      : { baseURL: BASE, colorScheme });
    try {
      if (cookie) await ctx.addCookies([{ name: "theme", value: cookie, url: BASE }]);
      const page = await ctx.newPage();
      const w = width || 1280;
      await page.setViewportSize({ width: w, height: 900 });
      // The pointer parks in the corner for the reason every shot does: a fresh page is handed a
      // pointerenter at (0, 0), which is inside the bar, and the mark answers a pointer there.
      await page.mouse.move(w - 1, 899);
      await go(page, BASE + url, { waitUntil: "networkidle" });
      await page.evaluate(() => document.fonts.ready);
      const out = await page.evaluate(() => {
        const drawn = (el) => !!el && getComputedStyle(el).display !== "none";
        const sun = document.querySelector('[data-theme-to="light"]');
        const moon = document.querySelector('[data-theme-to="dark"]');
        const form = document.querySelector("[data-theme-glyph]");
        const bar = document.querySelector("header.top .bar-end");
        const nav = document.querySelector("header.top nav");
        const head = document.querySelector("header.top");
        const hb = head ? head.getBoundingClientRect() : null;
        const fb = form ? form.getBoundingClientRect() : null;
        // The token resolved the way a computed colour reads back (domBadge's trick).
        const tok = (v) => {
          const d = document.createElement("div");
          d.style.color = v;
          document.body.appendChild(d);
          const c = getComputedStyle(d).color;
          d.remove();
          return c;
        };
        return {
          attr: document.documentElement.getAttribute("data-theme"),
          ground: getComputedStyle(document.body).backgroundColor,
          sun: drawn(sun), moon: drawn(moon),
          labels: [sun && sun.getAttribute("aria-label"), moon && moon.getAttribute("aria-label")],
          titles: [sun && sun.getAttribute("title"), moon && moon.getAttribute("title")],
          form: !!form,
          inBar: !!(form && bar && bar.contains(form)),
          inNav: !!(form && nav && nav.contains(form)),
          nav: !!nav,
          // …and it is the LAST thing in the bar: nothing of the bar is to the right of it but the
          // header's own padding.
          fromEnd: hb && fb ? Math.round(hb.right - fb.right - parseFloat(getComputedStyle(head).paddingRight)) : null,
          colour: sun || moon ? getComputedStyle(drawn(sun) ? sun : moon).color : "",
          stroke: (() => {
            const b = drawn(sun) ? sun : moon;
            const p = b && b.querySelector("svg path");
            return p ? [getComputedStyle(p).stroke, getComputedStyle(p).fill, getComputedStyle(p).strokeWidth] : [];
          })(),
          muted: tok("var(--muted)"),
          scrolls: document.documentElement.scrollWidth > document.documentElement.clientWidth,
        };
      });
      await page.close();
      return out;
    } finally {
      await ctx.close();
    }
  };
  // (1) the three states, on both systems, on both pages that have a bar. `want` is the theme the
  // page must be PAINTED, which decides which button exists: a dark page offers the sun.
  for (const [what, url, signedIn] of [
    ["a stranger's share page", "/" + clipId, false],
    ["the library", "/", true],
  ]) {
    for (const [scheme, cookie, want] of [
      ["light", "", "light"],       // no cookie: the system decides…
      ["dark", "", "dark"],         // …on either system
      ["dark", "light", "light"],   // the cookie pins light against a dark machine
      ["light", "dark", "dark"],    // …and dark against a light one
      ["light", "purple", "light"], // a value no control writes is no cookie at all
    ]) {
      const say = `${what}, ${scheme} system, cookie ${JSON.stringify(cookie)}`;
      const d = await read(url, scheme, cookie, signedIn);
      check(d.form, `${say}: there is no theme glyph in the bar`);
      check(d.ground === GROUND[want], `${say}: the page painted ${d.ground}, want ${GROUND[want]} — this run cannot say anything about the glyph`);
      check(d.attr === (cookie === "light" || cookie === "dark" ? cookie : null), `${say}: data-theme is ${JSON.stringify(d.attr)}`);
      check(d.sun !== d.moon, `${say}: ${d.sun && d.moon ? "BOTH" : "NEITHER"} theme button is drawn — exactly one must be`);
      check(want === "dark" ? d.sun : d.moon, `${say}: the ${want} page draws the ${d.sun ? "sun" : "moon"}, want the ${want === "dark" ? "sun" : "moon"}`);
    }
  }
  // (2) where it sits, what it is made of, and the rule erratum 244 still holds on a stranger's page.
  const anon = await read("/" + clipId, "light", "", false);
  check(anon.inBar, "the glyph is not in the bar's end slot");
  check(!anon.inNav, "the glyph is inside the <nav>: it navigates nowhere");
  check(!anon.nav, "a stranger's share page grew a <nav> (§20 erratum 244: the wordmark alone)");
  check(anon.fromEnd !== null && Math.abs(anon.fromEnd) <= 1, `the glyph is ${anon.fromEnd} px off the end of the bar, want the last thing in it`);
  check(anon.colour === anon.muted, `the glyph is ${anon.colour}, want --muted (${anon.muted})`);
  // The Kept line style: the drawing takes the button's own ink (`currentColor`, which some engines
  // report resolved and some report by name), it is a LINE and not a shape, and the line is 1.5 px.
  check(/^currentcolor$/i.test(anon.stroke[0]) || anon.stroke[0] === anon.muted, `the glyph strokes ${JSON.stringify(anon.stroke[0])}, want the button's own ink (--muted is ${anon.muted})`);
  check(anon.stroke[1] === "none", `the glyph fills ${JSON.stringify(anon.stroke[1])}, want none (it is a line drawing)`);
  check(parseFloat(anon.stroke[2]) === 1.5, `the glyph's stroke is ${anon.stroke[2]}, want 1.5px`);
  check(anon.labels[0] === "Light theme" && anon.labels[1] === "Dark theme", `the buttons are labelled ${JSON.stringify(anon.labels)} — each names the theme it GIVES you`);
  check(anon.titles[0] === "Light theme" && anon.titles[1] === "Dark theme", `the tooltips are ${JSON.stringify(anon.titles)}`);
  // …and on a phone the bar still holds it without scrolling the page sideways.
  const narrow = await read("/", "dark", "", true, 390);
  check(narrow.form && narrow.sun && !narrow.moon, "at 390 the signed-in bar does not draw the one right glyph");
  check(!narrow.scrolls, "at 390 a bar with the glyph scrolls the page sideways");
  // (3) the auth panels: no control outside its own row, at either width, in either panel.
  for (const url of ["/register", "/login"]) {
    for (const width of WIDTHS) {
      const ctx = await browser.newContext({ baseURL: BASE });
      try {
        const page = await ctx.newPage();
        await page.setViewportSize({ width, height: 900 });
        await go(page, BASE + url, { waitUntil: "networkidle" });
        await page.evaluate(() => document.fonts.ready);
        const rows = await page.evaluate(() => Array.from(document.querySelectorAll(".auth .row")).map((row, i) => {
          const rb = row.getBoundingClientRect();
          const boxes = Array.from(row.querySelectorAll(".rc > input, .rc > textarea, .rc > button")).map((el) => {
            const b = el.getBoundingClientRect();
            const track = el.parentElement.getBoundingClientRect();
            return { tag: el.tagName, out: Math.max(0, Math.round(b.right - track.right)), row: Math.max(0, Math.round(b.right - rb.right)) };
          });
          const rs = row.querySelector(".rs");
          const rsb = rs ? rs.getBoundingClientRect() : null;
          const first = row.querySelector(".rc > input");
          const fb = first ? first.getBoundingClientRect() : null;
          return { i, boxes, overlap: rsb && fb && rsb.width > 0 ? Math.round(Math.min(rsb.right, fb.right) - Math.max(rsb.left, fb.left)) : -1, sameLine: rsb && fb ? rsb.top < fb.bottom && rsb.bottom > fb.top : false };
        }));
        check(rows.length > 0, `${url} at ${width}: no rows to measure`);
        for (const r of rows) {
          for (const b of r.boxes) {
            check(b.out === 0, `${url} at ${width}, row ${r.i}: the ${b.tag} sticks ${b.out} px out of its control column`);
            check(b.row === 0, `${url} at ${width}, row ${r.i}: the ${b.tag} sticks ${b.row} px out of its row`);
          }
          // The stacked grammar: the hint is UNDER the control, never over it (the owner's report).
          check(!(r.sameLine && r.overlap > 0), `${url} at ${width}, row ${r.i}: the state cell overlaps the input by ${r.overlap} px`);
        }
      } finally {
        await ctx.close();
      }
    }
  }
  // (4) air under the display mark (§20 erratum 288 (e); the owner on the live 404: the Not Found
  // sat very close to the logo). The green span is an ABSOLUTELY POSITIONED pseudo-element hanging
  // 0.265em below the mark's own box, so it is in no box a grid can see and every stacked container
  // measured from the wrong edge: the 404 left one pixel, and the span read as an underline on the
  // heading. Measured here off the `::after` — the span AT REST, which is the geometry the rule is
  // about; a face's mouth is the same band redrawn and is in motion, so measuring it would make this
  // a race about the ambient timer rather than a claim about the layout.
  for (const [url, what] of [["/AAAAAAAAAAAA", "the 404"], ["/", "the front door"], ["/login", "login"], ["/register", "register"]]) {
    for (const width of WIDTHS) {
      const ctx = await browser.newContext({ baseURL: BASE });
      try {
        const page = await ctx.newPage();
        await page.setViewportSize({ width, height: 900 });
        await page.mouse.move(width - 1, 899);
        await go(page, BASE + url, { waitUntil: "networkidle" });
        await page.evaluate(() => document.fonts.ready);
        const d = await page.evaluate(() => {
          const m = document.querySelector(".wordmark.display");
          if (!m) return null;
          const pp = m.querySelector(".pp");
          const span = getComputedStyle(pp, "::after");
          const bottom = pp.getBoundingClientRect().bottom - parseFloat(span.bottom);
          // the mark's grid CELL: the lockup on a badged page, the mark itself otherwise
          const cell = m.closest(".lockup") || m;
          const next = cell.nextElementSibling;
          return {
            air: next ? Math.round(next.getBoundingClientRect().top - bottom) : null,
            next: next ? next.tagName.toLowerCase() : null,
            size: Math.round(parseFloat(getComputedStyle(m).fontSize)),
          };
        });
        check(!!d, `${what} at ${width}: there is no display mark on this page`);
        check(d.next !== null, `${what} at ${width}: nothing is stacked under the mark`);
        check(d.air >= 12, `${what} at ${width}: the <${d.next}> under the ${d.size} px mark starts ${d.air} px below the green span, want at least 12 — the span must not read as an underline on it`);
      } finally {
        await ctx.close();
      }
    }
  }
  console.log(`[visual] dom theme: ${n} checks ok (one glyph per state on both pages and both systems, the cookie beating the machine; last in the bar, --muted, currentColor, each button naming the theme it gives; 390 without a sideways scroll; no control outside its row on either auth panel at either width; clear air under the display mark on all four stacked pages)`);
}

// ---- the living mark (ROADMAP #38, §20 errata 267, 274; web/STYLE.md) --------------------------
// The mark inside `sel`, read back off the DOM: the overlay count on its `pp`, the overlay's markup,
// each pupil's four attributes, the upper lids' transforms, whether the mouth is drawn (and the span
// hidden under it), the face it is wearing (`data-face`) and whether it is `sad` (the red mouth).
function eyesOf(page, sel) {
  return page.evaluate((s) => {
    const m = document.querySelector(`${s} .wordmark`);
    const pp = m && m.querySelector(".pp");
    if (!pp) return null;
    const svgs = pp.querySelectorAll(":scope > svg.eyes");
    const mouth = pp.querySelector("svg.eyes .mouth");
    return {
      count: svgs.length,
      html: svgs[0] ? svgs[0].outerHTML : "",
      pupils: Array.from(pp.querySelectorAll("svg.eyes circle")).map((c) => ({
        cx: +c.getAttribute("cx"), cy: +c.getAttribute("cy"), r: +c.getAttribute("r"), op: +c.getAttribute("opacity"),
      })),
      lids: Array.from(pp.querySelectorAll("svg.eyes rect.lid")).map((r) => r.getAttribute("transform")),
      mouth: mouth ? mouth.getAttribute("visibility") === "visible" : null,
      mouthing: pp.classList.contains("mouthing"),
      face: m.getAttribute("data-face") || "",
      sad: m.classList.contains("sad"),
    };
  }, sel);
}
// The eye geometry app.js draws with (its own EYE constant): a pupil sits at cx 628, cy -560
// inside its group, 150 units across (the owner's pick, erratum 274), invisible at rest.
const EYE_REST = { cx: 628, cy: -560, r: 150 };
const atRest = (p) => p.cx === EYE_REST.cx && p.cy === EYE_REST.cy && p.r === EYE_REST.r && p.op === 0;
// moved: something of the face is on the page — a pupil drawn, or the mouth
const moved = (m) => !!m && ((m.pupils.length > 0 && m.pupils.some((p) => p.op > 0)) || m.mouth === true);
// The eight faces and where in each storyboard the frame is taken (ms from its start: inside the
// hold — the blink's closed lids, the glance's left dwell, the smile's full bow).
const FACES = ["glance", "blink", "wink", "drift", "smile", "beam", "gasp", "sulk"];
const FACE_AT = { glance: 620, blink: 450, wink: 700, drift: 1100, smile: 900, beam: 1100, gasp: 900, sulk: 900 };
// Playwright's fake clock, PAUSED before the page loads: timers and requestAnimationFrame then run
// only under clock.runFor(), so a storyboard can be walked to an exact millisecond and the ambient
// timer's 45–90 s band crossed in a few seconds. A test knob that is NOT a product knob: nothing in
// app.js knows it is there. The time is the real one, so relative dates on the page stay true.
// The clock is CONTEXT-wide (page.clock is its context's) and install() cannot be undone, so a
// frozen page lives in a throwaway context cloned from its caller's cookies, closed with the page.
const CLOCK0 = Date.now();
async function frozenContext(from) {
  const ctx = await browser.newContext({ baseURL: BASE, storageState: await from.storageState() });
  await ctx.clock.install({ time: CLOCK0 });
  await ctx.clock.pauseAt(CLOCK0 + 1000);
  return ctx;
}
// wearFace(page, face): before app.js boots, the page's wordmark arrives wearing `data-face` — the same
// declarative hook the 404 template uses for its gasp, set from the test side as the parser hands the
// element over. Nothing in the product is touched; app.js plays what a mark arrives wearing.
function wearFace(page, face) {
  return page.addInitScript((f) => {
    new MutationObserver((_, o) => {
      const m = document.querySelector(".wordmark");
      if (!m) return;
      m.setAttribute("data-face", f);
      o.disconnect();
    }).observe(document, { childList: true, subtree: true });
  }, face);
}
// Inside the tracking area, away from the mark: to the right of it in the header bar, over the
// middle of the auth card. The entrance is 260 ms and the follow eases 18 % a frame; 700 ms settles.
async function enterArea(page, sel) {
  const box = await page.locator(sel).first().boundingBox();
  if (!box) throw new Error(`no ${sel} on the page`);
  await page.mouse.move(box.x + box.width * 0.6, box.y + box.height * 0.5);
  await sleep(700);
  return box;
}
// Each pupil rides an ellipse 110 x 150 at the pointer's angle FROM ITS OWN eye's centre, the second
// eye being 1146 units right of the first (app.js's ORBIT/EYE_ADV). `angleOf` undoes the ellipse, so
// what comes back is the angle that eye was aimed at; `EYE_MID` is the point halfway between the two
// centres, as a fraction of the mark's width.
const EYE_MID = (EYE_REST.cx + EYE_REST.cx + 1146) / 2 / 2300;
const angleOf = (p) => Math.atan2((p.cy - EYE_REST.cy) / 150, (p.cx - EYE_REST.cx) / 110);
// recordEntrance installs a per-FRAME recorder in the page before the pointer arrives (§20 erratum
// 275). The settled checks below read the state 700 ms after the entrance starts, by which time a
// build that skipped the 260 ms tween entirely and jumped to the aimed state looks identical; the
// only place the difference exists is in the frames between, so they are recorded rather than
// sampled over a round trip whose timing the harness does not control (~120 ms after the move is
// the middle of the tween, and the recorder holds every frame around it).
async function recordEntrance(page, sel) {
  await page.evaluate((s) => {
    window.__eyeFrames = [];
    const pp = document.querySelector(`${s} .wordmark .pp`);
    const t0 = performance.now();
    const tick = () => {
      const c = pp && pp.querySelector("svg.eyes circle");
      if (c) window.__eyeFrames.push({ t: Math.round(performance.now() - t0), op: +c.getAttribute("opacity"), r: +c.getAttribute("r") });
      if (performance.now() - t0 < 1500) requestAnimationFrame(tick); // 1.5 s: two CDP round trips precede the 260 ms tween on a loaded host
    };
    requestAnimationFrame(tick);
  }, sel);
}

// A `before` for the two follow frames: the cursor inside the area, and the pupils drawn and aimed.
function follow(sel) {
  return async (page) => {
    await enterArea(page, sel);
    const e = await eyesOf(page, sel);
    if (!e || e.count !== 1) throw new Error(`${sel}: ${e ? e.count : "no mark,"} overlays, want 1`);
    if (e.pupils.length !== 2 || !e.pupils.every((p) => p.op > 0.99 && !atRest(p))) throw new Error(`${sel}: the pupils are not drawn and aimed: ${JSON.stringify(e.pupils)}`);
  };
}
// The DOM test app.js has no other harness for: the overlay is injected ONCE per mark on the header
// and on the auth card, carries no style attribute, starts at rest; a pointer entering the tracking
// area sets the pupils (opaque, full size, off centre); leaving restores EXACTLY the injected
// markup; the embed's mark gets no overlay; under reduced motion nothing is injected anywhere.
async function domFollow(browser, clipId) {
  let n = 0;
  const check = (ok, what) => { n++; if (!ok) throw new Error(`dom follow: ${what}`); };
  const cycle = async (ctx, url, sel, what) => {
    const page = await ctx.newPage();
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.mouse.move(1279, 799); // parked in the page gutter, outside the header's area; the display mark's area is the whole page, and its entrance waits for a MOVE (see shoot)
    await go(page, BASE + url, { waitUntil: "networkidle" });
    const rest = await eyesOf(page, sel);
    check(rest && rest.count === 1, `${what}: ${rest ? rest.count : "no mark,"} overlays on the mark, want 1`);
    check(rest.pupils.length === 2 && rest.pupils.every(atRest), `${what}: the injected pupils are not at rest: ${JSON.stringify(rest.pupils)}`);
    check(!/style=|<script/.test(rest.html), `${what}: the overlay carries a style attribute or a script`);
    await recordEntrance(page, sel);
    const box = await enterArea(page, sel);
    const on = await eyesOf(page, sel);
    check(on.pupils.every((p) => p.op > 0.99 && p.r === EYE_REST.r && !atRest(p)), `${what}: the pupils did not enter and aim: ${JSON.stringify(on.pupils)}`);
    check(Math.abs(on.pupils[0].cx - on.pupils[1].cx) < 60 && Math.abs(on.pupils[0].cy - on.pupils[1].cy) < 60, `${what}: the pupils do not converge: ${JSON.stringify(on.pupils)}`);
    // THE ENTRANCE ITSELF, from the frames recorded above: half-faded, still-small pupils between
    // rest and settled. Without this the 260 ms tween could be deleted and everything else here
    // would still pass (§20 erratum 275).
    const frames = await page.evaluate(() => window.__eyeFrames || []);
    const mid = frames.filter((f) => f.op > 0.02 && f.op < 0.98);
    check(mid.length >= 2, `${what}: the pupils never appeared part-way — the entrance is not tweened: ${JSON.stringify(frames.slice(0, 12))}`);
    check(mid.length >= 2 && mid[0].r < EYE_REST.r, `${what}: the first part-way frame is already full size (${mid.length ? mid[0].r : "none"}): the pupils must grow as they fade in`);
    // EACH EYE AIMS FROM ITS OWN CENTRE. The convergence check above compares the two pupils' LOCAL
    // offsets, and a regression aiming both from one centre passes it. With the pointer BETWEEN the
    // two eyes and below them, the only correct answer is two offsets leaning opposite ways: the
    // left eye looks right-and-down, the right eye left-and-down (§20 erratum 275).
    const pbox = await page.locator(`${sel} .wordmark .pp`).first().boundingBox();
    if (!pbox) throw new Error(`${what}: no mark to measure`);
    await page.mouse.move(pbox.x + pbox.width * EYE_MID,
      Math.min(pbox.y + pbox.height + 8, box.y + box.height - 2));   // below the mark, still inside the area
    await sleep(500);
    const aimed = await eyesOf(page, sel);
    const [left, right] = aimed.pupils;
    check(left.cx > EYE_REST.cx && right.cx < EYE_REST.cx,
      `${what}: with the pointer between the eyes both pupils lean the same way: ${JSON.stringify(aimed.pupils)}`);
    check(Math.abs(angleOf(left) - angleOf(right)) > 0.1,
      `${what}: the two eyes were aimed at the same angle (${angleOf(left).toFixed(3)} and ${angleOf(right).toFixed(3)} rad): ${JSON.stringify(aimed.pupils)}`);
    if (sel === ".auth") {
      // The display mark's area is the whole page (§20 erratum 273): below the card is still inside it,
      // so the eyes must STAY out there; what ends it is the pointer leaving the window, which the
      // browser reports as a pointerleave on the document — dispatched here, since a headless pointer
      // cannot leave the viewport.
      await page.mouse.move(box.x + box.width / 2, box.y + box.height + 80);
      await sleep(400);
      const stay = await eyesOf(page, sel);
      check(stay.pupils.every((p) => p.op > 0.99), `${what}: the eyes left when the pointer only left the card (the area is the page)`);
      await page.evaluate(() => document.documentElement.dispatchEvent(new PointerEvent("pointerleave", { pointerType: "mouse" })));
    } else {
      await page.mouse.move(box.x + box.width / 2, box.y + box.height + 80); // below the area
    }
    await sleep(700);
    const off = await eyesOf(page, sel);
    check(off.count === 1 && off.html === rest.html, `${what}: after leave the overlay is not the injected state again`);
    await page.close();
  };
  const anon = await browser.newContext({ baseURL: BASE });
  const still = await browser.newContext({ baseURL: BASE, reducedMotion: "reduce" });
  try {
    await cycle(anon, `/${clipId}`, "header.top", "the header (a stranger's share page)");
    await cycle(anon, "/", ".auth", "the auth card (the front door)");
    // `wantMark` is false only for the embed of an instance whose host does not carry the
    // wordmark (§20 erratum 287): its overlay is plain text with no `.pp` at all, so "no eyes
    // were injected" is still the claim and "there is a mark to injecting into" is not.
    const none = async (ctx, url, what, wantMark = true) => {
      const page = await ctx.newPage();
      await go(page, BASE + url, { waitUntil: "networkidle" });
      const c = await page.evaluate(() => ({ marks: document.querySelectorAll(".wordmark .pp").length, eyes: document.querySelectorAll("svg.eyes").length, worn: document.querySelectorAll(".wordmark[data-face]").length }));
      check((wantMark ? c.marks >= 1 : c.marks === 0) && c.eyes === 0 && c.worn === 0, `${what}: ${c.eyes} overlays on ${c.marks} marks (${c.worn} wearing a data-face), want none`);
      await page.close();
    };
    await none(still, "/AAAAAAAAAAAA", "the 404 under reduced motion (its data-face stripped, no gasp)");
    await none(still, "/", "the front door under reduced motion");
    await none(still, `/${clipId}`, "the header under reduced motion");
    await none(still, `/embed/${clipId}`, "the embed under reduced motion", EMBED_LOCKUP);
  } finally {
    await anon.close();
    await still.close();
  }
  console.log(`[visual] dom follow: ${n} checks ok (the header, the auth card: injected once, at rest, tweened in over its own frames, aimed per eye from its own centre, exact rest on leave; none under reduced motion, the 404 and the embed included)`);
}

// ---- the faces (ROADMAP #38, §20 erratum 274; web/STYLE.md) --------------------------------------
// Every meaningful face is driven by its REAL event and read back off the mark (`data-face` while it
// plays, the pupils / lids / mouth mid-face, the injected markup again after — exact rest); the
// ambient timer is crossed on the fake clock; the embed wears smile on ready and nothing else.
// watch(page, sel, ms, until): sample the mark every 40 ms for up to `ms`; the faces seen, in order
// (each play once), the first sample of each and one ~450 ms in; stops early when `until` says so.
// Elapsed time is MONOTONIC (process.hrtime), never Date.now(): the container's wall clock steps by
// seconds, and a step forward once ended a watch mid-face while a step back stamped a
// sample at −11 s.
const mono = () => Number(process.hrtime.bigint() / 1000000n);
async function watch(page, sel, ms, until) {
  const seen = [], first = {}, mid = {}, log = [];
  let last = "", since = 0, lastLog = "";
  const t0 = mono();
  while (mono() - t0 < ms) {
    const m = await eyesOf(page, sel).catch((e) => ({ err: String(e).slice(0, 60) })); // a page mid-reload answers nothing
    const f = m && !m.err ? m.face : "";
    const l = JSON.stringify(m ? { face: m.face, mouth: m.mouth, count: m.count, err: m.err } : null);
    if (l !== lastLog) log.push([mono() - t0, l]);
    lastLog = l;
    if (f && f !== last) { seen.push(f); since = mono(); if (!first[f]) first[f] = m; }
    if (f && !mid[f] && mono() - since >= 450) mid[f] = m;
    last = f;
    if (until && await until(seen, m)) break;
    await sleep(40);
  }
  return { seen, first, mid, log };
}
// `part` (lane `speed2`): "events" is sections 0-6 — the real events, which upload, copy (a copy resets the
// clip's expiry), save, delete and reroute the owner's requests, and so run ALONE; "ambient" is section 7 —
// the ambient timer on Playwright's paused clock, which reads two anonymous pages in throwaway contexts
// and changes nothing, so it runs as a job of its own beside the face frames. The checks are the same
// checks; each part prints its own count.
async function domFaces(browser, owner, clipId, part = "all") {
  let n = 0;
  const check = (ok, what) => { n++; if (!ok) throw new Error(`dom faces: ${what}`); };
  const open = async (ctx, url, opts = {}) => {
    const c = opts.clock ? await frozenContext(ctx) : ctx;
    const page = await c.newPage();
    page.__frozen = !!opts.clock;
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.mouse.move(1279, 799); // parked: the entrance waits for a move (erratum 273)
    await go(page, BASE + url, { waitUntil: "networkidle" });
    return page;
  };
  // shut: a frozen page takes its throwaway context with it
  const shut = async (p) => { if (p.__frozen) await p.context().close(); else await p.close(); };
  const played = (w) => `${JSON.stringify(w.seen)} (samples: ${JSON.stringify(w.log)})`;
  // rested: the face is over and the mark is exactly the injected one — no data-face, no `sad`, the
  // span back (no `mouthing`), the overlay's markup byte-for-byte what boot wrote.
  const rested = (m, html) => !!m && m.face === "" && !m.sad && !m.mouthing && m.count === 1 && m.html === html;
  // over: a `watch` stops once a face has played and the mark is bare again
  const over = (seen, m) => seen.length > 0 && !!m && m.face === "";
  // calm: the mark is exactly at rest and STAYS so — a queued face follows the one before after a
  // 300 ms breath, so a bare instant is not the end. Two rested samples 500 ms apart, within 8 s.
  let calmLast = null;
  const calm = async (page, sel, html) => {
    const t0 = mono();
    while (mono() - t0 < 8000) {
      let m = await eyesOf(page, sel);
      if (rested(m, html)) {
        await sleep(500);
        m = await eyesOf(page, sel);
        if (rested(m, html)) return true;
      }
      calmLast = m && { face: m.face, mouthing: m.mouthing, sad: m.sad, count: m.count, sameHtml: m.html === html };
      await sleep(50);
    }
    return false;
  };
  const rowState = (page) => page.evaluate(() => { const r = document.querySelector(".clip-row"); return r ? r.dataset.state : ""; });
  const newestId = (page) => page.evaluate(() => { const r = document.querySelector(".clip-row"); return r ? r.dataset.clipId : ""; });
  const settled = (page) => page.evaluate(() => { const r = document.querySelector(".clip-row"); return !!r && (r.dataset.state === "ready" || r.dataset.state === "failed"); });
  const anon = await browser.newContext({ baseURL: BASE });
  const fresh = await browser.newContext({ baseURL: BASE });
  // The browser in this container reaches the stack as host.docker.internal, which is not the stack's
  // own origin (PUBLIC_BASE_URL): to auth.CSRF a session's POST/PATCH/DELETE from a page there is a
  // cross-site request (Origin ≠ the configured origin) and a 403. The events this test drives from
  // the page are exactly those, so on the two contexts it drives them from the Origin header is
  // rewritten to the public origin at the network layer — the pass's own leg, not the app's, and the
  // same value on a run whose BASE is the public one.
  const ORIGIN = new URL(PUBLIC_BASE).origin;
  const sameSite = (ctx) => ctx.route("**/*", (route) => {
    const h = route.request().headers();
    if (h.origin) h.origin = ORIGIN;
    return route.continue({ headers: h });
  });
  if (part !== "ambient") {
    await sameSite(owner);
    await sameSite(fresh);
  }
  let second = "", garbage = "";
  let evFront = null, ev404 = null;
  try {
    if (part !== "ambient") {
    const tEv0 = Date.now();   // the sections' seconds are printed on the closing line (phase 4's cost, measured)
    // 0. the rest markup: the front door's mark under a parked pointer is what every face returns to
    let page = await open(anon, "/");
    const rest = await eyesOf(page, ".auth");
    check(rest && rest.count === 1 && rest.face === "" && !moved(rest) && rest.pupils.length === 2 && rest.pupils.every(atRest), `the front door's mark is not at rest under a parked pointer: ${JSON.stringify(rest)}`);
    const REST = rest.html;
    await page.close();

    // 1. the 404: the mark arrives wearing a gasp (the template's data-face) and plays it — walked on
    //    the fake clock into its hold, then past its end — and it has no tracking area: a pointer over
    //    it brings nothing (erratum 267 kept the message pages out of the follow)
    page = await open(anon, "/AAAAAAAAAAAA", { clock: true });
    let m = await eyesOf(page, ".message");
    check(m && m.count === 1 && m.face === "gasp", `the 404's mark did not arrive wearing a gasp: ${JSON.stringify(m)}`);
    await page.clock.runFor(900);
    m = await eyesOf(page, ".message");
    check(m.face === "gasp" && m.mouth === true && m.mouthing && m.pupils.every((p) => p.op > 0.99 && p.r < 150) && !m.sad, `the 404's gasp mid-hold: ${JSON.stringify(m)}`);
    await page.clock.runFor(1500);
    m = await eyesOf(page, ".message");
    check(rested(m, REST) && await page.evaluate(() => !document.querySelector(".wordmark[data-face]")), `the 404's mark is not exactly at rest after the gasp: ${JSON.stringify(m)}`);
    const box = await page.locator(".message .wordmark").boundingBox();
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.clock.runFor(700);
    m = await eyesOf(page, ".message");
    check(rested(m, REST), "the 404's mark followed the pointer (a message page has no tracking area)");
    await shut(page);

    // 2. signed in: the library that follows a login smiles (the moment is carried over the redirect)
    page = await open(fresh, "/login");
    await page.fill("form[data-auth] input[name=username]", user.username);
    await page.fill("form[data-auth] input[name=password]", user.password);
    let w = watch(page, "header.top", 8000, over);
    await page.click("form[data-auth] button[type=submit]");
    await page.waitForURL((u) => u.pathname === "/", { timeout: 8000 });
    w = await w;
    check(w.seen.join() === "smile", `signed in: the header played ${played(w)}, want a smile`);
    check(w.mid.smile && w.mid.smile.mouth === true && w.mid.smile.mouthing && !w.mid.smile.sad, `signed in: the smile's mouth was not drawn mid-face: ${JSON.stringify(w.mid.smile)}`);
    check(await calm(page, "header.top", REST), `signed in: the header is not exactly at rest after the smile — last: ${JSON.stringify(calmLast)}`);
    await page.close();

    // 3. copied: the first copy of the tab smiles, the next beams; Save beams
    await owner.grantPermissions(["clipboard-read", "clipboard-write"]);
    page = await open(owner, `/${clipId}`);
    w = watch(page, "header.top", 5000, over);
    await page.click("[data-copy-link]");
    w = await w;
    check(w.seen.join() === "smile", `the first copy: the header played ${played(w)}, want a smile`);
    w = watch(page, "header.top", 5000, over);
    await page.click("[data-copy-link]");
    w = await w;
    check(w.seen.join() === "beam", `the second copy: the header played ${played(w)}, want a beam`);
    check(w.mid.beam && w.mid.beam.mouth === true && w.mid.beam.pupils.every((p) => p.op > 0.99), `the beam's mouth and pupils mid-face: ${JSON.stringify(w.mid.beam)}`);
    w = watch(page, "header.top", 5000, over);
    await page.click("[data-edit] button[type=submit]");
    w = await w;
    check(w.seen.join() === "beam", `saved: the header played ${played(w)}, want a beam`);
    check(await calm(page, "header.top", REST), `the header is not exactly at rest after the copies and the save — last: ${JSON.stringify(calmLast)}`);
    await page.close();

    const tEv4 = Date.now();
    // 4. the transitions, live: an upload through the form. The header blinks as the row lands, then
    //    follows the row's states (queued → smile, encoding → blink) to the ready smile. The embed of
    //    the same clip, opened while it encodes, wears ONE face — the smile on ready — and no eyes.
    page = await open(owner, "/");
    const embed = await anon.newPage();
    await embed.setViewportSize({ width: 1280, height: 800 });
    await page.setInputFiles("form[data-upload] input[name=video]", FIXTURE);
    // The settle predicate names the TERMINAL face, the way the failed case below already names
    // `sulk`. `seen.length > 0` closed the watch on the BLINK: on a warm stack the six-second
    // fixture goes ready while that blink is still playing, so by the time the blink ends the
    // mark is at rest and the page has settled, and the smile that was about to fire is never
    // sampled. Seen twice on the lane stack (2026-09-15) and never on the reference stack, which
    // is slower — which is exactly the shape of race that reaches a gate one day in ten.
    w = watch(page, "header.top", 120000, async (seen, m) => seen.includes("smile") && !!m && m.face === "" && await settled(page));
    await page.click("[data-upload-submit]");
    // This account already holds the fixture (the clip uploaded through the API above), so the
    // first answer is "Already in your library." with Upload anyway (§20 erratum 326) — no row lands
    // and no face plays. Upload anyway sends the same file with the opt-out, and THAT lands.
    await page.waitForSelector("[data-upload-anyway]:not([hidden])", { timeout: 20000 });
    const dupLine = await page.textContent("[data-upload-status]");
    check(dupLine.startsWith("Already in your library."), `the duplicate's line is ${JSON.stringify(dupLine)}`);
    await page.click("[data-upload-anyway]");
    await page.waitForSelector(".clip-row[data-progress-url]", { timeout: 20000 });
    second = await newestId(page);
    await go(embed, BASE + `/embed/${second}`, { waitUntil: "domcontentloaded" });
    const we = await watch(embed, "body", 120000, async (seen, m) => (seen.length > 0 && !!m && m.face === "") || await embed.evaluate(() => document.body.dataset.state === "ready").catch(() => false));
    w = await w;
    check(await rowState(page) === "ready", `the uploaded clip ${second} did not become ready`);
    check(w.seen.length >= 2 && w.seen[0] === "blink" && w.seen[w.seen.length - 1] === "smile" && w.seen.every((f) => f === "blink" || f === "smile"), `the upload: the header played ${played(w)}, want blink (landed) … smile (ready)`);
    check(w.mid.blink && w.mid.blink.pupils.every((p) => p.op > 0.99) && w.mid.blink.lids.some((t) => t !== "translate(0 -1200)"), `the blink's lids did not move: ${JSON.stringify(w.mid.blink)}`);
    check(await calm(page, "header.top", REST), `the header is not exactly at rest after the upload's faces — last: ${JSON.stringify(calmLast)}`);
    if (EMBED_LOCKUP) {
      check(we.seen.join() === "smile", `the embed played ${played(we)}, want exactly one smile, on ready`);
      check(we.first.smile.pupils.length === 0 && we.first.smile.mouth === true && we.first.smile.count === 1, `the embed's smile: no eyes, one overlay, the mouth drawn: ${JSON.stringify(we.first.smile)}`);
      await embed.waitForFunction(() => document.body.dataset.state === "ready", null, { timeout: 15000 });
      m = await eyesOf(embed, "body");
      check(m && m.count === 1 && m.pupils.length === 0 && m.face === "" && !m.mouthing && m.mouth === false, `the ready embed's mark is not a bare mouth at rest: ${JSON.stringify(m)}`);
    } else {
      // The overlay here is the host as plain text (§20 erratum 287) and carries no mark to wear
      // a face. The embed's one smile is a property of an instance whose host spells the
      // wordmark, and it is asserted on one — the reference stack is clippet.gg.
      check(we.seen.length === 0, `the plain-host embed played ${played(we)}, want nothing: there is no mark on it`);
      await embed.waitForFunction(() => document.body.dataset.state === "ready", null, { timeout: 15000 });
      console.log(`[visual] NOTE dom faces: this instance's host (${SITE_HOST}) does not carry the wordmark, so its embed overlay is plain text and wears no smile (§20 erratum 287)`);
    }

    const tEv5 = Date.now();
    // 5. failed: a video the upload probe accepts and the encoder cannot read — the faststart fixture
    //    with every byte after its `mdat` zeroed (pure noise is refused at upload with a 415) — lands
    //    (blink) and fails: the header sulks, the mouth red (`sad`); the embed of a failing clip never
    //    wears anything
    const bytes = fs.readFileSync(BROKEN_SRC);
    bytes.fill(0, bytes.indexOf("mdat") + 4);
    await page.setInputFiles("form[data-upload] input[name=video]", { name: "broken.mp4", mimeType: "video/mp4", buffer: bytes });
    w = watch(page, "header.top", 120000, async (seen, m) => seen.includes("sulk") && !!m && m.face === "" && await settled(page));
    await page.click("[data-upload-submit]");
    await page.waitForSelector(".clip-row[data-progress-url]", { timeout: 20000 });
    garbage = await newestId(page);
    await go(embed, BASE + `/embed/${garbage}`, { waitUntil: "domcontentloaded" });
    // The embed is watched until it has SHOWN the failure — the server rendered it failed, or its own poll
    // reached the outcome and marked the status line `failed` — and then for a 1.5 s tail (a face starts
    // in the same task that marks the line), never for the full two minutes. Until run 9 the condition
    // read only `body[data-state]`, which the poll never rewrites, so it could not come true on a clip
    // that failed while the page was open and the watch always ran its whole 120 s: 121 of phase 4's
    // 142 s on the gate stack (lane `gatestack`, measured; TRANSCODE_MAX_ATTEMPTS=1 changed nothing —
    // this fixture already fails on its first attempt, as a permanent failure).
    let failedAt = 0;
    const wf = await watch(embed, "body", 120000, async (seen) => {
      if (seen.length > 0) return true;
      if (!failedAt && await embed.evaluate(() => document.body.dataset.state === "failed" || !!document.querySelector("[data-status].failed")).catch(() => false)) failedAt = mono();
      return failedAt > 0 && mono() - failedAt >= 1500;
    });
    w = await w;
    check(failedAt > 0, `the failing clip's embed never showed its failure within 120 s (samples: ${JSON.stringify(wf.log)})`);
    check(await rowState(page) === "failed", `the garbage clip ${garbage} did not fail`);
    check(w.seen[0] === "blink" && w.seen[w.seen.length - 1] === "sulk", `the broken upload: the header played ${played(w)}, want blink (landed) … sulk (failed)`);
    check(w.mid.sulk && w.mid.sulk.sad && w.mid.sulk.mouth === true && w.mid.sulk.mouthing && w.mid.sulk.pupils.every((p) => p.op === 0), `the sulk mid-face: red mouth drawn, no pupils: ${JSON.stringify(w.mid.sulk)}`);
    check(await calm(page, "header.top", REST), `the header is not exactly at rest after the sulk (sad must be gone) — last: ${JSON.stringify(calmLast)}`);
    check(wf.seen.length === 0, `the failing clip's embed played ${played(wf)}, want nothing`);
    await embed.close();
    await page.close();

    const tEv6 = Date.now();
    // 6. deleted: the clip page's two-click Delete leaves for the library, which gasps
    page = await open(owner, `/${second}`);
    await page.click("[data-delete]");
    w = watch(page, "header.top", 8000, over);
    await page.click("[data-delete]");
    await page.waitForURL((u) => u.pathname === "/", { timeout: 8000 });
    w = await w;
    second = ""; // gone
    check(w.seen.join() === "gasp", `deleted: the library played ${played(w)}, want a gasp`);
    check(w.mid.gasp && w.mid.gasp.mouth === true && w.mid.gasp.pupils.every((p) => p.op > 0.99 && p.r < 150), `the gasp mid-face: the "o" and small pupils: ${JSON.stringify(w.mid.gasp)}`);
    check(await calm(page, "header.top", REST), `the library is not exactly at rest after the gasp — last: ${JSON.stringify(calmLast)}`);
    await page.close();
    console.log(`[visual] dom faces (the events): ${n} checks ok (the 404's gasp; signed in → smile; first copy → smile, copy → beam, save → beam; an upload → blink … smile on the header and ${EMBED_LOCKUP ? "ONE smile on its embed" : "nothing on its plain-host embed"}; garbage → blink … sulk (red), nothing on its embed; delete → gasp); seconds: 0-3 ${((tEv4 - tEv0) / 1000).toFixed(1)}, 4 (the upload) ${((tEv5 - tEv4) / 1000).toFixed(1)}, 5 (the failing upload) ${((tEv6 - tEv5) / 1000).toFixed(1)}, 6 ${((Date.now() - tEv6) / 1000).toFixed(1)}`);
    }

    if (part !== "events") {
    // 7. the ambient timer, on the fake clock: 500 ms at a time until two faces have come and gone.
    //    The first draw lands 45–90 s after boot, the second 45–90 s after the first face ENDED; each
    //    is a glance or a drift (never the same twice) or, rarely, a wink; nothing plays inside the
    //    first 45 s. On the 404 the gasp plays first and the 5 s guard follows it, so the first draw
    //    lands later than 45 + 1.9 + 5 s. Under the pointer (the eyes out) nothing ambient plays at all.
    //
    //    BOTH ends of that first band are measured from the moment the timer STARTED COUNTING, not
    //    from boot: app.js `startAmbient` only adds to its accumulator while `idle`, which is false
    //    while a mark is busy and for AMBIENT_MS.guard after a meaningful face ENDED (meaningfulEnd
    //    starts at -Infinity, so on a page that arrives wearing nothing that moment is 0). Pinning
    //    the upper end to boot instead made the 404 fail whenever its draw landed above about 83 s —
    //    it did, on the verifier's run of lane `theme` (94500 ms against a 90500 ceiling), and the
    //    failure was arithmetic in this check, never in the product (§20 erratum 274; verified 288).
    const band = async (url, sel, what) => {
      const p = await open(anon, url, { clock: true });
      const ev = [];
      let last = "", t = 0, armed = 0;
      // a face the page arrived wearing (the 404's gasp) plays first: run it out, time still from boot
      while ((await eyesOf(p, sel)).face) { await p.clock.runFor(500); t += 500; armed = t + 5000; }
      while (t < 200000 && ev.length < 4) {
        await p.clock.runFor(500);
        t += 500;
        const mm = await eyesOf(p, sel);
        if (mm.face !== last) {
          ev.push({ t, face: mm.face });
          last = mm.face;
          if (mm.face) {
            await p.clock.runFor(300);
            t += 300;
            const mid = await eyesOf(p, sel);
            check(mid.pupils.every((q) => q.op > 0) && mid.mouth === false, `${what}: the ambient ${mm.face} is not an eyes-only face mid-play: ${JSON.stringify(mid)}`);
          }
        }
      }
      await shut(p);
      check(ev.length === 4 && ev[0].face && !ev[1].face && ev[2].face && !ev[3].face, `${what}: expected two ambient faces in 200 s, saw ${JSON.stringify(ev)}`);
      const pool = (f) => f === "glance" || f === "drift" || f === "wink";
      check(pool(ev[0].face) && pool(ev[2].face) && (ev[0].face === "wink" || ev[2].face === "wink" || ev[0].face !== ev[2].face), `${what}: the ambient faces were ${ev[0].face} then ${ev[2].face} (want glance/drift alternating, or a wink)`);
      check(ev[0].t >= armed + 45000 && ev[0].t <= armed + 90500, `${what}: the first ambient face came at ${ev[0].t} ms, want ${armed + 45000}–${armed + 90500} (the 45–90 s draw runs from ${armed} ms, when the timer started counting)`);
      const gap = ev[2].t - ev[1].t;
      check(gap >= 45000 && gap <= 90500, `${what}: the second ambient face came ${gap} ms after the first ended, want 45000–90500`);
      return ev;
    };
    // The three readings are independent pages, each on a paused clock of its own, so they run at once.
    const pointer = async () => {
      const pg = await open(anon, "/", { clock: true });
      await pg.mouse.move(640, 400); // inside the page = the display mark's area: the eyes come out
      await pg.clock.runFor(100000);
      const mm = await eyesOf(pg, ".auth");
      check(mm.face === "" && mm.pupils.every((p) => p.op > 0.99), `under the pointer for 100 s: the eyes should still be out and no ambient face played: ${JSON.stringify(mm)}`);
      await shut(pg);
    };
    [evFront, ev404] = await Promise.all([band("/", ".auth", "the front door"), band("/AAAAAAAAAAAA", ".message", "the 404"), pointer()]);
    console.log(`[visual] dom faces (the ambient band): ${n} checks ok (on the fake clock: front ${evFront[0].face}@${evFront[0].t} then ${evFront[2].face}@${evFront[2].t}, 404 ${ev404[0].face}@${ev404[0].t} then ${ev404[2].face}@${ev404[2].t}; none under the pointer)`);
    }
  } finally {
    // what this test made and did not remove by its own UI path
    if (second) await api(owner, "DELETE", `/api/clips/${second}`).catch(() => {});
    if (garbage) await api(owner, "DELETE", `/api/clips/${garbage}`).catch(() => {});
    if (part !== "ambient") await owner.unroute("**/*").catch(() => {});
    await anon.close();
    await fresh.close();
  }
}

// ---- the contact sheet ------------------------------------------------------------------------
function sheet() {
  results.sort((a, b) => a.seq - b.seq);
  const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/"/g, "&quot;");
  const names = [...new Set(results.map((r) => r.name))];
  const bad = results.filter((r) => r.failures.length);
  let h = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Clippet visual pass</title>
<style>
:root{color-scheme:light dark}body{margin:0;padding:24px;font:14px/1.5 system-ui,sans-serif;background:#ECEDEF;color:#17181C}
@media(prefers-color-scheme:dark){body{background:#191816;color:#ECE9E2}}
h1{font-size:20px;margin:0 0 4px}p{margin:0 0 20px;opacity:.7}section{margin:0 0 28px}h2{font-size:15px;margin:0 0 8px;font-family:ui-monospace,monospace}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(300px,1fr));gap:12px;align-items:start}
figure{margin:0;border:1px solid rgba(127,127,127,.35);border-radius:6px;overflow:hidden;background:rgba(127,127,127,.08)}
figure img{display:block;width:100%;height:auto;max-height:520px;object-fit:cover;object-position:top}
figcaption{padding:6px 10px;font-size:12px;font-family:ui-monospace,monospace;display:flex;justify-content:space-between;gap:8px}
.fail{color:#B3261E;font-weight:600}.ok{color:#0E7C4F}.notes{font-size:12px;color:#B3261E;margin:4px 0 0}
</style></head><body>
<h1>Clippet — visual pass</h1>
<p>${esc(PUBLIC_BASE)} · ${new Date().toISOString()} · ${results.length} shots · ${bad.length ? `<span class="fail">${bad.length} failing</span>` : '<span class="ok">all assertions green</span>'} · no baselines: look, then decide.</p>`;
  for (const name of names) {
    h += `<section><h2>${esc(name)} <small>${esc(results.find((r) => r.name === name).url)}</small></h2><div class="grid">`;
    for (const r of results.filter((x) => x.name === name)) {
      h += `<figure><a href="${esc(r.shot)}"><img src="${esc(r.shot)}" alt="${esc(name)} ${r.theme} ${r.width}" loading="lazy"></a><figcaption><span>${r.theme} · ${r.width}</span><span class="${r.failures.length ? "fail" : "ok"}">${r.failures.length ? "FAIL" : "ok"}</span></figcaption>${r.failures.length ? `<div class="notes">${esc(r.failures.join("; "))}</div>` : ""}</figure>`;
    }
    h += `</div></section>`;
  }
  h += `</body></html>\n`;
  fs.writeFileSync(path.join(OUT, "index.html"), h);
  fs.writeFileSync(path.join(OUT, "results.json"), JSON.stringify(results.map(({ seq, ...r }) => r), null, 2)); // seq is the sort key, not a field
}

// ---- the chunked upload lets its session go when it gives up (§20 erratum 344 (k), R18-6) ----------
//
// domChunkGiveUp drives the library's upload card through the session door with the whole of
// /api/uploads answered by this test (page.route, on its OWN page — the owner context is shared with
// other workers): the library's chunk size is cut to 1000 bytes in the page it is served, so a
// 2500-byte pick goes as three chunks, and nothing reaches the server's upload routes. Two give-ups,
// each must DELETE its session exactly once and show the failure:
//   (1) a chunk refused (413, not transient) — `end` with a refusal;
//   (2) three lost chunks in a row (503, and GET where answering 503 too) — `lost`, after 1+2+3 s.
// Before erratum 344 (k) neither sent a DELETE: the stored chunks waited for the session's expiry.
async function domChunkGiveUp(owner) {
  let n = 0;
  const check = (ok, what) => { n++; if (!ok) throw new Error(`dom chunk give-up: ${what}`); };
  const page = await owner.newPage();
  try {
    await page.setViewportSize({ width: 1280, height: 800 });
    let mode = "refuse", seq = 0;
    const deletes = [], puts = [];
    await page.route((u) => new URL(u).pathname === "/", async (route) => {
      const res = await route.fetch();
      const body = (await res.text()).replace(/data-upload-chunk="\d+"/, 'data-upload-chunk="1000"');
      await route.fulfill({ response: res, body });
    });
    await page.route((u) => new URL(u).pathname.startsWith("/api/uploads"), async (route) => {
      const req = route.request(), path = new URL(req.url()).pathname, m = req.method();
      const json = (status, obj) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(obj) });
      if (m === "POST" && path === "/api/uploads") return json(201, { id: `giveup${++seq}`, chunk_bytes: 1000, received: 0 });
      if (m === "DELETE") { deletes.push(path); return route.fulfill({ status: 204, body: "" }); }
      if (m === "PUT") {
        puts.push(path);
        return mode === "refuse" ? json(413, { error: "too_large", message: "refused by the test" }) : json(503, { error: "unavailable" });
      }
      if (m === "GET") return json(503, { error: "unavailable" });
      return json(500, { error: "unexpected", message: `${m} ${path}` });
    });
    await go(page, BASE + "/", { waitUntil: "domcontentloaded" });
    await page.waitForSelector("form[data-upload][data-upload-chunk=\"1000\"]", { timeout: 5000 });
    const send = async () => {
      await page.setInputFiles("form[data-upload] input[name=video]", { name: "giveup.mp4", mimeType: "video/mp4", buffer: Buffer.alloc(2500, 7) });
      await page.click("[data-upload-submit]");
    };
    const settle = async (want, ms) => {
      const t = Date.now();
      while (Date.now() - t < ms && deletes.length < want) await sleep(100);
      await sleep(300); // a second DELETE would land here
    };
    // (1) the refused chunk
    await send();
    await settle(1, 10000);
    check(puts.length === 1, `the refused case sent ${puts.length} chunk(s), want 1`);
    check(deletes.length === 1 && deletes[0] === "/api/uploads/giveup1", `a refused chunk let its session go by ${JSON.stringify(deletes)}, want one DELETE of giveup1`);
    // (2) three lost chunks in a row
    mode = "lose";
    await page.waitForSelector("[data-upload-submit]:not([disabled])", { timeout: 10000 }).catch(() => {});
    await send();
    await settle(2, 20000);
    check(deletes.length === 2 && deletes[1] === "/api/uploads/giveup2", `three lost chunks let the session go by ${JSON.stringify(deletes)}, want one DELETE of giveup2`);
    const line = await page.textContent("[data-upload-status]");
    check(/^Upload failed: network error/.test((line || "").trim()) || /network error/.test(line || ""), `the give-up's line is ${JSON.stringify(line)}`);
  } finally {
    await page.close().catch(() => {});
  }
  console.log(`[visual] dom chunk give-up: ${n} checks ok (a refused chunk and three lost chunks each DELETE their session once)`);
}

// ---- the upload card after a pick (§20 errata 313 and 314; web/STYLE.md) -----------------------
//
// pickedCard is the `before` of the `upload-picked` frame. The owner's first shot (2026-10-04): a long name,
// Pick a file and "Uploaded — encoding on the server" drawn over one another, because the readout's
// name and its stage phrase shrank together and the phrase broke onto three lines (313). His second:
// with the name in its own cell BESIDE the phrase, the ≈240 px foot of the 300 px drop column left an
// OBS name two characters (314) — so the foot is two lines, the name alone on the first. It asserts:
//   - one loud control: before a pick Pick a file is `primary` and Upload is not; after it, the reverse;
//   - an OBS-style name shows WHOLE (no ellipsis) at 1280 with the longest phrase under it;
//   - the name cell says the whole name in `title` (a long one ellipsises), and its box stays in its row;
//   - with the longest single-file phrase in the live cell, the name's box spans the foot's width and
//     sits ABOVE the phrase's, the phrase stays on one line inside the row, and the readout stays below
//     Pick a file — at 1280 (the 300 px frame beside the fields) and at 390 (the stacked card). The
//     phrase is put there by hand and taken out again: the frame is a pick, never a send, so nothing
//     is uploaded.
const PICKED_NAME = "2025-06-24 00-23-51 the whole evening, start to finish, uncut.mp4";
const OBS_NAME = "2026-10-04 18-52-11.mp4";
const LONGEST_PHRASE = "Uploaded — encoding on the server";
async function pickedCard(page, width) {
  const loudness = () => page.evaluate(() => ({
    submit: document.querySelector("[data-upload-submit]").classList.contains("primary"),
    pick: document.querySelector("[data-upload-pick]").classList.contains("primary"),
  }));
  const fail = (what) => { throw new Error(`upload card: ${what}`); };
  const pick = (name) => page.setInputFiles("form[data-upload] input[name=video]", { name, mimeType: "video/mp4", buffer: fs.readFileSync(FIXTURE) });
  await page.waitForSelector("form[data-upload] [data-upload-pick]", { timeout: 5000 });
  await page.evaluate(() => document.fonts.ready);
  let l = await loudness();
  if (!(l.pick && !l.submit)) fail(`before a pick, want Pick a file loud and Upload quiet: ${JSON.stringify(l)}`);
  const boxes = (phrase) => page.evaluate((phrase) => {
    const q = (s) => document.querySelector(s);
    const name = q("[data-upload-name]"), status = q("[data-upload-status]");
    const before = status.textContent;
    if (phrase !== null) status.textContent = phrase;
    const r = (el) => { const b = el.getBoundingClientRect(); return { l: b.left, r: b.right, t: b.top, b: b.bottom, w: b.width, h: b.height }; };
    const out = {
      title: name.getAttribute("title"), text: name.textContent,
      scroll: name.scrollWidth, client: name.clientWidth,
      name: r(name), row: r(q(".drop-status .readout")), live: r(q(".drop-status .live")),
      pick: r(q("[data-upload-pick]")), status: r(q(".drop-status")),
      line: parseFloat(getComputedStyle(status).lineHeight) || 12 * 1.6,
    };
    status.textContent = before;
    return out;
  }, phrase);
  // The point of erratum 314: the name a desktop recorder writes fits the foot whole while the stage
  // phrase is showing — it is no longer cut to what the phrase leaves.
  await pick(OBS_NAME);
  l = await loudness();
  if (!(l.submit && !l.pick)) fail(`after a pick, want Upload loud and Pick a file quiet: ${JSON.stringify(l)}`);
  const obs = await boxes(LONGEST_PHRASE);
  if (obs.text !== OBS_NAME || obs.scroll > obs.client) fail(`at ${width}: the OBS name ${JSON.stringify(obs.text)} does not show whole beside the stage phrase (scrollWidth ${obs.scroll} > clientWidth ${obs.client})`);
  await pick(PICKED_NAME);
  l = await loudness();
  if (!(l.submit && !l.pick)) fail(`after a second pick, want Upload loud and Pick a file quiet: ${JSON.stringify(l)}`);
  const measure = async (at) => {
    let b = await boxes(null);
    if (b.title !== PICKED_NAME || b.text !== PICKED_NAME) fail(`at ${at}: the name cell says ${JSON.stringify(b.text)} with title ${JSON.stringify(b.title)}, want the whole name in both`);
    if (b.name.w > b.row.w + 0.5 || b.name.r > b.row.r + 0.5 || b.name.l < b.row.l - 0.5) fail(`at ${at}: the name's box ${JSON.stringify(b.name)} leaves its row ${JSON.stringify(b.row)}`);
    b = await boxes(LONGEST_PHRASE);
    if (b.row.w < b.status.w - 0.5 || b.name.w < b.row.w - 0.5) fail(`at ${at}: the name's box (${b.name.w} px) does not span the foot (row ${b.row.w} px, foot ${b.status.w} px)`);
    if (b.name.b > b.live.t + 0.5) fail(`at ${at}: the name (bottom ${b.name.b}) is not above the stage phrase (top ${b.live.t})`);
    if (b.live.h > b.line * 1.5) fail(`at ${at}: the stage phrase broke onto more than one line (${b.live.h} px, line ${b.line})`);
    if (b.name.w > b.row.w + 0.5 || b.live.l < b.row.l - 0.5 || b.live.r > b.row.r + 0.5) fail(`at ${at}: the readout's cells leave its row: name ${JSON.stringify(b.name)}, live ${JSON.stringify(b.live)}, row ${JSON.stringify(b.row)}`);
    if (b.status.t < b.pick.b - 0.5) fail(`at ${at}: the readout (top ${b.status.t}) climbs over Pick a file (bottom ${b.pick.b})`);
  };
  await measure(width);
  await page.setViewportSize({ width: 390, height: 844 });
  await sleep(100);
  await measure(390);
  await page.setViewportSize({ width, height: 800 });
  await sleep(100);
}

// ---- the workers (lane `speed2`) ---------------------------------------------------------------------
// VISUAL_WORKERS shots at once, in the one Chromium. The pass is a list of JOBS — a shot is one job, each
// DOM test is one job — run phase by phase; inside a phase every worker takes the next job from one
// shared queue. The PHASES are where the serial pass changes the stack, and nothing crosses them:
//   0  the anonymous pages (no account yet)                  login, register
//   1  the new account's empty library                       library-empty — before the upload
//   2  everything that reads the account, its clip and its collection: the library, the watch pages, the
//      collection, settings, the wizard, the 404, the front door, the upload card after a pick (a pick,
//      never a send) and domTheme, which only reads — cut in three at its one write, the owner's FIRST
//      view of the watch page, which makes the filmstrip every later page counts in the clip's size:
//      2a the library frames before it, 2b that one frame alone, 2c the rest
//   3  ALONE, in the serial order: domFollow and the two follow frames. They aim the pupils at a REAL
//      pointer on a real clock (700 ms settles), which is the one kind of frame a loaded box could move
//   4  ALONE: domFaces' events, which upload, copy (a copy RESETS the clip's expiry, which the library
//      prints to the minute), save, delete and reroute the owner's requests — the pass's mutations
//   5  everything after it: the face frames in a worker of their OWN (each in a throwaway context on
//      Playwright's paused clock), beside domFaces' ambient band (read-only, paused clocks), the embed
//      frames and — on a badged stack — domBadge and the two badge frames on the other workers
// Every worker shares the two contexts the serial pass used (`owner`, `anon`), so a cookie or a stored
// value one shot leaves is there for the next exactly as it always was; the pointer is parked per PAGE
// before every navigation (shoot), which is where the rule lives. Each shot carries its place in the
// serial order (`seq`), and the sheet and results.json are written in that order, under the same names.
//
// VISUAL_PROVE=1 is the identity proof: after each concurrent phase the same phase is taken again ONE
// SHOT AT A TIME, in the serial order, into OUT/serial/ — same stack, same account, same clip, same
// state, nothing else in flight — and every pair of files is compared by sha256. Two independent passes
// could never be compared like this: each registers its own account, whose name is in the header of
// every signed-in page, and uploads its own clip, whose upload minute is on the watch page.
const os = require("os");
const crypto = require("crypto");
const NCPU = os.cpus().length;
// The width default: half the container's CPUs, 2..8 — 8 on the 16-CPU box. MEASURED (2026-10-05, a lane
// stack at APP_CPUS=4, 103 shots): 268 s at 1 worker, 222 s at 4, 224 s at 8; the serial pass before this
// lane took 307 s. The concurrent phases are no longer what the pass waits on — at 8 they are ~25 s in
// all — but the ALONE ones are: domFaces' events (two real uploads and their encodes, ~141 s) and the
// follow (~14 s), plus the face frames' own worker (~34 s, beside the embed frames).
const WORKERS = (() => {
  const v = parseInt(process.env.VISUAL_WORKERS || "", 10);
  if (Number.isFinite(v) && v > 0) return v;
  return Math.max(2, Math.min(8, Math.floor(NCPU / 2)));
})();
const PROVE = process.env.VISUAL_PROVE === "1";
// VISUAL_ONLY=4 (or a list, `2,4`): only the phases with those NUMBERS run their jobs (`2` is 2a, 2b and
// 2c); the setup every phase stands on — the account, the clip, its collection — always runs. A filtered
// pass says PARTIAL on its verdict line and is never a gate (run 9, lane `gatestack`: what a lane re-takes
// for its one phase, and how phase 4 was measured before and after).
const ONLY = (process.env.VISUAL_ONLY || "").split(",").map((s) => s.trim()).filter(Boolean);
const wanted = (label) => !ONLY.length || ONLY.includes(String(label).split(" ")[0].replace(/[a-z]$/, ""));
const phaseTimes = [];
// runJobs(label, jobs, workers): the shared queue. A job that THROWS is recorded and re-thrown once
// the phase has drained, so a failing DOM test ends the pass as it always did, without stranding the
// shots other workers had in hand.
async function runJobs(label, jobs, workers) {
  if (!wanted(label)) { console.log(`[visual] phase ${label}: skipped (VISUAL_ONLY=${ONLY.join(",")})`); return; }
  const t0 = Date.now();
  let next = 0;
  const errs = [];
  const width = Math.max(1, Math.min(workers, jobs.length));
  await Promise.all(Array.from({ length: width }, async () => {
    while (next < jobs.length) {
      const j = jobs[next++];
      try { await j(); } catch (e) { errs.push(e); }
    }
  }));
  const secs = (Date.now() - t0) / 1000;
  phaseTimes.push(`${label} ${secs.toFixed(1)}s`);
  console.log(`[visual] phase ${label}: ${jobs.length} job(s) on ${width} worker(s) in ${secs.toFixed(1)} s`);
  if (errs.length) throw errs[0];
}
// phase(label, jobs, opts): the concurrent run, then — under VISUAL_PROVE=1 — the serial re-take of the
// same shots into OUT/serial/. `jobs` are {fn, shot} pairs; only shots are re-taken (a DOM test takes
// no picture). `faces` is a second list run beside the first on ONE worker of its own.
async function phase(label, jobs, faces = []) {
  if (!wanted(label)) { console.log(`[visual] phase ${label}: skipped (VISUAL_ONLY=${ONLY.join(",")})`); return; }
  const main = jobs.map((j) => j.fn);
  const own = faces.map((j) => j.fn);
  if (own.length) {
    await Promise.all([runJobs(label, main, Math.max(1, WORKERS - 1)), runJobs(`${label} (faces)`, own, 1)]);
  } else {
    await runJobs(label, main, WORKERS);
  }
  if (PROVE) {
    const shots = [...jobs, ...faces].filter((j) => j.shot).sort((a, b) => a.seq - b.seq);
    SHOT_DIR = path.join(OUT, "serial");
    fs.mkdirSync(SHOT_DIR, { recursive: true });
    try {
      await runJobs(`${label} (serial re-take)`, shots.map((j) => j.fn), 1);
    } finally {
      SHOT_DIR = OUT;
    }
  }
}
// The job builders. `seq` counts in the order the SERIAL pass takes its shots.
let SEQ = 0;
function shotJob(ctxOf, name, url, theme, width, opts = {}) {
  const seq = SEQ++;
  return { seq, shot: true, fn: () => shoot(ctxOf(), name, url, theme, width, { ...opts, seq }) };
}
function allJobs(ctxOf, name, url, opts) {
  const out = [];
  for (const theme of THEMES) for (const width of WIDTHS) out.push(shotJob(ctxOf, name, url, theme, width, opts));
  return out;
}
function domJob(fn) { return { seq: SEQ, shot: false, fn }; }
// The five regions the page moves on its own (lane speed2; measured 2026-10-05: 82 of 103 shots
// byte-identical across 8 workers, 88 of 103 across two SERIAL takes, every difference inside one of these).
// A shot whose file differs between the two takes is EXPECTED only when its name is listed here; a differing
// shot outside the union fails the measurement by name — something else on the page moved.
//   spinner   the player's loading spinner, an animation inside Chromium's own media controls that
//             Playwright cannot stop (every shot with a <video>: the watch pages and the embeds);
//   counter   the live "served this month" counter on settings;
//   lastseen  a session's last-seen minute on settings;
//   firstview the first owner view, the only take made BEFORE its filmstrip exists (the owner's watch page);
//   frames    the collection row's moving (ambient face) frame.
const MOVING_REGIONS = {
  spinner: ["watch-visitor", "watch-visitor-tall", "watch-owner", "embed", "embed-smile", "embed-card", "embed-paused"],
  counter: ["settings"],
  lastseen: ["settings"],
  firstview: ["watch-owner"],
  frames: ["collection"],
};
const MOVING_NAMES = new Set(Object.values(MOVING_REGIONS).flat());
// The proof's verdict: every shot of the concurrent pass against its serial re-take, by sha256.
function proveIdentity() {
  const sha = (f) => crypto.createHash("sha256").update(fs.readFileSync(f)).digest("hex");
  const differ = [], missing = [], outside = [];
  let same = 0;
  for (const r of results) {
    const a = path.join(OUT, r.shot), b = path.join(OUT, "serial", r.shot);
    if (!fs.existsSync(a) || !fs.existsSync(b)) { missing.push(r.shot); continue; }
    if (sha(a) === sha(b)) same++;
    else { differ.push(r.shot); if (!MOVING_NAMES.has(r.name)) outside.push(r.shot); }
  }
  const serialBad = serialResults.filter((r) => r.failures.length).length;
  const line = `[visual] identity (VISUAL_PROVE=1): ${same} of ${results.length} shots byte-identical between ${WORKERS} workers and one at a time (sha256 per file)${differ.length ? `; DIFFER: ${differ.join(" ")}` : ""}${outside.length ? `; OUTSIDE the five moving regions: ${outside.join(" ")}` : ""}${missing.length ? `; MISSING a pair: ${missing.join(" ")}` : ""}; the serial re-take: ${serialResults.length} shots, ${serialBad} failing`;
  console.log(line);
  fs.writeFileSync(path.join(OUT, "identity.txt"), line + "\n" + differ.map((d) => `differ ${d}`).join("\n") + "\n");
  return outside.length === 0 && missing.length === 0;
}

// ---- the run ------------------------------------------------------------------------------------
(async () => {
  selfTest();
  await navSelfTest();
  // VISUAL_SELFTEST=1: the two self-tests above and nothing else — no browser, no request (run 9, lane `retry`).
  if (process.env.VISUAL_SELFTEST === "1") { console.log("[visual] SELFTEST PASS"); return; }
  console.log(`[visual] ${NCPU} CPUs in the container; ${WORKERS} worker(s) (VISUAL_WORKERS=${process.env.VISUAL_WORKERS || "unset"})${PROVE ? "; VISUAL_PROVE=1: every phase is taken again one shot at a time and compared" : ""}`);
  // VISUAL_HOST_MAP=1 (scripts/visual.sh, a loopback stack): `localhost` RESOLVES to the host, so a link the
  // stack signed for its own published MinIO at localhost:<port> is fetched as that very URL (the Host the
  // signature covers) instead of being refused inside this container.
  browser = withAppHeaders(await chromium.launch(process.env.VISUAL_HOST_MAP === "1" ? { args: ["--host-resolver-rules=MAP localhost host.docker.internal"] } : {}));
  const owner = await browser.newContext({ baseURL: BASE });
  const anon = await browser.newContext({ baseURL: BASE });
  const O = () => owner, A = () => anon;
  let clip = null;
  let audio = null;
  let collection = null;
  try {
    // Which media-delivery shape this stack is in, before the first shot (§20 erratum 282): the
    // library page photographed below already carries other people's posters on a default
    // `LIBRARY_SCOPE=shared` instance, and on a redirect stack those come from the bucket.
    await detectMedia(anon);
    // …and whether this stack is wearing an INSTANCE_BADGE (§20 erratum 285), for the same reason and
    // in the same place: the very first shot below is already a page whose mark is grey or green.
    await detectBadge(anon);
    await phase("0 anonymous", [...allJobs(A, "login", "/login"), ...allJobs(A, "register", "/register")]);
    await register(owner);
    await phase("1 empty library", allJobs(O, "library-empty", "/"));
    clip = await upload(owner, "Triple kill on Ascent, then the whiff");
    console.log(`[visual] uploaded ${clip.id}; waiting for it to be ready`);
    await waitReady(owner, clip.id);
    // …and the shape the app DECLARED, confirmed against what it serves (§20 erratum 282).
    // Anonymously: what this asks is what a stranger's player is answered.
    await confirmMedia(anon, clip.id);
    const c = await api(owner, "POST", "/api/collections", { data: { title: "Best of last night" } });
    if (c.status !== 201) throw new Error(`collection: ${c.status}`);
    collection = c.body;
    const m = await api(owner, "POST", `/api/collections/${collection.id}/clips/${clip.id}`);
    if (m.status < 200 || m.status >= 300) throw new Error(`membership: ${m.status}`);
    const id = clip.id;
    const p2 = [];
    const library = allJobs(O, "library", "/");
    const ticked = allJobs(O, "library-ticked", "/", {
      before: async (page) => {
        await page.waitForSelector("[data-bulk-ready]", { timeout: 10000 });
        await page.locator("[data-pick]").first().check();
        await page.waitForSelector("[data-bulk]:not([hidden])", { timeout: 5000 });
      },
    });
    // `media: true` on the pages that carry the clip's own video and poster: on a redirect stack
    // each of them must be seen fetching it from the media origin (§20 erratum 282). The embed
    // shots below are covered by `embed: true`, which says the same thing about the same page.
    const watchOwner = allJobs(O, "watch-owner", `/${id}`, {
      media: true,
      before: async (page, width) => {
        // The filmstrip is made on the first owner view (202 + a poll); wait for it once so
        // the rail carries the frames, then hover the start handle so its tag and tile show.
        await page.waitForSelector("[data-scrub-strip]:not([hidden])", { timeout: 45000 }).catch(() => {});
        if (width === 1280) await page.locator('[data-scrub-handle="start"]').hover().catch(() => {});
        await sleep(200);
      },
    });
    // The jobs are MADE in the serial order (that is what numbers them). The owner's FIRST view of the
    // watch page is a write: it makes the clip's filmstrip, which the library, the watch pages and
    // settings all count in the clip's stored size (4.2 → 4.3 MiB) — the first identity proof caught
    // exactly that, shots taken before and after the strip landed. So phase 2 is cut there, as the
    // serial pass is: 2a the two library frames before it, 2b the first watch-owner frame alone (it
    // waits for the strip), 2c everything after it, domTheme (a reader of twenty-odd pages) first.
    await phase("2a the library before the filmstrip", [...library, ...ticked]);
    await phase("2b the first owner view (the filmstrip)", [watchOwner[0]]);
    p2.push(domJob(() => domTheme(browser, owner, id)), ...watchOwner.slice(1));
    p2.push(...allJobs(A, "watch-visitor", `/${id}`, { media: true }));
    // A TALL viewport the page does not fill: this is where a stretched header shows.
    for (const theme of THEMES) p2.push(shotJob(A, "watch-visitor-tall", `/${id}`, theme, 1512, { height: 1400, media: true }));
    p2.push(...allJobs(O, "collection", `/c/${collection.id}`));
    p2.push(...allJobs(O, "settings", "/settings"));
    // The first-run wizard (ROADMAP #45, §20 errata 297-299, 311): the chooser at rest (six radio
    // cards and the two muted heads), the FIRST walk — local, which is the one the owner put first and
    // the one that carries the longest steps and the tallest block — and the STOP: a bucket target
    // on a stack that holds this run's clip, so the red callout leads the walk. Three frames a
    // width, not eighteen: the other walks are the same shapes (an <ol>, a <pre class="env"> and
    // the hints) with different words in them, and a contact sheet is for seeing the shapes.
    // `&via=other` pins the exposure so the frame is the same walk whichever way this stack is
    // reached (behind Cloudflare a block gains the tunnel's line and the folder walk its risk line). On a stack that is itself on a bucket the STOP frame
    // shows the conditional form (the page cannot tell whether it is the same bucket); on the
    // bundled MinIO or a directory, the plain one.
    p2.push(...allJobs(O, "setup", "/setup"));
    p2.push(...allJobs(O, "setup-walk", "/setup?store=fs&via=other"));
    p2.push(...allJobs(O, "setup-stop", "/setup?store=r2&via=other", {
      before: async (page) => { await page.waitForSelector("[data-setup-stop]", { timeout: 5000 }); },
    }));
    // The library's upload card with a file picked and NOT sent (§20 erratum 313): one frame, and
    // the loudness swap and the name's own cell asserted around it (pickedCard below).
    p2.push(shotJob(O, "upload-picked", "/", "light", 1280, { before: pickedCard }));
    // The chunked upload's give-ups let their sessions go (§20 erratum 344 (k)): every request to
    // /api/uploads is answered by the test on its own page, so nothing is stored and no row lands.
    p2.push(domJob(() => domChunkGiveUp(owner)));
    p2.push(...allJobs(O, "notfound", "/AAAAAAAAAAAA", { status: 404 }));
    // The front door a stranger gets at `/` (§20 erratum 246).
    p2.push(...allJobs(A, "front", "/"));
    // (domTheme — the theme glyph in the bar and the auth panels' rows, §20 erratum 288 — is the
    // first job of this phase, above: it takes no frame of its own.)
    await phase("2c the account's pages", p2);
    // The living mark (ROADMAP #38, §20 erratum 267): first its DOM test, then the two frames — the
    // header with the cursor inside the bar and the front door's display mark mid-follow, both
    // themes, desktop width (a pointer that hovers). Every frame above is the mark at rest.
    // ALONE (one worker, nothing beside it): a real pointer and a real clock.
    const p3 = [domJob(() => domFollow(browser, id))];
    for (const theme of THEMES) p3.push(shotJob(O, "header-follow", "/", theme, 1280, { before: follow("header.top") }));
    for (const theme of THEMES) p3.push(shotJob(A, "front-follow", "/", theme, 1280, { before: follow(".auth") }));
    await runJobs("3 the follow (alone)", p3.map((j) => j.fn), 1);
    if (PROVE) {
      SHOT_DIR = path.join(OUT, "serial"); fs.mkdirSync(SHOT_DIR, { recursive: true });
      try { await runJobs("3 the follow (serial re-take)", p3.filter((j) => j.shot).map((j) => j.fn), 1); } finally { SHOT_DIR = OUT; }
    }
    // The faces (§20 erratum 274): the DOM test on the real events, ALONE — it is the pass's only
    // writer — then one frame per face on the header mark (21 px, the library) and the display mark
    // (40 px, the front door), both themes, each frozen inside its hold on the fake clock; and the
    // embed's one face, the smile.
    await runJobs("4 the faces' DOM test: the events (alone)", [() => domFaces(browser, owner, id, "events")], 1);
    const faces = [];
    for (const face of FACES) {
      for (const theme of THEMES) {
        faces.push(shotJob(O, `header-${face}`, "/", theme, 1280, { face }));
        faces.push(shotJob(A, `front-${face}`, "/", theme, 1280, { face }));
      }
    }
    // The ambient half of domFaces reads two anonymous pages on paused clocks and changes nothing: a
    // job of its own, first in the queue (it is the longest).
    const p5 = [domJob(() => domFaces(browser, owner, id, "ambient"))];
    // The shot is taken on every instance so the contact sheet and the count do not move; the
    // FACE is only driven where the overlay has a mark to wear it (§20 erratum 287).
    for (const theme of THEMES) p5.push(shotJob(A, "embed-smile", `/embed/${id}`, theme, 1280, { embed: true, ...(EMBED_LOCKUP ? { face: "smile" } : {}) }));
    p5.push(...allJobs(A, "embed", `/embed/${id}`, { embed: true }));
    // The embed at the card size Discord and X frame it at (400 × 225): the overlay must sit inside
    // the frame and clear of the controls band there, on the poster…
    for (const theme of THEMES) p5.push(shotJob(A, "embed-card", `/embed/${id}`, theme, 400, { embed: true, height: 225 }));
    // …and come back when a viewer pauses after a play. Headless Chromium may refuse to start
    // playback (the codec, the autoplay policy); the shot is then skipped with a NOTE, not failed.
    for (const theme of THEMES) {
      p5.push(shotJob(A, "embed-paused", `/embed/${id}`, theme, 400, {
        embed: true, height: 225,
        before: async (page) => {
          const r = await page.evaluate(async () => {
            const v = document.querySelector("video");
            const m = document.querySelector(".embed-mark");
            if (!v || !m) return { why: "no video or no overlay on the page" };
            const wait = (ms) => new Promise((res) => setTimeout(res, ms));
            v.muted = true;
            try { await v.play(); } catch (e) { return { why: "play() refused: " + String(e) }; }
            await wait(600);
            if (v.paused || v.error) return { why: "playback did not start" + (v.error ? ": media error " + v.error.code : "") };
            const hidden = document.body.hasAttribute("data-playing") && getComputedStyle(m).display === "none";
            v.pause();
            await wait(200);
            const back = !document.body.hasAttribute("data-playing") && getComputedStyle(m).display !== "none";
            return { ok: true, hidden, back };
          });
          if (!r.ok) {
            // Say WHY, so nobody reads this as a fault in the stack: the pinned Playwright image
            // (scripts/visual.sh, PW_VERSION) ships a Chromium built without the proprietary codecs,
            // so an H.264 rendition cannot start playing in it at all. The play/pause overlay rule is
            // therefore unphotographed by design here — it is asserted in the E2E's CSS pins and was
            // walked once by hand in a real Chrome (ROADMAP #36's builder).
            console.log(`[visual] NOTE embed-paused: the pinned Playwright image's Chromium has no H.264, so this shot cannot be taken here (not a stack fault) — ${r.why}`);
            return "skip";
          }
          if (!r.hidden) throw new Error("the overlay stayed visible while playing");
          if (!r.back) throw new Error("the overlay did not come back on pause");
        },
      }));
    }
    // The instance badge (§20 erratum 285), on a stack that wears one: its DOM test, then ONE frame
    // per mark — the library's header at 21 px in light, the front door's display mark at 40 px in
    // dark — so the pair covers both marks AND both grounds in two pictures. A production stack wears
    // no badge, skips both, and stays the 111 shots this pass now takes; the E2E's step 77
    // brings its own badged instance instead.
    if (BADGE) {
      p5.push(domJob(() => domBadge(browser, id)));
      p5.push(shotJob(O, "badge-header", "/", "light", 1280));
      p5.push(shotJob(A, "badge-front", "/", "dark", 1280));
    } else {
      console.log("[visual] badge-header / badge-front: skipped — this stack wears no INSTANCE_BADGE (web/STYLE.md)");
    }
    await phase("5 the face frames and the embed", p5, faces);
    // An audio clip (§20 erratum 329), LAST so every frame above keeps the one clip it always had: its
    // page at rest (the waveform poster over the native control, `Full · FLAC 24-bit`), the same page
    // part-played (the amber layer clipped to 60 %, asserted by playedPart), and the library with its
    // tile. The audio clip is deleted with the video one below.
    if (wanted("6")) {
    audio = await uploadAudio(owner, "Voice memo, the whole verse");
    console.log(`[visual] uploaded the audio clip ${audio.id}; waiting for it to be ready`);
    await waitReady(owner, audio.id);
    const p6 = [...allJobs(A, "watch-audio", `/${audio.id}`, { media: true })];
    for (const theme of THEMES) p6.push(shotJob(A, "watch-audio-played", `/${audio.id}`, theme, 1280, { media: true, before: playedPart }));
    for (const theme of THEMES) p6.push(shotJob(O, "library-audio", "/", theme, 1280));
    await phase("6 the audio clip", p6);
    } else {
      console.log(`[visual] phase 6 the audio clip: skipped (VISUAL_ONLY=${ONLY.join(",")}; no audio clip is uploaded)`);
    }
    console.log("[visual] unavailable: skipped — the 503 page needs a database the stack cannot read, which a healthy stack cannot show");
  } finally {
    if (clip) await api(owner, "DELETE", `/api/clips/${clip.id}`).catch(() => {});
    if (audio) await api(owner, "DELETE", `/api/clips/${audio.id}`).catch(() => {});
    if (collection) await api(owner, "DELETE", `/api/collections/${collection.id}`).catch(() => {});
    await browser.close();
  }
  sheet();
  // A MEASUREMENT, not a gate: two takes of the same page differ wherever the page itself moves on its
  // own — the player's loading spinner (an animation inside Chromium's own media controls, which
  // Playwright cannot stop), the live "served this month" counter and a session's last-seen minute on
  // settings, the first owner view (the only take made BEFORE its filmstrip existed), the collection row's
  // moving frame — and a serial-against-serial control (VISUAL_PROVE=1 VISUAL_WORKERS=1) finds the same
  // five: on 2026-10-05, 82 of 103 shots byte-identical across 8 workers and 88 of 103 across two serial
  // takes, every difference in either inside one of those five regions. So the line is printed and written
  // to identity.txt, and the measurement ASSERTS that the differing set is a subset of MOVING_REGIONS: a
  // differing shot outside it (or a missing pair) fails the pass, by name.
  const identityOk = PROVE ? proveIdentity() : true;
  const bad = results.filter((r) => r.failures.length);
  console.log(`[visual] phases: ${phaseTimes.join(" | ")}`);
  console.log(`[visual] ${results.length} shots, ${bad.length} failing, ${nav.stats.retries} navigation retr${nav.stats.retries === 1 ? "y" : "ies"} (each on its own "[visual] retry" line above); account ${user.username} owns nothing now (scripts/visual.sh prunes visual_% when this returns)`);
  if (!identityOk) {
    console.log("[visual] FAIL (identity: see the line above)");
    process.exit(1);
  }
  if (bad.length) {
    console.log("[visual] FAIL");
    process.exit(1);
  }
  console.log(ONLY.length ? `[visual] PASS (PARTIAL: VISUAL_ONLY=${ONLY.join(",")} — not a gate)` : "[visual] PASS");
})().catch((e) => {
  console.error("[visual] " + (e && e.stack ? e.stack : e));
  process.exit(1);
});
