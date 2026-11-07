# 102-dedupe.sh — a byte-identical upload within one account answers the existing clip (DESIGN §20 erratum 326).
# Sourced by scripts/e2e.sh before the step-16 log gate (see
# scripts/e2e.d/README.md). Step 102.
#
# What this step proves on a live stack: the same file twice under ONE account is one id, one set of objects,
# one row and one job — the second answer 200 (never 201) with `"existing": true`, on the web grammar
# (POST /api/clips), the capture grammar (POST /api/upload) and the inbound URL (POST /in/{token}, the link as
# text); the opt-out `duplicate=allow` makes a new clip on both grammars; a SECOND account uploading the same
# bytes gets its own clip; the app's log carries one `upload deduped` line naming the clip and never the file;
# and a drop folder leaves a duplicate where it is with `already here <link>` beside it in `<name>.clippet.txt`. The bot's deduped
# post needs a successful fetch from Discord's CDN, which the suite never makes (85-bot's header): it is
# pinned in Go (internal/httpapi TestUploadFormDuplicateAppliesNothing, internal/notify
# TestBotFormDuplicateSaysNotAppliedAndPostsTheLink).
#
# e2e-schedule: parallel
# e2e-namespace: account=dd,dd2 prefix=DD_ buckets=-
# e2e-covers: internal/httpapi/ingest.go internal/httpapi/upload.go internal/httpapi/upload_capture.go
# e2e-covers: internal/httpapi/inbound.go internal/db/clips.go web/static/app.js web/templates/index.html
#   It creates its OWN two accounts (`bundle_account dd` and `dd2`, under the pattern the run's sweep covers),
#   uploads only as them — the duplicate check is per account, so no other bundle's uploads can be answered
#   with these clips or these with theirs — and deletes every clip it made. The drop leg is ONE throwaway
#   container of the app's own image with `--network none` (98-form's shape): its own DATA_DIR, SCRATCH_DIR
#   and an fs STORAGE_DIR under /tmp inside it, its own owner account; nothing of this stack is mounted, and
#   the trap removes it. It reads no shared clip, restarts and reconfigures nothing, and asserts nothing
#   bucket- or instance-wide (the log check greps for THIS step's own clip id).
#
# Variables are prefixed DD_ so nothing here can clobber a harness global.

step 102 "duplicates (§20 erratum 326): the same bytes twice in one account answer the existing clip (200, existing) on the web, capture and inbound grammars and write nothing; duplicate=allow makes a new one; another account gets its own; a drop folder notes 'already here <link>'"

DD_DIR="${CTX_DIR:-$E2E_DIR}/dedupe"
mkdir -p "$DD_DIR"
DD_SRC="$PWD/testclips/small-640x360.mp4"
DD_CLIP="$(W "$DD_SRC")"
DD_META="$DD_DIR/meta.json"; DD_METAW=$(W "$DD_META")
printf '%s' '{"title":"E2E dedupe first","ttl_days":1}' > "$DD_META"
DD_META2="$DD_DIR/meta2.json"; DD_META2W=$(W "$DD_META2")
printf '%s' '{"title":"E2E dedupe second","ttl_days":7,"unlisted":true}' > "$DD_META2"

bundle_account dd;  DD_USER=$BA_USER;  DD_JARW=$BA_JARW
bundle_account dd2; DD_USER2=$BA_USER; DD_JAR2W=$BA_JARW

# dd_web <jar> <metadata file> [extra curl args…] — POST /api/clips, the web grammar: metadata, then (when
# given) the extra parts, then the video. A plain curl: this is a WRITE, and a retried write is a second try.
dd_web() {
  local jar=$1 meta=$2; shift 2
  curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$jar" \
    -F "metadata=<$meta;type=application/json" "$@" -F "video=@$DD_CLIP;type=video/mp4" "$BASE/api/clips"
}
# dd_cap <jar> [extra curl args…] — POST /api/upload, the capture grammar: the fields, then the file.
dd_cap() {
  local jar=$1; shift
  curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$jar" "$@" -F "file=@$DD_CLIP;type=video/mp4" "$BASE/api/upload"
}

# --- (a) the first upload is a clip --------------------------------------------------------------------
code=$(dd_web "$DD_JARW" "$DD_METAW"); expect "$code" 201 "the first upload ($(cat "$RESP"))"
DD_A1=$(json id); expect_nonempty "$DD_A1" "the first clip's id"
expect_empty "$(json existing)" "a new clip's answer carries no existing"
wait_ready "$DD_A1" "the dedupe bundle's first clip"
DD_N1=$(prefix_count "$DD_A1")
echo "[e2e]   first upload: clip $DD_A1, $DD_N1 object(s) once ready"

# --- (b) the same bytes again, web grammar: 200 + existing, nothing written ---------------------------
code=$(dd_web "$DD_JARW" "$DD_META2W"); expect "$code" 200 "the same bytes again on POST /api/clips ($(cat "$RESP"))"
expect "$(json id)" "$DD_A1" "the duplicate answers the existing clip"
expect "$(json existing)" true "the duplicate's answer says existing"
expect "$(json title)" "E2E dedupe first" "the second upload's title is not applied"
expect "$(json unlisted)" false "the second upload's unlisted is not applied"
# …and the capture grammar, which a capture tool and the no-JS form speak.
code=$(dd_cap "$DD_JARW" -F 'title=ignored'); expect "$code" 200 "the same bytes again on POST /api/upload ($(cat "$RESP"))"
expect "$(json id)" "$DD_A1" "the capture duplicate answers the existing clip"
expect "$(json existing)" true "the capture duplicate's answer says existing"
expect "$(json url)" "$BASE/$DD_A1" "the capture duplicate's link"
# One clip, one transcode, and not one new object.
DD_LS=$(dc exec -T app clippet admin ls --owner "$DD_USER" | tr -d '\r') || die "admin ls --owner failed: $DD_LS"
contains "$DD_LS" "1 clips" || die "the account holds more than the one clip after two duplicates:
$DD_LS"
DD_JOBS=$(dc exec -T app clippet admin jobs --kind transcode --clip "$DD_A1" | tr -d '\r') || die "admin jobs failed: $DD_JOBS"
contains "$DD_JOBS" "1 job(s)" || die "the clip has more than one transcode job:
$DD_JOBS"
expect "$(prefix_count "$DD_A1")" "$DD_N1" "the clip's objects after the duplicates"
echo "[e2e]   two duplicates (web + capture) answered $DD_A1 with 200 and existing; still 1 clip, 1 transcode, $DD_N1 object(s)"

# --- (c) the inbound URL: the same link as text, 200 ----------------------------------------------------
DD_TOKURL=$(dc exec -T app clippet admin tokens mint "$DD_USER" --scopes inbound --name e2e-dedupe 2>/dev/null | tr -d '\r') \
  || die "admin tokens mint --scopes inbound failed"
case $DD_TOKURL in "$BASE/in/ci_"*) ;; *) die "the mint's stdout is not an inbound URL: $(redact_ "$DD_TOKURL")" ;; esac
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -F "file=@$DD_CLIP;type=video/mp4" "$DD_TOKURL")
expect "$code" 200 "the same bytes again on the inbound URL ($(cat "$RESP"))"
expect "$(tr -d '\r\n' < "$RESP")" "$BASE/$DD_A1" "the inbound duplicate's text answer is the existing link"

# --- (d) the log: one INFO line naming the clip, never the file ---------------------------------------
DD_LOG="$DD_DIR/app.log"
dc logs --no-color --since "$START_TS" app > "$DD_LOG" 2>&1 || die "docker compose logs failed"
DD_LINES=$(grep -c "msg=\"upload deduped\" clip=$DD_A1 " "$DD_LOG" || true)
expect "$DD_LINES" 3 "the 'upload deduped' lines for $DD_A1 (web, capture, inbound)"
grep "msg=\"upload deduped\" clip=$DD_A1 " "$DD_LOG" | grep -q 'small-640x360' \
  && die "a dedupe line names the file: $(grep 'upload deduped' "$DD_LOG" | head -1)"
rm -f "$DD_LOG"

# --- (e) the opt-out: duplicate=allow makes a new clip on both grammars --------------------------------
code=$(dd_cap "$DD_JARW" -F duplicate=allow); expect "$code" 201 "the capture grammar with duplicate=allow ($(cat "$RESP"))"
DD_A2=$(json id); expect_nonempty "$DD_A2" "the opted-out capture clip"
[ "$DD_A2" != "$DD_A1" ] || die "duplicate=allow answered the existing clip"
expect_empty "$(json existing)" "an opted-out upload's answer carries no existing"
code=$(dd_web "$DD_JARW" "$DD_METAW" -F duplicate=allow); expect "$code" 201 "the web grammar with duplicate=allow ($(cat "$RESP"))"
DD_A3=$(json id); expect_nonempty "$DD_A3" "the opted-out web clip"
[ "$DD_A3" != "$DD_A1" ] && [ "$DD_A3" != "$DD_A2" ] || die "duplicate=allow on the web grammar answered an existing clip"
echo "[e2e]   duplicate=allow made $DD_A2 (capture) and $DD_A3 (web, the part between metadata and video)"

# --- (f) never across accounts: another account's identical bytes are its own clip --------------------
code=$(dd_cap "$DD_JAR2W"); expect "$code" 201 "another account's upload of the same bytes ($(cat "$RESP"))"
DD_B1=$(json id)
case " $DD_A1 $DD_A2 $DD_A3 " in *" $DD_B1 "*) die "another account was answered with $DD_B1, a clip of $DD_USER" ;; esac
expect_empty "$(json existing)" "another account's upload carries no existing"
echo "[e2e]   $DD_USER2 uploading the same bytes got its own clip $DD_B1"

# --- (g) the drop folder: a duplicate stays where it is, with `already here <link>` in `<name>.clippet.txt` --
DD_APPCT=$(dc ps -q app 2>/dev/null | tr -d '\r' | head -1)
[ -n "$DD_APPCT" ] || die "the app container is not running"
DD_IMG=$(docker_ inspect --format '{{.Config.Image}}' "$DD_APPCT" | tr -d '\r')
[ -n "$DD_IMG" ] || die "could not read the app container's image"
DD_PORT=$(envfile CLIPPET_PUBLISH); DD_PORT=${DD_PORT##*:}; DD_PORT=${DD_PORT:-8481}
DD_CT="clippet-e2e-dedupe-$DD_PORT"
DD_ORIGIN=https://dedupe-probe.example
DD_MEDIA=/tmp/ddprobe-media
dd_stop() { docker_ rm -f "$DD_CT" >/dev/null 2>&1 || true; }
dd_stop
trap 'dd_stop' EXIT
docker_ run -d --name "$DD_CT" --network none --label "com.docker.compose.project=${E2E_PROJECT:-}" --label com.docker.compose.oneoff=True \
  -e PUBLIC_BASE_URL="$DD_ORIGIN" -e DATA_DIR=/tmp/ddprobe -e SCRATCH_DIR=/tmp/ddprobe-scratch \
  -e STORAGE_BACKEND=fs -e STORAGE_DIR="$DD_MEDIA" -e DROP_FOLDER_OWNER=ddowner -e DD_PW="$E2E_PASS" \
  --entrypoint sh "$DD_IMG" -c 'mkdir -p /tmp/ddprobe /tmp/ddprobe-scratch /tmp/ddprobe-media /tmp/gen &&
    clippet migrate >/tmp/boot.log 2>&1 &&
    printf %s "$DD_PW" | clippet admin users create ddowner >>/tmp/boot.log 2>&1 &&
    exec clippet serve >>/tmp/boot.log 2>&1' \
  >/dev/null || die "could not start the dedupe drop probe"
DD_N=0
until docker_ exec "$DD_CT" sh -c 'grep -q "drop folder watching" /tmp/boot.log' 2>/dev/null; do
  DD_N=$((DD_N + 1)); [ "$DD_N" -lt 60 ] || die "the dedupe drop probe did not start watching: $(docker_ exec "$DD_CT" cat /tmp/boot.log 2>&1)"
  sleep 1
done
# dd_plant <name> — the fixture written aside and RENAMED in, as 91-drop plants (bytes over `exec -i`).
dd_plant() {
  docker_ exec -i "$DD_CT" sh -c 'cat > /tmp/gen/f.mp4' < "testclips/small-640x360.mp4" || die "could not put the fixture into the probe"
  docker_ exec "$DD_CT" sh -c "mv /tmp/gen/f.mp4 '$DD_MEDIA/$1'" || die "could not plant $1"
}
dd_ls() { docker_ exec "$DD_CT" clippet admin ls --owner ddowner 2>&1 | tr -d '\r'; }
dd_plant first.mp4
DD_N=0
until contains "$(dd_ls)" "1 clips"; do
  DD_N=$((DD_N + 1)); [ "$DD_N" -lt 60 ] || die "the first dropped file never became a clip: $(dd_ls)
$(docker_ exec "$DD_CT" tail -20 /tmp/boot.log 2>&1)"
  sleep 2
done
DD_DID=$(dd_ls | awk 'NR==2 {print $1}')
expect_nonempty "$DD_DID" "the dropped clip's id"
dd_plant second.mp4
DD_N=0
until docker_ exec "$DD_CT" test -f "$DD_MEDIA/second.mp4.clippet.txt" 2>/dev/null; do
  DD_N=$((DD_N + 1)); [ "$DD_N" -lt 60 ] || die "the duplicate dropped file got no note: $(docker_ exec "$DD_CT" ls -A "$DD_MEDIA" 2>&1)
$(docker_ exec "$DD_CT" tail -20 /tmp/boot.log 2>&1)"
  sleep 2
done
DD_NOTE=$(docker_ exec "$DD_CT" cat "$DD_MEDIA/second.mp4.clippet.txt" | tr -d '\r')
expect "$DD_NOTE" "already here $DD_ORIGIN/$DD_DID" "the note beside the duplicate"
docker_ exec "$DD_CT" test -f "$DD_MEDIA/second.mp4" || die "the duplicate did not stay where it was"
docker_ exec "$DD_CT" test -e "$DD_MEDIA/second.mp4.failed.txt" && die "the duplicate got a .failed.txt: its note is second.mp4.clippet.txt (erratum 326, amended run 8)"
contains "$(dd_ls)" "1 clips" || die "the duplicate dropped file became a second clip: $(dd_ls)"
echo "[e2e]   drop folder: first.mp4 became $DD_DID; second.mp4 (the same bytes) stayed, noted 'already here $DD_ORIGIN/$DD_DID'"
dd_stop
trap - EXIT

# --- the litter this step made, gone --------------------------------------------------------------
for DD_ID in "$DD_A1" "$DD_A2" "$DD_A3"; do
  code=$(http DELETE "$BASE/api/clips/$DD_ID" -b "$DD_JARW"); expect_deleted "$code" "$DD_ID" "delete the dedupe clip $DD_ID"
done
code=$(http DELETE "$BASE/api/clips/$DD_B1" -b "$DD_JAR2W"); expect_deleted "$code" "$DD_B1" "delete the second account's clip"
for DD_ID in "$DD_A1" "$DD_A2" "$DD_A3" "$DD_B1"; do wait_gone "$DD_ID" "the dedupe clip $DD_ID"; done
rm -rf "$DD_DIR"

summary_row DD "$DD_A1" "duplicates (§20 erratum 326): the same bytes twice in one account answered the existing clip — 200 + existing on POST /api/clips and /api/upload, the link as text on /in/{token}; 1 clip, 1 transcode, no new object; one 'upload deduped' line per answer, no file name; duplicate=allow made new clips on both grammars; another account got its own; a drop-folder duplicate stayed with 'already here <link>'; clips deleted, probe removed (accounts swept by step 17)"
