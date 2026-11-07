# 88-fsstore.sh — a directory instead of a bucket (ROADMAP #37, §20 erratum 276): one throwaway app
# container with STORAGE_BACKEND=fs and no S3_* variable at all, driven end to end — register, upload,
# encode, serve (ranged and whole), `admin doctor`, `admin stats`, `admin orphans`, the sweeper's
# destroy, and the migrate verb's plan.
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 88. Everything it makes lives inside its own container and goes away with it.
#
# e2e-schedule: parallel
# e2e-namespace: account=f prefix=FS_ buckets=-
# e2e-covers: internal/store/fs.go internal/store/fs_sync_other.go internal/store/store.go cmd/clippet/admin_storage.go internal/config/config.go
# e2e-covers: internal/httpapi/media.go
#   It starts a SECOND app process with its own published port, its own database and its own media
#   directory, and it reads THAT instance's `admin doctor`/`stats`/`orphans`. Every one of those three
#   sentences is about the container it brought, not about the stack under test — which is the test
#   the README's exclusive list actually asks (§20 erratum 283 (e)). Its published port is derived
#   from `CLIPPET_PUBLISH` + 200 and is claimed by nothing else in the suite; the two other bundles
#   that publish one (37-library, 55-owner) are exclusive and therefore never concurrent with a lane.
#   The one thing it asks the STACK is a single `BucketExists` (through `storage migrate --to s3
#   --dry-run`, which copies nothing), which is a read.
#
# What it touches on THIS stack: nothing. Not the database, not the store, not a clip. The container
# has its own DATA_DIR, SCRATCH_DIR and STORAGE_DIR under /tmp, all inside the container's writable
# layer, and it is removed at the end and on any failure (trap). That is why this bundle is safe on
# the reference stack as well as on a lane: it proves a backend the stack itself is not running.
#
# Variables are prefixed FS_ so nothing here can clobber a harness global.

# ---------------------------------------------------------------------------------------------------
step 88 "a directory instead of a bucket (#37): the whole product on STORAGE_BACKEND=fs, with no S3_* variable anywhere"

FS_PORT=$(envfile CLIPPET_PUBLISH); FS_PORT=${FS_PORT##*:}; FS_PORT=${FS_PORT:-8481}
FS_PORT=$((FS_PORT + 200))
FS_BASE="http://localhost:$FS_PORT"
FS_CT="clippet-e2e-fsstore-$FS_PORT"
FS_USER="e2e_$(date +%s)_f"
FS_JAR="$E2E_DIR/cookies-fs.txt"; FS_JARW=$(W "$FS_JAR"); rm -f "$FS_JAR"
FS_OUT="$E2E_DIR/fsstore.txt"

fs_stop() {
  docker stop -t 20 "$FS_CT" >/dev/null 2>&1 || true
  docker rm -f "$FS_CT" >/dev/null 2>&1 || true
}
fs_stop
# The trap is armed BEFORE the container exists, so a failure anywhere below still removes it — a
# second app process left running against a published port is the one thing this step must not leave.
trap 'fs_stop' EXIT

# fs_sh <shell-line…> — run a command inside the container with the four bucket variables UNSET.
# `docker exec` inherits the environment the container was CREATED with, and compose interpolates
# `S3_ENDPOINT: ${S3_ENDPOINT:-http://s3:9000}` for every container of this project — so without
# this the app would see a bucket configured beside STORAGE_BACKEND=fs and refuse to start, which is
# exactly the rule under test. Unsetting them in the shell is also the honest shape: an operator on
# `fs` has no such lines in `.env` at all.
# The three MEDIA_* variables travel with the four bucket ones for the reason the container's own boot
# script gives: `docker exec` inherits the environment the container was CREATED with, and `redirect`
# on an `fs` instance is a boot error, so `admin doctor` and every other verb below would refuse on a
# stack that is in redirect mode (§20 erratum 280).
fs_sh() { docker_ exec "$FS_CT" sh -c "unset S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT; $*"; }

# fs_verdict/fs_failed_rows are 51-admin.sh's doctor readers, written again here on purpose:
# scripts/e2e.d/README.md's second convention (and a scripts/check.sh stage) forbids one drop-in
# calling a helper another one defines, and two three-line awk readers are not worth a harness global.
# fs_verdict <output-file> <check> → the WORST verdict that check printed (FAIL > skip > warn > ok).
fs_verdict() {
  awk -v c="$2" 'BEGIN{r["ok"]=1;r["warn"]=2;r["skip"]=3;r["FAIL"]=4;b=0;s=""}
    $1==c && r[$2]>b {b=r[$2]; s=$2} END{print s}' "$1"
}
# fs_failed_rows <output-file> → every FAIL line, for a message an operator can read.
fs_failed_rows() { awk '$2=="FAIL"' "$1"; }

# --- (a) the one-container instance ----------------------------------------------------------------
# No S3_ENDPOINT, no S3_BUCKET, no credentials: the media lives in /tmp/media and the binary has no
# outbound destination for it at all (§15). DATA_DIR and SCRATCH_DIR are private SIBLINGS (the third
# convention in scripts/e2e.d/README.md — a `clippet serve` on a private database over the LIVE
# /scratch deletes the real working directories), and STORAGE_DIR is a third one, because the app
# refuses a media store that sits inside either of them.
dc run -d --name "$FS_CT" --no-deps -p "127.0.0.1:$FS_PORT:8080" \
  -e "PUBLIC_BASE_URL=$FS_BASE" -e REGISTRATION=open -e SWEEP_INTERVAL=5s -e SWEEP_GRACE=1s \
  -e DATA_DIR=/tmp/fsdata -e SCRATCH_DIR=/tmp/fsscratch -e STORAGE_DIR=/tmp/media \
  -e STORAGE_BACKEND=fs \
  --entrypoint sh app -c '
    set -e
    # The four bucket variables, and every Discord one. compose interpolates all of them for every
    # container of this project, so a probe that did not clear them would (a) refuse to boot, which
    # is the `fs` rule under test, and (b) post this throwaway clip to the operator s own channel
    # when it became ready. `unset` in the shell is the only form that is certainly empty; a compose
    # `-e VAR=` is not a promise this suite should rest on.
    unset S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY
    unset DISCORD_WEBHOOK_URL DISCORD_WEBHOOK_ROUTES DISCORD_APP_ID DISCORD_PUBLIC_KEY DISCORD_BOT_TOKEN
    # ...and the delivery mode, for the same reason and one of its own. compose interpolates
    # MEDIA_DELIVERY for every container of this project, and `redirect` on STORAGE_BACKEND=fs is a
    # BOOT ERROR by design -- a directory has no URL to redirect anyone to (DESIGN 20 erratum 278(e)).
    # On a stack that is itself in redirect mode this probe would therefore refuse to start, over a
    # rule that has nothing to do with what it is testing. An operator on fs has no such line either.
    unset MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT
    exec clippet serve
  ' >/dev/null || die "could not start the STORAGE_BACKEND=fs container"
FS_N=0
while [ "$FS_N" -lt 90 ]; do
  code=$(get "$FS_BASE/readyz" 2>/dev/null || echo 000)
  [ "$code" = 200 ] && break
  FS_N=$((FS_N + 1)); sleep 1
done
[ "$code" = 200 ] || die "the fs container never became ready (readyz=$code):
$(docker_ logs --tail 40 "$FS_CT" 2>&1)"
echo "[e2e]   fs app up on $FS_BASE after ${FS_N}s"

# /readyz passing at all is the store ping answering over a DIRECTORY. /healthz says WHICH backend
# answered, which is the one fact an anonymous prober cannot otherwise establish about an unwell
# instance (§20 erratum 276) — and it is still bytes and words, never a path or a bucket name.
code=$(get "$FS_BASE/healthz"); expect "$code" 200 "GET /healthz on the fs instance"
FS_HEALTH=$(json storage)
expect "$FS_HEALTH" "ok (fs)" "/healthz's storage string names the backend"
case "$(cat "$RESP")" in
  */tmp/media*) die "/healthz leaked STORAGE_DIR: $(cat "$RESP")" ;;
esac
expect_nonempty "$(json storage_usage.stored_bytes)" "/healthz storage_usage on the fs instance"

# --- (b) admin doctor: the directory's questions, and the bucket rows skipped -----------------------
fs_sh clippet admin doctor > "$FS_OUT" 2>&1 || die "admin doctor on fs exited non-zero:
$(cat "$FS_OUT")"
sed -n 's/^/[e2e]   /p' "$FS_OUT"
# All fifteen, `media` and `drop` included (§20 errata 283 (h) and 296): `redirect` on a directory is a
# boot error, so
# the row an fs instance prints is the one that says it serves its own bytes — and a row that vanished
# would have left this loop, and the 14-check summary line below, both satisfied.
for chk in config baseurl paths db ffmpeg creds store bucket media drop proxies webhook bot repair prices; do
  v=$(fs_verdict "$FS_OUT" "$chk")
  expect_nonempty "$v" "doctor check '$chk' did not appear in the fs report"
done
[ -z "$(fs_failed_rows "$FS_OUT")" ] || die "admin doctor FAILED a check on a working fs instance:
$(fs_failed_rows "$FS_OUT")"
expect "$(fs_verdict "$FS_OUT" store)" ok "doctor's 'store' row on a working fs instance"
expect "$(fs_verdict "$FS_OUT" bucket)" skip "doctor's 'bucket' row on an fs instance (it must SKIP, with a reason)"
expect "$(fs_verdict "$FS_OUT" creds)" ok "doctor's 'creds' row on an fs instance"
has_file "$FS_OUT" 'doctor:  19 checks' "the fs summary line"
has_file "$FS_OUT" '1 skipped' "the fs summary must show the skipped bucket row"
has_file "$FS_OUT" 'STORAGE_DIR=/tmp/media writable' "the store row must PROVE writability, not just existence"
has_file "$FS_OUT" 'probe file created and removed' "the store row's write probe"
has_file "$FS_OUT" '.clippet-store is present' "clippet serve must write the store marker at boot"
has_file "$FS_OUT" 'no bucket credential at all' "the creds row on fs"
has_file "$FS_OUT" 'there is no bucket' "the bucket row's one-line reason"
has_file "$FS_OUT" 'NO outbound destination' "the §15 sentence this whole backend earns"

# The rule that earns that sentence: a leftover bucket variable is a BOOT ERROR on `fs`, so an
# operator reading `.env` can see there is nowhere for media to go.
FS_RC=0
docker_ exec -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= -e S3_BUCKET=clips "$FS_CT" clippet admin stats > "$FS_OUT" 2>&1 || FS_RC=$?
expect "$FS_RC" 2 "a leftover S3_BUCKET on STORAGE_BACKEND=fs must refuse to start"
has_file "$FS_OUT" 'takes no S3_\* variable' "the refusal must name the rule"
has_file "$FS_OUT" 'S3_BUCKET' "the refusal must name the variable"

# --- (c) the product, end to end, over a directory --------------------------------------------------
code=$(http POST "$FS_BASE/api/auth/register" -c "$FS_JARW" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$FS_USER\",\"password\":\"$E2E_PASS\"}")
expect "$code" 201 "register $FS_USER on the fs instance ($(cat "$RESP"))"

# The setup wizard's total-block rule, in the ONE direction only an `fs` instance can prove
# (ROADMAP #45, §20 erratum 298). This is MAJOR 1 of the scoping's skeptic: an operator moving off a
# directory onto B2 keeps booting `fs` — silently — if the block merely tells them to DELETE the
# COMPOSE_FILE line, because compose.fs.yaml hard-sets STORAGE_BACKEND and four empty S3_*, an empty
# value is not a boot error, and `Check now` would then say `ok` about the wrong store. The fix is
# that every block ASSIGNS COMPOSE_FILE, and this is the only place in the suite where the page is
# asked that question by an instance that is actually on `fs`.
FS_SETUP="$E2E_DIR/fs-setup.html"
code=$(curl_ -sS -o "$(W "$FS_SETUP")" -w '%{http_code}' -b "$FS_JARW" "$FS_BASE/setup?store=b2&via=other")
expect "$code" 200 "GET /setup?store=b2 on the fs instance"
has_file "$FS_SETUP" 'data-setup-backend="fs"' "the wizard does not know this instance is on a directory"
has_file_f "$FS_SETUP" 'COMPOSE_FILE=compose.yaml:compose.nobucket.yaml'   "the B2 block on an fs instance must ASSIGN the external-bucket flavour, not ask for a deletion"
has_file_f "$FS_SETUP" 'STORAGE_BACKEND=s3' "…and assign the backend it is moving to"
lacks_file_f "$FS_SETUP" 'compose.fs.yaml' "the B2 block still names the fs flavour"
echo "[e2e]   setup (#45): on a directory, the B2 block assigns compose.nobucket.yaml — the direction a deletion instruction would have failed silently"

printf '%s' '{"title":"E2E fs store","ttl_days":1}' > "$E2E_DIR/meta-fs.json"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$FS_JARW" \
  -F "metadata=<$(W "$E2E_DIR/meta-fs.json");type=application/json" \
  -F duplicate=allow -F "video=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4" "$FS_BASE/api/clips")
expect "$code" 201 "upload std-720p30.mp4 to the fs instance ($(cat "$RESP"))"
FS_ID=$(json id); expect_nonempty "$FS_ID" "the fs clip id"

FS_N=0
while [ "$FS_N" -lt 180 ]; do
  code=$(get "$FS_BASE/api/clips/$FS_ID/progress")
  expect "$code" 200 "fs progress"
  FS_STATE=$(json state)
  case $FS_STATE in
    ready) break ;;
    failed) die "the fs clip failed: $(json error)" ;;
  esac
  FS_N=$((FS_N + 1)); sleep 1
done
expect "$FS_STATE" ready "the fs clip reached ready within ${FS_N}s"
echo "[e2e]   the fs clip encoded in ${FS_N}s, out of a directory and back"

# The whole rendition, then a RANGE of it — the media hot path is store.NewSeeker over GetRange, and
# an off-by-one in an inclusive end is the bug that would show here and nowhere else.
FS_WHOLE=$(curl_ -sS -o /dev/null -w '%{http_code} %{size_download}' "$FS_BASE/api/clips/$FS_ID/video.mp4")
expect "${FS_WHOLE%% *}" 200 "GET the fs rendition whole"
FS_SIZE=${FS_WHOLE##* }
[ "$FS_SIZE" -gt 10000 ] 2>/dev/null || die "the fs rendition delivered $FS_SIZE bytes"
FS_RANGED=$(curl_ -sS -o /dev/null -w '%{http_code} %{size_download}' -H 'Range: bytes=100-199' "$FS_BASE/api/clips/$FS_ID/video.mp4")
expect "$FS_RANGED" "206 100" "a 100-byte range out of a directory (inclusive end)"
code=$(head_ "$FS_BASE/api/clips/$FS_ID/poster.jpg"); expect "$code" 200 "HEAD the fs poster"
expect "$(hdr content-type)" image/jpeg "the fs poster's content type comes from the DATABASE, not from the store"
code=$(get "$FS_BASE/$FS_ID"); expect "$code" 200 "the fs share page"

# --- (d) admin stats: one place, described once ----------------------------------------------------
fs_sh clippet admin stats > "$FS_OUT" 2>&1 || die "admin stats on fs failed:
$(cat "$FS_OUT")"
sed -nE 's/^(storage:|store:|disk:|served:)/[e2e]   \1/p' "$FS_OUT"
has_file "$FS_OUT" '^storage: used=' "the fs storage: line"
has_file "$FS_OUT" 'backend=fs dir=/tmp/media free=' "on fs the budget line and the free disk describe ONE place"
has_file "$FS_OUT" '^store:   objects=' "the listing line must say store:, not bucket:"
has_file "$FS_OUT" 'clip_prefixes=1' "the fs store holds this step's one clip"
lacks_file "$FS_OUT" '^bucket:' "an fs instance printed a bucket: line"
has_file "$FS_OUT" '^disk:' "the disk: line still describes the database and the scratch disk"

# --- (e) admin orphans over a directory ------------------------------------------------------------
fs_sh clippet admin orphans --min-age 0 > "$FS_OUT" 2>&1 || die "admin orphans on fs failed:
$(cat "$FS_OUT")"
has_file "$FS_OUT" 'prefixes=0 deleted=0 recent=0 missing=0 foreign=0 extra=0' "a clean fs store"
# A planted stray under the LIVE clip: `reportExtras` walks Store.List, which a directory walk
# satisfies — that is the claim, and this is the proof.
# Both plants get an mtime a few years in the past: the report skips anything YOUNGER than --min-age,
# and the Docker VM's clock steps — a file written a second before `now` can read as
# newer than it, and a negative age is "recent", never "orphan" (the TENTH GATE's one failed bundle).
fs_sh "mkdir -p /tmp/media/clips/$FS_ID/renditions && printf 'garbage' > /tmp/media/clips/$FS_ID/renditions/main-deadbeef.mp4 && touch -t 202001010000 /tmp/media/clips/$FS_ID/renditions/main-deadbeef.mp4"
fs_sh clippet admin orphans --min-age 0 > "$FS_OUT" 2>&1 || die "admin orphans (stray file) failed:
$(cat "$FS_OUT")"
has_file "$FS_OUT" "extra=1" "a planted stray file under a live clip must be reported"
has_file "$FS_OUT" "main-deadbeef.mp4" "the stray must be named"
fs_sh clippet admin orphans --min-age 0 --delete > "$FS_OUT" 2>&1 || die "admin orphans --delete failed:
$(cat "$FS_OUT")"
has_file "$FS_OUT" 'extra_deleted=1' "the stray must be removed"
fs_sh test ! -e "/tmp/media/clips/$FS_ID/renditions/main-deadbeef.mp4" \
  || die "the stray file is still on disk after --delete"
# A planted row-less PREFIX is the other report, and the directory walk finds it the same way.
fs_sh 'mkdir -p /tmp/media/clips/Zz9Yy8Xx7Ww6/original && printf "rowless" > /tmp/media/clips/Zz9Yy8Xx7Ww6/original/source.mp4 && touch -t 202001010000 /tmp/media/clips/Zz9Yy8Xx7Ww6/original/source.mp4'
fs_sh clippet admin orphans --min-age 0 > "$FS_OUT" 2>&1 || die "admin orphans (orphan prefix) failed:
$(cat "$FS_OUT")"
has_file "$FS_OUT" 'prefixes=1' "a planted row-less prefix must be reported"
fs_sh clippet admin orphans --min-age 0 --delete > "$FS_OUT" 2>&1 || die "admin orphans --delete (prefix) failed:
$(cat "$FS_OUT")"
has_file "$FS_OUT" 'deleted=1' "the orphan prefix must be removed"
fs_sh test ! -e /tmp/media/clips/Zz9Yy8Xx7Ww6 || die "the orphan prefix is still on disk after --delete"

# --- (f) admin storage migrate --dry-run: the plan, and nothing else --------------------------------
# The migrate verb is the ONE place STORAGE_BACKEND=fs may be read beside a configured bucket, so the
# bucket variables are supplied here and NOWHERE else in this step. `--dry-run` copies nothing; the
# only thing it asks the stack's own MinIO is whether the bucket exists.
FS_EP=$(envfile S3_ENDPOINT); FS_EP=${FS_EP:-http://s3:9000}
FS_BK=$(envfile S3_BUCKET); FS_BK=${FS_BK:-clips}
FS_AK=$(envfile S3_ACCESS_KEY); FS_AK=${FS_AK:-$(envfile MINIO_ROOT_USER)}; FS_AK=${FS_AK:-clippet}
FS_SECRET=$(envfile S3_SECRET_KEY); FS_SECRET=${FS_SECRET:-$(envfile MINIO_ROOT_PASSWORD)}; FS_SECRET=${FS_SECRET:-clippet-dev-secret}
docker_ exec -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e "S3_ENDPOINT=$FS_EP" -e "S3_BUCKET=$FS_BK" -e "S3_ACCESS_KEY=$FS_AK" -e "S3_SECRET_KEY=$FS_SECRET" \
  "$FS_CT" clippet admin storage migrate --to s3 --dry-run > "$FS_OUT" 2>&1 \
  || die "admin storage migrate --dry-run failed:
$(cat "$FS_OUT")"
sed -n 's/^migrate:/[e2e]   migrate:/p' "$FS_OUT"
has_file "$FS_OUT" "^migrate: from fs (/tmp/media) to s3 ($FS_BK at " "the plan must name both stores"
has_file "$FS_OUT" '^migrate: plan clips=1 objects=' "the plan must count what it would copy"
has_file "$FS_OUT" "^migrate: plan $FS_ID state=ready objects=" "the plan must name each clip"
has_file "$FS_OUT" 'the source is NEVER written to or deleted from' "the plan must say what it will not do"
has_file "$FS_OUT" 'nothing was copied' "--dry-run must copy nothing"
lacks_secret_f "$FS_OUT" "$FS_SECRET" "the migrate plan printed the bucket secret"
# Without the bucket variables the same command names exactly what is missing and does nothing.
FS_RC=0
fs_sh clippet admin storage migrate --to s3 --dry-run > "$FS_OUT" 2>&1 || FS_RC=$?
expect "$FS_RC" 2 "migrate --to s3 with no bucket configured"
has_file "$FS_OUT" 'S3_BUCKET' "the refusal must name what is missing"
# And it refuses to copy a store onto itself.
FS_RC=0
fs_sh clippet admin storage migrate --to fs --dry-run > "$FS_OUT" 2>&1 || FS_RC=$?
expect "$FS_RC" 2 "migrate --to fs on an fs instance"
has_file "$FS_OUT" 'already uses' "the refusal must say why"

# --- (g) the sweeper destroys a directory's objects, and verifies it --------------------------------
FS_TREE=$(fs_sh "ls /tmp/media/clips/$FS_ID/renditions" 2>&1) || die "the clip's tree is not on disk: $FS_TREE"
echo "[e2e]   on disk before the delete: $(echo "$FS_TREE" | tr '\n' ' ')"
code=$(http DELETE "$FS_BASE/api/clips/$FS_ID" -b "$FS_JARW"); expect "$code" 204 "DELETE the fs clip"
FS_N=0
while [ "$FS_N" -lt 90 ]; do
  fs_sh test ! -e "/tmp/media/clips/$FS_ID" && break
  FS_N=$((FS_N + 1)); sleep 1
done
fs_sh test ! -e "/tmp/media/clips/$FS_ID" \
  || die "the sweeper did not empty the clip's directory within ${FS_N}s"
echo "[e2e]   the sweeper destroyed the clip's objects in ${FS_N}s and pruned its directories"
fs_sh clippet admin orphans --min-age 0 > "$FS_OUT" 2>&1 || die "admin orphans after the sweep failed:
$(cat "$FS_OUT")"
has_file "$FS_OUT" 'prefixes=0 deleted=0 recent=0 missing=0 foreign=0 extra=0' "the fs store after the sweep"
# The marker survives everything: it is what tells the next boot this is the right directory.
fs_sh test -f /tmp/media/.clippet-store || die "the store marker did not survive the sweep"

fs_stop
trap - EXIT
summary_row FS "$FS_ID" "fs store (#37): a one-container instance on STORAGE_BACKEND=fs — uploaded, encoded, served whole and ranged, doctor 19 checks (bucket skipped), stats/orphans over a directory, migrate --dry-run; deleted and swept, container removed"
