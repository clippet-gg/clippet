# 82-links.sh — ROADMAP #35 (DESIGN.md §20 errata 248-252, 264): the share link is the id at the ROOT of the
# origin, the address it replaced is a permanent redirect, and an upload may draw its id in either of two
# grammars. Steps 82-84. Sourced by scripts/e2e.sh (see scripts/e2e.d/README.md). Every name here is
# LK_-prefixed.
#
# e2e-schedule: parallel
# e2e-namespace: account=lk prefix=LK_,LK1_ buckets=-
# e2e-covers: internal/ids/** internal/httpapi/share.go internal/httpapi/router.go internal/web/server.go
# e2e-covers: internal/db/defaults.go internal/web/pages.go
#   Until lane `excl` it was exclusive for two reasons. Step 83 changes an ACCOUNT-WIDE setting (the
#   default link grammar) and puts it back — done to the run's owner, every capture upload any other
#   bundle made meanwhile would have drawn a MEMORABLE id. So steps 83-84 now upload, capture and
#   PATCH settings as an ACCOUNT OF ITS OWN, `${E2E_USER}_lk` (`bundle_account`, scripts/e2e.sh); the
#   owner's grammar is never touched, and every assertion about the grammar is the one it was, about
#   that account. The other reason was that step 82 asserts /robots.txt and walks every literal route:
#   instance-wide READS of things no bundle changes (no bundle reconfigures or restarts the app, the
#   README's third rule), so they are a property of the stack and not of what runs beside them. Step
#   82's reads of the shared clip C, the owner's /settings and the owner's own collection stay the
#   owner's — none of them changes anything account-wide.
#
# 82  the root link on the shared ready clip C: the share page at `/{id}` with the same §9.1 card
#     inventory and `og:url` at the root, HEAD, `/clips/{id}` → 308 → the same page, oEmbed accepting
#     BOTH url forms, a single segment that is not an id → the 404 page, and every literal route still
#     answering as itself rather than being eaten by `/{id}`.
# 83  a MEMORABLE upload: three capitalised words, `link_style` in the DTO, a card that unfurls; then
#     the same clip PATCHed unlisted keeps its id — and NOBODY is told anything about how guessable it
#     is (§20 erratum 264 withdrew erratum 251's owner-only line); an upload that is unlisted from the
#     start draws FOUR words; and the account default set to memorable makes a capture upload (which
#     sends no field) follow it — then back to compact.
# 84  the refusals: `link_style=weird` on both upload routes and on the settings PATCH, all one code;
#     and the redirect cannot be used to probe existence — an unknown VALID id answers 308 exactly as a
#     real one does, and the page it lands on is byte-for-byte the 404 the root form gives.

# LK_oe <url> → the oEmbed request for a page URL, percent-encoded as a query value. (55-owner.sh has
# its own copy; a helper two bundles share belongs in the harness, and this one is three lines.)
LK_oe() { $PY -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1" | tr -d '\r'; }
LK_oembed() { get "$BASE/oembed?format=json&url=$(LK_oe "$1")" "${@:2}"; }
# LK_words <id> <n> <what>: the id is exactly n capitalised words of 3-9 characters, joined.
LK_words() {
  printf '%s' "$1" | grep -qE "^([A-Z][a-z]{2,8}){$2}\$" \
    || die "$3: '$1' is not $2 capitalised words"
}
# LK_capture <extra curl args…> → status; body in $RESP. POST /api/upload's own grammar.
# Both helpers act as the bundle's OWN account (LK_JARW, made at step 83).
LK_capture() {
  curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$LK_JARW" "$@" \
    -F duplicate=allow -F "file=@$(W "$PWD/testclips/small-640x360.mp4");type=video/mp4" "$BASE/api/upload"
}
# LK_settings <json> → status; body in $RESP.
LK_settings() { http PATCH "$BASE/api/auth/me/settings" -b "$LK_JARW" -H 'Content-Type: application/json' -d "$1"; }

step 82 "the share link is the id at the root; /clips/{id} is a permanent redirect; no literal route is shadowed"
# The card first: the whole §9.1 inventory on the new address, with og:url naming it.
code=$(get "$BASE/api/clips/$C"); expect "$code" 200 "GET the DTO of C"
expect "$(json urls.page)" "$BASE/$C" "the DTO's share URL is the id at the root"
expect "$(json link_style)" compact "C was minted in the compact grammar"
LK_PW=$(json poster.width); LK_PH=$(json poster.height); LK_W=$(json width); LK_H=$(json height)
expect_nonempty "$LK_PW" "C poster.width"; expect_nonempty "$LK_W" "C width"
code=$(page "$BASE/$C" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot GET /{id}"
card_check player "$PAGE" "$BASE" "$C" "E2E vertical" "$LK_PW" "$LK_PH" "$LK_W" "$LK_H" "$SECURE"
has_file "$PAGE" "<meta property=\"og:url\" content=\"$BASE/$C\">" "og:url is the root form"
has_file "$PAGE" "<link rel=\"canonical\" href=\"$BASE/$C\">" "the canonical link is the root form"
lacks_file "$PAGE" "$BASE/clips/" "the share page renders no /clips/ URL of its own"
cp "$PAGE" "$CTX_DIR/lk-root.html"
code=$(head_ "$BASE/$C"); expect "$code" 200 "HEAD /{id}"

# The old address: 308, method-preserving, to the root form — and the page it lands on is the same one.
code=$(get "$BASE/clips/$C"); expect "$code" 308 "GET /clips/{id}"
expect "$(hdr Location)" "/$C" "the redirect's target"
# It reads no row and depends on no principal, so an ANONYMOUS redirect is publicly cacheable. The one
# that carries a refreshed session cookie is NOT, and internal/web pins that half (§20 errata 207, 248).
expect "$(hdr Cache-Control)" "public, max-age=3600" "an anonymous redirect is publicly cacheable"
code=$(head_ "$BASE/clips/$C"); expect "$code" 308 "HEAD /clips/{id} (308 preserves the method; a 301 need not)"
expect "$(hdr Location)" "/$C" "the redirect's target for HEAD"
code=$(page "$BASE/clips/$C" -L -A "$DISCORD_UA"); expect "$code" 200 "following the redirect as a crawler"
has_file "$PAGE" "<meta property=\"og:url\" content=\"$BASE/$C\">" "the followed page carries the root og:url"
code=$(page "$BASE/clips/$C?autoplay=1"); expect "$code" 308 "a redirect with a query"
expect "$(hdr Location)" "/$C?autoplay=1" "the query rides along"

# oEmbed takes BOTH spellings: the new one the page advertises and the one somebody already pasted.
for lk_u in "$BASE/$C" "$BASE/clips/$C" "$BASE/embed/$C"; do
  code=$(LK_oembed "$lk_u"); expect "$code" 200 "oEmbed for $lk_u"
  expect "$(json type)" video "oEmbed type for $lk_u"
  contains "$(json html)" "$BASE/embed/$C" || die "oEmbed html for $lk_u: $(json html)"
done
code=$(LK_oembed "$BASE/$C/extra"); expect "$code" 404 "oEmbed for a root URL with something after the id"
code=$(LK_oembed "$BASE/not-an-id"); expect "$code" 404 "oEmbed for a root URL that is not an id"
code=$(LK_oembed "$BASE/settings"); expect "$code" 404 "oEmbed for a page that is not a clip"

# A single segment that is not an id is the 404 PAGE — never a redirect, never a 500 — for every method.
# (`favicon.ico` left this list with §20 erratum 304: it IS a route now, asserted in step 71; `sitemap.xml` keeps the
# dotted case — a reserved-looking word that no grammar admits and nothing serves.)
for lk_p in nope not-an-id aZ3kQ9xB2mL sitemap.xml; do
  code=$(page "$BASE/$lk_p"); expect "$code" 404 "GET /$lk_p"
  lacks_file "$PAGE" 'og:video' "the 404 page for /$lk_p carries no card tags"
  code=$(http_refused POST "$BASE/$lk_p" -d ''); expect "$code" 404 "POST /$lk_p (405 would say every one-segment path is a route)"
done

# Every literal route still answers as ITSELF: `/{id}` matches every single-segment path, and only the
# mux's specificity rule keeps /login a login form. A collection of this run's own proves /c/ too.
code=$(http POST "$BASE/api/collections" -b "$JARW" -H 'Content-Type: application/json' -d '{"title":"E2E links"}')
expect "$code" 201 "create a collection for the /c/ row"
LK_COL=$(json id); expect_nonempty "$LK_COL" "collection id"
# lk_route <path> <status> <marker|-> <what> [curl args…]. The marker is what only THIS route's
# answer carries; the second assertion is that the share page did not eat it (the watch page's own
# wrapper class, which the EMBED deliberately does not have — it shares data-clip-id).
lk_route() {
  local path=$1 want=$2 marker=$3 what=$4 code; shift 4
  code=$(page "$BASE$path" "$@"); expect "$code" "$want" "$what"
  [ "$marker" = - ] || has_file "$PAGE" "$marker" "$what: its own page"
  lacks_file "$PAGE" 'class="clip watch"' "$what: not the share page"
}
lk_route /login 200 'data-auth="login"' "GET /login"
lk_route /register 200 'data-auth="register"' "GET /register"
lk_route /settings 200 'Upload defaults' "GET /settings" -b "$JARW"
lk_route /robots.txt 200 'User-agent: \*' "GET /robots.txt"
lk_route /manifest.webmanifest 200 'share_target' "GET /manifest.webmanifest"
lk_route "/embed/$C" 200 'class="embed"' "GET /embed/{id}"
lk_route "/c/$LK_COL" 200 'E2E links' "GET /c/{id}" -b "$JARW"
lk_route /static/app.css 200 - "GET /static/app.css"
code=$(get "$BASE/healthz"); expect "$code" 200 "GET /healthz"
code=$(get "$BASE/readyz"); expect "$code" 200 "GET /readyz"
code=$(get "$BASE/oembed"); expect "$code" 400 "GET /oembed with no url is the API's own answer"
code=$(get "$BASE/api/clips/$C"); expect "$code" 200 "GET /api/clips/{id}"
code=$(page "$BASE/settings/tokens" -b "$JARW"); expect "$code" 303 "GET /settings/tokens"
code=$(http DELETE "$BASE/api/collections/$LK_COL" -b "$JARW"); expect "$code" 204 "delete the collection"
# robots.txt says what it now means: the whole site is closed and four addresses are allowed back.
code=$(get "$BASE/robots.txt"); expect "$code" 200 "GET /robots.txt for the policy"
has_file "$RESP" 'Disallow: /' "robots.txt closes the whole site"
has_file "$RESP" 'Allow: /\$' "robots.txt allows the front door"
lacks_file "$RESP" 'Disallow: /clips/' "robots.txt no longer names a prefix no share link uses"
echo "[e2e]   /$C is the share page (og:url + canonical at the root); /clips/$C → 308 → it; oEmbed takes both forms; 13 literal routes intact"

step 83 "two id grammars: a memorable upload, four words when it is unlisted, and the account default the capture route follows"
# Everything from here to the end of step 84 is the bundle's OWN account (see the header): its uploads
# (UPLOAD_JARW, read by the harness's `upload`), its capture route and its settings. The one owner page
# below — a COMPACT clip's, C's — stays the owner's.
bundle_account lk
LK_USER=$BA_USER; LK_JARW=$BA_JARW; UPLOAD_JARW=$LK_JARW
code=$(upload LK1 small-640x360.mp4 '{"title":"E2E memorable","link_style":"memorable","ttl_days":1}')
expect "$code" 201 "upload LK1 memorable ($(cat "$RESP"))"
LK1=$(json id); expect_nonempty "$LK1" "LK1 id"
LK_words "$LK1" 3 "a listed memorable id"
expect "$(json link_style)" memorable "LK1's DTO names its grammar"
expect "$(json urls.page)" "$BASE/$LK1" "LK1's share URL is the words at the root"
expect "$(json unlisted)" false "LK1 starts listed"
wait_ready "$LK1" LK1
code=$(get "$BASE/api/clips/$LK1"); expect "$code" 200 "GET LK1"
LK1_PW=$(json poster.width); LK1_PH=$(json poster.height); LK1_W=$(json width); LK1_H=$(json height)
code=$(page "$BASE/$LK1" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot GET a memorable link"
card_check player "$PAGE" "$BASE" "$LK1" "E2E memorable" "$LK1_PW" "$LK1_PH" "$LK1_W" "$LK1_H" "$SECURE"
code=$(get "$BASE/clips/$LK1"); expect "$code" 308 "/clips/{memorable id} redirects too"

# Flipped to unlisted LATER: the id is an ADDRESS and does not move (§20 erratum 251). NOBODY is told
# how guessable it is: erratum 264 withdrew the owner-only line 251 shipped (the owner rejected that copy).
code=$(page "$BASE/$LK1" -b "$LK_JARW"); expect "$code" 200 "LK1's owner page while listed"
lacks_file "$PAGE" 'data-link-note' "a LISTED memorable clip carries no link note, hidden or shown"
lacks_file "$PAGE" 'Memorable links are easier to guess' "…and never the withdrawn sentence"
code=$(http PATCH "$BASE/api/clips/$LK1" -b "$LK_JARW" -H 'Content-Type: application/json' -d '{"unlisted":true}')
expect "$code" 200 "PATCH LK1 unlisted"
expect "$(json id)" "$LK1" "hiding a clip did not re-mint its id"
expect "$(json link_style)" memorable "…and it is still memorable"
expect "$(json unlisted)" true "…and it is unlisted"
code=$(page "$BASE/$LK1" -b "$LK_JARW"); expect "$code" 200 "LK1's owner page while unlisted"
lacks_file "$PAGE" 'data-link-note' "the owner of an UNLISTED memorable clip gets no link note either"
lacks_file "$PAGE" 'Memorable links are easier to guess' "…and never the withdrawn sentence"
code=$(page "$BASE/$LK1"); expect "$code" 200 "LK1's page for a stranger"
lacks_file "$PAGE" 'data-link-note' "a stranger gets no such row at all"
lacks_file "$PAGE" 'Memorable links are easier to guess' "…and never the sentence"
code=$(page "$BASE/$C" -b "$JARW"); expect "$code" 200 "a COMPACT clip's owner page"
lacks_file "$PAGE" 'data-link-note' "a compact clip has no such row at all, hidden or not"
code=$(http DELETE "$BASE/api/clips/$LK1" -b "$LK_JARW"); expect_deleted "$code" "$LK1" "DELETE LK1"
summary_row LK1 "$LK1" "memorable (3 words), flipped unlisted, deleted"

# Unlisted from the START draws FOUR words: on an unlisted clip the link is the only authentication.
code=$(upload LK2 small-640x360.mp4 '{"title":"E2E memorable hidden","link_style":"memorable","unlisted":true,"ttl_days":1}')
expect "$code" 201 "upload LK2 memorable+unlisted ($(cat "$RESP"))"
LK2=$(json id); expect_nonempty "$LK2" "LK2 id"
LK_words "$LK2" 4 "an unlisted memorable id"
expect "$(json unlisted)" true "LK2 is unlisted"
code=$(page "$BASE/$LK2"); expect "$code" 200 "LK2's page (the link is the auth)"
code=$(http DELETE "$BASE/api/clips/$LK2" -b "$LK_JARW"); expect_deleted "$code" "$LK2" "DELETE LK2"
summary_row LK2 "$LK2" "memorable (4 words, unlisted at upload), deleted"

# The account default: the capture endpoint sends no field — the shipped clippet.sxcu has no argument
# for it — so it follows the ACCOUNT. Set it, upload, put it back.
code=$(get "$BASE/api/auth/me/settings" -b "$LK_JARW"); expect "$code" 200 "GET my settings"
expect "$(json default_link_style)" compact "the shipped default grammar"
code=$(LK_settings '{"default_link_style":"memorable"}'); expect "$code" 200 "PATCH the default grammar"
expect "$(json default_link_style)" memorable "the stored preference"
code=$(LK_capture -F 'title=E2E links capture' -F 'ttl_days=1')
expect "$code" 201 "a capture upload under the account default ($(cat "$RESP"))"
LK3=$(json id); expect_nonempty "$LK3" "LK3 id"
LK_words "$LK3" 3 "the capture upload followed the account default"
expect "$(json url)" "$BASE/$LK3" "what the capture tool pastes"
# An explicit field still wins over the account, in both directions.
code=$(upload LK4 small-640x360.mp4 '{"title":"E2E links explicit compact","link_style":"compact","ttl_days":1}')
expect "$code" 201 "upload LK4 explicitly compact ($(cat "$RESP"))"
LK4=$(json id); expect_nonempty "$LK4" "LK4 id"
printf '%s' "$LK4" | grep -qE '^[A-Za-z0-9]{12}$' || die "LK4 '$LK4' is not a 12-character compact id"
expect "$(json link_style)" compact "an explicit compact field beat the account default"
code=$(LK_settings '{"default_link_style":"compact"}'); expect "$code" 200 "put the default grammar back"
expect "$(json default_link_style)" compact "the default is compact again"
code=$(http DELETE "$BASE/api/clips/$LK3" -b "$LK_JARW"); expect_deleted "$code" "$LK3" "DELETE LK3"
code=$(http DELETE "$BASE/api/clips/$LK4" -b "$LK_JARW"); expect_deleted "$code" "$LK4" "DELETE LK4"
summary_row LK3 "$LK3" "capture upload following the account default, deleted"
summary_row LK4 "$LK4" "explicit compact against a memorable account default, deleted"
echo "[e2e]   memorable = 3 words listed / 4 unlisted; a later flip keeps the id; the capture route follows the account"

step 84 "link_style rejections, and a redirect that cannot be used to probe which ids exist"
# One code, on every route that takes the field, and the upload is refused before its body is read.
code=$(upload LK5 small-640x360.mp4 '{"title":"E2E links bad","link_style":"weird"}')
expect "$code" 400 "POST /api/clips with link_style=weird"
expect "$(json error)" invalid_link_style "the JSON upload's error code"
code=$(LK_capture -F 'link_style=weird')
expect "$code" 400 "POST /api/upload with link_style=weird"
expect "$(json error)" invalid_link_style "the capture upload's error code"
code=$(LK_settings '{"default_link_style":"weird"}')
expect "$code" 400 "PATCH settings with a bad grammar"
expect "$(json error)" invalid_link_style "the settings error code"
code=$(get "$BASE/api/auth/me/settings" -b "$LK_JARW"); expect "$code" 200 "GET my settings after the refusal"
expect "$(json default_link_style)" compact "a refused PATCH changed nothing"
# An EMPTY capture field means "not sent", like every other one.
code=$(LK_capture -F 'link_style=' -F 'ttl_days=1')
expect "$code" 201 "an empty capture link_style is 'not sent' ($(cat "$RESP"))"
LK6=$(json id); expect_nonempty "$LK6" "LK6 id"
printf '%s' "$LK6" | grep -qE '^[A-Za-z0-9]{12}$' || die "LK6 '$LK6' is not compact"
code=$(http DELETE "$BASE/api/clips/$LK6" -b "$LK_JARW"); expect_deleted "$code" "$LK6" "DELETE LK6"
summary_row LK6 "$LK6" "empty capture link_style = not sent, deleted"

# The redirect reads no row, so it cannot tell a real id from one nobody has — and the page it lands
# on is byte-for-byte the 404 the root form gives directly.
LK_UNKNOWN=AaBbCcDdEeFf
code=$(get "$BASE/clips/$LK_UNKNOWN"); expect "$code" 308 "an unknown VALID id still redirects"
expect "$(hdr Location)" "/$LK_UNKNOWN" "…to the root form"
code=$(page "$BASE/clips/$LK_UNKNOWN" -L); expect "$code" 404 "following it lands on the 404 page"
cp "$PAGE" "$CTX_DIR/lk-404-hopped.html"
code=$(page "$BASE/$LK_UNKNOWN"); expect "$code" 404 "the root form of the same unknown id"
cmp -s "$PAGE" "$CTX_DIR/lk-404-hopped.html" || die "the redirect's 404 differs from the root form's — the two spellings can be told apart"
lacks_file "$PAGE" 'og:video' "the unknown-id 404 carries no card tags"
# A memorable-shaped id nobody has behaves the same way.
code=$(get "$BASE/clips/PurpleUpdateCharacter"); expect "$code" 308 "an unknown MEMORABLE id redirects"
code=$(page "$BASE/PurpleUpdateCharacter"); expect "$code" 404 "…and its root form is the 404 page"
echo "[e2e]   one error code on three routes; the redirect answers 308 for every valid id and lands on the same 404"
code=$(http POST "$BASE/api/auth/logout" -b "$LK_JARW" || echo 000); echo "[e2e]   logout $LK_USER -> $code"
