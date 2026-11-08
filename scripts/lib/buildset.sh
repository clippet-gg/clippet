#!/usr/bin/env bash
# buildset.sh — the harness builds the SOURCE UNDER TEST, never the published image (lane image1).
#
# compose.yaml pulls ghcr.io/clippet-gg/clippet; compose.build.yaml is the overlay that builds ./Dockerfile
# from this checkout. Every stack the harness makes (scripts/e2e.sh's step 1, scripts/t1.sh's lane stack,
# the gate stack of scripts/lib/gatestack.sh) must carry the overlay, and a lane .env with no COMPOSE_FILE
# line must too: so the overlay is the HARNESS's default, added here, never something an operator's .env has
# to remember.
#
#   . scripts/lib/buildset.sh; buildset_export_ [env file]   # export COMPOSE_FILE with compose.build.yaml in it
#   bash scripts/lib/buildset.sh selftest                     # the insertion rule, no Docker
#
# The file set is the shell's COMPOSE_FILE, else the env file's COMPOSE_FILE line (default `.env`), else
# compose.yaml. compose.build.yaml goes right after compose.yaml, so a later overlay (compose.gpu.yaml: its
# target and its tag) still wins over it. A set that already names it is left as it is. The result is joined
# with `:` and COMPOSE_PATH_SEPARATOR=: is exported beside it, so the same value reads the same on every
# platform (the names here are relative, so no drive letter is ever split). Only the one line is read from the
# env file, and nothing of the file is printed.

# buildset_files_ <file set> → the set with compose.build.yaml, `:`-joined
buildset_files_() {
  local in=${1:-} f out="" placed=0
  in=$(printf '%s' "$in" | tr ';' ':')
  [ -n "$in" ] || in=compose.yaml
  case ":$in:" in *:compose.build.yaml:*) printf '%s' "$in" | sed 's/^:*//; s/:*$//; s/::*/:/g'; return 0 ;; esac
  case ":$in:" in *:compose.yaml:*) ;; *) in="compose.yaml:$in" ;; esac
  local IFS=:
  for f in $in; do
    [ -n "$f" ] || continue
    out="${out:+$out:}$f"
    if [ "$f" = compose.yaml ] && [ "$placed" = 0 ]; then out="$out:compose.build.yaml"; placed=1; fi
  done
  printf '%s' "$out"
}

# buildset_export_ [env file] — export COMPOSE_FILE (and COMPOSE_PATH_SEPARATOR=:) for this shell and its children
buildset_export_() {
  local envf=${1:-.env} cur
  cur=${COMPOSE_FILE:-$(sed -n 's/^COMPOSE_FILE=//p' "$envf" 2>/dev/null | tail -1 | tr -d '\r"')}
  COMPOSE_FILE=$(buildset_files_ "$cur")
  COMPOSE_PATH_SEPARATOR=:
  export COMPOSE_FILE COMPOSE_PATH_SEPARATOR
}

buildset_selftest_() {
  local n=0 bad=0 t
  ok_() { n=$((n + 1)); [ "$1" = "$2" ] || { bad=$((bad + 1)); echo "[buildset selftest] FAIL: $3 (got '$1', want '$2')"; }; }
  ok_ "$(buildset_files_ '')" compose.yaml:compose.build.yaml "an unset set is compose.yaml plus the overlay"
  ok_ "$(buildset_files_ compose.yaml)" compose.yaml:compose.build.yaml "compose.yaml alone gains the overlay"
  ok_ "$(buildset_files_ compose.yaml:compose.gpu.yaml:compose.e2e-bucket.yaml)" compose.yaml:compose.build.yaml:compose.gpu.yaml:compose.e2e-bucket.yaml "the overlay sits before the gpu flavour, so its target and tag still win"
  ok_ "$(buildset_files_ 'compose.yaml;compose.fs.yaml')" compose.yaml:compose.build.yaml:compose.fs.yaml "a ';' set is read and joined with ':'"
  ok_ "$(buildset_files_ compose.yaml:compose.build.yaml:compose.nobucket.yaml)" compose.yaml:compose.build.yaml:compose.nobucket.yaml "a set that names it is left as it is"
  ok_ "$(buildset_files_ compose.fs.yaml)" compose.yaml:compose.build.yaml:compose.fs.yaml "a set without compose.yaml gets it first"
  t=$(mktemp -d)
  printf 'A=1\nCOMPOSE_FILE=compose.yaml:compose.tunnel.yaml\n' > "$t/env"
  ok_ "$(env -u COMPOSE_FILE bash -c ". scripts/lib/buildset.sh; buildset_export_ '$t/env'; printf '%s|%s' \"\$COMPOSE_FILE\" \"\$COMPOSE_PATH_SEPARATOR\"")" "compose.yaml:compose.build.yaml:compose.tunnel.yaml|:" "the env file's line is read and exported"
  printf 'A=1\n' > "$t/env"
  ok_ "$(env -u COMPOSE_FILE bash -c ". scripts/lib/buildset.sh; buildset_export_ '$t/env'; printf '%s' \"\$COMPOSE_FILE\"")" compose.yaml:compose.build.yaml "an env file with no COMPOSE_FILE line (a lane .env) still builds"
  ok_ "$(COMPOSE_FILE=compose.yaml:compose.fs.yaml bash -c ". scripts/lib/buildset.sh; buildset_export_ '$t/env'; printf '%s' \"\$COMPOSE_FILE\"")" compose.yaml:compose.build.yaml:compose.fs.yaml "the shell's COMPOSE_FILE wins over the env file"
  rm -rf "$t"
  if [ "$bad" = 0 ]; then echo "[buildset selftest] PASS ($n checks: the overlay after compose.yaml and before every later overlay, an unset or lane set, ';' read, idempotent, the shell over the env file)"; return 0; fi
  echo "[buildset selftest] FAILED ($bad of $n)"; return 1
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 2
  case ${1:-} in
    selftest) buildset_selftest_ ;;
    *) echo "usage: bash scripts/lib/buildset.sh selftest" >&2; exit 2 ;;
  esac
fi
