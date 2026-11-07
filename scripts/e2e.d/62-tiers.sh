# 62-tiers.sh — bundle M1: quality tiers Original / Full / Light, the remux fast path, card-first
# ordering and the quality selector (DESIGN §17 #10, §20 errata 200–211). Sourced by scripts/e2e.sh
# before the step-16 log gate.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=T_,T1_,T3_,T5_ buckets=-
# e2e-covers: internal/transcode/tiers.go internal/transcode/plan.go internal/transcode/encode.go
# e2e-covers: internal/transcode/verify.go internal/transcode/hwaccel.go internal/transcode/threads.go internal/transcode/clock.go
# e2e-covers: internal/transcode/exec.go internal/transcode/transcode.go internal/worker/full.go
# e2e-covers: internal/worker/pipeline.go internal/worker/worker.go internal/db/full.go internal/httpapi/tiers.go
# e2e-covers: internal/httpapi/media.go internal/web/tiers.go internal/store/keys.go web/templates/clip.html
#   Step 66 ends by DRAINING the poll limiter — up to 400 requests to GET …/progress until one is
#   refused. That bucket is 120 a minute per client ADDRESS and it is the bucket every `wait_ready` in
#   the suite polls, which is what kept this bundle exclusive until lane `excl`: drained from the host,
#   a bundle beside it would have died on a 429 that was this file's doing. The drain now comes from an
#   address of its own (`burst_429 … apart`, scripts/e2e.sh: a throwaway container of this project at
#   the live app's service address — same process, same shipped ceiling, same route), so the host's
#   bucket is never touched. Everything else here is this file's own clips (T1-T5) and reads of
#   the stack: its one bucket-wide read (step 65's `admin orphans --min-age 0`) is asserted only about
#   `clips/$T3/`, and its one login of the run's owner (step 62's fresh session) is a session of its
#   own, which since lane `excl` nothing revokes (33-account changes the password of an account of its
#   own). The pool's `wait_ready` pacing counts it as a poller like every other bundle that polls.
#
# Every clip here is this file's own and is deleted again: nothing outlives the run, and no step
# touches A–E, W or X. Every claim is MEASURED with the host ffprobe — a tier that merely exists
# proves nothing, the question is always what is in it and whether the picture survived.
#
# The fixtures are built here rather than in scripts/testclips.sh because the ONE property that
# decides the fast path — `moov` before `mdat` — is exactly what testclips.sh does not control:
# ffmpeg writes moov LAST unless told otherwise, which is why every clip in that matrix takes the
# remux branch and none of them takes the direct one. A fourth fixture is the same faststart mp4 with a
# phone's own metadata on it, which is a remux case for a PRIVACY reason rather than a container one
# (§20 erratum 200, amended). The other two fixtures are the two shapes of
# UNPLAYABLE upload that matter — HEVC in mp4 (step 63) and H.264 in matroska (step 64, which is what
# OBS records by default) — and both are stream copies or ultrafast encodes, so the host pays nothing.

# T_dims <file> → "WxH" of the first video stream.
t_dims() {
  ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0:s=x "$(W "$1")" | tr -d '\r'
}
# t_fps <file> → the r_frame_rate as "num/den".
t_fps() {
  ffprobe -v error -select_streams v:0 -show_entries stream=r_frame_rate -of csv=p=0 "$(W "$1")" | tr -d '\r'
}
# t_codec <file> → the video codec name.
t_codec() {
  ffprobe -v error -select_streams v:0 -show_entries stream=codec_name -of csv=p=0 "$(W "$1")" | tr -d '\r'
}
# t_moov_first <file> → 0 when the top-level boxes put moov before mdat (a faststart mp4), 1 otherwise.
# It is the same walk transcode.VerifyMP4 does, in python, on the host.
t_moov_first() {
  $PY - "$(W "$1")" <<'PYEOF'
import sys
order = []
with open(sys.argv[1], 'rb') as fh:
    fh.seek(0, 2); end = fh.tell(); off = 0
    while off < end:
        fh.seek(off); hdr = fh.read(8)
        if len(hdr) < 8:
            break
        size = int.from_bytes(hdr[:4], 'big'); typ = hdr[4:8].decode('latin1')
        if size == 1:
            size = int.from_bytes(fh.read(8), 'big')
        elif size == 0:
            size = end - off
        if size < 8:
            break
        order.append(typ); off += size
if 'moov' not in order or 'mdat' not in order:
    sys.exit(1)
sys.exit(0 if order.index('moov') < order.index('mdat') else 1)
PYEOF
}
# t_tags <file> → every container and stream metadata tag ffprobe reports, one `key=value` per line.
# The privacy assertion reads it: the question is never "did a remux run" but "what is in the file the
# world can fetch".
t_tags() {
  # No CR stripping: every reader of this output is a SUBSTRING grep (has_file / lacks_file), which a
  # trailing carriage return cannot affect.
  ffprobe -v error -show_entries format_tags:stream_tags -of default=noprint_wrappers=1 "$(W "$1")"
}
# t_tier <json-file> <tier> <field> → one field of one entry of the DTO's `tiers` list.
t_tier() {
  $PY - "$(W "$1")" "$2" "$3" <<'PYEOF'
import json, sys
with open(sys.argv[1], encoding='utf-8') as fh:
    doc = json.load(fh)
for t in doc.get('tiers') or []:
    if t.get('tier') == sys.argv[2]:
        v = t.get(sys.argv[3])
        # Printed as the WIRE spells it — `true`, not python's `True` — so an
        # assertion in this file reads like the JSON it is about.
        print('' if v is None else (json.dumps(v) if isinstance(v, (bool, dict, list)) else v))
        sys.exit(0)
sys.exit('no %s tier in %s' % (sys.argv[2], doc.get('tiers')))
PYEOF
}
# t_progress_tier <json-file> <tier> → that tier's state on a GET …/progress document.
t_progress_tier() {
  $PY - "$(W "$1")" "$2" <<'PYEOF'
import json, sys
with open(sys.argv[1], encoding='utf-8') as fh:
    doc = json.load(fh)
for t in doc.get('tiers') or []:
    if t.get('tier') == sys.argv[2]:
        print(t.get('state', ''))
        sys.exit(0)
sys.exit('no %s tier in %s' % (sys.argv[2], doc.get('tiers')))
PYEOF
}
# t_admin_field <clip> <line prefix> → the first token of an `admin ls` line.
t_admin_field() {
  dc exec -T app clippet admin ls "$1" 2>/dev/null | tr -d '\r' | sed -n "s#^$2: *\([^ ]*\).*#\1#p"
}
step 62 "quality tiers (M1): a web-playable upload is STREAMED, not re-encoded — no full job, /original anonymous with Range"
# The fixture: 720p30 H.264 + AAC in mp4 with `moov` FIRST, and small. Every row of the playability
# table says yes, and the skip rule wants no card copy either, so the correct amount of ffmpeg work
# for this upload is a poster and nothing else.
#
# It comes from the SHARED matrix now (scripts/testclips.sh's faststart-720p30, cached across runs)
# and is only built here — with the host ffmpeg, on every run, for about a second — when the matrix
# has none, which is what this step did before the matrix had one. T_DIRECT_OWNED says which, because
# the cleanup at the foot of this bundle must not `rm` a cached fixture the next run wants.
# `t_moov_first` is asserted either way: it is the whole premise of the step, not a property this
# step is entitled to assume of a file somebody else wrote.
T_DIRECT="$PWD/testclips/faststart-720p30.mp4"
T_DIRECT_OWNED=0
if [ ! -s "$T_DIRECT" ]; then
  T_DIRECT="$E2E_DIR/tier-direct.mp4"
  T_DIRECT_OWNED=1
  ffmpeg -hide_banner -nostdin -loglevel error -y \
    -f lavfi -i "testsrc2=duration=6:size=1280x720:rate=30" -f lavfi -i "sine=frequency=440:duration=6" \
    -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac -b:a 128k -shortest \
    -movflags +faststart "$(W "$T_DIRECT")" || die "could not build the direct-play fixture"
fi
t_moov_first "$T_DIRECT" || die "the direct-play fixture ($T_DIRECT) is not faststart; this step would prove nothing"
T_DIRECT_DIMS=$(t_dims "$T_DIRECT"); T_DIRECT_FPS=$(t_fps "$T_DIRECT")
T_DIRECT_BYTES=$(wc -c < "$T_DIRECT" | tr -d ' ')
echo "[e2e]   fixture: $T_DIRECT_DIMS @ $T_DIRECT_FPS, $T_DIRECT_BYTES bytes, moov first"

printf '%s' '{"title":"E2E tier direct","ttl_days":1}' > "$E2E_DIR/meta-T1.json"
T_T0=$SECONDS
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "metadata=<$(W "$E2E_DIR/meta-T1.json");type=application/json" \
  -F duplicate=allow -F "video=@$(W "$T_DIRECT");type=video/mp4" "$BASE/api/clips")
expect "$code" 201 "upload T1, the direct-play subject ($(cat "$RESP"))"
T1=$(json id); expect_nonempty "$T1" "T1 id"
wait_ready "$T1" T1
echo "[e2e]   T1 time-to-ready: $((SECONDS - T_T0))s (probe + poster, no encoder)"

code=$(get "$BASE/api/clips/$T1"); expect "$code" 200 "GET T1"
cp "$RESP" "$E2E_DIR/tiers-T1.json"
# The clip's own numbers are the UPLOAD's: not one pixel was resampled.
expect "$(json width)x$(json height)" "$T_DIRECT_DIMS" "T1 dims = the upload's own"
expect "$(json fps)" "$T_DIRECT_FPS" "T1 fps = the upload's own"
expect "$(json card_tier)" original "T1 card_tier"
# `tiers`: the Original is ready and PLAYABLE, and the other two were never needed.
expect "$(t_tier "$E2E_DIR/tiers-T1.json" original state)" ready "T1 original tier state"
expect "$(t_tier "$E2E_DIR/tiers-T1.json" original playable)" true "T1 original tier playable"
expect "$(t_tier "$E2E_DIR/tiers-T1.json" original url)" "$BASE/api/clips/$T1/original" "T1 original tier url"
expect "$(t_tier "$E2E_DIR/tiers-T1.json" original bytes)" "$T_DIRECT_BYTES" "T1 original tier bytes"
expect "$(t_tier "$E2E_DIR/tiers-T1.json" original width)x$(t_tier "$E2E_DIR/tiers-T1.json" original height)" \
  "$T_DIRECT_DIMS" "T1 original tier dims"
expect "$(t_tier "$E2E_DIR/tiers-T1.json" original codec)" h264 "T1 original tier codec"
expect "$(t_tier "$E2E_DIR/tiers-T1.json" full state)" skipped "T1 full tier state (a playable upload needs none)"
expect_empty "$(t_tier "$E2E_DIR/tiers-T1.json" full url)" "T1 full tier url (there is no such file)"
expect "$(t_tier "$E2E_DIR/tiers-T1.json" light state)" skipped "T1 light tier state (720p30 and small)"
expect_empty "$(t_tier "$E2E_DIR/tiers-T1.json" light url)" "T1 light tier url"
# N079 is closed by that list: the DTO now SAYS whether a card file exists.

# NO full job was ever queued, and no encoder ran: the operator's views say so.
out=$(dc exec -T app clippet admin jobs --clip "$T1" --kind full) || die "admin jobs --kind full failed: $out"
contains "$out" "0 job(s)" || die "a full job exists for a clip whose upload plays: $out"
out=$(dc exec -T app clippet admin ls "$T1") || die "admin ls T1 failed: $out"
contains "$out" "rendition: (none" || die "T1 has a rendition object although its upload plays: $(printf '%s\n' "$out" | grep '^rendition:')"
contains "$out" "original playable=true remuxed=false" || die "admin ls does not report the tier verdict: $(printf '%s\n' "$out" | grep '^tiers:')"
contains "$out" "full=skipped" || die "T1's full tier is not skipped: $(printf '%s\n' "$out" | grep '^tiers:')"
# Three objects under the prefix and no more: the upload, the poster, probe.json.
expect_prefix_count "$T1" 3 "T1 objects (the upload, the poster, probe.json — no rendition at all)"

# The WATCH PAGE streams /original, and it says so in the HTML a reader with no JavaScript gets.
code=$(page "$BASE/$T1"); expect "$code" 200 "watch page for T1"
has_file "$PAGE" "src=\"/api/clips/$T1/original\"" "the watch page streams the upload itself"
lacks_file "$PAGE" 'data-quality-select' "one tier is not a choice, so there is no selector"
lacks_file "$PAGE" 'data-tier-note' "nothing is encoding, so there is no note"

# ANONYMOUS access to the upload: 200, ranged 206, inline, publicly cacheable.
code=$(head_ "$BASE/api/clips/$T1/original"); expect "$code" 200 "anonymous HEAD /original (the upload IS the clip)"
T1_OLEN=$(hdr Content-Length); T1_OETAG=$(hdr ETag)
expect "$T1_OLEN" "$T_DIRECT_BYTES" "anonymous /original Content-Length"
expect_nonempty "$T1_OETAG" "anonymous /original ETag"
expect "$(hdr Accept-Ranges)" bytes "anonymous /original Accept-Ranges"
expect "$(hdr Cache-Control)" "public, max-age=3600" "anonymous /original Cache-Control"
expect_empty "$(hdr Content-Disposition)" "a public player URL must not be an attachment"
expect_empty "$(hdr Vary)" "a public object varies on nothing"
code=$(get "$BASE/api/clips/$T1/original" -H 'Range: bytes=0-99'); expect "$code" 206 "anonymous ranged GET /original"
expect "$(hdr Content-Range)" "bytes 0-99/$T1_OLEN" "anonymous /original Content-Range"
expect "$(wc -c < "$RESP" | tr -d ' ')" 100 "anonymous /original ranged body bytes"
code=$(get "$BASE/api/clips/$T1/original" -H "If-None-Match: $T1_OETAG"); expect "$code" 304 "anonymous /original If-None-Match"
# …and `?download=1` is the SAVE, which is what the owner's link uses.
code=$(head_ "$BASE/api/clips/$T1/original?download=1" -b "$JARW"); expect "$code" 200 "owner HEAD /original?download=1"
contains "$(hdr Content-Disposition)" attachment || die "?download=1 is not an attachment: $(hdr Content-Disposition)"
# /video.mp4 serves the SAME object: a client written before the tiers is unaffected.
# `media_head`/`media_download` from here on (§20 erratum 280, rule (a)): every tier assertion is
# about the OBJECT a route serves — its ETag, its length, its measured dimensions, its bytes against
# the upload's — and an object is the same object whichever way `MEDIA_DELIVERY` hands it over. The
# /original lines around them are untouched: erratum 278(a) never signs that route, in any mode.
code=$(media_head "$BASE/api/clips/$T1/video.mp4"); expect_media "$code" 200 "HEAD T1 video.mp4"
# The ONE comparison in the suite that reads a validator from TWO sources, and the only one the
# delivery mode can separate (§20 erratum 280). This app's ETag is a sha256 it computed at ingest
# (`ingest.go`, `SourceETag: res.SHA256`) and stored in the row; a bucket's is the bucket's own.
# `/original` is app-served in EVERY mode, so in `proxy` both routes answer with the app's string
# and this is an equality; in `redirect` the 302's target answers with MinIO's or R2's, which is a
# correct validator for the same bytes and a different string. The length below still ties the two
# routes to one object, and the `cmp` two lines further down is the stronger claim in both modes:
# what /video.mp4 served is byte-identical to the file that was uploaded.
if [ "$E2E_MEDIA" = proxy ]; then
  expect "$(hdr ETag)" "$T1_OETAG" "/video.mp4 serves the upload (same ETag)"
else
  expect_nonempty "$(hdr ETag)" "/video.mp4 carries a validator in redirect mode"
  # The cross-source equality stands down, but "this validator belongs to the OBJECT" does not, and it
  # is asserted here rather than assumed (§20 erratum 283 (g)). A second `media_head` of the same route
  # mints a FRESH signature — a different URL, a different query, a different signing instant — and the
  # bucket must answer with the same validator. A store whose ETag moved with the link would hand every
  # viewer a cache that never hits and every crawler a conditional GET that never matches, and nothing
  # in this suite would have noticed on the origin that actually serves them.
  T_ETAG1=$(hdr ETag)
  code=$(media_head "$BASE/api/clips/$T1/video.mp4"); expect_media "$code" 200 "HEAD T1 video.mp4, signed again"
  expect "$(hdr ETag)" "$T_ETAG1" "the bucket's validator for /video.mp4 is the OBJECT's, not the signed link's"
fi
expect "$(hdr Content-Length)" "$T1_OLEN" "/video.mp4 Content-Length = the upload's"
# The bytes really are the upload's, to the byte.
code=$(media_download "$BASE/api/clips/$T1/video.mp4" "$E2E_DIR/T1-served.mp4")
expect "$code" 200 "download T1 video.mp4"
cmp -s "$T_DIRECT" "$E2E_DIR/T1-served.mp4" || die "what /video.mp4 served is not the file that was uploaded"
echo "[e2e]   T1: the served file is byte-identical to the upload"

# A response CARRYING A CREDENTIAL is never publicly cacheable, on the one public player URL that still
# runs the credential resolver (§20 erratum 207, amended). The resolver's sliding touch re-issues the
# session cookie at most once a minute per credential, so the case is provoked deterministically with a
# FRESH session: its first request is always the one that touches.
T_FJAR="$E2E_DIR/tiers-fresh-jar.txt"; T_FJARW=$(W "$T_FJAR"); rm -f "$T_FJAR"
code=$(http POST "$BASE/api/auth/login" -c "$T_FJARW" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$E2E_USER\",\"password\":\"$E2E_PASS\"}")
expect "$code" 200 "log in again for a session whose sliding touch is due"
code=$(get "$BASE/api/clips/$T1/original" -b "$T_FJARW")
expect "$code" 200 "GET the public /original with a session whose touch is due"
expect_nonempty "$(hdr Set-Cookie)" "the sliding touch re-issued the session cookie (this step needs it to)"
expect "$(hdr Cache-Control)" "private, max-age=3600" "a response carrying Set-Cookie must not be publicly cacheable"
expect "$(hdr Vary)" "Cookie, Authorization" "…and must vary on the credential"
echo "[e2e]   the public /original answers private+Vary while it carries a Set-Cookie"
# The SECOND request on that session is inside the touch interval, carries no cookie, and is public
# again — which is what erratum 207 is for, and is not what the fix takes away.
code=$(get "$BASE/api/clips/$T1/original" -b "$T_FJARW")
expect "$code" 200 "second GET on the same session"
expect_empty "$(hdr Set-Cookie)" "the touch fired twice inside its interval"
expect "$(hdr Cache-Control)" "public, max-age=3600" "a cookie-free answer is publicly cacheable again"
rm -f "$T_FJAR"

# PRIVACY: an upload carrying a phone's own tags is REMUXED (which strips them) rather than published
# byte for byte — the direct branch is the one path that publishes bytes the pipeline never rendered
# (§20 erratum 200, amended; DEPLOY §27). Same fixture as T1 plus the four tags that matter.
T_TAGGED="$E2E_DIR/tier-tagged.mp4"
ffmpeg -hide_banner -nostdin -loglevel error -y \
  -f lavfi -i "testsrc2=duration=4:size=1280x720:rate=30" -f lavfi -i "sine=frequency=660:duration=4" \
  -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac -b:a 128k -shortest \
  -metadata location=+51.5074-000.1278/ -metadata "artist=E2E phone" \
  -metadata comment=E2E-PRIVATE-COMMENT -metadata creation_time=2019-04-05T13:14:15Z \
  -movflags +faststart "$(W "$T_TAGGED")" || die "could not build the tagged fixture"
t_moov_first "$T_TAGGED" || die "the tagged fixture is not faststart; it would be remuxed anyway"
t_tags "$T_TAGGED" > "$E2E_DIR/tiers-T4-src-tags.txt"
has_file "$E2E_DIR/tiers-T4-src-tags.txt" "E2E-PRIVATE-COMMENT" "the tagged fixture carries no comment tag"
has_file "$E2E_DIR/tiers-T4-src-tags.txt" "+51.5074" "the tagged fixture carries no location tag"

printf '%s' '{"title":"E2E tier metadata","ttl_days":1}' > "$E2E_DIR/meta-T4.json"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "metadata=<$(W "$E2E_DIR/meta-T4.json");type=application/json" \
  -F duplicate=allow -F "video=@$(W "$T_TAGGED");type=video/mp4" "$BASE/api/clips")
expect "$code" 201 "upload T4, the metadata subject ($(cat "$RESP"))"
T4=$(json id); expect_nonempty "$T4" "T4 id"
wait_ready "$T4" T4
out=$(dc exec -T app clippet admin ls "$T4") || die "admin ls T4 failed: $out"
contains "$out" "original playable=true remuxed=true" \
  || die "a tagged upload was NOT remuxed: $(printf '%s\n' "$out" | grep '^tiers:')"
# What the world can fetch — /video.mp4, which is what the page streams and what a crawler gets —
# carries none of them.
code=$(media_download "$BASE/api/clips/$T4/video.mp4" "$E2E_DIR/T4-served.mp4")
expect "$code" 200 "download T4 video.mp4"
t_tags "$E2E_DIR/T4-served.mp4" > "$E2E_DIR/tiers-T4-tags.txt"
for word in "+51.5074" "E2E phone" "E2E-PRIVATE-COMMENT" "2019-04-05"; do
  lacks_file "$E2E_DIR/tiers-T4-tags.txt" "$word" "the served file still carries '$word'"
done
echo "[e2e]   T4: the remux stripped the location, the device, the comment and the capture time"
# …and the raw upload stays the OWNER's, because it was remuxed (erratum 207's second conjunct).
code=$(head_ "$BASE/api/clips/$T4/original"); case $code in 401|403) ;; *) die "a remuxed clip's /original is anonymous → $code";; esac

step 63 "a source no browser plays gets a FULL tier at its own size, and its /original stays owner-only"
# libx265 in an .mp4: HEVC plays in Safari and in neither Chrome nor Firefox on the desktop, so
# "playable" would mean "playable for some viewers" — which is worse than a Full everyone can watch.
T_HEVC="$E2E_DIR/tier-hevc.mp4"
ffmpeg -hide_banner -nostdin -loglevel error -y \
  -f lavfi -i "testsrc2=duration=6:size=1280x720:rate=30" -f lavfi -i "sine=frequency=520:duration=6" \
  -c:v libx265 -preset ultrafast -x265-params log-level=none -pix_fmt yuv420p -tag:v hvc1 \
  -c:a aac -b:a 128k -shortest \
  -movflags +faststart "$(W "$T_HEVC")" || die "could not build the hevc fixture"
expect "$(t_codec "$T_HEVC")" hevc "the hevc fixture's codec"
T_HEVC_DIMS=$(t_dims "$T_HEVC")
printf '%s' '{"title":"E2E tier hevc","ttl_days":1}' > "$E2E_DIR/meta-T2.json"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "metadata=<$(W "$E2E_DIR/meta-T2.json");type=application/json" \
  -F duplicate=allow -F "video=@$(W "$T_HEVC");type=video/mp4" "$BASE/api/clips")
expect "$code" 201 "upload T2, the unplayable subject ($(cat "$RESP"))"
T2=$(json id); expect_nonempty "$T2" "T2 id"
wait_ready "$T2" T2

code=$(get "$BASE/api/clips/$T2"); expect "$code" 200 "GET T2"
cp "$RESP" "$E2E_DIR/tiers-T2.json"
expect "$(json card_tier)" full "T2 card_tier (720p30: the Full IS the card's file)"
expect "$(t_tier "$E2E_DIR/tiers-T2.json" original playable)" false "T2 original tier playable (hevc)"
expect_empty "$(t_tier "$E2E_DIR/tiers-T2.json" original url)" "T2 original tier url (a download, not a stream)"
expect "$(t_tier "$E2E_DIR/tiers-T2.json" full state)" ready "T2 full tier state"
expect "$(t_tier "$E2E_DIR/tiers-T2.json" full codec)" h264 "T2 full tier codec (FULL_CODEC's default)"
expect "$(t_tier "$E2E_DIR/tiers-T2.json" full url)" "$BASE/api/clips/$T2/video.mp4" "T2 full tier url"
expect "$(t_tier "$E2E_DIR/tiers-T2.json" light state)" skipped "T2 light tier state (720p30 and small)"
# The Full is the source's own size — 720p in, 720p out, no cap in either direction.
expect "$(json width)x$(json height)" "$T_HEVC_DIMS" "T2 dims = the source's own"
code=$(media_download "$BASE/api/clips/$T2/video.mp4" "$E2E_DIR/T2.mp4")
expect "$code" 200 "download T2 video.mp4"
ffprobe -v error -print_format json -show_streams -show_format "$(W "$E2E_DIR/T2.mp4")" > "$E2E_DIR/probe-T2.json"
probe_check "$E2E_DIR/probe-T2.json" "$E2E_DIR/T2.mp4" 1 "${T_HEVC_DIMS%x*}" "${T_HEVC_DIMS#*x}" 30/1
expect "$(t_codec "$E2E_DIR/T2.mp4")" h264 "the Full tier's codec, measured"
# Erratum 93's unconditional Rec.709 stamp is an ENCODE property, and this is the encoded tier.
T2_COL=$(ffprobe -v error -select_streams v:0 -show_entries stream=color_space,color_transfer,color_primaries \
  -of csv=p=0 "$(W "$E2E_DIR/T2.mp4")" | tr -d '\r')
expect "$T2_COL" "bt709,bt709,bt709" "the Full tier's colour tags (§20 erratum 93)"

# The AUTH gate, which is the security half of this bundle: an unplayable upload is the OWNER's.
code=$(head_ "$BASE/api/clips/$T2/original"); case $code in 401|403) ;; *) die "anonymous /original on an unplayable clip → $code, want 401/403";; esac
code=$(head_ "$BASE/api/clips/$T2/original" -b "$JARW"); expect "$code" 200 "owner HEAD /original (unplayable)"
contains "$(hdr Content-Disposition)" attachment || die "an owner-only original must be an attachment: $(hdr Content-Disposition)"
contains "$(hdr Cache-Control)" private || die "an owner-only original must not be publicly cacheable: $(hdr Cache-Control)"
# The same gate through the CAPTURE-TOKEN credential (§20 erratum 19): an upload token never downloads.
if [ -n "${TOK:-}" ]; then
  code=$(head_ "$BASE/api/clips/$T2/original" -H "Authorization: Bearer $TOK")
  expect "$code" 403 "a Bearer token on an owner-only original"
fi
out=$(dc exec -T app clippet admin ls "$T2") || die "admin ls T2 failed: $out"
contains "$out" "original playable=false remuxed=false" || die "admin ls T2 tiers: $(printf '%s\n' "$out" | grep '^tiers:')"
contains "$out" "full=ready(h264)" || die "admin ls T2 does not name the Full's encoder: $(printf '%s\n' "$out" | grep '^tiers:')"

# ERRATUM 235's regression, on the SAME shape as T2 and only longer. T2 is card-shaped and small, so
# the Full IS the card's file and one encode is the whole job. Make that source long enough that the
# Full cannot fit under SOCIAL_MAX_BYTES and the card needs its own copy after all — and the size is the
# ONLY term of the skip rule that can say so, because 1280x720 at 30 fps trips neither the picture term
# nor the frame-rate one.
#
# Before erratum 235 the plan had no size for a file it had not encoded yet: it decided card_tier=full and
# LightNeeded=false, and the run flipped `ready` on ONE CRF 18 encode. What happened next depended on how
# far past the cap the Full actually came out — either the commit-time rule queued the card copy AFTER the
# flip (and db.CardReady was TRUE in that window, because the card object falls back to the play object,
# which is the Full), or the Full landed just under the cap and no card copy was ever made at all. Both
# endings put a file the Light tier exists to stay under behind og:video, which Discord then remembers per
# URL; the race half is pinned by the Go tests (db.TestCardReadyWaitsForAnEncodingLightCopy and
# worker.TestUnplayableCardShapedSourceOverTheCapGetsBothTiersAtTheFlip), and what THIS step pins is the
# plan: card_tier is `light` at the flip, both files are there, and the reply's card_ready is never true
# without /light.mp4 answering.
#
# The fixture is T2's own six seconds stream-COPIED eight times end to end, so the host pays nothing:
# 48 s of 1280x720 at 30 fps estimates 1280*720*30*48.2*0.2/8 = 33.3 MB against the shipped 25 MiB cap.
# It is deliberately short enough that the two-output encode finishes inside CAPTURE_READY_WAIT on an
# unloaded box, so the card_ready=true branch below is the one usually taken rather than a dead letter.
T_LONG="$E2E_DIR/tier-hevc-long.mp4"
ffmpeg -hide_banner -nostdin -loglevel error -y -stream_loop 7 -i "$(W "$T_HEVC")" \
  -c copy -movflags +faststart "$(W "$T_LONG")" || die "could not build the long card-shaped fixture"
expect "$(t_dims "$T_LONG")" "$T_HEVC_DIMS" "the long fixture's dims (still card-shaped)"
expect "$(t_codec "$T_LONG")" hevc "the long fixture's codec (still unplayable)"
T_LONG_SECS=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$(W "$T_LONG")" | tr -d '\r')
# The premise, in the shell, from the numbers the plan uses: W*H*fps*seconds*0.2/8 against the cap the
# CONTAINER is running. A fixture that did not cross it would make this whole block prove nothing.
T_LONG_CAP=$(dc exec -T app printenv SOCIAL_MAX_BYTES 2>/dev/null | tr -d '\r')
case ${T_LONG_CAP:-} in ''|*[!0-9]*) T_LONG_CAP=26214400;; esac
awk -v secs="$T_LONG_SECS" -v cap="$T_LONG_CAP" 'BEGIN {
  est = 1280 * 720 * 30 * secs * 0.2 / 8
  printf "[e2e]   fixture: %.1fs of 720p30 hevc, the Full estimates %d bytes against a %d cap\n", secs, est, cap
  exit !(est > cap)
}' || die "the long fixture's Full does not cross SOCIAL_MAX_BYTES; this block would prove nothing"

# POST /api/upload rather than /api/clips, because the reply is where `card_ready` is stated and holding
# it is what erratum 212 promises a capture tool (§8.1's own grammar: text fields, then `file`).
T_T0=$SECONDS
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F 'title=E2E tier long hevc' -F 'ttl_days=1' \
  -F duplicate=allow -F "file=@$(W "$T_LONG");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "POST /api/upload of the long card-shaped source ($(cat "$RESP"))"
T5=$(json id); expect_nonempty "$T5" "T5 id"
T5_CARD=$(json card_ready)
echo "[e2e]   T5: state=$(json state) card_ready=$T5_CARD after $((SECONDS - T_T0))s"
# THE INVARIANT. `card_ready: true` is a promise about the file og:video names, and for this clip that
# file is the card's own copy: if the reply says the link is worth pasting, /light.mp4 answers NOW.
# T5_BRANCH records WHICH of the two legal endings this run got, and the summary row prints it: the
# false branch asserts nothing about the 201's own window, so a fixed sentence in the table could not
# tell a run that exercised the invariant from one that skipped it.
case $T5_CARD in
  true)
    code=$(media_head "$BASE/api/clips/$T5/light.mp4")
    expect_media "$code" 200 "HEAD T5 light.mp4 the moment card_ready said true"
    code=$(page "$BASE/$T5" -A "$DISCORD_UA")
    expect "$code" 200 "Discordbot GET T5 the moment its link was handed over"
    has_file "$PAGE" 'name="twitter:card" content="player"' "T5's card is a PLAYER card on the first crawler fetch"
    T5_BRANCH="card_ready=true at the 201, and /light.mp4 answered 200 in that same moment"
    ;;
  false)
    # Legal only because the budget ran out — the encode of 48 s at one cpu is longer than the ceiling.
    # What must NOT have happened is the old behaviour: `ready` with the card copy merely queued.
    echo "[e2e]   NOTE: T5's card was not ready inside CAPTURE_READY_WAIT; the flip is asserted below"
    T5_BRANCH="card_ready=false at the 201 (not ready inside CAPTURE_READY_WAIT), so only the flip below was asserted"
    ;;
  *) die "T5 card_ready = '$T5_CARD' (want true or false): $(cat "$RESP")";;
esac
wait_ready "$T5" T5

# AT THE FLIP: both files, and the card is the Light copy (§20 errata 217, 235).
code=$(get "$BASE/api/clips/$T5"); expect "$code" 200 "GET T5 at the ready flip"
cp "$RESP" "$E2E_DIR/tiers-T5.json"
expect "$(json card_tier)" light "T5 card_tier at the ready flip (the Full will not fit under the cap)"
expect "$(t_tier "$E2E_DIR/tiers-T5.json" light state)" ready "T5 light tier at the ready flip"
expect "$(t_tier "$E2E_DIR/tiers-T5.json" full state)" ready "T5 full tier at the ready flip (one decode wrote both)"
expect "$(json width)x$(json height)" "$T_HEVC_DIMS" "T5 dims (the Full's own; over-planning a card copy costs no quality)"
code=$(media_head "$BASE/api/clips/$T5/light.mp4"); expect_media "$code" 200 "HEAD T5 light.mp4 at the flip"
T5_LLEN=$(hdr Content-Length)
code=$(media_head "$BASE/api/clips/$T5/video.mp4"); expect_media "$code" 200 "HEAD T5 video.mp4 at the flip"
T5_VLEN=$(hdr Content-Length)
[ -n "$T5_LLEN" ] && [ -n "$T5_VLEN" ] && [ "$T5_LLEN" -lt "$T5_VLEN" ] \
  || die "T5's card copy is $T5_LLEN bytes against a Full of $T5_VLEN; the card copy must be the smaller file"
echo "[e2e]   T5: the card copy is $T5_LLEN bytes, the Full $T5_VLEN"
# …and no `social` job was queued behind the flip to do work the plan should have asked for.
dc exec -T app clippet admin jobs --clip "$T5" > "$E2E_DIR/tiers-T5-jobs.txt" 2>&1 \
  || die "admin jobs --clip T5 failed: $(cat "$E2E_DIR/tiers-T5-jobs.txt")"
tr -d '\r' < "$E2E_DIR/tiers-T5-jobs.txt" > "$E2E_DIR/tiers-T5-jobs.clean" && mv "$E2E_DIR/tiers-T5-jobs.clean" "$E2E_DIR/tiers-T5-jobs.txt"
grep -q "kind=social" "$E2E_DIR/tiers-T5-jobs.txt" \
  && die "T5 queued a card copy behind its ready flip: $(grep 'kind=social' "$E2E_DIR/tiers-T5-jobs.txt")"
grep -q "kind=full" "$E2E_DIR/tiers-T5-jobs.txt" \
  && die "T5 has a full job: $(grep 'kind=full' "$E2E_DIR/tiers-T5-jobs.txt")"
out=$(dc exec -T app clippet admin ls "$T5") || die "admin ls T5 failed: $out"
contains "$out" "light=ready" || die "admin ls T5 tiers: $(printf '%s\n' "$out" | grep '^tiers:')"
echo "[e2e]   T5: one transcode wrote the Full AND the card copy; card_ready is never true without /light.mp4"

step 64 "both tiers out of ONE decode: a 1080p60 mkv is READY with its Full AND its card copy"
# The subject has to satisfy two things at once: no browser can play it (so a FULL tier is wanted) and
# it is big and fast (so a LIGHT copy is wanted too) — that pair is what card-first ordering is about.
# It is built by stream-COPYING hd-1080p60.mp4 four times end to end into a MATROSKA container: the copy
# costs the host nothing, mkv is refused by the playability table (DEPLOY §27 — every media URL promises
# an mp4), and 28 seconds of 1080p60 is long enough that the two encodes cannot both finish inside one
# poll. It is also the single most common real shape of an unplayable upload: OBS records .mkv by default.
[ -s testclips/hd-1080p60.mp4 ] || die "testclips/hd-1080p60.mp4 missing (scripts/testclips.sh)"
T_BIG="$E2E_DIR/tier-1080p60-long.mkv"
ffmpeg -hide_banner -nostdin -loglevel error -y -stream_loop 3 -i "$(W "$PWD/testclips/hd-1080p60.mp4")" \
  -c copy "$(W "$T_BIG")" || die "could not build the card-first fixture"
T_BIG_DIMS=$(t_dims "$T_BIG")
expect "$T_BIG_DIMS" 1920x1080 "the card-first fixture's dims"
echo "[e2e]   fixture: matroska, $T_BIG_DIMS @ $(t_fps "$T_BIG"), $(wc -c < "$T_BIG" | tr -d ' ') bytes"

printf '%s' '{"title":"E2E tier card-first","ttl_days":1}' > "$E2E_DIR/meta-T3.json"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "metadata=<$(W "$E2E_DIR/meta-T3.json");type=application/json" \
  -F duplicate=allow -F "video=@$(W "$T_BIG");type=video/x-matroska" "$BASE/api/clips")
expect "$code" 201 "upload T3, the card-first subject ($(cat "$RESP"))"
T3=$(json id); expect_nonempty "$T3" "T3 id"
# BEFORE it is ready: the two derived tiers are `pending` and not `skipped` (§20 erratum 205, amended).
# Nothing has probed the upload yet, so "not needed — a finished answer" is the one thing that is
# certainly untrue, and a client that believed it would stop waiting for the Full that is coming.
code=$(get "$BASE/api/clips/$T3"); expect "$code" 200 "GET T3 before it is ready"
cp "$RESP" "$E2E_DIR/tiers-T3-early.json"
case $(json state) in
  queued|processing)
    expect "$(t_tier "$E2E_DIR/tiers-T3-early.json" full state)" pending "T3 full tier before the decision"
    expect "$(t_tier "$E2E_DIR/tiers-T3-early.json" light state)" pending "T3 light tier before the decision"
    expect "$(t_tier "$E2E_DIR/tiers-T3-early.json" original state)" ready "T3 original tier (the upload is in the bucket)"
    expect_empty "$(json card_tier)" "T3 card_tier before anything is decided"
    code=$(get "$BASE/api/clips/$T3/progress"); expect "$code" 200 "T3 progress before it is ready"
    cp "$RESP" "$E2E_DIR/tiers-T3-early-progress.json"
    expect "$(t_progress_tier "$E2E_DIR/tiers-T3-early-progress.json" full)" pending "T3 progress: full before the decision"
    echo "[e2e]   T3's tiers read 'pending' while nothing has been decided"
    ;;
  ready) echo "[e2e]   NOTE: T3 was already ready at the first read; the pending states are pinned by the httpapi tests" ;;
  *) die "T3 is $(json state) immediately after its upload";;
esac
wait_ready "$T3" T3

# THE READY FLIP CARRIES BOTH TIERS (§20 erratum 217, which supersedes erratum 206's ordering for this
# case). One ffmpeg decoded the source once and wrote the Full and the card's copy together, so a ready
# clip has both: the card works AND nothing ever serves the 720p30 copy as this clip's own playable
# bytes. Two processes at once were measured 2.2x slower to the first file, and back to back pays for
# the decode twice.
code=$(get "$BASE/api/clips/$T3"); expect "$code" 200 "GET T3 at the ready flip"
cp "$RESP" "$E2E_DIR/tiers-T3-ready.json"
expect "$(json card_tier)" light "T3 card_tier at the ready flip"
expect "$(t_tier "$E2E_DIR/tiers-T3-ready.json" light state)" ready "T3 light tier at the ready flip"
T3_LIGHT_URL=$(t_tier "$E2E_DIR/tiers-T3-ready.json" light url)
expect "$T3_LIGHT_URL" "$BASE/api/clips/$T3/light.mp4" "T3 light tier url"
expect "$(t_tier "$E2E_DIR/tiers-T3-ready.json" full state)" ready "T3 full tier at the ready flip (one decode wrote both)"
expect "$(json width)x$(json height)" "$T_BIG_DIMS" "T3 dims at the ready flip (the Full's own, never the card copy's)"
expect "$(json fps)" "60/1" "T3 fps at the ready flip"
# …and there is no `full` job at all: the transcode made it.
if dc exec -T app clippet admin jobs --clip "$T3" 2>/dev/null | tr -d '\r' | grep -q "kind=full"; then
  die "a full job was queued although the transcode produced the Full itself (§20 erratum 217)"
fi
# The per-tier progress says the same thing, and the watch page carries no "still coming" line.
code=$(get "$BASE/api/clips/$T3/progress"); expect "$code" 200 "T3 progress at the ready flip"
cp "$RESP" "$E2E_DIR/tiers-T3-progress.json"
expect "$(json state)" ready "T3 progress state"
expect "$(t_progress_tier "$E2E_DIR/tiers-T3-progress.json" full)" ready "T3 progress: the full tier"
expect "$(t_progress_tier "$E2E_DIR/tiers-T3-progress.json" light)" ready "T3 progress: the light tier"
code=$(page "$BASE/$T3"); expect "$code" 200 "watch page for T3"
lacks_file "$PAGE" 'Full quality still encoding.' "a clip whose tiers both landed has nothing still coming"
lacks_file "$PAGE" 'data-card-note' "…and its card is ready, so it says nothing about that either"
# The card's file is real, small, ≤30 fps and inside the Light tier's own box.
code=$(media_download "$BASE/api/clips/$T3/light.mp4" "$E2E_DIR/T3-light.mp4")
expect "$code" 200 "download T3 light.mp4"
T3_LDIMS=$(t_dims "$E2E_DIR/T3-light.mp4"); T3_LFPS=$(t_fps "$E2E_DIR/T3-light.mp4")
awk -v f="$T3_LFPS" 'BEGIN { split(f, p, "/"); if (p[2] + 0 == 0 || p[1] / p[2] > 30.0001) exit 1 }' \
  || die "T3's light copy runs at $T3_LFPS, want <= 30 fps"
[ "${T3_LDIMS%x*}" -le 1280 ] && [ "${T3_LDIMS#*x}" -le 720 ] || die "T3's light copy is $T3_LDIMS, past its own box"
echo "[e2e]   T3 light copy: $T3_LDIMS @ $T3_LFPS, $(wc -c < "$E2E_DIR/T3-light.mp4" | tr -d ' ') bytes"
# The card TAGS point at that file and state ITS dimensions (§20 erratum 123).
T3_PW=$(jget "$(W "$E2E_DIR/tiers-T3-ready.json")" poster.width); T3_PH=$(jget "$(W "$E2E_DIR/tiers-T3-ready.json")" poster.height)
code=$(page "$BASE/$T3" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot share page for T3"
card_check player "$PAGE" "$BASE" "$T3" "E2E tier card-first" "$T3_PW" "$T3_PH" "${T3_LDIMS%x*}" "${T3_LDIMS#*x}" "$SECURE"

# ONE JOB DID ALL OF IT, from the job rows: a transcode that is `done`, and no `full` job beside it.
dc exec -T app clippet admin jobs --clip "$T3" > "$E2E_DIR/tiers-T3-jobs.txt" 2>&1 \
  || die "admin jobs --clip failed: $(cat "$E2E_DIR/tiers-T3-jobs.txt")"
tr -d '\r' < "$E2E_DIR/tiers-T3-jobs.txt" > "$E2E_DIR/tiers-T3-jobs.clean" && mv "$E2E_DIR/tiers-T3-jobs.clean" "$E2E_DIR/tiers-T3-jobs.txt"
T3_TJOB=$(sed -n 's/^#\([0-9]*\) .*kind=transcode .*/\1/p' "$E2E_DIR/tiers-T3-jobs.txt" | head -1)
expect_nonempty "$T3_TJOB" "T3's transcode job id (admin jobs --clip)"
contains "$(sed -n "/^#$T3_TJOB /p" "$E2E_DIR/tiers-T3-jobs.txt")" "state=done" \
  || die "T3's transcode did not finish: $(sed -n "/^#$T3_TJOB /p" "$E2E_DIR/tiers-T3-jobs.txt")"
grep -q "kind=full" "$E2E_DIR/tiers-T3-jobs.txt" \
  && die "T3 has a full job: $(grep 'kind=full' "$E2E_DIR/tiers-T3-jobs.txt")"
echo "[e2e]   one transcode job (#$T3_TJOB) wrote the poster, the Full and the card's copy"
T3_ORDER=$(dc exec -T app clippet admin ls "$T3" --stat 2>/dev/null | tr -d '\r') || true
if contains "$T3_ORDER" MISSING; then die "admin ls T3 --stat reports a missing object: $T3_ORDER"; fi
# The clip is on its Full: the source's own 1920x1080 at 60 fps, never downscaled.
code=$(get "$BASE/api/clips/$T3"); expect "$code" 200 "GET T3 with its Full"
cp "$RESP" "$E2E_DIR/tiers-T3-full.json"
expect "$(json width)x$(json height)" "$T_BIG_DIMS" "T3 dims after the Full (the source's own, not downscaled)"
expect "$(json fps)" "60/1" "T3 fps after the Full"
expect "$(t_tier "$E2E_DIR/tiers-T3-full.json" full state)" ready "T3 full tier state"
expect "$(json card_tier)" light "T3 card_tier after the Full (the card keeps its own copy)"
expect "$(t_tier "$E2E_DIR/tiers-T3-full.json" original playable)" false "T3 original tier (matroska is never streamed)"
# The two files are separate objects, and /video.mp4 is the FAITHFUL one: nobody streams the card copy.
code=$(media_head "$BASE/api/clips/$T3/light.mp4"); expect_media "$code" 200 "HEAD T3 light.mp4"
T3_LETAG=$(hdr ETag)
code=$(media_head "$BASE/api/clips/$T3/video.mp4"); expect_media "$code" 200 "HEAD T3 video.mp4"
[ "$(hdr ETag)" != "$T3_LETAG" ] || die "T3's /video.mp4 serves the card copy; it must serve the Full"
code=$(media_download "$BASE/api/clips/$T3/video.mp4" "$E2E_DIR/T3-full.mp4")
expect "$code" 200 "download T3 video.mp4"
expect "$(t_dims "$E2E_DIR/T3-full.mp4")" "$T_BIG_DIMS" "the Full's measured dims"
# THE SELECTOR now lists both tiers this clip has, with real numbers, server-rendered. T3's upload is
# matroska, so its tiers are the Full and the Light — the Original is present as a download and is never
# offered as something to stream (that half is step 62's, on a clip whose upload DOES play).
code=$(page "$BASE/$T3"); expect "$code" 200 "watch page for T3 with two tiers"
has_file "$PAGE" 'data-quality-select' "the quality selector renders server-side"
has_file "$PAGE" 'data-tier="full"' "…and offers the Full tier"
has_file "$PAGE" 'data-tier="light"' "…and the Light copy"
lacks_file "$PAGE" 'data-tier="original"' "…and never the unplayable upload"
has_file "$PAGE" "1920×1080" "…labelled with real numbers"
has_file "$PAGE" "60fps" "…including the frame rate, as a person says it"
lacks_file "$PAGE" 'style=' "the selector adds no inline style"
lacks_file "$PAGE" '<style' "…and no style element"

step 65 "every tier key is invalidated together: a trim replaces them, orphans stays clean"
# A trim re-derives the clip from the retained original, so it replaces the rendition, the poster and
# the card copy in ONE commit and supersedes the objects they used to be. The Original TIER stops
# being offered — the clip is a WINDOW of the upload now — and /original goes back to owner-only,
# which is the security half of the same change.
# Depends on `wait_trim`, a HARNESS global (scripts/e2e.sh, step 21) — in scope in a sourced drop-in and
# in a lane subshell alike, so the `command -v … || die` that used to stand here could never fire.
T3_BEFORE=$(prefix_count "$T3")
T3_RKEY=$(t_admin_field "$T3" rendition); T3_LKEY=$(t_admin_field "$T3" social)
expect_nonempty "$T3_RKEY" "T3's rendition key before the trim"
expect_nonempty "$T3_LKEY" "T3's light key before the trim"
code=$(http POST "$BASE/api/clips/$T3/trim" -b "$JARW" -H 'Content-Type: application/json' -d '{"start_ms":1000,"end_ms":5000}')
expect "$code" 202 "trim T3 ($(cat "$RESP"))"
wait_trim "$T3" T3
# The cut is NOT downscaled either: a trim is planned in the Full tier's box (§20 erratum 208).
code=$(get "$BASE/api/clips/$T3"); expect "$code" 200 "GET T3 after the trim"
cp "$RESP" "$E2E_DIR/tiers-T3-trim.json"
expect "$(json width)x$(json height)" "$T_BIG_DIMS" "T3 dims after the trim (a cut must not cost a clip its resolution)"
T3_RKEY2=$(t_admin_field "$T3" rendition)
[ -n "$T3_RKEY2" ] && [ "$T3_RKEY2" != "$T3_RKEY" ] || die "the trim did not replace T3's rendition ($T3_RKEY2)"
# The Original tier is no longer offered, and the upload is the owner's again.
expect "$(t_tier "$E2E_DIR/tiers-T3-trim.json" original playable)" false "a trimmed clip does not offer its whole upload"
code=$(head_ "$BASE/api/clips/$T3/original"); case $code in 401|403) ;; *) die "a trimmed clip's /original is anonymous → $code";; esac
code=$(head_ "$BASE/api/clips/$T3/original" -b "$JARW"); expect "$code" 200 "the owner still has the upload after a trim"
# The card copy is re-derived, and the superseded objects are GONE from the bucket. The etag to wait
# PAST is the new rendition's: the trim's commit NULLed the card columns, so until the fresh light copy
# lands `/social.mp4` falls back to the cut itself (41-social.sh makes the same move for the same reason).
code=$(media_head "$BASE/api/clips/$T3/video.mp4"); expect_media "$code" 200 "HEAD T3 video.mp4 after the trim"
T3_VETAG2=$(hdr ETag)
wait_social "$T3" "T3 (after the trim)" "$T3_VETAG2"
T3_LKEY2=$(t_admin_field "$T3" social)
[ -n "$T3_LKEY2" ] && [ "$T3_LKEY2" != "$T3_LKEY" ] || die "the light key did not change across the trim ($T3_LKEY2)"
prefix_names "$T3" > "$E2E_DIR/tiers-keys-T3.txt"
lacks_file "$E2E_DIR/tiers-keys-T3.txt" "${T3_RKEY##*/}" "the pre-trim rendition is still in the bucket"
lacks_file "$E2E_DIR/tiers-keys-T3.txt" "${T3_LKEY##*/}" "the pre-trim light copy is still in the bucket"
has_file "$E2E_DIR/tiers-keys-T3.txt" "${T3_RKEY2##*/}" "the new rendition is not in the bucket"
has_file "$E2E_DIR/tiers-keys-T3.txt" "${T3_LKEY2##*/}" "the new light copy is not in the bucket"
expect_prefix_count "$T3" "$T3_BEFORE" "T3's object count after the trim (one in, one out, per tier)"
out=$(dc exec -T app clippet admin ls "$T3" --stat) || die "admin ls T3 --stat failed: $out"
if contains "$out" MISSING; then die "admin ls T3 --stat reports a missing object after the trim: $out"; fi
# THE REPLACEMENT half of this step (ROADMAP #31). T1 is the fast path: ready, and no rendition object
# at all. The route used to refuse exactly this clip with 409 `not_ready` while the page rendered the
# form for it (§20 erratum 201, amended), so the assertion is the 202 — and then that the swap lands the
# clip back on the fast path with its tiers re-decided.
T1_SRCKEY=$(t_admin_field "$T1" source)
expect_nonempty "$T1_SRCKEY" "T1's source key before the replacement"
T_REPL="$E2E_DIR/tier-replacement.mp4"
ffmpeg -hide_banner -nostdin -loglevel error -y \
  -f lavfi -i "testsrc2=duration=5:size=854x480:rate=30" -f lavfi -i "sine=frequency=880:duration=5" \
  -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac -b:a 128k -shortest \
  -movflags +faststart "$(W "$T_REPL")" || die "could not build the replacement fixture"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F duplicate=allow -F "file=@$(W "$T_REPL");type=video/mp4" "$BASE/api/clips/$T1/replace")
expect "$code" 202 "POST replace on a clip whose upload IS the clip ($(cat "$RESP"))"
t0=$SECONDS
while :; do
  code=$(get "$BASE/api/clips/$T1/replace" -b "$JARW"); expect "$code" 200 "T1 replace status"
  case $(json state) in
    done) echo "[e2e]   T1 replaced after $((SECONDS - t0))s"; break ;;
    failed) die "T1's replacement failed: $(json error)" ;;
    queued|running) ;;
    *) die "T1 replace state '$(json state)'" ;;
  esac
  code=$(get "$BASE/api/clips/$T1"); expect "$code" 200 "GET T1 while its replacement runs"
  expect "$(json state)" ready "T1 stays ready throughout its replacement"
  [ $((SECONDS - t0)) -lt 180 ] || die "T1 not replaced within 180s"
  sleep 1   # was 2 (lane speed2): …/replace and the clip GET carry no limiter; the deadline is time
done
code=$(get "$BASE/api/clips/$T1" -b "$JARW"); expect "$code" 200 "GET T1 after the replacement"
cp "$RESP" "$E2E_DIR/tiers-T1-replaced.json"
expect "$(json width)x$(json height)" 854x480 "T1 dims after the replacement (the new file's own)"
expect "$(json card_tier)" original "T1 card_tier after the replacement (still the fast path)"
expect "$(t_tier "$E2E_DIR/tiers-T1-replaced.json" original playable)" true "T1's new upload is playable too"
expect "$(t_tier "$E2E_DIR/tiers-T1-replaced.json" full state)" skipped "T1 full tier after the replacement"
T1_SRCKEY2=$(t_admin_field "$T1" source)
[ -n "$T1_SRCKEY2" ] && [ "$T1_SRCKEY2" != "$T1_SRCKEY" ] || die "the replacement did not change T1's source key ($T1_SRCKEY2)"
out=$(dc exec -T app clippet admin ls "$T1") || die "admin ls T1 failed: $out"
contains "$out" "rendition: (none" || die "T1 left the fast path across a replacement: $(printf '%s\n' "$out" | grep '^rendition:')"
prefix_names "$T1" > "$E2E_DIR/tiers-keys-T1.txt"
lacks_file "$E2E_DIR/tiers-keys-T1.txt" "${T1_SRCKEY##*/}" "the replaced original is still in the bucket"
rm -f "$T_REPL"

# `orphans` sees nothing loose: every tier key the row stopped naming was deleted with it.
out=$(dc exec -T app clippet admin orphans --min-age 0 2>&1) || die "admin orphans failed: $out"
if contains "$out" "clips/$T3/"; then die "admin orphans reports objects of T3 after a tier swap:\n$out"; fi
echo "[e2e]   orphans clean after the tier swap"

step 66 "the quality selector and the per-tier states are server-rendered facts, not client guesses"
# T1 (one tier) got no control at all — step 62 checked that. T3 has two tiers with files, the Full and
# the Light, so it gets one <option> each and nothing for the Original its upload cannot provide. It is
# all checked in the HTML: the selector is progressive enhancement, so the page has to be right before
# any script runs.
code=$(page "$BASE/$T3"); expect "$code" 200 "watch page for T3 (trimmed, two tiers)"
has_file "$PAGE" '<select name="quality" data-quality-select>' "the selector is a real select"
has_file "$PAGE" 'data-tier="full"' "the Full tier is an option after a trim (the cut IS the Full)"
has_file "$PAGE" 'data-tier="light"' "the Light copy is an option"
lacks_file "$PAGE" 'data-tier="original"' "an unplayable upload is never offered as a tier"
has_file "$PAGE" 'selected="selected"' "the best tier is preselected, so a scriptless page plays it"
# The DTO and the page agree about which tiers exist — one function decides for both. It is re-read
# HERE rather than reused from step 65: the DTO captured immediately after the trim says
# `light: encoding`, which was true at that instant and is the whole point of the state.
code=$(get "$BASE/api/clips/$T3"); expect "$code" 200 "GET T3 with every tier settled"
cp "$RESP" "$E2E_DIR/tiers-T3-settled.json"
expect "$(t_tier "$E2E_DIR/tiers-T3-settled.json" full state)" ready "T3 full tier after the trim"
expect "$(t_tier "$E2E_DIR/tiers-T3-settled.json" light state)" ready "T3 light tier after the trim"
expect "$(t_tier "$E2E_DIR/tiers-T3-settled.json" original state)" ready "T3 original tier state (the upload is still there)"
expect "$(t_tier "$E2E_DIR/tiers-T3-settled.json" original playable)" false "…and is still not something to stream"
# The progress route reports every tier as a finished state now, so a poller stops.
code=$(get "$BASE/api/clips/$T3/progress"); expect "$code" 200 "T3 progress after every tier landed"
cp "$RESP" "$E2E_DIR/tiers-T3-progress2.json"
for tier in original full light; do
  state=$(t_progress_tier "$E2E_DIR/tiers-T3-progress2.json" "$tier")
  case $state in ready|skipped|failed) ;; *) die "T3's $tier tier is still '$state' after every job finished";; esac
done
# …and the page stops polling with them.
code=$(page "$BASE/$T3"); expect "$code" 200 "watch page for T3 with nothing left to encode"
lacks_file "$PAGE" 'data-progress-url' "a clip with no tier encoding must not poll"
lacks_file "$PAGE" 'data-tier-note' "…and has nothing to say about a missing tier"
# The EMBED plays the best tier and offers no selector: an embedder chose the player, not the bitrate.
code=$(page "$BASE/embed/$T3"); expect "$code" 200 "embed for T3"
has_file "$PAGE" '<video' "the embed has a player"
lacks_file "$PAGE" 'data-quality' "the embed carries no quality control"

# The POLL LIMITER (120/min/IP, its own bucket — §20 erratum 211, amended). A ready clip whose better
# tier is encoding is polled by every viewer holding the link, so this route stopped being the owner's
# own tab and had to stop being unlimited. It is drained from an address of its own (`apart`, the
# header above): the bucket the rest of the run polls from is not the one this empties.
# The burst goes through the harness's `burst_429` (§20 erratum 283 (f)): the same requests, against
# the same live ceiling, with every pre-refusal answer still asserted 200 and the refusal still
# required inside 400 — but in one `curl` process per chunk instead of one per request. This burst
# alone was 145 processes on the last public gate and 283 on a lane stack.
burst_429 "$BASE/api/clips/$T3/progress" 400 "the poll limiter on /progress" apart
T_PN=$BURST_N
expect "$(json error)" rate_limited "progress 429 error code"
expect_nonempty "$(hdr Retry-After)" "the 429 carries Retry-After"
echo "[e2e]   the single progress route is rate limited; 429 after $T_PN requests"

# Clean up: every clip this bundle created is its own.
for id in "$T1" "$T2" "$T3" "$T4" "$T5"; do
  [ -n "$id" ] || continue
  code=$(http DELETE "$BASE/api/clips/$id" -b "$JARW"); expect_deleted "$code" "$id" "DELETE $id"
  expect_prefix_count "$id" 0 "prefix empty after deleting $id"
done
# T_DIRECT only when this step built it: on a stack with the shared matrix it IS
# testclips/faststart-720p30.mp4, which every future run expects to find cached. An `if` and not a
# `[ … ] && rm`: under set -e a false test on the last command of a line ends the bundle.
if [ "$T_DIRECT_OWNED" = 1 ]; then rm -f "$T_DIRECT"; fi
rm -f "$T_HEVC" "$T_LONG" "$T_BIG" "$T_TAGGED" "$E2E_DIR/T1-served.mp4" "$E2E_DIR/T2.mp4" \
  "$E2E_DIR/T3-light.mp4" "$E2E_DIR/T3-full.mp4" "$E2E_DIR/T4-served.mp4"

# This bundle's rows in the closing summary table (scripts/e2e.d/README.md).
summary_row T1 "${T1:-}" "720p30 faststart mp4: STREAMED as uploaded (no rendition object, no encoder), /original anonymous + ranged; deleted"
summary_row T2 "${T2:-}" "hevc 720p30: a FULL tier at the source's own size, Rec.709 stamped, /original owner-only; deleted"
summary_row T3 "${T3:-}" "1080p60 x4 in matroska: ONE decode wrote the Full 1920x1080@60 and the card copy together, then trimmed (every tier key swapped, orphans clean); deleted"
summary_row T4 "${T4:-}" "720p30 faststart mp4 carrying GPS/device/comment/creation tags: REMUXED to strip them, /original owner-only; deleted"
summary_row T5 "${T5:-}" "hevc 720p30 x8 (48 s) via POST /api/upload: the Full will not fit under SOCIAL_MAX_BYTES, so ONE decode wrote the Full AND the card copy and card_ready was never true without /light.mp4 (§20 erratum 235) — this run: ${T5_BRANCH:-branch not recorded}; deleted"
