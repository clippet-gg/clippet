<p align="center">
  <img src="docs/brand/og/clippet-card-1200x630.png" width="480" alt="The clippet wordmark on its dark tile, with a green line under the double p">
</p>

# Clippet

**Upload a clip. Get a link.** Clippet is a clip server you run yourself. You upload a video or an
audio file, the server transcodes it, and the link plays as a card in Discord and on X.

## Quick start

You need Docker Engine 25 or newer, or Docker Desktop, with Compose v2. Go, Node and ffmpeg live inside
the image.

```sh
git clone https://github.com/clippet-gg/clippet.git && cd clippet
cp .env.example .env            # every value has a working default
docker compose up -d --build    # one container
```

Open <http://localhost:8481/> and register. Registration takes one account and then closes, so a public
URL never shows a signup form. Upload a clip and share its link. The clips land in `~/Videos/clippet`
on this machine. Back up that folder and you have backed up the instance.

> [!IMPORTANT]
> On Linux, Docker creates that folder owned by root and the app runs as uid 10001. Give it to that uid
> once, then restart the app. Docker Desktop on macOS and Windows needs no such step.
>
> ```sh
> sudo chown -R 10001:10001 ~/Videos/clippet
> docker compose restart app
> ```

The first build compiles the server and copies in a static ffmpeg, so expect a few minutes the first
time and seconds after. Stop the server with `docker compose down`. The clips and a copy of the
database stay in the folder; the live database is the volume `clippet_app_data`, which only
`down -v` removes. To serve on another port, set `CLIPPET_PUBLISH=127.0.0.1:PORT` in `.env`. To keep
the clips somewhere else, set `STORAGE_DIR_HOST` before the first `up`.

Configuration is read once at boot. After editing `.env`, run `docker compose up -d` to recreate the
container. `clippet admin users create` adds people after the first account
([accounts](DEPLOY.md#19-accounts-sessions-and-tokens)).

## Going public

A card needs a public https origin. X draws a player only over https. Discord fetches the card from
its own servers, so a `localhost` or LAN-only origin gets a bare link.

For a quick try, one `docker run` brings up a throwaway Cloudflare tunnel with no account, no port
forward and no certificate ([quick tunnel](DEPLOY.md#3-going-public-cloudflare-quick-tunnel)). For
anything permanent, use a named tunnel on a domain you own, or your own reverse proxy with Caddy or
nginx ([your own domain](DEPLOY.md#26-behind-your-own-domain-caddy-and-nginx)).

Whichever origin you end up on, set `PUBLIC_BASE_URL` in `.env` to exactly what the browser types.
Every card URL is built from it, and a mismatch fails silently.

## Where the clips live

A folder on this machine is a complete instance. A NAS is the same folder, mounted; set
`STORAGE_DIR_HOST` to point at it ([a directory](DEPLOY.md#32-a-directory-instead-of-a-bucket)).

A bucket you own is optional: Cloudflare R2, Backblaze B2, Amazon S3, or any S3-compatible server.
Open <http://localhost:8481/setup>, answer five questions, paste the `.env` block it prints, and press
**Check now** to prove the store answers. The wizard collects no credential
([the setup wizard](DEPLOY.md#35-where-clips-live-the-setup-wizard)). With a bucket, media can be
served from the bucket's own origin instead of streaming through the app
([serving by redirect](DEPLOY.md#33-serving-media-by-redirect)).

`clippet admin storage migrate` moves an existing instance between a folder and a bucket, either way.

## What it does

**Sharing** ([quality](DEPLOY.md#27-quality-tiers-original-full-light), [links](DEPLOY.md#29-links-what-a-share-url-looks-like-and-the-two-id-grammars))

- The link plays inline in Discord and on X. The card names your host, shows the title and the length,
  and its edge turns green when the clip is ready.
- A file every browser already plays is served as uploaded. It is never re-encoded, and the one-second
  container fix strips a phone's location and device name.
- Anything a browser cannot play gets one re-encode at its own resolution and frame rate. Large clips
  get a smaller copy for the card, and viewers get a quality picker with real numbers.
- An audio file is a clip too. Its card is a waveform drawn from the clip's own loudness.
- Links are twelve characters, or three words if you prefer: `your.host/aZ3kQ9xB2mLp` or
  `your.host/PurpleUpdateCharacter`.
- Every share page carries oEmbed, so Slack and most forum and wiki software unfurl it too.

**Uploading** ([capture tools](DEPLOY.md#9-capture-tools-sharex-replays-obs-curl), [uploading](DEPLOY.md#23-uploading))

- The browser form takes several files at once. Title, tags and lifetime typed once apply to all.
- ShareX, RePlays, OBS scripts and `curl` post to `/api/upload` with a token. A ShareX config is minted
  in one click. The reply waits until the card works, so the link on your clipboard plays on the first paste.
- A tool that can only POST a file gets an inbound URL. The URL is the credential, and uploading is all it can do.
- A video dropped at the root of the media folder becomes a clip
  ([the drop folder](DEPLOY.md#34-the-drop-folder-put-a-video-in-it-and-it-becomes-a-clip)).
  `clippet admin import` brings in a folder of clips you already have.
- On Android, install the site and Clippet joins the system Share list.
- Each clip can expire after 1, 7, 30 or 90 days, or never
  ([lifetime](DEPLOY.md#10-clip-lifetime-ttl-keep-alive-keep-forever)).

**Discord** ([the bot](DEPLOY.md#30-the-discord-bot-upload-clip-and-enrich-in-your-own-server), [auto-post](DEPLOY.md#11-discord-auto-post))

- A webhook posts every ready clip to one channel, or a channel per tag. A plain JSON webhook posts to a URL of your own.
- The optional bot adds `/upload`, `/clip` and an **Enrich** message command to your server. Build a Discord
  application of your own, or connect the free official bot to your instance.
- Clippet Auto turns a video posted in a bound server into a Clippet link, with one button or no press at all.
  It is off by default and is the only part of Clippet that reads a channel.

**Your library** ([the library](DEPLOY.md#20-the-library), [tags and collections](DEPLOY.md#14-tags-and-collections))

- Tags filter the library and never show on a watch page. Collections live at an unguessable `/c/{id}`.
- Search, a **Mine only** filter, and bulk actions over ticked rows.
- Trim a clip on the server with two handles over a filmstrip of its own frames
  ([trim](DEPLOY.md#12-trim)). Make a looping GIF on demand ([GIF](DEPLOY.md#13-gif-previews)).
- Put a different file under the same link. The address, title, tags and view count stay.
- Pause the player on the frame you want and press **Use this frame** to make it the poster.
- Every viewer can download a ready clip and copy an embed. `?autoplay`, `?muted` and `?loop` work on `/embed/{id}`.

The interface follows your system's theme. Its fonts ship inside the binary, so nothing is fetched to
draw a page. The wordmark's double p is a face: it follows the cursor, blinks when an upload lands and
smiles when a clip is ready. Never under reduced motion.

## Running it

- **The CLI is the admin panel.** `clippet admin` lists clips, jobs, users, sessions and tokens, retries
  and expires, backs up and restores, all inside the container against the same database
  ([operator toolbox](DEPLOY.md#24-operator-toolbox)).
- **`clippet admin doctor`** checks the configuration, both directories, the database, ffmpeg and the store,
  and names every mistake rather than the first. Run it against an edited `.env` before you bring the container up.
- **`/healthz`** reports the version, the queue's depth and its 24-hour failure count. **`/readyz`** pings the
  store live. Neither names a clip or a user.
- **A job a crash left running** is requeued or failed on a timer. `admin jobs unstick` does it by hand.
- **Backups.** `admin backup <dir> --rotate 7` writes a dated snapshot and keeps the newest seven. A copy of
  the database follows the clips into the store by default, so the store alone can bring the instance back
  ([backup and restore](DEPLOY.md#15-backup-and-restore)).
- **Who gets in.** `REGISTRATION` is `first`, `open`, `closed`, `members` or `role:<id>`. The last two make
  the Discord bot the door.
- **Tokens have scopes.** `upload` for a capture tool, `read` for a backup script. Neither can change a password.
- **What it costs.** Type your provider's prices and Settings shows this month's storage bill. A budget line
  warns when the store passes it, and only `STORAGE_BUDGET_HARD=1` makes it a cap
  ([what this instance costs](DEPLOY.md#31-what-this-instance-costs)).
- **Speed.** Every encode is pinned to the container's CPU quota; `APP_CPUS=4` is the one line worth adding
  on a real machine. With an NVIDIA card, the opt-in GPU image encodes several times faster.

## What it does not do

- **No telemetry, no call-home, no update check.** With the Discord features and the webhooks unset, the
  binary makes no outbound request beyond your bucket. The build fails if a second outbound call site appears.
- **No watermarks, no calls to action.** Nothing is drawn onto your video. A watch page has no sign-up, no
  "more from this user" and no ads.
- **No tiers, no per-user quotas, no upsell.** The limits are an upload size cap, a rate limit against abuse
  and, only if you switch it on, one storage line you drew yourself.
- **No social layer.** No likes, comments, followers, feeds or public browse page. View counts are the
  owner's and appear nowhere else.
- **No per-clip passwords.** A password page would be served to Discord's crawler too and would turn the card
  into a plain link. The unguessable id is the authentication; *Unlisted* and a short lifetime are for a
  clip that must not be found.
- **No upload from a URL.** That would be a second outbound connection, and one that could reach into your
  LAN. Fetch on the client and pipe it into `curl`.
- **No multi-tenancy, no federation, no email.** One instance, one store, one group of people. There is no
  password reset, because there is no email.

## Upgrading

Back up first, check out the tag you want, then `docker compose build --pull && docker compose up -d`.
Migrations apply during boot, and `/healthz` reports the version that came up
([upgrading](DEPLOY.md#16-upgrading)). The server is tagged by date, `vYYYY.MM.DD`, and
[`CHANGELOG.md`](CHANGELOG.md) has one line per tag.

## The desktop app and Clippet Pass

**Clippet for Windows and macOS** is this server in a tray app, with the store, the Discord connection and
updates in a window. It is at [get.clippet.gg](https://get.clippet.gg).

**Clippet Pass** is an optional, paid add-on run by the maintainer: a name on `clippet.gg` over TLS with
no port forward and no certificate of your own. The product does nothing different for it, and everything
above is free and complete without it
([what the product does for a Pass](DEPLOY.md#361-what-the-product-does-for-a-pass-nothing)).

## How it is built

One static Go binary holds the HTTP server, the transcode worker and the expiry sweeper. State is
SQLite. A static ffmpeg is copied into the image, so the host needs nothing at runtime. Media is
transcoded on a scratch volume and written to the store under one prefix per clip. Compose brings up
exactly one service. The frontend has no build step. Clippet is a clean-room rewrite from a written
specification; no other clip server's source was read or copied in making it.

Toolchains live in containers, and the tests run in tiers. [`CONTRIBUTING.md`](CONTRIBUTING.md) has
the commands and how a change gets in.

## Documentation

- [`DEPLOY.md`](DEPLOY.md), the operator guide: every setting, going public, day-to-day operation, backup and restore, upgrades.
- [`API.md`](API.md): every route, its auth class and its wire format, for a capture tool, a script or a second UI.
- [`DESIGN.md`](DESIGN.md): the architecture and the design record.
- [`web/STYLE.md`](web/STYLE.md) and [`docs/brand/`](docs/brand/README.md): the visual system and the mark.
- [`SECURITY.md`](SECURITY.md): how to report a vulnerability, and what an operator must know.
- [`THIRD_PARTY.md`](THIRD_PARTY.md): the licences of what is bundled.

Bugs and feature requests go in Issues. Questions about running an instance go in Discussions.

## Licence

Functional Source License 1.1 with Apache 2.0 as its future licence ([`LICENSE`](LICENSE)). Run it,
change it and self-host it freely. The one use it withholds is a competing hosted service, and each
version becomes Apache 2.0 two years after its release. The name, the wordmark and the striped clapper
are trademarks and not part of the licence ([`docs/brand/`](docs/brand/README.md)).
