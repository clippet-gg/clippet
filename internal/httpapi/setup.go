package httpapi

// setup.go — POST /api/setup/check, the `Check now` behind /setup (ROADMAP
// #45, DESIGN.md §20 erratum 297).
//
// It is `store.Probe` against the store this instance is configured for RIGHT
// NOW — Ping, put one small object under `{S3_KEY_PREFIX}doctor/`, delete it —
// which is the identical check `admin doctor`'s `bucket` row performs. One
// implementation, two surfaces.
//
// Its outcome is RECORDED in the shared `httpx.StorageHealth` that the boot
// ping loop and `/readyz` write and that `/healthz` and the setup page's first
// panel read (§20 erratum 297 (i)). That is what makes the page's `last check`
// row mean what it says: this button is, on a running instance, usually the
// only thing that ever takes a second reading.
//
// Three things it deliberately is not:
//
//   - It takes NO input. There is no endpoint field, no bucket field and no
//     credential field on the page it serves, so there is nothing to SSRF with
//     and nothing to validate that the operator did not already put in `.env`
//     and boot with. §15's list of outbound destinations is unchanged by this
//     route, which is why check.sh's one-call-site stage is untroubled by it.
//   - It adds NO rate limiter (the no-new-limiter rule, ROADMAP #35). A hundred presses
//     are one probe: the answer is single-flighted and then cached for ten
//     seconds, which is a cache and not a limit — nobody is ever refused, they
//     are told what the last ten seconds already proved.
//   - It prints NO sentence of the store's own. The body carries the bounded
//     class and nothing else: the doctor's line interpolates the bucket and
//     the endpoint, and `safe()` does not redact an access key id.
import (
	"context"
	"net/http"
	"sync"
	"time"

	"clippet/internal/auth"
	"clippet/internal/httpx"
	"clippet/internal/store"
)

// msgOwnerOnly is the refusal a signed-in account that is not the instance's
// owner gets from the owner's routes (§20 erratum 349) — the sentence the
// owner's pages print, word for word (internal/web ownerOnlyLine).
const msgOwnerOnly = "Only the account that set up this Clippet can see this."

// setupCheckTTL is how long one probe's answer is reused. Ten seconds: long
// enough that a person clicking while they read cannot spend a Class A
// operation per click, short enough that "paste the block, recreate, press it
// again" answers about the stack they just recreated.
const setupCheckTTL = 10 * time.Second

// setupCheckDTO is the whole answer. `state` is the closed §20 erratum 31
// vocabulary — `ok`, `unreachable`, `timeout`, `denied`, `bucket missing`,
// `unavailable` — and `checked_at` is when the probe behind it actually ran,
// which is how a caller can see that two presses in one second were one probe.
type setupCheckDTO struct {
	OK        bool   `json:"ok"`
	State     string `json:"state"`
	CheckedAt string `json:"checked_at"`
}

// setupProbe is the single-flight cache. It holds no configuration and no
// error text: only the bounded word and the instant it was true.
type setupProbe struct {
	mu      sync.Mutex
	valid   bool
	ok      bool
	state   string
	checked time.Time
	// running is non-nil while a probe is in flight and is closed when it
	// finishes. A second caller waits on it rather than starting a second
	// probe, which is what makes a burst of presses one put and one delete.
	running chan struct{}
}

// get answers from the cache, waits for a probe already in flight, or runs
// one. now is the handler clock.
//
// A negative age is treated as stale for the reason regCache's is: the clock
// this reads can step backwards, and a cache with no expiry is the one failure
// mode worth spending a comparison on.
func (p *setupProbe) get(ctx context.Context, now time.Time, run func(context.Context) error) (bool, string, time.Time) {
	p.mu.Lock()
	if p.valid {
		if age := now.Sub(p.checked); age >= 0 && age < setupCheckTTL {
			ok, state, at := p.ok, p.state, p.checked
			p.mu.Unlock()
			return ok, state, at
		}
	}
	if ch := p.running; ch != nil {
		p.mu.Unlock()
		select {
		case <-ch:
			// Whatever that probe learned is this caller's answer too.
			p.mu.Lock()
			ok, state, at, valid := p.ok, p.state, p.checked, p.valid
			p.mu.Unlock()
			if valid {
				return ok, state, at
			}
			return false, store.PingUnavailable, now
		case <-ctx.Done():
			return false, store.PingTimeout, now
		}
	}
	ch := make(chan struct{})
	p.running = ch
	p.mu.Unlock()
	err := run(ctx)
	at := time.Now()
	p.mu.Lock()
	p.valid, p.checked = true, at
	if err != nil {
		p.ok, p.state = false, store.ProbeClass(err)
	} else {
		p.ok, p.state = true, "ok"
	}
	ok, state := p.ok, p.state
	p.running = nil
	p.mu.Unlock()
	close(ch)
	return ok, state, at

}

// setupCheck is the handler. Session only: it is the page's own button, and a
// capture token must not be able to spend a storage operation.
func (h *handler) setupCheck(w http.ResponseWriter, r *http.Request) {
	// The owner's button, like the owner's page it sits on (§20 erratum 349):
	// another signed-in account is refused before any probe runs or any cache
	// is read.
	p, _ := auth.FromContext(r.Context())
	owner, err := h.d.DB.IsOwner(r.Context(), p.User.ID)
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	if !owner {
		httpx.SetLogField(r, "reject", "not_owner")
		httpx.WriteError(w, http.StatusForbidden, "not_owner", msgOwnerOnly)
		return
	}
	if h.d.Store == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "storage_unavailable",
			"this instance has no store to ask")
		return
	}
	ok, state, at := h.setupProbe.get(r.Context(), h.now(), func(ctx context.Context) error {
		err := store.Probe(ctx, h.d.Store, h.d.Cfg.S3.KeyPrefix)
		// The outcome goes into the SHARED last-ping cache, the way /readyz's
		// live ping does (§20 erratum 297 (i)). This is the only live probe
		// this page performs, and without this line it was thrown away: the
		// boot loop returns for good after its first success, nothing polls
		// /readyz (the compose healthcheck reads /healthz, which only Gets),
		// and so panel one — the state cell AND `last check` — stayed frozen
		// at the boot ping for the life of the process, saying `ok` about a
		// store this button had just watched fail. It is written inside the
		// run closure rather than beside the DTO so that only a probe that
		// actually RAN writes it: a cached answer is not a new measurement,
		// and `checked_at` would then disagree with `last check`.
		h.d.Storage.Set(err)
		return err
	})
	// The full error stays with the operator's surfaces — the log line the
	// store itself wrote, and `admin doctor` — exactly as /healthz's does.
	httpx.WriteJSON(w, http.StatusOK, setupCheckDTO{
		OK: ok, State: state, CheckedAt: at.UTC().Format(time.RFC3339Nano),
	})
}
