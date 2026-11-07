# 98-form.sh — the upload form on `/upload` (DESIGN.md §20 erratum 320). Step 98. Sourced by scripts/e2e.sh (see
# scripts/e2e.d/README.md). Every name here is FM_-prefixed.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=FM_ buckets=-
# e2e-covers: internal/httpapi/discord_form.go internal/httpapi/discord.go internal/notify/bot.go
# e2e-covers: internal/notify/reply.go
#   It touches nothing of this stack: ONE throwaway container of the app's own image (then a second, for the
#   `off` leg), started with `docker run --network none` — no compose service, so none of the service's
#   volumes is mounted (no shared DATA_DIR or /scratch to protect), and its own DATA_DIR, SCRATCH_DIR and an
#   fs STORAGE_DIR under /tmp inside it. It creates no clip and no user on this stack, runs no `dc up`, and
#   asserts nothing bucket-, log- or instance-wide. The container carries this project's compose label and a
#   `clippet-e2e-` name, so step 1's stale-probe sweep removes it after a kill; its trap removes it otherwise.
#   It also carries `com.docker.compose.oneoff=True`, the label `dc run` stamps on every probe (read off one,
#   lane harness9), so compose counts it as a one-off of the project and never as an orphan of it. (Compose
#   v2.40 printed no orphan warning for the label-less shape either; the label makes the four raw probes —
#   this one, 85-bot's enrich probe, 101, 102 — read like every `dc run` one whatever compose does next.)
#   The probe starts the app's image WITHOUT the compose service's device reservation or environment (every
#   variable it reads is set below); it booted and passed on the reference stack's GPU image in the
#   `.e2e/gate26b` gate run (98-form done in 8 s), and nothing here encodes.
#
# WHY --network none. This is the first bundle that starts a REAL `/upload` job, and a job's last act dials
# discord.com (the follow-up message). The suite reaches Discord for nothing (85-bot's header says why), so the
# container has no network at all: the CDN fetch fails at once (the job's failure line), and the follow-up
# fails at once too ("the follow-up could not be posted") — both on the container's own log, nothing on the
# wire. The fetch → ingest → link path with a SUCCESSFUL fetch is pinned in Go against httptest servers
# (internal/httpapi TestUploadFormWholePath, internal/notify TestBotFollowupPostsTheLinkAsANewMessage); the
# live proof with Discord's own client is T-form on dev (the owner's hands).
#
# 98  a signed `/upload` with a CDN-shaped attachment answers the MODAL (type 9, `cb:form:<32 hex>`, the title
#     pre-filled from the file name, the six Delete-after options with `default` selected); once the job's
#     `/clip failed` line is on the log, the form's submit is "That clip could not be uploaded.", the SAME
#     submit again is "That form has expired.", an unknown id is the same sentence, and a `cb:defaults` click
#     from a member without Manage Server is still "You need Manage Server here."; the job's failure went out
#     as a FOLLOW-UP (not an edit), and neither the nonce nor the submitted title is on the log. Then a second
#     container with DISCORD_UPLOAD_FORM=off answers the same `/upload` with `{"type":5}`.
#     Since §20 erratum 325 the form's job also tries its private "Uploading…" line (no network: the log says
#     it was not posted), no redraw is asked for a submit after a FAILED upload, and the interaction token is on
#     no log line; the `off` probe tries no such line. The redraw itself (a late title edits the posted link to
#     `?v=2`) needs a successful fetch and is pinned in Go (internal/httpapi TestUploadFormLateTitleRedrawsTheLink,
#     internal/notify TestBotRedrawEditsThePostedLinkOnce); the live proof is T-form 2 on dev (the owner's hands).

step 98 "the upload form: /upload answers a modal while the upload runs, a submit after a failed upload says so once and then expires, an unknown form expires, the panel stays gated, the failure is a follow-up and nothing typed is logged; DISCORD_UPLOAD_FORM=off answers the deferral"

command -v openssl >/dev/null 2>&1 || die "openssl is not on PATH; this step mints its own ed25519 keypair with it"
command -v xxd >/dev/null 2>&1 || die "xxd is not on PATH; this step needs it to render keys and signatures as hex"

FM_DIR="$E2E_DIR/form"
FM_TS=$(date +%s)   # the CLOCK's: the box admits a timestamp within ±300 s of its own (§20 erratum 279 (g), amended)
fm_sign() {
  printf '%s%s' "$FM_TS" "$1" > "$FM_DIR/msg.bin"
  openssl pkeyutl -sign -rawin -inkey "$FM_DIR/key.pem" -in "$FM_DIR/msg.bin" 2>/dev/null | xxd -p | tr -d '\n\r'
}

FM_G=444444444444444498
FM_APP=987654321098765498
FM_TOKEN=MTIzNDU2Nzg5MDEyMzQ1Njc4.GhIjKl.mNoPqRsTuVwXyZ0123456789abcdef   # token-SHAPED; nothing is registered
FM_PW=e2e-form-password-long-enough
FM_ORIGIN=https://form-probe.example
FM_TITLE_TYPED=FormTypedTitleNeverLogged
FM_UPLOAD='{"type":2,"token":"e2e-form-token","guild_id":"'$FM_G'","data":{"name":"upload","type":1,"options":[{"name":"file","type":11,"value":"1500000000000000098"}],"resolved":{"attachments":{"1500000000000000098":{"id":"1500000000000000098","filename":"form-probe.mp4","url":"https://cdn.discordapp.com/attachments/1/2/form-probe.mp4","size":4096,"content_type":"video/mp4"}}}}}'
# fm_submit <nonce> -> the form's MODAL_SUBMIT, from someone holding no Manage Server
fm_submit() {
  printf '{"type":5,"token":"e2e-form-submit","guild_id":"%s","member":{"permissions":"2147601473"},"data":{"custom_id":"cb:form:%s","components":[{"id":1,"type":10},{"id":2,"type":18,"component":{"id":3,"type":4,"custom_id":"cb:form:title","value":"%s"}},{"id":4,"type":18,"component":{"id":5,"type":21,"custom_id":"cb:form:ttl","value":"7"}},{"id":6,"type":18,"component":{"id":7,"type":4,"custom_id":"cb:form:tags","value":"gg"}}]}}' "$FM_G" "$1" "$FM_TITLE_TYPED"
}
FM_CLICK='{"type":3,"token":"e2e-form-token","guild_id":"'$FM_G'","member":{"permissions":"2147601473"},"data":{"component_type":2,"custom_id":"cb:defaults"}}'

# The image the stack's app runs, read off the running container — the build under test, whatever tag it has.
FM_APPCT=$(dc ps -q app 2>/dev/null | tr -d '\r' | head -1)
[ -n "$FM_APPCT" ] || die "the app container is not running"
FM_IMG=$(docker_ inspect --format '{{.Config.Image}}' "$FM_APPCT" | tr -d '\r')
[ -n "$FM_IMG" ] || die "could not read the app container's image"
FM_PORT=$(envfile CLIPPET_PUBLISH); FM_PORT=${FM_PORT##*:}; FM_PORT=${FM_PORT:-8481}
FM_CT="clippet-e2e-form-$FM_PORT"
FM_CT2="clippet-e2e-formoff-$FM_PORT"
# Everything this bundle starts is in these two names and the folder above; the trap removes them.
fm_stop() { docker_ rm -f "$FM_CT" "$FM_CT2" >/dev/null 2>&1 || true; rm -rf "$FM_DIR"; }
fm_stop; mkdir -p "$FM_DIR"
openssl genpkey -algorithm ed25519 -out "$FM_DIR/key.pem" >/dev/null 2>&1 || die "openssl could not mint an ed25519 key"
FM_PUB=$(openssl pkey -in "$FM_DIR/key.pem" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p | tr -d '\n\r')
[ "${#FM_PUB}" = 64 ] || die "the minted public key is ${#FM_PUB} hex characters, want 64: $FM_PUB"
trap 'fm_stop' EXIT

# fm_start <name> <DISCORD_UPLOAD_FORM value> — a probe with NO network, booted and bound.
fm_start() {
  docker_ run -d --name "$1" --network none --label "com.docker.compose.project=${E2E_PROJECT:-}" --label com.docker.compose.oneoff=True \
    -e DISCORD_APP_ID="$FM_APP" -e DISCORD_PUBLIC_KEY="$FM_PUB" -e DISCORD_BOT_TOKEN="$FM_TOKEN" \
    -e DISCORD_UPLOAD_FORM="$2" -e PUBLIC_BASE_URL="$FM_ORIGIN" \
    -e DATA_DIR=/tmp/formprobe -e SCRATCH_DIR=/tmp/formprobe-scratch \
    -e STORAGE_BACKEND=fs -e STORAGE_DIR=/tmp/formprobe-media \
    --entrypoint sh "$FM_IMG" -c 'mkdir -p /tmp/formprobe /tmp/formprobe-scratch /tmp/formprobe-media && exec clippet serve >/tmp/boot.log 2>&1' \
    >/dev/null || die "could not start the form probe $1 (if the image cannot boot without a network, this bundle must not run: it would otherwise reach Discord)"
  FM_N=0
  until docker_ exec "$1" sh -c 'grep -q "msg=listening" /tmp/boot.log' 2>/dev/null; do
    FM_N=$((FM_N + 1)); [ "$FM_N" -lt 60 ] || die "the form probe $1 did not boot: $(docker_ exec "$1" cat /tmp/boot.log 2>&1)"
    sleep 1
  done
  docker_ exec "$1" sh -c 'printf "%s" "'"$FM_PW"'" | clippet admin users create formowner >/dev/null && clippet admin bot bind '"$FM_G"' formowner --no-register >/dev/null' \
    || die "the form probe $1 could not create and bind its account"
}
# fm_post <container> <label> <body> — sign HERE, post THERE, one labelled line.
fm_post() {
  FM_TS=$(date +%s); FM_SIG=$(fm_sign "$3")
  docker_ exec -e BODY="$3" -e SIG="$FM_SIG" -e TS="$FM_TS" "$1" sh -c '
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -X POST -H "Content-Type: application/json" \
      -H "X-Signature-Ed25519: $SIG" -H "X-Signature-Timestamp: $TS" --data-binary "$BODY" http://localhost:8080/api/discord/interactions)
    printf "%s status=%s body=%s\n" "'"$2"'" "$code" "$(tr -d "\n" < /tmp/body)"'
}
fm_answer() {   # fm_answer <line> <sentence> — an ephemeral type-4 line
  contains "$1" 'status=200' || die "not a 200: $1"
  contains "$1" '"type":4' || die "not a message answer: $1"
  contains "$1" '"flags":64' || die "not ephemeral: $1"
  contains "$1" "\"content\":\"$2\"" || die "want \"$2\": $1"
}
fm_wait_log() {   # fm_wait_log <container> <fixed string> — a positive line, polled
  FM_N=0
  until docker_ exec "$1" grep -qF "$2" /tmp/boot.log 2>/dev/null; do
    FM_N=$((FM_N + 1)); [ "$FM_N" -lt 60 ] || die "the log of $1 never said \"$2\": $(docker_ exec "$1" cat /tmp/boot.log 2>&1)"
    sleep 1
  done
}

fm_start "$FM_CT" on
FM_MODAL=$(fm_post "$FM_CT" modal "$FM_UPLOAD")
printf '%s\n' "$FM_MODAL" | sed -n 's/^/[e2e]   form: /p'
contains "$FM_MODAL" 'status=200' || die "/upload was not answered: $FM_MODAL"
contains "$FM_MODAL" '"type":9' || die "/upload did not answer a modal: $FM_MODAL"
FM_NONCE=$(printf '%s' "$FM_MODAL" | sed -n 's/.*"custom_id":"cb:form:\([0-9a-f]\{32\}\)".*/\1/p' | head -1)
[ "${#FM_NONCE}" = 32 ] || die "the modal's custom_id is not cb:form:<32 hex>: $FM_MODAL"
contains "$FM_MODAL" '"title":"Your clip is uploading"' || die "the modal's title: $FM_MODAL"
contains "$FM_MODAL" '"value":"form-probe"' || die "the title is not pre-filled from the file name: $FM_MODAL"
contains "$FM_MODAL" '"default":true,"label":"default · ' || die "the first Delete-after option is not the selected default: $FM_MODAL"
for fm_v in default 1 7 30 90 0; do
  contains "$FM_MODAL" "\"value\":\"$fm_v\"" || die "the Delete-after options lack $fm_v: $FM_MODAL"
done
[ "$(printf '%s' "$FM_MODAL" | grep -o '"default":true' | wc -l | tr -d ' ')" = 1 ] || die "more than one option is selected: $FM_MODAL"
# The job runs behind the form, with no network: its fetch fails at once.
fm_wait_log "$FM_CT" 'discord bot: /clip failed'
FM_S1=$(fm_post "$FM_CT" failed "$(fm_submit "$FM_NONCE")")
FM_S2=$(fm_post "$FM_CT" again "$(fm_submit "$FM_NONCE")")
FM_S3=$(fm_post "$FM_CT" unknown "$(fm_submit 0123456789abcdef0123456789abcdef)")
FM_S4=$(fm_post "$FM_CT" panel "$FM_CLICK")
printf '%s\n' "$FM_S1" "$FM_S2" "$FM_S3" "$FM_S4" | sed -n 's/^/[e2e]   form: /p'
fm_answer "$FM_S1" 'That clip could not be uploaded.'
fm_answer "$FM_S2" 'That form has expired.'
fm_answer "$FM_S3" 'That form has expired.'
fm_answer "$FM_S4" 'You need Manage Server here.'
# The failure sentence went out as a FOLLOW-UP (a modal leaves nothing to edit), and could not be posted —
# the container has no network, which is the proof nothing reached Discord.
fm_wait_log "$FM_CT" 'discord bot: the follow-up could not be posted'
# The private "Uploading…" (erratum 325) was tried beside the fetch, and with no network it was not posted.
fm_wait_log "$FM_CT" 'discord bot: the uploading line was not posted'
FM_LOG=$(docker_ exec "$FM_CT" cat /tmp/boot.log 2>&1)
contains "$FM_LOG" 'card redraw' && die "a submit after a failed upload asked for a redraw: $FM_LOG"
contains "$FM_LOG" 'e2e-form-token' && die "the interaction token reached the log"
contains "$FM_LOG" 'the deferred reply could not be edited' && die "a form job tried to edit a deferred reply: $FM_LOG"
contains "$FM_LOG" 'upload_form=on' || die "the boot line does not say upload_form=on: $FM_LOG"
contains "$FM_LOG" "$FM_NONCE" && die "the form's nonce reached the log"
contains "$FM_LOG" "$FM_TITLE_TYPED" && die "the submitted title reached the log"
contains "$FM_LOG" "$FM_TOKEN" && die "the bot token reached the log"
FM_DOC=$(docker_ exec "$FM_CT" sh -c 'clippet admin doctor 2>&1 | grep "^bot"' 2>&1 || true)
contains "$FM_DOC" 'upload form: on - /upload opens a form (Title, Delete after, Tags) while the upload runs; DISCORD_UPLOAD_FORM=off removes it' \
  || die "doctor's bot row does not print the form: $FM_DOC"
docker_ rm -f "$FM_CT" >/dev/null 2>&1 || true

# The switch: the same /upload on a probe with DISCORD_UPLOAD_FORM=off is today's deferral, byte for byte.
fm_start "$FM_CT2" off
FM_OFF=$(fm_post "$FM_CT2" off "$FM_UPLOAD")
printf '%s\n' "$FM_OFF" | sed -n 's/^/[e2e]   form: /p'
[ "$FM_OFF" = 'off status=200 body={"type":5}' ] || die "with DISCORD_UPLOAD_FORM=off, /upload did not answer {\"type\":5}: $FM_OFF"
fm_wait_log "$FM_CT2" 'discord bot: the deferred reply could not be edited'
FM_LOG=$(docker_ exec "$FM_CT2" cat /tmp/boot.log 2>&1)
contains "$FM_LOG" 'upload_form=off' || die "the boot line does not say upload_form=off: $FM_LOG"
contains "$FM_LOG" 'uploading line' && die "with the form off, the job tried the Uploading line: $FM_LOG"
fm_stop
trap - EXIT
echo "[e2e]   form: /upload answered the modal (pre-filled title, six options, default selected); after the failed upload the submit said so once and then expired; an unknown form expired; the panel stayed gated; the failure went out as a follow-up, the private Uploading line was tried, no redraw for a failed upload, nothing typed, the nonce or the token on the log; DISCORD_UPLOAD_FORM=off answered {\"type\":5}; both probe containers removed"
