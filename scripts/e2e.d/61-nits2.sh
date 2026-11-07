# 61-nits2.sh — nit lane 3 (the E2E harness sweep): the one gap in this lane that needed a step of
# its own rather than a correction to an existing one.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=WARN_ buckets=-
# e2e-covers: internal/config/config.go cmd/clippet/serve.go
#   Part (c) asserts an ABSENCE over the WHOLE app log of the run, and that is what kept it exclusive
#   until lane `excl`. The line is a BOOT line of the process `dc logs app` reads — written once, when
#   that process starts, and only when its PUBLIC_BASE_URL is not loopback — so no request any other
#   bundle makes can write it, and the only thing that could is a bundle that RESTARTS the stack's app,
#   which no parallel bundle may do (scripts/e2e.d/README.md's third rule: "restarts, recreates and
#   reconfigures nothing"). Every probe container in the suite, this file's two included, logs to its
#   OWN container, never to the app's log. So the assertion is a property of the step and not of the
#   current bundle list, and the bundle runs in the pool. Its two probes are `dc run --rm` on their own
#   database and scratch under /tmp, killed as soon as they have booted (≈4 s each).
#
# Step 61 covers the shipped-credential boot WARN (§20 erratum 104, DEPLOY §7), which until now was
# unit-tested only (cmd/clippet TestWarnShippedCredentials). It cannot be observed on the stack under
# test: the line fires only when PUBLIC_BASE_URL is NOT loopback, and this suite drives
# http://localhost:<port>. So it is proven where step 35 proves REGISTRATION=first — in a throwaway
# `compose run` container on its own database AND its own scratch under /tmp, removed on exit. The
# process is killed as soon as it has logged its boot lines, so it opens no bucket object, claims no
# job and never touches the live database. The step creates no clip, no user and no token: step 17
# has nothing of ours.
#
# The private SCRATCH_DIR is not tidiness, it is the whole safety of the probe: `compose run` still
# mounts the service's volumes, so a container that redirects DATA_DIR alone boots with an EMPTY
# database over the LIVE `/scratch`, and serve.go's boot scratch janitor then finds every real
# working directory row-less (db.ErrNotFound -> remove, ahead of both the HasLiveJob and the
# `state != processing` guards) and deletes it — including one belonging to an in-flight transcode,
# which fails `permanent` a couple of minutes later and is never retried. A `compose run` of
# `clippet serve` with a private DATA_DIR MUST therefore carry a private SCRATCH_DIR too; serve.go
# MkdirAll's it before the janitor runs, so the janitor scans an empty directory of its own.
#
# It passes over plain http AND over https: the two probes name their own PUBLIC_BASE_URL and the one
# assertion about THIS stack is guarded on the app's own base being loopback.

step 61 "the shipped bucket credentials WARN on a public origin — and the values never print (N005)"

# warn_boot <public base> <access key> <secret key> -> the boot output of one throwaway container.
# `clippet serve` logs this warning before it opens the database (cmd/clippet/serve.go), so a boot is
# all the probe needs. The wait keys on `boot: reconcile` (either log format), the first line serve.go
# logs AFTER the warning — not on the `boot` line, which comes BEFORE it, and not on a `sleep 1` past
# that line, which decided (b)'s absence by time (lane `nits8`'s timing sweep); `|| true` on the kill and
# the wait keeps a process that has already exited out of the ERR trap. SCRATCH_DIR travels with
# DATA_DIR as its SIBLING — see the header, and nit sweep S28 for why the sibling shape rather than a
# nested one: without either, this probe's boot janitor empties the live stack's /scratch.
# A probe that INVENTS its own PUBLIC_BASE_URL must also pin the delivery mode. compose interpolates
# MEDIA_PRESIGN_ENDPOINT for every container of this project, and that origin was chosen to match the
# STACK's base -- so under a fabricated https base a plain-http presign origin is §20 erratum 278(e)'s
# mixed-content BOOT ERROR and the probe never starts. It cost the redirect-mode proof of erratum 280
# one run. `proxy` with both presign variables blank is what a probe with no viewers wants anyway.
warn_boot() {
  dc run --rm -T --no-deps -e PUBLIC_BASE_URL="$1" -e S3_ACCESS_KEY="$2" -e S3_SECRET_KEY="$3" \
    -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
    -e DATA_DIR=/tmp/warnprobe -e SCRATCH_DIR=/tmp/warnprobe-scratch --entrypoint sh app -c '
      set -e
      mkdir -p /tmp/warnprobe
      clippet serve >/tmp/boot.log 2>&1 &
      p=$!
      i=0
      while [ $i -lt 30 ]; do grep -q "boot: reconcile" /tmp/boot.log && break; i=$((i+1)); sleep 1; done
      kill "$p" 2>/dev/null || true
      wait "$p" 2>/dev/null || true
      cat /tmp/boot.log
    ' 2>&1
}
WARN_LINE='the bucket credentials are the ones published in this repository'
WARN_SHIPPED_KEY=clippet                 # compose's own fallbacks (compose.yaml S3_ACCESS_KEY/S3_SECRET_KEY),
WARN_SHIPPED_SECRET=clippet-dev-secret   # which is exactly what the warning is about
WARN_REAL_KEY=e2e-not-the-shipped-key
WARN_REAL_SECRET=e2e-not-the-shipped-secret

# (a) The positive: a PUBLIC origin on the credentials this repository publishes.
WARN_OUT=$(warn_boot https://clippet-warn.invalid "$WARN_SHIPPED_KEY" "$WARN_SHIPPED_SECRET") \
  || die "the shipped-credential probe container failed: $WARN_OUT"
contains "$WARN_OUT" "$WARN_LINE" || die "no shipped-credential warning on a public origin: $WARN_OUT"
contains "$WARN_OUT" 'WARN' || die "the shipped-credential line is not logged at WARN: $WARN_OUT"
contains "$WARN_OUT" 'https://clippet-warn.invalid' || die "the warning does not name the public base it warns about: $WARN_OUT"
# The whole point of the line is that it describes the exposure without adding to it: neither value
# may appear anywhere in the output, and the boot line above it prints the endpoint and bucket only.
contains "$WARN_OUT" "$WARN_SHIPPED_SECRET" && die "the boot output printed the bucket SECRET"
contains "$WARN_OUT" "access_key" && die "the boot output printed an access_key field"
# Every line below is from THIS STEP'S OWN throwaway container and is EXPECTED (Gate 10/12,
# harness). Two kinds arrive: the shipped-credential warning, which is what this step asserts is
# there, and a `storage ping failed ... 400 Bad Request` from the probe's own background ping — it was
# booted with the credentials this repository publishes against whatever bucket this stack uses, so it
# cannot authenticate, by design. `--rm` takes the container before step 16's log gate collects
# anything, so neither reaches the stack's log. The NOTE is here because a reader scanning a
# transcript for WARN meets these first and has no way to tell an asserted one from a stack fault.
echo "[e2e]   NOTE: the warn-probe lines below are this step's OWN container and are asserted, not warnings from the stack under test (the credential warning IS the assertion; the storage ping cannot authenticate by design)"
printf '%s\n' "$WARN_OUT" | grep -E 'WARN' | sed -n 's/^/[e2e]   warn-probe: /p' || true

# (b) The negative control: the same public origin with credentials of its own stays quiet, or (a)
#     proves nothing but that the app logs a warning at boot. The boot line must still be there —
#     absence of a warning in a container that never booted would be vacuous. Its background storage
#     ping cannot authenticate and says so at WARN (serve.go pingLoop); that is this probe's own
#     container, which `--rm` removes before the step-16 log gate collects anything.
WARN_OUT2=$(warn_boot https://clippet-warn.invalid "$WARN_REAL_KEY" "$WARN_REAL_SECRET") \
  || die "the real-credential probe container failed: $WARN_OUT2"
# It is judged AFTER the event: `boot: reconcile` is logged after the point where the warning would
# have been, in the same goroutine, so its presence means the warning had its chance and did not fire.
contains "$WARN_OUT2" 'boot: reconcile' \
  || die "the real-credential probe never got past the warning's point, so its silence proves nothing: $WARN_OUT2"
contains "$WARN_OUT2" "$WARN_LINE" && die "the warning fired on credentials that are not the shipped ones: $WARN_OUT2"
contains "$WARN_OUT2" "$WARN_REAL_SECRET" && die "the boot output printed the configured SECRET"
contains "$WARN_OUT2" "$WARN_REAL_KEY" && die "the boot output printed the configured access key"

# (c) …and the loopback half, live: this stack runs on the shipped credentials (nothing in .env says
#     otherwise on a dev box) and a loopback PUBLIC_BASE_URL, which is the combination that must stay
#     silent — a warning nobody can act on is a warning nobody reads. Skipped, loudly, when the stack
#     under test is on a public origin, where the line is expected to be present.
WARN_HOST=${ENV_BASE#*://}; WARN_HOST=${WARN_HOST%%/*}; WARN_HOST=${WARN_HOST%%:*}
case $WARN_HOST in
  localhost|127.*|::1|'[::1]')
    dc logs --no-color --since "$START_TS" app > "$E2E_DIR/warn-live.log" 2>&1 || die "docker compose logs failed"
    [ -s "$E2E_DIR/warn-live.log" ] || die "empty app log for the loopback control"
    lacks_file "$E2E_DIR/warn-live.log" "$WARN_LINE" \
      "this stack's PUBLIC_BASE_URL is loopback and must not carry the shipped-credential warning"
    echo "[e2e]   loopback ($ENV_BASE): the live stack stays quiet" ;;
  *)
    echo "[e2e]   NOTE: this stack's PUBLIC_BASE_URL ($ENV_BASE) is not loopback, so the live control is skipped" ;;
esac
echo "[e2e]   a public origin on the published credentials warns once, names the origin, and prints neither value"
