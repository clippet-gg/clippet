# 70-web.sh — bundle W1: the trim scrubber's filmstrip, and the four library behaviours the owner
# asked for on 2026-09-08; step 70 amended by bundle W2 for the filmstrip's owner gate (§20 erratum 227).
# Sourced by scripts/e2e.sh before the step-16 log gate (see e2e.d/README.md).
# Steps 70-72. Everything it creates it destroys; nothing it does outlives the run.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=W1_ buckets=view
# e2e-covers: internal/httpapi/filmstrip.go internal/worker/filmstrip.go internal/transcode/filmstrip.go
# e2e-covers: internal/db/filmstrip.go internal/worker/vocabulary.go internal/web/pages.go internal/web/format.go
# e2e-covers: web/templates/index.html web/static/app.js web/static/app.css
#   Its three clips are its own, every count is scoped to one of them, and the orphans check names W1A.
#   It is APART from 43-watch.sh (which declares it): its filmstrip poll is one request a second against
#   the `view` bucket (60 a minute per address, a compile-time constant), which leaves no room for that
#   file's preview-GIF poll to run at the same time.
#
# Every variable and clip id here is prefixed W1_/W1A/W1B so nothing collides with the harness's own
# A-E/W/X or with another drop-in (e2e.d/README.md's first rule).
#
# It passes over plain http AND over https: nothing below asserts a scheme, and the one assertion
# about an absolute URL compares against $BASE.

# ---------------------------------------------------------------------------------------------------
step 70 "the trim scrubber's FILMSTRIP (§20 errata 220, 227): the owner makes it, anyone reads it, measured, dropped by a trim, gone with the clip"

# The fixture is 12 seconds of MOVING colour, so the sheet's tiles are demonstrably different frames
# rather than the same picture two dozen times — which is what a wrong `fps` expression produces and
# what a geometry check alone cannot see. Keyframes every second (-g 30 -sc_threshold 0) so the trim
# below cuts where it is asked to instead of snapping back to a distant keyframe.
W1_SRC="$E2E_DIR/w1-filmstrip-src.mp4"
ffmpeg -hide_banner -nostdin -y \
  -f lavfi -i "color=c=black:s=640x360:r=30:d=12" \
  -vf "geq=lum='32+T*17':cb=128:cr=128" \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p -g 30 -keyint_min 30 -sc_threshold 0 \
  "$(W "$W1_SRC")" > "$E2E_DIR/w1-ffmpeg.txt" 2>&1 \
  || die "could not build the filmstrip fixture: $(tail -3 "$E2E_DIR/w1-ffmpeg.txt")"

code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "title=E2E filmstrip ramp" -F "ttl_days=1" \
  -F duplicate=allow -F "file=@$(W "$W1_SRC");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "upload the filmstrip fixture ($(cat "$RESP"))"
W1A=$(json id); expect_nonempty "$W1A" "W1A id"

# w1_strip <id> <outfile> [curl args…] → status; the body goes to outfile (a JPEG on 200, the JSON
# envelope otherwise) and the headers to $HDR. ANONYMOUS unless a credential is passed in: since §20
# erratum 227 the caller decides the answer, so every request below says which caller it is making.
w1_strip() {
  local id=$1 out=$2; shift 2
  curl -sS -o "$(W "$out")" -D "$HDRW" -w '%{http_code}' "$@" "$BASE/api/clips/$id/filmstrip.jpg"
}
# w1_own_strip — the same request as the clip's OWNER, the one caller that can make a sheet.
w1_own_strip() { w1_strip "$1" "$2" -b "$JARW"; }
# w1_field <outfile> <path> — a field of the JSON envelope w1_strip wrote. Never $RESP: the sheet's
# bytes and the error envelopes share one file here.
w1_field() { jget "$(W "$1")" "$2"; }

W1_JPG="$E2E_DIR/w1-strip.jpg"
# The gate does not wait for the clip: a caller who is not the owner is told there is no sheet
# whatever state the clip is in, which is the assertion this moment can make before the transcode
# finishes (it holds either way, which is what makes it safe to put here).
code=$(w1_strip "$W1A" "$W1_JPG"); expect "$code" 404 "an anonymous sheet request while W1A is new"
wait_ready "$W1A" W1A

# w1_sheets <id> → objects under clips/<id>/filmstrip/. The clip's WHOLE prefix is the wrong counter
# here: whether a social rendition is produced depends on the rendition's own size and frame rate
# (§20 erratum 121), so a total would be asserting something this step is not about.
#
# It narrows the harness's `prefix_names` rather than listing the bundled MinIO as it used to: that
# listing was this stack's store only while the stack kept its media there, and an instance on R2 read
# every count here as 0 (§20 erratum 281). `prefix_names` dies on a listing it cannot parse, so a 0 from
# here is a real zero.
w1_sheets() {
  local names
  names=$(prefix_names "$1")
  printf '%s\n' "$names" | grep -c "clips/$1/filmstrip/" || true
}
# w1_expect_sheets <id> <want> <what> — w1_sheets, POLLED until it says <want> (≤20 s): a superseded
# object is deleted AFTER the row that stopped referencing it is committed (best effort, by design), so a
# check that keys on a DB observable can look at the bucket inside that window. It was one retry after a
# bare `sleep 3`, which let the clock decide a pass on a loaded box (lane `nits8`'s timing sweep); the
# poll waits for the EVENT and a count that never arrives still fails with the last answer.
w1_expect_sheets() {
  local got n=0
  got=$(w1_sheets "$1")
  while [ "$got" != "$2" ] && [ "$n" -lt 40 ]; do
    [ "$n" -gt 0 ] || echo "[e2e]   $3: bucket says $got, want $2 — polling (the object delete follows the commit)"
    n=$((n + 1)); sleep 0.5
    got=$(w1_sheets "$1")
  done
  expect "$got" "$2" "$3"
}
expect "$(w1_sheets "$W1A")" 0 "a fresh clip has no filmstrip until somebody asks"

# w1_wait_strip <id> <outfile> <label> — poll AS THE OWNER until the sheet is served (≤180 s), which
# is what the trim control's own poll does. A 202 is the normal first answer and never an error;
# anything else is fatal at once.
w1_wait_strip() {
  local n=0 c
  while [ "$n" -lt 180 ]; do
    c=$(w1_own_strip "$1" "$2")
    case $c in
      200) echo "[e2e]   $3: the sheet was served after ${n}s"; return 0;;
      202) ;;
      *)   die "$3: GET filmstrip.jpg → $c (want 200 or 202): $(head -c 300 "$2")";;
    esac
    n=$((n + 1)); sleep 1
  done
  die "$3: no filmstrip within 180s"
}
# w1_dims <file> → "WxH" of an image, via host ffprobe. The sheet's real pixels are the only honest
# check that the geometry the API publishes is the geometry the file has.
w1_dims() {
  ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0:s=x "$(W "$1")" \
    2>/dev/null | tr -d ' \r'
}

# (a) The GATE (§20 erratum 227). Nobody but the clip's owner may cause a filmstrip pass — the pass
#     decodes the whole rendition and only the owner's trim control ever reads the sheet — so every
#     other caller is answered 404 and the QUEUE is asserted empty after each one. Then the owner's
#     own request makes exactly one job, and a second finds THAT job rather than making another.
W1_JOBS="$E2E_DIR/w1-jobs.txt"
# w1_jobs [<clip id>] → how many filmstrip job ROWS that clip has; W1A when no id is given, which is
# every call site below. The argument exists so the listing and the count can never disagree: the body
# used to close over the global, so a call meant for another clip would have silently counted W1A's.
# Only rows are counted: `admin jobs` ends with its own "N job(s) (kind=… clip=…)" summary line, which
# carries the id too — counting that as a job is how this assertion first read 2.
w1_jobs() {
  local id=${1:-$W1A}
  dc exec -T app clippet admin jobs --kind filmstrip --clip "$id" > "$W1_JOBS" 2>&1 \
    || die "admin jobs --kind filmstrip failed: $(cat "$W1_JOBS")"
  { grep -c "^#.*clip=$id" "$W1_JOBS" || true; }
}

code=$(w1_strip "$W1A" "$W1_JPG"); expect "$code" 404 "an anonymous GET of a ready clip with no sheet"
expect "$(w1_field "$W1_JPG" error)" not_found "the anonymous 404's error code"
# Every no-sheet answer depended on a credential, so it says so and forbids storing.
expect "$(hdr cache-control)" 'private, no-store' "the 404's Cache-Control"
expect "$(hdr vary)" 'Cookie, Authorization' "the 404's Vary"
expect "$(w1_jobs)" 0 "an anonymous request queued a filmstrip job"

# The owner's own API TOKEN is no better, with either scope: a token is an upload credential
# (erratum 19), and errata 111-112 widened it by two READS of rows, not by the right to spend a
# transcode slot. `read` is the interesting one — it can list this very clip.
code=$(http POST "$BASE/api/auth/tokens" -b "$JARW" -H 'Content-Type: application/json' \
  -d '{"name":"e2e w1 read","scopes":["read"]}')
expect "$code" 201 "mint a read-scope token ($(cat "$RESP"))"
W1_TOK=$(json token); W1_TID=$(json id)
expect_nonempty "$W1_TOK" "the read token"
code=$(w1_strip "$W1A" "$W1_JPG" -H "Authorization: Bearer $W1_TOK")
expect "$code" 404 "the owner's own read-scope token asking for a sheet"
expect "$(w1_jobs)" 0 "a read-scope token queued a filmstrip job"
# …and it really is a working credential on the route that scope IS for, so the 404 above is the
# gate and not a dead token.
code=$(get "$BASE/api/clips?limit=1" -H "Authorization: Bearer $W1_TOK")
expect "$code" 200 "the same token on GET /api/clips (the scope it was minted for)"
code=$(http DELETE "$BASE/api/auth/tokens/$W1_TID" -b "$JARW"); expect "$code" 204 "revoke the read token"

# Another ACCOUNT's session is a stranger here, whatever it owns elsewhere. The harness's second user
# (step 3 registers it and logs it out) has no session here, so this logs it in — one RATE_AUTH_PER_MIN token
# (10/min/IP shipped, one back every 6 s; a run raises it to 120 unless E2E_RATE_AUTH_PER_MIN says
# otherwise), which is why a 429 is retried once rather than fatal.
W1_JARS="$E2E_DIR/w1-cookies-stranger.txt"; W1_JARSW=$(W "$W1_JARS"); rm -f "$W1_JARS"
code=$(http POST "$BASE/api/auth/login" -c "$W1_JARSW" -H 'Content-Type: application/json' \
  -d "{\"username\":\"${E2E_USER}_b\",\"password\":\"$E2E_PASS\"}")
if [ "$code" = 429 ]; then
  echo "[e2e]   the auth limiter is warm; waiting 7s for one token"
  sleep 7
  code=$(http POST "$BASE/api/auth/login" -c "$W1_JARSW" -H 'Content-Type: application/json' \
    -d "{\"username\":\"${E2E_USER}_b\",\"password\":\"$E2E_PASS\"}")
fi
expect "$code" 200 "log the second user in for the filmstrip gate ($(cat "$RESP"))"
code=$(w1_strip "$W1A" "$W1_JPG" -b "$W1_JARSW")
expect "$code" 404 "another account's session asking for a sheet"
expect "$(w1_jobs)" 0 "another account's session queued a filmstrip job"
code=$(http POST "$BASE/api/auth/logout" -b "$W1_JARSW" || echo 000)
echo "[e2e]   logout ${E2E_USER}_b -> $code"
rm -f "$W1_JARS"

# The OWNER, and only now: 202, the pace, and one job.
code=$(w1_own_strip "$W1A" "$W1_JPG"); expect "$code" 202 "the owner's first GET filmstrip.jpg"
expect "$(hdr retry-after)" 5 "the 202's Retry-After"
expect "$(hdr cache-control)" 'private, no-store' "the 202's Cache-Control"
expect "$(hdr vary)" 'Cookie, Authorization' "the 202's Vary"
expect "$(w1_field "$W1_JPG" state)" queued "the 202's state"
code=$(w1_own_strip "$W1A" "$W1_JPG")
case $code in 200|202) ;; *) die "the owner's second GET filmstrip.jpg → $code";; esac
expect "$(w1_jobs)" 1 "two owner requests must produce exactly ONE filmstrip job"
has_file "$W1_JOBS" "kind=filmstrip" "admin jobs does not know the filmstrip kind"
# A stranger asking while that job is live still adds nothing. WHICH answer they get is a race this
# step must not pretend to control: this fixture's pass takes about a second, and the `admin jobs`
# call above is an in-container process, so the sheet may already have landed — in which case 200 is
# the RIGHT answer (the served-to-anybody case, asserted in (b) below). The job count is the
# load-bearing half and is asserted either way.
code=$(w1_strip "$W1A" "$W1_JPG")
case $code in
  404) echo "[e2e]   a stranger asking while the job was live was told there is no sheet";;
  200) echo "[e2e]   the sheet landed before the stranger's probe; the 200 is (b)'s case, not a gate failure";;
  *)   die "an anonymous GET while a filmstrip job is live → $code (want 404, or 200 once the sheet has landed)";;
esac
expect "$(w1_jobs)" 1 "an anonymous request added a second job to a live one"

# (b) The sheet lands, and its PIXELS match the geometry the DTO publishes.
w1_wait_strip "$W1A" "$W1_JPG" W1A
expect "$(hdr content-type)" image/jpeg "the sheet's Content-Type"
W1_ETAG=$(hdr etag); expect_nonempty "$W1_ETAG" "the sheet's ETag"
expect "$(hdr accept-ranges)" bytes "the sheet's Accept-Ranges"
contains "$(hdr cache-control)" 'max-age=3600' || die "the sheet is not cacheable: $(hdr cache-control)"
# The sheet the OWNER just made is served to ANYBODY — the URL may already be inside a page a cache
# handed somebody — and that answer is the public one it always was: shared-cacheable, no Vary, and
# no session cookie riding along on it, which is the whole reason erratum 227 resolves the credential
# on the other arm alone.
code=$(w1_strip "$W1A" "$W1_JPG"); expect "$code" 200 "an anonymous GET of a sheet that exists"
contains "$(hdr cache-control)" 'max-age=3600' || die "the anonymous sheet is not cacheable: $(hdr cache-control)"
expect_empty "$(hdr vary)" "a served sheet must carry no Vary"
lacks_file "$HDR" 'Set-Cookie' "the anonymous sheet answered with a cookie"
code=$(w1_own_strip "$W1A" "$W1_JPG"); expect "$code" 200 "the owner's own GET of a sheet that exists"
contains "$(hdr cache-control)" 'max-age=3600' || die "the owner's sheet answer is not cacheable: $(hdr cache-control)"
expect_empty "$(hdr vary)" "the owner's sheet answer must carry no Vary"
# One-way check: the sliding touch fires at most once a minute per credential, so this cannot prove
# a cookie WOULD have been re-issued — but it fails the moment the serving arm starts resolving one.
lacks_file "$HDR" 'Set-Cookie' "a session cookie was re-issued onto a public, cacheable sheet"
code=$(get "$BASE/api/clips/$W1A" -b "$JARW"); expect "$code" 200 "GET W1A after the sheet landed"
W1_TILES=$(json filmstrip.tiles); W1_TW=$(json filmstrip.tile_width)
W1_TH=$(json filmstrip.tile_height); W1_IVL=$(json filmstrip.interval_ms)
W1_URL=$(json filmstrip.url); W1_DUR=$(json duration_ms)
expect "$W1_TILES" 24 "the sheet's tile count"
expect "$W1_TW" 160 "the sheet's tile width"
# The interval is the MEASURED rendition length over the tile count — never a literal, because an
# encoder's own idea of a 12-second clip is 12000 ms give or take a frame.
expect "$W1_IVL" "$((W1_DUR / W1_TILES))" "the interval of $W1_TILES tiles over $W1_DUR ms"
expect "$W1_URL" "$BASE/api/clips/$W1A/filmstrip.jpg" "filmstrip.url is absolute on this origin"
# 160 px of a 640x360 source is 160x90.
expect "$W1_TH" 90 "the sheet's tile height (measured from the JPEG, never assumed)"
expect "$(w1_dims "$W1_JPG")" "$((W1_TILES * W1_TW))x$W1_TH" "the JPEG's real pixels vs the published geometry"
# The tiles are DIFFERENT frames, in order: the fixture's luma rises with time, so the mean luma of
# the left third of the sheet must be well below that of the right third. One measurement, and it
# rules out a sheet of one frame repeated and a sheet in the wrong order at the same time.
w1_third() {
  local out="$E2E_DIR/w1-third-$2.jpg"
  ffmpeg -hide_banner -nostdin -y -i "$(W "$W1_JPG")" \
    -vf "crop=${1}:$W1_TH:$3:0" "$(W "$out")" > "$E2E_DIR/w1-crop-$2.txt" 2>&1 \
    || die "could not crop the sheet: $(tail -3 "$E2E_DIR/w1-crop-$2.txt")"
  yavg "$out" "sheet-$2"
}
W1_THIRD=$((W1_TILES * W1_TW / 3))
W1_LEFT=$(w1_third "$W1_THIRD" left 0)
W1_RIGHT=$(w1_third "$W1_THIRD" right "$((W1_THIRD * 2))")
echo "[e2e]   sheet luma: left third=$W1_LEFT right third=$W1_RIGHT (the fixture brightens with time)"
awk -v l="$W1_LEFT" -v r="$W1_RIGHT" 'BEGIN { exit !(r - l > 40) }' \
  || die "the sheet's tiles are not the clip's frames in order (left=$W1_LEFT right=$W1_RIGHT)"

# (c) It is a media route like every other one: conditional GET and Range, through serveObject.
code=$(get "$BASE/api/clips/$W1A/filmstrip.jpg" -H "If-None-Match: $W1_ETAG")
expect "$code" 304 "If-None-Match on the sheet"
code=$(get "$BASE/api/clips/$W1A/filmstrip.jpg" -H 'Range: bytes=0-9')
expect "$code" 206 "a Range request for the sheet"
code=$(head_ "$BASE/api/clips/$W1A/filmstrip.jpg"); expect "$code" 200 "HEAD the sheet"
# …and it is one of the two media routes that are NEVER signed, in any delivery mode (§20 errata
# 278(a) and 280): the trim scrubber POLLS this route's status with `fetch`, and a `fetch` that
# follows a cross-origin 302 lands in a CORS check it cannot read, so the scrubber would silently
# never get its sheet. The 200 above is the proof; this is the sentence that says so on a run where
# video.mp4, social.mp4 and poster.jpg are all answering 302.
if [ "$E2E_MEDIA" = redirect ]; then
  expect_empty "$(hdr Location)" "filmstrip.jpg was redirected, which would break the scrubber's own poll"
  echo "[e2e]   redirect: filmstrip.jpg is app-served (it is polled by fetch, which cannot read a cross-origin 302)"
fi
# …and the cache-buster the owner's client appends after a trim (§20 erratum 220's client paragraph,
# amended: the address is stable, the bytes are per rendition, and a served sheet is cacheable for an
# hour) is not part of what the route serves: the busted address is the SAME object with the same ETag
# and the same directive, which is what lets one fetch fill one entry for the strip and both tiles.
code=$(head_ "$BASE/api/clips/$W1A/filmstrip.jpg?t=1757300000000")
expect "$code" 200 "HEAD the sheet at the address the client busts to"
expect "$(hdr etag)" "$W1_ETAG" "a query on the sheet's address changed the object it serves"
contains "$(hdr cache-control)" 'max-age=3600' || die "the busted address lost the sheet's cache directive: $(hdr cache-control)"

# (d) ONE object under the clip's own prefix, and the operator can see it.
w1_expect_sheets "$W1A" 1 "the sheet is exactly one object under the clip prefix"
W1_STAT="$E2E_DIR/w1-stat.txt"
dc exec -T app clippet admin ls "$W1A" --stat > "$W1_STAT" 2>&1 || die "admin ls --stat failed: $(cat "$W1_STAT")"
has_file "$W1_STAT" "filmstrip: " "admin ls --stat does not print the filmstrip"
has_file "$W1_STAT" "tiles=$W1_TILES tile=${W1_TW}x$W1_TH interval_ms=$W1_IVL" \
  "admin ls --stat does not print the tile geometry"
has_file "$W1_STAT" "/filmstrip/strip-" "the sheet is not under the §6 key layout"
W1_STATS="$E2E_DIR/w1-stats.txt"
dc exec -T app clippet admin stats > "$W1_STATS" 2>&1 || die "admin stats failed: $(cat "$W1_STATS")"
contains "$(grep '^jobs:' "$W1_STATS")" 'filmstrip=' || die "admin stats does not count filmstrip jobs"

# (e) A trim drops it. The sheet described a rendition that no longer exists, so the columns go, the
#     object goes with them, and the next request makes a new one — the preview GIF's rule exactly.
W1_OLD_ETAG=$W1_ETAG
code=$(http POST "$BASE/api/clips/$W1A/trim" -b "$JARW" -H 'Content-Type: application/json' \
  -d '{"start_ms":2000,"end_ms":8000}')
expect "$code" 202 "POST W1A trim ($(cat "$RESP"))"
wait_trim "$W1A" W1A
code=$(get "$BASE/api/clips/$W1A" -b "$JARW"); expect "$code" 200 "GET W1A after the trim"
expect_empty "$(json filmstrip)" "a trim must drop the filmstrip from the DTO"
expect "$(json state)" ready "W1A stays ready across a trim"
# The old object is gone (the delete follows the commit, which is the retry w1_expect_sheets builds
# in), and the route then makes a fresh sheet of the NEW rendition.
w1_expect_sheets "$W1A" 0 "the superseded sheet is deleted with the superseded rendition"
# …and the gate is right back where it was: with the sheet gone, a visitor is told there is none.
code=$(w1_strip "$W1A" "$W1_JPG"); expect "$code" 404 "an anonymous GET after the trim dropped the sheet"
code=$(w1_own_strip "$W1A" "$W1_JPG"); expect "$code" 202 "the owner's first GET after a trim queues a new sheet"
w1_wait_strip "$W1A" "$W1_JPG" "W1A after the trim"
W1_ETAG2=$(hdr etag); expect_nonempty "$W1_ETAG2" "the new sheet's ETag"
[ "$W1_ETAG2" != "$W1_OLD_ETAG" ] || die "the sheet's ETag did not change across a trim"
code=$(get "$BASE/api/clips/$W1A" -b "$JARW"); expect "$code" 200 "GET W1A after the second sheet"
expect "$(json filmstrip.tiles)" 24 "the new sheet's tile count"
W1_DUR2=$(json duration_ms)
# The interval follows the CUT, which is the whole point of re-deriving the sheet: the clip is now
# about six seconds rather than twelve, so one tile covers about half what it did.
expect "$(json filmstrip.interval_ms)" "$((W1_DUR2 / 24))" "the new sheet's interval follows the new cut"
awk -v a="$W1_IVL" -v b="$((W1_DUR2 / 24))" 'BEGIN { exit !(b < a) }' \
  || die "the re-derived interval ($((W1_DUR2 / 24)) ms) is not shorter than the old one ($W1_IVL ms)"
expect "$(w1_dims "$W1_JPG")" "$((24 * 160))x$W1_TH" "the new sheet's real pixels"
w1_expect_sheets "$W1A" 1 "the re-made sheet is again exactly one object"

# (f) `orphans` is clean: a live sheet is REFERENCED, and a report that called it an extra would
#     invite `--delete` to remove a file the scrubber is serving (the trap social_key fell into).
#     No sleep decides this (run 4: a `sleep 6` against `--min-age 5s` stood here, and the app
#     container's clock can sit seconds behind the bucket's). Age decides neither absence below:
#     `orphans` prints EVERY unreferenced object under a live clip on a line that starts
#     `extra   <id> ` — counted, skipped as younger than --min-age, or skipped for a live job alike
#     (cmd/clippet/admin.go reportExtras; TestAdminOrphansReportsAndDeletesExtraObjectsUnderALiveClip
#     pins the young line's prefix) — and `missing` has no age test at all. What must be POSITIVE first
#     is that the report looked at W1A's prefix: it is scoped to it (`--clip`, so nothing else on the
#     stack is listed) and its own summary says it found every referenced key there.
W1_ORPH="$E2E_DIR/w1-orphans.txt"
dc exec -T app clippet admin orphans --clip "$W1A" --min-age 5s > "$W1_ORPH" 2>&1 || die "admin orphans failed: $(cat "$W1_ORPH")"
has_file "$W1_ORPH" "^scope   .*clips/$W1A/ " "the orphans report was not scoped to W1A's prefix"
has_file "$W1_ORPH" "^orphans: prefixes=0 deleted=0 recent=0 missing=0 foreign=0 extra=0 " \
  "the scoped orphans summary is not clean for W1A"
lacks_file "$W1_ORPH" "extra   $W1A " "a live filmstrip is reported as an unreferenced extra"
lacks_file "$W1_ORPH" "missing $W1A " "W1A has a row pointing at an object the bucket does not hold"

# (g) An unknown or malformed id is a 404 — never a 202, which a poller would read as "one is
#     coming" — and it is the same 404 for the caller who owns clips as for one who owns none, so
#     the gate cannot be used to ask whether an id exists.
code=$(get "$BASE/api/clips/aaaaaaaaaaaa/filmstrip.jpg"); expect "$code" 404 "an unknown clip's sheet"
code=$(get "$BASE/api/clips/nope/filmstrip.jpg"); expect "$code" 404 "a malformed id's sheet"
code=$(get "$BASE/api/clips/aaaaaaaaaaaa/filmstrip.jpg" -b "$JARW"); expect "$code" 404 "an unknown clip's sheet, with a session"

# (h) The owner block says out loud that it needs a client (§20 erratum 229). The template test pins
#     the wording; this proves the SHIPPED template renders it for the owner and for nobody else.
code=$(page "$BASE/$W1A" -b "$JARW"); expect "$code" 200 "GET W1A's share page as its owner"
has_file "$PAGE" '<noscript><p class="hint">Editing this clip needs JavaScript; replacing the video still works.</p></noscript>'   "the owner's Manage block carries no <noscript> line"
code=$(page "$BASE/$W1A"); expect "$code" 200 "GET W1A's share page anonymously"
lacks_file "$PAGE" '<noscript>' "a visitor was told about controls they cannot see"

# ---------------------------------------------------------------------------------------------------
step 71 "the library without a client (§20 errata 222-223): the bulk bar ships hidden, and Mine only is a plain GET"

code=$(page "$BASE/" -b "$JARW"); expect "$code" 200 "GET the library as its owner"
# The bar is SERVER-rendered hidden and nothing reveals it in the HTML: without a client there is
# nothing to tick and nothing that looks clickable (erratum 117), and with one it now waits for the
# first tick (erratum 222) — which is app.js's business and not the server's.
has_file "$PAGE" '<div class="bulk" role="group" aria-label="Bulk actions" data-bulk hidden>' \
  "the bulk bar is not server-rendered hidden"
lacks_file "$PAGE" 'data-bulk-ready' "data-bulk-ready is set by app.js and must never be in the HTML"
# It is a group and not a form, because POST /api/clips/bulk takes JSON (erratum 117).
lacks_file "$PAGE" '<form data-bulk' "the bulk bar must not be a form"
# The search pane IS a plain GET form, and Mine only is a control inside it — which is what makes
# erratum 223's auto-submit progressive enhancement rather than the only way in.
has_file "$PAGE" '<form class="search" method="get" action="/" data-library-search>' \
  "the library search is not a plain GET form"
has_file "$PAGE" 'name="mine" value="1"' "Mine only is not a control of that form"

# …and the URL that form produces works with no client at all.
code=$(page "$BASE/?mine=1" -b "$JARW"); expect "$code" 200 "GET /?mine=1"
has_file "$PAGE" 'data-mine-filter="1"' "?mine=1 did not apply the filter"
has_file "$PAGE" "$W1A" "?mine=1 dropped the caller's own clip"
# The same URL for a caller with no session is the FRONT DOOR (§20 erratum 246; it was the login
# bounce before, and the filter was dropped either way), never somebody else's library.
code=$(page "$BASE/?mine=1"); expect "$code" 200 "GET /?mine=1 with no session"
has_file "$PAGE" '<section class="auth front">' "an anonymous /?mine=1 is the front door"
lacks_file "$PAGE" 'data-clip-list' "an anonymous /?mine=1 rendered a library"
lacks_file "$PAGE" "$W1A" "an anonymous /?mine=1 leaked the owner's clip"

# /favicon.ico (§20 erratum 304) — the front door's last static asset, and the only one nothing on a
# page links to: every template declares its own `data:` icon, so this path exists for the callers
# that were handed a URL and nothing else (bookmark tools, some link previewers, Safari's pinned-tab
# fallback), and it answered the 404 PAGE before. Three facts through the running stack, anonymously:
# it answers, it says what it is, and it IS an ICO — `00 00 01 00` then the directory's entry count,
# which is what a reader looks at before it looks at anything else. `od` and not `grep`, because the
# body is binary and the first six bytes are the whole assertion.
code=$(get "$BASE/favicon.ico"); expect "$code" 200 "GET /favicon.ico with no session"
expect "$(hdr content-type)" "image/vnd.microsoft.icon" "/favicon.ico Content-Type"
expect "$(od -An -tx1 -N6 "$RESP" | tr -d ' \t\n\r')" "000001000300" \
  "/favicon.ico does not open with an ICO header of three entries"

# ---------------------------------------------------------------------------------------------------
step 72 "what a row is doing (§20 erratum 224) and a collection picked by sight (erratum 225)"

# A stage phrase is only observable while a clip is being WORKED ON, so this step needs two clips in
# flight at once. What it builds, and why every part of it is the way it is:
#
#   THIRTY SECONDS of 10-bit 720p60 testsrc2, posted TWICE AT ONCE through `POST /api/clips` on a stack
#   running TRANSCODE_CONCURRENCY=1, with a bitrate cap that keeps each upload near 6 MB.
#
#   - 10-bit, because `yuv420p10le` is a pixel format no browser plays (DEPLOY §27's table): the server
#     owes a Full re-encode at the source's own 1280x720x60 AND the Light copy, in one ffmpeg. That is a
#     real encode per second of source at ANY core count, which is what makes the phrase half of this
#     assertion a fact rather than a race. (The 640x360 ramp above finishes inside a second.)
#   - the bitrate cap, because the WIRE must not be the slow part: the polls are meant to sample the
#     queue, and an upload that takes as long as the encode moves the window they see.
#   - `POST /api/clips` and NOT `/api/upload`, because the capture route deliberately WAITS for the clip
#     to be ready before it answers (§20 erratum 97, up to 60 s), so two sequential uploads through it
#     can never overlap — the first clip was ready before the second was even posted, which is exactly
#     how this step first failed to see a `Queued…` row.
#   - both POSTs fired from background curls, so the second clip is in the queue from the moment the
#     worker claims the first: the window the polls sample is the whole of the first encode, not
#     whatever is left of it after a second upload.
#
# The history, because the numbers in it are why the fixture is not simply longer: the first version was
# fifteen seconds of 8-bit 720p60, sized for the ONE core the shipped compose file gives the container.
# On a four-cpu container (the reference box since 2026-09-10, §20 erratum 245 (g)) that clip was repacked
# and its card copy encoded in under three seconds, and the second clip's 24 MB upload alone took half of
# that: its POST landed 40 ms before the first clip went ready, and no poll ever saw a `Queued…` row.
W1_SLOW="$E2E_DIR/w1-slow.mp4"
ffmpeg -hide_banner -nostdin -y \
  -f lavfi -i "testsrc2=duration=30:size=1280x720:rate=60" \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p10le -b:v 1500k -maxrate 1500k -bufsize 3M \
  "$(W "$W1_SLOW")" > "$E2E_DIR/w1-slow-ffmpeg.txt" 2>&1 \
  || die "could not build the slow fixture: $(tail -3 "$E2E_DIR/w1-slow-ffmpeg.txt")"

# w1_post <label> <title>: the §8.3 metadata-first grammar the upload FORM uses, with a file of this
# drop-in's own (the harness's `upload` helper only reaches testclips/). It writes ITS OWN response and
# status files (w1-resp-<label>.json, w1-code-<label>), because two of them run at once.
w1_post() {
  local mf="$E2E_DIR/w1-meta-$1.json"
  printf '{"title":"%s","ttl_days":1}' "$2" > "$mf"
  { curl -sS -o "$(W "$E2E_DIR/w1-resp-$1.json")" -w '%{http_code}' -b "$JARW" \
      -F "metadata=<$(W "$mf");type=application/json" \
      -F duplicate=allow -F "video=@$(W "$W1_SLOW");type=video/mp4" "$BASE/api/clips" 2>/dev/null || echo 000; } \
    > "$E2E_DIR/w1-code-$1"
}
rm -f "$E2E_DIR"/w1-resp-*.json "$E2E_DIR"/w1-code-*
# FOUR in flight, not two (the twentieth gate, 2026-10-05, on a quiet box: with TRANSCODE_CONCURRENCY=1 the second
# clip's `Queued…` lasted less than one poll on the GPU stack — the slow fixture encodes in about two seconds there —
# and no poll ever saw it). The third and fourth wait behind two encodes, so the row is on the page for several
# seconds whatever the box; the assertion is unchanged: at least one row said `Queued…` while clips were in flight.
w1_post b "E2E stage phrase one" &
w1_post c "E2E stage phrase two" &
w1_post d "E2E stage phrase three" &
w1_post e "E2E stage phrase four" &
wait || true
W1_T0=$SECONDS
for W1_L in b c d e; do
  code=$(tr -d ' \r\n' < "$E2E_DIR/w1-code-$W1_L")
  expect "$code" 201 "upload W1$W1_L ($(cat "$E2E_DIR/w1-resp-$W1_L.json"))"
done
W1B=$(jget "$(W "$E2E_DIR/w1-resp-b.json")" id); expect_nonempty "$W1B" "W1B id"
W1C=$(jget "$(W "$E2E_DIR/w1-resp-c.json")" id); expect_nonempty "$W1C" "W1C id"
W1D=$(jget "$(W "$E2E_DIR/w1-resp-d.json")" id); expect_nonempty "$W1D" "W1D id"
W1E=$(jget "$(W "$E2E_DIR/w1-resp-e.json")" id); expect_nonempty "$W1E" "W1E id"

# Every status line seen while either clip is unfinished must be one the server means to say: the
# label alone, or the label plus one of erratum 224's phrases. A raw stage word ("(encode)") is
# exactly what erratum 224 removed, so seeing one here is a failure.
#
# The list is the THIRD place the vocabulary lives — `internal/web`'s `stageNote` and `app.js`'s
# `stageNotes` are the other two, pinned against each other by
# `web.TestLibraryStageNotesMatchTheClient` — so a new §8.4 stage moves all three together. It gained
# `repacking the upload` and `encoding the card copy` at the lane-4 merge, for erratum 200's `remux`
# and erratum 203's `light`: the two stages a web-playable upload actually spends its wait in, and
# the ones this fixture (a plain H.264 mp4) therefore reports.
W1_PHRASES='reading the upload|inspecting the video|encoding on the server|repacking the upload|encoding the card copy|checking the result|making the thumbnail|uploading to storage|finishing up'
W1_SEEN_QUEUED=0
W1_SEEN_PHRASE=0
W1_N=0
# Twice a second for at most two minutes: the encode above lasts tens of seconds, so the row is
# sampled dozens of times rather than once, and the loop ends the moment both halves are seen.
while [ "$W1_N" -lt 240 ]; do
  code=$(page "$BASE/" -b "$JARW"); expect "$code" 200 "GET the library while clips encode"
  # The CLASSIFYING is python's, and every token it prints is ASCII. Both halves of the label carry
  # non-ASCII — the ellipsis and the middle dot — and Windows python writes stdout in cp1252 unless
  # told otherwise, which turned `Processing…` into a byte the shell's own pattern could never match
  # (§19's python traps, twice over: the encoding AND the CRLF `tr` strips below).
  W1_LINES=$(PYTHONIOENCODING=utf-8 $PY - "$(W "$PAGE")" <<'PYEOF' | tr -d '\r'
import re, sys
QUEUED, PROC, SEP = 'Queued…', 'Processing…', ' · '
html = open(sys.argv[1], encoding='utf-8', errors='replace').read()
for line in re.findall(r'<p class="status" data-status>(.*?)</p>', html):
    if line == QUEUED:
        print('QUEUED')
    elif line == PROC:
        print('PLAIN')
    elif line.startswith(PROC + SEP):
        print('PHRASE ' + line[len(PROC + SEP):])
    else:
        print('OTHER ' + line.encode('unicode_escape').decode('ascii'))
PYEOF
)
  if [ -n "$W1_LINES" ]; then
    while IFS= read -r W1_LINE; do
      [ -n "$W1_LINE" ] || continue
      case $W1_LINE in
        QUEUED) W1_SEEN_QUEUED=1;;
        PLAIN) ;;   # a claimed job that has not reported a stage yet
        'PHRASE '*)
          W1_NOTE=${W1_LINE#PHRASE }
          printf '%s' "$W1_NOTE" | grep -Eq "^($W1_PHRASES)$" \
            || die "a library row's phrase is '$W1_NOTE', which is not one of erratum 224's"
          W1_SEEN_PHRASE=1;;
        *) die "a library row's status line is $W1_LINE, which the server does not mean to say";;
      esac
    done <<EOF
$W1_LINES
EOF
  fi
  if [ "$W1_SEEN_QUEUED" = 1 ] && [ "$W1_SEEN_PHRASE" = 1 ]; then break; fi
  # The last clip ready → nothing left to observe; the assertions below report what was missed.
  code=$(get "$BASE/api/clips/$W1E"); expect "$code" 200 "GET W1E while polling the library"
  if [ "$(json state)" = ready ]; then break; fi
  W1_N=$((W1_N + 1)); sleep 0.5
done
expect "$W1_SEEN_QUEUED" 1 "no library row ever said 'Queued…' while four clips were in flight"
expect "$W1_SEEN_PHRASE" 1 "no library row ever carried a stage phrase (§20 erratum 224)"
echo "[e2e]   the library's rows said 'Queued…' and a stage phrase while two clips were in flight ($W1_N polls, $((SECONDS - W1_T0))s)"
wait_ready "$W1B" W1B
wait_ready "$W1C" W1C
wait_ready "$W1D" W1D
wait_ready "$W1E" W1E

# The collection picker shows each clip's card image beside its title (erratum 225).
code=$(http POST "$BASE/api/collections" -b "$JARW" -H 'Content-Type: application/json' \
  -d '{"title":"E2E W1 picker"}')
expect "$code" 201 "create the picker collection ($(cat "$RESP"))"
W1_COL=$(json id); expect_nonempty "$W1_COL" "the collection id"
code=$(http PUT "$BASE/api/collections/$W1_COL/clips" -b "$JARW" -H 'Content-Type: application/json' \
  -d "{\"clip_ids\":[\"$W1B\"]}")
expect "$code" 200 "put one clip in the collection ($(cat "$RESP"))"
code=$(page "$BASE/c/$W1_COL" -b "$JARW"); expect "$code" 200 "GET the collection page as its owner"
has_file "$PAGE" '<div class="picker" data-members-picker>' "the picker is missing"
has_file "$PAGE" "<img class=\"choice-thumb\" src=\"/api/clips/$W1B/poster.jpg\"" \
  "the picked clip's entry carries no card image"
has_file "$PAGE" "<img class=\"choice-thumb\" src=\"/api/clips/$W1C/poster.jpg\"" \
  "a clip that is NOT a member carries no card image"
has_file "$PAGE" '<span class="choice-title">' "the picker's titles are not their own element"
# The thumbnail is DECORATIVE and bounded: empty alt (so the label still reads as the title and the
# control says the same thing to a screen reader), lazy, and sized, so a hundred-clip picker neither
# fetches everything at once nor reflows as it fills.
has_file "$PAGE" "src=\"/api/clips/$W1B/poster.jpg\" alt=\"\" loading=\"lazy\" width=\"80\" height=\"45\"" \
  "the picker's thumbnail is not the decorative, lazy, sized image erratum 225 describes"
# A visitor sees the list and no picker at all.
code=$(page "$BASE/c/$W1_COL"); expect "$code" 200 "GET the collection page anonymously"
lacks_file "$PAGE" 'data-members-picker' "a visitor was offered the membership picker"
lacks_file "$PAGE" 'choice-thumb' "a visitor was rendered the picker's thumbnails"

# ---- cleanup: everything this drop-in made, in the order it made it ---------------------------------
code=$(http DELETE "$BASE/api/collections/$W1_COL" -b "$JARW")
expect "$code" 204 "delete the picker collection"
for W1_ID in "$W1A" "$W1B" "$W1C" "$W1D" "$W1E"; do
  code=$(http DELETE "$BASE/api/clips/$W1_ID" -b "$JARW")
  case $code in 204|202) ;; *) die "delete $W1_ID → $code";; esac
done
wait_gone "$W1A" W1A
wait_gone "$W1B" W1B
wait_gone "$W1C" W1C
wait_gone "$W1D" W1D
wait_gone "$W1E" W1E
summary_row W1A "${W1A:-}" "the luma-ramp clip: its filmstrip refused to every caller but its owner, then made on demand, measured against the JPEG's own pixels, read anonymously, dropped and re-made by a trim; deleted"
summary_row W1B "${W1B:-}" "stage phrase + the collection picker's thumbnail (a member); deleted"
summary_row W1C "${W1C:-}" "stage phrase (queued behind W1B) + the picker's thumbnail (a non-member); deleted"
summary_row W1D "${W1D:-}" "stage phrase (queued behind W1B and W1C); deleted"
summary_row W1E "${W1E:-}" "stage phrase (queued behind three); deleted"
W1A= ; W1B= ; W1C= ; W1D= ; W1E=
