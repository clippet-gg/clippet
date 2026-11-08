# DEPLOY.md — running Clippet

Operator guide: bring the stack up, put it behind a public **https** origin, and prove that a
share link unfurls as a playable card. Every command here is copy-paste correct for a **Linux or
macOS shell**; on **Windows** run them in **WSL2** (Ubuntu), where they are the same commands. Git
Bash on Windows works too, with one quirk, and the two places where Windows differs are called out.

> **Git Bash quirk, once:** MSYS rewrites arguments that look like Unix paths, so any command that
> passes an *in-container* absolute path (`docker compose exec app ls /data`, `docker run … -v …:/src`)
> must be prefixed with `MSYS_NO_PATHCONV=1`. Commands whose arguments contain no leading-slash path
> (everything in the Quick start below) need nothing. Conversely, a **host** path handed to a native
> Windows tool (`curl -o`, `ffmpeg -i`) must be converted with `cygpath -w`. Never use
> `Invoke-WebRequest` against this stack — it hangs on localhost; use `curl`. None of this applies in WSL2.

## Contents

- [1. Prerequisites](#1-prerequisites)
- [2. Quick start (local, http)](#2-quick-start-local-http)
- [3. Going public: Cloudflare quick tunnel](#3-going-public-cloudflare-quick-tunnel)
- [4. `TRUSTED_PROXIES` behind the tunnel](#4-trusted_proxies-behind-the-tunnel)
- [5. https is not optional (for X)](#5-https-is-not-optional-for-x)
- [6. Verify the public origin with the E2E](#6-verify-the-public-origin-with-the-e2e)
- [7. Configuration](#7-configuration)
- [8. Day-to-day operation](#8-day-to-day-operation)
- [9. Capture tools (ShareX, RePlays, OBS, curl)](#9-capture-tools-sharex-replays-obs-curl)
- [10. Clip lifetime: TTL, keep-alive, keep forever](#10-clip-lifetime-ttl-keep-alive-keep-forever)
- [11. Discord auto-post](#11-discord-auto-post)
- [12. Trim](#12-trim)
- [13. GIF previews](#13-gif-previews)
- [14. Tags and collections](#14-tags-and-collections)
- [15. Backup and restore](#15-backup-and-restore)
- [16. Upgrading](#16-upgrading)
- [17. Reproducibility and CI](#17-reproducibility-and-ci)
- [19. Accounts, sessions and tokens](#19-accounts-sessions-and-tokens)
- [20. The library](#20-the-library)
- [21. The social rendition and bitrate](#21-the-social-rendition-and-bitrate)
- [22. The watch page](#22-the-watch-page)
- [23. Uploading](#23-uploading)
- [24. Operator toolbox](#24-operator-toolbox)
- [25. Collections, oEmbed, defaults and routing](#25-collections-oembed-defaults-and-routing)
- [26. Behind your own domain: Caddy and nginx](#26-behind-your-own-domain-caddy-and-nginx)
- [27. Quality tiers: Original, Full, Light](#27-quality-tiers-original-full-light)
- [28. The look: light and dark, the fonts, the visual pass](#28-the-look-light-and-dark-the-fonts-the-visual-pass)
- [29. Links: what a share URL looks like, and the two id grammars](#29-links-what-a-share-url-looks-like-and-the-two-id-grammars)
- [30. The Discord bot: `/upload`, `/clip` and Enrich in your own server](#30-the-discord-bot-upload-clip-and-enrich-in-your-own-server)
- [31. What this instance costs](#31-what-this-instance-costs)
- [32. A directory instead of a bucket](#32-a-directory-instead-of-a-bucket)
- [33. Serving media by redirect](#33-serving-media-by-redirect)
- [34. The drop folder: put a video in it and it becomes a clip](#34-the-drop-folder-put-a-video-in-it-and-it-becomes-a-clip)
- [35. Where clips live: the setup wizard](#35-where-clips-live-the-setup-wizard)
- [36. Clippet Pass: a name on clippet.gg, the official bot, the portal](#36-clippet-pass-a-name-on-clippetgg-the-official-bot-the-portal)
- [37. The homepage on the apex](#37-the-homepage-on-the-apex)
- [38. What this instance records about people](#38-what-this-instance-records-about-people)
- [18. Not covered here](#18-not-covered-here)

One entry per `## N.` heading, in **file order** — not numeric order. Section numbers are cross-
referenced from `README.md`, `DESIGN.md` and `API.md` in a few dozen places, so they are stable ids,
not a table of contents' worth of ordering: §18 "Not covered here" is deliberately last in the file
(and last above) even though its number is not, and §18 is **never** renumbered to fill the gap.

---

## 1. Prerequisites

- **Docker Engine 25+ with Compose v2** (Docker Desktop on Windows/macOS, `docker` + `docker-compose-plugin` on Linux).
  `docker compose version` must print `v2.x`.
- **Nothing else to install.** Go, Node and ffmpeg live inside the images — the host needs no
  language or media toolchain. `scripts/check.sh` builds and tests in containers; to drive them it
  wants a POSIX shell with `bash` and GNU `grep` (plus `mktemp`), which any mainstream Linux, macOS
  or Git-Bash host already has (§17.2).
- Optional, for `scripts/e2e.sh` **only**: host `ffmpeg`/`ffprobe` 7.x, `python` ≥ 3.8, `curl`, `bash`.
  The E2E generates and probes test clips on the host; the product itself never uses them.
- Disk: the SQLite DB and the transcode scratch disk live in named volumes. Scratch needs, per clip
  being worked on, **one copy of the largest upload + `SCRATCH_MIN_FREE_BYTES` (512 MiB by default) +
  the rendition that clip encodes to** — plus whatever the media store holds (the media directory, or
  your bucket, which is elsewhere). Ingest and
  the worker use the same arithmetic on purpose (DESIGN.md §20 erratum 90): the upload API admits a body
  only when `free ≥ 1.5 × its size + SCRATCH_MIN_FREE_BYTES`, and once those bytes are on the disk the
  worker asks only for the floor plus room for the rendition it is about to write — so an upload the API
  accepts is never one the worker then refuses. Multiply the per-clip figure by `TRANSCODE_CONCURRENCY`
  if you raise it. **Since the quality tiers (§27) that figure covers the DERIVED jobs too**: on a clip
  whose upload is served as it stands there is no ≤720p rendition to read, so a GIF, a chosen card frame
  or a card copy downloads the upload itself and asks for the same room a transcode would. Scratch has to
  hold the largest upload you accept plus `SCRATCH_MIN_FREE_BYTES` for those jobs as well, not only at
  upload time — if it cannot, they requeue for ever without ever spending an attempt (§13, §21, §27). **"Named volumes" is not isolation**: on a default Docker install `app_data` and
  `app_scratch` live under one directory on the daemon's filesystem, which is also where the container
  logs go (§8) — and the default media directory (`~/Videos/clippet`, §32.1) is very often on that same
  disk. One free-space pool, shared by SQLite's WAL, the encoder's scratch copies and every stored clip,
  and the only thing standing between a 4 GiB upload and the database's next write is
  `SCRATCH_MIN_FREE_BYTES`, which nothing but the upload precheck respects. So budget the scratch figure
  *plus* the media *plus* headroom; raise
  `SCRATCH_MIN_FREE_BYTES` well above its 512 MiB default if the database matters more to you than
  accepting one more upload; and point the volumes at separate devices (`driver_opts`, or a bind mount)
  if you want real isolation.
- **Where the clips will live is a decision, and the instance will walk you through it.** The shipped
  default is a directory on this machine — your own `~/Videos/clippet` unless you name another (§32):
  one container, no keys. The other answers are a bucket of your own — Backblaze B2, Cloudflare R2,
  Amazon S3 or any S3-compatible server — which is what redirect delivery (§33) and the database
  snapshot in the bucket (§15) need. (Until DESIGN.md §20 erratum 345 the default was a bundled bucket
  server; §32.6 is the upgrade path from it.) Bring the stack up as below and open
  **`/setup`** — it prints the `.env` block for whichever you pick and proves the store answers. §35
  is its other half (the console links, the IAM policy, the region codes and the free-tier table).
  **One rule decides it for you if you are going out through a Cloudflare tunnel: the media has to be
  in a bucket** (§35.5).
- Ports: the app publishes **8481 on loopback only** — `ports: ["${CLIPPET_PUBLISH:-127.0.0.1:8481}:8080"]`
  in `compose.yaml`. `curl http://localhost:8481/…`, `scripts/e2e.sh` and a cloudflared reaching
  `host.docker.internal` all work as before; nothing *off* this machine reaches the app except through
  whatever proxy you put in front of it, which is what makes `TRUSTED_PROXIES` safe (§4). Widening it is
  one `.env` line — `CLIPPET_PUBLISH=0.0.0.0:8481` — and has a consequence spelled out in §4. Nothing
  else is published: the default stack is the app alone.

## 2. Quick start (local, http)

```sh
cp .env.example .env          # then edit; every value has a dev default in compose.yaml
docker compose up -d          # pulls the published image, starts the one service: app
docker compose ps             # app (healthy)
curl -sS http://localhost:8481/readyz     # {"ok":true} once the DB and a live store ping are good
```

**One service, and a folder.** Since DESIGN.md §20 erratum 345 the stack is the app alone, keeping the
media in a directory on the host (`STORAGE_BACKEND=fs`, §32): your own `~/Videos/clippet` unless
`STORAGE_DIR_HOST` in `.env` names another. Compose creates the directory on the first `up`. **On
Linux** Docker creates it owned by root, and the app runs as uid 10001, so the first ping logs
`storage unreachable; serving degraded` until you give it to that uid:

```sh
sudo chown -R 10001:10001 ~/Videos/clippet     # or the STORAGE_DIR_HOST you set
docker compose restart app
docker compose exec -T app clippet admin doctor   # the `store` row: writable, and which host directory
```

Docker Desktop (macOS, Windows) needs none of that. A bucket of your own is the opt-in (§7 *BYO
bucket*, §35); §32.6 is the upgrade path for a stack that ran the bundled bucket server.

`/readyz` is the gate that matters: it does a **live** storage ping. `/healthz` is liveness only
(DB + a cached storage result) and answers before storage is proven, so never wait on it.

Behind that cached result is one **boot** ping loop: the app opens its listener first, then asks the
store whether it is there — ten times, three seconds apart, each attempt capped at three seconds
(`STORAGE_PING_ATTEMPTS` / `STORAGE_PING_INTERVAL`, §4). The first success logs `storage reachable`
and the loop ends; ten failures log `storage unreachable; serving degraded` and the process goes on
serving — pages, logins and the library all work without the store, and only media does not. It never
crash-loops on a store that has not come up yet, which is why the defaults wait a whole minute; the
worker and the sweeper start once the loop is done either way.

Open <http://localhost:8481/>, register the first account, upload a clip. Configuration is read
**once at boot**: after any `.env` edit run `docker compose up -d` to recreate the app container.

**Then decide where the clips live** — <http://localhost:8481/setup> is the wizard (§35). It prints
the `.env` block for a directory (what you are running now), B2, R2, S3 or any other S3-compatible
bucket, and its `Check now` runs the same store probe `admin doctor` does. It is reachable
anonymously until the first account exists, and from Settings → Storage afterwards — by that first
account alone, the instance's **owner** (§35.1).

## 3. Going public: Cloudflare quick tunnel

A quick tunnel gives you a throwaway `https://<random-words>.trycloudflare.com` origin with no
Cloudflare account, no port-forwarding and no certificate work. It is how this repo's Phase 3 gate
was proven. For anything permanent use a **named** tunnel and your own domain:
`compose.tunnel.yaml` ships the cloudflared service for the dashboard-token form, selected with
`COMPOSE_FILE=compose.yaml:compose.tunnel.yaml` (append `:compose.gpu.yaml` for the GPU flavour — order does not
matter, the files touch different services) plus `CLOUDFLARE_TUNNEL_TOKEN` and the real `PUBLIC_BASE_URL` in `.env`.
It reaches the app as `app:8080` over the compose network, so it needs no published port at all. **Add the zone's
cache-bypass rule the same day** (a Cloudflare dashboard step): a fresh Cloudflare zone
caches `.mp4`/`.jpg`/`.gif` at its edge by extension and overrides the app's `no-store` with a four-hour browser TTL, which
breaks TTL, delete, unlisted and Range semantics for every media route until the rule exists.

> **A tunnel in front of a disk works, and Cloudflare's terms reserve video for a bucket.** Their
> Service-Specific Terms restrict serving **video** through a proxied hostname from an origin that is
> not Cloudflare's on the Free, Pro and Business plans, and since 25 Aug 2026 their video-policy page
> says in writing that a public hostname through a Cloudflare Tunnel is covered by it. A stack that
> keeps its media on a disk at home and streams it out through a tunnel is exactly that shape — and at
> a personal instance's size the worst on record is an email (the owner's verdict of 2026-10-14). The risk is yours to take or to remove: put the media in a bucket and
> run `MEDIA_DELIVERY=redirect` (§33 — the tunnel then carries HTML and a 300-byte redirect), or reach
> the instance some other way (§35.5 has the three: a port forward with your own domain and TLS, a
> relay VPS, or Tailscale for a private tailnet). The `/setup` wizard walks the folder behind a tunnel
> like any other pick and names the risk on that walk (DESIGN.md §20 erratum 348); it never refuses.

**The locally managed form** (`compose.tunnel-local.yaml`): the same cloudflared service, but the tunnel's credentials file
and its ingress live in a Docker volume you prepare once with `cloudflared tunnel login` / `create` / `route dns` (the file's
header has the exact sequence) instead of a dashboard token in `.env`. Select it INSTEAD of `compose.tunnel.yaml`; name the
volume in `TUNNEL_VOLUME`. A second instance beside the first (a dev box on `dev.example.com`) wants its OWN tunnel either way:
a second connector on the production tunnel's token would serve the production hostname from the other box. Add the new hostname
to the cache-bypass rule too.

```sh
# 1. start the tunnel next to the stack (cloudflared runs in Docker; nothing is installed on the host)
docker run -d --name clippet-tunnel \
  --add-host=host.docker.internal:host-gateway \
  cloudflare/cloudflared:2026.8.3 tunnel --url http://host.docker.internal:8481

# 2. read the issued URL out of its logs (it appears within ~10s)
docker logs clippet-tunnel 2>&1 | grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' | head -1
```

`--add-host=host.docker.internal:host-gateway` is what lets the container reach the app on the host's
published port. Docker Desktop resolves `host.docker.internal` on its own, so on Windows/macOS the
flag is merely harmless.

**On Linux the recipe above does not work as written, and the flag is not what fixes it.** §1's default
publish is `127.0.0.1:8481`, and Docker's userland port proxy answers only for that destination address:
from the bridge gateway — which is what `host.docker.internal:host-gateway` resolves to — the port is
simply refused. (Docker Desktop gets away with it because the "host" there is a VM whose networking is
not the same thing.) Do **not** fix it by widening the publish to `0.0.0.0:8481`: that puts the app
directly on your LAN, and combined with a non-empty `TRUSTED_PROXIES` it lets any caller spoof
`X-Real-IP` past every per-IP limit (§1, §4). Run cloudflared as a **compose service on the same
network instead**, pointed at the app by service name, so no port has to be reachable from anywhere:

```yaml
# compose.override.yaml — Linux quick tunnel. `docker compose up -d` picks it up automatically.
services:
  tunnel:
    image: cloudflare/cloudflared:2026.8.3
    command: tunnel --no-autoupdate --url http://app:8080
    depends_on: [app]
    restart: "no"     # a restart issues a NEW hostname; see the sharp edge below
```

`docker compose logs tunnel` then carries the issued URL, and steps 3 and 4 are unchanged. The same
service shape — with `--token` instead of `--url` — is how a **named** tunnel is run, which is what you
want for anything permanent. Note that `http://app:8080` is the container's
own listener, not the published port, so this path works identically on Windows and macOS; it is only
*required* on Linux.

```sh
# 3. point the app at that origin and recreate it (PUBLIC_BASE_URL is read at boot)
#    Edit the PUBLIC_BASE_URL line in .env (gitignored). Appending a second line also works —
#    both compose and scripts/e2e.sh take the LAST definition — but editing keeps the file honest.
docker compose up -d

# 4. prove the origin end to end
curl -sS -I https://<random-words>.trycloudflare.com/readyz   # HTTP/1.1 200, Server: cloudflare
```

**Before you hand the URL to anyone**, know where registration stands. The shipped default is
`REGISTRATION=first`: the origin accepts exactly one account, and the first person to reach
`/register` gets it. **So make yours now**, before the URL goes anywhere — open
`https://<your-hostname>/register` and register, and the door shuts behind you. To let friends sign
up, set `REGISTRATION=open` in `.env`, `docker compose up -d`, and put it back afterwards; or add
them one at a time with `clippet admin users create` and never open it at all. §19 is the whole
story, including what a public origin with `open` actually exposes (a 4 GiB upload cap and every
listed clip's metadata). (DESIGN.md §4 assigns this checklist to this
document.)

**From here on, drive the app through the public origin only.** `http://localhost:8481` still answers
`/readyz`, `/healthz` and serves pages, and the admin CLI (`docker compose exec`) is unaffected — but
it is no longer usable as a UI, for two reasons that stack. The session cookie is now issued with
`Secure` (it follows `PUBLIC_BASE_URL`, DESIGN.md §14), so a browser will not store or send it on an http
origin: you can log in at localhost and stay logged out. And any cookie-authenticated mutation that
did get through (upload, PATCH, DELETE, retry, logout) is rejected `403 csrf`, because the check
compares the browser's `Origin` against `PUBLIC_BASE_URL` and `http://localhost:8481` no longer
matches (DESIGN.md §14; register and login themselves are exempt). Both are the design working — use the
tunnel URL in the browser.

### Why `PUBLIC_BASE_URL` must equal the public origin

`PUBLIC_BASE_URL` is not cosmetic. It drives:

- **every absolute URL in a share card** — `og:url`, `og:image` (poster.jpg), `og:video` and
  `twitter:player:stream` (social.mp4), `twitter:player` (the embed page). Discord and X fetch those
  URLs *from the internet*; if they carry `http://localhost:8481` the crawler gets nothing and you
  get a bare link with no preview.
- **`og:video:secure_url`**, which is emitted **only** when the origin is https (DESIGN.md §9.1). Without it X
  will not treat the clip as a player.
- **the session cookie's `Secure` flag** (DESIGN.md §14): set iff the origin is https.
- the `url`/`embed_url` fields in API responses, i.e. what a capture tool pastes.

So the rule is: **`PUBLIC_BASE_URL` is whatever the browser types**, not what the container listens on.
A mismatch is silent — the app boots fine and only the cards are wrong.

### The quick tunnel's one sharp edge

**The URL changes every time the tunnel restarts.** A container restart, a `docker restart
clippet-tunnel`, a Docker Desktop update — any of them issues a brand-new hostname. Clips shared
under the old hostname go dead: the link 404s at Cloudflare, and Discord's cached embed for that URL
stays broken. Do not give a quick-tunnel link to anyone you would have to apologise to. Two
consequences worth internalising:

- Deliberately **do not** give the tunnel container a `--restart` policy. A silent restart that
  changes the origin behind a still-running app is worse than a tunnel that is visibly down.
- After any restart: read the new URL, update `.env`, `docker compose up -d`. Old links stay dead.

The fix is a **named tunnel on a domain you own** — a stable hostname, and `PUBLIC_BASE_URL` you set
once. That needs a Cloudflare account; the compose snippet is §3's (`compose.tunnel.yaml`).

## 4. `TRUSTED_PROXIES` behind the tunnel

Every request arriving through cloudflared reaches the app from the **Docker bridge gateway**, not
from the viewer. On this machine that is `172.19.0.1` (find yours with
`docker network inspect clippet_default --format '{{range .IPAM.Config}}{{.Subnet}} gw={{.Gateway}}{{end}}'`).
Left unset, the app therefore treats *the whole internet as one client*:

- the auth limiter (`RATE_AUTH_PER_MIN`, default 10/min, keyed by client IP) is shared by every
  visitor — one person fumbling their password can lock out the rest;
- the view limiter (60/min per IP) likewise;
- **view de-duplication collapses**: `POST /api/clips/{id}/view` is deduped per
  (clip, client IP, User-Agent) for `VIEW_DEDUPE_TTL`, so two different viewers on the same browser
  version count as one.

Fix it by trusting the proxy network:

```sh
# .env — the CIDR that contains the Docker bridge gateway
TRUSTED_PROXIES=172.16.0.0/12          # or the exact subnet, e.g. 172.19.0.0/16
```

then `docker compose up -d`. Only when the TCP peer is inside one of these prefixes does the app
honour `X-Real-IP`, and failing that walk `X-Forwarded-For` from the right, skipping hops that are
themselves inside a trusted prefix (DESIGN.md §13). Cloudflare's edge puts the visitor's address in
`X-Forwarded-For`, so that is the one that ends up being used. (`Cf-Connecting-Ip` is sent too but the
app does not read it — it needs no Cloudflare-specific knowledge.)

**Safe because the port is bound to loopback.** `TRUSTED_PROXIES` says "anything from this network
may tell me who the client is", and the shipped `compose.yaml` earns that by publishing the app on
`127.0.0.1` only (§1). The sole peers inside `172.16.0.0/12` are then the proxy containers, so the
recipe above is safe exactly as written, and nothing is lost: `curl http://localhost:8481/readyz`,
the E2E and cloudflared's `host.docker.internal` route all still work.

**Widening the bind is the opt-in, and it costs you every per-IP limit.** Set
`CLIPPET_PUBLISH=0.0.0.0:8481` in `.env` (then `docker compose up -d`) only when something off this
machine must reach the app *directly* — a LAN box with no proxy in front of it, or a cloudflared
running on another host. Once the port is reachable, a caller whose packets arrive from inside a
trusted CIDR can send its own `X-Real-IP` and the app will believe it: the auth limiter's 10/min
bucket resets on every request, and every fabricated value is a fresh view-dedupe key. So **do not
combine a widened bind with a non-empty `TRUSTED_PROXIES`** — pick one. If you want both a public
port and a proxy, terminate the proxy on this machine and leave the bind on loopback, or run
cloudflared as a compose service pointed at `http://app:8080` and drop the `ports:` line entirely
(the `compose.tunnel.yaml` shape of §3).

The app cannot see its own publish — a container has no view of the host's port map — so it does the
one thing it can: `serve` logs a single `WARN` at boot whenever `TRUSTED_PROXIES` is non-empty,
naming this section. It is a reminder, not a detection; a stack with the default bind logs it too.

Narrowing `TRUSTED_PROXIES` to the gateway address (`172.19.0.1/32`) is **not** a substitute on
Docker Desktop (Windows/macOS): published-port traffic is proxied by the Docker VM, so a LAN client
hitting `host:8481` arrives with the *same* `172.19.0.1` source address as cloudflared and is trusted
either way. On Linux the published port is DNAT'd with the real source address preserved, so there
the `/32` does help — but the loopback bind is the portable answer.

**What it does not change:** the `remote=` field on every access-log line is always the raw TCP peer,
so behind a tunnel it stays `172.19.0.1:xxxxx` no matter what you trust. Correlate requests by
`request_id=` instead; the resolved client address is used for limits and view dedupe, not for logs.

## 5. https is not optional (for X)

- **X / Twitter** only renders a `twitter:player` card whose player and stream URLs are **https**.
  An http origin degrades to a plain link, and the `og:video:secure_url` tag the card needs is not
  emitted at all on http (DESIGN.md §9.1). No amount of tag tweaking substitutes for TLS.
- **Discord** will embed an http *origin* — the scheme is not the constraint there — but Discordbot
  fetches the tags and the media from **Discord's own** servers, so the origin still has to be
  reachable from the internet. A LAN-only or `localhost` deployment therefore gets a bare link in
  Discord too (§3), not a card. http buys you "no certificate", not "no public origin".
- The product promise is *both*, so the supported deployment is https: quick tunnel for a demo,
  named tunnel / your own reverse proxy for real use.

TLS is terminated by Cloudflare (or your proxy); the app itself speaks plain http on `:8080` inside
the container and never sees a certificate. That is why `PUBLIC_BASE_URL`, not the listener, decides
whether cookies get `Secure`.

### HSTS: set it at your terminator too

When `PUBLIC_BASE_URL` is `https://…` the app sends
`Strict-Transport-Security: max-age=31536000; includeSubDomains` on every page and API response, so a
browser that has seen the site once upgrades the bare hostname by itself instead of making a plaintext
request first — the hop that carries the login page and the password typed into it. An `http://` base
sends nothing, so a local dev stack is unaffected.

**If you terminate TLS yourself** (your own nginx / Caddy / Traefik rather than a Cloudflare tunnel),
add the same header there as well. The app's copy only rides on responses that already reached it over
your proxy; a request your proxy answers itself — a redirect from `:80`, an error page, a maintenance
response — carries whatever the proxy sets, which is nothing by default. Cloudflare has its own HSTS
switch (SSL/TLS → Edge Certificates → HTTP Strict Transport Security); turning it on is the same idea.

Clippet deliberately does **not** ask for `preload`. Preloading is a hostname-wide, hard-to-undo
commitment baked into browsers, and it belongs to whoever owns the domain — not to one app deployed
under it. Add it at your terminator if that is a decision you want to make.

### `/robots.txt`, and what it does not do

The app serves a `/robots.txt` that tells general crawlers to stay out of the whole site (§29),
and an unlisted clip's share page also carries `<meta name="robots" content="noindex">`.
Discordbot, Twitterbot, facebookexternalhit and Slackbot-LinkExpanding are named individually and
allowed everything, so **cards keep working** — that is the whole point, and `noindex` never stopped
`og:` unfurling anyway.

What this buys you is that a share link which leaks into a crawler (browser toolbars, referrer chains,
paste sites) does not turn into a search result. What it does **not** buy you is secrecy: robots.txt is
a request, not an access control, and a crawler that ignores it is not doing anything the URL itself
prevents. The security property is unchanged: a random id (§10, §29) and a TTL. Treat a clip
link as the capability it is.

## 6. Verify the public origin with the E2E

```sh
BASE=https://<random-words>.trycloudflare.com bash scripts/e2e.sh
```

- `BASE` must **equal** `PUBLIC_BASE_URL`. The card assertions compare every emitted tag against
  `BASE`; pointing the script at a different hostname than the app was booted with fails at step 10
  (the script prints a warning first). With no `BASE`, the script reads `PUBLIC_BASE_URL` from the
  environment, then from `.env`, then falls back to `http://localhost:8481`.
- Over an `https://` base the run additionally asserts (DESIGN.md §16.3 and §20 erratum 56): register **and login**
  set a `Secure` cookie; `og:video:secure_url` is present; every URL-valued card tag is an https URL
  on the public origin; and — the real gate — **every one of those URLs is fetched back from the
  public origin with a Discordbot and a Twitterbot User-Agent and answers 200**, which is exactly
  what a crawler does. Over an `http://` base those assertions are skipped and nothing else changes.
- The admin-CLI steps (forced expiry) and the bucket object counts still run **locally** through
  `docker compose exec` / `docker compose run` — the E2E drives the public origin over HTTP and the
  stack from the host at the same time. Run it where the stack is.
- Expect 13–20 minutes (1163 s measured on a lane stack when the scheduler landed; 859 s over a
  Cloudflare quick tunnel at the shipped `cpus: 1.0` and 772 s at `APP_CPUS=4` on the same stack —
  the suite is mostly requests, so more cores move it less than they move an encode; longer on a slow
  encoder or a contended ffmpeg slot) and an `E2E PASS` summary table, which now ends with where the
  time went — the ten slowest steps and a line per bundle. It cleans up after itself (the clips it
  leaves with no TTL are deleted at step 17 and the session is logged out).

For the container-only CI story — `gofmt`, `go vet`, unit + ffmpeg + `-race` tests, image builds —
run `./scripts/check.sh` (a few minutes; run it alone, it is CPU-hungry).

## 7. Configuration

`.env` (gitignored, next to `compose.yaml`) is the only place to set values: `compose.yaml`
interpolates **every** variable in DESIGN.md §4 as `${VAR:-<default>}` and passes it to the app
(DESIGN.md §20 erratum 57), so anything named there can be set in `.env` and nothing else needs editing.
`docker compose config` prints the resolved environment if you want to check a value before
recreating. The one deliberate exception is `SESSION_SECRET`, which does not exist (DESIGN.md §4: nothing is
signed). `CLIPPET_LISTEN`, `DATA_DIR` and `SCRATCH_DIR` are settable but coupled to the port mapping,
the named volumes and the healthcheck — leave them alone. The operator-facing subset:

| Variable | Default | What it does |
|---|---|---|
| `PUBLIC_BASE_URL` | `http://localhost:8481` | **The one you must get right.** Absolute origin `scheme://host[:port]`, no path, no trailing slash. Drives every card URL, `og:video:secure_url`, the cookie `Secure` flag. |
| `DEFAULT_CLIP_TTL` | `never` | TTL for uploads that do not pick one: `never`, `1d`, `7d`, `30d`, `90d`. Per-clip TTL always wins. |
| `REGISTRATION` | `first` | `open` \| `first` \| `closed` \| `members` \| `role:<role id>`. **`first` accepts registration only while no account exists** — the first person to reach `/register` gets the instance and the door shuts. `open` accepts anybody, forever; `closed` nobody. A refusal is 403 and the `/register` page shows a notice. The effective mode is on the boot line. `members` and `role:<id>` keep `/register` closed and let a member of a bound server in through the bot's `/signin` (§30.12). `scripts/e2e.sh` needs `open`. Full story and the CLI that adds accounts under the other two: §19. |
| `SESSION_TTL` / `SESSION_MAX_TTL` | `720h` / `2160h` | How long a browser stays signed in. The session **slides** while it is used (`SESSION_TTL` from the last request) but never lives longer than `SESSION_MAX_TTL` from when it was created — so a stolen cookie that is kept warm still dies on a schedule (§19.3). Lower both for shared machines. |
| `LIBRARY_SCOPE` | `shared` | `own` makes each account's library and `GET /api/clips` its own clips only; `shared` (the default, and what every earlier version did) shows every account's listed clips. It narrows a LIST and nothing else — a share link still works for anyone holding it — see §20. |
| `INSTANCE_BADGE` | *(empty = production)* | **One word this instance wears so you cannot mistake it for the production one**: `dev`, `staging`, `lab-test`. The wordmark's green span goes **grey** everywhere the mark appears (the embed overlay included), the word sits beside it, every `<title>` gains it as a prefix, Discord's site line reads `clippet dev`, and an installed app is named `Clippet dev`. Lowercase letters, digits and `-`, at most 12; anything else is a boot error. Empty changes nothing at all — see §7.1. |
| `SETUP_WIZARD` | `on` | The `/setup` wizard, and its first-run door before any account exists. `off`: it answers 404 (its steps-only view stays) and Settings drops its link — the desktop app sets it, because the app does the setup. See §20 erratum 335 (d) in DESIGN.md. |
| `CARD_COMPONENTS` | `on` | A clip's page also declares its own Discord card, a component embed (the mark, the host, the title, the length, the playable video, the state's colour, an **Open** button). The ordinary card stays and is the fallback. `off` serves the ordinary card alone. See §28. |
| `TRUSTED_PROXIES` | *(empty = trust nothing)* | Comma-separated CIDRs whose `X-Real-IP` / `X-Forwarded-For` are honoured. See §4 above. |
| `MAX_UPLOAD_BYTES` | `4294967296` (4 GiB) | Hard limit on the video part → 413. There are no per-user quotas; this is your disk. |
| `UPLOAD_CHUNK_BYTES` | `52428800` (50 MiB) | The size of ONE request of the web uploader's chunked upload: a file larger than this goes up in pieces of this size (DESIGN.md §20 erratum 344; API.md "The chunked upload"). **The edge's per-request cap minus headroom** — see "Why chunks" below. `5242880` … `95000000`; anything else is a boot error. It changes the number of requests, never what is accepted: `MAX_UPLOAD_BYTES` is still the file's limit. |
| `STORAGE_BUDGET_BYTES` | `0` (off) | An advisory line, in bytes, on how much this instance stores — `200000000000` is 200 GB. **Never a cap: no upload is ever refused because of it, unless `STORAGE_BUDGET_HARD=1`** (§25.4), on either backend, and there are still no tiers and no per-user quotas either way. When the bucket passes it: one WARN in the log, a `storage_budget` object in `/healthz`, one line on the library page, and the `quota` row leading Settings' Storage panel (§25). |
| `STORAGE_BUDGET_HARD` | `0` (off) | Makes that line a **cap**: with `1`, a new clip is refused with `507 storage_quota` once the stored bytes reach `STORAGE_BUDGET_BYTES` — every upload route, the capture endpoint and the Discord bot, one sentence. A replacement or a trim of an existing clip is **not** refused. `1` without a budget is a boot error (§25). |
| `STORAGE_PRICE_PER_GB_MONTH` | `` (unset) | What your provider charges to **keep** one GB for a month, in your own currency: `0.015`. A plain decimal, at most six digits after the point; a sign, an exponent, a separator or a currency symbol is a boot error. Unset (the default) means **no cost line anywhere** — absent, never a `0.00`. Nothing is ever asked of a provider; see §31. |
| `EGRESS_PRICE_PER_GB` | `` (unset) | The same, for one GB **sent** to a viewer, against the per-day served-bytes counter §31 describes. Either price may be set without the other — on R2, storage is priced and egress is not. |
| `STORAGE_PRICING` | `` (unset) | A dated **preset** of one provider's list prices instead of typing them: `local`, `r2`, `b2` or `s3`. A table inside the image, read off the provider's own pricing page on the date it carries and **never fetched**; it also models that provider's free-egress rule. Either price above overrides its half of the preset (the allowance stays). Anything else is a boot error naming the four (§31.5). |
| `CAPTURE_READY_WAIT` | `30s` | How long `POST /api/upload` (the capture-tool endpoint, §9) holds its 201 waiting for the clip's **card** to work — the poster plus the file `og:video` points at — so the link ShareX pastes unfurls as a **player** card rather than a link-out one Discord then caches (§27). `0` switches it off; `60s` is the hard ceiling, and at the ceiling the link is returned anyway with `card_ready: false`. There is no per-request override: a `wait=` field is a 400. It does not apply to the browser uploader, which watches a progress bar instead. |
| `JOBS_UNSTICK_AFTER` | `TRANSCODE_HARD_TIMEOUT` + `15m` (`6h15m`) | How long a job may sit `running` with no worker before the app repairs it **itself**, without a restart and without you running `admin jobs unstick` (§24.6). `0` switches it off; the floor is 30 s. A job this process is running is never repaired whatever its age, so a value shorter than `TRANSCODE_HARD_TIMEOUT` is obeyed — with one boot WARN about the only thing it risks, a *second* process sharing the database. On a single instance, minutes is a better setting than the six-hour default. |
| `BACKUP_EVERY` | *(unset: off)* | How often the server writes a database snapshot into `BACKUP_DIR`: a Go duration such as `24h`, `10s` at least. Off unless `BACKUP_DIR` is set too; one without the other is a boot error (§15.6). |
| `BACKUP_DIR` | *(unset: off)* | The directory, inside the container, the snapshots are written to. It must already exist — the app never creates it — and may not overlap `SCRATCH_DIR` or `STORAGE_DIR` (§15.6). |
| `BACKUP_KEEP` | `30` | How many snapshots stay; older ones are removed after each run. `1` at least. Read only when timed backups are on (§15.6). |
| `DB_SNAPSHOT` | *(unset: `on`)* | `on` \| `off`. A copy of the database in the store, `{S3_KEY_PREFIX}_clippet/db.sqlite` — in the bucket, or on `STORAGE_BACKEND=fs` in the media folder — so the store alone can bring the instance back (`admin restore --from-store`, §15; DESIGN §20 erratum 348). |
| `DB_SNAPSHOT_EVERY` | `10m` | At most one snapshot upload per this interval after a clip changes, the last change of a burst always written; `5s` at least (§15). |
| `RATE_UPLOAD_PER_HOUR` | `60` | **One budget, per account, shared by every request that starts an ffmpeg job**: `POST /api/clips`, `/api/upload`, `/api/share`, and the four edit routes `…/trim`, `…/gif`, `…/poster`, `…/replace` (§12 Trim, §22 The watch page, §23 Uploading). Uploading five clips and trimming two of them in the same hour spends 7 against the same 60, not two separate counters. Over budget answers **429** `rate_limited` with `Retry-After`. |
| `RATE_UPLOAD_PER_HOUR_PER_IP` | `120` | The **second** budget an upload must satisfy, keyed on the client IP (`RATE_UPLOAD_PER_HOUR`, 60, is the per-account one, and both must admit the request). It exists because accounts are free — one address can register hundreds an hour, each with its own 60 — not to squeeze a household behind one NAT, which is why it is the looser of the pair. Raise it if several people share your address and upload hard. **It depends on `TRUSTED_PROXIES` (§4):** without it, every request behind a tunnel arrives as the same Docker gateway address, so *all* your users share this one bucket — which is the other reason the default is generous. |
| `SOCIAL_MAX_BYTES` | `26214400` (25 MiB) | The size at or above which the file a card would otherwise be handed also gets a **second, smaller mp4 for its card** — and it gets one whatever its size when it is over 1280×720 or above 30 fps; for a Full, the size is the one the plan ESTIMATES before encoding (§21, §27). It is what makes a long high-motion clip play inline in Discord instead of degrading to a link, and it costs roughly half the rendition's bytes again in your bucket for the clips it fires on. `0` turns it off entirely. |
| `STORAGE_BACKEND` | *(unset: `fs` with no `S3_*` line, `s3` with any)* | `fs` \| `s3`. **`fs`, the default since DESIGN.md §20 erratum 345 whenever no bucket is named, puts the media in a plain directory** — a disk, or a NAS share mounted on the host: one container, no bucket, no second set of credentials. On `fs` the four `S3_*` variables below are **refused at boot**, which is what makes "this instance has no outbound destination for media" checkable from `.env` alone. See §32. `s3` is a bucket of your own (*BYO bucket* below); `compose.nobucket.yaml` makes it that file's default. Unset, the app reads the four `S3_*`: none set means `fs`, any set means `s3`, so a `.env` from before erratum 345 keeps its bucket without a new line (the desktop app sets the key explicitly). |
| `STORAGE_DIR` | `/media` *(compose)* / *(empty)* | Required on `fs`: the absolute path, **inside the container**, where the media lives (`compose.yaml` binds `STORAGE_DIR_HOST` to `/media`; `compose.nobucket.yaml` falls back to empty; on Windows a UNC share, `\\server\share\…`, works too). It may not be, contain, or sit inside `DATA_DIR` or `SCRATCH_DIR` — a boot error naming both — because the janitor removes stale working directories under `SCRATCH_DIR`, and `DATA_DIR` is the small half of your backup. On `s3` it is optional and means two things: where `admin storage migrate --to fs` would copy to, and — when `DROP_FOLDER_OWNER` is set — the folder whose root is the drop folder (§34), which makes it required. |
| `STORAGE_DIR_HOST` | `~/Videos/clippet` | *(compose knob)* The **host** directory, or NAS mount point, bound to `/media`. The default is your own Videos folder, spelled from `HOME` or `USERPROFILE` (§32.1, DESIGN.md §20 erratum 295) — never a path inside the container, where the next `docker compose up --force-recreate` would take the media. A server sets it. Not used on the bucket flavour (`compose.nobucket.yaml` drops the bind). |
| `STORAGE_OP_TIMEOUT` | `30s` | How long one *control-plane* call may take — a bucket List/Stat/Remove/Ping, and the response-header wait of every S3 GET/PUT. **It applies on `fs` too** (§32.2): `Ping`, `Stat` and `List` give up on a directory that has not answered within it, which is what keeps `/readyz` and `admin stats` answering when a share hangs. |
| `STORAGE_IDLE_TIMEOUT` / `STORAGE_TRANSFER_MAX` | `2m` / `6h` | The no-progress watchdog and the absolute ceiling on one upload or download. **S3 only.** On `fs` there is no equivalent: a transfer stops when the caller's own context ends (a cancelled request, a job's own timeout) and not on a clock of its own — see §32.2. |
| `STORAGE_UPLOAD_THREADS` | `6` | How many 16 MiB parts one large upload to the bucket sends **at the same time** (an object of 16 MiB or less is one PUT whatever this says). It is what lets an upload — the app's own, or `admin storage migrate`'s (§32.3) — use more of a fast uplink than one stream does. **Memory:** about this many × 16 MiB per running upload, held only while it runs (96 MiB at the default; mind `APP_MEM_LIMIT` if you raise it on a busy box). `1` is one part at a time, the behaviour before DESIGN §20 erratum 337. `1`–`32`; anything else is a boot error naming the variable. **S3 only**; `fs` ignores it. Every part carries a Content-MD5, so the bucket refuses a part that arrived damaged. |
| `STORAGE_PING_ATTEMPTS` / `STORAGE_PING_INTERVAL` | `10` / `3s` | How hard the app tries to reach the store **at boot** before it gives up and logs `storage unreachable; serving degraded` (§2). Each attempt is capped at 3s, so the default is roughly a minute of retrying. The site and the API are already up throughout — this is not a startup gate — so raise the attempts if your bucket or NAS is slow to appear after a reboot, and lower them if it is always up and you would rather see a dead endpoint named in the log in seconds. Under `1` attempt is a boot error naming the variable, not a clamp. Neither changes `/readyz`, which pings live on every request whatever these say. Applies on `fs` as well as `s3`. **Passed through by `compose.yaml` since DESIGN §20 erratum 292** — before it these two were the only §4 names no `environment:` row carried, so a `.env` line setting either was silently inert; `scripts/check.sh` now diffs the two lists. |
| `MEDIA_DELIVERY` | `proxy` | `proxy` \| `redirect`. **`redirect` stops the app streaming media at all**: the same routes, after the same gates, answer `302` to a freshly signed link on your bucket's own origin, so no video byte crosses whatever sits in front of the app. The share link, the watch page and the Discord card are unchanged. It needs a bucket — on `STORAGE_BACKEND=fs` it is a boot error. Why you would want it, and the R2 recipe: §33. |
| `MEDIA_PRESIGN_TTL` | `1h` | How long one of those signatures lasts (`1m` … `24h`; outside that is a boot error, not a clamp). **Do not shorten it without reading §33**: a `<video>` keeps fetching the same URL while its page is open, so a signature that expires mid-watch breaks seeking for somebody who never reloaded. |
| `MEDIA_PRESIGN_ENDPOINT` | *(= `S3_ENDPOINT`)* | The origin **viewers** reach the bucket at, when that is not the one the app reaches it at — the signature covers the host name. Unset on R2/B2/S3 (one host serves both); needed for a server the app reaches by a compose service name or a LAN address. Set while `MEDIA_DELIVERY=proxy` it is ignored, with a NOTE at boot and on `admin doctor` (§33). |
| `S3_ENDPOINT` | *(empty)* | Full URL with scheme, of a bucket of your own. Required on `s3`. **Refused on `STORAGE_BACKEND=fs`**, as are the three below. (Until DESIGN.md §20 erratum 345 compose defaulted it to the bundled bucket server, §32.6.) |
| `S3_BUCKET` | *(empty)* | Required on `s3`. **Must already exist**: nothing in this stack creates a bucket. |
| `S3_REGION` | `us-east-1` | `auto` for R2; its region string for B2; most self-hosted servers ignore it. |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | *(empty)* | Credentials. Never logged. Required on `s3`: an **empty** value is a boot error naming it (compose falls back to no key since erratum 345). |
| `S3_FORCE_PATH_STYLE` | `true` *(compose)* / `false` *(binary)* | The two defaults differ: `compose.yaml` keeps `true`, the value every earlier compose stack resolved to (§32.6), so an existing `.env` behaves as it did, while the binary's own default (DESIGN.md §4) is `false`, the DNS/virtual-host style R2/B2/S3 want. Set it explicitly for your provider; the `/setup` blocks always do. |
| `S3_KEY_PREFIX` | *(empty)* | Optional prefix to share a bucket, e.g. `clippet/`. Leading slashes are stripped and a single trailing `/` is added if you leave it off, so `clippet`, `clippet/` and `/clippet//` are the same prefix. Charset `[A-Za-z0-9_./-]` (the `.` in the charset is for real filenames, not a free pass for path segments): `..` is refused, and so is a bare `.` segment on its own (`./`, `a/./b`): on `fs` a key becomes a path, where such a segment used to fail at the first call instead of at boot — §4's rule moved it earlier, so a prefix like that is a boot error now. |
| `MIGRATE_S3_ENDPOINT` | *(empty)* | *migrate only.* The destination bucket's endpoint for `admin storage migrate --to s3` on an `s3` instance — bucket to bucket (§32.3). Read by that one verb and by nothing else; `serve` never reads it. Required together with the next three. |
| `MIGRATE_S3_BUCKET` | *(empty)* | *migrate only.* The destination bucket (§32.3). |
| `MIGRATE_S3_ACCESS_KEY` | *(empty)* | *migrate only.* The destination bucket's key id (§32.3). Never printed. |
| `MIGRATE_S3_SECRET_KEY` | *(empty)* | *migrate only.* The destination bucket's secret (§32.3). Never printed. |
| `MIGRATE_S3_REGION` | `us-east-1` | *migrate only.* The destination bucket's region (§32.3). |
| `MIGRATE_S3_FORCE_PATH_STYLE` | `false` | *migrate only.* Path-style addressing for the destination bucket (§32.3). |
| `MIGRATE_S3_KEY_PREFIX` | `S3_KEY_PREFIX`'s value | *migrate only.* The destination's key prefix (§32.3). Keep it unset for a move you will switch to: the database names every object by its whole key, prefix included. |
| `TRANSCODE_THREADS` | `0` (auto) | How many threads one encode may use. Auto reads this container's own CPU quota, because `nproc` inside a `cpus: 1.0` container still reports every core the host has — measured 25 % slower unpinned (§27). The boot line prints the number it resolved. |
| `TRANSCODE_HWACCEL` | empty (CPU) | `cuda` selects the NVENC path, which needs the **`gpu` image** and is 9-11× faster at one core (§27). On the ordinary image it is a boot error: the static ffmpeg has no NVENC encoder at all. |
| `LIGHT_TAG_TEXT` | `{light} preview · {best} on Clippet` | The one small line burned into the card's **Light** copy, naming the preview quality and the real one (§27). `off` removes it. It is on nothing else — never the Original, the Full, a poster, a preview or a download. |
| `TRANSCODE_CONCURRENCY` | `1` | Simultaneous ffmpeg runs, and the pool a trim or GIF also takes a slot from. **1 … 16**; outside that is a boot error. Budget ~0.5 GiB and one core per slot — see the sizing note below. |
| `APP_MEM_LIMIT` / `APP_CPUS` | `2g` / `1.0` | *(compose knobs)* The app container's `mem_limit` and `cpus`. **`APP_CPUS` must not exceed the host's core count** (`nproc`) or the container is refused at creation — see the sizing note below. |
| `CLIPPET_VERSION` | `dev` | *(compose knob, read at BUILD time)* Baked into the binary as its version and reported by `/healthz` and `clippet version`, so an upgrade can be confirmed from the outside (§16). `git describe --always --dirty` is the obvious value. |
| `CLIPPET_PUBLISH` | `127.0.0.1:8481` | *(compose knob)* The host side of the published port — loopback-only by default. **This is the other half of the `TRUSTED_PROXIES` footgun** (§4): a non-loopback bind (`0.0.0.0:8481`, or any address a caller off this machine can reach) together with a non-empty `TRUSTED_PROXIES` lets any caller spoof `X-Real-IP`/`X-Forwarded-For` past every per-IP limit and view dedupe. `admin doctor`'s `proxies` check reads this exact pair and FAILs it. |
| `SESSION_COOKIE_NAME` | `clippet_session` | Session cookie name. A `__Host-`/`__Secure-` prefix requires an https `PUBLIC_BASE_URL` or the app refuses to boot (§19). |
| `DISCORD_WEBHOOK_URL` | *(empty = off)* | Auto-post every ready clip to one Discord channel — see §11. **Empty is the only state in which the app opens no outbound connection except to your bucket.** The value is a credential; a malformed one is a boot failure, not a warning. |
| `DISCORD_WEBHOOK_ROUTES` | *(empty = no routing)* | Per-tag channel routing — `<tag>=<url>` rules separated by `;`, first match wins, `default` reserved for `DISCORD_WEBHOOK_URL`'s own channel — see §25. Every rule is validated exactly as `DISCORD_WEBHOOK_URL` is and is equally a credential. |
| `DISCORD_WEBHOOK_UNMATCHED` | `default` | What a clip no `DISCORD_WEBHOOK_ROUTES` rule matched does: `default` falls back to `DISCORD_WEBHOOK_URL`, `skip` posts it nowhere — see §25. `skip` with no rules configured and a live `DISCORD_WEBHOOK_URL` refuses to boot: it would silently announce nothing. |
| `CLIP_WEBHOOK_URL` | *(empty = off)* | POST a signed JSON document to this URL for every clip that becomes ready — see §11.1. `http` or `https`. The value is a credential; a malformed one is a boot failure. Needs `CLIP_WEBHOOK_SECRET`. |
| `CLIP_WEBHOOK_SECRET` | *(empty)* | The key that signs each webhook body (HMAC-SHA256). At least 32 characters, no whitespace. Needs `CLIP_WEBHOOK_URL`: one without the other is a boot failure. |
| `DISCORD_APP_ID` / `DISCORD_PUBLIC_KEY` / `DISCORD_BOT_TOKEN` | *(empty = off)* | The Discord **bot**: `/upload`, `/clip` and Enrich in a server you connect to an account — see §30. A different feature from the Discord webhook above, and equally optional. **All three or none**: one of them alone refuses to boot, naming all three. Only the token is a credential; it is used for one call (publishing the command) and never logged. |
| `DISCORD_COMMAND_NAME` | `clip` | What the bot's LINK command (`/clip link:`) is **called**, without the slash — see §30.2. 1–32 characters of lowercase letters, digits, `-` and `_` (Discord's rule). Give a second instance a second name if it shares a server with the first (`clip-dev` beside `clip`). Setting it with the bot off refuses to boot, and so does any name but `clip` on the official bot (§30.8). |
| `DISCORD_UPLOAD_COMMAND_NAME` | `upload` | The same for the UPLOAD command (`/upload file:`), under the same rules — and it must differ from `DISCORD_COMMAND_NAME` (§30.2). |
| `DISCORD_ENRICH_COMMAND_NAME` | `Enrich` | The name of the MESSAGE command a person picks from a message's **Apps** menu (§30.4). 1–32 characters; uppercase and spaces are allowed (`Enrich (dev)`). Same orphan and official-bot rules. |
| `DISCORD_SETTINGS_COMMAND_NAME` | `settings` | The name of `/settings`, the link-defaults panel (§30.9), under the slash-command rules — and it must differ from `DISCORD_COMMAND_NAME` and `DISCORD_UPLOAD_COMMAND_NAME` (§30.2). Same orphan and official-bot rules. |
| `DISCORD_UPLOAD_FORM` | `on` | `/upload` opens a form — Title, Delete after, Tags — while the upload runs (§30.5). `off`: no form, and every upload gets the account's defaults. |
| `DISCORD_ENRICH_UPLOAD` | `off` | `on`: Enrich on a message with a video and no link to a clip here uploads that video, for whoever posted it or a member with Manage Server (§30.4). `off`: Enrich answers links only. |
| `DISCORD_AUTO_MODE` | `off` | Clippet Auto on this box (§30.10): `prompt` puts one `Upload to Clippet` button under each video posted in a bound server; `auto` turns every such video into a link with no press; `off` opens no connection. `prompt`/`auto` need the bot above configured. |
| `DISCORD_AUTO_TOKEN` | — | The bot token of the application Clippet Auto runs as (a SECRET; never `managed`). Required with `prompt`/`auto`; with the mode unset it is a boot error, with `off` it is kept and unread. |
| `DISCORD_AUTO_SECONDS` | `10` | How long a `prompt` reply waits for a press before it removes itself, 5–60. |
| `DISCORD_AUTO_NOTICE` | `on` | The line under each link, `Videos posted here become Clippet links.`; `off` removes it. |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `text` | `debug\|info\|warn\|error`; `text\|json`. |

The rest of DESIGN.md §4 — sweeper cadence, transcode preset/CRF/fps cap and attempt budgets, the optional
bitrate ceiling `TRANSCODE_MAX_BITRATE` (§21), storage and SSR timeouts, session TTL, view dedupe, rate
limits — has sane defaults, is equally settable in `.env`, and rarely needs touching.

### BYO bucket: Cloudflare R2 / Backblaze B2

**Optional.** The default stack keeps the media in a directory (§32) and is complete without a bucket.
A bucket of your own unlocks exactly three things a directory cannot do: **`MEDIA_DELIVERY=redirect`**
(§33), the **database snapshot in the bucket**, and **`admin restore --from-store`** from it (§15).
Select the bucket flavour with the same two lines every `/setup` bucket block starts with (§35.2):

```sh
# .env — the bucket flavour: the app alone, no folder bind, STORAGE_BACKEND=s3 by default
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml
STORAGE_BACKEND=s3
```

The app proxies every byte of media itself, so the bucket stays **private** and needs **no CORS
configuration** and no public access policy. Create the bucket first — the app never creates one.
All three stay true under `MEDIA_DELIVERY=redirect` (§33): the bucket is still private, the access is
the signature in the URL rather than a policy, and a plain `<video src>` — which is what the watch
page and Discord's proxy use — needs no CORS headers from the bucket either.

```sh
# .env — Cloudflare R2
S3_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
S3_REGION=auto
S3_BUCKET=<your-bucket>
S3_ACCESS_KEY=<R2 access key id>
S3_SECRET_KEY=<R2 secret access key>
S3_FORCE_PATH_STYLE=false
```

```sh
# .env — Backblaze B2 (S3-compatible endpoint)
S3_ENDPOINT=https://s3.us-west-004.backblazeb2.com
S3_REGION=us-west-004
S3_BUCKET=<your-bucket>
S3_ACCESS_KEY=<keyID>
S3_SECRET_KEY=<applicationKey>
S3_FORCE_PATH_STYLE=false
```

**Set a lifecycle rule that aborts incomplete multipart uploads after 1 day.** (R2: the bucket's
*Settings → Object lifecycle rules → Abort incomplete multipart uploads*; B2: the equivalent lifecycle
setting on the bucket.) Uploads over about 16 MiB go up in parts, and a run that dies mid-transfer — a
killed container, a lost tunnel, a 507 — leaves those parts in the bucket. The app aborts them on the
paths it controls, but a process that was killed cannot, and what is left is invisible to every tool
you have here: `admin stats` and `admin orphans` both build their view from a `List` of *completed*
objects, so a stranded 4 GiB upload shows as absent while your provider bills for it. The rule is one
click and it is the only thing that reclaims them. (A self-hosted S3 server usually has an equivalent
setting of its own; look for "stale" or "incomplete" uploads in its documentation.)

**An empty credential is a boot error.** On `STORAGE_BACKEND=s3` a line left blank — `S3_ACCESS_KEY=`
with nothing after the `=` — refuses to boot, naming the variable. (Until DESIGN.md §20 erratum 345
compose handed the app the bundled bucket server's published pair instead; that fallback went with
the server, §32.6.) `docker compose config | grep S3_`
shows exactly what the container will get.

After switching buckets: `docker compose up -d`, then `curl -sS http://localhost:8481/readyz`. The E2E
follows the switch: since DESIGN.md §20 erratum 281 every object assertion asks the STACK — `clippet
admin objects` for what is in the store, and since erratum 346 its `put` and `rm` forms for the handful
of places that PLANT or REMOVE one, all inside the app container with this instance's own S3_* — so the
whole suite runs against the real bucket, which is what the reference stack does.

### The credentials that ship with this repository

`clippet` / `clippet-dev-secret` are in this file and in the test harness, whose own bucket server
runs on them (run 22). Since DESIGN.md §20 erratum 345 nothing hands them to the app unless they are
typed into `.env`, which is fine for a throwaway stack on loopback and not fine for anything that
people reach. So the app logs **one WARN at boot** when it is using them *and* `PUBLIC_BASE_URL` is not
a loopback host:

```
level=WARN msg="the bucket credentials are the ones published in this repository, and PUBLIC_BASE_URL is
   not loopback - set S3_ACCESS_KEY and S3_SECRET_KEY in .env and recreate (DEPLOY.md §7)"
```

It warns on the COMBINATION rather than on a detection, because a container cannot see its own port
publish — the same shape as the `TRUSTED_PROXIES` warning in §4 (DESIGN.md §20 errata 96, 104). The
values themselves are never logged. To silence it honestly, set real `S3_ACCESS_KEY` /
`S3_SECRET_KEY`. `admin doctor`'s `creds` row says the same, and FAILs outright when the published pair
points at a public endpoint (§24.1). It is a warning and not a boot failure on purpose: the exposure
depends on whether the bucket server is reachable at all, which the app cannot see.

### Sizing: memory, CPU and `TRANSCODE_CONCURRENCY`

One process runs the HTTP server, the sweeper **and** every ffmpeg, so an encode that overshoots takes
the whole instance rather than one job. `compose.yaml` therefore sets `mem_limit: ${APP_MEM_LIMIT:-2g}`
and `cpus: ${APP_CPUS:-1.0}` on the app service. (Those are compose's v2 single-host keys. A
`deploy.resources.limits` block is Swarm-only and `docker compose up` ignores it **silently**, which is
worse than no limit at all.) With a limit, an over-large encode gets the container killed,
`restart: unless-stopped` brings it back, and the interrupted transcode is released to the queue without
counting an attempt against it (§16) — a bounded, self-healing failure instead of an unbounded one.

Measured on this stack, one slot rendering the heaviest clip in the test matrix (1080p60 → 720p):

| | container memory |
|---|---|
| idle | ~20 MiB |
| one transcode in flight | ~520 MiB peak |

**This table predates the quality tiers (§27)**, which stopped downscaling a Full re-encode to 720p —
so on an instance that takes uploads larger than that, read it as a floor rather than a ceiling; §27's
own figure (0.61 GiB peak, from a full-resolution source) is the current one to budget from. Much of
the peak either way is page cache for the scratch file and is reclaimable, so read it as the ceiling
one slot can ask for rather than as its resident cost. Measure your own with
`docker stats $(docker compose ps -q app)` during an upload, or exactly:

```sh
docker compose exec -T app cat /sys/fs/cgroup/memory/memory.max_usage_in_bytes   # cgroup v1
docker compose exec -T app cat /sys/fs/cgroup/memory.peak                        # cgroup v2
```

Budget **~0.5 GiB and one core per slot**, on top of the idle process, and raise `APP_MEM_LIMIT` /
`APP_CPUS` with `TRANSCODE_CONCURRENCY`.

> **`APP_CPUS` must not exceed the host's core count.** The two knobs are not symmetric: the daemon
> accepts a `mem_limit` above physical RAM, but a `cpus` above the number of cores is a hard **container
> creation** error — `Error response from daemon: range of CPUs is from 0.01 to N, as there are only N
> CPUs available` — so `docker compose up -d` (including §16 step 2 of the upgrade checklist) finishes
> with **no app container at all**, and `restart: unless-stopped` cannot help because nothing was ever
> created. `nproc` on the host is your ceiling. The `1.0` default is the documented one core for the
> default `TRANSCODE_CONCURRENCY=1` and is safe on a 1-vCPU VPS; raise both together.

The rule of thumb is

```
TRANSCODE_CONCURRENCY <= min(cores, RAM_GiB)
```

and the variable is bounded 1 … 16 at boot (DESIGN.md §20 erratum 104), so a trailing zero is an error
that names the variable rather than an OOM kill twenty minutes later.

**A bad `.env` names every mistake at once.** Configuration is read once at boot and every invalid value
is reported — one `clippet: config:` line per variable, then exit 2 — so a container that will not start
tells you the whole list in one `docker compose logs app`, instead of one item per restart:

```
clippet: config: TRANSCODE_CONCURRENCY: must be <= 16, got 40
clippet: config: TRANSCODE_CRF: must be <= 51, got 52
clippet: config: LOG_FORMAT: must be one of text|json, got "yaml"
```

**Why chunks: the edge's 100 MB cap.** A Clippet behind Cloudflare — every Pass name, and any instance
on a Cloudflare Tunnel or a proxied DNS record — sits behind an edge that refuses any one request body
over its plan's cap: 100 MB on the Free and Pro plans, 200 MB on Business. The refusal is a 413 from the
edge itself, before the guard Worker or the tunnel sees the request, so nothing reaches the box and
nothing is in its log. Cloudflare's own error-413 page names the remedy first: break the upload into
smaller requests. So the web uploader sends a file larger than `UPLOAD_CHUNK_BYTES` as a session — open,
the chunks in order, finish — and the finish runs the same ingest the one-shot upload does. A file at or
under one chunk is sent exactly as before. The default 50 MiB leaves room under 100 MB for the request's
headers; on a zone with a higher cap you may raise it toward that cap (fewer requests), never past it.
Nothing changes for the Discord bot or the drop folder, which never cross the edge with the file. A
capture tool posting to `POST /api/upload` through the edge is still one request, and still meets the
cap; the session routes are open to an `upload` token if it can speak them.

### 7.1 Running a dev instance beside production: `INSTANCE_BADGE`

Two Clippets on one desk is a normal shape — the box your friends use, and the one you break. The
danger is not that they exist; it is that the two look **identical**, so you upload the clip into the
wrong one, paste the wrong link, or debug the live box for ten minutes.

`INSTANCE_BADGE=dev` in the second instance's `.env` fixes that with one word:

```ini
INSTANCE_BADGE=dev
```

- **The wordmark goes monochrome.** The green span under the double p paints grey instead, everywhere
  the mark appears — the header, the front door, the login page, the 404, and the small overlay on the
  embedded player. The rule to remember is the one the Discord icons already use: **green span =
  production, grey span = dev.** A face the mark pulls (§20 erratum 274, `web/STYLE.md`) goes grey
  with it; a failed clip's frown stays red, because red means loss and that has nothing to do with
  which box you are on.
- **The word follows the mark**: a small uppercase tag, `DEV`, on the same line as the wordmark and 8 px
  after it — in the header and on the big front-door mark alike, one placement everywhere. The mark
  itself does not shift to make room: on the pages that centre it, it stays exactly where it was.
- **Every page title gains it**: `dev · Settings · Clippet`. Twenty tabs sort themselves out.
- **Cards say it too.** The site line Discord draws above a card reads `clippet dev` instead of
  `clippet` (and the oEmbed `provider_name` with it), so a card from the dev box is recognisable in a
  channel that carries both. The card's mark is the instance's too: on a badged instance the page's
  own Discord card (§28) shows `/static/icon-dev-192.png` — the same mark and tile as `icon-192.png`
  with the dev application's grey span, drawn by the same generator — instead of the green one.
  Everything else on the card — the poster, the player and the link — is the clip's.
- **An installed app is named `Clippet dev`** on the phone, so two icons are two names.
- **The Discord application's icon says it too.** `docs/brand/discord/` holds the companion mark on
  its ink tile at 512 and 1024: `clippet-*.png` with the green span for the production application,
  `clippet-dev-*.png` with a grey one for the dev application — the same rule as the wordmark's,
  drawn rather than styled, so it holds in a channel where nothing else of this page is visible.
  Upload it by hand in the developer portal (App Icon, under General Information); nothing on the
  instance reads or serves these two (the card's grey mark is the served `icon-dev-192.png` above). Both files are generated (`scripts/lib/mkicons.py`, §28) and never
  hand-edited.

Everything else is identical: the same routes, the same API, the same DTOs, the same `/healthz` (the
badge is deliberately **not** on it — it is for an eye, and the anonymous surface stays the size it
was), the same favicon. An instance with no `INSTANCE_BADGE` serves byte-for-byte the pages it served
before this variable existed, so **production needs no change and gets none**.

The value is one word a person reads: lowercase letters, digits and `-`, at most 12 characters.
`Dev`, `dev box` and `dev_box` are boot errors naming the variable — deliberately, rather than being
quietly corrected, because a page that disagrees with the file that configured it is the confusion
this exists to remove.

The rest of a second instance is the ordinary separation: its own `COMPOSE_PROJECT_NAME`, its own
`CLIPPET_PUBLISH` port, its own `PUBLIC_BASE_URL`, its own volumes, and its own bucket or
`S3_KEY_PREFIX` (§7, §32). **And its own tunnel**: a second connector started on the production
tunnel's token joins that tunnel and starts serving the production hostname from the other box, which
is the one mix-up a badge cannot warn you about because the page it appears on is the page a stranger
asked for — give the dev instance a tunnel and a hostname of its own (§3). If both instances are in
the same Discord server, give the dev one command names of its own as well (§30.2, §30.8) —
`/upload-dev`, `/clip-dev` and `Enrich (dev)` beside the defaults — so the badge and the commands agree
about which box you are talking to.

## 8. Day-to-day operation

```sh
docker compose logs -f app                  # structured lines: level=… msg=http method=… status=… dur=… request_id=…
docker compose logs --since 30m app         # bounded window (add MSYS_NO_PATHCONV=1 only if you pass container paths)
docker compose ps                           # the app's health (the one service)
docker compose restart app                  # picks up nothing new — config is read at boot; use `up -d` after .env edits
docker compose pull && docker compose up -d  # take the newest published image and recreate
```

**The log is rotated, and it lives on the same disk as everything else.** `compose.yaml` gives `app`
`json-file` with `max-size: 50m` and `max-file: 5`, so it keeps about **250 MB** and
older lines are dropped. That is not tidiness: the daemon's default is *unbounded*, the app writes one
INFO access-log line per request (only `/healthz` and `/readyz` drop to DEBUG), and the file the
daemon writes it to shares a filesystem with `app_data` and `app_scratch` on a default
Docker install (§1) — where `SCRATCH_MIN_FREE_BYTES` does not protect it. A scanner on a public
origin can otherwise write a GiB a day into the same free space the database needs.

- `docker compose logs --since 30m app` still works exactly as before; it reads the rotated files.
- On a busy public origin the knob is **`LOG_LEVEL=warn`** in `.env` — it drops the per-request access
  lines entirely and leaves the failures, which is what you would grep for anyway.
- If you want history rather than a rolling window, set `LOG_FORMAT=json` and ship the lines off-box
  (any Docker log driver, or a collector reading the same files). Rotation is a ceiling, not a
  retention policy — see §18.

Admin CLI — runs inside the app container against the same SQLite file:

```sh
docker compose exec -T app clippet admin stats                  # users, clips by state, queue depth, bucket usage
docker compose exec -T app clippet admin ls                     # every clip; `ls <id>` for one row + its keys + latest job
docker compose exec -T app clippet admin ls <id> --stat         # also stat each key in the bucket
docker compose exec -T app clippet admin expire <id> --ago 2h   # backdate expires_at; the sweeper destroys it next tick
docker compose exec -T app clippet admin orphans                # bucket prefixes with no row, and rows whose objects are missing
docker compose exec -T app clippet admin orphans --delete --min-age 1h   # remove the orphaned prefixes (rows are never removed)
docker compose exec -T app clippet admin orphans --clip <id> [--delete]  # the same reports, and the delete, scoped to ONE clip's prefix (§24.4)
docker compose exec -T app clippet admin users                  # every user: name, registered, clips, collections, live tokens
docker compose exec -T app clippet admin users prune 'e2e_%' --older-than 24h --no-clips
```

`clippet admin expire <id>` with no flag expires immediately. The sweeper ticks every
`SWEEP_INTERVAL` (60s) and destroys objects before the row, so a clip is gone from the API within
about two ticks.

**The rest of the CLI is in §24 (Operator toolbox)**, because it is what you reach for when something
is wrong rather than every week: `admin doctor` (a preflight you can run against an edited `.env`
*before* `up -d`), `admin jobs` and `admin ls --state` (which clips ended badly and why),
`admin retry` and `admin jobs unstick` (putting them back), the third `admin orphans` report, and
`admin backup <dir> --rotate N`. Accounts, sessions and tokens are §19.4.

**Timestamps come from the CONTAINER's clock, not the host's.** Every `created_at`, `expires_at`,
`started`/`finished` you see in `admin ls`, `admin ls --stat` and the API is minted inside the app
container, and Docker Desktop's time sync can leave that clock seconds behind the host and can step
it BACKWARDS. Two consequences, both observed on this stack: a job can print `finished` earlier than
`started`, and a clip touched immediately after upload can show an `expires_at` a second or two
before its `created_at`. Neither is a bug in Clippet and neither affects a TTL, which is measured in
days. Do not build monitoring that compares two app-minted timestamps at seconds scale; compare
against durations you chose instead (this is also why E2E step 19 asserts a *seven-day jump* rather
than a strict ordering).

**`clippet admin users prune` is the only command that deletes a user**, and it deletes only users
who own **nothing**. Three guards, none of which has a default:

- `<pattern>` is a SQL `LIKE` pattern over the username (`%` matches any run, `_` matches exactly one
  character — so `'e2e_%'` reads as "e2e, any character, anything") and it may not be blank. An
  operator who really means everybody types `%`; "no pattern" is refused so that a typo cannot expand
  into the whole table. Matching is case-insensitive, like registration.
- `--older-than D` is required — whoever registered while you were typing the command is not in it.
- `--no-clips` is required and is the assertion the command makes: a user who owns a clip **or** a
  collection is never removed, whatever the pattern says. That is not politeness. `clips.owner_id`
  CASCADEs from `users`, so deleting an owner would drop their clip rows while the objects stayed in
  the bucket with nothing left to name them — `admin orphans` would find the prefixes, but only
  after the fact.

Sessions and API tokens go with each pruned user through their own cascade; nothing in the bucket is
touched, because a candidate owns nothing there by definition. The command prints the rows it
removed, in the same format `admin users` prints, and a pattern that matches nobody is a normal exit 0.
Its intended use is housekeeping after automation: each `scripts/e2e.sh` run makes four throwaway
users named `e2e_<epoch>`, `e2e_<epoch>_b`, `e2e_<epoch>_c` and `e2e_<epoch>_d` (the last one through
`admin users create`, §19), and deletes its clips but not its users, so a long-lived stack accumulates
four rows per run. Run `admin users` first and read the list; then prune it.

Stopping:

```sh
docker compose stop                # stop containers, keep everything
docker compose down                # remove containers and the network; NAMED VOLUMES SURVIVE (db, scratch, bucket)
docker compose down -v             # ALSO delete the volumes: every user, clip and object is gone. No undo.
docker rm -f clippet-tunnel        # the tunnel is a separate container; removing it retires that public URL
```

`down -v` is the clean-machine reset used by the test suite. It is not a maintenance command.

## 9. Capture tools (ShareX, RePlays, OBS, curl)

Clippet accepts a **single-request upload** from any capture tool that can POST a multipart form.
Behind any Cloudflare edge — a tunnel, a proxied hostname, every Pass name — that one POST is capped
at 100 MB by the edge itself (a 413 that never reaches this instance; the browser's chunked upload is
not affected, §7):

```
POST /api/upload
Authorization: Bearer ck_...
multipart/form-data:
  title       (optional)  text — omitted, or equal to the file's own name, names the clip
                          from the filename (the rule is below the block)
  description (optional)  text
  ttl_days    (optional)  0 | 1 | 7 | 30 | 90   (0 = never; omitted = the server's DEFAULT_CLIP_TTL)
  unlisted    (optional)  true | false | 1 | 0 | on | off | yes | no
  file        (required)  the video — a part named `video` is accepted too

  (a `wait` field is REFUSED with 400: how long the reply is held is the
   server's CAPTURE_READY_WAIT and nothing a client may override — §27)
```

**The title a file name gives** (§20 erratum 317): the name without its extension — unless it carries a capture tool's date and clock (`2025-06-24 00-23-54`, `2025-06-24_00-23-54`, `Arc Raiders 2026.01.06 - 00.15.37.07.DVR`), in which case the title is that date and time as `2025-06-24 00:23`, any text before it kept (`Arc Raiders 2026-01-06 00:15`) and everything after the clock dropped.

Rules that matter when you wire up a tool:

- **The text fields may come in any order, but they must all come BEFORE the file part.** The server
  streams the file straight to disk and answers as soon as it is stored, so anything sent after it is
  never read — a `title` after the file is silently ignored and the title falls back to the filename.
- **Unknown text fields are ignored**, so a tool that always sends extras is fine.
- An **empty** value counts as "not sent" for every field.
- **The bounds on the form itself**: at most **32 text fields** before the file part, and at most
  **16 KiB per text field**. Both answer `400 invalid_request` naming what was exceeded. They are
  guards on a request that has not been authenticated as anything but a token yet, not product limits
  — no real capture tool comes near either.
- **The file part is the FIRST part carrying a `filename` parameter**, whatever it is named, and it must
  then be named `file` or `video`. This matters for one mistake: `curl -F "title=@notes.txt"` looks like
  a text field but curl sends it *with* a filename, so the server takes it as the file part, finds it is
  named `title`, and answers `400 missing_video`. Send text with `-F "title=My clip"` (no `@`); `@` is
  for the video.
- The reply is `201` with exactly:
  `{"id":"aZ3kQ9xB2mLp","url":"https://your.host/aZ3kQ9xB2mLp","embed_url":"https://your.host/embed/aZ3kQ9xB2mLp","state":"ready","expires_at":"2026-09-11T18:20:06Z"}`.
  **`url` is the link to paste** — it is the share page, and it unfurls as a playable card in Discord
  and X. `state` is normally `ready`, because of the wait described next; with the wait off it is
  `queued`, and transcoding starts immediately while the page shows progress until it is done.

#### The wait before the reply (`CAPTURE_READY_WAIT`)

By default the server holds this 201 for up to **30 seconds**, waiting for the clip's **Discord card**
to work, and then answers with its real `state` plus `card_ready`. A typical clip is ready in a second
or two, so you normally get `"state":"ready"`, `"card_ready":true` and a reply that feels instant.

It is there for one reason. **Discord caches the embed it extracted, per URL.** Paste a link while
the clip is still `queued` and Discord unfurls it as a bare link-out card — a thumbnail you have to
click — and it keeps showing that card long after the clip became playable. (This is the first thing
to check if you ever see "a card with a thumbnail, but clicking opens a browser tab".) The wait
closes that window at the only moment it can be closed: before the URL exists on anybody's
clipboard. It costs nothing, because ShareX and every other capture tool is blocked on the HTTP
response regardless — waiting for the *reply* is waiting the tool was already doing.

- Per server, and ONLY per server: `CAPTURE_READY_WAIT` in `.env` (§7). `0` switches it off.
- **A `wait` field is a `400`.** It used to override the wait per request, and `wait=0` bought a faster
  answer and a permanently link-out card for everyone who read that link — not a trade a client gets to
  make for your instance (§27). An empty `wait=` is still ignored, like every other empty field.
- A wait that runs out is **not** an error — you still get `201` and the same link, with the state the
  clip is really in and `"card_ready":false`. So is a clip that fails to transcode: `201` with
  `"state":"failed"`. There is deliberately no estimate of how much longer it would have been.
- What it waits for is the CARD, not just a playable clip: the poster and the file the card points at
  (§27, "Time to shareable").
- It applies to `POST /api/upload` only. The browser uploader answers at once and shows a progress
  bar instead.
- Errors use the same envelope as the rest of the API: `{"error":"snake_code","message":"human text"}`.
  The full set this endpoint returns:

  | status | code | means |
  |---|---|---|
  | 400 | `invalid_request` | a field failed validation (the `message` names the rule) |
  | 400 | `invalid_ttl` | `ttl_days` was not one of `0 1 7 30 90` |
  | 400 | `missing_video` | no part carried a filename, or it was not named `file`/`video` |
  | 400 | `empty_video` | the file part was zero bytes |
  | 400 | `upload_interrupted` | the client went away mid-body |
  | 401 | `unauthorized` | no credential, or a revoked/unknown token |
  | 403 | `insufficient_scope` | the token lacks the `upload` scope |
  | 403 | `csrf` | a cookie-authenticated POST from another origin (tokens never hit this) |
  | 413 | `too_large` | over `MAX_UPLOAD_BYTES` |
  | 415 | `not_multipart` | the body was not `multipart/form-data` |
  | 415 | `not_a_video` | ffprobe found no video stream, or the file is not one of the containers Clippet transcodes — a screenshot or a PDF lands here, and so does a playlist (`.m3u8`, `.mpd`, an ffconcat list) or a raw elementary stream; a sound is accepted (§27, "Audio clips"), but DSD, Monkey's Audio, two or more audio tracks and more than eight channels land here too |
  | 429 | `rate_limited` | over `RATE_UPLOAD_PER_HOUR` (per account) **or** `RATE_UPLOAD_PER_HOUR_PER_IP` (per address) — an upload must satisfy both; carries `Retry-After` |
  | 500 | `id_collision` | a share-id collision; send the same request again |
  | 503 | `unavailable` / `storage_unavailable` | the database or the bucket is down (carries `Retry-After`) |
  | 507 | `insufficient_scratch` | the scratch disk has no room for this upload |

  One answer in that table can reach you wearing someone else's clothes:

  | what you see | what it actually is |
  |---|---|
  | `502 Bad Gateway`, an **HTML** page from the proxy, on a large file | the token was refused *before* the body was read. The app answers `401` in microseconds and closes; a reverse proxy (Cloudflare's tunnel included) is still streaming your gigabytes into that closed connection and reports the reset as its own 502. The same credential on a small file gives the clean JSON `401`. Check `/settings` — the token was revoked, mistyped, or belongs to another instance. |

  The tell is the content type: every error Clippet itself produces is `{"error":…,"message":…}` JSON.
  An HTML error page did not come from the app.

  **Only `503` (and `429`, after its `Retry-After`) is worth an automatic retry** — both are "come back
  later", and both say when. `500 id_collision` is safe to repeat once. Everything else is a verdict on
  the request or the credential: retrying sends the same bytes to the same answer.
- The upload rate limit (`RATE_UPLOAD_PER_HOUR`) is **per user, shared with the browser uploader** — a
  token does not buy a second budget. Since it bounds an *account*, and accounts are free, there is a
  second bucket beside it, `RATE_UPLOAD_PER_HOUR_PER_IP` (120): an upload must satisfy both. Both
  answer the same `429 rate_limited` with `Retry-After`.
- A token carries **scopes**, ticked when you mint it (§19.5). `upload` is the capture-tool
  credential this section is about; `read` lets a script read the same library listing your browser
  sees — every clip *listed* on this instance plus your own unlisted ones, and your own view counts —
  and nothing else. Neither can change your password, sign anything out, mint another token, edit, delete,
  retry or trim a clip, or download an original — those answer `403 token_not_allowed`. Revoke a token
  from **`/settings`** (or `clippet admin tokens revoke`, §19.4) at any time and it stops working on
  the next request.

### ShareX

1. Log in, open **`/settings`**, and create a token in the **API tokens** section (name it after the
   machine, and leave **Upload** ticked — a token without it gets no `.sxcu`, §19.5).
2. The raw token appears **once**. Click **Download clippet.sxcu** right there — the file is built in
   your browser from that one response and carries the token, so it is never stored on the server and
   never appears in a URL. Treat the downloaded file as a password.
3. Import it into ShareX: **double-click the `.sxcu` file**, or
   **Destinations → Custom uploader settings → Import → From file**, then set that uploader as the
   active **File uploader** under **Destinations**. The file declares `FileUploader` only, on purpose:
   Clippet stores videos, so making it the *image* destination would send every screenshot here to be
   rejected with `415 not_a_video`.
4. Capture a clip. ShareX uploads it and puts the share link on the clipboard.

**No browser in front of you? Mint it from the shell instead.** Steps 1 and 2 need a logged-in session,
and two situations have none: setting a machine up for somebody else, and a box whose browser cannot
reach this instance. The operator CLI mints the same token and writes the same file:

```sh
# the raw token, shown ONCE, on stdout — everything else goes to stderr
MSYS_NO_PATHCONV=1 docker compose exec -T app clippet admin tokens mint alice --name "the gaming pc"

# …or the finished ShareX uploader, ready to double-click (step 3 above)
MSYS_NO_PATHCONV=1 docker compose exec -T app clippet admin tokens mint alice --sxcu > clippet.sxcu
```

`--scopes upload` is the default, exactly as it is for the settings page; `--scopes upload,read` mints
both. It is an ordinary token — it appears in `/settings` and in `clippet admin tokens list alice`, and
either place revokes it. **`--sxcu` needs the `upload` scope** and says so rather than writing a file: a
ShareX destination that cannot upload imports cleanly and then answers `403` on the first capture.
Treat both outputs as a password; stdout is the only place the raw value is ever printed (§19.5).

What the file contains (this is the whole schema Clippet uses; the token is redacted here):

```json
{
  "Version": "14.1.0",
  "Name": "Clippet",
  "DestinationType": "FileUploader",
  "RequestMethod": "POST",
  "RequestURL": "https://your.host/api/upload",
  "Headers": { "Authorization": "Bearer ck_xxxxxxxx..." },
  "Body": "MultipartFormData",
  "FileFormName": "file",
  "URL": "{json:url}",
  "ErrorMessage": "{json:message}"
}
```

`{json:url}` is what ShareX copies to the clipboard. Two fields are deliberately absent. There is no
`ThumbnailURL`: ShareX wants an image URL there and this endpoint returns pages, so a thumbnail entry
would only ever render broken. And there are no `Arguments` — a `title: {filename}` argument looks
helpful, but ShareX expands `{filename}` **with** the extension, so every card would read
"2026-09-04_18-22-13.mp4". Sending only the file part lets Clippet name the clip from the same filename
by the rule above: "2026-09-04 18:22". (If your tool insists on sending a title and fills it from the file name, that is handled
too: a title that is exactly the file's own name is treated as if it had not been sent. Any other title
is kept exactly as you send it.)

The file sends no `wait`: the field is refused with a `400` and the server already holds the reply
until the card works (§27), so what ShareX copies is a link that unfurls as a player on the first
paste. ShareX shows its "uploading" state for as long as the server waits, which is the waiting it was
doing anyway. Adding *other* arguments is a trap (`title: {filename}` is the one erratum 60 exists
about).

If you lose the file, mint a new token — the old one cannot be re-rendered, by design. Revoke the lost
one from `/settings`, or from the CLI if you cannot log in as that user
(`clippet admin tokens list <user>` then `tokens revoke <user> <tid>`, §19.4); the CLI can mint the
replacement too (`tokens mint <user> --sxcu`, above).

### An inbound URL — for a tool that can only POST a file

Some tools cannot send an `Authorization` header. An inbound URL carries the credential in its path:

```
POST {your host}/in/ci_...
multipart/form-data: the same fields as /api/upload, file last
```

Make one at `/settings` (API tokens, Kind: Inbound URL) or from a shell:

```sh
clippet admin tokens mint <username> --scopes inbound --name "Stream deck"
```

The URL is shown once. Use it:

```sh
curl -F "file=@clip.mp4" {your host}/in/ci_...
```

The reply is 201 and the share link as one line of text; send `Accept: application/json` for the JSON `/api/upload` answers. The wait, the rate limits, the quota and the size cap are `/api/upload`'s.

- **The URL is the password.** Anyone who has it can upload as that account until it is revoked, at `/settings` or with `clippet admin tokens revoke`. It is not a Bearer token and no other route accepts it.
- **A wrong or revoked URL answers 404**, the answer of a path with no route.
- **Clippet's log writes the path as `/in/{token}`.** A reverse proxy in front of Clippet keeps its own access log with the full path: switch that log off for `/in/`, or guard it as you would the URL. cloudflared at `--loglevel debug` writes request paths; leave it at the default.
- **Files only.** There is no field that makes the server fetch an address.

### A Clippet Auto URL — one press in Discord

One press needs a URL that lets Clippet Auto tell this instance to fetch a video from Discord. Make one at `/settings` (API tokens, Kind: Clippet Auto URL) or with `clippet admin tokens mint <user> --scopes autofetch`, as the account the server is connected to. In Discord: `/settings` → Clippet Auto → Edit → One press, and paste it. It takes no file, fetches only Discord attachments, and works only for servers connected to that account. Revoke it to end one press. The log rule for `/in/` above applies to it.

- **Its limits** (DESIGN.md §20 erratum 327). The account's upload budget (`RATE_UPLOAD_PER_HOUR`), and a second bucket of `RATE_UPLOAD_PER_HOUR_PER_IP` per **URL** rather than per address: every press reaches your instance from Clippet Auto's own egress, so an address bucket would be one cap for every server's presses at once. At most **two** of an account's fetches run at once; a third press while two are downloading is answered 429 and may be pressed again. The video's size as Discord states it is the size accepted: a download that runs past it stops there and is refused as too large. A file name longer than 255 bytes is shortened to 255 (its extension kept), never refused.

### RePlays, OBS scripts and anything else

The generic recipe is the same three ingredients: **`POST <base>/api/upload`**, the header
**`Authorization: Bearer ck_...`**, and a multipart body whose **file field is named `file`** (`video`
also works). Read the share link out of the response's `url` field. In a tool that lets you name the
file field and add form arguments (RePlays' custom/HTTP uploader, most OBS upload scripts), that is
the entire configuration; nothing else about Clippet needs to be described to it.

### curl

The exact one-liner — this is what the E2E runs, and the fastest way to prove a token works:

```sh
curl -sS -H "Authorization: Bearer ck_xxxxxxxx..." \
  -F "title=Ace clutch" -F "ttl_days=7" \
  -F "file=@clip.mp4;type=video/mp4" \
  https://your.host/api/upload
```

```json
{"id":"aZ3kQ9xB2mLp","url":"https://your.host/aZ3kQ9xB2mLp","embed_url":"https://your.host/embed/aZ3kQ9xB2mLp","state":"ready","expires_at":"2026-09-11T18:20:06Z"}
```

Git Bash on Windows: a host path handed to native curl must go through `cygpath -w`, i.e.
`-F "file=@$(cygpath -w "$PWD/clip.mp4");type=video/mp4"`.

`-F "wait=…"` is refused with a `400` naming `CAPTURE_READY_WAIT`: how long the reply is held is the
server's decision (§27). To get the reply the instant the original is stored, set
`CAPTURE_READY_WAIT=0` in `.env`.

Poll `GET <base>/api/clips/<id>/progress` if you want to know when it is playable; the share page shows
the same progress on its own, so a script normally just pastes `url` and walks away.

**"Upload from a URL" is this, on your machine.** There is deliberately no endpoint that fetches a URL
of your choosing — a server-side fetch of an arbitrary address is by definition a second outbound call site, the
one thing README promises does not exist, and it would reach everything inside your compose network
and your LAN. Do the fetch where it belongs, on the client, and stream it straight
through:

```sh
curl -sL "https://example.com/some/clip.mp4" \
  | curl -sS -H "Authorization: Bearer ck_xxxxxxxx..." \
      -F "title=From a link" \
      -F "file=@-;filename=clip.mp4;type=video/mp4" \
      https://your.host/api/upload
```

`@-` reads the video part from stdin, so nothing is written to disk on either side. The upload is
chunked, which the scratch precheck accounts for (DESIGN.md §8.3 step 2 reserves
`MAX_UPLOAD_BYTES/2` for a body that declares no length), and `MAX_UPLOAD_BYTES` still applies.

### Embed parameters

`embed_url` above is `/embed/{id}`: the chrome-less player X iframes and you can put in a page of
your own. **The share page now writes this snippet for you** — "Copy embed" on any ready clip puts the
element below on your clipboard, sized from that clip's own width and height so the box matches its
aspect (a phone capture gets a 9:16 box, not two black bars). Type it by hand only if you want
parameters on it. Three query parameters shape it:

```html
<iframe src="https://<origin>/embed/aZ3kQ9xB2mLp?autoplay=1&loop=1"
        width="640" height="360" frameborder="0" allowfullscreen allow="autoplay"></iframe>
```

| | |
|---|---|
| `autoplay` | start playing on load. **Implies `muted`** — every current browser refuses to start an unmuted video without a user gesture, so autoplay alone would give you a player that silently does nothing. It also switches preloading from `metadata` to the whole file. |
| `muted` | start muted; the viewer can unmute with the normal controls. |
| `loop` | play again from the start when it ends. |

`1`, `true`, `yes`, `on` or the bare parameter (`?autoplay`) mean yes; anything else — including
`?autoplay=0` — means no. Leave them off and the player behaves exactly as it always has: no
autoplay, sound on, one pass, `preload="metadata"`. Cross-origin iframes need `allow="autoplay"` on
the element as well; that is the embedding page's business, not the server's.

The **share page ignores these parameters**: `/{id}?autoplay=1` renders the ordinary watch page (and `/clips/{id}?autoplay=1` redirects to it, query and all — §29).
Someone opening a link you sent them did not ask for autoplay.

### Downloading a clip

Every viewer of a ready clip gets a **Download** link on the share page, and the owner gets a second
one — **Download original**, with the source size beside it — that hands back the untouched upload.
There is no paywall, no watermark and no account gate on either; a self-hosted clip host that would not
give you your own file back would be a strange thing to run.

Under the Download link is one optional query parameter on the ordinary rendition route:

```sh
curl -OJ 'https://<origin>/api/clips/aZ3kQ9xB2mLp/video.mp4?download=1'
```

| | |
|---|---|
| what it adds | `Content-Disposition: attachment; filename="<clip-title-slugged>.mp4"` — so a browser saves the file (and names it) instead of navigating to a player, which is the only way to get a file out of iOS Safari. |
| what it does **not** change | anything else at all: the same bytes, the same `ETag`, `Last-Modified` and `Cache-Control: public, max-age=3600`, the same `Range` handling and the same `304`/`412`/`416` answers. There is no `Vary`, because the query string is already part of the cache key. A resumed download works exactly as a seek in the player does. |
| what other values do | nothing. `?download=0`, `?download=yes` and a bare `?download` are all ignored and serve the plain response; the parameter can never make the route fail. |

`GET /api/clips/{id}/original` is the other half and is **owner-only** (a browser session, not an API
token): it serves the file exactly as it was uploaded, in every state except `deleting` — including a
clip whose transcode failed, which is when people actually need it.

## 10. Clip lifetime: TTL, keep-alive, keep forever

Every clip carries its own lifetime. Nothing is deleted for any other reason — there are no quotas, no
tiers and no "free clips expire" rule; expiry is a **tool the uploader points at their own clips**, and
one click makes any clip permanent.

**Where the lifetime comes from.**

- `DEFAULT_CLIP_TTL` (§7) applies to an upload that does not choose: `never` (the default), `1d`, `7d`,
  `30d`, `90d`. It is resolved **at upload time** into the row — changing the variable later does not
  retroactively re-age clips that already exist.
- A per-clip TTL always wins: the upload form's *Delete after* select, `"ttl_days": 0|1|7|30|90` in the
  `metadata` part of `POST /api/clips`, or the `ttl_days` field a capture tool sends (§9). **`0` means
  never**, and omitting the field means "use the server default" — the two are different on purpose.
- `PATCH /api/clips/{id}` with `{"ttl_days":n}` changes it afterwards, recomputing the expiry **from
  now**. `{"ttl_days":0}` is *keep forever*.

**Resetting the timer (keep-alive).** `POST /api/clips/{id}/touch` sets `expires_at = now + ttl_seconds`
— one owner request, no body, 200 with the clip. The share page wires it to **Copy link**: when the owner
copies a link to a clip that still has a countdown, the countdown restarts. A clip you keep sharing does
not quietly disappear, and a clip nobody has touched in 30 days does. If the clip is already permanent,
copying touches nothing.

Two things about that button are deliberate: the copy happens **first**, so a keep-alive that cannot
reach the server never costs you the link; and the timer resets **only when the OWNER copies from the
share page**. An anonymous visitor — or anyone the link was passed on to — cannot extend anything. The
route is session-only and owner-only: a visitor gets 401, another user 403, and an API token 403
`token_not_allowed` (capture tokens upload, they do not manage clips).

**Keep forever.** The owner's **Keep forever** button on the share page is the one-click `ttl_days:0`; the
lifetime line updates in place. Nothing else about the clip changes and the share URL is unaffected.

**When the deletion actually happens.** A background sweeper ticks every `SWEEP_INTERVAL` (60s, plus one
run 10s after boot) and destroys what is due: **objects first, then the row**, so the system converges on
"no orphaned objects" if it is interrupted. A clip that is **mid-transcode** at its expiry gets a grace
period of `SWEEP_GRACE` (1h) to finish rather than being cut off — after that it goes whatever state it is
in. Expect a clip to disappear from the API within about two ticks of its expiry, not at the second.

A touch that lands **after** the sweeper has listed the clip but before it deletes it still wins: the
tombstone write re-checks the expiry, so the clip is skipped and logged (`destroy … note="no longer
expired; skipped"`). Reaching for Copy link seconds before the deadline works.

**A clip destroyed mid-upload-to-the-bucket.** The destroy waits for the clip's own job to exit before it
touches the bucket, and that job now removes every object of its attempt **by key** rather than by
listing a prefix (DESIGN.md §20 erratum 305). That distinction is invisible on a server whose listings
are immediately consistent and it matters on R2 and B2, whose bucket LISTINGS are an eventually consistent index: an object whose upload
finished a millisecond ago can be missing from the very next listing, and a cleanup that asked the
listing therefore removed nothing and said it had. The symptom was a clip that disappeared from the API
on time and left exactly one object behind — most often a `renditions/main-….mp4` — which `clippet admin
orphans` then reported as a prefix with no row. If you are on a build older than this one and see that
shape, `admin orphans --delete --min-age 1h` is the right answer and always was; on this build the run
removes its own objects before the destroy ever lists them.

**When a deletion keeps failing.** A `Destroy` needs the bucket, and a bucket that black-holes one prefix
used to be able to eat every tick: `SweepCandidates` returns the same list each time, each attempt costs up
to `STORAGE_OP_TIMEOUT`, and everything behind it starved. So a clip whose `Destroy` fails is **backed off
per clip** — 2 minutes, doubling to an hour, with the first retry after a failure free so a whole batch
that failed because storage was down comes back in ONE tick when storage returns. The line names the clip
and its failure count:

```
level=WARN msg="sweep: destroy failed; backing this clip off" clip=dDvdz86vMM1y failures=3 retry_in=8m0s err=...
```

That schedule now **survives a restart** (`sweep_failures`, migration 0013). It is deliberately not the
same thing as forgetting it: a restart *you* perform plausibly fixes storage, but the restart nobody
performed — `restart: unless-stopped` plus a process dying every few minutes — used to reset the backoff on
every boot and hammer the same failing prefix for as long as the loop lasted. A restarted app now says what
it remembered and honours it:

```
level=INFO msg="sweep: restored the destroy-failure backoff a previous process left behind" clips=2
```

You never have to clean that table up: the row goes when the Destroy finally succeeds, and a clip removed
any other way takes its row with it. `clippet admin stats` prints how many clips are in it, so the state
has a surface that is not the log:

```
backoff: clips=0 waiting out a destroy failure (DEPLOY.md §10)
```

If you want a failing clip retried **now**, restart is not the lever —
fix the bucket, or force it with `admin expire` after the backoff has passed.

**Forcing an expiry (admin).** To retire a clip on a schedule of your own — or to test the sweeper:

```sh
docker compose exec -T app clippet admin expire <id> --ago 2h   # backdate expires_at; gone next tick
docker compose exec -T app clippet admin expire <id>            # expire immediately
```

This only moves `expires_at`; the clip's `ttl_seconds` is untouched, so the owner touching or PATCHing it
before the next tick rescues it. To remove a clip **now**, use the owner's Delete button or
`DELETE /api/clips/{id}`, which destroys objects and row inline.

## 11. Discord auto-post

Set `DISCORD_WEBHOOK_URL` and every clip that finishes transcoding is posted to that one Discord
channel as a message with the title and the share link — which Discord then unfurls into the same
playable card anyone gets from pasting the link by hand. That is the whole feature: one HTTP POST per
clip, no bot to invite, no OAuth, no per-user setup.

**What that card says** (nothing here is configurable): the small grey line above the title is your
instance's own host — `clips.example.com`, not the product's name — so a reader knows whose box served
the clip; the description is `{m:ss} · {your description} · {the quality clause when a Light copy is
playing}` (§27); the picture is the clip's poster with its length in the corner; and the bar down the
left edge is green when the clip is ready, amber while it is still encoding and red if it failed.

**Read this before you set it.**

- **One webhook per instance, one channel — unless you route by tag.** There is no per-user routing.
  By default everything goes to the same place; `DISCORD_WEBHOOK_ROUTES` sends clips carrying a given
  tag to a channel of their own, and §25.5 is the whole of it. Everything else on this page applies
  either way.
- **EVERY listed clip from EVERY user is posted** — not just yours. If registration is open and a
  stranger uploads something, it lands in your channel. Either close registration
  (`REGISTRATION=closed`, §7) or tell your users, before you turn this on.
- **Unlisted clips are never posted.** That is the opt-out, and it needs nothing new: an unlisted clip
  is already hidden from the library, so announcing it would contradict what the uploader chose. Tick
  *Unlisted* on the upload form (or send `unlisted=true` from a capture tool) and the clip is never
  announced. Making a clip unlisted **after** it is ready but before the post goes out also works.
- **The URL is a credential.** Its last path segment is a token, and anyone who has it can post to
  that channel forever. Treat `.env` as a secret file. The app never logs it, never shows it in
  `/healthz` or `admin` output and never stores it in the database — but if it does leak, delete the
  webhook in Discord and make a new one; there is no other revocation.
- **Clip titles come from users.** A title cannot ping `@everyone` (the app forbids mentions on every
  post). It cannot format the message either: every markdown character in a title is escaped wherever
  it appears, so a title cannot turn the post into a header, a code block, a spoiler or bold text —
  and, the one that matters, it cannot become a **masked link** (`[looks safe](goes-elsewhere)`), which
  Discord renders for webhook posts but not for messages people type. Invisible characters
  (zero-width, right-to-left overrides) become spaces. A title *can* still contain a plain URL, which
  Discord will also unfurl — visible, and pointing where it says. On an instance where you do not know
  every uploader, that is one more reason to close registration.

**Creating the webhook** (Discord, ~30 seconds): open the target channel → *Edit Channel* → 
*Integrations* → *Webhooks* → *New Webhook*. Give it a name and an avatar if you like — those are what
appear on the posts, and Clippet does not override them. Then **Copy Webhook URL**.

**Turning it on**:

```sh
# .env, next to compose.yaml
DISCORD_WEBHOOK_URL=https://discord.com/api/webhooks/1234567890/paste-the-rest-here

docker compose config | grep DISCORD_WEBHOOK_URL   # confirm compose will pass it (it prints the secret)
docker compose up -d
docker compose logs app | grep 'discord webhook'
#   ... msg="discord webhook configured" host=discord.com webhook_id=1234567890
```

That boot line is the confirmation. It prints the host and the numeric id and **not** the token, so it
is safe to paste when you ask for help.

The URL is validated at boot and the app **refuses to start** on anything that is not a real Discord
webhook: it must be `https` (the token is in the path — plain `http` would leak it to every hop), the
host must be exactly `discord.com`, `discordapp.com`, `ptb.discord.com` or `canary.discord.com`, and
the path must be `/api/webhooks/<numeric id>/<token>`. The failure message names the rule that failed
and never echoes your URL. This strictness is deliberate: it keeps "this variable can only ever point
at Discord" a claim you can check, and it means a typo cannot quietly start posting your share links
to someone else's server.

**Turning it off**: delete the line from `.env` and `docker compose up -d`. With it unset nothing is
posted to Discord. Any announcement still queued from before is dropped, not retried.

**What you will see in the log**, one line per clip:

| Line | Level | Means |
|---|---|---|
| `discord webhook configured host=… webhook_id=…` | INFO | Boot; the feature is on. Once per start. |
| `discord webhook delivered clip=… status=200` | INFO | Posted. |
| `discord webhook skipped reason=clip is unlisted` | INFO | The opt-out did its job. Also `clip deleted`, `clip is being deleted`, `clip is <state>`, `webhook not configured`. |
| `discord webhook delivery failed; requeued reason=…` | WARN | A network error, a timeout, a 429 or a 5xx. Retried automatically. |
| `discord webhook not delivered; giving up reason=…` | WARN | Three attempts used. **The clip is fine** — only the announcement was lost. |
| `discord webhook rejected; not retrying reason=rejected: HTTP 404 body=…` | WARN | Discord said no, permanently. Almost always a deleted or wrong webhook. Fix `.env` and restart; existing clips are not re-announced. |

**Failure behaviour, in one sentence: a failed post never affects a clip.** The clip is `ready` before
the announcement is even attempted and stays `ready` whatever happens — no failed state, no failure
reason, no retry of the encode. Retries are three at most (immediately, then +30s, then +60s; a
Discord `Retry-After` on a 429 is honoured up to five minutes), and anything Discord rejects outright
— a 400, a 401, a 404, a redirect — fails at once rather than hammering. Nothing about this feature
ever produces an `ERROR` line, so it will not set off any alerting you build on log levels (in this
app `ERROR` means "a human is needed", and a chat integration Discord is throttling is not that).

**Verifying it end to end**:

```sh
read -rsp 'Webhook URL: ' E2E_DISCORD_WEBHOOK_URL; echo   # not echoed, not written to shell history
export E2E_DISCORD_WEBHOOK_URL
BASE=https://<your origin> bash scripts/e2e.sh
unset E2E_DISCORD_WEBHOOK_URL
```

That runs the normal suite with the webhook wired in, asserts a delivery for every listed
clip of the run and none for the unlisted one, and hands the stack back without the webhook when it
finishes. Read the URL rather than typing it inline: it is a credential, and a command line is kept in
`~/.bash_history` in clear text — the same reason it belongs in `.env` and nowhere else. Expect
roughly eight posts in the channel, which you can delete afterwards. Without the variable the same
step still runs, in reverse: it proves no announcement was queued and nothing was posted to Discord.

### 11.1 A webhook to your own URL

Set `CLIP_WEBHOOK_URL` and `CLIP_WEBHOOK_SECRET` and the app POSTs one JSON document to that URL each time a clip becomes ready — for a script or an automation of your own. Both variables or neither: one alone stops the boot. Unset is off: nothing is sent and nothing is queued.

```sh
# .env, next to compose.yaml
CLIP_WEBHOOK_URL=https://automation.example.com/hooks/clippet
CLIP_WEBHOOK_SECRET=<32 characters or more>      # openssl rand -hex 32

docker compose up -d
docker compose logs app | grep 'clip webhook'
#   ... msg="clip webhook configured" scheme=https host=automation.example.com
```

**What is sent.** One `POST` with `Content-Type: application/json`:

```json
{"event":"clip.ready","id":"Z6eQucmYEyFQ","url":"https://clips.example.com/Z6eQucmYEyFQ",
 "title":"ace on the last round","duration_ms":6000,"owner":"alice",
 "created_at":"2026-10-06T05:33:17Z","expires_at":"2026-10-13T05:33:17Z","unlisted":false}
```

`owner` is the uploader's username. `expires_at` is `null` for a clip that never expires. The title is sent as typed. Never in it: a token, an email, a storage key.

**Read this before you set it.**

- **Every clip from every user is sent, unlisted ones too**, with `"unlisted": true`. The Discord auto-post skips unlisted clips; this does not. If your receiver publishes anything, filter on that field.
- **The URL is a credential.** It is never logged and never stored in the database; the boot line and `admin doctor` show its scheme and host only. Check that host: a mistyped one sends your share links to someone else.
- **Plain `http` is for a receiver on your own network.** Over the internet use `https`: on `http`, anyone on the path can read the document and the URL.
- **A redirect is never followed.** A 3xx answer is a rejection.

**Verifying a delivery.** Every request carries `X-Clippet-Signature: sha256=<hex>`: the HMAC-SHA256 of the raw request body, keyed with `CLIP_WEBHOOK_SECRET` trimmed of surrounding whitespace (the value the app loads never starts or ends with a space), in lowercase hex. Compute the same over the bytes you received, before parsing them, and compare in constant time:

```python
import hashlib, hmac

def verified(secret: bytes, body: bytes, header: str) -> bool:
    want = "sha256=" + hmac.new(secret, body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, header)
```

A clip is sent once, when it first becomes ready; a trim or a replacement sends nothing. A delivery can still arrive twice — a timeout after your receiver has already acted is retried — so de-duplicate on `id`.

**Failure behaviour: a failed delivery never affects a clip.** Five attempts at most (immediately, then +30s, +60s, +120s, +240s; a `Retry-After` on a 429 is honoured up to five minutes), ten seconds each. A network error, a timeout, a 429 or a 5xx is retried; any other answer — a 4xx, a redirect — fails at once. After that the delivery is dropped and logged; it is never sent again and never produces an `ERROR` line. `clippet admin jobs --kind hook` lists each delivery and its outcome until its clip is deleted.

| Line | Level | Means |
|---|---|---|
| `clip webhook configured scheme=… host=…` | INFO | Boot; the webhook is on. Once per start. |
| `clip webhook delivered clip=… status=200` | INFO | Delivered. |
| `clip webhook skipped reason=…` | INFO | The clip was deleted, or the webhook was unset, before the delivery ran. |
| `clip webhook delivery failed; requeued reason=…` | WARN | A network error, a timeout, a 429 or a 5xx. Retried automatically. |
| `clip webhook not delivered; giving up reason=…` | WARN | Five attempts used. **The clip is fine.** |
| `clip webhook rejected; not retrying reason=rejected: HTTP 404 body=…` | WARN | Your receiver said no. Fix it or the URL; a rejected delivery is not sent again. |

**Turning it off**: delete both lines from `.env` and `docker compose up -d`. A delivery still queued from before is dropped, not retried.

## 12. Trim

The share page's **Trim** control (owners only) cuts a clip down without re-uploading it. Two numbers —
*From* and *To*, in seconds — and a button; the page then polls until the new cut is live and reloads the
player onto it.

**The original is never touched, and the times you type are ITS times.** Clippet keeps the file you
uploaded under `original/` for the life of the clip (DESIGN.md §11's "upload is sacred"), and a trim re-derives the
rendition *from that original*, never from the rendition it replaces. **From / To are always measured on
the upload**, not on the cut you are looking at — the share page says so under the two boxes, and pre-fills
them with the cut the clip currently is. So trimming is:

- **repeatable, and reversible** — trim a 30 s upload to 10 s, then to 4 s, then back out to the whole
  30 s, or to seconds 12–18 you had cut away earlier. Every window names seconds of the upload, which
  never changes, so nothing you cut is out of reach; the second trim is not a copy of a copy;
- **lossless in the generation sense** — no clip ever accumulates re-encodes, however often it is cut;
- **safe** — if the trim fails, nothing happened. The clip keeps the rendition it had and stays playable.

Two windows are refused with `400 invalid_trim`: one that runs past the end of the upload, and one that is
*exactly the cut the clip already has* (it would re-encode the same seconds and change every ETag for
nothing). And if the upload's video track stops before the window you asked for — a screen capture whose
recorder died early, say — the job ends with *"there is no video in the part of the original you asked
for"* on the page, at INFO in the log. That is a fact about the file, not a fault, so nothing is retried
and nothing is marked failed.

**Two paths, and the app tells you which it took.** When the original is *already* the shape Clippet
produces — an mp4/mov holding h264 in `yuv420p` with at most one AAC track, no rotation, square pixels,
and small and slow enough that the normal encode would neither rescale it nor cap its frame rate — and
**both cut points land on a keyframe** (within one frame), the trim is a **stream copy**: the coded
frames are copied straight through, so it finishes in about the time it takes to move the bytes and the
picture is bit-for-bit the source's. Anything else is a **re-encode** through the ordinary pipeline, at
ordinary encode speed. Most clips re-encode, because most encoders place keyframes seconds apart and a
cut is rarely on one; that is the correct outcome, not a fault.

```
docker compose logs app | grep 'trim '
#  msg="trim planned" clip=aZ3kQ9xB2mLp job=42 path=reencode reason="the start does not land on a keyframe" …
#  msg="trim applied" clip=aZ3kQ9xB2mLp job=42 path=copy start_ms=1000 end_ms=3000 duration_ms=2067 …
```

**The link never changes; the bytes do.** `/clips/{id}`, `/api/clips/{id}/video.mp4`,
`/social.mp4` and `/poster.jpg` all keep their URLs (and with `MEDIA_DELIVERY=redirect` they keep
them while answering `302` to your bucket instead of the bytes — §33) — a link you already handed out keeps working and a
card already posted to Discord keeps playing. What changes is the **ETag** of each, so:

- anyone loading the page fresh gets the new cut immediately;
- **already-cached players and crawler cards catch up within an hour** (media is served
  `public, max-age=3600`, DESIGN.md §8.5). `public` matters: a shared cache between you and a viewer — a CDN, a
  company proxy — may hand out the pre-trim bytes for that hour too, to people this server never hears
  from. It is the same one-hour window every other rendition change uses; there is no way to shorten it
  without giving up caching altogether. The owner's own browser is the exception: the share page reloads
  the player with a cache-busting query the moment the trim lands, so you see the result at once.

**Applying the cut is a single database UPDATE.** Every attempt writes its output to fresh keys
(`renditions/main-{r8}.mp4`, `poster/poster-{r8}.jpg`), and only when both are uploaded and verified does
one transaction point the clip at them and mark the job done. Until that instant every viewer is served
the old rendition; after it, the new one. There is no window in which a clip is half-trimmed, and an
attempt that dies anywhere before the commit leaves nothing a reader could reach. The superseded
rendition and poster are deleted **after** the commit.

**Limits and failure behaviour.**

| | |
|---|---|
| Who | The owner, from a browser session. An API token gets 403 `token_not_allowed` — capture tokens upload, they do not edit clips. |
| When | Only a `ready` clip (409 `not_ready` otherwise), and only one trim at a time per clip (409 `trim_in_progress`). |
| Window | On the ORIGINAL's timeline: `0 ≤ start < end ≤` the upload's length, at least 500 ms long, and not the cut the clip already has — every violation is 400 `invalid_trim` with the rule that failed. |
| Cost | A trim occupies one of the `TRANSCODE_CONCURRENCY` ffmpeg slots — literally the same pool the encodes use, so a default install still runs one ffmpeg at a time — and counts against the same per-user upload rate limit. It runs on its own queue, so a long trim never delays a Discord announcement, and never jumps ahead of an upload's encode either. |
| Retries | Depends on the failure. A **permanent** one — `output_too_large`, a window the source cannot honour — fails on the first attempt and is not retried, because the same run would fail the same way. A **crash** (a panic, or a restart that caught the job mid-run) gets two attempts, then the job is `failed` with the reason. A **transient** one — storage or the database, neither of which is this file's fault — gets the instance's environment budget instead, `TRANSCODE_MAX_TRANSIENT` (10 by default) with a 1m·2ⁿ backoff, exactly what an encode gets, so a storage outage shorter than ≈2.5 h costs an edit nothing (§20 erratum 89). **A scratch shortage is its own case**: it is requeued every minute with *neither* counter touched, so it never runs out of budget and never becomes a permanent failure — a full disk is yours to fix, not the clip's to die of (§20 erratum 90). The clip is **never** marked failed and never loses its rendition — ask again from the share page when you have fixed whatever it was. |
| State | The clip is `ready` before, during and after. `GET /api/clips/{id}/progress` keeps saying so; the trim has its own status route. |

**From a script** (session cookie, not a token):

```sh
curl -sS -b jar https://<origin>/api/clips/<id>            # as the OWNER: how long the upload is, and the current cut
#  … "duration_ms":3500, "source_duration_ms":30000, "trim":{"start_ms":1000,"end_ms":4500} …
curl -sS -b jar -H 'Content-Type: application/json'   -d '{"start_ms":1000,"end_ms":4500}' https://<origin>/api/clips/<id>/trim
#  {"job_id":42,"state":"queued"}
curl -sS -b jar https://<origin>/api/clips/<id>/trim
#  {"job_id":42,"state":"running","stage":"encode","percent":40,"error":null,"start_ms":1000,"end_ms":4500,…}
```

`source_duration_ms` and `trim` are shown to the **owner only** — they say "this clip is a cut of
something longer", which is nobody else's business — and `trim` is absent while the clip is still the
whole upload.

`state` there is the **job's** — `queued`, `running`, `done`, `failed` or `cancelled` — and `404` means
this clip has never been trimmed, which a poller must not read as "finished". When it is `done` the answer
also carries `applied_start_ms`/`applied_end_ms`: what was actually cut. A stream copy can only start on a
keyframe, so the start may sit a few milliseconds earlier than you asked; a window running past the end of
the video track is shortened to it. The share page re-fills its boxes from those numbers. A clip that has been trimmed keeps that
row: the answer after a restart comes from the database, not from memory.

**One thing to know about the bucket.** Deleting the superseded rendition and poster is best effort: if
storage is unreachable in that one moment the trim still succeeds and the two old objects stay under the
clip's prefix, unreferenced. They are invisible to every route, they are removed when the clip is
deleted, and the log says so at WARN:

```
docker compose logs app | grep 'delete attempt output'
```

`clippet admin orphans` will **not** list them — it reports prefixes with no row and rows whose objects
are missing, not extra objects inside a live clip's prefix — so that log line is the way to notice. In
practice this needs storage to fail in a one-second window, and the cost of it happening is a few
megabytes until the clip goes away.

## 13. GIF previews

The share page's **GIF** control (owners only) makes a short, silent, looping preview of a clip and
hands it back as an ordinary image URL:

```
https://<origin>/api/clips/<id>/preview.gif
```

**Scratch, since the quality tiers (§27):** the job downloads the clip's playable file, which on a
clip whose upload is served as it stands (the fast path) is **the upload itself** rather than a ≤720p
rendition. So `SCRATCH_DIR` has to have room for the largest upload you accept plus
`SCRATCH_MIN_FREE_BYTES` before a GIF can be made. When it does not, the job is requeued with **no
counter spent** — the owner sees "the server had no room" and nothing fails permanently, but it will
keep waiting until there is room, so a small scratch volume on an instance that takes multi-gigabyte
uploads is worth sizing rather than discovering (§1 has the arithmetic).

**It is made on demand, not at upload time.** A clip has no preview until somebody asks for one, and
the first request says so rather than pretending otherwise:

```sh
curl -i https://<origin>/api/clips/<id>/preview.gif
#  HTTP/1.1 202 Accepted
#  Retry-After: 5
#  {"state":"queued"}
#  …a few seconds later…
#  HTTP/1.1 200 OK
#  Content-Type: image/gif
#  Cache-Control: public, max-age=3600
```

That 202 is the normal first answer — not an error, not a 404 (the clip exists), not a redirect. Wait
the `Retry-After` and ask again. Once the preview exists the same URL streams it like any other media
route: ETag, `Range`, `304 Not Modified`, an hour of caching. `GET /api/clips/<id>` reports
`"gif_url": null` before and the absolute URL after, which is the cheapest way for a script to tell.

**The preview is bounded on purpose.** It is cut from the clip's *rendition* (what people watch, not
the original upload), and always:

| | |
|---|---|
| Length | **6 seconds** (or the whole clip if it is shorter), taken from the frame the poster came from — so a clip that opens on a loading screen does not spend its preview on one |
| Frame rate | **12 fps** |
| Width | **480 px** at most, never upscaled; the height follows the aspect ratio |
| Size | hard cap **8 MiB** — a run that would exceed it is killed and the job fails at once (a permanent failure is not retried) |
| Time | a 60-second kill switch per pass |

**Where the six seconds start.** The poster's frame is *chosen* — a pass that looks at a window of the
clip and picks the one that stands out, which is why the card image is not a loading screen — and that
choice is not stored anywhere. So the preview job repeats it on the rendition it has just downloaded
(one short extra ffmpeg pass, inside the same concurrency slot the render holds) and starts there,
pulled back only far enough to fit six seconds before the end. If that pass cannot answer for any
reason the preview falls back to a plain seek 15% into the clip and is made anyway: a preview that
starts in the wrong place is not worth failing a job over. Nothing about the output changes — the same
bounds, the same two palette passes, the same URL.

A six-second 720p clip yields roughly **0.5–1.5 MB**. That is the cost per clip that keeps a preview,
in your bucket, until the clip is deleted — a GIF has no real inter-frame compression, which is why
every one of those bounds exists. Nothing is added to the picture: no watermark, no caption, no logo.

**Storage and lifecycle.** The object lives under the clip's own prefix
(`clips/<id>/renditions/preview-<r8>.gif`), so it is removed with the clip like everything else — no
extra cleanup, no separate retention. It is also the only derived object an operator never has to
make: if nobody asks, none exists.

**Trimming replaces it.** A trim re-cuts the rendition, so the old preview is a picture of frames that
no longer exist. The trim therefore drops it — `gif_url` goes back to `null` and the object is
deleted — and the next request for `preview.gif` makes a fresh one of the new cut. From the share page
that is one click on **GIF** again.

**One orphan this can leave, and why nothing raises it.** Dropping the superseded object happens
*after* the row that stops referencing it is committed — the commit is the truth, the delete is
best-effort. That is the right order (a delete that ran first and then failed to commit would have
destroyed a live object), but it means a delete that fails leaves an object with no row pointing at it
**inside a prefix that is still live**. The same is true of the previous rendition and poster after a
trim. `clippet admin orphans` will not show it: that command reports whole clip *prefixes* with no row,
and this prefix has one. Nothing serves it, nothing links to it, and it goes with the clip when the
clip goes — `Destroy` deletes the prefix, not a list of keys — so the cost is bytes until then. If you
are chasing bucket usage that does not match your clip count, `clippet admin ls <id> --stat` prints
every key a clip references, and anything else under `clips/<id>/` in your bucket browser is one of
these.

**Regenerating.** The owner can ask for a new preview explicitly:

```sh
curl -sS -X POST -b jar https://<origin>/api/clips/<id>/gif
#  {"state":"queued"}          202
#  {"error":"gif_in_progress"} 409 while one is already being made
```

This is a browser-session route (an API token gets 403 `token_not_allowed`, like every other owner
edit) and it counts against the same per-user upload rate limit. Re-rendering an *unchanged* rendition
produces byte-identical bytes, so the ETag does not move and nobody's cache is disturbed; only the
object's key changes.

**When something goes wrong.** A preview that fails is a failed *job*, never a failed clip: the clip
stays `ready`, keeps its rendition and keeps playing. The route then answers `503 gif_failed` with the
reason and — deliberately — does **not** try again on its own, because otherwise every visitor of a
page linking the preview would re-run an encode that is already known to fail. How many attempts it
gets depends on WHY it failed. A **permanent** one is terminal on the first attempt, because nothing
about running the same encode again would change it — `output_too_large` is the common real case: a
preview past the 8 MiB cap fails immediately and stays failed. A **crash** (a panic, or a restart that
caught the job mid-render) gets two attempts, then the job is `failed`. A **transient** one — storage
or the database, neither of which is this clip's fault — gets the instance's whole environment
budget instead, `TRANSCODE_MAX_TRANSIENT` (10 by default) with a 1m·2ⁿ backoff, exactly what an encode
gets, so a storage hiccup no longer costs every visitor the preview until the owner notices and asks
again (§20 erratum 89). A **scratch shortage** spends no budget at all: the job is requeued every
minute with neither counter touched, and the 503 a visitor would see says only `insufficient scratch
space` — never a byte count from your disk (§20 erratum 90). Either way the owner decides what happens
next:

```sh
docker compose logs app | grep 'gif '
#  msg="gif planned" clip=aZ3kQ9xB2mLp job=42 seconds=6 fps=12 width=480 …
#  msg="gif rendered" clip=aZ3kQ9xB2mLp job=42 bytes=1035161 dims=480x270 frames=72
#  msg="gif applied"  clip=aZ3kQ9xB2mLp job=42 key=clips/…/renditions/preview-2NFERf8Z.gif superseded=1
```

`clippet admin ls <id>` prints the `gif:` row and, with `--stat`, checks the object is really there.

**Who may ask.** The GET is public, exactly like `poster.jpg`: the unguessable link is the
authentication, and a preview of a clip anyone with the link may already watch frame by frame gives
nothing away. Because an anonymous caller can therefore start work, two guards stand behind it — at
most one preview job per clip is ever in flight (a hundred simultaneous requests produce one job), and
the route carries the same 60/minute/IP limiter the view beacon uses.

**The cards are unchanged.** `og:image` stays the poster JPEG. Discord and X do not honour an animated
`og:image` for a video card — they play `og:video` / `twitter:player` and show the still — so the
preview is a thing you make and paste yourself, not part of the unfurl.

## 14. Tags and collections

Two ways to keep a growing library findable, and both are deliberately small.

**Tags are words you file a clip under.** They are not a social feature: there is no page that lists a
tag to the world, no count beside one, nothing trending, and nobody but the logged-in users of your
instance can see any of it. A tag exists so *you* can find the clip again.

```
a-z, 0-9 and -            up to 32 characters, starting with a letter or a digit
at most 10 per clip       duplicates and letter case collapse ("Gaming" and "GAMING" are `gaming`)
```

You can set them in three places:

| Where | How |
|---|---|
| The upload form | a **Tags** box, comma separated (`gaming, clutch`) |
| The share page (owner only) | a **Tags** box under Manage — Save replaces the whole list, and emptying it clears the tags |
| A capture tool / `curl` | a `tags` form field on `POST /api/upload`, comma separated |

```sh
# tag at upload time, from ShareX/RePlays or by hand
curl -H "Authorization: Bearer ck_…" \
     -F "tags=gaming, clutch" \
     -F "file=@clip.mp4;type=video/mp4" \
     https://<origin>/api/upload

# or afterwards, from the browser session (the whole set at once)
curl -b cookies.txt -X PATCH -H 'Content-Type: application/json' \
     -d '{"tags":["gaming","clutch"]}' https://<origin>/api/clips/<id>
```

A tag that breaks the rules is refused with `400 invalid_tags` and the message names the word that was
wrong, so you never have to guess which one it was.

**Finding them again.** Every tag on a clip in your library is a link: click it, or go to
`https://<origin>/?tag=gaming`, and the library shows the clips carrying it. The JSON equivalent is
`GET /api/clips?tag=gaming`, which pages exactly like the unfiltered list. Two things to know:

- The filter **narrows** what you can already see — every listed clip plus your own unlisted ones. A tag
  never surfaces somebody else's unlisted clip.
- A page cursor belongs to the filter it came from. Keep `tag=` on the "next page" request; without it
  you get `400 invalid_cursor` rather than a page that quietly skips clips.

**Tags are invisible on the watch page.** Somebody you send a link to sees the clip, the `clippet`
wordmark linking home, and nothing else — no tags, no chips, not even the words. They are yours, not the visitor's.

---

**Collections are ordered lists of links.** A collection holds clips in the order *you* choose and has
its own page you can paste anywhere:

```
https://<origin>/c/<collection-id>
```

Make one from the **Collections** section of the library (title, Create). Its page then gives you, as the
owner, three controls: rename, a clip picker (a checkbox per clip of your own, each with its card
image beside the title — clips already in the collection come first and keep their order, anything you
tick lands after them), and a two-click Delete.
Save sends the whole membership, so a save that takes clips *out* relabels the button to
`Save clips (removes N)` and waits for a second click; a save that only adds goes straight through.
Removing a clip from a collection never deletes the clip.
The picker offers your **200 newest** clips plus everything already in the collection. If you have more
clips than that and want to add an older one, use the API: `PUT /api/collections/{id}/clips` takes any
clip id you own, and the page is the only thing that is capped.

**What a collection is not.** There are no likes, comments, followers or feeds; a collection page shows
no view count, no author name and no "more from this user"; and there is no browse page — yours are
listed to you in the library and to nobody else, anywhere. It is a list of links, and that is the whole
feature.

**A collection link is as unguessable as a compact clip link.** The id is 12 random characters from the
same generator (about 71 bits) — a collection is never minted in the memorable grammar, §29 — and there
is no listing anywhere that enumerates collection ids. That means:

- anyone with the link can open the page and watch what is in it — that is the point;
- anyone *without* it cannot find it, and cannot walk the id space to stumble on it;
- treat the link the way you treat a clip link: sharing it is publishing it.

Changing a collection needs a browser session. A capture-tool token can upload and nothing else, so a
leaked token cannot make, rename, refill or delete a collection (`403 token_not_allowed`).

**On Discord and X**, a collection link unfurls as a **link card**, not a player: a title, the site name
and a picture taken from the first ready clip in the list. That is deliberate — a collection is several
clips, so a player would have to pick one of them for the reader. Paste a *clip* link when you want the
video to play inline; paste a *collection* link when you want the list.

**Deleting things does the obvious thing.** Deleting a clip (or letting its TTL expire) removes it from
every collection it was in, automatically. Deleting a collection removes the list and **not one clip** —
the clips, their links and their cards are untouched.

```sh
# the whole flow with curl, from a logged-in session
curl -b cookies.txt -X POST -H 'Content-Type: application/json' \
     -d '{"title":"Best of last night"}' https://<origin>/api/collections
#  {"id":"CoLLeCtIoN12","title":"Best of last night","url":"https://<origin>/c/CoLLeCtIoN12",
#   "clip_count":0,"created_at":"…","updated_at":"…"}

# the ORDER you send is the order the page shows
curl -b cookies.txt -X PUT -H 'Content-Type: application/json' \
     -d '{"clip_ids":["aZ3kQ9xB2mLp","bY4lR8yC3nMq"]}' \
     https://<origin>/api/collections/CoLLeCtIoN12/clips

# anyone with the link can read it back
curl https://<origin>/api/collections/CoLLeCtIoN12
```

Limits, both of which exist so one request cannot ask for unbounded work: **500 collections per user**
(a 501st is `409 too_many_collections`) and **200 clips per collection**. Only your own clips can go in
one — anything else is `400 not_your_clip`, and the message names the clip. `clip_ids` is required on
that PUT: send `[]` to empty a collection; a body without the key is `400 invalid_request`, so a
truncated request can never be read as "delete everything".

`GET /api/collections/{id}` is public (the link is the auth) and answers with up to 200 clip records —
around a hundred times the bytes of a single clip. It therefore carries the same **60 requests per
minute per IP** limit the view counter and `preview.gif` carry; over it, `429` with `Retry-After`. That
is far above anything a person browsing or curating does. If you expose the origin directly rather than
through a proxy and you expect a collection link to be pasted somewhere busy, put a request limit in
front of it as well — the app's limiter is a guard on one process, not a CDN.

## 15. Backup and restore

**The first line of defence: the database follows the clips into the store** (DESIGN.md §20 errata
343, 348). The store holds bytes without names — every clip's id, title, owner and TTL lives only in
`clippet.db`, which lives in the `app_data` volume and not beside the clips — so the server keeps a copy
of that file in the store itself: in the bucket on `STORAGE_BACKEND=s3`, and on the default stack's
`STORAGE_BACKEND=fs` in the media folder, under its own `_clippet/` (the same names, relative to the
folder's root; the folder's `.clippet-store` marker sits at the root and never meets it):

| object | what |
|---|---|
| `{S3_KEY_PREFIX}_clippet/db.sqlite` | the database, taken the way `admin backup` takes one (`VACUUM INTO`, consistent while the server writes) and checked the way `restore-check` checks one |
| `{S3_KEY_PREFIX}_clippet/db.sqlite.sha256` | the file's sha256, written LAST, so a reader that finds the two agreeing holds a whole copy |
| `{S3_KEY_PREFIX}_clippet/db.sqlite.meta` | `{written_at, commit, clips, bytes, sha256, born}` |

It is **on by default on both backends** (`DB_SNAPSHOT` empty = `on`; `DB_SNAPSHOT=off` turns it off).
On a folder home that is what makes **backing up the media folder the whole backup** (§32.4): until
DESIGN.md §20 erratum 348 a folder home kept no copy at all, and the default install's database had no
backup of any kind while everything suggested the Videos folder was the instance. `admin doctor`'s
`snapshot` row WARNs when an instance has no backup of any kind — the snapshot off and timed backups
(§15.6) off too. It is written once at boot when the store's copy is older
than `DB_SNAPSHOT_EVERY` (10 minutes when empty, 5 s at least) or missing, then after a clip's card
becomes ready or a clip is deleted or expires — at most once per `DB_SNAPSHOT_EVERY`, the last change of a
burst always written — and once a day whatever happened (a new user, a token, a setting is not tracked).
Never on a request: an upload never waits for it and is never refused because of it; a failure is one
`db snapshot failed` WARN and a retry one interval later, and `admin doctor`'s `snapshot` row says so
(§24.1). It is never written over **another database's** snapshot: the meta's `born` is the database's
first-migration time, and a server whose database was born elsewhere — a fresh PC booted against the
bucket before its restore, a second instance on the same `S3_KEY_PREFIX` — refuses to overwrite the copy
that can bring the original back, and says so on every attempt. On a folder home the same refusal is what
keeps a fresh database booted over a restored folder from replacing the copy in it; and a folder with no
`.clippet-store` marker under a database that says it should hold clips — an unmounted share — gets no
marker and no snapshot at all, one ERROR, and a degraded `/readyz` (§32.4).

**The database carries every bcrypt password hash, every session and API token hash and the webhook
secrets, so the bucket's keys ARE the instance** — anyone who can read the bucket can read the copy;
give its keys the care you give the instance's admin password. On a folder home the same is true of the
folder and of every backup of it: `_clippet/db.sqlite` is a credential store.

**The recovery recipe — a new PC, the bucket's keys and one verb, and the links are alive:**

```sh
# 1. a .env with the SAME S3_* (endpoint, bucket, keys, S3_KEY_PREFIX) and PUBLIC_BASE_URL as before
# 2. fetch and check the snapshot (a dry run: nothing is changed)
docker compose run --rm --no-deps app admin restore --from-store
#   snapshot:  _clippet/db.sqlite bytes=1363968 (1.3 MiB) sha256 ok
#   written:   2026-10-19T02:41:07Z (4 min ago) commit=…
#   rows:      users=1 clips=212 jobs=640
#   current:   (none: this machine has no database yet)
# 3. put it in place, then start the server
docker compose run --rm --no-deps app admin restore --from-store --yes
docker compose up -d
```

`--yes` moves a database that is already there (with its `-wal` and `-shm`) aside as
`clippet.db.before-restore-<UTC>` and puts the snapshot in its place; keep the aside copy until the
instance looks right. The verb **refuses while a server runs on that database** — every `clippet serve`
holds `DATA_DIR/serve.lock` shared, the restore takes it exclusive — and refuses a snapshot whose sha256
does not match its sidecar. What the copy cannot hold is whatever changed after its last upload (up to
`DB_SNAPSHOT_EVERY`, or a day for an untracked change); the timed backups below keep a history the one
overwritten copy cannot (a mistake of yours is uploaded within ten minutes).

**On a folder home the recipe is the same verb over the folder.** Put the media folder back from its
backup (or mount the NAS share), bind it as before (`STORAGE_DIR_HOST`, §32.1), and run the two
`admin restore --from-store` lines above: the verb reads `_clippet/db.sqlite` out of the folder, checks
it against its sidecar, and puts it in place — no `.env` keys to carry, because the folder is the whole
instance. Taken while the server was running, a folder backup's snapshot is at most `DB_SNAPSHOT_EVERY`
older than its clips; the clips it does not know yet are orphans `admin orphans --delete` reclaims (§15.3).
The hidden `.clippet-store` file at the folder's root travels with it: a backup tool that skips dotfiles
restores the clips and `_clippet/` but not the marker, and `clippet serve` will not write a new one over a
folder the database says holds clips (§32.4) — so if your copy has none, create it empty before serving
(`touch .clippet-store` in the folder's root).

There are exactly **two** things to back up, and nothing else:

1. **The database** — `clippet.db` in the `app_data` volume. Users, sessions, API tokens, clips,
   tags, collections, jobs and the applied-migration list are all rows in it. It is small (a few
   hundred KiB for a personal instance) and it is the part you cannot regenerate.
2. **The bucket** — every original, rendition, poster, social MP4, GIF preview and `probe.json`,
   all under `{S3_KEY_PREFIX}clips/<id>/`, and the snapshot above under `{S3_KEY_PREFIX}_clippet/`.
   Big, and already durable if it lives at R2/B2; back it up anyway if you care about it, because a
   mistake of yours is not a provider outage.
   **On `STORAGE_BACKEND=fs` (§32) this is `STORAGE_DIR` instead**, and the sentence above about R2/B2
   does not apply: a directory has no versioning and no lifecycle rules, so the whole of its durability
   is whatever you give it. With the snapshot on (the default) the folder carries a copy of the
   database too, so a copy of the folder alone restores the instance (above); §32 says what a restore
   of a mismatched pair looks like, and how `admin orphans` tells you which way it is mismatched.

Not in either: no *key material*. There is nothing to escrow (DESIGN.md §4 has no `SESSION_SECRET` —
sessions are random opaque tokens stored hashed), no cache to warm and no state on the scratch volume
worth a second thought (it is scratch; `app_scratch` is rebuilt on demand). The only other file that
matters is **`.env`**, which is not in git and holds your bucket credentials and, if you set it, the
Discord webhook URL. Copy it wherever you keep passwords; everything else about the deployment is in
this repository.

**The snapshot itself is a secret, though.** "No secret to escrow" is true of key material and false
of the database: `clippet.db` carries every bcrypt password hash, every session token hash and every
API token hash, so a copy of it is a credential store. `admin backup` writes the file `0600` and says
so on its own output; the copy-out below is the step where that can be lost, which is why it ends in
a `chmod`. Keep backups where you keep passwords, and never commit one (`.gitignore` already refuses
`/backups/` for this reason).

### 15.1 The database: `admin backup`

```sh
# 1. take the snapshot inside the container (MSYS_NO_PATHCONV=1 on Git Bash: it passes a container path)
MSYS_NO_PATHCONV=1 docker compose exec -T app clippet admin backup /tmp/clippet-$(date -u +%Y%m%d-%H%M%SZ).db
#   backup:  /tmp/clippet-20260906-093544Z.db bytes=172032 (0.2 MiB)
#   schema:  applied=1,2,3,4
#   source:  /data/clippet.db
#   warning: this file contains password and token hashes - keep it where you keep passwords

# 2. copy it out to the host (the destination directory must exist), then drop the container's copy.
#    `docker compose cp` writes the host file with YOUR umask, not the 0600 the snapshot had, so
#    put it back — this file is a credential store (see the top of §15).
mkdir -p ./backups
MSYS_NO_PATHCONV=1 docker compose cp app:/tmp/clippet-20260906-093544Z.db ./backups/
chmod 600 ./backups/clippet-20260906-093544Z.db          # on Windows: keep ./backups out of anything shared
MSYS_NO_PATHCONV=1 docker compose exec -T app rm /tmp/clippet-20260906-093544Z.db

# 3. check what you just took — read-only, and it needs no configuration at all
MSYS_NO_PATHCONV=1 docker run --rm -v "$(cygpath -w "$PWD/backups")":/backup:ro \
  clippet-app:local admin restore-check /backup/clippet-20260906-093544Z.db
#   file:      /backup/clippet-20260906-093544Z.db bytes=172032 (0.2 MiB)
#   integrity: ok
#   schema:    applied=1,2,3,4
#   rows:      users=75 clips=5 jobs=5
```

Those commands are written for Git Bash; **on Linux drop the `cygpath -w` wrapper** (`-v
"$PWD/backups":/backup:ro`) and leave the `MSYS_NO_PATHCONV=1` prefixes wherever you like — there
they are a no-op environment variable. The same applies to every `cygpath` in §15 and §16.

Facts worth knowing about that:

- **`admin backup` is safe on a running instance.** It is SQLite's `VACUUM INTO`, which runs inside
  a read transaction: the result is the database as of one instant, including everything committed
  to the write-ahead log, and the app keeps serving and writing throughout. Never copy `clippet.db`
  out of the volume with `cp` while the app is up — that file alone is missing whatever is still in
  `clippet.db-wal`, and the pair is not consistent unless you take both at the same instant.
- **It never overwrites.** A destination that already exists is `1` and an error; it writes to
  `<path>.part-<random>` and renames, so an interrupted run cannot leave a half file under the name
  you would restore from.
- **The output is one file** — no `-wal`, no `-shm` — so it copies and compresses like any other.
- **It is written `0600`**, and so is the `.part-` file it is renamed from (the mode survives the
  rename). Every copy you then make is yours to protect: `docker compose cp`, `rclone`, a tarball and
  a git commit all use their own umask or none at all.
- `/tmp` inside the container is writable by the app's user and is discarded when the container is
  recreated, which is why the copy-out is step 2 and not an afterthought. `/data` works too, at the
  cost of keeping backups inside the volume you are backing up.
- `restore-check` is the **only** subcommand that needs no configuration and no live database: it
  opens the named file read-only, runs `PRAGMA integrity_check`, and prints the schema and the row
  counts. `integrity: ok` plus a plausible row count is the whole verdict. A file that is not a
  database at all says so and exits `1`:
  `admin restore-check: db: inspect: integrity_check: file is not a database (26)`.

### 15.2 The bucket

*(This is also step 2 of §33.3's move to R2: the same `rclone` stanza copies the media to a new
bucket, and it never deletes the source.)*

**The media directory** (the default stack, §32) is a folder on the host, so it is backed up the way
any folder is — `rsync`, your NAS's own snapshots, a copy to another disk; §32.4 has what matters about
the order and the marker file. (A stack that still runs the bundled bucket server from before DESIGN.md
§20 erratum 345 copies its clips out with §32.6.)

**R2, B2 or any other S3** is your tool's job, not the app's; the app has no export command and
needs none, because the layout is ordinary objects under one prefix. `rclone` is the usual answer:

```ini
# ~/.config/rclone/rclone.conf — `rclone config` writes this for you; provider names come from it
[clippet]
type = s3
provider = Cloudflare            # Other for B2 and self-hosted servers; `rclone config` lists the current set
endpoint = https://<account-id>.r2.cloudflarestorage.com
region = auto
access_key_id = <same as S3_ACCESS_KEY>
secret_access_key = <same as S3_SECRET_KEY>
```

```sh
rclone sync clippet:<your-bucket> ./backups/bucket --fast-list      # out
rclone sync ./backups/bucket clippet:<your-bucket> --fast-list      # back in
```

Use a **read-only** key for the backup direction if your provider can mint one (R2 can): a backup
job that cannot delete is one fewer way to lose everything.

### 15.3 Order, and what a hot pair can cost you

The two halves are separate snapshots, so unless nothing is changing they are not a consistent
pair. Two facts decide what the gap can do (both are load-bearing elsewhere in the design):

- an upload writes its **objects before** its row (DESIGN.md §8.3 — that is why `admin orphans`
  ignores prefixes younger than `--min-age`);
- a destroy deletes **objects before** the row (§10, DESIGN.md §12 — that is what makes the system
  converge on "no orphaned objects" when it is interrupted).

**The clean way is to quiesce**, and on a personal instance it costs about a minute:

```sh
docker compose stop app        # viewers get a connection error; nothing is lost, the DB is closed
#   … take both halves …
docker compose up -d app
```

**Backing up hot** (no downtime) is fine too, as long as you take the **database first and the
bucket second**. Doing it in that order, anything uploaded while the backup runs ends up as objects
with no row — an orphan, which is harmless and which `clippet admin orphans --delete` reclaims. The
other order breaks the useful case instead: the row is captured but the objects that arrived
moments earlier are not, and you restore a clip that cannot play. Either way the residue is small
and named: run `admin orphans` after any restore and it tells you exactly which prefixes have no
row and which rows are missing objects.

The one thing no ordering fixes: a **trim** or a **GIF** that commits between the two halves writes
new object keys and deletes the superseded ones, so that clip's rendition can be missing from the
pair whichever half you take first. It is one clip, it is reported by `admin orphans` as `missing`,
and the owner can re-trim it. If that matters to you, quiesce.

**Restoring is the reverse order, and it is not negotiable: bucket first, then the database.** The
app must never come up on a row whose objects are not there yet.

**After a host power loss, run `admin orphans`.** The database commits with `synchronous(FULL)`
(DESIGN.md §20 erratum 100), so a committed clip row survives a power cut as well as a crash — but the
row and the bucket are still not one transaction, and a machine that lost power *between* the object
landing and the row committing leaves an object with no row. `admin orphans` is what finds those; it
ignores anything younger than an hour, so run it once the queue is quiet.

### 15.4 The restore, and a rehearsal that costs nothing

This is the restore from a file **you** took (§15.1, §15.6): a file copy while the app is stopped.
The snapshot the store holds has its own verb, `admin restore --from-store` (the top of §15), which
reads only that snapshot and keeps the database it replaces aside; for any other file the copy below
is the restore, so the command that overwrites your live database is one you typed.

```sh
# 0. check the file you are about to trust (§15.1 step 3)
# 1. put the bucket back first (§15.2)
# 2. stop the app; the database is only safe to swap while nothing has it open
docker compose stop app

# 3. replace the file, WITH its -wal/-shm siblings, from inside a throwaway container.
#    The app image runs as its own user, so the copy lands owned by the app (uid 10001)
#    and needs no chown. `clippet_app_data` is the compose project's volume name.
MSYS_NO_PATHCONV=1 docker run --rm --entrypoint sh \
  -v clippet_app_data:/data -v "$(cygpath -w "$PWD/backups")":/backup:ro clippet-app:local \
  -ec 'rm -f /data/clippet.db /data/clippet.db-wal /data/clippet.db-shm
       cp /backup/clippet-20260906-093544Z.db /data/clippet.db'

# 4. bring it back and reconcile
docker compose up -d app
docker compose exec -T app clippet admin stats      # schema/users/clips/jobs + bucket usage
docker compose exec -T app clippet admin orphans    # prefixes with no row, rows with missing objects
```

Deleting `-wal` and `-shm` matters: leaving the old write-ahead log beside a different database is
how you corrupt one. The app recreates both on the next open.

**Rehearse it without touching your instance.** The same copy into a *throwaway* volume, then any
admin subcommand against it, proves the backup is a working live database and not just a readable
file:

```sh
docker volume create clippet-rehearsal
MSYS_NO_PATHCONV=1 docker run --rm --entrypoint sh -v clippet-rehearsal:/data \
  -v "$(cygpath -w "$PWD/backups")":/backup:ro clippet-app:local \
  -ec 'cp /backup/clippet-20260906-093544Z.db /data/clippet.db'
MSYS_NO_PATHCONV=1 docker run --rm -v clippet-rehearsal:/data \
  -e PUBLIC_BASE_URL=http://localhost:8481 -e S3_ENDPOINT=http://localhost:9000 -e S3_BUCKET=clips \
  -e S3_ACCESS_KEY=x -e S3_SECRET_KEY=y clippet-app:local admin stats --no-storage
#   schema:  applied=1,2,3,4
#   users:   75
#   clips:   5 (queued=0 processing=0 ready=5 failed=0 deleting=0)
docker volume rm clippet-rehearsal
```

(The `S3_*` values there are placeholders: `stats --no-storage` never opens the bucket, but
configuration is still validated at start-up, so the variables must be present and well-formed.
What the flag skips is the **bucket listing** at the end of the output; the `storage:` line above it
is a sum of database columns, needs no credentials, and is printed either way — which is exactly
what makes this form usable with placeholder values.)

### 15.5 Retention

Backups age in a way this product makes normal: **clips expire**. A month-old database restored
today carries rows for clips whose TTL has since passed, whose objects a sweeper deleted long ago
and which no bucket copy still holds. That is not corruption. After such a restore:

- `admin orphans` lists them as `missing <id> state=ready key=…` until the sweeper gets to them;
- the sweeper then does the right thing on its own: those rows are past their `expires_at`, so they
  are destroyed within a tick or two, and deleting objects that are already gone is a no-op;
- the reverse is equally harmless: an old bucket copy holds prefixes for clips no row references any
  more, and `admin orphans --delete` reclaims the space.

So keep it simple: a daily database snapshot (it is a few hundred KiB — keep a month of them), and a
bucket mirror as often as you can stomach the transfer. On a personal instance the whole thing is a
cron line and a directory.

> **There is a shorter cron line now.** `admin backup <dir> --rotate N` mints the dated filename
> itself, keeps the newest N and sweeps its own stranded `.part-` files, so the two paragraphs below
> about dating the path and cleaning up by hand stop being your problem — see §24.5. The form below
> still works, and is what to use when you want the snapshot copied straight off the box in the same
> line. And §15.6 needs no cron line at all: the server runs the same command on a timer.

```sh
# crontab, on the docker host. `\%` because cron eats a bare % — and note the LAST command runs
# unconditionally (`;`, not `&&`), while the container path carries the date.
17 4 * * * cd /srv/clippet && D=$(date -u +\%Y\%m\%d); docker compose exec -T app clippet admin backup /tmp/clippet-$D.db \
  && docker compose cp app:/tmp/clippet-$D.db ./backups/ ; docker compose exec -T app sh -c 'rm -f /tmp/clippet-*.db /tmp/*.part-*' 2>&1 | logger -t clippet-backup
```

**Why the date and the unconditional cleanup.** `admin backup` never overwrites (§15.1), so a *fixed*
container path turns one failed copy-out into a permanently failing backup job: the middle
`docker compose cp` fails once — `./backups` is missing, the host disk is full, compose hiccups — the
finished snapshot stays behind under that fixed name, and every night after that exits `1` with
`admin backup: /tmp/db.db already exists (refusing to overwrite)`. Cron discards output by default, so
you find out the night you need the rollback §16 opens with. Dating the path means no run can collide
with another, deleting `/tmp/clippet-*.db` unconditionally means a failed copy-out costs one night
instead of all of them, and `| logger -t clippet-backup` puts the failure somewhere you can grep
(`journalctl -t clippet-backup`) instead of `/dev/null`. The host copy is still yours to protect —
`chmod 600 ./backups/*.db`, or a mode-preserving `umask 077` in the cron environment.

**Sweep the `.part-` files.** `admin backup` writes to `<dst>.part-<id>` and renames it into place
(§15.1), so a run that died while `VACUUM INTO` was writing — a full disk, a killed container —
leaves that partial file behind on purpose: it is never under the name you would restore from, and
deleting it is not the failing command's job. Nothing in the app cleans them up — the cron line above
does it in its last, unconditional command — so an unattended job without that step and a repeated
failure quietly fills whatever directory it writes into. They live beside the
destination path, which for the cron line above is inside the app container, so check for them when
a backup is missing and delete them on sight — they are never a restore candidate (or let
`admin backup <dir> --rotate N` sweep the ones it minted, §24.5):

```sh
MSYS_NO_PATHCONV=1 docker compose exec -T app sh -c 'ls -l /tmp/*.part-* 2>/dev/null; rm -f /tmp/*.part-*'
```

**A restore is a credential rollback too.** The database holds the credentials as well as the clips,
and putting an older copy back undoes everything that happened to them since. Token revocation is a
soft update — `api_tokens.revoked_at` — so a token you revoked after the snapshot was taken comes back
usable, ShareX and all. Logout is a hard `DELETE` of the `sessions` row, but a row restored from a
backup is live again until its own `expires_at`, and `SESSION_TTL` defaults to `720h`: any snapshot
from the last thirty days re-arms every session that was open when it was taken. A password change is
rolled back the same way. **After restoring a backup older than an incident, re-revoke** — and
the CLI is now the tool for it (§19.4), which is exactly the case it was built for:

```sh
docker compose exec -T app clippet admin sessions revoke --all       # every browser, every account
docker compose exec -T app clippet admin tokens list alice           # then, per user
docker compose exec -T app clippet admin tokens revoke alice --all
```

A restored session row is also bounded by its own `max_expires_at` (§19.3), which the restore brings
back unchanged — so a snapshot older than `SESSION_MAX_TTL` re-arms nothing.

Test the restore once, on a throwaway volume, before you need it (§15.4). An untested backup is a
hypothesis.

### 15.6 Timed backups

**Off by default.** Set two variables and the server takes the snapshot itself: §24.5's
`admin backup <dir> --rotate N`, on a timer, with no cron line.

```sh
# .env
BACKUP_EVERY=24h            # how often: a Go duration, 10s at least
BACKUP_DIR=/data/backups    # a directory inside the container
#BACKUP_KEEP=30             # snapshots kept: 30 when unset, 1 at least
```

```sh
# once: the app never creates BACKUP_DIR (the prefix is for Git Bash, as in §15.1)
MSYS_NO_PATHCONV=1 docker compose exec -T app mkdir -p /data/backups
docker compose up -d
```

- **Both, or neither.** One of the two without the other is a boot error. `BACKUP_KEEP` is read
  only when both are set.
- **The database only.** Never the media (§15.2). The file is the credential store the top of §15
  describes, written `0600`.
- **When.** At start-up if the directory holds no snapshot, or its newest is older than
  `BACKUP_EVERY`; otherwise one interval after the newest. A restart does not reset the clock.
- **One log line per run:** `backup written`, `backup skipped`, `backup failed` — or, for a run a
  clean shutdown interrupted, `backup cancelled` (INFO; its `.part-` is removed and the last result
  stands). A failed run is tried again one interval later, and the server keeps serving either way.
- **Skipped** means the directory holds an unfinished snapshot (a `.part-` file) under an hour old:
  a manual `admin backup` into the same directory, or what a failed run left behind. A skipped run
  is tried again when that part turns stale — after `BACKUP_EVERY` or an hour, whichever is
  sooner — so a restart within the hour after a failed run, which is skipped at boot, costs an
  hour, not a whole interval.
- **The directory** must exist and be writable by uid 10001. The app never creates it, so a mount
  that is not there fails every run instead of filling the container. It may be `/data` or under
  it; it may not overlap `SCRATCH_DIR` or `STORAGE_DIR`.
- **`/data/backups` shares a disk with the database:** a rollback point, not a disaster plan. For
  another disk, bind a host directory owned by uid 10001, mode `0700`, into the container and name
  it in `BACKUP_DIR`. Either way, copy the newest file off the box (§15.1).
- **Use a local disk.** While a snapshot is written the server's own database writes wait for it:
  milliseconds on a local disk, and as long as the mount hangs on a mount that hangs.
- `admin doctor` has a `backup` row (§24.1): off, or the settings, whether the directory can be
  written to (one empty temp file, removed at once), the last run and the next.

Restoring is §15.4, unchanged.

## 16. Upgrading

Clippet is one container and one database, so an upgrade is a pull and a restart. There is no
migration command to run by hand and no maintenance window to declare.

**One exception, once: a stack that still runs the bundled MinIO** (three services in `docker compose
ps -a`: `app`, `minio`, `bucket-init`). DESIGN.md §20 erratum 345 withdrew that server, so its clips
have to be copied out before the new `compose.yaml` comes up. Read §32.6 **before** step 1 — it starts
by keeping the compose file you run now.

```sh
# 0. back up first — this is the rollback (§15.1)
MSYS_NO_PATHCONV=1 docker compose exec -T app clippet admin backup /tmp/pre-upgrade.db
MSYS_NO_PATHCONV=1 docker compose cp app:/tmp/pre-upgrade.db ./backups/

# 1. take the new revision: pull, then check out the latest vYYYY.MM.DD tag
git log --oneline -5 && git checkout <revision>

# 2. pull the new image and recreate. Migrations apply during boot, before the listener opens.
docker compose pull && docker compose up -d

# 3. confirm
docker compose ps -a                                   # app (healthy)
curl -sS http://localhost:8481/readyz                   # {"ok":true}
curl -sS http://localhost:8481/healthz                  # "version":"…" ← the release you just pulled
docker compose exec -T app clippet admin stats          # schema: applied=1,2,3,4 ← the new list
```

**Pulling a tag needs no build arguments.** A published image has its version and commit baked in, so
step 3's `/healthz` names the release you pulled. Without `CLIPPET_VERSION` in `.env`, compose pulls
`latest`, the newest release; `CLIPPET_VERSION=2026.11.07` pins that release, and moving the line is the
upgrade.

**Building from source** is the `compose.build.yaml` overlay (`COMPOSE_FILE=compose.yaml:compose.build.yaml`
in `.env`), and there the same two variables are build inputs. The overlay passes `CLIPPET_VERSION` to
the build as `VERSION`, and the Dockerfile bakes it into the binary; with it unset every build reports
`dev`, and then neither `/healthz` nor `clippet version` distinguishes the new container from the old
one. `admin stats`' schema line does not help either — it only moves on a release that carries a
migration. So, **two** variables, in front of the build (`docker compose build --pull`, then
`docker compose up -d`, in place of step 2):

```sh
# both are read at BUILD time, so only a build (or `up --build`) takes them
CLIPPET_VERSION=$(git describe --always --dirty) CLIPPET_COMMIT=$(git rev-parse HEAD) \
  docker compose build --pull
```

and after step 2, `curl -sS http://localhost:8481/healthz` must report exactly that version. `uptime_s`
in the same payload is the other half — a small number means the container really was recreated.

**In `.env` those two are values, not commands.** Compose does not run a shell over `.env`: it reads
`CLIPPET_COMMIT=$(git rev-parse HEAD)` there as the twenty-one characters you typed and hands *those*
to the build. `CLIPPET_VERSION` would then merely report that string back at you, which is visible in
one `curl`; `CLIPPET_COMMIT` is validated on the way out — anything that is not forty hex characters
is published as `null` (API.md) — so it reports nothing at all, and on a release that is published
rather than upgraded in place that is the failure §17.4 step 1 exists to name. So put the
substitution in front of the build, as above, or paste into `.env` what `git rev-parse HEAD` printed:
`CLIPPET_COMMIT=9f3c…`, forty characters, re-pasted whenever you move.

`CLIPPET_COMMIT` is the second half of the same idea and it costs nothing here: `CLIPPET_VERSION` is
what you chose to CALL the release and `CLIPPET_COMMIT` is what it is made OF, and the second is the
one a machine can check. `GET /api/instance` publishes it (API.md), which is how a Clippet Pass name
knows which release is answering (§36) and how you tell two boxes apart that were both built from
"dev". With it unset the field is `null`, which is honest and is all it can be. **Publishing** a
release — an image or a binary other people run — has one more step in a fixed order, and it is
§17.4.

**From source, `--pull`, not just `--build`.** `golang:1.27-alpine` and `alpine:3.22` are moving tags (§17.1):
without `--pull` Docker keeps using whatever it cached the first time, so a rebuild six months later
ships the *first* build's `ca-certificates`, `curl` and Go toolchain — the security patches you are
rebuilding for are exactly what it skips. `docker compose build --pull` re-reads them; it costs a
download and nothing else. `mwader/static-ffmpeg:7.1.1` is pinned to a version, so `--pull` does not
move it: a **patch** bump within 7.1.x (edit the `FROM`, run `./scripts/check.sh`) is routine, while a
minor or major bump is the code change §17.1 describes — goldens, SSIM thresholds and the dual-audio
measurements, plus a full `e2e.sh`.

**Migrations are forward-only and automatic.** Every `*.sql` under `internal/db/migrations/` is
applied in version order at boot, each in its own transaction, and its version is recorded in
`schema_migrations` inside that same transaction; anything already recorded is skipped. `clippet
admin stats` prints the result as `schema: applied=…`, which is how you tell — from the outside —
which schema a running instance is on.

**A migration whose number is BELOW one this database already ran is refused before anything is
applied.** Migrations run in ascending version order, so a file that fills a hole in the numbering —
one written on a branch, or back-ported — would be applied FIRST on a fresh database and LAST on one
that is already up to date: two instances of the same build, two different schemas, and nothing said
out loud. Boot stops instead, with
`db: migration out of order: 0009_x.sql would be applied after 0017, which is already applied;
renumber it above 0017`. The fix is to **renumber the file** above the highest applied version and
rebuild; never delete a row from `schema_migrations` to make room for it — the earlier migration
really did run, and a database that forgets it will try to run it again. Nothing this refusal can
reach exists on a first boot: a fresh database has applied nothing, so every file is ahead of it.

**A migration that fails leaves the database as it was.** The transaction rolls back, the version is
not recorded, `serve` logs `boot: migrate` at ERROR and exits before the HTTP listener is opened, and
compose's `restart: unless-stopped` then retries the same boot on a loop. So the failure mode is a
container that will not stay up and a database you can still restore from — never a half-applied
schema, and never a running app on one.

**A binary you run by hand upgrades the same way, plus its fonts.** Replacing `clippet.exe` (or a
`clippet` you built yourself) replaces nothing beside it: if you put `DejaVuSans.ttf` and
`spline-sans-mono.ttf` in a `fonts` directory next to it, carry that directory over with the binary, or
the new one quietly draws no Light tag and no poster label (§27). Everything else on this page — the
backup, the migrations, the rollback — is the same; only step 2's build is yours.

**Rolling back means restoring the backup** (§15.4), not re-pointing an old image at a migrated
database. Nothing un-migrates. An older binary can usually *read* a newer database — every statement
names its columns and every added column is nullable — but it does not know what the new ones mean.
The concrete example from this codebase: a binary from before the GIF feature would happily trim a
clip and leave `gif_key` pointing at a preview of frames that no longer exist, because clearing it is
code that binary does not have. Treat "downgrade" as "restore the pre-upgrade backup".

**Zero downtime is not a goal.** One container serves everything, `docker compose up -d` replaces
it, and the gap is a few seconds of connection errors, plus the build when you build from source. An
in-flight transcode is interrupted but **not** lost: on `SIGTERM` the worker releases the job back to
the queue without counting an attempt against it (compose allows `stop_grace_period`, 45 s, before
`SIGKILL`), and anything still `running` at the next boot is reconciled — released and re-queued if it
left a checkpoint, counted as a crash otherwise (DESIGN.md §10.1). The clip finishes after the
restart; the uploader sees the progress bar resume.

**Per-release checklist**, in order:

1. `admin backup` + copy it out (§15.1). *This is the rollback.*
2. `docker compose pull && docker compose up -d`. From source: set `CLIPPET_VERSION` (above), then
   `docker compose build --pull && docker compose up -d`.
3. `/healthz` — the `version` you just pulled or set and a small `uptime_s`; then `admin stats` for the new
   `schema: applied=…` and clip counts that match what you had.
4. Open a share page and play the clip; if the instance is public, paste the link somewhere that
   unfurls it. The card is the product, and it is what a bad `PUBLIC_BASE_URL` breaks silently.
5. `admin orphans` — a clean bill after an upgrade means the release did not lose track of objects.
6. If anything above is wrong: `docker compose stop app`, restore (§15.4), check out the old
   revision, set `CLIPPET_VERSION` to the old release, `docker compose up -d` (from source: `up -d --build`).

## 17. Reproducibility and CI

### 17.1 What is pinned

| Pinned in | What | How to bump |
|---|---|---|
| `Dockerfile` | `golang:1.27-alpine` (build + test stages) | edit the `FROM`, run `./scripts/check.sh` |
| `Dockerfile` | `mwader/static-ffmpeg:7.1.1` (the `ffmpeg`/`ffprobe` binaries copied into the test and runtime stages) | edit the `FROM`, then run **the full ffmpeg test suite and `e2e.sh`** — see below |
| `Dockerfile` | `alpine:3.22` (runtime stage) | edit the `FROM`, run `./scripts/check.sh` |
| `compose.tunnel.yaml` / `compose.tunnel-local.yaml` | `cloudflare/cloudflared:2026.8.3` (the ingress; DESIGN §20 erratum 292 — it ran the moving `latest` tag until 2026-09-16, so every `up -d` after a registry push was an unreviewed upgrade of the one process every viewer's bytes cross) | edit the tag in **both** files to the same value, `docker compose up -d` (the tunnel container is recreated; ~10 s of 502 at the edge), then `curl -s https://<host>/readyz`. `scripts/check.sh` refuses any compose file naming the moving tag. The index digest is recorded beside the tag as an anchor; re-read it with `docker buildx imagetools inspect cloudflare/cloudflared:<tag>` |
| `Dockerfile` (`gpu` stage) | `jrottenberg/ffmpeg:7.1-nvidia` (the CUDA-built ffmpeg, a **single amd64 manifest**, one person's repository like `mwader/static-ffmpeg`) | edit the `FROM`, rebuild with `compose.gpu.yaml` selected, then §27's hardware proof (a `/clip` through NVENC, `hwaccel=cuda` on the boot line) — the CPU images and tests do not exercise it |
| `Dockerfile` (`gpu` stage) | `nvidia/cuda:12.6.2-runtime-ubuntu24.04` (the GPU runtime base) | edit the `FROM`, rebuild as above; the driver on the host must be at least what that CUDA minor wants |
| `go.mod` / `go.sum` | every Go dependency, by version **and hash** | `go get` / `go mod tidy` in a Go container with the repo mounted (the host has no Go), then `./scripts/check.sh` |

`go.sum` pins a cryptographic hash for every module in the graph, and the test stage runs **`go mod
verify`**, which re-hashes what the module cache actually holds and fails the build if any of it was
tampered with. Nothing is fetched at runtime: the final image is Alpine plus one static binary, the
two static ffmpeg binaries, `ca-certificates`, `tzdata` and the `curl` the healthcheck uses.

**ffmpeg is the one bump that is a real change.** The transcode goldens, the SSIM thresholds and the
dual-audio channel measurements in `internal/transcode` were measured against 7.1.1, and `/healthz`
reports whatever is in the image. Bumping it is a code change with a test run behind it, not a patch.

**Digests, kept fresh by Dependabot.** Since run 25 (2026-11-06) the three CPU bases — `golang:1.27-alpine`,
`mwader/static-ffmpeg:7.1.1` and `alpine:3.22` — are pinned in the Dockerfile as `tag@sha256:<index digest>`:
the tag says what it is, the multi-arch index digest (amd64 and arm64) is what is pulled, so two builds of
one commit months apart start from the same bytes. The cost of a pin is that a base-image security
rebuild no longer arrives by itself; `.github/dependabot.yml` closes that gap by proposing the new digest
as a pull request each week, which `t0` then tests like any other change. It refreshes digests only — a
new Go, Alpine or ffmpeg version is still a deliberate edit. To re-read a digest by hand:

```sh
docker buildx imagetools inspect alpine:3.22    # the index's Digest: line
#   then, in the Dockerfile:  FROM alpine:3.22@sha256:<64 hex>
```

`compose.yaml` pulls no image of its own since DESIGN.md §20 erratum 345: the default stack is the app,
built from this checkout. (The test harness pins its own bucket server by digest; run 22.)

The two GPU-stage bases stay tags, with their digests recorded **as a comment** since DESIGN §20
erratum 292 (the ffmpeg one is a single amd64 manifest, the one platform the CUDA path has been proven
on); the two tunnel files carry cloudflared's the same way. A recorded digest is a recovery anchor, not
a pin: if a tag is retagged or deleted, it still names the image the build was verified against.
`mwader/static-ffmpeg` is one person's repository, a known risk; the documented
fallback if that image disappears entirely is Alpine's own `ffmpeg` package in the runtime stage, at the
cost of a dynamically linked binary and whatever version Alpine ships; the transcode goldens assume
7.1.1, so that is a change to test, not a swap.

### 17.2 `scripts/check.sh` — the CI story

One command, and on the host only a POSIX shell with `bash` and GNU `grep` (plus `mktemp`), and
Docker — no Go, no Node, no ffmpeg. Everything it builds and tests happens inside containers, which
is why the machine that runs CI needs no toolchain of its own.

```sh
./scripts/check.sh     # a few minutes; run it alone, it is CPU-hungry
```

| Stage | What it proves |
|---|---|
| `compose config` | `compose.yaml` parses and every interpolation resolves |
| `shell syntax` | `bash -n` over `scripts/*.sh` — a typo in the E2E is caught without running it |
| `jget.py selftest` | the E2E's JSON reader agrees with its own test vectors (skipped, loudly, with no `python` ≥ 3.8) |
| the companion mark's files | `web/static/icon-192.png`, `icon-512.png` and `icon-dev-192.png`, `web/static/favicon.ico`, the four Discord icons and `docs/brand/mark.svg`, and the favicon `data:` URI on `base.html` and `embed.html` still match `scripts/lib/mkicons.py` — the PNGs compared as decoded PIXELS so another zlib cannot fail it, the ICO's directory walked and its three PNG entries decoded and compared the same way (never the container bytes), the SVG and the two favicon lines as text (errata 189, 301, 304, 306; skipped the same way with no `python`) |
| no outbound URL literals | no `http(s)://` literal in `internal/`, `cmd/`, `web/` outside tests — the anti-telemetry tripwire (DESIGN.md §20 erratum 27). The scan is line-based and does **not** skip comments: a URL in prose fails it exactly as an egress would, and its self-test plants one in each of the four scanned languages to keep that true |
| one outbound HTTP call site | only `internal/notify` may construct a client, transport, request or dialer (erratum 66) |
| every ffmpeg input is protocol-whitelisted | any non-test file under `internal/transcode` that spells an `-i` of its own must also name a `-protocol_whitelist` (erratum 91) — ffmpeg is the second potential egress path, because a demuxer opens what its input names. A tripwire, not a proof: the guarantee is `inputArgs`/`probeInputArgs` in `internal/transcode/exec.go` and `ParseProbe`'s container allowlist (§15) |
| the product display name is spelled once per package | `internal/httpapi`'s `oembedProvider` and `internal/web`'s `siteName` are the same non-empty string — DESIGN.md §3 forbids an import in either direction, so nothing else can pin them (erratum 234) |
| only `internal/worker` and `internal/db` write `last_error` | the four reason-carrying writers (`FailAttempt`, `FailAttemptAfter`, `RequeueWithBackoff`, `CompleteJob`) are called from nowhere else, so operator jargon cannot reach an owner-facing route (errata 165, 234) |
| no E2E drop-in assigns a harness global | `scripts/e2e.d/*.sh` are *sourced*, so a bare `X=` would clobber a shared clip id (erratum 234) |
| a `serve` probe has a private `SCRATCH_DIR` | any file under `scripts/` that boots `clippet serve` on a private `DATA_DIR` must redirect scratch too, or its boot janitor empties the live one (erratum 234) |
| DEPLOY.md's Contents is complete | one entry per `## N.` heading — the section numbers are stable ids cross-referenced from README, DESIGN and API (erratum 234) |
| the Go stage's planner | `scripts/lib/gocheck.sh --selftest` against planted listings and a stub `go`: by default one test pass and no `-race`; with a race pass no test runs twice except the few whose body reads `raceDetector`; the shards are fixed by the tree (never by `CHECK_JOBS`), so Go's test cache can answer them; no `-count=1` (run 9) |
| the Go stage | `--target testsrc` (`gofmt -l` empty, `go mod verify`), then ONE container of it running `scripts/lib/gocheck.sh`: `go vet` with and without `-tags ffmpeg`, the windows/amd64 cross-compile, and `go test -tags ffmpeg ./...` cut into shards `CHECK_JOBS` at a time, with the named volume `clippet-gobuild` as Go's build and test cache so an unchanged package answers `(cached)`. No `-race` by default: `CHECK_RACE=1` adds it for the packages that own goroutines, `CHECK_RACE=all` for every package (T2 and the nightly). Skipped when this Go tree already passed with a race pass that covers the request (`CHECK_FULL=1` runs it anyway). Errata 41, 283; run 9 |
| `docker build` | the runtime image builds and is the image compose runs |

It exits non-zero on the first failure and prints `[check] OK` at the end. In a CI runner it needs:
a Docker daemon, outbound network for the base images (including `docker/dockerfile:1`, which
BuildKit resolves for the `# syntax=` line — a Docker Hub blip fails the build with
`no such host`; re-run), a shell with `bash` (the `shell syntax` stage runs `bash -n`), GNU `grep`
(the grep-based tripwire stages use `--include` / `--exclude`) and `mktemp -d` — **every** tripwire
stage carries a planted-file self-test, so a broken expression fails loudly instead of passing
vacuously — a bare `alpine`/BusyBox runner fails at the second stage — and nothing else. A GitHub
Actions job is `actions/checkout` plus `./scripts/check.sh`, and that is what the workflows under
`.github/workflows/` run: the CI story is the script, and the provider's YAML only invokes it.

### 17.3 `scripts/e2e.sh` — the release gate

`check.sh` proves the code; `e2e.sh` proves the **deployment**. It needs a stack that is already up
and drives it as a client for 100 steps (7 in the script itself — the setup 1–4, 6, 16, 17 — the rest in the forty-one
`scripts/e2e.d/` drop-in bundles it runs): registration and login, the five-clip transcode matrix
(720p30, no-audio, vertical, 60 fps, dual-audio — every one of which found a real bug once), the
full card-tag sweep fetched back with Discordbot and Twitterbot User-Agents (favicon, the theme-color
pair plus the card bar, self-hosted faces, `og:site_name` — §28, erratum 287), media `200/206/304/416`
behaviour, owner-only mutations and CSRF, the capture-tool upload API, TTL keep-alive with a forced
expiry actually swept, trim, GIF, embed parameters, tags and collections — then it cleans up after
itself, at both ends of the run: its own clips, its own collections (`admin collections rm`, §24) and
its own accounts, and an interrupted run's leftovers are cleared by the next run rather than by hand.

```sh
docker compose up -d
BASE=https://<your public origin> bash scripts/e2e.sh        # ends in E2E PASS
```

In this repository's own release process the whole run is one stage of **T2**, `bash scripts/gate.sh`,
on a local stack of the gate's own (§17.4 step 1); the public origin is then checked by the two-minute
smoke, `scripts/smoke-public.sh` (T3), after the deploy (DESIGN.md §20 erratum 331). Running it against
your own public origin by hand, as above, is still the most complete proof a self-hoster can give one.

**About six and a half minutes** — 384 s for one full run on a lane stack in run 9 (`serial setup 45 |
pool 324 | exclusive 15`, 6 slots, `APP_CPUS=4`, no gpu flavour; DESIGN.md §20 erratum 331), longer on
a slow encoder, a contended ffmpeg slot or a public origin. The run ends with the ten slowest steps and a
line per bundle, so a run that took longer than usual says where it went.

Since §20 erratum 245 the bundles are **scheduled** rather than sourced one after another: each declares
in its own header whether it may run beside another (`# e2e-schedule: parallel` / `# e2e-schedule:
exclusive`, and no marker means exclusive), and may name the bundles it must never overlap
(`# e2e-schedule-apart:`) or must follow (`# e2e-schedule-after:`). **Forty** of the forty-one run in
the pool, `E2E_LANES` at a time, longest first; the one exclusive bundle, `87-storage`, runs alone
afterwards because it measures the bucket as a whole. The command does not change;
`scripts/e2e.d/README.md` is the rule for adding one. Three knobs shape a run and none of them belongs
in a gate:

| knob | default | what it does |
|---|---|---|
| `E2E_ONLY=<stems>` | *(off)* | run only those bundles (`E2E_ONLY=70-web,77-design`) on a minimal setup — one bundle in about 90 s instead of the whole run. It prints `E2E PASS (PARTIAL)` and lists what it did NOT run, because a truncated run is not a gate. |
| `E2E_SKIP=<stems>` | *(off)* | the whole suite minus those bundles. |
| `E2E_LANES=N` | half the CPUs, 2–6 | how many bundles run at once. `E2E_LANES=1` is one at a time. |

Three more knobs override the app's own settings for the length of the run, by the same mechanism the
raised upload limiters have used since Phase 2 (a shell variable compose interpolates, dropped again by
step 17): `E2E_SWEEP_INTERVAL` (default `15s` for a run — the shipped default is 60s and three steps
each wait for a TICK), `E2E_RATE_AUTH_PER_MIN` (default 120, shipped 10 — four bundles logging in inside
one minute would otherwise be refused) and the `E2E_RATE_UPLOAD_PER_HOUR*` pair. Nothing in the suite
asserts any of those ceilings; the two limiters it DOES probe are compile-time constants no environment
can move.

`E2E_APP_CPUS` is the fourth, and it overrides a COMPOSE knob rather than an app setting: it sets the app
container's `cpus` quota for the run, so a lane or throwaway stack can be timed at the cores its box has
without hand-editing that checkout's `.env`. Unset — the default — `.env` decides exactly as before.
**It must not exceed the host's `nproc`** (§7: a `cpus` above the core count is a hard container-create
error, not a warning), and step 1 prints the quota compose resolved either way, because a run's timings
mean nothing without it.

- **`BASE` must equal `PUBLIC_BASE_URL`** (§6). Over an https base the run adds the assertions that
  matter for X: `Secure` cookies, `og:video:secure_url`, and every card URL fetched back from the
  public origin by a crawler User-Agent.
- **Three runs in one hour will trip `RATE_UPLOAD_PER_HOUR_PER_IP`** (§7). One run makes ~50
  rate-limited POSTs (uploads, trims, GIF regenerations) and each run registers a *new* user, so the
  per-user budget resets while the 120-per-address one does not — it refills at 2 a minute. Two
  back-to-back runs are fine; if you are gating in a tight fix-and-re-run loop, put
  `RATE_UPLOAD_PER_HOUR_PER_IP=400` in `.env` and `docker compose up -d` first.
- It needs host `curl`, `python` ≥ 3.8, `bash` and ffmpeg/ffprobe **7.1.1** (it generates its own
  test clips and probes the results), plus `docker` — it drives the admin CLI and counts bucket
  objects through `docker compose` at the same time as it drives the origin over HTTP. Run it on the
  machine the stack is on. No `jq` and no `sqlite3` are needed anywhere.
- Set `E2E_DISCORD_WEBHOOK_URL` to add the positive half of the Discord check (§11); without it that
  step still runs, in reverse, asserting nothing was queued and nobody was contacted.

### 17.4 Publishing a release

Releases are published by the maintainer; a self-hoster upgrades per §16 and needs none of this. Each
release tag publishes `ghcr.io/clippet-gg/clippet:<version>` and moves `latest`. The
runbook — the release manifest first, then the artefact — lives with the hosted plane's own documentation.

## 19. Accounts, sessions and tokens

Three credentials can act as a user, and this section is how each one is created, seen and taken
away: the **password**, the **session cookie** a browser holds, and an **API token** a capture or
backup tool holds. There is still no password reset and no admin web panel — the CLI below is the
panel, and it runs inside the app container against the same SQLite file as everything else.

### 19.1 Who may register — `REGISTRATION`

`REGISTRATION` has five values; the default is `first`.

| Value | Who may create an account |
|---|---|
| `first` | **The default.** Only while the `users` table is *empty*. The first person to reach `/register` becomes the account holder and the door closes behind them. |
| `open` | Anybody who can reach the origin, forever. |
| `closed` | Nobody. Accounts are added with `admin users create` (§19.4). |
| `members` | Any member of a server bound to this instance (§30.3), through the bot's `/signin` (§30.12). `/register` is closed exactly as under `closed`. |
| `role:<role id>` | A member of a bound server who holds that ONE role, through `/signin`. `/register` is closed. |

An operator who opens registration to the public (`open`, or a Discord mode on a public server) decides who may sign up,
and the rules on children's data (COPPA in the US, the GDPR's age of consent in the EU) look at that operator; the form
itself collects a username and a password and nothing else (§38).

The two Discord modes need the bot (§30.2; on the official bot, §30.8). A member's first `/signin`
makes their account, named after their Discord username and linked to their Discord id, with no
password; every later `/signin` opens the same account. Neither mode changes who may *log in* with a
password. The role id is a number, not a name: in Discord, User Settings → Advanced → **Developer Mode**
on, then Server Settings → Roles → right-click the role → **Copy Role ID**, and write
`REGISTRATION=role:123456789012345678`. A value after `role:` that is not 15-25 digits is a boot error.

`first` exists because the other two are each wrong for a fresh install, in opposite directions.
§3 brings a public Cloudflare tunnel up in one `docker run`, and with `open` the moment that URL
answers, anyone who finds the hostname has an account that can list every clip's metadata and upload
4 GiB. With `closed`, a fresh checkout ships a service you cannot get into. `first` gives you the
sixty seconds you need and then shuts.

Practically:

```sh
# a fresh stack: bring it up, open http://localhost:8481/register, make YOUR account.
# the second person to try that page gets "registration is closed".

# friends are signing up this evening:
#   .env:  REGISTRATION=open
docker compose up -d          # configuration is read once at boot; every flip is an `up -d`
#   …and put it back to `first` (or `closed`) afterwards.
```

The effective mode is on the boot line, so `docker compose logs app | head -1` always answers "is
this thing open?":

```
level=INFO msg=boot version=… public_base_url=https://clips.example registration=first
```

Every successful registration is logged with the account name and the **resolved** client address
(§4's `TRUSTED_PROXIES` decides what "resolved" means behind the tunnel):

```
level=INFO msg=http method=POST path=/api/auth/register status=201 registered=alice client_ip=203.0.113.9
```

Under `members` and `role:<id>` the boot line says so (`registration=members`), `/login` carries one
line under the form — `Members of the connected server: /signin in Discord.` — and `admin doctor`'s
`login` row names the mode (a warning if the bot is off: nobody new can get in that way).

> **`scripts/e2e.sh` registers throwaway users**, so a stack you run the E2E against needs
> `REGISTRATION=open` in `.env`. Step 1 checks and stops with that sentence rather than failing later
> with a bare 403.

### 19.2 Changing a password — `/settings`

The nav item is **Settings** (the old `/settings/tokens` URL still answers; it redirects to the
tokens section). The page has three sections, all for the account you are logged in as:

1. **Change password.** Current password, then the new one (8–72 bytes). It signs out every *other*
   browser and device at once, and leaves the one you are using signed in. That is the answer to "my
   laptop was stolen": borrow any other device, log in, change the password, and the stolen cookie
   stops working on its next request.
2. **Signed-in browsers.** Every live session: when it started, when it was last seen, when it
   expires, and which row is the browser you are reading it on. There is no per-session revoke and
   no session id anywhere in the page or the API — the button is **Sign out everywhere else**, which
   is the affordance an incident actually needs. API tokens are *not* affected by it; revoke those in
   the third section or with the CLI.
3. **API tokens** (§9), now with a scope picker — see §19.5.

A password change never touches anyone else's account, and there is still no reset flow: if a user
forgets their password, you set a new one for them by deleting the account (`admin users prune`, §8,
only for a user who owns nothing) or by handing them a fresh one — there is no "email me a link",
because there is no email.

### 19.3 How long a session lives — `SESSION_TTL` and `SESSION_MAX_TTL`

A session **slides**: using it pushes its expiry `SESSION_TTL` (720h = 30 days) into the future, at
most once a minute. It also has a **ceiling**: `SESSION_MAX_TTL` (2160h = 90 days) measured from the
moment the session was created, which no amount of use can move. So a browser you use daily stays
signed in for ninety days and then asks for the password again — and, more to the point, a cookie
somebody else took keeps working for at most ninety days rather than forever.

```sh
# a shared or public machine is in the picture: make sessions short
SESSION_TTL=24h
SESSION_MAX_TTL=168h        # a week, whatever the traffic
```

Setting `SESSION_MAX_TTL` *below* `SESSION_TTL` is legal and clamps everything to the lower value,
including brand-new sessions; the cookie is issued with the shorter life so the browser and the
database agree. Existing sessions get their ceiling from the migration (ninety days from when they
were created), so anything already older than that ends at its next request.

### 19.4 The operator's side — `clippet admin`

Make your own account before you open registration or `/signin` (§19.1, §30.12): the first account on the
instance is its owner whoever made it (§35.1), and `admin users owner <name>` is the way back.

```sh
docker compose exec -T app clippet admin users                       # every account, with what it owns; owner=yes marks the owner
docker compose exec -T app clippet admin users owner alice           # make alice the instance's owner (/setup, the Storage panel)
docker compose exec -T app clippet admin sessions list alice         # alice's live browsers
docker compose exec -T app clippet admin sessions revoke alice       # sign alice out everywhere
docker compose exec -T app clippet admin sessions revoke --all       # sign EVERYBODY out
docker compose exec -T app clippet admin tokens list alice           # alice's live API tokens
docker compose exec -T app clippet admin tokens mint alice           # a capture token, raw value on stdout, once
docker compose exec -T app clippet admin tokens revoke alice <tid>   # one token
docker compose exec -T app clippet admin tokens revoke alice --all   # all of alice's tokens
```

Every one of these needs an **explicit selector**, and none of them has a default that means
"everything" — the same rule `admin users prune` follows (§8):

- `sessions revoke` with no argument is a usage error, and so is a username *together with* `--all`.
  Signing the whole instance out is a thing you type on purpose.
- `tokens revoke alice` on its own is a usage error too: you must name a token id or say `--all`.
  "Revoke alice's tokens" is a sentence you can type while meaning "show me alice's tokens".
- A token that is unknown, already revoked, or somebody else's gives one answer — `no such live
  token` — and exit 1. The lists print each token's `prefix` (`ck_a1b2c3d4…`), never the token
  itself, and never a session's cookie or its hash.

**Creating an account from the CLI** is how you add people under `first` and `closed`. The password
is read from **stdin**, never from the command line, because a command line ends up in your shell
history, in `ps` output, and in the docker daemon's own logs:

```sh
read -rsp 'password for alice: ' PW; echo
printf '%s' "$PW" | docker compose exec -T app clippet admin users create alice
unset PW
```

The rules are the API's: 3–32 characters of letters, digits or underscore for the name, 8–72 bytes
for the password. The command prints the new row and exits 0; a name that is taken exits 1.

**Setting a password from the CLI** is the recovery verb — somebody forgot theirs, or an account
`/signin` made (§30.12) has none and its owner wants one. The new password comes over stdin exactly
as above, and the account is signed out of **every** browser in the same step, as a password change
does (§19.2; there is no browser of yours to keep):

```sh
read -rsp 'new password for alice: ' PW; echo
printf '%s' "$PW" | docker compose exec -T app clippet admin users password alice
unset PW
#   password set for 01H…  alice; signed out of 2 browser(s)
```

The password rule is the same 8–72 bytes; an unknown name exits 1. The Discord link, the clips and the
API tokens are untouched.

**Minting a token from the CLI** is the same idea for a credential: `POST /api/auth/tokens` needs a
browser session, and setting up somebody else's capture tool — or a box whose browser cannot reach this
instance — has none. `tokens mint <user> [--name n] [--scopes upload[,read]] [--sxcu]` writes the raw
token, or the whole ShareX file, to **stdout** and every other word to stderr, so
`… tokens mint alice --sxcu > clippet.sxcu` is an importable uploader and nothing else lands in the file
(DEPLOY §9 has the worked example). The value is shown once, exactly as `/settings` shows it once; the
row it makes is an ordinary one that `tokens list` and `tokens revoke` above see like any other, and
`--sxcu` is refused for a token without the `upload` scope rather than writing a destination that would
answer 403 on the first capture (§20 erratum 308).

Revoking a session or a token takes effect on the credential's **next request** — there is no cache
in front of it. A revoked session's browser gets a 401 and lands back on the login page; a revoked
token's tool gets `401 unauthorized`.

### 19.5 Token scopes — `upload` and `read`

A token is minted at `/settings` with checkboxes for what it may do:

| Scope | What it can reach |
|---|---|
| `upload` | `POST /api/upload` and `POST /api/clips` — the capture-tool credential (§9). |
| `read` | `GET /api/clips` and `GET /api/collections` — the same library listing a browser session gets: every clip **listed** on this instance (whoever owns it) plus your own unlisted ones, with your own view counts on your own rows. `GET /api/collections` is your own collections only. |

Both scopes reach `GET /api/auth/me`, and **neither reaches anything else**. A token cannot change
your password, cannot sign anything out, cannot mint another token, cannot edit, delete, retry or
trim a clip, and cannot download an original. Those all answer `403 token_not_allowed` and always
have.

An **inbound URL** (§9) is a third kind of row in the same list, minted with the scope `inbound` on its own. It is not a Bearer token: it reaches `POST /in/{token}` and nothing else — not `GET /api/auth/me` either. A **Clippet Auto URL** (§9) is a fourth, minted with the scope `autofetch` on its own; it reaches `POST /in/auto/{token}` and nothing else.

`read` is the scope for automation that has no business uploading — a backup script, a "sync my
library to a NAS" cron:

```sh
# the library listing, newest first, as JSON
curl -sS -H "Authorization: Bearer ck_…" https://clips.example/api/clips?limit=100
```

> **`read` is not narrower than a browser session — it is exactly as wide.** `GET /api/clips` is *the
> library listing*, and it answers with every clip **listed** on this instance, whoever owns it, plus
> your own unlisted ones. Owner-private fields (`views`, source duration, trim state) come back only
> on your own rows, and every owner action and owner-only read — `PATCH`, `DELETE`, `retry`, `touch`,
> `/original`, `…/trim` — still answers `403 token_not_allowed` for anybody's clip, yours included.
> `GET /api/collections` really is your own collections and nothing else. On a household instance that
> is the same reach the account already has in a browser; before handing a `read` token to a
> **third-party** service, note that it can enumerate the share ids — and therefore open the watch
> pages — of every listed clip on the instance, not only yours.

A token minted **without** `upload` gets no `clippet.sxcu` file, because a ShareX destination built
on it would import cleanly and then fail on the first screenshot. Tick `Upload` for a capture tool;
tick `Read` for a script; tick both if one credential really does do both jobs, knowing that it is
then one paste away from being both problems.

Existing tokens are unchanged: a token minted before this release has the `upload` scope and behaves
exactly as it did.

### 19.6 Sign in with Discord — a second key to an account, never a door

> **The door is `/signin` (§30.12; DESIGN §20 erratum 341).** Since then a member of a bound server signs
> in with the bot's own signed interaction — no application of yours beyond the bot, no OAuth, nothing
> through Clippet's plane — and `REGISTRATION=members` or `role:<id>` (§19.1) lets their first `/signin`
> make their account. The button below is unchanged and is still only a KEY: it opens an account that
> is already linked, under every mode, and is what a self-host that wants a button on `/login` sets up.

Optional, off by default, and **a different feature from both the webhook (§11) and the bot (§30)**: a
**Sign in with Discord** button on `/login` that opens an account somebody has already **linked**. It
never creates an account — under `first`, `open` or `closed` alike — so it changes nothing about who may
be on your instance: a Discord account is not an account here until you, or the account's owner, say so.

**What to create in Discord (about two minutes).** If you already run your **own** bot application
(§30.1), you have one: the OAuth2 client id **is** the application id, so `DISCORD_LOGIN_CLIENT_ID` is
your `DISCORD_APP_ID` and the secret is that application's. On the **official** bot (§30.8, `managed`)
the application is ours and its secret is not yours, so make a small one of your own:

1. **Developer Portal → Applications → New Application** ("Clippet login", or reuse your own bot's).
2. **OAuth2 → Client information**: copy the **CLIENT ID**. **Reset Secret → Copy**: this one **is** a
   secret and is shown once.
3. **OAuth2 → Redirects → Add Redirect**: exactly `<PUBLIC_BASE_URL>/api/auth/discord/callback` — for
   example `https://clips.example/api/auth/discord/callback`. Discord compares it byte for byte;
   `clippet admin doctor` prints the string to paste. Discord accepts a plain-http redirect only on
   `localhost`, so a LAN address or a quick tunnel needs the https origin here.

No scope, no bot user, no permission: the flow asks for `identify` and nothing else, and the instance
learns exactly one fact from it — which Discord user consented.

```sh
DISCORD_LOGIN_CLIENT_ID=123456789012345678      # OAuth2 → CLIENT ID (an own-bot instance: = DISCORD_APP_ID)
DISCORD_LOGIN_CLIENT_SECRET=<the client secret> # OAuth2 → Reset Secret (a SECRET: .env is gitignored)
```

**Both or none.** One without the other is a boot error that names both. Then `docker compose up -d`;
the boot line is `discord login configured client_id=… redirect_uri=…` and never the secret, and
`admin doctor`'s `login` row (§24.1) says the same, with a warning if the redirect URI is one Discord
would refuse.

**Linking, and who may.** Two hands, both explicit:

- **The account's owner**, signed in, on `/settings` → **Discord** panel: re-enter the **current
  password**, click **Link Discord**, consent at Discord, land back on Settings as `linked as
  <discord user> · since <date>`. The password is asked for the same reason a password change asks
  for it (§19.2): a browser that was logged in once is not proof that it is still the owner, and a
  permanent login key must not be minted from an unlocked laptop.
- **You**, from the CLI, for somebody who cannot or should not log in to do it:

  ```sh
  docker compose exec -T app clippet admin users link alice 111111111111111111   # Discord USER id
  docker compose exec -T app clippet admin users unlink alice
  docker compose exec -T app clippet admin users                                  # …discord=<id|->
  ```

  (The id: Discord → Settings → Advanced → **Developer Mode**, right-click the user → **Copy User ID**.)

One Discord account opens **one** account here, and an account holds **one** Discord: linking an id
that is already linked elsewhere is refused (the CLI names the other account; the page never does),
and linking a second one on an account that has one is refused until you unlink — one explicit change
at a time, no silent replacement. A username match is never a link; the id is the key and the name
shown is display only, refreshed each time that person signs in with Discord.

**Unlinking** takes no password (it removes a way in) and signs nobody out. **Changing the password**
does not unlink — they are different keys — and its "sign out every other browser" reaches a browser
that signed in with Discord exactly as it reaches any other, because it holds an ordinary session.

**What a Discord login is, on the wire.** The button starts an OAuth code flow with `state` and PKCE
held in one short-lived browser cookie (ten minutes, the session cookie's flags and its `__Host-`
prefix when you have one); the callback checks the state, exchanges the code with Discord from the
server, reads the user's id, and issues **the same session a password login issues** — same cookie,
same lifetime (§19.3), same row in **Signed-in browsers**. Nothing is kept from Discord: no access
token, no refresh token, no email. The cookie is signed with a key derived from your client secret, so
a browser that merely holds a session cannot build the link-mode cookie the password step mints — the
password really is required to add the second key. A callback that is replayed, stale, carries the wrong
state or carries a cookie this instance did not mint is refused before anything is sent to Discord and
lands on `/login` with one line saying so.

**If it does not work.** *"Invalid OAuth2 redirect_uri"* at Discord's own page: the redirect in the
portal is not byte-for-byte what `admin doctor` prints (scheme, host, port, path). *"No account here
is linked to that Discord"*: nobody has linked that Discord user yet — log in with the password and
link it in Settings, or `admin users link`. *"Discord did not answer"*: the exchange failed — the app
log has the reason (`discord login: the exchange failed`, the secret redacted), most often a reset
secret that was not copied into `.env`. The password always still works.

**What it is not.** Not a way to register (there is no account-creating mode, on purpose: an account
with no password has no recovery story yet), not global users across instances, and not the bot's
guild binding (§30.3) — a bound server and a linked person are different facts, and an `/upload` in a
bound server still lands in the bound account's library whoever ran it.

## 20. The library

The library is the page at `/` for a signed-in account, and the route behind it, `GET /api/clips`.
(A visitor with no session gets the front door at `/` instead — the wordmark, one line and a way in;
§28.) This section is everything that decides what the library shows and what you can do to twenty
clips at once.

### 20.1 Who sees whose clips — `LIBRARY_SCOPE`

| `LIBRARY_SCOPE` | The library and `GET /api/clips` show |
|---|---|
| `shared` (default) | every account's **listed** clips, plus the caller's own unlisted ones |
| `own` | the caller's **own** clips only, listed and unlisted |

`shared` is what every version before this one did, and it is the default so that setting nothing
changes nothing. Set `own` when the instance is for several people who are not one household:

```sh
echo 'LIBRARY_SCOPE=own' >> .env
docker compose up -d          # configuration is read once at boot
```

**It narrows a list, and nothing else.** A share link still works for everyone who has it — the
unguessable id has always been the authentication (DESIGN §7; twelve base62 characters, or three to
four words if the upload asked for them — §29) — so `own` does *not* mean "private clips":

- `GET /api/clips/{id}`, `/{id}`, `/embed/{id}`, `/c/{id}` and every media URL answer exactly
  as before, for anybody holding the link. If you want a clip not to be *surfaced*, that is what
  *Unlisted* is for, and it works under both values.
- The Discord auto-post (§11) is unchanged: one webhook is one channel, and it still posts every
  user's listed clips there. Per-user or per-tag routing is a separate feature that does not exist.
- Ownership is unchanged: nobody could ever edit or delete somebody else's clip, under either value.

What you get is the thing people actually ask for — "I do not want my job-hunting clip in my
friends' library, and I do not want to remember to tick *unlisted* every time".

The library page says so, rather than looking mysteriously short: with `own` in force it renders
*"This library shows your own clips only."* and drops the *Mine only* checkbox, which would be a
toggle you could not turn off.

Flipping the variable back to `shared` is safe and immediate — it changes one SQL predicate and
touches no data. Page links that a reader is holding keep working across the change.

### 20.2 Finding a clip: search, tags and "mine only"

The library carries three filters, and they compose. All three narrow the same set `LIBRARY_SCOPE`
defines; none of them can widen it, so no filter has ever shown anybody a clip they could not
already see.

| Control | URL | What it matches |
|---|---|---|
| Search box | `/?q=clutch` | a substring of the **title or the description**, case-insensitive for ASCII |
| A tag chip | `/?tag=gaming` | clips carrying that tag (§14) |
| *Mine only* | `/?mine=1` | the caller's own clips |

The same three work on the API: `GET /api/clips?q=clutch&tag=gaming&mine=1&limit=50`.

Notes worth having before you need them:

- **The search box is a plain form.** It works with JavaScript switched off, the URL it produces is
  bookmarkable, and the back button means what it says.
- **Ticking *Mine only* searches on its own** — no second click on **Search**. That is the browser
  submitting the same plain form, so the URL, the bookmark and the back button are exactly as they
  were, and a browser with no JavaScript still uses the button.
- **`q` is at most 64 characters** and must be text; anything longer, or carrying a control
  character, is `400 invalid_request` on the API and a bounce to the unfiltered library on the page.
  The search box stops at 64 too, so only a hand-typed URL or an API client ever meets that bounce.
  `%` and `_` are ordinary characters in a search, not wildcards.
- **Case-insensitivity is ASCII-only.** `CLUTCH` finds `clutch`; `É` does not find `é`. That is
  SQLite's `lower()`, and matching more of Unicode would mean silently merging words that only look
  the same (DESIGN §20 erratum 114).
- **`mine=1` is the only value `mine` takes.** There is no way to list another account's clips —
  that would be a user page, which this product deliberately does not have.
- **There is no index behind the search, on purpose.** A `%needle%` match cannot use one in any
  database. It is a scan of the `clips` table alone, bounded by the page size, and at the scale a
  self-hosted instance runs at (tens of thousands of rows) it is not measurable. If your library ever
  gets big enough for that to be false, the honest fix is a full-text index, not a bigger `LIKE`.
- **Page links carry their filters.** The *Older clips* link keeps whatever is in force, because the
  page token is minted under those filters; spending one under a different filter answers
  `400 invalid_cursor` rather than silently skipping rows.

### 20.3 Doing something to twenty clips at once

Tick the checkboxes on the rows you own and the action bar **appears** above the list: **Add tag**,
**Delete after** (a lifetime, with its own **Set**), **Add to collection**, and **Delete** (the red
one, which destroys the selection). It arrives with the first tick and goes away
again when nothing is selected — a bar of buttons about "the selected clips" has nothing to say while
none is selected — and it stays a moment longer when it has a result to show you (a bulk delete
removes the very rows that were the selection, and the summary of what happened must not vanish with
them). *select all* lives in that bar, so it is one tick away rather than zero. Each button is ONE
request (`POST /api/clips/bulk`), and each answers per clip.

- **Add tag** adds one tag to each selection and leaves the tags they already carry alone. (The
  editor on a clip's own page replaces the whole set — that is a different verb on purpose.)
- **Delete after** (the select and its **Set** button) applies the same TTL to the selection, using
  the same five values as an upload: 1, 7, 30, 90 days, never. It rests on *no change*, and **Set**
  sends nothing until you pick one.
- **Add to collection** appends to the end of a collection you own, without disturbing the order of
  what is already in it.
- **Delete** asks twice. The clips go immediately as far as every reader is concerned; on a large
  selection the objects in the bucket may be finished by the next sweeper tick (≤ `SWEEP_INTERVAL`)
  rather than inline — `admin stats` will show the bytes until then.

Two rules to know:

1. **One id going wrong does not undo the rest.** A selection that includes somebody else's clip
   deletes the rest and reports `403` for that one; an id that is already gone reports `404`. The
   status line tells you how many landed and why the others did not.
2. **At most 100 clips per request**, which is the library's page size — the action is "the rows I
   ticked on this page".

Without JavaScript the checkboxes and the action bar do not appear at all, and every one of these
actions is still available one clip at a time on that clip's own page.

`curl`, if you want it in a script — session cookie only, exactly like every other owner mutation
(an API token is an upload credential and gets `403 token_not_allowed`):

```sh
curl -sS -b jar.txt -H 'Content-Type: application/json' \
  -d '{"action":"tag","tag":"lan-party","ids":["aZ3kQ9xB2mLp","Bc7dEf1gH2iJ"]}' \
  http://localhost:8481/api/clips/bulk
# → {"action":"tag","ok":2,"failed":0,"results":[{"id":"aZ3kQ9xB2mLp","status":200,"error":null},…]}
```

### 20.4 What the rows tell you, and what the page costs

A ready row shows the clip's **duration** over its thumbnail and its **resolution** beside the date —
both measured at transcode time, both already on the clip's own page. A row that is still encoding
shows neither, because they do not exist yet, and a progress bar instead.

That bar now says **what** is happening, in two or three words: *queued* (as `Queued…`), *reading the
upload*, *inspecting the video*, *repacking the upload*, *encoding the card copy*, *encoding on the
server*, *checking the result*, *making the thumbnail*, *uploading to storage*, *finishing up*. The
two middle ones belong to the quality tiers (§27): *repacking* is the `-c copy` container rewrite of a
web-playable upload, where no frame is re-encoded, and *encoding the card copy* is the small file a
Discord card fetches — between them, most of what a modern upload's wait is spent on. It is the stage of the clip's own transcode job,
and it answers the question the striped bar could not: the work is ffmpeg **inside the app
container**, one encode at a time by default (`TRANSCODE_CONCURRENCY`, §7), and "uploading to
storage" is the last of it — that is your bucket, not the browser's upload, which finished before the
row existed. The phrase is rendered by the SERVER, so it is there with JavaScript switched off, and
one query serves the whole page.

Those progress bars are **one** request for the whole page, not one per row
(`GET /api/clips/progress?ids=…`, up to 100 ids). A ShareX watch-folder drop of forty files used to
make one open browser tab issue about forty requests a second at a box that was also running ffmpeg;
it is now one a second while anything is unfinished, and none at all when everything is done or the
tab is in the background. Nothing needs configuring for that.

## 21. The social rendition and bitrate

> **Since §27 (quality tiers) this file is the LIGHT tier**, and it is served at
> `/api/clips/<id>/light.mp4` as well as at the `/social.mp4` URL below (which is still what every card
> tag points at, and still works). Everything in this section is unchanged except the rule for WHEN one
> is made: a file larger than 1280×720 gets a card copy too, whatever its size, because the file a card
> would otherwise be handed is no longer capped at 720p. `SOCIAL_MAX_BYTES` keeps its name. §27 is the
> section to read for what the other two tiers are and what each costs. One operational note belongs
> here: the card copy is encoded FROM the clip's playable file, which on the fast path is the upload
> itself, so `SCRATCH_DIR` needs room for the largest upload you accept plus `SCRATCH_MIN_FREE_BYTES`
> for this job too — not only for a transcode (§1, §13, §27). And one more thing is true of this file
> alone: it carries a small semi-transparent corner line naming the preview quality and the real one
> (`LIGHT_TAG_TEXT`, §27). Nothing else does.

Every card Clippet emits points at one URL for the video itself:

```
https://<origin>/api/clips/<id>/social.mp4
```

That URL has always worked. What changed is what is behind it: for a clip whose rendition is big or
fast, Clippet now encodes a **second, smaller mp4 for the card** and serves that instead.

**Why a second file at all.** The main rendition is sized for a player somebody chose to open — up to
60 fps, CRF 23, no bitrate ceiling. A card is the opposite case: Discord fetches the file with its own
crawler and stops playing it inline somewhere past ~100 MB, and X wants a stream it can start quickly.
A four-minute high-motion 720p60 capture is over that line on its own, and it is exactly the clip you
most wanted to paste. Measured on the shipped ffmpeg 7.1.1, per minute of video:

| content | main rendition | social rendition |
|---|---|---|
| moderate motion, 720p60 | 1.64 Mbps = 11.7 MB/min | 0.89 Mbps = 6.4 MB/min |
| worst case (fine detail in constant motion) | 14.3 Mbps = **107 MB/min** | 2.35 Mbps = 17.7 MB/min |

The second row is the whole argument: CRF alone has no ceiling, so four minutes of that content is
428 MB, and the same four minutes for the card is 71 MB.

**When it happens.** A card copy is made when the file a card would otherwise be handed is **over
1280×720**, **over 30 fps**, or **at or over `SOCIAL_MAX_BYTES`** (default 25 MiB) — and for a Full,
the size term is applied to the size the plan ESTIMATES before encoding (§27). Otherwise nothing
happens at all and `/social.mp4` serves the main rendition byte for byte, as it always did. All three
terms matter: 60 fps is what makes a *long* clip too big and anything over 720p is already more than a
card is handed, so the rule catches the shape rather than waiting for one instance of it to cross a
threshold. A short 720p30 clip never costs you a second encode.

**What it is.** The same picture inside the 1280×720 box (`TRANSCODE_MAX_WIDTH`/`_HEIGHT`, §27) — the
same dimensions when the source is already inside it — encoded from the clip's own playable file (so it
matches what people watch, trim and all):

| | |
|---|---|
| Frame rate | **30 fps** at most (or your `TRANSCODE_MAX_FPS` if you set it lower) |
| Quality | CRF **26**, with a hard **2.5 Mbps** ceiling and a two-second buffer |
| Audio | AAC stereo 48 kHz at **96k** |
| Layout | faststart mp4, `clips/<id>/renditions/social-<r8>.mp4` |

Expect roughly **half** the rendition's bytes, and that is the cost per qualifying clip in your bucket
until the clip is deleted. Nothing is added to the picture: no watermark, no caption, no logo.

**Turning it off.** `SOCIAL_MAX_BYTES=0`. No job is ever queued, whatever the frame rate, and
`/social.mp4` goes back to serving the main rendition. Raising the number instead (say
`SOCIAL_MAX_BYTES=104857600`) keeps the 30 fps rule but stops the size rule from firing on ordinary
clips.

**Where it runs.** On the same media queue as trims and GIF previews, and its ffmpeg takes a slot from
the same `TRANSCODE_CONCURRENCY` pool — so a default install still runs **one** ffmpeg at a time
whatever is queued. It starts after the clip is already `ready`, so nobody waits for it: the clip is
playable and shareable the whole time, and the card simply gets better a few seconds later. If it fails
for any reason the clip is untouched and `/social.mp4` keeps serving the rendition; you will see one
line at INFO saying so, and nothing else in the system notices.

**The first paste can beat it — check before you test this.** "A few seconds later" is a real gap, and
inside it `/social.mp4` still answers with the main rendition: the exact file the feature exists to keep
out of a card. That matters more than a few seconds normally would, because **Discord decides an unfurl
from its first fetch of a URL and caches the result** — the same fact `CAPTURE_READY_WAIT` exists for
(§7, §9). Paste inside the window and the link-out card you get is the card that channel keeps, after the
good file lands and after the hour `max-age=3600` bounds: that header only governs a crawler that comes
back on its own. The window is a few seconds on an idle instance, and as long as the queue when it is
not — the social encode waits for the same single ffmpeg slot as every transcode already queued, so a
busy instance can leave a clip `ready` and card-less for minutes. The check is one command:

```sh
docker compose exec app clippet admin ls <id>
#  social:    (none: /social.mp4 serves the rendition)          ← no card file (see the two readings below)
#  social:    clips/<id>/renditions/social-DnPmwq3P.mp4 …       ← now the link unfurls from that file
```

**`(none: …)` has two readings — and only one of them is "wait".** For a clip that **qualifies** for a
social encode (at or above `SOCIAL_MAX_BYTES`, or above 30 fps — "When it happens", above) it means
*not yet*: the job is queued or running, and pasting now spends your one unfurl on the main rendition
(or, rarely, that the encode failed and the fallback is now permanent — the `admin jobs` check below
tells the two apart).
For a clip the rule **skipped** — small *and* at or under 30 fps — it is the *final* answer: no job was
ever queued, none ever will be, `/social.mp4` serves the rendition by design, and the link was safe to
paste the moment the clip went `ready`. A short 30 fps capture sits on that line for ever, correctly.

**Don't eyeball it — wait on it, with a bound.** The two `social:` lines differ by a whole word, and
the one that means "not yet" is the one that looks like an answer. `admin ls` prints one fact per line
and is stable enough to `grep`, so for a qualifying clip the wait is a loop rather than a judgement —
bounded, because a skipped clip (or a mistyped id, which makes `admin ls` fail and print nothing to
match) would otherwise spin for ever:

```sh
for i in $(seq 1 60); do                                    # five minutes, then give up
  docker compose exec -T app clippet admin ls <id> | grep -q '^social:    clips/' && break
  sleep 5
done
docker compose exec -T app clippet admin ls <id> | grep '^social:'
```

It falls through the moment the card file exists — then paste the link. If the five minutes run out
and the line still reads `(none: …)`, ask which reading applies:

```sh
docker compose exec -T app clippet admin jobs --clip <id> --kind social
#  0 job(s) (kind=social clip=<id>)          ← the rule skipped this clip, or the id is wrong:
#                                              nothing is coming, and the link is already safe
#  #187 … kind=social state=queued|running   ← still waiting for its ffmpeg slot: run the loop again
#  #187 … kind=social state=failed           ← it will not arrive; /social.mp4 keeps serving the
#                                              rendition (above), and that is the end of it
```

Four spaces after `social:` — the column `admin ls` aligns on — and the key begins `clips/` only when
`S3_KEY_PREFIX` is empty, which is the default; if you set a prefix, match on `^social:    <prefix>clips/`
instead. The grep matches the key line and nothing else, so it can never mistake a `(none: …)` line for
a card file; what it cannot do is tell you *which* `(none: …)` you are looking at, which is what the
bound and the `admin jobs` check are for.

**Trimming replaces it**, exactly like the preview GIF (§12, §13): the same UPDATE that swaps the
rendition clears the social columns, deletes the old object and queues a fresh encode, so a trimmed
clip never has an untrimmed card file. The same best-effort-delete caveat as §13 applies — a delete
that fails leaves an unreferenced object inside a live prefix, removed with the clip.

**What you can see.**

```sh
docker compose exec app clippet admin ls <id>
#  rendition: clips/<id>/renditions/main-kE55ivAk.mp4 size=1217696 etag=…
#  social:    clips/<id>/renditions/social-DnPmwq3P.mp4 size=679295 etag=… dims=1280x720
#  …or, for a clip the rule skipped:
#  social:    (none: /social.mp4 serves the rendition)

docker compose exec app clippet admin stats --no-storage
#  jobs:    queued=0 running=0 (transcode=12 notify=0 trim=1 gif=1 social=3 replace=0 poster=0)
```

`clippet admin ls <id> --stat` checks the key in the bucket like every other one, `clippet admin
orphans` covers it, and deleting the clip removes it with the prefix. There is nothing separate to
clean up.

**One thing to know about the card.** `og:video:width` / `og:video:height` and the Twitter player
dimensions describe **the social file**, because that is what a crawler fetches. Today it has the
rendition's dimensions and only half its frame rate, so the numbers look the same; they are read from
the file rather than assumed, so they stay correct if you ever narrow `TRANSCODE_MAX_WIDTH` /
`TRANSCODE_MAX_HEIGHT` on an instance that already has clips.

### `TRANSCODE_MAX_BITRATE` — a ceiling on the main rendition

The other half of the same problem, for the file people *play* rather than the one Discord fetches.
CRF is a quality target with no upper bound on bitrate: a minute of fine detail in constant motion
really does encode to 14 Mbps at CRF 23, and if your bucket is remote and your viewers are on phones,
that is the number they feel.

```sh
# in .env — BITS per second, not kilobits. 0 (the default) = no ceiling.
TRANSCODE_MAX_BITRATE=6000000     # 6 Mbps, generous for 720p
```

It becomes `-maxrate` on the main encode with a `-bufsize` of twice it. Leave it at `0` unless your
uplink, not your disk, is the limit — CRF alone gives the better picture for the same average size, and
the social rendition already handles the card. A value between 1 and 99999, or above 17179869184
(2^34), is refused at boot with a message naming the variable: `2500` is what someone thinking in
kilobits types, and 2.5 kbps of video would otherwise be encoded silently; the roof is past any sane
rendition and keeps the `-bufsize` doubling inside int64. The social rendition ignores this setting
entirely; it has its own ceiling.

**Changing `SOCIAL_MAX_BYTES` or `TRANSCODE_MAX_*` after clips already exist affects future encodes
only.** A clip already `ready` keeps the file it was encoded with under the settings that were live at
the time; nothing re-encodes on its own when you edit `.env` and restart. It re-derives — and picks up
whatever the settings say now — the next time something already re-encodes it anyway: a trim (§12) or a
replacement (roadmap #31). There is no separate "re-apply settings to every clip" command.

### Interlaced sources

Old capture cards and SD sources write **interlaced** video: two half-pictures of different moments
woven into one frame. Clippet's rendition is always progressive, so an interlaced upload is
deinterlaced (`yadif`) before anything else happens to it. There is nothing to configure, and there is
nothing to do if all your sources are modern console or PC capture — those are progressive and are not
touched: only frames a source actually flags as interlaced are processed.

It is worth knowing this happens, because the failure it prevents is invisible from the outside: an
interlaced source encoded as-is produces a file that *reports* itself progressive while the comb is
still in the pixels, and no player can undo that. If you have ever seen horizontal tearing on fast
motion in an SD clip and found nothing wrong with the file's metadata, this is the fix. One side
effect: trimming such a clip always re-encodes rather than taking the fast stream-copy path (§12),
because a copy would keep the fields.

## 22. The watch page

The page a link opens is the product, so three things on it are worth an operator knowing about: who
picks the picture Discord shows, what the trim control actually is, and what the page does in a
browser that it does not do on the server. **Nothing here needs configuring** — there is no new
variable in this section — and none of it changes an existing URL.

### 22.1 Choosing the card image

Every clip gets a card image automatically: the transcode opens a bounded twenty-second window a
little way into the clip and takes the most representative frame in it that is not near-black
(`DESIGN.md` §20 erratum 92 is the long version). That is a good guess, and on the clip somebody
actually cares about it is still a guess. The owner can override it.

On the share page, under **Card image**: pause the player on the frame you want and press **Use this
frame**. The page queues a small job, watches it, and swaps the picture in without a reload.

Three things follow from how it is stored, and they are the whole feature:

- **The choice is remembered against the ORIGINAL upload, not against the cut you are watching.**
  So if you trim the clip afterwards and the chosen moment is still inside the new cut, the card
  keeps showing that frame — the server simply takes it again from the new rendition. If the cut
  removes the moment, the choice is dropped and the automatic rule picks a frame, exactly as it does
  for a clip nobody has chosen one for.
- **The looping GIF preview opens on the same frame.** That is deliberate: the card and the preview
  are two pictures of the same clip and they should agree. Changing the card image does **not**
  rebuild an existing preview, though — the preview is still frames of the clip people are watching,
  so throwing it away would be destroying something that is not wrong. Press **Make GIF** again if
  you want the preview to move too.
- **The URL never changes.** `/api/clips/{id}/poster.jpg` is the same address before and after; its
  `ETag` and `Last-Modified` change, and it is served `public, max-age=3600`, so *already posted*
  cards and already cached players catch up within an hour. The person who made the change sees it
  at once, because the page cache-busts its own copy. This is the same trade §12 (Trim) describes,
  for the same reason and with the same shared-cache caveat.

Five answers you may see, and what they mean:

| What the page says | What happened |
|---|---|
| *there is no picture at that moment in the clip* | The seek landed past the last frame this clip's video stream holds. Some captures are two seconds of picture inside a forty-second file; pick an earlier moment. |
| *storage was unavailable while the frame was taken* | The bucket was unreachable. The clip is untouched and still shows the card image it had; try again. |
| *that frame could not be taken from this clip* | ffmpeg refused the extraction for a reason that is not "there is no frame there" — a rendition the bucket handed back short, say. The log line beside it carries the detail. |
| *the server had no room to take the frame; try again later* | The scratch disk was below `SCRATCH_MIN_FREE_BYTES` (§7) when the frame was taken. The job is requeued with no counter spent — free scratch, or raise the volume, and it runs again on its own. |
| *the card image could not be changed* | Anything that is not one of the four sentences above, including a JPEG the server wrote and then could not read back. The operator log line beside it carries the detail. |

Nothing in that list can hurt the clip. A poster job never writes the clip's state and never touches
the rendition: whatever happens to it, the clip is still `ready`, still playable, and still on a card
image. It runs on the same **media loop** as trims and previews and takes a slot from the same
`TRANSCODE_CONCURRENCY` semaphore (§7), so a default install still runs exactly one ffmpeg at a time.
`clippet admin stats` counts them beside the rest (`jobs: … poster=n`), and `clippet admin ls <id>`
shows the clip they belong to.

For the API: `POST /api/clips/{id}/poster {"seconds":n}` and `GET /api/clips/{id}/poster` — owner
session only, CSRF-guarded, on the upload rate limiter. `seconds` is on the original upload's
timeline and must be a moment the clip actually shows; the last tenth of a second is not choosable,
because a seek at the very end of a file produces no frame at all. The button never walks you into
that: a player left to run to its end is paused **on** the moment the API refuses, so the page
quietly asks for the last moment the clip does hold — you get the final frame, not a complaint about
the picture in front of you.

### 22.2 The trim scrubber and its filmstrip

**The trim control sits with the player.** It used to be a heading inside **Manage**, a scroll below
the picture it edits; it is now the block directly under the video, because its handles seek that
player and its filmstrip is made of that player's frames. Nothing about what it does moved with it,
and nothing about who may use it changed — it is the owner's, exactly as before.

The control (§12) is a track above its two number boxes, with a handle at each end. It is scaled to
the **whole original upload**, which is the timeline the API takes and the one that does not move
when you cut — so the handles can always reach footage an earlier cut removed.

- Dragging a handle **seeks the player** to that moment, so you can see where you are cutting. If the
  handle goes outside the cut the clip currently has, the picture greys out: the rendition you are
  watching does not contain that footage, and the player would otherwise be sitting on the wrong
  frame while pretending it was the right one. The trim itself still works — it reads the original.
- **From = now** and **To = now** set either end from wherever the player is.
- The handles are keyboard-operable: Tab to one, then ←/→ move it a tenth of a second, PageUp/PageDown
  a second, Home/End the ends.
- **The number boxes have not gone anywhere.** They are what the API takes and they are what a browser
  with JavaScript switched off shows: the track, the handles and the two "= now" buttons are all
  rendered hidden and revealed by the client, so a page with no script is the page it always was.

**The filmstrip.** The track carries a strip of thumbnails of the clip, and each handle shows the
single frame of the moment it is naming — so dragging shows you the picture rather than a number.
That strip is one JPEG (`GET /api/clips/{id}/filmstrip.jpg`), about two dozen 160-pixel tiles in a
row, sampled evenly across the rendition you are watching. Six things worth knowing:

- **It is made on demand, once per rendition.** The first owner to open the control on a clip that
  has no sheet gets a `202` and one `filmstrip` job; the page polls and the strip appears a few
  seconds later. Every visit after that is one cached image and no work at all.
- **A trim or a replacement drops it**, for the same reason it drops the preview GIF (§13): the tiles
  were frames of a rendition that no longer exists, and a strip whose tiles sat under the wrong
  handles would be worse than no strip. The next time the control is opened, a new one is made.
- **It runs on the media loop** beside trims, previews and card images, and takes a slot from the
  same `TRANSCODE_CONCURRENCY` semaphore (§7) — so a default install still runs exactly one ffmpeg
  at a time. `clippet admin stats` counts them (`jobs: … filmstrip=n`) and
  `clippet admin ls <id> --stat` prints the sheet's key, size and tile geometry.
- **It costs a few hundred kilobytes per clip**, under the clip's own prefix
  (`clips/{id}/filmstrip/strip-{r8}.jpg`), so it is deleted with the clip, counted by
  `clippet admin stats` / the storage budget (§24), and covered by `clippet admin orphans` like every
  other object. Nothing has to be configured or cleaned up by hand.
- **The pass decodes the rendition**, which is a few seconds of work for a clip and rather more for a
  very long one. It is bounded at three minutes; past that the job fails, the strip does not appear,
  and the scrubber keeps the plain track it has always had. Nothing else about the clip is affected,
  which is the rule for the whole feature: a filmstrip job never writes the clip's state and never
  touches its rendition, so whatever happens to a sheet the clip is still ready, still playable and
  still on the card image it had.
- **Reading a sheet is public; MAKING one is the owner's.** Anyone holding the clip's link may fetch a
  sheet that already exists, like `poster.jpg` and `preview.gif` — there is nothing in it the video does
  not show frame by frame — but only the owner's own browser session can cause one to be made, and a
  visitor who asks about a clip that has none is simply told there is none. That gate is there because
  this is the heaviest of the on-demand passes: a preview GIF samples six seconds at 480 px, a filmstrip
  decodes the whole rendition. So a widely shared link cannot spend your encoder on thumbnails nobody
  will look at (the only reader of a sheet is that trim control), and what bounds the owner's own asking
  is unchanged: one job per clip per rendition and never another, the `view` limiter (60/min/IP) on
  every call, and `TRANSCODE_CONCURRENCY` (§7) over the whole media loop. There is nothing to configure
  either way. (§20 erratum 227 is the gate; 226(f) records the day this was a public pass and what it
  cost.)

If a filmstrip cannot be made, the control loses its pictures and nothing else: the handles, the
number boxes, the keyboard behaviour and the trim itself are untouched. The two number boxes also
lost their spinner arrows, which sat on top of the digits in a field that narrow — type the value, or
use the handles.

There is no configuration for any of this.

### 22.3 Times, countdowns and keyboard shortcuts

**Times.** The page still renders every timestamp as absolute UTC — `expires 2026-09-07 17:29 UTC` —
and that is exactly what a crawler and a scriptless browser see. Each one is now wrapped in a
`<time datetime="…">` element, so a browser rewrites it into the **viewer's own locale**, and an
expiry less than a day away gains a live countdown (*in 4 hours*, *in 12 minutes*, *in under a
minute*) that refreshes once a minute while the tab is visible. A clip kept forever says "kept
forever" and has no countdown, because there is nothing to count.

**Keyboard.** A native HTML player only listens to keys while it has focus, which meant a share page
ignored the space bar until you clicked the picture. The share page now answers, wherever the focus
is (except inside a text field or on a button):

| Key | |
|---|---|
| `Space` / `K` | play / pause |
| `J` / `L` | back / forward 10 s |
| `←` / `→` | back / forward 5 s |
| `↑` / `↓` | volume |
| `M` | mute |
| `F` | fullscreen |
| `0`–`9` | jump to that tenth of the clip |

**Two preferences are remembered in the viewer's own browser** — mute, and the **Loop** toggle beside
the player — using `localStorage` and nothing else. They are never sent anywhere, never stored on the
server and never visible to you as the operator; they are also per-browser, so they do not follow a
person between devices. If a browser blocks site data, the controls still work and simply do not
remember.

**The embed is deliberately excluded** from all of that. `/embed/{id}?autoplay=1&muted=1&loop=1` is a
contract whoever wrote the embedding page relies on (§9), and a viewer preference must not override
it.

## 23. Uploading

Three ways in besides a capture tool (§9): the upload form, your phone's share sheet, and replacing
the file behind a link you have already posted.

### 23.1 The upload form: several files, drag-and-drop, cancel, retry

The form on the front page takes **several files at once** and uploads them **one after another**.
That is deliberate rather than a limitation: uploads in parallel would race each other for the same
rate-limit budget, the same scratch disk and the one ffmpeg slot `TRANSCODE_CONCURRENCY` gives you,
and would only make each of them slower. The status line says which file is in flight
(`2 of 5 · raid-kill.mp4 — 43%`).

- **Drop** files onto the upload card, or **paste** them (some recorders copy the file itself to the
  clipboard). The card highlights while you drag.
- Everything else on the form is shared by the whole batch: description, tags, *Delete after*, *Unlisted*.
  A title you type is suffixed `(2/5)` per file so the batch stays sortable; leave it blank and each
  clip is named after its own file, which is usually what you want.
- **Cancel** appears while a batch is running and stops it: the file in flight is aborted and the rest
  of the queue is dropped. While the transfer is still running nothing survives that — no clip row, no
  object in the bucket, no scratch directory — because the server treats the disconnect exactly as it
  treats a dropped connection. Once the last byte is on the wire (the line reads *saving…*) the
  outcome is the server's: it may already have stored the file and committed the clip, and the page
  says so rather than claiming an undo that did not happen.
- **Try again** appears after a failure or a cancel and re-sends that same file without re-picking it.
- At most **20 files** per batch. Past that the form takes the first 20 and says so; drop the rest in
  after.

**Without JavaScript** the same form still works, for one file: it posts to `/api/upload` (§9's
endpoint) with everything you filled in — title, description, tags, *Delete after* and *Unlisted* — and
answers with the clip's JSON, where the `url` field is the link. It cannot show a progress bar or
take a second file, and that is the whole difference.

The limits an upload meets are the ones in §7: `MAX_UPLOAD_BYTES` (413 `too_large`),
`RATE_UPLOAD_PER_HOUR` and `RATE_UPLOAD_PER_HOUR_PER_IP` (429), and free space on the scratch volume
(507 `insufficient_scratch`). A request that fails any of them — or arrives with no credential at all
— is refused **before its body is read**, and the answer closes the connection when the body is large.
An anonymous client cannot make this server spool a gigabyte to be told no.

### 23.2 From a phone: the share sheet

Clippet ships a web app manifest (`/manifest.webmanifest`) that declares a **share target**. Install
the site on an Android phone — Chrome's ⋮ menu, *Install app* / *Add to Home screen* — and Clippet
appears in the system share sheet: open a video in Files, Photos or your capture app, hit Share, pick
Clippet, and the file is uploaded to your account. The browser lands you on the clip's own page when
the transcode finishes (`CAPTURE_READY_WAIT`, §7), so the link is a player by the time you paste it.

What to expect:

- **Android Chrome only.** iOS has no share-target API at all; on an iPhone the same install gives you
  a home-screen icon and nothing more. Firefox and Samsung Internet support it to varying degrees;
  Chrome is the one this was built against.
- **https is required to install.** Over plain http the browser will not offer the install and the
  share target never registers (§5 is the reason https is the supported deployment anyway).
- **You must be signed in in that browser.** The share sheet POSTs with your session cookie; a
  signed-out share answers with a page telling you to sign in and share again. It does not redirect
  you to the login form, because the browser would not re-post the file afterwards — nothing is
  uploaded, and you lose nothing but the second tap.
- **No service worker is shipped**, deliberately: nothing here caches, nothing here works offline, and
  a script that outlives the page would need its own answer to "what does this binary call". If a
  future Chrome refuses to install without one, that is where it would go.

The upload itself is the same code every other upload takes — same limits, same rate buckets, same
ffprobe gate — so a share of something that is not a video comes back 415 like anything else.

### 23.3 Replacing the file behind a link

*"That is the wrong clip"* / *"I trimmed too hard and want to start from a better recording"*. On a
ready clip's page, the owner block has **Replace the file**: pick a video, and the clip keeps its
link while the new file is transcoded.

What is kept: **the address** (`/{id}`, so every paste of it keeps working, including cards
already posted in Discord), the title, description, *Unlisted*, tags, collection membership, the
lifetime and its countdown, and the view count.

What is reset: any **trim** (the new upload is its own timeline, and the whole of it is the new clip),
the **GIF preview** (make it again), and the social rendition, which is re-derived automatically.

What a viewer sees: **nothing**. The clip stays `ready` for the whole run — the old file plays right
up to the instant the new one commits — so a link you posted never degrades to a link-out card while
this happens. Players and crawlers catch up within the hour (`max-age=3600`, §12's note about caches
applies here exactly as it does to a trim); your own page cache-busts itself.

The previous upload is deleted **after** the new rendition is ready, never before: if the replacement
fails to transcode, the clip is untouched and still playing the file it had, and the page says why.
`clippet admin ls <id>` shows the new `source:` key afterwards (`…/original/source-<r8>.mp4`), and
`clippet admin stats` counts the job kind (`replace=n`).

**Two things the owner's page does while it runs.** *Cancel* stops the **upload** — the transfer, not
the job — and the clip is untouched, because an aborted transfer never reached the point where a job
exists. The button disappears the moment the server has the file: from there the work belongs to the
queue, and there is no route that cancels it (a wrong take that has already landed is fixed by
replacing it again). And a **reload while a replacement is transcoding rejoins it** rather than
showing a stale page with a Replace button whose only possible answer is `409 replace_in_progress`;
the page asks the same `GET …/replace` it polls with, so this costs nothing on a clip nobody has
replaced.

Two things it is not:

- **It is not a way to edit metadata.** Any text fields sent with a replacement are ignored; the title,
  tags and lifetime are changed on the same page, above.
- **It is not available to a capture token.** Replacing is session-only, like every other owner action
  (§9's token model): a leaked ShareX token can upload junk to your account, but it cannot overwrite
  the clip you posted last week.

**One thing it can leave behind.** A replacement uploads the new file into the clip's `original/`
prefix *before* the transcode starts, and its own failure paths remove it again. A replace job driven
terminal by **boot reconciliation** — the process died mid-run twice, and `replace` is capped at two
attempts — never gets to run those paths, so the uploaded file stays in the bucket, referenced by no
column. It costs storage and nothing else: the clip is untouched and still plays the file it had, and
deleting the clip removes the whole prefix.

The detector is §24.4's **third report**. Once the replace job is terminal the live-job interlock
lifts, and `orphans` prints the file as an `extra` under a live clip (an excerpt: plain `orphans` also prints
that clip's `extra … subtotal objects=1 bytes=…` line and the run's `extra=N extra_bytes=N` summary,
§24.4):

```
extra   dDvdz86vMM1y state=ready key=clips/dDvdz86vMM1y/original/source-9f3ab2c1.mp4 bytes=…
```

`--delete` will not remove it — it answers `refusing to delete an original; left alone`, because
nothing under `original/` is ever deleted by anything but deleting the clip (§24.4) — so this is a
*report*, not a cleanup: it tells you how much a crashed replacement is costing you, and `clippet
admin jobs --kind replace --state failed` tells you which ones. If the number ever matters, the clip
is the unit: delete it, or replace the file again successfully and delete the clip later.

## 24. Operator toolbox

§8 is the day-to-day page: logs, `up -d`, the handful of `admin` commands you type most weeks. This
section is the rest of the CLI — the commands you reach for when something is *wrong*, or before you
change something and would rather know first. All of them run inside the app container against the
same SQLite file and the same bucket, and all of them are read-only unless the verb says otherwise.

```sh
docker compose exec -T app clippet admin doctor            # preflight: will this configuration work?
docker compose exec -T app clippet admin jobs              # the job table, with a per-kind summary
docker compose exec -T app clippet admin ls --state failed # which clips ended badly
docker compose exec -T app clippet admin retry <id>        # put a failed clip's transcode back on the queue
docker compose exec -T app clippet admin jobs unstick --older-than 8h   # repair a job left `running`
docker compose exec -T app clippet admin orphans           # objects with no row, rows with no object, extras
docker compose exec -T app clippet admin objects <id>       # what is in the store under ONE clip's prefix (no database needed)
docker compose exec -T app clippet admin collections ls    # every collection: id, owner, clips, created, title
docker compose exec -T app clippet admin collections rm <id> --owner <user>   # remove that LIST; its clips stay
docker compose exec -T app clippet admin storage migrate --to fs --dry-run    # what moving to a directory would copy (§32)
docker compose exec -T app clippet admin backup /data/backups --rotate 7
# ...and one that takes a folder of its own rather than the running container (§34.4):
docker compose run --rm -v /path/to/old/clips:/import app clippet admin import /import --owner alice --dry-run
```

**Two rules hold for every verb here.** Nothing changes unless the verb's name says it does
(`retry`, `unstick`, `backup`, `orphans --delete`), and every command that *does* change something
needs an explicit selector — there is no default that means "everything", which is the same rule
`admin users prune` and `admin sessions revoke` follow (§8, §19.4). Output is plain text, one fact
per line, stable enough to `grep`.

**`admin collections rm` removes a LIST, never what is on it.** A collection is a list of links
(DESIGN.md §20 erratum 82; §14 and §25.1 here): deleting it deletes the membership rows through the database's own
cascade and touches not one clip, which is exactly what the owner's own Delete button in the web UI
does — the CLI verb calls the same statement. It exists for the one thing `admin users prune` cannot
do on its own: that prune only ever removes an account owning **nothing**, and a collection counts, so
an account whose session went away with an interrupted script (a test run, a capture tool, a person
closing a laptop) could not be removed by any command at all. `collections ls` finds the id,
`collections rm <id>` prints the row — id, owner, clip count, title — before it removes it, and
`--owner <username>` is an optional second selector that refuses the command if the row belongs to
somebody else. An unknown id is a plain `not found` and exit 1. `scripts/e2e.sh`'s own sweep uses
both verbs to clear the collections its throwaway accounts leave behind (§17.3, §20 erratum 275),
and `scripts/visual.sh` does the same for its own.

### 24.1 `admin doctor` — ask before you change anything

`doctor` answers "will this boot, and will it work once it has?" **without booting**. Its most
valuable form runs against a `.env` you have edited but not applied yet, in a throwaway container
that leaves the running app alone:

```sh
docker compose run --rm --no-deps app admin doctor    # the edited .env, resolved as `up -d` would
docker compose exec -T app clippet admin doctor       # the stack exactly as it runs right now
```

Prefer the `run` form for a preflight. It resolves the configuration the way the real container will
— `compose.yaml` carries the `${VAR:-default}` fallbacks for every §4 variable, and `.env` carries
only what you overrode — and it joins the compose network, so a `S3_ENDPOINT` naming a compose
service resolves. It creates and removes one container and touches nothing else.

A configuration that stands entirely on its own — a real bucket, every value spelled out — can also
be checked with no compose project at all:

```sh
docker run --rm --env-file .env.new clippet-app:local admin doctor
```

Two caveats for that form: it gets **no** defaults from `compose.yaml` (so `STORAGE_BACKEND` is the
binary's own `s3` unless `.env` says `fs`, and `S3_FORCE_PATH_STYLE`, among others, is whatever `.env`
says or the code's own default), and it is not on the compose network, so an `S3_ENDPOINT` naming a
compose service does not resolve — add `--network <project>_default` to reach one.

Nineteen checks, one line each, `<check> <verdict> <detail>`:

| Check | What it proves |
|---|---|
| `config` | every §4 variable parses — and on failure, **every** invalid one is named, not just the first |
| `baseurl` | `PUBLIC_BASE_URL` is https (X renders a player card), or loopback (a dev origin), else a warning |
| `paths` | `DATA_DIR` and `SCRATCH_DIR` exist, are writable **by uid 10001**, and how much free space is on each — `SCRATCH_DIR` under `SCRATCH_MIN_FREE_BYTES` FAILs, and `DATA_DIR` **on a device of its own** warns when it is under a derived floor (see below) |
| `db` | the SQLite file's `PRAGMA integrity_check` and its applied migrations |
| `ffmpeg` | both binaries resolve, and the version `/healthz` would report |
| `creds` | whether the bucket credentials are the ones **printed in this repository** (§7). On `STORAGE_BACKEND=fs` there is no credential to get wrong, and it says so |
| `store` | which backend is configured. On `s3`, one line pointing at `bucket` below. On `fs` (§32) it is the whole preflight for a directory: `STORAGE_DIR` exists and IS one, this process can write to it (a probe file, created and removed), how much room is left, whether it shares a filesystem with `SCRATCH_DIR` (reported, never judged), and whether the store **marker** is there — its absence on a database that already accounts for stored bytes is the shape a mis-mounted share takes. It also names the HOST directory bound at `/media` and which variable supplied it (§32.1: `STORAGE_DIR_HOST`, or the desktop default's `HOME`/`USERPROFILE`), because the container cannot otherwise say where on your disk the bytes went |
| `bucket` | the bucket is reachable **and writable**: it puts a probe object and removes it again. On `fs` it is `skip`, with its reason on the line |
| `media` | how the media is DELIVERED: `MEDIA_DELIVERY=proxy` (the app streams the bytes) or `redirect` — and then the origin a viewer is actually 302'd to, asked of the SIGNER rather than read off `S3_ENDPOINT`, because virtual-host bucket lookup puts the bucket in the host; plus the signature's life, and which routes stay app-served. A presign origin only this machine can reach, under a public `PUBLIC_BASE_URL`, is a warning: a viewer would follow the redirect to nothing (§33). A `MEDIA_PRESIGN_*` set while `proxy` is in force is a NOTE on the same row, never an error |
| `drop` | the drop folder (§34): off — which is the default and says so — or on, naming the ONE account it ingests as, whether that account EXISTS (a name that matches none is a **FAIL**: `clippet serve` refuses to boot on it, and learning that here beats learning it from a restart), and what is in the folder right now: files waiting, files with a `.failed.txt` beside them, and anything left staged by a process that was killed mid-ingest |
| `proxies` | `TRUSTED_PROXIES` against `CLIPPET_PUBLISH` — the §4 footgun |
| `webhook` | `DISCORD_WEBHOOK_URL` has a valid shape. It is **never called**: a preflight must not post to your channel. With `CLIP_WEBHOOK_URL` set, one more row names its scheme and host — never its path — and it is not called either (§11.1) |
| `bot` | the Discord bot's state (§30): off, or on with the application id, the three commands' names (`commands: /upload, /clip and Enrich`, or whatever the three `DISCORD_*_COMMAND_NAME` variables renamed them to — §30.2) and the Interactions Endpoint URL to paste; a loopback or plain-http `PUBLIC_BASE_URL` is a warning, because Discord cannot POST to it. On the **official** bot (`DISCORD_BOT_TOKEN=managed`, §30.8) it says `token managed`, prints the claim page's origin check instead of a URL to paste, and lists the servers you have bound. Like `webhook`, it calls nothing |
| `auto` | Clippet Auto on this box (§30.10): off — and then it calls nothing — or the mode, the seconds and the notice (never the token), plus ONE read-only question to Discord: does `GET /users/@me` answer with `DISCORD_AUTO_TOKEN` (FAIL if refused), and is the Message Content intent on for that application (FAIL if off: the gateway would refuse the connection). No answer from Discord is a warning |
| `login` | Sign in with Discord's state (§19.6): off, or on with the client id (never the secret) and the **Redirect URI** to paste into the Developer Portal → OAuth2 → Redirects, byte for byte. A plain-http `PUBLIC_BASE_URL` on any host but localhost is a warning: Discord refuses such a redirect at its own page |
| `repair` | what `JOBS_UNSTICK_AFTER` resolves to: the age at which the app repairs a job left `running` with no worker, the interval it checks on, or that the feature is off (§24.6). A value the `TRANSCODE_HARD_TIMEOUT` floor RAISED is a warning — you asked for one thing and will get another |
| `backup` | timed backups (§15.6): off — the default — or on, with the interval, the directory and how many snapshots are kept; then the last run and its result and when the next is due, as the server recorded them. A `BACKUP_DIR` that is not a directory is a **FAIL**: every run would fail. A last run that failed is a warning |
| `snapshot` | the database copy in the store — the bucket, or the media folder's `_clippet/` (§15, DESIGN §20 errata 343, 348), one of four shapes: `on · 2 min ago · 1.3 MiB` (how old the store's copy is and its size, from the two objects' metadata), `off (DB_SNAPSHOT=off)` — a **warn** when timed backups (`backup`) are off too, because then the database has no backup of any kind, and the line names `DB_SNAPSHOT` and `BACKUP_EVERY`/`BACKUP_DIR` —, `never` (the store holds none yet — the server writes one at boot), or a **FAIL** with the reason — the server's last upload failed more recently than it last succeeded (as `DATA_DIR/snapshot-status.json` records it; another database's snapshot on the same prefix is one), or the bucket could not be read. It reads; it writes nothing |
| `prices` | the storage prices as the app PARSED them (`storage 0.015/GB-month, egress unset`), or the `STORAGE_PRICING` preset behind them with the date its list was read and the free-egress rule it models (`preset b2 (STORAGE_PRICING, read 2026-09-13, USD): …`), or that nothing is priced and no cost estimate appears anywhere (§31, §31.5). Never an amount: what a price makes of your bytes is `admin stats`' `cost:` line, and never `/healthz` |

Verdicts are `ok`, `warn`, `FAIL` and `skip`. **Exit 0 unless something FAILED** — a warning never
fails, so `clippet admin doctor` is safe in a pre-deploy script. When the configuration does not
parse, every other check prints `skip` rather than a guess, because there is nothing trustworthy to
check it against.

Two things it is careful about, and both are the point of running it at all:

- **It writes almost nothing.** One probe object under `{S3_KEY_PREFIX}doctor/` — never under
  `clips/`, so `admin orphans` and the sweeper can never see it — which it deletes again, and one
  temp file in each of `DATA_DIR` and `SCRATCH_DIR`, because "the directory exists" and "this
  process may write to it" are different questions and only the second one matters. It never creates
  the database and never migrates.
- **It prints no secret.** Not `S3_SECRET_KEY`, not `S3_ACCESS_KEY`, not the webhook token — which is
  exactly why it exists in preference to `docker compose config`, whose whole output is your secrets
  in the clear. The webhook is reported as `discord.com/<id>/(token redacted)`, and any credential
  that appears inside a storage library's error message is redacted on the way out.

The `bucket` check is a **write** probe on purpose. `/readyz` only ever proved `BucketExists`, and a
read-only R2/B2 key answers that call happily and then fails the first upload with a 503 that the
uploader sees and you do not.

Three verdicts worth knowing before you meet them:

- `paths warn … SCRATCH_MIN_FREE_BYTES does not guard it` means you have put `DATA_DIR` on a
  **different filesystem** from `SCRATCH_DIR` and the database's disk is nearly full.
  `SCRATCH_MIN_FREE_BYTES` governs *ingest* only, so on a split install it says nothing about the
  device SQLite writes to — and a database with no room to write is as fatal as a full scratch disk.
  The floor is derived, not configured: **twice the size of `clippet.db`, never under 64 MiB** (room
  for a `VACUUM INTO` snapshot beside the live file). There is deliberately no `DATA_MIN_FREE_BYTES`
  — this is advice, so it is a `warn` and never a `FAIL`, and `doctor`'s exit code never turns on a
  heuristic. On the shipped compose both directories are one Docker filesystem (`admin stats` prints
  `same_device=true`), so this line cannot appear at all.
- `creds FAIL` means the resolved `S3_ACCESS_KEY`/`S3_SECRET_KEY` are the development pair published
  in this repository (§7) while `S3_ENDPOINT` is a public host. Against a server on this machine or
  network — a loopback or private address, or a single-label name such as a compose service — it is a
  warning when `PUBLIC_BASE_URL` is not loopback, and fine on a loopback origin. On `fs` the row is
  `ok`: there is no credential at all.
- `proxies FAIL` means `TRUSTED_PROXIES` is non-empty **and** `CLIPPET_PUBLISH` binds beyond
  loopback — the combination §4 says never to make, because a caller who can reach the port can then
  send its own `X-Real-IP` and reset every per-IP limit. When `CLIPPET_PUBLISH` is not in the
  environment you handed the command (it is a compose-only variable, so it is not inside the running
  container), you get a warning instead: the check cannot see a port map from in there.

### 24.2 Seeing an outage: `admin jobs`, `admin ls --state`, `/healthz`

```sh
docker compose exec -T app clippet admin jobs --state failed --since 24h
docker compose exec -T app clippet admin jobs --kind trim --state running
docker compose exec -T app clippet admin jobs --clip <id>
docker compose exec -T app clippet admin ls --state failed
docker compose exec -T app clippet admin ls --state ready --owner alice
```

`admin jobs` prints one line per job — id, clip, kind, state, stage, attempts, transient failures,
the age that matters for that state (`queued_for=` or `running_for=`), `run_after`, `started`,
`finished`, whether a checkpoint exists, and the truncated `last_error` as a quoted token — newest
first, capped at `--limit` (100 by default; the table keeps every job the instance has ever run).
After the rows comes a `totals:` block, one line per kind, counting every state. That block is the
answer to "is anything wrong at all": four zeros under `failed` and you can stop reading.

A misspelled `--state`, `--kind` or `--clip` is a **usage error**, not an empty result. "No clips in
state `redy`" is a true sentence and the wrong answer.

`admin ls --state <s>` narrows the clip listing the same way, and `--owner <username>` narrows it to
one account. The count line names the filter, so a short listing is never mistaken for an empty
instance.

**`/healthz` is the same picture from outside**, and it is what you poll:

```json
"queue":  {"queued":0, "running":1, "failed_24h":0, "oldest_queued_age_s":0, "longest_running_age_s":42},
"repair": {"repaired":0, "released":0, "requeued":0, "failed":0, "errors":0}
```

| Field | Alert on |
|---|---|
| `failed_24h` | anything above zero on a quiet instance — jobs are ending badly and nothing else says so |
| `oldest_queued_age_s` | climbing while `running` is 0: nothing is **claiming**. The worker is wedged or the process is not really up |
| `longest_running_age_s` | past `TRANSCODE_HARD_TIMEOUT` (6 h by default): probably a row nobody is finishing — check it against §24.3 |
| `repair.failed` | anything above zero: the automatic repair (§24.6) ran out of attempts on a job, so a clip's owner has been told the encode failed |
| `repair.errors` | anything above zero: a repair pass could not read or write the queue at all — that is the database, not the queue |

`oldest_queued_age_s` counts only jobs that are **claimable right now**. A job waiting out its
retry backoff is not, so a storage blip does not read as a stalled queue. Both ages are 0 when there
is no such row. Nothing here identifies a clip or a user, and nothing is sent anywhere: this is an
endpoint you poll, not telemetry (§15).

### 24.3 Putting it back: `admin retry` and `admin jobs unstick`

```sh
docker compose exec -T app clippet admin retry <id> [<id>…]
docker compose exec -T app clippet admin retry --all-failed
```

`retry` is the CLI half of the owner's **Retry** button, for the morning after an outage when there
are forty of them. Two guards, and neither is optional:

- **Only a `failed` clip is re-queued.** A ready, queued or processing clip is refused with its
  current state. That is the §5 invariant — a second process must not be able to put a clip that is
  mid-encode back on the queue — and it is enforced by the UPDATE itself, not by a check.
- **The original must still be in the bucket.** A retry re-derives the rendition, poster and preview
  from the upload, so a clip whose original is gone would be re-queued only to fail again. It says
  so and skips it; the honest fix for that clip is to delete it.

`--all-failed` is required to mean "all of them" — a bare `admin retry` is a usage error. A batch
that skipped something exits 1. The worker picks the retry up on its next 5 s poll: the CLI is a
separate process and deliberately does not reach into the running worker.

```sh
docker compose exec -T app clippet admin jobs unstick --older-than 8h    # every abandoned job
docker compose exec -T app clippet admin jobs unstick 41                 # one, by id
```

`unstick` repairs a job left in `running` by a worker that is no longer there — the state a crash
inside a job's own cleanup can leave behind. Since `JOBS_UNSTICK_AFTER` (§24.6) the app does this by
itself on a timer, so this command is for the row you do not want to wait for, and for the instance
where the automatic repair is switched off. It
applies exactly the repair the app applies at boot: a job that had **checkpointed** its outputs is
released and retried finalize-only with no attempt spent; a job that had not takes one crash
attempt, with the usual backoff, and fails for good when its attempts run out. Only a `transcode`
job moves its clip (`processing` → `queued`, or → `failed` on the last attempt); a `trim`, `gif` or
`notify` job leaves a `ready` clip exactly as ready as it was.

**Upgrading across §20 erratum 291.** A checkpoint written by a build older than that fix carries the
zero value of `TierFields` — the bug 291 fixed meant nothing had written the real one yet — so a
finalize-only retry of such a row repeats the same blank columns rather than gaining them; the row is
no worse off than it already was, and a fresh transcode from `queued` fixes it outright. `admin jobs
--state running` **before** the upgrade shows whether any such row exists at all (most instances have
none: a `running` job with no worker is already the exceptional case `unstick` exists for).

**About `--older-than`.** The command cannot see whether a worker is running a job — it is a second
process, and the run registry is in the app's memory. Its only evidence is age, so the threshold is
**floored at `TRANSCODE_HARD_TIMEOUT`**: ask for less and the command raises it to the floor and
prints that it did. Repairing a job an ffmpeg is still writing lets a second worker claim it, and the
work the older run is still doing is then wasted. It is only wasted, though: since DESIGN.md §20
erratum 291 every write a run makes to its own job row — the stage, the checkpoint, the cancel — is
pinned to the claim it holds (`started_at`), so a run whose row you repaired under it cannot write its
stage over the new attempt's, cannot hand the new attempt a checkpoint naming its own objects, and
cannot cancel the row. It logs `job row is no longer this attempt's; stage not recorded` at each stage
it passes, ends at its checkpoint with `job row is no longer this attempt's; outputs discarded, row
untouched` (INFO, both), removes what it uploaded and leaves the row — and its attempt counters —
exactly as the repair left them.

**And since erratum 293 that is true of every KIND, not only of a transcode.** The four writes that
END a job — the failure, the release, the disk-shortage requeue and the `done` — are pinned to the
same claim now, so `unstick` on a live `trim`, `gif`, `social`, `replace`, `poster`, `filmstrip`,
`full` or `notify` job loses that attempt just as cleanly: the older run removes whatever it uploaded
(a `notify` job uploads nothing) and leaves the row exactly as you and the next claim left it. Before
293 those four matched on `state = 'running'` alone, and a media run you unstuck could still spend the
NEW attempt's crash budget on its way out — at the cap, failing a job whose encode was running. Each of
the seven media kinds says so in its own line, `<kind> job row is no longer this attempt's; outputs
discarded, row untouched` (INFO). A `notify` job has no output to discard, so its line names what it did
not do instead: `notify job row is no longer this attempt's; nothing recorded, row untouched` (and
`nothing released` or `attempt not recorded` for its other two endings). And one case says the opposite
on purpose: a transcode that had already CHECKPOINTED logs `its checkpoint keeps the outputs, row
untouched` and deletes nothing, because a checkpointed row is the one your repair *released* and the
retry finalizes onto exactly those objects.

The automatic repair (§24.6) is floored at **30 s**
instead, because it *can* see which jobs this process is running — evidence this command does not have.

**That floor is a conservative heuristic, not a proof**, and it is worth knowing why.
`TRANSCODE_HARD_TIMEOUT` bounds **one encode**, not a job's lifetime: a transcode is acquire + probe
+ encode + verify + poster + store + checkpoint, and *acquiring* a large original is bounded by
`STORAGE_TRANSFER_MAX` (6 h) instead. A `trim`, `gif` or `notify` job is bounded by neither — they
wait out the transcode floor with nothing of their own behind it. So a live job can legitimately be
older than the floor, most easily on an instance that lowered `TRANSCODE_HARD_TIMEOUT` while a
multi-gigabyte upload is still downloading. Whenever the threshold is below `STORAGE_TRANSFER_MAX`
the command prints a second `note:` saying exactly that.

Which is why: run `admin jobs --state running` first and read the `running_for=` column. That is the
number this decision is really about — and each row names its clip and kind, so you can tell a
six-hour encode from a `notify` job that has been running since the outage. Repair by **id** where
you can; `--older-than` is the morning-after broom.

**What the owner is told is not what you read.** `unstick` writes its own sentence into the job's
`last_error` — *"interrupted: the job was found running with no worker"* — and so does the repair the
app runs at boot. Those are your words, not a visitor's: `admin jobs` and the log show them verbatim,
but a poster, trim or replacement that ends this way tells its owner the kind's own generic sentence
instead (*"the card image could not be changed"*, *"the trim failed"*, *"the replacement failed"*),
and a preview GIF's public 503 says only *"the preview could not be made"*. The one exception is a
**transcode**, whose last attempt writes the same sentence into `clips.failure_reason`, which the
share page renders: it is worded for that (it names no path, host, job id or operator).

A job that is too young, one that is not running, and one that does not exist are three distinct
answers, all exit 1, none of which changes anything. The id must be exactly the number `admin jobs`
prints after `#`: a pasted `#41`, `41,` or `41x` is a usage error — never job 41, and never job 4.

### 24.4 `admin orphans`: three reports, not two

```sh
docker compose exec -T app clippet admin orphans                          # report only
docker compose exec -T app clippet admin orphans --delete --min-age 1h    # and clean up
docker compose exec -T app clippet admin orphans --clip <id> --delete     # ONE clip's prefix, nothing else listed or removed
```

`orphans` reconciles the bucket against the database and prints three kinds of finding, plus
anything under `clips/` that is not shaped like one of ours (`foreign`, always left alone).
`--clip <id>` lists that one clip's prefix instead of the whole of `clips/` and prints a `scope` line
naming it: the three reports and `--delete` are then bounded by that prefix by construction, which is
what you want when you are cleaning up after one interrupted trim on a busy instance (and what the
E2E does on a shared stack, DESIGN.md §20 erratum 290):

| Line | Meaning | `--delete` |
|---|---|---|
| `orphan <prefix>` | a whole clip prefix with no row — an upload that died, or a delete that half-finished | removed |
| `missing <clip> key=…` | a row referencing an object that is not in the bucket | **never** removed; the row is the record |
| `extra <clip> key=…` | an object under a **live** clip that no column names | removed one object at a time — never while that clip has a running or queued job that writes objects |

`missing` and `extra` print the bucket key: a deliberate exception to §24's secrets discipline, not
a violation of it. The object IS the subject of the report, and a key is not a capability on this
design — the bucket is private, nothing in `internal/` presigns a URL, and every byte a caller sees
goes through the app; the operator running the CLI already holds the bucket credentials. `admin ls`
and `ls --stat` have always printed keys the same way. The rule that stands unqualified is the
**secrets** one (§24.1): never a webhook URL, never a credential, never a session or token hash,
never a user email.

The `extra` report is the one that catches a trim that died at the wrong moment. Every trim replaces
the rendition, poster and preview and deletes the superseded objects *after* the commit, best-effort
and never retried — so a crash in that window leaves paid-for objects under a clip that is perfectly
healthy, and no other report can see them. Each finding carries its size, each clip gets a subtotal,
and the summary line ends with `extra=N extra_bytes=N extra_deleted=N`.

Three safeguards:

- **A clip with a live job is skipped entirely.** A job's outputs are uploaded and *checkpointed*
  before the commit that writes `rendition_key`/`poster_key`, and a checkpointed retry publishes
  exactly those keys without re-uploading them — so between the checkpoint and the commit the
  objects are referenced by the **job**, not by a column, and to this report they look like garbage.
  Any clip with a `queued` or `running` job that could write an object is therefore reported and left
  alone:

  ```
  extra   dDvdz86vMM1y state=processing key=… bytes=… (skipped: the clip has a live job, …)
  ```

  This is the safeguard `--min-age` cannot provide, and the reason it is separate: a job that has
  been stuck since last night is *hours* old, which is exactly the job §24.3 exists to repair. Had
  the sweep deleted its outputs first, `unstick` would then flip the clip to `ready` on keys that no
  longer exist — a permanent 404 on `video.mp4` that `admin retry` will not touch, because the clip
  is `ready`. If you want those objects gone, finish the job first (§24.3, or let it fail out), then
  run `orphans` again.

  **`notify` is the one kind that does not count.** An announcement posts to a webhook and writes
  nothing to the bucket, and against a Discord that is down a queued notify job can sit through its
  retries for hours — so if it counted, the clip you are trying to clean up would be shielded by the
  one job that cannot possibly reference the objects. Every other kind counts, including any added
  later: the query excludes `notify` by name rather than listing the kinds that do write objects.
- **`--min-age` matters more here than anywhere.** An in-flight transcode or trim uploads its
  outputs *before* the commit that starts referencing them, so a young unreferenced object under a
  live clip is usually a job that is still running. The default of 1 h is deliberate; do not lower it
  on a busy instance while `--delete` is in the command.
- **Nothing under `original/` is ever deleted**, whatever the report says. The upload is sacred: the
  one code path allowed to remove an original is deleting the whole clip. Such an object is printed
  and left alone.

A skipped object — for either reason — is printed but is **not** counted in `extra=` or
`extra_bytes=`: those two numbers are what `--delete` would actually remove.

**`admin objects <id>` is the same question about ONE clip** (DESIGN.md §20 erratum 281):

```sh
docker compose exec -T app clippet admin objects dDvdz86vMM1y           # key, size, modified, then a count
docker compose exec -T app clippet admin objects dDvdz86vMM1y --count   # the number alone
```

It lists what is under `clips/<id>/` through the store this instance is configured with — a bucket on
R2, B2, S3 or a server of your own under whatever `S3_KEY_PREFIX` you set, or the directory on `STORAGE_BACKEND=fs`
(§32) — and it is unaffected by `MEDIA_DELIVERY` (§33), which is about how a viewer is handed bytes
rather than about where they are. `orphans` above reconciles the WHOLE bucket against the database and
is the verb for a census; this one answers "what is actually there for this clip", which is what you
want when a reader reports a 404, after a restore (§15.4), or when checking that a clip you deleted
really is gone.

**It asks the store and never the database**, which is the point of having it: the case it exists for
is a clip whose row has already been destroyed, and it answers for one exactly as it does for a live
clip — an id with nothing under it is `0 object(s)` and exit 0, not an error. Like `doctor` and
`restore-check`, and unlike every other verb on this page, it needs no database file at all. An id that
is not id-shaped is a usage error (exit 2); a store that will not answer is exit 1 with the store's own
sentence. `scripts/e2e.sh` makes every one of its bucket assertions through this verb, which is how the
suite reads whichever store the stack under test actually uses (DESIGN.md §20 erratum 281).

**Its other forms are the test harness's writes** (DESIGN.md §20 erratum 346). Keys and prefixes here
are from the bucket's root, not under `S3_KEY_PREFIX`:

```sh
docker compose exec -T app clippet admin objects init                     # create the bucket if it is missing
docker compose exec -T app clippet admin objects --prefix doctor/ --count # count from the bucket's root
printf x | docker compose exec -T app clippet admin objects put some/key  # store stdin under a key
docker compose exec -T app clippet admin objects rm some/key              # remove one key
docker compose exec -T app clippet admin objects rm --prefix e2e-quota-1789149541/   # a test probe's prefix
```

`init` sets no policy, so a new bucket stays private. `rm --prefix` refuses anything that is not a
test probe's `e2e-<word>-<epoch>/` prefix, and this instance's own key space, before it deletes
anything.

### 24.5 `admin backup <dir> --rotate N`

§15.1 covers taking a snapshot to a named file. Point the same command at a **directory** and it
mints the filename itself, which is what makes safe rotation possible:

```sh
docker compose exec -T app clippet admin backup /data/backups --rotate 7
```

- The name is `clippet-<UTC timestamp>.db` — `clippet-20260907T031500Z.db` — which sorts in time
  order, so rotation never has to trust an mtime that a copy would have rewritten.
- `--rotate N` keeps the newest **N** of those files and removes the rest. It also removes
  `clippet-….db.part-…` files older than an hour: the stranded partials §15.5 used to leave you to
  hunt by hand. A partial younger than an hour is kept, because a run may still be writing it.
- **It only ever considers files it minted itself.** Your own copies, a `README`, somebody else's
  backups in the same directory are invisible to it, and the summary line says how many it left
  alone (`untouched=N`). The snapshot it has just written is never a rotation candidate.
- Everything else is unchanged: `VACUUM INTO` a `.part-` file beside the destination, `chmod 0600`,
  atomic rename, never overwrite. Two snapshots in the same second are refused rather than merged —
  the second one would be overwriting a real backup.

A cron line that keeps a week, on the host:

```sh
15 3 * * * cd /srv/clippet && docker compose exec -T app clippet admin backup /data/backups --rotate 7
```

That writes inside the `app_data` volume, which is on the same disk as the database it is protecting
— fine as a rollback point, useless as a disaster plan. Copy the newest file off the box afterwards
(§15.1) and check it with `admin restore-check` (§15.4); off-host retention and encryption are still
yours (§18).

The server can run this on a timer itself: set `BACKUP_EVERY` and `BACKUP_DIR` (§15.6).

### 24.6 Automatic repair of stuck jobs — `JOBS_UNSTICK_AFTER`

§24.3's `unstick` is the morning-after broom. This is the thing that means you rarely need it: the app
repairs a job left `running` with no worker **by itself, on a timer, without a restart**.

```sh
# .env — the shipped behaviour, spelled out
#JOBS_UNSTICK_AFTER=6h15m     # unset = TRANSCODE_HARD_TIMEOUT + 15m
#JOBS_UNSTICK_AFTER=0         # off: back to "a restart or `admin jobs unstick` repairs it"
```

The repair is **exactly** the one §24.3 describes, applied by the app instead of by you: a job that had
checkpointed its outputs is released and retried finalize-only with no attempt spent; a job that had not
takes one crash attempt with the usual backoff and fails for good when its attempts run out; only a
`transcode` moves its clip (`processing` → `queued`, or → `failed` on the last attempt), and the sentence
written into `last_error` is the same one — *"interrupted: the job was found running with no worker"*. It
is the same function, not a second implementation, so the two cannot drift.

**What it will not do.** A job **this process is running** is never repaired, whatever its age says —
every claimed job, of every kind, is known to the worker for exactly as long as it runs it, which is the
evidence the CLI cannot have (it is a second process, §24.3). That is why the automatic repair is safe
where a cron line running `unstick --older-than 20m` would not be: the slow case that trips the age test
is a live job — most easily a multi-gigabyte original still downloading, bounded by `STORAGE_TRANSFER_MAX`
and not by the encode's timeout — and the worker simply knows about it.

**The age still matters**, because it is the only thing that can speak for a process that is *not* this
one: the one that died, and a second container somebody pointed at the same database. The shipped default
is past `TRANSCODE_HARD_TIMEOUT` for exactly that reason, so out of the box nothing that could still be
running *anywhere* is repaired.

**And if it repairs a second process's live job anyway, that job's run now loses its attempt cleanly**
(DESIGN.md §20 erratum 293). Every write a run makes to its own row — its stage, its checkpoint, its
cancel, and since 293 its failure, its release, its disk requeue and its `done` — is keyed on the claim
it holds, for every kind. So the other process's run finds its row is no longer its own, removes what it
uploaded, writes nothing, and logs `[<kind> ]job row is no longer this attempt's; outputs discarded, row
untouched` at INFO — a transcode's line carries no kind in front, a media kind's is prefixed with its
own (`trim `, `gif `, …). The counters your repair set, and the attempt that claimed the row after it, are
untouched. That is the difference between a wasted encode and a corrupted queue, and it is why the
`JOBS_UNSTICK_AFTER=0` advice at the end of this section is about *wasted work*, not about safety.

**You may set it much shorter, and on a single-instance deployment you probably should** — six hours is a
long time for a clip to sit unplayable. The only bound the app enforces is a sanity floor of **30
seconds** (a shorter value is raised to it and the boot line says so). What a value below
`TRANSCODE_HARD_TIMEOUT` costs is one WARN, and it is worth reading twice, because it is *not* about this
process's own jobs:

```
level=INFO msg="automatic stuck-job repair is on (JOBS_UNSTICK_AFTER)" after=30s every=7.5s transcode_hard_timeout=5m0s
level=WARN msg="JOBS_UNSTICK_AFTER is shorter than TRANSCODE_HARD_TIMEOUT: a job THIS process runs is never repaired whatever its age, but a live job of a SECOND process sharing this database could be - set JOBS_UNSTICK_AFTER=0 there (DEPLOY.md §24.6)"
level=INFO msg="automatic stuck-job repair is off (JOBS_UNSTICK_AFTER=0): a job left running with no worker waits for a restart or for `clippet admin jobs unstick`"
```

One container per database is the supported shape (§18), so on a normal install that warning describes a
situation you do not have. It is there because the E2E, and anybody debugging with
`docker compose run … app clippet serve`, briefly do.

The check runs every quarter of the age, never more than five minutes apart and never less than five
seconds. Each repair is one INFO line naming the job, its kind, what happened to it and how long it had
been running:

```
level=INFO msg="repaired a job that was running with no worker" loop=repair job=41 clip=dDvdz86vMM1y kind=transcode action=requeued clip_state=queued running_for_s=22910
```

A pass that repaired anything closes with a summary of the pass. `capped=true` there means the scan
filled its batch of 100 and there may be more stale rows behind it — the next tick takes them, and a
`capped=true` that repeats tick after tick is a crash loop worth reading the rest of the log for:

```
level=INFO msg="automatic repair" loop=repair repaired=3 alive_here=1 candidates=4 capped=false
```

**Where to look.**

- `clippet admin doctor` prints the schedule as its `repair` check — the one place the SETTING is
  reported, because `/healthz` is anonymous and names no configured value:
  ```
  repair   ok   stuck jobs repaired after 6h15m0s (JOBS_UNSTICK_AFTER), checked every 5m0s; a job this process runs is never repaired
  ```
  …and `repair warn` when the age is under `TRANSCODE_HARD_TIMEOUT` (the second-process case above) or
  under the 30-second floor.
- `/healthz` grows a `repair` block of five lifetime counts (§24.2, API.md):
  ```json
  "repair":{"repaired":3,"released":1,"requeued":2,"failed":0,"errors":0}
  ```
  `repaired` climbing is not an alarm — it is the app keeping a promise. **`failed` climbing is**: those
  are jobs whose attempts ran out, i.e. clips whose owners are now being told the encode failed. Alert on
  that one and on `queue.longest_running_age_s` (§24.2); the pair tells you whether the queue is being
  healed or is simply broken.
- `errors` counts passes that could not read or write the queue at all. Non-zero means the database, not
  the queue.

**When to turn it off.** Two cases, both narrow. If you run a second process against the same database
for anything but a moment (§18's "more than one node" is not supported, but people do it anyway), each
one repairs by age alone as far as the OTHER's jobs are concerned — set `JOBS_UNSTICK_AFTER=0` on the
one that must never touch the other's rows. And if you are debugging a wedged job and want the row to sit
exactly where it is, `0` is how you freeze it.

**A worked setting.** A single instance whose encodes are minutes rather than hours: leave
`TRANSCODE_HARD_TIMEOUT` where it is (it is the backstop for a runaway ffmpeg, not a target) and set
`JOBS_UNSTICK_AFTER=5m`. A crashed container's job is then claimable again about five minutes after the
crash, the running encode of the process that is up is never touched whatever its age, and the boot WARN
tells you the one thing that setting gives up.

## 25. Collections, oEmbed, defaults and routing

Five small things an instance grows into once it has been running for a while: filing a clip without
leaving its page, one more kind of card, per-account upload defaults, a number that says how much is
in here, and Discord posts that go to more than one channel.

### 25.1 File a clip from its own page

The watch page's **Manage** block now has a **Collections** list: one checkbox per collection you own,
already ticked for the ones this clip is in. Ticking one adds the clip; unticking removes it. Each
click is its own request against `POST` / `DELETE /api/collections/{cid}/clips/{clip}`, so two browser
tabs filing two different clips both land — which a "read the whole membership, edit it, send it back"
control could not promise.

Adding **appends**: the order you already chose is not disturbed, and a clip that is already in the
collection stays where it is. Removing takes out a **link and nothing else** — the clip, its objects
and its share URL are untouched, exactly as deleting a whole collection has always left its clips
alone.

On the collection page itself (`/c/{id}`), each row you own now carries **↑ ↓ Remove**: the arrows
reorder the list (which is a property of the whole list, so it saves the whole list), and Remove takes
that one clip out. The **Save clips** picker below is unchanged and is still what you use to add or
remove several at once, and still what works with JavaScript switched off — the per-row controls are
revealed by the client and are simply absent without it.

Both new routes are **session-only and CSRF-guarded** like every other owner action: an API token
cannot curate (it is an upload credential, §9). A collection that is not yours is `404`, whether it
exists or not; a clip that is not yours is `400 not_your_clip`, whether it exists or not.

### 25.2 oEmbed

Slack, and a number of forum and wiki packages, look for an **oEmbed** document before they read the
Open Graph tags. Every ready clip's share page now advertises one:

```html
<link rel="alternate" type="application/json+oembed" href="https://clips.example/oembed?format=json&url=…">
```

```sh
curl -s 'https://clips.example/oembed?format=json&url=https://clips.example/aZ3kQ9xB2mLp'
# {"version":"1.0","type":"video","provider_name":"Clippet","provider_url":"https://clips.example",
#  "title":"…","html":"<iframe src=\"https://clips.example/embed/aZ3kQ9xB2mLp\" …></iframe>",
#  "width":1280,"height":720,"thumbnail_url":"…/poster.jpg","thumbnail_width":1280,"thumbnail_height":720}
```

**Nothing about Discord or X changes.** They read the OG and Twitter tags, those tags are byte-for-byte
what they were, and this endpoint is purely additive reach. A clip that is still encoding (or that
failed) answers `type:"link"` with the placeholder image and no player, which is exactly what a
consumer should cache for one.

Things worth knowing:

- **Only URLs on your own `PUBLIC_BASE_URL` are described.** A `url` on any other host — including a
  lookalike, a different port, a different scheme or one with `user@` in front of your host — is
  `404`, the same answer a clip that has gone gets. That is deliberate: an oEmbed endpoint that
  described arbitrary URLs would be a small open proxy carrying your instance's name.
- It is **anonymous and rate limited**, 60 requests per minute per IP, like the view counter and the
  collection GET.
- `maxwidth` / `maxheight` shrink the player box and never grow it; `format=xml` answers `501`.
- An **unlisted** clip is described exactly like a listed one, because its share page is: the
  unguessable link is the authentication (§10), and an unfurler that has the link already has the clip.

### 25.3 Per-account upload defaults

`/settings/tokens` now opens with **Upload defaults**: how long *your* new clips are kept, and whether
they are unlisted. They apply to the upload form's boxes and to a capture tool that sends no
`ttl_days` or `unlisted` field.

The order is **what the upload said > your default > `DEFAULT_CLIP_TTL`**, and the first clause is the
one to remember: anything an upload sends explicitly always wins, so the `clippet.sxcu` this app mints
(which sends both) behaves exactly as it did before. Only silence is filled in.

The TTL choice has three states, and they are different on purpose: *server default* follows
`DEFAULT_CLIP_TTL` wherever you move it, while *never* is a decision that stays yours
even if you later set `DEFAULT_CLIP_TTL=7d` for everybody else.

Over the API: `GET` / `PATCH /api/auth/me/settings`, session-only. An absent key leaves that preference
alone; an explicit `"default_ttl_days": null` clears it back to the server default.

### 25.4 How much is in here — and a storage budget

`GET /api/auth/me/stats` (session-only) answers with **your own** tally, and the library page renders
one line of it above the list: clips, how many are ready, bytes stored, your total views, and how many
of your clips expire within a week. It is your numbers and nobody else's — there is no route that
answers this for another account, and there never will be.

For the instance as a whole, `STORAGE_BUDGET_BYTES` is a line you draw for yourself:

```sh
# .env — 200 GB, in bytes. 0 (the default) is off.
STORAGE_BUDGET_BYTES=200000000000
```

When the bytes this database says are in the bucket pass it:

- the sweeper logs **one** `WARN` on the crossing (and one `INFO` on the way back under — never a line
  a tick, so it stays worth reading);
- `/healthz` grows `"storage_budget":{"used":…,"budget":…,"over":true}` — an **unauthenticated** route,
  so the same numbers reach anyone who can reach the origin at all, not just a signed-in account. A
  reverse proxy (§26) may choose not to forward `/healthz` to the public internet if you would rather
  it stayed internal-only; the compose healthcheck reaches it from inside the Docker network either way,
  so declining to forward it publicly changes nothing about the stack's own liveness check;
- `clippet admin stats` prints a `quota:` line — how much room is left, in words — and the `storage:`
  line with the raw numbers under it;
- Settings' **Storage** panel leads with a `quota` row (`41.2 GiB of 186.3 GiB · 21 %`), red once you
  are at or past the line;
- the library page carries one warning for **anybody signed in** — every account, not an admin subset;
  this instance has no admin role, and the people already sharing the one bucket are the ones who can
  act on the number.

**By default it is never a cap.** No upload is refused because of it, no clip is deleted because of it,
and there are still no per-user quotas and no tiers — that is a product stance (README), not an
oversight. It exists so that "my bucket bill doubled" is something you find out from a log line rather
than from an invoice.

#### Making it a cap — `STORAGE_BUDGET_HARD=1`

If what you want is a line that **holds**, one more variable arms it:

```sh
# .env — 200 GB, and refuse new clips at it.
STORAGE_BUDGET_BYTES=200000000000
STORAGE_BUDGET_HARD=1
```

- A **new clip** is refused with `507 Insufficient Storage`, error code `storage_quota`, once the bytes
  this database says are stored have **reached** the budget: *"This instance is at its storage quota;
  delete or expire some clips, or raise the quota."* The browser upload form, `POST /api/clips`, the
  capture endpoint `POST /api/upload` and the Discord bot's `/upload` all say exactly that sentence.
- **At** the line, not one byte past it: a budget of N bytes is N bytes of room, and an instance holding
  N has none left. (The advisory WARN and `/healthz`'s `over` still fire when the line has been
  *crossed*, one byte later — each is right about its own question, and nothing that reads them changes.)
- **Nothing else is refused.** Replacing the file behind a link (§23.3) and trimming a clip both go
  through, because neither adds a clip — and a replacement is usually how you get back under the line.
  Nothing is deleted, no TTL changes, and no existing clip stops playing.
- **The number is read live**, once per upload, straight from the database — not from the sweeper's
  cached reading — so deleting a clip makes room immediately rather than at the next `SWEEP_INTERVAL`.
  It costs one sum over `clips` per upload, and only when the cap is armed.
- **It is where uploads stop, not a ceiling.** The clip that crosses the line is accepted whole (the
  check runs before a byte is read, against what is stored *now*); two uploads racing in under the line
  both land; and if the database read itself fails the upload goes through and the failure is logged,
  because a cap that refused on its own error would be worse than the overshoot. A household instance
  overshoots by a clip, never by a catalogue — plan the number with that in mind.
- **It is off by default and stays that way.** An operator who set a budget to be *warned* must not
  discover that it had quietly stopped their friends uploading; that is why this is a second variable
  and not a mode of the first. `STORAGE_BUDGET_HARD=1` with no `STORAGE_BUDGET_BYTES` is a **boot
  error** — a cap of zero would refuse everything.
- It is still not a tier and still not a per-user quota. One line, one instance, set by whoever runs it.

Every surface says which mode it is in: the library warning (*"A warning only"* / *"New uploads are
being refused"*), the sweeper's WARN, and `admin stats`:

```text
quota:   204.8 GiB of 186.3 GiB (109 %) HARD (STORAGE_BUDGET_HARD=1: a new clip is refused with 507 at or past the line)
storage: used=219902325555 (209715.2 MiB) budget=200000000000 (190734.9 MiB) OVER (STORAGE_BUDGET_HARD=1: a new clip is refused with 507 until this is back under the line)
```

Two things the number is not:

- **It is not a bucket listing.** It is the sum of what the rows say they reference, which is one
  indexed query and works even when the bucket is unreachable. Objects that no row references any more
  are **not** counted — `clippet admin orphans` is the report for those (§8), and a bucket that has
  been through crashes may hold more than this number admits.
- **What you SEE is not live to the second.** The sweeper measures it once at start and once a tick, so
  the number on `/healthz`, in the log line and in the `quota` row is at most `SWEEP_INTERVAL` old. The
  **cap** does not use that reading: it asks the database at the moment of the upload, which is why
  deleting a clip makes room immediately.

```sh
docker compose exec app clippet admin stats --no-storage
#   quota:   200.0 GiB of 186.3 GiB (107 %) advisory (STORAGE_BUDGET_HARD=0: no upload is ever refused because of it)
#   storage: used=214748364800 (204800.0 MiB) budget=200000000000 (190734.9 MiB) OVER (advisory only: no upload is ever refused, unless STORAGE_BUDGET_HARD=1)
```

### 25.5 Discord: a channel per tag

§11's one webhook still works exactly as it did and is still all most instances need. When you want
clutch plays in one channel and everything else in another, `DISCORD_WEBHOOK_ROUTES` maps **tags** to
webhooks:

```sh
# .env — <tag>=<url> rules, separated by semicolons. ORDER IS PRIORITY.
DISCORD_WEBHOOK_ROUTES=clutch=https://discord.com/api/webhooks/111/aaa;fails=https://discord.com/api/webhooks/222/bbb
DISCORD_WEBHOOK_URL=https://discord.com/api/webhooks/333/ccc     # where everything else goes
DISCORD_WEBHOOK_UNMATCHED=default                                # or `skip`
```

- A ready, **listed** clip is posted to the **first rule whose tag it carries**. The order in the
  variable is the priority: a clip tagged `fails, clutch` with the rules above goes to the `clutch`
  channel, because that rule is written first.
- A clip that matches no rule goes to `DISCORD_WEBHOOK_URL`. Set `DISCORD_WEBHOOK_UNMATCHED=skip` (or
  leave `DISCORD_WEBHOOK_URL` unset) and it is announced **nowhere** — that is how you say "only tagged
  clips are posted". `skip` is only meaningful *with* rules, so `skip` + an empty
  `DISCORD_WEBHOOK_ROUTES` + a live `DISCORD_WEBHOOK_URL` **refuses to start**: nothing could ever
  match, so every post would stop while the log still said `discord webhook configured`. To turn
  Discord off, unset `DISCORD_WEBHOOK_URL`.
- **`default` is a reserved tag** and cannot be a rule: it is already the name this instance's
  `DISCORD_WEBHOOK_URL` channel goes by in the logs and on a queued job. A rule tagged `default` fails
  the boot — give that channel any other tag.
- **Unlisted still means never posted.** Routing decides *where*, never *whether*.
- The channel is chosen when the clip becomes ready, from the tags it has *at that moment*, and is
  remembered on the queued job. Re-tagging a clip afterwards does not re-route a post that is already
  on its way.
- Every routed URL is validated exactly like `DISCORD_WEBHOOK_URL` (https, a real Discord host,
  `/api/webhooks/<id>/<token>`) and the app **refuses to start** on one that is not. Tags follow the
  tag grammar (`a-z`, `0-9` and `-`, up to 32 characters) — a tag no clip could carry fails the boot
  rather than silently never firing. A tag written twice is refused too, because "first match wins"
  only means something if a tag appears once.
- **Every one of these URLs is a credential.** They are treated exactly as the single webhook is: never
  logged, never in `/healthz`, never in an error message, never in the database. A rule the app refuses
  is named by its **position** in the variable (`rule 2: …`) and never by quoting what you typed — so if
  you write a rule backwards, the boot error still prints no token. What *is* logged is the tag:

```sh
docker compose logs app | grep 'discord webhook'
#   msg="discord webhook configured" host=discord.com webhook_id=333
#   msg="discord webhook route configured" tag=clutch host=discord.com webhook_id=111
#   msg="discord webhook delivered" clip=aZ3kQ9xB2mLp job=42 webhook=clutch status=200
```

If you remove a rule from `DISCORD_WEBHOOK_ROUTES` while a post for it is still queued, that post is
**skipped**, not redirected: putting a clip in a channel you did not choose for it is the one thing
routing exists to prevent.

## 26. Behind your own domain: Caddy and nginx

§3 brings up a Cloudflare tunnel, quick or named — both are
Cloudflare. If you already run your own domain on a VPS and would rather terminate TLS
yourself, this section is the other path: a **Caddy** recipe (automatic certificates, the
shortest correct config) and an **nginx + certbot** recipe (the more common stack), both
forwarding to the app's loopback-only publish. Read §4 first if you have not: it explains
`TRUSTED_PROXIES` and why the default `127.0.0.1` bind is what makes trusting a proxy safe
at all — the same safety argument applies here, just with a different trusted address.

Both recipes assume the **native, VPS-installed** Caddy/nginx a package manager gives you
(`apt install caddy`, `apt install nginx certbot`), not a container — that is the ordinary
"a domain, a `systemctl`-managed proxy, an app in Docker" shape this section is for. If you
would rather run the proxy itself as a Docker container, or point cloudflared at this
origin instead, see §3–§4: their Docker-bridge-gateway `TRUSTED_PROXIES` value applies to
that shape, not the `127.0.0.1/32` this section uses.

**Prerequisites for either recipe**: a domain's `A`/`AAAA` record already pointing at this
host, and ports 80 and 443 reachable from the internet (Let's Encrypt's HTTP-01 challenge
needs port 80, however briefly). `CLIPPET_PUBLISH` stays at its default,
`127.0.0.1:8481` (§1) — you are not widening the bind, only adding something in front of
it that already runs on this machine.

### `TRUSTED_PROXIES` here is `127.0.0.1/32`, not §4's bridge CIDR

§4's `172.16.0.0/12` is for a tunnel container reaching the app through Docker's bridge —
that traffic is masqueraded onto the bridge gateway address by the time the app sees it
(hairpin NAT, measured there). A **native** Caddy or nginx process connecting to
`127.0.0.1:8481` is a different path: on Linux, a published port's DNAT preserves the real
source address (§4 says so explicitly, in the paragraph about Docker Desktop's one
exception), so the app sees the TCP peer as `127.0.0.1` — literally the proxy, and only
the proxy, since nothing else on the loopback interface can reach that port from outside
this machine. Set:

```sh
# .env
TRUSTED_PROXIES=127.0.0.1/32
```

then `docker compose up -d`. If you also run IPv6 and your proxy might dial the app over
`::1`, add that too (`TRUSTED_PROXIES=127.0.0.1/32,::1/128`) — but the recipes below name
`127.0.0.1:8481` explicitly, so unless you changed that, IPv4 is all that is used.

### Caddy: automatic TLS, ten lines

Caddy obtains and renews the certificate itself — no certbot, no cron job, no renewal
hook. As long as DNS already resolves and ports 80/443 reach this host, the first request
to your domain is what triggers it.

```caddyfile
# /etc/caddy/Caddyfile
{
	servers {
		timeouts {
			read_body 1h   # generous rather than relied-on: a slow 4 GiB upload must not be
			write     1h   # cut mid-transfer, and CAPTURE_READY_WAIT (max 60s, §7) must fit easily
		}
	}
}

clips.example.com {
	reverse_proxy 127.0.0.1:8481 {
		header_up X-Real-IP {remote_host}
		header_up X-Forwarded-For {remote_host}
		header_up X-Forwarded-Proto {scheme}
	}
}

# HSTS on Caddy's own :80→:443 redirect (§5: the app's own HSTS header only rides on
# responses that reached it — this one is Caddy answering by itself).
http://clips.example.com {
	header Strict-Transport-Security "max-age=31536000; includeSubDomains"
	redir https://{host}{uri} permanent
}
```

`systemctl reload caddy` (or `caddy reload` if you run it outside systemd) picks this up.
Caddy's own ACME challenge handling intercepts `/.well-known/acme-challenge/` ahead of
your configured routes regardless of what the `http://` block says, so the explicit
redirect above does not interfere with certificate issuance or renewal.

Two things this Caddyfile deliberately does **not** set, and why:

- **No request body size limit.** Caddy imposes none by default — unlike nginx's 1 MiB
  default (below), there is nothing here that would 413 an upload before `MAX_UPLOAD_BYTES`
  (§7) does. If you want a *smaller* cap than the app's own, `request_body { max_size ... }`
  is the directive; most operators want the app's own limit to be the only one.
- **No `Upgrade`/`Connection: upgrade` handling.** The app opens no WebSocket — progress
  polling is a plain `GET` (DESIGN.md §8.4), never a long-lived connection — so the
  WebSocket dance most reverse-proxy tutorials show is machinery this app has no use for.

### nginx + certbot

nginx's defaults are the opposite of Caddy's on the two points that matter here: a
1 MiB body cap and (with request buffering on) a whole upload spooled to disk before nginx
forwards a byte of it. Both need an explicit line.

First, a plain-http server block so certbot's webroot method has something to answer the
challenge from:

```nginx
# /etc/nginx/sites-available/clippet
server {
	listen 80;
	server_name clips.example.com;

	location /.well-known/acme-challenge/ { root /var/www/certbot; }

	location / {
		add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
		return 301 https://$host$request_uri;
	}
}
```

```sh
sudo mkdir -p /var/www/certbot
sudo ln -s /etc/nginx/sites-available/clippet /etc/nginx/sites-enabled/clippet
sudo nginx -t && sudo systemctl reload nginx
sudo apt install certbot                       # python3-certbot-nginx not needed; webroot below
sudo certbot certonly --webroot -w /var/www/certbot -d clips.example.com
```

Then add the https block referencing the certificate certbot just issued:

```nginx
server {
	listen 443 ssl http2;
	server_name clips.example.com;

	ssl_certificate     /etc/letsencrypt/live/clips.example.com/fullchain.pem;
	ssl_certificate_key /etc/letsencrypt/live/clips.example.com/privkey.pem;

	# MAX_UPLOAD_BYTES (§7) is the real cap; 0 = nginx imposes none of its own, so the
	# two never drift out of sync when an operator changes the env var. (The web uploader
	# sends a file larger than UPLOAD_CHUNK_BYTES in requests of that size, §7 "Why
	# chunks"; a cap here at or above it would pass the browser and still refuse a large
	# one-shot capture-tool upload.)
	client_max_body_size 0;
	client_body_timeout  300s;
	# Stream the upload straight through instead of spooling the whole body to nginx's
	# temp dir first — the difference between "4 GiB of extra disk churn per upload" and
	# none, and it means the app starts seeing bytes immediately rather than after nginx
	# has buffered the entire file.
	proxy_request_buffering off;

	location / {
		proxy_pass http://127.0.0.1:8481;
		proxy_http_version 1.1;
		proxy_set_header Host $host;
		proxy_set_header X-Real-IP $remote_addr;
		proxy_set_header X-Forwarded-For $remote_addr;
		proxy_set_header X-Forwarded-Proto $scheme;
		# Generous rather than relied-on, same reasoning as the Caddy recipe above.
		proxy_read_timeout 1h;
		proxy_send_timeout 1h;
	}
}
```

`sudo nginx -t && sudo systemctl reload nginx`. Certbot's own systemd timer
(`certbot.timer`, installed with the package) renews the certificate automatically;
`sudo certbot renew --dry-run` proves it without waiting for the real expiry.

`X-Forwarded-For $remote_addr` (not nginx's usual `$proxy_add_x_forwarded_for`) is
deliberate: it **replaces** rather than appends, so a client that sends its own
`X-Forwarded-For` cannot plant a value ahead of the real one. It does not actually matter
here — the app reads `X-Real-IP` first when present (DESIGN.md §13) and this config always
sends one — but it keeps the header honest for anything else that might read it (an access
log, a WAF) without relying on the app's own parsing order to make a spoofed entry inert.

### What both recipes hand off unmodified, and why nothing else is needed

- **`Range`, `If-None-Match`, `If-Modified-Since`, `If-Range`.** Neither Caddy's
  `reverse_proxy` nor nginx's `proxy_pass` reinterprets these for a proxied (not
  locally-served) response — they are relayed as plain headers in both directions, so the
  app's own Range/ETag handling (DESIGN.md §8.5's `video.mp4`/`social.mp4`/`poster.jpg`/
  `original`) works exactly as it does with no proxy in front. There is nothing to
  configure for it.
- **`X-Forwarded-Proto`.** Both recipes set it, but the app does not currently read it for
  anything — the cookie `Secure` flag and the `Strict-Transport-Security` header are both
  decided from `PUBLIC_BASE_URL` alone (§5), not from a per-request header, because TLS is
  always terminated upstream of the app either way. It is included anyway because it costs
  nothing and is what an access log or a future feature would expect to find.
- **Websockets.** Neither recipe adds `Upgrade`/`Connection: upgrade` passthrough. There is
  nothing in this app that upgrades a connection (DESIGN.md §8.4's progress polling is a
  plain, short-lived `GET`), so the extra proxy configuration most tutorials include here
  would be dead weight.

### `PUBLIC_BASE_URL`, and verifying it

Set `PUBLIC_BASE_URL=https://clips.example.com` in `.env` and `docker compose up -d` —
exactly the §3 rule ("whatever the browser types," never what the container listens on)
applies unchanged behind your own proxy. Once the domain answers over https through
either recipe, prove it exactly as §6 does, because §6's assertions are about the public
origin and do not care what sits in front of it:

```sh
BASE=https://clips.example.com bash scripts/e2e.sh
```

That E2E run (the https-only assertions: `Secure` cookies, `og:video:secure_url`, and
every card URL fetched back from the public origin with a Discordbot and a Twitterbot
User-Agent) is the same proof a Cloudflare tunnel gets in §6, and it is what actually
confirms the proxy config above forwards uploads, Range requests and headers correctly —
reading a Caddyfile or an nginx block is not a substitute for watching a real clip upload,
transcode and unfurl through it.

## 27. Quality tiers: Original, Full, Light

Until this section existed, every upload was re-encoded exactly once and the result was fitted into
1280×720 at 60 fps. That meant a 1440p60 capture — the shape this product exists to share — came back
**worse than it went in**, silently, and the only copy of the good frames was the download link.

A clip now has up to three tiers, and none of them is ever downscaled:

| tier | what it is | when it exists | what it costs you |
|---|---|---|---|
| **Original** | the bytes you uploaded | always (the upload is kept for ever) | the upload's own size, once — plus a second copy of it when it had to be remuxed, below |
| **Full** | a faithful re-encode at the SOURCE's own resolution and frame rate | only when no browser can play the original as it stands | roughly the source's own size, sometimes less |
| **Light** | the small copy a Discord/X card fetches | when the file a card would otherwise get is over 1280×720, over 30 fps, or at/over `SOCIAL_MAX_BYTES` — for a Full, from the size the plan ESTIMATES before encoding it | ~2 MB per minute (§21 has the measurements) |

Nothing is added to the picture in any tier: no watermark, no caption, no logo.

### What happens to an upload

**If your file is already web-playable, it is not re-encoded at all.** That covers most of what people
upload — OBS, ShareX, Medal, RePlays, a phone's camera roll, anything an editor exported as H.264 mp4.
The transcode probes it, takes the card image, and the clip is `ready`. Playable means every one of
these, and any single "no" means a Full instead:

| | playable | needs a Full |
|---|---|---|
| container | mp4 / mov / m4v | mkv, webm, avi, ts, flv, wmv, ogg, gif |
| video codec | H.264, AV1 | HEVC/H.265, VP9, ProRes, MPEG-4, DivX |
| pixel format | 8-bit 4:2:0 | 10-bit, 4:2:2, 4:4:4 |
| colour | SDR | HDR (PQ / HLG) |
| scan | progressive | interlaced |
| audio | none, or ONE AAC track | two or more tracks, or Opus/MP3/PCM |
| rotation | a Display Matrix, or none | a legacy `rotate` tag |
| pixels | square | anamorphic (non-square) |
| picture | up to 7680×4320 | larger — the clip FAILS rather than being shrunk |

Three of those are judgements rather than limits, and it is worth knowing which way they went. **mkv
and webm are refused although browsers play them**, because `/video.mp4` and `?download=1` promise an
mp4 and remuxing VP9 into one is not a copy anybody should rely on. **HEVC is refused although Safari
plays it**, because desktop Chrome and Firefox do not — "playable" would mean "playable for some of
your viewers". **AV1 is accepted**, so a source already in AV1 is never re-encoded.

**A playable file whose container needs fixing is REMUXED, not re-encoded.** ffmpeg writes an mp4's
index (`moov`) at the END unless it is told otherwise, and a crawler wants it at the front; a `.mov`
also has to become an `.mp4` for Firefox. Both are fixed with `-c copy`: the container is rewritten and
**not one coded byte of the picture changes**, so the result is still the Original tier. It takes
under a second for a 227 MB file. It is also the case that costs you storage — see below.

**A third thing triggers that same copy, and it is about privacy rather than about players: metadata.**
A file straight off a phone carries where it was filmed (`location`, Apple's
`com.apple.quicktime.location.ISO6709`), what filmed it (`make`, `model`) and when
(`creation_time`); an edited one can carry an `artist` or a `comment`. Every file Clippet *renders*
has those stripped (`-map_metadata -1`), and serving an upload as it stands is the one path that
renders nothing — so an upload carrying any tag beyond the structural ones (`major_brand`,
`compatible_brands`, `encoder`, a track's `language` and `handler_name`) takes the `-c copy` branch,
which strips them, instead of being published byte for byte. The clip is still the **Original** tier,
the copy is still under a second, and what it costs is the storage row below. **Nothing you upload
ever publishes your GPS coordinates**: a camera-roll clip keeps the fast path's speed and loses the
tags on the way. If you want the zero-copy path for such files, strip the tags before uploading
(`ffmpeg -i in.mp4 -c copy -map_metadata -1 -movflags +faststart out.mp4`), which is what the operator
side of that trade looks like.

**When a Full is needed, both copies come out of ONE ffmpeg.** A 1440p60 HEVC upload needs a faithful
copy AND a card copy — and so does a *card-shaped* one that is simply long, because a 720p30 Full past
`SOCIAL_MAX_BYTES` is exactly what the card copy exists to stay under — and one process writes them from
a single decode — measured 1.05-1.42× the Full
alone, against 1.6-1.9× for encoding them back to back and *worse* than that for running them at the
same time (two processes decode the same source, and the first file then appears 2.2× later than it
would alone). See "Encode speed" below.

So the clip becomes `ready` with the Full and the card copy already in the bucket: `ready` means
**card-ready** and **best-quality** at once, and nobody ever streams the 720p30 card copy as the clip's
own file. The price is that the ready flip waits for the faithful encode rather than for the quick one —
minutes for a long 1440p60 upload on one core, which is what `APP_CPUS` and the GPU flavour below are
for — and that a permanent encoder failure fails the clip, exactly as it does for any other upload,
rather than costing it only its better tier.

### What an operator sees

```sh
docker compose exec app clippet admin ls <id>
```

```
rendition: (none: the original is web-playable and is served as it stands)
social:    (none: the card points at the playable file itself)
tiers:     original playable=true remuxed=false 2560x1440 fps=60/1 codec=h264 · full=skipped · light=skipped
```

That is the fast path: one upload, one poster, `probe.json`, and no encoder ever ran. The other shapes:

```
tiers:     original playable=true remuxed=true 1920x1080 fps=60/1 codec=h264 · full=skipped · light=ready
    the upload plays, its container was rewritten, and the card has its own copy

tiers:     original playable=false remuxed=false 2560x1440 fps=60/1 codec=hevc · full=ready(h264) · light=ready
    HEVC: one ffmpeg wrote the faithful copy and the card's copy from one decode

tiers:     original playable=false remuxed=false 720x576 fps=25/1 codec=h264 · full=ready(h264) · light=skipped
    an interlaced SD capture: deinterlaced into a Full, which is small enough to be the card file too
```

`full=` and `light=` are one of `skipped` (not needed — a finished answer, not a gap), `encoding`,
`ready`, `failed`, or `unknown` for a clip uploaded before this feature existed. `admin ls` prints the
stored column, so a clip that is still queued or being probed shows `unknown` there too; the API turns
that into **`pending`** ("nobody has decided this tier yet") in `tiers[]` and in `GET …/progress`,
because a client polling such a clip must not read it as "not needed". `admin jobs --clip
<id>` lists the jobs behind them, and `GET /api/clips/<id>` carries the same states in `tiers[]` with
the URLs and the measured numbers. A tier state cannot stick at `encoding`: every ending of a tier job
— published, failed at its cap, repaired without a worker, skipped, or the clip deleted — writes the
column in the same transaction, so `encoding` always means a job really is coming.

### What a viewer sees

The watch page gets a **quality selector** next to the player as soon as a clip has more than one tier
— a plain `<select>` labelled with real numbers, "Original 2560×1440 60fps · 38.0 MiB", best tier
first. It is server-rendered, so it works without JavaScript; with JavaScript it switches the source
without losing the viewer's position, and it remembers the choice in the browser (nothing is sent
anywhere — a taste in bitrate is not telemetry). A clip with one tier gets no control, because one tier
is not a choice.

While a better tier is still encoding the page says so in one line — "Full quality still encoding." —
and adds the tier to the selector when it lands. A transcode publishes both copies at once, so the state
a viewer meets that line in is a **replacement** whose better tier is behind its card copy, or a clip
whose Light copy is being made after the fact. If a Full ever fails, the page says that instead and the
clip goes on playing its Light copy; that is the one case worth acting on, because the clip is then
permanently a 720p copy of something better and re-uploading is the fix.

While a clip has no card at all — it is queued, still encoding, or having its card copy REMADE after a
trim or a replacement — the page says THAT in one line instead: "Encoding on the server — the Discord
card is not ready yet." No countdown and no estimate (§27, "Time to shareable"). A link-preview crawler
that asks in that window is answered `503` with `Retry-After: 30` rather than shown the file the card
copy is about to replace, which is the same doctrine and the same reason: Discord caches what it
extracted, per URL.

The **embed** (`/embed/<id>`) plays the best playable tier and carries no selector: whoever wrote the
embed URL chose the player, not the bitrate.

### Storage, priced

The uploaded file is kept for ever in every case (§8 "the upload is sacred"), so a tier's cost is what
it adds on top. Per clip:

| shape of upload | objects in your bucket | added bytes |
|---|---|---|
| playable, `moov` already first, small and ≤30 fps | upload, poster, `probe.json` | **~0** — a poster |
| playable, `moov` last (**the common case**) | + the remux | **≈ the upload again** |
| playable and faststart, but carrying phone/camera metadata | + the remux (it strips the tags) | **≈ the upload again** |
| playable, over 720p or over 30 fps | + the Light copy | ~2 MB/min |
| not playable | + the Full | ≈ the source's own size |
| not playable, over 720p or over 30 fps, **or long enough that the Full will not fit under `SOCIAL_MAX_BYTES`** | + the Full + the Light copy | the above plus ~2 MB/min |

The row to plan around is the second: **a remuxed original stores your upload twice.** That is the
price of a container Discord will fetch, and it is why the direct branch exists — a capture tool that
writes faststart mp4 (or an `ffmpeg -movflags +faststart` step in your own pipeline) costs you nothing
beyond the upload. `clippet admin stats` and the owner's own "Stored" line count every tier, and
`STORAGE_BUDGET_BYTES` (§25) is compared against the total.

Deleting a clip removes every tier with it — one prefix, one delete — and `admin orphans` (§24.4)
covers all of them, because a tier is an ordinary object under `clips/<id>/`.

**Scratch, not just the bucket.** A clip on the fast path has no ≤720p rendition, so every job that
derives something from it later — a **GIF** (§13), a **chosen card frame** (§22), the **card copy**
(§21) — downloads the upload itself and reserves room for it. `SCRATCH_DIR` therefore has to hold the
largest upload you accept plus `SCRATCH_MIN_FREE_BYTES` for those jobs and not only for the transcode
(§1 has the arithmetic). It matters because of how the refusal behaves: a job with no room is requeued
with **no attempt spent**, so it waits for room rather than failing — for ever, if the room never comes.
On an install that takes multi-gigabyte uploads, size the scratch volume for one of them.

### Settings

| variable | default | what it does now |
|---|---|---|
| `FULL_CODEC` | `h264` | which encoder the **Full** tier uses. `av1` (libsvtav1) roughly halves that one file for the same picture, encodes considerably slower, and wants decoder hardware from about 2019 (Chrome 70+, Firefox 67+, Edge, Safari 17+). It changes nothing else: a playable original is never re-encoded and the card's Light copy is H.264 whatever this says. |
| `SOCIAL_MAX_BYTES` | 25 MiB | the size threshold for the **Light** copy (§21). Since these tiers, being over 1280×720 or over 30 fps triggers one too, whatever the size. For a Full the threshold is applied to the size the PLAN estimates — conservatively, so a card-shaped source long enough to cross it gets its card copy in the same ffmpeg rather than in a job queued behind the ready flip. `0` turns the tier off. |
| `TRANSCODE_MAX_WIDTH` / `_HEIGHT` / `_FPS` / `_CRF` | 1280 / 720 / 60 / 23 | **the Light copy's box, and nothing else.** They no longer cap what a viewer watches. Lower them for smaller cards; they can never make a Full smaller than its source. |
| `TRANSCODE_PRESET` | `veryfast` | how much CPU an H.264 Full and the Light copy each spend. Unchanged. |
| `TRANSCODE_MAX_BITRATE` | `0` (off) | a VBV ceiling on the **Full**. It is the ONE setting that can make a Full less than faithful — if your uplink rather than your disk is the limit, that is the trade; otherwise leave it off. |
| `TRANSCODE_CONCURRENCY` | 1 | unchanged, and it still bounds every ffmpeg in the process: every media job takes a slot from the same pool as transcodes, trims and GIFs. Raise it only if you routinely queue several uploads at once — past about `APP_CPUS/8` it trades latency for throughput, and a share link wants latency. |
| `CAPTURE_READY_WAIT` | `30s` | how long the capture endpoint holds its 201 waiting for the **card** to work ("Time to shareable", below). |
| `LIGHT_TAG_TEXT` | `{light} preview · {best} on Clippet` | the line burned into the **Light** copy alone ("The Light tag", below). `off` removes it. |
| `TRANSCODE_THREADS` | `0` (auto) | the `-threads` value of every encode; auto is this container's CPU quota ("Encode speed", below). |
| `TRANSCODE_HWACCEL` | empty (CPU) | `cuda` on the **`gpu` image** ("The GPU flavour", below). A boot error on the ordinary image. |

There is no CRF setting for the Full tier on purpose. "Full" is a promise about fidelity, and a knob
that could set it to 30 would be shipping a tier whose name lies; it is CRF 18 for H.264 and 32 for
AV1, which measure about the same.

### Upgrading an instance that already has clips

Migration `0012` adds twelve columns and backfills two of them; `docker compose up -d` applies
it on boot as always (§16). Clips that already exist keep every byte they have and read correctly:
their rendition is reported as a **Full** (which is what it is — it was encoded by libx264 from a
source nobody asked about playability) and their card file, if they have one, as a **Light**. What
0012 deliberately does NOT do is guess `original_playable` for them: nobody probed those uploads for
it, and telling a viewer an old upload is streamable when nothing checked would be worse than saying
nothing. An old clip therefore shows one tier in the selector — which is exactly right, because one
file is all it has.

An old clip picks the tiers up the next time something re-derives it anyway: a **trim** (§12) or a
**replacement** (§23) — and a replacement works on every ready clip, including one whose upload is
served as it stands and one whose Full is still encoding. There is deliberately no "re-tier every clip" command; re-encoding somebody's
whole library on an upgrade is not a thing an operator should be able to do by accident.

### Turning it off

You cannot, and there is nothing to turn off: the tiers are how a clip is stored now. The two things
you can control are `FULL_CODEC` and whether the Light copy is made at all (`SOCIAL_MAX_BYTES=0`, which
puts the card back on the playable file itself, exactly as it was before the card copy existed).

One side effect of `SOCIAL_MAX_BYTES=0` is worth knowing, because the Light copy was quietly preventing
it: **the card IMAGE is cut from the Light copy whenever there is one**, so it is never larger than
1280×720. Without one it comes from the playable file itself, and a 4K clip therefore gets a 4K JPEG —
a megabyte or two, inside Discord's 8 MB limit and past X's 5 MB on a busy frame. If you turn the tier
off, that is the trade.

### Time to shareable: what the capture link waits for

`POST /api/upload` — the endpoint ShareX, RePlays and `clippet.sxcu` post to (§9) — does not answer
the moment the bytes are stored. It **holds its 201 until the clip's Discord card works**, up to
`CAPTURE_READY_WAIT` (default `30s`, hard ceiling `60s`).

That is a deliberate product decision and not a performance accident. Discord fetches a URL **once**
and caches what it extracted, per URL: a link pasted while the clip is still encoding unfurls as a bare
link-out card and stays one long after the clip is playable. So the link the tool puts on somebody's
clipboard is only handed over once it is worth pasting.

**What "card works" means** is the poster plus the file the card's `og:video` points at — the Light
copy when there is one, the playable file otherwise. Not "the upload is stored", and not "the clip is
playable": those were both true of links that unfurled as text.

**At the ceiling the link is still returned.** A capture tool cannot be held for minutes, so a clip
that is still encoding at 60 s answers 201 with

```json
{"id":"…","url":"…","embed_url":"…","state":"processing","card_ready":false,"expires_at":null,"tiers":[…]}
```

`state` is the CLIP's state, as it always was. `card_ready` is the new field and is the one to read.
There is **no estimate** of how much longer it will be, anywhere — on the page, in the reply or in a
header. An estimate of an encode is wrong often enough to be worse than nothing.

**There is no per-request override.** A `wait=` field used to exist and is now a `400`:

```
$ curl -sS -H 'Authorization: Bearer clp_…' -F 'wait=0' -F 'file=@clip.mp4' https://clips.example.com/api/upload
{"error":{"code":"invalid_request","message":"wait is no longer accepted: the server's CAPTURE_READY_WAIT decides how long the reply is held"}}
```

`wait=0` bought a faster answer and a permanently link-out card for everyone who read that link, which
is not a trade a client gets to make for your instance. `CAPTURE_READY_WAIT=0` is still yours to set.

**Crawlers are held rather than told the wrong thing.** While a clip has no card yet, a request to
`/{id}` whose `User-Agent` is a known link-preview crawler — `Discordbot`, `Twitterbot`,
`facebookexternalhit`, `Slackbot`, `TelegramBot` — is answered **`503` with `Retry-After: 30`** and a
page carrying no card tags at all. It is the same answer §9.1 gives for a backend outage and for the
same reason: a crawler that comes back finds the real card, and one that does not has cached nothing.
A human gets the processing page, unchanged, plus one line — "Encoding on the server — the Discord
card is not ready yet." A **failed** clip is never held: its link-out card is the card it will have for
ever.

**What the link-out card itself says.** Title, `Processing…` (or `Processing failed`) under it, and the
committed 1200×630 placeholder — the wordmark on the dark ground, with no text drawn on it. Since
DESIGN §20 erratum 294 that picture carries alt text as well, in both the Discord and the X spelling:
"The Clippet wordmark: this clip is still encoding", or "…could not be encoded" for a failed clip. A
reader on a screen reader is told what the picture is and why the link came back as a link rather than a
player, which is what the ready card's poster has said since erratum 103. There is nothing to configure
and nothing instance-specific in the sentence: it names neither your host nor the clip — the card's own
title and site line carry those — so it reads the same on a self-hosted instance as on clippet.gg.

**How long it actually takes.** Measured on the development stack. The `cpus` column is the app
container's quota (`APP_CPUS`, below); the `1` rows ran at the shipped `cpus: 1.0` with two other
encoders on the same host — loaded numbers, not best cases — and † marks a quiet host:

| upload | what runs | `cpus` | time to a link whose card works |
|---|---|---|---|
| a 6 s 720p30 mp4, `moov` first (OBS, ShareX) | nothing but a probe and the poster | 1 | **1-2 s** |
| the same carrying a phone's metadata | a `-c copy` remux | 1 | **2 s** |
| a 6 s 720p30 HEVC (no browser plays it, card-shaped) | one Full encode, which IS the card file | 1 | **4 s** |
| a 6 s 1080p60 mp4 (above 720p and 30 fps) | the remux plus the Light copy | 1 | **4 s** |
| a 24 s 1080p60 matroska (needs both tiers) | ONE ffmpeg writing the Full and the Light copy together | 1 | **14-30 s** |
| a 150 s 720p60 mp4, 228 MB (playable; over 30 fps, so the card wants a Light copy) | the remux and the Light copy | 4 † | **26 s** (the Light copy itself 22 s, and the capture link came back `card_ready: true` inside the 30 s wait; at one cpu the same encode is 74–103 s in the bench below and the link comes back `card_ready: false`) |
| the same 150 s 720p60 mp4, the GPU flavour (`TRANSCODE_HWACCEL=cuda`, erratum 247) | the remux and the Light copy | 4 † + GPU | **18 s** (the Light copy itself 14 s, hardware decode + `scale_cuda` + NVENC; `card_ready: true` inside the wait) |

Everything a person actually shares is inside the default 30 s. The honest outlier is the last row
scaled up: a long 1440p60 file no browser plays is minutes of encoding on one core, and the link then
comes back with `card_ready: false`. That is the case `APP_CPUS` (below) and the GPU flavour exist for —
`APP_CPUS=4` is close to four times faster, and a card is 9-11× faster again.

### The Light tag: one line on the card's copy

The card's **Light** copy carries one small semi-transparent line in its bottom-right corner:

> 720p30 preview · 1440p60 on Clippet

This is the **only** exception to this product's no-watermark rule, and it is on the **only** artefact
nobody watches on the site once a better tier exists. Nothing else is ever touched: not the Original,
not the Full, not a poster, not a preview GIF, and not anything a viewer downloads or the player plays.
The card IMAGE in particular is cut from an untagged frame — the faithful copy, or the upload itself —
and merely scaled to the card copy's size, so `poster.jpg` is clean everywhere it appears: on the watch
page's own player, in the embed, and as `og:image` (DESIGN.md §20 erratum 219).

It exists because of what a card IS: a reader watching a 720p30 copy of a 1440p60 capture inside
Discord, with no way to know the difference. Without the line the product looks like a 720p product to
everyone who never clicks through.

| setting | default | what it does |
|---|---|---|
| `LIGHT_TAG_TEXT` | `{light} preview · {best} on Clippet` | the line. `{light}` is the card copy's own quality ("720p30"), `{best}` the quality the link leads to ("1440p60"). Write your own; `off` removes it. At most 80 characters, one line — a control character in it is a boot error. |

The geometry is not configurable: ~2.2 % of the frame height (16 px on 720p), 55 % white with a
one-pixel shadow, inset 1.5 % from the bottom-right corner. A card is rendered a few hundred pixels
wide in a timeline, and there is one size at which such a line is legible without being the thing you
look at.

**The card's description says it too.** When a Light copy serves the card, `og:description` and
`twitter:description` carry `Preview 720p30 · Original 1440p60 at this link` — after the clip's own
description when it has one. Real numbers, from the row. Since 2026-09-15 the whole description reads
`{m:ss} · {your description} · {the clause}`, and your own words are CLAMPED so the clause always
survives: a card is about 200 characters before Discord starts cutting, and the clause is the only
thing standing between a Light card and a reader who concludes this is a 720p product. A long
description is cut with one `…`, at a word where there is one; the watch page still renders it whole.
The card's tag INVENTORY is unchanged (§9.1's 27/28 tags): these are values.

**The card image carries the clip's LENGTH, and that is a different thing.** Every `poster.jpg` is
drawn with `{m:ss}` in its top-right corner — the idiom every video site uses on a thumbnail — on a
small dark plate, sized at 4.5 % of the picture's height so it scales with the card. It is information
and not a watermark: no brand, no name, no logo, just the one measurement a reader wants before
deciding to click, and it is gone the instant anybody presses play. Only the poster carries it — the
video, the Original, the Full, the Light copy, the GIF, the filmstrip and the frames you pick from in
"Use this frame" are all untouched. It is not configurable and it can never fail a clip: an image with
no usable font simply draws no label.

The label is set in **Spline Sans Mono**, this product's own mono face, under the
[SIL Open Font License 1.1] — the same licence and the same family the pages use (§28). Unlike DejaVu
the file IS in this repository (`web/static/fonts/spline-sans-mono.ttf`, a 24 KB static instance at
weight 500, beside the `OFL.txt` that covers it) and the image installs a copy of it, with the licence,
at `/usr/local/share/clippet/fonts/`. If you build your own image without it, the label falls back to
whichever of the LIGHT tag's fonts your base ships, and to no label at all if it ships none.

[SIL Open Font License 1.1]: https://scripts.sil.org/OFL

**The font.** The image installs `font-dejavu` (Alpine) / `fonts-dejavu-core` (the GPU image) and the
line is drawn with **DejaVu Sans**, under the [DejaVu Fonts License] — a Bitstream Vera derivative:
free to use, redistribute and modify, with no attribution requirement in the rendered output. No font
file is vendored into this repository. If you build your own image on a base with no font, the app logs
one WARN at boot and the tag is off; the encode is never affected, because a cosmetic line must not be
able to fail a clip.

**Running the binary by hand, off the image?** Then neither path above exists — `/usr/share/fonts` is not
a place on Windows — so both faces are also looked for in a **`fonts` directory beside the executable**:
`fonts/DejaVuSans.ttf` for the Light tag and `fonts/spline-sans-mono.ttf` for the poster label (copy the
second out of `web/static/fonts/` with its `OFL.txt`). That entry is searched LAST, so an image that
installs a font is never overruled by a file next to the binary, and it is a path rather than a setting:
there is no variable to point at a font, on purpose. With no font in either place the tag and the label
are simply off, which is what they have always done (§20 erratum 308).

[DejaVu Fonts License]: https://dejavu-fonts.github.io/License.html

### Encode speed: the settings that actually move it

Everything here was measured on one host (16 CPUs, 62 GB, RTX 5090, Docker Desktop/WSL2) against three
samples — a 150 s 720p60 12.2 Mbps mp4, a 60 s 1080p60 20.2 Mbps mp4 and a 30 s 1440p60 30.3 Mbps mp4 —
using the app's own ffmpeg and its own argv. Your numbers will differ; the SHAPE will not.

**`APP_CPUS`: four is the sweet spot, and the default is one.**

| `APP_CPUS` | speedup vs 1 | what it buys |
|---|---|---|
| 1.0 (**the default**) | — | one core per slot; safe on a 1-vCPU VPS |
| **4.0** | **3.9-4.0×** | the sweet spot. Almost linear, and where a home server should be |
| 8.0 | 7.7-8.0× | the knee: another 1.9× over four |
| 12.0 | +10-20 % over 8 | x264's frame threading and the single-threaded mp4 muxer start to bind |
| 16.0 | nothing over 12 | do not bother |

**`APP_CPUS` must not exceed the host's core count** — see §7 for why (a hard container-create
error, not a killed container). The default stays `1.0` for exactly that reason: raising it in the
shipped file would refuse to start on every 1- and 2-vCPU VPS. If you have a real machine, set it:
`APP_CPUS=4` in `.env` is the single highest-value line in this section. It is what the reference
stack runs since 2026-09-10 (the owner's call for the reference box: four at least, even with a shipped
default of 1); measured there against the live app rather than the bench: the 150 s 720p60 sample's
Light copy in 22 s, the whole E2E suite (§17) in 772 s over the tunnel against 859 s at one cpu — the
suite is mostly requests, not encodes, so it moves less than an encode does. With the GPU flavour on top
(the section below; the reference stack's shape since 2026-09-10) the same Light copy takes 14 s.

`TRANSCODE_CONCURRENCY` stays `1`. Past about `APP_CPUS/8` it trades latency for throughput, and a share
link wants latency. Raise it only if you routinely queue several uploads at once, and keep
`TRANSCODE_CONCURRENCY <= min(cores, RAM_GiB)` (§7). Peak RSS per slot measured **0.61 GiB** worst
case — set by the SOURCE's resolution, not the output's — so `APP_MEM_LIMIT=2g` has room for two.

**`TRANSCODE_THREADS`: leave it at `0`.** `nproc` inside a container reports every core the HOST has,
whatever `cpus:` says, because a CFS quota is a share of time and not a cpuset. So x264 used to size its
frame-thread pool for sixteen cores and then get throttled to one: measured **25 % slower** than the
same encode pinned to the quota, with output 2-5 % *larger*. Auto reads the container's own
`cpu.max`/`cpu.cfs_quota_us` and pins `-threads` to it; the boot line prints the number
(`threads=4`). Set a number only when the arithmetic cannot know — a bare process with no cgroup, or a
`cpus:` you raised for something other than encoding.

**Nothing else in the pipeline is worth tuning.** For the record, so nobody re-measures it:

- **`TRANSCODE_PRESET` for the Light copy: `veryfast` is right.** `superfast` is 13-21 % faster and
  `ultrafast` 24-44 %, and they cost **+81 % bytes** on the worst-case sample *at the same VBV
  ceiling*. On clean content `ultrafast` needs 2.5× the bytes for +3.5 VMAF and lands at 2.68 Mbps —
  over the card's own ceiling, which is the failure the ceiling exists to prevent.
- **The card's copy and the faithful copy come out of ONE ffmpeg.** Running them as two processes at
  once is *worse* than running them back to back: both decode the same source, and the first file
  appears **2.2× later** than it would alone. One process with two outputs costs 1.05-1.42× the Full
  alone instead of 1.6-1.9×, and both files are durable the moment the clip is ready.
- **A remux is 270× faster than an encode** (0.27-0.64 s against 43-173 s), which is why the fast path
  exists at all.
- Chunked parallel encoding (9 % at four chunks, *negative* past that, a visible seam at each join) and
  bilinear scaling on a real rendition are not built, and will not be.

### The GPU flavour (`TRANSCODE_HWACCEL=cuda`)

An NVIDIA card changes the order of magnitude and nothing else can: a full CUDA path — hardware decode,
`scale_cuda`, `h264_nvenc` — measured **9-11× libx264 at ONE cpu** and is nearly core-count
independent, so a 1-vCPU box with a card beats a sixteen-core box without one. Swapping only the encoder
(CPU decode, CPU scale, NVENC) is 1.4-3.6× and is not worth a second image, because the decode and the
lanczos are ~82 % of that pipeline.

It is **opt-in, off by default, and a different image**. The ordinary image ships a static ffmpeg with
no NVENC encoder at all, and `TRANSCODE_HWACCEL=cuda` on it is a boot error naming this section. The
GPU image is built from your checkout: `compose.gpu.yaml` carries its own build block, so it builds
whether or not `compose.build.yaml` is on the list. To pull it instead, set
`CLIPPET_GPU_IMAGE=ghcr.io/clippet-gg/clippet:gpu-<version>` and run `docker compose pull` before step 3.

```sh
# 1. the host: the NVIDIA driver and the container toolkit. Prove it before anything else:
docker run --rm --gpus all nvidia/cuda:12.6.2-base-ubuntu24.04 nvidia-smi

# 2. .env — three lines, and nothing else changes
cat >> .env <<'EOF'
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.gpu.yaml
TRANSCODE_HWACCEL=cuda
EOF

# 3. build the GPU image and start it
docker compose up -d --build
docker compose logs app | head -1            # ... hwaccel=cuda ...
```

`compose.gpu.yaml` is an **override of the one `app` service** — three keys: the build (the Dockerfile
target `gpu`), its own tag (`CLIPPET_GPU_IMAGE`), and `gpus: all`. The service keeps its name, its port, its
volumes and its healthcheck, so every habit and script that says `app` (`exec app`, `logs app`, the
E2E's own `up`, §16's upgrade checklist) is the GPU build from then on; without the `COMPOSE_FILE`
line an operator never resolves the file, never pulls the CUDA base images and never asks the daemon
for a device. `COMPOSE_PATH_SEPARATOR=:` keeps the two-file list portable: Windows would otherwise read
it with `;`. (The first shape of this — an `app-gpu` sibling service in a compose *profile* — is gone:
a profile-only twin on the same port and data volume left every plain `docker compose up -d` free to
start the CPU container against it, which is exactly what the E2E and the upgrade checklist do.
DESIGN §20 erratum 247.)

It uses **`gpus: all`, not `deploy.resources.reservations.devices`**. The latter is Swarm-only and
`docker compose up` ignores it *silently* — the same trap the `mem_limit`/`cpus` comment in
`compose.yaml` records.

**Most of the filter graph cannot run on the card, and that is handled rather than refused.** A tone
map, a deinterlace, a pre-rotate and a square-pixel correction are CPU-only filters, so an HDR,
interlaced, tag-rotated or anamorphic source falls back to the ordinary CPU argv **on the same
binary**, silently and per clip. A phone's ordinary rotation — a Display Matrix, not a legacy tag — stays on
the card: the input is read with `-display_rotation:v 0`, `scale_cuda` scales to the coded orientation, and the
quarter turn runs on the CPU after `hwdownload` (erratum 352; only a matrix that also MIRRORS the picture falls
back). So does a source the hardware decoder cannot take — NVDEC decodes
8-bit 4:2:0 H.264/HEVC/VP8/VP9/AV1/MPEG-2/MPEG-4/MJPEG/VC-1 and nothing else, so a capture card's
10-bit High 10 H.264, ProRes, DNxHD or a 4:4:4 master stays on the CPU (erratum 247: with the card asked
for, ffmpeg decodes such a stream in software without a word and `scale_cuda` then fails the run). The per-clip `planned` log line says which ran and why:

```
msg=planned … hwaccel=cuda hwaccel_why=""
msg=planned … hwaccel="" hwaccel_why="the source is interlaced and yadif is a CPU filter"
```

**And a source the hardware decoder refuses at RUN time falls back too, mid-job.** Those rules are
decided before a frame is read, and they are not the whole story: NVDEC also refuses individual
streams for reasons nothing in a probe predicts. A real one is an NVIDIA ShadowPlay capture — plain
8-bit 1440p60 H.264, High profile, progressive, which every rule above admits — whose decode dies in
a second with `cuvidCreateDecoder … failed` and a zero-byte file. **The clip does not fail.** The
moment ffmpeg exits that way without having written anything, the app re-runs the same stage with the
decode on the CPU and the encoder still on the card: one line in the log, the same attempt, the same
output, and the clip goes ready as usual. The line is

```
level=WARN msg="the hardware decoder refused this source; re-running the stage with the decode on the
  CPU and the encode on the card (DEPLOY.md §27)" clip=… job=… kind=transcode stage=encode
  detail="[h264 @ …] Failed setup for format cuda: hwaccel initialisation returned error."
```

That is the line an install with **no GPU device at all** leads with — there is no `cuvid` line
before it to lead with instead. On a real REFUSED source (a working card that NVDEC declines this
particular stream on, §20 erratum 289(b)) the line printed is NVDEC's own, earlier in the same
buffer:

```
level=WARN msg="the hardware decoder refused this source; re-running the stage with the decode on the
  CPU and the encode on the card (DEPLOY.md §27)" clip=… job=… kind=transcode stage=encode
  detail="[h264] decoder->cvdl->cuvidCreateDecoder(&decoder->decoder, params) failed -> CUDA_ERROR_INVALID_VALUE: invalid argument"
```

Same WARN, same recovery, same "the clip does not fail" — `detail` is just whichever signature line
came first in that run's captured stderr, and either one means the fallback already ran.

It costs speed and nothing else: 30 s of that source into the card copy's shape, at four cpus, took
11 s on the fallback against 14 s for the pure-CPU encode — still the faster of the two, and well
short of what a working hardware decode gives. If you see that WARN on *every* clip **and the clips
then fail anyway**, the container has no GPU device at all rather than an awkward source: ffmpeg says
the same words for both, and NVENC needs the very device the decode could not reach, so nothing is
left to fall back to. `docker compose exec app nvidia-smi` is the test — it lists your card in a
healthy GPU container. DESIGN §20 erratum 265.

**If you ever saw a failed clip whose reason opened mid-word — `ed error.` and then `Impossible to
convert…` — that was this bug and it is fixed:** before DESIGN §20 erratum 289 the run-time test read
only the last kilobyte of ffmpeg's stderr, which a refused decode's own shutdown runs past, so the
WARN above never appeared and the clip failed on a card that could have decoded it on the CPU; since
289 the test reads the whole capture, that line is printed, and the clip lands.

`FULL_CODEC=av1` also stays on the CPU: `av1_nvenc` is the fastest encoder on the card and no card
platform plays AV1, so it is not built.

**Quality is calibrated, not assumed.** NVENC's quantizer is not x264's — at the *same* number it writes
1.8-4× the bytes — so each tier has its own offset, measured against the 60 s 1080p60 sample:

| tier | x264 | bytes | NVENC | bytes | difference |
|---|---|---|---|---|---|
| **Full** | CRF 18 | 238.7 MB | **`-cq 22`** | 232.9 MB | **-2.4 %** |
| | | | `-cq 24` | 183.0 MB | -23 % (visibly cheaper) |
| **Light** | CRF 26 | 10.06 MB | **`-cq 34`** | 11.38 MB | **+13.2 %** |
| | | | `-cq 32` | 14.64 MB | +45.6 % |

Those calibration numbers are HOST-ffmpeg numbers (`h264_nvenc -preset p4 -tune hq -rc vbr -cq N -b:v 0`)
with the app's own argv; the byte sizes reproduce in the container. **The container proof** (2026-09-10,
DESIGN §20 erratum 247): on the same box the 60 s 1080p60 sample inside the `gpu` image takes **12.3 s
for the Full tier and 8.3 s for the Light copy**, against **29.0 s and 12.7 s** for libx264 at four cpus
in the ordinary image — 2.4× and 1.5×, not the 9–11× measured host-native at one cpu: Docker Desktop's
WSL2 GPU path costs more per frame than a native driver, and four cores are a fair opponent. It is still
the fastest this box can be, and the gap widens with resolution. **Verify your own instance the first
time**: upload one clip and check that `admin ls <id> --stat` shows a rendition of a sane size and that
the watch page plays it.

**The trap that hid behind a driver update.** The CUDA base image asks the container runtime for the
`compute,utility` driver libraries only; NVDEC and NVENC (`libnvcuvid.so.1`, `libnvidia-encode.so.1`)
are behind the **`video`** capability, and without it every hardware decode dies with `Cannot load
libnvcuvid.so.1` while `nvidia-smi` and the boot capability check both pass (ffmpeg *lists* the
encoder whether or not the driver library it opens at run time is present, so no boot check can
catch this — the first encode's failure line is the tell). The `gpu` image now sets
`NVIDIA_DRIVER_CAPABILITIES=compute,utility,video` itself; there is nothing to configure.

**One trap, because it costs an afternoon.** `-colorspace bt709` on the output **cannot survive a CUDA
graph**: it forces buffersink colour negotiation, ffmpeg inserts a CPU scaler the CUDA frames cannot
reach, and the run dies with a zero-byte file. The CUDA argv therefore writes the Rec.709 stamp with the
`h264_metadata` bitstream filter instead — same three code points in the SPS, no pixel touched, and the
output probes `bt709` exactly as the CPU one does.

### Audio clips (DESIGN.md §20 erratum 328)

A clip can be SOUND: a file with no picture (an attached cover does not count) and one audio stream. The
upload is never stored as it came. It is STRIPPED first: its one audio stream is copied (`-c copy`, not
re-encoded) into a fresh container with every tag, chapter, cover picture and encoder string left behind —
a WAV's `bext` and `iXML`, a phone's location atom, an MP3's ID3 and cover, an Ogg's comments — and for PCM
the decoded samples are hashed before and after to prove not one changed. What is stored, served and
downloaded (the owner's "Download original" included) is that strip. Then:

| the stored file | Original (plays at `/audio`) | Full | the card copy |
|---|---|---|---|
| MP3 | `Original · MP3` | — | always |
| AAC (an `.m4a`, or an audio-only `.mov`/`.mp4`, stored as `.m4a`) | `Original · AAC` | — | always |
| Opus or Vorbis in Ogg (a voice note), or in WebM/Matroska (a browser recording; stored as `.ogg`) | `Original · Opus` (`· Vorbis`) | — | always |
| FLAC (up to 24 bits) | `Original · FLAC` | — | always |
| WAV, W64, AIFF, CAF (integer PCM), ALAC, WavPack, TTA | download only | `Full · FLAC`, bit-exact (hash-proven) | always |
| 32-bit float WAV (or integer past 24 bits, a 32-bit FLAC too) | download only | `Full · FLAC 24-bit`, lowered just under full scale when its peak is above it | always |
| WMA, AC-3, AMR, MP3/AAC in another box | download only | `Full · AAC` 256k | always |
| DSD (`.dsf`/`.dff`), Monkey's Audio, any codec in Matroska/WebM but Opus, Vorbis, FLAC, ALAC or PCM | refused (415) | | |

The card copy is the AUDIOGRAM (below) under the sound, H.264 + AAC (256k, fitted down toward `SOCIAL_MAX_BYTES` for
a long file, never below 96k — DESIGN §20 erratum 329). Past roughly 25–30 minutes of sound even 96k does not fit: the
card copy is then larger than the cap and Discord shows the bare link, and the clip plays in the page's own player.
It is what `/video.mp4`, `/light.mp4` and every card tag serve for an audio clip:
Discord plays no audio link, only a video. An audio clip has no trim, GIF, chosen card frame or replacement
(they answer `not_for_audio`). Nothing to configure; storage, TTL and the quota count an audio clip's objects
like any other's.

### The audio card: the waveform and the audiogram (DESIGN.md §20 erratum 329)

An audio clip's picture is a DRAWING of its sound, made by the server from the clip itself: one decode reads
its loudness, and 70 rounded bars mirrored about a centre line are drawn on the ink tile (`#17181C`) in grey
(`#A9A59C`), each as tall as its stretch of the clip is loud — the loudest stretch a full bar, a quiet one
short, silence a dotted line. Nothing is taken from the file's tags or its cover picture. A bar is the energy
of its stretch (RMS, both channels of a stereo file), not its single loudest sample, so a track with a hard
transient on every beat still shows where it gets louder and quieter (§20 erratum 332; clips made before it
keep the bars they were drawn with).

- **The poster** (`poster.jpg`, which for an audio clip is a **PNG** served as `image/png`, 1280×720) is that
  drawing, with the clip's length in its top-right corner (as every poster has, §22) and the mark in its
  bottom-left. It is the card's image, the library's tile and the picture above the share page's player.
- **The audiogram** (the card copy) is the same bars as a 30 fps H.264 video, the played bars turning amber
  (`#F0B24A`) left to right over the clip's length, under the sound — so the card Discord draws plays the
  clip with its own waveform filling as it goes. A keyframe every 5 s; the sound is AAC 256k at 48 kHz, or an
  AAC upload carried as it is when it fits; no loudness change anywhere. The picture is ~120 kbps. It is the
  one encode an audio clip really pays for: about 28 s of work per minute of sound at `APP_CPUS=1` (measured,
  the CPU image), and it shares the worker's slot and thread count with every other encode.
- **The share page** lays the same bars over the poster in amber as far as the `<audio>` has played (no
  library, nothing fetched), and gives the phone's lock screen the clip's title and picture (the browser's
  Media Session — local, no call anywhere). The embed (`/embed/{id}`) plays the audiogram.
- **The card tags** add `og:audio` (`/audio`, under its real type) beside the audiogram's `og:video`; iMessage
  reads it, Discord ignores it.

## 28. The look: light and dark, the fonts, the visual pass

The interface is the visual system "Kept" (`web/STYLE.md` is the one-page description; DESIGN.md §20
erratum 240 binds it). Three things an operator may meet.

**Light and dark.** Both are first class and the default follows the operating system — nothing to
configure. There are two controls (§20 erratum 288). The **glyph at the right-hand end of the top
bar** — a sun on a dark page, a moon on a light one, each naming the theme it gives you — is on every
page that has a bar, a share or collection page a visitor with no account is looking at included; and
**Settings → Look** has the three-way picker (System / Light / Dark), which is the only place `System`
can be chosen again. The choice is a cookie named `theme` (`light` or `dark`; absent = the system),
`Path=/`, `SameSite=Lax`, `Secure` on an https origin, valid a year, and it belongs to that BROWSER,
not the account — there is no column, no migration and nothing to back up. The page handlers read it
and render `data-theme` on `<html>`, so a pinned theme paints with the first byte (no flash, and no
inline script — the CSP has none). Both controls work without JavaScript: the form posts to
`POST /settings/theme`, which needs no session — the glyph is on pages nobody has signed in to — and
comes back to the page it was pressed on. Nothing about a visitor is recorded either way: one cookie,
one browser, no row. Discord's card edge and the mobile browser chrome
follow the same two grounds (`#ECEDEF` light, `#191816` dark) through the `theme-color` tags.

**The bar down the left of a Discord card follows the CLIP, not the theme.** A share page carries one
extra, unqualified `theme-color` tag after that pair, and it is green (`#0E7C4F`) when the clip is
ready, amber (`#D98E12`) while it is queued or encoding and red (`#F0665C`) when it failed — the same
colour law the interface uses, on the one surface it had never reached. Nothing is served differently
to anybody: a browser takes the FIRST `theme-color` whose media query matches, which is the pair above,
and Discord takes the LAST one on the page, which is this. That last-wins behaviour is measured rather
than documented, so if Discord ever changes it the bar goes back to a neutral colour — a card keeps
working either way. Only a clip's own page carries it; the front door, the library, Settings and a
collection page carry the pair alone. Nothing to configure.

**The page declares its own Discord card** (§20 erratum 316; `CARD_COMPONENTS`, on by default).
Discord can draw a link from a layout the PAGE declares — a "component embed": one
`<script id="discord:component-embed" type="application/vnd.discord.component-embed+json">` data block
in `<head>`, JSON, at most 3,000 bytes. A clip's page declares one: the mark beside the instance's
host, the title (a link to the clip when it holds no markdown character — Discord prints escapes
literally inside a link), the length and the quality line ending in **at this link** as a link, the
playable `social.mp4` (the same file `og:video` names), the state's colour as the accent and one
**Open** button. Measured on a real channel on 2026-10-05: a pasted link draws it on desktop, and the
video plays inside Discord. Everything the card had before stays on the page unchanged — it is what Discord shows when
it cannot draw this one. Nothing else changes: no new route, no new request, no change to the CSP (a
data block is never executed). Discord's documentation for the format is new and says it is subject
to change; `CARD_COMPONENTS=off` takes it out, and Discord's Embed Debugger
(`discord.com/developers/embeds`) shows what Discord reads from any link. A clip whose title is long
enough to push the block past 3,000 bytes simply carries the ordinary card. Behind a Clippet Pass
name, the plane's guard must know the element before a release with it is served through it (it
refuses any inline `<script>` today).

**The fonts are inside the binary.** Spline Sans and Spline Sans Mono (SIL Open Font License; the
licence text ships beside the files as `/static/fonts/OFL.txt`) are two ~50 KB variable woff2 files
embedded like every other static asset, served from `/static/fonts/` with the same one-hour cache and
ETag — and with the same `?v=<build id>` on the URL, written into the stylesheet as it is served, so an
upgrade that changes a face is never answered from a stale cache — and named by the
Content-Security-Policy's `font-src 'self'`. The running app fetches no font
from anywhere — the §15 promise (the binary talks to your bucket and, if you set it, Discord, and
nothing else) is unchanged. A browser that blocks web fonts, or has not loaded them yet, falls back to
Segoe UI / Roboto / the system face; nothing in the layout depends on the webfont.

**The visual pass.** `bash scripts/visual.sh` photographs every page of a RUNNING stack — the one
this checkout's `.env` names, or `BASE=<origin>` — in both themes at 1280 and 390 wide, the embed at
400 × 225 as well and once more paused after a play, with the
official Playwright container (`mcr.microsoft.com/playwright:v1.48.0-noble`, ~1.5 GB on first pull; no
Node on the host; the `playwright` package is installed once into a `clippet-pwmods` volume), and
asserts on each page: zero console errors, zero CSP violations, zero requests to any origin but the
stack's, no horizontal overflow at 390, the faces loaded from `/static/fonts/`, the ground painted
the theme asked for, and the embed overlay's box inside the frame and above the controls band —
including that the overlay's own text names this instance's host (erratum 287): the wordmark lockup on
`clippet.gg` and its subdomains, plain text everywhere else, with the right accessible name either way.
The screenshots and a contact sheet land in `.e2e/visual/index.html`
(gitignored); there are no committed baselines yet — what a page should look like is a decision a
person makes on the sheet. It registers a throwaway account (`visual_<epoch>`; there is no user-delete
route, `admin users prune` removes it later) and deletes the clip and collection it made. It is
deliberately not a `check.sh` stage: it needs a live stack and a browser image, which the
container-only CI story has neither of. The gate (`scripts/gate.sh`, T2) runs it when `web/` moved since
the last green gate, and a lane's `scripts/t1.sh` runs only the phases its touched pages need
(`VISUAL_ONLY`; DESIGN.md §20 erratum 331).

**The companion mark.** The wordmark is the logo and is the only mark on any page; where a word cannot
go, the instance carries its companion — the striped clapper (`docs/brand/README.md`; DESIGN.md §20
errata 301 and 306): the favicon in the tab (the mark itself, stripes and all, ink and green following
the browser's own
light or dark preference), the icons an installed app gets on a phone (`/static/icon-192.png`,
`icon-512.png`, the mark on an ink tile), the picture Discord shows on a card while a clip is still
encoding (`/static/card-placeholder.png`: the mark large, the wordmark small under it, on the dark
ground), and the Discord application's own icon, which you upload by hand (§7.1). All of them are drawn
by `scripts/lib/mkicons.py` and `scripts/lib/mkcard.py` and checked against them by `scripts/check.sh`
(§17.2); nothing about them is configurable, and nothing on a page moves because of them.

**`/favicon.ico`.** Since §20 erratum 304 that path answers the mark as one ICO holding 16, 32 and 48 px
drawings, where it used to answer the 404 page. No browser looking at a page ever asks for it — every
page declares its own icon, and that one follows the theme — but bookmark tools, some link previewers
and Safari's pinned-tab fallback ask for the path without reading a page, and used to get HTML. It is
cached for a day and there is nothing to configure. Behind a Clippet Pass name (§36) the plane's guard
serves it with its own static policy instead, an hour; the picture and the validator are the same.

**The front door.** A visitor with no session who opens `/` — someone who followed the wordmark on a
share page, or typed the origin — gets a page, not a login form (DESIGN.md §20 erratum 246): the
wordmark, one line ("Upload a clip. Get a link."), a **Log in** button and, only
while `REGISTRATION` is open (§19.1 — `open`, or `first` before the first account exists), a
**Create an account** link. Nothing else is on it: no feature list, no screenshots, no about or repo
link, no analytics. A signed-in browser at `/` still lands on the library (§20). Nothing to
configure; the page is indexable and carries no clip data.

**The wordmark on the embed — and your own hostname.** `/embed/{id}` — the player X frames for a card
and the one `Copy embed` hands out — carries a small mark in its top-LEFT corner, on the poster and
while paused, hidden while the video plays (it was top-right until the poster's own duration label took
that corner). Since 2026-09-15 it spells **your instance's own host**
(the hostname of `PUBLIC_BASE_URL`, with no scheme and no port), so somebody who likes the player on
somebody else's page can read where it came from. On `clippet.gg` and its subdomains that host contains
the wordmark, so the lockup is intact; on a host of your own it is your hostname in the same face and
size, without the green underline — that underline is this product's logo and it does not go under
someone else's domain. It links to the clip's share page (out of the iframe, into
the top window), where the header wordmark leads to the front door above. It never covers the native
controls at card sizes (400 × 225 and up), there is no text beside it, and nothing about the embed's
headers, card tags or oEmbed document changed. Discord's own client plays the `og:video` file inline
without this page, so there the only brand is the Light copy's corner tag (§27). Nothing to configure.

## 29. Links: what a share URL looks like, and the two id grammars

A share link is **`{PUBLIC_BASE_URL}/{id}`** — the id at the root of your origin, with nothing in
front of it. On a domain of your own that is `https://clips.example.com/PurpleUpdateCharacter` or
`https://clips.example.com/aZ3kQ9xB2mLp`, which is as short as a link on that domain can be. Nothing
here is configurable, and there is no setting to turn any of it off.

**Every link you have already sent keeps working.** The address before this release was
`/clips/{id}`, and it answers a **308 permanent redirect** to the new one: the same page, one hop
later. 308 rather than 301 because it preserves the method — the unfurlers and uptime checkers send
`HEAD`, and a 301 lets a client turn that into a `GET` — and Discord, X, Slack and Facebook all
follow permanent redirects when they unfurl, so a card pasted a year ago still resolves to the page
with the tags on it. The redirect reads no row: an id no clip has redirects exactly as a real one
does and lands on the same 404, so it tells nobody which links exist. It keeps the query string and
is publicly cacheable for an hour — except on the occasional response that is also re-issuing a
signed-in browser's session cookie, which is marked uncacheable instead, so no shared cache can store
one person's cookie and hand it to the next caller. `/embed/{id}` and `/c/{id}` are unchanged.

**Nothing in a reverse proxy needs to change.** The Caddy and nginx recipes in §26 pass the whole
origin through (`reverse_proxy` on the site block, `location /`), which is the only shape that ever
worked here — the app owns `/api/`, `/static/`, `/healthz` and the pages, and a proxy that routed
only `/clips/` would already have broken the embed. If you have written a rule of your own that
matches a path prefix, this is the release to check it: a share link no longer has one.

**Two id grammars, chosen per upload.**

| Grammar | Looks like | Entropy | When |
|---|---|---|---|
| `compact` | `aZ3kQ9xB2mLp` — 12 characters | ≈71 bits | **The default.** Every clip before this release, and every clip since unless somebody chose otherwise |
| `memorable` | `PurpleUpdateCharacter` — three words | ≈39 bits | A link somebody will read out, type from a phone, or say on a call |
| `memorable`, unlisted | `LanceFaceliftOverlordStrewn` — four words | ≈52 bits | Automatically, when the upload is **unlisted**: there the link is the only authentication |

The words come from the EFF long word list, shipped inside the binary and curated to the 7,772
entries that are three to nine lowercase letters. (Word list: EFF large wordlist, CC BY 3.0.) They
are drawn from `crypto/rand` with the same rejection sampling the 12-character ids use, so no word
is more likely than another.

**Who decides.** Every account has a default (`/settings` → *Upload defaults* → *Links*, or
`PATCH /api/auth/me/settings` with `default_link_style`), shipped as `compact`. Any upload may
override it: the browser form has the same two-option control beside *Delete after* — both carry a
hint showing one example link of each grammar on this instance's own host (§20 erratum 264) — and both API
routes take a `link_style` field (`POST /api/clips` in the metadata JSON, `POST /api/upload` as a
text field). **A capture tool that sends no field follows the account** — the `clippet.sxcu` this app
mints has no argument for it, so switching your default in Settings switches what ShareX produces
without reconfiguring ShareX.

**What memorable costs.** Thirty-nine bits is not seventy-one. An unlisted clip's link is its only
authentication (§20 — and it always has been, whichever grammar it is in), which is why an unlisted
upload takes four words instead of three. (A clip's own page used to tell its owner as much in one
line; §20 erratum 264 withdrew that line — the choice is explained where it is made, by an example of
each grammar beside the control.) There is no rate limit on the difference and none is wanted: guessing a three-word link is a
public clip's problem, and four words is the answer for the case where it would not be. If that trade
is not one you want, leave the account default at `compact` — it is what a fresh install ships.

**An id is permanent.** Making a clip unlisted after the fact does *not* re-mint it: the id is an
address, and every link already sent names it. A clip minted before this release is `compact` and
stays so; nothing is rewritten, and migration `0015` adds one column to `users` and touches no clip.

**`/robots.txt` changed shape.** It used to close `/clips/`, `/c/` and `/embed/` to general crawlers;
with the id at the root, a prefix list closes nothing, so it is now `Disallow: /` with `Allow` for the
front door, `/login`, `/register` and `/static/`. The four unfurler groups (Discordbot, Twitterbot,
facebookexternalhit, Slackbot-LinkExpanding) keep their empty `Disallow:` — that is what keeps cards
working — and neither `robots.txt` nor `noindex` is a security control: the id is. The file reads
as a closed site because this is one: everything else on it is somebody's private link.


## 30. The Discord bot: `/upload`, `/clip` and Enrich in your own server

Optional, off by default, and a **different feature from the webhook of §11**: the webhook posts a
ready clip *out* to a channel you chose, the bot answers commands *in* a server somebody ran them in.
Either can be on without the other. With the bot off, its route does not exist — the path answers the
ordinary 404 — and the binary opens no socket it did not open before.

What it does, in full — four commands (DESIGN.md §20 errata 315, 318):

- **`/upload`** with a video attached: the bot uploads it and posts the clip's message (below); a form for the title, the delete-after and the tags opens meanwhile (§30.5). (This was `/clip` until 2026-10-05; it is the same command under its own name.)
- **`/clip`** with a **link** to a clip already on your Clippet: the bot posts that clip's message at
  once. The link is read, never fetched.
- **Enrich**, on a message's **Apps** menu (right-click, or long-press on a phone): the bot posts the message for the first link to a clip on your Clippet in the message it was used on. With `DISCORD_ENRICH_UPLOAD=on`, and no such link but a video attached, it uploads that video as `/upload` does, without the form — for whoever posted the message, or a member with Manage Server (§30.4).
- **`/settings`**, for members who may manage the server: a panel, visible only to whoever ran it, whose
  **Edit** opens the connected account's link defaults — how long a clip is kept, whether it is listed,
  the link's shape — and saves them (§30.9).

**The message** is the same for the first three: the share link alone. Discord unfurls it into the card the
clip's own page declares (§28, `CARD_COMPONENTS`): your instance's host and mark, the clip's title,
its length, the video playing inside Discord, the state's colour down the edge (green ready, amber
working, red failed) and an **Open** button — the same card anybody who pastes the link gets. (Until
2026-10-05 the message carried an embed of its own as well; Discord never unfurls a link in a bot
message that carries one, so it hid the card.) That is the whole feature. It answers no other command, needs no
privileged intent and no permission in your server beyond "use application commands", and reads no
message except the one somebody picks Enrich on (§30.6).

### 30.1 What to create in Discord (about two minutes)

1. **Developer Portal → Applications → New Application**, called whatever you like ("Clippet").
2. **General Information**: copy the **APPLICATION ID** and the **PUBLIC KEY**. Neither is a secret —
   the id is in every request Discord sends, and the public key only *verifies* signatures.
3. **Bot → Reset Token → Copy**. This one **is** a secret and is shown once. It is used for exactly
   one thing: publishing the three commands in a server. If you lose it, reset it and re-run
   `clippet admin bot register`.
4. **Installation** (or **OAuth2 → URL Generator**): scope **`applications.commands`**. The `bot`
   scope is *not* required for this feature — nothing here reads messages, posts as a member or needs
   a permission — but adding it with **no permissions ticked** is harmless if you want the app to show
   as a member of the server. Install the app into your server.
5. **Copy your server's id**: Discord → Settings → Advanced → **Developer Mode** on, then right-click
   the server → **Copy Server ID**.

This application can also carry **Sign in with Discord** (§19.6): its client id is this same
application id, and its client secret (OAuth2 → Reset Secret) plus one redirect URL are the whole of
the extra setup.

### 30.2 What to put in `.env`

```sh
DISCORD_APP_ID=123456789012345678          # General Information → APPLICATION ID
DISCORD_PUBLIC_KEY=<64 hex characters>     # General Information → PUBLIC KEY
DISCORD_BOT_TOKEN=<the token>              # Bot → Reset Token (a SECRET: .env is gitignored)
```

**All three or none.** One of them set without the others is a **boot error** that names all three,
on purpose: a bot with no token publishes no command and a bot with no public key refuses every
interaction, and both look like Discord's fault from the outside. Then `docker compose up -d`.

The boot log prints one line — `discord bot configured app_id=… upload_command=/upload command=/clip
enrich_command=Enrich settings_command=/settings upload_form=on interactions_url=… attachment_hosts=…` — and never the
token. `clippet admin doctor` prints the same facts under the `bot` check, including the four names and
the endpoint URL to paste, and warns when
`PUBLIC_BASE_URL` is loopback or plain http, because Discord POSTs from its own network and cannot
reach either.

**Four optional variables: what the commands are called.**

```sh
DISCORD_UPLOAD_COMMAND_NAME=upload-dev     # default: upload    — /upload file:
DISCORD_COMMAND_NAME=clip-dev              # default: clip      — /clip link:
DISCORD_ENRICH_COMMAND_NAME=Enrich (dev)   # default: Enrich    — the message's Apps menu
DISCORD_SETTINGS_COMMAND_NAME=settings-dev # default: settings  — /settings, the link defaults (§30.9)
```

One Discord server holds one command per name, and an instance answers the names it publishes, so
**two instances that share a server need two sets of names** — a dev box on `/upload-dev`, `/clip-dev`
and `Enrich (dev)` beside the production box on the defaults is the shape this exists for. Everything
else about the commands is unchanged by a rename: same options, same descriptions, same refusals, which
name whichever command they belong to. (`DISCORD_COMMAND_NAME` named the upload command until
2026-10-05; it names `/clip link:` now, and the upload command has its own variable.)

The rules for the three slash-command names, and each is a boot error rather than a surprise later:

- **Discord's own**: 1–32 characters of lowercase letters, digits, `-` and `_` — no spaces, no
  uppercase, no leading slash. Discord rejects a registration that breaks this, which you would
  otherwise meet minutes later as "the command never appeared in the picker".
- **Whitespace AROUND the name** is refused rather than trimmed away. The rule above is about the
  characters *in* the name; this one is about the ones beside it. `DISCORD_COMMAND_NAME=clip `
  with a trailing space is a boot error — `must carry no leading or trailing whitespace … "clip "
  is not "clip"` — because the name is registered exactly as written, and the day the space is
  the typo rather than the name, a silent trim would have hidden it. A value that is **only**
  whitespace is "unset" and you get the default `clip`, like every other variable in this file.
- **Set with the bot off** — the three variables above unset — is refused: nothing would ever read it.
- **Any name but the default on the official bot** (`DISCORD_BOT_TOKEN=managed`, §30.8) is refused:
  that application publishes `/upload`, `/clip` and Enrich globally, so a renamed managed instance would
  be reached by nothing under the new name, and answer "Clippet does not know that command" to the
  interactions that did arrive.
- **They must differ.** `DISCORD_UPLOAD_COMMAND_NAME=clip` with `DISCORD_COMMAND_NAME` unset is
  refused — `must differ from DISCORD_COMMAND_NAME: both name /clip, and one Discord server holds one
  command per name, so the upload command and the link command would replace each other` — and
  `DISCORD_SETTINGS_COMMAND_NAME` must differ from both of the others the same way.

`DISCORD_ENRICH_COMMAND_NAME` is a **message** command's name, which Discord lets carry uppercase and
spaces: it is checked for length alone (1–32 characters), plus the whitespace-around, bot-off and
official-bot rules above. It may share a word with a slash command — Discord keeps the two kinds apart.

**Re-run `clippet admin bot register` after changing any of them**, for the same reason as after
changing `PUBLIC_BASE_URL`: the names live in the commands *Discord* stores. That call replaces the
server's whole command set, so the old names go with it — and until you run it, the old commands are
still in the picker and this instance answers nothing they send.

**Two switches: the upload form, and Enrich's upload.**

```sh
DISCORD_UPLOAD_FORM=off                    # default: on — /upload opens a form (§30.5)
DISCORD_ENRICH_UPLOAD=on                   # default: off — Enrich on a video uploads it (§30.4)
```

`on` or `off`; anything else is a boot error. The boot line prints `upload_form=…` and `enrich_upload=…`, and `clippet admin doctor` prints both under `bot`. Nothing to re-register: both are answers, not part of a command.

### 30.3 Connect a server to an account, then paste the URL

```sh
docker compose exec app clippet admin bot bind 222222222222222222 alice
```

That stores "this server uploads into alice's library" **and** publishes the five commands in that
server (`/upload`, `/clip`, Enrich, `/settings` and `/signin`), in one call (guild commands are live immediately; a global command would take up to an hour). Add
`--no-register` to store the binding without the Discord call, and run
`clippet admin bot register <guild-id>` later. The other verbs:

```sh
clippet admin bot ls                       # guild id, account, when it was bound, server|person (§30.8 step 3b)
clippet admin bot unbind <guild-id>        # all five are refused there again
clippet admin bot register [<guild-id>]    # (re)publish the five in one bound server, or all of them
```

Then, back in the portal: **General Information → Interactions Endpoint URL** =
`{PUBLIC_BASE_URL}/api/discord/interactions` → **Save**. Discord sends a signed PING (and a
deliberately *bad* signature, which the endpoint must refuse) before it accepts the URL, so a green
save is proof the whole path works: your tunnel or proxy, TLS, the route, and the key.

Now type `/upload` in the server with a short video attached. A form opens while the upload runs (§30.5); send it or close it, and a few seconds later the clip's message arrives: the link, with the card under it. Then `/clip` with that link answers with the same message at once.

### 30.4 The three commands, their options, and the bigger-files answer

Each slash command takes one option, and it is **required** — Discord will not let anybody send an
empty one. The message command takes none: the message it is used on is its input.

| Command | Option | Type | What it is |
|---|---|---|---|
| `/upload` | `file` | attachment | The video to upload. |
| `/clip` | `link` | text | A link to a clip on this Clippet — `https://your.host/<id>`, the old `/clips/<id>`, the player's `/embed/<id>`, the host without `https://`, or any of them inside Discord's `<…>` or `\|\|…\|\|`. It is **read, never fetched**: a link of any other host is refused without a request. |
| Enrich (Apps menu) | — | — | Posts the message for the **first** link to a clip on this Clippet in the picked message's text. With `DISCORD_ENRICH_UPLOAD=on`, no such link but a video attached: uploads the **first** video, as `/upload` does. |
| `/signin` | — | — | A one-time link, visible only to the person who ran it, that signs their browser in to this Clippet (§30.12). Answers "Discord sign-in is off on this Clippet." unless `REGISTRATION` is `members` or `role:<id>` (§19.1). Its name is fixed; `admin bot register` publishes it beside the others. |

`/clip`, and Enrich on a link, answer **in the channel**, at once, for a clip in any state — a link pasted
mid-encode gets the message right away with `encoding` in it. Three one-line refusals, visible only to
the person who asked: "That is not a link to a clip on this Clippet." (`/clip`), "No link to a clip on
this Clippet in that message." (Enrich), and "That clip is gone." (deleted or expired, either command).
An unlisted clip is posted like any other — whoever holds the link can already watch it.

**Enrich on a video.** Off unless `DISCORD_ENRICH_UPLOAD=on`. When the picked message has no link to a clip here and has a video attached, Enrich uploads that video as `/upload` uploads its attachment — the same checks and refusals, the connected account's defaults and upload budget, the file name as the title — and answers "thinking…" until the clip's message replaces it. No form opens. A link to a clip here wins: the link is answered and nothing is uploaded. Of several videos the **first** is taken; images and other files are skipped. A **forwarded** message's video is not read (Discord carries it in the forward's snapshot, not as the message's own attachment), so Enrich on a forward answers as on a message with no link. **Who may:** the person who posted the message, or a member with Administrator or Manage Server. Your instance decides that on every Enrich, from the interaction Discord signs. Anybody else is told "That video is not yours. You need Manage Server here." and nothing is fetched. Enrich has its own row under **Server Settings → Integrations**: a server that limits `/upload` there has not limited Enrich, and until it does, every member may upload a video of their own with it.

All three commands are guild-only in Discord's own client, so a command that arrived without a server (a direct message) is not a case a person can reach; the bot answers it with the generic "Clippet is not available right now." and nothing more.

**Discord draws a card once per link.** Discord reads a link's page once, when the message is posted,
and never again by itself. So when the bot posts a link whose card is not ready yet — `/upload`'s
link after its wait (§30.5), or `/clip` and Enrich on a clip still encoding — it edits its own message
once the card is ready, to the same link ending `?v=2` (`?v=3` for a second edit), and Discord draws
the card then (the message shows Discord's "(edited)"; the link opens the same page). That needs the
bot's memory of the message: after a restart, or past fifteen minutes from the command, the card
already in the channel stays as it was; a later paste of the link draws the card.

**Enrich's answer is not a reply.** It posts as the next message in the channel ("… used Enrich"),
because an interaction's answer cannot be threaded under another message without the bot token, which
a managed instance does not hold — and Discord's own card on the picked message stays, because the bot
asks for no Manage Messages permission.

There is a limit here that is not ours: **Discord refuses a too-big attachment in its own client, before
anything reaches your server — the limit is Discord's, per account, and Nitro raises it.** There is no
reply the bot can make at that moment, because it never hears about it. So the answer is given in advance, in the
command's own description, which reads

> Upload a clip. You get a link that plays in Discord. Bigger files: *your host*

— upload it on the site, and `/clip` with the link the site gives you posts it in the channel.

**The command definitions are stored by Discord, not read per interaction**, so they carry the options
and the host that existed when the commands were last published. **If you change `PUBLIC_BASE_URL`,
re-run `clippet admin bot register`** — it is a full replacement of that server's command set, so
running it again is safe and is the same command you would run after any upgrade. (A very long host
does not fit Discord's 100-character limit for a description; when it does not, the sentence drops
the host and says "Bigger files go on the site" instead, rather than producing a registration Discord
would reject.)

**Upgrading to this version (2026-10-05):** the upload command is `/upload` now, and `/clip` takes a
link. A server bound before this version still shows the old `/clip file:` in its picker, because the
picker shows what Discord **stored** — run `clippet admin bot register` (every bound server) or
`clippet admin bot register <guild-id>` once, and re-open the picker. Until you do, the old `/clip`
with a video attached still **uploads**, exactly as `/upload` would: a `/clip` carrying an attachment
and no link is treated as the old command. Registering publishes the three commands and re-binds
nothing.

### 30.5 What `/upload` does with the upload, and what it refuses

The clip is an **ordinary clip** in an ordinary library: that account's `ttl_days` and `unlisted`
defaults apply, its upload budget applies (a server running `/upload` in a loop spends the same tokens
that account's own capture tool would — `/clip` uploads nothing and spends none of it, and Enrich spends it only when it uploads a video), the
title is the one a file name gives (§9) unless the form below changes it, and nothing marks it as having come from Discord. The reply waits for the card the way `POST /api/upload` does
(`CAPTURE_READY_WAIT`, §27) so the link unfurls as a player on the first paste. If the card is not
ready when that wait runs out — an audio clip's audiogram, or a long video's Light copy, often is not —
the link is posted then, and the bot redraws it by editing the link once the card is ready, within
fifteen minutes of the command (§30.4, "Discord draws a card once per link"). After a restart, or past
fifteen minutes, the link stays bare, and a later paste of it draws the card.

**The form.** `/upload` answers with a form while the upload already runs: **Title** (filled in from the file name), **Delete after** (default · *what this upload gets otherwise*, 1 day, 7 days, 30 days, 90 days, never) and **Tags** (comma separated). Sending it changes that clip and answers "Changes saved."; closing it changes nothing. While the upload runs, the person who ran `/upload` sees a private "Uploading." (only they see it; it is deleted when the link or a failure line is posted) — the form has no "thinking…". Every five seconds while the file comes in from Discord, that line counts it — `Uploading… 120 of 263 MB` — and with the form off the "thinking…" does the same. The link is posted when the clip is ready either way — nobody waits on the form. A form sent after the link still changes the clip, and when it changes the title the bot edits its own message to the same link ending `?v=2` (`?v=3` when the message was already edited once, for a late card: §30.4), so Discord draws the card again with the new title (the message shows Discord's "(edited)"); the link opens the same page. That redraw needs the bot's memory of the message: after a restart, or past fifteen minutes, the title is still saved but the card already in the channel keeps the old one (§30.4). Tags that break the tag rule are left off, and the answer says so. Whoever may run `/upload` chooses the lifetime of their own upload, `never` included; who may run it is Discord's **Server Settings → Integrations**. `DISCORD_UPLOAD_FORM=off`: no form, `/upload` answers "thinking…", and every upload gets the account's defaults. A form is held in memory for fifteen minutes and takes one answer; after that, or after a restart, it is answered "That form has expired." Nothing typed into it is logged.

An audio file is an upload like a video (§27, "Audio clips"): `/upload` takes one, and so does Enrich, a Discord voice message included.

A refusal the command's first answer can give is one plain line, visible **only to the person who ran
the command**: an unconnected server, a DM, no video, a file whose type is not a video or audio, a file Discord
says is past `MAX_UPLOAD_BYTES`. A failure found AFTER that first answer — while the job runs: the
account over its upload budget, a full scratch disk, a file the probe says is not a video or audio or whose bytes
run past the limit, an attachment Discord's CDN would not serve, a restart — is also one plain line, but
it is **public**: it goes where the link would have gone, as the edit of the "thinking…" or, behind the
form, as a follow-up in the channel.

So what the channel sees is exactly three things: **the clip's message** (the link); with the form off,
**the "thinking…"** that the command's first answer is and that becomes that link or that failure line
(with the form on there is none — the waiting line is the private "Uploading."); and **a failure found
after the first answer**, in the link's place. Everything else — the form, "Uploading.", "Changes saved.",
every refusal of the first answer — is visible only to the person who ran the command. On the official
bot (§30.8) a command's "thinking…" is PUBLIC, and a box slower than Discord's three-second answer window
never leaves it hanging: it ends as the box's answer (the link edited into it; a private line from the box
goes to the person alone and the public "thinking…" is deleted), as one public line — "Your Clippet took
too long to answer. Try again." or "Your Clippet is offline." — or, when the box answered with the upload
form, as "Uploading…" and then deleted, with the link posted beneath when it is ready; a box that is up
but has not answered yet is left to edit it itself. A link already posted on an interaction is edited,
never posted again; only a response lost in transit after Discord stored the link can leave a second one,
and the box logs that case as `follow-up response lost` (DESIGN.md §20 erratum 320 (g)). A button's or the form's own wait is private. (Run 23, the forwarder's lane
`v1-forward`, the official bot's forwarder; before it, that wait could stay in the
channel as a public "thinking…" that never resolved, and a retry could post a second link.)

**A file already here** (DESIGN.md §20 erratum 326). Every `/upload` lands in the connected account, so a file
the account holder uploaded quietly on the web — unlisted, never posted — becomes findable by any member who
holds the identical file: the bot posts that clip's link instead of making a second copy. A duplicate still
uploads in full and spends the upload budget: it saves storage and the transcode, not the transfer. A deduped
upload fires no webhook (§11.1), which is sent only for a new clip.

### 30.6 What it does not do (yet), and the two rules it does not break

- **It answers its four commands, the clicks on `/settings`' own panel and the form `/upload` opens, and nothing else.** "Attachment rescue" — noticing a video somebody dropped in
  a channel and re-posting it as a card — needs a gateway connection *and* Discord's privileged
  MESSAGE_CONTENT intent, and is deliberately not built (DESIGN.md §20 erratum 253 records what that
  transport would have cost). So are per-guild collections, deleting the original message, and any
  command that needs to know who is in a channel. The skeleton exists so the shape of the rest can be
  decided by using it.
- **Egress.** Turning the bot on adds exactly two destinations to the list in §15's promise:
  `discord.com` (the follow-up edit or message and the command registration — the host the webhook was already
  allowed to use) and the attachment CDN, `cdn.discordapp.com` / `media.discordapp.net`. An
  attachment URL that is not on one of those two is refused before a socket is opened, and no
  redirect is ever followed — `/upload`'s attachment and the video Enrich takes alike. A Clippet Auto URL
  (§9) fetches under the same rule, and only an attachment path — at most two at once per account, no
  further than the size Discord stated, with its second bucket per URL (§9, erratum 327). **Nothing else**: `/clip`'s link and the
  links Enrich finds are compared with your own host and looked up in your own database, and a link of any
  other host is refused without a request.
- **Messages.** The bot reads no message unless somebody picks Enrich on it, and then Discord hands it that one message inside the interaction. Its text is scanned for a link to a clip here and dropped. With `DISCORD_ENRICH_UPLOAD=on`, a video and no such link, the author's id is compared with the id of the person who picked Enrich and both are dropped, and the first video is uploaded as `/upload` uploads its own: the file name becomes the clip's title. The access log gets the clip's id or an outcome word, never a person's id, the text or a URL; a failed upload's log line names the file, as `/upload`'s does. Nothing else of the message is kept.
- **Secrets.** The bot token is sent on exactly one route (the command registration) and never
  appears in a log line, an error, the admin CLI or a database row. The interaction token Discord
  sends with every command is treated the same way: for the fifteen minutes it is valid it lets its
  holder rewrite that reply, so it is scrubbed out of everything this app prints.

### 30.7 If it does not work

| What you see | What it is |
|---|---|
| The portal refuses to save the Interactions Endpoint URL | The URL is not reachable from the internet (loopback, an unstarted tunnel, a proxy that does not forward `POST`), or the three variables are not set on the running container — the route only exists when they are. `clippet admin doctor` prints the URL it expects and warns about a loopback origin. |
| "The application did not respond" | Discord never got an answer in 3 seconds: the app is down, or something in front of it is slow. Check `docker compose logs app` for the request; every interaction is one access-log line. |
| `/upload`, `/clip` or Enrich is not in the command list | The commands were never published in *that* server: `clippet admin bot register <guild-id>`. A guild command appears within seconds; try re-opening the command picker (Enrich is under a message's **Apps** menu, not in the slash picker). |
| The picker still offers `/clip` with a **file** and no `/upload` | That server holds the definition of a build older than 2026-10-05: `clippet admin bot register <guild-id>`. Until then the old `/clip` with a video still uploads (§30.4). |
| "This server is not connected to a Clippet account." | Nobody ran `clippet admin bot bind` for it — or the account it was bound to has been deleted, which unbinds every server that pointed at it (deliberately: the alternative is uploading into somewhere unexpected). |
| Every command is refused as unsigned (401 in the log) | `DISCORD_PUBLIC_KEY` is not this application's public key. Copy it again from General Information — it is not the bot token and not the client secret. |
| Every command is refused, 401 with `reject=stale_timestamp` in the log | This machine's clock is more than five minutes off. The app accepts a signed request only within 300 seconds of its own time (DESIGN.md §20 erratum 279 (g)); set the clock to sync automatically. |
| `admin bot register` says `HTTP 401` | The bot token is wrong or was reset. `HTTP 403` instead means the application is not installed in that server with the `applications.commands` scope. |
| `admin bot register` is refused **and** your `.env` has `DISCORD_BOT_TOKEN=managed` | You are on the **official** Clippet bot (§30.8): you never register anything, `/upload`, `/clip` and Enrich are published once by that application, and the refusal says so. Run `clippet admin bot bind <guild-id> <username>` and connect the server on the claim page instead. |
| The command picker still shows the old description, the old host, or the old **name** | All three are stored by Discord when the command is published. Re-run `clippet admin bot register <guild-id>` after changing `PUBLIC_BASE_URL` or any `DISCORD_*_COMMAND_NAME`, or after upgrading, and re-open the picker. |
| A command is in the picker but every use answers "Clippet does not know that command." | That command is not one this instance answers. Either a `DISCORD_*_COMMAND_NAME` was changed without re-running `admin bot register` (the old command is still published), or the command belongs to a *second* instance in the same server. `clippet admin doctor`'s `bot` row prints the names this box answers. |
| A big `/upload` takes a minute before its link appears | Most of it is the file coming in from Discord: every fetch from Discord's CDN logs one `discord fetch` line — `bytes`, `seconds`, `mb_s`, `proto`, `status`, `accept_ranges`, `ttfb_ms` and the `clip` — never the URL (Clippet Auto's fetches log it too). |
| `/clip` answers "That is not a link to a clip on this Clippet." for a link that plays | The link names another host than this instance's `PUBLIC_BASE_URL` (another instance, a typo, a different port), or a route that is not a clip (a collection, Settings). `/clip` compares the host exactly; it never follows the link to find out. |

### 30.8 The official bot: claim your server (no application of your own)

Everything above builds *your own* Discord application. You can skip that: the **official Clippet
bot** — one public application, run by clippet.gg — forwards each command from a server you connect
to *your* instance, and your instance answers it exactly as §30 describes — `/upload`, `/clip`, Enrich, and
`/settings`' **Edit** form (§30.9). No Developer Portal, no
token of your own, no endpoint URL to paste. What the bot's operator learns about you is listed at
the end of this section, because it is short.

**Three steps, in this order.**

1. **Add the official bot to your server** (the "Add to server" button on the claim page, or
   Discord's own install dialog with the same two scopes, `bot applications.commands`, and
   `permissions=0`). It asks for no permission — the bot user joins your server as a member holding
   none, because it never posts as itself — and that membership is what the claim page's second
   check looks for; an install that adds the slash command alone, without the bot user, cannot be
   connected.
2. **Three lines in `.env`, then restart.** The application id and the public key are printed on
   the claim page; they are the *official* application's, and they are public.

   ```sh
   DISCORD_APP_ID=<the official application id>
   DISCORD_PUBLIC_KEY=<the official public key>
   DISCORD_BOT_TOKEN=managed
   ```

   `managed` is **mandatory and literal**: it is the word that tells this instance it answers the
   official bot. The three-variable rule of §30.2 still holds (all three or none), and this is the
   third. `PUBLIC_BASE_URL` stays whatever it already is. A managed instance also keeps the command's
   default names — the three `DISCORD_*_COMMAND_NAME` variables must be unset (or the defaults) here
   (§30.2), because the official application publishes `/upload`, `/clip` and Enrich globally under
   exactly those names.
3. **Consent on the box, then connect.** On your instance:

   ```sh
   docker compose exec app clippet admin bot bind <guild-id> <username>
   ```

   (`--no-register` is implied: a managed instance publishes nothing.) Then open the claim page,
   **Sign in with Discord**, pick the server from the list Discord built, type your instance's
   address (the host of `PUBLIC_BASE_URL`), **Connect**. `/upload`, `/clip` and Enrich work in that
   server from then on, `/upload` uploading into `<username>`'s library.

3b. **Just me** — your own box in a server somebody else connected (§20 erratum 342). The server keeps its
   own Clippet; only *your* commands there reach yours. On your instance, link your Discord user id
   (Developer Mode → right-click yourself → Copy User ID), then bind the server for you alone:

   ```sh
   docker compose exec app clippet admin users link <username> <discord-id>
   docker compose exec app clippet admin bot bind <guild-id> <username> --person
   ```

   Then, in the server, run `/connect` → **Connect my Clippet** and type your instance's address. From
   then on (within a minute) your own `/upload`, `/clip`, Enrich and `/signin` there reach your box, and
   everyone else's still go to the server's Clippet. A person binding is your box's consent for ONE
   member: the bot's check asks `…/api/bot/claim?guild_id=<guild-id>&user_id=<your id>` and your box
   answers 200 (`"scope":"person"`) for your id alone and 404 to anyone else — so nobody can point the
   whole server at your box, and nobody but you can point themselves at it. A command from any other
   member that reaches your box anyway is answered *"Not your Clippet."* and nothing is fetched.
   `admin bot bind` refuses `--person` until the account has a linked id, `admin users unlink` is
   refused while the binding names the account (`admin bot unbind <guild-id>` first), and the same
   `bind` without `--person` makes the server every member's again. `admin bot ls` ends each row with
   `server` or `person`.

**Why step 3 has two halves.** The bind is the consent the claim page checks: before it writes the
route, it asks your instance `GET {PUBLIC_BASE_URL}/api/bot/claim?guild_id=<guild-id>` and connects
only on a **200**. That answer exists only after *you* ran `admin bot bind` on *your* box — which is
the one thing a stranger with a throwaway server cannot do. It discloses one boolean about a guild
id the asker already knows, plus the application id this box is configured for; never the account.
`admin doctor`'s `bot` row prints that GET, together with the servers you have bound.

**Never `admin bot register`, always `--no-register`.** The three global commands are published once
by the application's owner. A managed instance refuses `register` in words rather than sending the
word `managed` to Discord as a token and reading an `HTTP 401` back (§30.7's new row).

**One bucket for every server's `/clip`, and what that means.** The official bot reaches your box
from Cloudflare with a fixed `X-Real-IP` of `192.0.2.1` (TEST-NET-1, a sentinel — never a real
address) on every forwarded interaction. On a box that trusts its proxy (`TRUSTED_PROXIES`, §4),
that header is believed, so **every server you connect shares ONE `bot` bucket**
(`interactionsPerMinute`, 60 a minute) — one server never notices; several busy ones are thin, and
the 61st `/clip` in a minute answers *"Your Clippet is not available right now."* in Discord. It also
means that anyone who can reach this app *through* that trusted proxy with `X-Real-IP: 192.0.2.1`
set can drain exactly that bucket and turn real `/clip`s into that sentence: this is §4's footgun
(a header trusted from the wrong hop), now with a published constant as its target — so trust only
the hop that actually strips and sets the header. Without `TRUSTED_PROXIES` the header is ignored
and the bucket key is the TCP peer instead (the tunnel's or Docker's gateway address), which pools
your servers the same way. `GET /api/bot/claim` is public and anonymous, so it has a bucket of its
**own** (`claim`, 30 a minute per address): a stranger's GETs can never spend the tokens your
`/clip`s need.

**What the claim page can refuse, and what each means.**

| It says | It means |
|---|---|
| "That server is already connected to a Clippet." | The guild id is the key; a second claim is refused outright, never silently rebound. The sentence names whom to write to. |
| "You are no longer an admin of that server." | Proof A: the signed-in account holds neither `MANAGE_GUILD`, nor `ADMINISTRATOR`, nor ownership in that server *now*, whatever the page showed a minute ago. |
| "Clippet is not in that server." | Proof B: the official application is not a member of that server. Add it first. |
| "That address did not answer, or nobody there has run `clippet admin bot bind` for this server." | Proof C: your instance was unreachable at that host, or it answered 404 — the bind above was not run for *this* guild id. Two causes, two fixes. |
| "That Clippet is set up for a different Discord application." | Your `.env` carries another application's id or key. Use the two printed on the page. |
| "That address will not do: …" | The address must be a public host name — `https` only, no port, no path, no IP address, no `.local`/`.internal`. |

**The sentences the *bot* answers on its own** — these never reach your instance or its log, which
is why they are worded differently from §30.7's, on purpose:

| In Discord, ephemerally | It means |
|---|---|
| "No Clippet is connected to this server. Connect one at …" | Nobody has claimed this server. The one place a link ever appears. |
| "This server's Clippet connection is disabled." | The claim exists and was disabled by the bot's operator. |
| "Your Clippet is offline." | The bot could not open a connection to your instance (it refused, or answered 502/503/530) — or, after a public "thinking…", it was still unreachable when the bot settled the message (20 s). |
| a public "thinking…" that stays | Your instance took more than two seconds to answer. The official bot settles every public "thinking…" at 20 s: into your instance's late answer (a link resolves it in place; a private line goes to the person alone and the public message is deleted), into "Your Clippet is offline." when your instance is unreachable, or into "Your Clippet took too long to answer. Try again." when it is up but gave no usable answer; when it answered with the upload form, the message turns into "Uploading…" and is removed, and the link posts as its own message. The one case that stays is a command whose instance is up but silent past 20 s: it is left alone, because your instance edits it into the link itself. A click's or the form's own wait is private. Nothing leans on Discord's old rule that a follow-up after a deferral edits the loading message (DESIGN.md §20 erratum 320 (g); the forwarder's `settle()`). |
| "Your Clippet is not set up for the bot." | Your instance answered 404: the three variables are not set on the running container. |
| "Your Clippet does not recognise this bot." | Your instance answered 401: `DISCORD_PUBLIC_KEY` is not the official application's key. |
| "Your Clippet is not available right now." | Any other non-2xx, a redirect (never followed), or a non-JSON answer. Check `docker compose logs app`. |

Compare §30.7: *"This server is not connected to a Clippet account."* is **your instance** talking
(the guild is not bound there), *"No Clippet is connected to this server."* is **the bot** (no claim
at all).

**What the bot's operator learns, and keeps.** Per connected server: the server's id, the host you
typed, and the Discord user id of the person who connected it (so a claim can be disputed or
re-proved). With a person binding, the bot's operator also keeps your Discord user id beside that server's record. Per `/clip`: the server id, an outcome word, and a latency — never the interaction body,
never a user id, never a clip. Your instance's own logs are yours, as before.

**Your own application still works.** §30.1–§30.7 are unchanged and remain the path for anyone who
wants their own app. An instance serves ONE application at a time — the one whose three values `.env` carries —
because the signature against that one public key is the whole credential; to run both paths, run two instances
(one per environment is the usual shape: a dev box on your own application, the production box on the official bot).
If those two instances share a Discord **server**, give the dev one command names of its own (§30.2) —
`/upload-dev`, `/clip-dev` and `Enrich (dev)` beside the defaults — so each box answers only its own commands and
neither uploads into the other's library.
And give it an `INSTANCE_BADGE` too (§7.1): the command keeps the two boxes apart in Discord's picker, and the badge
keeps them apart everywhere a person looks — a grey wordmark instead of a green one, `dev` beside the mark, `dev · ` on
every tab, and `clippet dev` on the site line of the card the dev box posts back into the same channel.

**Sign in with Discord on a managed instance** (§19.6) needs an application of your own — the official one's secret is
not yours — so that is the one place the Developer Portal comes back: a small application with one redirect, two lines in
`.env` beside the three above, and nothing about the bot changes.

### 30.9 `/settings`: the link defaults from Discord

`/settings` (DESIGN.md §20 erratum 318) changes the **connected account's upload defaults** — the same
three the site's **Settings → Upload defaults** panel changes, through the same code — from inside the
server. It is how a server's admin sets what every `/upload` there starts from without signing in to the site; the uploader's own form (§30.5) can change the lifetime of that one upload.

**Who sees it.** The command is published with Discord's `default_member_permissions` set to **Manage
Server**, so only members who hold Manage Server (or Administrator) see it in the picker. A server's
admins may widen or narrow that under **Server Settings → Integrations → Clippet**; the instance checks
the permission again on every interaction, so widening the picker never widens who may change the
defaults. Anybody else is told "You need Manage Server here."

**What it shows.** Running it answers a panel only the person who ran it can see: a **Link defaults**
row with an **Edit** button, and a **Who may clip** line pointing at Discord's own Integrations page,
which is where that control lives. **Edit** opens a form, filled in with the account's current values:

| Field | Choices | The site's word for it |
|---|---|---|
| Delete after | server default · *the instance's `DEFAULT_CLIP_TTL`*, 1 day, 7 days, 30 days, 90 days, never | the same picker, same order |
| Unlisted | a checkbox: "New clips are unlisted unless you say otherwise." | "Unlisted by default" |
| Link | Compact, Memorable | the same two |

**Save** writes the three values and answers "Changes saved."; the site's Settings page shows them at once.
Anything outside those choices is refused with "That is not a choice here." and nothing is written.
Nothing in the form is logged: the access log records that a `/settings` click happened (`discord=
component` or `discord=modal`, `discord_prefix=cb:`) and nothing of what was chosen.

**A self-hosted application** publishes `/settings` beside the other three with `clippet admin bot
register` (re-run it after upgrading to this version; the name is `DISCORD_SETTINGS_COMMAND_NAME`,
§30.2). **On the official bot** (§30.8) the forwarder answers `/settings` itself with a larger panel —
the connection, the link defaults, who may clip, the Pass name — and relays only the **Edit** click and
the form to your instance, exactly as it relays `/clip`. Your instance needs nothing new for that: the
binding is the gate, as for every other command.

**If the form shows nothing to pick on a phone or the web client:** Discord's radio buttons and
checkboxes inside a form are recent. The instance has a fallback shape — the same three fields as
drop-down menus — behind one constant in `internal/httpapi/discord_settings.go`; which shape ships is
recorded in DESIGN.md §20 erratum 318.

### 30.10 Clippet Auto on your box

Clippet Auto turns the videos people post in a connected server into Clippet links, with no command:
the box itself holds a connection to Discord (its gateway) and watches the channels of the servers
bound to an account here (§30.3). It is **off** by default and it is a different thing from the bot
above, which only answers commands and never sees a channel's messages (DESIGN.md §20 erratum 333).

Clippet Auto is a separate, optional app. Clippet's own bot never reads your channel's messages.

**The three modes** (`DISCORD_AUTO_MODE`):

| Mode | What a posted video gets |
|---|---|
| `off` (default) | nothing — no socket is opened |
| `prompt` | one small reply under it with one button, **Upload to Clippet**. It removes itself after `DISCORD_AUTO_SECONDS` (default 10, 5–60) unless pressed. The poster, or anyone with Manage Server, may press it: the presser alone sees `Uploading…`, then the link is posted, the original message is removed and the reply goes too. |
| `auto` | no reply and no press: the video is uploaded, the link posted and the original removed. |

Under each link the bot posts one line, `Videos posted here become Clippet links.` —
`DISCORD_AUTO_NOTICE=off` removes it. A video is taken when the Clippet Auto URL (§9) would take it (its
rule: on Discord's CDN, a video or a sound, under `MAX_UPLOAD_BYTES`); anything else is
ignored without a word. The upload lands in the bound account with that account's link defaults and
is charged to its upload budget. Uploads run one at a time per server, ten waiting at most.
On a person-scoped binding (§30.8 step 3b) Clippet Auto acts only for that person's own videos.

**What to create in Discord.** Either the application you made in §30.1 (one app for both), or — on
the official bot (`DISCORD_BOT_TOKEN=managed`, §30.8) — a new application of your own:

1. **Developer Portal → Applications → New Application** (or open your §30.1 app).
2. **Bot → Privileged Gateway Intents → Message Content Intent: on.** Without it Discord refuses the
   connection and Clippet Auto stops (the log says `disallowed_intents`; `admin doctor` says so too).
3. **Bot → Reset Token → Copy** — this is `DISCORD_AUTO_TOKEN`, a secret.
4. **Installation** (or **OAuth2 → URL Generator**): scope **`bot`**, permissions **View Channels,
   Send Messages, Embed Links, Read Message History, Send Messages in Threads, Manage Messages** —
   the permissions integer **274878000128**. Manage Messages is what removes the original; without
   it the link is still posted, and the bot says `Clippet Auto needs Manage Messages to remove the
   original.` (to the presser in `prompt`, once a day in the channel in `auto`) and never retries.
5. Install it into the server, and bind the server to an account if it is not bound yet
   (`clippet admin bot bind <server id> <username>`, §30.3). A server that is not bound is ignored.
6. If this app has an **Interactions Endpoint URL** (the §30.1 app does), the button's presses arrive
   there and the instance handles them; a new app needs none — the presses arrive on the connection.

**What to put in `.env`:**

```sh
DISCORD_AUTO_MODE=prompt        # off | prompt | auto
DISCORD_AUTO_TOKEN=…            # the Auto app's bot token (secret)
#DISCORD_AUTO_SECONDS=10        # prompt's reply lifetime, 5-60
#DISCORD_AUTO_NOTICE=off        # no line under each link
```

`prompt` and `auto` need the §30 bot configured (your own app, or `managed`). With the mode unset,
setting any of the other three is a boot error; `DISCORD_AUTO_MODE=off` written out keeps the token in
the file and reads nothing. Restart the app after a change, then run `clippet admin doctor`: its
`auto` row asks Discord, read-only, whether the token answers and the Message Content intent is on.

**Why the reply is visible to everyone.** A message the bot posts on its own cannot be private:
Discord's private ("ephemeral") messages exist only as answers to somebody's command or button. So the
`prompt` reply is short-lived instead, and everything said to one person (`Uploading…`, a refusal) is
an answer to their press.

**What it holds and logs.** Nothing about a message is stored: a waiting reply's few facts (which
message, who posted it, its attachment) live in memory until it is pressed or expires; a restart forgets
them, and a press on a forgotten reply just removes it. Log lines are `discord auto: …` with the server
id and an outcome — never a token, a link, a file name or a message's content. The connection goes to
Discord's gateway (`gateway.discord.gg`, or the regional `gateway-….discord.gg` Discord names) and
nowhere else. If Discord refuses the token or the intent, Clippet Auto stops and says why once; fix
the cause and restart. While Discord is having trouble it reconnects slower and slower, up to once
every five minutes, and it never starts more than 500 sessions a day: Discord resets a bot's token
past 1000.


### 30.11 Hosted Clippet Auto

Nothing to run. A server connected to the official bot (§30.8) — a Clippet Pass name, or your own box claimed
there — turns Clippet Auto on in the official bot's `/settings`: Off, Prompt or Auto, the seconds and the notice,
and the server's Clippet Auto URL (Settings → API tokens on your Clippet, §20 erratum 324). One hosted process holds
the Clippet Auto app's gateway for every such server and sends your box the same small instruction `POST
/in/auto/{token}` takes; your box fetches the video from Discord itself, so no video passes through the hosted
process. Your box must answer `/in/auto` on its public https host with no port — it does since erratum 324, and a
Pass name's guard has carried the route since release cycle 3. A host whose name resolves to a private, loopback or
link-local address is never called (§20 erratum 334, amended 2026-10-12). Clippet Auto on your box (§30.10) is the other way to
run the same feature, with an app of your own; use one or the other in a server, never both.

`AUTO_TEST_ENDPOINTS=1` is a test-only setting of the hosted process (§20 erratum 347): it lets a name under the reserved `.test` domain reach a private address, so the E2E can run the process against a mock on a private network. It is never set in production, and the process refuses to start with it unless its token is not a Discord token, its plane is a `.test` name and no dead-man URL is set.

### 30.12 `/signin` — a member signs in with their Discord

With `REGISTRATION=members` (or `role:<role id>`, §19.1) and the bot configured — your own (§30.2) or the
official one (§30.8) — a member of a server bound to this instance (§30.3) types `/signin` there and gets,
privately, one line and one link:

> Sign in to *your host* · this link works once, for five minutes.

The link opens a page on your Clippet that says `Sign in to <host> as <their Discord name>` with one
**Sign in** button; the button signs that browser in. Their first `/signin` makes their account — named
after their Discord username (lowercase, `.` and anything else outside letters, digits and `_` become
`_`, and `_2`, `_3`… on a clash with a name already here), linked to their Discord **id**, with no
password; every later `/signin` opens the same account. A Discord name that happens to match an account
here is never linked to it: the id is the key. To link YOUR existing account first, so your own
`/signin` opens it: `clippet admin users link <you> <your Discord user id>` (§19.6).

**What it proves.** The command is a Discord interaction, signed with Discord's own key and verified by
**your box** against the application's public key — exactly as `/upload` is. On the official bot the
forwarder relays the signed bytes and can forge none of them; nothing about a sign-in passes through, or
is decided by, Clippet's plane, and there is no OAuth. The role, under `role:<id>`, is read from the same
signed interaction.

**The link.** One use, five minutes, stored only as a hash, and written `/login/discord/{token}` in every
log line. Opening it changes nothing — Discord itself fetches links a bot sends, and that fetch must not
spend it — only the button does. A used, expired or unknown link lands on `/login` with "That sign-in
link expired. Run /signin again." The session is the one a password login issues: the same cookie, the
same `SESSION_TTL` / `SESSION_MAX_TTL` (§19.3), one more row under **Settings → Signed-in browsers**.

**The refusals**, each visible only to the person who ran it: "Discord sign-in is off on this Clippet."
(any other `REGISTRATION`), "This server limits sign-in to a role you do not have." (`role:<id>` without
it), and the unbound server's "This server is not connected to a Clippet account."

**A member who leaves the server** cannot `/signin` again, keeps the browser session they already have
until it expires (thirty days idle, ninety at most, by default), and keeps their clips — the links are
already in your server's chat. To end it sooner: `clippet admin sessions revoke <name>` (§19.4).

**A password, if they want one:** `clippet admin users password <name>` (§19.4) sets it; their Discord
stays a way in. That is also the recovery when somebody loses their Discord account.

## 31. What this instance costs

Clippet can tell you roughly what your own storage bill looks like. It does this the only honest way
a self-hosted tool can: **you type what your provider charges you, and it multiplies.** Nothing here
asks a provider anything — there is no API call, no price lookup, no account to connect and no second
credential (§24's no-call-home promise is unchanged by this feature). Two optional variables, two
numbers this app already keeps, and an **estimate** on three surfaces.

### 31.1 The two prices

```sh
# .env — your own currency, per GB. Both optional; both unset (the default) = no cost line anywhere.
STORAGE_PRICE_PER_GB_MONTH=0.015     # what it costs to KEEP one GB for a month
EGRESS_PRICE_PER_GB=0.01             # what it costs to SEND one GB to a viewer
```

- **Your currency, not a currency this app knows.** Every figure is printed as a bare number with two
  decimals and no symbol, because the app is never told which currency you are in and never guesses.
  Write `0.015` whether that is dollars, euros or anything else.
- **Plain decimals.** Digits, one optional point, at most six digits after it (`0.015`, `0.36`, `0`,
  `0.000001`). A sign, an exponent (`1e-3`), a thousands separator or a `$` is a **boot error** that
  names the variable — a price is the one value where a typo would quietly produce a wrong number
  rather than a broken feature, so it fails at boot like every other §7 value.
- **Unset is absent, not zero.** With no price configured there is no cost line at all: no `cost:`
  line in `clippet admin stats`, and the Settings page's cost cells read `unpriced`. A `0.00` you
  never configured would say "storage here is free", which is the one wrong thing this could tell
  you. (`/healthz` shows no cost in either state — §31.4 says why.)
- **`0` is a real answer.** Setting a price to `0` means "this costs me nothing" and prints `0.00`.
  That is the **R2 case**: Cloudflare R2 charges for what you store and nothing for egress, so a
  friend on R2 sets only `STORAGE_PRICE_PER_GB_MONTH=0.015` and leaves `EGRESS_PRICE_PER_GB` unset
  (or `0`, if they would rather see the zero spelled out). Backblaze B2 charges for both; a box with
  its own disks and a metered uplink might price only egress.
- **`STORAGE_BUDGET_BYTES` (§25) is untouched** and is a different thing: a line you want to be warned
  about crossing, not a price.
- **`clippet admin doctor` reads them back** (§24.1's `prices` row): it prints the two values *as the
  app parsed them*, so `.015` comes back as `0.015` and a zero-padded `00000015` as `15`, or says
  both are unset. It prints no amount — the estimate lives where the byte counts are.

### 31.2 What is estimated, and what is not

| Number | Where it comes from | How exact |
|---|---|---|
| stored bytes | the sum of every clip's objects in the **database** (the same number §25's budget is compared against), plus the database snapshot under `_clippet/` (§15) on the displayed reading — a megabyte or two, counted like any stored byte, and never by the hard cap (`STORAGE_BUDGET_HARD`, §31.6 — the snapshot is never the reason a clip is refused) | exact for everything a clip row references; it does **not** count an object no row points at any more — `clippet admin orphans` (§24.4) is the report for those |
| served bytes | one integer per **UTC day**, counted as the app writes each media response | exact to the byte for what left this app; it is not your provider's own meter, and anything a CDN or a browser cache answered instead of this app never reached it at all |
| stored cost | stored bytes ÷ 10⁹ × `STORAGE_PRICE_PER_GB_MONTH` | an **estimate** of a full month at the size it is right now; it is not prorated, does not know what the month did before this moment, and does not know your provider's minimums, classes, request charges or tax |
| served cost | this calendar month's served bytes ÷ 10⁹ × `EGRESS_PRICE_PER_GB` | an **estimate** of the month so far |

**1 GB = 10⁹ bytes** here, the decimal gigabyte providers bill in — not the GiB (2³⁰) the rest of this
document measures files in. The difference is 7.4 %, in the direction that would make a bill look
smaller than it is, which is why it is spelled out. The byte figures on the pages stay in the IEC
units everything else uses (`24.4 MiB`); only the money is computed in decimal GB.

**It is an estimate, never a bill.** Every surface says so and every figure carries a `≈`. Your
provider's invoice is the truth; this is the number that tells you a week early that something
changed.

### 31.3 The served-bytes counter

Served bytes did not exist anywhere before this feature, and on a metered provider they are the
number that actually surprises people. The app now counts them:

- **What is counted:** the bytes it actually wrote to viewers — renditions, originals it streams,
  card copies, posters, filmstrips and preview GIFs. A viewer who seeks away or closes the tab is
  counted for what they received, which is what a provider meters. A `HEAD`, a `304 Not Modified` and
  a range this app refuses write no body and cost nothing. Pages, CSS, JavaScript and fonts are not
  counted: they come out of the container image, not out of the bucket you pay for.
- **What is NOT counted, and never will be:** anything about *who* watched. The table is one row per
  UTC day — a date and a number of bytes — with no viewer, no address, no user agent, no account and
  no clip in it. There is nothing to redact because there is nothing there (this is the same line
  §22's private view counts draw, and it is not crossed for an accounting number).
- **It is kept for 400 days** and the sweeper deletes the rest on its own tick. A year plus a margin:
  the surfaces show today and this calendar month, and the history behind them is what lets you tell
  an unusual month from a normal one.
- **With `MEDIA_DELIVERY=redirect` it is an ESTIMATE, and says so.** In that mode the bytes leave
  your bucket without passing through the app, so nothing here can measure them. Rather than report
  a zero nobody measured, **each request answered with a redirect is charged the whole object** —
  per request, not per view, so a viewer who seeks pays for the file again each time — and a `HEAD`
  is charged nothing, exactly as it is above. It is an upper bound, which is the safe direction for
  a cost line. Settings' Storage panel prints `estimated (redirect)` beside the figure and
  `admin stats` says the same on its `served:` and `cost:` lines; `/healthz` keeps bare integers,
  because it is a machine surface — this paragraph is its footnote. See §33.5.
- **A crash can lose up to half a minute of it.** The app holds the running count in memory and
  writes it to its database every 30 seconds, or every 64 MiB of serving, whichever comes first, and
  once more at shutdown. If the process is killed between writes, the bytes in that window are gone.
  At a typical `0.01` per GB, 64 MiB is 0.00064 of a currency unit — four orders of magnitude below
  the hundredth anything here prints. That trade buys the media path a counter that never touches the
  database while a video is streaming.

### 31.4 Where you see it

**`clippet admin stats`** (§24.1) — the operator's view, and the only one that needs no browser:

One instance carries all three samples in this section: 24 GB stored against a 200 GB budget,
3 GB served so far this month, priced at `0.015` and `0.01`.

```text
storage: used=24000000000 (22888.2 MiB) budget=200000000000 (190734.9 MiB) ok
served:  today=2000000000 (1.9 GiB) month=3000000000 (2.8 GiB) — bytes sent to viewers, aggregate per UTC day, 400-day history
cost:    stored ≈0.36 /month  served ≈0.03 this month  (an ESTIMATE at 1 GB = 10^9 bytes, from STORAGE_PRICE_PER_GB_MONTH and EGRESS_PRICE_PER_GB — never a bill)
```

The `served:` line is always printed — zero is a healthy answer and is printed as one. The `cost:`
line appears only when at least one price is set; the unpriced half of it reads `unpriced`. The CLI
is a **second process** sharing the database file, so its served figures can be up to one flush
window (§31.3) behind the running app's; `/healthz` and the Settings page are exact.

**`/healthz`** — anonymous, so treat it as public (§25 says the same of the budget object):

```json
"storage_usage":{"stored_bytes":24000000000,"served_today_bytes":2000000000,"served_month_bytes":3000000000}
```

**Bytes, and never a price.** The probe carries the three measurements and no cost at all, because
it is anonymous and is normally reachable wherever your origin is: an amount printed beside the byte
count it came from would hand anyone who can reach the origin what you pay per GB, one division
away — and a configured value is exactly what this route has never published (`DESIGN.md` §15). A
priced instance's `storage_usage` is byte-identical to an unpriced one's; the cost estimate is on
the signed-in Settings panel below, and in `admin stats` above, which is somebody already inside the
container.

Every other key is optional and an absent one is never a zero: `stored_bytes` appears once the
sweeper has taken its first reading (seconds after boot). If you would rather even the byte counts
were not on the public internet, a reverse proxy (§26) can decline to forward `/healthz` — the
compose healthcheck reaches it from inside the Docker network either way. Note the key: `storage` in
that same object is the store-ping string it has always been, and `storage_usage` is this.

**Settings → Storage** — one panel, for anyone signed in:

```text
Storage                              this whole instance · an estimate from your own prices, never a bill
quota                22.4 GiB of 186.3 GiB                                                     12 %
stored               22.4 GiB                                                        ≈ 0.36 / month
served this month    2.8 GiB                                                              ≈ 0.03
```

It is **instance-wide**, and since §20 erratum 349 it is the **owner's** — the first account created
on the instance (`admin users` marks it `owner=yes`; `admin users owner <name>` moves it). Every
other account sees the panel's head and one sentence, `Only the account that set up this Clippet can
see this.`, and no figure. The library's budget line (§25) stays on every account's page: a member
needs to know whether the next upload fits. There is deliberately **no per-account figure**
(that would be a tier by another name; your own tally is on the library page) and no chart. The
`quota` row appears only when `STORAGE_BUDGET_BYTES` is set and is **first** — §31.6 says why — and
its percentage turns red once you are at or past the line; the cost cells read `unpriced` when the
matching price is not set.

### 31.5 Presets: `STORAGE_PRICING`, and how close the estimate really is

Typing two numbers is fine until you have to go and look them up. `STORAGE_PRICING` names a **preset**
instead — one line, and the panel is priced:

```sh
# .env — one of: local | r2 | b2 | s3. Unset (the default) = type your own prices above.
STORAGE_PRICING=r2
```

A preset is a **dated copy of that provider's own published list**, typed into this image by a person
who read their pricing page on the date the row carries. It is not a lookup and never becomes one:
this app asks no provider anything, holds no second credential and makes no outbound call for a number
(§24's no-call-home promise, unchanged). Every surface that shows a figure from a preset says which
list and when it was read — `an estimate from R2 list prices as of 2026-09-13 (USD)` — so you can tell
an estimate from a quote.

| `STORAGE_PRICING` | storage | egress | free storage | free egress | read |
|---|---|---|---|---|---|
| `local` | `0` | `0` | — | — | 2026-09-13 |
| `r2` | `0.015` / GB-month | `0` | **10 GB** a month | — (R2 bills no internet egress at all) | 2026-09-13 |
| `b2` | `0.00695` / GB-month (6.95 per TB) | `0.01` / GB | **10 GB** a month | **3×** the stored bytes each month | 2026-09-13 |
| `s3` | `0.023` / GB-month | `0.09` / GB | — | **100 GB** a month | 2026-09-13 |

The free allowances are **modelled, not just mentioned**: R2's and B2's forever-free 10 GB-month comes
off the stored bytes before they are priced, and the egress rules come off the month's served bytes —
so a small R2 instance correctly reads `0.00`, because it is. (AWS has no standing free S3 storage —
what a new account gets on the read date is a *credit*, up to 200 USD over six months, not an allowance —
so `s3` deliberately models **none**: assuming a credit nobody is still on would understate every later bill.)

The three cloud rows are **USD**, because those three lists are quoted in USD; it is the one place this
app names a currency, and it names it because it is repeating somebody else's number rather than
reading yours (`local` names none — nobody quotes a price for a disk you already own).
`local` is a real row and not an absence — a disk you already own has a provider bill of zero, and the
panel prints `0.00` with the hint `local: no provider bill`, which is a different statement from
`unpriced`. `s3` is S3 Standard in US East (N. Virginia), first 50 TB.

**A typed price wins.** `STORAGE_PRICE_PER_GB_MONTH` and `EGRESS_PRICE_PER_GB` each override their half
of the preset; **both free allowances stay**, because an allowance is the provider's billing rule and
not a number you retyped. That is how you correct a drifted list price — or a region, or a plan — in
one `.env` line, today, without waiting for a new build:

```sh
STORAGE_PRICING=b2
EGRESS_PRICE_PER_GB=0.012     # your own rate; B2's 3x allowance still applies
```

`clippet admin doctor`'s `prices` row reads the whole thing back:

```text
prices   ok   preset b2 (STORAGE_PRICING, read 2026-09-13, USD): storage 0.00695/GB-month after 10 GB a month, egress 0.012/GB (yours, over the preset) after 3x what is stored - a dated copy of the provider's list, never fetched
```

#### How wrong can this be?

Honestly, and in one table. Nothing here is a bill; the question is only how far off it is.

| what | how far off | why |
|---|---|---|
| stored line | a few per cent on a steady month; much more on a month that grew or shrank a lot | it is a **full month at today's size**, not prorated. It does not know what the month did before this moment: double your storage on the 28th and the line jumps as if the whole month had been that big |
| served line | exact to the byte for what left this app, minus the modelled allowance | the counter is what this app wrote (§31.3). It is not your provider's meter, and anything a CDN or a browser cache answered instead never reached this app at all |
| B2's 3× allowance | right at the end of a steady month; optimistic on a month that grew | the rule is 3× the month's **average** storage and this app holds one reading — what is stored now — so the current reading stands in for the average |
| **not modelled: request charges** | under 1–2 % for video | per-1000-operations charges are real but tiny when the objects are clips rather than thumbnails |
| **not modelled: minimum duration and minimum size** | 0 on all three rows | R2, B2 and S3 Standard have none. A colder class (Glacier, B2 with a lifecycle rule) would, and this table does not model one |
| free-storage tiers | modelled on `r2` and `b2`, absent on `s3` by choice | R2's and B2's forever-free 10 GB-month is subtracted. AWS's new-account credit is not a standing allowance and is not modelled — an account still spending its credit is over-stated, the safe direction |
| **not modelled: tax** | your jurisdiction's | no invoice-side arithmetic of any kind |
| **not modelled: price drift** | a few per cent a year | a preset is right on its `read` date. Providers announce changes rarely and in small steps; the date is on every surface so you know when to check, and a typed price overrides it the moment you do |
| **not counted at all** | whatever crashes left behind | objects no row references any more are outside every figure here — `clippet admin orphans` (§24.4) is the report for those |

### 31.6 The quota as the headline

With `STORAGE_BUDGET_BYTES` set, the **first** row of the Storage panel is the quota:

```text
quota                41.2 GiB of 186.3 GiB                                                     22 %
```

It is first because "is there room" is the question people open this panel with, and the money rows
answer a different one. It is also the only row of the three that is *actionable*: the answer to a full
quota is to delete something, and the answer to a big number in the cost column is usually to think
about it later. The `budget` row that used to sit at the bottom of this panel is **this row** — one
row, not two saying the same pair of numbers.

Past the line the percentage turns **red**. Red means loss in this interface (`web/STYLE.md`: delete,
revoke, failed — text, never a fill), and a quota that is full is the loss of the next upload: refused
outright if `STORAGE_BUDGET_HARD=1` (§25), and otherwise the one somebody did not plan for. It is text
and a hairline, exactly like every other red in the product.

`clippet admin stats` leads with the same line, in the same order, and says which mode the budget is in:

```text
quota:   41.2 GiB of 186.3 GiB (22 %) advisory (STORAGE_BUDGET_HARD=0: no upload is ever refused because of it)
storage: used=44255305728 (42205.0 MiB) budget=200000000000 (190734.9 MiB) ok
```

`/healthz` is unchanged: its `storage_budget` object carries `used`, `budget` and `over` in bytes, and
never a price or a mode — that route is anonymous (§31.4).

### 31.7 Worked example: a friend on Cloudflare R2

R2 charges for storage and nothing for egress. Their `.env`, the short way:

```sh
S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com
S3_REGION=auto
STORAGE_PRICING=r2
```

…or the long way, which is what that preset expands to — minus the dated attribution the panel prints:

```sh
STORAGE_PRICE_PER_GB_MONTH=0.015
# EGRESS_PRICE_PER_GB stays unset: R2 does not bill it, and an unset price shows no line rather
# than a 0.00 that would look like a measurement. (The preset sets it to 0 instead, which says the
# same thing out loud — R2's egress really is free, and that is a measurement.)
```

With 240 GB of clips, Settings reads `stored 223.5 GiB · ≈ 3.45 / month` under the preset and
`≈ 3.60 / month` under the hand-typed price — the 15 cents between them is R2's forever-free 10 GB,
which the preset knows about and a typed price cannot. The served row reads `≈ 0.00` under the preset
or `unpriced` under the hand-typed pair; either is worth having beside the byte figure, because it is
how they find out that one clip went around a Discord server 4 000 times. A box on Backblaze B2 sets
`STORAGE_PRICING=b2` and gets the 3× free-egress rule modelled with it; a NAS at home with a metered
uplink sets `STORAGE_PRICING=local` and then `EGRESS_PRICE_PER_GB` over it. Whatever the pair, nothing
about uploads, TTLs or quality changes: this feature only ever *tells you a number*. (The one variable
that does change what happens is `STORAGE_BUDGET_HARD` — §25 — and it is off unless you set it.)

## 32. A directory instead of a bucket

*(ROADMAP #37, DESIGN.md §20 errata 276 and 345.)* **This is the default deployment since erratum 345:
one container, and a directory.** It is the shape for the two cases a self-host most often is:

- **"I have a VPS with a disk."** An object-storage server would be a second image to pull, a second
  thing to upgrade and a second set of credentials, and everything it would be doing is putting files
  on the disk that is already there.
- **"My media should live on the NAS."** The share is already mounted on the host; the shortest path
  from there to a clip is a bind mount, not an S3 daemon in front of it.

A bucket of your own is the opt-in (§7 *BYO bucket*, §35) for what a directory cannot do: redirect
delivery (§33), the database snapshot in the bucket and `admin restore --from-store` (§15). §32.3
moves an instance either way, and §32.6 is the upgrade path for a stack that ran the bundled bucket
server the default used to be.

Everything else about the product is unchanged. Same routes, same cards, same transcode, same TTLs
and the same sweeper. The store is seven methods (`internal/store`), and a directory answers them.

### 32.1 The one-container stack

**On a desktop this is no lines at all, because the media directory has a default: your own Videos
folder, `~/Videos/clippet`** (macOS, Linux and Windows alike — ROADMAP #42, DESIGN.md §20 erratum 295).
Compose creates it on the first `up` if it is not there, and the store keeps its objects under
`clips/` beside a small marker file, so the folder's *root* stays yours — which is what §34's drop
folder then reads. **On a server, set the path** (and on Linux make uid 10001 own it, §2):

```sh
STORAGE_DIR_HOST=/srv/clippet-media      # the HOST directory, or your NAS mount point
```

Then the commands you already use:

```sh
docker compose up -d
docker compose ps            # one service: app
docker compose exec -T app clippet admin doctor
```

`compose.yaml` binds `STORAGE_DIR_HOST` to `/media` and sets `STORAGE_BACKEND=fs` and
`STORAGE_DIR=/media` as its defaults. **`COMPOSE_FILE=compose.yaml:compose.fs.yaml`** — what this
section asked for before erratum 345, and what the `/setup` folder block still assigns — keeps working
and PINS the shape: that override hard-sets the backend and the directory and passes the four `S3_*`
through empty, so a bucket line left in `.env` cannot reach the app. It is an override of
`compose.yaml` (the same shape `compose.gpu.yaml` uses, §27): the service keeps its name, its port, its
volumes and its healthcheck, so nothing you have written down about this stack changes.

**How the default is spelled, and the one place it can be wrong.** Compose interpolation cannot
expand `~`, so the file spells it from the variables your shell already exports —
`${STORAGE_DIR_HOST:-${HOME:-${USERPROFILE}}/Videos/clippet}`: your own path if you set one, then
`HOME` (macOS, Linux, Git Bash), then `USERPROFILE` (PowerShell and cmd, where `HOME` is not set). It
is never a path inside the container, which is what a default would have been before erratum 295 and
what the next `--force-recreate` would have taken with it. **On Linux**, Docker creates a missing
directory owned by root and the app runs as uid 10001: `sudo chown -R 10001:10001` it once (§2), or the
`store` row of `admin doctor` reports it not writable and media fails while everything else serves.

Two things to know:

- **Put your own path in `.env`, not in your shell.** Compose reads `.env` itself, so the value
  arrives exactly as you typed it. An *exported* `STORAGE_DIR_HOST=/srv/clippet-media` on Git Bash is
  rewritten by MSYS into `C:/Program Files/Git/srv/clippet-media` before docker ever sees it — the
  same quirk §32.3 warns about for the migrate commands.
- **In an environment with no home directory at all** — a scheduler, a service account, `env -i` —
  `${HOME:-${USERPROFILE}}` is empty and the default resolves to `/Videos/clippet`, at the *root* of
  the host filesystem. The compose file cannot refuse that (a nested `:?` guard is evaluated eagerly
  by Compose v2 and would refuse on every Linux and macOS box instead), so `admin doctor`'s `store`
  row WARNs about it and names the fix. Set `STORAGE_DIR_HOST` explicitly anywhere you did not log in.

Either way, **`admin doctor` tells you which directory on your own disk `/media` is** and which
variable supplied it — the container cannot otherwise say, and on a desktop that is the first thing
worth checking:

```
store    ok   the host directory bound at /media is C:\<home>/Videos/clippet (the desktop
              default: your Videos folder, from HOME or USERPROFILE - set STORAGE_DIR_HOST in .env
              to move it, DEPLOY.md §32.1)
```

**Delete the four `S3_*` lines from `.env`.** On `fs` any one of them is a boot error naming itself:

```
clippet: config: S3_ENDPOINT: STORAGE_BACKEND=fs takes no S3_* variable: the media lives in
STORAGE_DIR and this instance has no bucket, so remove the line
```

That refusal is the feature's best line, and it is a refusal rather than a shrug so that the line is
*checkable*. §15's guarantee is "the only outbound destinations this binary has are the S3 endpoint,
Discord, and the one URL in `CLIP_WEBHOOK_URL`". On `STORAGE_BACKEND=fs` there is no S3 endpoint at
all, so the guarantee becomes: **the only outbound destinations are the ones you turned on —
Discord's and your clip webhook's — and with those unset there are none whatsoever.** An instance with a leftover `S3_ENDPOINT=` line in `.env` would leave a reader
unable to establish that by reading the file, so it does not boot. `admin doctor` says the same
sentence on its `store` row, from inside the container.

**Where it may live.** `STORAGE_DIR` may not be, contain, or sit inside `DATA_DIR` (`/data`) or
`SCRATCH_DIR` (`/scratch`), and a value that does is a boot error naming both. `SCRATCH_DIR` is
walked by the boot janitor and the sweeper, which *remove* working directories whose clip is gone
(§10) — a media store under it would be deleted a clip at a time by the app's own housekeeping. And
`DATA_DIR` holds the database, which is the small, precious, back-it-up-every-day half; the media
directory is the large half. A deployment that cannot tell an operator which is which has already
lost the argument about what to copy where.

### 32.2 On a NAS: what is true, and what is only hoped

Mount the share **on the host**, with the host's own tooling (`/etc/fstab`, `systemd.mount`,
Synology's own UI), and bind the mount point in. The container never speaks SMB or NFS: that is
deliberate, because a mount inside a container needs `CAP_SYS_ADMIN` and dies with the container,
while a host mount is something your OS already knows how to retry, log and monitor.

```sh
# /etc/fstab, one line, then `mount -a`
//nas.lan/media/clippet  /mnt/clippet  cifs  credentials=/root/.smb,uid=10001,gid=10001,file_mode=0644,dir_mode=0755,nofail,_netdev  0 0
```

```sh
STORAGE_DIR_HOST=/mnt/clippet
```

Four things to know before you do it, all of them facts about network filesystems rather than about
this app:

- **Ownership, not permissions.** The image runs as **uid 10001** and never as root. On SMB/CIFS the
  server does not know about your uids, so the mount option `uid=10001,gid=10001` (above) is what
  makes the files belong to the app; on NFS the *server's* export decides, so the directory must be
  owned by 10001 there (or the export must map it). If you get this wrong the symptom is honest and
  immediate: `admin doctor`'s `store` row FAILs with "is not writable by this process (uid 10001)",
  and `/readyz` refuses. Nothing is half-written.
- **`fsync` on a directory.** Every upload is written to a temp file, `fsync`ed, renamed over the
  final name, and then the *directory* is `fsync`ed — the sequence that makes a power cut leave
  either the old object or the new one and never half of either. **CIFS does not implement a
  directory `fsync`** and answers `EINVAL`. The app treats that one answer (and `ENOTSUP`/`ENOSYS`)
  as "this filesystem does not do that" and carries on, because the alternative is every upload
  failing on a share that is otherwise working perfectly. What you lose there is the *last* of those
  four guarantees: the rename is still atomic, but the share decides when its metadata reaches a
  platter. On NFS and on any local filesystem you lose nothing. **On Windows** (the desktop app's
  This PC or a NAS home) there is no directory `fsync` at all, so the app does not attempt one: the
  store relies on the file's own `fsync` and the rename, which NTFS journals (DESIGN §20 erratum 336).
- **Case sensitivity.** Keys are built by one function (`internal/store/keys.go`) and are
  `[A-Za-z0-9]` ids plus fixed lowercase names, so a case-insensitive share works — but note that a
  case-*folding* one makes two distinct clip ids (`aB…` and `Ab…`) collide. Clip ids are 12 base62
  characters, so this is a lottery you will not win; it is written down because it is the one way the
  key space could bite on a filesystem and not on a bucket.
- **Latency, and which timeouts are real here.** Serving a clip is a ranged read per request and a
  transcode copies the source to local scratch first, so the share sees large sequential I/O rather
  than chatter. What bounds it: **`STORAGE_OP_TIMEOUT` applies**, to `Ping`, `Stat` and `List` — so a
  share that has stopped answering makes `/readyz` fail and `admin stats` return with a
  `storage timeout` rather than hanging your terminal. What does **not**: `STORAGE_IDLE_TIMEOUT` and
  `STORAGE_TRANSFER_MAX` are S3-only. A filesystem read cannot be cancelled from outside — the store
  runs those three calls on a goroutine and abandons it when the deadline passes — and a *transfer*
  that stalls mid-file ends only when the caller's own context does (the request, or the job's
  timeout), not on a clock of the store's own. If that matters to you, mount with `soft` and a
  `timeo=` on NFS, which is where the answer belongs.
- **Nothing in `STORAGE_DIR` is followed through a symlink.** The store treats every component under
  it as a real directory or a real file: a symlink planted as an object is reported by `admin
  orphans` as an extra (and removed by `--delete`), and every read, write and stat of it is refused
  by name. A symlinked *directory* inside the store is refused the same way, which is what stops a
  path from leading out of it. `STORAGE_DIR` **itself** may be a symlink — pointing it at a mount is
  an ordinary way to deploy this, and the rule starts below the root. The practical consequence:
  **the store is the app's alone.** Do not curate it by hand, and do not link parts of it in from
  elsewhere; put the real directory where you want the bytes and bind THAT.

`admin doctor` reports whether `STORAGE_DIR` and `SCRATCH_DIR` share a filesystem. It reports it and
does not judge it: on a single-disk VPS they do and that is fine, and on a NAS deployment they must
not, because a transcode's scratch copy on the share would make every encode pay the network twice.

### 32.3 Moving: `admin storage migrate`

One verb, both directions, and it **never deletes from the source**:

```sh
clippet admin storage migrate --to fs  [--dry-run] [--manifest <path>] [--stop-file <path>] [--max-bytes N]
clippet admin storage migrate --to s3  [--dry-run] [--manifest <path>] [--stop-file <path>] [--max-bytes N]
```

The store this instance is configured to use is the SOURCE; `--to` names the other one, which is read
from the same `.env`. Both halves therefore have to be set for the length of the migration — and
`admin storage migrate` is the **only** command that may see both at once. That is not a variable you
can set: the relaxation lives in the binary's dispatch, on that exact subcommand, so `clippet serve`
cannot reach it and an instance that is serving on `fs` provably has no bucket configured.

**The running app can only reach the store it is configured for**, and a migration needs both. So
each direction is run in a THROWAWAY container that carries the other half, and the live app is left
alone until you flip it yourself. Both forms below are the ones this repository's own E2E runs.

**Moving off a bucket onto a disk.** The app is on `s3` (the bucket flavour,
`COMPOSE_FILE=compose.yaml:compose.nobucket.yaml`), so the DESTINATION has to be bound into the process
doing the copying — `docker compose run` mounts the service's own `/data` and `/scratch` (the live
database and scratch disk) and `-v` adds the media directory beside them. (From the bundled bucket server
of a stack older than DESIGN.md §20 erratum 345 it is the same verb run from that stack's own compose
file: §32.6.)

```sh
# 1. .env keeps every S3_* line and gains the HOST path. Nothing is switched over yet:
#    STORAGE_DIR_HOST=/srv/clippet-media
#    COMPOSE_FILE=compose.yaml                   <- NOT yet; the app still needs the bucket
#    STORAGE_BACKEND=fs                          <- NOT yet either
#
# 2. see what it would do (nothing is copied)
docker compose run --rm --no-deps -v ${STORAGE_DIR_HOST}:/media -e STORAGE_DIR=/media \
  app admin storage migrate --to fs --dry-run
#   migrate: from s3 (clips at https://<account>.r2.cloudflarestorage.com) to fs (/media)
#   migrate: plan clips=412 objects=1663 bytes=48211992576 (44.9 GiB) key_prefix=(none)
#   migrate: plan Ab3XyZ9kLmQp state=ready objects=4 bytes=118231040
#   …one line per clip…
#   migrate: the source is NEVER written to or deleted from; flip STORAGE_BACKEND yourself when this has finished
#   migrate: --dry-run: nothing was copied
#
# 3. do it (the plan prints again first, then one line per object)
docker compose run --rm --no-deps -v ${STORAGE_DIR_HOST}:/media -e STORAGE_DIR=/media \
  app admin storage migrate --to fs
#   migrate: progress clips/Ab3XyZ9kLmQp/original/source.mkv 41943040/96468992
#   migrate: progress clips/Ab3XyZ9kLmQp/original/source.mkv 96468992/96468992
#   migrate: copied clips/Ab3XyZ9kLmQp/original/source.mkv size=96468992 sha256=9f2c… (read back)
#   …
#   migrate: done copied=1663 skipped=0 bytes=48211992576 (44.9 GiB) failed=0
#   migrate: .clippet-store written in /media
#
# 4. NOW flip it: COMPOSE_FILE=compose.yaml (or compose.yaml:compose.fs.yaml), STORAGE_BACKEND=fs,
#    delete the four S3_* lines
docker compose down && docker compose up -d
docker compose exec -T app clippet admin doctor
#   store    ok   STORAGE_DIR=/media writable (probe file created and removed), free=… size=…
#   bucket   skip STORAGE_BACKEND=fs: there is no bucket …
#
# 5. only when you are satisfied, and by hand, remove the old bucket — the app never will
```

> **Use the `-v` form, not `exec -e STORAGE_DIR=/media`.** Without the bind there is no `/media` in
> the container at all, so `exec` would succeed and write every object into the container's own
> writable layer — filling the docker root and throwing the whole copy away at step 4's recreate.
> `admin doctor`'s marker WARN catches the aftermath, but the bytes are gone. The `run` form binds the
> real directory, and the `.clippet-store` file appearing on the HOST side is the proof it did.
>
> **On Git Bash, prefix both commands with `MSYS_NO_PATHCONV=1`** — they pass two *in-container*
> absolute paths (`…:/media` and `STORAGE_DIR=/media`), which MSYS otherwise rewrites into a Windows
> one. The symptom is loud rather than silent:
> `clippet: config: STORAGE_DIR: must be an absolute path, got "C:/Program Files/Git/media"`. Same
> rule as the quirk note at the top of this file; the `-e S3_…` form below needs nothing, because
> none of its values looks like a path.

**The other direction**, off a directory onto a bucket. Here the app is on `fs`, where the four
`S3_*` variables are refused at boot (and `compose.fs.yaml`, if you select it, forces them empty) — so
the bucket travels on the command itself, which `admin storage migrate` is the one verb allowed to
accept it from:

```sh
docker compose exec -T \
  -e S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com -e S3_BUCKET=clips \
  -e S3_ACCESS_KEY=… -e S3_SECRET_KEY=… \
  app clippet admin storage migrate --to s3 --dry-run
#   migrate: from fs (/media) to s3 (clips at https://<account>.r2.cloudflarestorage.com)
#   …then the same run without --dry-run
```

Afterwards set `STORAGE_BACKEND=s3` with those four variables in `.env`, and **assign
`COMPOSE_FILE=compose.yaml:compose.nobucket.yaml`** — the bucket flavour: one container, no folder
bind, `STORAGE_BACKEND=s3` as its default (§35.2). Assign the line rather than relying on a line being
absent — the setup wizard's blocks all do, for this reason.

The plan is printed **before** any byte moves, and `--dry-run` prints exactly that and stops: you
should never learn the size of an operation by watching it. Every object is verified at the
destination — so "done" means the clip arrived intact, not that the API call returned — in one of two
ways, and every verdict line ends by saying which (§20 erratum 337):

- **`(etag proof)`** — into a bucket whose ETag is the one predicted from the bytes that were hashed.
  Cloudflare R2 and AWS S3 document the rule: one PUT's ETag is the object's MD5, a multipart object's is
  the MD5 of its parts' MD5s followed by `-N`. The migrate computes that in the same pass that takes the
  sha256, the upload sends a Content-MD5 with every part (so the bucket refuses a part that arrived
  different), and the destination's own `HEAD` answer is compared with the prediction. Nothing is read
  back. The self-hosted server the test suite runs matched too, measured; Backblaze B2's rule is not
  documented anywhere we have read, so on B2 expect the next line instead.
- **`(read back)`** — anything else: a bucket whose ETag is shaped some other way, an object uploaded
  earlier at a different part size, or a **directory**, which has no ETag at all. The whole object is read
  back through the destination and its sha256 compared with what the source delivered, which costs one
  extra read per object. A bucket this project has no document for simply never matches, which is the
  safe side.

An object already at the destination at the same size is checked the same way: the SOURCE is read once
(for the manifest's sha256 and the predicted ETag together), and the destination is read only when its
ETag does not prove the copy — `migrate: skip <key> size=N (already at the destination, verified
sha256=H) (etag proof)` or `… (read back)`.

Large objects go up to a bucket as **`STORAGE_UPLOAD_THREADS` parts at once** (6 by default, 16 MiB each;
§7). That is the throughput knob for a migration onto R2 as much as for the app's own uploads: one
stream at a time measured about 300 Mbps from a Windows box to R2. Memory is about that many × 16 MiB per
running upload; `1` is the old one-part-at-a-time path. Set it on the command like any other variable
(`-e STORAGE_UPLOAD_THREADS=8`); a bucket-to-bucket copy uploads with the same value.

**The lines, for a program reading them** (the desktop app's Storage screen does). Every line the run
prints on stdout starts with `migrate: `. Per object, in order:

```
migrate: progress <key> <bytes>/<size>            at most once a second while bytes move, and once at
                                                  the end of the leg with <bytes> = <size>
migrate: copied <key> size=N sha256=H (etag proof)
migrate: copied <key> size=N sha256=H (read back)
migrate: skip <key> size=N (in the manifest)
migrate: skip <key> size=N (already at the destination, verified sha256=H) (etag proof)
migrate: skip <key> size=N (already at the destination, verified sha256=H) (read back)
migrate: <key> is at the destination but does not match the source (…); copying it again
migrate: FAILED <key>: <reason>
```

`progress` counts ONE leg per object, the one that crosses the network: the write into a bucket, or the
read out of a bucket into a directory (bucket to bucket, the write). So an object's bytes only ever
grow, and a skip prints none.

A re-run is safe and cheap: it copies what is missing, re-verifies what is there, and **copies again
anything whose hash does not match** — so "fix the cause and run this again" is true even when the
thing that went wrong left a same-size wrong file behind. `failed=` anything is an exit code of 1 and
a line telling you **not** to flip `STORAGE_BACKEND` yet.

The run has no overall time limit (DESIGN.md §20 erratum 350): a large library takes as long as it takes.
Each object has its own limit, `STORAGE_TRANSFER_MAX` (6 h by default), on a directory as on a bucket;
an object that hits it is `FAILED` and the run goes on to the next one.

**Between two buckets** (§20 erratum 335). `--to s3` on an instance already on `s3` copies into the
bucket the seven `MIGRATE_S3_*` variables name — read by this one verb and by nothing else; `serve` never
reads them. `MIGRATE_S3_ENDPOINT`, `MIGRATE_S3_BUCKET`, `MIGRATE_S3_ACCESS_KEY` and
`MIGRATE_S3_SECRET_KEY` are required together; `MIGRATE_S3_REGION` (default `us-east-1`),
`MIGRATE_S3_FORCE_PATH_STYLE` (default false) and `MIGRATE_S3_KEY_PREFIX` (default: `S3_KEY_PREFIX`'s
value) are optional. With none set, `--to s3` is the refusal above; with some set, the line names what is
missing (exit 2). `--to fs` ignores all seven. The plan names the destination as
`s3 (<bucket> at <endpoint>)` — never a key.

```sh
docker compose exec -T \
  -e MIGRATE_S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com -e MIGRATE_S3_BUCKET=clips \
  -e MIGRATE_S3_ACCESS_KEY=… -e MIGRATE_S3_SECRET_KEY=… -e MIGRATE_S3_REGION=auto \
  app clippet admin storage migrate --to s3 --manifest /data/move.manifest
#   migrate: from s3 (clips at https://<old-endpoint>) to s3 (clips at https://<account>.r2.cloudflarestorage.com)
#   …the plan, one line per object, the done line
```

Then set the four `S3_*` lines in `.env` to the new bucket and restart. Keep `MIGRATE_S3_KEY_PREFIX`
unset (or equal to `S3_KEY_PREFIX`): the database names every object by its whole key, prefix included.
Unset, it follows the source's prefix; a value that differs is not refused — setting it is your consent, and
the copy then holds keys the switched instance's rows do not name.

Three flags for a long copy, each one optional:

- **`--manifest <path>`** — a text file, one line per verified object: `<key>\t<size>\t<sha256>`, appended
  after every `copied` and every hashed `skip`. On a re-run an object whose key is in the manifest and whose
  destination size matches is skipped WITHOUT hashing: `migrate: skip <key> size=N (in the manifest)`. A
  line for a key the plan no longer holds is ignored. A missing file is an empty manifest; an unreadable or
  malformed line is fatal, `admin storage migrate: --manifest <path>: …` (exit 2) — never a silent trust.
- **`--stop-file <path>`** — read BETWEEN objects (after one finishes, before the next starts): once the file
  exists the run stops.
- **`--max-bytes N`** — read between objects too: the run stops before the object that would take the bytes
  placed this run plus the bytes skipped past N, so the copy stays a mirror of at most N.

A stop prints `migrate: stopped reason=stop-file copied=N skipped=N bytes=N (H) failed=N remaining=N` (or
`reason=max-bytes`) and exits **3**: nothing removed, no marker written. `remaining=` is the objects not yet
attempted. The exit codes: 0 done, 1 `failed=` anything (or a store that does not answer), 2 a refusal
before the run (`--to`, missing variables, the manifest), 3 stopped.

Two things it deliberately does not carry: a prefix with no row in the database (that is an *orphan*,
and `admin orphans` is the verb for those — a migration that copied them would make cleaning up the
first thing you did on the new store), and anything at all from a clip the database no longer has.

**Non-goals, unchanged from ROADMAP #37:** no multi-node story, no replication, and no "both at once"
mode. One copy that verifies is the whole of it.

### 32.4 Backups: the folder is the whole backup

**With the database snapshot on — the default — the folder holds a copy of the database too**, under
`_clippet/` (§15, DESIGN.md §20 erratum 348), written at most every `DB_SNAPSHOT_EVERY` (10 minutes)
after a clip changes and once a day whatever happened. So a copy of the folder alone brings the instance
back: put it in place and run `admin restore --from-store` (§15). The two halves below are still worth
taking when you want a history of the database or a rollback point newer than the last snapshot.

`admin backup` is the **database** and only the database (§15). On a bucket you could lean on the
provider — R2 and B2 have versioning and lifecycle rules, and a fat-fingered delete is often
recoverable without you. **A directory has none of that**, so on `fs` the whole of the media's
durability is what you give it:

```sh
# the database (small, precious, every day)
docker compose exec -T app clippet admin backup /data/backups --rotate 7

# the media (large; whatever your tool of choice is — this is just rsync)
rsync -a --delete /srv/clippet-media/ /backup/clippet-media/
```

Take them **close together**, and restore them **as a pair**. A mismatched pair is not a disaster and
it is not silent either — `admin orphans` names it, and which of its three reports fires tells you
which way round it is:

| What you restored | What `admin orphans` says | What it means |
|---|---|---|
| a database **newer** than the media | `missing <id> state=ready key=…` | rows whose objects are not there. Those clips 404 their media; the row and the page survive. Delete them, or restore a newer copy of the directory |
| a database **older** than the media | `orphan clips/<id>/ objects=4 bytes=…` | objects with no row: clips the database has forgotten. They serve nothing and cost disk. `admin orphans --delete` removes them once you are sure |
| the wrong directory entirely | doctor: `.clippet-store is MISSING while the database accounts for … of stored clips` | the mount is not the one this database knows. **Check the bind mount before serving** |

That last row is why `clippet serve` drops a `.clippet-store` marker in `STORAGE_DIR` at its first
boot. It holds nothing that matters; its *absence* is the signal, and it is the cheapest possible
answer to the failure this deployment shape invites — a share that did not mount, an empty directory
underneath the mount point, and an app that starts happily and answers 404 for every clip it still
lists. So **`clippet serve` never writes it over a folder the database says should hold clips**
(DESIGN.md §20 erratum 348): it logs one ERROR naming the folder, writes no marker and no database
snapshot there, refuses every write and delete into it, and serves degraded — `/readyz` answers 503 with `"storage":"error: unavailable (fs)"`
until the right folder is mounted and the app restarted. It is still not a refusal to boot, because an
instance upgraded from a build older than the marker looks identical from the inside: if this IS the
directory, create an empty file named `.clippet-store` at its root and restart.

### 32.5 What `admin stats` says on a directory

On a bucket, two lines describe two different places: `storage:` is the database's own sum of what
the bucket *should* hold, and `disk:` is the container's filesystem. On `fs` they describe one place,
so the budget line carries the directory with it:

```
storage: used=48211992576 (45983.5 MiB) budget=off (STORAGE_BUDGET_BYTES) backend=fs dir=/media free=412316860416 (384.0 GiB) size=1099511627776 (1.0 TiB)
served:  today=… month=… — bytes sent to viewers, aggregate per UTC day, 400-day history
disk:    data_free=… scratch_free=… min_free=… same_device=false
store:   objects=1663 bytes=48211992576 (45983.5 MiB) clip_prefixes=412
```

The listing line says `store:` rather than `bucket:` — there is no bucket, and a line that said
otherwise would be the one place this feature lied. `STORAGE_BUDGET_BYTES` still works exactly as §25
describes: **advisory unless `STORAGE_BUDGET_HARD=1`** (§25.4), on either backend. What
it gains here is a free-disk figure beside it, on the same line, about the same disk.

`/healthz` gains nothing but a word: its `storage` string reads `ok (fs)` here where a bucket
instance reads `ok (s3)` — both backends carry the suffix since ROADMAP #37 (API.md), so this
is a change of WORD, not of shape — and an
anonymous prober can tell a bucket problem from a disk problem. It names no path, no bucket and no
credential, and `storage_usage` is unchanged (§31).


### 32.6 Coming from the bundled MinIO (stacks older than DESIGN.md §20 erratum 345)

Until erratum 345 a plain `docker compose up` ran three services — the app, a **MinIO** server and a
one-shot `bucket-init` — and kept every clip in MinIO's own volume, `<project>_minio_data`
(`clippet_minio_data` for the default project name). MinIO's publisher deleted both images from
Docker Hub on 2026-09-11, so the current `compose.yaml`
no longer names them. **Your clips are still in that volume, in MinIO's format, and nothing in this
release touches it.** The app and its database are unaffected; what needs moving is the media.

**Before you take the new revision, read this whole section.** (Taken without it, an old `.env`'s active
`S3_BUCKET=clips` with no `S3_ENDPOINT` stops the app at boot with a `S3_ENDPOINT: required: S3_BUCKET is
set but S3_ENDPOINT is not …` line that names this section — DESIGN.md §20 erratum 348.) Three facts
decide the order:

- **The MinIO images exist only in your local image cache now.** `docker image ls minio/minio
  minio/mc` shows whether you still have them. `docker compose pull` on the old file **fails** — the
  repositories are gone — so do not run it, do not `docker image prune -a`, and do not remove the
  `minio_data` volume until §32.6's last step says so. (If the images are already gone from your
  machine too, the research note above records where the exact pinned binaries still are; that path
  is by hand and not covered here.)
- **The new `compose.yaml` cannot reach MinIO** — it has no such service — so the copy runs from the
  OLD file, kept beside the new one under another name.
- **`admin storage migrate --to fs` is the bucket → folder copy** (erratum 276, §32.3): it reads every
  live clip's objects from the bucket the instance is configured with and writes them into a directory,
  verified, and it never deletes from the source. If you would rather keep a bucket, `--to s3` with the
  `MIGRATE_S3_*` variables copies into a bucket of your own instead (§32.3, *Between two buckets*).

**The upgrade, into a directory:**

```sh
# 0. back up the database first (§15.1) — it is the rollback
docker compose exec -T app clippet admin backup /data/backups --rotate 7

# 1. keep the compose file you are running now, under another name, BEFORE the checkout replaces it
#    (or recover it afterwards: git show <the revision you ran>:compose.yaml > compose.bundled.yaml)
cp compose.yaml compose.bundled.yaml

# 2. take the new revision (§16), but do NOT bring it up yet

# 3. pick the HOST directory for the media and put it in .env; on Linux give it to uid 10001 (§2).
#    The commands below spell the same path on their `-v` (here /srv/clippet-media).
#    STORAGE_DIR_HOST=/srv/clippet-media

# 4. copy, from the OLD file (`-f` wins over any COMPOSE_FILE line in .env); MinIO must be running —
#    it is, if you never stopped the stack, and `docker compose -f compose.bundled.yaml up -d minio`
#    starts it from the cache if you did. The dry run prints the plan and copies nothing.
docker compose -f compose.bundled.yaml run --rm --no-deps -v /srv/clippet-media:/media \
  -e STORAGE_DIR=/media app admin storage migrate --to fs --dry-run
#   migrate: from s3 (clips at http://minio:9000) to fs (/media)
#   migrate: plan clips=412 objects=1663 bytes=48211992576 (44.9 GiB) key_prefix=(none)
docker compose -f compose.bundled.yaml run --rm --no-deps -v /srv/clippet-media:/media \
  -e STORAGE_DIR=/media app admin storage migrate --to fs --manifest /data/move.manifest
#   …one line per object…
#   migrate: done copied=1663 skipped=0 bytes=48211992576 (44.9 GiB) failed=0

# 5. switch: in .env delete COMPOSE_FILE (or set it to compose.yaml), MINIO_ROOT_USER,
#    MINIO_ROOT_PASSWORD and every S3_* line (S3_BUCKET is a boot error on the folder store), then
#    bring up the new file; --remove-orphans removes the old minio and bucket-init CONTAINERS, never
#    a volume
docker compose up -d --remove-orphans
docker compose exec -T app clippet admin doctor
#   store    ok   STORAGE_DIR=/media writable (probe file created and removed), …
#   store    ok   .clippet-store is present: this directory is the one an instance has been writing to
docker compose exec -T app clippet admin orphans          # every clip's objects are where the database says

# 6. only when you are satisfied — clips play, `admin orphans` reports nothing missing — and by hand:
#    docker volume rm clippet_minio_data          (the old media; the app never removes it)
#    rm compose.bundled.yaml
```

The rules of §32.3 apply unchanged: use the `-v` form, not `exec` (without the bind the copy lands in
the container's own writable layer and is thrown away); on Git Bash prefix the `run` commands with
`MSYS_NO_PATHCONV=1`; a run with `failed=` anything exits 1 and you do not switch yet — fix the cause
and run step 4 again, which copies only what is missing or wrong. A long copy can be stopped and
resumed with `--stop-file` and the manifest.

**Into a bucket of your own instead** (R2, B2, S3 — for redirect delivery, the database snapshot and
`admin restore --from-store`): make the bucket and its key first (§35), then step 4 becomes
`docker compose -f compose.bundled.yaml run --rm --no-deps -e MIGRATE_S3_ENDPOINT=… -e
MIGRATE_S3_BUCKET=… -e MIGRATE_S3_ACCESS_KEY=… -e MIGRATE_S3_SECRET_KEY=… -e MIGRATE_S3_REGION=… app
admin storage migrate --to s3` (dry run first), and step 5 sets
`COMPOSE_FILE=compose.yaml:compose.nobucket.yaml`, `STORAGE_BACKEND=s3` and the four `S3_*` lines of
the NEW bucket instead of deleting them (the `/setup` page's block for your provider is exactly that).
Keep `MIGRATE_S3_KEY_PREFIX` unset or equal to `S3_KEY_PREFIX`, as §32.3 says.

**Staying on the old shape for a while** is possible and is only a stopgap: `COMPOSE_FILE=compose.bundled.yaml`
in `.env` keeps running the three services from the cached images with the new app, exactly as before
— but a server whose images can no longer be pulled can no longer be patched either, and the next
`docker image prune -a` takes it. Move the clips while the cache still has it.

## 33. Serving media by redirect

*(ROADMAP #34 lane F, DESIGN.md §20 erratum 278.)* By default this app streams every media byte
itself: a viewer's player asks `/api/clips/{id}/video.mp4`, the app reads the object out of your
bucket and copies it to them. That is `MEDIA_DELIVERY=proxy`, and it is what every version before
this one did.

`MEDIA_DELIVERY=redirect` is the other mode. The routes, the share links and the Discord card are
**identical**; what changes is the last step. After every gate the route has always run — the clip
exists, it is not deleting, it is ready — the app answers **`302` with a freshly signed link to the
object on your bucket's own origin**, and the viewer's client fetches the bytes from there. No video
byte passes through the app, or through anything in front of it.

Three routes do this: `video.mp4`, `social.mp4` and `poster.jpg` — the player's file, the card's file
and the card's image, which is all of the traffic. Three do not, and never will: `/original` (the one
route with an owner check, which a signed link cannot carry), and `preview.gif` and `filmstrip.jpg`
(the two the watch page's own JavaScript polls, which cannot read the status of a cross-origin
redirect — and which would then need CORS on your bucket, which nothing else here does). They are
small and on demand; leaving them alone costs nothing.

**The reserved prefix `{S3_KEY_PREFIX}_clippet/` is never served, in either mode** (DESIGN.md §20
erratum 343). It is the instance's own — the database snapshot of §15 — and it sits beside `clips/`,
never inside it: no route names it, nothing is ever presigned for it, `admin orphans` and the sweeper
never list or delete it, and `admin objects _clippet` is the one verb that shows it. Keep the bucket
private (every recipe here does): the snapshot is a credential store, and a public-read bucket policy
would publish it with the media.

**Why you might want it.** Cloudflare's Service-Specific Terms restrict serving *video* through a
proxied hostname from a non-Cloudflare origin on the Free, Pro and Business plans, and since 25 Aug
2026 their video-policy page says in writing that a public hostname through a Cloudflare Tunnel is
covered by the same rule. A stack that keeps its media on a disk at home and streams it out through a
tunnel is exactly that shape. In `redirect` mode the tunnel carries HTML and a 300-byte redirect; the
video comes from the bucket. If the bucket is Cloudflare R2 the bytes are on Cloudflare's own
platform, which the rule allows explicitly; if it is Backblaze B2 or AWS S3 they never touch
Cloudflare at all. There is no other reason to switch — it is not faster for a viewer and it does not
change what anything costs.

### 33.1 The three variables

| Variable | Default | What it does |
|---|---|---|
| `MEDIA_DELIVERY` | `proxy` | `proxy` \| `redirect`. `redirect` needs a bucket: on `STORAGE_BACKEND=fs` it is a **boot error**, because a directory has no URL to redirect anyone to (§32). |
| `MEDIA_PRESIGN_TTL` | `1h` | How long one signature stays valid, between `1m` and `24h`. **Do not make it short.** A `<video>` element keeps fetching the *same* URL for as long as its page is open, so a signature that expires mid-watch breaks seeking for somebody who has not reloaded. An hour is longer than anyone watches one clip; a day is fine too. |
| `MEDIA_PRESIGN_ENDPOINT` | *(= `S3_ENDPOINT`)* | The origin **viewers** reach your bucket at, when that is not the origin this app reaches it at. The signature covers the host name, so a link signed for one host is rejected at any other. On R2, B2 and S3 there is one host for both and you leave this unset; a server of your own that the app reaches by a compose service name or a LAN address is the case that needs it, because nobody outside the stack can reach that name. |

Set `MEDIA_PRESIGN_TTL` or `MEDIA_PRESIGN_ENDPOINT` while `MEDIA_DELIVERY=proxy` and they are
**ignored** — deliberately, so you can put them in `.env` before you flip the switch — with one NOTE
on the boot line and on `admin doctor`'s `media` row so it is never a silent nothing. A plain-`http`
presign origin under an `https` `PUBLIC_BASE_URL` *is* a boot error: browsers block that as active
mixed content, so the player would go blank while the Discord card carried on working, which is the
worst way for a setting to be wrong.

### 33.2 What a viewer sees

Nothing, mostly. The share link is unchanged, the watch page is unchanged, and the Discord card is
unchanged — it still points at `/api/clips/{id}/social.mp4` on your own origin, and Discord's media
proxy follows the one redirect behind it. That is load-bearing and was measured against the real
unfurler before this shipped: a **presigned URL written into the card tag would be fatal**, because
an embed is frozen into a Discord message for ever and the signature would die an hour later in every
message that carried it. The tag therefore never carries one. The redirect is minted fresh on every
fetch.

Two things do change if somebody looks:

- **"Copy video address" now yields a signed link that expires.** It works for `MEDIA_PRESIGN_TTL`
  and then stops. The *share* link — the one people actually paste — never expires.
- **A viewer's address reaches your bucket vendor.** Their client connects to the bucket directly, so
  the vendor's logs see them instead of yours. On R2 that is Cloudflare, who already saw the request
  for the page; on B2 or S3 it is a party that saw nothing before. Nothing about the viewer is stored
  by this app either way (§31.3 — the served-bytes counter is one number per day and has never had a
  viewer in it), and there is still no telemetry of any kind. This is the one privacy consequence of
  the mode, and it is worth knowing before you turn it on.
- **What the link itself shows.** Anyone who sees a `Location` sees your bucket's host, the object's
  key, and your **access key id** inside `X-Amz-Credential` — never the secret, which a signature is
  computed with and never carries. On R2 the host contains your account id. None of that lets them
  do anything a share link does not already let them do (the key is as unguessable as the id, §29),
  and the signature cannot be edited without invalidating it.

**"I deleted it — is it gone?"** The route refuses at once: a deleted clip 404s and signs nothing,
before anything is minted. There is one window `proxy` mode does not have, and it is worth stating
plainly — a link someone was *already* handed keeps fetching the object until the sweeper's next tick
destroys it, at most one `SWEEP_INTERVAL` (60 s by default, §10). It is the OBJECT's life that closes
it, not the signature's, so `MEDIA_PRESIGN_TTL` neither shortens nor lengthens it.

### 33.3 The R2 recipe

The move for a stack that is behind Cloudflare today. It costs nothing at small sizes (R2's first
10 GB are free and its egress is not billed) and it is the change that ends the terms question rather
than reducing the odds of it being asked.

1. **Create a private R2 bucket and an API token scoped to it** in the Cloudflare dashboard. Do not
   give the bucket a public `r2.dev` URL and do not attach a custom domain: redirect mode wants a
   *private* bucket, and the signature is the access.
2. **Copy the media across.** §15.2's `rclone` stanza does it with the existing bucket as the source;
   nothing deletes the source, so you can go back.
3. **Point `.env` at R2 and switch the mode:**

   ```sh
   S3_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
   S3_REGION=auto
   S3_BUCKET=<your bucket>
   S3_ACCESS_KEY=<the token's access key id>
   S3_SECRET_KEY=<the token's secret>
   S3_FORCE_PATH_STYLE=false
   MEDIA_DELIVERY=redirect
   ```

   `MEDIA_PRESIGN_ENDPOINT` stays unset: R2 is one host for the app and for viewers.
4. **Preflight, then recreate:**

   ```sh
   docker compose run --rm --no-deps app admin doctor   # the `media` row reads the mode back
   docker compose up -d
   ```
5. **Verify from outside:**

   ```sh
   curl -sI 'https://<your origin>/api/clips/<id>/social.mp4'
   # HTTP/2 302
   # location: https://<account-id>.r2.cloudflarestorage.com/…?X-Amz-Signature=…
   # cache-control: no-store
   ```

   Then paste a share link into Discord and press play. If the card renders and plays, the whole
   path works — the unfurler and the media proxy both follow the hop.
6. **Retire the old bucket when you are ready.** Nothing here does it for you, and there is no hurry:
   keep it until you have watched a few clips from the new one.

**Backblaze B2** is the same recipe with `S3_ENDPOINT=https://s3.<region>.backblazeb2.com` and
`S3_REGION=<region>`; B2 supports presigned S3 URLs and the Bandwidth Alliance makes its egress to
Cloudflare free. **AWS S3** is the same with the regional endpoint and the real region. **A directory
(`STORAGE_BACKEND=fs`, §32) cannot do this at all** — there is nothing to sign — and says so at boot.

### 33.4 Two things to check when it does not work

- **The clock, in either direction.** A signature carries a start time and an expiry. A box whose
  clock runs **ahead** of the vendor's mints links the vendor considers "not yet valid"; a box whose
  clock runs **behind** mints links that expire early, or are already expired on arrival. Both look
  identical from outside — a 403 from the bucket on a link that looks perfect — and neither is caught
  by `admin doctor`, which has no trusted clock to compare against and may not go and ask for one
  (§15). Compare `date -u` inside the container with real time and fix the host's clock; Docker
  Desktop's VM clock drifts and steps, which §8 already warns about for timestamps generally. A
  longer `MEDIA_PRESIGN_TTL` hides small skew in one direction only and is not the fix.
- **The wrong host in the link.** If the `Location` points at something only your machine can resolve
  — a compose service name, a `192.168.*` address, a bare hostname — viewers get nothing. `admin doctor`'s
  `media` row **warns** about exactly that when `PUBLIC_BASE_URL` is public, and the fix is
  `MEDIA_PRESIGN_ENDPOINT`.

### 33.5 What the served-bytes figure means here

Bytes that leave your bucket never pass through this app, so the counter §31.3 describes cannot
measure them. It does not therefore report zero — a zero nobody measured is the one thing that
counter was built to avoid. Instead **each request answered with a redirect is charged the whole
object**.

Read that as *per request*, not per view, because the difference is large: a `<video>` fetches the
same URL again for every seek, so somebody scrubbing through a clip is charged the whole file each
time; a viewer who watches four seconds is charged for the file; and Discord fetches a card twice. A
`HEAD` is charged nothing at all, the same as in `proxy` mode. So the figure is an **upper bound**,
sometimes a generous one — which is the safe direction for a cost line, and why the words beside it
matter: Settings' Storage panel prints `estimated (redirect)`, and `admin stats` says it on both its
`served:` and `cost:` lines. (`/healthz` keeps bare integers: it is a machine surface, and §31.3 is
its footnote.) If you want a real number, your provider's own dashboard is the only place it exists.

The `stored` row and the quota are unaffected either way — they come from the database, not from
anything a viewer did.

### 33.6 Testing this mode, and the test suite in it

`scripts/e2e.sh` runs against **either** mode and needs telling nothing: once the run's first clip is
ready it asks the stack one anonymous question — a GET of that clip's `video.mp4` — and reads `proxy`
off a 200 or `redirect` off a 302, printing which and the origin the link lands on
(`[e2e]   media delivery: redirect (…)`). Everything about the media routes is then asserted one hop
further along: a range, a conditional request and a byte-for-byte download all follow the signed link
and are checked at the bucket, while the handful of assertions that are about *this app's* own answer
on a media route — the shape of its 416, its `If-Match` 412, the headers a 302 must not carry — print
a line saying they stand down and are proven instead by the suite's own redirect step, which brings up
a second instance in each mode against the same objects. The gate command is unchanged
(`bash scripts/e2e.sh`, 100 steps), and so is the answer it gives. (DESIGN.md §20 erratum 280.)

`scripts/visual.sh` reads the shape too, and for the same reason. It asks earlier and twice, because
the first page it photographs can already carry media: the app's own `Content-Security-Policy` names
the media origin in redirect mode and nothing but `'self'` in proxy, so the login page answers before
a clip exists — and once the pass's own clip is ready, one anonymous GET of its `video.mp4` must
answer 200 for a declared proxy, or 302 to a signed link on exactly that origin for a declared
redirect. It prints `[visual] media delivery: redirect (…)`, then allows exactly the signed media
requests to that origin while every other foreign request still fails the pass (DESIGN.md §20 erratum
282). Its browser runs
inside a container, so a LOCAL redirect stack it photographs must sign for an origin that container
can resolve — `MEDIA_PRESIGN_ENDPOINT=http://host.docker.internal:<the bucket server's published port>`
rather than `localhost`, which inside a container means the container.

To run a **local** stack in redirect mode — a laptop, a lane, a self-hoster trying the switch before
buying anything — a bucket server has to run beside the app AND be reachable from outside the compose
network, because the app reaches it by service name and a viewer cannot. The default stack has no such
server since DESIGN.md §20 erratum 345; the test harness brings its own, pinned, in an overlay of its
own (run 22), whose header names the `.env` lines — the shape is
`COMPOSE_FILE` with that overlay, `STORAGE_BACKEND=s3`, the `S3_*` of that server,
`MEDIA_DELIVERY=redirect` and a `MEDIA_PRESIGN_ENDPOINT` on its published loopback port. Then
`docker compose up -d`, `docker compose exec -T app clippet admin doctor` (the `media` row reads the
origin back) and `curl -sI 'http://localhost:8481/api/clips/<id>/video.mp4'` for the 302. This is a
development aid, not a deployment: a real instance in redirect mode points at R2, B2 or S3, where the
app and the viewer share one public host and `MEDIA_PRESIGN_ENDPOINT` stays unset (§33.3).


## 34. The drop folder: put a video in it and it becomes a clip

*(ROADMAP #42 and #51, DESIGN.md §20 errata 296 and 307. Either storage backend — §32 is the section
that sets up a directory.)* On `fs` the media lives in a directory of yours, and this app keeps
everything it owns under `clips/` inside it, beside a small `.clippet-store` marker. The *root* of that
directory has always been yours. This is what it can be for:

**Drop a video in it and it becomes a clip.** No browser, no upload form, no capture tool. On a
desktop, with §32.1's default, that folder is `~/Videos/clippet` — which is where a recorder already
saves, one `Move to` away.

It is **off** until you name the one account that should own whatever arrives:

```sh
DROP_FOLDER_OWNER=alice
```

then `docker compose up -d`. Nothing else changes; with the line absent — the default, and every
version before this one — nothing at the root of that directory is ever read.

### 34.1 The rules

**One account owns everything that arrives.** Not whoever is signed in, not a folder per person: one
name, in `.env`. A clip made this way is exactly a clip that account uploaded — its lifetime, its link
grammar (§29), unlisted or not, all from that account's own defaults in Settings — and it shows up in
their library, and is announced on your Discord channel by the same rule every other clip is (§11).

**The account has to exist.** `DROP_FOLDER_OWNER` naming nobody is a **boot error**, not a warning:

```
clippet: boot: DROP_FOLDER_OWNER names no account on this instance: register it, or remove the line
```

A folder being watched for nobody is the one state this must never be in — the symptom would be
somebody standing there waiting for a clip. Create the account first (`clippet admin users create
alice`, §19), or register it in the browser. `admin doctor`'s `drop` row says the same thing *before*
you restart.

**On a bucket it works too — what it needs is a folder, not a backend.** The clips still go to the
bucket; the only thing this feature wants from the disk is somewhere for you to put a video. So
`STORAGE_DIR` names that folder on **both** backends, and `DROP_FOLDER_OWNER` with no `STORAGE_DIR` is
a boot error naming `STORAGE_DIR` rather than the backend:

```
clippet: config: DROP_FOLDER_OWNER: names the account that owns whatever is dropped at the ROOT of
the media DIRECTORY, and this instance has not been given one: set STORAGE_DIR to a directory bound
into the container (on STORAGE_BACKEND=fs it is the media store itself and is already required; on
s3 it is a folder of its own and the clips still go to the bucket) (DEPLOY.md §34)
```

On `fs` that is already the media store and there is nothing to do. On `s3` (`compose.yaml` or
`compose.nobucket.yaml`) add the two lines yourself — a path inside the container, and the host folder
bound to it:

```sh
# .env - compose.yaml already passes STORAGE_DIR through, so this is all the app needs
STORAGE_DIR=/media
```

```yaml
# compose.drop.yaml, beside your compose.yaml - the one thing .env cannot do is a bind mount
services:
  app:
    volumes:
      - /srv/clippet-drop:/media   # the folder you will be dropping videos into
```

Then name that file so compose reads it. If your `.env` already has a `COMPOSE_FILE` line, **append to
it** (`COMPOSE_FILE=compose.yaml:compose.nobucket.yaml:compose.drop.yaml`); if it has none, add
`COMPOSE_FILE=compose.yaml:compose.drop.yaml` — a `docker-compose.override.yaml` is picked up
automatically only while `COMPOSE_FILE` is unset, which is the trap.

The app creates that directory if it is not there, and puts nothing in it but its own
`.clippet-drop/` staging folder — your library is in the bucket. Pick a folder of its own rather than
one you keep other things in, and note that it is also where `admin storage migrate --to fs` would
copy the library if you ever moved off the bucket (§32), which is the point: the same path keeps
working, under `clips/`, and the drop folder carries on at its root with no line changed.

**What it picks up**, checked every few seconds, one level deep, no recursion and no patterns:

- a **regular file** at the root — never a directory (`clips/` is one), never a symlink, never a
  dotfile;
- whose name does not end in `.part`, `.crdownload` or `.tmp` — the names things still being written
  carry;
- whose **size and timestamp have not moved since the last look**, which is what "finished writing"
  means for everything else;
- and which **ffprobe accepts as a video** — the same check every upload passes.

Anything else is **left exactly where it is**, silently. A screenshot, a text file, a folder of your
own, a recording still being written: none of them is a failure, and none of them is touched. (Turn
the log to `LOG_LEVEL=debug` and it will tell you once, per file, why it passed something over.)

**Copy a file in; do not point it at a copy that might not finish.** The two rules above cover
everything that is still being written *now*, and between them they cover every ordinary way a file
arrives. What they cannot see is a copy that **stops half-way and never comes back** — a network
share that dropped out, a laptop that slept, a sync client that gave up. Once a copy aborts, the
file's size and timestamp stop moving, which is the only signal there is that it finished. The
ffprobe check catches most of those, but not all: a file written *faststart* (which is how this app
writes its own renditions, and how a lot of tools write theirs) carries its index at the front, so
half of one is still a readable video and will be taken as a clip. If a clip turns out to be half a
video, that is what happened: **delete the clip and drop the file again.** The way to avoid it
entirely is the one a capture tool already uses — write, or copy, somewhere else and *move* the
finished file in, which is instant and cannot be half-done.

**What happens to a file it takes.** It is moved out of your way the moment it is taken — into a
`.clippet-drop/` staging directory inside the same folder, which is a rename and not a copy, so it is
instant whatever the file's size. Then it goes through the ordinary upload path: the same size limit
(`MAX_UPLOAD_BYTES`), the same quota (§25), the same working-disk check, the same account upload rate
limit. The title is the one a file name gives (§9). On success the staged file is removed — the
bytes are in the store now, under the clip's own key — and one line goes in the log:

```
level=INFO msg="drop folder: ingested" file="Team fight 3.mp4" clip=Ab3XyZ9kLmQp owner=alice bytes=48211992
```

**On failure the file goes back exactly where it was**, and a `<name>.failed.txt` appears beside it
saying why in one sentence. Fix the cause and save or `touch` the file and it will be looked at again;
delete the note whenever you like. Nothing else in that folder is ever deleted or renamed — the only
files this feature removes are one it moved and one it wrote.

**A file that is already a clip of that account, byte for byte, is not taken again** (DESIGN.md §20
erratum 326): it goes back exactly where it was and a `<name>.clippet.txt` appears beside it, one line
reading `already here <link>`. So the two notes are two different things: `.failed.txt` says a file
could not become a clip, `.clippet.txt` says it already is one. Neither is ever read as a video.

**Drop a hundred files at once and it takes them at its own pace.** Two of the gates above are about
your *instance* rather than about the file: the account's hourly upload budget
(`RATE_UPLOAD_PER_HOUR`, 60 by default and shared with trims, GIFs, posters and replaces — §7) and
the free space on the working disk. Meeting one of those is **not** a failure: no note is written,
nothing is remembered, and the file is simply looked at again a minute later, and a minute after
that, until it goes through. So emptying a capture folder into this one is a thing you can do — it
drains by itself over the following hours, in the order the files come up, and you do not have to be
there. (At `LOG_LEVEL=debug` there is one line per file saying it is waiting.) Raise
`RATE_UPLOAD_PER_HOUR` if you want it to go faster.

**It never ingests the same file twice**, across restarts included: every decision it takes (ingested,
or refused) is recorded against the file's name, size and timestamp, and remembered for 30 days. That
is what makes a crash mid-ingest safe — the next boot puts the staged file back at the root and then
leaves it alone, rather than making a second clip of the same video.

If the boot could **not** put it back because a file of the same name is at the root now, it says so
and leaves both alone — and so does every look that follows, so the one at the root waits too rather
than being written over the one that was rescued:

```
level=WARN msg="drop folder: a file of that name is already staged from an earlier run, so this one
is left alone; move the staged file out of the way (or delete it, if its clip was made) and this one
is taken on the next look" file="Team fight 3.mp4" staged=/media/.clippet-drop/Team fight 3.mp4
```

Look in `.clippet-drop/` (and in the library, for a clip of that name): keep whichever of the two you
want, move it out of the staging directory, and the one at the root is picked up at the next look.
`admin doctor`'s `drop` row counts anything left staged, so this never has to be discovered by
accident.

### 34.2 What the folder must, and must not, be

**A local disk or a NAS share is fine**, and §32.2's four facts about network filesystems all still
apply. The folder is on *your* machine or on a mount *your host* controls, the app writes into it as
uid 10001, and nothing else needs to be true.

**A Dropbox, Google Drive or OneDrive folder is a bad idea**, and it is worth saying why rather than
just saying don't:

- **A sync client writes a file more than once.** It creates a placeholder, streams into it, renames
  it, sometimes rewrites it after a conflict resolution — and on some clients it writes files it later
  *takes away again*. The "size has not moved" rule handles the ordinary case and cannot handle a file
  that is replaced ten minutes later.
- **This folder is written to.** The drop folder MOVES what it takes, and `clips/` underneath it is
  the store itself. Pointing a two-way sync client at a directory an application is writing objects
  into means every clip you make is uploaded to that provider, every clip you delete is a deletion it
  replicates, and a conflicted copy is a corrupted object. If your media is going into somebody's
  cloud, do that deliberately with §32.3 and a bucket, not by accident with a sync client.
- **Placeholder files look like small real files.** On-demand sync ("Files On-Demand", "Stream files")
  leaves a stub on disk until something opens it. The probe opens it, which makes the client download
  it, which turns a 30 GB folder into 30 GB of traffic the first time this feature looks at it.

If you want a folder your phone syncs into, sync it into somewhere *else* and move the file across
when it has landed. That is exactly the boundary ROADMAP #42 (c) — watched folders, read and never
written — is about, and it is not built.

**Do not point two instances at one folder.** Both would take the same file; one would win the rename
and the other would log a miss. One folder, one instance.

### 34.3 Checking it

```sh
docker compose exec -T app clippet admin doctor
```

```
drop     ok   on: a video left at the ROOT of /media becomes a clip owned by alice, with that
              account's own defaults (DEPLOY.md §34)
drop     ok   the folder holds 2 file(s) waiting, 1 with a .failed.txt beside them, 0 staged
              mid-ingest (a staged file is put back at the next boot)
```

Off, it says so and says what off means. On, it names the account, **FAILs** when that account does
not exist (which is what `clippet serve` would refuse to boot on), and counts what is in the folder
right now. `files waiting` is what it has not taken yet — a file still being written, something that
is not a video, or something it has already refused; `staged mid-ingest` is what a process killed
mid-upload left behind, and the next boot puts it back.

Settings' Storage panel carries one line for it too, so the people using the instance can see where a
dropped file ends up:

```
drop folder    a video left in this instance's media folder becomes a clip owned by alice      on
```

It names the account and never the directory: a path is your disk layout, and that page is read by
everyone with an account.

### 34.4 Moving a folder of old clips in: `admin import`

*(ROADMAP #51, DESIGN.md §20 erratum 307.)* The drop folder above watches for what arrives **next**.
This is the other half: a folder of videos you already have — an old `Clips` directory, a hard disk,
everything you exported off Streamable before it went away — walked through this instance's own upload
path **once**, and turned into clips owned by one account.

```sh
docker compose run --rm -v /path/to/old/clips:/import app clippet admin import /import --owner alice
```

That is the same shape §32 uses for `admin storage migrate`: `docker compose run` mounts the
service's own `/data` and `/scratch` (the live database and working disk), and `-v` adds your folder
beside them, read-only as far as this command is concerned. **On Windows, in Git Bash, put
`MSYS_NO_PATHCONV=1` in front of it** — otherwise the shell rewrites `/import` into a Windows path
before docker sees it.

**Moving off Streamable (or anything else): export first, then import.** Download your videos into one
folder — whatever names they come out with — and point this at that folder. The clip's title is the
one a file name gives (§9), so name the files the way you want the clips named.

**It reads your folder and changes nothing in it.** Not a move, not a rename, not a delete, not on a
re-run — and it leaves nothing of its own behind either (the `.failed.txt` notes and the staging
directory belong to §34's drop folder, in a folder this app owns). When it has finished, the folder is
exactly as it was, and your clips are in the library as well.

**Run it twice and the second run imports nothing.** It recognises the *content*, not the name: every
clip's original carries the checksum this instance computed when it stored it, so a file you have
already imported is reported as `seen` even if you have renamed it since. That is what makes this safe
to re-run after fixing something — and what `--dry-run` is for, which prints the verdict for every file
and imports nothing at all:

```sh
docker compose run --rm -v /path/to/old/clips:/import app \
  clippet admin import /import --owner alice --dry-run
```

**What you get back**, one line per file and then a summary:

```
import: /import -> alice (one level, no recursion)
import: the files are READ; nothing in that folder is moved, renamed or deleted
imported Ab3XyZ9kLmQp Team fight 3.mp4
imported Qm7RtY2wPlKx holiday.mp4
seen Ab3XyZ9kLmQp Team fight 3 (copy).mp4
refused not-a-video screenshot.png: this instance cannot read a video stream out of it
import: 2 imported, 1 seen, 1 refused, 118231040 bytes (112.8 MiB)
```

- **`--recursive`** walks sub-folders too (never a dotted one). Without it the walk is one level, like
  the drop folder's.
- **Directories and dotfiles are skipped in silence.** `.DS_Store`, `Thumbs.db` and a folder of your
  own are not failures and are not mentioned.
- **A `refused` line is not necessarily a problem.** A photo, a text file, a `.part` from an unfinished
  download: they stay where they are and the command still succeeds. The **exit code** is 0 for those.
- **It exits 1 when the refusal was this instance's**: the storage quota (§25), no room on the working
  disk, a store or database that did not answer. Fix the cause and run the command again — everything
  that got through the first time is `seen` the second, so nothing is imported twice.
- **It exits 2** on a usage mistake: no `--owner`, a path that is not a directory, or a folder that is
  one of this instance's own (`STORAGE_DIR`, `DATA_DIR`, `SCRATCH_DIR`, or anything inside one). That
  last refusal is deliberate — pointing this at the store would walk every file the app has ever
  written back through the upload path.

**Every gate an ordinary upload passes, it passes.** `MAX_UPLOAD_BYTES`, the storage quota, the
working-disk check, the ffprobe gate, and the owner's own defaults for lifetime, link grammar and
unlisted-or-not (§29, Settings). The one thing it does **not** spend is the account's hourly upload
budget (`RATE_UPLOAD_PER_HOUR`, §7): that limit is about what an account pushes over the network, and
this is you, at your own box, moving your own folder in one go.

**The clips encode in the background.** The command creates them and exits; the running instance's
worker picks the jobs up within a few seconds and works through them at `TRANSCODE_CONCURRENCY` (§7).
Importing a thousand clips is a minute of this command and then a while of the machine being busy —
watch it in the library, or with `clippet admin jobs`.

## 35. Where clips live: the setup wizard

*(ROADMAP #45, DESIGN.md §20 errata 297-299.)* The instance has a first-run wizard at **`/setup`**. It
answers one question — *where do clips live?* — for the five answers this app has, and it answers it
the only honest way a container can: it **prints** the `.env` block and you paste it. It collects no
credential and has no field to type one into, which is why it can be a page in the product rather
than a thing you have to trust.

This section is the wizard's other half. The page holds the walks in terms of **artifacts and
properties** and a dated hint naming today's console labels; everything that is a URL, a long JSON
document or a region code lives **here**, because a page under `web/` may not contain a link to
another origin (web/STYLE.md, DESIGN §20 erratum 298) and because those are the parts that rot on
somebody else's schedule.

### 35.1 How to reach it, and who may

- **Signed in, as the owner** — from Settings → Storage, the `where clips live` link in the panel
  head, or `{PUBLIC_BASE_URL}/setup` directly. The owner is the first account created on the instance
  (§20 erratum 349; `admin users` marks it `owner=yes`, `admin users owner <name>` moves it, and if it
  is deleted the oldest remaining account becomes the owner). Make your own account before you open
  registration or `/signin`: the first account is the owner whoever made it, and `admin users owner <name>`
  is the way back. Any other signed-in account gets 403 and
  one sentence — `Only the account that set up this Clippet can see this.` — and so does its `Check
  now` (`not_owner`), exactly as the Storage panel is the owner's (§31.5).
- **On a brand-new instance nobody has registered on yet** — anonymously. That is the first-run case
  and the reason the page exists; the moment the first account is created the door shuts and an
  anonymous caller is sent to `/login`.
- It is linked from **one** place in the product and nowhere else. There is no banner, no button and
  no nudge on the front door, the library, a share page or an embed.
- **`/setup?view=steps&store=<id>`** (§20 erratum 335) renders only one option's head, its caveat, the
  numbered steps with their dated hints and `labels read` — no chooser, no block, no command, no STOP, no
  Check now. Same gate as the page (the owner, or anybody before the first account); without a valid
  `store` it is the ordinary page.

The page never changes anything on its own. It prints, you paste, you `docker compose up -d`,
and then either `admin doctor` or the page's own **Check now** — which is the *same* probe: `Ping`,
put one small object under `{S3_KEY_PREFIX}doctor/`, delete it again, no read-back. Check now is
single-flighted and cached for ten seconds, so a hundred clicks are one probe; it is a cache and not
a rate limit, and nobody is ever refused.

**Check now is also what keeps the `last check` row honest.** Its outcome is written into the same
last-ping cache that `/readyz` and the boot ping loop write and that `/healthz` reads, so pressing it
moves the first panel's state *and* its timestamp. That matters because nothing else takes a second
reading on a running instance: the boot loop stops for good once the store has answered once, and the
compose healthcheck polls `/healthz`, which only reports the cached value. A panel that says `ok`
with a timestamp from boot is telling you about boot — press the button, or run `admin doctor`, to
learn anything newer.

### 35.2 The total-block rule, and `compose.nobucket.yaml`

**Every block the page prints assigns every variable that decides the outcome — `COMPOSE_FILE`,
`STORAGE_BACKEND`, `MEDIA_DELIVERY` and every `S3_*` this app reads — and never tells you to delete a
line.** That is not tidiness. Compose
merge beats `.env`: `compose.fs.yaml` hard-sets `STORAGE_BACKEND: fs` and four **empty** `S3_*` on
the app service, the last file's `environment` wins over the first file's interpolation, and an empty
value is not a boot error. So an `fs` operator who edits `.env` to point at B2 and forgets the
`COMPOSE_FILE` line keeps booting `fs` — **silently**, with `admin doctor` and Check now both saying
the store is fine, about the wrong store.

**The same thing happens between two buckets, through `.env` rather than through compose.** A value
still sitting in `.env` from the bucket you are leaving survives an edit that does not name it, and
compose's `${NAME:-default}` hands it straight to the app — so every bucket block assigns all of them.
(Until DESIGN.md §20 erratum 345 a sixth block, the bundled bucket server, assigned seven EMPTY `S3_*`
so that compose put its own defaults back; that server and its fallbacks are gone, and an empty
`S3_ACCESS_KEY` on a bucket is now a boot error naming it.)

The list is seven and not four because all seven decide something and all seven survive an edit that
does not name them: `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`,
`S3_FORCE_PATH_STYLE` (a `false` left behind by a hosted provider breaks a self-hosted server) and
`S3_KEY_PREFIX` (a stale one silently moves where every object lands). Every block that lands on a
bucket assigns all seven; the `fs` block assigns none of them, because on `fs` any of the four
credentials is a boot error naming itself.

| option | `COMPOSE_FILE` | `docker compose ps` shows |
|---|---|---|
| a directory (`fs`) | `compose.yaml:compose.fs.yaml` | 1 — `app` |
| B2 / R2 / S3 / any other bucket | `compose.yaml:compose.nobucket.yaml` | 1 — `app` |

`compose.nobucket.yaml` is the bucket flavour (DESIGN.md §20 errata 298 and 345): `STORAGE_BACKEND`
defaults to `s3` there — so a bucket `.env` written before erratum 345, when `s3` was the default
everywhere, boots as it did — the folder bind is dropped (the volume list is `!override`n to the two
named volumes), `STORAGE_DIR` falls back to empty and the `STORAGE_DIR_HOST` rows are emptied. Every
`S3_*` still interpolates from `.env` through `compose.yaml` exactly as §7 documents. It needs Compose
v2.24+ for the `!override` tag. (Before erratum 345 its job was the opposite half of the same choice:
to switch OFF the bundled bucket server `compose.yaml` started by default.)

The `docker compose ps` row at the end of every walk is the observable half of the assignment. If the
service count is wrong, the `COMPOSE_FILE` line did not take — which is exactly the failure above.

**Every block opens with `COMPOSE_PATH_SEPARATOR=:`** (DESIGN.md §20 erratum 348). Compose splits
`COMPOSE_FILE` on `;` on Windows and on `:` elsewhere, so without that line a block's file list is one
file name nobody has on Windows; elsewhere the line is inert. The folder block's `STORAGE_DIR_HOST` line
is commented out: the default — a `clippet` folder in your Videos folder — works, and you uncomment it
only for another directory or a NAS mount point (§32.1).

### 35.3 The five answers, with the parts a page may not carry

Every block below is the one the page prints. Endpoints are placeholders there and real here.

#### A directory, or a NAS (`STORAGE_BACKEND=fs`)

The whole of it is §32, and the wizard's walk is its short form. Two things worth repeating: the
directory must belong to **uid 10001** (`chown -R 10001:10001 …`; on SMB/CIFS mount with
`uid=10001,gid=10001,file_mode=0644,dir_mode=0755,nofail,_netdev`; on NFS the server's export
decides), and it may not be, contain or sit inside `DATA_DIR` or `SCRATCH_DIR` — the app refuses to
boot if it is, and names both. On a NAS, mount the share **on the host**: a mount made inside the
container needs `CAP_SYS_ADMIN` and dies with the container, while the host's own mount is something
the OS knows how to retry and log.

`STORAGE_PRICING=local` prices it `0 / 0` with the hint `local: no provider bill`, which is a
different statement from `unpriced` and is what makes the quota gauge the headline instead of money
(§31.5, §31.6). If your uplink is **metered**, the disk is free and the household bill is not: type
`EGRESS_PRICE_PER_GB` over the preset. And back up two halves from now on — `admin backup` is the
database, the directory is yours (§32.4).

#### Cloudflare R2 — 10 GB free, subscription first

The recommendation when the instance is behind Cloudflare, and the one this project's own reference
stack runs. Console path and the exact recipe: **§33.3**. Free-tier terms: <https://developers.cloudflare.com/r2/pricing/>;
getting started and the checkout flow: <https://developers.cloudflare.com/r2/get-started/>; account API
tokens: <https://developers.cloudflare.com/r2/api/tokens/>.

Two things the walk states in Cloudflare's own words because the difference matters to exactly the
person choosing between R2 and B2: you **complete a checkout flow to add an R2 subscription** before
a bucket exists (it is free to start with and it will want a payment method), and the free 10 GB
applies to **Standard** storage only, never Infrequent Access. Egress from R2 is $0.

#### Backblaze B2 — 10 GB free, no card

**Backblaze documents two live web consoles with two different vocabularies, and both are current.**
The walk names both; here they are side by side (read 2026-09-13):

| | standard web console | enterprise web console |
|---|---|---|
| reach keys by | `B2 Cloud Storage → Application Keys` | `Access Control → Application Keys` |
| the button | `Add a New Application Key`, then `Create New Key` | `Create App Key` |
| scope it to one bucket | `Allow Access to Bucket(s)` | a **Region** selector, then a bucket (or `All in Region`) |
| permission | `Read and Write` | `Read and Write` |
| expiry | `Duration (seconds)`, optional | an `Expiration` toggle, optional |

Sources, both read 2026-09-13: *How to Create and Manage App Keys in Backblaze B2 Cloud Storage* and
*How to Create and Manage Application Keys in the Enterprise Web Console*, at
<https://www.backblaze.com/docs/>. Pricing: <https://www.backblaze.com/cloud-storage/pricing>.

Do **not** use the master application key. Backblaze's own docs: it "provides complete access to your
account. This key has all capabilities, access to all buckets, and has no file prefix restrictions or
expiration." And set a lifecycle rule that cancels unfinished large files after a day — B2 has no
default for this, and a killed upload otherwise leaves parts no tool here can see and your provider
still bills for.

The endpoint B2 shows you contains your region string, and you need both: paste the endpoint whole
into `S3_ENDPOINT` and put the region part into `S3_REGION`.

#### Amazon S3

**Read this first.** A new AWS **Free plan** account "closes on its own 6 months after you open it or
when your credits run out, whichever comes first" (<https://aws.amazon.com/free/>, read 2026-09-13).
What it gives is **up to $200 in credits over 6 months**, not a storage allowance. Do not put a
Discord server's clips on a free AWS account you intend to keep.

The bucket stays private — leave "Block all public access" on. This app proxies or signs every byte
itself, so the bucket is never public and needs no CORS rule and no bucket policy. Set a lifecycle
rule that deletes incomplete multipart uploads after a day (the bucket's Management tab).

The policy, scoped to one bucket, is too long for the page:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Sid": "ListTheBucket",
      "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:ListBucketMultipartUploads"],
      "Resource": "arn:aws:s3:::YOUR-BUCKET" },
    { "Sid": "WorkOnTheObjects",
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject",
                 "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"],
      "Resource": "arn:aws:s3:::YOUR-BUCKET/*" }
  ]
}
```

Attach it directly to a user with **no console access**, and create an access key for it — the
"application running outside AWS" case (<https://docs.aws.amazon.com/IAM/latest/UserGuide/id_credentials_access-keys.html>,
read 2026-09-13). The secret is retrievable only at creation.

**The endpoint is constructed, not copied**: the S3 console shows no endpoint field on a bucket (it
offers the ARN, the S3 URI and a *static website* endpoint, none of which is what goes here). Build
it from your region — `https://s3.<region>.amazonaws.com`, for example
`https://s3.eu-west-2.amazonaws.com` (London), `https://s3.us-east-1.amazonaws.com` (N. Virginia),
`https://s3.ap-southeast-2.amazonaws.com` (Sydney). The full list is at
<https://docs.aws.amazon.com/general/latest/gr/s3.html>.

The shipped `s3` preset models **no free storage**, on purpose: what a new account gets is a credit,
not an allowance, and pricing a credit nobody is still on would understate every later bill.

#### Any other S3-compatible bucket

A self-hosted S3 server on the NAS, Wasabi, Hetzner, Garage, Ceph RGW, a LAN address. The endpoint is whatever you
type: only its **shape** is checked — a scheme, a host, no path, no query, no credentials in it.
There is no allowlist here (the two allowlists in this app are the webhook's and the bot's CDN one).
Self-hosted servers usually want `S3_FORCE_PATH_STYLE=true`; hosted providers want `false`, and if
the doctor reports a DNS failure with your bucket name glued to the front of the endpoint you have it
the wrong way round. Leave `STORAGE_PRICING` unset and type
`STORAGE_PRICE_PER_GB_MONTH` / `EGRESS_PRICE_PER_GB` (§31.4), or set it to the preset your provider's
prices resemble and override one half.

**This is the one walk whose block sets `MEDIA_DELIVERY=proxy`**, and the reason is the first case in
its own list. A redirect sends the **viewer's own browser** to the endpoint, so the endpoint has to
be somewhere that browser can reach: a LAN address or a NAS is not, and under an https instance a
plain-`http` endpoint is a boot refusal naming `MEDIA_PRESIGN_ENDPOINT` (§33). Proxy mode is the
shipped default, works for every endpoint including a private one, and is what the block prints. If
your bucket **does** have a secure origin a stranger's browser can reach — a hosted provider, or your
own server with a certificate and a public name — switch that one line to `redirect` and read §33
first; it takes this box out of the video path and changes nothing about the bucket's privacy.
On a Cloudflare tunnel the wizard prints `redirect` here instead, because there it is not a choice
(§35.5) — and a NAS on the LAN is then not a usable target at all.

### 35.4 The free-tier facts, and the date somebody read them

The page prints this table's substance with the date beside it and **never refreshes it**: there is
no price lookup and no call home anywhere in this product (§31). Every figure was read first-party on
**2026-09-13** on the page named.

| provider | free storage | card at signup | egress | storage list price | read on |
|---|---|---|---|---|---|
| **a directory** | the disk you already own | — | your uplink's | 0 | — |
| **Backblaze B2** | "First 10GB storage is always free" | **no** — the account can be created and the 10 GB used without one (sourced to the sign-up flow, not the pricing page) | "Free 3x monthly egress", then $0.01/GB; free above that through partner CDNs | $6.95 / TB / mo = $0.00695 / GB-month | backblaze.com *B2 Cloud Storage Pricing* |
| **Cloudflare R2** | 10 GB-month, Standard only; plus 1 M Class A and 10 M Class B a month | **effectively yes** — "complete the checkout flow to add an R2 subscription" | **$0** — egressing directly from R2 incurs no data-transfer charge | $0.015 / GB-month Standard ($0.01 Infrequent Access) | developers.cloudflare.com *R2 pricing*, *Get started*, *API tokens* |
| **Amazon S3** | **no allowance worth planning around** — a new account gets up to $200 in credits over 6 months and the account then closes | **yes** | 100 GB/month free, then $0.09/GB | $0.023 / GB-month (Standard, first 50 TB, US East N. Virginia) | aws.amazon.com/free; price and egress from the shipped `s3` preset |

Two consequences worth stating once: **B2 is the free-tier recommendation and R2 is the
no-egress-bill recommendation**, and **both free tiers are always-free rather than trials**. That
last one is what makes this container genuinely free for a small server — the storage is the
customer's, it costs them nothing, and it was never ours.

**The yearly re-read is a requirement, not a promise.** The page prints `labels read 2026-09-13` once, in
a small line under every walk's list (§20 errata 309-311), and the maintainer's cleanup list carries the entry that asks a
future pass to read the five walks' label hints against the live consoles again and move that date.

### 35.5 On a Cloudflare tunnel: name the risk, never refuse

**The wizard walks "media on this machine + Cloudflare" like any other pick, and names the risk on the
walk** (DESIGN.md §20 erratum 348, which withdrew erratum 299's refusal on the owner's verdict of
2026-10-14). Choose the folder while the instance is reached through Cloudflare — or through a Clippet
Pass name, which is a Cloudflare tunnel — and you get the folder's walk with the tunnel's one line
(`TRUSTED_PROXIES`, §4) and, under the block, the same words the desktop app puts on its folder card:
*Clips live in a folder here; the link plays from this machine through Cloudflare's tunnel. Their terms
reserve video for a bucket — at this size the worst on record is an email.* The reason is not ours and
is not a performance opinion: Cloudflare's Service-Specific Terms restrict serving **video** through a
proxied hostname from a non-Cloudflare origin on the Free, Pro and Business plans, and since 25 Aug 2026
their video-policy page says in writing that a public hostname through a **Cloudflare Tunnel** is covered
by the same rule (§33's opening paragraph carries the same statement, which is where
`MEDIA_DELIVERY=redirect` came from). At a personal instance's size the worst on record is an email, which is what the card's sentence
rests on. The two ways below are how to take the risk away.

The wizard guesses how you are reached from the request itself — anything that comes through
Cloudflare carries a `CF-Ray` header — and the `Reached` control on the chooser (`directly` · `via
Cloudflare` · `via a Clippet Pass`; its state cell reads `guessed` until you choose) lets you say
otherwise, in either direction. It reads that header for its presence only; nothing about it is a
permission.

That control has a **third** answer, *via a Clippet Pass*: the same blocks and the same risk line on the
folder's walk, plus the release line, the three `.env` lines a name on `clippet.gg` needs and
`compose.pass.yaml` on the file list (§36). It is never guessed — a Pass name carries `CF-Ray` like any other tunnel, and a
page that guessed it would be printing a paid product's instructions at everybody behind a tunnel of
their own — so you choose it, and choosing it changes nothing about how this instance behaves.

**Way one: put the media in a bucket.** On this exposure every bucket block sets
`MEDIA_DELIVERY=redirect` (§33), so the tunnel carries HTML and a 300-byte redirect and the video
comes from the bucket. On R2 the bytes are on Cloudflare's own platform, which the rule allows
explicitly; on B2 or S3 they never touch Cloudflare at all. The bucket stays **private** either way —
a redirect is a link this instance signs and that expires, not public access, and it still needs no
CORS rule and no bucket policy.

Two consequences of that `redirect`, both of which the wizard states on the page. The "any other
S3-compatible bucket" walk prints `proxy` on **every other** exposure and `redirect` only here
(§35.3) — behind a tunnel it is the terms that decide, not the target. And because a redirect goes to
the viewer's own browser, the bucket must be one that browser can reach: **a NAS or a LAN address is
not a way out of the terms.** For that case the answer is a real bucket, or way two.

**Way two: be reached some other way, and keep the media here.** Three shapes, and each is somebody
else's product with its own documentation, which is why this is a paragraph and not a walk. A **port
forward with your own domain and TLS**: forward 443 to a reverse proxy on the host (§26 has the Caddy
and nginx stanzas; Caddy will get the certificate itself), point the DNS A record at your address,
and do not widen `CLIPPET_PUBLISH` past loopback — the proxy reaches the app over the compose
network. A **relay VPS**: the same reverse proxy on a small rented host, with a WireGuard or SSH
tunnel back to the box that holds the disk; the rental is the price of not port-forwarding, and the
relay's own bandwidth becomes the ceiling. Or **Tailscale** (or any WireGuard mesh) for a **private
tailnet**: nothing is published at all, the people who can watch are the people on your tailnet,
`PUBLIC_BASE_URL` is the tailnet name, and a share link works for nobody outside it — which is the
right answer more often than it sounds for a household or one Discord server's admins. Whichever you
pick, `TRUSTED_PROXIES` must name the proxy and the app port must stay on loopback (§4).

### 35.6 What the wizard deliberately does not do

- **It writes no file.** The container cannot: `.env` lives on the host beside `compose.yaml`.
- **It prints no `admin storage migrate` command, in either direction.** When this instance already
  holds clips and you pick a different backend, one red callout leads the walk — ***5 clips stay where they
  are.*** *Switching does not move them. Move them first: `admin storage migrate` (DEPLOY.md §32.3).*; between
  two buckets *… If the new bucket is a different one, move them first: … This page cannot tell whether it
  is.* — and nothing else (§20 erratum 311). That command is order-sensitive, is refused if it is run *after* the flip,
  and needs a `-v ${STORAGE_DIR_HOST}:/media` bind whose omission silently throws the copy away. A
  page that prints a data-moving command must print the documented one or none. **Move the clips
  first, then paste the block.**
- **It offers no button that moves data**, creates no account on anyone's behalf, calls no provider
  API, fetches no price and contains no link out of the app.
- **It names no credential, no bucket and no endpoint host of yours.** Its state cell prints one of
  `ok`, `pending` or `error: <class>` and never the doctor's own sentence, which interpolates the
  bucket and the endpoint. `admin doctor` is where "which bucket am I on" is asked and answered.

---

## 36. Clippet Pass: a name on clippet.gg, the official bot, the portal

**A Clippet Pass is optional and paid, it is the maintainer's, and the free product is complete without it —
this instance is public, unlimited and finished whether or not one exists.** What a Pass sells is the
one thing a self-hoster cannot easily give themselves: a name on a domain somebody else keeps working
(`northlight.clippet.gg`), reached over TLS, with no port forward, no certificate to renew, no
Cloudflare account and no credential of ours on your disk. The clips, the database, the bucket and
the bytes stay exactly where they are; what changes is the address in front of them.

### 36.1 What the product does for a Pass: nothing

There is **no pass mode**. No configuration switch, no build flag, no code path in this binary that
behaves differently under a name than it does on `localhost`. The one thing the app owes is a
route — `GET /api/instance`, which is registered on every instance ever built (API.md, DESIGN.md §20
erratum 303) — and the one thing the operator adds is a **second container**, `clippet-pass-edge`,
which is a separate image with its own release cycle. The app never talks to it, never learns it is
there and never asks whether it is. The whole of the product's side is prose, a compose file, one
self-describing route and a page in the wizard.

That is a deliberate shape and not an accident of scheduling: a feature flag for a paid tier inside a
free self-hosted binary is a thing people would have to read the source to disbelieve.

The desktop app's Storage screen drives `admin storage migrate` (§32.3: `MIGRATE_S3_*`, `--manifest`,
`--stop-file`, `--max-bytes`) and opens `/setup?view=steps` (§35.1) — the same verb and the same page a
self-hoster has; the product is never told the app exists.

### 36.2 Running one

**First, check out the latest vYYYY.MM.DD tag and set CLIPPET_COMMIT to its commit.** A box built from
an arbitrary checkout is one the plane cannot match to a release (§36.3, §17.4), and with
`CLIPPET_COMMIT` unset the portal calls this Clippet behind for ever:

```sh
git fetch --tags && git checkout vYYYY.MM.DD          # the newest tag `git tag --list 'v*'` prints
git rev-parse 'vYYYY.MM.DD^{commit}'                  # paste these forty characters as CLIPPET_COMMIT
```

Three places print the rest — the wizard, the portal's Self-hosting fold and the lines below — and they
agree.

**The wizard.** `/setup` (§35), pick your store, and set the second control to *via a Clippet
Pass*. The block it prints is your store's own block with `compose.pass.yaml` on the file list,
opening with the release line and ending with the name's three. Paste it, `docker compose up -d`.

**By hand**, on a bucket (the folder's own file is `compose.fs.yaml` in place of `compose.nobucket.yaml`):

```sh
CLIPPET_COMMIT=<the forty characters above>
COMPOSE_PATH_SEPARATOR=:
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml:compose.pass.yaml
PUBLIC_BASE_URL=https://<your name>.clippet.gg
SESSION_COOKIE_NAME=__Host-clippet_session
CLIPPET_PASS_TOKEN=<the token from your Clippet portal>
TRUSTED_PROXIES=172.16.0.0/12
```

The bot's three lines are §30.8's; the portal's block carries the rest. `COMPOSE_PATH_SEPARATOR=:` is
what makes the `:` list work on Windows, and is inert elsewhere.

Most of those are ordinary and one is load-bearing twice over. `PUBLIC_BASE_URL` and
`SESSION_COOKIE_NAME` are the **app's** own and must be right from the **first boot on the name**:
every card tag and every share link is absolute and is built from the base URL, so a round of links
minted under the old one names the wrong host for ever, and the `__Host-` cookie prefix is refused on
a plain-http base (§19), so it cannot be added as an afterthought either. `CLIPPET_PASS_TOKEN` is the
**sidecar's** and reaches no `internal/config` reader at all — the same relationship
`CLOUDFLARE_TUNNEL_TOKEN` has with §3's tunnel. It is what buys you never holding a Cloudflare
credential: the sidecar exchanges it for whatever the current substrate needs, and a leaked token is
revoked in the portal without anybody touching a tunnel.

**A folder works; a bucket with `MEDIA_DELIVERY=redirect` takes the risk away.** A Pass name is a
Cloudflare tunnel, and §33 and §35.5 are the whole of that argument: Cloudflare's Service-Specific
Terms restrict serving video through a proxied hostname from an origin that is not theirs. The wizard
walks the folder on a Pass name and names that risk on the walk (DESIGN.md §20 erratum 348, which
withdrew erratum 299's refusal); the Pass itself does not judge it either — since verification-plane
amendment 17 (2026-10-14) a name in proxy mode gets no banner, no clock and no disable. The risk under
Cloudflare's terms is yours. R2 is the obvious bucket and is on Cloudflare's own platform; B2 and S3
work and never touch Cloudflare at all.

**What the logs should say.** `docker compose logs -f edge`, in order:
`{"event":"attach","outcome":"ok",…}` and then `{"event":"child_start","outcome":"ok",…}`, after
which the portal's *waiting for your Clippet to connect* becomes *up*. Two lines are worth
recognising. An `{"event":"attach","outcome":"no_state"}` **on the line after the `ok`** means the
tunnel is up but the credential is not being cached: the box works now and comes back dark from a
restart taken while the control plane is unreachable — fix the volume, change nothing else. A
`{"event":"attach","outcome":"bad_token"}` every few seconds is the `.env` line; correct it and
`docker compose restart edge`. Every line that container writes is three fields — an event, an
outcome and a duration — and no request it proxies is logged at all.

### 36.3 What the Pass reads from this instance

Three routes, from the Pass's own network, a few times an hour:

- **`GET /readyz`** — is it up. A box that stops answering is flagged and never disabled: a gaming
  PC that sleeps is the normal case.
- **`GET /api/instance`** — which release is answering (`version`, `build`) and how it serves media
  (`media_delivery`, `media_origin`). This is why `CLIPPET_COMMIT` is worth setting (§16): with it
  unset `build` is `null`, nothing can be looked up, and the portal says *your Clippet is behind*.
- **`GET /api/bot/claim?guild_id=…`** — for each Discord server you bound, whether this box still
  consents, and to which application (§30.8).

What a name may serve, and what happens when a box answers otherwise, is the Pass's own terms
(§36.4), not this product's: nothing in this binary behaves differently under a name (§36.1).

### 36.4 The legal half

The Pass's **Terms** and **Privacy** are served by the official bot's claim page, at
`bot.clippet.gg/claim/terms` and `bot.clippet.gg/claim/privacy`; both in force since 2026-10-14, and
abuse reports go to `abuse@clippet.gg`. A self-hosted
instance's operator stays responsible for their own content — the Pass is a conduit that carries
bytes and selects none of them, and the terms say so in one line.

## 37. The homepage on the apex

**This section is about clippet.gg alone; an instance you run yourself has none of it.** On
`clippet.gg` a Cloudflare Worker answers an anonymous `GET` or `HEAD` of the exact root `/` with the
product's homepage and passes everything else through to the instance unchanged: a request carrying
the session cookie, any other method or path, and any request carrying the header
**`X-Clippet-App: 1`**. That header is the seam for anything that must see the product's own front
door on that origin — the E2E harness (`scripts/e2e.sh`), the visual pass (`scripts/lib/visual.js`)
and the public smoke (`scripts/smoke-public.sh`, T3) all send it, so a run against `https://clippet.gg`
asserts on the app, not the homepage. The app itself ignores the header; on a self-host it changes nothing.

---

## 38. What this instance records about people

If you run Clippet for other people, you are the one responsible for what it keeps about them, and any privacy notice
or imprint your jurisdiction expects is yours to publish. This section is the inventory to write it from. The product
ships no policy text and shows none: a watch page carries nothing but the wordmark (§28), and the front door nothing
else, by design. The maintainer processes no data of your users; nothing leaves the instance except what §38.3 and §38.5
name.

### 38.1 Accounts

| What | Where | For how long |
|---|---|---|
| The username, a bcrypt hash of the password, the creation time | `users` | until the account is removed (§38.6) |
| The account's upload defaults (lifetime, unlisted, link style) and the owner flag | `users` | the same |
| The Discord user id, Discord username and the time of linking | `users` | only after the account's owner links Discord in Settings (§19.6); the OAuth scope is `identify` alone, so no email and no server list is ever read; the Settings button or `admin users unlink` clears the three columns |
| A browser session: a sha256 of the cookie (never the cookie itself), created, expires, last seen, and the browser's `User-Agent` string capped at 256 bytes (shown back to the user under Settings → Browsers) | `sessions` | until sign-out, `SESSION_TTL` (30 days, sliding, under the `SESSION_MAX_TTL` ceiling, §19.3) or `admin sessions revoke` |
| An API token: a hash, its prefix, name, scopes, last use, revocation time | `api_tokens` | until revoked (§19.5) |
| A one-time Discord sign-in link: the Discord id, username and display name | `signin_tokens` | 300 seconds (§30.12) |
| A Discord server binding: the server id and the account it routes to | `discord_guilds` | until unbound (§30.3) |

### 38.2 Clips

| What | Where | For how long |
|---|---|---|
| Title, description, tags, collections, the unlisted flag, lifetime and expiry, sizes, codecs, dimensions, the name of the uploaded file, the trim window and poster choice, and a view counter (one integer; no viewer is stored) | `clips`, `clip_tags`, `collections` | until the clip is deleted or expires (§10) |
| The media: the uploaded ORIGINAL, with whatever the camera wrote into it (GPS, device, capture time), which only the owner can download; the published renditions, which are stripped of that metadata (§27); the poster, GIF and filmstrip | the store, under `clips/<id>/` | the same; the objects go with the row (§24.4 names the orphans) |

### 38.3 Viewers

Nothing about a viewer is written to disk. The first `play` on a watch page POSTs `/api/clips/{id}/view`; the dedupe
key is a truncated hash of the viewer's IP kept in memory for 30 minutes, at most 50,000 entries, and lost at a
restart. Rate limits are keyed on the client IP, in memory. `bytes_served` is one number per UTC day (§31.3). With
`MEDIA_DELIVERY=redirect` the viewer's browser fetches the media from your bucket's own origin, so your bucket vendor's
logs see the viewer (§33); in the default `proxy` mode they see only this instance.

### 38.4 Cookies and browser storage

| Name | Flags | Lifetime | Purpose |
|---|---|---|---|
| `clippet_session` (`SESSION_COOKIE_NAME`; `__Host-clippet_session` on a Pass name) | `HttpOnly`, `SameSite=Lax`, `Secure` when `PUBLIC_BASE_URL` is https | `SESSION_TTL`, sliding | the signed-in session |
| `clippet_session_discord` | the same flags | 10 minutes | the state of a Discord sign-in round trip |
| `theme` | `SameSite=Lax`, `Secure` on https, readable by the page's script | 365 days, or removed for *System* | the light/dark choice of this browser; set only when the visitor presses the theme glyph or picker (§28) |

`localStorage` holds the player's `muted`, `loop` and `quality` choices; `sessionStorage` holds two keys for the
wordmark's animation. None of these identify a person, and none leaves the browser. There is no analytics, no
tracking and no third-party cookie, so no consent banner is owed under the EU's ePrivacy rules: the session cookies are
strictly necessary, and the theme is a preference the visitor sets.

### 38.5 Logs and copies

The access log is one line per request on the container's stdout: method, path, status, bytes, duration, a request id
and `remote`, the TCP peer's address, which behind a tunnel or reverse proxy is the proxy's own. On a sign-in outcome
the line also carries `client_ip`, the address resolved through `TRUSTED_PROXIES` (§4). Paths that carry a token are
written as `/in/{token}`. How long these lines live is Docker's decision, not Clippet's: set a `logging:` block with
`max-size` and `max-file` on the `app` service, or your log driver's retention, and you can write the number down.

Two copies outlive a deletion for a while. The database snapshot in the store (`_clippet/db.sqlite`, §15) is rewritten
at most every `DB_SNAPSHOT_EVERY` (10 minutes) after a change, so a removed row leaves it on the next write. Your own
`admin backup` files hold every row above, session and token hashes included, until you rotate them (§15.5).

### 38.6 Removing a person

A user can do most of it themselves: delete their clips (one, or every ticked row), delete their collections, sign out
every browser, revoke their tokens and unlink Discord, all under Settings. There is no delete-account button, so the
account row itself is yours to remove. The recipe, in this order:

```sh
docker compose exec -T app clippet admin ls --owner <username>            # their clips
docker compose exec -T app clippet admin expire <id>                      # each one; the sweeper removes the objects (§10)
docker compose exec -T app clippet admin collections ls --owner <username>
docker compose exec -T app clippet admin collections rm <id> --owner <username>
docker compose exec -T app clippet admin sessions revoke <username>       # every browser they were signed in on
docker compose exec -T app clippet admin users prune '<username>' --older-than 1s --no-clips   # a duration, must be positive
```

`users prune` refuses an account that still owns a clip or a collection, which is why the clips and collections go first;
it removes the account's sessions and tokens with the row. Then rotate the backups you keep (§38.5).

## 18. Not covered here


- **Metrics, dashboards and an alerting stack.** There is no `/metrics` endpoint and no time series
  of anything: `/healthz` reports the queue's depth, its 24-hour failure count and two ages — and
  `version` and `uptime_s`, the two release observables §16 tells you to check after an upgrade — and §24.2
  says which of those to alert on, but polling it, storing it and paging somebody is yours. The access
  log is structured (`LOG_FORMAT=json`) and is what you would ship somewhere.
- **Log retention.** `compose.yaml` caps the container logs at ~250 MB per service so they cannot eat
  the volumes' filesystem (§8) — that is a ceiling, not retention. Keeping lines for longer than the
  window means shipping them off-box; nothing here does that for you.
- **More than one node.** One container owns the SQLite file, the worker and the sweeper; there is
  no leader election and no shared-nothing story. Scaling up means a bigger machine, more
  `TRANSCODE_CONCURRENCY` (bounded at 16, and sized in §7 — ~0.5 GiB and one core per slot) and a
  bucket that was always remote anyway.
- **Off-host backup retention and encryption.** `admin backup <dir> --rotate N` rotates the
  snapshots it writes on the box (§24.5) and §15 gets the data out of the stack correctly; where it
  then lives, how long, and encrypted with what, is yours.
- **Named tunnels, custom domains and real R2/B2 credentials.** §3, §26 and §33 give the shapes; the
  accounts, tokens and zones they need are yours, and none is in this repository.
