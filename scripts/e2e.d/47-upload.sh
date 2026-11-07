# 47-upload.sh — bundle F: the upload half of the roadmap run.
#   47  multi-file upload (ROADMAP #7)      — the JS-less form, a two-file body, a sequential batch, a cancel
#   48  phone share sheet (ROADMAP #27)     — the manifest, the share target, and what a signed-out phone gets
#   49  clip replacement (ROADMAP #31)      — same link, new file, and what survives the swap
#   50  an unauthorized upload (ROADMAP #32) — a gibibyte that is never read
# Sourced by scripts/e2e.sh before the step-16 log gate. Every clip these steps create is destroyed by
# them; they touch A-E and X only by reading the shared cookie jar.
#
# e2e-schedule: parallel
# e2e-namespace: account=u prefix=UP_,R_ buckets=-
# e2e-covers: internal/httpapi/upload.go internal/httpapi/ingest.go internal/httpapi/replace.go
# e2e-covers: internal/httpapi/bodyguard.go internal/worker/replace.go internal/db/replace.go internal/db/clips.go
# e2e-covers: internal/httpx/budget.go internal/scratch/** internal/web/static.go web/static/app.js
# e2e-covers: web/templates/index.html
#   Until lane `excl` it was exclusive for ONE reason: step 47's cancel probe took an INSTANCE-WIDE
#   census either side of it — `admin stats`' clip and object counts and the whole of SCRATCH_DIR
#   listed for a `.upload-` spool — and any other bundle uploading, deleting or transcoding inside that
#   window changed both numbers (and its in-flight spool was indistinguishable from the one that must
#   not exist). The cancel now happens on an INSTANCE OF ITS OWN: one `dc run --rm` container of the
#   same image with its own database, its own SCRATCH_DIR, its own S3_KEY_PREFIX (and STORAGE_DIR, on
#   a directory stack), driven over its own loopback (89-quota's shape). The census is the same three
#   readings — clips, objects, the spool listing — of an instance nothing else talks to, so it is
#   exact again: before and after are both that instance's own, and the claim (a cut-off body leaves no
#   row, no object and no spool) is the same claim about the same handler. Everything else here is this
#   file's own clips. Its two other dependencies are declared, not coincidental: step 47's "the account
#   expresses no TTL preference" is about the run's owner, whose upload defaults no bundle changes since
#   lane `excl` (55-owner.sh step 57 uses an account of its own); and step 49's preview-GIF poll
#   spends the `view` bucket once every two seconds, which the arithmetic beside the poll shows the
#   bucket can carry beside either of the suite's other two `view` pollers.

# up_admin_key <id> <label> -> the object key `admin ls` prints for one line (source/rendition/poster/gif).
up_admin_key() {
  local out
  out=$(dc exec -T app clippet admin ls "$1") || die "admin ls $1 failed: $out"
  printf '%s\n' "$out" | sed -n "s#^$2: *\([^ ]*\).*#\1#p" | tr -d '\r'
}

# `up_names` used to stand here — bundle F's own copy of the mc listing 41-social.sh also had, written so
# the step did not depend on another bundle's file being present. Both listed the BUNDLED MINIO, which is
# this stack's store only while the stack keeps its media there (§20 erratum 281). `prefix_names` is a
# harness helper now and asks the app instead, so there is nothing left worth copying: step 49 calls it
# directly, which is what scripts/check.sh's cross-drop-in scan wants anyway.

step 47 "multi-file upload (#7): the JS-less form works for ONE file, a batch is sequential, a cancel leaves nothing"
# 1. The served form is what the no-JS path needs: it posts to the CAPTURE endpoint, whose grammar a
#    plain browser form fits, and it carries NO `multiple` — app.js adds that, because a second file
#    part would be sent and never read. data-upload-api is where the script posts instead.
code=$(page "$BASE/" -b "$JARW"); expect "$code" 200 "GET / as the owner"
has_file "$PAGE" 'action="/api/upload"' "the upload form does not post to the capture endpoint"
has_file "$PAGE" 'data-upload-api="/api/clips"' "the upload form does not name the JS endpoint"
has_file "$PAGE" 'data-upload-cancel' "the upload form has no Cancel button"
has_file "$PAGE" 'data-upload-retry' "the upload form has no Try-again button"
# EVERY named field of the form must precede the file field, not just the ones next to it: the capture
# reader RETURNS at the file part, so a field left after it is not read late, it is never read at all.
# The two that used to sit there were `ttl_days` and `unlisted` — a private clip served to the whole
# instance and announced to the webhook. The check names all six (§20 erratum 250 added `link_style`,
# whose failure mode is the same shape: every link minted in the account's other grammar) and prints
# the order it found.
UP_FORM=$($PY - "$(W "$PAGE")" <<'PYEOF'
import re, sys
html = open(sys.argv[1], encoding='utf-8').read()
form = html.split('data-upload ', 1)[1].split('</form>', 1)[0]
# `multiple` is looked for INSIDE the form and nowhere else: the index page also carries the whole
# shared library, so an unscoped scan reads every clip title, description and tag name in it and a
# clip called "multiple angles" would fail this step with a message about the upload form.
if 'multiple' in form:
    sys.exit('the served form offers multiple files without JavaScript')
file_at = form.find('name="video"')
if file_at < 0:
    sys.exit('the upload form has no file field')
seen, late = [(file_at, 'video')], []
for name in ('title', 'description', 'tags', 'ttl_days', 'unlisted', 'link_style'):
    at = form.find('name="%s"' % name)
    if at < 0:
        sys.exit('the upload form has no %s field' % name)
    seen.append((at, name))
    if at > file_at:
        late.append(name)
if late:
    sys.exit('these fields follow the file part and are read by nobody: %s' % ', '.join(late))
seen.sort()
print('served field order: ' + ' -> '.join(n for _, n in seen))
# The lifetime the server applies when the form sends none, read out of the form's own option
# ("server default · never | 1 day | N days" under the "Delete after" row, web/templates/index.html;
# §20 erratum 244) so part 2b can assert the VALUE a dropped ttl_days falls back to instead of
# merely asserting that the value it sent is gone.
# The attribute list after `value=""` is deliberately tolerant: the UPLOAD form's option carries nothing
# else, but settings.html renders the identical option as `<option value="" selected="selected">`, so a
# pin on a bare tag would break the moment the two templates ever share a partial.
mo = re.search('<option value=""[^>]*>server default \u00b7 ([^<]*)</option>', form)
if not mo:
    sys.exit('the upload form no longer names the server default lifetime')
label = mo.group(1).strip()
if label == 'never':
    days = 0                                   # `never` is ttl_days 0 on the wire (§8.3)
elif re.fullmatch(r'1 day', label):
    days = 1
elif re.fullmatch(r'[0-9]+ days', label):
    days = int(label.split()[0])
else:
    sys.exit('the server default lifetime reads %r, want never / 1 day / N days' % label)
print('default-ttl-days: %d' % days)
PYEOF
) || die "the upload form drops fields on the no-JS path (message above)"
UP_ORDER=$(printf '%s\n' "$UP_FORM" | sed -n 's/^served field order: //p')
UP_DEF_TTL=$(printf '%s\n' "$UP_FORM" | sed -n 's/^default-ttl-days: //p')
expect_nonempty "$UP_ORDER" "the served field order"
expect_nonempty "$UP_DEF_TTL" "the server default lifetime, read from the served form"
echo "[e2e]   the JS-less form: POST /api/upload, single file, $UP_ORDER, default lifetime ${UP_DEF_TTL}d"

# 2. What that form actually sends, by hand and IN THE ORDER IT SERVES: every text field, then ONE
#    file part named `video`. All of it is applied — including the lifetime and the Unlisted tick,
#    which the markup used to place after the file field, where nothing reads them.
STAMP=$(date +%s)
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "title=E2E nojs $STAMP" -F "description=posted by a plain form" -F "tags=nojs, e2e" \
  -F "ttl_days=1" -F "unlisted=on" \
  -F duplicate=allow -F "video=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "no-JS form POST ($(cat "$RESP"))"
N1=$(json id); expect_nonempty "$N1" "no-JS clip id"
code=$(get "$BASE/api/clips/$N1" -b "$JARW"); expect "$code" 200 "GET the no-JS clip"
expect "$(json title)" "E2E nojs $STAMP" "the no-JS form's title reaches the clip"
expect "$(json ttl_days)" 1 "the no-JS form's ttl_days reaches the clip"
expect_nonempty "$(json expires_at)" "the no-JS form's lifetime reaches the clip"
expect "$(json unlisted)" true "the no-JS form's Unlisted tick reaches the clip"
expect "$(json tags.0)" e2e "the no-JS form's tags reach the clip (sorted)"
expect "$(json tags.1)" nojs "the no-JS form's second tag"

# 2b. The negative that gives part 1 its teeth: the SAME request with `tags`, `ttl_days` and `unlisted`
#     moved AFTER the file part — the body the OLD markup produced. Those three are never read, so the
#     clip comes back untagged, listed and on the server's own default lifetime. `tags` and `unlisted`
#     make this assertion config-proof (no server default can supply either); the lifetime is compared
#     against the default the served form ADVERTISES, which part 1 read out of its own "Default
#     (server: …)" label — exact, and still true on an instance with another DEFAULT_CLIP_TTL. The
#     account expresses no TTL preference at this point in the run (step 57 sets one and clears it
#     again, later), so the instance default is the effective one here.
UP_LATE_TTL=90
[ "$UP_DEF_TTL" != 90 ] || UP_LATE_TTL=30   # the value sent must differ from the default, or nothing is proven
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "title=E2E late fields $STAMP" \
  -F duplicate=allow -F "video=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4" \
  -F "tags=late$STAMP" -F "ttl_days=$UP_LATE_TTL" -F "unlisted=on" "$BASE/api/upload")
expect "$code" 201 "no-JS POST with the fields after the file ($(cat "$RESP"))"
N5=$(json id); expect_nonempty "$N5" "late-fields clip id"
[ "$N5" != "$N1" ] || die "the late-fields POST returned the first clip's id"
code=$(get "$BASE/api/clips/$N5" -b "$JARW"); expect "$code" 200 "GET the late-fields clip"
expect "$(json title)" "E2E late fields $STAMP" "a field BEFORE the file part is still read"
expect "$(json unlisted)" false "an unlisted tick sent after the file part was applied"
expect_empty "$(json tags.0)" "a tag sent after the file part was applied"
expect "$(json ttl_days)" "$UP_DEF_TTL" "the ttl_days=$UP_LATE_TTL sent after the file part was applied (want the server default)"
echo "[e2e]   after the file part every field is dropped ($N5, lifetime ${UP_DEF_TTL}d); before it every field lands ($N1)"

# 3. TWO file parts in one body: the second is never read, so exactly ONE clip exists afterwards. The
#    files are copied under unique names so the assertion cannot be satisfied by another step's clip.
cp testclips/std-720p30.mp4 "$E2E_DIR/first-$STAMP.mp4"
cp testclips/noaudio-720p30.mp4 "$E2E_DIR/second-$STAMP.mp4"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F duplicate=allow -F "file=@$(W "$E2E_DIR/first-$STAMP.mp4");type=video/mp4" \
  -F "file=@$(W "$E2E_DIR/second-$STAMP.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "two-file POST ($(cat "$RESP"))"
N2=$(json id); expect_nonempty "$N2" "two-file clip id"
[ "$N2" != "$N1" ] || die "the two-file POST returned the first clip's id"
code=$(get "$BASE/api/clips/$N2" -b "$JARW"); expect "$code" 200 "GET the two-file clip"
expect "$(json title)" "first-$STAMP" "the FIRST file is the clip; its name is the title"
expect "$(json source.filename)" "first-$STAMP.mp4" "the clip's source filename"
code=$(get "$BASE/api/clips?limit=100" -b "$JARW"); expect "$code" 200 "list clips after the two-file POST"
lacks_file "$RESP" "second-$STAMP" "the SECOND file part became a clip of its own"
echo "[e2e]   two file parts in one body -> one clip ($N2), the second part unread"

# 3b. A capture tool's file name and no title (§20 erratum 317(a)): the fallback is the timestamp as a
#     date and a time, the text before it kept with its underscores made spaces, the rest dropped.
cp testclips/std-720p30.mp4 "$E2E_DIR/ts$STAMP"'_2025-06-24_00-23-54-00.00.22.520.mp4'
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F duplicate=allow -F "file=@$(W "$E2E_DIR/ts$STAMP"'_2025-06-24_00-23-54-00.00.22.520.mp4');type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "the timestamp-named POST ($(cat "$RESP"))"
N6=$(json id); expect_nonempty "$N6" "timestamp-named clip id"
code=$(get "$BASE/api/clips/$N6" -b "$JARW"); expect "$code" 200 "GET the timestamp-named clip"
expect "$(json title)" "ts$STAMP 2025-06-24 00:23" "a capture tool's timestamp is the fallback title"
echo "[e2e]   a timestamp file name -> the title 'ts$STAMP 2025-06-24 00:23' ($N6)"

# 4. The batch app.js sends: one POST per file, sequentially, sharing one metadata document. The titles
#    are what the client composes for a batch of two.
code=$(upload BATCH1 std-720p30.mp4 "{\"title\":\"E2E batch $STAMP (1/2)\",\"ttl_days\":1,\"tags\":[\"batch$STAMP\"]}")
expect "$code" 201 "batch upload 1 ($(cat "$RESP"))"
N3=$(json id)
code=$(upload BATCH2 noaudio-720p30.mp4 "{\"title\":\"E2E batch $STAMP (2/2)\",\"ttl_days\":1,\"tags\":[\"batch$STAMP\"]}")
expect "$code" 201 "batch upload 2 ($(cat "$RESP"))"
N4=$(json id)
[ -n "$N3" ] && [ -n "$N4" ] && [ "$N3" != "$N4" ] || die "the batch produced ids '$N3' and '$N4'"
code=$(get "$BASE/api/clips?tag=batch$STAMP&limit=10" -b "$JARW"); expect "$code" 200 "list the batch by its shared tag"
expect "$(json clips.0.tags.0)" "batch$STAMP" "the batch's shared tag"
expect_empty "$(json clips.2.id)" "the batch is exactly two clips"
echo "[e2e]   sequential batch: $N3 + $N4 under one tag"

# 5. A cancelled upload leaves NOTHING: no row, no object, no scratch directory. curl is cut off
#    mid-body, which is exactly what xhr.abort() does to the connection.
#    The census is taken with every clip above already READY, so the objects an in-flight transcode is
#    still uploading cannot be mistaken for something the cancelled request left.
for pair in "N1:$N1" "N2:$N2" "N3:$N3" "N4:$N4" "N5:$N5" "N6:$N6"; do wait_ready "${pair#*:}" "${pair%%:*}"; done
# The census is taken on an instance of the cancel's OWN (the header says why): a throwaway container,
# its own database, scratch and key prefix, driven over its own loopback. The fixture is the same
# 1080p60 file the step always cut off, handed in on stdin (a `compose run` container cannot see the
# host's testclips/), and the cut is the same: --limit-rate 400k --max-time 2, i.e. ~800 KB of 5.6 MB.
# It reads the census BEFORE, cuts the upload off, waits for the spool to be gone (or 30 s — the handler's
# cleanup runs after the client has left, so this is the event the old fixed `sleep 3` stood for, and a
# loaded box no longer turns a slow cleanup into a failure), then reads the census AFTER.
UP_PREFIX="e2e-upcancel-$(date +%s)/"
UP_OUT="$E2E_DIR/upload-cancel-probe.txt"
up_rm_prefix() {
  store_has_bucket || return 0
  store_rm_prefix_ok_ "${UP_PREFIX:-}" || return 0
  ( store_obj rm --prefix "$UP_PREFIX" >/dev/null ) 2>/dev/null \
    || echo "[e2e]   NOTE: could not remove the cancel probe's prefix $UP_PREFIX (its own, outside the stack's key prefix)"
}
trap 'up_rm_prefix' EXIT
UP_SH="$E2E_DIR/upload-cancel-probe.sh"
cat > "$UP_SH" <<'UPLOAD_CANCEL_PROBE'
set -e
mkdir -p /tmp/upc /tmp/upc-scratch /tmp/upc-store
cat > /tmp/in.mp4
echo "fixturebytes=$(wc -c < /tmp/in.mp4 | tr -d " ")"
clippet migrate >/dev/null
clippet serve >/tmp/serve.log 2>&1 &
SP=$!
i=0
while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
curl -sS -o /dev/null -c /tmp/jar -w "%{http_code}\n" -X POST -H "Content-Type: application/json" \
  -d "{\"username\":\"$UPUSER\",\"password\":\"$UPPASS\"}" http://127.0.0.1:8080/api/auth/register > /tmp/regcode
echo "register=$(tr -d "\r\n" < /tmp/regcode)"
census() {  # census <tag> — the same three readings the step always took, of THIS instance
  clippet admin stats > /tmp/st 2>&1 || true
  echo "$1clips=$(sed -n "s/^clips: *\([0-9]*\) .*/\1/p" /tmp/st | head -1)"
  echo "$1objects=$(sed -n "s/^bucket: *objects=\([0-9]*\) .*/\1/p" /tmp/st | head -1)"
  echo "$1spools=$(ls -a "$SCRATCH_DIR" 2>/dev/null | grep -c "^\.upload-" || true)"
}
census before
RC=0
curl -sS -o /dev/null -b /tmp/jar --limit-rate 400k --max-time 2 \
  -F "title=$UPTITLE" -F duplicate=allow -F "file=@/tmp/in.mp4;type=video/mp4" http://127.0.0.1:8080/api/upload >/dev/null 2>&1 || RC=$?
echo "curlrc=$RC"
i=0
while [ $i -lt 60 ]; do
  ls -a "$SCRATCH_DIR" 2>/dev/null | grep -q "^\.upload-" || break
  i=$((i+1)); sleep 0.5
done
echo "spoolwait=$i"
census after
echo "scratch=$(ls -a "$SCRATCH_DIR" 2>/dev/null | tr "\n" " ")"
curl -sS -o /tmp/lib -b /tmp/jar -w "%{http_code}" "http://127.0.0.1:8080/api/clips?limit=100" > /tmp/libcode
echo "libcode=$(cat /tmp/libcode)"
if grep -q "$UPTITLE" /tmp/lib; then echo "inlibrary=yes"; else echo "inlibrary=no"; fi
kill $SP 2>/dev/null || true
wait $SP 2>/dev/null || true
UPLOAD_CANCEL_PROBE
UP_RC=0
dc run --rm -T --no-deps \
  -e DATA_DIR=/tmp/upc -e SCRATCH_DIR=/tmp/upc-scratch -e STORAGE_DIR=/tmp/upc-store \
  -e "S3_KEY_PREFIX=$UP_PREFIX" \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 -e REGISTRATION=open -e SWEEP_INTERVAL=24h \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e DISCORD_APP_ID= -e DISCORD_PUBLIC_KEY= -e DISCORD_BOT_TOKEN= \
  -e "UPUSER=${E2E_USER}_u" -e "UPPASS=$E2E_PASS" -e "UPTITLE=E2E cancelled $STAMP" \
  --entrypoint sh app -c "$(cat "$UP_SH")" < "$PWD/testclips/hd-1080p60.mp4" > "$UP_OUT" 2>&1 || UP_RC=$?
sed -n 's/^/[e2e]   cancel-probe: /p' "$UP_OUT" | grep -v 'cancel-probe: scratch=' || true
[ "$UP_RC" = 0 ] || die "the cancel probe container failed (exit $UP_RC):
$(cat "$UP_OUT")"
upv() { sed -n "s/^$1=//p" "$UP_OUT" | tail -1 | tr -d '\r'; }
expect "$(upv fixturebytes)" "$(wc -c < testclips/hd-1080p60.mp4 | tr -d ' ')" "the cancel fixture reached the probe whole"
expect "$(upv register)" 201 "register the cancel probe's account"
CLIPS_BEFORE=$(upv beforeclips); OBJECTS_BEFORE=$(upv beforeobjects)
expect_nonempty "$CLIPS_BEFORE" "clip census before the cancel"
expect_nonempty "$OBJECTS_BEFORE" "object census before the cancel"
CURL_RC=$(upv curlrc)
[ "$CURL_RC" != 0 ] || die "the cancel probe finished; the upload was not interrupted (rc=$CURL_RC)"
echo "[e2e]   cancelled mid-body (curl rc=$CURL_RC); spool gone after $(upv spoolwait) half-second turn(s)"
CLIPS_AFTER=$(upv afterclips); OBJECTS_AFTER=$(upv afterobjects)
expect "$CLIPS_AFTER" "$CLIPS_BEFORE" "a cancelled upload created a clip row"
expect "$OBJECTS_AFTER" "$OBJECTS_BEFORE" "a cancelled upload left an object in the bucket"
expect "$(upv afterspools)" 0 "a cancelled upload left a spool directory ($(upv scratch))"
expect "$(upv libcode)" 200 "list clips after the cancel"
expect "$(upv inlibrary)" no "a cancelled upload is in the library"
up_rm_prefix
trap - EXIT

# 6. Clean up this step's six clips.
for id in "$N1" "$N2" "$N3" "$N4" "$N5" "$N6"; do
  code=$(http DELETE "$BASE/api/clips/$id" -b "$JARW")
  case $code in 204|202) ;; *) die "DELETE $id -> $code" ;; esac
done

step 48 "phone share sheet (#27): the manifest declares a share target, and the target needs a session"
# 1. The manifest itself. It is asserted as PARSED JSON: a manifest with a typo in `share_target` is
#    not a manifest a browser acts on, and acting on it is the entire feature.
code=$(get "$BASE/manifest.webmanifest"); expect "$code" 200 "GET /manifest.webmanifest"
expect "$(hdr Content-Type)" "application/manifest+json; charset=utf-8" "manifest Content-Type"
cp "$RESP" "$E2E_DIR/manifest.json"
$PY - "$(W "$E2E_DIR/manifest.json")" <<'PYEOF' || die "the manifest is not the shape a share target needs"
import json, sys
m = json.load(open(sys.argv[1], encoding='utf-8'))
fails = []
if m.get('name') != 'Clippet' or m.get('start_url') != '/' or m.get('scope') != '/':
    fails.append('identity: %r' % {k: m.get(k) for k in ('name', 'start_url', 'scope')})
if m.get('display') != 'standalone':
    fails.append('display=%r' % m.get('display'))
sizes = sorted(i.get('sizes') for i in m.get('icons', []))
if '192x192' not in sizes or '512x512' not in sizes:
    fails.append('icon sizes %r (Android installs neither without both)' % sizes)
st = m.get('share_target') or {}
if st.get('action') != '/api/share' or st.get('method') != 'POST' or st.get('enctype') != 'multipart/form-data':
    fails.append('share_target %r' % st)
files = (st.get('params') or {}).get('files') or []
if len(files) != 1 or files[0].get('name') != 'file' or 'video/*' not in (files[0].get('accept') or []):
    fails.append('share_target files %r' % files)
if (st.get('params') or {}).get('title') != 'title':
    fails.append('share_target title param %r' % (st.get('params') or {}).get('title'))
if 'service_worker' in m:
    fails.append('a service worker is declared; this build ships none')
if fails:
    sys.exit('; '.join(fails))
print('manifest ok: %s -> %s' % (m['name'], st['action']))
PYEOF
# Every icon it names is a file this server serves.
for icon in 192 512; do
  code=$(head_ "$BASE/static/icon-$icon.png"); expect "$code" 200 "GET /static/icon-$icon.png"
  expect "$(hdr Content-Type)" image/png "icon-$icon.png Content-Type"
done
# And a page links it, or no browser ever reads it.
code=$(page "$BASE/" -b "$JARW"); expect "$code" 200 "GET / for the manifest link"
has_file "$PAGE" '<link rel="manifest" href="/manifest.webmanifest">' "no page links the manifest"

# 2. The share target with a session: what Android's share sheet POSTs — the text fields it sends
#    beside the file — answered with a redirect to the clip's own page.
code=$(curl -sS -o "$PAGEW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "title=E2E share $STAMP" -F "text=look at this" -F "url=https://example.invalid/x" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4" "$BASE/api/share")
expect "$code" 303 "share-target POST with a session"
SHARE_LOC=$(hdr Location); expect_nonempty "$SHARE_LOC" "share-target Location"
SH=${SHARE_LOC##*/}
expect "$SHARE_LOC" "$BASE/$SH" "the share target redirects to the clip's share page"
code=$(get "$BASE/api/clips/$SH" -b "$JARW"); expect "$code" 200 "GET the shared clip"
expect "$(json title)" "E2E share $STAMP" "the share sheet's title becomes the clip's"
expect_empty "$(json description)" "the share sheet's text field must not become a description"
code=$(page "$SHARE_LOC"); expect "$code" 200 "the page the phone lands on"
echo "[e2e]   share target -> $SHARE_LOC"

# 3. Without a session: 401, and for a BROWSER an interstitial it can act on rather than an envelope
#    it cannot read. The file is never read on that path (step 50 measures that).
#    Both halves go through `http_refused` (§20 erratum 275): the app refuses this multipart POST
#    BEFORE it reads the body, and through an edge that is exactly the request that gets dropped —
#    it killed three gate runs on 2026-09-11. A refused write has no side effect to double, so one
#    retry is safe here and nowhere a write SUCCEEDS. The interstitial below lands in $RESP rather
#    than $PAGE, because that is where every http_refused body lands; it is an HTML body either way.
code=$(http_refused POST "$BASE/api/share" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4")
expect "$code" 401 "share-target POST with no session"
expect "$(json error)" unauthorized "signed-out share-target error code"
code=$(http_refused POST "$BASE/api/share" -H 'Accept: text/html' \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4")
expect "$code" 401 "share-target POST from a signed-out browser"
contains "$(hdr Content-Type)" "text/html" || die "the signed-out browser answer is $(hdr Content-Type), want HTML"
expect_empty "$(hdr Location)" "the signed-out answer redirects instead of holding the user"
has_file "$RESP" 'Sign in to share' "the signed-out interstitial does not say what happened"
has_file "$RESP" 'href="/login"' "the signed-out interstitial does not link the login form"
lacks_file "$RESP" '<script' "the interstitial carries an inline script"

code=$(http DELETE "$BASE/api/clips/$SH" -b "$JARW")
case $code in 204|202) ;; *) die "DELETE the shared clip -> $code" ;; esac

step 49 "clip replacement (#31): the same link plays a different file, and everything else survives"
# R is this step's own clip: tagged, on a one-day TTL, viewed once, trimmed and given a GIF, so the
# replacement has something of each class to keep and to reset.
code=$(upload R vertical-720x1280.mp4 "{\"title\":\"E2E replace $STAMP\",\"ttl_days\":1,\"tags\":[\"replace$STAMP\"]}")
expect "$code" 201 "upload R ($(cat "$RESP"))"
R=$(json id); expect_nonempty "$R" "R id"
wait_ready "$R" R
code=$(http POST "$BASE/api/clips/$R/view"); expect "$code" 204 "count a view on R"
code=$(get "$BASE/api/clips/$R" -b "$JARW"); expect "$code" 200 "GET R as its owner"
R_W=$(json width); R_H=$(json height); R_VIEWS=$(json views); R_EXPIRES=$(json expires_at)
expect "$R_W" 720 "R width (the vertical fixture)"; expect "$R_H" 1280 "R height"
expect "$R_VIEWS" 1 "R views before the replacement"
expect_nonempty "$R_EXPIRES" "R expires_at"
R_SRCKEY=$(up_admin_key "$R" source); expect_nonempty "$R_SRCKEY" "R source key"
code=$(head_ "$BASE/api/clips/$R/original" -b "$JARW"); expect "$code" 200 "HEAD R original before the replacement"
R_OETAG=$(hdr ETag); expect_nonempty "$R_OETAG" "R original ETag"
contains "$R_SRCKEY" "clips/$R/original/source." || die "R source key $R_SRCKEY is not the plain original shape"
# A cut, so the replacement has a trim window to reset...
code=$(http POST "$BASE/api/clips/$R/trim" -b "$JARW" -H 'Content-Type: application/json' -d '{"start_ms":500,"end_ms":3500}')
expect "$code" 202 "trim R ($(cat "$RESP"))"
wait_trim "$R" R
code=$(get "$BASE/api/clips/$R" -b "$JARW"); expect "$code" 200 "GET R after the trim"
expect_nonempty "$(json trim.start_ms)" "R has a trim window before the replacement"
# ...and a preview, so it has a GIF to drop.
code=$(get "$BASE/api/clips/$R/preview.gif" -b "$JARW")
case $code in 200|202) ;; *) die "GIF request on R -> $code" ;; esac
t0=$SECONDS
until code=$(head_ "$BASE/api/clips/$R/preview.gif"); [ "$code" = 200 ]; do
  [ $((SECONDS - t0)) -lt 120 ] || die "R has no preview GIF within 120s (last $code)"
  sleep 2   # was 1 while this bundle ran alone (lane speed2); in the pool it shares the `view` bucket (60/min/address)
            # with 43-watch's GIF poll (one every 2 s) or 70-web's filmstrip poll (one a second) — never both, they are
            # apart — so the worst case beside it is 1.5 a second against a refill of 1 and a burst of 60 (lane excl)
done
code=$(get "$BASE/api/clips/$R" -b "$JARW"); expect "$code" 200 "GET R with its GIF"
expect_nonempty "$(json gif_url)" "R gif_url before the replacement"
# The rendition R is serving RIGHT NOW — after the trim swapped it, and the file every assertion
# below compares against.
# `media_head` (§20 erratum 280, rule (a)): the ETag of the object R is serving is the thing under
# test on all three of these lines, and it is the object's wherever the bytes come from.
code=$(media_head "$BASE/api/clips/$R/video.mp4"); expect_media "$code" 200 "HEAD R video.mp4 before the replacement"
R_VETAG=$(hdr ETag); expect_nonempty "$R_VETAG" "R rendition ETag"

# 1. Only the owner, and only a session: a capture token is an upload credential, not an editor
#    (DESIGN 20 erratum 19). The token is minted and revoked inside this step.
code=$(http POST "$BASE/api/auth/tokens" -b "$JARW" -H 'Content-Type: application/json' -d '{"name":"e2e replace"}')
expect "$code" 201 "mint a token for the replacement negatives"
RTOK=$(json token); RTID=$(json id)
#    Both are REFUSALS, so `http_refused` (§20 erratum 275) — the same early-refused multipart POST
#    an edge dropped at step 48. The replacement itself, further down, is a write whose SUCCESS is
#    asserted and is never retried.
code=$(http_refused POST "$BASE/api/clips/$R/replace" -H "Authorization: Bearer $RTOK" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4")
expect "$code" 403 "replace with a capture token"
expect "$(json error)" token_not_allowed "replace-with-a-token error code"
code=$(http_refused POST "$BASE/api/clips/$R/replace" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4")
expect "$code" 401 "replace with no credential"
code=$(http DELETE "$BASE/api/auth/tokens/$RTID" -b "$JARW"); expect "$code" 204 "revoke the replacement token"
# Nothing above changed the clip.
code=$(media_head "$BASE/api/clips/$R/video.mp4"); expect_media "$code" 200 "HEAD R video.mp4 after the refusals"
R_VETAG2=$(hdr ETag)
expect "$R_VETAG2" "$R_VETAG" "a refused replacement changed R's rendition"

# 2. The replacement itself: a landscape file over a vertical clip, so the swap is visible in the
#    dimensions and cannot be confused with a re-encode of the same source.
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4" "$BASE/api/clips/$R/replace")
expect "$code" 202 "POST replace ($(cat "$RESP"))"
expect "$(json state)" queued "replace state"
expect_nonempty "$(json job_id)" "replace job id"
# The upload is sacred, and this is the ordering the whole feature turns on: the clip is STILL
# serving its old original — the row has not moved and the object has not been deleted — while the
# replacement is transcoded. One request, issued immediately after the 202, because the encode
# itself takes seconds and a bucket listing takes a container.
code=$(head_ "$BASE/api/clips/$R/original" -b "$JARW"); expect "$code" 200 "HEAD R original during the replacement"
R_OETAG_LIVE=$(hdr ETag); expect_nonempty "$R_OETAG_LIVE" "R original ETag during the replacement"
# The job's own state is read in the same breath, because "during" is a race this step cannot win by
# being quick: a 6-second clip re-encodes in under a second on an idle host, and the swap makes
# /original serve the new key the instant it commits. Both honest outcomes pass — the old original
# (the property under test) or a replacement that has already finished — and the third one fails: a
# DIFFERENT original while the job says it is still running is the swap landing before the commit.
code=$(get "$BASE/api/clips/$R/replace" -b "$JARW"); expect "$code" 200 "R replace status beside the in-flight read"
R_RSTATE=$(json state)
if [ "$R_OETAG_LIVE" = "$R_OETAG" ]; then
  echo "[e2e]   the upload is sacred: /original still serves the pre-replacement file (job $R_RSTATE)"
elif [ "$R_RSTATE" = done ]; then
  echo "[e2e]   NOTE: the replacement committed before this read; the swap's ordering is covered by the worker unit tests"
else
  die "R serves a different original ($R_OETAG_LIVE, was $R_OETAG) while its replacement is still $R_RSTATE"
fi
# A second replacement while the first is live is the 409 the database decides. It sends the SAME
# file, so the run is unharmed on the rare occasion the first one has already committed.
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4" "$BASE/api/clips/$R/replace")
case $code in
  409) expect "$(json error)" replace_in_progress "second replacement error code" ;;
  202) echo "[e2e]   NOTE: the first replacement had already finished; the second one is now the live job" ;;
  *)   die "second replacement -> $code ($(cat "$RESP"))" ;;
esac

# 3. Poll the job. Throughout it the clip is `ready` and the share page is a PLAYER card: a viewer
#    never sees a replacement happen.
t0=$SECONDS
while :; do
  code=$(get "$BASE/api/clips/$R/replace" -b "$JARW"); expect "$code" 200 "R replace status"
  RSTATE=$(json state); RPCT=$(json percent)
  [ -n "$RPCT" ] && [ "$RPCT" -ge 0 ] && [ "$RPCT" -le 100 ] 2>/dev/null || die "R replace percent '$RPCT'"
  case $RSTATE in
    done)   echo "[e2e]   R replaced after $((SECONDS - t0))s"; break ;;
    failed) die "R replacement failed: $(json error)" ;;
    queued|running) ;;
    *) die "R replace state '$RSTATE' (want queued|running|done|failed)" ;;
  esac
  code=$(get "$BASE/api/clips/$R"); expect "$code" 200 "GET R while its replacement runs"
  expect "$(json state)" ready "R state while its replacement runs (it never moves it)"
  [ $((SECONDS - t0)) -lt 180 ] || die "R not replaced within 180s (state=$RSTATE stage=$(json stage))"
  sleep 1   # was 2 (lane speed2): …/replace and the clip GET carry no limiter; the deadline is time
done

# 4. What changed, and what did not.
code=$(get "$BASE/api/clips/$R" -b "$JARW"); expect "$code" 200 "GET R after the replacement"
expect "$(json state)" ready "R state after the replacement"
expect "$(json width)" 1280 "R width after the replacement (the landscape file)"
expect "$(json height)" 720 "R height after the replacement"
expect "$(json title)" "E2E replace $STAMP" "the title survives a replacement"
expect "$(json tags.0)" "replace$STAMP" "the tags survive a replacement"
expect "$(json views)" "$R_VIEWS" "the view count survives a replacement"
expect "$(json expires_at)" "$R_EXPIRES" "the lifetime survives a replacement"
expect "$(json ttl_days)" 1 "ttl_days survives a replacement"
expect_empty "$(json trim)" "the trim window must be reset by a replacement"
expect_empty "$(json gif_url)" "the GIF must be dropped by a replacement"
expect "$(json source.filename)" std-720p30.mp4 "source.filename is the replacement's"
expect "$(json urls.page)" "$BASE/$R" "the share URL is unchanged"
code=$(media_head "$BASE/api/clips/$R/video.mp4"); expect_media "$code" 200 "HEAD R video.mp4 after the replacement"
R_VETAG3=$(hdr ETag)
[ "$R_VETAG3" != "$R_VETAG" ] || die "the replacement did not change the rendition ($R_VETAG3)"
# The share page still unfurls as a player, on the same URL, with the new file's dimensions.
code=$(get "$BASE/api/clips/$R" -b "$JARW")
R_PW=$(json poster.width); R_PH=$(json poster.height)
code=$(page "$BASE/$R" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot share page after the replacement"
card_check player "$PAGE" "$BASE" "$R" "E2E replace $STAMP" "$R_PW" "$R_PH" 1280 720 "$SECURE"
# The bucket: the old original is GONE (not merely unreferenced), the new one is there, and the
# superseded rendition, poster and GIF went with it.
code=$(head_ "$BASE/api/clips/$R/original" -b "$JARW"); expect "$code" 200 "HEAD R original after the replacement"
R_OETAG2=$(hdr ETag)
[ "$R_OETAG2" != "$R_OETAG" ] || die "/original still serves the file that was replaced ($R_OETAG2)"
R_SRCKEY2=$(up_admin_key "$R" source)
[ "$R_SRCKEY2" != "$R_SRCKEY" ] || die "the source key did not change across the replacement"
contains "$R_SRCKEY2" "clips/$R/original/source-" || die "the new source key $R_SRCKEY2 is not the per-attempt shape"
prefix_names "$R" > "$E2E_DIR/keys-R-after.txt"
lacks_file "$E2E_DIR/keys-R-after.txt" "${R_SRCKEY##*/}" "the previous original is still in the bucket"
has_file "$E2E_DIR/keys-R-after.txt" "${R_SRCKEY2##*/}" "the new original is not in the bucket"
expect_prefix_count "$R" 4 "R objects after the replacement (original, rendition, poster, probe.json)"
out=$(dc exec -T app clippet admin ls "$R" --stat) || die "admin ls R --stat failed: $out"
if contains "$out" MISSING; then die "admin ls R --stat reports a missing object after the replacement: $out"; fi
# The operator's census counts the kind.
out=$(dc exec -T app clippet admin stats --no-storage) || die "admin stats failed: $out"
contains "$out" "replace=" || die "admin stats does not count the replace kind: $out"

code=$(http DELETE "$BASE/api/clips/$R" -b "$JARW"); expect_deleted "$code" "$R" "DELETE R"
expect_prefix_count "$R" 0 "R prefix empty after the delete"

step 50 "an unauthorized upload is refused before its body (#32): a declared gibibyte is never read"
# The measurement is the point: a 401 that arrives after the server has read a gigabyte is not the
# behaviour this step is about. The probe declares Content-Length: 1073741824, sends the body in
# 64 KiB pieces, and stops the moment an answer comes back — so `sent` is an upper bound on what the
# server was willing to take before refusing.
#
# The elapsed bound is deliberately loose (5 s, not the sub-second this actually measures): three
# lanes share this host and two of them may be running ffmpeg. What the step really pins is the
# BYTES, which no amount of load changes.
# The path argument arrives WITHOUT its leading slash and gets one here: Git Bash rewrites a bare
# `/api/upload` argument into a Windows path (C:/Program Files/Git/api/upload), which would put
# spaces in the request line and earn a 400 from net/http before any handler of ours saw it.
up_bigbody() {
  $PY - "$1" "$2" "$3" <<'PYEOF'
import socket, sys, time
host, port, path = sys.argv[1], int(sys.argv[2]), "/" + sys.argv[3].lstrip("/")
CAP = 16 << 20
chunk = b"x" * 65536
s = socket.create_connection((host, port), timeout=15)
head = ("POST %s HTTP/1.1\r\nHost: %s\r\n"
        "Content-Type: multipart/form-data; boundary=zzzzzzzz\r\n"
        "Content-Length: 1073741824\r\n\r\n" % (path, host)).encode()
t0 = time.time()
s.sendall(head)
sent, resp = 0, b""
s.settimeout(0.2)
while time.time() - t0 < 15 and sent < CAP:
    try:
        data = s.recv(65536)
        if data:
            resp += data
        break                      # answered, or closed on us: stop pushing
    except socket.timeout:
        pass
    try:
        sent += s.send(chunk)      # blocks until the socket buffer takes some
    except socket.timeout:
        break                      # the server is not reading: that IS the result
    except OSError:
        break
elapsed = time.time() - t0
s.settimeout(3)
try:
    while True:
        data = s.recv(65536)
        if not data:
            break
        resp += data
except Exception:
    pass
s.close()
head_text = resp.decode("latin1", "replace").split("\r\n\r\n", 1)[0]
print("SENT=%d ELAPSED=%.3f" % (sent, elapsed))
print(head_text.replace("\r\n", " | "))
PYEOF
}
BASE_HOST=${BASE#*://}; BASE_HOST=${BASE_HOST%%/*}
BIG_HOST=${BASE_HOST%%:*}
BIG_PORT=${BASE_HOST#*:}
if [ "$BIG_PORT" = "$BASE_HOST" ]; then     # no ":port" in the origin: the scheme's own default
  case $BASE in https://*) BIG_PORT=443 ;; *) BIG_PORT=80 ;; esac
fi
if [ "$SECURE" = 1 ]; then
  echo "[e2e]   NOTE: skipping the raw-socket probe on an https base (it speaks HTTP/1.1 in the clear);"
  echo "[e2e]         the refusal itself is still checked below over curl"
else
  for path in api/upload api/clips; do
    out=$(up_bigbody "$BIG_HOST" "$BIG_PORT" "$path") || die "the big-body probe failed for /$path: $out"
    printf '%s\n' "$out" | sed -n 's/^/[e2e]   /p'
    BIG_SENT=$(printf '%s\n' "$out" | sed -n 's/^SENT=\([0-9]*\) .*/\1/p')
    BIG_ELAPSED=$(printf '%s\n' "$out" | sed -n 's/^SENT=[0-9]* ELAPSED=\([0-9.]*\)$/\1/p')
    contains "$out" " 401 " || die "/$path with no credential answered $(printf '%s' "$out" | tail -1)"
    contains "$out" "Connection: close" || die "/$path did not close the connection on a 1 GiB refusal"
    [ -n "$BIG_SENT" ] || die "the probe reported no byte count for /$path"
    [ "$BIG_SENT" -lt 8388608 ] || die "/$path accepted $BIG_SENT bytes of a body it refused (want under 8 MiB)"
    awk -v e="$BIG_ELAPSED" 'BEGIN { exit (e < 5.0) ? 0 : 1 }' \
      || die "/$path took ${BIG_ELAPSED}s to refuse a body it never read"
  done
fi
# The same refusal through curl, with a body that really is sent: a revoked credential gets a clean
# JSON envelope rather than a truncated connection, which is what a capture tool has to read.
code=$(http_refused POST "$BASE/api/upload" -H 'Authorization: Bearer ck_notarealtokenatall' \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4")
expect "$code" 401 "POST /api/upload with a bad token"
expect "$(json error)" unauthorized "bad-token error code"
# …and nothing it sent was kept.
code=$(get "$BASE/api/clips?limit=100" -b "$JARW"); expect "$code" 200 "list clips after the refusals"
lacks_file "$RESP" "E2E cancelled $STAMP" "a refused upload became a clip"

# This bundle's rows in the closing summary table (scripts/e2e.d/README.md).
summary_row N1 "${N1:-}" "no-JS form (#7): every field BEFORE the file part reaches the clip; deleted"
summary_row N5 "${N5:-}" "the same body with tags/ttl_days/unlisted AFTER the file part: all three dropped; deleted"
summary_row N2 "${N2:-}" "two file parts in one body -> exactly ONE clip, the second part unread; deleted"
summary_row N6 "${N6:-}" "a capture tool's file name, no title: the date-and-time fallback title (317); deleted"
summary_row N3 "${N3:-}" "the sequential batch app.js sends, 1/2; deleted"
summary_row N4 "${N4:-}" "the sequential batch, 2/2; deleted"
summary_row R "${R:-}" "replacement (#31): same link and metadata, new file, the old original served until the swap commits; deleted"
