// Package notify is the one package that may make an outbound HTTP request
// besides the S3 client (§15): the "clip is ready" post to the operator's
// Discord webhook (DESIGN.md §17 #4, §20 errata 63–66), the Discord bot and
// login (errata 253–256, 302), and the clip webhook to the one URL the
// operator names in CLIP_WEBHOOK_URL (erratum 319).
//
// Each exists only when its variables are set: New, NewBot, NewLogin and
// NewHook return nil otherwise, cmd/clippet/serve.go calls each exactly once,
// and every client is built by newHTTPClient, which never follows a redirect.
// Nothing else in the tree may construct an http.Client. scripts/check.sh
// greps the common call sites for a second one — a tripwire, not a proof; the
// guarantee is the configuration rules in internal/config and that single
// construction site (§20 erratum 66).
//
// **The endpoint is a secret.** A Discord webhook's token is the last segment
// of its path, so the URL must never reach a log line, an error string or a DB
// row. net/http hands back `*url.Error`, whose message embeds the URL it
// failed on, so every error is unwrapped to its cause and then scrubbed before
// it leaves this package.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"clippet/internal/urls"
)

const (
	// attemptTimeout bounds ONE delivery attempt end to end. The worker
	// retries; a slow Discord must never hold a worker goroutine (or, on
	// shutdown, worker.Stop) for longer than this.
	attemptTimeout = 10 * time.Second
	// maxRetryAfter caps how long a 429's Retry-After may push a retry out.
	// Discord's own rate limits are seconds; a wild value is a bug or a
	// hostile proxy, and a job must stay in the operator's sight.
	maxRetryAfter = 5 * time.Minute
	// bodySnippet is how much of a rejection body is captured for the log and
	// the job row. The BODY is not secret (it is Discord's error JSON, e.g.
	// {"message":"Unknown Webhook","code":10015}); the URL is.
	bodySnippet = 200
	// bodyWindow is how much of the body is read BEFORE scrubbing, and it is
	// deliberately larger than bodySnippet. Scrubbing a string that has
	// already been cut cannot remove a secret that straddled the cut — the
	// prefix left behind matches nothing — so the cut has to be the last step,
	// on an already-scrubbed string, and the window has to be wide enough to
	// contain any secret that reaches into the snippet. A webhook endpoint is
	// ~120 bytes and its token ~70, so 4× leaves a large margin.
	bodyWindow = 4 * bodySnippet
	// maxContent is Discord's message limit, in runes.
	maxContent = 2000
	// waitQuery makes Discord answer 200 with the created message instead of
	// 204 with an empty body. Both are success; the 200 is simply easier to
	// reason about when an operator curls the same URL by hand.
	waitQuery = "?wait=true"
	// userAgent is deliberately static: no version, no hostname, nothing that
	// identifies the deployment. It exists only because a generic HTTP-client
	// UA is the kind of thing an edge proxy in front of Discord challenges.
	userAgent = "Clippet"
	// redacted replaces the endpoint (or its token) if either ever appears in
	// a string on its way out of this package.
	redacted = "…"
)

// Result is the outcome of one delivery attempt.
//
//	OK    — a 2xx: the job is done.
//	Retry — a network error, a timeout, a 429 or a 5xx: requeue with backoff.
//	neither — any other status, including a 3xx: permanent, the job is failed.
type Result struct {
	OK    bool
	Retry bool
	// Status is the HTTP status, or 0 when no response was received.
	Status int
	// RetryAfter is a 429's honoured Retry-After (capped at maxRetryAfter);
	// 0 when the response carried none.
	RetryAfter time.Duration
	// Detail is one line for the log and the job row. It never contains the
	// endpoint or its token.
	Detail string
}

// Deps configures a Notifier.
//
// Endpoint is the already-validated webhook URL (config.DiscordWebhook.URL()).
// It is taken as a plain string rather than as a config.DiscordWebhook so the
// ALLOWLIST — which lives in config and is tested there — is not also the
// thing that decides where a unit test may point: the tests here drive a
// httptest server, and never the network.
type Deps struct {
	Endpoint string
	// Routes are the named webhooks of DISCORD_WEBHOOK_ROUTES (ROADMAP #26,
	// §20 erratum 158). A Name is a tag — not a secret — and is the ONLY half
	// of a route that leaves this package: it is what a log line says and what
	// a notify job's params_json stores. Endpoint is the secret and is
	// scrubbed exactly as the default one is.
	Routes []Route
	URLs   urls.Builder
	// Timeout overrides attemptTimeout (tests only; 0 = the default).
	Timeout time.Duration
	// Transport overrides the client's transport (tests only).
	Transport http.RoundTripper
}

// Route is one named webhook.
type Route struct{ Name, Endpoint string }

// DefaultRoute is the name of the unnamed webhook — DISCORD_WEBHOOK_URL — in
// a log line and in a job's params_json. A job that stores no route at all
// means this one; the constant exists so the WORD an operator greps for is
// defined in exactly one place.
const DefaultRoute = "default"

// Notifier delivers one message per ready clip, to the webhook the clip was
// ROUTED to when its job was queued.
type Notifier struct {
	// endpoints maps a route name to its secret endpoint, with ?wait=true
	// already appended. DefaultRoute is present iff DISCORD_WEBHOOK_URL is set.
	endpoints map[string]string
	secrets   []string // every spelling of every secret, longest first
	urls      urls.Builder
	client    *http.Client
}

// New returns a Notifier, or nil when NO webhook at all is configured —
// neither the default nor a route. A nil Notifier is the "Discord is off"
// state: no http.Client is constructed for Discord and no notify job is ever
// enqueued.
//
// However many webhooks are configured, there is exactly ONE client and one
// construction site: the §15 tripwire is about how many ways this binary can
// reach the network, and a per-route client would have been three.
func New(d Deps) *Notifier {
	endpoints := make(map[string]string, len(d.Routes)+1)
	var secrets []string
	add := func(name, endpoint string) {
		if endpoint == "" || name == "" {
			return
		}
		if _, dup := endpoints[name]; dup {
			// Unreachable from a loaded Config: it refuses a duplicate tag, and
			// since §20 erratum 160 a rule tagged DefaultRoute as well. Kept as
			// the last line of defence — the default webhook is added FIRST, so
			// whatever arrives here it can never be displaced silently.
			return
		}
		endpoints[name] = endpoint + waitQuery
		secrets = append(secrets, secretSpellings(endpoint)...)
	}
	add(DefaultRoute, d.Endpoint)
	for _, r := range d.Routes {
		add(r.Name, r.Endpoint)
	}
	if len(endpoints) == 0 {
		return nil
	}
	sortLongestFirst(secrets)
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = attemptTimeout
	}
	return &Notifier{
		endpoints: endpoints,
		secrets:   secrets,
		urls:      d.URLs,
		// Redirects are NOT followed (newHTTPClient, bot.go — the one
		// construction site both outbound features share). A 3xx off the
		// validated endpoint is exactly the laundering §20 erratum 27 exists
		// to prevent: it would let a value that passed the host allowlist
		// send the message (and the Authorization-shaped webhook token in the
		// URL) somewhere else. http.ErrUseLastResponse hands the 3xx back as
		// the response, and the classifier calls it permanent.
		client: newHTTPClient(timeout, d.Transport),
	}
}

// lastSegment is the webhook token — kept only so it can be scrubbed.
func lastSegment(u string) string {
	if i := strings.LastIndexByte(u, '/'); i >= 0 && i+1 < len(u) {
		return u[i+1:]
	}
	return ""
}

// secretSpellings enumerates every form the endpoint or its token can take in
// a string on its way out of this package, longest first so that replacing a
// short one never eats half of a longer one.
//
// The configured spelling is not the only one that can come back. net/url
// NORMALISES what it puts on the wire: a token carrying a character that has
// to be percent-encoded in a path is sent — and echoed back by the far end, or
// embedded in a *url.Error — in its escaped form, which no ReplaceAll of the
// raw value matches. (config now rejects such a token outright, §20 erratum
// 63; this package takes a plain string and must not depend on that.) The
// `?wait=true` form is what the request actually carries, and the token on its
// own is the half worth stealing, so it is scrubbed even when it turns up
// without a URL around it.
func secretSpellings(endpoint string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(endpoint)
	add(endpoint + waitQuery)
	if tok := lastSegment(endpoint); tok != "" {
		add(tok)
		add(url.PathEscape(tok))
		add(url.QueryEscape(tok))
	}
	if u, err := url.Parse(endpoint); err == nil {
		add(u.String())
		add(u.String() + waitQuery)
		add(lastSegment(u.EscapedPath()))
	}
	sortLongestFirst(out)
	return out
}

// sortLongestFirst orders the scrub needles so replacing a short one can
// never eat half of a longer one.
func sortLongestFirst(out []string) {
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
}

// HasRoute reports whether a route name still resolves to a configured
// webhook. The worker asks before delivering, because a job enqueued under a
// route the operator has since removed from DISCORD_WEBHOOK_ROUTES must be
// SKIPPED — posting it to the default channel instead would put a clip in a
// channel nobody chose for it, which is the one thing routing exists to stop.
func (n *Notifier) HasRoute(name string) bool {
	if name == "" {
		name = DefaultRoute
	}
	_, ok := n.endpoints[name]
	return ok
}

// Deliver posts one message for a ready clip to the webhook `route` names
// ("" = DefaultRoute) and classifies the outcome. It makes exactly one
// attempt; retrying (and counting attempts) is the worker's job, because that
// is where the durable job row lives.
func (n *Notifier) Deliver(ctx context.Context, route, clipID, title string) Result {
	if route == "" {
		route = DefaultRoute
	}
	endpoint, ok := n.endpoints[route]
	if !ok {
		// The worker checks HasRoute first, so this is unreachable in the
		// binary; it is a permanent result rather than a panic because the
		// alternative — falling back to the default webhook — would post to a
		// channel the operator did not choose. The route NAME is a tag and is
		// safe to say out loud; nothing here can name a URL.
		return Result{Detail: "no webhook configured for route " + strconv.Quote(route)}
	}
	body, err := json.Marshal(newPayload(Content(title, n.urls.Page(clipID))))
	if err != nil { // unreachable: the payload is two strings
		return Result{Detail: "payload could not be encoded"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		// The error text embeds the URL — say what happened, not what failed.
		return Result{Detail: "request could not be built"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return Result{Retry: true, Detail: "delivery failed: " + n.scrub(cause(err))}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()
	return classify(resp, n.bodySnippet(resp.Body))
}

// classify turns one response into a Result. It is shared by every delivery
// in this package that the worker retries (the Discord notifier and the clip
// webhook), so the two can never disagree about what a 429 or a 3xx means.
// snippet is the already-scrubbed rendering of the body.
func classify(resp *http.Response, snippet string) Result {
	code := resp.StatusCode
	switch {
	case code/100 == 2:
		return Result{OK: true, Status: code, Detail: fmt.Sprintf("delivered: HTTP %d", code)}
	case code == http.StatusTooManyRequests:
		after := retryAfter(resp.Header.Get("Retry-After"))
		return Result{Retry: true, Status: code, RetryAfter: after,
			Detail: fmt.Sprintf("rate limited: HTTP %d retry_after=%s %s", code, after, snippet)}
	case code/100 == 5:
		return Result{Retry: true, Status: code, Detail: fmt.Sprintf("server error: HTTP %d %s", code, snippet)}
	case code/100 == 3:
		// Never followed (see CheckRedirect): the operator's URL is meant to
		// be the destination, not a starting point.
		return Result{Status: code, Detail: fmt.Sprintf("rejected: HTTP %d (a redirect is not followed) %s", code, snippet)}
	default:
		return Result{Status: code, Detail: fmt.Sprintf("rejected: HTTP %d %s", code, snippet)}
	}
}

// scrub is the last line of defence for the secret: whatever string is about
// to leave this package, every spelling of the endpoint and of its token is
// removed from it.
func (n *Notifier) scrub(s string) string {
	for _, secret := range n.secrets {
		s = strings.ReplaceAll(s, secret, redacted)
	}
	return s
}

// cause strips net/http's *url.Error wrapper — whose Error() prints the URL it
// failed on, i.e. the secret — and returns the transport error underneath:
// "dial tcp …: connection refused", "context deadline exceeded (Client.Timeout
// exceeded while awaiting headers)", "context canceled". None of those names a
// URL, and one level of unwrapping keeps the useful detail that a full unwrap
// down to a bare syscall.Errno would throw away.
func cause(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// bodySnippet renders a rejection body for the log and the job row. Discord's
// error JSON is what makes a 400/404 diagnosable.
//
// The ORDER is the point: read a window, scrub the secret out of it, and only
// THEN cut it to bodySnippet bytes. Cutting first would leave the prefix of a
// secret that straddled the cut in the output, and no later scrub can match a
// half of its needle.
func (n *Notifier) bodySnippet(r io.Reader) string {
	s := truncateBytes(scrubWindow(r, bodyWindow, n.scrub, n.secrets), bodySnippet)
	if s == "" {
		return "(empty body)"
	}
	return "body=" + s
}

// scrubWindow reads a window of n bytes, scrubs it, and — when the read FILLED
// the window, so the body went on past it — strikes the window's tail if it is
// the start of a needle (run 8, the hook lane's two residual gaps): a receiver
// that echoes the secret right at the window's edge, after a run of leading
// whitespace the trim then removes, or as the last of several echoes, would
// otherwise leave that cut-off prefix where no whole-needle scrub can match
// it, and the leading-whitespace trim moves it inside the snippet.
func scrubWindow(r io.Reader, n int64, scrub func(string) string, needles []string) string {
	s, full := readWindowFull(r, n)
	s = scrub(s)
	if full {
		s = strikeCutNeedle(s, needles)
	}
	return s
}

// strikeCutNeedle replaces the longest tail of s that is a proper prefix of a
// needle with the redaction mark.
func strikeCutNeedle(s string, needles []string) string {
	best := 0
	for _, nd := range needles {
		for k := min(len(nd)-1, len(s)); k > best; k-- {
			if strings.HasSuffix(s, nd[:k]) {
				best = k
				break
			}
		}
	}
	if best == 0 {
		return s
	}
	return s[:len(s)-best] + redacted
}

// readWindow reads at most bodyWindow bytes and renders them on one line.
func readWindow(r io.Reader) string { return readWindowN(r, bodyWindow) }

// readWindowN is readWindow with a window of n bytes: the clip webhook's URL
// has no length cap, so its window grows with its longest scrub needle.
func readWindowN(r io.Reader, n int64) string {
	s, _ := readWindowFull(r, n)
	return s
}

// readWindowFull is readWindowN, plus whether the read filled the window (the
// body may go on past it).
func readWindowFull(r io.Reader, n int64) (string, bool) {
	b, _ := io.ReadAll(io.LimitReader(r, n))
	s := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, string(b)))
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	return s, int64(len(b)) >= n
}

// truncateBytes cuts s to at most n bytes without splitting a rune (the cut
// can land inside one: the window boundary is a byte count, and so is this).
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// retryAfter parses a Retry-After header: delta-seconds (Discord sends a
// fractional value) or an HTTP-date. Anything else, or a value in the past,
// is 0 — the caller then falls back to its own backoff.
//
// The float is range-checked BEFORE the cast to a Duration. A Duration is an
// int64 of nanoseconds, so anything from about 9.2e9 seconds up — and +Inf,
// which strconv.ParseFloat accepts from the header text "Inf" — converts to an
// implementation-defined value that is typically NEGATIVE. That number would
// reach a WARN line and jobs.last_error as "retry_after=-2562047h47m16s", and
// would be handed to FailAttemptAfter as a floor. `!(secs > 0)` also catches
// NaN, which fails every comparison including `secs <= 0`.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		switch {
		case !(secs > 0): // zero, negative, or NaN
			return 0
		case secs > maxRetryAfter.Seconds(): // huge, or +Inf
			return maxRetryAfter
		}
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return min(d, maxRetryAfter)
		}
	}
	return 0
}

// payload is the whole request body. There is no `embeds` object on purpose:
// Discord unfurls the share link itself from Clippet's Open Graph tags (§9.1),
// which is what makes the card a PLAYER rather than a picture.
type payload struct {
	Content         string          `json:"content"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

// allowedMentions with an empty parse list is MANDATORY: the content carries a
// user-supplied title, and without it a clip called "@everyone gg" would ping
// the whole server. Parse is initialised to a non-nil slice so it marshals as
// `[]` (which means "nothing may be mentioned") and never as `null` (which
// means "apply the default", i.e. everything).
type allowedMentions struct {
	Parse []string `json:"parse"`
}

func newPayload(content string) payload {
	return payload{Content: content, AllowedMentions: allowedMentions{Parse: []string{}}}
}

// inlineMeta are the markdown metacharacters escaped ANYWHERE in a title.
//
// The dangerous one is '[': Discord renders `[text](url)` as a MASKED LINK in
// webhook- and bot-authored content (it does not in a message a human types),
// so a user-controlled title would otherwise become a link whose visible text
// lies about where it goes — posted under the operator's webhook identity, in
// the operator's channel. ']' goes with it, and the rest ('`' code, '*' and
// '_' emphasis, '~' strikethrough, '|' spoiler, '#' headers, '>' quotes) are
// escaped by the same rule so a title cannot silently restyle, hide or
// code-format the message around the link either.
//
// The backslash itself is FIRST and is escaped too. Without that, a title
// containing `\[x](y)` would come out `\\[x](y)`: Discord reads `\\` as a
// literal backslash and the '[' it precedes is unescaped again — the masked
// link comes right back.
const inlineMeta = "\\`*_~|[]#>"

// leadingMeta are escaped only as the FIRST character, where they open a
// markdown BLOCK: '-' (a bullet). The rest of the old block-starter set
// ('#', '>', '*', '`') is in inlineMeta and so is covered everywhere. '-' is
// left alone mid-title because hyphens are ordinary in real titles and
// escaping every one of them would be visible in the common case.
const leadingMeta = "-"

// Content renders the message: the (neutralised) title on the first line, the
// share URL on the second, and nothing else — no CTA, no watermark, no
// instance branding (§15 anti-features).
//
// The title is neutralised in two steps.
//
//  1. Control characters — including the tab/newline/CR that §8.2's cleanText
//     allows in a title — become spaces, so one title is one line and the URL
//     always sits alone on the second. So do the bidi overrides and the
//     zero-width characters (U+200B–U+200F, U+202A–U+202E, U+2066–U+2069,
//     U+FEFF): they are invisible, they ride along into every copy of the
//     message, and a right-to-left override in a one-line title exists only to
//     make the line read as something other than what it is.
//  2. Every markdown metacharacter is prefixed with a backslash — inlineMeta
//     anywhere, leadingMeta at the start. Backslash rather than a zero-width
//     space because it neutralises the token twice over: Discord documents '\'
//     as its markdown escape, so "\# x" renders as "# x"; and even where that
//     escape did not apply, the line no longer BEGINS with the character,
//     which is what block markdown requires. A zero-width space would only
//     have the second property.
//
// The result is clamped to Discord's 2000-rune limit by trimming the TITLE —
// AFTER escaping, because escaping adds runes — and the trim never leaves a
// dangling backslash, which would escape the newline and pull the URL onto the
// title's line. The URL is the payload and is never cut.
//
// What is still NOT neutralised, and is documented rather than fixed: a title
// containing a bare URL is auto-linked by Discord and may unfurl alongside the
// clip. It cannot lie about its destination — that was the masked-link problem
// above, and it is fixed — so what is left is a visible link to a visible URL.
func Content(title, pageURL string) string {
	t := escapeMarkdown(strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f, r == 0x2028, r == 0x2029:
			return ' '
		case r >= 0x200b && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0xfeff:
			return ' '
		}
		return r
	}, title)))
	budget := maxContent - utf8.RuneCountInString(pageURL) - 1 // -1 for the newline
	if t == "" || budget <= 0 {
		return pageURL
	}
	if t = clamp(t, budget); t == "" {
		return pageURL
	}
	return t + "\n" + pageURL
}

// escapeMarkdown backslash-escapes every metacharacter of the title.
func escapeMarkdown(t string) string {
	var b strings.Builder
	b.Grow(len(t) + len(t)/8 + 1)
	for i, r := range t {
		if strings.ContainsRune(inlineMeta, r) || (i == 0 && strings.ContainsRune(leadingMeta, r)) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// clamp cuts an already-escaped title to n runes without ending on a dangling
// backslash: an odd number of trailing backslashes would escape the '\n' that
// follows and put the share URL on the title's line.
func clamp(t string, n int) string {
	r := []rune(t)
	if len(r) <= n {
		return t
	}
	r = r[:n]
	slashes := 0
	for i := len(r) - 1; i >= 0 && r[i] == '\\'; i-- {
		slashes++
	}
	if slashes%2 == 1 {
		r = r[:len(r)-1]
	}
	return string(r)
}
