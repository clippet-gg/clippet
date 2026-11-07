package sweeper

import (
	"testing"
	"time"
)

// raceSlack is how much longer this package's WALL-CLOCK deadlines are under
// `go test -race` (a harness fix after Gate 11).
//
// The eleventh gate's first check.sh failed in TestStartBlocksAndTicks — "Start
// never destroyed the expired clip" — where a 20 ms-interval sweeper had five
// seconds to destroy ONE clip; the same test passed alone, in 247 s, on the same
// tree. The box was carrying two lane builders and a third container's `go test`
// at the time, and the race detector instruments every one of the sweeper's
// operations. It is not a product fault and the fix may not be a product change,
// so what moves is the allowance: the assertions are identical, the patience is
// not. A fake clock would be the better answer and is written down as one (the
// tick loop would have to take an injectable ticker, which IS a product change).
const raceSlack = 12

// deadline scales a timing allowance for the build it is running in.
func deadline(d time.Duration) time.Duration {
	if raceDetector {
		return d * raceSlack
	}
	return d
}

// TestDeadlineScalesUnderRaceOnly: the build tag really decides, in both
// builds — check.sh runs this package twice, once plain and once with -race, so
// both halves are executed on every gate.
func TestDeadlineScalesUnderRaceOnly(t *testing.T) {
	got := deadline(time.Second)
	switch {
	case raceDetector && got != raceSlack*time.Second:
		t.Fatalf("deadline(1s) = %v under -race, want %v", got, raceSlack*time.Second)
	case !raceDetector && got != time.Second:
		t.Fatalf("deadline(1s) = %v in a plain build, want 1s", got)
	}
	if raceSlack < 2 {
		t.Fatal("raceSlack must actually lengthen something")
	}
}
