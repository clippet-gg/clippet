# Clippet

**Upload a clip. Get a link.** Clippet is a self-hosted clip server: you upload a video or a sound, the server
transcodes it, and the link you get unfurls into a playable card in Discord and on X.

![The card a Clippet link unfurls into: the clip's title, its length and the striped clapper](docs/brand/og/clippet-card-1200x630.png)

```sh
git clone https://github.com/clippet-gg/clippet.git && cd clippet
cp .env.example .env              # every value has a working default
docker compose up -d --build      # one container; the clips land in ~/Videos/clippet
# then http://localhost:8481/ in a browser: register the first account, upload, share the link
```

The link is short — `https://your.host/aZ3kQ9xB2mLp`, or `https://your.host/PurpleUpdateCharacter` if
you prefer words — and it is a **playable card** in Discord and on X instead of a bare URL. Clippet
stores everything in a plain directory on the host by default — a disk, or a NAS share you have
mounted (`DEPLOY.md` §32): one container and no object-storage server at all — or, with a few lines
in `.env`, in an S3-compatible bucket **you** own: Cloudflare R2, Backblaze B2, Amazon S3, or an S3
server you run yourself on your LAN. Either way the running cost is your storage bill and nothing
else. It is single-tenant by
design (you and the people you give accounts to), every clip can carry its own expiry, and the
binary makes no outbound connection except to your bucket, if you use one (and, only if you turn them on,
Discord's own hosts and the one webhook URL you set).

- **Operator guide**: [`DEPLOY.md`](DEPLOY.md) — configuration, going public over https, day-to-day
  operation, backup/restore, upgrades. Everything below links into it rather than repeating it.
- **Architecture**: [`DESIGN.md`](DESIGN.md) — the whole design, with the §20 errata at the end
  binding wherever they differ from the body.
- **API reference**: [`API.md`](API.md) — every route, its auth class and its wire format, for
  building a capture tool, a script or a second UI against this instance.
- **The visual system**: [`web/STYLE.md`](web/STYLE.md); **the mark**: [`docs/brand/`](docs/brand/README.md).
- **Contributing, security, changes**: [`CONTRIBUTING.md`](CONTRIBUTING.md), [`SECURITY.md`](SECURITY.md),
  [`CHANGELOG.md`](CHANGELOG.md) (one line per tag), [`THIRD_PARTY.md`](THIRD_PARTY.md).

## Quick start, in full (about 60 seconds, plus the build)

From a checkout of this repository, with Docker Desktop or Docker Engine 25+ and Compose v2:

```sh
cp .env.example .env          # every value already has a working dev default
docker compose up -d --build  # builds the image, starts the one service: app
docker compose ps             # app (healthy)
curl -sS http://localhost:8481/readyz     # {"ok":true} when the DB and the media folder are both good
```

The clips land in **`~/Videos/clippet`** on the host (your own Videos folder; compose creates it on
the first `up`). On Linux that directory is created owned by root, and the app runs as uid 10001:
`sudo chown 10001:10001 ~/Videos/clippet` once, or set `STORAGE_DIR_HOST` in `.env` to a directory
that uid owns (`DEPLOY.md` §2, §32.1). Back that folder up and you have backed up the instance: the
server keeps a copy of its database in the folder's `_clippet/` (`DEPLOY.md` §15).

Then open <http://localhost:8481/> — a visitor with no session sees the front door (the wordmark, one
line, **Log in**, and **Create an account** while registration is open; `DEPLOY.md` §28 — the wordmark
is the logo, and its companion mark, the striped clapper, is the favicon, the app icon and the Discord
icon, `docs/brand/`) — register
the first account — registration is open **only until that account exists** (`REGISTRATION=first`,
the default; `open` and `closed` are the other two, and `clippet admin users create` adds people
without reopening anything: `DEPLOY.md` §19; an account can also sign in with Discord once its owner
has linked one in Settings — a second key, never a way to register: §19.6) — choose a video in the
upload form, and share the resulting link:
the link unfurls once the origin is reachable from the internet — `DEPLOY.md` §3 gives you one in a
single `docker run`. That is the whole product loop.

**Then answer one question, and the instance walks you through it: where do the clips live?** Open
<http://localhost:8481/setup> — five answers (a folder or a NAS, which is what you are running now, R2,
B2, S3, or any other S3-compatible bucket), each ending in an `.env` block you paste and a
`Check now` that proves the store answers. It collects no credential and has no field to type one
into; `DEPLOY.md` §35 is its other half, with the console links, the IAM policy and the free-tier
figures. Going out through a Cloudflare tunnel, the folder still walks: Cloudflare's terms reserve video
for a bucket, and the wizard says so on the folder's walk.

Clippet stores media in a directory by default (`STORAGE_BACKEND=fs`, `DEPLOY.md` §32), so the stack
runs with no accounts, no keys and no second container. A NAS works the same way: mount it on the
host and point `STORAGE_DIR_HOST` at it. R2, B2, AWS or any S3-compatible server are the `S3_*`
variables (*Bring your own bucket* below, `DEPLOY.md` §7); point them at a real bucket when you want
what only a bucket can do, and `admin storage migrate` moves an existing instance either way. On a
desktop the directory defaults to your own **`~/Videos/clippet`** — the store keeps its objects in a
subfolder, so the root stays yours. Name one account with `DROP_FOLDER_OWNER` and that root becomes a **drop folder**: put a
video in it and it becomes a clip, with no upload step at all (`DEPLOY.md` §34 — it works on a bucket
too, given a folder to watch). For the clips you already have, `clippet admin import <dir> --owner
<name>` walks a folder through the same path once, reads it and changes nothing in it, and imports
nothing the second time you run it (`DEPLOY.md` §34.4). Configuration is read **once at boot**, so after editing `.env` run
`docker compose up -d` to recreate the app container.

## https is not optional (for X)

- **X / Twitter** renders a player card only when the player and the video stream are **https**
  URLs. On an http origin the `og:video:secure_url` tag is not even emitted and the link degrades
  to plain text.
- **Discord** accepts an http *scheme* — but Discordbot fetches the `og:*` URLs from **Discord's**
  servers, so the origin has to be reachable from the internet. A `localhost` or LAN-only origin
  gets a bare link with no preview, whatever the scheme (`DEPLOY.md` §3).
- The product promise is both, so the supported deployment is https. `DEPLOY.md` §3 brings up a
  throwaway Cloudflare quick tunnel in one `docker run` (no account, no port-forwarding, no
  certificates); §5 has the reasoning, and a named tunnel on a domain you own (`compose.tunnel.yaml`,
  §3) or your own reverse proxy (§26) is the path for anything permanent.

Whatever origin you end up on, `PUBLIC_BASE_URL` must equal what the browser types — it is what
every card URL and the cookie `Secure` flag are built from, and a mismatch is silent (`DEPLOY.md`
§3).

## Bring your own bucket

Optional: the folder above is a complete instance. A bucket of your own unlocks one thing a folder
cannot do — **`MEDIA_DELIVERY=redirect`** (below, `DEPLOY.md` §33) — and moves the **database
snapshot** off this machine with the clips, so **`admin restore --from-store`** brings the whole
instance back on another machine from the bucket alone (`DEPLOY.md` §15). By default the app proxies every byte of media
itself, so the bucket stays **private**: no public-read policy, no CORS configuration, no CDN
required. **Create the bucket first** — Clippet never creates one. Select the bucket flavour and set
the `S3_*` lines in `.env` (full reference: `DEPLOY.md` §7, provider matrix in `DESIGN.md` §4; the
`/setup` page prints the whole block for each provider):

```sh
COMPOSE_PATH_SEPARATOR=:                          # Windows splits the list on `;` without it; inert elsewhere
COMPOSE_FILE=compose.yaml:compose.nobucket.yaml   # the app alone, on a bucket
STORAGE_BACKEND=s3
```

```sh
# Cloudflare R2 — endpoint is the account-scoped host, region is literally `auto`
S3_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
S3_REGION=auto
S3_BUCKET=<your-bucket>
S3_ACCESS_KEY=<R2 access key id>
S3_SECRET_KEY=<R2 secret access key>
S3_FORCE_PATH_STYLE=false          # DNS/virtual-host addressing
```

```sh
# Backblaze B2 through its S3-compatible endpoint — the region is part of both
S3_ENDPOINT=https://s3.us-west-004.backblazeb2.com
S3_REGION=us-west-004
S3_BUCKET=<your-bucket>
S3_ACCESS_KEY=<keyID>
S3_SECRET_KEY=<applicationKey>
S3_FORCE_PATH_STYLE=false
```

```sh
# an S3 server of your own on a NAS or another box on the LAN — usually path-style
S3_ENDPOINT=http://nas.lan:9000    # or https:// if you terminate TLS in front of it
S3_REGION=us-east-1
S3_BUCKET=clips
S3_ACCESS_KEY=<access key>
S3_SECRET_KEY=<secret key>
S3_FORCE_PATH_STYLE=true
```

An **empty** `S3_ACCESS_KEY=` or `S3_SECRET_KEY=` on the bucket flavour is a boot error naming the
line; there is nothing to fall back to (`DEPLOY.md` §7). Coming from an older stack that ran the
bundled bucket server? `DEPLOY.md` §32.6 is the upgrade path — your clips are still where they were.

What redirect delivery does: **`MEDIA_DELIVERY=redirect`** keeps every route, every share
link and the Discord card exactly as they are, but the media answers a `302` to a freshly signed
link on the bucket's own origin instead of streaming through the app — so no video byte crosses
whatever sits in front of your box. `proxy`, the default, is the behaviour above (`DEPLOY.md` §33).

## What it does

| | |
|---|---|
| Playable cards in Discord and X | the point of the whole thing: server-rendered tags, one progressive MP4, a poster, and a chrome-less `/embed/{id}` player (`DESIGN.md` §9.1). The card names **your own host** on its top line, leads its description with the clip's length, burns that length into the corner of the poster, and paints the bar down its left edge green when the clip is ready, amber while it is encoding and red if it failed (`DEPLOY.md` §11, §27, §28) |
| Your 1440p60 stays 1440p60 | a clip is no longer squeezed into 720p. Upload something every browser already plays — H.264 or AV1, mp4, 8-bit, one audio track — and Clippet **does not re-encode it at all**: it fixes the container if it has to (a stream copy, under a second) and serves the file you uploaded. The same one-second copy is also what strips a phone's GPS coordinates, device name and capture time out of anything that carries them, so nothing you share publishes where it was filmed. Anything a browser cannot play (HEVC, ProRes, 10-bit, mkv, an interlaced capture) gets one faithful re-encode at **its own** resolution and frame rate instead. `FULL_CODEC=av1` halves that one file if your viewers are modern (`DEPLOY.md` §27) |
| A quality selector on the watch page | more than one tier means a picker beside the player, labelled with real numbers — "Original 2560×1440 60fps · 38.0 MiB" — not "high/medium/low". It works with JavaScript off, remembers your choice in your own browser and nowhere else, and keeps your place when you switch (`DEPLOY.md` §27) |
| A smaller file just for the card | a long, fast, big clip is exactly the one Discord refuses to play inline, so a file above 30 fps, larger than 1280×720 or past `SOCIAL_MAX_BYTES` gets a second, ~half-size mp4 behind the `/social.mp4` the cards already point at (and at `/light.mp4`, its own name). It is made **before** the clip goes `ready`, so `ready` means *the card works* — and when a faithful re-encode is needed too, one ffmpeg writes both copies from a single decode, so nobody ever streams the card's copy as the clip's own file (`DEPLOY.md` §21, §27) |
| Upload from a capture tool | `POST /api/upload` with a Bearer token, plus a `clippet.sxcu` file minted for ShareX in one click; RePlays, OBS scripts and `curl` use the same three ingredients (`DEPLOY.md` §9). The reply is **held until the clip's Discord card actually works** (`CAPTURE_READY_WAIT`, 30s by default), so the link it puts on your clipboard unfurls as a player on the first paste — Discord caches what it extracts per URL, so a link pasted a second too early stays a bare link for as long as that cache lives. A client cannot switch that off, and there is no estimate of the wait anywhere: an estimate of an encode is wrong often enough to be worse than nothing (`DEPLOY.md` §9, §27) |
| Upload from a tool that can only POST a file | an inbound URL — `POST /in/ci_…`, made and revoked beside the API tokens — takes the form `/api/upload` takes and answers the share link as one line of text. The URL is the credential, and uploading is all it can do (`DEPLOY.md` §9) |
| A crawler never caches a card that is not there yet | while a clip is still encoding, Discord, X, Facebook, Slack and Telegram get `503 Retry-After: 30` and a page with no card tags at all, instead of a correct-for-one-minute link-out card they would then remember for hours. A person gets the ordinary page, plus one line saying what is happening and where — "Encoding on the server — the Discord card is not ready yet." (`DEPLOY.md` §27) |
| The card's copy says it is the card's copy | the smaller file a card plays carries one small semi-transparent corner line — "720p30 preview · 1440p60 on Clippet" — and the card's own description states both real numbers. It is the only exception to *no watermarks*, and it is on the only file nobody watches on the site: never the original, never the faithful copy, never a poster or anything you download. Your own words instead, or `LIGHT_TAG_TEXT=off` for none (`DEPLOY.md` §27) |
| Encoding that uses the machine you gave it | every encode is pinned to the container's own CPU quota, because `nproc` inside a container lies about it and the mismatch measured 25 % slower. `APP_CPUS=4` is close to four times faster than the shipped default and is the one line worth adding on a real machine; with an NVIDIA card, the opt-in `gpu` image is **9-11× at one core** (`DEPLOY.md` §27) |
| Upload several files at once | drop a night's clips onto the upload card and they upload one after another, sharing the tags and the lifetime you typed once; cancel the one in flight, retry a failed one without re-picking it, and — with JavaScript off — post a single file the plain way (`DEPLOY.md` §23) |
| Upload from your phone's share sheet | install the site on Android and Clippet joins the system Share list: pick a video in Files or Photos, share it, and you land on the finished clip's page. Android Chrome only, https only, and no service worker is shipped (`DEPLOY.md` §23) |
| Replace the file behind a link | uploaded the wrong take? Put a different video under the same clip: the address, title, tags, lifetime and view count stay, the trim and the GIF reset, and viewers keep watching the old file until the new one is ready — a card you already posted never degrades while it happens (`DEPLOY.md` §23) |
| Light and dark, both for real | the interface follows your operating system's theme and lets you pin the other one in Settings — a cookie in your browser, not a setting on the account. Its two typefaces ship inside the binary, so nothing is fetched from anywhere to draw a page (`DEPLOY.md` §28) |
| The mark reacts | the wordmark's double p is a face: the eyes follow the cursor, and it blinks as an upload lands, smiles when a clip is ready or you sign in, beams when you copy a link or save, gasps at a delete or a 404, frowns — in red — when a clip fails, and glances about on its own now and then. Under a pointer that hovers only, never under reduced motion; the embed wears one smile, on ready (`web/STYLE.md`) |
| Per-clip lifetime, with a keep-alive | *Delete after* 1 / 7 / 30 / 90 days or never, per upload; the owner's **Copy link** resets the countdown and **Keep forever** clears it (`DEPLOY.md` §10) |
| Discord auto-post | set one webhook URL and every clip that becomes ready is announced to that channel; ticking *Unlisted* on an upload is the per-clip opt-out (`DEPLOY.md` §11) |
| `/upload`, `/clip` and Enrich in your Discord server | the optional bot, three commands (and `/settings`, which lets the server's admins change the connected account's upload defaults from Discord): attach a video to `/upload`: a form opens while it uploads (title, delete-after, tags; `DISCORD_UPLOAD_FORM=off`: no form), then the bot posts the share link, which Discord unfurls into the clip's own branded card (your instance's host and mark, the title, the length, the state's colour, the video playing in Discord); give `/clip` a link to a clip already here and it posts that same message at once; or pick **Apps → Enrich** on somebody's message that carries such a link; with `DISCORD_ENRICH_UPLOAD=on`, a video too. Discord's own 10 MB attachment limit is not yours, and the link outlives the attachment; when a file is too big for Discord to even send, `/upload`'s own description says where to upload it instead. It is three variables plus one `clippet admin bot bind`, it answers those three commands and nothing else (rename them with `DISCORD_UPLOAD_COMMAND_NAME`, `DISCORD_COMMAND_NAME`, `DISCORD_ENRICH_COMMAND_NAME` and `DISCORD_SETTINGS_COMMAND_NAME` when a second instance shares the server), and with the variables unset the route does not exist (`DEPLOY.md` §30). Or skip the application entirely and connect the **official Clippet bot** to your own instance: the same three variables with `DISCORD_BOT_TOKEN=managed`, one `admin bot bind`, and a claim page (`DEPLOY.md` §30.8) |
| Clippet Auto on your box | optional and off by default: the instance holds its own connection to Discord and, in the servers bound to an account here, turns a posted video into a Clippet link — `prompt` puts one `Upload to Clippet` button under it, `auto` needs no press — and removes the original; a separate app of your own, the only part of Clippet that reads a channel (`DEPLOY.md` §30.10) |
| Server-side trim | cut a clip down from the share page without re-uploading; the window is always measured on the **original**, so trimming is repeatable and reversible and never stacks re-encodes (`DEPLOY.md` §12) |
| GIF previews on demand | one click turns a clip into a bounded looping GIF at a plain image URL; nothing is generated until somebody asks (`DEPLOY.md` §13) |
| Downloads and an embed snippet | every viewer of a ready clip gets **Download** and **Copy embed** on the share page (the `<iframe>` sized from that clip's own dimensions), and the owner gets **Download original** — the untouched upload, in every state, including a clip whose transcode failed. When that upload is what the page streams (`DEPLOY.md` §27) the same URL is public and inline, and `?download=1` is what asks for the save; it works the same way on `video.mp4`. No paywall, no watermark, no account gate (`DEPLOY.md` §9, §27) |
| Embed parameters | `?autoplay`, `?muted`, `?loop` on `/embed/{id}` for pages of your own — and the share page deliberately ignores them (`DEPLOY.md` §9) |
| Tags and collections | tags with a `?tag=` filter over the library — they are visible to every account on the instance and to anyone holding a clip's link, and never appear on the watch page — and ordered collections at an unguessable `/c/{id}` link (`DEPLOY.md` §14) |
| Search your library, or just your own clips | a search box over titles and descriptions, a **Mine only** filter that searches the moment you tick it, and the tag chips, all composing into one bookmarkable URL that works with JavaScript switched off; every list keeps newest-first order and pages with a token that carries its filters (`DEPLOY.md` §20) |
| A library each account can keep to itself | `LIBRARY_SCOPE=own` makes an account's library its own clips only — the answer to "I run this for four friends and I do not want my job-hunting clip in their library". The default, `shared`, is what earlier versions did, and either way a share link still works for anyone holding it (`DEPLOY.md` §20) |
| A dev box you cannot confuse with the real one | `INSTANCE_BADGE=dev` on a second instance turns the wordmark's green span **grey** everywhere the mark appears — the embedded player included — puts the word beside it and prefixes every page title. Green span = production, grey span = dev; unset (the default) changes nothing at all. Discord's own site line needs no help: it is the instance's hostname, so a card from `dev.example.com` says so (`DEPLOY.md` §7.1, §28) |
| Do it to twenty clips at once | tick the rows — the action bar appears with the first tick — and add a tag, change when they are deleted, add to a collection or delete: one request each, with a per-clip result, so one id going wrong never undoes the rest (`DEPLOY.md` §20) |
| Choose the frame the card shows | pause the player on the moment you want and press **Use this frame**; the choice is remembered against the original upload, so a later trim keeps it whenever the moment survives the cut, and the looping preview opens on the same frame (`DEPLOY.md` §22) |
| A visual trim scrubber, right under the player | drag two handles over the whole original's timeline and watch the player follow them, with **From = now** / **To = now** buttons and full keyboard control — the two numeric second boxes stay, because they are what the API takes and what a browser with JavaScript off still shows (`DEPLOY.md` §22) |
| A filmstrip on the trim track | the track shows a strip of the clip's own frames, and each handle shows the frame of the moment it is naming, so you cut by sight instead of by number. It is one small image, made when YOU open the control (a visitor holding the link can read it but never start the work), remade after a cut, deleted with the clip — and if it cannot be made the scrubber simply keeps its plain track (`DEPLOY.md` §22) |
| Expiry in your own time zone, counting down | every timestamp renders as absolute UTC on the server and is re-rendered in the viewer's locale, with a live countdown once a clip has under a day left; "kept forever" still just says so (`DEPLOY.md` §22) |
| A watch page that answers the keyboard | space, j/l, arrows, m and f drive the player without clicking it first, and mute and loop are remembered in that browser alone — never sent anywhere, never on the embed, whose parameters belong to whoever wrote the page (`DEPLOY.md` §22) |
| Rows that tell you what a clip is | duration over the thumbnail and resolution beside the date, from the measurements the transcode already took; and one progress request for the whole page rather than one per encoding row (`DEPLOY.md` §20) |
| A progress bar that says what it is doing | an encoding row reads *encoding on the server* or *uploading to storage* rather than just moving — the stage of its own transcode, rendered by the server, so it is there with JavaScript switched off (`DEPLOY.md` §20) |
| Pick clips for a collection by sight | the membership picker shows each clip's card image beside its title, so a list of twenty is something you can read (`DEPLOY.md` §14) |
| File a clip from its own page | tick a collection on the clip you are looking at and it is filed — one request per click, so two tabs curating at once both land; the collection page grows a per-row **Remove** and up/down arrows beside the picker it already had (`DEPLOY.md` §25) |
| Slack and friends get the player too | every share page advertises an `oEmbed` document, which is what Slack and a number of forum and wiki packages look for before the Open Graph tags. Discord and X are unaffected — they read the same tags they always did — so this is reach, not a change. Only URLs on your own origin are ever described (`DEPLOY.md` §25) |
| Upload defaults per account | "everything I upload is unlisted and kept a week" is a setting, not a habit — and anything an upload sends explicitly still wins, so a capture tool that names its own lifetime keeps working exactly as it did (`DEPLOY.md` §25) |
| Links you can read out | a share link is your domain and the id, with nothing in between — `your.host/aZ3kQ9xB2mLp`. An account (or one upload) can ask for **memorable** ids instead: three words from the EFF long list, joined (`your.host/PurpleUpdateCharacter`), and four when the upload is unlisted, where the link is the only authentication. Every `/clips/{id}` link ever sent still works — it redirects (`DEPLOY.md` §29) |
| How much is in here | one line above your library: your clips, how many are ready, the bytes they take, your view total and what expires this week. Your numbers and nobody else's — there is no route that answers it for another account (`DEPLOY.md` §25) |
| A storage budget that warns, and blocks only if you ask | `STORAGE_BUDGET_BYTES` puts one line in the log, in `/healthz`, on the library page and at the head of Settings' Storage panel when the bucket passes it. **No upload is ever refused because of it — unless you set `STORAGE_BUDGET_HARD=1`**, the one opt-in that makes the same line a cap (a new clip gets a `507` and one sentence; replacing or trimming an existing clip still works). Off by default, because a budget you set to be *warned* about must not quietly stop your friends uploading. There are still no tiers and no per-user quotas either way (`DEPLOY.md` §25) |
| What it costs you, from your own prices | type what your provider charges per GB (`STORAGE_PRICE_PER_GB_MONTH`, `EGRESS_PRICE_PER_GB`) — or name a dated preset (`STORAGE_PRICING=local\|r2\|b2\|s3`, which also models that provider's free-egress rule) — and Settings and `admin stats` show what is stored and what has been **sent** this month, with a two-decimal estimate beside each and the list and date it came from. The anonymous `/healthz` carries the byte counts and never a price, so the origin publishes nothing about what you pay. Nothing is ever asked of a provider — no API, no price lookup, no second credential; a preset is a table inside the image, not a lookup — and the served-bytes counter is one aggregate number per UTC day with no viewer, no address and no clip in it. Set no price and no preset and there is no cost line at all, rather than a `0.00` that would look like a measurement (`DEPLOY.md` §31) |
| A Discord channel per tag | `DISCORD_WEBHOOK_ROUTES` sends clips tagged `clutch` to one channel and everything else to another, chosen when the clip becomes ready. Unlisted still means never posted, and every webhook URL is still a secret the app never logs (`DEPLOY.md` §25) |
| Change your password, see what is signed in | `/settings` has the password form, the list of signed-in browsers (started, last seen, expires) and a **Sign out everywhere else** button; changing the password signs every other device out in the same breath. Sessions slide while they are used but never outlive `SESSION_MAX_TTL` (90 days) from when they started, so a stolen cookie that is kept warm still expires (`DEPLOY.md` §19) |
| Secure by default the moment it is public | registration accepts **one** account and then closes (`REGISTRATION=first`), so the tunnel in §3 does not hand a signup form to the internet; the effective mode and every new account, with its address, are on the log (`DEPLOY.md` §19) |
| Read-only API tokens | a token is minted with scopes: `upload` for a capture tool, `read` for a backup or sync script — `read` gets the same library listing a browser session does (every clip listed on the instance, plus your own unlisted ones and your own view counts) and your own collections, and can do nothing else. Neither can change your password, sign anything out or mint another token (`DEPLOY.md` §19.5) |
| An operator CLI | `clippet admin stats / ls / jobs / retry / expire / orphans / users / users create / users prune / sessions / tokens / storage migrate [--manifest] [--stop-file] [--max-bytes] / backup / restore-check`, all inside the app container against the same database — including listing the jobs an outage left behind and revoking one user's sessions, or everyone's, after an incident — and `storage migrate` between two buckets with `MIGRATE_S3_*`, a manifest for cheap re-runs, and a stop file or a byte cap that stops it between objects (`DEPLOY.md` §8, §15, §19, §24, §32.3) |
| A preflight before you change anything | `clippet admin doctor` checks the configuration (naming **every** mistake, not the first), both directories and their free space, the database's integrity, ffmpeg, and whether the bucket is reachable **and writable** — then warns about the two footguns that break an instance quietly: the credentials printed in this repository still being in use, and `TRUSTED_PROXIES` beside a port published beyond loopback. Run it against an edited `.env` *before* `up -d` (`docker compose run --rm --no-deps app admin doctor`); unlike `docker compose config` it prints no secret (`DEPLOY.md` §24.1) |
| See an outage, then undo it | `/healthz` reports the queue's depth, its 24-hour failure count and how long the oldest job has been waiting; `admin jobs` and `admin ls --state failed` say which clips ended badly and why; `admin retry` puts them back, and `admin jobs unstick` repairs a job a crash left `running` by hand — which the app now also does by itself (§24.6). Nothing identifies a clip or a user in the endpoint, and nothing is sent anywhere (`DEPLOY.md` §24.2, §24.3) |
| A queue that heals itself | a job left `running` by a crash — a killed container, an OOM'd encode — is repaired by the app itself on a timer (`JOBS_UNSTICK_AFTER`, six hours and a quarter by default, minutes if you want it brisk, `0` to switch it off), with the same repair `admin jobs unstick` performs by hand: requeued inside its attempt budget, or failed honestly once that runs out. A job the running process is actually working on is never touched, whatever its age. `/healthz` counts the repairs and `admin doctor` prints the schedule (`DEPLOY.md` §24.6) |
| Backups that rotate themselves | `admin backup <dir> --rotate 7` writes a dated snapshot, keeps the newest seven and clears its own stranded partials — and considers only the files it minted itself, so nothing else in that directory is ever at risk. Set `BACKUP_EVERY` and `BACKUP_DIR` and the server runs it on a timer; off by default (`DEPLOY.md` §15.6). `admin orphans` now also finds paid-for objects left under a healthy clip by an interrupted trim (`DEPLOY.md` §24.4, §24.5) |
| A deployment that tells you what it is | `/healthz` reports the version you built (`CLIPPET_VERSION`), so an upgrade is confirmed from the outside; the app container has a memory and CPU ceiling (`APP_MEM_LIMIT`, `APP_CPUS`) so one runaway encode cannot take the instance; a bad `.env` names **every** mistake in one go instead of one per restart; and it warns at boot if it is still running on the bucket credentials printed in this repository (`DEPLOY.md` §7, §16) |
| Your own domain, your own reverse proxy | Caddy (automatic TLS) and nginx + certbot recipes that forward to the app's loopback-only publish, raise the upload body/timeout limits to match the app's own, and set the header the app trusts for per-viewer rate limits and view dedupe — an alternative to §3's Cloudflare tunnel for a VPS you already run (`DEPLOY.md` §26) |
| Or rent a name instead, if you would rather not | **Clippet Pass** is an optional, paid add-on — a name on `clippet.gg` (`northlight.clippet.gg`) over TLS with no port forward, no certificate and no Cloudflare account of your own. Nothing in this product changes for it: no flag, no mode, one extra container, and the wizard prints the four `.env` lines. Its Terms and Privacy live on the official bot's claim page, at `bot.clippet.gg/claim/terms` and `/claim/privacy`; everything above is free and complete without it (`DEPLOY.md` §36; [§36.1](DEPLOY.md#361-what-the-product-does-for-a-pass-nothing) says what the product does for a Pass: nothing) |
| An API reference for integrators | every route, its auth class, request fields and response DTOs, with curl examples for both the cookie-jar and the Bearer-token workflow — for a capture tool, a script, or a second UI built against this instance (`API.md`) |

## What it deliberately does not do

- **No telemetry, no call-home, no update check.** With the Discord features (webhook, bot, sign-in) and `CLIP_WEBHOOK_URL`
  unset the binary makes no outbound HTTP request at all beyond `S3_ENDPOINT`; `scripts/check.sh` greps every
  compiled `*.go` under `internal/`, `cmd/` and `web/` and fails the build if a second outbound call
  site appears in any of the common spellings — a tripwire over those, not a proof (`DESIGN.md` §20
  errata 66, 96).
- **No watermarks, no CTAs.** Nothing is drawn onto your video, and a watch page has no
  "sign up", no "more from this user" and no ads — only the `clippet` wordmark in the header, linking
  home, so a viewer knows whose player this is (`DESIGN.md` §20 erratum 244).
- **No tiers, no per-user quotas, no upsell.** The only limits are an upload size cap
  (`MAX_UPLOAD_BYTES` — that is your disk, not a plan), a rate limit that exists to stop abuse, and
  — only if you switch it on — one instance-wide storage line you drew yourself
  (`STORAGE_BUDGET_HARD`, `DEPLOY.md` §25). All of them are yours to set, none of them is per
  account, and nobody is offered a bigger one for money. Clips expire only because someone chose a
  TTL for them.
- **No social layer.** No likes, comments, followers, feeds, trending or public browse page. Tags
  are a filing system, not a surface: they never appear on a watch page.
- **View counts are the owner's.** The count is rendered for the owner and is absent — not hidden —
  from everybody else's HTML and from the API.
- **No per-clip passwords.** A password interstitial is served to Discordbot and Twitterbot too, so it
  turns exactly the clips you most want to control into link-out cards — it breaks the product to add a
  lock. The unguessable id **is** the authentication for a share link (`DEPLOY.md` §14, §29) —
  twelve base62 characters by default, or three words (four on an unlisted upload) if you chose the
  memorable grammar; when a clip must not be findable at all, that is what *Unlisted* and a short TTL
  are for.
- **No upload-from-a-URL.** "Paste a link and we will fetch it" is, by definition, a second place this
  binary opens an outbound connection — the one thing the first bullet above promises does not exist,
  and one that would reach every address inside your compose network and your LAN. Do
  it on the client, where the fetch is yours:
  `curl -sL <url> | curl -F 'file=@-;filename=clip.mp4' -H "Authorization: Bearer $TOKEN" …`
  (`DEPLOY.md` §9).
- **No multi-tenancy, no federation, no email.** One instance, one bucket, one group of people. You
  can change your own password from `/settings`, but there is no *reset* — no "email me a link",
  because there is no email — and no admin web panel: the CLI is the panel (`DEPLOY.md` §19).

## How it is built

One static Go binary (`cmd/clippet`: the HTTP server, the transcode worker and the expiry sweeper
in one process), SQLite through a single-writer connection pool for state, and a static ffmpeg
7.1.1 copied into the image so nothing on the host is needed at runtime. Media is transcoded on a
scratch volume and then written to the store — the media directory, or your bucket; a clip's objects
all live under one prefix, which is what makes deleting it a prefix operation. Compose brings up
exactly one service, the app.

Toolchains live in containers — the host has no Go and no Node, and there is no build step for the
frontend (`web/static/app.js` is ES2020, served as-is). The tests run in four tiers (`DESIGN.md` §20 erratum 331):

```sh
bash scripts/t0.sh             # T0, every commit, no stack: gofmt, go vet, the Go tests from Go's cache, the cheap checks
bash scripts/t1.sh             # T1, once per change, on a stack of your own: T0, -race where goroutines moved, the E2E bundles the diff touches
bash scripts/gate.sh           # T2, at a release or a shape change: the full suite with -race, the whole E2E and the visual pass
bash scripts/smoke-public.sh   # T3, after a deploy: a short smoke against the public origin
```

`check.sh` needs a POSIX shell with `bash` and GNU `grep` (plus `mktemp`), and Docker — no Go, no Node, no
ffmpeg (`DEPLOY.md` §17). `e2e.sh` needs a running stack plus host `curl`, `python3` and ffmpeg 7.1, and is
documented in `DEPLOY.md` §6 and §17.3; `scripts/e2e.d/README.md` is the contract for a new bundle.

**Versions.** The server is tagged by date — `v2026.11.03` — and `/healthz` reports the one an instance runs;
[`CHANGELOG.md`](CHANGELOG.md) has one line per tag. The desktop app has its own version (1.0.0) and updates itself.

**Desktop app.** Clippet for Windows and macOS — the server above in a tray app, with the store, the Discord
connection and the updates in a window — is at [get.clippet.gg](https://get.clippet.gg).

**Provenance.** Clippet is a clean-room rewrite from a written specification. No third-party clip server's
source was read or copied in making it, and every line here is original.

---

Licensed under the Functional Source License, version 1.1, with the Apache License 2.0 as its future licence
(FSL-1.1-ALv2, [`LICENSE`](LICENSE)): run it, change it and self-host it freely; a competing hosted service is the
one use it withholds, and each version becomes Apache-2.0 two years after its release. The name, the wordmark and
the striped clapper are trademarks and not part of the licence ([`docs/brand/`](docs/brand/README.md)).
