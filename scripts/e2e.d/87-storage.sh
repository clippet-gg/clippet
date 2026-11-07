# 87-storage.sh — the storage-cost line (ROADMAP #39, §20 erratum 270): the per-day served-bytes
# counter, its three surfaces (/healthz, `admin stats`, the settings page) and the rule that the
# ANONYMOUS probe carries bytes and never a price.
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 87. It uploads one clip of its own and destroys it; nothing it does outlives the run.
#
# e2e-schedule: exclusive
# e2e-namespace: account=- prefix=SB_ buckets=-
# e2e-covers: internal/db/served.go internal/cost/counter.go internal/httpapi/media.go internal/httpx/health.go
# e2e-covers: internal/httpapi/health.go internal/httpapi/settings.go
#   It asserts on an INSTANCE-WIDE counter — the bytes this app has served today — which every
#   other bundle's media fetch moves. The DELTA assertions below survive a lane, but the flush half
#   would not: it waits for the app's own 30 s window to put THIS step's bytes in the database, and
#   a bundle streaming a rendition beside it would satisfy that wait without the bytes under test
#   ever landing. It also reads `admin stats`, whose lines are instance-wide by definition. So:
#   alone, where every delta is this step's own.
#
# Nothing here assumes an UNPRICED or UNBUDGETED instance: the two prices and STORAGE_BUDGET_BYTES
# are exactly what this feature invites an operator to set, and the gate stack may carry them. The
# step reads which state it is in (SB_PRICED from the settings panel, SB_BUDGETED from /healthz) and
# asserts the matching form of every surface.
#
# Variables are prefixed SB_ (storage bundle) so nothing here can clobber a harness global.

# ---------------------------------------------------------------------------------------------------
step 87 "served bytes are counted once, per UTC day, and the anonymous probe carries bytes and no price"

# The reading BEFORE anything of ours is served. The counter is wired on every `clippet serve`, so
# the two served keys are always present (a zero here means "nothing served today", which is a
# measurement); `stored_bytes` appears once the sweeper has taken its first reading, which it does
# immediately at boot.
code=$(get "$BASE/healthz"); expect "$code" 200 "GET /healthz"
SB_BEFORE=$(json storage_usage.served_today_bytes)
expect_nonempty "$SB_BEFORE" "/healthz storage_usage.served_today_bytes ($(cat "$RESP"))"
SB_STORED=$(json storage_usage.stored_bytes)
expect_nonempty "$SB_STORED" "/healthz storage_usage.stored_bytes (the sweeper's cached reading)"
[ "$SB_STORED" -gt 0 ] 2>/dev/null || die "the instance stores $SB_STORED bytes: the reading is not the clips' sum"
# NO COST, EVER — priced or not (§20 erratum 270(e)). /healthz is anonymous and reachable wherever
# the origin is, and an amount beside the bytes it came from is the operator's per-GB price one
# division away. The whole body is checked, not just the two keys the first cut carried.
expect_empty "$(json storage_usage.stored_cost_month)" "a cost on the anonymous probe"
expect_empty "$(json storage_usage.served_cost_month)" "a cost on the anonymous probe"
case "$(cat "$RESP")" in
  *cost*|*price*) die "/healthz names a cost or a price: $(cat "$RESP")";;
esac
# The store-ping health string keeps its own key beside the object.
expect_nonempty "$(json storage)" "/healthz's storage health string"
# Is this instance budgeted? The object is present only when STORAGE_BUDGET_BYTES is set AND a
# reading exists (erratum 157), so an empty value is the unbudgeted state.
SB_BUDGET=$(json storage_budget.budget)
SB_BUDGETED=0
[ -n "$SB_BUDGET" ] && [ "$SB_BUDGET" -gt 0 ] 2>/dev/null && SB_BUDGETED=1

# One clip of this bundle's own.
code=$(upload SB std-720p30.mp4 '{"title":"E2E storage cost","ttl_days":1}')
expect "$code" 201 "upload the storage-cost clip ($(cat "$RESP"))"
SB_ID=$(json id); expect_nonempty "$SB_ID" "the storage-cost clip id"
wait_ready "$SB_ID" SB

# The DATABASE's own baseline, taken before a byte of ours is served: the flush assertion at the
# foot of this step is a DELTA, because on any instance that has served something this month the
# absolute `month=` already exceeds what this step is about to deliver — and an absolute comparison
# then "succeeds" on its first read with the bytes under test still sitting in memory.
SB_OUT="$E2E_DIR/storage-stats.txt"
dc exec -T app clippet admin stats --no-storage > "$SB_OUT" 2>&1 || die "admin stats failed:
$(cat "$SB_OUT")"
SB_DBBEFORE=$(sed -n 's/^served:.*month=\([0-9][0-9]*\).*/\1/p' "$SB_OUT" | head -1)
expect_nonempty "$SB_DBBEFORE" "admin stats' month= figure before the reads"

# Two ranged reads, and what the WIRE actually delivered each time (curl's own byte count, not the
# range we asked for: the counter's claim is "what the viewer received").
# `media_get` (§20 erratum 280): in `redirect` mode the app answers 302 and the bytes come from the
# bucket, so `%{size_download}` off the app hop would read zero — which is the one answer erratum 270
# forbids a measurement to give. What is counted here is still "what the viewer received", measured on
# the body that actually arrived, wherever it arrived from.
SB_GOT=0
for SB_RANGE in "bytes=0-99999" "bytes=100000-149999"; do
  code=$(media_get "$BASE/api/clips/$SB_ID/video.mp4" -H "Range: $SB_RANGE")
  expect "$code" 206 "the ranged read of $SB_RANGE"
  SB_N=$(wc -c < "$RESP" | tr -d ' ')
  expect_nonempty "$SB_N" "the bytes delivered for $SB_RANGE"
  [ "$SB_N" -gt 0 ] 2>/dev/null || die "the ranged read of $SB_RANGE delivered nothing"
  SB_GOT=$((SB_GOT + SB_N))
done
# A HEAD and a 304 deliver no body and must therefore cost nothing.
code=$(media_head "$BASE/api/clips/$SB_ID/video.mp4"); expect_media "$code" 200 "HEAD the rendition"
SB_ETAG=$(hdr etag); expect_nonempty "$SB_ETAG" "the rendition ETag"
code=$(media_get "$BASE/api/clips/$SB_ID/video.mp4" -H "If-None-Match: $SB_ETAG")
expect "$code" 304 "the conditional GET"
echo "[e2e]   this bundle received $SB_GOT bytes over two ranged reads"

# /healthz counts them, at once: the surfaces add the window that has not been flushed yet, so the
# number is exact the moment the response ends rather than a flush behind it.
code=$(get "$BASE/healthz"); expect "$code" 200 "GET /healthz after the reads"
SB_AFTER=$(json storage_usage.served_today_bytes)
expect_nonempty "$SB_AFTER" "/healthz served_today_bytes after the reads"
SB_DELTA=$((SB_AFTER - SB_BEFORE))
[ "$SB_DELTA" -ge "$SB_GOT" ] 2>/dev/null \
  || die "/healthz counted $SB_DELTA bytes for $SB_GOT delivered (before=$SB_BEFORE after=$SB_AFTER)"
# The 304 and the HEAD are inside that delta too, and neither wrote a body, so a counter that
# charged them would show up as a delta far past what curl received. The rendition is ~1 MiB, so one
# uncounted body is an order of magnitude, not a rounding.
#
# That UPPER bound holds only on a DIRECT origin. Through a CDN the edge may answer a HEAD with a
# GET to the origin, widen a Range, or prefetch — all correct behaviour of something that is not
# this app, and all of it genuinely served by the app. The LOWER bound is the claim about the
# product and is asserted on every origin.
#
# `E2E_NONLOOPBACK` is the harness's own answer to "is there an edge in front of this origin"
# (scripts/e2e.sh, §20 erratum 275); it was derived here first, one case statement, and moved up
# when `http_refused` came to need the same question answered.
#
# It holds on a DIRECT origin in `proxy` mode and nowhere else, and since §20 erratum 280 the harness
# says WHICH of the two reasons it is standing down for. In `redirect` mode the counter is an ESTIMATE
# by design (erratum 278(i)): a request answered with a 302 is charged the WHOLE object, because the
# bytes leave the bucket and never pass this app — so two ranged reads of 150 KB against a ~1 MiB
# rendition are charged about a mebibyte each and the upper bound is MEANT to be exceeded. The LOWER
# bound above is the claim about the product and is asserted on every origin and in both modes;
# over-counting is the safe direction for a cost line, and the two surfaces below say so in words.
if [ "$E2E_MEDIA" = redirect ]; then
  echo "[e2e]   NOTE: the 'nothing bodyless was charged' bound does not apply in redirect mode — every 302 is charged its whole object (§20 erratum 278(i))"
elif [ "$E2E_NONLOOPBACK" = 0 ]; then
  [ "$SB_DELTA" -lt $((SB_GOT + SB_GOT + 65536)) ] 2>/dev/null \
    || die "/healthz counted $SB_DELTA bytes for $SB_GOT delivered: something with no body was charged"
else
  echo "[e2e]   NOTE: the 'nothing bodyless was charged' bound is skipped through an edge origin"
fi
SB_MONTH=$(json storage_usage.served_month_bytes)
[ "$SB_MONTH" -ge "$SB_AFTER" ] 2>/dev/null \
  || die "this month ($SB_MONTH) holds less than today ($SB_AFTER)"

# The settings page — the third surface, and the only one a person reads. Since §20 erratum 349 the panel
# is the OWNER's: read only when step 3 found the run's account owns this stack (E2E_IS_OWNER); on a stack a
# person owns, the panel's assertions and the cost line's (which follows the panel) stand down with a NOTE.
SB_PRICED=
if [ "${E2E_IS_OWNER:-0}" != 1 ]; then
  echo "[e2e]   NOTE: the Storage panel stands down: it is the owner's, and this stack's owner is a person, not the run's account (§20 erratum 349)"
else
# Instance-wide, in the row
# grammar. It is also where this step LEARNS whether the instance is priced: since erratum 270(e)
# the anonymous probe carries no cost at all, so the panel is the only place a price shows.
code=$(page "$BASE/settings" -b "$JARW"); expect "$code" 200 "GET /settings"
has_file_f "$PAGE" '<div class="panel" id="storage">' "the settings page has no Storage panel"
has_file_f "$PAGE" '<h2>Storage</h2>' "the Storage panel has no heading"
has_file_f "$PAGE" 'served this month' "the Storage panel has no served row"
# The panel is also where the delivery mode must be VISIBLE, and this is what pins erratum 278(i)'s
# promise that the over-count is never silent: in `redirect` the served figure reads
# `estimated (redirect)`, and in `proxy` — where the app measured every byte it wrote — it must not.
if [ "$E2E_MEDIA" = redirect ]; then
  has_file_f "$PAGE" 'estimated (redirect)' "the Storage panel does not say the served figure is an estimate on a redirect instance"
else
  lacks_file_f "$PAGE" 'estimated (redirect)' "a proxy instance qualified a figure it measured itself"
fi
SB_PRICED=0
grep -qF '≈' "$PAGE" && SB_PRICED=1
if [ "$SB_PRICED" = 1 ]; then
  echo "[e2e]   this instance IS priced: asserting the priced form of every surface"
  lacks_file_f "$PAGE" 'unpriced' "a priced instance's Storage panel still says unpriced"
else
  echo "[e2e]   this instance is unpriced: asserting the absent form of every cost"
  has_file_f "$PAGE" 'unpriced' "an unpriced instance's cost cell does not say so"
fi
# Since §20 erratum 277 the budget row IS the panel's first row, relabelled `quota`: one row, not
# two, and the percentage with no `used` after it. Step 89 asserts the rest of what it says.
if [ "$SB_BUDGETED" = 1 ]; then
  has_file_f "$PAGE" 'data-storage-quota' "STORAGE_BUDGET_BYTES is set ($SB_BUDGET) but the panel has no quota row"
  has_file_f "$PAGE" '>quota<' "the quota row has no label"
else
  lacks_file_f "$PAGE" 'data-storage-quota' "a quota row with STORAGE_BUDGET_BYTES unset"
fi
# The `budget` row erratum 270 shipped is gone into the quota row on every instance, budgeted or not.
lacks_file_f "$PAGE" '<span class="rl">budget</span>' "the old budget row is still rendered beside the quota"
fi   # E2E_IS_OWNER: the Storage panel

# The FLUSH: the app holds the window in memory and writes it on its own 30 s timer, so a second
# process — the CLI, sharing the database file — sees the bytes only once that has happened. This is
# the durability half of the counter, and it is polled rather than slept for (the container clock
# steps backwards under load). The comparison is the DELTA against the baseline taken
# before the reads.
SB_I=0
SB_DBDELTA=0
while [ "$SB_I" -lt 120 ]; do
  dc exec -T app clippet admin stats --no-storage > "$SB_OUT" 2>&1 || die "admin stats failed:
$(cat "$SB_OUT")"
  SB_DBMONTH=$(sed -n 's/^served:.*month=\([0-9][0-9]*\).*/\1/p' "$SB_OUT" | head -1)
  if [ -n "$SB_DBMONTH" ]; then
    SB_DBDELTA=$((SB_DBMONTH - SB_DBBEFORE))
    [ "$SB_DBDELTA" -ge "$SB_GOT" ] 2>/dev/null && break
  fi
  SB_I=$((SB_I + 1)); sleep 1
done
sed -nE 's/^(served:|cost:|storage:)/[e2e]   \1/p' "$SB_OUT"
has_file "$SB_OUT" '^served:  today=' "admin stats prints no served: line"
# What the line CALLS the number follows the delivery mode, because in `redirect` the app did not send
# the bytes — it sent a link to them (§20 erratum 278(i)).
if [ "$E2E_MEDIA" = redirect ]; then
  has_file "$SB_OUT" 'bytes a viewer was redirected to fetch' "the served: line still claims the app sent the bytes"
  has_file "$SB_OUT" 'estimated (redirect)' "the served: line does not say the figure is an estimate"
else
  has_file "$SB_OUT" 'bytes sent to viewers' "the served: line does not say what it counts"
fi
expect_nonempty "$SB_DBMONTH" "admin stats' month= figure"
[ "$SB_DBDELTA" -ge "$SB_GOT" ] 2>/dev/null \
  || die "the counter never flushed: after ${SB_I}s the database has grown by $SB_DBDELTA bytes this month (from $SB_DBBEFORE to $SB_DBMONTH), want at least $SB_GOT"
echo "[e2e]   the window reached the database after ${SB_I}s (+$SB_DBDELTA bytes, month=$SB_DBMONTH)"
# The cost line follows the same state the settings panel reported.
if [ "$SB_PRICED" = 1 ]; then
  has_file "$SB_OUT" '^cost:' "a priced instance's admin stats prints no cost line"
  has_file "$SB_OUT" 'an ESTIMATE' "the cost line does not say it is an estimate"
elif [ "$SB_PRICED" = 0 ]; then
  lacks_file "$SB_OUT" '^cost:' "an unpriced instance printed a cost line"
else
  echo "[e2e]   NOTE: the cost line is not asserted: the panel that says whether this instance is priced was not read"
fi

code=$(http DELETE "$BASE/api/clips/$SB_ID" -b "$JARW"); expect_deleted "$code" "$SB_ID" "DELETE the storage-cost clip"
expect_prefix_count "$SB_ID" 0 "the storage-cost clip's prefix is empty"
summary_row SB "$SB_ID" "storage cost (#39): $SB_GOT bytes served over two ranged reads, counted by /healthz at once and in the database after ${SB_I}s (+$SB_DBDELTA); deleted"
