# e2e.d/51-admin.sh — bundle G (operator CLI and observability), ROADMAP #9–#14, §20 errata 143–149.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=ORPH_,OPS_,ST_,AD_ buckets=-
# e2e-covers: cmd/clippet/admin*.go cmd/clippet/doctor.go cmd/clippet/ffcaps.go cmd/clippet/volumepath.go
# e2e-covers: cmd/clippet/freebytes_unix.go internal/httpapi/health.go internal/httpx/health.go internal/db/jobs.go
# e2e-covers: internal/db/ops.go internal/db/backup.go internal/db/stats.go internal/worker/runner.go
# e2e-covers: internal/worker/freebytes_unix.go internal/store/probe.go
# e2e-covers: internal/autobot/** internal/notify/auto.go internal/httpapi/autobox.go internal/config/discordauto.go
#   It was exclusive for three reasons until lane `excl`, and none of them survives a reading:
#   - step 52 sweeps the LIVE queue with `admin jobs unstick --older-than 1s` and asserts it repaired
#     nothing. The threshold is RAISED to the TRANSCODE_HARD_TIMEOUT floor (hours) and the step asserts
#     that it was, so the sweep can only ever touch a job that has been running for hours; no bundle
#     makes one, beside it or not. That is a fact about the floor, not about what else is running.
#   - step 53 runs a throwaway instance whose ffmpeg fails on purpose: its own container, database,
#     scratch and S3_KEY_PREFIX, the shape every parallel probe bundle (73-ops, 89-quota) already has;
#     the load it adds is a veryslow encode, which only makes its own "still running" window wider.
#   - step 54 planted objects under the SHARED clip C and counted C's prefix either side. It now plants
#     under a clip of its OWN (AD_C, uploaded at step 51, ready by step 54), so the counts are of a
#     prefix nothing else in the run writes to; the scoped orphans report and delete were already
#     `--clip`. Its bucket-wide reading is the summary line, asserted as a floor since erratum 245.
#   Everything else is a read of the stack (doctor, /healthz, `admin jobs/ls/stats`) or a refusal.
#
# Sourced by scripts/e2e.sh before the step-16 log gate, so every line these steps make is covered by
# it, and before step 17's cleanup, because step 52 reads the owner session's clip C.
#
# What it creates and what it leaves behind:
#   - nothing on the live stack's database. Step 52 is read-only apart from ONE refused `admin retry`;
#   - two THROWAWAY containers (51, 53) on their own databases under /tmp, removed with --rm. Step 53's
#     writes two clips into the live bucket under its OWN `S3_KEY_PREFIX=e2e-ops-<epoch>/`, which no
#     other command on this stack looks at, and destroys both through its own sweeper before it exits —
#     the step asserts the prefix is empty afterwards;
#   - one small clip of its own (AD_C), uploaded at step 51 and deleted at the end of step 54, and two
#     EXTRA objects under its prefix in step 54, both removed inside the step (one by `admin orphans
#     --delete`, one by `admin objects rm`, because `orphans` deliberately refuses to delete an original);
#   - one backup directory under /tmp inside the app container, removed at the end of step 54.
# AD_C is re-checked ready and serving after the sweep, before it is deleted.
#
# Both halves pass over plain http AND over https: nothing below asserts a scheme, and every assertion
# about `doctor` that could differ between a loopback dev origin and a public https one is written as
# "no FAIL", never as "everything is ok".

# doctor_verdict <output-file> <check> → the WORST verdict that check printed (FAIL > skip > warn > ok).
doctor_verdict() {
  awk -v c="$2" 'BEGIN{r["ok"]=1;r["warn"]=2;r["skip"]=3;r["FAIL"]=4;b=0;s=""}
    $1==c && r[$2]>b {b=r[$2]; s=$2} END{print s}' "$1"
}
# doctor_fails <output-file> → every FAIL line, for a message an operator can read.
doctor_fails() { awk '$2=="FAIL"' "$1"; }

step 51 "admin doctor (#9): green on this stack, red on a broken bucket credential and a broken config, never a secret"
# Step 54's subject, uploaded now so its encode is done long before step 54 plants beside it.
code=$(upload AD small-640x360.mp4 '{"title":"E2E orphans subject","ttl_days":1}')
expect "$code" 201 "upload AD_C, the orphans step's own clip ($(cat "$RESP"))"
AD_C=$(json id); expect_nonempty "$AD_C" "AD_C id"
DOC="$E2E_DIR/doctor.txt"
dc exec -T app clippet admin doctor > "$DOC" 2>&1 || die "admin doctor exited non-zero on a working stack:
$(cat "$DOC")"
sed -n 's/^/[e2e]   /p' "$DOC"
# Every check ran. A check that silently stopped running would otherwise read as a clean bill of health.
# The list is ALL FIFTEEN and the count below is the same fifteen: `media` and `bot` were the two
# newest rows and the two this loop did not name, which made them exactly the two a silent
# disappearance would have hidden — the summary line would still have said 14 while the report carried
# a different set (§20 erratum 283 (h)); `drop` is the newest and is in the list for the same reason
# (§20 erratum 296), and `login` joined them with §20 erratum 302.
for chk in config baseurl paths db ffmpeg creds store bucket media drop proxies webhook bot auto login repair backup prices; do
  v=$(doctor_verdict "$DOC" "$chk")
  expect_nonempty "$v" "doctor check '$chk' did not appear in the report"
done
[ -z "$(doctor_fails "$DOC")" ] || die "admin doctor FAILED a check on a working stack:
$(doctor_fails "$DOC")"
# The checks whose verdict cannot legitimately differ between a loopback dev stack and a public https
# one. (`baseurl` and `creds` are ok on either but for different reasons, and `proxies` is a warning
# whenever TRUSTED_PROXIES is set — which DEPLOY §4 tells a tunnelled stack to do.)
for chk in config db ffmpeg bucket prices; do
  expect "$(doctor_verdict "$DOC" "$chk")" ok "doctor check '$chk' on a working stack"
done
# `repair` is asserted apart from that loop and accepts BOTH verdicts, because it differs by .env and
# not by origin: a JOBS_UNSTICK_AFTER under TRANSCODE_HARD_TIMEOUT — the `5m` .env.example suggests and
# DEPLOY §24.6 tells a single-instance operator it "probably should" use — prints a SECOND repair line
# at `warn`, and that is a healthy stack. (The only thing that warning costs is a second process
# sharing one database, which DEPLOY §18 does not support.) What must never happen is the check going
# missing (the loop above), FAILing (`doctor_fails` above) or skipping on a configuration that parsed.
DOC_REPAIR=$(doctor_verdict "$DOC" repair)
case $DOC_REPAIR in
  ok|warn) ;;
  *) die "doctor check 'repair' on a working stack is '$DOC_REPAIR'; ok (or warn, with a short JOBS_UNSTICK_AFTER) is the only healthy pair" ;;
esac
has_file "$DOC" 'probe object put and removed' "the bucket check must PROVE writability, not just reachability"
has_file "$DOC" 'integrity=ok applied=' "the db check must report integrity and the applied schema"
has_file "$DOC" 'doctor:  19 checks' "the summary line"
# It prints no secret — the reason it exists in preference to `docker compose config`.
DOC_SECRET=$(envfile S3_SECRET_KEY); DOC_SECRET=${DOC_SECRET:-$(envfile MINIO_ROOT_PASSWORD)}
DOC_SECRET=${DOC_SECRET:-clippet-dev-secret}
lacks_secret_f "$DOC" "$DOC_SECRET" "admin doctor printed the bucket secret"
if [ -n "${E2E_DISCORD_WEBHOOK_URL:-}" ]; then
  lacks_secret_f "$DOC" "${E2E_DISCORD_WEBHOOK_URL##*/}" "admin doctor printed the Discord webhook token"
  has_file "$DOC" '(token redacted)' "the webhook is reported by its non-secret half"
fi

# --- red (a): a wrong bucket credential, in a throwaway container on its own DATA_DIR --------------
# DATA_DIR is redirected so this probe never opens the live database; the point is that the ONE thing
# it reports as broken is the bucket, and that the report still contains no secret.
DOCBAD="$E2E_DIR/doctor-badkey.txt"
DRC=0
dc run --rm -T --no-deps -e S3_SECRET_KEY=e2e-definitely-not-the-secret -e DATA_DIR=/tmp/doctor \
  app admin doctor > "$DOCBAD" 2>&1 || DRC=$?
expect "$DRC" 1 "admin doctor with a wrong bucket credential"
expect "$(doctor_verdict "$DOCBAD" bucket)" FAIL "the bucket check with a wrong credential"
expect "$(doctor_verdict "$DOCBAD" config)" ok "the configuration still PARSES with a wrong credential"
lacks_file "$DOCBAD" e2e-definitely-not-the-secret "admin doctor echoed the (wrong) secret it was given"
lacks_secret_f "$DOCBAD" "$DOC_SECRET" "admin doctor printed the real bucket secret"
sed -n 's/^/[e2e]   badkey: /p' "$DOCBAD" | grep -E 'bucket|config' || true

# --- red (b): a broken configuration reports EVERY bad variable, and guesses at nothing after it ---
DOCCFG="$E2E_DIR/doctor-badconfig.txt"
DRC=0
dc run --rm -T --no-deps -e PUBLIC_BASE_URL=not-a-url -e TRANSCODE_CRF=99 -e SWEEP_INTERVAL=banana \
  app admin doctor > "$DOCCFG" 2>&1 || DRC=$?
expect "$DRC" 1 "admin doctor with three invalid variables"
for v in PUBLIC_BASE_URL TRANSCODE_CRF SWEEP_INTERVAL; do
  has_file "$DOCCFG" "$v" "doctor must name EVERY invalid variable, not the first ($v missing)"
done
has_file "$DOCCFG" '3 variable(s) invalid' "the count of invalid variables"
for chk in baseurl paths db ffmpeg creds store bucket drop proxies webhook repair backup snapshot prices; do
  expect "$(doctor_verdict "$DOCCFG" "$chk")" skip "check '$chk' with an unparsable configuration"
done
# The line says what was ASSERTED, not "green": `repair` is accepted at ok OR warn a few dozen lines
# above, because a JOBS_UNSTICK_AFTER under TRANSCODE_HARD_TIMEOUT — the value .env.example suggests —
# warns on a perfectly healthy stack. A run that printed "19 checks green" while one of them warned was
# the only thing wrong here; nothing asserts this wording.
echo "[e2e]   doctor: 19 checks, none FAILing or skipped here (this stack is on STORAGE_BACKEND=s3, where 'store' is one line and 'bucket' does the work; step 88 runs the fs side); repair may warn on a short JOBS_UNSTICK_AFTER; a wrong key fails only 'bucket'; a bad .env names all three variables and guesses at nothing"

step 52 "queue observability (#10, #11, #13): /healthz counts and ages, admin jobs/ls filters, free disk, and a retry the §5 rule refuses"
# /healthz gained three ANONYMOUS fields (§20 erratum 145). They are numbers, and the block is closed.
code=$(get "$BASE/healthz"); expect "$code" 200 "GET /healthz"
for f in queued running failed_24h oldest_queued_age_s longest_running_age_s; do
  HV=$(json "queue.$f")
  expect_nonempty "$HV" "healthz queue.$f"
  case $HV in *[!0-9]*) die "healthz queue.$f = '$HV' is not a whole number (the block is counts and ages only)" ;; esac
done
$PY - "$(W "$RESP")" <<'PYEOF' || die "the /healthz queue block is not a closed set of five numbers"
import json, sys
q = json.load(open(sys.argv[1], encoding='utf-8'))['queue']
want = {'queued', 'running', 'failed_24h', 'oldest_queued_age_s', 'longest_running_age_s'}
if set(q) != want:
    sys.exit('queue keys %r, want %r' % (sorted(q), sorted(want)))
for k, v in q.items():
    if not isinstance(v, int):
        sys.exit('queue.%s is %r; the block must never name a clip or a user' % (k, v))
print('healthz queue: %s' % q)
PYEOF
echo "[e2e]   healthz queue: $(cat "$RESP" | tr -d ' ' | sed -n 's/.*\("queue":{[^}]*}\).*/\1/p')"

# admin jobs: the rows, the filters, and the totals block that answers "is anything wrong at all".
JOBS="$E2E_DIR/admin-jobs.txt"
dc exec -T app clippet admin jobs --kind transcode --limit 200 > "$JOBS" 2>&1 || die "admin jobs failed: $(cat "$JOBS")"
has_file "$JOBS" "clip=$C" "admin jobs does not list C's transcode job"
has_file "$JOBS" 'totals:  transcode ' "the per-kind totals block"
has_file "$JOBS" 'totals:  notify ' "a kind with no rows must still be counted, not silently absent"
has_file "$JOBS" 'job(s) (kind=transcode)' "the count line names the filter it applied"
OUT=$(dc exec -T app clippet admin jobs --clip "$C" --state done) || die "admin jobs --clip --state failed: $OUT"
contains "$OUT" "clip=$C" || die "admin jobs --clip $C --state done: $OUT"
contains "$OUT" 'kind=transcode' || die "the done job of a ready clip is its transcode: $OUT"
# A typo is a usage error, never an empty listing: "no jobs in state redy" is the wrong answer.
for probe in "--state:redy" "--kind:transcod" "--clip:not-an-id"; do
  f=${probe%%:*}; v=${probe#*:}
  SC=0; SOUT=$(dc exec -T app clippet admin jobs "$f" "$v" 2>&1) || SC=$?
  expect "$SC" 2 "admin jobs $f $v"
  contains "$SOUT" 'is not a' || die "the refusal does not name the closed set: $SOUT"
done

# admin ls --state / --owner
OUT=$(dc exec -T app clippet admin ls --state ready) || die "admin ls --state ready failed: $OUT"
contains "$OUT" "$C" || die "admin ls --state ready does not list the ready clip C: $OUT"
contains "$OUT" '(state=ready)' || die "the count line names the filter: $OUT"
OUT=$(dc exec -T app clippet admin ls --owner "$E2E_USER") || die "admin ls --owner failed: $OUT"
contains "$OUT" "$C" || die "admin ls --owner $E2E_USER does not list C: $OUT"
SC=0; SOUT=$(dc exec -T app clippet admin ls --state readyy 2>&1) || SC=$?
expect "$SC" 2 "admin ls with a misspelled --state"
SC=0; SOUT=$(dc exec -T app clippet admin ls "$C" --state ready 2>&1) || SC=$?
expect "$SC" 2 "a filter combined with a single <id>"

# admin stats: free disk (#13) and the same three queue observables /healthz reports (#11).
STATS="$E2E_DIR/admin-stats.txt"
dc exec -T app clippet admin stats > "$STATS" 2>&1 || die "admin stats failed: $(cat "$STATS")"
sed -n 's/^/[e2e]   /p' "$STATS"
has_file "$STATS" 'disk:    data_free=' "the parseable disk line"
has_file "$STATS" 'scratch_free=' "scratch free bytes"
has_file "$STATS" 'same_device=' "whether DATA_DIR and SCRATCH_DIR share a filesystem"
has_file "$STATS" 'SCRATCH_MIN_FREE_BYTES=' "the human disk line"
has_file "$STATS" 'queue:   failed_24h=' "the queue observables on the stats page"
lacks_file "$STATS" 'unavailable' "the disk line could not be computed inside the container"

# retry refuses a clip that is not failed: the §5 invariant a second process must not be able to break.
SC=0; SOUT=$(dc exec -T app clippet admin retry "$C" 2>&1) || SC=$?
expect "$SC" 1 "admin retry on a READY clip"
contains "$SOUT" 'not failed (state=ready)' || die "the refusal does not name the state: $SOUT"
code=$(get "$BASE/api/clips/$C" -b "$JARW"); expect "$code" 200 "C after the refused retry"
expect "$(json state)" ready "a refused retry must not move the clip"
SC=0; SOUT=$(dc exec -T app clippet admin retry 2>&1) || SC=$?
expect "$SC" 2 "admin retry with no selector"
contains "$SOUT" 'a selector is required' || die "no-selector retry printed no usage: $SOUT"
SC=0; SOUT=$(dc exec -T app clippet admin jobs unstick 2>&1) || SC=$?
expect "$SC" 2 "admin jobs unstick with no selector"
# The floor, proven on the LIVE stack: a sub-TRANSCODE_HARD_TIMEOUT threshold is RAISED and says so,
# and the sweep touches nothing, because nothing here has been running for six hours.
OUT=$(dc exec -T app clippet admin jobs unstick --older-than 1s) || die "admin jobs unstick failed: $OUT"
contains "$OUT" 'raised to the TRANSCODE_HARD_TIMEOUT floor' || die "a sub-floor --older-than was obeyed silently: $OUT"
contains "$OUT" 'unstuck: 0 job(s)' || die "the sub-floor sweep repaired something on a live stack: $OUT"
echo "[e2e]   jobs/ls filters read the live tables; the unstick floor refuses to touch a live worker's job"

step 53 "outage recovery (#10, #12) in a throwaway instance: a forced-failed transcode is re-queued, and a job left running is unstuck"
# Both halves need an instance whose ffmpeg misbehaves on purpose, which the live stack must not do:
# one container, its own database under /tmp, its own S3_KEY_PREFIX, and both clips destroyed by its
# own sweeper before it exits. Phase A boots with an ffmpeg that answers `-version` and fails every
# encode (so the upload's ffprobe sniff still works and only the ENCODE fails), and the worker is
# stopped BEFORE the recovery verbs run, so what they report cannot be racing a retry the worker has
# already picked up. Phase B boots with the real one at a preset slow enough that the job is still
# running when the process is killed -9, leaving exactly the row §11 promises never stays stuck.
#
# `TRANSCODE_HWACCEL=` (empty) pins the CPU argv: `veryslow` is an x264 preset and the whole reason the
# encode below outlasts the 20 s hard timeout; on the GPU flavour a `compose run` probe inherits `cuda`
# and the card and NVENC would finish long before it (§20 erratum 247, step 73 died exactly so).
# TRANSCODE_HARD_TIMEOUT is 20s here and it is doing two jobs at once: it is long enough that the
# encode is still running a few seconds in (when the process is killed), and it is `unstick`'s age
# floor — which is why the step waits past 21s rather than inventing a shorter threshold the command
# would refuse to honour anyway. It does not SLEEP for that: it polls the age the CLI itself measures
# (`running_for`) until the CLI says the job is past 21 s, which is the only clock the repair consults
# — the container's steps backwards under load, and a fixed sleep ended with a job the CLI still called
# too young (§20 erratum 245 (g)).
OPS_PREFIX="e2e-ops-$(date +%s)/"
OPS="$E2E_DIR/ops-probe.txt"
trap 'probe_prefix_tidy_ "$OPS_PREFIX" "the ops probe"' EXIT   # a die below still leaves the bucket root clean
dc run --rm -T --no-deps \
  -e DATA_DIR=/tmp/ops -e SCRATCH_DIR=/tmp/ops-scratch -e S3_KEY_PREFIX="$OPS_PREFIX" \
  -e REGISTRATION=open -e CAPTURE_READY_WAIT=0 -e SWEEP_INTERVAL=3s -e SWEEP_GRACE=1s \
  -e TRANSCODE_HARD_TIMEOUT=20s -e TRANSCODE_PRESET=veryslow -e TRANSCODE_CRF=18 -e TRANSCODE_HWACCEL= \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 \
  --entrypoint sh app -c '
    set -e
    mkdir -p /tmp/ops /tmp/ops-scratch /tmp/bin
    printf "%s\n" "#!/bin/sh" "case \"\$1\" in -version) exec /usr/local/bin/ffmpeg -version ;; esac" \
      "echo \"e2e: forced encode failure\" >&2" "exit 1" > /tmp/bin/ffmpeg
    chmod +x /tmp/bin/ffmpeg
    wait_up() { i=0; while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && return 0; i=$((i+1)); sleep 1; done; echo "the probe app never became healthy"; exit 1; }
    clip_state() { clippet admin ls "$1" | sed -n "s/^state: *//p" | awk "{print \$1}"; }
    # The clip id is the FIRST "id" in the reply: ClipDTO nests the owner, whose
    # id is also 12 characters, so a greedy match would return the wrong thing.
    upload() { curl -sS -b /tmp/jar -F "metadata={\"title\":\"$1\"};type=application/json" \
        -F duplicate=allow -F "video=@/tmp/in.mp4;type=video/mp4" http://127.0.0.1:8080/api/clips \
        | grep -o "\"id\":\"[A-Za-z0-9_-]\{12\}\"" | head -1 | sed -n "s/.*:\"\(.*\)\"/\1/p"; }
    running_job() { clippet admin jobs --clip "$1" --kind transcode --state running --limit 1 | sed -n "s/^#\([0-9]*\) .*/\1/p" | head -1; }
    clippet migrate >/dev/null

    # --- phase A: every encode fails on purpose ---------------------------------------------------
    FFMPEG_PATH=/tmp/bin/ffmpeg clippet serve >/tmp/serve-a.log 2>&1 &
    SP=$!
    wait_up
    curl -sS -o /dev/null -c /tmp/jar -X POST -H "Content-Type: application/json" \
      -d "{\"username\":\"opsuser\",\"password\":\"ops-pass-12345\"}" http://127.0.0.1:8080/api/auth/register
    /usr/local/bin/ffmpeg -hide_banner -loglevel error -f lavfi -i testsrc2=size=1280x720:rate=60:duration=30 \
      -c:v libx264 -preset ultrafast -pix_fmt yuv420p -movflags +faststart -y /tmp/in.mp4
    P=$(upload "ops failed clip")
    echo "P=$P"
    i=0; while [ $i -lt 90 ]; do S=$(clip_state "$P"); [ "$S" = failed ] && break; i=$((i+1)); sleep 1; done
    echo "pstate=$S"
    # Stop the worker before the recovery verbs run, so what they report is not racing it.
    kill -9 $SP 2>/dev/null || true
    sleep 1
    echo "lsfailed=$(clippet admin ls --state failed | tail -1)"
    echo "lsnamesp=$(clippet admin ls --state failed | grep -c "$P" || true)"
    echo "jobsfailed=$(clippet admin jobs --state failed --kind transcode | grep -c "clip=$P" || true)"
    echo "retryout=$(clippet admin retry "$P" | tail -1)"
    echo "pafterretry=$(clip_state "$P")"
    echo "pjobafter=$(clippet admin jobs --clip "$P" --kind transcode --limit 1 | head -1)"

    # --- phase B: a real encode, killed mid-run, leaves a job `running` with no worker -------------
    FFMPEG_PATH=/usr/local/bin/ffmpeg clippet serve >/tmp/serve-b.log 2>&1 &
    SP=$!
    wait_up
    Q=$(upload "ops stuck clip")
    echo "Q=$Q"
    i=0; JID=
    while [ $i -lt 90 ]; do
      JID=$(running_job "$Q")
      [ -n "$JID" ] && break
      i=$((i+1)); sleep 1
    done
    echo "jid=$JID"
    kill -9 $SP 2>/dev/null || true
    sleep 1
    # The two REFUSALS below assert that a job younger than the 20s floor is repaired by nothing, so
    # they must run while it still IS younger — and every CLI call between the claim and them spends a
    # second of that window (nit sweep S08: with four lanes up, run 1 of an earlier fix reached the
    # sweep 23 s after started= and the step died claiming the sweep had repaired a job that was too
    # young). So they come FIRST, before the observational echoes, and the age the CLI itself prints is
    # recorded so the host can tell "too young, as intended" from "the floor had already passed".
    echo "runningfor=$(clippet admin jobs --clip "$Q" --kind transcode --state running --limit 1 | sed -n "s/.*running_for=\([^ ]*\).*/\1/p" | head -1)"
    YOUNG=$(clippet admin jobs unstick "$JID" 2>&1) && YRC=0 || YRC=$?
    echo "youngrc=$YRC"
    echo "young=$(printf "%s" "$YOUNG" | tr "\n" "|")"
    SWEEP0=$(clippet admin jobs unstick --older-than 20s 2>&1) && S0RC=0 || S0RC=$?
    echo "sweep0rc=$S0RC"
    echo "sweep0=$(printf "%s" "$SWEEP0" | tail -1)"
    echo "qstuckstate=$(clip_state "$Q")"
    echo "qstuckjob=$(clippet admin jobs --clip "$Q" --kind transcode --limit 1 | head -1)"
    # POLL the age the CLI itself measures past the 20s floor rather than sleeping 25 s (the rule of
    # §20 erratum 245, and its (g)). NO APOSTROPHE IN THIS SCRIPT: it is one single-quoted string. The
    # container clock steps BACKWARDS under load (the Docker VM clock -- by 27 s once, on a four-cpu container),
    # and a fixed sleep then ends with the CLI still seeing a job younger than the floor, so "not
    # repaired" would be the verdict of the clock and not of the repair. The age the CLI prints is the
    # age the sweep will use, so that is what the wait is on. Go renders it as 25s / 1m5s / 2h0m0s.
    i=0; RF=
    while [ $i -lt 120 ]; do
      RF=$(clippet admin jobs --clip "$Q" --kind transcode --state running --limit 1 | sed -n "s/.*running_for=\([^ ]*\).*/\1/p" | head -1)
      case $RF in
        *m*|*h*) break ;;
        *s) [ "${RF%s}" -ge 21 ] 2>/dev/null && break ;;
      esac
      i=$((i+1)); sleep 1
    done
    echo "waited=${i}s agebeforesweep=${RF:-none}"
    SWEEP1=$(clippet admin jobs unstick --older-than 20s 2>&1) && S1RC=0 || S1RC=$?
    echo "sweep1rc=$S1RC"
    echo "sweep1=$(printf "%s" "$SWEEP1" | tr "\n" "|")"
    echo "qafter=$(clip_state "$Q")"
    echo "qjobafter=$(clippet admin jobs --clip "$Q" --kind transcode --limit 1 | head -1)"
    AGAIN=$(clippet admin jobs unstick "$JID" 2>&1) && ARC=0 || ARC=$?
    echo "againrc=$ARC"
    echo "again=$(printf "%s" "$AGAIN" | tr "\n" "|")"

    # --- cleanup: both clips destroyed by this instance own sweeper, then the prefix proven empty --
    FFMPEG_PATH=/tmp/bin/ffmpeg clippet serve >/tmp/serve-c.log 2>&1 &
    SP=$!
    wait_up
    clippet admin expire "$P" --ago 2h >/dev/null
    clippet admin expire "$Q" --ago 2h >/dev/null
    i=0; while [ $i -lt 60 ]; do
      N=$(clippet admin stats --no-storage | sed -n "s/^clips: *\([0-9]*\).*/\1/p")
      [ "$N" = 0 ] && break
      i=$((i+1)); sleep 2
    done
    echo "clipsleft=$N"
    echo "bucketleft=$(clippet admin stats | sed -n "s/^bucket:  //p")"
    kill -9 $SP 2>/dev/null || true
  ' > "$OPS" 2>&1 || die "the outage-recovery probe container failed:
$(cat "$OPS")"
sed -n 's/^/[e2e]   ops-probe: /p' "$OPS"
ops() { sed -n "s/^$1=//p" "$OPS" | tail -1 | tr -d '\r'; }
expect_nonempty "$(ops P)" "the forced-failure clip id"
expect "$(ops pstate)" failed "the forced-failure clip reached the failed state"
[ "$(ops lsnamesp)" -ge 1 ] 2>/dev/null || die "admin ls --state failed did not name the failed clip: $(ops lsfailed)"
[ "$(ops jobsfailed)" -ge 1 ] 2>/dev/null || die "admin jobs --state failed did not name its transcode job"
contains "$(ops retryout)" '1 re-queued, 0 skipped' || die "admin retry on a failed clip: $(ops retryout)"
expect "$(ops pafterretry)" queued "the retried clip goes back to queued"
contains "$(ops pjobafter)" 'state=queued' || die "the retried transcode job: $(ops pjobafter)"
contains "$(ops pjobafter)" 'attempts=0' || die "a retry resets the attempt counter: $(ops pjobafter)"
# The stuck half.
expect_nonempty "$(ops Q)" "the stuck clip id"
expect_nonempty "$(ops jid)" "the job id of the killed run (the encode finished before it could be killed?)"
expect "$(ops qstuckstate)" processing "a killed worker leaves the clip processing (this is the stuck state)"
contains "$(ops qstuckjob)" 'state=running' || die "the killed run left no running job: $(ops qstuckjob)"
# The two refusals are only a fact while the job is younger than the 20s floor. The probe prints the
# age the CLI itself measured immediately before them; anything at or past the floor is a load
# artefact of THIS run and is reported as a skip, never as a failure and never by loosening the floor
# (nit sweep S08).
OPS_RF=$(ops runningfor)
case ${OPS_RF:-unknown} in
  *m*|*h*) OPS_YOUNG=0 ;;
  *s)      OPS_YOUNG=$([ "${OPS_RF%s}" -lt 20 ] 2>/dev/null && echo 1 || echo 0) ;;
  *)       OPS_YOUNG=0 ;;
esac
if [ "$OPS_YOUNG" = 1 ]; then
  expect "$(ops youngrc)" 1 "unsticking a job younger than TRANSCODE_HARD_TIMEOUT (running_for=$OPS_RF)"
  contains "$(ops young)" 'a worker may still be on it' || die "the too-young refusal: $(ops young)"
  expect "$(ops sweep0rc)" 0 "the sweep with nothing old enough"
  contains "$(ops sweep0)" 'unstuck: 0 job(s)' || die "the sweep repaired a job that was too young: $(ops sweep0)"
else
  echo "[e2e]   SKIP the two too-young refusals: the killed job was already running_for=${OPS_RF:-unknown} when they ran (>= the 20s floor), which is this host's load and not the floor's behaviour"
fi
expect "$(ops sweep1rc)" 0 "the sweep once the job is past the floor"
contains "$(ops sweep1)" 'unstuck: 1 job(s)' || die "the stale job was not repaired: $(ops sweep1)"
contains "$(ops sweep1)" 'job requeued, clip queued' || die "the repair did not apply the §5 coupling: $(ops sweep1)"
expect "$(ops qafter)" queued "the unstuck clip is claimable again (§11: never stuck)"
contains "$(ops qjobafter)" 'state=queued' || die "the unstuck job: $(ops qjobafter)"
contains "$(ops qjobafter)" 'attempts=1' || die "a crash-class repair spends exactly one attempt: $(ops qjobafter)"
expect "$(ops againrc)" 1 "unsticking the same job twice"
contains "$(ops again)" 'not running' || die "the second unstick: $(ops again)"
# It cleaned up after itself: no rows, and nothing left under its own key prefix.
expect "$(ops clipsleft)" 0 "the probe instance destroyed both of its clips"
contains "$(ops bucketleft)" 'objects=0' || die "the probe left objects under $OPS_PREFIX: $(ops bucketleft)"
probe_prefix_swept_ "$OPS_PREFIX" "the ops probe"   # what the instance wrote after its own tidy (its DB snapshot)
trap - EXIT
echo "[e2e]   throwaway instance: a failed transcode listed and re-queued; a job left running repaired under the hard-timeout floor; prefix $OPS_PREFIX empty"

step 54 "orphans' third report (#13) and backup rotation (#14): extras under a LIVE clip, and a directory that keeps N"
# An `extra` object is what an interrupted trim leaves: the superseded rendition is deleted AFTER the
# commit, best effort and never retried, so a crash in that window leaves a paid-for object under a
# perfectly healthy clip that no column names. Two are planted under AD_C — one replaceable, one under
# `original/`, which the upload-is-sacred rule (§11) says must be reported and never deleted.
#
# AD_C (this bundle's own clip — C is shared, and a prefix another bundle could write to is not one to
# count) is `ready` with a DONE transcode, which is the only shape this step can assert on. The other
# safeguard (§20 erratum 150: a clip with a queued or running job is skipped entirely, because a
# job's CHECKPOINT can reference an object no column names) needs a clip held mid-job for the length
# of a plant-and-sweep, and nothing on a live stack holds one for a bounded time. It is proven in
# cmd/clippet instead — TestAdminOrphansNeverDeletesTheOutputsOfALiveJob checkpoints a running job,
# sweeps, and then finalizes onto the objects that survived.
# Every plant below goes through the harness's `store_obj put` (`admin objects put`), so a stack with no bucket at all
# (STORAGE_BACKEND=fs) has nothing here to plant into and says so rather than failing:
# §20 erratum 281 (f). Everything else in step 54 — the backup rotation below — is unaffected.
wait_ready "$AD_C" AD_C
if store_has_bucket; then
  # The two keys are written under the STACK'S OWN key prefix, because the whole point is that
  # `admin orphans` — which lists what the app itself would have written — finds them. $SM_PREFIX is
  # S3_KEY_PREFIX as the APP normalizes it (leading slashes off, exactly one trailing slash), so
  # EXTRA_KEY is byte-for-byte the key the report prints; the raw `.env` value this used to paste made
  # `fooclips/…` out of the perfectly legal `S3_KEY_PREFIX=foo`. `admin objects put` takes the key from
  # the bucket's root, so "$SM_PREFIX<key>" is the stack's own key space in whichever store the app uses
  # — R2 on the reference stack; the bundled MinIO in every version of this step before erratum 281 (f),
  # so the plants landed where nothing would ever read them (errata 281 (f), 346).
  # THE STRANGER, planted FIRST (Gate 10, harness): a row-less prefix under a clip id nothing
  # on this stack owns, beside C's key space. A bucket-wide `--delete --min-age 5s` would take it the
  # moment it aged; a delete scoped to C must not list it at all, and its survival below is the proof.
  # It is planted here rather than beside the delete loop because an object store has no `touch`: the
  # only way to know it is OLD enough to have been deleted is to make it older than the two plants the
  # report loop already waits for, and then ask. A plant that was seconds old when the delete ran would
  # have survived because of its AGE, and the assertion would have proved nothing about the scope —
  # which is the class of mistake 88-fsstore's 2020 mtime exists to avoid on the filesystem side.
  # Its name is FIXED so that a step that died between the plant and the removal below leaves at most
  # one such object, which the next run's plant overwrites and removes.
  ST_ID="e2eStrangerA"; ST_REL="clips/$ST_ID/original/stranger.bin"
  printf 'not one of this step'"'"'s\n' | store_obj put "$SM_PREFIX$ST_REL" >/dev/null || die "could not plant the stranger"
  EXTRA_REL="clips/$AD_C/renditions/main-e2eextra.mp4"
  EXTRA_ORIG_REL="clips/$AD_C/original/e2e-extra.bin"
  EXTRA_KEY="${SM_PREFIX}$EXTRA_REL"            # what `admin orphans` prints: the WHOLE key
  EXTRA_ORIG="${SM_PREFIX}$EXTRA_ORIG_REL"
  printf 'superseded by a trim that died\n' | store_obj put "$SM_PREFIX$EXTRA_REL" >/dev/null \
    || die "could not plant the extra object"
  printf 'not a real original\n' | store_obj put "$SM_PREFIX$EXTRA_ORIG_REL" >/dev/null \
    || die "could not plant the extra original"
  C_BEFORE=$(prefix_count "$AD_C")
  echo "[e2e]   planted 2 extra objects under clips/$AD_C/ (prefix now $C_BEFORE objects)"
  # The plants must AGE past --min-age before the report can see them, and the comparison is
  # cross-container: `orphans` takes `now` from the APP's clock while the objects' LastModified comes
  # from MinIO's listing, and the app container's clock runs ~13 s behind the host and steps BACKWARDS
  # under load (the Docker VM's clock steps). A fixed `sleep 6` against `--min-age 5s` left one second of margin and
  # lost it, reporting `age=-13s (skipped: younger than --min-age 5s)` (nit sweep S02). So: poll the
  # REPORT, which mutates nothing, until it names both plants — the threshold is not loosened and the
  # sleep is not lengthened, the step simply waits for the fact it is about to assert.
  #
  # A MARGIN does not work here, and §20 erratum 245 replaced the one that was: the poll used to ask
  # `--min-age 25s` and the assertions then read a SECOND report at `--min-age 5s`, on the theory that
  # twenty seconds of slack is more than the clock can lose. It is not. On a loaded box the poll broke on
  # its very first call (the app's clock was ~25 s AHEAD of MinIO's) and the report taken seconds later
  # put the same two objects at `age=2s` and `age=4s` — a backwards step of about twenty seconds between
  # two `docker compose exec` calls, which is exactly what §8 warns the app container's clock does.
  #
  # So there is ONE report and one threshold: the loop polls `--min-age $ORPH_MIN` until the plants are
  # COUNTED, and the assertions below read THAT file rather than taking another. A clock that steps
  # backwards now costs another turn of the poll instead of failing the step, and nothing is loosened —
  # the threshold is the one the assertions are about.
  #
  # BOTH loops pass `--clip "$AD_C"` (§20 erratum 290, AUDIT #4): the report and the delete are scoped to
  # clip C's own prefix, so this step never lists — and `--delete` never removes — anything under any
  # other clip on the stack. Until this erratum the delete loop was a BUCKET-WIDE `orphans --delete`,
  # run unattended on the reference stack (production R2) every gate: the project's rule says a delete
  # on that stack prints its list and waits for a yes, and this was the one standing violation. The
  # stranger planted before the delete loop is what proves the scope.
  ORPH="$E2E_DIR/admin-orphans.txt"
  ORPH_MIN=5s
  ORPH_N=0
  while [ "$ORPH_N" -lt 90 ]; do
    dc exec -T app clippet admin orphans --clip "$AD_C" --min-age "$ORPH_MIN" > "$ORPH" 2>&1 || die "admin orphans failed: $(cat "$ORPH")"
    # The SUBTOTAL, not the two key lines: a plant that is still too young is
    # REPORTED with `(skipped: younger than --min-age …)` and counted in nothing,
    # so keying on the key lines would break out of this loop one second early —
    # which is the very failure the poll exists to prevent.
    if grep -q "extra   $AD_C subtotal objects=2" "$ORPH"; then break; fi
    ORPH_N=$((ORPH_N + 1)); sleep 1
  done
  [ "$ORPH_N" -lt 90 ] || die "the planted extras were never COUNTED as extras at --min-age $ORPH_MIN after ${ORPH_N}s (app clock skew?):
$(cat "$ORPH")"
  echo "[e2e]   both plants counted at --min-age $ORPH_MIN after ${ORPH_N}s of polling"
  { grep -E "^extra   $AD_C " "$ORPH" || true; } | sed -n 's/^/[e2e]   /p'
  has_file "$ORPH" "extra   $AD_C state=ready key=$EXTRA_KEY" "the superseded rendition is not reported"
  has_file "$ORPH" "extra   $AD_C state=ready key=$EXTRA_ORIG" "the extra original is not reported"
  has_file "$ORPH" "extra   $AD_C subtotal objects=2" "the per-clip subtotal"
  # The SUMMARY counters are bucket-WIDE, so they are asserted as a floor and not as an equality (§20
  # erratum 245). `extra=2` was an equality, and it made this step fail on somebody ELSE'S leftovers: an
  # aborted run leaves its clip and its two plants behind, `grep e2e_` over `admin ls` matches nothing
  # (there is no owner column in that listing), and the next run died here with a count that was correct.
  # What must be exact is THIS clip's subtotal, which is the line above; what the summary has to show is
  # that it counted our two and really measured bytes.
  ORPH_SUM=$(sed -n 's/.*\(extra=[0-9]* extra_bytes=[0-9]* extra_deleted=[0-9]*\).*/\1/p' "$ORPH" | tail -1)
  expect_nonempty "$ORPH_SUM" "the orphans summary counters line"
  ORPH_EXTRA=${ORPH_SUM#extra=}; ORPH_EXTRA=${ORPH_EXTRA%% *}
  ORPH_BYTES=${ORPH_SUM#*extra_bytes=}; ORPH_BYTES=${ORPH_BYTES%% *}
  { [ "$ORPH_EXTRA" -ge 2 ] && [ "$ORPH_BYTES" -gt 0 ]; } 2>/dev/null \
    || die "the summary counters do not include this step's two plants: $ORPH_SUM"
  [ "$ORPH_EXTRA" = 2 ] || echo "[e2e]   NOTE: the bucket holds $ORPH_EXTRA counted extras, of which 2 are this step's — an earlier run of this suite was aborted (find it with 'clippet admin ls --owner <user>')"
  lacks_file "$ORPH" "meta/probe.json" "meta/probe.json is the one legitimate unreferenced key and must never be reported"
  # Reporting removes nothing.
  expect "$(prefix_count "$AD_C")" "$C_BEFORE" "a report without --delete must remove nothing"

  # The delete is POLLED for the same reason the report above is: the app's clock can step backwards
  # between two commands and put a plant back under the threshold, in which case this run reports it as
  # skipped and removes nothing. A delete of a key that is already gone is a no-op, so asking again costs
  # nothing and the loop ends the moment OUR rendition is gone (§20 erratum 245).
  # It waits for BOTH plants, not just the rendition: the original is planted SECOND and is therefore the
  # YOUNGER of the two, so one pass can perfectly well delete the rendition and skip the original — and
  # the interlock assertion reads the LAST report. The deletion is remembered across turns because a key
  # that has been deleted once is simply absent from the next report.
  # The stranger is now OLD ENOUGH TO HAVE BEEN DELETED, and that is asserted before the delete runs
  # (Gate 10, harness). The report is scoped to the stranger's OWN prefix, so it lists nothing
  # else on the stack and removes nothing at all: `orphan  <prefix>` is the line a row-less prefix past
  # --min-age gets, and `recent  <prefix> … (skipped: younger than …)` is the line it gets while it is
  # still too young. Without this the survival assertion below would pass on a stack where the delete
  # simply never looked at an object that age.
  ST_PRE="${SM_PREFIX}clips/$ST_ID/"
  ORPH_N=0
  while [ "$ORPH_N" -lt 60 ]; do
    dc exec -T app clippet admin orphans --clip "$ST_ID" --min-age "$ORPH_MIN" > "$ORPH" 2>&1 \
      || die "admin orphans --clip $ST_ID failed: $(cat "$ORPH")"
    if grep -q "^orphan  $ST_PRE " "$ORPH"; then break; fi
    ORPH_N=$((ORPH_N + 1)); sleep 1
  done
  [ "$ORPH_N" -lt 60 ] || die "the stranger was never old enough to be deletable at --min-age $ORPH_MIN, so its survival below would prove nothing:
$(cat "$ORPH")"
  { grep -E "^orphan  $ST_PRE|^scope " "$ORPH" || true; } | sed -n 's/^/[e2e]   /p'
  echo "[e2e]   the stranger is past --min-age $ORPH_MIN: a bucket-wide --delete would take it now"
  ORPH_N=0; ORPH_KEY_DELETED=0
  while [ "$ORPH_N" -lt 60 ]; do
    dc exec -T app clippet admin orphans --clip "$AD_C" --delete --min-age "$ORPH_MIN" > "$ORPH" 2>&1 || die "admin orphans --delete failed: $(cat "$ORPH")"
    if grep -A1 -F "key=$EXTRA_KEY " "$ORPH" | grep -q '^ *deleted$'; then ORPH_KEY_DELETED=1; fi
    if [ "$ORPH_KEY_DELETED" = 1 ] && grep -A1 -F "key=$EXTRA_ORIG " "$ORPH" | grep -q 'refusing to delete an original'; then
      break
    fi
    ORPH_N=$((ORPH_N + 1)); sleep 2
  done
  [ "$ORPH_N" -lt 60 ] || die "the planted extras were still too young for --delete --min-age $ORPH_MIN after ${ORPH_N} attempts (app clock skew?):
$(cat "$ORPH")"
  # The stranger's count is read BEFORE it is removed, and it is removed before any assertion below can
  # die, so the plant never outlives the step on a stack that is not this suite's to litter.
  ST_N=$(prefix_count "$ST_ID")
  store_obj rm "$SM_PREFIX$ST_REL" >/dev/null || die "could not remove the stranger"
  expect "$ST_N" 1 "an object outside clips/$AD_C/ survives a --clip $AD_C --delete (the delete is scoped to one clip's prefix)"
  lacks_file "$ORPH" "$ST_ID" "a --clip $AD_C report must not even list another clip's prefix"
  has_file "$ORPH" "scope   ${SM_PREFIX}clips/$AD_C/" "the report names the one prefix it was scoped to"
  # The indented VERDICT lines are in the filter too — `deleted` as well as `refusing to delete` — because
  # since §20 erratum 245 the pair (a key line, then what was done with it) IS the headline assertion, and
  # a transcript that printed only the key lines left the reader unable to see the half that now decides.
  { grep -E "^extra   $AD_C |^ +deleted$|refusing to delete" "$ORPH" || true; } | sed -n 's/^/[e2e]   /p'
  has_file "$ORPH" 'refusing to delete an original; left alone' "the upload-is-sacred interlock (§11)"
  # WHICH object went, not how many went anywhere. `extra_deleted=1` was another bucket-wide equality
  # somebody else's leftovers could break, and it never said that the object deleted was ours: the report
  # prints the key line and then, indented, what it did with it, so the pair is read together — and that
  # pair is what the loop above waited for. This is strictly stronger than the count it replaces
  # (§20 erratum 245).
  expect "$ORPH_KEY_DELETED" 1 "the superseded rendition (and not something else) is what --delete removed"
  grep -A1 -F "key=$EXTRA_ORIG " "$ORPH" | grep -q 'refusing to delete an original' \
    || die "the extra ORIGINAL was not the one left alone: $(grep -A1 -F "key=$EXTRA_ORIG " "$ORPH" || echo 'no line for it at all')"
  # …and the summary still carries the counter, which is all it can honestly say here: the deletion may
  # have happened on an earlier turn of the loop, in which case THIS report deleted nothing and says so.
  ORPH_DEL=$(sed -n 's/.*extra_deleted=\([0-9]*\).*/\1/p' "$ORPH" | tail -1)
  expect_nonempty "$ORPH_DEL" "the summary counters still report extra_deleted"
  expect "$(prefix_count "$AD_C")" "$((C_BEFORE - 1))" "the replaceable extra is gone and the original is not"
  # The keys the row DOES reference are untouched: AD_C still plays.
  code=$(get "$BASE/api/clips/$AD_C" -b "$JARW"); expect "$code" 200 "AD_C after the extras sweep"
  expect "$(json state)" ready "AD_C is still ready"
  # `media_head` (§20 erratum 280, rule (a)): "C still plays" is a question about the object, and the
  # route answers it in either delivery shape.
  code=$(media_head "$BASE/api/clips/$AD_C/video.mp4"); expect_media "$code" 200 "AD_C still serves its rendition"
  code=$(media_head "$BASE/api/clips/$AD_C/poster.jpg"); expect_media "$code" 200 "AD_C still serves its poster"
  store_obj rm "$SM_PREFIX$EXTRA_ORIG_REL" >/dev/null || die "could not remove the planted extra original"
  expect_prefix_count "$AD_C" "$((C_BEFORE - 2))" "AD_C's prefix is back to what it was"
else
  echo "[e2e]   NOTE: this stack keeps its media in a directory (STORAGE_BACKEND=fs), so there is no bucket to plant an extra object in: the orphans report's third case is not exercised on this stack"
fi

# --- backup <dir> --rotate N ----------------------------------------------------------------------
BKDIR="/tmp/e2e-backups-$(date +%s)"
dc exec -T app sh -c "mkdir -p $BKDIR && printf 'not mine' > $BKDIR/keep-me.db && printf 'partial' > $BKDIR/clippet-20260101T000000Z.db.part-stale && touch -t 202601010000 $BKDIR/clippet-20260101T000000Z.db.part-stale && printf 'partial' > $BKDIR/clippet-20260102T000000Z.db.part-fresh" \
  || die "could not prepare $BKDIR"
# The minted name has second resolution and the CLI never overwrites, so consecutive runs must land in
# different seconds OF THE CONTAINER'S CLOCK — a host-side `sleep 1` proved not enough (2026-09-10: the VM
# clock stepped back inside the sleep and backup 3 minted backup 2's name; the seconds-scale clock trap).
#
# Waiting on a `date +%s` read is not enough EITHER, and that is the second lesson (2026-09-11): the
# snapshot is stamped somewhere INSIDE the command, so a second read taken before it — or after it —
# is not the second the file was named for, and a run whose stamp landed a second or two later than
# the reading satisfies the wait immediately and then collides. So the wait is on the NAME: each run
# reports the file it minted, and the next one does not start until the container's own clock would
# format a DIFFERENT name. That is the invariant the CLI enforces, stated exactly, and it holds
# whichever way the clock steps.
#
# And the third lesson (2026-09-16, the thirteenth gate): the wait above proves the clock HAS moved on — it
# cannot prove it will not step BACK between the `date` that satisfied it and the stamp the command takes a
# moment later (the VM clock steps). When it does, the CLI refuses with "a snapshot was already
# taken this second" and overwrites nothing; its own advice is "run it again in a second", and that is what
# the harness does, ONCE per run (a bounded retry of a refusal that had no side effect, the http_refused
# shape): any other failure dies as before, and a second refusal dies too.
for n in 1 2 3; do
  BK="$E2E_DIR/admin-backup-$n.txt"
  if ! dc exec -T app clippet admin backup "$BKDIR" --rotate 2 > "$BK" 2>&1; then
    grep -q 'a snapshot was already taken this second' "$BK" || die "admin backup $n failed: $(cat "$BK")"
    echo "[e2e]   NOTE: admin backup $n refused (the clock stepped back onto the previous snapshot's second); retrying once in a second"
    sleep 1.2
    dc exec -T app clippet admin backup "$BKDIR" --rotate 2 > "$BK" 2>&1 || die "admin backup $n failed again: $(cat "$BK")"
  fi
  if [ "$n" != 3 ]; then
    AD_BKNAME=$(sed -n 's|^backup:  .*/\(clippet-[0-9TZ]*\.db\) .*|\1|p' "$BK")
    [ -n "$AD_BKNAME" ] || die "admin backup $n did not name the snapshot it minted: $(cat "$BK")"
    i=0
    while [ "clippet-$(dc exec -T app date -u +%Y%m%dT%H%M%SZ | tr -d '\r').db" = "$AD_BKNAME" ] && [ $i -lt 30 ]; do
      sleep 1; i=$((i+1))
    done
  fi
done
sed -n 's/^/[e2e]   /p' "$E2E_DIR/admin-backup-3.txt"
has_file "$E2E_DIR/admin-backup-1.txt" 'a stranded partial snapshot' "the stale .part- file was swept on the first run"
has_file "$E2E_DIR/admin-backup-3.txt" 'rotate:  keep   ' "rotation names what it kept"
has_file "$E2E_DIR/admin-backup-3.txt" 'rotate:  remove ' "rotation names what it removed"
has_file "$E2E_DIR/admin-backup-3.txt" 'untouched=1' "rotation reports the files it may not consider"
BKLS=$(dc exec -T app sh -c "ls -1 $BKDIR" | tr -d '\r') || die "could not list $BKDIR"
printf '%s\n' "$BKLS" | sed -n 's/^/[e2e]   backups: /p'
expect "$(printf '%s\n' "$BKLS" | grep -c '^clippet-.*\.db$' || true)" 2 "--rotate 2 keeps exactly two snapshots"
contains "$BKLS" 'keep-me.db' || die "rotation removed a file it did not mint: $BKLS"
contains "$BKLS" 'part-fresh' || die "a partial younger than an hour may still be being written: $BKLS"
if contains "$BKLS" 'part-stale'; then die "the stranded partial older than an hour survived: $BKLS"; fi
# The snapshot holds every password and token hash, so it is 0600 like the single-file form.
BKMODE=$(dc exec -T app sh -c "ls -l $BKDIR/clippet-*.db | head -1 | cut -c1-10") || die "could not stat the snapshot"
expect "$(printf '%s' "$BKMODE" | tr -d '\r')" '-rw-------' "the dated snapshot's mode"
# The newest one really is a database: restore-check needs no configuration at all.
BKNEW=$(printf '%s\n' "$BKLS" | { grep '^clippet-.*\.db$' || true; } | sort | tail -1)
OUT=$(dc exec -T app clippet admin restore-check "$BKDIR/$BKNEW") || die "restore-check on a rotated snapshot: $OUT"
contains "$OUT" 'integrity: ok' || die "the rotated snapshot is not sound: $OUT"
dc exec -T app sh -c "rm -rf $BKDIR" || echo "[e2e]   NOTE: could not remove $BKDIR"
echo "[e2e]   orphans found and cleared an extra under a live clip (never the original); backup --rotate 2 kept 2 and touched nothing it did not name"
code=$(http DELETE "$BASE/api/clips/$AD_C" -b "$JARW"); expect_deleted "$code" "$AD_C" "DELETE AD_C"
summary_row AD "${AD_C:-}" "the orphans step's own subject: two extras planted under it, one swept, the original refused; deleted"
