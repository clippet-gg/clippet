package httpx

import "sync"

// StorageBudget caches the instance's known object total against the advisory
// STORAGE_BUDGET_BYTES line (ROADMAP #24, §20 erratum 157). The sweeper
// measures and writes it once a tick; /healthz and the library page read it
// and therefore never run an aggregate of their own — the liveness probe that
// compose calls every ten seconds must not grow a scan of `clips`.
//
// Nothing in the binary reads THIS to refuse work, and that stayed true when
// the opt-in cap arrived (STORAGE_BUDGET_HARD=1, §20 erratum 277): the cap
// reads the database live, at the one place a clip is created, because a cache
// that is up to one SWEEP_INTERVAL old would go on refusing for a minute after
// somebody deleted a clip to make room. This value is the DISPLAYED reading —
// the WARN, /healthz's `storage_budget` and the settings panel's quota row — and
// it exists so an operator paying for a bucket finds out that they crossed their
// own line from a warning rather than from an invoice. README's "no tiers, no
// per-user quotas" stays literally true either way.
type StorageBudget struct {
	mu     sync.Mutex
	known  bool
	used   int64
	budget int64
}

// BudgetStatus is one reading. Known is false until the first measurement, so
// a caller can tell "nothing measured yet" from "measured, and it is zero".
// Over is false whenever no budget is configured (Budget == 0 = off).
type BudgetStatus struct {
	Known  bool
	Used   int64
	Budget int64
	Over   bool
}

// Set records a measurement against the configured budget.
func (b *StorageBudget) Set(used, budget int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.known, b.used, b.budget = true, used, budget
}

// Get returns the last measurement.
func (b *StorageBudget) Get() BudgetStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BudgetStatus{
		Known:  b.known,
		Used:   b.used,
		Budget: b.budget,
		Over:   b.known && b.budget > 0 && b.used > b.budget,
	}
}
