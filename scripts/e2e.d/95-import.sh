# 95-import.sh — `clippet admin import <dir>` (ROADMAP #51, §20 erratum 307): a one-time walk of a
# folder of existing videos through this app's own upload path, idempotent by CONTENT. One throwaway
# app container with a private database, scratch and media directory, driven through `docker exec` —
# three files planted (two videos, one that is not), the verb run TWICE, and the folder checked
# afterwards for the one thing this verb promises: that it read the files and changed nothing in it.
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 95. Everything it makes lives inside its own container and goes away with it.
#
# e2e-schedule: parallel
# e2e-schedule-apart: 91-drop
# e2e-namespace: account=im prefix=IM_ buckets=-
# e2e-covers: cmd/clippet/admin_import.go internal/httpapi/importer.go internal/db/imports.go
#   It starts a second app PROCESS — `clippet migrate` and `clippet admin`, never `clippet serve` —
#   with its own database, its own scratch and its own media directory, and every assertion below is
#   about THAT container. It publishes NO port (nothing here is served over HTTP), so unlike
#   88-fsstore and 91-drop it claims nothing from the port space at all. It is APART from 91-drop, the
#   other bundle that drives admin verbs in a container of its own: the two never ran together while
#   they shared lane 4, and lane `speed2` kept that rather than argue it away.
#
# What it touches on THIS stack: nothing. Not the database, not the store, not a clip. DATA_DIR,
# SCRATCH_DIR and STORAGE_DIR are three private siblings under /tmp inside the container's writable
# layer — the third convention in scripts/e2e.d/README.md, and it bites harder here than usual: the
# import SPOOLS every file it takes into SCRATCH_DIR and leaves the spool there for a worker, so an
# import over the LIVE /scratch would plant working directories for clips that exist only in this
# container's database, which the stack's own janitor would then remove (§12's "row missing").
#
# Why a container of its own rather than the stack: the verb's whole subject is a folder somebody
# else owns, and the second half of its contract is a CLIP COUNT that must not move. Both are things
# to do to a database nobody else is using.
#
# Variables are prefixed IM_ so nothing here can clobber a harness global.

# ---------------------------------------------------------------------------------------------------
step 95 "admin import (#51): a folder of old clips walks through the upload path once, and only once"

IM_PORT=$(envfile CLIPPET_PUBLISH); IM_PORT=${IM_PORT##*:}; IM_PORT=${IM_PORT:-8481}
IM_CT="clippet-e2e-import-$IM_PORT"
IM_USER="e2e_$(date +%s)_im"
IM_OUT="$E2E_DIR/import.txt"
IM_DIR=/tmp/incoming

im_stop() {
  docker stop -t 20 "$IM_CT" >/dev/null 2>&1 || true
  docker rm -f "$IM_CT" >/dev/null 2>&1 || true
}
im_stop
# Armed BEFORE the container exists, so a failure anywhere below still removes it.
trap 'im_stop' EXIT

# im_sh <shell-line…> — run a command inside the container with the bucket and delivery variables
# UNSET, for the reasons 88-fsstore.sh's and 91-drop.sh's own helpers record: `docker exec` inherits
# the environment the container was CREATED with, compose interpolates all seven for every container
# of this project, and on `fs` a leftover S3_* is a boot error while `redirect` is refused outright.
im_sh() { docker_ exec "$IM_CT" sh -c "unset S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT; $*"; }

# im_count <verdict> — how many lines of the last run began with that verdict word. `grep -c` prints
# 0 and EXITS 1 when it matches nothing, which is the ordinary answer here and not a failure.
im_count() { IM_C=$(grep -c "^$1 " "$IM_OUT" 2>/dev/null || true); printf '%s' "${IM_C:-0}"; }

# --- (a) an instance of its own, with an account and an empty folder to import from ---------------
dc run -d --name "$IM_CT" --no-deps \
  -e DATA_DIR=/tmp/imdata -e SCRATCH_DIR=/tmp/imscratch -e STORAGE_DIR=/tmp/immedia \
  -e STORAGE_BACKEND=fs -e "IM_PW=$E2E_PASS" -e "IM_USER=$IM_USER" -e "IM_DIR=$IM_DIR" \
  --entrypoint sh app -c '
    set -e
    unset S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY
    unset DISCORD_WEBHOOK_URL DISCORD_WEBHOOK_ROUTES DISCORD_APP_ID DISCORD_PUBLIC_KEY DISCORD_BOT_TOKEN
    unset MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT
    clippet migrate
    printf %s "$IM_PW" | clippet admin users create "$IM_USER"
    mkdir -p "$IM_DIR" /tmp/imscratch /tmp/immedia
    touch /tmp/im-ready
    # There is nothing to serve: this bundle drives the CLI and asserts on a clip COUNT, which a
    # worker would be busy changing. Stay up for the execs and no longer.
    sleep 900
  ' >/dev/null || die "could not start the import container"

IM_N=0
while [ "$IM_N" -lt 90 ]; do
  docker_ exec "$IM_CT" test -f /tmp/im-ready >/dev/null 2>&1 && break
  IM_N=$((IM_N + 1)); sleep 1
done
docker_ exec "$IM_CT" test -f /tmp/im-ready >/dev/null 2>&1 || die "the import container never became ready:
$(docker_ logs --tail 40 "$IM_CT" 2>&1)"
echo "[e2e]   import app up after ${IM_N}s, owner $IM_USER, folder $IM_DIR"

# --- (b) the folder: two videos and one file that is not one ---------------------------------------
# The bytes are piped in over `docker exec -i`, never `docker cp`: the harness runs with MSYS path
# conversion ON (scripts/e2e.sh's own note), and a `ct:/tmp/…` argument is exactly the shape it
# rewrites. Two DIFFERENT fixtures, because two copies of one file are one video and would be `seen`.
docker_ exec -i "$IM_CT" sh -c "cat > '$IM_DIR/Team fight 3.mp4'" < testclips/std-720p30.mp4 \
  || die "could not plant the first video"
docker_ exec -i "$IM_CT" sh -c "cat > '$IM_DIR/holiday.mp4'" < testclips/small-640x360.mp4 \
  || die "could not plant the second video"
im_sh "printf %s 'these are not the bytes of any video' > '$IM_DIR/notes.txt'" \
  || die "could not plant the file that is not a video"
echo "[e2e]   planted: 'Team fight 3.mp4', holiday.mp4 (videos), notes.txt (not one)"

# --- (c) the DRY RUN comes first, because it is what an operator does first ------------------------
im_sh "clippet admin import '$IM_DIR' --owner $IM_USER --dry-run" > "$IM_OUT" 2>&1 \
  || die "admin import --dry-run exited non-zero:
$(cat "$IM_OUT")"
has_file "$IM_OUT" 'nothing in that folder is moved, renamed or deleted' "the header must say what the verb does to the folder"
has_file "$IM_OUT" 'refused not-a-video notes.txt' "the plan must name what it would refuse, and why"
has_file "$IM_OUT" '0 imported, 0 seen, 1 refused' "the plan's summary counts the verdicts it reached"
has_file "$IM_OUT" 'nothing was imported' "the plan must say it imported nothing"
expect "$(im_count import)" 2 "the plan names both videos it would import"
IM_LS=$(im_sh "clippet admin ls --owner $IM_USER" 2>&1 || true)
case $IM_LS in
  *"no clips"*) ;;
  *) die "--dry-run created something:
$IM_LS" ;;
esac
echo "[e2e]   --dry-run planned 2 imports, refused the text file, and created nothing"

# --- (d) the real run: two clips, one refusal, and the folder untouched ----------------------------
im_sh "clippet admin import '$IM_DIR' --owner $IM_USER" > "$IM_OUT" 2>&1 \
  || die "admin import exited non-zero:
$(cat "$IM_OUT")"
has_file "$IM_OUT" '2 imported, 0 seen, 1 refused' "the first run's summary"
has_file "$IM_OUT" 'refused not-a-video notes.txt' "a file that is not a video is refused by name and reason"
expect "$(im_count imported)" 2 "one imported line per video"
IM_LS=$(im_sh "clippet admin ls --owner $IM_USER" 2>&1 || true)
case $IM_LS in
  *"2 clips"*) ;;
  *) die "the import did not make two clips:
$IM_LS
--- the run ---
$(cat "$IM_OUT")" ;;
esac
IM_ID=$(printf '%s\n' "$IM_LS" | awk 'NR==2 {print $1}')
expect_nonempty "$IM_ID" "an imported clip's id"
# The TITLE is the file name without its extension — erratum 60's rule for every other upload.
case $IM_LS in
  *"Team fight 3"*) ;;
  *) die "an imported clip is not titled after its file:
$IM_LS" ;;
esac
# The original is really in the store, under the clip's own key.
IM_OBJ=$(im_sh "clippet admin objects $IM_ID --count" 2>&1 | tr -d ' \r')
expect "$IM_OBJ" 1 "the imported clip's original is in the store"
echo "[e2e]   imported 2 clips (first $IM_ID), refused the text file, original in the store"

# The one promise the verb makes about somebody else's folder: it READ it. Three files went in and
# three files are there, with the same bytes and nothing of ours beside them — no `.failed.txt`, no
# staging directory. Those belong to the drop folder, in a folder this app owns (§34 vs §34.4).
IM_LEFT=$(im_sh "ls -A '$IM_DIR' | wc -l" | tr -d ' \r')
expect "$IM_LEFT" 3 "the folder after an import holds exactly what went into it, and nothing of ours"
for IM_F in 'Team fight 3.mp4' 'holiday.mp4' 'notes.txt'; do
  im_sh "test -f '$IM_DIR/$IM_F'" || die "the import moved or removed $IM_F:
$(im_sh "ls -A '$IM_DIR'")"
done
docker_ exec -i "$IM_CT" sh -c "cmp -s - '$IM_DIR/holiday.mp4'" < testclips/small-640x360.mp4 \
  || die "an imported file's bytes changed"
echo "[e2e]   the folder still holds exactly the three files that went into it, byte for byte"

# --- (e) a SECOND run imports nothing, and says so per file ----------------------------------------
# This is the whole of #51's "idempotent by content hash": the memory is `clips.source_etag`, which is
# the sha256 the store computed while writing each original, so the second run recognises the bytes
# rather than the name. Rename one of them to prove exactly that.
im_sh "mv '$IM_DIR/holiday.mp4' '$IM_DIR/holiday-2019.mp4'" || die "could not rename the second video"
im_sh "clippet admin import '$IM_DIR' --owner $IM_USER" > "$IM_OUT" 2>&1 \
  || die "the second admin import exited non-zero:
$(cat "$IM_OUT")"
has_file "$IM_OUT" '0 imported, 2 seen, 1 refused' "the second run's summary: nothing new"
has_file "$IM_OUT" "seen $IM_ID " "the second run names the clip the bytes already are"
expect "$(im_count imported)" 0 "the second run imports nothing"
has_file "$IM_OUT" 'holiday-2019.mp4' "a renamed file is recognised by its CONTENT, not by its name"
IM_LS=$(im_sh "clippet admin ls --owner $IM_USER" 2>&1 || true)
case $IM_LS in
  *"2 clips"*) ;;
  *) die "the second run changed what the account owns (it must import NOTHING):
$IM_LS" ;;
esac
echo "[e2e]   the second run imported 0, saw 2 (one of them renamed), and the clip count did not move"

# --- (f) the refusals that are worth a word of their own -------------------------------------------
# An account that does not exist is exit 1 and says so: there is no default owner, and a typo must not
# quietly become somebody else's library.
IM_RC=0
im_sh "clippet admin import '$IM_DIR' --owner nosuchaccount" > "$IM_OUT" 2>&1 || IM_RC=$?
expect "$IM_RC" 1 "admin import for an account that does not exist"
has_file "$IM_OUT" 'no such user' "the refusal must say what is wrong"
# And this instance's OWN directories are not a folder of old clips: pointing the verb at the store
# would walk every object of every clip back through the upload path.
IM_RC=0
im_sh "clippet admin import /tmp/immedia --owner $IM_USER" > "$IM_OUT" 2>&1 || IM_RC=$?
expect "$IM_RC" 2 "admin import pointed at STORAGE_DIR"
has_file "$IM_OUT" "this instance's own directories" "the refusal must say why the store is not importable"
echo "[e2e]   an unknown owner is exit 1, and this instance's own directories are exit 2"

im_stop
trap - EXIT
summary_row IM "$IM_ID" "admin import (#51): a folder of two videos and one text file became two clips owned by the named account, with the text file refused by name and reason; --dry-run planned the same and created nothing; a second run (one file renamed) imported ZERO and named the clips its bytes already were; the folder was left with exactly the files that went into it, byte for byte; an unknown owner and this instance's own directories both refused; container removed"
