# 73-ops.sh — the app repairing its own queue (ROADMAP #12's automatic half, §20 errata 230-231), the
# sweeper remembering a failing delete across a restart (§20 erratum 232) and the poster taking its
# frame from a ranged prefix of the rendition (§20 erratum 233).
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Steps 73-76. Everything it creates it destroys; nothing it does outlives the run.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=O1_ buckets=-
# e2e-covers: internal/worker/repair.go internal/worker/runner.go internal/worker/registry.go
# e2e-covers: internal/worker/worker.go internal/sweeper/** internal/lifecycle/** internal/db/sweepfail.go
# e2e-covers: internal/db/jobs.go internal/store/seeker.go internal/transcode/poster.go
# e2e-covers: internal/transcode/hwfallback.go internal/httpapi/health.go cmd/clippet/doctor.go
#   Steps 73-75 happen entirely inside throwaway containers with their own DATA_DIR, their own
#   SCRATCH_DIR and their own S3_KEY_PREFIX, so nothing they do is visible to the live database, the
#   live scratch or any other bundle's objects; step 76 uses a clip of its own and greps the app log for
#   that clip's id alone. It is one of the longest bundles in the suite (two probe containers, several
#   real encodes), which the pool's longest-first plan reads off the last run's durations — it used to
#   have lane 1 to itself so that it started first.
#
# Variables are prefixed O1_ (bundle O1) so nothing here can clobber a harness global.

# ---------------------------------------------------------------------------------------------------
step 73 "automatic unstick: a job killed mid-encode is repaired with NO restart, and a live encode is left alone"
# The two halves of the promise need two app processes on ONE database, which is what makes this a
# throwaway instance rather than the lane's own stack:
#
#   R  the repairer:  JOBS_UNSTICK_AFTER=25s, port 8080
#   V  the victim:    JOBS_UNSTICK_AFTER=0,   port 8081, killed -9 mid-encode
#
# Both boot BEFORE any job is claimed, on purpose: `ReconcileOnStartup` repairs every `running` row it
# finds, so a process that booted later would repair the other's live job and prove nothing.
# TRANSCODE_CONCURRENCY is 1 in each, so with two clips each process ends up holding exactly one — and
# which process claimed which clip is read off the logs rather than assumed, because both are nudged by
# their own upload and the claim is a race the step does not need to win.
#
# The numbers are what make the SECOND half provable. JOBS_UNSTICK_AFTER=30s is the shortest age the
# app accepts (config.MinUnstickAfter); TRANSCODE_HARD_TIMEOUT stays long (5m) and the encode must
# still be RUNNING when the repair of the other process's job lands, because "the repairer left its own
# live job alone" is a claim about a job that was alive at that moment. With the age at or above the
# hard timeout, as the shipped default is, nothing could ever be both alive and past the age and this
# assertion would be vacuous.
#
# The preset is `placebo` and not `veryslow` (§20 erratum 245). `veryslow` over 30 s of 720p60 was
# described here as "minutes of work" and is not: it finished in 29 s on this host, and the loop that
# watches for the repair — one `clippet admin jobs` process per turn — takes about half a second a turn
# when the box is busy, so with the suite running its bundles in lanes the repair was observed 57 s in,
# three seconds AFTER R's own encode had finished. The step then died reading an empty `running_for`,
# correctly: it could not prove the half it exists for. `placebo` is several times slower per frame,
# costs nothing to prepare (the source is unchanged) and costs nothing to run either, because this
# encode is killed a few lines below and never completes in any case.
#
# The margin is no longer the only thing standing between this step and that failure. The second half is
# proven from R's OWN LOG ORDER — its claim line, the repair line, and the line that ends its own run if
# it has one — which is a single append-only stream from a single process and therefore consults no
# clock at all. `placebo` still buys the margin that makes the later `running_for` read meaningful; when
# the margin is lost the step now says so in a NOTE instead of dying on an empty read.
#
# `TRANSCODE_HWACCEL=` (empty) pins the CPU argv whatever the stack runs (§20 erratum 247). On the GPU
# flavour the probe inherits `cuda` and the card from the service, NVENC has no `placebo`, and R's
# whole encode took 7 s — the repair landed 25 s after it had finished and this step died on the
# same empty `running_for`. The slow preset is the whole mechanism here, and it is an x264 preset.
O1_PREFIX="e2e-o1-$(date +%s)/"
O1_OUT="$E2E_DIR/o1-unstick.txt"
trap 'probe_prefix_tidy_ "$O1_PREFIX" "the unstick probe"' EXIT   # a die below still leaves the bucket root clean
dc run --rm -T --no-deps \
  -e DATA_DIR=/tmp/o1 -e SCRATCH_DIR=/tmp/o1-scratch -e S3_KEY_PREFIX="$O1_PREFIX" \
  -e REGISTRATION=open -e CAPTURE_READY_WAIT=0 -e SWEEP_INTERVAL=3s -e SWEEP_GRACE=1s \
  -e TRANSCODE_HARD_TIMEOUT=5m -e JOBS_UNSTICK_AFTER=30s \
  -e TRANSCODE_PRESET=placebo -e TRANSCODE_CRF=18 -e TRANSCODE_HWACCEL= \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 \
  --entrypoint sh app -c '
    set -e
    mkdir -p /tmp/o1 /tmp/o1-scratch /tmp/bin
    wait_up() { i=0; while [ $i -lt 90 ]; do curl -fsS "http://127.0.0.1:$1/healthz" >/dev/null 2>&1 && return 0; i=$((i+1)); sleep 1; done; echo "the probe app on port $1 never became healthy"; exit 1; }
    clip_state() { clippet admin ls "$1" | sed -n "s/^state: *//p" | awk "{print \$1}"; }
    job_line() { clippet admin jobs --clip "$1" --kind transcode --limit 1 | head -1; }
    running_job() { clippet admin jobs --clip "$1" --kind transcode --state running --limit 1 | sed -n "s/^#\([0-9]*\) .*/\1/p" | head -1; }
    run_for() { clippet admin jobs --clip "$1" --kind transcode --state running --limit 1 | sed -n "s/.*running_for=\([^ ]*\).*/\1/p" | head -1; }
    started_clip() { sed -n "s/.*transcode started\" clip=\([A-Za-z0-9_-]*\) .*/\1/p" "$1" | head -1; }
    upload() { curl -sS -b /tmp/jar -F "metadata={\"title\":\"$2\"};type=application/json" \
        -F duplicate=allow -F "video=@/tmp/in.mp4;type=video/mp4" "http://127.0.0.1:$1/api/clips" \
        | grep -o "\"id\":\"[A-Za-z0-9_-]\{12\}\"" | head -1 | sed -n "s/.*:\"\(.*\)\"/\1/p"; }
    clippet migrate >/dev/null

    # --- both processes up, queue empty ---------------------------------------------------------
    clippet serve >/tmp/r.log 2>&1 &
    RP=$!
    wait_up 8080
    JOBS_UNSTICK_AFTER=0 CLIPPET_LISTEN=:8081 clippet serve >/tmp/v.log 2>&1 &
    VP=$!
    wait_up 8081
    echo "rboot=$(grep -c "automatic stuck-job repair is on" /tmp/r.log || true)"
    echo "rbootline=$(grep "automatic stuck-job repair is on" /tmp/r.log | head -1 | tr -d "\r")"
    echo "vboot=$(grep -c "automatic stuck-job repair is off" /tmp/v.log || true)"
    echo "rwarn=$(grep "shorter than TRANSCODE_HARD_TIMEOUT" /tmp/r.log | head -1 | tr -d "\r")"
    echo "vwarn=$(grep -c "shorter than TRANSCODE_HARD_TIMEOUT" /tmp/v.log || true)"
    curl -sS -o /dev/null -c /tmp/jar -X POST -H "Content-Type: application/json" \
      -d "{\"username\":\"o1user\",\"password\":\"o1-pass-12345\"}" http://127.0.0.1:8080/api/auth/register
    /usr/local/bin/ffmpeg -hide_banner -loglevel error -f lavfi -i testsrc2=size=1280x720:rate=60:duration=30 \
      -c:v libx264 -preset ultrafast -pix_fmt yuv420p -movflags +faststart -y /tmp/in.mp4

    # --- one clip through each process; each claims the one its own Wake nudged it for ----------
    A1=$(upload 8080 "o1 first")
    i=0; while [ $i -lt 60 ]; do [ -n "$(running_job "$A1")" ] && break; i=$((i+1)); sleep 1; done
    B1=$(upload 8081 "o1 second")
    i=0; while [ $i -lt 60 ]; do [ -n "$(running_job "$B1")" ] && break; i=$((i+1)); sleep 1; done
    RC=$(started_clip /tmp/r.log)
    VC=$(started_clip /tmp/v.log)
    echo "a1=$A1"
    echo "b1=$B1"
    echo "rclip=$RC"
    echo "vclip=$VC"
    VJOB=$(running_job "$VC")
    echo "vjob=$VJOB"
    echo "vjobbefore=$(job_line "$VC")"

    # --- kill V mid-encode: its row is left `running` with no worker ----------------------------
    kill -9 $VP 2>/dev/null || true
    sleep 1
    echo "vstuckstate=$(clip_state "$VC")"
    echo "vstuckjob=$(job_line "$VC")"
    # R repairs it on its own timer. 30s of age plus a tick of a quarter of that, and no restart of
    # anything: the wait is generous because the container clock steps backwards under load.
    i=0; while [ $i -lt 90 ]; do
      case "$(job_line "$VC")" in *state=queued*|*state=failed*) break;; esac
      i=$((i+1)); sleep 1
    done
    echo "repairsecs=$i"
    echo "vjobafter=$(job_line "$VC")"
    echo "vstateafter=$(clip_state "$VC")"
    echo "repairline=$(grep "repaired a job that was running with no worker" /tmp/r.log | head -1 | tr -d "\r")"
    # …and R has NOT touched the encode it is running itself. That claim needs R own job to be BOTH
    # past the repair age and still alive at the moment it is read, and the loop above cannot promise
    # either: it exits when the OTHER process job is repaired, which on an idle box happens while R own
    # job is 28s old (under the 30s age, so the assertion would prove nothing) and on a busy box happens
    # after R own encode has finished (so there is nothing to read at all). Both were seen, in that
    # order, on the same machine. So: poll R own age past the floor, then give the repair loop a whole
    # further tick (every=7.5s) to touch a job it must not touch, and read the answer after that.
    i=0
    while [ $i -lt 120 ]; do
      RF=$(run_for "$RC")
      if [ -z "$RF" ]; then break; fi
      case "$RF" in
        *m*|*h*) break ;;
        *s) if [ "${RF%s}" -ge 35 ]; then break; fi ;;
      esac
      i=$((i+1)); sleep 1
    done
    echo "rwaited=$i"
    sleep 9
    echo "repairsofrclip=$(grep -c "repaired a job that was running with no worker\".*clip=$RC" /tmp/r.log || true)"
    echo "rrunfor=$(run_for "$RC")"
    echo "rjobafter=$(job_line "$RC")"
    echo "rstateafter=$(clip_state "$RC")"
    # R OWN LOG, BY LINE NUMBER. R is one process writing one append-only stream, so the order of these
    # three lines IS the order the events happened in -- no clock is consulted, which matters because
    # this container clock steps backwards under load. They answer the question the running_for read
    # above can only answer if the timing goes the right way: was R own encode still alive at the moment
    # R wrote the repair of the other process job?
    #   rclaimno  R claim of its OWN clip
    #   repairno  the repair of the OTHER process job
    #   rendno    the line that ENDS R own run, if it has one yet (it is killed below, so usually none)
    echo "rclaimno=$(grep -n "transcode started\".*clip=$RC" /tmp/r.log | head -1 | cut -d: -f1)"
    echo "repairno=$(grep -n "repaired a job that was running with no worker\"" /tmp/r.log | head -1 | cut -d: -f1)"
    echo "rendno=$(grep -nE "msg=\"(clip ready|clip failed|job released)\".*clip=$RC" /tmp/r.log | head -1 | cut -d: -f1)"

    # --- what the two operator surfaces say (step 74) -------------------------------------------
    echo "rhealth=$(curl -sS http://127.0.0.1:8080/healthz)"
    clippet admin doctor >/tmp/doctor.txt 2>&1 || true
    echo "doctorrepair=$(grep "^repair" /tmp/doctor.txt | head -1 | tr -d "\r")"
    echo "doctorwarn=$(grep "^repair   warn" /tmp/doctor.txt | head -1 | tr -d "\r")"
    echo "doctorsummary=$(grep "^doctor:" /tmp/doctor.txt | head -1 | tr -d "\r")"
    echo "doctorsecret=$(grep -c "o1-pass-12345" /tmp/doctor.txt || true)"

    # --- cleanup: both clips destroyed by this instance own sweeper, then the prefix proven empty
    kill -9 $RP 2>/dev/null || true
    sleep 1
    printf "%s\n" "#!/bin/sh" "case \"\$1\" in -version) exec /usr/local/bin/ffmpeg -version ;; esac" \
      "echo \"e2e: forced encode failure\" >&2" "exit 1" > /tmp/bin/ffmpeg
    chmod +x /tmp/bin/ffmpeg
    FFMPEG_PATH=/tmp/bin/ffmpeg JOBS_UNSTICK_AFTER=0 clippet serve >/tmp/c.log 2>&1 &
    CP=$!
    wait_up 8080
    clippet admin expire "$A1" --ago 2h >/dev/null
    clippet admin expire "$B1" --ago 2h >/dev/null
    i=0; while [ $i -lt 60 ]; do
      N=$(clippet admin stats --no-storage | sed -n "s/^clips: *\([0-9]*\).*/\1/p")
      [ "$N" = 0 ] && break
      i=$((i+1)); sleep 2
    done
    echo "clipsleft=$N"
    echo "bucketleft=$(clippet admin stats | sed -n "s/^bucket:  //p")"
    kill -9 $CP 2>/dev/null || true
  ' > "$O1_OUT" 2>&1 || die "the automatic-unstick probe container failed:
$(cat "$O1_OUT")"
sed -n 's/^/[e2e]   o1-probe: /p' "$O1_OUT"
o1() { sed -n "s/^$1=//p" "$O1_OUT" | tail -1 | tr -d '\r'; }

# The premise: two clips, one claimed by each process. Without that this step tests nothing, so it is
# asserted rather than assumed.
expect_nonempty "$(o1 a1)" "the first probe clip id"
expect_nonempty "$(o1 b1)" "the second probe clip id"
expect_nonempty "$(o1 rclip)" "the clip the REPAIRER claimed (its log names none)"
expect_nonempty "$(o1 vclip)" "the clip the VICTIM claimed (its log names none)"
[ "$(o1 rclip)" != "$(o1 vclip)" ] || die "both processes claimed the same clip $(o1 rclip); the step cannot tell the two halves apart"
expect_nonempty "$(o1 vjob)" "the job id of the killed run (the encode finished before it could be killed?)"
expect "$(o1 vstuckstate)" processing "a killed worker leaves its clip processing (this is the stuck state)"
contains "$(o1 vstuckjob)" 'state=running' || die "the killed run left no running job: $(o1 vstuckjob)"

# The repair itself: on a timer, in the app, with `unstick`'s own accounting and wording.
contains "$(o1 vjobafter)" 'state=queued' || die "the app never repaired the abandoned job in $(o1 repairsecs)s: $(o1 vjobafter)"
contains "$(o1 vjobafter)" 'attempts=1' || die "a crash-class repair spends exactly one attempt: $(o1 vjobafter)"
contains "$(o1 vjobafter)" 'interrupted: the job was found running with no worker' \
  || die "the repair did not write the operator's own sentence: $(o1 vjobafter)"
expect "$(o1 vstateafter)" queued "the repaired clip is claimable again (§11: never stuck)"
contains "$(o1 repairline)" 'action=requeued' || die "the repair log line: $(o1 repairline)"
contains "$(o1 repairline)" "clip=$(o1 vclip)" || die "the repair log line names another clip: $(o1 repairline)"
echo "[e2e]   the abandoned job was repaired $(o1 repairsecs)s after the kill, with no restart"

# And the other half, which is the whole reason this lives in the worker: the encode the repairer is
# RUNNING was not touched.
#
# THE PROOF IS R OWN LOG ORDER, not a clock. R is one process writing one stream, so `rclaimno <
# repairno < (rendno or nothing)` says, exactly and with no timing in it, that R own encode was claimed
# before the repair and had not ended when the repair was written — which is the whole of "a live encode
# is left alone". The `running_for` read below is a SECOND read, made seconds later, and it is what made
# this step a race between two clocks: on an idle box it exits with R own job 28 s old (under the age,
# so the assertion proved nothing) and on a busy box after R encode has already finished (nothing left
# to read at all, and the step died as if the feature were broken). Both were seen on this machine.
expect_nonempty "$(o1 rclaimno)" "the repairer's own claim line (its log does not name the clip it started)"
expect_nonempty "$(o1 repairno)" "the repair line in the repairer's log"
O1_CLAIMNO=$(o1 rclaimno); O1_REPAIRNO=$(o1 repairno); O1_ENDNO=$(o1 rendno)
[ "$O1_CLAIMNO" -lt "$O1_REPAIRNO" ] 2>/dev/null \
  || die "the repairer wrote the repair (log line $O1_REPAIRNO) BEFORE claiming its own encode (line $O1_CLAIMNO): it had no live job of its own to leave alone"
expect "$(o1 repairsofrclip)" 0 "the log claims a repair of the clip the repairer is encoding"
if [ -z "$O1_ENDNO" ]; then
  # The normal ending: R own encode never finished at all (it is `placebo` over 30 s of 720p60 and R is
  # killed a few lines later), so the SECOND read is meaningful too and is asserted exactly as before.
  O1_RF=$(o1 rrunfor)
  case ${O1_RF:-unknown} in
    *m*|*h*) : ;;
    *s) [ "${O1_RF%s}" -ge 30 ] 2>/dev/null || die "the repairer's own encode had only been running $O1_RF when the abandoned job was repaired: it was never past the 30s age, so this half proves nothing" ;;
    *) die "could not read the repairer's own running_for (got '${O1_RF:-}'), and its log carries no line ending that run either" ;;
  esac
  contains "$(o1 rjobafter)" 'state=running' || die "the repairer repaired its OWN live encode: $(o1 rjobafter)"
  contains "$(o1 rjobafter)" 'attempts=0' || die "the repairer spent an attempt on its own live encode: $(o1 rjobafter)"
  expect "$(o1 rstateafter)" processing "the live encode's clip must still be processing"
  echo "[e2e]   the repairer's own encode (running_for=$O1_RF, past the 30s age, claim line $O1_CLAIMNO < repair line $O1_REPAIRNO, never ended) was left alone"
else
  [ "$O1_ENDNO" -gt "$O1_REPAIRNO" ] 2>/dev/null \
    || die "the repairer's own encode had already ended (log line $O1_ENDNO) when it wrote the repair (line $O1_REPAIRNO): this half proves nothing, and the preset or the fixture needs to be slower"
  echo "[e2e]   NOTE: the repairer's own encode finished during the extra tick (log line $O1_ENDNO, after the repair at $O1_REPAIRNO) — the log order is the proof here; the later reads are running_for=$(o1 rrunfor) job=$(o1 rjobafter) state=$(o1 rstateafter)"
  echo "[e2e]   the repairer's own encode (claim line $O1_CLAIMNO < repair line $O1_REPAIRNO < its own ending at $O1_ENDNO) was left alone"
fi

# It cleaned up after itself: no rows, and nothing left under its own key prefix.
expect "$(o1 clipsleft)" 0 "the probe instance destroyed both of its clips"
contains "$(o1 bucketleft)" 'objects=0' || die "the probe left objects under $O1_PREFIX: $(o1 bucketleft)"
probe_prefix_swept_ "$O1_PREFIX" "the unstick probe"   # what the instance wrote after its own tidy (its DB snapshot)
trap - EXIT

# ---------------------------------------------------------------------------------------------------
step 74 "the two operator surfaces of that repair: /healthz counts it, admin doctor prints the setting"
# Same probe, its own output: the boot lines, the anonymous counters and the operator-only schedule.
expect "$(o1 rboot)" 1 "the repairer logs the setting exactly once at boot"
contains "$(o1 rbootline)" 'JOBS_UNSTICK_AFTER' || die "the boot line does not name the setting: $(o1 rbootline)"
contains "$(o1 rbootline)" 'after=30s' || die "the boot line does not name the age: $(o1 rbootline)"
contains "$(o1 rbootline)" 'transcode_hard_timeout=5m0s' || die "the boot line does not name the hard timeout beside it: $(o1 rbootline)"
expect "$(o1 vboot)" 1 "an instance with JOBS_UNSTICK_AFTER=0 says so at boot, once"
# An age SHORTER than the hard timeout is obeyed and warned about — and the warning is about the
# process this one cannot see into, not about its own jobs.
contains "$(o1 rwarn)" 'shorter than TRANSCODE_HARD_TIMEOUT' || die "an age under the hard timeout did not warn: $(o1 rwarn)"
contains "$(o1 rwarn)" 'SECOND process' || die "the warning does not name what it is about: $(o1 rwarn)"
expect "$(o1 vwarn)" 0 "an instance with the repair OFF must not warn about its age"
# /healthz: five counts, and the repair this step just watched is among them. No setting appears —
# the route is anonymous (§15), which is exactly why doctor exists below.
O1_HEALTH=$(o1 rhealth)
contains "$O1_HEALTH" '"repair":{"repaired":1' || die "/healthz did not count the repair: $O1_HEALTH"
contains "$O1_HEALTH" '"requeued":1' || die "/healthz's repair block: $O1_HEALTH"
contains "$O1_HEALTH" '"released":0' || die "/healthz's repair block: $O1_HEALTH"
contains "$O1_HEALTH" '"failed":0' || die "/healthz's repair block: $O1_HEALTH"
contains "$O1_HEALTH" '"errors":0' || die "/healthz's repair block: $O1_HEALTH"
case $O1_HEALTH in
  *unstick*|*JOBS_UNSTICK*|*after_s*) die "/healthz names a CONFIGURED value: $O1_HEALTH";;
esac
# doctor: the setting, the interval, and the promise about a live job — to somebody inside the container.
contains "$(o1 doctorrepair)" 'JOBS_UNSTICK_AFTER' || die "doctor's repair check does not name the setting: $(o1 doctorrepair)"
contains "$(o1 doctorrepair)" '30s' || die "doctor's repair check does not print the age: $(o1 doctorrepair)"
contains "$(o1 doctorrepair)" 'never repaired' || die "doctor's repair check does not state the live-job rule: $(o1 doctorrepair)"
contains "$(o1 doctorwarn)" 'SECOND process' || die "doctor does not warn about the age being under the hard timeout: $(o1 doctorwarn)"
contains "$(o1 doctorsummary)" '19 checks' || die "doctor's summary: $(o1 doctorsummary)"
# At least one warn (the repair line asserted above). Not EXACTLY one: a stack whose PUBLIC_BASE_URL is a
# public origin on the shipped credentials adds a second warn of its own (step 61), so the count differs by
# .env and by origin, not by this bundle — the same rule step 51 follows for the repair check's verdict.
case "$(o1 doctorsummary)" in
  *" 0 warn"*) die "doctor's summary does not carry the repair warning: $(o1 doctorsummary)";;
esac
expect "$(o1 doctorsecret)" 0 "admin doctor printed the probe account's password"
echo "[e2e]   healthz repair={repaired 1, requeued 1} and doctor's repair check both read the same schedule"

# ---------------------------------------------------------------------------------------------------
step 75 "the sweeper's destroy-failure backoff SURVIVES a restart (N004): a crash loop cannot hammer storage"
# Three `clippet serve` processes over one database, in one throwaway container — a restart is a new
# PROCESS, and that is the whole point of the table. The lane's own app is left alone: making a Destroy
# fail needs a bucket that black-holes, which the stack under test must not have.
#
#   1. a working bucket: one clip, made ready, then force-expired
#   2. S3_ENDPOINT pointing at a closed port: the sweeper's Destroy fails and the backoff is recorded
#   3. the same, restarted: the schedule is RESTORED and the clip is NOT attempted again
#
# SWEEP_INTERVAL is 20s rather than the 3s the other probes use, and that is load-bearing: the per-tick
# budget is the interval minus 5s (never under half of it), and a Destroy against a closed port takes
# longer than a 1.5s budget — so with a 3s interval every attempt is CUT SHORT by the tick and counts as
# a DEFERRAL, never as a failure (§20 erratum 101). No failure, no backoff, and nothing to remember.
#
# There is no fourth phase that lets the clip finish: the backoff it is under is two minutes, waiting it
# out would be two minutes of a run, and the owner's inline Destroy cannot help (a `deleting` clip 404s
# on every route by design). The database is the container's own and goes with it; the objects are in the
# shared bucket under a prefix of this step's own, and the host removes that prefix below.
O1_PREFIX2="e2e-o1b-$(date +%s)/"
O1_OUT2="$E2E_DIR/o1-sweepmem.txt"
# The removal of that prefix is armed HERE, before the objects exist, and not only written at the foot
# of the step: everything between this line and there can `die` — the probe container itself, every
# assertion on its log, the count below — and a step that died would otherwise leave `e2e-o1b-<epoch>/`
# in the bucket for good. It is bounded (four objects) and outside the stack's own key prefix, so
# `admin orphans` never reports it and nobody would ever find it. The trap is the pattern 37-library.sh
# and 55-owner.sh use for their probe containers, and it is cleared once the explicit removal has run.
#
# The paths do NOT prepend the stack's own S3_KEY_PREFIX: `-e S3_KEY_PREFIX=…` on the probe REPLACES
# compose's value rather than nesting under it (the convention step 53's ops probe uses), so
# $O1_PREFIX2 already IS the whole prefix and sits at the bucket root. Prepending would agree with the
# objects only while the shipped default leaves S3_KEY_PREFIX unset; with `S3_KEY_PREFIX=foo/` in .env
# (supported, .env.example) it would list an empty `foo/e2e-o1b-…/`, count 0, die claiming the Destroy
# had not failed, and leave the four objects it did write behind.
#
# Which is why the prefix goes to `admin objects` (the harness's `store_obj`) exactly as written: its
# `--prefix` and `rm --prefix` forms take a prefix from the bucket ROOT, not under the stack's key prefix
# (errata 281 (f), 346). What came before them was a private client pointed at the BUNDLED MinIO, which
# stopped being the reference stack's store when it moved to R2, at which point the count below read 0
# off a bucket nothing had written to and this step would have reported a Destroy failure it never
# proved.
o1_rm_prefix2() {   # best effort, and never the cause of a failure: it runs from an EXIT trap
  if ! store_has_bucket; then
    echo "[e2e]   NOTE: this stack keeps its media in a directory, so the probe's prefix $O1_PREFIX2 is in no bucket for the harness to sweep"
    return 0
  fi
  # The prefix must be the harness's own minted shape before `rm --prefix` sees it (§20 erratum
  # 290, AUDIT #28): an empty variable here would have named the bucket root.
  store_rm_prefix_ok_ "${O1_PREFIX2:-}" || return 0
  # A subshell: `store_obj` DIES on a failure, and this one promises never to be the cause of one.
  ( store_obj rm --prefix "$O1_PREFIX2" >/dev/null ) 2>/dev/null \
    || echo "[e2e]   NOTE: could not remove the sweep-memory probe's prefix $O1_PREFIX2 (four objects, outside the stack's key prefix)"
}
# STORAGE_PING_ATTEMPTS/STORAGE_PING_INTERVAL (§20 erratum 283) are what made this step the slowest
# in the suite. Phases 2 and 3 below boot the probe with S3_ENDPOINT pointed at a CLOSED port ON
# PURPOSE, and serve.go's boot ping loop runs BEFORE the worker and the sweeper start — so at the
# shipped 10 × (3s timeout + 3s pause) each of those two boots spent a full sixty seconds failing to
# reach a port this step had just shut, before the sweeper it is actually testing had ticked once.
# Measured identically in three recorded runs (`failsecs=74/74/73`), and 106s of a 763s suite.
#
# Two attempts a second apart is the same loop with the same outcome — the endpoint is closed, every
# attempt fails, the process serves degraded and logs `storage unreachable` — reached in ~7s instead
# of ~60s. NOTHING this step asserts is about the ping loop: phases 2 and 3 are about a failed
# `Destroy` being REMEMBERED across a restart, which is the sweeper's memory, and the loop's own
# shape stays at its default in every other container the suite boots and in TestDefaults.
trap 'o1_rm_prefix2' EXIT
dc run --rm -T --no-deps \
  -e DATA_DIR=/tmp/o2 -e SCRATCH_DIR=/tmp/o2-scratch -e S3_KEY_PREFIX="$O1_PREFIX2" \
  -e REGISTRATION=open -e CAPTURE_READY_WAIT=0 -e SWEEP_INTERVAL=20s -e SWEEP_GRACE=1s \
  -e STORAGE_PING_ATTEMPTS=2 -e STORAGE_PING_INTERVAL=1s \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 \
  --entrypoint sh app -c '
    set -e
    mkdir -p /tmp/o2 /tmp/o2-scratch
    wait_up() { i=0; while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && return 0; i=$((i+1)); sleep 1; done; echo "the probe app never became healthy"; exit 1; }
    clip_state() { clippet admin ls "$1" | sed -n "s/^state: *//p" | awk "{print \$1}"; }
    clippet migrate >/dev/null

    # 1. a working bucket: one small ready clip, force-expired so it is a sweep candidate
    clippet serve >/tmp/s1.log 2>&1 &
    SP=$!
    wait_up
    curl -sS -o /dev/null -c /tmp/jar -X POST -H "Content-Type: application/json" \
      -d "{\"username\":\"o2user\",\"password\":\"o2-pass-12345\"}" http://127.0.0.1:8080/api/auth/register
    /usr/local/bin/ffmpeg -hide_banner -loglevel error -f lavfi -i testsrc2=size=320x180:rate=15:duration=2 \
      -c:v libx264 -preset ultrafast -pix_fmt yuv420p -movflags +faststart -y /tmp/small.mp4
    SC=$(curl -sS -b /tmp/jar -F "metadata={\"title\":\"o1 sweep memory\"};type=application/json" \
      -F duplicate=allow -F "video=@/tmp/small.mp4;type=video/mp4" http://127.0.0.1:8080/api/clips \
      | grep -o "\"id\":\"[A-Za-z0-9_-]\{12\}\"" | head -1 | sed -n "s/.*:\"\(.*\)\"/\1/p")
    echo "sc=$SC"
    i=0; while [ $i -lt 90 ]; do S=$(clip_state "$SC"); [ "$S" = ready ] && break; i=$((i+1)); sleep 1; done
    echo "scstate=$S"
    kill -9 $SP 2>/dev/null || true
    sleep 1
    clippet admin expire "$SC" --ago 2h >/dev/null

    # 2. a bucket that is not there: every Destroy fails, and the failure is written down
    S3_ENDPOINT=http://127.0.0.1:1 clippet serve >/tmp/s2.log 2>&1 &
    SP=$!
    wait_up
    i=0; while [ $i -lt 120 ]; do grep -q "destroy failed; backing this clip off" /tmp/s2.log && break; i=$((i+1)); sleep 1; done
    echo "failsecs=$i"
    echo "s2sweeps=$(grep -c "msg=sweep " /tmp/s2.log || true)"
    echo "failline=$(grep "destroy failed; backing this clip off" /tmp/s2.log | head -1 | tr -d "\r")"
    echo "failcount=$(grep -c "destroy failed; backing this clip off" /tmp/s2.log || true)"
    kill -9 $SP 2>/dev/null || true
    sleep 1

    # 3. the restart nobody performed: the schedule is restored and NOTHING is attempted again
    S3_ENDPOINT=http://127.0.0.1:1 clippet serve >/tmp/s3.log 2>&1 &
    SP=$!
    wait_up
    i=0; while [ $i -lt 120 ]; do grep -q "msg=sweep " /tmp/s3.log && break; i=$((i+1)); sleep 1; done
    echo "s3firstsweep=$i"
    # A SECOND tick has to happen — before the table, every tick re-attempted the black-holed prefix —
    # so this waits for the tick and not for the clock (§20 erratum 245). It was `sleep 22` against a
    # 20s interval, which is two seconds of margin for a container whose clock steps backwards under
    # load: a late tick made `s3sweeps >= 2` fail for a reason that had nothing to do with the backoff.
    # The bound is generous and the assertion below is unchanged, so a tick that never comes still fails.
    i=0; while [ $i -lt 90 ]; do
      [ "$(grep -c "msg=sweep " /tmp/s3.log || true)" -ge 2 ] && break
      i=$((i+1)); sleep 1
    done
    echo "s3secondsweep=$i"
    echo "s3sweeps=$(grep -c "msg=sweep " /tmp/s3.log || true)"
    echo "restoreline=$(grep "restored the destroy-failure backoff" /tmp/s3.log | head -1 | tr -d "\r")"
    echo "retryafterrestart=$(grep -c "destroy failed; backing this clip off" /tmp/s3.log || true)"
    # The lines themselves, with their timestamps, for the host to compare against the backoff the
    # failure above SCHEDULED. A retry is only a broken promise while that backoff still holds.
    echo "s3faillines=$(grep "destroy failed; backing this clip off" /tmp/s3.log | tr "\n" "|" | tr -d "\r")"
    echo "backoffticks=$(grep -c "backoff=1" /tmp/s3.log || true)"
    echo "s3state=$(clip_state "$SC")"
    kill -9 $SP 2>/dev/null || true
  ' > "$O1_OUT2" 2>&1 || die "the sweeper-memory probe container failed:
$(cat "$O1_OUT2")"
sed -n 's/^/[e2e]   o1-sweepmem: /p' "$O1_OUT2"
o1b() { sed -n "s/^$1=//p" "$O1_OUT2" | tail -1 | tr -d '\r'; }

expect_nonempty "$(o1b sc)" "the sweep-memory clip id"
expect "$(o1b scstate)" ready "the probe clip never became ready"
# Phase 2 recorded a failure and named the clip and its count.
contains "$(o1b failline)" "clip=$(o1b sc)" || die "the failing Destroy did not name the clip: $(o1b failline)"
contains "$(o1b failline)" 'failures=1' || die "the first failure must be the first: $(o1b failline)"
contains "$(o1b failline)" 'retry_in=2m0s' || die "the backoff the failure scheduled: $(o1b failline)"
# Phase 3 — the assertion this step exists for.
contains "$(o1b restoreline)" 'clips=1' || die "the restarted process did not restore the schedule: $(o1b restoreline)"
# The claim is "the restarted process did not re-attempt WHILE THE BACKOFF HELD", and the backoff the
# phase-2 failure scheduled is two minutes long. Asserting a flat zero made that a statement about the
# WALL CLOCK as well: the probe boots a process, waits out a boot delay and two twenty-second ticks, and
# on a busy box (the suite's own bundles running in lanes, host ffmpeg on every core) that took 136 s —
# so the backoff had legitimately expired and the retry it then made was the app doing exactly what it
# promised. §20 erratum 245: compare the timestamps instead. `backoffticks` below is what keeps this
# from going vacuous — at least one tick must have REPORTED the candidate as backed off, which cannot
# happen after the deadline.
$PY - "$(o1b failline)" "$(o1b s3faillines)" <<'PYEOF' || die "the restarted process re-attempted the black-holed prefix while its backoff still held (the backoff was forgotten)"
import re, sys, calendar, time
def stamp(s):
    m = re.search(r'time=(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)', s)
    return calendar.timegm(time.strptime(m.group(1), '%Y-%m-%dT%H:%M:%S')) if m else None
fail = stamp(sys.argv[1])
if fail is None:
    sys.exit('could not read the phase-2 failure timestamp from: %r' % sys.argv[1])
m = re.search(r'retry_in=(\d+)m(\d+)s', sys.argv[1])
if not m:
    sys.exit('the failure line does not name the backoff it scheduled: %r' % sys.argv[1])
deadline = fail + int(m.group(1)) * 60 + int(m.group(2))
early = []
for line in sys.argv[2].split('|'):
    if 'destroy failed' not in line:
        continue
    t = stamp(line)
    if t is None or t < deadline:
        early.append(line.strip()[:160])
if early:
    sys.stderr.write('retried %ds into a %ds backoff:\n  ' % (min(stamp(l) or 0 for l in early) - fail, deadline - fail)
                     + '\n  '.join(early) + '\n')
    sys.exit(1)
print('[e2e]   no destroy was re-attempted inside the %ds backoff (%d line(s) after it, which is correct)'
      % (deadline - fail, len(early) or len([l for l in sys.argv[2].split('|') if 'destroy failed' in l])))
PYEOF
[ "$(o1b backoffticks)" -ge 1 ] 2>/dev/null || die "no tick reported the candidate as backed off after the restart"
[ "$(o1b s3sweeps)" -ge 2 ] 2>/dev/null || die "the restarted process only swept $(o1b s3sweeps) time(s): too few to say it did not retry"
expect "$(o1b s3state)" deleting "the clip keeps its tombstone while the backoff holds"
# The probe's database went with its container; its objects are in the shared bucket, under the prefix
# armed with the trap above, and they are removed here — the clip is mid-backoff, so no instance was ever
# going to get to them inside this run.
#
# COUNT, REMOVE, then assert, in that order: the count is the evidence and the removal is the tidying,
# and doing the assertion first meant a step that dies on it never reaches the removal two lines below.
#
# These two READS are `admin objects --prefix … --count` rather than `prefix_count` (§20 errata 281 (c),
# 346) for one reason: `admin objects <id>` asks about `clips/<id>/` under the STACK's key prefix, and
# what is being counted here is a whole throwaway INSTANCE's key space at the bucket root, whose clip id
# belongs to a database that no longer exists — so the count is of the root-relative prefix itself.
if store_has_bucket; then
  O1_LEFT=$(store_obj --prefix "$O1_PREFIX2" --count | tr -d ' \r')
  o1_rm_prefix2
  O1_LEFT2=$(store_obj --prefix "$O1_PREFIX2" --count | tr -d ' \r')
  trap - EXIT
  [ "${O1_LEFT:-0}" -ge 1 ] 2>/dev/null \
    || die "the failing Destroy left NO objects under $O1_PREFIX2 ($O1_LEFT): then it did not fail the way this step assumed"
  expect "${O1_LEFT2:-0}" 0 "the sweep-memory probe's prefix is empty again"
  echo "[e2e]   a failing Destroy was remembered across a restart and retried by neither process ($O1_LEFT objects removed by hand)"
else
  trap - EXIT
  echo "[e2e]   NOTE: this stack keeps its media in a directory, so the probe wrote its four objects into no bucket: what is left under $O1_PREFIX2 is neither counted nor removed here"
fi

# ---------------------------------------------------------------------------------------------------
step 76 "the card image is taken from a RANGED prefix of the rendition (N051), not the whole file"
# On the LANE's own stack, because the assertion is about bytes on the wire between the app and the
# bucket: one clip long enough for the arithmetic to pay (a 20 s 720p30 rendition is several MiB), one
# chosen frame near the START of it, and the app's own log line for what it fetched.
O1_SRC="$E2E_DIR/o1-poster-src.mp4"
ffmpeg -hide_banner -nostdin -y -f lavfi -i testsrc2=size=1280x720:rate=30:duration=20 \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p -movflags +faststart "$(W "$O1_SRC")" \
  > "$E2E_DIR/o1-poster-src.txt" 2>&1 || die "could not build the ranged-poster fixture: $(tail -3 "$E2E_DIR/o1-poster-src.txt")"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "title=E2E ranged poster" -F "ttl_days=1" \
  -F duplicate=allow -F "file=@$(W "$O1_SRC");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "upload the ranged-poster clip ($(cat "$RESP"))"
O1_P=$(json id); expect_nonempty "$O1_P" "the ranged-poster clip id"
wait_ready "$O1_P" O1_P

# The rendition's real size, from the route that serves it.
# `media_head` (§20 erratum 280, rule (a)): this reads the OBJECT's size, which the log line below is
# compared against, and the object is the same size on either side of a 302.
code=$(media_head "$BASE/api/clips/$O1_P/video.mp4"); expect_media "$code" 200 "HEAD the ranged-poster rendition"
O1_SIZE=$(hdr content-length); expect_nonempty "$O1_SIZE" "the rendition's Content-Length"
[ "$O1_SIZE" -gt 1048576 ] 2>/dev/null \
  || die "the fixture's rendition is only $O1_SIZE bytes; under a mebibyte the whole file is fetched by design, so this step would prove nothing"

# A frame one second in: the prefix is the index plus a fraction of the media.
code=$(http POST "$BASE/api/clips/$O1_P/poster" -b "$JARW" -H 'Content-Type: application/json' -d '{"seconds":1}')
expect "$code" 202 "POST …/poster 1s ($(cat "$RESP"))"
O1_N=0
while [ "$O1_N" -lt 120 ]; do
  code=$(get "$BASE/api/clips/$O1_P/poster" -b "$JARW"); expect "$code" 200 "GET …/poster"
  case "$(json state)" in
    done) break;;
    failed) die "the poster job failed: $(json error)";;
    cancelled) die "the poster job was cancelled";;
  esac
  O1_N=$((O1_N + 1)); sleep 1
done
expect "$(json state)" done "the poster job finished"
expect "$(json applied_seconds)" 1 "the clip carries the chosen moment"
code=$(media_head "$BASE/api/clips/$O1_P/poster.jpg"); expect_media "$code" 200 "the chosen card image is served"

# What it cost, from the app's own log.
O1_LOG="$E2E_DIR/o1-poster.log"
dc logs --no-color --since "$START_TS" app 2>&1 | grep "clip=$O1_P" > "$O1_LOG" || true
sed -n 's/^.*msg=/[e2e]   /p' "$O1_LOG" | grep -E 'poster source|poster taken' || true
has_file "$O1_LOG" 'poster source: a ranged prefix of the rendition' \
  "the poster downloaded the whole rendition instead of a ranged prefix"
lacks_file "$O1_LOG" 'poster source: the whole rendition' "the ranged prefix could not produce the frame and the whole file was fetched"
# BOTH numbers are read out of the ONE line that reports the fetch, never out of the whole log. Since
# the quality tiers this fixture — a faststart H.264 mp4 — takes the remux fast path, so the clip has
# NO rendition object and `clip ready` truthfully logs `rendition_bytes=0` (§20 errata 200-201); an
# unscoped `rendition_bytes=` therefore read that zero and this step failed on a poster that had done
# exactly what it promised. The file the poster ranges is `db.PlayObject`'s — here the upload itself,
# which is also what `/video.mp4` served for O1_SIZE above, so the equality is the same equality.
O1_FETCH=$(grep 'poster source: a ranged prefix of the rendition' "$O1_LOG" | head -1)
O1_PREFIX_BYTES=$(printf '%s' "$O1_FETCH" | sed -n 's/.*prefix_bytes=\([0-9]*\).*/\1/p')
O1_ROW_BYTES=$(printf '%s' "$O1_FETCH" | sed -n 's/.*rendition_bytes=\([0-9]*\).*/\1/p')
expect_nonempty "$O1_PREFIX_BYTES" "the bytes the poster fetched"
expect "$O1_ROW_BYTES" "$O1_SIZE" "the log's rendition size must be the size the route serves"
[ "$O1_PREFIX_BYTES" -lt "$O1_SIZE" ] 2>/dev/null \
  || die "the poster fetched $O1_PREFIX_BYTES of a $O1_SIZE-byte rendition: that is not a prefix"
echo "[e2e]   the card image cost $O1_PREFIX_BYTES of $O1_SIZE bytes ($((100 - 100 * O1_PREFIX_BYTES / O1_SIZE))% less transferred)"
contains "$(sed -n 's/.*\(ranged_source=[a-z]*\).*/\1/p' "$O1_LOG" | head -1)" 'ranged_source=true' \
  || die "the poster did not record its source as ranged"

code=$(http DELETE "$BASE/api/clips/$O1_P" -b "$JARW"); expect_deleted "$code" "$O1_P" "DELETE the ranged-poster clip"
expect_prefix_count "$O1_P" 0 "the ranged-poster clip's prefix is empty"
summary_row O1 "$O1_P" "ranged poster (N051): 20 s 720p30, one chosen frame taken from a $O1_PREFIX_BYTES-byte prefix of a $O1_SIZE-byte rendition; deleted"
