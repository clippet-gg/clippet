#!/usr/bin/env bash
# e2e-registry.sh — the E2E drop-ins' NAMESPACE REGISTRY and CHANGE MAP (run 9, lane `select`; the toil
# proposal's §4 R3 and §3 T1). SOURCED by scripts/e2e.sh (bundle_account and the self-test) and by
# scripts/select.sh; run directly for the two views a person wants:
#
#   bash scripts/lib/e2e-registry.sh --table   the registry as a table (scripts/e2e.d/README.md's view of it)
#   bash scripts/lib/e2e-registry.sh --check   every rule below, against the real tree; exit 1 on a refusal
#
# EVERY drop-in (scripts/e2e.d/*.sh) carries two lines in its first 60 lines, beside its schedule marker:
#
#   # e2e-namespace: account=<suffix>[,<suffix>…] prefix=<VARPREFIX>[,…] buckets=<bucket>[,…]
#   # e2e-covers: <glob>[ <glob>…]
#
# `-` is the explicit "none" for any of the three namespace keys; the covers line is never empty (more
# than one `# e2e-covers:` line is allowed and they are read together).
#
#   account=  the `${E2E_USER}_<suffix>` accounts the bundle REGISTERS on the stack under test — through
#             bundle_account, or by name (`${E2E_USER}_x`, `e2e_$(date +%s)_x`). The core owns `b` — the
#             second user step 3 registers (REG_CORE_ACCOUNTS); a bundle may READ it and never declares it
#             (`c` was the core's too until run 9's lane `pool` made it 20-pages'). bundle_account
#             refuses a suffix the calling bundle does not declare.
#   prefix=   the shell-variable prefixes the bundle OWNS (`AC_`, `DD_`…). No other bundle, and not the
#             harness, assigns a variable that starts with one of them.
#   buckets=  the rate-limit buckets (internal/httpapi: auth upload uploadip view poll bot claim) the
#             bundle DRAINS from the run's own address — a poll loop or more than a handful of tokens
#             in a minute, beyond wait_ready's paced poll, which the planner already paces for everyone.
#             A burst fired with `burst_429 … apart` comes from another address and is not declared.
#             Two bundles that drain one bucket must be `# e2e-schedule-apart:` (either side).
#   covers    globs over the PRODUCT (internal/ cmd/ web/): the files this bundle proves. `*` stays inside
#             one directory, `**` crosses them, `?` is one character. scripts/select.sh maps a diff to
#             bundles through them.
#
# Two more lists live beside the bundles, one row per line, `#` comments allowed:
#
#   scripts/e2e.d/GO-ONLY   `<glob> <TestName>[ <TestName>…]` — product files proved by Go tests alone;
#                           every test named must exist (`func TestName(` in a _test.go file); the tokens
#                           `@cross-compile` and `@not-built` stand in for a file no test on this box runs.
#   scripts/e2e.d/CORE      `<glob>` — files only the harness's own core steps (7-30) prove, or that
#                           every bundle stands on; a change to one selects the WHOLE suite.
#
# THE RULES (reg_check_; the E2E self-test runs them on the real tree and on planted stubs):
#   a bundle with no namespace line, a malformed one, or no covers line is refused; a suffix outside
#   [a-z0-9]{1,3}, a suffix two bundles declare (or one of the core's), a suffix a bundle mints and does
#   not declare; a prefix two bundles declare, or one that is the start of another's, a declared prefix
#   no variable in the file uses, a variable in another file (or the harness) that starts with a
#   declared prefix; an unknown bucket, a bucket two bundles drain without `apart`; a glob that matches
#   no product file (stale); a product file no bundle, CORE row or GO-ONLY row covers; a GO-ONLY test
#   that does not exist. A new product file with no row therefore fails T0's self-test.
#
# Portable: bash 3.2 and a POSIX awk (no gawk-only function), so it runs on the Mac smoke box as well.

REG_CORE_ACCOUNTS="b"
REG_BUCKETS="auth upload uploadip view poll bot claim"
REG_HEAD=60

# reg_field_ <file> <key> → the key's values, space-separated; empty for `-` or a missing key.
reg_field_() {
  # `|| true`: sourced into a `set -euo pipefail` shell, a head that closes its pipe early must not end
  # the caller (a missing file is an empty answer, and the caller says what that means).
  # One awk, not a pipeline: bundle_account calls this once per bundle and a fork is dear on Git Bash.
  awk -v head="$REG_HEAD" -v key="$2" '
    FNR > head { exit }
    /^# *e2e-namespace:/ {
      sub(/\r$/, ""); sub(/^# *e2e-namespace: */, ""); n = split($0, kv, / +/)
      for (i = 1; i <= n; i++) if (index(kv[i], key "=") == 1) {
        v = substr(kv[i], length(key) + 2); if (v == "-") v = ""; gsub(/,/, " ", v); print v; exit
      }
      exit
    }' "$1" 2>/dev/null || true
}
# reg_covers_ <file> → the bundle's covers globs, one per line.
reg_covers_() {
  sed -n "1,${REG_HEAD}{s/^# *e2e-covers: *//p;}" "$1" 2>/dev/null | tr -d '\r' | tr ' ' '\n' | sed '/^$/d'
}
# reg_rows_ <list file> → its rows with comments and blank lines removed.
reg_rows_() { [ -f "$1" ] || return 0; sed -e 's/#.*$//' -e 's/[[:space:]]*$//' -e '/^[[:space:]]*$/d' "$1" | tr -d '\r'; }

# reg_products_ <root> → every non-test product file under internal/ cmd/ web/ (tracked, or untracked
# and not ignored — a new file is a product file the moment it exists), relative to <root>. Test files,
# testdata/ and Markdown are not product. Outside git it walks the directories instead.
reg_products_() {
  (
    cd "$1" || exit 1
    top=$(git rev-parse --show-toplevel 2>/dev/null || true)
    if [ -n "$top" ] && [ "$(cd "$top" 2>/dev/null && pwd)" = "$(pwd)" ]; then
      # A tracked file deleted in the working tree is not a product file any more (`ls-files -d`); the
      # sentinel line keeps grep's pattern list non-empty, since an empty -f list matches nothing.
      gone=$(mktemp 2>/dev/null || mktemp -t reg)
      { git ls-files -d -- internal cmd web 2>/dev/null; echo '//none//'; } > "$gone"
      git ls-files -co --exclude-standard -- internal cmd web 2>/dev/null | grep -vxF -f "$gone" || true
      rm -f "$gone"
    else
      find internal cmd web -type f 2>/dev/null
    fi
  ) | grep -Ev '_test\.go$|(^|/)testdata/|\.md$' | LC_ALL=C sort
}
# reg_tests_ <root> → every Go test function name under internal/ cmd/ web/, one per line.
reg_tests_() {
  ( cd "$1" && find internal cmd web -name '*_test.go' -type f -exec sed -n 's/^func \(Test[A-Za-z0-9_]*\)(.*/\1/p' {} + 2>/dev/null ) \
    | LC_ALL=C sort -u
}

# reg_scan_ <dropin dir> [harness file] → one TSV row per fact, for reg_check_ and the table:
#   NS <stem> <account> <prefix> <buckets> <ok|bad>    (or NONS <stem>)
#   COV <stem> <glob>         APART <stem> <other>      MINT <stem> <suffix>      VAR <stem> <NAME>
# The harness file, when given, contributes VAR rows under the stem `(harness)`.
reg_scan_() {
  local d=$1 h=${2:-} f files=""
  # An EMPTY drop-in gives awk no first line to notice it by, so it is reported here.
  for f in "$d"/*.sh; do
    [ -e "$f" ] || continue
    if [ -s "$f" ]; then files="$files $f"; else printf 'NONS	%s
' "$(basename "$f" .sh)"; fi
  done
  [ -n "$h" ] && [ -f "$h" ] && files="$files $h"
  [ -n "$files" ] || return 0
  # shellcheck disable=SC2086 # a deliberate word list of paths without spaces
  awk -v head="$REG_HEAD" -v harness="$h" '
    function stem_of(p,   s) { s = p; sub(/^.*\//, "", s); sub(/\.sh$/, "", s); return s }
    FNR == 1 {
      if (cur != "" && !isharness && !sawns) print "NONS\t" cur
      cur = stem_of(FILENAME); sawns = 0; isharness = (harness != "" && FILENAME == harness)
      if (isharness) cur = "(harness)"
      delete seenv; delete seenm
    }
    { sub(/\r$/, "") }
    !isharness && FNR <= head && /^# *e2e-namespace:/ && !sawns {
      sawns = 1; line = $0; sub(/^# *e2e-namespace: */, "", line)
      a = ""; p = ""; b = ""; ok = "ok"; n = split(line, kv, / +/)
      for (i = 1; i <= n; i++) {
        if (kv[i] ~ /^account=/) { a = substr(kv[i], 9); na++ }
        else if (kv[i] ~ /^prefix=/) { p = substr(kv[i], 8); np++ }
        else if (kv[i] ~ /^buckets=/) { b = substr(kv[i], 9); nb++ }
        else if (kv[i] != "") ok = "bad"
      }
      if (a == "" || p == "" || b == "") ok = "bad"
      print "NS\t" cur "\t" a "\t" p "\t" b "\t" ok
      next
    }
    !isharness && FNR <= head && /^# *e2e-covers:/ {
      line = $0; sub(/^# *e2e-covers: */, "", line); n = split(line, g, / +/)
      for (i = 1; i <= n; i++) if (g[i] != "") print "COV\t" cur "\t" g[i]
      next
    }
    !isharness && FNR <= 40 && /^# *e2e-schedule-apart:/ {
      line = $0; sub(/^# *e2e-schedule-apart: */, "", line); gsub(/ /, "", line); n = split(line, g, ",")
      for (i = 1; i <= n; i++) if (g[i] != "") print "APART\t" cur "\t" g[i]
      next
    }
    /^[[:space:]]*#/ { next }
    {
      s = $0
      if (!isharness) {
        t = s
        while (match(t, /bundle_account +[a-z0-9A-Z_]+/)) { m = substr(t, RSTART, RLENGTH); sub(/^bundle_account +/, "", m); if (!(m in seenm)) { seenm[m] = 1; print "MINT\t" cur "\t" m }; t = substr(t, RSTART + RLENGTH) }
        t = s
        # bundle_user_ <suffix> (run 9 lane retry): the name an attempt registers, the fresh retry name included.
        while (match(t, /bundle_user_ +[a-z0-9A-Z_]+/)) { m = substr(t, RSTART, RLENGTH); sub(/^bundle_user_ +/, "", m); if (!(m in seenm)) { seenm[m] = 1; print "MINT\t" cur "\t" m }; t = substr(t, RSTART + RLENGTH) }
        t = s
        while (match(t, /E2E_USER\}?_[a-z0-9]+/)) { m = substr(t, RSTART, RLENGTH); sub(/^E2E_USER\}?_/, "", m); if (!(m in seenm)) { seenm[m] = 1; print "MINT\t" cur "\t" m }; t = substr(t, RSTART + RLENGTH) }
        t = s
        while (match(t, /e2e_\$\(date \+%s\)_[a-z0-9]+/)) { m = substr(t, RSTART, RLENGTH); sub(/^.*\)_/, "", m); if (!(m in seenm)) { seenm[m] = 1; print "MINT\t" cur "\t" m }; t = substr(t, RSTART + RLENGTH) }
      }
      # An assignment: NAME= at a command position — the start of the line, after ; & | ( { or a space
      # (which is also how `local X=` and an `-e NAME=` inside a probe command read).
      t = " " s
      while (match(t, /[ \t;&|({][A-Z][A-Z0-9_]*=/)) {
        m = substr(t, RSTART + 1, RLENGTH - 2)
        if (!(m in seenv)) { seenv[m] = 1; print "VAR\t" cur "\t" m }
        t = substr(t, RSTART + RLENGTH)
      }
    }
    END { if (cur != "" && !isharness && !sawns) print "NONS\t" cur }
  ' $files
}

# reg_check_ <dropin dir> <repo root> → prints one `REFUSE <rule>: <why>` line per violation and returns
# 1 when there is any; prints nothing and returns 0 on a clean registry. The harness file is
# <repo root>/scripts/e2e.sh when it exists (its variables are checked against the declared prefixes).
reg_check_() {
  local d=$1 root=$2 tmp h="" rc=0
  tmp=$(mktemp -d 2>/dev/null || mktemp -d -t reg)
  [ -f "$root/scripts/e2e.sh" ] && h="$root/scripts/e2e.sh"
  reg_scan_ "$d" "$h" > "$tmp/scan"
  reg_products_ "$root" > "$tmp/products"
  reg_tests_ "$root" > "$tmp/tests"
  reg_rows_ "$d/GO-ONLY" > "$tmp/goonly"
  reg_rows_ "$d/CORE" > "$tmp/core"
  # Every drop-in must PARSE (lane autoe2e): a file bash stops reading half-way is sourced up to the error and
  # its bundle has been seen to pass with the rest of its assertions never run. Checked by the same bash that
  # sources it, so a quirk of that bash (an apostrophe inside "${x:-…}") is caught as it would bite.
  local f
  for f in "$d"/*.sh; do
    [ -e "$f" ] || continue
    if ! "${BASH:-bash}" -n "$f" > "$tmp/syntax" 2>&1; then
      echo "REFUSE syntax: $(basename "$f") does not parse (bash -n): $(head -1 "$tmp/syntax" | sed 's|^.*/||')"; rc=1
    fi
  done
  awk -F'\t' -v core="$REG_CORE_ACCOUNTS" -v buckets="$REG_BUCKETS" '
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
    function refuse(rule, why) { print "REFUSE " rule ": " why; bad = 1 }
    BEGIN { n = split(core, cs, " "); for (i = 1; i <= n; i++) coreacct[cs[i]] = 1
            n = split(buckets, bs, " "); for (i = 1; i <= n; i++) knownb[bs[i]] = 1 }
    FILENAME == ARGV[1] {
      if ($1 == "NONS") { refuse("no-namespace", $2 ".sh has no \"# e2e-namespace: account=… prefix=… buckets=…\" line in its first 60 lines"); stems[$2] = 1 }
      else if ($1 == "NS") {
        stems[$2] = 1
        if ($6 != "ok") refuse("bad-namespace", $2 ".sh: the namespace line must carry exactly account=, prefix= and buckets= (\"-\" for none)")
        if ($3 != "-") { n = split($3, xs, ","); for (i = 1; i <= n; i++) {
          s = xs[i]
          if (s !~ /^[a-z0-9][a-z0-9]?[a-z0-9]?$/) refuse("bad-suffix", $2 ".sh declares account suffix \"" s "\": 1-3 of [a-z0-9], the only shape the sweep can reach")
          else if (s in coreacct) refuse("dup-suffix", $2 ".sh declares account suffix \"" s "\", which is the core harness'"'"'s own")
          else if (s in acct) refuse("dup-suffix", "account suffix \"" s "\" is declared by " acct[s] ".sh AND " $2 ".sh (two bundles registering one account is a 409, gate 27a)")
          else acct[s] = $2
          decl[$2 SUBSEP s] = 1 } }
        if ($4 != "-") { n = split($4, xs, ","); for (i = 1; i <= n; i++) {
          p = xs[i]
          if (p !~ /^[A-Z][A-Z0-9]*_?$/) { refuse("bad-prefix", $2 ".sh declares prefix \"" p "\": capitals and digits, optionally ending in _"); continue }
          if (p in pfx) { refuse("dup-prefix", "prefix " p " is declared by " pfx[p] ".sh AND " $2 ".sh"); continue }
          pfx[p] = $2; npfx++; plist[npfx] = p } }
        if ($5 != "-") { n = split($5, xs, ","); for (i = 1; i <= n; i++) {
          b = xs[i]
          if (!(b in knownb)) { refuse("bad-bucket", $2 ".sh declares bucket \"" b "\"; the buckets are: " buckets); continue }
          nbk++; bk_b[nbk] = b; bk_s[nbk] = $2 } }
      }
      else if ($1 == "COV") { ncov++; cov_s[ncov] = $2; cov_g[ncov] = $3; cov_re[ncov] = g2re($3); hascov[$2] = 1 }
      else if ($1 == "APART") { apart[$2 SUBSEP $3] = 1; apart[$3 SUBSEP $2] = 1 }
      else if ($1 == "MINT") { nmint++; mint_s[nmint] = $2; mint_x[nmint] = $3 }
      else if ($1 == "VAR") { nvar++; var_s[nvar] = $2; var_n[nvar] = $3 }
      next
    }
    FILENAME == ARGV[2] { nprod++; prod[nprod] = $0; next }
    FILENAME == ARGV[3] { tests[$0] = 1; next }
    FILENAME == ARGV[4] { ngo++; split($0, gw, /[ \t]+/); go_g[ngo] = gw[1]; go_re[ngo] = g2re(gw[1]); go_t[ngo] = $0; next }
    FILENAME == ARGV[5] { ncore++; core_g[ncore] = $1; core_re[ncore] = g2re($1); next }
    END {
      for (s in stems) if (!(s in hascov)) refuse("no-covers", s ".sh has no \"# e2e-covers: <glob> …\" line: name the product files it proves")
      # Prefixes: no one is the start of another; each is used in its own file; nobody else assigns into it.
      for (i = 1; i <= npfx; i++) for (j = 1; j <= npfx; j++) if (i != j && pfx[plist[i]] != pfx[plist[j]] && index(plist[j], plist[i]) == 1)
        refuse("dup-prefix", "prefix " plist[i] " (" pfx[plist[i]] ".sh) is the start of " plist[j] " (" pfx[plist[j]] ".sh): one namespace inside another")
      for (i = 1; i <= npfx; i++) {
        p = plist[i]; used = 0
        for (k = 1; k <= nvar; k++) if (index(var_n[k], p) == 1) {
          if (var_s[k] == pfx[p]) used = 1
          else if (!((var_s[k] SUBSEP p) in told)) { told[var_s[k] SUBSEP p] = 1
            refuse("foreign-prefix", (var_s[k] == "(harness)" ? "scripts/e2e.sh" : var_s[k] ".sh") " assigns " var_n[k] ", inside " pfx[p] ".sh'"'"'s prefix " p) }
        }
        if (!used) refuse("unused-prefix", pfx[p] ".sh declares prefix " p " and assigns no variable that starts with it")
      }
      # A suffix the file mints must be declared (the core accounts may be read by anyone).
      for (i = 1; i <= nmint; i++) { s = mint_x[i]
        if (s in coreacct) continue
        if (!((mint_s[i] SUBSEP s) in decl)) refuse("undeclared-suffix", mint_s[i] ".sh registers or names the account suffix \"" s "\" and its namespace line does not declare it") }
      # A bucket two bundles drain must be kept apart.
      for (i = 1; i <= nbk; i++) for (j = i + 1; j <= nbk; j++) if (bk_b[i] == bk_b[j] && bk_s[i] != bk_s[j] && !((bk_s[i] SUBSEP bk_s[j]) in apart))
        refuse("bucket-clash", bk_s[i] ".sh and " bk_s[j] ".sh both drain the " bk_b[i] " bucket and neither declares \"# e2e-schedule-apart:\" the other")
      # Globs: every one matches a product file; every product file is matched by one.
      for (k = 1; k <= nprod; k++) {
        f = prod[k]; hit = 0
        for (i = 1; i <= ncov; i++) if (f ~ cov_re[i]) { hit = 1; covhit[i] = 1 }
        for (i = 1; i <= ngo; i++) if (f ~ go_re[i]) { hit = 1; gohit[i] = 1 }
        for (i = 1; i <= ncore; i++) if (f ~ core_re[i]) { hit = 1; corehit[i] = 1 }
        if (!hit) refuse("uncovered", f " is covered by no bundle'"'"'s \"# e2e-covers:\" glob, no scripts/e2e.d/CORE row and no scripts/e2e.d/GO-ONLY row")
      }
      for (i = 1; i <= ncov; i++) if (!(i in covhit)) refuse("stale-glob", cov_s[i] ".sh covers \"" cov_g[i] "\", which matches no product file")
      for (i = 1; i <= ngo; i++) {
        if (!(i in gohit)) refuse("stale-glob", "GO-ONLY row \"" go_g[i] "\" matches no product file")
        n = split(go_t[i], gw, /[ \t]+/)
        if (n < 2) refuse("go-only-test", "GO-ONLY row \"" go_g[i] "\" names no Go test")
        for (j = 2; j <= n; j++) if (gw[j] != "@cross-compile" && gw[j] != "@not-built" && !(gw[j] in tests)) refuse("go-only-test", "GO-ONLY row \"" go_g[i] "\" names " gw[j] ", and there is no such test")
      }
      for (i = 1; i <= ncore; i++) if (!(i in corehit)) refuse("stale-glob", "CORE row \"" core_g[i] "\" matches no product file")
      exit bad
    }
  ' "$tmp/scan" "$tmp/products" "$tmp/tests" "$tmp/goonly" "$tmp/core" || rc=1
  rm -rf "$tmp"
  return "$rc"
}

# reg_table_ <dropin dir> → the registry as a Markdown table (the README's generated view): one row per
# bundle with its accounts, prefixes and buckets, then every apart pair with the buckets it shares.
reg_table_() {
  local d=$1
  reg_scan_ "$d" | awk -F'\t' '
    $1 == "NS" { n++; st[n] = $2; a[$2] = $3; p[$2] = $4; b[$2] = $5 }
    $1 == "NONS" { n++; st[n] = $2; a[$2] = "?"; p[$2] = "?"; b[$2] = "?" }
    $1 == "APART" { if ($2 < $3) k = $2 "|" $3; else k = $3 "|" $2; if (!(k in ap)) { na++; ap[k] = 1; al[na] = k } }
    END {
      print "| bundle | accounts (`${E2E_USER}_<suffix>`) | variable prefixes | buckets it drains |"
      print "|---|---|---|---|"
      for (i = 1; i <= n; i++) printf "| %s | %s | %s | %s |\n", st[i], a[st[i]], p[st[i]], b[st[i]]
      print ""
      print "| apart pair | buckets both drain |"
      print "|---|---|"
      for (i = 1; i <= na; i++) {
        split(al[i], xy, "|"); sh = ""
        nx = split(b[xy[1]], bx, ","); ny = split(b[xy[2]], by, ",")
        for (x = 1; x <= nx; x++) for (y = 1; y <= ny; y++) if (bx[x] == by[y] && bx[x] != "-") sh = sh (sh == "" ? "" : ",") bx[x]
        printf "| %s / %s | %s |\n", xy[1], xy[2], (sh == "" ? "none: apart for another reason, named in the file" : sh)
      }
    }'
}

if [ "${BASH_SOURCE[0]:-$0}" = "$0" ]; then
  set -eu
  cd "$(dirname "$0")/../.."
  case ${1:-} in
    --table) reg_table_ scripts/e2e.d ;;
    --check) if reg_check_ scripts/e2e.d .; then echo "e2e-registry: OK"; else echo "e2e-registry: REFUSED (the lines above)" >&2; exit 1; fi ;;
    *) echo "usage: bash scripts/lib/e2e-registry.sh --table | --check" >&2; exit 2 ;;
  esac
fi
