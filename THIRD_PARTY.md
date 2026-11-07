# Third-party material

What this repository ships, builds with or copies into its image that is not Clippet's own, with each
item's licence. Clippet's own code is under [`LICENSE`](LICENSE); nothing below changes that.

## Shipped inside the repository

| what | where | licence |
|---|---|---|
| The EFF Large Wordlist (7,776 words, curated to `^[a-z]{3,9}$` — the four hyphenated entries dropped, nothing else), the source of the memorable link ids | `internal/ids/eff_large_wordlist.txt`, embedded in the binary by `internal/ids/words.go` | [CC BY 3.0](https://creativecommons.org/licenses/by/3.0/), the Electronic Frontier Foundation — <https://www.eff.org/dice> |
| Spline Sans (variable, 300–700, latin subset) and Spline Sans Mono, the two typefaces every page is drawn with, served from the binary and fetched from nowhere; plus `spline-sans-mono.ttf`, a static weight-500 instance of Spline Sans Mono the server draws into every poster's duration label (installed into the image) | `web/static/fonts/` | [SIL Open Font License 1.1](web/static/fonts/OFL.txt) (the full text is that file; the copyright lines name the Spline Sans and Spline Sans Mono project authors) |

## Installed into the image at build time (not in the repository)

| what | how it gets there | licence |
|---|---|---|
| ffmpeg 7.1.1, a static build | `Dockerfile` copies it out of the image `mwader/static-ffmpeg:7.1.1`; the GPU image (`Dockerfile`'s `gpu` stage) takes its ffmpeg from `jrottenberg/ffmpeg:7.1-nvidia` | GPL (the build enables GPL components). This repository redistributes no ffmpeg binary; an image published from it would carry one, and with it the GPL's notice and source obligations for that build |
| DejaVu Sans, the Light tag's font | `apk add font-dejavu` (Alpine) in the runtime and test stages; `fonts-dejavu-core` (Ubuntu) in the GPU stage | the [DejaVu Fonts licence](https://dejavu-fonts.github.io/License.html) (Bitstream Vera, with public-domain changes) |
| The base images: `golang:1.27-alpine`, `alpine:3.22`, `nvidia/cuda:12.6.2-runtime-ubuntu24.04` | `Dockerfile` | each image's own terms; the CUDA image under NVIDIA's licence for its runtime |

## Go modules

From `go.mod`, as reported by `go-licenses report ./...` (`github.com/google/go-licenses`, run in a `golang:1.27-alpine`
container over this tree on 2026-11-06). The six direct dependencies first; the rest are theirs.

| module | licence |
|---|---|
| `github.com/coder/websocket` v1.8.15 | ISC |
| `github.com/minio/minio-go/v7` v7.3.0 | Apache-2.0 |
| `golang.org/x/crypto` v0.55.0 | BSD-3-Clause |
| `golang.org/x/sys` v0.47.0 | BSD-3-Clause |
| `golang.org/x/time` v0.15.0 | BSD-3-Clause |
| `modernc.org/sqlite` v1.57.0 | BSD-3-Clause |
| `github.com/cespare/xxhash/v2` v2.3.0 | MIT |
| `github.com/dustin/go-humanize` v1.0.1 | MIT |
| `github.com/google/uuid` v1.6.0 | BSD-3-Clause |
| `github.com/klauspost/compress` v1.19.2 | Apache-2.0 (its `s2` package BSD-3-Clause) |
| `github.com/klauspost/cpuid/v2` v2.4.0 | MIT |
| `github.com/klauspost/crc32` v1.3.0 | BSD-3-Clause |
| `github.com/minio/crc64nvme` v1.1.1 | Apache-2.0 |
| `github.com/minio/md5-simd` v1.1.2 | Apache-2.0 |
| `github.com/philhofer/fwd` v1.2.0 | MIT |
| `github.com/remyoudompheng/bigfft` (20230129) | BSD-3-Clause |
| `github.com/rs/xid` v1.6.0 | MIT |
| `github.com/tinylib/msgp` v1.6.4 | MIT |
| `github.com/zeebo/xxh3` v1.1.0 | BSD-2-Clause |
| `go.yaml.in/yaml/v3` v3.0.5 | MIT |
| `golang.org/x/net` v0.58.0 | BSD-3-Clause |
| `golang.org/x/text` v0.41.0 | BSD-3-Clause |
| `gopkg.in/ini.v1` v1.67.3 | Apache-2.0 |
| `modernc.org/libc` v1.74.4 | MIT (its `LICENSE-3RD-PARTY.md` lists the C sources it wraps) |
| `modernc.org/mathutil` v1.7.1 | BSD-3-Clause (the tool reports it as unknown; read from the module's own `LICENSE`) |
| `modernc.org/memory` v1.11.0 | BSD-3-Clause |

`github.com/mattn/go-isatty` and `github.com/ncruces/go-strftime` are in `go.sum` but not linked into the binaries
the tool walks.

## Used by the test harness only (never in the product image)

`mcr.microsoft.com/playwright` (the visual pass's browser, `Dockerfile.visual`), `rustfs/rustfs` and `pgsty/mc` (the
end-to-end suite's S3 server and client, `compose.e2e-bucket.yaml`), `cloudflare/cloudflared` (`compose.tunnel*.yaml`,
an operator's optional tunnel). Each is pulled under its own licence and nothing of it is redistributed here.
