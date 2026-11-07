# 106-botsignin.sh — `/signin`, the bot is the door (DESIGN.md §20 erratum 341): a member of a bound
# server runs `/signin`, gets a one-time link, and the link's page signs them in. Step 106. Sourced by
# scripts/e2e.sh (see scripts/e2e.d/README.md). Every name here is SG_-prefixed.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=SG_ buckets=-
# e2e-covers: internal/httpapi/discord_signin.go internal/web/signin.go web/templates/signin.html
# e2e-covers: internal/db/signin.go internal/ids/ids.go internal/auth/auth.go internal/httpx/logpath.go
#   ONE throwaway `dc run --rm` probe, 85-bot's first shape: its own DATA_DIR and SCRATCH_DIR, its own
#   loopback, nothing published, REGISTRATION=members, a keypair the harness mints itself (openssl
#   ed25519, Discord's algorithm) — the suite has no Discord and needs none, because the credential is
#   the signature and the box verifies it. Its accounts live in the probe's private database and never
#   on this stack. Parallel for 85-bot's reason: no clip, no account, no setting of THIS stack, and no
#   instance-wide assertion.
#
# 106 a signed `/signin` from a member of the bound server → the ephemeral reply (flags 68: ephemeral +
#     SUPPRESS_EMBEDS) with the owner's sentence and the link on its own line in `<…>`; two GETs of the link
#     (the crawler's, then the member's) render the page and change nothing — the second one is still the
#     page; a cross-site POST is refused and spends nothing; the POST signs in (303 to /, a session cookie
#     that /api/auth/me answers as the new account `mal_lory`, linked by id); a second POST is refused
#     (303 to /login's expired line, no cookie); `admin users` lists the account with its Discord id; and
#     neither the token nor the member's id reaches the probe's log.

step 106 "/signin (#341): a signed /signin in a bound server answers a private one-time link; GETs of it change nothing; a cross-site POST is refused; the POST signs in as a new linked account; a second POST is refused; no token on the log"

command -v openssl >/dev/null 2>&1 || die "openssl is not on PATH; this step mints its own ed25519 keypair with it"
command -v xxd >/dev/null 2>&1 || die "xxd is not on PATH; this step needs it to render keys and signatures as hex"

SG_DIR="$E2E_DIR/botsignin"; rm -rf "$SG_DIR"; mkdir -p "$SG_DIR"
openssl genpkey -algorithm ed25519 -out "$SG_DIR/key.pem" >/dev/null 2>&1 || die "openssl could not mint an ed25519 key"
SG_PUB=$(openssl pkey -in "$SG_DIR/key.pem" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p | tr -d '\n\r')
[ "${#SG_PUB}" = 64 ] || die "the minted public key is ${#SG_PUB} hex characters, want 64"
SG_TS=$(date +%s)   # the CLOCK's: the box admits a timestamp within ±300 s of its own (§20 erratum 279 (g), amended)
sg_sign() {
  printf '%s%s' "$SG_TS" "$1" > "$SG_DIR/msg.bin"
  openssl pkeyutl -sign -rawin -inkey "$SG_DIR/key.pem" -in "$SG_DIR/msg.bin" 2>/dev/null | xxd -p | tr -d '\n\r'
}
SG_G=222222222222222206
SG_MEMBER=555555555555555506
SG_BODY='{"type":2,"token":"e2e-signin-token","guild_id":"'$SG_G'","member":{"permissions":"2147601473","roles":[],"user":{"id":"'$SG_MEMBER'","username":"mal.lory","global_name":"Mallory"}},"data":{"name":"signin","type":1}}'
SG_SIG=$(sg_sign "$SG_BODY")
[ "${#SG_SIG}" = 128 ] || die "the signature is ${#SG_SIG} hex characters, want 128"
SG_APP=987654321098765406
SG_TOKEN=MTIzNDU2Nzg5MDEyMzQ1Njc4.GhIjKl.mNoPqRsTuVwXyZ0123456789abcdef
SG_PW=e2e-signin-password-long-enough

SG_OUT=$(dc run --rm -T --no-deps \
  -e DISCORD_APP_ID="$SG_APP" -e DISCORD_PUBLIC_KEY="$SG_PUB" -e DISCORD_BOT_TOKEN="$SG_TOKEN" \
  -e DISCORD_COMMAND_NAME= -e DISCORD_UPLOAD_COMMAND_NAME= -e DISCORD_ENRICH_COMMAND_NAME= -e DISCORD_SETTINGS_COMMAND_NAME= \
  -e DISCORD_AUTO_MODE= -e DISCORD_AUTO_TOKEN= -e DISCORD_LOGIN_CLIENT_ID= -e DISCORD_LOGIN_CLIENT_SECRET= \
  -e REGISTRATION=members \
  -e DATA_DIR=/tmp/signinprobe -e SCRATCH_DIR=/tmp/signinprobe-scratch -e PUBLIC_BASE_URL=http://localhost:8080 \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e SG_TS="$SG_TS" -e SG_BODY="$SG_BODY" -e SG_SIG="$SG_SIG" -e SG_G="$SG_G" -e SG_PW="$SG_PW" \
  --entrypoint sh app -c '
    set -e
    mkdir -p /tmp/signinprobe
    clippet serve >/tmp/boot.log 2>&1 &
    p=$!
    i=0
    while [ $i -lt 30 ]; do grep -q "msg=listening" /tmp/boot.log && break; i=$((i+1)); sleep 1; done
    grep -q "msg=listening" /tmp/boot.log || { echo "PROBE-DID-NOT-BOOT"; cat /tmp/boot.log; exit 1; }
    O=http://localhost:8080   # named O and not U: the harness owns the one-letter globals (85-bot names EP for the same reason)
    printf "%s" "$SG_PW" | clippet admin users create serverowner >/dev/null
    clippet admin bot bind "$SG_G" serverowner --no-register >/dev/null
    code=$(curl -sS -o /tmp/reply -w "%{http_code}" -X POST -H "Content-Type: application/json" \
      -H "X-Signature-Ed25519: $SG_SIG" -H "X-Signature-Timestamp: $SG_TS" --data-binary "$SG_BODY" "$O/api/discord/interactions")
    # The token is printed NOWHERE: the transcript gets the reply with it replaced.
    printf "reply status=%s body=%s\n" "$code" "$(tr -d "\n" < /tmp/reply | sed "s/cs_[A-Za-z0-9_-]*/cs_{token}/g")"
    L=$(sed -n "s|.*\(/login/discord/cs_[A-Za-z0-9_-]*\).*|\1|p" /tmp/reply | head -1)
    [ -n "$L" ] || { echo "NO-LINK"; exit 1; }
    for g in get1 get2; do
      code=$(curl -sS -o /tmp/page -w "%{http_code}" "$O$L")
      grep -q "Sign in to localhost:8080 as Mallory" /tmp/page && pg=yes || pg=no
      printf "%s status=%s page=%s\n" "$g" "$code" "$pg"
    done
    post() {   # post <label> [<extra curl args>] -> status, Location and whether a cookie was set
      code=$(curl -sS -o /dev/null -D /tmp/h -w "%{http_code}" -X POST --data "next=/" "$@" "$O$L")
      loc=$(tr -d "\r" < /tmp/h | sed -n "s/^[Ll]ocation: //p" | head -1)
      grep -qi "^set-cookie:" /tmp/h && ck=yes || ck=no
      printf "%s status=%s location=%s cookie=%s\n" "$PL" "$code" "$loc" "$ck"
    }
    PL=xsite post -H "Origin: https://evil.example"
    PL=post1 post -H "Origin: $O"
    cookie=$(tr -d "\r" < /tmp/h | grep -i "^set-cookie:" | head -1 | sed "s/^[^:]*: *//; s/;.*//")
    code=$(curl -sS -o /tmp/me -w "%{http_code}" -H "Cookie: $cookie" "$O/api/auth/me")
    printf "me status=%s body=%s\n" "$code" "$(tr -d "\n" < /tmp/me)"
    PL=post2 post -H "Origin: $O"
    clippet admin users | sed "s/^/users: /"
    printf "logtoken=%s logmember=%s logpath=%s\n" "$(grep -cE "cs_[A-Za-z0-9_-]{43}" /tmp/boot.log || true)" \
      "$(grep -c "555555555555555506" /tmp/boot.log || true)" "$(grep -c "/login/discord/{token}" /tmp/boot.log || true)"
    kill "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  ' 2>&1) || die "the /signin probe container failed: $SG_OUT"

printf '%s\n' "$SG_OUT" | sed -n 's/^/[e2e]   probe: /p'
# A probe line is `<label> …` or `<label>=…` (the log counts are the second shape).
sg_line() { printf '%s\n' "$SG_OUT" | grep "^$1[ =]" | head -1; }
SG_L=$(sg_line reply)
contains "$SG_L" 'status=200' || die "/signin was not answered: $SG_L"
contains "$SG_L" '"flags":68' || die "the /signin reply is not ephemeral + SUPPRESS_EMBEDS (68): $SG_L"
# Go's encoder writes the brackets as \u003c and \u003e (json.Marshal escapes them); Discord reads them back as < and >.
contains "$SG_L" '"content":"Sign in to localhost:8080 · this link works once, for five minutes.\n\u003chttp://localhost:8080/login/discord/cs_{token}\u003e"' \
  || die "the /signin reply is not the sentence and the bracketed link: $SG_L"
[ "$(sg_line get1)" = 'get1 status=200 page=yes' ] || die "the first GET of the link was not the page: $(sg_line get1)"
[ "$(sg_line get2)" = 'get2 status=200 page=yes' ] || die "the second GET of the link was not the page (a GET consumed it): $(sg_line get2)"
[ "$(sg_line xsite)" = 'xsite status=403 location= cookie=no' ] || die "a cross-site POST was not refused: $(sg_line xsite)"
[ "$(sg_line post1)" = 'post1 status=303 location=/ cookie=yes' ] || die "the POST did not sign in: $(sg_line post1)"
SG_L=$(sg_line me)
contains "$SG_L" 'status=200' || die "the sign-in cookie does not open /api/auth/me: $SG_L"
contains "$SG_L" '"username":"mal_lory"' || die "the new account is not mal_lory: $SG_L"
[ "$(sg_line post2)" = 'post2 status=303 location=/login?discord=link cookie=no' ] || die "a second POST was not refused: $(sg_line post2)"
printf '%s\n' "$SG_OUT" | grep '^users: ' | grep -q "mal_lory .*discord=$SG_MEMBER" || die "admin users does not list mal_lory linked to the member: $SG_OUT"
SG_L=$(sg_line logtoken)
contains "$SG_L" 'logtoken=0 logmember=0' || die "the token or the member's id reached the probe's log: $SG_L"
contains "$SG_L" 'logpath=0' && die "the probe's log has no redacted link path at all (the access lines are missing): $SG_L"
echo "[e2e]   /signin: a private one-time link (flags 68, the link in <…>); two GETs rendered the page and spent nothing; a cross-site POST was refused; the POST signed in as mal_lory, linked by id; the second POST landed on /login's expired line; no token and no member id on the log"
