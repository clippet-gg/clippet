package cost

import (
	"strings"
	"testing"

	"clippet/internal/config"
)

// TestPresetTableMatchesTheAcceptedNames: the loader validates STORAGE_PRICING
// against config.PricingPresets and this package holds the rows, so the two
// lists must be the same list. A name accepted at boot with no row behind it
// would price an instance at zero and say nothing; a row nobody can select is
// dead code.
func TestPresetTableMatchesTheAcceptedNames(t *testing.T) {
	rows := Presets()
	if len(rows) != len(config.PricingPresets) {
		t.Fatalf("%d preset rows for %d accepted names: %v vs %v",
			len(rows), len(config.PricingPresets), names(rows), config.PricingPresets)
	}
	for _, name := range config.PricingPresets {
		p, ok := PresetByName(name)
		if !ok {
			t.Errorf("STORAGE_PRICING accepts %q and no preset row has it", name)
			continue
		}
		if p.Read == "" || len(p.Read) != len("2026-09-13") {
			t.Errorf("preset %q has no read date (%q): an undated list price is a rumour", name, p.Read)
		}
		if p.Display == "" {
			t.Errorf("preset %q has no display name", name)
		}
		if p.StoragePerGBMonth < 0 || p.EgressPerGB < 0 {
			t.Errorf("preset %q has a negative price", name)
		}
	}
	// The empty name is the CUSTOM case and must never resolve to a row: it is
	// the shipped default, and a row behind it would price every instance.
	if p, ok := PresetByName(""); ok {
		t.Errorf("the unset STORAGE_PRICING resolved to preset %q", p.Name)
	}
	if p, ok := PresetByName("gcs"); ok {
		t.Errorf("an unknown name resolved to preset %q", p.Name)
	}
	// The table is a copy: nothing may edit a list price at run time.
	rows[0].StoragePerGBMonth = 999
	if again, _ := PresetByName(rows[0].Name); again.StoragePerGBMonth == 999 {
		t.Error("Presets() handed out the live table")
	}
}

func names(rows []Preset) []string {
	out := make([]string, len(rows))
	for i, p := range rows {
		out[i] = p.Name
	}
	return out
}

// TestPresetPricesRoundTrip: every list price renders back as the decimal
// written beside it in presets.go, so the comment and the number cannot drift.
// These are the figures a person read off four pricing pages on the Read date
// and they are the whole claim this feature makes.
func TestPresetPricesRoundTrip(t *testing.T) {
	want := map[string][2]string{
		config.PricingLocal: {"0", "0"},
		config.PricingR2:    {"0.015", "0"},
		config.PricingB2:    {"0.00695", "0.01"},
		config.PricingS3:    {"0.023", "0.09"},
	}
	for name, pair := range want {
		p, ok := PresetByName(name)
		if !ok {
			t.Fatalf("no preset %q", name)
		}
		if got := PriceString(p.StoragePerGBMonth); got != pair[0] {
			t.Errorf("preset %s storage = %s, want %s", name, got, pair[0])
		}
		if got := PriceString(p.EgressPerGB); got != pair[1] {
			t.Errorf("preset %s egress = %s, want %s", name, got, pair[1])
		}
	}
}

// TestPresetArithmeticAtThreeSizes: each preset's two amounts at an empty
// instance, a small one and a big one — the numbers a person can check on
// paper, including the allowance boundary for the two providers that have one.
func TestPresetArithmeticAtThreeSizes(t *testing.T) {
	cases := []struct {
		preset               string
		stored, served       int64
		wantStored, wantServ string
		why                  string
	}{
		// local: everything is free, and the zero is PRINTED (the owner's ask)
		// rather than absent, because "no provider bill" is a
		// measurement and "no price configured" is not.
		{config.PricingLocal, 0, 0, "0.00", "0.00", "an empty local box"},
		{config.PricingLocal, 500 * GB, 2_000 * GB, "0.00", "0.00", "a full local box still bills nothing"},

		// R2: storage priced past the forever-free 10 GB, egress free of
		// charge at any volume.
		{config.PricingR2, 0, 0, "0.00", "0.00", "an empty R2 bucket"},
		{config.PricingR2, 10 * GB, 0, "0.00", "0.00", "exactly the free 10 GB-month"},
		{config.PricingR2, 11 * GB, 0, "0.02", "0.00", "one GB past the free tier, at 0.015"},
		{config.PricingR2, 24 * GB, 3 * GB, "0.21", "0.00", "14 chargeable GB of a 24 GB bucket"},
		{config.PricingR2, 2_010 * GB, 40_000 * GB, "30.00", "0.00", "40 TB of egress on R2 is still free"},

		// B2: 6.95 per TB-month past its own free 10 GB, and egress free up
		// to 3x what is stored.
		{config.PricingB2, 0, 0, "0.00", "0.00", "an empty B2 bucket"},
		{config.PricingB2, 10 * GB, 0, "0.00", "0.00", "exactly the free 10 GB of storage"},
		{config.PricingB2, 1_010 * GB, 3_030 * GB, "6.95", "0.00", "exactly 3x stored is the last free byte"},
		{config.PricingB2, 1_010 * GB, 3_031 * GB, "6.95", "0.01", "one GB past the allowance is one GB billed"},
		{config.PricingB2, 1_010 * GB, 13_030 * GB, "6.95", "100.00", "10 TB past the allowance at 0.01"},
		// The egress allowance is a MULTIPLE of what is stored, so an instance
		// that stores nothing has none: the whole month's egress is billable.
		{config.PricingB2, 0, 500 * GB, "0.00", "5.00", "nothing stored means no allowance to earn"},

		// S3 Standard: 0.023 per GB-month from the FIRST byte (AWS's new-account
		// credit is not a standing allowance and is deliberately not modelled), and the flat
		// 100 GB a month of free internet egress whatever is stored.
		{config.PricingS3, 0, 0, "0.00", "0.00", "an empty S3 bucket"},
		{config.PricingS3, 10 * GB, 0, "0.23", "0.00", "no free storage tier on S3"},
		{config.PricingS3, 100 * GB, 100 * GB, "2.30", "0.00", "exactly the free 100 GB of egress"},
		{config.PricingS3, 100 * GB, 101 * GB, "2.30", "0.09", "one GB past the free egress tier"},
		{config.PricingS3, 1_000 * GB, 1_100 * GB, "23.00", "90.00", "1 TB billable at 0.09"},
	}
	for _, tc := range cases {
		p := PricesOf(config.Config{StoragePricing: tc.preset})
		stored, served := p.Stored(tc.stored), p.Served(tc.served, tc.stored)
		if stored == nil || served == nil {
			t.Fatalf("%s (%s): a preset must price both halves, got %v / %v", tc.preset, tc.why, stored, served)
		}
		if stored.String() != tc.wantStored {
			t.Errorf("%s (%s): stored %d bytes = %s, want %s", tc.preset, tc.why, tc.stored, stored, tc.wantStored)
		}
		if served.String() != tc.wantServ {
			t.Errorf("%s (%s): served %d bytes against %d stored = %s, want %s",
				tc.preset, tc.why, tc.served, tc.stored, served, tc.wantServ)
		}
	}
}

// TestATypedPriceOverridesThePresetAndTheAllowanceStays: the override law of
// §20 erratum 277. A price is a number the operator can correct — list prices
// drift, and a region or a plan may quote another one — but an ALLOWANCE is the
// provider's billing rule, not a number anybody retyped, so it survives.
func TestATypedPriceOverridesThePresetAndTheAllowanceStays(t *testing.T) {
	own := int64(20_000) // 0.02 a GB, over B2's 0.01
	p := PricesOf(config.Config{StoragePricing: config.PricingB2, EgressPriceMicros: &own})
	if !p.HasPreset() {
		t.Fatal("the preset is gone after an override")
	}
	if *p.EgressPerGB != own {
		t.Errorf("egress = %d micros, want the typed %d", *p.EgressPerGB, own)
	}
	if !p.EgressTyped || p.StorageTyped {
		t.Errorf("override flags: storage=%v egress=%v, want false/true", p.StorageTyped, p.EgressTyped)
	}
	if *p.StoragePerGBMonth != 6_950 {
		t.Errorf("the un-overridden half changed: %d micros", *p.StoragePerGBMonth)
	}
	// 4 TB served against 1 TB stored: 3 TB is free, 1 TB is billed — at the
	// operator's 0.02, not B2's 0.01.
	if a := p.Served(4_000*GB, 1_000*GB); a == nil || a.String() != "20.00" {
		t.Errorf("the typed price over the preset's allowance = %v, want 20.00", a)
	}
	// The other direction: a typed STORAGE price over a preset whose FREE
	// STORAGE allowance must survive it — 100 GB less R2's free 10, at 0.03.
	storage := int64(30_000)
	q := PricesOf(config.Config{StoragePricing: config.PricingR2, StoragePriceMicros: &storage})
	if a := q.Stored(100 * GB); a == nil || a.String() != "2.70" {
		t.Errorf("the typed storage price over R2's = %v, want 2.70", a)
	}
	if a := q.Served(500*GB, 100*GB); a == nil || a.String() != "0.00" {
		t.Errorf("R2's zero egress survived an unrelated override as %v", a)
	}
}

// TestNoPresetNoAllowanceNoSource: an instance with two typed prices and no
// STORAGE_PRICING is exactly what erratum 270 shipped — no allowance, no
// currency, no attribution.
func TestNoPresetNoAllowanceNoSource(t *testing.T) {
	storage, egress := int64(15_000), int64(10_000)
	p := PricesOf(config.Config{StoragePriceMicros: &storage, EgressPriceMicros: &egress})
	if p.HasPreset() || p.Source() != "" {
		t.Errorf("typed prices must attribute nothing: preset=%q source=%q", p.Preset.Name, p.Source())
	}
	if p.Preset.Allowance.Any() || p.Preset.FreeStorageBytes != 0 {
		t.Error("typed prices must carry no free allowance, for egress or for storage")
	}
	if a := p.Stored(GB); a == nil || a.String() != "0.02" {
		t.Errorf("a typed storage price billed %v for one GB, want 0.02 from the first byte", a)
	}
	if a := p.Served(GB, 10_000*GB); a == nil || a.String() != "0.01" {
		t.Errorf("a typed egress price billed %v for one GB, want 0.01 whatever is stored", a)
	}
}

// TestSourceNamesTheListAndTheDate: what every priced surface's hint says.
func TestSourceNamesTheListAndTheDate(t *testing.T) {
	for _, tc := range []struct{ preset, want string }{
		{config.PricingLocal, "local: no provider bill"},
		{config.PricingR2, "R2 list prices as of 2026-09-13 (USD)"},
		{config.PricingB2, "B2 list prices as of 2026-09-13 (USD)"},
		{config.PricingS3, "S3 Standard list prices as of 2026-09-13 (USD)"},
	} {
		if got := PricesOf(config.Config{StoragePricing: tc.preset}).Source(); got != tc.want {
			t.Errorf("%s source = %q, want %q", tc.preset, got, tc.want)
		}
	}
}

// TestAllowanceWords: the clause `admin doctor` puts after an egress price.
func TestAllowanceWords(t *testing.T) {
	for _, tc := range []struct {
		a    EgressAllowance
		want string
	}{
		{EgressAllowance{}, ""},
		{EgressAllowance{FreeBytes: 100 * GB}, "after 100 GB a month"},
		{EgressAllowance{FreeMultipleOfStored: 3}, "after 3x what is stored"},
	} {
		if got := tc.a.Words(); got != tc.want {
			t.Errorf("Words() = %q, want %q", got, tc.want)
		}
	}
	// Never negative, and never a credit: an allowance bigger than the month
	// makes the bill zero and stops there.
	if got := (EgressAllowance{FreeBytes: 100 * GB}).Billable(10*GB, 0); got != 0 {
		t.Errorf("Billable under the allowance = %d, want 0", got)
	}
	if got := (EgressAllowance{FreeMultipleOfStored: 3}).Billable(-5, 10); got != 0 {
		t.Errorf("Billable of a negative month = %d, want 0", got)
	}
}

// TestEveryPresetNameIsLowerCaseAndShort: the value goes in a `.env` line and
// the loader lower-cases what it reads, so a row named `R2` could never be
// selected.
func TestEveryPresetNameIsLowerCaseAndShort(t *testing.T) {
	for _, p := range Presets() {
		if p.Name != strings.ToLower(p.Name) {
			t.Errorf("preset name %q is not lower case: config's oneOf lower-cases the value before it compares", p.Name)
		}
	}
}
