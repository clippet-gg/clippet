#!/usr/bin/env sh
# fsdefault.sh — the one-container flavour's DESKTOP DEFAULT, pinned
# (ROADMAP #42 (a), DESIGN.md §20 erratum 295, DEPLOY.md §32.1).
#
# `compose.fs.yaml` used to REQUIRE STORAGE_DIR_HOST (`${STORAGE_DIR_HOST:?…}`).
# It now defaults to the person's own Videos folder, spelled from the variables
# a shell already exports, because compose interpolation cannot expand `~`:
#
#   ${STORAGE_DIR_HOST:-${HOME:-${USERPROFILE}}/Videos/clippet}
#
# Nothing but `docker compose config` can tell you what that resolves to, and
# the answer differs per shell (Git Bash exports HOME, PowerShell does not) —
# which is exactly the kind of fact that rots silently. This is a check.sh-style
# stage that asks compose itself, with a throwaway env-file, so the repository's
# own `.env` decides nothing.
#
# It also pins the spelling that does NOT work, because that one is a trap a
# future reader will walk back into: a nested `${USERPROFILE:?…}` guard is
# evaluated EAGERLY by Compose v2 — it fires whenever USERPROFILE is unset, even
# when HOME already supplied the value — so it would refuse on every Linux and
# macOS machine. Case 3 plants exactly that spelling in a throwaway file and
# asserts it fails where the shipped one resolves.
#
#   sh scripts/fsdefault.sh
#
# It starts no container, touches no stack and writes nothing outside a temp
# directory. Run it alone; it is deliberately NOT part of check.sh yet (the lane
# that added it does not own that file).
#
# A note on the fixture values below: they are asserted by the TOKEN in them
# (`fixture-home`, `fixture-profile`) and by the tail of the path, never by the
# whole string. On Git Bash the MSYS runtime rewrites a path-shaped HOME into a
# Windows path before docker.exe is handed it (`/fixture-home` →
# `C:\Program Files\Git\fixture-home`), which is a real thing operators meet and
# not something this stage should pretend away — what it is testing is WHICH
# variable supplied the value, and that survives the rewrite intact.
set -eu
cd "$(dirname "$0")/.."

FD_T=$(mktemp -d)
trap 'rm -rf "$FD_T"' EXIT
: > "$FD_T/empty.env"
printf 'STORAGE_DIR_HOST=/srv/clippet-media\n' > "$FD_T/named.env"

# fd_bind <output> — the HOST side of the bind mounted at /media.
fd_bind() {
  awk '/^[[:space:]]*source: /{s=$0; sub(/^[[:space:]]*source: /,"",s); gsub(/^"|"$/,"",s)}
       /^[[:space:]]*target: \/media$/{print s}' "$1"
}
# fd_env <output> <NAME> — one row of the app service's environment block.
fd_env() {
  sed -n "s/^      $2: //p" "$1" | sed 's/^"//; s/"$//' | head -1
}
fd_fail() { echo "  $1"; echo "  --- what compose said ---"; sed 's/^/  /' "$FD_T/out"; exit 1; }
# fd_has <string> <token> <what> — a token must appear in the resolved path.
fd_has() {
  case $1 in
    *"$2"*) ;;
    *) fd_fail "$3: '$1' does not carry '$2'" ;;
  esac
}
fd_lacks() {
  case $1 in
    *"$2"*) fd_fail "$3: '$1' carries '$2' and must not" ;;
  esac
}
fd_tail() {
  case $1 in
    */Videos/clippet) ;;
    *) fd_fail "$2: '$1' does not end in /Videos/clippet" ;;
  esac
}
echo "[fsdefault] compose $(docker compose version --short 2>/dev/null || echo '(version unknown)')"

# --- 1. HOME supplies it (macOS, Linux, Git Bash) ---------------------------
env -u USERPROFILE -u STORAGE_DIR_HOST HOME=/fixture-home \
  sh -c 'docker compose -f compose.yaml -f compose.fs.yaml --env-file "$1" config 2>&1' _ "$FD_T/empty.env" > "$FD_T/out" || true
FD_V=$(fd_bind "$FD_T/out")
fd_tail "$FD_V" "HOME must supply the default"
fd_has "$FD_V" fixture-home "HOME must supply the default"
[ "$(fd_env "$FD_T/out" STORAGE_DIR_HOST)" = "$FD_V" ] \
  || fd_fail "the STORAGE_DIR_HOST row must carry the same resolved path the bind does (admin doctor's store row reads it)"
[ -z "$(fd_env "$FD_T/out" STORAGE_DIR_HOST_SET)" ] \
  || fd_fail "STORAGE_DIR_HOST_SET must be EMPTY when the operator set nothing"
echo "[fsdefault]   HOME        -> $FD_V"

# --- 2. USERPROFILE supplies it (PowerShell, cmd), and HOME beats it --------
env -u HOME -u STORAGE_DIR_HOST USERPROFILE=/fixture-profile \
  sh -c 'docker compose -f compose.yaml -f compose.fs.yaml --env-file "$1" config 2>&1' _ "$FD_T/empty.env" > "$FD_T/out" || true
FD_V=$(fd_bind "$FD_T/out")
fd_tail "$FD_V" "USERPROFILE must supply the default when HOME is unset"
fd_has "$FD_V" fixture-profile "USERPROFILE must supply the default when HOME is unset"
echo "[fsdefault]   USERPROFILE -> $FD_V"

env -u STORAGE_DIR_HOST HOME=/fixture-home USERPROFILE=/fixture-profile \
  sh -c 'docker compose -f compose.yaml -f compose.fs.yaml --env-file "$1" config 2>&1' _ "$FD_T/empty.env" > "$FD_T/out" || true
FD_V=$(fd_bind "$FD_T/out")
fd_has "$FD_V" fixture-home "with both set, HOME wins"
fd_lacks "$FD_V" fixture-profile "with both set, HOME wins"
echo "[fsdefault]   both        -> HOME wins"

# --- 3. the nested `:?` spelling, and why it is not the one shipped ---------
# A guard that fires when NEITHER variable exists is the obvious way to keep the
# default off the filesystem root. Compose does not nest `:?`: it evaluates the
# inner expression whatever the outer branch resolved to. Planted here so the
# next person to reach for it reads the measurement instead of repeating it.
sed 's|${HOME:-${USERPROFILE}}|${HOME:-${USERPROFILE:?no home directory}}|' compose.fs.yaml > "$FD_T/eager.yaml"
grep -q 'USERPROFILE:?' "$FD_T/eager.yaml" || fd_fail "the planted :? spelling did not take (the volume line has moved)"
if env -u USERPROFILE -u STORAGE_DIR_HOST HOME=/fixture-home \
     sh -c 'docker compose -f compose.yaml -f "$2" --env-file "$1" config' _ "$FD_T/empty.env" "$FD_T/eager.yaml" > "$FD_T/out" 2>&1; then
  fd_fail "the nested :? spelling resolved with HOME set: Compose has changed and the shipped spelling could be tightened (erratum 295)"
fi
grep -q 'USERPROFILE' "$FD_T/out" \
  || fd_fail "the nested :? spelling failed for some other reason than the eager guard"
echo "[fsdefault]   nested :?   -> fires even with HOME set, so it is not shipped"

# What the shipped spelling does in that same environment: it resolves, and the
# WARN about it is `admin doctor`'s rather than compose's.
env -u HOME -u USERPROFILE -u STORAGE_DIR_HOST \
  sh -c 'docker compose -f compose.yaml -f compose.fs.yaml --env-file "$1" config 2>&1' _ "$FD_T/empty.env" > "$FD_T/out" || true
[ "$(fd_bind "$FD_T/out")" = "/Videos/clippet" ] \
  || fd_fail "with neither variable set the default must resolve to /Videos/clippet (which admin doctor WARNs about), got '$(fd_bind "$FD_T/out")'"
echo "[fsdefault]   neither     -> /Videos/clippet (admin doctor's store row WARNs)"

# --- 4. an operator's own path still wins, un-mangled -----------------------
# From the ENV FILE, which is where DEPLOY §32.1 puts it: compose reads that
# file itself, so MSYS never sees the value (an EXPORTED one on Git Bash is
# rewritten to `C:/Program Files/Git/srv/…`, the quirk §32.3 already warns about
# for the migrate commands).
env -u HOME -u USERPROFILE -u STORAGE_DIR_HOST \
  sh -c 'docker compose -f compose.yaml -f compose.fs.yaml --env-file "$1" config 2>&1' _ "$FD_T/named.env" > "$FD_T/out" || true
[ "$(fd_bind "$FD_T/out")" = "/srv/clippet-media" ] \
  || fd_fail "STORAGE_DIR_HOST from the env file must win, got '$(fd_bind "$FD_T/out")'"
[ "$(fd_env "$FD_T/out" STORAGE_DIR_HOST_SET)" = "1" ] \
  || fd_fail "STORAGE_DIR_HOST_SET must be 1 when the operator named the path"
echo "[fsdefault]   STORAGE_DIR_HOST -> $(fd_bind "$FD_T/out")"

# --- 5. and on THIS box, in whichever shell is running this -----------------
env -u STORAGE_DIR_HOST \
  sh -c 'docker compose -f compose.yaml -f compose.fs.yaml --env-file "$1" config 2>&1' _ "$FD_T/empty.env" > "$FD_T/out" || true
FD_V=$(fd_bind "$FD_T/out")
fd_tail "$FD_V" "on this machine the default"
echo "[fsdefault]   this shell  -> $FD_V"

# --- 6. the bucket flavour is untouched -------------------------------------
# compose.yaml alone must not grow a /media bind or either informational row:
# they belong to the override, and an operator on `s3` has no host directory.
docker compose -f compose.yaml --env-file "$FD_T/empty.env" config > "$FD_T/out" 2>&1 \
  || fd_fail "compose.yaml alone no longer resolves"
[ -z "$(fd_bind "$FD_T/out")" ] || fd_fail "compose.yaml alone must bind nothing at /media"
[ -z "$(fd_env "$FD_T/out" STORAGE_DIR_HOST)" ] || fd_fail "compose.yaml alone must not carry STORAGE_DIR_HOST"
echo "[fsdefault]   compose.yaml alone: no bind, no rows"

echo "[fsdefault] PASS"
