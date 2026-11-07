# gotree.sh — the Go-tree checkpoint's hash (lane `speed2`), SOURCED by scripts/check.sh and by
# scripts/e2e.sh's self-test, which pins it on a fixed tree. POSIX sh; no side effects.
#
# What the Go stage of check.sh proves (gofmt, go mod verify, vet with and without the ffmpeg tag, the
# unit + ffmpeg tests, the -race pass, the windows cross-compile, the runtime image) is a function of
# exactly what `docker build` copies into the image: the Go source AND everything it embeds. The brief
# named `internal/ cmd/ go.mod go.sum Dockerfile`; `web/` and `.dockerignore` are here as well, because
# `web/static` and `web/templates` are compiled INTO the binary (their embed.go files) and
# internal/web's tests render those templates — a template edit that skipped the Go stage would skip
# the tests that read it — and `.dockerignore` decides what the build context holds at all. And
# `scripts/lib/gocheck.sh`, which is not in the image but IS the stage: a new check added to it must
# not be skipped on the strength of a tree it never looked at.
#
# The hash is over CONTENT, not over the index: every tracked file under those paths that exists in
# the working tree, plus every UNTRACKED file there that git does not ignore (a new .go file is in the
# build context the moment it is saved, committed or not). A deleted tracked file drops out of the
# list, which moves the hash. Each line is `<sha256> <path>`; the listing is hashed again and cut to 16
# hex characters. Outside a git checkout, or over an empty list, it prints NOTHING, and an empty hash
# never matches a checkpoint — the stage then simply runs.
GOTREE_PATHS="internal cmd web go.mod go.sum Dockerfile .dockerignore scripts/lib/gocheck.sh"
gotree_hash_() {   # gotree_hash_ <repo root> → 16 hex characters, or nothing
  (
    cd "$1" 2>/dev/null || exit 0
    git rev-parse --is-inside-work-tree >/dev/null 2>&1 || exit 0
    # shellcheck disable=SC2086 # GOTREE_PATHS is a deliberate word list
    list=$(git ls-files -co --exclude-standard -- $GOTREE_PATHS 2>/dev/null \
      | while IFS= read -r f; do [ -f "$f" ] && printf '%s\n' "$f"; done | LC_ALL=C sort)
    [ -n "$list" ] || exit 0
    printf '%s\n' "$list" | tr '\n' '\0' \
      | xargs -0 sha256sum 2>/dev/null \
      | sed 's/^\([0-9a-f]*\) [ *]/\1 /' \
      | sha256sum | cut -c1-16
  )
}
