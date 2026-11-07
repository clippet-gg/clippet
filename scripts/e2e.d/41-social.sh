# 41-social.sh — bundle D: the dedicated social rendition (DESIGN §17 #3, §20 errata 121–124) and
# interlaced sources (§20 erratum 125). Sourced by scripts/e2e.sh before the step-16 log gate.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=S_,D_ buckets=-
# e2e-covers: internal/transcode/social.go internal/transcode/plan.go internal/transcode/encode.go
# e2e-covers: internal/transcode/probe.go internal/transcode/trim.go internal/transcode/transcode.go
# e2e-covers: internal/worker/social.go internal/worker/trim.go internal/worker/pipeline.go internal/db/social.go
# e2e-covers: internal/db/trim.go internal/httpapi/trim.go internal/httpapi/media.go
#   Both clips are its own and both are deleted; every count it takes is scoped to one clip's prefix.
#   Its one reading of shared state is C's `social: (none)` line, which is a property of C's shape and
#   not of anything another bundle does to it.
#
# Both steps create their own clips and delete them again: nothing here outlives the run, and neither
# step touches A–E or X. Everything is measured with the host ffprobe/ffmpeg — a card file that merely
# EXISTS proves nothing, the question is always what is in it.
#
# Every media fetch here goes through the harness's `media_*` helpers (§20 erratum 280, rule (a)):
# what this bundle asks of `video.mp4`, `social.mp4` and `poster.jpg` is the OBJECT — its bytes, its
# size, its validator, its dimensions — and all of that travels with the bytes to wherever
# `MEDIA_DELIVERY` is sending them. Nothing here asserts an app-only header, so no assertion in this
# file stands down in either shape.

# `social_etag`, `wait_social` and `prefix_names` were defined here and are now harness helpers in
# scripts/e2e.sh (§20 erratum 245). 62-tiers.sh calls two of them, which worked only while every
# drop-in was sourced into one shell in name order; a bundle that runs in a lane runs in a SUBSHELL, so
# a helper two bundles share has to live where both can see it. Nothing about them changed.

# idet_combed <file> <label> → how many of the first 100 frames ffmpeg's own interlace detector calls
# interlaced (TFF+BFF). It is the only honest measurement here: `field_order` is metadata that any
# re-encode rewrites, and the comb is in the pixels whatever the container says.
idet_combed() {
  local out="$E2E_DIR/idet-$2.txt"
  ffmpeg -hide_banner -nostdin -i "$(W "$1")" -an -vf idet -frames:v 100 -f null - > "$out" 2>&1 \
    || die "ffmpeg idet failed on $2: $(tail -3 "$out")"
  sed -n 's/.*Multi frame detection: *TFF: *\([0-9]*\) *BFF: *\([0-9]*\).*/\1 \2/p' "$out" | tail -1 \
    | awk '{ print $1 + $2 }'
}

step 41 "the social rendition (#3): a 60 fps clip gets its own smaller /social.mp4, a small one does not, a trim replaces it"
# 1. A 1080p60 upload. Since the quality tiers (§20 errata 200-206) the file a viewer streams is the
#    REMUX of it — 1920x1080 at 60 fps, not downscaled — and the card gets its own LIGHT copy for two
#    independent reasons: the picture is larger than 1280x720, and the rate is above 30. Six seconds of
#    it is far under SOCIAL_MAX_BYTES, which is the point: the rule catches the SHAPE, not just the
#    instance.
code=$(upload S hd-1080p60.mp4 '{"title":"E2E social","ttl_days":1}'); expect "$code" 201 "upload S ($(cat "$RESP"))"
D_S=$(json id); expect_nonempty "$D_S" "S id"
wait_ready "$D_S" S
code=$(get "$BASE/api/clips/$D_S"); expect "$code" 200 "GET S"
expect "$(json fps)" "60/1" "S rendition fps (the social rule's trigger)"
S_W=$(json width); S_H=$(json height); S_PW=$(json poster.width); S_PH=$(json poster.height)
expect "$S_W" 1920 "S width (the upload's own, remuxed rather than downscaled)"; expect "$S_H" 1080 "S height"

code=$(media_head "$BASE/api/clips/$D_S/video.mp4"); expect_media "$code" 200 "HEAD S video.mp4"
S_VETAG=$(hdr ETag); S_VLEN=$(hdr Content-Length)
expect_nonempty "$S_VETAG" "S video ETag"; expect_nonempty "$S_VLEN" "S video Content-Length"
# Before the social job lands, /social.mp4 IS the rendition (the route's COALESCE). Either state is
# legal here — the encode may already have finished — so this only pins that the route answers.
expect_nonempty "$(social_etag "$D_S")" "S social.mp4 answers before the encode lands"

wait_social "$D_S" S "$S_VETAG"
code=$(media_head "$BASE/api/clips/$D_S/social.mp4"); expect_media "$code" 200 "HEAD S social.mp4"
S_SETAG=$(hdr ETag); S_SLEN=$(hdr Content-Length)
expect "$S_SETAG" "$SOCIAL_ETAG" "S social ETag is stable between polls"
[ "$S_SETAG" != "$S_VETAG" ] || die "S social.mp4 still serves the rendition: $S_SETAG"
expect "$(hdr Content-Type)" video/mp4 "S social Content-Type"
# Smaller than the file a player streams, and under the cap the rule is written in terms of.
[ "$S_SLEN" -lt "$S_VLEN" ] || die "S social.mp4 is $S_SLEN bytes, the rendition $S_VLEN — the card file must be smaller"
[ "$S_SLEN" -lt 26214400 ] || die "S social.mp4 is $S_SLEN bytes, past SOCIAL_MAX_BYTES (26214400)"
echo "[e2e]   S rendition $S_VLEN bytes → social $S_SLEN bytes"

# 2. What is actually in it: h264, at most 30 fps, faststart, and its own dimensions.
code=$(media_download "$BASE/api/clips/$D_S/social.mp4" "$E2E_DIR/S-social.mp4")
expect "$code" 200 "download S social.mp4"
ffprobe -v error -print_format json -show_streams -show_format "$(W "$E2E_DIR/S-social.mp4")" > "$E2E_DIR/probe-S-social.json"
S_SFPS=$(ffprobe -v error -select_streams v:0 -show_entries stream=r_frame_rate -of csv=p=0 "$(W "$E2E_DIR/S-social.mp4")" | tr -d '\r')
awk -v f="$S_SFPS" 'BEGIN { split(f, p, "/"); if (p[2] + 0 == 0 || p[1] / p[2] > 30.0001) exit 1 }' \
  || die "S social.mp4 runs at $S_SFPS, want <= 30 fps"
S_SDIMS=$(ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0:s=x "$(W "$E2E_DIR/S-social.mp4")" | tr -d '\r')
S_SW=${S_SDIMS%x*}; S_SH=${S_SDIMS#*x}
# The LIGHT copy is the one tier with a box of its own — 1280x720 at 30 fps (§20 erratum 203) — and it
# fits INSIDE the clip's own picture rather than matching it, which is exactly the change the tiers
# made: the file a viewer streams is 1080p and the file a crawler fetches is 720p. Pinning both is what
# makes the card's dims safe: a card must state what it will actually be handed (§20 erratum 123).
probe_check "$E2E_DIR/probe-S-social.json" "$E2E_DIR/S-social.mp4" 1 "$S_SW" "$S_SH" "$S_SFPS" 1280x720
[ "$S_SW" -le 1280 ] && [ "$S_SH" -le 720 ] || die "the light copy is ${S_SW}x${S_SH}, past its own 1280x720 box"
[ "$S_SW" -lt "$S_W" ] || die "the light copy ($S_SW) is not smaller than the clip's own picture ($S_W)"
echo "[e2e]   S social.mp4: ${S_SDIMS} @ $S_SFPS"

# 3. The card must describe THAT file, not the rendition (§20 erratum 123), and a crawler must be able
#    to fetch it in pieces — Discordbot ranges rather than downloading whole videos.
code=$(media_get "$BASE/api/clips/$D_S/social.mp4" -A "$DISCORD_UA" -H 'Range: bytes=0-99'); expect "$code" 206 "Discordbot ranged GET on social.mp4"
expect "$(hdr Content-Range)" "bytes 0-99/$S_SLEN" "Discordbot Content-Range"
expect "$(wc -c < "$RESP" | tr -d ' ')" 100 "Discordbot ranged body bytes"
code=$(page "$BASE/$D_S" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot share page for S"
card_check player "$PAGE" "$BASE" "$D_S" "E2E social" "$S_PW" "$S_PH" "$S_SW" "$S_SH" "$SECURE"

# 4. The operator's view names the key and its measured dims, and the key is a `social-` one under the
#    clip's own prefix (so the delete and `admin orphans` already cover it).
out=$(dc exec -T app clippet admin ls "$D_S") || die "admin ls S failed: $out"
S_SKEY=$(printf '%s\n' "$out" | sed -n 's#^social: *\([^ ]*\).*#\1#p' | tr -d '\r')
expect_nonempty "$S_SKEY" "admin ls names the social key"
contains "$S_SKEY" "clips/$D_S/renditions/social-" || die "social key $S_SKEY is not the §6 shape"
contains "$out" "dims=${S_SW}x${S_SH}" || die "admin ls social dims disagree with the file: $(printf '%s\n' "$out" | grep '^social:')"

# 5. The other half of the rule: C is 30 fps and small, so it never got a job at all and /social.mp4
#    still serves the rendition byte for byte — the COALESCE fallback this feature must not break.
#    (Until run 9 C was also the clip step 21 had trimmed; 21-trim now trims a clip of its own and asserts
#    the same COALESCE on the swapped rendition itself — "social.mp4 must still be the same object".)
expect_nonempty "$C" "C (the skip-rule control) is still alive at this point in the run"
code=$(media_head "$BASE/api/clips/$C/video.mp4"); expect_media "$code" 200 "HEAD C video.mp4"
C_VETAG=$(hdr ETag); C_VLEN=$(hdr Content-Length)
code=$(media_head "$BASE/api/clips/$C/social.mp4"); expect_media "$code" 200 "HEAD C social.mp4"
expect "$(hdr ETag)" "$C_VETAG" "C social ETag = video ETag (the skip rule left it alone)"
expect "$(hdr Content-Length)" "$C_VLEN" "C social Content-Length = video"
out=$(dc exec -T app clippet admin ls "$C") || die "admin ls C failed: $out"
contains "$out" "social:    (none" || die "C has a social rendition although it is 30 fps and small: $(printf '%s\n' "$out" | grep '^social:')"

# 6. A trim replaces it (erratum 70's reservation, now due): the columns are NULLed with the rendition,
#    the superseded object is deleted, and a fresh encode is queued by the same commit. Five objects
#    remain under the prefix: the original, the new rendition, the new poster, probe.json and the new
#    social rendition.
# Depends on `wait_trim`, a HARNESS global (scripts/e2e.sh, step 21) — in scope in a sourced drop-in and
# in a lane subshell alike. The `command -v … || die` that used to stand here was written while the
# helper lived in another drop-in and could not fire once it moved (scripts/e2e.d/README.md's second
# convention); this line is the dependency it was really documenting.
code=$(http POST "$BASE/api/clips/$D_S/trim" -b "$JARW" -H 'Content-Type: application/json' -d '{"start_ms":500,"end_ms":3500}')
expect "$code" 202 "trim S ($(cat "$RESP"))"
wait_trim "$D_S" S
code=$(media_head "$BASE/api/clips/$D_S/video.mp4"); expect_media "$code" 200 "HEAD S video.mp4 after the trim"
S_VETAG2=$(hdr ETag)
[ "$S_VETAG2" != "$S_VETAG" ] || die "the trim did not replace S's rendition"
wait_social "$D_S" "S (after the trim)" "$S_VETAG2"
[ "$SOCIAL_ETAG" != "$S_SETAG" ] || die "S kept its PRE-TRIM social rendition: $SOCIAL_ETAG"
out=$(dc exec -T app clippet admin ls "$D_S") || die "admin ls S after the trim failed: $out"
S_SKEY2=$(printf '%s\n' "$out" | sed -n 's#^social: *\([^ ]*\).*#\1#p' | tr -d '\r')
[ -n "$S_SKEY2" ] && [ "$S_SKEY2" != "$S_SKEY" ] || die "the social key did not change across the trim ($S_SKEY2)"
expect_prefix_count "$D_S" 5 "S objects after trim + social (original, rendition, poster, probe.json, social)"
# ...and the superseded card file is gone from the BUCKET, not merely unreferenced by the row.
prefix_names "$D_S" > "$E2E_DIR/keys-S.txt"
lacks_file "$E2E_DIR/keys-S.txt" "${S_SKEY##*/}" "the pre-trim social object is still in the bucket"
has_file "$E2E_DIR/keys-S.txt" "${S_SKEY2##*/}" "the new social object is not in the bucket"
out=$(dc exec -T app clippet admin ls "$D_S" --stat) || die "admin ls S --stat failed: $out"
if contains "$out" MISSING; then die "admin ls S --stat reports a missing object after the trim: $out"; fi

# 7. Clean up: this step's clip is the step's own.
code=$(http DELETE "$BASE/api/clips/$D_S" -b "$JARW"); expect_deleted "$code" "$D_S" "DELETE S"
expect_prefix_count "$D_S" 0 "S prefix empty after the delete"

step 42 "interlaced sources (#28): the comb is removed, not relabelled"
# The fixture really is combed — `interlaced-576i.mp4` declares field_order=tt AND measures interlaced
# under idet. Both halves matter: a source that only CLAIMED to be interlaced would make the assertion
# below pass for the wrong reason.
[ -s testclips/interlaced-576i.mp4 ] || die "testclips/interlaced-576i.mp4 missing (scripts/testclips.sh)"
SRC_ORDER=$(ffprobe -v error -select_streams v:0 -show_entries stream=field_order -of csv=p=0 "$(W "$PWD/testclips/interlaced-576i.mp4")" | tr -d '\r')
expect "$SRC_ORDER" tt "the interlaced fixture's declared field order"
SRC_COMBED=$(idet_combed "$PWD/testclips/interlaced-576i.mp4" src)
[ -n "$SRC_COMBED" ] && [ "$SRC_COMBED" -ge 90 ] \
  || die "the interlaced fixture is not actually combed (idet says $SRC_COMBED of 100 frames); this step would prove nothing"
echo "[e2e]   fixture: field_order=$SRC_ORDER, $SRC_COMBED/100 frames combed"

code=$(upload I interlaced-576i.mp4 '{"title":"E2E interlaced","ttl_days":1}'); expect "$code" 201 "upload I ($(cat "$RESP"))"
D_I=$(json id); expect_nonempty "$D_I" "I id"
wait_ready "$D_I" I
code=$(get "$BASE/api/clips/$D_I"); expect "$code" 200 "GET I"
expect "$(json width)" 720 "I width (720x576 fits the box unscaled)"; expect "$(json height)" 576 "I height"
code=$(media_download "$BASE/api/clips/$D_I/video.mp4" "$E2E_DIR/I.mp4"); expect "$code" 200 "download I"
# The metadata says progressive either way — that is the trap, not the proof.
expect "$(ffprobe -v error -select_streams v:0 -show_entries stream=field_order -of csv=p=0 "$(W "$E2E_DIR/I.mp4")" | tr -d '\r')" \
  progressive "I rendition field_order"
OUT_COMBED=$(idet_combed "$E2E_DIR/I.mp4" out)
[ -n "$OUT_COMBED" ] && [ "$OUT_COMBED" -le 10 ] \
  || die "the rendition is still combed: idet says $OUT_COMBED of 100 frames interlaced (source: $SRC_COMBED)"
echo "[e2e]   rendition: $OUT_COMBED/100 frames combed (source $SRC_COMBED/100)"
code=$(http DELETE "$BASE/api/clips/$D_I" -b "$JARW"); expect_deleted "$code" "$D_I" "DELETE I"
expect_prefix_count "$D_I" 0 "I prefix empty after the delete"

# This bundle's rows in the closing summary table (scripts/e2e.d/README.md). D_S/D_I carry the ids:
# a drop-in's variables are globals shared with the harness, so they wear its bundle letter.
summary_row S "${D_S:-}" "1080p60: remuxed to 1920x1080@60 + a DEDICATED light copy (<=30 fps, <=720p, smaller), remade by a trim, the superseded object gone; deleted"
summary_row I "${D_I:-}" "interlaced 576i: deinterlaced to progressive, no combing left in the rendition; deleted"
