# API.md — Clippet HTTP API reference

> **Reconciled against the merged tree at commit `0010eed9` (2026-09-07).** The three lanes
> (A nits/B account security/G operator CLI, C library/E watch page/H owner extras, D
> media/F upload/I docs) are merged into `main`; this document was diffed route-for-route
> against `internal/httpapi/router.go` and every example below was re-run against the live
> stack. Nothing here is stale. `internal/httpapi/router.go` has grown since that count was
> taken — 45 `route(` call sites (including the two Discord bot ones registered only when
> the bot is configured) and 59 `http.Method*:` keys across their `byMethod` maps, as of
> this revision (reproducible with a plain-text `grep -c` on the file — the two counts drift
> together with the table below, not with the reconciliation date above).

This is a reference for someone writing a client against Clippet: a capture tool (ShareX,
RePlays, an OBS script), a browser-less script, or a second UI. It documents the wire
contract as it **is**, including every `DESIGN.md` §20 erratum that changed it — you do
not need to read the errata to use this API correctly, though the numbers are cited so you
can go find the reasoning if you want it.

For the architecture, the state machine, and *why* any of this is shaped the way it is,
see `DESIGN.md` §8 (API contract) and its §20 errata. For running the server, see
`DEPLOY.md`.

## Contents

- [Conventions](#conventions)
- [Authentication and sessions](#authentication-and-sessions)
- [Account settings and stats](#account-settings-and-stats)
- [Uploading a clip](#uploading-a-clip)
- [Clips](#clips)
- [Editing a clip in place: trim, replace, GIF, poster](#editing-a-clip-in-place-trim-replace-gif-poster)
- [Media](#media)
- [Views](#views)
- [Collections](#collections)
- [oEmbed](#oembed)
- [The Discord bot's interactions endpoint](#the-discord-bots-interactions-endpoint) (and the official bot's origin proof, `GET /api/bot/claim`)
- [Health](#health) (and the instance's self-description, `GET /api/instance`)
- [The setup wizard's store check](#the-setup-wizards-store-check)
- [Pages (HTML, not this reference's subject)](#pages)
- [The ClipDTO wire format](#the-clipdto-wire-format)
- [Rate limits](#rate-limits)
- [Two complete recipes](#two-complete-recipes)

## Conventions

**Base URL.** Every absolute URL in a response (`urls.*`, `url`, `embed_url`) is built from
the server's `PUBLIC_BASE_URL` at request time. The examples in this file were run against
the live stack: `http://localhost:8481` for an anonymous or Bearer-token request, and the
instance's https origin for a cookie-authenticated one (a session cookie is `Secure`, so it
never rides on a plain-http request — DESIGN.md §4). Substitute your own origin.

**JSON.** Every JSON response is `Content-Type: application/json; charset=utf-8`.
Timestamps are RFC3339 UTC strings (`"2026-09-07T06:41:19Z"`), never Unix numbers, on the
wire — the database stores Unix seconds, but nothing outside the process ever sees that.
A `null` in this document always means "the JSON literal `null`", never "the key is
missing" — the two are different and a few fields (`gif_url`, `filmstrip`, `poster`, `trim`) depend on
telling them apart; each field below says which it uses.

**Ids.** Every `{id}`/`{cid}`/`{tid}` path parameter is a 12-character id
(`[A-Za-z0-9]{12}`, ~71 bits of entropy — DESIGN.md §7) — **or**, for a CLIP whose upload
asked for the memorable grammar, three to four capitalised words joined
(`^(?:[A-Z][a-z]{2,8}){3,4}$`, ~39 or ~52 bits; DESIGN.md §20 errata 249-250, and see
`link_style` below). A malformed one (neither shape) is answered **404** immediately,
before any database access, identically to an id that is merely unknown — so a probe
cannot distinguish "not shaped like an id" from "not found". This one 404 has no
error-envelope surprises: it is always
`{"error":"not_found","message":"..."}`.

**The examples' cast.** Every id in this document reused across more than one example is the
same clip or collection throughout — `Z6eQucmYEyFQ` is the walkthrough clip, `tRtDPzBjICct`
its collection, `hk2fNeguypTA`/`G66oZ2D68OOP` the second clips a list or bulk example needs
alongside it, and `smW1BNY9aa23` the upload example's result — so a future regeneration of
this file has a continuity to keep rather than a fresh id to invent per paragraph. These ids
are illustrative; they exist on no instance you can reach.

**Examples never carry a real token, a real password, or an account that outlives the doc
run.** A `token`, `Authorization: Bearer …` value or `password` field you see below is
always a placeholder — either the recipes section's `ck_YOUR_TOKEN_HERE` for a value you are
meant to paste your own into, or a clearly-fake, shape-correct string (`ck_` + 43 base64url
characters) standing in for a 201's one-time reveal. Nothing in this document, run again,
authenticates against any instance.

That is a rule for the doc RUN as well as for the doc: a reconciliation pass that registers
throwaway accounts to re-verify these transcripts registers them with a passphrase it never
prints, and prunes them afterwards. The passphrases spelled out below — including the
`register` example's — therefore authenticate against nothing, and must stay that way; using
one for a live account would turn a published example into a working credential (nit sweep
S01).

**Error envelope.** Every error this API produces — including 404/405/413/429/5xx written
by the framework rather than a handler — is:

```json
{"error": "snake_case_code", "message": "a sentence a human can read"}
```

The `error` code is the stable thing to match on; `message` can change wording between
versions. Every error response in this document is written exactly as observed against the
live stack.

**Auth classes.** Every route below is tagged with one of:

| Tag | Meaning |
|---|---|
| **anonymous** | No credential of any kind. The unguessable link is the only gate where one exists. |
| **user** | A session cookie **or** an `Authorization: Bearer ck_…` token. A token additionally needs the right *scope* — `upload` to create clips, `read` to list them (§20 erratum 111) — and a token lacking it gets **403** `insufficient_scope` on a route gated by `RequireScope`, or **403** `token_not_allowed` on the two `session`-or-`read` routes below. |
| **session** | A session cookie only. A Bearer token on a session-only route gets **403** `token_not_allowed` (DESIGN.md §20 erratum 19) — a leaked capture-tool token can upload, and that is *all* it can do: it cannot list, read a private field, edit, delete, retry, trim, replace, choose a poster frame, manage collections, or touch the account (password, sessions, settings). |
| **session or `read` token** | `GET /api/clips` and `GET /api/collections` only (§20 erratum 111): a browser session, exactly as `session` above, **or** a Bearer token minted with the `read` scope — the shape that lets an owner's own backup/sync script list its own library without a cookie jar. An upload-only token still gets **403** `token_not_allowed`, the same as a plain `session` route. |
| **owner** | `session`, further scoped to the caller whose id equals the resource's `owner_id`. |

**A precedence worth knowing, because it is not what you would guess from "owner → 403
forbidden":** on every `session`/`owner` route the credential check runs as **middleware**,
*before* the handler ever looks up the resource. So a request with no credential, or one
carrying a Bearer token, gets its 401/403 straight away, with no database read at all — the
resource's existence is never consulted. Only once a caller **has** a valid session does the
handler run its own 404-then-403 check (unknown or tombstoned resource → 404; a different
owner's resource → 403).

**And one subtlety that decides which of 401/403 you get: a token the server cannot resolve
is not a failed credential, it is *no* credential.** An unknown, mistyped, revoked or
expired `Authorization: Bearer ck_…` value falls through the resolver exactly as if the
header had never been sent, so the middleware sees an anonymous request and answers **401** —
only a token the server can actually look up reaches the `token_not_allowed` branch. That
means the three commands below are *not* interchangeable: the second and third differ only
in the token value, and that is the whole difference between 401 and 403. (`$TOKEN` in case
3 must hold a **real** token — mint one with [`POST /api/auth/tokens`](#post-apiauthtokens--session-csrf)
and export it. Run case 3 with `$TOKEN` unset and the shell sends an empty Bearer value, so
you get case 2's 401 instead — which is the point, not a broken example.)

```sh
# 1. no credential at all: 401, whether or not the id is real
$ curl -s -X PATCH http://localhost:8481/api/clips/anything123 \
    -H 'Content-Type: application/json' -d '{"title":"x"}'
{"error":"unauthorized","message":"authentication required"}                          # 401

# 2. a Bearer value the server cannot resolve (typo, revoked, expired) — the
#    literal string below is NOT a real token, and the answer is identical to
#    case 1, because no principal was ever established
$ curl -s -X PATCH http://localhost:8481/api/clips/anything123 \
    -H 'Authorization: Bearer ck_notarealtoken' \
    -H 'Content-Type: application/json' -d '{"title":"x"}'
{"error":"unauthorized","message":"authentication required"}                          # 401

# 3. a REAL token, minted by POST /api/auth/tokens and put in $TOKEN (an upload
#    credential, not a session): 403. This is the only one of the three that
#    demonstrates the session-only rule.
$ curl -s -X PATCH http://localhost:8481/api/clips/anything123 \
    -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' -d '{"title":"x"}'
{"error":"token_not_allowed","message":"this endpoint requires a browser session"}    # 403
```

**Those two answers — 401 for no resolvable credential, 403 `token_not_allowed` for a real
Bearer token — are produced by the middleware on _every_ `session`/`owner` route in this
document**, whether or not that route's own `Errors:` line repeats them. Those lists
otherwise enumerate what the **handler** adds on top: the `403 forbidden` a valid session
that is not the owner gets, the 404s, the 409s, and the rest.

**CSRF** (DESIGN.md §14). Applies only to a **cookie**-authenticated mutating request
(`POST`/`PATCH`/`PUT`/`DELETE`). Rejected `403 csrf` iff the request carries an `Origin`
header that does not match the server's own origin, **or** a `Sec-Fetch-Site` header equal
to `cross-site`. A request with neither header — every example in this document, since
plain `curl` sends neither — passes. Bearer-authenticated requests skip the check
entirely (there is no cookie to ride along on a forged cross-site request). Every JSON
route additionally requires `Content-Type: application/json` on requests with a body
(**415** `unsupported_media_type` otherwise) — an HTML `<form>` cannot set that
cross-origin without a CORS preflight, so this alone already blocks a bare cross-site form
post; the explicit check is defense in depth for the fetch/XHR case.

**Rate limiting.** A limited route answers **429** `rate_limited` with `Retry-After:
<seconds>` when its bucket is empty. See [Rate limits](#rate-limits) for which route is
limited by what.

**Backend failures never look like 404.** A database outage is **503** `unavailable` +
`Retry-After: 5`; a storage (S3/R2/B2) outage on a route that needs storage is
**503** `storage_unavailable` + `Retry-After: 5`. Neither is ever answered as a 404 — a
share link that Discord or X has already cached must never look permanently gone because
of a transient backend hiccup.

## Authentication and sessions

### `POST /api/auth/register` — anonymous (rate: `auth`, 10/min/IP)

```json
{"username": "alice", "password": "correct horse battery staple"}
```
`username`: `^[A-Za-z0-9_]{3,32}$`. `password`: 8–72 **bytes** (bcrypt's own truncation
limit). On success, **201** with the user and a `Set-Cookie` for the session — registering
logs you in.

```sh
$ curl -s -c cookies.txt -X POST https://clips.example.com/api/auth/register \
    -H 'Content-Type: application/json' \
    -d '{"username":"rc_apiwalk","password":"correct horse battery staple"}'
{"user":{"id":"X3Jk3t6SgznF","username":"rc_apiwalk","created_at":"2026-09-07T10:37:11Z"}}
```

Errors: **400** `invalid_request` (username or password shape); **403**
`registration_closed` — `REGISTRATION` is `closed`, or it is `first` (the default) and an
account already exists (§20 erratum 106); **409** `username_taken`; **415**
`unsupported_media_type` (non-JSON body); **500** `id_collision` (astronomically unlikely —
could not allocate a user id after retrying; auth_handlers.go).

### `POST /api/auth/login` — anonymous (rate: `auth`, 10/min/IP)

Same body shape. **200** with the user and `Set-Cookie` on success. **401**
`invalid_credentials` for both "no such user" and "wrong password" — the server runs a
dummy bcrypt compare on the unknown-user path so the two cases cost the same wall-clock
time and cannot be told apart by timing.

Two rejections that look like the same mistake and are not — worth telling apart, because
they are the two ways a JSON route can refuse a body and they come from different places:

```sh
# no Content-Type of our own, so curl sends its default
# (application/x-www-form-urlencoded): the media-type gate fires FIRST, before
# anything has tried to decode the bytes
$ curl -s -X POST http://localhost:8481/api/auth/login -d 'username=x&password=y'
{"error":"unsupported_media_type","message":"Content-Type must be application/json"}  # 415

# the header says JSON but the body is not: the media-type gate passes and the
# DECODER behind it is what rejects the request
$ curl -s -X POST http://localhost:8481/api/auth/login -H 'Content-Type: application/json' \
    -d 'username=x&password=y'
{"error":"invalid_request","message":"body must be a JSON object"}                    # 400
```
So a client that sets the header correctly can never see the first one: **415** means "your
`Content-Type` is wrong", **400** `invalid_request` means "your bytes are not a JSON object"
(a truncated body, a top-level array or number, or form syntax like the above), and the same
split holds on every JSON route in this document.

Errors: **400** `invalid_request` (malformed body, or a missing `username`/`password` key —
`{"error":"invalid_request","message":"username and password are required"}`); **401**
`invalid_credentials`; **415** `unsupported_media_type`; **429** `rate_limited`.

### `POST /api/auth/logout` — session (CSRF)

No body. **204**, cookie cleared, session row deleted. **401**; **403**
(`token_not_allowed` — there is no session for a Bearer token to end — or `csrf`).

### `GET /api/auth/me` — user

**200** `{"user": UserDTO, "via": "session"|"token"}` — `via` is how *this particular
request* authenticated, which is how a client tells whether it is looking at a browser
session or a capture token.

### `POST /api/auth/tokens` — session (CSRF)

```json
{"name": "ShareX on desktop", "scopes"?: ["upload", "read"]}
```
`name`: 1–64 characters after trimming. `scopes` is optional (§20 erratum 111): **absent
or `null` means `["upload"]`** — the shape every capture tool and every client written
before scopes existed already sends — and an **empty array is a 400**, not "no
permissions": a token that can do nothing is a mistake, not a request. Any other value
must be a subset of `upload`, `read`; duplicates collapse and the response always renders
the set in canonical order (`upload` before `read`) however the request sent them.

`"scopes": ["inbound"]`, alone, mints an inbound URL instead of a token: `token` is `ci_…`, `url` is the whole address to POST a file to (`POST /in/{token}`), and `sxcu` is null. `"scopes": ["autofetch"]`, alone, mints a Clippet Auto URL the same way: `token` is `ca_…` and `url` the whole address (`POST /in/auto/{token}`; DESIGN.md §20 erratum 324). `inbound` or `autofetch` together with any other scope is a 400. `url` is absent for every other token.

**201**, and the raw token is shown **exactly once** — there is no route that will ever
show it again, only its `prefix` (first 8 characters, for display). The response also
carries `sxcu`, a ready-to-import [ShareX custom uploader](https://getsharex.com/)
document (DESIGN.md §20 erratum 59) that already points at `POST /api/upload` with the
token pre-filled as a Bearer header — **and is `null` for a token without the `upload`
scope** (§20 erratum 111): a read-only token importing as an upload destination would
fail on the first screenshot with nothing to explain why.

```sh
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/auth/tokens \
    -H 'Content-Type: application/json' -d '{"name":"ShareX on desktop"}'
{"id":"nJhrKiMW4UZg","name":"ShareX on desktop","prefix":"EXAMPLEU","scopes":["upload"],
 "created_at":"2026-09-07T10:37:11Z","last_used_at":null,
 "token":"ck_EXAMPLEUPLOADTOKENnotARealSecret01234567890",
 "sxcu":{"Version":"14.1.0","Name":"Clippet","DestinationType":"FileUploader",
   "RequestMethod":"POST","RequestURL":"https://clips.example.com/api/upload",
   "Headers":{"Authorization":"Bearer ck_EXAMPLEUPLOADTOKENnotARealSecret01234567890"},
   "Body":"MultipartFormData","FileFormName":"file","URL":"{json:url}",
   "ErrorMessage":"{json:message}"}}
# "token" above is a placeholder, not a real value — your own POST here gets
# a genuine one, shown to you exactly once and never again after that.

# a read-only token for a backup/sync script: sxcu is null, and this token
# opens exactly two routes — GET /api/clips and GET /api/collections — that a
# plain upload token gets 403 token_not_allowed on
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/auth/tokens \
    -H 'Content-Type: application/json' -d '{"name":"Backup script","scopes":["read"]}'
{"id":"oi3Let511tEo","name":"Backup script","prefix":"EXAMPLER","scopes":["read"],
 "created_at":"2026-09-07T10:27:51Z","last_used_at":null,
 "token":"ck_EXAMPLEREADTOKENnotARealSecretABCDEFGHIJKLM","sxcu":null}
# same placeholder rule as above: never a real token in this document.

$ curl -s -b cookies.txt -X POST https://clips.example.com/api/auth/tokens \
    -H 'Content-Type: application/json' -d '{"name":"Bad","scopes":["admin"]}'
{"error":"invalid_request","message":"scopes must be a non-empty subset of upload, read; or inbound alone; or autofetch alone"}
```

A token's own shape: `ck_` + 43 base64url characters. `upload` lets it create clips and
nothing an owner route protects (see the precedence note above); `read` lets it read
`GET /api/clips` and `GET /api/collections` (both under [Clips](#clips) and
[Collections](#collections) below) — including the owner-only fields those routes carry
for the caller's own clips — and nothing else. A token can carry both.

### `GET /api/auth/tokens` — session

**200** `{"tokens": [TokenDTO, ...]}`, newest first, revoked tokens omitted. `scopes` is on
every entry (§20 erratum 111). Never carries a raw token value.

```sh
$ curl -s -b cookies.txt https://clips.example.com/api/auth/tokens
{"tokens":[{"id":"nJhrKiMW4UZg","name":"ShareX on desktop","prefix":"EXAMPLEU",
 "scopes":["upload"],"created_at":"2026-09-07T10:37:11Z",
 "last_used_at":"2026-09-07T10:37:11Z"}]}
```

### `DELETE /api/auth/tokens/{tid}` — session (CSRF)

Revokes (sets `revoked_at`; the row is kept for audit, `GetTokenByHash` just stops
matching it). **204**. **404** `not_found` for an unknown or already-foreign token id — a
caller cannot revoke someone else's token, and the 404 does not distinguish "not yours"
from "does not exist". **401**; **403** (`token_not_allowed` or `csrf`) — a token cannot
revoke itself or any other, exactly like every other session route.

### `PATCH /api/auth/password` — session (CSRF, rate: `auth`, 10/min/IP)

```json
{"current_password": "correct horse battery staple", "new_password": "a new one, 8-72 bytes"}
```
Rotates the bcrypt hash and, in the **same transaction**, deletes every **other** session
of the caller (§20 erratum 108) — the incident response for "my laptop was stolen": change
the password from any other browser and the stolen cookie stops resolving on its very next
request. The current password is re-verified even though the caller already holds a live
session — a session proves "this browser logged in once", not "this is still the account's
owner" — so an unlocked laptop cannot silently become a permanent takeover. Held by the
**`auth`** limiter, not `upload`: this route runs a bcrypt compare, so it belongs in the
same bucket as login and register rather than a per-user budget a thief holding the cookie
already owns.

**200** `{"sessions_revoked": n}` — a count is the only honest receipt: the caller cannot
see the sessions it just ended, and the number is what tells them the stolen browser was
among them.

```sh
$ curl -s -b cookies.txt -X PATCH https://clips.example.com/api/auth/password \
    -H 'Content-Type: application/json' \
    -d '{"current_password":"wrong","new_password":"newpassword123"}'
{"error":"invalid_credentials","message":"the current password is wrong"}             # 401

$ curl -s -b cookies.txt -X PATCH https://clips.example.com/api/auth/password \
    -H 'Content-Type: application/json' \
    -d '{"current_password":"correct horse battery staple","new_password":"newpassword123"}'
{"sessions_revoked":1}
```

Errors: **400** `invalid_request` (either field empty, `new_password` outside 8–72 bytes,
or equal to the current one — rotating to the same secret would still log every other
browser out, which is a lot of damage for a no-op the caller did not mean); **401**
`unauthorized`; **401** `invalid_credentials` (wrong `current_password`); **403**
(`token_not_allowed` or `csrf`); **415**; **429** `rate_limited`.

### `GET /api/auth/sessions` — session

**200** `{"sessions": [SessionDTO, ...]}` — the caller's **live** sessions, newest first,
`private, no-store` + `Vary: Cookie, Authorization` (`privateNoStore`, router.go:153; §20 erratum 107).
There is deliberately **no id and
no handle of any kind** on a session: its only identity is the hex SHA-256 of its cookie,
which never leaves `internal/auth` — a per-session revoke would need a name for it that a
page (and therefore an attacker's page) could then send. `current` is the only thing a
reader has to match against the browser in front of them.

```sh
$ curl -s -b cookies.txt https://clips.example.com/api/auth/sessions
{"sessions":[{"created_at":"2026-09-07T10:27:30Z","last_seen_at":"2026-09-07T10:27:30Z",
 "expires_at":"2026-10-07T10:27:30Z","current":true,"user_agent":"curl/8.18.0"}]}
```

Errors: **401**; **403** `token_not_allowed`.

### `DELETE /api/auth/sessions` — session (CSRF)

"Log out everywhere but here" (§20 erratum 107): every session of the caller **except**
the one making the request. The caller's own cookie is untouched, which is the difference
between this and `POST /api/auth/logout` and the reason it is safe to click from the phone
you are holding right now.

**200** `{"sessions_revoked": n}` (the same shape `PATCH /api/auth/password` answers with).

```sh
$ curl -s -b cookies.txt -X DELETE https://clips.example.com/api/auth/sessions
{"sessions_revoked":1}
```

Errors: **401**; **403** (`token_not_allowed` or `csrf`).

### `GET /api/auth/discord/start?next=` — anonymous, a navigation (rate: `auth`, 10/min/IP)

Sign in with Discord (DEPLOY.md §19.6; DESIGN.md §20 erratum 302). Exists only when
`DISCORD_LOGIN_CLIENT_ID`/`_SECRET` are set — otherwise the router's plain **404** `not_found`, and
`/login` shows no button. Sets the flow cookie (the session cookie's name + `_discord`; `HttpOnly;
SameSite=Lax; Max-Age=600`, `Secure` on https; its value carries a MAC only this instance can produce,
so a hand-built or altered one is `expired` at the callback) and answers **302** to Discord's authorize URL
(`scope=identify`, PKCE S256, a `state` derived from the cookie). A caller with a session gets
**303** `next` instead; a request carrying `Sec-Fetch-Site: cross-site` gets **303** `/login`. `next`
is held to the same rule as `/login?next=`.

### `GET /api/auth/discord/callback?code&state[&error]` — anonymous, a navigation (rate: `auth`)

Where Discord sends the browser back. Never JSON: every outcome is a **303**. The flow cookie is
cleared first (single-use), then: no cookie / wrong `state` / no `code` → `/login?discord=expired`;
`error=access_denied` → `?discord=denied`; the exchange failed → `?discord=failed`; the Discord user is
linked to nobody here → `/login?discord=unlinked` — **no account is created, under any
`REGISTRATION`**; linked → a session cookie exactly as `POST /api/auth/login` issues one, then `next`.
In link mode (a cookie minted by the POST below) the landing page is `/settings?discord=linked|taken`.

### `POST /api/auth/discord` — session (CSRF, rate: `auth`)

```json
{"current_password": "correct horse battery staple"}
```
Mint a **link** flow for the caller's own account. The password is re-verified for the same reason
`PATCH /api/auth/password` re-verifies it. **200** `{"url": "<Discord authorize URL>"}` — the page
navigates there (a script navigation, not a form). Errors: **400** `invalid_request`; **401**
`invalid_credentials`; **403** `token_not_allowed` / `csrf`; **409** `discord_linked` (unlink first);
**415**.

```sh
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/auth/discord \
    -H 'Content-Type: application/json' -d '{"current_password":"correct horse battery staple"}'
{"url":"https://discord.com/oauth2/authorize?client_id=…&code_challenge=…&code_challenge_method=S256&redirect_uri=…&response_type=code&scope=identify&state=…"}
```

### `DELETE /api/auth/discord` — session (CSRF)

Unlink. No password (it removes a capability); no session is touched. **204**; **404** `not_linked`;
**401**; **403** `token_not_allowed` / `csrf`.

## Account settings and stats

Two owner-only, `session`-only routes (ROADMAP #22, #23; §20 errata 155, 156) — a leaked
capture token can upload, and that is all it can do; it certainly cannot read how much an
account has stored or change what its future uploads default to. Both answer for the
**caller** and there is no `{user}` form of either, and never will be: private view counts
are the ceiling, and an aggregate of somebody else's clips is over it.

### `GET /api/auth/me/settings` — session

**200**:
```json
{"default_ttl_days": null, "default_unlisted": false,
 "default_link_style": "compact", "instance_default_ttl_days": 0}
```
`default_ttl_days` carries a **meaningful `null`**: `null` is "no preference — follow
whatever `DEFAULT_CLIP_TTL` is", while `0` is "this account has chosen keep-forever" —
different answers on an instance whose `DEFAULT_CLIP_TTL` is not `never`, so they are
different values on the wire. `instance_default_ttl_days` says what the `null` case
resolves to right now, so a settings page can label the option honestly ("server default
· 7 days") without a second request.

`default_link_style` is the opposite shape on purpose: `compact` | `memorable`, two states
and **no null** (DESIGN.md §20 erratum 250). There is no `DEFAULT_LINK_STYLE` for a null to
defer to, so "no preference" and "compact" would be the same answer if there were one.
`private, no-store`.

```sh
$ curl -s -b cookies.txt https://clips.example.com/api/auth/me/settings
{"default_ttl_days":null,"default_unlisted":false,"default_link_style":"compact","instance_default_ttl_days":0}
```

Errors: **401**; **403** `token_not_allowed`.

### `PATCH /api/auth/me/settings` — session (CSRF)

```json
{"default_ttl_days"?: null|0|1|7|30|90, "default_unlisted"?: true|false,
 "default_link_style"?: "compact"|"memorable"}
```
All three fields optional; an **absent** key leaves that preference alone, an explicit
`null` on `default_ttl_days` **clears** it back to "follow the instance" (a `null`
`default_link_style` reads as absent — there is no third state for it to mean). **A default
is not a permission**: nothing here can make an upload do something an explicit field did not ask
for — the resolution order is field sent > user default > instance default, in that order,
in one place (§20 erratum 155), so a capture tool that names its own `ttl_days` keeps
working exactly as it did before this route existed.

**200** the same body `GET` returns.

```sh
$ curl -s -b cookies.txt -X PATCH https://clips.example.com/api/auth/me/settings \
    -H 'Content-Type: application/json' -d '{"default_ttl_days":7,"default_unlisted":true}'
{"default_ttl_days":7,"default_unlisted":true,"default_link_style":"compact","instance_default_ttl_days":0}

$ curl -s -b cookies.txt -X PATCH https://clips.example.com/api/auth/me/settings \
    -H 'Content-Type: application/json' -d '{"default_ttl_days":null}'
{"default_ttl_days":null,"default_unlisted":true,"default_link_style":"compact","instance_default_ttl_days":0}

$ curl -s -b cookies.txt -X PATCH https://clips.example.com/api/auth/me/settings \
    -H 'Content-Type: application/json' -d '{"default_link_style":"memorable"}'
{"default_ttl_days":null,"default_unlisted":true,"default_link_style":"memorable","instance_default_ttl_days":0}
```

Errors: **400** `invalid_ttl` (outside the closed set, or not a number); **400**
`invalid_link_style` (anything but `compact` or `memorable`); **401**; **403**
(`token_not_allowed` or `csrf`); **415**.

### `GET /api/auth/me/stats` — session

**200**:
```json
{"clips","ready","collections","bytes_source","bytes_renditions","bytes_total",
 "views_total","expiring_7d"}
```
The caller's own tally, derived at read time, tombstones excluded. Deliberately dull —
no rank, no comparison, no "top clip", no other account's number anywhere: what it answers
is the question an operator of a self-hosted box actually asks — how much is in here, how
much of it is about to expire, how much space am I paying for. `private, no-store`.

```sh
$ curl -s -b cookies.txt https://clips.example.com/api/auth/me/stats
{"clips":1,"ready":1,"collections":0,"bytes_source":1923666,"bytes_renditions":1761987,
 "bytes_total":3685653,"views_total":0,"expiring_7d":1}
```

Errors: **401**; **403** `token_not_allowed`.

## Uploading a clip

There are four ways to create a clip, and they exist for four different callers. All
four share one ingestion core (`beginUpload` + `ingest` in `internal/httpapi/upload.go`):
the multipart body is bounded (`MAX_UPLOAD_BYTES` + 1 MiB of framing headroom), spooled to
a scratch disk, passed through an `ffprobe` gate (**415** `not_a_video` for anything that is
neither a real video stream nor one sound Clippet can play — nothing unplayable ever reaches the
bucket), and only then
uploaded and committed in one transaction. **A 201/202 from any of the four means the
original file is already durable in your bucket** — there is no `uploading` clip state to
poll past (DESIGN.md §1).

| | Caller | Credential | Body |
|---|---|---|---|
| `POST /api/clips` | A browser with a progress bar | session | metadata-part-first multipart |
| `POST /api/uploads` … | The same browser, for a file larger than one chunk | session (or an `upload` token) | JSON to open, then the file in chunks, then a finish |
| `POST /api/upload` | A capture tool (ShareX, RePlays, OBS scripts) | Bearer token or session | simple fields, file last |
| `POST /api/share` | A phone's OS share sheet | session only | same grammar as `/api/upload`; answers with a redirect, not JSON |
| `POST /in/{token}` | A tool that can only POST a file to a URL | the URL itself | `/api/upload`'s grammar; answers the link as text |
| `POST /in/auto/{token}` | Clippet Auto, one press | the URL itself | a JSON instruction; the instance fetches the video from Discord |

**A clip can be sound** (DESIGN.md §20 erratum 328). A file with no picture — no video stream but
an attached cover — and ONE audio stream in an allowlisted container (MP3, WAV/W64, FLAC, AIFF, CAF,
Ogg, MP4/M4A/MOV, Matroska/WebM, WavPack, TTA, ADTS AAC, AC-3, E-AC-3, AMR, ASF/WMA) is an `audio`
clip. What is stored is never the upload itself but its STRIP: a `-c copy` of the one audio stream into
a fresh container with every tag, chapter, cover picture and encoder string left behind (bit-exact
for PCM, proven by a decoded-sample hash before the bytes are stored); the container the strip wrote
decides the stored extension and media type (an audio-only `.mov` is stored as an `.m4a`,
`audio/mp4`; Matroska/WebM is never stored as one — Opus/Vorbis move into an `.ogg`, FLAC into a
`.flac`, ALAC into an `.m4a`, PCM into a `.wav` or `.aiff`, and any other codec in Matroska is the 415). Refused with the same **415** `not_a_video`: DSD (`.dsf`/`.dff`), Monkey's Audio
(`.ape`, which no copy can strip), two or more audio tracks, more than eight channels, no duration.
The part's media type is a first opinion only — `audio/*` is read as well as `video/*` — and the
probe decides. A duplicate (below) is matched on the strip's hash, which is reproducible.

**A duplicate answers the existing clip** (DESIGN.md §20 erratum 326). When the ACCOUNT the
upload lands in already holds a live clip (`queued`, `processing` or `ready`, not past its expiry)
whose original is byte for byte the same file, nothing is created — no clip, no object, no job,
no webhook — and every route answers **200** (never 201) with THAT clip and `"existing": true`
(`POST /api/clips`: its `ClipDTO`; `/api/upload`, `/in/{token}` and `/in/auto/{token}`: its
`UploadDTO`; `/in/{token}` as text: its link; `/api/share`: a 303 to its page). The request's
own fields (title, description, unlisted, TTL, tags) are not applied to it. `existing` is absent
from every other answer. The check is per account only — another account's identical file is
its own clip — and the whole file is still sent (it is hashed as it arrives), so a duplicate
saves storage and the transcode, not the transfer, and spends the upload limiter as any upload
does. **The opt-out is `duplicate=allow`**: a text field before the file on the `/api/upload`
grammar, ONE text part between `metadata` and `video` on `POST /api/clips`, and
`"duplicate": "allow"` in the `/in/auto/{token}` instruction. Only `allow` opts out; an empty
field is "not sent" and leaves the check on; any other value is **400** `invalid_request`
(`duplicate must be allow` on the two multipart grammars), before the file is read.

### `POST /api/clips` — user (rate: `upload`, both buckets — see [Rate limits](#rate-limits))

Streaming multipart with **exactly two parts, in order**: part 1 named `metadata` (JSON),
part 2 named `video` (the file, with a filename). One text part named `duplicate` may come
between them — the duplicate check's opt-out, `duplicate=allow` (see above); any other part
there is `missing_video`. This grammar exists because the browser
uploader wants to validate metadata before it starts streaming a multi-gigabyte file — a
plain HTML `<form>` cannot honor "metadata first", which is why `/api/upload` exists with
a more form-friendly grammar (below).

Metadata part: `{"title"?, "description"?, "unlisted"?, "ttl_days"?, "tags"?,
"link_style"?}` — every field optional. `title` defaults to the filename with its extension
stripped — or, when the name carries a capture tool's date and clock (`2025-06-24 00-23-54`,
`Arc Raiders 2026.01.06 - 00.15.37.07`), to that date and time as `2025-06-24 00:23` with any text
before it kept (§20 erratum 317); `ttl_days`/`tags` follow the rules in
[the ClipDTO wire format](#the-clipdto-wire-format). `link_style` is `compact` (twelve
base62 characters) or `memorable` (three capitalised words, **four** when the upload is also
`unlisted`) — absent means the ACCOUNT's `default_link_style`, and the id it picks is
permanent (DESIGN.md §20 errata 249-251).

**201** `ClipDTO` with `"state":"queued"` on success (the transcode is queued; poll
`progress` or the DTO's own `state` for the rest). **200** `ClipDTO` with `"existing": true`
when the account already holds a live clip of exactly these bytes — that clip, unchanged
(§20 erratum 326).

```sh
$ printf '{"title":"Standing 720p clip","ttl_days":1,"tags":["gaming","clutch"]}' > meta.json
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/clips \
    -F 'metadata=<meta.json;type=application/json' \
    -F 'video=@clip.mp4;type=video/mp4'
{"id":"Z6eQucmYEyFQ","title":"Standing 720p clip","description":"","unlisted":false,
 "state":"queued","failure_reason":null,"views":0,"tags":["clutch","gaming"], ...}
```

Errors: **400** `metadata_first` (part 1 was not named `metadata`), `invalid_request`
(malformed JSON or a text-field length/control-character violation), `invalid_ttl`,
`invalid_link_style`, `invalid_tags`, `missing_video` (no video part, or it has no filename), `empty_video`
(zero bytes); **403** `insufficient_scope` (a real Bearer token lacking the `upload`
scope — a `read`-only token, for instance); **413** `too_large`; **415** `not_multipart`
/ `not_a_video`; **507** `insufficient_scratch` (the scratch disk precheck failed,
DESIGN.md §8.3) or `storage_quota` (this instance is at the storage line its operator set and
`STORAGE_BUDGET_HARD=1` — §20 erratum 277, DEPLOY.md §25; it cannot occur on an instance that
has not armed the cap, which is the default); **503** `storage_unavailable` / `unavailable`;
**500** `id_collision` (astronomically unlikely).

```sh
$ curl -s -X POST http://localhost:8481/api/clips -H "Authorization: Bearer $READ_TOKEN" \
    -F 'metadata=<meta.json;type=application/json' -F 'video=@clip.mp4;type=video/mp4'
{"error":"insufficient_scope","message":"token lacks the upload scope"}
```

### The chunked upload: `POST /api/uploads` and its session (DESIGN.md §20 erratum 344)

One file as a run of requests, each under the per-request body cap of whatever edge sits in
front of the instance — Cloudflare refuses any one request body over **100 MB** on its Free and
Pro plans, before the request reaches a Worker or the tunnel, so a one-shot `POST /api/clips` of a
2 GB file to a Pass name never arrives. The web uploader sends a file larger than ONE chunk this
way and every other file one-shot. The finish runs **the same ingest** `POST /api/clips` runs — the
duplicate check, the quota, the scratch check, the probe, the audio strip, the store, the
transaction — and answers what it answers. The chunk size is the server's word
(`UPLOAD_CHUNK_BYTES`, default 52428800 = 50 MiB; DEPLOY.md §7).

| Route | Credential | Answers |
|---|---|---|
| `POST /api/uploads` | user (CSRF, rate: `upload`, both buckets — one upload is one token, however many chunks) | **201** the session |
| `PUT /api/uploads/{uid}/{index}` | the session's account (CSRF) | **200** the session |
| `POST /api/uploads/{uid}/finish` | the session's account (CSRF) | **201** `ClipDTO`, or **200** `ClipDTO` + `"existing": true` |
| `GET /api/uploads/{uid}` | the session's account | **200** the session (and `clip` once finished) |
| `DELETE /api/uploads/{uid}` | the session's account (CSRF) | **204** |

**The session** is `{"id", "size", "chunk_bytes", "received", "finishing"?, "clip"?}`: `id` is 32
lowercase hex characters; `received` is how many chunks are in — the next one to send is that
index; `finishing` is `true` while a finish runs; `clip` is the finished upload's `ClipDTO` (with
`existing` when it was the account's existing clip). Another account's session, an unknown id and
an expired one are all **404** `not_found` (`no such upload`). A session lives **24 hours** from its
open, finished or not; the sweeper removes it (and its bytes) after that.

**Open** (`Content-Type: application/json`, ≤ 16 KiB): `{"size", "name"?, "type"?, "duplicate"?}`
plus the metadata document of `POST /api/clips` (`title`, `description`, `unlisted`, `ttl_days`,
`tags`, `link_style`) in the same object. `size` is the file's exact byte count; `name` and `type`
are what the multipart part's filename and media type would have been. Every gate that can be
asked before the bytes is asked here: **400** `invalid_request` (`size must be a positive integer`,
a text field, `duplicate must be allow`), `invalid_ttl`, `invalid_link_style`, `invalid_tags`;
**413** `too_large` (`size` over `MAX_UPLOAD_BYTES`); **415** `unsupported_media_type`; **507**
`insufficient_scratch` (1.5 × `size` + `SCRATCH_MIN_FREE_BYTES` is not free) or `storage_quota`;
**429** from either upload bucket.

**A chunk** is the body of `PUT /api/uploads/{uid}/{index}`, any media type, and its length is
fixed: `chunk_bytes`, the last chunk the remainder (`size − index × chunk_bytes`). `Content-Length`
is required (**411** `length_required`, `Content-Length is required`) and must be that length
(**400** `invalid_request`, `chunk length does not match`). `{index}` is a decimal with no leading
zero; one past the last chunk is **400** (`chunk index out of range`). Chunks go **in order**: a
chunk already received is **200** and its body is never read (a retry of a lost answer); one past
`received` is **409** `out_of_order` (`chunk out of order`) with `received`, `chunk_bytes` and
`size` in the body. **507** `insufficient_scratch` when the chunk would leave less than
`SCRATCH_MIN_FREE_BYTES` free; **400** `upload_interrupted` when the body ended early (`received`
is unchanged, so the same chunk is simply sent again).

**Finish** takes no body. Before the last chunk: **409** `incomplete` (`upload is missing chunks`,
with `received`). Then exactly `POST /api/clips`' answers and errors (above). The ingest runs to its
end even if the client goes away — a 4 GiB file's probe and store can outlast an edge's wait for an
answer — so a client whose finish answer was lost asks `GET` until `clip` appears; a second finish
waits for the first and answers the same clip. A refused finish (a 415, a quota) keeps the session
and its bytes until it is deleted or expires; a transient one (**503**) can simply be sent again.
The web page's own client asks for ten minutes (200 GETs, 3 s apart) and then gives up; every way it
gives up — a refused chunk, three lost chunks in a row, a refused finish, a finish never answered —
DELETEs the session (DESIGN.md §20 erratum 344 (k)).

**Delete** abandons the session and its bytes. While a finish is running it answers **204** at once
and the session goes when the finish returns — the clip that finish makes is made, exactly as a
one-shot upload cancelled after its last byte.

```sh
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/uploads -H 'Content-Type: application/json' \
    -d '{"size":157286400,"name":"match.mp4","type":"video/mp4","title":"Match point"}'
{"id":"9f2c1e0b7a6d4c3e8f1a2b3c4d5e6f70","size":157286400,"chunk_bytes":52428800,"received":0}
$ split -b 52428800 -d match.mp4 part.
$ for i in 0 1 2; do curl -s -b cookies.txt -T part.0$i \
    https://clips.example.com/api/uploads/9f2c1e0b7a6d4c3e8f1a2b3c4d5e6f70/$i; done
{"id":"9f2c1e0b7a6d4c3e8f1a2b3c4d5e6f70","size":157286400,"chunk_bytes":52428800,"received":1}
...
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/uploads/9f2c1e0b7a6d4c3e8f1a2b3c4d5e6f70/finish
{"id":"Z6eQucmYEyFQ","title":"Match point", ... ,"state":"queued", ...}
```

### `POST /api/upload` — user, Bearer **or** session (rate: `upload`, both buckets)

The capture-tool endpoint (DESIGN.md §20 erratum 58). Multipart with simple text fields
— `title?`, `description?`, `ttl_days?`, `unlisted?`, `tags?`, `link_style?`, `duplicate?`
(`allow` is the duplicate check's opt-out, above; erratum 326) (and `wait?`,
which is now refused — below) — **in any order**,
followed by the file part, named **`file` or `video`** (both are accepted so a
`clippet.sxcu` import and a hand-built form both work; `file` is what the minted `.sxcu`
uses). A field after the file part is never read — the handler stops at the file, streams
it, and answers.

Field notes that differ from the JSON metadata part:
- `ttl_days`, `unlisted`, `tags` are **strings**, not JSON types: `unlisted` accepts
  `true/false/1/0/on/off/yes/no/y/n` (case-insensitive); `tags` is comma-separated
  (`"gaming, clutch"`), not a JSON array.
- `link_style` is `compact` or `memorable`, and every capture field is **trimmed** — so
  `" memorable "` is accepted here and refused as JSON. Absent (which is what the shipped
  `clippet.sxcu` sends: it has no argument for it) means the ACCOUNT's
  `default_link_style`, so a capture tool follows whatever `/settings` says without being
  reconfigured (DESIGN.md §20 erratum 250).
- `wait`: **REFUSED** with 400 `invalid_request` (erratum 212). It used to hold the reply for
  a caller-chosen number of seconds; the wait is now the operator's setting alone — see below.
- An empty field value means "not sent", not "explicitly empty" — a tool that always emits
  its arguments (even blank) is not forced to invent a real TTL or title.
- A title **equal to the file's own name** is treated as absent (erratum 60): ShareX's
  `{filename}` argument would otherwise put the raw filename, extension included, into
  `og:title` and every Discord card.

**The card wait, this route's one real behavioral difference from `POST /api/clips`.**
The 201 is held — up to `CAPTURE_READY_WAIT` (default **30s**, hard ceiling 60s) — until the
clip's **card** works: a poster plus the file `og:video` points at, and no card copy still
being encoded (errata 212 and 235, superseding erratum 97's "a terminal state"). That is what
makes the link a capture tool puts on the clipboard unfurl as a **player** card on first paste
rather than as a link-out card Discord then caches for good.

The wait never errors. It ends when the card is ready, when the clip fails, when the budget
runs out, or when the client disconnects, and the reply then reports two separate facts:

- `state` is the CLIP's state, exactly as it always was — normally `ready`, `queued` or
  `processing` when the wait ran out, `failed` if the transcode failed inside the window;
- `card_ready` is whether the link is worth pasting right now.

**At the ceiling the link is still returned**, with `card_ready: false` — a capture tool
cannot be held for minutes. There is deliberately **no estimate** of how much longer it will
be, in the body or in a header.

There is no per-request override any more. A `wait` field of any value is
`400 invalid_request` with `"wait is no longer accepted: the server's CAPTURE_READY_WAIT
decides how long the reply is held"`; an EMPTY `wait=` still means "not sent", like every
other capture field. `wait=0` used to buy a faster answer and a permanently link-out card for
everyone who read that link, which is not a trade a client makes for an instance.

**201** `{"id", "url", "embed_url", "state", "card_ready", "expires_at", "tiers"}` (**200** and
`"existing": true` added for a duplicate, erratum 326; the wait applies to it too) — still
deliberately smaller than `ClipDTO`: this is what goes on a clipboard, not a page to render.
`tiers` is the same list `ClipDTO` carries (always three entries, best first — see
`tiers[]` below), so a tool that wants to say "1440p60, card at 720p30" needs no second
request.

```sh
$ curl -s -X POST http://localhost:8481/api/upload \
    -H "Authorization: Bearer ck_YOUR_TOKEN_HERE" \
    -F 'title=API doc test clip' -F 'ttl_days=1' \
    -F 'file=@clip.mp4;type=video/mp4'
{"id":"smW1BNY9aa23","url":"https://clips.example.com/smW1BNY9aa23",
 "embed_url":"https://clips.example.com/embed/smW1BNY9aa23","state":"ready",
 "card_ready":true,"expires_at":"2026-09-08T10:37:21Z",
 "tiers":[{"tier":"original","url":"https://clips.example.com/api/clips/smW1BNY9aa23/original",
           "width":1920,"height":1080,"fps":"60/1","bytes":15117125,"codec":"h264",
           "playable":true,"state":"ready"},
          {"tier":"full","url":null,"width":null,"height":null,"fps":null,"bytes":null,
           "codec":"","playable":true,"state":"skipped"},
          {"tier":"light","url":"https://clips.example.com/api/clips/smW1BNY9aa23/light.mp4",
           "width":1280,"height":720,"fps":"30/1","bytes":1583104,"codec":"h264",
           "playable":true,"state":"ready"}]}
```

Errors: the same set `POST /api/clips` has, plus **400** `invalid_request` for a `wait` field
(above). A field with a closed set of its own answers that field's code — `invalid_ttl`,
`invalid_tags`, `invalid_link_style`; everything else on this route shares the one
`invalid_request` code and distinguishes itself in `message` only.

### `POST /in/{token}` — the inbound URL (rate: `upload`, both buckets)

The capture endpoint for a tool that cannot send a header (DESIGN.md §20 erratum 322). `{token}` is an inbound URL's secret — `ci_` and 43 characters — minted by `POST /api/auth/tokens` with `"scopes": ["inbound"]`. No cookie and no `Authorization` header is read.

Body: `POST /api/upload`'s multipart grammar, unchanged.

**201** — `text/plain`: the share URL and a newline. With `Accept: application/json`: the `UploadDTO` `POST /api/upload` answers. A duplicate (erratum 326) is the same two shapes with **200**: the existing clip's link, and in JSON `"existing": true`.

**404** `not_found`, "no such route" — any value that is not a live inbound URL: malformed, unknown, revoked, or a `ck_` token. Byte for byte the answer of a path with no route.

Every other refusal is `POST /api/upload`'s.

### `POST /in/auto/{token}` — the Clippet Auto URL (rate: `upload`, both buckets)

Makes this instance fetch one Discord attachment into the token's account. `{token}` is `ca_` and 43 characters, minted with `"scopes": ["autofetch"]`. No cookie and no `Authorization` header is read, and no file body is accepted. Body: JSON `guild_id`, `url`, `filename`, `content_type`, `size`, and optionally `duplicate` (`"allow"` is the duplicate check's opt-out; any other value is 400). `url` must be an attachment on Discord's CDN; `guild_id` must be a server connected to the token's account. **201**: the upload. **200** with `"existing": true`: a duplicate (erratum 326). **202** `{"state":"working"}`: not finished after 100 s; it continues. **404**, as a missing route: any value that is not a live Clippet Auto URL. Since erratum 327: the second bucket is keyed by the URL, not the client IP; **429** `rate_limited` (with `Retry-After`) also when two of the account's fetches are already running; a `filename` over 255 bytes is shortened to 255, its extension kept, never refused; `size` is the most that is read — a download past it is **413** `too_large`; a restart while Discord's CDN is being read is **503** `unavailable` with `Retry-After: 5`, not 502.

### `POST /api/share` — session only (rate: `upload`, both buckets)

The Android share-sheet target (`/manifest.webmanifest`'s `share_target`; DESIGN.md §20
erratum 137). Same grammar and ingest as `POST /api/upload`, but this is a browser
top-level navigation, not a script reading JSON: on success it answers **303** to the
clip's own share page — what the phone lands on — rather than a body. A caller that wants
JSON (anything that does not send `Accept: text/html`) gets the ordinary error envelope on
failure; a browser that is not signed in gets an HTML interstitial explaining that nothing
was uploaded, because the browser will not silently re-POST a file after a login redirect.
A Bearer token is never accepted here — a share-sheet launch has nowhere to put one, and a
token must not be able to spend an account's uploads through a route that redirects rather
than answers.

### `GET /manifest.webmanifest` — anonymous

**200** `application/manifest+json; charset=utf-8`, `Cache-Control: public, max-age=3600`.
The W3C web app manifest; its `share_target` is what makes `POST /api/share` reachable
from a phone's OS share list once the site is installed (Android Chrome only, https only).
Not something you call for its own sake, but worth knowing it exists if you are building
against the share flow.

## Clips

### `GET /api/clips?limit=&cursor=&tag=&q=&mine=` — session or `read` token

The library: under `LIBRARY_SCOPE=shared` (the default) every listed (`unlisted:false`)
clip on the instance plus the caller's own unlisted ones; under `LIBRARY_SCOPE=own`
(DESIGN.md §20 erratum 113) the caller's own clips alone. Newest first, every state except
`deleting`. `views` (and the trim/duration owner fields) are present only on clips **the
caller owns** — `LIBRARY_SCOPE=shared` mixes clips from every account, and other people's
view counts are never in it.

- `limit`: 1–100, default 50.
- `cursor`: opaque, from a previous response's `next_cursor`; pass it back verbatim.
- `tag`: narrows the same set to clips carrying that tag (DESIGN.md §20 erratum 81) — this
  is a filter over the library, not a public "browse by tag" page: there is no route that
  lists a tag's clips to an anonymous caller.
- `q`: a case-insensitive substring of the title **or** the description, at most 64 runes
  (§20 erratum 114) — a search box, not a query language.
- `mine=1`: narrows to the caller's own clips only (§20 erratum 115). It is a switch, not
  a value: the only spelling that turns it on is exactly `1`, and any other value is a 400
  — a filter that silently did nothing would show a wider list than the caller asked for.

Every filter composes with every other (`?tag=clutch&q=standing&mine=1` is legal), and
each one narrows the same underlying set independently.

"Newest first" is `created_at DESC, id ASC` — `created_at` is whole seconds, so clips
created in the same second are ordered by their (random) id, not upload order. That is a
stable, total order (what a cursor needs), just not sub-second insertion order.

```sh
$ curl -s -b cookies.txt 'https://clips.example.com/api/clips?limit=2'
{"clips":[{"id":"Z6eQucmYEyFQ", ...}, {"id":"smW1BNY9aa23", ...}],
 "next_cursor":"MTc4ODc3NzQ0MTpzbVcxQk5ZOWFhMjM"}

$ curl -s -b cookies.txt 'https://clips.example.com/api/clips?tag=gaming'
{"clips":[{"id":"Z6eQucmYEyFQ","tags":["clutch","gaming"], ...}, {"id":"G66oZ2D68OOP", ...}],
 "next_cursor":null}

$ curl -s -b cookies.txt 'https://clips.example.com/api/clips?q=standing'
{"clips":[{"id":"Z6eQucmYEyFQ","title":"Standing 720p clip", ...}, {"id":"G66oZ2D68OOP", ...}],
 "next_cursor":null}

$ curl -s -b cookies.txt 'https://clips.example.com/api/clips?mine=1&limit=2'
{"clips":[{"id":"Z6eQucmYEyFQ","owner":{"username":"rc_apiwalk", ...}, ...},
          {"id":"smW1BNY9aa23","owner":{"username":"rc_apiwalk", ...}, ...}],
 "next_cursor":null}

$ curl -s -b cookies.txt 'https://clips.example.com/api/clips?mine=bad'
{"error":"invalid_request","message":"mine must be 1 (there is no way to list another user's clips)"}

# a token minted with the read scope may call this route too (§20 erratum 111)
$ curl -s -H "Authorization: Bearer $READ_TOKEN" 'http://localhost:8481/api/clips?limit=2'
{"clips":[...], "next_cursor":"..."}

# an upload-only token may not
$ curl -s -H "Authorization: Bearer $UPLOAD_TOKEN" 'http://localhost:8481/api/clips?limit=2'
{"error":"token_not_allowed","message":"this endpoint requires a browser session or a token with the read scope"}
```

Errors: **400** `invalid_cursor` (garbage cursor, **or** a cursor minted under a different
`tag`/`q`/`mine` filter than the one now in the query string — a cursor is only valid for
the exact list it paged); **400** `invalid_tags` (a `tag=` value that cannot be a real
tag); **400** `invalid_request` (a `q` over 64 runes, or a `mine` that is not `1`);
**401**; a Bearer token without the `read` scope gets **403** `token_not_allowed` (though
this route's auth column reads "user" in the design doc's shorthand — `RequireSessionOrScope`
is what actually guards it, and it accepts a session too).

### `GET /api/clips/{id}` — anonymous

Public: the unguessable link is the auth. **200** `ClipDTO` in any state but `deleting`.
`views`, `source_duration_ms` and `trim` are present **only when the caller is the owner** —
a browser session always qualifies, and since §20 erratum 112 so does a Bearer token reading
its own clip through this route **if the token carries the `read` scope** (`auth.Principal.OwnerReader`);
an upload-only token still sees the public DTO of its own clips, never the owner-private fields.
`TestReadScopeMatrix` (account_test.go) pins the full session/read-token/upload-token matrix.

```sh
$ curl -s http://localhost:8481/api/clips/Z6eQucmYEyFQ | python -m json.tool | head -6
{
    "id": "Z6eQucmYEyFQ",
    "title": "Standing 720p clip",
    ...
    # no "views", "source_duration_ms" or "trim" key at all — anonymous caller
```

The full shape — every field, once the owner has opened the trim control at least once so
`filmstrip` is populated too (`tiers`, `card_tier`, `urls.light`/`urls.original` are always
there, not only once a Full or Light exists):

```json
{
    "id": "Z6eQucmYEyFQ", "title": "Standing 720p clip", "description": "",
    "unlisted": false, "state": "ready", "link_style": "compact",
    "failure_reason": null,
    "tags": ["clutch", "gaming"],
    "created_at": "2026-09-07T10:37:26Z", "updated_at": "2026-09-07T10:37:26Z",
    "ready_at": "2026-09-07T10:37:26Z", "ttl_days": 1,
    "expires_at": "2026-09-08T10:37:26Z",
    "duration_ms": 6000, "width": 1280, "height": 720, "fps": "30/1", "has_audio": true,
    "poster": {"width": 1280, "height": 720},
    "gif_url": null,
    "filmstrip": {"url": "https://clips.example.com/api/clips/Z6eQucmYEyFQ/filmstrip.jpg",
                  "tiles": 24, "tile_width": 160, "tile_height": 90, "interval_ms": 250},
    "source": {"filename": "clip.mp4", "size": 137239, "content_type": "video/mp4"},
    "owner": {"id": "X3Jk3t6SgznF", "username": "rc_apiwalk"},
    "urls": {
        "page": "https://clips.example.com/Z6eQucmYEyFQ",
        "embed": "https://clips.example.com/embed/Z6eQucmYEyFQ",
        "video": "https://clips.example.com/api/clips/Z6eQucmYEyFQ/video.mp4",
        "social": "https://clips.example.com/api/clips/Z6eQucmYEyFQ/social.mp4",
        "light": "https://clips.example.com/api/clips/Z6eQucmYEyFQ/light.mp4",
        "original": "https://clips.example.com/api/clips/Z6eQucmYEyFQ/original",
        "poster": "https://clips.example.com/api/clips/Z6eQucmYEyFQ/poster.jpg",
        "progress": "https://clips.example.com/api/clips/Z6eQucmYEyFQ/progress"
    },
    "tiers": [
        {"tier": "original", "url": "https://clips.example.com/api/clips/Z6eQucmYEyFQ/original",
         "width": 1280, "height": 720, "fps": "30/1", "bytes": 137239, "codec": "h264",
         "playable": true, "state": "ready"},
        {"tier": "full", "url": null, "width": null, "height": null, "fps": null,
         "bytes": null, "codec": "", "playable": true, "state": "skipped"},
        {"tier": "light", "url": null, "width": null, "height": null, "fps": null,
         "bytes": null, "codec": "h264", "playable": true, "state": "skipped"}
    ],
    "card_tier": "original"
}
```

This is a one-tier clip (720p30 never crosses `SOCIAL_MAX_BYTES` or needs a Full), which is
why `full`/`light` are `skipped` and the selector §27 describes does not appear for it — see
[the ClipDTO wire format](#the-clipdto-wire-format) for what a clip with more than one tier
carries instead.

Errors: **404** `not_found` (unknown, malformed id, or `deleting`).

### `PATCH /api/clips/{id}` — owner (CSRF)

```json
{"title"?, "description"?, "unlisted"?, "ttl_days"?, "tags"?}
```
Every field is independently optional; an absent key leaves that field unchanged. Two
exceptions worth knowing:
- `ttl_days` follows the [TTL rule](#ttl_days-input) — `null`/absent means "unchanged" on
  PATCH (unlike upload, where absent means "apply the default").
- `tags`, when present, **replaces the whole set**: `[]` clears every tag, absent leaves
  the set alone (there is no "add one tag" operation on this route).

**200** `ClipDTO` (owner fields included — a PATCH response always reflects the owner
view, since only the owner can reach this route). **400** `invalid_request` /
`invalid_ttl` / `invalid_tags`; **401**; **403** `token_not_allowed` (a Bearer token),
`forbidden` (a session that is not the owner) or `csrf`; **404**; **415**.

```sh
$ curl -s -b cookies.txt -X PATCH https://clips.example.com/api/clips/Z6eQucmYEyFQ \
    -H 'Content-Type: application/json' -d '{"ttl_days":0}'
{"id":"Z6eQucmYEyFQ", ..., "ttl_days":0, "expires_at":null, ...}
```

### `DELETE /api/clips/{id}` — owner (CSRF)

No body. Deletes the row and every stored object under the clip's prefix. **204** when
everything is gone by the time the request returns (the common case — deletion runs inline
under a 60-second budget). **202** `{"state":"deleting"}` in the rare case the storage step
outran that budget or failed transiently: the row is already tombstoned (every public route
answers 404 for it from that instant on) and the sweeper finishes the object cleanup within
its next tick. **401**; **403** (`token_not_allowed`, `forbidden` or `csrf`); **404**.

```sh
$ curl -s -b cookies.txt -X DELETE https://clips.example.com/api/clips/smW1BNY9aa23 -w '%{http_code}\n'
204
```

### `POST /api/clips/{id}/retry` — owner (CSRF)

Requeues a `failed` clip — the original is still in the bucket, so this is free. **202**
`ClipDTO` with `"state":"queued"`. **409** `not_failed` (only a failed clip can be
retried); **401**; **403** (`token_not_allowed`, `forbidden` or `csrf`); **404**.

### `POST /api/clips/{id}/touch` — owner (CSRF)

No body. The keep-alive: resets `expires_at = now + ttl_seconds`, in every state but
`deleting`. A no-op **200** (no field changes) for a clip with no TTL at all. This is what
the share page's own "Copy link" button calls for the owner — re-sharing a link is the
clearest signal a clip is still wanted (DESIGN.md §20 erratum 62). No route-specific rate
limit; it is one indexed `UPDATE`. **200** `ClipDTO`; **401**; **403**
(`token_not_allowed`, `forbidden` or `csrf`); **404**.

### `POST /api/clips/bulk` — session (CSRF)

The library's multi-select actions (§20 errata 117–119): one request, one action, up to
100 clip ids, and a **per-id** result — the alternative (a loop over the single-clip routes
in the browser) is twenty round trips over one connection, each with its own CSRF check
and session lookup, and a tab closed halfway through leaves the set half-applied with
nothing that says which half. Here the half that applied **is** the answer. It is **not**
a transaction over the set: each id is applied on its own and reported on its own —
"delete these twenty" where one belongs to somebody else deletes nineteen and says
`forbidden` about the twentieth.

```json
{"action": "delete"|"ttl"|"tag"|"collection", "ids": ["…"], "ttl_days"?, "tag"?, "collection_id"?}
```
- `ids`: 1–100, duplicates collapse to one result row.
- `action=ttl` reads `ttl_days` (the [TTL wire values](#ttl_days-input)).
- `action=tag` reads `tag`: **one** tag, **added** to each clip's existing set.
- `action=collection` reads `collection_id`: the clips are **appended** to it, in the
  order sent. The collection is checked **once**, before any clip is touched — a missing
  or foreign `collection_id` fails the whole request with the collection's own 404, before
  a single clip is looked at.
- There is no bulk `PATCH` of free text (title, description): overwriting twenty different
  values with one is the accident this surface is built to be incapable of.

**200** always, when the **request** itself was valid — no transport status can say
"nineteen of twenty":
```json
{"action","ok": n,"failed": n,"results": [{"id","status","error": null|"code"}, ...]}
```
`status` is the status the equivalent single-clip request would have answered with (204/202
for a delete, 200 for an edit, 403 for somebody else's clip, 404 for one that is not
there), in the order the ids were sent.

```sh
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/clips/bulk \
    -H 'Content-Type: application/json' \
    -d '{"action":"tag","ids":["Z6eQucmYEyFQ","hk2fNeguypTA"],"tag":"apidoc"}'
{"action":"tag","ok":2,"failed":0,"results":[{"id":"Z6eQucmYEyFQ","status":200,"error":null},
 {"id":"hk2fNeguypTA","status":200,"error":null}]}

# one real id, one that does not exist: 200 overall, one result each
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/clips/bulk \
    -H 'Content-Type: application/json' \
    -d '{"action":"delete","ids":["hk2fNeguypTA","nosuchclipid1"]}'
{"action":"delete","ok":1,"failed":1,"results":[{"id":"hk2fNeguypTA","status":204,"error":null},
 {"id":"nosuchclipid1","status":404,"error":"not_found"}]}
```

Errors (request-level, before any per-id result): **400** `invalid_request` (no action, an
unknown action, no ids, over 100); **400** `invalid_ttl`; **400** `invalid_tags`; **404**
`not_found` (a `collection_id` that is missing or not the caller's); **401**; **403**
(`token_not_allowed` or `csrf`); **415**. Per-id errors ride inside `results[].error` and
never change the 200: `not_found` (404, a stranger's or a deleted clip); `invalid_tags` (400,
`action=tag` on a clip already at its own tag cap — §20 erratum 81); `too_many_clips` (409,
`action=collection` once the collection has reached `db.MaxClipsPerCollection` mid-request,
bulk.go:340); `unavailable` (503).

### `GET /api/clips/{id}/progress` — anonymous (rate: `poll`, 120/min/IP)

`Cache-Control: no-store`. **200**:
```json
{"id","state":"queued|processing|ready|failed","stage","percent":0-100,
 "indeterminate":false|true,"error":null|"...","updated_at":"...",
 "tiers":[{"tier":"original|full|light","state":"ready|encoding|failed|skipped|pending"}]}
```
Source of truth is the worker's live in-memory registry while it is running that job;
after a restart, or before a job is claimed, it falls back to the database. `stage` moves
through `queued, acquire, probe, remux, light, encode, verify, poster, store, finalize,
done` — `remux` is the `-c copy` container rewrite of a playable original and `light` is the
card copy's encode, both of which a given clip may or may not have (DEPLOY.md §27).

`pending` is a tier on a clip that is still `queued` or `processing`: nobody has probed the
upload yet, so nothing has been decided. It is the honest word for that — `skipped` means
"not needed, a finished answer", and a client that read it there would stop waiting for a
Full that is on its way (DESIGN.md §20 erratum 205, amended).

**`tiers` is the one reason to keep polling a clip that is already `ready`** (DESIGN.md §20
erratum 205). `state:"ready"` with `full:"encoding"` is the ordinary shape of a clip somebody
can already watch while its faithful copy finishes: a viewer has a player and a card, and a
better tier is coming. It carries the names and the states only — a poller that wants the
dimensions and the URLs reads `GET /api/clips/{id}`, which is the one place a tier is fully
described. When no tier is `encoding` any more there is nothing left to wait for.

This route carries its OWN rate bucket, `poll` at 120/min per client address, and not the
`view` one its batched sibling shares: a poll must not spend the tokens that count views and
serve preview GIFs (DESIGN.md §20 erratum 211, amended). 120 is twice what one watch page can
issue — it polls every 5 s for the first minute and every 15 s after that — so several viewers
behind one address still poll comfortably. A limited answer is **429** `rate_limited` with
`Retry-After`, which a poller treats like any other transient error: wait and ask again.

**Poll discipline, because it matters:** there are exactly three terminal signals —
`state="ready"`, `state="failed"` (with `error` set), and **HTTP 404**. A 404 means the
clip is gone (never seen, or deleted) and must **never** be read as "processing finished" —
a poller that treats 404 as success will show a broken player for a clip that was simply
deleted mid-poll.

```sh
$ curl -s http://localhost:8481/api/clips/Z6eQucmYEyFQ/progress
{"id":"Z6eQucmYEyFQ","state":"ready","stage":"done","percent":100,
 "indeterminate":false,"error":null,"updated_at":"2026-09-07T10:37:23Z",
 "tiers":[{"tier":"original","state":"ready"},{"tier":"full","state":"skipped"},
          {"tier":"light","state":"ready"}]}
```

### `GET /api/clips/progress?ids=a,b,c` — anonymous (rate: `view`, 60/min/IP)

One poll for a **whole page** of the library instead of one per unfinished row (§20
erratum 116) — a watch-folder drop of forty files made one open browser tab issue ~40
requests a second against a box that was simultaneously running ffmpeg. `Cache-Control:
no-store`. **200**:
```json
{"progress": {"<id>": ProgressDTO, ...}}
```
Every fact here is a fact the single-clip route already gives the same caller — same
public route class, same `ProgressDTO` shape, same three terminal signals. What changes is
the cost, so this route is under the `view` limiter where the single one is not: one call
can do up to 100× the work of one poll (the same reasoning DESIGN.md §20 erratum 87
applies to the collection `GET`).

An id that resolves to no live clip — unknown, or a `deleting` tombstone — is **absent
from the map**. That is this route's spelling of the single route's 404, and it means the
same thing: a poller must read a missing key as "gone", never as "done".

- `ids`: 1–100 clip ids, comma-separated, duplicates collapse (the answer is a map, so
  asking twice cannot cost a second row).

```sh
$ curl -s 'https://clips.example.com/api/clips/progress?ids=Z6eQucmYEyFQ,hk2fNeguypTA'
{"progress":{"Z6eQucmYEyFQ":{"id":"Z6eQucmYEyFQ","state":"ready","stage":"done",
 "percent":100,"indeterminate":false,"error":null,"updated_at":"2026-09-07T10:37:23Z",
 "tiers":[{"tier":"original","state":"ready"},{"tier":"full","state":"skipped"},
          {"tier":"light","state":"ready"}]}}}
```
(`hk2fNeguypTA` had already been deleted by the bulk example above, so it is simply not a
key in the answer.)

Errors: **400** `invalid_request` (`ids` absent or empty, a malformed id anywhere in the
list, or more than 100 of them — a bad id is a 400 here rather than a silently dropped
key, because a batch that quietly omitted it would report "this clip is gone" for what is
really a typo); **429** `rate_limited`; **503**.

## Editing a clip in place: trim, replace, GIF, poster

Four features share one shape: a `POST` that queues a job and answers **202** with a
`state` and, for three of the four, a `job_id` (nothing about the clip changes yet — GIF
alone tracks no job_id, because its own status lives entirely on the clip's `gif_url`), and
a `GET` (all but GIF, which has no status route of its own) that reports that job's own
status — **not** the clip's `progress`, which keeps reading `ready`/100% the whole time,
because **the clip stays `ready` and fully servable for the entire run**. All four are
`session`-only, CSRF-guarded, and share the `TRANSCODE_CONCURRENCY` ffmpeg semaphore with
ordinary uploads (DESIGN.md §20 erratum 79) — a capture-tool Bearer token cannot start any
of them (**403** `token_not_allowed`).

### `POST /api/clips/{id}/trim` — owner (CSRF, rate: `upload`)

```json
{"start_ms": 500, "end_ms": 3000}
```
**The window is always measured on the ORIGINAL upload's timeline, never on the current
rendition** (DESIGN.md §20 erratum 68) — that is what makes a second trim able to
re-extend past an earlier cut. Rules: `0 ≤ start_ms < end_ms`; the window is at least
500 ms long; `end_ms` does not run past the original's measured duration; and the request
must not be **exactly** the cut the clip already has (that would re-encode for zero
change). **202** `{"job_id", "state":"queued"}`.

```sh
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/clips/Z6eQucmYEyFQ/trim \
    -H 'Content-Type: application/json' -d '{"start_ms":500,"end_ms":3000}'
{"job_id":423,"state":"queued"}
```

Errors: **400** `invalid_trim` (one sentence naming which rule failed); **409**
`not_ready` (not a `ready` clip); **409** `trim_in_progress` (one at a time); **401**;
**403** (`token_not_allowed`, `forbidden` or `csrf`); **404**; **415**.

### `GET /api/clips/{id}/trim` — owner

`Cache-Control: no-store`. **200**, the *latest* trim job:
```json
{"job_id","state":"queued|running|done|failed|cancelled","stage","percent":0-100,
 "error":null|"...","start_ms","end_ms","applied_start_ms":null|n,"applied_end_ms":null|n}
```
`start_ms`/`end_ms` are what was **asked** for; `applied_*` are what was actually **cut**
(present only once `state="done"`) — they can legitimately differ, because a stream-copy
trim snaps its start to the nearest keyframe. A **later replacement** also retires
`applied_start_ms`/`applied_end_ms` back to `null` on an otherwise-`done` job, the same
shape and the same reason as the poster route's staleness above: the file the window cut
is gone, and `start_ms`/`end_ms` keep naming a window on the clip's retired original.
**404** `not_found` — for an unknown clip,
**and** for a clip that has never been trimmed at all; a poller must not read "no job" as
"finished". **401**; **403** (`token_not_allowed`, or `forbidden` for a session that is not
the owner) as on every owner route.

```sh
$ curl -s -b cookies.txt https://clips.example.com/api/clips/Z6eQucmYEyFQ/trim
{"job_id":423,"state":"done","stage":"done","percent":100,"error":null,
 "start_ms":500,"end_ms":3000,"applied_start_ms":500,"applied_end_ms":3000}
```

### `POST /api/clips/{id}/replace` — owner (CSRF, rate: `upload`, both buckets)

Multipart: any text fields (all **ignored** — a replacement changes the file and nothing
else; edit title/tags/TTL with `PATCH`) then the file part, named `file` or `video` — the
same grammar `POST /api/upload` uses, minus the fields. The old file keeps serving every
URL (`video.mp4`, the share page, an already-posted card) until the new one is ready; then
the swap is one guarded `UPDATE`. **202** `{"job_id", "state":"queued"}`.

```sh
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/clips/Z6eQucmYEyFQ/replace \
    -F 'file=@better-take.mp4;type=video/mp4'
{"job_id":424,"state":"queued"}
```

Errors: **400** `missing_video` / `empty_video` / `invalid_request`; **401**; **403**
`token_not_allowed`/`forbidden`/`csrf`; **404**; **409** `not_ready` (only a `ready` clip
can be replaced — and every ready clip can be, including one whose upload is served as it
stands and one whose Full is still encoding: DESIGN.md §20 erratum 201, amended); **409**
`replace_in_progress`; **413**; **415**; **503** (`storage_unavailable`, or `unavailable` for
a ready row with no playable bytes at all — an invariant violation, never a retry); **507**.

### `GET /api/clips/{id}/replace` — owner

`Cache-Control: no-store`. **200**, the latest replacement job:
```json
{"job_id","state":"queued|running|done|failed|cancelled","stage","percent":0-100,
 "error":null|"...","filename"}
```
`filename` is the name of the file that was uploaded for this replacement — useful when a
client wants to say *which* attempt it is showing progress for. **404** `not_found` for a
clip that has never been replaced. **401**; **403** (`token_not_allowed`, or `forbidden` for
a session that is not the owner) as on every owner route.

### `GET`/`HEAD /api/clips/{id}/preview.gif` — anonymous (rate: `view`, 60/min/IP)

Public, like `poster.jpg` — a preview of a clip anyone with the link may already watch
frame by frame leaks nothing new. **200/206/304** `image/gif` once one exists (same
Range/ETag machinery as every other media route, below). **202** `{"state":"queued"|
"running"}` + `Retry-After: 5` when the clip is `ready` and has no preview yet — visiting
this URL *starts* the encode (at most one live job per clip; a hundred simultaneous
requests still make one job).

```sh
$ curl -s -D - -o /dev/null https://clips.example.com/api/clips/Z6eQucmYEyFQ/preview.gif
HTTP/1.1 202 Accepted
Retry-After: 5
Cache-Control: no-store
Content-Type: application/json; charset=utf-8
```

Errors: **404** `not_found` (unknown clip, or `deleting`); **409** `not_ready` (not a
ready clip, so nothing exists to make a preview from); **503** `gif_failed` (the last
attempt failed — this is reported once, never silently retried on every visitor's behalf;
the owner's `POST` below is what tries again); **416**; **429**.

### `GET`/`HEAD /api/clips/{id}/filmstrip.jpg` — anonymous to READ, owner to MAKE (rate: `view`, 60/min/IP)

The trim scrubber's filmstrip (§20 errata 220 and 227): ONE JPEG holding ~24 thumbnails of
the clip's **current rendition**, evenly spaced across the whole of it and laid out in a
single row. One URL, two arms.

**Reading a sheet that exists is public**, for `preview.gif`'s reason exactly — thumbnails of
a clip anyone with the link may already watch frame by frame leak nothing, and this URL
travels inside pages that caches and chat clients keep. **200/206/304** `image/jpeg` to any
caller, through the same Range/ETag machinery as every other media route (below), with the
same `Cache-Control: public, max-age=3600` and no `Vary`.

**Making one is the owner's**, from a browser session. A request that finds no sheet is
answered by who is asking:

- the clip's **owner**, with a session cookie: **202** `{"state":"queued"|"running"}` +
  `Retry-After: 5` when the clip is `ready`, and that request *starts* the pass (at most one
  live job per clip, so a hundred simultaneous requests still make one job);
- **everybody else** — no credential at all, another account's session, or a Bearer token of
  either scope: **404** `not_found`, and nothing is queued. The pass decodes the whole
  rendition for a sheet whose only reader is the owner's own trim control, so it is not a
  stranger's to start (DESIGN.md §20 erratum 227 records the decision and why a `read` token
  is refused with the rest).

Every no-sheet answer carries `Cache-Control: private, no-store` and
`Vary: Cookie, Authorization`, the 404 included: it depended on a credential.

```sh
$ curl -s -D - -o /dev/null -b cookies.txt \
    https://clips.example.com/api/clips/Z6eQucmYEyFQ/filmstrip.jpg
HTTP/1.1 202 Accepted
Cache-Control: private, no-store
Content-Type: application/json; charset=utf-8
Retry-After: 5
Vary: Cookie, Authorization
```

There is no owner `POST` beside it, and none is needed: a filmstrip is invalidated by exactly
the events that invalidate the rendition — a trim and a replacement both drop it — so the
next time the owner opens the control there is no sheet and this GET makes a new one.

**How to read the sheet.** The geometry is in the clip DTO's `filmstrip` object, never in a
header:

```json
"filmstrip": {"url": "https://clips.example.com/api/clips/Z6eQucmYEyFQ/filmstrip.jpg",
              "tiles": 24, "tile_width": 160, "tile_height": 90, "interval_ms": 250}
```

`null` means there is no sheet for the current rendition — the owner's client asks this route and
one is made; anyone else asking gets the 404 above.
Every number is MEASURED from the produced JPEG, so the sheet is exactly
`tiles × tile_width` wide and `tile_height` tall, and the tile of a moment `ms` on the
**rendition's** timeline is at x offset `floor(ms / interval_ms) × tile_width`, clamped to
`tiles − 1`. A caller holding a moment on the ORIGINAL's timeline subtracts the applied cut's
start first (`trim.start_ms`), exactly as the chosen card frame does.

Errors: **404** `not_found` — an unknown or `deleting` clip, and (erratum 227) *any* no-sheet
request that is not the owner's own session, whatever the clip's state. The owner's other
no-sheet answers: **409** `not_ready` (not a ready clip, so there is nothing to sample);
**503** `filmstrip_failed` (the last attempt failed — reported once, never silently retried
every time the control is opened; a trim or a replacement gives the clip a fresh rendition and
therefore a fresh attempt). Then **416** and **429** as on every media route.

### `POST /api/clips/{id}/gif` — owner (CSRF, rate: `upload`)

No body. The owner's explicit (re)generate — after a failure, after a trim (which clears
any preview cut from the rendition it replaced), or simply on request. **202**
`{"state":"queued"}`. **409** `gif_in_progress`; **409** `not_ready`; **401**; **403**
(`token_not_allowed`, `forbidden` or `csrf`); **404**.

```sh
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/clips/Z6eQucmYEyFQ/gif
{"state":"queued"}
```

### `POST /api/clips/{id}/poster` — owner (CSRF, rate: `upload`)

```json
{"seconds": 2.5}
```
The owner's chosen card image (ROADMAP #5; §20 errata 127–130) — the same conventions as
`trim`, one job over one field. `seconds` is a **moment on the ORIGINAL's timeline**
(erratum 68's coordinate system, the one that does not move under the owner), bounded to
the part of it the **current rendition** actually shows: `trim_start_ms/1000 ≤ seconds ≤
trim_start_ms/1000 + (duration_ms − 100)/1000` — the 100 ms edge guard exists because a
seek at or past the last picture makes ffmpeg write no file at all. It is a pointer under
the hood so `0` (the first frame) is a legal, explicit choice, distinct from "absent".

**202** `{"job_id", "state":"queued", "seconds"}` — `seconds` rounded to the millisecond
the server will actually seek to; every fact about the clip is still true, and the new card
image lands when the job commits.

```sh
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/clips/Z6eQucmYEyFQ/poster \
    -H 'Content-Type: application/json' -d '{"seconds":2}'
{"job_id":428,"state":"queued","seconds":2}
```

Errors: **400** `invalid_poster` (absent, or a moment this clip does not show); **400**
`invalid_request` for a body whose `seconds` is not a JSON number (decodeJSON's house-wide
contract — `NaN`/`Infinity` are not legal JSON literals, so a caller that tries one never
reaches the poster-specific check); **409** `not_ready` (only a `ready`, measured clip has a card image to
choose); **409** `poster_in_progress`; **401**; **403** (`token_not_allowed`, `forbidden`
or `csrf`); **404**; **415**.

### `GET /api/clips/{id}/poster` — owner

`Cache-Control: no-store`. **200**, the *latest* poster job:
```json
{"job_id","state":"queued|running|done|failed|cancelled","stage","error":null|"...",
 "seconds","applied_seconds":null|n}
```
`seconds` is what was **asked** for; `applied_seconds` is the choice the **clip** now
carries, read once the job is `done` — a poster job keeps no progress registry (one seek
has no meaningful percentage), so this is the row itself. A swap that lost its race to a
newer choice still resolves `done` but wrote nothing, so `applied_seconds` reads the clip
rather than echoing the request, and stays `null` for a card image that is still automatic.

**A `done` job with `applied_seconds: null` is not always a lost race.** The clip's chosen
frame goes back to automatic — and this route keeps reporting the job that finished, never
404 — by three routes: a swap that lost its race (above), a **later trim** whose applied
window no longer contains the chosen moment, or a **later replacement**, which retires the
chosen frame outright along with the trim window (the file the moment named is gone). In
every one of these, `seconds` may still name a moment on a clip's **retired original** — it
is simply what was asked for at the time, not a claim about the clip's current file. The
share page never shows this staleness (it renders the "chosen frame" note only when a frame
is actually chosen); a caller polling this route directly can.
**How a client tells a real swap from a lost race: read `stage`, not the seconds.** A job
that lost its race resolves `"state":"done","stage":"skipped"`; one that published resolves
`"done","done"`. Comparing `applied_seconds` against the moment the caller already knew about
reports a job that re-took the SAME moment as a no-op, although it published a fresh object —
which is why the share page reads `stage` (DESIGN.md §20 erratum 228).

**404** `not_found` for an unknown clip, **and** for one whose card image has never been
chosen — a poller must not read "no job" as "finished". The same staleness shape exists on
`GET …/trim`'s `applied_start_ms`/`applied_end_ms` after a replacement, for the identical
reason.

`error` is set only when `state` is `failed`, and it is always one of the **fixed
sentences** that kind's runs word their endings with (DESIGN.md §20 errata 80 and 165) —
never an ffmpeg stderr tail, a path, a byte count, or an operator's wording (a job the server
repaired after a restart or an `admin jobs unstick` included). A failure the
server cannot attribute to anything the owner did comes out as the kind's generic sentence
(`the card image could not be changed` here, `the trim failed`, `the replacement failed`);
the raw text stays in the server log. The same rule governs `error` on
`GET …/trim` and `GET …/replace`, and the `message` of `preview.gif`'s 503.

```sh
$ curl -s -b cookies.txt https://clips.example.com/api/clips/Z6eQucmYEyFQ/poster
{"job_id":428,"state":"done","stage":"done","error":null,"seconds":2,"applied_seconds":2}
```

Errors: **401**; **403** (`token_not_allowed`, or `forbidden` for a session that is not
the owner) as on every owner route.

## Media

Six routes serve bytes through the same machinery (`internal/httpapi/media.go`
`serveObject`): `video.mp4`, `light.mp4`, `social.mp4`, `poster.jpg`, `preview.gif` and
`filmstrip.jpg` are **anonymous** and require the clip to be `ready` (**409** `not_ready` otherwise, JSON —
except on `filmstrip.jpg`, where since erratum 227 that 409 is the OWNER's answer and every
other caller who finds no sheet gets **404**, above);
`original` is **owner**-only, in every state — a failed transcode still leaves the source
downloadable — and follows the ordinary precedence: **401** with no resolvable credential,
**403** `token_not_allowed` for a Bearer token, **403** `forbidden` for a session that is not
the owner. All of them:

- Support `Range`, `If-None-Match`, `If-Modified-Since`, `If-Range` exactly per RFC 9110.
  A single range → **206** + `Content-Range`; an unsatisfiable one → **416** with
  `Content-Range: bytes */size` and **zero** storage calls; a matching `If-None-Match` →
  **304** with zero storage calls; **HEAD** never touches storage either.
- `ETag` is the hex SHA-256 of the object's own bytes, computed once at write time — the
  same value regardless of which S3-compatible provider is behind it.
- `Cache-Control: public, max-age=3600` (or `private, max-age=3600` for `original`) —
  short and ETag-revalidated, **never** `immutable`: a trim or a GIF regenerate changes
  the bytes under these same stable URLs, and a client must re-check within the hour.
- `X-Content-Type-Options: nosniff`, and no `X-Frame-Options`/CSP (those guard pages).

**With `MEDIA_DELIVERY=redirect`, `video.mp4`, `light.mp4`, `social.mp4` and `poster.jpg` answer `302` instead of the bytes**
(DESIGN.md §20 erratum 278, DEPLOY.md §33). Every status above is unchanged — 404, 409, 401/403
and 503 are decided first, and only a request that would have been served gets a redirect — but
the successful answer becomes `302` with `Location` = a freshly presigned URL on the bucket's own
origin, `Cache-Control: no-store`, and **no** `ETag`, `Last-Modified`, `Accept-Ranges`,
`Content-Type` or body: those describe bytes this response does not carry. `Range` and the
conditional headers are then the TARGET's to answer, so no `206`, `304`, `412` or `416` comes from
this app; `?download=1` travels to the bucket as `response-content-disposition=attachment`. `HEAD`
gets the same `302`. **`original`, `preview.gif` and `filmstrip.jpg` are never redirected**, in any
mode: the first has an identity gate, and the other two are the routes this app's own JavaScript
polls with `fetch`, which cannot read the status of a cross-origin redirect. A client that follows
redirects (every browser, `curl -L`, Discord's media proxy)
sees no difference; a client that does not must be taught to. The default, `proxy`, is every word
above this paragraph.

```sh
$ curl -sI http://localhost:8481/api/clips/Z6eQucmYEyFQ/video.mp4
HTTP/1.1 200 OK
Accept-Ranges: bytes
Cache-Control: public, max-age=3600
Content-Length: 1710000
Content-Type: video/mp4
Etag: "d11a0bb00c6850fe0ae8c7b47cd34fc2c3d4177763b7474d9377c13981cc6c2f"
Last-Modified: Mon, 07 Sep 2026 10:38:33 GMT

$ curl -sI -H 'Range: bytes=0-99' http://localhost:8481/api/clips/Z6eQucmYEyFQ/video.mp4
HTTP/1.1 206 Partial Content
Content-Range: bytes 0-99/1710000
Content-Length: 100
...
```

### `GET`/`HEAD /api/clips/{id}/video.mp4?download=1` — anonymous

**The best playable tier of the clip.** Which object that is depends on the upload
(DESIGN.md §20 erratum 201, DEPLOY.md §27): the FULL re-encode when the source needed one,
the `-c copy` remux of a playable original, the UPLOAD ITSELF when it was already a
faststart mp4 — in which case this URL and `/original` serve the same bytes with the same
ETag — or the LIGHT copy while a Full is still encoding. The URL never changes and your
client should not need to know which case it is; a client that DOES care reads `tiers[]` on
the ClipDTO. `?download=1` is the **only** query parameter this route understands:
it adds `Content-Disposition: attachment; filename=<title-slug>.mp4` and changes
**nothing else** — identical bytes, ETag, Range and 304/412/416 handling (DESIGN.md §20
erratum 98). Any other value of `download` is silently ignored, never an error, because
this parameter is reached from a link on the share page and a link must never be a way to
make the route fail.

### `GET`/`HEAD /api/clips/{id}/audio?download=1` — anonymous (an `audio` clip)

An audio clip's best playable tier under its REAL media type (DESIGN.md §20 erratum 328): the stored
original when it plays in every browser (MP3 `audio/mpeg`, AAC `audio/mp4`, Opus or Vorbis in Ogg
`audio/ogg`, FLAC `audio/flac`), otherwise its Full (a FLAC — bit-exact for integer PCM, ALAC,
WavPack and TTA; 24-bit for float, gain applied when the source is over full scale — or AAC 256k in an
MP4 for anything else). Ranges, validators and the redirect rule exactly as `video.mp4`. `?download=1`
saves it as `<title-slug>.<its own extension>`. A VIDEO clip answers **404** here. For an audio clip,
`video.mp4`, `light.mp4` and `social.mp4` serve its CARD COPY — the card picture under the sound,
H.264 + AAC — which is what every card tag and the embed page play. `preview.gif`, `filmstrip.jpg`,
the trim, the GIF, the chosen card frame and the replacement answer **409** `not_for_audio`
(`Not for audio.`). The verification plane's guard (clippet-pass) lists routes by name, so on a Clippet
Pass name this route answers 404 until the guard's next release adds it.

### `GET`/`HEAD /api/clips/{id}/light.mp4` — anonymous
### `GET`/`HEAD /api/clips/{id}/social.mp4` — anonymous

**One object, two paths.** The card's own file: the LIGHT tier when the clip has one, and
the best playable tier otherwise (the same `COALESCE` this route has always done, over the
tier order of erratum 201). `/light.mp4` is the canonical name — it is what `tiers[]` and
`urls.light` carry, and what DEPLOY.md §27 calls it — and `/social.mp4` is the name every
card tag points at and every client written before the tiers uses. Both keep working, for
ever: a card URL that moved would re-fetch nothing while the old one still answered.

The URL never changes as the clip's tiers do: a client should not need to know which case it
is, and one that cares reads `card_tier` on the ClipDTO.

### `GET`/`HEAD /api/clips/{id}/poster.jpg` — anonymous

`image/jpeg`. Frame extracted from the finished rendition. For an AUDIO clip it is `image/png`: the clip's
waveform drawn by the server, 1280×720, with its length and the mark (DESIGN §20 erratum 329); the name stays
`poster.jpg`, the media type says what the bytes are.

### `GET`/`HEAD /api/clips/{id}/original?download=1` — owner, **or anonymous when the upload IS the clip**

The uploaded bytes. `ETag` is `source_etag` (computed at ingest, distinct from the
rendition's), and it is served in **every** state but `deleting` — the source is durable from
the 201/202 on, so a clip whose transcode failed can still hand its upload back. **Never**
409: there is no "not ready" for the original.

**Two audiences, and the row decides which** (DESIGN.md §20 erratum 207). When the upload is
web-playable AND was not remuxed AND the clip is untrimmed AND `ready` — the case where this
URL is what the watch page STREAMS, and where `tiers[]` reports
`{"tier":"original","playable":true,"url":"…/original"}` — the route is **anonymous** and is
served INLINE: no `Content-Disposition`, `Cache-Control: public, max-age=3600`, no `Vary`. In
every other case it is owner-session-only exactly as it always was: `private, max-age=3600`,
`Vary: Cookie, Authorization`, and
`Content-Disposition: attachment; filename=<sanitized original filename>`.

One exception to the public pair, and it applies to every media route: a response that is
carrying a `Set-Cookie` — which this one can, because the sliding session touch re-issues the
cookie on whatever response it lands on — is always `private, max-age=3600` with
`Vary: Cookie, Authorization`. A credential-bearing answer is never publicly cacheable
(DESIGN.md §20 erratum 207, amended). The anonymous request, which is the one a CDN is there
for, carries no cookie and gets the public pair.

`?download=1` asks for the attachment either way, which is what the share page's "Download
original" link sends. It is the same parameter, with the same rule, as on `video.mp4`: same
bytes, same validators, presentation only (erratum 98).

The `filename` parameter is **unquoted** whenever the sanitized name is a bare RFC 2045
token, which is the ordinary case: the header is built with Go's `mime.FormatMediaType`,
which quotes only when a character forces it (and adds the RFC 5987 `filename*` form when
the name is not ASCII). Parse it with a real header parser rather than by stripping quotes.

This route follows the [precedence rule](#conventions) exactly, so all four answers are
distinct — an anonymous caller is **401**, not 403:

The four answers below are the OWNER-ONLY case — a clip whose upload is not what it plays.
A clip whose upload IS what it plays answers **200** to all four callers, because the link is
then the auth exactly as it is for `video.mp4`:

```sh
# the upload IS the clip: anonymous, inline, publicly cacheable, and it ranges
$ curl -sI https://clips.example.com/api/clips/Z6eQucmYEyFQ/original
HTTP/1.1 200 OK
Accept-Ranges: bytes
Cache-Control: public, max-age=3600
Content-Type: video/mp4
ETag: "e5673cf6ea386cff4a51823abfc091c9d7abd039c441751224e4c47523be84c7"
```

```sh
# the owner, with a session cookie
$ curl -sI -b cookies.txt https://clips.example.com/api/clips/Z6eQucmYEyFQ/original
HTTP/1.1 200 OK
Accept-Ranges: bytes
Cache-Control: private, max-age=3600
Content-Disposition: attachment; filename=clip.mp4
Content-Length: 1923666
Content-Type: video/mp4
ETag: "e5673cf6ea386cff4a51823abfc091c9d7abd039c441751224e4c47523be84c7"
Vary: Cookie, Authorization

# no credential at all — 401 from the middleware, before the clip is looked up
$ curl -s http://localhost:8481/api/clips/Z6eQucmYEyFQ/original
{"error":"unauthorized","message":"authentication required"}                          # 401

# a real Bearer token: this is a session-only route (erratum 19)
$ curl -s -H "Authorization: Bearer $TOKEN" \
    http://localhost:8481/api/clips/Z6eQucmYEyFQ/original
{"error":"token_not_allowed","message":"this endpoint requires a browser session"}    # 403

# a valid session belonging to somebody else — the ONLY 403 forbidden here
$ curl -s -b other-cookies.txt https://clips.example.com/api/clips/Z6eQucmYEyFQ/original
{"error":"forbidden","message":"only the owner may download the original"}            # 403
```

Errors (owner-only case): **401**; **403** `token_not_allowed` (a Bearer token) or
`forbidden` (a session that is not the owner); **404** (unknown, malformed id, or
`deleting`); **416**; **503**. One behaviour changed with erratum 207: because the row is now
read before the credential is judged, an anonymous request for an **unknown** id answers
**404** where it used to answer 401 — the same information `GET /api/clips/{id}` already
gives anonymously.

## Views

### `POST /api/clips/{id}/view` — anonymous (rate: `view`, 60/min/IP)

No body. **204** either way (never leaks whether the increment happened). Counted once per
`(clip, resolved client IP)` inside a rolling dedupe window (`VIEW_DEDUPE_TTL`, default 30
minutes) — the caller-supplied User-Agent was dropped from the key by §20 erratum 103 (it let
any link holder mint fake views by varying it). A page load or an unfurl fetch never counts;
only the player's own first `play` event fires this from the browser. View counts are
**never** exposed to anyone but the owner (see `ClipDTO.views`) — there is no route that
answers with someone else's count.

```sh
$ curl -s -X POST http://localhost:8481/api/clips/Z6eQucmYEyFQ/view -w '%{http_code}\n'
204
```

Errors: **404** `not_found`.

## Collections

An ordered list of links to clips — nothing else: no likes, no comments, no author byline,
no view count. `POST`/`PATCH`/`DELETE`/`PUT …/clips` are **session**-only, scoped by
owner; a **404** from any of them means "not there" *and* "not yours" indistinguishably
(they scope by `owner_id` in the lookup itself, unlike clips, whose public `GET` means the
403-vs-404 split is possible there). `GET /api/collections/{cid}` is the one **public**
route in the group — the id is the auth, same as a clip's.

### `POST /api/collections` — session (CSRF)

```json
{"title": "Best of 2026"}
```
1–200 characters, same rule as a clip title. **201** `CollectionDTO` (no `clips` key — see
below). **400** `invalid_request` (title shape); **401**; **403** (`token_not_allowed` or
`csrf`); **409** `too_many_collections` (500 per user, DESIGN.md §20 erratum 82); **415**;
**500** `id_collision` (astronomically unlikely — collections.go).

```sh
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/collections \
    -H 'Content-Type: application/json' -d '{"title":"API doc collection"}'
{"id":"tRtDPzBjICct","title":"API doc collection","url":"https://clips.example.com/c/tRtDPzBjICct",
 "clip_count":0,"created_at":"2026-09-07T10:38:59Z","updated_at":"2026-09-07T10:38:59Z"}
```

### `GET /api/collections` — session or `read` token

**200** `{"collections": [CollectionDTO, ...]}` — the caller's **own**, newest first, no
`clips` key on any of them (shells only), no pagination (the 500 cap bounds it). There is
no route that lists somebody else's collections. Widened by exactly the same rule as
`GET /api/clips` above (§20 erratum 111): a token minted with the `read` scope may call
this route too; an upload-only token still gets **403** `token_not_allowed`.

### `GET /api/collections/{cid}` — anonymous (rate: `view`, 60/min/IP)

The one route that answers **with** `clips`: `CollectionDTO` plus every member, in the
collection's own order, as **public** `ClipDTO`s (no `views`, no owner-only fields — this
is a list of links, not the owner's dashboard) for every caller including the owner
themselves. `clips` is **always present**, `[]` for an empty collection (DESIGN.md §20
erratum 82) — never absent and never `null` on this one route.

```sh
$ curl -s http://localhost:8481/api/collections/tRtDPzBjICct
{"id":"tRtDPzBjICct","title":"API doc collection", ...,
 "clips":[{"id":"Z6eQucmYEyFQ", "title":"Standing 720p clip", ... }], ...}
```

### `PATCH /api/collections/{cid}` — session-owner (CSRF)

```json
{"title": "..."}
```
Rename only. **200** `CollectionDTO`. **400** `invalid_request`; **401**; **403**
(`token_not_allowed` or `csrf`); **404** for missing **or** not-yours; **415**.

### `DELETE /api/collections/{cid}` — session-owner (CSRF)

**204**. Removes the collection and its membership rows only — **no clip is touched**
(deleting a collection never deletes a clip). **401**; **403** (`token_not_allowed` or
`csrf`); **404** for missing **or** not-yours.

### `PUT /api/collections/{cid}/clips` — session-owner (CSRF)

```json
{"clip_ids": ["Z6eQucmYEyFQ", "..."]}
```
Replaces the **whole ordered membership** in one transaction; the array order is the
render order. The key is required — `{}` is **400**, `[]` empties the collection
(DESIGN.md §20 erratum 82, amended: PUT semantics, unlike the sibling PATCH's
"absent = unchanged", because "leave it alone" and "delete everything" on neighboring
routes would otherwise be one silent typo apart). At most 200 ids; duplicates collapse to
their first position.

```sh
$ curl -s -b cookies.txt -X PUT https://clips.example.com/api/collections/tRtDPzBjICct/clips \
    -H 'Content-Type: application/json' -d '{"clip_ids":["Z6eQucmYEyFQ"]}'
{"id":"tRtDPzBjICct", ..., "clip_count":1, ...}
```

Errors: **400** `not_your_clip` naming the first id that is not a clip the caller owns —
unknown, malformed, someone else's and tombstoned ids all answer identically, so this
route is never an existence oracle for the clip id space; **400** `invalid_request` (past
the 200-id cap); **401**; **403** (`token_not_allowed` or `csrf`); **404** for a collection
that is missing **or** not-yours; **415**.

### `POST /api/collections/{cid}/clips/{id}` — session-owner (CSRF)

No body (one is ignored). One clip at a time, beside the whole-membership `PUT` above
(ROADMAP #17; §20 erratum 151) — the route the clip page's "Collections" checkboxes tick.
The clip is **appended** to the end, leaving the order the owner already chose alone.
**200** `CollectionDTO` (no `clips` key — the same shape `PUT` answers with). Adding a clip
that is **already** a member is 200 too, and keeps its position: the caller asked for a
state, and that state already holds.

```sh
$ curl -s -b cookies.txt -X POST https://clips.example.com/api/collections/tRtDPzBjICct/clips/Z6eQucmYEyFQ
{"id":"tRtDPzBjICct","title":"API doc collection", ...,"clip_count":1, ...}
```

Errors: **400** `not_your_clip` (a malformed, unknown, tombstoned or another user's clip id
— one answer for all four, before any database access for a malformed one); **400**
`invalid_request` ("a collection holds at most 200 clips" — the same cap and the same
`writeCollectionFull` the whole-list `PUT` above answers with, hit here when the collection
is already at `db.MaxClipsPerCollection` and this clip is not already a member); **401**;
**403** (`token_not_allowed` or `csrf`); **404** `not_found` for a collection that is
missing **or** not the caller's.

### `DELETE /api/collections/{cid}/clips/{id}` — session-owner (CSRF)

**204** — the membership row and **nothing else**; the clip itself is untouched.
**Idempotent in the clip** (removing one that is not a member is still 204: the requested
state already holds) but **never idempotent in the collection** (a collection that is
missing or not the caller's is 404, so this route can no more be used to probe somebody
else's ids than any other collection route can).

```sh
$ curl -s -b cookies.txt -X DELETE https://clips.example.com/api/collections/tRtDPzBjICct/clips/Z6eQucmYEyFQ -w '%{http_code}\n'
204

# removing it again: still 204, the clip was already not a member
$ curl -s -b cookies.txt -X DELETE https://clips.example.com/api/collections/tRtDPzBjICct/clips/Z6eQucmYEyFQ -w '%{http_code}\n'
204
```

Errors: **400** `not_your_clip` (a malformed clip id); **401**; **403**
(`token_not_allowed` or `csrf`); **404** `not_found` (collection missing or not the
caller's).

### `GET /c/{cid}` — anonymous (page, not JSON)

The collection's HTML page: title, its clips as links, a link-out card for unfurlers. Not
this reference's subject (see [Pages](#pages)) — listed here because it is the collection
link an owner actually pastes.

## oEmbed

### `GET /oembed?url=&format=json[&maxwidth=&maxheight=]` — anonymous (rate: `view`, 60/min/IP)

oEmbed discovery (ROADMAP #18; §20 erratum 152). Slack's documented unfurl order is oEmbed
first and the Open Graph/Twitter tags second, discovered through a `<link rel="alternate"
type="application/json+oembed">` the share and embed pages already carry. **Discord and X
are completely unaffected** — they read the same `og:*`/`twitter:*` tags they always did —
so this is reach, not a behavior change for either of them. It is **not** under `/api/` —
the `<link>` a consumer follows names `/oembed`, which is where every oEmbed provider
expects it.

- `url` (**required**): a `/{id}`, `/clips/{id}` or `/embed/{id}` URL on
  **`PUBLIC_BASE_URL`'s own origin**. `/{id}` is the share URL the page advertises now
  (erratum 248); `/clips/{id}` is kept because a consumer's stored URL — and every link
  somebody already pasted — names it, and answering 404 for those would break a card that
  still works everywhere else — the host check is the point of the route: without it, an oEmbed endpoint that
  takes a URL from the caller and describes it is a small open proxy for metadata. A query
  string and fragment on it are **ignored**, not refused (a consumer routinely carries
  tracking parameters that still name the same clip); userinfo (`user@host`) is refused
  outright.
- `format`: absent or `json` only. `xml` gets the spec's own **501** `unsupported_format`;
  anything else is **400**.
- `maxwidth`/`maxheight`: 1–10000, scale the player **down** only — never up past the
  clip's own measured size.

**200**, the oEmbed 1.0 document. A **ready** clip is `type:"video"` and carries `html` —
the exact `<iframe>` snippet the share page's "Copy embed" hands out, so an oEmbed consumer
and a person pasting into a forum never end up with two different players. Any other
state (or an unmeasured clip) is `type:"link"` with no `html` at all and the placeholder
card image — there is nothing to embed for a clip that is still encoding or has failed. An
**unlisted** clip answers exactly as a listed one does: the link is the auth, same as the
share page. `provider_name` is this instance's own public **host** — the hostname of
`PUBLIC_BASE_URL`, with no scheme and no port — which is the same string the share page's
`og:site_name` carries, so a consumer that renders the provider line (Slack does, Discord does)
shows which box served the clip, by a name a reader could type. `INSTANCE_BADGE` does not
reach it: `dev.clippet.gg` already says which instance that is (DEPLOY.md §7.1, §28).

```sh
$ curl -s 'https://clips.example.com/oembed?url=https://clips.example.com/Z6eQucmYEyFQ&format=json'
{"version":"1.0","type":"video","provider_name":"clips.example.com","provider_url":"https://clips.example.com",
 "title":"Standing 720p clip",
 "html":"<iframe src=\"https://clips.example.com/embed/Z6eQucmYEyFQ\" width=\"1280\" height=\"720\" frameborder=\"0\" allowfullscreen allow=\"autoplay\"></iframe>",
 "width":1280,"height":720,
 "thumbnail_url":"https://clips.example.com/api/clips/Z6eQucmYEyFQ/poster.jpg",
 "thumbnail_width":1280,"thumbnail_height":720}

$ curl -s 'https://clips.example.com/oembed?url=https://clips.example.com/Z6eQucmYEyFQ&format=json&maxwidth=320&maxheight=180' \
    | python -c 'import json,sys;d=json.load(sys.stdin);print(d["width"],d["height"])'
320 180

$ curl -s 'https://clips.example.com/oembed?url=https://clips.example.com/Z6eQucmYEyFQ&format=xml' -w '\n%{http_code}\n'
{"error":"unsupported_format","message":"only format=json is supported"}
501

# a URL on somebody else's host: 404, the same answer a gone clip gets, so the
# route reports on no other origin
$ curl -s 'https://clips.example.com/oembed?url=https://example.com/clips/Z6eQucmYEyFQ&format=json'
{"error":"not_found","message":"no such clip"}
```

Errors: **400** `invalid_request` (no `url`, an unknown `format`, a `maxwidth`/`maxheight`
outside 1–10000); **404** `not_found` — for a URL that is not this origin's, an invalid or
unknown id, or a `deleting` tombstone, all one answer; **429** `rate_limited`; **501**
`unsupported_format` (`format=xml`); **503**.

## The Discord bot's interactions endpoint

### `POST /api/discord/interactions` — **Discord only** (rate: `bot`, 60/min/IP)

This route is documented for completeness, not for integrators: **Discord is the only
caller**, and the only credential it accepts is an Ed25519 signature made with the
application's private key, which only Discord holds. There is nothing here for a client of
yours to call, and nothing it can do that `POST /api/upload` does not do better.

It **does not exist** unless `DISCORD_APP_ID`, `DISCORD_PUBLIC_KEY` and `DISCORD_BOT_TOKEN`
are all set (DEPLOY.md §30): with the bot off the path answers the router's ordinary
**404** `not_found`, like any other unknown route. No session, no Bearer token and no CSRF
check are involved — there is no browser in this exchange.

**Authentication.** Every request must carry `X-Signature-Ed25519` (hex) and
`X-Signature-Timestamp`, and the signature must verify against `DISCORD_PUBLIC_KEY` over
the timestamp concatenated with the **raw** request body. Anything else — a missing header,
a malformed one, a signature over other bytes — is **401 with an empty body**. That is not
a stylistic choice: Discord validates an Interactions Endpoint URL by sending a request
with a deliberately invalid signature and refuses to save a URL that answers 200.

**Body**: Discord's interaction object, at most 64 KiB. This app reads `type`, `token`,
`guild_id`, `data.name`, `data.type`, the ATTACHMENT option in `data.options[]` and the file it
names in `data.resolved.attachments{}`, the STRING option named `link`, and — for the message command — `data.target_id` and that message's `content`, `author.id` and `attachments[]` in `data.resolved.messages{}`; when `DISCORD_ENRICH_UPLOAD=on` and that message has a video and no link to a clip here (§20 erratum 323), also `member.user.id` and `member.permissions` — the two ids are compared and dropped, never stored or logged; for
`/settings` and its panel (§20 erratum 318) `member.permissions`, `data.custom_id`, and a modal
submit's `data.components[]` (each Label's one `component`: its `custom_id` and its `value` or
`values`); for `/upload`'s form (§20 erratum 320) the same `data.custom_id` and `data.components[]`. Nothing about the member is read for the form. Everything else in the body is ignored, including any other option.

**The four commands** (DESIGN.md §20 errata 315, 318; one PUT per guild publishes all four):

| Command | Discord type | Option | Named by (default) |
|---|---|---|---|
| `/upload` | 1 (CHAT_INPUT) | `file`, 11 (ATTACHMENT), **required** — a snowflake resolved in `data.resolved.attachments{}` | `DISCORD_UPLOAD_COMMAND_NAME` (`upload`) |
| `/clip` | 1 (CHAT_INPUT) | `link`, 3 (STRING), **required** — a share link of THIS instance (`{origin}/{id}`, `/clips/{id}`, `/embed/{id}`, `{host}/{id}`, inside Discord's `<…>` or `\|\|…\|\|`). Parsed, never fetched | `DISCORD_COMMAND_NAME` (`clip`) |
| `Enrich` | 3 (MESSAGE) — a message's Apps menu | none: the picked message is the input | `DISCORD_ENRICH_COMMAND_NAME` (`Enrich`) |
| `/settings` | 1 (CHAT_INPUT), `default_member_permissions: "32"` (Manage Server) | none | `DISCORD_SETTINGS_COMMAND_NAME` (`settings`) |

An instance answers exactly the names it publishes (DEPLOY.md §30.2); a message command is routed by
`data.type` first, since Discord keeps those names in a namespace of their own. Every other name is
the "Clippet does not know that command." row below. The refusals that *name* a command carry
whichever name this instance answers; the sentences below are the default spelling.

**The message** — what a successful `/upload` ends in (an edit of the deferred reply, or a follow-up after the form), and what `/clip` and `Enrich`
answer with — is ONE builder's output, public (no `flags`):

```json
{"content":"https://clips.example.com/smW1BNY9aa23","allowed_mentions":{"parse":[]}}
```

`content` is the share link alone, in every state, and nothing else: no `embeds`, no `components`,
no `flags` (DESIGN.md §20 erratum 316, amending 315). Discord unfurls the link under the message into
the card the clip's page declares (`CARD_COMPONENTS`, DEPLOY.md §28) — a bot message carrying an
embed of its own never gets that unfurl, which is why the embed 315 added is gone.

**Answers** — always **200** with an interaction response, whatever the outcome:

| Interaction | Answer |
|---|---|
| `type: 1` (PING) | `{"type":1}` — the handshake the developer portal performs on Save |
| `/upload` in a **bound** server with a usable video | `{"type":9,"data":{"custom_id":"cb:form:<32 hex>","title":"Your clip is uploading","components":[…]}}` — a modal: a Text Display, then three Labels (`Title`: a Text Input pre-filled from the file name; `Delete after`: a Radio Group; `Tags`: a Text Input). The upload starts at once; meanwhile a private follow-up "Uploading." (ephemeral, deleted when the job ends), then the message posted as a follow-up on the command's token. With `DISCORD_UPLOAD_FORM=off`, or while 256 forms are open: `{"type":5}` (deferred, public), edited later into the message. Either way, a link posted before its card was ready is edited once more when it is — the same link plus `?v=2` — so Discord draws the card (§20 erratum 330) |
| `type: 5` (modal submit) `custom_id` `cb:form:<32 hex>` — no permission gate: the id is the credential, single-use, fifteen minutes, this server only | the title, delete-after and tags written to the clip that `/upload` created, through the same writer as `PATCH /api/clips/{id}`, then ephemeral "Changes saved." — and when the link was already posted and the title changed, the bot's message is edited to the same link plus `?v=2` (`?v=3` when it was already edited once, for a late card: §20 erratum 330) so Discord redraws the card (§20 erratum 325); bad tags: the rest is written, ephemeral "Saved without the tags. …"; an unknown, expired or already-sent id: ephemeral "That form has expired."; the upload failed: ephemeral "That clip could not be uploaded."; a delete-after outside the choices (a forged body): ephemeral "That is not a choice here.", nothing written and the form kept; a title past its rule (a forged body): ephemeral "Not saved: a title is at most 200 characters, with no control characters.", nothing written and the form kept; the clip deleted before the submit: ephemeral "That clip is gone."; the upload was a duplicate (§20 erratum 326): ephemeral "Already in your library. The form was not applied.", nothing written |
| `/clip` with a link to a clip here, in a **bound** server | `{"type":4,"data":<the message>}` — at once, public, in every state but gone (an unlisted clip too); a clip whose card is not ready yet has that reply edited to the same link plus `?v=2` once it is (§20 erratum 330) |
| `Enrich` on a message whose text carries a link to a clip here, in a **bound** server | the same, for the FIRST such link (a video beside it is not uploaded) |
| when `DISCORD_ENRICH_UPLOAD=on`: `Enrich` on a message with no such link and a video attached (the FIRST attachment that reads as a video), from the message's author (`member.user.id` = the message's `author.id`) or a member holding Administrator or Manage Server, in a **bound** server | handled exactly as `/upload` with `DISCORD_UPLOAD_FORM=off` (§20 erratum 323): its refusals, then `{"type":5}`, edited later into the message — never the form |
| when `DISCORD_ENRICH_UPLOAD=on`: that `Enrich` from anybody else | ephemeral: "That video is not yours. You need Manage Server here." — nothing is fetched |
| any command in an **unbound** server | `{"type":4,"data":{"content":"This server is not connected to a Clippet account.","flags":64}}` — ephemeral |
| `/upload` in a DM, with no attachment, with a non-video, or with a file past `MAX_UPLOAD_BYTES` | the same ephemeral shape, one line each ("Attach a video to /upload." for no attachment — a `link` option on `/upload` is no attachment) |
| `/clip` naming another host, a route or id that is no clip here, or nothing | ephemeral: "That is not a link to a clip on this Clippet." |
| `/clip` or `Enrich` naming a clip being deleted or past its expiry | ephemeral: "That clip is gone." |
| `Enrich` on a message with no link to a clip here and no video — or any video, with `DISCORD_ENRICH_UPLOAD` off (the default) | ephemeral: "No link to a clip on this Clippet in that message." |
| `/clip` with an **attachment** and no link (the `/clip file:` every server held before erratum 315, until it is re-registered) | handled exactly as `/upload` |
| `/settings` in a **bound** server, from a member holding Administrator or Manage Server | `{"type":4,"data":{"flags":32832,"components":[{"type":17,"components":[…]}]}}` — ephemeral, Components V2: a Section ("Link defaults", button `Edit` = `cb:defaults`) and the "Who may clip" line |
| `type: 3` (component) `custom_id` `cb:defaults`, same gate | `{"type":9,"data":{"custom_id":"cb:defaults:modal","title":"Link defaults","components":[…]}}` — a modal of three Labels (`Delete after`: a Radio Group; `Unlisted`: a Checkbox; `Link`: a Radio Group), pre-filled from the bound account's defaults |
| `type: 5` (modal submit) `custom_id` `cb:defaults:modal`, same gate | the three values written through the same code as `PATCH /api/auth/me/settings`, then ephemeral "Changes saved."; a value outside the choices: ephemeral "That is not a choice here.", nothing written |
| `/settings` or any `cb:` interaction but the form's, from a member without Administrator or Manage Server | ephemeral: "You need Manage Server here." |
| any other command, any component or modal submit whose `custom_id` is not `cb:…` (or an unknown `cb:` one), autocomplete, or an unparsable body | the same ephemeral shape: "Clippet does not know that command." |

Errors: **401** (empty body) for the signature; **429** `rate_limited`. It answers no 5xx of
its own — Discord renders every 5xx as "The application did not respond", which tells nobody
anything.

**What happens after `/upload`'s deferred answer** (and Enrich's, on a video, with `DISCORD_ENRICH_UPLOAD=on`), since it is observable in the library. For a
`file`: the attachment is fetched from `cdn.discordapp.com` or `media.discordapp.net` (the
only two hosts this app will fetch from) and ingested through the SAME path as
`POST /api/upload`, as the account the server is bound to — that account's `ttl_days` and
`unlisted` defaults, that account's upload budget, the same ffprobe gate, the same
`CAPTURE_READY_WAIT` wait for the card. The clip that appears is an ordinary clip in an
ordinary library; nothing about it is marked as having come from Discord. That is the whole
of it: there is no other kind of job. The one route that fetches a URL its caller names is
`POST /in/auto/{token}`, and it fetches a Discord attachment or nothing.

### `GET /api/bot/claim?guild_id=<snowflake>` — anonymous (rate: `claim`, 30/min/IP — its own bucket, never the `bot` one)

The **origin proof** for the official Clippet bot (DEPLOY.md §30.8, DESIGN.md §20 erratum 279).
Before the official bot's claim page connects a Discord server to an instance, it asks that
instance this one question, and connects only on a 200. It exists exactly when the interactions
route above does — with the bot off, the router's ordinary **404** `not_found` — and it is
**inbound only**: nothing on this instance calls anywhere because of it.

| Answer | Meaning |
|---|---|
| **200** `{"bound":true,"app_id":"<DISCORD_APP_ID>","defaults":{"ttl_days":N,"unlisted":bool,"link":"compact"\|"memorable"}}` | the operator ran `clippet admin bot bind <guild_id> …` on this instance. `app_id` is the application this instance verifies signatures for (public: it is in every interaction), so a claim for a differently-configured box is refused before its first `/clip`. `defaults` (since 2026-10-12, erratum 318 amended) is the bound account's link defaults — `ttl_days` the effective term (0 = never) — which the official bot's `/settings` panel prints under "Link defaults"; absent when they could not be read |
| **404**, empty body | no binding for that guild id — one bit, no hint |
| **400** `invalid_guild_id` | `guild_id` is not 15–25 digits |
| **503** `unavailable` | the database could not be read |

It never names the bound account, a user id, a count or a list (the three defaults are preferences, not identity); `Cache-Control: no-store` on
every answer, because a bind or unbind changes it at once. GET only.

## Health

### `GET /healthz` — anonymous

Process + database liveness, answers within about a second of container start; never
blocks on storage. **200**:
```json
{"ok":true,"version":"dev","ffmpeg":"7.1.1","db":"ok",
 "storage":"pending"|"ok"|"error: ..."  (each optionally " (s3)"|" (fs)"),
 "queue":{"queued":n,"running":n,"failed_24h":n,"oldest_queued_age_s":n,"longest_running_age_s":n},
 "sweep":{"destroyed":n,"errors":n},
 "repair"?:{"repaired":n,"released":n,"requeued":n,"failed":n,"errors":n},
 "storage_budget"?:{"used":n,"budget":n,"over":false},
 "storage_usage"?:{"stored_bytes"?:n,"served_today_bytes"?:n,"served_month_bytes"?:n},
 "uptime_s":n}
```
`storage` is the **cached** result of the last background ping — never a live call, so a
storage outage cannot fail this endpoint (which is what the compose healthcheck polls).
Since ROADMAP #37 (DESIGN.md §20 erratum 276) it also names the backend that answered —
`"ok (s3)"`, `"error: unavailable (fs)"` — which is the one fact an anonymous prober
cannot otherwise establish about an instance whose storage is unwell: is this a bucket
problem or a disk problem. It is one of two words and names no endpoint, bucket, path or
credential. §15's "no config values" rule was **amended** for it rather than bent: it now
reads "no config values beyond the storage backend's name, which is one of two words", and
that word is the only one that has ever been added. A build older than that erratum, and
any process that has not named its backend, prints the bare form.
`queue`'s last three fields are additive (DESIGN.md §20 erratum 145): counts and ages in
seconds only, nothing that names a clip, a user or a configured value — `failed_24h` is
jobs that reached `failed` in the last 24 hours, `oldest_queued_age_s` is how long the
oldest **claimable** queued job has been waiting (0 when none is; a job still in its retry
backoff is not claimable, so a storage blip does not read as a stalled queue), and
`longest_running_age_s` is how long the longest-running job has been running (0 when none
is; past `TRANSCODE_HARD_TIMEOUT` it is probably a stuck row — DEPLOY.md §24.2).
`repair` is the automatic stuck-job repair's lifetime counters (DESIGN.md §20 erratum 231,
DEPLOY.md §24.6): `repaired` is every job this process put back on the queue or released
after finding it `running` with no worker — the sum of `released` (a job whose outputs were
already checkpointed, retried finalize-only with no attempt spent), `requeued` (one crash
attempt spent) and `failed` (its attempts ran out, so for a transcode its owner is now
told the encode failed) — and `errors` counts repair passes that could not read or write
the queue at all. Counts only, never `JOBS_UNSTICK_AFTER` itself: this route is anonymous
and names no configured value (§15), which is what `clippet admin doctor` is for. The
object is absent when no worker published the counters (a `clippet serve` that never
started one), never because the feature is off — with `JOBS_UNSTICK_AFTER=0` the numbers
are simply zero for ever.

`storage_budget` is present only when `STORAGE_BUDGET_BYTES` is set **and** the sweeper has
taken a reading (§20 erratum 157), and **nothing reads this object to refuse anything**:
`over:true` changes nothing the app does. The budget is advisory unless the operator armed
the cap with `STORAGE_BUDGET_HARD=1` (§20 erratum 277) — and even then the refusal is decided
at the upload from a LIVE database read, never from this cached reading, so the object's keys,
shape and meaning are identical on a capped instance and an uncapped one. The probe says
nothing about which it is, exactly as it says nothing about a price.

`storage_usage` is the storage-cost line's **measurements** (ROADMAP #39, DESIGN.md §20
erratum 270): what this instance stores, and what it has sent to viewers today and this
calendar month, as JSON numbers of bytes. Note the key — `storage` above is the store-ping
string and keeps its meaning; `storage_usage` is the object, beside `storage_budget`.

**No cost appears here, and none ever will.** This route is anonymous and is normally
reachable wherever the origin is, so an amount printed beside the byte count it was computed
from would hand any caller the operator's configured per-GB price, one division away — and
§15 says this route exposes no configured value. The cost estimate `STORAGE_PRICE_PER_GB_MONTH`
and `EGRESS_PRICE_PER_GB` produce lives on the signed-in Settings page and in
`clippet admin stats` (DEPLOY.md §31.4). A priced instance's `storage_usage` is byte-identical
to an unpriced one's.

**Every key is optional, and an absent one is never a zero.** `stored_bytes` is the sweeper's
cached reading and is absent until it has taken one (the same rule `storage_budget` follows);
the two served counts are absent when no counter is wired (a `clippet serve` always wires
one) or when their single read failed, which is logged and never fails the probe. The served
counts include the window the app has not flushed to its database yet, so they are exact the
instant a response ends; the counter itself is aggregate bytes per UTC day and carries nothing
about any viewer. **500** `{"ok":false,"db":"error: ..."}` only on an actual DB ping failure.

```sh
$ curl -s http://localhost:8481/healthz
{"ok":true,"version":"dev","ffmpeg":"7.1.1","db":"ok","storage":"ok",
 "queue":{"queued":0,"running":0,"failed_24h":0,"oldest_queued_age_s":0,"longest_running_age_s":0},
 "sweep":{"destroyed":0,"errors":0},
 "repair":{"repaired":0,"released":0,"requeued":0,"failed":0,"errors":0},"uptime_s":2120}
```

### `GET /readyz` — anonymous

DB liveness **plus a live** 2-second `store.Ping`. **200** `{"ok":true}`. **503**
`{"ok":false,"db":"ok"|"error: ...","storage":"pending"|"error: ..."}` (the same
backend suffix as above) — this is the one
an orchestration script (or this document's own author, bringing the stack up) should poll
before assuming the app is really ready to serve.

### `GET /api/instance` — anonymous (rate: `claim`, 30/min/IP — the bucket it shares with `/api/bot/claim`)

**What build this is, and how it serves media.** Four keys, always all four, in this order, and never
a fifth — the key set is a contract and DESIGN.md §20 erratum 303 is where it is recorded.
`Cache-Control: no-store`, because the answer changes with a rebuild or an `.env` edit. **200**:

```json
{"version":"2026.09.18","build":"4f1c0a9e5c2b8d3a7e6f1029384756abcdef0123",
 "media_delivery":"redirect","media_origin":"https://clips.example.r2.cloudflarestorage.com"}
```

| field | |
|---|---|
| `version` | what this build calls itself: the `CLIPPET_VERSION` build argument, or `"dev"` when it was never set. The same string `/healthz` reports |
| `build` | the **git commit** this binary was built from (the `CLIPPET_COMMIT` build argument, DEPLOY.md §16 and §17.4) — 40 lower-case hex characters, or the JSON literal `null` when it was not set. Anything that is not exactly that shape is published as `null` rather than as itself: a reader that looks a release up by it treats a value it cannot parse as drift, so a wrong shape is worse than an absent one. It is **not** the `?v=` cache-busting build id, which is a different string with a different job |
| `media_delivery` | `"proxy"` or `"redirect"`, and exactly one of those two words (DEPLOY.md §33). It is derived from the same predicate the media handlers and the CSP use, so the three cannot disagree |
| `media_origin` | in `redirect` mode, the `scheme://host` a viewer's player is sent to — the origin already visible in a media route's `Location` and in every page's CSP. `null` in `proxy` mode, which is every instance that serves its own media bytes |

**Anonymous, GET only** (`405` + `Allow: GET, HEAD` otherwise), **registered
unconditionally** — unlike `GET /api/bot/claim`, which exists only when the bot is configured, this
route is on every instance whatever else is set, because there is no feature behind it: the body is a
function of the build and `.env` alone. It sets no cookie and reads none.

Nothing else is in it. No clip, no account, no count, no bucket, no endpoint, no path, no queue and
no configured value beyond the two above — the same rule `/healthz` keeps (DESIGN.md §15). It exists
because a **Clippet Pass** name is verified from outside by a control plane that holds the release
artefacts and needs to know which release is answering and whether the box is serving media bytes
through the tunnel (DEPLOY.md §36). Both are facts about the process, and a process is the only
thing that can answer them.

## The setup wizard's store check

### `POST /api/setup/check` — session only, owner only

The `Check now` button on `/setup` (ROADMAP #45, DESIGN.md §20 erratum 297). It **takes no
body and has no input of any kind** — there is no endpoint field, no bucket field and no
credential field on the page it serves, which is the decision that makes this route
uninteresting to attack: there is nothing to SSRF with and nothing to validate that the
operator did not already put in `.env` and boot with.

It runs `store.Probe` against the store this instance is configured for **right now** —
`Ping`, put one small object under `{S3_KEY_PREFIX}doctor/`, delete it again, **no
read-back** — which is the identical check `clippet admin doctor`'s `bucket` row performs.
The probe key space is deliberately outside `clips/`, so `admin orphans` and the sweeper
can never see a probe object.

**200** (both outcomes; a store that refuses is an answer, not a server error):
```json
{"ok":true,"state":"ok","checked_at":"2026-09-17T09:41:02.318Z"}
```

| field | |
|---|---|
| `ok` | the probe completed |
| `state` | a CLOSED vocabulary: `ok`, `unreachable`, `timeout`, `denied`, `bucket missing`, `unavailable`. It is the §20 erratum 31 class set, and nothing outside it is ever returned — the library's own message carries the endpoint, the bucket and the access key id, and none of those reaches a page |
| `checked_at` | when the probe behind this answer actually RAN (RFC 3339, UTC) |

**Owner only** (DESIGN.md §20 erratum 349) — the instance's owner is the first account created
on it (`clippet admin users` marks it `owner=yes`); any other account's session gets **403**
`{"error":"not_owner","message":"Only the account that set up this Clippet can see this."}`
before any probe runs. **Session only** — a capture token gets `403 token_not_allowed`,
anonymous gets **401** — CSRF-guarded like every other browser-driven POST, and **it carries no rate limiter of its
own**. It does not need one: the answer is single-flighted and then cached for ten seconds
instance-wide, so a hundred presses are one probe and nobody is ever refused. Two calls
inside that window return the **same `checked_at`**, which is how a caller can tell.

`503 storage_unavailable` is the one other answer, and it means this process has no store
object at all — a configuration that could not build a client, which `admin doctor`'s own
rows explain.

## Pages

`GET /`, `/login`, `/register`, `/settings`, `/settings/tokens`, `/setup`, `/{id}`, `/clips/{id}`,
`/embed/{id}`, `/c/{id}`, `/static/{file...}`, `/robots.txt`, `/favicon.ico` are server-rendered HTML
or static assets, not JSON — outside this reference's scope (see DESIGN.md §9 for the share-page card
contract, the thing an unfurler actually reads). Three worth knowing about as an integrator:

- the library at `/` requires a **session** — unlike `GET /api/clips/{id}`, there is no
  anonymous "browse the instance" page (DESIGN.md §1's deliberate divergence, kept on
  purpose). A session-less `GET /` is a **200**, the front door
  (DESIGN.md §20 erratum 246): the wordmark, one line, a `Log in` link and, while
  registration is open, `Create an account` — no clip data, no card tags, nothing to
  integrate against; `private, no-store` + `Vary: Cookie`. It was `303 /login?next=/`
  before.
- `/embed/{id}` carries one link since the same erratum: the `clippet` wordmark, top-right,
  to the clip's share page with `target="_top"`, hidden while the video plays. The headers
  (`frame-ancestors *`, no `X-Frame-Options`), the `og:video`/`twitter:player` contract and
  the oEmbed document are unchanged.
- `/login/discord/{token}` is the bot's one-time login link (DESIGN.md §20 erratum 341, DEPLOY.md
  §30.12), and exists only while the bot is configured and `REGISTRATION` is `members` or
  `role:<id>` (otherwise the 404 page). `GET` renders a page with one button and changes NOTHING;
  the form's `POST` (form-encoded, `next`; a cross-site `Origin`/`Sec-Fetch-Site` is **403**)
  consumes the token once and answers **303** `next` with the session cookie `POST /api/auth/login`
  issues — or **303** `/login?discord=link` for a used, expired or unknown link.
- `/setup` is the first-run wizard (DESIGN.md §20 errata 297-299, DEPLOY.md §35): it renders
  the `.env` block for each of six storage answers and nothing else. It is the **owner's**
  (DESIGN.md §20 erratum 349): the first account created on the instance gets the page; any
  other signed-in account gets **403** with the heading and one sentence, `Only the account
  that set up this Clippet can see this.` (the steps-only view too); anonymous is sent to
  `/login` — with one exception: on an instance nobody has registered on yet it answers
  anonymously, because that is the state it exists for. `/settings`' Storage panel follows
  the same rule: the owner reads its figures, any other account its head and that sentence. It holds no credential, prints none, and links
  nowhere outside this origin.
- **`/{id}` is the URL every `url`/`urls.page` field in this document points at** — the id
  at the ROOT of the origin (DESIGN.md §20 erratum 248), the page an unfurler fetches and a
  human lands on after pasting a link. A single segment that is not a valid id is the 404
  page, for every method.
- `/clips/{id}` is that page's OLD address and answers **308** to `/{id}`, method-preserving
  (a 301 would let a client turn the unfurlers' `HEAD` into a `GET`), query string
  preserved, `public, max-age=3600` (`private, no-store` on the rare response that is also
  re-issuing a session cookie). It reads no row, so an id no clip has redirects exactly as a
  real one does and the hop lands on the same 404 — the redirect cannot be used to probe
  which ids exist. Discord, X, Slack and Facebook all follow permanent redirects, so
  every link already pasted still unfurls.
- `/settings` is the account page behind `PATCH /api/auth/password`, `GET`/`DELETE
  /api/auth/sessions` and the token routes above: the password form, the list of
  signed-in browsers and a "Sign out everywhere else" button. `/settings/tokens` is the
  Phase 1 URL kept answering (DESIGN.md §20 erratum 110): it **303**s to `/settings#tokens`
  rather than 404ing, so nothing that bookmarked or linked it breaks.
- `POST /settings/theme` — **no session needed**, **form-encoded** (`theme=system|light|dark`), not
  JSON: the no-JavaScript half of BOTH theme controls — the Settings picker and the top bar's
  light/dark glyph (DESIGN.md §20 errata 242 and 288, `DEPLOY.md` §28). It sets the `theme` cookie
  (`light`/`dark`, a year, `Path=/`, `SameSite=Lax`, `Secure` on https, not HttpOnly — `app.js`
  writes the same cookie from the page) or deletes it for `system`, and **303**s to the optional
  `next` field's value — a path on this origin, validated the way a login `next` is, anything else
  becoming `/` — or to `/settings#theme` when the form carries none. Any other `theme` value is
  **400** and `GET` is **405**. It is the one page route with no Authenticator around it: the glyph
  is in the header of a stranger's share page, and the handler writes one cookie about one browser
  and reads no row. The page handlers read the cookie and render `data-theme` on `<html>`; it is a
  browser preference, not account data, and no API route reports it.
- `GET /favicon.ico` — **anonymous**, and not HTML: one `image/vnd.microsoft.icon` holding PNG
  drawings at 16, 32 and 48 px (DESIGN.md §20 erratum 304, `DEPLOY.md` §28). It answered the 404 page
  before, which mattered to no browser — every page declares its own `data:` icon — and did matter to
  bookmark tools, some link previewers and Safari's pinned-tab fallback, which ask for this path
  without reading a page. `public, max-age=86400` (a day: the path carries no `?v=` and nothing
  renders it, so there is nothing to bust), a strong `ETag`, **304** on `If-None-Match`, `HEAD`, and
  **405** `Allow: GET, HEAD` on anything else. The same bytes are at `/static/favicon.ico`, under
  `/static/`'s ordinary hour. **The day is the origin's**: an instance reached through a Clippet Pass
  name is answered by the plane's guard, which writes its own policy over every static response — an
  hour, like the rest of `/static/`. The bytes, the media type and the `ETag` are the same string
  either way, so nothing a client does about this file changes (DESIGN.md §20 erratum 304 (g)).

## The ClipDTO wire format

The shape returned by `POST /api/clips`, `POST /api/clips/{id}/retry`,
`GET`/`PATCH /api/clips/{id}`, and embedded in `GET /api/clips` and
`GET /api/collections/{cid}`:

| Field | Type | Notes |
|---|---|---|
| `id` | string | `[A-Za-z0-9]{12}`, **or** three to four capitalised words joined (`PurpleUpdateCharacter`) — see `link_style` (DESIGN.md §20 erratum 249) |
| `title`, `description` | string | Never null; `description` is `""` when unset |
| `unlisted` | bool | Hidden from the shared library; also suppresses a Discord auto-post for this clip |
| `state` | string | `queued`\|`processing`\|`ready`\|`failed` (never `deleting` — a tombstoned clip is 404 everywhere) |
| `kind` | string | `video`|`audio` (DESIGN.md §20 erratum 328). Always present. An `audio` clip has `width`, `height` and `fps` null — it has no picture of its own; its card copy's size is on the `light` tier |
| `audio` | object|**absent** | An `audio` clip's one stream as stored: `{"codec","sample_rate","bit_depth","channels"}` (`bit_depth` null for a lossy codec). Absent on a video |
| `link_style` | string | `compact`\|`memorable` — which grammar `id` is in. **Derived from the id**, stored nowhere, always present, never null, not owner-private. Chosen at upload time and permanent: flipping a clip to `unlisted` later does not re-mint it |
| `failure_reason` | string\|null | Set only when `state="failed"` |
| `views` | int\|**absent** | **Owner-reader only** (a session, or since §20 erratum 112 a Bearer token carrying the `read` scope reading its own clip — `auth.Principal.OwnerReader`, pinned by `TestReadScopeMatrix`). The JSON *key itself* is omitted for every other caller — not `null`, absent. |
| `source_duration_ms` | int\|**absent** | Owner-reader only, same rule as `views` above. The original's length — the timeline every `trim` window is measured on. |
| `trim` | object\|**absent** | Owner-reader only, same rule as `views` above. `{"start_ms","end_ms"}` — the window the *current* rendition was cut from; absent (not present-and-null) when the clip is the whole original. |
| `tags` | []string | **Always** present, sorted, never null — `[]` means no tags. Not owner-private: it is visible to everyone this DTO already reaches. |
| `created_at`, `updated_at` | string | RFC3339 UTC |
| `ready_at` | string\|null | Null until the transcode finishes |
| `ttl_days` | int | `0` means **never** (never `null` on output — see below) |
| `expires_at` | string\|null | Null means never |
| `duration_ms`, `width`, `height`, `fps`, `has_audio` | number/string/bool\|null | Null until `ready`; `fps` is a string like `"30/1"` |
| `poster` | object\|null | `{"width","height"}` — the poster's own measured dimensions, null until `ready` |
| `gif_url` | string\|null | See below |
| `filmstrip` | object\|null | `{"url","tiles","tile_width","tile_height","interval_ms"}` — the trim scrubber's sprite sheet, measured. `null` until a `filmstrip` job has committed for the CURRENT rendition, and `null` again after a trim or a replacement drops it. See below. |
| `source` | object | `{"filename","size","content_type"}` of the original |
| `owner` | object | `{"id","username"}` — always the public owner reference |
| `urls` | object | `{"page","embed","video","social","light","original","poster","progress"}`, all absolute, built from `PUBLIC_BASE_URL` at request time. **`page` is `{PUBLIC_BASE_URL}/{id}`** — the id at the ROOT since erratum 248; `/clips/{id}` still answers, with a 308 to it. `social` and `light` are the SAME route under two names (erratum 210); `video` is "the best playable tier" |
| `tiers` | []object | **Always** present, never null, three entries in quality order (`original`, `full`, `light`). See below |
| `card_tier` | string | Which tier a crawler is actually served — `original`\|`full`\|`light`, or `""` for a clip that is not ready |

**`ttl_days` input** (`POST /api/clips`'s metadata part, `POST /api/upload`'s `ttl_days`
field, `PATCH`'s `ttl_days` key) preserves a three-way distinction that a plain nullable
integer cannot:

| Sent | Upload meaning | PATCH meaning |
|---|---|---|
| absent, or JSON `null` | apply `DEFAULT_CLIP_TTL` | leave the TTL unchanged |
| `0` | never expires | change to never |
| `1`, `7`, `30`, `90` | that many days from now | recompute `expires_at` from now |
| anything else (`-1`, `3.5`, `"7"`) | **400** `invalid_ttl` | **400** `invalid_ttl` |

This is why the DTO never emits `null` for "never" on output (`ttl_days:0` instead) — a
client that reads a `GET` and PATCHes it back unchanged must not turn "never" into
"apply the default" by round-tripping through `null`.

**`tiers` says what this clip HAS, and `url` is null for what it has not.** Each entry is
`{"tier","url","width","height","fps","bytes","codec","playable","state"}` (DESIGN.md §20
erratum 205, DEPLOY.md §27):

| field | meaning |
|---|---|
| `tier` | `original` \| `full` \| `light` — the list is always all three, in that order |
| `state` | `ready` (the file exists) \| `encoding` \| `failed` \| `skipped` (never needed — a finished answer, not a gap) \| `pending` (this clip is not `ready` yet and nothing has been decided) |
| `url` | the tier's own playback address, or **null** when it has no file. A client is never handed an address that 404s |
| `width`, `height`, `fps`, `bytes` | MEASURED, or null. A `full` that is still `encoding` carries the ORIGINAL's numbers, because that is the size it will be |
| `codec` | `h264`, `av1`, `hevc`, … as ffprobe named it; `""` for a clip uploaded before the tiers |
| `playable` | whether a BROWSER plays this tier's file. Always true for `full` and `light`; for `original` it is the probe's verdict, narrowed to "and the upload is still the whole of what this clip is" — so a TRIMMED clip reports its original present and not playable, which is the truth |

"the best tier that exists" is the first entry with `state:"ready"` and a non-null `url`,
which is also what `/video.mp4` resolves to server-side. A clip uploaded before this feature
existed reports `original` present-but-not-playable and its rendition as a ready `full`,
because that is what it is; nobody probed its upload for playability, and the API does not
guess.

**`gif_url` is a fact, not always an address.** It sits outside `urls` on purpose: every
field in `urls` answers at any time (even a 202 while a preview is being made), while this
one says "a preview currently exists" — `null` until a `gif` job has committed at least
once, and `null` again after a trim replaces the rendition the preview was cut from
(DESIGN.md §20 erratum 76). When it wants one, a client calls
`GET .../preview.gif` (which starts the job) rather than reading a URL that was never
there to read.

**`filmstrip` carries the geometry, not just the address** (§20 erratum 220). It is the one
field that is an OBJECT for the reason `gif_url` is outside `urls`, one step further on: a
client cannot place a single tile from a URL alone — it needs how many tiles there are, how wide
one is and how much clip time one covers. So the fact and the layout travel together, and
`null` means "no sheet for this rendition" — and, for the clip's OWNER, "ask the route and it
will make one" (§20 erratum 227: making one is the owner's, reading one is anybody's). Every
number is measured from the produced JPEG, so the tile of a moment `ms` on the RENDITION's
timeline is at x offset `floor(ms / interval_ms) × tile_width`, clamped to `tiles − 1`. The
field itself is not owner-private: a sheet that exists is served like `poster.jpg`, so its
geometry says nothing the video does not.

**`CollectionDTO.clips` has three states, not two** — this is the one field in this whole
API that is a genuine three-valued optional. `GET /api/collections/{cid}` always sends it
(`[]` for an empty collection); every *other* response that carries a `CollectionDTO`
(create, list, patch, the clips-PUT echo) omits the key entirely. A client must therefore
treat "key present, value `[]`" and "key absent" as different facts, not normalize them —
the shell-vs-full distinction is the whole reason the field is typed the way it is
server-side (a pointer to a slice, DESIGN.md §20 erratum 82).

## Rate limits

| Bucket | Limit | Keyed by | Routes |
|---|---|---|---|
| `auth` | 10/min | client IP | `POST /api/auth/register`, `POST /api/auth/login`, `PATCH /api/auth/password`, and the three Discord sign-in entry points `GET /api/auth/discord/start`, `GET /api/auth/discord/callback`, `POST /api/auth/discord` (a Discord login costs two tokens; DESIGN.md §20 erratum 302) |
| `upload` | 60/hour | account (user id) | `POST /api/clips`, `POST /api/upload`, `POST /in/{token}`, `POST /in/auto/{token}`, `POST /api/share`, `POST …/replace`, `POST …/trim`, `POST …/gif`, `POST …/poster` |
| `upload` (2nd bucket) | 120/hour | client IP (the URL's row on `/in/auto/{token}`, erratum 327) | **Both** `POST /api/clips` and `POST /api/upload`/`/in/{token}`/`/in/auto/{token}`/`/api/share`/`/replace` — an upload-class request must satisfy the account bucket **and** this one (DESIGN.md §20 erratum 97). It exists because accounts are free (one IP can register hundreds), not to punish a household behind one NAT — which is why it is looser than the account budget. |
| `view` | 60/min | client IP | `POST …/view`, `GET …/preview.gif`, `GET …/filmstrip.jpg`, `GET /api/collections/{cid}`, `GET /api/clips/progress` (the BATCHED one), `GET /oembed` |
| `poll` | 120/min | client IP | `GET /api/clips/{id}/progress` — its own bucket, because a watch page whose better tier is encoding polls it and must not spend a household's view budget (DESIGN.md §20 erratum 211, amended) |
| `bot` | 60/min | client IP | `POST /api/discord/interactions` — its own bucket, because Discord's interactions arrive from a small set of egress addresses and must not spend the `auth` budget every human behind the same proxy needs (DESIGN.md §20 erratum 256). The route exists only when the bot is configured. On an instance behind the **official** bot every connected server's `/clip` arrives with one fixed `X-Real-IP` and shares ONE of these buckets when the proxy is trusted (DEPLOY.md §30.8) |
| `claim` | 30/min | client IP | `GET /api/bot/claim` — its own bucket, because it is anonymous and public, and a stranger's GETs must not drain the `bot` bucket the official bot's `/clip`s need (DESIGN.md §20 erratum 279(h)). Exists only when the bot is configured. **`GET /api/instance` shares it** (erratum 303): the two are the same shape of anonymous machine GET, and a second limiter for a second such route is the thing that rule exists to prevent — so draining either refuses both |

`POST /api/clips/bulk` carries **no** route-specific limiter of its own — it is bounded
instead by the 100-id cap on one request and the session/CSRF gate every mutating owner
route already has.

A limited response is **429** `rate_limited` with `Retry-After: <seconds until the bucket
has room>`. "Client IP" is the address `internal/clientip.Resolve` computes — `RemoteAddr`
unless it is inside `TRUSTED_PROXIES`, in which case `X-Real-IP` (or a walked
`X-Forwarded-For`) is trusted instead (DESIGN.md §13; DEPLOY.md §4 and §26 for what an
operator sets this to). Every route not listed above carries no route-specific limiter of
its own (some, like media serving, are deliberately unlimited — a crawler must be able to
fetch card assets freely).

## Two complete recipes

Both were run as written against the live stack: the cookie-jar recipe over the instance's
https origin (a session cookie is `Secure`), the Bearer recipe over plain `http://localhost:8481`.

### The cookie-jar walkthrough (a second UI, a script driving the browser API)

```sh
BASE=https://clips.example.com
JAR=cookies.txt

# 1. register (or POST /api/auth/login if the account exists)
curl -s -c "$JAR" -X POST "$BASE/api/auth/register" -H 'Content-Type: application/json' \
  -d '{"username":"rc_recipe1","password":"correct horse battery staple"}'

# 2. upload — metadata part first, video part second
printf '{"title":"Standing 720p clip","ttl_days":7}' > meta.json
curl -s -b "$JAR" -X POST "$BASE/api/clips" \
  -F 'metadata=<meta.json;type=application/json' -F 'video=@clip.mp4;type=video/mp4'
# → {"id":"...", "state":"queued", ...}

# 3. poll until ready (state=ready|failed, or HTTP 404 = gone — never treat 404 as done)
curl -s "$BASE/api/clips/<id>/progress"

# 4. the finished share link is urls.page from the clip DTO, or build it yourself:
#    $BASE/<id>

# 5. clean up when you're done experimenting
curl -s -b "$JAR" -X DELETE "$BASE/api/clips/<id>"
curl -s -b "$JAR" -X POST "$BASE/api/auth/logout"
```

### The Bearer-token walkthrough (a capture tool — ShareX, RePlays, a script with no cookie jar)

```sh
BASE=http://localhost:8481

# 1. mint a token ONCE, from a browser session (there is no way to mint one with curl
#    alone — a token can only be created by something that is already logged in)
#    → POST /api/auth/tokens from your browser's dev console, or from the same cookie
#      jar as above; save the "token" field, shown exactly once.
#    → or mint one from the SHELL, which needs no session at all:
#      `clippet admin tokens mint <username> [--scopes upload[,read]|inbound|autofetch] [--sxcu]` prints the raw
#      token (or the whole ShareX file) on stdout, once (DEPLOY.md §9, §20 erratum 308).

# 2. every upload after that needs no cookie at all:
curl -s -X POST "$BASE/api/upload" \
  -H "Authorization: Bearer ck_YOUR_TOKEN_HERE" \
  -F 'title=Captured clip' \
  -F 'file=@capture.mp4;type=video/mp4'
# → {"id":"...","url":"https://clips.example.com/...","state":"ready",
#    "card_ready":true,"tiers":[...], ...}
#   The reply is HELD until this clip's CARD works — up to the server's
#   CAPTURE_READY_WAIT (30s by default, 60s ceiling) — so the link on your clipboard
#   already unfurls as a player, and `card_ready` says whether it does. There is no
#   `wait` field to ask for less: sending one is 400 invalid_request.
```

Or skip step 1's manual curl entirely: the `sxcu` object `POST /api/auth/tokens` returns
is a complete, importable ShareX custom uploader — `RequestURL`, the `Authorization`
header and the field name are all filled in already. `clippet admin tokens mint <user>
--sxcu` writes that same document to stdout: one generator, so the file is the same
whichever door minted it.
