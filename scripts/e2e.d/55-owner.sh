# 55-owner.sh — owner extras and integrations: one-clip collection membership, oEmbed, per-account
# upload defaults, the owner's own tally, the advisory storage budget and per-tag Discord routing.
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Steps 55-60. Everything it creates it destroys; nothing it does outlives the run.
#
# e2e-schedule: parallel
# e2e-namespace: account=ow,ob prefix=OWN_,OW_,OE_,OL_ buckets=-
# e2e-covers: internal/httpapi/collections.go internal/db/collections.go web/templates/collection.html
# e2e-covers: internal/httpapi/oembed.go internal/db/defaults.go internal/httpapi/settings.go
# e2e-covers: internal/httpapi/owner_error.go internal/cost/** internal/db/tags.go internal/notify/notify.go
# e2e-covers: internal/worker/notify.go internal/db/stats.go
#   Until lane `excl` it was exclusive for three reasons, and each now has its own fix:
#   - step 56 ends by DRAINING the `view` limiter (up to 200 oEmbed requests until one is refused), a
#     bucket of 60 a minute per client ADDRESS that every other bundle's GIF poll, filmstrip poll and
#     view beacon draw on. It is drained from an address of its own now (`burst_429 … apart`,
#     scripts/e2e.sh): the same live app, ceiling, route and query, and the host's bucket untouched.
#   - step 57 changed the SHARED account's upload defaults (30 days, unlisted) and changed them back,
#     which every silent upload in the run would have inherited meanwhile. It now changes them on an
#     ACCOUNT OF ITS OWN, `${E2E_USER}_ow` (`bundle_account`): every assertion is the one it was, about
#     that account, and the owner's defaults are never touched.
#   - steps 59-60 started a SECOND app process against the LIVE database (only while the queue was
#     idle) and asserted a GLOBAL notify-job count. They now run on an instance of their OWN — a
#     `dc run --rm` container with its own database, scratch, key prefix and STORAGE_DIR, driven over
#     its own loopback (89-quota's shape) — so the notify count is that instance's own and is asserted
#     EXACTLY on every stack (it used to be skipped whenever the run had a real webhook). The one
#     reading of the live stack that stays is the pair of read-only `admin stats` probes with and
#     without a budget, which open the database and serve nothing.
#   Steps 55, 56 and 58 stay the owner's: a collection of the owner's own (C and E as members, rows
#   only), oEmbed reads of C and E, and the owner's own tally — none of them changes anything
#   account-wide.
#
#   (It carried `# e2e-schedule-after: 47-upload` until lane `excl`: 47-upload asserts the OWNER expresses
#   no TTL preference, which step 57 used to set and clear on the owner. Step 57 no longer touches the
#   owner, so there is no order left to keep, and the two run beside each other.)

# ---------------------------------------------------------------------------------------------------
step 55 "collections one clip at a time: POST/DELETE …/clips/{id}, an APPEND, and the whole matrix"
# The subjects are the run's own ready clips (C and E), which live until step 17 — no upload, no
# transcode, and the collection this step makes is deleted before it ends.
code=$(http POST "$BASE/api/collections" -b "$JARW" -H 'Content-Type: application/json' -d '{"title":"E2E owner collection"}')
expect "$code" 201 "create the owner collection ($(cat "$RESP"))"
OWN_COL=$(json id); expect_nonempty "$OWN_COL" "the owner collection id"

member() { http "$1" "$BASE/api/collections/$2/clips/$3" "${@:4}"; }   # member <METHOD> <cid> <clip> [curl args]

# (a) One clip, one request. The answer is the CollectionDTO the PUT answers with.
code=$(member POST "$OWN_COL" "$C" -b "$JARW"); expect "$code" 200 "add C ($(cat "$RESP"))"
expect "$(json clip_count)" 1 "clip_count after adding C"
expect "$(json id)" "$OWN_COL" "the add answers with the collection"
expect_empty "$(json clips.0.id)" "the add's answer carries no clips array (that is the GET's shape)"
# (b) The second add APPENDS: the order the owner already has is not disturbed.
code=$(member POST "$OWN_COL" "$E" -b "$JARW"); expect "$code" 200 "add E"
expect "$(json clip_count)" 2 "clip_count after adding E"
code=$(get "$BASE/api/collections/$OWN_COL"); expect "$code" 200 "GET the owner collection"
expect "$(json clips.0.id)" "$C" "C is still first"
expect "$(json clips.1.id)" "$E" "E was appended after it"
# (c) Adding a clip that is already in it is SUCCESS and does not move it.
code=$(member POST "$OWN_COL" "$C" -b "$JARW"); expect "$code" 200 "re-add C"
expect "$(json clip_count)" 2 "a repeated add did not duplicate the membership"
code=$(get "$BASE/api/collections/$OWN_COL")
expect "$(json clips.0.id)" "$C" "a repeated add did not move C to the end"

# (d) Remove is 204, idempotent in the CLIP, and it removes a LINK and not a clip.
code=$(member DELETE "$OWN_COL" "$C" -b "$JARW"); expect "$code" 204 "remove C"
code=$(member DELETE "$OWN_COL" "$C" -b "$JARW"); expect "$code" 204 "removing a non-member is still 204"
code=$(get "$BASE/api/clips/$C"); expect "$code" 200 "C itself survived being removed from a collection"
code=$(get "$BASE/api/collections/$OWN_COL")
expect "$(json clip_count)" 1 "only E is left"
expect "$(json clips.0.id)" "$E" "…and it is E"

# (e) The matrix. A stranger's collection is 404 and a clip that is not the caller's is 400 — the two
#     answers the whole-list PUT already gives, on the same rules.
JARO="$E2E_DIR/cookies-owner-b.txt"; JAROW=$(W "$JARO"); rm -f "$JARO"
code=$(http POST "$BASE/api/auth/login" -c "$JAROW" -H 'Content-Type: application/json' \
  -d "{\"username\":\"${E2E_USER}_b\",\"password\":\"$E2E_PASS\"}")
expect "$code" 200 "log the second user in for the membership matrix"
code=$(http POST "$BASE/api/collections" -b "$JAROW" -H 'Content-Type: application/json' -d '{"title":"E2E stranger collection"}')
expect "$code" 201 "the stranger's collection"
OWN_THEIRS=$(json id); expect_nonempty "$OWN_THEIRS" "the stranger collection id"

code=$(member POST "$OWN_THEIRS" "$E" -b "$JARW"); expect "$code" 404 "a stranger's collection"
expect "$(json error)" not_found "stranger-collection error code"
OWN_BODY=$(cat "$RESP")
code=$(member POST "AaBbCcDdEeFf" "$E" -b "$JARW"); expect "$code" 404 "a collection that does not exist"
expect "$(cat "$RESP")" "$OWN_BODY" "a real stranger's collection and an invented one answer identically"
code=$(member POST "not-an-id" "$E" -b "$JARW"); expect "$code" 404 "a malformed collection id"
code=$(member DELETE "$OWN_THEIRS" "$E" -b "$JARW"); expect "$code" 404 "a stranger's collection, remove"
code=$(member POST "$OWN_COL" "AaBbCcDdEeFf" -b "$JARW"); expect "$code" 400 "a clip that does not exist"
expect "$(json error)" not_your_clip "unknown-clip error code"
code=$(member POST "$OWN_COL" "not-an-id" -b "$JARW"); expect "$code" 400 "a malformed clip id"
expect "$(json error)" not_your_clip "malformed-clip error code"
code=$(member DELETE "$OWN_COL" "not-an-id" -b "$JARW"); expect "$code" 400 "a malformed clip id, remove"

# (f) The posture: session only, CSRF-guarded, and no other method.
code=$(member POST "$OWN_COL" "$E"); expect "$code" 401 "an anonymous add"
code=$(http POST "$BASE/api/auth/tokens" -b "$JARW" -H 'Content-Type: application/json' -d '{"name":"e2e member"}')
expect "$code" 201 "mint a token for the membership posture check"
OWN_TOK=$(json token); OWN_TID=$(json id)
code=$(member POST "$OWN_COL" "$E" -H "Authorization: Bearer $OWN_TOK")
expect "$code" 403 "a capture token add"
expect "$(json error)" token_not_allowed "token add error code"
code=$(member DELETE "$OWN_COL" "$E" -H "Authorization: Bearer $OWN_TOK")
expect "$code" 403 "a capture token remove"
code=$(http DELETE "$BASE/api/auth/tokens/$OWN_TID" -b "$JARW"); expect "$code" 204 "revoke the membership token"
code=$(member POST "$OWN_COL" "$E" -b "$JARW" -H 'Origin: https://evil.example'); expect "$code" 403 "a cross-origin add"
code=$(member DELETE "$OWN_COL" "$E" -b "$JARW" -H 'Origin: https://evil.example'); expect "$code" 403 "a cross-origin remove"
code=$(get "$BASE/api/collections/$OWN_COL/clips/$E" -b "$JARW"); expect "$code" 405 "GET the membership route"
# …and the whole-list PUT beside it is untouched by any of this.
code=$(http PUT "$BASE/api/collections/$OWN_COL/clips" -b "$JARW" -H 'Content-Type: application/json' -d "{\"clip_ids\":[\"$E\"]}")
expect "$code" 200 "the whole-list PUT still works"
code=$(get "$BASE/api/collections/$OWN_COL"); expect "$(json clip_count)" 1 "…and E is still the one member"

# (g) The clip page carries the control, ticked from the row, and a stranger's copy carries none of it.
code=$(page "$BASE/$E" -b "$JARW"); expect "$code" 200 "the owner's share page"
has_file "$PAGE" 'data-clip-collections' "the owner's share page offers the collection control"
has_file "$PAGE" "data-member-url=\"/api/collections/$OWN_COL/clips/$E\"" "…pointing at the single-clip route"
has_file "$PAGE" "E2E owner collection" "…and naming the collection"
lacks_file "$PAGE" "E2E stranger collection" "a stranger's collection is never offered"
lacks_file "$PAGE" 'onclick=' "the share page carries no inline handler"
code=$(page "$BASE/$E"); expect "$code" 200 "the anonymous share page"
lacks_file "$PAGE" 'data-clip-collections' "an anonymous share page has no collection control"
lacks_file "$PAGE" 'data-member-url' "…and no membership URL"

code=$(http DELETE "$BASE/api/collections/$OWN_THEIRS" -b "$JAROW"); expect "$code" 204 "delete the stranger's collection"
code=$(http DELETE "$BASE/api/collections/$OWN_COL" -b "$JARW"); expect "$code" 204 "delete the owner collection"
code=$(get "$BASE/api/clips/$E"); expect "$code" 200 "E survived the collection being deleted"
echo "[e2e]   one clip in, one clip out; 404 for a collection that is not yours, 400 for a clip that is not"

# ---------------------------------------------------------------------------------------------------
step 56 "oEmbed: a player document for a ready clip, a link for the rest, this origin only, rate limited"
OE_PAGE="$BASE/$C"
# The `url` parameter is a whole URL inside a query value, so it is percent-encoded — and the CR a
# Windows Python would put on stdout is stripped, because this value goes straight into a request.
oe_q() { $PY -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1" | tr -d '
'; }
oembed() { get "$BASE/oembed?format=json&url=$(oe_q "$1")" "${@:2}"; }

# A clip that is NOT ready is described as a `link`: no `html` to embed, and the link-out card's own
# placeholder picture. Proving that live needs a clip that is STILL ENCODING while the request is
# made, and every fixture in testclips/ is through the encoder in about a second — so the subject is
# built here, by stream-COPYING hd-1080p60.mp4 ten times end to end. The copy costs the host nothing
# (no re-encode, ~0.2 s) and the server sixty seconds of 1080p60 to scale, which is six to seven
# seconds of encoding on an idle box and considerably more with the other lanes running. The document
# is then fetched the instant the 201 lands, and the clip's state is re-read AFTERWARDS: a clip that
# is queued or processing at t+1 was certainly not ready at t, which is what makes the assertion
# below a statement about a not-ready clip and not about whichever state it happened to be in.
OE_LONG="$E2E_DIR/oembed-notready.mp4"
ffmpeg -hide_banner -nostdin -y -stream_loop 9 -i "$(W "$PWD/testclips/hd-1080p60.mp4")" \
  -c copy -movflags +faststart "$(W "$OE_LONG")" > "$E2E_DIR/oembed-ffmpeg.txt" 2>&1 \
  || die "could not build the slow-encode fixture: $(tail -3 "$E2E_DIR/oembed-ffmpeg.txt")"
printf '%s' '{"title":"E2E oembed not ready","ttl_days":1}' > "$E2E_DIR/meta-OL.json"
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$JARW" \
  -F "metadata=<$(W "$E2E_DIR/meta-OL.json");type=application/json" \
  -F duplicate=allow -F "video=@$(W "$OE_LONG");type=video/mp4" "$BASE/api/clips")
expect "$code" 201 "upload OL, the sixty-second not-ready subject ($(cat "$RESP"))"
OL=$(json id); expect_nonempty "$OL" "OL id"
code=$(oembed "$BASE/$OL"); expect "$code" 200 "oEmbed for a clip that is still encoding"
# The document is KEPT, byte for byte, before anything else touches $RESP: every assertion below is
# about the answer to that one request, at that one instant, and not about a re-read that may have
# crossed into `ready` meanwhile.
OL_DOC="$E2E_DIR/oembed-notready.json"; OL_DOCW=$(W "$OL_DOC"); cp "$RESP" "$OL_DOC"
# The same instant's SHARE PAGE, kept the same way: this is the suite's one deterministic link-out
# card, so the §9.1 link-out inventory (§20 erratum 294: sixteen tags, alt sentences included) is
# asserted on it rather than only on the harness's own clip E, whose encode can finish first. It is
# fetched as a HUMAN: a crawler with no card to read is held at 503 with no card tags at all
# (erratum 213), which is a different page and 67-capture.sh's step 68 is where that one is proven.
code=$(page "$BASE/$OL"); expect "$code" 200 "OL's share page, at the same instant as the document"
OL_PAGE_HTML="$E2E_DIR/oembed-notready.html"; cp "$PAGE" "$OL_PAGE_HTML"
code=$(get "$BASE/api/clips/$OL" -b "$JARW"); expect "$code" 200 "OL's state, read after the document"
OL_STATE=$(json state)
case $OL_STATE in
  queued|processing)
    expect "$(jget "$OL_DOCW" type)" link "a clip that is $OL_STATE is a LINK, not a player"
    lacks_file "$OL_DOC" '"html"' "a clip that is not ready must carry no player to embed"
    expect_empty "$(jget "$OL_DOCW" width)" "…and no player dimensions"
    contains "$(jget "$OL_DOCW" thumbnail_url)" 'card-placeholder' \
      || die "the not-ready thumbnail is not the placeholder card: $(jget "$OL_DOCW" thumbnail_url)"
    # …and the page that document describes carries the whole link-out card, counted.
    card_check linkout "$OL_PAGE_HTML" "$BASE" "$OL" "E2E oembed not ready" encoding
    echo "[e2e]   a clip that is $OL_STATE is a link with the placeholder card and its sixteen tags" ;;
  *)
    # Sixty seconds of 1080p60 through the encoder before a second request could land is not a
    # machine this suite has ever run on; if it ever happens, say so rather than assert nothing.
    echo "[e2e]   NOTE: OL was already $OL_STATE — the not-ready document was not observed live"
    expect "$(jget "$OL_DOCW" type)" video "OL is $OL_STATE, so its document must be the player one"
    expect "$OL_STATE" ready "OL reached a state that is neither encoding nor ready" ;;
esac

code=$(oembed "$OE_PAGE"); expect "$code" 200 "GET /oembed for a ready clip ($(cat "$RESP"))"
expect "$(json version)" 1.0 "oEmbed version"
expect "$(json type)" video "a ready clip is a PLAYER, not a link"
expect "$(json provider_name)" "$SITE_HOST" "provider_name is this instance's own host (§20 erratum 287)"
expect "$(json provider_url)" "$BASE" "provider_url is this origin"
contains "$(json html)" "$BASE/embed/$C" || die "the html does not embed this clip: $(json html)"
contains "$(json html)" 'allowfullscreen' || die "the iframe is not the share page's snippet: $(json html)"
contains "$(json thumbnail_url)" "/api/clips/$C/poster.jpg" || die "thumbnail_url = $(json thumbnail_url)"
# It carries what a consumer needs and NOTHING owner-private.
for key in views owner source trim source_duration_ms; do
  lacks_file "$RESP" "\"$key\"" "the oEmbed document must not carry $key"
done
# The discovery link is on the page, and it points at exactly this document.
code=$(page "$OE_PAGE"); expect "$code" 200 "the share page for the discovery link"
has_file "$PAGE" 'rel="alternate" type="application/json+oembed"' "the share page advertises its oEmbed document"
has_file "$PAGE" "$BASE/oembed?format=json&amp;url=" "…at this origin's /oembed"
# An UNLISTED clip answers exactly as a listed one does: the link is the auth.
code=$(oembed "$BASE/$E"); expect "$code" 200 "oEmbed for an unlisted clip"
expect "$(json type)" video "an unlisted clip is described exactly like a listed one"
# The embed page's URL names the same clip.
code=$(oembed "$BASE/embed/$C"); expect "$code" 200 "oEmbed for the embed URL"
expect "$(json type)" video "the embed URL names the same clip"
# maxwidth scales DOWN and never up.
code=$(oembed "$OE_PAGE" ); OE_W=$(json width); OE_H=$(json height)
code=$(get "$BASE/oembed?format=json&maxwidth=320&url=$(oe_q "$OE_PAGE")")
expect "$code" 200 "oEmbed with maxwidth"
expect "$(json width)" 320 "maxwidth=320 bounded the player"
contains "$(json html)" 'width="320"' || die "the iframe was not resized: $(json html)"
code=$(get "$BASE/oembed?format=json&maxwidth=4000&url=$(oe_q "$OE_PAGE")")
expect "$(json width)" "$OE_W" "a maxwidth above the clip is a ceiling, not an instruction to grow"
expect "$(json height)" "$OE_H" "…in both dimensions"

# The HOST CHECK. Every one of these is a plain 404 that reports nothing about the URL it was handed.
for bad in \
  "https://evil.example/clips/$C" \
  "http://evil.example/clips/$C" \
  "http://localhost:8481@evil.example/clips/$C" \
  "http://user:pass@localhost:8483/clips/$C" \
  "$BASE/CLIPS/$C" \
  "$BASE/clips/$C/extra" \
  "$BASE/clips/not-an-id" \
  "$BASE/c/$C" \
  "$BASE/" ; do
  code=$(oembed "$bad"); expect "$code" 404 "oEmbed($bad)"
  lacks_file "$RESP" 'evil.example' "the 404 must not echo the URL it was handed"
done
code=$(oembed "$BASE/AaBbCcDdEeFf"); expect "$code" 404 "a well-formed id no clip has"
expect "$(json error)" not_found "missing-clip error code"
# The parameter grammar.
code=$(get "$BASE/oembed"); expect "$code" 400 "no url at all"
expect "$(json error)" invalid_request "missing-url error code"
code=$(get "$BASE/oembed?format=xml&url=$(oe_q "$OE_PAGE")")
expect "$code" 501 "format=xml is Not Implemented, which is the spec's own answer"
code=$(get "$BASE/oembed?format=yaml&url=$(oe_q "$OE_PAGE")")
expect "$code" 400 "an unknown format"
code=$(http_refused POST "$BASE/oembed?url=$OE_PAGE"); expect "$code" 405 "POST /oembed"

# OL has served its purpose. It is driven to a terminal state before it is deleted (the shape step 57
# uses), and once it is gone oEmbed describes it to nobody: the tombstone and the purged row are the
# same plain 404 the wrong-host and unknown-id cases get.
OE_N=0
while [ "$OE_N" -lt 180 ]; do
  code=$(get "$BASE/api/clips/$OL/progress"); expect "$code" 200 "progress OL"
  case $(json state) in ready|failed) break;; esac
  OE_N=$((OE_N + 1)); sleep 1
done
code=$(http DELETE "$BASE/api/clips/$OL" -b "$JARW"); expect_deleted "$code" "$OL" "DELETE OL"
code=$(oembed "$BASE/$OL"); expect "$code" 404 "oEmbed for a clip that has been deleted"
expect "$(json error)" not_found "deleted-clip error code"
rm -f "$OE_LONG" "$OL_DOC"   # the fixture is 56 MB of looped 1080p60; it has done its work

# The view limiter (60/min/IP), which is what stops the route being pointed at something. The bucket is
# shared with /api/clips/{id}/view, the progress batch, the collection GET, preview.gif and
# filmstrip.jpg, so the burst comes from an address of its own (`apart`, the header): the bucket the
# rest of the run spends is not the one this empties.
# The burst goes through the harness's `burst_429` (§20 erratum 283 (f)): the same requests, against
# the same live ceiling, with every pre-refusal answer still asserted 200 and the refusal still
# required inside 200 — but in one `curl` process per chunk instead of one per request. The URL is the
# one `oembed` builds; it is spelled out here because the helper takes a URL rather than a wrapper.
burst_429 "$BASE/oembed?format=json&url=$(oe_q "$OE_PAGE")" 200 "the view limiter on /oembed" apart
OE_N=$BURST_N
expect "$(json error)" rate_limited "oEmbed 429 error code"
expect_nonempty "$(hdr Retry-After)" "the 429 carries Retry-After"
echo "[e2e]   oEmbed describes this origin's clips and nothing else; 429 after $OE_N requests"

# ---------------------------------------------------------------------------------------------------
step 57 "per-account upload defaults: a field always wins, silence takes the account's, then the instance's"
# The account is the bundle's OWN (the header): its settings, its uploads (UPLOAD_JARW, read by the
# harness's `upload`) and its deletes. Nothing here touches the run's owner.
bundle_account ow
OW_USER=$BA_USER; OW_JARW=$BA_JARW; UPLOAD_JARW=$OW_JARW
code=$(get "$BASE/api/auth/me/settings" -b "$OW_JARW"); expect "$code" 200 "GET my settings"
expect_empty "$(json default_ttl_days)" "a fresh account expresses no TTL preference"
expect "$(json default_unlisted)" false "…and no visibility preference"
expect_nonempty "$(json instance_default_ttl_days)" "the settings say what the server default resolves to"
contains "$(hdr cache-control)" no-store || die "the settings answer is cacheable: $(hdr cache-control)"

code=$(http PATCH "$BASE/api/auth/me/settings" -b "$OW_JARW" -H 'Content-Type: application/json' \
  -d '{"default_ttl_days":30,"default_unlisted":true}')
expect "$code" 200 "PATCH my settings ($(cat "$RESP"))"
expect "$(json default_ttl_days)" 30 "the stored TTL preference"
expect "$(json default_unlisted)" true "the stored visibility preference"

# (a) An upload that says NOTHING gets the account's defaults.
code=$(upload OD small-640x360.mp4 '{"title":"E2E defaults silent"}')
expect "$code" 201 "upload OD ($(cat "$RESP"))"
OD=$(json id); expect_nonempty "$OD" "OD id"
expect "$(json ttl_days)" 30 "OD took the ACCOUNT's 30 days"
expect "$(json unlisted)" true "OD took the ACCOUNT's unlisted default"
expect_ttl_ahead "$(json expires_at)" 2592000 "OD expires thirty days out"
# (b) An upload that says something WINS, in both directions.
code=$(upload OE2 small-640x360.mp4 '{"title":"E2E defaults explicit","ttl_days":1,"unlisted":false}')
expect "$code" 201 "upload OE2 ($(cat "$RESP"))"
OE2=$(json id); expect_nonempty "$OE2" "OE2 id"
expect "$(json ttl_days)" 1 "an explicit ttl_days beat the account default"
expect "$(json unlisted)" false "an explicit unlisted beat the account default"
# (c) The capture form agrees — the defaults live in ONE place both routes run.
code=$(curl -sS -o "$RESPW" -D "$HDRW" -w '%{http_code}' -b "$OW_JARW" \
  -F "title=E2E defaults capture" \
  -F duplicate=allow -F "file=@$(W "$PWD/testclips/small-640x360.mp4");type=video/mp4" "$BASE/api/upload")
expect "$code" 201 "capture upload under the account defaults ($(cat "$RESP"))"
OF=$(json id); expect_nonempty "$OF" "OF id"
code=$(get "$BASE/api/clips/$OF" -b "$OW_JARW"); expect "$code" 200 "GET the capture clip"
expect "$(json ttl_days)" 30 "the capture route took the ACCOUNT's 30 days"
expect "$(json unlisted)" true "…and its unlisted default"

# The settings posture: session only, CSRF-guarded, and the closed TTL set.
code=$(get "$BASE/api/auth/me/settings"); expect "$code" 401 "anonymous settings"
code=$(http POST "$BASE/api/auth/tokens" -b "$OW_JARW" -H 'Content-Type: application/json' -d '{"name":"e2e settings"}')
expect "$code" 201 "mint a token for the settings posture check"
OWN_TOK=$(json token); OWN_TID=$(json id)
code=$(get "$BASE/api/auth/me/settings" -H "Authorization: Bearer $OWN_TOK"); expect "$code" 403 "a capture token reading settings"
expect "$(json error)" token_not_allowed "token settings error code"
code=$(http DELETE "$BASE/api/auth/tokens/$OWN_TID" -b "$OW_JARW"); expect "$code" 204 "revoke the settings token"
code=$(http_refused PATCH "$BASE/api/auth/me/settings" -b "$OW_JARW" -H 'Content-Type: application/json' -H 'Origin: https://evil.example' -d '{}')
expect "$code" 403 "a cross-origin settings PATCH"
code=$(http_refused PATCH "$BASE/api/auth/me/settings" -b "$OW_JARW" -H 'Content-Type: application/json' -d '{"default_ttl_days":5}')
expect "$code" 400 "an illegal default TTL"
expect "$(json error)" invalid_ttl "illegal-default-TTL error code"

# Back to no preference at all — the round trip is part of what is proven: null clears the TTL
# preference and an upload is listed again.
code=$(http PATCH "$BASE/api/auth/me/settings" -b "$OW_JARW" -H 'Content-Type: application/json' \
  -d '{"default_ttl_days":null,"default_unlisted":false}')
expect "$code" 200 "clear my settings"
expect_empty "$(json default_ttl_days)" "null cleared the TTL preference"
expect "$(json default_unlisted)" false "…and the visibility one"
code=$(upload OG small-640x360.mp4 '{"title":"E2E defaults cleared","ttl_days":1}')
expect "$code" 201 "upload OG ($(cat "$RESP"))"
OG=$(json id)
expect "$(json unlisted)" false "with no preference an upload is listed again"

# Everything this step made goes, once its encode is somewhere it can be deleted from.
own_wait_terminal() {   # own_wait_terminal <id> <label>
  local n=0 state
  while [ "$n" -lt 180 ]; do
    code=$(get "$BASE/api/clips/$1/progress" || echo 000)
    expect "$code" 200 "progress $2"
    state=$(json state)
    case $state in ready|failed) echo "[e2e]   $2 -> $state"; return 0;; esac
    n=$((n + 1)); sleep "$POLL_SLEEP"   # the pool's `poll`-bucket pacing, as wait_ready (was a flat 1 s)
  done
  die "$2 never reached a terminal state"
}
for pair in "OD:$OD" "OE2:$OE2" "OF:$OF" "OG:$OG"; do
  own_wait_terminal "${pair#*:}" "${pair%%:*}"
done
for pair in "OD:$OD" "OE2:$OE2" "OF:$OF" "OG:$OG"; do
  code=$(http DELETE "$BASE/api/clips/${pair#*:}" -b "$OW_JARW" || echo 000)
  echo "[e2e]   DELETE ${pair%%:*} ${pair#*:} -> $code"
done
echo "[e2e]   field > account default > DEFAULT_CLIP_TTL, on both upload routes"
code=$(http POST "$BASE/api/auth/logout" -b "$OW_JARW" || echo 000); echo "[e2e]   logout $OW_USER -> $code"
UPLOAD_JARW=

# ---------------------------------------------------------------------------------------------------
step 58 "the owner's own tally: their numbers, nobody else's, and never on a page a stranger can see"
code=$(get "$BASE/api/auth/me/stats" -b "$JARW"); expect "$code" 200 "GET my stats ($(cat "$RESP"))"
OWN_CLIPS=$(json clips); expect_nonempty "$OWN_CLIPS" "clips"
[ "$OWN_CLIPS" -ge 1 ] || die "the owner's tally says $OWN_CLIPS clips, and C exists"
expect_nonempty "$(json ready)" "ready"
expect_nonempty "$(json bytes_total)" "bytes_total"
expect_nonempty "$(json views_total)" "views_total"
contains "$(hdr cache-control)" no-store || die "the stats answer is cacheable: $(hdr cache-control)"
# It is the CALLER's tally. The second user's is its own, and neither can ask for the other's.
code=$(get "$BASE/api/auth/me/stats" -b "$JAROW"); expect "$code" 200 "the second user's stats"
OWN_THEIR_CLIPS=$(json clips)
[ "$OWN_THEIR_CLIPS" != "$OWN_CLIPS" ] || echo "[e2e]   NOTE: both accounts happen to own $OWN_CLIPS clips"
code=$(get "$BASE/api/auth/me/stats"); expect "$code" 401 "anonymous stats"
code=$(get "$BASE/api/auth/me/stats/somebodyelse" -b "$JARW"); expect "$code" 404 "there is no per-user stats route"
# The library page carries the strip; the share page carries the clip's stored size for the OWNER only;
# and neither number is ever in a card.
code=$(page "$BASE/" -b "$JARW"); expect "$code" 200 "the library page"
has_file "$PAGE" 'data-account-stats' "the library carries the owner's tally"
has_file "$PAGE" 'data-stats-views=' "…including their private view total"
code=$(page "$BASE/$C" -b "$JARW"); expect "$code" 200 "the owner's share page"
has_file "$PAGE" 'data-stored' "the owner sees what this clip stores"
code=$(page "$BASE/$C"); expect "$code" 200 "the anonymous share page"
lacks_file "$PAGE" 'data-stored' "a visitor must not see what a clip stores"
lacks_file "$PAGE" 'data-views' "…nor the view count (unchanged by any of this)"
code=$(page "$BASE/$C" -A "$DISCORD_UA"); expect "$code" 200 "the Discordbot card"
lacks_file "$PAGE" 'data-stored' "a card must never carry a size"
lacks_file "$PAGE" 'data-account-stats' "…nor an account tally"
echo "[e2e]   the tally is the caller's own, on their library and their clip pages alone"

# ---------------------------------------------------------------------------------------------------
step 59 "STORAGE_BUDGET_BYTES is a WARNING and not a cap unless STORAGE_BUDGET_HARD=1 (§20 erratum 277): over the line, and an upload still succeeds"
# The lane's own app keeps its configured budget (none) for the whole run — recreating it would destroy
# the log history step 16 reads. An instance of the step's OWN carries the budget instead (the header
# says why it no longer shares the live database): the same image, its own database, scratch, key prefix
# and STORAGE_DIR, STORAGE_BUDGET_BYTES=1, the per-tag webhooks step 60 needs, and a 3 s sweep — the
# budget reading is the sweeper's, taken at boot (an empty database, 0 bytes) and again every tick, so
# the instance is OVER its line once the step's first clip has landed and a tick has read it. That
# first clip is UNLISTED, so it is announced to nobody and step 60's notify count stays exact.
# Everything runs inside the container over its own loopback (89-quota's shape): nothing is published
# to the host and the probe's own http PUBLIC_BASE_URL is the one its cookies are minted for.
#
# Obviously-fake webhooks: a numeric id of zeros and a token that says what it is. They pass the §17 #4
# allowlist (that is the point — the app refuses anything else at boot) and could never be a real
# channel. OWN_TOKEN is what must not appear in ANY log line, error or row.
OWN_TOKEN="e2e-not-a-real-webhook-token"
OWN_HOOK_DEFAULT="https://discord.com/api/webhooks/100000000000000001/${OWN_TOKEN}-default"
OWN_HOOK_TAGGED="https://discord.com/api/webhooks/100000000000000002/${OWN_TOKEN}-tagged"
OWN_TAG="e2eroute"
OWN_PREFIX="e2e-owner-$(date +%s)/"
OWN_OUT="$CTX_DIR/owner-probe.txt"
own_rm_prefix() {
  store_has_bucket || return 0
  store_rm_prefix_ok_ "${OWN_PREFIX:-}" || return 0
  ( store_obj rm --prefix "$OWN_PREFIX" >/dev/null ) 2>/dev/null \
    || echo "[e2e]   NOTE: could not remove the budget probe's prefix $OWN_PREFIX (its own, outside the stack's key prefix)"
}
trap 'own_rm_prefix' EXIT
OWN_SH="$CTX_DIR/owner-probe.sh"
cat > "$OWN_SH" <<'OWNER_PROBE'
set -e
mkdir -p /tmp/own /tmp/own-scratch /tmp/own-store
clippet migrate >/dev/null
clippet serve >/tmp/serve.log 2>&1 &
SP=$!
i=0
while [ $i -lt 90 ]; do curl -fsS http://127.0.0.1:8080/readyz >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
echo "ready=$(curl -sS -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/readyz)"
ffmpeg -v error -y -f lavfi -i testsrc2=size=320x180:rate=15:duration=2 \
  -f lavfi -i sine=frequency=440:duration=2 \
  -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac -shortest /tmp/o.mp4
curl -sS -o /dev/null -c /tmp/jar -w "%{http_code}\n" -X POST -H "Content-Type: application/json" \
  -d "{\"username\":\"$OUSER\",\"password\":\"$OPASS\"}" http://127.0.0.1:8080/api/auth/register > /tmp/regcode
echo "register=$(tr -d "\r\n" < /tmp/regcode)"
oup() {   # oup <title> [extra -F…] → the status; the body lands in /tmp/up
  t=$1; shift
  curl -sS -o /tmp/up -w "%{http_code}" -b /tmp/jar -F "title=$t" -F "ttl_days=1" "$@" \
    -F duplicate=allow -F "file=@/tmp/o.mp4;type=video/mp4" http://127.0.0.1:8080/api/upload
}
oid() { grep -o "\"id\":\"[^\"]*\"" /tmp/up | head -1 | cut -d"\"" -f4; }
ostate() { curl -sS http://127.0.0.1:8080/api/clips/$1/progress | grep -o "\"state\":\"[a-z]*\"" | head -1 | cut -d"\"" -f4; }
oready() { i=0; while [ $i -lt 180 ]; do s=$(ostate "$1"); [ "$s" = ready ] && return 0; [ "$s" = failed ] && break; i=$((i+1)); sleep 1; done; echo "notready_$1=$s"; return 0; }
# The first clip, unlisted: what puts the instance over a one-byte line.
echo "seedup=$(oup "E2E budget seed" -F "unlisted=on")"
OS=$(oid); echo "OS=$OS"; oready "$OS"
# /healthz says so once a sweep tick has read it — polled, never assumed.
i=0
while [ $i -lt 60 ]; do
  curl -sS -o /tmp/hz http://127.0.0.1:8080/healthz
  grep -q "\"over\":true" /tmp/hz && break
  i=$((i+1)); sleep 1
done
echo "overafter=$i"
echo "=====healthz"; cat /tmp/hz; echo
echo "=====page"; curl -sS -b /tmp/jar http://127.0.0.1:8080/; echo
echo "=====end"
# …and an upload WHILE over still succeeds, which is the whole claim of step 59.
echo "OBup=$(oup "E2E over budget")"
OB=$(oid); echo "OB=$OB"; oready "$OB"
# Step 60: a TAGGED clip names the route; the untagged OB names the default.
echo "OTup=$(oup "E2E routed clip" -F "tags=$OTAG")"
OT=$(oid); echo "OT=$OT"; oready "$OT"
echo "=====dto"; curl -sS -b /tmp/jar http://127.0.0.1:8080/api/clips/$OT; echo
echo "=====end"
i=0
while [ $i -lt 90 ]; do
  grep -q "webhook=$OTAG" /tmp/serve.log && grep -q "clip=$OB" /tmp/serve.log && break
  i=$((i+1)); sleep 1
done
echo "notifywait=$i"
echo "=====adminls"; clippet admin ls "$OT" --stat 2>&1 || true
echo "=====statsbefore"; clippet admin stats --no-storage 2>&1 || true
echo "=====end"
for c in "$OS" "$OB" "$OT"; do
  echo "del_$c=$(curl -sS -o /dev/null -w "%{http_code}" -b /tmp/jar -X DELETE http://127.0.0.1:8080/api/clips/$c)"
done
echo "=====statsafter"; clippet admin stats --no-storage 2>&1 || true
echo "=====end"
kill $SP 2>/dev/null || true
wait $SP 2>/dev/null || true
echo "=====log"; cat /tmp/serve.log
echo "=====end"
OWNER_PROBE
OWN_RC=0
dc run --rm -T --no-deps \
  -e DATA_DIR=/tmp/own -e SCRATCH_DIR=/tmp/own-scratch -e STORAGE_DIR=/tmp/own-store \
  -e "S3_KEY_PREFIX=$OWN_PREFIX" \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 -e REGISTRATION=open -e SWEEP_INTERVAL=3s \
  -e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT= \
  -e STORAGE_BUDGET_BYTES=1 -e STORAGE_BUDGET_HARD= \
  -e "DISCORD_WEBHOOK_URL=$OWN_HOOK_DEFAULT" -e "DISCORD_WEBHOOK_ROUTES=$OWN_TAG=$OWN_HOOK_TAGGED" \
  -e DISCORD_APP_ID= -e DISCORD_PUBLIC_KEY= -e DISCORD_BOT_TOKEN= \
  -e "OUSER=${E2E_USER}_ob" -e "OPASS=$E2E_PASS" -e "OTAG=$OWN_TAG" \
  --entrypoint sh app -c "$(cat "$OWN_SH")" > "$OWN_OUT" 2>&1 || OWN_RC=$?
# The probe's sections, each into a file of its own (a section runs to the next `=====` line); the
# key=value lines OUTSIDE every section are read with ownv() and printed into the transcript.
own_section() { tr -d '\r' < "$OWN_OUT" | awk -v want="=====$1" '/^=====/ { on = ($0 == want); next } on { print }'; }
own_top_() { tr -d '\r' < "$OWN_OUT" | awk '/^=====end$/ { on = 0; next } /^=====/ { on = 1; next } !on { print }'; }
ownv() { own_top_ | sed -n "s/^$1=//p" | tail -1; }
own_top_ | grep -E '^[A-Za-z_][A-Za-z0-9_]*=' | sed -n 's/^/[e2e]   owner-probe: /p' || true
[ "$OWN_RC" = 0 ] || die "the budget/routing probe container failed (exit $OWN_RC):
$(cat "$OWN_OUT")"
expect "$(ownv ready)" 200 "the budget/routing instance became ready"
expect "$(ownv register)" 201 "register the probe's account"
expect "$(ownv seedup)" 201 "the seed upload (unlisted)"
expect_empty "$(ownv "notready_$(ownv OS)")" "the seed clip reached ready"

# /healthz reports the reading, and says it is over.
own_section healthz > "$CTX_DIR/owner-healthz.json"
OWN_HZW=$(W "$CTX_DIR/owner-healthz.json")
expect "$(jget "$OWN_HZW" storage_budget.over)" true "/healthz says the instance is over its budget (after $(ownv overafter) s)"
expect "$(jget "$OWN_HZW" storage_budget.budget)" 1 "/healthz reports the configured budget"
expect_nonempty "$(jget "$OWN_HZW" storage_budget.used)" "/healthz reports what is stored"
expect "$(jget "$OWN_HZW" ok)" true "a crossed budget is not a health problem"
# The lane's own app has no budget configured, so it reports none at all — an absent object is never
# read as 'you are fine'.
code=$(get "$BASE/healthz"); expect "$code" 200 "GET /healthz on the lane's own app"
expect_empty "$(json storage_budget.over)" "no STORAGE_BUDGET_BYTES, no storage_budget object"

# The library page carries the warning, and says in the sentence itself that nothing has stopped.
own_section page > "$CTX_DIR/owner-library.html"
has_file "$CTX_DIR/owner-library.html" 'data-storage-budget' "the library carries the budget warning"
has_file "$CTX_DIR/owner-library.html" 'uploads are never refused' "…and says so, in the warning itself"
code=$(page "$BASE/" -b "$JARW"); expect "$code" 200 "the lane's own library page"
lacks_file "$PAGE" 'data-storage-budget' "no budget, no warning"

# …and an upload still succeeds, which is the whole claim.
expect "$(ownv OBup)" 201 "an upload while over the budget"
OB=$(ownv OB); expect_nonempty "$OB" "OB id"
expect_empty "$(ownv "notready_$OB")" "OB (the untagged clip) reached ready"

# The operator's own view of the same number, through the CLI, with the budget on and off — read-only
# `admin stats` runs against the LIVE database (no serve, so no janitor and no worker).
out=$(dc run --rm -T --no-deps -e STORAGE_BUDGET_BYTES=1 app admin stats --no-storage 2>&1) \
  || die "admin stats (budget on) failed: $out"
contains "$out" 'budget=1' || die "admin stats does not report the budget: $out"
contains "$out" 'OVER' || die "admin stats does not say the instance is over it: $out"
contains "$out" 'no upload is ever refused' || die "admin stats does not say the budget is advisory: $out"
out=$(dc run --rm -T --no-deps app admin stats --no-storage 2>&1) || die "admin stats (budget off) failed: $out"
contains "$out" 'budget=off' || die "admin stats does not say the budget is off: $out"
echo "[e2e]   over budget: /healthz, the library and admin stats all say so, and the upload still landed"

# ---------------------------------------------------------------------------------------------------
step 60 "Discord per-tag routing: the job names the TAG it was routed to, and no URL is anywhere"
# The instance of step 59 carries the routes. A TAGGED clip must name the route; an untagged one must
# name the default; and the webhook token must appear nowhere at all. Its log is the only app log the
# notify jobs could have been written to: they are in its own database.
expect "$(ownv OTup)" 201 "the tagged upload"
OT=$(ownv OT); expect_nonempty "$OT" "OT id"
expect_empty "$(ownv "notready_$OT")" "OT (the tagged clip) reached ready"
own_section dto > "$CTX_DIR/owner-OT.json"
OWN_OTW=$(W "$CTX_DIR/owner-OT.json")
expect "$(jget "$OWN_OTW" tags.0)" "$OWN_TAG" "the tagged clip carries the route's tag"
expect "$(jget "$OWN_OTW" unlisted)" false "…and is LISTED, so it is announced at all"
OWN_LOG="$CTX_DIR/owner-notify.log"
own_section log > "$OWN_LOG"
has_file "$OWN_LOG" 'discord webhook route configured' "the boot line naming the route"
has_file "$OWN_LOG" "tag=$OWN_TAG" "…and the tag it routes"
grep -q "clip=$OT" "$OWN_LOG" || die "no notify line for the tagged clip $OT"
grep -Eq "clip=$OT .*webhook=$OWN_TAG|webhook=$OWN_TAG .*clip=$OT" "$OWN_LOG" \
  || die "the tagged clip's job did not name the route '$OWN_TAG': $(grep "clip=$OT" "$OWN_LOG" | head -3)"
grep -Eq "clip=$OB .*webhook=default|webhook=default .*clip=$OB" "$OWN_LOG" \
  || die "the UNTAGGED clip's job did not fall back to the default webhook: $(grep "clip=$OB" "$OWN_LOG" | head -3)"
# THE SECRET. Not one fragment of either webhook URL may be in any log line, nor in any other thing the
# probe printed — its DTO, its `admin ls --stat`, its `admin stats` (the whole transcript is checked).
for secret in "$OWN_TOKEN" "${OWN_TOKEN}-tagged" "${OWN_TOKEN}-default" "$OWN_HOOK_TAGGED" "$OWN_HOOK_DEFAULT"; do
  lacks_secret_f "$OWN_LOG" "$secret" "a webhook URL reached a log line"
done
lacks_secret_f "$CTX_DIR/owner-OT.json" "$OWN_TOKEN" "a webhook URL reached a DTO"
own_section adminls > "$CTX_DIR/owner-adminls.txt"
has_file "$CTX_DIR/owner-adminls.txt" "$OT" "admin ls names the tagged clip"
lacks_secret_f "$CTX_DIR/owner-adminls.txt" "$OWN_TOKEN" "a webhook URL reached 'admin ls --stat'"
own_section statsbefore > "$CTX_DIR/owner-stats.txt"
lacks_secret_f "$CTX_DIR/owner-stats.txt" "$OWN_TOKEN" "a webhook URL reached 'admin stats'"
# Two notify jobs exist — one per LISTED clip (the unlisted seed is announced to nobody) — and NONE was
# queued for anything else. The instance is the step's own, so the count is exact on every stack.
has_file "$CTX_DIR/owner-stats.txt" 'notify=2' "want two notify jobs (one per listed clip)"
echo "[e2e]   the tagged clip routed to '$OWN_TAG', the untagged one to 'default', no URL anywhere"

# Everything the pair of steps made goes, and the notify rows go with the clips (ON DELETE CASCADE).
for c in "$(ownv OS)" "$OB" "$OT"; do
  case $(ownv "del_$c") in 202|204) ;; *) die "DELETE $c on the probe -> $(ownv "del_$c")";; esac
done
own_section statsafter > "$CTX_DIR/owner-stats-after.txt"
has_file "$CTX_DIR/owner-stats-after.txt" 'notify=0' "the notify rows outlived their clips"
own_rm_prefix
trap - EXIT
code=$(http POST "$BASE/api/auth/logout" -b "$JAROW" || echo 000); echo "[e2e]   logout ${E2E_USER}_b -> $code"
rm -f "$JARO"
echo "[e2e]   budget/routing instance gone with its database; nothing of ours is left on the stack"

# This bundle's rows in the closing summary table (scripts/e2e.d/README.md).
summary_row OL "${OL:-}" "the 60 s oEmbed subject: a NOT-ready clip answers the link document, not the player; deleted"
summary_row OD "${OD:-}" "upload defaults (#22): silence took the ACCOUNT's 30 days and its unlisted tick; deleted"
summary_row OE2 "${OE2:-}" "the same route with explicit fields: they beat the account default; deleted"
summary_row OF "${OF:-}" "the capture route under the same account defaults; deleted"
summary_row OG "${OG:-}" "defaults cleared: the instance default is what silence means again; deleted"
summary_row OB "${OB:-}" "storage budget (its own instance): the warning is advisory, the upload over the line still succeeds; deleted"
summary_row OT "${OT:-}" "per-tag Discord routing (its own instance): the job named the TAG's webhook, never a URL; deleted"
