#!/bin/sh
# box-tidy.sh [report|apply|--selftest] — THE BOX AS A WHOLE (the owner's decision of 2026-10-18:
# cleanup is part of every piece of work, so none of it is forgotten). Every lane, gate and T0/T1 run cleans up its OWN things (lane-teardown.sh, gate.sh's `down -v`, t0/t1's
# containers and tags), but nothing looked at the Docker box itself, and six weeks of runs left 73 images, 467 anonymous
# dangling volumes, 12 stopped containers and 29.5 GB of build cache behind (measured 2026-10-18). This script reports that
# litter and removes it — and ONLY what its allow-list names, which is the whole of its safety.
#
#   bash scripts/box-tidy.sh            # = report: read-only; one labelled line per class (count, bytes) with its items
#                                       #   indented under it, then ONE closing line: `box-tidy: <N> item(s), <bytes>
#                                       #   reclaimable` or `box-tidy: clean`. Exit 0 always (even with no Docker).
#   bash scripts/box-tidy.sh apply      # removes EXACTLY the items the report it prints first lists: every item is re-checked
#                                       #   for use BEFORE anything is removed (one in use → exit 2, nothing removed), then
#                                       #   each name is printed before its removal; ends `box-tidy: removed <N> item(s),
#                                       #   <bytes>` and the closing line of a re-report (`box-tidy: clean`).
#   bash scripts/box-tidy.sh --selftest # the pins, against a stub `docker` on PATH and scratch git repos (check.sh's boxtidy
#                                       #   stage, so T0 runs it): `box-tidy self-test: N passed, 0 failed`.
#
# THE ALLOW-LIST — nothing outside it is ever named for removal (the self-test plants one of every excluded kind):
#   1 volumes     ANONYMOUS dangling volumes only: the name is 64 lowercase hex digits AND `docker volume ls -f
#                 dangling=true` lists it. Never a named volume — clippet-gobuild, clippet-gomod, the deploy node_modules,
#                 the cargo targets and every project's own volumes are caches or data.
#   2 images      lane images `clippet-<lane>-app:local|gpu`, <lane> one word of [a-z0-9], not app/dev/gate/never/pass/main,
#                 with NO worktree named clippet-<lane> in this repo's `git worktree list` (nor in GATE_WIN_ROOT's, the
#                 Windows repo a WSL gate came from), and no container (any state) of it or of a child image. When no
#                 worktree list can be read the class is SKIPPED: an image is never litter by default.
#   3 tags        the harness's per-run tags `clippet-t0-deps:*`, `clippet-t1-deps:*` (t0.sh/t1.sh: <pid>-<epoch>) and
#                 `clippet-check-*:*` (check.sh's clippet-check-src:<pid>-<epoch>) older than BOX_TIDY_AGE (the tag's
#                 epoch, else the image's creation time; unknown age = not litter), with no container. The long-lived
#                 harness images (clippet-gotest:*, clippet-race:*, clippet-visual:*, clippet-rust:*) are NOT litter.
#   4 containers  STOPPED (exited/created/dead) containers named clippet-e2e-*, clippet-t0-*, clippet-t1-*, clippet-check-*
#                 older than BOX_TIDY_AGE. Never a compose SERVICE container (a com.docker.compose.service label that is not
#                 a one-off `run` — the E2E's probes are one-offs and do count), never a running one.
#   5 cache       build cache records unused for 168 h, reclaimable (`docker buildx du`, aged by LastUsedAt), listed as ONE
#                 item; bytes = the records that are not shared with an image. AN ESTIMATE, NOT IN THE CLOSING TOTAL: the
#                 prune applies Docker's own rule and has no dry run, and on a Windows GPU box it took none of what this counted
#                 (`removed 1.48 GB`, then the same 1.48 GB on the next report; run 18c, lane boxtidy). apply runs
#                 `docker builder prune -f --filter until=168h` when the class lists records, and its `removed` line counts
#                 the bytes the prune's OWN total line says it reclaimed (`Total:` / `Total reclaimed space:`), never the
#                 estimate. The one class that is not clippet's alone: the cache is the daemon's, and a week-unused record is
#                 rebuilt on demand, never data.
# NEVER: a name that does not start with `clippet` (other projects live on this Docker), a running container, an image in
# use, a named volume, `docker system prune`, `--volumes`, any `-a` prune. Every removal goes through bt_remove_, which
# refuses an empty name. Nothing of a .env is read.
#
# Knobs: BOX_TIDY_AGE (default 24h; Ns|Nm|Nh|Nd or seconds — `0s` lists a fresh plant, the live proof's knob),
# BOX_TIDY_CLASSES (default "volumes images tags containers cache"; a class left out says `skipped`), BOX_TIDY_REPO (the repo
# whose worktrees count; default this script's main worktree).
#
# HOOKS (part of the process, not a memory): scripts/lane-teardown.sh and scripts/gate.sh run `report` (read-only) at their
# end and print its closing line; clippet-pass's lane-teardown.sh calls this file the same way. When it reports litter the
# maintainer runs `bash scripts/box-tidy.sh apply` right then — the allow-list is the guard.
set -u
BT_SELF=$0
BT_ALL_CLASSES="volumes images tags containers cache"

bt_die_() { echo "box-tidy: $*" >&2; exit 1; }
# bt_bytes_ <docker's size: 5.4GB, 12.3kB, 32B*, 0B (virtual 1GB)> → bytes (docker's units are decimal).
bt_bytes_() {
  printf '%s\n' "$1" | awk '{ s = $1; sub(/\*$/, "", s)
    if (!match(s, /^[0-9.]+/)) { print 0; exit }
    n = substr(s, 1, RLENGTH); u = tolower(substr(s, RLENGTH + 1)); m = 1
    if (u == "kb" || u == "k") m = 1e3; else if (u == "mb") m = 1e6; else if (u == "gb") m = 1e9; else if (u == "tb") m = 1e12
    printf "%.0f\n", n * m }'
}
# bt_human_ <bytes> → 1.23 GB
bt_human_() {
  awk -v b="${1:-0}" 'BEGIN { split("B kB MB GB TB", u, " "); i = 1; while (b >= 1000 && i < 5) { b /= 1000; i++ }
    if (i == 1) printf "%d B\n", b; else printf "%.2f %s\n", b, u[i] }'
}
# bt_secs_ <24h|90m|0s|2d|3600> → seconds, or nothing when it is not a duration.
bt_secs_() {
  case $1 in
    *[!0-9smhd]*|''|[smhd]*) return 0 ;;
    *s) echo $(( ${1%s} )) ;; *m) echo $(( ${1%m} * 60 )) ;; *h) echo $(( ${1%h} * 3600 )) ;; *d) echo $(( ${1%d} * 86400 )) ;;
    *) echo $(( $1 )) ;;
  esac
}
# bt_epoch_ <docker's CreatedAt: 2026-10-18 17:39:07 -0400 EDT> → epoch seconds, or nothing.
# GNU date -d first; BSD date (a Mac) has no -d, so its -j -f reads the same stamp.
bt_epoch_() { s=$1; s=${s% [A-Z]*}; date -d "$s" +%s 2>/dev/null || date -j -f '%Y-%m-%d %H:%M:%S %z' "$s" +%s 2>/dev/null || true; }
bt_class_on_() { case " $BT_CLASSES " in *" $1 "*) return 0 ;; esac; return 1; }
# bt_unused_ <ref> → 0 when no container (any state) runs the image or a child of it.
bt_unused_() { [ -n "$1" ] || bt_die_ "bt_unused_: empty ref"; [ -z "$(docker ps -a -q --filter "ancestor=$1" < /dev/null 2>/dev/null | tr -d '\r')" ]; }

# bt_collect_ <dir> — writes <dir>/items: `class|name|bytes` per litter item, in removal order (containers before the images
# and volumes they could hold, the cache last), and <dir>/notes. Read-only.
bt_collect_() {
  d=$1; : > "$d/items"; : > "$d/notes"
  now=$(date +%s); age=$(bt_secs_ "$BOX_TIDY_AGE")
  [ -n "$age" ] || bt_die_ "BOX_TIDY_AGE=$BOX_TIDY_AGE is not a duration (Ns|Nm|Nh|Nd or seconds)"
  # -- 4 containers ----------------------------------------------------------------------------------------------------
  if bt_class_on_ containers; then
    docker ps -a --size --format '{{.Names}}|{{.State}}|{{.CreatedAt}}|{{.Size}}|{{.Label "com.docker.compose.service"}}|{{.Label "com.docker.compose.oneoff"}}' 2>/dev/null \
      | tr -d '\r' > "$d/ps" || : > "$d/ps"
    while IFS='|' read -r n st cr sz svc one; do
      [ -n "$n" ] || continue
      case $n in clippet-e2e-*|clippet-t0-*|clippet-t1-*|clippet-check-*) ;; *) continue ;; esac
      case $st in exited|created|dead) ;; *) continue ;; esac
      if [ -n "$svc" ] && [ "$one" != True ]; then continue; fi
      e=$(bt_epoch_ "$cr"); [ -n "$e" ] || continue
      [ $(( now - e )) -ge "$age" ] || continue
      echo "ctr|$n|$(bt_bytes_ "$sz")" >> "$d/items"
    done < "$d/ps"
  fi
  # -- 2 images + 3 tags (one listing; the full one is read so a shared image id is counted as freeing nothing) ---------
  if bt_class_on_ images || bt_class_on_ tags; then
    docker images --format '{{.Repository}}:{{.Tag}}|{{.ID}}|{{.CreatedAt}}|{{.Size}}' 2>/dev/null | tr -d '\r' > "$d/images" || : > "$d/images"
    wt_ok=0; : > "$d/wt"
    for r in "$BT_REPO" ${GATE_WIN_ROOT:+"$GATE_WIN_ROOT"}; do
      [ -n "$r" ] || continue
      if git -c safe.directory='*' -C "$r" worktree list --porcelain > "$d/wt.raw" 2>/dev/null; then
        wt_ok=1; sed -n 's/^worktree //p' "$d/wt.raw" | sed 's|\\|/|g; s|/*$||; s|.*/||' >> "$d/wt"
      fi
    done
    if bt_class_on_ images && [ "$wt_ok" = 0 ]; then echo "lane images: SKIPPED — no worktree list could be read (an image is never litter by default)" >> "$d/notes"; fi
    : > "$d/cand"
    while IFS='|' read -r ref id cr sz; do
      case $ref in clippet*) ;; *) continue ;; esac
      repo=${ref%:*}; tag=${ref##*:}
      case $ref in
        clippet-t0-deps:*|clippet-t1-deps:*|clippet-check-*:*)
          bt_class_on_ tags || continue
          e=
          case $tag in *[0-9]-[0-9]*) e=${tag##*-}; case $e in *[!0-9]*|'') e= ;; esac ;; esac
          [ -n "$e" ] && [ "${#e}" -ge 9 ] || e=$(bt_epoch_ "$cr")
          [ -n "$e" ] || continue
          [ $(( now - e )) -ge "$age" ] || continue
          bt_unused_ "$ref" || continue
          echo "tag|$ref|$id|$sz" >> "$d/cand" ;;
        clippet-*-app:local|clippet-*-app:gpu)
          bt_class_on_ images && [ "$wt_ok" = 1 ] || continue
          lane=${repo#clippet-}; lane=${lane%-app}
          case $lane in *[!a-z0-9]*|''|app|dev|gate|never|pass|main) continue ;; esac
          grep -qx "clippet-$lane" "$d/wt" && continue
          bt_unused_ "$ref" || continue
          echo "img|$ref|$id|$sz" >> "$d/cand" ;;
      esac
    done < "$d/images"
    # An id another tag still holds frees nothing when this tag goes; two listed tags of one id count it once.
    awk -F'|' '
      FNR == 1 { f++ }
      f == 1 { listed[$2] = 1; next }
      f == 2 { if (!($1 in listed)) other[$2] = 1; next }
      { print $1 "|" $2 "|" ((($3 in other) || ($3 in seen)) ? "0B" : $4); seen[$3] = 1 }' "$d/cand" "$d/images" "$d/cand" \
      | while IFS='|' read -r c ref sz; do echo "$c|$ref|$(bt_bytes_ "$sz")"; done >> "$d/items"
  fi
  # -- 1 volumes ---------------------------------------------------------------------------------------------------------
  if bt_class_on_ volumes; then
    docker volume ls -q -f dangling=true 2>/dev/null | tr -d '\r' | grep -E '^[0-9a-f]{64}$' > "$d/vols" || true
    if [ -s "$d/vols" ]; then
      docker system df -v 2>/dev/null | tr -d '\r' \
        | awk '/^Local Volumes space usage:/ { f = 1; next } /space usage:/ { f = 0 } f && $1 ~ /^[0-9a-f]+$/ && NF >= 3 { print $1 "|" $NF }' > "$d/vsz" || : > "$d/vsz"
      while IFS= read -r v; do
        [ -n "$v" ] || continue
        sz=$(awk -F'|' -v v="$v" '$1 == v { print $2; exit }' "$d/vsz")
        echo "vol|$v|$(bt_bytes_ "${sz:-0B}")" >> "$d/items"
      done < "$d/vols"
    fi
  fi
  # -- 5 cache -----------------------------------------------------------------------------------------------------------
  if bt_class_on_ cache; then
    # `buildx du` IGNORES --filter until= (measured 2026-10-18: until=1h, 168h and 100000h all listed the same 117 records),
    # so the age is read here from LastUsedAt, which docker words as go-units' HumanDuration: "N days ago" up to 13 days,
    # then weeks, months, years. 168 h = 7 days or more. A record with no LastUsedAt is not counted.
    if docker buildx du --format '{{.ID}}|{{.Size}}|{{.Shared}}|{{.Reclaimable}}|{{.LastUsedAt}}' > "$d/cache" 2>/dev/null; then
      tr -d '\r' < "$d/cache" | awk -F'|' '
        function by(s,  n, u, m) { sub(/\*$/, "", s); if (!match(s, /^[0-9.]+/)) return 0
          n = substr(s, 1, RLENGTH); u = tolower(substr(s, RLENGTH + 1)); m = 1
          if (u == "kb") m = 1e3; else if (u == "mb") m = 1e6; else if (u == "gb") m = 1e9; else if (u == "tb") m = 1e12; return n * m }
        $4 != "true" { next }
        { w = $5; old = 0
          if (w ~ /^[0-9]+ days? ago$/) { split(w, p, " "); old = (p[1] + 0 >= 7) }
          else if (w ~ / (weeks?|months?|years?) ago$/) old = 1 }
        old { n++; if ($3 == "false") b += by($2) }
        END { printf "%d %.0f\n", n, b }' > "$d/cache.n"
      read -r cn cb < "$d/cache.n"
      if [ "${cn:-0}" -gt 0 ]; then echo "cache|build cache: $cn record(s) unused for 168 h|$cb" >> "$d/items"; fi
    else
      echo "build cache: SKIPPED — docker buildx du did not answer" >> "$d/notes"
    fi
  fi
}

# bt_print_ <dir> — the report: the class lines with their items, the notes, the closing line.
bt_print_() {
  d=$1
  for c in volumes images tags containers cache; do
    case $c in
      volumes) k=vol; label="anonymous dangling volumes" ;;
      images) k=img; label="lane images with no worktree" ;;
      tags) k=tag; label="stale harness tags (older than $BOX_TIDY_AGE)" ;;
      containers) k=ctr; label="stopped harness containers (older than $BOX_TIDY_AGE)" ;;
      cache) k=cache; label="build cache unused for 168 h (an estimate — Docker's prune decides; not in the total)" ;;
    esac
    if ! bt_class_on_ "$c"; then echo "box-tidy: $label: skipped (BOX_TIDY_CLASSES)"; continue; fi
    n=$(grep -c "^$k|" "$d/items" || true)
    b=$(awk -F'|' -v k="$k" '$1 == k { s += $3 } END { printf "%.0f\n", s + 0 }' "$d/items")
    echo "box-tidy: $label: $n, $(bt_human_ "$b")"
    grep "^$k|" "$d/items" | while IFS='|' read -r _ name by; do echo "  $name  ($(bt_human_ "$by"))"; done
  done
  sed 's/^/box-tidy: /' "$d/notes"
  bt_close_ "$d"
}
# The closing line counts what a removal is known to free: every class but the cache's estimate (class 5 above).
bt_close_() {
  n=$(grep -vc '^cache|' "$1/items" || true)
  if [ "$n" = 0 ]; then echo "box-tidy: clean"
  else echo "box-tidy: $n item(s), $(bt_human_ "$(awk -F'|' '$1 != "cache" { s += $3 } END { printf "%.0f\n", s + 0 }' "$1/items")") reclaimable"; fi
}
# bt_pruned_ <prune output> → the bytes the prune says it reclaimed (buildx: `Total:  1.476GB`; the classic CLI:
# `Total reclaimed space: 1.476GB`), or nothing when it printed no total.
bt_pruned_() {
  t=$(tr -d '\r' < "$1" | awk '/^Total reclaimed space:/ { v = $4 } /^Total:/ { v = $2 } END { if (v != "") print v }')
  [ -z "$t" ] || bt_bytes_ "$t"
}

# bt_inuse_ <class> <name> → 0 when the item is in use NOW (apply's re-check, before anything is removed).
bt_inuse_() {
  [ -n "$1" ] && [ -n "$2" ] || bt_die_ "bt_inuse_: empty class or name"
  case $1 in
    vol) [ -n "$(docker ps -a -q --filter "volume=$2" < /dev/null 2>/dev/null | tr -d '\r')" ] ;;
    img|tag) ! bt_unused_ "$2" ;;
    ctr) s=$(docker inspect -f '{{.State.Status}}' "$2" < /dev/null 2>/dev/null | tr -d '\r'); case $s in exited|created|dead) return 1 ;; esac; return 0 ;;
    cache) return 1 ;;
    *) return 0 ;;
  esac
}
# bt_remove_ <class> <name> — THE ONE place anything is removed. An empty selector dies; nothing here takes -f but the
# cache prune, which is filtered by age and never --all.
bt_remove_() {
  [ -n "${1:-}" ] || bt_die_ "bt_remove_: empty class — refused"
  [ -n "${2:-}" ] || bt_die_ "bt_remove_: empty name — refused"
  case $1 in
    vol) printf '%s\n' "$2" | grep -qE '^[0-9a-f]{64}$' || bt_die_ "bt_remove_: '$2' is not an anonymous volume — refused"
         docker volume rm "$2" >/dev/null ;;
    img|tag) case $2 in clippet*) ;; *) bt_die_ "bt_remove_: '$2' is not a clippet image — refused" ;; esac
         docker rmi "$2" >/dev/null ;;
    ctr) case $2 in clippet-e2e-*|clippet-t0-*|clippet-t1-*|clippet-check-*) ;; *) bt_die_ "bt_remove_: '$2' is not a harness container — refused" ;; esac
         docker rm "$2" >/dev/null ;;
    cache) docker builder prune -f --filter until=168h > "${W:?}/prune.out" ;;
    *) bt_die_ "bt_remove_: unknown class '$1' — refused" ;;
  esac
}

bt_main_() {
  verb=${1:-report}
  BOX_TIDY_AGE=${BOX_TIDY_AGE:-24h}
  BT_CLASSES=$(printf '%s' "${BOX_TIDY_CLASSES:-$BT_ALL_CLASSES}" | tr ',' ' ')
  for c in $BT_CLASSES; do case " $BT_ALL_CLASSES " in *" $c "*) ;; *) bt_die_ "BOX_TIDY_CLASSES names '$c' (the classes: $BT_ALL_CLASSES)" ;; esac; done
  BT_REPO=${BOX_TIDY_REPO:-}
  if [ -z "$BT_REPO" ]; then
    gd=$(cd "$(dirname "$BT_SELF")/.." 2>/dev/null && git rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || gd=
    [ -z "$gd" ] || BT_REPO=$(dirname "$gd")
  fi
  W=$(mktemp -d 2>/dev/null) || bt_die_ "no temp directory"
  trap 'rm -rf "$W"' EXIT
  case $verb in
    report)
      if ! docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then echo "box-tidy: docker not reachable — nothing read"; exit 0; fi
      ( bt_collect_ "$W" ) || { echo "box-tidy: the report could not be made (above) — nothing read"; exit 0; }
      bt_print_ "$W"; exit 0 ;;
    apply)
      docker version --format '{{.Server.Version}}' >/dev/null 2>&1 || bt_die_ "docker not reachable — nothing removed"
      bt_collect_ "$W"; bt_print_ "$W"
      [ -s "$W/items" ] || exit 0
      while IFS='|' read -r c n _; do
        if bt_inuse_ "$c" "$n"; then echo "box-tidy: REFUSED: $n is in use now — nothing removed"; exit 2; fi
      done < "$W/items"
      rn=0; rb=0; bad=0
      while IFS='|' read -r c n b; do
        echo "box-tidy: removing $n"
        if ! bt_remove_ "$c" "$n" < /dev/null; then echo "box-tidy: FAILED to remove $n (docker refused; above)"; bad=1; continue; fi
        if [ "$c" = cache ]; then
          # The prune's own total, never the estimate: Docker's rule decides what goes (class 5).
          b=$(bt_pruned_ "$W/prune.out")
          if [ -z "$b" ]; then echo "box-tidy: build cache: the prune printed no total — counted as nothing"; b=0; fi
          echo "box-tidy: build cache: the prune reclaimed $(bt_human_ "$b")"
          [ "$b" -gt 0 ] || continue
        fi
        rn=$((rn + 1)); rb=$((rb + b))
      done < "$W/items"
      echo "box-tidy: removed $rn item(s), $(bt_human_ "$rb")"
      mkdir -p "$W/after"; bt_collect_ "$W/after"; bt_close_ "$W/after"
      [ "$bad" = 0 ] || exit 2
      exit 0 ;;
    _remove)   # the self-test's way into the guard; refused otherwise
      [ "${BOX_TIDY_SELFTEST_INNER:-0}" = 1 ] || bt_die_ "_remove is the self-test's"
      bt_remove_ "${2:-}" "${3:-}"; exit $? ;;
    --selftest|selftest) bt_selftest_; exit $? ;;
    *) echo "usage: bash scripts/box-tidy.sh [report|apply|--selftest]"; exit 2 ;;
  esac
}

# ---- the self-test ------------------------------------------------------------------------------------------------------
# A stub `docker` (POSIX sh) serves planted listings from $ST and logs every call to $ST/calls; its rm/rmi/volume rm/builder
# prune remove the planted line, so a re-report sees what apply left. Scratch git repos give the worktree lists.
bt_selftest_() {
  sp=0; sf=0
  ok_() { if [ "$1" = "$2" ]; then sp=$((sp + 1)); else sf=$((sf + 1)); echo "  box-tidy self-test FAIL: $3"; echo "    got:  $1"; echo "    want: $2"; fi; }
  T=$(mktemp -d 2>/dev/null) || { echo "box-tidy self-test: no temp directory"; return 1; }
  # Git Bash under MSYS_NO_PATHCONV=1 (check.sh exports it): git.exe cannot read /tmp/…, so the mixed form C:/… for both.
  TB=$T/bin   # PATH keeps the POSIX form (a C: would split it)
  if command -v cygpath >/dev/null 2>&1; then T=$(cygpath -m "$T"); fi
  ST=$T/st; mkdir -p "$ST/anc" "$T/bin"
  cat > "$T/bin/docker" <<'STUB'
#!/bin/sh
ST=${BT_STUB:?}
echo "$*" >> "$ST/calls"
[ ! -f "$ST/down" ] || exit 1
del() { grep -v "$2" "$ST/$1" > "$ST/$1.n" 2>/dev/null; mv "$ST/$1.n" "$ST/$1"; }
case $1 in
  version) echo 29.0.1 ;;
  volume)
    case $2 in
      ls) cat "$ST/vols" ;;
      rm) del vols "^$3\$"; echo "$3" ;;
    esac ;;
  system) printf 'Images space usage:\n\nREPOSITORY TAG\n\nLocal Volumes space usage:\n\nVOLUME NAME LINKS SIZE\n'; cat "$ST/dfvols"; printf '\nBuild cache usage: 0B\n' ;;
  images) cat "$ST/images" ;;
  ps)
    case "$*" in
      *ancestor=*) all="$*"; a=${all##*ancestor=}; f=$ST/anc/$(printf '%s' "$a" | tr '/:' '__')
        n=$(cat "$f.n" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > "$f.n"
        if [ -f "$f" ]; then cat "$f"; elif [ -f "$f.after1" ] && [ "$n" -ge 2 ]; then cat "$f.after1"; fi ;;
      *volume=*) : ;;
      *) cat "$ST/ps" ;;
    esac ;;
  inspect) n=$4; awk -F'|' -v n="$n" '$1 == n { print $2 }' "$ST/ps" ;;
  rmi) a=$2; del images "^$a|"; echo "Untagged: $a" ;;
  rm) del ps "^$2|"; echo "$2" ;;
  buildx) cat "$ST/cache" ;;
  builder) grep -vE "^r[15][|]" "$ST/cache" > "$ST/cache.n"; mv "$ST/cache.n" "$ST/cache"   # Docker's own rule: not all the report aged
           printf 'ID\tRECLAIMABLE\tSIZE\tLAST ACCESSED\nr1\ttrue\t1.476GB\t4 weeks ago\n\n%s\n' "$(cat "$ST/prunetotal" 2>/dev/null || printf 'Total:\t1.476GB')" ;;
esac
exit 0
STUB
  chmod +x "$T/bin/docker"
  now=$(date +%s); old=$((now - 3 * 86400))
  OLDC="2020-01-01 00:00:00 +0000 UTC"; NOWC=$(date -u '+%Y-%m-%d %H:%M:%S +0000 UTC')
  H1=1111111111111111111111111111111111111111111111111111111111111111
  H2=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  # Two scratch repos: this repo (worktree clippet-haswt) and the "Windows" one a WSL gate came from (clippet-winlane).
  git_() { git -c user.name=selftest -c user.email=selftest@localhost -c init.defaultBranch=main "$@" >/dev/null 2>&1; }
  for r in clippet:haswt win/clippet:winlane; do
    rd=$T/${r%%:*}; mkdir -p "$rd"; git_ -C "$rd" init -q && git_ -C "$rd" commit -q --allow-empty -m init \
      && git_ -C "$rd" worktree add -q "$(dirname "$rd")/clippet-${r##*:}" -b "${r##*:}" || { echo "box-tidy self-test: git could not make the scratch repos"; rm -rf "$T"; return 1; }
  done
  plant_() {
    rm -rf "$ST"; mkdir -p "$ST/anc"; : > "$ST/calls"
    # 1 volumes: one anonymous dangling (litter); a NAMED dangling cache volume and a project's dangling data volume (kept).
    printf '%s\n' "$H1" clippet-gobuild clippet-gate_pg_data > "$ST/vols"
    printf '%s\n' "$H1 0 1.5GB" "$H2 1 2GB" "clippet-gobuild 0 9GB" > "$ST/dfvols"
    # 2 images + 3 tags (+ every excluded kind). clippet-busy-app is run by a container; clippet-t1-deps:555 too.
    { echo "clippet-oldlane-app:local|sha1|$OLDC|400MB"
      echo "clippet-oldlane-app:gpu|sha2|$OLDC|5GB"
      echo "clippet-haswt-app:local|sha3|$OLDC|400MB"
      echo "clippet-winlane-app:gpu|sha4|$OLDC|5GB"
      echo "clippet-busy-app:local|sha5|$OLDC|400MB"
      echo "clippet-app:local|sha6|$OLDC|450MB"; echo "clippet-app:gpu|sha7|$OLDC|5.4GB"
      echo "clippet-dev-app:gpu|sha8|$OLDC|5.4GB"; echo "clippet-gate-app:local|sha9|$OLDC|450MB"
      echo "clippet-never-app:local|sha10|$OLDC|450MB"; echo "clippet-pass-app:local|sha11|$OLDC|450MB"
      echo "clippet-x-y-app:local|sha12|$OLDC|450MB"
      echo "clippet-t0-deps:123-$old|sha20|$NOWC|1GB"
      echo "clippet-t1-deps:456-$old|sha21|$NOWC|1GB"
      echo "clippet-check-src:789-$old|sha22|$NOWC|1.2GB"
      echo "clippet-t0-deps:999-$now|sha23|$OLDC|1GB"
      echo "clippet-t1-deps:555-$old|sha24|$NOWC|1GB"
      echo "clippet-check-src:old|sha25|$OLDC|1GB"
      echo "clippet-gotest:ffmpeg|sha26|$OLDC|780MB"; echo "clippet-race:ffmpeg|sha27|$OLDC|1GB"
      echo "clippet-visual:1.48.0|sha28|$OLDC|3GB"; echo "clippet-rust:desktop|sha29|$OLDC|1.7GB"
      echo "clippet-auto:main-1|sha30|$OLDC|18MB"
      echo "otherproj-app:local|sha40|$OLDC|1GB"; echo "immich-server:release|sha41|$OLDC|2GB"
      echo "notclippet-t0-deps:1-$old|sha42|$OLDC|1GB"
      echo "keepme:tag|sha1|$OLDC|400MB"; } > "$ST/images"
    echo deadbeef > "$ST/anc/clippet-busy-app_local"; echo deadbeef > "$ST/anc/clippet-t1-deps_555-$old"
    # 4 containers: name|state|created|size|compose service|oneoff
    { echo "clippet-e2e-old|exited|$OLDC|1MB (virtual 1GB)||"
      echo "clippet-t0-fmt-1|created|$OLDC|0B (virtual 1GB)||"
      echo "clippet-check-go-2|exited|$OLDC|2MB (virtual 1GB)||"
      echo "clippet-t1-go-3|dead|$OLDC|3MB (virtual 1GB)||"
      echo "clippet-e2e-scope-9|exited|$OLDC|4MB (virtual 1GB)|app|True"
      echo "clippet-e2e-run|running|$OLDC|1MB||"
      echo "clippet-e2e-pause|paused|$OLDC|1MB||"
      echo "clippet-e2e-fresh|exited|$NOWC|1MB||"
      echo "clippet-e2e-svc-1|exited|$OLDC|1MB|app|False"
      echo "clippet-gate-s3-1|exited|$OLDC|1MB|s3|False"
      echo "clippet-app-1|exited|$OLDC|1MB|app|False"
      echo "immich_postgres|exited|$OLDC|1MB||"
      echo "chatterbox-e2e-x|exited|$OLDC|1MB||"
      echo "xclippet-e2e-a|exited|$OLDC|1MB||"
      echo "clippet-manual-scratch|exited|$OLDC|1MB||"; } > "$ST/ps"
    # 5 cache: two reclaimable records (one shared with an image), one not reclaimable.
    # 5 cache: id|size|shared|reclaimable|last used. Old and reclaimable: r1 (private), r2 (shared), r5, r6, r7 (shared);
    # kept: r3 not reclaimable, r4 used 6 days ago, r8 an hour ago, r9 never used.
    printf '%s\n' "r1|1.5GB|false|true|4 weeks ago" "r2|2GB|true|true|7 days ago" "r3|9GB|false|false|5 weeks ago" \
      "r4|8GB|false|true|6 days ago" "r5|0B|false|true|3 months ago" "r6|0B|false|true|13 days ago" "r7|1GB*|true|true|2 years ago" \
      "r8|7GB|false|true|About an hour ago" "r9|6GB|false|true|" > "$ST/cache"
  }
  run_() { PATH="$TB:$PATH" BT_STUB=$ST BOX_TIDY_REPO=$T/clippet GATE_WIN_ROOT=$T/win/clippet sh "$BT_SELF" "$@"; }
  items_() { sed -n 's/^  \(.*\)  ([^()]*)$/\1/p' "$1" | LC_ALL=C sort | tr '\n' ';'; }
  removes_() { grep -E '^(volume rm|rmi|rm|builder prune|system prune|container prune|image prune|volume prune)( |$)' "$ST/calls" | LC_ALL=C sort | tr '\n' ';'; }
  WANT_ITEMS=$(printf '%s\n' "$H1" clippet-oldlane-app:local clippet-oldlane-app:gpu "clippet-t0-deps:123-$old" "clippet-t1-deps:456-$old" \
    "clippet-check-src:789-$old" clippet-check-src:old clippet-e2e-old clippet-t0-fmt-1 clippet-check-go-2 clippet-t1-go-3 clippet-e2e-scope-9 \
    "build cache: 5 record(s) unused for 168 h" | LC_ALL=C sort | tr '\n' ';')
  WANT_RM=$(printf '%s\n' "volume rm $H1" "rmi clippet-oldlane-app:local" "rmi clippet-oldlane-app:gpu" "rmi clippet-t0-deps:123-$old" \
    "rmi clippet-t1-deps:456-$old" "rmi clippet-check-src:789-$old" "rmi clippet-check-src:old" "rm clippet-e2e-old" "rm clippet-t0-fmt-1" \
    "rm clippet-check-go-2" "rm clippet-t1-go-3" "rm clippet-e2e-scope-9" "builder prune -f --filter until=168h" | LC_ALL=C sort | tr '\n' ';')

  # -- report ------------------------------------------------------------------------------------------------------------
  plant_; rc=0; run_ report > "$T/r1" 2>&1 || rc=$?
  ok_ "$rc" 0 "report exits 0"
  ok_ "$(items_ "$T/r1")" "$WANT_ITEMS" "report lists exactly the five classes' items (one of every excluded kind planted: a named dangling volume, a project's dangling data volume, lane images whose worktree exists here and in GATE_WIN_ROOT, the app/dev/gate/never/pass images, a two-word lane, an image in use, a fresh t0 tag, a tag in use, the long-lived harness images, non-clippet images and containers, a compose service container, running and paused probes, a fresh probe)"
  ok_ "$(grep -c '^  build cache: 5 record(s) unused for 168 h  (1.50 GB)$' "$T/r1")" 1 "the cache is ONE item: its reclaimable records counted, the bytes only the unshared ones"
  ok_ "$(grep -c "^box-tidy: build cache unused for 168 h (an estimate — Docker's prune decides; not in the total): 1, 1.50 GB$" "$T/r1")" 1 "the cache line says it is an estimate"
  ok_ "$(grep '^box-tidy: anonymous dangling volumes: ' "$T/r1")" "box-tidy: anonymous dangling volumes: 1, 1.50 GB" "the volumes line: count and the size from docker system df -v"
  ok_ "$(grep '^box-tidy: lane images with no worktree: ' "$T/r1")" "box-tidy: lane images with no worktree: 2, 5.00 GB" "the lane images line: an id another tag holds (keepme:tag) frees nothing"
  ok_ "$(tail -1 "$T/r1")" "box-tidy: 12 item(s), 10.71 GB reclaimable" "the closing line is last: the count and the bytes, without the cache's estimate"
  ok_ "$(removes_)" "" "report removes nothing (no rm, rmi, volume rm, prune of any kind)"
  ok_ "$(grep -cE 'prune .*(-a|--all|--volumes)|system prune' "$ST/calls")" 0 "no -a, no --volumes, no system prune is ever called"
  # -- apply -------------------------------------------------------------------------------------------------------------
  plant_; rc=0; run_ apply > "$T/a1" 2>&1 || rc=$?
  ok_ "$rc" 0 "apply exits 0"
  ok_ "$(removes_)" "$WANT_RM" "apply calls remove for exactly the listed names and nothing else"
  ok_ "$(grep -c '^box-tidy: removing ' "$T/a1")" 13 "apply prints each name before removing it"
  ok_ "$(grep -c '^box-tidy: build cache: the prune reclaimed 1.48 GB$' "$T/a1")" 1 "apply prints what the prune's own total line says it reclaimed (1.476GB), not the 1.50 GB estimate"
  ok_ "$(tail -2 "$T/a1" | head -1)" "box-tidy: removed 13 item(s), 12.19 GB" "apply's count and bytes: 10.71 GB of items + the prune's own 1.476 GB"
  ok_ "$(tail -1 "$T/a1")" "box-tidy: clean" "apply ends with a re-report's closing line: clean — the records Docker's rule kept (r2, r6, r7) are an estimate, not litter"
  ok_ "$(grep -c '^clippet-e2e-run|\|^clippet-manual-scratch|\|^immich_postgres|\|^clippet-app-1|' "$ST/ps"):$(grep -c '^otherproj-app\|^clippet-app:local\|^clippet-haswt-app' "$ST/images"):$(grep -c clippet-gobuild "$ST/vols")" "4:3:1" "every excluded kind is still there after apply"
  # -- the prune that takes nothing (a Windows GPU box's 2026-10-20 case): no bytes claimed, the item not counted --------------------------
  plant_; : > "$ST/vols"; : > "$ST/images"; : > "$ST/ps"; : > "$ST/dfvols"; printf 'Total:\t0B' > "$ST/prunetotal"
  rc=0; run_ report > "$T/r7" 2>&1 || rc=$?
  ok_ "$rc:$(tail -1 "$T/r7")" "0:box-tidy: clean" "a box with only the cache's estimate reports clean (the estimate is listed above it)"
  rc=0; run_ apply > "$T/a5" 2>&1 || rc=$?
  ok_ "$rc:$(grep -c '^box-tidy: build cache: the prune reclaimed 0 B$' "$T/a5"):$(tail -2 "$T/a5" | head -1)" "0:1:box-tidy: removed 0 item(s), 0 B" "a prune whose own total is 0B claims nothing"
  ok_ "$(bt_pruned_ "$(printf 'Total reclaimed space: 2.5GB\n' > "$T/pt"; echo "$T/pt")"):$(printf 'nothing\n' > "$T/pt"; bt_pruned_ "$T/pt")" "2500000000:" "the classic CLI's total line parses; no total line is nothing"
  # -- the in-use refusal: a tag gains a container between the listing and its turn ----------------------------------------
  plant_; echo cafe > "$ST/anc/clippet-t0-deps_123-$old.after1"; rc=0; run_ apply > "$T/a2" 2>&1 || rc=$?
  ok_ "$rc" 2 "apply exits 2 when a listed item is in use when it gets to it"
  ok_ "$(removes_)" "" "…and removes NOTHING (every item is re-checked before the first removal)"
  ok_ "$(grep -c "^box-tidy: REFUSED: clippet-t0-deps:123-$old is in use now" "$T/a2")" 1 "…and names the item"
  # -- the empty-list guard ----------------------------------------------------------------------------------------------
  plant_; : > "$ST/vols"; : > "$ST/images"; : > "$ST/ps"; : > "$ST/cache"; : > "$ST/dfvols"
  rc=0; run_ report > "$T/r2" 2>&1 || rc=$?
  ok_ "$rc:$(tail -1 "$T/r2")" "0:box-tidy: clean" "an empty box reports clean"
  rc=0; run_ apply > "$T/a3" 2>&1 || rc=$?
  ok_ "$rc:$(tail -1 "$T/a3"):$(removes_)" "0:box-tidy: clean:" "apply on an empty list removes nothing and says clean"
  for a in "vol/" "/$H1" "ctr/" "img/" "tag/" "cache/" "vol/clippet-gobuild" "img/immich-server:release" "tag/otherproj-app:local" \
           "ctr/clippet-app-1" "ctr/immich_postgres" "zz/x"; do
    : > "$ST/calls"; rc=0; PATH="$TB:$PATH" BT_STUB=$ST BOX_TIDY_SELFTEST_INNER=1 sh "$BT_SELF" _remove "${a%%/*}" "${a#*/}" > /dev/null 2>&1 || rc=$?
    ok_ "$([ "$rc" != 0 ] && echo refused):$(removes_)" "refused:" "the removal guard refuses an empty selector or a name outside the allow-list ('$a')"
  done
  rc=0; PATH="$TB:$PATH" BT_STUB=$ST sh "$BT_SELF" _remove vol "$H1" > /dev/null 2>&1 || rc=$?
  ok_ "$([ "$rc" != 0 ] && echo refused)" refused "the _remove door is the self-test's only"
  # -- the knobs and the edges -------------------------------------------------------------------------------------------
  plant_; rc=0; BOX_TIDY_AGE=0s run_ report > "$T/r3" 2>&1 || rc=$?
  ok_ "$(grep -c "^  clippet-t0-deps:999-$now \|^  clippet-e2e-fresh " "$T/r3")" 2 "BOX_TIDY_AGE=0s lists a fresh tag and a fresh probe (the live proof's knob)"
  plant_; rc=0; BOX_TIDY_CLASSES="volumes,containers" run_ report > "$T/r4" 2>&1 || rc=$?
  ok_ "$(grep -c ': skipped (BOX_TIDY_CLASSES)$' "$T/r4"):$(tail -1 "$T/r4")" "3:box-tidy: 6 item(s), 1.51 GB reclaimable" "BOX_TIDY_CLASSES limits the classes; the others say skipped"
  plant_; rc=0; PATH="$TB:$PATH" BT_STUB=$ST BOX_TIDY_REPO=$T/nowhere GATE_WIN_ROOT= sh "$BT_SELF" report > "$T/r5" 2>&1 || rc=$?
  ok_ "$(grep -c 'app:\(local\|gpu\)  (' "$T/r5"):$(grep -c 'lane images: SKIPPED' "$T/r5")" "0:1" "no worktree list → the lane-image class is SKIPPED, never everything"
  plant_; touch "$ST/down"; rc=0; run_ report > "$T/r6" 2>&1 || rc=$?
  ok_ "$rc:$(tail -1 "$T/r6")" "0:box-tidy: docker not reachable — nothing read" "report exits 0 with no Docker"
  rc=0; run_ apply > "$T/a4" 2>&1 || rc=$?
  ok_ "$([ "$rc" != 0 ] && echo failed)" failed "apply with no Docker fails"
  ok_ "$(bt_bytes_ 12.3kB):$(bt_bytes_ '0B (virtual 1GB)'):$(bt_bytes_ '1.415GB*'):$(bt_bytes_ 32B)" "12300:0:1415000000:32" "docker's size strings to bytes"
  ok_ "$(bt_secs_ 24h):$(bt_secs_ 0s):$(bt_secs_ 90m):$(bt_secs_ 2d):$(bt_secs_ 3600):$(bt_secs_ x1)" "86400:0:5400:172800:3600:" "the age knob's durations"
  rm -rf "$T"
  echo "box-tidy self-test: $sp passed, $sf failed"
  [ "$sf" = 0 ]
}

bt_main_ "$@"
