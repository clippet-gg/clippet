Drop-in E2E steps live here (see scripts/e2e.sh, `run_dropins`, just above step 16). One file per
bundle, named <first step number>-<bundle>.sh. Keep this file so the directory is tracked.

**The host** (DESIGN §20 erratum 338): Linux or macOS with Docker, curl, ffmpeg ≥ 6.1 and python3; Windows through
WSL2 (`scripts/lib/wslexec.sh` re-runs the harness there). On Windows, `scripts/gate.sh`, `t0.sh` and `t1.sh` typed in
Git Bash re-run themselves in WSL2 on an ext4 clone (`$HOME/dev/clippet` inside WSL by default, and a worktree of it
per lane) at the same commit; `W()` is the identity there and the MSYS rules below are inert. `CLIPPET_WSL=0` keeps the
Git Bash host (native curl, ffmpeg, `python`), where every `W()` and MSYS rule still applies. NEVER run `e2e.sh` or
`visual.sh` bare in that WSL clone: compose.yaml's project name is `clippet`, and the clone's `.env` is only a guard
(`clippet-never`, wslexec.sh writes it; the reference `.env` is never copied there) — only through `gate.sh` (the gate
stack) or `t1.sh` (a lane's worktree and `.env`).

The files today, in name order (DESIGN.md §16.3 names the same list). **108 steps / 49 bundles**: setup
steps 1-4 and 6 serial, then the pool, then the exclusive 87-storage, then 16 and 17 serial — the closing
banner's phases line reads `serial setup | pool | exclusive`. The first six rows were scripts/e2e.sh's own
serial core until run 9 (lane `pool`; "The core in the pool" below):

| file | steps | schedule |
|---|---|---|
| 7-clip.sh | 7-14 | parallel |
| 15-ttl.sh | 15, 19 | parallel |
| 18-capture.sh | 18, 29 | parallel, apart from 47-upload, 62-tiers and 103-audio |
| 20-pages.sh | 20, 23, 27, 30 | parallel |
| 21-trim.sh | 21, 24 | parallel |
| 22-gif.sh | 22 | parallel, apart from 43-watch and 70-web |
| 31-nits.sh | 31-32 | parallel |
| 33-account.sh | 33-36 | parallel |
| 37-library.sh | 37-40 | parallel |
| 41-social.sh | 41-42 | parallel |
| 43-watch.sh | 43-46 | parallel, apart from 70-web |
| 47-upload.sh | 47-50 | parallel, apart from 18-capture |
| 51-admin.sh | 51-54 | parallel |
| 55-owner.sh | 55-60 | parallel |
| 61-nits2.sh | 61 | parallel |
| 62-tiers.sh | 62-66 | parallel, apart from 18-capture |
| 67-capture.sh | 67-69 | parallel, apart from 41-social |
| 70-web.sh | 70-72 | parallel |
| 73-ops.sh | 73-76 | parallel |
| 77-design.sh | 77-79 | parallel |
| 80-front.sh | 80-81 | parallel |
| 82-links.sh | 82-84 | parallel |
| 85-bot.sh | 85-86 | parallel |
| 87-storage.sh | 87 | exclusive |
| 88-fsstore.sh | 88 | parallel |
| 89-quota.sh | 89 | parallel |
| 90-media.sh | 90 | parallel |
| 91-drop.sh | 91 | parallel |
| 92-setup.sh | 92 | parallel |
| 93-signin.sh | 93 | parallel |
| 94-instance.sh | 94 | parallel, apart from 85-bot |
| 95-import.sh | 95 | parallel, apart from 91-drop |
| 96-mint.sh | 96 | parallel |
| 97-hook.sh | 97 | parallel |
| 98-form.sh | 98 | parallel |
| 99-backup.sh | 99 | parallel |
| 100-inhook.sh | 100 | parallel |
| 101-autofetch.sh | 101 | parallel |
| 102-dedupe.sh | 102 | parallel |
| 103-audio.sh | 103 | parallel, apart from 18-capture |
| 104-audiocard.sh | 104 | parallel |
| 105-migrate2.sh | 105 | parallel |
| 106-botsignin.sh | 106 | parallel |
| 107-snapshot.sh | 107 | parallel |
| 108-person.sh | 108 | parallel |
| 109-chunked.sh | 109 | parallel |
| 110-autohost.sh | 110 | parallel |
| 111-autobot.sh | 111 | parallel |
| 112-rotated.sh | 112 | parallel |

A file's own number IS its first step, per the naming convention above — add a row when you add a file.

## How to run it, by tier (run 9; DESIGN.md §20 erratum 331)

Nobody runs the whole suite to check a change any more. Which part runs, and where, depends on the tier:

- **T0** (`bash scripts/t0.sh`, every commit, no stack) runs only `E2E_SELFTEST=1`. That is the check on this
  directory itself: the namespace registry, the change map's coverage, the retry classifier and the scheduler.
- **T1** (`bash scripts/t1.sh`, once at a lane's end, on the lane's own stack) runs `E2E_ONLY=$(bash scripts/select.sh)`.
  These are the bundles whose `# e2e-covers:` lines match the diff.
  - `none` runs no bundle.
  - `all` (a CORE, uncovered or unknown path) runs the whole suite, with a warning.
  - A shape path stops T1 with "this diff needs T2".
  - `T1_WHY=1` prints why each path selected what it did. A fixer re-runs only its own slice with `T1_ONLY=<stem>`.
- **T2** (`bash scripts/gate.sh`) runs the whole suite with no environment, on the local `clippet-gate` stack. It is
  owed at a release, for a shape change, in the nightly slot, or when the owner asks. It is never run per lane or per merge.
  `E2E_RESUME=1` still makes a resumed run plus the earlier runs one gate, as long as the product tree is unchanged.
- **T3** (`BASE=https://<your instance> bash scripts/smoke-public.sh`, after a deploy) is not this suite. It is about two minutes against the
  public origin.

`E2E_ONLY=<stem>` and `BASE=… bash scripts/e2e.sh` still work by hand. A run with `E2E_ONLY` set says
`PARTIAL` and is never a gate.

## How a new drop-in is scheduled

Every file declares, in a comment inside its first 40 lines, how it may be RUN:

```sh
# e2e-schedule: exclusive           # it runs ALONE, after every parallel bundle, in name order
# e2e-schedule: parallel            # it runs beside the other parallel bundles, in a subshell
# e2e-schedule-apart: <stem>[,...]  # optional: it must never run AT THE SAME TIME as those (symmetric)
# e2e-schedule-after: <stem>[,...]  # optional: it must run AFTER those bundles
```

`parallel lane=<n>` was the syntax until lane `speed2` and is now FATAL with a message naming the new
one: a branch written against the old syntax would otherwise lose the conflicts its lane number stood
for, silently.

**`e2e-schedule-after` makes a precondition a rule instead of a coincidence.** The exclusive phase's
order used to be load-bearing — 47-upload asserted the owner expressed no TTL preference, which
55-owner's step 57 set and cleared — and name order happened to satisfy it. (Since lane `excl` step 57
does that to an account of its own and neither bundle declares an order; the rule stays for the next
pair that needs one.) The harness checks every declaration BEFORE any bundle runs:
an exclusive bundle may name any parallel bundle (the whole parallel phase finishes first) or an
exclusive one earlier in name order; a parallel bundle may name any other PARALLEL bundle, and the pool
simply does not start it until that one has finished (passed or failed); it may not name an exclusive
one, which cannot have run yet. Anything else is fatal, and so is an `apart` that names no drop-in.
A predecessor a FILTER left out is fatal in a full run and a loud NOTE in a filtered one: `E2E_ONLY`
used to drop a precondition and still print a PASS line.

**A file with no marker is exclusive, and the harness says so out loud when it runs.** That is the
default a new drop-in keeps until somebody has shown otherwise, because every one of the reasons below
was found by reading a bundle that looked innocent. Mark a bundle `parallel` only when ALL of these
hold, and write the reason under the marker (every file here does):

- it creates its own clips and its own users, and deletes them;
- it reads the shared clips (`C`, `E`, `X`; A, B and D are 7-clip's own since run 9) and never mutates
  them — no trim, no PATCH, no planted
  object, no assertion on a counter another bundle could move;
- it restarts, recreates and reconfigures nothing: no `dc up`, no second app process against this
  database, no change to an account-wide setting OF THE RUN'S OWNER. A bundle whose subject IS an
  account-wide setting (a password, the session list, the upload defaults, the default link grammar)
  does it to an account of its own — `bundle_account <suffix>` (scripts/e2e.sh) registers
  `${E2E_USER}_<suffix>` — a suffix its own `# e2e-namespace: account=` declares — with a jar in the bundle's context, and the harness's `upload` uses
  `UPLOAD_JARW` when a bundle sets it (33-account, 55-owner's step 57 and 82-links' steps 83-84 do);
- it asserts nothing bucket-WIDE, log-WIDE or instance-WIDE — `admin stats`' clip and object counts,
  `admin orphans`' summary line, a listing of the whole of `/scratch`, a grep for the absence of
  something over the whole app log. Scope every count to a clip's own prefix or to a clip's own id;
- it probes no rate limiter FROM THE RUN'S ADDRESS. Every limiter a bundle can reach is keyed by the
  client's address and every request of the run comes from one, so a drain from the host empties the
  bucket the other bundles' polls draw on. Two bundles drain one (55-owner.sh the `view` bucket,
  62-tiers.sh the `poll` one) and both do it with `burst_429 <url> <max> <what> apart`: the same
  requests, at the same LIVE app and its shipped ceiling, fired from a throwaway container of the
  stack's compose project at `http://app:8080` — an address nothing else in the run uses. That is
  what took them out of the exclusive phase (lane `excl`).
- it needs an INSTANCE of its own for anything above — a second `serve` with other settings, a census
  of clips, objects or `/scratch`, a global job count — and then it brings one: a `dc run` container of
  the same image with its own `DATA_DIR`, `SCRATCH_DIR`, `S3_KEY_PREFIX` (`e2e-<word>-<epoch>/`, removed
  by an EXIT trap with `store_rm_prefix_ok_`'s guard) and `STORAGE_DIR` (so a directory stack keeps it
  apart too), and `-e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT=`. 89-quota
  is the shape (everything over the container's own loopback); 37-library's step 38, 47-upload's
  cancel census and 55-owner's steps 59-60 are the newest examples.

**`apart` is a promise about what does not overlap.** Two bundles can each be safe on their own and
still fight over something neither of them is about: the `view` bucket is 60 requests a minute per client
ADDRESS and the `poll` bucket 120, both compile-time constants in `internal/httpapi` that no environment
can raise, and every bundle reaches the app from the same address. 70-web.sh's filmstrip poll spends one
a second and 43-watch.sh's preview-GIF poll one every two, so 43-watch declares
`# e2e-schedule-apart: 70-web` and the two never run together. Until lane `speed2` that promise was made
by FIXED LANES — both in lane 2 — and with it eleven unrelated bundles queued one behind another in lane
4, so the phase could never be wider than four whatever the machine. The four pairs the lanes were
actually keeping apart, each named in its file with the reason (still four after lane `excl`; the
`apart` argument of `burst_429` above is a different thing that shares the word — a drain fired from
another address, not a scheduling pair). The table of them is no longer written by hand: it is the
GENERATED view of the bundles' own headers (next section) — `bash scripts/lib/e2e-registry.sh --table`
prints it, and the self-test refuses two bundles that declare one bucket without an `apart` between them.
As printed on 2026-10-08 (run 9, lane `pool`, which added 22-gif's two pairs: its preview-GIF poll drains
`view` like the other two):

| apart pair | buckets both drain |
|---|---|
| 18-capture / 103-audio | none: apart for another reason, named in the file (the toil merge: 103-audio's burst outlasts step 29's 30 s hold) |
| 18-capture / 47-upload | none: apart for another reason, named in the file (the fix pass: its uploads keep the worker queue full, and an `expect` mismatch is never retried) |
| 18-capture / 62-tiers | none: as above (its x4/x8 tier encodes hold the worker) |
| 22-gif / 43-watch | view |
| 22-gif / 70-web | view |
| 43-watch / 70-web | view |
| 41-social / 67-capture | none: apart for another reason, named in the file |
| 85-bot / 94-instance | none: apart for another reason, named in the file |
| 91-drop / 95-import | none: apart for another reason, named in the file |

The reasons that are not a bucket stay in the files: 67-capture is the bundle most sensitive to LOAD (a
60 s bound on a small capture, an encode still running when a crawler asks) and 41-social is the
heaviest encoder beside it; 91-drop and 95-import both drive admin verbs in a container of their own and
never ran together, which was kept rather than argued away. **85-bot / 94-instance** was written down as
"both spend the `claim` bucket"; reading 85-bot for the registry showed that every claim request it makes
goes to `http://localhost:8080` INSIDE its own probe container — another process with its own limiter —
so only 94-instance drains the stack's `claim` bucket. The pair is kept (removing a scheduling promise is
not this lane's call); the table says what it rests on.

Put a new poller apart from the other poller of the same bucket, and remember that `wait_ready` already
paces itself: the plan sets its interval from the number of polling bundles that can run at once (at
most 1.5 progress requests a second against a refill of 2), and prints it.

**The pool.** The parallel bundles run `E2E_LANES` at a time (the default comes from the machine, below)
from ONE queue, LONGEST FIRST: `.e2e/checkpoints/durations__<BASE>.txt` (one file per origin, so a lane stack's seconds never order a
clippet.gg run's plan) keeps the last passing run's seconds per
bundle (`<stem> TAB <seconds> TAB <utc>`, rewritten after every run for the bundles that passed and kept
for the rest; a failed bundle is not recorded, because a run that died early would teach the planner it
is short), and a bundle with no row is planned as the longest known one, so a new drop-in starts early
rather than late. A slot that frees up takes the longest bundle that may start — nothing it is apart from
running, every `after` finished — so the phase ends close to its longest chain. The plan is printed
before the first bundle starts: the order with each bundle's last seconds, the `apart` pairs, and the
poll interval.

**How wide.** The default is `e2e_lanes_default_`: half the CPUs, at least 2 and at most 6 — 6 on the
16-CPU box. Measured on a lane stack pinned like the live one (`APP_CPUS=4`), 2026-10-05, the parallel
phase alone (`E2E_ONLY=` the eighteen parallel bundles, the same product tree, durations from the run
before):

| width | parallel phase | 73-ops | 70-web | 67-capture |
|---|---|---|---|---|
| 4 fixed lanes (before this lane) | 221 s | 149 s | 75 s | 35 s |
| pool, 4 | 162 s | 153 s | 83 s | 36 s |
| pool, 6 | 155 s | 154 s | 100 s | 51 s |
| pool, 8 | 157 s | 154 s | 105 s | 53 s |
| pool, 12 | 151 s | 150 s | 117 s | 68 s |

The phase cannot end before its longest bundle, and 73-ops (two probe containers, several real
encodes) is ~150 s of it on its own; longest-first gets the phase to within a few seconds of that floor
at 6, and every slot past it only buys noise. What wider DOES buy is load: every bundle that encodes
waits on the same app, so 70-web and 67-capture get slower as the width grows — 67-capture, whose step
67 bounds a small capture at 60 s, went from 36 s to 68 s for the whole bundle at 12. Nothing failed at
any width, and 6 is where the curve stops paying for that. The fixed lanes lost to the pool at the SAME
width because lane 4 queued eleven bundles behind one another (219 s of them) while lanes 1-3 sat idle.

**Overlap (`E2E_OVERLAP=1`) is gone** (run 9, lane `pool`). It started the pool beside the core's last
steps 27-30, measured once at 775 s against 774 s without it, and stayed a switch. With the core in the
pool there is nothing for it to overlap; `E2E_OVERLAP=1` is refused (exit 2, with the reason) rather than
quietly ignored, and the self-test pins the refusal.

**A helper two bundles share belongs in `scripts/e2e.sh`, and `scripts/check.sh` enforces it.** A bundle
that runs in the pool runs in a SUBSHELL, so the functions and variables it defines go away with it —
while everything was sourced into one shell in name order, a drop-in could quietly call a helper another
drop-in happened to define earlier, and three did (`wait_social` and `prefix_names`, written in
41-social.sh and called by 62-tiers.sh; `wait_trim`, written inside step 21 and called by two bundles;
`probe_queue_idle`/`probe_queue_count`, written in 37-library.sh and called by 55-owner.sh). All of them
became harness helpers (the last two were removed in lane `nits8`, uncalled since lane `excl`). The failure mode is loud rather than silent — `command not found` fails the
step — but only once that step runs, which is why check.sh has a stage that lists each drop-in's own
`name()` definitions and fails if another drop-in calls one. It strips comments and quoted strings
first: without that the word "member" in an assertion message reads as a call to 55-owner.sh's
`member()`.

The parallel bundles run first, in the pool; then the harness waits for the queue to go idle; then
the exclusive bundles run one at a time, in name order, each in a subshell of the harness's own shell.
A bundle in the pool gets a scratch context of its own (`.e2e/ctx-<stem>/`) and a log of its own
(`.e2e/bundle-<stem>.log`), which is printed into the run's transcript in bundle order when the phase
ends — so the transcript reads the same whichever bundle finished first.

Three conventions, all learned the hard way:

- **Namespace your variables — and declare the prefix** (`# e2e-namespace: … prefix=`, the next
  section; the self-test refuses a prefix two bundles declare, one inside another's, or a variable any
  other file assigns inside yours). A drop-in is *sourced* into the harness, so every name it sets is a
  global shared with scripts/e2e.sh and with every other drop-in. Prefix a bundle's own clip ids and
  scratch variables with a letter of its own (`D_S`, `D_I` in 41-social.sh, `L1`/`L2` in
  37-library.sh, `N1`–`N5`/`R` in 47-upload.sh, `O*` in 55-owner.sh). Bare one-letter names collide:
  41-social.sh's `I` and e2e.sh step 29's `I` were the same variable, and only the order in which
  they ran kept that harmless. The shared clips the harness itself owns — `C`, `E` and `X` — are never
  re-assigned by a drop-in (and the names A-J stay the harness's: 7-clip calls its own MX_A, MX_B, MX_D).
- **Name your clips in the summary.** The table under `E2E PASS` is the run's record. Its fixed rows
  are the harness's own clips, so a drop-in adds its own with `summary_row <label> <id> <text>` (one
  call per clip it created, after the step that destroyed it), or its clips appear nowhere.
- **A private `DATA_DIR` needs a private `SCRATCH_DIR`.** A throwaway `dc run … app` that boots
  `clippet serve` on a database of its own (steps 35 and 61) still mounts the *service's* volumes,
  so redirecting `DATA_DIR` alone leaves it booting an EMPTY database over the LIVE `/scratch`.
  serve.go's boot scratch janitor then looks every working directory up, gets `db.ErrNotFound`, and
  removes it — ahead of both the `HasLiveJob` and the `state != processing` guards, and `.upload-*`
  spools go unconditionally. Measured on a lane stack: a probe run beside an in-flight 1080p60
  transcode logged `janitor scanned=1 removed=1`, and the clip failed `permanent` ("Unable to
  re-open … output file") two minutes later, so it was never retried. Nothing but execution order
  keeps that from happening on a green run. Always pass a private `-e SCRATCH_DIR=…` next to
  `-e DATA_DIR=…`; serve.go MkdirAll's it before the janitor runs, so the scan finds an empty
  directory of its own. Make it a SIBLING of the data directory rather than a child of it —
  `-e DATA_DIR=/tmp/ops -e SCRATCH_DIR=/tmp/ops-scratch`, 51-admin.sh's ops probe, is the pattern to
  copy — because production wants the two separable, which is the reason `admin stats` reports
  `same_device` at all. Both shapes work (nothing in the code enumerates DATA_DIR's entries); the
  point of picking one is that the suite's own examples read the same way. A probe that does not run `serve` (51-admin.sh's `admin doctor` reds, 55-owner.sh's
  read-only `admin stats` pair) has no janitor and needs nothing; a `serve` that shares the live
  database overrides neither and is likewise safe from the janitor — the hazard is exactly the
  *mismatch* — but since lane `excl` no bundle runs one, because its boot reconcile and its worker
  reach the LIVE queue (nit sweep S27) and that is exclusive by construction.

## The namespace registry and the change map (run 9, lane `select`)

Every drop-in carries two more lines beside its schedule marker, in its first 60 lines — this is the
exact format, and a new bundle needs both:

```sh
# e2e-namespace: account=<suffix>[,<suffix>…] prefix=<VARPREFIX>[,…] buckets=<bucket>[,…]
# e2e-covers: <glob>[ <glob>…]
```

`-` is "none" for any of the three keys (all three are always written). The covers line may be repeated
when it gets long; the lines are read together.

| key | what it declares | what refuses a wrong one |
|---|---|---|
| `account=` | every `${E2E_USER}_<suffix>` account the bundle REGISTERS on the stack — through `bundle_account`, or by name (`${E2E_USER}_q`, `e2e_$(date +%s)_hk`). `_b` and `_c` are the core's: any bundle may log in as them, none declares them. Accounts a probe makes on its OWN instance (`botowner`, `opsuser`…) are not on the stack and are not declared. | `bundle_account <suffix>` dies on a suffix its calling bundle does not declare (it reads `E2E_BUNDLE`, set by the pool and the exclusive loop). The self-test refuses a suffix outside `[a-z0-9]{1,3}`, a suffix two bundles declare or the core owns, and a suffix a file mints without declaring it. |
| `prefix=` | the shell-variable prefixes the bundle OWNS (`AG_`, `DD_`…) | the self-test refuses a prefix two bundles declare, one that is the start of another's (`X` inside `XA_`), a declared prefix the file never assigns, and a variable any OTHER file — another bundle or scripts/e2e.sh — assigns inside it. (Lane `select` renamed four to get there: 104-audiocard's `AC_` → `AG_`, 33-account's `D_PASS` → `AC_DPASS`, 51-admin's `BK_NAME` → `AD_BKNAME`, the core's `T_EXP*` → `TTL_EXP*`.) |
| `buckets=` | the rate-limit buckets (`auth upload uploadip view poll bot claim`, internal/httpapi) the bundle DRAINS from the run's own address: a poll loop, or more than a handful of tokens a minute, beyond `wait_ready`'s paced poll. A `burst_429 … apart` comes from another address and is not declared (55-owner, 62-tiers). | the self-test refuses an unknown bucket and two bundles that drain one bucket without an `# e2e-schedule-apart:` between them (either side may declare it). |
| `e2e-covers:` | the PRODUCT files (internal/ cmd/ web/) the bundle proves — written by reading what it hits and which package or template answers; generous, never empty. `*` stays in one directory, `**` crosses them, `?` is one character. | the self-test refuses a bundle with no covers line and a glob that matches no product file (stale). |

Two lists sit beside the bundles, one row per line:

- **`scripts/e2e.d/CORE`** — product files every bundle stands on (since run 9 the old core steps 7-30
  are bundles with covers lines of their own) (`internal/db/db.go`, the migrations, `internal/httpapi/httpapi.go`, the middleware,
  the two `embed.go`). `E2E_ONLY` runs steps 1-6 only, so a change to one selects the whole suite.
- **`scripts/e2e.d/GO-ONLY`** — `<glob> <TestName>[ …]`: product files no bundle proves, each with the Go
  test that does (every name must exist). `@cross-compile` (windows-only files, which check.sh's Go stage
  compiles and no test runs) and `@not-built` (the `!unix && !windows` fallbacks nothing builds) stand in
  where no test touches the file, and say so.

**The coverage rule.** Every non-test product file under `internal/ cmd/ web/` (tracked, or untracked and
not ignored; `*_test.go`, `testdata/` and Markdown are not product) must match a bundle's glob, a CORE row
or a GO-ONLY row. A new product file with no row FAILS the self-test, which runs in T0 and in
`scripts/check.sh` — that is the maintenance cost of the map, paid by whoever adds the file.

**Every drop-in must parse** (lane `autoe2e`): the check runs `bash -n` over each `*.sh` here, with the bash that
sources them, and refuses one that does not (`REFUSE syntax:`) — a file bash stops reading half-way was seen to
pass with the rest of its assertions never run.

The views: `bash scripts/lib/e2e-registry.sh --check` runs every rule above on the real tree (the
self-test runs the same function, and plants each refusal in a scratch registry to show it fires);
`bash scripts/lib/e2e-registry.sh --table` prints the registry and the apart pairs (the table in "How a
new drop-in is scheduled" above is its output).

**What reads the map: `scripts/select.sh`.** It prints the `E2E_ONLY` value a diff owes (`git diff
--name-only ${SELECT_BASE:-main}...HEAD` plus the uncommitted tree, or `--range A...B`, or `--files "…"`):
`T2` when a SHAPE path changed (`compose*.yaml`, `Dockerfile*`, `.dockerignore`, `internal/config/**`,
`internal/db/migrations/**`, `cmd/clippet/serve.go`, `internal/store/**`, `scripts/e2e.sh`,
`scripts/lib/*.sh`, `scripts/lib/jget.py`, `go.mod`, `go.sum`); `all` when a product path no row covers, a
CORE path or a path it does not know changed (fail-safe); `none` for docs, records, Go tests, GO-ONLY
files and harness tools outside the E2E; otherwise the union of the bundles whose globs match (and any
drop-in that itself changed), longest first by the last recorded run. `--why` prints every path's
reason on stderr, `--go` the Go packages of the diff, `--plan` all of it for a person. The self-test is
owed whatever it prints.

**A drop-in's own program** lives beside it in `scripts/e2e.d/<stem>/` (110-autohost's mock of Discord, the plane and
a box's edge, in Go). The Docker build context excludes `scripts/`, so the product image never contains it; the bundle
builds it in the Dockerfile's `testdeps` stage with the checkout mounted read-only, as T0 does, and T0's `go vet` and
`go test ./...` compile it. `select.sh` maps a change under `scripts/e2e.d/<stem>/` to `<stem>` and to every other
drop-in that names `scripts/e2e.d/<stem>` to build it (111-autobot builds 110-autohost's mock).

**Adding a bundle — the checklist.** (1) `# e2e-schedule:` and its reason, plus any `-apart:`/`-after:`;
(2) `# e2e-namespace:` with every account it registers, every prefix it owns, every bucket it drains;
(3) `# e2e-covers:` with every product file it proves; (4) a row in the file table at the top of this
README and DESIGN §16.3's list; (5) `E2E_SELFTEST=1 bash scripts/e2e.sh` green — it checks (2) and (3)
against every other bundle.

## The exclusive phase, read bundle by bundle (lane `excl`, 2026-10-06)

Lane `speed2` left nine bundles exclusive — run one at a time after the pool, ≈510 s of a 1006 s run on
the lane stack. Each was read for WHY, and classified: (i) instance-wide state it mutates, (ii) a shared
account or clip it reuses, (iii) a global assertion another bundle's traffic would move, (iv) no reason.
What each had, and what it does now — every assertion that existed still exists; where WHAT is asserted
moved, the row says so:

| bundle | class | why it was exclusive | what it does now | asserted differently |
|---|---|---|---|---|
| 33-account | ii, iii | changed the OWNER's password, revoked the owner's other sessions and counted them; listed the owner's tokens | does all of it to `${E2E_USER}_ac`, with a clip of its own (AC_C) for the token scope matrix | the subject is the bundle's account and AC_C instead of the owner and C; every count is that account's |
| 37-library | i | step 38 started a second `serve` on the LIVE database (only with the queue idle: its boot reconcile and worker reach the live queue) | step 38 runs on an instance of its own: `LIBRARY_SCOPE=own` on one port and, in the same container, a second process on the same private database with the scope `shared` | "the shared app shows both clips" is the private twin's answer to user 1, not the live app's to the owner |
| 47-upload | iii | the cancel probe took an instance-wide census (`admin stats` clips + objects, the whole of `/scratch`) | the cancel is cut off on an instance of its own (89-quota's shape; the 1080p60 fixture on stdin), and the three readings are that instance's | the census is exact (0 → 0) instead of a delta of the live stack; the fixed `sleep 3` became "the spool is gone, then read" |
| 51-admin | ii, iii | step 52's `unstick --older-than 1s` over the live queue; step 53's failing instance; step 54 planted under the SHARED clip C | step 52 unchanged (the floor raises the threshold to hours, which no bundle reaches — a property of the floor, asserted in the same step); step 53 unchanged (its own instance); step 54 plants under a clip of its own (AD_C) | step 54's subject is AD_C instead of C |
| 55-owner | i, ii, iii | drained the `view` bucket from the run's address; changed the owner's upload defaults; steps 59-60 ran a second `serve` on the live database and asserted a GLOBAL notify count | the drain is `apart`; step 57 is `${E2E_USER}_ow`'s; steps 59-60 run on an instance of their own (a 3 s sweep, so the unlisted seed clip puts it over a one-byte budget) | notify=2 / notify=0 are asserted on EVERY stack (they were skipped whenever the run had a real webhook); the logs read are the probe's own; the account in steps 57 and 59-60 is the bundle's |
| 61-nits2 | iv | a log-wide absence over the app's log | unchanged: the line is a BOOT line of the stack's own process, which no parallel bundle restarts | — |
| 62-tiers | i | drained the `poll` bucket — the one every `wait_ready` polls — from the run's address | the drain is `apart` | — |
| 82-links | ii | step 83 changed the OWNER's default link grammar (every capture beside it would have drawn a memorable id) | steps 83-84 are `${E2E_USER}_lk`'s; step 82's instance-wide reads (robots.txt, the literal routes) are of things no bundle changes | the account in steps 83-84 is the bundle's |
| 87-storage | i, iii | the served-bytes counter is ONE number per instance; the flush half waits for the app's own 30 s window, which another bundle's bytes would satisfy | **still exclusive.** A probe instance would count ITS bytes, on loopback — and this step is about the stack's own counter behind its own origin (on clippet.gg, behind the edge). It is the honest remainder, ≈20-30 s. | — |

The self-test pins the outcome: every drop-in declares its phase (none relies on the default), and the
exclusive set is exactly `87-storage` — a bundle that changes phase changes that pin and this table in
the same commit.

**Measured** on the lane stack (:8548, `APP_CPUS=4`, 16 CPUs, the same product tree; durations warm):

| run | total | core (steps 1-30, 16-17) | parallel | exclusive |
|---|---|---|---|---|
| BEFORE (main's harness) | 1006 s | 321 s | 175 s (18 bundles) | 510 s (9 bundles) |
| AFTER, run 1 (same box state as BEFORE) | 581 s | 268 s | 281 s (26 bundles) | 31 s (87-storage) |
| AFTER, final tree (after a PC reboot: the core alone ran 187 s, so not comparable with BEFORE) | 380 s | 187 s | 164 s | 29 s |
| width 8 (`E2E_LANES=8`, once) | 557 s | 276 s | 261 s | 20 s |

Per bundle (seconds; BEFORE = the last BEFORE run, AFTER = run 1 / the final run):

| bundle | BEFORE | AFTER (run 1 / final) |
|---|---|---|
| 31-nits | 23 | 20 / 7 |
| 33-account (was exclusive) | 36 | 56 / 23 |
| 37-library (was exclusive) | 57 | 93 / 38 |
| 41-social | 47 | 41 / 22 |
| 43-watch | 39 | 40 / 15 |
| 47-upload (was exclusive) | 86 | 129 / 108 |
| 51-admin (was exclusive) | 99 | 129 / 89 |
| 55-owner (was exclusive) | 75 | 119 / 77 |
| 61-nits2 (was exclusive) | 7 | 9 / 8 |
| 62-tiers (was exclusive) | 89 | 172 / 91 |
| 67-capture | 101 | 58 / 24 |
| 70-web | 132 | 109 / 86 |
| 73-ops | 171 | 174 / 141 |
| 77-design | 53 | 47 / 18 |
| 80-front | 15 | 14 / 4 |
| 82-links (was exclusive) | 37 | 60 / 23 |
| 85-bot | 56 | 40 / 25 |
| 87-storage (exclusive) | 24 | 31 / 29 |
| 88-fsstore | 46 | 33 / 11 |
| 89-quota | 10 | 10 / 5 |
| 90-media | 37 | 35 / 14 |
| 91-drop | 75 | 59 / 44 |
| 92-setup | 58 | 48 / 20 |
| 93-signin | 27 | 24 / 11 |
| 94-instance | 10 | 8 / 3 |
| 95-import | 45 | 37 / 27 |
| 96-mint | 15 | 21 / 16 |
| 97-hook (newer than this table) | — | 37 (gate26b) |
| 99-backup (newer than this table) | — | 40 (gate26b) |

The two `gate26b` rows are not lane-stack numbers: they are the full public E2E of the `.e2e/gate26b/` gate run
(started 2026-10-07T01:36:58Z on https://clippet.gg — 16 CPUs, `APP_CPUS=4`, the GPU image, six slots, both bundles
in the pool beside five others), read from that run's `pool: … done in` lines.

The parallel phase is now WORK-bound rather than path-bound: ≈1585 slot-seconds over 6 slots,
against a longest bundle of ≈174 s. The bundles the pool took in run slower than they did alone
(62-tiers ≈90 → ≈170 s, 55-owner ≈75 → ≈120 s), because they now encode beside everything else — that
is the box being used, and the run is still 425 s shorter. Width 8 was measured once and NOT
shipped: 261 s against 281 s, inside the noise of one run, with every big bundle slower
again (62-tiers 223 s), and it exposed a load race in 67-capture's crawler hold (fixed: judged
after the event — the clip's `card_ready`, read after each answer, decides whether a 200 is a failure).
`E2E_LANES` keeps its default (half the CPUs, 2..6).

## The core in the pool (run 9, lane `pool`)

Until run 9 scripts/e2e.sh ran steps 1-30 one after another before the pool could start: 207-295 s on a
normal day, 966 s on gate 28b, and every `E2E_RESUME=1` paid it again to re-prove one bundle. The pool
waited because four of those steps read the WHOLE instance (step 20's notify count, the `admin stats` of
steps 22/23, step 26's clip count) and a bundle uploading beside them would have moved a correct number.
Now steps 1-6 are the serial SETUP — the stack, the fixtures, the owner, the second user `_b` (moved here
from step 19), clips C, E and X (X moved here from step 23, untagged; A, B and D moved OUT, into 7-clip,
the only bundle that ever read them) — and the old steps 7-30 are six pool
bundles that keep their step numbers, so `step 21` in a transcript is still the trim:

| bundle | old steps | what changed when it moved |
|---|---|---|
| 7-clip | 7-14 | A, B and D are its OWN uploads (step 4's asserts on them came along); it expires A, PATCHes and deletes B, deletes D — nothing else ever read the three. A retry makes them afresh |
| 15-ttl | 15, 19 | step 19 LOGS IN the second user (setup registers it) instead of registering it |
| 18-capture | 18, 29 | nothing |
| 20-pages | 20, 23, 27, 30 | step 20 SCOPED (below); step 23 registers its third user with `bundle_account c` (`c` is this bundle's, no longer the core's) and its tagged capture clip is its own (PG_X, not the shared X); step 27 flips PG_X unlisted and back, not C |
| 21-trim | 21, 24 | step 21 trims a clip of its OWN (TR_C, the same vertical fixture) instead of C, which six bundles read |
| 22-gif | 22 | its subject is its OWN noaudio clip (GF_B) instead of B; the "destroyed clip → 404" check reads GF_B after its own delete instead of D; it drains the `view` bucket (preview.gif polls) and so is apart from 43-watch and 70-web |

**The four instance-wide reads.** Step 20 now counts notify jobs for the clips the run's owner holds
(`admin ls --owner`, then `admin jobs --kind notify`, filtered by the harness's `e2e_notify_scoped_`, which
the self-test pins on a planted listing: two foreign rows ignored, a clip id quoted inside another row's
error not counted); its only whole-log read is for the webhook's BOOT line, a fact about the stack's own
process. Steps 22 and 23 (and 21) read only `admin stats`' schema line (`applied=1,2,3…`), which no clip
moves. Step 26 was cut (below), and its clip count with it.

**The duplicates, read** (proposal §5; each decided by reading both sides; the proof that stayed is named):

| step | decision | the proof that stays |
|---|---|---|
| 5 (a crawler is held while a clip has no card) | CUT | 67-capture step 68 (`the hold's Retry-After`, `the hold page must carry no Open Graph tag`, `a human GET of a clip with no card yet`, five crawlers held) and 55-owner's `card_check linkout … encoding` on a not-ready clip; Go: internal/web `TestCrawlerHoldOverHTTP`, `TestLinkOutInventoryIsSixteenTags` |
| 25 (a chunked upload works) | MOVED (the wire half) | Go: internal/httpapi `TestChunkedUploadIsGatedLikeAnyOther` drives the refusal and an in-process 201 (a recorder, no wire framing, a canned probe); the WIRE half — a real `Transfer-Encoding: chunked` POST that spools whole and becomes a ready clip of the fixture's full length — is 18-capture step 18 (4), ≈6 s on the step's own token |
| 26 (a playlist is not a video) | CUT | Go: internal/httpapi `TestUploadPlaylistIsNotAVideo` (415 not_a_video, storage untouched, no clip row) and, with the real ffprobe, internal/transcode `TestProbeRefusesACraftedPlaylist` (`-tags ffmpeg`) |
| 28 (CSRF, unlisted visibility, logout) | CUT | Go: internal/httpapi `TestPatchClip` and `TestDeleteClip` (a cross-site Origin and `Sec-Fetch-Site: cross-site` → 403 csrf on the real router), `TestListClipsVisibilityAndPagination` (another user's unlisted clip absent, the owner's present), `TestRegisterLoginLogoutMe` (`me after logout` → 401 with the retired cookie) |
| 29 (the capture reply waits for the card) | KEPT | the one deterministic proof that a small capture answers `ready` + `card_ready: true` and unfurls as a player on the first fetch; 67-capture step 67 tolerates `card_ready=false` under load |
| 21 (trim) beside 41-social and 62-tiers step 65 | KEPT | those two prove the card copy and the tiers follow a trim; only 21 proves the trim itself (the race, both paths, the seconds, the re-extension) |

**History.** run 9 (lane `pool`): steps 7-30 → the six bundles above; steps 5, 26 and 28 cut, 25 to its wire half (the fix pass moved that into 18-capture); the
second user and X into setup; `E2E_OVERLAP` removed; 103-audio's fourteen uploads submitted at once; the
suite 104 → 100 steps, 35 → 41 bundles.

**Measured** on the lane stack (:8580, `APP_CPUS=4`, 16 CPUs, `E2E_LANES` 6, the lane's own image built from the Dockerfile — not the GPU flavour, `MEDIA_DELIVERY=proxy`):

| run (2026-10-08, the logs are the lane's: run9/pool/final-*.log) | total | serial setup | pool | exclusive |
|---|---|---|---|---|
| the FULL suite once (100 steps / 41 bundles; one bundle failed on a planted harness-only line at its END, so every bundle did all its work; 20-pages retried once on a loopback `curl: (28) Failed to connect` and passed) | 384 s | 45 s | 324 s | 15 s |
| `E2E_RESUME=1` after removing the plant: the setup and the one unrecorded bundle (18-capture) | 39 s | 29 s | 9 s | 0 s |

The pool's floor on that run was its work over its width — 1711 bundle-seconds / 6 slots ≈ 285 s — and it
took 324 s because it was the first full run on that stack: with no recorded durations every bundle was
planned as the longest, so 73-ops (145 s) started late. The six old-core bundles were 143 of the 1711
bundle-seconds (7-clip 37, 21-trim 30, 20-pages 29, 15-ttl 20, 22-gif 17, 18-capture 10). For comparison
only (public runs on clippet.gg, not this stack): gate 27b printed `serial main 207 | parallel 15` (a
resume: the core alone) and gate 27a `serial main 234 | parallel 288`; gate 28c's resume of ONE bundle was
621 s of E2E. 103-audio on its own (`E2E_ONLY=103-audio`, same stack): 98 s before → 95 s with the uploads
at once → 56 s with the checks at once too (final-103-before.log, final-103-after-uploads-only.log,
final-103-after.log).

## The delivery mode, and the media helpers

The suite runs against **both** shapes of `MEDIA_DELIVERY` (DEPLOY.md §33, DESIGN.md §20 errata 278
and 280). In `proxy` the app streams every media byte; in `redirect` — which is what clippet.gg, the
stack every gate runs on, has done since 2026-09-14 — `video.mp4`, `social.mp4`/`light.mp4` and
`poster.jpg` answer **302** to a freshly presigned link on the bucket's own origin, after every gate
they have always run. `/original`, `preview.gif` and `filmstrip.jpg` are app-served in both.

`scripts/e2e.sh` **detects** which, from the stack, immediately after step 6: one anonymous GET of the
run's first ready clip's rendition, 200 → `proxy`, 302 → `redirect`, anything else fatal. It is never
read out of `.env` — that is what the operator meant, not what the process booted with. Two globals
come out of it, and a drop-in may read either:

| variable | value |
|---|---|
| `E2E_MEDIA` | `proxy` or `redirect`. Set before any bundle runs, so a pool subshell inherits it. |
| `E2E_MEDIA_ORIGIN` | in `redirect`, the `scheme://host[:port]` every presigned `Location` must land on. Empty in `proxy`. |

**Never fetch a media route with `get`, `head_` or a bare `curl` again.** Use the helpers, which are
their plain counterparts argument-for-argument in `proxy` and assert the 302 and follow it in
`redirect`:

| helper | in place of | what it leaves behind |
|---|---|---|
| `media_get <url> [curl args…]` | `get` | status on stdout, body in `$RESP`, headers in `$HDR` — of the hop that carried the bytes. A `Range:` or `If-None-Match:` is passed through to the bucket, which honours both on a presigned GET. |
| `media_download <url> <host file> [curl args…]` | `curl -sS -o "$(W …)" -w '%{http_code}'` | the bytes at that host path (already `W()`'d), status on stdout. For anything ffprobe, `cmp` or `wc -c` will read. |
| `media_head <url> [curl args…]` | `head_` | `$HDR` from the object, status on stdout. In `redirect` the object is asked with `Range: bytes=0-0` — a bare HEAD on a SigV4 link is 403, the method is signed — so the honest status is **206**, and `Content-Length` is rewritten from the `Content-Range` total so size comparisons keep working. Pair it with `expect_media`. The ETag it leaves is the **bucket's**, not this app's (the app's is a sha256 it computed at ingest): compare ETags within one source only. |
| `media_maybe <url> [curl args…]` | `get`, for a URL that MAY be a media route | as `media_get`, but a non-302 passes straight through. Step 10's card-URL sweep is the case: a page, an embed and three renditions in one list. |
| `expect_media ACTUAL WANT WHAT` | `expect`, after `media_head` | accepts 206 where 200 is wanted, in `redirect` only. Nothing else. |
| `media_skip <what>` | — | prints `(redirect: <what> is the bucket's; the app's <what> is proven by step 90)`. |

**Adding an assertion about a media route.** Decide which of it you are asserting, and the answer is
one of three:

1. **The bytes, or the object's own facts** (its size, its content type, its pixels, its ETag against
   ANOTHER media route's) — use the helpers and write the assertion exactly as you would have; it
   holds in both modes. The one thing that does not travel is an ETag compared across sources: the
   app serves its own sha256 and a bucket serves the bucket's, so `/original`'s validator and
   `/video.mp4`'s are one string in `proxy` and two in `redirect` (62-tiers.sh step 62 is the only
   place in the suite that ever compared them, and it says so).
2. **This app's own answer on the route** (the shape of a 416, an `If-Match` 412, `Accept-Ranges`,
   `Vary`, which headers a 304 carries) — a 302 replaces that response, so guard it with
   `if [ "$E2E_MEDIA" = proxy ]; then … else media_skip "…"; fi`, or, better, find the redirect's own
   version of the same truth and assert THAT (a crawler's ranged GET is 302-then-206 from the bucket,
   and that is worth more than a skip). **Never leave it silent**, and never let it pass by accident.
3. **`/original`, `preview.gif` or `filmstrip.jpg`** — plain `get`/`head_`, unchanged. If your
   assertion would be broken by a 302, add `expect_empty "$(hdr Location)"` beside it and you have
   pinned erratum 278(a) as well.

A helper **refuses a cookie jar** (`-b`): in `redirect` the second hop goes to the bucket, which is a
third party. No media route needs a credential — `/original` is the one that does and is never
redirected — so that `die` can only ever fire on a harness mistake. No message ever prints a signed
Location's query, which carries the signature and the access key id.

**A probe that invents a base invents a mode to go with it.** A drop-in that brings its own
`clippet serve` inherits `MEDIA_DELIVERY` and `MEDIA_PRESIGN_ENDPOINT` from compose like every other
variable, and usually that is harmless: a presign makes no network call, so a probe signs happily for
an origin nothing in its container could reach. Two shapes are not harmless, and both are boot errors
by design (DESIGN.md §20 erratum 278(e)):

- **`STORAGE_BACKEND=fs`** — `redirect` on a directory is refused at boot, so `88-fsstore.sh` unsets
  the three `MEDIA_*` variables beside the four `S3_*` ones it already unsets.
- **An invented https `PUBLIC_BASE_URL`** — `61-nits2.sh` and `85-bot.sh` each name an origin of
  their own so their assertions read the same on a lane stack and on the reference stack, and a
  plain-http presign origin under an https base is the mixed-content refusal. Both pin
  `-e MEDIA_DELIVERY=proxy -e MEDIA_PRESIGN_TTL= -e MEDIA_PRESIGN_ENDPOINT=`. This one was found by
  the redirect-mode proof of erratum 280 and would not have shown on the reference stack, where the
  presign origin is R2's own https host.

Copy whichever applies; a probe with no viewers has nothing to redirect anybody to.

## Asking what is in the store

**Never list a bucket directly; ask the app.** `prefix_count <id>` and `prefix_names <id>` (harness
helpers in `scripts/e2e.sh`) run `clippet admin objects <id> [--count]` inside the app container, so
they answer for whichever store this instance actually uses — the harness's S3 server, R2, B2 or S3 under any
`S3_KEY_PREFIX`, or a directory on `STORAGE_BACKEND=fs` — and the delivery mode above does not affect
them. They used to run `mc ls` against the compose MinIO by service name, which stopped being the
reference stack's store the day it moved to R2: every prefix assertion in the suite then saw 0 objects,
and the ones that WANT 0 — `wait_gone`, and every `expect_prefix_count <id> 0` — passed without looking
at anything (DESIGN.md §20 erratum 281).

| helper | what it gives you |
|---|---|
| `prefix_count <id>` | the number of objects under `clips/<id>/`, as a bare number. Dies on output that is not a count. |
| `expect_prefix_count <id> <want> <what>` | the same, POLLED to the wanted count for up to 12 s (an object delete follows the commit that stopped referencing it). |
| `prefix_names <id>` | the object KEYS, one per line. Dies on a listing whose own count line disagrees with the lines it parsed. |

The verb opens no database, so it answers about a clip whose row has already gone — which is what
`wait_gone` needs, and part of why these belong to the harness rather than to a bundle. A bundle that
wants a NARROWER count derives it from `prefix_names` (70-web.sh counts `clips/<id>/filmstrip/` that
way) instead of writing a second listing of its own.

### Putting something IN it, or taking it out

**Planting and removing go through `store_obj` — one helper, in `scripts/e2e.sh` — which is the
product's own `clippet admin objects` run through `dc exec` in the stack's app container (DESIGN.md §20
erratum 346).** Nothing in this suite runs `mc` or any other S3 client: the bucket on a test stack is
RustFS (`rustfs/rustfs`, pinned by digest, defined whole in `compose.e2e-bucket.yaml` as the service
`s3`), the E2E's step 1 creates its bucket with `admin objects init`, and every write goes through
the app with S3_* exactly as the app reads them — so it lands in whichever store THIS stack uses, and no
credential leaves the app container. Ten bundles call it today (37, 47, 51, 55, 73, 89, 90, 92, 105, 107):
51-admin.sh plants objects under a live clip for the orphans report and removes them again, and the rest
count or remove the bucket prefix their own probe container wrote. **Never bring a client of your own.**

| what you write | what it does |
|---|---|
| `printf … \| store_obj put "${SM_PREFIX}<key>"` | stores stdin under a key in the stack's OWN key space — byte-for-byte the key the app would write for the same thing, and the only kind `admin orphans` or `prefix_count` can see. |
| `store_obj rm "${SM_PREFIX}<key>"` | removes that one key. |
| `store_obj rm --prefix "<probe prefix>"` | removes a probe container's whole prefix at the bucket ROOT: `-e S3_KEY_PREFIX=…` REPLACES compose's value rather than nesting under it, so the probe's objects sit beside the stack's key space. The PRODUCT refuses anything but the `e2e-<word>-<epoch>/` shape and the stack's own key space; ask `store_rm_prefix_ok_` first anyway, so a refused trap prints a line. |
| `store_obj --prefix "<prefix>" --count` | the number of objects under a root-relative prefix (73-ops, 92-setup). |
| `probe_prefix_swept_ "<probe prefix>" "<what>"` | ONCE THE PROBE CONTAINER HAS EXITED: removes the prefix and dies unless `--count` then reads 0. A probe instance writes its database snapshot (§20 erratum 343) under `<prefix>_clippet/` as it goes down — after its own in-container tidy and its own `objects=0` — so only the host can leave the bucket root empty (51-admin, 73-ops, 89-quota, 90-media). Stands down on a directory stack. |
| `probe_prefix_tidy_ "<probe prefix>" "<what>"` | the same removal, best effort, for the EXIT trap armed before the probe runs (cleared with `trap - EXIT` after `probe_prefix_swept_`). |
| `$SM_PREFIX` | the stack's key prefix on its own, normalized the way the app normalizes it (one trailing slash, empty stays empty) — `admin objects`' write forms take keys from the bucket's ROOT, so you add it yourself when you mean the stack's own space. |
| `store_has_bucket` | ask it FIRST, in a condition. On `STORAGE_BACKEND=fs` there is no bucket and the caller stands down with a printed line; `store_obj` itself dies rather than write into a directory nobody meant. |

stdin and stdout pass straight through, and `store_obj` dies on any failure with the verb's own stderr. A
cleanup that runs from an EXIT trap and must never fail a run therefore calls it inside a subshell —
`( store_obj … ) || echo "[e2e]   NOTE: …"` — which is the pattern every probe bundle uses.

## What a run leaves behind: nothing

Since §20 erratum 272 the harness sweeps its OWN litter at both ends of a run. `e2e_sweep_own` runs at
the start of step 1, before the run has made anything, and again at the end of step 17, after that
step's deletes: it lists the stack's accounts, keeps only the ones matching
`^e2e_[0-9]{9,11}(_[a-z0-9]{1,3})?$` — `e2e_<epoch>` and the `_b`/`_c`/`_d`/`_s1`/`_s2` suffixes this
suite adds, and nothing else on any stack — force-expires every clip they own, waits for the sweeper to
destroy them, removes any COLLECTION those accounts still own (`admin collections ls --owner` →
`admin collections rm <id> --owner`, §20 erratum 275 — the row only, never a clip), and prunes the
emptied accounts (`users prune 'e2e_%' --older-than 1s --no-clips`, which skips any match that still owns
something). It PRINTS the target list before it acts, every time, and it
is FATAL at step 1 (a stack that cannot be swept is not a stack to test on) and best effort at step 17
(nothing there may fail a run that has already passed). A passing run therefore leaves no account and no
clip — it used to leave four users — and an ABORTED run's leftovers are cleared by the next run rather
than by a person typing a delete loop by hand, which is the habit that cost this project a stack on
2026-09-11.

What that means for a drop-in: **keep deleting your own clips and collections in the bundle that made
them.** The sweep is the floor, not the cleanup plan. It only ever reaches clips and collections owned by an
account whose name matches the pattern above, so anything a bundle created under any other owner is
invisible to it — and a bundle that leaves its own clips behind still leaves the run slower and its
own assertions weaker than they were meant to be. (Until §20 erratum 275 a leftover COLLECTION was
worse than that: nothing could remove one outside its owner's session, so the account it stranded was
NOTEd by every future run and cleared by none. `admin collections rm` closed that, and the sweep uses
it.) `scripts/visual.sh` does the same two things for `visual_%` at both ends, for the same reasons.

## Resuming a run, and a bundle that fails

Every bundle that passes is written to `.e2e/checkpoints/<product-key>__<origin>.txt`. The key is `git ls-tree HEAD`
over `internal cmd web go.mod go.sum Dockerfile compose.yaml` — what the binary is built from — so a harness edit keeps
the key and a product edit moves it; an uncommitted change under those paths makes it `-dirty`, which records nothing
and resumes nothing.

`E2E_RESUME=1 bash scripts/e2e.sh` runs the setup (steps 1-6, the fixtures) and only the bundles that key does NOT
record as passed at this origin. The banner is `E2E PASS (RESUMED)` and names the bundles credited from earlier runs.
That union IS the gate when the product tree is unchanged, and it is how a fix to one drop-in is proven: fix, then
`E2E_RESUME=1` — the setup plus that bundle, not the suite. A change under the product paths owes the full run.
The checkpoints are the HOST's: a run in WSL records in the WSL clone's `.e2e/checkpoints/`, a Git Bash run
(`CLIPPET_WSL=0`) in the Windows checkout's, and neither sees the other's — resume on the side that recorded.

A bundle that fails no longer ends the run. It is recorded (`.e2e/failed-bundles.txt`), its checkpoint line is removed,
every other bundle still runs, steps 16 and 17 still run, and the closing banner is `E2E FAIL … bundle(s) failed: …`
with exit 1. Exclusive bundles run in a subshell for this, exactly like pool bundles — so nothing a bundle sets in the
shell can be read by a later one (it never could across the pool; the helper scan and the prefix convention above are what
keep it so). The setup's own steps (1-6) stay fatal.

## Retry: once, on a classified transient, with a reason log (run 9, lane `retry`)

A POOL bundle that fails is classified by `scripts/lib/transient.sh` (`transient_sig <log> [context dir]`), which reads
`.e2e/bundle-<stem>.log` from its last `[e2e] FAIL step` line and answers a signature or nothing:

| signature | what the log shows |
|---|---|
| `curl-7` `curl-28` `curl-35` `curl-52` `curl-56` | curl's own `curl: (N)` line in the 15 lines before the FAIL line (not one `curl_` already retried and got past), when it is the failing request's: the ring under FAIL ends in `000`, or there is none, or the curl line is the one right before FAIL |
| `edge-502` `edge-520` … `edge-524` | the failing request (the last line of the request ring under FAIL) answered that status AND an edge said so: a `cf-ray` / `server: cloudflare` header or Cloudflare's error body, in the log or in the bundle's last response files (`ctx-<stem>/resp.hdr`, `page.html`, `resp.json`). A 502 with no edge evidence came from the stack under test and is a failure. |
| `pw-closed` `pw-reset` `pw-timeout` | Playwright's `net::ERR_CONNECTION_CLOSED` / `net::ERR_CONNECTION_RESET` / `net::ERR_TIMED_OUT` or `ERR_CONNECTION_TIMED_OUT` |
| `docker-125` | the daemon refused: `docker: Error response from daemon` (docker run/create's own line, not one `dc` retried and got past), or an `exit`/`rc` of 125 on the FAIL line |

NEVER classified, so never retried: an `expect` on a status somebody answered (`got '409'`, `got '301'` — gate 27a's
suffix collision and gate 28b's redirect rule are both in the self-test as things that must stay failures), a Go or a
visual assertion, a missing fixture, anything else.

A classified failure runs the bundle AGAIN, once, in the same run and the same slot, after `E2E_RETRY_PAUSE`:

- **on a fresh account namespace.** The retry's subshell has `E2E_ATTEMPT=2`, and `bundle_account <suffix>` then
  registers `e2e_<epoch>2_<suffix>` instead of `e2e_<epoch>_<suffix>` — the run's stamp with a `2`, eleven digits,
  still inside the sweep's `^e2e_[0-9]{9,11}(_[a-z0-9]{1,3})?$`, and never a name any first attempt can have. The stamp
  and not the suffix: 102-dedupe registers both `dd` and `dd2`, so `dd` → `dd2` would collide with its own first
  attempt. A bundle that names an account on the shared stack WITHOUT `bundle_account` (33-account's admin-made `_d`)
  takes it from `bundle_user_ <suffix>`, which is the same rule. **A new bundle's contract:** everything it registers
  on the shared stack goes through one of the two, and anything else it makes there must tolerate a second go.
- **with the first attempt kept**: `.e2e/bundle-<stem>.1.log` (the merged transcript prints it first, under its own
  header), and `ctx-<stem>/{summary-rows,step-times,requests}.1.txt`.
- **written down**: `.e2e/retries.txt`, one `stem TAB signature TAB first-attempt seconds TAB utc` per retry, emptied
  at the start of each run; the per-bundle time table tags the row `retry (first attempt Ns, <signature>)`.
- **on the closing banner, PASS or FAIL**: `retried: 47-upload (edge-502, 1 retry)`, `… 1 retry, failed again`, or
  `retried: none`. A pass that needed a retry is a pass; the line is what keeps a real bug that looks like a
  transient from hiding behind one.

A second failure is a failure, recorded as any other. `E2E_RETRY=0` turns the policy off, for a run that wants to see
the raw transient. The serial setup (steps 1-6) is not a bundle and is not re-run; `curl_`, `dc` and `http_refused`
are its own bounded retries, and since run 10 (lane `nits10`) its five writes — the two registrations, uploads C, E
and X — go through `setup_once_`: sent again ONCE when curl says the request never left (exit 7, or exit 28 with
`Failed to connect` / `Connection timed out`), on the stack's own loopback only, with a `setup-step<N>` row in
`retries.txt` and the banner's `retried:` line; a 28 after the connection opened may have landed and is never re-sent.
What is NOT retried: the exclusive phase (87-storage, run inline), every bundle under `E2E_LANES=1` (which runs
them all exclusive). The self-test pins the classifier's table, the namespace
(every drop-in suffix's retry name inside the sweep pattern and apart from every first-attempt name), and the retry on
planted bundles: exactly one, the first log kept, the row written, an app 409 never retried, `E2E_RETRY=0` off.

The visual pass does the same for a NAVIGATION (`scripts/lib/navretry.js`, required by `visual.js`): a `page.goto`
that fails with `pw-closed`, `pw-reset` or `pw-timeout` waits 2 s and goes once more, logging
`[visual] retry <url> (<signature>)`; a second failure fails the shot as before. The summary line counts them
(`[visual] N shots, M failing, K navigation retries`). `VISUAL_SELFTEST=1` runs visual.js's own checks — the media
rules and these — and exits before a browser starts.

## What this suite deliberately does not cover

One ending is Go-test territory rather than a missing step, and it is written down here so the gap reads
as a choice: **a tier job that is SKIPPED still settles its tier state.** Reaching it live means changing
`SOCIAL_MAX_BYTES` underneath a job that is already queued — a probe container of its own, with its own
`DATA_DIR` and `SCRATCH_DIR`, restarted mid-queue — for one boolean, and the restart would be the most
fragile thing in the suite. `internal/worker.TestSkippedTierJobSettlesItsTierState` and
`internal/db.TestSettleTierEncodingOnlyOverwritesAPromise` pin it instead, with no clock and no
container. If a later change makes the transition reachable without a restart, it belongs in 73-ops.sh
beside the other probe-container steps.

## The knobs the harness itself reads

`bash scripts/e2e.sh` with none of them set is the whole suite, and that is the gate. All of them are
off (or at their default) unless you say otherwise.

| variable | default | what it does |
|---|---|---|
| `BASE` | the app's `PUBLIC_BASE_URL` | run against a public origin. It must EQUAL the app's own `PUBLIC_BASE_URL`: every card tag is absolute and is compared against it (§20 erratum 56). Every request through the wrappers (`get`/`page`/`head_`/`http`/`http_refused`/`curl_`) carries `X-Clippet-App: 1`, so clippet.gg's homepage Worker on the exact root `/` hands it to the app (DEPLOY.md §37) — a bare `curl` of `/` must add the header itself. The proof it is sent: the self-test pins every wrapper's argv (and the retry's) and `visual.js`'s own check pins `extraHTTPHeaders` on every context; on the wire the header shows in the gate's visual log, where a timed-out POST printed it (lane `front2`) — no separate Playwright wire test was written, by decision (lane `nits7`). |
| `E2E_ONLY` | *(empty)* | run only these bundles, by file stem: `E2E_ONLY=70-web,77-design`. Steps 7-15 and 18-30 do not run; the minimal setup they all depend on does (see below). The closing banner says `E2E PASS (PARTIAL)` and lists what did not run. |
| `E2E_SKIP` | *(empty)* | the complement: the whole suite minus these bundles. |
| `E2E_RESUME` | *(empty)* | `1` runs the setup (steps 1-6) and only the bundles not recorded as passed on this product tree at this origin (the section above). The banner says `E2E PASS (RESUMED)` and lists what was credited. |
| `E2E_LANES` | half the CPUs, 2..6 (6 here) | how many parallel-safe bundles run at once — the pool's width (see "How wide" below; step 1 prints the CPU count and the width). `E2E_LANES=1` runs every bundle one at a time in name order, which is exactly what the suite did before §20 erratum 245. |
| `E2E_OVERLAP` | — | **gone** (run 9, lane `pool`): any value but `0` is refused with exit 2 — the core is in the pool. |
| `E2E_CLEAN` | `0` | empty `.e2e/` of the RUN's files before the run — it keeps `backups/ checkpoints/ logo/ bot/ demo-user.txt webhook.txt` and prints what it removes first (§20 erratum 290). Leave it off while you are diagnosing: the bodies under `.e2e/` are the post-mortem of the run that failed. |
| `E2E_SWEEP_INTERVAL` | `15s` | the sweeper's tick for this run (the shipped default is 60s and steps 14, 15 and 19 each wait for a tick). `E2E_SWEEP_INTERVAL=60s` restores it. |
| `E2E_APP_CPUS` | *(unset)* | the app container's `cpus` quota for this run, so a lane stack can be timed at the cores its box has without hand-editing its `.env`. Unset, `.env` decides (compose's own default is `1.0`). **It must not exceed the host's `nproc`** or the container is refused at creation (DEPLOY §7) — step 1 prints the value compose resolved either way. |
| `E2E_RATE_AUTH_PER_MIN` | `120` | the auth limiter for this run (shipped: 10/min/IP). Nothing in the suite asserts that ceiling; four bundles logging in inside one minute would otherwise be refused. |
| `E2E_RATE_UPLOAD_PER_HOUR`, `E2E_RATE_UPLOAD_PER_HOUR_PER_IP` | `600`, `1200` | the upload limiters for this run, raised since Phase 2 for the reason step 1 records. |
| `E2E_DISCORD_WEBHOOK_URL` | *(empty)* | runs step 20's POSITIVE half against a real webhook (a run by hand with real credentials). |
| `E2E_CLEAN_BUCKET` | `0` | step 17 ends with a bucket-wide `admin orphans --min-age 0` census, printed in every run. With this set to `1` it also ASSERTS `extra=0`, which is a real invariant on a stack the run started clean and none of the suite's business on a shared one. |
| *(none — `E2E_MEDIA` is DETECTED)* | — | there is no knob for the delivery mode: the suite asks the stack after step 6 and prints the answer. See "The delivery mode" above. |
| `E2E_SELFTEST` | `0` | not a run at all: `E2E_SELFTEST=1 bash scripts/e2e.sh` exercises the harness's three retry wrappers (`dc` on docker's exit 125, `curl_` on curl's 7/28/35, `http_refused` on an edge 502/520-524 or a 7/28/35 and never on loopback) against a stub, and the sweep's patterns (§20 errata 272 and 275: which account names it may ever act on, which id shapes it accepts, how it reads the collections listing) against planted text, then exits — without a daemon, a network or a stack. It also pins the four media helpers that no live run could pin — `media_origin_`, `media_redact_`, `media_no_cookie_` and `media_total_` (the `Content-Range` → `Content-Length` mapping), §20 erratum 283 (h). Since run 9 (lane `select`) it also runs the namespace registry and the change map on the real tree and plants each of their refusals in a scratch registry, refuses `bundle_account` an undeclared suffix, and pins `scripts/select.sh`'s rules (none / T2 / all / a bundle). It ends in **`E2E SELFTEST PASS`** and never in `E2E PASS`, so a caller that greps for the suite's pass line cannot read a self-test as a run. `scripts/check.sh` runs it, which is the only reason the retry branches are ever executed. |
| `E2E_BURST_CHUNK` | `25` | how many requests one `curl --next` process makes when a step drains a rate limiter (`burst_429`, §20 erratum 283 (f)). The two drains fire the same requests against the same live ceiling as before; this is only how many of them share a process. |
| `E2E_RETRY` | `1` | `0` turns off the pool's retry of a bundle that failed on a classified transient, and the setup's `setup_once_` with it ("Retry" above); anything but `0`/`1` is refused. The banner says `(E2E_RETRY=0: the policy was off)`. |
| `E2E_RETRY_PAUSE` | `3` | how long the three retry wrappers wait before their ONE retry. There is no reason to change it in a run; it exists because `E2E_SELFTEST=1` exercises seven retry branches against a stub with no network, where three seconds each was ~20 s of `scripts/check.sh` spent asleep. The self-test asserts the default is 3 and then sets it to 0 (§20 erratum 283 (d)). |

A name in `E2E_ONLY` or `E2E_SKIP` that matches no drop-in is FATAL before step 1 does any work: a typo
that quietly ran nothing would still end in a PASS line.

**What `E2E_ONLY` sets up.** What every run sets up: steps 1-6 — the stack with this run's limiters, the
fixtures, the owner and the second user `${E2E_USER}_b` (seven bundles log it in to be a stranger), and
the clips C, E and X ready (X came through the capture route; 37-library.sh names it). Since run 9 a full
run makes nothing more than a filtered one does before the pool starts. If a new drop-in comes to depend
on something another bundle makes, it makes it itself (or the setup grows, for every bundle at once).
