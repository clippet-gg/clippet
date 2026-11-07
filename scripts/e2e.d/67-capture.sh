# 67-capture.sh — bundle M2: the capture link is handed over only when the CARD works, a link-preview
# crawler is held rather than told the wrong thing, and the card's own copy says it is the card's copy
# (DESIGN §20 errata 212-214, 217). Sourced by scripts/e2e.sh before the step-16 log gate.
#
# e2e-schedule: parallel
# e2e-schedule-apart: 41-social
# e2e-namespace: account=- prefix=K_,K1_,K5_ buckets=-
# e2e-covers: internal/httpapi/upload_capture.go internal/httpapi/sxcu.go internal/web/crawlerhold.go
# e2e-covers: internal/transcode/lighttag.go internal/transcode/posterlabel.go internal/worker/tag.go
# e2e-covers: internal/web/meta.go internal/web/compembed.go web/templates/clip.html
#   Every clip is its own and is deleted; nothing it asserts is instance-wide. It is the bundle most
#   sensitive to LOAD rather than to state — step 67 bounds a small capture at 60 s and step 68 wants an
#   encode still running when a crawler asks — so it is kept APART from 41-social.sh, the heaviest
#   encoder of the parallel set, exactly as sharing lane 3 with it did until lane `speed2`.
#
# Every clip here is this file's own and is deleted again in the step that made it: nothing outlives the
# run, and no step touches A-E, W or X.
#
# Two of the three claims are about what is NOT in an answer — no card tags for a crawler that would
# cache them, no tag pixels on anything but the Light copy — so each is measured against a control in
# the same run rather than asserted on its own.

# k_yavg <file> <crop> → the mean luminance of one crop of the FIRST frame, as ffmpeg's own signalstats
# reports it. It is how the burned-in tag is proven: a semi-transparent white line raises the mean of
# the corner it is in and leaves the other three alone.
k_yavg() {
  local out="$E2E_DIR/k-yavg.txt"
  ffmpeg -hide_banner -nostdin -y -i "$(W "$1")" -vf "crop=$2,signalstats,metadata=print:key=lavfi.signalstats.YAVG" \
    -frames:v 1 -f null - > "$out" 2>&1 || die "ffmpeg signalstats failed: $(tail -3 "$out")"
  sed -n 's/.*lavfi.signalstats.YAVG=//p' "$out" | head -1 | tr -d '\r'
}
# k_gt <a> <b> → 0 when a > b, in float arithmetic awk can do and the shell cannot.
k_gt() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 > b + 0) }'; }
# k_dims <file> → "WxH" of the first video stream.
k_dims() {
  ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0:s=x "$(W "$1")" | tr -d '\r'
}
# k_delete <id> <label>: the owner deletes their own clip and the bucket prefix empties.
k_delete() {
  local code
  code=$(http DELETE "$BASE/api/clips/$1" -b "$JARW"); expect_deleted "$code" "$1" "DELETE $2"
  expect_prefix_count "$1" 0 "$2's objects after DELETE"
}
# k_capture <label> <file> <extra curl args…> → status; body in $RESP. POST /api/upload's own grammar:
# simple text fields, then the file part named `file` (§8.1).
k_capture() {
  local label=$1 file=$2; shift 2
  curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" "$@" \
    -F duplicate=allow -F "file=@$(W "$file");type=video/mp4" "$BASE/api/upload"
}

step 67 "the capture link is handed over only when the CARD works — and a client cannot switch that off"
# CAPTURE_READY_WAIT is the operator's, and the container is the only place that knows it.
K_WAIT=$(dc exec -T app printenv CAPTURE_READY_WAIT 2>/dev/null | tr -d '\r')
[ -n "$K_WAIT" ] || K_WAIT=30s
echo "[e2e]   CAPTURE_READY_WAIT=$K_WAIT"

# A 1080p60 capture: above 720p and above 30 fps, so the card gets its own copy and the reply has
# something real to wait for.
[ -s testclips/hd-1080p60.mp4 ] || die "testclips/hd-1080p60.mp4 missing (scripts/testclips.sh)"
K_T0=$SECONDS
code=$(k_capture K1 "$PWD/testclips/hd-1080p60.mp4" -F 'title=E2E capture card wait' -F 'ttl_days=1')
K_ELAPSED=$((SECONDS - K_T0))
expect "$code" 201 "POST /api/upload of a 1080p60 capture ($(cat "$RESP"))"
K1=$(json id); expect_nonempty "$K1" "K1 id"
K1_CARD=$(json card_ready); K1_STATE=$(json state)
echo "[e2e]   K1: state=$K1_STATE card_ready=$K1_CARD after ${K_ELAPSED}s"
# The reply carries the CARD's readiness as its own field, and the clip's state as `state` — two facts,
# never one word doing both jobs (§20 erratum 212).
case $K1_CARD in
  true)
    expect "$K1_STATE" ready "K1 state when card_ready is true"
    # AND THE LINK REALLY WORKS THIS INSTANT. That is the whole claim: not "the clip is stored", not
    # "the clip is playable", but "paste this and Discord inlines it".
    code=$(page "$BASE/$K1" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot GET K1 the moment its link was handed over"
    has_file "$PAGE" 'name="twitter:card" content="player"' "K1's card is a PLAYER card on the first crawler fetch"
    has_file "$PAGE" 'property="og:video"' "…with a video tag"
    ;;
  false)
    # Legal, and the ONE thing that makes it legal is that the budget really ran out: at the ceiling a
    # capture tool is answered anyway rather than held for minutes.
    K_WAIT_S=${K_WAIT%s}
    case $K_WAIT_S in *[!0-9]*) K_WAIT_S=30;; esac
    [ "$K_ELAPSED" -ge $((K_WAIT_S - 5)) ] \
      || die "K1 answered card_ready=false after only ${K_ELAPSED}s of a $K_WAIT budget — the reply returned early"
    echo "[e2e]   NOTE: K1's card was not ready inside the $K_WAIT budget (host under load); the wait itself is proved by the ${K_ELAPSED}s"
    wait_ready "$K1" K1
    ;;
  *) die "K1 card_ready = '$K1_CARD' (want true or false): $(cat "$RESP")";;
esac
# There is no time estimate anywhere in that reply. An estimate of an encode is wrong often enough to
# be worse than nothing (the owner's decision, 2026-09-08), so no key may imply one.
for k in eta estimate estimated_seconds remaining seconds_remaining retry_after; do
  expect_empty "$(json "$k")" "K1 reply must carry no time estimate ($k)"
done
# The reply's `tiers` is the ClipDTO's own list: three entries, best first, so a capture tool can say
# "1080p60, card at 720p30" without a second request.
cp "$RESP" "$E2E_DIR/capture-K1.json"
K1_TIERS=$($PY - "$(W "$E2E_DIR/capture-K1.json")" <<'PYEOF'
import json, sys
with open(sys.argv[1], encoding='utf-8') as fh:
    doc = json.load(fh)
print(','.join(t.get('tier', '?') for t in (doc.get('tiers') or [])))
PYEOF
)
expect "$K1_TIERS" "original,full,light" "K1 reply tiers (always three, best first)"

# A TINY clip comes back in seconds: the wait is a ceiling, never a delay.
[ -s testclips/std-720p30.mp4 ] || die "testclips/std-720p30.mp4 missing (scripts/testclips.sh)"
K_T0=$SECONDS
code=$(k_capture K2 "$PWD/testclips/std-720p30.mp4" -F 'title=E2E capture tiny' -F 'ttl_days=1')
K2_ELAPSED=$((SECONDS - K_T0))
expect "$code" 201 "POST /api/upload of a small 720p30 clip ($(cat "$RESP"))"
K2=$(json id); expect_nonempty "$K2" "K2 id"
echo "[e2e]   K2: state=$(json state) card_ready=$(json card_ready) after ${K2_ELAPSED}s"
[ "$K2_ELAPSED" -le 60 ] || die "a six-second 720p30 clip took ${K2_ELAPSED}s to answer"

# THE `wait` FIELD IS GONE, as a 400 and not as a silent ignore: a `.sxcu` carrying wait=0 must be told
# once, in a sentence naming what replaced it, rather than go on getting the opposite of what it asked
# for (§20 erratum 212).
for bad in 0 8 9999 abc -1; do
  code=$(k_capture Kw "$PWD/testclips/std-720p30.mp4" -F "wait=$bad")
  expect "$code" 400 "POST /api/upload with wait=$bad ($(cat "$RESP"))"
  expect "$(json error)" invalid_request "wait=$bad error code"
  contains "$(json message)" CAPTURE_READY_WAIT \
    || die "the wait=$bad message does not name the setting that replaced it: $(json message)"
done
# …and an EMPTY wait= still means "not sent", like every other capture field.
code=$(k_capture Kw "$PWD/testclips/std-720p30.mp4" -F 'wait=' -F 'title=E2E capture empty wait' -F 'ttl_days=1')
expect "$code" 201 "POST /api/upload with an empty wait= ($(cat "$RESP"))"
K3=$(json id); expect_nonempty "$K3" "K3 id"

wait_ready "$K2" K2; wait_ready "$K3" K3
# The card of the clip that was handed over is a player card for every one of them.
code=$(page "$BASE/$K2" -A "$TWITTER_UA"); expect "$code" 200 "Twitterbot GET K2"
has_file "$PAGE" 'name="twitter:card" content="player"' "K2's card is a player card"
k_delete "$K2" K2; k_delete "$K3" K3
summary_row K2 "$K2" "capture upload, small clip: answered in ${K2_ELAPSED}s, deleted"
summary_row K3 "$K3" "capture upload with an empty wait= (still 'not sent'), deleted"

step 68 "a link-preview crawler is HELD while a clip has no card — 503 + Retry-After, and no card tags"
# The subject has to be slow enough to still be encoding when the crawler asks, and the reliable way to
# get that without loosening anything is a source that needs BOTH tiers: twelve seconds of 1080p60
# stream-copied into matroska, which the playability table refuses (every media URL promises an mp4),
# so ONE ffmpeg writes a 1080p60 Full and a 720p30 card copy from it (§20 erratum 217). The copy costs
# the host nothing and the encode is tens of seconds at the shipped `cpus: 1.0` — long enough for the
# fetches below, short enough that three lanes encoding at once cannot push it past wait_ready's 180 s.
K_SLOW="$E2E_DIR/capture-slow.mkv"
ffmpeg -hide_banner -nostdin -loglevel error -y -stream_loop 1 -i "$(W "$PWD/testclips/hd-1080p60.mp4")" \
  -c copy "$(W "$K_SLOW")" || die "could not build the crawler-hold fixture"
expect "$(k_dims "$K_SLOW")" 1920x1080 "the crawler-hold fixture's dims"

printf '%s' '{"title":"E2E crawler hold","ttl_days":1}' > "$E2E_DIR/meta-K4.json"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "metadata=<$(W "$E2E_DIR/meta-K4.json");type=application/json" \
  -F duplicate=allow -F "video=@$(W "$K_SLOW");type=video/x-matroska" "$BASE/api/clips")
expect "$code" 201 "upload K4, the crawler-hold subject ($(cat "$RESP"))"
K4=$(json id); expect_nonempty "$K4" "K4 id"
K4_STATE=$(json state)

# POST /api/clips does NOT wait (that is POST /api/upload's job), so the clip is queued right now.
K_HELD=0
if [ "$K4_STATE" != ready ]; then
  code=$(page "$BASE/$K4" -A "$DISCORD_UA")
  if [ "$code" = 503 ]; then
    K_HELD=1
    expect "$(hdr Retry-After)" 30 "the hold's Retry-After"
    expect "$(hdr Cache-Control)" no-store "the hold's Cache-Control"
    # THE POINT: nothing for a crawler to remember. Not one og: tag, and not the title either.
    lacks_file "$PAGE" 'og:' "the hold page must carry no Open Graph tag"
    lacks_file "$PAGE" 'twitter:' "the hold page must carry no Twitter tag"
    lacks_file "$PAGE" 'E2E crawler hold' "the hold page must not even name the clip"
    has_file "$PAGE" 'name="robots" content="noindex"' "the hold page is noindex"
    # Every crawler in the table, and each one held.
    # Judged after the event, as the human page below is: a 200 is a failure unless the clip's card
    # state, read AFTER that answer, says the window has closed (the card only goes not-ready → ready).
    # `card_ready` is POST /api/upload's word and GET /api/clips/{id} does not carry it (ClipDTO), so the
    # read is db.CardReady spelled in the clip DTO's own fields: `ready`, a poster, the Light tier not
    # `encoding` — until 2026-10-18 this read an absent field, never saw the window close, and a 55 ms
    # race failed release cycle 14's gate (the GPU encodes the fixture in 13 s).
    k4_card_ready() {
      get "$BASE/api/clips/$K4" >/dev/null
      [ "$(json state)" = ready ] && [ -n "$(json poster)" ] && [ "$(json tiers.2.state)" != encoding ]
    }
    for ua in "$TWITTER_UA" 'facebookexternalhit/1.1' 'Slackbot-LinkExpanding 1.0' 'TelegramBot (like TwitterBot)'; do
      code=$(page "$BASE/$K4" -A "$ua")
      if [ "$code" != 503 ] && k4_card_ready; then
        echo "[e2e]   NOTE: K4's card became ready during the crawler sweep (at '$ua'); the held answers above stand"
        break
      fi
      expect "$code" 503 "the hold for User-Agent '$ua'"
    done
    # A HUMAN is never held: the ordinary processing page, its link-out card, and one short line saying
    # what is happening and where.
    code=$(page "$BASE/$K4" -A 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/140.0 Safari/537.36')
    expect "$code" 200 "a human GET of a clip with no card yet"
    # The hold is a WINDOW, and the encode can close it between the crawler fetches above and this one
    # (it did, once, at E2E_LANES=8 in lane `excl`'s measurements). So the human page is judged AFTER
    # the event: the clip's card state is read once the page is in hand, and the card only ever goes
    # not-ready → ready, so "still not ready NOW" proves it was not when the page was rendered — then
    # every line below must hold. A window that has closed is printed, never passed silently (the five
    # holds above already stood).
    cp "$PAGE" "$CTX_DIR/k4-human.html"
    if k4_card_ready && ! grep -q 'data-card-note' "$CTX_DIR/k4-human.html"; then
      echo "[e2e]   NOTE: K4's card became ready between the crawler holds and the human GET; the human page's not-ready line is pinned by the web unit tests"
    else
      has_file "$CTX_DIR/k4-human.html" 'data-card-note' "the human page names what is happening"
      has_file "$CTX_DIR/k4-human.html" 'Encoding on the server' "…and where it happens, in one short line"
      has_file "$CTX_DIR/k4-human.html" 'the Discord card is not ready yet' "…and the consequence"
      lacks_file "$CTX_DIR/k4-human.html" 'og:video' "a clip with no card carries no video tag for anyone"
    fi
    # curl's own default agent is not a crawler either.
    code=$(page "$BASE/$K4"); expect "$code" 200 "a plain (curl) GET of a clip with no card yet"
    echo "[e2e]   the hold is live: five crawlers held at 503, a human and curl at 200"
  else
    echo "[e2e]   NOTE: K4 was already card-ready at the first crawler fetch (code $code); the hold is pinned deterministically by the web unit tests"
  fi
fi
wait_ready "$K4" K4

# ONCE READY the same crawler gets the card it came for — the hold is a window, not a policy.
code=$(page "$BASE/$K4" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot GET K4 once it is ready"
has_file "$PAGE" 'name="twitter:card" content="player"' "K4's card is a player card once ready"
lacks_file "$PAGE" 'data-card-note' "a card-ready clip carries no not-ready line"
code=$(get "$BASE/api/clips/$K4"); expect "$code" 200 "GET K4 once ready"
expect "$(json card_tier)" light "K4 card_tier (a 1080p60 mkv gets a card copy)"
# One decode wrote both tiers, so the FAITHFUL copy is here at the ready flip and nothing ever served
# the 720p30 card copy as this clip's own file (§20 erratum 217).
expect "$(json width)x$(json height)" 1920x1080 "K4 dims at the ready flip (the Full, not the card copy)"

# A FAILED clip is NEVER held: its link-out card is the card it will have for ever, so withholding it
# would leave the paste with no card at all. This suite has no permanently-failed clip to point at (a
# clip that passes the probe gate and then fails the encode cannot be arranged reliably), so that
# regression guard is pinned by internal/web's TestCrawlerHoldOverHTTP, which drives the real handler
# over a failed fixture and asserts the link-out card. Named here so it is not looked for twice.
k_delete "$K4" K4
rm -f "$K_SLOW"
summary_row K4 "$K4" "crawler hold (held=$K_HELD), both tiers from one decode, deleted"

step 69 "the LIGHT copy carries the tag, the poster carries its duration, and the card's description states both qualities"
# A BLACK 1080p60 source, so a semi-transparent white line is the only thing that can move a corner's
# mean luminance. The real fixtures are testsrc2 patterns and far too busy for that measurement to mean
# anything; this is the unit test's own method, run end to end through the real stack.
K_BLACK="$E2E_DIR/capture-black.mp4"
ffmpeg -hide_banner -nostdin -loglevel error -y -f lavfi -i "color=c=black:s=1920x1080:rate=60:duration=5" \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p -an -movflags +faststart "$(W "$K_BLACK")" \
  || die "could not build the tag fixture"
expect "$(k_dims "$K_BLACK")" 1920x1080 "the tag fixture's dims"

K_T0=$SECONDS
code=$(k_capture K5 "$K_BLACK" -F 'title=E2E light tag' -F 'ttl_days=1')
expect "$code" 201 "POST /api/upload of the tag fixture ($(cat "$RESP"))"
K5=$(json id); expect_nonempty "$K5" "K5 id"
[ "$(json card_ready)" = true ] || wait_ready "$K5" K5
echo "[e2e]   K5 shareable after $((SECONDS - K_T0))s"

# The media helpers (§20 erratum 280, rule (a)): every K5 assertion below is measured in the PIXELS
# of the file that was fetched, so the fetch follows the bytes wherever the mode sends them.
code=$(media_download "$BASE/api/clips/$K5/light.mp4" "$E2E_DIR/K5-light.mp4")
expect "$code" 200 "download K5 light.mp4"
K5_LDIMS=$(k_dims "$E2E_DIR/K5-light.mp4")
expect "$K5_LDIMS" 1280x720 "K5's light copy is the card's own box"
# The card image is fetched HERE and not inside the tagged branch below: that it exists and borrows the
# card copy's SIZE is true whether or not a tag was measured, and a build with no font/drawtext (or
# LIGHT_TAG_TEXT off) used to take the NOTE path and assert nothing at all about poster.jpg.
code=$(media_download "$BASE/api/clips/$K5/poster.jpg" "$E2E_DIR/K5-poster.jpg")
expect "$code" 200 "download K5 poster.jpg"
expect "$(k_dims "$E2E_DIR/K5-poster.jpg")" "$K5_LDIMS" "K5's card image is the card's own box"
# The tag is a 420x40 region in the BOTTOM-RIGHT corner. Its own corner brightens; the other three do
# not, which is what makes this a measurement of the tag rather than of the encoder.
K5_BR=$(k_yavg "$E2E_DIR/K5-light.mp4" "420:40:in_w-420:in_h-40")
K5_BL=$(k_yavg "$E2E_DIR/K5-light.mp4" "420:40:0:in_h-40")
K5_TR=$(k_yavg "$E2E_DIR/K5-light.mp4" "420:40:in_w-420:0")
echo "[e2e]   K5 light copy corner luminance: bottom-right=$K5_BR bottom-left=$K5_BL top-right=$K5_TR"
K_TAG_ON=0
if k_gt "$K5_BR" "$(awk -v v="$K5_BL" 'BEGIN { print v + 0.5 }')"; then
  K_TAG_ON=1
  k_gt "$K5_TR" "$(awk -v v="$K5_BL" 'BEGIN { print v + 0.5 }')" && die "the top-right corner brightened too ($K5_TR vs $K5_BL): the tag is not confined to one corner"
  echo "[e2e]   the tag is burned into the bottom-right corner of the card's copy and nowhere else"
  # AND NOWHERE ELSE AT ALL. The upload is what this clip streams (a playable faststart mp4), so
  # /video.mp4 is the Original tier — the same corner must be untouched.
  code=$(media_download "$BASE/api/clips/$K5/video.mp4" "$E2E_DIR/K5-video.mp4")
  expect "$code" 200 "download K5 video.mp4"
  K5_VBR=$(k_yavg "$E2E_DIR/K5-video.mp4" "420:40:in_w-420:in_h-40")
  k_gt "$K5_VBR" "$(awk -v v="$K5_BL" 'BEGIN { print v + 0.5 }')" \
    && die "the file a VIEWER streams carries the tag ($K5_VBR); it is the Light copy's alone"
  echo "[e2e]   …and the file a viewer streams is untouched (bottom-right=$K5_VBR)"
  # NOR THE CARD IMAGE (§20 erratum 219). The poster is the og:image, the still this site's own
  # player shows before play, and a JPEG anybody can download — none of which the amendment
  # covers — so it is cut from an UNTAGGED frame and borrows only the card copy's SIZE. (The file and
  # its box were asserted above, where they hold regardless of the tag; only the pixels belong here.)
  K5_PBR=$(k_yavg "$E2E_DIR/K5-poster.jpg" "420:40:in_w-420:in_h-40")
  K5_PBL=$(k_yavg "$E2E_DIR/K5-poster.jpg" "420:40:0:in_h-40")
  k_gt "$K5_PBR" "$(awk -v v="$K5_PBL" 'BEGIN { print v + 0.5 }')" \
    && die "the CARD IMAGE carries the tag (bottom-right=$K5_PBR vs bottom-left=$K5_PBL)"
  echo "[e2e]   …and the card image is untagged too (bottom-right=$K5_PBR bottom-left=$K5_PBL) at $K5_LDIMS"
  # …but it DOES carry its own duration, burned into the TOP-right (§20 erratum 287). That is not
  # the LIGHT tag and it is not a watermark: it carries no brand and no name, it is one
  # measurement of the file it is drawn on, and it is gone the moment anybody presses play.
  # Measured here rather than in its own step because this is the one fixture in the suite whose
  # picture is BLACK end to end, so a corner's mean luminance means something; K_TAG_ON is the
  # proof that this image has drawtext AND a font, which is exactly what the label needs too (the
  # poster's font list falls back to the LIGHT tag's). The text is #ECE9E2 on a 72 % dark plate,
  # so on a black frame the top-right corner BRIGHTENS and the top-left does not.
  K5_PTR=$(k_yavg "$E2E_DIR/K5-poster.jpg" "220:90:in_w-220:0")
  K5_PTL=$(k_yavg "$E2E_DIR/K5-poster.jpg" "220:90:0:0")
  k_gt "$K5_PTR" "$(awk -v v="$K5_PTL" 'BEGIN { print v + 0.5 }')" \
    || die "the card image carries no duration in its top-right corner (top-right=$K5_PTR vs top-left=$K5_PTL); §20 erratum 287"
  echo "[e2e]   …and it carries its own duration top-right (top-right=$K5_PTR top-left=$K5_PTL)"
else
  echo "[e2e]   NOTE: no tag measured (LIGHT_TAG_TEXT off, or no font/drawtext in this image); the pixels of the tag AND of the poster's duration label are pinned by the -tags ffmpeg tests"
fi

# THE CARD'S DESCRIPTION states both real qualities when a Light copy serves the card (§20 erratum 214).
code=$(page "$BASE/$K5" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot GET K5"
has_file "$PAGE" 'Preview 720p30' "the card description names the preview quality"
has_file "$PAGE" 'Original 1080p60 at this link' "…and the quality the link leads to, with the tier's own word"
# The card tag INVENTORY is unchanged by any of it: these are values (§9.1).
code=$(get "$BASE/api/clips/$K5"); expect "$code" 200 "GET K5"
cp "$RESP" "$E2E_DIR/capture-K5.json"
K5_PW=$(jget "$(W "$E2E_DIR/capture-K5.json")" poster.width)
K5_PH=$(jget "$(W "$E2E_DIR/capture-K5.json")" poster.height)
code=$(page "$BASE/$K5" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot GET K5 for the inventory check"
card_check player "$PAGE" "$BASE" "$K5" "E2E light tag" "$K5_PW" "$K5_PH" 1280 720 "$SECURE"

k_delete "$K5" K5
k_delete "$K1" K1
rm -f "$K_BLACK"
summary_row K5 "$K5" "light tag (measured=$K_TAG_ON) + the card's two-quality description, deleted"
summary_row K1 "$K1" "capture upload held for the card: card_ready=$K1_CARD in ${K_ELAPSED}s, deleted"
