package handler

import (
	"log/slog"
	"net/http"
	"os"
)

// LLMDecisionHandler serves the autonomous LLM trade loop's latest per-symbol decision snapshot so
// the dashboard can show what Claude decided (trade/no_trade + side + TP/SL + reason), the cycle
// stage, and the current playbook. It streams the file the scheduler writes each cycle
// (runtime/llm_decision_status.json); a missing file returns an empty snapshot (not an error).
//
//	GET  /api/llm-decision          → latest snapshot
//	POST /api/llm-decision/trigger  → manually re-run the whole decision loop now
type LLMDecisionHandler struct {
	StatusPath string
	// Trigger kicks off one decision cycle for ALL pairs immediately (the dashboard's
	// manual "全ペア再判断" button). It returns fast (the cycle runs detached); a non-nil
	// error means a cycle is already running. nil → /trigger returns 503.
	Trigger func() error
	Logger  *slog.Logger
}

func (h *LLMDecisionHandler) Status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h == nil || h.StatusPath == "" {
		_, _ = w.Write([]byte(`{"by_symbol":{}}`))
		return
	}
	b, err := os.ReadFile(h.StatusPath)
	if err != nil {
		// Not yet written (bot just started / loop disabled) — empty snapshot, not a 500.
		_, _ = w.Write([]byte(`{"by_symbol":{}}`))
		return
	}
	_, _ = w.Write(b)
}

// TriggerNow handles POST /api/llm-decision/trigger — the manual "全ペア再判断" button.
// It returns 202 immediately (the cycle runs in the background; the dashboard's poll
// shows the refreshed snapshot when it lands), or 409 if a cycle is already running.
func (h *LLMDecisionHandler) TriggerNow(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	if h == nil || h.Trigger == nil {
		WriteError(w, http.StatusServiceUnavailable, "llm decision trigger not configured")
		return
	}
	if err := h.Trigger(); err != nil {
		if h.Logger != nil {
			h.Logger.Info("llm_decision_trigger_busy", "remote", r.RemoteAddr, "err", err)
		}
		WriteJSON(w, http.StatusConflict, map[string]any{"started": false, "error": err.Error()})
		return
	}
	if h.Logger != nil {
		h.Logger.Info("llm_decision_trigger_requested", "remote", r.RemoteAddr)
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"started": true})
}
