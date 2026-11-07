#!/usr/bin/env bash
# select.sh — which E2E bundles a diff owes (run 9, lane `select`; the toil proposal's §3 T1). It reads
# the change map every drop-in carries (`# e2e-covers:` globs, scripts/lib/e2e-registry.sh) and prints the
# value for `E2E_ONLY`:
#
#   bash scripts/select.sh                       the diff `git diff --name-only ${SELECT_BASE:-main}...HEAD`,
#                                                plus what is changed and not committed (staged, unstaged and
#                                                untracked) — a lane's T1 runs on the tree it has
#   bash scripts/select.sh --range A...B         that range instead (read-only; `A..B` works too)
#   bash scripts/select.sh --files "a b c"       these paths instead (space- or newline-separated)
#   … --why                                      also print, on stderr, every path and what it selected
#   … --go                                       print the Go packages of the diff instead (for t0.sh / t1.sh)
#   … --plan                                     print the whole answer for a person: the self-test, E2E_ONLY,
#                                                the Go packages and every path's reason
#
# The value on stdout is ONE word or list:
#
#   T2       a SHAPE path changed — compose*.yaml, Dockerfile*, .dockerignore, internal/config/**,
#            internal/db/migrations/**, cmd/clippet/serve.go, internal/store/**, scripts/e2e.sh,
#            scripts/lib/*.sh, scripts/lib/jget.py, go.mod, go.sum: the caller owes the full gate (T2)
#   all      a product path no bundle covers, a CORE path (scripts/e2e.d/CORE), or a path this script
#            does not know — the WHOLE suite, fail-safe; --why names the path
#   none     docs and records only (*.md, docs/**, scratchpad/**), Go tests, GO-ONLY files, or harness
#            tools outside the E2E: no bundle
#            (a file under scripts/e2e.d/<stem>/ — a drop-in's own program, 110-autohost's mock — selects <stem> and
#            every other drop-in that names `scripts/e2e.d/<stem>` to build it: 111-autobot runs that mock too)
#   a,b,c    the union of the bundles whose globs match (and any drop-in that itself changed), longest
#            first by the last recorded run (.e2e/checkpoints/durations__*.txt; name order without one)
#
# Whatever the value, the E2E self-test (`E2E_SELFTEST=1 bash scripts/e2e.sh`, no stack) is owed too: it
# is what checks the map itself. `--plan` says so on its first line.
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/lib/e2e-registry.sh

SEL_WHY=0; SEL_MODE=value; SEL_FILES=; SEL_RANGE=; SEL_HAVE_FILES=0
while [ $# -gt 0 ]; do
  case $1 in
    --why) SEL_WHY=1 ;;
    --go) SEL_MODE=go ;;
    --plan) SEL_MODE=plan; SEL_WHY=1 ;;
    --files) shift; SEL_FILES=${1:-}; SEL_HAVE_FILES=1 ;;
    --range) shift; SEL_RANGE=${1:-} ;;
    -h|--help) sed -n '2,32p' "$0"; exit 0 ;;
    *) echo "select.sh: unknown argument '$1' (--why, --go, --plan, --files \"…\", --range A...B)" >&2; exit 2 ;;
  esac
  shift
done

SEL_TMP=$(mktemp -d 2>/dev/null || mktemp -d -t select)
trap 'rm -rf "$SEL_TMP"' EXIT
if [ "$SEL_HAVE_FILES" = 1 ]; then
  printf '%s\n' "$SEL_FILES" | tr ' ' '\n' | sed '/^$/d' > "$SEL_TMP/paths"
elif [ -n "$SEL_RANGE" ]; then
  git diff --name-only "$SEL_RANGE" > "$SEL_TMP/paths"
else
  base=${SELECT_BASE:-main}
  { git diff --name-only "$base...HEAD"; git diff --name-only HEAD; git ls-files -o --exclude-standard; } > "$SEL_TMP/paths"
fi
LC_ALL=C sort -u "$SEL_TMP/paths" -o "$SEL_TMP/paths"

reg_scan_ scripts/e2e.d > "$SEL_TMP/scan"
reg_rows_ scripts/e2e.d/CORE > "$SEL_TMP/core"
reg_rows_ scripts/e2e.d/GO-ONLY > "$SEL_TMP/goonly"
ls scripts/e2e.d/*.sh 2>/dev/null | sed 's|.*/||; s|\.sh$||' > "$SEL_TMP/stems"
# <dir> TAB <stem>: each drop-in that names a drop-in's own program directory (it builds that program).
: > "$SEL_TMP/progusers"
for sel_d in scripts/e2e.d/*/; do
  [ -d "$sel_d" ] || continue
  sel_d=${sel_d%/}; sel_d=${sel_d##*/}
  for sel_f in $(grep -lF "scripts/e2e.d/$sel_d" scripts/e2e.d/*.sh 2>/dev/null); do
    sel_f=${sel_f##*/}; printf '%s\t%s\n' "$sel_d" "${sel_f%.sh}" >> "$SEL_TMP/progusers"
  done
done

# classify: one row per path — <path> TAB <class> TAB <detail>. Classes: shape, doc, gotest, goonly, core,
# bundle (detail: the stems), dropin (detail: the stem), prog (detail: the stems), tool, uncovered, unknown.
awk -F'\t' '
  function g2re(g,   r, i, c, n) {
    r = ""; n = length(g)
    for (i = 1; i <= n; i++) {
      c = substr(g, i, 1)
      if (c == "*") {
        if (substr(g, i + 1, 1) == "*") {
          if (substr(g, i + 2, 1) == "/") { r = r "(.*/)?"; i += 2 } else { r = r ".*"; i++ }
        } else r = r "[^/]*"
      } else if (c == "?") r = r "[^/]"
      else if (index(".+()|^$[]{}\\", c)) r = r "\\" c
      else r = r c
    }
    return "^" r "$"
  }
  BEGIN {
    ns = split("compose*.yaml Dockerfile* .dockerignore internal/config/** internal/db/migrations/** cmd/clippet/serve.go internal/store/** scripts/e2e.sh scripts/lib/*.sh scripts/lib/jget.py go.mod go.sum", sg, " ")
    for (i = 1; i <= ns; i++) shape[i] = g2re(sg[i])
    nd = split("**/*.md docs/** scratchpad/**", dg, " ")
    for (i = 1; i <= nd; i++) doc[i] = g2re(dg[i])
    nt = split("scripts/check.sh scripts/gate.sh scripts/visual.sh scripts/lane-teardown.sh scripts/box-tidy.sh scripts/fsdefault.sh scripts/select.sh scripts/t0.sh scripts/t1.sh scripts/workflows/** scripts/lib/visual.js scripts/lib/navretry.js scripts/lib/go-weights.txt scripts/smoke-public.sh scripts/lib/mk*.py scripts/e2e.d/GO-ONLY scripts/e2e.d/CORE scripts/lib/pii-*.txt .gitattributes .gitignore .gitleaks.toml .mcp.json .github/**", tg, " ")
    for (i = 1; i <= nt; i++) tool[i] = g2re(tg[i])
  }
  FILENAME == ARGV[1] { if ($1 == "COV") { nc++; cs[nc] = $2; cr[nc] = g2re($3) } ; next }
  FILENAME == ARGV[2] { ncore++; corer[ncore] = g2re($1); next }
  FILENAME == ARGV[3] { split($0, w, /[ \t]+/); ngo++; gor[ngo] = g2re(w[1]); next }
  FILENAME == ARGV[4] { isstem[$0] = 1; next }
  FILENAME == ARGV[5] { if (index("," users[$1] ",", "," $2 ",") == 0) users[$1] = users[$1] (users[$1] == "" ? "" : ",") $2; next }
  {
    p = $0
    for (i = 1; i <= ns; i++) if (p ~ shape[i]) { print p "\tshape\t"; next }
    for (i = 1; i <= nd; i++) if (p ~ doc[i]) { print p "\tdoc\t"; next }
    if (p ~ /^scripts\/e2e\.d\/[^\/]*\.sh$/) {
      s = p; sub(/^scripts\/e2e\.d\//, "", s); sub(/\.sh$/, "", s)
      if (s in isstem) print p "\tdropin\t" s; else print p "\ttool\t(a drop-in that no longer exists)"
      next
    }
    if (p ~ /^scripts\/e2e\.d\/[^\/]+\//) {
      # the own directory of a drop-in (scripts/e2e.d/<stem>/, the mock of 110-autohost) selects that drop-in and
      # every drop-in that builds it (111-autobot)
      s = p; sub(/^scripts\/e2e\.d\//, "", s); sub(/\/.*$/, "", s)
      hit = (s in isstem) ? s : ""
      if (s in users) { n = split(users[s], u, ","); for (i = 1; i <= n; i++) if (index("," hit ",", "," u[i] ",") == 0) hit = hit (hit == "" ? "" : ",") u[i] }
      if (hit != "") { print p "\tprog\t" hit; next }
    }
    for (i = 1; i <= nt; i++) if (p ~ tool[i]) { print p "\ttool\t"; next }
    if (p ~ /^(internal|cmd|web)\//) {
      if (p ~ /_test\.go$/ || p ~ /(^|\/)testdata\//) { print p "\tgotest\t"; next }
      hit = ""
      for (i = 1; i <= nc; i++) if (p ~ cr[i] && index("," hit ",", "," cs[i] ",") == 0) hit = hit (hit == "" ? "" : ",") cs[i]
      if (hit != "") { print p "\tbundle\t" hit; next }
      for (i = 1; i <= ncore; i++) if (p ~ corer[i]) { print p "\tcore\t"; next }
      for (i = 1; i <= ngo; i++) if (p ~ gor[i]) { print p "\tgoonly\t"; next }
      print p "\tuncovered\t"; next
    }
    print p "\tunknown\t"
  }
' "$SEL_TMP/scan" "$SEL_TMP/core" "$SEL_TMP/goonly" "$SEL_TMP/stems" "$SEL_TMP/progusers" "$SEL_TMP/paths" > "$SEL_TMP/class"

# The bundles the map names, whatever the verdict — longest first by the last recorded run.
cut -f2,3 "$SEL_TMP/class" | awk -F'\t' '$1 == "bundle" || $1 == "dropin" || $1 == "prog" { n = split($2, s, ","); for (i = 1; i <= n; i++) print s[i] }' \
  | LC_ALL=C sort -u > "$SEL_TMP/picked"
: > "$SEL_TMP/dur"
for f in .e2e/checkpoints/durations__*.txt; do [ -f "$f" ] && cat "$f" >> "$SEL_TMP/dur"; done
SEL_BUNDLES=$(awk -F'\t' '
  FILENAME == ARGV[1] { if ($2 ~ /^[0-9]+$/ && $2 + 0 > d[$1]) d[$1] = $2 + 0; next }
  { n++; s[n] = $1 }
  END {
    for (i = 1; i <= n; i++) for (j = i + 1; j <= n; j++)
      if (d[s[j]] > d[s[i]] || (d[s[j]] == d[s[i]] && s[j] + 0 < s[i] + 0)) { t = s[i]; s[i] = s[j]; s[j] = t }
    for (i = 1; i <= n; i++) printf "%s%s", (i > 1 ? "," : ""), s[i]
  }' "$SEL_TMP/dur" "$SEL_TMP/picked")

if grep -q $'\tshape\t' "$SEL_TMP/class"; then SEL_VALUE=T2
elif grep -qE $'\t(core|uncovered|unknown)\t' "$SEL_TMP/class"; then SEL_VALUE=all
elif [ -n "$SEL_BUNDLES" ]; then SEL_VALUE=$SEL_BUNDLES
else SEL_VALUE=none
fi

# The Go packages of the diff: the directory of each changed file under internal/ cmd/ web/, walked up to
# the nearest one that holds a .go file; a template or a static file adds ./internal/web as well, whose
# tests render them.
SEL_GO=$(cut -f1 "$SEL_TMP/class" | { grep -E '^(internal|cmd|web)/' || true; } | while IFS= read -r p; do
  d=${p%/*}
  # (pat) with its leading paren: macOS's bash 3.2 misparses a bare pat) inside $( ).
  while [ -n "$d" ] && [ "$d" != . ] && ! ls "$d"/*.go >/dev/null 2>&1; do
    case $d in (*/*) d=${d%/*} ;; (*) d= ;; esac
  done
  [ -z "$d" ] || printf './%s\n' "$d"
  case $p in (web/templates/*|web/static/*) printf './internal/web\n' ;; esac
done | LC_ALL=C sort -u | tr '\n' ' ' | sed 's/ $//')

sel_why_() {
  awk -F'\t' '
    $2 == "shape"     { print "  " $1 " -> T2 (a shape path: the full gate)"; next }
    $2 == "doc"       { print "  " $1 " -> none (docs/records)"; next }
    $2 == "gotest"    { print "  " $1 " -> none (a Go test: the Go pass)"; next }
    $2 == "goonly"    { print "  " $1 " -> none (scripts/e2e.d/GO-ONLY: proved by Go tests)"; next }
    $2 == "core"      { print "  " $1 " -> all (scripts/e2e.d/CORE: only the core steps or every bundle prove it)"; next }
    $2 == "bundle"    { print "  " $1 " -> " $3; next }
    $2 == "dropin"    { print "  " $1 " -> " $3 " (the drop-in itself)"; next }
    $2 == "prog"      { print "  " $1 " -> " $3 " (a drop-in'"'"'s own program: its drop-in and every drop-in that builds it)"; next }
    $2 == "tool"      { print "  " $1 " -> none (a harness tool outside the E2E; the self-test checks the harness)" ($3 == "" ? "" : " " $3); next }
    $2 == "uncovered" { print "  " $1 " -> all (UNCOVERED: no bundle, CORE or GO-ONLY row names it — the self-test fails on it too)"; next }
    $2 == "unknown"   { print "  " $1 " -> all (a path this script does not classify: fail-safe)"; next }
  ' "$SEL_TMP/class"
  [ "$SEL_VALUE" = "$SEL_BUNDLES" ] || [ -z "$SEL_BUNDLES" ] || echo "  (the bundles the map names, inside the $SEL_VALUE above: $SEL_BUNDLES)"
}

case $SEL_MODE in
  value)
    printf '%s\n' "$SEL_VALUE"
    if [ "$SEL_WHY" = 1 ]; then { echo "select.sh: $(wc -l < "$SEL_TMP/paths" | tr -d ' ') path(s):"; sel_why_; } >&2; fi ;;
  go)
    printf '%s\n' "${SEL_GO:-none}"
    if [ "$SEL_WHY" = 1 ]; then { echo "select.sh: $(wc -l < "$SEL_TMP/paths" | tr -d ' ') path(s):"; sel_why_; } >&2; fi ;;
  plan)
    echo "selftest: E2E_SELFTEST=1 bash scripts/e2e.sh (always: it checks the map and the registry themselves)"
    echo "e2e:      $SEL_VALUE"
    echo "go:       ${SEL_GO:-none}"
    echo "why ($(wc -l < "$SEL_TMP/paths" | tr -d ' ') path(s)):"
    sel_why_ ;;
esac
