#!/usr/bin/env bash
# gatestack.sh — the gate's OWN local stack (run 9, lane `gatestack`; toil/proposal.md §2 F6/F7/F8, §3 T2).
#
# Until this lane every gate ran the E2E and the visual pass against https://clippet.gg: each attempt
# rebuilt and recreated the PRODUCTION app four times with test settings, and every edge hiccup was a
# failed gate. The gate now runs on a compose project of its own, built from the checkout it runs in,
# on loopback, with its own volumes and its own S3 server, in `MEDIA_DELIVERY=redirect` against that server
# (compose.e2e-bucket.yaml, the shape step 90 already proves; since run 22 the server is RustFS, pinned by
# digest in that overlay because MinIO's images are gone, and since §20 erratum 346 it has no client image:
# the app's own `admin objects init` makes the bucket, run by `up` below and by the E2E's step 1).
# Nothing here reads, writes or restarts the reference stack; the one thing it reads of it is the NAMES of the keys in its `.env` (below).
#
#     bash scripts/lib/gatestack.sh env        # write the gate stack's .env (its path is printed, never its values)
#     bash scripts/lib/gatestack.sh keys       # the key-diff self-test against the reference .env (names only)
#     bash scripts/lib/gatestack.sh up         # env + keys + docker compose up -d --build, wait for /readyz
#     bash scripts/lib/gatestack.sh run CMD…   # env + keys, then CMD with the gate stack's environment
#     bash scripts/lib/gatestack.sh once CMD…  # run, then down (whatever CMD said; GATE_KEEP=1 keeps the stack): a lane's one-shot proof
#     bash scripts/lib/gatestack.sh down       # docker compose down -v, its image tags, its .env: exactly what it made
#     bash scripts/lib/gatestack.sh visual-decision   # RUN … | SKIPPED (no change under web/ since <commit>)
#     bash scripts/lib/gatestack.sh gate-key   # tree= env= docker= nvidia= images=: the input key a green gate records (G1)
#     bash scripts/lib/gatestack.sh flavour    # gpu | cpu: the image this gate stack proves (GATE_GPU), recorded as flavour=
#     bash scripts/lib/gatestack.sh pins       # every pinned image the stack pulls is here or pullable, before anything is built
#     bash scripts/lib/gatestack.sh selftest   # the pins, against scratch directories (scripts/check.sh runs it)
#
# Knobs: GATE_PROJECT (default clippet-gate), GATE_PORT (8576), GATE_MINIO_PORT (GATE_PORT + 1000: the S3 server's host port),
# GATE_GPU (1: the gpu flavour, as production runs — the default but on a Mac; 0: the CPU image), GATE_APP_CPUS (4, capped at the
# host's cores), GATE_REF_ENV (default: the MAIN worktree's .env — the reference stack's; with none there, .env.example, and
# then the key-diff is informational: PORTABLE MODE, for a fresh clone or a CI runner).
#
# HOW the stack is selected without touching the checkout's own `.env` (which in the main checkout IS the
# reference stack's): compose reads `COMPOSE_ENV_FILES` INSTEAD of `.env` (measured: a `.env` beside it is
# ignored entirely, and its COMPOSE_PROJECT_NAME and COMPOSE_FILE lines are honoured), so `run` exports
#   COMPOSE_ENV_FILES=.e2e/gatestack/<project>.env   COMPOSE_PROJECT_NAME=<project>   BASE=http://localhost:<port>
# and every `docker compose` the harness makes — the E2E's dc, the visual pass's tidy-up — lands on the
# gate stack. The three harness readers of `.env` (e2e.sh, visual.sh, gate.sh `envfile`) read the same
# file through COMPOSE_ENV_FILES.
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 2

GATE_PROJECT=${GATE_PROJECT:-clippet-gate}
GATE_PORT=${GATE_PORT:-8576}
# A Mac has no NVIDIA runtime under Docker Desktop: the CPU image there by default (GATE_GPU=1 still forces the gpu flavour).
# So a gate typed on the Mac proves the CPU image ONLY — not compose.gpu.yaml, TRANSCODE_HWACCEL=cuda, the NVDEC-refusal
# classifier (errata 265/289) or the audio bundles' GPU-image path. The green record says so as flavour=cpu (and nvidia=none);
# the gpu proof stays with a gate on the NVIDIA box (the PC's nightly) while clippet.gg runs the gpu flavour (run 21 review).
case $(uname -s) in Darwin) GATE_GPU=${GATE_GPU:-0} ;; *) GATE_GPU=${GATE_GPU:-1} ;; esac
GATE_APP_CPUS=${GATE_APP_CPUS:-4}
gs_fail_() { echo "[gatestack] $*" >&2; exit 2; }
case $GATE_PROJECT in
  clippet|clippet-dev) gs_fail_ "GATE_PROJECT=$GATE_PROJECT is the reference or the dev stack: the gate never runs on either" ;;
  clippet-[a-z0-9]*) ;;
  *) gs_fail_ "GATE_PROJECT must be clippet-<name> (lowercase letters, digits, hyphens), not '$GATE_PROJECT'" ;;
esac
printf '%s' "$GATE_PROJECT" | grep -Eq '^clippet-[a-z0-9-]+$' || gs_fail_ "GATE_PROJECT must be clippet-<name>, not '$GATE_PROJECT'"
case $GATE_PORT in ''|*[!0-9]*) gs_fail_ "GATE_PORT must be a number, not '$GATE_PORT'" ;; esac
case $GATE_PORT in 8480|8481) gs_fail_ "GATE_PORT=$GATE_PORT is the dev or the reference stack's port" ;; esac
GATE_MINIO_PORT=${GATE_MINIO_PORT:-$((GATE_PORT + 1000))}
case $GATE_MINIO_PORT in ''|*[!0-9]*) gs_fail_ "GATE_MINIO_PORT must be a number, not '$GATE_MINIO_PORT'" ;; esac
GS_NCPU=$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
case $GS_NCPU in ''|*[!0-9]*|0) GS_NCPU=4 ;; esac
case $GATE_APP_CPUS in ''|*[!0-9]*|0) GATE_APP_CPUS=4 ;; esac
[ "$GATE_APP_CPUS" -le "$GS_NCPU" ] || GATE_APP_CPUS=$GS_NCPU   # a cpus above the core count is a CREATE error (DEPLOY §7)
GS_DIR=.e2e/gatestack
GS_ENVF=$GS_DIR/$GATE_PROJECT.env          # relative: compose resolves it against this directory, and no MSYS path ever reaches it
GS_BASE=http://localhost:$GATE_PORT
GS_IMAGE=$GATE_PROJECT-app:local
GS_GPU_IMAGE=$GATE_PROJECT-app:gpu
GS_GREEN=.e2e/checkpoints/gate-green.txt     # the last green gate's commit: what "visual only when web/ moves" diffs against

# ---- the generated .env ---------------------------------------------------------------------------------
# The gate's values, one per key. EVERY key the reference stack's .env sets must be here (the key-diff below
# fails on a missing one, by NAME), so a key production gains is a key the gate is made to decide about.
# Secrets production holds are set EMPTY here: the gate stack has no tunnel, no bot and no bucket of its
# own beyond its S3 server, and an empty S3 key falls back to that server's dev credentials (the `:-` in
# compose.e2e-bucket.yaml, which spells the app's four S3_* itself so the stack does not depend on what
# compose.yaml's default storage is).
# CLIPPET_VERSION is a gate value (`gate`, or the caller's) because the release cycle's preflight requires the
# reference .env to carry CLIPPET_VERSION=<version> (clippet-pass scripts/release-cycle.sh step 0), and a key the
# reference sets must exist here or the key-diff refuses the gate: the version string itself proves nothing about
# the gate's shape. GATE_RELEASE=1 runs pass the real one through.
gs_values_() {
  local files=compose.yaml hw=
  if [ "$GATE_GPU" = 1 ]; then files=compose.yaml:compose.gpu.yaml; hw=cuda; fi
  cat <<EOF
COMPOSE_PROJECT_NAME=$GATE_PROJECT
CLIPPET_IMAGE=$GS_IMAGE
CLIPPET_GPU_IMAGE=$GS_GPU_IMAGE
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=$files:compose.e2e-bucket.yaml
CLIPPET_PUBLISH=127.0.0.1:$GATE_PORT
PUBLIC_BASE_URL=$GS_BASE
TRUSTED_PROXIES=
REGISTRATION=open
APP_CPUS=$GATE_APP_CPUS
CLIPPET_VERSION=${CLIPPET_VERSION:-gate}
TRANSCODE_HWACCEL=$hw
CLOUDFLARE_TUNNEL_TOKEN=
DISCORD_APP_ID=
DISCORD_PUBLIC_KEY=
DISCORD_BOT_TOKEN=
SESSION_COOKIE_NAME=
S3_ENDPOINT=http://s3:9000
S3_REGION=us-east-1
S3_BUCKET=clips
S3_ACCESS_KEY=
S3_SECRET_KEY=
S3_FORCE_PATH_STYLE=true
MEDIA_DELIVERY=redirect
MEDIA_PRESIGN_ENDPOINT=http://localhost:$GATE_MINIO_PORT
MINIO_PUBLISH=127.0.0.1:$GATE_MINIO_PORT
EOF
}
# Keys the gate sets that production does not, and why each is allowed: the lane-stack keys (HANDOFF §5:
# the project, the two image tags so nothing re-tags the reference's images), the published S3 server and the
# origin a viewer reaches it at (production's bucket is R2, one public host), and every ACTIVE line of
# .env.example (the documented base the file is built from). Anything else extra is drift and fails.
GS_GATE_ONLY="COMPOSE_PROJECT_NAME CLIPPET_IMAGE CLIPPET_GPU_IMAGE MINIO_PUBLISH MEDIA_PRESIGN_ENDPOINT"
# gs_keys_of_ <file> — the key NAMES a dotenv file sets, one per line, sorted; never a value.
gs_keys_of_() { tr -d '\r' < "$1" | sed -n 's/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}\([A-Za-z_][A-Za-z0-9_]*\)=.*/\2/p' | sort -u; }
gs_write_() {   # gs_write_ <out file>
  local out=$1 tmp keys
  mkdir -p "$(dirname "$out")"
  tmp=$out.tmp
  keys=$(gs_values_ | sed 's/=.*//')
  {
    echo "# GENERATED by scripts/lib/gatestack.sh for compose project $GATE_PROJECT — the gate stack's own .env."
    echo "# Never the reference stack's. Rewritten on every gate; removed by 'gatestack.sh down'."
    echo "# --- the active lines of .env.example, minus every key the gate sets below"
    if [ -f .env.example ]; then
      tr -d '\r' < .env.example | grep -E '^[A-Za-z_][A-Za-z0-9_]*=' | while IFS= read -r line; do
        printf '%s\n' "$keys" | grep -qx "${line%%=*}" || printf '%s\n' "$line"
      done
    fi
    echo "# --- the gate's values"
    gs_values_
  } > "$tmp" && mv -f "$tmp" "$out"
}
# gs_ref_env_ — the reference .env: GATE_REF_ENV when set; else the MAIN worktree's .env (the reference stack's);
# else, when that file does not exist (a fresh clone, a CI runner: PORTABLE MODE, run 25 lane `pub-ci`), this
# checkout's .env.example — the documented base the gate's own .env is built from.
gs_ref_env_() {
  if [ -n "${GATE_REF_ENV:-}" ]; then printf '%s' "$GATE_REF_ENV"; return; fi
  local main
  main=$(git worktree list --porcelain 2>/dev/null | sed -n '1s/^worktree //p' | tr -d '\r')
  if [ ! -f "${main:-.}/.env" ] && [ -f .env.example ]; then printf '.env.example'; return; fi
  printf '%s/.env' "${main:-.}"
}
# gs_portable_ — 0 when the reference is an .env.example (by default with no .env, or GATE_REF_ENV=.env.example as
# the nightly CI job sets it): there is no reference STACK to match, so the key-diff is printed and never refuses.
gs_portable_() { case $(gs_ref_env_) in .env.example|*/.env.example) return 0 ;; esac; return 1; }
# gs_keydiff_ <reference .env> <gate .env> — 0 when the gate sets every key the reference sets and nothing
# else but GS_GATE_ONLY and .env.example's active keys; otherwise 1, naming each key. NAMES ONLY: the
# reference file is read through gs_keys_of_, whose sed keeps nothing right of the `=`.
gs_keydiff_() {
  local ref=$1 gate=$2 rk gk allow missing extra
  [ -f "$ref" ] || { echo "[gatestack] keys: the reference .env ($ref) is not there — nothing to compare the gate stack's shape against"; return 1; }
  [ -f "$gate" ] || { echo "[gatestack] keys: the gate's .env ($gate) is not there"; return 1; }
  rk=$(gs_keys_of_ "$ref"); gk=$(gs_keys_of_ "$gate")
  allow=$( { printf '%s\n' $GS_GATE_ONLY; [ ! -f .env.example ] || gs_keys_of_ .env.example; } | sort -u)
  missing=$(comm -23 <(printf '%s\n' "$rk") <(printf '%s\n' "$gk") | grep . || true)
  extra=$(comm -13 <(printf '%s\n' "$rk") <(printf '%s\n' "$gk") | comm -23 - <(printf '%s\n' "$allow") | grep . || true)
  if [ -z "$missing$extra" ]; then
    echo "[gatestack] keys: the gate's .env sets every key the reference .env sets ($(printf '%s\n' "$rk" | grep -c .) keys) and no undeclared other"
    return 0
  fi
  for k in $missing; do echo "[gatestack] keys: MISSING from the gate stack: $k (the reference .env sets it; give it a gate value in scripts/lib/gatestack.sh gs_values_)"; done
  for k in $extra; do echo "[gatestack] keys: EXTRA in the gate stack: $k (the reference .env does not set it; drop it, or declare it in GS_GATE_ONLY with the reason)"; done
  echo "[gatestack] keys: REFUSED — the gate stack's .env and the reference .env differ in shape (names above; no value is ever read out)"
  return 1
}
# The gate's environment, exported. A key the generated file sets is UNSET from this shell first, because a
# shell variable wins over the env file in compose's interpolation — the file must be the whole truth.
gs_export_() {
  local k
  for k in $(gs_keys_of_ "$GS_ENVF"); do
    case $k in COMPOSE_PROJECT_NAME) continue ;; esac
    unset "$k" 2>/dev/null || true
  done
  export COMPOSE_ENV_FILES=$GS_ENVF COMPOSE_PROJECT_NAME=$GATE_PROJECT BASE=$GS_BASE
}
gs_ready_() {   # /readyz = 200 within 180 s (a bounded curl: under WSL's mirrored networking a closed loopback port times out, erratum 338)
  local code="" end=$((SECONDS + 180))
  while [ "$SECONDS" -lt "$end" ]; do
    code=$(curl -s --connect-timeout 2 --max-time 5 -o /dev/null -w '%{http_code}' "$GS_BASE/readyz" 2>/dev/null || true)
    [ "$code" = 200 ] && return 0
    sleep 0.5
  done
  echo "[gatestack] $GS_BASE/readyz did not answer 200 within 180 s (last: ${code:-none})"; return 1
}
gs_down_() {
  local names
  if [ ! -f "$GS_ENVF" ]; then gs_write_ "$GS_ENVF"; fi   # down needs the same COMPOSE_FILE set the stack was made with
  gs_export_
  echo "[gatestack] down: compose project $GATE_PROJECT (its containers, its network and its volumes), image tags $GS_IMAGE $GS_GPU_IMAGE, and $GS_ENVF"
  # A probe container a killed bundle left (the E2E's clippet-e2e-*), by THIS project's label only.
  names=$(MSYS_NO_PATHCONV=1 docker ps -a --filter "label=com.docker.compose.project=$GATE_PROJECT" --format '{{.Names}}' 2>/dev/null | tr -d '\r' | grep '^clippet-e2e-' || true)
  # shellcheck disable=SC2086
  [ -z "$names" ] || { echo "[gatestack] down: probe containers of $GATE_PROJECT: $(printf '%s' "$names" | tr '\n' ' ')"; MSYS_NO_PATHCONV=1 docker rm -f $names >/dev/null 2>&1 || true; }
  MSYS_NO_PATHCONV=1 docker compose down -v --remove-orphans 2>&1 | sed 's/^/[gatestack]   /'
  MSYS_NO_PATHCONV=1 docker image rm "$GS_IMAGE" "$GS_GPU_IMAGE" >/dev/null 2>&1 || true
  rm -f "$GS_ENVF"; rmdir "$GS_DIR" 2>/dev/null || true
  echo "[gatestack] down: done"
}

# ---- visual only when web/ moves (fix 7) -------------------------------------------------------------------
# gs_visual_decide_ — one line: `RUN <why>` or `SKIPPED (no change under web/ since <commit>)`.
# The baseline is the last GREEN gate's commit (GS_GREEN, written by gate.sh on GATE PASS) when it is an
# ancestor of HEAD; else the merge-base with main when HEAD is not main; else nothing is known and it RUNS.
# The diff is that baseline to HEAD plus the working tree, over web/, internal/web/ and internal/httpx/ (the
# CSP and the other security headers live in internal/httpx/middleware.go, and the visual pass asserts no
# console or CSP error on every page). GATE_VISUAL=1 and GATE_RELEASE=1 (a release gate) always run it.
GS_WEB_PATHS="web internal/web internal/httpx"
gs_visual_decide_() {
  local base="" from="" changed
  if [ "${GATE_RELEASE:-0}" = 1 ]; then echo "RUN (GATE_RELEASE=1: a release gate always runs the visual pass)"; return 0; fi
  if [ "${GATE_VISUAL:-0}" = 1 ]; then echo "RUN (GATE_VISUAL=1)"; return 0; fi
  if [ -s "$GS_GREEN" ]; then
    base=$(sed -n 's/^commit=//p' "$GS_GREEN" | tail -1 | tr -d '\r')
    if [ -n "$base" ] && git merge-base --is-ancestor "$base" HEAD 2>/dev/null; then from="the last green gate"
    else base=""; fi
  fi
  if [ -z "$base" ]; then
    local head main
    head=$(git rev-parse HEAD 2>/dev/null || true); main=$(git rev-parse --verify -q main 2>/dev/null || true)
    if [ -n "$main" ] && [ "$head" != "$main" ]; then base=$(git merge-base main HEAD 2>/dev/null || true); from="main"; fi
  fi
  if [ -z "$base" ]; then echo "RUN (no green gate is recorded for an ancestor of HEAD, and HEAD is main: nothing to diff against)"; return 0; fi
  # shellcheck disable=SC2086
  changed=$( { git diff --name-only "$base" HEAD -- $GS_WEB_PATHS; git status --porcelain -- $GS_WEB_PATHS | cut -c4-; } 2>/dev/null | sort -u | grep . || true)
  if [ -n "$changed" ]; then
    echo "RUN ($(printf '%s\n' "$changed" | grep -c .) file(s) under web/, internal/web/ or internal/httpx/ changed since $(git rev-parse --short "$base") ($from): $(printf '%s\n' "$changed" | head -3 | tr '\n' ' '))"
    return 0
  fi
  echo "SKIPPED (no change under web/ since $(git rev-parse --short "$base"))"
}
# ---- the gate's input key (run 15, lane `gatekey`) -------------------------------------------------------------
# A green gate's record also carries WHAT it proved, so a later reader can tell a byte-identical input from a
# commit that merely moved (every release cycle is followed by a `records:` commit, so commit= alone never
# matches twice). Nothing in this repo reads the two lines; clippet-pass's release cycle reads them for its
# opt-in --gate-cached, and computes its side with `gatestack.sh gate-key` in its clone — this one function.
#   tree=<sha256 of `git ls-tree -r HEAD` over GS_KEY_PATHS + the root's Dockerfile* and compose*.yaml>
#     the gate's inputs: what `docker build` copies (internal cmd web testdata go.mod go.sum and the Dockerfiles,
#     .dockerignore deciding the context), the compose files the stack is made of, .env.example (the gate's .env
#     is built from its active lines), the whole harness (scripts/: gate.sh, check.sh, e2e.sh and every drop-in),
#     DEPLOY.md (check.sh stage 17 reads its Contents) and docs/brand (stage 06's mkicons.py --check compares the
#     mark and the Discord icons there). Docs, records and every other *.md do not move it.
#     `dirty` when any of those paths has a local change or an untracked file: the gate built the working tree,
#     not HEAD, so no HEAD hash may stand for it.
#   env=<sha256 of the sorted KEY NAMES of the reference .env> (gs_ref_env_, the file `keys` compares; names
#     only — gs_keys_of_ keeps nothing right of the `=`), `none` when there is no such file.
GS_KEY_PATHS="internal cmd web testdata go.mod go.sum .dockerignore .env.example scripts DEPLOY.md docs/brand"
gs_tree_key_() {
  local roots
  git rev-parse --verify -q HEAD >/dev/null 2>&1 || { echo none; return 0; }
  # ls-tree takes literal paths only, so the two root globs are resolved against HEAD's own listing; status takes
  # them as pathspecs, so a new untracked compose file is a local change too.
  roots=$(git ls-tree --name-only HEAD | tr -d '\r' | grep -E '^(Dockerfile.*|compose.*\.yaml)$' | tr '\n' ' ')
  # shellcheck disable=SC2086
  if [ -n "$(git status --porcelain -- $GS_KEY_PATHS 'Dockerfile*' 'compose*.yaml' 2>/dev/null | head -1)" ]; then echo dirty; return 0; fi
  # shellcheck disable=SC2086
  git ls-tree -r HEAD -- $GS_KEY_PATHS $roots | sha256sum | cut -d' ' -f1
}
gs_env_key_() {   # gs_env_key_ [<dotenv file>] — default the reference .env
  local f=${1:-$(gs_ref_env_)}
  [ -f "$f" ] || { echo none; return 0; }
  gs_keys_of_ "$f" | sha256sum | cut -d' ' -f1
}
# The host half of the key (G1: an equal tree on another engine, driver or base image is not the same gate).
# Read-only, well under a second: the docker server's version, the nvidia driver (`none` without nvidia-smi,
# as on a CPU box), and images=<sha256 of `<ref> <local image id>` for each base image the root's Dockerfile*
# FROM lines name — stage aliases skipped, a ${ARG} taken from its default, `absent` for one not pulled here.
gs_base_refs_() {   # the base images of ./Dockerfile*, one per line, sorted
  local f
  for f in Dockerfile*; do
    [ -f "$f" ] || continue
    tr -d '\r' < "$f" | awk '
      toupper($1) == "ARG" { n = index($2, "="); if (n > 1) arg[substr($2, 1, n - 1)] = substr($2, n + 1); next }
      toupper($1) == "FROM" {
        i = 2; while ($i ~ /^--/) i++
        ref = $i
        for (k in arg) { gsub("\\$\\{" k "\\}", arg[k], ref) }
        for (j = i + 1; j < NF; j++) if (toupper($j) == "AS") alias[tolower($(j + 1))] = 1
        if (!(tolower(ref) in alias) && ref != "scratch") print ref
      }'
  done | sort -u
}
gs_host_key_() {
  local v d
  v=$(docker version -f '{{.Server.Version}}' 2>/dev/null | tr -d '\r'); echo "docker=${v:-none}"
  d=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | tr -d '\r' | head -1); echo "nvidia=${d:-none}"
  echo "images=$(gs_base_refs_ | while read -r r; do id=$(docker image inspect -f '{{.Id}}' "$r" 2>/dev/null | tr -d '\r'); echo "$r ${id:-absent}"; done | sha256sum | cut -d' ' -f1)"
}
gs_gate_key_() { echo "tree=$(gs_tree_key_)"; echo "env=$(gs_env_key_)"; gs_host_key_; }
gs_flavour_() { if [ "$GATE_GPU" = 1 ]; then echo gpu; else echo cpu; fi; }   # `flavour`: the image the gate stack proves
# gs_pins_ — `pins`: every image the gate stack's compose files name that is PULLED, not built (the app's own two tags
# skipped), must be PINNED BY DIGEST (`name:tag@sha256:<index>`) and be in the local cache or pullable now, so a vanished
# or moved pin is a refusal before stage 1 instead of an E2E that dies in its first two seconds (run 21 review: Docker Hub
# dropped minio/minio and minio/mc, quay.io has no such tags; compose.yaml's two tag-only pins existed only in a box's
# cache — run 22 replaced them in compose.e2e-bucket.yaml with RustFS and pgsty/mc, each by its multi-arch index digest, so
# the PC and the Mac resolve the same bytes; erratum 346 removed the client, so RustFS is the one left). A present image is
# never pulled again. Read-only but for a pull.
gs_pins_() {
  local refs r bad=0 n=0
  refs=$( (gs_export_; MSYS_NO_PATHCONV=1 docker compose config --images 2>/dev/null) | tr -d '\r' | sort -u) \
    || { echo "[gatestack] pins: FAIL — docker compose config --images did not answer"; return 1; }
  [ -n "$refs" ] || { echo "[gatestack] pins: FAIL — the gate stack's compose files named no image"; return 1; }
  for r in $refs; do
    case $r in "$GS_IMAGE"|"$GS_GPU_IMAGE") continue ;; esac
    n=$((n + 1))
    case $r in
      *@sha256:[0-9a-f]*) ;;
      *) echo "[gatestack] pins: NOT BY DIGEST $r (a tag can move or vanish: pin it as <name>:<tag>@sha256:<index digest>)"; bad=$((bad + 1)); continue ;;
    esac
    if docker image inspect "$r" >/dev/null 2>&1; then echo "[gatestack] pins: $r present"
    elif docker pull -q "$r" >/dev/null 2>&1; then echo "[gatestack] pins: $r pulled"
    else echo "[gatestack] pins: UNAVAILABLE $r (not in this box's cache, and the registry gives no such image)"; bad=$((bad + 1)); fi
  done
  if [ "$bad" = 0 ]; then echo "[gatestack] pins: OK ($n pulled images, each pinned by digest and present or pulled)"; return 0; fi
  echo "[gatestack] pins: FAIL ($bad of $n pulled images unavailable or not pinned by digest: load each from a box that still has it — docker save there, docker load here — or re-pin the compose file that names it)"
  return 1
}   # `gate-key`: the record's key lines, the one recipe
# gs_green_record_ <base> <ran|skipped> <gate dir> — gate.sh, on GATE PASS: the commit, what the run WAS (release=1
# only under GATE_RELEASE=1; visual= stage 3's decision, `unknown` when the caller did not say; dir= the gate's logs,
# whose gate.log ends GATE PASS; flavour= gpu|cpu, the image the stack ran — a Mac gate is cpu) and the input key — unless the tree has local changes under web/. A reader that
# skips a gate on this record checks release=1 and visual=ran itself: a nightly's or a lane's T2 records release=0.
gs_green_record_() {
  local dirty
  # shellcheck disable=SC2086
  dirty=$(git status --porcelain -- $GS_WEB_PATHS 2>/dev/null | head -1)
  if [ -n "$dirty" ]; then echo "[gatestack] not recording a green gate: local changes under web/ (the next gate will run the visual pass)"; return 0; fi
  mkdir -p "$(dirname "$GS_GREEN")"
  local key rel vis fl
  key=$(gs_gate_key_); fl=$(gs_flavour_)
  if [ "${GATE_RELEASE:-0}" = 1 ]; then rel=1; else rel=0; fi
  case ${2:-} in ran|skipped) vis=$2 ;; *) vis=unknown ;; esac
  { echo "commit=$(git rev-parse HEAD)"; echo "proved=$(date -u +%Y-%m-%dT%H:%M:%SZ)"; echo "base=${1:-?}"
    echo "release=$rel"; echo "visual=$vis"; echo "dir=${3:-?}"; echo "flavour=$fl"; printf '%s\n' "$key"; } > "$GS_GREEN"
  echo "[gatestack] green gate recorded: $(git rev-parse --short HEAD) (release=$rel, visual=$vis, flavour=$fl, $(printf '%s\n' "$key" | grep -E '^(tree|env)=' | cut -c1-17 | tr '\n' ' ')) in $GS_GREEN"
}

# ---- the self-test (scripts/check.sh runs it; no daemon, no network, no stack) -------------------------------
gs_selftest_() {
  local t n=0 bad=0 out rc c p
  # The decision cases below are about the DIFF, not the caller: a release gate (GATE_RELEASE=1, as the release cycle
  # exports it around check.sh) or a forced visual pass would answer RUN for every case. Found by release cycle 5's
  # first gate step (2026-10-09): three cases failed under the cycle and passed alone.
  unset GATE_RELEASE GATE_VISUAL
  ok_() { n=$((n + 1)); if [ "$1" = "$2" ]; then :; else bad=$((bad + 1)); echo "[gatestack selftest] FAIL: $3 (got '$1', want '$2')"; fi; }
  t=$(mktemp -d)
  # 1. the key-diff. The reference copy carries a VALUE that must never be printed.
  GS_ENVF=$t/gate.env gs_write_ "$t/gate.env"
  gs_keys_of_ "$t/gate.env" | grep -vxE "$(printf '%s|' $GS_GATE_ONLY | sed 's/|$//')" | sed 's/$/=never-printed-value/' > "$t/ref.env"
  # .env.example's own keys may be absent from a reference: drop them from it as an operator's file would.
  out=$(gs_keydiff_ "$t/ref.env" "$t/gate.env" 2>&1); rc=$?
  ok_ "$rc" 0 "a reference .env with exactly the gate's non-gate-only keys passes"
  printf 'PLANTED_EXTRA_KEY=never-printed-value\n' >> "$t/ref.env"
  out=$(gs_keydiff_ "$t/ref.env" "$t/gate.env" 2>&1); rc=$?
  ok_ "$rc" 1 "a key the reference sets and the gate does not is REFUSED"
  ok_ "$(printf '%s\n' "$out" | grep -c 'MISSING from the gate stack: PLANTED_EXTRA_KEY')" 1 "…by its name"
  ok_ "$(printf '%s\n' "$out" | grep -c 'never-printed-value')" 0 "…and no value reaches the transcript"
  grep -v '^PLANTED_EXTRA_KEY=' "$t/ref.env" > "$t/ref.env.new"; mv "$t/ref.env.new" "$t/ref.env"   # not sed -i: BSD sed reads its argument as a suffix
  printf 'GATE_ONLY_DRIFT=1\n' >> "$t/gate.env"
  out=$(gs_keydiff_ "$t/ref.env" "$t/gate.env" 2>&1); rc=$?
  ok_ "$rc" 1 "an undeclared key the gate sets and the reference does not is REFUSED"
  ok_ "$(printf '%s\n' "$out" | grep -c 'EXTRA in the gate stack: GATE_ONLY_DRIFT')" 1 "…by its name"
  # 2. visual only when web/ moves, in a scratch repository: docs-only → SKIPPED, a web/ whitespace → RUN.
  mkdir -p "$t/repo/web" "$t/repo/docs" "$t/repo/internal/httpx" "$t/repo/.e2e/checkpoints"
  (
    cd "$t/repo" || exit 1
    git init -q -b main . && git config user.email t@t && git config user.name t
    printf 'a\n' > web/app.css; printf 'd\n' > docs/x.md; printf 'h\n' > internal/httpx/middleware.go
    git add . && git commit -qm one
    printf 'commit=%s\n' "$(git rev-parse HEAD)" > .e2e/checkpoints/gate-green.txt
    printf 'd2\n' > docs/x.md; git commit -qam docs
    echo "docs:$(gs_visual_decide_)"
    printf 'a \n' > web/app.css
    echo "webdirty:$(gs_visual_decide_)"
    git commit -qam web
    echo "webcommitted:$(gs_visual_decide_)"
    git checkout -q HEAD~1 -- web/app.css && git commit -qm revert
    echo "reverted-but-moved:$(gs_visual_decide_)"
    printf 'h2\n' > internal/httpx/middleware.go
    echo "httpx:$(gs_visual_decide_)"
    git checkout -q -- internal/httpx/middleware.go
    echo "release:$(GATE_RELEASE=1 gs_visual_decide_)"
    rm -f .e2e/checkpoints/gate-green.txt
    echo "nogreen-on-main:$(gs_visual_decide_)"
  ) > "$t/vis.txt" 2>&1
  ok_ "$(grep -c '^docs:SKIPPED (no change under web/ since ' "$t/vis.txt")" 1 "a docs-only diff since the green gate SKIPS the visual pass"
  ok_ "$(grep -c '^webdirty:RUN ' "$t/vis.txt")" 1 "a whitespace change under web/ in the working tree RUNS it"
  ok_ "$(grep -c '^webcommitted:RUN ' "$t/vis.txt")" 1 "…and so does the same change committed"
  ok_ "$(grep -c '^reverted-but-moved:SKIPPED ' "$t/vis.txt")" 1 "a change made and reverted since the green gate SKIPS it (web/ is byte-for-byte what that gate photographed)"
  ok_ "$(grep -c '^httpx:RUN ' "$t/vis.txt")" 1 "a change under internal/httpx/ (the CSP header) RUNS it"
  ok_ "$(grep -c '^release:RUN (GATE_RELEASE=1' "$t/vis.txt")" 1 "GATE_RELEASE=1 always RUNS it"
  ok_ "$(grep -c '^nogreen-on-main:RUN (no green gate' "$t/vis.txt")" 1 "no green gate recorded and HEAD on main RUNS it"
  # 3. the input key (G1), in a scratch repository: docs and records keep it, every gate input moves it, a local
  # change records `dirty`; env= is the key NAMES only. Each line is `<case>:<tree key>`, compared below.
  mkdir -p "$t/key/internal" "$t/key/web" "$t/key/scripts" "$t/key/docs/brand" "$t/key/docs/runs"
  printf 'A=never-printed-value\nB=2\n' > "$t/ref1.env"
  printf 'B=other-value\r\nexport A=3\r\n# C=commented\r\n' > "$t/ref2.env"
  printf 'A=1\nB=2\nC=3\n' > "$t/ref3.env"
  (
    cd "$t/key" || exit 1
    git init -q -b main . && git config user.email t@t && git config user.name t
    printf 'package x\n' > internal/x.go; printf 'a\n' > web/app.css; printf 's\n' > scripts/e2e.sh
    printf 'm\n' > docs/brand/mark.svg; printf 'r\n' > docs/runs/x.md; printf 'S\n' > STATE.md
    printf 'D\n' > DEPLOY.md; printf 'FROM x\n' > Dockerfile; printf 'v\n' > Dockerfile.visual
    printf 'c\n' > compose.yaml; printf 'K=\n' > .env.example
    git add . && git commit -qm one
    echo "first:$(gs_tree_key_)"
    printf 'r2\n' > docs/runs/x.md; printf 'S2\n' > STATE.md; git commit -qam records
    echo "records:$(gs_tree_key_)"
    printf 's2\n' > scripts/e2e.sh; git commit -qam harness; echo "scripts:$(gs_tree_key_)"
    printf 'a2\n' > web/app.css; git commit -qam web; echo "web:$(gs_tree_key_)"
    printf 'D2\n' > DEPLOY.md; git commit -qam deploy; echo "deploymd:$(gs_tree_key_)"
    printf 'm2\n' > docs/brand/mark.svg; git commit -qam brand; echo "brand:$(gs_tree_key_)"
    printf 'v2\n' > Dockerfile.visual; git commit -qam visual; echo "dockerfile-glob:$(gs_tree_key_)"
    printf 'g\n' > compose.extra.yaml; echo "untracked-compose:$(gs_tree_key_)"
    git add compose.extra.yaml && git commit -qm compose; echo "compose-glob:$(gs_tree_key_)"
    printf 'package y\n' > internal/x.go; echo "dirty:$(gs_tree_key_)"
    # The host half against stubs (no daemon): functions shadow the two commands for this subshell only.
    docker() { case $1 in version) echo 99.1 ;; image) echo "sha256:${STUB_IMG:-one}-${*: -1}" ;; *) return 1 ;; esac; }
    nvidia-smi() { echo 555.55; }
    GATE_REF_ENV=$t/ref1.env gs_green_record_ "http://localhost:1" >/dev/null
    echo "rec-dirty:$(sed -n 's/^tree=//p' .e2e/checkpoints/gate-green.txt)"
    git checkout -q -- internal/x.go
    GATE_REF_ENV=$t/ref1.env gs_green_record_ "http://localhost:1" >/dev/null
    echo "rec-tree:$(sed -n 's/^tree=//p' .e2e/checkpoints/gate-green.txt)"
    echo "rec-env:$(sed -n 's/^env=//p' .e2e/checkpoints/gate-green.txt)"
    echo "rec-commit:$(sed -n 's/^commit=//p' .e2e/checkpoints/gate-green.txt)"
    echo "rec-proved:$(grep -c '^proved=' .e2e/checkpoints/gate-green.txt)"
    echo "rec-leak:$(grep -c 'never-printed-value' .e2e/checkpoints/gate-green.txt)"
    echo "now-tree:$(gs_tree_key_)"; echo "head:$(git rev-parse HEAD)"
    echo "verb:$(GATE_REF_ENV=$t/ref1.env gs_main_ gate-key | tr '\n' ' ')"
    echo "rec-keylines:$(grep -E '^(tree|env|docker|nvidia|images)=' .e2e/checkpoints/gate-green.txt | tr '\n' ' ')"
    # What the run WAS: a plain T2 (a nightly, a lane's) is release=0 whatever its tree; visual= is the caller's word.
    echo "rec-release:$(sed -n 's/^release=//p' .e2e/checkpoints/gate-green.txt)"
    echo "rec-visual:$(sed -n 's/^visual=//p' .e2e/checkpoints/gate-green.txt)"
    echo "rec-dir:$(sed -n 's/^dir=//p' .e2e/checkpoints/gate-green.txt)"
    echo "rec-docker:$(sed -n 's/^docker=//p' .e2e/checkpoints/gate-green.txt)"
    echo "rec-nvidia:$(sed -n 's/^nvidia=//p' .e2e/checkpoints/gate-green.txt)"
    echo "rec-images:$(sed -n 's/^images=//p' .e2e/checkpoints/gate-green.txt)"
    GATE_RELEASE=1 GATE_REF_ENV=$t/ref1.env gs_green_record_ "b" ran .e2e/gate-x >/dev/null
    echo "rel-release:$(sed -n 's/^release=//p' .e2e/checkpoints/gate-green.txt)"
    echo "rel-visual:$(sed -n 's/^visual=//p' .e2e/checkpoints/gate-green.txt)"
    echo "rel-dir:$(sed -n 's/^dir=//p' .e2e/checkpoints/gate-green.txt)"
    GATE_REF_ENV=$t/ref1.env gs_green_record_ "b" skipped .e2e/gate-y >/dev/null
    echo "skip-visual:$(sed -n 's/^visual=//p' .e2e/checkpoints/gate-green.txt)"
    STUB_IMG=two GATE_REF_ENV=$t/ref1.env gs_green_record_ "b" bogus >/dev/null
    echo "bogus-visual:$(sed -n 's/^visual=//p' .e2e/checkpoints/gate-green.txt)"
    echo "moved-images:$(sed -n 's/^images=//p' .e2e/checkpoints/gate-green.txt)"
    echo "rec-flavour:$(GATE_GPU=0; GATE_REF_ENV=$t/ref1.env gs_green_record_ "b" >/dev/null; sed -n 's/^flavour=//p' .e2e/checkpoints/gate-green.txt)/$(GATE_GPU=1; GATE_REF_ENV=$t/ref1.env gs_green_record_ "b" >/dev/null; sed -n 's/^flavour=//p' .e2e/checkpoints/gate-green.txt)"
    docker() { return 1; }; nvidia-smi() { return 127; }
    echo "nohost:$(gs_host_key_ | tr '\n' ' ')"
  ) > "$t/key.txt" 2>&1
  # The base-image list: stage aliases, scratch and --platform skipped, an ARG default substituted, CRLF tolerated.
  mkdir -p "$t/img"
  printf 'ARG V=1.2\r\nFROM --platform=linux/amd64 lib/a:${V}-x AS base\r\nFROM base AS build\r\nfrom b:3 as Other\r\nFROM other\r\nFROM scratch\r\n' > "$t/img/Dockerfile"
  printf 'FROM c@sha256:00\n' > "$t/img/Dockerfile.visual"
  ok_ "$(cd "$t/img" && gs_base_refs_ | tr '\n' ' ')" "b:3 c@sha256:00 lib/a:1.2-x " "the base images are the FROM refs that are not a stage, with ARG defaults resolved"
  kv_() { sed -n "s/^$1://p" "$t/key.txt" | tail -1; }
  ok_ "$(kv_ first | grep -cE '^[0-9a-f]{64}$')" 1 "the tree key is a sha256"
  ok_ "$(kv_ records)" "$(kv_ first)" "a docs/records-only commit keeps the tree key"
  for c in scripts web deploymd brand dockerfile-glob compose-glob; do
    case $c in scripts) p=records ;; web) p=scripts ;; deploymd) p=web ;; brand) p=deploymd ;; dockerfile-glob) p=brand ;; compose-glob) p=dockerfile-glob ;; esac
    ok_ "$( [ "$(kv_ $c)" != "$(kv_ $p)" ] && kv_ $c | grep -cE '^[0-9a-f]{64}$' || echo same)" 1 "a commit under $c moves the tree key"
  done
  ok_ "$(kv_ untracked-compose)" dirty "an untracked compose*.yaml is a local change (dirty)"
  ok_ "$(kv_ dirty)" dirty "an uncommitted edit under internal/ is a local change (dirty)"
  ok_ "$(kv_ rec-dirty)" dirty "…and a green gate on that tree records tree=dirty, never HEAD's hash"
  ok_ "$(kv_ rec-tree)" "$(kv_ now-tree)" "the green record's tree= is the clean tree's key"
  ok_ "$(kv_ rec-commit)" "$(kv_ head)" "…written beside commit="
  ok_ "$(kv_ rec-proved)" 1 "…and proved="
  ok_ "$(kv_ rec-env)" "$(gs_env_key_ "$t/ref1.env")" "the green record's env= is the reference .env's key-name hash"
  ok_ "$(kv_ rec-leak)" 0 "…and no value reaches the record"
  ok_ "$(kv_ verb)" "$(kv_ rec-keylines)" "gate-key prints the same key lines the record carries"
  ok_ "$(kv_ verb | grep -c "^tree=$(kv_ now-tree) env=$(kv_ rec-env) docker=99.1 nvidia=555.55 images=")" 1 "…tree, env, docker, nvidia, images, in that order"
  ok_ "$(kv_ rec-release)" 0 "a green record written without GATE_RELEASE=1 says release=0"
  ok_ "$(kv_ rel-release)" 1 "…and with GATE_RELEASE=1, release=1"
  ok_ "$(kv_ rec-visual)" unknown "visual= is unknown when the caller did not say"
  ok_ "$(kv_ rel-visual)/$(kv_ skip-visual)/$(kv_ bogus-visual)" ran/skipped/unknown "…ran or skipped as stage 3 decided, anything else unknown"
  ok_ "$(kv_ rec-dir)/$(kv_ rel-dir)" "?/.e2e/gate-x" "dir= is the gate's log directory"
  ok_ "$(kv_ rec-flavour)" cpu/gpu "flavour= is cpu under GATE_GPU=0 (a Mac's default) and gpu under GATE_GPU=1"
  # 4. the pins (no daemon): a stub docker names five images — the app's own tag (skipped), one cached, one pullable,
  # one gone, one pinned by tag alone — and the verb refuses naming exactly the gone one and the tag-only one.
  # (the stub's case lives outside the $( ): bash 3.2 misparses a case pattern's `)` inside a command substitution)
  gs_stub_docker_() {
    case "$1 $2" in
      "compose config") printf '%s\r\n' "$GS_IMAGE" cached:1@sha256:aa pullable:2@sha256:bb gone:3@sha256:cc tagonly:4 ;;
      "image inspect") [ "$3" = cached:1@sha256:aa ] || [ "$3" = tagonly:4 ] ;;
      "pull -q") [ "$3" = pullable:2@sha256:bb ] ;;
      *) return 1 ;;
    esac
  }
  out=$( (
    docker() { gs_stub_docker_ "$@"; }
    GS_ENVF=$t/gate.env gs_pins_; echo "rc=$?"
  ) 2>&1)
  ok_ "$(printf '%s\n' "$out" | grep -c '^\[gatestack\] pins: cached:1@sha256:aa present$')/$(printf '%s\n' "$out" | grep -c '^\[gatestack\] pins: pullable:2@sha256:bb pulled$')" 1/1 "pins: a cached image is present, a missing one is pulled"
  ok_ "$(printf '%s\n' "$out" | grep -c '^\[gatestack\] pins: UNAVAILABLE gone:3@sha256:cc ')/$(printf '%s\n' "$out" | grep -c "$GS_IMAGE")" 1/0 "…an unpullable one is UNAVAILABLE by name, the app's own tag never checked"
  ok_ "$(printf '%s\n' "$out" | grep -c '^\[gatestack\] pins: NOT BY DIGEST tagonly:4 ')/$(printf '%s\n' "$out" | grep -c 'tagonly:4 present')" 1/0 "…a tag-only pin is refused by name even when it sits in the cache"
  ok_ "$(printf '%s\n' "$out" | grep -c '^\[gatestack\] pins: FAIL (2 of 4 ')/$(printf '%s\n' "$out" | tail -1)" 1/rc=1 "…and the verb fails, 2 of 4"
  # The overlay the gate stack is made of names its ONE pulled image by digest, both architectures in one index, and
  # no client image at all (§20 erratum 346: the app's `admin objects` does what `mc` did).
  ok_ "$(grep -cE '^    image: rustfs/rustfs:[^@ ]+@sha256:[0-9a-f]{64}$' compose.e2e-bucket.yaml)/$(grep -cE '^    image:' compose.e2e-bucket.yaml)" 1/1 "compose.e2e-bucket.yaml pins its one image, the S3 server, by digest"
  ok_ "$(grep -c 'bash "$GS" pins' scripts/gate.sh)" 1 "gate.sh runs the pins before stage 1"
  ok_ "$(kv_ rec-docker)/$(kv_ rec-nvidia)" 99.1/555.55 "docker= is the server version, nvidia= the driver"
  ok_ "$(kv_ rec-images | grep -cE '^[0-9a-f]{64}$')" 1 "images= is a sha256"
  ok_ "$( [ "$(kv_ moved-images)" != "$(kv_ rec-images)" ] && echo moved)" moved "…that moves when a base image's local id does"
  ok_ "$(kv_ nohost | cut -d' ' -f1-2)" "docker=none nvidia=none" "without docker or nvidia-smi the two say none"
  ok_ "$(grep -c '"$GS" green "$BASE" "$GATE_VISUAL_RAN" "$GATE_DIR"' scripts/gate.sh)" 1 "gate.sh hands the record stage 3's decision and its log directory"
  ok_ "$(gs_env_key_ "$t/ref2.env")" "$(gs_env_key_ "$t/ref1.env")" "env= ignores values, order, export, CRLF and comments"
  ok_ "$( [ "$(gs_env_key_ "$t/ref3.env")" != "$(gs_env_key_ "$t/ref1.env")" ] && echo moved)" moved "env= moves with a key added"
  ok_ "$(gs_env_key_ "$t/absent.env")" none "env= is none without a reference .env"
  # 5. portable mode (run 25, lane pub-ci): a checkout with no .env compares against its .env.example and never refuses.
  mkdir -p "$t/port"
  (
    cd "$t/port" || exit 1
    git init -q -b main . && git config user.email t@t && git config user.name t
    printf 'K=1\n' > .env.example; git add . && git commit -qm one
    unset GATE_REF_ENV
    echo "noenv-ref:$(gs_ref_env_)"
    out=$(gs_keys_ok_ 2>&1); echo "noenv-rc:$?"
    echo "noenv-portable:$(printf '%s\n' "$out" | tail -1 | grep -c '^\[gatestack\] keys: PORTABLE — the reference is .env.example')"
    echo "noenv-named:$(printf '%s\n' "$out" | grep -c 'EXTRA in the gate stack: DISCORD_BOT_TOKEN')"
    echo "explicit-rc:$(GATE_REF_ENV=$PWD/.env.example gs_keys_ok_ >/dev/null 2>&1; echo $?)"
    printf 'PLANTED_EXTRA_KEY=never-printed-value\n' > .env
    echo "env-ref:$(gs_ref_env_)"
    out=$(gs_keys_ok_ 2>&1); echo "env-rc:$?"
    echo "env-portable:$(printf '%s\n' "$out" | grep -c PORTABLE)"
    rm -f .env.example .env
    echo "bare-rc:$(gs_keys_ok_ >/dev/null 2>&1; echo $?)"
  ) > "$t/port.txt" 2>&1
  pv_() { sed -n "s/^$1://p" "$t/port.txt" | tail -1; }
  ok_ "$(pv_ noenv-ref)" .env.example "no .env in the main worktree: the reference is .env.example (portable mode)"
  ok_ "$(pv_ noenv-rc)/$(pv_ noenv-portable)" 0/1 "…and the key-diff passes with a PORTABLE last line"
  ok_ "$(pv_ noenv-named)" 1 "…still naming each key it would have refused, informationally"
  ok_ "$(pv_ explicit-rc)" 0 "GATE_REF_ENV=<path>/.env.example is portable mode too (the nightly CI job)"
  ok_ "$(pv_ env-ref | grep -c '/\.env$')/$(pv_ env-rc)/$(pv_ env-portable)" 1/1/0 "with a .env present it is the reference, and a key it sets that the gate does not is still REFUSED"
  ok_ "$(pv_ bare-rc)" 1 "with neither an .env nor an .env.example the key-diff still refuses"
  rm -rf "$t"
  if [ "$bad" = 0 ]; then echo "[gatestack selftest] PASS ($n checks: the key-diff refuses a missing and an extra key by name and prints no value; visual SKIPPED on a docs-only diff, RUN on a web/ or internal/httpx/ change, a release and an unknown baseline; the input key kept by docs, moved by every gate input, dirty on a local change, env= by key names only, the host half (docker, nvidia, base-image ids); the record says release=0 off a release gate, visual=ran|skipped as stage 3 decided and flavour=cpu|gpu; the pins refuse an unpullable or tag-only image by name; with no .env the reference is .env.example and the key-diff is informational, portable mode)"; return 0; fi
  echo "[gatestack selftest] FAILED ($bad of $n)"; return 1
}

# gs_keys_ok_ — the key-diff on a SCRATCH copy of the generated file: a refused gate writes nothing it leaves behind.
# In PORTABLE mode (gs_portable_) the diff is informational: it is printed, and the last line says so and passes.
gs_keys_ok_() {
  local kt rc=0
  kt=$(mktemp -d); gs_write_ "$kt/gate.env"
  gs_keydiff_ "$(gs_ref_env_)" "$kt/gate.env" || rc=$?
  rm -rf "$kt"
  if gs_portable_; then
    echo "[gatestack] keys: PORTABLE — the reference is $(gs_ref_env_), not a running stack's .env (none here): the comparison above is informational (exit $rc), not a refusal"
    return 0
  fi
  return $rc
}

# ---- the verbs -----------------------------------------------------------------------------------------------
gs_main_() {
  local verb=${1:-}; shift || true
  case $verb in
    env)  gs_write_ "$GS_ENVF"; echo "[gatestack] wrote $GS_ENVF for $GATE_PROJECT ($GS_BASE; S3 127.0.0.1:$GATE_MINIO_PORT; gpu=$GATE_GPU; APP_CPUS=$GATE_APP_CPUS)" ;;
    keys) gs_keys_ok_ ;;
    up)
      gs_keys_ok_ || exit 1
      gs_write_ "$GS_ENVF"; gs_export_
      export CLIPPET_COMMIT="$(git rev-parse HEAD 2>/dev/null || true)"
      echo "[gatestack] up: $GATE_PROJECT on $GS_BASE (build commit ${CLIPPET_COMMIT:-none})"
      # the bucket BEFORE the app boots (run 25: the boot-time snapshot raced the init that used to follow `up`;
      # `run` brings the harness `s3` up through the app's depends_on and makes the bucket from a one-off container)
      if MSYS_NO_PATHCONV=1 docker compose config --services 2>/dev/null | grep -qx s3; then
        MSYS_NO_PATHCONV=1 docker compose run --rm --build -T app admin objects init || exit 1
      fi
      MSYS_NO_PATHCONV=1 docker compose up -d --build || exit 1
      # and once more after it, idempotent: a stack without the harness `s3` (a real bucket) is created here (§20 erratum 346)
      MSYS_NO_PATHCONV=1 docker compose exec -T app clippet admin objects init || exit 1
      gs_ready_ || exit 1
      echo "[gatestack] up: $GS_BASE/readyz = 200" ;;
    run)
      [ "$#" -gt 0 ] || gs_fail_ "run: name a command"
      gs_keys_ok_ || exit 1
      gs_write_ "$GS_ENVF"; gs_export_
      "$@" ;;
    once)   # CMD on the gate stack, then the stack down whatever CMD said (GATE_KEEP=1 keeps it); CMD's status
      [ "$#" -gt 0 ] || gs_fail_ "once: name a command"
      gs_keys_ok_ || exit 1
      gs_write_ "$GS_ENVF"
      local rc=0
      ( gs_export_; "$@" ) || rc=$?
      if [ "${GATE_KEEP:-0}" = 1 ]; then echo "[gatestack] GATE_KEEP=1: $GATE_PROJECT stays up ($GS_BASE); 'gatestack.sh down' removes it"; else gs_down_; fi
      return $rc ;;
    down) gs_down_ ;;
    visual-decision) gs_visual_decide_ ;;
    green) gs_green_record_ "${1:-}" "${2:-}" "${3:-}" ;;
    gate-key) gs_gate_key_ ;;
    flavour) gs_flavour_ ;;
    pins)   # on a scratch .env beside the real one (relative, as compose needs under MSYS), removed after: a refusal leaves nothing
      local rc=0; GS_ENVF=$GS_DIR/pins-$$.env
      mkdir -p "$GS_DIR"; gs_write_ "$GS_ENVF"; gs_pins_ || rc=$?
      rm -f "$GS_ENVF"; rmdir "$GS_DIR" 2>/dev/null || true
      return $rc ;;
    selftest) gs_selftest_ ;;
    *) sed -n '2,23p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
  esac
}
if [ "${BASH_SOURCE[0]}" = "$0" ]; then gs_main_ "$@"; fi
