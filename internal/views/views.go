// Package views deduplicates view beacons per (clip, viewer) in bounded
// memory (DESIGN.md §13): a map of key → record plus a FIFO of insertions.
// Every insert first drops expired heads, then — at the hard cap — drops
// the oldest live entries too, so a scanner can only shorten the dedupe
// window for the oldest viewers, never grow memory.
package views

import (
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"sync"
	"time"
)

const defaultMax = 50000

// Deduper remembers keys for ttl with a hard cap on entries (oldest evicted
// first, even if unexpired).
type Deduper struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	seq  uint64
	seen map[string]record
	fifo []slot // insertion order; head index avoids reallocating on pop
	head int
}

type record struct {
	seq    uint64
	expiry time.Time
}

type slot struct {
	key    string
	seq    uint64
	expiry time.Time
}

// NewDeduper returns a Deduper with the given window and cap (≤ 0 → 50,000).
func NewDeduper(ttl time.Duration, max int) *Deduper {
	if max < 1 {
		max = defaultMax
	}
	return &Deduper{ttl: ttl, max: max, seen: map[string]record{}}
}

// Seen returns true (do not count) when key was recorded within ttl;
// otherwise it records key at now and returns false (count it).
func (d *Deduper) Seen(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rec, ok := d.seen[key]; ok && now.Before(rec.expiry) {
		return true
	}
	d.popExpired(now)
	for len(d.seen) >= d.max {
		d.popHead()
	}
	d.seq++
	rec := record{seq: d.seq, expiry: now.Add(d.ttl)}
	d.seen[key] = rec
	d.fifo = append(d.fifo, slot{key: key, seq: rec.seq, expiry: rec.expiry})
	return false
}

// Len reports the number of live entries (tests and metrics).
func (d *Deduper) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// popExpired drops FIFO heads whose expiry has passed. A re-recorded key
// leaves a stale slot behind; the seq check in popHead makes sure popping
// the stale slot never evicts the fresh record.
func (d *Deduper) popExpired(now time.Time) {
	for d.head < len(d.fifo) && !now.Before(d.fifo[d.head].expiry) {
		d.popHead()
	}
}

func (d *Deduper) popHead() {
	if d.head >= len(d.fifo) {
		return
	}
	s := d.fifo[d.head]
	d.fifo[d.head] = slot{} // release the string for GC
	d.head++
	if rec, ok := d.seen[s.key]; ok && rec.seq == s.seq {
		delete(d.seen, s.key)
	}
	// Compact once the consumed prefix dominates the backing array.
	if d.head > 1024 && d.head > len(d.fifo)/2 {
		d.fifo = append([]slot(nil), d.fifo[d.head:]...)
		d.head = 0
	}
}

// Key is clipID + "|" + hex(sha256(ip))[:16] -- the ADDRESS alone (§20
// erratum 103).
//
// The User-Agent used to be in the key, to let two people behind one NAT each
// count once. That is a real case, but the header is written by the caller:
// three POSTs from one address with three invented User-Agents counted three
// views, so any link holder could inflate the owner's counter at the view
// limiter's 60/min, and the churn also evicted honest entries at
// VIEW_DEDUPE_MAX and so shortened the dedupe window for everyone else. A
// count is bounded by ADDRESSES now, which the caller cannot mint. The cost is
// the NAT case -- a household is one view per window -- and that is the right
// side to be wrong on for a private number whose only job is to tell its owner
// roughly how a clip did.
//
// The address is still hashed: the dedupe map is memory, but it is memory this
// process can be asked to hand over, and an IP is the one identifying thing in
// it (§13).
func Key(clipID string, ip netip.Addr) string {
	sum := sha256.Sum256([]byte(ip.String()))
	return clipID + "|" + hex.EncodeToString(sum[:])[:16]
}
