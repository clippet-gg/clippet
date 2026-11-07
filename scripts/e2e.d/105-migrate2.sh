# 105-migrate2.sh — `admin storage migrate` bucket to bucket, the manifest, the stop file and the
# byte cap (ROADMAP #56, §20 erratum 335): the verb the desktop app's Storage screen drives.
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 105.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=MG_ buckets=-
# e2e-covers: cmd/clippet/admin_storage.go internal/store/etag.go
#   89-quota's shape: one throwaway `dc run --rm` container with its own DATA_DIR, its own SCRATCH_DIR
#   and its own S3_KEY_PREFIX (`e2e-mg-<epoch>/`) at the bucket root; one small upload over its own
#   loopback, then `admin storage migrate --to s3` with MIGRATE_S3_* = this stack's own store under a
#   SECOND prefix (`e2e-mg2-<epoch>/`). Its account lives in the probe's private database and never
#   on this stack. Both prefixes are removed by `store_obj rm --prefix` in the host's EXIT trap.
#
# Variables are prefixed MG_ so nothing here can clobber a harness global.

# ---------------------------------------------------------------------------------------------------
step 105 "admin storage migrate bucket to bucket (#56): MIGRATE_S3_*, --manifest, --max-bytes and --stop-file (exit 3)"

if ! store_has_bucket; then
  echo "[e2e]   NOTE: this stack keeps its media in a directory, so there is no bucket to migrate between — step 105 stands down"
  summary_row MG "-" "migrate2 (#56): stood down — this stack has no bucket"
else
MG_TS=$(date +%s)
MG_PREFIX="e2e-mg-$MG_TS/"
MG_PREFIX2="e2e-mg2-$MG_TS/"
MG_OUT="$E2E_DIR/migrate2-probe.txt"

# Both prefixes are the harness's own minted shape before `rm --prefix` sees them (§20
# erratum 290): an empty variable here would have named the bucket root. Best effort, never the cause
# of a failure.
mg_rm_prefixes() {
  for MG_P in "${MG_PREFIX:-}" "${MG_PREFIX2:-}"; do
    store_rm_prefix_ok_ "$MG_P" || continue
    ( store_obj rm --prefix "$MG_P" >/dev/null ) 2>/dev/null \
      || echo "[e2e]   NOTE: could not remove the migrate probe's prefix $MG_P"
  done
}
trap 'mg_rm_prefixes' EXIT
echo "[e2e]   probe container: its own database, its own scratch, prefixes $MG_PREFIX → $MG_PREFIX2"

MG_SH="$E2E_DIR/migrate2-probe.sh"
cat > "$MG_SH" <<'MIGRATE2_PROBE'
set -e
mkdir -p /tmp/mg /tmp/mg-scratch
clippet migrate >/tmp/migrate.log 2>&1

clippet serve >/tmp/serve.log 2>&1 &
MSRV=$!
i=0
while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
echo "ready=$(curl -sS -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/healthz)"

ffmpeg -v error -y -f lavfi -i testsrc2=size=320x180:rate=15:duration=2 \
  -f lavfi -i sine=frequency=440:duration=2 \
  -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac -shortest /tmp/m.mp4
curl -sS -o /tmp/reg -c /tmp/jar -w "%{http_code}" -X POST -H "Content-Type: application/json" \
  -d "{\"username\":\"migrate2\",\"password\":\"$MPASS\"}" http://127.0.0.1:8080/api/auth/register > /tmp/regcode
echo "register=$(tr -d "\r\n" < /tmp/regcode)"
echo "up=$(curl -sS -o /tmp/up -w "%{http_code}" -b /tmp/jar -F "title=migrate two" -F ttl_days=1 \
  -F duplicate=allow -F "file=@/tmp/m.mp4;type=video/mp4" http://127.0.0.1:8080/api/upload)"
echo "id=$(grep -o "\"id\":\"[^\"]*\"" /tmp/up | head -1 | cut -d"\"" -f4)"
i=0
while [ $i -lt 180 ]; do
  clippet admin stats --no-storage > /tmp/st 2>&1 || true
  grep -q "queued=0 running=0" /tmp/st && break
  i=$((i+1)); sleep 1
done
kill $MSRV 2>/dev/null || true
wait $MSRV 2>/dev/null || true

# The destination is this stack's own store, under a second prefix: the seven MIGRATE_S3_* are the
# S3_* this container was handed, read for this one verb.
export MIGRATE_S3_ENDPOINT="$S3_ENDPOINT" MIGRATE_S3_BUCKET="$S3_BUCKET" \
  MIGRATE_S3_ACCESS_KEY="$S3_ACCESS_KEY" MIGRATE_S3_SECRET_KEY="$S3_SECRET_KEY" \
  MIGRATE_S3_REGION="$S3_REGION" MIGRATE_S3_FORCE_PATH_STYLE="$S3_FORCE_PATH_STYLE" \
  MIGRATE_S3_KEY_PREFIX="$MPREFIX2"
mrun() {  # mrun <name> <args…> → <name>.log and <name>rc=
  n=$1; shift
  rc=0
  clippet admin storage migrate --to s3 "$@" > /tmp/$n.log 2>&1 || rc=$?
  echo "${n}rc=$rc"
  sed "s/^/$n: /" /tmp/$n.log
}
mrun first --manifest /tmp/mg.manifest
echo "manifestlines=$(wc -l < /tmp/mg.manifest | tr -d " ")"
mrun second --manifest /tmp/mg.manifest
mrun cap --max-bytes 1
touch /tmp/mg.stop
mrun stop --stop-file /tmp/mg.stop
# A partial MIGRATE_S3_* set names what is missing (exit 2) and copies nothing.
MIGRATE_S3_SECRET_KEY= mrun partial
echo "logerrors=$(grep -c -E "panic|level=ERROR" /tmp/serve.log | tr -d " ")"
echo "done=1"
MIGRATE2_PROBE

# Every Discord variable and the clip webhook are blanked (dc's probe_env_blank and these): this
# probe UPLOADS, and an instance that inherited the stack's webhook would announce its clip.
MG_RC=0
dc run --rm -T --no-deps \
  -e "S3_KEY_PREFIX=$MG_PREFIX" -e "MPREFIX2=$MG_PREFIX2" \
  -e DATA_DIR=/tmp/mg -e SCRATCH_DIR=/tmp/mg-scratch \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 \
  -e REGISTRATION=open -e CAPTURE_READY_WAIT=30s \
  -e DISCORD_APP_ID= -e DISCORD_PUBLIC_KEY= -e DISCORD_BOT_TOKEN= \
  -e "MPASS=$E2E_PASS" \
  --entrypoint sh app -c "$(cat "$MG_SH")" > "$MG_OUT" 2>&1 || MG_RC=$?
grep -E '^(ready|register|up|id|[a-z]+rc|manifestlines)=|: migrate: (from|plan clips|done|stopped)|^partial: ' "$MG_OUT" \
  | sed 's/^/[e2e]   migrate2-probe: /'
[ "$MG_RC" = 0 ] || die "the migrate2 probe container failed (exit $MG_RC):
$(cat "$MG_OUT")"
has_file "$MG_OUT" '^done=1' "the migrate2 probe did not run to the end"
mg_get() { sed -n "s/^$1=//p" "$MG_OUT" | head -1; }

expect "$(mg_get register)" 201 "register on the probe instance"
expect "$(mg_get up)" 201 "the probe's upload"
MG_ID=$(mg_get id); expect_nonempty "$MG_ID" "the probe clip's id"
MG_BK=$(sed -n 's/^first: migrate: from s3 (\([^ ]*\) at .*/\1/p' "$MG_OUT" | head -1)
expect_nonempty "$MG_BK" "the first run's from line"

# (1) bucket to bucket: both stores named (bucket at endpoint, never a key), every object copied.
expect "$(mg_get firstrc)" 0 "migrate --to s3 bucket to bucket"
has_file "$MG_OUT" "^first: migrate: from s3 ($MG_BK at [^)]*) to s3 ($MG_BK at [^)]*)\$" "the from line does not name both buckets"
MG_N=$(sed -n 's/^first: migrate: plan clips=1 objects=\([0-9][0-9]*\) .*/\1/p' "$MG_OUT" | head -1)
expect_nonempty "$MG_N" "the plan's object count"
has_file "$MG_OUT" "^first: migrate: done copied=$MG_N skipped=0 .* failed=0\$" "the first run did not copy every object"
has_file "$MG_OUT" "^first: migrate: copied ${MG_PREFIX}clips/$MG_ID/" "the copied lines do not name the source keys"
lacks_file "$MG_OUT" '^first: migrate: .clippet-store written' "a bucket destination got the directory marker"
expect "$(mg_get manifestlines)" "$MG_N" "the manifest's line count after the first run"

# (2) the manifest: every object skipped on its word, nothing copied.
expect "$(mg_get secondrc)" 0 "the manifest re-run"
expect "$(grep -c '^second: migrate: skip .* (in the manifest)$' "$MG_OUT" | tr -d ' \r')" "$MG_N" "the manifest re-run's in-the-manifest skips"
has_file "$MG_OUT" "^second: migrate: done copied=0 skipped=$MG_N " "the manifest re-run copied something"

# (3) the byte cap and (4) the stop file: exit 3, the stopped line, nothing more.
expect "$(mg_get caprc)" 3 "--max-bytes 1"
has_file "$MG_OUT" "^cap: migrate: stopped reason=max-bytes copied=0 skipped=0 bytes=0 (0 B) failed=0 remaining=$MG_N\$" "the max-bytes stopped line"
expect "$(mg_get stoprc)" 3 "a standing stop file"
has_file "$MG_OUT" "^stop: migrate: stopped reason=stop-file copied=0 skipped=0 bytes=0 (0 B) failed=0 remaining=$MG_N\$" "the stop-file stopped line"
lacks_file "$MG_OUT" '^\(cap\|stop\): migrate: done' "a stopped run printed done"

# (5) a partial MIGRATE_S3_* set.
expect "$(mg_get partialrc)" 2 "a partial MIGRATE_S3_* set"
has_file_f "$MG_OUT" 'partial: admin storage migrate: --to s3 needs MIGRATE_S3_ENDPOINT, MIGRATE_S3_BUCKET, MIGRATE_S3_ACCESS_KEY, MIGRATE_S3_SECRET_KEY in this environment; MIGRATE_S3_SECRET_KEY unset' "the partial set's line"
lacks_file "$MG_OUT" '^partial: migrate: plan' "a partial set planned anyway"
if [ "$(mg_get logerrors)" != 0 ]; then
  echo "[e2e]   NOTE: the probe's own app log has $(mg_get logerrors) ERROR/panic line(s) — see $MG_OUT"
fi
echo "[e2e]   migrate2: $MG_N objects bucket to bucket, $MG_N manifest skips with nothing hashed, exit 3 on the cap and on the stop file"

mg_rm_prefixes
trap - EXIT
summary_row MG "$MG_ID" "migrate2 (#56): bucket to bucket under $MG_PREFIX2, manifest re-run, max-bytes and stop-file stops (exit 3); both prefixes removed"
fi
