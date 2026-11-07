# 96-mint.sh — the capture token an operator mints from the SHELL (ROADMAP #48, DESIGN §20 erratum 308).
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 96.
#
# POST /api/auth/tokens needs a browser SESSION, and the two situations a capture token is most often
# wanted in have none: a machine being set up by somebody who is not the account's owner, and a shell on
# a box whose browser cannot reach this instance. `clippet admin tokens mint` is the second door, and
# what this step proves on a live stack is that it is the SAME mint — an ordinary token that the upload
# route accepts, that `tokens list` and `tokens revoke` see, and whose `--sxcu` file is the one the
# settings page downloads, carrying this instance's own base URL.
#
# e2e-schedule: parallel
# e2e-namespace: account=mt prefix=MT_ buckets=-
# e2e-covers: cmd/clippet/admin.go internal/httpapi/sxcu.go internal/httpapi/token_handlers.go
#   It creates its OWN account (`admin users create`, under the name pattern the run's own sweep covers),
#   mints for it, uploads ONE clip with the minted token, and removes the clip and the account again. It
#   reads no shared clip and mutates none, restarts and reconfigures nothing, asserts nothing bucket-wide
#   or instance-wide, and probes no rate limiter: one upload and one deliberate 400, against the pair of
#   upload limiters this run already raises.
#
# Variables are prefixed MT_ so nothing here can clobber a harness global.

step 96 "admin tokens mint: the capture token an operator mints from the shell, and the ShareX file it writes (#48)"

MT_USER="e2e_$(date +%s)_mt"
MT_PASS="e2e-mint-$(date +%s)"
MT_ERR="$E2E_DIR/mint-stderr.txt"
MT_SXCU="$E2E_DIR/mint.sxcu"

# The account this step owns. The password goes in on STDIN, never argv — shell history and `ps` both
# keep an argument (§20 erratum 109).
MT_OUT=$(printf '%s' "$MT_PASS" | dc exec -T app clippet admin users create "$MT_USER" 2>&1) \
  || die "admin users create failed: $MT_OUT"
echo "[e2e]   $MT_OUT"

# --- (a) the mint: stdout is the artefact, stderr is for people -------------------------------------
# `clippet admin tokens mint alice > token.txt` is the whole shape of the verb, so the raw value must be
# on stdout and NOWHERE else — not in the confirmation, not in a log line this program could grow.
MT_TOK=$(dc exec -T app clippet admin tokens mint "$MT_USER" 2>"$MT_ERR" | tr -d '\r') \
  || die "admin tokens mint failed: $(cat "$MT_ERR")"
case $MT_TOK in ck_*) ;; *) die "the mint's stdout is not a token: $(cat "$MT_ERR")" ;; esac
expect "${#MT_TOK}" 46 "the minted token's length"
lacks_secret_f "$MT_ERR" "$MT_TOK" "the raw token must never reach stderr"
has_file_f "$MT_ERR" "$(printf '%s' "$MT_TOK" | cut -c1-11)" "stderr must name the token by its display prefix"
has_file_f "$MT_ERR" "scopes=upload" "the default scope set is the route's own default"
echo "[e2e]   minted a token for $MT_USER from the shell; stdout carried it and stderr did not"

# --- (b) it is a capture credential POST /api/upload accepts ----------------------------------------
# A plain `curl`, not curl_: this is a WRITE, and a repeated write is a second clip (scripts/e2e.sh's
# note on http()).
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -H "Authorization: Bearer $MT_TOK" \
  -F 'title=E2E shell-minted capture' -F 'ttl_days=1' \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/std-720p30.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "POST /api/upload with the shell-minted token ($(cat "$RESP"))"
MT_ID=$(json id); expect_nonempty "$MT_ID" "the shell-minted capture's clip id"
expect_nonempty "$(json url)" "the reply's share link"
echo "[e2e]   the token uploaded clip $MT_ID"

# --- (c) an ORDINARY token: listed and revoked like any other ---------------------------------------
MT_LS=$(dc exec -T app clippet admin tokens list "$MT_USER" | tr -d '\r') || die "admin tokens list failed: $MT_LS"
contains "$MT_LS" "1 live token(s) for $MT_USER" || die "tokens list does not show the minted token: $MT_LS"
contains "$MT_LS" 'scopes=upload' || die "tokens list does not print the scopes: $MT_LS"
case $MT_LS in *"$MT_TOK"*) die "admin tokens list printed the raw token" ;; esac

# --- (d) --sxcu: the file the settings page downloads, with this instance's own base -----------------
dc exec -T app clippet admin tokens mint "$MT_USER" --name 'e2e sharex' --sxcu > "$MT_SXCU.raw" 2>"$MT_ERR" \
  || die "admin tokens mint --sxcu failed: $(cat "$MT_ERR")"
tr -d '\r' < "$MT_SXCU.raw" > "$MT_SXCU"; rm -f "$MT_SXCU.raw"
has_file_f "$MT_SXCU" '"DestinationType": "FileUploader"' "the .sxcu must claim the FILE destination only (§20 erratum 59)"
has_file_f "$MT_SXCU" "\"RequestURL\": \"$BASE/api/upload\"" "the .sxcu must carry this instance's own upload route"
has_file_f "$MT_SXCU" '"FileFormName": "file"' "the .sxcu must name the file part"
lacks_file "$MT_SXCU" 'ThumbnailURL' "the .sxcu must not point a thumbnail at an HTML page (§20 erratum 59)"
lacks_file "$MT_SXCU" 'Arguments' "the .sxcu must send no title argument (§20 erratum 60)"
MT_SXTOK=$(sed -n 's/.*"Authorization": "Bearer \(ck_[^"]*\)".*/\1/p' "$MT_SXCU" | head -1)
expect_nonempty "$MT_SXTOK" "the credential inside the .sxcu"
[ "$MT_SXTOK" != "$MT_TOK" ] || die "the second mint re-issued the first token"
lacks_secret_f "$MT_ERR" "$MT_SXTOK" "the raw token must never reach stderr, --sxcu or not"

# The file is driven by its OWN fields, exactly as ShareX would drive it — and the request is
# deliberately the one that costs nothing: no file part at all. `400 missing_video` is the answer only a
# request that got PAST auth, the upload scope and the CSRF rule can receive, so it proves the
# credential without asking the box to encode a second clip (a 401 or a 403 would have come first).
MT_URL=$(sed -n 's/.*"RequestURL": "\([^"]*\)".*/\1/p' "$MT_SXCU" | head -1)
code=$(http_refused POST "$MT_URL" -H "Authorization: Bearer $MT_SXTOK" -F 'title=E2E sxcu probe')
expect "$code" 400 "an upload driven by the .sxcu's own fields reaches the route ($(cat "$RESP"))"
expect "$(json error)" missing_video "the .sxcu's credential passed auth and the upload scope"
echo "[e2e]   the .sxcu the shell wrote is an importable uploader for $BASE"

# --- (e) revoke: both tokens, and the credential stops at once ---------------------------------------
MT_OUT=$(dc exec -T app clippet admin tokens revoke "$MT_USER" --all | tr -d '\r') || die "admin tokens revoke failed: $MT_OUT"
contains "$MT_OUT" "revoked 2 token(s) for $MT_USER" || die "tokens revoke --all: $MT_OUT"
code=$(get "$BASE/api/auth/me" -H "Authorization: Bearer $MT_TOK")
expect "$code" 401 "the revoked shell-minted token is rejected"
code=$(get "$BASE/api/auth/me" -H "Authorization: Bearer $MT_SXTOK")
expect "$code" 401 "the revoked .sxcu token is rejected"

# --- the litter this step made, gone --------------------------------------------------------------
dc exec -T app clippet admin expire "$MT_ID" --ago 2h >/dev/null || die "could not expire $MT_ID"
wait_gone "$MT_ID" "the shell-minted capture"
MT_OUT=$(dc exec -T app clippet admin users prune "$MT_USER" --older-than 1s --no-clips | tr -d '\r') \
  || die "admin users prune failed: $MT_OUT"
# `pruned 1 user(s)`, never the count alone: the pattern is echoed back in that same line, so
# `contains "$MT_OUT" "$MT_USER"` would pass on a prune that removed nothing.
contains "$MT_OUT" 'pruned 1 user(s)' || die "users prune did not remove the account: $MT_OUT"
rm -f "$MT_SXCU" "$MT_ERR"

summary_row MT "$MT_ID" "admin tokens mint (#48): a capture token minted from the shell with no session — raw value on stdout only, accepted by POST /api/upload, listed and revoked like any other; its --sxcu is the settings page's own file on this instance's base and drives the route by its own fields; clip expired, account pruned"
