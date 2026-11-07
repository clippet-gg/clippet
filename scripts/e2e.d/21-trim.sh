# 21-trim.sh — server-side trim (#5): the negatives, eight simultaneous POSTs and exactly one winner, the
# re-encode and the stream-copy paths, WHICH seconds came out and a re-extension past the current cut; and
# the scratch janitor against a live trim. Steps 21 and 24, which were the serial core of scripts/e2e.sh
# until run 9's lane `pool` (scripts/e2e.d/README.md "The core in the pool").
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=TR_ buckets=-
# e2e-covers: internal/httpapi/trim.go internal/worker/trim.go internal/db/trim.go internal/transcode/trim.go
# e2e-covers: internal/scratch/scratch.go internal/worker/worker.go
#   Every clip it trims is its OWN: step 21's subject was the harness's C until run 9, and C is read by six
#   bundles (31-nits compares its social.mp4 validators across requests, 41-social uses it as the
#   skip-rule control), so a trim of C beside them would change what they read mid-assertion. TR_C is the
#   same fixture (vertical 720x1280, 6 s, audio) uploaded here, and it is deleted at the end. The harness's
#   second user is signed in with a session of its own. Every count is one clip's own prefix and every log
#   read is filtered to one clip id; the one instance-wide read is `admin stats`' schema line, which no
#   clip moves.
#
# Variables are prefixed TR_.
#
# Read beside 41-social ("a trim replaces it") and 62-tiers' step 65 ("every tier key is invalidated
# together: a trim replaces them") in run 9's duplicates read, and KEPT: those two prove the CARD copy and
# the tiers follow a trim; only this step proves the trim itself — the race, both paths, the seconds cut,
# the re-extension, the superseded main rendition and poster gone.

step 21 "server-side trim (#5): re-encode and stream copy, one UPDATE, superseded keys gone, clip never leaves ready"
code=$(upload TR_C vertical-720x1280.mp4 '{"title":"E2E trim subject","ttl_days":0}'); expect "$code" 201 "upload TR_C, the trim subject ($(cat "$RESP"))"
TR_C=$(json id); expect_nonempty "$TR_C" "TR_C id"
wait_ready "$TR_C" C
TR_JAR2="$CTX_DIR/cookies-b.txt"; TR_JAR2W=$(W "$TR_JAR2")
# 0. The schema this contract needs, on the live volume — which was created by 0001 alone.
out=$(dc exec -T app clippet admin stats --no-storage) || die "admin stats failed: $out"
contains "$out" 'applied=1,2,3' || die "migration 0003 is not applied on the live volume: $out"
echo "[e2e]   $(printf '%s\n' "$out" | sed -n 's/^schema: */schema /p' | tr -d '\r')"

# 0b. C's own measurements. The bound a window is checked against is the ORIGINAL's length, which only the
#    OWNER is told (§8.2), so the negatives below are built from that rather than from the rendition's.
code=$(get "$BASE/api/clips/$TR_C"); expect "$code" 200 "GET C before the trim"
expect "$(json state)" ready "C state before the trim"
TR_CW=$(json width); TR_CH=$(json height); TR_CDUR=$(json duration_ms)
{ [ "$TR_CDUR" -ge 5500 ] && [ "$TR_CDUR" -le 6500 ]; } || die "C duration_ms=$TR_CDUR, want ≈6000"
code=$(get "$BASE/api/clips/$TR_C" -b "$JARW"); expect "$code" 200 "GET C as its owner"
TR_CSRC=$(json source_duration_ms)
expect_nonempty "$TR_CSRC" "C source_duration_ms (the owner is told how long the ORIGINAL is)"
{ [ "$TR_CSRC" -ge 5500 ] && [ "$TR_CSRC" -le 6500 ]; } || die "C source_duration_ms=$TR_CSRC, want ≈6000"
expect_empty "$(json trim)" "C trim window before any trim (it is the whole upload)"
# …and nobody else is: the cut history of a clip is the owner's business.
code=$(get "$BASE/api/clips/$TR_C"); expect_empty "$(json source_duration_ms)" "source_duration_ms must not reach a visitor"

# 1. The negatives first, while no trim is in flight to confuse them (§14 owner-route conventions).
code=$(http_refused POST "$BASE/api/clips/$TR_C/trim" -H 'Content-Type: application/json' -d '{"start_ms":0,"end_ms":2000}')
expect "$code" 401 "anonymous trim"; expect "$(json error)" unauthorized "anonymous trim error code"
# The harness's second user (step 3 registers it), in a session of this bundle's own.
rm -f "$TR_JAR2"
code=$(http POST "$BASE/api/auth/login" -c "$TR_JAR2W" -H 'Content-Type: application/json' \
  -d "{\"username\":\"${E2E_USER}_b\",\"password\":\"$E2E_PASS\"}")
expect "$code" 200 "log the second user back in"
code=$(http_refused POST "$BASE/api/clips/$TR_C/trim" -b "$TR_JAR2W" -H 'Content-Type: application/json' -d '{"start_ms":0,"end_ms":2000}')
expect "$code" 403 "another user's trim"; expect "$(json error)" forbidden "another user's trim error code"
code=$(http POST "$BASE/api/auth/tokens" -b "$JARW" -H 'Content-Type: application/json' -d '{"name":"e2e trim"}')
expect "$code" 201 "mint a token for the trim negative"
TR_TOK=$(json token); TR_TID=$(json id)
code=$(http_refused POST "$BASE/api/clips/$TR_C/trim" -H "Authorization: Bearer $TR_TOK" -H 'Content-Type: application/json' -d '{"start_ms":0,"end_ms":2000}')
expect "$code" 403 "token trim"; expect "$(json error)" token_not_allowed "token trim error code (§20 erratum 19)"
code=$(http DELETE "$BASE/api/auth/tokens/$TR_TID" -b "$JARW"); expect "$code" 204 "revoke the trim token"
# Every invalid window is one code, invalid_trim. C has never been cut, so its CURRENT window is
# 0..source_duration_ms — and asking for exactly that is the no-op (§20 erratum 68).
for body in '{"start_ms":3000,"end_ms":1000}' '{"start_ms":1000,"end_ms":1000}' '{"start_ms":-1,"end_ms":3000}' \
            "{\"start_ms\":1000,\"end_ms\":$((TR_CSRC + 1))}" '{"start_ms":1000,"end_ms":1400}' "{\"start_ms\":0,\"end_ms\":$TR_CSRC}" '{"end_ms":3000}'; do
  code=$(http_refused POST "$BASE/api/clips/$TR_C/trim" -b "$JARW" -H 'Content-Type: application/json' -d "$body")
  expect "$code" 400 "invalid trim $body"
  expect "$(json error)" invalid_trim "invalid trim $body error code"
done
code=$(get "$BASE/api/clips/$TR_C/trim" -b "$JARW"); expect "$code" 404 "GET trim before any trim exists"
expect "$(json error)" not_found "GET trim before any trim exists: error code"

# 2. The real thing on C (vertical 720x1280, 6 s, audio): a cut between keyframes → the re-encode path.
code=$(media_head "$BASE/api/clips/$TR_C/video.mp4"); expect_media "$code" 200 "C video.mp4 before the trim"
TR_CETAG0=$(hdr ETag); expect_nonempty "$TR_CETAG0" "C video ETag before the trim"
code=$(media_head "$BASE/api/clips/$TR_C/poster.jpg"); expect_media "$code" 200 "C poster.jpg before the trim"
TR_CPETAG0=$(hdr ETag)
TR_COBJ0=$(prefix_count "$TR_C")
# `renditions/main-` and not `renditions/`: the social rendition (renditions/social-{r8}.mp4) and the
# preview GIF (renditions/preview-{r8}.gif) share that prefix, so an unqualified match would return
# two lines here and break the count below the day C acquires either (§6, store.SocialKey/GIFKey).
TR_CKEY0=$(dc exec -T app clippet admin ls "$TR_C" --stat | sed -n 's#.*present \(.*renditions/main-[^ ]*\).*#\1#p' | tr -d '\r')
expect_nonempty "$TR_CKEY0" "C rendition key before the trim"
echo "[e2e]   C before: ${TR_CW}x${TR_CH} ${TR_CDUR}ms etag=$TR_CETAG0 objects=$TR_COBJ0"
# EIGHT simultaneous POSTs, not one-then-another: the guarded INSERT is what decides, so exactly one may
# win however the requests interleave. (The old "fire a second one afterwards" form only held while the
# first trim was still running — a ~300 ms window on a fast box, i.e. a test that passed by luck.)
rm -f "$E2E_DIR"/race-*.code "$E2E_DIR"/race-*.json
for i in 1 2 3 4 5 6 7 8; do
  ( { curl -sS -o "$(W "$E2E_DIR/race-$i.json")" -w '%{http_code}' -b "$JARW" -X POST \
        -H 'Content-Type: application/json' -d '{"start_ms":1000,"end_ms":3000}' \
        "$BASE/api/clips/$TR_C/trim" 2>/dev/null || echo 000; } > "$E2E_DIR/race-$i.code" ) &
done
wait || true
TR_RACE202=0; TR_RACE409=0; TR_RACE400=0
for i in 1 2 3 4 5 6 7 8; do
  rc=$(tr -d ' \r\n' < "$E2E_DIR/race-$i.code")
  case $rc in
    202) TR_RACE202=$((TR_RACE202 + 1)) ;;
    409) TR_RACE409=$((TR_RACE409 + 1))
         expect "$(jget "$(W "$E2E_DIR/race-$i.json")" error)" trim_in_progress "the loser of the race says why" ;;
    # A loser that lands AFTER the winner's trim has APPLIED is refused as "the cut this clip already
    # has" — the same single trim, seen from the far side of it. C is 6 s and re-encodes in ~0.4 s on a
    # four-cpu container, and a tunnel staggers eight "simultaneous" POSTs over ~0.5 s, so this is the
    # second honest shape of losing (§20 erratum 245 (g)). The winner count below is what the guarded
    # INSERT promises, and it holds either way; a second 202 would still be the bug this step exists for.
    400) TR_RACE400=$((TR_RACE400 + 1))
         expect "$(jget "$(W "$E2E_DIR/race-$i.json")" error)" invalid_trim "a late loser's error code"
         contains "$(jget "$(W "$E2E_DIR/race-$i.json")" message)" 'already has' || die "a late loser must be refused as the cut the clip already has: $(cat "$E2E_DIR/race-$i.json")" ;;
    *)   die "concurrent trim $i answered $rc (want 202, 409, or the post-apply 400): $(cat "$E2E_DIR/race-$i.json")" ;;
  esac
done
expect "$TR_RACE202" 1 "exactly one of eight simultaneous trims is accepted"
expect "$((TR_RACE409 + TR_RACE400))" 7 "the other seven lose: 409 trim_in_progress while it runs, 400 once it has applied"
[ "$TR_RACE409" -ge 1 ] || echo "[e2e]   NOTE: every loser arrived after the trim had applied — the in-flight guard was not exercised this run"
echo "[e2e]   race: 202=$TR_RACE202 409=$TR_RACE409 late-400=$TR_RACE400"
wait_trim "$TR_C" C
# 3. What changed, and what did not.
code=$(get "$BASE/api/clips/$TR_C"); expect "$code" 200 "GET C after the trim"
expect "$(json state)" ready "C state after the trim"
TR_CDUR1=$(json duration_ms)
$PY -c "import sys; d=int(sys.argv[1]); sys.exit(0 if abs(d-2000)<=150 else 'duration_ms=%d, want 2000 +-150' % d)" "$TR_CDUR1" || die "C duration after the trim"
expect "$(json width)" "$TR_CW" "C width after the trim"; expect "$(json height)" "$TR_CH" "C height after the trim"
code=$(media_head "$BASE/api/clips/$TR_C/video.mp4"); expect_media "$code" 200 "C video.mp4 after the trim"
TR_CETAG1=$(hdr ETag); expect_nonempty "$TR_CETAG1" "C video ETag after the trim"
[ "$TR_CETAG1" != "$TR_CETAG0" ] || die "the video ETag did not change; the URL is stable but the bytes are not"
code=$(media_head "$BASE/api/clips/$TR_C/social.mp4"); expect_media "$code" 200 "C social.mp4 after the trim"
expect "$(hdr ETag)" "$TR_CETAG1" "social.mp4 must still be the same object as video.mp4 (COALESCE)"
code=$(media_head "$BASE/api/clips/$TR_C/poster.jpg"); expect_media "$code" 200 "C poster.jpg after the trim"
[ "$(hdr ETag)" != "$TR_CPETAG0" ] || die "the poster ETag did not change; the poster is cut from the NEW rendition"
# The window is recorded on the ORIGINAL's timeline, and the original's own length has not moved.
code=$(get "$BASE/api/clips/$TR_C" -b "$JARW"); expect "$code" 200 "GET C as its owner after the trim"
expect "$(json trim.start_ms)" 1000 "C trim.start_ms (the applied window, on the original)"
expect "$(json trim.end_ms)" 3000 "C trim.end_ms"
expect "$(json source_duration_ms)" "$TR_CSRC" "C source_duration_ms after the trim (it describes the UPLOAD)"
# The bytes themselves, measured by the host ffprobe.
code=$(media_download "$BASE/api/clips/$TR_C/video.mp4" "$E2E_DIR/C-trimmed.mp4"); expect "$code" 200 "download the trimmed C"
ffprobe -v error -print_format json -show_format -show_streams "$(W "$E2E_DIR/C-trimmed.mp4")" > "$E2E_DIR/C-trimmed.json" \
  || die "ffprobe on the trimmed C failed"
probe_check "$E2E_DIR/C-trimmed.json" "$E2E_DIR/C-trimmed.mp4" 1 "$TR_CW" "$TR_CH" || die "the trimmed C is not the shape the pipeline promises"
$PY - "$(W "$E2E_DIR/C-trimmed.json")" <<'PYTRIM' || die "the trimmed C is not 2 s long on disk"
import json, sys
with open(sys.argv[1], encoding='utf-8') as fh:
    d = float(json.load(fh)['format']['duration']) * 1000
if abs(d - 2000) > 150:
    sys.exit('the trimmed file is %.0f ms, want 2000 +-150' % d)
print('[e2e]   trimmed file measures %.0f ms' % d)
PYTRIM
# The superseded rendition and poster are gone: the prefix holds exactly what it held before.
expect "$(prefix_count "$TR_C")" "$TR_COBJ0" "C bucket objects after the trim (the superseded pair must be deleted)"
out=$(dc exec -T app clippet admin ls "$TR_C" --stat) || die "admin ls C failed"
if contains "$out" MISSING; then die "admin ls C --stat reports a missing object after the trim: $out"; fi
expect "$(printf '%s\n' "$out" | grep -c 'present .*renditions/main-')" 1 "exactly one MAIN rendition object"
expect "$(printf '%s\n' "$out" | grep -c 'present .*poster/')" 1 "exactly one poster object"
TR_CKEY1=$(printf '%s\n' "$out" | sed -n 's#.*present \(.*renditions/main-[^ ]*\).*#\1#p' | tr -d '\r')
[ "$TR_CKEY1" != "$TR_CKEY0" ] || die "the rendition key did not change; every attempt writes a fresh {r8}"
contains "$out" 'kind=trim state=done' || die "admin ls C does not show a done trim job: $out"
# The clip's OWN progress never learned about any of this (§8.4 is untouched).
code=$(get "$BASE/api/clips/$TR_C/progress"); expect "$code" 200 "C progress after the trim"
expect "$(json state)" ready "C progress state"; expect "$(json percent)" 100 "C progress percent"
echo "[e2e]   C after: ${TR_CDUR1}ms etag=$TR_CETAG1 rendition $TR_CKEY0 -> $TR_CKEY1"

# 4. The stream-copy path, and WHICH SECONDS came out. The matrix clips are encoded with x264's default
#    250-frame keyint, i.e. ONE keyframe over six seconds, so every interior cut of them re-encodes. A copy
#    needs a keyframe grid: build one (-g 30 -sc_threshold 0 → a keyframe every second). Every second is
#    also a DIFFERENT FLAT COLOUR, which is what turns "the trim finished" into "the trim cut the seconds
#    that were asked for": the first frame of the result names the second of the ORIGINAL it came from.
TR_COLOURCLIP="$E2E_DIR/colours10.mp4"
if [ ! -s "$TR_COLOURCLIP" ]; then
  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "color=c=red:s=640x360:d=1:r=30"     -f lavfi -i "color=c=lime:s=640x360:d=1:r=30" \
    -f lavfi -i "color=c=blue:s=640x360:d=1:r=30"    -f lavfi -i "color=c=yellow:s=640x360:d=1:r=30" \
    -f lavfi -i "color=c=magenta:s=640x360:d=1:r=30" -f lavfi -i "color=c=cyan:s=640x360:d=1:r=30" \
    -f lavfi -i "color=c=white:s=640x360:d=1:r=30"   -f lavfi -i "color=c=gray:s=640x360:d=1:r=30" \
    -f lavfi -i "color=c=orange:s=640x360:d=1:r=30"  -f lavfi -i "color=c=purple:s=640x360:d=1:r=30" \
    -f lavfi -i "sine=frequency=440:duration=10" \
    -filter_complex "[0:v][1:v][2:v][3:v][4:v][5:v][6:v][7:v][8:v][9:v]concat=n=10:v=1:a=0[v]" \
    -map "[v]" -map 10:a -c:v libx264 -preset veryfast -pix_fmt yuv420p \
    -g 30 -keyint_min 30 -sc_threshold 0 -c:a aac -b:a 128k -shortest "$(W "$TR_COLOURCLIP")" \
    || die "could not build the colour-per-second clip"
fi
# tr_first_colour <mp4> → the palette name of the first frame's centre, or "" when it cannot be read.
tr_first_colour() {
  local f=$1 ppm="$E2E_DIR/frame.ppm"
  rm -f "$ppm"
  ffmpeg -hide_banner -loglevel error -y -i "$(W "$f")" -frames:v 1 -pix_fmt rgb24 -f image2 -vcodec ppm "$(W "$ppm")" >/dev/null 2>&1 || return 0
  $PY - "$(W "$ppm")" <<'PYCOL'
import sys
# The ten colours the clip is built from, one per second of the ORIGINAL.
PALETTE = [("red", (255, 0, 0)), ("lime", (0, 255, 0)), ("blue", (0, 0, 255)), ("yellow", (255, 255, 0)),
           ("magenta", (255, 0, 255)), ("cyan", (0, 255, 255)), ("white", (255, 255, 255)),
           ("gray", (128, 128, 128)), ("orange", (255, 165, 0)), ("purple", (128, 0, 128))]
with open(sys.argv[1], 'rb') as fh:
    b = fh.read()
# P6 header: magic, width, height, maxval, then ONE whitespace byte, then the pixels.
tok, i = [], 0
while len(tok) < 4:
    while i < len(b) and b[i:i + 1].isspace():
        i += 1
    if b[i:i + 1] == b'#':
        while i < len(b) and b[i:i + 1] not in (b'\n', b'\r'):
            i += 1
        continue
    j = i
    while j < len(b) and not b[j:j + 1].isspace():
        j += 1
    tok.append(b[i:j])
    i = j
if tok[0] != b'P6':
    sys.exit('not a P6 ppm')
w, h = int(tok[1]), int(tok[2])
px = b[i + 1:]
# Average a 9x9 block at the centre: one pixel could be an artefact, a block cannot.
acc, n = [0, 0, 0], 0
for y in range(h // 2 - 4, h // 2 + 5):
    for x in range(w // 2 - 4, w // 2 + 5):
        o = (y * w + x) * 3
        for c in range(3):
            acc[c] += px[o + c]
        n += 1
rgb = [v / n for v in acc]
print(min(PALETTE, key=lambda p: sum((rgb[c] - p[1][c]) ** 2 for c in range(3)))[0])
PYCOL
}
printf '%s' '{"title":"E2E trim copy","ttl_days":1}' > "$E2E_DIR/meta-V.json"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "metadata=<$(W "$E2E_DIR/meta-V.json");type=application/json" \
  -F duplicate=allow -F "video=@$(W "$TR_COLOURCLIP");type=video/mp4" "$BASE/api/clips")
expect "$code" 201 "upload V (the colour-per-second, keyframe-aligned clip)"
TR_V=$(json id); expect_nonempty "$TR_V" "V id"
wait_ready "$TR_V" V
code=$(get "$BASE/api/clips/$TR_V" -b "$JARW"); expect "$code" 200 "GET V as its owner"
TR_VSRC=$(json source_duration_ms)
$PY -c "import sys; d=int(sys.argv[1]); sys.exit(0 if abs(d-10000)<=200 else 'source_duration_ms=%d, want 10000 +-200' % d)" "$TR_VSRC" \
  || die "V source_duration_ms"
expect_empty "$(json trim)" "V trim window before any trim"
# 4a. Cut seconds 5–9 of the ORIGINAL: the result must BEGIN with the original's sixth second (cyan).
code=$(http POST "$BASE/api/clips/$TR_V/trim" -b "$JARW" -H 'Content-Type: application/json' -d '{"start_ms":5000,"end_ms":9000}')
expect "$code" 202 "POST trim V 5000..9000 ($(cat "$RESP"))"
wait_trim "$TR_V" V
code=$(get "$BASE/api/clips/$TR_V" -b "$JARW"); expect "$code" 200 "GET V after the first trim"
expect "$(json state)" ready "V state after the first trim"
TR_VDUR=$(json duration_ms)
$PY -c "import sys; d=int(sys.argv[1]); sys.exit(0 if abs(d-4000)<=150 else 'duration_ms=%d, want 4000 +-150' % d)" "$TR_VDUR" || die "V duration after the first trim"
expect "$(json trim.start_ms)" 5000 "V trim.start_ms after the first trim"
expect "$(json trim.end_ms)" 9000 "V trim.end_ms after the first trim"
expect "$(json source_duration_ms)" "$TR_VSRC" "V source_duration_ms after the first trim"
code=$(media_download "$BASE/api/clips/$TR_V/video.mp4" "$E2E_DIR/V-cut1.mp4"); expect "$code" 200 "download V's first cut"
TR_VCOL1=$(tr_first_colour "$E2E_DIR/V-cut1.mp4")
expect "$TR_VCOL1" cyan "the first frame of the 5000..9000 cut is the ORIGINAL's second 5"
echo "[e2e]   V cut 5000..9000 → ${TR_VDUR}ms, first frame $TR_VCOL1"
# 4b. Now RE-EXTEND: 2000..4000 is outside the cut the clip currently is, and reaches footage that cut
#     threw away. Under the old contract this was a 400 (the window was measured against the rendition);
#     under §20 erratum 68 it is the whole point of retaining the original.
code=$(http POST "$BASE/api/clips/$TR_V/trim" -b "$JARW" -H 'Content-Type: application/json' -d '{"start_ms":2000,"end_ms":4000}')
expect "$code" 202 "POST trim V 2000..4000 — re-extending past the current cut ($(cat "$RESP"))"
wait_trim "$TR_V" V
code=$(get "$BASE/api/clips/$TR_V" -b "$JARW"); expect "$code" 200 "GET V after the second trim"
TR_VDUR2=$(json duration_ms)
$PY -c "import sys; d=int(sys.argv[1]); sys.exit(0 if abs(d-2000)<=150 else 'duration_ms=%d, want 2000 +-150' % d)" "$TR_VDUR2" || die "V duration after the second trim"
expect "$(json trim.start_ms)" 2000 "V trim.start_ms after the second trim"
expect "$(json trim.end_ms)" 4000 "V trim.end_ms after the second trim"
expect "$(json source_duration_ms)" "$TR_VSRC" "V source_duration_ms after the second trim"
code=$(media_download "$BASE/api/clips/$TR_V/video.mp4" "$E2E_DIR/V-cut2.mp4"); expect "$code" 200 "download V's second cut"
TR_VCOL2=$(tr_first_colour "$E2E_DIR/V-cut2.mp4")
expect "$TR_VCOL2" blue "the first frame of the 2000..4000 cut is the ORIGINAL's second 2, not the previous cut's"
echo "[e2e]   V cut 2000..4000 → ${TR_VDUR2}ms, first frame $TR_VCOL2 (footage the first cut had removed)"

# 5. Which path each one took. The app says so itself, once per applied trim.
TR_LOG="$E2E_DIR/app-trim.log"
dc logs --no-color --since "$START_TS" app > "$TR_LOG" 2>&1 || die "docker compose logs failed"
grep -Eq "trim applied.*clip=$TR_C .*path=reencode" "$TR_LOG" \
  || die "C's trim did not take the re-encode path: $(grep -E "trim (applied|planned).*clip=$TR_C " "$TR_LOG" || echo 'no trim line at all')"
grep -Eq "trim applied.*clip=$TR_V .*path=copy" "$TR_LOG" \
  || die "V's keyframe-aligned trim did not take the stream-copy path: $(grep -E "trim (applied|planned).*clip=$TR_V " "$TR_LOG" || echo 'no trim line at all')"
echo "[e2e]   both paths exercised: C re-encoded, V stream-copied"
code=$(http DELETE "$BASE/api/clips/$TR_V" -b "$JARW"); expect_deleted "$code" "$TR_V" "DELETE V"
code=$(http POST "$BASE/api/auth/logout" -b "$TR_JAR2W" || echo 000); echo "[e2e]   logout ${E2E_USER}_b -> $code"
step 24 "janitor vs a live media run (§20 erratum 89): a ready clip's scratch dir belongs to its trim"
# The scratch janitor used to remove $SCRATCH_DIR/{id} for any clip that was merely `ready` — which a clip
# is for every second of a trim or a GIF render, and both of those work in exactly that dir. A janitor pass
# comes round on every 10th sweep tick (2.5 minutes at this run's 15s interval, 10 minutes at the shipped
# 60s one — §20 erratum 245), far too rarely to provoke on purpose and far too
# expensive to wait for, so this step asserts the GUARD rather than the race: a trim of a clip of its own
# runs to `done`, the admin CLI reports its job throughout, the clip never leaves `ready`, and the run's log
# carries no janitor removal — and no requeued attempt blaming an environment that never failed — for it.
code=$(upload TR_Z std-720p30.mp4 '{"title":"E2E janitor vs a live trim","ttl_days":1}'); expect "$code" 201 "upload Z"
TR_Z=$(json id); expect_nonempty "$TR_Z" "Z id"
wait_ready "$TR_Z" Z
code=$(http POST "$BASE/api/clips/$TR_Z/trim" -b "$JARW" -H 'Content-Type: application/json' -d '{"start_ms":500,"end_ms":4500}')
expect "$code" 202 "POST Z trim (4 s of the 6 s original)"
# The job is on the queue NOW. Whichever state this round trip catches it in, the row is there and the clip
# is still `ready` — the two facts the old janitor rule read as "nobody is using that dir".
out=$(dc exec -T app clippet admin ls "$TR_Z" --stat) || die "admin ls Z failed"
contains "$out" 'kind=trim' || die "admin ls does not report the trim job that was just accepted: $out"
code=$(get "$BASE/api/clips/$TR_Z"); expect "$code" 200 "GET Z while its trim is queued"
expect "$(json state)" ready "Z state while its trim is queued (a trim never moves it)"
wait_trim "$TR_Z" Z
out=$(dc exec -T app clippet admin ls "$TR_Z" --stat) || die "admin ls Z after the trim failed"
contains "$out" 'kind=trim state=done' || die "admin ls Z does not show a done trim job: $out"
if contains "$out" MISSING; then die "admin ls Z --stat reports a missing object after the trim: $out"; fi
code=$(get "$BASE/api/clips/$TR_Z"); expect "$code" 200 "GET Z after its trim"
expect "$(json state)" ready "Z state after its trim"
# This run's log, filtered to this clip: the trim landed, and nothing took its scratch dir away.
TR_ZLOG="$E2E_DIR/step24-$TR_Z.log"
dc logs --no-color --since "$START_TS" app 2>&1 | grep -E "clip=$TR_Z( |\"|$)" > "$TR_ZLOG" || true
[ -s "$TR_ZLOG" ] || die "no log lines at all for $TR_Z"
grep -q 'msg="trim applied"' "$TR_ZLOG" || die "no 'trim applied' line for Z: $(cat "$TR_ZLOG")"
if grep -qE 'janitor: remove|trim attempt failed|trim failed|storage was unavailable' "$TR_ZLOG"; then
  die "a live trim's scratch dir was taken from under it: $(cat "$TR_ZLOG")"
fi
echo "[e2e]   TR_Z=$TR_Z trimmed in peace: $(wc -l < "$TR_ZLOG" | tr -d ' ') log lines, no janitor interference"
code=$(http DELETE "$BASE/api/clips/$TR_Z" -b "$JARW"); expect_deleted "$code" "$TR_Z" "DELETE Z"
code=$(http DELETE "$BASE/api/clips/$TR_C" -b "$JARW"); expect_deleted "$code" "$TR_C" "DELETE TR_C, the trim subject"
summary_row TC "$TR_C" "vertical (the trim subject, its own since run 9): trimmed 1–3 s by one of eight simultaneous POSTs, re-encode path; deleted"
summary_row V "$TR_V" "trim (#5): keyframe-aligned original → STREAM-COPY path, re-extended past its cut; deleted"
summary_row Z "$TR_Z" "janitor vs a live trim: trimmed in peace; deleted"
