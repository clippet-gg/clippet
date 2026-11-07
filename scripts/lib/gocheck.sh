#!/bin/sh
# gocheck.sh — the Go tests of scripts/check.sh and scripts/t0.sh, run INSIDE one container (lane `speed2`;
# reshaped by run 9's lane `t0`). POSIX sh for the image's busybox. check.sh runs it in the Dockerfile's
# `testsrc` stage (the source copied in, gofmt and `go mod verify` already done); t0.sh runs it in the
# `testdeps` stage with the checkout bind-mounted read-only at /src (the container test loop).
#
# WHAT it runs, by default (CHECK_RACE unset):
#   go vet ./...  and  go vet -tags ffmpeg ./...          (GOCHECK_VET narrows the list, `none` skips both)
#   GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/clippet   (GOCHECK_CROSS=0 skips it)
#   go test -tags ffmpeg ./...                            ONE pass: every test, tagged ones included, no -race
# With CHECK_RACE set, a second pass under the race detector:
#   CHECK_RACE=1     CGO_ENABLED=1 go test -race over GOCHECK_RACE_DEFAULT below: the packages whose non-test
#                    code starts a goroutine or runs a ticker loop, or holds a mutex its concurrent callers
#                    share (the list is part of this file, so editing it moves scripts/lib/gotree.sh's hash)
#   CHECK_RACE=all   the same over ./...                  (T2 and the nightly)
#   CHECK_RACE='./internal/a ./cmd/b'                     an explicit list (T1: the changed goroutine owners)
# and then NO test runs twice: a package in the race pass runs its untagged tests ONLY under -race, and the
# tagged pass runs, for that package, only the names its untagged listing does not have (the ffmpeg-tagged
# tests: internal/transcode's *_ffmpeg_test.go). The one exception is a test whose own body reads
# `raceDetector` (cmd/clippet and internal/sweeper declare it in race_on_test.go/race_off_test.go): it was
# written to run in both builds — TestBackupCancelInterruptsVacuum skips itself under -race and says "the
# pass without the race detector runs it" — so it runs in both passes and the plan prints its name.
#
# NO -count=1. The container mounts the named volume clippet-gobuild at /root/.cache/go-build (check.sh and
# t0.sh both do; created if absent, never removed), so compiles AND test results are cached across runs.
# Go's test cache is keyed on the test binary (the package's sources and its dependencies), the flags, the
# environment variables the test read and the files it opened: an unchanged package answers `(cached)`.
#
# HOW: no test in this module calls t.Parallel, so a package's tests run one after another. Each pass's test
# names are LISTED (`go test -list .`, which also builds every test binary), each package is cut into shards
# of whole top-level tests, and the shards run CHECK_JOBS at a time, longest first, each as
# `go test -run '^(A|B|…)$' <pkg>`. Every planned name lands in exactly one shard of its pass — checked
# against the listing before anything runs — and a shard whose log says "no tests to run" fails the stage.
#
# THE PARTITION IS A FUNCTION OF THE TREE ALONE. A shard's `-run` expression is part of its cache key, so a
# plan that moved between two runs of the same tree would miss the cache it is there to hit. Until run 9 the
# shard count per package came from the LAST run's measured seconds and from CHECK_JOBS; now it comes from
# the committed weights file scripts/lib/go-weights.txt (`key TAB package TAB seconds`; a missing row is 0.4 s
# a test) and a fixed target of GOCHECK_TARGET seconds a shard (default 10), whatever CHECK_JOBS is — so T0 at
# 4 jobs and check.sh at 16 plan the same shards and share one cache. The measured seconds are still written
# (to $GOCHECK_OUT/go-times.txt, which check.sh keeps in .e2e/checkpoints/), only from shards that really ran;
# to rebalance, copy that file over go-weights.txt (one cold run follows, on purpose).
#
# GOMAXPROCS: every shard runs with GOMAXPROCS=$CHECK_GOMAXPROCS (default 4; `0` leaves Go's default, every
# CPU). Run 9, lane `pool`: CHECK_JOBS shards × every CPU's worth of Ps, beside a docker build, oversubscribed
# the box (16 × 16 on the 16-CPU PC) and turned timing budgets into failures; the one test that needed every
# P (internal/worker TestPosterIsCancelledSynchronously) waits on the run's own signal since lane `settle`.
#
# Logs: $GOCHECK_OUT/go/ — each shard's log starts with the command it ran (`$ go test …`).
# `sh gocheck.sh --selftest` runs the planner and the shard command against planted listings and a stub `go`
# (check.sh stage 18; no container, no Go).
set -u
OUT=${GOCHECK_OUT:-/out}
G="$OUT/go"
GOCHECK_RACE_DEFAULT="./cmd/clippet ./internal/auth ./internal/cost ./internal/drop ./internal/httpapi ./internal/notify ./internal/ratelimit ./internal/store ./internal/sweeper ./internal/transcode ./internal/views ./internal/worker"
TAB=$(printf '\t')

# gmp_ <CHECK_GOMAXPROCS> → the GOMAXPROCS every shard runs with: 4 when unset, 0 (Go's default) for 0,
# the number itself for a positive number. Anything else is refused by the main block before a shard runs.
gmp_() {
  case ${1:-} in
    '') echo 4 ;;
    *[!0-9]*) echo bad ;;
    *) if [ "$1" -eq 0 ]; then echo 0; else echo "$1"; fi ;;
  esac
}

# race_pkgs_ <CHECK_RACE> → the package patterns the -race pass covers; nothing for unset or 0.
race_pkgs_() {
  case ${1:-} in
    ''|0) ;;
    1)    echo "$GOCHECK_RACE_DEFAULT" ;;
    all)  echo ./... ;;
    *)    echo "$1" ;;
  esac
}

# listed_ <listing log> → "pkg TAB name", one per listed top-level test. A package's names come before its
# `ok <pkg>` line (cached or not); `?` (no test files) and FAIL lines drop what came before them.
listed_() {
  awk '
    /^(Test|Example|Fuzz)[A-Za-z0-9_]*$/ { names[++n] = $0; next }
    /^(ok|---|FAIL)[ \t]/ || /^\?[ \t]/ { if ($1 == "ok") for (i = 1; i <= n; i++) print $2 "\t" names[i]; n = 0; next }
  ' "$1"
}

# dual_ <module path> <root dir> < race packages (import paths) → "pkg TAB name" for every top-level Test
# function whose body names raceDetector: the tests written to run in both builds.
dual_() {
  while IFS= read -r p; do
    [ -n "$p" ] || continue
    d="$2${p#"$1"}"
    for f in "$d"/*_test.go; do
      [ -f "$f" ] || continue
      awk -v p="$p" '
        /^func / { cur = ""; if (match($0, /^func Test[A-Za-z0-9_]*\(/)) cur = substr($0, 6, RLENGTH - 6) }
        /^func .*\}[ \t]*$/ { cur = "" }
        /^}/ { cur = "" }
        /raceDetector/ && cur != "" { print p "\t" cur }
      ' "$f"
    done
  done | sort -u
}

# plan_ <dir> — reads <dir>/listed-ffmpeg.txt, listed-race.txt, dual.txt and $WEIGHTS; writes
#   names.txt  mode TAB pkg TAB name TAB weight-key                (every test the run runs, once per pass)
#   units.txt  mode TAB pkg TAB weight TAB names TAB weight-key    (the shards, longest first)
# and fails if the shards do not hold names.txt exactly once each.
plan_() {
  d=$1
  [ -f "$d/listed-race.txt" ] || : > "$d/listed-race.txt"
  [ -f "$d/dual.txt" ] || : > "$d/dual.txt"
  awk -F'\t' -v OFS='\t' -v RF="$d/listed-race.txt" -v DF="$d/dual.txt" '
    FILENAME == RF { race[$1 SUBSEP $2] = 1; rp[$1] = 1; print "race", $1, $2, "race"; next }
    FILENAME == DF { dual[$1 SUBSEP $2] = 1; next }
    {
      if (!($1 in rp)) print "ffmpeg", $1, $2, "ffmpeg"
      else if (!(($1 SUBSEP $2) in race) || (($1 SUBSEP $2) in dual)) print "ffmpeg", $1, $2, "ffmpeg-part"
    }
  ' "$d/listed-race.txt" "$d/dual.txt" "$d/listed-ffmpeg.txt" > "$d/names.txt"
  pw=${WEIGHTS:-/dev/null}; [ -r "$pw" ] || pw=/dev/null
  awk -F'\t' -v T="${TARGET:-10}" -v WF="$pw" '
    FILENAME == WF { if ($0 !~ /^#/ && NF >= 3) prev[$1 "\t" $2] = $3; next }
    { k = $4 "\t" $2; mode[k] = $1; cnt[k]++; names[k] = names[k] (names[k] == "" ? "" : " ") $3 }
    END {
      for (k in cnt) {
        w = (k in prev && prev[k] > 0) ? prev[k] : cnt[k] * 0.4
        s = int(w / T); if (s * T < w) s++; if (s < 1) s = 1; if (s > cnt[k]) s = cnt[k]; if (s > 32) s = 32
        m = split(names[k], nm, " ")
        for (i = 1; i <= s; i++) shard[i] = ""
        for (i = 1; i <= m; i++) { j = (i - 1) % s + 1; shard[j] = shard[j] (shard[j] == "" ? "" : " ") nm[i] }
        split(k, kp, "\t")
        for (i = 1; i <= s; i++) printf "%s\t%s\t%.2f\t%s\t%s\n", mode[k], kp[2], w / s, shard[i], kp[1]
      }
    }' "$pw" "$d/names.txt" | sort -t "$TAB" -k3,3nr -k2,2 -k4,4 > "$d/units.txt"
  cut -f1,2,4 "$d/units.txt" | awk -F'\t' '{ n = split($3, a, " "); for (i = 1; i <= n; i++) print $1 "\t" $2 "\t" a[i] }' | sort > "$d/names.sharded"
  cut -f1-3 "$d/names.txt" | sort > "$d/names.sorted"
  if ! cmp -s "$d/names.sorted" "$d/names.sharded"; then
    echo "gocheck: the shards do not hold every planned test exactly once:"; diff "$d/names.sorted" "$d/names.sharded" | head -20
    return 1
  fi
}

# unit_ <id> — run one shard (its line in $G/units.txt). The log's first line is the command it ran.
unit_() {
  uid=$1
  line=$(sed -n "${uid}p" "$G/units.txt")
  mode=$(printf '%s' "$line" | cut -f1); pkg=$(printf '%s' "$line" | cut -f2)
  names=$(printf '%s' "$line" | cut -f4); wkey=$(printf '%s' "$line" | cut -f5)
  re="^($(printf '%s' "$names" | tr ' ' '|'))\$"
  case $mode in
    race)   set -- env CGO_ENABLED=1 go test -race -run "$re" "$pkg" ;;
    ffmpeg) set -- go test -tags ffmpeg -run "$re" "$pkg" ;;
    *)      set -- false ;;
  esac
  # The cap goes in front of the command, so the log's first line says what every shard ran with.
  gmp=$(gmp_ "${CHECK_GOMAXPROCS:-}")
  [ "$gmp" = 0 ] || set -- env "GOMAXPROCS=$gmp" "$@"
  log="$G/unit-$uid.log"; ut=$(date +%s)
  printf '$ %s\n' "$*" > "$log"
  "$@" >> "$log" 2>&1
  rc=$?
  if [ "$rc" = 0 ] && grep -q 'no tests to run' "$log"; then rc=97; echo "gocheck: a shard matched no test — the partition lost a name" >> "$log"; fi
  cached=0; grep -q '(cached)' "$log" && cached=1
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$uid" "$rc" "$(( $(date +%s) - ut ))" "$mode" "$pkg" "$cached" "$wkey" > "$G/unit-$uid.rc"
}

# ---- the self-test (check.sh stage 18): the planner and the shard command, no Go, no container -------------
selftest_() {
  st=$(mktemp -d); bad=0
  no_() { echo "  gocheck self-test: $1"; bad=1; }
  mkdir -p "$st/root/internal/r" "$st/stub"
  # two packages: r (in the race pass; RA/RB untagged, RT tagged-only, RD reads raceDetector) and p (not)
  printf 'package r\n\nfunc TestRA(t *testing.T) {}\n\nfunc TestRD(t *testing.T) {\n\tif raceDetector {\n\t\tt.Skip()\n\t}\n}\n\nfunc helper() bool { return raceDetector }\n' > "$st/root/internal/r/a_test.go"
  printf 'TestRA\nTestRB\nTestRD\nTestRT\nok  \tm/internal/r\t0.1s\nTestPA\nTestPB\nok  \tm/internal/p\t(cached)\n?   \tm/internal/none\t[no test files]\n' > "$st/list-ffmpeg.log"
  printf 'TestRA\nTestRB\nTestRD\nok  \tm/internal/r\t0.1s\n' > "$st/list-race.log"
  # (1) the default: no race listing → one pass, every listed name once, no race shard
  mkdir -p "$st/d1"; listed_ "$st/list-ffmpeg.log" > "$st/d1/listed-ffmpeg.txt"
  WEIGHTS=/dev/null TARGET=10 plan_ "$st/d1" > "$st/d1.out" 2>&1 || no_ "the default plan failed: $(cat "$st/d1.out")"
  [ "$(wc -l < "$st/d1/names.txt" | tr -d ' ')" = 6 ] || no_ "the default plan must hold the 6 listed names (got $(wc -l < "$st/d1/names.txt" | tr -d ' '))"
  ! grep -q '^race' "$st/d1/units.txt" || no_ "CHECK_RACE unset planned a -race shard"
  # (2) a race pass over r: r's untagged names only under -race, RT only tagged, RD in both, p tagged only
  mkdir -p "$st/d2"; cp "$st/d1/listed-ffmpeg.txt" "$st/d2/"
  listed_ "$st/list-race.log" > "$st/d2/listed-race.txt"
  cut -f1 "$st/d2/listed-race.txt" | sort -u | dual_ m "$st/root" > "$st/d2/dual.txt"
  [ "$(cat "$st/d2/dual.txt")" = "m/internal/r${TAB}TestRD" ] || no_ "dual_ must name TestRD alone, not TestRA or the helper (got '$(cat "$st/d2/dual.txt")')"
  WEIGHTS=/dev/null TARGET=10 plan_ "$st/d2" > "$st/d2.out" 2>&1 || no_ "the race plan failed: $(cat "$st/d2.out")"
  both=$(cut -f2,3 "$st/d2/names.txt" | sort | uniq -d | tr '\t\n' ': ')
  [ "$both" = "m/internal/r:TestRD " ] || no_ "only TestRD may be in both passes (got '$both')"
  [ "$(grep -c "^ffmpeg${TAB}m/internal/r${TAB}" "$st/d2/names.txt")" = 2 ] || no_ "the tagged pass must run RT and RD of the race package and nothing more"
  [ "$(grep -c "^ffmpeg${TAB}m/internal/p${TAB}" "$st/d2/names.txt")" = 2 ] || no_ "the tagged pass must run all of a package outside the race pass"
  ! grep -q "^race${TAB}m/internal/r${TAB}TestRT" "$st/d2/names.txt" || no_ "a tagged-only test was planned under -race"
  # (3) the partition is a function of the tree: the weights decide it, CHECK_JOBS does not
  printf 'TestA1\nTestA2\nTestA3\nTestA4\nTestA5\nTestA6\nok  \tm/internal/big\t1s\n' > "$st/list-big.log"
  printf 'ffmpeg\tm/internal/big\t35\n' > "$st/w.txt"
  for j in 4 16; do
    mkdir -p "$st/j$j"; listed_ "$st/list-big.log" > "$st/j$j/listed-ffmpeg.txt"
    CHECK_JOBS=$j WEIGHTS="$st/w.txt" TARGET=10 plan_ "$st/j$j" > /dev/null 2>&1 || no_ "the plan at $j jobs failed"
  done
  cmp -s "$st/j4/units.txt" "$st/j16/units.txt" || no_ "the shards moved with CHECK_JOBS — the cache keys would move with them"
  [ "$(wc -l < "$st/j4/units.txt" | tr -d ' ')" = 4 ] || no_ "35 weighted seconds at a 10 s target must be 4 shards (got $(wc -l < "$st/j4/units.txt" | tr -d ' '))"
  # (4) the shard command, against a stub go that answers (cached): no -count=1 anywhere, -race exactly on
  # the race shards, -tags ffmpeg exactly on the others, and the (cached) answer recorded
  cat > "$st/stub/go" <<'STUB'
#!/bin/sh
for a in "$@"; do last=$a; done
printf 'ok  \t%s\t(cached)\n' "$last"
STUB
  chmod +x "$st/stub/go"
  n=$(wc -l < "$st/d2/units.txt" | tr -d ' '); i=1
  while [ "$i" -le "$n" ]; do
    G="$st/d2" PATH="$st/stub:$PATH" unit_ "$i"
    c=$(head -1 "$st/d2/unit-$i.log"); m=$(sed -n "${i}p" "$st/d2/units.txt" | cut -f1)
    case $c in *-count=1*) no_ "shard $i runs with -count=1: $c" ;; esac
    case $m:$c in
      race:*' -race '*) ;;
      race:*) no_ "race shard $i has no -race: $c" ;;
      ffmpeg:*-race*) no_ "tagged shard $i runs -race: $c" ;;
      ffmpeg:*'-tags ffmpeg'*) ;;
      *) no_ "shard $i ($m) has no -tags ffmpeg: $c" ;;
    esac
    [ "$(cut -f6 "$st/d2/unit-$i.rc")" = 1 ] || no_ "shard $i did not record the stub's (cached)"
    i=$((i + 1))
  done
  # (5) the knob: unset and 0 race nothing; 1 the owners' list; all every package; anything else itself
  [ -z "$(race_pkgs_ '')" ] && [ -z "$(race_pkgs_ 0)" ] || no_ "CHECK_RACE unset or 0 must race nothing (got '$(race_pkgs_ '')')"
  [ "$(race_pkgs_ 1)" = "$GOCHECK_RACE_DEFAULT" ] || no_ "CHECK_RACE=1 must race GOCHECK_RACE_DEFAULT"
  [ "$(race_pkgs_ all)" = ./... ] || no_ "CHECK_RACE=all must race ./..."
  [ "$(race_pkgs_ './internal/a ./cmd/b')" = './internal/a ./cmd/b' ] || no_ "an explicit CHECK_RACE list must be raced as given"
  # (6) GOMAXPROCS (run 9, lane `pool`): every shard command carries GOMAXPROCS=4 by default, the knob's
  # number when it is set, and nothing when it is 0 (Go's default); a non-number is refused.
  [ "$(gmp_ '')" = 4 ] && [ "$(gmp_ 0)" = 0 ] && [ "$(gmp_ 8)" = 8 ] && [ "$(gmp_ x)" = bad ] \
    || no_ "CHECK_GOMAXPROCS: unset → 4, 0 → Go's default, 8 → 8, x → refused (got '$(gmp_ '')' '$(gmp_ 0)' '$(gmp_ 8)' '$(gmp_ x)')"
  for v in '' 2 0; do
    i=1
    while [ "$i" -le "$n" ]; do
      CHECK_GOMAXPROCS=$v G="$st/d2" PATH="$st/stub:$PATH" unit_ "$i"
      c=$(head -1 "$st/d2/unit-$i.log")
      case ${v:-4}:$c in
        0:*GOMAXPROCS=*) no_ "CHECK_GOMAXPROCS=0 must leave GOMAXPROCS alone: $c" ;;
        0:*) ;;
        *:'$ env GOMAXPROCS='"${v:-4}"' '*) ;;
        *) no_ "shard $i does not run with GOMAXPROCS=${v:-4} (CHECK_GOMAXPROCS='$v'): $c" ;;
      esac
      i=$((i + 1))
    done
  done
  rm -rf "$st"
  if [ "$bad" = 0 ]; then echo "  gocheck self-test PASS: one pass by default; -race never doubles a test but the raceDetector ones; the shards fixed by the tree; no -count=1; GOMAXPROCS=4 on every shard unless CHECK_GOMAXPROCS says otherwise"; return 0; fi
  return 1
}

case ${1:-} in
  --unit) unit_ "$2"; exit 0 ;;
  --selftest) selftest_; exit $? ;;
esac

NCPU=$(nproc 2>/dev/null || echo 4)
J=${CHECK_JOBS:-$NCPU}
case $J in ''|*[!0-9]*|0) J=$NCPU ;; esac
RACE=$(race_pkgs_ "${CHECK_RACE:-}")
case ${CHECK_RACE:-} in
  ''|0) RACE_SAY="no -race pass (CHECK_RACE unset)" ;;
  1)    RACE_SAY="-race over the goroutine owners (CHECK_RACE=1)" ;;
  all)  RACE_SAY="-race over ./... (CHECK_RACE=all)" ;;
  *)    RACE_SAY="-race over $CHECK_RACE" ;;
esac
VET=${GOCHECK_VET-./...}; [ -n "$VET" ] || VET=none
CROSS=${GOCHECK_CROSS:-1}
WEIGHTS=${GOCHECK_WEIGHTS:-$(dirname "$0")/go-weights.txt}
TARGET=${GOCHECK_TARGET:-10}
MOD=$(awk '/^module /{ print $2; exit }' go.mod)
# GOMAXPROCS: 4 in every shard by default (CHECK_GOMAXPROCS; run 9, lane `pool`). Until then it was left at
# Go's default, every CPU, because a pinned GOMAXPROCS=2 (lane `speed2`'s first measured run) failed
# internal/worker TestPosterIsCancelledSynchronously ("Busy is still true after Cancel returned"): the test
# read a flag on a timer the scheduler could miss. Lane `settle` rewrote it to check at the instant the run
# has seen its cancel and to end on the run's settle; it passes at -cpu 1,2,4,16 -count 20 under load.
GMP=$(gmp_ "${CHECK_GOMAXPROCS:-}")
[ "$GMP" != bad ] || { echo "gocheck: CHECK_GOMAXPROCS='${CHECK_GOMAXPROCS:-}' is not a number (0 = Go's default, unset = 4)"; echo "GOCHECK FAIL"; exit 2; }
case $GMP in 0) GMP_SAY="GOMAXPROCS left at Go's default in each (CHECK_GOMAXPROCS=0)" ;; *) GMP_SAY="GOMAXPROCS=$GMP in each (CHECK_GOMAXPROCS=${CHECK_GOMAXPROCS:-unset})" ;; esac
rm -rf "$G"; mkdir -p "$G"
# GOCHECK_CHOWN=<uid>:<gid> (erratum 338: the harness on WSL): this container runs as root, and on a Linux host a
# root-owned directory in a bind mount cannot be removed by the user who owns the checkout — the next t0/t1/check
# run's `rm -rf .e2e/<tier>` printed Permission denied. The caller passes its own ids on Linux; on Git Bash (NTFS
# ignores ownership) it passes nothing and this is a no-op. Every exit path below runs it.
if [ -n "${GOCHECK_CHOWN:-}" ]; then trap 'chown -R "$GOCHECK_CHOWN" "$OUT" 2>/dev/null || true' EXIT; fi
T0=$(date +%s)
echo "gocheck: $NCPU CPUs in the container; $J shard(s) at a time (CHECK_JOBS=${CHECK_JOBS:-unset}); $RACE_SAY; $GMP_SAY"

# ---- phase A: vet, the cross-compile and the listings — at the same time ----------------------------------
bg() { name=$1; shift; ( t=$(date +%s); printf '$ %s\n' "$*" > "$G/$name.log"; "$@" >> "$G/$name.log" 2>&1; printf '%s %s\n' "$?" "$(( $(date +%s) - t ))" > "$G/$name.rc" ) & }
PA="list-ffmpeg"
# shellcheck disable=SC2086 # VET and RACE are deliberate word lists of package patterns
if [ "$VET" != none ]; then bg vet go vet $VET; bg vet-ffmpeg go vet -tags ffmpeg $VET; PA="vet vet-ffmpeg $PA"; fi
if [ "$CROSS" != 0 ]; then bg cross env GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/clippet; PA="$PA cross"; fi
bg list-ffmpeg go test -tags ffmpeg -list . ./...
# shellcheck disable=SC2086
if [ -n "$RACE" ]; then bg list-race env CGO_ENABLED=1 go test -race -list . $RACE; PA="$PA list-race"; fi
wait
TA=$(date +%s)
echo "gocheck: phase A ($PA; every test binary built or found in the cache) $((TA - T0)) s"

FAIL=0
for n in $PA; do
  set -- $(cat "$G/$n.rc" 2>/dev/null || echo "99 0")
  if [ "$1" != 0 ]; then FAIL=1; echo "gocheck: FAIL $n (exit $1) —"; sed 's/^/    /' "$G/$n.log"; fi
done
for n in list-ffmpeg list-race; do
  [ -f "$G/$n.rc" ] || continue
  if [ "$(cut -d' ' -f1 "$G/$n.rc")" != 0 ]; then echo "gocheck: a listing failed, so no test was run"; echo "GOCHECK FAIL"; exit 1; fi
done

# ---- phase B: the plan — every planned name in exactly one shard of its pass -------------------------------
listed_ "$G/list-ffmpeg.log" > "$G/listed-ffmpeg.txt"
if [ -n "$RACE" ]; then
  listed_ "$G/list-race.log" > "$G/listed-race.txt"
  cut -f1 "$G/listed-race.txt" | sort -u | dual_ "$MOD" . > "$G/dual.txt"
fi
[ -s "$G/listed-ffmpeg.txt" ] || { echo "gocheck: the listing parsed to no test names at all"; echo "GOCHECK FAIL"; exit 1; }
plan_ "$G" || { echo "GOCHECK FAIL"; exit 1; }
TOTAL=$(wc -l < "$G/names.txt" | tr -d ' ')
NU=$(wc -l < "$G/units.txt" | tr -d ' ')
NR=$(grep -c '^race' "$G/names.txt"); NF=$(grep -c '^ffmpeg' "$G/names.txt")
if [ -n "$RACE" ]; then
  echo "gocheck: $TOTAL planned tests ($NR in the -race pass over $(cut -f1 "$G/listed-race.txt" | sort -u | wc -l | tr -d ' ') package(s), $NF in the -tags ffmpeg pass) in $NU shard(s), every name once per pass"
  echo "gocheck: in both passes (their bodies read raceDetector): $(cut -f2 "$G/dual.txt" | tr '\n' ' ')"
else
  echo "gocheck: $TOTAL planned tests in one -tags ffmpeg pass, in $NU shard(s), every name once"
fi

# ---- phase C: the shards, CHECK_JOBS at a time, longest first ---------------------------------------------
i=1; while [ "$i" -le "$NU" ]; do echo "$i"; i=$((i + 1)); done | xargs -P "$J" -n 1 sh "$0" --unit
TC=$(date +%s)
cat "$G"/unit-*.rc > "$G/units.done" 2>/dev/null || : > "$G/units.done"
ND=$(wc -l < "$G/units.done" | tr -d ' ')
NC=$(awk -F'\t' '$6 == 1' "$G/units.done" | wc -l | tr -d ' ')
if [ "$ND" != "$NU" ]; then FAIL=1; echo "gocheck: FAIL — $NU shards were planned and $ND reported back"; fi
while IFS="$TAB" read -r id rc secs mode pkg cached wkey; do
  if [ "$rc" != 0 ]; then FAIL=1; echo "gocheck: FAIL shard $id ($mode $pkg, exit $rc, ${secs}s) —"; sed 's/^/    /' "$G/unit-$id.log"; fi
done < "$G/units.done"
# measured seconds per (weight key, package), only where no shard answered from the cache, folded over the
# previous record ($OUT/go-times.prev, which check.sh copies in): a record for a reader and for rebalancing
# go-weights.txt by hand — the plan never reads it.
PREV="$OUT/go-times.prev"; [ -f "$PREV" ] || : > "$PREV"
awk -F'\t' -v OFS='\t' -v DONE="$G/units.done" '
  FILENAME == DONE { k = $7 "\t" $5; s[k] += $3; if ($6 == 1 || $2 != 0) skip[k] = 1; next }
  NF >= 3 { old[$1 "\t" $2] = $3 }
  END { for (k in s) if (!(k in skip)) old[k] = s[k]; for (k in old) print k, old[k] }
' "$G/units.done" "$PREV" | sort > "$OUT/go-times.txt"
echo "gocheck: phase C (the shards) $((TC - TA)) s; $NC of $ND shard(s) answered (cached); test-seconds by pass: $(awk -F'\t' '{ s[$4] += $3 } END { for (m in s) printf "%s %d s  ", m, s[m] }' "$G/units.done")"
echo "gocheck: slowest shards: $(sort -t "$TAB" -k3,3nr "$G/units.done" | head -5 | awk -F'\t' '{ printf "%s %s %ss; ", $4, $5, $3 }')"
if [ "$FAIL" != 0 ]; then echo "GOCHECK FAIL ($(( $(date +%s) - T0 )) s)"; exit 1; fi
case $VET in none) SAY_VET="no vet" ;; ./...) SAY_VET="vet x2" ;; *) SAY_VET="vet x2 on $(echo $VET | wc -w | tr -d ' ') package(s)" ;; esac
SAY_X=", windows build"; [ "$CROSS" != 0 ] || SAY_X=""
echo "GOCHECK OK ($(( $(date +%s) - T0 )) s): $SAY_VET$SAY_X, $TOTAL tests in $NU shards ($NC cached), all green"
