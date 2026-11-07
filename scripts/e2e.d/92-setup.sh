# 92-setup.sh — the first-run wizard at /setup (ROADMAP #45, §20 errata 297-299, 309-311): the page's own
# gates, the five total blocks, the STOP callout, the folder behind Cloudflare walking with its risk named (erratum 348), and `Check now` — the identical
# probe `admin doctor` runs, single-flighted, cleaning up after itself.
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 92.
#
# e2e-schedule: parallel
# e2e-namespace: account=so prefix=SU_ buckets=-
# e2e-covers: internal/httpapi/setup.go internal/web/setup.go internal/web/pages.go web/templates/setup.html
#   It creates no clip and mutates nothing on this stack but its own member account (`bundle_account so`,
#   under the pattern the run's sweep covers): the owner's pages are the run's account's when step 3 found it
#   is this stack's owner (E2E_IS_OWNER=1, §20 erratum 349), and the member is refused them. On a stack a
#   person owns the run's account IS the member: it is refused them itself, no `so` is made, and sections
#   2-6 (the owner's page and Check now) stand down with a NOTE. It reads pages, and it presses the ONE
#   button that writes: `Check now`, which puts and removes one small object under
#   `{S3_KEY_PREFIX}doctor/` — deliberately outside `clips/`, so nothing that walks the clip prefixes
#   (`admin orphans`, the sweeper, every `prefix_count` in this suite) can see it, and no bundle's
#   counts can be disturbed by it. The only other bundle that writes under that prefix is 51-admin's
#   `admin doctor`, which is EXCLUSIVE and therefore never concurrent with a lane. The rest of the
#   step happens inside one throwaway `dc run --rm` container with its own DATA_DIR, its own
#   SCRATCH_DIR and its own S3_KEY_PREFIX at the bucket root, which it empties before it exits
#   (the 89-quota pattern).
#
# Variables are prefixed SU_ so nothing here can clobber a harness global.

# ---------------------------------------------------------------------------------------------------
step 92 "the setup wizard (#45): five total blocks, the STOP callout, the folder behind Cloudflare named not refused, and Check now's own probe"

SU_PAGE="$E2E_DIR/setup-page.html"
SU_PAGEW=$(W "$SU_PAGE")

# su_page <query> [curl args…] → status; the body lands in $SU_PAGE. Signed in as the run's own
# account unless a caller says otherwise.
su_page() {
  local q=$1; shift
  curl_ -sS -o "$SU_PAGEW" -D "$HDRW" -w '%{http_code}' -b "$JARW" "$@" "$BASE/setup$q"
}
# su_count <pattern> → how many lines of the fetched page match it.
su_count() { grep -c -- "$1" "$SU_PAGE" | tr -d ' \r'; }

# --- (1) who may open it -------------------------------------------------------------------------
SU_C=$(curl_ -sS -o "$SU_PAGEW" -D "$HDRW" -w '%{http_code}' "$BASE/setup")
expect "$SU_C" 303 "an anonymous GET /setup on a configured instance"
expect "$(hdr Location)" "/login?next=/setup" "…and where it is sent"
# The owner's session reads the page (200); a member's is the owner-only page (403) — the run's own
# account is one or the other, by step 3's E2E_IS_OWNER.
SU_WANT=403; [ "${E2E_IS_OWNER:-0}" = 1 ] && SU_WANT=200
SU_C=$(su_page "")
expect "$SU_C" "$SU_WANT" "a signed-in GET /setup (the run's account, owner=${E2E_IS_OWNER:-0})"
expect "$(hdr Cache-Control)" "private, no-store" "the setup page's cache headers"
echo "[e2e]   the door: anonymous → /login?next=/setup, a session → $SU_WANT private,no-store"

# --- (1b) the owner's page (§20 erratum 349) ------------------------------------------------------
# A member gets the heading and the one sentence, 403, on the page and on the steps view; its /settings
# Storage panel is the head and the same sentence with no figure; and its Check now is refused before any
# probe. The member is a second account when the run's account owns this stack, and the run's account
# itself when a person does (step 3) — a better pin there: the refusal a real member meets.
SU_OWNER_LINE='Only the account that set up this Clippet can see this.'
if [ "${E2E_IS_OWNER:-0}" = 1 ]; then
  bundle_account so; SU_MJARW=$BA_JARW
else
  SU_MJARW=$JARW
fi
SU_C=$(su_page "" -b "$SU_MJARW"); expect "$SU_C" 403 "a member's GET /setup"
expect "$(hdr Cache-Control)" "private, no-store" "the member's setup page's cache headers"
has_file_f "$SU_PAGE" "$SU_OWNER_LINE" "the member's setup page lacks the owner-only sentence"
has_file_f "$SU_PAGE" 'data-setup-owner-only' "the member's setup page is not the owner-only view"
for SU_NOT in 'data-setup-backend' 'Pick a storage backend' '<h2>Now</h2>' 'data-setup-check'; do
  lacks_file_f "$SU_PAGE" "$SU_NOT" "the member's setup page shows the owner's page"
done
SU_C=$(su_page "?view=steps&store=b2" -b "$SU_MJARW"); expect "$SU_C" 403 "a member's steps view"
has_file_f "$SU_PAGE" "$SU_OWNER_LINE" "the member's steps view lacks the owner-only sentence"
SU_C=$(curl_ -sS -o "$SU_PAGEW" -D "$HDRW" -w '%{http_code}' -b "$SU_MJARW" "$BASE/settings"); expect "$SU_C" 200 "a member's /settings"
has_file_f "$SU_PAGE" 'data-storage-owner-only' "the member's Storage panel is not the owner-only view"
has_file_f "$SU_PAGE" "$SU_OWNER_LINE" "the member's Storage panel lacks the owner-only sentence"
for SU_NOT in '>stored<' '>served this month<' 'href="/setup"'; do
  lacks_file_f "$SU_PAGE" "$SU_NOT" "the member's Storage panel shows the owner's figures"
done
SU_C=$(http POST "$BASE/api/setup/check" -b "$SU_MJARW"); expect "$SU_C" 403 "a member's Check now ($(cat "$RESP"))"
expect "$(json error)" not_owner "a member's Check now → error"
if [ "${E2E_IS_OWNER:-0}" = 1 ]; then
  SU_C=$(curl_ -sS -o "$SU_PAGEW" -D "$HDRW" -w '%{http_code}' -b "$JARW" "$BASE/settings"); expect "$SU_C" 200 "the owner's /settings"
  has_file_f "$SU_PAGE" '>stored<' "the owner's Storage panel lost its figures"
  lacks_file_f "$SU_PAGE" 'data-storage-owner-only' "the owner's Storage panel is the member's view"
  SU_C=$(su_page ""); expect "$SU_C" 200 "the owner's GET /setup again (section 2 reads it)"
  echo "[e2e]   the owner's page: a member gets 403 and the one sentence on /setup and its steps view, the Storage panel's sentence and no figure, and Check now 403 not_owner"
else
  echo "[e2e]   the owner's page: the run's account is a member here, and gets 403 and the one sentence on /setup and its steps view, the Storage panel's sentence and no figure, and Check now 403 not_owner"
  echo "[e2e]   NOTE: sections 2-6 (the owner's page and Check now) stand down: this stack's owner is a person, not the run's account (§20 erratum 349)"
fi

# Sections 2-6 read the owner's page and press the owner's Check now: the run's account must own this
# stack (E2E_IS_OWNER, step 3). The block is not re-indented, so its history stays readable.
if [ "${E2E_IS_OWNER:-0}" = 1 ]; then
# --- (2) what the page says about this instance ---------------------------------------------------
has_file "$SU_PAGE" 'data-setup-backend="s3"' "the page does not carry this stack's backend"
has_file "$SU_PAGE" 'Where do clips live?' "the page's own heading"
# The panels read as a numbered sequence (§20 errata 310, 311): the facts panel is `Now` and unnumbered —
# it is not a step — with a `clips` row; then `1 · Pick a storage backend` (five radio cards; the field is
# still `store`); and before any pick the muted heads `2 · Set up` and `3 · Apply` alone, so the route
# is visible before the first step.
has_file "$SU_PAGE" '<h2>Now</h2>' "the facts panel is not headed Now"
has_file_f "$SU_PAGE" '<span class="rl">clips</span>' "the Now panel has no clips row"
has_file_f "$SU_PAGE" '<h2><span class="n">1</span> Pick a storage backend</h2>' "the chooser is not step 1, 'Pick a storage backend'"
has_file_f "$SU_PAGE" '<div class="panel later" id="walk-later" data-setup-later>' "step 2's muted head is missing before a pick"
has_file_f "$SU_PAGE" '<h2><span class="n">2</span> Set up</h2>' "step 2's muted head does not read 'Set up'"
has_file_f "$SU_PAGE" '<h2><span class="n">3</span> Apply</h2>' "step 3's muted head does not read 'Apply'"
lacks_file_f "$SU_PAGE" '>Pick a store<' "the old 'Pick a store' head is still on the page"
lacks_file_f "$SU_PAGE" 'Current store' "the old 'Current store' head is still on the page"
lacks_file_f "$SU_PAGE" 'Storage backend</' "the old 'Storage backend' head or label is still on the page"
lacks_file_f "$SU_PAGE" '<select id="store"' "the storage backend is still a <select>"
lacks_file_f "$SU_PAGE" 'data-setup-walk=' "a walk rendered before any pick"
# The five choices are five radio cards shown at once (§20 erratum 311), each its name and its one fact in
# the state cell — never a suffix on the name. The exposure is a closed set of three, so it is a `.seg`
# of radios and never a <select> (web/STYLE.md, §20 erratum 309); with no `via` in the query the page
# GUESSED it, and the Reached row's state cell says so in one word. No hint under either control: the
# one rule is said in the walk it decides.
expect "$(su_count 'type="radio" name="store"')" 5 "the chooser has $(su_count 'type="radio" name="store"') storage backend radios, want 5"
has_file_f "$SU_PAGE" '<span class="name">Cloudflare R2</span><span class="rs">10 GB free · egress free</span>' "the R2 card's name and fact"
has_file_f "$SU_PAGE" '<span class="name">Backblaze B2</span><span class="rs">10 GB free · no card</span>' "the B2 card's name and fact"
lacks_file_f "$SU_PAGE" 'Cloudflare R2 — ' "a card's name still carries its fact as a suffix"
expect "$(su_count 'type="radio" name="via"')" 3 "the Reached seg has $(su_count 'type="radio" name="via"') radios, want 3"
has_file "$SU_PAGE" '<span class="rs">guessed</span>' "the guessed exposure is not said in the Reached row's state cell"
lacks_file_f "$SU_PAGE" '<select id="via"' "the exposure is still a <select>"
lacks_file_f "$SU_PAGE" 'walks read' "the old walks-read row is still on the chooser"
# The backend row says which of TWO shapes this instance is on — a directory, or a bucket somebody
# gave it (since §20 erratum 345 there is no bundled server) — and this bundle runs on both (a lane
# stack's harness bucket is RustFS, the reference stack is on R2, and 88-fsstore brings a directory). It is READ here rather than asserted,
# because the diagonal below depends on it and a hard-coded word would make this bundle pass on one
# stack and fail on another for no product reason.
SU_CUR=
grep -qF 'a directory (fs)' "$SU_PAGE" && SU_CUR=fs
[ -n "$SU_CUR" ] || { grep -qF 'a bucket (s3)' "$SU_PAGE" && SU_CUR=bucket; }
expect_nonempty "$SU_CUR" "the backend row names neither of the two shapes this page knows"
echo "[e2e]   this stack's store, as the page classifies it: $SU_CUR"
for SU_O in fs r2 b2 s3 other; do
  has_file "$SU_PAGE" "value=\"$SU_O\"" "the chooser is missing the '$SU_O' option"
done

# The no-URL law at RUNTIME (§20 erratum 298's two placeholder grammars). check.sh greps the SOURCE;
# this greps what was actually rendered, with this instance's own base taken out first — the only
# absolute URL any page of this app is allowed to carry.
SU_STRIPPED="$E2E_DIR/setup-stripped.txt"
sed "s|$BASE||g" "$SU_PAGE" > "$SU_STRIPPED"
if grep -qE 'https?://' "$SU_STRIPPED"; then
  die "the rendered setup page carries a URL other than this instance's own: $(grep -oE 'https?://[^\"<[:space:]]*' "$SU_STRIPPED" | head -3 | tr '\n' ' ')"
fi
# …and no secret, no bucket name, no endpoint host (C5: the page names none of them).
lacks_file_f "$SU_PAGE" 'clippet-dev-secret' "the page carries the harness's published bucket password"
lacks_file_f "$SU_PAGE" '//s3:9000' "the page carries this stack's endpoint host"
SU_KEY=$(envfile S3_ACCESS_KEY); SU_KEY=${SU_KEY:-$(envfile MINIO_ROOT_USER)}
if [ -n "$SU_KEY" ] && [ ${#SU_KEY} -ge 8 ]; then
  lacks_file_f "$SU_PAGE" "$SU_KEY" "the page carries this stack's access key id"
fi
echo "[e2e]   the page: four state rows, five radio cards, two muted heads, no URL but this origin's, no bucket and no credential"

# --- (3) the five walks, and the total-block rule at runtime ---------------------------------------
for SU_O in fs r2 b2 s3 other; do
  SU_C=$(su_page "?store=$SU_O&via=other"); expect "$SU_C" 200 "GET /setup?store=$SU_O"
  expect "$(su_count '^COMPOSE_FILE=')" 1 "?store=$SU_O renders $(su_count '^COMPOSE_FILE=') COMPOSE_FILE lines, want exactly 1"
  expect "$(su_count '^STORAGE_BACKEND=')" 1 "?store=$SU_O renders $(su_count '^STORAGE_BACKEND=') STORAGE_BACKEND lines, want exactly 1"
  expect "$(su_count '^MEDIA_DELIVERY=')" 1 "?store=$SU_O renders $(su_count '^MEDIA_DELIVERY=') MEDIA_DELIVERY lines, want exactly 1"
  # §20 erratum 297: no PASTEABLE data-moving command, in either direction. The STOP line names the
  # verb and DEPLOY §32.3 so an operator can find them; a form somebody could copy is what it must
  # never print, because the documented one carries a bind whose omission throws the copy away.
  lacks_file_f "$SU_PAGE" 'storage migrate --to' "?store=$SU_O prints a runnable migrate command"
done

SU_C=$(su_page "?store=b2&via=other")
has_file_f "$SU_PAGE" 'COMPOSE_FILE=compose.yaml:compose.nobucket.yaml' "the B2 block's COMPOSE_FILE"
has_file_f "$SU_PAGE" 'STORAGE_BACKEND=s3' "the B2 block's STORAGE_BACKEND"
has_file_f "$SU_PAGE" 'STORAGE_PRICING=b2' "the B2 block's pricing preset"
has_file_f "$SU_PAGE" 'MEDIA_DELIVERY=redirect' "the B2 block's delivery mode"
lacks_file_f "$SU_PAGE" 'TRUSTED_PROXIES=' "the B2 block reached directly names a proxy to trust (only a tunnel's block does, DEPLOY §4)"
has_file_f "$SU_PAGE" 'no card' "the B2 option's own words"
# BOTH Backblaze console vocabularies, because both are current and each of two readings of the
# rev-1 walk concluded the other was stale (§20 erratum 298).
has_file_f "$SU_PAGE" 'Add a New Application Key' "the standard console's label"
has_file_f "$SU_PAGE" 'Create App Key' "the enterprise console's label"
expect "$(su_count '<li>')" 7 "the B2 walk has $(su_count '<li>') steps, want 7"
has_file_f "$SU_PAGE" '<h2><span class="n">2</span> Backblaze B2</h2>' "the B2 walk is not step 2, headed by the backend's own name"
has_file_f "$SU_PAGE" '<h2><span class="n">3</span> Apply</h2>' "Apply is not step 3"
lacks_file_f "$SU_PAGE" 'id="walk-later"' "step 2's muted head is still on the page beside the walk"
lacks_file_f "$SU_PAGE" 'id="apply-later"' "step 3's muted head is still on the page beside Apply"
# The chosen card is checked and still says its fact (the verdict is `on now`, never the pick).
has_file_f "$SU_PAGE" 'value="b2" checked><span class="dot"></span><span class="name">Backblaze B2</span><span class="rs">10 GB free · no card</span>' "the chosen card is not checked with its fact"
# The list runs full width in the walk's body — no label column, no `steps` label (§20 errata 310,
# 311) — and the date somebody last read those labels is ONCE, in the line under it (§20 erratum
# 309) — and no longer on every step and on the chooser besides.
lacks_file_f "$SU_PAGE" '<span class="rl">steps</span>' "the walk's list still carries a 'steps' label"
has_file_f "$SU_PAGE" '<div class="walk-body">' "the walk has no full-width body"
has_file_f "$SU_PAGE" '<div class="walk-meta"><span class="rs">labels read 2026-09-13</span></div>' "the dated labels line under the list"
has_file_f "$SU_PAGE" '<div class="env-head"><span class="rl">your .env</span>' "the block's 'your .env' head"
expect "$(su_count 'labels read ')" 1 "the walk prints the date $(su_count 'labels read ') times, want once"
lacks_file_f "$SU_PAGE" '(read 2026-09-13)' "a step hint still carries its own date"
# …and `via` chosen: the Reached state cell is empty.
lacks_file_f "$SU_PAGE" '<span class="rs">guessed</span>' "the Reached row still says guessed after via= was chosen"

SU_C=$(su_page "?store=fs&via=other")
has_file_f "$SU_PAGE" 'COMPOSE_FILE=compose.yaml:compose.fs.yaml' "the local block's COMPOSE_FILE"
has_file_f "$SU_PAGE" 'STORAGE_BACKEND=fs' "the local block's STORAGE_BACKEND"
has_file_f "$SU_PAGE" 'STORAGE_PRICING=local' "the local block's pricing preset"
has_file_f "$SU_PAGE" 'Videos folder' "the local walk's own default directory"

# (The bundled-MinIO walk and its going-back assertions left with that server: §20 erratum 345.)

# The one walk whose MEDIA_DELIVERY is not a function of the target alone (§20 erratum 298 (f)): "any
# other S3-compatible bucket" is the only target that may be a NAS or a LAN address, which a viewer's
# browser cannot follow a redirect to, so it proxies — and asks for a redirect only on the exposure
# whose terms require one.
SU_C=$(su_page "?store=other&via=other")
has_file_f "$SU_PAGE" 'MEDIA_DELIVERY=proxy' "the self-hosted-bucket block's delivery mode off Cloudflare"
SU_C=$(su_page "?store=other" -H 'CF-Ray: 8f0c1d2e3f4a5b6c-LHR')
has_file_f "$SU_PAGE" 'MEDIA_DELIVERY=redirect' "the self-hosted-bucket block's delivery mode ON Cloudflare"
lacks_file_f "$SU_PAGE" 'MEDIA_DELIVERY=proxy' "the block on a tunnel still offered proxy"
has_file_f "$SU_PAGE" 'a LAN address or a NAS will not work' "…and the line that says the endpoint must then be public"

SU_C=$(su_page "?store=zzz"); expect "$SU_C" 200 "an unknown ?store="
has_file_f "$SU_PAGE" 'That is not one of the five.' "an unknown ?store= does not say so"
lacks_file_f "$SU_PAGE" 'data-setup-walk=' "an unknown ?store= rendered a panel"
echo "[e2e]   walks: one COMPOSE_FILE, one STORAGE_BACKEND and one MEDIA_DELIVERY in every block; no migrate command; an unknown store is the chooser"

# --- (3b) the steps-only view (§20 erratum 335): ?view=steps&store=<id> --------------------------
# The head, its caveat, the numbered steps with their dated hints, `labels read` — and nothing else:
# no block, no command, no STOP, no chooser. The desktop app's `B2 steps` opens exactly this.
SU_C=$(su_page "?view=steps&store=b2"); expect "$SU_C" 200 "the steps-only view"
has_file_f "$SU_PAGE" 'data-setup-steps="b2"' "the steps view does not name its store"
has_file_f "$SU_PAGE" '<h2>Backblaze B2</h2><span class="hint">Egress free up to three times what you store.</span>' "the steps view's head and its caveat"
has_file_f "$SU_PAGE" '<ol class="walk">' "the steps view has no numbered list"
has_file_f "$SU_PAGE" 'Add a New Application Key' "the steps view lost a dated hint"
has_file_f "$SU_PAGE" '<div class="walk-meta"><span class="rs">labels read 2026-09-13</span></div>' "the steps view's labels line"
for SU_NOT in 'pre class="env' 'data-setup-cmd' 'data-setup-stop' 'name="store"' 'data-setup-refusal' \
              'data-setup-check' 'data-setup-block' 'Where do clips live?' '<h2>Now</h2>' 'docker compose'; do
  lacks_file_f "$SU_PAGE" "$SU_NOT" "the steps view carries more than the steps"
done
SU_C=$(su_page "?view=steps&store=zzz"); expect "$SU_C" 200 "the steps view with no valid store"
has_file_f "$SU_PAGE" 'Where do clips live?' "the steps view with no valid store is not the ordinary page"
lacks_file_f "$SU_PAGE" 'data-setup-steps' "the steps view rendered for an unknown store"
SU_C=$(curl_ -sS -o "$SU_PAGEW" -D "$HDRW" -w '%{http_code}' "$BASE/setup?view=steps&store=b2")
expect "$SU_C" 303 "an anonymous steps view on a configured instance"
echo "[e2e]   steps view: head, caveat, steps, labels read — no block, no command, no STOP, no chooser; the page's own gate"

# --- (4) the STOP line, and the diagonal -----------------------------------------------------------
# This stack holds the run's clips, so a target that is not what it is on is a real move. The target
# is chosen from what the page just said this instance is, so the assertion is the same claim on
# every stack. The STOP is a callout, first thing in the walk's body (§20 erratum 311): its bold lead
# is the fact, the verb is set in <code>, and it is neither a row nor a red line in the head.
SU_OTHER=fs; [ "$SU_CUR" = fs ] && SU_OTHER=r2
SU_C=$(su_page "?store=$SU_OTHER&via=other")
has_file_f "$SU_PAGE" '<div class="callout" data-setup-stop><b>' "the STOP callout does not render with clips present and a different target ($SU_CUR → $SU_OTHER)"
lacks_file_f "$SU_PAGE" '<span class="rs danger">stop</span>' "the STOP still carries a red 'stop' state cell"
lacks_file_f "$SU_PAGE" 'hint danger' "the STOP is still a red line in the panel head"
has_file_f "$SU_PAGE" '32.3' "the STOP callout does not name the section that DOES move clips"
# The count is this stack's own (one clip under E2E_ONLY, many on a full run), so the lead is pinned by
# its shape — `N clips stay where they are.` / `1 clip stays where it is.` — and the rest by the words
# both forms share.
has_file "$SU_PAGE" '<b>[0-9][0-9]* clips\{0,1\} stays\{0,1\} where \(they are\|it is\)\.</b> Switching does not move' "off the diagonal, the STOP's bold lead does not say the clips stay put"
has_file_f "$SU_PAGE" 'first: <code>admin storage migrate</code> (DEPLOY.md §32.3).' "the STOP callout does not name the verb an operator has to go and read"
lacks_file_f "$SU_PAGE" 'docker compose run --rm --no-deps -v' "the STOP callout printed a migrate command"
lacks_file_f "$SU_PAGE" 'storage migrate --to' "the STOP callout printed a runnable migrate command"
# …and the diagonal, where one exists: the option this instance is already on renders no STOP and
# says so in its own state cell. On an external bucket there is no exact match among the four bucket
# options — the page prints no endpoint host and could not tell R2 from B2 if it did — so the STOP
# there is the CONDITIONAL sentence instead, which is what is asserted.
if [ "$SU_CUR" = bucket ]; then
  SU_C=$(su_page "?store=r2&via=other")
  has_file_f "$SU_PAGE" 'If the new bucket is a different one, move ' "on a bucket, another bucket must get the conditional STOP rather than silence"
  has_file_f "$SU_PAGE" 'This page cannot tell whether it is.' "…and say that the page cannot tell"
  echo "[e2e]   stop: bucket → bucket gets the conditional sentence, because the page cannot see which bucket this is"
else
  SU_C=$(su_page "?store=$SU_CUR&via=other")
  lacks_file_f "$SU_PAGE" 'data-setup-stop' "the diagonal ($SU_CUR) still rendered a STOP line"
  has_file_f "$SU_PAGE" '<span class="rs">on now</span>' "the diagonal does not say 'on now' on its card"
  echo "[e2e]   stop: the diagonal ($SU_CUR) renders no STOP and says 'on now' on its card"
fi
echo "[e2e]   stop: a store change with clips present names DEPLOY §32.3 and the verb in words, and prints no command"

# --- (5) the folder behind Cloudflare walks, with its risk named (§20 erratum 348 (b)) --------------
# The owner's verdict of 2026-10-14 withdrew erratum 299's refusal: never refuse, name the risk. A request
# that came through Cloudflare carries CF-Ray; on one, the folder option WALKS — its card keeps its own
# fact, the block is the folder block plus the tunnel's TRUSTED_PROXIES line and still `proxy`, and the
# desktop app's own folder-card words are the walk's closing line.
SU_C=$(su_page "?store=fs" -H 'CF-Ray: 8f0c1d2e3f4a5b6c-LHR')
expect "$SU_C" 200 "the folder behind Cloudflare"
has_file_f "$SU_PAGE" 'data-setup-walk="fs"' "the folder behind Cloudflare does not walk"
has_file_f "$SU_PAGE" 'COMPOSE_FILE=compose.yaml:compose.fs.yaml' "the folder block behind Cloudflare"
has_file_f "$SU_PAGE" 'MEDIA_DELIVERY=proxy' "the folder block behind Cloudflare is not proxy"
expect "$(su_count '^TRUSTED_PROXIES=172\.16\.0\.0/12$')" 1 "the folder walk behind Cloudflare renders $(su_count '^TRUSTED_PROXIES=172\.16\.0\.0/12$') TRUSTED_PROXIES lines, want exactly 1"
has_file_f "$SU_PAGE" 'Their terms reserve video for a bucket' "the folder walk behind Cloudflare does not name the risk"
has_file_f "$SU_PAGE" 'value="fs" checked><span class="dot"></span><span class="name">A folder or NAS on this machine</span><span class="rs">free · machine must be on</span>' "the folder card behind Cloudflare lost its own fact"
lacks_file_f "$SU_PAGE" 'data-setup-refusal' "the folder behind Cloudflare is still refused"
lacks_file_f "$SU_PAGE" 'rs danger' "a card still reads a red verdict"
has_file_f "$SU_PAGE" 'id="apply"' "the folder walk behind Cloudflare has no Apply"
# A bucket behind the same tunnel walks exactly as it always did…
SU_C=$(su_page "?store=r2" -H 'CF-Ray: 8f0c1d2e3f4a5b6c-LHR')
has_file_f "$SU_PAGE" 'COMPOSE_FILE=compose.yaml:compose.nobucket.yaml' "the R2 walk behind Cloudflare"
# …with the one line the tunnel decides (DEPLOY §4): every viewer arrives from the tunnel's one address.
expect "$(su_count '^TRUSTED_PROXIES=172\.16\.0\.0/12$')" 1 "the R2 walk behind Cloudflare renders $(su_count '^TRUSTED_PROXIES=172\.16\.0\.0/12$') TRUSTED_PROXIES lines, want exactly 1"
lacks_file_f "$SU_PAGE" 'Their terms reserve video' "the R2 walk behind Cloudflare carries the folder's risk line"
# …and the select wins over the header.
SU_C=$(su_page "?store=fs&via=other" -H 'CF-Ray: 8f0c1d2e3f4a5b6c-LHR')
lacks_file_f "$SU_PAGE" 'Their terms reserve video' "the Reached seg did not override the header"
lacks_file_f "$SU_PAGE" 'TRUSTED_PROXIES=' "the folder walk reached directly still names the tunnel's proxy"
echo "[e2e]   cloudflare: the folder walks with its risk named (never refused), a bucket walks without it, and the seg overrides the guess"

# --- (6) Check now: the same probe, single-flighted, cleaning up after itself ----------------------
SU_C=$(http POST "$BASE/api/setup/check"); expect "$SU_C" 401 "an anonymous Check now"
SU_T0=$SECONDS
SU_C=$(http POST "$BASE/api/setup/check" -b "$JARW"); expect "$SU_C" 200 "a signed-in Check now ($(cat "$RESP"))"
expect "$(json ok)" true "Check now against this stack's own store"
SU_STATE=$(json state)
case "$SU_STATE" in
  ok|unreachable|timeout|denied|"bucket missing"|unavailable) ;;
  *) die "Check now's state '$SU_STATE' is outside the closed vocabulary" ;;
esac
expect "$SU_STATE" ok "the state of a store that answered"
SU_AT=$(json checked_at); expect_nonempty "$SU_AT" "Check now's checked_at"
# Two presses are ONE probe: a cache, not a limiter (the owner's no-new-rate-limit rule). The claim is about
# two presses INSIDE the ten-second window, and whether they were is judged AFTER the presses, on the
# harness's own clock: a pair that straddled the window (a loaded box, an edge in the path) is a pair
# that proves nothing about the cache, so it is re-taken back to back (bounded) instead of failing —
# a pair that was inside the window and still re-probed fails as before.
SU_PAIR=0
while :; do
  SU_P0=$SECONDS
  SU_C=$(http POST "$BASE/api/setup/check" -b "$JARW"); expect "$SU_C" 200 "a second Check now"
  SU_P1=$SECONDS
  SU_AT2=$(json checked_at)
  [ "$SU_AT2" = "$SU_AT" ] && break
  SU_PAIR=$((SU_PAIR + 1))
  # A press that took 8 s or more (or follows a first one that long ago) cannot be held to the window.
  [ "$SU_PAIR" -lt 3 ] && [ $((SU_P1 - SU_P0)) -ge 3 -o $((SU_P0 - SU_T0)) -ge 8 ]     || die "two presses inside ten seconds re-probed the store: '$SU_AT' then '$SU_AT2' (press took $((SU_P1 - SU_P0))s, $((SU_P0 - SU_T0))s after the first)"
  echo "[e2e]   NOTE: the two Check-now presses straddled the ten-second window; taking the pair again"
  SU_T0=$SECONDS
  SU_C=$(http POST "$BASE/api/setup/check" -b "$JARW"); expect "$SU_C" 200 "Check now (pair re-taken)"
  SU_AT=$(json checked_at)
done
echo "[e2e]   check now: 401 anonymous, '$SU_STATE' signed in, and a second press inside the window is the same probe"

# …and the press WROTE its outcome into the shared last-ping cache (§20 erratum 297 (i)), which is
# what makes panel one's `last check` mean the last check. Nothing else takes a second reading on a
# running instance: serve.go's boot loop returns for good after its first success and the compose
# healthcheck polls /healthz, which only reads. The minute is the page's own resolution.
SU_MIN=$(printf '%s' "$SU_AT" | cut -c1-16 | tr 'T' ' ')
SU_C=$(su_page ""); expect "$SU_C" 200 "the setup page after a Check now"
# (The cell is the bare date since ec36d03 cut "the store was asked at" — a pin that commit left stale.)
has_file_f "$SU_PAGE" "<span class=\"rc\">$SU_MIN UTC</span>" "panel one's 'last check' row did not follow Check now's own probe"
has_file_f "$SU_PAGE" "data-setup-state>$SU_STATE<" "panel one's state cell does not agree with the probe that just ran"
echo "[e2e]   …and panel one now reads that probe: state '$SU_STATE', last check $SU_MIN UTC"

# …and it left nothing behind. The probe object lives under `{S3_KEY_PREFIX}doctor/`, which is why
# `admin orphans` and every prefix count in this suite are blind to it — and why this is the only
# place that can prove it is gone.
if store_has_bucket; then
  SU_LEFT=$(store_obj --prefix "${SM_PREFIX}doctor/" --count 2>/dev/null || echo 0)
  SU_LEFT=$(printf '%s' "$SU_LEFT" | tr -d ' \r\n')
  expect "${SU_LEFT:-0}" 0 "Check now left $SU_LEFT object(s) under the doctor prefix"
  echo "[e2e]   …and the doctor/ prefix is empty: the probe removed its own object"
else
  echo "[e2e]   NOTE: this stack keeps its media in a directory, so there is no bucket prefix to check"
fi

fi   # E2E_IS_OWNER: sections 2-6

# --- (7) the R2 walk's block, booted in a throwaway container -------------------------------------
# What a block CAN be proven to do, from outside: boot. The placeholders are filled with THIS
# stack's own bucket (the derivation 88-fsstore.sh's migrate probe makes), so what is under test is
# the block's set of variables and their values — including MEDIA_DELIVERY=redirect, which is the
# line the R2 walk exists to set and which the shipped default does not carry. The container brings
# its own DATA_DIR, its own SCRATCH_DIR (README's third convention: a private database over the LIVE
# /scratch has its boot janitor delete an in-flight upload's working directory) and its own
# S3_KEY_PREFIX at the bucket ROOT, which it empties before it exits.
#
# It also runs the second half of the brief's proof: a WRONG bucket must fail the check with the
# doctor's own words, which is the same store.Probe underneath (§20 erratum 297).
SU_PREFIX="e2e-setup-$(date +%s)/"
SU_OUT="$E2E_DIR/setup-probe.txt"
su_rm_prefix() {
  if ! store_has_bucket; then return 0; fi
  store_rm_prefix_ok_ "${SU_PREFIX:-}" || return 0
  ( store_obj rm --prefix "$SU_PREFIX" >/dev/null ) 2>/dev/null \
    || echo "[e2e]   NOTE: could not remove the setup probe's prefix $SU_PREFIX (its own objects, outside the stack's key prefix)"
}
trap 'su_rm_prefix' EXIT

SU_SH="$E2E_DIR/setup-probe.sh"
cat > "$SU_SH" <<'SETUP_PROBE'
set -e
mkdir -p /tmp/setup /tmp/setup-scratch

# (a) the R2 walk's block, with this stack's own bucket in the placeholders. MEDIA_DELIVERY=redirect
#     is the block's own line; PUBLIC_BASE_URL is the container's loopback, so the plain-http presign
#     origin under it is not the mixed-content refusal (scripts/e2e.d/README.md).
clippet migrate >/tmp/migrate.log 2>&1
echo "migrated=$(tail -1 /tmp/migrate.log | tr -d "\r")"
clippet admin doctor > /tmp/doctor.txt 2>&1 || true
echo "delivery=$(grep -c "MEDIA_DELIVERY=redirect:" /tmp/doctor.txt | tr -d " ")"
echo "bucketrow=$(grep "^bucket" /tmp/doctor.txt | head -1 | tr -d "\r")"
clippet serve >/tmp/serve.log 2>&1 &
SSRV=$!
i=0
while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
echo "ready=$(curl -sS -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/healthz)"
echo "readyz=$(curl -sS -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/readyz)"
# The wizard answers on a brand-new instance with no account at all — a first-run wizard you have to
# log in to see is not one (§20 erratum 297).
echo "firstrun=$(curl -sS -o /tmp/setup.html -w "%{http_code}" http://127.0.0.1:8080/setup)"
echo "firstrunhead=$(grep -c "Where do clips live?" /tmp/setup.html | tr -d " ")"
echo "firstruncheck=$(grep -c "data-setup-check>" /tmp/setup.html | tr -d " ")"
kill $SSRV 2>/dev/null || true
wait $SSRV 2>/dev/null || true

# (b) a WRONG bucket, through the doctor — the same store.Probe the page's Check now runs, so the
#     words below are the words a failing check is classified by.
WRONG=$(S3_BUCKET=no-such-bucket-$(date +%s) clippet admin doctor 2>&1 || true)
echo "wrongrow=$(printf "%s" "$WRONG" | grep "^bucket" | head -1 | tr -d "\r")"

# (c) the "any other S3-compatible bucket" walk's MEDIA_DELIVERY line, against the endpoint shape
#     that walk names FIRST: MinIO on the NAS — a plain-http origin on somebody's own network —
#     under an https instance. The endpoint is spelled HERE rather than taken from the stack's own:
#     a lane stack's bundled MinIO is that shape by accident, the reference stack's R2 is https and
#     would boot either way (the FOURTEENTH GATE on clippet.gg found this). The refusal is the config
#     loader's, before any network call, so the host need not exist. The two runs below differ in
#     one line and nothing else. proxy boots; redirect is a boot refusal naming the variable, because
#     a redirect sends the VIEWER'S browser to that origin (§20 erratum 298 (f)).
OTHERPROXY=$(S3_ENDPOINT=http://nas.example.test:9000 PUBLIC_BASE_URL=https://clips.example.test MEDIA_DELIVERY=proxy clippet admin doctor 2>&1 || true)
echo "otherproxy=$(printf "%s" "$OTHERPROXY" | grep -c "MEDIA_PRESIGN_ENDPOINT" | tr -d " ")"
OTHERREDIR=$(S3_ENDPOINT=http://nas.example.test:9000 PUBLIC_BASE_URL=https://clips.example.test MEDIA_DELIVERY=redirect clippet admin doctor 2>&1 || true)
echo "otherredir=$(printf "%s" "$OTHERREDIR" | grep -c "MEDIA_PRESIGN_ENDPOINT: is a plain-http origin" | tr -d " ")"
echo "logerrors=$(grep -c -E "panic|level=ERROR" /tmp/serve.log | tr -d " ")"
echo "done=1"
SETUP_PROBE

SU_RC=0
dc run --rm -T --no-deps \
  -e "S3_KEY_PREFIX=$SU_PREFIX" \
  -e MEDIA_DELIVERY=redirect -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e STORAGE_PRICING=r2 \
  -e DATA_DIR=/tmp/setup -e SCRATCH_DIR=/tmp/setup-scratch \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 \
  -e REGISTRATION=first -e SWEEP_INTERVAL=30s \
  -e DISCORD_APP_ID= -e DISCORD_PUBLIC_KEY= -e DISCORD_BOT_TOKEN= \
  --entrypoint sh app -c "$(cat "$SU_SH")" > "$SU_OUT" 2>&1 || SU_RC=$?
head -40 "$SU_OUT" | sed -n 's/^/[e2e]   setup-probe: /p'
[ "$SU_RC" = 0 ] || die "the setup probe container failed (exit $SU_RC):
$(cat "$SU_OUT")"
has_file "$SU_OUT" '^done=1' "the setup probe did not run to the end"

su_get() { sed -n "s/^$1=//p" "$SU_OUT" | head -1; }
expect "$(su_get delivery)" 1 "the block's MEDIA_DELIVERY=redirect did not reach the instance it booted"
expect "$(su_get ready)" 200 "the R2 walk's block booted an instance ($(su_get ready))"
expect "$(su_get readyz)" 200 "…and its live storage ping ($(su_get readyz))"
contains "$(su_get bucketrow)" 'reachable, writable and cleanable' \
  || die "the doctor's bucket row on the block's own configuration: $(su_get bucketrow)"
expect "$(su_get firstrun)" 200 "the wizard on a brand-new instance with no account"
expect "$(su_get firstrunhead)" 1 "…and it is the wizard"
expect "$(su_get firstruncheck)" 0 "…with no Check now button for a caller who cannot be anybody"
SU_V=$(su_get wrongrow)
contains "$SU_V" 'FAIL' || die "a wrong bucket did not fail the doctor: $SU_V"
# The word depends on the provider: MinIO answers 404 for a bucket that does not exist (`bucket missing`); R2 answers 403
# for one the token cannot see (`denied`) — the FOURTEENTH GATE on clippet.gg (R2) found this. Both are the probe's own
# closed vocabulary (§20 erratum 297 (b)); what the step proves is that a wrong bucket fails IN it, never a raw error.
contains "$SU_V" 'bucket missing' || contains "$SU_V" 'denied' || die "a wrong bucket did not fail in the probe's own vocabulary: $SU_V"
expect "$(su_get otherproxy)" 0 "the self-hosted-bucket walk's own MEDIA_DELIVERY=proxy did not boot against a plain-http endpoint under an https instance"
expect "$(su_get otherredir)" 1 "…and MEDIA_DELIVERY=redirect there is not the boot refusal that walk's block exists to avoid"
echo "[e2e]   self-hosted endpoint: the 'any other bucket' block boots against a plain-http origin under an https instance; the redirect that walk does NOT print is a config FAIL there"
if [ "$(su_get logerrors)" != 0 ]; then
  echo "[e2e]   NOTE: the probe's own app log has $(su_get logerrors) ERROR/panic line(s) — see $SU_OUT"
fi
su_rm_prefix
trap - EXIT
echo "[e2e]   block: the R2 walk's own lines boot an instance whose bucket row reads 'reachable, writable and cleanable'; a wrong bucket is 'bucket missing'"
if [ "${E2E_IS_OWNER:-0}" = 1 ]; then
  summary_row SU "" "setup (#45): /setup gated, five total blocks, the STOP callout, the folder behind Cloudflare named not refused, Check now single-flighted; prefix $SU_PREFIX emptied"
else
  summary_row SU "" "setup (#45): /setup gated, the member's 403 on the owner's pages (the owner's page stood down: a person owns this stack); prefix $SU_PREFIX emptied"
fi
