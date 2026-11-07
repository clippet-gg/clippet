# 112-rotated.sh — a phone recording's shape through both encode paths (DESIGN.md §20 erratum 352; erratum 351's
# audio-first layout in the same fixture). Sourced by scripts/e2e.sh before the step-16 log gate (see
# scripts/e2e.d/README.md). Step 112.
#
# The fixture is testclips/rotated90-audiofirst.mp4: coded PORTRAIT 720x1280 at 60 fps, a Display Matrix rotation
# of 90 (displayed 1280x720), audio stream 0 and video stream 1. Two uploads of it:
#   RT1 — the mp4 as it is: playable, moov last, so it is REMUXED and the Light copy (60 fps wants one) is encoded
#         FROM THE REMUX — the owner's /clip of 2026-10-29, the path both errata broke.
#   RT2 — the same picture as HEVC in mp4 (built here, a stream copy behind an ultrafast encode, coded 360x640 for 2 s): unplayable, so ONE
#         pass writes the Full AND the Light (§20 erratum 217) — the Full of a turned source had the same defect.
# Every file the world can fetch is MEASURED with the host ffprobe: landscape frames (1280x720; RT2 640x360) and NO Display
# Matrix (a matrix left on a turned picture makes every player turn it a second time — a portrait, squeezed card).
# On a stack with HWACCEL=cuda (a Windows GPU box's gate) this is the proof of the CUDA graph (`-display_rotation:v 0`, the
# scale to the coded orientation, the turn after hwdownload); on a CPU stack it proves the software path.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=RT_ buckets=-
# e2e-covers: internal/transcode/encode.go internal/transcode/hwaccel.go internal/transcode/plan.go
# e2e-covers: internal/transcode/probe.go internal/worker/pipeline.go
#   It uploads as the run's owner (two clips of its own, deleted at the end), reads nothing shared, restarts and
#   reconfigures nothing. Its HEVC fixture lives under its own directory and is removed at the end.
#
# Variables are prefixed RT_ so nothing here can clobber a harness global.

step 112 "a phone recording (matrix rotation 90, audio first): the Light from the remux, and the Full + Light of one pass, are landscape with no display matrix (§20 errata 351, 352)"

RT_DIR="${CTX_DIR:-$E2E_DIR}/rotated"
rm -rf "$RT_DIR"; mkdir -p "$RT_DIR"
RT_SRC="$PWD/testclips/rotated90-audiofirst.mp4"
[ -s "$RT_SRC" ] || die "testclips/rotated90-audiofirst.mp4 is missing (scripts/testclips.sh)"

# rt_probe <file> → "WxH|<side data types>" of the first video stream: the frame size and every side data entry.
rt_probe() {
  local wh sd
  # csv prints the (empty) side data subsection as one more separator on a file that has side data: "720x1280x"
  wh=$(ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0:s=x "$(W "$1")" | tr -d '\r' | head -n 1 | sed 's/x*$//')
  sd=$(ffprobe -v error -select_streams v:0 -show_entries stream_side_data=side_data_type -of csv=p=0 "$(W "$1")" | tr -d '\r' | tr '\n' ',')
  printf '%s|%s' "$wh" "$sd"
}
# rt_tier <json-file> <tier> <field> → one field of one entry of the DTO's `tiers` list.
rt_tier() {
  $PY - "$(W "$1")" "$2" "$3" <<'PYEOF'
import json, sys
with open(sys.argv[1], encoding='utf-8') as fh:
    doc = json.load(fh)
for t in doc.get('tiers') or []:
    if t.get('tier') == sys.argv[2]:
        v = t.get(sys.argv[3])
        print('' if v is None else v)
        sys.exit(0)
sys.exit('no %s tier in %s' % (sys.argv[2], doc.get('tiers')))
PYEOF
}
# rt_wait_light <id> <label> — after the ready flip, poll until the Light tier is `ready` (≤120 s); a `failed`
# Light (erratum 351's ending) dies at once.
rt_wait_light() {
  local id=$1 label=$2 t0=$SECONDS st code
  while :; do
    code=$(get "$BASE/api/clips/$id"); expect "$code" 200 "GET $label"
    cp "$RESP" "$RT_DIR/$label.json"
    st=$(rt_tier "$RT_DIR/$label.json" light state)
    case $st in
      ready) echo "[e2e]   $label light tier ready after $((SECONDS - t0))s"; return 0 ;;
      failed|skipped) die "$label light tier is '$st' — a 60 fps upload wants a card copy, and it must encode" ;;
    esac
    [ $((SECONDS - t0)) -lt 120 ] || die "$label light tier not ready within 120s (state '$st')"
    sleep "$POLL_SLEEP"
  done
}
# rt_assert_landscape <url> <label> <WxH> — download one media route and require WxH frames with no side data.
rt_assert_landscape() {
  local f="$RT_DIR/$2.mp4" got code want=$3
  code=$(media_download "$1" "$f"); expect "$code" 200 "download $2"
  got=$(rt_probe "$f")
  case $got in
    *"Display Matrix"*) die "$2 still carries a Display Matrix ($got): players would turn the picture a second time" ;;
  esac
  expect "${got%%|*}" "$want" "$2's frame size (landscape, turned — not the coded portrait, not squeezed)"
  echo "[e2e]   $2: ${got%%|*}, no display matrix"
}

# The fixture is what it claims: audio first, coded portrait, a matrix of 90.
RT_IN=$(rt_probe "$RT_SRC")
expect "${RT_IN%%|*}" 720x1280 "the fixture's coded size"
case $RT_IN in *"Display Matrix"*) ;; *) die "the fixture carries no Display Matrix ($RT_IN); this step would prove nothing" ;; esac
expect "$(ffprobe -v error -show_entries stream=codec_type -of csv=p=0 "$(W "$RT_SRC")" | tr -d '\r' | head -n 1)" audio "the fixture's stream 0"

# --- RT1: remux + the Light from the remux ----------------------------------------------------------------------
code=$(upload RT1 rotated90-audiofirst.mp4 '{"title":"E2E rotated remux","ttl_days":1}')
expect "$code" 201 "upload RT1 ($(cat "$RESP"))"
RT1=$(json id); expect_nonempty "$RT1" "RT1 id"
wait_ready "$RT1" RT1
rt_wait_light "$RT1" RT1
expect "$(rt_tier "$RT_DIR/RT1.json" full state)" skipped "RT1 full tier (a playable upload is remuxed, not encoded)"
expect "$(json width)x$(json height)" 1280x720 "RT1's display dims"
rt_assert_landscape "$BASE/api/clips/$RT1/light.mp4" RT1-light 1280x720

# --- RT2: HEVC, one pass writes the Full and the Light ----------------------------------------------------------
# Small and short on purpose (coded 360x640, 2 s, still 60 fps so a Light is wanted): the Full is a CRF 18 encode
# at the source's own size, and at the phone fixture's 720x1280 x 6 s it ran past wait_ready's 180 s on a 4-cpu
# CPU lane stack. The geometry under test does not depend on the size.
RT_HEVC_RAW="$RT_DIR/hevc-raw.mp4"; RT_HEVC="$RT_DIR/rotated-hevc.mp4"
ffmpeg -hide_banner -nostdin -loglevel error -y \
  -f lavfi -i "testsrc2=duration=2:size=360x640:rate=60" -f lavfi -i "sine=frequency=490:duration=2" \
  -c:v libx265 -preset ultrafast -x265-params log-level=none -pix_fmt yuv420p -tag:v hvc1 \
  -c:a aac -b:a 128k -shortest "$(W "$RT_HEVC_RAW")" || die "could not encode the hevc fixture"
ffmpeg -hide_banner -nostdin -loglevel error -y -display_rotation 90 -i "$(W "$RT_HEVC_RAW")" \
  -map 0:a -map 0:v -c copy -tag:v hvc1 "$(W "$RT_HEVC")" || die "could not remux the hevc fixture with a matrix"
case $(rt_probe "$RT_HEVC") in 360x640*"Display Matrix"*) ;; *) die "the hevc fixture is $(rt_probe "$RT_HEVC")" ;; esac
printf '%s' '{"title":"E2E rotated hevc","ttl_days":1}' > "$RT_DIR/meta-RT2.json"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "metadata=<$(W "$RT_DIR/meta-RT2.json");type=application/json" \
  -F duplicate=allow -F "video=@$(W "$RT_HEVC");type=video/mp4" "$BASE/api/clips")
expect "$code" 201 "upload RT2 ($(cat "$RESP"))"
RT2=$(json id); expect_nonempty "$RT2" "RT2 id"
wait_ready "$RT2" RT2
rt_wait_light "$RT2" RT2
expect "$(rt_tier "$RT_DIR/RT2.json" full state)" ready "RT2 full tier (hevc is encoded)"
expect "$(json width)x$(json height)" 640x360 "RT2's display dims"
rt_assert_landscape "$BASE/api/clips/$RT2/video.mp4" RT2-full 640x360
rt_assert_landscape "$BASE/api/clips/$RT2/light.mp4" RT2-light 640x360

# Clean up: both clips are this bundle's own, and so is its directory.
for id in "${RT1:-}" "${RT2:-}"; do
  [ -n "$id" ] || continue
  code=$(http DELETE "$BASE/api/clips/$id" -b "$JARW"); expect_deleted "$code" "$id" "DELETE $id"
done
rm -rf "$RT_DIR"

summary_row RT1 "${RT1:-}" "phone shape (coded 720x1280, matrix 90, audio first, 60 fps): remuxed; the Light from the remux is 1280x720 with no matrix; deleted"
summary_row RT2 "${RT2:-}" "the same shape as hevc (coded 360x640, 2 s): one pass wrote the Full and the Light, both 640x360 with no matrix; deleted"
