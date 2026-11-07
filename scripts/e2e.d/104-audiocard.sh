# 104-audiocard.sh — an audio clip's CARD: the drawn waveform, the audiogram, the card tags (DESIGN §20
# erratum 329; run 8 lane `audiocard`). Sourced by scripts/e2e.sh before the step-16 log gate (see
# scripts/e2e.d/README.md). Step 104.
#
# What this step proves on a live stack, for three fixtures it makes itself (an MP3 with ID3 tags and a
# cover, a WAV 24/96, and a WAV past SOCIAL_MAX_BYTES), all with a RISING envelope so the bars differ:
#   - the poster is a PNG of the card frame (1280×720) under image/png, with no tag (host ffprobe);
#   - the audiogram (/video.mp4, the card copy) passes the checks the video Light passes in 62-tiers
#     (probe_check: exactly one H.264 stream and one AAC stream, 1280×720 inside the Light box, 30/1,
#     ftyp first and moov before mdat), carries no tag, lasts the sound's length within one frame, and is
#     the poster at frame 0 (the last bar in the unplayed grey) and amber by the end (an early bar played);
#   - the big WAV's audiogram is under SOCIAL_MAX_BYTES (25 MiB);
#   - the share page carries the card tags — og:video = social.mp4, og:image:type image/png at 1280×720,
#     og:audio = /audio with its type — and the page's own progress data (70 bars of peaks, the geometry);
#   - the component embed's Media Gallery item is the audiogram (social.mp4);
#   - the embed page's <video> plays the audiogram with the waveform poster;
#   - the bars are LOUDNESS (erratum 332): a fourth fixture, `edm.wav` (internal/transcode's
#     waveform_ffmpeg_test.go recipe: a quiet intro, then a full-scale burst in every bar over a bed that
#     drops for a breakdown), reads through the page's own peaks as an intro under every later bar and a
#     brick that is NOT all full — the peak reading drew every bar after the intro at 255.
#
# e2e-schedule: parallel
# e2e-namespace: account=ag prefix=AG_ buckets=-
# e2e-covers: internal/transcode/waveform.go internal/worker/audio.go internal/web/meta.go web/templates/clip.html
# e2e-covers: web/static/app.js
#   It creates its OWN account (`bundle_account ag`), uploads only as it, and deletes every clip it made.
#   Its fixtures are made by the host ffmpeg into its own context directory, NOT into testclips/ (103-audio
#   owns testclips/audio/ and removes it when it is done; the two run side by side), and the directory is
#   removed at the end. It reads no shared clip, restarts and reconfigures nothing, asserts nothing bucket-,
#   log- or instance-wide (every count is a clip's own), and drains no limiter.
#
# Variables are prefixed AG_ so nothing here can clobber a harness global.

step 104 "the audio card (§20 erratum 329): a PNG waveform poster, the audiogram passing the Light's checks (one H.264 1280x720 30 fps + one AAC, faststart, no tag, the sound's length, frame 0 = the poster, amber when played), under the cap for a 28.8 MB WAV, the card tags with og:audio, the component embed's gallery item, the embed's <video>; the bars are loudness (§20 erratum 332): a full-scale burst in every bar does not fill them"

AG_DIR="${CTX_DIR:-$E2E_DIR}/audiocard"
rm -rf "$AG_DIR"; mkdir -p "$AG_DIR/src"
AG_T0=$SECONDS
# A rising sound: a 220 Hz tone that grows from silence to full scale, so the waveform has a shape.
AG_RAMP="aevalsrc=sin(2*PI*220*t)*t/DUR"
ac_make() { # ac_make <file> <seconds> <ffmpeg args…>
  local f=$1 d=$2; shift 2
  ffmpeg -hide_banner -loglevel error -nostdin -y -f lavfi -i "${AG_RAMP//DUR/$d}:s=48000:d=$d" "$@" "$(W "$AG_DIR/src/$f")" \
    > "$AG_DIR/gen-$f.log" 2>&1 || die "the fixture $f could not be made: $(tail -3 "$AG_DIR/gen-$f.log")"
}
ffmpeg -hide_banner -loglevel error -nostdin -y -f lavfi -i "${AG_RAMP//DUR/8}:s=48000:d=8" -f lavfi -i "color=c=red:s=64x64:d=1" \
  -map 0:a -map 1:v -ac 2 -c:a libmp3lame -b:a 192k -c:v mjpeg -disposition:v attached_pic -id3v2_version 3 \
  -metadata "title=E2E secret title" -metadata "artist=E2E artist" "$(W "$AG_DIR/src/song.mp3")" > "$AG_DIR/gen-song.log" 2>&1 \
  || die "the MP3 fixture could not be made: $(tail -3 "$AG_DIR/gen-song.log")"
ac_make w2496.wav 6 -ac 2 -ar 96000 -c:a pcm_s24le -metadata "title=E2E secret title"
# Past SOCIAL_MAX_BYTES (25 MiB): 24/48 stereo for 100 s is 28.8 MB.
ac_make big.wav 100 -ac 2 -ar 48000 -c:a pcm_s24le
# The brick (erratum 332): waveform_ffmpeg_test.go's waveEDMRecipe, the same bytes.
ffmpeg -hide_banner -loglevel error -nostdin -y -f lavfi \
  -i "aevalsrc='if(lt(t,2),0.0316*sin(2*PI*440*t),if(lt(mod(t-2,0.25),0.02),1-2*lt(mod(t,0.02),0.01),if(between(t,10,14),0.125,0.5)*sin(2*PI*110*t)))':s=44100:d=20" \
  -c:a pcm_s16le -fflags +bitexact -flags +bitexact "$(W "$AG_DIR/src/edm.wav")" > "$AG_DIR/gen-edm.log" 2>&1 \
  || die "the brick fixture could not be made: $(tail -3 "$AG_DIR/gen-edm.log")"
echo "[e2e]   audio card fixtures made in $((SECONDS - AG_T0)) s"

bundle_account ag; AG_JARW=$BA_JARW

# ac_rgb <mp4> <seconds> <x> <y> — one pixel of the frame at that time, as "R G B" (BT.709, as stamped).
ac_rgb() {
  ffmpeg -hide_banner -loglevel error -nostdin -ss "$2" -i "$(W "$1")" -frames:v 1 \
    -vf "scale=in_color_matrix=bt709:in_range=tv,crop=1:1:$3:$4,format=rgb24" -f rawvideo - | od -An -tu1 | tr -s ' ' | sed 's/^ //'
}
# ac_near "<R G B>" "<R G B>" <tolerance> — every channel within the tolerance (an empty reading fails).
ac_near() {
  awk -v a="$1" -v b="$2" -v t="$3" 'BEGIN { if (split(a, x, " ") != 3 || split(b, y, " ") != 3) exit 1
    for (i = 1; i <= 3; i++) { d = x[i] - y[i]; if (d < 0) d = -d; if (d > t) exit 1 } }'
}
# ac_notags <host file> <what> — no tag but an MP4's structural boxes, no attached picture.
ac_notags() {
  local tags pics
  tags=$(ffprobe -v error -show_entries format_tags:stream_tags -of default=nw=1 "$(W "$1")" | tr -d '\r' \
    | grep -Ev '^TAG:(major_brand|minor_version|compatible_brands|language|handler_name|vendor_id)=' || true)
  [ -z "$tags" ] || die "$2 carries tags: $(printf '%s' "$tags" | tr '\n' ' ')"
  pics=$(ffprobe -v error -show_entries stream_disposition=attached_pic -of csv=p=0 "$(W "$1")" | tr -d '\r' | grep -c '^1' || true)
  expect "$pics" 0 "$2's attached pictures"
}

AG_IDS=""
AG_REST="169 165 156"     # WaveRestHex   #A9A59C
AG_PLAYED="240 178 74"    # WavePlayedHex #F0B24A
# ac_case <fixture> <part type> <seconds> <og:audio:type>
ac_case() {
  local f=$1 ct=$2 secs=$3 atype=$4 code id dur rgb
  code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$AG_JARW" -H "$E2E_APP_HDR" \
    -F "file=@$(W "$AG_DIR/src/$f");type=$ct" "$BASE/api/upload")
  expect "$code" 201 "upload $f ($(cat "$RESP"))"
  id=$(json id); expect_nonempty "$id" "$f's id"; AG_IDS="$AG_IDS $id"
  wait_ready "$id" "audio card $f"
  code=$(get "$BASE/api/clips/$id" -b "$AG_JARW"); expect "$code" 200 "GET $f's clip"
  expect "$(json kind)" audio "$f's kind"

  # The poster: a PNG of the card frame, no tag.
  code=$(media_download "$BASE/api/clips/$id/poster.jpg" "$AG_DIR/$id.png"); expect_media "$code" 200 "GET $f's poster"
  expect "$(hdr Content-Type)" image/png "$f's poster media type"
  expect "$(head -c 4 "$AG_DIR/$id.png" | od -An -tx1 | tr -d ' \n')" 89504e47 "$f's poster signature"
  expect "$(ffprobe -v error -show_entries stream=codec_name,width,height -of csv=p=0 "$(W "$AG_DIR/$id.png")" | tr -d '\r')" "png,1280,720" "$f's poster"
  ac_notags "$AG_DIR/$id.png" "$f's poster"

  # The audiogram: the video Light's checks (62-tiers), no tag, the sound's length, frame 0 = the poster.
  code=$(media_download "$BASE/api/clips/$id/video.mp4" "$AG_DIR/$id.mp4"); expect_media "$code" 200 "GET $f's audiogram"
  ffprobe -v error -print_format json -show_format -show_streams "$(W "$AG_DIR/$id.mp4")" > "$AG_DIR/$id.json" \
    || die "ffprobe of $f's audiogram"
  probe_check "$AG_DIR/$id.json" "$AG_DIR/$id.mp4" 1 1280 720 30/1 1280x720 2
  ac_notags "$AG_DIR/$id.mp4" "$f's audiogram"
  dur=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$(W "$AG_DIR/$id.mp4")" | tr -d '\r')
  awk -v d="$dur" -v s="$secs" 'BEGIN { x = d - s; if (x < 0) x = -x; exit !(x <= 0.034) }' \
    || die "$f's audiogram lasts $dur s, the sound $secs s (more than one frame apart)"
  rgb=$(ac_rgb "$AG_DIR/$id.mp4" 0 1192 360)
  ac_near "$rgb" "$AG_REST" 12 || die "$f's audiogram frame 0 at the last bar is '$rgb', want the poster's grey ($AG_REST)"
  rgb=$(ac_rgb "$AG_DIR/$id.mp4" "$(awk -v s="$secs" 'BEGIN { printf "%.3f", s - 0.2 }')" 584 360)
  ac_near "$rgb" "$AG_PLAYED" 14 || die "$f's audiogram near its end at a played bar is '$rgb', want amber ($AG_PLAYED)"

  # The card tags, the page's own progress data, the component embed's gallery item.
  code=$(page "$BASE/$id"); expect "$code" 200 "$f's share page"
  has_file_f "$PAGE" "<meta property=\"og:video\" content=\"$BASE/api/clips/$id/social.mp4\">" "$f's og:video (the audiogram)"
  has_file_f "$PAGE" '<meta property="og:image:type" content="image/png">' "$f's og:image:type"
  has_file_f "$PAGE" '<meta property="og:image:width" content="1280">' "$f's og:image:width"
  has_file_f "$PAGE" "<meta property=\"og:audio\" content=\"$BASE/api/clips/$id/audio\">" "$f's og:audio"
  has_file_f "$PAGE" "<meta property=\"og:audio:type\" content=\"$atype\">" "$f's og:audio:type"
  grep -q 'data-wave="1280,720,80,160,1120,400,10,6,10" data-peaks="[0-9a-f]\{140\}"' "$PAGE" \
    || die "$f's page carries no waveform data for its own progress"
  has_file_f "$PAGE" '<script id="discord:component-embed"' "$f's component embed"
  has_file_f "$PAGE" "\"url\":\"$BASE/api/clips/$id/social.mp4\"" "$f's component embed gallery item (the audiogram)"

  # The embed page plays the audiogram, with the waveform poster.
  code=$(page "$BASE/embed/$id"); expect "$code" 200 "$f's embed page"
  has_file_f "$PAGE" "poster=\"/api/clips/$id/poster.jpg\" src=\"/api/clips/$id/video.mp4\"" "$f's embed <video> (the audiogram)"
  AG_LAST=$id
  echo "[e2e]   $f → $id: PNG poster 1280x720, audiogram $(wc -c < "$AG_DIR/$id.mp4" | tr -d ' ') bytes / $dur s, the card tags, the gallery item, the embed"
}

ac_case song.mp3  audio/mpeg 8   audio/mpeg; AG_MP3=$AG_LAST
ac_case w2496.wav audio/wav  6   audio/flac
ac_case big.wav   audio/wav  100 audio/flac; AG_BIG=$AG_LAST
AG_BIGCARD=$(wc -c < "$AG_DIR/$AG_BIG.mp4" | tr -d ' ')
[ "$AG_BIGCARD" -lt $((25 * 1024 * 1024)) ] || die "the big WAV's audiogram is $AG_BIGCARD bytes, past SOCIAL_MAX_BYTES"
echo "[e2e]   the 28.8 MB WAV's audiogram is $AG_BIGCARD bytes, under the 25 MiB cap"

# --- the bars are loudness (§20 erratum 332), read from the payload the page draws ---------------------
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$AG_JARW" -H "$E2E_APP_HDR" \
  -F "file=@$(W "$AG_DIR/src/edm.wav");type=audio/wav" "$BASE/api/upload")
expect "$code" 201 "upload edm.wav ($(cat "$RESP"))"
AG_EDM=$(json id); expect_nonempty "$AG_EDM" "edm.wav's id"; AG_IDS="$AG_IDS $AG_EDM"
wait_ready "$AG_EDM" "audio card edm.wav"
code=$(page "$BASE/$AG_EDM"); expect "$code" 200 "edm.wav's share page"
AG_PEAKS=$(sed -n 's/.*data-peaks="\([0-9a-f]\{140\}\)".*/\1/p' "$PAGE" | head -1)
expect_nonempty "$AG_PEAKS" "edm.wav's page peaks"
# Bars 0-5 are the intro, 8-69 the brick (bar 6 straddles the 2 s cut, 7 its first burst).
AG_BARS=$(for ((AG_I = 0; AG_I < 140; AG_I += 2)); do printf '%d ' "$((16#${AG_PEAKS:AG_I:2}))"; done)
awk -v bars="$AG_BARS" 'BEGIN { n = split(bars, h, " "); if (n != 70) exit 1
    for (j = 1; j <= 6; j++) if (h[j] > intro) intro = h[j]
    low = 255; full = 1
    for (j = 9; j <= 70; j++) { if (h[j] < low) low = h[j]; if (h[j] < 250) full = 0 }
    exit !(intro < low && !full) }' \
  || die "edm.wav's bars are not loudness (want the intro under every brick bar and the brick not all >= 250): $AG_BARS"
echo "[e2e]   edm.wav → $AG_EDM: the bars are loudness — $AG_BARS"

# --- the litter this step made, gone ---------------------------------------------------------------
for AG_ID in $AG_IDS; do
  code=$(http DELETE "$BASE/api/clips/$AG_ID" -b "$AG_JARW"); expect_deleted "$code" "$AG_ID" "delete the audio clip $AG_ID"
done
for AG_ID in $AG_IDS; do wait_gone "$AG_ID" "the audio clip $AG_ID"; done
rm -rf "$AG_DIR"

summary_row AG "$AG_MP3" "the audio card (§20 erratum 329): MP3 + WAV 24/96 + a 28.8 MB WAV — PNG waveform poster 1280x720, the audiogram (H.264 1280x720 30 fps + AAC, faststart, no tag, the sound's length, frame 0 the poster, amber when played, the big one under the cap), og:audio + og:image:type image/png, the page's peaks, the gallery item, the embed's <video>; the brick's bars are loudness, not all full (332); clips and fixtures removed"
