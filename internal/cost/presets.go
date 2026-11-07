package cost

import (
	"fmt"
	"strconv"

	"clippet/internal/config"
)

// The provider presets (STORAGE_PRICING; ROADMAP #40, DESIGN.md §20 erratum 277).
//
// A preset is a dated row of somebody else's LIST PRICES, typed out here by a
// person who read the provider's own pricing page on the date each row carries.
// It is not a lookup and never becomes one: this app asks no provider anything,
// holds no second credential and makes no outbound call for a number (§15,
// erratum 270). The whole feature is "you would have typed 0.023; here it is
// already typed, with the date it was read".
//
// The pages the rows were read from, by NAME (no URL literal may appear in a
// Go file — scripts/check.sh greps for one, comments included):
//
//   - r2: Cloudflare's "R2 pricing" page in the Cloudflare Developers docs.
//   - b2: Backblaze's "B2 Cloud Storage Pricing" page.
//   - s3: Amazon's "S3 pricing" page, US East (N. Virginia), S3 Standard.
//
// Every row is USD, because that is the currency those three lists quote. It is
// the ONE place this app names a currency, and it names it because it is
// repeating somebody else's number rather than reading the operator's (erratum
// 270's "currency-agnostic by construction" holds for the custom case, which is
// still the default). The hint on every priced surface says which list and when
// it was read, so a reader can tell an estimate from a quote.
//
// **Prices drift.** A row is right on its Read date and slowly wrong after it —
// providers announce changes a few times a decade, a few per cent at a time.
// An operator who wants today's number types it: STORAGE_PRICE_PER_GB_MONTH and
// EGRESS_PRICE_PER_GB each OVERRIDE their half of the preset (PricesOf), and the
// allowance rule stays, because an allowance is a billing rule and not a price.

// Preset is one provider's row.
type Preset struct {
	// Name is the STORAGE_PRICING value; Display is how a surface says it.
	Name, Display string
	// Currency is the currency the list is quoted in — "USD" for all three
	// cloud rows, and "" for `local`, which quotes nothing because nobody
	// bills it.
	Currency string
	// Read is the date a person read the page, `YYYY-MM-DD`. It is printed
	// beside every figure the row produces: an estimate from a dated list is
	// worth having, an undated one is a rumour.
	Read string
	// StoragePerGBMonth and EgressPerGB are MICROS per GB (config.PriceScale),
	// exactly as a typed price is held, so nothing downstream can tell a preset
	// price from an operator's own.
	StoragePerGBMonth, EgressPerGB int64
	// FreeStorageBytes is an ALWAYS-FREE storage allowance, subtracted before
	// the stored bytes are priced: R2 and B2 both give every account 10 GB a
	// month, for ever, and on a household instance that is most of the bill.
	//
	// Always-free only. AWS has no standing free S3 storage: what a new account
	// gets is a credit (up to 200 USD over six months on the Read date, in place
	// of the old twelve-month trial), and a credit is not an allowance — an
	// estimate that quietly assumed one nobody is still on would understate
	// every later bill, which is the direction that matters. So `s3` carries none.
	FreeStorageBytes int64
	// Allowance is what the provider does not bill for egress.
	Allowance EgressAllowance
	// Note is the one clause a surface adds after the list's name, or "".
	Note string
}

// EgressAllowance is a provider's free-egress rule, as code rather than as
// prose in a document nobody reads at the moment they need it.
//
// Two shapes cover the three cloud rows and are deliberately not a general
// scheme: a FLAT monthly allowance in bytes (AWS's always-free 100 GB of
// internet egress), and a MULTIPLE of what is stored (Backblaze's "free egress
// up to 3× your average monthly storage"). R2 needs neither — it bills no
// egress at all, which is a price of zero and not an allowance — and `local`
// bills nothing.
//
// The zero value is "no allowance", which is what a custom, preset-less
// instance has: an operator who typed a price typed the price they pay.
type EgressAllowance struct {
	// FreeBytes is a flat monthly allowance.
	FreeBytes int64
	// FreeMultipleOfStored is a multiple of the stored bytes. The provider's
	// rule is a multiple of the month's AVERAGE stored bytes and this app
	// holds one reading — what is stored now — so the current reading stands
	// in for the average. DEPLOY §31.5 says so in the accuracy table: on an
	// instance that grew or shrank a lot this month, the allowance it models
	// is the one the END of the month earns.
	FreeMultipleOfStored int64
}

// Billable is how much of `served` a provider actually charges for, given
// `stored` bytes to size a multiple-of-storage allowance against. Never
// negative: an allowance bigger than the month's egress makes the bill zero,
// not a credit.
func (a EgressAllowance) Billable(served, stored int64) int64 {
	if served <= 0 {
		return 0
	}
	free := a.FreeBytes
	if a.FreeMultipleOfStored > 0 && stored > 0 {
		if m := a.FreeMultipleOfStored * stored; m > free {
			free = m
		}
	}
	return billable(served, free)
}

// Any reports whether there is an allowance to describe at all.
func (a EgressAllowance) Any() bool { return a.FreeBytes > 0 || a.FreeMultipleOfStored > 0 }

// Words is the clause a surface puts after an egress price: `after 100 GB a
// month`, `after 3x what is stored`, or "" when the provider bills from the
// first byte. It is one clause and not a sentence, so a caller can put it
// where the grammar needs it.
func (a EgressAllowance) Words() string {
	switch {
	case a.FreeMultipleOfStored > 0:
		return "after " + strconv.FormatInt(a.FreeMultipleOfStored, 10) + "x what is stored"
	case a.FreeBytes > 0:
		return "after " + decimalGB(a.FreeBytes) + " a month"
	}
	return ""
}

// FreeStorageWords is the clause a surface puts after a storage price: `after
// 10 GB a month` on R2 and B2, or "" where the provider bills from the first
// byte. The counterpart of EgressAllowance.Words, and the same shape, because
// `admin doctor` prints the two halves side by side.
func (p Preset) FreeStorageWords() string {
	if p.FreeStorageBytes <= 0 {
		return ""
	}
	return "after " + decimalGB(p.FreeStorageBytes) + " a month"
}

// decimalGB renders a byte figure in the DECIMAL gigabytes a provider's
// allowance is quoted in (100 GB is 100 × 10^9 bytes on the invoice), not the
// GiB the product measures files in. It is only ever asked for a round
// allowance, so it prints no fraction.
func decimalGB(b int64) string { return strconv.FormatInt(b/GB, 10) + " GB" }

// presets is the table. Micros are written with their decimal beside them, and
// TestPresetPricesRoundTrip asserts each one renders back as exactly that
// decimal, so a comment cannot drift from the number it explains.
var presets = []Preset{
	{
		Name: config.PricingLocal, Display: "local", Currency: "", Read: "2026-09-13",
		StoragePerGBMonth: 0, EgressPerGB: 0,
		Note: "no provider bill",
	},
	{
		// Cloudflare R2: storage is billed, egress is not — the whole reason
		// R2 is on this list. The zero egress price is a real number and is
		// printed as 0.00, not hidden: erratum 270 made "a price of 0 IS a
		// number" the rule, and R2 is the case it was written for.
		Name: config.PricingR2, Display: "R2", Currency: "USD", Read: "2026-09-13",
		StoragePerGBMonth: 15_000,  // 0.015 per GB-month (R2 Standard)
		EgressPerGB:       0,       // egress to the internet is not billed
		FreeStorageBytes:  10 * GB, // the forever-free 10 GB-month
	},
	{
		// Backblaze B2, pay-as-you-go: 6.95 per TB-month, the first 10 GB of
		// storage always free, and free egress up to three times the average
		// monthly storage with 0.01 per GB past that.
		Name: config.PricingB2, Display: "B2", Currency: "USD", Read: "2026-09-13",
		StoragePerGBMonth: 6_950,   // 0.00695 per GB-month = 6.95 per TB-month
		EgressPerGB:       10_000,  // 0.01 per GB past the allowance
		FreeStorageBytes:  10 * GB, // "first 10GB storage is always free"
		Allowance:         EgressAllowance{FreeMultipleOfStored: 3},
	},
	{
		// Amazon S3 Standard, US East (N. Virginia), first 50 TB a month; and
		// AWS's always-free 100 GB a month of internet egress, then 0.09 per
		// GB. Both figures are the region's list price on the Read date; the
		// higher tiers (450 TB at 0.022, above 500 TB at 0.021, egress under
		// 0.09 above 150 TB) are not modelled, and an instance big enough to
		// reach one is not estimating its bill from this panel. No free
		// STORAGE: AWS's new-account credit is not a standing allowance.
		Name: config.PricingS3, Display: "S3 Standard", Currency: "USD", Read: "2026-09-13",
		StoragePerGBMonth: 23_000, // 0.023 per GB-month
		EgressPerGB:       90_000, // 0.09 per GB past the allowance
		Allowance:         EgressAllowance{FreeBytes: 100 * GB},
	},
}

// PresetByName finds a row. The empty name is the CUSTOM case and is not a
// row: it has no date, no currency and no allowance, and its prices are
// whatever the operator typed.
func PresetByName(name string) (Preset, bool) {
	for _, p := range presets {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

// Presets is the table, for a test and for a document generator. It is a copy:
// nothing may edit a list price at run time.
func Presets() []Preset { return append([]Preset(nil), presets...) }

// Source is how a surface names where a figure's prices came from: `R2 list
// prices as of 2026-09-13 (USD)`, `local: no provider bill`, or "" for the
// custom case, where the operator's own two numbers need no attribution.
func (p Preset) Source() string {
	if p.Note != "" {
		return p.Display + ": " + p.Note
	}
	s := fmt.Sprintf("%s list prices as of %s", p.Display, p.Read)
	if p.Currency != "" {
		s += " (" + p.Currency + ")"
	}
	return s
}
