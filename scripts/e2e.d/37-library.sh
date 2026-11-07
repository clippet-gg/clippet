# 37-library.sh — the library: search, "mine only", LIBRARY_SCOPE, batched progress and bulk actions.
#
# e2e-schedule: parallel
# e2e-namespace: account=s1,s2 prefix=LIB_ buckets=-
# e2e-covers: internal/db/library.go internal/db/bulk.go internal/httpapi/bulk.go internal/httpapi/progress_batch.go
# e2e-covers: internal/httpapi/clip_handlers.go internal/httpapi/collections.go internal/web/pages.go
# e2e-covers: web/templates/index.html
#   Until lane `excl`, step 38 started a SECOND app process against the LIVE database and scratch
#   volume, which it could only do while the live queue was idle (that process's boot reconcile repairs
#   every `running` row it finds and its worker claims from the one queue — nit sweep S27), and that
#   kept the bundle exclusive. Step 38 now runs its LIBRARY_SCOPE=own app on an instance of its OWN:
#   a throwaway container with its own database, scratch, key prefix (and STORAGE_DIR on a directory
#   stack), and inside it a SECOND process on the same private database with the scope left `shared` —
#   which is what the live app used to stand in for, so "both clips are visible through a shared app on
#   the same database" is still asserted, against a database nothing else uses. The live queue is
#   never touched, so there is nothing to wait for. Steps 37, 39 and 40 use clips of their own and read
#   the shared ones; step 39's batched progress spends the `view` bucket ~8 times and step 40's
#   collection GETs ~5, well inside the burst of 60 beside the suite's two `view` pollers.
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Steps 37-40. Everything it creates it destroys; nothing it does outlives the run.

# ---------------------------------------------------------------------------------------------------
step 37 "library search and mine-only: ?q= and ?mine=1 narrow the same set, the cursor carries them"
# A needle no other run on this shared stack can have used.
LIB_Q="e2eq${E2E_USER#e2e_}"
echo "[e2e]   run needle $LIB_Q"

# Two clips, 1.2 s apart so `created_at DESC, id` is a total order and the paging below is
# deterministic (the seconds-scale tie-break trap). L1 carries the needle in its TITLE and L2 in its
# DESCRIPTION, because the filter reads both.
code=$(upload L1 small-640x360.mp4 "{\"title\":\"search $LIB_Q one\",\"ttl_days\":1}")
expect "$code" 201 "upload L1 ($(cat "$RESP"))"
L1=$(json id); expect_nonempty "$L1" "L1 id"
sleep 1.2
code=$(upload L2 small-640x360.mp4 "{\"title\":\"E2E search two\",\"description\":\"a note about $LIB_Q\",\"ttl_days\":1}")
expect "$code" 201 "upload L2 ($(cat "$RESP"))"
L2=$(json id); expect_nonempty "$L2" "L2 id"

# The filter is exactly the matching set — L1 by its TITLE and L2 by its DESCRIPTION — in the
# library's own order.
#
# The ORDER is asserted as the property it is ("newest first") rather than as "L2 then L1": the app
# container's clock steps backwards by ten to twenty seconds after a burst of ffmpeg work, so which
# of two uploads a second apart carries the later `created_at` is not something a test on this host
# may assume. What the list must do is order itself by the timestamps it reports, and that is what is
# checked here and what the paging below is measured against.
code=$(get "$BASE/api/clips?q=$LIB_Q" -b "$JARW"); expect "$code" 200 "GET /api/clips?q="
LIB_FIRST=$(json clips.0.id); LIB_SECOND=$(json clips.1.id)
expect_nonempty "$LIB_FIRST" "searched list[0]"
expect_nonempty "$LIB_SECOND" "searched list[1]"
expect_empty "$(json clips.2.id)" "the search holds exactly the two clips that carry the needle"
not_before "$(json clips.0.created_at)" "$(json clips.1.created_at)" "the search is newest first"
case "$LIB_FIRST $LIB_SECOND" in
  "$L1 $L2"|"$L2 $L1") ;;
  *) die "the search returned $LIB_FIRST,$LIB_SECOND; want exactly $L1 (title) and $L2 (description)";;
esac
# Case does not matter: the server normalises the needle and the column the same ASCII way.
code=$(get "$BASE/api/clips?q=$(printf '%s' "$LIB_Q" | tr 'a-z' 'A-Z')" -b "$JARW")
expect "$(json clips.0.id)" "$LIB_FIRST" "an uppercase ?q= is the same filter"
expect "$(json clips.1.id)" "$LIB_SECOND" "an uppercase ?q= is the same list"
# A LIKE metacharacter is a CHARACTER. Both probes are anchored on the needle and on a word L1's
# title really holds, so an UNESCAPED `_` or `%` would match L1 and an escaped one matches nothing:
# the assertion fails in exactly the case the ESCAPE clause exists for.
code=$(get "$BASE/api/clips?q=${LIB_Q}_one" -b "$JARW"); expect "$code" 200 "GET /api/clips?q=<needle>_one"
expect_empty "$(json clips.0.id)" "an _ in ?q= must be a character, not 'any character'"
code=$(get "$BASE/api/clips?q=${LIB_Q}%25one" -b "$JARW"); expect "$code" 200 "GET /api/clips?q=<needle>%one"
expect_empty "$(json clips.0.id)" "a % in ?q= must be a character, not 'anything'"

# Pagination: one row per page, and the cursor is minted UNDER the filter.
code=$(get "$BASE/api/clips?q=$LIB_Q&limit=1" -b "$JARW"); expect "$code" 200 "searched page 1"
expect "$(json clips.0.id)" "$LIB_FIRST" "searched page 1 row (the unpaged list's first)"
LIB_CUR=$(json next_cursor); expect_nonempty "$LIB_CUR" "a searched cursor"
code=$(get "$BASE/api/clips?q=$LIB_Q&limit=1&cursor=$LIB_CUR" -b "$JARW"); expect "$code" 200 "searched page 2"
expect "$(json clips.0.id)" "$LIB_SECOND" "searched page 2 row (the unpaged list's second)"
expect_empty "$(json next_cursor)" "the search ended after two rows"
code=$(get "$BASE/api/clips?limit=1&cursor=$LIB_CUR" -b "$JARW"); expect "$code" 400 "a searched cursor spent unfiltered"
expect "$(json error)" invalid_cursor "unfiltered-search cursor error code"
code=$(get "$BASE/api/clips?q=other&limit=1&cursor=$LIB_CUR" -b "$JARW"); expect "$code" 400 "a searched cursor spent under another needle"

# ?mine=1: the viewer's own clips, and no spelling of it names anybody else. The second user of
# steps 19/21/28 is logged back in rather than a fourth row being registered.
JARL="$E2E_DIR/cookies-lib.txt"; JARLW=$(W "$JARL"); rm -f "$JARL"
code=$(http POST "$BASE/api/auth/login" -c "$JARLW" -H 'Content-Type: application/json' \
  -d "{\"username\":\"${E2E_USER}_b\",\"password\":\"$E2E_PASS\"}")
expect "$code" 200 "log the second user in for the library filters"
code=$(get "$BASE/api/clips?limit=100&mine=1" -b "$JARW"); expect "$code" 200 "owner GET /api/clips?mine=1"
contains "$(cat "$RESP")" "\"id\":\"$L1\"" || die "?mine=1 dropped the caller's own clip $L1"
code=$(get "$BASE/api/clips?limit=100&mine=1" -b "$JARLW"); expect "$code" 200 "second user GET /api/clips?mine=1"
if contains "$(cat "$RESP")" "\"id\":\"$L1\""; then die "?mine=1 showed ${E2E_USER}_b somebody else's clip $L1"; fi
# …and the SHARED library still holds it, so what changed is the filter and not the visibility set.
code=$(get "$BASE/api/clips?limit=100" -b "$JARLW"); expect "$code" 200 "second user GET /api/clips"
contains "$(cat "$RESP")" "\"id\":\"$L1\"" || die "the shared library lost a listed clip"
# The combination narrows twice, and the cursor carries both.
code=$(get "$BASE/api/clips?q=$LIB_Q&mine=1&limit=1" -b "$JARW"); expect "$code" 200 "q + mine page 1"
expect "$(json clips.0.id)" "$LIB_FIRST" "q + mine row (both filters, same order)"
LIB_CUR=$(json next_cursor); expect_nonempty "$LIB_CUR" "a q+mine cursor"
code=$(get "$BASE/api/clips?q=$LIB_Q&limit=1&cursor=$LIB_CUR" -b "$JARW"); expect "$code" 400 "a q+mine cursor spent without mine"
expect "$(json error)" invalid_cursor "q+mine cursor error code"

# Illegal values bounce exactly as an illegal ?tag= does: 400 on the API, 303 to the whole library on
# the page. A filter that silently did nothing would show a WIDER list than the caller asked for.
LIB_LONG=aaaaaaaaaabbbbbbbbbbccccccccccddddddddddeeeeeeeeeeffffffffffggggg   # 65 characters, cap is 64
code=$(get "$BASE/api/clips?q=$LIB_LONG" -b "$JARW"); expect "$code" 400 "?q= over the 64-character cap"
expect "$(json error)" invalid_request "over-long ?q= error code"
code=$(get "$BASE/api/clips?mine=0" -b "$JARW"); expect "$code" 400 "?mine=0"
expect "$(json error)" invalid_request "?mine=0 error code"
code=$(get "$BASE/api/clips?mine=true" -b "$JARW"); expect "$code" 400 "?mine=true"
code=$(page "$BASE/?mine=0" -b "$JARW"); expect "$code" 303 "an illegal ?mine= on the library"
expect "$(hdr Location)" / "an illegal ?mine= bounces to the whole library"
code=$(page "$BASE/?q=$LIB_LONG" -b "$JARW"); expect "$code" 303 "an over-long ?q= on the library"
expect "$(hdr Location)" / "an over-long ?q= bounces to the whole library"

# The page: a plain GET form (so search works with no JavaScript at all), the filter line, and the
# box re-filled with what the server actually searched for.
code=$(page "$BASE/?q=$LIB_Q&mine=1" -b "$JARW"); expect "$code" 200 "the filtered library page"
has_file "$PAGE" '<form class="search" method="get" action="/"' "the library search is a plain GET form"
has_file "$PAGE" "data-q-filter=\"$LIB_Q\"" "the library says which search is on"
has_file "$PAGE" 'data-mine-filter="1"' "the library says mine-only is on"
has_file "$PAGE" "value=\"$LIB_Q\"" "the search box is re-filled with the normalised needle"
has_file "$PAGE" 'name="q" maxlength="64"' "the search box stops where CleanSearch does, so the page cannot hand the user a needle it discards"
has_file "$PAGE" "data-clip-id=\"$L1\"" "the filtered library holds L1"
lacks_file "$PAGE" 'onclick=' "the library page carries no inline handler"
echo "[e2e]   ?q= and ?mine=1 filter, page and bind their cursor; illegal values bounce"

# Both clips are driven to a terminal state before steps 39-40 read and change them (step 39 asserts
# both are ready). Step 38 no longer shares their database (the header).
lib_wait_terminal() {
  local id=$1 n=0 state
  while [ "$n" -lt 180 ]; do
    code=$(get "$BASE/api/clips/$id/progress" || echo 000)
    expect "$code" 200 "progress $id"
    state=$(json state)
    case $state in ready|failed) echo "[e2e]   $id -> $state"; return 0;; esac
    n=$((n + 1)); sleep "$POLL_SLEEP"   # the pool's `poll`-bucket pacing, as wait_ready (was a flat 1 s)
  done
  die "$id never reached a terminal state"
}
lib_wait_terminal "$L1"
lib_wait_terminal "$L2"

# ---------------------------------------------------------------------------------------------------
step 38 "LIBRARY_SCOPE=own: two users, two libraries, and nothing else about the instance changes"
# The lane's own app keeps its configured default for the whole run — recreating it would destroy the
# log history step 16 reads. The scope is proven on a THROWAWAY instance instead (the header says why it
# is no longer the live database): the same image, a database, scratch, key prefix and STORAGE_DIR of
# its own, LIBRARY_SCOPE=own on its published port, its own PUBLIC_BASE_URL (so its cookies are
# plain-http ones whatever origin the run itself is against), and a sweep interval it will never reach.
# Inside the same container a SECOND process serves the same private database with the scope left at
# the shipped `shared`, on a port of its own — the control: what changes between the two answers is the
# variable and not the data. It is removed at the end of the step, before the log gate.
LIB_PORT=$(envfile CLIPPET_PUBLISH); LIB_PORT=${LIB_PORT##*:}; LIB_PORT=${LIB_PORT:-8481}
LIB_SHPORT=$((LIB_PORT + 300))
LIB_PORT=$((LIB_PORT + 100))
LIB_BASE="http://localhost:$LIB_PORT"
LIB_SHBASE="http://localhost:$LIB_SHPORT"
LIB_CT="clippet-e2e-scope-$LIB_PORT"
LIB_PREFIX="e2e-scope-$(date +%s)/"
lib_stop() {
  docker stop -t 20 "$LIB_CT" >/dev/null 2>&1 || true
  docker rm -f "$LIB_CT" >/dev/null 2>&1 || true
  if store_has_bucket && store_rm_prefix_ok_ "${LIB_PREFIX:-}"; then
    ( store_obj rm --prefix "$LIB_PREFIX" >/dev/null ) 2>/dev/null \
      || echo "[e2e]   NOTE: could not remove the scope probe's prefix $LIB_PREFIX (its own, outside the stack's key prefix)"
  fi
}
lib_stop
# An EXIT trap so a failing assertion below cannot leave the instance running; `docker stop -t 20`
# before the rm, so its two processes shut down rather than being SIGKILLed mid-claim.
trap 'lib_stop' EXIT
dc run -d --name "$LIB_CT" --no-deps -p "127.0.0.1:$LIB_PORT:8080" -p "127.0.0.1:$LIB_SHPORT:8081" \
  -e DATA_DIR=/tmp/scope -e SCRATCH_DIR=/tmp/scope-scratch -e STORAGE_DIR=/tmp/scope-store \
  -e "S3_KEY_PREFIX=$LIB_PREFIX" \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e LIBRARY_SCOPE=own -e "PUBLIC_BASE_URL=$LIB_BASE" -e SWEEP_INTERVAL=24h -e REGISTRATION=open \
  --entrypoint sh app -c '
    mkdir -p /tmp/scope /tmp/scope-scratch /tmp/scope-store
    clippet migrate >/dev/null || exit 1
    LIBRARY_SCOPE=shared CLIPPET_LISTEN=:8081 clippet serve >/tmp/serve-shared.log 2>&1 &
    exec clippet serve' >/dev/null || die "could not start the LIBRARY_SCOPE=own container"
LIB_N=0
while [ "$LIB_N" -lt 60 ]; do
  code=$(get "$LIB_BASE/readyz" 2>/dev/null || echo 000)
  if [ "$code" = 200 ]; then
    code=$(get "$LIB_SHBASE/readyz" 2>/dev/null || echo 000)
    [ "$code" = 200 ] && break
  fi
  LIB_N=$((LIB_N + 1)); sleep 1
done
expect "$code" 200 "the LIBRARY_SCOPE=own instance (both of its processes) became ready"
echo "[e2e]   scoped app up on $LIB_BASE, its shared-scope twin on $LIB_SHBASE (one private database)"

# Two throwaway accounts with one clip each, created THROUGH the scoped app so every assertion below
# is that app's own answer. The capture route's ready-wait means each 201 is an encoded clip.
LIB_U1="${E2E_USER}_s1"; LIB_U2="${E2E_USER}_s2"
JARS1="$E2E_DIR/cookies-s1.txt"; JARS1W=$(W "$JARS1"); rm -f "$JARS1"
JARS2="$E2E_DIR/cookies-s2.txt"; JARS2W=$(W "$JARS2"); rm -f "$JARS2"
code=$(http POST "$LIB_BASE/api/auth/register" -c "$JARS1W" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$LIB_U1\",\"password\":\"$E2E_PASS\"}")
expect "$code" 201 "register $LIB_U1 on the scoped app"
code=$(http POST "$LIB_BASE/api/auth/register" -c "$JARS2W" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$LIB_U2\",\"password\":\"$E2E_PASS\"}")
expect "$code" 201 "register $LIB_U2 on the scoped app"
lib_upload() {  # lib_upload <jar-win-path> <title> → sets LIB_ID
  code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$1" \
    -F "title=$2" -F "ttl_days=1" \
    -F duplicate=allow -F "file=@$(W "$PWD/testclips/small-640x360.mp4");type=video/mp4" "$LIB_BASE/api/upload")
  expect "$code" 201 "scoped upload '$2' ($(cat "$RESP"))"
  LIB_ID=$(json id)
  expect_nonempty "$LIB_ID" "scoped upload '$2' id"
}
lib_upload "$JARS1W" "scoped one"; S1=$LIB_ID
lib_upload "$JARS2W" "scoped two"; S2=$LIB_ID

# The matrix. Both clips are LISTED, so under `shared` each user would see both.
code=$(get "$LIB_BASE/api/clips?limit=100" -b "$JARS1W"); expect "$code" 200 "scoped list, user 1"
contains "$(cat "$RESP")" "\"id\":\"$S1\"" || die "LIBRARY_SCOPE=own hid the caller's own clip"
if contains "$(cat "$RESP")" "\"id\":\"$S2\""; then die "LIBRARY_SCOPE=own listed another account's clip"; fi
code=$(get "$LIB_BASE/api/clips?limit=100" -b "$JARS2W"); expect "$code" 200 "scoped list, user 2"
contains "$(cat "$RESP")" "\"id\":\"$S2\"" || die "LIBRARY_SCOPE=own hid user 2's own clip"
if contains "$(cat "$RESP")" "\"id\":\"$S1\""; then die "LIBRARY_SCOPE=own listed user 1's clip to user 2"; fi
# The library PAGE agrees with the API, and says so rather than looking mysteriously short.
code=$(page "$LIB_BASE/" -b "$JARS1W"); expect "$code" 200 "the scoped library page"
has_file "$PAGE" 'data-library-scope="own"' "the scoped library page says the scope is on"
has_file "$PAGE" "data-clip-id=\"$S1\"" "the scoped library page holds the caller's clip"
lacks_file "$PAGE" "$S2" "the scoped library page holds nobody else's clip"
lacks_file "$PAGE" 'name="mine"' "a scoped library offers no mine-only toggle it could not turn off"
# It narrows a LIST and nothing else: the share link is still the authentication.
code=$(get "$LIB_BASE/api/clips/$S2" -b "$JARS1W"); expect "$code" 200 "the direct route under scope=own"
code=$(get "$LIB_BASE/api/clips/$S1"); expect "$code" 200 "an anonymous GET under scope=own"
# …and it never widens: a stranger still cannot edit a clip that is not theirs.
code=$(http_refused PATCH "$LIB_BASE/api/clips/$S2" -b "$JARS1W" -H 'Content-Type: application/json' -d '{"title":"mine now"}')
expect "$code" 403 "PATCH a stranger's clip under scope=own"

# The SAME database through a SHARED-scope app: both clips are visible there, to the very user the
# scoped app showed only one of — so what changed is the variable and not the data. (The session is a
# row of that one database, so user 1's cookie is good on both processes.)
code=$(get "$LIB_SHBASE/api/clips?limit=100" -b "$JARS1W"); expect "$code" 200 "the shared-scope app's library, user 1"
contains "$(cat "$RESP")" "\"id\":\"$S1\"" || die "the shared library lost $S1"
contains "$(cat "$RESP")" "\"id\":\"$S2\"" || die "the shared library lost $S2"
code=$(page "$LIB_SHBASE/" -b "$JARS1W"); expect "$code" 200 "the shared-scope library page"
lacks_file "$PAGE" 'data-library-scope="own"' "the shared-scope app says no scope is on"
echo "[e2e]   shared: both clips visible to everyone; own: each account sees only its own"

# Clean up everything this step made, then the instance (whose prefix goes with it).
code=$(http DELETE "$LIB_BASE/api/clips/$S1" -b "$JARS1W" || echo 000); echo "[e2e]   DELETE S1 $S1 -> $code"
code=$(http DELETE "$LIB_BASE/api/clips/$S2" -b "$JARS2W" || echo 000); echo "[e2e]   DELETE S2 $S2 -> $code"
code=$(http POST "$LIB_BASE/api/auth/logout" -b "$JARS1W" || echo 000)
code=$(http POST "$LIB_BASE/api/auth/logout" -b "$JARS2W" || echo 000)
rm -f "$JARS1" "$JARS2"
lib_stop
trap - EXIT
echo "[e2e]   scoped instance removed (its two users lived only in its own database)"

# ---------------------------------------------------------------------------------------------------
step 39 "batched progress: N clips, ONE request, a per-id answer, and a missing key means GONE"
# The library used to start an independent 1 s poll per unfinished row; forty rows meant ~40 requests
# a second at a box that was also running ffmpeg.
LIB_GONE="AaBbCcDdEeFf"      # a well-formed id no clip has
# Five live clips and one that is not there, in ONE request. (A, B and D are not among them: they are
# 7-clip's to expire, change and delete beside this bundle.)
code=$(get "$BASE/api/clips/progress?ids=$C,$E,$X,$L1,$L2,$LIB_GONE")
expect "$code" 200 "GET /api/clips/progress?ids="
expect "$(hdr cache-control)" no-store "the batch is never cached"
for id in "$C" "$E" "$X" "$L1" "$L2"; do
  expect "$(json "progress.$id.id")" "$id" "the batch answered for $id"
done
for id in "$C" "$L1" "$L2"; do
  expect "$(json "progress.$id.state")" ready "$id is ready"
  expect "$(json "progress.$id.percent")" 100 "$id is at 100%"
  expect "$(json "progress.$id.stage")" done "$id's stage"
done
expect_empty "$(json "progress.$LIB_GONE.state")" "an id no clip has must be ABSENT, never 'done'"
# It says the same thing the single-clip route says about the same clip — one DTO, two shapes.
code=$(get "$BASE/api/clips/$C/progress"); expect "$code" 200 "single progress C"
LIB_SINGLE=$(json state)
code=$(get "$BASE/api/clips/progress?ids=$C"); expect "$code" 200 "batch progress C"
expect "$(json "progress.$C.state")" "$LIB_SINGLE" "the batch and the single route agree"
# The bounds and the vocabulary. A malformed id is a 400 the caller cannot miss, never a silent
# omission that would read as "this clip is gone".
code=$(get "$BASE/api/clips/progress"); expect "$code" 400 "no ids at all"
expect "$(json error)" invalid_request "missing-ids error code"
code=$(get "$BASE/api/clips/progress?ids="); expect "$code" 400 "an empty ids list"
code=$(get "$BASE/api/clips/progress?ids=$C,not-an-id"); expect "$code" 400 "a malformed id in the list"
LIB_MANY=$LIB_GONE; LIB_N=1
while [ "$LIB_N" -lt 101 ]; do LIB_MANY="$LIB_MANY,$LIB_GONE"; LIB_N=$((LIB_N + 1)); done
code=$(get "$BASE/api/clips/progress?ids=$LIB_MANY"); expect "$code" 400 "101 ids"
expect "$(json error)" invalid_request "over-the-cap ids error code"
# It carries progress and NOTHING else: no title, no owner, no view count.
code=$(get "$BASE/api/clips/progress?ids=$C"); expect "$code" 200 "batch progress C again"
lacks_file "$RESP" '"views"' "the progress batch must not carry a view count"
lacks_file "$RESP" '"owner"' "the progress batch must not carry an owner"
lacks_file "$RESP" '"title"' "the progress batch must not carry a title"
echo "[e2e]   one request answered for 5 clips and stayed silent about a sixth"

# ---------------------------------------------------------------------------------------------------
step 40 "bulk actions: one request each, a per-id result, and a foreign id refused while the rest land"
# The foreign clip belongs to the second user, who is still logged in from step 37.
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARLW" \
  -F "title=E2E bulk stranger" -F "ttl_days=1" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/small-640x360.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "the second user's clip for the bulk matrix ($(cat "$RESP"))"
L3=$(json id); expect_nonempty "$L3" "L3 id"
LIB_BULK="[\"$L1\",\"$L3\",\"$L2\",\"$LIB_GONE\"]"

bulk() { http POST "$BASE/api/clips/bulk" -b "$JARW" -H 'Content-Type: application/json' -d "$1"; }
# The per-id shape is the same for every action: the ids come back in the order they were sent, each
# with the status the equivalent single-clip request would have answered with.
expect_bulk() {  # expect_bulk <index> <id> <status> <error|-> <what>
  expect "$(json "results.$1.id")" "$2" "$5: results[$1].id"
  expect "$(json "results.$1.status")" "$3" "$5: results[$1].status"
  if [ "$4" = "-" ]; then
    expect_empty "$(json "results.$1.error")" "$5: results[$1].error"
  else
    expect "$(json "results.$1.error")" "$4" "$5: results[$1].error"
  fi
}

# (a) Add a tag. It ADDS — the tags a clip already carries are still there afterwards, which is the
#     whole difference between this and the PATCH that replaces the set.
code=$(http PATCH "$BASE/api/clips/$L1" -b "$JARW" -H 'Content-Type: application/json' -d '{"tags":["keepme"]}')
expect "$code" 200 "seed a tag on L1"
code=$(bulk "{\"action\":\"tag\",\"tag\":\"$LIB_Q\",\"ids\":$LIB_BULK}")
expect "$code" 200 "bulk tag ($(cat "$RESP"))"
expect "$(json ok)" 2 "bulk tag: two succeeded"
expect "$(json failed)" 2 "bulk tag: two failed"
expect_bulk 0 "$L1" 200 - "bulk tag"
expect_bulk 1 "$L3" 403 forbidden "bulk tag"
expect_bulk 2 "$L2" 200 - "bulk tag"
expect_bulk 3 "$LIB_GONE" 404 not_found "bulk tag"
code=$(get "$BASE/api/clips/$L1"); expect "$code" 200 "GET L1 after the bulk tag"
expect "$(json tags.0)" "$LIB_Q" "L1 gained the bulk tag"
expect "$(json tags.1)" keepme "…and kept the tag it already had — this is an ADD, not a replace"
code=$(get "$BASE/api/clips/$L3"); expect "$code" 200 "GET L3 after the bulk tag"
expect_empty "$(json tags.0)" "a refused id was not tagged anyway"
# An illegal tag fails the REQUEST and names itself: it is one mistake, not four.
code=$(bulk "{\"action\":\"tag\",\"tag\":\"NOT OK\",\"ids\":$LIB_BULK}")
expect "$code" 400 "bulk tag with an illegal tag"
expect "$(json error)" invalid_tags "illegal bulk tag error code"
contains "$(json message)" 'NOT OK' || die "the 400 does not name the offending tag: $(cat "$RESP")"

# (b) Set the lifetime.
code=$(bulk "{\"action\":\"ttl\",\"ttl_days\":7,\"ids\":$LIB_BULK}")
expect "$code" 200 "bulk ttl ($(cat "$RESP"))"
expect_bulk 0 "$L1" 200 - "bulk ttl"
expect_bulk 1 "$L3" 403 forbidden "bulk ttl"
expect_bulk 3 "$LIB_GONE" 404 not_found "bulk ttl"
code=$(get "$BASE/api/clips/$L1"); expect "$(json ttl_days)" 7 "L1's new lifetime"
expect_ttl_ahead "$(json expires_at)" 604800 "L1 expires seven days out"
code=$(get "$BASE/api/clips/$L3"); expect "$(json ttl_days)" 1 "a refused id kept its own lifetime"
code=$(bulk "{\"action\":\"ttl\",\"ttl_days\":5,\"ids\":[\"$L1\"]}")
expect "$code" 400 "bulk ttl 5"
expect "$(json error)" invalid_ttl "bulk ttl 5 error code"

# (c) Add to a collection: an APPEND, so the order the owner already chose is not disturbed.
code=$(http POST "$BASE/api/collections" -b "$JARW" -H 'Content-Type: application/json' -d '{"title":"E2E bulk collection"}')
expect "$code" 201 "create the bulk collection ($(cat "$RESP"))"
LIB_COL=$(json id); expect_nonempty "$LIB_COL" "the bulk collection id"
code=$(http PUT "$BASE/api/collections/$LIB_COL/clips" -b "$JARW" -H 'Content-Type: application/json' -d "{\"clip_ids\":[\"$C\"]}")
expect "$code" 200 "seed the collection with C"
code=$(bulk "{\"action\":\"collection\",\"collection_id\":\"$LIB_COL\",\"ids\":$LIB_BULK}")
expect "$code" 200 "bulk add to collection ($(cat "$RESP"))"
expect_bulk 0 "$L1" 200 - "bulk collection"
expect_bulk 1 "$L3" 403 forbidden "bulk collection"
expect_bulk 2 "$L2" 200 - "bulk collection"
expect_bulk 3 "$LIB_GONE" 404 not_found "bulk collection"
code=$(get "$BASE/api/collections/$LIB_COL"); expect "$code" 200 "GET the bulk collection"
expect "$(json clips.0.id)" "$C" "the seeded member kept its position"
expect "$(json clips.1.id)" "$L1" "the appended members follow, in the order they were sent"
expect "$(json clips.2.id)" "$L2" "…and the second one after it"
expect "$(json clip_count)" 3 "a refused id joined nothing"
# Somebody else's collection is 404, exactly as every other collection mutation answers.
code=$(http_refused POST "$BASE/api/clips/bulk" -b "$JARLW" -H 'Content-Type: application/json' \
  -d "{\"action\":\"collection\",\"collection_id\":\"$LIB_COL\",\"ids\":[\"$L3\"]}")
expect "$code" 404 "a stranger's bulk add to collection"
expect "$(json error)" not_found "stranger-collection error code"
expect "$(json message)" "no such collection" "stranger-collection message"
# A collection_id that was never sent is the SAME answer, message included: the clip named is the
# caller's own and exists, so a 404 saying "no such clip" would send them after the wrong parameter
# (§20 erratum 30).
code=$(bulk "{\"action\":\"collection\",\"ids\":[\"$L1\"]}")
expect "$code" 404 "a bulk collection action with no collection_id"
expect "$(json error)" not_found "missing-collection_id error code"
expect "$(json message)" "no such collection" "the 404 names the collection, not the clip"
code=$(bulk "{\"action\":\"collection\",\"collection_id\":\"not-an-id\",\"ids\":[\"$L1\"]}")
expect "$code" 404 "a bulk collection action with a malformed collection_id"
expect "$(json message)" "no such collection" "the malformed-id 404 names the collection"
code=$(get "$BASE/api/collections/$LIB_COL"); expect "$(json clip_count)" 3 "a rejected request added nobody"

# (d) The posture: session only, CSRF-guarded, JSON only, bounded.
code=$(http_refused POST "$BASE/api/clips/bulk" -H 'Content-Type: application/json' -d "{\"action\":\"ttl\",\"ttl_days\":1,\"ids\":[\"$L1\"]}")
expect "$code" 401 "an anonymous bulk"
code=$(http POST "$BASE/api/auth/tokens" -b "$JARW" -H 'Content-Type: application/json' -d '{"name":"e2e bulk"}')
expect "$code" 201 "mint a token for the bulk posture check"
LIB_TOK=$(json token); LIB_TID=$(json id)
code=$(http_refused POST "$BASE/api/clips/bulk" -H "Authorization: Bearer $LIB_TOK" -H 'Content-Type: application/json' \
  -d "{\"action\":\"ttl\",\"ttl_days\":1,\"ids\":[\"$L1\"]}")
expect "$code" 403 "a capture token bulk"
expect "$(json error)" token_not_allowed "token bulk error code"
code=$(http DELETE "$BASE/api/auth/tokens/$LIB_TID" -b "$JARW"); expect "$code" 204 "revoke the bulk token"
code=$(http_refused POST "$BASE/api/clips/bulk" -b "$JARW" -H 'Content-Type: application/json' -H 'Origin: https://evil.example' \
  -d "{\"action\":\"ttl\",\"ttl_days\":1,\"ids\":[\"$L1\"]}")
expect "$code" 403 "a cross-origin bulk"
code=$(http_refused POST "$BASE/api/clips/bulk" -b "$JARW" -H 'Content-Type: application/x-www-form-urlencoded' -d 'action=delete')
expect "$code" 415 "a form-encoded bulk"
code=$(bulk "{\"action\":\"burn\",\"ids\":[\"$L1\"]}"); expect "$code" 400 "an unknown bulk action"
LIB_MANY="\"$LIB_GONE\""; LIB_N=1
while [ "$LIB_N" -lt 101 ]; do LIB_MANY="$LIB_MANY,\"$LIB_GONE\""; LIB_N=$((LIB_N + 1)); done
code=$(bulk "{\"action\":\"delete\",\"ids\":[$LIB_MANY]}"); expect "$code" 400 "101 ids in one bulk"
expect "$(json error)" invalid_request "over-the-cap bulk error code"

# (e) Delete: the rest of the set goes, the foreign id stays, and the collection loses the members
#     that went without losing the one that is still there.
code=$(bulk "{\"action\":\"delete\",\"ids\":$LIB_BULK}")
expect "$code" 200 "bulk delete ($(cat "$RESP"))"
expect_bulk 1 "$L3" 403 forbidden "bulk delete"
expect_bulk 3 "$LIB_GONE" 404 not_found "bulk delete"
for i in 0 2; do
  case "$(json "results.$i.status")" in
    204|202) ;;
    *) die "bulk delete: results[$i].status = $(json "results.$i.status"), want 204 or 202";;
  esac
  expect_empty "$(json "results.$i.error")" "bulk delete: results[$i].error"
done
code=$(get "$BASE/api/clips/$L1"); expect "$code" 404 "L1 after the bulk delete"
code=$(get "$BASE/api/clips/$L2"); expect "$code" 404 "L2 after the bulk delete"
code=$(get "$BASE/api/clips/$L3" -b "$JARLW"); expect "$code" 200 "a refused id survived the bulk delete"
code=$(get "$BASE/api/collections/$LIB_COL"); expect "$code" 200 "the collection after the deletes"
expect "$(json clip_count)" 1 "the deleted clips left the collection; C did not"
expect "$(json clips.0.id)" "$C" "C is still the collection's only member"

# The library page carries the bulk controls, inert until a client reveals them, and a checkbox only
# on rows the caller owns.
code=$(page "$BASE/" -b "$JARW"); expect "$code" 200 "the library page after the bulk actions"
has_file "$PAGE" 'data-bulk hidden' "the bulk bar is inert without JavaScript"
has_file "$PAGE" 'data-bulk-action="delete"' "the bulk bar offers delete"
lacks_file "$PAGE" 'action="/api/clips/bulk"' "the bulk bar is not a form that could post without a client"
lacks_file "$PAGE" 'onclick=' "the library page carries no inline handler"

# Everything this file made is gone: L1/L2 by the bulk delete above, L3 by its owner, the collection
# here. Step 17 has nothing of ours to clean, and the four extra users are pruned with the rest.
code=$(http DELETE "$BASE/api/clips/$L3" -b "$JARLW" || echo 000); echo "[e2e]   DELETE L3 $L3 -> $code"
code=$(http DELETE "$BASE/api/collections/$LIB_COL" -b "$JARW" || echo 000); echo "[e2e]   DELETE collection $LIB_COL -> $code"
code=$(http POST "$BASE/api/auth/logout" -b "$JARLW" || echo 000); echo "[e2e]   logout ${E2E_USER}_b -> $code"
rm -f "$JARL"
echo "[e2e]   bulk tag/ttl/collection/delete: 2 applied, 1 forbidden, 1 not found, every time"

# The run's record: this bundle's clips get their own rows in the closing summary table (see
# scripts/e2e.d/README.md), which lists the harness's own A-Y and would otherwise never name them.
summary_row L1 "${L1:-}" "library search (#8): the TITLE match for the run's needle; bulk-deleted by step 40"
summary_row L2 "${L2:-}" "library search: the DESCRIPTION match for the same needle; bulk-deleted by step 40"
summary_row L3 "${L3:-}" "the second user's clip: the foreign id the bulk matrix must refuse; deleted by its owner"
