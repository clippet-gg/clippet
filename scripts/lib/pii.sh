#!/usr/bin/env bash
# pii.sh — check.sh's stage `pii`: no tracked text file carries a personal value or a machine-shaped path.
#
#   bash scripts/lib/pii.sh [repo [pathspec…]]  # scan the git checkout at repo (default: the current directory),
#                                               #   all of it or only the tracked files under the pathspecs
#   PII_SELFTEST=1 bash scripts/lib/pii.sh  # the pins, against a scratch git repository; no stack, no network
#
# TWO HALVES, one extended regular expression per line each, matched over `git ls-files` (text files only:
# grep -I; never docs/brand/*.png, web/static/fonts/ or go.sum):
#   - PUBLIC: scripts/lib/pii-public.txt, in this tree — impersonal SHAPES only (a Windows or macOS home
#     folder, a personal mail domain, a private registry path, a cloud id beside its label), matched
#     CASE-SENSITIVELY: under -i the macOS home `/Users/<name>/` is any `/users/<word>/` path segment (DEPLOY's
#     `schema/users/clips/jobs`), so a shape that takes either case spells it.
#   - PRIVATE: the file PII_DENY_FILE names (default ../clippet-internal/pii-deny.txt, beside the checkout) —
#     the VALUES (names, addresses, ids), matched case-insensitively. Read only when it exists; otherwise one
#     line says the half is off.
#     CI writes it from a repository secret to a temporary file and points PII_DENY_FILE at it.
# A hit prints `path:line  pattern #k (<file name>)` — k is the pattern's line number in its file — and NEVER
# the matched text, so a log of a failing run discloses nothing the deny file holds. Any hit fails (exit 1).
# scripts/lib/pii-allow.txt lists the few files that carry a PUBLIC shape on purpose (one pattern each).
# Exit: 0 clean, 1 a hit, 2 a pattern file that cannot be read or a pattern grep refuses.
set -uo pipefail

PII_HERE=$(cd "$(dirname "$0")" && pwd)
PII_PUBLIC=${PII_PUBLIC_FILE:-$PII_HERE/pii-public.txt}
PII_ALLOW=${PII_ALLOW_FILE:-$PII_HERE/pii-allow.txt}

# pii_files_ — the tracked files to read, NUL-separated, from the checkout in the current directory.
PII_SPECS=(.)
pii_files_() {
  git ls-files -z -- "${PII_SPECS[@]}" ':(exclude)go.sum' ':(exclude)web/static/fonts/' ':(exclude,glob)docs/brand/*.png'
}

# pii_half_ <pattern file> — every hit of every pattern in that file, as `path:line  pattern #k (<name>)`.
# Every pattern is tried first (a bad one is exit 2, named by number, before anything is scanned).
pii_half_() {
  local file=$1 name k pat rc out hit icase=-i
  name=$(basename "$file")
  [ "$file" = "$PII_PUBLIC" ] && icase=
  k=0
  while IFS= read -r pat || [ -n "$pat" ]; do
    k=$((k + 1)); pat=${pat%$'\r'}
    case $pat in ''|'#'*) continue ;; esac
    rc=0; printf '' | grep -E -e "$pat" >/dev/null 2>&1 || rc=$?
    [ "$rc" -le 1 ] || { echo "pii: pattern #$k ($name) is not a valid extended regular expression"; return 2; }
  done < "$file"
  k=0
  while IFS= read -r pat || [ -n "$pat" ]; do
    k=$((k + 1)); pat=${pat%$'\r'}
    case $pat in ''|'#'*) continue ;; esac
    rc=0
    # grep prints path:line:text; only the first two fields leave this pipe, so the text is never printed.
    out=$(pii_files_ | xargs -0 grep -I $icase -n -H -E -e "$pat" -- 2>/dev/null | cut -d: -f1,2) || rc=$?
    # xargs is 123 (GNU) or 1 (BSD) when a grep in it found nothing (a refusal is ruled out above); 125 and up
    # is xargs' own failure.
    case $rc in 0|1|123) ;; *) echo "pii: pattern #$k ($name) could not be run (exit $rc)"; return 2 ;; esac
    while IFS= read -r hit; do
      [ -n "$hit" ] || continue
      if [ "$name" = pii-public.txt ] && [ -f "$PII_ALLOW" ] \
         && awk -v p="${hit%%:*}" -v k="pii-public.txt#$k" '$1 == p && $2 == k { f = 1 } END { exit !f }' "$PII_ALLOW"; then
        continue
      fi
      printf '%s  pattern #%s (%s)\n' "$hit" "$k" "$name"
    done <<EOF_HITS
$out
EOF_HITS
  done < "$file"
}

pii_scan_() {
  local repo=$1 deny hits rc=0 r
  cd "$repo" || { echo "pii: no such directory"; return 2; }
  git rev-parse --is-inside-work-tree >/dev/null 2>&1 || { echo "pii: not a git checkout"; return 2; }
  [ -r "$PII_PUBLIC" ] || { echo "pii: the public patterns ($(basename "$PII_PUBLIC")) cannot be read"; return 2; }
  hits=$(pii_half_ "$PII_PUBLIC"); r=$?
  [ "$r" = 2 ] && { printf '%s\n' "$hits"; return 2; }
  deny=${PII_DENY_FILE:-../clippet-internal/pii-deny.txt}
  if [ -f "$deny" ]; then
    [ -r "$deny" ] || { echo "pii: the private deny file ($(basename "$deny")) cannot be read"; return 2; }
    local ph; ph=$(pii_half_ "$deny"); r=$?
    [ "$r" = 2 ] && { printf '%s\n' "$ph"; return 2; }
    hits=$(printf '%s\n%s\n' "$hits" "$ph")
    echo "pii: the private half is on ($(basename "$deny"))"
  else
    echo "pii: the private half is off (no private deny file; set PII_DENY_FILE)"
  fi
  hits=$(printf '%s\n' "$hits" | sed '/^$/d')
  if [ -n "$hits" ]; then
    printf '%s\n' "$hits"
    echo "pii: FAIL — $(printf '%s\n' "$hits" | wc -l | tr -d ' ') hit(s) above: remove the value, or name the file and pattern in scripts/lib/pii-allow.txt with its reason (public shapes only)"
    rc=1
  else
    echo "pii: ok — no tracked text file matches a pattern"
  fi
  return $rc
}

# ---- the self-test: one hit per half planted in a scratch repository --------------------------------------
pii_selftest_() {
  local t n=0 bad=0 out rc pub priv
  t=$(mktemp -d)
  ok_() { n=$((n + 1)); [ "$1" = "$2" ] || { bad=$((bad + 1)); echo "[pii selftest] FAIL: $3 (got '$1', want '$2')"; }; }
  # The planted values are ASSEMBLED, so this file's own text never matches the patterns it pins.
  pub="/Us""ers/Zed""wick/notes"                                         # a capitalised name: still a hit
  priv="Quill""Farrow"                                                  # the deny line is lower case: -i there
  seg="schema/us""ers/clips/jobs"                                       # a path segment, not a home: no hit
  mkdir -p "$t/repo/docs/brand" "$t/repo/sub"
  printf 'one\nsee %s for it\n' "$pub" > "$t/repo/sub/a.txt"
  printf 'x\ny\nsigned %s\n' "$priv" > "$t/repo/b.md"
  printf 'clean\n%s\n' "$seg" > "$t/repo/c.txt"
  printf '%s\n' "$pub" > "$t/repo/go.sum"                               # excluded by name
  printf '\000\001%s\n' "$pub" > "$t/repo/bin.dat"                       # binary: grep -I skips it
  printf '# a comment line\n\nquill''farrow\n' > "$t/deny.txt"            # the stub private half: pattern #3
  printf 'compose.x.yaml  pii-public.txt#7\n' > "$t/allow.txt"
  git -C "$t/repo" init -q && git -C "$t/repo" add -A
  # 1. one hit per half: exit 1, both named by path, line and pattern number, the text NEVER printed
  rc=0; out=$(PII_DENY_FILE="$t/deny.txt" PII_ALLOW_FILE="$t/allow.txt" PII_SELFTEST=0 bash "$0" "$t/repo" 2>&1) || rc=$?
  ok_ "$rc" 1 "a planted hit in each half fails the stage"
  ok_ "$(printf '%s\n' "$out" | grep -c '^sub/a.txt:2  pattern #8 (pii-public.txt)$')" 1 "the public hit is named path:line pattern #k"
  ok_ "$(printf '%s\n' "$out" | grep -c '^b.md:3  pattern #3 (deny.txt)$')" 1 "the private hit is named by the deny file's line number"
  ok_ "$(printf '%s\n' "$out" | grep -ci -e 'zedwick' -e 'quillfarrow')" 0 "the matched text never appears in the output"
  ok_ "$(printf '%s\n' "$out" | grep -c -e '^go.sum' -e '^bin.dat')" 0 "go.sum and a binary file are not read"
  ok_ "$(printf '%s\n' "$out" | grep -c '^c.txt')" 0 "the public half is case-sensitive: a lower-case /users/ path segment is not a home"
  ok_ "$(printf '%s\n' "$out" | grep -c 'private half is on (deny.txt)')" 1 "the private half says it is on"
  # 2. the deny file absent: the private half is off, said in one line; the public hit still fails
  rc=0; out=$(PII_DENY_FILE="$t/none.txt" PII_ALLOW_FILE="$t/allow.txt" PII_SELFTEST=0 bash "$0" "$t/repo" 2>&1) || rc=$?
  ok_ "$rc" 1 "without a deny file the public half still fails"
  ok_ "$(printf '%s\n' "$out" | grep -c 'private half is off')" 1 "…and one line says the private half is off"
  ok_ "$(printf '%s\n' "$out" | grep -c 'deny.txt')" 0 "…and nothing of the private half is reported"
  # 3. an allow line covers its own file and pattern only
  printf 'sub/a.txt  pii-public.txt#8\n' > "$t/allow.txt"
  rc=0; out=$(PII_DENY_FILE="$t/none.txt" PII_ALLOW_FILE="$t/allow.txt" PII_SELFTEST=0 bash "$0" "$t/repo" 2>&1) || rc=$?
  ok_ "$rc" 0 "an allowed file and pattern pass"
  printf 'sub/a.txt  pii-public.txt#7\n' > "$t/allow.txt"
  rc=0; out=$(PII_DENY_FILE="$t/none.txt" PII_ALLOW_FILE="$t/allow.txt" PII_SELFTEST=0 bash "$0" "$t/repo" 2>&1) || rc=$?
  ok_ "$rc" 1 "…and an allow line for another pattern does not"
  # 4. no hit: exit 0
  printf 'one\nclean now\n' > "$t/repo/sub/a.txt"; printf 'x\ny\nsigned\n' > "$t/repo/b.md"
  rc=0; out=$(PII_DENY_FILE="$t/deny.txt" PII_ALLOW_FILE="$t/none" PII_SELFTEST=0 bash "$0" "$t/repo" 2>&1) || rc=$?
  ok_ "$rc" 0 "a clean tree passes with both halves on"
  ok_ "$(printf '%s\n' "$out" | tail -1)" "pii: ok — no tracked text file matches a pattern" "…and says so last"
  # 5. a pattern grep refuses is exit 2, named by number
  printf 'ok\n(unclosed\n' > "$t/deny.txt"
  rc=0; out=$(PII_DENY_FILE="$t/deny.txt" PII_ALLOW_FILE="$t/none" PII_SELFTEST=0 bash "$0" "$t/repo" 2>&1) || rc=$?
  ok_ "$rc" 2 "an invalid pattern is exit 2"
  ok_ "$(printf '%s\n' "$out" | grep -c 'pattern #2 (deny.txt) is not a valid')" 1 "…named by its line number"
  rm -rf "$t"
  if [ "$bad" = 0 ]; then echo "[pii selftest] PASS ($n checks: a hit per half fails by path, line and pattern number with the text never printed; the public half case-sensitive, the private half not; go.sum and binaries unread; the private half off in one line; the allow list per file and pattern; a clean tree passes; an invalid pattern is exit 2)"; return 0; fi
  echo "[pii selftest] FAILED ($bad of $n)"; return 1
}

if [ "${PII_SELFTEST:-0}" = 1 ]; then pii_selftest_; exit $?; fi
pii_repo=${1:-.}; [ "$#" -gt 1 ] && { shift; PII_SPECS=("$@"); }
pii_scan_ "$pii_repo"
