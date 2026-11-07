# 15-ttl.sh — a clip's lifetime: expiry while it is still in flight, and the TTL keep-alive (touch, the
# rescue from the sweeper against an untouched control, keep-forever). Steps 15 and 19, which were the
# serial core of scripts/e2e.sh until run 9's lane `pool` (scripts/e2e.d/README.md "The core in the pool").
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=TTL_ buckets=-
# e2e-covers: internal/httpapi/clip_handlers.go internal/sweeper/sweeper.go internal/lifecycle/lifecycle.go
# e2e-covers: internal/db/clips.go cmd/clippet/admin.go web/templates/clip.html
#   Every clip it expires or touches is its own (F, T, U, uploaded here as the run's owner); of the
#   harness's clips it touches only C, the never-expiring one, and a touch of a clip kept forever changes
#   nothing (asserted). It signs the harness's second user in with a session of its own. Every count is one
#   clip's own prefix. The sweeper tick it waits for is the instance's, but what it asserts is about its
#   own two clips — a tick that also destroys another bundle's expired clip changes neither.
#
# Variables are prefixed TTL_.

step 15 "expire-while-in-flight (best effort): upload F, expire immediately → gone within 150s whatever its state"
code=$(upload TTL_F std-720p30.mp4 '{"title":"E2E inflight"}'); expect "$code" 201 "upload F"
TTL_F=$(json id)
out=$(dc exec -T app clippet admin expire "$TTL_F" --ago 2h) || die "admin expire F failed: $out"
code=$(get "$BASE/api/clips/$TTL_F"); echo "[e2e]   F=$TTL_F state at expiry: $(json state 2>/dev/null || echo "http $code")"
wait_gone "$TTL_F" F
step 19 "TTL keep-alive (#2): touch resets the timer, rescues an expired clip from the sweeper, keep-forever ends it"
code=$(upload TTL_T std-720p30.mp4 '{"title":"E2E touch","ttl_days":7}'); expect "$code" 201 "upload T ($(cat "$RESP"))"
TTL_T=$(json id); expect_nonempty "$TTL_T" "T id"
expect "$(json ttl_days)" 7 "T ttl_days"
TTL_EXP0=$(json expires_at); expect_nonempty "$TTL_EXP0" "T expires_at at upload"
# U is T's control for the rescue below: same clip, same TTL, everything the same except that it is
# never touched. It is uploaded now so it transcodes while the rest of the step runs.
code=$(upload TTL_U std-720p30.mp4 '{"title":"E2E touch control","ttl_days":7}'); expect "$code" 201 "upload U ($(cat "$RESP"))"
TTL_U=$(json id); expect_nonempty "$TTL_U" "U id"
wait_ready "$TTL_T" T; wait_ready "$TTL_U" U
# 1. The timer restarts from now: a full 7 days again, and never backwards.
#    NOT a strict seconds-scale comparison against the upload's expiry, deliberately. Both values come from
#    the APP's clock, and a container clock can step backwards under the host's time sync (observed here:
#    the app running 12s behind the host, stepping back ~8s mid-run), which would invert a comparison whose
#    real gap is the couple of seconds it took to transcode. The claim "recomputed from now, not extended"
#    is proven at step 5 instead, where a force-expired clip jumps SEVEN DAYS forward in one touch — a gap
#    no clock jitter can fake. Here: a full TTL ahead of now (±10 min absorbs the skew) and never earlier.
#    "Never earlier" carries the same slack as everything else that reads this clock: a STRICT comparison
#    of the two app-minted values is precisely what the paragraph above says must not be written, and the
#    line below was one until it cost a gate run on 2026-09-08 (the app container stepped 13 s backwards
#    between the upload and the touch, so a correct touch produced an expiry 13 s "earlier"). A touch that
#    really shortened a lifetime moves it by hours or days, so 120 s of slack keeps the assertion and
#    drops the false failure.
code=$(http POST "$BASE/api/clips/$TTL_T/touch" -b "$JARW"); expect "$code" 200 "touch T ($(cat "$RESP"))"
expect "$(json state)" ready "touch → state unchanged"; expect "$(json ttl_days)" 7 "touch → ttl_days unchanged"
TTL_EXP1=$(json expires_at); expect_nonempty "$TTL_EXP1" "touch → expires_at"
$PY - "$TTL_EXP1" "$TTL_EXP0" <<'PYEOF' || die "touch moved expires_at backwards"
import calendar, sys, time
new = calendar.timegm(time.strptime(sys.argv[1], '%Y-%m-%dT%H:%M:%SZ'))
old = calendar.timegm(time.strptime(sys.argv[2], '%Y-%m-%dT%H:%M:%SZ'))
if new < old - 120:
    sys.exit('%s is %ds before %s, further back than the container clock can step'
             % (sys.argv[1], old - new, sys.argv[2]))
PYEOF
expect_ttl_ahead "$TTL_EXP1" 604800 "touch → expires_at ≈ now + 7 days"
echo "[e2e]   T expires_at $TTL_EXP0 → $TTL_EXP1"
# 2. It is the OWNER's keep-alive: anonymous 401, another user 403, a capture token 403 (§20 erratum 19).
code=$(http_refused POST "$BASE/api/clips/$TTL_T/touch"); expect "$code" 401 "anonymous touch"
expect "$(json error)" unauthorized "anonymous touch error code"
# The second user is the harness's (step 3 registers it): this bundle signs in a session of its own.
TTL_JAR2="$CTX_DIR/cookies-b.txt"; TTL_JAR2W=$(W "$TTL_JAR2"); rm -f "$TTL_JAR2"
code=$(http POST "$BASE/api/auth/login" -c "$TTL_JAR2W" -H 'Content-Type: application/json' \
  -d "{\"username\":\"${E2E_USER}_b\",\"password\":\"$E2E_PASS\"}")
expect "$code" 200 "log the second user in"
code=$(http_refused POST "$BASE/api/clips/$TTL_T/touch" -b "$TTL_JAR2W"); expect "$code" 403 "another user's touch"
expect "$(json error)" forbidden "another user's touch error code"
code=$(http POST "$BASE/api/auth/tokens" -b "$JARW" -H 'Content-Type: application/json' -d '{"name":"e2e touch"}')
expect "$code" 201 "mint a token for the touch negative"
TTL_TOK=$(json token); TTL_TID=$(json id)
code=$(http_refused POST "$BASE/api/clips/$TTL_T/touch" -H "Authorization: Bearer $TTL_TOK"); expect "$code" 403 "token touch"
expect "$(json error)" token_not_allowed "token touch error code"
code=$(http DELETE "$BASE/api/auth/tokens/$TTL_TID" -b "$JARW"); expect "$code" 204 "revoke the touch token"
# 3. A clip kept forever has no timer to reset: 200, and expires_at stays null (C was uploaded ttl_days=0).
code=$(http POST "$BASE/api/clips/$C/touch" -b "$JARW"); expect "$code" 200 "touch a never-expiring clip"
expect_empty "$(json expires_at)" "touch never → expires_at stays null"
expect "$(json ttl_days)" 0 "touch never → ttl_days"
# 4. The owner controls are the owner's alone (the anonymous page carries neither URL nor button).
code=$(page "$BASE/$TTL_T" -b "$JARW"); expect "$code" 200 "owner share page"
has_file "$PAGE" "data-touch-url=\"/api/clips/$TTL_T/touch\"" "owner share page: the keep-alive URL"
has_file "$PAGE" 'data-keep-forever' "owner share page: the keep-forever control"
has_file "$PAGE" 'data-expires' "owner share page: the countdown marker copy-link keys on"
code=$(page "$BASE/$TTL_T"); expect "$code" 200 "anonymous share page"
# All FOUR owner-only lifetime attributes, which is what §20 erratum 62 claims this step asserts
# by name. The Lifetime LINE itself stays public - it is a fact about a link somebody was handed.
lacks_file "$PAGE" 'data-touch-url' "anonymous share page"
lacks_file "$PAGE" 'data-keep-forever' "anonymous share page"
lacks_file "$PAGE" 'data-expires' "anonymous share page"
lacks_file "$PAGE" 'data-copy-note' "anonymous share page"
# 5. THE RESCUE, against a control. Both clips are forced two hours past their expiry; only T is touched.
#    The CONTROL's destruction is what proves a full sweep tick ran and looked at both — a fixed sleep
#    proves nothing, since a tick that never happened looks exactly like a clip that survived one. U is
#    swept away by that tick, so it needs no cleanup of its own.
# ttl_rescue_once <id>: backdate, then touch. Returns 1 when the SWEEPER got there first.
# There is a real race here: `admin expire` puts the clip two hours past its expiry, and a sweep tick
# whose SweepCandidates + MarkDeletingIfDue land in the milliseconds before the touch destroys it — the
# touch then correctly answers 404 and the step would die as if the feature were broken. The odds are
# per ATTEMPT and they scale with the tick: the window is the few hundred milliseconds between the two
# commands, so at the 15 s tick this run configures (§20 erratum 245) an attempt loses it roughly 2% of
# the time rather than the 0.5% the shipped 60 s tick gave. Two things narrow and then absorb it: U's
# expire has moved BELOW (a whole `docker compose exec` round trip used to sit inside the window), and a
# 404 buys one fresh clip and one more attempt — which puts a RUN at about 0.04%. Twice is not luck.
ttl_rescue_once() {
  out=$(dc exec -T app clippet admin expire "$1" --ago 2h) || die "admin expire T failed: $out"; echo "[e2e]   $out"
  TTL_EXPIRED=${out##*expires_at=}; TTL_EXPIRED=${TTL_EXPIRED%%[!0-9TZ:-]*}; expect_nonempty "$TTL_EXPIRED" "the backdated expires_at"
  code=$(http POST "$BASE/api/clips/$1/touch" -b "$JARW")
  case $code in
    200) TTL_EXP2=$(json expires_at); return 0 ;;
    404) return 1 ;;
    *)   die "rescue touch → $code, want 200 ($(cat "$RESP"))" ;;
  esac
}
if ! ttl_rescue_once "$TTL_T"; then
  echo "[e2e]   NOTE: the sweeper destroyed $TTL_T between \`admin expire\` and the rescue touch (the known ~2%/attempt race at this run's 15s tick; ~0.04% per RUN with this retry); retrying once with a fresh clip"
  code=$(upload TTL_T std-720p30.mp4 '{"title":"E2E touch retry","ttl_days":7}'); expect "$code" 201 "re-upload T ($(cat "$RESP"))"
  TTL_T=$(json id); expect_nonempty "$TTL_T" "T id (retry)"; wait_ready "$TTL_T" T
  ttl_rescue_once "$TTL_T" || die "the rescue touch answered 404 twice: a touched clip is being destroyed inside the same tick, or expire/touch is broken"
fi
out=$(dc exec -T app clippet admin expire "$TTL_U" --ago 2h) || die "admin expire U failed: $out"; echo "[e2e]   $out"
expect_ttl_ahead "$TTL_EXP2" 604800 "rescue touch → expires_at ≈ now + 7 days"
# The clock-proof form of "recomputed from now, not extended": two hours in the past → seven days ahead,
# in one touch. Nothing but a recompute produces that jump, and no clock jitter can manufacture it.
later "$TTL_EXP2" "$TTL_EXPIRED" "the touch did not recompute the expiry from now"
echo "[e2e]   rescue: expires_at $TTL_EXPIRED → $TTL_EXP2 (a 7-day jump from a backdated expiry)"
t0=$SECONDS
until code=$(get "$BASE/api/clips/$TTL_U"); [ "$code" = 404 ]; do
  [ $((SECONDS - t0)) -lt 120 ] || die "the untouched control clip $TTL_U outlived 120s (≥8 ticks at this run's 15s interval; last status $code)"
  code=$(get "$BASE/api/clips/$TTL_T"); expect "$code" 200 "the touched clip $((SECONDS - t0))s into the sweep window"
  sleep 1   # was 5 (lane speed2): two unlimited GETs; the tick is 15 s and the deadline 120 s of time
done
echo "[e2e]   control U destroyed after $((SECONDS - t0))s — a full sweep tick ran, and it looked at both clips"
code=$(get "$BASE/api/clips/$TTL_T"); expect "$code" 200 "the touched clip survived the tick that destroyed the control"
expect "$(json state)" ready "the touched clip's state after that tick"
expect "$(json expires_at)" "$TTL_EXP2" "the touched clip's rescued expiry after that tick"
n=$(prefix_count "$TTL_T"); [ "${n:-0}" -gt 0 ] || die "the rescued clip's bucket prefix is empty"
expect_prefix_count "$TTL_U" 0 "the control clip's bucket prefix after it was swept"
echo "[e2e]   T kept $n objects and expires_at $TTL_EXP2; U (identical, untouched) is gone"
# 6. Keep forever ends the countdown for good; a touch after it is a 200 no-op.
code=$(http PATCH "$BASE/api/clips/$TTL_T" -b "$JARW" -H 'Content-Type: application/json' -d '{"ttl_days":0}')
expect "$code" 200 "keep forever"; expect "$(json ttl_days)" 0 "keep forever → ttl_days"
expect_empty "$(json expires_at)" "keep forever → expires_at null"
code=$(http POST "$BASE/api/clips/$TTL_T/touch" -b "$JARW"); expect "$code" 200 "touch after keep forever"
expect_empty "$(json expires_at)" "touch after keep forever → expires_at stays null"
code=$(http DELETE "$BASE/api/clips/$TTL_T" -b "$JARW"); expect_deleted "$code" "$TTL_T" "DELETE T"
code=$(http POST "$BASE/api/auth/logout" -b "$TTL_JAR2W" || echo 000); echo "[e2e]   logout ${E2E_USER}_b → $code"
summary_row F "$TTL_F" "expired while in flight → 404 + empty prefix"
summary_row T "$TTL_T" "TTL keep-alive: touch resets 7d, survived the tick that swept its control, keep-forever → null; deleted"
summary_row U "$TTL_U" "the control: T's twin, force-expired and NOT touched → destroyed by that tick (this is what proves a tick ran)"
