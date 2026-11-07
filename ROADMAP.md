# ROADMAP.md — the public index of numbered items

Every item Clippet has planned or built since 2026-09-06 carries a number, and the code, the operator
guide and the design document cite it as `ROADMAP #N`. This file is the index those citations resolve
to: one row per number, a neutral heading and one line saying whether it shipped, is open, or is in
part. Numbers are never reused or renumbered; a withdrawn item keeps its row and says so.

The mechanism behind a shipped item is in DESIGN.md §20 (the erratum the row names, or the bundle's range of
errata for the 2026-09-07 items) and the operator's view is in DEPLOY.md (the section the row names). Items that are business decisions rather than
product changes are kept outside this repository and say so in one line.

Hard constraints every item respects: no telemetry or call-home, no watermarks, no tiering, no social
layer, no calls to action on watch pages, view counts private.

---

### 1. Account security: change a password, log out everywhere, operator revocation — shipped 2026-09-07
`PATCH /api/auth/password`, `GET/DELETE /api/auth/sessions`, `admin users password` (errata 105–112).

### 2. `LIBRARY_SCOPE=shared|own` — shipped 2026-09-07
An instance can keep every account's library to itself (errata 113–120, DEPLOY §7).

### 3. The dedicated social rendition, measured first — shipped 2026-09-07
A card-sized copy for the Discord player, behind `SOCIAL_MAX_BYTES` (erratum 121).

### 4. Library search and a "mine only" filter — shipped 2026-09-07
Search, `?mine=1` and the un-cursored `Newest clips` link (errata 113–120, 266); a true reverse cursor is open.

### 5. Poster at a chosen time — shipped 2026-09-07
"Use this frame" on the watch page (errata 127–134).

### 6. A visual trim scrubber — shipped 2026-09-07; the filmstrip 2026-09-09
Two handles instead of two second boxes; the filmstrip behind them (errata 127–134, 220).

### 7. Multi-file, drag-and-drop, cancel and retry on upload — shipped 2026-09-07
Errata 135–142.

### 8. Expiry countdown and local time — shipped 2026-09-07
Errata 127–134.

### 9. `clippet admin doctor` — shipped 2026-09-07
One preflight before the stack goes down (errata 143–150, DEPLOY §25).

### 10. `admin jobs` / `admin ls --state` / `admin retry` — shipped 2026-09-07
See and recover an outage from the CLI (errata 143–150).

### 11. `/healthz` queue observability — shipped 2026-09-07
Queue depth, failure count and ages on the health route (errata 143–150, DEPLOY §24).

### 12. Repair a job stuck `running` — shipped 2026-09-07; the automatic half 2026-09-09
`admin jobs unstick`, then `JOBS_UNSTICK_AFTER` (errata 143–150, 230–231).

### 13. `admin orphans` third report, and `admin stats` free disk — shipped 2026-09-07
Errata 143–150.

### 14. `admin backup <dir> --rotate N` — shipped 2026-09-07
Errata 143–150, DEPLOY §15.

### 15. Batched progress polling for the library — shipped 2026-09-07
Errata 113–120.

### 16. Bulk actions in the library — shipped 2026-09-07
`POST /api/clips/bulk` (erratum 117).

### 17. Add-to-collection from the clip page, and a one-clip-at-a-time collection editor — shipped 2026-09-07
Errata 151–160.

### 18. `oEmbed` endpoint — shipped 2026-09-07
Errata 151–160, API.md.

### 19. Reverse-proxy recipes (Caddy, nginx) — shipped 2026-09-07
DEPLOY §26; a live proof on a real domain stayed open (erratum 162).

### 20. `API.md`, a reference an integrator can use — shipped 2026-09-07
Errata 161–164; the route-coverage tripwire it proposed is open.

### 21. Secure-by-default registration — shipped 2026-09-07
`REGISTRATION` defaults to `first`: open while the users table is empty, closed once an account exists (erratum 106).

### 22. Per-user defaults (TTL, unlisted) — shipped 2026-09-07
`GET/PATCH /api/auth/me/settings` (errata 151–160).

### 23. Owner-only stats strip — shipped 2026-09-07
`GET /api/auth/me/stats`, rendered for the caller alone (errata 151–160).

### 24. Instance storage budget warning, never a cap — shipped 2026-09-07
`STORAGE_BUDGET_BYTES` (errata 151–160; the opt-in hard cap came with #40).

### 25. A read-only token scope — shipped 2026-09-07
`--scopes upload,read` (errata 105–112).

### 26. Discord per-tag channel routing — shipped 2026-09-07
`DISCORD_WEBHOOK_ROUTES` (errata 158–160, DEPLOY §25).

### 27. Phone share-sheet upload — shipped 2026-09-07
`manifest.webmanifest` with a `share_target` (erratum 137).

### 28. Interlaced sources are encoded as-is — shipped 2026-09-07
Errata 121–126.

### 29. Keyboard shortcuts and loop/mute memory on the watch page — shipped 2026-09-07
Errata 127–134.

### 30. Duration and resolution badges on library rows — shipped 2026-09-07
Errata 113–120.

### 31. Clip replacement: re-upload under the same id — shipped 2026-09-07
Errata 135–142.

### 32. Bound an unauthorized large upload's body — shipped 2026-09-07
Errata 135–142.

### 33. A Discord bot talking to the operator's own instance — shipped 2026-09-10, grown since
An interactions endpoint, `/upload`, `/clip`, Enrich, `/settings`, `/signin`, `/connect` (errata 253–257, 268, 315, 316, 320, 323, 341, 342; DEPLOY §30).

### 34. A hosted, per-server plan — a business item; kept outside this repository
The product's side is `GET /api/instance` and one compose overlay (erratum 303, DEPLOY §36).

### 35. Links: ids at the root, memorable ids, one grammar per upload — shipped 2026-09-10
Errata 248–252, migration 0015, DEPLOY §29.

### 36. A front door for strangers and branding on the embed player — shipped 2026-09-10
Erratum 246.

### 37. A plain-filesystem store — shipped 2026-09-13 as part 1 of #40
`STORAGE_BACKEND=fs` (erratum 276, DEPLOY §32).

### 38. The living mark: the wordmark's eyes follow the cursor, and every face — shipped 2026-09-11 and 2026-09-13
Errata 267, 273, 274.

### 39. Two servers, two backends, and a storage-cost line — the storage-cost line shipped 2026-09-11
Erratum 270, migration 0017, DEPLOY §31; the two-server frame became #40.

### 40. Storage v2: the filesystem store, pricing presets, the quota as the headline — shipped 2026-09-13/14
Errata 276, 277; DEPLOY §31.5, §31.6, §32; the timed backup followed as erratum 321.

### 41. The 2026-09-16 general audit's to-do — 25 of 30 closed 2026-09-16; the rest open
Errata 290–292.

### 42. The desktop default for the filesystem store, and a drop folder at its root — (a) and (b) shipped 2026-09-17; (c) not built
Errata 295, 296, migration 0018, DEPLOY §32.1, §34.

### 43. The companion mark: the striped clapper — shipped 2026-09-17
Erratum 301.

### 44. Sign in with Discord on an instance — shipped 2026-09-17
Erratum 302, migration 0019, DEPLOY §19.6.

### 45. The `/setup` wizard on the instance — shipped 2026-09-17
Errata 297–299, 309–311, DEPLOY §35.

### 46. The desktop app updates itself — shipped 2026-10-06, in the desktop app (outside this repository)
No product change.

### 47. "Your PC is off" as a bot DM — shipped 2026-09-18, in the hosted bot (outside this repository)
No product change.

### 48. Capture presets from the desktop app — shipped 2026-09-18
The product's half is `admin tokens mint <user> [--sxcu]` (erratum 308, DEPLOY §19).

### 49. NVENC on by default when the desktop app sees the GPU — shipped 2026-09-18, in the desktop app
The product's half is the hardware-refusal fallback (errata 247, 265, 289).

### 50. Phone uploads through the share sheet — shipped (already built as #27)
Erratum 137.

### 51. Import a folder once — shipped 2026-09-18
`clippet admin import <dir> --owner <user>` (erratum 307, DEPLOY §34.4).

### 52. A founding price — a business item; kept outside this repository

### 53. A server bundle — a business item; kept outside this repository

### 54. The subscription-value playbook — a business item; kept outside this repository
Of the picks it produced, the product-side ones shipped as errata 319 and 322.

### 55. The `/settings` panel's Link defaults line, two follow-ups — open
The line redraws after a save; the second follow-up is in the hosted bot.

### 56. Storage in the desktop app — shipped in part (2026-10-12 to 2026-10-15)
The product's half is `admin storage migrate` (erratum 335, DEPLOY §32.3); (d) became #57; (e) is not promised.

### 57. Profiles: more than one Pass in one desktop app — open
The first large item after the public 1.0 release.

### 58. The Pass sidecar image, published publicly — open until the first public release
The release cycle publishes the image (DEPLOY §36.2).

### 59. The cycle as CI — dropped 2026-10-20
The number stays so that citations resolve.
