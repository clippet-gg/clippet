# 103-audio.sh — a clip can be SOUND (DESIGN §20 erratum 328; run 8 lane `audiocore`). Sourced by
# scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md). Step 103.
#
# What this step proves on a live stack, for the audio matrix scripts/testclips.sh makes when asked
# (TESTCLIPS_AUDIO=1: an MP3 with ID3 and a cover, an M4A with moov last and a location, WAV 16/44.1,
# WAV 24/96, a +3 dBFS 32-bit float WAV, FLAC 24/48, an Ogg Opus voice note, a WebM Opus (a browser's
# recording) and an MKA FLAC — both stored out of Matroska, as an .ogg and a .flac —, AIFF, ALAC, a cover-only
# "video", a 5.1 FLAC, a WAV past 25 MiB and a DSD-shaped file):
#   - every one but the DSD is kind `audio`, with the tier states the decision table says — the Original
#     plays (MP3, AAC, Opus, FLAC) or a Full is made (FLAC for PCM/AIFF/ALAC, FLAC 24-bit for float);
#   - GET /api/clips/{id}/audio serves it under its REAL media type, and every served copy — /audio, the
#     owner's /original and the card copy /video.mp4 — carries no tag beyond an MP4's structural boxes,
#     no attached picture and no encoder string (host ffprobe);
#   - on every lossless path the decoded PCM of what is served hashes EQUAL to the fixture's (host
#     ffmpeg), and on the float path it DIFFERS, the label says `24-bit`, and the decoded peak is under
#     0 dBFS (no clipping);
#   - the card copy is H.264 + AAC, stereo for the 5.1 source, under SOCIAL_MAX_BYTES for the big WAV;
#   - the DSD file is a 415; the same MP3 twice is ONE clip (erratum 326's dedupe, on the strip's hash);
#   - the share page holds an <audio>, and trim answers 409 not_for_audio.
# The bot leg (an `audio/ogg` attachment through /upload and Enrich) needs a fetch from Discord's CDN,
# which the suite never makes (85-bot's header); it is pinned in Go (internal/httpapi
# TestIsMediaTakesAVoiceMessage, TestAudioUploadStoresTheStrip).
#
# e2e-schedule: parallel
# e2e-namespace: account=au prefix=AU_ buckets=-
# e2e-covers: internal/transcode/audio.go internal/worker/audio.go internal/httpapi/audio.go
# e2e-covers: internal/transcode/probe.go internal/transcode/plan.go internal/transcode/tiers.go
#   It creates its OWN account (`bundle_account au`), uploads only as it, and deletes every clip it made.
#   Its fixtures are generated into testclips/audio/ and the ones THIS run wrote (testclips/audio/.made)
#   are removed at the end. It reads no shared clip, restarts and reconfigures nothing, asserts nothing
#   bucket-, log- or instance-wide (every count is a clip's own), and drains no limiter.
#
# Variables are prefixed AU_ so nothing here can clobber a harness global.

step 103 "audio (§20 erratum 328): kind audio, the tier table, /audio's real types, no tag on any served copy, PCM hash equality on every lossless path and inequality + 24-bit + no clipping on float, the card copy, DSD refused, dedupe on audio, the page's <audio>"

AU_DIR="${CTX_DIR:-$E2E_DIR}/audio"
mkdir -p "$AU_DIR"
AU_SRC="$PWD/testclips/audio"
AU_T0=$SECONDS
TESTCLIPS_AUDIO=1 TESTCLIPS_ONLY=none bash scripts/testclips.sh "$PWD/testclips" > "$AU_DIR/gen.log" 2>&1 \
  || die "the audio matrix could not be made: $(tail -3 "$AU_DIR/gen.log")"
echo "[e2e]   audio matrix: $(grep -c '^  [+=] audio/' "$AU_DIR/gen.log") fixtures in $((SECONDS - AU_T0)) s"

bundle_account au; AU_JARW=$BA_JARW

# au_up <file> <part type> [extra curl args…] — POST /api/upload (the capture grammar) as the bundle's
# account. A plain curl: a write is not retried.
au_up() {
  local f=$1 ct=$2; shift 2
  curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$AU_JARW" -H "$E2E_APP_HDR" "$@" \
    -F "file=@$(W "$AU_SRC/$f");type=$ct" "$BASE/api/upload"
}
# au_notags <host file> <what> — no tag but an MP4's structural boxes, no picture, no encoder.
au_notags() {
  local f=$1 what=$2 tags pics
  tags=$(ffprobe -v error -show_entries format_tags:stream_tags -of default=nw=1 "$(W "$f")" | tr -d '\r' \
    | grep -Ev '^TAG:(major_brand|minor_version|compatible_brands|language|handler_name|vendor_id)=' || true)
  [ -z "$tags" ] || die "$what carries tags: $(printf '%s' "$tags" | tr '\n' ' ')"
  pics=$(ffprobe -v error -show_entries stream_disposition=attached_pic -of csv=p=0 "$(W "$f")" | tr -d '\r' | grep -c '^1' || true)
  expect "$pics" 0 "$what's attached pictures"
}
# au_pcm <host file> [float] — the sha256 of the first audio stream's decoded samples (32-bit).
au_pcm() {
  local codec=pcm_s32le; [ "${2:-}" = float ] && codec=pcm_f32le
  ffmpeg -hide_banner -loglevel error -nostdin -i "$(W "$1")" -map 0:a:0 -c:a "$codec" -f hash -hash sha256 - | tr -d '\r' | sed 's/^SHA256=//'
}
# au_peak <host file> — the overall sample peak in dBFS.
au_peak() {
  ffmpeg -hide_banner -nostdin -i "$(W "$1")" -map 0:a:0 -af astats=measure_overall=Peak_level:measure_perchannel=none \
    -f null - 2>&1 | tr -d '\r' | sed -n 's/.*Peak level dB: *//p' | tail -1
}

AU_IDS=""
# THE UPLOADS GO TOGETHER (run 9, lane `pool`): the fourteen used to be one capture POST, one wait_ready and
# one round of checks each, in turn — and the capture route HOLDS its reply up to CAPTURE_READY_WAIT for the
# card, so each upload also sat out its own encode before the next one was even sent. Now all fourteen are
# submitted at once, then waited for in order (the queue runs them in arrival order, so by the time the
# first is ready the later ones are ready or close, and each later wait_ready is a poll or two — the
# `poll` bucket sees what ONE wait_ready always spent; the batched progress route would have drained the
# `view` bucket 43-watch and 70-web poll), then checked one by one exactly as before. The upload limiters
# admit fourteen at once: their burst is the hourly figure, which the run raises to 600 per account (this
# bundle's own `au`) and 1200 per address (scripts/e2e.sh step 1).
#
# au_cases — one fixture a line: <fixture>|<part type>|<playable>|<full codec>|<served type>|<lossless yes|no|float>
au_cases() {
  cat <<'AUCASES'
mp3-id3-cover.mp3|audio/mpeg|true||audio/mpeg|no
m4a-moovlast.m4a|audio/x-m4a|true||audio/mp4|no
wav-16-44.wav|audio/x-wav|false|flac|audio/flac|yes
wav-24-96.wav|audio/wav|false|flac|audio/flac|yes
wav-32f-hot.wav|audio/wav|false|flac24|audio/flac|float
flac-24-48.flac|audio/flac|true||audio/flac|yes
opus-voice.ogg|audio/ogg|true||audio/ogg|no
webm-opus.webm|audio/webm|true||audio/ogg|no
mka-flac.mka|audio/x-matroska|true||audio/flac|yes
aiff-16.aiff|audio/aiff|false|flac|audio/flac|yes
alac.m4a|audio/mp4|false|flac|audio/flac|yes
cover-only.mp4|video/mp4|true||audio/mp4|no
flac-51.flac|audio/flac|true||audio/flac|yes
wav-big.wav|audio/wav|false|flac|audio/flac|yes
AUCASES
}
# (Matroska/WebM is never stored as one: the browser recording becomes an Ogg that PLAYS, the MKA FLAC a flac.)
# 1. Every upload at once, each in a subshell of its own writing its own answer files.
# The loops read the table from a variable, never from stdin: a command inside one that drained stdin
# (ffmpeg without -nostdin, a `docker compose exec`) would swallow the rest of the table in silence.
AU_T1=$SECONDS; AU_N=0
for AU_L in $(au_cases); do
  AU_F=${AU_L%%|*}; AU_CT=${AU_L#*|}; AU_CT=${AU_CT%%|*}
  AU_N=$((AU_N + 1))
  ( c=$(curl -sS -o "$(W "$AU_DIR/up-$AU_N.json")" -w '%{http_code}' -b "$AU_JARW" -H "$E2E_APP_HDR" \
        -F "file=@$(W "$AU_SRC/$AU_F");type=$AU_CT" "$BASE/api/upload" 2> "$AU_DIR/up-$AU_N.err") || c="000 (curl exit $?)"
    printf '%s' "$c" > "$AU_DIR/up-$AU_N.code" ) < /dev/null &
done
wait
echo "[e2e]   $AU_N uploads submitted at once, every reply in after $((SECONDS - AU_T1)) s"
# 2. Every answer, then every clip ready, in the order they were sent.
AU_N=0; AU_ORDER=""
for AU_L in $(au_cases); do
  AU_F=${AU_L%%|*}
  AU_N=$((AU_N + 1))
  expect "$(cat "$AU_DIR/up-$AU_N.code")" 201 "upload $AU_F ($(cat "$AU_DIR/up-$AU_N.json" "$AU_DIR/up-$AU_N.err" 2>/dev/null))"
  AU_ID=$(jget "$(W "$AU_DIR/up-$AU_N.json")" id); expect_nonempty "$AU_ID" "$AU_F's id"
  expect "$(jget "$(W "$AU_DIR/up-$AU_N.json")" kind)" audio "$AU_F's kind on the capture answer"
  AU_IDS="$AU_IDS $AU_ID"; AU_ORDER="$AU_ORDER $AU_ID"
done
set -- $AU_ORDER
for AU_L in $(au_cases); do wait_ready "$1" "audio ${AU_L%%|*}"; shift; done
echo "[e2e]   all $AU_N audio clips ready $((SECONDS - AU_T1)) s after the first upload"
# au_check <id> <fixture> <playable> <full codec> <served type> <lossless yes|no|float> — the checks every
# fixture always had, on a clip that is ready.
au_check() {
  local id=$1 f=$2 playable=$3 full=$4 served=$5 lossless=$6 code out
  code=$(get "$BASE/api/clips/$id" -b "$AU_JARW"); expect "$code" 200 "GET $f's clip"
  expect "$(json kind)" audio "$f's kind"
  expect_empty "$(json width)" "$f has no picture of its own (width)"
  expect "$(json tiers.0.playable)" "$playable" "$f's Original playable"
  expect "$(json tiers.2.state)" ready "$f's card copy"
  if [ -n "$full" ]; then
    expect "$(json tiers.1.state)" ready "$f's Full"; expect "$(json tiers.1.codec)" "$full" "$f's Full codec"
  else
    expect "$(json tiers.1.state)" skipped "$f's Full (the Original plays)"
  fi
  # /audio: the best playable tier under its real media type, then every tag check on it.
  out="$AU_DIR/$id.audio"
  code=$(media_download "$BASE/api/clips/$id/audio" "$out"); expect_media "$code" 200 "GET /audio of $f"
  expect "$(hdr Content-Type)" "$served" "/audio's media type for $f"
  au_notags "$out" "/audio of $f"
  case $lossless in
    yes)   expect "$(au_pcm "$out")" "$(au_pcm "$AU_SRC/$f")" "the decoded PCM of $f's /audio against the fixture" ;;
    float) [ "$(au_pcm "$out" float)" != "$(au_pcm "$AU_SRC/$f" float)" ] || die "a 24-bit FLAC of a float source decodes to the source's samples"
           AU_PK=$(au_peak "$out")
           case $AU_PK in -*) ;; *) die "the float Full peaks at '$AU_PK' dBFS: clipped (want under 0)" ;; esac ;;
  esac
  # The owner's /original: the stored strip, no tags either.
  code=$(curl_ -sS -o "$(W "$AU_DIR/$id.orig")" -D "$HDRW" -w '%{http_code}' -b "$AU_JARW" "$BASE/api/clips/$id/original?download=1")
  expect "$code" 200 "the owner's /original of $f"
  au_notags "$AU_DIR/$id.orig" "/original of $f"
  [ "$lossless" = yes ] && expect "$(au_pcm "$AU_DIR/$id.orig")" "$(au_pcm "$AU_SRC/$f")" "the decoded PCM of $f's stored original"
  # The card copy: H.264 + AAC, no tags.
  code=$(media_download "$BASE/api/clips/$id/video.mp4" "$AU_DIR/$id.card"); expect_media "$code" 200 "the card copy of $f"
  expect "$(ffprobe -v error -show_entries stream=codec_name -of csv=p=0 "$(W "$AU_DIR/$id.card")" | tr -d '\r' | tr '\n' ' ')" "h264 aac " "$f's card copy streams"
  au_notags "$AU_DIR/$id.card" "the card copy of $f"
  echo "[e2e]   $f → $id: original playable=$playable, full=${full:-none}, /audio $served, no tags, lossless=$lossless"
}
# 3. The checks, every fixture at once. They are the bundle's real cost on a quiet stack (downloads, three
#    ffprobes and up to four PCM decodes each, on the host), and each fixture's are independent of every
#    other's: so each runs in a subshell with a request context of its own (e2e_ctx — the response files a
#    `get` writes are per context, so two checks never read each other's answers), its log is printed in
#    fixture order afterwards, and any one that died fails the step with its own message above.
AU_T3=$SECONDS; AU_N=0; AU_PIDS=""
set -- $AU_ORDER
for AU_L in $(au_cases); do
  IFS='|' read -r AU_F AU_CT AU_PL AU_FULL AU_SV AU_LL <<< "$AU_L"
  AU_N=$((AU_N + 1))
  ( e2e_ctx "103-audio/check-$AU_N"; au_check "$1" "$AU_F" "$AU_PL" "$AU_FULL" "$AU_SV" "$AU_LL" ) \
    > "$AU_DIR/check-$AU_N.log" 2>&1 < /dev/null &
  AU_PIDS="$AU_PIDS $!"
  case $AU_F in
    mp3-id3-cover.mp3) AU_MP3=$1 ;;
    wav-32f-hot.wav)   AU_FLOAT=$1 ;;
    flac-51.flac)      AU_51=$1 ;;
    wav-big.wav)       AU_BIG=$1 ;;
  esac
  shift
done
# `wait <pid>` for each, so every check's own exit status is read (a bare `wait` would return 0).
AU_BAD=""; AU_K=0
for AU_PID in $AU_PIDS; do
  AU_K=$((AU_K + 1))
  wait "$AU_PID" || AU_BAD="$AU_BAD $AU_K"
done
AU_K=0
while [ "$AU_K" -lt "$AU_N" ]; do AU_K=$((AU_K + 1)); cat "$AU_DIR/check-$AU_K.log"; done
rm -rf "$E2E_DIR/ctx-103-audio/check-"*
[ -z "$AU_BAD" ] || die "the checks of fixture(s)$AU_BAD failed (their lines above, in fixture order)"
echo "[e2e]   the $AU_N fixtures' checks ran at once, all green in $((SECONDS - AU_T3)) s"

# The 5.1 source's card copy is stereo; the big WAV's is under SOCIAL_MAX_BYTES.
expect "$(ffprobe -v error -select_streams a:0 -show_entries stream=channels -of csv=p=0 "$(W "$AU_DIR/$AU_51.card")" | tr -d '\r')" 2 "the 5.1 FLAC's card copy channels"
AU_BIGCARD=$(wc -c < "$AU_DIR/$AU_BIG.card" | tr -d ' ')
[ "$AU_BIGCARD" -lt $((25 * 1024 * 1024)) ] || die "the big WAV's card copy is $AU_BIGCARD bytes, past SOCIAL_MAX_BYTES"
echo "[e2e]   the 5.1 FLAC's card copy is stereo; the 28.8 MB WAV's is $AU_BIGCARD bytes"

# DSD is refused (D8a), with nothing made.
code=$(au_up dsd.dsf audio/x-dsf); expect "$code" 415 "the DSD upload ($(cat "$RESP"))"
expect "$(json error)" not_a_video "the DSD refusal's code"

# The same MP3 twice is one clip (erratum 326 on the strip's hash).
code=$(au_up mp3-id3-cover.mp3 audio/mpeg); expect "$code" 200 "the same MP3 again ($(cat "$RESP"))"
expect "$(json id)" "$AU_MP3" "the duplicate MP3 answers the first clip"
expect "$(json existing)" true "the duplicate MP3's answer says existing"

# The share page: an <audio> playing /audio; the float clip's label says 24-bit; trim is not for audio.
code=$(page "$BASE/$AU_MP3"); expect "$code" 200 "the MP3's share page"
contains "$(cat "$PAGE")" "<audio controls preload=\"metadata\" src=\"/api/clips/$AU_MP3/audio\"" || die "the MP3's page holds no <audio> on /audio"
contains "$(cat "$PAGE")" "Original · MP3" || die "the MP3's page does not say Original · MP3"
code=$(page "$BASE/$AU_FLOAT"); expect "$code" 200 "the float WAV's share page"
contains "$(cat "$PAGE")" "Full · FLAC 24-bit" || die "the float WAV's page does not say 24-bit"
code=$(http POST "$BASE/api/clips/$AU_MP3/trim" -b "$AU_JARW" -H 'Content-Type: application/json' -d '{"start_ms":0,"end_ms":1000}')
expect "$code" 409 "trim of an audio clip ($(cat "$RESP"))"; expect "$(json error)" not_for_audio "trim's refusal"
echo "[e2e]   DSD → 415; the same MP3 twice → $AU_MP3 (existing); the page has an <audio> on /audio and the words; trim → not_for_audio"

# --- the litter this step made, gone ---------------------------------------------------------------
for AU_ID in $AU_IDS; do
  code=$(http DELETE "$BASE/api/clips/$AU_ID" -b "$AU_JARW"); expect_deleted "$code" "$AU_ID" "delete the audio clip $AU_ID"
done
# The waits too, at once (each in a context of its own, as above): a gone clip is a gone clip whichever
# order they are confirmed in.
AU_N=0; AU_PIDS=""
for AU_ID in $AU_IDS; do
  AU_N=$((AU_N + 1))
  ( e2e_ctx "103-audio/gone-$AU_N"; wait_gone "$AU_ID" "the audio clip $AU_ID" ) > "$AU_DIR/gone-$AU_N.log" 2>&1 < /dev/null &
  AU_PIDS="$AU_PIDS $!"
done
AU_BAD=""; AU_K=0
for AU_PID in $AU_PIDS; do
  AU_K=$((AU_K + 1))
  wait "$AU_PID" || AU_BAD="$AU_BAD $AU_K"
done
cat "$AU_DIR"/gone-*.log
rm -rf "$E2E_DIR/ctx-103-audio/gone-"*
[ -z "$AU_BAD" ] || die "the audio clip wait(s)$AU_BAD did not end in 404 + an empty prefix (above)"

if [ -f "$AU_SRC/.made" ]; then
  while IFS= read -r AU_F; do
    case $AU_F in ''|*/*|.*) continue ;; esac   # a bare file name this run wrote, nothing else
    rm -f "$AU_SRC/$AU_F"
  done < "$AU_SRC/.made"
  rm -f "$AU_SRC/.made"
fi
rmdir "$AU_SRC" 2>/dev/null || true
rm -rf "$AU_DIR"

summary_row AU "$AU_MP3" "audio (§20 erratum 328): 14 fixtures kind audio (WebM Opus and MKA FLAC stored as .ogg and .flac) with the decision table's tiers; /audio under its real types; no tag/picture/encoder on /audio, /original or the card copy; PCM hash equal on every lossless path, unequal + 24-bit + peak under 0 dBFS on float; the 5.1 card stereo, the big WAV's card under the cap; DSD 415; the same MP3 twice one clip; <audio> on the page; trim not_for_audio; clips and fixtures removed"
