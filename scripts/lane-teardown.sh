#!/usr/bin/env bash
# lane-teardown.sh [--check] <lane>… — remove MERGED lane worktrees and everything they own: the compose
# project's containers and network, its volumes, its image, the branch. Part of every merge since 2026-09-15
# (the owner's rule: whoever merges a lane cleans up its branch and worktree), so the maintainer runs it right
# after `git merge --no-ff <lane>` with no question asked.
#
# The checks ARE the guard — every one must pass for every lane named, or nothing at all is removed:
#   - the script is being run FROM the reference repo and not from inside a lane worktree (a copy run
#     from a worktree resolves every path against THAT tree and passes every check below vacuously);
#   - the name is never `main`, `master` or `dev` (the dev instance's worktree is NOT a lane: it is
#     fast-forwarded to main, so "merged" is always true of it — §20 erratum 290, AUDIT #2), and never
#     the reference project;
#   - the lane's branch is an ancestor of main (merged);
#   - its worktree directory is `<repo>-<lane>` beside this repo and its tree is clean;
#   - that worktree's `.env` carries the exact line `COMPOSE_PROJECT_NAME=clippet-<lane>` — because
#     compose resolves the project from the directory it runs in, and a worktree with no `.env` resolves
#     to compose.yaml's own `name:`, which is the REFERENCE project (AUDIT A1: `down` there would have
#     taken clippet.gg offline). The `down` below names the project with `-p` as well, so the file is
#     never what decides which stack goes;
#   - no container of its compose project `clippet-<lane>` is running;
#   - the only volumes ever touched are `clippet-<lane>_*`, the only image `clippet-<lane>-app:local`,
#     matched by exact prefix.
# The reference stack, its volumes and the tunnel are never named here. Branches are deleted with
# `git branch -d`, which refuses an unmerged one on its own.
#
# `--check` runs the check loop alone and removes nothing: it prints OK or REFUSED per lane and exits 0
# or 1. It is what the E2E self-test calls (with `dev`, and with a scratch worktree that has no `.env`).
#
# THE WSL WORKTREE (DESIGN §20 erratum 338). A lane's T0/T1 typed in Git Bash run in a WSL worktree of the
# ext4 clone, $HOME/dev/<repo>-<lane> inside WSL (scripts/lib/wslexec.sh). When wsl.exe and that clone are present, each lane's
# checks also ask WSL (`wslexec.sh wsl-teardown --check`): its WSL worktree, when there, must be a worktree of the
# clone and no harness run may hold it. The removal then takes it too — a root-owned .e2e/ (a container's writes)
# through busybox first, then `git worktree remove --force` and prune in the clone — after the Windows worktree.
# CLIPPET_WSL=0 skips the WSL half.
#
# The auto-mode classifier refuses `git worktree remove` and `docker volume rm` typed by an agent, so
# this script exists to be allowed ONCE as a whole: a permission rule for `bash scripts/lane-teardown.sh`.
set -euo pipefail
CHECK=0
if [ "${1:-}" = --check ]; then CHECK=1; shift; fi
[ $# -ge 1 ] || { echo "usage: bash scripts/lane-teardown.sh [--check] <lane>…"; exit 2; }
cd "$(dirname "$0")/.."
REPO=$(pwd -W 2>/dev/null || pwd); REPO=${REPO%/}
# RUN ME FROM THE REFERENCE REPO (§20 erratum 290, amended). `$0` decides which checkout this is, so
# a copy of this script run from INSIDE a lane worktree resolved REPO to that worktree and then looked
# for `<worktree>-<lane>`: a directory that does not exist, reported as "already gone", with the
# branch trivially an ancestor of main before its first commit — so `--check <its own lane>` said OK
# and a real run would have removed the branch out from under the tree it was standing in. Git knows
# which worktree is the main one: --git-common-dir is the shared .git for every worktree, and its
# parent is the main checkout. The maintainer runs this from the reference repo; this makes the
# script say so instead of trusting it.
MAIN_WT=$(git rev-parse --git-common-dir 2>/dev/null) || { echo "REFUSED: $REPO is not a git checkout"; exit 1; }
MAIN_WT=$(cd "$MAIN_WT/.." 2>/dev/null && { pwd -W 2>/dev/null || pwd; }) || { echo "REFUSED: could not resolve the main worktree from $REPO"; exit 1; }
MAIN_WT=${MAIN_WT%/}
[ "$MAIN_WT" = "$REPO" ] || { echo "REFUSED: run this from the reference repo ($MAIN_WT), not from the worktree $REPO — from a lane worktree it would look for '$REPO-<lane>' and report every lane as already gone"; exit 1; }
REF_PROJECT=$(sed -n 's/^name:[[:space:]]*//p' compose.yaml | head -1 | tr -d '\r')
# The WSL half (erratum 338): only from Git Bash, with wsl.exe and WSL's clone present — and only when this repo has
# scripts/lib/wslexec.sh (the E2E self-test runs a copy of this script alone in a scratch repo, which must stay local).
WSL_TD=0
if [ -f scripts/lib/wslexec.sh ]; then
  . scripts/lib/wslexec.sh
  if [ "$(wsl_decide_ "${MSYSTEM:-}" "${CLIPPET_WSL:-auto}" "${CLIPPET_IN_WSL:-}" "$(command -v wsl.exe >/dev/null 2>&1 && echo 1 || echo 0)" 1)" = wsl ] \
     && wsl_exe_ test -d "$WSL_CLONE_REL/.git" >/dev/null 2>&1; then WSL_TD=1; fi
  WSL_SELF=$(wsl_mnt_ "$REPO")/scripts/lib/wslexec.sh
  WSL_PFX=$(basename "$REPO")
fi
[ -n "$REF_PROJECT" ] || REF_PROJECT=$(basename "$REPO")
ok=1
for l in "$@"; do
  case $l in ''|main|master|dev|*/*|*' '*|"$REF_PROJECT") echo "REFUSED: '$l' is not a lane name"; ok=0; continue ;; esac
  proj="clippet-$l"
  [ "$proj" != "$REF_PROJECT" ] || { echo "REFUSED: $proj is the reference project"; ok=0; continue; }
  git show-ref --verify --quiet "refs/heads/$l" || { echo "no branch: $l"; ok=0; continue; }
  git merge-base --is-ancestor "$l" main || { echo "NOT MERGED into main: $l"; ok=0; continue; }
  wt="$REPO-$l"
  if [ -d "$wt" ]; then
    [ -z "$(git -C "$wt" status --porcelain 2>/dev/null)" ] || { echo "DIRTY worktree: $wt"; ok=0; continue; }
    # The exact line, and read through `tr`/`grep -x` only — the file's other lines are the lane's
    # credentials and are never printed.
    if ! { [ -f "$wt/.env" ] && tr -d '\r' < "$wt/.env" | grep -qx "COMPOSE_PROJECT_NAME=$proj"; }; then
      echo "REFUSED: $wt/.env does not carry the line COMPOSE_PROJECT_NAME=$proj (compose would resolve the project from compose.yaml's own name)"; ok=0; continue
    fi
  fi
  n=$(docker ps --format '{{.Names}}' | grep -c "^$proj-" || true)
  [ "$n" = 0 ] || { echo "RUNNING containers for $proj: $n (stop them first: docker compose stop in the worktree)"; ok=0; continue; }
  if [ "$WSL_TD" = 1 ]; then
    wo=$(wsl_exe_ bash "$WSL_SELF" wsl-teardown --check "$WSL_PFX-$l" 2>&1) || { echo "$wo"; ok=0; continue; }
    [ "$CHECK" = 0 ] || echo "$wo"
  fi
  [ "$CHECK" = 0 ] || echo "OK: $l (branch merged, worktree ${wt} $([ -d "$wt" ] && echo 'clean with its .env' || echo 'already gone'), project $proj idle)"
done
[ "$ok" = 1 ] || { echo "checks failed — nothing removed"; exit 1; }
[ "$CHECK" = 0 ] || { echo "--check: every check passed for: $* (nothing removed)"; exit 0; }
echo "=== lane teardown: $* ==="
for l in "$@"; do
  proj="clippet-$l"; wt="$REPO-$l"
  echo "--- $l"
  if [ -d "$wt" ]; then
    # `-p "$proj"`: the project is NAMED, never resolved from the directory (AUDIT A1).
    (cd "$wt" && docker compose -p "$proj" down --remove-orphans 2>&1 | tail -1) || true
    git worktree remove --force "$wt" && echo "worktree removed: $wt"
  else
    echo "no worktree dir (already gone): $wt"
  fi
  [ "$WSL_TD" = 0 ] || wsl_exe_ bash "$WSL_SELF" wsl-teardown "$WSL_PFX-$l" || echo "NOTE: the WSL worktree ~/$(dirname "$WSL_CLONE_REL")/$WSL_PFX-$l was not removed (above)"
  # `|| true` on each grep: a lane whose builder already ran `down -v` has no volume (or no image) to
  # match, and under pipefail the empty match ended the script here — before its branch was deleted and
  # before any later lane on the command line (run 6: hook, backup, form).
  docker volume ls --format '{{.Name}}' | { grep -E "^${proj}_" || true; } | while read -r v; do docker volume rm "$v" >/dev/null && echo "volume removed: $v"; done
  docker images --format '{{.Repository}}:{{.Tag}}' | { grep -E "^${proj}-app:local$" || true; } | while read -r i; do docker rmi "$i" >/dev/null && echo "image removed: $i"; done
  git branch -d "$l" && echo "branch deleted: $l"
done
git worktree prune
echo "=== after ==="; git worktree list
# THE BOX AS A WHOLE (lane boxtidy, 2026-10-18): the READ-ONLY report of what no single teardown owns — its
# closing line is the last but one; when it names litter the maintainer runs `bash scripts/box-tidy.sh apply` right
# then. The branch list stays the last line. A scratch copy of this script (the E2E self-test) has none.
if [ -f scripts/box-tidy.sh ]; then sh scripts/box-tidy.sh report || true; else echo "box-tidy: not here"; fi
git branch --list | tr -d ' ' | tr '\n' ' '; echo
