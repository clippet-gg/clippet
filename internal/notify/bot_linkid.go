package notify

import (
	"crypto/sha256"
	"sync"
	"time"
)

// The link follow-up's own message id, per interaction token (§20 erratum 320
// (g), the box's half). After the upload form there is no "thinking…" to edit,
// so the link is a NEW follow-up message on the command's token, and a POST has
// no idempotency key at Discord. The id Discord answers the link's POST with is
// held here, so a link follow-up on a token that already posted one EDITS that
// message (PATCH `/messages/{id}`) instead of posting a second.
//
// Only the LINK is held. The ephemeral lines — "Uploading…" (erratum 325) and
// the form's not-applied line (erratum 326) — are their own messages with
// their own flow, and a failure sentence never follows a posted link, so none
// of them reads or fills this table.
//
// The key is the token's SHA-256, never the token itself: the caller already
// has the token when it asks, so the table need not hold a credential. Memory
// only, bounded, an entry gone with the token's life; a restart forgets it,
// and with it every job that could have posted on that token (a bot job is a
// goroutine, never re-delivered after a restart).

const (
	// linkIDMax bounds the table. A full table evicts its oldest entry: an
	// evicted link is only unguarded against a second post on its token,
	// which nothing in this binary attempts after the link is out.
	linkIDMax = 1024
)

type linkIDEntry struct {
	id      string
	expires time.Time
}

// linkIDTable holds the link follow-up's message id by the token's hash.
type linkIDTable struct {
	mu sync.Mutex
	m  map[[sha256.Size]byte]linkIDEntry
}

func newLinkIDTable() *linkIDTable {
	return &linkIDTable{m: map[[sha256.Size]byte]linkIDEntry{}}
}

func linkIDKey(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

// get is the link's held id on this token, or "" when none is held (or it
// expired with the token).
func (t *linkIDTable) get(token string, now time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := linkIDKey(token)
	e, ok := t.m[k]
	if !ok {
		return ""
	}
	if !now.Before(e.expires) {
		delete(t.m, k)
		return ""
	}
	return e.id
}

// hold records the link's id for the token's remaining life (linkLife from
// now: the token was born a little earlier, so an entry can outlive it by the
// job's own length — an edit on a dead token fails exactly as a post would).
// An empty id holds nothing.
func (t *linkIDTable) hold(token, id string, now time.Time) {
	if id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	k := linkIDKey(token)
	if _, ok := t.m[k]; !ok && len(t.m) >= linkIDMax {
		t.sweepLocked(now)
		if len(t.m) >= linkIDMax {
			t.evictOldestLocked()
		}
	}
	t.m[k] = linkIDEntry{id: id, expires: now.Add(linkLife)}
}

// sweepLocked drops every expired entry.
func (t *linkIDTable) sweepLocked(now time.Time) {
	for k, e := range t.m {
		if !now.Before(e.expires) {
			delete(t.m, k)
		}
	}
}

// evictOldestLocked drops the entry held longest: every entry's life is the
// same, so the earliest expiry is the oldest hold.
func (t *linkIDTable) evictOldestLocked() {
	var oldest [sha256.Size]byte
	var at time.Time
	first := true
	for k, e := range t.m {
		if first || e.expires.Before(at) {
			oldest, at, first = k, e.expires, false
		}
	}
	if !first {
		delete(t.m, oldest)
	}
}

// len is for the tests.
func (t *linkIDTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}
