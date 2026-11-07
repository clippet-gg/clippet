package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"clippet/internal/urls"
)

// The clip webhook (ROADMAP #54, DESIGN.md §20 erratum 319): one signed JSON
// document per clip that becomes ready, POSTed to the one URL the operator
// named in CLIP_WEBHOOK_URL. It shares the notifier's discipline end to end —
// one attempt per call (the worker owns retries), the same Result classifier,
// a client from newHTTPClient so no redirect is ever followed, and every
// string that leaves this file scrubbed of the URL first.

const (
	// HookEvent is the one event the webhook sends today.
	HookEvent = "clip.ready"
	// HookSignatureHeader carries `sha256=<hex>`: the HMAC-SHA256 of the
	// exact body bytes, keyed with CLIP_WEBHOOK_SECRET.
	HookSignatureHeader = "X-Clippet-Signature"
	// hookSignaturePrefix names the algorithm inside the header's value.
	hookSignaturePrefix = "sha256="
	// minHookNeedle is the shortest PART of the URL that is scrubbed on its
	// own. A real token in a path segment or a query value is long; a short
	// one — `/in`, `hooks`, `1` — is a word an honest error line is made of,
	// and striking it out would corrupt every log line and job row the
	// webhook writes. The whole URL is always scrubbed, whatever its length.
	// (`admin doctor`'s minSecretLen is the precedent.)
	minHookNeedle = 8
)

// ClipEvent is what the worker knows about a clip at delivery time, as plain
// values: this package does not import internal/db. Every field is something
// the clip's public API already reports to anyone holding its link.
type ClipEvent struct {
	ID, Title string
	// Owner is the uploader's username — never the account id.
	Owner      string
	DurationMS *int64
	CreatedAt  int64  // unix seconds
	ExpiresAt  *int64 // unix seconds; nil = never expires
	Unlisted   bool
}

// hookPayload is the wire contract, in this order. The times use the public
// API's layout (internal/httpapi's rfc3339: UTC, whole seconds), and the two
// nullable fields are the clip row's nullable columns, so a receiver that also
// reads `GET /api/clips/{id}` sees the same values.
type hookPayload struct {
	Event      string  `json:"event"`
	ID         string  `json:"id"`
	URL        string  `json:"url"`
	Title      string  `json:"title"`
	DurationMS *int64  `json:"duration_ms"`
	Owner      string  `json:"owner"`
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  *string `json:"expires_at"`
	Unlisted   bool    `json:"unlisted"`
}

// HookDeps configures a Hook. Endpoint and Secret are the already-validated
// config.ClipWebhook values, taken as plain strings so a unit test can point
// the hook at an httptest server.
type HookDeps struct {
	Endpoint, Secret string
	URLs             urls.Builder
	// Timeout overrides attemptTimeout (tests only; 0 = the default).
	Timeout time.Duration
	// Transport overrides the client's transport (tests only).
	Transport http.RoundTripper
}

// Hook delivers the clip webhook.
type Hook struct {
	endpoint string
	key      []byte
	needles  []string // every spelling of the URL worth scrubbing, longest first
	window   int64    // how much of a rejection body is read before the scrub
	urls     urls.Builder
	client   *http.Client
}

// NewHook returns a Hook, or nil when either value is empty — the OFF state:
// no client is built and the worker enqueues no `hook` job. A Hook with no
// key would send documents a receiver cannot verify, so a missing secret is
// off rather than unsigned (config refuses that pair at boot anyway).
func NewHook(d HookDeps) *Hook {
	if d.Endpoint == "" || d.Secret == "" {
		return nil
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = attemptTimeout
	}
	needles := hookNeedles(d.Endpoint, d.Secret)
	window := int64(bodyWindow)
	if len(needles) > 0 {
		// Wide enough that a needle reaching into the snippet is wholly
		// inside what is scrubbed (bodyWindow's reason, for a URL with no
		// length cap).
		window = max(window, int64(bodySnippet+len(needles[0])))
	}
	return &Hook{
		endpoint: d.Endpoint,
		key:      []byte(d.Secret),
		needles:  needles,
		window:   window,
		urls:     d.URLs,
		client:   newHTTPClient(timeout, d.Transport),
	}
}

// hookNeedles lists what scrub removes. The WHOLE URL in every spelling it can
// come back in (as configured, as net/url re-renders it) and the key are
// always needles. Its parts — the path, the escaped path, the raw query, each
// path segment and each query value, raw and unescaped — are needles only at
// minHookNeedle bytes or more. Longest first, so replacing a short needle can
// never eat half of a longer one.
func hookNeedles(endpoint, secret string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string, always bool) {
		if s == "" || seen[s] || (!always && len(s) < minHookNeedle) {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(endpoint, true)
	add(secret, true)
	if u, err := url.Parse(endpoint); err == nil {
		add(u.String(), true)
		add(u.Path, false)
		add(u.EscapedPath(), false)
		add(u.RawQuery, false)
		for _, seg := range strings.Split(u.Path, "/") {
			add(seg, false)
			add(url.PathEscape(seg), false)
		}
		for _, vs := range u.Query() {
			for _, v := range vs {
				add(v, false)
				add(url.QueryEscape(v), false)
			}
		}
	}
	sortLongestFirst(out)
	return out
}

// Sign returns the header value for body: `sha256=` and the lowercase hex
// HMAC-SHA256 of the exact bytes, keyed with the secret's bytes as typed.
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hookSignaturePrefix + hex.EncodeToString(m.Sum(nil))
}

// Body renders one event as the bytes that are sent and signed.
func (h *Hook) Body(ev ClipEvent) ([]byte, error) {
	p := hookPayload{
		Event:      HookEvent,
		ID:         ev.ID,
		URL:        h.urls.Page(ev.ID),
		Title:      ev.Title,
		DurationMS: ev.DurationMS,
		Owner:      ev.Owner,
		CreatedAt:  hookTime(ev.CreatedAt),
		Unlisted:   ev.Unlisted,
	}
	if ev.ExpiresAt != nil {
		s := hookTime(*ev.ExpiresAt)
		p.ExpiresAt = &s
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	// The title is sent as typed: `<`, `>` and `&` are not escaped into
	// < and friends, which only a browser embedding the JSON needs.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// hookTime is internal/httpapi's rfc3339, restated: the API's layout is the
// contract, and this package does not import httpapi.
func hookTime(unix int64) string { return time.Unix(unix, 0).UTC().Format(time.RFC3339) }

// Deliver POSTs one event and classifies the outcome. It makes exactly one
// attempt; the worker counts attempts on the durable job row. The signature is
// computed here, at delivery, so a key rotated while a job waited applies to
// it after the restart.
func (h *Hook) Deliver(ctx context.Context, ev ClipEvent) Result {
	body, err := h.Body(ev)
	if err != nil { // unreachable: plain strings, numbers and a bool
		return Result{Detail: "payload could not be encoded"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(body))
	if err != nil {
		// The error text embeds the URL — say what happened, not what failed.
		return Result{Detail: "request could not be built"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(HookSignatureHeader, Sign(h.key, body))

	resp, err := h.client.Do(req)
	if err != nil {
		return Result{Retry: true, Detail: "delivery failed: " + h.scrub(cause(err))}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()
	return classify(resp, h.bodySnippet(resp.Body))
}

// scrub removes every needle from a string on its way out of this package.
func (h *Hook) scrub(s string) string {
	for _, n := range h.needles {
		s = strings.ReplaceAll(s, n, redacted)
	}
	return s
}

// bodySnippet is the notifier's, in the notifier's order — read a window,
// scrub it (a cut-off echo at the window's edge included), THEN cut — over a
// window sized to this URL.
func (h *Hook) bodySnippet(r io.Reader) string {
	s := truncateBytes(scrubWindow(r, h.window, h.scrub, h.needles), bodySnippet)
	if s == "" {
		return "(empty body)"
	}
	return "body=" + s
}
