#!/bin/sh
# wslexec.sh — THE HARNESS RUNS ON WSL (DESIGN §20 erratum 338). On this PC Git Bash spawns one process in ~45 ms
# (500 × /bin/true = 22.8 s, measured 2026-10-18) and WSL2 Ubuntu in ~0.3 ms (the same loop, `i=0; while [ $i -lt 500 ];
# do /bin/true; i=$((i+1)); done` under `time`: 0.160 / 0.160 / 0.163 s, measured 2026-10-18); the E2E, the gate's
# check.sh and two load-sensitive timing pins were all spawn-bound. So scripts/gate.sh (gate mode only),
# scripts/t0.sh and scripts/t1.sh, typed in Git Bash, RE-RUN THEMSELVES inside WSL on the ext4 clone at the SAME
# commit, stream their output, return its status, and (gate.sh) leave the records the Windows side reads exactly
# where it reads them. Docker is the same daemon from either side (Docker Desktop's WSL integration), so the
# stacks, the images, the volumes and the GPU are the same ones.
#
# POSIX sh: t0.sh sources this under dash. Three ways in:
#   . scripts/lib/wslexec.sh; wsl_reexec_ <kind> <interp> <script> [args…]   (Windows side; returns when the run stays here)
#   bash scripts/lib/wslexec.sh wsl-enter …        (WSL side, run by wsl_reexec_ through wsl.exe: never by hand)
#   bash scripts/lib/wslexec.sh wsl-kill <name>    (WSL side: the Windows wrapper's INT/TERM relay)
#   bash scripts/lib/wslexec.sh wsl-teardown [--check] <dir name>   (WSL side: scripts/lane-teardown.sh's WSL worktree)
#   bash scripts/lib/wslexec.sh selftest           (the pins; no wsl.exe, no Docker, scratch git repos only — check.sh's wslexec stage)
#
# WHEN it re-runs (wsl_decide_): Git Bash (MSYSTEM set), CLIPPET_WSL is not 0, not already inside WSL
# (CLIPPET_IN_WSL), wsl.exe on PATH, and WSL's home holds the clone (CLIPPET_WSL_CLONE, default dev/clippet;
# CLIPPET_WSL_DISTRO, default Ubuntu). Otherwise it runs where it was typed, as before, with a note on Git Bash.
#
# THE SAME COMMIT. Windows side: H = HEAD of the invoking checkout, which must be CLEAN (`git status
# --porcelain` empty — refused otherwise: WSL runs the commit, never a working tree). WSL side, under a lock:
# the clone is detached and mirror-fetched from the Windows repo (`+refs/heads/*:refs/heads/*` and the tags, so
# `main` in WSL IS the Windows main — select.sh, T0_BASE/T1_BASE and the visual decision read it), H must be
# visible (refused otherwise: a commit on no branch or tag), and the run directory is detached at H and asserted
# clean. The run directory: the clone itself for the gate ($HOME/dev/clippet inside WSL, CLIPPET_WSL_CLONE); a
# detached worktree of it for t0/t1, $HOME/dev/<the invoking checkout's folder name> (clippet-main for the main
# checkout), made on first use.
#
# THE .env FILES. The reference .env (the Windows MAIN worktree's, the production stack's) is NEVER copied into
# WSL. The gate reads it IN PLACE: wsl-enter exports GATE_REF_ENV=<the main worktree, /mnt/c/…>/.env, which
# gatestack.sh's key-diff and gate-key's env= read by NAME only (so release-cycle --bump-env's edit of the Windows
# file is what the next gate sees, with nothing to sync). The clone's own .env is a GUARD (wsl_guard_env_, checked
# on every run kind, mode 600, no secret in it): COMPOSE_PROJECT_NAME=clippet-never, image tags of that name and
# PUBLIC_BASE_URL=http://refused.invalid — compose.yaml's `name:` is `clippet`, so without a project line a bare
# `docker compose up`, e2e.sh or visual.sh in the WSL clone WOULD be production rebuilt from WSL; with the guard it
# is a project nobody runs, and clippet-app:local is never re-tagged. gate.sh reads the guard's PUBLIC_BASE_URL as
# "this checkout's stack", so its mode stays `gate`. For t0/t1 from a lane checkout, that checkout's .env is copied
# into the WSL worktree when it names a lane project (COMPOSE_PROJECT_NAME=clippet-<lane>, not clippet-dev or
# clippet-gate), mode 600, so T1 builds and tests the lane's own stack. No value is ever printed.
#
# Locks ($HOME/dev/.<name>-run.lock, flock -n: a second run on the same directory is REFUSED) and a short fetch
# lock per clone. The pid of the run is $HOME/dev/.<name>-run.pid; the Windows wrapper relays INT/TERM to its process
# group through `wsl-kill`. Values cross only through WSLENV, by NAME (wsl_envlist_): GATE_*, E2E_*, CHECK_*,
# VISUAL_*, T0_*, T1_*, SELECT_BASE, PW_VERSION, CLIPPET_VERSION, BASE — but never GATE_WIN_ROOT or GATE_REF_ENV,
# which wsl-enter sets itself (a Windows path means nothing in WSL). Nothing else of the Windows environment.
WSL_DISTRO=${CLIPPET_WSL_DISTRO:-Ubuntu}
WSL_CLONE_REL=${CLIPPET_WSL_CLONE:-dev/clippet}

# ---- pure decisions (the self-test pins each) ----------------------------------------------------------------
# wsl_decide_ <MSYSTEM> <CLIPPET_WSL> <CLIPPET_IN_WSL> <wsl.exe on PATH: 1|0> <clone seen: 1|0> → `wsl`, or
# `native (<why>)`.
wsl_decide_() {
  if [ -n "$3" ]; then echo "native (already inside WSL)"; return 0; fi
  if [ -z "$1" ]; then echo "native (not Git Bash)"; return 0; fi
  if [ "$2" = 0 ]; then echo "native (CLIPPET_WSL=0: the Git Bash path)"; return 0; fi
  if [ "$4" != 1 ]; then echo "native (no wsl.exe on PATH)"; return 0; fi
  if [ "$5" != 1 ]; then echo "native (WSL $WSL_DISTRO has no clone at ~/$WSL_CLONE_REL)"; return 0; fi
  echo wsl
}
# wsl_envlist_ < names (one per line) → the WSLENV value: the harness's knobs, by name, sorted, colon-separated.
wsl_envlist_() {
  grep -E '^((GATE|E2E|CHECK|VISUAL|T0|T1)_[A-Za-z0-9_]+|SELECT_BASE|PW_VERSION|CLIPPET_VERSION|BASE)$' \
    | grep -vxE 'GATE_WIN_ROOT|GATE_REF_ENV' | LC_ALL=C sort -u | tr '\n' ':' | sed 's/:$//'
}
# wsl_mnt_ <a Windows path: C:/x, C:\x or Git Bash's /c/x> → WSL's /mnt/c/x.
wsl_mnt_() {
  # awk, not sed's \L (GNU-only: BSD sed on a Mac wrote /mnt/LC/…); the same three rewrites in the same order.
  printf '%s\n' "$1" | awk '{ gsub(/\\/, "/")
    if ($0 ~ /^[A-Za-z]:\//) $0 = "/mnt/" tolower(substr($0, 1, 1)) substr($0, 3)
    else if ($0 ~ /^[A-Za-z]:$/) $0 = "/mnt/" tolower(substr($0, 1, 1))
    else if ($0 ~ /^\/[A-Za-z]\//) $0 = "/mnt/" tolower(substr($0, 2, 1)) substr($0, 3)
    print }'
}
# wsl_rundir_ <kind> <the invoking checkout's folder name> <the clone's folder name> → the run directory's name
# under the clone's parent: the clone for the gate; a worktree of the checkout's own name for t0/t1.
wsl_rundir_() {
  case $1 in gate) echo "$3"; return 0 ;; esac
  case $2 in
    "$3") echo "$3-main" ;;
    "$3"-*) echo "$2" ;;
    *) echo "$3-$2" ;;
  esac
}
# wsl_lane_project_ok_ <COMPOSE_PROJECT_NAME value> → 0 when it is a lane's own project (the .env is copied).
wsl_lane_project_ok_() {
  case $1 in
    clippet|clippet-dev|clippet-gate|'') return 1 ;;
    clippet-*) printf '%s' "$1" | grep -Eq '^clippet-[a-z0-9-]+$' ;;
    *) return 1 ;;
  esac
}
# wsl_record_fresh_ <gate-green.txt> <commit> <start, ISO UTC> → 0 when THIS run wrote the record: its commit= is
# the run's and its proved= is not before the run began (ISO-8601 UTC strings compare as text).
wsl_record_fresh_() {
  [ -s "$1" ] || return 1
  c=$(sed -n 's/^commit=//p' "$1" | tail -1); p=$(sed -n 's/^proved=//p' "$1" | tail -1)
  [ "$c" = "$2" ] || return 1
  [ -n "$p" ] || return 1
  [ "$(printf '%s\n%s\n' "$3" "$p" | LC_ALL=C sort | head -1)" = "$3" ]
}
# wsl_keys_n_ <dotenv> → how many key names it sets (gatestack.sh's recipe; nothing right of `=` is kept).
wsl_keys_n_() { tr -d '\r' < "$1" | sed -n 's/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}\([A-Za-z_][A-Za-z0-9_]*\)=.*/\2/p' | sort -u | grep -c .; }
# wsl_envval_ <dotenv> <KEY> → that key's value, for a DECISION only (never printed by this file).
wsl_envval_() { tr -d '\r"' < "$1" | sed -n "s/^$2=//p" | tail -1; }

# ---- the git half (WSL side; the self-test runs it on scratch repos) ----------------------------------------
wsl_refuse_() { echo "[wsl] REFUSED — $*"; exit 2; }
# wsl_mirror_ <clone> — detach the clone (a checked-out branch is a ref the mirror may not move) and make every
# branch and tag the Windows repo's, pruning what it no longer has. Serialised per clone by a short lock.
wsl_mirror_() {
  (
    if command -v flock >/dev/null 2>&1; then flock -w 600 8 || { echo "[wsl] the fetch lock of $1 was held for 10 min"; exit 3; }; fi
    git -C "$1" checkout -q --detach 2>/dev/null || true
    git -C "$1" fetch -q --prune origin '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*' 2>&1 || exit 1
  ) 8>"$(dirname "$1")/.$(basename "$1")-fetch.lock"
}
# wsl_worktree_ <clone> <run dir> <commit> — the run directory as a DETACHED worktree of the clone, made on first use.
wsl_worktree_() {
  if [ -e "$2/.git" ]; then
    common=$(cd "$2" && cd "$(git rev-parse --git-common-dir)" && pwd -P)
    [ "$common" = "$(cd "$1/.git" && pwd -P)" ] || { echo "$2 is a git checkout but not a worktree of $1"; return 1; }
    return 0
  fi
  [ ! -e "$2" ] || { echo "$2 exists and is not a git worktree"; return 1; }
  git -C "$1" worktree add -q --detach "$2" "$3" 2>&1
}
# wsl_at_ <run dir> <commit> — the commit is visible, the directory is clean, and HEAD is exactly that commit.
wsl_at_() {
  git -C "$1" cat-file -e "$2^{commit}" 2>/dev/null \
    || { echo "the WSL clone cannot see $2 (fetched from the Windows repo: is the commit on a branch or a tag there?)"; return 1; }
  d=$(git -C "$1" status --porcelain 2>/dev/null | head -5 | tr '\n' ' ')
  [ -z "$d" ] || { echo "the WSL checkout $1 has local changes: $d(it is scratch: git -C $1 status)"; return 1; }
  git -C "$1" checkout -q --detach "$2" 2>&1 || return 1
  [ "$(git -C "$1" rev-parse HEAD)" = "$(git -C "$1" rev-parse "$2^{commit}")" ] || { echo "$1 is not at $2 after the checkout"; return 1; }
  [ -z "$(git -C "$1" status --porcelain 2>/dev/null | head -1)" ] || { echo "$1 is not clean after the checkout"; return 1; }
}
# wsl_ref_env_ <reference .env (Windows, over /mnt/c)> — the gate's: says where it is read and how many key names it
# sets (never a value); the gate exports GATE_REF_ENV to it. Nothing is copied.
wsl_ref_env_() {
  if [ -f "$1" ]; then echo "[wsl] the reference .env: read in place at $1 ($(wsl_keys_n_ "$1") key names; GATE_REF_ENV), never copied"
  else echo "[wsl] the reference .env: none at $1 (the gate's key check will refuse)"; fi
}
# wsl_guard_text_ — the clone's .env: no secret, and a project, image tags and origin nobody runs.
wsl_guard_text_() {
  printf '%s\n' \
    "# GUARD, written by scripts/lib/wslexec.sh (DESIGN §20 erratum 338). This WSL clone is the harness's scratch" \
    "# checkout, never a stack: the gate reads the reference .env in place (GATE_REF_ENV, names only) and builds its own" \
    "# project through gatestack.sh. compose.yaml's name: is clippet (production), so these lines keep a bare docker" \
    "# compose, e2e.sh or visual.sh here off it and off its image tags. Rewritten on every harness run; put nothing here." \
    "COMPOSE_PROJECT_NAME=clippet-never" \
    "CLIPPET_IMAGE=clippet-never-app:local" \
    "CLIPPET_GPU_IMAGE=clippet-never-app:gpu" \
    "PUBLIC_BASE_URL=http://refused.invalid"
}
# wsl_guard_env_ <the clone's .env> — make it the guard (whatever it held: an older harness copied the reference
# there), mode 600 either way. Says so only when it changed something; never prints what was there.
wsl_guard_env_() {
  gt=$(dirname "$1")/.env.wsl-guard.$$   # ignored by .gitignore's .env.*; one per run, so two runs never share it
  ( umask 077; wsl_guard_text_ > "$gt" ) || { echo "[wsl] could not write the guard .env"; return 1; }
  if cmp -s "$gt" "$1"; then rm -f "$gt"
  else
    gw="none"; [ ! -f "$1" ] || gw="$(wsl_keys_n_ "$1") key names"
    mv -f "$gt" "$1" || { rm -f "$gt"; echo "[wsl] could not replace $1 with the guard"; return 1; }
    echo "[wsl] the clone's .env is the guard now (project clippet-never; it held: $gw)"
  fi
  chmod 600 "$1"
}
# wsl_lane_env_ <the invoking checkout (WSL path)> <the main checkout (WSL path)> <run dir> — a lane's .env into its worktree.
wsl_lane_env_() {
  [ "$1" != "$2" ] || return 0                     # the main checkout's .env IS the reference: never copied into a run dir
  if [ ! -f "$1/.env" ]; then
    [ ! -f "$3/.env" ] || { rm -f "$3/.env"; echo "[wsl] the lane .env: gone on Windows, removed from $3"; }
    return 0
  fi
  if ! wsl_lane_project_ok_ "$(wsl_envval_ "$1/.env" COMPOSE_PROJECT_NAME)"; then
    echo "[wsl] the lane .env: NOT copied (its COMPOSE_PROJECT_NAME is not a lane's own clippet-<lane>)"; return 0
  fi
  if cmp -s "$1/.env" "$3/.env"; then chmod 600 "$3/.env"; echo "[wsl] the lane .env: unchanged ($(wsl_keys_n_ "$1/.env") key names)"; return 0; fi
  ( umask 077; cp "$1/.env" "$3/.env.wsl-tmp" ) && mv -f "$3/.env.wsl-tmp" "$3/.env" && chmod 600 "$3/.env" && echo "[wsl] the lane .env: copied ($(wsl_keys_n_ "$3/.env") key names)"
}
# wsl_copyback_ <Windows checkout> <gate dir, relative> <record or empty> [visual dir] — the gate's LAST act in WSL:
# the dir first, then the record that names it. SILENT on success (release-cycle reads `tail -1` = GATE PASS);
# a failure says so on stderr and returns 1.
wsl_copyback_() {
  [ -d "$1" ] || { echo "[wsl] copy-back: the Windows checkout $1 is not there" >&2; return 1; }
  case $2 in /*|'') echo "[wsl] copy-back: GATE_DIR '$2' is not relative to the checkout; its logs stay in WSL" >&2; return 1 ;; esac
  mkdir -p "$1/$2" && cp -R "$2/." "$1/$2/" || { echo "[wsl] copy-back: could not copy $2 to $1" >&2; return 1; }
  if [ -n "${4:-}" ] && [ -d "$4" ]; then
    rm -rf "${1:?}/$4" && mkdir -p "$1/$4" && cp -R "$4/." "$1/$4/" || { echo "[wsl] copy-back: could not copy $4 to $1" >&2; return 1; }
  fi
  [ -n "$3" ] || return 0
  mkdir -p "$1/.e2e/checkpoints" && cp "$3" "$1/.e2e/checkpoints/gate-green.txt.wsl-tmp" \
    && mv -f "$1/.e2e/checkpoints/gate-green.txt.wsl-tmp" "$1/.e2e/checkpoints/gate-green.txt" \
    || { echo "[wsl] copy-back: could not write $1/.e2e/checkpoints/gate-green.txt" >&2; return 1; }
}

# ---- the Windows side ------------------------------------------------------------------------------------------
wsl_exe_() { MSYS_NO_PATHCONV=1 wsl.exe -d "$WSL_DISTRO" --cd "~" --exec "$@"; }
# wsl_reexec_ <kind gate|t0|t1> <interp> <script, relative to the checkout> [args…] — re-run inside WSL and EXIT with
# its status, or return (and the caller runs here).
wsl_reexec_() {
  wk=$1 wi=$2 ws=$3; shift 3
  wh=0; command -v wsl.exe >/dev/null 2>&1 && wh=1
  wd=$(wsl_decide_ "${MSYSTEM:-}" "${CLIPPET_WSL:-auto}" "${CLIPPET_IN_WSL:-}" "$wh" 1)
  if [ "$wd" = wsl ]; then
    wc=0; wsl_exe_ test -d "$WSL_CLONE_REL/.git" >/dev/null 2>&1 && wc=1
    wd=$(wsl_decide_ "${MSYSTEM:-}" "${CLIPPET_WSL:-auto}" "${CLIPPET_IN_WSL:-}" "$wh" "$wc")
  fi
  if [ "$wd" != wsl ]; then
    case $wd in "native (not Git Bash)"|"native (already inside WSL)") ;; *) echo "[wsl] $wk runs here on Git Bash: ${wd#native }" ;; esac
    # One gate stack on the box: a Git Bash gate refuses while a WSL gate holds the clone.
    if [ "$wk" = gate ] && [ "$wh" = 1 ] && [ -n "${MSYSTEM:-}" ] && [ -z "${CLIPPET_IN_WSL:-}" ] \
       && wsl_exe_ test -d "$WSL_CLONE_REL/.git" >/dev/null 2>&1 \
       && ! wsl_exe_ flock -n "$(dirname "$WSL_CLONE_REL")/.$(basename "$WSL_CLONE_REL")-run.lock" true >/dev/null 2>&1; then
      echo "[wsl] REFUSED — a gate is running in WSL (~/$WSL_CLONE_REL holds its lock): one gate stack at a time"; exit 2
    fi
    return 0
  fi
  wH=$(git rev-parse HEAD 2>/dev/null) || wsl_refuse_ "this checkout has no commit"
  wD=$(git status --porcelain 2>/dev/null | head -5 | tr '\n' ' ')
  [ -z "$wD" ] || wsl_refuse_ "this checkout has local changes ($wD…): $wk in WSL runs the COMMIT, never a working tree — commit first, or CLIPPET_WSL=0 runs it here on Git Bash"
  wW=$( (pwd -W) 2>/dev/null || pwd ); wM=$(wsl_mnt_ "$wW")
  wR=$(git worktree list --porcelain 2>/dev/null | sed -n '1s/^worktree //p' | tr -d '\r'); wR=$(wsl_mnt_ "${wR:-$wW}")
  wN=$(wsl_rundir_ "$wk" "$(basename "$wW")" "$(basename "$WSL_CLONE_REL")")
  WSLENV=$(env | sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' | wsl_envlist_); export WSLENV
  echo "[wsl] $wk runs in WSL ($WSL_DISTRO, ~/$(dirname "$WSL_CLONE_REL")/$wN) at $(git rev-parse --short HEAD); CLIPPET_WSL=0 runs it here on Git Bash"
  # The relay: the wrapper waits on a BACKGROUND wsl.exe so a trapped INT/TERM interrupts the wait, sends TERM to
  # the WSL run's process group (its own EXIT traps then run: the gate stack's teardown), and waits for the end.
  wP=
  trap 'echo "[wsl] interrupted: relaying TERM to the WSL run"; wsl_exe_ bash "$wM/scripts/lib/wslexec.sh" wsl-kill "$wN" || true' INT TERM
  wsl_exe_ bash -l "$wM/scripts/lib/wslexec.sh" wsl-enter "$wk" "$wH" "$wM" "$wR" "$wi" "$ws" "$@" &
  wP=$!
  wrc=0; wait "$wP" || wrc=$?
  while kill -0 "$wP" 2>/dev/null; do wrc=0; wait "$wP" || wrc=$?; done
  exit "$wrc"
}

# ---- the WSL side ------------------------------------------------------------------------------------------------
# wsl-enter <kind> <commit> <the invoking checkout, WSL path> <the main checkout, WSL path> <interp> <script> [args…]
wsl_enter_() {
  ek=$1 eH=$2 eW=$3 eR=$4 ei=$5 es=$6; shift 6
  CLIPPET_IN_WSL=1; GATE_WIN_ROOT=$eW; export CLIPPET_IN_WSL GATE_WIN_ROOT
  eC=$HOME/$WSL_CLONE_REL; eV=$(dirname "$eC")
  command -v flock >/dev/null 2>&1 || wsl_refuse_ "no flock in WSL (util-linux)"
  docker info >/dev/null 2>&1 || wsl_refuse_ "docker does not answer inside WSL (is Docker Desktop up, with its WSL integration for $WSL_DISTRO on?)"
  eN=$(wsl_rundir_ "$ek" "$(basename "$eW")" "$(basename "$eC")"); eD=$eV/$eN
  exec 9>"$eV/.$eN-run.lock"
  flock -n 9 || wsl_refuse_ "another harness run holds $eD (pid $(cat "$eV/.$eN-run.pid" 2>/dev/null || echo ?)): one run per directory"
  ef=$(wsl_mirror_ "$eC") || wsl_refuse_ "the mirror fetch from the Windows repo failed: $ef"
  if [ "$eD" != "$eC" ]; then eo=$(wsl_worktree_ "$eC" "$eD" "$eH") || wsl_refuse_ "$eo"; fi
  eo=$(wsl_at_ "$eD" "$eH") || wsl_refuse_ "$eo"
  # The clone's .env is the guard on EVERY run kind (a t0/t1 run is the first to replace an older copy of the
  # reference); only the gate reads the reference, in place, and only t0/t1 copy a lane's own .env.
  wsl_guard_env_ "$eC/.env" || exit 2
  if [ "$ek" = gate ]; then
    wsl_ref_env_ "$eR/.env"; GATE_REF_ENV=$eR/.env; export GATE_REF_ENV
  else
    wsl_lane_env_ "$eW" "$eR" "$eD"
  fi
  cd "$eD" || exit 2
  # The run is a process group of its own (setsid), whose leader writes its pid: wsl-kill signals that group and
  # nothing else (not wsl.exe's relay, whose death would close the run's stdout under its EXIT traps). -w: the
  # exit status is the run's. The lock (fd 9) is inherited, so it is held until the run ends.
  exec setsid -w sh -c 'echo "$$" > "$1"; shift; exec "$@"' sh "$eV/.$eN-run.pid" "$ei" "$es" "$@"
}
# wsl-kill <name> — TERM to the process group of the run that holds $HOME/dev/.<name>-run.lock (nothing when none does).
wsl_kill_() {
  kV=$(dirname "$HOME/$WSL_CLONE_REL")
  if flock -n "$kV/.$1-run.lock" true 2>/dev/null; then echo "[wsl] no run holds $kV/.$1-run.lock"; return 0; fi
  kp=$(cat "$kV/.$1-run.pid" 2>/dev/null); [ -n "$kp" ] || return 0
  kg=$(ps -o pgid= -p "$kp" 2>/dev/null | tr -d ' ')
  # the leader of its own group (wsl-enter's setsid) gets the group signal; anything else, the pid alone
  if [ "$kg" = "$kp" ]; then kill -TERM -- "-$kp" 2>/dev/null; else kill -TERM "$kp" 2>/dev/null; fi
  echo "[wsl] TERM sent to the run (pid $kp, group ${kg:-?})"
}
# wsl-teardown [--check] <dir name> — the WSL worktree of a lane: refused while a run holds it; its root-owned dirs
# (a container's writes) removed through busybox, then `git worktree remove --force` and prune; its lock/pid files.
wsl_teardown_() {
  tc=0; [ "${1:-}" != --check ] || { tc=1; shift; }
  tC=$HOME/$WSL_CLONE_REL; tV=$(dirname "$tC"); tD=$tV/$1
  case $1 in "$(basename "$tC")"-*) ;; *) echo "REFUSED: '$1' is not a lane worktree name of $(basename "$tC")"; return 1 ;; esac
  if [ ! -e "$tD" ]; then echo "WSL: no worktree (already gone): ~/${tD#"$HOME"/}"; return 0; fi
  [ -e "$tD/.git" ] && eo=$(wsl_worktree_ "$tC" "$tD" HEAD) || { echo "REFUSED: WSL $tD is not a worktree of $tC ${eo:-}"; return 1; }
  if ! flock -n "$tV/.$1-run.lock" true 2>/dev/null; then echo "REFUSED: a harness run holds WSL $tD"; return 1; fi
  if [ "$tc" = 1 ]; then echo "WSL: OK ~/${tD#"$HOME"/} (a detached worktree of ~/$WSL_CLONE_REL, no run in it)"; return 0; fi
  if ! rm -rf "$tD/.e2e" 2>/dev/null; then
    docker run --rm -v "$tD:/w" busybox rm -rf /w/.e2e >/dev/null 2>&1 || { echo "WSL: could not remove $tD/.e2e (root-owned)"; return 1; }
  fi
  git -C "$tC" worktree remove --force "$tD" 2>/dev/null || {
    docker run --rm -v "$tD:/w" busybox chown -R "$(id -u):$(id -g)" /w >/dev/null 2>&1
    git -C "$tC" worktree remove --force "$tD" || return 1
  }
  git -C "$tC" worktree prune
  rm -f "$tV/.$1-run.lock" "$tV/.$1-run.pid"
  echo "WSL worktree removed: ~/${tD#"$HOME"/}"
}

# ---- the self-test ------------------------------------------------------------------------------------------------
wsl_selftest_() {
  # Its scratch repositories are host paths handed to git: on Git Bash, git.exe needs MSYS's path conversion, which
  # check.sh and t0.sh turn off for themselves (MSYS_NO_PATHCONV=1) — measured: `git -C /tmp/…` then fails.
  unset MSYS_NO_PATHCONV
  st=$(mktemp -d); sn=0; sbad=0
  ok_() { sn=$((sn + 1)); [ "$1" = "$2" ] || { sbad=$((sbad + 1)); echo "[wslexec selftest] FAIL: $3 (got '$1', want '$2')"; }; }
  # 1. when it re-runs
  ok_ "$(wsl_decide_ MINGW64 auto '' 1 1)" wsl "Git Bash, wsl.exe and the clone: re-run in WSL"
  ok_ "$(wsl_decide_ MINGW64 0 '' 1 1)" "native (CLIPPET_WSL=0: the Git Bash path)" "CLIPPET_WSL=0 stays on Git Bash"
  ok_ "$(wsl_decide_ '' auto '' 1 1)" "native (not Git Bash)" "Linux (no MSYSTEM) runs natively"
  ok_ "$(wsl_decide_ MINGW64 auto 1 1 1)" "native (already inside WSL)" "CLIPPET_IN_WSL=1 never re-runs (no recursion)"
  ok_ "$(wsl_decide_ MINGW64 auto '' 0 1)" "native (no wsl.exe on PATH)" "no wsl.exe stays on Git Bash"
  ok_ "$(wsl_decide_ MINGW64 1 '' 1 0 | cut -c1-24)" "native (WSL Ubuntu has n" "no clone in WSL stays on Git Bash"
  # 2. what crosses: the harness knobs by name, never the rest
  ok_ "$(printf 'GATE_RELEASE\nE2E_RESUME\nSECRET_Z\nCHECK_RACE\nVISUAL_ONLY\nT1_WHY\nT0_BASE\nSELECT_BASE\nBASE\nPW_VERSION\nCLIPPET_VERSION\nMSYS_NO_PATHCONV\nPUBLIC_BASE_URL\nGATE_WIN_ROOT\nGATE_REF_ENV\nBASEX\nE2E_\nPATH\n' | wsl_envlist_)" \
    "BASE:CHECK_RACE:CLIPPET_VERSION:E2E_RESUME:GATE_RELEASE:PW_VERSION:SELECT_BASE:T0_BASE:T1_WHY:VISUAL_ONLY" "WSLENV lists the harness knobs only"
  # 3. paths
  ok_ "$(wsl_mnt_ 'C:/src/alice/clippet')" /mnt/c/src/alice/clippet "C:/ → /mnt/c/"
  ok_ "$(wsl_mnt_ 'D:\x\y')" /mnt/d/x/y "D:\\ → /mnt/d/"
  ok_ "$(wsl_mnt_ /c/src/alice)" /mnt/c/src/alice "Git Bash's /c/ → /mnt/c/"
  # 4. the run directory
  ok_ "$(wsl_rundir_ gate clippet-wslgate clippet)" clippet "the gate runs in the clone itself"
  ok_ "$(wsl_rundir_ t1 clippet-wslgate clippet)" clippet-wslgate "t1 of a lane runs in the WSL dev folder's clippet-<lane>"
  ok_ "$(wsl_rundir_ t0 clippet clippet)" clippet-main "t0 of the main checkout runs in clippet-main, never in the gate's clone"
  ok_ "$(wsl_rundir_ t0 other clippet)" clippet-other "a checkout of another name gets clippet-<name>"
  # 5. a lane .env is copied only for a lane project
  for p in clippet clippet-dev clippet-gate '' other 'clippet-A' 'clippet-x y'; do wsl_lane_project_ok_ "$p" && ok_ "copied" "refused" "project '$p'"; done
  wsl_lane_project_ok_ clippet-wslgate || ok_ refused copied "project clippet-wslgate"
  # 6. the record this run wrote
  printf 'commit=aaa\nproved=2026-10-18T21:00:05Z\n' > "$st/rec"
  wsl_record_fresh_ "$st/rec" aaa 2026-10-18T21:00:00Z && r=fresh || r=stale; ok_ "$r" fresh "a record of this commit proved after the start is this run's"
  wsl_record_fresh_ "$st/rec" aaa 2026-10-18T21:00:06Z && r=fresh || r=stale; ok_ "$r" stale "a record proved before the start is an older gate's"
  wsl_record_fresh_ "$st/rec" bbb 2026-10-18T20:00:00Z && r=fresh || r=stale; ok_ "$r" stale "a record of another commit is not this run's"
  wsl_record_fresh_ "$st/none" aaa 2026-10-18T20:00:00Z && r=fresh || r=stale; ok_ "$r" stale "no record is not a record"
  # 7. the same commit, on scratch repos: win (the Windows repo), its lane worktree, and wsl (a clone of it)
  (
    set -e; cd "$st"
    git init -q -b main win; cd win; git config user.email t@t; git config user.name t
    echo a > f; git add f; git commit -qm A
    git branch lane; git worktree add -q ../win-lane lane; cd ../win-lane; echo b > g; git add g; git commit -qm B; cd ..
    git clone -q win wsl 2>/dev/null; git -C wsl config user.email t@t; git -C wsl config user.name t
    git -C win commit -q --allow-empty -m C
  ) > "$st/git.log" 2>&1 || echo "[wslexec selftest] scratch repos: $(cat "$st/git.log")"
  B=$(git -C "$st/win-lane" rev-parse HEAD); C=$(git -C "$st/win" rev-parse HEAD)
  out=$(wsl_mirror_ "$st/wsl" 2>&1) && r=ok || r="failed: $out"; ok_ "$r" ok "the mirror fetch"
  ok_ "$(git -C "$st/wsl" rev-parse main)" "$C" "…makes WSL's main the Windows main (it had moved on)"
  ok_ "$(git -C "$st/wsl" rev-parse lane 2>/dev/null)" "$B" "…and brings the lane branch"
  out=$(wsl_at_ "$st/wsl" "$B" 2>&1) && r=ok || r="refused: $out"; ok_ "$r" ok "a visible commit: the clone detached at it"
  ok_ "$(git -C "$st/wsl" rev-parse HEAD)" "$B" "…HEAD is the commit"
  out=$(wsl_at_ "$st/wsl" 0123456789abcdef0123456789abcdef01234567 2>&1) && r=ok || r=refused; ok_ "$r" refused "a commit the clone cannot see is REFUSED"
  ok_ "$(printf '%s' "$out" | grep -c 'cannot see')" 1 "…by that reason"
  echo dirty > "$st/wsl/f"
  out=$(wsl_at_ "$st/wsl" "$B" 2>&1) && r=ok || r=refused; ok_ "$r" refused "a dirty WSL checkout is REFUSED"
  git -C "$st/wsl" checkout -q -- f
  out=$(wsl_worktree_ "$st/wsl" "$st/wsl-lane" "$B" 2>&1) && r=ok || r="failed: $out"; ok_ "$r" ok "a t1 run directory is a detached worktree, made on first use"
  ok_ "$(git -C "$st/wsl-lane" rev-parse HEAD)/$(git -C "$st/wsl-lane" symbolic-ref -q HEAD || echo detached)" "$B/detached" "…detached at the commit"
  out=$(wsl_worktree_ "$st/wsl" "$st/wsl-lane" "$B" 2>&1) && r=ok || r="failed: $out"; ok_ "$r" ok "…and reused when it is there"
  ( cd "$st/win-lane" && echo x > untracked.txt ) 2>/dev/null
  # the Windows-side refusal: the dirty test wsl_reexec_ makes, on the lane checkout
  ok_ "$( [ -n "$(git -C "$st/win-lane" status --porcelain | head -1)" ] && echo refused || echo ran)" refused "an untracked file is a dirty invoking tree"
  # 8. the .env files: the reference read in place (names only), the clone's .env the guard, a lane's copied 600
  printf 'A=never-printed-value\nB=2\n' > "$st/ref.env"
  out=$(wsl_ref_env_ "$st/ref.env" 2>&1); ok_ "$(printf '%s' "$out" | grep -c 'read in place at .* (2 key names; GATE_REF_ENV), never copied')" 1 "the gate's reference .env is read in place, by a count of names"
  ok_ "$(printf '%s' "$out" | grep -c never-printed)" 0 "…and no value is printed"
  # (stat's mode only on Linux: NTFS under Git Bash reports what MSYS emulates, not what chmod asked for)
  md_() { case $(uname -s) in Linux) stat -c %a "$1" 2>/dev/null || echo none ;; *) echo 600 ;; esac; }
  ok_ "$(grep -c '^wsl_env_sync_()' "$0")/$(sed -n '/^wsl_enter_()/,/^}/p' "$0" | grep -c 'cp ')" 0/0 "nothing in wsl-enter copies the reference .env (the old sync is gone)"
  ok_ "$(sed -n '/^wsl_enter_()/,/^}/p' "$0" | grep -c 'GATE_REF_ENV=\$eR/.env; export GATE_REF_ENV')" 1 "…the gate exports GATE_REF_ENV=<the Windows main worktree>/.env instead"
  ok_ "$(sed -n '/^wsl_enter_()/,/^}/p' "$0" | grep -n 'wsl_guard_env_\|\[ "\$ek" = gate \]' | head -1 | grep -c wsl_guard_env_)" 1 "…and writes the guard before it decides the kind (every kind, not the gate alone)"
  cp "$st/ref.env" "$st/clone.env"; chmod 755 "$st/clone.env"
  out=$(wsl_guard_env_ "$st/clone.env" 2>&1); ok_ "$(printf '%s' "$out" | grep -c 'the guard now (project clippet-never; it held: 2 key names)')" 1 "an old copy of the reference becomes the guard"
  ok_ "$(printf '%s' "$out" | grep -c never-printed)/$(grep -c never-printed "$st/clone.env")" 0/0 "…its values neither printed nor kept"
  ok_ "$(sed -n 's/^COMPOSE_PROJECT_NAME=//p;s/^CLIPPET_IMAGE=//p;s/^CLIPPET_GPU_IMAGE=//p;s/^PUBLIC_BASE_URL=//p' "$st/clone.env" | tr '\n' ' ')" "clippet-never clippet-never-app:local clippet-never-app:gpu http://refused.invalid " "…a project, two image tags and an origin nobody runs"
  ok_ "$(md_ "$st/clone.env")" 600 "…mode 600"
  chmod 755 "$st/clone.env"
  out=$(wsl_guard_env_ "$st/clone.env" 2>&1); ok_ "$out/$(md_ "$st/clone.env")" "/600" "an equal guard is left alone, silently, and still made 600"
  rm -f "$st/clone.env"; out=$(wsl_guard_env_ "$st/clone.env" 2>&1); ok_ "$(printf '%s' "$out" | grep -c 'it held: none')" 1 "a missing .env gets the guard"
  ok_ "$(ls -a "$st" | grep -c 'wsl-guard')" 0 "…and no temporary file is left"
  mkdir -p "$st/lane" "$st/run"; printf 'COMPOSE_PROJECT_NAME=clippet-x\nK=never-printed-value\n' > "$st/lane/.env"
  out=$(wsl_lane_env_ "$st/lane" "$st/main" "$st/run" 2>&1); ok_ "$(cmp -s "$st/lane/.env" "$st/run/.env" && echo copied)" copied "a lane .env is copied into its worktree"
  ok_ "$(printf '%s' "$out" | grep -c never-printed)" 0 "…silently as to its values"
  chmod 755 "$st/run/.env"; out=$(wsl_lane_env_ "$st/lane" "$st/main" "$st/run" 2>&1)
  ok_ "$(printf '%s' "$out" | grep -c unchanged)/$(md_ "$st/run/.env")" 1/600 "an equal lane copy is made 600 too"
  printf 'COMPOSE_PROJECT_NAME=clippet-gate\n' > "$st/lane/.env"
  out=$(wsl_lane_env_ "$st/lane" "$st/main" "$st/run" 2>&1); ok_ "$(printf '%s' "$out" | grep -c 'NOT copied')" 1 "a .env naming the gate project is not copied"
  rm -f "$st/run/.env"; out=$(wsl_lane_env_ "$st/main" "$st/main" "$st/run" 2>&1); ok_ "$( [ -f "$st/run/.env" ] && echo copied || echo none)" none "the main checkout's .env (the reference) is never copied into a run directory"
  # 9. the copy-back: silent, the dir then the record
  mkdir -p "$st/g/.e2e/gate-x" "$st/w"; ( cd "$st/g" && printf 'GATE PASS\n' > .e2e/gate-x/gate.log && printf 'commit=aaa\n' > rec )
  out=$(cd "$st/g" && wsl_copyback_ "$st/w" .e2e/gate-x rec 2>&1); ok_ "$out" "" "the copy-back prints nothing on success"
  ok_ "$(tail -1 "$st/w/.e2e/gate-x/gate.log" 2>/dev/null)/$(cat "$st/w/.e2e/checkpoints/gate-green.txt" 2>/dev/null)" "GATE PASS/commit=aaa" "…the gate dir and the record arrive"
  rm -rf "$st/w2"; mkdir -p "$st/w2"; out=$(cd "$st/g" && wsl_copyback_ "$st/w2" .e2e/gate-x '' 2>&1)
  ok_ "$( [ -f "$st/w2/.e2e/gate-x/gate.log" ] && [ ! -e "$st/w2/.e2e/checkpoints/gate-green.txt" ] && echo dir-only)" dir-only "a failed gate copies the dir only"
  out=$(cd "$st/g" && wsl_copyback_ "$st/absent" .e2e/gate-x rec 2>/dev/null) && r=ok || r=failed; ok_ "$r" failed "a copy-back that cannot write fails (the gate then exits 1)"
  # 10. gate.sh refuses the own/foreign modes inside WSL, before it makes anything
  root=$(cd "$(dirname "$0")/../.." && pwd)
  out=$(cd "$root" && CLIPPET_IN_WSL=1 BASE=http://foreign.invalid:1 GATE_DIR="$st/gd" bash scripts/gate.sh 2>&1); r=$?
  ok_ "$r/$(printf '%s' "$out" | grep -c 'REFUSED')/$( [ -e "$st/gd" ] && echo made || echo none)" "2/1/none" "gate.sh in WSL refuses a non-gate BASE, before it makes its log dir"
  rm -rf "$st"
  if [ "$sbad" = 0 ]; then echo "[wslexec selftest] PASS ($sn checks: when it re-runs and when it stays; WSLENV carries the harness knobs only; the paths; the run directory; the reference .env read in place and never copied, the clone's .env the guard (mode 600), a lane .env copied for a lane project only, values never printed; this run's record only; the mirror fetch, a visible commit detached clean, an invisible one and a dirty clone refused; the silent copy-back; gate.sh's own/foreign refusal in WSL)"; return 0; fi
  echo "[wslexec selftest] FAILED ($sbad of $sn)"; return 1
}

case ${0##*/} in
  wslexec.sh)
    v=${1:-}; [ "$#" = 0 ] || shift
    case $v in
      wsl-enter) wsl_enter_ "$@" ;;
      wsl-kill) wsl_kill_ "$@" ;;
      wsl-teardown) wsl_teardown_ "$@" ;;
      selftest) wsl_selftest_ ;;
      *) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
    esac ;;
esac
