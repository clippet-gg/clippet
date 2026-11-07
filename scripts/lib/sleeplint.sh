#!/bin/sh
# sleeplint.sh — rule R1 of the test tiers (§20 erratum 331): no
# `time.Sleep(` in the tests of the five packages whose flakes were timing flakes — internal/worker,
# internal/httpapi, internal/store, internal/notify, internal/sweeper — unless the SAME line carries a
# `// sleep: <reason>` comment saying why no signal can replace it (the settle hook, the fake clock, a
# channel the code already closes). Every sleep left in those packages carries one, so a line this
# script prints is a NEW sleep.
#
# Usage: sh scripts/lib/sleeplint.sh [repo-root]   (default: the directory two levels above this file)
# Exit 0 = clean ("sleeplint: OK …"), 1 = at least one bare sleep (each printed as file:line: text).
# POSIX sh + grep only, so it runs on the host (Git Bash) and in the busybox Go image alike. Lane
# `tiers` wires it into check.sh and t0.sh.
set -u
ROOT=${1:-$(cd "$(dirname "$0")/../.." && pwd)}
PKGS="internal/worker internal/httpapi internal/store internal/notify internal/sweeper"
bad=0
n=0
for p in $PKGS; do
  for f in "$ROOT/$p"/*_test.go; do
    [ -f "$f" ] || continue
    # every line that calls time.Sleep( — a comment that merely names it is not a call
    hits=$(grep -n 'time\.Sleep(' "$f" | grep -v '^[0-9]*:[[:space:]]*//' || true)
    [ -n "$hits" ] || continue
    while IFS= read -r line; do
      n=$((n + 1))
      case $line in
        *'// sleep: '?*) ;;
        *) bad=$((bad + 1)); echo "sleeplint: ${f#"$ROOT"/}:$line" ;;
      esac
    done <<EOF
$hits
EOF
  done
done
if [ "$bad" -gt 0 ]; then
  echo "sleeplint: FAIL — $bad time.Sleep( call(s) without a '// sleep: <reason>' comment on the same line"
  echo "sleeplint: wait on a signal instead (worker: h.until / awaitJob on the settle hook; transcode: the fake clock),"
  echo "sleeplint: or say on that line why no signal exists."
  exit 1
fi
echo "sleeplint: OK — $n time.Sleep( call(s) in $PKGS, every one with a reason"
