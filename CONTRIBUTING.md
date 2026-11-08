# Contributing

Issues and Discussions are welcome: a bug with the steps to reproduce it, a question about running an
instance, a feature you miss. Use the issue forms; a question goes to Discussions.

**Pull requests are accepted by prior agreement, for small fixes.** Every change here goes through a
tiered test harness and a review flow, so a larger change that arrives unannounced is more work to take in
than to write. Open an issue first and say what you would change; if the answer is yes, the pull request
is welcome.

## The Developer Certificate of Origin

Every commit in a pull request carries a `Signed-off-by:` line (`git commit -s`), which states that you
wrote the change or have the right to submit it under this repository's licence — the
[Developer Certificate of Origin](https://developercertificate.org/). A CI step checks the line. There is
no contributor licence agreement.

## Running the tests

Toolchains live in containers: the host needs Docker (Engine 25+ or Docker Desktop) with Compose v2, bash,
GNU grep, curl, python3 and ffmpeg 7.1 — no Go and no Node. The tiers, from `DESIGN.md` §20 erratum 331:

```sh
bash scripts/t0.sh          # T0, every commit, no stack: gofmt, go vet on the changed packages, go test
                            #   from Go's own cache, and check.sh's cheap stages
bash scripts/t1.sh          # T1, once per change, on a stack of your own (a .env naming a compose project
                            #   of its own): T0, -race on the changed packages that own goroutines, then
                            #   only the end-to-end bundles and visual phases your diff touches
bash scripts/select.sh --why    # what a diff owes: T2 | all | none | <bundles>, and why
bash scripts/gate.sh        # T2, the full suite with -race; a release, a shape change, or when T1 asks for it
```

T1 stops with `this diff needs T2: <path>` when a shape path moved (compose, the Dockerfile, a migration,
`internal/config`, `internal/store`, the harness core); say so in the pull request and the maintainer runs
the gate. The heads of `scripts/t0.sh`, `scripts/t1.sh` and `scripts/select.sh` document every knob.

The tests build the image from your checkout through the `compose.build.yaml` overlay by themselves, so
they never run the published image.

## Writing an end-to-end bundle

The suite is a directory of drop-ins, `scripts/e2e.d/`, one file per bundle. Its
[`README.md`](scripts/e2e.d/README.md) is the contract: the schedule marker every file declares
(`# e2e-schedule: parallel|exclusive`), the `# e2e-covers:` lines that map product files to the bundle (what
`select.sh` reads), the namespace registry, and the rules a bundle must keep to run in the pool. A drop-in
with no marker runs alone and the harness says so.

## Conventions

- **Citations.** A comment cites `DESIGN.md §20 erratum N` or `ROADMAP #N`, never a person. The errata are
  the binding design record; their numbers are stable and are never renumbered.
- **Commit subjects** under 72 characters, plain; the body says why.
- **No URL literal** (`http://` or `https://`) in a comment or string under `internal/`, `cmd/` or `web/`:
  `scripts/check.sh` fails on one, because the same grep is the outbound-connection tripwire. Name the
  origin in words.
- **Formatting**: `gofmt`; `web/static/app.js` is ES2020 with no build step; `web/STYLE.md` is binding
  for any change to the templates, `app.css` or `app.js`.
- **Line endings** are LF, enforced by `.gitattributes`.
- A test spins down as cleanly as it spins up: whatever a test creates — containers, volumes, accounts,
  files — it removes.

The code of conduct is [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md); security reports go through
[`SECURITY.md`](SECURITY.md).
