#!/usr/bin/env sh
set -eu; cd "$(dirname "$0")/.."; export MSYS_NO_PATHCONV=1   # safe here: this script passes no host paths to native tools
# THE STAGES RUN AT THE SAME TIME (lane `speed2`). Until this lane every stage below ran after the one
# before it, and the whole of check.sh was the sum of them — six to seven minutes on a 16-CPU box, five of
# them the Go stage. Every stage is independent of every other (each reads the tree and writes nothing
# another reads), so each now runs in a subshell of its own, at once, with its output in a log of its
# own under .e2e/check/; when the last one has finished, every stage is printed IN THE FIXED ORDER below
# — its title, its output, its seconds and its verdict — so the transcript reads the same whichever
# finished first, and the closing line names every stage that failed. No check changed: each stage's
# body is the text it always was, inside a function.
#
# The Go stage is the one that is not seconds, and it is the one that changed shape — see its own
# description below: the tests are sharded across CHECK_JOBS processes in one container, and the whole
# stage is SKIPPED when the Go tree is byte-for-byte one that has already passed it (the checkpoint;
# CHECK_FULL=1 runs it anyway). The skip is printed every time, with the hash and when it was proved.
#
# WHAT THE GO STAGE RUNS BY DEFAULT (run 9, lane `t0`): gofmt, `go mod verify`, `go vet` with and without the
# ffmpeg tag, the windows cross-compile, the runtime image, and ONE test pass — `go test -tags ffmpeg ./...`,
# every test, no -race, no -count=1 — in a container that mounts the named volume clippet-gobuild as Go's
# cache, so an unchanged package answers `(cached)`. The race detector is the CHECK_RACE knob's.
#
# Knobs (each printed once, at the top of the run):
#   CHECK_JOBS=N     how many Go test shards run at once (default: this machine's CPU count)
#   CHECK_FULL=1     run the Go stage even when the checkpoint says this tree has passed it
#   CHECK_RACE=1     add a -race pass over the goroutine owners (scripts/lib/gocheck.sh's GOCHECK_RACE_DEFAULT);
#   CHECK_RACE=all   ... over ./... (T2 and the nightly set this); CHECK_RACE='./internal/a ./cmd/b' over that list.
#                    With a race pass no test runs twice: a raced package's untagged tests run only under -race
#                    (gocheck.sh says which few run in both, and why). A checkpoint records its race pass and only
#                    skips a run whose race pass it covers (all covers everything; none covers only none).
#   CHECK_GOMAXPROCS=N  the GOMAXPROCS every Go test shard runs with (default 4; 0 = Go's default, every CPU —
#                    run 9, lane `pool`: CHECK_JOBS shards × every CPU's Ps beside a docker build oversubscribed the box)
#   CHECK_ONLY=ids   run only these stages (space-separated ids from the titles' order: 01 … 18, pii, sleeplint, t1, gatestack, wslexec, boxtidy, go);
#                    CHECK_ONLY=cheap is every stage but go — scripts/t0.sh runs that beside its own Go container
#   CHECK_DIR=path   where the stage logs go, relative to the checkout (default .e2e/check; t0.sh: .e2e/t0/check)
CK_T0=$(date +%s)
CK_DIR=${CHECK_DIR:-.e2e/check}   # relative to the checkout; t0.sh points it at .e2e/t0/check
rm -rf "$CK_DIR/stages"; mkdir -p "$CK_DIR/stages" .e2e/checkpoints
CK_NCPU=$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
case $CK_NCPU in ''|*[!0-9]*|0) CK_NCPU=4 ;; esac
CK_JOBS_FROM="CHECK_JOBS"
if [ -z "${CHECK_JOBS:-}" ]; then CHECK_JOBS=$CK_NCPU; CK_JOBS_FROM="the default: one per CPU"; fi
case $CHECK_JOBS in ''|*[!0-9]*|0) CHECK_JOBS=$CK_NCPU ;; esac
export CHECK_JOBS
CHECK_RACE=${CHECK_RACE:-}; export CHECK_RACE
CHECK_ONLY=${CHECK_ONLY:-}
echo "[check] $CK_NCPU CPUs; Go test shards $CHECK_JOBS at a time ($CK_JOBS_FROM); CHECK_FULL=${CHECK_FULL:-0}; CHECK_RACE=${CHECK_RACE:-unset}${CHECK_ONLY:+; CHECK_ONLY=$CHECK_ONLY}; every stage at once, printed in order below"
# The python every python-backed stage uses, found ONCE before the stages fork (it used to be set by the
# jget stage and read by the three after it, which a subshell per stage would lose).
PY=; for c in python python3 "py -3"; do if $c -c 'import sys; assert sys.version_info >= (3, 8)' >/dev/null 2>&1; then PY=$c; break; fi; done
# ck_add <id> <title> — register a stage; st_<id> is its body, defined right below its ck_add line.
: > "$CK_DIR/stages/order"
ck_add() { printf '%s\n' "$1" >> "$CK_DIR/stages/order"; printf '%s\n' "$2" > "$CK_DIR/stages/$1.title"; }

ck_add 01 "[check] compose config"
st_01() {
docker compose config -q
}
ck_add 02 "[check] shell syntax"
st_02() {
for f in scripts/*.sh; do bash -n "$f"; done
}
ck_add 03 "[check] every variable internal/config reads is a row of compose.yaml's app environment: block (§20 erratum 292; AUDIT #14: STORAGE_PING_ATTEMPTS and STORAGE_PING_INTERVAL were documented in DEPLOY §7 and .env.example, read by config.go, and reached NO container, because that block is the only path from .env to the process and nothing compared the two lists. The left list is every l.<kind>(\"NAME\" call in internal/config's non-test files — the helper may carry a digit, l.int64Min — plus every Env<Name> = \"NAME\" constant, which is how discordbot.go names its four; the right is every KEY under the app service's environment:. A name on the left and not on the right fails. The floor of 50 names guards the extraction itself: a regex that rots to nothing would otherwise pass vacuously)"
st_03() {
                                    cfg_names() { { grep -ohE --exclude='*_test.go' 'l\.[a-zA-Z0-9]+\("[A-Z][A-Z0-9_]*"' "$1"/internal/config/*.go | sed 's/.*("//'
                                                    grep -ohE --exclude='*_test.go' '^[[:space:]]*Env[A-Za-z0-9]+[[:space:]]*=[[:space:]]*"[A-Z][A-Z0-9_]*"' "$1"/internal/config/*.go | sed 's/.*"\([A-Z0-9_]*\)"/\1/'; } | sed 's/"$//' | sort -u; }
                                    env_names() { awk '/^  app:/{a=1} a && /^    environment:/{e=1; next} e && /^    [a-z]/{e=0; a=0} e && /^      [A-Z][A-Z0-9_]*:/{sub(/^ +/, ""); sub(/:.*/, ""); print}' "$1/compose.yaml" | sort -u; }
                                    t=$(mktemp -d); mkdir -p "$t/internal/config"
                                    printf 'package config\n\nconst (\n\tEnvQux = "QUX"\n)\n\nfunc f(l *loader) {\n\tl.str("FOO", "")\n\tl.duration("BAR", 0)\n\tl.int64Min("BIG", 0, 0)\n\tl.fail("BAR", "a sentence")\n\tl.str(EnvQux, "")\n}\n' > "$t/internal/config/config.go"
                                    printf 'package config\n\nfunc g(l *loader) { l.str("ONLY_IN_TEST", "") }\n' > "$t/internal/config/config_test.go"
                                    printf 'services:\n  app:\n    environment:\n      FOO: ${FOO:-}\n      BAZ: ${BAZ:-}\n    volumes:\n      - x:/x\n  minio:\n    environment:\n      BAR: ${BAR:-}\n' > "$t/compose.yaml"
                                    cfg_names "$t" > "$t/names.cfg"; env_names "$t" > "$t/names.env"; miss=$(comm -23 "$t/names.cfg" "$t/names.env" | tr '\n' ' '); rm -rf "$t"
                                    [ "$miss" = "BAR BIG QUX " ] || { echo "  self-test failed: the diff must name BAR (passed only to minio), BIG (an int64 helper) and QUX (an Env constant), clear FOO, ignore ONLY_IN_TEST and the extra BAZ (got '$miss')"; exit 1; }
                                    t=$(mktemp -d); cfg_names . > "$t/names.cfg"; env_names . > "$t/names.env"; n=$(wc -l < "$t/names.cfg" | tr -d ' ')
                                    [ "$n" -ge 50 ] || { rm -rf "$t"; echo "  the extraction found only $n names in internal/config (want >= 50): the l.<kind>(\"NAME\" spelling this stage greps for has moved"; exit 1; }
                                    miss=$(comm -23 "$t/names.cfg" "$t/names.env"); rm -rf "$t"
                                    [ -z "$miss" ] || { printf '%s\n' "$miss" | sed 's/^/  /'; echo "  the variable(s) above are read by internal/config and passed by NO row of compose.yaml's app environment: block, so a .env line setting them is silently inert (§20 erratum 292): add the row with its documented default"; exit 1; }
}
ck_add 04 "[check] no compose file runs a moving :latest tag (§20 erratum 292; AUDIT #10: the production tunnel ran cloudflare/cloudflared:latest, so every docker compose up -d after a registry push was an unreviewed upgrade of the ingress. Every other image here is pinned — DEPLOY.md §17.1 — and this stage keeps the set closed over compose*.yaml, comments included: a :latest in a comment is a recipe somebody will paste. A default spelled \${TAG:-latest} or a -latest tag counts too. ONE exemption, lane image1: this project's OWN image, ghcr.io/clippet-gg/clippet, whose latest and gpu-latest tags compose.yaml's default pulls — they are its own release tags, moved only by its own release workflow (.github/workflows/image.yml) on a release tag, so they are a release, not an unreviewed upgrade. Only that reference is stripped from a line before the grep; anything else on the same line still fails)"
st_04() {
                                    latest_grep() { grep -nE '(:|-)latest([^A-Za-z0-9_.]|$)' "$@" | sed -E 's#ghcr\.io/clippet-gg/clippet:(\$\{[A-Z_]+:-)?(gpu-)?latest##g' | grep -E '(:|-)latest([^A-Za-z0-9_.]|$)' || [ $? = 1 ]; }
                                    t=$(mktemp -d)
                                    printf 'services:\n  a:\n    image: x/y:latest\n' > "$t/a.yaml"; printf 'services:\n  a:\n    image: x/y:1.2.3\n' > "$t/b.yaml"
                                    printf 'services:\n  a:\n    image: ${A_IMAGE:-x/y:${T:-latest}}\n' > "$t/c.yaml"; printf '# pull it: docker pull x/y:latest\n' > "$t/d.yaml"
                                    printf 'services:\n  a:\n    image: ${CLIPPET_IMAGE:-ghcr.io/clippet-gg/clippet:${CLIPPET_VERSION:-latest}}\n# or ghcr.io/clippet-gg/clippet:latest, ghcr.io/clippet-gg/clippet:gpu-latest\n' > "$t/e.yaml"
                                    printf 'services:\n  a:\n    image: ghcr.io/clippet-gg/clippet:latest # beside x/y:latest\n' > "$t/f.yaml"; printf 'services:\n  a:\n    image: ghcr.io/clippet-gg/clippet-other:latest\n' > "$t/g.yaml"
                                    hits=$(latest_grep "$t"/*.yaml); rm -rf "$t"
                                    got=$(printf '%s\n' "$hits" | sed -n 's#^.*/\([a-z]\)\.yaml:.*#\1#p' | tr -d '\n')
                                    [ "$got" = acdfg ] || { echo "  self-test failed: the grep must flag a third-party :latest (a), a \${T:-latest} default (c), one in a comment (d), one beside our own on a line (f) and another image of the same owner (g), and clear a pin (b) and our own image's three spellings (e) (got '$got')"; exit 1; }
                                    hits=$(latest_grep compose*.yaml); [ -z "$hits" ] || { printf '%s\n' "$hits" | sed 's/^/  /'; echo "  the compose file(s) above name a :latest tag: pin the version (DEPLOY.md §17.1 says how to bump it)"; exit 1; }
}
ck_add 05 "[check] jget.py selftest"
st_05() {
PY=; for c in python python3 "py -3"; do if $c -c 'import sys; assert sys.version_info >= (3, 8)' >/dev/null 2>&1; then PY=$c; break; fi; done
                                    if [ -n "$PY" ]; then $PY scripts/lib/jget.py --selftest; else echo "  skipped: no python >= 3.8 on PATH (e2e.sh needs one)"; fi
}
ck_add 06 "[check] the companion mark's files match their generator (§20 errata 137, 301, 304, 306: web/static/icon-192.png, icon-512.png and icon-dev-192.png (erratum 316), web/static/favicon.ico, the four Discord icons and docs/brand/mark.svg, and the favicon data: URI on base.html and embed.html are all DRAWN by scripts/lib/mkicons.py and committed, so nothing but this stage notices a hand-edited or stale one. The PNG comparison is over DECODED pixel rows, never the compressed bytes, which depend on the interpreter's zlib — and the ICO is read the same way, its directory walked and each of its three PNG entries decoded and compared, never the container bytes; the SVGs and the two favicon lines are compared as text)"
st_06() {
                                    if [ -n "$PY" ]; then $PY scripts/lib/mkicons.py --check; else echo "  skipped: no python >= 3.8 on PATH"; fi
}
ck_add 07 "[check] the link-out card placeholder matches its generator (§20 erratum 287: web/static/card-placeholder.png is the picture Discord puts on every processing clip's card and on a FAILED clip's card for ever, and since the redraw it is DRAWN by scripts/lib/mkcard.py and committed as a binary. Same shape of stage as the icons above, same reason and the same DECODED-pixel comparison; it costs about half a second, because the mark occupies a fifth of the card and the rest is one flat colour that needs no scanline pass)"
st_07() {
                                    if [ -n "$PY" ]; then $PY scripts/lib/mkcard.py --check; else echo "  skipped: no python >= 3.8 on PATH"; fi
}
ck_add 08 "[check] no outbound URL literals (§15 / §20 erratum 27: the binary opens no outbound connection but S3_ENDPOINT's, Discord's allowlisted hosts when the bot or a webhook is on, and the webhook you set; tests and docs may name URLs). The scan is LINE-based and does NOT skip comments: a URL written in PROSE fails exactly as a real egress would, in Go, HTML (including a {{/* … */}} template comment), JS and CSS alike. That is deliberate — the reviewer cannot tell a commented-out client from a sentence, and two prose comments have already reached this gate — so write no literal http(s):// URL in any comment under internal/, cmd/ or web/. Say 'the tunnel origin' or split the scheme. The self-test below pins the behaviour in all four languages."
st_08() {
                                    url_grep() { grep -rnE --include='*.go' --include='*.html' --include='*.js' --include='*.css' --exclude='*_test.go' 'https?://' "$@"; }
                                    t=$(mktemp -d); printf 'package p\n\n// a dev stack on https://example.com stays quiet\nconst x = "https://example.com"\n' > "$t/a.go"; printf 'package p\n\nconst y = "http://example.com"\n' > "$t/a_test.go"
                                    printf '{{- /* the origin https://example.com */ -}}\n' > "$t/a.html"; printf '// prose only: see https://example.com\n' > "$t/a.js"; printf '/* https://example.com */\n' > "$t/a.css"
                                    n=$(url_grep "$t" | wc -l | tr -d ' '); rm -rf "$t"; [ "$n" = 5 ] || { echo "  self-test failed: the grep must flag a.go twice (the const AND the comment), the html/js/css comments once each, and skip a_test.go (got $n hits, want 5)"; exit 1; }
                                    hits=$(url_grep internal cmd web || [ $? = 1 ]); [ -z "$hits" ] || { printf '%s\n' "$hits"; echo "  outbound URL literal(s) above: not allowed outside tests and docs (§15) — a COMMENT counts, see the stage description"; exit 1; }
}
ck_add 09 "[check] one outbound HTTP call site (§20 errata 66, 96: only internal/notify may build a client, transport, request or dialer, and the scan covers internal, cmd AND web — web/{static,templates}/embed.go are compiled into the binary. A TRIPWIRE over the common spellings, not a proof — the guarantee is the host allowlist in internal/config (plus the one URL it does not list: the webhook you set, CLIP_WEBHOOK_URL) and the single construction site in serve.go. The S3 client is the minio module and is in scope by design; net.Listen is absent from the expression on purpose, being serve.go's INBOUND listener)"
st_09() {
                                    client_grep() { grep -rnE --include='*.go' --exclude='*_test.go' 'net\.(Dial|Dialer)|http\.(Transport|Client|Request)\{|\bhttp\.Client\b|new\(http\.(Client|Transport|Request)\)|httputil\.|http\.(NewRequest|Post|PostForm|Get\(|Head\(|DefaultClient|DefaultTransport)' "$@"; }
                                    t=$(mktemp -d); printf 'package p\n\nvar c http.Client\nvar d = new(http.Client)\nvar e = net.Dialer{}\nvar f = httputil.NewSingleHostReverseProxy(nil)\n' > "$t/a.go"; printf 'package p\n\nvar c http.Client\nvar e = net.Dialer{}\n' > "$t/a_test.go"
                                    n=$(client_grep "$t" | wc -l | tr -d ' '); rm -rf "$t"; [ "$n" = 4 ] || { echo "  self-test failed: the grep must flag all 4 planted forms in a.go (zero value, new(), a dialer literal, httputil) and skip a_test.go (got $n hits)"; exit 1; }
                                    hits=$(client_grep internal cmd web || [ $? = 1 ]); hits=$(printf '%s' "$hits" | grep -v '^internal/notify/' || [ $? = 1 ])
                                    [ -z "$hits" ] || { printf '%s\n' "$hits"; echo "  outbound HTTP call site(s) above live outside internal/notify, the one package that may build a client (§15)"; exit 1; }
}
ck_add 10 "[check] every ffmpeg input is protocol-whitelisted (§15 / §20 erratum 91: ffmpeg/ffprobe are the second potential egress path — a demuxer opens what its input names — and the pipeline reads nothing but files. A TRIPWIRE over the common spelling, not a proof: any non-test file under internal/transcode that spells an -i of its own must also name a whitelist. The guarantee is inputArgs/probeInputArgs in internal/transcode/exec.go, which every argv builder goes through, and the container allowlist in ParseProbe)"
st_10() {
                                    wl_scan() { grep -rl --include='*.go' --exclude='*_test.go' '"-i"' "$@" | while read -r f; do grep -q 'protocol_whitelist' "$f" || echo "$f"; done; }
                                    t=$(mktemp -d); printf 'package p\n\nvar a = []string{"-i", in}\n' > "$t/a.go"; printf 'package p\n\nvar b = []string{"-protocol_whitelist", "file", "-i", in}\n' > "$t/b.go"; printf 'package p\n\nvar c = []string{"-i", in}\n' > "$t/a_test.go"
                                    n=$(wl_scan "$t" | wc -l | tr -d ' '); rm -rf "$t"; [ "$n" = 1 ] || { echo "  self-test failed: the scan must flag a.go, clear b.go and skip a_test.go (got $n hits)"; exit 1; }
                                    hits=$(wl_scan internal/transcode); [ -z "$hits" ] || { printf '%s\n' "$hits"; echo "  the file(s) above build an ffmpeg input without a -protocol_whitelist: build it with inputArgs (§20 erratum 91)"; exit 1; }
}
ck_add 11 "[check] the product display name is spelled once per package and the two spellings agree (§20 erratum 234; nit sweep S03: internal/httpapi/oembed.go's oembedProvider is provider_name and internal/web/meta.go's siteName is og:site_name. DESIGN §3 forbids an import in either direction, so nothing but this stage can pin them. An EMPTY match fails too, so renaming or moving either const trips it rather than silently passing)"
st_11() {
                                    name_oembed() { sed -n 's/^[[:space:]]*oembedProvider[[:space:]]*= "\(.*\)"$/\1/p' "$1/internal/httpapi/oembed.go"; }
                                    name_web()    { sed -n 's/^[[:space:]]*siteName[[:space:]]*= "\(.*\)"$/\1/p' "$1/internal/web/meta.go"; }
                                    t=$(mktemp -d); mkdir -p "$t/internal/httpapi" "$t/internal/web"
                                    printf 'package httpapi\n\nconst (\n\toembedProvider = "Same"\n)\n' > "$t/internal/httpapi/oembed.go"
                                    printf 'package web\n\nconst (\n\tsiteName = "Other"\n)\n' > "$t/internal/web/meta.go"
                                    [ "$(name_oembed "$t")" = "Same" ] && [ "$(name_web "$t")" = "Other" ] || { rm -rf "$t"; echo "  self-test failed: the extraction does not read a planted const"; exit 1; }
                                    printf 'package web\n\nconst (\n\tsiteNameRenamed = "Other"\n)\n' > "$t/internal/web/meta.go"
                                    [ -z "$(name_web "$t")" ] || { rm -rf "$t"; echo "  self-test failed: a renamed const must extract as empty"; exit 1; }
                                    rm -rf "$t"
                                    one=$(name_oembed .); two=$(name_web .)
                                    { [ -n "$one" ] && [ "$one" = "$two" ]; } || { echo "  oembedProvider=${one:-<not found>} and siteName=${two:-<not found>} must be the same non-empty product name (§20 erratum 234)"; exit 1; }
}
ck_add 12 "[check] only internal/worker and internal/db write a job's last_error (§20 errata 165, 234; nit sweep S06: worker.OwnerError and db.IsOperatorReason both depend on that column carrying either a media kind's own fixed vocabulary or one of internal/db's two operator sentences. A future admin verb calling FailAttempt with its own wording would put operator jargon straight onto the public GIF 503 and the three owner DTOs, silently)"
st_12() {
                                    reason_grep() { grep -rnE --include='*.go' --exclude='*_test.go' '\.(FailAttempt|FailAttemptAfter|RequeueWithBackoff|CompleteJob)\(' "$@" || [ $? = 1 ]; }
                                    t=$(mktemp -d); printf 'package p\n\nfunc f() {\n\td.FailAttempt(ctx)\n\td.FailAttemptAfter(ctx)\n\td.RequeueWithBackoff(ctx)\n\td.CompleteJob(ctx)\n}\n' > "$t/a.go"; printf 'package p\n\nfunc g() { d.FailAttempt(ctx) }\n' > "$t/a_test.go"
                                    n=$(reason_grep "$t" | wc -l | tr -d ' '); rm -rf "$t"; [ "$n" = 4 ] || { echo "  self-test failed: the grep must flag all four writers in a.go and skip a_test.go (got $n hits)"; exit 1; }
                                    hits=$(reason_grep internal cmd web | grep -vE '^internal/(worker|db)/' || [ $? = 1 ])
                                    [ -z "$hits" ] || { printf '%s\n' "$hits"; echo "  the call site(s) above write jobs.last_error from outside internal/worker and internal/db: word the ending in the kind's own vocabulary inside internal/worker, or add the sentence to db.IsOperatorReason (§20 erratum 165)"; exit 1; }
}
ck_add 13 "[check] no E2E drop-in assigns one of the harness's own globals (§20 erratum 234; nit sweep S22: scripts/e2e.d/*.sh are SOURCED into scripts/e2e.sh, so every name is shared. The shared clips A-E and X (steps 3-6; W was step 23's until run 9) and the helper globals are what every step reads. BURST_N is the count burst_429 leaves behind, and E2E_BURST_CHUNK/E2E_RETRY_PAUSE are the two knobs it and the retry wrappers read (§20 erratum 283). E2E_BUNDLE is the stem bundle_account reads its declared suffixes from (run 9 lane select): a drop-in that set it would register under another bundle's declaration. The expression names ONLY what the harness owns - P, Q, R, S, N and the two-letter prefixes drop-ins already use are free, which is why this is not a general rule about short names)"
st_13() {
                                    dropin_grep() { grep -nE '^[[:space:]]*(A|B|C|D|E|F|G|H|I|J|T|U|V|W|X|Y|Z|BASE|PAGE|RESP|HDR|JAR|PY|SECURE|E2E_DIR|PAGEW|RESPW|HDRW|JARW|E2E_MEDIA|E2E_MEDIA_ORIGIN|MEDIA_LOC|SM_PREFIX|SM_BACKEND|BURST_N|E2E_BURST_CHUNK|E2E_RETRY_PAUSE|E2E_RETRY|E2E_ATTEMPT|E2E_BUNDLE)=' "$@" || [ $? = 1 ]; }
                                    t=$(mktemp -d); printf 'X=1\nOX=2\n  W="clip"\nMYVAR=3\n' > "$t/a.sh"
                                    n=$(dropin_grep "$t/a.sh" | wc -l | tr -d ' '); rm -rf "$t"; [ "$n" = 2 ] || { echo "  self-test failed: the grep must flag X= and W= and leave OX=/MYVAR= alone (got $n hits)"; exit 1; }
                                    hits=$(dropin_grep scripts/e2e.d/*.sh); [ -z "$hits" ] || { printf '%s\n' "$hits"; echo "  the assignment(s) above clobber a scripts/e2e.sh global: prefix a drop-in's names with a letter of its own (scripts/e2e.d/README.md's first convention)"; exit 1; }
}
ck_add 14 "[check] a probe that boots clippet serve on a private DATA_DIR also sets a private SCRATCH_DIR (§20 erratum 234; nit sweep S26, and the one MAJOR trap of the shared volumes: compose still mounts the service's volumes, so redirecting DATA_DIR alone boots an EMPTY database over the LIVE /scratch and serve.go's boot janitor deletes every real working directory - an in-flight transcode then fails permanent. A probe that does NOT run serve has no janitor and is exempt, which is why the rule is per FILE and keyed on both spellings. The scan skips exactly ONE path, scripts/check.sh, which is this file: the sweep below is over scripts/*.sh, so the stage was scanning itself and clearing itself only on the strength of the SCRATCH_DIR inside its own self-test printfs - delete those two lines and the stage would have flagged check.sh. It used to skip any file NAMED check.sh, which would have exempted a hypothetical scripts/e2e.d/check.sh as well; the skip is a statement about this file and is now spelled as one - §20 erratum 275.)"
st_14() {
                                    scratch_scan() { for f in "$@"; do case $f in scripts/check.sh|*/scripts/check.sh) continue ;; esac
                                      if grep -q 'DATA_DIR=' "$f" && grep -q 'clippet serve' "$f"; then grep -q 'SCRATCH_DIR=' "$f" || echo "$f"; fi; done; }
                                    t=$(mktemp -d); printf 'dc run -e DATA_DIR=/tmp/x app sh -c "clippet serve"\n' > "$t/bad.sh"
                                    printf 'dc run -e DATA_DIR=/tmp/x -e SCRATCH_DIR=/tmp/x-scratch app sh -c "clippet serve"\n' > "$t/good.sh"
                                    printf 'dc run -e DATA_DIR=/tmp/doctor app admin doctor\n' > "$t/doctor.sh"
                                    printf 'dc run -e DATA_DIR=/tmp/x app sh -c "clippet serve"\n' > "$t/check.sh"
                                    mkdir -p "$t/scripts"; printf 'dc run -e DATA_DIR=/tmp/x app sh -c "clippet serve"\n' > "$t/scripts/check.sh"
                                    n=$(scratch_scan "$t"/*.sh "$t"/scripts/*.sh | wc -l | tr -d ' '); rm -rf "$t"; [ "$n" = 2 ] || { echo "  self-test failed: the scan must flag the serve probe with no SCRATCH_DIR, clear the one that has it, exempt the doctor probe, skip only scripts/check.sh - and FLAG a flagworthy file merely named check.sh somewhere else (got $n hits, want 2)"; exit 1; }
                                    hits=$(scratch_scan scripts/*.sh scripts/e2e.d/*.sh); [ -z "$hits" ] || { printf '%s\n' "$hits"; echo "  the file(s) above boot clippet serve on a private DATA_DIR with no private SCRATCH_DIR: pass -e SCRATCH_DIR next to -e DATA_DIR (scripts/e2e.d/README.md's third convention)"; exit 1; }
}
ck_add 15 "[check] the E2E's two retry wrappers and its sweep's two patterns, against stubs (scripts/e2e.sh's E2E_SELFTEST=1, which runs before step 1 and touches no daemon, no network and no stack: dc retries once on docker's exit 125 - the daemon refused, never a code the clippet binary can produce - and curl_ once on curl's 7/28/35, the three that mean the request got no answer. Neither branch has fired in a real run, so without this a typo inside one would surface for the first time DURING the failure it exists to absorb. It also pins what must NOT be retried: http() carries the writes and a repeated write is a wrong assertion, so a non-125 docker failure and an exit-22 curl go straight through. And it pins e2e_sweep_own's guards against planted text (§20 errata 272 and 275): E2E_OWN_RE, which decides which ACCOUNTS the harness may ever empty, E2E_ID_RE, which decides what it will accept from admin ls as a target, and the collections listing the sweep reads before a prune - a wrong answer from any of them is somebody's wiped stack, so none is left to be exercised for the first time on a live one. It also pins the four MEDIA helpers, which no live run can pin (§20 erratum 283 (h)): media_redact_, which is what keeps X-Amz-Signature and X-Amz-Credential out of a transcript that lives on disk; media_no_cookie_, which is what keeps the session cookie from reaching a third-party bucket and 'can only ever fire on a harness mistake', i.e. never, i.e. it was dead code nobody could watch rot; media_origin_, which decides the one origin every signed Location is compared against; and media_total_, which every size comparison in redirect mode rests on. A third wrapper is pinned here too: http_refused, the bounded retry for a write whose EXPECTED answer is a refusal, which retries an edge 502/520-524 or a 7/28/35 exactly once through a NON-LOOPBACK origin and never on loopback. A few seconds: the pause the retry branches take is E2E_RETRY_PAUSE, which this run sets to 0 after asserting that its DEFAULT is 3 - the one place in the suite that ever says so (§20 erratum 283 (d)). Since §20 erratum 290 it also pins the scripts' DESTRUCTIVE edges, each against a stub or a scratch directory: the EXIT-trap hand-back of a shared stack (once, status kept, invisible to a bundle's subshell, never armed here), the stale-probe sweep's project-label filter, E2E_CLEAN's keep-list, the resume key's .env fold, the mc rm prefix guard, lane-teardown --check (dev refused, no .env refused, the wrong project refused) and its removal half against a docker stub (the project by name, exactly the lane's own volumes and image, a lane with none left not stopping the run), visual.sh's tidy-up gate on a foreign BASE, and testclips.sh's regeneration of an unreadable fixture. Since lane speed2 it also pins the E2E scheduler: the pool on planted stub bundles (the slot cap, two bundles declared apart never together, an after honoured, a cycle reported as a DEADLOCK), the longest-first planner and its durations file, the width default, and this file's own Go-tree checkpoint hash on a fixed scratch tree. Since run 8 (lane harness9) it also pins that no script prints a credential-named variable whole on a failure (redact_, lacks_secret_f, the request ring's inbound-URL redaction, and a grep over every die/expect/lacks_file) and that every dc run probe starts with probe_env_blank (the cookie name, Discord's webhook, the clip webhook). It ends in E2E SELFTEST PASS and never in E2E PASS)"
st_15() {
                                    if [ -n "$PY" ]; then
                                      out=$(E2E_SELFTEST=1 bash scripts/e2e.sh 2>&1) || { printf '%s\n' "$out" | sed 's/^/  /'; echo "  the E2E's self-test failed (scripts/e2e.sh, E2E_SELFTEST=1)"; exit 1; }
                                      printf '%s\n' "$out" | sed 's/^/  /'
                                    else echo "  skipped: no python >= 3.8 on PATH (scripts/e2e.sh needs one before it reaches the self-test)"; fi
}
ck_add 16 "[check] no E2E drop-in calls a helper another drop-in defines (§20 erratum 234; scripts/e2e.d/README.md's second convention: a bundle that runs in a LANE runs in a subshell, so the functions it defines go away with it. While every drop-in was sourced into one shell in name order a file could quietly call a helper another file happened to define earlier, and three did - wait_social/prefix_names, wait_trim and probe_queue_idle - which the scheduler turned into a 'command not found' the first time the lanes moved. A helper a second file needs belongs in scripts/e2e.sh. The scan strips comments and QUOTED STRINGS before looking for a call, because without that the word 'member' in an assertion message reads as a call to 55-owner.sh's member(); it then wants the name at a command position - start of line or after ; & | ( or whitespace, and followed by whitespace, ; ) or end of line - so data-bulk and name= are not calls)"
st_16() {
                                    helper_strip() { sed -e 's/^[[:space:]]*#.*$//' -e 's/"[^"]*"//g' -e "s/'[^']*'//g" "$1"; }
                                    # This stage LEANS ON `set -eu` WITHOUT `pipefail` (line 2 sets neither -o pipefail
                                    # nor a shell that has it): `grep -oE` finds no definition in a drop-in that declares
                                    # none, exits 1, and a pipeline's status is its LAST command's — the `sort -u` —
                                    # so the loop simply runs zero times. The `|| true` below says that out loud and
                                    # makes it true whatever the shell: adding pipefail here would otherwise turn a
                                    # drop-in with no helpers of its own into a failed CI run, which is the opposite of
                                    # what this stage is about (§20 erratum 275).
                                    # Strip each file ONCE and ask about each name ONCE (§20 erratum 283). The scan used to
                                    # sed-strip and grep every file again for every helper name it had found — 21 drop-ins × 76
                                    # definitions is ~1,500 sed+grep pairs, and this stage was 46 s of a 433 s run, all of it
                                    # process start. The same strip and the SAME regex now run over pre-stripped copies, one
                                    # `grep -l` per name across all of them at once: ~97 processes instead of ~3,000. What it
                                    # flags is unchanged — same command-position expression, same comment-and-quoted-string
                                    # strip, same message, same order — and the self-test below is what says so.
                                    #
                                    # The stripped copies are named `f.<basename>`, and `map` carries basename → the path the
                                    # caller passed, because the message must name the file the READER knows. Mapping back costs
                                    # one `sed` PER HIT, which on a green run is zero.
                                    helper_scan() { hs_t=$(mktemp -d); : > "$hs_t/map"
                                      for f in "$@"; do helper_strip "$f" > "$hs_t/f.${f##*/}"; printf '%s %s\n' "f.${f##*/}" "$f" >> "$hs_t/map"; done
                                      for def in "$@"; do hs_def="f.${def##*/}"
                                        { grep -oE '^[a-z_][a-z0-9_]*\(\)' "$def" 2>/dev/null || true; } | tr -d '()' | sort -u | while read -r n; do
                                          [ -n "$n" ] || continue
                                          { grep -lE '(^|[;&|(]|[[:space:]])'"$n"'([[:space:]]|;|\)|$)' "$hs_t"/f.* 2>/dev/null || true; } | while read -r hit; do
                                            hit=${hit##*/}; [ "$hit" != "$hs_def" ] || continue
                                            echo "$(sed -n "s|^$hit ||p" "$hs_t/map") calls $n(), which $def defines"
                                          done
                                        done
                                      done; rm -rf "$hs_t"; }
                                    t=$(mktemp -d); printf 'shared() { echo hi; }\nshared\n' > "$t/10-a.sh"
                                    printf 'shared\n' > "$t/20-b.sh"; printf 'echo "the shared bar is hidden"\nx=shared\ndata-shared\n' > "$t/30-c.sh"
                                    n=$(helper_scan "$t"/*.sh | wc -l | tr -d ' '); rm -rf "$t"; [ "$n" = 1 ] || { echo "  self-test failed: the scan must flag 20-b.sh's bare call, ignore 30-c.sh's quoted prose, assignment and hyphenated word, and never flag the defining file itself (got $n hits, want 1)"; exit 1; }
                                    hits=$(helper_scan scripts/e2e.d/*.sh); [ -z "$hits" ] || { printf '%s\n' "$hits"; echo "  the call(s) above cross drop-in files: move the helper into scripts/e2e.sh (scripts/e2e.d/README.md's second convention)"; exit 1; }
}
ck_add 17 "[check] DEPLOY.md's Contents has one entry per numbered section (§20 erratum 234; nit sweep S29: the file's section numbers are stable ids cross-referenced from README, DESIGN and API in dozens of places, and three parallel lanes each adding a section is exactly how an entry gets forgotten. The window is the Contents block up to its first --- rule, which is how the file is laid out today: a deliberate re-layout of that block breaks this stage on purpose)"
st_17() {
                                    toc_heads() { grep -c '^## [0-9]' "$1" || true; }
                                    toc_entries() { awk '/^## Contents/{f=1;next}/^---$/{f=0}f' "$1" | grep -c '^- \[' || true; }
                                    t=$(mktemp -d); printf '## Contents\n\n- [1. a](#a)\n- [2. b](#b)\n\n---\n\n## 1. a\n## 2. b\n## 3. c\n' > "$t/bad.md"
                                    printf '## Contents\n\n- [1. a](#a)\n- [2. b](#b)\n\n---\n\n## 1. a\n## 2. b\n' > "$t/good.md"
                                    [ "$(toc_heads "$t/bad.md")" != "$(toc_entries "$t/bad.md")" ] && [ "$(toc_heads "$t/good.md")" = "$(toc_entries "$t/good.md")" ] || { rm -rf "$t"; echo "  self-test failed: the counts do not tell an out-of-sync Contents from an in-sync one"; exit 1; }
                                    rm -rf "$t"
                                    h=$(toc_heads DEPLOY.md); e=$(toc_entries DEPLOY.md)
                                    [ "$h" = "$e" ] || { echo "  DEPLOY.md has $h numbered sections and $e Contents entries: add the entry in FILE order (the Contents block is deliberately not in numeric order - §18 is last)"; exit 1; }
}
ck_add 19 "[check] every cited erratum and roadmap number resolves to a heading (run 25, lane pub-design; §20 erratum 353: a code comment, a script, DEPLOY.md, API.md or web/STYLE.md cites a decision as '§20 erratum N' or 'ROADMAP #N' and never by a person's name, so those numbers are the one key into the two documents and a number that resolves to nothing is a dead citation. The scan is over cmd/ internal/ web/ scripts/ DEPLOY.md API.md web/STYLE.md; an erratum number must be a numbered entry under DESIGN.md §20 (a line opening 'N. ' after the §20 heading), a roadmap number a '### N.' heading of ROADMAP.md. A range 'A-B' (either dash) is a bundle's allocation and resolves when A is an entry and B is no higher than the newest one, because the numbering note says an allocated number a bundle did not need stays unused. A list after 'errata' is split on commas, slashes and 'and'; a clause letter after the number, '283 (h)', is ignored. The self-test plants a dead erratum and a dead roadmap citation beside two live ones in a scratch tree: exactly the two dead ones must be named and the check must fail, then pass with them removed)"
st_19() {
                                    cite_heads_design() { awk '/^## 20\. /{f=1} f && /^[0-9]+\. /{sub(/\. .*/,""); print}' "$1" | sort -un; }
                                    cite_heads_roadmap() { grep -oE '^### [0-9]+\. ' "$1" | grep -oE '[0-9]+' | sort -un; }
                                    cite_check() { d=$1; r=$2; shift 2; eh=$(cite_heads_design "$d" | tr '\n' ' '); rh=$(cite_heads_roadmap "$r" | tr '\n' ' '); emax=$(printf '%s\n' "$eh" | awk '{ print $NF }'); bad=0; ed=$(printf '\342\200\223')
                                      [ -n "$eh" ] && [ -n "$rh" ] || { echo "  no headings found in $d / $r"; return 1; }
                                      { LC_ALL=C grep -rnoIE --exclude-dir=__pycache__ "errat(um|a) [0-9]+((-|$ed)[0-9]+)?((, | and |/)[0-9]+((-|$ed)[0-9]+)?)*" "$@" 2>/dev/null || [ $? = 1 ]; } | LC_ALL=C sed "s/$ed/-/g" | awk -v heads="$eh" -v max="$emax" '
                                        BEGIN { n = split(heads, a, " "); for (i = 1; i <= n; i++) h[a[i]] = 1 }
                                        { loc = $0; sub(/:errat.*/, "", loc); list = $0; sub(/.*errat(um|a) /, "", list); gsub(/ and /, ",", list); gsub(/\//, ",", list)
                                          n = split(list, t, /, ?/)
                                          for (i = 1; i <= n; i++) { x = t[i]; if (x ~ /-/) { split(x, ab, /-/); if (!(ab[1] in h) || ab[2] + 0 > max + 0) { print loc ": erratum " x " resolves to no §20 entry"; bad = 1 } }
                                                                      else if (!(x in h)) { print loc ": erratum " x " resolves to no §20 entry"; bad = 1 } } }
                                        END { exit bad }' || bad=1
                                      { grep -rnoIE --exclude-dir=__pycache__ 'ROADMAP #[0-9]+' "$@" 2>/dev/null || [ $? = 1 ]; } | awk -v heads="$rh" '
                                        BEGIN { n = split(heads, a, " "); for (i = 1; i <= n; i++) h[a[i]] = 1 }
                                        { loc = $0; sub(/:ROADMAP.*/, "", loc); x = $0; sub(/.*ROADMAP #/, "", x); if (!(x in h)) { print loc ": ROADMAP #" x " resolves to no heading"; bad = 1 } }
                                        END { exit bad }' || bad=1
                                      return $bad; }
                                    t=$(mktemp -d); printf '## 19. x\n\n5. not an entry\n\n## 20. Errata\n\n5. **five**\n6. **six**\n' > "$t/DESIGN.md"; printf '# r\n\n### 7. seven\n### 8. eight\n' > "$t/ROADMAP.md"; mkdir "$t/src"
                                    printf '// §20 erratum 5 and errata 5-6, 6 (c); ROADMAP #7\n// erratum 9 is dead; ROADMAP #10 is dead\n' > "$t/src/a.go"
                                    out=$({ cite_check "$t/DESIGN.md" "$t/ROADMAP.md" "$t/src" && echo "rc=0"; } || echo "rc=$?")
                                    case "$out" in *"erratum 9 resolves"*"ROADMAP #10 resolves"*"rc=1") ;; *) rm -rf "$t"; echo "  self-test failed: the planted dead citations were not both named with a failing status:"; printf '%s\n' "$out"; exit 1;; esac
                                    [ "$(printf '%s\n' "$out" | grep -c resolves)" = 2 ] || { rm -rf "$t"; echo "  self-test failed: a live citation was flagged:"; printf '%s\n' "$out"; exit 1; }
                                    printf '// §20 erratum 5 and errata 5-6, 6 (c); ROADMAP #7\n' > "$t/src/a.go"
                                    cite_check "$t/DESIGN.md" "$t/ROADMAP.md" "$t/src" || { rm -rf "$t"; echo "  self-test failed: the tree with only live citations did not pass"; exit 1; }
                                    rm -rf "$t"
                                    cite_check DESIGN.md ROADMAP.md cmd internal web scripts DEPLOY.md API.md web/STYLE.md || { echo "  dead citation(s) above: every '§20 erratum N' must be an entry of DESIGN.md §20 and every 'ROADMAP #N' a heading of ROADMAP.md (a number is never reused; cite the erratum that owns the subject)"; exit 1; }
}
ck_add 18 "[check] the Go stage's planner, against planted listings (run 9, lane t0: scripts/lib/gocheck.sh --selftest, no container and no Go. It pins what makes the Go stage cheap and what keeps it honest: by default ONE pass and no -race shard; with a race pass, a raced package's untagged tests run only under -race, its tagged-only tests only in the tagged pass, and only a test whose own body reads raceDetector runs in both; the shards are a function of the listing and the committed weights, never of CHECK_JOBS, because a shard's -run expression is part of Go's test-cache key; and a shard's command carries no -count=1, -race exactly on the race shards, and records a (cached) answer)"
st_18() {
                                    sh scripts/lib/gocheck.sh --selftest
}
ck_add pii "[check] no tracked text file carries a personal value or a machine-shaped path (run 25, lane pub-harness: scripts/lib/pii.sh. Its self-test first, PII_SELFTEST=1, against a scratch git repository: one planted hit per half fails by path, line and pattern number and the matched text is never printed, the public half is case-sensitive and the private half is not, go.sum and binaries are not read, the private half is off in one line without its file, the allow list covers one file and one pattern, a clean tree passes, an invalid pattern is exit 2. Then the scan: the PUBLIC shapes of scripts/lib/pii-public.txt and, when PII_DENY_FILE (default ../clippet-internal/pii-deny.txt) exists, the PRIVATE values, over every tracked text file. A hit fails the stage; while this checkout still tracks the private records (HANDOFF.md), the count is reported and the stage passes unless PII_STRICT=1)"
st_pii() {
                                    PII_SELFTEST=1 bash scripts/lib/pii.sh || { echo "  the PII scan's self-test failed (scripts/lib/pii.sh, PII_SELFTEST=1)"; exit 1; }
                                    rc=0; out=$(bash scripts/lib/pii.sh 2>&1) || rc=$?
                                    if [ "$rc" = 0 ]; then printf '%s\n' "$out" | sed 's/^/  /'; exit 0; fi
                                    if [ "$rc" = 1 ] && [ "${PII_STRICT:-0}" != 1 ] && git ls-files --error-unmatch HANDOFF.md >/dev/null 2>&1; then
                                      echo "  NOTE: $(printf '%s\n' "$out" | grep -c '  pattern #') hit(s), not failed: this checkout still tracks the private records (HANDOFF.md), which the public tree leaves out. By top-level path (PII_STRICT=1 fails on them; bash scripts/lib/pii.sh lists each):"
                                      printf '%s\n' "$out" | grep '  pattern #' | cut -d: -f1 | cut -d/ -f1 | sort | uniq -c | sort -rn | sed 's/^/   /'
                                      exit 0
                                    fi
                                    printf '%s\n' "$out" | sed 's/^/  /'; exit 1
}
ck_add sleeplint "[check] no bare time.Sleep in the timing-flake packages' tests (run 9, lanes settle and tiers; §20 erratum 331, rule R1: scripts/lib/sleeplint.sh over the *_test.go files of internal/worker, internal/httpapi, internal/store, internal/notify and internal/sweeper — every time.Sleep( left there carries a '// sleep: <reason>' comment on the SAME line saying why no signal can replace it, so a line this stage prints is a NEW sleep; wait on the settle hook, the fake clock or a channel instead. The self-test plants a bare sleep and a reasoned one in a scratch tree: exactly the bare one must be named and the script must exit 1; a comment that merely names time.Sleep is not a call)"
st_sleeplint() {
                                    t=$(mktemp -d); mkdir -p "$t/internal/worker" "$t/internal/notify"
                                    printf 'package worker\n\nfunc a() {\n\ttime.Sleep(time.Second)\n\ttime.Sleep(10 * time.Millisecond) // sleep: the subject of the test\n\t// time.Sleep(1) is not a call\n}\n' > "$t/internal/worker/a_test.go"
                                    printf 'package notify\n\nfunc b() { time.Sleep(1) } // sleep: a planted reason\n' > "$t/internal/notify/b_test.go"
                                    rc=0; out=$(sh scripts/lib/sleeplint.sh "$t" 2>&1) || rc=$?
                                    n=$(printf '%s\n' "$out" | grep -c '^sleeplint: internal/' || true)
                                    { [ "$rc" = 1 ] && [ "$n" = 1 ] && printf '%s\n' "$out" | grep -q '^sleeplint: internal/worker/a_test.go:4:'; } || { rm -rf "$t"; printf '%s\n' "$out" | sed 's/^/  /'; echo "  self-test failed: the lint must name exactly the bare sleep at internal/worker/a_test.go:4 and exit 1 (got exit $rc, $n line(s))"; exit 1; }
                                    printf 'package worker\n\nfunc a() {\n\ttime.Sleep(time.Second) // sleep: now reasoned\n}\n' > "$t/internal/worker/a_test.go"
                                    sh scripts/lib/sleeplint.sh "$t" >/dev/null 2>&1 || { rm -rf "$t"; echo "  self-test failed: a tree whose every sleep has a reason must pass"; exit 1; }
                                    rm -rf "$t"
                                    sh scripts/lib/sleeplint.sh .
}
ck_add t1 "[check] T1's decisions, against planted inputs (run 9, lane tiers; §20 erratum 331: T1_SELFTEST=1 bash scripts/t1.sh, no Docker, no git, no stack. It pins what keeps T1 off the stacks that are not a lane's — the compose projects clippet, clippet-dev and clippet-gate refused, a PUBLIC_BASE_URL that is not a lane's own loopback port refused, a lane .env whose COMPOSE_PROJECT_NAME is not clippet-<its own folder> refused (the line lane-teardown.sh keys on, lane nits22) — and what sizes it to the diff: the race set is the changed packages that own goroutines and nothing else, the windows cross-compile runs on a cmd/ or _windows.go change, a page maps to its visual phases and a shared file to all of them, and a T2 stop names the shape path)"
st_t1() {
                                    T1_SELFTEST=1 bash scripts/t1.sh
}
ck_add gatestack "[check] the gate's own stack and the public smoke, against scratch directories and a stub server (run 9, lane gatestack: scripts/lib/gatestack.sh selftest — the .env key-diff REFUSES a key the reference sets and the gate does not, and an undeclared key the gate sets and the reference does not, each by NAME and with no value in the transcript; the visual pass is SKIPPED on a docs-only diff since the last green gate and RUN on a whitespace change under web/, committed or not, on GATE_RELEASE=1 and when no baseline is known — and scripts/smoke-public.sh's SMOKE_SELFTEST=1: GET /?mine=1 answered 301 by a stub server FAILS by name, the filtered library at 200 passes. No daemon, no stack; loopback only)"
st_gatestack() {
                                    bash scripts/lib/gatestack.sh selftest
                                    SMOKE_SELFTEST=1 bash scripts/smoke-public.sh
}
ck_add wslexec "[check] the harness's WSL re-run, against scratch repositories (DESIGN §20 erratum 338: bash scripts/lib/wslexec.sh selftest, no wsl.exe and no Docker. It pins when gate.sh/t0.sh/t1.sh re-run inside WSL and when they stay — Git Bash only, never inside WSL, never under CLIPPET_WSL=0 —, that WSLENV carries the harness knobs by name and nothing else, the run directory, that the reference .env is read in place (GATE_REF_ENV) and never copied while the clone's .env is a guard (project clippet-never, mode 600), that a lane .env is copied for a lane project only and no value is printed, the same-commit rule — the mirror fetch makes WSL's main the Windows main, a commit WSL cannot see and a dirty clone are refused —, that the copy-back is silent and carries only the record this run wrote, and that gate.sh refuses its own/foreign modes inside WSL)"
st_wslexec() {
                                    bash scripts/lib/wslexec.sh selftest
}
ck_add boxtidy "[check] the box tidy's allow-list, against a stub docker and scratch git repos (lane boxtidy, 2026-10-18: sh scripts/box-tidy.sh --selftest, no daemon. It pins what the report lists and what apply removes — exactly the anonymous dangling volumes, the lane images with no worktree here or in GATE_WIN_ROOT, the harness's per-run tags and stopped probes older than BOX_TIDY_AGE, and the build cache unused for 168 h (aged by LastUsedAt: buildx du ignores --filter until; an estimate outside the closing total, and apply's removed line counts the prune's OWN reclaimed total, lane nits22) — against one planted item of every excluded kind: named volumes, the app/dev/gate/never/pass images, an image or tag in use, a fresh tag, the long-lived harness images, compose service containers, running and paused probes, every non-clippet name; that apply re-checks every item before the first removal and refuses with nothing removed, that an empty list and an empty or foreign selector remove nothing, that no -a, --volumes or system prune is ever called, and that a box with no Docker or no worktree list reports and skips instead of guessing)"
st_boxtidy() {
                                    sh scripts/box-tidy.sh --selftest
}
ck_add go "[check] the Go stage: gofmt, go mod verify, go vet (with and without the ffmpeg tag), the tests (ONE -tags ffmpeg pass by default; a -race pass only with CHECK_RACE), the windows/amd64 cross-compile and the runtime image (§20 errata 41, 283, 308; lanes speed2 and t0). The tests include the medal-#7 unfurl regression gate — internal/web TestUnfurlRegression: full §9.1 tag inventory with absolute URLs, secure_url iff https, link-out has no player tags; internal/httpapi TestUnfurlRegressionSocialMP4: social.mp4 answers 200 and 206 — and the §14 rate-limit gates: internal/ratelimit unit tests plus TestAuthRateLimit/TestUploadRateLimit/TestViewRateLimit wiring. By default there is ONE test pass, -tags ffmpeg over ./..., which runs every test the module has (run 9, lane t0: -race made the pass 4-5x slower and was where 3 of the last 4 Go failures were); CHECK_RACE=1|all|<packages> adds the -race pass, and then the two are COMPLEMENTARY, never doubled: a raced package's untagged tests run only under -race and only its tagged-only tests in the tagged pass, except the few whose own body reads raceDetector, which run in both. The cross-compile is there because the binary is also something a person runs on WINDOWS (clippet.exe admin doctor) and three files exist only for that target, which go vet and go test on this GOOS never compile. HOW it runs since lane speed2: the Dockerfile's testsrc stage is built (the source copy, gofmt, go mod verify — the parent the old test/racetest/crosswindows targets shared), and ONE container of it runs scripts/lib/gocheck.sh, which vets and cross-compiles while it LISTS the passes' tests, then cuts every package into shards of whole tests and runs CHECK_JOBS shards at a time, longest first — no test in this module calls t.Parallel, so the -race pass used to be internal/httpapi's ~220 s alone however many cores there were. Every planned test runs exactly once per pass and the partition is checked before anything runs. The container mounts the named volume clippet-gobuild as Go's build and test cache and runs nothing with -count=1, so an unchanged package answers (cached); the shards are fixed by the tree and scripts/lib/go-weights.txt, never by CHECK_JOBS, so they hit that cache. The runtime image builds beside it. THE CHECKPOINT: a GREEN stage writes .e2e/checkpoints/check-<hash>.txt, the hash being scripts/lib/gotree.sh's sha256 over the content of internal/ cmd/ web/ go.mod go.sum Dockerfile .dockerignore and gocheck.sh itself; when the tree hashes to a checkpoint that exists AND whose race pass covers this run's CHECK_RACE (all covers every request, none only none), the stage is SKIPPED and says so with the hash and the time it was proved. CHECK_FULL=1 runs it anyway"
st_go() {
  . scripts/lib/gotree.sh
  # The runtime image's TAG is the one compose would run (compose.yaml: image: ${CLIPPET_IMAGE:-clippet-app:local}):
  # the shell's CLIPPET_IMAGE, else this checkout's .env line, else the default. A lane worktree's .env names
  # its own `clippet-<lane>-app:local`, so a lane's check.sh never re-tags the reference checkout's image with
  # the lane's tree (run 8, lane harness9). Only the one line is read from .env, and nothing of it is printed
  # but the tag.
  ck_img=${CLIPPET_IMAGE:-$(sed -n 's/^CLIPPET_IMAGE=//p' .env 2>/dev/null | tail -1 | tr -d '\r"' || true)}
  ck_img=${ck_img:-clippet-app:local}
  ck_h=$(gotree_hash_ .)
  # The lint's configuration is part of what the stage proves: a changed .golangci.yml moves the hash too.
  if [ -n "$ck_h" ] && [ -f .golangci.yml ]; then
    ck_h=$(printf '%s %s\n' "$ck_h" "$(sha256sum < .golangci.yml | cut -c1-64)" | sha256sum | cut -c1-16)
  fi
  ck_ckpt=".e2e/checkpoints/check-$ck_h.txt"
  # The race pass a run asks for, spelled one way (none | 1 | all | the sorted package list), and the one a
  # checkpoint proved (its race= line; a checkpoint written before lane t0 has none, and proves only none).
  # all covers every request, none is covered by every checkpoint, anything else only by itself.
  ck_race_() { case ${1:-} in ''|0|none) echo none ;; 1|all) echo "$1" ;; *) for ck_w in $1; do echo "$ck_w"; done | LC_ALL=C sort -u | paste -sd' ' - ;; esac; }
  ck_covers_() { [ "$2" = none ] || [ "$1" = all ] || [ "$1" = "$2" ]; }
  ck_want=$(ck_race_ "$CHECK_RACE")
  ck_have=$(ck_race_ "$(sed -n 's/^race=//p' "$ck_ckpt" 2>/dev/null)")
  if [ -n "$ck_h" ] && [ "${CHECK_FULL:-0}" != 1 ] && [ -s "$ck_ckpt" ] && ck_covers_ "$ck_have" "$ck_want"; then
    echo "  skipped: Go tree unchanged since $(sed -n 's/^proved=//p' "$ck_ckpt") (hash $ck_h, proved on $(sed -n 's/^commit=//p' "$ck_ckpt") with race=$ck_have, which covers race=$ck_want: $(sed -n 's/^verdict=//p' "$ck_ckpt")); the runtime image build ($ck_img) is skipped with it — CHECK_FULL=1 runs both"
    return 0
  fi
  if [ -z "$ck_h" ]; then echo "  (not a git checkout: no checkpoint is read or written; the stage runs)"
  elif [ "${CHECK_FULL:-0}" = 1 ]; then echo "  CHECK_FULL=1: the stage runs whatever the checkpoint says (hash $ck_h)"
  elif [ -s "$ck_ckpt" ]; then echo "  the checkpoint for this Go tree (hash $ck_h) proved race=$ck_have, which does not cover race=$ck_want: the stage runs"
  else echo "  no checkpoint for this Go tree (hash $ck_h): the stage runs"; fi
  ck_go_rc=0
  # The runtime image builds BESIDE the tests: it needs nothing they produce.
  docker build -t "$ck_img" . > "$CK_DIR/runtime-image.log" 2>&1 & ck_rt=$!
  # testsrc is the shared parent of the three old targets: the source copy, gofmt and go mod verify.
  # Tagged so a container can run it, and the tag is removed below whatever happens (the layers stay in
  # the build cache, which is what makes the next build of an unchanged tree a cache hit). The tag is this
  # run's own (run 9: six lane builders share the box, and one lane's rmi of a shared tag could pull the
  # image from under another's docker run).
  ck_src="clippet-check-src:$$-$(date +%s)"
  # Go's build and test cache: the named volume the container test loop uses, created if absent and never removed.
  docker volume inspect clippet-gobuild >/dev/null 2>&1 || docker volume create clippet-gobuild >/dev/null
  if ! docker build --target testsrc -t "$ck_src" . > "$CK_DIR/testsrc.log" 2>&1; then
    echo "  ---- --target testsrc (the source copy, gofmt, go mod verify) FAILED ----"
    sed 's/^/  /' "$CK_DIR/testsrc.log"
    ck_go_rc=1
  else
    echo "  testsrc built: gofmt clean, go mod verify ok"
    ck_host=$( (pwd -W) 2>/dev/null || pwd )
    cp .e2e/checkpoints/go-times.txt "$CK_DIR/go-times.prev" 2>/dev/null || : > "$CK_DIR/go-times.prev"
    # On Linux (the harness on WSL, erratum 338) the container chowns /out back to this user at its exit.
    ck_chown=; case $(uname -s) in Linux) ck_chown="$(id -u):$(id -g)" ;; esac
    # golangci-lint, BESIDE the tests: one more stage on top of testsrc, built from the Dockerfile with that
    # stage appended, through the image the `test` stage's RUN line pins (read from it, so there is one pin).
    # The modules are already in testsrc, so it needs no network once the image is pulled; BuildKit keeps its
    # cache between runs, and a tree that already linted clean answers from the layer cache.
    ck_lint_img=$(sed -n 's/.*--mount=from=\(golangci\/golangci-lint:[^,]*\),.*/\1/p' Dockerfile | head -1)
    { cat Dockerfile
      printf '\nFROM testsrc AS clippet-lint\nRUN --mount=from=%s,source=/usr/bin/golangci-lint,target=/usr/local/bin/golangci-lint --mount=type=cache,id=clippet-golangci,target=/root/.cache golangci-lint run ./...\n' "$ck_lint_img"
    } | docker build --progress=plain --target clippet-lint -f - . > "$CK_DIR/lint.log" 2>&1 & ck_lint=$!
    # govulncheck, beside them too: the pinned tool installed into the container, then run over the tree.
    # Both steps need the network (the module proxy, the vulnerability database); without it the stage
    # prints one notice and goes on. Exit 3 is a vulnerability the code CALLS, and that fails the stage.
    CK_VULN_PIN=golang.org/x/vuln/cmd/govulncheck@v1.9.0
    docker run --rm --name "clippet-check-vuln-$$" -v clippet-gobuild:/root/.cache/go-build "$ck_src" sh -c \
      "GOBIN=/tmp/vb go install $CK_VULN_PIN || exit 0; /tmp/vb/govulncheck -tags ffmpeg ./...; echo VULN_RC=\$?" \
      > "$CK_DIR/govulncheck.log" 2>&1 & ck_vuln=$!
    docker run --rm --name "clippet-check-go-$$" -e CHECK_JOBS -e CHECK_RACE -e CHECK_GOMAXPROCS -e GOCHECK_OUT=/out -e "GOCHECK_CHOWN=$ck_chown" \
      -v "$ck_host/$CK_DIR:/out" -v "$ck_host/scripts/lib/gocheck.sh:/check/gocheck.sh:ro" \
      -v "$ck_host/scripts/lib/go-weights.txt:/check/go-weights.txt:ro" -v clippet-gobuild:/root/.cache/go-build \
      "$ck_src" sh /check/gocheck.sh > "$CK_DIR/gocheck.log" 2>&1 || ck_go_rc=1
    sed 's/^/  /' "$CK_DIR/gocheck.log"
    [ ! -s "$CK_DIR/go-times.txt" ] || cp "$CK_DIR/go-times.txt" .e2e/checkpoints/go-times.txt
    ck_lint_rc=0; wait "$ck_lint" || ck_lint_rc=$?
    if [ "$ck_lint_rc" != 0 ]; then
      echo "  ---- golangci-lint ($ck_lint_img) FAILED ----"
      sed -n 's/^#[0-9]* [0-9.]* //p' "$CK_DIR/lint.log" | grep -v '^\(sha256\|resolve\|DONE\|CACHED\|transferring\)' | tail -60 | sed 's/^/  /'
      ck_go_rc=1
    else
      echo "  golangci-lint clean (.golangci.yml; $ck_lint_img)"
    fi
    wait "$ck_vuln" || true
    ck_vrc=$(sed -n 's/^VULN_RC=//p' "$CK_DIR/govulncheck.log" | tail -1)
    case $ck_vrc in
      0) echo "  govulncheck: no called vulnerability ($CK_VULN_PIN, -tags ffmpeg)" ;;
      3) echo "  ---- govulncheck: the code CALLS a known vulnerability ----"
         grep -v '^VULN_RC=' "$CK_DIR/govulncheck.log" | sed 's/^/  /'
         ck_go_rc=1 ;;
      *) echo "  notice: govulncheck did not run (no network, or the vulnerability database did not answer); $CK_DIR/govulncheck.log says why — not a failure" ;;
    esac
  fi
  docker rmi "$ck_src" >/dev/null 2>&1 || true
  ck_rt_rc=0; wait "$ck_rt" || ck_rt_rc=$?
  if [ "$ck_rt_rc" != 0 ]; then
    echo "  ---- the runtime image (docker build -t $ck_img .) FAILED ----"
    sed 's/^/  /' "$CK_DIR/runtime-image.log"
    ck_go_rc=1
  else
    echo "  runtime image $ck_img built"
  fi
  if [ "$ck_go_rc" != 0 ]; then echo "  the Go stage failed (every log is above; the shards' own logs are in $CK_DIR/go/)"; exit 1; fi
  if [ -n "$ck_h" ] && ck_covers_ "$ck_have" "$ck_want" && ! ck_covers_ "$ck_want" "$ck_have"; then
    echo "  checkpoint kept: $ck_ckpt proved race=$ck_have, more than this run's race=$ck_want"
  elif [ -n "$ck_h" ]; then
    { echo "hash=$ck_h"; echo "race=$ck_want"; echo "proved=$(date -u +%Y-%m-%dT%H:%M:%SZ)"; echo "commit=$(git rev-parse --short HEAD 2>/dev/null || echo none)$(git status --porcelain -- $GOTREE_PATHS 2>/dev/null | grep -q . && echo '+local changes')"
      echo "verdict=$(grep '^GOCHECK OK' "$CK_DIR/gocheck.log" | tail -1)"; } > "$ck_ckpt"
    echo "  checkpoint written: $ck_ckpt"
  fi
}


# ---- run every stage at once, then print them in order -------------------------------------------------
# Each stage runs in a subshell of its own with `set -e` IN FORCE (a function called on the left of `||`
# would run with errexit silently off, so the verdict is read from `$?` on the next line instead), its
# output in .e2e/check/stages/<id>.log and its exit status and seconds in <id>.rc.
# CHECK_ONLY narrows the list (run 9, lane t0): a word that names no stage is refused rather than ignored,
# so a typo cannot turn into a check that silently ran nothing.
ck_ids=$(cat "$CK_DIR/stages/order"); ck_all=$(echo $ck_ids | wc -w | tr -d ' ')
if [ -n "$CHECK_ONLY" ]; then
  for ck_w in $CHECK_ONLY; do
    case " cheap $(echo $ck_ids) " in *" $ck_w "*) ;; *) echo "[check] CHECK_ONLY names '$ck_w', which is no stage (the stages: cheap $(echo $ck_ids))"; exit 2 ;; esac
  done
  ck_sel=""
  for ck_id in $ck_ids; do
    case " $CHECK_ONLY " in *" $ck_id "*) ck_sel="$ck_sel $ck_id" ;; *" cheap "*) [ "$ck_id" = go ] || ck_sel="$ck_sel $ck_id" ;; esac
  done
  ck_ids=$ck_sel
fi
for ck_id in $ck_ids; do
  (
    set +e
    ck_s=$(date +%s)
    ( set -e; "st_$ck_id" ) > "$CK_DIR/stages/$ck_id.log" 2>&1
    printf '%s %s\n' "$?" "$(( $(date +%s) - ck_s ))" > "$CK_DIR/stages/$ck_id.rc"
  ) &
done
wait
ck_failed=""
for ck_id in $ck_ids; do
  cat "$CK_DIR/stages/$ck_id.title"
  [ ! -s "$CK_DIR/stages/$ck_id.log" ] || cat "$CK_DIR/stages/$ck_id.log"
  set -- $(cat "$CK_DIR/stages/$ck_id.rc" 2>/dev/null || echo "99 0")
  if [ "$1" = 0 ]; then echo "  ok — took $2 s"; else echo "  FAILED (exit $1) — took $2 s"; ck_failed="$ck_failed $ck_id"; fi
done
CK_NOW=$(date +%s)
if [ -n "$ck_failed" ]; then
  echo "[check] FAIL ($((CK_NOW - CK_T0)) s total, every stage at once): failed stage(s):$ck_failed — each one's output is printed under its title above"
  exit 1
fi
ck_ran=$(echo $ck_ids | wc -w | tr -d ' ')
if [ "$ck_ran" != "$ck_all" ]; then echo "[check] OK ($((CK_NOW - CK_T0)) s total, every stage at once; CHECK_ONLY: $ck_ran of $ck_all stages ran)"; exit 0; fi
echo "[check] OK ($((CK_NOW - CK_T0)) s total, every stage at once)"
