#!/usr/bin/env bash
# smoke-public.sh — T3: the PUBLIC edge, in about two minutes, after a deliberate deploy (run 9, lane `gatestack`;
# DESIGN §20 erratum 331). The full suite (T2, scripts/gate.sh) runs on the gate's own loopback stack and never
# crosses Cloudflare; this proves what only the public origin can, and nothing else:
#
#     bash scripts/smoke-public.sh                                  # the instance this checkout's .env names
#     BASE=https://clips.example.com bash scripts/smoke-public.sh   # the same, said out loud (it must match)
#     BASE=http://localhost:8576 bash scripts/lib/gatestack.sh run bash scripts/smoke-public.sh   # any local stack
#
# It checks exactly what the full E2E's closing banner claims for a public origin: Secure + HttpOnly session
# cookies; the five card URLs https, on the origin and 200 to a Discordbot and a Twitterbot user agent; the three
# signed media routes 302 to the bucket's own origin and 200 there; no mixed content on the share page or the
# embed; the homepage Worker passing `X-Clippet-App: 1` through to the product; one small upload → ready → the
# card's tags; and `GET /?mine=1` → 200 for a signed-in person (the gate-28b class: a redirect rule answered 301).
# On an http origin the https-only checks are SKIPPED with a printed reason, and so is the 302 check on a stack in
# MEDIA_DELIVERY=proxy. `SMOKE PASS` or `SMOKE FAIL` is the last line.
#
# THE ACCOUNT: ONE, `e2e_<epoch>_sm` — inside the E2E's sweep selector (E2E_OWN_RE), so a smoke killed before its
# own teardown is swept by the next E2E. It is made with `clippet admin users create` through `docker compose exec`
# on THIS checkout's stack (the password on stdin, never on a command line, never printed), and that is the choice
# of the three: it needs NO restart of production and does not open registration — opening it would mean
# recreating the app with REGISTRATION=open, which is exactly the production restart T3 exists to avoid — and,
# unlike a minted API token, it gives a real browser session, whose cookie is one of the things to check. Before it
# creates anything it asks that stack's app for its PUBLIC_BASE_URL and REFUSES unless it is BASE: an account is
# never made on a stack other than the one being smoked.
#
# What it makes is written down first and removed by its own teardown on ANY exit: the clip (owner DELETE), the
# session (logout), the account (`admin users prune` on the exact name, after checking the pattern matches that one
# account and nothing else), and its scratch directory (the fixture, the jar). It never recreates the app.
#
# Knobs: BASE (the public origin of the stack this checkout runs; unset = PUBLIC_BASE_URL from SMOKE_ENV_FILE,
# default this checkout's .env — the file the stack was started with; neither = refused), SMOKE_MEDIA_HOST (a
# suffix the media origin's host must end with, e.g. r2.cloudflarestorage.com for an R2 bucket; unset = any host
# that is not BASE's own), SMOKE_SELFTEST=1 (the pins against a stub server; no stack, no network beyond loopback;
# needs no BASE).
set -uo pipefail; cd "$(dirname "$0")/.."
# smoke_base_ — BASE, else the PUBLIC_BASE_URL line of SMOKE_ENV_FILE (default .env: the stack this checkout runs,
# which step 2 below checks again against the running app); empty when neither names one. The value is used,
# never printed here.
smoke_base_() {
  local b=${BASE:-} f=${SMOKE_ENV_FILE:-.env}
  [ -n "$b" ] || [ ! -f "$f" ] || b=$(tr -d '\r"'"'" < "$f" | sed -n 's/^PUBLIC_BASE_URL=//p' | tail -1)
  printf '%s' "${b%/}"
}
BASE=$(smoke_base_)
if [ -z "$BASE" ] && [ "${SMOKE_SELFTEST:-0}" != 1 ]; then
  echo "[smoke] FAIL  set BASE=https://<your instance>, or PUBLIC_BASE_URL in .env"; echo "SMOKE FAIL  (no BASE)"; exit 2
fi
SM_T0=$SECONDS
PY=; for c in python python3 "py -3"; do if $c -c 'import sys; assert sys.version_info >= (3, 8)' >/dev/null 2>&1; then PY=$c; break; fi; done
W() { if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi; }
SM_DIR=$(mktemp -d); SM_DIRW=$(W "$SM_DIR")
RESP="$SM_DIR/resp"; HDR="$SM_DIR/hdr"; JAR="$SM_DIR/jar"
SM_FAILS=0; SM_SKIPS=0
ok()   { echo "[smoke] ok    $*"; }
bad()  { echo "[smoke] FAIL  $*"; SM_FAILS=$((SM_FAILS + 1)); }
skip() { echo "[smoke] skip  $*"; SM_SKIPS=$((SM_SKIPS + 1)); }
die()  { echo "[smoke] FAIL  $*"; SM_FAILS=$((SM_FAILS + 1)); exit 1; }
hdr()  { sed -n "s/^$1: *//Ip" "$HDR" | tail -1 | tr -d '\r'; }
APP_HDR='X-Clippet-App: 1'
DISCORD_UA='Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)'
TWITTER_UA='Twitterbot/1.0'
curl__() { curl "$@"; }   # the self-test's seam
# get <url> [curl args…] → the status; body in $RESP, headers in $HDR. A READ, retried once on the three curl
# exits that mean no answer (7/28/35) — the edge's known transient, never a status.
get() {
  local u=$1 c rc=0; shift
  c=$(curl__ -sS -o "$(W "$RESP")" -D "$(W "$HDR")" -w '%{http_code}' "$@" "$u") || rc=$?
  case $rc in 7|28|35) echo "[smoke]       (curl exit $rc on $u; retried once)" >&2; sleep 3; rc=0
                       c=$(curl__ -sS -o "$(W "$RESP")" -D "$(W "$HDR")" -w '%{http_code}' "$@" "$u") || rc=$? ;; esac
  printf '%s' "${c:-000}"
}
# A WRITE is never retried: a repeated POST is a second clip.
post() { local u=$1; shift; curl__ -sS -o "$(W "$RESP")" -D "$(W "$HDR")" -w '%{http_code}' -X POST "$@" "$u" || true; }
redact() { printf '%s?<signed>' "${1%%\?*}"; }

# smoke_mine_ — `/?mine=1` for a signed-in person is the library, 200, filtered. A 301/302 here is a redirect rule
# at the edge eating the query (gate 28b), and it fails by name. Sent WITHOUT X-Clippet-App: a real browser has
# only its session cookie, and the homepage Worker must pass that through too.
smoke_mine_() {
  local code
  code=$(get "$BASE/?mine=1" -b "$(W "$JAR")")
  if [ "$code" != 200 ]; then bad "GET /?mine=1 (signed in) answered $code, want 200$(l=$(hdr Location); [ -z "$l" ] || printf ' (Location: %s)' "$l")"; return 1; fi
  if ! grep -q 'data-mine-filter="1"' "$RESP"; then bad "GET /?mine=1 (signed in) is 200 but not the filtered library (no data-mine-filter=\"1\")"; return 1; fi
  ok "GET /?mine=1 signed in: 200, the filtered library"
}

# ---- the self-test: the /?mine=1 check against a stub that answers 301, then one that answers the library ------
if [ "${SMOKE_SELFTEST:-0}" = 1 ]; then
  [ -n "$PY" ] || { echo "[smoke selftest] skipped: no python >= 3.8"; exit 0; }
  cat > "$SM_DIR/stub.py" <<'PYEOF'
import http.server, sys
mode = sys.argv[1]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if mode == "redirect":
            self.send_response(301); self.send_header("Location", "/"); self.end_headers(); return
        body = b'<main data-clip-list data-mine-filter="1"></main>'
        self.send_response(200); self.send_header("Content-Type", "text/html"); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
s = http.server.HTTPServer(("127.0.0.1", 0), H)
print(s.server_address[1], flush=True)
s.serve_forever()
PYEOF
  st_n=0; st_bad=0
  # Bounded (erratum 338): the stub gets 20 s to print its port (a cold first run on WSL took longer than the old
  # 4 s), an empty port fails BY NAME instead of becoming `http://127.0.0.1:` (= :80), and the self-test's curl has
  # --max-time 5 — under WSL's mirrored networking a closed loopback port times out instead of refusing, and that
  # curl hung for minutes.
  curl__() { curl --connect-timeout 2 --max-time 5 "$@"; }
  for mode in redirect library; do
    $PY -I "$(W "$SM_DIR/stub.py")" "$mode" > "$SM_DIR/port-$mode" 2>/dev/null &
    st_pid=$!
    st_i=0; while [ "$st_i" -lt 100 ]; do [ -s "$SM_DIR/port-$mode" ] && break; sleep 0.2; st_i=$((st_i + 1)); done
    st_port=$(tr -d '\r\n' < "$SM_DIR/port-$mode")
    case $st_port in
      ''|*[!0-9]*) kill "$st_pid" 2>/dev/null; wait "$st_pid" 2>/dev/null; st_n=$((st_n + 1)); st_bad=$((st_bad + 1))
                   echo "[smoke selftest] FAIL: the $mode stub printed no port within 20 s (got '${st_port}')"; continue ;;
    esac
    : > "$JAR"
    out=$(BASE="http://127.0.0.1:$st_port"; SM_FAILS=0; smoke_mine_ 2>&1; echo "rc=$?")
    kill "$st_pid" 2>/dev/null; wait "$st_pid" 2>/dev/null
    st_n=$((st_n + 1))
    case $mode in
      redirect) printf '%s\n' "$out" | grep -q 'FAIL  GET /?mine=1 (signed in) answered 301, want 200' && printf '%s\n' "$out" | grep -q '^rc=1$' \
                  || { st_bad=$((st_bad + 1)); echo "[smoke selftest] FAIL: a 301 on /?mine=1 was not refused: $out"; } ;;
      library)  printf '%s\n' "$out" | grep -q '^rc=0$' || { st_bad=$((st_bad + 1)); echo "[smoke selftest] FAIL: the library answer was refused: $out"; } ;;
    esac
  done
  # No built-in origin: an unset BASE with no PUBLIC_BASE_URL to read is refused before anything runs, by name.
  out=$(env -u BASE SMOKE_ENV_FILE="$SM_DIR/no.env" SMOKE_SELFTEST=0 bash "$0" 2>&1; echo "rc=$?"); st_n=$((st_n + 1))
  printf '%s\n' "$out" | grep -q 'set BASE=https://<your instance>' && printf '%s\n' "$out" | grep -q '^rc=2$' \
    || { st_bad=$((st_bad + 1)); echo "[smoke selftest] FAIL: an unset BASE was not refused: $out"; }
  # …and an unset BASE takes the env file's PUBLIC_BASE_URL (how a caller that names no BASE smokes its own
  # stack); a BASE that is set wins over the file.
  printf 'FOO=1\nPUBLIC_BASE_URL="https://clips.example.test/"\r\n' > "$SM_DIR/a.env"; st_n=$((st_n + 2))
  out=$(BASE= SMOKE_ENV_FILE="$SM_DIR/a.env" smoke_base_)
  [ "$out" = https://clips.example.test ] || { st_bad=$((st_bad + 1)); echo "[smoke selftest] FAIL: an unset BASE did not take the env file's PUBLIC_BASE_URL (got '$out')"; }
  out=$(BASE=http://other.example.test SMOKE_ENV_FILE="$SM_DIR/a.env" smoke_base_)
  [ "$out" = http://other.example.test ] || { st_bad=$((st_bad + 1)); echo "[smoke selftest] FAIL: a set BASE did not win over the env file (got '$out')"; }
  rm -rf "$SM_DIR"
  if [ "$st_bad" = 0 ]; then echo "[smoke selftest] PASS ($st_n checks: /?mine=1 answered 301 by a stub server FAILS by name; the filtered library at 200 passes; an unset BASE takes the env file's PUBLIC_BASE_URL, and with none is refused)"; exit 0; fi
  echo "[smoke selftest] FAILED ($st_bad of $st_n)"; exit 1
fi

case $BASE in https://*) SECURE=1 ;; *) SECURE=0 ;; esac
SMOKE_MEDIA_HOST=${SMOKE_MEDIA_HOST:-}
SM_USER="e2e_$(date +%s)_sm"
SM_PASS="sm-$(date +%s)-$RANDOM$RANDOM$RANDOM"
SM_CLIP=""; SM_MADE_USER=0; SM_LOGGED_IN=0
dce() { MSYS_NO_PATHCONV=1 docker compose exec -T app "$@"; }
# ---- the teardown: exactly what this run made, whatever happened ---------------------------------------------------
sm_teardown_() {
  local code out n
  trap - EXIT
  if [ -n "$SM_CLIP" ]; then
    code=$(curl__ -sS -o /dev/null -w '%{http_code}' -X DELETE -H "$APP_HDR" -b "$(W "$JAR")" "$BASE/api/clips/$SM_CLIP" 2>/dev/null || true)
    if [ "$code" = 204 ]; then echo "[smoke] teardown: clip $SM_CLIP deleted (204)"; else echo "[smoke] teardown: NOTE the DELETE of clip $SM_CLIP answered '$code' — the next E2E's sweep removes it (its owner is inside E2E_OWN_RE)"; fi
  fi
  if [ "$SM_LOGGED_IN" = 1 ]; then curl__ -sS -o /dev/null -X POST -H "$APP_HDR" -b "$(W "$JAR")" "$BASE/api/auth/logout" 2>/dev/null || true; fi
  if [ "$SM_MADE_USER" = 1 ]; then
    # The pattern is the exact name; LIKE's `_` matches any one character, so the user list is read FIRST and the
    # prune runs only when the pattern matches this one account and nothing else.
    out=$(dce clippet admin users 2>/dev/null | tr -d '\r' | awk '{print $2}' | $PY -c "import re,sys; p=re.compile('^'+re.escape(sys.argv[1]).replace('_','.')+'\$', re.I); print(sum(1 for l in sys.stdin if p.match(l.strip())))" "$SM_USER" 2>/dev/null || echo "?")
    if [ "$out" = 1 ]; then
      n=0
      while [ "$n" -lt 30 ]; do
        out=$(dce clippet admin users prune "$SM_USER" --older-than 1s --no-clips 2>&1 | tr -d '\r' | tail -1)
        case $out in "pruned 1 user(s)"*) break ;; esac
        sleep 1; n=$((n + 1))   # the clip's row goes with its objects; until it has, --no-clips keeps the account
      done
      case $out in "pruned 1 user(s)"*) echo "[smoke] teardown: account $SM_USER removed" ;;
        *) echo "[smoke] teardown: NOTE account $SM_USER not removed ($out) — the next E2E's sweep removes it" ;; esac
    else
      echo "[smoke] teardown: NOTE the pattern '$SM_USER' matches ${out} account(s), not exactly this one — nothing pruned; the next E2E's sweep removes it"
    fi
  fi
  rm -rf "$SM_DIR"
  local secs=$((SECONDS - SM_T0))
  if [ "$SM_FAILS" = 0 ]; then echo "SMOKE PASS  ($secs s, base $BASE, $SM_SKIPS check(s) skipped with a reason above)"; exit 0; fi
  echo "SMOKE FAIL  ($secs s, base $BASE, $SM_FAILS failure(s) above)"; exit 1
}
trap sm_teardown_ EXIT
echo "[smoke] $(date -u +%Y-%m-%dT%H:%M:%SZ) base $BASE (https checks: $([ "$SECURE" = 1 ] && echo on || echo 'off: an http origin'); media host suffix: ${SMOKE_MEDIA_HOST:-any})"
[ -n "$PY" ] || die "no python >= 3.8 (python, python3, py -3)"
command -v ffmpeg >/dev/null 2>&1 || die "no ffmpeg on PATH (the fixture is made here, two seconds of test pattern)"

# 1. the stack answers, and the Worker passes the header through to the product
code=$(get "$BASE/readyz" -H "$APP_HDR"); [ "$code" = 200 ] || die "GET /readyz answered $code"
ok "GET /readyz: 200"
code=$(get "$BASE/" -H "$APP_HDR")
if [ "$code" = 200 ] && grep -q '<section class="auth front">' "$RESP"; then ok "GET / with X-Clippet-App: 1 → the product's front door (200)"
else bad "GET / with X-Clippet-App: 1 answered $code and $(grep -q '<section class="auth front">' "$RESP" && echo 'the front door' || echo 'NOT the front door') — the homepage Worker did not pass the header through"; fi
code=$(get "$BASE/")
if grep -q '<section class="auth front">' "$RESP"; then echo "[smoke]       (an anonymous GET / without the header is the product too: no homepage Worker in front of $BASE)"
else echo "[smoke]       (an anonymous GET / without the header answered $code with a page that is not the product: the homepage Worker)"; fi

# 2. the account, on THIS stack only
WANT=$(dce printenv PUBLIC_BASE_URL 2>/dev/null | tr -d '\r' | tail -1); WANT=${WANT%/}
[ "$WANT" = "$BASE" ] || die "this checkout's compose stack serves '${WANT:-nothing reachable}', not $BASE — the smoke makes its account with docker compose exec, so it runs from the checkout (or under scripts/lib/gatestack.sh run) whose stack IS $BASE"
echo "[smoke] made by this run (removed by its teardown): account $SM_USER"
SM_MADE_USER=1
out=$(printf '%s\n' "$SM_PASS" | dce clippet admin users create "$SM_USER" 2>&1) || { SM_MADE_USER=0; die "admin users create $SM_USER: $(printf '%s' "$out" | tail -1)"; }
code=$(post "$BASE/api/auth/login" -H "$APP_HDR" -c "$(W "$JAR")" -H 'Content-Type: application/json' -d "{\"username\":\"$SM_USER\",\"password\":\"$SM_PASS\"}")
[ "$code" = 200 ] || die "POST /api/auth/login answered $code"
SM_LOGGED_IN=1
COOKIE=$(hdr Set-Cookie)
case $COOKIE in *HttpOnly*) ok "the login cookie is HttpOnly" ;; *) bad "the login Set-Cookie lacks HttpOnly: ${COOKIE#*;}" ;; esac
if [ "$SECURE" = 1 ]; then
  case $COOKIE in *"; Secure"*) ok "the login cookie is Secure" ;; *) bad "the login Set-Cookie lacks Secure on an https origin: ${COOKIE#*;}" ;; esac
else skip "Secure on the session cookie: $BASE is http (the app sets Secure iff its origin is https)"; fi

# 3. /?mine=1, signed in
smoke_mine_ || true

# 4. one small upload → ready → the card
"$(command -v ffmpeg)" -v error -y -f lavfi -i "testsrc2=duration=2:size=320x240:rate=30" -f lavfi -i "sine=frequency=440:duration=2" \
  -c:v libx264 -pix_fmt yuv420p -c:a aac -shortest -movflags +faststart "$SM_DIRW\\smoke.mp4" || die "ffmpeg could not make the fixture"
printf '{"title":"smoke %s"}' "$SM_USER" > "$SM_DIR/meta.json"
code=$(post "$BASE/api/clips" -H "$APP_HDR" -b "$(W "$JAR")" -F "metadata=<$SM_DIRW\\meta.json;type=application/json" -F duplicate=allow -F "video=@$SM_DIRW\\smoke.mp4;type=video/mp4")
[ "$code" = 201 ] || die "POST /api/clips answered $code: $(head -c 300 "$RESP")"
SM_CLIP=$($PY -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8")).get("id",""))' "$(W "$RESP")" 2>/dev/null | tr -d '\r')
printf '%s' "$SM_CLIP" | grep -Eq '^[A-Za-z0-9]{6,40}$' || die "the upload's answer carries no clip id"
echo "[smoke] made by this run (removed by its teardown): clip $SM_CLIP"
t0=$SECONDS; state=""
while [ $((SECONDS - t0)) -lt 180 ]; do
  code=$(get "$BASE/api/clips/$SM_CLIP/progress" -H "$APP_HDR" -b "$(W "$JAR")")
  state=$($PY -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8")).get("state",""))' "$(W "$RESP")" 2>/dev/null | tr -d '\r')
  case $state in ready|failed) break ;; esac
  sleep 1
done
[ "$state" = ready ] || die "the clip is '$state' after $((SECONDS - t0)) s, want ready"
ok "upload → ready in $((SECONDS - t0)) s"
# A crawler is HELD (503 + Retry-After) until the card works (§20 erratum 212): wait as Discord does, ≤ 60 s.
t0=$SECONDS
while :; do
  code=$(get "$BASE/$SM_CLIP" -H "$APP_HDR" -A "$DISCORD_UA")
  [ "$code" = 503 ] && [ $((SECONDS - t0)) -lt 60 ] || break
  sleep 2
done
[ "$code" = 200 ] || die "the share page answered $code to a Discordbot $((SECONDS - t0)) s after ready"
cp "$RESP" "$SM_DIR/share.html"
missing=$($PY - "$(W "$SM_DIR/share.html")" <<'PYEOF'
import html.parser, sys
need = ['og:title', 'og:url', 'og:image', 'og:video', 'twitter:card', 'twitter:player', 'twitter:player:stream']
seen = set()
class P(html.parser.HTMLParser):
    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == 'meta' and (a.get('property') or a.get('name')) and a.get('content'):
            seen.add(a.get('property') or a.get('name'))
P().feed(open(sys.argv[1], encoding='utf-8').read())
print(' '.join(k for k in need if k not in seen))
PYEOF
)
missing=$(printf '%s' "$missing" | tr -d '\r')
[ -z "$missing" ] && ok "the share page carries the player card's tags (og:video, twitter:player, …)" || bad "the share page lacks the card tag(s): $missing"

# 5. the five card URLs: https, on the origin, 200 to both bots (a 302 to the media origin followed, as the bots do)
$PY - "$(W "$SM_DIR/share.html")" "$BASE" > "$SM_DIR/urls" <<'PYEOF'
import html.parser, sys
from urllib.parse import urljoin
KEYS = ('og:url', 'og:image', 'og:video', 'og:video:secure_url', 'twitter:player', 'twitter:player:stream', 'twitter:image')
path, base = sys.argv[1], sys.argv[2].rstrip('/') + '/'
urls = []
def add(v):
    if v:
        u = urljoin(base, v)
        if u.startswith('http') and u not in urls: urls.append(u)
class P(html.parser.HTMLParser):
    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == 'meta' and (a.get('property') or a.get('name')) in KEYS: add(a.get('content'))
        elif tag == 'link' and a.get('rel') == 'canonical': add(a.get('href'))
        elif tag == 'video': add(a.get('src')); add(a.get('poster'))
P().feed(open(path, encoding='utf-8').read())
print('\n'.join(urls))
PYEOF
tr -d '\r' < "$SM_DIR/urls" > "$SM_DIR/urls.lf"
# The four a card always names; the fifth is the player's own <video src>, which is the clip's BEST tier (§20
# erratum 211) and so differs by source — every URL the page hands out is swept below whatever it is.
for want in "/$SM_CLIP" "/api/clips/$SM_CLIP/poster.jpg" "/api/clips/$SM_CLIP/social.mp4" "/embed/$SM_CLIP"; do
  grep -Fxq "$BASE$want" "$SM_DIR/urls.lf" || bad "the card does not hand out $BASE$want (it hands out: $(tr '
' ' ' < "$SM_DIR/urls.lf"))"
done
n=0
while read -r u; do
  [ -n "$u" ] || continue
  n=$((n + 1))
  if [ "$SECURE" = 1 ]; then case $u in https://*) ;; *) bad "a card URL is not https: $u" ;; esac; fi
  case $u in "$BASE"/*) ;; *) bad "a card URL is not on $BASE: $u" ;; esac
  for pair in "Discordbot:$DISCORD_UA" "Twitterbot:$TWITTER_UA"; do
    code=$(get "$u" -H "$APP_HDR" -A "${pair#*:}")
    if [ "$code" = 302 ]; then loc=$(hdr Location); code=$(get "$loc" -A "${pair#*:}"); fi
    [ "$code" = 200 ] || bad "${pair%%:*} GET $u → $code, want 200"
  done
done < "$SM_DIR/urls.lf"
echo "[smoke]       card URLs: $(tr "
" " " < "$SM_DIR/urls.lf")"
[ "$n" -ge 5 ] && ok "$n card URLs on $BASE, 200 to Discordbot and Twitterbot$([ "$SECURE" = 1 ] && echo ', all https')" || bad "only $n card URLs on a ready share page"
[ "$SECURE" = 1 ] || skip "https on the card URLs: $BASE is http"

# 6. the three signed routes → 302 to the bucket's own origin → 200 there
code=$(get "$BASE/api/clips/$SM_CLIP/video.mp4" -H "$APP_HDR")
if [ "$code" = 200 ]; then
  skip "the 302 to the bucket: this stack is in MEDIA_DELIVERY=proxy (video.mp4 answered 200 itself)"
elif [ "$code" = 302 ]; then
  origin=""
  for r in video.mp4 social.mp4 poster.jpg; do
    code=$(get "$BASE/api/clips/$SM_CLIP/$r" -H "$APP_HDR")
    [ "$code" = 302 ] || { bad "$r answered $code, want 302 (the stack is in redirect mode)"; continue; }
    loc=$(hdr Location)
    case $loc in *X-Amz-Signature=*) ;; *) bad "$r: the Location is not a signed link: $(redact "$loc")"; continue ;; esac
    case $(hdr Cache-Control) in *no-store*) ;; *) bad "$r: a signed Location must not be stored (Cache-Control: $(hdr Cache-Control))" ;; esac
    o=$(printf '%s' "$loc" | cut -d/ -f1-3)
    [ -z "$origin" ] && origin=$o
    [ "$o" = "$origin" ] || bad "$r: signed for $o, the others for $origin (one media origin per instance)"
    [ "$o" != "$BASE" ] || bad "$r: the Location is on $BASE itself, not the bucket's origin"
    h=${o#*://}; h=${h%%:*}
    if [ -n "$SMOKE_MEDIA_HOST" ]; then case $h in *"$SMOKE_MEDIA_HOST") ;; *) bad "$r: the media origin $h is not under $SMOKE_MEDIA_HOST" ;; esac; fi
    code=$(get "$loc")
    [ "$code" = 200 ] && ok "$r → 302 → $h → 200" || bad "$r: the signed link $(redact "$loc") answered $code, want 200"
  done
else
  bad "video.mp4 answered $code, want 200 (proxy) or 302 (redirect)"
fi

# 7. no mixed content on the share page and the embed
if [ "$SECURE" = 1 ]; then
  for p in "/$SM_CLIP" "/embed/$SM_CLIP"; do
    code=$(get "$BASE$p" -H "$APP_HDR")
    if [ "$code" = 200 ] && ! grep -q 'http://' "$RESP"; then ok "$p: 200, no plain-http URL (no mixed content)"
    else bad "$p answered $code$(grep -q 'http://' "$RESP" && echo ', and it carries a plain-http URL (mixed content)')"; fi
  done
else
  skip "mixed content: $BASE is http"
fi
exit 0
