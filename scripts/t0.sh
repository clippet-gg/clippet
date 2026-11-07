#!/usr/bin/env sh
# t0.sh — T0, the every-commit tier (run 9, lane `t0`; DESIGN §20 erratum 331): no stack, no
# network but Docker's, one command, ≤ 60 s on a lane-sized diff once Go's cache is warm. It runs, at once:
#
#   image   the Dockerfile's `testdeps` stage (Go, the static ffmpeg, the font, gcc — no source), under a tag of
#           this run's own that is removed at the end (its layers stay in the build cache)
#   gofmt   `gofmt -l` over every Go file git knows (tracked, or untracked and not ignored); prints its verdict
#   vet     `go vet` and `go vet -tags ffmpeg` on the CHANGED packages: the files `git diff --name-only
#           $T0_BASE...HEAD` names, plus the working tree's own changes and untracked files, each mapped to the
#           nearest directory with a .go file in it (under cmd/ internal/ web/, and a drop-in's own program under
#           scripts/e2e.d/<stem>/ — gofmt already reads every Go file git knows, that one included); ALL packages when that list is empty, when the base is
#           unknown, or when go.mod/go.sum moved; none when only non-Go paths moved
#   test    `go test -tags ffmpeg ./...` — every test, no -race, no -count=1 — through scripts/lib/gocheck.sh,
#           sharded CHECK_JOBS at a time (unset: 8 when the box is quiet — the Docker VM's 1-minute load
#           under a quarter of its CPUs, 8 or more of them — else 4: a lane shares the box; run 10 measured a
#           store+transcode lane diff at 105 s with 4 and 63 s with 8 on a quiet box). The container mounts the
#           checkout READ-ONLY at /src and the named volume clippet-gobuild as Go's cache, so an unchanged
#           package answers `(cached)` — the shards are fixed by the tree (gocheck.sh says why), and check.sh's
#           Go stage plans the same ones
#   stages  scripts/check.sh with CHECK_ONLY=cheap CHECK_DIR=.e2e/t0/check: every stage but the Go one (the grep stages, the E2E
#           self-test, the planner's self-test, the sleep lint — scripts/lib/sleeplint.sh, rule R1 of §20 erratum 331:
#           a bare time.Sleep in the timing-flake packages' tests fails T0 — and T1's own self-test), beside the Go container
#
# One line per step with its seconds, then `T0 PASS` or `T0 FAIL` last; a failed step prints its log after
# the lines. Logs: .e2e/t0/. Not in T0: -race, the windows cross-compile, the runtime image (check.sh / T1).
#
# What it leaves on the box: .e2e/t0/ (its logs, replaced by the next run) and the named volume
# clippet-gobuild (Go's cache, shared with check.sh and the container test loop; created if absent and never
# removed — it is the point). The image tag and the containers are its own and are removed, on any exit.
#
# Knobs: CHECK_JOBS (default: t0_jobs_ below, 8 quiet / 4 busy), T0_BASE (the diff's base, default main).
# `T0_SELFTEST=1 sh scripts/t0.sh` runs the jobs decision against planted readings (no Docker; t1's self-test calls it).
set -u; cd "$(dirname "$0")/.."; export MSYS_NO_PATHCONV=1
# t0_jobs_ <cpus> <1-minute load> → the test step's CHECK_JOBS when the caller set none: 8 when the box is
# quiet (load under a quarter of the CPUs, and 8 or more CPUs), else 4. The reading is the Docker VM's, where
# the shards run; a quiet box has the cores the second four shards need, a busy one does not.
t0_jobs_() { awk -v n="$1" -v l="$2" 'BEGIN { if (n ~ /^[0-9]+$/ && l ~ /^[0-9.]+$/ && n + 0 >= 8 && l + 0 < n / 4) print 8; else print 4 }'; }
if [ "${T0_SELFTEST:-0}" = 1 ]; then
  bad=0
  for c in "16 0.9 8" "16 3.99 8" "16 4.0 4" "16 10.65 4" "8 1.5 8" "8 2 4" "4 0.0 4" "16 x 4" "z 0.1 4"; do
    set -- $c; got=$(t0_jobs_ "$1" "$2")
    [ "$got" = "$3" ] || { echo "  t0 self-test FAIL: t0_jobs_ $1 $2 = $got, want $3"; bad=$((bad + 1)); }
  done
  if [ "$bad" = 0 ]; then echo "  t0 self-test PASS: CHECK_JOBS 8 on a quiet box of 8+ CPUs, 4 under load or on fewer"; exit 0; fi
  exit 1
fi
# IN WSL (DESIGN §20 erratum 338): typed in Git Bash with WSL's clone present, T0 re-runs itself in the WSL
# worktree $HOME/dev/<this checkout's folder> at this checkout's commit — a dirty tree is REFUSED there (WSL tests the
# commit: commit first, or CLIPPET_WSL=0 runs it here with the working tree). scripts/lib/wslexec.sh says how.
. scripts/lib/wslexec.sh
wsl_reexec_ t0 sh scripts/t0.sh "$@"
T0_START=$(date +%s)
D=.e2e/t0
rm -rf "$D"; mkdir -p "$D"
T0_JOBS_GIVEN=${CHECK_JOBS:-}
CHECK_JOBS=${CHECK_JOBS:-4}; export CHECK_JOBS
T0_BASE=${T0_BASE:-main}
IMG="clippet-t0-deps:$$-$(date +%s)"
GOC="clippet-t0-go-$$"
FMC="clippet-t0-fmt-$$"
STG_PID=
T0_SIG=
t0_rm_() { docker rm -f "$GOC" "$FMC" >/dev/null 2>&1; docker rmi -f "$IMG" >/dev/null 2>&1; }
t0_cleanup() {
  [ -z "$STG_PID" ] || kill "$STG_PID" 2>/dev/null || true
  t0_rm_
  # A signal kills the `docker run` CLIENT, not the daemon's create already under way: its container can appear
  # AFTER the first rm (measured 2026-10-18: a TERM 7 s in left a Created clippet-t0-fmt-<pid> behind), so a second
  # pass once the daemon has finished, then a third; a run that ended by itself has nothing in flight.
  if [ -n "$T0_SIG" ]; then sleep 1; t0_rm_; sleep 2; t0_rm_; fi
  return 0
}
trap t0_cleanup EXIT
trap 'T0_SIG=INT; exit 130' INT
trap 'T0_SIG=TERM; exit 143' TERM
FAILED=""
line() {   # line <step> <ok|FAIL|skip> <seconds> <what>
  printf 't0: %-7s %-4s %4s s  %s\n' "$1" "$2" "$3" "$4"
  [ "$2" != FAIL ] || FAILED="$FAILED $1"
}
HOST=$( (pwd -W) 2>/dev/null || pwd )
# On Linux the Go container (root) hands its /out back to this user at exit, so the next run's rm -rf of .e2e/t0
# can remove it (erratum 338); on Git Bash NTFS ignores ownership and nothing is passed.
T0_CHOWN=; case $(uname -s) in Linux) T0_CHOWN="$(id -u):$(id -g)" ;; esac

# ---- stages: check.sh's cheap stages, in the background for the whole run ------------------------------------
STG_T=$(date +%s)
( CHECK_ONLY=cheap CHECK_DIR="$D/check" sh scripts/check.sh > "$D/stages.log" 2>&1; echo "$? $(( $(date +%s) - STG_T ))" > "$D/stages.rc" ) &
STG_PID=$!

# ---- the changed packages ---------------------------------------------------------------------------------
{ git diff --name-only "$T0_BASE...HEAD" 2>/dev/null || echo "@unknown-base"
  git diff --name-only HEAD 2>/dev/null; git ls-files -o --exclude-standard 2>/dev/null; } | sort -u > "$D/changed.txt"
if [ ! -s "$D/changed.txt" ] || grep -qxE '@unknown-base|go\.mod|go\.sum' "$D/changed.txt"; then
  VET=./...; VET_SAY="all packages ($( [ -s "$D/changed.txt" ] && echo "go.mod/go.sum moved or the base $T0_BASE is unknown" || echo "no diff against $T0_BASE"))"
else
  while IFS= read -r f; do
    # scripts/e2e.d/<stem>/ is a drop-in's own Go program (110-autohost's mock): vetted like a product package.
    case $f in cmd/*|internal/*|web/*|scripts/e2e.d/*/*) ;; *) continue ;; esac
    d=$(dirname "$f")
    while :; do
      case $d in .|cmd|internal|web|scripts|scripts/e2e.d) d=; break ;; esac
      if ls "$d"/*.go >/dev/null 2>&1; then break; fi
      d=$(dirname "$d")
    done
    [ -z "$d" ] || echo "./$d"
  done < "$D/changed.txt" | sort -u > "$D/vet-pkgs.txt"
  if [ -s "$D/vet-pkgs.txt" ]; then VET=$(tr '\n' ' ' < "$D/vet-pkgs.txt" | sed 's/ $//'); VET_SAY="$(wc -l < "$D/vet-pkgs.txt" | tr -d ' ') changed package(s): $VET"
  else VET=none; VET_SAY="no Go package changed against $T0_BASE"; fi
fi

# ---- image ------------------------------------------------------------------------------------------------
t=$(date +%s)
if docker build --target testdeps -t "$IMG" . > "$D/image.log" 2>&1; then
  line image ok $(( $(date +%s) - t )) "the testdeps stage (Go, ffmpeg, the font, gcc; no source) as $IMG, removed at exit"
  docker volume inspect clippet-gobuild >/dev/null 2>&1 || docker volume create clippet-gobuild >/dev/null

  # ---- gofmt ----------------------------------------------------------------------------------------------
  t=$(date +%s)
  git ls-files -co --exclude-standard -- '*.go' | while IFS= read -r f; do [ -f "$f" ] && echo "$f"; done > "$D/gofiles.txt"
  docker run --rm --name "$FMC" -v "$HOST:/src:ro" -w /src "$IMG" sh -c 'xargs gofmt -l < .e2e/t0/gofiles.txt' > "$D/gofmt.log" 2>&1
  rc=$?; nf=$(wc -l < "$D/gofiles.txt" | tr -d ' ')
  if [ "$rc" = 0 ] && [ ! -s "$D/gofmt.log" ]; then line gofmt ok $(( $(date +%s) - t )) "$nf files, none needs formatting"
  else line gofmt FAIL $(( $(date +%s) - t )) "gofmt -l lists $(wc -l < "$D/gofmt.log" | tr -d ' ') of $nf file(s) (exit $rc)"; fi

  # ---- vet + test: one container, gocheck.sh --------------------------------------------------------------
  JOBS_SAY="CHECK_JOBS=$CHECK_JOBS (given)"
  if [ -z "$T0_JOBS_GIVEN" ]; then
    set -- $(docker run --rm "$IMG" sh -c 'nproc; cut -d" " -f1 /proc/loadavg' 2>/dev/null | tr '
' ' ')
    if [ "$#" = 2 ]; then CHECK_JOBS=$(t0_jobs_ "$1" "$2"); JOBS_SAY="CHECK_JOBS=$CHECK_JOBS (load $2 on $1 CPUs)"
    else JOBS_SAY="CHECK_JOBS=4 (no load reading)"; fi
    export CHECK_JOBS
  fi
  t=$(date +%s)
  docker run --rm --name "$GOC" -e CHECK_JOBS -e CHECK_RACE= -e CHECK_GOMAXPROCS -e GOCHECK_OUT=/out -e "GOCHECK_VET=$VET" -e GOCHECK_CROSS=0 -e "GOCHECK_CHOWN=$T0_CHOWN" \
    -v "$HOST:/src:ro" -v "$HOST/$D:/out" -v clippet-gobuild:/root/.cache/go-build -w /src \
    "$IMG" sh /src/scripts/lib/gocheck.sh > "$D/gocheck.log" 2>&1
  rc=$?; secs=$(( $(date +%s) - t ))
  if [ "$VET" = none ]; then line vet skip 0 "$VET_SAY"
  else
    vrc=0; vs=0
    for v in vet vet-ffmpeg; do
      set -- $(cat "$D/go/$v.rc" 2>/dev/null || echo "99 0")
      [ "$1" = 0 ] || vrc=$1; [ "$2" -le "$vs" ] || vs=$2
    done
    if [ "$vrc" = 0 ]; then line vet ok "$vs" "go vet, with and without -tags ffmpeg, on $VET_SAY"
    else line vet FAIL "$vs" "go vet failed (exit $vrc) on $VET_SAY"; fi
  fi
  ok=$(grep '^GOCHECK OK' "$D/gocheck.log" | tail -1)
  if [ "$rc" = 0 ] && [ -n "$ok" ]; then
    line test ok "$secs" "$(echo "$ok" | sed 's/^GOCHECK OK ([0-9]* s): //'), the container's wall time; $JOBS_SAY"
  else line test FAIL "$secs" "gocheck.sh failed (exit $rc); the shards' logs are in $D/go/"; fi
else
  line image FAIL $(( $(date +%s) - t )) "docker build --target testdeps failed"
fi

# ---- stages: wait for check.sh ----------------------------------------------------------------------------
wait "$STG_PID" 2>/dev/null; STG_PID=
set -- $(cat "$D/stages.rc" 2>/dev/null || echo "99 0")
if [ "$1" = 0 ]; then line stages ok "$2" "$(tail -1 "$D/stages.log" | sed 's/^\[check\] //')"
else line stages FAIL "$2" "check.sh CHECK_ONLY=cheap exited $1"; fi

# ---- verdict ----------------------------------------------------------------------------------------------
TOTAL=$(( $(date +%s) - T0_START ))
if [ -n "$FAILED" ]; then
  for s in $FAILED; do
    echo "---- $s ----"
    case $s in
      image)  tail -30 "$D/image.log" ;;
      gofmt)  sed 's/^/  needs gofmt: /' "$D/gofmt.log" ;;
      vet|test) grep -v '^\$ ' "$D/gocheck.log" | sed 's/^/  /' ;;
      stages) grep -B2 -A30 'FAILED (exit' "$D/stages.log" | head -80; tail -1 "$D/stages.log" ;;
    esac
  done
  echo "T0 FAIL (${TOTAL} s):$FAILED"
  exit 1
fi
echo "T0 PASS (${TOTAL} s)"
