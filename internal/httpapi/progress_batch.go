package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"clippet/internal/db"
	"clippet/internal/httpx"
)

// batchProgress is the body of GET /api/clips/progress (§20 erratum 116): the
// §8.4 DTO of every id that resolved, keyed by clip id.
//
// An id that did not resolve — unknown, or a `deleting` tombstone — is ABSENT
// from the map. That is the batch spelling of the single route's 404, and it
// carries the same rule: a poller treats a missing key as an ERROR ("this clip
// is gone"), never as "done". Spelling it as an absence rather than as a
// per-id error object keeps the client's read one lookup and keeps this route
// from becoming a nicer existence oracle than the one it replaces.
type batchProgress struct {
	Progress map[string]ProgressDTO `json:"progress"`
}

// progressBatch answers one poll for a whole page of the library instead of
// one poll per unfinished row (ROADMAP #15).
//
// Every fact it returns is a fact `GET /api/clips/{id}/progress` already gives
// the same caller — same public route class, same DTO, same three terminal
// signals — so this widens nothing. What it changes is the cost: a ShareX
// watch-folder drop of forty files made one open tab issue ~40 requests a
// second against a box that was simultaneously running ffmpeg. It is public
// like its sibling, and unlike its sibling it carries the `view` limiter,
// because one call does up to MaxProgressIDs times the work of one poll — the
// same reasoning erratum 87 applied to the collection GET.
func (h *handler) progressBatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ids, ok := parseProgressIDs(w, r.URL.Query().Get("ids"))
	if !ok {
		return
	}
	if h.d.DB == nil {
		writeDBError(w, r, errNoDB)
		return
	}
	rows, err := h.d.DB.ProgressRows(r.Context(), ids)
	if err != nil {
		writeDBError(w, r, err)
		return
	}
	out := batchProgress{Progress: make(map[string]ProgressDTO, len(rows))}
	for _, row := range rows {
		dto := dbProgress(row)
		if p, found := h.d.Worker.Progress(row.ClipID); found {
			dto = registryProgress(baseProgress(row.ClipID, row.State, row.UpdatedAt), p)
		}
		// The tier states, on the same terms and through the same function as the
		// single route (§20 errata 116, 205): a library row that is already
		// playable while a better copy encodes says so here too.
		dto.Tiers = progressTiers(db.TierStatesOf(row.State, row.FullState, row.LightState, row.HasFull, row.HasLight))
		out.Progress[row.ClipID] = dto
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// parseProgressIDs validates `?ids=a,b,c`: 1..MaxProgressIDs well-formed clip
// ids, duplicates collapsed (the answer is a map, so asking twice can only be
// a client bug and must not cost a second row).
//
// A malformed id is a 400 rather than a silent omission: on the single route a
// bad id is a 404 the caller cannot miss, and a batch that quietly dropped it
// would report "this clip is gone" for what is really a typo.
func parseProgressIDs(w http.ResponseWriter, raw string) ([]string, bool) {
	if strings.TrimSpace(raw) == "" {
		writeInvalid(w, "ids must be a comma-separated list of clip ids")
		return nil, false
	}
	parts := strings.Split(raw, ",")
	if len(parts) > db.MaxProgressIDs {
		writeInvalid(w, "ids must name at most "+strconv.Itoa(db.MaxProgressIDs)+" clips")
		return nil, false
	}
	seen := make(map[string]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		id := strings.TrimSpace(p)
		if !validID(id) {
			writeInvalid(w, "ids must be a comma-separated list of clip ids")
			return nil, false
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, true
}

// dbProgress builds the §8.4 DTO from the database alone — the fallback both
// progress routes share when the worker's registry has no entry (after a
// restart, or for a job not yet claimed). It lives here, taking a
// db.ProgressRow, so the two routes cannot drift: the batch reads its rows in
// one query and the single route builds one row from its own two reads, and
// from there they are the same code.
func dbProgress(row db.ProgressRow) ProgressDTO {
	dto := baseProgress(row.ClipID, row.State, row.UpdatedAt)
	if row.Stage != "" {
		dto.Stage = row.Stage
	}
	if row.JobUpdatedAt > row.UpdatedAt {
		dto.UpdatedAt = rfc3339(row.JobUpdatedAt)
	}
	switch row.State {
	case db.ClipReady:
		dto.Percent = percentReady
		dto.Stage = "done"
	case db.ClipProcessing:
		dto.Percent = percentQueued
		if row.Checkpointed {
			dto.Percent = percentCheckpointed
		}
	case db.ClipFailed:
		dto.Percent = percentQueued
		dto.Error = row.FailureReason
	default:
		dto.Percent = percentQueued
	}
	return dto
}
