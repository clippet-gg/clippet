package httpx

import (
	"errors"
	"sync"
	"time"
)

// StorageHealth caches the outcome of the last store.Ping (§8.7). The boot
// ping loop and /readyz write it; /healthz reads it and therefore never
// performs a live storage call.
type StorageHealth struct {
	mu      sync.Mutex
	set     bool
	err     error
	checked time.Time
	backend string
}

// Set records the result of a ping.
func (h *StorageHealth) Set(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.set, h.err, h.checked = true, err, time.Now()
}

// SetBackend names which store answers this instance's pings — `s3` or `fs`
// (ROADMAP #37, §20 erratum 276). `serve` calls it once at boot; a zero
// StorageHealth has no backend and renders exactly as it always did.
func (h *StorageHealth) SetBackend(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.backend = name
}

// Get renders the cached state: "pending" until the first Set, then "ok" or
// "error: <class>", each followed by the backend's name in parentheses once
// SetBackend has been called. It is what the anonymous /healthz and /readyz
// print, so it renders the failure's bounded CLASS, never its message (§20
// erratum 31): a ping failure is a net/http *url.Error carrying the whole
// request URL, and §15 says /healthz exposes no config values. The full error
// stays with the callers, which log it.
//
// The backend word is held to that same rule: it is one of TWO values, it
// names no endpoint, bucket, path or credential, and it answers the first
// question anybody asks of an unwell probe — is this a bucket problem or a
// disk problem — which nothing else anonymous can answer.
func (h *StorageHealth) Get() string {
	s := h.State()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.backend != "" {
		s += " (" + h.backend + ")"
	}
	return s
}

// State is Get without the backend word: `pending`, `ok` or `error: <class>`
// and nothing else (§20 erratum 297). It exists because /setup's first panel
// already names the backend on a row of its own, and a state cell repeating it
// would be the one place in the product where one fact is printed twice —
// while the CLASS is the only storage word a non-operator surface may print,
// so the page must not compose that string itself.
func (h *StorageHealth) State() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case !h.set:
		return "pending"
	case h.err != nil:
		return "error: " + storageClass(h.err)
	default:
		return "ok"
	}
}

// healthClassifier is implemented by an error that knows how to describe
// itself to an anonymous prober (store.PingError). The interface keeps this
// package free of a store dependency.
type healthClassifier interface{ HealthClass() string }

// storageClass renders the class of a ping failure. Anything that does not
// classify itself is "unavailable": the leak is closed by default, never by
// enumerating the error shapes that would have leaked.
func storageClass(err error) string {
	var c healthClassifier
	if errors.As(err, &c) {
		if s := c.HealthClass(); s != "" {
			return s
		}
	}
	return "unavailable"
}

// OK reports whether the last ping succeeded (false while pending).
func (h *StorageHealth) OK() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.set && h.err == nil
}

// LastChecked returns when Set was last called (zero while pending).
func (h *StorageHealth) LastChecked() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.checked
}
