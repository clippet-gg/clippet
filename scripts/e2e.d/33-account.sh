# e2e.d/33-account.sh — bundle B (account security), ROADMAP #1/#21/#25, §20 errata 105–112.
#
# e2e-schedule: parallel
# e2e-namespace: account=ac,d prefix=AC_ buckets=-
# e2e-covers: internal/auth/** internal/httpapi/auth_handlers.go internal/httpapi/token_handlers.go
# e2e-covers: internal/db/users.go cmd/clippet/admin.go internal/clientip/** internal/ratelimit/**
# e2e-covers: web/templates/settings.html web/templates/login.html web/templates/register.html
#   It CHANGES an account's password and then revokes every other session of that account, twice, and
#   counts what went ("sessions_revoked = N_SESSIONS - 1", "exactly one session is left"); it also
#   asserts the contents of `admin tokens list <account>`. Done to the run's OWNER — as it was until
#   lane `excl` — that kept it exclusive: four other bundles log the owner in as a second browser and
#   every bundle that mints a token appears in the owner's list, so anything beside it would be signed
#   out mid-step or would move the counts. So the bundle does all of it to an ACCOUNT OF ITS OWN,
#   `${E2E_USER}_ac` (`bundle_account`, scripts/e2e.sh), with a clip of its own (AC_C) for the token
#   scope matrix that used to read the shared clip C: every count and list is then this bundle's alone,
#   and every assertion is the one it was, about that account. The run's owner is never touched.
#
# Sourced by scripts/e2e.sh before the step-16 log gate, so every line these steps make is covered by
# it. Every request here that needs a session uses the bundle's own account's, in $AC_JARW.
#
# What it creates and what it leaves behind:
#   - its own ACCOUNT, `${E2E_USER}_ac`, and one small clip of it (AC_C), deleted at the end of step 36;
#     the account is pruned by step 17 with the run's other suffixed users;
#   - two extra SESSIONS of that account, both ended inside the steps that make them;
#   - one API token per scope, all revoked before the step that minted them ends;
#   - one throwaway CONTAINER (step 35) on its own database under /tmp, removed with --rm — the seven
#     registrations it makes (six concurrent, one after) are on ITS database and ITS rate limiter, so
#     they cost the live stack nothing;
#   - one extra USER, `${E2E_USER}_d`, made by `clippet admin users create` — it owns nothing and its
#     name matches the `e2e_%` pattern step 17 prunes, so a later run removes it exactly as it removes
#     this run's other three. There is no user-delete ROUTE by design (DEPLOY §8).
# The account's password is CHANGED and changed back inside step 33, so every later login of it sees
# the password it started with.
#
# Rate-limiter budget: login, register and PATCH /api/auth/password all draw on RATE_AUTH_PER_MIN.
# The shipped ceiling is 10/min/IP, but a run raises it (E2E_RATE_AUTH_PER_MIN, 120/min/IP by default;
# scripts/e2e.sh step 1, §20 erratum 245) because the pool puts several bundles' logins in the same
# minute and nothing in the suite asserts that ceiling. This file spends EIGHT tokens against the live
# limiter — six in step 33 (a login, the refused PATCH, the real PATCH, two logins that prove the
# change, the PATCH back), one login in step 34 and one in step 36 — and step 35 spends none of them:
# its throwaway container's seven registrations run against ITS OWN process on 127.0.0.1:8080, and
# POST /api/auth/tokens carries csrf and requireJSON only. Under the raised limiter that is not a
# budget to plan by hand; it becomes one again only on a run that sets E2E_RATE_AUTH_PER_MIN back to 10.
#
# Both halves pass over plain http AND over https: nothing below asserts a scheme.

step 33 "password change (#1): the old cookie 401s, the old password 401s, the new one logs in, other sessions are gone"
# The bundle's own account (see the header) and its one clip, uploaded now so its encode runs while
# steps 33-34 do; step 35's scope matrix reads it.
bundle_account ac
AC_USER=$BA_USER; AC_JARW=$BA_JARW
code=$(UPLOAD_JARW=$AC_JARW upload AC small-640x360.mp4 '{"title":"E2E account scopes","ttl_days":1}')
expect "$code" 201 "upload AC_C, the account bundle's own clip ($(cat "$RESP"))"
AC_C=$(json id); expect_nonempty "$AC_C" "AC_C id"
PJAR="$E2E_DIR/cookies-pw.txt"; PJARW=$(W "$PJAR"); rm -f "$PJAR"
E2E_NEWPASS="e2e-newpass-$(date +%s)"
# A SECOND browser of the owner: the session a stolen laptop would be holding.
code=$(http POST "$BASE/api/auth/login" -c "$PJARW" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$AC_USER\",\"password\":\"$E2E_PASS\"}")
expect "$code" 200 "open a second session for $AC_USER"
code=$(get "$BASE/api/auth/me" -b "$PJARW"); expect "$code" 200 "the second session resolves before the change"

# The wrong current password is refused, and refusing it must revoke nothing: a change that swept the
# other sessions before verifying would be a denial-of-service any holder of one cookie could run.
code=$(http_refused PATCH "$BASE/api/auth/password" -b "$AC_JARW" -H 'Content-Type: application/json' \
  -d "{\"current_password\":\"definitely not it\",\"new_password\":\"$E2E_NEWPASS\"}")
expect "$code" 401 "PATCH /api/auth/password with the wrong current password"
expect "$(json error)" invalid_credentials "wrong-current error code"
code=$(get "$BASE/api/auth/me" -b "$PJARW"); expect "$code" 200 "a REFUSED change revokes nothing"

# The real change. The reply is a receipt: how many other sessions went.
code=$(http PATCH "$BASE/api/auth/password" -b "$AC_JARW" -H 'Content-Type: application/json' \
  -d "{\"current_password\":\"$E2E_PASS\",\"new_password\":\"$E2E_NEWPASS\"}")
expect "$code" 200 "PATCH /api/auth/password ($(cat "$RESP"))"
PW_REVOKED=$(json sessions_revoked); expect_nonempty "$PW_REVOKED" "sessions_revoked"
[ "$PW_REVOKED" -ge 1 ] 2>/dev/null || die "sessions_revoked = '$PW_REVOKED', want at least 1"
lacks_file "$RESP" "$E2E_NEWPASS" "the change response must not echo the new password"
# The three claims the feature is FOR.
code=$(get "$BASE/api/auth/me" -b "$PJARW"); expect "$code" 401 "the OTHER session's cookie after the change"
expect "$(json error)" unauthorized "revoked-session error code"
code=$(get "$BASE/api/auth/me" -b "$AC_JARW"); expect "$code" 200 "the CHANGING session stays signed in"
code=$(http_refused POST "$BASE/api/auth/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$AC_USER\",\"password\":\"$E2E_PASS\"}")
expect "$code" 401 "the OLD password"
expect "$(json error)" invalid_credentials "old-password error code"
rm -f "$PJAR"
code=$(http POST "$BASE/api/auth/login" -c "$PJARW" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$AC_USER\",\"password\":\"$E2E_NEWPASS\"}")
expect "$code" 200 "the NEW password logs in"

# Put it back, from the main session, so the rest of the run and any re-run see the password step 3
# minted. This second change also proves the sweep from the other side: the session the NEW password
# just opened is the one that goes.
code=$(http PATCH "$BASE/api/auth/password" -b "$AC_JARW" -H 'Content-Type: application/json' \
  -d "{\"current_password\":\"$E2E_NEWPASS\",\"new_password\":\"$E2E_PASS\"}")
expect "$code" 200 "change the password back"
expect "$(json sessions_revoked)" 1 "the second change revoked exactly the session the first one opened"
code=$(get "$BASE/api/auth/me" -b "$PJARW"); expect "$code" 401 "that session is gone too"
code=$(get "$BASE/api/auth/me" -b "$AC_JARW"); expect "$code" 200 "and the owner is still signed in"
rm -f "$PJAR"
echo "[e2e]   password changed and restored; $PW_REVOKED other session(s) revoked by the first change"

step 34 "active sessions (#1): the list names no credential, and 'log out everywhere else' keeps exactly this one"
# One more browser to have something to sign out.
SJAR="$E2E_DIR/cookies-sess.txt"; SJARW=$(W "$SJAR"); rm -f "$SJAR"
code=$(http POST "$BASE/api/auth/login" -c "$SJARW" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$AC_USER\",\"password\":\"$E2E_PASS\"}")
expect "$code" 200 "open a session to sign out"
SESS_COOKIE=$(sed -n 's/.*clippet_session[[:space:]]*//p' "$SJAR" | tail -1 | tr -d '\r')
expect_nonempty "$SESS_COOKIE" "the raw cookie value from the jar"

code=$(get "$BASE/api/auth/sessions" -b "$AC_JARW"); expect "$code" 200 "GET /api/auth/sessions"
contains "$(hdr Cache-Control)" no-store || die "the session list must not be storable: $(hdr Cache-Control)"
contains "$(hdr Vary)" Cookie || die "the session list must Vary on Cookie: $(hdr Vary)"
# Exactly the fields §20 erratum 107 promises, and NO handle of any kind: the hash is the credential's
# shadow, so a list that carried one would hand a page (and therefore an attacker's page) a name for
# somebody else's session.
expect_nonempty "$(json sessions.0.created_at)" "sessions[0].created_at"
expect_nonempty "$(json sessions.0.last_seen_at)" "sessions[0].last_seen_at"
expect_nonempty "$(json sessions.0.expires_at)" "sessions[0].expires_at"
expect_empty "$(json sessions.0.id)" "sessions[0] must carry no id"
expect_empty "$(json sessions.0.token_hash)" "sessions[0] must carry no hash"
lacks_secret_f "$RESP" "$SESS_COOKIE" "the session list echoed a raw cookie value"
# Two live sessions, exactly one of them current.
N_SESSIONS=$($PY - "$(W "$RESP")" <<'PYEOF'
import json, sys
doc = json.load(open(sys.argv[1], encoding='utf-8'))
rows = doc['sessions']
cur = [r for r in rows if r.get('current')]
if len(cur) != 1:
    sys.exit('%d rows marked current, want exactly 1' % len(cur))
print(len(rows))
PYEOF
) || die "the session list must mark exactly one row as the caller's"
[ "$N_SESSIONS" -ge 2 ] 2>/dev/null || die "session list has $N_SESSIONS rows, want at least 2"

code=$(http DELETE "$BASE/api/auth/sessions" -b "$AC_JARW" -H 'Content-Type: application/json')
expect "$code" 200 "DELETE /api/auth/sessions ($(cat "$RESP"))"
expect "$(json sessions_revoked)" "$((N_SESSIONS - 1))" "log-out-everywhere-else revoked every other session"
code=$(get "$BASE/api/auth/me" -b "$SJARW"); expect "$code" 401 "the other browser is signed out"
code=$(get "$BASE/api/auth/me" -b "$AC_JARW"); expect "$code" 200 "THIS browser is not"
code=$(get "$BASE/api/auth/sessions" -b "$AC_JARW"); expect "$code" 200 "list again"
expect "$(json sessions.0.current)" true "the one row left is the caller's"
expect_empty "$(json sessions.1.created_at)" "exactly one session is left"
# A second call is a clean zero, not an error.
code=$(http DELETE "$BASE/api/auth/sessions" -b "$AC_JARW" -H 'Content-Type: application/json')
expect "$code" 200 "a second log-out-everywhere-else"
expect "$(json sessions_revoked)" 0 "…revokes nothing"
# The settings page renders all three sections, server-side, with no inline script or style.
code=$(page "$BASE/settings" -b "$AC_JARW"); expect "$code" 200 "GET /settings"
has_file "$PAGE" 'data-password-form' "the settings page's password form"
has_file "$PAGE" 'autocomplete="current-password"' "current-password autocomplete"
has_file "$PAGE" 'autocomplete="new-password"' "new-password autocomplete"
has_file "$PAGE" 'data-session-list' "the settings page's session list"
has_file "$PAGE" 'data-logout-others' "the log-out-everywhere-else button"
has_file "$PAGE" 'data-token-mint' "the tokens section"
has_file "$PAGE" 'name="scopes" value="read"' "the read-scope checkbox"
# The token NAME is a label for a token, not a value a browser profile has: the field carries
# autocomplete="off" so nothing is offered for it (nit N027).
has_file "$PAGE" 'name="name" maxlength="64" autocomplete="off"' "the token name field opts out of autofill"
# Both forms here POST to JSON-only routes, so without JavaScript each is a 405/415 envelope with no
# way back to the page. They say so; requireJSON is NOT relaxed (§20 errata 120, 178 — nit N109).
has_file "$PAGE" '<noscript>' "the settings forms say they need JavaScript"
has_file "$PAGE" 'needs JavaScript in this browser' "…and say it in words"
lacks_file "$PAGE" '<style' "settings page style element"
lacks_file "$PAGE" 'onclick=' "settings page inline handler"
# The nav says Settings now, and the Phase 1 URL still answers rather than 404ing.
has_file "$PAGE" 'href="/settings"' "the header nav points at /settings"
code=$(page "$BASE/settings/tokens" -b "$AC_JARW")
expect "$code" 303 "/settings/tokens still answers"
expect "$(hdr Location)" "/settings#tokens" "…as a redirect to the tokens section"
rm -f "$SJAR"
echo "[e2e]   $N_SESSIONS sessions listed with no credential in the body; one survived the sweep"

step 35 "secure-by-default registration (#21) and the read-only token scope (#25)"
# --- (a) REGISTRATION=first, proven in a THROWAWAY container on its own database ------------------
# The live stack must stay REGISTRATION=open (step 1 insists on it, because this script registers
# users), so the new default is proven where it can be: one `compose run` container with
# DATA_DIR under /tmp, its own empty users table, and REGISTRATION=first. It is removed on exit,
# takes the same image the stack is running, and never touches the live database or the bucket.
# SCRATCH_DIR travels with DATA_DIR and must stay that way: `compose run` still mounts the service's
# volumes, so a `clippet serve` with a private database over the LIVE /scratch sees every real
# working directory as row-less and its boot janitor deletes them — including one belonging to an
# in-flight transcode, which then fails `permanent` (scripts/e2e.d/README.md, third convention). The
# two directories are SIBLINGS, like every other probe's (nit sweep S28): production wants them
# separable — which is why `admin stats` reports `same_device` at all — so the example the suite sets
# should be the shape production uses.
# The first thing the probe does is fire a CONCURRENT BURST at the empty table, because that is the
# guarantee `first` actually makes and the one a sequential probe cannot see: bcrypt (cost 12) sits
# between any pre-check and the insert, so a check-then-insert leaves a window hundreds of
# milliseconds wide in which every racer reads "empty" and every racer is accepted (§20 erratum 106).
FIRST_OUT=$(dc run --rm -T --no-deps -e REGISTRATION=first -e DATA_DIR=/tmp/first \
  -e SCRATCH_DIR=/tmp/first-scratch \
  --entrypoint sh app -c '
    set -e
    mkdir -p /tmp/first
    clippet migrate >/dev/null
    clippet serve >/tmp/serve.log 2>&1 &
    i=0
    while [ $i -lt 60 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
    reg() {
      curl -sS -o "${2:-/tmp/body}" -w "%{http_code}\n" -X POST -H "Content-Type: application/json" \
        -d "{\"username\":\"$1\",\"password\":\"bootstrap-pass\"}" http://127.0.0.1:8080/api/auth/register
    }
    i=1; pids=""
    while [ $i -le 6 ]; do
      ( reg "racer$i" /tmp/rbody.$i > /tmp/rcode.$i ) &
      pids="$pids $!"
      i=$((i+1))
    done
    # Wait for the SIX racers by pid, never a bare `wait`: `clippet serve` is a
    # background job of this same shell and a bare wait would block on it forever.
    for p in $pids; do wait "$p" || true; done
    echo "burst=$(cat /tmp/rcode.* | tr "\n" " ")"
    echo "burst201=$(cat /tmp/rcode.* | grep -c "^201$" || true)"
    echo "burst403=$(cat /tmp/rcode.* | grep -c "^403$" || true)"
    echo "burstclosed=$(grep -l registration_closed /tmp/rbody.* | wc -l | tr -d " ")"
    echo "second=$(reg seconduser)"
    echo "secondbody=$(cat /tmp/body)"
    echo "inventory=$(clippet admin users | tail -1)"
  ' 2>&1) || die "the REGISTRATION=first probe container failed: $FIRST_OUT"
printf '%s\n' "$FIRST_OUT" | sed -n 's/^/[e2e]   first-probe: /p'
contains "$FIRST_OUT" 'burst201=1' || die "a concurrent burst against an EMPTY users table got more than one account, which is the one thing REGISTRATION=first promises: $FIRST_OUT"
contains "$FIRST_OUT" 'burst403=5' || die "the losers of the bootstrap race got something other than 403: $FIRST_OUT"
contains "$FIRST_OUT" 'burstclosed=5' || die "a racer that lost got an error code other than registration_closed: $FIRST_OUT"
contains "$FIRST_OUT" 'second=403' || die "REGISTRATION=first stayed open after the first account: $FIRST_OUT"
contains "$FIRST_OUT" 'registration_closed' || die "the second registration's error code is wrong: $FIRST_OUT"
contains "$FIRST_OUT" 'inventory=1 user' || die "the probe instance ended up with more than the ONE account it may hold: $FIRST_OUT"
# …and the shipped default really is `first`: compose interpolates it, and an unknown value is a boot
# error that names the three accepted ones (the deliberate failure is kept out of the ERR trap).
grep -qF 'REGISTRATION: ${REGISTRATION:-first}' compose.yaml || die "compose no longer defaults REGISTRATION to first"
grep -qF 'REGISTRATION=first' .env.example || die ".env.example no longer documents the first default"
echo "[e2e]   shipped default: REGISTRATION=first (this stack overrides it to open, which step 1 requires)"
REG_CFG=0
REG_OUT=$(dc exec -T -e REGISTRATION=sometimes app clippet migrate 2>&1) || REG_CFG=$?
expect "$REG_CFG" 2 "an unknown REGISTRATION value is a boot error"
contains "$REG_OUT" 'REGISTRATION: must be one of open|first|closed' || die "the boot error does not name the three modes: $REG_OUT"

# --- (b) the read scope: what it opens, and everything it does not (§20 errata 111–112) -----------
code=$(http POST "$BASE/api/auth/tokens" -b "$AC_JARW" -H 'Content-Type: application/json' \
  -d '{"name":"e2e read-only","scopes":["read"]}')
expect "$code" 201 "mint a read-only token ($(cat "$RESP"))"
RDTOK=$(json token); RDTID=$(json id)
expect_nonempty "$RDTOK" "read token"; expect_nonempty "$RDTID" "read token id"
expect "$(json scopes)" '["read"]' "the minted scopes"
# A read-only token is not a ShareX destination: a `.sxcu` for it would import cleanly and 403 on the
# first screenshot.
expect_empty "$(json sxcu)" "a read-only token must carry no ShareX file"
code=$(http POST "$BASE/api/auth/tokens" -b "$AC_JARW" -H 'Content-Type: application/json' \
  -d '{"name":"e2e upload-only","scopes":["upload"]}')
expect "$code" 201 "mint an upload-only token"
UPTOK=$(json token); UPTID=$(json id)
expect_nonempty "$UPTOK" "upload token"; expect_nonempty "$UPTID" "upload token id"
expect "$(json scopes)" '["upload"]' "the upload token's scopes"
expect_nonempty "$(json sxcu.RequestURL)" "an upload token still carries its ShareX file"
# An unknown scope, and an empty set, are refused rather than stored and ignored.
code=$(http_refused POST "$BASE/api/auth/tokens" -b "$AC_JARW" -H 'Content-Type: application/json' \
  -d '{"name":"e2e bad","scopes":["admin"]}')
expect "$code" 400 "an unknown scope"; expect "$(json error)" invalid_request "unknown-scope error code"
code=$(http_refused POST "$BASE/api/auth/tokens" -b "$AC_JARW" -H 'Content-Type: application/json' \
  -d '{"name":"e2e empty","scopes":[]}')
expect "$code" 400 "an empty scope set"

wait_ready "$AC_C" AC_C
# READ: the owner's own library and collections, and the owner-private fields of the owner's clips.
code=$(get "$BASE/api/clips?limit=5" -H "Authorization: Bearer $RDTOK"); expect "$code" 200 "read token: GET /api/clips"
code=$(get "$BASE/api/collections" -H "Authorization: Bearer $RDTOK"); expect "$code" 200 "read token: GET /api/collections"
code=$(get "$BASE/api/clips/$AC_C" -H "Authorization: Bearer $RDTOK"); expect "$code" 200 "read token: GET one clip"
expect_nonempty "$(json views)" "the OWNER's read token sees the owner-private view count (erratum 112)"
# …and NOTHING else. Every owner mutation, and the owner-only reads erratum 19 kept for sessions.
for probe in "PATCH:/api/clips/$AC_C" "DELETE:/api/clips/$AC_C" "POST:/api/clips/$AC_C/touch" "POST:/api/clips/$AC_C/gif" "POST:/api/auth/tokens"; do
  m=${probe%%:*}; p=${probe#*:}
  code=$(http "$m" "$BASE$p" -H "Authorization: Bearer $RDTOK" -H 'Content-Type: application/json' -d '{}')
  expect "$code" 403 "read token: $m $p"
  expect "$(json error)" token_not_allowed "read token on $m $p error code"
done
code=$(get "$BASE/api/clips/$AC_C/original" -H "Authorization: Bearer $RDTOK")
expect "$code" 403 "read token: GET /original"
code=$(get "$BASE/api/auth/sessions" -H "Authorization: Bearer $RDTOK")
expect "$code" 403 "read token: the session list is not a read route"
# A read token may not upload — the two scopes are separate grants, and the refusal is the SCOPE one.
printf 'x' > "$E2E_DIR/tiny-read.bin"
code=$(http_refused POST "$BASE/api/upload" -H "Authorization: Bearer $RDTOK" \
  -F duplicate=allow -F "file=@$(W "$E2E_DIR/tiny-read.bin");type=video/mp4")
expect "$code" 403 "read token: POST /api/upload"
expect "$(json error)" insufficient_scope "read token on the upload route error code"

# UPLOAD-ONLY: erratum 19 is untouched — it still cannot list, and it still sees the PUBLIC DTO of
# the owner's own clip.
code=$(get "$BASE/api/clips?limit=5" -H "Authorization: Bearer $UPTOK"); expect "$code" 403 "upload token: GET /api/clips"
expect "$(json error)" token_not_allowed "upload token on the list error code"
code=$(get "$BASE/api/collections" -H "Authorization: Bearer $UPTOK"); expect "$code" 403 "upload token: GET /api/collections"
code=$(get "$BASE/api/clips/$AC_C" -H "Authorization: Bearer $UPTOK"); expect "$code" 200 "upload token: GET one clip"
expect_empty "$(json views)" "an upload-only token must NOT see the owner's view count (erratum 19)"
# Both still answer /api/auth/me: that is what "any valid token" has always meant.
code=$(get "$BASE/api/auth/me" -H "Authorization: Bearer $RDTOK"); expect "$code" 200 "read token: GET /api/auth/me"
code=$(get "$BASE/api/auth/me" -H "Authorization: Bearer $UPTOK"); expect "$code" 200 "upload token: GET /api/auth/me"
# AC_C is untouched by any of that.
code=$(get "$BASE/api/clips/$AC_C" -b "$AC_JARW"); expect "$code" 200 "AC_C survived the whole scope matrix"
expect "$(json state)" ready "AC_C is still ready"
code=$(http DELETE "$BASE/api/auth/tokens/$RDTID" -b "$AC_JARW"); expect "$code" 204 "revoke the read token"
echo "[e2e]   REGISTRATION=first closes after one account; read opens exactly two routes and the owner's own fields"

step 36 "operator revocation (#1): admin users create / sessions / tokens, each with an explicit selector"
# `users create` is the account REGISTRATION=first leaves the operator to make by hand. The password
# goes in on STDIN — never argv, which shell history and `ps` both keep.
E2E_USER_D=$(bundle_user_ d)    # `${E2E_USER}_d`, or a retry's fresh one (scripts/e2e.d/README.md "Retry")
AC_DPASS="e2e-admin-made-$(date +%s)"
CREATE_OUT=$(printf '%s' "$AC_DPASS" | dc exec -T app clippet admin users create "$E2E_USER_D" 2>&1) \
  || die "admin users create failed: $CREATE_OUT"
contains "$CREATE_OUT" "$E2E_USER_D" || die "admin users create did not name the account: $CREATE_OUT"
case $CREATE_OUT in *"$AC_DPASS"*) die "admin users create echoed the password" ;; esac
echo "[e2e]   $CREATE_OUT"
# It is a real account with that password: log in as it.
DJAR="$E2E_DIR/cookies-d.txt"; DJARW=$(W "$DJAR"); rm -f "$DJAR"
code=$(http POST "$BASE/api/auth/login" -c "$DJARW" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$E2E_USER_D\",\"password\":\"$AC_DPASS\"}")
expect "$code" 200 "log in as the account the CLI created"

# --- sessions ------------------------------------------------------------------------------------
OUT=$(dc exec -T app clippet admin sessions list "$E2E_USER_D") || die "admin sessions list failed: $OUT"
contains "$OUT" "1 live session(s) for $E2E_USER_D" || die "sessions list: $OUT"
# The cookie's hash is what a session IS; it never reaches an operator's terminal either.
SESS_D=$(sed -n 's/.*clippet_session[[:space:]]*//p' "$DJAR" | tail -1 | tr -d '\r')
expect_nonempty "$SESS_D" "the created user's raw cookie"
case $OUT in *"$SESS_D"*) die "admin sessions list printed a raw cookie value" ;; esac
# The guards: no selector, and a username AND --all, are usage errors — never "all of them".
SC=0; SOUT=$(dc exec -T app clippet admin sessions revoke 2>&1) || SC=$?
expect "$SC" 2 "admin sessions revoke with no selector"
contains "$SOUT" 'usage:' || die "no-selector revoke printed no usage: $SOUT"
SC=0; SOUT=$(dc exec -T app clippet admin sessions revoke "$E2E_USER_D" --all 2>&1) || SC=$?
expect "$SC" 2 "admin sessions revoke <username> --all is not a selector"
# The real thing, on the throwaway account so the owner session this run needs survives.
OUT=$(dc exec -T app clippet admin sessions revoke "$E2E_USER_D") || die "admin sessions revoke failed: $OUT"
contains "$OUT" "revoked 1 session(s) for $E2E_USER_D" || die "sessions revoke: $OUT"
code=$(get "$BASE/api/auth/me" -b "$DJARW"); expect "$code" 401 "the revoked session stops resolving"
code=$(get "$BASE/api/auth/me" -b "$AC_JARW"); expect "$code" 200 "the account's OWN session is untouched"
rm -f "$DJAR"

# --- tokens --------------------------------------------------------------------------------------
OUT=$(dc exec -T app clippet admin tokens list "$AC_USER") || die "admin tokens list failed: $OUT"
contains "$OUT" "$UPTID" || die "the token list does not name the live token: $OUT"
contains "$OUT" 'scopes=upload' || die "the token list does not print scopes: $OUT"
case $OUT in *"$UPTOK"*) die "admin tokens list printed a raw token" ;; esac
if contains "$OUT" "$RDTID"; then die "the token list shows a token revoked at step 35"; fi
# Guards again: a username alone is not a selector for a revoke.
SC=0; SOUT=$(dc exec -T app clippet admin tokens revoke "$AC_USER" 2>&1) || SC=$?
expect "$SC" 2 "admin tokens revoke with no token id and no --all"
contains "$SOUT" 'usage:' || die "no-selector token revoke printed no usage: $SOUT"
# Revoke by id, and the credential stops working at once.
OUT=$(dc exec -T app clippet admin tokens revoke "$AC_USER" "$UPTID") || die "admin tokens revoke failed: $OUT"
contains "$OUT" "revoked 1 token(s) for $AC_USER" || die "tokens revoke: $OUT"
code=$(get "$BASE/api/auth/me" -H "Authorization: Bearer $UPTOK"); expect "$code" 401 "the revoked token is rejected"
# Unknown, already revoked and somebody else's are one answer, and exit 1.
SC=0; SOUT=$(dc exec -T app clippet admin tokens revoke "$AC_USER" "$UPTID" 2>&1) || SC=$?
expect "$SC" 1 "revoking the same token twice"
contains "$SOUT" 'no such live token' || die "the second revoke's message: $SOUT"
SC=0; SOUT=$(dc exec -T app clippet admin tokens list nobodyatall 2>&1) || SC=$?
expect "$SC" 1 "admin tokens list for an unknown user"
contains "$SOUT" 'no such user' || die "unknown-user message: $SOUT"
echo "[e2e]   admin created $E2E_USER_D, revoked its session and one of $AC_USER's tokens; no selector defaults to everything"
code=$(http DELETE "$BASE/api/clips/$AC_C" -b "$AC_JARW"); expect_deleted "$code" "$AC_C" "DELETE AC_C"
code=$(http POST "$BASE/api/auth/logout" -b "$AC_JARW" || echo 000); echo "[e2e]   logout $AC_USER -> $code"
summary_row AC "${AC_C:-}" "the account bundle's own clip: the read/upload token scope matrix's subject; deleted"
