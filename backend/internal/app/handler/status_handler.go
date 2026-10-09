package handler

import (
	"net/http"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/usecase/query"
)

// StatusHandler exposes:
//
//	GET /healthz           — liveness
//	GET /api/status        — delegated to GetBotStatusQuery
//	GET /api/active-config — current active strategy config(s)
//
// ActiveConfig serves two shapes from the same endpoint:
//   - no query        → JSON { "configs": { "<symbol>": {...} } } over every
//     symbol the holder currently knows about.
//   - ?symbol=<sym>   → JSON of just that symbol's StrategyConfig.
type StatusHandler struct {
	StatusQuery      *query.GetBotStatusQuery
	GetActiveConfigs func() map[string]*config.StrategyConfig
}

// Healthz は GET /healthz。常に 200 を返す。
func (h *StatusHandler) Healthz(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Status は GET /api/status。
func (h *StatusHandler) Status(w http.ResponseWriter, r *http.Request) {
	if h.StatusQuery == nil {
		WriteError(w, http.StatusServiceUnavailable, "status query not configured")
		return
	}
	v, err := h.StatusQuery.Execute(r.Context(), query.GetBotStatusInput{})
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, v)
}

// ActiveConfig is GET /api/active-config.
//
//   - no query        → all-symbol map { "configs": { sym: cfg } }
//   - ?symbol=<sym>   → single config
//
// 404 for: holder absent, unknown ?symbol, or empty all-symbol map.
func (h *StatusHandler) ActiveConfig(w http.ResponseWriter, r *http.Request) {
	if h.GetActiveConfigs == nil {
		WriteError(w, http.StatusNotFound, "no active config")
		return
	}
	configs := h.GetActiveConfigs()
	if sym := r.URL.Query().Get("symbol"); sym != "" {
		cfg, ok := configs[sym]
		if !ok || cfg == nil {
			WriteError(w, http.StatusNotFound, "no active config for symbol")
			return
		}
		WriteJSON(w, http.StatusOK, cfg)
		return
	}
	if len(configs) == 0 {
		WriteError(w, http.StatusNotFound, "no active config")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"configs": configs})
}
