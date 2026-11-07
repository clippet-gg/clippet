#!/usr/bin/env bash
# t1.sh — T1, the lane tier (run 9, lane `tiers`; DESIGN §20 erratum 331).
# ONE command a lane runs ONCE at its end, sized to its diff, on the lane's OWN loopback stack (the stack this
# checkout's .env names), ≤ 5 min for a lane-sized diff. In order, one line per step with its seconds:
#
#   select  scripts/select.sh over the diff (`git diff --name-only $T1_BASE...HEAD`, plus what is changed and
#           not committed): the E2E value (T2 | all | none | a,b,c) and the Go packages. T2 STOPS here — "this
#           diff needs T2: <path>" — because a shape path (compose, the Dockerfile, a migration, internal/config,
#           internal/store, serve.go, the harness core) is owed the full gate and no slice of it is a proof
#   t0      scripts/t0.sh (gofmt, vet on the changed packages, `go test -tags ffmpeg ./...` from Go's cache, the
#           cheap check.sh stages — the sleep lint and the E2E self-test among them), with the same base
#   race    `go test -race` over the changed packages that own goroutines: select.sh's package list ∩
#           scripts/lib/gocheck.sh's GOCHECK_RACE_DEFAULT, through gocheck.sh with CHECK_RACE=<that list>
#           (skipped, and said, when the intersection is empty)
#   tagged  the ffmpeg-tagged tests of the changed packages: t0's one pass is `-tags ffmpeg`, so they ran there;
#           with a race pass, gocheck's complementary plan runs a raced package's tagged-only names again in its
#           `-tags ffmpeg` half (the line says which)
#   cross   the windows/amd64 cross-compile, whenever a Go file, go.mod or go.sum changed (≈ 6 s warm: run 10)
#   e2e     E2E_ONLY=<the selection> bash scripts/e2e.sh on the lane stack (step 1 does its own up -d --build);
#           `all` runs the WHOLE suite after a printed warning and its estimate; `none` skips
#   visual  scripts/visual.sh with VISUAL_ONLY=<the phases of the touched pages> when web/ or internal/web/
#           changed (the map is t1_phases_ below; a shared file — app.css, app.js, base.html, a font, internal/web
#           — is every phase, i.e. the whole pass, with a warning)
#   down    the lane stack, `docker compose down -v`, ONLY when t1 found it down and brought it up (T1_KEEP=1
#           leaves it up), and the .env's CLIPPET_IMAGE tag when this run built it; a stack that was already running is
#           left exactly as found
#
# `T1 PASS` or `T1 FAIL` is the last line; a failed step prints its log's tail after the lines. Logs: .e2e/t1/.
#
# It REFUSES (exit 2, before anything runs) when the compose project is `clippet`, `clippet-dev` or `clippet-gate`
# — the reference stack, the dev stack, the gate stack — and when the stack's PUBLIC_BASE_URL is
# not a loopback origin of the lane's own (a checkout with no .env resolves to compose.yaml's `name: clippet`, so
# it is refused too), when .env's COMPOSE_PROJECT_NAME is not `clippet-<lane>` for this checkout's own folder (the
# line scripts/lane-teardown.sh keys on: t1_lane_env_), and when COMPOSE_ENV_FILES is set (scripts/lib/gatestack.sh's way of pointing compose at
# another stack). BASE and PUBLIC_BASE_URL from the shell are ignored: T1 runs on the checkout's own stack.
#
# Knobs: T1_BASE (the diff's base, default main; passed on as SELECT_BASE and T0_BASE), T1_WHY=1 (print the
# selection's reason for every path), T1_ONLY=<bundles> (a fixer re-runs a slice: this E2E_ONLY instead of the
# selection's; the race and cross steps still follow the diff), T1_VISUAL=<phases>|none|all (the visual phases
# instead of the map's), T1_KEEP=1 (leave a stack t1 brought up), CHECK_JOBS (the race step: default 4; t0: unset = 8 quiet / 4 busy).
# `T1_SELFTEST=1 bash scripts/t1.sh` runs the pure decisions (the refusal, the race intersection, the cross rule,
# the page → phase map, the T2 message) against planted inputs: no Docker, no git, no stack (check.sh's t1 stage).
#
# What it leaves on the box: .e2e/t1/ (replaced by the next run), what t0.sh, e2e.sh and visual.sh leave (each
# says), and a lane stack only if it was already up or T1_KEEP=1. Its testdeps image tag is its own and removed.
set -u; cd "$(dirname "$0")/.."

# ---- the decisions, as functions (the self-test calls them) -------------------------------------------------
# t1_project_ → the compose project this checkout's `docker compose` would use: the shell's, then .env's, then
# compose.yaml's own `name:`.
t1_envline_() { { [ -f .env ] && sed -n "s/^$1=//p" .env | tail -1 | tr -d '\r"'; } || true; }
t1_project_() {
  local p=${COMPOSE_PROJECT_NAME:-}
  [ -n "$p" ] || p=$(t1_envline_ COMPOSE_PROJECT_NAME)
  [ -n "$p" ] || p=$(sed -n 's/^name:[[:space:]]*//p' compose.yaml 2>/dev/null | head -1 | tr -d '\r"')
  printf '%s\n' "${p:-clippet}"
}
# t1_refuse_ <project> <base> → prints the reason and returns 1 when T1 must not touch this stack.
t1_refuse_() {
  case $1 in
    clippet|clippet-dev|clippet-gate) echo "the compose project is '$1' (the reference, dev or gate stack): T1 runs on a lane's own stack only"; return 1 ;;
  esac
  case $2 in
    http://localhost:*|http://127.0.0.1:*) ;;
    *) echo "the stack's PUBLIC_BASE_URL is '${2:-<unset>}', not a loopback origin of the lane's own"; return 1 ;;
  esac
  case ${2##*:} in
    8480|8481|8576) echo "PUBLIC_BASE_URL '$2' is the port of the dev, reference or gate stack"; return 1 ;;
  esac
  return 0
}
# t1_lane_env_ <checkout folder> <.env's COMPOSE_PROJECT_NAME> → prints the reason and returns 1 unless .env names
# the project `clippet-<lane>` that IS this checkout's folder (the lane worktree is <repo>-<lane>). That exact line is
# what scripts/lane-teardown.sh keys the lane's stack on and refuses without — a lane .env that said `autonits15`
# reached teardown before anyone saw it (run 15a [harness]); here it surfaces at the lane's first T1.
t1_lane_env_() {
  case $1 in
    clippet-?*) ;;
    *) echo "this checkout's folder is '$1', not a lane worktree's clippet-<lane>"; return 1 ;;
  esac
  [ "$2" = "$1" ] || { echo ".env's COMPOSE_PROJECT_NAME is '${2:-<unset>}', not '$1' (this lane worktree's folder): write the line COMPOSE_PROJECT_NAME=$1 — scripts/lane-teardown.sh refuses the lane without it"; return 1; }
  return 0
}
# t1_race_ "<changed packages>" "<the race default>" → the packages in both, in the default's order, or nothing.
t1_race_() {
  local p out=""
  for p in $2; do case " $1 " in *" $p "*) out="$out $p" ;; esac; done
  printf '%s\n' "${out# }"
}
# t1_cross_ <paths file> → `yes` when any Go file, go.mod or go.sum changed, else `no`. Until run 10 it was
# cmd/ or a *_windows.go file only, and a rename in a package a windows file calls into (internal/worker's
# freeBytes, say) broke the windows build unseen until T2 (run 9 R8). Measured on run 10's lane box:
# the testdeps image from cache 2 s + the build on a warm clippet-gobuild 2-4 s, so "always, for Go" is cheaper
# than keeping a list of the importers, and it cannot go stale when a new _windows.go file appears.
t1_cross_() { if grep -qE '\.go$|^go\.(mod|sum)$' "$1"; then echo yes; else echo no; fi; }
# t1_phases_ <paths file> → none | all | a comma list of visual.js's phase numbers, for the paths under web/ and
# internal/web/. 0 = login/register, 1 = the empty library, 2 = the library, a watch page, the account's pages, the
# front door and the 404, 3 = the follow, 4 = the faces' DOM test, 5 = the face frames and the embed, 6 = the audio
# clip. A file every page stands on is every phase.
T1_ALL_PHASES="0 1 2 3 4 5 6"
t1_phases_() {
  local f ph="" any=0
  while IFS= read -r f; do
    case $f in web/*|internal/web/*) ;; *) continue ;; esac
    case $f in *_test.go|*/testdata/*) continue ;; esac
    any=1
    case $f in
      web/templates/login.html|web/templates/register.html) ph="$ph 0" ;;
      web/templates/index.html) ph="$ph 1 2 6" ;;
      web/templates/clip.html) ph="$ph 2 6" ;;
      web/templates/collection.html|web/templates/settings.html|web/templates/setup.html|web/templates/notfound.html) ph="$ph 2" ;;
      web/templates/front.html) ph="$ph 2 3 4 5" ;;
      web/templates/embed.html) ph="$ph 5" ;;
      *) ph="$T1_ALL_PHASES"; break ;;
    esac
  done < "$1"
  [ "$any" = 1 ] || { echo none; return; }
  ph=$(for f in $ph; do echo "$f"; done | sort -un | tr '\n' ' ' | sed 's/ $//')
  if [ "$ph" = "$T1_ALL_PHASES" ]; then echo all; else echo "$ph" | tr ' ' ','; fi
}
# t1_visual_skip_ <T1_VISUAL> → why the visual stage is skipped when its phases are none: the knob, when the
# operator set it, else the diff (run 10 [nits10]: the line blamed the diff under T1_VISUAL=none).
t1_visual_skip_() {
  if [ "${1:-}" = none ]; then echo "skipped by T1_VISUAL=none"; else echo "nothing under web/ or internal/web/ changed"; fi
}
# t1_bucket_ <COMPOSE_FILE value> → `bucket` when the stack is made with the harness's S3 server
# (compose.e2e-bucket.yaml: RustFS, run 22), else `none`. Without it the lane stack keeps its media
# wherever compose.yaml's default puts it (a folder since run 22's `folder22`), and every bundle that plants
# into or reads back from a bucket stands down on `store_has_bucket` — a PASS that proved no bucket path.
t1_bucket_() {
  case ":$(printf '%s' "$1" | tr ';' ':'):" in
    *:compose.e2e-bucket.yaml:*) echo bucket ;;
    *) echo none ;;
  esac
}
# t1_t2paths_ <select.sh --why output> → the paths that selected T2, space-separated.
t1_t2paths_() { sed -n 's/^  \(.*\) -> T2 (a shape path.*/\1/p' "$1" | tr '\n' ' ' | sed 's/ $//'; }
# t1_paths_ <select.sh --why output> → every path the selection read, one per line.
t1_paths_() { sed -n 's/^  \(.*\) -> .*/\1/p' "$1" | grep -v '^(' || true; }

# ---- the self-test ----------------------------------------------------------------------------------------
if [ "${T1_SELFTEST:-0}" = 1 ]; then
  st=$(mktemp -d); bad=0
  no_() { echo "  t1 self-test FAIL: $*"; bad=$((bad + 1)); }
  for p in clippet clippet-dev clippet-gate; do t1_refuse_ "$p" http://localhost:8581 >/dev/null && no_ "project $p was not refused"; done
  t1_refuse_ clippet-tiers http://localhost:8581 >/dev/null || no_ "a lane project on its own loopback port was refused"
  t1_refuse_ clippet-tiers https://clippet.gg >/dev/null && no_ "a public PUBLIC_BASE_URL was not refused"
  t1_refuse_ clippet-tiers "" >/dev/null && no_ "an unset PUBLIC_BASE_URL was not refused"
  for port in 8480 8481 8576; do t1_refuse_ clippet-x "http://localhost:$port" >/dev/null && no_ "port $port was not refused"; done
  t1_lane_env_ clippet-nits22 clippet-nits22 >/dev/null || no_ "a lane .env naming its own folder's project was refused"
  t1_lane_env_ clippet-autonits15 autonits15 >/dev/null && no_ "a lane .env naming the bare lane (not clippet-<lane>) was not refused"
  t1_lane_env_ clippet-nits22 clippet-other >/dev/null && no_ "a lane .env naming ANOTHER lane's project was not refused"
  t1_lane_env_ clippet-nits22 "" >/dev/null && no_ "a lane .env with no COMPOSE_PROJECT_NAME was not refused"
  t1_lane_env_ scratch scratch >/dev/null && no_ "a checkout folder that is not clippet-<lane> was not refused"
  case $(t1_lane_env_ clippet-autonits15 autonits15) in *"COMPOSE_PROJECT_NAME=clippet-autonits15"*) ;; *) no_ "the lane .env refusal must name the line to write" ;; esac
  [ "$(t1_race_ './internal/notify' './cmd/clippet ./internal/httpapi ./internal/notify ./internal/worker')" = ./internal/notify ] || no_ "notify ∩ the race default must be ./internal/notify"
  [ -z "$(t1_race_ './internal/web ./internal/urls' './internal/notify ./internal/worker')" ] || no_ "packages outside the race default must race nothing"
  [ "$(t1_race_ './internal/worker ./cmd/clippet' './cmd/clippet ./internal/worker')" = './cmd/clippet ./internal/worker' ] || no_ "two owners must both race, in the default's order"
  [ -z "$(t1_race_ 'none' './internal/notify')" ] || no_ "select.sh's 'none' must race nothing"
  printf 'internal/notify/bot.go\n' > "$st/p1"; [ "$(t1_cross_ "$st/p1")" = yes ] || no_ "a notify change must cross-compile (any Go file)"
  printf 'internal/worker/scratch.go\n' > "$st/p1b"; [ "$(t1_cross_ "$st/p1b")" = yes ] || no_ "a change in a package a _windows.go file lives in must cross-compile"
  printf 'go.sum\n' > "$st/p1c"; [ "$(t1_cross_ "$st/p1c")" = yes ] || no_ "a go.sum change must cross-compile"
  printf 'README.md\nscripts/t1.sh\nweb/static/app.css\n' > "$st/p1d"; [ "$(t1_cross_ "$st/p1d")" = no ] || no_ "a diff with no Go file must not cross-compile"
  printf 'cmd/clippet/admin.go\n' > "$st/p2"; [ "$(t1_cross_ "$st/p2")" = yes ] || no_ "a cmd/ change must cross-compile"
  printf 'internal/worker/freebytes_windows.go\n' > "$st/p3"; [ "$(t1_cross_ "$st/p3")" = yes ] || no_ "a _windows.go change must cross-compile"
  printf 'cmd/clippet/admin_test.go\n' > "$st/p3b"; [ "$(t1_cross_ "$st/p3b")" = yes ] || no_ "a cmd/ test change must cross-compile (it is still a cmd/ .go file)"
  [ "$(t1_phases_ "$st/p1")" = none ] || no_ "a notify change must take no visual phase"
  printf 'web/templates/login.html\n' > "$st/p4"; [ "$(t1_phases_ "$st/p4")" = 0 ] || no_ "login.html must be phase 0"
  printf 'web/templates/embed.html\nweb/templates/login.html\nREADME.md\n' > "$st/p5"; [ "$(t1_phases_ "$st/p5")" = 0,5 ] || no_ "embed + login must be phases 0,5 (got $(t1_phases_ "$st/p5"))"
  printf 'web/static/app.css\n' > "$st/p6"; [ "$(t1_phases_ "$st/p6")" = all ] || no_ "app.css must be every phase"
  printf 'internal/web/meta.go\n' > "$st/p7"; [ "$(t1_phases_ "$st/p7")" = all ] || no_ "internal/web must be every phase"
  printf 'internal/web/meta_test.go\n' > "$st/p8"; [ "$(t1_phases_ "$st/p8")" = none ] || no_ "an internal/web test must take no visual phase"
  printf 'web/templates/index.html\nweb/templates/front.html\nweb/templates/login.html\n' > "$st/p9"; [ "$(t1_phases_ "$st/p9")" = all ] || no_ "index + front + login cover every phase and must say all"
  [ "$(t1_visual_skip_ none)" = "skipped by T1_VISUAL=none" ] || no_ "T1_VISUAL=none's skip must name the knob (got '$(t1_visual_skip_ none)')"
  [ "$(t1_visual_skip_ '')" = "nothing under web/ or internal/web/ changed" ] || no_ "a diff with no page must say nothing under web/ changed"
  printf 'select.sh: 2 path(s):\n  compose.yaml -> T2 (a shape path: the full gate)\n  README.md -> none (docs/records)\n' > "$st/why"
  [ "$(t1_t2paths_ "$st/why")" = compose.yaml ] || no_ "the T2 message must name compose.yaml (got '$(t1_t2paths_ "$st/why")')"
  [ "$(t1_paths_ "$st/why" | wc -l | tr -d ' ')" = 2 ] || no_ "the path list must read both paths"
  [ "$(t1_bucket_ compose.yaml:compose.e2e-bucket.yaml)" = bucket ] || no_ "the bucket overlay in COMPOSE_FILE must read as bucket"
  [ "$(t1_bucket_ 'compose.yaml;compose.gpu.yaml;compose.e2e-bucket.yaml')" = bucket ] || no_ "…with ; as the separator too"
  [ "$(t1_bucket_ compose.yaml:compose.fs.yaml)" = none ] || no_ "a stack without the overlay must read as none"
  [ "$(t1_bucket_ '')" = none ] || no_ "an unset COMPOSE_FILE must read as none"
  T0_SELFTEST=1 sh scripts/t0.sh > "$st/t0self" 2>&1 || no_ "t0.sh's own self-test (the CHECK_JOBS decision): $(cat "$st/t0self")"
  rm -rf "$st"
  if [ "$bad" = 0 ]; then echo "  t1 self-test PASS: the three stacks refused, a public or reference base refused, a lane .env not naming clippet-<its folder> refused, race = the diff ∩ the owners, cross on any Go change, pages → visual phases, the bucket overlay read from COMPOSE_FILE, T2 names its path, the visual skip names its reason, t0's CHECK_JOBS decision"; exit 0; fi
  exit 1
fi

# ---- the run ----------------------------------------------------------------------------------------------
T1_START=$(date +%s)
D=.e2e/t1
rm -rf "$D"; mkdir -p "$D"
T1_JOBS=${CHECK_JOBS:-4}   # the race step's; t0 picks its own when CHECK_JOBS is unset (8 quiet / 4 busy, t0.sh t0_jobs_)
T1_BASE=${T1_BASE:-main}
FAILED=""
line() {   # line <step> <ok|FAIL|skip|STOP> <seconds> <what>
  printf 't1: %-7s %-4s %4s s  %s\n' "$1" "$2" "$3" "$4"
  case $2 in FAIL|STOP) FAILED="$FAILED $1" ;; esac
}
verdict_() {
  local total=$(( $(date +%s) - T1_START )) s
  if [ -n "$FAILED" ]; then
    for s in $FAILED; do
      case $s in
        t0)     echo "---- t0 ----"; grep -E '^t0: |^T0 |^---- |^  ' "$D/t0.log" | tail -60 ;;
        race)   echo "---- race ----"; grep -v '^\$ ' "$D/race.log" | tail -60 ;;
        cross)  echo "---- cross ----"; tail -30 "$D/cross.log" ;;
        e2e)    echo "---- e2e ----"; grep -E 'FAIL|^E2E ' "$D/e2e.log" | tail -30 ;;
        visual) echo "---- visual ----"; tail -40 "$D/visual.log" ;;
        image)  echo "---- image ----"; tail -30 "$D/image.log" ;;
      esac
    done
    echo "T1 FAIL (${total} s):$FAILED${T1_SAY:+ — $T1_SAY}"
    exit 1
  fi
  echo "T1 PASS (${total} s)"
  exit 0
}

if [ -n "${COMPOSE_ENV_FILES:-}" ]; then
  echo "t1: REFUSED — COMPOSE_ENV_FILES is set ($COMPOSE_ENV_FILES; the gate stack's way of naming a stack): T1 runs on this checkout's own .env only"
  exit 2
fi
PROJ=$(t1_project_)
PBASE=$(t1_envline_ PUBLIC_BASE_URL); PBASE=${PBASE%/}
if ! why=$(t1_refuse_ "$PROJ" "$PBASE"); then
  echo "t1: REFUSED — $why"
  exit 2
fi
if ! why=$(t1_lane_env_ "$(basename "$PWD")" "$(t1_envline_ COMPOSE_PROJECT_NAME)"); then
  echo "t1: REFUSED — $why"
  exit 2
fi
unset BASE PUBLIC_BASE_URL
# IN WSL (DESIGN §20 erratum 338): the refusals above ran here first; typed in Git Bash with WSL's clone present,
# T1 now re-runs itself in the WSL worktree $HOME/dev/<this checkout's folder> at this checkout's commit, with this
# checkout's .env copied in (a lane project only), so the lane stack is built from that worktree — and the refusals
# run again there. A dirty tree is REFUSED (WSL tests the commit; CLIPPET_WSL=0 runs it here on Git Bash).
. scripts/lib/wslexec.sh
wsl_reexec_ t1 bash scripts/t1.sh "$@"

# ---- select -----------------------------------------------------------------------------------------------
t=$(date +%s)
SEL=$(SELECT_BASE=$T1_BASE bash scripts/select.sh --why 2> "$D/select-why.txt") || { line select FAIL $(( $(date +%s) - t )) "scripts/select.sh failed: $(tail -1 "$D/select-why.txt")"; verdict_; }
GOPK=$(SELECT_BASE=$T1_BASE bash scripts/select.sh --go 2>/dev/null) || GOPK=none
t1_paths_ "$D/select-why.txt" > "$D/paths.txt"
NPATHS=$(wc -l < "$D/paths.txt" | tr -d ' ')
if [ "${T1_WHY:-0}" = 1 ]; then sed 's/^/t1: why /' "$D/select-why.txt"; fi
E2E_V=$SEL
[ -z "${T1_ONLY:-}" ] || E2E_V=$T1_ONLY
if [ "$SEL" = T2 ] && [ -z "${T1_ONLY:-}" ]; then
  line select STOP $(( $(date +%s) - t )) "$NPATHS path(s) against $T1_BASE: T2"
  echo "t1: this diff needs T2: $(t1_t2paths_ "$D/select-why.txt")"
  echo "t1: (a shape path is owed the full gate — bash scripts/gate.sh, run by the maintainer; T1_WHY=1 names every path's reason)"
  T1_SAY="this diff needs T2"
  verdict_
fi
line select ok $(( $(date +%s) - t )) "$NPATHS path(s) against $T1_BASE: e2e=$E2E_V${T1_ONLY:+ (T1_ONLY; the selection said $SEL)}, go=$GOPK"

# ---- t0 ---------------------------------------------------------------------------------------------------
t=$(date +%s)
if T0_BASE=$T1_BASE sh scripts/t0.sh > "$D/t0.log" 2>&1; then line t0 ok $(( $(date +%s) - t )) "$(tail -1 "$D/t0.log")"
else line t0 FAIL $(( $(date +%s) - t )) "$(tail -1 "$D/t0.log")"; fi

# ---- race / tagged / cross: one testdeps image of this run's own --------------------------------------------
RDEF=$(sed -n 's/^GOCHECK_RACE_DEFAULT="\(.*\)"$/\1/p' scripts/lib/gocheck.sh)
RACE=""; [ "$GOPK" = none ] || RACE=$(t1_race_ "$GOPK" "$RDEF")
CROSS=$(t1_cross_ "$D/paths.txt")
if [ -n "$RACE" ] || [ "$CROSS" = yes ]; then
  IMG="clippet-t1-deps:$$-$(date +%s)"; GOC="clippet-t1-go-$$"
  # The image and the container are this run's own, removed on any exit. A signal kills the `docker run` CLIENT, not
  # the daemon's create under way, so the container can appear after the first rm (t0.sh's cleanup, measured there):
  # on a signal, two more passes once the daemon has finished.
  T1_SIG=
  t1_rm_() { docker rm -f "$GOC" >/dev/null 2>&1; docker rmi -f "$IMG" >/dev/null 2>&1; }
  trap 't1_rm_; if [ -n "$T1_SIG" ]; then sleep 1; t1_rm_; sleep 2; t1_rm_; fi' EXIT
  trap 'T1_SIG=INT; exit 130' INT
  trap 'T1_SIG=TERM; exit 143' TERM
  HOST=$( (pwd -W) 2>/dev/null || pwd )
  t=$(date +%s)
  if ! MSYS_NO_PATHCONV=1 docker build --target testdeps -t "$IMG" . > "$D/image.log" 2>&1; then
    line image FAIL $(( $(date +%s) - t )) "docker build --target testdeps failed"
  else
    docker volume inspect clippet-gobuild >/dev/null 2>&1 || docker volume create clippet-gobuild >/dev/null
    t=$(date +%s)
    if [ -n "$RACE" ]; then
      mkdir -p "$D/race"
      T1_CHOWN=; case $(uname -s) in Linux) T1_CHOWN="$(id -u):$(id -g)" ;; esac   # erratum 338: /out back to this user on Linux
      MSYS_NO_PATHCONV=1 docker run --rm --name "$GOC" -e "CHECK_JOBS=$T1_JOBS" -e "CHECK_RACE=$RACE" -e GOCHECK_OUT=/out -e GOCHECK_VET=none -e "GOCHECK_CHOWN=$T1_CHOWN" \
        -e "GOCHECK_CROSS=$( [ "$CROSS" = yes ] && echo 1 || echo 0 )" \
        -v "$HOST:/src:ro" -v "$HOST/$D/race:/out" -v clippet-gobuild:/root/.cache/go-build -w /src \
        "$IMG" sh /src/scripts/lib/gocheck.sh > "$D/race.log" 2>&1
      rc=$?; secs=$(( $(date +%s) - t ))
      ok=$(grep '^GOCHECK OK' "$D/race.log" | tail -1)
      nr=$(grep -c '^race' "$D/race/go/names.txt" 2>/dev/null); nr=${nr:-0}
      np=$(awk -F'\t' '$1 == "ffmpeg" && $4 == "ffmpeg-part"' "$D/race/go/names.txt" 2>/dev/null | wc -l | tr -d ' ')
      if [ "$rc" = 0 ] && [ -n "$ok" ]; then line race ok "$secs" "-race over $RACE: $nr test(s); $(echo "$ok" | sed 's/^GOCHECK OK ([0-9]* s): //')"
      else line race FAIL "$secs" "gocheck.sh CHECK_RACE='$RACE' failed (exit $rc); the shards' logs are in $D/race/go/"; fi
      line tagged ok 0 "t0's pass is -tags ffmpeg; the race run's complementary -tags ffmpeg half ran the raced packages' $np tagged-only/dual test(s) again"
      if [ "$CROSS" = yes ]; then
        set -- $(cat "$D/race/go/cross.rc" 2>/dev/null || echo "99 0")
        if [ "$1" = 0 ]; then line cross ok "$2" "GOOS=windows GOARCH=amd64 go build ./cmd/clippet (inside the race run)"
        else line cross FAIL "$2" "the windows cross-compile failed (exit $1): $D/race/go/cross.log"; fi
      else line cross skip 0 "no Go file, go.mod or go.sum changed"; fi
    else
      line race skip 0 "no changed package owns a goroutine (go=$GOPK; the owners: gocheck.sh's GOCHECK_RACE_DEFAULT)"
      line tagged ok 0 "t0's one pass is -tags ffmpeg: the changed packages' tagged tests ran there"
      MSYS_NO_PATHCONV=1 docker run --rm --name "$GOC" -e GOOS=windows -e GOARCH=amd64 -e CGO_ENABLED=0 \
        -v "$HOST:/src:ro" -v clippet-gobuild:/root/.cache/go-build -w /src \
        "$IMG" go build -o /dev/null ./cmd/clippet > "$D/cross.log" 2>&1
      rc=$?
      if [ "$rc" = 0 ]; then line cross ok $(( $(date +%s) - t )) "GOOS=windows GOARCH=amd64 go build ./cmd/clippet"
      else line cross FAIL $(( $(date +%s) - t )) "the windows cross-compile failed (exit $rc)"; fi
    fi
  fi
else
  line race skip 0 "no changed package owns a goroutine (go=$GOPK)"
  line tagged ok 0 "t0's one pass is -tags ffmpeg: the changed packages' tagged tests ran there"
  line cross skip 0 "no Go file, go.mod or go.sum changed"
fi

# ---- the lane stack: e2e, then visual ---------------------------------------------------------------------
case ${T1_VISUAL:-} in
  '') PHASES=$(t1_phases_ "$D/paths.txt") ;;
  *)  PHASES=$T1_VISUAL ;;
esac
STACK_WAS_UP=0
if [ -n "$(docker compose -p "$PROJ" ps -q 2>/dev/null)" ]; then STACK_WAS_UP=1; fi
# the app image the stack runs (compose.yaml: ${CLIPPET_IMAGE:-clippet-app:local}): removed at the end only if it did
# not exist before this run AND t1 brought the stack up — never the default tag, which other checkouts build too
APPIMG=$(t1_envline_ CLIPPET_IMAGE)
IMG_WAS=1; [ -z "$APPIMG" ] || docker image inspect "$APPIMG" >/dev/null 2>&1 || IMG_WAS=0
STACK_USED=0
if [ "$E2E_V" != none ] && [ "$(t1_bucket_ "$(t1_envline_ COMPOSE_FILE)")" = none ]; then
  line stack ok 0 "NOTE: COMPOSE_FILE does not name compose.e2e-bucket.yaml, so this lane stack has no bucket and the bucket bundles stand down (gatestack.sh's shape adds it)"
fi
t=$(date +%s)
case $E2E_V in
  none) line e2e skip 0 "the selection is none: no bundle covers this diff (the E2E self-test ran in t0)" ;;
  *)
    STACK_USED=1
    if [ "$E2E_V" = all ]; then
      echo "t1: WARNING — the selection is 'all' (a CORE, uncovered or unknown path; T1_WHY=1 names it): the WHOLE suite runs on the"
      echo "t1:           lane stack, ≈ 6–10 min (380–581 s measured on a lane stack, scripts/e2e.d/README.md) — past T1's 5 min."
      env -u MSYS_NO_PATHCONV bash scripts/e2e.sh > "$D/e2e.log" 2>&1; rc=$?
    else
      env -u MSYS_NO_PATHCONV E2E_ONLY="$E2E_V" bash scripts/e2e.sh > "$D/e2e.log" 2>&1; rc=$?
    fi
    v=$(grep -E '^E2E (PASS|FAIL)' "$D/e2e.log" | tail -1 | cut -c1-120)
    if [ "$rc" = 0 ]; then line e2e ok $(( $(date +%s) - t )) "${v:-?} on $PBASE"
    else line e2e FAIL $(( $(date +%s) - t )) "${v:-no verdict line} (exit $rc; $D/e2e.log)"; fi ;;
esac
t=$(date +%s)
case $PHASES in
  none) line visual skip 0 "$(t1_visual_skip_ "${T1_VISUAL:-}")" ;;
  *)
    STACK_USED=1
    if [ "$E2E_V" = none ]; then
      # the E2E's step 1 did not bring the stack up: do it here, the way step 1 does
      env -u MSYS_NO_PATHCONV docker compose -p "$PROJ" up -d --build > "$D/up.log" 2>&1 || true
      # the bucket before the wait, as step 1 makes it (§20 erratum 346): a fresh S3 server has none
      env -u MSYS_NO_PATHCONV docker compose -p "$PROJ" exec -T app clippet admin objects init >> "$D/up.log" 2>&1 || true
      end=$((SECONDS + 90)); while [ "$SECONDS" -lt "$end" ]; do
        # --connect-timeout/--max-time: under WSL's mirrored networking a closed loopback port TIMES OUT instead of
        # refusing (erratum 338), and an unbounded curl would stretch this bounded poll without limit
        [ "$(curl -s --connect-timeout 2 --max-time 5 -o /dev/null -w '%{http_code}' "$PBASE/readyz" 2>/dev/null)" = 200 ] && break
        sleep 0.5   # a poll for /readyz bounded by the clock (90 s), as gate.sh's gate_ready_
      done
    fi
    if [ "$PHASES" = all ]; then
      echo "t1: WARNING — a file every page stands on changed (T1_WHY=1 names it): the WHOLE visual pass runs (≈ 4 min)"
      env -u MSYS_NO_PATHCONV bash scripts/visual.sh > "$D/visual.log" 2>&1; rc=$?
    else
      env -u MSYS_NO_PATHCONV VISUAL_ONLY="$PHASES" bash scripts/visual.sh > "$D/visual.log" 2>&1; rc=$?
    fi
    v=$(grep -E '^\[visual\] [0-9]+ shots' "$D/visual.log" | tail -1 | cut -c1-100)
    if [ "$rc" = 0 ]; then line visual ok $(( $(date +%s) - t )) "phases $PHASES: ${v:-?}"
    else line visual FAIL $(( $(date +%s) - t )) "phases $PHASES: ${v:-no verdict line} (exit $rc; $D/visual.log)"; fi ;;
esac

# ---- down: only what t1 brought up ------------------------------------------------------------------------
if [ "$STACK_USED" = 1 ] && [ "$STACK_WAS_UP" = 0 ]; then
  if [ "${T1_KEEP:-0}" = 1 ]; then line down skip 0 "T1_KEEP=1: the lane stack $PROJ stays up ($PBASE)"
  else
    t=$(date +%s)
    if docker compose -p "$PROJ" down -v > "$D/down.log" 2>&1; then
      say="the lane stack $PROJ was down when t1 began: down -v (its containers, network, volumes)"
      if [ "$IMG_WAS" = 0 ] && docker rmi "$APPIMG" >> "$D/down.log" 2>&1; then say="$say, and the image $APPIMG this run built"; fi
      line down ok $(( $(date +%s) - t )) "$say"
    else line down FAIL $(( $(date +%s) - t )) "docker compose -p $PROJ down -v failed ($D/down.log)"; fi
  fi
elif [ "$STACK_USED" = 1 ]; then
  line down skip 0 "the lane stack $PROJ was already up when t1 began: left as found"
fi
verdict_
