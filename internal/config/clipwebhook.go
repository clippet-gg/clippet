package config

import (
	"log/slog"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The clip webhook (ROADMAP #54, DESIGN.md §20 erratum 319): ONE URL the
// operator names, POSTed a small signed JSON document each time a clip becomes
// ready. Two variables that travel together: both unset is the OFF state (no
// `hook` job is ever enqueued and internal/notify builds nothing for it), and
// one without the other is fatal at config.Load for the reason the login's two
// are (erratum 302): a half-configured webhook boots and is silently dead — or,
// worse, sends unsigned documents a receiver cannot tell from a forgery.
//
// It is the one outbound feature whose destination is NOT on an allowlist: the
// receivers it is for (a script, an automation server on the operator's own
// network) live wherever the operator runs them. What keeps it honest is that
// the destination is named in `.env` by the operator and nowhere else, that
// the boot line and `admin doctor` print its scheme and host, and that the
// request goes through the same single construction function as every other
// outbound call, which never follows a redirect (§15).
const (
	// EnvClipWebhookURL is the receiver. It is treated as a SECRET: receivers
	// commonly carry their own credential in the path or the query.
	EnvClipWebhookURL = "CLIP_WEBHOOK_URL"
	// EnvClipWebhookSecret is the HMAC-SHA256 key that signs each body. It is
	// a secret and is never sent anywhere: only the signature leaves the box.
	EnvClipWebhookSecret = "CLIP_WEBHOOK_SECRET"
)

// minClipWebhookSecretChars is the shortest key accepted. 32 characters is
// what `openssl rand -hex 16` prints and half of what DEPLOY suggests; the rule
// exists to stop a placeholder or a pasted word, not to grade entropy.
const minClipWebhookSecretChars = 32

// ClipWebhook is the validated pair.
//
// **Both values are SECRETS** and are reachable only through URL() and
// Secret(). The String()/LogValue() pair renders the scheme and the host —
// never the path, the query or the key — so a Config printed with %v, handed
// to slog or walked by `admin doctor` can never leak either one.
type ClipWebhook struct {
	raw    string // "" = not configured
	secret string
	scheme string
	host   string // with the port when one was given
	tail   bool   // the URL has a path beyond "/" or a query
}

// Configured reports whether the webhook is on.
func (w ClipWebhook) Configured() bool { return w.raw != "" }

// URL is the receiver exactly as configured — a secret. Only internal/notify
// may put it on the wire; nothing may put it in a log line, an error or a DB
// row.
func (w ClipWebhook) URL() string { return w.raw }

// Secret is the signing key — a secret that never leaves the process.
func (w ClipWebhook) Secret() string { return w.secret }

// Scheme is `http` or `https` (not secret).
func (w ClipWebhook) Scheme() string { return w.scheme }

// Host is the receiver's host, with its port when one was given (not secret).
func (w ClipWebhook) Host() string { return w.host }

// String is the redacted rendering: `<scheme>://<host>` and, when the URL
// carries anything after the host, `/(path redacted)`.
func (w ClipWebhook) String() string {
	if !w.Configured() {
		return "(unset)"
	}
	s := w.scheme + "://" + w.host
	if w.tail {
		s += "/(path redacted)"
	}
	return s
}

// LogValue makes slog print the redacted form even if the whole value is
// logged by accident.
func (w ClipWebhook) LogValue() slog.Value { return slog.StringValue(w.String()) }

// clipWebhook reads and validates the two variables as ONE value. No message
// it writes contains any part of either value: a URL pasted into the wrong
// variable would otherwise put the key, or the receiver's token, in a boot log.
func (l *loader) clipWebhook() ClipWebhook {
	raw := l.raw(EnvClipWebhookURL)
	secret := l.raw(EnvClipWebhookSecret)
	if raw == "" && secret == "" {
		return ClipWebhook{} // the default: off
	}
	w, urlOK := ClipWebhook{}, false
	if raw != "" {
		w, urlOK = l.parseClipWebhookURL(raw)
	}
	secretOK := secret != "" && l.clipWebhookSecret(secret)
	switch {
	case raw == "":
		l.fail(EnvClipWebhookURL, "is unset but %s is set: the clip webhook needs both, or neither", EnvClipWebhookSecret)
		return ClipWebhook{}
	case secret == "":
		l.fail(EnvClipWebhookSecret, "is unset but %s is set: the clip webhook needs both, or neither", EnvClipWebhookURL)
		return ClipWebhook{}
	case !urlOK || !secretOK:
		return ClipWebhook{}
	}
	w.secret = secret
	return w
}

// parseClipWebhookURL applies the URL's rules. A query is allowed (receivers
// put their token there); a fragment is not, because it is never sent and so
// can only be a paste that did not mean what it says.
func (l *loader) parseClipWebhookURL(v string) (ClipWebhook, bool) {
	fail := func(format string) { l.fail(EnvClipWebhookURL, "%s", format) }
	u, err := url.Parse(v)
	if err != nil {
		fail("is not a valid URL (the value is a secret and is not echoed here)")
		return ClipWebhook{}, false
	}
	// url.Parse lowercases the scheme. Nothing of the value is echoed here:
	// the two variables swapped would parse the key's prefix as a "scheme".
	if u.Scheme != "http" && u.Scheme != "https" {
		fail("scheme must be http or https")
		return ClipWebhook{}, false
	}
	if u.Hostname() == "" {
		fail("must name a host")
		return ClipWebhook{}, false
	}
	if u.User != nil {
		fail("must not contain userinfo (user:password@)")
		return ClipWebhook{}, false
	}
	// A trailing "#" leaves Fragment empty, so the raw string is checked too.
	if u.Fragment != "" || strings.Contains(v, "#") {
		fail("must not carry a fragment")
		return ClipWebhook{}, false
	}
	return ClipWebhook{
		raw:    v,
		scheme: u.Scheme,
		host:   asciiLower(u.Host), // ASCII case alone: what is shown is what is dialled (§20 erratum 327)
		tail:   (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery,
	}, true
}

// clipWebhookSecret applies the key's rule: long enough to be a key and free
// of whitespace, which is what a value pasted from somewhere it was wrapped
// carries.
func (l *loader) clipWebhookSecret(s string) bool {
	ok := utf8.RuneCountInString(s) >= minClipWebhookSecretChars && strings.IndexFunc(s, unicode.IsSpace) < 0
	if !ok {
		l.fail(EnvClipWebhookSecret, "must be at least %d characters with no whitespace (the value is a secret and is not echoed here)", minClipWebhookSecretChars)
	}
	return ok
}
