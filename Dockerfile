# syntax=docker/dockerfile:1
# Clippet image (DESIGN.md §2). `docker compose up -d --build` builds the
# final stage from local source; `scripts/check.sh` builds `--target test` AND
# `--target racetest`, at the same time, then `--target crosswindows`; `--target gpu` is the opt-in NVENC
# flavour (DESIGN.md §20 erratum 216, DEPLOY.md §27). The CPU stage is LAST on
# purpose, so a build with no `--target` is the ordinary image and BuildKit
# never even resolves the CUDA bases.
#
# The stage graph (§20 erratum 283):
#
#   golang:1.27-alpine ─ build (on the BUILD platform) ──────── (the binary, cross-compiled)
#
#   golang:1.27-alpine ─ base ─ testdeps ─ testsrc ─┬─ test          (vet + ffmpeg tags)
#                                                   ├─ racetest      (cgo -race)
#                                                   └─ crosswindows  (windows/amd64 builds)
#
# **`build` runs on the builder's own platform and cross-compiles** (run 25, lane
# `pub-ci`). The image is published for linux/amd64 AND linux/arm64; with the
# compiler inside the TARGET platform, the other architecture's `go build` ran
# under QEMU emulation, many times slower. `--platform=$BUILDPLATFORM` keeps the
# Go toolchain native and `GOOS/GOARCH` from BuildKit's `TARGETOS/TARGETARCH`
# pick the output; the binary is static (CGO off), so nothing else changes. On
# a native build the two FROM lines resolve to the same image and BuildKit
# shares the `go mod download` layer between them.
#
# **The three CPU bases are pinned by digest** (`tag@sha256:<index>`, each a
# multi-arch index carrying amd64 and arm64). The tag stays for the reader;
# the digest is what is pulled. Dependabot (.github/dependabot.yml) proposes a
# new digest when a tag is rebuilt, so a base-image patch is a reviewed pull
# request rather than a silent change (DEPLOY.md §17.1).
#
# Two things about that shape are load-bearing and neither is cosmetic.
#
# **`testdeps` sits ABOVE the source.** The static ffmpeg, font-dejavu and the
# cgo toolchain used to be installed AFTER `COPY . .`, so editing one Go file
# re-ran all three — measured at ≈30s of every `check.sh`. They depend on
# nothing in the tree, so they belong on the far side of the copy, where the
# layer cache keeps them for good.
#
# **`test` and `racetest` are SIBLINGS, not a chain.** They are complementary
# passes, not duplicate ones — `-tags ffmpeg` runs only in the first
# (`internal/transcode` 66.8s → 4.2s without it) and `-race` only in the second
# (`internal/httpapi` 48.7s → 218.8s with it) — and neither needs the other's
# result. As siblings off one parent, check.sh can run both at once and the
# shorter hides inside the longer. Everything they SHARE (the source copy,
# gofmt, `go mod verify`) is in `testsrc` and is therefore done once.
FROM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

# The binary's own lineage. It repeats `base`'s first two layers rather than
# starting FROM it, because `base` is the TARGET platform's (the test stages
# run what they compile) and this stage is the BUILDER's.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Set by BuildKit from --platform (linux/amd64, linux/arm64); a plain
# `docker build` sets them to the host's own, so the output is unchanged there.
ARG TARGETOS TARGETARCH
ARG VERSION=dev
# COMMIT is the git object name this build is made from (§20 erratum 303), and
# it defaults to EMPTY on purpose: a local `docker build` with nothing passed
# must not claim a commit, and `GET /api/instance` publishes an absent or
# malformed one as JSON `null`. compose passes `${CLIPPET_COMMIT:-}`; the
# release runbook (DEPLOY.md §17.4) sets that from `git rev-parse HEAD`.
ARG COMMIT=""
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/clippet ./cmd/clippet

# Static binaries, amd64 + arm64. The three CPU bases are pinned by their
# multi-arch index digest (2026-11-06, each read with `docker buildx imagetools
# inspect <image>`; DEPLOY.md §17.1). The digests recorded here
# on 2026-09-06 for golang and alpine had since been rebuilt under the same tags;
# the ffmpeg one had not.
FROM mwader/static-ffmpeg:7.1.1@sha256:11a44711684c0b9f754c047dcd64235b8b52deab251bd0e0a86f22faa160749c AS ffmpeg

# Everything both test passes need that does NOT come from the tree, above the
# source copy so a one-line edit never reinstalls it (§20 erratum 283).
FROM base AS testdeps
COPY --from=ffmpeg /ffmpeg /ffprobe /usr/local/bin/
# font-dejavu is here for the same reason it is in the runtime stage: the LIGHT
# tag's ffmpeg-tagged test draws the real line with the real font and skips
# without one (§20 erratum 214). gcc/musl-dev is the cgo toolchain the -race
# pass needs; it is installed for BOTH leaves rather than inside the race RUN,
# because that is what makes this layer shared and cacheable.
RUN apk add --no-cache font-dejavu gcc musl-dev

# The source, and the two checks over it that both passes want done once.
FROM testdeps AS testsrc
COPY . .
RUN test -z "$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
# go.sum pins a hash for every module in the graph; `go mod verify` re-hashes
# what `go mod download` actually put in the cache and fails if any of it was
# tampered with. It is the reproducibility half of the CI story (DEPLOY.md §17).
RUN go mod verify

# unit + ffmpeg-tagged tests run here (check.sh)
FROM testsrc AS test
RUN go vet ./... && go test -tags ffmpeg ./...
# golangci-lint over the tree (.golangci.yml), its binary mounted from the
# pinned image for this one RUN. This line is the pin: check.sh's `go` stage
# reads the image from it, so the two always lint with the same binary.
RUN --mount=from=golangci/golangci-lint:v2.15.0-alpine@sha256:2ba875b990f1d8501ad4ec19be1ea0701e316ec3a4b764f7b129f62f01318bbf,source=/usr/bin/golangci-lint,target=/usr/local/bin/golangci-lint \
    golangci-lint run ./...

# Race pass (§20 erratum 41). The media path hands one seeker to a goroutine
# http.ServeContent starts and never joins, and nothing but -race catches
# that class; -race needs cgo, hence the toolchain in `testdeps`. No ffmpeg
# tag: this pass is about concurrency, not encoders. A SIBLING of `test` and
# not a continuation of it, so check.sh runs the two concurrently — see the
# stage graph at the top of this file.
FROM testsrc AS racetest
RUN CGO_ENABLED=1 go test -race -count=1 ./...

# Does this tree still build for Windows? (§20 erratum 308, check.sh's
# `cross-compile: windows/amd64` stage.)
#
# The product ships as a Linux container and always will, but the binary is
# also something a person runs on Windows — `clippet.exe admin doctor` against
# a box's own data directory — and three files exist only for that target
# (cmd/clippet/freebytes_windows.go, internal/worker/freebytes_windows.go, and
# the `!unix && !windows` stubs behind them). Nothing else here compiles them:
# `go vet ./...` and `go test` only ever see the host's GOOS, so a dependency
# that pulls in cgo, a syscall that is not on that platform, or a build tag
# that stopped covering it would all pass every other stage and be discovered
# by whoever next tried to make a Windows build.
#
# A SIBLING of `test` and `racetest` off the same `testsrc`, for the same
# reason they are siblings of each other: it needs neither's result, so it
# shares their source copy, their gofmt and their `go mod verify` and adds one
# link step rather than a second tree.
#
# `-o /dev/null` because the artefact is not wanted — a real Windows build is
# made on a Windows host with `CLIPPET_VERSION` set (DEPLOY.md §16). What is
# wanted is the compiler's verdict.
FROM testsrc AS crosswindows
RUN GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/clippet

# ---- the GPU flavour, opt-in and never the default (§20 erratum 216) --------
# A full CUDA path — hardware decode, scale_cuda, h264_nvenc — was measured at
# 9-11x libx264 AT ONE CPU on an RTX 5090, and is nearly core-count independent.
# It needs an ffmpeg with NVENC, which the static build has none of, so this
# stage takes its binaries from a CUDA-enabled build and its runtime from the
# CUDA runtime image. Nothing else about the app changes: TRANSCODE_HWACCEL=cuda
# selects the argv, and every plan a CUDA graph cannot render (HDR, interlaced,
# tag-rotated, anamorphic) falls back to the CPU one on this same binary.
#
# Digests as of 2026-09-16, RECORDED, not pinned (the three CPU bases above are
# pinned; these two stay tags because the CUDA path is amd64-only and opt-in), for the same recovery reason
# (§20 erratum 292 — the two lines below had promised them since 2026-09-08 and
# listed none). Re-read them with `docker buildx imagetools inspect <image>`; the
# ffmpeg one is a SINGLE amd64 manifest, not a multi-arch index, and amd64 is the
# one platform the CUDA path has ever been proven on (erratum 247):
#   jrottenberg/ffmpeg:7.1-nvidia               sha256:8cf54406a51a382f9c0e71961243546fafddf9c1cf7562a656633b076223241e   (ffmpeg 7.1, h264/hevc/av1_nvenc, scale_cuda, scale_npp; linux/amd64 only)
#   nvidia/cuda:12.6.2-runtime-ubuntu24.04      sha256:16411bb06f363425265b410810a86fc43da705847ee9721be45b478bc4d0ea26   (index: linux/amd64 + linux/arm64)
FROM jrottenberg/ffmpeg:7.1-nvidia AS gpuffmpeg
FROM nvidia/cuda:12.6.2-runtime-ubuntu24.04 AS gpu
# The CUDA base asks the container runtime for the `compute,utility` driver
# libraries only. NVDEC and NVENC live behind the `video` capability
# (libnvcuvid.so.1, libnvidia-encode.so.1); without it every `-hwaccel cuda`
# decode dies with "Cannot load libnvcuvid.so.1" while nvidia-smi and the
# boot capability check both pass, because the ENCODER is listed by ffmpeg
# whether or not the driver library it dlopens at run time is present. Found
# on the first live container proof (DESIGN.md erratum 247).
ENV NVIDIA_DRIVER_CAPABILITIES=compute,utility,video
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl tzdata fonts-dejavu-core \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 10001 clippet && useradd -u 10001 -g 10001 -M clippet \
 && mkdir -p /data /scratch && chown -R 10001:10001 /data /scratch
# The binaries AND their libraries. This is the one place the investigation's
# recipe was incomplete, and it fails loudly: that ffmpeg is DYNAMICALLY linked
# against /usr/local/lib (libavdevice.so.61 and thirty more), so copying the two
# executables alone produced an image whose ffmpeg exits 127 on every invocation
# ("error while loading shared libraries") — which the boot capability check
# would then report as "this ffmpeg lists no h264_nvenc". Both images are Ubuntu
# 24.04 CUDA images, so the libraries drop in where they were built to live.
COPY --from=gpuffmpeg /usr/local/lib /usr/local/lib
COPY --from=gpuffmpeg /usr/local/bin/ffmpeg /usr/local/bin/ffprobe /usr/local/bin/
RUN ldconfig
# Spline Sans Mono is the face of the duration burned into every poster's
# top-right corner (§20 erratum 287): web/STYLE.md gives that face exactly this
# job -- "things a machine measured", timecodes among them -- and the page prints
# the same number in the same face under the player. It is under the SIL Open
# Font License 1.1; the licence text ships beside it and the file carries its own
# copyright and licence URL in its name table (DEPLOY §27). Unlike DejaVu it IS
# in the repository, because it is this product's own typeface and no package
# manager on either base has it. One static instance at weight 500, 24 KB.
COPY web/static/fonts/spline-sans-mono.ttf web/static/fonts/OFL.txt /usr/local/share/clippet/fonts/
COPY --from=build /out/clippet /usr/local/bin/clippet
USER 10001
EXPOSE 8080
VOLUME ["/data", "/scratch"]
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
  CMD curl -fsS http://localhost:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["clippet"]
CMD ["serve"]

# ---- the ordinary image: LAST, so it is what a targetless build produces ----
# Named so CI can name it (`--target runtime`); a build with no --target is this stage all the same.
FROM alpine:3.22@sha256:bfe7ceb9f09ec0566c48fb93356e45a47a00be5a1dd14fa5b909e7709c66b034 AS runtime
# font-dejavu is the LIGHT tag's font (§20 erratum 214): one small
# semi-transparent line burned into the card's copy, naming the preview quality
# and the real one. DejaVu is under the DejaVu Fonts License — free to use,
# redistribute and modify, no attribution required in the rendered output — and
# it is not vendored into this repository, only installed here. DEPLOY §27
# records the licence; LIGHT_TAG_TEXT=off removes the line and this stays.
RUN apk add --no-cache ca-certificates tzdata curl font-dejavu \
 && addgroup -S -g 10001 clippet && adduser -S -u 10001 -G clippet clippet \
 && mkdir -p /data /scratch && chown -R 10001:10001 /data /scratch
# Spline Sans Mono is the face of the duration burned into every poster's
# top-right corner (§20 erratum 287): web/STYLE.md gives that face exactly this
# job -- "things a machine measured", timecodes among them -- and the page prints
# the same number in the same face under the player. It is under the SIL Open
# Font License 1.1; the licence text ships beside it and the file carries its own
# copyright and licence URL in its name table (DEPLOY §27). Unlike DejaVu it IS
# in the repository, because it is this product's own typeface and no package
# manager on either base has it. One static instance at weight 500, 24 KB.
COPY web/static/fonts/spline-sans-mono.ttf web/static/fonts/OFL.txt /usr/local/share/clippet/fonts/
COPY --from=ffmpeg /ffmpeg /ffprobe /usr/local/bin/
COPY --from=build /out/clippet /usr/local/bin/clippet
USER 10001
EXPOSE 8080
VOLUME ["/data", "/scratch"]
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
  CMD curl -fsS http://localhost:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["clippet"]
CMD ["serve"]
