# 107-snapshot.sh — the database snapshot follows the clips into the store (DESIGN §20 errata 343, 348).
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 107.
#
# Three parts. (a) The stack under test has DB_SNAPSHOT unset, which is ON on either backend (erratum 348):
# its own doctor's `snapshot` row reads `on · <age> · <size>` once the boot snapshot is written. (b) A
# throwaway `--rm` instance with DB_SNAPSHOT_EVERY=5s (the floor) proves the rest end to end: the boot
# upload, one clip, the debounced upload after its card is ready (the meta then counts it), `admin
# objects _clippet` listing the three objects under their own heading, `admin orphans` leaving them out,
# `admin restore --from-store` refused while the server runs — and, the server stopped, the recovery
# recipe itself: a NEW data directory, the bucket's keys and the verb, and `admin ls` shows the clip — on a
# stack with a bucket only. (c) runs on EVERY stack: a `--rm` instance on STORAGE_BACKEND=fs with its own
# media folder under /tmp (erratum 348 (a)): the boot snapshot and the debounced one land under the folder's
# own `_clippet/` beside the `.clippet-store` marker, and the recovery recipe is a NEW data directory, the
# same folder and the verb. Then erratum 348 (c): the restored database booted over an EMPTY folder (the
# shape of an unmounted share) writes no marker and no snapshot there, logs one ERROR and answers /readyz 503.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=SN_ buckets=-
# e2e-covers: cmd/clippet/snapshot.go cmd/clippet/admin_restore.go cmd/clippet/datalock.go cmd/clippet/datalock_unix.go
# e2e-covers: cmd/clippet/admin_objects.go internal/lifecycle/lifecycle.go
#   (a) is one read of the stack's own doctor. (b) is a `--rm` container with its own DATA_DIR and
#   SCRATCH_DIR (the README's rule for a probe that boots `clippet serve`), its own S3_KEY_PREFIX
#   `e2e-snap-<epoch>/` at the bucket ROOT — so its snapshot can never meet the stack's — removed by the
#   probe's own deletes and again, whole, by the host's EXIT trap (89-quota's pattern). Its one account and
#   one clip live in its own database, which goes with the container. No limiter, nothing instance-wide.
#
# Variables are prefixed SN_ so nothing here can clobber a harness global.

step 107 "the database snapshot follows the clips into the bucket: on by default, debounced, never an orphan, and the bucket alone brings the instance back"

# --- (a) on, on the stack under test -------------------------------------------------------------------
# The boot snapshot is written once the bucket has answered the ping, seconds after the stack came up; a
# bundle early in the pool may still beat it, so the row is polled.
SN_DOC="$E2E_DIR/snapshot-doctor.txt"
SN_N=0
while [ "$SN_N" -lt 60 ]; do
  dc exec -T app clippet admin doctor > "$SN_DOC" 2>&1 || true
  grep -q '^snapshot ok   on · ' "$SN_DOC" && break
  SN_N=$((SN_N + 1)); sleep 1
done
has_file "$SN_DOC" '^snapshot ok   on · ' "the stack under test (DB_SNAPSHOT unset) must have its snapshot on and written into its store"
echo "[e2e]   stack: $(grep '^snapshot' "$SN_DOC" | head -1)"
store_has_bucket || echo "[e2e]   NOTE: this stack keeps its media in a directory: (b) needs a bucket and is skipped; (c) runs"

# --- (b) the whole life of one, in a throwaway instance ---------------------------------------------------
if store_has_bucket; then
SN_PREFIX="e2e-snap-$(date +%s)/"
SN_OUT="$E2E_DIR/snapshot-probe.txt"
sn_rm_prefix() {
  store_rm_prefix_ok_ "${SN_PREFIX:-}" || return 0
  ( store_obj rm --prefix "$SN_PREFIX" >/dev/null ) 2>/dev/null \
    || echo "[e2e]   NOTE: could not remove the snapshot probe's prefix $SN_PREFIX (its own objects, outside the stack's key prefix)"
}
trap 'sn_rm_prefix' EXIT
echo "[e2e]   probe container: its own database, its own scratch, bucket prefix $SN_PREFIX"

SN_SH="$E2E_DIR/snapshot-probe.sh"
cat > "$SN_SH" <<'SNAP_PROBE'
set -e
mkdir -p /tmp/sn /tmp/sn-scratch /tmp/sn2 /tmp/sn2-scratch
written() { grep -c 'msg="db snapshot written"' /tmp/serve.log || true; }
up() { i=0; while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && return 0; i=$((i+1)); sleep 1; done; return 1; }

clippet migrate >/dev/null
clippet serve >/tmp/serve.log 2>&1 &
SP=$!
up || { echo "serveup=0"; cat /tmp/serve.log; exit 3; }
echo "bootline=$(grep 'msg=boot ' /tmp/serve.log | grep -o 'db_snapshot=[a-z]*' | head -1)"

# 1. the boot upload: nothing in this prefix yet, so it is written at once
i=0; while [ $i -lt 60 ]; do [ "$(written)" -ge 1 ] && break; i=$((i+1)); sleep 1; done
echo "boot=$(written)"

# 2. one clip, and the debounced upload after its card is ready (DB_SNAPSHOT_EVERY=5s)
ffmpeg -v error -y -f lavfi -i testsrc2=size=320x180:rate=15:duration=2 -c:v libx264 -preset veryfast -pix_fmt yuv420p /tmp/s.mp4
curl -sS -o /dev/null -c /tmp/jar -X POST -H "Content-Type: application/json" \
  -d '{"username":"snapuser","password":"snap-pass-12345"}' http://127.0.0.1:8080/api/auth/register
curl -sS -o /tmp/up -b /tmp/jar -F "title=snapshot clip" -F ttl_days=1 -F duplicate=allow \
  -F "file=@/tmp/s.mp4;type=video/mp4" http://127.0.0.1:8080/api/upload
SID=$(grep -o '"id":"[^"]*"' /tmp/up | head -1 | cut -d'"' -f4)
echo "id=$SID"
i=0; while [ $i -lt 120 ]; do [ "$(written)" -ge 2 ] && break; i=$((i+1)); sleep 1; done
echo "after=$(written)"
echo "afterwait=$i"

# 3. what the store holds under the reserved prefix, and what orphans says about it
clippet admin objects _clippet > /tmp/objects.txt 2>&1 || true
echo "objhead=$(head -1 /tmp/objects.txt)"
echo "objdb=$(grep -c '_clippet/db.sqlite ' /tmp/objects.txt || true)"
echo "objsum=$(grep -c '_clippet/db.sqlite.sha256 ' /tmp/objects.txt || true)"
echo "objmeta=$(grep -c '_clippet/db.sqlite.meta ' /tmp/objects.txt || true)"
echo "objcount=$(tail -1 /tmp/objects.txt)"
clippet admin orphans --min-age 0s > /tmp/orphans.txt 2>&1 || true
echo "orphansreserved=$(grep -c '_clippet' /tmp/orphans.txt || true)"
clippet admin doctor > /tmp/doc.txt 2>&1 || true
echo "doctor=$(grep '^snapshot' /tmp/doc.txt | head -1)"

# 4. a restore while the server runs is refused, and changes nothing
RC=0; clippet admin restore --from-store --yes > /tmp/r1.txt 2>&1 || RC=$?
echo "runningrc=$RC"
echo "running=$(tail -1 /tmp/r1.txt)"

kill $SP 2>/dev/null || true
wait $SP 2>/dev/null || true
echo "logerrors=$(grep -c -E 'panic|level=ERROR' /tmp/serve.log | tr -d ' ')"
echo "logwarns=$(grep -c 'msg="db snapshot failed"' /tmp/serve.log | tr -d ' ')"

# 5. the recovery recipe: a NEW data directory, the bucket's keys, the verb
export DATA_DIR=/tmp/sn2 SCRATCH_DIR=/tmp/sn2-scratch
clippet admin restore --from-store > /tmp/r2.txt 2>&1 || true
echo "dryrun=$(grep -c '^dry run:' /tmp/r2.txt || true)"
echo "drysha=$(grep -c 'sha256 ok' /tmp/r2.txt || true)"
echo "dryrows=$(grep '^rows:' /tmp/r2.txt | head -1)"
echo "drynodb=$(test -e /tmp/sn2/clippet.db && echo present || echo absent)"
clippet admin restore --from-store --yes > /tmp/r3.txt 2>&1 || true
echo "restored=$(grep -c '^restored:' /tmp/r3.txt || true)"
clippet admin ls "$SID" > /tmp/ls.txt 2>&1 || true
echo "lsclip=$(grep -c "^clip: *$SID" /tmp/ls.txt || true)"
echo "lstitle=$(grep -c 'snapshot clip' /tmp/ls.txt || true)"
echo "done=1"
SNAP_PROBE

SN_RC=0
dc run --rm -T --no-deps \
  -e "S3_KEY_PREFIX=$SN_PREFIX" -e DB_SNAPSHOT= -e DB_SNAPSHOT_EVERY=5s \
  -e DATA_DIR=/tmp/sn -e SCRATCH_DIR=/tmp/sn-scratch \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 -e LOG_FORMAT=text \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e REGISTRATION=open -e CAPTURE_READY_WAIT=0 \
  -e DISCORD_APP_ID= -e DISCORD_PUBLIC_KEY= -e DISCORD_BOT_TOKEN= -e DROP_FOLDER_OWNER= \
  --entrypoint sh app -c "$(cat "$SN_SH")" > "$SN_OUT" 2>&1 || SN_RC=$?
sed -n 's/^/[e2e]   snapshot-probe: /p' "$SN_OUT" | head -40
[ "$SN_RC" = 0 ] || die "the snapshot probe container failed (exit $SN_RC):
$(cat "$SN_OUT")"
has_file "$SN_OUT" '^done=1' "the snapshot probe did not run to the end"
sn_get() { sed -n "s/^$1=//p" "$SN_OUT" | head -1; }

expect "$(sn_get bootline)" db_snapshot=on "the boot line names db_snapshot=on on s3 with DB_SNAPSHOT unset"
[ "$(sn_get boot)" -ge 1 ] 2>/dev/null || die "no snapshot was written at boot into an empty prefix"
expect_nonempty "$(sn_get id)" "the probe clip's id"
[ "$(sn_get after)" -ge 2 ] 2>/dev/null || die "no debounced snapshot after the clip's card was ready (waited $(sn_get afterwait) s)"
contains "$(sn_get objhead)" "${SN_PREFIX}_clippet/ (this instance's own" || die "admin objects _clippet has no heading of its own: $(sn_get objhead)"
expect "$(sn_get objdb)" 1 "admin objects _clippet lists the snapshot"
expect "$(sn_get objsum)" 1 "admin objects _clippet lists the sha256 sidecar"
expect "$(sn_get objmeta)" 1 "admin objects _clippet lists the meta"
expect "$(sn_get objcount)" '3 object(s)' "the reserved prefix holds exactly three objects"
expect "$(sn_get orphansreserved)" 0 "admin orphans named the reserved prefix"
contains "$(sn_get doctor)" 'snapshot ok   on · ' || die "the probe's doctor row: $(sn_get doctor)"
expect "$(sn_get runningrc)" 1 "admin restore while the server runs"
contains "$(sn_get running)" 'the server is running on this database' || die "the refusal's words: $(sn_get running)"
expect "$(sn_get logerrors)" 0 "ERROR or panic lines in the probe server's log"
expect "$(sn_get logwarns)" 0 "a snapshot failed on the probe instance"
expect "$(sn_get dryrun)" 1 "restore without --yes is a dry run"
expect "$(sn_get drysha)" 1 "the snapshot's sha256 matches its sidecar"
contains "$(sn_get dryrows)" 'clips=1' || die "the snapshot does not carry the clip: $(sn_get dryrows)"
expect "$(sn_get drynodb)" absent "the dry run created a database"
expect "$(sn_get restored)" 1 "restore --from-store --yes into a new data directory"
expect "$(sn_get lsclip)" 1 "the restored database lists the clip"
expect "$(sn_get lstitle)" 1 "…with its title"
sn_rm_prefix
trap - EXIT
echo "[e2e]   probe: boot + debounced uploads, three objects under _clippet/, never an orphan, restore refused under a running server, the clip back from the bucket alone"
fi
# --- (c) the folder home's snapshot, in a throwaway instance on STORAGE_BACKEND=fs (erratum 348) --------
# Everything it writes is under its own container's /tmp — a private DATA_DIR, SCRATCH_DIR and media
# folder — so nothing reaches the stack's store or database, and the container takes it all with it.
FN_OUT="$E2E_DIR/snapshot-fs-probe.txt"
FN_SH="$E2E_DIR/snapshot-fs-probe.sh"
cat > "$FN_SH" <<'FS_PROBE'
set -e
M=/tmp/fsn-media
mkdir -p /tmp/fsn /tmp/fsn-scratch /tmp/fsn2 /tmp/fsn2-scratch /tmp/fsn-empty
written() { grep -c 'msg="db snapshot written"' /tmp/serve.log || true; }
up() { i=0; while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && return 0; i=$((i+1)); sleep 1; done; return 1; }

clippet migrate >/dev/null
clippet serve >/tmp/serve.log 2>&1 &
SP=$!
up || { echo "serveup=0"; cat /tmp/serve.log; exit 3; }
echo "bootline=$(grep 'msg=boot ' /tmp/serve.log | grep -o 'db_snapshot=[a-z]*' | head -1)"
i=0; while [ $i -lt 60 ]; do [ "$(written)" -ge 1 ] && break; i=$((i+1)); sleep 1; done
echo "boot=$(written)"

ffmpeg -v error -y -f lavfi -i testsrc2=size=320x180:rate=15:duration=2 -c:v libx264 -preset veryfast -pix_fmt yuv420p /tmp/s.mp4
curl -sS -o /dev/null -c /tmp/jar -X POST -H "Content-Type: application/json" \
  -d '{"username":"fsnapuser","password":"snap-pass-12345"}' http://127.0.0.1:8080/api/auth/register
curl -sS -o /tmp/up -b /tmp/jar -F "title=folder snapshot clip" -F ttl_days=1 -F duplicate=allow \
  -F "file=@/tmp/s.mp4;type=video/mp4" http://127.0.0.1:8080/api/upload
SID=$(grep -o '"id":"[^"]*"' /tmp/up | head -1 | cut -d'"' -f4)
echo "id=$SID"
i=0; while [ $i -lt 120 ]; do [ "$(written)" -ge 2 ] && break; i=$((i+1)); sleep 1; done
echo "after=$(written)"
echo "afterwait=$i"

# the three files under the folder's own _clippet/, the marker at its root, and nothing else of ours there
for f in db.sqlite db.sqlite.sha256 db.sqlite.meta; do [ -f "$M/_clippet/$f" ] && echo "file_$f=1" || echo "file_$f=0"; done
echo "marker=$([ -f "$M/.clippet-store" ] && echo 1 || echo 0)"
echo "markerinreserved=$([ -e "$M/_clippet/.clippet-store" ] && echo 1 || echo 0)"
echo "metaclips=$(grep -o '"clips":[0-9]*' "$M/_clippet/db.sqlite.meta" | cut -d: -f2)"
clippet admin objects _clippet --count > /tmp/oc.txt 2>&1 || true
echo "objcount=$(tr -d ' \r' < /tmp/oc.txt | tail -1)"
clippet admin doctor > /tmp/doc.txt 2>&1 || true
echo "doctor=$(grep '^snapshot' /tmp/doc.txt | head -1)"
kill $SP 2>/dev/null || true
wait $SP 2>/dev/null || true
echo "logerrors=$(grep -c -E 'panic|level=ERROR' /tmp/serve.log | tr -d ' ')"
echo "logwarns=$(grep -c 'msg="db snapshot failed"' /tmp/serve.log | tr -d ' ')"

# the recovery recipe on a folder home: a NEW data directory, the same folder, the verb
export DATA_DIR=/tmp/fsn2 SCRATCH_DIR=/tmp/fsn2-scratch
clippet admin restore --from-store > /tmp/r2.txt 2>&1 || true
echo "dryrun=$(grep -c '^dry run:' /tmp/r2.txt || true)"
echo "drysha=$(grep -c 'sha256 ok' /tmp/r2.txt || true)"
echo "dryrows=$(grep '^rows:' /tmp/r2.txt | head -1)"
clippet admin restore --from-store --yes > /tmp/r3.txt 2>&1 || true
echo "restored=$(grep -c '^restored:' /tmp/r3.txt || true)"
clippet admin ls "$SID" > /tmp/ls.txt 2>&1 || true
echo "lsclip=$(grep -c "^clip: *$SID" /tmp/ls.txt || true)"

# erratum 348 (c): that database over an EMPTY folder — the shape of a share that did not mount
export STORAGE_DIR=/tmp/fsn-empty
clippet serve >/tmp/serve2.log 2>&1 &
SP=$!
up || { echo "serve2up=0"; cat /tmp/serve2.log; exit 4; }
echo "readyz=$(curl -sS -o /tmp/ready.json -w '%{http_code}' http://127.0.0.1:8080/readyz)"
echo "readybody=$(tr -d '\n' < /tmp/ready.json)"
sleep 2
echo "emptymarker=$([ -e /tmp/fsn-empty/.clippet-store ] && echo 1 || echo 0)"
echo "emptysnap=$([ -e /tmp/fsn-empty/_clippet ] && echo 1 || echo 0)"
echo "unmarkederror=$(grep -c 'level=ERROR.*the marker was NOT written' /tmp/serve2.log || true)"
kill $SP 2>/dev/null || true
wait $SP 2>/dev/null || true
echo "done=1"
FS_PROBE

FN_RC=0
dc run --rm -T --no-deps \
  -e STORAGE_BACKEND=fs -e STORAGE_DIR=/tmp/fsn-media \
  -e S3_ENDPOINT= -e S3_BUCKET= -e S3_ACCESS_KEY= -e S3_SECRET_KEY= -e S3_KEY_PREFIX= \
  -e DB_SNAPSHOT= -e DB_SNAPSHOT_EVERY=5s \
  -e DATA_DIR=/tmp/fsn -e SCRATCH_DIR=/tmp/fsn-scratch \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 -e LOG_FORMAT=text \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e REGISTRATION=open -e CAPTURE_READY_WAIT=0 -e STORAGE_PING_ATTEMPTS=2 -e STORAGE_PING_INTERVAL=1s \
  -e DISCORD_APP_ID= -e DISCORD_PUBLIC_KEY= -e DISCORD_BOT_TOKEN= -e DROP_FOLDER_OWNER= \
  --entrypoint sh app -c "$(cat "$FN_SH")" > "$FN_OUT" 2>&1 || FN_RC=$?
sed -n 's/^/[e2e]   snapshot-fs-probe: /p' "$FN_OUT" | head -40
[ "$FN_RC" = 0 ] || die "the folder snapshot probe container failed (exit $FN_RC):
$(cat "$FN_OUT")"
has_file "$FN_OUT" '^done=1' "the folder snapshot probe did not run to the end"
fn_get() { sed -n "s/^$1=//p" "$FN_OUT" | head -1; }

expect "$(fn_get bootline)" db_snapshot=on "the boot line names db_snapshot=on on STORAGE_BACKEND=fs with DB_SNAPSHOT unset"
[ "$(fn_get boot)" -ge 1 ] 2>/dev/null || die "no snapshot was written at boot into an empty media folder"
expect_nonempty "$(fn_get id)" "the folder probe clip's id"
[ "$(fn_get after)" -ge 2 ] 2>/dev/null || die "no debounced snapshot after the folder clip's card was ready (waited $(fn_get afterwait) s)"
expect "$(fn_get file_db.sqlite)" 1 "the snapshot is under the media folder's _clippet/"
expect "$(fn_get file_db.sqlite.sha256)" 1 "…with its sha256 sidecar"
expect "$(fn_get file_db.sqlite.meta)" 1 "…and its meta"
expect "$(fn_get marker)" 1 "the store marker at the folder's root"
expect "$(fn_get markerinreserved)" 0 "a marker under _clippet/ (the marker and the snapshot must not meet)"
expect "$(fn_get metaclips)" 1 "the folder snapshot's meta counts the clip"
expect "$(fn_get objcount)" 3 "admin objects _clippet --count on the folder store"
contains "$(fn_get doctor)" 'snapshot ok   on · ' || die "the folder probe's doctor row: $(fn_get doctor)"
expect "$(fn_get logerrors)" 0 "ERROR or panic lines in the folder probe server's log"
expect "$(fn_get logwarns)" 0 "a snapshot failed on the folder probe instance"
expect "$(fn_get dryrun)" 1 "restore --from-store on a folder home without --yes is a dry run"
expect "$(fn_get drysha)" 1 "the folder snapshot's sha256 matches its sidecar"
contains "$(fn_get dryrows)" 'clips=1' || die "the folder snapshot does not carry the clip: $(fn_get dryrows)"
expect "$(fn_get restored)" 1 "restore --from-store --yes from the folder into a new data directory"
expect "$(fn_get lsclip)" 1 "the database restored from the folder lists the clip"
expect "$(fn_get readyz)" 503 "/readyz on a database with clips over an empty, unmarked folder"
contains "$(fn_get readybody)" '"storage":"error: unavailable (fs)"' || die "the unmarked folder's /readyz body: $(fn_get readybody)"
expect "$(fn_get emptymarker)" 0 "the marker was written over a folder the database says holds clips"
expect "$(fn_get emptysnap)" 0 "a snapshot was written into a folder that is not this database's"
expect "$(fn_get unmarkederror)" 1 "the ERROR line naming the unwritten marker"
echo "[e2e]   folder probe: boot + debounced snapshots under the folder's _clippet/ beside its marker, restore from the folder alone brought the clip back; over an empty folder no marker, no snapshot, one ERROR, /readyz 503"
summary_row SN snapshot "db snapshot (errata 343, 348): on by default on s3 AND fs, _clippet/db.sqlite + sha256 + meta in the bucket or the media folder, debounced, restore --from-store brings the clip back in a new data directory; an unmarked folder over stored clips gets no marker and serves degraded"
