# 108-person.sh — a person-scoped server binding (DESIGN.md §20 erratum 342, migration 0024): the box
# consents to a Discord server for ONE member, the bound account's linked Discord user, so the official
# bot's plane may route that member's commands here and nobody else's. Step 108. Sourced by
# scripts/e2e.sh (see scripts/e2e.d/README.md). Every name here is BP_-prefixed.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=BP_ buckets=-
# e2e-covers: internal/httpapi/bot_claim.go internal/httpapi/discord.go internal/db/discord.go
# e2e-covers: cmd/clippet/admin_bot.go cmd/clippet/admin.go internal/db/migrations/0024_guild_scope.sql
#   ONE throwaway `dc run --rm` probe, 85-bot's managed shape: its own DATA_DIR and SCRATCH_DIR, its own
#   loopback, nothing published, DISCORD_BOT_TOKEN=managed (the official bot's box, where a person
#   binding lives) and a keypair the harness mints itself (openssl ed25519, Discord's algorithm). Its
#   account lives in the probe's private database and never on this stack. Parallel for 85-bot's
#   reason: no clip, no account, no setting of THIS stack, and no instance-wide assertion.
#
# 108 in order: `admin bot bind --person` is refused while the account has no Discord user id (exit 1,
#     the sentence naming `admin users link`); `users link`; the bind succeeds and says `(person)`;
#     `bot ls` ends the row with `person`; the origin proof GET /api/bot/claim answers 404 (empty) with
#     no user_id and with another member's id, 200 `{bound, app_id, scope:"person"}` with NO `defaults`
#     for the linked id, and 400 `invalid_user_id` for a malformed one; a signed `/upload` from another
#     member is `Not your Clippet.` (ephemeral) and from the linked member passes the check to the
#     attachment refusal; `users unlink` is refused while the binding names the account; after `bot
#     unbind` the proof is 404 for the linked id too; and no member id reaches the probe's log.

step 108 "person binding (#342): bind --person needs a linked id; the claim proof answers the linked member alone, scope person, no defaults; another member's /upload is 'Not your Clippet.'; unlink is held while the binding names the account"

command -v openssl >/dev/null 2>&1 || die "openssl is not on PATH; this step mints its own ed25519 keypair with it"
command -v xxd >/dev/null 2>&1 || die "xxd is not on PATH; this step needs it to render keys and signatures as hex"

BP_DIR="$E2E_DIR/person"; rm -rf "$BP_DIR"; mkdir -p "$BP_DIR"
openssl genpkey -algorithm ed25519 -out "$BP_DIR/key.pem" >/dev/null 2>&1 || die "openssl could not mint an ed25519 key"
BP_PUB=$(openssl pkey -in "$BP_DIR/key.pem" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p | tr -d '\n\r')
[ "${#BP_PUB}" = 64 ] || die "the minted public key is ${#BP_PUB} hex characters, want 64"
BP_TS=$(date +%s)   # the CLOCK's: the box admits a timestamp within ±300 s of its own (§20 erratum 279 (g), amended)
bp_sign() {
  printf '%s%s' "$BP_TS" "$1" > "$BP_DIR/msg.bin"
  openssl pkeyutl -sign -rawin -inkey "$BP_DIR/key.pem" -in "$BP_DIR/msg.bin" 2>/dev/null | xxd -p | tr -d '\n\r'
}
BP_G=222222222222222208
BP_LINKED=444444444444444408   # the bound account's Discord user
BP_OTHER=555555555555555508    # somebody else in the same server
bp_upload() {   # bp_upload <user id> -> an `/upload` with no attachment, sent by that user
  printf '{"type":2,"token":"e2e-person-token","guild_id":"%s","member":{"permissions":"0","user":{"id":"%s","username":"m%s"}},"data":{"name":"upload"}}' "$BP_G" "$1" "$1"
}
BP_UO=$(bp_upload "$BP_OTHER"); BP_SIG_UO=$(bp_sign "$BP_UO")
BP_UL=$(bp_upload "$BP_LINKED"); BP_SIG_UL=$(bp_sign "$BP_UL")
[ "${#BP_SIG_UO}" = 128 ] || die "the signature is ${#BP_SIG_UO} hex characters, want 128"
BP_APP=987654321098765408
BP_PW=e2e-person-password-long-enough

BP_OUT=$(dc run --rm -T --no-deps \
  -e DISCORD_APP_ID="$BP_APP" -e DISCORD_PUBLIC_KEY="$BP_PUB" -e DISCORD_BOT_TOKEN=managed \
  -e DISCORD_COMMAND_NAME= -e DISCORD_UPLOAD_COMMAND_NAME= -e DISCORD_ENRICH_COMMAND_NAME= -e DISCORD_SETTINGS_COMMAND_NAME= \
  -e DISCORD_AUTO_MODE= -e DISCORD_AUTO_TOKEN= -e DISCORD_LOGIN_CLIENT_ID= -e DISCORD_LOGIN_CLIENT_SECRET= \
  -e DATA_DIR=/tmp/personprobe -e SCRATCH_DIR=/tmp/personprobe-scratch -e PUBLIC_BASE_URL=http://localhost:8080 \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e BP_TS="$BP_TS" -e BP_G="$BP_G" -e BP_LINKED="$BP_LINKED" -e BP_OTHER="$BP_OTHER" -e BP_PW="$BP_PW" \
  -e BP_UO="$BP_UO" -e BP_SIG_UO="$BP_SIG_UO" -e BP_UL="$BP_UL" -e BP_SIG_UL="$BP_SIG_UL" \
  --entrypoint sh app -c '
    set -e
    mkdir -p /tmp/personprobe
    clippet serve >/tmp/boot.log 2>&1 &
    p=$!
    i=0
    while [ $i -lt 30 ]; do grep -q "msg=listening" /tmp/boot.log && break; i=$((i+1)); sleep 1; done
    grep -q "msg=listening" /tmp/boot.log || { echo "PROBE-DID-NOT-BOOT"; cat /tmp/boot.log; exit 1; }
    EP=http://localhost:8080/api/discord/interactions   # named EP and CP, never one letter: the harness owns those globals (85-bot)
    CP=http://localhost:8080/api/bot/claim
    say() { printf "%s status=%s body=%s\n" "$1" "$2" "$3"; }
    cli() {   # cli <label> <args…> -> one line: the exit code and every word it printed
      rc=0; clippet admin "$@" >/tmp/cli.out 2>&1 || rc=$?
      printf "%s rc=%s %s\n" "$L" "$rc" "$(tr "\n" " " < /tmp/cli.out)"
    }
    claim() {   # claim <label> <query tail>
      code=$(curl -sS -o /tmp/body -w "%{http_code}" "$CP?guild_id=$BP_G$2")
      say "$1" "$code" "$(tr -d "\n" < /tmp/body)"
    }
    call() {   # call <label> <signature> <body>
      code=$(curl -sS -o /tmp/body -w "%{http_code}" -X POST -H "Content-Type: application/json" \
        -H "X-Signature-Ed25519: $2" -H "X-Signature-Timestamp: $BP_TS" --data-binary "$3" "$EP")
      say "$1" "$code" "$(tr -d "\n" < /tmp/body)"
    }
    printf "%s" "$BP_PW" | clippet admin users create friend >/dev/null
    L=bind-noid cli bot bind "$BP_G" friend --person
    L=link cli users link friend "$BP_LINKED"
    L=bind cli bot bind "$BP_G" friend --person
    clippet admin bot ls | sed "s/^/ls: /"
    claim claim-nouser ""
    claim claim-other "&user_id=$BP_OTHER"
    claim claim-linked "&user_id=$BP_LINKED"
    claim claim-bad "&user_id=nope"
    call upload-other "$BP_SIG_UO" "$BP_UO"
    call upload-linked "$BP_SIG_UL" "$BP_UL"
    L=unlink cli users unlink friend
    L=unbind cli bot unbind "$BP_G"
    claim claim-after "&user_id=$BP_LINKED"
    printf "logids=%s logfield=%s\n" "$(grep -cE "$BP_LINKED|$BP_OTHER" /tmp/boot.log || true)" "$(grep -c "discord_guild=not_yours" /tmp/boot.log || true)"
    kill "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  ' 2>&1) || die "the person-binding probe container failed: $BP_OUT"

printf '%s\n' "$BP_OUT" | sed -n 's/^/[e2e]   probe: /p'
bp_line() { printf '%s\n' "$BP_OUT" | grep "^$1[ =]" | head -1; }
# (a) --person needs the account's Discord user id, and says which command gives it one.
BP_L=$(bp_line bind-noid)
contains "$BP_L" 'rc=1' || die "bind --person without an id did not exit 1: $BP_L"
contains "$BP_L" 'admin bot bind: friend has no Discord user id - run `clippet admin users link friend <discord-id>` first' \
  || die "bind --person without an id is not the sentence: $BP_L"
contains "$(bp_line link)" 'rc=0' || die "users link failed: $(bp_line link)"
# (b) The bind, the (person) note, and ls's fourth column.
BP_L=$(bp_line bind)
contains "$BP_L" 'rc=0' || die "bind --person failed: $BP_L"
contains "$BP_L" "guild $BP_G bound to friend (" || die "bind --person did not report the binding: $BP_L"
contains "$BP_L" ') (person)' || die "bind --person does not say (person): $BP_L"
printf '%s\n' "$BP_OUT" | grep "^ls: $BP_G  friend  " | grep -q '  person$' || die "ls does not end the row with person: $BP_OUT"
# (c) The origin proof answers the linked member alone.
[ "$(bp_line claim-nouser)" = 'claim-nouser status=404 body=' ] || die "a guild-only ask of a person binding is not an empty 404: $(bp_line claim-nouser)"
[ "$(bp_line claim-other)" = 'claim-other status=404 body=' ] || die "another member's ask is not an empty 404: $(bp_line claim-other)"
BP_L=$(bp_line claim-linked)
contains "$BP_L" 'status=200' || die "the linked member's ask is not a 200: $BP_L"
contains "$BP_L" '"bound":true' || die "the 200 does not say bound: $BP_L"
contains "$BP_L" '"app_id":"'"$BP_APP"'"' || die "the 200 does not carry the application id: $BP_L"
contains "$BP_L" '"scope":"person"' || die "the 200 does not say scope person: $BP_L"
contains "$BP_L" 'defaults' && die "a person answer carries the account's defaults: $BP_L"
contains "$BP_L" "$BP_LINKED" && die "the 200 echoes the member's id: $BP_L"
BP_L=$(bp_line claim-bad)
contains "$BP_L" 'status=400' || die "a malformed user_id is not a 400: $BP_L"
contains "$BP_L" 'invalid_user_id' || die "the 400 does not name its rule: $BP_L"
# (d) The invoker check: another member is refused in one private line; the linked member goes on to
#     the ordinary attachment refusal (no attachment in the body, so nothing is fetched either way).
BP_L=$(bp_line upload-other)
contains "$BP_L" 'status=200' || die "another member's /upload was not answered: $BP_L"
contains "$BP_L" '"content":"Not your Clippet."' || die "another member's /upload is not refused: $BP_L"
contains "$BP_L" '"flags":64' || die "the refusal is not ephemeral: $BP_L"
BP_L=$(bp_line upload-linked)
contains "$BP_L" '"content":"Attach a video to /upload."' || die "the linked member's /upload did not pass the check: $BP_L"
# (e) Unlink is held while the person binding names the account; the unbind ends the consent.
BP_L=$(bp_line unlink)
contains "$BP_L" 'rc=1' || die "users unlink under a person binding did not exit 1: $BP_L"
contains "$BP_L" "admin users unlink: friend has a person binding (guild $BP_G) - run \`clippet admin bot unbind $BP_G\` first" \
  || die "users unlink under a person binding is not the sentence: $BP_L"
contains "$(bp_line unbind)" 'rc=0' || die "bot unbind failed: $(bp_line unbind)"
[ "$(bp_line claim-after)" = 'claim-after status=404 body=' ] || die "the proof still answers after the unbind: $(bp_line claim-after)"
# (f) The log says why and names nobody.
BP_L=$(bp_line logids)
contains "$BP_L" 'logids=0 ' || die "a member's id reached the probe's log: $BP_L"
contains "$BP_L" 'logfield=0' && die "the probe's log has no discord_guild=not_yours line: $BP_L"
echo "[e2e]   person binding: --person refused without an id, then bound (person) and listed; the proof 404/404/200 scope person (no defaults)/400; another member's /upload 'Not your Clippet.', the linked member's past the check; unlink held; unbind ends it; no id on the log"
