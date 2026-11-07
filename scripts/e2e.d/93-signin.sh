# 93-signin.sh — ROADMAP #44 (DESIGN.md §20 erratum 302): "Sign in with Discord" on an instance —
# the start, the callback's refusals, the Settings panel, the CLI link matrix, the doctor row and the
# OFF state. Step 93. Sourced by scripts/e2e.sh (see scripts/e2e.d/README.md). Every name here is
# SI_-prefixed.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=SI_ buckets=-
# e2e-covers: internal/httpapi/discord_login.go internal/config/discordlogin.go internal/notify/login.go
# e2e-covers: internal/db/users.go web/templates/login.html web/templates/settings.html
#   Two throwaway containers, each on its own DATA_DIR and its own SCRATCH_DIR, neither publishing a
#   port — they curl themselves on loopback. On the live stack it READS two things (`admin doctor`'s
#   `login` row and /login) and POSTs nothing; it creates no clip and no user there, mutates no shared
#   clip and no account-wide setting, runs no `dc up`, probes no limiter and asserts nothing
#   instance-wide. The same shape as 85-bot, which it copies.
#
# WHAT THIS CAN AND CANNOT PROVE (erratum 257's rule, applied). The suite has no Discord, so nothing
# below completes an exchange: a callback carrying a good state AND a code would make the app POST
# to Discord's token endpoint, and an E2E that reached it would be testing Discord's uptime while
# adding an egress the product does not have. Everything answerable BEFORE the exchange is proven
# here with no outbound call anywhere — the button, the authorize URL and the flow cookie, every
# refusal a callback can make before step 7 of the checklist, the replay, the CLI matrix, the panel,
# the doctor row, the OFF state and the half-configured boot error. The exchange itself and every
# outcome past it (unlinked under every REGISTRATION, a linked login's session, link/taken, the
# secret's absence from every string) are pinned against httptest in internal/notify/login_test.go
# and internal/httpapi/discord_login_test.go, where a fake Discord can be read; the LIVE proof is
# the operator's own run with a real Discord application.
#
# The flow cookie is the browser-side HALF of `state` + PKCE and there is no server-side table, so
# the harness can hold it the way a browser does: the first container mints it (its own
# /api/auth/discord/start), the HOST derives the `state` Discord would echo (a pure function of the
# nonce — base64url(sha256("clippet-discord-state." ‖ nonce)), openssl on the host, exactly the
# derivation the app performs), and a SECOND container with the same configuration answers the
# callback for it. That is not a shortcut: it is the property the design chose (erratum 302 (c)).
# The cookie's third segment is a MAC under a key derived from the client secret (erratum 302 (h)):
# the second container verifies it because it holds the same secret, and (h) below also sends a
# hand-built link cookie and a tampered real one — the one shape the harness sends WITH a good state
# and a code — which must be refused at step 2 before the exchange exists (`outbound: 0` proves it).

step 93 "Sign in with Discord: the button, the authorize URL and the flow cookie; a callback with no cookie, a wrong state, Discord's own refusal and a replay are refused with no outbound call; the Settings panel follows the link; admin users link/unlink refuse a second link on either side; doctor prints the redirect URI and never the secret; the OFF state; one variable alone refuses to boot"

command -v openssl >/dev/null 2>&1 || die "openssl is not on PATH; this step derives the OAuth state with it"

SI_ID=444444444444444444          # the Discord user linked during this step
SI_ID2=555555555555555555         # a second id, for the refusals
SI_CLIENT=876543210987654321      # a client-id-SHAPED value: nothing below reaches Discord
SI_SECRET=e2eSecretThatMustNeverPrint0123   # a secret-SHAPED value, passed as a variable, grepped for as absent
SI_PW=e2e-signin-password-long-enough
SI_OUT_FILE="$E2E_DIR/signin-probe.txt"

# (a) One container, one boot, most of the assertions. PUBLIC_BASE_URL is the container's OWN loopback
#     origin, so the redirect URI the start mints and the doctor prints is the one Discord would accept
#     for a dev box, and the doctor's `login` row is `ok` rather than the plain-http warning. The media
#     variables are pinned for erratum 280's reason: a probe with no viewers has nothing to redirect to.
SI_OUT=$(dc run --rm -T --no-deps \
  -e DISCORD_LOGIN_CLIENT_ID="$SI_CLIENT" -e DISCORD_LOGIN_CLIENT_SECRET="$SI_SECRET" \
  -e DATA_DIR=/tmp/signin -e SCRATCH_DIR=/tmp/signin-scratch -e PUBLIC_BASE_URL=http://localhost:8080 \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e REGISTRATION=closed \
  -e SI_ID="$SI_ID" -e SI_ID2="$SI_ID2" -e SI_PW="$SI_PW" -e SI_SECRET="$SI_SECRET" \
  --entrypoint sh app -c '
    set -e
    mkdir -p /tmp/signin
    clippet serve >/tmp/boot.log 2>&1 &
    p=$!
    i=0
    while [ $i -lt 30 ]; do grep -q "msg=listening" /tmp/boot.log && break; i=$((i+1)); sleep 1; done
    grep -q "msg=listening" /tmp/boot.log || { echo "PROBE-DID-NOT-BOOT"; cat /tmp/boot.log; exit 1; }
    grep "discord login configured" /tmp/boot.log | sed "s/^/bootline: /"
    SB=http://localhost:8080   # named SB and not B: check.sh greps this directory for the one-letter globals the harness owns
    say() { printf "%s status=%s body=%s\n" "$1" "$2" "$3"; }
    nav() {   # nav <label> [curl args…] <url> -> status + Location, headers left in /tmp/hdr
      l=$1; shift
      code=$(curl -sS -o /tmp/body -D /tmp/hdr -w "%{http_code}" "$@")
      printf "%s status=%s location=%s\n" "$l" "$code" "$(sed -n "s/^[Ll]ocation: *//p" /tmp/hdr | tr -d "\r")"
    }
    # The login page carries the anchor, with next.
    code=$(curl -sS -o /tmp/body -w "%{http_code}" "$SB/login?next=/settings")
    say login-page "$code" "$(grep -c "data-discord-start" /tmp/body)"
    grep -o "href=\"/api/auth/discord/start?next=[^\"]*\"" /tmp/body | head -1 | sed "s/^/login-href: /"
    # The start: a 302 to Discord with the flow cookie.
    nav start "$SB/api/auth/discord/start?next=/settings"
    grep -i "^set-cookie: clippet_session_discord=" /tmp/hdr | tr -d "\r" | sed "s/^/start-cookie: /"
    grep -i "^cache-control:" /tmp/hdr | tr -d "\r" | sed "s/^/start-cache: /"
    SI_C=$(grep -i "^set-cookie: clippet_session_discord=" /tmp/hdr | head -1 | sed "s/^[^=]*=//; s/;.*//" | tr -d "\r")
    printf "flow-cookie: %s\n" "$SI_C"
    # A cross-site navigation may not launch a flow.
    nav xsite -H "Sec-Fetch-Site: cross-site" "$SB/api/auth/discord/start?next=/settings"
    printf "xsite-cookie: %s\n" "$(grep -ci "^set-cookie: clippet_session_discord=" /tmp/hdr || true)"
    # The callback with no cookie, and with the cookie but the wrong state — no exchange either way.
    nav cb-nocookie "$SB/api/auth/discord/callback?code=x&state=y"
    nav cb-badstate -b "clippet_session_discord=$SI_C" "$SB/api/auth/discord/callback?code=x&state=notthestate"
    grep -i "^set-cookie: clippet_session_discord=" /tmp/hdr | tr -d "\r" | sed "s/^/badstate-cookie: /"
    # Two accounts (REGISTRATION=closed: the CLI is the door), one signed in.
    printf "%s" "$SI_PW" | clippet admin users create signinuser | sed "s/^/create: /"
    printf "%s" "$SI_PW" | clippet admin users create signinother | sed "s/^/create2: /"
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -c /tmp/jar -X POST -H "Content-Type: application/json" \
      -d "{\"username\":\"signinuser\",\"password\":\"$SI_PW\"}" "$SB/api/auth/login")
    say pw-login "$code" ""
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -b /tmp/jar "$SB/settings")
    say settings-unlinked "$code" "link=$(grep -c "data-discord-link" /tmp/body) notlinked=$(grep -c "not linked" /tmp/body) unlink=$(grep -c "data-discord-unlink data-confirm-text" /tmp/body)"
    # The link POST: the wrong password is 401 and mints nothing; the right one answers the URL.
    code=$(curl -sS -o /tmp/body -D /tmp/hdr -w "%{http_code}" -b /tmp/jar -X POST -H "Content-Type: application/json" \
      -d "{\"current_password\":\"definitely not it\"}" "$SB/api/auth/discord")
    say link-badpw "$code" "$(tr -d "\n" < /tmp/body) cookie=$(grep -ci "^set-cookie: clippet_session_discord=" /tmp/hdr || true)"
    code=$(curl -sS -o /tmp/body -D /tmp/hdr -w "%{http_code}" -b /tmp/jar -X POST -H "Content-Type: application/json" \
      -d "{\"current_password\":\"$SI_PW\"}" "$SB/api/auth/discord")
    say link-post "$code" "$(tr -d "\n" < /tmp/body | cut -c1-400) cookie=$(grep -ci "^set-cookie: clippet_session_discord=" /tmp/hdr || true)"
    # DELETE on an unlinked account is 404 not_linked.
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -b /tmp/jar -X DELETE -H "Content-Type: application/json" "$SB/api/auth/discord")
    say unlink-none "$code" "$(tr -d "\n" < /tmp/body)"
    # The CLI matrix.
    clippet admin users link signinuser "$SI_ID" | sed "s/^/link: /"
    clippet admin users | sed "s/^/users: /"
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -b /tmp/jar "$SB/settings")
    say settings-linked "$code" "unlink=$(grep -c "data-discord-unlink data-confirm-text" /tmp/body) linked=$(grep -c ">linked" /tmp/body) form=$(grep -c "data-discord-link" /tmp/body) id=$(grep -c "$SI_ID" /tmp/body)"
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -b /tmp/jar -X POST -H "Content-Type: application/json" \
      -d "{\"current_password\":\"$SI_PW\"}" "$SB/api/auth/discord")
    say link-again "$code" "$(tr -d "\n" < /tmp/body)"
    rc=0; clippet admin users link signinother "$SI_ID" >/tmp/o 2>&1 || rc=$?
    printf "link-taken rc=%s %s\n" "$rc" "$(tr -d "\n" < /tmp/o)"
    rc=0; clippet admin users link signinuser "$SI_ID2" >/tmp/o 2>&1 || rc=$?
    printf "link-second rc=%s %s\n" "$rc" "$(tr -d "\n" < /tmp/o)"
    rc=0; clippet admin users link signinuser 12 >/tmp/o 2>&1 || rc=$?
    printf "link-badid rc=%s %s\n" "$rc" "$(tr -d "\n" < /tmp/o)"
    # The route unlinks; the session survives; a second unlink is the 404.
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -b /tmp/jar -X DELETE -H "Content-Type: application/json" "$SB/api/auth/discord")
    say unlink-route "$code" "$(tr -d "\n" < /tmp/body)"
    code=$(curl -sS -o /tmp/body -w "%{http_code}" -b /tmp/jar "$SB/api/auth/me")
    say me-after-unlink "$code" ""
    clippet admin users unlink signinuser | sed "s/^/unlink-cli: /"
    clippet admin users link signinother "$SI_ID" | sed "s/^/link2: /"
    clippet admin users unlink signinother | sed "s/^/unlink2: /"
    clippet admin users | sed "s/^/users-end: /"
    clippet admin doctor 2>&1 | grep "^login" | sed "s/^/doctor: /" || true
    printf "secret-in-log: %s\n" "$(grep -c "$SI_SECRET" /tmp/boot.log || true)"
    kill "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  ' 2>&1) || die "the sign-in probe container failed: $SI_OUT"
printf '%s\n' "$SI_OUT" > "$SI_OUT_FILE"
printf '%s\n' "$SI_OUT" | sed -n 's/^/[e2e]   probe: /p'

si_line() { printf '%s\n' "$SI_OUT" | grep "^$1 " | head -1; }
si_field() { printf '%s\n' "$SI_OUT" | sed -n "s/^$1: //p" | head -1; }

# (b) The boot line names the client id and the redirect URI, never the secret.
SI_L=$(si_field bootline)
expect_nonempty "$SI_L" "the probe logged no 'discord login configured' line"
contains "$SI_L" "client_id=$SI_CLIENT" || die "the boot line does not name the client id: $SI_L"
contains "$SI_L" "redirect_uri=http://localhost:8080/api/auth/discord/callback" || die "the boot line does not print the redirect URI: $SI_L"
contains "$SI_OUT" "$SI_SECRET" && die "the client SECRET reached the probe's output"
expect "$(si_field secret-in-log)" 0 "the secret's occurrences in the app log"

# (c) /login carries the anchor — a plain link with next, no form.
contains "$(si_line login-page)" 'status=200 body=1' || die "/login does not carry exactly one Discord anchor: $(si_line login-page)"
expect "$(si_field login-href)" 'href="/api/auth/discord/start?next=%2Fsettings"' "the anchor's href"

# (d) The start is a 302 to Discord's authorize URL: the code grant, identify and nothing else, S256, a
#     43-character state, the redirect URI byte for byte — and the flow cookie with the session cookie's
#     flags, ten minutes, no Secure on http.
SI_L=$(si_line start)
contains "$SI_L" 'status=302' || die "start is not a 302: $SI_L"
SI_LOC=${SI_L#*location=}
case "$SI_LOC" in https://discord.com/oauth2/authorize\?*) ;; *) die "start does not go to Discord's authorize URL: $SI_LOC";; esac
for q in "response_type=code" "client_id=$SI_CLIENT" "scope=identify" "code_challenge_method=S256" \
         "redirect_uri=http%3A%2F%2Flocalhost%3A8080%2Fapi%2Fauth%2Fdiscord%2Fcallback"; do
  case "&${SI_LOC#*\?}&" in *"&$q&"*) ;; *) die "the authorize URL lacks $q: $SI_LOC";; esac
done
case "&${SI_LOC#*\?}&" in *"&scope=identify%20"*|*"&scope=identify+"*) die "the authorize URL asks for more than identify: $SI_LOC";; esac
SI_STATE_URL=$(printf '%s' "&${SI_LOC#*\?}&" | sed -n 's/.*&state=\([^&]*\)&.*/\1/p')
[ "${#SI_STATE_URL}" = 43 ] || die "the state is ${#SI_STATE_URL} characters, want 43: $SI_STATE_URL"
SI_SETCOOKIE=$(si_field start-cookie)   # the whole Set-Cookie header: its value is a live nonce — a die prints the attributes and redact_ of name=value
contains "$SI_SETCOOKIE" 'HttpOnly' || die "the flow cookie is not HttpOnly: $(redact_ "${SI_SETCOOKIE%%;*}");${SI_SETCOOKIE#*;}"
contains "$SI_SETCOOKIE" 'SameSite=Lax' || die "the flow cookie is not SameSite=Lax: $(redact_ "${SI_SETCOOKIE%%;*}");${SI_SETCOOKIE#*;}"
contains "$SI_SETCOOKIE" 'Max-Age=600' || die "the flow cookie does not live ten minutes: $(redact_ "${SI_SETCOOKIE%%;*}");${SI_SETCOOKIE#*;}"
contains "$SI_SETCOOKIE" 'Path=/' || die "the flow cookie is not Path=/: $(redact_ "${SI_SETCOOKIE%%;*}");${SI_SETCOOKIE#*;}"
contains "$SI_SETCOOKIE" 'Secure' && die "the flow cookie is Secure on a plain-http origin: $(redact_ "${SI_SETCOOKIE%%;*}");${SI_SETCOOKIE#*;}"
contains "$(si_field start-cache)" 'no-store' || die "the start is storable: $(si_field start-cache)"
SI_COOKIE=$(si_field flow-cookie)
expect_nonempty "$SI_COOKIE" "the flow cookie's value"
SI_NONCE=${SI_COOKIE%%.*}
[ "${#SI_NONCE}" = 43 ] || die "the nonce is ${#SI_NONCE} characters, want 43: $SI_NONCE"
case "$SI_COOKIE" in *.*.*) ;; *) die "the flow cookie has no MAC segment (erratum 302 (h)): $(redact_ "$SI_COOKIE")";; esac
# What a session thief would build (erratum 302 (h)): the real nonce — so the state matches — and a
# link payload naming an account, with no MAC; and the real cookie with the last byte of its MAC
# changed. Neither is a flow, whatever it says.
SI_FORGED="$SI_NONCE.$(printf 'link\000V8jHBPpf5roG\000/settings' | openssl base64 -A | tr '+/' '-_' | tr -d '=')"
SI_LAST=${SI_COOKIE#"${SI_COOKIE%?}"}
if [ "$SI_LAST" = A ]; then SI_TAMPERED="${SI_COOKIE%?}B"; else SI_TAMPERED="${SI_COOKIE%?}A"; fi
# The state in the URL is the one the app derives from THIS nonce — the harness derives it the same way.
SI_STATE=$(printf '%s' "clippet-discord-state.$SI_NONCE" | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')
expect "$SI_STATE" "$SI_STATE_URL" "the state derived from the cookie's nonce, against the authorize URL's"
# …and the verifier itself is in neither.
contains "$SI_LOC" "$SI_NONCE" && die "the authorize URL carries the verifier"

# (e) The refusals before any exchange: cross-site, no cookie, a wrong state.
SI_L=$(si_line xsite)
contains "$SI_L" 'status=303 location=/login' || die "a cross-site start was not bounced to /login: $SI_L"
expect "$(si_field xsite-cookie)" 0 "a cross-site start minted a flow cookie"
contains "$(si_line cb-nocookie)" 'status=303 location=/login?discord=expired' || die "a callback with no cookie: $(si_line cb-nocookie)"
contains "$(si_line cb-badstate)" 'status=303 location=/login?discord=expired' || die "a callback with the wrong state: $(si_line cb-badstate)"
SI_SETCOOKIE=$(si_field badstate-cookie)
contains "$SI_SETCOOKIE" 'Max-Age=0' || die "a refused callback did not clear the flow cookie: $(redact_ "${SI_SETCOOKIE%%;*}");${SI_SETCOOKIE#*;}"

# (f) The Settings panel, the link POST and the CLI matrix.
contains "$(si_line pw-login)" 'status=200' || die "the password login failed: $(si_line pw-login)"
contains "$(si_line settings-unlinked)" 'status=200 body=link=1 notlinked=1 unlink=0' || die "the unlinked panel: $(si_line settings-unlinked)"
SI_L=$(si_line link-badpw)
contains "$SI_L" 'status=401' || die "the link POST with the wrong password: $SI_L"
contains "$SI_L" 'invalid_credentials' || die "the wrong-password error code: $SI_L"
contains "$SI_L" 'cookie=0' || die "a refused link POST minted a flow cookie: $SI_L"
SI_L=$(si_line link-post)
contains "$SI_L" 'status=200' || die "the link POST: $SI_L"
contains "$SI_L" '"url":"https://discord.com/oauth2/authorize?' || die "the link POST does not answer the authorize URL: $SI_L"
contains "$SI_L" 'cookie=1' || die "the link POST minted no flow cookie: $SI_L"
contains "$(si_line unlink-none)" 'status=404' || die "DELETE on an unlinked account: $(si_line unlink-none)"
contains "$(si_line unlink-none)" 'not_linked' || die "the not-linked error code: $(si_line unlink-none)"
contains "$SI_OUT" "link: linked " || die "the CLI link did not report: $SI_OUT"
contains "$SI_OUT" "signinuser  created=" || die "users does not list the account: $SI_OUT"
printf '%s\n' "$SI_OUT" | grep '^users: ' | grep 'signinuser ' | grep -q "discord=$SI_ID" || die "users does not show the linked id: $SI_OUT"
printf '%s\n' "$SI_OUT" | grep '^users: ' | grep 'signinother ' | grep -q 'discord=-' || die "users shows a link on the other account: $SI_OUT"
contains "$(si_line settings-linked)" 'status=200 body=unlink=1 linked=1 form=0 id=0' || die "the linked panel (and the id must not be on the page): $(si_line settings-linked)"
SI_L=$(si_line link-again)
contains "$SI_L" 'status=409' || die "a second link POST on a linked account: $SI_L"
contains "$SI_L" 'discord_linked' || die "the second-link error code: $SI_L"
SI_L=$(si_line link-taken)
contains "$SI_L" 'rc=1' || die "linking a taken id to another account was not refused: $SI_L"
contains "$SI_L" 'already linked to signinuser' || die "the refusal does not name the first account: $SI_L"
SI_L=$(si_line link-second)
contains "$SI_L" 'rc=1' || die "a second link on a linked account was not refused: $SI_L"
contains "$SI_L" 'users unlink signinuser' || die "the refusal does not point at unlink: $SI_L"
contains "$(si_line link-badid)" 'rc=2' || die "a malformed id was not a usage error: $(si_line link-badid)"
contains "$(si_line unlink-route)" 'status=204' || die "DELETE /api/auth/discord: $(si_line unlink-route)"
contains "$(si_line me-after-unlink)" 'status=200' || die "the unlink touched the session: $(si_line me-after-unlink)"
contains "$SI_OUT" 'unlink-cli: signinuser: no Discord link' || die "a second unlink did not say there was nothing: $SI_OUT"
contains "$SI_OUT" 'link2: linked ' || die "the freed id could not be linked to the other account: $SI_OUT"
contains "$SI_OUT" "unlink2: signinother unlinked from Discord user $SI_ID" || die "the CLI unlink did not report: $SI_OUT"
printf '%s\n' "$SI_OUT" | grep '^users-end: ' | grep -q "discord=$SI_ID" && die "a link survived the unlinks: $SI_OUT"

# (g) The doctor's `login` row: on, the client id, the redirect URI, never the secret.
contains "$SI_OUT" "doctor: login    ok   discord login: on - client $SI_CLIENT, secret (redacted)" || die "doctor's login row: $SI_OUT"
contains "$SI_OUT" 'Redirect URI (paste this into the Developer Portal' || die "doctor does not print the redirect URI: $SI_OUT"
contains "$SI_OUT" 'http://localhost:8080/api/auth/discord/callback' || die "doctor's redirect URI is not the base plus the callback: $SI_OUT"
contains "$SI_OUT" 'login    warn' && die "doctor warns on a loopback base: $SI_OUT"
echo "[e2e]   probe: the anchor, a 302 to Discord (identify, S256, the state derived from the cookie's nonce matches), the flow cookie's flags, cross-site/no-cookie/bad-state refused, the panel follows the link, link POST 401/200/409, DELETE 404/204, the CLI matrix, doctor prints the redirect URI and never the secret"

# (h) A SECOND container answers the callback for the cookie the first one minted, with the GOOD state:
#     Discord's own refusal (`error=access_denied`) lands on ?discord=denied and clears the cookie, so
#     the SAME callback URL a second time — with the jar that held the cookie — is refused as expired
#     with no exchange; a good state with no code is refused the same way. None of these reaches step 7,
#     so no outbound call is made anywhere (the one callback shape this suite must never send is a good
#     state WITH a code).
SI_OUT2=$(dc run --rm -T --no-deps \
  -e DISCORD_LOGIN_CLIENT_ID="$SI_CLIENT" -e DISCORD_LOGIN_CLIENT_SECRET="$SI_SECRET" \
  -e DATA_DIR=/tmp/signin2 -e SCRATCH_DIR=/tmp/signin2-scratch -e PUBLIC_BASE_URL=http://localhost:8080 \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e SI_COOKIE="$SI_COOKIE" -e SI_STATE="$SI_STATE" -e SI_FORGED="$SI_FORGED" -e SI_TAMPERED="$SI_TAMPERED" \
  --entrypoint sh app -c '
    set -e
    mkdir -p /tmp/signin2
    clippet serve >/tmp/boot.log 2>&1 &
    p=$!
    i=0
    while [ $i -lt 30 ]; do grep -q "msg=listening" /tmp/boot.log && break; i=$((i+1)); sleep 1; done
    grep -q "msg=listening" /tmp/boot.log || { echo "PROBE-DID-NOT-BOOT"; cat /tmp/boot.log; exit 1; }
    SB=http://localhost:8080
    nav() {
      l=$1; shift
      code=$(curl -sS -o /tmp/body -D /tmp/hdr -w "%{http_code}" "$@")
      printf "%s status=%s location=%s\n" "$l" "$code" "$(sed -n "s/^[Ll]ocation: *//p" /tmp/hdr | tr -d "\r")"
    }
    CB="$SB/api/auth/discord/callback"
    nav denied -b "clippet_session_discord=$SI_COOKIE" -c /tmp/jar "$CB?error=access_denied&state=$SI_STATE"
    grep -i "^set-cookie: clippet_session_discord=" /tmp/hdr | tr -d "\r" | sed "s/^/denied-cookie: /"
    printf "jar-after: %s\n" "$(grep -c clippet_session_discord /tmp/jar || true)"
    nav replay -b /tmp/jar "$CB?error=access_denied&state=$SI_STATE"
    nav nocode -b "clippet_session_discord=$SI_COOKIE" "$CB?state=$SI_STATE"
    nav longcode -b "clippet_session_discord=$SI_COOKIE" "$CB?state=$SI_STATE&code=$(printf "%0600d" 0)"
    nav forged -b "clippet_session_discord=$SI_FORGED" "$CB?state=$SI_STATE&code=abcdefghij"
    nav tampered -b "clippet_session_discord=$SI_TAMPERED" "$CB?state=$SI_STATE&code=abcdefghij"
    printf "outbound: %s\n" "$(grep -c "discord login: the exchange failed" /tmp/boot.log || true)"
    kill "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  ' 2>&1) || die "the second sign-in probe container failed: $SI_OUT2"
printf '%s\n' "$SI_OUT2" | sed -n 's/^/[e2e]   callback: /p'
si2_line() { printf '%s\n' "$SI_OUT2" | grep "^$1 " | head -1; }
contains "$(si2_line denied)" 'status=303 location=/login?discord=denied' || die "Discord's own refusal with a good state: $(si2_line denied)"
contains "$(printf '%s\n' "$SI_OUT2" | sed -n 's/^denied-cookie: //p')" 'Max-Age=0' || die "the denied callback did not clear the flow cookie: $SI_OUT2"
contains "$SI_OUT2" 'jar-after: 0' || die "the jar still holds the flow cookie after the clear: $SI_OUT2"
contains "$(si2_line replay)" 'status=303 location=/login?discord=expired' || die "the SAME callback URL a second time was not refused: $(si2_line replay)"
contains "$(si2_line nocode)" 'status=303 location=/login?discord=expired' || die "a good state with no code: $(si2_line nocode)"
contains "$(si2_line longcode)" 'status=303 location=/login?discord=expired' || die "a good state with an absurd code: $(si2_line longcode)"
contains "$(si2_line forged)" 'status=303 location=/login?discord=expired' || die "a hand-built link cookie with a good state and a code was not refused as no flow (erratum 302 (h)): $(si2_line forged)"
contains "$(si2_line tampered)" 'status=303 location=/login?discord=expired' || die "a real cookie with its MAC altered was not refused as no flow (erratum 302 (h)): $(si2_line tampered)"
contains "$SI_OUT2" 'outbound: 0' || die "a callback here reached the exchange (which would have called Discord): $SI_OUT2"
echo "[e2e]   callback: a good state + Discord's refusal lands on ?discord=denied and clears the cookie; the replay, no code and an absurd code are all expired; a forged link cookie and a tampered MAC are no flow even with a code; nothing reached the exchange"

# (i) The live stack: the OFF state is the lane's shipped default and the reference stack's until its
#     operator turns sign-in on; both shapes are real and the harness asserts whichever doctor reports
#     (no origin-dependent assertions). OFF: the router's own 404, no anchor on /login, and
#     the row says so in words. ON: the start is the 302 proven in detail above, and /login carries it.
dc exec -T app clippet admin doctor > "$E2E_DIR/signin-doctor.txt" 2>&1 || die "admin doctor failed"
code=$(get "$BASE/login"); expect "$code" 200 "GET /login on the live stack"
SI_ANCHORS=$(grep -c 'data-discord-start' "$RESP" || true)
code=$(http_refused GET "$BASE/api/auth/discord/start")
if grep -q 'discord login: on' "$E2E_DIR/signin-doctor.txt"; then
  expect "$code" 302 "GET /api/auth/discord/start on a stack that has the login on"
  expect "$SI_ANCHORS" 1 "/login's Discord anchor with the login on"
  echo "[e2e]   this stack has Sign in with Discord ON (doctor: on): the start is a 302 and /login carries the anchor"
else
  expect "$code" 404 "GET /api/auth/discord/start with the login off"
  expect "$(json error)" not_found "…and it is the router's own 404, not a handler's refusal"
  expect "$SI_ANCHORS" 0 "/login's Discord anchor with the login off"
  has_file "$E2E_DIR/signin-doctor.txt" 'discord login: off' "doctor reports the login as off"
  has_file "$E2E_DIR/signin-doctor.txt" '/api/auth/discord/start is not served' "…and says the route is not served"
fi
# (j) One of the two set without the other is a BOOT ERROR naming both (§4): a half-configured login
#     is a silently dead one. The other is pinned EMPTY, for 85-bot's reason.
SI_BOOT=$(dc run --rm -T --no-deps -e DISCORD_LOGIN_CLIENT_ID="$SI_CLIENT" -e DISCORD_LOGIN_CLIENT_SECRET= \
  -e DATA_DIR=/tmp/signinboot -e SCRATCH_DIR=/tmp/signinboot-scratch app serve 2>&1) && \
  die "a container with only DISCORD_LOGIN_CLIENT_ID set booted: $SI_BOOT"
contains "$SI_BOOT" 'DISCORD_LOGIN_CLIENT_ID' || die "the boot error does not name DISCORD_LOGIN_CLIENT_ID: $SI_BOOT"
contains "$SI_BOOT" 'DISCORD_LOGIN_CLIENT_SECRET' || die "the boot error does not name DISCORD_LOGIN_CLIENT_SECRET: $SI_BOOT"
printf '%s\n' "$SI_BOOT" | sed -n 's/^/[e2e]   boot-probe: /p'
echo "[e2e]   the route and /login match what doctor says of this stack; with one variable of two the process refuses to boot and names both"
