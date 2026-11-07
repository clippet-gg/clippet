# 97-hook.sh — the clip webhook (ROADMAP #54, §20 erratum 319): one signed JSON document per new clip,
# POSTed to the one URL the operator names in CLIP_WEBHOOK_URL. Off by default.
# Sourced by scripts/e2e.sh before the step-16 log gate (see scripts/e2e.d/README.md).
# Step 97.
#
# e2e-schedule: parallel
# e2e-namespace: account=hk prefix=HK_ buckets=-
# e2e-covers: internal/notify/hook.go internal/worker/hook.go internal/config/clipwebhook.go
#   (a) reads the STACK — the hook jobs of THIS run's account's clips (`admin ls --owner`, `admin jobs
#   --kind hook`), which nothing in the suite can create while the stack's own CLIP_WEBHOOK_URL is empty
#   (no `hook` job is ever enqueued then), so another bundle cannot disturb it; with the variable set it
#   SKIPs, and an exec that cannot read the variable fails. (b)-(e) run in a throwaway app
#   container of their OWN: its own DATA_DIR, SCRATCH_DIR and STORAGE_DIR (STORAGE_BACKEND=fs, 88-fsstore's
#   shape, so not one object reaches the stack's store), its own receiver on its own loopback, NO
#   published port (everything is `docker exec`), named from the lane port and removed by a trap armed
#   before it exists. Nothing in the stack's database, store or log is touched.
#
# The receiver is busybox `nc -lk … -e` — an applet the runtime image already ships (alpine's busybox;
# `httpd` is in busybox-extras and is NOT there). No package is added to the product image for a test.
# The GPU flavour's image is Ubuntu and ships no `nc` (the twenty-fourth gate's first attempt died on
# exactly that, on the reference stack): there the same handler runs under a few lines of perl, whose
# core socket module that image does carry — one connection at a time, the handler on the socket.
# It answers 500 to its FIRST request and 200 after, saves each request's header lines and EXACTLY
# Content-Length bytes of body, so the host can recompute each signature over the true body.
#
# Variables are prefixed HK_ so nothing here can clobber a harness global.

# ---------------------------------------------------------------------------------------------------
step 97 "a webhook on every new clip (#54): signed, retried once, unlisted included; nothing queued when unset"

# --- (a) the stack: unset means nothing is queued --------------------------------------------------
# The test is made INSIDE the container and prints nothing of the value: an operator who turned the
# feature on must not fail the gate, and the value is a credential that must never reach this log. The
# probe prints one of two WORDS, so an exec that fails for any other reason (the app container down) is
# a failure of the step and never the SKIP.
# The count is THIS RUN'S: the hook jobs of the clips the run's own account owns (every core clip is
# ready by now, so each would have had its job queued in the same commit had the variable been set) —
# never `admin stats`' whole-database `hook=`, which a stack where the webhook was once on keeps until
# those clips are deleted (step 20's notify=0 NOTE, the same trap).
HK_SET=$(dc exec -T app sh -c 'if [ -z "$CLIP_WEBHOOK_URL" ]; then echo unset; else echo set; fi' 2>&1 | tr -d '\r') || true
case $HK_SET in
  unset)
    HK_MINE=$(dc exec -T app clippet admin ls --owner "$E2E_USER" 2>&1 | tr -d '\r') || die "admin ls --owner $E2E_USER failed: $HK_MINE"
    HK_MINE=$(printf '%s\n' "$HK_MINE" | awk 'NR > 1 && NF >= 4 && $1 ~ /^[A-Za-z0-9_-]+$/ { print $1 }')
    [ -n "$HK_MINE" ] || die "the run's account $E2E_USER owns no clip to count hook jobs for (the core makes its clips before the pool)"
    HK_JOBS=$(dc exec -T app clippet admin jobs --kind hook --limit 100000 2>&1 | tr -d '\r') || die "admin jobs --kind hook failed: $HK_JOBS"
    # shellcheck disable=SC2086
    HK_HIT=$(printf '%s\n' "$HK_JOBS" | awk -v ids="$(printf '%s ' $HK_MINE)" \
      'BEGIN { n = split(ids, a, " "); for (i = 1; i <= n; i++) mine["clip=" a[i]] = 1 } ($2 in mine) { print }')
    [ -z "$HK_HIT" ] || die "hook jobs exist for this run's clips although the stack's CLIP_WEBHOOK_URL is empty:
$HK_HIT"
    echo "[e2e]   the stack: CLIP_WEBHOOK_URL empty and no hook job for any of the $(printf '%s\n' "$HK_MINE" | grep -c .) clips of this run's account"
    ;;
  set) echo "[e2e]   SKIP (CLIP_WEBHOOK_URL is set on this stack): the no-hook-job half is for a stack with the webhook off" ;;
  *) die "could not read whether the stack's CLIP_WEBHOOK_URL is set (dc exec app): $HK_SET" ;;
esac

# --- (b) a throwaway instance with the webhook on, and its receiver --------------------------------
HK_PORT=$(envfile CLIPPET_PUBLISH); HK_PORT=${HK_PORT##*:}; HK_PORT=${HK_PORT:-8481}
HK_CT="clippet-e2e-hook-$HK_PORT"
HK_RX=9797                                                   # the receiver, on the container's own loopback
HK_QTOK=$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')          # the URL's query token: 24 hex, a credential
HK_SECRET=$(od -An -N20 -tx1 /dev/urandom | tr -d ' \n')     # the signing key: 40 hex
HK_URL="http://127.0.0.1:$HK_RX/in?k=$HK_QTOK"
HK_USER="e2e_$(date +%s)_hk"
HK_DIR="$E2E_DIR/hookrx"
HK_OUT="$E2E_DIR/hook.txt"
rm -rf "$HK_DIR"

hk_stop() {
  docker_ rm -f "$HK_CT" >/dev/null 2>&1 || true
}
hk_stop
# Armed BEFORE the container exists: a failure anywhere below still removes it, and with it the
# receiver, the clips, the database and the media — everything this step makes lives inside it.
trap 'hk_stop' EXIT

# hk_sh <shell-line…> — a command inside the container with the stack's bucket, delivery and Discord
# variables unset (88-fsstore's fs_sh, for the same reason: `docker exec` inherits what compose
# interpolated for every container of this project). The two hook variables are the container's OWN.
hk_sh() { docker_ exec -i "$HK_CT" sh -c "unset S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT DISCORD_WEBHOOK_URL DISCORD_WEBHOOK_ROUTES DISCORD_APP_ID DISCORD_PUBLIC_KEY DISCORD_BOT_TOKEN DISCORD_LOGIN_CLIENT_ID DISCORD_LOGIN_CLIENT_SECRET; $*"; }

# Both hook variables are set EXPLICITLY on the container and never inherited from the stack.
dc run -d --name "$HK_CT" --no-deps \
  -e PUBLIC_BASE_URL=http://127.0.0.1:8080 -e REGISTRATION=closed -e LOG_FORMAT=text \
  -e DATA_DIR=/tmp/hkdata -e SCRATCH_DIR=/tmp/hkscratch -e STORAGE_DIR=/tmp/hkmedia \
  -e STORAGE_BACKEND=fs -e CAPTURE_READY_WAIT=60s \
  -e "CLIP_WEBHOOK_URL=$HK_URL" -e "CLIP_WEBHOOK_SECRET=$HK_SECRET" \
  --entrypoint sh app -c '
    set -e
    unset S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY
    unset MEDIA_DELIVERY MEDIA_PRESIGN_TTL MEDIA_PRESIGN_ENDPOINT
    unset DISCORD_WEBHOOK_URL DISCORD_WEBHOOK_ROUTES DISCORD_APP_ID DISCORD_PUBLIC_KEY DISCORD_BOT_TOKEN
    unset DISCORD_LOGIN_CLIENT_ID DISCORD_LOGIN_CLIENT_SECRET
    exec clippet serve
  ' >/dev/null || die "could not start the hook container"

# The receiver: one handler per connection. `read` takes the header lines a byte at a time, so the
# body is still in the pipe for `head -c <Content-Length>`.
cat > "$E2E_DIR/hook-rx.sh" <<'HOOK_RX'
#!/bin/sh
RX=/tmp/hookrx
n=$(cat "$RX/count" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > "$RX/count"
len=0
: > "$RX/req-$n.hdr"
while IFS= read -r line; do
  line=$(printf '%s' "$line" | tr -d '\r')
  [ -z "$line" ] && break
  printf '%s\n' "$line" >> "$RX/req-$n.hdr"
  case $line in [Cc]ontent-[Ll]ength:*) len=$(printf '%s' "${line#*:}" | tr -d ' ') ;; esac
done
head -c "$len" > "$RX/req-$n.body"
if [ "$n" = 1 ]; then st="500 Internal Server Error"; else st="200 OK"; fi
printf 'HTTP/1.1 %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n' "$st"
HOOK_RX
docker_ exec -i "$HK_CT" sh -c 'mkdir -p /tmp/hookrx && cat > /tmp/hookrx/h.sh && chmod +x /tmp/hookrx/h.sh' < "$E2E_DIR/hook-rx.sh" \
  || die "could not install the receiver"
if docker_ exec -i "$HK_CT" sh -c 'command -v nc >/dev/null 2>&1'; then
  docker_ exec -d "$HK_CT" nc -lk -s 127.0.0.1 -p "$HK_RX" -e /tmp/hookrx/h.sh || die "could not start the receiver"
elif docker_ exec -i "$HK_CT" sh -c 'perl -MIO::Socket::INET -e 1 >/dev/null 2>&1'; then
  docker_ exec -d "$HK_CT" perl -MIO::Socket::INET -e '
    my $s = IO::Socket::INET->new(Listen => 5, LocalAddr => "127.0.0.1", LocalPort => $ARGV[0], ReuseAddr => 1) or die "listen: $!";
    while (my $c = $s->accept) {
      my $pid = fork;
      if (defined $pid && $pid == 0) { open(STDIN, "<&", $c); open(STDOUT, ">&", $c); exec $ARGV[1]; exit 127; }
      close $c; waitpid($pid, 0) if defined $pid;
    }' "$HK_RX" /tmp/hookrx/h.sh || die "could not start the receiver (perl)"
else
  die "the app image has neither nc nor perl's socket module: step 97 has no receiver to run in it"
fi

HK_N=0
while [ "$HK_N" -lt 90 ]; do
  docker_ exec "$HK_CT" curl -fsS -o /dev/null http://127.0.0.1:8080/readyz >/dev/null 2>&1 && break
  HK_N=$((HK_N + 1)); sleep 1
done
[ "$HK_N" -lt 90 ] || die "the hook container never became ready:
$(docker_ logs --tail 40 "$HK_CT" 2>&1)"
echo "[e2e]   hook instance up after ${HK_N}s, its receiver on its own loopback :$HK_RX"

# --- (c) two clips — one listed, one unlisted — through a minted capture token ----------------------
printf '%s' "$E2E_PASS" | hk_sh clippet admin users create "$HK_USER" > "$HK_OUT" 2>&1 \
  || die "admin users create failed: $(cat "$HK_OUT")"
HK_TOK=$(hk_sh clippet admin tokens mint "$HK_USER" 2>/dev/null | tr -d '\r') || die "admin tokens mint failed"
case $HK_TOK in ck_*) ;; *) die "the mint did not print a token" ;; esac
hk_sh 'ffmpeg -v error -y -f lavfi -i testsrc2=size=320x180:rate=15:duration=2 -f lavfi -i sine=frequency=440:duration=2 -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac -shortest /tmp/hk.mp4' \
  || die "could not make the test clip"
# hk_up <title> <unlisted> → the reply's clip id. The capture route waits for the card, so the clip is
# ready (and its hook job queued, in the same commit) when this returns.
hk_up() {
  docker_ exec "$HK_CT" curl -sS -H "Authorization: Bearer $HK_TOK" -F "title=$1" -F 'ttl_days=1' \
    -F "unlisted=$2" -F duplicate=allow -F 'file=@/tmp/hk.mp4;type=video/mp4' http://127.0.0.1:8080/api/upload \
    | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1
}
HK_L=$(hk_up 'E2E hook listed' false); expect_nonempty "$HK_L" "the listed clip's id"
# The listed clip's first delivery is answered 500 BEFORE the second clip exists, so the request order
# is fixed: 1 = listed (500), 2 = unlisted (200), 3 = the listed retry (200, ~30 s later).
HK_N=0
until docker_ exec "$HK_CT" test -s /tmp/hookrx/req-1.body 2>/dev/null; do
  HK_N=$((HK_N + 1)); [ "$HK_N" -lt 30 ] || die "the receiver never got the listed clip's delivery:
$(docker_ logs --tail 30 "$HK_CT" 2>&1 | grep -v ' msg=http ')"
  sleep 1
done
HK_U=$(hk_up 'E2E hook unlisted' true); expect_nonempty "$HK_U" "the unlisted clip's id"

# Both jobs done: the unlisted one at once, the listed one after its 30 s backoff.
HK_N=0
while :; do
  hk_sh clippet admin jobs --kind hook > "$HK_OUT" 2>&1 || die "admin jobs --kind hook failed: $(cat "$HK_OUT")"
  [ "$(grep -c 'kind=hook *state=done' "$HK_OUT")" = 2 ] && break
  HK_N=$((HK_N + 1)); [ "$HK_N" -lt 120 ] || die "the two hook jobs did not finish within 120 s:
$(cat "$HK_OUT")"
  sleep 1
done
sed -n 's/^/[e2e]   /p' "$HK_OUT" | grep 'kind=hook' || true
grep -q "clip=$HK_L .*state=done *stage=delivered *attempts=1 " "$HK_OUT" \
  || die "the listed clip's job is not delivered after exactly one failed attempt: $(grep "clip=$HK_L" "$HK_OUT")"
grep -q "clip=$HK_U .*state=done *stage=delivered *attempts=0 " "$HK_OUT" \
  || die "the unlisted clip's job is not delivered on its first attempt: $(grep "clip=$HK_U" "$HK_OUT")"
echo "[e2e]   two deliveries: the listed clip after one 500 and its 30 s backoff, the unlisted clip at once"

# --- (d) what arrived, verified on the host ---------------------------------------------------------
docker_ cp "$HK_CT:/tmp/hookrx" "$(W "$HK_DIR")" >/dev/null || die "could not copy the receiver's files out"
expect "$(tr -d '\r\n' < "$HK_DIR/count")" 3 "the receiver's request count (500, then two 200s)"
# One Python pass over the three requests: the signature recomputed over the exact bytes received with
# the key, a wrong key refused, the field set and its values, and nothing that must never be sent.
HK_SECRET="$HK_SECRET" HK_QTOK="$HK_QTOK" HK_TOK="$HK_TOK" HK_L="$HK_L" HK_U="$HK_U" HK_USER="$HK_USER" \
$PY - "$(W "$HK_DIR")" <<'HOOK_CHECK' || die "the deliveries did not verify (above)"
import hashlib, hmac, json, os, re, sys
d = sys.argv[1]
key = os.environ["HK_SECRET"].encode()
want_keys = ["event", "id", "url", "title", "duration_ms", "owner", "created_at", "expires_at", "unlisted"]
ids = []
for n in (1, 2, 3):
    body = open(os.path.join(d, "req-%d.body" % n), "rb").read()
    hdr = open(os.path.join(d, "req-%d.hdr" % n), encoding="utf-8").read().splitlines()
    if not hdr[0].startswith("POST /in?k="):
        sys.exit("request %d: %r" % (n, hdr[0][:8]))
    h = {}
    for line in hdr[1:]:
        k, _, v = line.partition(":")
        h[k.strip().lower()] = v.strip()
    if h.get("content-type") != "application/json" or h.get("user-agent") != "Clippet":
        sys.exit("request %d: headers %r" % (n, {k: h[k] for k in h if k != "x-clippet-signature"}))
    sig = h.get("x-clippet-signature", "")
    if not re.fullmatch(r"sha256=[0-9a-f]{64}", sig):
        sys.exit("request %d: signature shape %r" % (n, sig[:12]))
    if not hmac.compare_digest(sig, "sha256=" + hmac.new(key, body, hashlib.sha256).hexdigest()):
        sys.exit("request %d: the signature does not verify over the received bytes" % n)
    if hmac.compare_digest(sig, "sha256=" + hmac.new(b"x" + key, body, hashlib.sha256).hexdigest()):
        sys.exit("request %d: the signature verifies with another key" % n)
    doc = json.loads(body)
    if list(doc) != want_keys:
        sys.exit("request %d: fields %r" % (n, list(doc)))
    for leak in (os.environ["HK_QTOK"], os.environ["HK_SECRET"], os.environ["HK_TOK"], "@", "clips/", "/tmp/"):
        if leak.encode() in body:
            sys.exit("request %d: the body carries something it must never carry" % n)
    cid = doc["id"]
    ids.append(cid)
    ok = (doc["event"] == "clip.ready" and doc["url"] == "http://127.0.0.1:8080/" + cid
          and doc["owner"] == os.environ["HK_USER"] and isinstance(doc["duration_ms"], int)
          and re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ", doc["created_at"])
          and re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ", doc["expires_at"] or "")
          and doc["unlisted"] == (cid == os.environ["HK_U"])
          and doc["title"] == ("E2E hook unlisted" if cid == os.environ["HK_U"] else "E2E hook listed"))
    if not ok:
        sys.exit("request %d: document %r" % (n, doc))
if ids[0] != os.environ["HK_L"] or sorted(ids[1:]) != sorted([os.environ["HK_L"], os.environ["HK_U"]]):
    sys.exit("request order %r" % ids)
# The retry is the same document, byte for byte: nothing in it is a clock.
first = open(os.path.join(d, "req-1.body"), "rb").read()
again = [open(os.path.join(d, "req-%d.body" % n), "rb").read() for n in (2, 3) if ids[n - 1] == os.environ["HK_L"]][0]
if first != again:
    sys.exit("the listed clip's retry is not byte-identical to its first delivery")
print("[e2e]   three requests verified: signatures over the exact bytes, nine fields, unlisted marked, the retry identical")
HOOK_CHECK

# --- (e) the operator's side: the log, the doctor, and the half-configured refusal ------------------
docker_ logs "$HK_CT" > "$HK_OUT" 2>&1
has_file "$HK_OUT" "msg=\"clip webhook configured\" scheme=http host=127.0.0.1:$HK_RX" "the boot line names the scheme and the host"
has_file "$HK_OUT" 'msg="clip webhook delivery failed; requeued"' "the 500 was logged as a requeue"
[ "$(grep -c 'msg="clip webhook delivered"' "$HK_OUT")" = 2 ] || die "want two 'clip webhook delivered' lines:
$(grep 'clip webhook' "$HK_OUT")"
lacks_secret_f "$HK_OUT" "$HK_QTOK" "the app log carries the URL's token"
lacks_secret_f "$HK_OUT" "$HK_SECRET" "the app log carries the signing key"
lacks_file "$HK_OUT" 'discord webhook' "the hook instance wrote a Discord line"
if grep -Eq '^time=[^ ]+ level=ERROR |panic: |fatal error: ' "$HK_OUT"; then
  die "the hook instance logged an ERROR:
$(grep -E '^time=[^ ]+ level=ERROR |panic: |fatal error: ' "$HK_OUT")"
fi

HK_RC=0
hk_sh clippet admin doctor > "$HK_OUT" 2>&1 || HK_RC=$?
grep '^webhook\|^store .*outbound' "$HK_OUT" | sed -n 's/^/[e2e]   /p'
has_file_f "$HK_OUT" "CLIP_WEBHOOK_URL parses as http://127.0.0.1:$HK_RX/(path redacted) - every clip that becomes ready is sent there, unlisted ones included, signed with CLIP_WEBHOOK_SECRET (DEPLOY.md §11.1); the shape is checked, nothing is sent; plain http can be read by anyone on the network path" "the doctor's hook row"
has_file_f "$HK_OUT" 'DISCORD_WEBHOOK_URL is unset: no clip is posted to Discord' "the Discord-unset row with the hook set"
has_file_f "$HK_OUT" 'on `fs` the media path has NO outbound destination' "the fs store row"
lacks_file_f "$HK_OUT" 'opens no outbound connection' "a doctor sentence the hook makes false"
has_file "$HK_OUT" 'doctor:  19 checks' "the doctor's summary"
lacks_secret_f "$HK_OUT" "$HK_QTOK" "admin doctor printed the URL's token"
lacks_secret_f "$HK_OUT" "$HK_SECRET" "admin doctor printed the signing key"

HK_RC=0
hk_sh 'unset CLIP_WEBHOOK_SECRET; clippet admin stats --no-storage' > "$HK_OUT" 2>&1 || HK_RC=$?
expect "$HK_RC" 2 "the URL without the secret must refuse to start"
has_file_f "$HK_OUT" 'CLIP_WEBHOOK_SECRET: is unset but CLIP_WEBHOOK_URL is set: the clip webhook needs both, or neither' "the refusal names the missing variable"
lacks_secret_f "$HK_OUT" "$HK_QTOK" "the refusal echoed the URL"
echo "[e2e]   log, doctor and refusal name the scheme and host only; the half-configured pair refuses to boot"

hk_stop
trap - EXIT
rm -rf "$HK_DIR"
summary_row HK "$HK_L" "clip webhook (#54): listed — delivered after one 500 and its backoff, signature verified on the host; container removed"
summary_row HK "$HK_U" "clip webhook (#54): unlisted — delivered with \"unlisted\": true; container removed"
