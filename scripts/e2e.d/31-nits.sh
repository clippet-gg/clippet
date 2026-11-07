# e2e.d/31-nits.sh — bundle A (the deep-audit nits), §20 errata 99–104.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=CFG_,ONE_,WANT_ buckets=-
# e2e-covers: internal/httpapi/media.go internal/httpapi/view.go internal/views/** internal/web/meta.go
# e2e-covers: internal/transcode/social.go internal/worker/social.go cmd/clippet/main.go internal/config/**
#   It reads C (pages, media headers, its own DTO) and creates nothing. Its one mutation of shared state
#   is the three view beacons in (d), and those assert a DELTA of at most one on a counter no other
#   bundle posts to. `clippet migrate` with a deliberately bad config exits before it opens a database.
#
# Sourced by scripts/e2e.sh before the step-16 log gate, so every line these steps make is covered by it.
# It uses the shared clip C -- uploaded at step 4, ready and owned by the session in $JARW for the whole
# pool, never trimmed or flipped by anything beside it since run 9's lane `pool` (21-trim and 20-pages work
# on clips of their own), and deleted by step 17 -- and creates nothing of its own, so there is nothing
# here for step 17 to clean up that it does not already clean.
#
# Both steps must pass over plain http AND over https: nothing below asserts a scheme, and the one
# mixed-content check is guarded by SECURE exactly as step 10's is.

step 31 "the crawler's OTHER verbs on social.mp4 (§20 erratum 103): a ranged and a conditional Discordbot GET, the added card tags, the 412, and a view counter a User-Agent cannot inflate"
# --- (a) the card tags erratum 103 added, on the clip a bot would actually fetch ------------------------
# The three tags themselves — og:video:url, video:duration and og:image:alt — are asserted by VALUE in
# `card_check`'s player branch (scripts/e2e.sh), which runs on every ready player card in the suite
# rather than on this one clip. What stays here is the cross-check a flat expect map cannot express:
# og:video and og:video:url are the SAME OGP property, so a card that disagreed with itself would be
# worse than one that carried a single tag — and C's card is not one of the ones card_check reads.
code=$(page "$BASE/$C" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot share page for C"
# og:video:url, video:duration and og:image:alt are asserted by card_check on every player card (nit N002);
# twitter:image:alt is the one tag card_check learned only after both lanes merged, so it is asserted here too.
has_file "$PAGE" 'name="twitter:image:alt"' "twitter:image:alt (X's spelling of the same sentence, §20 erratum 177)"
$PY - "$(W "$PAGE")" <<'PYEOF' || die "the added card tags are inconsistent"
import html.parser, sys
m = {}
class P(html.parser.HTMLParser):
    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        k = a.get('property') or a.get('name')
        if tag == 'meta' and k and k not in m:
            m[k] = a.get('content', '')
P().feed(open(sys.argv[1], encoding='utf-8').read())
bad = []
if not m.get('og:video'):
    bad.append('og:video is missing')
if m.get('og:video') != m.get('og:video:url'):
    bad.append('og:video=%r but og:video:url=%r' % (m.get('og:video'), m.get('og:video:url')))
if 'E2E vertical' not in m.get('og:image:alt', ''):
    bad.append('og:image:alt=%r does not name the clip' % m.get('og:image:alt'))
if m.get('twitter:image:alt') != m.get('og:image:alt'):
    bad.append('twitter:image:alt=%r but og:image:alt=%r' % (m.get('twitter:image:alt'), m.get('og:image:alt')))
if bad:
    sys.stderr.write('card tags:\n  ' + '\n  '.join(bad) + '\n'); sys.exit(1)
print('[e2e]   og:video:url == og:video, video:duration=%s, image alt (both cards)=%r' % (m['video:duration'], m['og:image:alt']))
PYEOF
# A real icon and a theme-color, on the share page and on the embed (which used to make the browser GET
# /favicon.ico and receive the whole HTML 404 page). The icon is a data: URI, so this adds no request and
# no outbound anything, and it is percent-encoded in full — which is also why the https mixed-content
# assertion below is not upset by the SVG namespace.
has_file "$PAGE" 'rel="icon" href="data:image/svg+xml,' "share page favicon"
has_file "$PAGE" 'name="theme-color"' "share page theme-color"
lacks_file "$PAGE" 'href="data:,"' "the old blank favicon"
lacks_file "$PAGE" 'style=' "share page inline style attribute (the icon must not have introduced one)"
lacks_file "$PAGE" '<style' "share page style element"
code=$(page "$BASE/embed/$C"); expect "$code" 200 "embed for C"
has_file "$PAGE" 'rel="icon" href="data:image/svg+xml,' "embed favicon"
lacks_file "$PAGE" 'style=' "embed inline style attribute"
[ "$SECURE" = 0 ] || lacks_file "$PAGE" 'http://' "embed plain-http URL after the icon was added"

# --- (b) the crawler sweep's missing verbs: a RANGED and a CONDITIONAL GET of social.mp4 ---------------
# Step 10 fetches every card URL as a plain GET and step 8 runs the whole Range matrix against video.mp4
# with a default UA, so the one handler branch a dedicated social rendition will replace had the least
# coverage. Everything below is asserted against social.mp4's OWN validators — never against video.mp4's —
# so it keeps holding the day social becomes a separate object.
# Since §20 erratum 280 this runs on BOTH delivery shapes. The verbs a CRAWLER uses — a ranged GET and
# a conditional GET — are what the card's file must answer, and in `redirect` mode the answer comes
# from the bucket one hop along, which is exactly what Discord's media proxy was measured doing
# (erratum 278(g)). The helpers carry the Range and the If-None-Match through that hop, so the
# assertions below are the same assertions. The two that are about the APP's own error ENVELOPE — its
# 416's Content-Range and the 412 of section (c) — describe a response a 302 replaces entirely and
# stand down with a printed line; step 90 proves both, in both modes, on an instance of its own.
code=$(media_head "$BASE/api/clips/$C/social.mp4" -A "$DISCORD_UA"); expect_media "$code" 200 "Discordbot HEAD social.mp4"
SOCIAL_ETAG=$(hdr ETag); expect_nonempty "$SOCIAL_ETAG" "social.mp4 ETag"
SOCIAL_LEN=$(hdr Content-Length); expect_nonempty "$SOCIAL_LEN" "social.mp4 Content-Length"
if [ "$E2E_MEDIA" = proxy ]; then
  expect "$(hdr Accept-Ranges)" bytes "social.mp4 Accept-Ranges"
else
  # NOT a stand-down since §20 erratum 283 (g): `media_head` asked the BUCKET for one byte of this
  # object and asserted `Accept-Ranges: bytes` on that answer. A crawler's ranged GET is what this
  # whole section is about, and in `redirect` it is the bucket that has to admit it accepts one.
  echo "[e2e]   (redirect: Accept-Ranges was asserted on the BUCKET's own answer, one hop along)"
fi
code=$(media_get "$BASE/api/clips/$C/social.mp4" -A "$DISCORD_UA" -H 'Range: bytes=0-1023')
expect "$code" 206 "Discordbot ranged GET social.mp4"
expect "$(hdr Content-Range)" "bytes 0-1023/$SOCIAL_LEN" "social.mp4 206 Content-Range"
expect "$(hdr Content-Length)" 1024 "social.mp4 206 body length"
expect "$(hdr ETag)" "$SOCIAL_ETAG" "social.mp4 206 keeps its own ETag"
expect "$(wc -c < "$RESP" | tr -d ' ')" 1024 "social.mp4 206 bytes on the wire"
: > "$RESP"   # curl leaves a -o file untouched when the response has no body, and the 206 above filled it
code=$(media_get "$BASE/api/clips/$C/social.mp4" -A "$DISCORD_UA" -H "If-None-Match: $SOCIAL_ETAG")
expect "$code" 304 "Discordbot conditional GET social.mp4"
expect "$(hdr ETag)" "$SOCIAL_ETAG" "social.mp4 304 keeps its own ETag"
expect "$(wc -c < "$RESP" | tr -d ' ')" 0 "social.mp4 304 has no body"
if [ "$E2E_MEDIA" = proxy ]; then
  expect_empty "$(hdr Content-Length)" "social.mp4 304 Content-Length"
else
  media_skip "the headers a 304 does and does not carry"
fi
# A range past the end is still the considered 416 (erratum 85), through this route too.
code=$(media_get "$BASE/api/clips/$C/social.mp4" -A "$DISCORD_UA" -H 'Range: bytes=99999999-')
expect "$code" 416 "social.mp4 unsatisfiable range"
if [ "$E2E_MEDIA" = proxy ]; then
  expect "$(hdr Content-Range)" "bytes */$SOCIAL_LEN" "social.mp4 416 Content-Range"
else
  # The app's ENVELOPE stands down; the REFUSAL is the bucket's and is asserted (§20 erratum 283 (g)).
  # `Content-Range` on a 416 is a SHOULD in RFC 7233 and not every S3 implementation sends one, so what
  # is checked is the half that is not optional: the refusal carried none of the card's bytes, and it
  # came from the media origin the app signed for. A store that answered a crawler's past-the-end range
  # with the whole file would put the object on the wire to Discord in place of an error.
  [ "$(wc -c < "$RESP" | tr -d ' ')" -lt "$SOCIAL_LEN" ] || die "the bucket answered an unsatisfiable range on social.mp4 with the whole object ($(wc -c < "$RESP" | tr -d ' ') bytes of $SOCIAL_LEN)"
  expect "$(media_origin_ "$MEDIA_LOC")" "$E2E_MEDIA_ORIGIN" "social.mp4's 416 was the BUCKET's own refusal"
  media_skip "the 416 ENVELOPE"
fi

# --- (c) the 412 is an error, not the object (§20 erratum 103 extends erratum 85's doctrine) -----------
# `If-Match` is a CONDITIONAL, and erratum 278(g) says a 302 does not look at one: the app hands back
# the same signed link and the bucket, which has never seen an ETag of ours, answers the object. The
# app's 412 is therefore unreachable through this route in `redirect` mode — not broken, replaced —
# and step 90's own instance is where it is proven in both. `/original` below is NEVER redirected in
# any mode (erratum 278(a)), so the half of this section with teeth runs unchanged on every stack.
if [ "$E2E_MEDIA" = proxy ]; then
code=$(get "$BASE/api/clips/$C/social.mp4" -H 'If-Match: "nope"'); expect "$code" 412 "If-Match miss on social.mp4"
expect "$(json error)" precondition_failed "412 error envelope"
expect_empty "$(hdr ETag)" "the 412 must not carry the object's ETag"
expect_empty "$(hdr Content-Disposition)" "the 412 must not carry a Content-Disposition"
expect_empty "$(hdr Content-Range)" "a 412 is not a range error"
expect "$(hdr Cache-Control)" no-store "the 412 must not be storable"
else
media_skip "the 412 on social.mp4 (a 302 looks at no conditional header)"
fi
# …and on the route where it has teeth: /original is served `attachment`, so a download client that hit a
# 412 would otherwise write the JSON envelope to disk under the clip's own filename.
code=$(get "$BASE/api/clips/$C/original" -b "$JARW" -H 'If-Match: "nope"'); expect "$code" 412 "If-Match miss on /original"
expect_empty "$(hdr Content-Disposition)" "the /original 412 must not name the clip's file"
expect_empty "$(hdr ETag)" "the /original 412 must not carry the object's ETag"
# The happy path still does carry them, or the three assertions above prove nothing.
code=$(head_ "$BASE/api/clips/$C/original" -b "$JARW"); expect "$code" 200 "owner HEAD /original"
contains "$(hdr Content-Disposition)" 'attachment' || die "the 200 on /original lost its attachment disposition"
expect_nonempty "$(hdr ETag)" "the 200 on /original carries an ETag"

# --- (d) the view counter is bounded by ADDRESSES, not by request rate (§20 erratum 103) --------------
# Three POSTs from this one address with three different User-Agents used to count THREE views, which let
# any link holder run the owner's private counter up at the view limiter's 60/min. Only step 13 posts a
# beacon at all, and it posts to A, so this clip starts at zero on a clean stack -- but the assertion is
# on the DELTA, so a re-run against a warm stack proves exactly the same thing.
code=$(get "$BASE/api/clips/$C" -b "$JARW"); expect "$code" 200 "owner GET the clip before the beacons"
V0=$(json views); expect_nonempty "$V0" "views before the beacons"
for agent in "player/1" "player/2" "$DISCORD_UA"; do
  code=$(http POST "$BASE/api/clips/$C/view" -A "$agent"); expect "$code" 204 "view beacon as $agent"
done
code=$(get "$BASE/api/clips/$C" -b "$JARW"); expect "$code" 200 "owner GET the clip after the beacons"
# The DELTA is what this proves, and it is at most one: this address may already have been counted for
# the clip inside the 30-minute window, in which case the honest answer is zero. What must never happen
# is three.
expect "$(json views)" "$((V0 + 1))" "three User-Agents from one address are ONE view (was $V0)"
code=$(get "$BASE/api/clips/$C"); expect_empty "$(json views)" "the view count stays absent for everyone but the owner"
if [ "$E2E_MEDIA" = proxy ]; then
  echo "[e2e]   social.mp4 answers 206 + 304 to Discordbot on its own ETag; the 412 drops the object headers; 3 UAs = 1 view"
else
  echo "[e2e]   social.mp4 302s to $E2E_MEDIA_ORIGIN and answers 206 + 304 there on its own ETag; the app's 412 is step 90's; 3 UAs = 1 view"
fi

step 32 "the build says which binary it is, and a bad .env names every mistake at once (§20 erratum 104)"
# `docker compose up -d --build` (step 1) passes VERSION=${CLIPPET_VERSION:-dev} to the Dockerfile, which
# bakes it into `-X main.version`. Without that arg every local build reported "dev" and §16's upgrade
# checklist had nothing that changes on a release. The expected value is whatever this stack's .env sets,
# and "dev" when it sets nothing — so this holds on a stock checkout as well as on a lane.
WANT_VERSION=$(envfile CLIPPET_VERSION); WANT_VERSION=${WANT_VERSION:-dev}
code=$(get "$BASE/healthz"); expect "$code" 200 "GET /healthz"
expect "$(json version)" "$WANT_VERSION" "/healthz version (compose build arg VERSION)"
expect "$(json db)" ok "/healthz db"
out=$(dc exec -T app clippet version) || die "clippet version failed: $out"
contains "$out" "$WANT_VERSION" || die "clippet version prints '$out', want '$WANT_VERSION'"
# The health payload names no configuration value: §15's rule, and the reason the DB field renders a
# bounded class rather than the driver's message (erratum 99), which can carry DATA_DIR or a statement.
lacks_file "$RESP" '/data' "/healthz must expose no path"
lacks_file "$RESP" 'clippet.db' "/healthz must not name the database file"

# Configuration: EVERY bad variable is reported, not just the first. This runs the real binary inside the
# real container with three deliberately wrong values; it loads no database and writes nothing, because
# config.Load fails before any of that. Its output goes to the exec, not to the container log, so the
# step-16 gate never sees it.
# `|| CFG_CODE=$?` keeps the deliberate failure out of the ERR trap (bash does not run it for the left
# side of an || list), so the trap's own message cannot end up inside the output being asserted on.
CFG_CODE=0
CFG_OUT=$(dc exec -T -e TRANSCODE_CRF=52 -e LOG_FORMAT=yaml -e TRANSCODE_CONCURRENCY=40 app clippet migrate 2>&1) || CFG_CODE=$?
expect "$CFG_CODE" 2 "a bad config exits 2"
printf '%s\n' "$CFG_OUT" | sed -n 's/^/[e2e]   /p'
for want in TRANSCODE_CRF LOG_FORMAT TRANSCODE_CONCURRENCY; do
  contains "$CFG_OUT" "clippet: config: $want" || die "the config report does not name $want: $CFG_OUT"
done
# TRANSCODE_CONCURRENCY has a ceiling now, so a trailing zero is a boot error rather than an OOM kill of
# the whole instance — the process that also serves HTTP and runs the sweeper.
contains "$CFG_OUT" 'TRANSCODE_CONCURRENCY: must be <= 16' || die "TRANSCODE_CONCURRENCY has no ceiling: $CFG_OUT"
n=$(printf '%s\n' "$CFG_OUT" | grep -c 'clippet: config:' || true)
expect "$n" 3 "one line per bad variable"
# …and a single mistake still reads as one line, exactly as it always did.
ONE_OUT=$(dc exec -T -e TRANSCODE_CRF=52 app clippet migrate 2>&1) || true
n=$(printf '%s\n' "$ONE_OUT" | grep -c 'clippet: config:' || true)
expect "$n" 1 "a lone bad variable is still one line"
echo "[e2e]   version=$WANT_VERSION live; a three-typo .env named all three at once"
