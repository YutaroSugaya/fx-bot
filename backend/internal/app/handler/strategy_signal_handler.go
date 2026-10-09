package handler

import (
	"net/http"

	"fx-bot/backend/internal/app/livesignal"
)

// StrategySignalHandler serves GET /api/strategy/signal?symbol= — the bot's
// most recent LIVE evaluation for a symbol (strategy outcome + risk-gate
// outcome). Unlike the frontend reproduction, this is the bot's real decision,
// so the dashboard can show "entry met" only when the bot would actually enter.
type StrategySignalHandler struct {
	// Get returns the latest snapshot for a symbol, or nil if none yet.
	Get func(symbol string) *livesignal.Snapshot
}

func (h *StrategySignalHandler) Signal(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Get == nil {
		WriteError(w, http.StatusServiceUnavailable, "strategy signal not configured")
		return
	}
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		WriteError(w, http.StatusBadRequest, "symbol required")
		return
	}
	snap := h.Get(symbol)
	if snap == nil {
		WriteError(w, http.StatusNotFound, "no live signal for symbol yet")
		return
	}
	WriteJSON(w, http.StatusOK, snap)
}
