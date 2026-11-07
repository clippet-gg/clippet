package httpapi

import (
	"errors"
	"net/http"

	"clippet/internal/clientip"
	"clippet/internal/db"
	"clippet/internal/httpx"
	"clippet/internal/views"
)

// view is the player's first-play beacon (§8.6): 204 either way, counted
// only when the (clip, ip) key has not been seen inside the dedupe window --
// the ADDRESS alone, never the caller-supplied User-Agent (§20 erratum 103).
// Rate-limited per IP by the router.
func (h *handler) view(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c, ok := h.lookupClip(w, r)
	if !ok {
		return
	}
	ip := clientip.Resolve(r, h.d.Cfg.TrustedProxies)
	key := views.Key(c.ID, ip)
	if h.d.Views.Seen(key, h.now()) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.d.DB.IncrementViews(r.Context(), c.ID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeNotFound(w)
			return
		}
		writeDBError(w, r, err)
		return
	}
	httpx.SetLogField(r, "view_counted", true)
	w.WriteHeader(http.StatusNoContent)
}
