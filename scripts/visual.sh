#!/usr/bin/env bash
# visual.sh — the Playwright visual pass over a LIVE stack (web/STYLE.md; DESIGN.md §20 erratum 240).
#
# Screenshots every page of the app — login, register, the library empty / with clips / with one
# ticked, the owner's and a visitor's watch page, a collection, settings, the 404 page and the
# embed — at 1280 and 390 wide in BOTH themes, into .e2e/visual/ (gitignored) with an index.html
# contact sheet, and asserts on every page: zero console errors, zero CSP violations, zero requests
# to any origin but the stack's, no horizontal overflow at 390, and the faces loaded from
# /static/fonts/. There are no committed baselines yet: the sheet is for a person to look at.
#
# It reads BOTH media-delivery shapes (§20 erratum 282). On a stack in `MEDIA_DELIVERY=redirect`
# (§20 erratum 278, DEPLOY §33) the watch page's and the embed's own media come from the bucket's
# origin behind the app's 302, which is a legitimate second origin and is named by the app's own
# CSP: visual.js reads the mode off that policy before the first shot, prints it, confirms it
# against what the media routes serve once a clip is ready, and allows exactly the signed media
# requests to that one origin — nothing else moves.
#
# It reads the INSTANCE BADGE the same way (§20 erratum 285, DEPLOY §7.1): `data-badge` on the stack's
# own <html>, asked once before the first shot and printed. A stack wearing `INSTANCE_BADGE=dev` gets
# `domBadge` (the attribute, the tag's metrics and position, the span's and a mouth's computed colour
# against --line-strong, the embed's grey span and absent tag) and two extra frames, `badge-header` and
# `badge-front`. An unbadged stack — production, and the reference stack — skips both, so the sheet
# stays the 111 shots it takes since the setup wizard (ROADMAP #45) added its four, §20 erratum 311 its STOP frame,
# erratum 313 the upload card after a pick and erratum 329 the audio clip's eight.
#
# It runs a Playwright container — no Node on the host — against the stack this checkout's .env
# names (or BASE=...), the way scripts/e2e.sh does; the browser inside the container reaches the
# host's loopback as host.docker.internal. That container is given TWO directories and no more —
# `scripts/` and `testclips/`, both read-only (AUDIT #25) — never the repository root, which is where
# `.env` lives; `visual.js` refuses to start if it can see one.
# The image is built here from Dockerfile.visual (the
# official Playwright base plus the matching npm package, built with an EMPTY context so nothing
# from this repository goes into it) and is cached by the daemon under a tag that carries the
# version: the npm fetch that used to happen on the first run, into a named volume, is a build step
# now and is the only request this pass makes to anything but the stack under test — and, on a
# redirect stack, that stack's own bucket.
#
# STANDALONE, deliberately: it needs a running stack and a 1.5 GB browser image, neither of which
# scripts/check.sh's container-only CI story has. Run it after the E2E, before a gate:
#     bash scripts/visual.sh            # → .e2e/visual/index.html
#     BASE=http://localhost:8491 bash scripts/visual.sh
#     VISUAL_WORKERS=1 bash scripts/visual.sh   # one shot at a time, as every pass before lane `speed2`
#     VISUAL_PROVE=1 bash scripts/visual.sh     # + every phase re-taken one shot at a time, compared by sha256
# Since lane `speed2` the shots are taken VISUAL_WORKERS at a time (the default comes from the
# container's CPU count and is printed at the top of the pass), in phases that never cross a point where
# the serial pass changes the stack; scripts/lib/visual.js's "the workers" block says which, why, and how
# the identity with a one-at-a-time pass is proved.
# It registers a throwaway account (visual_<epoch>) and deletes the clip and collection it made,
# then removes any collection an INTERRUPTED pass left behind (`admin collections rm`, §20 erratum
# 275 — the row only, never a clip) and prunes `visual_%` from the stack's users table (best effort)
# so the accounts do not pile up — and runs both BEFORE it registers, so an aborted pass is cleared
# by the next one rather than by a person typing a delete by hand (§20 erratum 272).
set -euo pipefail; cd "$(dirname "$0")/.."
OUT="$PWD/.e2e/visual"
# The stack's dotenv: `.env`, or the file COMPOSE_ENV_FILES names (the gate's own stack, scripts/lib/gatestack.sh).
VISUAL_ENVF=${COMPOSE_ENV_FILES:-.env}; VISUAL_ENVF=${VISUAL_ENVF##*,}
envfile() { { [ -f "$VISUAL_ENVF" ] && sed -n "s/^$1=//p" "$VISUAL_ENVF" | tail -1 | tr -d '\r"'; } || true; }
# ENV_BASE is the origin THIS checkout's compose project serves, resolved as scripts/e2e.sh resolves it
# (the shell, then .env, then compose's default); BASE is the origin being photographed. The tidy-up
# below (`users prune`, `collections rm`) runs through `docker compose exec` in THIS directory, so it
# only ever reaches the local project — and when BASE names some other origin that project is not the
# stack the pass registered on, and pruning it would be a delete on a stack this pass never touched
# (§20 erratum 290, AUDIT #24). So the tidy-up runs only when the two agree, and says so when they do not.
ENV_BASE=${PUBLIC_BASE_URL:-$(envfile PUBLIC_BASE_URL)}; ENV_BASE=${ENV_BASE:-http://localhost:8481}; ENV_BASE=${ENV_BASE%/}
BASE=${BASE:-$ENV_BASE}; BASE=${BASE%/}
if [ "$BASE" = "$ENV_BASE" ]; then VISUAL_TIDY=1; else VISUAL_TIDY=0; fi
[ "$VISUAL_TIDY" = 1 ] || echo "[visual] NOTE: BASE $BASE is not this checkout's own stack ($ENV_BASE): the visual_% tidy-up is skipped at both ends (it would prune the LOCAL project, which is not the stack being photographed)"
# What the Playwright container is given, and it is ONLY these two directories (AUDIT #25). It used to
# be `-v "$PWD":/src:ro` — the whole checkout, including the gitignored `.env` with this stack's bucket
# credentials — into an image that npm-installs Playwright from the network on a cold build. Nothing in
# the pass ever wanted more than its own harness and the fixtures: visual.js is `/src/scripts/lib/` and
# the two clips it uploads are `/src/testclips/`, so both keep their paths because each directory is
# mounted where it already was. Read-only, as before. The list is built HERE, above the dry run, so
# `VISUAL_DRY=1` can print the exact arguments without a daemon, an image or a browser.
W() { if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi; }
VISUAL_MOUNTS="-v $(W "$PWD/scripts"):/src/scripts:ro -v $(W "$PWD/testclips"):/src/testclips:ro"
# VISUAL_DRY=1 prints the decisions above and stops before anything is touched — no sheet removed, no
# image built, no `docker compose exec`. It is what the E2E self-test runs with a foreign BASE.
if [ "${VISUAL_DRY:-0}" = 1 ]; then
  echo "[visual] dry run: stack $BASE (own stack: $ENV_BASE); tidy-up $([ "$VISUAL_TIDY" = 1 ] && echo on || echo OFF); stopping here"
  echo "[visual] dry run: container mounts $VISUAL_MOUNTS (the repository root, and its .env, are NOT mounted — AUDIT #25)"
  exit 0
fi
rm -rf "$OUT"; mkdir -p "$OUT"
PW_VERSION=${PW_VERSION:-1.48.0}          # the ONE pin: Dockerfile.visual builds both halves from it
PW_IMAGE=${PW_IMAGE:-clippet-visual:$PW_VERSION}
# The browser runs in a container: a loopback origin on the host is host.docker.internal there.
# VISUAL_HOST_MAP=1 goes with it (run 9, lane `gatestack`): a loopback stack in `MEDIA_DELIVERY=redirect`
# against its own published S3 server — the gate stack's shape — signs every media link for `localhost:<port>`,
# and the signature covers that host, so the browser must ask for exactly that URL. Chromium in the
# container is told to RESOLVE `localhost` to the host (`--host-resolver-rules`); the URL, and so the Host
# header the signature was made for, is unchanged. Measured: without it a signed link is
# ERR_CONNECTION_REFUSED in the container, with it 200.
VISUAL_HOST_MAP=0
case $BASE in
  http://localhost:*|http://127.0.0.1:*) IN_BASE="http://host.docker.internal:${BASE##*:}"; VISUAL_HOST_MAP=1;;
  http://localhost|http://127.0.0.1) IN_BASE="http://host.docker.internal"; VISUAL_HOST_MAP=1;;
  *) IN_BASE=$BASE;;
esac
echo "[visual] stack $BASE (in the container: $IN_BASE; localhost resolved to the host in the browser: $VISUAL_HOST_MAP); image $PW_IMAGE; out $OUT"
# The throwaway accounts, pruned the way scripts/e2e.sh sweeps its own (§20 errata 86 and 272; DEPLOY
# §8), at BOTH ends of the pass. visual.js deletes the clip and the collection it made in a `finally`,
# so `visual_<epoch>` owns nothing by the end and `--no-clips` — which skips any match that still owns
# something, whatever the pattern says — is satisfied. `--older-than 1s` is enough to take THIS pass's
# account too: it was registered minutes ago, and unlike the E2E there is no second user to outlive.
# Running it at the START as well is what clears a pass that was INTERRUPTED before its `finally`: the
# next pass tidies up, rather than a person typing a delete by hand, which is the one habit that has
# already cost this project a stack. `_` is a LIKE wildcard, so the pattern is `visual`
# plus one character plus anything — and `--no-clips` bounds what that can ever mean to an account
# owning nothing at all. It CANNOT be narrowed from here: `users prune` takes the pattern straight
# into `username LIKE ?` with no ESCAPE clause (internal/db's pruneWhere), so `visual\_%` would match
# a literal backslash rather than an underscore and quietly stop matching anything. The exact risk,
# then, is an account named `visualX…` that owns NOTHING being removed by a run of this pass; adding
# an escape to the CLI is the only fix and is not this pass's to make (§20 erratum 275).
# An interrupted pass whose clip SURVIVED is therefore left exactly where it is,
# and shows up in `clippet admin users` for a person to deal with: this pass removes empty accounts, it
# never removes a clip. The count line says how many went.
# Best effort, always: a pass that has produced its sheet is never failed by the tidying, and on a run
# against somebody else's origin (BASE=…) the local compose project may not even be the stack that was
# photographed — which is exactly when this says so and moves on.
prune_visual_() {
  local out
  if out=$(docker compose exec -T app clippet admin users prune 'visual_%' --older-than 1s --no-clips 2>&1); then
    printf '%s\n' "$out" | tail -1 | sed -n "s/^/[visual] $1: /p"
  else
    echo "[visual] NOTE: could not prune the visual_% accounts $1 (not fatal): $(printf '%s' "$out" | tail -1)"
  fi
}
# …and the ONE thing that prune cannot reach on its own: a collection (§20 erratum 275). visual.js
# makes one and deletes it in its `finally`, so this only ever finds something after a pass that was
# INTERRUPTED — and until `admin collections rm` existed, that pass's account was stuck in
# `clippet admin users` for ever, because `--no-clips` skips an account that still owns one.
# The guard shape is scripts/e2e.sh's e2e_sweep_own, in miniature and best effort throughout: one
# anchored selector over the names this pass itself mints, the target list printed before anything is
# removed, every id checked against the two id grammars AND against the listing's own count line, and
# `--owner` passed to `rm` as a second selector. `rm` removes the collection ROW and never a clip, so
# the "this pass removes empty accounts, never a clip" promise above is unchanged.
VISUAL_OWN_RE='^visual_[0-9]{9,14}$'   # visual_<epoch-ms>, the name visual.js registers, and nothing else
VISUAL_ID_RE='^([A-Za-z0-9]{12}|([A-Z][a-z]{2,8}){3,4})$'
rm_visual_collections_() {
  local why=$1 inv names name out rows want got pairs pair cid cname n=0
  if ! inv=$(docker compose exec -T app clippet admin users 2>&1); then
    echo "[visual] NOTE: could not read the user list $why (not fatal): $(printf '%s' "$inv" | tail -1)"
    return 0
  fi
  names=$(printf '%s\n' "$inv" | tr -d '\r' | awk '{ print $2 }' | grep -E "$VISUAL_OWN_RE" || true)
  pairs=
  for name in $names; do
    [ -n "$name" ] || continue
    if ! out=$(docker compose exec -T app clippet admin collections ls --owner "$name" 2>&1); then
      echo "[visual] NOTE: 'admin collections ls --owner $name' failed (not fatal): $(printf '%s' "$out" | tail -1)"
      return 0
    fi
    out=$(printf '%s\n' "$out" | tr -d '\r')
    rows=$(printf '%s\n' "$out" | awk '$4 ~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T/ { print $1 }')
    want=$(printf '%s\n' "$out" | tail -1 | sed -n 's/^no collections.*/0/p; s/^\([0-9][0-9]*\) collections.*/\1/p')
    got=$(printf '%s' "$rows" | grep -c . || true)
    if [ -z "$want" ] || [ "$want" != "$got" ]; then
      echo "[visual] NOTE: 'admin collections ls --owner $name' counts '${want:-?}' and parsed $got row(s) — nothing removed"
      return 0
    fi
    for cid in $rows; do
      [ -n "$cid" ] || continue
      if ! printf '%s' "$cid" | grep -Eq "$VISUAL_ID_RE"; then
        echo "[visual] NOTE: 'admin collections ls --owner $name' printed '$cid' where an id belongs — nothing removed"
        return 0
      fi
      pairs="$pairs $name:$cid"; n=$((n + 1))
    done
  done
  [ "$n" != 0 ] || return 0
  echo "[visual] $why: $n collection(s) left by an interrupted pass, owner:id —$pairs"
  for pair in $pairs; do
    cname=${pair%%:*}; cid=${pair#*:}
    [ -n "$cname" ] && [ -n "$cid" ] || continue     # a loop body NEVER runs on an empty selector
    docker compose exec -T app clippet admin collections rm "$cid" --owner "$cname" >/dev/null 2>&1 \
      || echo "[visual] NOTE: could not remove collection $cid of $cname (not fatal)"
  done
}
if [ "$VISUAL_TIDY" = 1 ]; then rm_visual_collections_ "before the pass"; prune_visual_ "before the pass"; fi
echo "[visual] fixtures"; bash scripts/testclips.sh >/dev/null
OUTW=$(W "$OUT")
export MSYS_NO_PATHCONV=1
# `- < Dockerfile.visual` is an EMPTY build context: the image needs nothing from this checkout (the
# harness is bind-mounted below), so nothing is uploaded to the daemon and no .dockerignore applies.
# On every run after the first this is a cache hit and costs about a second.
echo "[visual] image"
docker build -q --build-arg "PW_VERSION=$PW_VERSION" -t "$PW_IMAGE" - < Dockerfile.visual >/dev/null \
  || { echo "[visual] could not build $PW_IMAGE from Dockerfile.visual (the base image and the npm package both need the network on a cold build)"; exit 1; }
# On Linux (the harness on WSL, erratum 338) the container — root — hands /out back to this user when the pass ends,
# whatever it said: a root-owned DIRECTORY in a bind mount (VISUAL_PROVE's serial/) cannot be removed by the user,
# and the next run's `rm -rf "$OUT"` failed on it. On Git Bash NTFS ignores ownership and nothing is chowned.
VISUAL_CHOWN=; case $(uname -s) in Linux) VISUAL_CHOWN="$(id -u):$(id -g)" ;; esac
# shellcheck disable=SC2086 # VISUAL_MOUNTS is a deliberate argument list, built above.
docker run --rm -t \
  $VISUAL_MOUNTS -v "$OUTW":/out \
  -e "BASE=$IN_BASE" -e "PUBLIC_BASE=$BASE" ${VISUAL_WORKERS:+-e "VISUAL_WORKERS=$VISUAL_WORKERS"} ${VISUAL_PROVE:+-e "VISUAL_PROVE=$VISUAL_PROVE"} ${VISUAL_ONLY:+-e "VISUAL_ONLY=$VISUAL_ONLY"} -e "VISUAL_HOST_MAP=$VISUAL_HOST_MAP" -e "VISUAL_CHOWN=$VISUAL_CHOWN" -w /pw \
  "$PW_IMAGE" sh -c 'rc=0; node /src/scripts/lib/visual.js || rc=$?; [ -z "$VISUAL_CHOWN" ] || chown -R "$VISUAL_CHOWN" /out; exit $rc'
echo "[visual] contact sheet: $OUT/index.html"
if [ "$VISUAL_TIDY" = 1 ]; then rm_visual_collections_ "after the pass"; prune_visual_ "after the pass"; fi
