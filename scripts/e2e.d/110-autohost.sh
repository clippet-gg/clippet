# 110-autohost.sh — the HOSTED Clippet Auto (cmd/clippet-auto, DESIGN §20 erratum 334) end to end, against one mock of
# Discord, the claim plane and a box's edge (DESIGN §20 erratum 347).
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 110.
#
# What this step proves on a live stack: the process the tree builds holds a gateway session, sees a video in a server
# whose Auto is on, opens that server's sealed Clippet Auto URL and POSTs the instruction to the box, and the BOX fetches
# the attachment from "Discord's CDN" itself, ingests it and answers the link the process posts — in Auto (the link, the
# notice, the original removed) and in Prompt (the offer, the poster's press, `Uploading…`, the link, the original and
# the reply removed); after op 7 it RESUMEs on the regional host it was given, ignores an event Discord delivers twice
# and keeps `last`; stopped and started, it reads its session back from the plane and RESUMEs instead of IDENTIFYing.
# Every Discord REST call it makes is recorded and asserted; its log carries no token, bearer, seal key or box URL.
#
# HOW, with nothing reaching Discord or the plane: a private docker network (`--internal`: no route out) of three
# throwaway containers of the APP's own image, none of them a compose service —
#   mock   scripts/e2e.d/110-autohost/ (Go, built here in the Dockerfile's `testdeps` stage — the harness's Go image —
#          with the checkout mounted read-only, as T0 builds; the product image never contains it). It answers under
#          the REAL names (network aliases discord.com, gateway.discord.gg, gateway-e2e.discord.gg, cdn.discordapp.com)
#          and two reserved `.test` names (plane.clippet.test, box.clippet.test), over TLS from a CA of its own;
#   box    `clippet serve` on a private DATA_DIR/SCRATCH_DIR and an fs store, DISCORD_BOT_TOKEN=managed (the kind of
#          instance Clippet Auto reaches), PUBLIC_BASE_URL https://box.clippet.test, its own account bound to both servers;
#   auto   clippet-auto from this tree with AUTO_TEST_ENDPOINTS=1 — the ONE test-only knob: a `.test` name may reach a
#          private address; refused unless the token is not a Discord token, the plane is a `.test` name and no
#          dead-man is set (the refusals are run first, below).
# The two clippet processes trust the mock's CA through SSL_CERT_FILE (Go's own variable) and NOTHING else changes in
# them: Discord's hosts, the gateway allowlist, the attachment allowlist, https and certificate checks are production's.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=AH_ buckets=-
# e2e-covers: cmd/clippet-auto/** internal/notify/autohost.go internal/notify/auto.go internal/autobot/** internal/autorule/**
# e2e-covers: internal/httpapi/autofetch.go
#   It registers no account on the stack and reads no shared clip: everything happens on its own network, in its own
#   three containers (this project's compose label and `clippet-e2e-` names, so step 1's stale-probe sweep removes them
#   after a kill; its trap removes them, the network, the build tag and its scratch files otherwise). It restarts and
#   reconfigures nothing of the stack, asserts nothing bucket-, log- or instance-wide (the box is its own instance), and
#   probes no limiter.
#
# Variables are prefixed AH_ so nothing here can clobber a harness global.

step 110 "the hosted Clippet Auto end to end: a mock gateway, REST, CDN, plane and box edge on a private network; Auto and Prompt reach the box and post its link, op 7 RESUMEs and keeps last, a restart RESUMEs from the plane, the knob refuses production"

AH_PORT=$(envfile CLIPPET_PUBLISH); AH_PORT=${AH_PORT##*:}; AH_PORT=${AH_PORT:-8481}
AH_NET="clippet-e2e-ah-$AH_PORT"
AH_MOCK="clippet-e2e-ah-mock-$AH_PORT"; AH_BOX="clippet-e2e-ah-box-$AH_PORT"; AH_AUTO="clippet-e2e-ah-auto-$AH_PORT"
AH_REFUSE="clippet-e2e-ah-refuse-$AH_PORT"
AH_DEPS="clippet-e2e-ah-deps:$AH_PORT"
AH_DIR="$E2E_DIR/autohost-$AH_PORT"
AH_TOKEN="e2e-autohost-token.not.discord-$(date +%s)"
AH_PSECRET="e2e-autohost-plane-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
AH_LABEL="com.docker.compose.project=${E2E_PROJECT:-}"
AH_GA=1430000000000000001; AH_GB=1430000000000000002
AH_CA_CH=1430000000000000011; AH_CB_CH=1430000000000000012
AH_MA=1430000000000000101; AH_MB=1430000000000000102; AH_REPLY=1430000000000000201
for AH_F in small-640x360 anamorphic-sar; do [ -s "testclips/$AH_F.mp4" ] || die "testclips/$AH_F.mp4 is missing (step 2)"; done

ah_stop() {
  # The record and the process's log (neither carries a secret: (g) and (h) assert it) stay for a post-mortem.
  [ ! -s "$AH_DIR/state.txt" ] || cp "$AH_DIR/state.txt" "$E2E_DIR/autohost-record.txt"
  [ ! -s "$AH_DIR/auto.log" ] || cp "$AH_DIR/auto.log" "$E2E_DIR/autohost-auto.log"
  docker_ rm -f "$AH_AUTO" "$AH_BOX" "$AH_MOCK" "$AH_REFUSE" >/dev/null 2>&1 || true
  docker_ network rm "$AH_NET" >/dev/null 2>&1 || true
  docker_ rmi "$AH_DEPS" >/dev/null 2>&1 || true
  rm -rf "$AH_DIR"
}
ah_stop
trap 'ah_stop' EXIT
mkdir -p "$AH_DIR"

AH_APPCT=$(dc ps -q app 2>/dev/null | tr -d '\r' | head -1)
[ -n "$AH_APPCT" ] || die "the app container is not running"
AH_IMG=$(docker_ inspect --format '{{.Config.Image}}' "$AH_APPCT" | tr -d '\r')
[ -n "$AH_IMG" ] || die "could not read the app container's image"

# --- (a) the two programs, built from this tree in the harness's Go image ------------------------------------------
AH_T=$SECONDS
docker_ build --target testdeps -t "$AH_DEPS" . > "$AH_DIR/deps.log" 2>&1 || die "docker build --target testdeps failed: $(tail -20 "$AH_DIR/deps.log")"
docker_ volume inspect clippet-gobuild >/dev/null 2>&1 || docker_ volume create clippet-gobuild >/dev/null
docker_ run --rm --network none -v "$(W "$PWD"):/src:ro" -v clippet-gobuild:/root/.cache/go-build -w /src -e CGO_ENABLED=0 "$AH_DEPS" \
  sh -c 'set -e; mkdir -p /tmp/b; go build -trimpath -o /tmp/b/clippet-auto ./cmd/clippet-auto >&2; go build -trimpath -o /tmp/b/mockdiscord ./scripts/e2e.d/110-autohost >&2; tar -C /tmp/b -cf - clippet-auto mockdiscord' \
  > "$AH_DIR/bin.tar" 2> "$AH_DIR/build.log" || die "building clippet-auto and the mock failed: $(tail -20 "$AH_DIR/build.log")"
echo "[e2e]   built clippet-auto and the mock from this tree in $((SECONDS - AH_T)) s"

# A container of the app's image running one of the two programs: created, given the binaries (and the CA), started.
ah_create() {   # ah_create <name> <entrypoint> [docker create args…] [-- <command…>]
  local name=$1 entry=$2; shift 2
  docker_ create --name "$name" --label "$AH_LABEL" --label com.docker.compose.oneoff=True --entrypoint "$entry" "$@" >/dev/null \
    || die "could not create $name"
  docker_ cp - "$name:/usr/local/bin" < "$AH_DIR/bin.tar" || die "could not copy the binaries into $name"
}

# --- (b) the knob refuses production: the built binary, no network ------------------------------------------------
AH_REAL="MTQzMDAwMDAwMDAwMDAwMDAyMQ.GaBcDe.$(printf 'x%.0s' $(seq 1 30))"   # base64 of a user id: Discord's shape
ah_refuse() {   # ah_refuse <want> <env…> — clippet-auto exits 2 with <want> on stderr
  local want=$1 rc; shift
  ah_create "$AH_REFUSE" /usr/local/bin/clippet-auto --network none -e AUTO_PLANE_SECRET=x -e 'AUTO_SEAL_PRIVATE={"kty":"EC"}' -e AUTO_TEST_ENDPOINTS=1 "$@" "$AH_IMG"
  docker_ start -a "$AH_REFUSE" > "$AH_DIR/refuse.txt" 2>&1 || true
  rc=$(docker_ inspect --format '{{.State.ExitCode}}' "$AH_REFUSE" | tr -d '\r')
  docker_ rm -f "$AH_REFUSE" >/dev/null 2>&1 || true
  expect "$rc" 2 "clippet-auto's exit with AUTO_TEST_ENDPOINTS=1 and $want"
  has_file_f "$AH_DIR/refuse.txt" "$want" "the refusal names its reason"
}
ah_refuse "AUTO_TEST_ENDPOINTS needs a DISCORD_AUTO_TOKEN that is not a Discord token (a test's mock gateway; never production)" \
  -e "DISCORD_AUTO_TOKEN=$AH_REAL" -e AUTO_PLANE_URL=https://plane.clippet.test/claim
ah_refuse "AUTO_TEST_ENDPOINTS needs an AUTO_PLANE_URL under the reserved .test domain (a test's mock plane; never production's)" \
  -e "DISCORD_AUTO_TOKEN=$AH_TOKEN" -e AUTO_PLANE_URL=https://bot.clippet.gg/claim
ah_refuse "AUTO_TEST_ENDPOINTS refuses AUTO_DEADMAN_URL: a test pings nothing real" \
  -e "DISCORD_AUTO_TOKEN=$AH_TOKEN" -e AUTO_PLANE_URL=https://plane.clippet.test/claim -e AUTO_DEADMAN_URL=https://hc-ping.example/x
ah_refuse "AUTO_TEST_ENDPOINTS must be 1 or unset" -e "DISCORD_AUTO_TOKEN=$AH_TOKEN" -e AUTO_PLANE_URL=https://plane.clippet.test/claim -e AUTO_TEST_ENDPOINTS=yes
echo "[e2e]   the knob refused a Discord-shaped token, production's plane, a dead-man and a value other than 1 (exit 2 each)"

# --- (c) the network and the mock ---------------------------------------------------------------------------------
docker_ network create --internal --label "clippet.e2e.project=${E2E_PROJECT:-}" "$AH_NET" >/dev/null || die "could not create $AH_NET"
ah_create "$AH_MOCK" /usr/local/bin/mockdiscord --network "$AH_NET" -u 0 \
  --network-alias discord.com --network-alias gateway.discord.gg --network-alias gateway-e2e.discord.gg \
  --network-alias cdn.discordapp.com --network-alias plane.clippet.test --network-alias box.clippet.test \
  -e "MOCK_TOKEN=$AH_TOKEN" -e "MOCK_PLANE_SECRET=$AH_PSECRET" -e "MOCK_BOX=$AH_BOX:8080" "$AH_IMG" serve
docker_ cp "$(W "$PWD/testclips/small-640x360.mp4")" "$AH_MOCK:/tmp/e2e-auto.mp4" || die "could not copy server A's video into the mock"
docker_ cp "$(W "$PWD/testclips/anamorphic-sar.mp4")" "$AH_MOCK:/tmp/e2e-press.mp4" || die "could not copy server B's video into the mock"
docker_ start "$AH_MOCK" >/dev/null || die "could not start the mock"
AH_N=0
until docker_ exec "$AH_MOCK" test -f /tmp/mock/ready 2>/dev/null; do
  AH_N=$((AH_N + 1)); [ "$AH_N" -lt 30 ] || die "the mock did not start: $(docker_ logs "$AH_MOCK" 2>&1 | tail -20)"
  sleep 1
done
docker_ cp "$AH_MOCK:/tmp/mock/ca.pem" "$(W "$AH_DIR/ca.pem")" >/dev/null || die "could not read the mock's CA"
docker_ cp "$AH_MOCK:/tmp/mock/seal.jwk" "$(W "$AH_DIR/seal.jwk")" >/dev/null || die "could not read the mock's seal key"

# --- (d) the box: its own instance, its own account bound to both servers, a Clippet Auto URL minted ---------------
ah_create "$AH_BOX" sh --network "$AH_NET" \
  -e DISCORD_APP_ID=987654321098765432 -e DISCORD_PUBLIC_KEY=abababababababababababababababababababababababababababababababab \
  -e DISCORD_BOT_TOKEN=managed -e PUBLIC_BASE_URL=https://box.clippet.test -e SSL_CERT_FILE=/tmp/ah-ca.pem \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e DATA_DIR=/tmp/ahbox -e SCRATCH_DIR=/tmp/ahbox-scratch -e STORAGE_BACKEND=fs -e STORAGE_DIR=/tmp/ahbox-media \
  "$AH_IMG" -c 'mkdir -p /tmp/ahbox /tmp/ahbox-scratch /tmp/ahbox-media && exec clippet serve >/tmp/boot.log 2>&1'
docker_ cp "$(W "$AH_DIR/ca.pem")" "$AH_BOX:/tmp/ah-ca.pem" || die "could not copy the CA into the box"
docker_ start "$AH_BOX" >/dev/null || die "could not start the box"
AH_N=0
until docker_ exec "$AH_BOX" sh -c 'grep -q "msg=listening" /tmp/boot.log' 2>/dev/null; do
  AH_N=$((AH_N + 1)); [ "$AH_N" -lt 60 ] || die "the box did not boot: $(docker_ exec "$AH_BOX" cat /tmp/boot.log 2>&1 | tail -20)"
  sleep 1
done
docker_ exec "$AH_BOX" sh -c 'printf "%s" "e2e-autohost-box-pw" | clippet admin users create ahowner >/dev/null && clippet admin bot bind '"$AH_GA"' ahowner >/dev/null && clippet admin bot bind '"$AH_GB"' ahowner >/dev/null' \
  || die "the box could not create its account and bind both servers"
AH_URL=$(docker_ exec "$AH_BOX" sh -c 'clippet admin tokens mint ahowner --scopes autofetch 2>/dev/null' | tr -d '\r')
case $AH_URL in https://box.clippet.test/in/auto/ca_*) ;; *) die "the box's mint is not a Clippet Auto URL: $(redact_ "$AH_URL")" ;; esac
AH_SECRET=${AH_URL#https://box.clippet.test/in/auto/}
printf '%s\n' "$AH_URL" > "$AH_DIR/box-url"
docker_ cp "$(W "$AH_DIR/box-url")" "$AH_MOCK:/tmp/mock/box-url" || die "could not hand the box's URL to the mock's plane"
echo "[e2e]   the mock, the box (two servers bound, a Clippet Auto URL minted) and the private network are up"

# --- (e) clippet-auto, with the knob ------------------------------------------------------------------------------
{ printf 'DISCORD_AUTO_TOKEN=%s\nAUTO_PLANE_URL=https://plane.clippet.test/claim\nAUTO_PLANE_SECRET=%s\n' "$AH_TOKEN" "$AH_PSECRET"
  printf 'AUTO_SEAL_PRIVATE=%s\nAUTO_TEST_ENDPOINTS=1\nSSL_CERT_FILE=/tmp/ah-ca.pem\n' "$(cat "$AH_DIR/seal.jwk")"; } > "$AH_DIR/auto.env"
chmod 600 "$AH_DIR/auto.env"
ah_create "$AH_AUTO" /usr/local/bin/clippet-auto --network "$AH_NET" --env-file "$(W "$AH_DIR/auto.env")" "$AH_IMG"
docker_ cp "$(W "$AH_DIR/ca.pem")" "$AH_AUTO:/tmp/ah-ca.pem" || die "could not copy the CA into clippet-auto"
docker_ start "$AH_AUTO" >/dev/null || die "could not start clippet-auto"

ah_wait() {   # ah_wait <phase> <seconds> <what>
  docker_ exec "$AH_MOCK" mockdiscord wait "$1" "$2" > "$AH_DIR/wait.txt" 2>&1 && return 0
  docker_ exec "$AH_MOCK" mockdiscord state > "$AH_DIR/state.txt" 2>&1 || true
  docker_ logs "$AH_AUTO" > "$AH_DIR/auto.log" 2>&1 || true
  die "$3 (phase $1 not reached in $2 s): $(cat "$AH_DIR/wait.txt")
--- the mock's record:
$(tail -40 "$AH_DIR/state.txt")
--- clippet-auto's log:
$(tail -20 "$AH_DIR/auto.log")
--- the box's log:
$(docker_ exec "$AH_BOX" tail -20 /tmp/boot.log 2>&1)"
}
AH_T=$SECONDS
ah_wait auto 200 "server A's video never became a link"
echo "[e2e]   Auto: the video became a link in $((SECONDS - AH_T)) s"
ah_wait press 200 "server B's press never became a link"
ah_wait saved 20 "the session was never saved after the press"
ah_wait replayed 60 "op 7 was never followed by a RESUME"
echo "[e2e]   Prompt: offered, pressed, linked; op 7 → RESUME on the regional host, the duplicate event ignored ($((SECONDS - AH_T)) s)"

# --- (f) a stop and a start: the session comes back from the plane --------------------------------------------------
docker_ stop -t 20 "$AH_AUTO" >/dev/null || die "could not stop clippet-auto"
expect "$(docker_ inspect --format '{{.State.ExitCode}}' "$AH_AUTO" | tr -d '\r')" 0 "clippet-auto's exit on SIGTERM"
docker_ start "$AH_AUTO" >/dev/null || die "could not start clippet-auto again"
ah_wait restarted 60 "the restarted process never RESUMEd"
docker_ exec "$AH_MOCK" mockdiscord state > "$AH_DIR/state.txt" 2>&1 || die "could not read the mock's record"
docker_ logs "$AH_AUTO" > "$AH_DIR/auto.log" 2>&1 || die "could not read clippet-auto's log"
AH_S="$AH_DIR/state.txt"

# --- (g) what the mock recorded -------------------------------------------------------------------------------------
ah_has() { has_file_f "$AH_S" "$1" "${2:-the record of the mock}"; }
ah_count() { grep -cF -- "$1" "$AH_S" || true; }
ah_lacks() { ! grep -qF -- "$1" "$AH_S" || die "$2: $(grep -F -- "$1" "$AH_S" | head -5)"; }
ah_lacks "phase FAILED" "the mock's script"
ah_lacks "auth=bad" "every REST call carries the bot token and every plane call the bearer"
ah_lacks "unknown=1" "the process called nothing the mock does not know"
ah_lacks "unknown-host" "the process reached no host but the six"
ah_lacks "kind=followup" "no failure line was said to the presser"
ah_has "plane GET /auto/guilds auth=ok"
ah_has "plane GET /auto/session auth=ok answer=404" "the first start found no session"
ah_has "rest GET /api/v10/gateway/bot auth=ok"
ah_has "gw conn=1 host=gateway.discord.gg query=v=10&encoding=json"
ah_has "identify conn=1 token=ok intents=33281" "IDENTIFY carries the token and the three intents"
ah_has "rest GET /api/v10/channels/$AH_CA_CH/messages auth=ok after=" "the fresh IDENTIFY's backfill read server A's channel"
ah_has "rest GET /api/v10/channels/$AH_CB_CH/messages auth=ok after=" "…and server B's"
expect "$(ah_count 'messages auth=ok after=')" 2 "the backfill reads text channels only (the category is skipped)"
AH_SIZE=$(wc -c < testclips/small-640x360.mp4 | tr -d ' ')
ah_has "box-in guild=$AH_GA url=https://cdn.discordapp.com/attachments/$AH_CA_CH/1430000000000000111/e2e-auto.mp4?ex=1&is=2&hm=3 filename=e2e-auto.mp4 type=video/mp4 size=$AH_SIZE ua=clippet-auto/1 auth=false" \
  "server A's instruction: the attachment as Discord named it, the clippet-auto/1 agent, no Authorization"
ah_has "box-in guild=$AH_GB url=https://cdn.discordapp.com/attachments/$AH_CB_CH/1430000000000000112/e2e-press.mp4?ex=1&is=2&hm=3" "server B's instruction"
expect "$(ah_count 'cdn GET /attachments/')" 2 "the box fetched each attachment from the CDN once"
expect "$(ah_count 'box POST /in/auto/ca_')" 2 "the process posted to the box twice (once per video; the duplicate event made no third)"
lacks_secret_f "$AH_S" "$AH_SECRET" "the box URL's secret never reaches the record"
AH_LINKA=$(sed -n "s|^box POST /in/auto/ca_[^ ]* status=201 link=||p" "$AH_S" | sed -n 1p)
AH_LINKB=$(sed -n "s|^box POST /in/auto/ca_[^ ]* status=201 link=||p" "$AH_S" | sed -n 2p)
case $AH_LINKA in https://box.clippet.test/?*) ;; *) die "the box's first answer is not a 201 with a link: $(grep '^box ' "$AH_S")" ;; esac
case $AH_LINKB in https://box.clippet.test/?*) ;; *) die "the box's second answer is not a 201 with a link: $(grep '^box ' "$AH_S")" ;; esac
[ "$AH_LINKA" != "$AH_LINKB" ] || die "the two videos became one clip: $AH_LINKA"
ah_has "rest POST /api/v10/channels/$AH_CA_CH/messages auth=ok kind=post content=\"$AH_LINKA\\n-# Videos posted here become Clippet links.\"" \
  "Auto posts the box's link with the notice"
ah_has "rest DELETE /api/v10/channels/$AH_CA_CH/messages/$AH_MA auth=ok" "Auto removes the original"
expect "$(ah_count "rest POST /api/v10/channels/$AH_CA_CH/messages ")" 1 "server A got ONE link (the replayed event was not acted on twice)"
ah_has "rest POST /api/v10/channels/$AH_CB_CH/messages auth=ok kind=offer custom_id=auto:go reply_to=$AH_MB" "Prompt offers the button as a reply"
ah_has "rest POST /api/v10/interactions/1430000000000000301/mock-itok-1/callback auth=none type=4 content=\"Uploading…\" flags=64" \
  "the press is answered privately, without the bot token"
ah_has "rest POST /api/v10/channels/$AH_CB_CH/messages auth=ok kind=post content=\"$AH_LINKB\"" "Prompt posts the link (server B's notice is off)"
ah_has "rest DELETE /api/v10/channels/$AH_CB_CH/messages/$AH_MB auth=ok" "Prompt removes the original"
ah_has "rest DELETE /api/v10/channels/$AH_CB_CH/messages/$AH_REPLY auth=ok" "…and its own reply"
grep -qE "^plane PUT /auto/session auth=ok session=mock-session-1 seq=[0-9]+ resume=wss://gateway-e2e\.discord\.gg last=$AH_MB user=1430000000000000021 app=1430000000000000022\$" "$AH_S" \
  || die "the act's save (the session, the regional resume host, last = the newest video, the identity) is not in the record: $(grep '^plane PUT' "$AH_S")"
ah_has "gw conn=2 host=gateway-e2e.discord.gg" "op 7: the process reconnects to the resume host READY named"
ah_has "resume conn=2 session=mock-session-1 seq=6 token=ok" "…and RESUMEs the session at the last sequence"
ah_has "seq=8 resume=wss://gateway-e2e.discord.gg last=$AH_MB " "the stop saved the latest sequence and kept last"
ah_has "plane GET /auto/session auth=ok answer=200" "the restart read the session back"
ah_has "gw conn=3 host=gateway-e2e.discord.gg" "the restart dialled the saved resume host"
ah_has "resume conn=3 session=mock-session-1 seq=8 token=ok" "…and RESUMEd at the saved sequence"
lacks_file_f "$AH_S" "identify conn=2" "op 7 never IDENTIFYs"
lacks_file_f "$AH_S" "identify conn=3" "a restart with a saved session never IDENTIFYs"
echo "[e2e]   the record: $(wc -l < "$AH_S" | tr -d ' ') lines — gateway, REST, CDN, plane and box calls as asserted"

# --- (h) the process's log: the knob said first, nothing secret --------------------------------------------------------
AH_L="$AH_DIR/auto.log"
head -1 "$AH_L" | grep -qF '"level":"WARN"' || die "clippet-auto's first line is not a WARN: $(head -1 "$AH_L")"
head -1 "$AH_L" | grep -qF "AUTO_TEST_ENDPOINTS=1: a name under .test may reach a private address. This process is a test's, never production's" \
  || die "clippet-auto's first line is not the knob's: $(head -1 "$AH_L")"
expect "$(grep -cF 'AUTO_TEST_ENDPOINTS=1: a name under .test' "$AH_L")" 2 "the knob's WARN, once per start"
has_file_f "$AH_L" "test_endpoints=true" "the start line says the knob is on"
has_file_f "$AH_L" '"msg":"clippet-auto stopped","event":"stop","outcome":"clean"' "SIGTERM is a clean stop"
for AH_X in "$AH_TOKEN" "$AH_PSECRET" "$AH_SECRET" "$(sed -n 's/.*"d":"\([^"]*\)".*/\1/p' "$AH_DIR/seal.jwk")" box.clippet.test/in/auto; do
  [ -n "$AH_X" ] || continue
  lacks_secret_f "$AH_L" "$AH_X" "clippet-auto's log"
done

# --- (i) the box holds both clips -------------------------------------------------------------------------------------
for AH_LINK in "$AH_LINKA" "$AH_LINKB"; do
  AH_CODE=$(docker_ exec "$AH_BOX" curl -s -o /dev/null -w '%{http_code}' "http://localhost:8080/${AH_LINK#https://box.clippet.test/}" | tr -d '\r')
  expect "$AH_CODE" 200 "the box serves the clip $AH_LINK"
done
echo "[e2e]   the box serves both clips; clippet-auto's log said the knob first and carries no token, bearer, seal key or box URL"

ah_stop
trap - EXIT
summary_row AH "-" "the hosted Clippet Auto (erratum 347): clippet-auto from this tree against one mock (Discord's REST, gateway, CDN; the plane; the box's edge) on an --internal network; the knob refuses a Discord token, production's plane, a dead-man; Auto and Prompt each reach the box, which fetched the attachment and answered the link posted; op 7 RESUMEs on the regional host, a duplicate event is ignored, last kept; a restart RESUMEs from the plane; no secret in its log"
