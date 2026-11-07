# 101-autofetch.sh — the Clippet Auto URL: POST /in/auto/{token} makes the box fetch one Discord attachment (DESIGN §20 erratum 324).
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 101.
#
# Clippet Auto's one press sends this instance a small JSON instruction naming a Discord attachment, and the
# instance fetches the video ITSELF through the fetcher `/upload` uses. The credential is erratum 322's
# machinery with its own `ca_` shape and `autofetch` scope. What this step proves on a live stack: the shell
# mints it (the URL on stdout, on this instance's own base); every miss — a random, a malformed, the other
# route's secret on either route, a revoked one — answers exactly what a path with no route answers; the
# refusals that come before any fetch (not JSON, the grammar, a URL that is not a Discord attachment, a server
# not connected to the account) answer in that order whether or not this stack runs a bot; the secret is no
# Bearer credential; the log writes /in/{token}. Then a PROBE proves the path reaches the fetcher.
#
# WHY A PROBE, and why --network none. A successful fetch would dial Discord's CDN, and the suite reaches
# Discord for nothing (85-bot's header). The fetch → ingest → link path with a SUCCESSFUL fetch is pinned in
# Go against an httptest CDN (internal/httpapi TestAutoFetchWholePath); live, it is the owner's one-press test.
# So a throwaway container of the app's own image (no compose service, none of its volumes; a private DATA_DIR,
# SCRATCH_DIR and fs STORAGE_DIR), booted with NO network and DISCORD_BOT_TOKEN=managed (the official bot's
# instance, the only kind Clippet Auto reaches), its own account bound to a server: an on-CDN attachment for
# that server is attempted and fails as `fetch` (the probe has no network) — one `autofetch failed` line,
# outcome=fetch — while a not-a-video instruction starts no job. Its log carries no secret, guild id, URL or
# file name. It uses only `sh`, `curl` and `clippet` inside the container: all three are in the default
# (alpine) image and in the GPU (Ubuntu) image alike.
#
# e2e-schedule: parallel
# e2e-namespace: account=af prefix=AF_ buckets=-
# e2e-covers: internal/httpapi/autofetch.go internal/httpapi/inbound.go internal/autorule/**
#   It creates its OWN account (`admin users create`, under the run's sweep pattern), mints for it, uploads
#   nothing on this stack (every request here is refused before a fetch), and removes the account again. The
#   probe carries this project's compose label and a `clippet-e2e-` name, so step 1's stale-probe sweep removes
#   it after a kill; its trap removes it otherwise. It reads no shared clip, restarts nothing, asserts nothing
#   bucket-wide or instance-wide (the log check greps for THIS run's own secret only), and probes no limiter.
#
# Variables are prefixed AF_ so nothing here can clobber a harness global.

step 101 "the Clippet Auto URL (one press): POST /in/auto/{token} is minted from the shell, a miss is a missing route on both /in/ routes, refusals come before any fetch, a probe reaches the fetcher, and nothing secret is logged"

AF_USER="e2e_$(date +%s)_af"
AF_PASS="e2e-autofetch-$(date +%s)"
AF_ERR="$E2E_DIR/autofetch-stderr.txt"
AF_NOPE="$E2E_DIR/autofetch-nope.json"
# A server id nobody has connected on this stack: digits only, 18 or 19 of them.
AF_G="77$(date +%s)$(printf '%06d' "$$")"
AF_CDN="https://cdn.discordapp.com/attachments/111111111111111111/222222222222222222/onepress-vid.mp4?ex=1&is=2&hm=3"

af_body() {   # af_body <guild> <url> [<filename> <content type>] -> one instruction
  printf '{"guild_id":"%s","url":"%s","filename":"%s","content_type":"%s","size":4096}' \
    "$1" "$2" "${3:-onepress-vid.mp4}" "${4:-video/mp4}"
}

# The account this step owns. The password goes in on STDIN, never argv (§20 erratum 109).
AF_OUT=$(printf '%s' "$AF_PASS" | dc exec -T app clippet admin users create "$AF_USER" 2>&1) \
  || die "admin users create failed: $AF_OUT"
echo "[e2e]   $AF_OUT"

# --- (a) the mint: stdout is the URL, on this instance's own base; stderr never carries the secret -------
AF_TOKURL=$(dc exec -T app clippet admin tokens mint "$AF_USER" --scopes autofetch --name e2e 2>"$AF_ERR" | tr -d '\r') \
  || die "admin tokens mint --scopes autofetch failed: $(cat "$AF_ERR")"
case $AF_TOKURL in "$BASE/in/auto/ca_"*) ;; *) die "the mint's stdout is not a Clippet Auto URL on $BASE: $(redact_ "$AF_TOKURL") $(cat "$AF_ERR")" ;; esac
AF_RAW=${AF_TOKURL#"$BASE/in/auto/"}
expect "${#AF_RAW}" 46 "the Clippet Auto secret's length"
lacks_secret_f "$AF_ERR" "$AF_RAW" "the secret must never reach stderr"
has_file_f "$AF_ERR" "ca_$(printf '%s' "$AF_RAW" | cut -c4-11)" "stderr must name the row by its ca_ display prefix"
has_file_f "$AF_ERR" "scopes=autofetch" "the row holds the autofetch scope alone"
echo "[e2e]   minted a Clippet Auto URL for $AF_USER from the shell"

# --- (b) every miss is a missing route, on BOTH /in/ routes -----------------------------------------------
code=$(http_refused POST "$BASE/api/nope" -H 'Content-Type: application/json' --data-binary '{"x":1}'); cp "$RESP" "$AF_NOPE"
expect "$code" 404 "POST /api/nope, the answer every miss must equal"
AF_NOPE_CT=$(hdr Content-Type)
af_miss() {   # af_miss <path after /in/> <what>
  local code
  code=$(http_refused POST "$BASE/in/$1" -H 'Content-Type: application/json' --data-binary '{"x":1}')
  expect "$code" 404 "$2: the status of a missing route"
  cmp -s "$RESP" "$AF_NOPE" || die "$2: the body differs from POST /api/nope's: $(cat "$RESP")"
  expect "$(hdr Content-Type)" "$AF_NOPE_CT" "$2: the Content-Type of a missing route"
}
AF_RAND="ca_$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n' | cut -c1-43)"
expect "${#AF_RAND}" 46 "the random probe's length"
af_miss "auto/$AF_RAND" "a random ca_ value"
af_miss "auto/ca_short" "a value of the wrong shape"
af_miss "$AF_RAW" "the live Clippet Auto secret on the inbound route"
AF_CITOK=$(dc exec -T app clippet admin tokens mint "$AF_USER" --scopes inbound --name 'e2e ci' 2>/dev/null | tr -d '\r') \
  || die "admin tokens mint --scopes inbound (for the probe) failed"
case $AF_CITOK in "$BASE/in/ci_"*) ;; *) die "the inbound URL for the probe is not one: $(redact_ "$AF_CITOK")" ;; esac
af_miss "auto/${AF_CITOK#"$BASE/in/"}" "a live inbound secret on the Clippet Auto route"
# GET answers the route's one 405 whatever the value: no token oracle.
code=$(get "$AF_TOKURL"); expect "$code" 405 "GET on the live Clippet Auto URL"
code=$(get "$BASE/in/auto/$AF_RAND"); expect "$code" 405 "GET on a random Clippet Auto URL"

# --- (c) the refusals before any fetch, in order ----------------------------------------------------------
# Every one is a REFUSAL with no side effect, so http_refused may carry it; none can reach a fetcher.
code=$(http_refused POST "$AF_TOKURL" -H 'Content-Type: text/plain' --data-binary "$(af_body "$AF_G" "$AF_CDN")")
expect "$code" 415 "a body that is not JSON ($(cat "$RESP"))"
expect "$(json error)" "unsupported_media_type" "the not-JSON code"
code=$(http_refused POST "$AF_TOKURL" -H 'Content-Type: application/json' --data-binary "$(af_body x "https://example.com/x")")
expect "$code" 400 "a grammar refusal ($(cat "$RESP"))"
expect "$(json message)" "instruction must be JSON: guild_id, url, filename, content_type, size" "the grammar sentence"
for AF_BAD in "https://example.com/attachments/111111111111111111/222222222222222222/a.mp4" \
              "https://cdn.discordapp.com/avatars/111111111111111111/a.png" \
              "http://cdn.discordapp.com/attachments/111111111111111111/222222222222222222/a.mp4"; do
  code=$(http_refused POST "$AF_TOKURL" -H 'Content-Type: application/json' --data-binary "$(af_body "$AF_G" "$AF_BAD")")
  expect "$code" 400 "not a Discord attachment: $AF_BAD ($(cat "$RESP"))"
  expect "$(json error)" "attachment_host" "the not-an-attachment code"
  expect "$(json message)" "url is not a Discord attachment" "the not-an-attachment sentence"
done
code=$(http_refused POST "$AF_TOKURL" -H 'Content-Type: application/json' --data-binary "$(af_body "$AF_G" "$AF_CDN")")
expect "$code" 403 "a server not connected to this account ($(cat "$RESP"))"
expect "$(json error)" "not_bound" "the not-connected code"
expect "$(json message)" "that server is not connected to this account" "the not-connected sentence"
echo "[e2e]   not JSON 415, the grammar 400, three non-attachments 400 attachment_host, an unconnected server 403 not_bound"

# --- (d) the secret is no Bearer credential ----------------------------------------------------------------
code=$(get "$BASE/api/auth/me" -H "Authorization: Bearer $AF_RAW")
expect "$code" 401 "the Clippet Auto secret as a Bearer token"

# --- (e) the probe: the path reaches the fetcher ------------------------------------------------------------
AF_APPCT=$(dc ps -q app 2>/dev/null | tr -d '\r' | head -1)
[ -n "$AF_APPCT" ] || die "the app container is not running"
AF_IMG=$(docker_ inspect --format '{{.Config.Image}}' "$AF_APPCT" | tr -d '\r')
[ -n "$AF_IMG" ] || die "could not read the app container's image"
AF_PORT=$(envfile CLIPPET_PUBLISH); AF_PORT=${AF_PORT##*:}; AF_PORT=${AF_PORT:-8481}
AF_CT="clippet-e2e-autofetch-$AF_PORT"
AF_PG=888888888888888801
af_stop() { docker_ rm -f "$AF_CT" >/dev/null 2>&1 || true; }
af_stop
trap 'af_stop' EXIT
# Every variable the probe reads is pinned here: none comes from this stack's .env.
docker_ run -d --name "$AF_CT" --network none --label "com.docker.compose.project=${E2E_PROJECT:-}" --label com.docker.compose.oneoff=True \
  -e DISCORD_APP_ID=987654321098765432 -e DISCORD_PUBLIC_KEY=abababababababababababababababababababababababababababababababab \
  -e DISCORD_BOT_TOKEN=managed -e PUBLIC_BASE_URL=https://af-box.example \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e DATA_DIR=/tmp/afprobe -e SCRATCH_DIR=/tmp/afprobe-scratch -e STORAGE_BACKEND=fs -e STORAGE_DIR=/tmp/afprobe-media \
  --entrypoint sh "$AF_IMG" -c 'mkdir -p /tmp/afprobe /tmp/afprobe-scratch /tmp/afprobe-media && exec clippet serve >/tmp/boot.log 2>&1' \
  >/dev/null || die "could not start the autofetch probe (if the image cannot boot without a network, this leg must not run: it would otherwise reach Discord)"
AF_N=0
until docker_ exec "$AF_CT" sh -c 'grep -q "msg=listening" /tmp/boot.log' 2>/dev/null; do
  AF_N=$((AF_N + 1)); [ "$AF_N" -lt 60 ] || die "the autofetch probe did not boot: $(docker_ exec "$AF_CT" cat /tmp/boot.log 2>&1)"
  sleep 1
done
docker_ exec "$AF_CT" sh -c 'printf "%s" "e2e-autofetch-probe-pw" | clippet admin users create afowner >/dev/null && clippet admin bot bind '"$AF_PG"' afowner >/dev/null' \
  || die "the autofetch probe could not create and bind its account"
AF_PTOKURL=$(docker_ exec "$AF_CT" sh -c 'clippet admin tokens mint afowner --scopes autofetch 2>/dev/null' | tr -d '\r')
case $AF_PTOKURL in https://af-box.example/in/auto/ca_*) ;; *) die "the probe's mint is not a Clippet Auto URL: $(redact_ "$AF_PTOKURL")" ;; esac
AF_PRAW=${AF_PTOKURL#https://af-box.example/in/auto/}
af_ppost() {   # af_ppost <label> <body> — post inside the probe, one labelled line
  docker_ exec -e BODY="$2" -e RAW="$AF_PRAW" "$AF_CT" sh -c '
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -X POST -H "Content-Type: application/json" --data-binary "$BODY" "http://localhost:8080/in/auto/$RAW")
    printf "%s status=%s body=%s\n" "'"$1"'" "$code" "$(tr -d "\n" < /tmp/body)"'
}
af_pfails() { docker_ exec "$AF_CT" sh -c 'grep -c "discord bot: autofetch failed" /tmp/boot.log || true' | tr -d '\r'; }
AF_P1=$(af_ppost notvideo "$(af_body "$AF_PG" "$AF_CDN" onepress-vid.png image/png)")
AF_P2=$(af_ppost unbound "$(af_body 888888888888888802 "$AF_CDN")")
AF_P0=$(af_pfails)
AF_P3=$(af_ppost fetch "$(af_body "$AF_PG" "$AF_CDN")")
AF_N=0
until [ "$(af_pfails)" = 1 ]; do
  AF_N=$((AF_N + 1)); [ "$AF_N" -lt 30 ] || die "the probe's job never ended: $(docker_ exec "$AF_CT" cat /tmp/boot.log 2>&1)"
  sleep 1
done
AF_PLOG=$(docker_ exec "$AF_CT" cat /tmp/boot.log 2>&1)
af_stop
trap - EXIT
printf '%s\n' "$AF_P1" "$AF_P2" "$AF_P3" | sed -n 's/^/[e2e]   probe: /p'
contains "$AF_P1" 'status=415' || die "a not-a-video instruction in the probe was not 415: $AF_P1"
contains "$AF_P1" '"error":"not_video"' || die "the not-a-video code: $AF_P1"
contains "$AF_P2" 'status=403' || die "an unbound server in the probe was not 403: $AF_P2"
[ "$AF_P0" = 0 ] || die "a refused instruction started a job in the probe ($AF_P0)"
contains "$AF_P3" 'status=502' || die "the probe's on-CDN instruction did not reach the fetcher and fail as fetch: $AF_P3"
contains "$AF_P3" '"error":"fetch"' || die "the fetch code: $AF_P3"
contains "$AF_P3" '"message":"the attachment could not be fetched from Discord"' || die "the fetch sentence: $AF_P3"
contains "$AF_PLOG" 'outcome=fetch' || die "the probe's job line carries no outcome=fetch: $AF_PLOG"
contains "$AF_PLOG" 'autofetch=' || die "the probe's lines carry no autofetch=<row id>: $AF_PLOG"
contains "$AF_PLOG" '/in/{token}' || die "the probe's access lines do not write /in/{token}"
for AF_SECRET in "$(printf '%s' "$AF_PRAW" | cut -c4-)" "$AF_PG" 888888888888888802 onepress-vid attachments/111111111111111111; do
  contains "$AF_PLOG" "$AF_SECRET" && die "the probe's log carries $(redact_ "$AF_SECRET")"
done
echo "[e2e]   probe: not a video 415 and an unbound server 403 with no job; an on-CDN attachment reached the fetcher and failed as fetch (no network); its log has no secret, guild, URL or file name"

# --- (f) the stack's log writes /in/{token}, never the secret ----------------------------------------------
AF_LOG="$E2E_DIR/autofetch-app.log"
dc logs --no-color --since "$START_TS" app > "$AF_LOG" 2>&1 || die "docker compose logs failed"
lacks_secret_f "$AF_LOG" "$(printf '%s' "$AF_RAW" | cut -c4-)" "the Clippet Auto secret must never reach the app log"
lacks_file_f "$AF_LOG" "$AF_G" "the instruction's server id must never reach the app log"
has_file_f "$AF_LOG" "/in/{token}" "the app log writes the path redacted"
rm -f "$AF_LOG"

# --- (g) revoke: the URL is a missing route at once ---------------------------------------------------------
AF_LS=$(dc exec -T app clippet admin tokens list "$AF_USER" | tr -d '\r') || die "admin tokens list failed: $AF_LS"
contains "$AF_LS" "ca_$(printf '%s' "$AF_RAW" | cut -c4-11)" || die "tokens list does not show the ca_ row: $AF_LS"
case $AF_LS in *"$AF_RAW"*) die "admin tokens list printed the secret" ;; esac
AF_TID=$(printf '%s\n' "$AF_LS" | grep -F "ca_$(printf '%s' "$AF_RAW" | cut -c4-11)" | awk '{print $1}' | head -1)
expect_nonempty "$AF_TID" "the Clippet Auto row's id"
AF_OUT=$(dc exec -T app clippet admin tokens revoke "$AF_USER" "$AF_TID" | tr -d '\r') || die "admin tokens revoke failed: $AF_OUT"
contains "$AF_OUT" "revoked 1 token(s) for $AF_USER" || die "tokens revoke: $AF_OUT"
af_miss "auto/$AF_RAW" "the revoked Clippet Auto URL"

# --- the litter this step made, gone (the tokens go with the account) --------------------------------------
AF_OUT=$(dc exec -T app clippet admin users prune "$AF_USER" --older-than 1s --no-clips | tr -d '\r') \
  || die "admin users prune failed: $AF_OUT"
contains "$AF_OUT" 'pruned 1 user(s)' || die "users prune did not remove the account: $AF_OUT"
rm -f "$AF_ERR" "$AF_NOPE"

summary_row AF "-" "the Clippet Auto URL (one press): minted from the shell; random, malformed, cross-route and revoked values answer POST /api/nope's 404; not JSON 415, grammar 400, non-attachments 400, unconnected server 403 before any fetch; no Bearer; a --network none probe reaches the fetcher (502 fetch) and logs no secret, guild, URL or file name; account pruned"
