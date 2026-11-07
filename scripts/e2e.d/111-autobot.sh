# 111-autobot.sh — Clippet Auto ON THE BOX (`clippet serve` with DISCORD_AUTO_MODE, DESIGN §20 erratum 333) end to end,
# against bundle 110's mock of Discord (DESIGN §20 erratum 347, the on-box paragraph).
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 111.
#
# What this step proves on a live stack: an instance whose operator set DISCORD_AUTO_TOKEN holds a gateway session of
# its own, sees a video in a server bound with `admin bot bind`, fetches the attachment from "Discord's CDN" itself,
# ingests it as the bound account and answers in the channel — in Auto (the link with the notice, the original removed)
# and in Prompt with DISCORD_AUTO_NOTICE=off (the offer as a reply, the poster's press answered privately with
# `Uploading…`, the link with no notice, the original and the reply removed); after op 7 it RESUMEs on the regional
# host it was given, at the last sequence. It reads no channel back (the box has no backfill) and calls nothing the
# mock does not know. Every Discord REST call it makes is recorded and asserted; its log carries no token.
#
# HOW, with nothing reaching Discord: bundle 110's way — a private docker network (`--internal`: no route out) of two
# throwaway containers of the APP's own image, none of them a compose service —
#   mock   scripts/e2e.d/110-autohost/ (built here in the Dockerfile's `testdeps` stage with the checkout mounted
#          read-only) with MOCK_SCRIPT=box (no session save, no duplicate event, no restart: the box keeps its session
#          in memory and a restart IDENTIFYs afresh) and MOCK_MODES naming ONE server, server A, in the pass's mode;
#          under the REAL names (network aliases discord.com, gateway.discord.gg, gateway-e2e.discord.gg,
#          cdn.discordapp.com) over TLS from a CA of its own;
#   box    `clippet serve` on a private DATA_DIR/SCRATCH_DIR and an fs store, DISCORD_BOT_TOKEN=managed (Auto needs the
#          §30 bot configured: it acts in the servers that bot's `bind` bound and fetches through its fetcher), its own
#          account bound to server A BEFORE it serves (the mock posts the video right after READY), and a
#          DISCORD_AUTO_TOKEN that is a test's (the alphabet and length the box checks, no Discord user id in it).
# The box trusts the mock's CA through SSL_CERT_FILE and NOTHING else changes in it — no knob: the on-box Auto and its
# fetch have no private-address check (AUTO_TEST_ENDPOINTS is the hosted process's and is not set here). The mode is
# global on a box, so the step runs two passes, each with a fresh mock and a fresh box.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=AB_ buckets=-
# e2e-covers: internal/httpapi/autobox.go internal/autobot/** internal/config/discordauto.go internal/notify/auto.go
#   It registers no account on the stack and reads no shared clip: everything happens on its own network, in its own
#   containers (this project's compose label and `clippet-e2e-` names, so step 1's stale-probe sweep removes them after
#   a kill; its trap removes them, the network, the build tag and its scratch files otherwise). It restarts and
#   reconfigures nothing of the stack, asserts nothing bucket-, log- or instance-wide (the box is its own instance),
#   and probes no limiter.
#
# Variables are prefixed AB_ so nothing here can clobber a harness global.

step 111 "Clippet Auto on the box end to end: a mock gateway, REST and CDN on a private network; Auto posts the link with the notice and removes the original, Prompt (notice off) offers, takes the press and posts the link; op 7 RESUMEs"

AB_PORT=$(envfile CLIPPET_PUBLISH); AB_PORT=${AB_PORT##*:}; AB_PORT=${AB_PORT:-8481}
AB_NET="clippet-e2e-ab-$AB_PORT"
AB_MOCK="clippet-e2e-ab-mock-$AB_PORT"; AB_BOX="clippet-e2e-ab-box-$AB_PORT"
AB_DEPS="clippet-e2e-ab-deps:$AB_PORT"
AB_DIR="$E2E_DIR/autobot-$AB_PORT"
AB_TOKEN="e2e-autobot-token.not.discord-$(date +%s)"
AB_LABEL="com.docker.compose.project=${E2E_PROJECT:-}"
AB_G=1430000000000000001; AB_CH=1430000000000000011; AB_MSG=1430000000000000101; AB_REPLY=1430000000000000202
for AB_F in small-640x360 anamorphic-sar; do [ -s "testclips/$AB_F.mp4" ] || die "testclips/$AB_F.mp4 is missing (step 2)"; done

ab_down() {   # the pass's two containers
  docker_ rm -f "$AB_BOX" "$AB_MOCK" >/dev/null 2>&1 || true
}
ab_stop() {
  # The last pass's record and box log (neither carries a secret: asserted) stay for a post-mortem.
  [ ! -s "$AB_DIR/state.txt" ] || cp "$AB_DIR/state.txt" "$E2E_DIR/autobot-record.txt"
  [ ! -s "$AB_DIR/box.log" ] || cp "$AB_DIR/box.log" "$E2E_DIR/autobot-box.log"
  ab_down
  docker_ network rm "$AB_NET" >/dev/null 2>&1 || true
  docker_ rmi "$AB_DEPS" >/dev/null 2>&1 || true
  rm -rf "$AB_DIR"
}
ab_stop
trap 'ab_stop' EXIT
mkdir -p "$AB_DIR"

AB_APPCT=$(dc ps -q app 2>/dev/null | tr -d '\r' | head -1)
[ -n "$AB_APPCT" ] || die "the app container is not running"
AB_IMG=$(docker_ inspect --format '{{.Config.Image}}' "$AB_APPCT" | tr -d '\r')
[ -n "$AB_IMG" ] || die "could not read the app container's image"

# --- (a) the mock, built from this tree in the harness's Go image ----------------------------------------------------
AB_T=$SECONDS
docker_ build --target testdeps -t "$AB_DEPS" . > "$AB_DIR/deps.log" 2>&1 || die "docker build --target testdeps failed: $(tail -20 "$AB_DIR/deps.log")"
docker_ volume inspect clippet-gobuild >/dev/null 2>&1 || docker_ volume create clippet-gobuild >/dev/null
docker_ run --rm --network none -v "$(W "$PWD"):/src:ro" -v clippet-gobuild:/root/.cache/go-build -w /src -e CGO_ENABLED=0 "$AB_DEPS" \
  sh -c 'set -e; mkdir -p /tmp/b; go build -trimpath -o /tmp/b/mockdiscord ./scripts/e2e.d/110-autohost >&2; tar -C /tmp/b -cf - mockdiscord' \
  > "$AB_DIR/bin.tar" 2> "$AB_DIR/build.log" || die "building the mock failed: $(tail -20 "$AB_DIR/build.log")"
echo "[e2e]   built the mock from this tree in $((SECONDS - AB_T)) s"
docker_ network create --internal --label "clippet.e2e.project=${E2E_PROJECT:-}" "$AB_NET" >/dev/null || die "could not create $AB_NET"

ab_wait() {   # ab_wait <phase> <seconds> <what>
  docker_ exec "$AB_MOCK" mockdiscord wait "$1" "$2" > "$AB_DIR/wait.txt" 2>&1 && return 0
  docker_ exec "$AB_MOCK" mockdiscord state > "$AB_DIR/state.txt" 2>&1 || true
  docker_ exec "$AB_BOX" cat /tmp/boot.log > "$AB_DIR/box.log" 2>&1 || true
  die "$3 (phase $1 not reached in $2 s): $(cat "$AB_DIR/wait.txt")
--- the mock's record:
$(tail -40 "$AB_DIR/state.txt")
--- the box's log:
$(tail -20 "$AB_DIR/box.log")"
}

# ab_pass <mode> <notice> <clip> — one fresh mock (server A in <mode>) and one fresh box (DISCORD_AUTO_MODE=<mode>,
# DISCORD_AUTO_NOTICE=<notice>), run until the box has RESUMEd after op 7; the record lands in $AB_DIR/state.txt and
# the box's log in $AB_DIR/box.log.
ab_pass() {
  local mode=$1 notice=$2 clip=$3 phase n
  AB_MODE=$mode; AB_NOTICE=$notice; AB_EVENT=auto; [ "$mode" = auto ] || AB_EVENT=press
  ab_down
  rm -f "$AB_DIR/state.txt" "$AB_DIR/box.log"
  docker_ create --name "$AB_MOCK" --label "$AB_LABEL" --label com.docker.compose.oneoff=True --entrypoint /usr/local/bin/mockdiscord \
    --network "$AB_NET" -u 0 \
    --network-alias discord.com --network-alias gateway.discord.gg --network-alias gateway-e2e.discord.gg \
    --network-alias cdn.discordapp.com \
    -e "MOCK_TOKEN=$AB_TOKEN" -e MOCK_SCRIPT=box -e "MOCK_MODES=$mode" "$AB_IMG" serve >/dev/null || die "could not create $AB_MOCK"
  docker_ cp - "$AB_MOCK:/usr/local/bin" < "$AB_DIR/bin.tar" || die "could not copy the mock into $AB_MOCK"
  docker_ cp "$(W "$PWD/testclips/$clip.mp4")" "$AB_MOCK:/tmp/e2e-auto.mp4" || die "could not copy server A's video into the mock"
  docker_ start "$AB_MOCK" >/dev/null || die "could not start the mock"
  n=0
  until docker_ exec "$AB_MOCK" test -f /tmp/mock/ready 2>/dev/null; do
    n=$((n + 1)); [ "$n" -lt 30 ] || die "the mock did not start: $(docker_ logs "$AB_MOCK" 2>&1 | tail -20)"
    sleep 1
  done
  docker_ cp "$AB_MOCK:/tmp/mock/ca.pem" "$(W "$AB_DIR/ca.pem")" >/dev/null || die "could not read the mock's CA"

  # The box: migrated, its account made and bound to server A, THEN serving (the gateway starts with serve).
  docker_ create --name "$AB_BOX" --label "$AB_LABEL" --label com.docker.compose.oneoff=True --entrypoint sh --network "$AB_NET" \
    -e DISCORD_APP_ID=987654321098765432 -e DISCORD_PUBLIC_KEY=abababababababababababababababababababababababababababababababab \
    -e DISCORD_BOT_TOKEN=managed -e PUBLIC_BASE_URL=https://box.clippet.test -e SSL_CERT_FILE=/tmp/ab-ca.pem \
    -e "DISCORD_AUTO_MODE=$mode" -e "DISCORD_AUTO_TOKEN=$AB_TOKEN" -e "DISCORD_AUTO_NOTICE=$notice" \
    -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
    -e DATA_DIR=/tmp/abbox -e SCRATCH_DIR=/tmp/abbox-scratch -e STORAGE_BACKEND=fs -e STORAGE_DIR=/tmp/abbox-media \
    "$AB_IMG" -c 'mkdir -p /tmp/abbox /tmp/abbox-scratch /tmp/abbox-media && clippet migrate >/tmp/boot.log 2>&1 &&
      printf "%s" "e2e-autobot-box-pw" | clippet admin users create abowner >>/tmp/boot.log 2>&1 &&
      clippet admin bot bind '"$AB_G"' abowner >>/tmp/boot.log 2>&1 && exec clippet serve >>/tmp/boot.log 2>&1' >/dev/null \
    || die "could not create $AB_BOX"
  docker_ cp "$(W "$AB_DIR/ca.pem")" "$AB_BOX:/tmp/ab-ca.pem" || die "could not copy the CA into the box"
  docker_ start "$AB_BOX" >/dev/null || die "could not start the box"

  phase=auto; [ "$mode" = auto ] || phase=press
  AB_T=$SECONDS
  ab_wait "$phase" 200 "the box's $mode pass never made a link"
  ab_wait resumed 60 "op 7 was never followed by a RESUME"
  echo "[e2e]   $mode (notice $notice): the video became a link and op 7 RESUMEd in $((SECONDS - AB_T)) s"
  docker_ exec "$AB_MOCK" mockdiscord state > "$AB_DIR/state.txt" 2>&1 || die "could not read the mock's record"
  docker_ exec "$AB_BOX" cat /tmp/boot.log > "$AB_DIR/box.log" 2>&1 || die "could not read the box's log"
}

AB_S="$AB_DIR/state.txt"
ab_has() { has_file_f "$AB_S" "$1" "${2:-the record of the mock}"; }
ab_count() { grep -cF -- "$1" "$AB_S" || true; }
ab_lacks() { ! grep -qF -- "$1" "$AB_S" || die "$2: $(grep -F -- "$1" "$AB_S" | head -5)"; }
# ab_common <resume seq> — what both passes share: the session, the fetch, op 7, nothing unknown, the log; sets AB_LINK.
ab_common() {
  ab_lacks "phase FAILED" "the mock's script"
  ab_lacks "auth=bad" "every REST call carries the box's Auto token"
  ab_lacks "unknown=1" "the box called nothing the mock does not know"
  ab_lacks "unknown-host" "the box reached no host but the mock's four"
  ab_lacks "kind=followup" "no failure line was said"
  ab_lacks "plane " "the box has no plane"
  ab_has "rest GET /api/v10/gateway/bot auth=ok"
  ab_has "gw conn=1 host=gateway.discord.gg query=v=10&encoding=json"
  ab_has "identify conn=1 token=ok intents=33281" "IDENTIFY carries the box's token and the three intents"
  expect "$(ab_count 'messages auth=ok after=')" 0 "the box reads no channel back (no backfill on a box)"
  expect "$(ab_count "cdn GET /attachments/$AB_CH/1430000000000000111/e2e-auto.mp4")" 1 "the box fetched the attachment from the CDN once"
  AB_LINK=$(sed -n "s|^rest POST /api/v10/channels/$AB_CH/messages auth=ok kind=post content=\"\(https://box\.clippet\.test/[^\"\\\\]*\).*|\1|p" "$AB_S" | sed -n 1p)
  case $AB_LINK in https://box.clippet.test/?*) ;; *) die "the box posted no link of its own: $(grep '^rest POST' "$AB_S")" ;; esac
  expect "$(ab_count "rest POST /api/v10/channels/$AB_CH/messages auth=ok kind=post")" 1 "ONE link posted"
  ab_has "rest DELETE /api/v10/channels/$AB_CH/messages/$AB_MSG auth=ok" "the original removed"
  ab_has "gw conn=2 host=gateway-e2e.discord.gg" "op 7: the box reconnects to the resume host READY named"
  ab_has "resume conn=2 session=mock-session-1 seq=$1 token=ok" "…and RESUMEs the session at the last sequence"
  lacks_file_f "$AB_S" "identify conn=2" "op 7 never IDENTIFYs"
  has_file_f "$AB_DIR/box.log" "msg=\"discord auto configured\" auto=\"$AB_MODE, 10 s, notice $AB_NOTICE, token (redacted)\"" \
    "the box's boot line names Clippet Auto's mode and notice, the token redacted"
  has_file_f "$AB_DIR/box.log" "msg=\"discord auto: link\" guild_id=$AB_G event=$AB_EVENT outcome=linked" "the box's log says the link"
  has_file_f "$AB_DIR/box.log" 'msg="discord auto: resumed" outcome=resumed' "the box's log says the RESUME"
  lacks_secret_f "$AB_DIR/box.log" "$AB_TOKEN" "the box's log"
  AB_CODE=$(docker_ exec "$AB_BOX" curl -s -o /dev/null -w '%{http_code}' "http://localhost:8080/${AB_LINK#https://box.clippet.test/}" | tr -d '\r')
  expect "$AB_CODE" 200 "the box serves the clip $AB_LINK"
}

# --- (b) Auto, the notice on: READY 1, GUILD_CREATE 2, the video 3 → RESUME at 3 ---------------------------------------
ab_pass auto on small-640x360
ab_common 3
ab_has "rest POST /api/v10/channels/$AB_CH/messages auth=ok kind=post content=\"$AB_LINK\\n-# Videos posted here become Clippet links.\"" \
  "Auto posts the link with the notice"
ab_lacks "kind=offer" "Auto offers nothing"
ab_lacks "/callback" "Auto answers no interaction"
echo "[e2e]   Auto: the record is $(wc -l < "$AB_S" | tr -d ' ') lines — the link with the notice, the original removed, RESUME at 3"

# --- (c) Prompt, the notice off: …, the video 3, the press 4 → RESUME at 4 ---------------------------------------------
ab_pass prompt off anamorphic-sar
ab_common 4
ab_has "rest POST /api/v10/channels/$AB_CH/messages auth=ok kind=offer custom_id=auto:go reply_to=$AB_MSG" "Prompt offers the button as a reply"
ab_has "rest POST /api/v10/interactions/1430000000000000301/mock-itok-1/callback auth=none type=4 content=\"Uploading…\" flags=64" \
  "the press is answered privately, without the bot token"
ab_has "rest POST /api/v10/channels/$AB_CH/messages auth=ok kind=post content=\"$AB_LINK\"" "Prompt posts the link alone (DISCORD_AUTO_NOTICE=off)"
ab_has "rest DELETE /api/v10/channels/$AB_CH/messages/$AB_REPLY auth=ok" "…and removes its own reply"
has_file_f "$AB_DIR/box.log" "msg=\"discord auto: offer\" guild_id=$AB_G event=offer outcome=sent" "the box's log says the offer"
echo "[e2e]   Prompt: the record is $(wc -l < "$AB_S" | tr -d ' ') lines — offered, pressed, the link alone, the original and the reply removed, RESUME at 4"

ab_stop
trap - EXIT
summary_row AB "-" "Clippet Auto on the box (erratum 333; the mock of erratum 347, no knob): clippet serve with DISCORD_AUTO_TOKEN on an --internal network; Auto posts the box's link with the notice and removes the original, Prompt with the notice off offers, takes the press privately, posts the link and removes both; the box fetched each attachment from the CDN itself, read no channel back, RESUMEd after op 7; no token in its log"
