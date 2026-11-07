# 22-gif.sh — GIF export (#6): the on-demand 202 → 200, bounded, cached, revalidated, regenerated with its
# one conflict, invalidated by a trim, gone with the clip. Step 22, which was the serial core of
# scripts/e2e.sh until run 9's lane `pool` (scripts/e2e.d/README.md "The core in the pool").
#
# e2e-schedule: parallel
# e2e-schedule-apart: 43-watch,70-web
# e2e-namespace: account=- prefix=GF_ buckets=view
# e2e-covers: internal/httpapi/gif.go internal/worker/gif.go internal/db/gif.go internal/transcode/gif.go
#   Its subject is its OWN clip (GF_B, the noaudio fixture the harness's B is made from; it was B itself
#   until run 9, and B is now 7-clip's), deleted at the end because "the preview goes with the clip" is a
#   claim worth failing on. Every count is GF_B's own prefix; the one instance-wide read is `admin stats`'
#   schema line, which no clip moves.
#   It DRAINS the `view` bucket: preview.gif carries it and the step polls it once a second while a preview
#   renders. 43-watch (a preview-GIF poll) and 70-web (a filmstrip poll) drain the same bucket, so it is
#   apart from both — the pair the two of them already keep.
#
# Variables are prefixed GF_.

step 22 "GIF export (#6): on-demand 202 → 200, bounded, cached, invalidated by a trim, gone with the clip"
code=$(upload GF_B noaudio-720p30.mp4 '{"title":"E2E gif subject"}'); expect "$code" 201 "upload GF_B, the GIF subject ($(cat "$RESP"))"
GF_B=$(json id); expect_nonempty "$GF_B" "GF_B id"
wait_ready "$GF_B" B
# 0. The schema this whole feature needs, on the live volume — which was created by 0001 alone.
out=$(dc exec -T app clippet admin stats --no-storage) || die "admin stats failed: $out"
contains "$out" 'applied=1,2,3' || die "migrations 0002/0003 are not applied on the live volume: $out"
echo "[e2e]   $(printf '%s\n' "$out" | sed -n 's/^schema: */schema /p' | tr -d '\r')"
# 1. The ANONYMOUS first request: 202 (not 404, not 500 — the clip exists and a preview is being made).
code=$(get "$BASE/api/clips/$GF_B/preview.gif"); expect "$code" 202 "first GET preview.gif ($(cat "$RESP"))"
case $(json state) in queued|running) ;; *) die "202 body state '$(json state)', want queued|running" ;; esac
expect "$(hdr Retry-After)" 5 "202 Retry-After"
code=$(get "$BASE/api/clips/$GF_B"); expect "$code" 200 "GET B"
gif_url_now=$(json gif_url)
# The preview can be READY by now (1 s on the GPU stack; the fifteenth gate saw it land between these two requests),
# so the null-before-exists rule is judged against what preview.gif answers at this instant, not against a timer.
# The two halves read B at DIFFERENT instants, on purpose: a 202 here proves the preview did not exist when B was
# read a moment earlier (it cannot un-exist), so THAT read must have been null; a 200 here proves nothing about the
# earlier read — the preview may have landed between the two requests (the twentieth gate, 2026-10-05, on a quiet
# box: 202, B null, then 200) — so the exists half re-reads B AFTER the 200 and judges that.
code=$(get "$BASE/api/clips/$GF_B/preview.gif")
if [ "$code" = 202 ]; then
  expect_empty "$gif_url_now" "gif_url before the preview exists (§8.2: null, not absent)"
else
  expect "$code" 200 "preview.gif while gif_url is judged"
  code=$(get "$BASE/api/clips/$GF_B"); expect "$code" 200 "GET B after the preview exists"
  expect_nonempty "$(json gif_url)" "gif_url once the preview exists (§8.2)"
  echo "[e2e]   preview was already ready when gif_url was judged (the null-before-exists half is not observable on this stack)"
fi
# 2. Poll to 200. The job is bounded by construction, so 90s is generous.
t0=$SECONDS
while :; do
  code=$(get "$BASE/api/clips/$GF_B/preview.gif")
  case $code in
    200) break ;;
    202) ;;
    *) die "preview.gif → $code ($(cat "$RESP"))" ;;
  esac
  [ $((SECONDS - t0)) -lt 90 ] || die "the preview was not ready within 90s"
  sleep 1   # was 3 (lane speed2): preview.gif spends the `view` bucket, whose refill is one a second
done
echo "[e2e]   preview ready after $((SECONDS - t0))s"
expect "$(hdr Content-Type)" image/gif "preview Content-Type"
GF_ETAG=$(hdr ETag); expect_nonempty "$GF_ETAG" "preview ETag"
contains "$(hdr Cache-Control)" 'max-age=3600' || die "preview Cache-Control: $(hdr Cache-Control)"
expect_empty "$(hdr Retry-After)" "a served preview must not carry Retry-After"
# 3. The bytes themselves, measured by the host ffprobe: a real, animated, bounded GIF.
GF_FILE="$CTX_DIR/B-preview.gif"
code=$(curl -sS -o "$(W "$GF_FILE")" -D "$HDRW" -w '%{http_code}' "$BASE/api/clips/$GF_B/preview.gif")
expect "$code" 200 "download the preview"
GF_BYTES=$(wc -c < "$GF_FILE" | tr -d ' ')
[ "$GF_BYTES" -le 8388608 ] || die "the preview is $GF_BYTES bytes, past the 8 MiB cap"
ffprobe -v error -select_streams v:0 -count_frames \
  -show_entries stream=codec_name,width,height,nb_read_frames -print_format json "$(W "$GF_FILE")" \
  > "$CTX_DIR/B-preview.json" || die "ffprobe on the preview failed"
GF_J=$(W "$CTX_DIR/B-preview.json")
GF_CODEC=$(jget "$GF_J" streams.0.codec_name); GF_W=$(jget "$GF_J" streams.0.width)
GF_H=$(jget "$GF_J" streams.0.height);        GF_FRAMES=$(jget "$GF_J" streams.0.nb_read_frames)
expect "$GF_CODEC" gif "the preview's codec"
{ [ "$GF_W" -gt 0 ] && [ "$GF_W" -le 480 ]; } || die "preview width $GF_W, want 1..480"
[ "$GF_FRAMES" -gt 1 ] || die "the preview has $GF_FRAMES frame(s); it does not animate"
echo "[e2e]   preview: ${GF_W}x${GF_H}, $GF_FRAMES frames, $GF_BYTES bytes"
# 4. It is a media route like any other: served again at once, and revalidated by ETag.
code=$(get "$BASE/api/clips/$GF_B/preview.gif"); expect "$code" 200 "second GET is served, not queued"
# …and it is one of the two routes that are NEVER signed, in any delivery mode (§20 erratum 278(a)):
# this app's own JavaScript polls preview.gif's STATUS with `fetch`, and a `fetch` that follows a
# cross-origin 302 lands in a CORS check it cannot read, so the GIF button would poll out silently.
# The 200 above is what proves it — all this adds is the sentence that says so, on the run where the
# rest of the media routes are answering 302.
if [ "$E2E_MEDIA" = redirect ]; then
  expect_empty "$(hdr Location)" "preview.gif was redirected, which would break the page's own poll"
  echo "[e2e]   redirect: preview.gif is app-served (it is polled by fetch, which cannot read a cross-origin 302)"
fi
code=$(get "$BASE/api/clips/$GF_B/preview.gif" -H "If-None-Match: $GF_ETAG"); expect "$code" 304 "If-None-Match → 304"
# 5. The DTO says so, to everyone: the preview is as public as the poster.
code=$(get "$BASE/api/clips/$GF_B" -b "$JARW"); expect "$code" 200 "owner GET B"
expect "$(json gif_url)" "$BASE/api/clips/$GF_B/preview.gif" "owner gif_url"
code=$(get "$BASE/api/clips/$GF_B"); expect "$(json gif_url)" "$BASE/api/clips/$GF_B/preview.gif" "anonymous gif_url"
# 6. The object is in the bucket, under the clip's own prefix, and the admin CLI sees it.
out=$(dc exec -T app clippet admin ls "$GF_B" --stat) || die "admin ls B failed"
if contains "$out" MISSING; then die "admin ls B --stat reports a missing object: $out"; fi
contains "$out" 'present ' || die "admin ls B --stat printed no bucket lines: $out"
GF_KEY=$(printf '%s\n' "$out" | sed -n 's#.*present \(.*renditions/preview-[^ ]*\).*#\1#p' | tr -d '\r')
expect_nonempty "$GF_KEY" "the preview key in the bucket"
echo "[e2e]   preview key $GF_KEY"
# 7. A malformed id is a 404 like every other media route (a DESTROYED clip's is asserted at the end, on
#    this clip once it has gone).
code=$(get "$BASE/api/clips/nope/preview.gif"); expect "$code" 404 "preview.gif for a malformed id"
# 8. The owner's explicit regenerate, and its one conflict.
GF_BOBJ=$(prefix_count "$GF_B")
code=$(http POST "$BASE/api/clips/$GF_B/gif" -b "$JARW"); expect "$code" 202 "POST gif ($(cat "$RESP"))"
expect "$(json state)" queued "POST gif state"
# The 409 is observable only while the first job is LIVE. On the GPU stack a 6 s preview renders inside the gap
# between two edge round trips (the twentieth gate, 2026-10-05: 202 then 202), and then the second POST is a
# regenerate of its own — a live job is the only 409 (§20 erratum 87), so a 202 after the first has landed is the
# rule holding, not breaking. Either answer is judged; a 202 here is settled below before the object count is read.
code=$(http_refused POST "$BASE/api/clips/$GF_B/gif" -b "$JARW")
GF_SECOND=$code
case $code in
  409) expect "$(json error)" gif_in_progress "second POST error code" ;;
  202) echo "[e2e]   the first regenerate had landed before the second POST (the 409 half is not observable on this stack)" ;;
  *) die "a second POST: got '$code', want 409 while the first runs or 202 once it has landed ($(cat "$RESP"))" ;;
esac
code=$(http_refused POST "$BASE/api/clips/$GF_B/gif"); expect "$code" 401 "anonymous POST gif"
# It has landed when the KEY changes: every attempt writes a fresh {r8} (§6), so that — not the ETag —
# is what moves. Re-rendering an unchanged rendition is deterministic, so the BYTES (and therefore the
# ETag) are identical, which is the right outcome: no cache anywhere is churned by a regenerate.
t0=$SECONDS
while :; do
  out=$(dc exec -T app clippet admin ls "$GF_B" --stat) || die "admin ls B failed"
  GF_KEY2=$(printf '%s\n' "$out" | sed -n 's#.*present \(.*renditions/preview-[^ ]*\).*#\1#p' | tr -d '\r')
  { [ -n "$GF_KEY2" ] && [ "$GF_KEY2" != "$GF_KEY" ]; } && break
  [ $((SECONDS - t0)) -lt 90 ] || die "the regenerated preview did not land within 90s (key '$GF_KEY2')"
  sleep 1   # was 3 (lane speed2): one `admin ls` exec a turn
done
if [ "$GF_SECOND" = 202 ]; then
  # The second regenerate is still landing: preview.gif answers 202 while a gif job is queued or running.
  t0=$SECONDS
  while [ "$(get "$BASE/api/clips/$GF_B/preview.gif")" != 200 ]; do
    [ $((SECONDS - t0)) -lt 90 ] || die "the second regenerate did not settle within 90s"
    sleep 1   # was 2 (lane speed2)
  done
  out=$(dc exec -T app clippet admin ls "$GF_B" --stat) || die "admin ls B failed"
  GF_KEY2=$(printf '%s\n' "$out" | sed -n 's#.*present \(.*renditions/preview-[^ ]*\).*#\1#p' | tr -d '\r')
fi
if contains "$out" MISSING; then die "admin ls B --stat reports a missing object after the regenerate: $out"; fi
expect_prefix_count "$GF_B" "$GF_BOBJ" "B bucket objects after the regenerate (the superseded preview must be deleted)"
code=$(head_ "$BASE/api/clips/$GF_B/preview.gif"); expect "$code" 200 "HEAD the regenerated preview"
expect "$(hdr ETag)" "$GF_ETAG" "the same rendition re-rendered must be byte-identical (a regenerate churns no cache)"
echo "[e2e]   regenerated: key $GF_KEY -> $GF_KEY2, same bytes, one preview object"
# 9. A trim throws the preview away: it was cut from frames that no longer exist.
code=$(http POST "$BASE/api/clips/$GF_B/trim" -b "$JARW" -H 'Content-Type: application/json' -d '{"start_ms":0,"end_ms":3000}')
expect "$code" 202 "POST trim B ($(cat "$RESP"))"
wait_trim "$GF_B" B
code=$(get "$BASE/api/clips/$GF_B"); expect "$code" 200 "GET B after the trim"
expect "$(json state)" ready "B state after the trim"
expect_empty "$(json gif_url)" "gif_url after a trim (the preview is stale by definition)"
out=$(dc exec -T app clippet admin ls "$GF_B" --stat) || die "admin ls B failed"
if contains "$out" MISSING; then die "admin ls B --stat reports a missing object after the trim: $out"; fi
if contains "$out" 'renditions/preview-'; then die "the trimmed clip still references a preview: $out"; fi
n=$(prefix_count "$GF_B"); [ "$n" -lt "$GF_BOBJ" ] || die "the superseded preview was not deleted (objects $GF_BOBJ -> $n)"
echo "[e2e]   the trim dropped the preview: $GF_BOBJ -> $n objects"
code=$(get "$BASE/api/clips/$GF_B/preview.gif"); expect "$code" 202 "preview.gif after a trim queues a fresh one"
# 10. And the preview goes with the clip: the prefix must end empty (this is why B is deleted here).
code=$(http DELETE "$BASE/api/clips/$GF_B" -b "$JARW")
case $code in 204|202) echo "[e2e]   DELETE B $GF_B → $code" ;; *) die "DELETE B → $code, want 204/202" ;; esac
wait_gone "$GF_B" B
GF_BID=$GF_B; GF_B=
# A clip that is not there is a 404, exactly like every other media route — this one destroyed a moment ago.
code=$(get "$BASE/api/clips/$GF_BID/preview.gif"); expect "$code" 404 "preview.gif for a destroyed clip"
expect "$(json error)" not_found "destroyed clip error code"
summary_row GB "$GF_BID" "noaudio (the GIF subject, its own since run 9): GIF (#6) made, regenerated, dropped by a trim, gone with the clip"
