# clippet — orientation for a coding session

This is the source of Clippet, a self-hosted clip server: upload a video or a sound, the server transcodes it,
the link unfurls into a playable card in Discord and on X. One Go binary, SQLite, a static ffmpeg in the image,
a folder or an S3-compatible bucket as the store. The product's own front door is `README.md`.

## Read first

1. `README.md` — what it is, the quick start, how it is built.
2. `DEPLOY.md` — the operator guide; its section numbers are stable ids cited everywhere.
3. `API.md` — every route, its auth class and its wire format.
4. `DESIGN.md` — the architecture; **§20's errata at the end are binding** wherever they differ from the body,
   and their numbers are never reused or renumbered.
5. `web/STYLE.md` — binding for any change to the templates, `web/static/app.css` or `app.js`.
6. `scripts/e2e.d/README.md` — the contract for an end-to-end bundle.

## The tiers

Never unit tests alone, never the whole suite per change (`DESIGN.md` §20 erratum 331).

```sh
bash scripts/t0.sh          # T0, every commit, no stack
bash scripts/t1.sh          # T1, once per change, on a stack of your own (.env with a compose project of its own)
bash scripts/select.sh --why    # what a diff owes, and why
bash scripts/gate.sh        # T2, the full suite: a release, a shape change, or when T1 stops with "needs T2"
```

Toolchains live in containers; the host needs Docker, bash, GNU grep, curl, python3 and ffmpeg 7.1 only.

## Rules a session keeps

- **Citations**: a comment cites `DESIGN.md §20 erratum N` or `ROADMAP #N`, never a person; dates and lane names
  may stay.
- **No URL literal** (`http://` or `https://`) in a comment or string under `internal/`, `cmd/` or `web/` —
  `scripts/check.sh`'s outbound-connection tripwire fails on one. Name the origin in words.
- **No secret in the tree**: `.env`, `.e2e/`, `.secrets/`, `backups/` are ignored; never read a value out of
  one into a log, a commit or a reply.
- **Anti-features** are binding: no telemetry or call-home, no watermarks, no tiers or quotas, no social layer,
  no CTAs on a watch page.
- **Commit subjects** under 72 characters, plain; every commit carries `Signed-off-by:` (`CONTRIBUTING.md`).
- **LF line endings** (`.gitattributes`); `gofmt`; `web/static/app.js` is ES2020 with no build step.
- **Port 8481** on loopback is the default publish (`compose.yaml`); a second stack takes a project name and a
  port of its own in its `.env`.
- Records of how the project was run — plans, run reports, release notes beyond `CHANGELOG.md` — live outside
  this repository. A `CLAUDE.local.md` beside this file (gitignored) says where, when the clone has one.
