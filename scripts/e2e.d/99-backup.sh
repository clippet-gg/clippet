# 99-backup.sh — timed backups of the database, OFF by default (ROADMAP #40 item 4, DESIGN §20 erratum 321).
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 99.
#
# Two halves. (a) The stack under test has timed backups OFF and its own `admin doctor` says so: that is
# the proof of "enabled nowhere" — every compose file passes BACKUP_EVERY/BACKUP_DIR/BACKUP_KEEP through
# empty. (b) A throwaway `--rm` container turns them on with a 10 s interval and BACKUP_KEEP=2 and shows
# a snapshot appear, rotate, restore, and step aside for an unfinished one.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=BK_ buckets=-
# e2e-covers: cmd/clippet/backup_schedule.go cmd/clippet/admin_backup_dir.go internal/db/backup.go
#   Everything (b) writes is inside its own `--rm` container's /tmp: a private DATA_DIR and SCRATCH_DIR
#   (the README's rule for a probe that boots `clippet serve`), a private BACKUP_DIR, and S3_ENDPOINT on
#   a CLOSED port (73-ops's form), so the probe never reaches the bucket — which also shows a backup does
#   not wait on storage. Nothing is left on the host, in the bucket or on the stack's database. (a) is one
#   read of the stack's own doctor. No account, no clip, no limiter.
#
# Variables are prefixed BK_ so nothing here can clobber a harness global.

step 99 "timed backups (#40): off on this stack; on a throwaway instance a snapshot every 10 s, kept to two, skipped beside an unfinished one"

# --- (a) off on the stack under test ------------------------------------------------------------------
# Doctor's exit code is 51-admin's to assert; this half reads one row.
BK_DOC="$E2E_DIR/backup-doctor.txt"
dc exec -T app clippet admin doctor > "$BK_DOC" 2>&1 || true
has_file_f "$BK_DOC" "backup   ok   off: BACKUP_EVERY and BACKUP_DIR are unset (DEPLOY.md §15.6)" \
  "the stack under test must have timed backups OFF, and doctor must say so"

# --- (b) on, in a throwaway container ------------------------------------------------------------------
BK_OUT="$E2E_DIR/backup-probe.txt"
dc run --rm -T --no-deps \
  -e DATA_DIR=/tmp/bk -e SCRATCH_DIR=/tmp/bk-scratch \
  -e S3_ENDPOINT=http://127.0.0.1:1 -e STORAGE_PING_ATTEMPTS=2 -e STORAGE_PING_INTERVAL=1s \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 -e LOG_FORMAT=text \
  -e DISCORD_APP_ID= -e DISCORD_PUBLIC_KEY= -e DISCORD_BOT_TOKEN= -e DROP_FOLDER_OWNER= \
  -e BACKUP_EVERY=10s -e BACKUP_DIR=/tmp/bk-out -e BACKUP_KEEP=2 \
  --entrypoint sh app -c '
    set -e
    mkdir -p /tmp/bk /tmp/bk-scratch /tmp/bk-out
    echo "not a snapshot" > /tmp/bk-out/README
    runs() { grep -cE "msg=\"backup (written|skipped|failed)\"" /tmp/serve.log || true; }

    # 1. one of the pair without the other is a configuration error naming it
    BACKUP_DIR= clippet admin doctor > /tmp/half.txt 2>&1 || true
    echo "half=$(grep "^config" /tmp/half.txt | grep BACKUP_EVERY | head -1)"

    # 2. before the server has run: nothing recorded
    clippet migrate >/dev/null
    clippet admin doctor > /tmp/d1.txt 2>&1 || true
    echo "before=$(grep "^backup" /tmp/d1.txt | tr "\n" "|")"

    # 3. boot, and three runs ten seconds apart
    clippet serve > /tmp/serve.log 2>&1 &
    SP=$!
    i=0; while [ $i -lt 40 ]; do [ "$(grep -c "msg=\"backup written\"" /tmp/serve.log || true)" -ge 3 ] && break; i=$((i+1)); sleep 1; done
    echo "waited=$i"
    echo "scheduled=$(grep -c "msg=\"backup scheduled\"" /tmp/serve.log || true)"
    echo "written=$(grep -c "msg=\"backup written\"" /tmp/serve.log || true)"
    echo "lastwritten=$(grep "msg=\"backup written\"" /tmp/serve.log | tail -1)"
    echo "snaps=$(ls /tmp/bk-out | grep -cE "^clippet-[0-9]{8}T[0-9]{6}Z\.db$" || true)"
    echo "decoy=$(cat /tmp/bk-out/README)"
    for f in /tmp/bk-out/clippet-*.db; do
      echo "mode=$(stat -c %a "$f")"
      if clippet admin restore-check "$f" >/dev/null 2>&1; then echo "restore=ok"; else echo "restore=FAILED $f"; fi
    done

    # 4. an unfinished snapshot under an hour old: the next run steps aside
    before=$(runs)
    touch /tmp/bk-out/clippet-20260101T000000Z.db.part-e2e99
    i=0; while [ $i -lt 25 ]; do [ "$(runs)" -gt "$before" ] && break; i=$((i+1)); sleep 1; done
    echo "afterplant=$(grep -E "msg=\"backup (written|skipped|failed)\"" /tmp/serve.log | sed -n "$((before+1))p")"

    # 5. doctor reads what the server recorded: the skip did not replace the last result
    clippet admin doctor > /tmp/d2.txt 2>&1 || true
    echo "after=$(grep "^backup" /tmp/d2.txt | tr "\n" "|")"

    # 6. a clean stop
    kill -TERM $SP
    wait $SP || true
    echo "complete=$(grep -c "msg=\"shutdown complete\"" /tmp/serve.log || true)"
    echo "didnotstop=$(grep -c "did not stop" /tmp/serve.log || true)"
    echo "failed=$(grep -c "msg=\"backup failed\"" /tmp/serve.log || true)"
  ' > "$BK_OUT" 2>&1 || die "the timed-backup probe container failed:
$(cat "$BK_OUT")"
sed -n 's/^/[e2e]   backup: /p' "$BK_OUT"
bk() { sed -n "s/^$1=//p" "$BK_OUT" | tail -1 | tr -d '\r'; }

contains "$(bk half)" "BACKUP_EVERY: is set and BACKUP_DIR is not: timed backups need both" \
  || die "BACKUP_EVERY without BACKUP_DIR must be a configuration error naming it: $(bk half)"
contains "$(bk before)" "backup   ok   on: a database snapshot every 10s into /tmp/bk-out, newest 2 kept (no media)" \
  || die "doctor's on line: $(bk before)"
contains "$(bk before)" "no run recorded yet" || die "doctor before the first run: $(bk before)"
expect "$(bk scheduled)" 1 "the boot's one 'backup scheduled' line"
[ "$(bk written)" -ge 3 ] || die "three runs within 40 s of boot: $(bk written) written ($(bk waited) s)"
contains "$(bk lastwritten)" "file=/tmp/bk-out/clippet-" || die "the written line names the file: $(bk lastwritten)"
expect "$(bk snaps)" 2 "BACKUP_KEEP=2 after three runs"
expect "$(bk decoy)" "not a snapshot" "the file rotation did not mint"
for m in $(sed -n 's/^mode=//p' "$BK_OUT" | tr -d '\r'); do expect "$m" 600 "a snapshot's mode"; done
expect "$(grep -c '^restore=ok' "$BK_OUT" || true)" 2 "admin restore-check on both snapshots"
contains "$(bk afterplant)" 'msg="backup skipped"' || die "the run after a fresh .part- file must be skipped: $(bk afterplant)"
contains "$(bk after)" "backup   ok   last: " || die "doctor after the skip: $(bk after)"
contains "$(bk after)" " ok, /tmp/bk-out/clippet-" || die "the skip replaced the last real result: $(bk after)"
# `next: <time>`, and never `next: was due <time>`: the skip moved `next` in the status file (§20
# erratum 321, deviation 1), so the doctor run right after it reads a time ten seconds AHEAD. A skip
# that left `next` alone would leave the time of the skipped run itself — already past — and doctor
# says `was due` for exactly that, which a bare "next: " prefix also matched.
printf '%s' "$(bk after)" | grep -Eq 'backup   ok   next: [0-9]{4}-' \
  || die "doctor's next line after the skip is not a time ahead (a skip must move next): $(bk after)"
expect "$(bk failed)" 0 "no backup run failed"
expect "$(bk complete)" 1 "the probe's clean shutdown"
expect "$(bk didnotstop)" 0 "a budget warning at shutdown"
echo "[e2e]   timed backups: off on this stack; the probe wrote $(bk written), kept 2 (0600, both restore), skipped beside an unfinished one, and stopped clean"
summary_row BK "" "timed backups (#40): off on the stack; a --rm probe in its own /tmp wrote, rotated to 2 and skipped — nothing on the host or in the bucket"
