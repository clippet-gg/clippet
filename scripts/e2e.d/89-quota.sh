# 89-quota.sh — the storage quota as a headline and a CAP, and the pricing presets
# (ROADMAP #40 (2)+(3), §20 erratum 277): STORAGE_PRICING=b2 prices the panel from a dated list,
# STORAGE_BUDGET_BYTES draws the line, STORAGE_BUDGET_HARD=1 makes it hold — and a delete makes
# room again at once.
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 89.
#
# e2e-schedule: parallel
# e2e-namespace: account=q prefix=QO_ buckets=-
# e2e-covers: internal/cost/** internal/db/stats.go internal/httpapi/settings.go internal/httpapi/upload.go
# e2e-covers: web/templates/settings.html
#   It runs a SECOND `clippet serve` against a database of its own, restarted once mid-step, and
#   nothing it asserts is about the stack under test at all — its budget, its prices and its cap are
#   all read at ITS boot. What kept it exclusive was the cost of a second app process beside a bundle
#   streaming a rendition; measured, it is one 320x180 two-second clip and four or five seconds of
#   wall (§20 erratum 283 (e)). Its objects live under an
#   `e2e-quota-<ts>/` prefix of its own at the bucket ROOT, removed by the probe and again by the
#   host's EXIT trap — which the pool makes SAFER, not riskier: `run_bundle_` sources each bundle in a
#   subshell of its own, so that trap is scoped to this bundle instead of to the harness's own shell.
#
# **It brings its own instance, and touches the stack's only through the bucket.** The stack under
# test may be unpriced, unbudgetted and uncapped — the reference stack is all three — and none of
# what this step proves could be reached by configuring it, because a budget is read at boot. So:
# one throwaway `dc run --rm` container with its own DATA_DIR, its own SCRATCH_DIR (the third
# convention in scripts/e2e.d/README.md: a private database over the LIVE /scratch has its boot
# janitor delete an in-flight upload's working directory) and its own S3_KEY_PREFIX in the shared
# bucket, which it empties before it exits. `--rm` removes the container on success, on failure and
# on the trap below.
#
# Everything happens INSIDE that container, over its own loopback, for two reasons: nothing has to
# be published to the host, and the probe's PUBLIC_BASE_URL is then `http` whatever the stack under
# test uses — a session cookie minted against an https base carries `Secure` and would never come
# back over the container's own loopback (this is exactly how a public-origin run would have broken).
#
# Variables are prefixed QO_ (quota) so nothing here can clobber a harness global.

# ---------------------------------------------------------------------------------------------------
step 89 "the quota is the panel's headline, STORAGE_BUDGET_HARD=1 makes it a cap, and STORAGE_PRICING names a dated list"

QO_PREFIX="e2e-quota-$(date +%s)/"
QO_USER="${E2E_USER}_q"
QO_PASS="$E2E_PASS"
QO_OUT="$E2E_DIR/quota-probe.txt"
# The prefix is emptied INSIDE the container on the happy path; an `exit 3`/`exit 4` or an interrupt
# would leave `e2e-quota-<ts>/` objects behind. So the host holds an EXIT trap too (73-ops.sh's
# pattern): best effort, never the cause of a failure, released once the step has proven the prefix
# empty itself.
#
# It removes them with the harness's `store_obj rm --prefix` — the app's own `admin objects`, errata
# 281 (f) and 346 — and never with a client of its own: the objects are in whichever bucket THIS STACK
# writes to — R2 on the reference stack — and a trap pointed at a bundled server tidied up nothing
# while reporting success. The prefix is given from the bucket's root, for the reason the paragraph
# above gives: `-e S3_KEY_PREFIX` REPLACES compose's value, so $QO_PREFIX already IS the whole prefix.
qo_rm_prefix() {
  if ! store_has_bucket; then
    echo "[e2e]   NOTE: this stack keeps its media in a directory, so the quota probe's prefix $QO_PREFIX is in no bucket for the harness to sweep"
    return 0
  fi
  # The prefix must be the harness's own minted shape before `rm --prefix` sees it (§20 erratum
  # 290, AUDIT #28): an empty variable here would have named the bucket root.
  store_rm_prefix_ok_ "${QO_PREFIX:-}" || return 0
  # A subshell: `store_obj` DIES on a failure, and this one promises never to be the cause of one.
  ( store_obj rm --prefix "$QO_PREFIX" >/dev/null ) 2>/dev/null \
    || echo "[e2e]   NOTE: could not remove the quota probe's prefix $QO_PREFIX (its own objects, outside the stack's key prefix)"
}
trap 'qo_rm_prefix' EXIT
echo "[e2e]   probe container: its own database, its own scratch, bucket prefix $QO_PREFIX"

# The in-container script is written to a file rather than inlined: it is long, it is the thing a
# post-mortem reads, and a quoting mistake inside a `-c '…'` argument is the hardest kind to see.
QO_SH="$E2E_DIR/quota-probe.sh"
cat > "$QO_SH" <<'QUOTA_PROBE'
set -e
mkdir -p /tmp/quota /tmp/quota-scratch

# --- (a) the two boot errors, before anything else runs ------------------------------------------
# A cap with no line to cap at would refuse every upload on a fresh instance; a preset nobody has a
# row for would price it at nothing. Both fail at boot, naming the variables (§4).
QE=$(STORAGE_BUDGET_HARD=1 clippet migrate 2>&1 || true)
echo "hardnobudget=$(printf "%s" "$QE" | tr "\n" " ")"
QE=$(STORAGE_PRICING=nope clippet migrate 2>&1 || true)
echo "badpreset=$(printf "%s" "$QE" | tr "\n" " ")"

clippet migrate >/tmp/migrate.log 2>&1
echo "migrated=$(tail -1 /tmp/migrate.log | tr -d "\r")"

# --- (b) phase one: no quota at all. One clip, and what it actually stores ------------------------
clippet serve >/tmp/serve1.log 2>&1 &
QSRV=$!
i=0
while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
echo "up1ready=$(curl -sS -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/healthz)"

# The clip is MADE here rather than mounted: ffmpeg is in this image, the host's testclips/ is not
# reachable from a `compose run` container, and a lavfi source is the same two seconds every time.
ffmpeg -v error -y -f lavfi -i testsrc2=size=320x180:rate=15:duration=2 \
  -f lavfi -i sine=frequency=440:duration=2 \
  -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac -shortest /tmp/q.mp4
echo "clipbytes=$(wc -c < /tmp/q.mp4 | tr -d " ")"

curl -sS -o /tmp/reg -c /tmp/jar -w "%{http_code}\n" -X POST -H "Content-Type: application/json" \
  -d "{\"username\":\"$QUSER\",\"password\":\"$QPASS\"}" http://127.0.0.1:8080/api/auth/register > /tmp/regcode
echo "register=$(tr -d "\r\n" < /tmp/regcode)"

qup() {   # qup <title> → the status code; the body lands in /tmp/up
  curl -sS -o /tmp/up -w "%{http_code}" -b /tmp/jar \
    -F "title=$1" -F "ttl_days=1" -F duplicate=allow -F "file=@/tmp/q.mp4;type=video/mp4" \
    http://127.0.0.1:8080/api/upload
}
qid() { grep -o "\"id\":\"[^\"]*\"" /tmp/up | head -1 | cut -d"\"" -f4; }
qidle() {  # wait for this instance's own queue to go quiet
  i=0
  while [ $i -lt 180 ]; do
    clippet admin stats --no-storage > /tmp/st 2>&1 || true
    grep -q "queued=0 running=0" /tmp/st && return 0
    i=$((i+1)); sleep 1
  done
  return 0
}

echo "up1=$(qup "quota one")"
QID1=$(qid)
echo "id1=$QID1"
qidle
# The line is drawn at the LIVE database aggregate — `admin stats`' `used=`, which is
# db.StorageUsed and is exactly what the cap compares. NOT /healthz's stored_bytes: that is the
# sweeper's cached reading, and a probe that read it seconds after boot got the 0 the sweeper
# measured before this clip existed (which then made the cap's own boot check refuse a budget of 0 —
# a good failure, in the wrong place).
clippet admin stats --no-storage > /tmp/st1 2>&1 || true
QSTORED=$(sed -n "s/^storage: *used=\([0-9][0-9]*\).*/\1/p" /tmp/st1 | head -1)
echo "stored=$QSTORED"
[ -n "$QSTORED" ] && [ "$QSTORED" -gt 0 ] || { echo "storefail=$(tr "\n" " " < /tmp/st1)"; exit 3; }

kill $QSRV 2>/dev/null || true
wait $QSRV 2>/dev/null || true

# --- (c) phase two: the same database, the line drawn AT what it holds, and the cap armed ---------
# The budget is a boot value, so the only way to draw it at a figure this step measured is to start
# a second process on the same file. `used >= budget` is the refusal, so an instance holding
# exactly its budget has no room at all — the boundary, live.
# EXPORTED rather than prefixed onto `clippet serve`: `admin stats` and `admin doctor` below are
# separate processes of this same shell, and a budget only the server knew about would have them
# reporting `budget=off` about the instance they share a database with.
export STORAGE_BUDGET_BYTES=$QSTORED
export STORAGE_BUDGET_HARD=1
clippet serve >/tmp/serve2.log 2>&1 &
QSRV=$!
i=0
while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
QC=$(curl -sS -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/healthz || true)
echo "up2ready=$QC"
[ "$QC" = 200 ] || { echo "serve2fail=$(tr "\n" " " < /tmp/serve2.log)"; exit 4; }

# The settings panel: the quota row exists, it is FIRST, it is red at the line, and the hint names
# the list the prices came from and the date somebody read it. The row needs the sweeper's cached
# reading, which is taken at Start and then once a tick, so it is polled rather than assumed.
i=0
while [ $i -lt 60 ]; do
  curl -sS -o /tmp/settings -b /tmp/jar http://127.0.0.1:8080/settings
  grep -q "data-storage-quota" /tmp/settings && break
  i=$((i+1)); sleep 1
done
echo "panelquota=$(grep -c "data-storage-quota" /tmp/settings | tr -d " ")"
echo "panelfirst=$(grep -o ">quota<\|>stored<" /tmp/settings | head -1)"
echo "panelred=$(grep -o "rs danger\">[0-9]* %" /tmp/settings | head -1)"
echo "panelhint=$(grep -o "B2 list prices as of [0-9][0-9-]*" /tmp/settings | head -1)"
echo "panelunpriced=$(grep -c "unpriced" /tmp/settings | tr -d " ")"

clippet admin stats --no-storage > /tmp/st2 2>&1 || true
echo "statsquota=$(grep "^quota:" /tmp/st2 | tr -d "\r")"
echo "statsstorage=$(grep "^storage:" /tmp/st2 | tr -d "\r")"
echo "statscost=$(grep "^cost:" /tmp/st2 | tr -d "\r")"

clippet admin doctor > /tmp/doc 2>&1 || true
echo "doctorprices=$(grep "^prices" /tmp/doc | head -1 | tr -d "\r")"

# The refusal itself.
echo "up2=$(qup "quota two")"
echo "up2body=$(tr -d "\r\n" < /tmp/up)"

# …and a delete makes room AT ONCE: the cap reads the database live, not the sweeper's cache, so
# there is nothing to wait for here. A tombstoned clip leaves StorageUsed the moment it is marked.
echo "del1=$(curl -sS -o /dev/null -w "%{http_code}" -b /tmp/jar -X DELETE http://127.0.0.1:8080/api/clips/$QID1)"
echo "up3=$(qup "quota three")"
QID3=$(qid)
echo "id3=$QID3"

# --- (d) this step's own litter, gone before the container is ------------------------------------
qidle
curl -sS -o /dev/null -b /tmp/jar -X DELETE http://127.0.0.1:8080/api/clips/$QID3 || true
i=0
while [ $i -lt 120 ]; do
  clippet admin stats > /tmp/st3 2>&1 || true
  grep -q "^bucket:  objects=0 " /tmp/st3 && break
  i=$((i+1)); sleep 1
done
echo "bucket=$(grep "^bucket:" /tmp/st3 | tr -d "\r")"

kill $QSRV 2>/dev/null || true
wait $QSRV 2>/dev/null || true
echo "logerrors=$(cat /tmp/serve1.log /tmp/serve2.log | grep -c -E "panic|level=ERROR" | tr -d " ")"
echo "done=1"
QUOTA_PROBE

# `dc run --rm` removes the container on success and on failure, which is the whole reason the probe
# is a `run` and not an `up`. Every Discord variable is blanked (the bot's here; Discord's webhook
# and the clip webhook by dc's probe_env_blank): this probe UPLOADS, and an instance that inherited the
# stack's webhook would announce its throwaway clips in somebody's real channel.
#
# `|| QO_RC=$?` rather than `set +e`: the ERR trap fires even with errexit off (47-upload.sh's note),
# and the whole point here is to read the container's output before deciding anything.
QO_RC=0
dc run --rm -T --no-deps \
  -e STORAGE_PRICING=b2 \
  -e "S3_KEY_PREFIX=$QO_PREFIX" \
  -e DATA_DIR=/tmp/quota -e SCRATCH_DIR=/tmp/quota-scratch \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 \
  -e REGISTRATION=open -e SWEEP_INTERVAL=5s -e CAPTURE_READY_WAIT=30s \
  -e DISCORD_APP_ID= -e DISCORD_PUBLIC_KEY= -e DISCORD_BOT_TOKEN= \
  -e "QUSER=$QO_USER" -e "QPASS=$QO_PASS" \
  --entrypoint sh app -c "$(cat "$QO_SH")" > "$QO_OUT" 2>&1 || QO_RC=$?
head -60 "$QO_OUT" | sed -n 's/^/[e2e]   quota-probe: /p'
[ "$QO_RC" = 0 ] || die "the quota probe container failed (exit $QO_RC):
$(cat "$QO_OUT")"
has_file "$QO_OUT" '^done=1' "the quota probe did not run to the end"

# qo_get <key> — one `key=value` line the probe printed. The script that produced them stays in
# .e2e/ beside the output: a probe whose assertions fail is read as a pair.
qo_get() { sed -n "s/^$1=//p" "$QO_OUT" | head -1; }

# --- the boot gates ------------------------------------------------------------------------------
QO_V=$(qo_get hardnobudget)
contains "$QO_V" 'STORAGE_BUDGET_HARD' || die "STORAGE_BUDGET_HARD=1 with no budget did not name the variable: $QO_V"
contains "$QO_V" 'STORAGE_BUDGET_BYTES' || die "…and did not say which budget it needs: $QO_V"
QO_V=$(qo_get badpreset)
contains "$QO_V" 'STORAGE_PRICING' || die "an unknown STORAGE_PRICING did not name the variable: $QO_V"
for QO_P in local r2 b2 s3; do
  contains "$QO_V" "$QO_P" || die "the STORAGE_PRICING boot error does not name the preset '$QO_P': $QO_V"
done
echo "[e2e]   boot: a cap with no budget and an unknown preset are both refused, by name"

# --- the panel -----------------------------------------------------------------------------------
expect "$(qo_get register)" 201 "register on the probe instance"
expect "$(qo_get up1)" 201 "the first upload into an empty capped instance ($(qo_get up1))"
expect_nonempty "$(qo_get id1)" "the first clip's id"
QO_STORED=$(qo_get stored)
expect_nonempty "$QO_STORED" "the probe's stored-bytes reading"
[ "$QO_STORED" -gt 0 ] 2>/dev/null || die "the probe instance stores $QO_STORED bytes after one ready clip"

expect "$(qo_get panelquota)" 1 "the Storage panel has exactly one quota row"
expect "$(qo_get panelfirst)" '>quota<' "the quota row is not the panel's FIRST row"
QO_V=$(qo_get panelred)
expect_nonempty "$QO_V" "the quota row's state cell is not red at the line (want 'rs danger'): $QO_V"
contains "$QO_V" '100 %' || die "an instance AT its line does not read 100 %: $QO_V"
expect "$(qo_get panelhint)" 'B2 list prices as of 2026-09-13' "the panel's hint does not name the preset and the date it was read"
expect "$(qo_get panelunpriced)" 0 "a preset-priced panel still says unpriced"
echo "[e2e]   panel: quota first, red at 100 %, priced from the b2 list of 2026-09-13"

# --- the CLI -------------------------------------------------------------------------------------
QO_V=$(qo_get statsquota)
contains "$QO_V" '(100 %)' || die "admin stats' quota line is not at 100 %: $QO_V"
contains "$QO_V" 'HARD (STORAGE_BUDGET_HARD=1' || die "the quota line does not say the cap is armed: $QO_V"
contains "$(qo_get statsstorage)" "budget=$QO_STORED" || die "admin stats' storage line lost the raw budget: $(qo_get statsstorage)"
QO_V=$(qo_get statscost)
contains "$QO_V" 'B2 list prices as of 2026-09-13 (USD)' || die "the cost line does not name the preset: $QO_V"
contains "$QO_V" 'an ESTIMATE' || die "the cost line does not say it is an estimate: $QO_V"
QO_V=$(qo_get doctorprices)
contains "$QO_V" 'preset b2 (STORAGE_PRICING, read 2026-09-13, USD)' || die "doctor's prices row does not name the preset: $QO_V"
contains "$QO_V" 'storage 0.00695/GB-month' || die "doctor's prices row lost the preset's storage price: $QO_V"
contains "$QO_V" 'after 3x what is stored' || die "doctor's prices row does not model B2's free-egress rule: $QO_V"
echo "[e2e]   cli: the quota line leads, the cost line names the list, doctor reads the preset back"

# --- the cap, and the way out of it --------------------------------------------------------------
expect "$(qo_get up2)" 507 "an upload AT the storage quota ($(qo_get up2body))"
QO_V=$(qo_get up2body)
contains "$QO_V" '"error":"storage_quota"' || die "the refusal's error code: $QO_V"
contains "$QO_V" 'This instance is at its storage quota; delete or expire some clips, or raise the quota.' \
  || die "the refusal does not carry the one sentence: $QO_V"
expect "$(qo_get del1)" 204 "delete the clip that filled the quota"
expect "$(qo_get up3)" 201 "the upload after the delete ($(qo_get up3)) — a delete must make room at once"
expect_nonempty "$(qo_get id3)" "the third clip's id"
echo "[e2e]   cap: 507 at the line with one sentence, and the next upload lands the moment room exists"

# --- and it left nothing behind ------------------------------------------------------------------
QO_V=$(qo_get bucket)
contains "$QO_V" 'objects=0' || die "the probe's own bucket prefix ($QO_PREFIX) is not empty: $QO_V"
# The probe's own reading is taken BEFORE it exits; its database snapshot (erratum 343) lands after it,
# so the host empties the prefix and counts it once the container is gone.
probe_prefix_swept_ "$QO_PREFIX" "the quota probe"
trap - EXIT
if [ "$(qo_get logerrors)" != 0 ]; then
  echo "[e2e]   NOTE: the probe's own app log has $(qo_get logerrors) ERROR/panic line(s) — see $QO_OUT"
fi
summary_row QO "$(qo_get id1)" "quota (#40): probe instance, b2 preset, line at $QO_STORED bytes; 507 at the line, room after a delete; prefix $QO_PREFIX emptied"
