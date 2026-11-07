# 90-media.sh — MEDIA_DELIVERY=redirect (ROADMAP #34 lane F, §20 erratum 278): the media routes
# answer 302 to a freshly presigned URL on the bucket's own origin, after every gate they have
# always run, and the bytes behind the redirect are the same bytes `proxy` mode serves.
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 90.
#
# e2e-schedule: parallel
# e2e-namespace: account=m prefix=MD_,MC_ buckets=-
# e2e-covers: internal/httpapi/media.go internal/store/minio.go internal/store/store.go internal/urls/**
# e2e-covers: internal/config/config.go
#   It runs a SECOND `clippet serve` against a database of its own, twice — once in each delivery
#   mode. Nothing it asserts is about the stack under test, which may be in EITHER mode: the mode is
#   read at ITS boot, and the whole point of the bundle is to hold one database and one set of objects
#   still while the mode changes underneath them. Like 89-quota it kept `exclusive` for the cost of a
#   second app process rather than for a shared thing it touches (§20 erratum 283 (e)); its objects
#   are under an `e2e-media-<ts>/` prefix of its own at the bucket ROOT, removed by the probe and
#   again by the host's EXIT trap, which a lane subshell scopes to this bundle alone.
#
# **It brings its own instance and touches the stack's only through the bucket.** The mode is read at
# BOOT, so none of this could be reached by configuring the stack under test — and this step is the
# only place in the suite where BOTH modes are exercised against the SAME database and the same
# objects, which is what makes "the bytes behind the redirect are the bytes proxy mode serves" an
# assertion and not an assumption. So: one throwaway `dc run --rm` container with its own DATA_DIR,
# its own SCRATCH_DIR (the third convention in scripts/e2e.d/README.md) and its own S3_KEY_PREFIX in
# the shared bucket, which it empties before it exits.
#
# Since §20 erratum 280 the stack under test is free to be on either mode, and the rest of the suite
# detects which at step 6 and asserts accordingly. What this step keeps for itself is everything a
# harness running against ONE stack cannot see: the two boot refusals, the virtual-host arm, the
# sha256 equality across the two modes, and the app-side behaviour the other bundles stand down from
# in `redirect` (the 416 envelope, the 412, the headers a 302 must not carry). Every `[e2e]
# (redirect: … proven by step 90)` line the suite prints points here.
#
# Everything happens INSIDE that container, over its own loopback and the compose network, for
# three reasons: nothing has to be published to the host; the probe's PUBLIC_BASE_URL is then
# `http` whatever the stack under test uses (a Secure session cookie would never come back over the
# container's own loopback); and the presigned Location points at `s3:9000`, which is reachable
# from inside the compose network and from nowhere else — which is the shape a real redirect
# deployment has, where the app reaches the bucket privately and the signature is minted for the
# origin a VIEWER uses.
#
# Variables are prefixed MD_ (media delivery) so nothing here can clobber a harness global.

# ---------------------------------------------------------------------------------------------------
step 90 "MEDIA_DELIVERY=redirect: the media routes 302 to a presigned URL, after every gate, and the bytes are the same"

MD_PREFIX="e2e-media-$(date +%s)/"
MD_USER="${E2E_USER}_m"
MD_PASS="$E2E_PASS"
MD_OUT="$E2E_DIR/media-probe.txt"
# The prefix is emptied INSIDE the container on the happy path; an `exit 3`/`exit 4` or an interrupt
# would leave `e2e-media-<ts>/` objects behind, so the host holds an EXIT trap too (73-ops.sh's and
# 89-quota.sh's pattern): best effort, never the cause of a failure, released once the step has
# proven the prefix empty itself.
#
# The removal goes through the harness's `store_obj rm --prefix` — the app's own `admin objects`,
# errata 281 (f) and 346 — and not a client of its own: this step's whole subject is a stack whose
# media may not be in a bundled server, and a trap that swept one regardless swept nothing. The
# prefix is from the bucket's root: `-e S3_KEY_PREFIX` REPLACES compose's value, so $MD_PREFIX
# already IS the whole prefix and sits at the bucket root.
md_rm_prefix() {
  if ! store_has_bucket; then
    echo "[e2e]   NOTE: this stack keeps its media in a directory, so the media probe's prefix $MD_PREFIX is in no bucket for the harness to sweep"
    return 0
  fi
  # The prefix must be the harness's own minted shape before `rm --prefix` sees it (§20 erratum
  # 290, AUDIT #28): an empty variable here would have named the bucket root.
  store_rm_prefix_ok_ "${MD_PREFIX:-}" || return 0
  # A subshell: `store_obj` DIES on a failure, and this one promises never to be the cause of one.
  ( store_obj rm --prefix "$MD_PREFIX" >/dev/null ) 2>/dev/null \
    || echo "[e2e]   NOTE: could not remove the media probe's prefix $MD_PREFIX (its own objects, outside the stack's key prefix)"
}
trap 'md_rm_prefix' EXIT
echo "[e2e]   probe container: its own database, its own scratch, bucket prefix $MD_PREFIX"

# The in-container script is written to a file rather than inlined: it is long, it is the thing a
# post-mortem reads, and a quoting mistake inside a `-c '…'` argument is the hardest kind to see.
MD_SH="$E2E_DIR/media-probe.sh"
cat > "$MD_SH" <<'MEDIA_PROBE'
set -e
mkdir -p /tmp/media /tmp/media-scratch

# --- (a) the two boot refusals, before anything else runs ----------------------------------------
# A directory has no URL to redirect to, and a signature that expires mid-watch breaks seeking on a
# page nobody reloaded. Both fail at boot, naming the variable (§4).
ME=$(MEDIA_DELIVERY=redirect STORAGE_BACKEND=fs STORAGE_DIR=/tmp/media-dir S3_ENDPOINT= S3_BUCKET= S3_ACCESS_KEY= S3_SECRET_KEY= clippet migrate 2>&1 || true)
echo "redirectonfs=$(printf "%s" "$ME" | tr "\n" " ")"
ME=$(MEDIA_PRESIGN_TTL=10s clippet migrate 2>&1 || true)
echo "shortttl=$(printf "%s" "$ME" | tr "\n" " ")"

clippet migrate >/tmp/migrate.log 2>&1
echo "migrated=$(tail -1 /tmp/migrate.log | tr -d "\r")"

# --- (b) phase one: redirect ----------------------------------------------------------------------
export MEDIA_DELIVERY=redirect
# Path-style on purpose: this arm IS the path-style boot (the Location must land on MD_ORIGIN, the bare
# endpoint); the stack under test may run virtual-host lookup (R2: S3_FORCE_PATH_STYLE=false), and a probe that
# inherited it would sign for <bucket>.<endpoint> instead — the SEVENTH GATE's attempt 6 died here on exactly
# that. The (d2) arm below sets false explicitly and proves the virtual-host shape.
export S3_FORCE_PATH_STYLE=true
export MEDIA_PRESIGN_TTL=30m
export MEDIA_PRESIGN_ENDPOINT=$MD_ORIGIN
clippet serve >/tmp/serve1.log 2>&1 &
MSRV=$!
i=0
while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
MC=$(curl -sS -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/healthz || true)
echo "up1ready=$MC"
[ "$MC" = 200 ] || { echo "serve1fail=$(tr "\n" " " < /tmp/serve1.log)"; exit 3; }
echo "bootline=$(grep -o "media delivery.*" /tmp/serve1.log | head -1 | tr -d "\r")"

# The clip is MADE here rather than mounted: ffmpeg is in this image, the host's testclips/ is not
# reachable from a `compose run` container, and a lavfi source is the same two seconds every time.
ffmpeg -v error -y -f lavfi -i testsrc2=size=320x180:rate=15:duration=2 \
  -f lavfi -i sine=frequency=440:duration=2 \
  -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac -shortest /tmp/m.mp4

curl -sS -o /tmp/reg -c /tmp/jar -w "%{http_code}\n" -X POST -H "Content-Type: application/json" \
  -d "{\"username\":\"$MUSER\",\"password\":\"$MPASS\"}" http://127.0.0.1:8080/api/auth/register > /tmp/regcode
echo "register=$(tr -d "\r\n" < /tmp/regcode)"

mup() {   # mup <title> <unlisted> → the status code; the body lands in /tmp/up
  curl -sS -o /tmp/up -w "%{http_code}" -b /tmp/jar \
    -F "title=$1" -F "ttl_days=1" -F "unlisted=$2" -F duplicate=allow -F "file=@/tmp/m.mp4;type=video/mp4" \
    http://127.0.0.1:8080/api/upload
}
mcid() { grep -o "\"id\":\"[^\"]*\"" /tmp/up | head -1 | cut -d"\"" -f4; }

echo "up1=$(mup "media redirect" false)"
MID=$(mcid)
echo "id1=$MID"
[ -n "$MID" ] || { echo "nolistedid=$(tr -d "\r\n" < /tmp/up)"; exit 3; }
echo "up2=$(mup "media redirect unlisted" true)"
MIDU=$(mcid)
echo "idu=$MIDU"

# Both clips ready. The capture endpoint already waited for the CARD, but `video.mp4` is a
# different object on a clip that gets a Full, so the state is polled rather than assumed.
mready() {
  i=0
  while [ $i -lt 180 ]; do
    MS=$(curl -sS http://127.0.0.1:8080/api/clips/$1 | grep -o "\"state\":\"[a-z]*\"" | head -1 | cut -d"\"" -f4)
    [ "$MS" = ready ] && return 0
    [ "$MS" = failed ] && return 1
    i=$((i+1)); sleep 1
  done
  return 1
}
mready "$MID" || { echo "notready=$MID"; exit 3; }
mready "$MIDU" || { echo "notready=$MIDU"; exit 3; }
echo "ready=1"

# --- (c) the redirect itself ----------------------------------------------------------------------
# One helper for "what did this route answer, and where does it point": the status, the Location and
# the two headers a 302 here must and must not carry, in one line per probe.
mprobe() {   # mprobe <method> <path> → "status|location|cachecontrol|etag"
  # `-I` rather than `-X HEAD`: with -X curl waits for a body the server will
  # never send. With -I the headers ARE the body stream, which -o discards and
  # -D keeps, so -w still has stdout to itself.
  MOPT="-X GET"
  [ "$1" = HEAD ] && MOPT="-I"
  curl -sS -o /dev/null -D /tmp/hdr $MOPT -w "%{http_code}" "http://127.0.0.1:8080$2" > /tmp/code
  MCODE=$(tr -d "\r\n" < /tmp/code)
  MWHERE=$(sed -n "s/^[Ll]ocation: //p" /tmp/hdr | tr -d "\r" | head -1)
  MCC=$(sed -n "s/^[Cc]ache-[Cc]ontrol: //p" /tmp/hdr | tr -d "\r" | head -1)
  METAG=$(sed -n "s/^[Ee][Tt]ag: //p" /tmp/hdr | tr -d "\r" | head -1)
  echo "$MCODE|$MWHERE|$MCC|$METAG"
}
for R in social.mp4 video.mp4 poster.jpg; do
  echo "get_$R=$(mprobe GET /api/clips/$MID/$R)"
  echo "head_$R=$(mprobe HEAD /api/clips/$MID/$R)"
done
echo "unlisted=$(mprobe GET /api/clips/$MIDU/video.mp4)"
echo "download=$(mprobe GET "/api/clips/$MID/video.mp4?download=1")"
echo "notfound=$(mprobe GET /api/clips/aaaaaaaaaaaa/video.mp4)"

# `/original` is the one identity-gated object and is never redirected. Its STATUS depends on the
# row (§20 erratum 207: a playable upload is public), so what is asserted is the thing that is
# origin-independent — it is not a 302 and carries no Location — plus the owner's own 200.
echo "original_anon=$(mprobe GET /api/clips/$MID/original)"
curl -sS -o /dev/null -D /tmp/hdr -b /tmp/jar -w "%{http_code}" http://127.0.0.1:8080/api/clips/$MID/original > /tmp/code
echo "original_owner=$(tr -d "\r\n" < /tmp/code)|$(sed -n "s/^[Ll]ocation: //p" /tmp/hdr | tr -d "\r" | head -1)"

# --- (d) the bytes behind the redirect ------------------------------------------------------------
# The Location is on the bucket's own origin, reachable from inside this network and from nowhere
# else. A 200, a 206 on a Range, and the content type the ROW said — not whatever the object was
# stored with.
curl -sS -o /dev/null -D /tmp/hdr http://127.0.0.1:8080/api/clips/$MID/video.mp4
MTARGET=$(sed -n "s/^[Ll]ocation: //p" /tmp/hdr | tr -d "\r" | head -1)
[ -n "$MTARGET" ] || { echo "noloc=1"; exit 3; }
curl -sS -o /tmp/redirect.mp4 -D /tmp/objhdr -w "%{http_code}" "$MTARGET" > /tmp/code
echo "objget=$(tr -d "\r\n" < /tmp/code)"
echo "objtype=$(sed -n "s/^[Cc]ontent-[Tt]ype: //p" /tmp/objhdr | tr -d "\r" | head -1)"
echo "objdisp=$(sed -n "s/^[Cc]ontent-[Dd]isposition: //p" /tmp/objhdr | tr -d "\r" | head -1)"
echo "objsha=$(sha256sum /tmp/redirect.mp4 | cut -d" " -f1)"
echo "objbytes=$(wc -c < /tmp/redirect.mp4 | tr -d " ")"
curl -sS -o /tmp/part.bin -H "Range: bytes=0-99" -w "%{http_code}" "$MTARGET" > /tmp/code
echo "objrange=$(tr -d "\r\n" < /tmp/code)"
echo "objrangebytes=$(wc -c < /tmp/part.bin | tr -d " ")"

# The settings panel says the served figure is an estimate, and the watch page's CSP names the
# origin a media route now redirects to (without it a browser blocks the player's own <video>).
curl -sS -o /tmp/settings -b /tmp/jar http://127.0.0.1:8080/settings
echo "panelestimate=$(grep -c "estimated (redirect)" /tmp/settings | tr -d " ")"
curl -sS -o /dev/null -D /tmp/pagehdr http://127.0.0.1:8080/$MID
echo "csp=$(sed -n "s/^[Cc]ontent-[Ss]ecurity-[Pp]olicy: //p" /tmp/pagehdr | tr -d "\r" | head -1)"

clippet admin doctor > /tmp/doc 2>&1 || true
echo "doctormedia=$(grep "^media" /tmp/doc | head -1 | tr -d "\r")"
echo "doctorsummary=$(grep "^doctor:" /tmp/doc | head -1 | tr -d "\r")"

# The ORIGIN the Location actually lands on, as a field of its own, so the harness can compare it
# for EQUALITY with the CSP's and with doctor's rather than for containment — a bucket-prefixed host
# satisfies `contains` and was exactly the bug the first cut of this step could not see.
# morigin <url> — the scheme://host[:port] of a URL, and nothing else.
morigin() { printf "%s" "$1" | cut -d/ -f1-3; }
# mcspdir <header-file> <directive> — the one source that directive allows past
# 'self', or "" when it allows nothing past it. Written as three plain pipes on
# their own line: nesting quotes inside $( ) inside "" silently produced "".
mcspdir() {
  MC_LINE=$(sed -n "s/^[Cc]ontent-[Ss]ecurity-[Pp]olicy: //p" "$1" | tr -d "\r" | head -1)
  MC_D=$(printf "%s" "$MC_LINE" | tr ";" "\n" | grep "$2 " | head -1)
  printf "%s" "$MC_D" | sed "s/^ *$2 //" | sed "s/^.self. *//" | sed "s/^data: *//" | tr -d " "
}
# mdoctororigin — the origin `admin doctor`'s media row says a viewer is sent to.
mdoctororigin() { grep "^media" "$1" | head -1 | tr -d "\r" | sed "s/.*answer 302 to //" | cut -d" " -f1 | tr -d ","; }
echo "locorigin=$(morigin "$MTARGET")"
echo "cspmedia=$(mcspdir /tmp/pagehdr media-src)"
echo "cspimg=$(mcspdir /tmp/pagehdr img-src)"
echo "doctororigin=$(mdoctororigin /tmp/doc)"

# A delete stops the redirect at once: the link is minted AFTER the lookup, never before.
echo "del=$(curl -sS -o /dev/null -w "%{http_code}" -b /tmp/jar -X DELETE http://127.0.0.1:8080/api/clips/$MIDU)"
echo "afterdel=$(mprobe GET /api/clips/$MIDU/video.mp4)"

kill $MSRV 2>/dev/null || true
wait $MSRV 2>/dev/null || true

# --- (d2) the VIRTUAL-HOST arm: S3_FORCE_PATH_STYLE=false ------------------------------------------
# The mode above runs on compose's `true`, which is the bundled MinIO's setting and the ONLY one that
# hides the bug this arm exists for: with DNS bucket lookup — the binary's own default, and what
# DEPLOY §7's R2 and B2 stanzas and §33.3's recipe set — minio-go prefixes the BUCKET to the host, so
# the Location lands on `clips.s3:9000` while a CSP built from S3_ENDPOINT would name `s3:9000`.
# CSP host matching is exact, so the player would be blocked by our own policy, silently.
#
# It only compares HEADERS. It cannot fetch the object: nothing resolves `clips.s3`, and the
# bundled MinIO serves no virtual-host bucket without MINIO_DOMAIN — which is fine, because the three
# strings agreeing IS the assertion.
export MEDIA_DELIVERY=redirect
export MEDIA_PRESIGN_TTL=30m
export MEDIA_PRESIGN_ENDPOINT=$MD_ORIGIN
MD_PS_WAS=${S3_FORCE_PATH_STYLE:-true}
export S3_FORCE_PATH_STYLE=false
clippet serve >/tmp/serve3.log 2>&1 &
MSRV=$!
i=0
while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
MC=$(curl -sS -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/healthz || true)
echo "up3ready=$MC"
[ "$MC" = 200 ] || { echo "serve3fail=$(tr "\n" " " < /tmp/serve3.log)"; exit 5; }
curl -sS -o /dev/null -D /tmp/hdr3 http://127.0.0.1:8080/api/clips/$MID/video.mp4
MV=$(sed -n "s/^[Ll]ocation: //p" /tmp/hdr3 | tr -d "\r" | head -1)
echo "vh_locorigin=$(morigin "$MV")"
curl -sS -o /dev/null -D /tmp/pagehdr3 http://127.0.0.1:8080/$MID
echo "vh_cspmedia=$(mcspdir /tmp/pagehdr3 media-src)"
echo "vh_cspimg=$(mcspdir /tmp/pagehdr3 img-src)"
clippet admin doctor > /tmp/doc3 2>&1 || true
echo "vh_doctororigin=$(mdoctororigin /tmp/doc3)"
kill $MSRV 2>/dev/null || true
wait $MSRV 2>/dev/null || true
# RESTORED, never unset: compose passes `true` into this container while the
# binary's own default is `false`, so unsetting it would leave phase three on
# virtual-host lookup, which the bundled MinIO cannot serve.
export S3_FORCE_PATH_STYLE=$MD_PS_WAS

# --- (e) phase three: the SAME database and bucket on `proxy` --------------------------------------
# The same object, through the app this time. Same bytes, and no Location anywhere.
unset MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT
clippet serve >/tmp/serve2.log 2>&1 &
MSRV=$!
i=0
while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
MC=$(curl -sS -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/healthz || true)
echo "up2ready=$MC"
[ "$MC" = 200 ] || { echo "serve2fail=$(tr "\n" " " < /tmp/serve2.log)"; exit 4; }
curl -sS -o /tmp/proxy.mp4 -D /tmp/proxyhdr -w "%{http_code}" http://127.0.0.1:8080/api/clips/$MID/video.mp4 > /tmp/code
echo "proxyget=$(tr -d "\r\n" < /tmp/code)"
echo "proxysha=$(sha256sum /tmp/proxy.mp4 | cut -d" " -f1)"
echo "proxyloc=$(sed -n "s/^[Ll]ocation: //p" /tmp/proxyhdr | tr -d "\r" | head -1)"
echo "proxyetag=$(sed -n "s/^[Ee][Tt]ag: //p" /tmp/proxyhdr | tr -d "\r" | head -1)"
echo "proxyaccept=$(sed -n "s/^[Aa]ccept-[Rr]anges: //p" /tmp/proxyhdr | tr -d "\r" | head -1)"
curl -sS -o /tmp/settings2 -b /tmp/jar http://127.0.0.1:8080/settings
echo "panelestimate2=$(grep -c "estimated (redirect)" /tmp/settings2 | tr -d " ")"
curl -sS -o /dev/null -D /tmp/pagehdr2 http://127.0.0.1:8080/$MID
echo "csp2=$(sed -n "s/^[Cc]ontent-[Ss]ecurity-[Pp]olicy: //p" /tmp/pagehdr2 | tr -d "\r" | head -1)"
echo "cspmedia2=$(mcspdir /tmp/pagehdr2 media-src)"

# --- (f) this step's own litter, gone before the container is -------------------------------------
curl -sS -o /dev/null -b /tmp/jar -X DELETE http://127.0.0.1:8080/api/clips/$MID || true
i=0
while [ $i -lt 120 ]; do
  clippet admin stats > /tmp/st 2>&1 || true
  grep -q "^bucket:  objects=0 " /tmp/st && break
  i=$((i+1)); sleep 1
done
echo "bucket=$(grep "^bucket:" /tmp/st | tr -d "\r")"

kill $MSRV 2>/dev/null || true
wait $MSRV 2>/dev/null || true
echo "logerrors=$(cat /tmp/serve1.log /tmp/serve2.log /tmp/serve3.log | grep -c -E "panic|level=ERROR" | tr -d " ")"
echo "logpanics=$(cat /tmp/serve1.log /tmp/serve2.log /tmp/serve3.log | grep -c -E "panic" | tr -d " ")"
echo "done=1"
MEDIA_PROBE

# `dc run --rm` removes the container on success and on failure, which is the whole reason the probe
# is a `run` and not an `up`. Every Discord variable is blanked (the bot's here; Discord's webhook
# and the clip webhook by dc's probe_env_blank): this probe UPLOADS, and an instance that inherited the
# stack's webhook would announce its throwaway clips in somebody's real channel.
#
# MD_ORIGIN is the bucket as a VIEWER of this probe reaches it — here the compose network's own
# MinIO, which is also what the app itself uses. On R2/B2/S3 the two are one public host and the
# variable is left unset; the unit tests cover the two-client split.
MD_ORIGIN=$(dc exec -T app printenv S3_ENDPOINT | tr -d '\r' | head -1)
[ -n "$MD_ORIGIN" ] || die "could not read S3_ENDPOINT out of the running app container"
echo "[e2e]   presign origin: $MD_ORIGIN (the compose network's own bucket)"

MD_RC=0
dc run --rm -T --no-deps \
  -e "S3_KEY_PREFIX=$MD_PREFIX" \
  -e DATA_DIR=/tmp/media -e SCRATCH_DIR=/tmp/media-scratch \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 \
  -e REGISTRATION=open -e SWEEP_INTERVAL=5s -e CAPTURE_READY_WAIT=30s \
  -e DISCORD_APP_ID= -e DISCORD_PUBLIC_KEY= -e DISCORD_BOT_TOKEN= \
  -e "MD_ORIGIN=$MD_ORIGIN" -e "MUSER=$MD_USER" -e "MPASS=$MD_PASS" \
  --entrypoint sh app -c "$(cat "$MD_SH")" > "$MD_OUT" 2>&1 || MD_RC=$?
# The probe's own lines carry the signed Locations it read (`get_social.mp4=302|https://…?X-Amz-…`), signed
# with the STACK'S credentials — on the reference stack a live R2 key id — and this transcript lives on
# disk. So every print of $MD_OUT goes through md_redact_, which strips the two credential-shaped query
# values and nothing else (the speed lane's verifier found eight full links per run in the gate logs).
md_redact_() { sed -E 's/(X-Amz-Credential=)[^&|[:space:]]+/<redacted>/g; s/(X-Amz-Signature=)[0-9a-fA-F]+/<redacted>/g'; }
head -70 "$MD_OUT" | md_redact_ | sed -n 's/^/[e2e]   media-probe: /p'
[ "$MD_RC" = 0 ] || die "the media probe container failed (exit $MD_RC):
$(md_redact_ < "$MD_OUT")"
has_file "$MD_OUT" '^done=1' "the media probe did not run to the end"

# md_get <key> — one `key=value` line the probe printed. The script that produced them stays in
# .e2e/ beside the output: a probe whose assertions fail is read as a pair.
md_get() { sed -n "s/^$1=//p" "$MD_OUT" | head -1; }
# md_field <line> <n> — one field of a `status|location|cache-control|etag` probe line.
md_field() { printf '%s' "$1" | cut -d'|' -f"$2"; }

# --- the boot gates -------------------------------------------------------------------------------
MD_V=$(md_get redirectonfs)
contains "$MD_V" 'MEDIA_DELIVERY' || die "redirect on a directory did not name the variable: $MD_V"
contains "$MD_V" 'STORAGE_BACKEND=fs' || die "…and did not say which backend cannot do it: $MD_V"
MD_V=$(md_get shortttl)
contains "$MD_V" 'MEDIA_PRESIGN_TTL' || die "a TTL under the floor did not name the variable: $MD_V"
echo "[e2e]   boot: redirect on a directory and a sub-minute signature are both refused, by name"

expect "$(md_get up1ready)" 200 "the probe instance came up in redirect mode"
contains "$(md_get bootline)" 'redirect' || die "the boot line does not name the delivery mode: $(md_get bootline)"
expect "$(md_get register)" 201 "register on the probe instance"
expect "$(md_get up1)" 201 "the listed upload ($(md_get up1))"
expect "$(md_get up2)" 201 "the unlisted upload ($(md_get up2))"
expect_nonempty "$(md_get id1)" "the listed clip's id"
expect "$(md_get ready)" 1 "both probe clips reached ready"

# --- the 302 ---------------------------------------------------------------------------------------
MD_HOST=${MD_ORIGIN#*://}
for MD_R in social.mp4 video.mp4 poster.jpg; do
  for MD_M in get head; do
    MD_V=$(md_get "${MD_M}_${MD_R}")
    expect "$(md_field "$MD_V" 1)" 302 "$MD_M $MD_R in redirect mode ($MD_V)"
    MD_L=$(md_field "$MD_V" 2)
    contains "$MD_L" "$MD_ORIGIN/" || die "$MD_M $MD_R points somewhere else: $(media_redact_ "$MD_L")"
    contains "$MD_L" 'X-Amz-Signature=' || die "$MD_M $MD_R is not a signed link: $(media_redact_ "$MD_L")"
    contains "$MD_L" 'X-Amz-Expires=1800' || die "$MD_M $MD_R does not carry MEDIA_PRESIGN_TTL: $(media_redact_ "$MD_L")"
    contains "$MD_L" 'response-content-disposition=inline' || die "$MD_M $MD_R is not marked inline: $(media_redact_ "$MD_L")"
    expect "$(md_field "$MD_V" 3)" 'no-store' "$MD_M $MD_R: a signed Location must not be stored"
    [ -z "$(md_field "$MD_V" 4)" ] || die "$MD_M $MD_R: the 302 carries an ETag for bytes it does not hold"
  done
done
echo "[e2e]   302: six probes on three routes, GET and HEAD alike, each to a freshly signed link on $MD_HOST, no-store, no validators"

MD_V=$(md_get unlisted)
expect "$(md_field "$MD_V" 1)" 302 "an unlisted clip redirects exactly like a listed one ($MD_V)"
MD_V=$(md_get download)
expect "$(md_field "$MD_V" 1)" 302 "?download=1 in redirect mode ($MD_V)"
contains "$(md_field "$MD_V" 2)" 'response-content-disposition=attachment' \
  || die "?download=1 did not carry the attachment into the presign: $(md_field "$MD_V" 2)"
MD_V=$(md_get notfound)
expect "$(md_field "$MD_V" 1)" 404 "an unknown clip is refused before anything is signed ($MD_V)"
[ -z "$(md_field "$MD_V" 2)" ] || die "an unknown clip still handed out a Location: $MD_V"

MD_V=$(md_get original_anon)
[ "$(md_field "$MD_V" 1)" != 302 ] || die "/original was redirected: it is the one identity-gated object ($MD_V)"
[ -z "$(md_field "$MD_V" 2)" ] || die "/original handed out a presigned Location: $MD_V"
MD_V=$(md_get original_owner)
expect "$(md_field "$MD_V" 1)" 200 "the owner's /original is still served by the app ($MD_V)"
[ -z "$(md_field "$MD_V" 2)" ] || die "the owner's /original was redirected: $MD_V"
echo "[e2e]   gates: unlisted redirects the same, ?download=1 carries the attachment, an unknown clip signs nothing, /original stays app-served"

# --- the bytes behind the link ---------------------------------------------------------------------
expect "$(md_get objget)" 200 "fetching the presigned Location from inside the network"
expect "$(md_get objtype)" 'video/mp4' "the bucket answered with the type the ROW said"
expect "$(md_get objdisp)" 'inline' "the bucket answered with the disposition the presign asked for"
expect "$(md_get objrange)" 206 "a Range against the presigned Location"
expect "$(md_get objrangebytes)" 100 "the 206 carried the hundred bytes asked for"
MD_BYTES=$(md_get objbytes)
[ -n "$MD_BYTES" ] && [ "$MD_BYTES" -gt 0 ] || die "the presigned fetch returned $MD_BYTES bytes"

expect "$(md_get up2ready)" 200 "the same instance came back up on proxy"
expect "$(md_get proxyget)" 200 "the same object through the app in proxy mode"
expect_empty "$(md_get proxyloc)" "proxy mode handed out a Location"
expect_nonempty "$(md_get proxyetag)" "proxy mode lost the object's ETag"
expect "$(md_get proxyaccept)" 'bytes' "proxy mode lost Accept-Ranges"
expect "$(md_get objsha)" "$(md_get proxysha)" "the bytes behind the redirect are not the bytes proxy mode serves"
echo "[e2e]   bytes: $MD_BYTES B through the bucket == the same sha256 through the app, 206 on a Range, and proxy mode is byte-for-byte untouched"

# --- what the operator sees ------------------------------------------------------------------------
expect "$(md_get panelestimate)" 1 "the Storage panel's served row does not say the figure is an estimate"
expect "$(md_get panelestimate2)" 0 "proxy mode still qualifies the served figure"
MD_V=$(md_get csp)
contains "$MD_V" "media-src 'self' $MD_ORIGIN;" || die "the page CSP does not allow the media origin: $MD_V"
contains "$MD_V" "img-src 'self' data: $MD_ORIGIN;" || die "the page CSP does not allow the poster's origin: $MD_V"
# The three strings that MUST be the same host, compared for EQUALITY and not containment: where the
# redirect lands, what the policy allows, and what doctor reports. `contains` was satisfied by a
# bucket-prefixed host, which is how a virtual-host deployment shipped a policy that blocked its own
# player (§20 erratum 278(j), amended).
expect "$(md_get locorigin)" "$MD_ORIGIN" "the Location's origin on a path-style boot"
expect "$(md_get cspmedia)" "$(md_get locorigin)" "the CSP's media-src origin is not where the Location points"
expect "$(md_get cspimg)" "$(md_get locorigin)" "the CSP's img-src origin is not where the poster's Location points"
expect "$(md_get doctororigin)" "$(md_get locorigin)" "doctor's media row names an origin the Location does not use"
expect_empty "$(md_get cspmedia2)" "proxy mode's media-src allows something past 'self'"
MD_V=$(md_get csp2)
contains "$MD_V" "media-src 'self';" || die "proxy mode's CSP moved: $MD_V"
if contains "$MD_V" "$MD_ORIGIN"; then die "proxy mode names a media origin in its CSP: $MD_V"; fi
MD_V=$(md_get doctormedia)
contains "$MD_V" 'MEDIA_DELIVERY=redirect' || die "doctor's media row does not name the mode: $MD_V"
contains "$MD_V" "$MD_ORIGIN" || die "doctor's media row does not name the presign origin: $MD_V"
contains "$MD_V" '30m' || die "doctor's media row does not name the signature's life: $MD_V"
contains "$(md_get doctorsummary)" '19 checks' || die "doctor's summary lost a check: $(md_get doctorsummary)"
echo "[e2e]   operator: the panel says estimated (redirect), the CSP names $MD_HOST in both media directives, doctor's media row reads the mode back"

# --- the same three strings with VIRTUAL-HOST bucket lookup ------------------------------------------
# S3_FORCE_PATH_STYLE=false is the binary's own default and what R2, B2 and S3 are configured with, and
# it puts the BUCKET in the host. All three must move together; if the CSP still named the bare
# endpoint the watch page would be blocked by our own policy while the Discord card kept working.
expect "$(md_get up3ready)" 200 "the probe instance came up with virtual-host bucket lookup"
MD_VH=$(md_get vh_locorigin)
expect_nonempty "$MD_VH" "the virtual-host arm's Location origin"
[ "$MD_VH" != "$MD_ORIGIN" ] || die "S3_FORCE_PATH_STYLE=false did not move the host: $MD_VH (the arm proves nothing)"
contains "$MD_VH" "$S3_BUCKET." || die "a virtual-host Location does not carry the bucket in the host: $MD_VH"
expect "$(md_get vh_cspmedia)" "$MD_VH" "media-src does not follow the bucket into the host"
expect "$(md_get vh_cspimg)" "$MD_VH" "img-src does not follow the bucket into the host"
expect "$(md_get vh_doctororigin)" "$MD_VH" "doctor's media row does not follow the bucket into the host"
echo "[e2e]   virtual-host: S3_FORCE_PATH_STYLE=false lands on $MD_VH and the CSP and doctor say the same, exactly"

# --- and a delete still means it --------------------------------------------------------------------
expect "$(md_get del)" 204 "delete the unlisted probe clip"
MD_V=$(md_get afterdel)
expect "$(md_field "$MD_V" 1)" 404 "a deleted clip still redirected ($MD_V)"
[ -z "$(md_field "$MD_V" 2)" ] || die "a deleted clip handed out a Location: $MD_V"

# --- and it left nothing behind ----------------------------------------------------------------------
MD_V=$(md_get bucket)
contains "$MD_V" 'objects=0' || die "the probe's own bucket prefix ($MD_PREFIX) is not empty: $MD_V"
# The probe's own reading is taken BEFORE it exits; its database snapshot (erratum 343) lands after it,
# so the host empties the prefix and counts it once the container is gone.
probe_prefix_swept_ "$MD_PREFIX" "the media probe"
trap - EXIT
# A PANIC is fatal, the way step 16's log gate treats one on the stack under test: it is never
# something a probe may shrug at. An ERROR line stays a NOTE — this probe deliberately provokes
# refusals, and the app logs some of them at ERROR by design.
expect "$(md_get logpanics)" 0 "the probe's own app log carries a panic — see $MD_OUT"
if [ "$(md_get logerrors)" != 0 ]; then
  echo "[e2e]   NOTE: the probe's own app log has $(md_get logerrors) ERROR line(s) — see $MD_OUT"
fi
summary_row MD "$(md_get id1)" "media (#34 lane F): probe instance in MEDIA_DELIVERY=redirect — 302 on social/video/poster, GET and HEAD, to $MD_HOST; the bytes behind the link match proxy mode's sha256; /original never redirected; prefix $MD_PREFIX emptied"
