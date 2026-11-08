#!/usr/bin/env bash
# e2e.sh — live-stack end-to-end verification (DESIGN.md §16.3; the spec §5 E2E bar + §2 tag inventory).
#
# Host: Linux or macOS with Docker, curl, ffmpeg >= 6.1 and python3; Windows through WSL2 (scripts/lib/wslexec.sh
# re-runs the harness there; erratum 338), or Git Bash with native curl, ffmpeg/ffprobe and python 3
# (CLIPPET_WSL=0). None of them needs jq, sqlite3 or mc.
# Everything the harness needs from inside the stack goes through
#   dc exec -T app clippet admin …            (the SQLite file, and — `admin objects` — the STORE: whichever
#                                              backend this instance actually uses, §20 erratum 281)
#   store_obj put|rm|init …                   (PLANTING and REMOVING objects: `admin objects`' write forms
#                                              through the same `dc exec`, in the stack's OWN store with the
#                                              app's own S3_* — errata 281 (f) and 346; no `mc` anywhere)
# Git Bash discipline (§16.3/§19): W() on EVERY host path handed to a native tool (curl -c/-b/-o/-F, ffmpeg,
# python); MSYS_NO_PATHCONV=1 scoped to docker compose only (dc()); all host files under .e2e/; `python`
# before `python3` (Microsoft Store stub). Never Invoke-WebRequest. On Linux W() is the identity, the
# MSYS variable is inert and the launcher loop finds python3. NEVER run this bare in the WSL clone gate.sh
# uses (its .env is a guard and compose.yaml's own project name is the production one) — only through
# gate.sh/t1.sh there.
set -euo pipefail; cd "$(dirname "$0")/.."
E2E_DIR="$PWD/.e2e"
# ALL host files live here (jar, meta JSON, downloads); gitignored. Nothing sweeps it BY DEFAULT,
# deliberately: downloads, probe JSON, admin output and the fixture build logs are the post-mortem of a
# run that failed, and a default-on sweep would destroy exactly that. It reaches a few hundred MB, so
# `E2E_CLEAN=1 bash scripts/e2e.sh` empties it first (nit sweep S24) — nothing in the suite depends on a
# surviving file: the cookie jar is removed and recreated below, and every other path is written before
# it is read.
# It empties the RUN's files and keeps what is not the run's (§20 erratum 290, AUDIT #3): `backups/`
# (the only copies of two pre-handover tarballs and a SQLite snapshot), `checkpoints/` (E2E_RESUME's
# record of which bundles passed on which product tree), `logo/` and `bot/` (exploration files that were
# put here because it is gitignored), and the two operator files `demo-user.txt` and `webhook.txt` —
# which this function names and never reads. It prints what it is about to remove before it removes it.
# An `if`, not a `[ … ] && rm`: under `set -e` a false test on the last command of
# the line would end the run before step 1.
# `gatestack/` holds the gate stack's generated .env while that stack is up (scripts/lib/gatestack.sh): an
# E2E_CLEAN=1 run INSIDE a gate must not take the file compose reads that stack's shape from.
E2E_CLEAN_KEEP="backups logo bot demo-user.txt webhook.txt checkpoints gatestack"
e2e_clean_() {   # e2e_clean_ <dir> — remove every top-level entry of <dir> that is not on the keep-list
  local dir=$1 e name gone="" kept=""
  [ -d "$dir" ] || return 0
  for e in "$dir"/* "$dir"/.[!.]*; do
    [ -e "$e" ] || continue
    name=${e##*/}
    case " $E2E_CLEAN_KEEP " in *" $name "*) kept="$kept $name"; continue ;; esac
    gone="$gone $name"
  done
  echo "[e2e] E2E_CLEAN=1: removing from $dir:${gone:- (nothing)}"
  echo "[e2e]   keeping:${kept:- (nothing on the keep-list is present)}   (keep-list: $E2E_CLEAN_KEEP)"
  for name in $gone; do
    # On Linux a directory a root container wrote into a bind mount (an older t0/t1/check's go/, a visual
    # prove's serial/) cannot be removed by this user, and under set -e the failed rm ended the run before
    # step 1 (erratum 338). Such an entry is removed through a throwaway busybox container instead.
    # Linux only: on Git Bash NTFS ignores ownership, and a failed rm there stays the error it always was.
    if [ "$(uname -s)" != Linux ]; then rm -rf "$dir/$name"; continue; fi
    rm -rf "$dir/$name" 2>/dev/null || [ ! -e "$dir/$name" ] || {
      echo "[e2e]   $name: not removable as $(id -un 2>/dev/null || echo this user) (a root container wrote it); removing it through busybox"
      MSYS_NO_PATHCONV=1 docker run --rm -v "$dir:/clean" busybox rm -rf "/clean/$name" || { echo "[e2e] E2E_CLEAN=1: could not remove $dir/$name"; exit 1; }
    }
  done
}
if [ "${E2E_CLEAN:-0}" = 1 ]; then e2e_clean_ "$E2E_DIR"; fi
mkdir -p "$E2E_DIR"
W() { if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi; }   # EVERY host path → native tools
# dc: docker compose with path conversion disabled ONLY for docker compose (in-container paths)
# — never `export MSYS_NO_PATHCONV=1` globally: it would also stop W()-less paths like /tmp/x from
#   being converted and curl would write C:\tmp\x.
# It retries ONCE on exit 125, which is docker's OWN failure code (the daemon refused, the container
# could not be created) and never a code the `clippet` binary inside can produce — `admin` exits 0, 1
# or 2 and several steps ASSERT those, so a blanket retry would run a deliberate failure twice and
# double every side effect. 125 is the one status that means "this never reached the command".
#
# `docker_` is the one place this file spells the tool's name, and `curl__` below is the same for curl.
# The indirection buys exactly one thing: E2E_SELFTEST=1 (further down, run by scripts/check.sh) can
# replace them with a stub that fails once and then succeeds, and so exercise both retry branches
# without a daemon, a network or a stack. Neither branch has ever fired in a real run — they exist for
# a daemon that refused and a tunnel that dropped a request — so until that self-test a typo inside one
# would have surfaced for the first time during the failure it exists to absorb.
docker_() { MSYS_NO_PATHCONV=1 docker "$@"; }
# E2E_RETRY_PAUSE — how long the three retry wrappers below wait before their ONE retry. Three seconds
# in a run, and that is the only value an operator or a gate ever uses. It is a variable for exactly one
# caller: E2E_SELFTEST=1, which exercises seven retry branches against a stub with no network at all, so
# every one of those pauses was ~3s of dead clock inside `scripts/check.sh` — about 20s of a stage whose
# own description had to apologise for it. What the self-test proves is that a wrapper retries ONCE and
# that the right things are NOT retried; it has never proven, and cannot prove, that the pause is three
# seconds — so the default is now asserted explicitly instead of being felt (§20 erratum 283 (d)).
E2E_RETRY_PAUSE=${E2E_RETRY_PAUSE:-3}
# probe_env_blank — the `-e NAME=` pairs EVERY `dc run` probe starts with (dc, below, puts them first).
# A `dc run` container inherits everything compose interpolated for the app service: the operator's
# session cookie name (why SESSION_COOKIE_NAME is here, see dc), and the two OUTBOUND posts — Discord's
# webhook (DISCORD_WEBHOOK_URL/_ROUTES, step 1 exports the test one with E2E_DISCORD_WEBHOOK_URL) and
# the clip webhook (CLIP_WEBHOOK_URL/_SECRET, §20 erratum 319). A probe's throwaway clips must never
# reach the operator's receivers, and on a stack with the hook set 88-fsstore's doctor `store` row
# would change text. So they are blanked HERE, once, for all of them; a probe that needs one (55-owner's
# own test receiver, 97-hook's own URL) passes it AFTER, and compose takes the last `-e` of a name
# (measured, lane harness9: `-e X= -e X=v` → X=v). The bot's own variables are NOT here: whether a
# probe has a bot is each probe's decision, and several set them. A raw `docker_ run` of the image
# (85-bot's enrich probe, 98-form, 101, 102) inherits none of compose's environment, so it needs none.
# E2E_SELFTEST=1 pins the list, its place in the argument list, and that no drop-in starts a compose
# probe any other way.
probe_env_blank() { printf '%s\n' -e SESSION_COOKIE_NAME= -e DISCORD_WEBHOOK_URL= -e DISCORD_WEBHOOK_ROUTES= -e CLIP_WEBHOOK_URL= -e CLIP_WEBHOOK_SECRET=; }
dc() {
  local rc=0
  # Every `dc run` is a THROWAWAY probe container that boots on its own http
  # loopback (33-account's first-run probe, 51-admin's doctor, 73-ops, 85-bot,
  # 88/89/90's second instances). The operator's .env may name the session
  # cookie `__Host-…` (live on clippet.gg since 2026-09-14),
  # and the app refuses to boot with that prefix on an http base (§20 erratum
  # 28's prefix rule) — correctly, for a real instance; fatally, for a probe
  # whose cookies never reach a browser. So a probe always gets the DEFAULT
  # name. The stack under test is untouched: `up`/`exec`/`config` pass through
  # unchanged, and an empty value means "the default" to the loader. The cookie name and the outbound
  # posts are probe_env_blank's list (above).
  # shellcheck disable=SC2046
  if [ "${1:-}" = run ]; then shift; set -- run $(probe_env_blank) "$@"; fi
  docker_ compose "$@" && return 0
  rc=$?
  if [ "$rc" = 125 ]; then
    echo "[e2e]   NOTE: docker compose exited 125 (the daemon, not the command); retrying once in ${E2E_RETRY_PAUSE}s" >&2
    sleep "$E2E_RETRY_PAUSE"
    docker_ compose "$@"
    return $?
  fi
  return $rc
}
# dc_config_has_ <text> — the merged compose config holds <text>. NOT `dc config | grep -q …`: grep -q exits at its
# first match, compose's next write then gets SIGPIPE, and under pipefail the pipeline's status is 141 — a race that
# Git Bash's slow process start always won and Linux's pipes lose (step 1 died on it in the first WSL gate, DESIGN §20
# erratum 338). The config is read whole and searched as a here-string, so no early-exiting reader sits on a pipe.
dc_config_has_() { local c; c=$(dc config) || return 1; grep -Fq -- "$1" <<< "$c"; }
# ---- the hand-back of a SHARED stack, on ANY exit (§20 erratum 290, AUDIT A2) ------------------------
# Step 1 exports this run's overrides into compose's environment — the raised limiters, the 15 s sweep,
# REGISTRATION=open, the test webhook, APP_CPUS — and recreates the app with them. Until this erratum the
# ONLY place that dropped them again was the end of step 17, so every run that died before it (a core
# `die`, a killed shell, a Claude process that exited) left clippet.gg accepting registrations and
# posting to the owner's test channel until somebody typed `docker compose up -d` — a manual hand-back
# that was repeated several times over.
# So the hand-back is a FUNCTION, armed as an EXIT trap the moment the overrides are about to reach the
# app (just before step 1's `dc up -d --build`) and called by step 17 as its last act; the flag makes it
# run ONCE whichever of the two reaches it first. Bash resets caught traps in every subshell, so no
# bundle — a lane's `( … ) &` or an exclusive `( . file )` — can fire it or inherit it (the self-test
# pins that), and E2E_SELFTEST=1 exits before step 1 and never arms it. It is best effort by
# construction: every command is guarded, so it can neither change the exit status it runs under nor
# trip the ERR trap. What it cannot cover is SIGKILL and host death, which no trap sees — the run after
# one of those is what hands back, exactly as before.
E2E_HANDBACK_ARMED=0
e2e_handback_() {   # e2e_handback_ <why>
  [ "$E2E_HANDBACK_ARMED" = 1 ] || return 0
  E2E_HANDBACK_ARMED=0
  unset RATE_UPLOAD_PER_HOUR RATE_UPLOAD_PER_HOUR_PER_IP RATE_AUTH_PER_MIN SWEEP_INTERVAL REGISTRATION DISCORD_WEBHOOK_URL APP_CPUS
  if dc up -d >/dev/null 2>&1; then
    echo "[e2e]   handed back ($1): app recreated with this run's own environment dropped (the stack is shared)"
  else
    echo "[e2e]   NOTE: could not recreate the app ($1) — run 'docker compose up -d' yourself to drop"
    echo "[e2e]         this run's raised limiters, its shortened sweep interval and its open registration${E2E_DISCORD_WEBHOOK_URL:+, and its test webhook}"
  fi
  return 0
}
e2e_handback_arm_() { E2E_HANDBACK_ARMED=1; trap 'e2e_handback_ "on exit at step ${STEP:-?}"' EXIT; }
# ---- stale probe containers, swept before the stack is rebuilt (§20 erratum 290, AUDIT #11) ------------
# Three bundles boot a NAMED second app container (`clippet-e2e-scope-…`, `-owner-…`, `-fsstore-…`) and
# remove it from an EXIT trap. That trap fires on a `die`, on INT and on TERM (measured), and on nothing
# else: a SIGKILL or a host that died mid-bundle leaves the probe running on its port, holding the
# database it was pointed at, for the next run to trip over. So step 1 lists them and removes them
# BEFORE the stack is rebuilt — filtered by THIS compose project's label AND by the name prefix, so a
# lane's run can never take another stack's probe, and never by name alone. Printed before it acts.
e2e_sweep_probes_() {   # e2e_sweep_probes_ <compose project>
  local proj=$1 names n
  if [ -z "$proj" ]; then echo "[e2e]   NOTE: the compose project name could not be read; no stale probe container is swept"; return 0; fi
  names=$(docker_ ps -a --filter "label=com.docker.compose.project=$proj" --format '{{.Names}}' 2>/dev/null | tr -d '\r' | grep '^clippet-e2e-' || true)
  n=$(printf '%s' "$names" | grep -c . || true)
  if [ "$n" = 0 ]; then echo "[e2e]   stale probe containers of project $proj: none"; return 0; fi
  echo "[e2e]   stale probe containers of project $proj (an earlier run's, left by a kill): $(printf '%s' "$names" | tr '\n' ' ')"
  # shellcheck disable=SC2086
  docker_ stop -t 20 $names >/dev/null 2>&1 || echo "[e2e]   NOTE: could not stop one of them"
  # shellcheck disable=SC2086
  docker_ rm $names >/dev/null 2>&1 || echo "[e2e]   NOTE: could not remove one of them"
  return 0
}
# ---- the product key (§20 erratum 283; the `.env` fold is erratum 290, AUDIT #7) ----------------------
# The key a bundle's PASS is recorded under for E2E_RESUME: the product tree (what the binary and the
# image are built from), and since erratum 290 also the sha256 of `.env` and of every file COMPOSE_FILE
# names — because `MEDIA_DELIVERY`, `STORAGE_BACKEND`, a cookie name or a compose overlay change what a
# bundle proved without moving one tracked file. The contents are hashed and never printed. A key with
# local changes under the tracked paths records nothing and resumes nothing (`-dirty`); no git → `nogit`.
E2E_PRODUCT_PATHS="internal cmd web go.mod go.sum Dockerfile compose.yaml"
# E2E_ENVF — the dotenv file compose reads for this stack: `.env`, or the file COMPOSE_ENV_FILES names (its
# LAST entry, as compose lets a later file win) — which is how the gate's own stack is selected without
# touching the checkout's `.env` (scripts/lib/gatestack.sh, run 9). Every read of "the stack's .env" uses it.
E2E_ENVF=${COMPOSE_ENV_FILES:-.env}; E2E_ENVF=${E2E_ENVF##*,}
# THE SOURCE UNDER TEST (lane image1): compose.yaml pulls the published image, so every stack this run builds
# carries compose.build.yaml, added to the stack's own file set (the shell's COMPOSE_FILE, else the env file's
# line, else compose.yaml) by scripts/lib/buildset.sh. A lane .env needs no COMPOSE_FILE line for it.
. scripts/lib/buildset.sh
buildset_export_ "$E2E_ENVF"
e2e_product_key_() {   # e2e_product_key_ [env file]  (the argument exists for the self-test)
  local envf=${1:-$E2E_ENVF} k dirty f files
  k=$(git ls-tree HEAD -- $E2E_PRODUCT_PATHS 2>/dev/null | sha256sum | cut -c1-12)
  [ -n "$k" ] || { printf 'nogit'; return 0; }
  files=${COMPOSE_FILE:-$(sed -n 's/^COMPOSE_FILE=//p' "$envf" 2>/dev/null | tail -1 | tr -d '\r"')}
  files=$(printf '%s' "$files" | tr ':;' '  ')
  k=$({ printf '%s\n' "$k"; sha256sum < "$envf" 2>/dev/null || echo "no-env"
        for f in $files; do sha256sum < "$f" 2>/dev/null || printf 'absent %s\n' "$f"; done
      } | sha256sum | cut -c1-12)
  dirty=$(git status --porcelain -- $E2E_PRODUCT_PATHS 2>/dev/null | head -1)
  printf '%s%s' "$k" "${dirty:+-dirty}"
}
# ---- the shape a probe's bucket prefix must have before `admin objects rm --prefix` sees it (290, #28)
# Several drop-ins remove their probe's whole prefix from an EXIT trap. The prefix is a variable, the
# trap's expansion is late, and a recursive removal with an EMPTY prefix is the bucket. So every one of
# those traps asks this first: the prefix must be the harness's own minted shape, `e2e-<word>-<epoch>/`,
# and nothing else — not empty, not `/`, not a dot, not the stack's key space. Since §20 erratum 346 the
# PRODUCT refuses exactly the same (cmd/clippet/admin_objects.go, probePrefixRefusal) before any delete;
# this copy stays so a refused trap prints a line instead of spending an exec on a refusal.
store_rm_prefix_ok_() {   # store_rm_prefix_ok_ <prefix> — true when the prefix may be removed recursively
  case ${1:-} in
    ''|/|.*) echo "[e2e]   REFUSED: 'admin objects rm --prefix' on prefix '${1:-}' (empty, root or a dot-path is the bucket)"; return 1 ;;
  esac
  printf '%s' "$1" | grep -Eq '^e2e-[a-z0-9]+-[0-9]+/$' \
    || { echo "[e2e]   REFUSED: 'admin objects rm --prefix' on prefix '$1' (not the harness's e2e-<word>-<epoch>/ shape)"; return 1; }
  return 0
}
# STEP/step: every step prints its number, and the time the PREVIOUS step took is recorded so the run
# can end with a table of where it went (§20 erratum 245 — "21 minutes" was never broken down before).
STEP=0; STEP_T0=$SECONDS
step_close_() {
  if [ "${STEP:-0}" != 0 ] && [ -n "${CTX_DIR:-}" ]; then
    printf '%s\t%s\t%s\n' "$STEP" "$((SECONDS - STEP_T0))" "${CTX:-main}" >> "$CTX_DIR/step-times.txt"
  fi
}
step() { step_close_; STEP=$1; STEP_T0=$SECONDS; shift; echo "[e2e] step $STEP: $*"; }
# ---- the last five requests, on the FAIL line (§20 erratum 283 (i)) --------------------------------
# A failure used to say which STEP and what was wrong, and the two most recent gate failures were both
# diagnosed by reading hundreds of lines backwards for the request that had actually gone wrong: the
# G6 run died at step 73 on a cookie-name trap (its log is literally named
# `e2e-attempt1-step73-cookie.log`) and the proxy/redirect runs on probes inheriting the `__Host-`
# cookie name. The five requests before a failure are almost always the whole story.
#
# The ring is a FILE and not a variable, for the reason the self-test's stub counter is: every one of
# these wrappers runs inside a command substitution, so a variable it set would go with the subshell.
# It is in $CTX_DIR, so a lane's ring is its own and a bundle that dies in lane 3 prints lane 3's
# requests. Append-only: one `printf` per request and no process at all — the trimming happens in
# `die`, which runs once.
ring_() {   # ring_ <method> <url> <status>
  [ -n "${CTX_DIR:-}" ] || return 0
  case $2 in
    # A signed Location carries X-Amz-Signature and X-Amz-Credential and this file lives on disk, so
    # it goes through the same redaction every message does. The `case` is what keeps the common path
    # free of a fork: an ordinary URL is written with its query intact, which is what makes an
    # `?format=json&url=…` line worth reading.
    *X-Amz-*) printf '%s %s -> %s\n' "$1" "$(media_redact_ "$2")" "$3" >> "$CTX_DIR/requests.txt" ;;
    # An inbound or Clippet Auto URL IS its credential (the last segment), and 101-autofetch drives the
    # live one through get/http_refused: the ring names it by its shape (redact_, below), as the app's
    # own log writes /in/{token}.
    */in/c[a-z]_*|*/in/auto/c[a-z]_*) printf '%s %s -> %s\n' "$1" "$(redact_ "$2")" "$3" >> "$CTX_DIR/requests.txt" ;;
    *)        printf '%s %s -> %s\n' "$1" "$2" "$3" >> "$CTX_DIR/requests.txt" ;;
  esac
}
ring_show_() {
  [ -n "${CTX_DIR:-}" ] && [ -s "$CTX_DIR/requests.txt" ] || return 0
  echo "[e2e]   the last five requests before this failure:" >&2
  tail -5 "$CTX_DIR/requests.txt" | sed 's/^/[e2e]     /' >&2
}
die() { echo "[e2e] FAIL step $STEP: $*" >&2; ring_show_; exit 1; }
# ---- a credential's SHAPE, never its value (run 8, lane harness9) ------------------------------------
# A die that quotes what it got is the right habit everywhere but here: when the value is a capture
# token (`ck_`), an inbound or Clippet Auto URL (`…/in/ci_…`, `…/in/auto/ca_…`), a signing key or a
# throwaway receiver's query token, a shape failure would write a LIVE credential of the step's account
# into the run log — which lives on disk under .e2e/, is pasted into reports, and outlives the run.
# `redact_ <value>` prints what a reader needs to diagnose a shape failure and nothing more: everything
# up to the last `/` (the origin and the route, which is what a wrong-base failure is about), then the
# last segment cut to its DISPLAY prefix — `ck_`/`ci_`/`ca_` plus 8, the same characters `admin tokens
# list` and the mint's stderr already print — or to 4 characters for a value of no known prefix, then
# the whole value's length. `lacks_secret_f` is lacks_file_f for a credential: on a failure it names
# the value by that shape instead of quoting it. E2E_SELFTEST=1 pins both, and greps every script for
# a die / expect / lacks_file_f that would print a credential-named variable whole.
redact_() {   # redact_ <value>
  local v=$1 head='' seg
  # The head is cut too, wherever a prefixed value sits in it: a mint that printed `…/ci_<secret>/`
  # or two lines would otherwise carry the credential BEFORE the last slash.
  case $v in */*) head=$(printf '%s/' "${v%/*}" | sed -E 's/(c[a-z]_[A-Za-z0-9_-]{0,8})[A-Za-z0-9_-]*/\1…/g') ;; esac
  seg=${v##*/}
  case $seg in c[a-z]_*) seg=${seg:0:11} ;; *) seg=${seg:0:4} ;; esac
  printf "'%s%s…' (%d chars)" "$head" "$seg" "${#v}"
}
lacks_secret_f() { if grep -qF -- "$2" "$1"; then die "$3: $(redact_ "$2") found in $(basename "$1")"; fi; }   # lacks_secret_f FILE CREDENTIAL WHAT
# secret_print_lines_ <file>… — every line that would PRINT a credential-named variable whole on a
# failure: a `die "…"` string, an `expect` or a lacks_file/has_file pattern naming `$…TOK…`, `$…SECRET…`,
# `$…RAW…`, `$…WEBHOOK_URL…` or `$…COOKIE…` (the suite's names for a token, a key, a URL that is
# one, the raw value, a session cookie). A `${X#*;}` (a cookie's attributes, never its value) passes. What
# may carry one is cut out first: a `$(redact_ "…")`, a bounded prefix `$(printf '%s' "$X" | cut -c4-11)`
# (the display prefix — an unbounded `cut -c4-` is the secret's body and stays visible), and `${#X}`
# never matches (a length). Comments are skipped. The self-test fails on any line this prints.
secret_print_lines_() {
  local re='\$\{?[A-Z0-9_]*(TOK|SECRET|RAW|WEBHOOK_URL|COOKIE)[A-Z0-9_]*'
  grep -nHE -- "(die \"|(^|[^_[:alnum:]])expect |(lacks|has)_file(_f)? )" "$@" 2>/dev/null \
    | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#' \
    | sed -E 's/\$\(redact_ "[^"]*"\)//g; s/\$\{[A-Z0-9_]+#\*;\}//g; s/\$\(printf '"'"'%s'"'"' "\$\{?[A-Z0-9_]+\}?" \| cut -c[0-9]+-[0-9]+\)//g' \
    | grep -E -- "(die \"[^#]*$re|(^|[^_[:alnum:]])expect [^#;]*$re|(lacks|has)_file(_f)? [^#;]*$re)" || true
}
set -E; trap 'echo "[e2e] FAIL step $STEP: command failed (line $LINENO)" >&2; ring_show_' ERR
PY=; for c in python python3 "py -3"; do             # `python` first: on this host `python3` may be the Microsoft Store stub
  if $c -c 'import sys; assert sys.version_info >= (3, 8)' >/dev/null 2>&1; then PY=$c; break; fi
done; [ -n "$PY" ] || die "no python >= 3.8 found (tried python, python3, py -3)"
jget() { $PY scripts/lib/jget.py "$1" "$2"; }        # reads a JSON file, prints a dotted path, null → empty
envfile() { { [ -f "$E2E_ENVF" ] && sed -n "s/^$1=//p" "$E2E_ENVF" | tail -1 | tr -d '\r"'; } || true; }   # compose's .env (or COMPOSE_ENV_FILES'), when present
# BASE = the origin under test. Precedence (§20 erratum 56): an explicit `BASE=https://… bash scripts/e2e.sh`
# (the public-origin run) wins, then PUBLIC_BASE_URL from the environment, then compose's .env, then local.
# BASE must MATCH the app's own PUBLIC_BASE_URL: every card tag is absolute and is compared against BASE.
# ENV_BASE is what the APP is baked with, resolved exactly as compose resolves it: the shell
# environment first (compose interpolates ${PUBLIC_BASE_URL:-...} from it), then .env, then compose's
# own default. Reading only .env meant a shell-env value produced no warning at all, and comparing an
# untrimmed .env value against a trimmed BASE produced a spurious one; both sides are trimmed here.
ENV_BASE=${PUBLIC_BASE_URL:-$(envfile PUBLIC_BASE_URL)}; ENV_BASE=${ENV_BASE:-http://localhost:8481}; ENV_BASE=${ENV_BASE%/}
BASE=${BASE:-$ENV_BASE}; BASE=${BASE%/}
S3_BUCKET=${S3_BUCKET:-$(envfile S3_BUCKET)}; S3_BUCKET=${S3_BUCKET:-clips}
# E2E_NONLOOPBACK: 0 when BASE is this machine's own loopback, 1 when the run goes through
# SOMETHING ELSE — a tunnel, a CDN, a reverse proxy. Two things in the suite need to know the
# difference, and they used to spell it out one at a time: step 87's "nothing bodyless was charged"
# upper bound, which an edge may legitimately break by answering a HEAD with a GET, and the bounded
# retry `http_refused` performs (§20 erratum 275). One derivation, because they are one question.
# `\[::1\]` is escaped: unescaped, a case pattern reads `[::1]` as a one-character bracket expression.
case $BASE in
  http://localhost|https://localhost|http://127.0.0.1|https://127.0.0.1|http://\[::1\]|https://\[::1\]) E2E_NONLOOPBACK=0 ;;
  http://localhost:*|https://localhost:*|http://127.0.0.1:*|https://127.0.0.1:*|http://\[::1\]:*|https://\[::1\]:*) E2E_NONLOOPBACK=0 ;;
  *) E2E_NONLOOPBACK=1 ;;
esac
JAR="$E2E_DIR/cookies.txt"; JARW=$(W "$JAR")         # curl -c "$JARW" -b "$JARW"
rm -f "$JAR"

# ---- run shape: the bundle filter and the lanes (§20 erratum 245) ----------------------------------
# `bash scripts/e2e.sh` with no environment is still the WHOLE suite — the gate command never changes.
# Three knobs shape a run:
#
#   E2E_ONLY=<stems>   run ONLY these drop-in bundles (`E2E_ONLY=70-web,77-design`), on the setup every
#                      run makes: steps 1–6 (the stack with the run's raised limiters, the fixtures, the
#                      owner and the second user, clips C, E and X ready), then the log gate (16) and
#                      the hand-back (17). Since run 9 (lane `pool`) the old core steps 7–30 are bundles
#                      too, so a filter names them like any other (`E2E_ONLY=7-clip`), and the closing
#                      banner lists every bundle that did NOT run — a truncated run that printed a bare
#                      PASS reads as covered, which is the whole hazard of a filter.
#   E2E_SKIP=<stems>   the complement: the whole suite MINUS these bundles.
#   E2E_LANES=N        how many parallel-safe bundles run at once. The DEFAULT comes from this machine
#                      (e2e_lanes_default_ below, read once and printed at step 1); 1 = one at a time
#                      in name order, which is what every run did before this erratum.
#
# E2E_OVERLAP (lane `speed2`: start the pool beside the core's last steps) is GONE since run 9's lane
# `pool`: the core is in the pool, so there is nothing left for the pool to overlap. A run that still
# sets it is refused below rather than quietly ignored — a gate script carrying it should be told.
#
# A name in either list that matches no drop-in is FATAL (see dropin_select_): a typo that quietly ran
# nothing is the worst answer a filter can give.
E2E_ONLY=${E2E_ONLY:-}
E2E_SKIP=${E2E_SKIP:-}
# ---- the machine, read ONCE (lane `speed2`) ---------------------------------------------------------
# The parallel width of every stage scales to the box instead of being a literal. `nproc` is the count
# this shell may use (Git Bash and Linux); a Mac has `sysctl`; anything else falls back to getconf and
# then to 4, the width every run used before this. It is read here, once, and printed at step 1 beside
# the width it produced, so a transcript says what it was run with.
e2e_ncpu_() {
  local n
  n=$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
  n=$(printf '%s' "$n" | tr -d ' \r')
  case $n in ''|*[!0-9]*|0) n=4 ;; esac
  printf '%s' "$n"
}
# e2e_lanes_default_ <ncpu> → the parallel width when E2E_LANES is not set: half the CPUs, at least 2,
# at most 6. MEASURED on the 16-CPU box with the app pinned at APP_CPUS=4 (2026-10-05, the eighteen
# parallel bundles alone; scripts/e2e.d/README.md "How wide"): the phase took 162 s at 4, 155 s at 6,
# 157 s at 8 and 151 s at 12 — its floor is 73-ops alone (~150 s), so every slot past 6 buys noise —
# while the bundles that encode on the one app got slower as it widened (67-capture, whose step 67
# bounds a capture at 60 s, 36 s → 68 s for the whole bundle at 12). The four fixed lanes took 221 s.
e2e_lanes_default_() {
  local n=$(( ${1:-4} / 2 ))
  [ "$n" -ge 2 ] || n=2
  [ "$n" -le 6 ] || n=6
  printf '%s' "$n"
}
E2E_NCPU=$(e2e_ncpu_)
E2E_LANES_FROM="E2E_LANES"
if [ -z "${E2E_LANES:-}" ]; then E2E_LANES=$(e2e_lanes_default_ "$E2E_NCPU"); E2E_LANES_FROM="the default for $E2E_NCPU CPUs"; fi
case $E2E_LANES in ''|*[!0-9]*|0) E2E_LANES=1 ;; esac
case ${E2E_OVERLAP:-} in
  ''|0) ;;
  *) echo "[e2e] E2E_OVERLAP is gone (run 9, lane pool): the old core steps 7-30 are pool bundles now, so the pool starts right after step 6 by itself. Unset it." >&2; exit 2 ;;
esac
POLL_SLEEP=1                                         # wait_ready's poll interval; the scheduler raises it
# ---- the schedule's memory: the last run's per-bundle durations (lane `speed2`) ---------------------
# `.e2e/checkpoints/durations__<BASE>.txt`, one `<stem> TAB <seconds> TAB <utc>` per bundle that PASSED, rewritten
# after every run for the bundles that run passed and kept for the rest. The parallel phase hands its
# bundles out LONGEST FIRST (LPT): the phase can end no earlier than its longest bundle, so starting that
# one first is what keeps the tail short — name order put 67-capture (≈2 min) behind 41-social (≈2.5 min)
# in one lane. A FAILED bundle is not recorded (a run that died early would teach the planner that it is
# short). A bundle with no row yet is planned as the longest known one, so a new drop-in starts early
# rather than late. It lives in checkpoints/ because E2E_CLEAN keeps that directory.
# KEYED ON BASE (lane `nits7`): one file per origin, `durations__<BASE>.txt`, like the run checkpoints — a lane
# stack's seconds must not order a clippet.gg run's plan.
e2e_durations_file_() { printf '%s/checkpoints/durations__%s.txt' "$E2E_DIR" "$(printf '%s' "$1" | tr -c 'A-Za-z0-9' '_')"; }
E2E_DURATIONS=$(e2e_durations_file_ "$BASE")
# e2e_dur_of_ <durations file> <stem> → the recorded seconds, or nothing.
e2e_dur_of_() { { [ -s "$1" ] && awk -F'\t' -v s="$2" '$1 == s && $2 ~ /^[0-9]+$/ { d = $2 } END { if (d != "") print d }' "$1"; } || true; }
# e2e_lpt_order_ <durations file> <stem>... → the stems, longest first (unknown = the longest known, or
# 600 s with no file at all), ties in name order. Pure: the self-test drives it with a planted file.
e2e_lpt_order_() {
  local f=$1 s d max=0; shift
  if [ -s "$f" ]; then max=$(awk -F'\t' '$2 ~ /^[0-9]+$/ && $2 + 0 > m { m = $2 + 0 } END { print m + 0 }' "$f"); fi
  [ "$max" -gt 0 ] || max=600
  for s in "$@"; do
    d=$(e2e_dur_of_ "$f" "$s")
    printf '%s\t%s\n' "${d:-$max}" "$s"
  done | sort -t "$(printf '\t')" -k1,1nr -k2,2 | cut -f2
}
# e2e_durations_merge_ <durations file> <bundle-times file> <passed-stems file> — rewrite the file with
# this run's seconds for every bundle that PASSED and keep every other row. Pure, like the planner.
e2e_durations_merge_() {
  local f=$1 times=$2 passed=$3 now
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  { [ -s "$times" ] && [ -s "$passed" ]; } || return 0
  [ -e "$f" ] || : > "$f"
  awk -F'\t' -v OFS='\t' -v now="$now" '
    FILENAME == ARGV[1] { ok[$1] = 1; next }
    FILENAME == ARGV[2] { if (($1 in ok) && $2 ~ /^[0-9]+$/) fresh[$1] = $2; next }
    !($1 in fresh) && NF >= 2 { print }
    END { for (s in fresh) print s, fresh[s], now }' "$passed" "$times" "$f" | sort > "$f.new"
  mv "$f.new" "$f"
}
# ---- the drop-in markers and the pool, defined ABOVE the self-test so it can drive them on stubs ----
# (lane `speed2`). E2E_DROPIN_DIR is where the bundles live; the self-test points it at planted stubs.
E2E_DROPIN_DIR="$(dirname "$0")/e2e.d"
# The namespace registry and the change map (run 9, lane `select`): every drop-in's
# `# e2e-namespace:` and `# e2e-covers:` lines, read by bundle_account below and checked by the
# self-test. E2E_BUNDLE is the stem of the bundle running in THIS shell (set by run_bundle_ and the
# exclusive loop; empty in the core), which is how bundle_account knows whose header to read.
. scripts/lib/e2e-registry.sh
E2E_BUNDLE=
dropin_marker() { sed -n '1,40{s/^# *e2e-schedule: *//p;}' "$1" | head -1; }
dropin_after() { sed -n '1,40{s/^# *e2e-schedule-after: *//p;}' "$1" | head -1 | tr -d ' \r'; }
dropin_stems() { local f; for f in "$E2E_DROPIN_DIR"/*.sh; do [ -e "$f" ] || continue; basename "$f" .sh; done; }
# dropin_apart <file> → the stems a bundle declares it must never overlap, comma-separated.
dropin_apart() { sed -n '1,40{s/^# *e2e-schedule-apart: *//p;}' "$1" | head -1 | tr -d ' \r'; }
# dropin_phase_ <file> → `parallel`, `exclusive` (declared) or `unmarked` (no marker in the first 40
# lines, which RUNS exclusive and is announced). A marker in the old `parallel lane=<n>` syntax prints
# `fatal-lane`. Pure, so the self-test can drive it on planted files (lane `excl`).
dropin_phase_() {
  local marker
  marker=$(dropin_marker "$1")
  case $marker in
    *lane=*) echo fatal-lane ;;
    parallel*) echo parallel ;;
    exclusive*) echo exclusive ;;
    *) echo unmarked ;;
  esac
}
# e2e_pool_may_start_ <stem> <running stems> — 0 when nothing the bundle is apart from is running and
# every predecessor it names that is in THIS pool has finished.
e2e_pool_may_start_() {
  local stem=$1 running=$2 r dep
  for r in $running; do
    grep -qx "$stem $r" "$E2E_DIR/dropin-apart.txt" 2>/dev/null && return 1
  done
  for dep in $(dropin_after "$E2E_DROPIN_DIR/$stem.sh" | tr ',' ' '); do
    case " $E2E_POOL_ALL " in *" $dep "*) [ -e "$E2E_DIR/pool/$dep.rc" ] || return 1 ;; esac
  done
  return 0
}
# e2e_pool_ — the parallel phase itself: E2E_POOL_ORDER (longest first) on E2E_LANES slots. It writes
# its whole outcome to FILES (ran-/failed-bundles, bundle-times, checkpoints), never to a variable, so
# the subshells it starts can report back without a pipe (it ran in the background under E2E_OVERLAP,
# lane `speed2`, until run 9's lane `pool` removed that knob). A deadlock — bundles
# left, none running, none startable — can only be a cycle of `after`s and is fatal.
e2e_pool_() {
  local pending=$E2E_POOL_ORDER running="" stem rest free slot i n=0 started p rc
  rm -rf "$E2E_DIR/pool"; mkdir -p "$E2E_DIR/pool"
  free=""; i=1; while [ "$i" -le "$E2E_LANES" ]; do free="$free $i"; i=$((i + 1)); done
  while [ -n "$pending" ] || [ -n "$running" ]; do
    if [ -e "$E2E_DIR/pool/stop" ] && [ -n "$pending" ]; then
      echo "[e2e] pool: STOP requested — not starting:$pending"
      pending=""
    fi
    rest=""
    for stem in $pending; do
      if [ "$n" -lt "$E2E_LANES" ] && e2e_pool_may_start_ "$stem" "$running"; then
        slot=${free# }; slot=${slot%% *}; free=" ${free# }"; free=${free# $slot}
        run_bundle_ "$stem" "$slot" &
        printf '%s' "$!" > "$E2E_DIR/pool/$stem.pid"
        printf '%s' "$slot" > "$E2E_DIR/pool/$stem.slot"
        running="$running $stem"; n=$((n + 1))
        p=$(e2e_dur_of_ "$E2E_DURATIONS" "$stem")
        echo "[e2e] pool: start $stem in slot $slot (last run ${p:-?}s; $n running)"
      else
        rest="$rest $stem"
      fi
    done
    pending=$rest
    if [ -z "$running" ]; then
      [ -z "$pending" ] || { echo "[e2e] pool: DEADLOCK — nothing running and none of$pending may start (a cycle of e2e-schedule-after?)"; return 1; }
      break
    fi
    # Wait for ANY running bundle to finish: its .rc file is the signal (portable to the bash 3.2 a Mac
    # ships, which has no `wait -n`). A quarter of a second is noise against bundles that take minutes.
    started=""
    while [ -z "$started" ]; do
      for stem in $running; do
        if [ -e "$E2E_DIR/pool/$stem.rc" ]; then started="$started $stem"; continue; fi
        p=$(cat "$E2E_DIR/pool/$stem.pid" 2>/dev/null || true)
        # A runner that vanished without writing its verdict (killed) is a FAILED bundle, never a hang.
        if [ -n "$p" ] && ! kill -0 "$p" 2>/dev/null && [ ! -e "$E2E_DIR/pool/$stem.rc" ]; then
          printf '%s\n' "$stem" >> "$E2E_DIR/failed-bundles.txt"
          printf '1' > "$E2E_DIR/pool/$stem.rc"
          echo "[e2e] pool: the runner of $stem vanished without a verdict — recorded as FAILED"
          started="$started $stem"
        fi
      done
      [ -n "$started" ] || sleep 0.25
    done
    for stem in $started; do
      p=$(cat "$E2E_DIR/pool/$stem.pid" 2>/dev/null || true)
      [ -z "$p" ] || wait "$p" 2>/dev/null || true
      slot=$(cat "$E2E_DIR/pool/$stem.slot" 2>/dev/null || true)
      free="$free $slot"
      rest=""; for p in $running; do [ "$p" = "$stem" ] || rest="$rest $p"; done; running=$rest
      n=$((n - 1))
    done
  done
  return 0
}
# ---- retry ONCE on a CLASSIFIED transient (run 9, lane `retry`; scripts/e2e.d/README.md "Retry") ----
# Nine gate attempts were lost to the edge, the tunnel or the daemon (run 9's test-toil report,
# class 1: never fixed, only tolerated), each followed by the same manual cycle — recreate the tunnel,
# E2E_RESUME=1, a new attempt. So the pool does that once, by itself, and writes down why:
#   * a pool bundle that FAILS is classified by scripts/lib/transient.sh (`transient_sig`): a curl exit
#     7/28/35/52/56, an edge 502/520-524 with the edge's own evidence, a Playwright net::ERR_CONNECTION_
#     CLOSED/RESET or net:: timeout, docker's 125. Anything else — an `expect` on a status the app
#     answered above all — is NEVER retried.
#   * a classified failure runs the bundle AGAIN, once, in the same run, on a FRESH account namespace:
#     E2E_ATTEMPT=2 in the bundle's subshell, which `bundle_account` reads (the run's stamp gets a `2`,
#     `e2e_<epoch>2_<suffix>`, inside the sweep's pattern). The first attempt's log is kept as
#     `.e2e/bundle-<stem>.1.log`, and `.e2e/retries.txt` gets `stem TAB signature TAB first-attempt
#     seconds TAB utc`. A second failure is a failure; the closing banner prints every retry.
#   * E2E_RETRY=0 turns the policy off, for a run that wants to see the raw transient.
# The serial core is NOT retried here (it is not a bundle; its own wrappers — curl_, dc, http_refused —
# are what it has). The exclusive phase is not either: it runs one bundle at a time inline, and since
# lane `excl` that is 87-storage alone. And E2E_LANES=1 runs every bundle exclusive, so it retries nothing.
. scripts/lib/transient.sh
E2E_RETRY=${E2E_RETRY:-1}
case $E2E_RETRY in 0|1) ;; *) echo "[e2e] E2E_RETRY is 0 (off) or 1 (the default), not '$E2E_RETRY'" >&2; exit 2 ;; esac
E2E_ATTEMPT=1          # never read from the environment: only the pool's retry sets it, in the bundle's subshell
# e2e_bundle_once_ <stem> <slot> <attempt> — one attempt of one pool bundle, in a subshell with its own
# context and its own log (`bundle-<stem>.log`); 0 when it passed.
e2e_bundle_once_() {
  local stem=$1 slot=$2 attempt=$3 f
  f="$E2E_DROPIN_DIR/$stem.sh"
  (
    E2E_ATTEMPT=$attempt
    E2E_BUNDLE=$stem                      # whose header bundle_account reads (scripts/lib/e2e-registry.sh)
    e2e_ctx "$stem"
    STEP=0                                # the harness's last step belongs to the harness's own timing
    exec > "$E2E_DIR/bundle-$stem.log" 2>&1 < /dev/null
    printf '%s' "${BASHPID:-$(exec sh -c 'echo $PPID')}" > "$E2E_DIR/pool/$stem.inner"
    if [ "$attempt" = 1 ]; then
      echo "[e2e] ===== bundle $stem (pool slot $slot of $E2E_LANES) ====="
    else
      echo "[e2e] ===== bundle $stem (pool slot $slot of $E2E_LANES) — RETRY, attempt $attempt, on a fresh account namespace ($(transient_retry_user "${E2E_USER:-e2e_?}")_*) ====="
    fi
    # The bundle runs on the right of `||` (e2e_run_bundle_), where bash 4+ ignores errexit and the ERR trap
    # for everything inside, a sourced drop-in included: only die/expect end a bundle. macOS's bash 3.2
    # re-arms both inside a sourced file (measured: 85-bot's `contains … && die` as a function's last line
    # ended the bundle), so there they are switched off by hand — the same semantics, and bash 4+ untouched.
    if [ "${BASH_VERSINFO[0]}" -lt 4 ]; then set +eE; trap - ERR; fi
    . "$f"
    step_close_
  )
}
# e2e_retried_line_ → the closing banner's `retried:` line, from .e2e/retries.txt and failed-bundles.txt.
e2e_retried_line_() {
  local out
  out=$( { [ -s "$E2E_DIR/retries.txt" ] && awk -F'\t' -v failed="$E2E_DIR/failed-bundles.txt" '
    BEGIN { while ((getline s < failed) > 0) bad[s] = 1 }
    { printf "%s%s (%s, 1 retry%s)", (NR > 1 ? ", " : ""), $1, $2, (($1 in bad) ? ", failed again" : "") }' "$E2E_DIR/retries.txt"; } || true)
  printf 'retried: %s' "${out:-none}"
}
# e2e_run_bundle_ <stem> <slot> — what run_bundle_ (the pool's runner, defined below the self-test) does:
# an attempt, the classification and at most ONE retry, then the outcome lines and, LAST, pool/<stem>.rc.
# Here, above the self-test, so E2E_SELFTEST=1 drives it on planted bundles.
e2e_run_bundle_() {
  local stem=$1 slot=$2 t0 rc=0 sig="" first x tag
  t0=$SECONDS; tag="slot$slot"
  e2e_bundle_once_ "$stem" "$slot" 1 || rc=1
  if [ "$rc" != 0 ] && [ "$E2E_RETRY" = 1 ] && [ ! -e "$E2E_DIR/pool/stop" ]; then
    sig=$(transient_sig "$E2E_DIR/bundle-$stem.log" "$E2E_DIR/ctx-$stem")
    if [ -n "$sig" ]; then
      first=$((SECONDS - t0))
      mv -f "$E2E_DIR/bundle-$stem.log" "$E2E_DIR/bundle-$stem.1.log"
      # The first attempt's rows and ring move aside with its log: the banner's summary rows, the step
      # table and a second FAIL's request ring are the retry's alone.
      for x in summary-rows step-times requests; do
        [ ! -e "$E2E_DIR/ctx-$stem/$x.txt" ] || mv -f "$E2E_DIR/ctx-$stem/$x.txt" "$E2E_DIR/ctx-$stem/$x.1.txt"
      done
      printf '%s\t%s\t%s\t%s\n' "$stem" "$sig" "$first" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$E2E_DIR/retries.txt"
      echo "[e2e] pool: bundle $stem failed on a transient ($sig) after ${first}s — retrying it ONCE on a fresh account namespace (the first attempt's log is bundle-$stem.1.log)"
      sleep "$E2E_RETRY_PAUSE"
      if [ -e "$E2E_DIR/pool/stop" ]; then
        echo "[e2e] pool: STOP requested during the pause — $stem is not retried"
      else
        t0=$SECONDS; tag="slot$slot retry (first attempt ${first}s, $sig)"; rc=0
        e2e_bundle_once_ "$stem" "$slot" 2 || rc=1
      fi
    fi
  fi
  printf '%s\t%s\t%s\n' "$stem" "$((SECONDS - t0))" "$tag" >> "$E2E_DIR/bundle-times.txt"
  if [ "$rc" != 0 ]; then
    printf '%s\n' "$stem" >> "$E2E_DIR/failed-bundles.txt"
    ckpt_forget_ "$stem"
    echo "[e2e] pool: bundle $stem FAILED after $((SECONDS - t0))s (slot $slot${sig:+, the retry}; its log is printed below); the pool goes on"
  else
    printf '%s\n' "$stem" >> "$E2E_DIR/ran-bundles.txt"
    ckpt_record_ "$stem"
    echo "[e2e] pool: $stem done in $((SECONDS - t0))s (slot $slot${sig:+, on the retry})"
  fi
  printf '%s' "$rc" > "$E2E_DIR/pool/$stem.rc"
}

# ---- HTTP helpers: every body is a file under .e2e/, every status is captured ---------------------------------------
# The three response files and the summary sink are PER CONTEXT: the harness itself runs in `main`, and
# a parallel bundle runs in one of its own, because two bundles writing `resp.json` at the same time
# would each read the other's answers. e2e_ctx is the first thing a bundle subshell calls.
# The two accumulators — the summary rows and the step times — are emptied ONCE, here, for every
# context this or an earlier run made. Emptying them inside e2e_ctx would look right and be wrong: the
# exclusive bundles all run in `main` and re-enter it one after another, so each would have wiped the
# rows the one before it wrote. The response BODIES under each context are deliberately left alone —
# they are the post-mortem of the run that failed, which is why nothing sweeps `.e2e/` by default.
rm -f "$E2E_DIR"/ctx-*/summary-rows.txt "$E2E_DIR"/ctx-*/step-times.txt "$E2E_DIR"/ctx-*/requests.txt "$E2E_DIR"/step-times.txt "$E2E_DIR"/ctx-*/requests.txt "$E2E_DIR"/step-times.txt
e2e_ctx() {
  CTX=$1; CTX_DIR="$E2E_DIR/ctx-$1"; mkdir -p "$CTX_DIR"
  RESP="$CTX_DIR/resp.json"; RESPW=$(W "$RESP")
  HDR="$CTX_DIR/resp.hdr";   HDRW=$(W "$HDR")
  PAGE="$CTX_DIR/page.html"; PAGEW=$(W "$PAGE")
  SUMMARY_SINK="$CTX_DIR/summary-rows.txt"
}
e2e_ctx main
# curl_: curl, retried ONCE on the three exit codes that mean the request never got an answer —
# 7 (could not connect), 28 (timed out), 35 (TLS handshake) — with a 3 s pause and a line saying so.
# A quick tunnel degrades after days and drops a request mid-run; one lost connection
# should cost three seconds, not a 21-minute run. The first attempt's stdout is DISCARDED before the
# retry: curl still writes `%{http_code}` (as `000`) when it fails, so appending would hand the caller
# `000200`. It is used by the READ helpers only — see `http` below.
#
# E2E_APP_HDR — every request the wrappers make (`curl_`, and so get/page/head_ and the media helpers;
# `http`; `http_refused`) carries `X-Clippet-App: 1`. On clippet.gg a Worker serves the product's
# HOMEPAGE to an anonymous GET/HEAD of the exact root `/`, and passes through anything carrying a
# session cookie or this header (DEPLOY §37). The suite's front-door steps (80-front, and every
# anonymous `page "$BASE/"`) must see the PRODUCT there, so the header rides on every request — one
# place instead of each root fetch, and harmless where there is no Worker (a lane stack, dev, a
# self-host: the app ignores it). A `curl --next` chain (burst_429) carries it on its first block only;
# no burst targets `/`. The self-test pins it on all five wrappers.
E2E_APP_HDR='X-Clippet-App: 1'
# --connect-timeout 10 (erratum 338): under WSL's mirrored networking a closed loopback port TIMES OUT (curl 28)
# instead of refusing (curl 7), and with no bound curl waits its own default of minutes; 28 is in every retry set.
curl__() { curl --connect-timeout 10 "$@"; }         # the self-test's injection point; see docker_ above
curl_() {
  local rc=0 out
  out=$(curl__ -H "$E2E_APP_HDR" "$@") || rc=$?
  case $rc in
    7|28|35)
      echo "[e2e]   NOTE: curl exit $rc (no answer); retrying once in ${E2E_RETRY_PAUSE}s" >&2
      sleep "$E2E_RETRY_PAUSE"
      rc=0; out=$(curl__ -H "$E2E_APP_HDR" "$@") || rc=$?
      ;;
  esac
  printf '%s' "$out"
  return $rc
}
# http() is NOT retried: it carries POST/PATCH/PUT/DELETE, and exit 28 can mean the request landed and
# the ANSWER was lost — re-sending it would mint a second clip, a second token or a second collection.
# A read that is repeated costs a round trip; a write that is repeated costs a wrong assertion.
http()  { local m=$1 u=$2 c rc=0; shift 2; c=$(curl__ -H "$E2E_APP_HDR" -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -X "$m" "$@" "$u") || rc=$?
          ring_ "$m" "$u" "${c:-no-answer}"; printf '%s' "$c"; return $rc; }                                 # → status; body $RESP
# http_refused: the ONE class of write that may be retried — a write whose EXPECTED answer is a
# REFUSAL (§20 erratum 275). Same arguments, same outputs and the same status as http().
#
# The reason the rule above does not apply to it is the whole of it: a request the origin refuses
# has NO SIDE EFFECT TO DOUBLE. A repeated 401 is a 401, a repeated 403 is a 403; nothing was
# minted the first time and nothing is minted the second. Every other write — one whose SUCCESS a
# step asserts — is still never retried, and http() is what carries those.
#
# What it retries, and only through a NON-LOOPBACK origin (E2E_NONLOOPBACK): curl's 7/28/35, the
# three exits that mean the request got no answer at all, and a 502 or 520-524, which are an EDGE's
# own vocabulary for "I could not reach or read from the origin" and never something this app
# emits. On loopback there is no edge to absorb and a 502 would be a real answer worth failing on,
# so it retries nothing and behaves exactly as http() does.
#
# This is the accommodation for the failure that cost the 2026-09-11 gate three runs:
# the tunnel dropped multipart POSTs the app refuses BEFORE reading the body — steps 48 and 49 —
# and the origin answered every one of them correctly in isolation. The earlier, wider idea (a
# retry keyed on the request id) was deferred because the id reaches the client only in a response
# header, which is exactly what such a failure loses (§20 erratum 269's deferred list, item ii).
# This one needs no correlation at all, because there is nothing to correlate: the request left no
# trace to find.
http_refused() {
  local m=$1 u=$2; shift 2
  local rc=0 code
  code=$(curl__ -H "$E2E_APP_HDR" -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -X "$m" "$@" "$u") || rc=$?
  if [ "${E2E_NONLOOPBACK:-0}" = 1 ]; then
    case "$rc:$code" in
      7:*|28:*|35:*|0:502|0:520|0:521|0:522|0:523|0:524)
        echo "[e2e]   NOTE: retry (edge): $m $u answered '${code:-none}' (curl exit $rc); a refused write has no side effect to double" >&2
        sleep "$E2E_RETRY_PAUSE"
        rc=0; code=$(curl__ -H "$E2E_APP_HDR" -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -X "$m" "$@" "$u") || rc=$?
        ;;
    esac
  fi
  ring_ "$m" "$u" "${code:-no-answer}"
  printf '%s' "$code"
  return $rc
}
# setup_once_ <what> <command …>: the SERIAL SETUP's writes (steps 3-4: the two registrations, the three
# uploads) get the pool's one retry on a classified transient (run 10, lane `nits10`; run 9
# `[pool]`). The pool re-runs a failed bundle on a fresh namespace; the setup cannot be re-run in place
# (every bundle reads the variables it sets), so its retry is per request, and narrower than http()'s
# rule above on purpose: ONLY when curl says the request was never sent — exit 7 (could not connect), or
# exit 28 with curl's own `Failed to connect` / `Connection timed out` (the connect phase timed out: the
# gate-29 lane met `curl: (28) Failed to connect to localhost port 8580 after 21046 ms` twice) — and
# ONLY on the stack's own loopback. A 28 after the connection opened (`Operation timed out … bytes
# received`) may have landed, and is never re-sent. The retry writes `setup-step<N> TAB curl-<exit> TAB
# first-attempt seconds TAB utc` to retries.txt, the pool's row, so the closing banner names it.
# E2E_RETRY=0 turns it off with the pool's. The command's stdout and status are handed back as http()'s.
setup_unsent_() {   # setup_unsent_ <curl exit> <its stderr file> → 0 when the request never left
  case $1 in
    7) return 0 ;;
    28) grep -qE 'Failed to connect|Connection timed out' "$2" 2>/dev/null ;;
    *) return 1 ;;
  esac
}
setup_once_() {
  local what=$1 rc=0 out t0 err="$E2E_DIR/setup-once.err"; shift
  t0=$(date +%s)
  out=$("$@" 2> "$err") || rc=$?
  if [ "$rc" != 0 ] && [ "${E2E_NONLOOPBACK:-0}" = 0 ] && [ "${E2E_RETRY:-1}" = 1 ] && setup_unsent_ "$rc" "$err"; then
    cat "$err" >&2
    printf '%s\t%s\t%s\t%s\n' "setup-step${STEP:-?}" "curl-$rc" "$(( $(date +%s) - t0 ))" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$E2E_DIR/retries.txt"
    echo "[e2e]   NOTE: setup ($what): curl exit $rc before the request was sent; retrying it once in ${E2E_RETRY_PAUSE}s (retries.txt)" >&2
    sleep "$E2E_RETRY_PAUSE"
    rc=0; out=$("$@" 2> "$err") || rc=$?
  fi
  cat "$err" >&2; rm -f "$err"
  printf '%s' "$out"
  return $rc
}
# Each records the request it just made and then hands the caller EXACTLY what it always did: the
# status on stdout and curl's own exit status, which several callers rely on under `set -e`.
get()   { local u=$1 c rc=0; shift; c=$(curl_ -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' "$@" "$u") || rc=$?
          ring_ GET "$u" "${c:-no-answer}"; printf '%s' "$c"; return $rc; }
page()  { local u=$1 c rc=0; shift; c=$(curl_ -sS -o "$PAGEW" -D "$HDRW" -w '%{http_code}' "$@" "$u") || rc=$?
          ring_ GET "$u" "${c:-no-answer}"; printf '%s' "$c"; return $rc; }                                   # → status; body $PAGE
head_() { local u=$1 c rc=0; shift; c=$(curl_ -sS -I -o "$HDRW" -w '%{http_code}' "$@" "$u") || rc=$?
          ring_ HEAD "$u" "${c:-no-answer}"; printf '%s' "$c"; return $rc; }                                  # → status; headers $HDR
hdr()   { sed -n "s/^$1: *//Ip" "$HDR" | tail -1 | tr -d '\r'; }                                             # case-insensitive header value
json()  { jget "$RESPW" "$1"; }
expect() { [ "$1" = "$2" ] || die "$3: got '$1', want '$2'"; }                                               # expect ACTUAL WANT WHAT
expect_nonempty() { [ -n "$1" ] || die "$2: empty"; }
later() { [ "$1" \> "$2" ] || die "$3: '$1' is not after '$2'"; }                                             # RFC3339 UTC sorts lexically
not_before() { [ ! "$1" \< "$2" ] || die "$3: '$1' is before '$2'"; }                                         # same, allowing equality
# expect_ttl_ahead ISO SECONDS WHAT — the timestamp is SECONDS from now, ±10 minutes (host and app share a clock)
expect_ttl_ahead() {
  $PY - "$1" "$2" <<'PYEOF' || die "$3"
import calendar, sys, time
got = calendar.timegm(time.strptime(sys.argv[1], '%Y-%m-%dT%H:%M:%SZ'))
delta = got - time.time() - float(sys.argv[2])
if abs(delta) > 600:
    sys.exit('%s is %.0fs off the expected now+%ss' % (sys.argv[1], delta, sys.argv[2]))
PYEOF
}
expect_empty() { [ -z "$1" ] || die "$2: got '$1', want empty/absent"; }
contains() { case "$1" in *"$2"*) return 0;; esac; return 1; }                                               # contains HAYSTACK NEEDLE
lacks_file() { if grep -q -- "$2" "$1"; then die "$3: '$2' found in $(basename "$1")"; fi; }                 # lacks_file FILE SUBSTR WHAT
has_file() { grep -q -- "$2" "$1" || die "$3: '$2' missing from $(basename "$1")"; }
# The LITERAL pair. The two above are basic-regex greps, so `.` matches anything and `[` opens a
# character class — fine for an HTML attribute, wrong for a CSS selector or a bracketed attribute
# selector, which step 81 worked around by amputating the opening bracket of `[data-playing]`. These
# take the pattern as plain text (`grep -F`) and are what a pin on exact source should use; the regex
# pair stays for the handful of patterns that want one.
lacks_file_f() { if grep -qF -- "$2" "$1"; then die "$3: '$2' found in $(basename "$1")"; fi; }              # lacks_file_f FILE LITERAL WHAT
has_file_f() { grep -qF -- "$2" "$1" || die "$3: '$2' missing from $(basename "$1")"; }

# ---- reaching a rate limiter: one curl process per CHUNK, not one per request (§20 erratum 283 (f)) --
# Two steps prove a ceiling by reaching it: 62-tiers step 66 on the `poll` bucket (120/min/IP) and
# 55-owner step 56 on the `view` one (60/min/IP). Each fired one `curl` PROCESS per request in a while
# loop — 203 of them in the last public gate, 361 on a lane stack, each a fork, a fresh connection and,
# through an edge, a fresh TLS handshake. Measured on this project's own loopback: forty requests are
# 9.8 s as forty processes and 0.29 s as one.
#
# What does NOT change, and this is the whole point of doing it this way rather than with a probe
# container and a lowered ceiling: the limiter is still reached on the LIVE stack at its own shipped
# ceiling, every request before the refusal must still be 200, the refusal must still arrive inside the
# same bound, and the 429's envelope is still read off a real refused response. The requests are the
# same requests; only the number of processes making them is different.
#
# burst_429 <url> <max> <what> [apart] — GET <url> until it is refused, at most <max> times, in chunks of
# E2E_BURST_CHUNK inside ONE `curl --next` process each. Sets BURST_N to the number of 200s before the
# first 429, and leaves $RESP/$HDR holding a REFUSED answer for the caller to assert the envelope on.
#
# `apart` (lane `excl`, §20 erratum 245 amended): the same requests from a CLIENT ADDRESS OF THEIR OWN.
# Both buckets a burst reaches are keyed by the client's address (internal/clientip), and every other
# request of the run comes from ONE address — the host's — so a burst from the host empties the bucket
# every other bundle's preview-GIF poll, filmstrip poll and `wait_ready` draws on. That, and nothing
# else, is what kept 55-owner and 62-tiers out of the parallel pool. With `apart` the burst is fired
# from a throwaway container of this compose project (`clippet-e2e-burst-…`, the app's own image, which
# carries curl) at the LIVE app by its service name, `http://app:8080`, on the compose network: the
# same process, the same shipped ceiling, the same route and query, every pre-refusal answer still
# asserted 200, the refusal still required inside <max>, the envelope still read off a real refused
# answer — and a bucket no other bundle can touch, because no other request of the run comes from that
# container's address. (`compose run` does not give the container the service's alias, so `app`
# resolves to the stack's own app and never to the burst container.) What it does NOT cover is the
# public edge in front of the origin: on clippet.gg the burst no longer crosses Cloudflare. The edge
# changes nothing a burst asserts — the bucket is keyed on the address the app resolves, and the
# app's own resolution of a forwarded address is step 73's and internal/clientip's business. The
# container sleeps for at most E2E_BURST_TTL seconds and is `--rm`, so a bundle that dies mid-burst
# leaves nothing past that (and step 1 removes any `clippet-e2e-*` container of this project anyway).
E2E_BURST_TTL=${E2E_BURST_TTL:-300}
# burst_url_apart_ <url> → the same path and query on the live app's service address.
burst_url_apart_() { local p=${1#*://}; case $p in */*) p=/${p#*/} ;; *) p=/ ;; esac; printf 'http://app:8080%s' "$p"; }
# owner_of_listing_ < `admin users` output → the owner's username (`owner=yes` ends its line), or nothing.
# Pure, so E2E_SELFTEST=1 pins it on planted listings; step 3 sets E2E_IS_OWNER from it (§20 erratum 349).
owner_of_listing_() { tr -d '\r' | awk '!f && / owner=yes$/ { print $2; f = 1 }'; }   # reads to the end: no SIGPIPE under pipefail
#
# It goes through `curl_`, so a chunk whose connection dies is re-fired once like every other read in
# this suite. That re-fire replays the chunk's requests, which can only make BURST_N smaller than the
# number actually sent — the count is printed, never asserted, and neither the "every one before it was
# 200" nor the "refused inside <max>" claim depends on it.
E2E_BURST_CHUNK=${E2E_BURST_CHUNK:-25}
BURST_N=0
burst_429() {
  local u=$1 max=$2 what=$3 via=${4:-} n=0 chunk i out code hit=0 tries=0 ct=""
  BURST_N=0
  if [ "$via" = apart ]; then
    ct="clippet-e2e-burst-${CTX:-main}-${BASHPID:-$(exec sh -c 'echo $PPID')}"
    docker_ rm -f "$ct" >/dev/null 2>&1 || true
    dc run -d --rm --no-deps --name "$ct" --entrypoint sleep app "$E2E_BURST_TTL" >/dev/null \
      || die "$what: could not start the burst container $ct"
    u=$(burst_url_apart_ "$u")
    echo "[e2e]   $what: the burst comes from $ct (an address of its own) at $u"
  elif [ -n "$via" ]; then
    die "burst_429: unknown source '$via' (want nothing or 'apart')"
  fi
  while [ "$n" -lt "$max" ] && [ "$hit" = 0 ]; do
    chunk=$E2E_BURST_CHUNK
    [ $((n + chunk)) -le "$max" ] || chunk=$((max - n))
    set --
    i=0
    while [ "$i" -lt "$chunk" ]; do
      [ "$i" = 0 ] || set -- "$@" --next
      # A SPACE and not a `\n`: curl on Git Bash writes CRLF for the newline escape, and `for code in
      # $out` then splits into `200<CR>` tokens that compare equal to nothing (found by the first run
      # of this helper, whose failure read `got '200', want '200'`). A space needs no escape at all.
      set -- "$@" -sS -o /dev/null -w '%{http_code} ' "$u"
      i=$((i + 1))
    done
    if [ -n "$ct" ]; then
      out=$(docker_ exec "$ct" curl -H "$E2E_APP_HDR" "$@" | tr -d '\r') || die "$what: curl failed in $ct during the burst after $BURST_N request(s)"
    else
      out=$(curl_ "$@") || die "$what: curl failed during the burst after $BURST_N request(s)"
    fi
    for code in $out; do
      [ "$hit" = 0 ] || continue                 # everything after the first refusal is the bucket's own business
      if [ "$code" = 429 ]; then hit=1; continue; fi
      expect "$code" 200 "$what: burst request $BURST_N"
      BURST_N=$((BURST_N + 1))
    done
    n=$((n + chunk))
  done
  expect "$hit" 1 "$what: a burst of $max requests was refused"
  # THE ENVELOPE, read from a refused answer of its own, and read the same way the burst was made.
  # The chunks above discard their bodies, and a single follow-up request will NOT do instead: these
  # buckets refill (the `poll` one at 2/s, the `view` one at 1/s) while starting a `curl` PROCESS costs
  # about a third of a second on this host, so single requests arrive SLOWER than the refill and are
  # simply admitted again — measured, after a burst that had just been refused, seven of eight
  # follow-ups came back 200. So the envelope read is itself a short burst inside one process, and only
  # its LAST block writes $RESP and $HDR, which also settles any question about what a repeated `-o`
  # does across `--next` blocks: exactly one response is ever written to those two files.
  while [ "$tries" -lt 5 ]; do
    set --
    i=0
    while [ "$i" -lt 9 ]; do
      [ "$i" = 0 ] || set -- "$@" --next
      set -- "$@" -sS -o /dev/null -w '%{http_code} ' "$u"
      i=$((i + 1))
    done
    if [ -n "$ct" ]; then
      set -- "$@" --next -sS -o /tmp/burst-resp -D /tmp/burst-hdr -w '%{http_code}' "$u"
      out=$(docker_ exec "$ct" curl -H "$E2E_APP_HDR" "$@" | tr -d '\r') || die "$what: curl failed in $ct reading the refusal"
      code=${out##* }
      if [ "$code" = 429 ]; then
        docker_ exec "$ct" cat /tmp/burst-resp > "$RESP" || die "$what: could not read the refusal's body out of $ct"
        docker_ exec "$ct" cat /tmp/burst-hdr > "$HDR" || die "$what: could not read the refusal's headers out of $ct"
        docker_ rm -f "$ct" >/dev/null 2>&1 || true
        return 0
      fi
    else
      set -- "$@" --next -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' "$u"
      out=$(curl_ "$@") || die "$what: curl failed reading the refusal"
      code=${out##* }
      [ "$code" = 429 ] && return 0
    fi
    tries=$((tries + 1))
  done
  [ -z "$ct" ] || docker_ rm -f "$ct" >/dev/null 2>&1 || true
  die "$what: the burst was refused after $BURST_N request(s), but five bursts of ten could not get a refusal back to read"
}

# bundle_account <suffix> — register `${E2E_USER}_<suffix>` on the stack under test and sign it in, for
# a bundle whose steps change something ACCOUNT-WIDE (a password, the session list, the upload
# defaults, the default link grammar) and so must not do it to the run's owner while other bundles
# upload, log in and list as that owner beside it (lane `excl`, §20 erratum 245 amended). Sets BA_USER,
# BA_JAR and BA_JARW (the jar lives in the bundle's own context directory). The suffix is one to three
# of [a-z0-9] — exactly what erratum 272's sweep pattern `^e2e_[0-9]{9,11}(_[a-z0-9]{1,3})?$` admits —
# so the account, and anything a died bundle left it owning, is swept by the next run's step 1 and
# pruned by this run's step 17 like every other suffix. The password is the run's own.
# ON A RETRY (run 9, lane `retry`: the pool re-runs a bundle that failed on a classified transient, with
# E2E_ATTEMPT=2) the account is a FRESH one, `bundle_user_` below: the first attempt may have registered
# the suffix already, and a second register of it is a 409.
# The suffix must be one the CALLING bundle declares (`# e2e-namespace: account=…`, run 9 lane `select`):
# a hand-picked suffix cannot drift into another bundle's, which is how gate 27a's 409 happened. Called
# outside a bundle (E2E_BUNDLE empty) it is refused too — the core's own account is `_b` (step 3). The
# check reads the SUFFIX, never BA_USER: a retry changes the stamp, not the suffix.
bundle_user_() {   # bundle_user_ <suffix> → the account name this attempt registers for it
  if [ "${E2E_ATTEMPT:-1}" = 1 ]; then printf '%s_%s' "$E2E_USER" "$1"; else printf '%s_%s' "$(transient_retry_user "$E2E_USER")" "$1"; fi
}
bundle_account() {
  printf '%s' "${1:-}" | grep -Eq '^[a-z0-9]{1,3}$' \
    || die "bundle_account: suffix '${1:-}' is not 1-3 of [a-z0-9] (the sweep could never reach it)"
  [ -n "${E2E_BUNDLE:-}" ] || die "bundle_account $1: called outside a bundle (E2E_BUNDLE is empty), so there is no header to declare it"
  local ba_decl
  ba_decl=$(reg_field_ "$E2E_DROPIN_DIR/$E2E_BUNDLE.sh" account)
  case " $ba_decl " in
    *" $1 "*) ;;
    *) die "bundle_account: suffix '$1' is not declared by $E2E_BUNDLE.sh — its '# e2e-namespace: account=' declares: ${ba_decl:-none} (scripts/lib/e2e-registry.sh)" ;;
  esac
  BA_USER=$(bundle_user_ "$1")
  printf '%s' "$BA_USER" | grep -Eq "$E2E_OWN_RE" \
    || die "bundle_account: '$BA_USER' is outside the sweep's pattern (attempt ${E2E_ATTEMPT:-1}; the sweep could never reach it)"
  BA_JAR="${CTX_DIR:-$E2E_DIR}/cookies-acct-$1.txt"; BA_JARW=$(W "$BA_JAR"); rm -f "$BA_JAR"
  local code
  code=$(http POST "$BASE/api/auth/register" -c "$BA_JARW" -H 'Content-Type: application/json' \
    -d "{\"username\":\"$BA_USER\",\"password\":\"$E2E_PASS\"}")
  expect "$code" 201 "register the bundle's own account $BA_USER ($(cat "$RESP"))"
  echo "[e2e]   this bundle's own account: $BA_USER"
}

# e2e_notify_scoped_ <`admin jobs --kind notify` listing> <clip id>… → how many notify rows in the listing
# name one of the given clips (run 9, lane `pool`). Step 20 (20-pages.sh) asserts that Discord auto-post
# queued nothing for the RUN's clips; it used to read `admin stats`' `notify=<n>`, a count over the WHOLE
# database, which another account's row — an aborted positive run's leftover on a shared stack, or any
# bundle beside it in the pool — would move. The listing names each row's clip (`#<id> clip=<id> kind=…`),
# so the count is scoped by filtering it: pure, and pinned by E2E_SELFTEST=1 on a planted listing.
e2e_notify_scoped_() {
  local f=$1; shift
  awk -v ids=" $* " '
    /^#[0-9]+ / {
      for (i = 2; i <= NF; i++) if ($i ~ /^clip=/) { c = substr($i, 6); if (index(ids, " " c " ")) n++; break }
    }
    END { print n + 0 }' "$f"
}

# ---- media delivery: the suite runs on BOTH shapes (§20 erratum 280) --------------------------------
# `MEDIA_DELIVERY=redirect` (§20 erratum 278, DEPLOY §33) makes three media routes — `video.mp4`,
# `social.mp4`/`light.mp4` and `poster.jpg` — answer **302** to a freshly presigned URL on the bucket's
# own origin, after every gate they have always run. The reference stack has been on that shape since
# 2026-09-14, so the gate must pass on it AND on a lane stack that proxies; a harness that could only
# read one of them is a harness that can no longer prove the product.
#
# Two rules decide everything below.
#
#   * **The mode is DETECTED, from the stack, once, and printed.** Never read out of `.env` and never
#     passed in: `.env` is what the operator MEANT and the answer on the wire is what the stack DOES,
#     and the second is the only one an assertion may rest on. The detection is step 6's, the moment
#     the run's first clip is ready.
#   * **An assertion is converted, never weakened.** Where the bytes are the point (a download for
#     ffprobe, a byte count, a colour check, the object's own validators) the helpers below follow the
#     302 and the assertion stands unchanged in substance. Where the APP's own behaviour on the media
#     route is the point (its 416 envelope, its `If-Match` 412, the headers a 302 must not carry) the
#     assertion runs in `proxy` as it always did and in `redirect` becomes the redirect's own truth
#     where one exists — otherwise it stands down with a PRINTED line (`media_skip`), never silently,
#     and step 90 proves it in both modes on an instance of its own.
#
# `/original`, `preview.gif` and `filmstrip.jpg` are app-served in EVERY mode (erratum 278(a)) and
# their sites are untouched; the three assertions that pin that are at the detection block, step 22
# and step 71.
E2E_MEDIA=             # proxy | redirect — set by the detection block after step 6, never before
E2E_MEDIA_ORIGIN=      # scheme://host[:port] the presigned Location lands on, in redirect mode only
MEDIA_LOC=             # the last Location a media helper followed (redacted in every message)
# media_origin_ <url> → `scheme://host[:port]`, and nothing else. The first three `/`-separated fields
# of any absolute URL are exactly that, whatever the query string holds.
media_origin_() { printf '%s' "$1" | cut -d/ -f1-3; }
# media_redact_ <url> → the URL with its query replaced by a marker. EVERY message that names a
# presigned Location goes through this: the query carries `X-Amz-Signature` and `X-Amz-Credential`,
# which are a credential and the access key id (DEPLOY §33.2), and a run's transcript is a file on
# disk. The path is kept because the path is what a post-mortem needs.
media_redact_() { printf '%s?<signed>' "${1%%\?*}"; }
# media_total_ <Content-Range value> — the COMPLETE LENGTH out of a range header, or empty when there
# is not one: `bytes 0-0/12345` and `bytes */12345` both give `12345`, and `bytes 0-0/*` gives nothing.
# Split out of media_head so E2E_SELFTEST can pin it against planted strings (§20 erratum 283 (h)).
# Every SIZE comparison in the suite rests on it in redirect mode — media_head rewrites Content-Length
# from its answer — so a wrong one would quietly compare one byte against a whole rendition.
media_total_() {
  local t=${1##*/}
  case ${t:-x} in
    ''|*[!0-9]*) printf '' ;;
    *) printf '%s' "$t" ;;
  esac
}
# media_no_cookie_ <curl args…> — the one thing a media helper refuses to forward. In `redirect` mode
# the second hop goes to the BUCKET, which is a third party, and a session cookie must never be sent
# to one. No caller needs it: `/original` is the only media route with an identity gate and erratum
# 278(a) never redirects it, so this can only ever fire on a harness mistake.
media_no_cookie_() {
  local a
  for a in "$@"; do
    case $a in
      -b|--cookie) die "a media helper was given a cookie jar: in redirect mode that would send the session to the bucket" ;;
    esac
  done
}
# media_redirect_ <get|head_> <url> [curl args…] — the APP hop of a media request in redirect mode,
# and every assertion erratum 278(g) makes about it: 302, a signed `Location` on the run's ONE media
# origin, `no-store`, and no validator for bytes this response does not hold. The caller's own
# arguments go to this hop too, which costs nothing and pins one more clause of the same erratum: a
# `Range` or an `If-None-Match` does not change the answer, because a 302 has no representation to
# range over.
#
# The first argument is the harness's own GET or HEAD wrapper, and passing it through matters twice:
# erratum 278(g) says the two methods get the SAME 302 and this asserts it on every media request the
# suite makes, not only in step 90; and erratum 278(i) charges the served-bytes counter for a redirect
# GET and NOTHING for a HEAD, so a `media_head` that quietly sent a GET would put bytes nobody
# received into the very figure step 87 measures.
# The Location is left in $MEDIA_LOC. Never called in proxy mode.
media_redirect_() {
  local how=$1 u=$2 code; shift 2
  code=$("$how" "$u" "$@")
  [ "$code" = 302 ] || die "media $u: got '$code', want 302 (this stack reported MEDIA_DELIVERY=redirect)"
  MEDIA_LOC=$(hdr Location)
  [ -n "$MEDIA_LOC" ] || die "media $u: a 302 with no Location"
  case $MEDIA_LOC in
    *X-Amz-Signature=*) ;;
    *) die "media $u: the Location is not a signed link: $(media_redact_ "$MEDIA_LOC")" ;;
  esac
  # `contains` and not an equality: this hop may come through an edge, which is allowed to add to a
  # Cache-Control it must not remove from. What matters is that a link with an expiry in it is never
  # stored — a cache that kept one would go on handing out a signature that has stopped working.
  contains "$(hdr Cache-Control)" no-store || die "media $u: a signed Location must not be stored (Cache-Control: $(hdr Cache-Control))"
  expect_empty "$(hdr ETag)" "media $u: the 302 carries a validator for bytes it does not hold"
  # ONE origin for the whole run, asserted on every redirect the suite follows. A stack that signed
  # some links for one host and some for another would be a stack whose CSP can only name one of them
  # (erratum 278(j)), and it would pass every per-request assertion below.
  expect "$(media_origin_ "$MEDIA_LOC")" "$E2E_MEDIA_ORIGIN" "media $u: the Location's origin"
}
# media_get <url> [curl args…] — "fetch the BYTES of this media route", in whichever shape the stack
# is in. In `proxy` it IS `get`, argument for argument. In `redirect` it asserts the app's 302 and
# then fetches the Location with the caller's own extra arguments, so a `Range:` or an
# `If-None-Match:` reaches the bucket — MinIO, R2, B2 and S3 all honour ranges and conditionals on a
# presigned GET, which is what makes a converted assertion the same assertion.
# What it returns and where it leaves the answer are identical in both modes: the status on stdout,
# the body in $RESP and the headers in $HDR, of whichever hop actually carried the bytes.
media_get() {
  local u=$1; shift
  if [ "$E2E_MEDIA" != redirect ]; then get "$u" "$@"; return; fi
  media_no_cookie_ "$@"
  media_redirect_ get "$u" "$@"
  get "$MEDIA_LOC" "$@"
}
# media_download <url> <host file> [curl args…] — a media GET whose body is wanted ON DISK under a
# name of the caller's own, because ffprobe, `cmp` and a byte count all want a real file. Same two
# modes and the same assertions as media_get; the bytes are written by curl straight to that path
# rather than copied out of $RESP, because several of these are tens of megabytes. The host path is
# W()'d here so no caller has to remember to (§16.3's Git Bash rule).
media_download() {
  local u=$1 f=$2 out; shift 2
  out=$(W "$f")
  if [ "$E2E_MEDIA" != redirect ]; then curl_ -sS -o "$out" -D "$HDRW" -w '%{http_code}' "$@" "$u"; return; fi
  media_no_cookie_ "$@"
  media_redirect_ get "$u" "$@"
  curl_ -sS -o "$out" -D "$HDRW" -w '%{http_code}' "$@" "$MEDIA_LOC"
}
# media_head <url> [curl args…] — "what does this media route say about the object", in either shape.
# In `proxy` it IS `head_`. In `redirect` a bare HEAD against the presigned Location is **403**: SigV4
# signs the METHOD and the link is signed for GET. So the object is asked with a one-byte ranged GET
# (`Range: bytes=0-0`) instead, which every S3 implementation answers 206 carrying the BUCKET's ETag
# for that object, its Content-Type and `Content-Range: bytes 0-0/<total>`. The bucket's ETag is not
# this app's: the app's is a sha256 it computed at ingest and stored in the row (`ingest.go`), the
# bucket's is the bucket's own. Every ETag comparison in the suite is WITHIN one source, and the one
# that was not — 62-tiers' `/original` against `/video.mp4` — says so where it stands.
# $HDR is then that answer's, with ONE
# line appended: `Content-Length: <total>`, read off the Content-Range. `hdr()` takes the LAST match,
# so a caller that reads Content-Length gets the OBJECT's size and not the one byte on the wire, and
# every size comparison in the suite keeps working with no edit at all.
# The status it returns is the honest one — 206, not 200 — and `expect_media` is what accepts it.
media_head() {
  local u=$1 code total; shift
  if [ "$E2E_MEDIA" != redirect ]; then head_ "$u" "$@"; return; fi
  media_no_cookie_ "$@"
  media_redirect_ head_ "$u" "$@"
  code=$(get "$MEDIA_LOC" -H 'Range: bytes=0-0' "$@")
  if [ "$code" = 206 ]; then
    total=$(media_total_ "$(hdr Content-Range)")
    [ -n "$total" ] || die "media $u: the bucket's 206 says 'Content-Range: $(hdr Content-Range)', which carries no total"
    # THE PRODUCTION DELIVERY PATH'S RANGE SEMANTICS, ASSERTED ON EVERY MEDIA HEAD (§20 erratum 283
    # (g)). This one-byte ranged GET is the only request in the suite that asks the real bucket for
    # part of a real object, and until now it was only READ — for the total — never checked. Since
    # clippet.gg moved to R2 on 2026-09-14, `Accept-Ranges`, the shape of a satisfied single range and
    # the byte count behind it have been asserted NOWHERE on the origin that actually serves viewers:
    # step 90 proves each of them against the bundled MinIO on an instance of its own, which is a
    # different store. All three are unconditional in RFC 7233 for an answer that IS a 206, and a
    # store that fails one of them is a store whose clips will not seek in a browser.
    expect "$(hdr Content-Range)" "bytes 0-0/$total" "media $u: the bucket's satisfied single range"
    expect "$(wc -c < "$RESP" | tr -d ' ')" 1 "media $u: the bucket's one-byte 206 carried one byte"
    expect "$(hdr Accept-Ranges)" bytes "media $u: the bucket at ${E2E_MEDIA_ORIGIN#*://} advertises byte ranges on the object itself"
    printf 'Content-Length: %s\r\n' "$total" >> "$HDR"
  fi
  printf '%s' "$code"
}
# media_maybe <url> [curl args…] — for a URL that MAY be a media route, because the caller has a LIST
# and not a route name: step 10 sweeps every URL a card hands out, and a share page, an embed and a
# rendition all arrive in the same file. In `proxy` it is `get`. In `redirect` a 302 to the run's media
# origin is followed — which is exactly what the crawler on the other end of that sweep does — and any
# other answer is handed back untouched.
media_maybe() {
  local u=$1 code; shift
  code=$(get "$u" "$@")
  if [ "$E2E_MEDIA" = redirect ] && [ "$code" = 302 ]; then
    MEDIA_LOC=$(hdr Location)
    [ -n "$MEDIA_LOC" ] || die "media $u: a 302 with no Location"
    expect "$(media_origin_ "$MEDIA_LOC")" "$E2E_MEDIA_ORIGIN" "media $u: the Location's origin"
    code=$(get "$MEDIA_LOC" "$@")
  fi
  printf '%s' "$code"
}
# expect_media ACTUAL WANT WHAT — `expect`, widened by exactly ONE case: in `redirect` mode a media
# HEAD is proven by a 302 followed by a one-byte ranged GET, so the honest status where `proxy`
# answers 200 is the bucket's 206. Nothing else is accepted, in either mode. It is not a "pass if
# either" helper — it is the one status the mode legitimately moves.
expect_media() {
  if [ "$E2E_MEDIA" = redirect ] && [ "$2" = 200 ] && [ "$1" = 206 ]; then return 0; fi
  expect "$1" "$2" "$3"
}
# media_skip <what> — the line an assertion leaves behind when it is about the APP's own behaviour on
# a media route and this mode has moved that behaviour to the bucket. A skipped assertion is never
# silent: a reader of a redirect-mode transcript can see every one the mode stood down, and step 90
# proves each of them on its own instance, in both modes, on every run.
media_skip() { echo "[e2e]   (redirect: $1 is the bucket's; the app's $1 is proven by step 90)"; }
# summary_row <label> <id> <text> — one line in the closing summary table for a clip a DROP-IN made.
# The fixed rows printed after `E2E PASS` are the harness's own clips (A–Y); a drop-in creates and
# destroys clips of its own, and without this they are exercised and then named nowhere in the run's
# record. It writes to a FILE and not to an array (§20 erratum 245): a parallel bundle runs in a
# subshell, so an array it appended to would be discarded with the subshell and its clips would vanish
# from the record. The rows are collected back in BUNDLE order: the parallel bundles first, in name
# order, then the exclusive ones in the order they ran (also name order). That is stable from run to run
# and independent of which lane finished first — it is not the single interleaved name order a
# one-at-a-time run produces, which `E2E_LANES=1` still gives.
summary_row() { printf '  %-5s %-13s %s\n' "$1" "${2:-}" "$3" >> "$SUMMARY_SINK"; }

# ---- the harness cleans up after ITSELF, at both ends of a run (§20 erratum 272) -------------------
# A run that PASSES deletes what it made. A run that was INTERRUPTED — Ctrl-C, a step that died, an
# agent that was killed — leaves its accounts and their clips on the stack, and until now the only
# thing that ever cleared those was a person typing a delete loop by hand. On 2026-09-11 exactly such
# a hand-typed sweep, with an EMPTY owner variable, destroyed every clip on the reference stack
# (the destructive-actions rule). So the harness gets a standing, narrowly-scoped licence to sweep its OWN litter and
# nothing else, and the guards ARE the feature:
#
#   * ONE selector, E2E_OWN_RE, anchored, matching only the names this script mints;
#   * the target list is PRINTED in full before anything is expired — every time, including the
#     "nothing to sweep" case, so a transcript always says what the sweep was looking at;
#   * every id is checked against the id grammars AND against the listing's own count before it is
#     used: output this function does not understand ends the run rather than being guessed at;
#   * no loop body ever runs with an empty variable;
#   * the collections it removes are removed with `--owner` as a second selector, and removing one
#     takes no clip with it (§20 erratum 275);
#   * the account prune that follows can only ever remove an account that owns NOTHING.
#
# Why not a fixed, reusable set of accounts instead: the four parallel lanes inside one run already
# need distinct accounts, and the per-run stamp is what keeps two stacks and two runs apart. A fixed
# set would only move the question to "whose leftovers are the fixed accounts' leftovers".
#
# E2E_OWN_RE is the ONLY selector e2e_sweep_own may ever use. It matches what step 3 registers,
# `e2e_<epoch>`, and that same name with one of the suffixes this suite adds to it (`_b`, `_c`, `_d`,
# `_s1`, `_s2`). Nothing else on any stack can match it: the digits after `e2e_` are required, so
# `demo_account`, `operator`, `visual_<epoch>` and an operator's own `e2e_alice` are all outside it,
# and the suffix is at most three characters, so `e2e_1789149541_long` is outside it too. WIDENING
# THIS IS NOT AN EDIT TO A REGULAR EXPRESSION — it is a change to what the harness may destroy.
E2E_OWN_RE='^e2e_[0-9]{9,11}(_[a-z0-9]{1,3})?$'
# E2E_ID_RE is the shape every id the sweep touches must have: internal/ids' TWO grammars (§7, §20
# errata 248-251) — twelve base62 characters, or three to four capitalised 3-9-letter words. Both,
# because a run mints both and an aborted one can leave either. It is a tripwire and not a filter: an
# id outside it means `admin ls` printed something this function cannot read, and guessing at that is
# how a sweep reaches the wrong row.
E2E_ID_RE='^([A-Za-z0-9]{12}|([A-Z][a-z]{2,8}){3,4})$'
# e2e_own_names_ reads `clippet admin users` on stdin and prints the usernames E2E_OWN_RE matches.
# printUsers writes `<user id>  <username>  created=… clips=N …`, so the name is the second field, and
# the trailing `N user(s)` line has `user(s)` there and matches nothing. It is a function of its own
# so that E2E_SELFTEST=1 can run the filter against a planted listing with no stack at all.
e2e_own_names_() { tr -d '\r' | awk '{ print $2 }' | grep -E "$E2E_OWN_RE" || true; }
# e2e_sweep_fail_ <fatal|best-effort> <why> <message…> — the one exit from trouble inside
# e2e_sweep_own. FATAL at step 1: a stack that cannot be swept is not a stack to test on. A NOTE at
# step 17, which runs after the step-16 log gate and may never fail a run that has already passed.
e2e_sweep_fail_() {
  local mode=$1 why=$2; shift 2
  [ "$mode" != fatal ] || die "sweep ($why): $*"
  echo "[e2e]   NOTE: sweep ($why): $* (not fatal at this end of the run)"
}
# e2e_sweep_own <why> [fatal|best-effort] — expire every clip owned by an account E2E_OWN_RE matches,
# wait for the sweeper to destroy them, then prune the accounts it emptied. Called twice: at the START
# of step 1, where it clears whatever an aborted earlier run left (fatal), and at the END of step 17,
# where it takes this run's own accounts with it (best effort).
e2e_sweep_own() {
  local why=${1:-} mode=${2:-fatal}
  local inv names name out rows want got ids id n=0 m=0 users left still t0
  local cpairs cpair cname cid cn=0
  [ -n "$why" ] || die "e2e_sweep_own: called with no reason"
  case $mode in fatal|best-effort) ;; *) die "e2e_sweep_own: mode is 'fatal' or 'best-effort', not '$mode'";; esac

  if ! inv=$(dc exec -T app clippet admin users 2>&1); then
    e2e_sweep_fail_ "$mode" "$why" "'clippet admin users' failed: $(printf '%s' "$inv" | tail -1)"
    return 0
  fi
  users=$(printf '%s\n' "$inv" | tr -d '\r' | sed -n 's/^\([0-9][0-9]*\) user(s)$/\1/p' | tail -1)
  names=$(printf '%s\n' "$inv" | e2e_own_names_)
  ids=
  for name in $names; do
    [ -n "$name" ] || continue                     # a loop body NEVER runs on an empty selector
    m=$((m + 1))
    if ! out=$(dc exec -T app clippet admin ls --owner "$name" 2>&1); then
      e2e_sweep_fail_ "$mode" "$why" "'clippet admin ls --owner $name' failed: $(printf '%s' "$out" | tail -1)"
      return 0
    fi
    out=$(printf '%s\n' "$out" | tr -d '\r')
    # A clip row is one whose SECOND field is a clip state — the header's is `STATE` and the count
    # line's is `clips`, so neither can be mistaken for one. The count line is then compared with the
    # number of rows this picked up, which catches a listing that changed shape but still parses.
    rows=$(printf '%s\n' "$out" | awk '$2 ~ /^(queued|processing|ready|failed|deleting)$/ { print $1 }')
    want=$(printf '%s\n' "$out" | tail -1 | sed -n 's/^no clips .*/0/p; s/^\([0-9][0-9]*\) clips .*/\1/p')
    if [ -z "$want" ]; then
      e2e_sweep_fail_ "$mode" "$why" "'admin ls --owner $name' did not end in a clip count: $(printf '%s\n' "$out" | tail -1)"
      return 0
    fi
    got=$(printf '%s' "$rows" | grep -c . || true)
    if [ "$got" != "$want" ]; then
      e2e_sweep_fail_ "$mode" "$why" "'admin ls --owner $name' counts $want clip(s) and $got row(s) parsed — the CLI output changed under us"
      return 0
    fi
    for id in $rows; do
      [ -n "$id" ] || continue
      if ! printf '%s' "$id" | grep -Eq "$E2E_ID_RE"; then
        e2e_sweep_fail_ "$mode" "$why" "'admin ls --owner $name' printed '$id' where a clip id belongs — a target is never guessed at"
        return 0
      fi
      ids="$ids $id"; n=$((n + 1))
    done
  done
  ids=${ids# }
  # THE TARGET LIST, PRINTED BEFORE ANYTHING IS TOUCHED (the destructive-actions rule). Every time, including when
  # there is nothing: "the sweep found nothing" and "the sweep did not run" must not read alike.
  if [ "$n" != 0 ]; then
    echo "[e2e]   sweep ($why): $n clip(s) of $m e2e_ account(s): $ids"
    echo "[e2e]   sweep ($why): accounts: $(printf '%s' "$names" | tr '\n' ' ')"
  elif [ "$m" != 0 ]; then
    echo "[e2e]   sweep ($why): nothing to sweep; $m empty e2e_ account(s) to prune: $(printf '%s' "$names" | tr '\n' ' ')"
  else
    echo "[e2e]   sweep ($why): nothing to sweep (${users:-?} user(s) on the stack, none of them the harness's own)"
  fi
  for id in $ids; do
    [ -n "$id" ] || continue
    # --ago 2h, the same backdating steps 15 and 19 use: past expiry for a queued/ready/failed clip
    # and past expiry + SWEEP_GRACE (1h) for one still processing, so the state a leftover happened
    # to be abandoned in changes nothing.
    if ! out=$(dc exec -T app clippet admin expire "$id" --ago 2h 2>&1); then
      e2e_sweep_fail_ "$mode" "$why" "'admin expire $id' failed: $(printf '%s' "$out" | tail -1)"
      return 0
    fi
  done
  if [ -n "$ids" ]; then
    # Wait for the SWEEPER to destroy them — the row going is what says the objects went with it
    # (§12). One `admin ls` per round rather than one per id: the whole listing is a single exec
    # whatever the count, and a clip row always begins at column 0 with its id.
    t0=$SECONDS
    left=$ids
    while [ -n "$left" ]; do
      if ! out=$(dc exec -T app clippet admin ls 2>&1); then
        e2e_sweep_fail_ "$mode" "$why" "'admin ls' failed while waiting for the sweeper: $(printf '%s' "$out" | tail -1)"
        return 0
      fi
      out=$(printf '%s\n' "$out" | tr -d '\r')
      still=
      for id in $left; do
        [ -n "$id" ] || continue
        if printf '%s\n' "$out" | grep -q "^$id "; then still="$still $id"; fi
      done
      left=${still# }
      [ -n "$left" ] || break
      # 90s is six ticks of the 15s SWEEP_INTERVAL step 1 exports before it brings the stack up, so
      # the wait is already measured in ticks at both call sites.
      if [ $((SECONDS - t0)) -ge 90 ]; then
        e2e_sweep_fail_ "$mode" "$why" "still on the stack after 90s (sweep interval ${SWEEP_INTERVAL:-60s}):$left"
        break
      fi
      sleep 2   # was 5 (lane speed2): the deadline is 90 s of time; this only decides how late a finished sweep is seen
    done
    [ -n "$left" ] || echo "[e2e]   sweep ($why): $n clip(s) destroyed after $((SECONDS - t0))s"
  fi
  # THE COLLECTIONS, between the clips and the accounts (§20 erratum 275). `users prune --no-clips`
  # skips an account that still owns a COLLECTION, and until `admin collections rm` nothing outside
  # its owner's SESSION could remove one — so an aborted run's account survived every later sweep and
  # was NOTEd by each of them for ever (erratum 272(c) said exactly that, and this is the verb it was
  # waiting for). `rm` removes the COLLECTION ROW and never a clip, which is why this is safe to do
  # unattended at all.
  #
  # The guard shape is the clips': the target list is PRINTED before anything is removed, every id is
  # checked against E2E_ID_RE and against the listing's own count line, `--owner` is passed to `rm`
  # as a second selector so a row that changed hands between the two calls is refused rather than
  # removed, and output this cannot parse ends the run instead of being guessed at.
  cpairs=
  for name in $names; do
    [ -n "$name" ] || continue
    if ! out=$(dc exec -T app clippet admin collections ls --owner "$name" 2>&1); then
      e2e_sweep_fail_ "$mode" "$why" "'clippet admin collections ls --owner $name' failed: $(printf '%s' "$out" | tail -1)"
      return 0
    fi
    out=$(printf '%s\n' "$out" | tr -d '\r')
    # A collection row is one whose FOURTH field is an RFC3339 stamp: the header's is `CREATED` and
    # the count line has no fourth field at all, so neither can be mistaken for one. The title is
    # last and is printed quoted, so nothing a person typed can add a field to the left of it.
    rows=$(printf '%s\n' "$out" | awk '$4 ~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T/ { print $1 }')
    want=$(printf '%s\n' "$out" | tail -1 | sed -n 's/^no collections.*/0/p; s/^\([0-9][0-9]*\) collections.*/\1/p')
    if [ -z "$want" ]; then
      e2e_sweep_fail_ "$mode" "$why" "'admin collections ls --owner $name' did not end in a count: $(printf '%s\n' "$out" | tail -1)"
      return 0
    fi
    got=$(printf '%s' "$rows" | grep -c . || true)
    if [ "$got" != "$want" ]; then
      e2e_sweep_fail_ "$mode" "$why" "'admin collections ls --owner $name' counts $want collection(s) and $got row(s) parsed — the CLI output changed under us"
      return 0
    fi
    for id in $rows; do
      [ -n "$id" ] || continue
      if ! printf '%s' "$id" | grep -Eq "$E2E_ID_RE"; then
        e2e_sweep_fail_ "$mode" "$why" "'admin collections ls --owner $name' printed '$id' where a collection id belongs — a target is never guessed at"
        return 0
      fi
      cpairs="$cpairs $name:$id"; cn=$((cn + 1))
    done
  done
  cpairs=${cpairs# }
  # Printed in the empty case too, for the reason the clip list is: "found none" and "did not look"
  # must not read alike in a transcript.
  if [ "$cn" = 0 ] && [ "$m" != 0 ]; then
    echo "[e2e]   sweep ($why): no collection is owned by the $m e2e_ account(s)"
  fi
  if [ "$cn" != 0 ]; then
    echo "[e2e]   sweep ($why): $cn collection(s) of the same account(s), owner:id — $cpairs"
    for cpair in $cpairs; do
      cname=${cpair%%:*}; cid=${cpair#*:}
      [ -n "$cname" ] && [ -n "$cid" ] || continue      # a loop body NEVER runs on an empty selector
      if ! out=$(dc exec -T app clippet admin collections rm "$cid" --owner "$cname" 2>&1); then
        e2e_sweep_fail_ "$mode" "$why" "'admin collections rm $cid --owner $cname' failed: $(printf '%s' "$out" | tail -1)"
        return 0
      fi
      printf '%s\n' "$out" | tr -d '\r' | tail -1 | sed -n "s/^/[e2e]   sweep ($why): /p"
    done
  fi
  # THE ACCOUNTS. `e2e_%` is a SQL LIKE pattern, in which `_` matches any ONE character, so on its own
  # it is wider than E2E_OWN_RE. It is acceptable here for two reasons and no others: it is only ever
  # reached after the loop above emptied every account the REGEX matched, and `--no-clips` means the
  # command skips any match that still owns a clip or a collection whatever the pattern says (§20
  # erratum 86, DEPLOY §8) — so the widest thing it can do is remove an EMPTY account whose name
  # begins with `e2e` and one more character. `--older-than 1s` rather than the `1h` this line used to
  # carry: at step 1 the run's own accounts do not exist yet, and at step 17 they are meant to go too.
  # "A run cannot tidy up after itself" was the old shape of this line, and is exactly what left the
  # litter this function exists to stop leaving.
  if ! out=$(dc exec -T app clippet admin users prune 'e2e_%' --older-than 1s --no-clips 2>&1); then
    e2e_sweep_fail_ "$mode" "$why" "'admin users prune' failed: $(printf '%s' "$out" | tail -1)"
    return 0
  fi
  printf '%s\n' "$out" | tr -d '\r' | tail -1 | sed -n "s/^/[e2e]   sweep ($why): /p"
  # …and say whether it worked. Until §20 erratum 275 this was where the one leftover it could not
  # clear was named: `users prune` skips an account that still owns a COLLECTION, and nothing could
  # remove one outside its owner's session. The step above removes them now, so an account that
  # still survives here means something this function did NOT expect — a clip made after its
  # listing, a collection created between the two calls — and it is still named rather than guessed
  # at. It owns no clips and no objects by this point.
  if inv=$(dc exec -T app clippet admin users 2>&1); then
    names=$(printf '%s\n' "$inv" | e2e_own_names_)
    if [ -n "$names" ]; then
      echo "[e2e]   NOTE: sweep ($why): $(printf '%s' "$names" | tr '\n' ' ')survived the prune — it must have acquired a clip or a collection while the sweep ran (`clippet admin ls --owner` and `admin collections ls --owner` say which)"
    else
      echo "[e2e]   sweep ($why): no e2e_ account is left on the stack"
    fi
  fi
}

# ---- E2E_SELFTEST=1: the three retry wrappers and the sweep's guards, against stubs, with no stack -
# `bash scripts/e2e.sh` with E2E_SELFTEST=1 runs THIS and exits. scripts/check.sh is what runs it, so
# the branches are proven by the CI story rather than by the failure they exist for. It is placed here
# because every helper it needs is defined above and nothing below it has touched docker yet.
# It ends in `E2E SELFTEST PASS` and NEVER in `E2E PASS`: it is not a run of the suite, and a caller
# that greps for the suite's own pass line must not be able to mistake one for the other.
if [ "${E2E_SELFTEST:-0}" = 1 ]; then
  SELF_FAIL=0
  SELF_CT="$E2E_DIR/selftest-count"; SELF_OUT="$E2E_DIR/selftest.out"; SELF_ERR="$E2E_DIR/selftest.err"
  self_ok() { if [ "$1" = "$2" ]; then echo "[selftest] ok   $3"; else echo "[selftest] FAIL $3: got '$1', want '$2'"; SELF_FAIL=1; fi; }
  # The stub counts its calls in a FILE and not a variable: curl_ runs its command inside a command
  # substitution, so a variable the stub set would be lost with that subshell.
  self_stub() { local n; n=$(cat "$SELF_CT" 2>/dev/null || echo 0); n=$((n + 1)); printf '%s' "$n" > "$SELF_CT"
    # The failing attempt prints SELF_FAILOUT first: curl writes its %{http_code} (000) even when it fails,
    # so an implementation that APPENDED the two attempts would hand the caller "000answer" — the exact
    # bug curl_ exists to avoid, and one the stub must be able to show.
    # SELF_OKOUT is what the SECOND attempt prints; it defaults to the word the retry checks below
    # look for, and the http_refused cases set it to a status code instead.
    if [ "$n" = 1 ]; then printf '%s' "${SELF_FAILOUT:-}"; return "$SELF_CODE"; fi; printf '%s' "${SELF_OKOUT:-answer}"; }
  self_calls() { cat "$SELF_CT" 2>/dev/null || echo 0; }
  self_notes() { grep -c 'NOTE:' "$SELF_ERR" 2>/dev/null || true; }
  self_reset() { printf '0' > "$SELF_CT"; : > "$SELF_OUT"; : > "$SELF_ERR"; }
  docker_() { self_stub; }
  curl__() { self_stub; }

  # ---- the retry PAUSE (§20 erratum 283 (d)) -------------------------------------------------------
  # Asserted here and nowhere else, because nowhere else could: what the branches below prove is that a
  # wrapper retries ONCE and that the things that must not be retried are not, and a wrapper that waited
  # zero seconds would satisfy every one of them. So the default is read BEFORE it is overridden — this
  # is the one line in the suite that says three seconds is three seconds — and then set to zero for the
  # rest of this block. Seven of the branches below take the pause, and against a stub that never
  # touches a network they were ~20s of `scripts/check.sh` spent asleep, in the stage whose own
  # description had to warn about it. Nothing about the retries themselves changes.
  self_ok "$E2E_RETRY_PAUSE" 3 "the retry wrappers pause 3s before their one retry (the DEFAULT, read before this block sets it to 0)"
  E2E_RETRY_PAUSE=0

  SELF_CODE=125; SELF_FAILOUT=; self_reset; SELF_RC=0
  dc up -d > "$SELF_OUT" 2> "$SELF_ERR" || SELF_RC=$?
  self_ok "$SELF_RC" 0 "dc survives one exit 125"
  self_ok "$(self_calls)" 2 "dc ran the command twice"
  self_ok "$(self_notes)" 1 "dc printed exactly one NOTE"
  self_ok "$(cat "$SELF_OUT")" answer "dc returned the second attempt's output"

  SELF_CODE=1; SELF_FAILOUT=; self_reset; SELF_RC=0
  dc up -d > "$SELF_OUT" 2> "$SELF_ERR" || SELF_RC=$?
  self_ok "$SELF_RC" 1 "dc passes a NON-125 failure straight through"
  self_ok "$(self_calls)" 1 "…having run the command once (a blanket retry would double every side effect)"
  self_ok "$(self_notes)" 0 "…and said nothing about retrying"

  SELF_FAILOUT=000
  for SELF_CODE in 7 28 35; do
    self_reset; SELF_RC=0
    SELF_BODY=$(curl_ -sS 2> "$SELF_ERR") || SELF_RC=$?
    self_ok "$SELF_RC" 0 "curl_ survives one exit $SELF_CODE"
    self_ok "$(self_calls)" 2 "curl_ ran curl twice on exit $SELF_CODE"
    self_ok "$(self_notes)" 1 "curl_ printed exactly one NOTE on exit $SELF_CODE"
    self_ok "$SELF_BODY" answer "curl_ returned the second attempt's body, not both (exit $SELF_CODE)"
  done

  SELF_CODE=22; SELF_FAILOUT=000; self_reset; SELF_RC=0
  SELF_BODY=$(curl_ -sS 2> "$SELF_ERR") || SELF_RC=$?
  self_ok "$SELF_RC" 22 "curl_ passes an exit 22 (an HTTP error) straight through"
  self_ok "$(self_calls)" 1 "…having run curl once"
  self_ok "$(self_notes)" 0 "…and said nothing about retrying"
  self_ok "$SELF_BODY" 000 "…and handed the caller curl's own output (the 000 code) untouched"

  # ---- http_refused: the bounded retry for a write the origin REFUSES (§20 erratum 275) ------------
  # The rule is asymmetric on purpose and both halves are pinned here: through an EDGE a refusal that
  # never arrived is retried once, and on LOOPBACK nothing is ever retried — there is no edge to
  # absorb, so a 502 there is a real answer and re-sending would be inventing one. The stub answers
  # 502 first and 401 second, which is the exact shape of the failure this exists for.
  SELF_REFUSE_URL="the-origin/api/share"
  SELF_CODE=0; SELF_FAILOUT=502; SELF_OKOUT=401
  E2E_NONLOOPBACK=1; self_reset; SELF_RC=0
  SELF_BODY=$(http_refused POST "$SELF_REFUSE_URL" 2> "$SELF_ERR") || SELF_RC=$?
  self_ok "$SELF_RC" 0 "http_refused survives an edge 502"
  self_ok "$SELF_BODY" 401 "…and hands the caller the REFUSAL the origin gave on the retry"
  self_ok "$(self_calls)" 2 "…having sent the request twice"
  self_ok "$(grep -c 'retry (edge)' "$SELF_ERR" 2>/dev/null || true)" 1 "…and said 'retry (edge)' exactly once"

  E2E_NONLOOPBACK=0; self_reset; SELF_RC=0
  SELF_BODY=$(http_refused POST "$SELF_REFUSE_URL" 2> "$SELF_ERR") || SELF_RC=$?
  self_ok "$SELF_BODY" 502 "on LOOPBACK a 502 is an answer, not a transient: http_refused returns it"
  self_ok "$(self_calls)" 1 "…having sent the request once"
  self_ok "$(self_notes)" 0 "…and said nothing about retrying"

  # A refusal that ARRIVED is never sent twice, whatever the origin: that is the common case, and a
  # second request would be pointless traffic against a rate limiter the suite also tests.
  E2E_NONLOOPBACK=1; SELF_FAILOUT=403; SELF_OKOUT=401; self_reset; SELF_RC=0
  SELF_BODY=$(http_refused POST "$SELF_REFUSE_URL" 2> "$SELF_ERR") || SELF_RC=$?
  self_ok "$SELF_BODY" 403 "a 403 through an edge is the origin's own refusal and is kept"
  self_ok "$(self_calls)" 1 "…having sent the request once"

  # …and curl's own "no answer" exits are retried through an edge, exactly as the READ wrapper's are.
  SELF_CODE=7; SELF_FAILOUT=000; SELF_OKOUT=401; self_reset; SELF_RC=0
  SELF_BODY=$(http_refused POST "$SELF_REFUSE_URL" 2> "$SELF_ERR") || SELF_RC=$?
  self_ok "$SELF_RC" 0 "http_refused survives a curl exit 7 through an edge"
  self_ok "$SELF_BODY" 401 "…and returns the second attempt's status alone (not '000401')"
  self_ok "$(self_calls)" 2 "…having sent the request twice"
  SELF_OKOUT=; E2E_NONLOOPBACK=0

  # ---- setup_once_: the serial setup's one retry (run 10, lane nits10; run 9 [pool]) ------
  # The stub fails its first call with SELF_CODE and curl's own stderr line SELF_CERR, then answers 201.
  # A connect-phase failure on loopback is retried once with its row in retries.txt; a 28 after the
  # connection opened, and anything through an edge, is never sent twice.
  self_setup_stub() { local n; n=$(cat "$SELF_CT" 2>/dev/null || echo 0); n=$((n + 1)); printf '%s' "$n" > "$SELF_CT"
    if [ "$n" = 1 ]; then printf '000'; [ -z "$SELF_CERR" ] || echo "$SELF_CERR" >&2; return "$SELF_CODE"; fi; printf '201'; }
  SELF_RT_SAVE=$(cat "$E2E_DIR/retries.txt" 2>/dev/null || true); : > "$E2E_DIR/retries.txt"
  SELF_STEP_SAVE=${STEP:-}; STEP=3
  E2E_NONLOOPBACK=0; SELF_CODE=28; SELF_CERR='curl: (28) Failed to connect to localhost port 8580 after 21046 ms: Timeout was reached'
  self_reset; SELF_RC=0
  SELF_BODY=$(setup_once_ register self_setup_stub 2> "$SELF_ERR") || SELF_RC=$?
  self_ok "$SELF_RC:$SELF_BODY:$(self_calls)" "0:201:2" "setup_once_: a loopback 'Failed to connect' 28 is sent again ONCE and the caller gets the second answer alone"
  self_ok "$(awk -F'\t' '$1 == "setup-step3" && $2 == "curl-28" && $3 ~ /^[0-9]+$/ && $4 ~ /Z$/ { n++ } END { print n + 0 }' "$E2E_DIR/retries.txt" 2>/dev/null)" 1 \
    "…and retries.txt has the pool's row for it (setup-step3, curl-28, seconds, utc)"
  self_ok "$(self_notes)" 1 "…and said so in one NOTE"
  SELF_CODE=7; SELF_CERR='curl: (7) Failed to connect to localhost port 8580 after 0 ms: Connection refused'; self_reset; SELF_RC=0
  SELF_BODY=$(setup_once_ register self_setup_stub 2> "$SELF_ERR") || SELF_RC=$?
  self_ok "$SELF_RC:$SELF_BODY:$(self_calls)" "0:201:2" "setup_once_: a loopback exit 7 (could not connect) is sent again once"
  SELF_CODE=28; SELF_CERR='curl: (28) Operation timed out after 30001 milliseconds with 0 bytes received'; self_reset; SELF_RC=0
  SELF_BODY=$(setup_once_ register self_setup_stub 2> "$SELF_ERR") || SELF_RC=$?
  self_ok "$SELF_RC:$(self_calls):$(self_notes)" "28:1:0" "setup_once_: a 28 AFTER the connection opened (it may have landed) is never sent twice"
  E2E_NONLOOPBACK=1; SELF_CERR='curl: (28) Failed to connect to clippet.gg port 443 after 21046 ms: Timeout was reached'; self_reset; SELF_RC=0
  SELF_BODY=$(setup_once_ register self_setup_stub 2> "$SELF_ERR") || SELF_RC=$?
  self_ok "$SELF_RC:$(self_calls)" "28:1" "setup_once_: through an edge (not the stack's own loopback) nothing is retried"
  E2E_NONLOOPBACK=0; E2E_RETRY=0; SELF_CERR='curl: (28) Failed to connect to localhost port 8580 after 21046 ms: Timeout was reached'; self_reset; SELF_RC=0
  SELF_BODY=$(setup_once_ register self_setup_stub 2> "$SELF_ERR") || SELF_RC=$?
  self_ok "$SELF_RC:$(self_calls)" "28:1" "setup_once_: E2E_RETRY=0 turns it off with the pool's"
  E2E_RETRY=1; SELF_CODE=22; SELF_CERR=; self_reset; SELF_RC=0
  SELF_BODY=$(setup_once_ register self_setup_stub 2> "$SELF_ERR") || SELF_RC=$?
  self_ok "$SELF_RC:$(self_calls)" "22:1" "setup_once_: an answered error (exit 22) is passed straight through"
  self_ok "$(grep -c . "$E2E_DIR/retries.txt" 2>/dev/null || echo 0)" 2 "…and retries.txt holds the two retried requests' rows (the 28 and the 7), none for the four that were not"
  rm -f "$E2E_DIR/retries.txt"; [ -z "$SELF_RT_SAVE" ] || printf '%s\n' "$SELF_RT_SAVE" > "$E2E_DIR/retries.txt"
  STEP=$SELF_STEP_SAVE

  # ---- X-Clippet-App: 1 on every wrapper (DEPLOY §37) ----------------------------------------------
  # clippet.gg's Worker serves the homepage to an anonymous GET/HEAD of `/` unless the request carries
  # this header, so a wrapper that dropped it would photograph — and assert on — the homepage instead of
  # the product's front door, and only on the gate's origin. The stub records each argument on a line of
  # its own; the header must be there exactly once per attempt, on the retry too.
  SELF_ARGS="$E2E_DIR/selftest-args"
  curl__() { printf '%s\n' "$@" >> "$SELF_ARGS"; self_stub; }
  self_app_hdr() { grep -cx 'X-Clippet-App: 1' "$SELF_ARGS" 2>/dev/null || true; }
  SELF_CODE=0; SELF_FAILOUT=200; SELF_OKOUT=200
  for SELF_WRAP in get page head_; do
    self_reset; : > "$SELF_ARGS"
    "$SELF_WRAP" "the-origin/" > /dev/null 2> "$SELF_ERR" || true
    self_ok "$(self_app_hdr)" 1 "$SELF_WRAP sends X-Clippet-App: 1 (once)"
  done
  for SELF_WRAP in http http_refused; do
    self_reset; : > "$SELF_ARGS"
    "$SELF_WRAP" GET "the-origin/" > /dev/null 2> "$SELF_ERR" || true
    self_ok "$(self_app_hdr)" 1 "$SELF_WRAP sends X-Clippet-App: 1 (once)"
  done
  SELF_CODE=7; SELF_FAILOUT=000; self_reset; : > "$SELF_ARGS"
  page "the-origin/" > /dev/null 2> "$SELF_ERR" || true
  self_ok "$(self_calls)" 2 "a page whose first attempt got no answer is sent twice…"
  self_ok "$(self_app_hdr)" 2 "…and the RETRY carries X-Clippet-App: 1 as well"
  SELF_OKOUT=; SELF_FAILOUT=; rm -f "$SELF_ARGS"
  curl__() { self_stub; }

  # ---- the sweep's two guards (§20 erratum 272) ----------------------------------------------------
  # E2E_OWN_RE decides what the harness is allowed to destroy and E2E_ID_RE decides what it will
  # accept as a target, so both are pinned HERE — against planted text, with no stack — rather than
  # only ever exercised on a run where a wrong answer is somebody's wiped stack.
  SELF_USERS="$E2E_DIR/selftest-users"
  printf '%s\n' \
    'usr_01  operator             created=2026-09-11T00:00:00Z clips=3 collections=0 tokens=0' \
    'usr_02  demo_account         created=2026-09-11T00:00:00Z clips=9 collections=1 tokens=0' \
    'usr_03  visual_1789149541    created=2026-09-11T00:00:00Z clips=0 collections=0 tokens=0' \
    'usr_04  e2e_alice            created=2026-09-11T00:00:00Z clips=0 collections=0 tokens=0' \
    'usr_05  e2e_                 created=2026-09-11T00:00:00Z clips=0 collections=0 tokens=0' \
    'usr_06  e2e_1789149541_long  created=2026-09-11T00:00:00Z clips=1 collections=0 tokens=0' \
    'usr_07  e2e_17891495411      created=2026-09-11T00:00:00Z clips=1 collections=0 tokens=0' \
    'usr_08  e2e_1789149541_s1    created=2026-09-11T00:00:00Z clips=1 collections=0 tokens=0' \
    'usr_09  e2e_1789149541_zz    created=2026-09-11T00:00:00Z clips=1 collections=0 tokens=0' \
    'usr_10  e2e_1789149541       created=2026-09-11T00:00:00Z clips=2 collections=0 tokens=0' \
    '10 user(s)' > "$SELF_USERS"
  self_ok "$(e2e_own_names_ < "$SELF_USERS" | tr '\n' ' ')" \
    'e2e_17891495411 e2e_1789149541_s1 e2e_1789149541_zz e2e_1789149541 ' \
    "the sweep's name filter keeps the harness's own accounts (11-digit stamp and 1-3 char suffixes included)"
  self_ok "$(e2e_own_names_ < "$SELF_USERS" | grep -c -e operator -e demo_account -e visual_ -e e2e_alice -e e2e_1789149541_long || true)" 0 \
    "…and NEVER operator, demo_account, visual_*, an operator's e2e_alice, a 4-char suffix or a bare e2e_"
  printf '%s\n' 'usr_01  operator  created=2026-09-11T00:00:00Z clips=3 collections=0 tokens=0' '1 user(s)' > "$SELF_USERS"
  self_ok "$(e2e_own_names_ < "$SELF_USERS")" "" "…and matches nothing at all on a stack the harness has never run against"
  rm -f "$SELF_USERS"
  self_id() { if printf '%s' "$1" | grep -Eq "$E2E_ID_RE"; then printf accepted; else printf refused; fi; }
  self_ok "$(self_id aB3dE5gH7jK9)"                    accepted "id shape: twelve base62 characters (the compact grammar)"
  self_ok "$(self_id CorrectHorseBattery)"             accepted "id shape: three capitalised words (a listed memorable id)"
  self_ok "$(self_id CorrectHorseBatteryStaple)"       accepted "id shape: four capitalised words (an unlisted memorable id)"
  self_ok "$(self_id aB3dE5gH7jK)"                     refused  "id shape: eleven base62 characters is not an id"
  self_ok "$(self_id correcthorsebattery)"             refused  "id shape: uncapitalised words are not an id"
  # `BigHorse` and not `CorrectHorse`: the latter is two words AND exactly twelve base62 characters, so
  # internal/ids accepts it as a COMPACT id and so must this — the overlap New refuses to mint into.
  self_ok "$(self_id BigHorse)"                        refused  "id shape: two words that are not also twelve characters are not an id"
  self_ok "$(self_id CorrectHorseBatteryStapleExtra)"  refused  "id shape: five words are not an id"
  self_ok "$(self_id '2 clips (owner=e2e_1789149541)')" refused "id shape: a count line is not an id"
  self_ok "$(self_id '../../etc')"                     refused  "id shape: a path is not an id"

  # ---- the collections listing the sweep reads (§20 erratum 275) -----------------------------------
  # The row rule is "the FOURTH field is an RFC3339 stamp", which has to pick out the rows and
  # NOTHING else from `admin collections ls --owner`: not the header, not the count line, and not a
  # title — which is a person's own text, is printed last, and is printed QUOTED so it cannot add a
  # field to the left of itself. Pinned here against planted output for the reason the two patterns
  # above are: a wrong answer is a target this function should never have touched.
  SELF_COLS="$E2E_DIR/selftest-collections"
  printf '%s\n' \
    'ID           OWNER                CLIPS  CREATED              TITLE' \
    'aB3dE5gH7jK9 e2e_1789149541       2      2026-09-13T06:00:00Z "Best of last night"' \
    'zZ9yY8xX7wW6 e2e_1789149541_s1    0      2026-09-13T06:01:00Z "2026-01-02T00:00:00Z is a legal title"' \
    '2 collections (owner=e2e_1789149541)' > "$SELF_COLS"
  self_ok "$(awk '$4 ~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T/ { print $1 }' "$SELF_COLS" | tr '\n' ' ')" \
    'aB3dE5gH7jK9 zZ9yY8xX7wW6 ' \
    "the collections listing: the two rows are read and the header, the count line and a date-shaped TITLE are not"
  self_ok "$(tail -1 "$SELF_COLS" | sed -n 's/^no collections.*/0/p; s/^\([0-9][0-9]*\) collections.*/\1/p')" 2 \
    "…and the count line the row count is checked against is read"
  printf '%s\n' 'no collections (owner=e2e_1789149541)' > "$SELF_COLS"
  self_ok "$(tail -1 "$SELF_COLS" | sed -n 's/^no collections.*/0/p; s/^\([0-9][0-9]*\) collections.*/\1/p')" 0 \
    "…and an account that owns none reads as zero, not as unparsable"
  rm -f "$SELF_COLS"

  # ---- the four media helpers, against planted strings and no stack (§20 erratum 283 (h)) ----------
  # Two of these are SECURITY functions and neither had ever been exercised outside a live redirect
  # run: `media_redact_` is what keeps `X-Amz-Signature` and `X-Amz-Credential` out of a transcript
  # that lives on disk, and `media_no_cookie_` is what keeps the session cookie from being sent to a
  # third-party bucket. A green run proves nothing about either — a redaction that did nothing would
  # look identical on a run where no message was printed, and the cookie guard "can only ever fire on
  # a harness mistake", which is to say never, which is to say it was dead code nobody could see rot.
  # `media_origin_` decides the ONE origin every signed Location is compared against, and
  # `media_total_` is what every size comparison in redirect mode rests on. All four are pure string
  # or argument functions, so they are pinned here, with no daemon and no stack.
  SELF_SIGNED='https://clippet-media.abc123.r2.cloudflarestorage.com/clips/AbcDefGhiJkl/renditions/720p.mp4?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAPLANTED%2F20260914%2Fauto%2Fs3%2Faws4_request&X-Amz-Signature=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'
  self_ok "$(media_origin_ "$SELF_SIGNED")" 'https://clippet-media.abc123.r2.cloudflarestorage.com' \
    "media_origin_ takes the origin off a signed URL and leaves the path and the query behind"
  self_ok "$(media_origin_ 'http://127.0.0.1:9510/clips/x.mp4?a=b/c')" 'http://127.0.0.1:9510' \
    "…keeps the PORT, and a slash inside the query does not extend it"
  SELF_RED=$(media_redact_ "$SELF_SIGNED")
  self_ok "$SELF_RED" 'https://clippet-media.abc123.r2.cloudflarestorage.com/clips/AbcDefGhiJkl/renditions/720p.mp4?<signed>' \
    "media_redact_ keeps the whole path a post-mortem needs and replaces the query with a marker"
  self_ok "$(printf '%s' "$SELF_RED" | grep -c -e X-Amz-Signature -e X-Amz-Credential -e AKIAPLANTED || true)" 0 \
    "…and neither the signature, the access key id nor the algorithm survives it"
  self_ok "$(media_redact_ 'https://bucket.example/clips/x.mp4')" 'https://bucket.example/clips/x.mp4?<signed>' \
    "…and a URL with no query at all is still redacted rather than passed through unrecognised"
  # media_no_cookie_ DIES, so each call is made in a subshell and only its status is read.
  SELF_RC=0; ( media_no_cookie_ -sS -H 'Range: bytes=0-0' ) >/dev/null 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC" 0 "media_no_cookie_ passes ordinary curl arguments"
  SELF_RC=0; ( media_no_cookie_ -sS -b /tmp/jar ) >/dev/null 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC" 1 "…and DIES on -b: in redirect mode the second hop is a third party's bucket"
  SELF_RC=0; ( media_no_cookie_ --cookie /tmp/jar -sS ) >/dev/null 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC" 1 "…and on the long spelling --cookie, wherever it sits in the argument list"
  self_ok "$(media_total_ 'bytes 0-0/12345')" 12345 "media_total_ reads the object's size out of a satisfied single range"
  self_ok "$(media_total_ 'bytes */12345')"  12345 "…and out of a 416's unsatisfied-range form"
  self_ok "$(media_total_ 'bytes 0-0/*')"    ""    "…and refuses an unknown total rather than passing '*' on as a size"
  self_ok "$(media_total_ '')"               ""    "…and an absent header is not a size either"

  # ---- the scripts' destructive edges (§20 erratum 290) ---------------------------------------------
  # Eight guards, each pinned against a stub or a scratch directory and no stack: every one of them
  # decides whether something is REMOVED — a shared stack's overrides, a container, a directory, a
  # bucket prefix, a compose project — and every one was, until this erratum, exercised only where a
  # wrong answer is somebody's stack.
  #
  # A2 — the hand-back. No trap here (the self-test never reaches step 1); armed in a subshell it fires
  # exactly once on that subshell's exit, keeps the exit status it runs under, runs once when step 17
  # calls it first, and is invisible to a nested subshell (bash resets caught traps there).
  self_ok "$(trap -p EXIT)" "" "E2E_SELFTEST=1 arms no EXIT trap: the hand-back belongs to step 1"
  SELF_CODE=0; SELF_FAILOUT=; self_reset; SELF_RC=0
  ( e2e_handback_arm_; exit 4 ) > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC" 4 "an armed run that dies keeps its exit status through the hand-back"
  self_ok "$(self_calls)" 1 "…and the hand-back recreated the app exactly once on the way out"
  self_ok "$(grep -c 'handed back (on exit at step' "$SELF_OUT" || true)" 1 "…saying so, with the step it died at"
  self_reset
  ( e2e_handback_arm_; e2e_handback_ "end of run"; exit 0 ) > "$SELF_OUT" 2>&1 || true
  self_ok "$(self_calls)" 1 "a run that reaches step 17 hands back there, and the EXIT trap then does nothing"
  self_ok "$(grep -c 'handed back (end of run)' "$SELF_OUT" || true)" 1 "…and the transcript names step 17 as the one that did it"
  self_reset
  ( e2e_handback_arm_; ( exit 1 ) || true; ( : ); printf '%s' "$(self_calls)" > "$SELF_OUT"; exit 0 ) >/dev/null 2>&1 || true
  self_ok "$(cat "$SELF_OUT")" 0 "a bundle's subshell exiting inside an armed run does NOT fire the hand-back (no bundle inherits the trap)"
  self_ok "$(self_calls)" 1 "…which the run's own exit then does, once"

  # #11 — the stale-probe sweep: this project's label AND the harness's own name prefix, or nothing.
  SELF_ARGS="$E2E_DIR/selftest-docker-args"; : > "$SELF_ARGS"
  docker_() { printf '%s\n' "$*" >> "$SELF_ARGS"; if [ "${1:-}" = ps ]; then printf 'clippet-e2e-scope-8493\nclippet-selftest-app-1\nclippet-e2e-owner-8494\n'; fi; }
  e2e_sweep_probes_ clippet-selftest > "$SELF_OUT" 2>&1 || true
  self_ok "$(grep -c '^ps -a --filter label=com.docker.compose.project=clippet-selftest ' "$SELF_ARGS" || true)" 1 \
    "the probe sweep lists by THIS compose project's label (a lane's run can never take another stack's probe)"
  self_ok "$(grep -c '^stop -t 20 clippet-e2e-scope-8493 clippet-e2e-owner-8494$' "$SELF_ARGS" || true)" 1 \
    "…stops only the clippet-e2e-* names in that listing (the app container beside them is not touched), -t 20"
  self_ok "$(grep -c '^rm clippet-e2e-scope-8493 clippet-e2e-owner-8494$' "$SELF_ARGS" || true)" 1 "…and removes exactly those"
  self_ok "$(grep -c 'stale probe containers of project clippet-selftest' "$SELF_OUT" || true)" 1 "…after printing them"
  : > "$SELF_ARGS"
  e2e_sweep_probes_ "" > "$SELF_OUT" 2>&1 || true
  self_ok "$(wc -l < "$SELF_ARGS" | tr -d ' ')" 0 "with no project name the sweep calls docker NOT AT ALL (a label-less filter would be every stack's probes)"
  self_ok "$(grep -c 'NOTE: the compose project name could not be read' "$SELF_OUT" || true)" 1 "…and says why"
  docker_() { self_stub; }

  # #3 — E2E_CLEAN's keep-list, on a scratch directory: the run's files go, the operator's stay.
  SELF_CLEAN="$E2E_DIR/selftest-clean"; rm -rf "$SELF_CLEAN"
  mkdir -p "$SELF_CLEAN/backups" "$SELF_CLEAN/checkpoints" "$SELF_CLEAN/logs" "$SELF_CLEAN/ctx-main" "$SELF_CLEAN/gatestack"
  : > "$SELF_CLEAN/gatestack/clippet-gate.env"
  : > "$SELF_CLEAN/backups/x"; : > "$SELF_CLEAN/checkpoints/y"; : > "$SELF_CLEAN/logs/z"; : > "$SELF_CLEAN/ctx-main/resp.json"
  : > "$SELF_CLEAN/demo-user.txt"; : > "$SELF_CLEAN/webhook.txt"; : > "$SELF_CLEAN/app.log"
  e2e_clean_ "$SELF_CLEAN" > "$SELF_OUT" 2>&1 || true
  self_ok "$([ -e "$SELF_CLEAN/backups/x" ] && [ -e "$SELF_CLEAN/checkpoints/y" ] && [ -e "$SELF_CLEAN/demo-user.txt" ] && [ -e "$SELF_CLEAN/webhook.txt" ] && [ -e "$SELF_CLEAN/gatestack/clippet-gate.env" ] && echo kept)" kept \
    "E2E_CLEAN keeps backups/, checkpoints/, gatestack/ (the gate stack's .env), demo-user.txt and webhook.txt"
  self_ok "$([ ! -e "$SELF_CLEAN/logs" ] && [ ! -e "$SELF_CLEAN/ctx-main" ] && [ ! -e "$SELF_CLEAN/app.log" ] && echo gone)" gone \
    "…and removes the run's own directories and files"
  self_ok "$(grep -c '^\[e2e\] E2E_CLEAN=1: removing from .*: app.log ctx-main logs$' "$SELF_OUT" || true)" 1 "…printing the removal list before it acts"
  rm -rf "$SELF_CLEAN"

  # #7 — the resume key folds `.env` and the COMPOSE_FILE members: a changed MEDIA_DELIVERY line is a
  # different key, a second read of the same file is the same key. (`-dirty` may be appended in a working
  # tree with product changes; the comparison strips it.)
  SELF_ENV_A="$E2E_DIR/selftest-env-a"; SELF_ENV_B="$E2E_DIR/selftest-env-b"; SELF_ENV_C="$E2E_DIR/selftest-env-c"
  printf 'PUBLIC_BASE_URL=http://localhost:8481\nMEDIA_DELIVERY=proxy\n' > "$SELF_ENV_A"
  printf 'PUBLIC_BASE_URL=http://localhost:8481\nMEDIA_DELIVERY=redirect\n' > "$SELF_ENV_B"
  printf 'PUBLIC_BASE_URL=http://localhost:8481\nMEDIA_DELIVERY=proxy\nCOMPOSE_FILE=compose.yaml:compose.fs.yaml\n' > "$SELF_ENV_C"
  SELF_KA=$(COMPOSE_FILE= e2e_product_key_ "$SELF_ENV_A"); SELF_KA=${SELF_KA%-dirty}
  SELF_KB=$(COMPOSE_FILE= e2e_product_key_ "$SELF_ENV_B"); SELF_KB=${SELF_KB%-dirty}
  SELF_KC=$(COMPOSE_FILE= e2e_product_key_ "$SELF_ENV_C"); SELF_KC=${SELF_KC%-dirty}
  self_ok "$(printf '%s' "$SELF_KA" | grep -Ec '^[0-9a-f]{12}$' || true)" 1 "the resume key is twelve hex characters (git is present here)"
  self_ok "$([ "$SELF_KA" = "$(COMPOSE_FILE= e2e_product_key_ "$SELF_ENV_A" | sed 's/-dirty$//')" ] && echo same)" same "…and is stable across two reads of the same .env"
  self_ok "$([ "$SELF_KA" != "$SELF_KB" ] && echo differs)" differs "…and a changed MEDIA_DELIVERY line in .env changes it (a bundle's PASS in proxy mode is not a PASS in redirect mode)"
  self_ok "$([ "$SELF_KA" != "$SELF_KC" ] && echo differs)" differs "…and so does a COMPOSE_FILE overlay, whose contents are folded in too"
  rm -f "$SELF_ENV_A" "$SELF_ENV_B" "$SELF_ENV_C"

  # #28 — the prefix a trap may `admin objects rm --prefix`: the harness's own minted shape, or refused.
  self_rmok() { if ( store_rm_prefix_ok_ "$1" ) >/dev/null 2>&1; then printf allowed; else printf refused; fi; }
  self_ok "$(self_rmok 'e2e-quota-1789149541/')" allowed "rm --prefix guard: e2e-<word>-<epoch>/ is the shape the three probes mint"
  self_ok "$(self_rmok '')"                      refused "rm --prefix guard: an EMPTY prefix is the bucket root — refused"
  self_ok "$(self_rmok '/')"                     refused "rm --prefix guard: '/' is refused"
  self_ok "$(self_rmok '.')"                     refused "rm --prefix guard: a dot-path is refused"
  self_ok "$(self_rmok 'clips/')"                refused "rm --prefix guard: the stack's own key space is refused"
  self_ok "$(self_rmok 'e2e-quota-1789149541')"  refused "rm --prefix guard: the shape without its trailing slash is refused (it would match a sibling by prefix)"

  # #2 + A1 — lane-teardown's check loop, run as `--check` in a SCRATCH repo (a copy of the script, cd's
  # to its own root): `dev` is refused by name; a merged lane whose worktree has no `.env` is refused; one
  # whose `.env` names the wrong project is refused; the right line passes. Nothing is removed in --check.
  SELF_TD="$E2E_DIR/selftest-teardown"; rm -rf "$SELF_TD"
  mkdir -p "$SELF_TD/repo/scripts"
  cp scripts/lane-teardown.sh "$SELF_TD/repo/scripts/"; printf 'name: clippet\n' > "$SELF_TD/repo/compose.yaml"; printf '.env\n' > "$SELF_TD/repo/.gitignore"
  # A REAL worktree of the scratch repo (`.env` gitignored, as in this repository), so its status is
  # clean and the only thing the check can refuse it for is the `.env` line.
  if git -C "$SELF_TD/repo" init -q -b main 2>/dev/null && git -C "$SELF_TD/repo" add -A 2>/dev/null \
     && git -C "$SELF_TD/repo" -c user.name=selftest -c user.email=selftest@localhost commit -q -m init 2>/dev/null \
     && git -C "$SELF_TD/repo" worktree add -q "$SELF_TD/repo-lane1" -b lane1 2>/dev/null; then
    SELF_RC=0; bash "$SELF_TD/repo/scripts/lane-teardown.sh" --check dev lane1 > "$SELF_OUT" 2>&1 || SELF_RC=$?
    self_ok "$SELF_RC" 1 "lane-teardown --check exits 1 when any named lane is refused"
    self_ok "$(grep -c "^REFUSED: 'dev' is not a lane name" "$SELF_OUT" || true)" 1 "…'dev' is refused by name (the dev worktree is fast-forwarded to main, so 'merged' is always true of it)"
    self_ok "$(grep -c '^REFUSED: .*repo-lane1/.env does not carry the line COMPOSE_PROJECT_NAME=clippet-lane1' "$SELF_OUT" || true)" 1 \
      "…a merged lane whose worktree has NO .env is refused (compose would resolve the project from compose.yaml's own name: the reference)"
    self_ok "$(grep -c 'nothing removed' "$SELF_OUT" || true)" 1 "…and the verdict line says nothing was removed"
    printf 'COMPOSE_PROJECT_NAME=clippet\n' > "$SELF_TD/repo-lane1/.env"
    SELF_RC=0; bash "$SELF_TD/repo/scripts/lane-teardown.sh" --check lane1 > "$SELF_OUT" 2>&1 || SELF_RC=$?
    self_ok "$SELF_RC:$(grep -c '^REFUSED: .*does not carry the line' "$SELF_OUT" || true)" 1:1 "…an .env naming ANOTHER project is refused too (the exact line, not a substring)"
    printf 'S3_SECRET_KEY=never-printed\r\nCOMPOSE_PROJECT_NAME=clippet-lane1\r\n' > "$SELF_TD/repo-lane1/.env"
    SELF_RC=0; bash "$SELF_TD/repo/scripts/lane-teardown.sh" --check lane1 > "$SELF_OUT" 2>&1 || SELF_RC=$?
    self_ok "$SELF_RC:$(grep -c '^OK: lane1 ' "$SELF_OUT" || true):$(grep -c 'never-printed' "$SELF_OUT" || true)" 0:1:0 \
      "…the exact line (CRLF tolerated) passes the check, and nothing else in that .env reaches the transcript"
    # …and the same copy, run FROM the lane worktree, refuses outright (lane guard). `$0`
    # decides which checkout the script thinks it is in, so from inside a worktree it looked for
    # `<worktree>-lane1`, found nothing, and called the lane "already gone" — an OK for the tree it was
    # standing in. git --git-common-dir names the main worktree whatever the cwd is.
    SELF_RC=0; bash "$SELF_TD/repo-lane1/scripts/lane-teardown.sh" --check lane1 > "$SELF_OUT" 2>&1 || SELF_RC=$?
    self_ok "$SELF_RC:$(grep -c '^REFUSED: run this from the reference repo' "$SELF_OUT" || true):$(grep -c '^OK: lane1 ' "$SELF_OUT" || true)" 1:1:0 \
      "…and the same script run FROM a lane worktree refuses instead of reporting OK for the tree it is standing in"
    # The REMOVAL half (run 8, lane harness9 — it had no test: run 6's pipefail stop was found on a real
    # teardown). Two scratch lanes get a commit each and are merged, then the real script runs WITHOUT
    # --check on both in ONE call, with `docker` a stub on PATH that answers a planted listing and records
    # every call, so no daemon is touched. lane2 goes FIRST and owns no volume and no image (its builder
    # ran `down -v`): that is run 6's case, where the empty grep under pipefail ended the script before
    # lane2's branch and before every later lane. Then lane1: its compose project goes by NAME, exactly its
    # own volumes and image go (a sibling lane10, the reference project's volume and a lane1 tag that is not
    # `-app:local` stay), and git loses both worktrees and both branches. What this test creates is
    # recorded FIRST in a manifest and every path in it is checked gone at the end.
    SELF_TDM="$SELF_TD/created.txt"; SELF_TDA="$SELF_TD/docker-args.txt"; : > "$SELF_TDA"
    printf '%s\n' "dir $SELF_TD/repo" "worktree $SELF_TD/repo-lane1" "branch lane1" "worktree $SELF_TD/repo-lane2" "branch lane2" "dir $SELF_TD/bin" > "$SELF_TDM"
    mkdir -p "$SELF_TD/bin"
    { printf '#!/bin/sh\nprintf "%%s\\n" "$*" >> "%s"\n' "$SELF_TDA"
      printf 'case "$1 $2" in\n'
      printf '  "volume ls") printf "clippet-lane1_app_data\\nclippet-lane1_minio_data\\nclippet_app_data\\nclippet-lane10_app_data\\n" ;;\n'
      printf '  "images --format") printf "clippet-lane1-app:local\\nclippet-app:local\\nclippet-lane1-app:gpu\\nclippet-lane10-app:local\\n" ;;\n'
      printf 'esac\nexit 0\n'; } > "$SELF_TD/bin/docker"
    chmod +x "$SELF_TD/bin/docker"
    if printf 'lane work\n' > "$SELF_TD/repo-lane1/lane.txt" && git -C "$SELF_TD/repo-lane1" add lane.txt 2>/dev/null \
       && git -C "$SELF_TD/repo-lane1" -c user.name=selftest -c user.email=selftest@localhost commit -q -m lane 2>/dev/null \
       && git -C "$SELF_TD/repo" -c user.name=selftest -c user.email=selftest@localhost merge -q --no-ff -m merge lane1 2>/dev/null \
       && git -C "$SELF_TD/repo" worktree add -q "$SELF_TD/repo-lane2" -b lane2 2>/dev/null \
       && printf 'COMPOSE_PROJECT_NAME=clippet-lane2\n' > "$SELF_TD/repo-lane2/.env" \
       && printf 'more lane work\n' > "$SELF_TD/repo-lane2/lane2.txt" && git -C "$SELF_TD/repo-lane2" add lane2.txt 2>/dev/null \
       && git -C "$SELF_TD/repo-lane2" -c user.name=selftest -c user.email=selftest@localhost commit -q -m lane2 2>/dev/null \
       && git -C "$SELF_TD/repo" -c user.name=selftest -c user.email=selftest@localhost merge -q --no-ff -m merge lane2 2>/dev/null; then
      SELF_RC=0; PATH="$SELF_TD/bin:$PATH" bash "$SELF_TD/repo/scripts/lane-teardown.sh" --check lane2 lane1 > "$SELF_OUT" 2>&1 || SELF_RC=$?
      self_ok "$SELF_RC:$(grep -c '^OK: lane[12] ' "$SELF_OUT" || true):$(grep -c '^volume rm\|^rmi\|down' "$SELF_TDA" || true)" 0:2:0 \
        "lane-teardown removal: the two merged scratch lanes pass a dry --check first, which removes nothing"
      SELF_RC=0; PATH="$SELF_TD/bin:$PATH" bash "$SELF_TD/repo/scripts/lane-teardown.sh" lane2 lane1 > "$SELF_OUT" 2>&1 || SELF_RC=$?
      self_ok "$SELF_RC" 0 "…then the removal itself exits 0"
      self_ok "$(grep -c '^compose -p clippet-lane2 down --remove-orphans$' "$SELF_TDA" || true):$(grep -c '^compose -p clippet-lane1 down --remove-orphans$' "$SELF_TDA" || true)" 1:1 \
        "…takes each lane's compose project down by NAME (-p clippet-<lane>), once each"
      self_ok "$(grep '^volume rm ' "$SELF_TDA" | tr '\n' ' ')" "volume rm clippet-lane1_app_data volume rm clippet-lane1_minio_data " \
        "…removes exactly lane1's own volumes (not the reference project's, not lane10's; lane2 has none)"
      self_ok "$(grep '^rmi ' "$SELF_TDA" | tr '\n' ' ')" "rmi clippet-lane1-app:local " "…and exactly lane1's clippet-lane1-app:local image"
      self_ok "$(for l in lane2 lane1; do printf '%s:%s ' "$([ -d "$SELF_TD/repo-$l" ] && echo present || echo gone)" "$(git -C "$SELF_TD/repo" show-ref --verify --quiet "refs/heads/$l" && echo present || echo gone)"; done)" "gone:gone gone:gone " \
        "…and git loses both worktrees and both branches — a lane with NO volume or image left does not stop the run (run 6)"
      self_ok "$(grep -c '^branch deleted: lane[12]$' "$SELF_OUT" || true)" 2 "…which the transcript says, for each"
      # lane boxtidy: the teardown ends with the box tidy's read-only report, last but one; a copy with no
      # scripts/box-tidy.sh beside it (this scratch repo) says so and goes on, and the branch list stays the last line.
      self_ok "$(tail -2 "$SELF_OUT" | head -1):$(tail -1 "$SELF_OUT" | grep -c 'main' || true)" "box-tidy: not here:1" \
        "…its last-but-one line is the box tidy's (here: not here — no box-tidy.sh in the scratch copy), the branch list still last"
    else
      self_ok skipped skipped "lane-teardown removal: git could not commit and merge the scratch lanes here (SKIPPED)"
    fi
  else
    self_ok skipped skipped "lane-teardown --check: git could not make the scratch repo here (SKIPPED)"
  fi
  SELF_TDLIST=$(sed -nE 's/^(dir|worktree) //p' "$SELF_TD/created.txt" 2>/dev/null || true)
  rm -rf "$SELF_TD"
  if [ -n "$SELF_TDLIST" ]; then
    self_ok "$(printf '%s\n' "$SELF_TDLIST" | while read -r p; do [ ! -e "$p" ] || printf 'LEFT %s ' "$p"; done)" "" \
      "…and every path the teardown test recorded creating is gone after it (the branch went with its repo)"
  fi

  # #24 — visual.sh's tidy-up runs only when BASE is this checkout's own stack: a dry run with a foreign
  # BASE prints the NOTE and calls docker not at all (a stub `docker` on PATH records every call).
  SELF_BIN="$E2E_DIR/selftest-bin"; mkdir -p "$SELF_BIN"; : > "$SELF_ARGS"
  printf '#!/bin/sh\nprintf "%%s\\n" "$*" >> "%s"\n' "$SELF_ARGS" > "$SELF_BIN/docker"; chmod +x "$SELF_BIN/docker"
  SELF_RC=0; PATH="$SELF_BIN:$PATH" VISUAL_DRY=1 BASE=http://example.invalid bash scripts/visual.sh > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC" 0 "visual.sh VISUAL_DRY=1 with a foreign BASE exits 0 before touching anything"
  self_ok "$(grep -c 'NOTE: BASE http://example.invalid is not this checkout.s own stack' "$SELF_OUT" || true)" 1 "…and says the tidy-up is skipped"
  self_ok "$(grep -c 'tidy-up OFF' "$SELF_OUT" || true)" 1 "…with the decision on its dry-run line"
  self_ok "$(grep -c 'compose exec' "$SELF_ARGS" || true)" 0 "…and ran no 'docker compose exec' (not one prune, not one collections rm)"
  # "left to default" means UNSET here: gate.sh exports BASE (the gate's own stack since run 9) before it runs
  # check.sh, and this self-test runs inside that stage — the case is about visual.sh's default, not the gate's.
  SELF_RC=0; PATH="$SELF_BIN:$PATH" VISUAL_DRY=1 env -u BASE -u PUBLIC_BASE_URL bash scripts/visual.sh > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC:$(grep -c 'tidy-up on' "$SELF_OUT" || true)" 0:1 "…while BASE left to default (unset) to this checkout's own stack keeps the tidy-up on"
  rm -rf "$SELF_BIN" "$SELF_ARGS"

  # #18 — testclips.sh regenerates a fixture that ffprobe cannot read (a 1-byte plant beside a stale
  # `.part`), writes through `.part` and renames on success. Needs the host ffmpeg; one 6 s clip.
  if command -v ffmpeg >/dev/null 2>&1 && command -v ffprobe >/dev/null 2>&1; then
    SELF_CLIPS="$E2E_DIR/selftest-clips"; rm -rf "$SELF_CLIPS"; mkdir -p "$SELF_CLIPS"
    printf 'x' > "$SELF_CLIPS/std-720p30.mp4"; printf 'stale' > "$SELF_CLIPS/.std-720p30.mp4.part"
    SELF_RC=0; TESTCLIPS_ONLY=std-720p30 bash scripts/testclips.sh "$SELF_CLIPS" > "$SELF_OUT" 2>&1 || SELF_RC=$?
    self_ok "$SELF_RC:$(grep -c '! std-720p30.mp4 is not a readable clip' "$SELF_OUT" || true)" 0:1 "testclips.sh sees a 1-byte plant as NOT a fixture and regenerates it"
    self_ok "$([ ! -e "$SELF_CLIPS/.std-720p30.mp4.part" ] && echo gone)" gone "…the stale .part is replaced and renamed away"
    # env -u: the host ffprobe is native and reads this path only through MSYS's conversion, which check.sh's own
    # exported MSYS_NO_PATHCONV=1 would switch off (testclips.sh unsets it for itself the same way).
    self_ok "$(env -u MSYS_NO_PATHCONV ffprobe -v error -show_entries format=duration -of csv=p=0 "$SELF_CLIPS/std-720p30.mp4" 2>/dev/null | grep -c '^[0-9]' || true)" 1 \
      "…and the final name now holds a clip ffprobe reads a duration out of"
    SELF_RC=0; TESTCLIPS_ONLY=std-720p30 bash scripts/testclips.sh "$SELF_CLIPS" > "$SELF_OUT" 2>&1 || SELF_RC=$?
    self_ok "$SELF_RC:$(grep -c '= std-720p30.mp4 (exists)' "$SELF_OUT" || true)" 0:1 "…which the next run keeps"
    rm -rf "$SELF_CLIPS"
  else
    self_ok skipped skipped "testclips.sh regeneration: no ffmpeg/ffprobe on this host (SKIPPED)"
  fi

  # ---- lane `speed2`: the width, the longest-first plan, the durations file, the pool, the Go-tree hash ---
  # The width default (half the CPUs, 2..6 — the measured knee, README "How wide"), and the CPU count it reads is a number.
  self_ok "$(e2e_lanes_default_ 1):$(e2e_lanes_default_ 4):$(e2e_lanes_default_ 8):$(e2e_lanes_default_ 16):$(e2e_lanes_default_ 64)" 2:2:4:6:6 \
    "E2E_LANES defaults from the CPU count: 1→2, 4→2, 8→4, 16→6, 64→6"
  self_ok "$(printf '%s' "$E2E_NCPU" | grep -Ec '^[1-9][0-9]*$' || true)" 1 "…and this machine's CPU count reads as a positive number ($E2E_NCPU)"
  # The planner: longest first from a planted durations file, a bundle with no row planned as the longest
  # known one, ties in name order; with no file at all, name order.
  SELF_DUR="$E2E_DIR/selftest-durations"
  printf '41-social\t154\t2026-10-05T00:00:00Z\n67-capture\t117\t2026-10-05T00:00:00Z\n73-ops\t104\t2026-10-05T00:00:00Z\n94-instance\t5\t2026-10-05T00:00:00Z\n' > "$SELF_DUR"
  self_ok "$(e2e_lpt_order_ "$SELF_DUR" 94-instance 73-ops new-one 41-social 67-capture | tr '\n' ' ')" "41-social new-one 67-capture 73-ops 94-instance " \
    "the plan is longest first, a bundle with no row is planned as the longest known one, ties in name order"
  self_ok "$(e2e_lpt_order_ "$E2E_DIR/selftest-no-such-file" c-b a-z b-y | tr '\n' ' ')" "a-z b-y c-b " "…and with no durations file at all it is name order"
  self_ok "$([ "$(e2e_durations_file_ http://localhost:8549)" != "$(e2e_durations_file_ https://clippet.gg)" ] && case $(e2e_durations_file_ https://clippet.gg) in (*/checkpoints/durations__https___clippet_gg.txt) echo keyed ;; esac)" keyed \
    "the durations file is keyed on the BASE origin (a lane stack's seconds never order another origin's plan)"
  # The merge: a bundle that PASSED gets this run's seconds, one that failed keeps its old row (or none),
  # every other row is kept, and the phase line is never a row.
  SELF_TIMES="$E2E_DIR/selftest-times"; SELF_PASSED="$E2E_DIR/selftest-passed"
  printf '41-social\t140\tslot1\n(parallel phase)\t300\t6 slots\n94-instance\t9\tslot2\nnew-one\t33\tslot3\n' > "$SELF_TIMES"
  printf '41-social\nnew-one\n' > "$SELF_PASSED"
  e2e_durations_merge_ "$SELF_DUR" "$SELF_TIMES" "$SELF_PASSED"
  self_ok "$(cut -f1,2 "$SELF_DUR" | tr '\t\n' ': ')" "41-social:140 67-capture:117 73-ops:104 94-instance:5 new-one:33 " \
    "the durations file takes a PASSED bundle's new seconds, keeps a failed one's old row, and never records the phase line"
  rm -f "$SELF_DUR" "$SELF_TIMES" "$SELF_PASSED"
  # The pool itself, on six planted stub bundles and a stub runner that sleeps instead of testing: THREE
  # slots, s2 and s3 apart, s4 after s1. The stub appends `start`/`end` lines to one log in the order they
  # happen, and the assertions replay that log: never more than three running, s2 and s3 never together,
  # s4 not before s1 ended, all six run once — and the first three started are s1, s2 and s5, because the
  # third slot must SKIP s3 (apart from the running s2) and s4 (s1 still running) for the next in the plan,
  # and s6 must WAIT for a slot although nothing else holds it back (without the cap it would make four).
  SELF_KEEP_DIR=$E2E_DIR; SELF_KEEP_DROP=$E2E_DROPIN_DIR; SELF_KEEP_LANES=$E2E_LANES
  E2E_DIR="$SELF_KEEP_DIR/selftest-pool"; rm -rf "$E2E_DIR"; mkdir -p "$E2E_DIR/stubs"
  E2E_DROPIN_DIR="$E2E_DIR/stubs"
  for s in s1 s2 s3 s4 s5 s6; do printf '# e2e-schedule: parallel\n' > "$E2E_DROPIN_DIR/$s.sh"; done
  printf '# e2e-schedule-after: s1\n' >> "$E2E_DROPIN_DIR/s4.sh"
  printf 's2 s3\ns3 s2\n' > "$E2E_DIR/dropin-apart.txt"
  SELF_POOL_LOG="$E2E_DIR/events.txt"; : > "$SELF_POOL_LOG"
  run_bundle_() {
    local d
    # seconds, wide enough that a slow fork on a LOADED box cannot reorder them: the peak-of-three assertion needs
    # the third slot's bundle to start while s1 and s2 still run, and check.sh runs this beside its other stages
    # (0.3/1.5/2 read a peak of 2 under the clippet-pass gate twice on 2026-10-18 and failed release cycle 14's gate)
    case $1 in s1) d=6 ;; s2) d=4.5 ;; s3) d=1.5 ;; *) d=1 ;; esac
    printf '%s start\n' "$1" >> "$SELF_POOL_LOG"; sleep "$d"; printf '%s end\n' "$1" >> "$SELF_POOL_LOG"
    printf '%s\n' "$1" >> "$E2E_DIR/ran-bundles.txt"; printf '0' > "$E2E_DIR/pool/$1.rc"
  }
  E2E_LANES=3; E2E_POOL_ORDER="s1 s2 s3 s4 s5 s6"; E2E_POOL_ALL=" s1 s2 s3 s4 s5 s6"; SELF_KEEP_DUR=$E2E_DURATIONS; E2E_DURATIONS="$E2E_DIR/none"
  SELF_RC=0; e2e_pool_ > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC:$(sort "$E2E_DIR/ran-bundles.txt" 2>/dev/null | tr '\n' ' ')" "0:s1 s2 s3 s4 s5 s6 " "the pool runs every planned bundle exactly once"
  self_ok "$(awk '$2 == "start" { n++; if (n > m) m = n } $2 == "end" { n-- } END { print m + 0 }' "$SELF_POOL_LOG")" 3 "…never more than E2E_LANES (3) at once, and it did fill all three"
  self_ok "$(awk '$2 == "start" { r[$1] = 1; if (r["s2"] && r["s3"]) bad = 1 } $2 == "end" { delete r[$1] } END { print bad + 0 }' "$SELF_POOL_LOG")" 0 "…two bundles declared apart (s2, s3) never run at the same time"
  self_ok "$(awk '$1 == "s1" && $2 == "end" { e = NR } $1 == "s4" && $2 == "start" { s = NR } END { print (s > e && e > 0) ? "after" : "BEFORE" }' "$SELF_POOL_LOG")" after "…a bundle declared after another (s4 after s1) starts only once it has finished"
  self_ok "$(awk '$2 == "start" { printf "%s ", $1; if (++n == 3) exit }' "$SELF_POOL_LOG")" "s1 s2 s5 " "…and the third slot skipped s3 (apart from s2) and s4 (after s1) for s5, longest first otherwise"
  # A cycle of `after`s cannot run, and the pool says so instead of hanging.
  rm -rf "$E2E_DIR/pool" "$E2E_DIR/ran-bundles.txt"; : > "$SELF_POOL_LOG"
  printf '# e2e-schedule: parallel\n# e2e-schedule-after: s5\n' > "$E2E_DROPIN_DIR/s1.sh"; printf '# e2e-schedule: parallel\n# e2e-schedule-after: s1\n' > "$E2E_DROPIN_DIR/s5.sh"
  E2E_POOL_ORDER="s1 s5"; E2E_POOL_ALL=" s1 s5"
  SELF_RC=0; e2e_pool_ > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC:$(grep -c 'DEADLOCK' "$SELF_OUT" || true)" 1:1 "…a cycle of e2e-schedule-after is a DEADLOCK the pool reports, never a hang"
  E2E_DIR=$SELF_KEEP_DIR; E2E_DROPIN_DIR=$SELF_KEEP_DROP; E2E_LANES=$SELF_KEEP_LANES; E2E_DURATIONS=$SELF_KEEP_DUR; rm -rf "$E2E_DIR/selftest-pool"
  unset -f run_bundle_
  # The Go-tree checkpoint's hash (scripts/lib/gotree.sh, which check.sh sources), on a FIXED scratch
  # tree whose expected value was computed independently of this script (python's hashlib over the same
  # `<sha256> <path>` listing of the seven files, hashed and cut to 16). A file outside the paths and an
  # IGNORED file do not move it; an untracked .go file under internal/ and a content edit do; no git, no hash.
  . scripts/lib/gotree.sh
  SELF_GT="$E2E_DIR/selftest-gotree"; rm -rf "$SELF_GT"; mkdir -p "$SELF_GT/internal/a" "$SELF_GT/cmd/x" "$SELF_GT/web"
  printf '.git\n' > "$SELF_GT/.dockerignore"; printf 'FROM scratch\n' > "$SELF_GT/Dockerfile"
  printf 'package main\n\nfunc main() {}\n' > "$SELF_GT/cmd/x/main.go"; printf 'module x\n' > "$SELF_GT/go.mod"; : > "$SELF_GT/go.sum"
  printf 'package a\n' > "$SELF_GT/internal/a/a.go"; printf '<p>x</p>\n' > "$SELF_GT/web/t.html"
  printf 'readme\n' > "$SELF_GT/README.md"; printf 'internal/ignored.go\n' > "$SELF_GT/.gitignore"
  if git -C "$SELF_GT" init -q 2>/dev/null && git -C "$SELF_GT" -c core.autocrlf=false add -A 2>/dev/null \
     && git -C "$SELF_GT" -c user.name=selftest -c user.email=selftest@localhost commit -q -m tree 2>/dev/null; then
    SELF_H0=$(gotree_hash_ "$SELF_GT")
    self_ok "$SELF_H0" 1ae3db56f69fad4a "the Go-tree hash of the fixed scratch tree is the independently computed value"
    printf 'changed\n' > "$SELF_GT/README.md"; printf 'package a\n' > "$SELF_GT/internal/ignored.go"
    self_ok "$(gotree_hash_ "$SELF_GT")" "$SELF_H0" "…a file outside the Go tree and an IGNORED file do not move it"
    printf 'package a\n' > "$SELF_GT/internal/a/new.go"
    self_ok "$([ "$(gotree_hash_ "$SELF_GT")" != "$SELF_H0" ] && echo moved)" moved "…an UNTRACKED .go file under internal/ moves it (it is in the build context the moment it exists)"
    rm -f "$SELF_GT/internal/a/new.go"; printf 'package a // edited\n' > "$SELF_GT/internal/a/a.go"
    self_ok "$([ "$(gotree_hash_ "$SELF_GT")" != "$SELF_H0" ] && echo moved)" moved "…and so does a content edit of a tracked file"
  else
    self_ok skipped skipped "the Go-tree hash: git could not make the scratch repo here (SKIPPED)"
  fi
  rm -rf "$SELF_GT"; SELF_GT=$(mktemp -d)
  self_ok "$(gotree_hash_ "$SELF_GT")" "" "…and outside a git checkout it prints nothing, which never matches a checkpoint"
  rm -rf "$SELF_GT"

  # ---- lane `excl`: the schedule's classification, the `apart` burst address, the bundle account -----
  # The marker → phase rule on planted files: declared parallel, declared exclusive, NO marker (exclusive
  # by default, and announced — the planner prints the NOTE for exactly this answer), a marker below line
  # 40 (not read, so unmarked), and the retired fixed-lane syntax (fatal).
  SELF_PH="$E2E_DIR/selftest-phase"; rm -rf "$SELF_PH"; mkdir -p "$SELF_PH"
  printf '# x\n# e2e-schedule: parallel\n' > "$SELF_PH/p.sh"
  printf '# e2e-schedule: exclusive\n' > "$SELF_PH/e.sh"
  printf '# a bundle with no marker\nstep 1 "x"\n' > "$SELF_PH/n.sh"
  { i=0; while [ "$i" -lt 40 ]; do echo '#'; i=$((i + 1)); done; echo '# e2e-schedule: parallel'; } > "$SELF_PH/late.sh"
  printf '# e2e-schedule: parallel lane=2\n' > "$SELF_PH/l.sh"
  self_ok "$(for f in p e n late l; do printf '%s ' "$(dropin_phase_ "$SELF_PH/$f.sh")"; done)" "parallel exclusive unmarked unmarked fatal-lane " \
    "the schedule marker: parallel / exclusive as declared, NO marker (or one past line 40) is the exclusive default, the fixed-lane syntax is fatal"
  rm -rf "$SELF_PH"
  # …and the REAL tree, pinned: every drop-in declares its phase (none relies on the default), and the
  # exclusive set is exactly the one scripts/e2e.d/README.md's classification table names. A bundle that
  # changes phase changes this line on purpose, in the same commit as its README row.
  SELF_UNMARKED=""; SELF_EXCL=""
  for f in "$E2E_DROPIN_DIR"/*.sh; do
    [ -e "$f" ] || continue
    case $(dropin_phase_ "$f") in
      unmarked|fatal-lane) SELF_UNMARKED="$SELF_UNMARKED $(basename "$f" .sh)" ;;
      exclusive) SELF_EXCL="$SELF_EXCL $(basename "$f" .sh)" ;;
    esac
  done
  self_ok "${SELF_UNMARKED:-none}" none "every drop-in in scripts/e2e.d declares its schedule"
  self_ok "${SELF_EXCL# }" "87-storage" "the exclusive set is exactly the README's (87-storage: the instance-wide served-bytes counter)"
  # The `apart` burst's address: the same path and query, on the live app's service name — whatever the
  # origin, port or scheme the run is against.
  self_ok "$(burst_url_apart_ 'https://clippet.gg/oembed?format=json&url=https%3A%2F%2Fclippet.gg%2FAbC')" \
    'http://app:8080/oembed?format=json&url=https%3A%2F%2Fclippet.gg%2FAbC' "burst_429 apart: the oEmbed burst keeps its path and query on http://app:8080"
  self_ok "$(burst_url_apart_ 'http://localhost:8548/api/clips/AbCdEfGhIjKl/progress')" \
    'http://app:8080/api/clips/AbCdEfGhIjKl/progress' "…and so does the progress burst from a lane stack's port"
  self_ok "$(burst_url_apart_ 'http://localhost:8548')" 'http://app:8080/' "…and a bare origin is the root"
  SELF_RC=0; ( burst_429 "http://localhost:1/x" 1 "selftest" sideways ) > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC:$(grep -c "unknown source 'sideways'" "$SELF_OUT" || true)" 1:1 "…and an unknown source is refused before anything is started"
  # `bundle_account`: a suffix the sweep could never reach is refused BEFORE any request is made, and
  # every name it can mint is one erratum 272's pattern admits.
  SELF_RC=0; ( E2E_USER=e2e_1791247704; bundle_account abcd ) > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC:$(grep -c 'is not 1-3 of' "$SELF_OUT" || true)" 1:1 "bundle_account refuses a four-character suffix (the sweep pattern stops at three)"
  SELF_RC=0; ( E2E_USER=e2e_1791247704; bundle_account 'A_' ) > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC" 1 "…and one outside [a-z0-9]"
  self_ok "$(for sfx in ac lk ow u ob; do printf '%s\n' "e2e_1791247704_$sfx" | grep -Ec "$E2E_OWN_RE" || true; done | tr -d '\n')" 11111 \
    "…and the suffixes the bundles use (ac lk ow, the probes' u ob) are all names the sweep may act on"
  # Step 3's owner reading (§20 erratum 349): who `admin users` says owns the stack, on planted listings —
  # the run's account, somebody else's (a member run: E2E_IS_OWNER=0, never a die), nobody, a CRLF line.
  SELF_U1='u1  e2e_1791247704  created=2026-10-27T00:00:00Z clips=0 collections=0 tokens=0 discord=- owner=yes'
  SELF_U2='u2  owner  created=2026-09-01T00:00:00Z clips=9 collections=1 tokens=2 discord=- owner=yes'
  SELF_U3='u3  e2e_1791247704  created=2026-10-27T00:00:00Z clips=0 collections=0 tokens=0 discord=- owner=no'
  self_ok "$(printf '%s\n' "$SELF_U1" | owner_of_listing_)" e2e_1791247704 "owner_of_listing_: the run's account owns a fresh stack"
  self_ok "$(printf '%s\n%s\n' "$SELF_U2" "$SELF_U3" | owner_of_listing_)" owner "…and a person owns the reference stack (the run's account is a member there)"
  self_ok "$(printf '%s\n' "$SELF_U3" | owner_of_listing_)" "" "…and nobody owns a stack whose only line is owner=no"
  self_ok "$(printf '%s\r\n' "$SELF_U2" | owner_of_listing_)" owner "…and a CRLF line still reads"
  # …and the stand-down is in the tree: each bundle that reads the owner's pages as the run's account names
  # E2E_IS_OWNER, and nothing prints the remedy that would hand a person's stack to the run's account (the
  # pattern is assembled so that this line does not match itself).
  SELF_MISS=""
  for f in 87-storage 92-setup 94-instance; do grep -q 'E2E_IS_OWNER' "$E2E_DROPIN_DIR/$f.sh" || SELF_MISS="$SELF_MISS $f"; done
  self_ok "${SELF_MISS:-none}" none "87, 92 and 94 put the owner's pages behind E2E_IS_OWNER (a member run stands down)"
  SELF_PAT='admin users owner '"$"'E2E_USER'
  self_ok "$(cat scripts/e2e.sh "$E2E_DROPIN_DIR"/*.sh | grep -cF "$SELF_PAT" || true)" 0 "no line prints 'admin users owner' for the run's account"

  # ---- lane harness9: a credential is never printed whole; every compose probe starts blanked ---------
  # redact_ against planted values, then lacks_secret_f's die line, then the grep that keeps the pattern
  # out of the tree (100-inhook's die printed a live inbound URL) — on planted lines first, so the grep is
  # proven to SEE each shape, then on every script. The planted lines spell `$` as `@` here so that this
  # file does not match its own grep; sed turns them back.
  self_ok "$(redact_ 'http://localhost:8572/in/ci_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG')" \
    "'http://localhost:8572/in/ci_abcdefgh…' (71 chars)" "redact_: an inbound URL keeps its origin, its route and its ci_ display prefix, and says its length"
  self_ok "$(redact_ 'ck_abcdefghijklmnop')" "'ck_abcdefgh…' (19 chars)" "…a capture token its ck_ display prefix"
  self_ok "$(redact_ 'deadbeefcafe0123')" "'dead…' (16 chars)" "…a value of no known prefix (a signing key) four characters"
  self_ok "$(redact_ 'http://x/in/ci_abcdefghijklmnopqrstuvwxyz/x' | grep -c 'ijklmn' || true)" 0 "…and a prefixed value BEFORE the last slash is cut too"
  SELF_SEC="$E2E_DIR/selftest-secret"; printf 'a line with ci_SELFTESTsecretBODYxxxxxxxxxxxxxxxxxxxxxxxxxx in it\n' > "$SELF_SEC"
  SELF_RC=0; ( STEP=selftest; CTX_DIR=; lacks_secret_f "$SELF_SEC" ci_SELFTESTsecretBODYxxxxxxxxxxxxxxxxxxxxxxxxxx "the planted secret" ) > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC:$(grep -c 'secretBODY' "$SELF_OUT" || true):$(grep -c "'ci_SELFTEST…' (47 chars) found in selftest-secret" "$SELF_OUT" || true)" 1:0:1 \
    "lacks_secret_f fails on a credential it finds and names it by its shape, never its body"
  SELF_RING="$E2E_DIR/selftest-ring"; rm -rf "$SELF_RING"; mkdir -p "$SELF_RING"
  ( CTX_DIR=$SELF_RING; ring_ POST "http://localhost:8572/in/ci_SELFTESTsecretBODYxxxxxxxxxxxxxxxxxxxxxxxxxxx" 201
    ring_ GET "http://localhost:8572/in/auto/ca_SELFTESTsecretBODYxxxxxxxxxxxxxxxxxxxxxxxxxxx" 405
    ring_ GET "http://localhost:8572/api/clips/AbCdEfGhIjKl" 200 )
  self_ok "$(grep -c 'secretBODY' "$SELF_RING/requests.txt" || true):$(grep -c '/in/ci_SELFTEST…\|/in/auto/ca_SELFTEST…' "$SELF_RING/requests.txt" || true):$(grep -c '/api/clips/AbCdEfGhIjKl -> 200' "$SELF_RING/requests.txt" || true)" 0:2:1 \
    "the request ring (printed on every FAIL) names an inbound and a Clippet Auto URL by their shape, and keeps an ordinary one whole"
  rm -rf "$SELF_RING"
  printf '%s\n' \
    "die \"the mint's stdout is not an inbound URL: '@IH_TOKURL'\"" \
    'die "the mint is not one: @(redact_ "@IH_TOKURL")"' \
    'expect "@(json sxcu.Headers.Authorization)" "Bearer @SXTOK" "sxcu Authorization"' \
    'lacks_file_f "@F" "@MT_TOK" "the raw token must never reach stderr"' \
    "has_file_f \"@F\" \"ci_@(printf '%s' \"@IH_RAW\" | cut -c4-11)\" \"the display prefix\"" \
    "lacks_file \"@F\" \"@(printf '%s' \"@IH_RAW\" | cut -c4-)\" \"the secret's body\"" \
    'lacks_secret_f "@F" "@IH_RAW" "the secret must never reach stderr"' \
    'expect "@{#IH_RAW}" 46 "the length"' \
    '  # die "a comment naming @HK_SECRET"' \
    'die "the planted webhook @{E2E_DISCORD_WEBHOOK_URL}"' \
    'die "login Set-Cookie lacks HttpOnly: @{LOGIN_COOKIE#*;}"' \
    'die "login Set-Cookie lacks HttpOnly: @LOGIN_COOKIE"' | sed 's/@/$/g' > "$SELF_SEC"
  self_ok "$(secret_print_lines_ "$SELF_SEC" | cut -d: -f2 | tr '\n' ' ')" "1 3 4 6 10 12 " \
    "secret_print_lines_ sees a die / expect / lacks_file naming a credential whole, and passes a redact_, a bounded prefix cut, a length, a cookie's attributes, lacks_secret_f and a comment"
  self_ok "$(secret_print_lines_ scripts/e2e.sh "$E2E_DROPIN_DIR"/*.sh | head -5)" "" \
    "no script in the suite prints a credential-named variable whole on a failure"
  rm -f "$SELF_SEC"
  # probe_env_blank: the list, its PLACE (first, so a probe's own value comes after and wins — compose
  # takes the last -e of a name), `dc` adding it to `run` and to nothing else, and no drop-in starting a
  # compose probe any other way than `dc run`.
  self_ok "$(probe_env_blank | tr '\n' ' ')" "-e SESSION_COOKIE_NAME= -e DISCORD_WEBHOOK_URL= -e DISCORD_WEBHOOK_ROUTES= -e CLIP_WEBHOOK_URL= -e CLIP_WEBHOOK_SECRET= " \
    "probe_env_blank blanks the cookie name, Discord's webhook and the clip webhook (CLIP_WEBHOOK_URL and _SECRET)"
  SELF_ARGS="$E2E_DIR/selftest-docker-args"; : > "$SELF_ARGS"
  docker_() { printf '%s\n' "$*" >> "$SELF_ARGS"; }
  dc run --rm -T --no-deps -e CLIP_WEBHOOK_URL=http://own.example/x app clippet serve >/dev/null 2>&1 || true
  dc exec -T app true >/dev/null 2>&1 || true
  self_ok "$(sed -n 1p "$SELF_ARGS")" "compose run -e SESSION_COOKIE_NAME= -e DISCORD_WEBHOOK_URL= -e DISCORD_WEBHOOK_ROUTES= -e CLIP_WEBHOOK_URL= -e CLIP_WEBHOOK_SECRET= --rm -T --no-deps -e CLIP_WEBHOOK_URL=http://own.example/x app clippet serve" \
    "dc run puts probe_env_blank FIRST and the probe's own -e after it (so 97-hook's own URL wins)"
  self_ok "$(sed -n 2p "$SELF_ARGS")" "compose exec -T app true" "…and dc exec passes through untouched (the stack under test keeps its own variables)"
  docker_() { self_stub; }
  rm -f "$SELF_ARGS"
  self_ok "$(grep -nE '(^|[[:space:];|&(])docker_? +compose( +[^|;&]*)? +run( |$)' "$E2E_DROPIN_DIR"/*.sh | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#' | head -3 || true)" "" \
    "no drop-in starts a compose probe except through dc run (every probe that boots clippet serve gets probe_env_blank)"
  self_ok "$(grep -hoE '^[[:space:]]*bundle_account +[a-z0-9]{1,3}' "$E2E_DROPIN_DIR"/*.sh | awk '{print $2}' | sort | uniq -d | tr '
' ' ')" ""     "every bundle_account suffix is used by ONE drop-in (two parallel bundles registering the same account is a 409 — gate 27a, 33-account vs 104-audiocard)"

  # ---- run 9, lane `retry`: the classifier's table, the retry's namespace, and the retry itself ------
  # transient_sig on planted logs — each signature it must name, and the failures it must NEVER name,
  # four of them shaped on real ones (gate 27a's 409, gate 28b's 301, gate 23's visual pass, the core's
  # step-29 curl 35 in gate-nits1.log).
  SELF_TR="$E2E_DIR/selftest-transient"; rm -rf "$SELF_TR"; mkdir -p "$SELF_TR/ctx"
  self_sig() {   # self_sig <name> <want> <what> <line>... — plant a log, classify it with an empty context
    local name=$1 want=$2 what=$3; shift 3
    printf '%s\n' "$@" > "$SELF_TR/$name.log"
    self_ok "$(transient_sig "$SELF_TR/$name.log" "$SELF_TR/ctx")" "$want" "transient_sig: $what"
  }
  SELF_RING_HDR="[e2e]   the last five requests before this failure:"
  self_sig c28 curl-28 "curl 28 on the failing request is curl-28" "[e2e] step 99: x" "curl: (28) Operation timed out after 1000 milliseconds with 0 bytes received" \
    "[e2e] FAIL step 99: GET /x: got '000', want '200'" "$SELF_RING_HDR" "[e2e]     GET http://localhost:8578/x -> 000"
  self_sig c35 curl-35 "the core's step-29 shape (gate-nits1.log: curl 35, then the ERR trap's FAIL) is curl-35" \
    "[e2e]   waited upload a1D2g9PAqkwx answered ready after 3s" "curl: (35) Recv failure: Connection was reset" "[e2e] FAIL step 29: command failed (line 1797)"
  self_sig c7 curl-7 "curl 7 (could not connect) is curl-7" "curl: (7) Failed to connect to localhost port 8578" "[e2e] FAIL step 47: the probe got no answer"
  self_sig c52 curl-52 "curl 52 (empty reply) is curl-52" "curl: (52) Empty reply from server" "[e2e] FAIL step 47: no answer"
  self_sig c56 curl-56 "curl 56 (receive failure) is curl-56" "curl: (56) Recv failure: Connection reset by peer" "[e2e] FAIL step 47: no answer"
  self_sig cabs "" "a curl failure curl_ already retried and got past is NOT the cause" "curl: (28) Operation timed out" \
    "[e2e]   NOTE: curl exit 28 (no answer); retrying once in 3s" "[e2e]   Z trim done after 24s" "[e2e] FAIL step 24: Z trimmed twice"
  self_sig cboot "" "a curl 7 printed while a probe waited for its server, then a failure the ring shows ANSWERED, is not one" \
    "curl: (7) Failed to connect to 127.0.0.1 port 8080" "[e2e]   the probe is up" "[e2e] FAIL step 89: the quota line is wrong" \
    "$SELF_RING_HDR" "[e2e]     GET http://localhost:8578/api/storage -> 200"
  self_sig dabs "" "a daemon refusal dc retried and got past is not the cause either" \
    "docker: Error response from daemon: Conflict." "[e2e]   NOTE: docker compose exited 125 (the daemon, not the command); retrying once in 3s" \
    "[e2e]   the probe ran" "[e2e] FAIL step 51: the doctor printed 15 rows"
  self_sig c22 "" "curl 22 (an HTTP error answered) is not a transient" "curl: (22) The requested URL returned error: 404" "[e2e] FAIL step 47: no clip"
  self_sig e502 edge-502 "a 502 with the edge's own header in the log is edge-502" "< server: cloudflare" "< cf-ray: 8c0ffee0deadbeef-AMS" \
    "[e2e] FAIL step 48: upload over the cap: got '502', want '413'" "$SELF_RING_HDR" "[e2e]     POST https://clippet.gg/api/upload -> 502"
  self_sig e502l "" "a 502 with NO edge evidence (the stack under test answered it) is not a transient" \
    "[e2e] FAIL step 48: upload over the cap: got '502', want '413'" "$SELF_RING_HDR" "[e2e]     POST http://localhost:8578/api/upload -> 502"
  printf 'HTTP/2 522\r\nserver: cloudflare\r\ncf-ray: 8c0ffee0deadbeef-AMS\r\n' > "$SELF_TR/ctx/resp.hdr"
  self_sig e522 edge-522 "a 522 whose RESPONSE FILE carries the edge's header (the context dir) is edge-522" \
    "[e2e] FAIL step 65: GET the tier: got '522', want '200'" "$SELF_RING_HDR" "[e2e]     GET https://clippet.gg/x -> 522"
  self_sig e409c "" "…and with the same edge header present, an app 409 is still NOT one" \
    "[e2e] FAIL step 33: register the bundle's own account e2e_1791361979_ac ({\"error\":\"username_taken\"}): got '409', want '201'" \
    "$SELF_RING_HDR" "[e2e]     POST https://clippet.gg/api/auth/register -> 409"
  rm -f "$SELF_TR/ctx/resp.hdr"
  printf '<html><title>clippet.gg | 524: A timeout occurred</title>Error code 524 … Cloudflare Ray ID: 8c0ffee0deadbeef</html>\n' > "$SELF_TR/ctx/page.html"
  self_sig e524 edge-524 "a 524 whose page is Cloudflare's error body is edge-524" \
    "[e2e] FAIL step 43: share page: got '524', want '200'" "$SELF_RING_HDR" "[e2e]     GET https://clippet.gg/AbCdEfGhIjKl -> 524"
  rm -f "$SELF_TR/ctx/page.html"
  self_sig a409 "" "an app 409 (gate 27a: the suffix collision) is never a transient" \
    "[e2e] FAIL step 33: register the bundle's own account e2e_1791361979_ac ({\"error\":\"username_taken\",\"message\":\"that username is taken\"}): got '409', want '201'" \
    "$SELF_RING_HDR" "[e2e]     POST https://clippet.gg/api/auth/register -> 409"
  self_sig a409c "" "…not even with a curl 28 on the line right above it: the app's own answer settles it" \
    "curl: (28) Operation timed out" "[e2e] FAIL step 33: register the bundle's own account: got '409', want '201'" \
    "$SELF_RING_HDR" "[e2e]     POST http://localhost:8578/api/auth/register -> 409"
  self_sig a301 "" "a 301 (gate 28b: the zone's redirect rule) is never a transient" \
    "[e2e] FAIL step 71: GET /?mine=1 with no session: got '301', want '200'" "$SELF_RING_HDR" "[e2e]     GET https://clippet.gg/?mine=1 -> 301"
  self_sig pwc pw-closed "Playwright's ERR_CONNECTION_CLOSED (gate 23's seven shots) is pw-closed" \
    "[visual] FAIL library light 390 — exception: Error: page.goto: net::ERR_CONNECTION_CLOSED at https://clippet.gg/" "[e2e] FAIL step 70: the browser probe failed"
  self_sig pwr pw-reset "…ERR_CONNECTION_RESET is pw-reset" "[e2e] FAIL step 77: the probe died: Error: page.goto: net::ERR_CONNECTION_RESET at http://host.docker.internal:8578/"
  self_sig pwt pw-timeout "…and a net:: timeout is pw-timeout" "Error: page.goto: net::ERR_CONNECTION_TIMED_OUT at http://host.docker.internal:8578/" "[e2e] FAIL step 47: the upload page never loaded"
  self_sig pwa "" "a Playwright ASSERTION is not a transient" "Error: expect(received).toBe(expected) // the title" "[e2e] FAIL step 70: the library tile's title"
  self_sig d125 docker-125 "the daemon's refusal (docker run's own prefix) is docker-125" \
    "docker: Error response from daemon: driver failed programming external connectivity on endpoint x." "[e2e] FAIL step 85: the enrich probe did not start"
  self_sig d125r docker-125 "…and so is a FAIL line naming rc=125" "[e2e] FAIL step 88: the fsstore probe exited rc=125"
  self_sig d1 "" "…but a probe's own exit 1 is not" "[e2e] FAIL step 88: the fsstore probe exited rc=1"
  self_sig gof "" "a Go assertion is not a transient" "--- FAIL: TestNotifyGivesUpAfterThreeAttempts (10.02s)" "[e2e] FAIL step 30: go test failed"
  self_sig fix "" "a missing fixture is not a transient" "[e2e] FAIL step 6: testclips/std-720p30.mp4 is missing (scripts/testclips.sh)"
  self_sig none "" "a log with no FAIL line (a runner killed from outside) is not classified" "[e2e] ===== bundle 47-upload (pool slot 1 of 6) =====" "[e2e] step 47: x"
  rm -rf "$SELF_TR"
  # The retry's namespace: a fresh name per suffix, inside the sweep's pattern, never a first attempt's.
  self_ok "$(transient_retry_user e2e_1791247704)" e2e_17912477042 "the retry's stamp is the run's with a 2 (eleven digits)"
  self_ok "$(E2E_USER=e2e_1791247704; E2E_ATTEMPT=1; bundle_user_ ac):$(E2E_USER=e2e_1791247704; E2E_ATTEMPT=2; bundle_user_ ac)" \
    "e2e_1791247704_ac:e2e_17912477042_ac" "bundle_user_: the first attempt's account, and a retry's fresh one"
  SELF_SFX=$( { grep -hoE '^[[:space:]]*bundle_account +[a-z0-9]{1,3}' "$E2E_DROPIN_DIR"/*.sh | awk '{print $2}'
                grep -hoE 'bundle_user_ +[a-z0-9]{1,3}' "$E2E_DROPIN_DIR"/*.sh | awk '{print $2}'; } | sort -u | tr '\n' ' ')
  self_ok "$(for s in $SELF_SFX; do n=$(E2E_USER=e2e_1791247704; E2E_ATTEMPT=2; bundle_user_ "$s"); printf '%s\n' "$n" | grep -Eq "$E2E_OWN_RE" || printf '%s ' "$n"; done)" "" \
    "every drop-in suffix's RETRY account (${SELF_SFX% }) is inside the sweep's pattern"
  self_ok "$(for s in $SELF_SFX; do n=$(E2E_USER=e2e_1791247704; E2E_ATTEMPT=2; bundle_user_ "$s"); for t in $SELF_SFX; do [ "$n" != "e2e_1791247704_$t" ] || printf '%s ' "$n"; done; done)" "" \
    "…and none of them is any drop-in's FIRST-attempt account (102-dedupe's dd and dd2 stay apart)"
  SELF_RC=0; ( E2E_USER=e2e_17912477041; E2E_ATTEMPT=2; E2E_BUNDLE=33-account; bundle_account ac ) > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC:$(grep -c 'outside the sweep' "$SELF_OUT" || true)" 1:1 "bundle_account refuses a retry name the sweep could not reach (a 12-digit stamp), before any request"
  # The retry itself: e2e_run_bundle_ on three planted bundles, each logging every attempt it is run.
  # rt fails on a curl 28 and passes on the retry; ru fails on an app 409; r2 fails on a curl 28 twice.
  SELF_KEEP_DIR=$E2E_DIR; SELF_KEEP_DROP=$E2E_DROPIN_DIR; SELF_KEEP_USER=${E2E_USER:-}
  E2E_DIR="$SELF_KEEP_DIR/selftest-retry"; rm -rf "$E2E_DIR"; mkdir -p "$E2E_DIR/stubs" "$E2E_DIR/pool"
  E2E_DROPIN_DIR="$E2E_DIR/stubs"; E2E_USER=e2e_1791247704
  ckpt_record_() { printf '%s\n' "$1" >> "$E2E_DIR/ckpt.txt"; }
  ckpt_forget_() { :; }
  printf '%s\n' 'printf "%s %s\n" "$E2E_ATTEMPT" "$(bundle_user_ rt)" >> "$E2E_DIR/runs-rt.txt"' \
    'if [ "$E2E_ATTEMPT" = 1 ]; then echo "curl: (28) Operation timed out"; ring_ GET http://localhost:1/x 000; die "GET /x: got '"'000'"', want '"'200'"'"; fi' \
    'echo "[e2e]   rt passed on attempt $E2E_ATTEMPT"' > "$E2E_DROPIN_DIR/rt.sh"
  printf '%s\n' 'printf "%s\n" "$E2E_ATTEMPT" >> "$E2E_DIR/runs-ru.txt"' \
    'ring_ POST http://localhost:1/api/auth/register 409; die "register: got '"'409'"', want '"'201'"'"' > "$E2E_DROPIN_DIR/ru.sh"
  printf '%s\n' 'printf "%s\n" "$E2E_ATTEMPT" >> "$E2E_DIR/runs-r2.txt"' \
    'echo "curl: (28) Operation timed out"; ring_ GET http://localhost:1/x 000; die "GET /x: got '"'000'"', want '"'200'"'"' > "$E2E_DROPIN_DIR/r2.sh"
  for s in rt ru r2; do e2e_run_bundle_ "$s" 1 >> "$SELF_OUT" 2>&1 || true; done
  self_ok "$(tr '\n' '|' < "$E2E_DIR/runs-rt.txt")" "1 e2e_1791247704_rt|2 e2e_17912477042_rt|" \
    "the pool retries a classified failure EXACTLY once, the retry on a fresh account namespace"
  self_ok "$(cat "$E2E_DIR/pool/rt.rc"):$(grep -cx rt "$E2E_DIR/ran-bundles.txt" || true):$(grep -cx rt "$E2E_DIR/failed-bundles.txt" 2>/dev/null || true):$(grep -cx rt "$E2E_DIR/ckpt.txt" || true)" "0:1:0:1" \
    "…a retry that passes is a pass (ran, checkpointed, not failed)"
  self_ok "$(grep -c 'FAIL step' "$E2E_DIR/bundle-rt.1.log" 2>/dev/null || true):$(grep -c 'RETRY, attempt 2' "$E2E_DIR/bundle-rt.log" || true):$(grep -c 'FAIL step' "$E2E_DIR/bundle-rt.log" || true)" "1:1:0" \
    "…the first attempt's log is KEPT as bundle-rt.1.log and the retry's is bundle-rt.log"
  self_ok "$(awk -F'\t' '$1 == "rt" && $2 == "curl-28" && $3 ~ /^[0-9]+$/ && $4 ~ /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z$/ { n++ } END { print n + 0 }' "$E2E_DIR/retries.txt")" 1 \
    "…and retries.txt has its row: stem, signature, first-attempt seconds, utc"
  self_ok "$(tr '\n' '|' < "$E2E_DIR/runs-ru.txt"):$(cat "$E2E_DIR/pool/ru.rc"):$(grep -c '^ru	' "$E2E_DIR/retries.txt" || true):$([ -e "$E2E_DIR/bundle-ru.1.log" ] && echo kept || echo none)" "1|:1:0:none" \
    "an UNCLASSIFIED failure (an app 409) is never retried: one run, failed, no row, no first-attempt log"
  self_ok "$(tr '\n' '|' < "$E2E_DIR/runs-r2.txt"):$(cat "$E2E_DIR/pool/r2.rc"):$(grep -cx r2 "$E2E_DIR/failed-bundles.txt" || true)" "1|2|:1:1" \
    "a SECOND failure is a failure (two runs, never a third)"
  self_ok "$(e2e_retried_line_)" "retried: rt (curl-28, 1 retry), r2 (curl-28, 1 retry, failed again)" "the closing banner's line names every retry and the one that failed again"
  rm -rf "$E2E_DIR/pool" "$E2E_DIR"/runs-*.txt "$E2E_DIR"/*.log "$E2E_DIR"/*-bundles.txt "$E2E_DIR/retries.txt"; mkdir -p "$E2E_DIR/pool"
  SELF_KEEP_RETRY=$E2E_RETRY; E2E_RETRY=0
  e2e_run_bundle_ rt 1 >> "$SELF_OUT" 2>&1 || true
  self_ok "$(tr '\n' '|' < "$E2E_DIR/runs-rt.txt"):$(cat "$E2E_DIR/pool/rt.rc"):$([ -e "$E2E_DIR/retries.txt" ] && echo row || echo none):$(e2e_retried_line_)" \
    "1 e2e_1791247704_rt|:1:none:retried: none" "E2E_RETRY=0: the same classified failure is NOT retried, and the banner says retried: none"
  E2E_RETRY=$SELF_KEEP_RETRY
  E2E_DIR=$SELF_KEEP_DIR; E2E_DROPIN_DIR=$SELF_KEEP_DROP; E2E_USER=$SELF_KEEP_USER; rm -rf "$E2E_DIR/selftest-retry"
  unset -f ckpt_record_ ckpt_forget_ self_sig
  # ---- run 9, lane `select`: the namespace registry and the change map (scripts/lib/e2e-registry.sh) ----
  # The REAL tree first: every drop-in's `# e2e-namespace:` and `# e2e-covers:` lines, scripts/e2e.d/CORE
  # and GO-ONLY, against every product file under internal/ cmd/ web/. A new product file with no row, a
  # glob that went stale, a prefix or an account two bundles share — any of them fails here, i.e. in T0.
  SELF_RC=0; SELF_REG=$(reg_check_ "$E2E_DROPIN_DIR" . 2>&1) || SELF_RC=$?
  self_ok "$SELF_RC:${SELF_REG:-clean}" "0:clean" "the registry and the change map hold on the real tree (bash scripts/lib/e2e-registry.sh --check)"
  self_ok "$(reg_scan_ "$E2E_DROPIN_DIR" | awk -F'\t' '$1 == "NS" && $3 != "-" { n = split($3, s, ","); for (i = 1; i <= n; i++) print "e2e_1791247704_" s[i] }' \
    | { grep -Ev "$E2E_OWN_RE" || true; } | head -1)" "" \
    "…and every account suffix a bundle declares is a name the sweep may act on"
  # …then every refusal, planted at once in a scratch registry over a scratch product tree: two clean
  # stub bundles (x1, x2 — apart, both draining `view`) and one stub per refusal (p_*). Every refusal
  # line NAMES the file it is about, so each assertion greps for its own stub's line and one plant can
  # never pass another's assertion. The clean pair is checked alone first: two apart bundles may share a
  # bucket, and nothing in the clean shape is refused.
  SELF_RG="$E2E_DIR/selftest-registry"; rm -rf "$SELF_RG"
  mkdir -p "$SELF_RG/root/internal/a" "$SELF_RG/root/web" "$SELF_RG/d"
  printf 'package a\n' > "$SELF_RG/root/internal/a/a.go"; printf '<p>x</p>\n' > "$SELF_RG/root/web/t.html"
  printf 'package a\n\nimport "testing"\n\nfunc TestA(t *testing.T) {}\n' > "$SELF_RG/root/internal/a/a_test.go"
  self_stub_() {   # self_stub_ <stem> <namespace line or ""> <covers line or ""> <body line>…
    local f="$SELF_RG/d/$1.sh"; printf '# e2e-schedule: parallel\n' > "$f"
    [ -z "$2" ] || printf '# e2e-namespace: %s\n' "$2" >> "$f"
    [ -z "$3" ] || printf '# e2e-covers: %s\n' "$3" >> "$f"
    shift 3; [ $# = 0 ] || printf '%s\n' "$@" >> "$f"
  }
  self_stub_ x1 'account=aa prefix=XA_ buckets=view' 'internal/a/a.go web/*.html' '# e2e-schedule-apart: x2' 'bundle_account aa' 'XA_ONE=1'
  self_stub_ x2 'account=bb prefix=XB_ buckets=view' 'internal/a/**' 'XB_ONE=1; XB_U="${E2E_USER}_bb"'
  : > "$SELF_RG/d/GO-ONLY"; : > "$SELF_RG/d/CORE"
  SELF_RC=0; reg_check_ "$SELF_RG/d" "$SELF_RG/root" > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC:$(cat "$SELF_OUT")" "0:" "…a planted registry of two clean bundles is clean (two APART bundles may drain one bucket)"
  self_stub_ p_dupsfx  'account=aa prefix=XD_ buckets=-' 'internal/a/a.go' 'XD_1=1'
  self_stub_ p_coresfx 'account=b prefix=XC_ buckets=-'  'internal/a/a.go' 'XC_1=1'
  self_stub_ p_undecl  'account=- prefix=XN_ buckets=-'  'internal/a/a.go' 'XN_1=1' 'bundle_account zz'
  self_stub_ p_badsfx  'account=abcd prefix=XS_ buckets=-' 'internal/a/a.go' 'XS_1=1'
  self_stub_ p_duppfx1 'account=- prefix=XV_ buckets=-'  'internal/a/a.go' 'XV_1=1'
  self_stub_ p_duppfx2 'account=- prefix=XV_ buckets=-'  'internal/a/a.go'
  self_stub_ p_nest    'account=- prefix=Y buckets=-'    'internal/a/a.go' 'Y=1'
  self_stub_ p_nest2   'account=- prefix=YZ_ buckets=-'  'internal/a/a.go' 'YZ_1=1'
  self_stub_ p_foreign 'account=- prefix=XF_ buckets=-'  'internal/a/a.go' 'XF_1=1' 'XA_TWO=2'
  self_stub_ p_unused  'account=- prefix=XU_,XQ_ buckets=-' 'internal/a/a.go' 'XU_1=1'
  self_stub_ p_clash1  'account=- prefix=XK_ buckets=poll' 'internal/a/a.go' 'XK_1=1'
  self_stub_ p_clash2  'account=- prefix=XL_ buckets=poll' 'internal/a/a.go' 'XL_1=1'
  self_stub_ p_badbk   'account=- prefix=XM_ buckets=nope' 'internal/a/a.go' 'XM_1=1'
  self_stub_ p_nons    ''                                'internal/a/a.go' 'XO_1=1'
  self_stub_ p_badns   'account=- prefix=XP_'            'internal/a/a.go' 'XP_1=1'
  self_stub_ p_nocov   'account=- prefix=XR_ buckets=-'  ''                'XR_1=1'
  self_stub_ p_stale   'account=- prefix=XT_ buckets=-'  'internal/a/a.go internal/gone/**' 'XT_1=1'
  self_stub_ p_syntax  'account=- prefix=XW_ buckets=-'  'internal/a/a.go' 'XW_1=1' "XW_2=\"\${2:-the mock's record}\""
  mkdir -p "$SELF_RG/root/cmd/new"; printf 'package main\n' > "$SELF_RG/root/cmd/new/main.go"
  printf 'web/t.html TestNope\n' > "$SELF_RG/d/GO-ONLY"
  SELF_RC=0; reg_check_ "$SELF_RG/d" "$SELF_RG/root" > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC" 1 "…and with every plant below in it, the registry is refused"
  self_reg_() {   # self_reg_ <fixed string the refusal line carries> <what>
    self_ok "$( { grep -cF -- "$1" "$SELF_OUT" || true; } | sed 's/^[1-9][0-9]*$/seen/')" seen "the registry refuses $2"
  }
  self_reg_ 'REFUSE dup-suffix: account suffix "aa" is declared by p_dupsfx.sh AND x1.sh' "an account suffix two bundles declare"
  self_reg_ 'REFUSE dup-suffix: p_coresfx.sh declares account suffix "b"'     "…and one that is the core's own (_b)"
  self_reg_ 'REFUSE undeclared-suffix: p_undecl.sh registers or names the account suffix "zz"' "an account suffix a bundle registers and does not declare"
  self_reg_ 'REFUSE bad-suffix: p_badsfx.sh declares account suffix "abcd"'   "a suffix the sweep could never reach"
  self_reg_ 'REFUSE dup-prefix: prefix XV_ is declared by p_duppfx1.sh AND p_duppfx2.sh' "a variable prefix two bundles declare"
  self_reg_ 'REFUSE dup-prefix: prefix Y (p_nest.sh) is the start of YZ_ (p_nest2.sh)' "…and one that is the START of another bundle's"
  self_reg_ "REFUSE foreign-prefix: p_foreign.sh assigns XA_TWO, inside x1.sh's prefix XA_" "a variable assigned inside another bundle's prefix"
  self_reg_ 'REFUSE syntax: p_syntax.sh does not parse (bash -n)' "a drop-in bash cannot parse (it would pass with half its assertions unrun)"
  self_reg_ 'REFUSE unused-prefix: p_unused.sh declares prefix XQ_'           "a declared prefix no variable in the file uses"
  self_reg_ 'REFUSE bucket-clash: p_clash1.sh and p_clash2.sh both drain the poll bucket' "a bucket two bundles drain without apart"
  self_reg_ 'REFUSE bad-bucket: p_badbk.sh declares bucket "nope"'            "a bucket that is not one of the app's limiters"
  self_reg_ 'REFUSE no-namespace: p_nons.sh has no'                           "a bundle with no namespace line"
  self_reg_ 'REFUSE bad-namespace: p_badns.sh:'                               "a namespace line missing a key"
  self_reg_ 'REFUSE no-covers: p_nocov.sh has no'                             "a bundle with no covers line"
  self_reg_ 'REFUSE stale-glob: p_stale.sh covers "internal/gone/**"'         "a covers glob that matches no product file"
  self_reg_ 'REFUSE uncovered: cmd/new/main.go is covered by no'              "a product file no bundle, CORE row or GO-ONLY row covers"
  self_reg_ 'REFUSE go-only-test: GO-ONLY row "web/t.html" names TestNope'    "a GO-ONLY row naming a test that does not exist"
  self_ok "$(grep -cE '(^|[^a-z0-9_])x2\.sh' "$SELF_OUT" || true)" 0 "…and the clean stub x2 is named by none of them"
  rm -rf "$SELF_RG"; unset -f self_stub_ self_reg_
  # bundle_account reads the CALLING bundle's header: a suffix it does not declare is refused before any
  # request, a call outside a bundle is refused, and a declared one goes on to register (http stubbed).
  SELF_RC=0; ( E2E_USER=e2e_1791247704; E2E_BUNDLE=33-account; bundle_account zz ) > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC:$(grep -c "suffix 'zz' is not declared by 33-account.sh" "$SELF_OUT" || true)" 1:1 "bundle_account refuses a suffix its bundle's header does not declare"
  SELF_RC=0; ( E2E_USER=e2e_1791247704; E2E_BUNDLE=; bundle_account ac ) > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC:$(grep -c 'called outside a bundle' "$SELF_OUT" || true)" 1:1 "…and a call from outside any bundle"
  SELF_RC=0; ( E2E_USER=e2e_1791247704; E2E_PASS=selftest; E2E_BUNDLE=33-account; http() { echo 201; }; bundle_account ac ) > "$SELF_OUT" 2>&1 || SELF_RC=$?
  self_ok "$SELF_RC:$(grep -c "own account: e2e_1791247704_ac" "$SELF_OUT" || true)" 0:1 "…and registers one it does (33-account declares ac)"
  # scripts/select.sh's rules, on the real map: (c) docs, a Go test and a GO-ONLY file select nothing;
  # (b) a migration selects T2; (a) a product file nothing covers and a CORE file select the whole suite;
  # a covered file and its own drop-in select exactly their bundle.
  self_ok "$(bash scripts/select.sh --files 'README.md docs/x.md internal/httpapi/media_test.go internal/worker/freebytes_windows.go' 2>&1)" none "select.sh: docs, a Go test and a GO-ONLY file select none"
  self_ok "$(bash scripts/select.sh --files 'internal/db/migrations/0999_x.sql internal/notify/bot_redraw.go' 2>&1)" T2 "select.sh: a migration selects T2, whatever else the diff holds"
  self_ok "$(bash scripts/select.sh --files 'internal/zz_selftest/x.go internal/notify/bot_redraw.go' 2>&1)" all "select.sh: a product file no row covers selects the whole suite (fail-safe)"
  self_ok "$(bash scripts/select.sh --files 'internal/httpapi/httpapi.go' 2>&1)" all "select.sh: …and so does a CORE file"
  self_ok "$(bash scripts/select.sh --files 'internal/notify/bot_redraw.go scripts/e2e.d/85-bot.sh' 2>&1)" 85-bot "select.sh: a covered file and its own drop-in select exactly that bundle"
  # Run 9, lane `pool`: step 20's notify count is the RUN's, never the database's. A planted listing holds
  # two foreign notify rows (another account's clips) and one of the run's; the filter must count the
  # run's one, and zero when the run's clip has none — where the old `admin stats` total would say 2 or 3.
  printf '%s\n' \
    '#41     clip=FoReIgN0001a kind=notify    state=done      stage=           attempts=0 transient=0 - run_after=2026-10-08T04:00:00Z started=- finished=- checkpoint=false error=""' \
    '#42     clip=RuNcLiP0001a kind=notify    state=queued    stage=           attempts=0 transient=0 queued_for=1s run_after=2026-10-08T04:00:00Z started=- finished=- checkpoint=false error=""' \
    '#43     clip=FoReIgN0002b kind=notify    state=failed    stage=           attempts=3 transient=0 - run_after=2026-10-08T04:00:00Z started=- finished=- checkpoint=false error="x clip=RuNcLiP0002b"' \
    '3 job(s) (kind=notify)' \
    'totals:  notify     queued=1 running=0 done=1 failed=1 cancelled=0' > "$SELF_OUT"
  self_ok "$(e2e_notify_scoped_ "$SELF_OUT" RuNcLiP0001a RuNcLiP0002b)" 1 "step 20's notify count: the run's clip's row is counted, two foreign rows are not (the old global count read 3)"
  self_ok "$(e2e_notify_scoped_ "$SELF_OUT" RuNcLiP0002b)" 0 "…a clip id quoted inside another row's error is not that row's clip"
  self_ok "$(e2e_notify_scoped_ "$SELF_OUT")" 0 "…and no clips counts nothing"
  # …and the core is in the pool: the old core steps 7-30 are parallel bundles that declare what they
  # cover (the registry checks above read them), and E2E_OVERLAP is refused rather than ignored.
  for SELF_S in 7-clip 15-ttl 18-capture 20-pages 21-trim 22-gif; do
    self_ok "$(dropin_phase_ "$E2E_DROPIN_DIR/$SELF_S.sh" 2>/dev/null || echo missing)" parallel "the old core's $SELF_S runs in the pool"
  done
  # (A nested self-test, so that a refusal someone removed cannot start a run: the child would run the
  # self-test instead — without this check, so it cannot recurse — and print no refusal.)
  if [ -z "${E2E_SELFTEST_NESTED:-}" ]; then
    SELF_RC=0; ( E2E_OVERLAP=1 E2E_SELFTEST=1 E2E_SELFTEST_NESTED=1 bash "$0" ) > "$SELF_OUT" 2>&1 || SELF_RC=$?
    self_ok "$SELF_RC:$(grep -c 'E2E_OVERLAP is gone' "$SELF_OUT" || true)" 2:1 "E2E_OVERLAP=1 is refused with a reason, before anything runs"
  fi

  rm -f "$SELF_CT" "$SELF_OUT" "$SELF_ERR"
  if [ "$SELF_FAIL" = 0 ]; then
    echo "[selftest] the retry wrappers are correct (dc on 125; curl_ on 7/28/35; http_refused on an EDGE 502/520-524 or 7/28/35 and never on loopback, and nothing else; all five carry X-Clippet-App: 1), the sweep's selector and id tripwire hold, and the scripts' destructive edges (§20 erratum 290: the hand-back, the probe sweep, E2E_CLEAN's keep-list, the resume key's .env fold, the rm --prefix guard, lane-teardown --check and its removal, visual.sh's tidy-up gate, testclips' regeneration) are guarded, no credential is printed whole, every compose probe starts blanked, and the pool retries a CLASSIFIED transient exactly once on a fresh account namespace (scripts/lib/transient.sh's table; never an app's own answer; E2E_RETRY=0 off)"
    # Its OWN terminal line, and never `E2E PASS` (§20 erratum 275): a caller that greps for the
    # suite's pass line must not be able to read a self-test — which touches no stack at all — as a
    # run of the suite. The exit status is 0 either way, which is precisely why the words differ.
    echo "E2E SELFTEST PASS"
    exit 0
  fi
  echo "[selftest] FAILED" >&2; exit 1
fi

DISCORD_UA='Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)'
TWITTER_UA='Twitterbot/1.0'
# SECURE=1 turns on the https-only assertions (§16.3; §20 erratum 56): Secure cookies, og:video:secure_url,
# every card URL https + publicly resolvable to both bots, no mixed content. An http BASE runs exactly as before.
case $BASE in https://*) SECURE=1;; *) SECURE=0;; esac
# SITE_HOST is the line Discord draws above a card's title and the oEmbed document's
# provider_name: this instance's own public HOST, with no scheme and no port (§20 erratum 287,
# superseding 241's "the wordmark's spelling" and 285's "clippet dev"). It is DERIVED from BASE
# rather than written down, because it differs by origin — `clippet.gg` on the reference stack,
# `localhost` on a lane stack — and the rule is that an origin-dependent assertion
# asserts what the stack reports. The bracketed branch is what makes this the SAME answer as the
# app's own urls.Builder.SiteName (Go's url.Hostname): an IPv6 literal is written `[::1]:8080` in a
# URL and the app strips the brackets, so a plain `%%:*` here would leave the assertions comparing
# the app's `::1` against a lone `[`.
SITE_HOST=${BASE#*://}; SITE_HOST=${SITE_HOST%%/*}
case $SITE_HOST in
  \[*\]*) SITE_HOST=${SITE_HOST#\[}; SITE_HOST=${SITE_HOST%%\]*};;
  *)      SITE_HOST=${SITE_HOST%%:*};;
esac
[ "$ENV_BASE" = "$BASE" ] || \
  echo "[e2e] NOTE: BASE=$BASE differs from the app's PUBLIC_BASE_URL=$ENV_BASE — the app bakes its own value into every
      card tag, so step 10 will fail unless they match (fix .env or the environment, then \`docker compose up -d\`, or drop BASE=)"

# ======================================================================================================================
# THE STORE: what is in it (prefix_count, prefix_names) and what this suite PUTS in it (store_obj)
# §20 erratum 281 — the read side is (c), the write side is (f), and since erratum 346 both are `admin objects`.
# ======================================================================================================================

# store_obj <admin objects arguments…> — the ONE way this suite WRITES to the stack's store: `clippet admin
# objects` through `dc exec` in the stack's own app container (§20 erratum 346).
#
# The write-side half of erratum 281, and since 346 it speaks the product's own verb. Until then every plant
# and every probe-prefix tidy-up ran `mc` in a one-off `bucket-init` container — a third-party client whose
# upstream left, pinned by digest beside the S3 server, with the stack's credentials copied out of `.env`
# into its environment. Now the binary under test does it, with S3_* exactly as the app reads them, so the
# writes land in whichever store THIS stack uses (the bundled S3 server, R2, B2, S3) and no credential
# crosses a command line or a second container at all. The forms, each `admin objects …`:
#
#   store_obj init                      create the bucket if it is missing (step 1, before the readiness wait)
#   printf … | store_obj put <key>      store stdin under <key>
#   store_obj rm <key>                  remove one key
#   store_obj rm --prefix <p>           remove a probe's whole prefix; the PRODUCT refuses anything but the
#                                       harness's e2e-<word>-<epoch>/ shape and the stack's own key space —
#                                       store_rm_prefix_ok_ below still asks first, so a trap's refusal is a
#                                       printed line and not a failed exec
#   store_obj --prefix <p> --count      count under <p>
#
# THE KEYS ARE FROM THE BUCKET'S ROOT, and that is the whole difference between the two places this suite
# writes:
#
#   "${SM_PREFIX}<key>"   the stack's OWN key space. A key written so is byte-for-byte the key the app would
#                         write for the same thing, which is why `admin orphans` and prefix_count can see it
#                         (51-admin's plants under clip C).
#   "<probe prefix>"      the bucket ROOT. A probe container is given `-e S3_KEY_PREFIX=<its own>/`, and that
#                         REPLACES compose's value rather than nesting under it, so the probe's objects sit
#                         BESIDE the stack's key space and not inside it (73-ops, 89-quota and 90-media each
#                         say so where they stand).
#
# $SM_PREFIX is that key prefix alone, NORMALIZED the way internal/config normalizes it — leading slashes off,
# exactly one trailing slash, empty stays empty. The store joins nothing: the app concatenates prefix and key,
# so `S3_KEY_PREFIX=foo` (legal, and what a person types) is `foo/` to the app and would be `fooclips/…` to a
# harness that pasted the raw value out of `.env`.
#
# stdout and stdin pass STRAIGHT THROUGH (a caller pipes bytes into `put` and reads a `--count`); stderr is
# captured and shown only when the command failed, and then store_obj dies — a caller that must never fail a
# run (an EXIT trap) calls it inside a subshell, `( store_obj … ) || echo "[e2e]   NOTE: …"`.
#
# On STORAGE_BACKEND=fs the verb works on the directory too, but every caller asks `store_has_bucket` FIRST
# and stands down with a printed line, as it always has: what each one plants or tidies is a bucket's
# business (a probe instance at the bucket root), and store_obj keeps refusing to be called otherwise so a
# caller that forgot to ask fails loudly instead of writing into a directory nobody meant.
store_prefix_derive_() {
  local p
  p=$(envfile S3_KEY_PREFIX)
  while [ "${p#/}" != "$p" ]; do p=${p#/}; done
  while [ "${p%/}" != "$p" ]; do p=${p%/}; done
  if [ -n "$p" ]; then p="$p/"; fi
  SM_PREFIX=$p
}
# Derived once, HERE, and never lazily: a caller expands "${SM_PREFIX}…" when it builds its arguments — an
# EXIT trap armed before the first plant does exactly that — so it has to be a whole global from the moment
# the suite starts. It reads `.env` and nothing else.
store_prefix_derive_

# store_has_bucket → true when this stack keeps its media in a bucket. Asked of the CONTAINER that is
# running and not of `.env`, because on the one-container flavour `STORAGE_BACKEND: fs` comes from
# compose.fs.yaml's override and `.env` says nothing about it. Cached: the answer cannot change inside a
# run. Use it in a condition (`if store_has_bucket; then …`) — it returns false, it never dies.
SM_BACKEND=
store_has_bucket() {
  if [ -z "$SM_BACKEND" ]; then
    SM_BACKEND=$(dc exec -T app printenv STORAGE_BACKEND 2>/dev/null | tr -d ' \r' | head -1) || SM_BACKEND=
    SM_BACKEND=${SM_BACKEND:-s3}
  fi
  [ "$SM_BACKEND" = s3 ]
}

store_obj() {
  # $BASHPID (bash 3.2 on a Mac has none: then the PPID of an exec'd sh, the same process) and not a fixed
  # name: a bundle that runs in a LANE runs in a subshell, and two of them sharing one scratch file would
  # each read the other's errors. Removed when the command succeeded and KEPT when it did not (nothing in
  # .e2e/ is swept by default, because a failed run's files are the post-mortem).
  local rc=0 err="$E2E_DIR/store-obj-${BASHPID:-$(exec sh -c 'echo $PPID')}.err" any=
  # --any-backend: the caller knows the verb is right on a directory too (the probe-prefix sweep below).
  if [ "${1:-}" = --any-backend ]; then any=1; shift; fi
  if [ "${1:-}" != init ] && [ -z "$any" ] && ! store_has_bucket; then
    die "store_obj: this stack keeps its media in a directory (STORAGE_BACKEND=$SM_BACKEND), not a bucket — a caller must stand down on store_has_bucket before it writes: admin objects $*"
  fi
  dc exec -T app clippet admin objects "$@" 2>"$err" || rc=$?
  if [ "$rc" != 0 ]; then
    die "store_obj failed (exit $rc) on: admin objects $*
$(cat "$err")"
  fi
  rm -f "$err"
}

# ---- a probe container's prefix, emptied AFTER the probe has exited (run 22, lane nits22) ------------
# A probe instance (`dc run … -e S3_KEY_PREFIX=e2e-<word>-<epoch>/ … serve`) tidies its own clips before it
# exits, and its own `admin stats` then reads `objects=0` — but the instance keeps writing after that: its
# database snapshot (§20 erratum 343) lands under `<prefix>_clippet/` as the process goes down, so every run
# left a few objects per probe at the bucket ROOT (51-admin, 73-ops, 89-quota and 90-media: twelve). The
# host is the only side that sees the prefix after the container is gone, so the host empties it:
#
#   probe_prefix_tidy_ <prefix> <what>    best effort and never the cause of a failure — the EXIT trap's form
#   probe_prefix_swept_ <prefix> <what>   the same removal, then `--count` must read 0 — the bundle's own
#                                         teardown, once the probe container has exited; the assertion is the
#                                         regression pin
#
# On STORAGE_BACKEND=fs both run the same verb against the stack's media folder: since DESIGN §20
# erratum 348 a probe on a folder stack writes its snapshot under `<prefix>_clippet/` in that shared
# folder, which the probe's own tidy never sees either (`admin objects` works on a directory, erratum 346).
probe_prefix_tidy_() {
  store_rm_prefix_ok_ "${1:-}" || return 0
  ( store_obj --any-backend rm --prefix "$1" >/dev/null ) 2>/dev/null \
    || echo "[e2e]   NOTE: could not remove $2's prefix $1 (outside the stack's key prefix)"
}
probe_prefix_swept_() {
  local n
  store_rm_prefix_ok_ "${1:-}" || die "$2's prefix '${1:-}' is not the harness's e2e-<word>-<epoch>/ shape"
  n=$(store_obj --any-backend --prefix "$1" --count | tr -d ' \r')
  store_obj --any-backend rm --prefix "$1" >/dev/null
  echo "[e2e]   $2: $n object(s) under $1 after the probe exited, removed"
  n=$(store_obj --any-backend --prefix "$1" --count | tr -d ' \r')
  expect "$n" 0 "$2's prefix $1 is empty once the probe has exited (its database snapshot, erratum 343, included)"
}

# prefix_count <id> → the number of objects under clips/<id>/ IN THE STACK'S OWN STORE (§20 erratum 281).
#
# It asks the app — `clippet admin objects <id> --count` — and that indirection IS the fix. This used to
# run `mc ls` in a one-off `bucket-init` container against the bundled MinIO, which is this stack's store
# only while the stack keeps its media there. clippet.gg moved to R2 on 2026-09-14, the compose MinIO went
# empty, and every prefix assertion in the suite then either failed (step 19, the SEVENTH GATE) or — worse
# — PASSED FOR THE WRONG REASON: steps 12, 14 and 15 each read "empty prefix" off a bucket nothing had ever
# written to. There is deliberately no fallback to mc: the old helper was not wrong until the day the
# store moved, it was right for as long as the store stayed where it was hard-coded to look.
# The verb goes through store.Open and S3_KEY_PREFIX, so the answer is right on MinIO, R2, B2, S3 and on a
# plain directory (STORAGE_BACKEND=fs) alike, and MEDIA_DELIVERY cannot affect it: where the bytes live is
# a different question from how a viewer is handed them.
#
# `exec`, not `run`: the app container is already up, so this costs no container per call — and these are
# POLLED, by expect_prefix_count and by wait_gone — and the verb opens no database, so the second process
# cannot contend with the one serving the run. `dc run`'s cookie-name override (see dc) is for probes that
# boot `serve`; this boots nothing.
prefix_count() {
  local out
  out=$(dc exec -T app clippet admin objects "$1" --count) || die "admin objects $1 --count failed: $out"
  out=$(printf '%s' "$out" | tr -d ' \r')
  case $out in
    ''|*[!0-9]*) die "admin objects $1 --count answered '$out', which is not a count" ;;
  esac
  printf '%s' "$out"
}

# expect_prefix_count <id> <want> <what> — prefix_count, POLLED to the wanted count for up to 12s.
# The bucket and the database are not one transaction and the database is what the polls above watch:
# a superseded object is deleted AFTER the row that stopped referencing it is committed (best effort,
# by design — a delete that ran first and then failed to commit would have destroyed a live object).
# A loop that exits on a DB observable can therefore look at the bucket inside that window and see one
# object too many.
#
# It was a fixed `sleep 3` and one re-read (§20 erratum 245): that spent three seconds on every miss
# whatever the window really was, and had exactly one attempt to spare on a loaded box. A poll pays the
# window and not a constant — the listing is one `docker compose exec` (erratum 281), so it is polled every
# second (it was two while each listing was a container of its own; lane `speed2`) —
# and a count that is still wrong at the deadline fails with the same message it always did.
expect_prefix_count() {
  local got t0=$SECONDS
  got=$(prefix_count "$1")
  if [ "$got" != "$2" ]; then
    echo "[e2e]   $3: bucket says $got, want $2 — polling (the object delete follows the commit)"
    while [ "$got" != "$2" ] && [ $((SECONDS - t0)) -lt 12 ]; do
      sleep 1   # was 2 (lane speed2): the listing is an `exec`, not a container, since erratum 281
      got=$(prefix_count "$1")
    done
  fi
  expect "$got" "$2" "$3"
}

# upload <label> <clipfile> <metadata-json> → status; body in $RESP. Part 1 is `metadata`, part 2 is `video` (§8.3).
# The jar is the run's owner ($JARW) unless the caller names another in UPLOAD_JARW — a bundle that
# uploads as an account of its own (`bundle_account` below) sets it for the uploads that must be that
# account's, and leaves it empty everywhere else.
upload() {
  local label=$1 clip=$2 mf="$E2E_DIR/meta-$1.json"
  printf '%s' "$3" > "$mf"
  curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "${UPLOAD_JARW:-$JARW}" \
    -F "metadata=<$(W "$mf");type=application/json" \
    -F duplicate=allow -F "video=@$(W "$PWD/testclips/$clip");type=video/mp4" "$BASE/api/clips"
}

# card_check linkout <html> <base> <id> <title> [encoding|failed]
# card_check player  <html> <base> <id> <title> <poster_w> <poster_h> <video_w> <video_h> <secure 0|1>
# Parses every <meta> with html.parser and asserts the §9.1 inventory (presence AND absence).
# A link-out card's last argument is the clip's STATE, which decides two of its values — the
# description and the alt sentence its placeholder carries (§20 erratum 294). It defaults to
# `encoding`, which is what a clip on its way to a card is.
# The site line comes in through the environment as SITE_HOST (§20 erratum 287): it is the
# instance's own public host, so it is derived from BASE once and not spelled here.
card_check() {
  SITE_HOST="$SITE_HOST" $PY - "$1" "$(W "$2")" "$3" "$4" "$5" "${6:-}" "${7:-}" "${8:-}" "${9:-}" "${10:-}" <<'PYEOF'
import html.parser, os, re, sys
mode, path, base, cid, title = sys.argv[1:6]
extra = sys.argv[6:]
site = os.environ['SITE_HOST']
class P(html.parser.HTMLParser):
    def __init__(self):
        super().__init__(); self.meta = {}; self.attr = {}; self.title = ''; self.in_title = False
    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == 'meta':
            key = a.get('property') or a.get('name')
            if key and key not in self.meta:
                self.meta[key] = a.get('content', '')
                self.attr[key] = 'property' if a.get('property') else 'name'
        elif tag == 'title':
            self.in_title = True
    def handle_endtag(self, tag):
        if tag == 'title':
            self.in_title = False
    def handle_data(self, data):
        if self.in_title:
            self.title += data
with open(path, encoding='utf-8') as fh:
    raw = fh.read()
p = P(); p.feed(raw); m = p.meta
fails = []
card = []                                  # the §9.1 subset, bound for real in both branches below
def want(key, value=None):
    if key not in m:
        fails.append('missing %s' % key)
    elif value is not None and m[key] != value:
        fails.append('%s = %r, want %r' % (key, m[key], value))
def absent(prefix):
    for k in m:
        if k.startswith(prefix):
            fails.append('unexpected %s = %r' % (k, m[k]))
    if prefix in raw:
        fails.append('substring %r present in the HTML' % prefix)
if mode == 'linkout':
    # §9.1's link-out set, EXACTLY: sixteen tags in every state and on either scheme (fourteen
    # before §20 erratum 294 added the two alt tags). It was the one card in the suite asserted by
    # presence alone, which is how a set stops being a set; the ready card has been counted since
    # the nit sweep and this now reads the same way.
    state = (extra[0] or 'encoding') if extra else 'encoding'
    if state not in ('encoding', 'failed'):
        fails.append('card_check linkout: unknown state %r (want encoding|failed)' % state)
    failed = state == 'failed'
    # The two values the state decides. The ellipsis is written as an ESCAPE rather than as the
    # character, so an assertion's value cannot depend on how a `python -` decoded its own source;
    # internal/web/meta.go's statusProcessing/statusFailed and placeholderAlt are the originals.
    desc = 'Processing failed' if failed else 'Processing\u2026'
    alt = ('The Clippet wordmark: this clip could not be encoded' if failed
           else 'The Clippet wordmark: this clip is still encoding')
    for k, v in (('og:title', title), ('og:description', desc), ('og:type', 'website'),
                 ('og:url', base + '/' + cid), ('og:site_name', site),
                 ('og:image:type', 'image/png'), ('og:image:width', '1200'), ('og:image:height', '630'),
                 ('og:image:alt', alt),
                 ('twitter:card', 'summary_large_image'), ('twitter:title', title),
                 ('twitter:description', desc), ('twitter:image:alt', alt), ('description', desc)):
        want(k, v)
    # The picture is the committed placeholder on THIS origin, both times, with whatever cache-busting
    # query the asset version adds — so it is matched by prefix and never by the whole string.
    placeholder = base + '/static/card-placeholder.png'
    for k in ('og:image', 'twitter:image'):
        want(k)
        if k in m and not m[k].startswith(placeholder):
            fails.append('%s = %r is not %r' % (k, m[k], placeholder))
    absent('og:video'); absent('twitter:player')
    card = sorted(k for k in m if k.startswith(('og:', 'twitter:', 'video:')) or k == 'description')
    if len(card) != 16:
        fails.append('%d card meta tags, want exactly 16 (the link-out set is exact too): %s'
                     % (len(card), ', '.join(card)))
else:
    pw, ph, vw, vh, secure = extra
    social = base + '/api/clips/' + cid + '/social.mp4'
    expect = {
        'og:title': title, 'og:type': 'video.other', 'og:url': base + '/' + cid, 'og:site_name': site,
        'og:image': base + '/api/clips/' + cid + '/poster.jpg', 'og:image:width': pw, 'og:image:height': ph,
        'og:image:type': 'image/jpeg',
        'og:video': social, 'og:video:url': social, 'og:video:type': 'video/mp4',
        'og:video:width': vw, 'og:video:height': vh,
        'twitter:card': 'player', 'twitter:title': title, 'twitter:player': base + '/embed/' + cid,
        'twitter:player:width': vw, 'twitter:player:height': vh, 'twitter:player:stream': social,
        'twitter:player:stream:content_type': 'video/mp4',
        'twitter:image': base + '/api/clips/' + cid + '/poster.jpg',
    }
    for k, v in expect.items():
        want(k, v)
    want('og:description')
    if m.get('og:description', '') == '':
        fails.append('og:description is blank')
    # `{m:ss} · …` since §20 erratum 287: the length leads the card's description, because it is
    # the one fact a reader wants before clicking and the one Discord renders nothing for from
    # `video:duration`. A ready row always has a duration, so the leading timecode is not
    # optional — and the budget that keeps the quality clause alive is pinned in Go.
    elif not re.match(r'^(?:[0-9]+:)?[0-9]+:[0-9]{2}(?: · |$)', m['og:description']):
        fails.append('og:description = %r does not begin with the clip\'s length' % m['og:description'])
    # The three tags that carry the SAME sentence as og:description (meta.go's one
    # metaDescription): asserted equal to it rather than re-deriving the rules a
    # description follows (nit sweep S21).
    for k in ('twitter:description', 'description'):
        want(k)
        if k in m and m[k] != m.get('og:description'):
            fails.append('%s = %r, want og:description %r' % (k, m[k], m.get('og:description')))
    # The two per-clip tags the map above cannot hold, asserted for EVERY player card rather than for
    # the one clip a drop-in looks at. `imageAlt` collapses runs of whitespace in the title and has its
    # own wording for a title-less clip (internal/web/meta.go), so both spellings are built here.
    alt_title = ' '.join(title.split())
    want('og:image:alt', ('A still frame from the clip: ' + alt_title) if alt_title else 'A still frame from a clip')
    want('twitter:image:alt', ('A still frame from the clip: ' + alt_title) if alt_title else 'A still frame from a clip')  # §20 erratum 177: X's spelling of the same sentence
    # Whole seconds: never milliseconds, never a float. A ready row always carries a duration, and a
    # row without one emits no tag at all (meta.go `durationSeconds`), so an absent tag on a ready
    # card is a real gap and is reported as one.
    if not re.fullmatch(r'[0-9]+', m.get('video:duration', '')):
        fails.append('video:duration = %r, want whole seconds' % m.get('video:duration'))
    if secure == '1':
        want('og:video:secure_url', social)
        # On an https origin EVERY URL-valued tag must be https on that origin — a card with one
        # http URL is a mixed-content card, and X drops the player entirely.
        for k, v in sorted(m.items()):
            if not v.startswith('http'):
                continue
            if not v.startswith('https://'):
                fails.append('%s = %r is not https on an https base' % (k, v))
            elif not v.startswith(base + '/'):
                fails.append('%s = %r is not on the public origin %r' % (k, v, base))
    elif 'og:video:secure_url' in m:
        fails.append('og:video:secure_url emitted on a plain-http base (%r)' % m['og:video:secure_url'])
    for k in ('og:url', 'og:image', 'og:video', 'twitter:player', 'twitter:player:stream', 'og:video:secure_url'):
        if k in m and not m[k].startswith(base):
            fails.append('%s = %r does not start with %r' % (k, m[k], base))
    for k, suffix in (('og:video', '/api/clips/%s/social.mp4' % cid), ('twitter:player:stream', '/api/clips/%s/social.mp4' % cid),
                      ('twitter:player', '/embed/%s' % cid)):
        if k in m and not m[k].endswith(suffix):
            fails.append('%s = %r does not end with %r' % (k, m[k], suffix))
    for k in m:
        if k.startswith('og:') and p.attr[k] != 'property':
            fails.append('%s uses name= instead of property=' % k)
        if k.startswith('twitter:') and p.attr[k] != 'name':
            fails.append('%s uses property= instead of name=' % k)
    # And the INVENTORY is exact, which is what makes this the whole of §9.1 rather
    # than a list somebody remembered to extend: internal/web/meta.go's readyMeta
    # emits 27 tags on an http origin and 28 on https (erratum 177), and
    # internal/web/meta_test.go pins the same two numbers. A tag added or lost
    # without moving BOTH now fails the suite and not only the Go golden (S21).
    # The count is over the CARD's tags only: the page also carries viewport,
    # robots and theme-color, which are the template's and not §9.1's.
    card = sorted(k for k in m if k.startswith(('og:', 'twitter:', 'video:')) or k == 'description')
    ready_tags = 28 if secure == '1' else 27
    if len(card) != ready_tags:
        fails.append('%d card meta tags, want exactly %d (§9.1 is an exact set): %s'
                     % (len(card), ready_tags, ', '.join(card)))
if title not in p.title:
    fails.append('<title> %r does not contain %r' % (p.title, title))
if fails:
    sys.stderr.write('card check (%s) failed:\n  ' % mode + '\n  '.join(fails) + '\n')
    sys.exit(1)
# The number the ASSERTION is on comes first: `card` is the §9.1 set BOTH branches now count exactly,
# `m` is every meta tag on the page (viewport, robots and theme-color are the template's). Printing only
# the second made the success line and the failure line count different things.
print('card check (%s): %d card meta tags ok (%d meta tags on the page)' % (mode, len(card), len(m)))
PYEOF
}

# card_urls <html> <base> → every URL a crawler or a player is asked to fetch, absolutised against <base>,
# one per line, deduped in document order: the URL-valued §9.1 meta tags (og:url, og:image,
# og:video[:secure_url], twitter:player[:stream], twitter:image), <link rel=canonical> and the <video>
# src/poster — the last two are ROOT-RELATIVE in the body (only the card tags must be absolute, §9.1), so
# urljoin is what brings `video.mp4` into the sweep. `data-view-url` is deliberately skipped: it is POST-only.
# Step 10 fetches each one back from the public origin — the brief's "tag URLs publicly resolvable".
card_urls() {
  $PY - "$(W "$1")" "$2" <<'PYEOF'
import html.parser, sys
from urllib.parse import urljoin
KEYS = ('og:url', 'og:image', 'og:video', 'og:video:secure_url',
        'twitter:player', 'twitter:player:stream', 'twitter:image')
path, base = sys.argv[1], sys.argv[2].rstrip('/') + '/'
class P(html.parser.HTMLParser):
    def __init__(self):
        super().__init__(); self.urls = []
    def add(self, v):
        if not v:
            return
        u = urljoin(base, v)
        if u.startswith('http') and u not in self.urls:
            self.urls.append(u)
    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == 'meta' and (a.get('property') or a.get('name')) in KEYS:
            self.add(a.get('content'))
        elif tag == 'link' and a.get('rel') == 'canonical':
            self.add(a.get('href'))
        elif tag == 'video':
            self.add(a.get('src')); self.add(a.get('poster'))
p = P()
with open(path, encoding='utf-8') as fh:
    p.feed(fh.read())
print('\n'.join(p.urls))
PYEOF
}

# probe_check <probe.json> <mp4> <audio 0|1> <dto_w> <dto_h> [<r_frame_rate>] [<box WxH>] [<channels>]
#   host ffprobe output + a top-level box walk.
#
# The BOX is now an explicit, optional argument, and it is empty by default:
# since the quality tiers (§20 errata 200-206) there is no instance-wide frame
# ceiling to check against — a Full is encoded at the SOURCE's own size, and a
# playable original is served at it. The `<dto_w> <dto_h>` comparison is the
# assertion that matters (the file is exactly what the API says it is); pass a
# box only where a tier really has one, which is the LIGHT copy's 1280x720.
#
# CHANNELS is optional for the same reason: `-ac 2` is what an ENCODE does, and a
# playable original is served with the audio it was uploaded with — a mono phone
# capture stays mono, which every browser plays. Pass 2 where a tier really was
# encoded (step 9's E, whose two source tracks are merged into one stereo one).
probe_check() {
  $PY - "$(W "$1")" "$(W "$2")" "$3" "$4" "$5" "${6:-}" "${7:-}" "${8:-}" <<'PYEOF'
import json, sys
probe, mp4, audio, w, h, fps, box, channels = sys.argv[1:9]
with open(probe, encoding='utf-8') as fh:
    doc = json.load(fh)
streams = doc.get('streams', [])
video = [s for s in streams if s.get('codec_type') == 'video']
aud = [s for s in streams if s.get('codec_type') == 'audio']
fails = []
if len(video) != 1 or video[0].get('codec_name') != 'h264':
    fails.append('video streams: %s' % [(s.get('codec_name')) for s in video])
if len(aud) != int(audio):
    fails.append('audio streams: %d, want %s' % (len(aud), audio))
for s in aud:
    ch = int(s.get('channels', 0))
    if s.get('codec_name') != 'aac' or ch < 1:
        fails.append('audio stream %s/%s ch, want aac with at least one channel' % (s.get('codec_name'), ch))
    if channels and ch != int(channels):
        fails.append('audio stream has %d channels, want %s' % (ch, channels))
if video:
    vw, vh = int(video[0]['width']), int(video[0]['height'])
    if vw % 2 or vh % 2:
        fails.append('odd dims %dx%d' % (vw, vh))
    if box:
        bw, bh = (int(n) for n in box.lower().split('x'))
        if max(vw, vh) > bw or min(vw, vh) > bh:
            fails.append('dims %dx%d exceed the %s box' % (vw, vh, box))
    if (vw, vh) != (int(w), int(h)):
        fails.append('dims %dx%d differ from the DTO %sx%s' % (vw, vh, w, h))
    if fps and video[0].get('r_frame_rate') != fps:
        fails.append('r_frame_rate %s, want %s' % (video[0].get('r_frame_rate'), fps))
    if float(doc.get('format', {}).get('duration', 0)) <= 0:
        fails.append('duration missing')
boxes = []
with open(mp4, 'rb') as fh:
    fh.seek(0, 2); end = fh.tell(); off = 0
    while off < end:
        fh.seek(off); hdr = fh.read(8)
        if len(hdr) < 8:
            break
        size = int.from_bytes(hdr[:4], 'big'); typ = hdr[4:8].decode('latin1')
        if size == 1:
            size = int.from_bytes(fh.read(8), 'big')
        elif size == 0:
            size = end - off
        if size < 8:
            fails.append('bad box size at %d' % off); break
        boxes.append((typ, off)); off += size
types = [t for t, _ in boxes]
if not types or types[0] != 'ftyp':
    fails.append('first box is %r, want ftyp' % (types[:1],))
if 'moov' not in types or 'mdat' not in types:
    fails.append('boxes %s lack moov/mdat' % types)
elif types.index('moov') > types.index('mdat'):
    fails.append('moov after mdat (no faststart): %s' % boxes)
if fails:
    sys.stderr.write('probe check failed for %s:\n  ' % mp4 + '\n  '.join(fails) + '\n')
    sys.exit(1)
print('probe ok: %s %s audio=%s boxes=%s' % (mp4, ('%sx%s' % (w, h)), audio, ','.join(types)))
PYEOF
}

# rms <file> <channel 0|1> <freq> → "RMS level dB" of the band-passed channel (the §16.2 both-ears recipe)
rms() {
  local out="$E2E_DIR/astats-$2-$3.txt"
  ffmpeg -hide_banner -nostdin -i "$(W "$1")" -map 0:a:0 \
    -af "pan=mono|c0=c$2,bandpass=f=$3:width_type=h:w=40,astats=measure_perchannel=RMS_level:measure_overall=none" \
    -f null - > "$out" 2>&1 || die "ffmpeg astats failed: $(tail -3 "$out")"
  sed -n 's/.*RMS level dB: *//p' "$out" | tail -1 | tr -d '\r'
}

# yavg <image> <label> → the mean luma (0–255) of one frame, via ffmpeg's signalstats. The poster is
# the picture Discord and X put in the card (§20 erratum 92), and a black one is a card with no image.
yavg() {
  local out="$E2E_DIR/signalstats-$2.txt"
  ffmpeg -hide_banner -nostdin -i "$(W "$1")" -vf signalstats,metadata=print:file=- \
    -f null - > "$out" 2>&1 || die "ffmpeg signalstats failed: $(tail -3 "$out")"
  sed -n 's/.*YAVG=//p' "$out" | tail -1 | tr -d '\r'
}

# wait_ready <id> <label>: poll progress ≤180s; ready → ok; failed/404 → die; percent monotone, ends at 100
#
# The interval is $POLL_SLEEP and not a literal 1 (§20 erratum 245). GET …/progress carries its own
# limiter — 120 requests a minute per client ADDRESS, `httpapi.pollsPerMinute`, a compile-time constant
# no environment can raise — and every bundle in a parallel phase polls it from the same address. One
# poll a second per bundle would put four lanes at 240/min and earn the 429 that step 66 exists to
# prove, in a step that is not about rate limiting. The scheduler sets POLL_SLEEP from the number of
# pollers that can run at once (dropins_plan_, lane `speed2`), which keeps the whole run's polling at or
# under 1.5 requests a second against a refill of 2 whatever the width; it is 1 for a serial run.
wait_ready() {
  local id=$1 label=$2 last=-1 pct state code t0=$SECONDS
  while :; do
    code=$(get "$BASE/api/clips/$id/progress")
    [ "$code" != 404 ] || die "$label progress → 404 (a poller must never treat 404 as done)"
    expect "$code" 200 "$label progress status"
    state=$(json state); pct=$(json percent)
    [ -n "$pct" ] && [ "$pct" -ge "$last" ] 2>/dev/null || die "$label percent went $last → '$pct' (state=$state)"
    last=$pct
    case $state in
      ready)  expect "$pct" 100 "$label final percent"; echo "[e2e]   $label ready after $((SECONDS - t0))s"; return 0 ;;
      failed) die "$label failed: $(json error)" ;;
    esac
    [ $((SECONDS - t0)) -lt 180 ] || die "$label not ready within 180s (state=$state stage=$(json stage) percent=$pct)"
    sleep "$POLL_SLEEP"
  done
}

# wait_trim <id> <label>: poll GET …/trim to `done` (≤120s). On EVERY poll the clip itself must still
# be `ready` — that is the §5 promise the whole feature rests on — and percent must stay in 0..100.
#
# It lives HERE, with the other waiters, and not inside step 21 where it was written (§20 erratum 245):
# 41-social.sh and 47-upload.sh call it, 41 guards on `command -v wait_trim`, and a filtered run
# (E2E_ONLY=41-social) does not execute step 21 at all — a helper a drop-in depends on may not be a
# side effect of a step that drop-in did not ask for.
wait_trim() {
  local id=$1 label=$2 t0=$SECONDS state pct stage code
  while :; do
    code=$(get "$BASE/api/clips/$id/trim" -b "$JARW")
    expect "$code" 200 "$label trim status"
    state=$(json state); pct=$(json percent); stage=$(json stage)
    [ -n "$pct" ] && [ "$pct" -ge 0 ] && [ "$pct" -le 100 ] 2>/dev/null || die "$label trim percent '$pct'"
    case $state in
      done)   echo "[e2e]   $label trim done after $((SECONDS - t0))s"; return 0 ;;
      failed) die "$label trim failed: $(json error)" ;;
      queued|running) ;;
      *) die "$label trim state '$state' (want queued|running|done|failed)" ;;
    esac
    code=$(get "$BASE/api/clips/$id")
    expect "$code" 200 "$label clip while its trim runs"
    expect "$(json state)" ready "$label clip state while its trim runs (a trim never moves it)"
    [ $((SECONDS - t0)) -lt 120 ] || die "$label trim not done within 120s (state=$state stage=$stage percent=$pct)"
    sleep 1   # was 2 (lane speed2): …/trim and the clip GET carry no limiter, and the deadline is time
  done
}

# social_etag <id> → the current ETag of /social.mp4 (empty when the route does not answer 200).
# wait_social <id> <label> <not-this-etag>: poll /social.mp4 until its ETag differs from the given one
# (the rendition's, or the previous social file's), ≤180s. The clip must stay `ready` on every poll —
# the §5 promise the whole media-loop design rests on.
# prefix_names <id> → the object names under clips/<id>/, one per line: prefix_count's sibling, so a
# superseded object can be shown GONE from the BUCKET rather than merely unreferenced by the row.
#
# All three were written in 41-social.sh and live here since §20 erratum 245. 62-tiers.sh calls
# wait_social and prefix_names, which worked only while every drop-in was sourced into one shell in
# name order; a bundle that runs in a lane runs in a SUBSHELL and its definitions go with it. A helper
# two bundles share belongs to the harness — the same reason wait_trim moved out of step 21.
# `media_head` and not `head_`: in redirect mode the route answers 302 and the ETag a poll compares is
# the OBJECT's, which comes back from the bucket (§20 erratum 280). The accepted status is the mode's
# own — 200 proxied, 206 from the one-byte ranged GET behind the 302. Anything else in `proxy` means
# the route is not answering yet, which is what an empty string tells the caller; in `redirect` the
# helper reports it at once, because a READY clip's `/social.mp4` always answers (it COALESCEs to the
# rendition until the card copy lands, 41-social.sh section 1) and a non-302 there is not a wait.
social_etag() {
  local code
  code=$(media_head "$BASE/api/clips/$1/social.mp4") || true
  case $code in 200|206) ;; *) echo ""; return 0 ;; esac
  hdr ETag
}
wait_social() {
  local id=$1 label=$2 avoid=$3 t0=$SECONDS got code
  while :; do
    got=$(social_etag "$id")
    if [ -n "$got" ] && [ "$got" != "$avoid" ]; then
      echo "[e2e]   $label social rendition after $((SECONDS - t0))s (etag $got)"
      SOCIAL_ETAG=$got
      return 0
    fi
    code=$(get "$BASE/api/clips/$id"); expect "$code" 200 "$label clip while its social encode runs"
    expect "$(json state)" ready "$label clip state while its social encode runs (it never moves it)"
    [ $((SECONDS - t0)) -lt 180 ] || die "$label has no social rendition within 180s (etag still '$got')"
    sleep 1   # was 2 (lane speed2): a ranged HEAD of social.mp4 and a clip GET, neither limited
  done
}
# prefix_names asks the same verb as prefix_count and for the same reason (§20 erratum 281): the KEY column
# of `clippet admin objects <id>`, whose listing is `<key> <size> <modified>` per object and then a
# `<n> object(s)` line. The count line is what the parse is checked against — a listing whose key lines and
# whose own count disagree is output this helper does not understand, and it dies rather than hand a caller
# a short list that `lacks_file` would read as proof.
prefix_names() {
  local out n keys nk
  out=$(dc exec -T app clippet admin objects "$1") || die "admin objects $1 failed: $out"
  out=$(printf '%s' "$out" | tr -d '\r')
  n=$(printf '%s\n' "$out" | tail -n 1 | sed -n 's/^\([0-9][0-9]*\) object(s)$/\1/p')
  [ -n "$n" ] || die "admin objects $1 did not end in a count line: $out"
  keys=$(printf '%s\n' "$out" | sed '$d' | awk 'NF == 3 { print $1 }')
  nk=$(printf '%s\n' "$out" | sed '$d' | awk 'NF == 3 { c++ } END { print c+0 }')
  [ "$nk" = "$n" ] || die "admin objects $1 says $n object(s) but $nk of its lines parse as one: $out"
  [ "$n" = 0 ] || printf '%s\n' "$keys"
}

# wait_gone <id> <label>: API 404 AND share page 404 AND empty bucket prefix within 150s — ≈2 sweep
# ticks plus margin at the SHIPPED 60s interval, and ten of them at the 15s one a run configures
# (§20 erratum 245), which is why the budget did not have to move when the interval did.
# expect_deleted <code> <id> <what> — a clip DELETE's honest answers (API.md: `DELETE /api/clips/{id}`).
# 204 is the usual one: the row and every object are gone. 202 `{"state":"deleting"}` is the DOCUMENTED
# rare one: the row is gone from that instant (the API answers 404 for it at once) and the storage step
# is deferred to the sweeper, which finishes the object cleanup on its next tick — the origin logs
# `clip delete deferred to sweeper`. It happens when a job is still writing an object under the prefix
# at the moment of the delete (a clip deleted right after its upload) or a bucket answers slowly; on the
# bundled MinIO the job always wins the race, on R2 it did not (SEVENTH GATE attempt 4, step 83, 2026-09-15).
# So the harness asserts what the stack reports: 204, or 202 AND the immediate 404 the contract promises,
# printed so a reader can see how often the deferred path fires. Anything else dies as before. A step
# that needs the OBJECTS gone (an empty prefix) still calls wait_gone after this, as it always did.
expect_deleted() {
  local code=$1 id=$2 what=$3 after
  case $code in
    204) ;;
    202)
      after=$(get "$BASE/api/clips/$id")
      [ "$after" = 404 ] || die "$what: 202 (deferred) but GET /api/clips/$id answered $after, want 404 at once"
      echo "[e2e]   $what: 202 — the storage step was deferred to the sweeper (a job was still writing, or the bucket was slow); the row is gone (404)"
      ;;
    *) die "$what: got '$code', want 204 (or the documented 202)" ;;
  esac
}

wait_gone() {
  local id=$1 label=$2 t0=$SECONDS api pg n=
  while :; do
    api=$(get "$BASE/api/clips/$id"); pg=$(page "$BASE/$id")
    if [ "$api" = 404 ] && [ "$pg" = 404 ]; then
      n=$(prefix_count "$id")
      [ "$n" = 0 ] && { echo "[e2e]   $label gone (404 + empty prefix) after $((SECONDS - t0))s"; return 0; }
    fi
    [ $((SECONDS - t0)) -lt 150 ] || die "$label not gone within 150s (api=$api page=$pg prefix_objects=${n:-?})"
    sleep 1   # was 5 (lane speed2): the clip GET and the page carry no limiter; up to 4 s of dead clock per wait
  done
}

# ======================================================================================================================
# THE DROP-IN SCHEDULER (§20 erratum 245, amended by lane `speed2`; scripts/e2e.d/README.md)
#
# Steps 7–30 and 31–104 live in scripts/e2e.d/*.sh, one file per bundle (7–30 since run 9's lane `pool`). Each file declares, in its first 40
# lines, how it may be RUN:
#
#     # e2e-schedule: exclusive           — it runs ALONE, in name order, after every parallel bundle
#     # e2e-schedule: parallel            — it runs beside the other parallel bundles, in a subshell
#
# A file with NO marker is exclusive, loudly. That is the safe default and the rule a new drop-in
# follows until somebody has shown otherwise: a bundle is parallel-safe only when it owns its clips and
# its users, restarts and recreates nothing, asserts nothing bucket-wide, log-wide or instance-wide,
# changes no account-wide setting, and probes no rate limiter.
#
# A WORK POOL with declared conflicts (lane `speed2`; it replaced four fixed lanes). Two bundles can each
# be safe on their own and still fight over something neither of them is about: the `view` bucket is 60
# requests a minute per client address (a compile-time constant), a filmstrip poll spends one a second
# and a preview-GIF poll one every two, so 70-web and 43-watch would 429 each other if they ever ran
# together. Fixed lanes made that promise by putting both in one lane — and with it made eleven
# unrelated bundles queue behind one another in lane 4, so the phase could never be wider than four
# whatever the machine. The promise is now spelled as what it IS:
#
#     # e2e-schedule-apart: 70-web        — never at the same time as these (symmetric: one side says it)
#
# and the pool runs E2E_LANES bundles at once, LONGEST FIRST from the last run's durations, starting
# each the moment a slot is free and nothing it is apart from is running. `lane=<n>` is refused rather
# than ignored: a branch written against the old syntax would otherwise lose its conflicts in silence.
#
# ORDER, declared rather than assumed. A second marker states that a bundle must run AFTER another:
#
#     # e2e-schedule-after: 37-library,47-upload
#
# The exclusive phase's order has always been load-bearing and nothing enforced it: 47-upload asserts
# the account expresses no TTL preference, which 55-owner's step 57 sets and clears. Name order happens
# to satisfy that, which is a coincidence and not a rule — and a filtered run (`E2E_ONLY=55-owner`)
# silently dropped the precondition and still printed a PASS line. (The other dependency the light pass
# recorded, 55-owner calling `probe_queue_idle` out of 37-library, is gone: it became a harness global, and
# lane `nits8` removed it once lane `excl` had left it with no caller.)
# dropin_check_order_ turns each declaration into a check made BEFORE any bundle
# runs: an order that contradicts it is fatal, and a predecessor a FILTER left out is fatal in a full
# run and a loud NOTE in a filtered one, which is exactly what a filtered run's PARTIAL banner is for.
# In the pool an `after` is honoured by waiting: the bundle is not started until its predecessor has
# finished (passed or failed — a failed predecessor never stopped a lane either).
# dropin_select_ <stem> → 0 when this bundle runs. E2E_ONLY wins, then E2E_SKIP removes from it.
dropin_select_() {
  local stem=$1
  if [ -n "$E2E_ONLY" ]; then
    case ",$E2E_ONLY," in *",$stem,"*) ;; *) return 1 ;; esac
  fi
  if [ -n "$E2E_SKIP" ]; then
    case ",$E2E_SKIP," in *",$stem,"*) return 1 ;; esac
  fi
  return 0
}
# A name that matches nothing is a typo, and a typo in E2E_ONLY runs NOTHING while still ending in a
# PASS line. Both lists are checked against the directory before step 1 does any work.
dropin_check_names() {
  local list name known
  known=$(dropin_stems | tr '\n' ' ')
  for list in "$E2E_ONLY" "$E2E_SKIP"; do
    [ -n "$list" ] || continue
    for name in $(printf '%s' "$list" | tr ',' ' '); do
      case " $known " in
        *" $name "*) ;;
        *) die "E2E_ONLY/E2E_SKIP names '$name', which is not a drop-in. The bundles are: $known" ;;
      esac
    done
  done
}
# e2e_quiesce <what> — wait (≤90s) for the live queue to hold no running and no queued job.
# It is what separates the parallel phase from the exclusive one: the exclusive bundles include a
# census of every clip and every object on the instance (47-upload) and two that boot a SECOND app
# process against this database (37-library, 55-owner), and neither is a statement about anything while
# another bundle's transcode is still landing. It NOTES and continues rather than dying — a busy queue
# is a slower answer, not a wrong one.
# `n` caps the ITERATIONS and $q0 measures the TIME: each turn of the loop is two `docker compose exec`
# round trips plus a sleep, which is well over a second on a loaded box, so the old `(${n}s)` understated
# every wait it reported — sometimes by a factor of three.
e2e_quiesce() {
  local n=0 r q q0=$SECONDS
  # Half-second polls since lane `speed2` (they were whole seconds): the wait ends on the EVENT, the
  # idle queue, and a second's granularity was up to a second of dead clock at every phase boundary.
  # The budget only grew: 180 turns of at least half a second each, plus the two execs per turn.
  while [ "$n" -lt 180 ]; do
    r=$(dc exec -T app clippet admin jobs --state running --limit 1 2>/dev/null | sed -n 's/^\([0-9][0-9]*\) job(s).*/\1/p' | head -1 | tr -d '\r')
    q=$(dc exec -T app clippet admin jobs --state queued --limit 1 2>/dev/null | sed -n 's/^\([0-9][0-9]*\) job(s).*/\1/p' | head -1 | tr -d '\r')
    if [ "${r:-x}" = 0 ] && [ "${q:-x}" = 0 ]; then
      echo "[e2e] the queue is idle before $1 ($((SECONDS - q0))s, $n poll(s))"
      return 0
    fi
    n=$((n + 1)); sleep 0.5
  done
  echo "[e2e] NOTE: the queue still holds running=${r:-?} queued=${q:-?} after $((SECONDS - q0))s ($n polls) before $1"
}
# (probe_queue_count / probe_queue_idle, the second-process window guard 37-library and 55-owner shared,
# were removed in lane `nits8`: since lane `excl` moved both second processes onto instances of their own,
# nothing called them.)
# run_bundle_ <stem> <slot> — ONE parallel bundle, in a subshell with a context and a log of its own.
# The pool starts it in the background and learns it has finished from `pool/<stem>.rc`, which is the
# LAST thing this writes — after the outcome lines, so a reader of the files never sees a finished
# bundle whose verdict is not yet recorded.
#
# The bundle runs with stdin on /dev/null, and that is not tidiness: a bundle is full of commands that
# drain stdin without being asked — `docker compose exec`/`run` attach it by default even with -T, and
# ffmpeg is why `-nostdin` is written everywhere in this suite. The first lane runner read its list on
# the same stdin and ran ONE bundle per lane before the list was swallowed; the pool has no list on any
# descriptor, and the completed list below is still checked against what was scheduled — a scheduler
# that can lose work silently is worse than no scheduler.
# The body is e2e_run_bundle_ (above the self-test, with the retry of run 9's lane `retry`): the
# self-test plants its own run_bundle_ to drive the pool and removes it again, so the pool's runner is
# this one-line name and the work lives where the self-test can reach it.
run_bundle_() { e2e_run_bundle_ "$@"; }
# dropin_sched_ <stem> → "<position><TAB><phase>" for a SELECTED bundle, empty for one that is not.
# Position is the line number in the schedule table, i.e. name order, which is the order the exclusive
# phase runs in. Phase is 0 for exclusive and 1 for the parallel pool.
dropin_sched_() { awk -F'\t' -v s="$1" '$1 == s { print NR "\t" $2; exit }' "$E2E_DIR/dropin-sched.txt"; }
# dropin_check_order_ <dir> — every `# e2e-schedule-after:` and `# e2e-schedule-apart:` declaration,
# checked before any bundle runs.
#
# What "after" can MEAN depends on the phase, because the phases are not one list: every parallel
# bundle finishes before the first exclusive one starts. So:
#   - an exclusive bundle may depend on any parallel bundle (the whole parallel phase finishes first)
#     or on an exclusive bundle EARLIER in name order;
#   - a parallel bundle may depend on another parallel bundle (the pool waits for it), never on an
#     exclusive one, which cannot have run yet.
# Anything else is a dependency the scheduler cannot honour, and it fails here rather than as a wrong
# assertion sixty steps later. An `apart` naming no drop-in is a typo, and fatal for the same reason.
dropin_check_order_() {
  local d=$1 stem lane deps dep known myseq mylane depinfo depseq deplane
  known=" $(dropin_stems | tr '\n' ' ') "
  : > "$E2E_DIR/dropin-apart.txt"
  while IFS="$(printf '\t')" read -r stem lane; do
    [ -n "$stem" ] || continue
    for dep in $(dropin_apart "$d/$stem.sh" | tr ',' ' '); do
      case $known in
        *" $dep "*) ;;
        *) die "$stem declares '# e2e-schedule-apart: $dep' and there is no such drop-in. The bundles are:$known" ;;
      esac
      printf '%s %s\n%s %s\n' "$stem" "$dep" "$dep" "$stem" >> "$E2E_DIR/dropin-apart.txt"
    done
    deps=$(dropin_after "$d/$stem.sh")
    [ -n "$deps" ] || continue
    depinfo=$(dropin_sched_ "$stem"); myseq=${depinfo%%	*}; mylane=${depinfo##*	}
    for dep in $(printf '%s' "$deps" | tr ',' ' '); do
      case $known in
        *" $dep "*) ;;
        *) die "$stem declares '# e2e-schedule-after: $dep' and there is no such drop-in. The bundles are:$known" ;;
      esac
      depinfo=$(dropin_sched_ "$dep")
      if [ -z "$depinfo" ]; then
        if [ -n "$E2E_ONLY" ] || [ -n "$E2E_SKIP" ] || [ -n "$E2E_RESUME" ]; then
          echo "[e2e] NOTE: $stem runs after $dep, and this run is not running $dep (a filter, or a resume crediting it) — its precondition is not set up by THIS run (read a PARTIAL banner as covering less than it looks)"
          continue
        fi
        die "$stem runs after $dep and $dep is not scheduled at all"
      fi
      depseq=${depinfo%%	*}; deplane=${depinfo##*	}
      if [ "$mylane" = 0 ]; then
        if [ "$deplane" = 0 ] && [ "$depseq" -ge "$myseq" ]; then
          die "$stem must run after $dep and both are exclusive, but $dep is LATER in name order — rename one, or the dependency is not expressible in this schedule"
        fi
      else
        [ "$deplane" != 0 ] || die "$stem (parallel) must run after $dep, which is exclusive — the exclusive phase starts only after every parallel bundle has finished, so make $stem exclusive too"
      fi
    done
  done < "$E2E_DIR/dropin-sched.txt"
}
# run_dropins — the whole of steps 31–96: the parallel phase, then the exclusive one, in that order.
# ---- checkpoints, resume, and a bundle that fails without ending the run (2026-09-15) -----------------
# Every bundle that PASSES is recorded under a key naming the PRODUCT tree (internal, cmd, web, go.mod,
# go.sum, Dockerfile, compose.yaml — what the binary and the image are built from; a harness edit does not
# move it), the sha256 of `.env` and of every COMPOSE_FILE member (erratum 290: a changed `MEDIA_DELIVERY`
# line changes what a bundle proved), and the origin under test. `E2E_RESUME=1` then skips the bundles that key already records as
# PASSED and runs the rest, so a harness-only fix after a failure costs the core plus the failed bundle,
# not the whole suite: the SEVENTH GATE's attempts 1–6 each re-ran up to 89 passed steps to reach the one
# that had changed. The setup (steps 1–6) always runs — it makes the fixtures every bundle uses; since run
# 9's lane `pool` the old core steps 7–30 are bundles, recorded and resumed like the rest — and a
# key with local changes under those paths records nothing and resumes nothing (`-dirty`).
# A bundle that FAILS no longer ends the run: it is recorded, its PASS line (if any) is forgotten, every
# other bundle still runs, and the closing banner names each failed bundle and exits 1. The core's own
# steps stay fatal: a fixture that did not get made cannot be worked around.
E2E_RESUME=${E2E_RESUME:-}
# `e2e_product_key_` is defined above the self-test (§20 erratum 290 folded `.env` and the COMPOSE_FILE
# members into it, and the self-test pins that fold).
E2E_KEY=$(e2e_product_key_)
E2E_CKPT="$E2E_DIR/checkpoints/${E2E_KEY}__$(printf '%s' "$BASE" | tr -c 'A-Za-z0-9' '_').txt"
mkdir -p "$E2E_DIR/checkpoints"
ckpt_passed_() { [ -s "$E2E_CKPT" ] && grep -q "^$1	PASS	" "$E2E_CKPT"; }
ckpt_record_() {
  case $E2E_KEY in *-dirty|nogit) return 0 ;; esac
  printf '%s\tPASS\t%s\n' "$1" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$E2E_CKPT"
}
ckpt_forget_() {
  [ -s "$E2E_CKPT" ] || return 0
  grep -v "^$1	PASS	" "$E2E_CKPT" > "$E2E_CKPT.new" || true
  mv "$E2E_CKPT.new" "$E2E_CKPT"
}
# dropins_plan_ — decide the run's schedule ONCE (the selection, the two phases, the order checks, the
# longest-first plan and the poll pacing) and print it. run_dropins calls it, once.
E2E_PLANNED=0
dropins_plan_() {
  local d f stem lane excl="" par="" skipped="" order="" resumed="" pollers=0 width
  [ "$E2E_PLANNED" = 0 ] || return 0
  E2E_PLANNED=1
  d=$E2E_DROPIN_DIR
  # Last run's per-bundle logs go too: the merged transcript prints `bundle-<stem>.log` for every
  # bundle it scheduled, and a stale file would put a PREVIOUS run's output into this run's record.
  # (`bundle-*.log` takes a retried bundle's first-attempt `bundle-<stem>.1.log` too, and retries.txt is
  # this run's alone — run 9, lane `retry`.)
  rm -f "$E2E_DIR"/lane-*.list "$E2E_DIR"/bundle-*.log "$E2E_DIR/card-urls.txt" \
        "$E2E_DIR/failed-bundles.txt" "$E2E_DIR/bundle-times.txt" "$E2E_DIR/ran-bundles.txt" "$E2E_DIR/retries.txt"
  # The schedule as a TABLE (stem, phase), written as it is decided and read back by
  # dropin_check_order_ below. A file rather than parallel shell strings: the check wants to ask
  # "where does X sit" by name, and this is the cheapest way to answer that in POSIX shell.
  : > "$E2E_DIR/dropin-sched.txt"
  for f in "$d"/*.sh; do
    [ -e "$f" ] || continue
    stem=$(basename "$f" .sh)
    if ! dropin_select_ "$stem"; then skipped="$skipped $stem"; continue; fi
    if [ -n "$E2E_RESUME" ] && ckpt_passed_ "$stem"; then resumed="$resumed $stem"; continue; fi
    case $(dropin_phase_ "$f") in
      fatal-lane)
        die "$stem declares '# e2e-schedule: $(dropin_marker "$f")': fixed lanes are gone (lane speed2). Write '# e2e-schedule: parallel' and name what it must never overlap with '# e2e-schedule-apart: <stem>[,...]' (scripts/e2e.d/README.md)" ;;
      parallel) lane=1; [ "$E2E_LANES" -gt 1 ] || lane=0 ;;
      exclusive) lane=0 ;;
      *)
        echo "[e2e] NOTE: $stem carries no '# e2e-schedule:' marker — running it EXCLUSIVE (the safe default; scripts/e2e.d/README.md)"
        lane=0 ;;
    esac
    order="$order $stem"
    printf '%s\t%s\n' "$stem" "$lane" >> "$E2E_DIR/dropin-sched.txt"
    if [ "$lane" = 0 ]; then
      excl="$excl $stem"
    else
      par="$par $stem"
      grep -q 'wait_ready' "$f" && pollers=$((pollers + 1))
    fi
  done
  DROPINS_ORDER=$order; DROPINS_SKIPPED=$skipped; DROPINS_RESUMED=$resumed
  DROPINS_PAR=$par; DROPINS_EXCL=$excl
  [ -z "$skipped" ] || echo "[e2e] bundle filter: NOT running$skipped"
  if [ -n "$E2E_RESUME" ]; then
    if [ -n "$resumed" ]; then
      echo "[e2e] resume: product tree $E2E_KEY at $BASE — skipping the bundle(s) recorded as PASSED in $(basename "$E2E_CKPT"):$resumed"
    else
      echo "[e2e] resume: product tree $E2E_KEY at $BASE — nothing recorded as passed, running everything"
    fi
  fi
  dropin_check_order_ "$d"
  E2E_POOL_ALL=$par
  # shellcheck disable=SC2086
  E2E_POOL_ORDER=$(e2e_lpt_order_ "$E2E_DURATIONS" $par | tr '\n' ' ')
  # The progress poll's pacing (§20 erratum 245, re-derived for a pool). GET …/progress is 120 a minute
  # per client ADDRESS — a compile-time constant — and every bundle polls it from the same address. The
  # most pollers that can ever run at once is the narrower of the width and the number of scheduled
  # bundles that poll at all; pacing each at two-thirds of a
  # second per poller keeps the whole run at or under 1.5 requests a second, against a refill of 2 and a
  # burst of 120 — the same margin the lanes kept, without making a four-lane run wait longer per poll.
  width=$pollers; [ "$width" -le "$E2E_LANES" ] || width=$E2E_LANES
  E2E_POOL_POLL=$(( (width * 2 + 2) / 3 )); [ "$E2E_POOL_POLL" -ge 1 ] || E2E_POOL_POLL=1
  if [ -n "$par" ]; then
    echo "[e2e] ----- the parallel plan: $(printf '%s' "$par" | wc -w | tr -d ' ') bundle(s) on $E2E_LANES slot(s) ($E2E_LANES_FROM), longest first -----"
    printf '[e2e]   order:'
    for stem in $E2E_POOL_ORDER; do printf ' %s(%ss)' "$stem" "$(e2e_dur_of_ "$E2E_DURATIONS" "$stem" | grep . || echo '?')"; done
    printf '\n'
    if [ -s "$E2E_DIR/dropin-apart.txt" ]; then
      echo "[e2e]   apart (never at the same time): $(awk '$1 < $2 { printf "%s|%s ", $1, $2 }' "$E2E_DIR/dropin-apart.txt" | sort -u)"
    fi
    echo "[e2e]   progress polls every ${E2E_POOL_POLL}s while it runs ($pollers polling bundle(s))"
  fi
}
# e2e_parallel_phase_ — the pool, then the merged transcript and the completeness check.
e2e_parallel_phase_() {
  local stem rc=0 pt0
  [ -n "${DROPINS_PAR:-}" ] || return 0
  echo "[e2e] ----- parallel phase: $E2E_LANES slot(s) -----"
  POLL_SLEEP=$E2E_POOL_POLL
  pt0=$SECONDS
  e2e_pool_ || rc=1
  POLL_SLEEP=1
  # The merged transcript: every bundle's own log, in bundle order, so the run reads the same way it
  # always did whatever order the pool actually finished them in.
  # A retried bundle's FIRST attempt comes first, under its own header line (run 9, lane `retry`).
  for stem in $DROPINS_PAR; do
    if [ -s "$E2E_DIR/bundle-$stem.1.log" ]; then
      printf '\n[e2e] ----- %s, the first attempt (a classified transient; retried once — .e2e/retries.txt) -----\n' "$stem"
      cat "$E2E_DIR/bundle-$stem.1.log"
    fi
    [ -s "$E2E_DIR/bundle-$stem.log" ] || continue
    printf '\n'; cat "$E2E_DIR/bundle-$stem.log"
  done
  [ "$rc" = 0 ] || die "the parallel pool itself failed (not a bundle): $(tr '\n' ' ' < "$E2E_DIR/failed-bundles.txt" 2>/dev/null)"
  if [ -s "$E2E_DIR/failed-bundles.txt" ]; then
    echo "[e2e] parallel bundle(s) FAILED: $(tr '\n' ' ' < "$E2E_DIR/failed-bundles.txt") — the run goes on; the closing banner will say so"
  fi
  # EVERY bundle that was scheduled must have finished, passed or failed. A runner that stops early
  # without failing is the one way a green run can cover less than it claims to, and it has happened.
  for stem in $DROPINS_PAR; do
    grep -qx "$stem" "$E2E_DIR/ran-bundles.txt" 2>/dev/null || grep -qx "$stem" "$E2E_DIR/failed-bundles.txt" 2>/dev/null \
      || die "the parallel phase scheduled $stem and it never ran: $(tr '\n' ' ' < "$E2E_DIR/ran-bundles.txt" 2>/dev/null)"
  done
  printf '%s\t%s\t%s\n' "(parallel phase)" "$((SECONDS - pt0))" "$E2E_LANES slots" >> "$E2E_DIR/bundle-times.txt"
  echo "[e2e] ----- parallel phase done in $((SECONDS - pt0))s -----"
  e2e_quiesce "the exclusive phase"
}
run_dropins() {
  local stem bt0 d
  d=$E2E_DROPIN_DIR
  # Close the harness's last step BEFORE anything here runs, or its timer keeps running through the
  # whole parallel phase and the table reports step 6 as the slowest step in the suite.
  step_close_; STEP=0
  dropins_plan_
  e2e_parallel_phase_
  for stem in ${DROPINS_EXCL:-}; do
    e2e_ctx main
    step_close_; STEP=0
    bt0=$SECONDS
    # A subshell, exactly like a pool bundle: a `die` inside ends THIS bundle, not the run. Its output
    # stays inline (it runs alone), and its step timings and summary rows go to the `main` context's
    # sinks as before. Nothing a bundle sets in the shell is read by a later bundle (the README's
    # conventions and check.sh's helper scan keep it so).
    if ( E2E_BUNDLE=$stem; . "$d/$stem.sh"; step_close_ ); then
      printf '%s\n' "$stem" >> "$E2E_DIR/ran-bundles.txt"
      ckpt_record_ "$stem"
    else
      printf '%s\n' "$stem" >> "$E2E_DIR/failed-bundles.txt"
      ckpt_forget_ "$stem"
      echo "[e2e] bundle $stem FAILED (exclusive) after $((SECONDS - bt0))s — the run goes on with the next bundle"
    fi
    STEP=0
    printf '%s\t%s\t%s\n' "$stem" "$((SECONDS - bt0))" exclusive >> "$E2E_DIR/bundle-times.txt"
  done
  # The schedule's memory for the NEXT run: this run's seconds for every bundle that passed.
  e2e_durations_merge_ "$E2E_DURATIONS" "$E2E_DIR/bundle-times.txt" "$E2E_DIR/ran-bundles.txt"
}
# ======================================================================================================================
dropin_check_names
step 1 "docker compose up -d --build; wait ≤120s for GET /readyz = 200; sweep whatever an aborted earlier run left"
START_TS=$(date -u -d '-1 minute' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)
# Feature #4's positive half (step 20) needs the app to hold a real webhook, and only the operator can
# supply one. A SHELL variable wins over .env in compose interpolation, so
# exporting it here is the whole mechanism — nothing is written to .env, and step 17 recreates the
# app WITHOUT it so the shared stack never keeps a test webhook. Unset (the normal run) changes
# nothing: the app boots with no webhook and step 20 proves it made no outbound request.
if [ -n "${E2E_DISCORD_WEBHOOK_URL:-}" ]; then
  export DISCORD_WEBHOOK_URL="$E2E_DISCORD_WEBHOOK_URL"
  dc_config_has_ "DISCORD_WEBHOOK_URL: $E2E_DISCORD_WEBHOOK_URL" \
    || die "compose did not pass DISCORD_WEBHOOK_URL to the app (a shell variable must override .env)"
  echo "[e2e]   Discord auto-post ENABLED for this run (step 20 runs its positive half)"
fi
# The upload limiters, raised for this run alone, by the same mechanism and for a reason the
# suite already documents (step 12: "the upload limiters are shared with every later step and a
# tripped bucket would poison the run", which is why nothing here asserts their ceiling).
#
# `uploadRate` is charged by far more than uploads: POST /api/clips, /api/upload and /api/share,
# AND every POST that starts media work on a clip — trim, gif, replace and poster all carry it
# (internal/httpapi/router.go). One pass of this suite makes ~85 of those requests for the ONE
# account step 3 registers, against a production default of 60 per hour per user, so the run
# refuses its own later steps — steps about upload defaults and card images, not about rate
# limiting. The ceilings themselves are unit-tested (internal/ratelimit, internal/httpapi), so
# raising them here costs no coverage, and step 17 hands the stack back without them.
export RATE_UPLOAD_PER_HOUR="${E2E_RATE_UPLOAD_PER_HOUR:-600}"
export RATE_UPLOAD_PER_HOUR_PER_IP="${E2E_RATE_UPLOAD_PER_HOUR_PER_IP:-1200}"
dc_config_has_ "RATE_UPLOAD_PER_HOUR: \"$RATE_UPLOAD_PER_HOUR\"" \
  || die "compose did not pass RATE_UPLOAD_PER_HOUR to the app (a shell variable must override .env)"
# The AUTH limiter, raised for the same reason and by the same mechanism (§20 erratum 245).
# RATE_AUTH_PER_MIN is 10 a minute per client ADDRESS and register/login/PATCH password all draw on it.
# One pass of this suite already spends eight of them inside 33-account.sh alone and names its
# neighbours' spends in a comment, because the budget was tight enough to plan by hand; with bundles
# running in lanes the spends of four files land in the same minute and the limiter starts refusing
# logins in steps that are not about rate limiting (70-web.sh already carries a 429 retry for exactly
# that reason). NOTHING in the suite asserts this ceiling — the two limiter probes that do are the
# `view` one in 55-owner.sh and the `poll` one in 62-tiers.sh, and both are compile-time constants this
# cannot touch — so raising it costs no coverage, exactly as the upload pair above costs none. Step 17
# hands the stack back without it.
export RATE_AUTH_PER_MIN="${E2E_RATE_AUTH_PER_MIN:-120}"
# The SWEEPER's tick, likewise. `SWEEP_INTERVAL` is an operator knob (DEPLOY §4) and its shipped
# default is 60s, which is the right default for a deployment and a pure toll on a test run: steps 14,
# 15 and 19 each wait for a TICK — up to a minute of a 21-minute run apiece, spent asleep — and the
# thing they assert is that the sweeper destroys what is due, never how often it looks. Fifteen seconds
# keeps every assertion (wait_gone still allows 150s, i.e. ten ticks now instead of two) and takes about
# ninety seconds off the run. `E2E_SWEEP_INTERVAL=60s bash scripts/e2e.sh` restores the shipped value.
export SWEEP_INTERVAL="${E2E_SWEEP_INTERVAL:-15s}"
dc_config_has_ "SWEEP_INTERVAL: $SWEEP_INTERVAL" \
  || die "compose did not pass SWEEP_INTERVAL to the app (a shell variable must override .env)"
# REGISTRATION, likewise, and for the same reason the limiters are: this script registers three
# throwaway users per run, and an instance on a real domain keeps registration CLOSED between
# runs (the owner's rule of 2026-09-11, the day clippet.gg went live). Opening it for the run alone, in the
# environment, means .env never has to say `open` for the suite's sake, and step 17 hands the
# stack back closed. Nothing in the suite asserts the shipped value of this knob: the three-way
# behaviour (open / first / closed) is proven by 33-account.sh in its OWN probe containers.
# `E2E_REGISTRATION=first bash scripts/e2e.sh` runs against the shipped default (and fails at the
# guard below on any stack whose users table is not empty — which is the guard doing its job).
export REGISTRATION="${E2E_REGISTRATION:-open}"
dc_config_has_ "REGISTRATION: $REGISTRATION" \
  || die "compose did not pass REGISTRATION to the app (a shell variable must override .env)"
# The app container's CPU QUOTA, run-scoped the same way the knobs above are. compose reads
# `cpus: ${APP_CPUS:-1.0}` and until now the only way to time a gate at the cores a box actually has
# was to hand-edit that stack's `.env` — which a lane worktree then carries around as an untracked
# difference from every other lane. With E2E_APP_CPUS unset nothing is exported and `.env` decides
# exactly as before.
#
# It MUST NOT EXCEED the host's core count (DEPLOY §7): unlike `mem_limit`, which the daemon accepts
# above physical RAM, a `cpus` above `nproc` is a hard container-CREATE error, so the app is never
# built and `restart: unless-stopped` cannot help. The value compose RESOLVED is printed either way,
# because a run's timings say nothing without it — the same run is about four times faster at 4.
if [ -n "${E2E_APP_CPUS:-}" ]; then export APP_CPUS="$E2E_APP_CPUS"; fi
# The APP service's own quota, not the first `cpus:` in the merged file: compose prints one service
# per two-space key and its settings under it, so the value is read inside the `app:` block and an
# overlay that gave a `cpus` to a service sorting before it (an `agent:`, say) can no longer be
# printed as the app's (§20 erratum 275).
E2E_CONF=$(dc config 2>/dev/null | tr -d '\r' || true)   # read once, whole: see dc_config_has_ on early-exit readers
E2E_CPUS=$(awk '
  /^  [A-Za-z0-9_.-]+:[[:space:]]*$/ { svc = substr($1, 1, length($1) - 1); next }
  svc == "app" && $1 == "cpus:" { print $2; exit }' <<< "$E2E_CONF")
echo "[e2e]   app cpu quota: ${E2E_CPUS:-unknown} (E2E_APP_CPUS=${E2E_APP_CPUS:-unset}; it must not exceed this host's core count)"
echo "[e2e]   machine: $E2E_NCPU CPUs; parallel width E2E_LANES=$E2E_LANES ($E2E_LANES_FROM)"
# Whatever probe container a KILLED earlier run left (§20 erratum 290, AUDIT #11): this project's, by
# label, and the harness's own names, before the stack is rebuilt underneath them.
E2E_PROJECT=$(sed -n '/^name:/{s/^name:[[:space:]]*//p;q;}' <<< "$E2E_CONF")
e2e_sweep_probes_ "$E2E_PROJECT"
# From here the exports above reach the app, so from here the stack must be handed back on ANY exit
# (§20 erratum 290, AUDIT A2): the EXIT trap does what step 17's last act does, once.
e2e_handback_arm_
# The COMMIT this stack is built from (§20 erratum 303). compose reads
# `COMMIT: ${CLIPPET_COMMIT:-}` as a build argument and the binary publishes it
# on GET /api/instance, which step 94 asserts against this same value — so a
# gate-built stack always carries one rather than the `null` a bare
# `docker compose build` produces. A shell variable wins over `.env` in compose
# interpolation, exactly as the limiters above do.
#
# It stamps HEAD even on a DIRTY tree, deliberately: `build` is not a claim that
# the working copy is clean (`git describe --dirty` is what CLIPPET_VERSION is
# for), it is the commit the verification plane looks a release manifest up by,
# and a release is published from a clean tree by the runbook that says so
# (DEPLOY §17.4).
export CLIPPET_COMMIT="$(git rev-parse HEAD 2>/dev/null || true)"
echo "[e2e]   build commit: ${CLIPPET_COMMIT:-(none: not a git checkout)}"
# THE BUCKET BEFORE THE APP BOOTS (run 25): the app's boot-time snapshot (cmd/clippet/snapshot.go, due_now on a
# fresh database) wrote into a bucket this step had not created yet — under load the `up` below won the race
# against the `init` after it — and the doctor then reported that one failure for the snapshot's whole
# ten-minute retry period: four doctor-running bundles red with nothing wrong. With the harness S3 service in
# the stack, `run` brings `s3` up (its healthcheck is the app's depends_on) and makes the bucket from a one-off
# app container; the `up` after it finds the bucket there. A stack without `s3` (the folder store, a real
# bucket) has nothing to do here. Not fatal: a failure leaves the `init` below and /readyz to say so.
if dc config --services 2>/dev/null | grep -qx s3; then
  out=$(dc run --rm --build -T app admin objects init 2>&1 || echo "init-failed")
  case $out in
    *init-failed) echo "[e2e]   NOTE: admin objects init before the app failed: $(printf '%s' "$out" | grep -v '^init-failed$' | tail -1)" ;;
    *) echo "[e2e]   store (before the app boots): $(printf '%s' "$out" | tail -1)" ;;
  esac
fi
dc up -d --build
# THE BUCKET, before the readiness wait (§20 erratum 346): `admin objects init` creates it when it is
# missing, which is what the overlay's old `bucket-init` container did with `mc mb --ignore-existing`. On a
# fresh harness S3 server it is missing, and /readyz — a LIVE ping that asks whether the bucket exists —
# answers 503 until it is not; the app's boot ping loop retries for 30 s, well past this. On a bucket that
# exists (R2, a stack that has run before) it is one HEAD; on STORAGE_BACKEND=fs it says there is nothing to
# create. Not fatal here: if it fails, the wait below fails with the store's own word on /readyz.
# The `||` is INSIDE the substitution: the ERR trap is inherited by the subshell (set -E), and a failure
# there that is not itself in a tested list would print a "FAIL step 1" line for a step that goes on.
out=$(dc exec -T app clippet admin objects init 2>&1 || echo "init-failed")
case $out in
  *init-failed) echo "[e2e]   NOTE: admin objects init failed: $(printf '%s' "$out" | grep -v '^init-failed$' | tail -1)" ;;
  *) echo "[e2e]   store: $out" ;;
esac
deadline=$((SECONDS + 120))
until code=$(get "$BASE/readyz" 2>/dev/null || true); [ "$code" = 200 ]; do
  [ $SECONDS -lt $deadline ] || die "GET /readyz did not answer 200 within 120s (last: '$code' $(cat "$RESP" 2>/dev/null || true))"
  sleep 0.5   # was 2 (lane speed2): the boot is a few seconds and this is the first thing every run waits on
done
echo "[e2e]   ready: $(cat "$RESP")"
# THE CLOCK. The Docker VM's clock runs behind this host and STEPS — it has been measured 12-27 s
# behind, and it has stepped BACKWARDS by 27 s inside a single 25 s sleep under load.
# Nothing in the PRODUCT cares (a TTL is days), but every seconds-scale probe in this suite carries
# that margin implicitly — steps 53, 54 and 73 are the three — and when one of them fails the first
# hour goes into rediscovering the fact. So it is measured once here and once at the summary, with no
# assertion either time: a number in the transcript turns the next clock failure into one line.
e2e_vm_epoch() { dc exec -T app sh -c 'date +%s' 2>/dev/null | tr -d ' \r' | head -1; }
E2E_SKEW0=
E2E_VM0=$(e2e_vm_epoch || true)
case ${E2E_VM0:-x} in
  ''|*[!0-9]*) echo "[e2e]   clock: the app container's clock could not be read (skew unknown)" ;;
  *) E2E_SKEW0=$(( $(date +%s) - E2E_VM0 )); echo "[e2e]   clock: host-VM skew ${E2E_SKEW0}s (the host is ahead by that much)" ;;
esac
# WHATEVER AN ABORTED EARLIER RUN LEFT, before this one adds anything of its own (§20 erratum 272).
# Here rather than earlier in the step because the sweeper the wait depends on is the one `dc up -d
# --build` just started with this run's own 15s SWEEP_INTERVAL already in force; here rather than
# later because step 3 is about to mint the accounts this very selector matches. FATAL: a stack that
# cannot be swept is not a stack to test on, and a run that starts on somebody else's leftovers is a
# run whose bucket census and user inventory mean nothing.
e2e_sweep_own "step 1, before this run makes anything" fatal
# REGISTRATION defaults to `first` since §20 erratum 106 — open only while the users table is EMPTY —
# and this script registers three throwaway users per run. Step 1 exported `REGISTRATION=open` into
# the run's own environment (above), so on any stack compose can recreate this probe answers 400;
# a 403 here means the export did not reach the app (an image older than the knob, or a stack that
# was not recreated). Fail HERE, with the fix, rather than at step 3 with a bare 403 that reads
# like a bug in the app. The probe is a deliberately INVALID registration: `registration_closed`
# is decided before the body is validated, so a closed instance answers 403 and an open one answers
# 400 — without ever creating an account. `|| echo 000` keeps a curl failure out of the ERR trap.
# BUDGET: it still spends one RATE_AUTH_PER_MIN token — the limiter reserves before the handler runs,
# so a 400 costs exactly what a real attempt costs. That budget stopped being tight when the run raised
# the limiter (E2E_RATE_AUTH_PER_MIN, 120/min/IP by default, above; the shipped 10/min/IP is unit-tested
# and asserted nowhere here): one token of 120 needs no counting by hand any more.
REG_CODE=$(http POST "$BASE/api/auth/register" -H 'Content-Type: application/json' -d '{"username":"","password":""}' || echo 000)
if [ "$REG_CODE" = 403 ]; then
  die "registration is closed on this stack, and this script registers three users per run.
      Step 1 exports REGISTRATION=open for the run (E2E_REGISTRATION overrides it) and recreates the app;
      a 403 here means that export did not reach the app — check 'docker compose config' and the app's
      boot line (registration=…), or run 'docker compose up -d' with REGISTRATION=open in the environment."
fi
[ "$REG_CODE" = 400 ] || echo "[e2e]   NOTE: the registration probe answered $REG_CODE (expected 400); continuing"

step 2 "test-clip matrix (scripts/testclips.sh, idempotent)"
bash scripts/testclips.sh
for f in std-720p30 noaudio-720p30 vertical-720x1280 hd-1080p60 dualaudio-720p30; do
  [ -s "testclips/$f.mp4" ] || die "testclips/$f.mp4 missing"
done

step 3 "register a unique user and the second user (its name + _b); GET /api/auth/me with the cookie jar"
E2E_USER="e2e_$(date +%s)"; E2E_PASS="e2e-pass-$(date +%s)"
code=$(setup_once_ register http POST "$BASE/api/auth/register" -c "$JARW" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$E2E_USER\",\"password\":\"$E2E_PASS\"}")
expect "$code" 201 "register"
REG_COOKIE=$(hdr Set-Cookie); expect_nonempty "$REG_COOKIE" "register Set-Cookie"
expect "$(json user.username)" "$E2E_USER" "register → user.username"
code=$(get "$BASE/api/auth/me" -b "$JARW"); expect "$code" 200 "me"
expect "$(json user.username)" "$E2E_USER" "me → user.username"
expect "$(json via)" session "me → via"
# Is the run's account this stack's OWNER (§20 erratum 349)? The first account an instance gets is, so on
# a stack swept by step 1 it always is — and 87-storage, 92-setup and 94-instance then read the owner's
# pages (/setup, the Storage panel, Check now) as it. On a stack whose owner is a PERSON (the reference
# stack under `BASE=… gate.sh`, dev) it is a member: the run goes on, those three read the owner-only parts
# behind E2E_IS_OWNER and print a stand-down, and 92 pins the member's refusal with the run's own account.
# It changes nothing to find out, and nothing here ever hands the owner role to the run's account.
E2E_OWNER=$(dc exec -T app clippet admin users 2>&1 | owner_of_listing_)
if [ "$E2E_OWNER" = "$E2E_USER" ]; then
  E2E_IS_OWNER=1
  echo "[e2e]   the run's account is this stack's owner (owner=yes in admin users)"
else
  E2E_IS_OWNER=0
  echo "[e2e]   NOTE: the run's account is a member here (${E2E_OWNER:-nobody} is this stack's owner): 87, 92 and 94 stand down on the owner's pages; for those, use a fresh stack (docker compose down -v)"
fi
if [ "$SECURE" = 1 ]; then
  # https-only (§14: `Secure` iff urls.IsHTTPS()). Both cookie-issuing routes are checked — a login
  # over a public origin is the path a returning user actually takes, and it is the only place the
  # E2E exercises POST /api/auth/login. The probe session is logged out immediately.
  contains "$REG_COOKIE" '; Secure' || die "register Set-Cookie lacks Secure on an https base: ${REG_COOKIE#*;} (its attributes; the value is a live session)"
  contains "$REG_COOKIE" HttpOnly || die "register Set-Cookie lacks HttpOnly: ${REG_COOKIE#*;} (its attributes; the value is a live session)"
  LJAR="$E2E_DIR/cookies-login.txt"; LJARW=$(W "$LJAR"); rm -f "$LJAR"
  code=$(http POST "$BASE/api/auth/login" -c "$LJARW" -H 'Content-Type: application/json' \
    -d "{\"username\":\"$E2E_USER\",\"password\":\"$E2E_PASS\"}")
  expect "$code" 200 "login"
  LOGIN_COOKIE=$(hdr Set-Cookie); expect_nonempty "$LOGIN_COOKIE" "login Set-Cookie"
  contains "$LOGIN_COOKIE" '; Secure' || die "login Set-Cookie lacks Secure on an https base: ${LOGIN_COOKIE#*;} (its attributes; the value is a live session)"
  contains "$LOGIN_COOKIE" HttpOnly || die "login Set-Cookie lacks HttpOnly: ${LOGIN_COOKIE#*;} (its attributes; the value is a live session)"
  code=$(get "$BASE/api/auth/me" -b "$LJARW"); expect "$code" 200 "me (login session)"
  code=$(http POST "$BASE/api/auth/logout" -b "$LJARW"); expect "$code" 204 "logout (login probe session)"
  echo "[e2e]   https: register and login both set Secure; HttpOnly cookies"
fi
# The SECOND user, a stranger to the owner's clips: 15-ttl, 21-trim, 37-library, 43-watch, 55-owner, 70-web
# and 77-design each sign it in with a session of their own. Step 19 registered it until run 9; it is
# setup now, so no bundle depends on another for it. Its session here is logged out at once.
JARB="$E2E_DIR/cookies-b.txt"; JARBW=$(W "$JARB"); rm -f "$JARB"
code=$(setup_once_ "register the second user" http POST "$BASE/api/auth/register" -c "$JARBW" -H 'Content-Type: application/json' \
  -d "{\"username\":\"${E2E_USER}_b\",\"password\":\"$E2E_PASS\"}")
expect "$code" 201 "register the second user ${E2E_USER}_b"
code=$(http POST "$BASE/api/auth/logout" -b "$JARBW" || echo 000); rm -f "$JARB"

step 4 "uploads C and E (metadata part first, then video) and X (the capture route); TTL wire format: an explicit 0"
# A, B and D were uploaded here until run 9 (lane `pool`); only 7-clip ever read them, and it expires,
# PATCHes and deletes them, so they are its own now and the setup waits for three clips fewer. The
# shared clips below are the ones OTHER bundles read: C (six of them), E and X.
code=$(setup_once_ "upload C" upload C vertical-720x1280.mp4 '{"title":"E2E vertical","ttl_days":0}'); expect "$code" 201 "upload C ($(cat "$RESP"))"
C=$(json id); expect "$(json ttl_days)" 0 "C ttl_days (explicit 0)"; expect_empty "$(json expires_at)" "C expires_at (0 = never, not absent)"
# E is the run's UNLISTED clip: it is the control for feature #4's per-upload opt-out (step 20) —
# an unlisted clip is never auto-posted — and unlisted changes nothing else about it (§9.1 renders
# the same card).
code=$(setup_once_ "upload E" upload E dualaudio-720p30.mp4 '{"title":"E2E dual","unlisted":true}'); expect "$code" 201 "upload E ($(cat "$RESP"))"
E=$(json id); expect "$(json unlisted)" true "E unlisted"
# X is the run's shared CAPTURE clip: the one clip of the six that came through POST /api/upload (with the
# owner's session, which the capture route accepts beside a token). 37-library's batched-progress step
# names it beside C and E. It was step 23's tagged capture upload until run 9 and the filtered run's own
# setup; since the core moved into the pool every run makes it here, untagged.
code=$(setup_once_ "upload X" curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "title=E2E capture X" -F "ttl_days=1" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/small-640x360.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "upload X through the capture route ($(cat "$RESP"))"
X=$(json id); expect_nonempty "$X" "X id"
echo "[e2e]   C=$C E=$E X=$X"

step 6 "poll progress for C, E and X (≤180s each; 404 is an error, never done; percent monotone → 100)"
wait_ready "$C" C; wait_ready "$E" E; wait_ready "$X" X

# ---- WHICH SHAPE is this stack serving media in? (§20 erratum 280) ----------------------------------
# One anonymous GET of the run's first ready clip, with no `-L` and the body thrown away. A 200 is
# `MEDIA_DELIVERY=proxy` — the app streams every byte, which is what every run before 2026-09-14 saw.
# A 302 is `redirect` (erratum 278): the bytes come from the bucket's own origin behind a freshly
# signed link, and the whole media half of this suite is asserted one hop further along.
#
# It is asked of the STACK and never of `.env`: a public run has no `.env` to read, a lane's `.env` is
# what an operator MEANT rather than what the process booted with, and the answer on the wire is the
# only one an assertion may rest on (the origin-dependent-assertion rule, one step further).
# Anything but those two codes ends the run here rather than at the first converted site: the media
# routes' contract is that a ready clip's rendition is fetchable, and a third answer is a stack this
# suite has nothing true to say about.
#
# It runs in every run, filtered or not: every drop-in that touches media needs it, and `E2E_ONLY` must
# not change what the helpers do.
code=$(curl_ -sS -o /dev/null -D "$HDRW" -w '%{http_code}' "$BASE/api/clips/$C/video.mp4")
case $code in
  200) E2E_MEDIA=proxy ;;
  302)
    E2E_MEDIA=redirect
    MEDIA_LOC=$(hdr Location)
    [ -n "$MEDIA_LOC" ] || die "media delivery: a 302 with no Location — this stack is in no shape this suite knows"
    E2E_MEDIA_ORIGIN=$(media_origin_ "$MEDIA_LOC")
    case $E2E_MEDIA_ORIGIN in
      http://*|https://*) ;;
      *) die "media delivery: the Location's origin reads '$E2E_MEDIA_ORIGIN'" ;;
    esac
    ;;
  *) die "media delivery: GET /api/clips/$C/video.mp4 answered '$code'; a ready clip's rendition is 200 (proxy) or 302 (redirect)" ;;
esac
if [ "$E2E_MEDIA" = redirect ]; then
  echo "[e2e]   media delivery: redirect (${E2E_MEDIA_ORIGIN#*://})"
  # …and the OTHER half of erratum 278(a), asserted once here rather than assumed everywhere: the
  # three routes that are never signed stay app-served in this mode too. `/original` is the one with
  # an identity gate, which a presigned URL cannot carry; `preview.gif` and `filmstrip.jpg` are the
  # two this app's own JavaScript POLLS with `fetch`, which cannot read the status of a cross-origin
  # redirect — steps 22 and 71 pin each of those on the answer they already fetch.
  code=$(head_ "$BASE/api/clips/$C/original" -b "$JARW")
  expect "$code" 200 "the owner's /original with the stack in redirect mode"
  expect_empty "$(hdr Location)" "/original is the one identity-gated object and is never signed (§20 erratum 278(a))"
  expect_nonempty "$(hdr ETag)" "…and being app-served it still carries the object's own validator"
  echo "[e2e]   redirect: /original is still served by the app, with its own validators and its owner gate"
else
  echo "[e2e]   media delivery: proxy (${BASE#*://})"
fi

# ----------------------------------------------------------------------------------------------------
# Steps 7-30 were the harness's own serial core until run 9 (lane `pool`): they are bundles in the pool now,
# scripts/e2e.d/7-clip.sh (7-14), 15-ttl.sh (15, 19), 18-capture.sh (18, 29), 20-pages.sh (20, 23, 27, 30),
# 21-trim.sh (21, 24) and 22-gif.sh (22), each keeping its old step numbers. Steps 5, 25, 26 and 28 were cut
# after the duplicates read (scripts/e2e.d/README.md "The core in the pool" names the proof that stayed).
# What steps 1-6 hand every bundle: the stack with the run's limiters, the fixtures, the owner ($E2E_USER,
# $JARW), the second user ${E2E_USER}_b, and the ready clips C, E and X (A, B and D are 7-clip's own).
# ----------------------------------------------------------------------------------------------------

# Drop-in steps: scripts/e2e.d/*.sh run here, BEFORE the log gate (step 16) and the logout (17), so
# parallel builders add a FILE each instead of fighting over these lines. A drop-in uses the same
# helpers (step/expect/die/W/dc/upload…) and the same shared clips (C, E and X, made by steps 4–6, live
# until step 17) and must
# leave nothing behind that step 17 would not clean. Name = first step number + bundle:
# e2e.d/33-account.sh. Since §20 erratum 245 they are not simply sourced in name order: each declares
# whether it may run beside another, run_dropins runs the parallel-safe ones in lanes and then the
# exclusive ones alone, and E2E_ONLY/E2E_SKIP narrow the set. `E2E_LANES=1` is the old behaviour exactly.
run_dropins

step 16 "app log: no panic, no ERROR lines (§12 doctrine)"
# The gate keys on the level FIELD, never on a bare substring (§20 erratum 24): request paths are client-controlled and
# echoed (quoted) into INFO access-log lines, so `GET /x%20level=ERROR` or `GET /panic` must not fail it. Every slog
# record starts `time=… level=… msg=…` (JSON: {"time":…,"level":…}); an unrecovered crash prints the Go runtime's
# `panic: …` / `fatal error: …` at the start of a line. `(^|\| )` anchors to the record after compose's `name | ` prefix.
LOG_ERR_RE='(^|\| )(time=[^ ]+ level=ERROR |\{"time":"[^"]*","level":"ERROR"|panic: |fatal error: )'
LOG="$E2E_DIR/app.log"
printf '%s\n' \
  'app-1  | time=2026-01-01T00:00:00.000Z level=INFO msg=http method=GET path="/nope level=ERROR msg=x panic: y" status=404' \
  'app-1  | time=2026-01-01T00:00:00.000Z level=INFO msg=http method=GET path=/panic-probe status=404' \
  'app-1  | {"time":"2026-01-01T00:00:00Z","level":"INFO","msg":"http","path":"/x\"level\":\"ERROR\" panic: y"}' \
  > "$E2E_DIR/loggate-planted.txt"
printf '%s\n' \
  'app-1  | time=2026-01-01T00:00:00.000Z level=ERROR msg=panic err=boom method=GET path=/' \
  'app-1  | {"time":"2026-01-01T00:00:00Z","level":"ERROR","msg":"panic","err":"boom"}' \
  'app-1  | panic: runtime error: index out of range [3] with length 3' \
  'app-1  | fatal error: all goroutines are asleep - deadlock!' \
  > "$E2E_DIR/loggate-real.txt"
if grep -qE "$LOG_ERR_RE" "$E2E_DIR/loggate-planted.txt"; then die "log gate self-test: a client-planted substring matched the gate"; fi
n=$(grep -cE "$LOG_ERR_RE" "$E2E_DIR/loggate-real.txt" || true); expect "$n" 4 "log gate self-test: real ERROR/panic lines caught"
dc logs --no-color --since "$START_TS" app > "$LOG" 2>&1 || die "docker compose logs failed"
[ -s "$LOG" ] || die "empty app log"
if grep -nE "$LOG_ERR_RE" "$LOG"; then die "ERROR/panic lines in the app log (above)"; fi
echo "[e2e]   $(wc -l < "$LOG" | tr -d ' ') log lines, zero ERROR/panic"

step 17 "cleanup: DELETE C/E/X, log out, sweep this run's own accounts — the stack is shared and these have no TTL of their own"
# 7-clip removes its A, B and D; the other core bundles delete what they made. C and E are uploaded
# with ttl `never` and X (step 4's capture upload) with one day, so without this every run left clips,
# their objects and a logged-in session behind on the shared stack. Best effort ONLY: nothing here may
# fail a run that has already passed (hence `|| echo 000` on every call), and it runs AFTER the step-16
# log gate so its own lifecycle lines are never read as trouble.
# ONE EXCEPTION, and it is opt-in: with `E2E_CLEAN_BUCKET=1` the closing bucket census ASSERTS, and a
# stack the run started clean that ends dirty fails the run right here. That is the knob's whole
# purpose (§20 erratum 269(d)), so it is named rather than left as a contradiction of the line above.
# (A, B and D are 7-clip's own since run 9, and that bundle deletes or expires them itself.)
E2E_CLEAN_LIST="C:${C:-} E:${E:-} X:${X:-}"
for pair in $E2E_CLEAN_LIST; do
  label=${pair%%:*}; id=${pair#*:}
  [ -n "$id" ] || continue
  code=$(http DELETE "$BASE/api/clips/$id" -b "$JARW" || echo 000)   # 204 = destroyed, objects and row gone
  echo "[e2e]   DELETE $label $id → $code"
done
code=$(http POST "$BASE/api/auth/logout" -b "$JARW" -c "$JARW" || echo 000)
echo "[e2e]   logout $E2E_USER → $code"
# THE USERS THIS RUN MADE, and anything of theirs the DELETEs above have not finished removing
# ($E2E_USER, _b, _c, the _d 33-account.sh's `admin users create` minted, and the _s1/_s2 pair). There
# is no user-delete ROUTE by design — removing an account is housekeeping, not a product surface — so
# this goes through the same `e2e_sweep_own` step 1 opens the run with (§20 erratum 272): the clips
# are force-expired and WAITED for, which is also what makes the census below read a settled stack,
# and the emptied accounts are pruned. Until this erratum the prune here carried `--older-than 1h`,
# which by construction could not match the run's own users: every passing run left four accounts
# behind and every aborted one left their clips too. Best effort like everything else in this step:
# nothing here may fail a run that has already passed, and the sweep prints its target list either
# way.
e2e_sweep_own "end of run" best-effort
# THE BUCKET, ONCE, AFTER EVERYTHING. Step 54's `extra` assertion became a FLOOR when the bundles
# started running in lanes (a bucket-wide count cannot be exact while another bundle is uploading), and
# every other `admin orphans` call in the suite is scoped to one clip — so a stray object under a clip
# nobody in the run owns stopped being caught anywhere. This is the census that closes that: it runs
# after the last bundle AND after the deletes above, so nothing of this run's is still in flight, and
# `--min-age 0` means it looks at everything rather than only at what is already old.
#
# It PRINTS by default and asserts only with E2E_CLEAN_BUCKET=1, and that split is the point: the gate
# runs on a shared stack where an operator's own leftovers are none of this suite's business, while on a
# stack the run started clean `extra=0` is a real invariant worth failing on. A number in every
# transcript is most of the value either way — a leak becomes visible on the run that introduced it.
if out=$(dc exec -T app clippet admin orphans --min-age 0 2>&1); then
  E2E_ORPH=$(printf '%s\n' "$out" | tr -d '\r' | grep '^orphans: ' | tail -1 || true)
  echo "[e2e]   ${E2E_ORPH:-orphans: (no summary line in the output)}"
  E2E_EXTRA=$(printf '%s' "${E2E_ORPH:-}" | sed -n 's/.* extra=\([0-9][0-9]*\) .*/\1/p')
  # All THREE of the census's "something is wrong" counts, not just the last one: `extra=` is an
  # object under a live clip that no column names, `prefixes=` is a whole prefix with no row behind
  # it, and `missing=` is a row whose object is gone. The stray this assertion exists to catch can
  # land in any of the three, and asserting one of them left the other two uncaught.
  E2E_PREFIXES=$(printf '%s' "${E2E_ORPH:-}" | sed -n 's/^orphans: prefixes=\([0-9][0-9]*\) .*/\1/p')
  E2E_MISSING=$(printf '%s' "${E2E_ORPH:-}" | sed -n 's/.* missing=\([0-9][0-9]*\) .*/\1/p')
  if [ "${E2E_CLEAN_BUCKET:-0}" = 1 ]; then
    expect_nonempty "$E2E_EXTRA" "E2E_CLEAN_BUCKET=1: admin orphans printed no extra= count"
    expect_nonempty "$E2E_PREFIXES" "E2E_CLEAN_BUCKET=1: admin orphans printed no prefixes= count"
    expect_nonempty "$E2E_MISSING" "E2E_CLEAN_BUCKET=1: admin orphans printed no missing= count"
    expect "$E2E_EXTRA" 0 "E2E_CLEAN_BUCKET=1: the bucket must hold no row-less objects when the run ends"
    expect "$E2E_PREFIXES" 0 "E2E_CLEAN_BUCKET=1: the bucket must hold no orphaned clip prefix when the run ends"
    expect "$E2E_MISSING" 0 "E2E_CLEAN_BUCKET=1: no surviving row may name an object that is gone"
    echo "[e2e]   E2E_CLEAN_BUCKET=1: the bucket is clean (prefixes=0 missing=0 extra=0) after every bundle and every delete"
  fi
else
  echo "[e2e]   NOTE: 'clippet admin orphans' failed (not fatal): $out"
fi
# …and hand the stack back the way this run found it. Step 1 exported the raised upload limiters
# into compose's environment — and, with E2E_DISCORD_WEBHOOK_URL set, the test webhook too — and
# the stack is SHARED: leaving either in place would keep every future upload posting to the owner's
# test channel, and would leave an operator running a limit they never configured.
# The same function the EXIT trap armed in step 1 calls (§20 erratum 290, AUDIT A2): it runs once, here
# on a run that reached its end, from the trap on one that did not.
e2e_handback_ "end of run"

step_close_; STEP=0
echo
# WHERE THE TIME WENT (§20 errata 245, 283 (i)). "About twenty minutes" was all anybody could say
# about this suite before; the per-step and per-bundle tables are what a speed pass reads first, and
# they cost one sort at the end of a run.
#
# EVERY step, and not the slowest ten. The ten that fitted were all a reader ever got, `e2e_ctx`
# empties the per-context files at the start of the NEXT run, and the other eighty were gone for good
# — which is why the 2026-09-14 speed research had to reconstruct a step table out of three runs'
# top-tens plus a per-bundle residual. The merged table is written to `.e2e/step-times.txt` in STEP
# order as well, so it outlives the transcript and diffs against the next run.
if [ -s "$E2E_DIR/ctx-main/step-times.txt" ]; then
  cat "$E2E_DIR"/ctx-*/step-times.txt 2>/dev/null | sort -k1,1n > "$E2E_DIR/step-times.txt"
  echo "  steps (s), slowest first — the same rows in step order are in .e2e/step-times.txt:"
  sort -k2,2nr "$E2E_DIR/step-times.txt" | awk -F'\t' '{ printf "    step %-4s %5ss  (%s)\n", $1, $2, $3 }'
fi
if [ -s "$E2E_DIR/bundle-times.txt" ]; then
  echo "  bundles (s):"
  sort -k2,2nr "$E2E_DIR/bundle-times.txt" | awk -F'\t' '{ printf "    %-12s %5ss  %s\n", $1, $2, $3 }'
  # …and the PHASE split, which until now had to be derived by subtracting one table from the total.
  # It is the number that says whether a lane rebalance would buy anything: `serial setup` is steps 1-6,
  # 16 and 17 (until run 9's lane `pool` this was `serial main`, and it held the core steps 7-30 too;
  # they are pool bundles now), `pool` is the whole parallel phase however wide it was, and `exclusive`
  # is the bundles that run one at a time after it.
  awk -F'\t' -v total="$SECONDS" '
    $1 == "(parallel phase)" { par = $2; next }
    $3 == "exclusive" { exc += $2 }
    END { printf "  phases (s): serial setup %d | pool %d | exclusive %d   (total %d)\n", total - par - exc, par, exc, total }
  ' "$E2E_DIR/bundle-times.txt"
fi
# The clock again (step 1 took the first reading). Still no assertion: a skew that MOVED during the run
# is the fact worth having in the transcript, because it is what a seconds-scale probe failing halfway
# down the suite will turn out to have been.
if [ -n "$E2E_SKEW0" ]; then
  E2E_VM1=$(e2e_vm_epoch || true)
  case ${E2E_VM1:-x} in
    ''|*[!0-9]*) echo "  clock: host-VM skew was ${E2E_SKEW0}s at step 1; the container's clock could not be read again" ;;
    *) E2E_SKEW1=$(( $(date +%s) - E2E_VM1 ))
       if [ "$E2E_SKEW1" -gt $((E2E_SKEW0 + 1)) ] || [ "$E2E_SKEW1" -lt $((E2E_SKEW0 - 1)) ]; then
         echo "  NOTE: the VM clock MOVED during the run — host-VM skew ${E2E_SKEW0}s → ${E2E_SKEW1}s"
       else
         echo "  clock: host-VM skew ${E2E_SKEW0}s → ${E2E_SKEW1}s (steady)"
       fi ;;
  esac
fi
echo
E2E_RC=0
if [ -s "$E2E_DIR/failed-bundles.txt" ]; then
  E2E_RC=1
  echo "E2E FAIL  ($SECONDS s, base $BASE, user $E2E_USER) — bundle(s) failed: $(tr '\n' ' ' < "$E2E_DIR/failed-bundles.txt")"
  echo "  every other bundle ran; a lane bundle's own log is printed above under its ===== header, an exclusive one's inline."
  echo "  after a HARNESS-only fix: E2E_RESUME=1 runs the setup (steps 1-6) and only the bundles not recorded as passed on this product tree ($E2E_KEY)."
elif [ -n "${DROPINS_SKIPPED:-}" ]; then
  echo "E2E PASS (PARTIAL)  ($SECONDS s, base $BASE, user $E2E_USER)"
  echo "  !! THIS IS NOT THE GATE. A filtered run proves what it ran and nothing else:"
  echo "     bundles NOT run:${DROPINS_SKIPPED}"
  echo "     the gate is 'bash scripts/e2e.sh' with no E2E_ONLY and no E2E_SKIP."
elif [ -n "${DROPINS_RESUMED:-}" ]; then
  echo "E2E PASS (RESUMED)  ($SECONDS s, base $BASE, user $E2E_USER)"
  echo "  the setup ran now; bundle(s) passed EARLIER on the same product tree $E2E_KEY at this origin and were not re-run:${DROPINS_RESUMED}"
  echo "  (the union is the gate: nothing the binary is built from changed between those passes and this run — $(basename "$E2E_CKPT"))"
else
  echo "E2E PASS  ($SECONDS s, base $BASE, user $E2E_USER)"
fi
# Every retry the pool made, on every banner, PASS or FAIL (run 9, lane `retry`): a pass that needed one
# is a pass, and the line is what keeps a real bug that looks like a transient from hiding behind it.
printf '  %s%s\n' "$(e2e_retried_line_)" "$([ "$E2E_RETRY" = 1 ] || echo '   (E2E_RETRY=0: the policy was off)')"
# WHICH media shape this run proved, in the closing banner as well as at step 6: it is the one thing
# about a run that changes what a third of the assertions above actually asked (§20 erratum 280), and a
# transcript read a week later should not need the step-6 line found to say which.
if [ "$E2E_MEDIA" = redirect ]; then
  printf '  %s\n' "media delivery: redirect — the three signed routes were followed to ${E2E_MEDIA_ORIGIN#*://} and asserted there"
else
  printf '  %s\n' "media delivery: proxy — the app served every media byte itself"
fi
# The card-URL count is 7-clip's step 10 (its file, removed at the start of every run): a run that did not
# run 7-clip says nothing about the card URLs rather than repeat an old run's number.
if [ "$SECURE" = 1 ]; then
  printf '  %s\n' "public https: Secure+HttpOnly cookies on register and login, og:video:secure_url emitted"
  if [ -s "$E2E_DIR/card-urls.txt" ]; then
    printf '  %s\n' "$(grep -c . "$E2E_DIR/card-urls.txt" || true) card URLs https on this origin and 200 to Discordbot + Twitterbot, embed framable, no mixed content (7-clip step 10)"
  fi
fi
printf '  %-5s %-13s %s\n' clip id result
# The harness's own clips, as setup made them (steps 3-6). What a bundle then DID to one of them is that
# bundle's own row below (7-clip's A, B, D and E; 20-pages' C), so a filtered run that ran none of them
# lists them as what they are — setup — and nothing more (the hazard erratum 245 closed).
printf '  %s\n' "setup (steps 3-6): C=${C:-} E=${E:-} X=${X:-}"
# …and the bundles' own rows, bundle by bundle. The rows are read back out of each context's sink
# because a parallel bundle wrote them in a subshell of its own. The ORDER is not the finishing order —
# it is the parallel bundles in name order, then the exclusive ones in the order they ran, which is also
# name order — so it is stable from run to run, but it is NOT the single interleaved name order a
# one-at-a-time run produces (`E2E_LANES=1` still gives that).
for stem in ${DROPINS_ORDER:-}; do
  if [ -s "$E2E_DIR/ctx-$stem/summary-rows.txt" ]; then cat "$E2E_DIR/ctx-$stem/summary-rows.txt"; fi
done
# The exclusive bundles all ran in `main`; their rows follow, in the order they ran.
if [ -s "$E2E_DIR/ctx-main/summary-rows.txt" ]; then cat "$E2E_DIR/ctx-main/summary-rows.txt"; fi
exit "$E2E_RC"
