#!/usr/bin/env bash
# testclips.sh — generate the transcode test-clip matrix with the HOST ffmpeg: 6.1 or newer (WSL's Ubuntu ships 6.1.1,
# Git Bash's is 7.1.1). Measured 2026-10-18 (DESIGN §20 erratum 338): 6.1.1 builds every clip and the audio matrix, and
# each clip's ffprobe summary equals the 7.1.1-made one's.
# Every variant here found a real bug in the reference build; e2e.sh and Phase 2 use them.
# Usage: scripts/testclips.sh [outdir]   (default ./testclips, git-ignored)
set -euo pipefail
# On Linux the line below is a no-op. Under Git Bash the host ffmpeg/ffprobe are NATIVE Windows programs: the paths below reach them only through MSYS's
# argument conversion, which an exported MSYS_NO_PATHCONV=1 switches off (check.sh exports it for itself and runs the
# E2E self-test, which runs this script — the TWELFTH GATE's first check.sh died here with "No such file or directory"
# on the .part name). This script never passes a container path, so the conversion is always wanted: unset it here,
# for this process and every child, whatever the caller exported.
unset MSYS_NO_PATHCONV
OUT="${1:-$(cd "$(dirname "$0")/.." && pwd)/testclips}"
mkdir -p "$OUT"
FF=(ffmpeg -hide_banner -loglevel error -y)
V=(-c:v libx264 -preset veryfast -pix_fmt yuv420p)
A=(-c:a aac -b:a 128k)
D=6   # seconds

# sound <file> — true when ffprobe reads a duration out of it, i.e. the container has a `moov` it can
# find. `-s` was the test before (§20 erratum 290, AUDIT #18) and it accepted anything non-empty: a
# 1-byte plant, or the head of a fixture whose ffmpeg was interrupted mid-write — which is also why
# every fixture is now written to a dot-`.part` name and renamed into place only when ffmpeg exited 0,
# so the final name is either absent or whole.
sound() { [ -s "$1" ] && ffprobe -v error -show_entries format=duration -of csv=p=0 "$1" 2>/dev/null | grep -q '^[0-9]'; }
# TESTCLIPS_ONLY=<name>[,<name>] generates only the named fixtures (the E2E self-test's regeneration pin
# wants one clip, not eleven); unset, everything below is made.
wanted() { case ",${TESTCLIPS_ONLY:-}," in ,,|*",$1,"*) return 0 ;; *) return 1 ;; esac; }
gen() { # name, then ffmpeg args
  local name="$1"; shift
  wanted "$name" || return 0
  if sound "$OUT/$name.mp4"; then echo "  = $name.mp4 (exists)"; return; fi
  [ ! -e "$OUT/$name.mp4" ] || echo "  ! $name.mp4 is not a readable clip — regenerating"
  "${FF[@]}" "$@" -f mp4 "$OUT/.$name.mp4.part"     # -f: the .part name has no extension for ffmpeg to read
  mv -f "$OUT/.$name.mp4.part" "$OUT/$name.mp4"
  echo "  + $name.mp4"
}

echo "[testclips] writing to $OUT"
# 1. standard 720p30 with audio (the baseline)
gen std-720p30 -f lavfi -i "testsrc2=duration=$D:size=1280x720:rate=30" \
  -f lavfi -i "sine=frequency=440:duration=$D" "${V[@]}" "${A[@]}" -shortest
# 2. no audio stream at all (must not blind-map 0:a:0)
gen noaudio-720p30 -f lavfi -i "testsrc2=duration=$D:size=1280x720:rate=30" "${V[@]}" -an
# 3. vertical (short side <=720 AND long side <=1280 box logic)
gen vertical-720x1280 -f lavfi -i "testsrc2=duration=$D:size=720x1280:rate=30" \
  -f lavfi -i "sine=frequency=520:duration=$D" "${V[@]}" "${A[@]}" -shortest
# 4. 60fps 1080p (downscale to box + progress denominator uses OUTPUT fps)
gen hd-1080p60 -f lavfi -i "testsrc2=duration=$D:size=1920x1080:rate=60" \
  -f lavfi -i "sine=frequency=600:duration=$D" "${V[@]}" "${A[@]}" -shortest
# 5. dual audio tracks (game + mic style) — must be merged, not dropped
gen dualaudio-720p30 -f lavfi -i "testsrc2=duration=$D:size=1280x720:rate=30" \
  -f lavfi -i "sine=frequency=440:duration=$D" -f lavfi -i "sine=frequency=880:duration=$D" \
  -map 0:v -map 1:a -map 2:a "${V[@]}" "${A[@]}" -shortest \
  -metadata:s:a:0 title=game -metadata:s:a:1 title=mic

# --- extra edge cases from spec §3 ---
# 6. rotation side-data (phone-style): coded 1280x720 with a 90° Display Matrix → displays 720x1280.
#    Encode, then stream-copy remux with -display_rotation: the matrix lands as side-data entry [1]
#    behind an SEI entry [0] — the exact "scan ALL side-data entries" trap from the spec.
if ! wanted rotated90-sidedata; then :
elif ! sound "$OUT/rotated90-sidedata.mp4"; then
  [ ! -e "$OUT/rotated90-sidedata.mp4" ] || echo "  ! rotated90-sidedata.mp4 is not a readable clip — regenerating"
  "${FF[@]}" -f lavfi -i "testsrc2=duration=$D:size=1280x720:rate=30" \
    -f lavfi -i "sine=frequency=330:duration=$D" "${V[@]}" "${A[@]}" -shortest "$OUT/.rot-src.mp4"
  "${FF[@]}" -display_rotation 90 -i "$OUT/.rot-src.mp4" -c copy -movflags +faststart -f mp4 "$OUT/.rotated90-sidedata.mp4.part"
  mv -f "$OUT/.rotated90-sidedata.mp4.part" "$OUT/rotated90-sidedata.mp4"
  rm -f "$OUT/.rot-src.mp4"; echo "  + rotated90-sidedata.mp4"
else echo "  = rotated90-sidedata.mp4 (exists)"; fi
# 7. anamorphic / non-square SAR: coded 720x576, SAR 16:11 → display ~1047x576
gen anamorphic-sar -f lavfi -i "testsrc2=duration=$D:size=720x576:rate=25" \
  -f lavfi -i "sine=frequency=300:duration=$D" -vf setsar=16/11 "${V[@]}" "${A[@]}" -shortest
# 8. fractional frame rate 29.97 (30000/1001) — never hardcode 30
gen fractional-2997 -f lavfi -i "testsrc2=duration=$D:size=1280x720:rate=30000/1001" \
  -f lavfi -i "sine=frequency=400:duration=$D" "${V[@]}" "${A[@]}" -shortest
# 9. tiny source that must NOT be upscaled
gen small-640x360 -f lavfi -i "testsrc2=duration=$D:size=640x360:rate=30" \
  -f lavfi -i "sine=frequency=450:duration=$D" "${V[@]}" "${A[@]}" -shortest

# 10. INTERLACED SD source (§20 erratum 125): testsrc2 at 50 fps woven into 25
#     interlaced frames, the field order stated, and x264 told to code fields —
#     so the container really declares field_order=tt, the way a capture card
#     writes it. Encoded as-is the output probes `progressive` with the comb
#     still in the pixels, which is the bug the deinterlacer fixes.
gen interlaced-576i -f lavfi -i "testsrc2=duration=$D:size=720x576:rate=50" \
  -f lavfi -i "sine=frequency=350:duration=$D" \
  -vf "tinterlace=interleave_top,fieldorder=tff" "${V[@]}" "${A[@]}" -shortest \
  -flags +ilme+ildct -top 1
# 11. the 720p30 baseline with `moov` FIRST — the DIRECT-PLAY fixture (§20 erratum 200's Original
#     tier). Every row of the playability table says yes about it and it is progressive-download
#     ready, so the only correct amount of encoder work for this upload is a poster: the server
#     streams the bytes it was given. The matrix had none — every other fixture here writes its moov
#     at the end and therefore takes the REMUX branch, which is why E2E step 62 was building one of
#     its own with the host ffmpeg on every run.
gen faststart-720p30 -f lavfi -i "testsrc2=duration=$D:size=1280x720:rate=30" \
  -f lavfi -i "sine=frequency=440:duration=$D" "${V[@]}" "${A[@]}" -shortest -movflags +faststart

# 12. a PHONE recording's shape (§20 erratum 352; the audio-first report): coded PORTRAIT 720x1280 at 60 fps,
#     a Display Matrix rotation of 90 (displayed 1280x720 landscape), and the AUDIO stream first (stream 0 audio,
#     stream 1 video). 60 fps so the Light copy is wanted (it caps at 30), and moov last so it takes the remux
#     branch: the Light is then encoded FROM THE REMUX, which is the path both errata 351 and 352 broke — on the
#     CUDA graph the picture came out unturned, squeezed and still carrying the matrix. E2E step 112 reads it.
if ! wanted rotated90-audiofirst; then :
elif ! sound "$OUT/rotated90-audiofirst.mp4"; then
  [ ! -e "$OUT/rotated90-audiofirst.mp4" ] || echo "  ! rotated90-audiofirst.mp4 is not a readable clip — regenerating"
  "${FF[@]}" -f lavfi -i "testsrc2=duration=$D:size=720x1280:rate=60" \
    -f lavfi -i "sine=frequency=470:duration=$D" "${V[@]}" "${A[@]}" -shortest "$OUT/.rotaf-src.mp4"
  "${FF[@]}" -display_rotation 90 -i "$OUT/.rotaf-src.mp4" -map 0:a -map 0:v -c copy -f mp4 "$OUT/.rotated90-audiofirst.mp4.part"
  mv -f "$OUT/.rotated90-audiofirst.mp4.part" "$OUT/rotated90-audiofirst.mp4"
  rm -f "$OUT/.rotaf-src.mp4"; echo "  + rotated90-audiofirst.mp4"
else echo "  = rotated90-audiofirst.mp4 (exists)"; fi

# --- the AUDIO matrix (§20 erratum 328; E2E step 103) -----------------------------------------------------
# Made only when asked (TESTCLIPS_AUDIO=1, which 103-audio.sh sets), into $OUT/audio/, from lavfi sources
# like everything above — nothing here is ever committed. Every fixture that carries tags carries the kind a
# person would not want published (a title, an artist, a comment, a location) so the strip has something to
# remove. Each file this run WRITES is recorded in $OUT/audio/.made, one name per line, and 103-audio.sh
# removes exactly those when it is done (the "spins down as cleanly as it spins up" rule, CLAUDE.md).
if [ "${TESTCLIPS_AUDIO:-}" = 1 ]; then
  AU="$OUT/audio"; mkdir -p "$AU"
  AU_T0=$SECONDS
  TAGS=(-metadata "title=E2E secret title" -metadata "artist=E2E artist" -metadata "comment=E2E home address")
  # genau <file> <ffmpeg args…> — one fixture, written to a dot-.part name and renamed when ffmpeg exits 0.
  genau() {
    local f="$1"; shift
    if sound "$AU/$f"; then echo "  = audio/$f (exists)"; return; fi
    "${FF[@]}" "$@" "$AU/.$f.part.${f##*.}"
    mv -f "$AU/.$f.part.${f##*.}" "$AU/$f"
    printf '%s\n' "$f" >> "$AU/.made"
    echo "  + audio/$f"
  }
  S440="sine=frequency=440:duration=$D"
  genau mp3-id3-cover.mp3 -f lavfi -i "$S440" -f lavfi -i "color=c=red:s=64x64:d=1" -map 0:a -map 1:v \
    -c:a libmp3lame -b:a 320k -c:v mjpeg -disposition:v attached_pic -id3v2_version 3 "${TAGS[@]}"
  genau m4a-moovlast.m4a -f lavfi -i "$S440" -c:a aac -b:a 192k "${TAGS[@]}" -metadata "location=+40.6892-074.0445/" -f mp4
  genau wav-16-44.wav -f lavfi -i "$S440" -ac 2 -ar 44100 -c:a pcm_s16le "${TAGS[@]}"
  genau wav-24-96.wav -f lavfi -i "$S440" -ac 2 -ar 96000 -c:a pcm_s24le "${TAGS[@]}"
  # +3 dBFS float (round-2 research §5.3): a sine at amplitude 1.4125 is over full scale on purpose.
  genau wav-32f-hot.wav -f lavfi -i "aevalsrc=1.4125*sin(2*PI*440*t):s=48000:d=$D" -c:a pcm_f32le "${TAGS[@]}"
  genau flac-24-48.flac -f lavfi -i "$S440" -ac 2 -ar 48000 -sample_fmt s32 -c:a flac "${TAGS[@]}"
  genau opus-voice.ogg -f lavfi -i "$S440" -ac 1 -ar 48000 -c:a libopus -b:a 32k "${TAGS[@]}"
  # A browser's MediaRecorder (`audio/webm;codecs=opus`) and an MKA FLAC: the strip never writes Matroska back
  # (its muxers keep DURATION and `encoder` tags), so these come out as an .ogg and a .flac.
  genau webm-opus.webm -f lavfi -i "$S440" -ac 1 -ar 48000 -c:a libopus -b:a 64k "${TAGS[@]}"
  genau mka-flac.mka -f lavfi -i "$S440" -ac 2 -ar 48000 -c:a flac "${TAGS[@]}"
  genau aiff-16.aiff -f lavfi -i "$S440" -ac 2 -c:a pcm_s16be "${TAGS[@]}"
  genau alac.m4a -f lavfi -i "$S440" -ac 2 -c:a alac "${TAGS[@]}" -f mp4
  # The cover-only "video" the video gate used to refuse: an mp4 whose ONLY picture is an attached one.
  genau cover-only.mp4 -f lavfi -i "$S440" -f lavfi -i "color=c=blue:s=64x64:d=1" -map 0:a -map 1:v \
    -c:a aac -c:v mjpeg -disposition:v:0 attached_pic "${TAGS[@]}" -f mp4
  genau flac-51.flac -f lavfi -i "$S440" -af "pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0" -c:a flac
  # Past SOCIAL_MAX_BYTES (25 MiB): 24/48 stereo for 100 s is 28.8 MB, so the card copy's fitting runs.
  genau wav-big.wav -f lavfi -i "sine=frequency=330:duration=100" -ac 2 -ar 48000 -c:a pcm_s24le
  # A DSD-SHAPED file (D8a refuses DSD): ffmpeg has a DSF demuxer and no muxer, so the header is written
  # here by hand — the 28-byte `DSD ` chunk, the 52-byte `fmt ` chunk (2.8224 MHz, 1 bit, two channels,
  # 4096-byte blocks), and a `data` chunk of silence (0x69 is DSD's idle pattern). ffprobe demuxes it as
  # `dsf` / `dsd_lsbf_planar`, which is what the gate is asked about.
  if [ ! -s "$AU/dsd.dsf" ]; then
    perl -e 'my $blk=4096; my $data=chr(0x69) x ($blk*2*8); my $n=length($data);
      my $fmt = "fmt " . pack("Q<",52) . pack("VVVVVV",1,0,2,2,2822400,1) . pack("Q<",$blk*8*8) . pack("VV",$blk,0);
      my $body = $fmt . "data" . pack("Q<",12+$n) . $data; my $total = 28 + length($body);
      binmode STDOUT; print "DSD " . pack("Q<",28) . pack("Q<",$total) . pack("Q<",0) . $body;' > "$AU/.dsd.dsf.part"
    mv -f "$AU/.dsd.dsf.part" "$AU/dsd.dsf"; printf '%s\n' dsd.dsf >> "$AU/.made"; echo "  + audio/dsd.dsf"
  fi
  echo "[testclips] audio matrix ready in $((SECONDS - AU_T0)) s"
fi

echo "[testclips] ffprobe summary:"
for f in "$OUT"/*.mp4; do
  [ -e "$f" ] || continue
  printf '  %-26s ' "$(basename "$f")"
  ffprobe -v error -show_entries stream=codec_type,codec_name,width,height,r_frame_rate,sample_aspect_ratio:stream_side_data=rotation:format=duration \
    -of compact=p=0:nk=0 "$f" | tr '\n' ' ' | sed 's/side_data_type=[^|]*|\?//g'
  echo
done
