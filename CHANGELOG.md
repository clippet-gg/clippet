# Changelog

What each server release changed, one line per tag, newest first. The server is tagged by date
(`vYYYY.MM.DD`, a second tag on the same day adds `.1`); `/healthz` reports the version an instance runs. The
desktop app has a version of its own and is not listed here unless a tag's point was the app. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); the design record behind every item is
`DESIGN.md` §20.

## 2026.11.07

The first release from the public repository github.com/clippet-gg/clippet, under the Functional Source License (FSL-1.1-ALv2): a rewritten README, golangci-lint and govulncheck in CI, a digest-pinned cross-compiled Dockerfile, a personal-data tripwire in check.sh, and the desktop app at 1.0.0.

## 2026.11.03

`SETUP_WIZARD=off` shuts the web `/setup` wizard and its first-run door (the steps view stays; the desktop app sets it); on a Pass box `/setup` answers 404 except for that view; the Discord `/upload` and `/clip` paths never tell a later submit there is no clip when the clip exists.

## 2026.10.30

The Discord bot's deferred reply keeps its link id through a redraw, so a late failure never writes a failure sentence over a link it already posted.

## 2026.10.29.1

A video turned by its Display Matrix — a phone recording — came out sideways on the GPU image; it now comes out upright and matrix-free.

## 2026.10.29

Some phone recordings failed to process after upload: the Light copy encoded from a produced file now addresses that file's own streams.

## 2026.10.28

The 1.0 hardening wave: the database snapshot on a folder home too, and `admin restore --from-store` from it; the folder store's marker is never written over a folder the database says holds clips (the instance serves degraded and says so); the first account is the instance owner and alone holds `/setup`, Check now and the Storage panel (migration 0025); a trim or replacement supersedes a running tier job; `admin storage migrate` and `admin orphans --delete` outlive the ten-minute budget; a backup's part file is created 0600; the chunked upload deletes its session on every give-up; the hosted Clippet Auto's backfill URL bug fixed and its dead-man pings on behaviour; `admin objects` for the harness; `S3_BUCKET` without `S3_ENDPOINT` names the withdrawn bundled server at boot.

## 2026.10.26

The default stack is the folder store: `compose.yaml` is the app alone, a bucket is bring-your-own and the bundled bucket server is withdrawn (`DEPLOY.md` §32.6 is the upgrade path); `STORAGE_BACKEND` unset follows `S3_ENDPOINT`.

## 2026.10.22

No server change; the tag paired the desktop app's first macOS build.

## 2026.10.20.2

No server change.

## 2026.10.20.1

No server change.

## 2026.10.20

No server change.

## 2026.10.19.1

The web upload is chunked — a run of 50 MiB requests, one ingest — so it works under a 100 MB per-request cap at the edge; the wizard's tunnel and Pass blocks name `TRUSTED_PROXIES`.

## 2026.10.19

`/signin`: the Discord bot is the door (`REGISTRATION=members|role:<id>`, a one-time link, migration 0023); a person-scoped server binding (`admin bot bind --person`, migration 0024); the database snapshot follows the clips into the bucket (`_clippet/db.sqlite`, `admin restore --from-store`); the bot's fetch reports its bytes while it uploads; the closed-registration notice names the instance's host; the test tiers run in WSL2 when typed in Git Bash on Windows; `scripts/box-tidy.sh` reports and removes the box's test litter.

## 2026.10.18

Harness only: a race in the capture bundle's guard, the self-test's load pins and the gate record's input key.

## 2026.10.16

`admin storage migrate` uploads in parallel parts (`STORAGE_UPLOAD_THREADS`), proves each object by ETag instead of reading it back, and reports progress per byte.

## 2026.10.15.2

The folder store on Windows: no directory fsync there (the store never wrote), and its ping takes a real object's path; the on-box Clippet Auto counts the plane's routable servers.

## 2026.10.15.1

No server change.

## 2026.10.15

No server change.

## 2026.10.14.1

`STORAGE_DIR` and `BACKUP_DIR` take a Windows UNC share.

## 2026.10.14

`admin storage migrate` bucket to bucket (`MIGRATE_S3_*`, `--manifest`, `--stop-file`, `--max-bytes`); `/setup?view=steps`; `STORAGE_DIR` and `BACKUP_DIR` take a Windows drive-letter path; Clippet Auto saves as each act completes and re-reads its settings at the timer and the press.

## 2026.10.12.1

The hosted Clippet Auto (`cmd/clippet-auto`, dispatching to the boxes); `GET /api/bot/claim` answers the bound account's link defaults for the bot's `/settings` panel.

## 2026.10.12

Clippet Auto on the box: the instance holds its own Discord gateway connection and, in a bound server, turns a posted video into a clip link.

## 2026.10.09

The audio card's bars are loudness, not peaks; the bot redraws a link whose card was not ready when it posted; `Changes saved.` after a settings save; the test tiers T0–T3.

## 2026.10.07.1

A clip can be sound: an audio upload is stripped and remuxed, tiered to FLAC and AAC, served at `/audio` and drawn as a waveform card (migrations 0021–0022); the Clippet Auto URL's in-flight and declared-size caps.

## 2026.10.07

A signed webhook on every new clip (`CLIP_WEBHOOK_URL`); the `/upload` form in Discord (`DISCORD_UPLOAD_FORM`); timed database backups (`BACKUP_EVERY`, off by default); the inbound URL `POST /in/{token}`; Enrich takes a video (`DISCORD_ENRICH_UPLOAD`, off by default); the Clippet Auto URL `POST /in/auto/{token}`; a byte-identical re-upload within one account answers the existing clip.

## 2026.10.06

The Discord bot's `/upload`, `/clip` and Enrich; the share page declares its own Discord component card (`CARD_COMPONENTS`); a capture tool's timestamp in a file name becomes a date-and-time title; the instance's side of `/settings` from Discord (the link defaults).

## 2026.10.04

The Light tag's drawtext paths parse on Windows; the upload card shows a long file name in its own cell.

## 2026.09.25

The first tag: the server the desktop app's first installer was built against; the `/setup` wizard rebuilt as a numbered route with radio cards and red callouts.
