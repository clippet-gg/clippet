# 80-front.sh — ROADMAP #36 (DESIGN.md §20 erratum 246): the front door for strangers and the wordmark on
# the embed player. Steps 80-81. Sourced by scripts/e2e.sh (see scripts/e2e.d/README.md). Every name here
# is FD_-prefixed.
#
# e2e-schedule: parallel
# e2e-namespace: account=- prefix=FD_ buckets=-
# e2e-covers: web/templates/front.html web/templates/embed.html internal/web/pages.go internal/web/mark.go
# e2e-covers: internal/web/compembed.go internal/httpapi/oembed.go
#   It renders pages: the anonymous `/`, the session's `/`, the embed and the share page of the shared
#   clip C, and C's oEmbed document. It creates no clip and no user, mutates nothing, restarts nothing,
#   asserts nothing bucket-, log- or instance-wide, and polls no limiter (a handful of page GETs).
#
# 80  a stranger at `/` gets the front door (200): the display wordmark, the one line, one ink `Log in`
#     button to /login, `Create an account` (every E2E stack runs REGISTRATION=open), and NOTHING else —
#     no header slot, no nav, no session control, no card tags, no noindex. The redirect it replaced
#     carried private/no-store + Vary: Cookie, and so does the page. A session's `/` is still the
#     library; HEAD works; POST is 405.
# 81  the embed carries the wordmark overlay -- this instance's own public HOST since §20 erratum
#     287, as the lockup on clippet.gg and as plain text anywhere else -- and nothing else: one
#     link to /C with target="_top",
#     no header, no nav, no inline style, no absolute href; the iframe-safe headers, the share page's
#     card inventory (og:video, twitter:player = the embed URL) and the oEmbed document are unchanged.

step 80 "the front door: anonymous / is a 200 with the wordmark, the line and a way in; a session's / is the library"
code=$(page "$BASE/"); expect "$code" 200 "GET / without a session"
has_file "$PAGE" '<title>Clippet</title>' "the front door's title is the product name"
has_file "$PAGE" '<section class="auth front">' "the front door in the auth layout"
has_file "$PAGE" '<span class="wordmark display" role="img" aria-label="clippet">cli<span class="pp">pp</span>et</span>' "the display wordmark"
has_file "$PAGE" '<p class="line">Upload a clip. Get a link.</p>' "the one line (§20 erratum 264: two sentences, no Discord clause)"
has_file "$PAGE" '<a class="btn primary" href="/login">Log in</a>' "the one ink button, to /login"
has_file "$PAGE" '<a href="/register">Create an account</a>' "Create an account (REGISTRATION=open on an E2E stack)"
lacks_file "$PAGE" '<header' "the front door has no header slot"
lacks_file "$PAGE" '<nav' "…no nav"
lacks_file "$PAGE" 'data-logout' "…no session control"
lacks_file "$PAGE" 'data-upload' "…no upload form"
lacks_file "$PAGE" 'data-clip-list' "…no library"
lacks_file "$PAGE" 'property="og:' "…no card tags"
lacks_file "$PAGE" 'name="robots"' "…and it is indexable"
lacks_file "$PAGE" 'style=' "…no inline style"
lacks_file "$PAGE" '<style' "…no style element"
FD_CC=$(hdr Cache-Control); expect "$FD_CC" "private, no-store" "the front door's Cache-Control (the headers the 303 carried)"
expect "$(hdr Vary)" "Cookie" "the front door varies on the cookie"
expect "$(hdr X-Frame-Options)" "DENY" "the front door is a page like any other"
code=$(head_ "$BASE/"); expect "$code" 200 "HEAD /"
code=$(http_refused POST "$BASE/" -d ''); expect "$code" 405 "POST / is still 405"
expect "$(hdr Allow)" "GET, HEAD" "…with Allow"
# A session lands on the library exactly as before: the upload form, the list, the nav.
code=$(page "$BASE/" -b "$JARW"); expect "$code" 200 "GET / with a session"
has_file "$PAGE" 'data-upload' "the session's / is the library (upload form)"
has_file "$PAGE" 'data-clip-list' "…the clip list"
has_file "$PAGE" 'data-logout' "…the session nav"
lacks_file "$PAGE" 'class="auth front"' "…and not the front door"
# /login is unchanged: the form, and next= still round-trips.
code=$(page "$BASE/login?next=/settings"); expect "$code" 200 "GET /login?next=/settings"
has_file "$PAGE" 'data-auth="login"' "the login form"
has_file "$PAGE" 'name="next" value="/settings"' "…carrying next"
echo "[e2e]   anonymous / → 200 front door (wordmark, the line, Log in, Create an account; no header/nav/cards); session / → the library; POST / 405"

step 81 "the embed carries the wordmark overlay and nothing else; headers, card inventory and oEmbed unchanged"
code=$(page "$BASE/embed/$C"); expect "$code" 200 "GET /embed/C"
# The overlay reads this instance's own HOST since §20 erratum 287. On clippet.gg and any subdomain
# of it the host CONTAINS the wordmark, so the lockup is intact -- a prefix, the live-text mark and
# `.gg`; anywhere else it is the host as plain text with no span, because a self-hoster's domain is
# not this product's logo. WHICH form a stack gets is a property of its origin, so the expected
# markup is BUILT from SITE_HOST (no origin-dependent assertions: assert what the stack reports).
case $SITE_HOST in
  clippet.gg|*.clippet.gg)
    FD_MARK="<a class=\"wordmark embed-mark\" href=\"/$C\" target=\"_top\" rel=\"noopener\" aria-label=\"$SITE_HOST\">${SITE_HOST%clippet.gg}cli<span class=\"pp\">pp</span>et.gg</a>"
    FD_MARK_WHAT="the overlay: the wordmark lockup spelling $SITE_HOST, to the share page, target=_top" ;;
  *)
    FD_MARK="<a class=\"wordmark embed-mark\" href=\"/$C\" target=\"_top\" rel=\"noopener\">$SITE_HOST</a>"
    FD_MARK_WHAT="the overlay: the host $SITE_HOST as plain text, to the share page, target=_top" ;;
esac
has_file "$PAGE" "$FD_MARK" "$FD_MARK_WHAT"
FD_LINKS=$( { grep -o '<a ' "$PAGE" || true; } | wc -l | tr -d ' '); expect "$FD_LINKS" 1 "the embed's link count (the overlay and nothing else)"
lacks_file "$PAGE" '<header' "the embed carries no header"
lacks_file "$PAGE" '<nav' "…no nav"
lacks_file "$PAGE" 'class="wordmark" href="/"' "…not the header's wordmark"
lacks_file "$PAGE" 'href="http' "…no absolute href (it resolves against this origin, and never mixed content)"
lacks_file "$PAGE" 'style=' "…no inline style"
lacks_file "$PAGE" '<style' "…no style element"
has_file "$PAGE" '<video controls playsinline' "the player is untouched"
lacks_file "$PAGE" 'data-views' "…no view count"
# The iframe-safe headers, unchanged.
code=$(head_ "$BASE/embed/$C"); expect "$code" 200 "HEAD /embed/C"
expect_empty "$(hdr X-Frame-Options)" "the embed still carries no X-Frame-Options"
FD_CSP=$(hdr Content-Security-Policy); contains "$FD_CSP" 'frame-ancestors *' || die "the embed CSP lost frame-ancestors *: $FD_CSP"
contains "$FD_CSP" "style-src 'self'" || die "the embed CSP is not style-src 'self': $FD_CSP"
# The overlay is one class in app.css, and the play/pause rule is an attribute on the body.
FD_CSS=$(sed -n 's/.*<link rel="stylesheet" href="\([^"]*\)".*/\1/p' "$PAGE" | head -1)
expect_nonempty "$FD_CSS" "the embed's stylesheet link"
code=$(get "$BASE$FD_CSS"); expect "$code" 200 "GET the stylesheet"
# has_file_f, not has_file: these are CSS selectors, and the regex pair would read `.` as any character
# and `[` as an opening character class. The selectors are pinned whole here — the second used to have
# its `[data-playing]` amputated to dodge exactly that.
has_file_f "$RESP" '.embed-mark { position: absolute; top: 10px; left: 12px;' "the overlay's rule (top-LEFT since §20 erratum 287: the poster's duration owns the top-right)"
has_file_f "$RESP" 'body.embed[data-playing] .embed-mark { display: none; }' "the hidden-while-playing rule"
# The share page's card inventory is unchanged: the full §9.1 check with the dims the page states,
# og:video still the social file, twitter:player still the embed URL.
code=$(page "$BASE/$C" -A "$DISCORD_UA"); expect "$code" 200 "Discordbot share page for C"
code=$(get "$BASE/api/clips/$C"); expect "$code" 200 "GET /api/clips/C"
FD_PW=$(json poster.width); FD_PH=$(json poster.height); expect_nonempty "$FD_PW" "C poster.width"; expect_nonempty "$FD_PH" "C poster.height"
FD_VW=$(sed -n 's/.*<meta property="og:video:width" content="\([0-9]*\)".*/\1/p' "$PAGE" | head -1)
FD_VH=$(sed -n 's/.*<meta property="og:video:height" content="\([0-9]*\)".*/\1/p' "$PAGE" | head -1)
expect_nonempty "$FD_VW" "og:video:width on C"; expect_nonempty "$FD_VH" "og:video:height on C"
card_check player "$PAGE" "$BASE" "$C" "E2E vertical" "$FD_PW" "$FD_PH" "$FD_VW" "$FD_VH" "$SECURE"
has_file "$PAGE" "<meta name=\"twitter:player\" content=\"$BASE/embed/$C\">" "twitter:player is still the embed URL"
has_file "$PAGE" "<meta property=\"og:video\" content=\"$BASE/api/clips/$C/social.mp4\">" "og:video is still the social file"
# The oEmbed document is unchanged: provider_name, type video, the iframe to the embed.
code=$(get "$BASE/oembed?url=$BASE/$C&format=json"); expect "$code" 200 "GET /oembed for C"
expect "$(json type)" video "oEmbed type"
expect "$(json provider_name)" "$SITE_HOST" "oEmbed provider_name (this instance's own host, §20 erratum 287)"
contains "$(json html)" "$BASE/embed/$C" || die "the oEmbed html does not frame the embed: $(json html)"
echo "[e2e]   embed: one overlay link to /C (target=_top), no header/nav/inline style; frame-ancestors *, the card inventory and the oEmbed document unchanged"
