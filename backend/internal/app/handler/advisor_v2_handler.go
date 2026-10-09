package handler

import (
	"log/slog"
	"net/http"
	"os"
)

// AdvisorV2Handler serves the advisor v2 firing-condition snapshot so the dashboard/UI can show,
// per symbol, the current trend, the buy/sell trigger level, how far price is from the trigger,
// ATR, and the last cycle stage. It just streams the file the scheduler writes each cycle
// (runtime/advisor_v2_status.json); a missing file returns an empty snapshot (not an error).
//
//	GET /api/advisor-v2
type AdvisorV2Handler struct {
	StatusPath string
	Logger     *slog.Logger
}

func (h *AdvisorV2Handler) Status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h == nil || h.StatusPath == "" {
		_, _ = w.Write([]byte(`{"by_symbol":{}}`))
		return
	}
	b, err := os.ReadFile(h.StatusPath)
	if err != nil {
		// Not yet written (bot just started / v2 disabled) — return an empty snapshot, not a 500.
		_, _ = w.Write([]byte(`{"by_symbol":{}}`))
		return
	}
	_, _ = w.Write(b)
}
