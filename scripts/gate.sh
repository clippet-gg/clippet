#!/usr/bin/env bash
# gate.sh — the whole gate as ONE command (lane `speed2`; run 9 lane `gatestack`): scripts/check.sh, then the
# E2E, then the visual pass. One log per stage under a directory it names, a status line per stage, and
# `GATE PASS` or `GATE FAIL` as the last line.
#
#     bash scripts/gate.sh                                  # THE GATE (T2): on the gate's OWN local stack
#     E2E_RESUME=1 bash scripts/gate.sh                     # after a HARNESS-only fix (scripts/e2e.d/README.md)
#     GATE_KEEP=1 bash scripts/gate.sh                      # …and leave the gate stack up afterwards
#     BASE=https://clippet.gg bash scripts/gate.sh          # the OLD way, deliberately: on the reference stack
#     BASE=http://localhost:85xx bash scripts/gate.sh       # a lane stack (its own checkout's .env names it)
#
# WHERE IT RUNS (run 9, toil/proposal.md §2 F6/F7/F8). By default the E2E and the visual pass run on a
# compose project of the gate's own — `clippet-gate`, http://localhost:8576, its own volumes and S3 server
# (RustFS, compose.e2e-bucket.yaml), MEDIA_DELIVERY=redirect against it — built from THIS checkout by the E2E's step 1 and torn down
# (`down -v`, its image tags, its generated .env) when the gate ends, unless GATE_KEEP=1. Nothing in it
# crosses the Cloudflare edge and nothing restarts production. scripts/lib/gatestack.sh is that stack, and
# before stage 1 the gate REFUSES TO START when the KEYS of the reference stack's .env and the gate stack's
# generated .env differ (names only; a value is never read out). Registration is open in the gate stack's
# own .env, so the registration open/hand-back dance is gone on this path.
# The public edge is proven by scripts/smoke-public.sh (T3, ≈2 min) after a deliberate deploy, not here.
#
# BASE set to anything else is the old way, and it still works: BASE equal to this checkout's own stack
# (its .env's PUBLIC_BASE_URL) rebuilds and recreates THAT app — on the reference checkout, production —
# and opens registration for the visual pass and hands it back; a warning says so first.
#
# VISUAL ONLY WHEN web/ MOVES (fix 7): stage 3 runs when web/ or internal/web/ changed since the last GREEN
# gate's commit (.e2e/checkpoints/gate-green.txt, written here on GATE PASS: commit=, proved=, base=, what the run
# was — release=1|0, visual=ran|skipped, dir= — and the input key tree= / env= / docker= / nvidia= / images= that
# clippet-pass's opt-in --gate-cached compares — `gatestack.sh gate-key`; flavour=gpu|cpu says which image the gate
# stack ran: a gate typed on a Mac is cpu, and proves nothing of compose.gpu.yaml / NVDEC), or GATE_VISUAL=1, or
# GATE_RELEASE=1 (a release gate always runs it); otherwise `3 visual: SKIPPED (no change under web/ since
# <commit>)`. CHECK_RACE=all is passed to check.sh: the gate is where the -race pass lives (run 9, lane t0).
#
# The stages run ONE AFTER ANOTHER and never overlap: one activity per stack, and check.sh is
# load-sensitive. The parallelism is INSIDE each stage — check.sh's stages at once and its Go tests
# sharded, the E2E's pool, the visual pass's workers — and each of those prints the width it used.
#
# It needs no terminal, so it can be started detached (`cmd /c start /min "" bash.exe <a script that
# calls this>` from PowerShell — a plain `Start-Process bash -c` hangs silently; the detached Git Bash holds
# the foreground wsl.exe below). A pure-WSL detach is `wsl.exe -d Ubuntu --exec setsid -f bash -lc '…'`
# (measured: the child outlives wsl.exe). Everything it says goes
# to stdout AND to <dir>/gate.log, so a detached run is read from the file. A stage that fails ends the
# gate there (`GATE_CONTINUE=1` runs the remaining stages anyway, for a post-mortem); the hand-back of a
# shared stack after the visual pass happens whatever the pass's verdict.
#
# IT RUNS IN WSL (DESIGN §20 erratum 338, scripts/lib/wslexec.sh). Typed in Git Bash with WSL's clone present
# ($HOME/dev/clippet inside WSL by default), a gate-mode run re-runs itself THERE at this checkout's commit (a dirty tree is refused: WSL
# runs the commit), streams, and exits with its status. Its LAST act in WSL is a silent copy of the run's
# .e2e/gate-<utc>/ and — on GATE PASS, the record this run wrote — .e2e/checkpoints/gate-green.txt back into THIS
# checkout, so clippet-pass's release cycle reads `GATE PASS` as the last line and the record where it always
# has (GATE_COPY_VISUAL=1 copies .e2e/visual/ too). CLIPPET_WSL=0 runs it here on Git Bash, as before; a Git Bash
# gate refuses while a WSL gate holds the clone. The own and foreign modes (a BASE that is not the gate stack's)
# never re-run, and are REFUSED inside WSL: the WSL clone is the gate's scratch clone, whose .env is a guard
# (project clippet-never; wslexec.sh), not a stack — and compose.yaml's own name is `clippet`, production.
#
# Knobs it reads, all optional: BASE, E2E_RESUME and every E2E_* / CHECK_* / VISUAL_* knob (passed through
# untouched), GATE_DIR (default .e2e/gate-<UTC time>), GATE_CONTINUE=1, GATE_KEEP=1, GATE_VISUAL=1,
# GATE_RELEASE=1, and the gate stack's own (GATE_PROJECT, GATE_PORT, GATE_GPU, … — scripts/lib/gatestack.sh).
set -u; cd "$(dirname "$0")/.."
. scripts/lib/wslexec.sh
GATE_T0=$SECONDS
GS=scripts/lib/gatestack.sh
envfile() { { [ -f .env ] && sed -n "s/^$1=//p" .env | tail -1 | tr -d '\r"'; } || true; }
ENV_BASE=${PUBLIC_BASE_URL:-$(envfile PUBLIC_BASE_URL)}; ENV_BASE=${ENV_BASE:-http://localhost:8481}; ENV_BASE=${ENV_BASE%/}
GATE_PORT=${GATE_PORT:-8576}; export GATE_PORT
GATE_BASE=http://localhost:$GATE_PORT
BASE=${BASE:-$GATE_BASE}; BASE=${BASE%/}; export BASE
# The mode: `gate` (the default — the gate's own stack), `own` (BASE is this checkout's own stack: the old
# way, which recreates that app), `foreign` (BASE is somewhere else; nothing here touches it but the E2E).
if [ "$BASE" = "$GATE_BASE" ]; then GATE_MODE=gate
elif [ "$BASE" = "$ENV_BASE" ]; then GATE_MODE=own
else GATE_MODE=foreign; fi
# Inside WSL only the gate's own stack: the clone is scratch (its .env the guard wslexec.sh writes; the reference
# .env is read in place through GATE_REF_ENV), and no other mode has a stack there. Refused before anything is made.
if [ "${CLIPPET_IN_WSL:-}" = 1 ] && [ "$GATE_MODE" != gate ]; then
  echo "[gate] REFUSED — BASE=$BASE is the $GATE_MODE mode, and inside WSL the gate runs on its own stack only ($GATE_BASE): the own and foreign modes run on Git Bash (CLIPPET_WSL=0)"
  exit 2
fi
if [ "$GATE_MODE" = gate ]; then wsl_reexec_ gate bash scripts/gate.sh "$@"; fi   # exits with the WSL run's status when it re-runs
GATE_START_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)
GATE_DIR=${GATE_DIR:-.e2e/gate-$(date -u +%Y%m%dT%H%M%SZ)}
mkdir -p "$GATE_DIR"
say() { printf '%s\n' "$*" | tee -a "$GATE_DIR/gate.log"; }
# The gate's own stdout/stderr, kept: a signal's trap runs INSIDE the stage that was running, whose `> <stage log>`
# is still in force, so without these the exit path's lines (the teardown, GATE FAIL) went to that stage's log and
# never to the caller (measured on the G4 repro: the Windows side saw nothing after "TERM sent").
exec 7>&1 8>&2
# gate_copyback_ pass|fail — in WSL, the run's last act: its gate dir (and on PASS the record THIS run wrote) into
# the Windows checkout that typed the command. Silent on success; a failure is on stderr and fails the gate.
gate_copyback_() {
  [ "${CLIPPET_IN_WSL:-}" = 1 ] && [ -n "${GATE_WIN_ROOT:-}" ] || return 0
  local rec="" vis=""
  if [ "$1" = pass ] && wsl_record_fresh_ .e2e/checkpoints/gate-green.txt "$(git rev-parse HEAD)" "$GATE_START_UTC"; then rec=.e2e/checkpoints/gate-green.txt; fi
  [ "${GATE_COPY_VISUAL:-0}" != 1 ] || vis=.e2e/visual
  wsl_copyback_ "$GATE_WIN_ROOT" "$GATE_DIR" "$rec" "$vis"
}
# The test webhook (for a run by hand with real credentials) rides along when the operator keeps one in .e2e/webhook.txt and
# has not set it: the E2E's step 20 then runs its positive half. Never printed.
if [ -z "${E2E_DISCORD_WEBHOOK_URL:-}" ] && [ -s .e2e/webhook.txt ]; then
  E2E_DISCORD_WEBHOOK_URL=$(tr -d '\r\n' < .e2e/webhook.txt); export E2E_DISCORD_WEBHOOK_URL
  GATE_HOOK="from .e2e/webhook.txt"
else
  GATE_HOOK=${E2E_DISCORD_WEBHOOK_URL:+set in the environment}
fi
say "[gate] $(date -u +%Y-%m-%dT%H:%M:%SZ) base $BASE (mode $GATE_MODE; the gate stack: $GATE_BASE; this checkout's stack: $ENV_BASE); commit $(git rev-parse --short HEAD 2>/dev/null || echo none); logs in $GATE_DIR"
say "[gate] E2E_RESUME=${E2E_RESUME:-unset}; the test webhook: ${GATE_HOOK:-none}; GATE_CONTINUE=${GATE_CONTINUE:-0}; GATE_KEEP=${GATE_KEEP:-0}"
case $GATE_MODE in
  own)
    say "[gate] WARNING: BASE is this checkout's OWN stack ($ENV_BASE). The E2E's step 1 rebuilds and recreates its app with test"
    say "[gate]          settings, and the visual pass recreates it twice more — on the reference checkout that is the PRODUCTION app."
    say "[gate]          The gate's own stack is the default: drop BASE= to use it. Proceeding as asked." ;;
  foreign)
    say "[gate] NOTE: BASE is neither the gate stack nor this checkout's stack: the E2E and the visual pass run against it as it is" ;;
esac
GATE_FAILED=""
# stage <n> <name> <command…> — run one stage into <dir>/<n>-<name>.log and say its verdict, its time and
# the line of its own log that says so.
stage() {
  local n=$1 name=$2 t0=$SECONDS rc=0 log; shift 2
  log="$GATE_DIR/$n-$name.log"
  if [ -n "$GATE_FAILED" ] && [ "${GATE_CONTINUE:-0}" != 1 ]; then
    say "[gate] $n $name: NOT RUN (an earlier stage failed)"; return 0
  fi
  say "[gate] $n $name: running ($log)"
  "$@" > "$log" 2>&1 || rc=$?
  local verdict
  case $name in
    keys)   verdict=$(grep -E '^\[gatestack\] keys: ' "$log" | tail -1) ;;
    pins)   verdict=$(grep -E '^\[gatestack\] pins: (OK|FAIL)' "$log" | tail -1) ;;
    check)  verdict=$(grep -E '^\[check\] (OK|FAIL)' "$log" | tail -1) ;;
    e2e)    verdict=$(grep -E '^E2E (PASS|FAIL)' "$log" | tail -1) ;;
    visual) verdict="$(grep -E '^\[visual\] [0-9]+ shots' "$log" | tail -1 | cut -c1-80) $(grep -E '^\[visual\] (PASS|FAIL)$' "$log" | tail -1)" ;;
  esac
  if [ "$rc" = 0 ]; then say "[gate] $n $name: PASS in $((SECONDS - t0)) s — ${verdict:-?}"
  else say "[gate] $n $name: FAIL (exit $rc) in $((SECONDS - t0)) s — ${verdict:-no verdict line; read the log}"; GATE_FAILED="$GATE_FAILED $name"; fi
}
# The E2E and the visual pass must NOT run under MSYS_NO_PATHCONV; check.sh sets it for itself.
# On the gate stack both run inside `gatestack.sh run`, which exports COMPOSE_ENV_FILES / COMPOSE_PROJECT_NAME
# / BASE so every `docker compose` they make lands on that stack and never on this checkout's own.
gate_e2e() {
  if [ "$GATE_MODE" = gate ]; then env -u MSYS_NO_PATHCONV bash "$GS" run bash scripts/e2e.sh
  else env -u MSYS_NO_PATHCONV bash scripts/e2e.sh; fi
}
# The visual pass registers an account. On the gate stack registration is open in its own .env, so the
# pass just runs. On this checkout's OWN stack (the old way) registration is opened for it — the app
# recreated with REGISTRATION=open in the environment — and handed back afterwards by recreating it
# WITHOUT that, whatever the pass said. The boot line the app writes on the hand-back is the proof
# (never probe /api/auth/register on the reference stack — that would create an account).
gate_ready_() {   # the recreated app answers /readyz (≤90 s by the clock, polled every half second; each curl bounded —
  local code="" end=$((SECONDS + 90))   # a closed loopback port TIMES OUT under WSL's mirrored networking, erratum 338)
  while [ "$SECONDS" -lt "$end" ]; do
    code=$(curl -s --connect-timeout 2 --max-time 5 -o /dev/null -w '%{http_code}' -H 'X-Clippet-App: 1' "$BASE/readyz" 2>/dev/null || true)
    [ "$code" = 200 ] && return 0
    sleep 0.5
  done
  echo "[gate] the recreated app did not answer /readyz with 200 within 90 s (last: ${code:-none})"; return 1
}
gate_visual() {
  local rc=0
  if [ "$GATE_MODE" = gate ]; then
    echo "[gate] the gate stack: registration is open in its own .env; no hand-back"
    env -u MSYS_NO_PATHCONV bash "$GS" run bash scripts/visual.sh || rc=$?
    return $rc
  fi
  if [ "$GATE_MODE" = own ]; then
    echo "[gate] registration opened for the visual pass (REGISTRATION=open docker compose up -d)"
    REGISTRATION=open docker compose up -d || { echo "[gate] could not open registration"; return 1; }
    gate_ready_ || return 1
  else
    echo "[gate] BASE is not this checkout's stack: registration is left as that stack has it"
  fi
  env -u MSYS_NO_PATHCONV bash scripts/visual.sh || rc=$?
  if [ "$GATE_MODE" = own ]; then
    if env -u REGISTRATION docker compose up -d; then
      # `up -d` REPLACES the container, and `logs` then shows only the new one's — so any boot line it
      # holds is the hand-back's. (Not `--since <host time>`: the Docker VM's clock runs behind this host.)
      local boot="" i=0
      while [ -z "$boot" ] && [ "$i" -lt 30 ]; do
        boot=$(docker compose logs --no-color app 2>/dev/null | grep -oE 'registration=[a-z]+' | tail -1 || true)
        [ -n "$boot" ] || { sleep 1; i=$((i + 1)); }
      done
      echo "[gate] handed back: the app recreated without REGISTRATION in the environment; its boot line says ${boot:-(no registration= line within 30 s — check docker compose logs app)}"
    else
      echo "[gate] NOTE: could not recreate the app — run 'docker compose up -d' to hand registration back"; rc=1
    fi
  fi
  return $rc
}
# The gate stack is torn down on ANY exit once it may exist (a killed gate leaves nothing but what a
# SIGKILL always leaves; the next gate's step 1 rebuilds over it either way), unless GATE_KEEP=1.
gate_down_() {
  [ "$GATE_MODE" = gate ] || return 0
  [ "${GATE_DOWN_DONE:-0}" = 0 ] || return 0
  GATE_DOWN_DONE=1
  if [ "${GATE_KEEP:-0}" = 1 ]; then say "[gate] GATE_KEEP=1: the gate stack stays up ($GATE_BASE); 'bash $GS down' removes it"; gate_tidy_; return 0; fi
  # The app's whole log first: once the stack is down it is the only post-mortem a failed gate has.
  bash "$GS" run sh -c 'MSYS_NO_PATHCONV=1 docker compose logs --no-color --no-log-prefix app' > "$GATE_DIR/4-app.log" 2>&1 || true
  bash "$GS" down >> "$GATE_DIR/4-down.log" 2>&1 && say "[gate] the gate stack is down (containers, network, volumes, image tags, its .env; $GATE_DIR/4-down.log)" \
    || say "[gate] NOTE: the gate stack's teardown failed — read $GATE_DIR/4-down.log and run 'bash $GS down'"
  gate_tidy_
}
# gate_tidy_ — THE BOX AS A WHOLE (lane boxtidy, 2026-10-18): after the gate's own down, scripts/box-tidy.sh's
# READ-ONLY report (whole in 4-boxtidy.log) and its closing line here, on whichever side runs the gate — Docker is the
# same daemon from WSL, and GATE_WIN_ROOT gives the report the Windows repo's worktrees too. It never changes the
# verdict and is never the last line. When it names litter the maintainer runs `bash scripts/box-tidy.sh apply`.
gate_tidy_() {
  local l
  [ -f scripts/box-tidy.sh ] || { say "[gate] box-tidy: not here"; return 0; }
  sh scripts/box-tidy.sh report > "$GATE_DIR/4-boxtidy.log" 2>&1 || true
  l=$(tail -1 "$GATE_DIR/4-boxtidy.log" 2>/dev/null)
  say "[gate] ${l:-box-tidy: no report (read $GATE_DIR/4-boxtidy.log)}"
  return 0
}
# gate_exit_ — the EXIT trap once the stack may exist: the teardown, and — on a run that never reached its own
# verdict (a relayed TERM, a Ctrl-C, an unexpected exit) — a GATE FAIL line naming why and the copy-back of its logs,
# which the verdict paths below otherwise do themselves (GATE_COPIED says they did).
GATE_SIGNAL=""; GATE_COPIED=0
# gate_settle_ — on a signal, BEFORE the teardown: the rest of this run's process group (the stage's e2e.sh and its
# pool's bundles, TERMed with it) is still running its own EXIT traps, and the E2E's hand-back RECREATES the app — so a
# teardown under them is undone (measured 2026-10-18, the lane's review G4: a TERM 15 s after the gate app came up;
# `down` removed the app, the E2E's hand-back made it again, and the network and two volumes stayed "in use"). So it
# waits, ≤ 120 s by the clock, until no other live member of the group is left. /proc read with builtins only (a
# child of ours would count itself), and only when this shell leads its group — wsl-enter's setsid makes it so.
gate_settle_() {
  [ -n "$GATE_SIGNAL" ] && [ -r "/proc/$$/stat" ] || return 0
  local s rest f n said=0 end=$((SECONDS + 120))
  read -r s 2>/dev/null < "/proc/$$/stat" || return 0
  rest=${s##*) }; set -- $rest   # after the comm field: state ppid pgrp …
  [ "${3:-}" = "$$" ] || return 0
  while :; do
    n=0
    for f in /proc/[0-9]*/stat; do
      [ "$f" != "/proc/$$/stat" ] || continue
      read -r s 2>/dev/null < "$f" || continue
      rest=${s##*) }; set -- $rest
      if [ "${3:-}" = "$$" ] && [ "${1:-}" != Z ]; then n=$((n + 1)); fi
    done
    if [ "$n" = 0 ]; then [ "$said" = 0 ] || say "[gate] the run's other processes have ended"; return 0; fi
    if [ "$SECONDS" -ge "$end" ]; then say "[gate] NOTE: $n process(es) of this run still alive 120 s after the $GATE_SIGNAL; tearing down anyway"; return 0; fi
    [ "$said" = 1 ] || { say "[gate] $GATE_SIGNAL: waiting (≤ 120 s) for the run's $n other process(es) to finish their own exit traps (the E2E hands its stack back) before the teardown"; said=1; }
    sleep 0.5
  done
}
gate_exit_() {
  local rc=$?
  trap '' TERM INT   # a second signal must not cut the teardown short (an `exit` inside this trap would skip the rest)
  exec 1>&7 2>&8     # the gate's own streams again, not the interrupted stage's log
  gate_settle_
  gate_down_
  [ "$GATE_COPIED" = 0 ] || return 0
  GATE_COPIED=1
  say "GATE FAIL  ($((SECONDS - GATE_T0)) s, base $BASE; ended by ${GATE_SIGNAL:+$GATE_SIGNAL, }exit $rc before its verdict; logs $GATE_DIR)"
  gate_copyback_ fail || true
}
if [ "$GATE_MODE" = gate ]; then
  # 0: the shape check. A gate stack whose .env does not have the reference's keys is not a gate: it is
  # refused before anything is built, and nothing is torn down (this gate made nothing).
  stage 0 keys bash "$GS" keys
  if [ -n "$GATE_FAILED" ]; then
    grep -E '^\[gatestack\] keys: (MISSING|EXTRA)' "$GATE_DIR/0-keys.log" | sed 's/^/[gate]   /' | tee -a "$GATE_DIR/gate.log"
    say "GATE FAIL  ($((SECONDS - GATE_T0)) s, base $BASE; the gate stack's .env keys differ from the reference .env's — the gate did not start; logs $GATE_DIR)"
    gate_copyback_ fail
    exit 1
  fi
  # 0: the pins. Every image the stack pulls (the S3 server — compose.e2e-bucket.yaml, RustFS since run 22; its `mc` client
  # went with erratum 346) is pinned by digest and in this box's cache or pullable now, or the gate is refused here, before anything
  # is built (run 21 review: both MinIO pins vanished from every registry, and the E2E died in its first two seconds). Nothing is torn down: this gate made nothing but, at most, a pull.
  stage 0 pins bash "$GS" pins
  if [ -n "$GATE_FAILED" ]; then
    grep -E '^\[gatestack\] pins: (UNAVAILABLE|NOT BY DIGEST)' "$GATE_DIR/0-pins.log" | sed 's/^/[gate]   /' | tee -a "$GATE_DIR/gate.log"
    say "GATE FAIL  ($((SECONDS - GATE_T0)) s, base $BASE; a pinned image the gate stack pulls is not here and not pullable, or not pinned by digest — the gate did not start; logs $GATE_DIR)"
    gate_copyback_ fail
    exit 1
  fi
  # The flavour, said once: a CPU-image gate (GATE_GPU=0, the default on a Mac) is not the gpu proof production needs.
  if [ "$(bash "$GS" flavour 2>/dev/null)" = cpu ]; then
    say "[gate] flavour: cpu — the CPU image (GATE_GPU=0; the default on a Mac). This gate does NOT prove compose.gpu.yaml, TRANSCODE_HWACCEL=cuda,"
    say "[gate]          the NVDEC-refusal classifier or the GPU image's audio path; its record says flavour=cpu. The gpu proof is a gate on the NVIDIA box."
  else
    say "[gate] flavour: gpu — compose.gpu.yaml, as production runs"
  fi
  trap gate_exit_ EXIT
  # A TERM or an INT (the Windows wrapper's relay into WSL, or a Ctrl-C) ends the gate THROUGH the EXIT trap, so
  # the gate stack is torn down and the run's logs reach Windows: without these, bash dies of the signal and skips it.
  trap 'GATE_SIGNAL=TERM; exit 143' TERM; trap 'GATE_SIGNAL=INT; exit 130' INT
fi
# In gate mode check.sh's runtime image is tagged as the gate stack's, so the check never re-tags the
# reference checkout's own image; the tag goes with the stack's teardown.
if [ "$GATE_MODE" = gate ]; then stage 1 check env CHECK_RACE=all CLIPPET_IMAGE="${GATE_PROJECT:-clippet-gate}-app:local" bash scripts/check.sh
else stage 1 check env CHECK_RACE=all bash scripts/check.sh; fi
stage 2 e2e gate_e2e
GATE_VISUAL_DECISION=$(bash "$GS" visual-decision 2>/dev/null || echo "RUN (the decision could not be made)")
case $GATE_VISUAL_DECISION in
  SKIPPED*) GATE_VISUAL_RAN=skipped
    if [ -n "$GATE_FAILED" ] && [ "${GATE_CONTINUE:-0}" != 1 ]; then say "[gate] 3 visual: NOT RUN (an earlier stage failed)"
    else say "[gate] 3 visual: $GATE_VISUAL_DECISION"; fi ;;
  *)
    say "[gate] 3 visual: the decision — $GATE_VISUAL_DECISION"
    GATE_VISUAL_RAN=ran; stage 3 visual gate_visual ;;
esac
gate_down_
GATE_SECS=$((SECONDS - GATE_T0))
if [ -z "$GATE_FAILED" ]; then
  bash "$GS" green "$BASE" "$GATE_VISUAL_RAN" "$GATE_DIR" 2>&1 | tee -a "$GATE_DIR/gate.log" >/dev/null || true
  say "GATE PASS  ($GATE_SECS s, base $BASE, logs $GATE_DIR)"
  GATE_COPIED=1
  gate_copyback_ pass || exit 1   # nothing may be printed after GATE PASS: release-cycle reads the last line
  exit 0
fi
say "GATE FAIL  ($GATE_SECS s, base $BASE; failed:$GATE_FAILED; logs $GATE_DIR)"
GATE_COPIED=1
gate_copyback_ fail
exit 1
