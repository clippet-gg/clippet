# 94-instance.sh — the instance's own self-description, GET /api/instance (§20 erratum 303): the four
# keys, the delivery mode this stack actually serves in, the commit step 1 built it from, no-store,
# anonymous, GET only — and the `/setup` wizard's third exposure, the Clippet Pass walk.
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 94.
#
# e2e-schedule: parallel
# e2e-schedule-apart: 85-bot
# e2e-namespace: account=- prefix=IN_ buckets=claim
# e2e-covers: internal/httpapi/instance.go internal/web/setup.go web/templates/setup.html
#   It creates nothing, deletes nothing and changes nothing: four anonymous GETs of one route, one
#   POST of it that is refused before any handler runs, and two reads of `/setup` with the run's own
#   cookie. It brings no container, publishes no port and starts no second process. It asserts
#   nothing bucket-wide, log-wide or instance-wide — the two facts it reads about the instance
#   (`media_delivery`, `build`) are the ones the harness itself already knows, which is what makes
#   them assertable rather than merely printable. It probes no rate limiter: the route shares the
#   `claim` bucket at 30/min per address and this bundle spends five of them, while 85-bot's own
#   claim assertions spend more — so the two are APART (they shared lane 4 for it until lane `speed2`).
#
# Variables are prefixed IN_ so nothing here can clobber a harness global.

# ---------------------------------------------------------------------------------------------------
step 94 "the instance's self-description (erratum 303): GET /api/instance, and the wizard's Clippet Pass walk"

IN_PAGE="$E2E_DIR/instance-setup.html"
IN_PAGEW=$(W "$IN_PAGE")

# --- (1) the four keys, and nothing else ----------------------------------------------------------
IN_C=$(get "$BASE/api/instance")
expect "$IN_C" 200 "an anonymous GET /api/instance ($(cat "$RESP"))"
expect "$(hdr Cache-Control)" "no-store" "the route's cache directive"
case "$(hdr Content-Type)" in
  application/json*) ;;
  *) die "Content-Type is '$(hdr Content-Type)', want application/json" ;;
esac
# The KEY SET, in order, out of the bytes — not out of a decode, because the order is part of the
# contract the plane reads (the same reason internal/httpapi's own pin walks the raw body). `jget`
# answers one key at a time, so the shape is asserted here with a plain sed over the body: four
# quoted keys at the top level, in this order, and no fifth.
IN_KEYS=$(tr -d ' \r\n' < "$RESP" | sed 's/^{//; s/}$//' | tr ',' '\n' | sed -n 's/^"\([a-z_]*\)":.*/\1/p' | tr '\n' ' ' | sed 's/ *$//')
expect "$IN_KEYS" "version build media_delivery media_origin" "the body's key set, in order (got: $(cat "$RESP"))"

IN_VERSION=$(json version); expect_nonempty "$IN_VERSION" "the body's version"
# …and it agrees with /healthz, which has reported the same field since Phase 1.
IN_C=$(get "$BASE/healthz"); expect "$IN_C" 200 "GET /healthz beside it"
expect "$(json version)" "$IN_VERSION" "/healthz and /api/instance disagree about this build's version"

# --- (2) media_delivery is the mode this stack actually serves in ---------------------------------
# E2E_MEDIA is DETECTED from the stack after step 6 (one anonymous GET of a rendition: 200 → proxy,
# 302 → redirect), never read out of `.env`. So this is the route's answer against an independently
# measured fact, on both shapes, which is the whole reason it is worth asserting at all.
IN_C=$(get "$BASE/api/instance")
expect "$(json media_delivery)" "$E2E_MEDIA" "the route's delivery mode against the one the harness measured"
IN_ORIGIN=$(json media_origin)
if [ "$E2E_MEDIA" = redirect ]; then
  expect_nonempty "$IN_ORIGIN" "in redirect mode media_origin must name the bucket origin"
  # …and it is the SAME origin every presigned Location lands on, which the harness also measured.
  expect "$IN_ORIGIN" "$E2E_MEDIA_ORIGIN" "media_origin against the origin the harness measured from a 302"
  echo "[e2e]   delivery: redirect, media_origin $IN_ORIGIN — the origin the harness measured from a real 302"
else
  # jget renders a JSON null as an empty string, which is what an absent origin must look like: in
  # proxy mode this instance IS the media origin and the field says there is no other.
  expect_empty "$IN_ORIGIN" "in proxy mode media_origin must be null"
  echo "[e2e]   delivery: proxy, media_origin null — every byte comes from this instance"
fi

# --- (3) build is the commit step 1 built this stack from ------------------------------------------
# Step 1 exports CLIPPET_COMMIT="$(git rev-parse HEAD)" before its `up -d --build`, compose passes it
# as the `COMMIT` build argument and the Dockerfile bakes it in as `-X main.commit`. So this is the
# one assertion that proves the whole chain — .env/shell → compose arg → ldflags → the wire.
IN_HEAD=$(git rev-parse HEAD 2>/dev/null || true)
IN_BUILD=$(json build)
if [ -n "$IN_HEAD" ]; then
  expect "$IN_BUILD" "$IN_HEAD" "build is not the commit this stack was built from (step 1 exported $IN_HEAD)"
  echo "[e2e]   build: $IN_BUILD — the commit step 1 handed compose, through the build arg and the linker"
else
  echo "[e2e]   NOTE: not a git checkout, so there is no HEAD to compare build against"
fi
# Whatever it is, it is a commit or it is null — never a word, a tag or a shell that did not run.
# The plane refuses anything else as drift and disables the name, so the shape is the contract.
case "$IN_BUILD" in
  "") echo "[e2e]   NOTE: build is null on this stack (nothing exported CLIPPET_COMMIT)" ;;
  *[!0-9a-f]*) die "build is '$IN_BUILD', which is not lower-case hex" ;;
  *) [ ${#IN_BUILD} = 40 ] || die "build is ${#IN_BUILD} characters, want 40" ;;
esac

# --- (4) anonymous, no cookie, GET only -----------------------------------------------------------
# It answered above with no jar at all, which is the anonymous half. The other half is that it hands
# nothing back: a verifier holds no credential on the box it asks and must not be given one.
expect_empty "$(hdr Set-Cookie)" "the route set a cookie"
IN_C=$(http POST "$BASE/api/instance")
expect "$IN_C" 405 "POST /api/instance"
expect "$(hdr Allow)" "GET, HEAD" "the 405's Allow header"
IN_C=$(head_ "$BASE/api/instance")
expect "$IN_C" 200 "HEAD /api/instance"
echo "[e2e]   the route: 200 no-store anonymous, four keys in order, 405 on POST, version '$IN_VERSION'"

# --- (5) the wizard's third exposure: the Clippet Pass walk ----------------------------------------
# One fetch with the owner cookie, the way 92-setup.sh does it. The block is the chosen store's
# CLOUDFLARE block plus three lines and `compose.pass.yaml` on the file list — so law 2 still holds
# (exactly one COMPOSE_FILE) and the delivery mode is still the one a name may use.
# The page is the OWNER's (§20 erratum 349): read only when step 3 found the run's account owns this
# stack (E2E_IS_OWNER); on a stack a person owns it stands down with a NOTE (92-setup pins the member's 403).
IN_WALK="the wizard's Pass walk"
if [ "${E2E_IS_OWNER:-0}" != 1 ]; then
  IN_WALK="the wizard's Pass walk stood down (a person owns this stack)"
  echo "[e2e]   NOTE: the setup page's Pass walk stands down: /setup is the owner's, and this stack's owner is a person, not the run's account"
else
in_page() { local q=$1; shift; curl_ -sS -o "$IN_PAGEW" -D "$HDRW" -w '%{http_code}' -b "$JARW" "$@" "$BASE/setup$q"; }
in_count() { grep -c -- "$1" "$IN_PAGE" | tr -d ' \r'; }

IN_C=$(in_page ""); expect "$IN_C" 200 "a signed-in GET /setup"
has_file_f "$IN_PAGE" 'value="pass"' "the chooser is missing the Clippet Pass exposure"
has_file_f "$IN_PAGE" '>via a Clippet Pass<' "…and its label"

IN_C=$(in_page "?store=r2&via=pass"); expect "$IN_C" 200 "GET /setup?store=r2&via=pass"
for IN_V in COMPOSE_FILE STORAGE_BACKEND MEDIA_DELIVERY PUBLIC_BASE_URL SESSION_COOKIE_NAME CLIPPET_PASS_TOKEN TRUSTED_PROXIES; do
  expect "$(in_count "^$IN_V=")" 1 "the Pass block renders $(in_count "^$IN_V=") $IN_V lines, want exactly 1"
done
has_file_f "$IN_PAGE" 'COMPOSE_FILE=compose.yaml:compose.nobucket.yaml:compose.pass.yaml' "the Pass block's file list"
has_file_f "$IN_PAGE" 'MEDIA_DELIVERY=redirect' "the Pass block's delivery mode"
has_file_f "$IN_PAGE" 'SESSION_COOKIE_NAME=__Host-clippet_session' "the Pass block's cookie name"
has_file_f "$IN_PAGE" 'on a Clippet Pass name' "the Pass block's own head line"
has_file_f "$IN_PAGE" 'CLIPPET_COMMIT' "the closing line that says what the Pass reads from this instance"
lacks_file_f "$IN_PAGE" 'data-setup-refusal' "a bucket on a Pass name was refused"
# Still no field to type a credential into (setup-page.md §0.1): the token is a printed LINE.
lacks_file_f "$IN_PAGE" 'name="CLIPPET_PASS_TOKEN"' "the wizard grew a field for the Pass token"
# The no-URL law at RUNTIME, with law 1's ONE exception taken out first: this origin, and the Pass
# name's own base inside the block. Nothing else, on this page state either.
IN_STRIPPED="$E2E_DIR/instance-setup-stripped.txt"
sed -e "s|$BASE||g" -e 's|https://&lt;name&gt;\.clippet\.gg||g' -e 's|https://<name>\.clippet\.gg||g' "$IN_PAGE" > "$IN_STRIPPED"
if grep -qE 'https?://' "$IN_STRIPPED"; then
  die "the rendered Pass walk carries a URL beyond this origin and the Pass base: $(grep -oE 'https?://[^"<[:space:]]*' "$IN_STRIPPED" | head -3 | tr '\n' ' ')"
fi
# …and a store on THIS machine WALKS under a Pass name with its risk named (erratum 348 (b), which
# withdrew erratum 299's refusal): the folder block with `compose.pass.yaml` on its file list, the
# name's lines, and the Cloudflare-terms line ABOVE the Pass's own closing line.
IN_C=$(in_page "?store=fs&via=pass"); expect "$IN_C" 200 "GET /setup?store=fs&via=pass"
has_file_f "$IN_PAGE" 'data-setup-walk="fs"' "a folder on a Pass name did not walk"
has_file_f "$IN_PAGE" 'COMPOSE_FILE=compose.yaml:compose.fs.yaml:compose.pass.yaml' "the folder Pass block's file list"
has_file_f "$IN_PAGE" 'MEDIA_DELIVERY=proxy' "the folder Pass block's delivery mode"
has_file_f "$IN_PAGE" 'CLIPPET_PASS_TOKEN=' "the folder Pass block's token line"
has_file_f "$IN_PAGE" 'Their terms reserve video for a bucket' "the folder walk on a name does not name its risk"
lacks_file_f "$IN_PAGE" 'data-setup-refusal' "a folder on a Pass name was refused"
IN_RISK=$(grep -nF -- 'Their terms reserve video for a bucket' "$IN_PAGE" | head -1 | cut -d: -f1)
IN_PASSN=$(grep -nF -- 'or the portal calls this Clippet behind' "$IN_PAGE" | head -1 | cut -d: -f1)
[ -n "$IN_RISK" ] && [ -n "$IN_PASSN" ] && [ "$IN_RISK" -le "$IN_PASSN" ] \
  || die "the folder's risk line must render above the Pass's (lines ${IN_RISK:-none} and ${IN_PASSN:-none})"
echo "[e2e]   setup: the Pass walk is the Cloudflare block + three lines with one COMPOSE_FILE; a folder on a name walks with its risk named"
fi   # E2E_IS_OWNER: the Pass walk

summary_row IN "" "instance (erratum 303): /api/instance four keys, delivery '$E2E_MEDIA', build ${IN_BUILD:-null}, $IN_WALK"
