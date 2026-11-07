package httpapi

import (
	"context"
	"net/http"
	"time"

	"clippet/internal/db"
	"clippet/internal/httpx"
)

const healthTimeout = 2 * time.Second

// queueDTO is /healthz's queue block. The first two fields are Phase 1's; the
// three after them are additive (§20 erratum 145) and close the gap that made
// every worker failure mode invisible — a `failed` backlog does not appear in
// queued/running at all, and `queued:3` cannot tell "three uploads just
// arrived" from "nothing has been claimed for an hour".
//
// Every field is a COUNT or an AGE IN SECONDS. Nothing here names a clip, a
// user, a key or a configured value: /healthz is anonymous (§15), and the
// alerts DEPLOY §24 defines need numbers, not identities.
type queueDTO struct {
	Queued  int `json:"queued"`
	Running int `json:"running"`
	// Failed24h counts jobs that reached `failed` in the last 24 hours.
	Failed24h int `json:"failed_24h"`
	// OldestQueuedAgeS is how long the oldest CLAIMABLE queued job has been
	// claimable (0 when none is): climbing while running is 0 means nothing
	// is claiming.
	OldestQueuedAgeS int64 `json:"oldest_queued_age_s"`
	// LongestRunningAgeS is how long the longest-running job has been running
	// (0 when none is): past TRANSCODE_HARD_TIMEOUT it is a stuck row.
	LongestRunningAgeS int64 `json:"longest_running_age_s"`
}

type sweepDTO struct {
	Destroyed int64 `json:"destroyed"`
	Errors    int64 `json:"errors"`
}

// repairDTO is the automatic stuck-job repair's lifetime counters (ROADMAP #12,
// §20 erratum 231). Five counts and nothing else: no clip, no kind, no job id
// and — deliberately — not JOBS_UNSTICK_AFTER itself, because /healthz is
// anonymous and names no configured value (§15). `admin doctor` is where the
// setting is reported, to somebody who is already inside the container.
//
// `repaired` climbing is not an error condition: it means the app is keeping
// §11's promise without a restart. `failed` climbing is the one to alert on —
// those are jobs whose attempts ran out, i.e. clips their owners are being told
// about (DEPLOY §24.6).
type repairDTO struct {
	Repaired int64 `json:"repaired"`
	Released int64 `json:"released"`
	Requeued int64 `json:"requeued"`
	Failed   int64 `json:"failed"`
	Errors   int64 `json:"errors"`
}

// budgetDTO is the storage line (ROADMAP #24, §20 errata 157 and 277). It
// carries a measurement and a verdict and NOTHING actionable: no route reads
// THIS object, no upload consults it, and `over` being true changes nothing the
// app does. (An instance that armed the cap with STORAGE_BUDGET_HARD=1 refuses
// from a LIVE database read at the upload, never from here, so this object's
// keys, shape and meaning are identical on a capped instance and an uncapped
// one — an anonymous probe says no more about the configuration than it ever
// did.) It is present only when STORAGE_BUDGET_BYTES is set AND the sweeper has
// taken a reading, so an absent key is never read as "you are fine".
type budgetDTO struct {
	Used   int64 `json:"used"`
	Budget int64 `json:"budget"`
	Over   bool  `json:"over"`
}

// storageUsageDTO is the storage-cost line's MEASUREMENTS (ROADMAP #39, §20
// erratum 270): what this instance is storing, and what it has sent to viewers
// today and this calendar month.
//
// **No cost appears here, ever.** The two amounts this object carried in the
// first cut were removed before the feature shipped: /healthz is ANONYMOUS and
// is reachable through the tunnel, and a cost beside the byte count it was
// computed from is the operator's configured per-GB price, recoverable by one
// division. §15 says this route exposes no configured value; a price is one.
// The costs live on the SIGNED-IN settings panel and in `admin stats`, which is
// somebody already inside the container. The three byte counts stay, on the
// precedent `storage_budget.used` set (erratum 157) — they are aggregates that
// name no clip, no account and nobody who watched anything.
//
// **The key is `storage_usage`, not `storage`.** `storage` has been the CACHED
// STORE-PING RESULT — a string, `ok` / `pending` / `error: …` — since Phase 1,
// and it is what the compose healthcheck and DEPLOY §24's alerts read; renaming
// it to make room for an object would break every one of them silently, for a
// key name. `storage_usage` sits beside `storage_budget`, which is what the
// object is a sibling of.
//
// EVERY field is optional, and an absent key is never a zero — the same rule
// budgetDTO above keeps: `stored_bytes` is absent until the sweeper has taken
// its reading, and the served counts are absent when no counter is wired or its
// one read failed.
type storageUsageDTO struct {
	StoredBytes      *int64 `json:"stored_bytes,omitempty"`
	ServedTodayBytes *int64 `json:"served_today_bytes,omitempty"`
	ServedMonthBytes *int64 `json:"served_month_bytes,omitempty"`
}

type healthDTO struct {
	OK      bool             `json:"ok"`
	Version string           `json:"version"`
	FFmpeg  string           `json:"ffmpeg"`
	DB      string           `json:"db"`
	Storage string           `json:"storage"`
	Queue   queueDTO         `json:"queue"`
	Sweep   *sweepDTO        `json:"sweep,omitempty"`
	Repair  *repairDTO       `json:"repair,omitempty"`
	Budget  *budgetDTO       `json:"storage_budget,omitempty"`
	Usage   *storageUsageDTO `json:"storage_usage,omitempty"`
	UptimeS int64            `json:"uptime_s"`
}

type healthFailDTO struct {
	OK bool   `json:"ok"`
	DB string `json:"db"`
}

// healthz is process + DB liveness (§8.1). The storage field is the CACHED
// last-Ping result — never a live call — so storage trouble cannot fail the
// compose healthcheck.
func (h *handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	if err := h.pingDB(ctx); err != nil {
		httpx.SetLogField(r, "db_error", err.Error())
		httpx.WriteJSON(w, http.StatusInternalServerError, healthFailDTO{OK: false, DB: dbHealth(err)})
		return
	}
	q, err := h.d.DB.QueueHealth(ctx, time.Now().Unix())
	if err != nil {
		httpx.SetLogField(r, "db_error", err.Error())
		httpx.WriteJSON(w, http.StatusInternalServerError, healthFailDTO{OK: false, DB: dbHealth(err)})
		return
	}
	var sweep *sweepDTO
	if h.d.SweepTotals != nil {
		destroyed, errs := h.d.SweepTotals()
		sweep = &sweepDTO{Destroyed: destroyed, Errors: errs}
	}
	var repair *repairDTO
	if h.d.Repairs != nil {
		r := h.d.Repairs()
		repair = &repairDTO{Repaired: r.Repaired, Released: r.Released, Requeued: r.Requeued,
			Failed: r.Failed, Errors: r.Errors}
	}
	var budget *budgetDTO
	if h.d.Budget != nil {
		if b := h.d.Budget.Get(); b.Known && b.Budget > 0 {
			budget = &budgetDTO{Used: b.Used, Budget: b.Budget, Over: b.Over}
		}
	}
	httpx.WriteJSON(w, http.StatusOK, healthDTO{
		OK:      true,
		Usage:   h.storageUsage(ctx, r),
		Version: h.d.Version,
		FFmpeg:  h.d.Cfg.Transcode.FFmpegVersion,
		DB:      "ok",
		Storage: h.d.Storage.Get(),
		Queue: queueDTO{Queued: q.Queued, Running: q.Running, Failed24h: q.Failed24h,
			OldestQueuedAgeS: q.OldestQueuedAgeS, LongestRunningAgeS: q.LongestRunningAgeS},
		Sweep:   sweep,
		Repair:  repair,
		Budget:  budget,
		UptimeS: int64(time.Since(h.d.Started).Seconds()),
	})
}

// storageUsage builds the storage-cost line's measurements, or nil when there
// is nothing true to say (ROADMAP #39, §20 erratum 270).
//
// It costs ONE statement — an index range over at most 31 rows of the ≤400-row
// `bytes_served` table — and no scan of `clips`: the stored figure is the
// sweeper's CACHED reading, for exactly the reason erratum 157 gave when it
// cached it, which is that a probe compose polls every ten seconds must not
// grow an aggregate over the clip table.
//
// It reads NO price and renders no amount: this route is anonymous (§15), and
// an amount beside the bytes it came from is the operator's own per-GB price
// one division away. internal/cost is deliberately not imported here.
//
// A served read that FAILS omits the served keys and logs; it never fails the
// probe. /healthz is liveness, and an accounting number must not be able to
// tell a container supervisor the process is dead.
func (h *handler) storageUsage(ctx context.Context, r *http.Request) *storageUsageDTO {
	var dto storageUsageDTO
	said := false
	if h.d.Budget != nil {
		if b := h.d.Budget.Get(); b.Known {
			used := b.Used
			dto.StoredBytes, said = &used, true
		}
	}
	if h.d.Served != nil {
		today, month, err := h.d.Served.Totals(ctx, h.now())
		if err != nil {
			httpx.SetLogField(r, "served_bytes_error", err.Error())
		} else {
			dto.ServedTodayBytes, dto.ServedMonthBytes, said = &today, &month, true
		}
	}
	if !said {
		return nil
	}
	return &dto
}

type readyDTO struct {
	OK      bool   `json:"ok"`
	DB      string `json:"db,omitempty"`
	Storage string `json:"storage,omitempty"`
}

// readyz is DB + a LIVE store.Ping (2s), whose outcome is recorded in the
// shared StorageHealth cache (§8.7).
func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	if err := h.pingDB(ctx); err != nil {
		httpx.SetLogField(r, "db_error", err.Error())
		httpx.WriteJSON(w, http.StatusServiceUnavailable, readyDTO{OK: false, DB: dbHealth(err), Storage: h.d.Storage.Get()})
		return
	}
	if h.d.Store == nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, readyDTO{OK: false, DB: "ok", Storage: "pending"})
		return
	}
	err := h.d.Store.Ping(ctx)
	h.d.Storage.Set(err)
	if err != nil {
		httpx.SetLogField(r, "storage_error", err.Error())
		httpx.WriteJSON(w, http.StatusServiceUnavailable, readyDTO{OK: false, DB: "ok", Storage: h.d.Storage.Get()})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, readyDTO{OK: true})
}

// dbHealth renders a database failure for an ANONYMOUS prober: the bounded
// class db.HealthClass names, never the driver's message (§20 erratum 99).
// A modernc/SQLite error can carry DATA_DIR's file path or the failing
// statement, and §15 says /healthz exposes no configuration values — the
// same reasoning erratum 31 applied to the storage half, which has printed
// only `error: <class>` ever since. The full error goes to the access log
// through SetLogField at every call site.
func dbHealth(err error) string { return "error: " + db.HealthClass(err) }

func (h *handler) pingDB(ctx context.Context) error {
	if h.d.DB == nil {
		return errNoDB
	}
	return h.d.DB.Ping(ctx)
}

type constError string

func (e constError) Error() string { return string(e) }

const errNoDB = constError("database not configured")
