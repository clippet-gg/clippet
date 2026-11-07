# 109-chunked.sh — the chunked web upload (DESIGN.md §20 erratum 344): one file as a run of requests, each under
# the edge's per-request cap, and ONE ingest. Sourced by scripts/e2e.sh before the step-16 log gate (see
# scripts/e2e.d/README.md). Step 109.
#
# What this step proves on a live stack: POST /api/uploads opens a session in the server's chunk size; a fixture
# sized to THREE chunks of whatever UPLOAD_CHUNK_BYTES this stack runs (the stack's own small clip padded with an
# MP4 `free` box, so the video stays two seconds whatever the size) goes up as PUT /api/uploads/{uid}/{index}; a
# chunk sent out of order is 409 with `received`, a chunk sent twice is 200 and changes nothing, GET answers
# `received`, a finish before the last chunk is 409 `incomplete`; the finish answers 201 with the clip, which
# goes ready, and whose stored original is the file byte for byte; then the OTHER door — a one-shot POST
# /api/clips of the same file — answers that clip with 200 and `"existing": true` (the duplicate check of the
# one ingest); a second session is abandoned by DELETE and is gone.
#
# e2e-schedule: parallel
# e2e-namespace: account=ck prefix=CK_ buckets=-
# e2e-covers: internal/chunks/** internal/httpapi/uploads.go internal/httpapi/ingest.go internal/httpapi/upload.go
# e2e-covers: internal/sweeper/sweeper.go web/templates/index.html web/static/app.js
#   It creates its OWN account (`bundle_account ck`, under the pattern the run's sweep covers), uploads only as
#   it — the duplicate check is per account, so no other bundle's uploads can answer this file — and deletes the
#   clip it made. It reads no shared clip, restarts and reconfigures nothing (the chunk size is read off the
#   open's answer, never set), and asserts nothing bucket- or instance-wide. Its fixture lives under its own
#   directory and is removed at the end.
#
# Variables are prefixed CK_ so nothing here can clobber a harness global.

step 109 "the chunked web upload (§20 erratum 344): open, three chunks (one out of order, one repeated), finish → the clip, byte-identical; the one-shot door answers it as existing; DELETE abandons"

CK_DIR="${CTX_DIR:-$E2E_DIR}/chunked"
rm -rf "$CK_DIR"; mkdir -p "$CK_DIR"
CK_BASE="$PWD/testclips/small-640x360.mp4"
[ -s "$CK_BASE" ] || die "testclips/small-640x360.mp4 is missing (scripts/testclips.sh)"
bundle_account ck; CK_USER=$BA_USER; CK_JARW=$BA_JARW

# ck_open <size> — POST /api/uploads; the answer in $RESP.
ck_open() {
  printf '{"size":%s,"name":"chunked.mp4","type":"video/mp4","title":"E2E chunked","ttl_days":1}' "$1" > "$CK_DIR/open.json"
  http POST "$BASE/api/uploads" -b "$CK_JARW" -H 'Content-Type: application/json' --data-binary "@$(W "$CK_DIR/open.json")"
}
# ck_put <id> <index> <file> — PUT one chunk; -T streams the file and declares its length.
ck_put() {
  http PUT "$BASE/api/uploads/$1/$2" -b "$CK_JARW" -H 'Content-Type: application/octet-stream' -T "$(W "$3")"
}

# --- (a) the server's chunk size, from a session opened and abandoned ------------------------------------------
code=$(ck_open 1); expect "$code" 201 "open a probe session ($(cat "$RESP"))"
CK_PROBE=$(json id); CK_CHUNK=$(json chunk_bytes)
expect_nonempty "$CK_PROBE" "the probe session's id"
case $CK_CHUNK in ''|*[!0-9]*) die "chunk_bytes is not a number: '$CK_CHUNK'" ;; esac
[ "$CK_CHUNK" -ge 5242880 ] || die "chunk_bytes $CK_CHUNK is under the 5 MiB floor"
expect "$(json received)" 0 "a new session's received"
code=$(http DELETE "$BASE/api/uploads/$CK_PROBE" -b "$CK_JARW"); expect "$code" 204 "DELETE the probe session ($(cat "$RESP"))"
code=$(http GET "$BASE/api/uploads/$CK_PROBE" -b "$CK_JARW"); expect "$code" 404 "the abandoned session is gone"
echo "[e2e]   the stack's chunk is $CK_CHUNK bytes"

# --- (b) a fixture of three chunks: the clip, then one `free` box (size + type + zeros) -------------------------
CK_FILE="$CK_DIR/chunked.mp4"
CK_BASESZ=$(wc -c < "$CK_BASE" | tr -d ' ')
CK_SIZE=$((2 * CK_CHUNK + CK_CHUNK / 2))
CK_PAD=$((CK_SIZE - CK_BASESZ))
[ "$CK_PAD" -gt 8 ] || die "the base clip ($CK_BASESZ bytes) is larger than the fixture"
cp "$CK_BASE" "$CK_FILE"
CK_HEX=$(printf '%08x' "$CK_PAD")
printf "\\x${CK_HEX:0:2}\\x${CK_HEX:2:2}\\x${CK_HEX:4:2}\\x${CK_HEX:6:2}free" >> "$CK_FILE"
dd if=/dev/zero bs=1048576 count=$(((CK_PAD - 8) / 1048576)) 2>/dev/null >> "$CK_FILE"
dd if=/dev/zero bs=1 count=$(((CK_PAD - 8) % 1048576)) 2>/dev/null >> "$CK_FILE"
expect "$(wc -c < "$CK_FILE" | tr -d ' ')" "$CK_SIZE" "the fixture's size"
CK_SUM=$(sha256sum "$CK_FILE" | cut -c1-64)
for CK_I in 0 1 2; do
  # head -c then tail -c +, not dd's GNU-only iflag=skip_bytes,count_bytes (BSD dd on a Mac has no iflag); in this
  # order the reader drains its pipe, so no SIGPIPE reaches pipefail
  { head -c $(((CK_I + 1) * CK_CHUNK)) "$CK_FILE" | tail -c +$((CK_I * CK_CHUNK + 1)) > "$CK_DIR/part$CK_I"; } 2>/dev/null || die "could not cut chunk $CK_I"
done
expect "$(cat "$CK_DIR/part0" "$CK_DIR/part1" "$CK_DIR/part2" | sha256sum | cut -c1-64)" "$CK_SUM" "the three chunks are the file"

# --- (c) open, the chunks — out of order, in order, one twice — and an early finish -----------------------------
code=$(ck_open "$CK_SIZE"); expect "$code" 201 "open the session ($(cat "$RESP"))"
CK_ID=$(json id); expect_nonempty "$CK_ID" "the session's id"
expect "$(json chunk_bytes)" "$CK_CHUNK" "the session's chunk size"
code=$(ck_put "$CK_ID" 1 "$CK_DIR/part1"); expect "$code" 409 "chunk 1 before chunk 0 ($(cat "$RESP"))"
expect "$(json error)" out_of_order "the out-of-order refusal's code"
expect "$(json received)" 0 "the out-of-order refusal says where to resume"
code=$(ck_put "$CK_ID" 0 "$CK_DIR/part0"); expect "$code" 200 "chunk 0 ($(cat "$RESP"))"
expect "$(json received)" 1 "received after chunk 0"
code=$(ck_put "$CK_ID" 0 "$CK_DIR/part0"); expect "$code" 200 "chunk 0 again ($(cat "$RESP"))"
expect "$(json received)" 1 "a repeated chunk changes nothing"
code=$(http POST "$BASE/api/uploads/$CK_ID/finish" -b "$CK_JARW"); expect "$code" 409 "a finish before the last chunk ($(cat "$RESP"))"
expect "$(json error)" incomplete "the early finish's code"
code=$(ck_put "$CK_ID" 1 "$CK_DIR/part1"); expect "$code" 200 "chunk 1 ($(cat "$RESP"))"
code=$(ck_put "$CK_ID" 2 "$CK_DIR/part2"); expect "$code" 200 "chunk 2 ($(cat "$RESP"))"
code=$(http GET "$BASE/api/uploads/$CK_ID" -b "$CK_JARW"); expect "$code" 200 "GET the session ($(cat "$RESP"))"
expect "$(json received)" 3 "GET says all three are in"
echo "[e2e]   session $CK_ID: three chunks of $CK_SIZE bytes in, one refused out of order, one repeated"

# --- (d) the finish: the one-shot's answer, a clip that goes ready, the original byte for byte ------------------
code=$(http POST "$BASE/api/uploads/$CK_ID/finish" -b "$CK_JARW"); expect "$code" 201 "finish ($(cat "$RESP"))"
CK_CLIP=$(json id); expect_nonempty "$CK_CLIP" "the finished upload's clip"
expect "$(json title)" "E2E chunked" "the clip's title is the open's"
expect_empty "$(json existing)" "a new clip's answer carries no existing"
wait_ready "$CK_CLIP" "the chunked clip"
code=$(curl_ -sS -L -o "$(W "$CK_DIR/original")" -D "$HDRW" -w '%{http_code}' -b "$CK_JARW" "$BASE/api/clips/$CK_CLIP/original?download=1")
expect "$code" 200 "download the chunked clip's original"
expect "$(sha256sum "$CK_DIR/original" | cut -c1-64)" "$CK_SUM" "the stored original is the file, byte for byte"
code=$(http GET "$BASE/api/uploads/$CK_ID" -b "$CK_JARW"); expect "$code" 200 "GET the finished session ($(cat "$RESP"))"
expect "$(json clip.id)" "$CK_CLIP" "GET names the finished upload's clip"

# --- (e) the other door: the same file one-shot is the existing clip -------------------------------------------
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$CK_JARW" -H "$E2E_APP_HDR" \
  -F 'metadata={"title":"E2E one-shot"};type=application/json' -F "video=@$(W "$CK_FILE");type=video/mp4" "$BASE/api/clips")
expect "$code" 200 "the one-shot upload of the same file ($(cat "$RESP"))"
expect "$(json id)" "$CK_CLIP" "the one-shot door answers the chunked clip"
expect "$(json existing)" true "the one-shot door's answer says existing"
echo "[e2e]   finish made $CK_CLIP (ready, the original's sha256 is the file's); POST /api/clips of the same file answered it with existing"

# --- the litter this step made, gone --------------------------------------------------------------------------
code=$(http DELETE "$BASE/api/clips/$CK_CLIP" -b "$CK_JARW"); expect_deleted "$code" "$CK_CLIP" "delete the chunked clip"
wait_gone "$CK_CLIP" "the chunked clip"
rm -rf "$CK_DIR"

summary_row CK "$CK_CLIP" "the chunked web upload (§20 erratum 344): a $CK_SIZE-byte file in three $CK_CHUNK-byte chunks — one refused out of order (409 received 0), one repeated (200, unchanged), an early finish 409 incomplete — finished into a ready clip whose original is the file byte for byte; the one-shot POST /api/clips of the same file answered it with existing; a probe session abandoned by DELETE (404 after); clip deleted (account swept by step 17)"
