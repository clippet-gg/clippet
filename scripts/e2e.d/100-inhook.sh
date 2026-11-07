# 100-inhook.sh — the inbound URL: POST /in/{token} drops a clip on the box (ROADMAP #54 pick 5, DESIGN §20 erratum 322).
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 100.
#
# A tool that can only POST a file to an address gets a URL whose last segment IS the credential: an
# api_tokens row holding the scope `inbound` alone, with a `ci_` secret. What this step proves on a live
# stack: the shell mints it (the URL on stdout, on this instance's own base, and nowhere else); a plain
# `curl -F file=@…` to it is an upload that answers the share link as one line of text, or the upload
# route's JSON when asked; a wrong, revoked or `ck_` value answers exactly what a path with no route
# answers; the secret is no Bearer credential; and the app's log writes the path as /in/{token} and
# never the secret — also for the doubled-slash spelling the root mux answers before any handler.
#
# e2e-schedule: parallel
# e2e-namespace: account=ih prefix=IH_ buckets=-
# e2e-covers: internal/httpapi/inbound.go cmd/clippet/admin.go
#   It creates its OWN account (`admin users create`, under the name pattern the run's own sweep covers),
#   mints for it, uploads TWO clips through the URL and removes them and the account again. It reads no
#   shared clip and mutates none, restarts and reconfigures nothing, asserts nothing bucket-wide or
#   instance-wide (the log check greps for THIS run's own secret only), and probes no rate limiter.
#
# Variables are prefixed IH_ so nothing here can clobber a harness global.

step 100 "the inbound URL (#54): POST /in/{token} drops a clip; a wrong token is a missing route; the secret never reaches the log"

IH_USER="e2e_$(date +%s)_ih"
IH_PASS="e2e-inhook-$(date +%s)"
IH_ERR="$E2E_DIR/inhook-stderr.txt"
IH_NOPE="$E2E_DIR/inhook-nope.json"
IH_CLIP="$(W "$PWD/testclips/std-720p30.mp4")"

# The account this step owns. The password goes in on STDIN, never argv (§20 erratum 109).
IH_OUT=$(printf '%s' "$IH_PASS" | dc exec -T app clippet admin users create "$IH_USER" 2>&1) \
  || die "admin users create failed: $IH_OUT"
echo "[e2e]   $IH_OUT"

# --- (a) the mint: stdout is the URL, on this instance's own base; stderr never carries the secret -------
IH_TOKURL=$(dc exec -T app clippet admin tokens mint "$IH_USER" --scopes inbound --name e2e 2>"$IH_ERR" | tr -d '\r') \
  || die "admin tokens mint --scopes inbound failed: $(cat "$IH_ERR")"
case $IH_TOKURL in "$BASE/in/ci_"*) ;; *) die "the mint's stdout is not an inbound URL on $BASE: $(redact_ "$IH_TOKURL") $(cat "$IH_ERR")" ;; esac
IH_RAW=${IH_TOKURL#"$BASE/in/"}
expect "${#IH_RAW}" 46 "the inbound secret's length"
lacks_secret_f "$IH_ERR" "$IH_RAW" "the secret must never reach stderr"
has_file_f "$IH_ERR" "ci_$(printf '%s' "$IH_RAW" | cut -c4-11)" "stderr must name the row by its ci_ display prefix"
has_file_f "$IH_ERR" "scopes=inbound" "the row holds the inbound scope alone"
echo "[e2e]   minted an inbound URL for $IH_USER from the shell"

# --- (b) a plain curl -F file=@… is an upload; the answer is the share link as one line of text ---------
# A plain `curl`, not curl_: this is a WRITE, and a repeated write is a second clip.
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -F duplicate=allow -F "file=@$IH_CLIP;type=video/mp4" "$IH_TOKURL")
expect "$code" 201 "POST /in/{token} with a file ($(cat "$RESP"))"
contains "$(hdr Content-Type)" "text/plain" || die "the default answer is text/plain, got '$(hdr Content-Type)'"
IH_LINK=$(tr -d '\r\n' < "$RESP")
case $IH_LINK in "$BASE/"*) ;; *) die "the body is not a share link on $BASE: '$IH_LINK'" ;; esac
IH_ID1=${IH_LINK#"$BASE/"}
expect "$(wc -l < "$RESP" | tr -d ' ')" 1 "the body is one line"
code=$(get "$BASE/api/clips/$IH_ID1"); expect "$code" 200 "the clip the inbound URL made"
echo "[e2e]   the inbound URL uploaded clip $IH_ID1 and answered $IH_LINK"

# --- (c) Accept: application/json → /api/upload's answer; the capture fields work ------------------------
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -H 'Accept: application/json' \
  -F 'title=E2E inbound title' -F 'ttl_days=1' -F duplicate=allow -F "file=@$IH_CLIP;type=video/mp4" "$IH_TOKURL")
expect "$code" 201 "POST /in/{token} asking for JSON ($(cat "$RESP"))"
IH_ID2=$(json id); expect_nonempty "$IH_ID2" "the JSON answer's clip id"
expect "$(json url)" "$BASE/$IH_ID2" "the JSON answer's share link"
expect_nonempty "$(json embed_url)" "the JSON answer is /api/upload's UploadDTO"
code=$(get "$BASE/api/clips/$IH_ID2"); expect "$code" 200 "the second clip"
expect "$(json title)" "E2E inbound title" "the capture title field"

# --- (d) a wrong token answers exactly what a path with no route answers -------------------------------
# Every probe is a REFUSAL with no side effect, so http_refused may carry it. The body is a text field
# and no file: whatever a probe ever reached, nothing could be ingested.
code=$(http_refused POST "$BASE/api/nope" -F 'title=e2e miss'); cp "$RESP" "$IH_NOPE"
expect "$code" 404 "POST /api/nope, the answer every miss must equal"
IH_NOPE_CT=$(hdr Content-Type)
ih_miss() {   # ih_miss <path segment> <what>
  local code
  code=$(http_refused POST "$BASE/in/$1" -F 'title=e2e miss')
  expect "$code" 404 "$2: the status of a missing route"
  cmp -s "$RESP" "$IH_NOPE" || die "$2: the body differs from POST /api/nope's: $(cat "$RESP")"
  expect "$(hdr Content-Type)" "$IH_NOPE_CT" "$2: the Content-Type of a missing route"
}
IH_RAND="ci_$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n' | cut -c1-43)"
expect "${#IH_RAND}" 46 "the random probe's length"
ih_miss "$IH_RAND" "a random ci_ value"
ih_miss "ci_short" "a value of the wrong shape"
IH_CKTOK=$(dc exec -T app clippet admin tokens mint "$IH_USER" --name 'e2e ck' 2>/dev/null | tr -d '\r') \
  || die "admin tokens mint (the capture token for the probe) failed"
case $IH_CKTOK in ck_*) ;; *) die "the capture token for the probe is not ck_: $(redact_ "$IH_CKTOK")" ;; esac
ih_miss "$IH_CKTOK" "a capture token in the path"

# --- (e) the secret is no Bearer credential ------------------------------------------------------------
code=$(get "$BASE/api/auth/me" -H "Authorization: Bearer $IH_RAW")
expect "$code" 401 "the inbound secret as a Bearer token"

# --- (f) the log writes /in/{token}, never the secret — also for the doubled slash ---------------------
# `//in/…` is answered by the ROOT mux (its own redirect) before any handler runs: the redaction must not
# depend on reaching one. --path-as-is keeps curl from cleaning the path itself. No file part: whatever
# answers it, nothing is ingested.
code=$(http_refused POST "${BASE}//in/$IH_RAW" --path-as-is -F 'title=e2e doubled slash')
case $code in 301|307|308|400|404) ;; *) die "POST //in/{token} answered $code ($(cat "$RESP"))" ;; esac
IH_LOG="$E2E_DIR/inhook-app.log"
dc logs --no-color --since "$START_TS" app > "$IH_LOG" 2>&1 || die "docker compose logs failed"
lacks_secret_f "$IH_LOG" "$(printf '%s' "$IH_RAW" | cut -c4-)" "the inbound secret must never reach the app log"
has_file_f "$IH_LOG" "/in/{token}" "the app log writes the inbound path redacted"
rm -f "$IH_LOG"
echo "[e2e]   the app log names /in/{token} and never the secret (the doubled slash answered $code)"

# --- (g) revoke: the URL is a missing route at once -----------------------------------------------------
IH_LS=$(dc exec -T app clippet admin tokens list "$IH_USER" | tr -d '\r') || die "admin tokens list failed: $IH_LS"
contains "$IH_LS" "ci_$(printf '%s' "$IH_RAW" | cut -c4-11)" || die "tokens list does not show the ci_ row: $IH_LS"
case $IH_LS in *"$IH_RAW"*) die "admin tokens list printed the secret" ;; esac
IH_TID=$(printf '%s\n' "$IH_LS" | grep -F "ci_$(printf '%s' "$IH_RAW" | cut -c4-11)" | awk '{print $1}' | head -1)
expect_nonempty "$IH_TID" "the inbound row's id"
IH_OUT=$(dc exec -T app clippet admin tokens revoke "$IH_USER" "$IH_TID" | tr -d '\r') || die "admin tokens revoke failed: $IH_OUT"
contains "$IH_OUT" "revoked 1 token(s) for $IH_USER" || die "tokens revoke: $IH_OUT"
ih_miss "$IH_RAW" "the revoked inbound URL"
echo "[e2e]   a random, a malformed, a ck_ and a revoked value each answered POST /api/nope's 404"

# --- the litter this step made, gone --------------------------------------------------------------
for IH_ID in "$IH_ID1" "$IH_ID2"; do
  dc exec -T app clippet admin expire "$IH_ID" --ago 2h >/dev/null || die "could not expire $IH_ID"
  wait_gone "$IH_ID" "an inbound URL's clip"
done
IH_OUT=$(dc exec -T app clippet admin users prune "$IH_USER" --older-than 1s --no-clips | tr -d '\r') \
  || die "admin users prune failed: $IH_OUT"
contains "$IH_OUT" 'pruned 1 user(s)' || die "users prune did not remove the account: $IH_OUT"
rm -f "$IH_ERR" "$IH_NOPE"

summary_row IH "$IH_ID1" "the inbound URL (#54): minted from the shell (the URL on stdout only), a plain curl -F file uploads and answers the link as text, Accept: application/json gives /api/upload's answer; random, malformed, ck_ and revoked values answer POST /api/nope's 404; no Bearer; the log writes /in/{token} even for //in/; clips expired, account pruned"
