# 77-design.sh — the visual system "Kept" (DESIGN.md §20 errata 240-243; web/STYLE.md): steps 77-79.
# Sourced by scripts/e2e.sh (see scripts/e2e.d/README.md). Every name here is DS_-prefixed.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=DS_,BD_ buckets=-
# e2e-covers: internal/web/theme.go internal/web/mark.go internal/web/meta.go internal/web/static.go
# e2e-covers: internal/web/server.go internal/web/web.go web/templates/*.html web/static/*.css web/static/*.js
# e2e-covers: web/static/*.png web/static/*.ico web/static/fonts/**
#   It renders pages and static assets, makes and deletes one collection of its own, and sets a THEME
#   cookie, which lives in the request and never on the account. It creates no clip and starts no job
#   ON THIS STACK. Since §20 erratum 285 step 77 also runs a THROWAWAY second instance of its own
#   (`INSTANCE_BADGE=dev`, `STORAGE_BACKEND=fs` into its own directory, its own database and scratch,
#   `dc run --rm`, everything inside the container's own loopback and writable layer) — the shape
#   88-fsstore and 89-quota already take. It publishes no port and names no bucket prefix, so another
#   bundle's probe running at the same moment shares nothing with it but the host (lane 4 used to run
#   these one after another; lane `speed2` found nothing they touch that ever needed it).
#   Nothing it does reaches the stack under test: not the database, not the store, not a clip.
#
# 77  the share page wears the system: the companion mark's favicon as a data: URI, one theme-color tag per
#     scheme, nothing fetched from anywhere but this origin (the stylesheet names no http(s)
#     URL and the faces are served from /static/fonts/ with `font-src 'self'` in the CSP);
#     og:site_name and the oEmbed provider_name are this instance's own public HOST (§20 erratum
#     287, superseding 241's "the wordmark's spelling" and 285's "clippet dev"); the card's bar
#     is ONE unqualified theme-color tag rendered LAST, after the browser's pair, on the pages
#     that carry a card and nowhere else; the embed overlay reads the host too.
# 78  the theme cookie is what the page handlers read: `theme=dark` renders data-theme="dark"
#     on <html> (the share page, the library, the embed) with ONE theme-color tag; `light` the
#     same; a value the control never writes renders nothing.
# 79  the Settings control round-trips without a client: POST /settings/theme sets the cookie
#     (SameSite=Lax, a year, Path=/) and bounces back to Settings, where the radio is ticked;
#     `system` deletes it; a bogus value is a 400; a session-less caller goes to /login.

step 77 "the share page wears Kept: favicon, the theme-color pair plus the card bar, self-hosted faces, og:site_name = the public host"
code=$(page "$BASE/$C"); expect "$code" 200 "GET /C"
has_file "$PAGE" 'rel="icon" href="data:image/svg+xml,' "the favicon is an inline data: URI"
has_file "$PAGE" 'prefers-color-scheme%3Adark' "the favicon SVG carries its own colour-scheme rule"
# The favicon is the companion mark ITSELF since §20 erratum 306 (the owner revoked 301's size rule): the
# unit geometry of docs/brand/mark.svg — the lid turned 9° at the hinge (0, 230) in font units, its
# three cuts holes in one even-odd path — at every size, the plain 16 px mitre deleted. The lid's
# transform is the one string neither the pp nor the old mitre carried; the pp's first curve is the
# one string the mark never does. internal/web's TestFaviconIsTheStripedMark decodes the whole thing.
has_file "$PAGE" 'rotate%28-9%200%20230%29' "the favicon is the companion mark's unit geometry (§20 erratum 306)"
lacks_file "$PAGE" 'rotate%28-9%201%204%29' "the favicon is still the plain 16 px mitre (deleted by §20 erratum 306)"
lacks_file "$PAGE" 'M718%2013Q637' "the favicon still carries the pp glyph (superseded by §20 erratum 301)"
has_file "$PAGE" '<meta name="theme-color" media="(prefers-color-scheme: light)" content="#ECEDEF">' "the light theme-color tag"
has_file "$PAGE" '<meta name="theme-color" media="(prefers-color-scheme: dark)" content="#191816">' "the dark theme-color tag"
has_file "$PAGE" "<meta property=\"og:site_name\" content=\"$SITE_HOST\">" "og:site_name is this instance's own public host (§20 erratum 287)"
lacks_file "$PAGE" '<meta property="og:site_name" content="clippet">' "the site line still carries the wordmark's spelling (241 is superseded)"
# THE CARD'S BAR (§20 erratum 287). Discord paints the rule down the left edge of an unfurl from
# the LAST theme-color tag on the page and ignores `media` entirely (MEASURED 2026-09-15, eight
# variants against a real channel); a browser takes the FIRST candidate whose media matches. So
# the card's tag must be unqualified AND last, and the pair above it must still be first — the
# ORDER is the whole mechanism, which is why this asserts the order and not just the presence.
ds_theme_tags() { grep -o '<meta name="theme-color"[^>]*>' "$1" | tr -d '\r'; }
DS_TC=$(ds_theme_tags "$PAGE")
expect "$(printf '%s\n' "$DS_TC" | wc -l | tr -d ' ')" 3 "the share page carries the browser's pair plus the card's one tag: $DS_TC"
expect "$(printf '%s\n' "$DS_TC" | tail -1)" '<meta name="theme-color" content="#0E7C4F">' \
  "the LAST theme-color tag is the ready card's green, unqualified (Discord takes the last; a browser never reaches it)"
expect "$(printf '%s\n' "$DS_TC" | head -1)" '<meta name="theme-color" media="(prefers-color-scheme: light)" content="#ECEDEF">' \
  "…and the browser's own pair is still FIRST"
# A stranger's page carries the wordmark ALONE, linking home (§20 erratum 244 — the owner's amendment of
# the no-CTA rule): the same lockup as the library's header, no nav, no "sign up", no text beside it.
#
# The assertion is the WHOLE header slot against its one literal, not a list of elements that must be
# absent: naming `<nav>`, `data-logout`, `/settings` and `Sign up` one at a time is an enumeration of
# what somebody thought of, and any future element added inside the slot would pass every one of them.
# `ds_header` cuts the block out of the rendered page; anything but the wordmark inside it is a diff.
#
# Since §20 erratum 288 the slot also ends with ONE control — the theme glyph, which changes how this
# browser paints and sells nothing — so the bar's end slot is here and empty. The glyph's own form is
# CUT OUT before the comparison, the way internal/web/badge_test.go cuts the badge out before its
# byte-for-byte one, precisely so that this keeps saying "anything ELSE in this slot is a diff": a nav,
# a sign-up line or a second control would all still fail it. Step 79 is where the glyph is pinned.
DS_HEADER_WANT='<header class="top">
  <a class="wordmark" href="/" aria-label="clippet">cli<span class="pp">pp</span>et</a>
  <div class="bar-end">
  </div>
</header>'
ds_header() { sed -n '/<header class="top">/,/<\/header>/p' "$1" | tr -d '\r' | sed '/<form class="theme-glyph"/,/<\/form>/d'; }
DS_HEADER=$(ds_header "$PAGE")
[ "$DS_HEADER" = "$DS_HEADER_WANT" ] || die "the anonymous share page's header slot is not the wordmark ALONE (§20 erratum 244).
      got:  $(printf '%s' "$DS_HEADER" | tr '\n' '|')
      want: $(printf '%s' "$DS_HEADER_WANT" | tr '\n' '|')"
# The embed carries the wordmark as ONE overlay and nothing else (§20 erratum 246 flipped 244's "an
# iframe is someone else's page"): linking to the share page, out of the iframe; no header, no nav.
code=$(page "$BASE/embed/$C"); expect "$code" 200 "GET /embed/C"
has_file "$PAGE" 'rotate%28-9%200%20230%29' "the embed carries the same striped-mark favicon (§20 erratum 306)"
lacks_file "$PAGE" 'M718%2013Q637' "the embed's favicon still carries the pp glyph"
# The overlay reads this instance's own HOST since §20 erratum 287 (the owner asked for the
# instance's host in the part baked into the video). On clippet.gg and any subdomain of it the host CONTAINS the
# wordmark, so the lockup is intact — a prefix, `cli<span class="pp">pp</span>et`, and `.gg`;
# anywhere else it is the host as plain text with no span. WHICH form this stack gets depends on
# its own origin, so the expected markup is BUILT from SITE_HOST rather than written down
# (no origin-dependent assertions: assert what the stack reports).
case $SITE_HOST in
  clippet.gg|*.clippet.gg)
    DS_MARK="class=\"wordmark embed-mark\" href=\"/$C\" target=\"_top\" rel=\"noopener\" aria-label=\"$SITE_HOST\">${SITE_HOST%clippet.gg}cli<span class=\"pp\">pp</span>et.gg</a>"
    DS_MARK_WHAT="the embed overlay is the wordmark lockup spelling $SITE_HOST" ;;
  *)
    DS_MARK="class=\"wordmark embed-mark\" href=\"/$C\" target=\"_top\" rel=\"noopener\">$SITE_HOST</a>"
    DS_MARK_WHAT="the embed overlay is the host $SITE_HOST as plain text (not this product's domain, so no span)" ;;
esac
has_file "$PAGE" "$DS_MARK" "$DS_MARK_WHAT"
# The embed page carries NO card, so it carries no card bar either — its one theme-color tag is
# the player's own black, which it has had since §9.2.
expect "$(ds_theme_tags "$PAGE" | wc -l | tr -d ' ')" 1 "the embed page carries more than its own theme-color tag"
lacks_file "$PAGE" '<header' "the embed carries no header"
lacks_file "$PAGE" '<nav' "the embed carries no nav"
code=$(page "$BASE/$C"); expect "$code" 200 "GET /C again"
lacks_file "$PAGE" 'fonts.googleapis' "the page names the font origin"
lacks_file "$PAGE" 'fonts.gstatic' "the page names the font file origin"
# No <link rel="stylesheet"> and no <script> may point off this origin. (The canonical and
# oEmbed links are absolute on PUBLIC_BASE_URL by design, so the grep is on those two rels.)
if grep -Eq '<link[^>]*rel="stylesheet"[^>]*href="https?://' "$PAGE"; then die "a stylesheet is fetched from an absolute URL"; fi
if grep -Eq '<script[^>]*src="https?://' "$PAGE"; then die "a script is fetched from an absolute URL"; fi
DS_CSS=$(sed -n 's/.*<link rel="stylesheet" href="\([^"]*\)".*/\1/p' "$PAGE" | head -1)
expect_nonempty "$DS_CSS" "the stylesheet link"
code=$(get "$BASE$DS_CSS"); expect "$code" 200 "GET the stylesheet"
lacks_file "$RESP" 'http://' "the stylesheet names an http URL"
lacks_file "$RESP" 'https://' "the stylesheet names an https URL"
has_file "$RESP" 'url("/static/fonts/spline-sans.woff2?v=' "the sans face is self-hosted"
has_file "$RESP" 'url("/static/fonts/spline-sans-mono.woff2?v=' "the mono face is self-hosted"
has_file "$RESP" 'font-display: swap' "the faces swap in"
for DS_F in spline-sans.woff2 spline-sans-mono.woff2 OFL.txt; do
  code=$(head_ "$BASE/static/fonts/$DS_F"); expect "$code" 200 "HEAD /static/fonts/$DS_F"
  DS_CC=$(hdr Cache-Control); contains "$DS_CC" 'max-age=3600' || die "fonts/$DS_F: Cache-Control is '$DS_CC', want the static policy"
done
code=$(head_ "$BASE/static/fonts/spline-sans.woff2"); expect "$(hdr Content-Type)" "font/woff2" "the woff2 media type"
DS_CSP=$(hdr Content-Security-Policy); contains "$DS_CSP" "font-src 'self'" || die "the CSP has no font-src 'self': $DS_CSP"
code=$(head_ "$BASE/static/fonts/nope.woff2"); expect "$code" 404 "an unknown font name is the 404 page"
# og:site_name on the collection card, provider_name in the oEmbed document.
code=$(http POST "$BASE/api/collections" -b "$JARW" -H 'Content-Type: application/json' -d '{"title":"E2E design"}')
expect "$code" 201 "create a collection for the card"
DS_COL=$(json id); expect_nonempty "$DS_COL" "collection id"
code=$(page "$BASE/c/$DS_COL"); expect "$code" 200 "GET /c/{id}"
has_file "$PAGE" "<meta property=\"og:site_name\" content=\"$SITE_HOST\">" "the collection card's site name is the public host"
# A collection is a list of several clips and has no state of its own to colour, so it carries the
# browser's pair ALONE — the card bar is the share page's (§20 erratum 287).
expect "$(ds_theme_tags "$PAGE" | wc -l | tr -d ' ')" 2 "the collection page carries a card bar; it has no single clip's state to paint"
DS_HEADER=$(ds_header "$PAGE")
[ "$DS_HEADER" = "$DS_HEADER_WANT" ] || die "the anonymous collection page's header slot is not the wordmark ALONE (§20 erratum 244).
      got:  $(printf '%s' "$DS_HEADER" | tr '\n' '|')
      want: $(printf '%s' "$DS_HEADER_WANT" | tr '\n' '|')"
code=$(http DELETE "$BASE/api/collections/$DS_COL" -b "$JARW"); expect "$code" 204 "delete the collection again"
code=$(get "$BASE/oembed?url=$BASE/$C&format=json"); expect "$code" 200 "GET /oembed"
expect "$(json provider_name)" "$SITE_HOST" "provider_name is this instance's own public host"
# The manifest icons are the companion mark on an ink tile, drawn by scripts/lib/mkicons.py; both still serve.
code=$(head_ "$BASE/static/icon-192.png"); expect "$code" 200 "icon-192"
code=$(head_ "$BASE/static/icon-512.png"); expect "$code" 200 "icon-512"
code=$(head_ "$BASE/static/icon-dev-192.png"); expect "$code" 200 "icon-dev-192 (a badged instance's card mark, erratum 316)"
code=$(get "$BASE/manifest.webmanifest"); expect "$code" 200 "the manifest"
has_file "$RESP" '"theme_color": "#ECEDEF"' "the manifest's theme colour is the light ground"
echo "[e2e]   favicon, the theme-color pair + the card bar last, self-hosted faces (font-src 'self'), og:site_name/provider_name = $SITE_HOST, the stranger's wordmark"

# ---------------------------------------------------------------------------------------------------
# The instance badge (§20 erratum 285; web/STYLE.md, DEPLOY §7.1): `INSTANCE_BADGE=dev` makes an
# instance say which one it is — grey span instead of green, one word beside the mark, the title
# prefix, `clippet dev` on the line Discord draws as the logo — and an UNSET badge changes nothing.
#
# Both halves are proven here, and they need two different instances, because a badge is read at boot:
#   (a) THIS stack, unbadged (every E2E stack is, and the reference stack is), carries none of it —
#       and its stylesheet, the one file both halves share, carries the badge's own rules as SERVED
#       (the block below names each of them, and the one spelling that must NOT be in the file);
#   (b) a throwaway `dc run --rm` container with the badge set carries all of it, and refuses two bad
#       values at boot by name. Variables are BD_-prefixed so nothing here can clobber a harness global.

# (a) the unbadged stack: not an attribute, not a tag, not a prefix, anywhere a mark appears.
for DS_P in "/" "/login" "/register" "/$C" "/embed/$C"; do
  code=$(page "$BASE$DS_P"); expect "$code" 200 "GET $DS_P (unbadged)"
  lacks_file "$PAGE" 'data-badge' "$DS_P carries data-badge on an instance with no INSTANCE_BADGE"
  lacks_file "$PAGE" 'class="instance"' "$DS_P carries the instance tag with no INSTANCE_BADGE"
  lacks_file "$PAGE" '<title>dev · ' "$DS_P carries a badge title prefix with no INSTANCE_BADGE"
done
code=$(get "$BASE/manifest.webmanifest"); expect "$code" 200 "the manifest (unbadged)"
has_file "$RESP" '"name": "Clippet"' "the manifest's name on an unbadged instance"
has_file "$RESP" '"short_name": "Clippet"' "the manifest's short name on an unbadged instance"
# The stylesheet the badged instance depends on. `has_file_f` (fixed strings) for 80-front's reason:
# these are CSS selectors, and a regex would read `.` as any character and `[` as a character class.
code=$(get "$BASE$DS_CSS"); expect "$code" 200 "GET the stylesheet for the badge rules"
has_file_f "$RESP" ':root[data-badge] .wordmark .pp::after { background: var(--line-strong); }' "the monochrome span rule"
# `:not(.sad)` in BOTH mouth rules is the fix for the one bug this feature could introduce: without it
# the badge's selector outweighs `.wordmark.sad .eyes .mouth` and a failed clip frowns GREY on a badged
# instance, repealing the colour law's one recorded exception (§20 erratum 285 (b)). `domBadge` proves
# the computed colour in a browser; this proves the rule is in the file the stack actually serves.
has_file_f "$RESP" ':root[data-badge] .wordmark:not(.sad) .eyes .mouth { stroke: var(--line-strong); }' "the monochrome mouth rule, excluding sulk"
has_file_f "$RESP" ':root[data-badge] .embed-mark .pp::after { background: #837F77; }' "the embed overlay's monochrome span rule"
has_file_f "$RESP" ':root[data-badge] .embed-mark:not(.sad) .eyes .mouth { stroke: #837F77; }' "the embed overlay's mouth takes the SAME grey (without it the page theme's grey leaks in)"
lacks_file "$RESP" 'embed-mark .pp::after { background: #514D47' "the embed's badge grey is still --line-strong, which is unreadable on the player's black"
has_file_f "$RESP" '.instance { font-family: var(--mono);' "the instance tag's own rule"
has_file_f "$RESP" '.lockup { display: grid; grid-template-columns: 1fr auto 1fr;' "the display lockup keeps the mark on the container's centre"
has_file_f "$RESP" '.lockup > .wordmark { grid-column: 2; }' "…with the mark in the middle column"
has_file_f "$RESP" '.wordmark.sad .eyes .mouth { stroke: var(--danger); }' "sulk stays red on every instance (the badge recolours no loss)"

# (b) the badged instance. Its own database, scratch and media directory, all under /tmp inside the
# container; `--rm` removes it on success, on failure and on an interrupt. The in-container script is
# WRITTEN TO A FILE and then handed to `sh -c` as one argument (89-quota's note, and the same shape):
# the heredoc is quoted, so nothing in it is expanded by the harness's shell, and the file under
# .e2e/ is what a post-mortem reads. A long script typed inline instead is where a quoting mistake
# is hardest to see.
BD_OUT="$E2E_DIR/badge-probe.txt"
BD_SH="$E2E_DIR/badge-probe.sh"
cat > "$BD_SH" <<'BADGE_PROBE'
set -e
# compose interpolates the bucket, Discord and media variables for every container of this project.
# The bucket four and the media three because this probe runs on STORAGE_BACKEND=fs, where a leftover
# S3_* is a boot error and `redirect` is a boot error (88-fsstore's note, DESIGN 20 erratum 278(e));
# the Discord five because this probe UPLOADS, and an instance that inherited the stack's webhook
# would announce a throwaway clip in somebody's real channel.
unset S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY
unset MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT
unset DISCORD_WEBHOOK_URL DISCORD_WEBHOOK_ROUTES DISCORD_APP_ID DISCORD_PUBLIC_KEY DISCORD_BOT_TOKEN
mkdir -p /tmp/bdata /tmp/bscratch /tmp/bmedia
export DATA_DIR=/tmp/bdata SCRATCH_DIR=/tmp/bscratch STORAGE_DIR=/tmp/bmedia STORAGE_BACKEND=fs
export PUBLIC_BASE_URL=http://127.0.0.1:8080 REGISTRATION=open SWEEP_INTERVAL=5s CAPTURE_READY_WAIT=0

# --- the boot gates, before anything else runs -----------------------------------------------------
# A word a person reads: lowercase letters, digits and `-`, at most twelve. Anything else is fatal at
# boot naming the variable and the rule -- never a silent lowercasing, never a silent trim.
BE=$(INSTANCE_BADGE=Dev clippet migrate 2>&1 || true)
echo "badupper=$(printf "%s" "$BE" | tr "\n" " ")"
BE=$(INSTANCE_BADGE=aaaaaaaaaaaaaaaaaaaa clippet migrate 2>&1 || true)
echo "badlong=$(printf "%s" "$BE" | tr "\n" " ")"

export INSTANCE_BADGE=dev
clippet migrate >/tmp/migrate.log 2>&1
echo "migrated=$(tail -1 /tmp/migrate.log | tr -d "\r")"
clippet serve >/tmp/serve.log 2>&1 &
BSRV=$!
i=0
while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
echo "ready=$(curl -sS -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/healthz)"
# The badge is for an EYE: it must not have grown a fact on the anonymous surface.
curl -sS -o /tmp/health http://127.0.0.1:8080/healthz
echo "healthbadge=$(grep -c "badge" /tmp/health | tr -d " ")"

bd_page() { curl -sS -o "$2" -w "%{http_code}" "http://127.0.0.1:8080$1"; }   # bd_page <path> <file> -> code
echo "front=$(bd_page / /tmp/front)"
echo "login=$(bd_page /login /tmp/login)"
echo "manifest=$(bd_page /manifest.webmanifest /tmp/manifest)"

# One clip, made here (ffmpeg is in this image and the host's testclips/ is unreachable from a
# `compose run` container). It is never waited on: a card's site line and an oEmbed provider are the
# INSTANCE's, not a clip's state, so a queued clip proves them exactly as a ready one would.
ffmpeg -v error -y -f lavfi -i testsrc2=size=320x180:rate=15:duration=2 \
  -c:v libx264 -preset veryfast -pix_fmt yuv420p /tmp/b.mp4
curl -sS -o /tmp/reg -c /tmp/jar -w "%{http_code}" -X POST -H "Content-Type: application/json" \
  -d "{\"username\":\"$BUSER\",\"password\":\"$BPASS\"}" http://127.0.0.1:8080/api/auth/register > /tmp/regcode
echo "register=$(tr -d "\r\n" < /tmp/regcode)"
curl -sS -o /tmp/up -w "%{http_code}" -b /tmp/jar \
  -F "title=Badge probe" -F "ttl_days=1" -F duplicate=allow -F "file=@/tmp/b.mp4;type=video/mp4" \
  http://127.0.0.1:8080/api/upload > /tmp/upcode
echo "upload=$(tr -d "\r\n" < /tmp/upcode)"
BID=$(grep -o "\"id\":\"[^\"]*\"" /tmp/up | head -1 | cut -d"\"" -f4)
echo "id=$BID"
echo "share=$(bd_page /$BID /tmp/share)"
echo "embed=$(bd_page /embed/$BID /tmp/embed)"
echo "oemb=$(bd_page "/oembed?format=json&url=http%3A%2F%2F127.0.0.1%3A8080%2F$BID" /tmp/oembed)"

# One `key=value` line per fact, so the host asserts strings and never a grep exit status.
bd_say() { echo "$1=$(grep -cF -- "$3" "$2" | tr -d " ")"; }   # bd_say <key> <file> <fixed string>
bd_say htmlattr  /tmp/front '<html lang="en" data-badge="dev">'
bd_say fronttag  /tmp/front '<span class="instance">dev</span></span>'
bd_say frontlock /tmp/front '<span class="lockup">'
bd_say fronttitle /tmp/front '<title>dev · Clippet</title>'
bd_say logintag  /tmp/login '<span class="instance">dev</span>'
bd_say logintitle /tmp/login '<title>dev · Log in · Clippet</title>'
bd_say sharehdr  /tmp/share '<span class="instance">dev</span>'
bd_say sharesite /tmp/share '<meta property="og:site_name" content="127.0.0.1">'
bd_say sharebadged /tmp/share '<meta property="og:site_name" content="clippet dev">'
bd_say shareplain /tmp/share '<meta property="og:site_name" content="clippet">'
bd_say embedattr /tmp/embed 'data-badge="dev"'
bd_say embedtag  /tmp/embed 'class="instance"'
bd_say embedlock /tmp/embed 'class="lockup"'
bd_say embedmark /tmp/embed 'class="wordmark embed-mark"'
bd_say mfname    /tmp/manifest '"name": "Clippet dev"'
bd_say mfshort   /tmp/manifest '"short_name": "Clippet dev"'
echo "provider=$(grep -o "\"provider_name\":\"[^\"]*\"" /tmp/oembed | head -1 | cut -d"\"" -f4)"
# The header slot of a stranger's share page: the wordmark, then the word, and nothing else. The
# theme glyph's form is cut out (§20 erratum 288), exactly as the unbadged assertion above cuts it.
echo "hdr=$(sed -n "/<header class=\"top\">/,/<\/header>/p" /tmp/share | tr -d "\r" | sed "/<form class=\"theme-glyph\"/,/<\/form>/d" | tr "\n" "|")"

kill $BSRV 2>/dev/null || true
wait $BSRV 2>/dev/null || true
echo "logerrors=$(grep -c -E "panic|level=ERROR" /tmp/serve.log | tr -d " ")"
echo "done=1"
BADGE_PROBE

BD_RC=0
dc run --rm -T --no-deps \
  -e DATA_DIR=/tmp/bdata -e SCRATCH_DIR=/tmp/bscratch \
  -e "BUSER=${E2E_USER}_b" -e "BPASS=$E2E_PASS" \
  --entrypoint sh app -c "$(cat "$BD_SH")" > "$BD_OUT" 2>&1 || BD_RC=$?
sed -n 's/^/[e2e]   badge-probe: /p' "$BD_OUT" | head -40
[ "$BD_RC" = 0 ] || die "the badge probe container failed (exit $BD_RC):
$(cat "$BD_OUT")"
has_file "$BD_OUT" '^done=1' "the badge probe did not run to the end"
bd_get() { sed -n "s/^$1=//p" "$BD_OUT" | head -1; }

# The two boot refusals, by name and by rule.
DS_V=$(bd_get badupper)
contains "$DS_V" 'INSTANCE_BADGE' || die "INSTANCE_BADGE=Dev was not refused by name: $DS_V"
contains "$DS_V" 'lowercase' || die "…and did not state the charset rule: $DS_V"
DS_V=$(bd_get badlong)
contains "$DS_V" 'INSTANCE_BADGE' || die "a twenty-character INSTANCE_BADGE was not refused by name: $DS_V"
contains "$DS_V" 'at most 12' || die "…and did not state the length: $DS_V"

expect "$(bd_get ready)" 200 "the badged probe instance never became healthy"
expect "$(bd_get healthbadge)" 0 "/healthz grew a badge fact (the anonymous surface must not move)"
expect "$(bd_get register)" 201 "register on the badged probe instance"
expect "$(bd_get upload)" 201 "upload to the badged probe instance"
expect_nonempty "$(bd_get id)" "the badged probe's clip id"
# `oemb` is the oEmbed document's key here and not `oembed`, because 55-owner.sh defines an `oembed()`
# helper and check.sh's cross-drop-in scan reads a bare word at a command position as a call to it.
for DS_K in front login manifest share embed oemb; do
  expect "$(bd_get $DS_K)" 200 "the badged probe instance's '$DS_K' page"
done
# The attribute, the tag and the title, on every mark.
expect "$(bd_get htmlattr)" 1 "the front door's <html> does not carry data-badge=\"dev\""
expect "$(bd_get fronttag)" 1 "the front door does not carry the instance tag"
# The lockup (§20 erratum 285 (c)) is what keeps the mark on the container's centre while the tag hangs
# off its side, and it exists ONLY on a badged page — the embed never gets one, having no tag to hold.
expect "$(bd_get frontlock)" 1 "the front door's display mark is not inside a lockup"
expect "$(bd_get fronttitle)" 1 "the front door's title is not prefixed with the badge"
expect "$(bd_get logintag)" 1 "the login page does not carry the instance tag"
expect "$(bd_get logintitle)" 1 "the login page's title is not prefixed with the badge"
expect "$(bd_get sharehdr)" 1 "a stranger's share page does not carry the instance tag"
# The card's site line is the instance's own HOST, and the badge does NOT reach it (§20 erratum 287
# supersedes 285's one card tag): the probe's PUBLIC_BASE_URL is http://127.0.0.1:8080, so its
# cards say `127.0.0.1` — no scheme, no port — on a badged instance exactly as on an unbadged one.
# `dev.clippet.gg` is what tells a dev card from a production one now, and it needs no variable.
expect "$(bd_get sharesite)" 1 "og:site_name is not the probe instance's own host"
expect "$(bd_get sharebadged)" 0 "og:site_name still carries the badge (285's site line is superseded by 287)"
expect "$(bd_get shareplain)" 0 "og:site_name still carries the wordmark's spelling (241 is superseded by 287)"
expect "$(bd_get provider)" "127.0.0.1" "the oEmbed provider_name is the probe instance's own host"
# The embed: the attribute and NOT the tag. The overlay is 16 px of live text over a picture on
# somebody else's page, and the grey span the attribute produces is its whole signal (erratum 246).
expect "$(bd_get embedattr)" 1 "the embed's <html> does not carry data-badge"
expect "$(bd_get embedtag)" 0 "the embed carries the instance tag, and it must never"
expect "$(bd_get embedlock)" 0 "the embed carries a lockup, and it has no tag to hold"
expect "$(bd_get embedmark)" 1 "the embed lost its wordmark overlay"
expect "$(bd_get mfname)" 1 "the manifest's name is not 'Clippet dev' on a badged instance"
expect "$(bd_get mfshort)" 1 "the manifest's short_name is not 'Clippet dev' on a badged instance"
# The whole header slot, against its one literal: the wordmark, the word, the bar's (empty) end slot
# and nothing else. An enumeration of absent elements would pass for anything somebody later added
# inside the slot; the glyph's own form is cut out before the comparison (§20 erratum 288), so a NAV,
# a sign-up line or a second control beside it would each still fail this.
DS_BADGE_HDR_WANT='<header class="top">|  <a class="wordmark" href="/" aria-label="clippet">cli<span class="pp">pp</span>et</a>|  <span class="instance">dev</span>|  <div class="bar-end">|  </div>|</header>|'
[ "$(bd_get hdr)" = "$DS_BADGE_HDR_WANT" ] || die "a badged stranger's header slot is not the wordmark plus the one word (§20 erratum 285).
      got:  $(bd_get hdr)
      want: $DS_BADGE_HDR_WANT"
if [ "$(bd_get logerrors)" != 0 ]; then
  echo "[e2e]   NOTE: the badge probe's own app log has $(bd_get logerrors) ERROR/panic line(s) — see $BD_OUT"
fi
echo "[e2e]   badge (285, amended by 287): this stack is unbadged and says nothing; a probe on INSTANCE_BADGE=dev wears data-badge, the tag, 'dev · ' titles, 'Clippet dev' in the manifest and the attribute ALONE on the embed — and its card's site line is its own HOST, not 'clippet dev'; Dev and a 20-character word refused at boot"

step 78 "the theme cookie renders data-theme on <html>: share page, library, embed"
code=$(page "$BASE/$C" -b "theme=dark"); expect "$code" 200 "GET /C with theme=dark"
has_file "$PAGE" '<html lang="en" data-theme="dark">' "the share page pins dark"
has_file "$PAGE" '<meta name="theme-color" content="#191816">' "the browser's one theme-color tag, the dark one"
lacks_file "$PAGE" 'prefers-color-scheme: light' "the system pair is not rendered when the cookie decided"
# …and the card's bar still comes AFTER it (§20 erratum 287): in this branch the browser's single
# tag is first and unqualified too, which is exactly why the ORDER and not the media attribute is
# what the mechanism rests on.
expect "$(ds_theme_tags "$PAGE" | wc -l | tr -d ' ')" 2 "a cookie-pinned share page: the browser's one tag plus the card's"
expect "$(ds_theme_tags "$PAGE" | tail -1)" '<meta name="theme-color" content="#0E7C4F">' "the card's bar is still last on a cookie-pinned page"
code=$(page "$BASE/$C" -b "theme=light"); expect "$code" 200 "GET /C with theme=light"
has_file "$PAGE" '<html lang="en" data-theme="light">' "the share page pins light"
has_file "$PAGE" '<meta name="theme-color" content="#ECEDEF">' "one theme-color tag, the light one"
code=$(page "$BASE/$C" -b "theme=purple"); expect "$code" 200 "GET /C with a value the control never writes"
has_file "$PAGE" '<html lang="en">' "an unknown value renders no data-theme at all"
lacks_file "$PAGE" 'data-theme=' "…nothing"
code=$(page "$BASE/$C"); expect "$code" 200 "GET /C with no cookie"
lacks_file "$PAGE" 'data-theme=' "no cookie, no attribute: the system decides"
code=$(page "$BASE/" -b "$JARW" -b "theme=dark"); expect "$code" 200 "GET / with theme=dark"
has_file "$PAGE" '<html lang="en" data-theme="dark">' "the library pins dark"
has_file "$PAGE" 'class="wordmark" href="/" aria-label="clippet">cli<span class="pp">pp</span>et</a>' "the header carries the wordmark, live text, the pp span"
lacks_file "$PAGE" 'style=' "the library page carries no inline style"
code=$(page "$BASE/embed/$C" -b "theme=dark"); expect "$code" 200 "GET /embed/C with theme=dark"
has_file "$PAGE" '<html lang="en" data-theme="dark">' "the embed pins dark too"
echo "[e2e]   theme=dark|light → data-theme on <html> + one theme-color; anything else → the system"

step 79 "the Settings theme control round-trips without a client: POST /settings/theme"
code=$(page "$BASE/settings" -b "$JARW"); expect "$code" 200 "GET /settings"
has_file "$PAGE" 'data-theme-form action="/settings/theme" method="post"' "the theme form"
has_file "$PAGE" 'value="system" checked data-theme-pick' "System is ticked with no cookie"
lacks_file "$PAGE" 'value="dark" checked' "Dark is not ticked with no cookie"
code=$(http POST "$BASE/settings/theme" -b "$JARW" --data-urlencode theme=dark); expect "$code" 303 "POST theme=dark"
expect "$(hdr Location)" "/settings#theme" "…bounces back to Settings"
DS_SC=$(hdr Set-Cookie); expect_nonempty "$DS_SC" "the theme Set-Cookie"
contains "$DS_SC" 'theme=dark' || die "Set-Cookie does not carry theme=dark: $DS_SC"
contains "$DS_SC" 'Path=/' || die "Set-Cookie is not site-wide: $DS_SC"
contains "$DS_SC" 'Max-Age=31536000' || die "Set-Cookie is not a year: $DS_SC"
contains "$DS_SC" 'SameSite=Lax' || die "Set-Cookie is not SameSite=Lax: $DS_SC"
if [ "$SECURE" = 1 ]; then contains "$DS_SC" 'Secure' || die "Set-Cookie lacks Secure on an https base: $DS_SC"; fi
if contains "$DS_SC" 'HttpOnly'; then die "the theme cookie must not be HttpOnly: the client writes it too"; fi
code=$(page "$BASE/settings" -b "$JARW" -b "theme=dark"); expect "$code" 200 "GET /settings with the cookie"
has_file "$PAGE" '<html lang="en" data-theme="dark">' "Settings pins dark"
has_file "$PAGE" 'value="dark" checked data-theme-pick' "Dark is ticked"
lacks_file "$PAGE" 'value="system" checked' "System is no longer ticked"
code=$(http POST "$BASE/settings/theme" -b "$JARW" --data-urlencode theme=system); expect "$code" 303 "POST theme=system"
DS_SC=$(hdr Set-Cookie); contains "$DS_SC" 'Max-Age=0' || die "theme=system must delete the cookie: $DS_SC"
code=$(http_refused POST "$BASE/settings/theme" -b "$JARW" --data-urlencode theme=neon); expect "$code" 400 "a value the control never writes is refused"
code=$(get "$BASE/settings/theme" -b "$JARW"); expect "$code" 405 "GET /settings/theme is not a page"
# The TOP BAR's half of the same control (§20 erratum 288, the owner's ask for a light/dark
# glyph in the top bar): one form, two buttons, and the stylesheet draws exactly one of them — so
# the markup a person gets is the same in all three theme states and only CSS differs, which is what
# lets the server render the bar without knowing what the visitor's operating system prefers.
code=$(page "$BASE/" -b "$JARW"); expect "$code" 200 "GET / signed in, for the glyph"
has_file "$PAGE" '<form class="theme-glyph" action="/settings/theme" method="post" data-theme-glyph>' "the bar's theme form"
has_file "$PAGE" 'name="theme" value="light" data-theme-to="light" aria-label="Light theme" title="Light theme"' "the sun button, naming the theme it gives you"
has_file "$PAGE" 'name="theme" value="dark" data-theme-to="dark" aria-label="Dark theme" title="Dark theme"' "the moon button, naming the theme it gives you"
has_file "$PAGE" '<input type="hidden" name="next" value="/">' "…and it comes back to the page it was pressed on"
# It is on a STRANGER's page too — which is the whole reason the route below answers with no session —
# and erratum 244's rule for that page is unchanged: the wordmark alone, no nav.
code=$(page "$BASE/$C"); expect "$code" 200 "GET /C as a stranger, for the glyph"
has_file "$PAGE" 'data-theme-to="dark"' "a stranger's share page carries the glyph"
has_file "$PAGE" '<input type="hidden" name="next" value="/'"$C"'">' "…pointing back at the clip being watched"
lacks_file "$PAGE" '<nav>' "a stranger's page still has no nav (erratum 244)"
code=$(page "$BASE/login"); expect "$code" 200 "GET /login, for the glyph"
lacks_file "$PAGE" 'data-theme-to' "a page with no header bar carries no glyph"
# …and the three-state CSS that decides which button exists, as SERVED. Fixed strings: these are
# selectors, and a regex would read `.` as any character and `[` as a character class.
code=$(get "$BASE$DS_CSS"); expect "$code" 200 "GET the stylesheet for the glyph rules"
has_file_f "$RESP" '.theme-glyph [data-theme-to="light"] { display: none; }' "light is the bare rule: the moon is drawn"
has_file_f "$RESP" ':root:not([data-theme="light"]) .theme-glyph [data-theme-to="light"] { display: inline-flex; }' "a dark SYSTEM draws the sun, unless the cookie pinned light"
has_file_f "$RESP" ':root[data-theme="dark"] .theme-glyph [data-theme-to="light"] { display: inline-flex; }' "a dark COOKIE draws the sun"
has_file_f "$RESP" ':root[data-theme="dark"] .theme-glyph [data-theme-to="dark"] { display: none; }' "…and never both at once"
# The anonymous POST. Nobody is signed in on a stranger's share page, and the route writes one cookie
# about one browser and reads no row, so it answers rather than bouncing to /login.
# `--data` with the slashes PRE-ENCODED, not `--data-urlencode next=/…`: on Git Bash the MSYS runtime
# rewrites a `name=/path` argument into a Windows path before curl ever sees it (`next=/abc` arrives as
# `next=C:/Program Files/Git/abc`), which this probe caught the first time it ran. The ids and the
# hostname below carry nothing else that needs encoding.
code=$(http POST "$BASE/settings/theme" --data-urlencode theme=dark --data "next=%2F$C"); expect "$code" 303 "session-less POST"
expect "$(hdr Location)" "/$C" "…comes back to the page it was pressed on"
DS_SC=$(hdr Set-Cookie); contains "$DS_SC" 'theme=dark' || die "the session-less POST set no theme cookie: $DS_SC"
contains "$DS_SC" 'SameSite=Lax' || die "the session-less Set-Cookie is not SameSite=Lax: $DS_SC"
code=$(http POST "$BASE/settings/theme" --data-urlencode theme=light --data "next=%2F%2Fevil.example%2F"); expect "$code" 303 "a next that is not a path on this origin"
expect "$(hdr Location)" "/" "…is the root, never an authority"
code=$(http_refused POST "$BASE/settings/theme" --data-urlencode theme=neon); expect "$code" 400 "a bad value is still refused with no session"
echo "[e2e]   POST /settings/theme: cookie set (Lax, a year), deleted on system, 400 on nonsense, ANONYMOUS (the bar's glyph), back to the page that posted it; the glyph's two buttons and their three-state CSS"
