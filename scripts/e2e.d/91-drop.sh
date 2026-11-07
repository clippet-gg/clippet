# 91-drop.sh — the DROP FOLDER (ROADMAP #42 (b), §20 erratum 296): a video left at the ROOT of an
# `fs` instance's media directory becomes a clip owned by ONE configured account. One throwaway app
# container with STORAGE_BACKEND=fs and DROP_FOLDER_OWNER set, driven end to end — the doctor's row,
# a finished file taken, a still-growing one left alone, an unreadable one refused with a note beside
# it, and a RESTART that re-ingests nothing and never writes over an orphan left in the staging
# directory by a process that was killed mid-ingest.
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 91. Everything it makes lives inside its own container and goes away with it.
#
# e2e-schedule: parallel
# e2e-namespace: account=dr prefix=DR_ buckets=-
# e2e-covers: internal/drop/** internal/db/drop.go internal/httpapi/drop.go
#   It starts a SECOND app process with its own published port, its own database and its own media
#   directory, and every assertion below is about THAT container — the same reasoning 88-fsstore.sh
#   records (§20 erratum 283 (e)). Its published port is derived from `CLIPPET_PUBLISH` + 201 and is
#   claimed by nothing else in the suite (88-fsstore takes +200; the two other bundles that publish
#   one, 37-library and 55-owner, are exclusive and therefore never concurrent with a lane).
#
# What it touches on THIS stack: nothing at all. Not the database, not the store, not a clip — not
# even the one `BucketExists` 88-fsstore asks, because nothing here migrates. DATA_DIR, SCRATCH_DIR
# and STORAGE_DIR are three private siblings under /tmp inside the container's writable layer (the
# third convention in scripts/e2e.d/README.md: a `clippet serve` on a private database over the LIVE
# /scratch deletes the real working directories), and the container is removed at the end and on any
# failure (trap).
#
# Why a container of its own rather than the stack: the owner must EXIST before the process boots (a
# name that matches none is a boot refusal by design), and the whole feature is about files appearing
# at the root of a directory this app owns. Neither is something to do to a stack other steps are using.
#
# The folder is `fs`-shaped HERE and not by rule: since §20 erratum 296 (m) it runs on either backend —
# what it needs is a directory, which `s3` can have too. The live legs of that change are the doctor's
# own row on a bucket instance and the refusal when no directory is named, both in (f) below; the
# INGEST on a bucket is pinned in Go (internal/httpapi's two bucket drop tests) rather than here,
# because a throwaway container that really wrote into the gate stack's bucket would need that stack's
# credentials — which a drop-in may not read from `.env` — and would leave objects behind on R2.
#
# Variables are prefixed DR_ so nothing here can clobber a harness global.

# ---------------------------------------------------------------------------------------------------
step 91 "the drop folder (#42): a video left at the root of the media directory becomes a clip, once"

DR_PORT=$(envfile CLIPPET_PUBLISH); DR_PORT=${DR_PORT##*:}; DR_PORT=${DR_PORT:-8481}
DR_PORT=$((DR_PORT + 201))
DR_BASE="http://localhost:$DR_PORT"
DR_CT="clippet-e2e-drop-$DR_PORT"
DR_USER="e2e_$(date +%s)_dr"
DR_OUT="$E2E_DIR/drop.txt"
DR_MEDIA=/tmp/media

dr_stop() {
  docker stop -t 20 "$DR_CT" >/dev/null 2>&1 || true
  docker rm -f "$DR_CT" >/dev/null 2>&1 || true
}
dr_stop
# Armed BEFORE the container exists, so a failure anywhere below still removes it — a second app
# process left running against a published port is the one thing this step must not leave.
trap 'dr_stop' EXIT

# dr_sh <shell-line…> — run a command inside the container with the bucket and delivery variables
# UNSET, for the reasons 88-fsstore.sh's own helper records: `docker exec` inherits the environment
# the container was CREATED with, compose interpolates all seven for every container of this project,
# and on `fs` a leftover S3_* is a boot error while `redirect` is refused outright.
dr_sh() { docker_ exec "$DR_CT" sh -c "unset S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT; $*"; }

# dr_verdict <output-file> <check> → the WORST verdict that check printed. 51-admin.sh's reader,
# written again here on purpose: scripts/e2e.d/README.md's second convention (and a scripts/check.sh
# stage) forbids one drop-in calling a helper another one defines.
dr_verdict() {
  awk -v c="$2" 'BEGIN{r["ok"]=1;r["warn"]=2;r["skip"]=3;r["FAIL"]=4;b=0;s=""}
    $1==c && r[$2]>b {b=r[$2]; s=$2} END{print s}' "$1"
}
# dr_root → what is at the root of the media directory right now, one name per line.
dr_root() { dr_sh "ls -A $DR_MEDIA" 2>/dev/null | tr -d '\r'; }
# dr_wait_ready <seconds> — poll until the instance answers /readyz.
dr_wait_ready() {
  DR_N=0
  while [ "$DR_N" -lt "$1" ]; do
    code=$(get "$DR_BASE/readyz" 2>/dev/null || echo 000)
    [ "$code" = 200 ] && return 0
    DR_N=$((DR_N + 1)); sleep 1
  done
  return 1
}

# --- (a) the instance, with an owner that exists ---------------------------------------------------
# The account is created BEFORE `serve` in the same container, because the boot refusal under test is
# exactly "DROP_FOLDER_OWNER names no account" — an operator does the same two commands in the same
# order, and the refusal itself is asserted in (f) below.
dc run -d --name "$DR_CT" --no-deps -p "127.0.0.1:$DR_PORT:8080" \
  -e "PUBLIC_BASE_URL=$DR_BASE" -e REGISTRATION=open -e SWEEP_INTERVAL=5s -e SWEEP_GRACE=1s \
  -e DATA_DIR=/tmp/drdata -e SCRATCH_DIR=/tmp/drscratch -e "STORAGE_DIR=$DR_MEDIA" \
  -e STORAGE_BACKEND=fs -e "DROP_FOLDER_OWNER=$DR_USER" -e "DR_PW=$E2E_PASS" \
  --entrypoint sh app -c '
    set -e
    unset S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY
    unset DISCORD_WEBHOOK_URL DISCORD_WEBHOOK_ROUTES DISCORD_APP_ID DISCORD_PUBLIC_KEY DISCORD_BOT_TOKEN
    unset MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT
    clippet migrate
    # Both of these run again on a RESTART (the container command is re-executed,
    # which is what (e) below depends on), so both have to be idempotent: migrate
    # already is, and a second `users create` is a loud exit 1 that `set -e` would
    # turn into a container that never comes back.
    printf %s "$DR_PW" | clippet admin users create "$DROP_FOLDER_OWNER" \
      || echo "91-drop: the owner account already exists (this is a restart)"
    exec clippet serve
  ' >/dev/null || die "could not start the drop-folder container"
dr_wait_ready 90 || die "the drop container never became ready:
$(docker_ logs --tail 40 "$DR_CT" 2>&1)"
echo "[e2e]   drop app up on $DR_BASE after ${DR_N}s, owner $DR_USER"

# The boot line says the folder is watched, for whom, and how often. An operator who turned this on
# and sees nothing happen reads this line first.
docker_ logs "$DR_CT" > "$DR_OUT" 2>&1
has_file "$DR_OUT" 'drop folder watching' "the boot log must say the folder is being watched"
has_file "$DR_OUT" "owner=$DR_USER" "the boot line must name the account it ingests as"

# --- (b) the doctor's row --------------------------------------------------------------------------
dr_sh clippet admin doctor > "$DR_OUT" 2>&1 || die "admin doctor on the drop instance exited non-zero:
$(cat "$DR_OUT")"
sed -n 's/^drop/[e2e]   drop/p' "$DR_OUT"
expect "$(dr_verdict "$DR_OUT" drop)" ok "doctor's 'drop' row on a working drop instance"
has_file "$DR_OUT" "owned by $DR_USER" "the drop row must name the account"
has_file "$DR_OUT" "ROOT of $DR_MEDIA" "the drop row must name the folder whose root is watched"
has_file "$DR_OUT" '0 file(s) waiting' "the drop row must report the folder's state (empty here)"

# --- (c) a finished file becomes a clip, and leaves the root ---------------------------------------
# The bytes are piped in over `docker exec -i`, never `docker cp`: the harness runs with MSYS path
# conversion ON (scripts/e2e.sh's own note), and a `ct:/tmp/…` argument is exactly the shape it
# rewrites. A redirect from the host shell carries a RELATIVE path and nothing rewrites it.
#
# It is written to /tmp/gen first and RENAMED in, which is what a capture tool doing the right thing
# does — and what makes this step about the drop folder rather than about how fast a copy runs.
dr_sh "mkdir -p /tmp/gen" >/dev/null
docker_ exec -i "$DR_CT" sh -c 'cat > /tmp/gen/drop.mp4' < testclips/std-720p30.mp4 \
  || die "could not put the fixture into the drop container"
# The still-growing file: a background appender inside the container keeps its size moving for two
# minutes, which is longer than everything below. A file whose size has not been still for a whole
# poll is never taken, whatever else is true of it.
docker_ exec -d "$DR_CT" sh -c "i=0; while [ \$i -lt 60 ]; do printf growing >> $DR_MEDIA/growing.mp4; i=\$((i+1)); sleep 2; done" \
  || die "could not start the appender for the still-growing file"
# …and the `.part` rule, which needs no clock: a name a download manager is still writing.
docker_ exec -i "$DR_CT" sh -c "cat > $DR_MEDIA/half.mp4.part" < testclips/std-720p30.mp4 \
  || die "could not plant the .part file"
dr_sh "mv /tmp/gen/drop.mp4 '$DR_MEDIA/E2E drop clip.mp4'" || die "could not plant the dropped file"
echo "[e2e]   planted: 'E2E drop clip.mp4' (finished), growing.mp4 (still being written), half.mp4.part"

DR_N=0
while [ "$DR_N" -lt 120 ]; do
  DR_LS=$(dr_sh "clippet admin ls --owner $DR_USER" 2>&1 || true)
  case $DR_LS in
    *"1 clips"*) break ;;
  esac
  DR_N=$((DR_N + 1)); sleep 2
done
case $DR_LS in
  *"1 clips"*) ;;
  *) die "the dropped file did not become a clip within $((DR_N * 2))s:
$DR_LS
--- the folder ---
$(dr_root)
--- the log ---
$(docker_ logs --tail 30 "$DR_CT" 2>&1)" ;;
esac
DR_ID=$(printf '%s\n' "$DR_LS" | awk 'NR==2 {print $1}')
expect_nonempty "$DR_ID" "the dropped clip's id"
echo "[e2e]   the dropped file became clip $DR_ID in about $((DR_N * 2))s, owned by $DR_USER"

# The TITLE is the file name without its extension — erratum 60's rule for every other upload, and
# the only thing anybody typed here.
case $DR_LS in
  *"E2E drop clip"*) ;;
  *) die "the clip's title is not the file name without its extension:
$DR_LS" ;;
esac
# It is gone from the root: the folder is where you PUT things, not where they pile up.
DR_ROOT=$(dr_root)
case $DR_ROOT in
  *"E2E drop clip.mp4"*) die "the ingested file is still at the root of the folder:
$DR_ROOT" ;;
esac
# …and it is not sitting in the staging directory either: a successful ingest leaves nothing behind.
DR_STAGED=$(dr_sh "ls -A $DR_MEDIA/.clippet-drop 2>/dev/null" || true)
[ -z "$DR_STAGED" ] || die "a file was left in the staging directory after a successful ingest: $DR_STAGED"
# The two that must be untouched.
case $DR_ROOT in
  *growing.mp4*) ;;
  *) die "the still-growing file was taken:
$DR_ROOT" ;;
esac
case $DR_ROOT in
  *half.mp4.part*) ;;
  *) die "the .part file was taken:
$DR_ROOT" ;;
esac
echo "[e2e]   the folder now holds: $(echo "$DR_ROOT" | tr '\n' ' ')"

# The log names the ingest at INFO with the clip id (the operator's record of what arrived).
docker_ logs "$DR_CT" > "$DR_OUT" 2>&1
has_file "$DR_OUT" "drop folder: ingested" "the log must name each ingest"
has_file "$DR_OUT" "clip=$DR_ID" "the ingest line must carry the clip id"

# It is an ordinary clip: it encodes and it plays.
DR_N=0
while [ "$DR_N" -lt 180 ]; do
  code=$(get "$DR_BASE/api/clips/$DR_ID/progress"); expect "$code" 200 "drop progress"
  DR_STATE=$(json state)
  case $DR_STATE in
    ready) break ;;
    failed) die "the dropped clip failed: $(json error)" ;;
  esac
  DR_N=$((DR_N + 1)); sleep 1
done
expect "$DR_STATE" ready "the dropped clip reached ready within ${DR_N}s"
DR_WHOLE=$(curl_ -sS -o /dev/null -w '%{http_code} %{size_download}' "$DR_BASE/api/clips/$DR_ID/video.mp4")
expect "${DR_WHOLE%% *}" 200 "GET the dropped clip's rendition"
[ "${DR_WHOLE##* }" -gt 10000 ] 2>/dev/null || die "the dropped clip delivered ${DR_WHOLE##* } bytes"
echo "[e2e]   the dropped clip encoded in ${DR_N}s and serves its rendition"

# --- (d) a file this process cannot read is REFUSED, in writing, where the person will see it -------
# Planted as root with no permissions: the image runs as uid 10001, so this is the one rejection that
# is about the FILE rather than about what the file IS — and silence would leave somebody watching a
# folder that never empties.
docker_ exec -u 0 -i "$DR_CT" sh -c "cat > $DR_MEDIA/locked.mp4 && chmod 000 $DR_MEDIA/locked.mp4" < testclips/std-720p30.mp4 \
  || die "could not plant the unreadable file"
DR_N=0
while [ "$DR_N" -lt 60 ]; do
  dr_sh "test -f '$DR_MEDIA/locked.mp4.failed.txt'" && break
  DR_N=$((DR_N + 1)); sleep 1
done
dr_sh "test -f '$DR_MEDIA/locked.mp4.failed.txt'" || die "an unreadable file got no .failed.txt within ${DR_N}s:
$(dr_root)
$(docker_ logs --tail 20 "$DR_CT" 2>&1)"
DR_NOTE=$(dr_sh "cat '$DR_MEDIA/locked.mp4.failed.txt'")
case $DR_NOTE in
  *"could not be opened"*) ;;
  *) die "the note does not say what happened: $DR_NOTE" ;;
esac
case $DR_NOTE in
  *"left exactly where it was"*) ;;
  *) die "the note does not say the file was left alone: $DR_NOTE" ;;
esac
dr_sh "test -f '$DR_MEDIA/locked.mp4'" || die "the unreadable file was moved anyway"
echo "[e2e]   the unreadable file was refused in writing and left where it was"
# Still exactly one clip: a refusal creates nothing.
DR_LS=$(dr_sh "clippet admin ls --owner $DR_USER" 2>&1 || true)
case $DR_LS in
  *"1 clips"*) ;;
  *) die "a refused file changed the clip count:
$DR_LS" ;;
esac

# The doctor sees the same folder the person does.
dr_sh clippet admin doctor > "$DR_OUT" 2>&1 || die "admin doctor after the refusal exited non-zero:
$(cat "$DR_OUT")"
has_file "$DR_OUT" "1 with a .failed.txt" "the drop row must count the refusals it wrote"

# --- (e) a RESTART re-ingests nothing ---------------------------------------------------------------
# The window this closes: the watcher moves a file aside, ingests it, records the clip, and only then
# removes the staged copy. A process killed inside that window leaves a staged file the next boot puts
# back at the root — and without migration 0018's memory it would become a second clip of the same
# video, every restart, for ever. Here the file is already gone, and what the restart must not do is
# re-ingest `locked.mp4` (decided, and still at the root) or take the growing file's leftovers.
#
# The SAME restart carries the other half of that window (§20 erratum 296 (f)): an orphan left in the
# staging directory whose name has since been taken at the root. The boot pass refuses to put it back —
# and the ticks that follow must not quietly rename the root file over it, because `os.Rename` replaces
# its destination and the successful ingest would then remove the result. That is somebody's only copy
# of the video the earlier run was killed in the middle of.
dr_sh "mkdir -p '$DR_MEDIA/.clippet-drop'" >/dev/null || die "could not make the staging directory"
docker_ exec -i "$DR_CT" sh -c "cat > '$DR_MEDIA/orphan.mp4'" < testclips/std-720p30.mp4 \
  || die "could not plant the root half of the staged collision"
dr_sh "printf %s 'the-orphan-bytes' > '$DR_MEDIA/.clippet-drop/orphan.mp4'" \
  || die "could not plant the staged orphan"
echo "[e2e]   planted a staged orphan and a root file of the same name (orphan.mp4)"
docker_ stop -t 20 "$DR_CT" >/dev/null || die "could not stop the drop container"
docker_ start "$DR_CT" >/dev/null || die "could not restart the drop container"
dr_wait_ready 90 || die "the drop container did not come back:
$(docker_ logs --tail 40 "$DR_CT" 2>&1)"
sleep 15   # three poll intervals: long enough for anything it was going to take
DR_LS=$(dr_sh "clippet admin ls --owner $DR_USER" 2>&1 || true)
case $DR_LS in
  *"1 clips"*) ;;
  *) die "the restart changed what the account owns (it must re-ingest NOTHING):
$DR_LS" ;;
esac
dr_sh "test -f '$DR_MEDIA/locked.mp4'" || die "the restart moved the file it had already refused"
echo "[e2e]   after a restart: still one clip, and the refused file is untouched"

# The staged orphan is still there, byte for byte, and so is the file at the root: neither was written
# over, and the clip count above already proved the root file was not taken. `1 clips` is the assertion
# that would have caught the loss too — the old code ingested the root file, so the count went to 2
# while the orphan's bytes went away with it.
DR_ORPHAN=$(dr_sh "cat '$DR_MEDIA/.clippet-drop/orphan.mp4' 2>/dev/null" | tr -d '\r')
expect "$DR_ORPHAN" "the-orphan-bytes" "the staged orphan survives the ticks after the boot pass"
dr_sh "test -f '$DR_MEDIA/orphan.mp4'" || die "the file at the root was staged over the orphan of its own name"
docker_ logs "$DR_CT" > "$DR_OUT" 2>&1
has_file "$DR_OUT" 'cannot go back' "the boot pass must say why the orphan stayed staged"
has_file "$DR_OUT" 'already staged' "the tick must say why the file at the root is being left alone"
echo "[e2e]   the staged orphan and the root file of its name were both left alone"

# --- (f) the boot refusal: an owner that names no account -------------------------------------------
# A folder watched for nobody is the one state this feature must never be in, so it is a config error
# like a bad DISCORD_APP_ID rather than a warning nobody reads.
DR_RC=0
docker_ exec -e DROP_FOLDER_OWNER=nosuchaccount "$DR_CT" sh -c '
  unset S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT
  clippet admin doctor' > "$DR_OUT" 2>&1 || DR_RC=$?
expect "$DR_RC" 1 "admin doctor with a DROP_FOLDER_OWNER that names no account"
expect "$(dr_verdict "$DR_OUT" drop)" FAIL "doctor's 'drop' row for an account that does not exist"
has_file "$DR_OUT" 'names no account' "the failure must say what is wrong"
has_file "$DR_OUT" 'refuse to start' "the failure must say what serve will do about it"
# And what the folder really needs is a DIRECTORY, not a backend (§20 erratum 296 (m), lane `import`).
# On `s3` with nowhere to watch the knob is refused, naming the variable that supplies one — it used to
# be refused for being on `s3` at all, which sent the operator to the wrong fix.
DR_RC=0
docker_ exec -e DROP_FOLDER_OWNER=someone -e STORAGE_BACKEND=s3 -e STORAGE_DIR= \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e S3_ENDPOINT=http://minio:9000 -e S3_BUCKET=clips -e S3_ACCESS_KEY=ak -e S3_SECRET_KEY=sk \
  "$DR_CT" clippet admin stats > "$DR_OUT" 2>&1 || DR_RC=$?
expect "$DR_RC" 2 "DROP_FOLDER_OWNER with no media directory must refuse to start"
has_file "$DR_OUT" 'DROP_FOLDER_OWNER' "the refusal must name the variable"
has_file "$DR_OUT" 'STORAGE_DIR' "the refusal must name the variable that supplies the folder"
# …and on `s3` WITH one it is on, and the doctor's row reads exactly as it does on `fs`. The bucket
# here is unreachable on purpose (nothing on this stack answers at that endpoint since the media moved),
# so only the `drop` row's own verdict is asserted — which is the point: the backend no longer decides.
dr_sh "mkdir -p /tmp/s3media" >/dev/null || die "could not make the s3 instance's drop folder"
docker_ exec -e DROP_FOLDER_OWNER="$DR_USER" -e STORAGE_BACKEND=s3 -e STORAGE_DIR=/tmp/s3media \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e S3_ENDPOINT=http://minio:9000 -e S3_BUCKET=clips -e S3_ACCESS_KEY=ak -e S3_SECRET_KEY=sk \
  "$DR_CT" clippet admin doctor > "$DR_OUT" 2>&1 || true
sed -n 's/^drop/[e2e]   drop (s3)/p' "$DR_OUT"
expect "$(dr_verdict "$DR_OUT" drop)" ok "doctor's 'drop' row on a BUCKET instance with a media directory"
has_file "$DR_OUT" "ROOT of /tmp/s3media" "the drop row on s3 must name the folder whose root is watched"

dr_stop
trap - EXIT
summary_row DR "$DR_ID" "drop folder (#42): a video left at the root of an fs instance's media directory became a clip owned by the configured account and left the root; a still-growing file and a .part were untouched; an unreadable file got a .failed.txt and stayed; a restart re-ingested nothing and left a staged orphan and the root file of its name both untouched; the doctor's drop row on, its boot refusals, and the same row on a BUCKET instance with a media directory (erratum 296 (m)); container removed"
