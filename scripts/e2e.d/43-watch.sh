# 43-watch.sh — the watch page: a chosen card image, the trim scrubber, the clock, the player controls.
#
# e2e-schedule: parallel
# e2e-schedule-apart: 70-web
# e2e-namespace: account=- prefix=WATCH_ buckets=view
# e2e-covers: internal/httpapi/poster.go internal/worker/poster.go internal/db/poster.go
# e2e-covers: internal/transcode/poster.go internal/transcode/posterat.go internal/httpapi/gif.go
# e2e-covers: internal/worker/gif.go internal/transcode/gif.go internal/db/gif.go internal/web/format.go
# e2e-covers: internal/web/pages.go web/templates/clip.html web/static/app.js
#   Its clip P is its own and is deleted; it reads C's page only. It is APART from 70-web.sh
#   deliberately: its preview-GIF poll (one request every two seconds) and that file's filmstrip poll
#   (one a second) both spend the `view` bucket, which is 60 a minute per address and cannot be raised —
#   run together they would 429 each other in a step that is not about rate limiting. (Until lane
#   `speed2` the two shared LANE 2, which made the same promise by queueing them.)
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Steps 43-46. Everything it creates it destroys; nothing it does outlives the run.

# ---------------------------------------------------------------------------------------------------
step 43 "the card image the owner CHOSE: the frame is that frame, and a cut keeps the choice"
# The fixture is the assertion. Fifteen seconds in three flat luma bands, five seconds each — dark,
# bright, mid — so the PICTURE in the served JPEG says which second it came from and nothing else
# could. Keyframes every second (-g 30 -sc_threshold 0) so the trim below cuts where it is asked to
# rather than snapping back to a distant one, which is the difference between a window that contains
# the chosen moment and one that does not.
WATCH_SRC="$E2E_DIR/bands.mp4"
ffmpeg -hide_banner -nostdin -y \
  -f lavfi -i "color=c=0x202020:s=640x360:r=30:d=5" \
  -f lavfi -i "color=c=0xc8c8c8:s=640x360:r=30:d=5" \
  -f lavfi -i "color=c=0x707070:s=640x360:r=30:d=5" \
  -filter_complex "[0:v][1:v][2:v]concat=n=3:v=1:a=0[v]" -map "[v]" \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p -g 30 -keyint_min 30 -sc_threshold 0 \
  "$(W "$WATCH_SRC")" > "$E2E_DIR/bands-ffmpeg.txt" 2>&1 \
  || die "could not build the luma-band fixture: $(tail -3 "$E2E_DIR/bands-ffmpeg.txt")"

# watch_yavg_at <file> <seconds> <label> → the mean luma of ONE frame of a video, via the same
# signalstats the poster gate uses. It is how a band is named without hard-coding a number that
# depends on an ffmpeg build's colour conversion.
watch_yavg_at() {
  local out="$E2E_DIR/frame-$3.jpg"
  ffmpeg -hide_banner -nostdin -y -ss "$2" -i "$(W "$1")" -frames:v 1 "$(W "$out")" \
    > "$E2E_DIR/frame-$3.txt" 2>&1 || die "could not extract $1 at ${2}s: $(tail -3 "$E2E_DIR/frame-$3.txt")"
  yavg "$out" "$3"
}
# watch_near <got> <want> <tolerance> <what> — |got - want| must be within tolerance.
watch_near() {
  awk -v g="$1" -v w="$2" -v t="$3" 'BEGIN { if (g == "" || w == "") exit 1; d = g - w; if (d < 0) d = -d; exit !(d <= t) }' \
    || die "$4: YAVG=$1, want within $3 of $2"
}
# watch_far <got> <other> <gap> <what> — |got - other| must EXCEED gap (the premise of every
# assertion above: the bands really are different pictures).
watch_far() {
  awk -v g="$1" -v o="$2" -v m="$3" 'BEGIN { if (g == "" || o == "") exit 1; d = g - o; if (d < 0) d = -d; exit !(d > m) }' \
    || die "$4: YAVG=$1 is within $3 of $2"
}

WATCH_DARK=$(watch_yavg_at "$WATCH_SRC" 2.5 dark)
WATCH_BRIGHT=$(watch_yavg_at "$WATCH_SRC" 7.5 bright)
WATCH_MID=$(watch_yavg_at "$WATCH_SRC" 12.5 mid)
echo "[e2e]   fixture bands: dark=$WATCH_DARK bright=$WATCH_BRIGHT mid=$WATCH_MID"
watch_far "$WATCH_BRIGHT" "$WATCH_DARK" 60 "the fixture's bright and dark bands are not distinguishable"
watch_far "$WATCH_BRIGHT" "$WATCH_MID" 40 "the fixture's bright and mid bands are not distinguishable"

code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "title=E2E watch bands" -F "ttl_days=1" \
  -F duplicate=allow -F "file=@$(W "$WATCH_SRC");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "upload the luma-band clip ($(cat "$RESP"))"
P=$(json id); expect_nonempty "$P" "P id"
wait_ready "$P" P

# The automatic card image is what this clip has now; the poster's ETag is what must change, and the
# RENDITION's is what must not — a card image is a picture of the clip, never a new clip.
# The media helpers (§20 erratum 280, rule (a)): every assertion in this step is about the OBJECT —
# which poster is served, whose ETag it has, what the pixels say — and an object is the same object
# whichever way `MEDIA_DELIVERY` hands it over.
code=$(media_head "$BASE/api/clips/$P/poster.jpg"); expect_media "$code" 200 "HEAD P poster.jpg"
WATCH_ETAG0=$(hdr etag); expect_nonempty "$WATCH_ETAG0" "the automatic poster's etag"
code=$(media_head "$BASE/api/clips/$P/video.mp4"); expect_media "$code" 200 "HEAD P video.mp4"
WATCH_VETAG=$(hdr etag); expect_nonempty "$WATCH_VETAG" "the rendition's etag"

# watch_poll_poster <id> <what> — poll GET …/poster until the job is terminal; die on anything but done.
watch_poll_poster() {
  local n=0 state
  while [ "$n" -lt 120 ]; do
    code=$(get "$BASE/api/clips/$1/poster" -b "$JARW"); expect "$code" 200 "GET …/poster ($2)"
    state=$(json state)
    case $state in
      done)      echo "[e2e]   $2: poster job done after ${n}s"; return 0;;
      failed)    die "$2: the poster job failed: $(json error)";;
      cancelled) die "$2: the poster job was cancelled";;
    esac
    n=$((n + 1)); sleep 1
  done
  die "$2: the poster job never finished (state=$state)"
}

# Nobody has chosen a frame yet, so there is no job to report — and a poller must never read that
# 404 as "finished" (§8.4's rule, one route along).
code=$(get "$BASE/api/clips/$P/poster" -b "$JARW"); expect "$code" 404 "GET …/poster before any choice"
expect "$(json error)" not_found "no-choice error code"

# Choose the BRIGHT band. `seconds` is on the ORIGINAL's timeline; this clip is untrimmed, so it is
# also the player's clock.
code=$(http POST "$BASE/api/clips/$P/poster" -b "$JARW" -H 'Content-Type: application/json' -d '{"seconds":7.5}')
expect "$code" 202 "POST …/poster 7.5 ($(cat "$RESP"))"
expect "$(json state)" queued "the 202 says a job exists"
expect "$(json seconds)" 7.5 "the 202 echoes the moment it queued"
expect_nonempty "$(json job_id)" "the 202 carries a job id"
watch_poll_poster "$P" "the chosen frame"
expect "$(json applied_seconds)" 7.5 "the CLIP carries the choice once the job is done"

# The proof: the served card image IS the frame at 7.5 s and is not the frame at 2.5 s.
code=$(media_download "$BASE/api/clips/$P/poster.jpg" "$E2E_DIR/P-chosen.jpg")
expect "$code" 200 "download P's chosen poster"
WATCH_Y=$(yavg "$E2E_DIR/P-chosen.jpg" P-chosen); echo "[e2e]   chosen poster YAVG=$WATCH_Y (bright band = $WATCH_BRIGHT)"
watch_near "$WATCH_Y" "$WATCH_BRIGHT" 20 "the chosen card image is not the frame at 7.5 s"
watch_far "$WATCH_Y" "$WATCH_DARK" 60 "the chosen card image is still the dark band"
code=$(media_head "$BASE/api/clips/$P/poster.jpg"); expect_media "$code" 200 "HEAD P poster.jpg after the choice"
WATCH_ETAG1=$(hdr etag); expect_nonempty "$WATCH_ETAG1" "the chosen poster's etag"
[ "$WATCH_ETAG1" != "$WATCH_ETAG0" ] || die "the poster ETag did not change: a cache would keep serving the old card image"
# The clip is untouched by all of it: still ready, still on the same rendition, still playable.
code=$(get "$BASE/api/clips/$P"); expect "$code" 200 "GET P after the choice"
expect "$(json state)" ready "a poster job never marks its clip"
code=$(media_head "$BASE/api/clips/$P/video.mp4"); expect_media "$code" 200 "HEAD P video.mp4 after the choice"
expect "$(hdr etag)" "$WATCH_VETAG" "a poster job must not touch the rendition"

# …and the looping preview opens on the SAME frame (the regenerate half of the choice).
code=$(http POST "$BASE/api/clips/$P/gif" -b "$JARW" -H 'Content-Type: application/json')
expect "$code" 202 "POST …/gif after choosing a frame ($(cat "$RESP"))"
WATCH_N=0
while [ "$WATCH_N" -lt 120 ]; do
  code=$(curl -sS -o "$(W "$E2E_DIR/P.gif")" -w '%{http_code}' "$BASE/api/clips/$P/preview.gif")
  [ "$code" = 200 ] && break
  case $code in 202) ;; *) die "GET preview.gif -> $code";; esac
  WATCH_N=$((WATCH_N + 1)); sleep 2
done
expect "$code" 200 "the preview GIF was made"
WATCH_Y=$(watch_yavg_at "$E2E_DIR/P.gif" 0 gif0)
echo "[e2e]   preview first frame YAVG=$WATCH_Y (bright band = $WATCH_BRIGHT)"
watch_near "$WATCH_Y" "$WATCH_BRIGHT" 30 "the preview does not open on the chosen frame"
watch_far "$WATCH_Y" "$WATCH_DARK" 50 "the preview still opens on the clip's first (dark) seconds"

# A cut that CONTAINS the chosen moment keeps it: 6 s-11 s holds 7.5 s, so the choice survives and
# the frame is simply re-taken from the new rendition at 1.5 s of it.
code=$(http POST "$BASE/api/clips/$P/trim" -b "$JARW" -H 'Content-Type: application/json' -d '{"start_ms":6000,"end_ms":11000}')
expect "$code" 202 "POST …/trim 6-11 s ($(cat "$RESP"))"
WATCH_N=0
while [ "$WATCH_N" -lt 180 ]; do
  code=$(get "$BASE/api/clips/$P/trim" -b "$JARW"); expect "$code" 200 "GET …/trim"
  case "$(json state)" in
    done) echo "[e2e]   trim done after ${WATCH_N}s"; break;;
    failed) die "the trim failed: $(json error)";;
    cancelled) die "the trim was cancelled";;
  esac
  WATCH_N=$((WATCH_N + 1)); sleep 1
done
expect "$(json state)" done "the trim finished"
WATCH_CUT=$(json applied_start_ms); expect_nonempty "$WATCH_CUT" "the applied cut start"
code=$(get "$BASE/api/clips/$P/poster" -b "$JARW"); expect "$code" 200 "GET …/poster after the trim"
expect "$(json applied_seconds)" 7.5 "the cut KEPT the chosen moment (it is on the original's timeline)"
code=$(media_download "$BASE/api/clips/$P/poster.jpg" "$E2E_DIR/P-trimmed.jpg")
expect "$code" 200 "download P's poster after the trim"
WATCH_Y=$(yavg "$E2E_DIR/P-trimmed.jpg" P-trimmed)
echo "[e2e]   poster after the trim: YAVG=$WATCH_Y (applied cut starts at ${WATCH_CUT}ms)"
watch_near "$WATCH_Y" "$WATCH_BRIGHT" 20 "the trim did not re-take the chosen frame"
echo "[e2e]   the chosen frame is the frame, the preview opens on it, and the cut kept it"

# ---------------------------------------------------------------------------------------------------
step 44 "the card-image routes: the owner-route posture, and bounds that follow the cut"
# No frame endpoint was added — the scrubber previews with the PLAYER (§20 erratum 132) — so this
# step is the poster pair's own matrix instead.
# P is now cut to 6 s-11 s, so the reachable part of the ORIGINAL's timeline starts at the cut.
code=$(http_refused POST "$BASE/api/clips/$P/poster" -H 'Content-Type: application/json' -d '{"seconds":7}')
expect "$code" 401 "an anonymous POST …/poster"
code=$(get "$BASE/api/clips/$P/poster"); expect "$code" 401 "an anonymous GET …/poster"
# A second account: the run's own _b user, logged back in rather than a fourth row being registered.
JARP="$E2E_DIR/cookies-watch.txt"; JARPW=$(W "$JARP"); rm -f "$JARP"
code=$(http POST "$BASE/api/auth/login" -c "$JARPW" -H 'Content-Type: application/json' \
  -d "{\"username\":\"${E2E_USER}_b\",\"password\":\"$E2E_PASS\"}")
expect "$code" 200 "log the second user in for the card-image matrix"
code=$(http_refused POST "$BASE/api/clips/$P/poster" -b "$JARPW" -H 'Content-Type: application/json' -d '{"seconds":7}')
expect "$code" 403 "a stranger's POST …/poster"
expect "$(json error)" forbidden "stranger POST error code"
code=$(get "$BASE/api/clips/$P/poster" -b "$JARPW"); expect "$code" 403 "a stranger's GET …/poster"
# A capture token is an upload credential and unlocks nothing here (§20 erratum 19).
code=$(http POST "$BASE/api/auth/tokens" -b "$JARW" -H 'Content-Type: application/json' -d '{"name":"e2e poster"}')
expect "$code" 201 "mint a token for the card-image matrix"
WATCH_TOK=$(json token); WATCH_TID=$(json id)
code=$(http_refused POST "$BASE/api/clips/$P/poster" -H "Authorization: Bearer $WATCH_TOK" -H 'Content-Type: application/json' -d '{"seconds":7}')
expect "$code" 403 "a capture token's POST …/poster"
expect "$(json error)" token_not_allowed "token POST error code"
code=$(get "$BASE/api/clips/$P/poster" -H "Authorization: Bearer $WATCH_TOK"); expect "$code" 403 "a capture token's GET …/poster"
code=$(http DELETE "$BASE/api/auth/tokens/$WATCH_TID" -b "$JARW"); expect "$code" 204 "revoke the card-image token"
# CSRF and the media type, both decided before the handler runs.
code=$(http_refused POST "$BASE/api/clips/$P/poster" -b "$JARW" -H 'Content-Type: application/json' -H 'Origin: https://evil.example' -d '{"seconds":7}')
expect "$code" 403 "a cross-origin POST …/poster"
code=$(http_refused POST "$BASE/api/clips/$P/poster" -b "$JARW" -H 'Content-Type: application/x-www-form-urlencoded' -d 'seconds=7')
expect "$code" 415 "a form-encoded POST …/poster"
# The bounds, on the CUT: 6 s-11 s of the original, less the tenth of a second at the end where a
# seek writes no frame at all.
code=$(http_refused POST "$BASE/api/clips/$P/poster" -b "$JARW" -H 'Content-Type: application/json' -d '{"seconds":1}')
expect "$code" 400 "a moment before the cut"
expect "$(json error)" invalid_poster "before-the-cut error code"
code=$(http_refused POST "$BASE/api/clips/$P/poster" -b "$JARW" -H 'Content-Type: application/json' -d '{"seconds":14}')
expect "$code" 400 "a moment past the cut"
code=$(http_refused POST "$BASE/api/clips/$P/poster" -b "$JARW" -H 'Content-Type: application/json' -d '{}')
expect "$code" 400 "no seconds at all"
expect "$(json error)" invalid_poster "missing-seconds error code"
# An unknown clip is a 404 before any work, for the owner as for anybody.
code=$(http_refused POST "$BASE/api/clips/AaBbCcDdEeFf/poster" -b "$JARW" -H 'Content-Type: application/json' -d '{"seconds":1}')
expect "$code" 404 "POST …/poster on a clip that does not exist"
code=$(get "$BASE/api/clips/$C/poster" -b "$JARW"); expect "$code" 404 "GET …/poster on a clip nobody has chosen a frame for"
# …and the PUBLIC card image beside it is untouched by every one of those rules.
code=$(media_head "$BASE/api/clips/$P/poster.jpg"); expect_media "$code" 200 "an anonymous HEAD poster.jpg"
code=$(http POST "$BASE/api/auth/logout" -b "$JARPW" || echo 000); echo "[e2e]   logout ${E2E_USER}_b -> $code"
rm -f "$JARP"
echo "[e2e]   session-only, CSRF-guarded, JSON-only, bounded by the cut; poster.jpg still public"

# ---------------------------------------------------------------------------------------------------
step 45 "the two dates are machine-readable instants, and the UTC sentence is still what renders"
# P was uploaded with ttl_days=1, so it has a countdown; C is kept forever and must have none.
code=$(page "$BASE/$P" -b "$JARW"); expect "$code" 200 "P's share page (owner)"
has_file "$PAGE" '<dd data-expiry>expires <time datetime="' "the Lifetime line wraps its instant in <time>"
has_file "$PAGE" 'data-local data-countdown>' "the expiry carries the countdown hook"
has_file "$PAGE" 'UTC</time>' "the absolute UTC text is still what a reader with no JavaScript sees"
has_file "$PAGE" '<dt>Uploaded</dt><dd><time datetime="' "the Uploaded line wraps its instant too"
lacks_file "$PAGE" 'onclick=' "the share page carries no inline handler"
# A clip kept forever names no instant, so it carries no countdown to run.
code=$(page "$BASE/$C" -b "$JARW"); expect "$code" 200 "C's share page (kept forever)"
has_file "$PAGE" '<dd data-expiry>kept forever</dd>' "a clip kept forever says so, with no <time>"
lacks_file "$PAGE" 'data-countdown' "a clip kept forever has no countdown"
# The player's own controls belong to EVERY viewer of a ready clip and are inert without a client.
code=$(page "$BASE/$P"); expect "$code" 200 "P's share page (anonymous)"
has_file "$PAGE" '<div class="player-extras" data-player-extras hidden>' "the player controls are inert without JavaScript"
has_file "$PAGE" 'data-loop aria-pressed="false"' "the loop toggle, off until a client remembers otherwise"
# …and never on the embed: its autoplay/muted/loop are the embedder's choice (§20 erratum 77).
code=$(page "$BASE/embed/$P"); expect "$code" 200 "P's embed page"
lacks_file "$PAGE" 'data-player-extras' "the embed carries no viewer preference controls"
lacks_file "$PAGE" 'data-loop' "the embed carries no loop toggle"
echo "[e2e]   <time datetime> on both dates, the UTC fallback intact, player controls inert and share-page only"

# ---------------------------------------------------------------------------------------------------
step 46 "the owner page carries the scrubber and the card-image control; a stranger's page carries neither"
code=$(page "$BASE/$P" -b "$JARW"); expect "$code" 200 "P's share page (owner)"
has_file "$PAGE" '<div class="scrubber" data-scrubber hidden>' "the scrubber is server-rendered and inert"
has_file "$PAGE" 'data-scrub-handle="start" role="slider"' "the start handle is a real, focusable slider"
has_file "$PAGE" 'data-scrub-handle="end" role="slider"' "the end handle is a real, focusable slider"
has_file "$PAGE" 'data-scrub-set="start" hidden' "the 'From = now' button appears only with a client"
has_file "$PAGE" 'data-trim-start>' "the number boxes stay: they are the API's truth and the no-JS fallback"
has_file "$PAGE" 'data-poster data-poster-url=' "the card-image control"
has_file "$PAGE" 'data-poster-go>Use this frame' "the 'Use this frame' button"
has_file "$PAGE" 'data-cut-start-ms=' "the control carries the cut's start, so the client can speak the API's timeline"
has_file "$PAGE" 'data-poster-seconds=' "…and says which moment is currently chosen"
# The replace control's Cancel ships HIDDEN: app.js reveals it for the UPLOAD and hides it again the
# moment the server has the file, so a browser without JavaScript is never offered a control that
# does nothing and an owner is never offered one that cannot do what it says (nit N091).
has_file "$PAGE" 'data-replace-cancel hidden>Cancel' "the replace control's Cancel, server-rendered and hidden"
# Every owner-block field carries a name (Chrome's audit wants an id or a name on each; app.js POSTs
# JSON, so the name is inert and the data-* attribute stays the selector) and the tag box's bound is
# the server's: 329 = db.MaxTagsPerClip x db.MaxTagRunes + 9 commas (nits N045, N048).
has_file "$PAGE" 'name="tags" maxlength="329"' "the tag editor's maxlength is the widest legal tag string"
has_file "$PAGE" 'name="trim_start"' "the trim start box has a name"
has_file "$PAGE" 'name="trim_end"' "the trim end box has a name"
lacks_file "$PAGE" 'maxlength="400"' "the old 400-character tag bound is gone"
lacks_file "$PAGE" 'onclick=' "the owner page carries no inline handler"
lacks_file "$PAGE" 'style="' "the owner page carries no inline style"

code=$(page "$BASE/$P"); expect "$code" 200 "P's share page (anonymous)"
for owned in data-scrubber data-scrub- data-poster data-trim data-owner-controls data-touch-url; do
  lacks_file "$PAGE" "$owned" "an anonymous watch page must not carry $owned"
done
echo "[e2e]   every owner control is the owner's; the anonymous page gained nothing"

# Everything this file made is gone before the log gate: P is ours and nothing else was created.
code=$(http DELETE "$BASE/api/clips/$P" -b "$JARW" || echo 000); echo "[e2e]   DELETE P $P -> $code"

# This bundle's own row in the closing summary table (scripts/e2e.d/README.md).
summary_row P "${P:-}" "luma bands: the owner's CHOSEN card image is that frame, survives a cut, rendition untouched; deleted"
