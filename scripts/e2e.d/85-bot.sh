# 85-bot.sh — ROADMAP #33 (DESIGN.md §20 errata 253-257, 286, 315): the Discord bot's interactions endpoint,
# the guild binding CLI, and the off state. Steps 85-86. Sourced by scripts/e2e.sh (see
# scripts/e2e.d/README.md). Every name here is BT_-prefixed.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=BT_ buckets=-
# e2e-covers: internal/httpapi/discord*.go internal/httpapi/bot_claim.go internal/notify/bot*.go
# e2e-covers: internal/notify/reply.go internal/config/discordbot.go internal/db/discord.go cmd/clippet/admin_bot.go
# e2e-covers: internal/worker/worker.go internal/worker/repair.go internal/urls/**
#   Step 85 boots SIX throwaway containers, each with its own DATA_DIR and its own SCRATCH_DIR (see
#   below) and none of them publishing a port — they talk to themselves on the container's loopback.
#   The fifth (§20 erratum 315, the enhanced message) also keeps its media in a private STORAGE_DIR
#   (STORAGE_BACKEND=fs), so the one clip it encodes never reaches this stack's bucket; it is NAMED
#   (`clippet-e2e-botcard-<port>`) because the harness drives it with `docker exec`, and it is removed
#   at the end of the step and on any failure (trap), and by step 1's stale-probe sweep after a kill.
#   The sixth (§20 erratum 323, Enrich on a video) is `docker run --network none` of the app's own image
#   (`clippet-e2e-botvid-<port>`, this project's label, its own DATA_DIR/SCRATCH_DIR/STORAGE_DIR, no
#   compose volume), named and removed the same way: an accepted Enrich starts a real job, and with no
#   network its fetch cannot reach Discord. It is booted twice: once without DISCORD_ENRICH_UPLOAD (the
#   default, off), once with it on.
#   Step 86 reads `admin doctor` on the live stack and POSTs one request the app refuses.
#
#   It used to say `exclusive`, and its own reason said why that was habit: "Neither is
#   parallel-unsafe on today's bundles". Read against the README's list (§20 erratum 283 (e)): it
#   creates no clip and no user on this stack, mutates no shared clip and no account-wide setting,
#   runs no `dc up` and no second process against THIS database, and asserts nothing bucket-, log- or
#   instance-wide — step 86 reads exactly two things out of `doctor`, the word `on` or `off` on the
#   `bot` row, and matches the route's status to it. The doctor's bucket WRITE probe writes and
#   removes one key of its own, which no per-clip prefix count can see and which the bundles that DO
#   census the bucket (47-upload, 51-admin) never meet, being exclusive and therefore later.
#
# WHAT THIS CAN AND CANNOT PROVE. The suite has no Discord: it cannot make Discord POST an
# interaction, it cannot fetch an attachment from Discord's CDN, and it must not try — the app's only
# new egress is two CDN hosts, and an E2E that reached them would be testing Discord's uptime. So the
# endpoint is proven with a keypair the harness MINTS ITSELF (openssl, ed25519 — the same algorithm
# Discord signs with), and every interaction below is one this app can answer without any outbound
# call at all: a PING, and a `/clip` with NO attachment (the guild binding is read before the
# attachment is looked at, so the refusal proves the binding either way). The fetch → ingest →
# follow-up path is unit-tested against httptest servers in internal/notify, and the live proof — a
# real `/clip` with a real video in the owner's own server — is a run by hand and is the one half
# no agent can perform.
#
# 85  the endpoint, in a throwaway container configured with a minted key: a bad signature is 401 with
#     an EMPTY body (Discord's endpoint validation sends exactly that and refuses a 200); a signed PING
#     is `{"type":1}`; a signed `/clip` from an UNBOUND guild is the one-line ephemeral refusal; the
#     `admin bot bind`/`ls`/`unbind` round trip changes which of the two refusals a bound guild gets;
#     and a `/clip` carrying the WITHDRAWN `link` option (§20 erratum 286 withdrew erratum 268's second
#     option; an OLD command definition still registered in a server can still send one) is answered by
#     the ordinary "attach a video" line, with no outbound call and no second vocabulary.
#     Since §20 erratum 279 (the official bot, ROADMAP #34 lane A) the same container also proves the
#     ORIGIN PROOF, `GET /api/bot/claim?guild_id=`: 404 with an empty body before `admin bot bind`, 200
#     `{"bound":true,"app_id":…}` after it, 404 again after `unbind`, 400 for a malformed id — and a
#     SECOND short container booted with `DISCORD_BOT_TOKEN=managed` (the sentinel) proves that
#     `admin bot register` is refused in words, `bind` publishes nothing and says so, the claim endpoint
#     answers for the bound server, and `admin doctor`'s bot row says `managed` and lists the binding.
#     Since §20 erratum 284 a THIRD short container proves the command's NAME is a setting
#     (DISCORD_COMMAND_NAME=clip-dev): the boot line and `admin doctor` both name it, `bind` prints it,
#     the renamed interaction is answered by the binding and the DEFAULT `/clip` is refused as an
#     unknown command — and a FOURTH, managed AND renamed, refuses to boot.
#     Since §20 erratum 315 there are THREE commands — `/upload file:` (the old `/clip`, renamed), `/clip
#     link:` and the message command "Enrich" — so the bodies above say `upload`, the renamed container
#     renames all three, and a FIFTH container proves the ENHANCED MESSAGE: it encodes one tiny clip of
#     its own (ffmpeg's test pattern, imported with `admin import`, on a private STORAGE_DIR) and signs
#     `/clip link:` and Enrich interactions at it — the public type-4 answer carries the link alone as
#     its content and nothing else (§20 erratum 316: the page declares the card), in
#     the encoding state and again once ready; a foreign link, a message with no link of ours and an
#     expired clip get their one-line refusals. `/upload`'s own follow-up is the same builder's output
#     but goes to Discord's API, which this suite never calls: its body is pinned in internal/notify
#     against an httptest Discord (TestBotClipFetchesIngestsAndPostsTheLink), and so is what `admin bot
#     register` PUTs (TestRegisterPublishesTheGuildCommandWithTheBotToken: three definitions).
#     Since §20 erratum 318 the bound guild of the FIRST container also answers `/settings` (run 4's panel
#     contract §4/§5): the box's own command is the one-row Components V2 panel (ephemeral, `Link defaults`
#     + `Edit`), a signed click on `cb:defaults` is the MODAL (type 9, the three Labels: a Radio Group, a
#     Checkbox, a Radio Group, pre-filled from the account), a signed MODAL_SUBMIT is `Changes saved.` — and the
#     account's defaults read back through GET /api/auth/me/settings are what the modal saved (the
#     settings page's own writer); a click from a member without Manage Server is refused in one line.
#     The renamed container renames `/settings` too (`settings-dev`), and the managed refusal names it.
# 86  the OFF state on the live stack (no DISCORD_* set): the route is 404, `admin doctor` says
#     "discord bot: off" — and a container given ONE of the three variables refuses to boot, naming
#     all three.
#
# The private SCRATCH_DIR beside the private DATA_DIR is not tidiness (README's third convention, and
# check.sh has a tripwire for it): `compose run` still mounts the SERVICE's volumes, so a probe that
# redirects DATA_DIR alone boots an EMPTY database over the LIVE /scratch and serve.go's boot janitor
# deletes every real working directory it finds row-less.

step 85 "the interactions endpoint: a bad signature is 401, a signed PING is a PONG, the guild binding decides the rest, a link option on /upload is answered as no attachment; the origin proof GET /api/bot/claim follows the binding; the managed sentinel registers nothing; the four command names are settings and a managed instance may not rename them; /settings answers its panel, the Edit click its modal and the modal submit Changes saved. through the settings writer; /clip link: and Enrich post the enhanced message; Enrich on a video uploads it for the poster or a manager"

command -v openssl >/dev/null 2>&1 || die "openssl is not on PATH; this step mints its own ed25519 keypair with it"
command -v xxd >/dev/null 2>&1 || die "xxd is not on PATH; this step needs it to render keys and signatures as hex"

BT_DIR="$E2E_DIR/bot"; rm -rf "$BT_DIR"; mkdir -p "$BT_DIR"
# The keypair. It never leaves .e2e/ and exists for this run only: the app is given the PUBLIC half
# (which is all DISCORD_PUBLIC_KEY ever is — it verifies and cannot sign), and the harness keeps the
# private half to sign with, exactly as Discord does.
openssl genpkey -algorithm ed25519 -out "$BT_DIR/key.pem" >/dev/null 2>&1 || die "openssl could not mint an ed25519 key"
# The raw 32-byte public key is the tail of the DER SubjectPublicKeyInfo (12 bytes of prefix).
BT_PUB=$(openssl pkey -in "$BT_DIR/key.pem" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p | tr -d '\n\r')
[ "${#BT_PUB}" = 64 ] || die "the minted public key is ${#BT_PUB} hex characters, want 64: $BT_PUB"

# bt_sign <body> -> the hex signature over <timestamp><body>, which is what Discord signs; bt_sign_at <ts> <body>
# with a timestamp of its own. The timestamp is the CLOCK's: the box admits one within ±300 s of its own
# (§20 erratum 279 (g), amended), so bt_now re-reads it before every batch of signatures and no signature is
# used more than a few seconds after it was made.
bt_now() { BT_TS=$(date +%s); }
bt_now
bt_sign_at() {
  printf '%s%s' "$1" "$2" > "$BT_DIR/msg.bin"
  openssl pkeyutl -sign -rawin -inkey "$BT_DIR/key.pem" -in "$BT_DIR/msg.bin" 2>/dev/null | xxd -p | tr -d '\n\r'
}
bt_sign() { bt_sign_at "$BT_TS" "$1"; }

BT_G1=222222222222222222      # bound during this step
BT_G2=333333333333333333      # never bound
BT_PING='{"type":1}'
BT_C1='{"type":2,"token":"e2e-interaction-token","guild_id":"'$BT_G1'","data":{"name":"upload"}}'
BT_C2='{"type":2,"token":"e2e-interaction-token","guild_id":"'$BT_G2'","data":{"name":"upload"}}'
# The WITHDRAWN `link` option (§20 erratum 286). Erratum 268 gave `/clip` a second
# option; the owner withdrew it — a person who uploads on the site already holds the
# link, and a person who uploads in the server gets it dropped where they ran the
# command, so the option only ever asked somebody to type a link back at the place
# it came from. What a server's command picker offers is the definition Discord
# STORED when that server was last registered, though, so a `/clip` published by an
# older build can still send one, and this is the probe for that: an option this
# build does not know is not an attachment, so the answer is the ordinary "attach
# a video" line, not a crash and not a second sentence that would distinguish one
# link from another.
#
# The link in that body names BT_ORIGIN below, and NOTHING READS IT BACK: since
# erratum 286 the answer is one fixed sentence, so the value only has to be a
# well-formed link of a plausible origin. The probe is still given its own
# PUBLIC_BASE_URL rather than the stack's, because an assertion that ever did read
# it would otherwise say something different on a lane stack and on the reference
# stack (no origin-dependent assertions).
BT_ORIGIN=https://bot-probe.example
# …which is an https origin, so the three probe containers that take it below also pin MEDIA_DELIVERY=proxy and
# blank the presign variables: compose interpolates MEDIA_PRESIGN_ENDPOINT for every container of
# this project and it was chosen for the STACK's base, so a plain-http one under this fabricated
# https base is §20 erratum 278(e)'s mixed-content boot error and the probe would never start
# (§20 erratum 280). A probe with no viewers has nothing to redirect anybody to.
BT_ID=AbcDefGhiJkl
bt_opt() {   # bt_opt <options json> -> a signed-able /upload body in the bound guild
  printf '{"type":2,"token":"e2e-interaction-token","guild_id":"%s","data":{"name":"upload","options":[%s]}}' "$BT_G1" "$1"
}
# A STRING `link` option on the UPLOAD command (§20 erratum 286 withdrew it there; erratum 315 gave
# `link` a command of its own, `/clip`): naming a link of THIS probe's own origin — the one shape worth
# proving the upload command never acts on.
BT_LINK_OLD=$(bt_opt '{"name":"link","type":3,"value":"'"$BT_ORIGIN"'/'"$BT_ID"'"}')
BT_SIG_PING=$(bt_sign "$BT_PING")
# A GENUINE signature over a timestamp 301 s old: outside the box's window, refused like a bad signature.
BT_TS_OLD=$((BT_TS - 301))
BT_SIG_STALE=$(bt_sign_at "$BT_TS_OLD" "$BT_PING")
BT_SIG_C1=$(bt_sign "$BT_C1")
BT_SIG_C2=$(bt_sign "$BT_C2")
BT_SIG_OLD=$(bt_sign "$BT_LINK_OLD")
# `/settings` (§20 erratum 318): the command, the Edit click (MESSAGE_COMPONENT, a Button), the modal's
# submit (MODAL_SUBMIT: three Labels, each with its one component — the shapes of Discord's components
# reference), each from a member holding MANAGE_GUILD ("32"); and the click again from a member holding
# neither ADMINISTRATOR nor MANAGE_GUILD (invite, reactions, view, send, embed, attach, history, commands).
BT_ST='{"type":2,"token":"e2e-interaction-token","guild_id":"'$BT_G1'","member":{"permissions":"32"},"data":{"name":"settings","type":1}}'
BT_SC='{"type":3,"token":"e2e-interaction-token","guild_id":"'$BT_G1'","member":{"permissions":"32"},"data":{"component_type":2,"custom_id":"cb:defaults"}}'
BT_SC0='{"type":3,"token":"e2e-interaction-token","guild_id":"'$BT_G1'","member":{"permissions":"2147601473"},"data":{"component_type":2,"custom_id":"cb:defaults"}}'
BT_SM='{"type":5,"token":"e2e-interaction-token","guild_id":"'$BT_G1'","member":{"permissions":"32"},"data":{"custom_id":"cb:defaults:modal","components":[{"id":1,"type":18,"component":{"id":2,"type":21,"custom_id":"cb:defaults:ttl","value":"30"}},{"id":3,"type":18,"component":{"id":4,"type":23,"custom_id":"cb:defaults:unlisted","value":true}},{"id":5,"type":18,"component":{"id":6,"type":21,"custom_id":"cb:defaults:link","value":"memorable"}}]}}'
BT_SIG_ST=$(bt_sign "$BT_ST")
BT_SIG_SC=$(bt_sign "$BT_SC")
BT_SIG_SC0=$(bt_sign "$BT_SC0")
BT_SIG_SM=$(bt_sign "$BT_SM")
[ "${#BT_SIG_PING}" = 128 ] || die "the signature is ${#BT_SIG_PING} hex characters, want 128"
# A well-formed signature that is simply not ours — what Discord's endpoint validation sends.
BT_SIG_BAD=00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000

BT_APP=987654321098765432
# A token-SHAPED value that is not a token: the probe never reaches Discord (nothing below registers a
# command), and config only checks the shape. It is still passed as a variable rather than written in
# a file, out of habit — it is the one secret of the three.
BT_TOKEN=MTIzNDU2Nzg5MDEyMzQ1Njc4.GhIjKl.mNoPqRsTuVwXyZ0123456789abcdef
BT_PW=e2e-bot-password-long-enough

# One container, one boot, every assertion: `curl` is in the app image (the healthcheck uses it), so
# the probe talks to itself on :8080 and nothing is published. `clippet admin` inside it reads the
# SAME private database the probe booted, which is what makes the CLI round trip real.
BT_OUT=$(dc run --rm -T --no-deps \
  -e DISCORD_APP_ID="$BT_APP" -e DISCORD_PUBLIC_KEY="$BT_PUB" -e DISCORD_BOT_TOKEN="$BT_TOKEN" \
  -e DATA_DIR=/tmp/botprobe -e SCRATCH_DIR=/tmp/botprobe-scratch -e PUBLIC_BASE_URL="$BT_ORIGIN" \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e BT_TS="$BT_TS" -e BT_PING="$BT_PING" -e BT_C1="$BT_C1" -e BT_C2="$BT_C2" \
  -e BT_SIG_PING="$BT_SIG_PING" -e BT_SIG_C1="$BT_SIG_C1" -e BT_SIG_C2="$BT_SIG_C2" \
  -e BT_LINK_OLD="$BT_LINK_OLD" -e BT_SIG_OLD="$BT_SIG_OLD" \
  -e BT_SIG_BAD="$BT_SIG_BAD" -e BT_G1="$BT_G1" -e BT_PW="$BT_PW" \
  -e BT_TS_OLD="$BT_TS_OLD" -e BT_SIG_STALE="$BT_SIG_STALE" \
  -e DISCORD_SETTINGS_COMMAND_NAME= -e BT_ST="$BT_ST" -e BT_SC="$BT_SC" -e BT_SC0="$BT_SC0" -e BT_SM="$BT_SM" \
  -e BT_SIG_ST="$BT_SIG_ST" -e BT_SIG_SC="$BT_SIG_SC" -e BT_SIG_SC0="$BT_SIG_SC0" -e BT_SIG_SM="$BT_SIG_SM" \
  --entrypoint sh app -c '
    set -e
    mkdir -p /tmp/botprobe
    clippet serve >/tmp/boot.log 2>&1 &
    p=$!
    i=0
    while [ $i -lt 30 ]; do grep -q "msg=listening" /tmp/boot.log && break; i=$((i+1)); sleep 1; done
    grep -q "msg=listening" /tmp/boot.log || { echo "PROBE-DID-NOT-BOOT"; cat /tmp/boot.log; exit 1; }
    grep "discord bot configured" /tmp/boot.log | sed "s/^/bootline: /"
    EP=http://localhost:8080/api/discord/interactions   # named EP and not U: check.sh greps this whole directory for the one-letter globals the harness owns, line by line, and cannot tell that this line runs inside a container
    CP=http://localhost:8080/api/bot/claim   # the origin proof (§20 erratum 279); named like EP, for the same reason
    say() { printf "%s status=%s body=%s\n" "$1" "$2" "$3"; }
    call() {
      code=$(curl -sS -o /tmp/body -w "%{http_code}" -X POST -H "Content-Type: application/json" \
        -H "X-Signature-Ed25519: $2" -H "X-Signature-Timestamp: $BT_TS" --data-binary "$3" "$EP")
      say "$1" "$code" "$(tr -d "\n" < /tmp/body)"
    }
    claim() {   # claim <label> <guild id> -> the anonymous GET the claim page makes
      code=$(curl -sS -o /tmp/body -w "%{http_code}" "$CP?guild_id=$2")
      say "$1" "$code" "$(tr -d "\n" < /tmp/body)"
    }
    call badsig "$BT_SIG_BAD" "$BT_PING"
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -X POST -H "Content-Type: application/json" \
      -H "X-Signature-Ed25519: $BT_SIG_STALE" -H "X-Signature-Timestamp: $BT_TS_OLD" --data-binary "$BT_PING" "$EP")
    say stale "$code" "$(tr -d "\n" < /tmp/body)"
    call ping "$BT_SIG_PING" "$BT_PING"
    call unbound1 "$BT_SIG_C1" "$BT_C1"
    claim claim-unbound "$BT_G1"
    claim claim-badid 12
    printf "%s" "$BT_PW" | clippet admin users create botowner | sed "s/^/create: /"
    clippet admin bot ls | sed "s/^/ls-empty: /"
    clippet admin bot bind "$BT_G1" botowner --no-register | sed "s/^/bind: /"
    clippet admin bot ls | sed "s/^/ls-bound: /"
    claim claim-bound "$BT_G1"
    call bound "$BT_SIG_C1" "$BT_C1"
    call linkold "$BT_SIG_OLD" "$BT_LINK_OLD"
    call settings "$BT_SIG_ST" "$BT_ST"
    call edit "$BT_SIG_SC" "$BT_SC"
    call edit-nomanage "$BT_SIG_SC0" "$BT_SC0"
    call saved "$BT_SIG_SM" "$BT_SM"
    # The bound account read back through the settings API, on a session from the API login. The
    # cookie is Secure on this https base, so it is carried by hand over the loopback.
    printf "{\"username\":\"botowner\",\"password\":\"%s\"}" "$BT_PW" > /tmp/login.json
    curl -sS -o /dev/null -D /tmp/login.h -X POST -H "Content-Type: application/json" --data-binary @/tmp/login.json http://localhost:8080/api/auth/login
    ck=$(tr -d "\r" < /tmp/login.h | grep -i "^set-cookie:" | head -1 | sed "s/^[^:]*: *//; s/;.*//")
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -H "Cookie: $ck" http://localhost:8080/api/auth/me/settings)
    say defaults "$code" "$(tr -d "\n" < /tmp/body)"
    call unbound2 "$BT_SIG_C2" "$BT_C2"
    clippet admin bot unbind "$BT_G1" | sed "s/^/unbind: /"
    call unbound3 "$BT_SIG_C1" "$BT_C1"
    claim claim-after "$BT_G1"
    clippet admin bot ls | sed "s/^/ls-after: /"
    clippet admin doctor 2>&1 | grep "^bot" | sed "s/^/doctor: /" || true
    kill "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  ' 2>&1) || die "the bot probe container failed: $BT_OUT"

printf '%s\n' "$BT_OUT" | sed -n 's/^/[e2e]   probe: /p'

# (a) The signature IS the credential. Discord's portal sends a deliberately bad one when the URL is
#     saved and requires a 401; an endpoint that answers 200 is refused there.
printf '%s\n' "$BT_OUT" | grep -q '^badsig status=401 body=$'   || die "a bad signature was not answered 401 with an EMPTY body: $BT_OUT"
#     …and so is a genuine signature over a timestamp 301 s old (§20 erratum 279 (g), amended).
printf '%s\n' "$BT_OUT" | grep -q '^stale status=401 body=$'   || die "a signature over a stale timestamp was not answered 401 with an EMPTY body: $BT_OUT"
# (b) The handshake.
contains "$BT_OUT" 'ping status=200 body={"type":1}' || die "a signed PING was not answered with a PONG: $BT_OUT"
# (c) An unbound server gets one terse line, ephemerally (flags 64), and nothing else. (The JSON is
#     Go's map ordering — `data` before `type` — so each answer is read by LABEL rather than by a
#     prefix that would pin an ordering nothing promises.)
bt_line() { printf '%s\n' "$BT_OUT" | grep "^$1 " | head -1; }
bt_answer() {   # bt_answer <label> <status> <content>
  BT_L=$(bt_line "$1")
  [ -n "$BT_L" ] || die "the probe printed no $1 line: $BT_OUT"
  contains "$BT_L" "status=$2" || die "$1 answered $BT_L, want status=$2"
  [ -z "$3" ] || contains "$BT_L" "\"content\":\"$3\"" || die "$1 answered $BT_L, want the line $3"
  [ -z "$3" ] || contains "$BT_L" '"type":4' || die "$1 is not a message answer: $BT_L"
  [ -z "$3" ] || contains "$BT_L" '"flags":64' || die "$1 is not ephemeral: $BT_L"
}
BT_UNBOUND='This server is not connected to a Clippet account.'
bt_answer unbound1 200 "$BT_UNBOUND"
# (d) The CLI round trip, and the binding CHANGING what the same signed interaction is answered.
contains "$BT_OUT" 'ls-empty: 0 binding(s)' || die "the fresh probe has bindings: $BT_OUT"
contains "$BT_OUT" 'bind: guild '"$BT_G1"' bound to botowner' || die "bind did not report the binding: $BT_OUT"
contains "$BT_OUT" 'ls-bound: '"$BT_G1"'  botowner' || die "ls does not list the binding: $BT_OUT"
bt_answer bound 200 'Attach a video to /upload.'
# (d2) The WITHDRAWN `link` option (§20 erratum 286), in the guild that IS bound. The command this
#      build publishes has ONE option and it is `required`, so Discord refuses an empty `/clip` in the
#      client — but the definition Discord STORES is whatever was last published, so a server on an
#      older registration can still send a `link`. It is answered by the ordinary line, decided on the
#      three-second response with no outbound call of any kind, and it is the SAME line an empty
#      `/clip` gets: one answer, so nothing here can be asked which links name a clip.
bt_answer linkold 200 'Attach a video to /upload.'
# (d3) `/settings` (§20 erratum 318; run 4's panel contract §4/§5). The box's own command answers the
#      one-row panel: type 4, IS_COMPONENTS_V2 | EPHEMERAL (32832), one Container holding the "Link
#      defaults" Section whose accessory is the `Edit` button `cb:defaults`, and no message content.
BT_L=$(bt_line settings)
contains "$BT_L" 'status=200' || die "/settings was not answered: $BT_L"
contains "$BT_L" '"flags":32832' || die "the /settings panel is not an ephemeral Components V2 message: $BT_L"
contains "$BT_L" '"type":17' || die "the /settings panel has no Container: $BT_L"
contains "$BT_L" '"custom_id":"cb:defaults"' || die "the /settings panel has no Edit button: $BT_L"
contains "$BT_L" '"label":"Edit"' || die "the /settings panel's button is not Edit: $BT_L"
contains "$BT_L" '"data":{"content"' && die "the panel carries message content beside its components: $BT_L"
# …the Edit click opens the MODAL (type 9) with the three Labels, pre-filled from the fresh account's
# defaults (no TTL preference, so the server-default radio is the one marked).
BT_L=$(bt_line edit)
contains "$BT_L" 'status=200' || die "the Edit click was not answered: $BT_L"
contains "$BT_L" '"type":9' || die "the Edit click did not open a modal: $BT_L"
contains "$BT_L" '"custom_id":"cb:defaults:modal"' || die "the modal's custom_id is not cb:defaults:modal: $BT_L"
contains "$BT_L" '"title":"Link defaults"' || die "the modal's title: $BT_L"
for bt_k in '"label":"Delete after"' '"label":"Unlisted"' '"label":"Link"' '"type":21' '"type":23' \
            '"default":true,"label":"server default' '"label":"never","value":"0"'; do
  contains "$BT_L" "$bt_k" || die "the modal lacks $bt_k: $BT_L"
done
# …from a member without Manage Server: one line, ephemeral (proof A).
bt_answer edit-nomanage 200 'You need Manage Server here.'
# …and the submit is `Changes saved.` (type 4, ephemeral) — and the account's defaults ARE what it saved, read
# through the settings page's own API.
bt_answer saved 200 'Changes saved.'
BT_L=$(bt_line defaults)
contains "$BT_L" 'status=200' || die "the bound account's settings could not be read back: $BT_L"
for bt_k in '"default_ttl_days":30' '"default_unlisted":true' '"default_link_style":"memorable"'; do
  contains "$BT_L" "$bt_k" || die "the modal's submit did not write $bt_k: $BT_L"
done
bt_answer unbound2 200 "$BT_UNBOUND"
contains "$BT_OUT" 'unbind: guild '"$BT_G1"' unbound' || die "unbind did not report: $BT_OUT"
contains "$BT_OUT" 'ls-after: 0 binding(s)' || die "the binding survived unbind: $BT_OUT"
# (e) …and after the unbind the SAME signed interaction is refused again, which is the whole point of
#     the binding: it is what decides, not the signature.
bt_answer unbound3 200 "$BT_UNBOUND"

# (f) The boot line names the application and the endpoint to paste, and never the token.
contains "$BT_OUT" "bootline:" || die "the probe logged no 'discord bot configured' line: $BT_OUT"
contains "$BT_OUT" "app_id=$BT_APP" || die "the boot line does not name the application: $BT_OUT"
contains "$BT_OUT" '/api/discord/interactions' || die "the boot line does not print the interactions URL: $BT_OUT"
contains "$BT_OUT" "$BT_TOKEN" && die "the bot TOKEN reached the probe's output"
contains "$BT_OUT" 'discord bot: on' || die "doctor does not report the bot as on: $BT_OUT"
# (g) The origin proof (§20 erratum 279): GET /api/bot/claim answers ONE bit per guild id, and the bit
#     is the binding. 404 with an EMPTY body before the bind, 200 with the boolean and the application
#     id after it, 404 again after the unbind; a malformed id is a 400 naming its rule, never a 404
#     that would read as "not bound". Anonymous: the claim page's Worker makes exactly this GET.
printf '%s\n' "$BT_OUT" | grep -q '^claim-unbound status=404 body=$' || die "the claim endpoint did not answer 404 with an empty body before the bind: $BT_OUT"
printf '%s\n' "$BT_OUT" | grep -q '^claim-after status=404 body=$'   || die "the claim endpoint did not answer 404 again after the unbind: $BT_OUT"
BT_CL=$(bt_line claim-bound)
contains "$BT_CL" 'status=200' || die "the claim endpoint did not answer 200 for the bound guild: $BT_CL"
contains "$BT_CL" '"bound":true' || die "the claim endpoint's 200 does not say bound: $BT_CL"
contains "$BT_CL" '"app_id":"'"$BT_APP"'"' || die "the claim endpoint does not echo the application id: $BT_CL"
contains "$BT_CL" '"defaults":{"link":"' || die "the claim endpoint's 200 does not carry the bound account's link defaults (erratum 318 amended): $BT_CL"
contains "$BT_CL" 'botowner' && die "the claim endpoint named the bound account: $BT_CL"
BT_CL=$(bt_line claim-badid)
contains "$BT_CL" 'status=400' || die "a malformed guild id was not a 400: $BT_CL"
contains "$BT_CL" 'invalid_guild_id' || die "the 400 does not name its rule: $BT_CL"
echo "[e2e]   401 on a bad signature (empty body), PONG on a signed PING, the one-line refusal for an unbound guild, bind/ls/unbind round trip, the withdrawn link option answered as no attachment with no fetch, no token in any output; GET /api/bot/claim 404/200/404 around the binding, 400 on a bad id; /settings: the panel, the Edit modal, Manage Server required, Changes saved. and the account's defaults changed"

# (h) The MANAGED sentinel (§20 erratum 279, DEPLOY §30.8): the official bot's instance sets
#     DISCORD_BOT_TOKEN=managed. A second, shorter probe container proves what that changes and what it
#     does not: the boot line says managed=true; `admin bot register` is refused in words (exit 2) with
#     no call anywhere; `bind` WITHOUT --no-register stores the binding, publishes nothing and says so;
#     the claim endpoint answers 200 for the bound server exactly as above; a signed `/clip` is still
#     answered by the binding (the credential is Discord's signature either way); and `admin doctor`'s
#     bot row says `token managed`, tells the operator to paste nothing, and lists the binding.
bt_now; BT_SIG_C1=$(bt_sign "$BT_C1")
BT_MOUT=$(dc run --rm -T --no-deps \
  -e DISCORD_APP_ID="$BT_APP" -e DISCORD_PUBLIC_KEY="$BT_PUB" -e DISCORD_BOT_TOKEN=managed \
  -e DATA_DIR=/tmp/botmanaged -e SCRATCH_DIR=/tmp/botmanaged-scratch -e PUBLIC_BASE_URL="$BT_ORIGIN" \
  -e DISCORD_SETTINGS_COMMAND_NAME= \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e BT_TS="$BT_TS" -e BT_C1="$BT_C1" -e BT_SIG_C1="$BT_SIG_C1" -e BT_G1="$BT_G1" -e BT_PW="$BT_PW" \
  --entrypoint sh app -c '
    set -e
    mkdir -p /tmp/botmanaged
    clippet serve >/tmp/boot.log 2>&1 &
    p=$!
    i=0
    while [ $i -lt 30 ]; do grep -q "msg=listening" /tmp/boot.log && break; i=$((i+1)); sleep 1; done
    grep -q "msg=listening" /tmp/boot.log || { echo "PROBE-DID-NOT-BOOT"; cat /tmp/boot.log; exit 1; }
    grep "discord bot configured" /tmp/boot.log | sed "s/^/bootline: /"
    EP=http://localhost:8080/api/discord/interactions
    CP=http://localhost:8080/api/bot/claim
    say() { printf "%s status=%s body=%s\n" "$1" "$2" "$3"; }
    printf "%s" "$BT_PW" | clippet admin users create botowner | sed "s/^/create: /"
    rc=0; clippet admin bot register >/tmp/reg.out 2>&1 || rc=$?
    printf "register rc=%s %s\n" "$rc" "$(tr -d "\n" < /tmp/reg.out)"
    clippet admin bot bind "$BT_G1" botowner | sed "s/^/bind: /"
    clippet admin bot ls | sed "s/^/ls: /"
    code=$(curl -sS -o /tmp/body -w "%{http_code}" "$CP?guild_id=$BT_G1"); say claim-managed "$code" "$(tr -d "\n" < /tmp/body)"
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -X POST -H "Content-Type: application/json" \
      -H "X-Signature-Ed25519: $BT_SIG_C1" -H "X-Signature-Timestamp: $BT_TS" --data-binary "$BT_C1" "$EP")
    say bound-managed "$code" "$(tr -d "\n" < /tmp/body)"
    clippet admin doctor 2>&1 | grep "^bot" | sed "s/^/doctor: /" || true
    kill "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  ' 2>&1) || die "the managed probe container failed: $BT_MOUT"
printf '%s\n' "$BT_MOUT" | sed -n 's/^/[e2e]   managed: /p'
contains "$BT_MOUT" 'managed=true' || die "the boot line does not say managed=true: $BT_MOUT"
contains "$BT_MOUT" 'register rc=2' || die "admin bot register was not refused (exit 2) on the managed instance: $BT_MOUT"
contains "$BT_MOUT" 'register rc=2 admin bot register: nothing to publish' || die "the refusal does not explain itself: $BT_MOUT"
contains "$BT_MOUT" 'HTTP 401' && die "the managed instance sent the sentinel to Discord and got a 401: $BT_MOUT"
contains "$BT_MOUT" 'bind: guild '"$BT_G1"' bound to botowner' || die "bind on the managed instance did not store the binding: $BT_MOUT"
contains "$BT_MOUT" 'nothing to publish' || die "bind on the managed instance does not say it publishes nothing: $BT_MOUT"
contains "$BT_MOUT" 'NOT published' && die "bind on the managed instance still points at register: $BT_MOUT"
contains "$BT_MOUT" 'ls: discord bot: managed' || die "ls does not say managed: $BT_MOUT"
BT_ML=$(printf '%s\n' "$BT_MOUT" | grep '^claim-managed ' | head -1)
contains "$BT_ML" 'status=200' || die "the claim endpoint on the managed instance did not answer 200: $BT_ML"
contains "$BT_ML" '"app_id":"'"$BT_APP"'"' || die "the managed claim answer does not echo the application id: $BT_ML"
BT_ML=$(printf '%s\n' "$BT_MOUT" | grep '^bound-managed ' | head -1)
contains "$BT_ML" 'status=200' || die "a signed /clip on the managed instance was not answered: $BT_ML"
contains "$BT_ML" 'Attach a video to /upload.' || die "the managed instance did not answer the bound guild's /upload by the binding: $BT_ML"
# …and its register refusal names all four commands the official application publishes (§20 errata 315, 318).
contains "$BT_MOUT" '/upload, /clip, Enrich and /settings are published once' || die "the managed register refusal does not name the four commands: $BT_MOUT"
contains "$BT_MOUT" 'token managed' || die "doctor does not say token managed: $BT_MOUT"
contains "$BT_MOUT" 'paste this into the Developer Portal' && die "doctor tells a managed instance to paste an endpoint URL: $BT_MOUT"
contains "$BT_MOUT" 'bound server(s): '"$BT_G1"' → botowner' || die "doctor does not list the bound server: $BT_MOUT"
contains "$BT_MOUT" 'redacted' && die "doctor calls the sentinel redacted: $BT_MOUT"
echo "[e2e]   managed: boot says managed=true, register refused in words, bind stores and publishes nothing, the claim endpoint and a signed /clip both follow the binding, doctor lists the bound server"

# (i) The command's NAME is a setting (§20 erratum 284, DEPLOY §30.2). A THIRD short container, renamed
#     to `/clip-dev`, proves the half a live stack CAN prove: the boot line and `admin doctor` name the
#     command, `bind --no-register` prints it, the renamed interaction is answered by the binding, and
#     the DEFAULT `/clip` is refused as an unknown command — which is the whole point on a box running a
#     dev instance beside a production one. The REGISTRATION body (what `admin bot register` PUTs) has
#     no witness here ON PURPOSE: this suite reaches Discord for nothing and has no fake Discord to
#     register against, so that half is pinned against an httptest server in internal/notify
#     (TestRegisterPublishesTheConfiguredCommandName), where the body can be read.
BT_NAME=clip-dev
BT_UNAME=upload-dev
BT_ENAME='Enrich (dev)'
BT_SNAME=settings-dev
bt_now; BT_SIG_C1=$(bt_sign "$BT_C1"); BT_SIG_ST=$(bt_sign "$BT_ST")
BT_STD='{"type":2,"token":"e2e-interaction-token","guild_id":"'$BT_G1'","member":{"permissions":"8"},"data":{"name":"'$BT_SNAME'","type":1}}'
BT_SIG_STD=$(bt_sign "$BT_STD")
BT_CD='{"type":2,"token":"e2e-interaction-token","guild_id":"'$BT_G1'","data":{"name":"'$BT_UNAME'"}}'
BT_SIG_CD=$(bt_sign "$BT_CD")
BT_NOUT=$(dc run --rm -T --no-deps \
  -e DISCORD_APP_ID="$BT_APP" -e DISCORD_PUBLIC_KEY="$BT_PUB" -e DISCORD_BOT_TOKEN="$BT_TOKEN" \
  -e DISCORD_COMMAND_NAME="$BT_NAME" -e DISCORD_UPLOAD_COMMAND_NAME="$BT_UNAME" -e DISCORD_ENRICH_COMMAND_NAME="$BT_ENAME" \
  -e DISCORD_SETTINGS_COMMAND_NAME="$BT_SNAME" -e BT_STD="$BT_STD" -e BT_SIG_STD="$BT_SIG_STD" -e BT_ST="$BT_ST" -e BT_SIG_ST="$BT_SIG_ST" \
  -e DATA_DIR=/tmp/botnamed -e SCRATCH_DIR=/tmp/botnamed-scratch -e PUBLIC_BASE_URL="$BT_ORIGIN" \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e BT_TS="$BT_TS" -e BT_C1="$BT_C1" -e BT_SIG_C1="$BT_SIG_C1" \
  -e BT_CD="$BT_CD" -e BT_SIG_CD="$BT_SIG_CD" -e BT_G1="$BT_G1" -e BT_PW="$BT_PW" \
  --entrypoint sh app -c '
    set -e
    mkdir -p /tmp/botnamed
    clippet serve >/tmp/boot.log 2>&1 &
    p=$!
    i=0
    while [ $i -lt 30 ]; do grep -q "msg=listening" /tmp/boot.log && break; i=$((i+1)); sleep 1; done
    grep -q "msg=listening" /tmp/boot.log || { echo "PROBE-DID-NOT-BOOT"; cat /tmp/boot.log; exit 1; }
    grep "discord bot configured" /tmp/boot.log | sed "s/^/bootline: /"
    EP=http://localhost:8080/api/discord/interactions
    say() { printf "%s status=%s body=%s\n" "$1" "$2" "$3"; }
    call() {
      code=$(curl -sS -o /tmp/body -w "%{http_code}" -X POST -H "Content-Type: application/json" \
        -H "X-Signature-Ed25519: $2" -H "X-Signature-Timestamp: $BT_TS" --data-binary "$3" "$EP")
      say "$1" "$code" "$(tr -d "\n" < /tmp/body)"
    }
    printf "%s" "$BT_PW" | clippet admin users create botowner | sed "s/^/create: /"
    clippet admin bot bind "$BT_G1" botowner --no-register | sed "s/^/bind: /"
    call renamed "$BT_SIG_CD" "$BT_CD"
    call default "$BT_SIG_C1" "$BT_C1"
    call settings-renamed "$BT_SIG_STD" "$BT_STD"
    call settings-default "$BT_SIG_ST" "$BT_ST"
    clippet admin doctor 2>&1 | grep "^bot" | sed "s/^/doctor: /" || true
    kill "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  ' 2>&1) || die "the renamed-command probe container failed: $BT_NOUT"
printf '%s\n' "$BT_NOUT" | sed -n 's/^/[e2e]   named: /p'
contains "$BT_NOUT" "command=/$BT_NAME" || die "the boot line does not name the link command: $BT_NOUT"
contains "$BT_NOUT" "upload_command=/$BT_UNAME" || die "the boot line does not name the upload command: $BT_NOUT"
contains "$BT_NOUT" "enrich_command=\"$BT_ENAME\"" || die "the boot line does not name the message command: $BT_NOUT"
contains "$BT_NOUT" "settings_command=/$BT_SNAME" || die "the boot line does not name the settings command: $BT_NOUT"
contains "$BT_NOUT" "bind: /$BT_UNAME, /$BT_NAME, $BT_ENAME, /$BT_SNAME and /signin were NOT published" || die "bind does not name the four renamed commands and /signin: $BT_NOUT"
BT_NL=$(printf '%s\n' "$BT_NOUT" | grep '^renamed ' | head -1)
contains "$BT_NL" 'status=200' || die "the renamed command was not answered: $BT_NL"
contains "$BT_NL" "Attach a video to /$BT_UNAME." || die "the renamed command was not answered by the binding, or the refusal names another command: $BT_NL"
BT_NL=$(printf '%s\n' "$BT_NOUT" | grep '^default ' | head -1)
contains "$BT_NL" 'Clippet does not know that command.' || die "a renamed instance still answered the default /upload: $BT_NL"
contains "$BT_NOUT" "doctor: " || die "the renamed probe printed no doctor bot row: $BT_NOUT"
contains "$BT_NOUT" "commands: /$BT_UNAME, /$BT_NAME, $BT_ENAME and /$BT_SNAME" || die "doctor does not print the four names: $BT_NOUT"
# The renamed /settings is answered with the panel (an ADMINISTRATOR member, "8"); the default one is not.
BT_NL=$(printf '%s\n' "$BT_NOUT" | grep '^settings-renamed ' | head -1)
contains "$BT_NL" '"custom_id":"cb:defaults"' || die "the renamed /$BT_SNAME was not answered with the panel: $BT_NL"
BT_NL=$(printf '%s\n' "$BT_NOUT" | grep '^settings-default ' | head -1)
contains "$BT_NL" 'Clippet does not know that command.' || die "a renamed instance still answered the default /settings: $BT_NL"

# (j) …and the ONE combination that is a boot error rather than a setting: the official bot publishes one
#     global /clip and forwards that command alone, so a MANAGED instance may not rename its own. The
#     container refuses to start and the sentence names the variable, the sentinel and §30.8 — it is the
#     erratum-279 refusal's twin, moved to boot time because nothing later could ever say it.
BT_MBOOT=$(dc run --rm -T --no-deps \
  -e DISCORD_APP_ID="$BT_APP" -e DISCORD_PUBLIC_KEY="$BT_PUB" -e DISCORD_BOT_TOKEN=managed \
  -e DISCORD_COMMAND_NAME="$BT_NAME" \
  -e DATA_DIR=/tmp/botmboot -e SCRATCH_DIR=/tmp/botmboot-scratch app serve 2>&1) && \
  die "a managed container with DISCORD_COMMAND_NAME=$BT_NAME booted: $BT_MBOOT"
contains "$BT_MBOOT" 'DISCORD_COMMAND_NAME' || die "the managed rename error does not name the variable: $BT_MBOOT"
contains "$BT_MBOOT" '§30.8' || die "the managed rename error does not point at the official-bot section: $BT_MBOOT"
printf '%s\n' "$BT_MBOOT" | sed -n 's/^/[e2e]   managed-boot: /p'
echo "[e2e]   named: /$BT_UNAME, /$BT_NAME, $BT_ENAME and /$BT_SNAME are on the boot line, in doctor and in bind's output; the renamed interactions are answered by the binding and the defaults are not; managed + a rename refuses to boot"

# (k) THE MESSAGE (§20 errata 315, 316): `/clip link:` and the message command "Enrich" answer at
#     once, PUBLICLY (type 4, no flags), with the share link ALONE — no embed of our own, because a bot
#     message carrying one never gets Discord's unfurl (measured) and the clip's PAGE now declares the
#     branded card Discord draws under the link (a component embed, CARD_COMPONENTS, on by default): the
#     ready page is fetched here too and carries that one data block with the ready colour and the clip's
#     social.mp4. A FIFTH container encodes one tiny clip of its own so there is something to point at: the
#     media lives in a private STORAGE_DIR (STORAGE_BACKEND=fs, the bucket variables unset), the webhook
#     variables are unset (the clip must not be posted to the operator's channel when it becomes ready),
#     and the sweep is an hour out so the clip expired by hand below is still a row when it is asked about.
#     It is driven with `docker exec` because the signatures are made HERE, on the host, after the clip's
#     id exists — the image carries no openssl to sign with.
BT_PORT=$(envfile CLIPPET_PUBLISH); BT_PORT=${BT_PORT##*:}; BT_PORT=${BT_PORT:-8481}
BT_CT="clippet-e2e-botcard-$BT_PORT"
bt_card_stop() { docker_ rm -f "$BT_CT" >/dev/null 2>&1 || true; }
bt_card_stop
trap 'bt_card_stop' EXIT
dc run -d --name "$BT_CT" --no-deps \
  -e DISCORD_APP_ID="$BT_APP" -e DISCORD_PUBLIC_KEY="$BT_PUB" -e DISCORD_BOT_TOKEN="$BT_TOKEN" \
  -e DISCORD_COMMAND_NAME= -e DISCORD_UPLOAD_COMMAND_NAME= -e DISCORD_ENRICH_COMMAND_NAME= -e DISCORD_SETTINGS_COMMAND_NAME= \
  -e DATA_DIR=/tmp/botcard -e SCRATCH_DIR=/tmp/botcard-scratch -e STORAGE_DIR=/tmp/botcard-media \
  -e STORAGE_BACKEND=fs -e PUBLIC_BASE_URL="$BT_ORIGIN" -e SWEEP_INTERVAL=1h -e SWEEP_GRACE=1h \
  --entrypoint sh app -c '
    set -e
    unset S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT
    unset DISCORD_WEBHOOK_URL DISCORD_WEBHOOK_ROUTES
    mkdir -p /tmp/botcard /tmp/botcard-media /tmp/botcard-in
    exec clippet serve >/tmp/boot.log 2>&1
  ' >/dev/null || die "could not start the enhanced-message probe container"
# bt_sh <shell line> — inside the probe, with the bucket, delivery and webhook variables unset exactly as
# its boot script unset them (`docker exec` inherits the environment the container was CREATED with).
bt_sh() { docker_ exec "$BT_CT" sh -c "unset S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT DISCORD_WEBHOOK_URL DISCORD_WEBHOOK_ROUTES; $1"; }
BT_N=0
until bt_sh 'grep -q "msg=listening" /tmp/boot.log' 2>/dev/null; do
  BT_N=$((BT_N + 1)); [ "$BT_N" -lt 60 ] || die "the enhanced-message probe did not boot: $(bt_sh 'cat /tmp/boot.log' 2>&1)"
  sleep 1
done
BT_BOOTED=$SECONDS   # the sweeper's FIRST tick is ten seconds after this boot (internal/sweeper bootDelay); see the expire below
bt_sh 'printf "%s" "'"$BT_PW"'" | clippet admin users create botowner >/dev/null && clippet admin bot bind '"$BT_G1"' botowner --no-register >/dev/null' \
  || die "the enhanced-message probe could not create and bind its account"
# Two seconds of ffmpeg's test pattern, named so its title (the filename without its extension, erratum
# 60) carries no markdown to escape.
bt_sh 'ffmpeg -hide_banner -nostdin -loglevel error -f lavfi -i testsrc=duration=2:size=320x240:rate=15 -pix_fmt yuv420p -y /tmp/botcard-in/probe.mp4' \
  || die "ffmpeg could not draw the probe clip inside the container"
BT_IMP=$(bt_sh 'clippet admin import /tmp/botcard-in --owner botowner' 2>&1) || die "admin import failed: $BT_IMP"
BT_CID=$(printf '%s\n' "$BT_IMP" | tr -d '\r' | sed -n 's/^imported \([A-Za-z0-9]*\) probe.mp4$/\1/p' | head -1)
[ -n "$BT_CID" ] || die "admin import printed no clip id: $BT_IMP"
# bt_post <label> <body> — sign HERE, post THERE, print one labelled line exactly as the probes above do.
bt_post() {
  bt_now; BT_SIG=$(bt_sign "$2")
  docker_ exec -e BODY="$2" -e SIG="$BT_SIG" -e TS="$BT_TS" "$BT_CT" sh -c '
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -X POST -H "Content-Type: application/json" \
      -H "X-Signature-Ed25519: $SIG" -H "X-Signature-Timestamp: $TS" --data-binary "$BODY" http://localhost:8080/api/discord/interactions)
    printf "%s status=%s body=%s\n" "'"$1"'" "$code" "$(tr -d "\n" < /tmp/body)"'
}
bt_link() {   # bt_link <link> -> a /clip link: body in the bound guild
  printf '{"type":2,"token":"e2e-interaction-token","guild_id":"%s","data":{"name":"clip","type":1,"options":[{"name":"link","type":3,"value":"%s"}]}}' "$BT_G1" "$1"
}
bt_enrich() {   # bt_enrich <message text> -> an Enrich body on one message carrying that text
  printf '{"type":2,"token":"e2e-interaction-token","guild_id":"%s","data":{"name":"Enrich","type":3,"target_id":"1500000000000000001","resolved":{"messages":{"1500000000000000001":{"id":"1500000000000000001","content":"%s","embeds":[],"attachments":[]}}}}}' "$BT_G1" "$1"
}
BT_LINK="$BT_ORIGIN/$BT_CID"
BT_ENC=$(bt_post encoding "$(bt_link "$BT_LINK")")
BT_N=0
until bt_sh "clippet admin ls $BT_CID | grep -q '^state: *ready'"; do
  BT_N=$((BT_N + 1)); [ "$BT_N" -lt 120 ] || die "the probe clip never became ready: $(bt_sh "clippet admin ls $BT_CID" 2>&1)"
  sleep 1
done
BT_RDY=$(bt_post ready "$(bt_link "<$BT_LINK>")")
BT_ENR=$(bt_post enrich "$(bt_enrich "look at this one: $BT_LINK")")
BT_FOREIGN=$(bt_post foreign "$(bt_link "https://clips.elsewhere.test/$BT_CID")")
BT_NOLINK=$(bt_post enrich-nolink "$(bt_enrich "gg wp")")
# The page is read BEFORE the expire: an expired clip's page is the 404 it should be.
# The page is fetched until it answers 200 (a loaded box can hold an SSR read past SSR_TIMEOUT, which is
# the §9.1 503 by design), and what it answered is kept so a failure says WHAT the page was.
BT_N=0
until BT_PCODE=$(bt_sh "curl -sS -o /tmp/page.html -w '%{http_code}' http://localhost:8080/$BT_CID" 2>&1) && [ "$BT_PCODE" = 200 ]; do
  BT_N=$((BT_N + 1)); [ "$BT_N" -lt 30 ] || die "the probe's share page never answered 200 (last: $BT_PCODE)"
  sleep 1
done
BT_PAGE=$(bt_sh 'cat /tmp/page.html')
# 30 minutes ago, inside SWEEP_GRACE (pinned to an hour on the probe): EXPIRED to the bot, which answers
# "gone" for any expires_at in the past, and NOT yet a sweep candidate. `--ago 1h` sat exactly on the
# grace, so the sweeper's first tick — ten seconds after boot, whatever SWEEP_INTERVAL says — destroyed
# the row whenever a loaded box reached this line before that tick, and the clip answered "not a link".
# SWEEP_GRACE does NOT protect it: the grace covers a PROCESSING clip only, and a READY clip past its
# expiry is a candidate at once (internal/db/clips.go, the sweep predicate). What protects it is the
# timing: with SWEEP_INTERVAL=1h the only tick inside this bundle is the FIRST, ten seconds after boot,
# so the expire waits until that tick is behind it (run 9, lane gatestack: a fast gate stack reached this
# line inside the ten seconds and the clip answered "That is not a link to a clip on this Clippet.").
while [ $((SECONDS - BT_BOOTED)) -lt 13 ]; do sleep 1; done
bt_sh "clippet admin expire $BT_CID --ago 30m >/dev/null" || die "admin expire failed on the probe clip"
BT_GONE=$(bt_post gone "$(bt_link "$BT_LINK")")
bt_card_stop
trap - EXIT
printf '%s\n' "$BT_ENC" "$BT_RDY" "$BT_ENR" "$BT_FOREIGN" "$BT_NOLINK" "$BT_GONE" | sed -n 's/^/[e2e]   card: /p'
# bt_card <line> — the message: the share link alone, in every state, and nothing else at all.
bt_card() {
  contains "$1" 'status=200' || die "the message was not a 200: $1"
  contains "$1" '"type":4' || die "the message is not an immediate message (no deferral): $1"
  contains "$1" '"flags"' && die "the message carries flags (it must be public): $1"
  contains "$1" '"data":{"content":"'"$BT_LINK"'","allowed_mentions":{"parse":[]}}' \
    || die "the message is not the share link alone with an empty mention list: $1"
  for bt_k in embeds components author color; do
    contains "$1" "\"$bt_k\"" && die "the message carries $bt_k (the page's card is the branding now): $1"
  done
}
# Encoding (or still queued), ready (asked by Discord's no-unfurl wrap), and Enrich: one shape.
bt_card "$BT_ENC"
bt_card "$BT_RDY"
bt_card "$BT_ENR"
# …and the page that link unfurls into declares the branded card: ONE component-embed data block, the
# ready green as its accent, the clip's own social.mp4 in its gallery and the Open button.
BT_NBLK=$(printf '%s' "$BT_PAGE" | grep -o '<script id="discord:component-embed" type="application/vnd.discord.component-embed+json">' | wc -l | tr -d ' ')
[ "$BT_NBLK" = 1 ] || die "the ready page carries $BT_NBLK component-embed blocks, want 1; its <head> starts: $(printf '%s' "$BT_PAGE" | tr -d '\n' | cut -c1-600)"
contains "$BT_PAGE" '{"component":{"type":17,"accent_color":949327,' || die "the page's card is not a Container with the ready green"
contains "$BT_PAGE" '"url":"'"$BT_ORIGIN"'/api/clips/'"$BT_CID"'/social.mp4"' || die "the page's card does not play the clip's social.mp4"
contains "$BT_PAGE" '"style":5,"url":"'"$BT_LINK"'","label":"Open"' || die "the page's card has no Open link button"
contains "$BT_FOREIGN" '"content":"That is not a link to a clip on this Clippet."' || die "a foreign link was not refused: $BT_FOREIGN"
contains "$BT_FOREIGN" '"flags":64' || die "the foreign-link refusal is not ephemeral: $BT_FOREIGN"
contains "$BT_NOLINK" '"content":"No link to a clip on this Clippet in that message."' || die "Enrich on a message with no link was not refused: $BT_NOLINK"
contains "$BT_NOLINK" '"flags":64' || die "the Enrich refusal is not ephemeral: $BT_NOLINK"
contains "$BT_GONE" '"content":"That clip is gone."' || die "an expired clip was not answered as gone: $BT_GONE"
contains "$BT_GONE" '"flags":64' || die "the gone refusal is not ephemeral: $BT_GONE"
echo "[e2e]   card: /clip link: and Enrich post the share link alone in every state, and the ready page declares the branded component card (green, social.mp4, Open); a foreign link, a message with no link of ours and an expired clip get their one-line refusals; probe container removed"

# (l) ENRICH ON A VIDEO (§20 erratum 323): with no link of ours in the text, Enrich uploads the message's
#     first video through `/upload`'s own path — for the message's poster or a member holding Administrator
#     or Manage Server only. A SIXTH probe, started like 98-form's: `docker run --network none` of the app's
#     own image (no compose service, so none of its volumes; a private DATA_DIR, SCRATCH_DIR and fs
#     STORAGE_DIR), because an accepted Enrich starts a REAL job whose fetch dials Discord's CDN — and the
#     suite reaches Discord for nothing. With no network every job ends in ONE `discord bot: /clip failed`
#     line, so the count of those lines IS the count of jobs started: a refused Enrich adds none, which is
#     the proof nothing was fetched. The fetch → ingest → link path with a successful fetch is pinned in Go
#     against httptest servers (internal/httpapi TestEnrichVideoWholePath); live, it is T-enrichvid on dev.
#     Legs: the poster's Enrich on a CDN-shaped video → `{"type":5}`, job 1; a stranger's → the one-line
#     refusal, no job; a manager's (Manage Server) on that same message → `{"type":5}`, job 2; a message
#     with a link to a clip here AND a video → the public link answer, no job; a video whose URL is not on
#     Discord's CDN → `{"type":5}`, job 3, refused by the allowlist before a socket ("not on an allowed
#     Discord CDN host"), exactly as `/upload` refuses one. Nothing of the people is on the log.
#     All of that is DISCORD_ENRICH_UPLOAD=on, which is OFF by default; so the probe is first booted WITHOUT
#     the variable and the poster's Enrich on the same video is the no-link line and starts no job — then it
#     is removed and booted again with the variable on for the legs above. (A second boot of the same tiny
#     probe is the cheapest non-vacuous place: the lane/reference stack's own app may run no bot at all.)
BT_VAPPCT=$(dc ps -q app 2>/dev/null | tr -d '\r' | head -1)
[ -n "$BT_VAPPCT" ] || die "the app container is not running"
BT_VIMG=$(docker_ inspect --format '{{.Config.Image}}' "$BT_VAPPCT" | tr -d '\r')
[ -n "$BT_VIMG" ] || die "could not read the app container's image"
BT_VCT="clippet-e2e-botvid-$BT_PORT"
bt_vid_stop() { docker_ rm -f "$BT_VCT" >/dev/null 2>&1 || true; }
bt_vid_stop
trap 'bt_vid_stop' EXIT
# bt_vid_start [<extra docker run args>...] — the probe, booted with no network, its account created and bound.
bt_vid_start() {
  docker_ run -d --name "$BT_VCT" --network none --label "com.docker.compose.project=${E2E_PROJECT:-}" --label com.docker.compose.oneoff=True \
    -e DISCORD_APP_ID="$BT_APP" -e DISCORD_PUBLIC_KEY="$BT_PUB" -e DISCORD_BOT_TOKEN="$BT_TOKEN" \
    -e PUBLIC_BASE_URL="$BT_ORIGIN" -e DATA_DIR=/tmp/botvid -e SCRATCH_DIR=/tmp/botvid-scratch \
    -e STORAGE_BACKEND=fs -e STORAGE_DIR=/tmp/botvid-media "$@" \
    --entrypoint sh "$BT_VIMG" -c 'mkdir -p /tmp/botvid /tmp/botvid-scratch /tmp/botvid-media /tmp/botvid-in && exec clippet serve >/tmp/boot.log 2>&1' \
    >/dev/null || die "could not start the Enrich-video probe (if the image cannot boot without a network, this leg must not run: it would otherwise reach Discord)"
  BT_N=0
  until docker_ exec "$BT_VCT" sh -c 'grep -q "msg=listening" /tmp/boot.log' 2>/dev/null; do
    BT_N=$((BT_N + 1)); [ "$BT_N" -lt 60 ] || die "the Enrich-video probe did not boot: $(docker_ exec "$BT_VCT" cat /tmp/boot.log 2>&1)"
    sleep 1
  done
  docker_ exec "$BT_VCT" sh -c 'printf "%s" "'"$BT_PW"'" | clippet admin users create botowner >/dev/null && clippet admin bot bind '"$BT_G1"' botowner --no-register >/dev/null' \
    || die "the Enrich-video probe could not create and bind its account"
}
BT_POSTER=100000000000000001
BT_OTHER=100000000000000002
# bt_evid <member id> <permissions> <text> <attachment url> -> Enrich on one message by BT_POSTER carrying
# that text and one video (Discord's shape: the member's user and permissions, the message's author and
# attachments under data.resolved.messages).
bt_evid() {
  printf '{"type":2,"token":"e2e-interaction-token","guild_id":"%s","member":{"permissions":"%s","user":{"id":"%s"}},"data":{"name":"Enrich","type":3,"target_id":"1500000000000000001","resolved":{"messages":{"1500000000000000001":{"id":"1500000000000000001","content":"%s","author":{"id":"%s"},"embeds":[],"attachments":[{"id":"1600000000000000001","filename":"enrich-probe.mp4","url":"%s","size":4096,"content_type":"video/mp4"}]}}}}}' \
    "$BT_G1" "$2" "$1" "$3" "$BT_POSTER" "$4"
}
bt_vpost() {   # bt_vpost <label> <body> — sign HERE, post THERE, one labelled line
  bt_now; BT_SIG=$(bt_sign "$2")
  docker_ exec -e BODY="$2" -e SIG="$BT_SIG" -e TS="$BT_TS" "$BT_VCT" sh -c '
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -X POST -H "Content-Type: application/json" \
      -H "X-Signature-Ed25519: $SIG" -H "X-Signature-Timestamp: $TS" --data-binary "$BODY" http://localhost:8080/api/discord/interactions)
    printf "%s status=%s body=%s\n" "'"$1"'" "$code" "$(tr -d "\n" < /tmp/body)"'
}
bt_vjobs() { docker_ exec "$BT_VCT" sh -c 'grep -c "discord bot: /clip failed" /tmp/boot.log || true' | tr -d '\r'; }
bt_vwait() {   # bt_vwait <n> — until exactly n jobs have ended
  BT_N=0
  until [ "$(bt_vjobs)" = "$1" ]; do
    BT_N=$((BT_N + 1)); [ "$BT_N" -lt 60 ] || die "the Enrich-video probe never ended job $1 (ended: $(bt_vjobs)): $(docker_ exec "$BT_VCT" cat /tmp/boot.log 2>&1)"
    sleep 1
  done
}
BT_VCDN=https://cdn.discordapp.com/attachments/1/2/enrich-probe.mp4
# OFF (the variable not passed at all — the shipped default): the poster's Enrich on a video is the no-link
# line, and no job ends in the two seconds after it (a job with no network fails at once).
bt_vid_start
BT_V0=$(bt_vpost off "$(bt_evid "$BT_POSTER" 2147601473 'gg' "$BT_VCDN")")
sleep 2
BT_V0N=$(bt_vjobs)
BT_V0LOG=$(docker_ exec "$BT_VCT" cat /tmp/boot.log 2>&1)
BT_V0DOC=$(docker_ exec "$BT_VCT" sh -c 'clippet admin doctor 2>&1 | grep "enrich upload"' 2>&1 || true)
bt_vid_stop
# ON: the legs above, on a fresh boot of the same probe.
bt_vid_start -e DISCORD_ENRICH_UPLOAD=on
# One clip of its own, so a link in the message names something here (any state answers: the link wins).
docker_ exec "$BT_VCT" sh -c 'ffmpeg -hide_banner -nostdin -loglevel error -f lavfi -i testsrc=duration=1:size=160x120:rate=10 -pix_fmt yuv420p -y /tmp/botvid-in/vidprobe.mp4' \
  || die "ffmpeg could not draw the Enrich-video probe's clip"
BT_VIMP=$(docker_ exec "$BT_VCT" clippet admin import /tmp/botvid-in --owner botowner 2>&1) || die "admin import failed in the Enrich-video probe: $BT_VIMP"
BT_VCID=$(printf '%s\n' "$BT_VIMP" | tr -d '\r' | sed -n 's/^imported \([A-Za-z0-9]*\) vidprobe.mp4$/\1/p' | head -1)
[ -n "$BT_VCID" ] || die "admin import printed no clip id in the Enrich-video probe: $BT_VIMP"
BT_V1=$(bt_vpost poster "$(bt_evid "$BT_POSTER" 2147601473 'gg' "$BT_VCDN")")
bt_vwait 1
BT_V2=$(bt_vpost stranger "$(bt_evid "$BT_OTHER" 2147601473 'gg' "$BT_VCDN")")
BT_V3=$(bt_vpost manager "$(bt_evid "$BT_OTHER" 32 'gg' "$BT_VCDN")")
bt_vwait 2
BT_V4=$(bt_vpost linkwins "$(bt_evid "$BT_POSTER" 2147601473 "see $BT_ORIGIN/$BT_VCID" "$BT_VCDN")")
BT_V5=$(bt_vpost offcdn "$(bt_evid "$BT_POSTER" 2147601473 'gg' 'https://attachments.example/enrich-probe.mp4')")
bt_vwait 3
sleep 1   # one more second for a late job the stranger or the link leg might have started
BT_VLOG=$(docker_ exec "$BT_VCT" cat /tmp/boot.log 2>&1)
BT_VN=$(bt_vjobs)
BT_VDOC=$(docker_ exec "$BT_VCT" sh -c 'clippet admin doctor 2>&1 | grep "enrich upload"' 2>&1 || true)
bt_vid_stop
trap - EXIT
printf '%s\n' "$BT_V0" "$BT_V1" "$BT_V2" "$BT_V3" "$BT_V4" "$BT_V5" | sed -n 's/^/[e2e]   enrich-video: /p'
contains "$BT_V0LOG" 'enrich_upload=off' || die "the boot line without the variable does not say enrich_upload=off: $BT_V0LOG"
contains "$BT_V0" '"content":"No link to a clip on this Clippet in that message."' || die "with DISCORD_ENRICH_UPLOAD unset, a video Enrich was not the no-link line: $BT_V0"
contains "$BT_V0" '"flags":64' || die "the off answer is not ephemeral: $BT_V0"
[ "$BT_V0N" = 0 ] || die "with DISCORD_ENRICH_UPLOAD unset, a video Enrich started $BT_V0N job(s)"
contains "$BT_VLOG" 'enrich_upload=on' || die "the boot line with the variable does not say enrich_upload=on"
# …and `admin doctor`'s bot check says which, in its own words.
contains "$BT_V0DOC" 'enrich upload: off - Enrich answers links only; DISCORD_ENRICH_UPLOAD=on lets it upload a video' \
  || die "doctor's bot check does not print the off line: $BT_V0DOC"
contains "$BT_VDOC" 'enrich upload: on - Enrich on a message with a video uploads it, for its poster or Manage Server; DISCORD_ENRICH_UPLOAD=off stops it' \
  || die "doctor's bot check does not print the on line: $BT_VDOC"
[ "$BT_V1" = 'poster status=200 body={"type":5}' ] || die "the poster's Enrich on a video was not deferred: $BT_V1"
contains "$BT_V2" 'status=200' || die "the stranger's Enrich was not answered: $BT_V2"
contains "$BT_V2" '"content":"That video is not yours. You need Manage Server here."' || die "the stranger's Enrich was not refused: $BT_V2"
contains "$BT_V2" '"flags":64' || die "the stranger's refusal is not ephemeral: $BT_V2"
[ "$BT_V3" = 'manager status=200 body={"type":5}' ] || die "a manager's Enrich on another's video was not deferred: $BT_V3"
contains "$BT_V4" '"data":{"content":"'"$BT_ORIGIN/$BT_VCID"'","allowed_mentions":{"parse":[]}}' || die "a message with a link and a video was not answered with the link: $BT_V4"
contains "$BT_V4" '"flags"' && die "the link answer is not public: $BT_V4"
[ "$BT_V5" = 'offcdn status=200 body={"type":5}' ] || die "an off-CDN video was not deferred as /upload defers it: $BT_V5"
[ "$BT_VN" = 3 ] || die "the Enrich-video probe started $BT_VN job(s), want 3 (poster, manager, off-CDN — the stranger and the link leg start none)"
[ "$(printf '%s\n' "$BT_VLOG" | grep 'discord bot: /clip failed' | grep -c 'not on an allowed Discord CDN host')" = 1 ] \
  || die "the off-CDN attachment was not refused by the allowlist (exactly one such failure line): $BT_VLOG"
# Nothing of the people, no secret and no attachment URL anywhere on the log; and the ACCESS lines of the
# interactions carry no CDN host either (a job's fetch-failure line may name the host it could not reach, as
# `/upload`'s does, and the boot line lists the allowed hosts — so the host is checked where Enrich logs).
for bt_k in "$BT_POSTER" "$BT_OTHER" "$BT_TOKEN" e2e-interaction-token attachments/1/2/enrich-probe.mp4 attachments.example; do
  contains "$BT_VLOG$BT_V0LOG" "$bt_k" && die "$bt_k reached the Enrich-video probe's log"
done
BT_VACC=$(printf '%s\n' "$BT_VLOG" "$BT_V0LOG" | grep 'discord=enrich' || true)
[ -n "$BT_VACC" ] || die "the Enrich-video probe logged no access line for Enrich"
contains "$BT_VACC" 'cdn.discordapp.com' && die "the CDN host reached an Enrich access line: $BT_VACC"
echo "[e2e]   enrich-video: with DISCORD_ENRICH_UPLOAD unset a video Enrich is the no-link line and starts nothing; with it on, the poster's and a manager's Enrich on a video started the upload (deferred), a stranger's was refused and started nothing, a link of ours beside a video was answered with the link and uploaded nothing, an off-CDN video was refused by the allowlist; no member or author id on the log; probe container removed"

step 86 "the off state: no variables, no route, no bot — and one variable alone refuses to boot"

# (a)+(b) The stack under test may run WITHOUT a bot (a lane stack, the shipped default: no DISCORD_* at
#     all) or WITH one (the reference stack since 2026-09-11 — the owner's own application in its
#     .env). Both are real shapes and the harness must not assume either (no origin-dependent
#     assertions): `admin doctor` says which, and the route is asserted accordingly. OFF: the route does
#     not exist, so it is a 404 from the catch-all exactly like any other unknown path, and doctor says
#     so in words an operator can grep for. ON: the route exists and an unsigned POST is refused with
#     the 401 step 85 already proved in detail. The OFF shape of the process itself is proven by the boot
#     probe in (c) on either stack.
dc exec -T app clippet admin doctor > "$E2E_DIR/bot-doctor.txt" 2>&1 || die "admin doctor failed"
code=$(http_refused POST "$BASE/api/discord/interactions" -H 'Content-Type: application/json' -d '{"type":1}')
if grep -q 'discord bot: on' "$E2E_DIR/bot-doctor.txt"; then
  expect "$code" 401 "POST /api/discord/interactions unsigned, on a stack that runs a bot"
  echo "[e2e]   this stack runs a bot (doctor: on): the route exists and refuses an unsigned POST"
else
  expect "$code" 404 "POST /api/discord/interactions with the bot off"
  expect "$(json error)" "not_found" "…and it is the router's own 404, not a handler's refusal"
  has_file "$E2E_DIR/bot-doctor.txt" 'discord bot: off' "doctor reports the bot as off"
  has_file "$E2E_DIR/bot-doctor.txt" 'is not served' "…and says the route is not served"
fi
# (c) One of the three set without the others is a BOOT ERROR naming all three: a half-configured bot
#     is a silently dead one, and silence is the worst way for an integration to fail (§4).
#     The other two are pinned EMPTY: a stack whose .env carries a real bot would otherwise hand the
#     probe all three and it would boot (empty = unset for these three, exactly as the shipped compose
#     file passes them).
BT_BOOT=$(dc run --rm -T --no-deps -e DISCORD_APP_ID=987654321098765432 \
  -e DISCORD_PUBLIC_KEY= -e DISCORD_BOT_TOKEN= \
  -e DATA_DIR=/tmp/botboot -e SCRATCH_DIR=/tmp/botboot-scratch app serve 2>&1) && \
  die "a container with only DISCORD_APP_ID set booted: $BT_BOOT"
contains "$BT_BOOT" 'DISCORD_APP_ID' || die "the boot error does not name DISCORD_APP_ID: $BT_BOOT"
contains "$BT_BOOT" 'DISCORD_PUBLIC_KEY' || die "the boot error does not name DISCORD_PUBLIC_KEY: $BT_BOOT"
contains "$BT_BOOT" 'DISCORD_BOT_TOKEN' || die "the boot error does not name DISCORD_BOT_TOKEN: $BT_BOOT"
printf '%s\n' "$BT_BOOT" | sed -n 's/^/[e2e]   boot-probe: /p'
# (d) DISCORD_COMMAND_NAME is the same kind of orphan (§20 erratum 284): it names a command on an
#     instance that publishes none and answers none, so it refuses to boot too, naming itself and the
#     three it needs. The value is deliberately the DEFAULT one — the fault is that nothing would ever
#     read it, not that it is misspelt.
BT_NBOOT=$(dc run --rm -T --no-deps -e DISCORD_APP_ID= -e DISCORD_PUBLIC_KEY= -e DISCORD_BOT_TOKEN= \
  -e DISCORD_COMMAND_NAME=clip \
  -e DATA_DIR=/tmp/botnboot -e SCRATCH_DIR=/tmp/botnboot-scratch app serve 2>&1) && \
  die "a container with DISCORD_COMMAND_NAME and no bot booted: $BT_NBOOT"
contains "$BT_NBOOT" 'DISCORD_COMMAND_NAME' || die "the orphan error does not name DISCORD_COMMAND_NAME: $BT_NBOOT"
contains "$BT_NBOOT" 'DISCORD_APP_ID' || die "the orphan error does not name the three it needs: $BT_NBOOT"
printf '%s\n' "$BT_NBOOT" | sed -n 's/^/[e2e]   name-boot-probe: /p'
echo "[e2e]   the route matches what doctor says of this stack; with one of three, or with a command name and no bot: the process refuses to boot and names what is missing"
