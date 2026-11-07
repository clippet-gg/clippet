package cost

import (
	"testing"
	"time"
)

func micros(n int64) *int64 { return &n }

// The arithmetic, as a table (ROADMAP #39, §20 erratum 270). Two things are
// pinned here and nothing else in the product pins either: the unit is the
// DECIMAL gigabyte a provider bills in, and the rounding happens once, to the
// hundredth, half away from zero.
func TestTheArithmetic(t *testing.T) {
	cases := []struct {
		what  string
		bytes int64
		price int64 // micros per GB
		want  string
	}{
		{"nothing stored costs nothing", 0, 15_000, "0.00"},
		{"a free provider is a real answer", 500 * GB, 0, "0.00"},
		{"one GB at R2's storage price", GB, 15_000, "0.02"}, // 0.015 → 0.02
		{"100 GB at R2's storage price", 100 * GB, 15_000, "1.50"},
		{"1 TB of egress at 0.01", 1000 * GB, 10_000, "10.00"},
		{"24.4 MiB is a rounding-to-zero case", 25_585_254, 15_000, "0.00"},
		{"a GiB is NOT a GB: 1 GiB at 0.015 is more than 0.015", 1 << 30, 15_000, "0.02"},
		{"half a hundredth rounds AWAY from zero", GB, 5_000, "0.01"}, // 0.005 → 0.01
		{"just under half a hundredth rounds down", GB, 4_999, "0.00"},
		{"a big month at a real egress price", 3_500 * GB, 10_000, "35.00"},
		{"petabyte scale still lands on the hundredth", 1_000_000 * GB, 15_000, "15000.00"},
	}
	for _, tc := range cases {
		p := Prices{StoragePerGBMonth: micros(tc.price), EgressPerGB: micros(tc.price)}
		if got := p.Stored(tc.bytes); got == nil || got.String() != tc.want {
			t.Errorf("%s: Stored(%d) at %d micros = %v, want %s", tc.what, tc.bytes, tc.price, got, tc.want)
		}
		// No preset, so no allowance: the stored figure is ignored and the
		// month's egress is billed from the first byte (§20 erratum 277).
		if got := p.Served(tc.bytes, 7*GB); got == nil || got.String() != tc.want {
			t.Errorf("%s: Served(%d) at %d micros = %v, want %s", tc.what, tc.bytes, tc.price, got, tc.want)
		}
	}
}

// Unset is ABSENT, never zero — the rule every surface of this feature is built
// on, and the one an operator who has typed no price depends on.
func TestAnUnsetPriceHasNoAmountAtAll(t *testing.T) {
	var none Prices
	if none.Any() {
		t.Error("no price configured must not read as priced")
	}
	if a := none.Stored(500 * GB); a != nil {
		t.Errorf("an unset storage price produced an amount: %v", a)
	}
	if a := none.Served(500*GB, 0); a != nil {
		t.Errorf("an unset egress price produced an amount: %v", a)
	}
	// One of the two is the R2 case: storage costs something, egress is free of
	// charge and the operator has not priced it.
	half := Prices{StoragePerGBMonth: micros(15_000)}
	if !half.Any() {
		t.Error("one price set must read as priced")
	}
	if a := half.Stored(GB); a == nil || a.String() != "0.02" {
		t.Errorf("the priced half: %v", a)
	}
	if a := half.Served(GB, 0); a != nil {
		t.Errorf("the unpriced half produced %v, want nothing", a)
	}
	// A price of zero is the operator SAYING zero, which is not the same thing.
	zero := Prices{EgressPerGB: micros(0)}
	if a := zero.Served(500*GB, 0); a == nil || a.String() != "0.00" {
		t.Errorf("a configured zero price must produce 0.00, got %v", a)
	}
}

func TestAmountRendersTwoDecimalsAndNoSymbol(t *testing.T) {
	for _, tc := range []struct {
		in   Amount
		want string
	}{{0, "0.00"}, {1, "0.01"}, {9, "0.09"}, {10, "0.10"}, {100, "1.00"}, {1234, "12.34"}, {-5, "-0.05"}} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Amount(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}

// The day and month keys are UTC, and the month is the half-open range the
// database reads with.
func TestDayAndMonthAreUTC(t *testing.T) {
	// 23:30 on the 11th in a zone nine hours ahead is already the 11th at 14:30
	// UTC; and 00:30 on the 1st in a zone five hours behind is still the last
	// day of the previous month in UTC.
	east := time.FixedZone("east", 9*3600)
	west := time.FixedZone("west", -5*3600)
	if got := Day(time.Date(2026, 9, 12, 8, 30, 0, 0, east)); got != "2026-09-11" {
		t.Errorf("Day in an eastern zone = %q", got)
	}
	if got := Day(time.Date(2026, 10, 1, 0, 30, 0, 0, west)); got != "2026-10-01" {
		t.Errorf("Day in a western zone = %q", got)
	}
	if got := Month(time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)); got != "2026-09" {
		t.Errorf("Month = %q", got)
	}
	start, end := MonthRange(time.Date(2026, 12, 14, 12, 0, 0, 0, time.UTC))
	if start != "2026-12-01" || end != "2027-01-01" {
		t.Errorf("MonthRange over a year boundary = %q..%q", start, end)
	}
	start, end = MonthRange(time.Date(2028, 2, 5, 0, 0, 0, 0, time.UTC))
	if start != "2028-02-01" || end != "2028-03-01" {
		t.Errorf("MonthRange over a leap February = %q..%q", start, end)
	}
	// Every day of a month sorts inside its own range, which is what makes the
	// half-open comparison an index range rather than arithmetic.
	last := Day(time.Date(2028, 2, 29, 23, 0, 0, 0, time.UTC))
	if !(last >= start && last < end) {
		t.Errorf("the last day of the month (%s) is outside %q..%q", last, start, end)
	}
}

func TestPruneBeforeIsTheRetentionWindow(t *testing.T) {
	now := time.Date(2026, 9, 11, 6, 0, 0, 0, time.UTC)
	got := PruneBefore(now)
	// COUNTED BY HAND, not recomputed with the expression under test (§20
	// erratum 275): 2026-09-11 back 365 days is 2025-09-11 (no February 29 in
	// between), and 35 more is 2025-08-07 — 4 days to the 7th of September
	// plus 31 for August. A `want` built from AddDate(0, 0, -RetentionDays)
	// asserted only that the function calls the function it calls.
	const want = "2025-08-07"
	if RetentionDays != 400 {
		t.Fatalf("RetentionDays is %d: the date above was counted for 400", RetentionDays)
	}
	if got != want {
		t.Errorf("PruneBefore = %q, want %q", got, want)
	}
	if got >= Day(now) {
		t.Errorf("PruneBefore (%q) would delete today (%q)", got, Day(now))
	}
}

// TestPriceString: micros come back as the decimal an operator would have
// typed, trailing zeros and all removed (§20 erratum 275). It is what
// `admin doctor`'s `prices` row prints, so it must never look like an amount.
func TestPriceString(t *testing.T) {
	for _, tc := range []struct {
		micros int64
		want   string
	}{
		{0, "0"},
		{15_000, "0.015"},
		{10_000, "0.01"},
		{500_000, "0.5"},
		{1, "0.000001"},
		{1_000_000, "1"},
		{12_340_000, "12.34"},
		{1_000_000_000_000, "1000000"},
	} {
		if got := PriceString(tc.micros); got != tc.want {
			t.Errorf("PriceString(%d) = %q, want %q", tc.micros, got, tc.want)
		}
	}
}
