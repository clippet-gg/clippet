# 20-pages.sh — what the run's pages and lists say: Discord auto-post off (or on) for the run's own clips,
# tags and collections, the crawler policy and the response headers, and the share/download affordances.
# Steps 20, 23, 27 and 30, which were the serial core of scripts/e2e.sh until run 9's lane `pool`
# (scripts/e2e.d/README.md "The core in the pool").
#
# e2e-schedule: parallel
# e2e-namespace: account=c prefix=PG_ buckets=-
# e2e-covers: internal/worker/notify.go internal/db/tags.go internal/db/collections.go internal/db/library.go
# e2e-covers: internal/httpapi/collections.go internal/web/pages.go internal/web/server.go internal/web/crawlerhold.go
# e2e-covers: web/templates/collection.html web/templates/index.html web/templates/clip.html internal/httpapi/media.go
#   Step 20 was one of the core's four instance-wide reads (a GLOBAL notify count from `admin stats`) and is
#   SCOPED since run 9: it counts notify jobs only for the clips the run's owner holds (`admin ls --owner`
#   then `admin jobs --kind notify`, filtered by e2e_notify_scoped_), and the only whole-log read left is
#   for the webhook's BOOT line, a fact about the stack's own process (61-nits2's rule). Step 23 tags C —
#   a write no other bundle reads (they read C's media, its pages and its collections of their own) — and
#   makes its other clips (PG_W, PG_X) and its third user (`bundle_account c`) itself; step 27 flips
#   PG_X, not C, because C is read by six bundles and an unlisted C would change what their pages say.
#   Step 30 only reads C. Every count is this bundle's own (a tag no other run can use, its collection).
#
# Variables are prefixed PG_.

step 20 "Discord auto-post (#4): every listed clip posted, unlisted never, and nothing at all when unset"
# SCOPED TO THIS RUN (run 9, lane `pool`): the clips are the ones the run's owner holds right now — the
# harness's C, E and X at least, and whatever another bundle has uploaded as the owner and not yet
# deleted — and the notify rows counted are theirs alone. A notify row of any other account (an aborted
# POSITIVE run's leftover on a shared stack, which is what the old global `notify=0` used to trip on)
# cannot move it. e2e_notify_scoped_ is the filter, and the E2E self-test pins it on a planted listing.
PG_NOTIFY_LOG="$CTX_DIR/app-notify.log"
dc logs --no-color --since "$START_TS" app > "$PG_NOTIFY_LOG" 2>&1 || die "docker compose logs failed"
out=$(dc exec -T app clippet admin ls --owner "$E2E_USER") || die "admin ls --owner $E2E_USER failed: $out"
PG_IDS=$(printf '%s\n' "$out" | tr -d '\r' | awk 'NR > 1 && NF >= 4 && $1 != "ID" && $2 != "clips" { print $1 }' | tr '\n' ' ')
contains " $PG_IDS" " $C " || die "admin ls --owner $E2E_USER does not list the harness's C: $out"
dc exec -T app clippet admin jobs --kind notify --limit 10000 > "$CTX_DIR/notify-jobs.txt" 2>&1 || die "admin jobs --kind notify failed: $(cat "$CTX_DIR/notify-jobs.txt")"
# shellcheck disable=SC2086 # PG_IDS is a list of ids
PG_NOTIFY=$(e2e_notify_scoped_ "$CTX_DIR/notify-jobs.txt" $PG_IDS)
echo "[e2e]   the run's $(printf '%s' "$PG_IDS" | wc -w | tr -d ' ') clip(s) hold $PG_NOTIFY notify job(s); the instance holds $(grep -c '^#[0-9]' "$CTX_DIR/notify-jobs.txt" || true) in all"
if [ -z "${E2E_DISCORD_WEBHOOK_URL:-}" ]; then
  # The default run, and the one that proves the §15 promise: with DISCORD_WEBHOOK_URL unset the binary
  # opens no outbound connection but S3's and the webhook you set (CLIP_WEBHOOK_URL, the bot and the
  # sign-in are each opt-in, §15). The listing counts notify jobs in EVERY state, so a delivered one
  # would still show; zero means none was ever queued for the run's clips — the enqueue is inside the
  # ready transaction (C, E and X are ready; a clip another bundle is still encoding counts too, harmlessly).
  expect "$PG_NOTIFY" 0 "notify jobs for the run's own clips with DISCORD_WEBHOOK_URL unset"
  # The webhook's BOOT line is the stack process's own (no bundle restarts it); a delivery line names its
  # clip, and none may name one of the run's.
  if grep -n 'discord webhook configured' "$PG_NOTIFY_LOG"; then die "the app booted with a webhook (line above) although DISCORD_WEBHOOK_URL is unset"; fi
  for id in $PG_IDS; do
    if grep -n "discord webhook.*clip=$id" "$PG_NOTIFY_LOG"; then die "webhook log line(s) above for the run's clip $id with DISCORD_WEBHOOK_URL unset"; fi
  done
  echo "[e2e]   no notify job was ever queued for the run's clips and the app booted with no webhook"
  echo "[e2e]   SKIP (E2E_DISCORD_WEBHOOK_URL unset): the posting half needs a real Discord webhook, for a run by hand with real credentials"
else
  has_file "$PG_NOTIFY_LOG" 'discord webhook configured' "the boot line naming the webhook host and id"
  if grep -Fq "$E2E_DISCORD_WEBHOOK_URL" "$PG_NOTIFY_LOG"; then die "the app log contains the webhook URL — it is a secret (its token is the path)"; fi
  # Every LISTED clip of the harness's must have been posted (C and X; 7-clip's A, B and D are its own,
  # and may be gone — with their notify rows — before this runs). The
  # notify job runs after the ready flip, so the delivery line is waited for, not assumed.
  posted=0
  for id in "$C" "$X"; do
    t0=$SECONDS
    # a here-string, not `dc logs | grep -q` (an early-exiting reader on a pipe is a SIGPIPE 141 under pipefail: erratum 338)
    until grep -Eq "discord webhook delivered.*clip=$id.*status=2[0-9][0-9]" <<< "$(dc logs --no-color --since "$START_TS" app 2>&1)"; do
      [ $((SECONDS - t0)) -lt 60 ] || die "the listed clip $id was never delivered to Discord (60 s)"
      sleep 1
    done
    posted=$((posted + 1))
  done
  dc logs --no-color --since "$START_TS" app > "$PG_NOTIFY_LOG" 2>&1 || die "docker compose logs failed"
  # …and the unlisted one must NOT have been. That is the whole opt-out: no flag, no API, no config.
  if grep -Eq "discord webhook delivered.*clip=$E" "$PG_NOTIFY_LOG"; then die "the UNLISTED clip $E was auto-posted"; fi
  [ "$PG_NOTIFY" -gt 0 ] || die "the webhook is configured but no notify job exists for the run's clips"
  echo "[e2e]   $posted listed clips delivered; the unlisted clip $E was not; the URL never reached a log line"
fi
step 23 "tags + collections (#9): a grammar, a filter that pages, and a public list of links"
# 0. The schema this whole feature needs, on the live volume — which was created by 0001 alone.
out=$(dc exec -T app clippet admin stats --no-storage) || die "admin stats failed: $out"
contains "$out" 'applied=1,2,3,4' || die "migration 0004 is not applied on the live volume: $out"
echo "[e2e]   $(printf '%s\n' "$out" | sed -n 's/^schema: */schema /p' | tr -d '\r')"
# The stack is SHARED and persistent, so the filter assertions key on a tag no other run can have used.
PG_TAG="e2e-${E2E_USER#e2e_}${E2E_ATTEMPT#1}"   # a retry (E2E_ATTEMPT=2) tags with a fresh word: its first attempt's clips may still carry the old one
echo "[e2e]   run tag $PG_TAG"

# 1. PATCH tags on C: the server normalises (trim, lower-case, dedupe, sort) whatever was sent.
code=$(http PATCH "$BASE/api/clips/$C" -b "$JARW" -H 'Content-Type: application/json' \
  -d "{\"tags\":[\"  Gaming \",\"$PG_TAG\",\"GAMING\"]}")
expect "$code" 200 "PATCH tags on C ($(cat "$RESP"))"
expect "$(json tags.0)" "$PG_TAG" "C tags[0] (sorted: e- before g-)"
expect "$(json tags.1)" gaming "C tags[1] (lower-cased and deduped)"
expect_empty "$(json tags.2)" "C tags[2] (three inputs, two tags)"
# A rejection NAMES the offending word — a rejected list is useless to somebody who cannot see which
# word was wrong — and changes nothing.
code=$(http_refused PATCH "$BASE/api/clips/$C" -b "$JARW" -H 'Content-Type: application/json' -d '{"tags":["ok","NOT OK"]}')
expect "$code" 400 "PATCH an illegal tag"
expect "$(json error)" invalid_tags "illegal tag error code"
contains "$(json message)" 'NOT OK' || die "the 400 does not name the offending tag: $(cat "$RESP")"
code=$(http PATCH "$BASE/api/clips/$C" -b "$JARW" -H 'Content-Type: application/json' \
  -d '{"tags":["a","b","c","d","e","f","g","h","i","j","k"]}')
expect "$code" 400 "PATCH eleven tags"
expect "$(json error)" invalid_tags "eleven-tag error code"
contains "$(json message)" '10' || die "the cap 400 does not name the cap: $(cat "$RESP")"
code=$(get "$BASE/api/clips/$C"); expect "$(json tags.0)" "$PG_TAG" "C's tags survived both rejections"
# Clearing, and putting them back: [] is how "no tags" is spelled in both directions.
code=$(http PATCH "$BASE/api/clips/$C" -b "$JARW" -H 'Content-Type: application/json' -d '{"tags":[]}')
expect "$code" 200 "PATCH [] clears"
expect_empty "$(json tags.0)" "tags after clearing"
has_file "$RESP" '"tags":\[\]' "an empty tag set must serialise as [], not null"
# Absent means unchanged: a title-only edit must not wipe the tags.
code=$(http PATCH "$BASE/api/clips/$C" -b "$JARW" -H 'Content-Type: application/json' -d "{\"tags\":[\"$PG_TAG\",\"vertical\"]}")
expect "$code" 200 "re-tag C"
code=$(http PATCH "$BASE/api/clips/$C" -b "$JARW" -H 'Content-Type: application/json' -d '{"title":"E2E vertical"}')
expect "$code" 200 "title-only PATCH"
expect "$(json tags.0)" "$PG_TAG" "a title-only PATCH left the tags alone"
# And tags are an OWNER mutation (§20 erratum 19): a capture token gets 403, not a write.
code=$(http POST "$BASE/api/auth/tokens" -b "$JARW" -H 'Content-Type: application/json' -d '{"name":"e2e tags"}')
expect "$code" 201 "mint a token for #9"
PG_TAGTOK=$(json token); PG_TAGTID=$(json id); expect_nonempty "$PG_TAGTOK" "the #9 token"
code=$(http_refused PATCH "$BASE/api/clips/$C" -H "Authorization: Bearer $PG_TAGTOK" -H 'Content-Type: application/json' -d '{"tags":["x"]}')
expect "$code" 403 "token PATCH tags"
expect "$(json error)" token_not_allowed "token PATCH tags error code"

# 2. Tags at upload time, through both upload routes.
code=$(upload PG_W small-640x360.mp4 "{\"title\":\"E2E tagged upload\",\"ttl_days\":1,\"tags\":[\"$PG_TAG\",\"Upload-Tag\"]}")
expect "$code" 201 "upload W with tags ($(cat "$RESP"))"
PG_W=$(json id); expect_nonempty "$PG_W" "W id"
expect "$(json tags.0)" "$PG_TAG" "W tags[0] straight from the 201"
expect "$(json tags.1)" upload-tag "W tags[1] (lower-cased)"
# The capture form spells the same set comma-separated (§20 erratum 58's grammar, one more field).
# The sleep is a COURTESY, not the guarantee: two uploads inside one created_at second come back in
# either order (the list breaks ties by random id — a documented limitation, internal/db/clips.go's
# ORDER BY comment), and this second of daylight makes that rarer. It cannot do more than that,
# because the app container's clock also steps BACKWARDS by ten to twenty seconds after heavy ffmpeg
# work, which is exactly what steps 21-22 just did. Part 3 below therefore asserts the ORDER as the
# property it is (the list is sorted by the timestamps it reports) and the SET as a set, and pins
# only C, which is ~20 steps older than any correction reaches.
sleep 1.2
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -H "Authorization: Bearer $PG_TAGTOK" \
  -F "title=E2E tagged capture" -F "ttl_days=1" -F "tags= $PG_TAG , Capture-Tag ," \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/small-640x360.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "capture upload with tags ($(cat "$RESP"))"
PG_X=$(json id); expect_nonempty "$PG_X" "X id"
expect_empty "$(json tags)" "the capture 201 stays erratum 58's five fields"
code=$(get "$BASE/api/clips/$PG_X"); expect "$code" 200 "GET X"
expect "$(json tags.0)" capture-tag "X tags[0] (trimmed and lower-cased; c- sorts before e-)"
expect "$(json tags.1)" "$PG_TAG" "X tags[1] from the capture form"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -H "Authorization: Bearer $PG_TAGTOK" \
  -F "tags=fine, NOT OK" -F duplicate=allow -F "file=@$(W "$PWD/testclips/small-640x360.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 400 "capture upload with an illegal tag"
expect "$(json error)" invalid_tags "capture illegal tag error code"
code=$(http DELETE "$BASE/api/auth/tokens/$PG_TAGTID" -b "$JARW"); expect "$code" 204 "revoke the #9 token"

# 3. The filter: exactly the tagged set, in the library's own order, and it pages.
code=$(get "$BASE/api/clips?tag=$PG_TAG" -b "$JARW"); expect "$code" 200 "GET /api/clips?tag="
# WHICH of W and X is newest is not something a test on this host may assert: the app container's
# clock steps backwards by ten to twenty seconds after a burst of ffmpeg work, so the capture upload
# can legitimately carry an EARLIER created_at than the form upload a second before it (`admin ls`
# has shown a 16 s inversion here, and it cost two full gate runs). What the list must do is order
# itself by the timestamps it REPORTS and hold exactly the three tagged clips; C, uploaded ~20 steps
# earlier, is the one position further back than any clock correction reaches. Do not "fix" this by
# lengthening the sleep above, and do not touch the ORDER BY or the cursor: `created_at DESC, id ASC`
# and its same-second tie are settled (see the ORDER BY comment in internal/db/clips.go).
PG_TAGFIRST=$(json clips.0.id); PG_TAGSECOND=$(json clips.1.id)
expect_nonempty "$PG_TAGFIRST" "filtered list[0]"; expect_nonempty "$PG_TAGSECOND" "filtered list[1]"
not_before "$(json clips.0.created_at)" "$(json clips.1.created_at)" "the filtered list is newest first"
not_before "$(json clips.1.created_at)" "$(json clips.2.created_at)" "the filtered list is newest first (rows 1,2)"
case "$PG_TAGFIRST $PG_TAGSECOND" in
  "$PG_X $PG_W"|"$PG_W $PG_X") ;;
  *) die "the filter's two newest rows are $PG_TAGFIRST,$PG_TAGSECOND; want the two tagged uploads $PG_X and $PG_W";;
esac
expect "$(json clips.2.id)" "$C" "filtered list[2] (the oldest tagged clip)"
expect_empty "$(json clips.3.id)" "the filtered list holds exactly the three tagged clips"
expect_empty "$(json next_cursor)" "no cursor on a three-row page"
# An UPPERCASE filter is the same filter: the grammar normalises before the query.
code=$(get "$BASE/api/clips?tag=$(printf '%s' "$PG_TAG" | tr 'a-z' 'A-Z')" -b "$JARW")
expect "$(json clips.0.id)" "$PG_TAGFIRST" "an uppercase ?tag= is the same filter"
# Pagination: one row per page, and the cursor carries its filter.
PG_PAGED=; PG_CUR=; PG_N=0
while :; do
  if [ -z "$PG_CUR" ]; then code=$(get "$BASE/api/clips?tag=$PG_TAG&limit=1" -b "$JARW")
  else code=$(get "$BASE/api/clips?tag=$PG_TAG&limit=1&cursor=$PG_CUR" -b "$JARW"); fi
  expect "$code" 200 "filtered page $PG_N"
  PG_PAGED="$PG_PAGED $(json clips.0.id)"
  PG_CUR=$(json next_cursor); PG_N=$((PG_N + 1))
  [ -n "$PG_CUR" ] || break
  [ "$PG_N" -lt 6 ] || die "the filtered list did not terminate within 6 pages"
done
expect "$PG_PAGED" " $PG_TAGFIRST $PG_TAGSECOND $C" "paging the filter one row at a time (the unpaged sequence)"
# A cursor is minted UNDER its filter: spending it without one would silently skip rows.
code=$(get "$BASE/api/clips?tag=$PG_TAG&limit=1" -b "$JARW"); PG_CUR=$(json next_cursor); expect_nonempty "$PG_CUR" "a filtered cursor"
code=$(get "$BASE/api/clips?limit=1&cursor=$PG_CUR" -b "$JARW"); expect "$code" 400 "a filtered cursor spent unfiltered"
expect "$(json error)" invalid_cursor "stale-filter cursor error code"
# A tag that cannot exist is a 400 that names it, never an empty page.
code=$(get "$BASE/api/clips?tag=BAD%21" -b "$JARW"); expect "$code" 400 "?tag=BAD!"
expect "$(json error)" invalid_tags "?tag=BAD! error code"
contains "$(json message)" 'BAD!' || die "the 400 does not name the tag: $(cat "$RESP")"

# 4. The pages: the OWNER edits tags on the share page; the ANONYMOUS watch page is the clip and
#    nothing else — no editor, no chips, not even the words (§9).
code=$(page "$BASE/$C" -b "$JARW"); expect "$code" 200 "owner share page"
has_file "$PAGE" "data-tags-edit data-api-url=\"/api/clips/$C\"" "owner share page: the tag editor"
has_file "$PAGE" "value=\"$PG_TAG, vertical\" data-tags-input" "owner share page: the editor is pre-filled"
code=$(page "$BASE/$C"); expect "$code" 200 "anonymous share page"
lacks_file "$PAGE" 'data-tags-edit' "anonymous share page"
lacks_file "$PAGE" 'data-tags-input' "anonymous share page"
lacks_file "$PAGE" "$PG_TAG" "anonymous share page carries no tag text at all"
# The library shows them as chips that link back into the filter, and nothing more: no counts.
code=$(page "$BASE/?tag=$PG_TAG" -b "$JARW"); expect "$code" 200 "library filtered by tag"
has_file "$PAGE" "data-tag-filter=\"$PG_TAG\"" "the library says which filter is on"
has_file "$PAGE" "<a class=\"tag\" href=\"/?tag=$PG_TAG\">$PG_TAG</a>" "the library renders tag chips"
has_file "$PAGE" "data-clip-id=\"$C\"" "the filtered library holds C"
code=$(page "$BASE/?tag=NOT%20OK" -b "$JARW"); expect "$code" 303 "an illegal ?tag= on the library"
expect "$(hdr Location)" / "an illegal ?tag= bounces to the whole library"

# 5. A collection: created, filled in a CHOSEN order, and public at its own unguessable link.
code=$(http POST "$BASE/api/collections" -b "$JARW" -H 'Content-Type: application/json' -d '{"title":"E2E collection"}')
expect "$code" 201 "POST /api/collections ($(cat "$RESP"))"
PG_COL=$(json id); expect_nonempty "$PG_COL" "collection id"
expect "$(json title)" "E2E collection" "collection title"
expect "$(json url)" "$BASE/c/$PG_COL" "collection url (absolute, on the public origin)"
expect "$(json clip_count)" 0 "a fresh collection is empty"
# The order sent is the order stored — and it is deliberately NOT the library's newest-first.
code=$(http PUT "$BASE/api/collections/$PG_COL/clips" -b "$JARW" -H 'Content-Type: application/json' \
  -d "{\"clip_ids\":[\"$C\",\"$PG_W\",\"$C\"]}")
expect "$code" 200 "PUT the membership ($(cat "$RESP"))"
expect "$(json clip_count)" 2 "duplicates collapse"
code=$(get "$BASE/api/collections/$PG_COL"); expect "$code" 200 "anonymous GET the collection"
expect "$(json clips.0.id)" "$C" "membership[0] is the order that was sent, not the library's"
expect "$(json clips.1.id)" "$PG_W" "membership[1]"
expect_empty "$(json clips.2.id)" "exactly two members"
expect "$(json clips.0.tags.0)" "$PG_TAG" "the embedded clips carry their tags"
lacks_file "$RESP" '"views"' "a collection never carries a view count"
code=$(get "$BASE/api/collections/$PG_COL" -b "$JARW"); expect "$code" 200 "owner GET the collection"
lacks_file "$RESP" '"views"' "not even for the owner: a list of links is not where a private count belongs"
# The membership is the OWNER's own clips: a second user's clip is 400 not_your_clip, naming it.
# The third user is this bundle's own account (`c`, declared in the header): bundle_account registers it.
bundle_account c; PG_JAR3W=$BA_JARW; PG_USER3=$BA_USER
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$PG_JAR3W" \
  -F "title=E2E stranger" -F "ttl_days=1" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/small-640x360.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "the third user's upload ($(cat "$RESP"))"
PG_Y=$(json id); expect_nonempty "$PG_Y" "Y id"
code=$(http PUT "$BASE/api/collections/$PG_COL/clips" -b "$JARW" -H 'Content-Type: application/json' \
  -d "{\"clip_ids\":[\"$C\",\"$PG_Y\"]}")
expect "$code" 400 "PUT with another user's clip"
expect "$(json error)" not_your_clip "another user's clip error code"
contains "$(json message)" "$PG_Y" || die "the 400 does not name the offending clip: $(cat "$RESP")"
code=$(get "$BASE/api/collections/$PG_COL"); expect "$(json clip_count)" 2 "a rejected PUT changed nothing"
# A collection is the owner's to change: a stranger gets 404, not 403 — the mutating routes scope by
# owner, so "not there" and "not yours" are literally the same query result (§20 erratum 82).
code=$(http_refused PATCH "$BASE/api/collections/$PG_COL" -b "$PG_JAR3W" -H 'Content-Type: application/json' -d '{"title":"mine now"}')
expect "$code" 404 "a stranger's PATCH"
expect "$(json error)" not_found "a stranger's PATCH error code"
code=$(http_refused DELETE "$BASE/api/collections/$PG_COL" -b "$PG_JAR3W"); expect "$code" 404 "a stranger's DELETE"
code=$(http DELETE "$BASE/api/clips/$PG_Y" -b "$PG_JAR3W"); expect_deleted "$code" "$PG_Y" "the third user deletes their clip"
code=$(http POST "$BASE/api/auth/logout" -b "$PG_JAR3W" || echo 000); echo "[e2e]   logout $PG_USER3 → $code"
# …and a capture token cannot make one at all (§20 erratum 19 again).
code=$(http POST "$BASE/api/auth/tokens" -b "$JARW" -H 'Content-Type: application/json' -d '{"name":"e2e collections"}')
expect "$code" 201 "mint a token for the collection negative"
PG_COLTOK=$(json token); PG_COLTID=$(json id)
code=$(http_refused POST "$BASE/api/collections" -H "Authorization: Bearer $PG_COLTOK" -H 'Content-Type: application/json' -d '{"title":"nope"}')
expect "$code" 403 "token POST /api/collections"
expect "$(json error)" token_not_allowed "token POST error code"
code=$(http DELETE "$BASE/api/auth/tokens/$PG_COLTID" -b "$JARW"); expect "$code" 204 "revoke the collection token"

# 6. The card a pasted collection link unfurls into: a LINK-OUT card, always. A collection is several
#    clips, so an unfurler that carried og:video would have to pick one of them for the reader.
code=$(page "$BASE/c/$PG_COL" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot GET /c/$PG_COL"
has_file "$PAGE" '<meta property="og:type" content="website">' "collection og:type"
has_file "$PAGE" "<meta property=\"og:title\" content=\"E2E collection\">" "collection og:title"
has_file "$PAGE" "<meta property=\"og:url\" content=\"$BASE/c/$PG_COL\">" "collection og:url"
has_file "$PAGE" "<meta property=\"og:image\" content=\"$BASE/api/clips/$C/poster.jpg\">" \
  "collection og:image = the FIRST ready member's poster"
has_file "$PAGE" '<meta name="twitter:card" content="summary_large_image">' "collection twitter:card"
has_file "$PAGE" "<link rel=\"canonical\" href=\"$BASE/c/$PG_COL\">" "collection canonical"
has_file "$PAGE" '<title>E2E collection</title>' "collection <title>"
lacks_file "$PAGE" 'og:video' "a collection card carries no video"
lacks_file "$PAGE" 'twitter:player' "a collection card carries no player"
lacks_file "$PAGE" 'data-views' "a collection page shows no view count"
lacks_file "$PAGE" 'style=' "collection page: no inline style (CSP style-src self)"
lacks_file "$PAGE" '<style' "collection page: no style element"
# The body is the list itself: every member links to its own share page.
has_file "$PAGE" "href=\"/$C\"" "the collection page links to C"
has_file "$PAGE" "href=\"/$PG_W\"" "the collection page links to W"
lacks_file "$PAGE" 'data-collection-controls' "an anonymous visitor gets no controls"
code=$(page "$BASE/c/$PG_COL" -b "$JARW"); expect "$code" 200 "owner GET /c/$PG_COL"
has_file "$PAGE" 'data-collection-controls' "the owner's controls"
has_file "$PAGE" "data-clips-url=\"/api/collections/$PG_COL/clips\"" "the owner's membership route"
has_file "$PAGE" 'data-collection-delete data-confirm-text="Really delete"' "delete is two clicks, never a confirm dialog"
# The membership picker is a CHECKBOX LIST (§20 erratum 95). It was a
# <select multiple>, where an unmodified click REPLACES the selection: one
# stray click sent a PUT holding a single id and silently dropped every other
# member. Each current member is ticked, and taking any of them out now costs
# a second click.
has_file "$PAGE" "<input type=\"checkbox\" name=\"clip_ids\" value=\"$C\" checked=\"checked\">" \
  "the picker is a checkbox list and the member C is ticked"
has_file "$PAGE" "<input type=\"checkbox\" name=\"clip_ids\" value=\"$PG_W\" checked=\"checked\">" \
  "…and so is W, the other current member"
lacks_file "$PAGE" '<select' "the membership picker must not be a <select multiple>"
has_file "$PAGE" 'data-members-hint' "a save that removes members takes a second click"
# What the swap must NOT change is the ordering contract: document order is
# what the client PUTs, and the page renders the current members first in
# their position order. Sending back exactly what the page shows is a no-op —
# proved from the wire, since the click itself is out of reach here.
code=$(http PUT "$BASE/api/collections/$PG_COL/clips" -b "$JARW" -H 'Content-Type: application/json' \
  -d "{\"clip_ids\":[\"$C\",\"$PG_W\"]}")
expect "$code" 200 "PUT the membership exactly as the page renders it ($(cat "$RESP"))"
expect "$(json clip_count)" 2 "re-saving the rendered set changed the count"
code=$(get "$BASE/api/collections/$PG_COL"); expect "$code" 200 "GET after the no-op save"
expect "$(json clips.0.id)" "$C" "membership[0] survived the re-save in order"
expect "$(json clips.1.id)" "$PG_W" "membership[1] survived the re-save in order"
expect_empty "$(json clips.2.id)" "still exactly two members"
# And the library lists it — the only place a collection is ever listed.
code=$(page "$BASE/" -b "$JARW"); expect "$code" 200 "library"
has_file "$PAGE" "data-collection-id=\"$PG_COL\"" "the library's Collections section"
has_file "$PAGE" 'data-collection-new' "the create form"
# An unknown or malformed collection id is a true 404 with no card at all.
code=$(page "$BASE/c/AAAAAAAAAAAA"); expect "$code" 404 "an unknown collection id"
lacks_file "$PAGE" 'og:title' "the collection 404 carries no card tags"
has_file "$PAGE" 'name="robots" content="noindex"' "the collection 404 is noindex"
code=$(page "$BASE/c/xxxx"); expect "$code" 404 "a malformed collection id"
code=$(get "$BASE/api/collections/AAAAAAAAAAAA"); expect "$code" 404 "GET an unknown collection"
expect "$(json error)" not_found "unknown collection error code"

# 7. Deleting a clip removes it from every collection through the FK cascade — no membership step
#    anywhere — and deleting the collection takes rows and NOT ONE clip.
code=$(http DELETE "$BASE/api/clips/$PG_W" -b "$JARW"); expect_deleted "$code" "$PG_W" "DELETE W"
code=$(get "$BASE/api/collections/$PG_COL"); expect "$code" 200 "GET the collection after the member went"
expect "$(json clip_count)" 1 "the deleted clip left the collection"
expect "$(json clips.0.id)" "$C" "the surviving member"
expect_empty "$(json clips.1.id)" "exactly one member now"
code=$(http DELETE "$BASE/api/collections/$PG_COL" -b "$JARW"); expect "$code" 204 "DELETE the collection"
code=$(get "$BASE/api/collections/$PG_COL"); expect "$code" 404 "GET after the collection was deleted"
code=$(page "$BASE/c/$PG_COL"); expect "$code" 404 "/c/{id} after the collection was deleted"
code=$(get "$BASE/api/clips/$C"); expect "$code" 200 "the collection's clip outlived the collection"
expect "$(json state)" ready "C is untouched by any of this"
echo "[e2e]   collection $PG_COL: 2 members in a chosen order, cascaded to 1, then gone; C untouched"
step 27 "crawler policy and response headers (§20 erratum 94)"
# The flip's subject is PG_X — this bundle's own listed clip, tagged in step 23 — and not C: six bundles
# read C's pages beside this one, and an unlisted C would change what they read.
# An unlisted clip is "do not surface this" and its link is its only authentication, but nothing in the
# app ever told a crawler so: no /robots.txt at all, and no `noindex` on any clip page. The two
# mechanisms are not interchangeable and both are proven here — `meta robots` does not stop og:
# unfurling (the tags are read, the page is simply not indexed), and robots.txt does not stop it either
# for an agent it allows. Neither is a security control; an id is still 71 bits.
code=$(get "$BASE/robots.txt"); expect "$code" 200 "GET /robots.txt"
contains "$(hdr content-type)" 'text/plain' || die "robots.txt Content-Type: '$(hdr content-type)'"
# Since the id moved to the root (§20 erratum 248) the honest general block is the WHOLE site plus an
# Allow for each of the four addresses meant to be found. A prefix list would have closed nothing.
has_file "$RESP" 'Disallow: /' "robots.txt closes the whole site to general crawlers"
has_file "$RESP" 'Allow: /\$' "robots.txt allows the front door"   # \$ = a literal $ (has_file greps a basic regex)
has_file "$RESP" 'Allow: /login' "robots.txt allows /login"
has_file "$RESP" 'Allow: /register' "robots.txt allows /register"
has_file "$RESP" 'Allow: /static/' "robots.txt allows /static/"
lacks_file "$RESP" 'Disallow: /clips/' "robots.txt no longer names a prefix no share link uses"
# The unfurler groups are what keep the card working: a named User-agent group REPLACES the `*` group
# for that agent, and an empty Disallow there means "allow everything".
has_file "$RESP" 'User-agent: Discordbot' "robots.txt names Discordbot"
has_file "$RESP" 'User-agent: Twitterbot' "robots.txt names Twitterbot"
$PY - "$RESP" <<'PYEOF' || die "an unfurler group does not allow everything"
import io, sys
lines = io.open(sys.argv[1], encoding='utf-8').read().splitlines()
for bot in ('Discordbot', 'Twitterbot', 'facebookexternalhit', 'Slackbot-LinkExpanding'):
    head = 'User-agent: ' + bot
    if head not in lines:
        sys.exit('robots.txt has no %s group' % bot)
    nxt = lines[lines.index(head) + 1].strip()
    if nxt != 'Disallow:':
        sys.exit('%s is followed by %r, want a bare "Disallow:"' % (bot, nxt))
PYEOF
# E was uploaded unlisted and nothing since has changed that: its page must carry the directive.
code=$(page "$BASE/$E"); expect "$code" 200 "share page of the unlisted clip"
has_file "$PAGE" 'content="noindex"' "the unlisted clip's page is noindex"
# PG_X is listed. Flip it both ways: the directive is a function of the FLAG, not of the template. It came
# through the capture route, which waits for the card but not necessarily for ready, so it is waited for.
wait_ready "$PG_X" X
code=$(page "$BASE/$PG_X"); expect "$code" 200 "share page of the listed clip"
lacks_file "$PAGE" 'content="noindex"' "a listed clip's page"
has_file "$PAGE" 'og:title' "the listed clip's card"   # the card is untouched by any of this
code=$(http PATCH "$BASE/api/clips/$PG_X" -b "$JARW" -H 'Content-Type: application/json' -d '{"unlisted":true}')
expect "$code" 200 "PATCH X unlisted"; expect "$(json unlisted)" true "X unlisted → unlisted"
code=$(page "$BASE/$PG_X"); expect "$code" 200 "share page of X while unlisted"
has_file "$PAGE" 'content="noindex"' "X while unlisted"
has_file "$PAGE" 'og:title' "X's card while unlisted"  # allowing the unfurlers is the whole point
code=$(http PATCH "$BASE/api/clips/$PG_X" -b "$JARW" -H 'Content-Type: application/json' -d '{"unlisted":false}')
expect "$code" 200 "PATCH X listed again"; expect "$(json unlisted)" false "X listed again → unlisted false"
code=$(page "$BASE/$PG_X"); expect "$code" 200 "share page of X after the flip back"
lacks_file "$PAGE" 'content="noindex"' "X after the flip back"
# The three CSP directives default-src does not cover. base-uri and form-action have NO fallback in
# CSP3: unset, one injected <base> would repoint every relative URL on the page and a rewritten form
# action would post the login off-origin.
PG_CSP=$(hdr Content-Security-Policy)
contains "$PG_CSP" "base-uri 'none'" || die "share page CSP lacks base-uri: '$PG_CSP'"
contains "$PG_CSP" "form-action 'self'" || die "share page CSP lacks form-action: '$PG_CSP'"
contains "$PG_CSP" "object-src 'none'" || die "share page CSP lacks object-src: '$PG_CSP'"
# HSTS follows the PUBLIC origin's scheme, not the request's — TLS is terminated upstream — so an http
# dev stack must send none and a public https origin must send one.
PG_HSTS=$(hdr strict-transport-security)
if [ "$SECURE" = 1 ]; then
  contains "$PG_HSTS" 'max-age=31536000' || die "no HSTS on an https origin: '$PG_HSTS'"
  if contains "$PG_HSTS" 'preload'; then die "HSTS asks for preload — that is the domain owner's call"; fi
else
  expect_empty "$PG_HSTS" "HSTS on an http origin"
fi
# Step 30 CREATES NOTHING and therefore cleans up nothing: the subject is C — ready, owned by this run's
# first user, alive until step 17 — and the anonymous half deliberately carries no cookie jar.
step 30 "share and download affordances (§20 erratum 98): ?download=1, and a share row every viewer gets"
PG_VIDC="$BASE/api/clips/$C/video.mp4"
# (a) The parameter adds a Content-Disposition and changes NOTHING else. Same object, same validator:
#     nothing but the query string — which is already its own cache key, hence no Vary — separates the
#     two answers, so a shared cache holding one can never hand out the wrong one.
#     In `redirect` mode the ask travels as `response-content-disposition` on the presigned link —
#     the only place it can — so the disposition assertions still read the answer the viewer gets
#     (§20 errata 278(g) and 280). The two headers that say the app did nothing ELSE differently,
#     Vary and Cache-Control, belong to a response a 302 replaces: the 302 carries `no-store` and no
#     Vary whatever the query string says, which `media_head` asserts on both fetches already.
code=$(media_head "$PG_VIDC"); expect_media "$code" 200 "HEAD C video.mp4"
PG_CETAG=$(hdr etag); expect_nonempty "$PG_CETAG" "C video.mp4 etag"
PG_CVARY=$(hdr vary); PG_CCC=$(hdr cache-control)
if [ "$E2E_MEDIA" = proxy ]; then
  expect_empty "$(hdr content-disposition)" "a plain video.mp4 must not be an attachment"
else
  # The presign asks the bucket for `inline` on a plain fetch, which is the same claim in the one
  # vocabulary this mode has: a thing to PLAY, never a thing to save.
  if contains "$(hdr content-disposition)" attachment; then die "a plain video.mp4 must not be an attachment: '$(hdr content-disposition)'"; fi
  expect "$(hdr content-disposition)" inline "a plain video.mp4 is presigned inline"
fi
code=$(media_head "$PG_VIDC?download=1"); expect_media "$code" 200 "HEAD C video.mp4?download=1"
PG_CD=$(hdr content-disposition)
contains "$PG_CD" 'attachment' || die "?download=1 did not set an attachment disposition: '$PG_CD'"
contains "$PG_CD" '.mp4' || die "?download=1 did not name a .mp4: '$PG_CD'"
expect "$(hdr etag)" "$PG_CETAG" "the etag must not depend on ?download=1"
if [ "$E2E_MEDIA" = proxy ]; then
  expect "$(hdr vary)" "$PG_CVARY" "?download=1 must not change Vary"
  expect "$(hdr cache-control)" "$PG_CCC" "?download=1 must not change Cache-Control"
else
  media_skip "Vary and Cache-Control on the object"
fi
echo "[e2e]   $PG_CD"
# (b) A save is resumable exactly as a stream is: the same 206, the same Content-Range.
code=$(media_get "$PG_VIDC" -H 'Range: bytes=0-99'); expect "$code" 206 "ranged plain GET"
PG_CRANGE=$(hdr content-range); expect_nonempty "$PG_CRANGE" "plain ranged Content-Range"
code=$(media_get "$PG_VIDC?download=1" -H 'Range: bytes=0-99'); expect "$code" 206 "ranged ?download=1"
expect "$(hdr content-range)" "$PG_CRANGE" "the same Content-Range with and without ?download=1"
# (c) The share row is PUBLIC: fetched with NO cookie jar, exactly as a friend arriving from Discord
#     sees it. Its owner-only half — the keep-alive and the link to the untouched upload — is not.
code=$(page "$BASE/$C"); expect "$code" 200 "anonymous share page"
has_file "$PAGE" 'data-copy-embed' "the copy-embed control on an anonymous share page"
has_file "$PAGE" "video.mp4?download=1" "the download link on an anonymous share page"
lacks_file "$PAGE" 'data-touch-url' "the keep-alive URL is the owner's alone (§20 erratum 62)"
lacks_file "$PAGE" "clips/$C/original" "the link to the original is the owner's alone"
# (d) ...and the owner gate on /original is untouched by any of this, in both directions.
code=$(head_ "$BASE/api/clips/$C/original?download=1")
case $code in 401|403) ;; *) die "anonymous original?download=1 -> $code, want 401/403";; esac
code=$(head_ "$BASE/api/clips/$C/original?download=1" -b "$JARW"); expect "$code" 200 "the owner's original?download=1"
contains "$(hdr content-disposition)" 'attachment' || die "the original lost its attachment disposition"
echo "[e2e]   public row on $C: copy link, copy embed, download; original still owner-only"
code=$(http DELETE "$BASE/api/clips/$PG_X" -b "$JARW"); expect_deleted "$code" "$PG_X" "DELETE PG_X"
summary_row C "$C" "vertical: ready 720x1280, ttl_days=0 → never; tagged (#9) and the collection's first member; read by six bundles; cleaned up"
summary_row W "$PG_W" "tags (#9) at upload: filtered, second member of the collection in a CHOSEN order, deleted — and it left the collection"
summary_row TX "$PG_X" "tags (#9) through the capture form: comma-separated, normalised, filtered; flipped unlisted and back (step 27); deleted"
summary_row Y "$PG_Y" "a THIRD user's clip: the one thing a collection PUT must refuse (400 not_your_clip); deleted by its owner"
