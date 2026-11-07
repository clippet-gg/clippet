// Package cost owns the storage-cost line: the per-day served-bytes counter
// that feeds it and the arithmetic over the operator's two optional prices
// (ROADMAP #39, DESIGN.md §20 erratum 270).
//
// One package for both halves, because they are one feature and nothing else
// wants either half: the counter exists so the cost line can be computed, and
// the arithmetic is meaningless without it. Keeping them together means no
// other package has to know about UTC days, flush windows, decimal gigabytes
// or how a price is scaled — /healthz, `admin stats` and the settings page each
// ask for the same three numbers and the same two amounts, and cannot disagree
// about any of them.
//
// **What this package never does**, and what §15 and ROADMAP #39 both say it
// must never grow: it asks no provider what anything costs, looks no price up,
// makes no outbound call of any kind, and holds nothing about a viewer. The
// prices are two numbers the operator typed into their own `.env`; the bytes
// are aggregate totals per UTC day. Every figure it produces is an ESTIMATE and
// every surface that prints one says so.
package cost

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"clippet/internal/config"
)

// GB is the unit a storage provider bills in: a DECIMAL gigabyte, 10^9 bytes.
//
// It is deliberately not the GiB (2^30) the rest of this app measures files in,
// and the difference is 7.4 % — enough to make an estimate wrong in the
// direction that matters. R2, B2 and S3 all price "per GB" and all mean 10^9,
// so a cost line computed in GiB would quietly under-state every bill by that
// margin. The BYTES the surfaces print stay in the IEC units the rest of the
// product uses (`24.4 MiB`); only the money is computed in these.
const GB = 1_000_000_000

// Amount is a currency amount in HUNDREDTHS of the operator's own currency
// unit — the scale a price list is read in, and the scale every surface prints.
//
// It is an integer, so the number a surface renders is the number the
// arithmetic produced: rounding happens exactly once, where the amount is
// computed, and never again in a formatter. It carries no currency, because the
// app has never been told one and never asks (§15); the operator knows what
// they are paying in.
type Amount int64

// String renders the amount with two decimals and no symbol: `0.01`, `12.40`.
// Amounts are never negative (bytes and prices are both non-negative), and a
// negative one would be a bug rather than a credit, so it is rendered plainly
// rather than specially.
func (a Amount) String() string {
	if a < 0 {
		return "-" + Amount(-a).String()
	}
	return fmt.Sprintf("%d.%02d", int64(a)/100, int64(a)%100)
}

// PriceString renders a price in MICROS back as the decimal an operator would
// have typed: `0.015`, `0.36`, `2`, `0`. Trailing zeros of the fraction go, so
// `0.5` and `0.500000` — the same price, which is why config.parsePrice pads —
// print alike, and a whole price prints with no point at all.
//
// It exists for `admin doctor`, which reports what the app PARSED rather than
// what the variable said (§20 erratum 275): a price is scaled here, so it is
// un-scaled here too, and no other package has to know that PriceScale is a
// million. It is never a cost — Amount is the only thing that renders one.
func PriceString(micros int64) string {
	if micros < 0 {
		return "-" + PriceString(-micros)
	}
	whole := micros / config.PriceScale
	frac := micros % config.PriceScale
	if frac == 0 {
		return strconv.FormatInt(whole, 10)
	}
	return strconv.FormatInt(whole, 10) + "." +
		strings.TrimRight(fmt.Sprintf("%0*d", config.MaxPriceFractionDigits, frac), "0")
}

// Prices is what this instance is priced at: millionths of a currency unit per
// GB, or nil for unset. Unset is ABSENT and not zero — a surface shows no cost
// line at all rather than a 0.00 somebody could read as "storage here is free".
//
// Two things can fill it (§20 erratum 277): a PRESET, which is a dated row of a
// provider's list prices held in presets.go and never fetched, and the
// operator's own typed prices, which override the preset's matching half. The
// preset's free-allowance rule is not a price and is not overridden by one.
type Prices struct {
	// StoragePerGBMonth is what one GB costs to keep for a month; EgressPerGB
	// what one GB costs to send. Either may be set without the other — a
	// friend on R2 pays for storage and nothing for egress, which is the
	// worked example DEPLOY §31 is written around.
	StoragePerGBMonth, EgressPerGB *int64
	// Preset is the row STORAGE_PRICING named, or the zero value for the
	// custom case. HasPreset() is the question to ask.
	Preset Preset
	// StorageTyped and EgressTyped record that the operator's own variable
	// supplied that half — which, with a preset, means it OVERRODE the list
	// price. `admin doctor` is the only reader: an operator who typed a price
	// over a preset must be able to see that it took.
	StorageTyped, EgressTyped bool
}

// PricesOf resolves a configuration into prices: the preset's row first (when
// STORAGE_PRICING names one), then each typed price over its own half.
func PricesOf(c config.Config) Prices {
	var p Prices
	if pre, ok := PresetByName(c.StoragePricing); ok {
		p.Preset = pre
		storage, egress := pre.StoragePerGBMonth, pre.EgressPerGB
		p.StoragePerGBMonth, p.EgressPerGB = &storage, &egress
	}
	if c.StoragePriceMicros != nil {
		p.StoragePerGBMonth, p.StorageTyped = c.StoragePriceMicros, true
	}
	if c.EgressPriceMicros != nil {
		p.EgressPerGB, p.EgressTyped = c.EgressPriceMicros, true
	}
	return p
}

// HasPreset reports whether a provider row is behind these prices.
func (p Prices) HasPreset() bool { return p.Preset.Name != "" }

// Any reports whether the instance is priced at all — whether any cost line
// exists anywhere.
func (p Prices) Any() bool { return p.StoragePerGBMonth != nil || p.EgressPerGB != nil }

// Source names where the prices came from, for the one hint each surface
// carries: `R2 list prices as of 2026-09-13 (USD)`, `local: no provider bill`,
// or "" when the operator typed their own and there is nothing to attribute.
func (p Prices) Source() string {
	if !p.HasPreset() {
		return ""
	}
	return p.Preset.Source()
}

// Stored is what `bytes` of stored objects cost for a month, or nil when no
// storage price is set.
//
// A preset's ALWAYS-FREE storage allowance comes off first (R2's and B2's 10 GB
// a month), which on a household instance is most of the bill — and, like the
// egress allowance, it survives an operator typing their own price over the
// preset's, because it is the provider's billing rule and not a number anybody
// retyped.
func (p Prices) Stored(bytes int64) *Amount {
	return at(billable(bytes, p.Preset.FreeStorageBytes), p.StoragePerGBMonth)
}

// billable takes a free allowance off a figure, never below zero: an allowance
// bigger than the thing it applies to makes the bill nothing, not a credit.
func billable(bytes, free int64) int64 {
	if bytes <= free {
		return 0
	}
	return bytes - free
}

// Served is what `served` bytes sent to viewers cost, or nil when no egress
// price is set.
//
// `stored` sizes the preset's free allowance where the provider's rule is a
// multiple of what is kept (B2's 3×); it is ignored by a flat allowance and by
// the custom case, which has none. It is a parameter rather than a second
// method because every caller that has one figure has the other, and a cost
// line computed without the allowance would over-state a B2 bill by the whole
// of it.
func (p Prices) Served(served, stored int64) *Amount {
	return at(p.Preset.Allowance.Billable(served, stored), p.EgressPerGB)
}

// at is the whole arithmetic: bytes → decimal GB → × price → an amount rounded
// to the nearest hundredth, half away from zero.
//
// It is done in float64 on purpose, and safely: both operands are integers
// float64 holds EXACTLY (a byte count stays under 2^53 up to 9 PB, and a price
// in micros is at most 10^12), so the only error is the one relative rounding
// of the product — about one part in 10^16, or fifteen orders of magnitude
// below the hundredth this rounds to. An integer form would have to defend a
// bytes × micros product against overflow at petabyte scale for a precision
// nothing can observe.
//
// A price of exactly 0 is a real answer and produces 0: the operator said
// "this costs me nothing", which is not the same as saying nothing.
func at(bytes int64, priceMicros *int64) *Amount {
	if priceMicros == nil {
		return nil
	}
	if bytes < 0 {
		bytes = 0
	}
	hundredths := float64(bytes) / GB * float64(*priceMicros) / config.PriceScale * 100
	a := Amount(roundHalfAway(hundredths))
	return &a
}

// roundHalfAway rounds to the nearest integer, halves away from zero — what a
// person doing the same sum on paper does, and what makes a table of test cases
// readable.
//
// It is spelled out rather than delegated to math.Round for a reason beyond
// taste: `float64(int64(v + 0.5))` is NOT math.Round, and the difference is a
// double rounding at the one input where adding 0.5 is itself inexact
// (0.49999999999999994 + 0.5 == 1 in binary floating point, so this returns 1
// where math.Round returns 0). That input cannot arise here — an amount in
// hundredths comes from a byte count times a price, never from a value chosen
// to sit a single ulp under a half — and the arithmetic above says why the
// margin is fifteen orders of magnitude. The rule of the money is written down
// beside the money, exactness included.
func roundHalfAway(v float64) float64 {
	if v < 0 {
		return -roundHalfAway(-v)
	}
	return float64(int64(v + 0.5))
}

// Day is the UTC day a moment belongs to, `YYYY-MM-DD` — the key of one
// `bytes_served` row.
func Day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Month is the UTC calendar month of a moment, `YYYY-MM`. Every day string in
// that month begins with it, which is what lets the unflushed window be summed
// by prefix without parsing a date.
func Month(t time.Time) string { return t.UTC().Format("2006-01") }

// MonthRange is the half-open day range of a moment's UTC calendar month:
// `[start, end)`, both `YYYY-MM-DD`. Half-open rather than a `LIKE` prefix so
// the read is an index range over the primary key whatever SQLite's LIKE
// optimisation is doing, and so a month's last day needs no arithmetic.
func MonthRange(t time.Time) (start, end string) {
	u := t.UTC()
	first := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return Day(first), Day(first.AddDate(0, 1, 0))
}

// RetentionDays is how long a day's row is kept: 400 days, a year plus a
// margin. The surfaces show today and this calendar month; the history behind
// them is what makes "is this month unusual" a question an operator can answer
// at all, and a year of one row a day is 400 rows.
const RetentionDays = 400

// PruneBefore is the day the sweeper deletes rows older than: `now` minus the
// retention window.
func PruneBefore(now time.Time) string { return Day(now.UTC().AddDate(0, 0, -RetentionDays)) }
