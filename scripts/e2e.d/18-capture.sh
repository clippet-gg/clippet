# 18-capture.sh — the capture route as a capture tool drives it: a token and its ShareX file, the
# ShareX-shaped POST /api/upload with its negatives, and the reply held until the CARD works. Steps 18 and
# 29, which were the serial core of scripts/e2e.sh until run 9's lane `pool` (scripts/e2e.d/README.md "The
# core in the pool").
#
# e2e-schedule: parallel
# e2e-schedule-apart: 47-upload,62-tiers,103-audio
# e2e-namespace: account=- prefix=CP_ buckets=-
# e2e-covers: internal/httpapi/upload_capture.go internal/httpapi/sxcu.go internal/httpapi/token_handlers.go
# e2e-covers: internal/httpapi/ingest.go internal/httpapi/upload.go internal/auth/auth.go internal/web/meta.go
#   Every clip and every token it makes is its own and is deleted or revoked again in the step that made
#   it; it reads none of the harness's clips. Nothing it asserts is a count. Step 29's card fetch is one
#   page, not a drain.
#
# Variables are prefixed CP_.
#
# Step 29 was read beside 67-capture's step 67 (run 9, lane `pool`, the duplicates read) and KEPT: it is
# the one DETERMINISTIC proof that a small capture answers `state: ready` + `card_ready: true` and that the
# pasted link is a PLAYER card on the first crawler fetch. Step 67 tolerates `card_ready=false` for its
# 1080p60 subject under load and asserts no state for its small one.
#
# Apart from 103-audio (run 9 merge seam, found on `toil`): step 29's 201 is held for the worker to finish
# within CAPTURE_READY_WAIT (30 s), and 103-audio submits its fourteen uploads AT ONCE, which fills the
# worker's queue for longer than that. Side by side, `E2E_ONLY=18-capture,103-audio` failed step 29 with
# `state: queued` every time; kept apart, both pass.
#
# Apart from 47-upload and 62-tiers too (run 9's fix pass): they are the other two bundles that keep the
# worker's queue full for minutes (47's uploads, 62's x4/x8 tier encodes). Side by side from a cold start
# step 29 still answered ready in 4 s, but in a full run this short bundle starts mid-way through them, and
# a `got 'queued'` is an `expect` mismatch, which the pool never retries. Both are the longest bundles and
# start first, so 18-capture (≈15 s) simply runs after them, beside the shorter bundles.

step 18 "capture tools (#1): mint a token + its clippet.sxcu, then a ShareX-shaped POST /api/upload"
code=$(http POST "$BASE/api/auth/tokens" -b "$JARW" -H 'Content-Type: application/json' -d '{"name":"e2e sharex"}')
expect "$code" 201 "mint capture token"
CP_SXTOK=$(json token); expect_nonempty "$CP_SXTOK" "raw capture token"
CP_SXTID=$(json id); expect_nonempty "$CP_SXTID" "capture token id"
# The sxcu object IS the ShareX file: these are the fields that make it work.
expect "$(json sxcu.Name)" Clippet "sxcu Name"
expect "$(json sxcu.RequestMethod)" POST "sxcu RequestMethod"
expect "$(json sxcu.RequestURL)" "$BASE/api/upload" "sxcu RequestURL (must be on the public origin)"
expect "$(json sxcu.Body)" MultipartFormData "sxcu Body"
expect "$(json sxcu.FileFormName)" file "sxcu FileFormName"
# No Arguments: ShareX expands {filename} WITH the extension, so a title argument would make every
# card read "….mp4"; sending only the file part lets the server name the clip from it (§20 erratum 60).
expect_empty "$(json sxcu.Arguments)" "sxcu Arguments (must be absent or empty)"
lacks_file "$RESP" '{filename}' "sxcu title argument"
expect "$(json sxcu.URL)" '{json:url}' "sxcu URL (what ShareX pastes)"
expect "$(json sxcu.ErrorMessage)" '{json:message}' "sxcu ErrorMessage"
[ "$(json sxcu.Headers.Authorization)" = "Bearer $CP_SXTOK" ] \
  || die "sxcu Authorization is not 'Bearer ' + the raw token: got $(redact_ "$(json sxcu.Headers.Authorization)")"
# FileUploader ONLY: ShareX's import activates the destinations a file declares, and an image
# destination would send every screenshot here to be rejected 415 not_a_video (§20 erratum 59).
expect "$(json sxcu.DestinationType)" FileUploader "sxcu DestinationType"
echo "[e2e]   sxcu: POST $(json sxcu.RequestURL) file=$(json sxcu.FileFormName) → $(json sxcu.URL)"
# The request ShareX actually sends: text fields first, then the file part named `file`. No cookie jar.
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -H "Authorization: Bearer $CP_SXTOK" \
  -F "title=E2E sharex" -F "ttl_days=7" -F "unlisted=false" -F "who=cares" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "ShareX-shaped POST /api/upload ($(cat "$RESP"))"
CP_G=$(json id); expect_nonempty "$CP_G" "G id"
expect "$(json url)" "$BASE/$CP_G" "upload url = the share page ShareX pastes"
expect "$(json embed_url)" "$BASE/embed/$CP_G" "upload embed_url"
# Erratum 97: a capture upload now WAITS for ready (CAPTURE_READY_WAIT, default 8 s), so the 201 normally
# says ready; queued is still legal when the wait expires on a slow box. Step 29 pins both halves.
case $(json state) in queued|ready) ;; *) die "upload state '$(json state)', want queued|ready" ;; esac
expect_nonempty "$(json expires_at)" "upload expires_at (ttl_days=7)"
wait_ready "$CP_G" G
code=$(get "$BASE/api/clips/$CP_G"); expect "$code" 200 "GET G"
CP_GPW=$(json poster.width); CP_GPH=$(json poster.height); CP_GW=$(json width); CP_GH=$(json height)
code=$(page "$BASE/$CP_G" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot share page for the ShareX clip"
card_check player "$PAGE" "$BASE" "$CP_G" "E2E sharex" "$CP_GPW" "$CP_GPH" "$CP_GW" "$CP_GH" "$SECURE"
# Negatives. (1) no credential → 401 before any body is read; a 1-byte part keeps the probe out of it.
printf 'x' > "$CTX_DIR/tiny.bin"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -F "title=nope" \
  -F duplicate=allow -F "file=@$(W "$CTX_DIR/tiny.bin");type=video/mp4" "$BASE/api/upload")
expect "$code" 401 "unauthenticated POST /api/upload"; expect "$(json error)" unauthorized "unauthenticated error code"
# (2) the file part first: the fields after it are never read, so the title falls back to the filename.
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -H "Authorization: Bearer $CP_SXTOK" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/noaudio-720p30.mp4");type=video/mp4" -F "title=ignored after the file part" \
  "$BASE/api/upload")
expect "$code" 201 "file-part-first upload ($(cat "$RESP"))"
CP_H=$(json id); expect_nonempty "$CP_H" "H id"
code=$(get "$BASE/api/clips/$CP_H"); expect "$code" 200 "GET H"
expect "$(json title)" noaudio-720p30 "title falls back to the filename when title arrives after the file"
# (3) the capture token is an upload credential, not a session (§20 erratum 19).
code=$(get "$BASE/api/clips" -H "Authorization: Bearer $CP_SXTOK"); expect "$code" 403 "token on a session-only route"
expect "$(json error)" token_not_allowed "session-only route error code"
# (4) A CHUNKED body (§20 erratum 90; was step 25 until run 9 — erratum 331(i)). net/http reports
# ContentLength = -1 for `Transfer-Encoding: chunked`, and both upload pre-flight gates once sat inside
# `if r.ContentLength > 0`. The refusal and an in-process 201 are pinned by httpapi
# TestChunkedUploadIsGatedLikeAnyOther; what only the wire proves is the working path end to end: a real
# streaming client's undeclared-length body spools whole and becomes a READY clip, not a 201 over an
# empty or truncated spool. curl drops Content-Length and frames the body when handed this header itself.
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -H "Authorization: Bearer $CP_SXTOK" \
  -H 'Transfer-Encoding: chunked' -F "title=E2E chunked" -F "ttl_days=1" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/small-640x360.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "chunked POST /api/upload ($(cat "$RESP"))"
# The reply is OURS, not a proxy's: an edge that refused the framing would answer text/html.
contains "$(hdr content-type)" 'application/json' || die "chunked upload answered '$(hdr content-type)', not JSON: $(cat "$RESP")"
CP_CH=$(json id); expect_nonempty "$CP_CH" "chunked upload id"
case $(json state) in queued|ready) ;; *) die "chunked upload state '$(json state)', want queued|ready" ;; esac
wait_ready "$CP_CH" CH
code=$(get "$BASE/api/clips/$CP_CH"); expect "$code" 200 "GET the chunked clip"
expect "$(json state)" ready "chunked clip state"
expect_empty "$(json failure_reason)" "chunked clip failure_reason"
# The fixture is 6 s (scripts/testclips.sh): a spool cut short would not come back the whole length.
CP_CHD=$(json duration_ms)
case $CP_CHD in ''|*[!0-9]*) die "chunked clip duration_ms '$CP_CHD' is not a number" ;; esac
[ "$CP_CHD" -ge 5800 ] && [ "$CP_CHD" -le 6200 ] || die "chunked clip duration_ms $CP_CHD, want ≈6000 (the whole body)"
code=$(http DELETE "$BASE/api/clips/$CP_CH" -b "$JARW"); expect_deleted "$code" "$CP_CH" "DELETE the chunked clip"
# Cleanup: both clips (owner session) and the token itself, which must stop working at once.
code=$(http DELETE "$BASE/api/clips/$CP_G" -b "$JARW"); expect_deleted "$code" "$CP_G" "DELETE G"
code=$(http DELETE "$BASE/api/clips/$CP_H" -b "$JARW")
case $code in 204|202) echo "[e2e]   DELETE H $CP_H → $code" ;; *) die "DELETE H → $code, want 204/202" ;; esac
code=$(http DELETE "$BASE/api/auth/tokens/$CP_SXTID" -b "$JARW"); expect "$code" 204 "revoke capture token"
code=$(get "$BASE/api/auth/me" -H "Authorization: Bearer $CP_SXTOK"); expect "$code" 401 "revoked token is rejected"
step 29 "the capture upload waits for the CARD (§20 errata 97, 212): what ShareX pastes unfurls as a PLAYER card"
# Step 18 revoked its own token, so mint one for this step and revoke it again at the end.
code=$(http POST "$BASE/api/auth/tokens" -b "$JARW" -H 'Content-Type: application/json' -d '{"name":"e2e ready-wait"}')
expect "$code" 201 "mint the ready-wait token"
CP_RWTOK=$(json token); expect_nonempty "$CP_RWTOK" "ready-wait raw token"
CP_RWTID=$(json id); expect_nonempty "$CP_RWTID" "ready-wait token id"
# The reply is HELD until the clip's card works — CAPTURE_READY_WAIT, and nothing a client sends,
# since erratum 212. Without that hold the 201 would be `queued`, and the first Discordbot fetch of
# the pasted link would be answered 503 by the hold of erratum 213 (67-capture.sh's step 68) rather than with the card
# somebody is waiting for.
t0=$SECONDS
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -H "Authorization: Bearer $CP_RWTOK" \
  -F "title=E2E ready wait" -F "ttl_days=1" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "capture upload of a small clip ($(cat "$RESP"))"
CP_I=$(json id); expect_nonempty "$CP_I" "I id"
CP_IURL=$(json url); expect "$CP_IURL" "$BASE/$CP_I" "the url a waited upload pastes"
expect "$(json state)" ready "state in the 201 of a waited upload — the whole point of the wait"
expect "$(json card_ready)" true "card_ready in the 201 — what erratum 212 made the wait about"
echo "[e2e]   waited upload $CP_I answered ready+card_ready after $((SECONDS - t0))s"
# THE assertion this feature exists for: fetch exactly what went on the clipboard, as Discord does.
code=$(page "$CP_IURL" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot GET of the pasted url"
has_file "$PAGE" 'property="og:video"' "og:video on the pasted link's card"
has_file "$PAGE" 'name="twitter:player"' "twitter:player on the pasted link's card"
has_file "$PAGE" 'property="og:type" content="video.other"' "og:type of the pasted link's card"
has_file "$PAGE" 'name="twitter:card" content="player"' "twitter:card of the pasted link's card"
# A `wait` field is REFUSED, whatever it says: a client may not configure the card away for everyone
# who reads its links (erratum 212; 67-capture.sh's step 67 walks the whole value table).
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -H "Authorization: Bearer $CP_RWTOK" \
  -F "wait=0" -F "title=E2E no wait" -F "ttl_days=1" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 400 "capture upload with wait=0 ($(cat "$RESP"))"
expect "$(json error)" invalid_request "the wait=0 error code"
contains "$(json message)" CAPTURE_READY_WAIT || die "the wait=0 message does not name the setting: $(json message)"
# No 429 assertion here: the upload limiters are shared with every later step and a tripped bucket
# would poison the run. RATE_UPLOAD_PER_HOUR and RATE_UPLOAD_PER_HOUR_PER_IP are unit-tested.
# Cleanup: the clip and the token, which stops at once.
code=$(http DELETE "$BASE/api/clips/$CP_I" -b "$JARW"); expect_deleted "$code" "$CP_I" "DELETE I"
code=$(http DELETE "$BASE/api/auth/tokens/$CP_RWTID" -b "$JARW"); expect "$code" 204 "revoke the ready-wait token"
code=$(get "$BASE/api/auth/me" -H "Authorization: Bearer $CP_RWTOK"); expect "$code" 401 "the revoked ready-wait token"
summary_row G "$CP_G" "capture API: sxcu-shaped POST /api/upload → 201, url = share page, player card; deleted"
summary_row H "$CP_H" "capture API: file part first → title falls back to the filename; deleted"
summary_row I "$CP_I" "capture API: the reply held until the card works (ready + card_ready in the 201); deleted"
