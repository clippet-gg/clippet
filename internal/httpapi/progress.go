package httpapi

import (
	"errors"
	"net/http"
	"time"

	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/worker"
)

// Percent values of the DB fallback (§8.4).
const (
	percentQueued       = 0
	percentCheckpointed = 98
	percentReady        = 100
)

// progress answers the §8.4 DTO: the worker's in-memory registry first, the
// DB (clip state + the latest TRANSCODE job's stage) when the registry has no
// entry.
// Unknown or deleting clips are 404 — a poller must never read that as done.
func (h *handler) progress(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c, ok := h.lookupClip(w, r)
	if !ok {
		return
	}
	base := baseProgress(c.ID, c.State, c.UpdatedAt)
	// The per-tier states, on both answers below (§20 erratum 205). They come
	// from the ROW and not from the registry, because the two jobs they describe
	// — the Full and the Light — have no registry entry at all: the clip they
	// belong to is `ready` throughout, and §8.4's registry is about the clip's
	// own encode. This is how a watch page that is already playing finds out its
	// better tier has landed.
	base.Tiers = progressTiers(db.TierStatesOfClip(c))
	if p, found := h.d.Worker.Progress(c.ID); found {
		// A live registry entry answers on its own: it is the running job's
		// own view, so the job row it would be merged with is a second read
		// that could only tell it what it already knows.
		httpx.WriteJSON(w, http.StatusOK, registryProgress(base, p))
		return
	}
	// The clip's TRANSCODE job, by kind — never merely its newest job. Since
	// Phase 4 the newest job of a ready clip is routinely a notify, a trim or
	// a gif, and this DTO's subject is the clip's own encode: its stage, its
	// updated_at and its checkpoint (§5's partition; §20 erratum 89).
	row := db.ProgressRow{ClipID: c.ID, State: c.State, UpdatedAt: c.UpdatedAt, FailureReason: c.FailureReason}
	job, err := h.d.DB.LatestJobOfKind(r.Context(), c.ID, db.KindTranscode)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		writeDBError(w, r, err)
		return
	}
	if err == nil {
		row.Stage, row.JobUpdatedAt, row.Checkpointed = job.Stage, job.UpdatedAt, job.CheckpointJSON != nil
	}
	dto := dbProgress(row)
	dto.Tiers = base.Tiers
	httpx.WriteJSON(w, http.StatusOK, dto)
}

// progressTiers renders db's reconciled tier states as the polling DTO. Both
// progress routes call it — the single one from a whole clip row, the batched one
// from its projection — so erratum 116's promise holds for the tiers too: a
// batched poll and a single poll of one clip cannot answer differently.
func progressTiers(rows []db.TierStateRow) []ProgressTierDTO {
	out := make([]ProgressTierDTO, 0, len(rows))
	for _, t := range rows {
		out = append(out, ProgressTierDTO{Tier: t.Tier, State: t.State})
	}
	return out
}

// baseProgress is the DTO before either source has spoken: the clip's own
// state and timestamp, and the stage a job starts at.
func baseProgress(clipID, state string, updatedAt int64) ProgressDTO {
	return ProgressDTO{ID: clipID, State: state, Stage: db.JobStageQueued, UpdatedAt: rfc3339(updatedAt)}
}

// registryProgress layers the worker's live registry entry over that base.
// Both progress routes call it with the same base (§20 erratum 116), so a
// batched poll and a single poll of one clip can never answer differently.
func registryProgress(dto ProgressDTO, p worker.Progress) ProgressDTO {
	if p.State != "" {
		dto.State = p.State
	}
	if p.Stage != "" {
		dto.Stage = p.Stage
	}
	dto.Percent = clampPercent(p.Percent)
	dto.Indeterminate = p.Indeterminate
	if p.Error != "" {
		e := p.Error
		dto.Error = &e
	}
	if !p.UpdatedAt.IsZero() {
		dto.UpdatedAt = p.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return dto
}

func clampPercent(p int) int {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}
