package views

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"testing"
	"time"
)

func TestSeenDedupesWithinTTLAndExpires(t *testing.T) {
	d := NewDeduper(30*time.Minute, 100)
	t0 := time.Unix(1_700_000_000, 0)
	if d.Seen("a", t0) {
		t.Fatal("first sighting reported as seen")
	}
	if !d.Seen("a", t0.Add(29*time.Minute)) {
		t.Fatal("second sighting inside the window not deduped")
	}
	if d.Seen("b", t0.Add(29*time.Minute)) {
		t.Fatal("different key deduped")
	}
	// At exactly ttl the entry has expired: count again and re-record.
	if d.Seen("a", t0.Add(30*time.Minute)) {
		t.Fatal("expired entry still deduped")
	}
	if !d.Seen("a", t0.Add(59*time.Minute)) {
		t.Fatal("re-recorded entry not deduped")
	}
	if d.Len() != 2 {
		t.Fatalf("Len = %d, want 2", d.Len())
	}
}

func TestHardCapEvictsOldestEvenIfUnexpired(t *testing.T) {
	d := NewDeduper(time.Hour, 3)
	t0 := time.Unix(1_700_000_000, 0)
	for i := 0; i < 3; i++ {
		d.Seen(fmt.Sprint("k", i), t0.Add(time.Duration(i)*time.Second))
	}
	if d.Len() != 3 {
		t.Fatalf("Len = %d", d.Len())
	}
	d.Seen("k3", t0.Add(3*time.Second)) // at cap: k0 (oldest) is evicted
	if d.Len() != 3 {
		t.Fatalf("Len after overflow = %d, want 3", d.Len())
	}
	if d.Seen("k0", t0.Add(4*time.Second)) {
		t.Fatal("evicted oldest key still deduped")
	}
	// k0's re-insert evicted k1; k2 and k3 survive.
	if !d.Seen("k2", t0.Add(5*time.Second)) || !d.Seen("k3", t0.Add(5*time.Second)) {
		t.Fatal("younger keys evicted before the oldest")
	}
	if d.Seen("k1", t0.Add(5*time.Second)) {
		t.Fatal("k1 should have been evicted by k0's re-insert")
	}
}

func TestMemoryBoundUnderScan(t *testing.T) {
	const max = 500
	d := NewDeduper(time.Hour, max)
	t0 := time.Unix(1_700_000_000, 0)
	for i := 0; i < 20*max; i++ {
		d.Seen(fmt.Sprint("scan", i), t0.Add(time.Duration(i)*time.Millisecond))
		if d.Len() > max {
			t.Fatalf("Len = %d exceeds cap %d", d.Len(), max)
		}
	}
	if live := len(d.fifo) - d.head; live > max {
		t.Fatalf("fifo holds %d live slots, cap %d", live, max)
	}
	if len(d.fifo) > 4*max {
		t.Fatalf("fifo backing array grew to %d", len(d.fifo))
	}
}

func TestStaleSlotDoesNotEvictFreshRecord(t *testing.T) {
	d := NewDeduper(time.Minute, 100)
	t0 := time.Unix(1_700_000_000, 0)
	d.Seen("a", t0)                    // slot 1, expires t0+1m
	d.Seen("a", t0.Add(2*time.Minute)) // expired: re-recorded as slot 2; slot 1 popped
	d.Seen("b", t0.Add(2*time.Minute)) // another insert pops nothing new
	if !d.Seen("a", t0.Add(2*time.Minute+30*time.Second)) {
		t.Fatal("fresh record for a evicted by its stale slot")
	}
	if d.Len() != 2 {
		t.Fatalf("Len = %d, want 2", d.Len())
	}
}

func TestCompaction(t *testing.T) {
	d := NewDeduper(time.Millisecond, 10)
	t0 := time.Unix(1_700_000_000, 0)
	for i := 0; i < 5000; i++ {
		d.Seen(fmt.Sprint("k", i), t0.Add(time.Duration(i)*time.Second))
	}
	if d.Len() != 1 || len(d.fifo) > 2048 {
		t.Fatalf("Len=%d fifo=%d head=%d", d.Len(), len(d.fifo), d.head)
	}
}

func TestKey(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.7")
	got := Key("aZ3kQ9xB2mLp", ip)
	sum := sha256.Sum256([]byte("203.0.113.7"))
	want := "aZ3kQ9xB2mLp|" + hex.EncodeToString(sum[:])[:16]
	if got != want {
		t.Fatalf("Key = %q, want %q", got, want)
	}
	if Key("aZ3kQ9xB2mLp", netip.MustParseAddr("203.0.113.8")) == got {
		t.Fatal("key ignores the ip")
	}
	if Key("bZ3kQ9xB2mLp", ip) == got {
		t.Fatal("key ignores the clip")
	}
}

// TestKeyIgnoresTheUserAgent is the §20 erratum 103 regression: the key is
// the ADDRESS, so a caller cannot mint a fresh dedupe slot by varying a header
// it writes itself. The signature no longer accepts a User-Agent at all, which
// is the strongest form of this assertion the language allows; what the test
// pins is that the same address always lands on the same key.
func TestKeyIgnoresTheUserAgent(t *testing.T) {
	ip := netip.MustParseAddr("198.51.100.9")
	first := Key("aZ3kQ9xB2mLp", ip)
	for i := 0; i < 3; i++ {
		if got := Key("aZ3kQ9xB2mLp", ip); got != first {
			t.Fatalf("Key is not a function of (clip, ip): %q vs %q", got, first)
		}
	}
}

func TestNewDeduperDefaultsCap(t *testing.T) {
	if d := NewDeduper(time.Minute, 0); d.max != defaultMax {
		t.Fatalf("max = %d", d.max)
	}
}
