# transient.sh — the ONE classifier of a failed bundle (run 9, lane `retry`; scripts/e2e.d/README.md "Retry").
# Sourced by scripts/e2e.sh. No side effects; it reads files and prints.
#
#   transient_sig <bundle log> [context dir]   → a SIGNATURE on stdout, or nothing
#
# A signature names a failure the PRODUCT did not cause: the request never got an answer, an edge
# answered for the origin, or the docker daemon refused to start a probe. Those, and only those, are
# what the pool retries once (e2e.sh's e2e_run_bundle_). Everything else prints nothing and is never
# retried: an `expect` on a status the app answered (a 4xx/5xx with its own body — gate 27a's 409,
# gate 28b's 301), a Go or visual assertion, a missing fixture, anything this file cannot name.
#
#   curl-<N>     curl exit 7/28/35/52/56 on the failing request: could not connect, timed out, TLS
#                handshake, empty reply, receive failure — a `curl: (N)` line in the 15 lines before the
#                FAIL line, NOT followed by curl_'s own "retrying once" note (that one was absorbed), and
#                only when it is the failing request's: the ring under FAIL ends in 000, or there is no
#                ring, or the curl line is the line right before FAIL.
#   edge-<code>  the failing request (the last line of the request ring printed under FAIL) answered
#                502 or 520-524 AND an edge said so: a `cf-ray`/`server: cloudflare` header or a
#                Cloudflare error body, in the log or in the bundle's last response files. A 502 with no
#                edge evidence is an answer from the stack under test, and stays a failure.
#   pw-closed / pw-reset / pw-timeout
#                Playwright's net::ERR_CONNECTION_CLOSED / net::ERR_CONNECTION_RESET /
#                net::ERR_TIMED_OUT or ERR_CONNECTION_TIMED_OUT, from the FAIL line on, or in the 15 lines before it.
#   docker-125   the daemon refused (docker's own exit 125): `docker: Error response from daemon` (the
#                CLI's own line for run/create; not one dc's "retrying once" note follows), or an exit/rc
#                of 125 named on the FAIL line.
#
# The order is the rule: an edge 5xx first (it is an answer, but not the app's); then an `expect`
# mismatch on a real status (`got 'NNN'`) ends the question with NOTHING, whatever else the log holds;
# then curl, Playwright and docker. The FAIL line is the LAST `[e2e] FAIL step` line in the log (the
# bundle's die). A log with no FAIL line (a runner killed from outside) is not classified.
# E2E_SELFTEST=1 pins every row of this, on planted logs, with real failures from gates 23, 27a, 28b.
transient_sig() {
  local log=$1 ctx=${2:-} f files=""
  [ -s "$log" ] || return 0
  # Edge evidence in the bundle's last response files (resp.hdr / page.html / resp.json are where the
  # wrappers write the failing request's answer), read by the SAME awk as the log — one process per
  # call, because the self-test classifies thirty planted logs and a fork is not free on Windows.
  if [ -n "$ctx" ]; then
    for f in "$ctx/resp.hdr" "$ctx/page.html" "$ctx/resp.json"; do [ ! -s "$f" ] || files="$files
$f"; done
  fi
  # The response files are newline-separated paths, split on newline only (this function's own IFS).
  local IFS='
'
  # shellcheck disable=SC2086
  awk -v lf="$log" '
    FILENAME != lf {
      l = tolower($0)
      if (FILENAME ~ /resp\.hdr$/ && l ~ /^(cf-ray:|server: *cloudflare)/) edge = 1
      if (FILENAME !~ /resp\.hdr$/ && l ~ /cloudflare ray id|error code:? *5(02|2[0-4])/) edge = 1
      next
    }
    { sub(/\r$/, ""); line[++nl] = $0 }
    /^\[e2e\] FAIL step/ { f = nl }
    END {
      if (!f) exit
      lo = f - 15; if (lo < 1) lo = 1
      msg = line[f]
      # the request ring under the FAIL line: "[e2e]     METHOD URL -> STATUS"
      st = ""
      for (i = f + 1; i <= nl; i++) if (line[i] ~ /^\[e2e\]     [A-Z]+ .* -> [^ ]+$/) { n = split(line[i], w, " "); st = w[n] }
      for (i = lo; i <= nl; i++) if (tolower(line[i]) ~ /(^|[^a-z])cf-ray|server: *cloudflare|cloudflare ray id|error code:? *5(02|2[0-4])/) edge = 1
      if (st ~ /^(502|52[0-4])$/ && edge == 1) { print "edge-" st; exit }
      # an expect on a status somebody ANSWERED: never a transient
      if (match(msg, /got .[0-9][0-9][0-9]./)) { c = substr(msg, RSTART + 5, 3); if (c != "000") exit }
      # curl: the last `curl: (N)` before the FAIL line, unless curl_ retried it and went on
      cl = 0
      for (i = lo; i < f; i++) if (line[i] ~ /^curl: \((7|28|35|52|56)\)/) cl = i
      # …and only when it IS the failing request: the ring under FAIL ends in no answer (000), or there
      # is no ring, or the curl line is the very line before FAIL (an ERR-trap failure, as in gate-nits1.log).
      # A probe that printed a `curl: (7)` while it waited for a server to boot, and then failed on
      # something the ring shows was answered, is not a transient.
      if (cl && line[cl + 1] !~ /retrying once/ && (st == "" || st == "000" || st == "no-answer" || cl == f - 1)) {
        match(line[cl], /\([0-9]+\)/); print "curl-" substr(line[cl], RSTART + 1, RLENGTH - 2); exit
      }
      for (i = lo; i <= nl; i++) {
        if (line[i] ~ /net::ERR_CONNECTION_CLOSED/) { print "pw-closed"; exit }
        if (line[i] ~ /net::ERR_CONNECTION_RESET/) { print "pw-reset"; exit }
        if (line[i] ~ /net::ERR_(CONNECTION_)?TIMED_OUT/) { print "pw-timeout"; exit }
      }
      # docker: the docker CLI daemon-refusal line, unless dc retried it (its note follows) and went on;
      # or an exit/rc of 125 on the FAIL line itself.
      dl = 0
      for (i = lo; i <= nl; i++) if (line[i] ~ /^docker: Error response from daemon/) dl = i
      if (dl && line[dl + 1] !~ /retrying once/ && line[dl + 2] !~ /retrying once/) { print "docker-125"; exit }
      if (msg ~ /(exit(ed)?( code| status)?|rc[=:]) ?125([^0-9]|$)/) { print "docker-125"; exit }
    }' "$log" $files
}
# transient_retry_user <run user> → the run's user stem as a RETRY's bundle_account sees it (run 9, lane
# `retry`). The retry runs on a FRESH namespace so nothing the first attempt registered can answer 409: the run's
# stamp gets a `2` (`e2e_1791247704_ac` → `e2e_17912477042_ac`). The stamp and not the suffix, because
# a suffix has no room (102-dedupe registers BOTH `dd` and `dd2`, so `dd` → `dd2` would collide with its
# own first attempt, and a three-character suffix cannot grow): 11 digits are still inside the sweep's
# `^e2e_[0-9]{9,11}(_[a-z0-9]{1,3})?$`, and no first attempt ever has 11.
transient_retry_user() { printf '%s2' "$1"; }
