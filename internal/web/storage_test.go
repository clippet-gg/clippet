package web

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"clippet/internal/cost"
	"clippet/internal/httpx"
)

// fixtureNow is the instant the fixture's handler runs at — the same clock the
// page reads, so a row seeded for "today" is the day the panel asks for.
func fixtureNow(f *fixture) time.Time { return time.Unix(f.now, 0) }

// panelOf is the Storage panel alone. The whole page is the wrong haystack for
// "did an amount appear": base.html's favicon is a data URI whose paths carry
// decimals of their own (`1.3`, `15.2`; until §20 erratum 301 `scale(0.00645)`),
// so a body-wide search for a number can find the icon.
func panelOf(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `<div class="panel" id="storage">`)
	if i < 0 {
		t.Fatalf("no Storage panel in the page:\n%s", sliceAround(body, "id=\"tokens\""))
	}
	rest := body[i:]
	if j := strings.Index(rest, `<div class="panel" id="tokens">`); j > 0 {
		rest = rest[:j]
	}
	return rest
}

// The settings page's "Storage" panel (ROADMAP #39 and #40, §20 errata 270 and
// 277): the quota first when there is one, then the two money rows, in the row
// grammar, instance-wide, an estimate — and never a 0.00 with no price behind
// it (unless the price IS zero, which `local` and R2's egress both are).

// countingFixture gives the fixture a counter over its own database, and
// returns the same counter so a test can put bytes in it.
func countingFixture(t *testing.T) (*fixture, *cost.Counter) {
	t.Helper()
	f := newFixture(t)
	f.served = cost.New(f.db, nil)
	return f, f.served
}

func TestSettingsStoragePanelUnpriced(t *testing.T) {
	f, _ := countingFixture(t)
	// A reading, no budget, no prices: the two figures are there and both cost
	// cells are the muted word, because an operator who typed no price must not
	// be shown a number that looks like a bill.
	f.budget = &httpx.StorageBudget{}
	f.budget.Set(25_585_254, 0) // 24.4 MiB
	if err := f.db.AddBytesServed(context.Background(), cost.Day(fixtureNow(f)), 1_288_490_189); err != nil {
		t.Fatal(err)
	}

	body := get(f.handler(session(f.owner)), "/settings").Body.String()
	mustContain(t, body, `<div class="panel" id="storage">`, "<h2>Storage</h2>", "never a bill",
		">stored<", ">served this month<", "24.4 MiB", "1.2 GiB")
	if n := strings.Count(body, "unpriced"); n != 2 {
		t.Errorf("want both cost cells reading `unpriced`, found %d:\n%s", n, sliceAround(body, "id=\"storage\""))
	}
	panel := panelOf(t, body)
	if strings.ContainsAny(panel, "≈") || strings.Contains(panel, "0.00") {
		t.Errorf("an unpriced instance rendered an amount:\n%s", panel)
	}
	if strings.Contains(panel, "data-storage-quota") {
		t.Errorf("a quota row with STORAGE_BUDGET_BYTES off:\n%s", panel)
	}
	assertNoInline(t, "settings", body)
}

func TestSettingsStoragePanelPricedAndBudgeted(t *testing.T) {
	f, _ := countingFixture(t)
	storage, egress := int64(15_000), int64(10_000) // 0.015 and 0.01 per GB
	f.storagePrice, f.egressPrice = &storage, &egress
	f.budget = &httpx.StorageBudget{}
	f.budget.Set(24*cost.GB, 200*cost.GB) // 12 % of the line
	if err := f.db.AddBytesServed(context.Background(), cost.Day(fixtureNow(f)), 3*cost.GB); err != nil {
		t.Fatal(err)
	}

	body := get(f.handler(session(f.owner)), "/settings").Body.String()
	mustContain(t, body,
		"≈ 0.36 / month", // 24 GB × 0.015
		"≈ 0.03",         // 3 GB × 0.01
		"data-storage-quota", ">quota<", "22.4 GiB", "186.3 GiB", "12 %")
	if panel := panelOf(t, body); strings.Contains(panel, "unpriced") {
		t.Errorf("a priced instance still says unpriced:\n%s", panel)
	}
	// The MERGED row (§20 erratum 277): the budget IS the quota now, and the
	// pair of numbers is printed once rather than in two rows.
	if panel := panelOf(t, body); strings.Contains(panel, ">budget<") {
		t.Errorf("the old budget row is still rendered beside the quota:\n%s", panel)
	}
	// Under the line, so the state cell stays the ordinary muted one.
	if panel := panelOf(t, body); strings.Contains(panel, "rs danger") {
		t.Errorf("12 %% of the quota painted the state cell red:\n%s", panel)
	}
	assertNoInline(t, "settings", body)
}

// TestSettingsStoragePanelQuotaIsFirstAndRedAtTheLine: the two claims of §20
// erratum 277's headline. The quota row comes BEFORE the money rows, because
// "is there room" is the question somebody opens this panel with; and at or
// past the line its state cell is red — web/STYLE.md's law is that red is LOSS,
// and a quota reached is the loss of the next upload.
func TestSettingsStoragePanelQuotaIsFirstAndRedAtTheLine(t *testing.T) {
	// EXACTLY at the line: a budget of N bytes is N bytes of room, and an
	// instance holding N has none left — the same boundary the hard cap
	// refuses at.
	f, _ := countingFixture(t)
	f.budget = &httpx.StorageBudget{}
	f.budget.Set(200*cost.GB, 200*cost.GB)
	panel := panelOf(t, get(f.handler(session(f.owner)), "/settings").Body.String())
	mustContain(t, panel, "data-storage-quota", "100 %", `class="rs danger"`)
	if i, j := strings.Index(panel, ">quota<"), strings.Index(panel, ">stored<"); i < 0 || j < 0 || i > j {
		t.Errorf("the quota row is not the first row of the panel (quota at %d, stored at %d):\n%s", i, j, panel)
	}

	// One byte under the line is not full: there is room for one more byte,
	// and the row says so in the ordinary colour.
	g, _ := countingFixture(t)
	g.budget = &httpx.StorageBudget{}
	g.budget.Set(200*cost.GB-1, 200*cost.GB)
	if under := panelOf(t, get(g.handler(session(g.owner)), "/settings").Body.String()); strings.Contains(under, "rs danger") {
		t.Errorf("one byte under the line painted the state cell red:\n%s", under)
	}

	// Past it the percentage keeps counting rather than pinning at 100.
	h, _ := countingFixture(t)
	h.budget = &httpx.StorageBudget{}
	h.budget.Set(500*cost.GB, 200*cost.GB)
	mustContain(t, panelOf(t, get(h.handler(session(h.owner)), "/settings").Body.String()),
		"250 %", `class="rs danger"`)
}

// TestSettingsStoragePanelLocalPresetPricesEverythingAtZero: STORAGE_PRICING=
// local is a real answer and not an absence — a box with its own disks has a
// provider bill of zero, and the owner asked for the zero to be printed. The hint says why, so nobody reads the 0.00 as a fault.
func TestSettingsStoragePanelLocalPresetPricesEverythingAtZero(t *testing.T) {
	f, _ := countingFixture(t)
	f.storagePricing = "local"
	f.budget = &httpx.StorageBudget{}
	f.budget.Set(24*cost.GB, 0)
	if err := f.db.AddBytesServed(context.Background(), cost.Day(fixtureNow(f)), 3*cost.GB); err != nil {
		t.Fatal(err)
	}
	panel := panelOf(t, get(f.handler(session(f.owner)), "/settings").Body.String())
	mustContain(t, panel, "local: no provider bill", "≈ 0.00 / month", "≈ 0.00")
	if strings.Contains(panel, "unpriced") {
		t.Errorf("a local instance still says unpriced:\n%s", panel)
	}
}

// TestSettingsStoragePanelPresetNamesItsListAndDate: a preset is somebody
// else's dated list, and the hint says whose and when — an estimate whose
// source is unstated is a number a reader has to trust blind. The served cell
// also proves the allowance is modelled where the panel shows it.
func TestSettingsStoragePanelPresetNamesItsListAndDate(t *testing.T) {
	f, _ := countingFixture(t)
	f.storagePricing = "b2"
	f.budget = &httpx.StorageBudget{}
	f.budget.Set(1_010*cost.GB, 0)
	// Two allowances, both modelled. Storage: B2's first 10 GB are free, so
	// 1 000 of the 1 010 GB are billed at 0.00695 — 6.95. Egress: 4 TB served
	// against 1 010 GB stored, of which 3× is free, so 970 GB is billable at
	// 0.01 — 9.70, not the 40.00 an unmodelled allowance would have printed.
	if err := f.db.AddBytesServed(context.Background(), cost.Day(fixtureNow(f)), 4_000*cost.GB); err != nil {
		t.Fatal(err)
	}
	mustContain(t, panelOf(t, get(f.handler(session(f.owner)), "/settings").Body.String()),
		"B2 list prices as of 2026-09-13 (USD)", "≈ 6.95 / month", "≈ 9.70")
}

// Two states in which the panel says less rather than something untrue: no
// counter wired at all (no panel), and a counter with no reading behind it yet
// (the seconds between boot and the sweeper's first measurement).
func TestSettingsStoragePanelSaysNothingUntrue(t *testing.T) {
	f := newFixture(t) // no counter
	if body := get(f.handler(session(f.owner)), "/settings").Body.String(); strings.Contains(body, `id="storage"`) {
		t.Error("the Storage panel rendered with nothing counting")
	}

	f2, _ := countingFixture(t)
	f2.budget = &httpx.StorageBudget{} // wired, never measured
	body := get(f2.handler(session(f2.owner)), "/settings").Body.String()
	// The stored row carries no figure at all and says why; the served row is a
	// different source and still answers (zero bytes served IS a measurement).
	panel := panelOf(t, body)
	mustContain(t, panel, "not measured yet", `<span class="mono">—</span>`,
		">served this month<", `<span class="mono">0 bytes</span>`)
}

// brokenServedStore fails every read, which is the one state the panel used to
// describe as "not measured yet" (§20 erratum 275).
type brokenServedStore struct{}

func (brokenServedStore) AddBytesServed(context.Context, string, int64) error { return errServedRead }
func (brokenServedStore) BytesServed(context.Context, string, string, string) (int64, int64, error) {
	return 0, 0, errServedRead
}

var errServedRead = errors.New("the served-bytes table could not be read")

// TestSettingsStoragePanelSaysWhenAReadFailed: a read that FAILED says
// `unavailable`, not `not measured yet` — the first is a fault and the second
// goes away by itself, and a panel that spells them alike sends the operator
// looking for the wrong thing. The page still renders, as it does when the
// session list cannot be read.
func TestSettingsStoragePanelSaysWhenAReadFailed(t *testing.T) {
	f := newFixture(t)
	f.served = cost.New(brokenServedStore{}, nil)
	f.budget = &httpx.StorageBudget{}
	f.budget.Set(25_585_254, 0) // the STORED half is a different source and still answers

	res := get(f.handler(session(f.owner)), "/settings")
	if res.Code != 200 {
		t.Fatalf("a failed served read took the settings page down: %d", res.Code)
	}
	panel := panelOf(t, res.Body.String())
	mustContain(t, panel, "unavailable", "24.4 MiB", `<span class="mono">—</span>`)
	if strings.Contains(panel, "not measured yet") {
		t.Errorf("a failed read still reads as never measured:\n%s", panel)
	}
}

// The drop folder's one line (ROADMAP #42 (b), §20 erratum 296): there only
// when the folder is on, naming the OWNER and never the directory.
func TestSettingsStoragePanelDropRow(t *testing.T) {
	f, _ := countingFixture(t)
	f.budget = &httpx.StorageBudget{}
	f.budget.Set(1024, 0)

	// Off is the default, and then there is no row: an empty one would say
	// something untrue about where a dropped file goes.
	panel := panelOf(t, get(f.handler(session(f.owner)), "/settings").Body.String())
	if strings.Contains(panel, "data-storage-drop") {
		t.Errorf("a drop row with DROP_FOLDER_OWNER unset:\n%s", panel)
	}

	f.dropFolderOwner = "carol"
	body := get(f.handler(session(f.owner)), "/settings").Body.String()
	panel = panelOf(t, body)
	for _, want := range []string{"data-storage-drop", ">drop folder<", "carol", ">on<"} {
		if !strings.Contains(panel, want) {
			t.Errorf("the drop row does not carry %q:\n%s", want, panel)
		}
	}
	// The DIRECTORY is the operator's disk layout. /healthz has refused to
	// leak it since erratum 276, and this page — which every signed-in account
	// can open — has no more business printing it.
	if strings.Contains(panel, "/media") || strings.Contains(panel, "STORAGE_DIR") {
		t.Errorf("the drop row leaked the media directory:\n%s", panel)
	}
	assertNoInline(t, "settings", body)
}

// TestSettingsStoragePanelIsTheOwners: the Storage panel is the instance
// owner's (§20 erratum 349). Another account's /settings carries the panel's
// head and the one sentence and NONE of its figures — not the quota, the
// stored or served rows, the price source, the drop folder's owner, nor the
// link to /setup — while the rest of its settings page is its own as before.
func TestSettingsStoragePanelIsTheOwners(t *testing.T) {
	f, _ := countingFixture(t)
	f.budget = &httpx.StorageBudget{}
	f.budget.Set(25_585_254, 190_000_000)
	body := get(f.handler(session(f.other)), "/settings").Body.String()
	panel := panelOf(t, body)
	mustContain(t, panel, "<h2>Storage</h2>", "data-storage-owner-only",
		"Only the account that set up this Clippet can see this.")
	mustNotContain(t, panel, ">stored<", ">served this month<", "data-storage-quota", "24.4 MiB",
		"never a bill", "href=\"/setup\"", "data-storage-drop")
	mustContain(t, body, `id="tokens"`, "Signed in as <strong>friend</strong>")

	own := panelOf(t, get(f.handler(session(f.owner)), "/settings").Body.String())
	mustContain(t, own, ">stored<", "24.4 MiB", "data-storage-quota", "href=\"/setup\"")
	mustNotContain(t, own, "data-storage-owner-only", "Only the account that set up")
}
