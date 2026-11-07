# 7-clip.sh — the matrix clips A-E (A, B and D its own, C and E the setup's): what the API measured, the media
# routes' headers and ranges, the bytes themselves (host ffprobe), the cards and the embed, the owner's
# PATCH and DELETE, view dedupe and a forced expiry. Steps 7-14, which were the serial core of
# scripts/e2e.sh until run 9's lane `pool` (scripts/e2e.d/README.md "The core in the pool").
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=MX_ buckets=-
# e2e-covers: internal/httpapi/media.go internal/httpapi/clip_handlers.go internal/httpapi/view.go
# e2e-covers: internal/views/views.go internal/web/meta.go internal/web/pages.go internal/web/server.go
# e2e-covers: web/templates/clip.html web/templates/embed.html web/templates/notfound.html
# e2e-covers: internal/transcode/encode.go internal/transcode/poster.go internal/transcode/transcode.go
# e2e-covers: internal/worker/pipeline.go internal/worker/runner.go internal/sweeper/sweeper.go cmd/clippet/admin.go
#   A, B and D are its OWN (uploaded here since run 9; they were the setup's): it gives A two view beacons
#   and expires it, PATCHes B and deletes it, and deletes D. C and E — which 31-nits, 37-library,
#   41-social, 43-watch, 51-admin and 55-owner read — it only reads. Every count it asserts is one clip's
#   own (A's views, D's prefix), so nothing another bundle uploads can move it, and a retry makes its three
#   clips afresh. It restarts and reconfigures nothing and drains no limiter: two view beacons are not a
#   drain.
#
# Variables are prefixed MX_ (its A, B, D are MX_A, MX_B, MX_D; the harness's C and E are read, never assigned).

step 7 "metadata: measured dims/fps/audio per clip"
# A, B and D are this bundle's OWN since run 9 (lane `pool`): it is the only one that reads them, and it
# expires A, PATCHes B and deletes D — so they are made here, where a retry on a fresh attempt makes them
# again, and the setup waits for three clips fewer. Step 4's upload asserts for them moved with them.
# C and E are the harness's (setup, steps 4 and 6); this bundle only reads them.
MX_C=$C; MX_E=$E
code=$(upload MX_A std-720p30.mp4 '{"title":"E2E std","ttl_days":1}'); expect "$code" 201 "upload A ($(cat "$RESP"))"
MX_A=$(json id); expect_nonempty "$MX_A" "A id"
expect "$(json state)" queued "A state"; expect "$(json ttl_days)" 1 "A ttl_days"; expect_nonempty "$(json expires_at)" "A expires_at"
expect "$(json title)" "E2E std" "A title"; expect_empty "$(json poster)" "A poster before ready"
expect "$(json views)" 0 "A views for the owner (uploader)"
# B sends no TTL at all and C (setup) sends an explicit 0: the wire format's two spellings of "never".
code=$(upload MX_B noaudio-720p30.mp4 '{"title":"E2E noaudio"}'); expect "$code" 201 "upload B ($(cat "$RESP"))"
MX_B=$(json id); expect "$(json ttl_days)" 0 "B ttl_days (server default never)"; expect_empty "$(json expires_at)" "B expires_at"
code=$(upload MX_D hd-1080p60.mp4 '{"title":"E2E hd60"}'); expect "$code" 201 "upload D ($(cat "$RESP"))"
MX_D=$(json id); expect_nonempty "$MX_D" "D id"
wait_ready "$MX_A" A; wait_ready "$MX_B" B; wait_ready "$MX_D" D
code=$(get "$BASE/api/clips/$MX_A"); expect "$code" 200 "GET A"
expect "$(json state)" ready "A state"; expect "$(json width)" 1280 "A width"; expect "$(json height)" 720 "A height"
expect "$(json has_audio)" true "A has_audio"; expect "$(json fps)" "30/1" "A fps"
dur=$(json duration_ms); { [ "$dur" -ge 5500 ] && [ "$dur" -le 6500 ]; } || die "A duration_ms=$dur, want ≈6000"
MX_APW=$(json poster.width); MX_APH=$(json poster.height); expect_nonempty "$MX_APW" "A poster.width"; expect_nonempty "$MX_APH" "A poster.height"
expect_nonempty "$(json ready_at)" "A ready_at"; expect "$(json urls.social)" "$BASE/api/clips/$MX_A/social.mp4" "A urls.social"
code=$(get "$BASE/api/clips/$MX_B"); expect "$code" 200 "GET B"; expect "$(json has_audio)" false "B has_audio"
code=$(get "$BASE/api/clips/$C"); expect "$code" 200 "GET C"; expect "$(json width)" 720 "C width"; expect "$(json height)" 1280 "C height"
# D is hd-1080p60, and since the quality tiers it is NOT downscaled: the upload
# is web-playable H.264/AAC in mp4, so the transcode remuxes it (`-c copy` into a
# faststart container) and the clip is 1920x1080 at 60 fps — the numbers the file
# was uploaded with. 62-tiers.sh is where that path is proved; here it is simply
# what the metadata now says.
code=$(get "$BASE/api/clips/$MX_D"); expect "$code" 200 "GET D"; expect "$(json width)" 1920 "D width"; expect "$(json height)" 1080 "D height"; expect "$(json fps)" "60/1" "D fps"
code=$(get "$BASE/api/clips/$E"); expect "$code" 200 "GET E"; expect "$(json has_audio)" true "E has_audio"
for x in B C D E; do v=MX_$x; code=$(get "$BASE/api/clips/${!v}"); eval "MX_${x}PW=$(json poster.width); MX_${x}PH=$(json poster.height); MX_${x}W=$(json width); MX_${x}H=$(json height)"; done
# A's upload is web-playable H.264/AAC in mp4 with `moov` at the end, so what
# /video.mp4 serves is the REMUX of it: the same coded bytes in a faststart
# container. Its colour tags are therefore the SOURCE's, unchanged and unclaimed —
# which is the honest answer for a file nothing re-encoded, and is checked here as
# an equality rather than as a constant. Erratum 93's unconditional Rec.709 stamp
# is an ENCODE property and belongs to a Full tier; 62-tiers.sh asserts it there.
# The BYTES are the point, so the mode is followed to wherever they are (§20 erratum 280, rule (a)).
code=$(media_download "$BASE/api/clips/$MX_A/video.mp4" "$E2E_DIR/A-colour.mp4"); expect "$code" 200 "download A for the colour check"
mx_colour_of() { ffprobe -v error -select_streams v:0 -show_entries stream=color_space,color_transfer,color_primaries -of csv=p=0 "$(W "$1")" | tr -d '\r'; }
MX_ACOL=$(mx_colour_of "$E2E_DIR/A-colour.mp4")
expect "$MX_ACOL" "$(mx_colour_of "$PWD/testclips/std-720p30.mp4")" "A's remux carries the upload's own colour tags"
rm -f "$E2E_DIR/A-colour.mp4"
step 8 "media headers on A: video.mp4 (200/206/304/416), social.mp4 (same ETag+length), poster.jpg, original (owner only)"
# The whole of this step is `video.mp4`, which is one of the three routes `MEDIA_DELIVERY=redirect`
# answers with a 302 (§20 erratum 278(a)), so it is the step where the two conversion rules of erratum
# 280 are both visible. What the OBJECT says — its type, its validator, its length, a range of it, a
# conditional on it — travels with the bytes and is asserted in either mode through `media_head` and
# `media_get`. What the APP's own range PARSER says — erratum 38's zero-length suffix, erratum 44's
# unknown unit, erratum 43's stale If-Range, and the shape of the 416 envelope — is a statement about
# a response a 302 replaces entirely, so in `redirect` it stands down with a printed line and step 90
# proves it on an instance of its own, in both modes.
MX_VID="$BASE/api/clips/$MX_A/video.mp4"
code=$(media_head "$MX_VID"); expect_media "$code" 200 "HEAD video.mp4"
expect "$(hdr Content-Type)" video/mp4 "video Content-Type"
MX_ETAG=$(hdr ETag); expect_nonempty "$MX_ETAG" "video ETag"
if [ "$E2E_MEDIA" = proxy ]; then
  expect "$(hdr Accept-Ranges)" bytes "video Accept-Ranges"
else
  # NOT a stand-down since §20 erratum 283 (g): the `media_head` above asked the BUCKET for one byte of
  # this object and asserted `Accept-Ranges: bytes` on that answer. The app's header is replaced by a
  # 302, and the origin that actually serves the bytes now has to make the same claim.
  echo "[e2e]   (redirect: Accept-Ranges was asserted on the BUCKET's own answer, one hop along)"
fi
MX_LEN=$(hdr Content-Length); expect_nonempty "$MX_LEN" "video Content-Length"
contains "$(hdr Cache-Control)" immutable && die "video Cache-Control must never be immutable: $(hdr Cache-Control)"
code=$(media_get "$MX_VID" -H 'Range: bytes=0-99'); expect "$code" 206 "Range 0-99"
expect "$(hdr Content-Range)" "bytes 0-99/$MX_LEN" "Content-Range"
expect "$(wc -c < "$RESP" | tr -d ' ')" 100 "Range 0-99 body bytes"
: > "$RESP"   # curl leaves a -o file untouched when the answer has no body, and the 206 above filled it
code=$(media_get "$MX_VID" -H "If-None-Match: $MX_ETAG"); expect "$code" 304 "If-None-Match"
expect "$(wc -c < "$RESP" | tr -d ' ')" 0 "the 304 has no body"
code=$(media_get "$MX_VID" -H 'Range: bytes=999999999999-'); expect "$code" 416 "unsatisfiable Range"
if [ "$E2E_MEDIA" = proxy ]; then
  expect "$(hdr Content-Range)" "bytes */$MX_LEN" "416 Content-Range"
  code=$(get "$MX_VID" -H 'Range: bytes=-0'); expect "$code" 416 "zero-length suffix Range"   # §20 erratum 38
  expect "$(hdr Content-Range)" "bytes */$MX_LEN" "zero-suffix 416 Content-Range"
else
  # The app's 416 ENVELOPE is replaced by a 302 and stands down — but the refusal itself is the
  # bucket's now, and that is asserted rather than assumed (§20 erratum 283 (g)). RFC 7233 makes
  # `Content-Range: bytes */<len>` on a 416 a SHOULD, and not every S3 implementation sends one, so
  # what is checked is the part that is not optional: a refusal carries none of the object, and it
  # came from the media origin the app signed for.
  [ "$(wc -c < "$RESP" | tr -d ' ')" -lt "$MX_LEN" ] || die "the bucket answered an unsatisfiable range with the whole object ($(wc -c < "$RESP" | tr -d ' ') bytes of $MX_LEN)"
  expect "$(media_origin_ "$MEDIA_LOC")" "$E2E_MEDIA_ORIGIN" "the 416 was the BUCKET's own refusal, at the origin the app signed for"
  media_skip "the 416 ENVELOPE, and erratum 38's zero-length suffix range"
fi
code=$(media_get "$MX_VID" -H 'Range: bytes=-100'); expect "$code" 206 "suffix Range"
expect "$(wc -c < "$RESP" | tr -d ' ')" 100 "suffix Range body bytes"
# …and the 206 says WHICH hundred bytes. New in BOTH modes (§20 erratum 283 (g)): the body length was
# checked and the Content-Range was not, so an implementation that answered the FIRST hundred bytes to
# a suffix request satisfied every assertion here. In `redirect` this is the bucket's arithmetic and in
# `proxy` it is the app's, and a suffix range is the one shape where each has to work out a number it
# was never told.
expect "$(hdr Content-Range)" "bytes $((MX_LEN - 100))-$((MX_LEN - 1))/$MX_LEN" "suffix Range Content-Range"
if [ "$E2E_MEDIA" = proxy ]; then
  code=$(head_ "$MX_VID" -H 'Range: items=0-1'); expect "$code" 200 "unknown range unit ignored"       # §20 erratum 44
  expect "$(hdr Content-Length)" "$MX_LEN" "ignored-unit Content-Length"
  code=$(head_ "$MX_VID" -H 'Range: bytes=-0' -H 'If-Range: "stale"'); expect "$code" 200 "stale If-Range ignores the Range"  # §20 erratum 43
else
  media_skip "erratum 44's unknown range unit and erratum 43's stale If-Range"
fi
code=$(media_head "$BASE/api/clips/$MX_A/social.mp4"); expect_media "$code" 200 "HEAD social.mp4"
# The equality is the SKIP RULE's control, not a property of social.mp4: A is std-720p30 — 30 fps and
# far under SOCIAL_MAX_BYTES — so no dedicated card file is made for it and the route COALESCEs to the
# rendition (§20 erratum 121; 41-social.sh section 5 documents the same control for C, and its clip S
# is the positive case). Swap A's fixture for a 60 fps or large clip and these two lines start failing
# for that reason, not because social.mp4 broke.
expect "$(hdr ETag)" "$MX_ETAG" "social ETag = video ETag"; expect "$(hdr Content-Length)" "$MX_LEN" "social Content-Length = video"
code=$(media_head "$BASE/api/clips/$MX_A/poster.jpg"); expect_media "$code" 200 "HEAD poster.jpg"; expect "$(hdr Content-Type)" image/jpeg "poster Content-Type"
MX_ORIG="$BASE/api/clips/$MX_A/original"
code=$(head_ "$MX_ORIG" -b "$JARW"); expect "$code" 200 "HEAD original (owner)"
MX_OETAG=$(hdr ETag); expect_nonempty "$MX_OETAG" "original ETag"
contains "$(hdr Content-Disposition)" attachment || die "original Content-Disposition: $(hdr Content-Disposition)"
code=$(head_ "$MX_ORIG" -b "$JARW" -H "If-None-Match: $MX_OETAG"); expect "$code" 304 "original If-None-Match"
code=$(head_ "$MX_ORIG"); case $code in 401|403) ;; *) die "anonymous original → $code, want 401/403";; esac
step 9 "download A–E + host ffprobe (codecs, dims, box, faststart); poster dims + no black card image; E both-ears astats"
for x in A B C D E; do
  v=MX_$x
  code=$(media_download "$BASE/api/clips/${!v}/video.mp4" "$E2E_DIR/$x.mp4"); expect "$code" 200 "download $x"
  ffprobe -v error -print_format json -show_streams -show_format "$(W "$E2E_DIR/$x.mp4")" > "$E2E_DIR/probe-$x.json"
done
probe_check "$E2E_DIR/probe-A.json" "$E2E_DIR/A.mp4" 1 1280 720 30/1 1280x720
probe_check "$E2E_DIR/probe-B.json" "$E2E_DIR/B.mp4" 0 "$MX_BW" "$MX_BH"
probe_check "$E2E_DIR/probe-C.json" "$E2E_DIR/C.mp4" 1 "$MX_CW" "$MX_CH"
# No box for D: it is 1080p, which is the whole point (the tiers removed the cap).
probe_check "$E2E_DIR/probe-D.json" "$E2E_DIR/D.mp4" 1 "$MX_DW" "$MX_DH" 60/1
# E is the dual-audio fixture: two source tracks the pipeline MERGES into one
# stereo aac track, which is a property of the encode and is pinned as one.
probe_check "$E2E_DIR/probe-E.json" "$E2E_DIR/E.mp4" 1 "$MX_EW" "$MX_EH" "" 1280x720 2
# Every matrix clip's card image, measured: a poster is only a poster if there is a PICTURE in it
# (§20 erratum 92 — the old blind 15% seek served a black frame for anything with an intro).
for x in A B C D E; do
  v=MX_$x
  code=$(media_download "$BASE/api/clips/${!v}/poster.jpg" "$E2E_DIR/$x.jpg"); expect "$code" 200 "download $x poster"
  y=$(yavg "$E2E_DIR/$x.jpg" "$x"); echo "[e2e]   $x poster YAVG=$y"
  awk -v y="$y" 'BEGIN { if (y == "") exit 1; exit !(y + 0 > 8) }' \
    || die "clip $x poster YAVG=$y: the card image is a black frame"
done
expect "$(ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0:s=x "$(W "$E2E_DIR/A.jpg")" | tr -d '\r')" "${MX_APW}x${MX_APH}" "poster dims = DTO poster.width/height"
# E: both source tones (440 Hz game, 880 Hz mic) present at equal level in BOTH channels — merged, not dropped, not panned.
for f in 440 880; do
  l=$(rms "$E2E_DIR/E.mp4" 0 "$f"); r=$(rms "$E2E_DIR/E.mp4" 1 "$f")
  echo "[e2e]   E ${f}Hz: c0=${l} dB c1=${r} dB"
  awk -v l="$l" -v r="$r" 'BEGIN { if (l == "" || r == "" || l == "-inf" || r == "-inf") exit 1; if (l <= -40 || r <= -40) exit 2; d = l - r; if (d < 0) d = -d; if (d >= 1) exit 3; exit 0 }' \
    || die "E ${f}Hz: c0=$l c1=$r (want both > -40 dB and |c0-c1| < 1 dB)"
done
step 10 "Discordbot + Twitterbot card sweep on /$MX_A (full §9.1 inventory); view-count privacy; no inline style"
code=$(page "$BASE/$MX_A" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot share page"
card_check player "$PAGE" "$BASE" "$MX_A" "E2E std" "$MX_APW" "$MX_APH" 1280 720 "$SECURE"
code=$(page "$BASE/$MX_A" -A "$TWITTER_UA"); expect "$code" 200 "Twitterbot share page"
card_check player "$PAGE" "$BASE" "$MX_A" "E2E std" "$MX_APW" "$MX_APH" 1280 720 "$SECURE"
code=$(page "$BASE/$MX_A"); expect "$code" 200 "anonymous share page"
lacks_file "$PAGE" 'data-views' "anonymous share page view count"
# https-only: an https page that references a plain-http URL is mixed content — browsers block the
# media silently and X drops the card. The templates hold no absolute literals, so `http://` in the
# rendered HTML can only come from a misconfigured PUBLIC_BASE_URL.
[ "$SECURE" = 0 ] || lacks_file "$PAGE" 'http://' "anonymous share page plain-http URL (mixed content)"
lacks_file "$PAGE" 'style=' "share page inline style attribute"; lacks_file "$PAGE" '<style' "share page style element"
has_file "$PAGE" '<video' "share page player"
code=$(page "$BASE/$MX_A" -b "$JARW"); expect "$code" 200 "owner share page"
has_file "$PAGE" 'data-views' "owner share page view count"
lacks_file "$PAGE" 'style=' "owner page inline style"; lacks_file "$PAGE" '<style' "owner page style element"
# The owner's own upload token is an upload credential, not a session (§20 errata 19/22): valid on /api/auth/me,
# it unlocks neither the private view count nor the owner controls on the share page.
code=$(http POST "$BASE/api/auth/tokens" -b "$JARW" -H 'Content-Type: application/json' -d '{"name":"e2e share-page probe"}')
expect "$code" 201 "mint token"; MX_TOK=$(json token); expect_nonempty "$MX_TOK" "raw token"
code=$(get "$BASE/api/auth/me" -H "Authorization: Bearer $MX_TOK"); expect "$code" 200 "me via token"; expect "$(json via)" token "me → via (token)"
code=$(page "$BASE/$MX_A" -H "Authorization: Bearer $MX_TOK"); expect "$code" 200 "share page with a Bearer token"
lacks_file "$PAGE" 'data-views' "token share page view count"; lacks_file "$PAGE" 'data-owner-controls' "token share page owner controls"
code=$(page "$BASE/aZ3kQ9xB2mLp"); expect "$code" 404 "unknown clip page"
lacks_file "$PAGE" 'og:video' "404 page must carry no card tags"
if [ "$SECURE" = 1 ]; then
  # PUBLIC RESOLVABILITY — the brief's Phase 3 gate condition, and the one thing a local run cannot
  # prove: every URL this card hands out is fetched BACK over the internet, from the public origin,
  # with each bot's own User-Agent, and must answer 200. og:url (share page), og:image (poster.jpg),
  # og:video + twitter:player:stream (social.mp4), og:video:secure_url, twitter:player (embed),
  # twitter:image, canonical, and the <video> src/poster (video.mp4). A tag URL that 404s from
  # outside turns a playable card into a bare link, and Discord caches that verdict per URL.
  code=$(page "$BASE/$MX_A" -A "$DISCORD_UA"); expect "$code" 200 "share page for the URL sweep"
  card_urls "$PAGE" "$BASE" | tr -d '\r' > "$E2E_DIR/card-urls.txt"   # python on Windows prints CRLF; curl rejects a URL with a CR
  MX_CARD_URLS=$(grep -c . "$E2E_DIR/card-urls.txt" || true); MX_CARD_URLS=${MX_CARD_URLS:-0}
  for want in "/$MX_A" "/api/clips/$MX_A/poster.jpg" "/api/clips/$MX_A/social.mp4" "/embed/$MX_A" "/api/clips/$MX_A/video.mp4"; do
    grep -Fxq "$BASE$want" "$E2E_DIR/card-urls.txt" || die "public URL sweep: $BASE$want is not among the page's URLs"
  done
  [ "$MX_CARD_URLS" -ge 5 ] || die "public URL sweep: only $MX_CARD_URLS URLs on a ready share page"
  while read -r u; do
    [ -n "$u" ] || continue
    case $u in https://*) ;; *) die "card URL is not https: $u";; esac
    case $u in "$BASE"/*) ;; *) die "card URL is not on the public origin $BASE: $u";; esac
    for pair in "Discordbot:$DISCORD_UA" "Twitterbot:$TWITTER_UA"; do
      # `media_maybe`: the list holds a share page, an embed and three media routes, and in
      # `redirect` mode the last three answer 302 (§20 erratum 280). The question this sweep asks is
      # "can the bot on the other end get these bytes", and the bot follows the hop — measured
      # against the real unfurler before redirect mode shipped (erratum 278(h)) — so the helper
      # follows it too, after asserting it lands on the run's one media origin.
      c=$(media_maybe "$u" -A "${pair#*:}") || die "${pair%%:*} could not reach $u"
      expect "$c" 200 "${pair%%:*} GET $u"
    done
    echo "[e2e]   200 to Discordbot + Twitterbot: $u"
  done < "$E2E_DIR/card-urls.txt"
  echo "[e2e]   $MX_CARD_URLS card URLs publicly resolvable from $BASE"
fi
step 11 "embed /embed/$MX_A: iframe-safe headers, player, no view count, no inline style"
code=$(head_ "$BASE/embed/$MX_A"); expect "$code" 200 "HEAD embed"
expect_empty "$(hdr X-Frame-Options)" "embed X-Frame-Options must be absent"
contains "$(hdr Content-Security-Policy)" 'frame-ancestors *' || die "embed CSP lacks frame-ancestors *: $(hdr Content-Security-Policy)"
code=$(page "$BASE/embed/$MX_A"); expect "$code" 200 "GET embed"
has_file "$PAGE" '<video' "embed player"; lacks_file "$PAGE" 'data-views' "embed view count"
lacks_file "$PAGE" 'style=' "embed inline style"; lacks_file "$PAGE" '<style' "embed style element"
# Player params (#8): the plain embed carries none of them and preloads metadata only…
has_file "$PAGE" 'preload="metadata"' "plain embed preload"
lacks_file "$PAGE" ' autoplay' "plain embed autoplay"; lacks_file "$PAGE" ' muted' "plain embed muted"
lacks_file "$PAGE" ' loop' "plain embed loop"
# …and ?autoplay=1&loop=1 maps to the <video> attributes, with MUTED added: no browser starts an
# unmuted video by itself, so autoplay without it would render a player that silently does nothing.
code=$(page "$BASE/embed/$MX_A?autoplay=1&loop=1"); expect "$code" 200 "GET embed with player params"
has_file "$PAGE" ' autoplay' "embed autoplay attribute"
has_file "$PAGE" ' muted' "embed muted attribute (autoplay implies it)"
has_file "$PAGE" ' loop' "embed loop attribute"
has_file "$PAGE" 'preload="auto"' "embed preload with autoplay"
lacks_file "$PAGE" 'style=' "embed inline style with params"
# The share page ignores the same query: a person opening a link did not ask for autoplay.
code=$(page "$BASE/$MX_A?autoplay=1&loop=1"); expect "$code" 200 "share page with player params"
lacks_file "$PAGE" ' autoplay' "share page autoplay"; lacks_file "$PAGE" ' loop' "share page loop"
if [ "$SECURE" = 1 ]; then
  # The embed is what X iframes from an https page: it must be framable AND free of http URLs, or the
  # browser blocks the media as mixed content inside a card that otherwise looks fine.
  lacks_file "$PAGE" 'http://' "embed plain-http URL (mixed content inside an https iframe)"
  code=$(head_ "$BASE/embed/$MX_A" -A "$TWITTER_UA"); expect "$code" 200 "embed HEAD via the public origin (Twitterbot)"
  expect_empty "$(hdr X-Frame-Options)" "embed X-Frame-Options through the public origin"
  contains "$(hdr Content-Security-Policy)" 'frame-ancestors *' || die "embed CSP through the public origin lacks frame-ancestors *: $(hdr Content-Security-Policy)"
  echo "[e2e]   https: embed is iframe-safe through the public origin and carries no plain-http URL"
fi
step 12 "owner ops: PATCH title/TTL on B (cookie jar, no Origin → CSRF passes); DELETE D → 204, 404, empty prefix"
code=$(http PATCH "$BASE/api/clips/$MX_B" -b "$JARW" -H 'Content-Type: application/json' -d '{"title":"renamed"}'); expect "$code" 200 "PATCH title"
expect "$(json title)" renamed "PATCH title → title"
code=$(http PATCH "$BASE/api/clips/$MX_B" -b "$JARW" -H 'Content-Type: application/json' -d '{"ttl_days":7}'); expect "$code" 200 "PATCH ttl 7"
expect "$(json ttl_days)" 7 "PATCH ttl 7 → ttl_days"; expect_nonempty "$(json expires_at)" "PATCH ttl 7 → expires_at"
code=$(http PATCH "$BASE/api/clips/$MX_B" -b "$JARW" -H 'Content-Type: application/json' -d '{"ttl_days":0}'); expect "$code" 200 "PATCH ttl 0"
expect "$(json ttl_days)" 0 "PATCH ttl 0 → ttl_days"; expect_empty "$(json expires_at)" "PATCH ttl 0 → expires_at"
code=$(http_refused PATCH "$BASE/api/clips/$MX_B" -b "$JARW" -H 'Content-Type: application/json' -d '{"ttl_days":5}'); expect "$code" 400 "PATCH ttl 5"
expect "$(json error)" invalid_ttl "PATCH ttl 5 → error"
code=$(http DELETE "$BASE/api/clips/$MX_D" -b "$JARW"); expect_deleted "$code" "$MX_D" "DELETE D"
code=$(get "$BASE/api/clips/$MX_D"); expect "$code" 404 "GET D after delete"
code=$(get "$BASE/api/clips/$MX_D/progress"); expect "$code" 404 "progress D after delete"
expect "$(prefix_count "$MX_D")" 0 "D bucket prefix empty"
step 13 "view dedupe + privacy: two anonymous views → owner sees views=1; anonymous DTO has no views key"
code=$(http POST "$BASE/api/clips/$MX_A/view"); expect "$code" 204 "view 1"
code=$(http POST "$BASE/api/clips/$MX_A/view"); expect "$code" 204 "view 2 (deduped)"
code=$(get "$BASE/api/clips/$MX_A" -b "$JARW"); expect "$code" 200 "owner GET A"; expect "$(json views)" 1 "owner views"
code=$(get "$BASE/api/clips/$MX_A"); expect "$code" 200 "anonymous GET A"; expect_empty "$(json views)" "anonymous views key"
step 14 "forced expiry via admin CLI (§8.8): expire A --ago 2h → sweeper destroys objects + row within ≈2 ticks"
out=$(dc exec -T app clippet admin expire "$MX_A" --ago 2h) || die "admin expire failed: $out"
echo "[e2e]   $out"; contains "$out" 'expires_at=' || die "admin expire did not print the new expires_at"
wait_gone "$MX_A" A
out=$(dc exec -T app clippet admin ls "$MX_A" 2>&1 || true); contains "$out" 'not found' || die "admin ls after sweep: $out"
code=$(http DELETE "$BASE/api/clips/$MX_B" -b "$JARW"); expect_deleted "$code" "$MX_B" "DELETE B (this bundle's own)"
summary_row A "$MX_A" "std 720p30: ready, full player card, media/range/etag, views, expired + swept"
summary_row B "$MX_B" "noaudio: ready, PATCH title/ttl, TTL sent as nothing → never; deleted"
summary_row D "$MX_D" "1080p60: ready 1920x1080@60 (remuxed, not downscaled), owner DELETE → 204 + empty prefix"
summary_row E "$E" "dual audio + UNLISTED: ready, one aac stereo, both tones in both ears (its #4 opt-out is 20-pages' step 20)"
