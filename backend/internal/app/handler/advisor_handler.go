package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"fx-bot/backend/internal/usecase/query"
)

// TriggerAdvisorFunc は POST /api/advisor/trigger から呼ばれる関数型。
// cmd/bot/main.go で AdvisorCycle.Run + holder.Set をラップした closure を渡す。
//
// symbol="" → 全 bundle 並列発火 (fan-out)。
// symbol="USD_JPY" などの値 → その bundle のみ発火。
// 未知 symbol → エラー。
type TriggerAdvisorFunc func(ctx context.Context, symbol string) (TriggerResult, error)

// AdvisorHandler exposes:
//
//	GET  /api/advisor/recent  — delegated to ListRecentDecisionsQuery
//	POST /api/advisor/trigger — fire one Claude cycle on demand
type AdvisorHandler struct {
	RecentQuery *query.ListRecentDecisionsQuery
	Trigger     TriggerAdvisorFunc // nil → /trigger returns 503
	CLITimeout  time.Duration      // 0 → default 120s
	Logger      *slog.Logger
}

// Recent は GET /api/advisor/recent?limit=N。
func (h *AdvisorHandler) Recent(w http.ResponseWriter, r *http.Request) {
	if h.RecentQuery == nil {
		WriteError(w, http.StatusServiceUnavailable, "recent decisions not configured")
		return
	}
	limit := 0
	if q := r.URL.Query().Get("limit"); q != "" {
		if v, err := strconv.Atoi(q); err == nil {
			limit = v
		}
	}
	out, err := h.RecentQuery.Execute(r.Context(), query.ListRecentDecisionsInput{Limit: limit})
	if err != nil {
		WriteError(w, http.StatusInternalServerError, fmt.Sprintf("recent: %v", err))
		return
	}
	WriteJSON(w, http.StatusOK, out)
}

// TriggerNow は POST /api/advisor/trigger。Claude advisor cycle を即実行。
//
// Body は JSON `{"symbol":"USD_JPY"}` 形式 (省略可)。symbol を指定すれば
// その bundle のみ発火、未指定 (= 空 body or symbol:"") なら全 bundle を
// 並列に動かして PerSymbol に詰める。
func (h *AdvisorHandler) TriggerNow(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	if h.Trigger == nil {
		WriteError(w, http.StatusServiceUnavailable, "advisor trigger not configured")
		return
	}
	// Body は省略可。空 body / decode 失敗時は symbol="" (= 全 fan-out) 扱い。
	var req TriggerAdvisorRequest
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	// Multi-symbol fan-out timeout: per-symbol Claude CLI が並列に走るが
	// 直列に動くケース (= MaxConcurrentSymbols=1) でも完走できる buffer を
	// 用意する。fan-out 時は CLI timeout × 2 + 60s をデッドラインに採用。
	cliTimeout := h.CLITimeout
	if cliTimeout == 0 {
		cliTimeout = 120 * time.Second
	}
	deadline := cliTimeout + 60*time.Second
	if req.Symbol == "" {
		deadline = cliTimeout*2 + 60*time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), deadline)
	defer cancel()

	start := time.Now()
	if h.Logger != nil {
		h.Logger.Info("advisor_trigger_requested",
			"remote", r.RemoteAddr, "symbol", req.Symbol)
	}
	res, err := h.Trigger(ctx, req.Symbol)
	res.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		if h.Logger != nil {
			h.Logger.Error("advisor_trigger_failed",
				"err", err, "symbol", req.Symbol, "duration_ms", res.DurationMs)
		}
		res.Error = err.Error()
		WriteJSON(w, http.StatusInternalServerError, res)
		return
	}
	if h.Logger != nil {
		h.Logger.Info("advisor_trigger_done",
			"symbol", res.Symbol, "promoted", res.Promoted, "config_id", res.ConfigID,
			"strategy", res.Strategy, "per_symbol", len(res.PerSymbol),
			"duration_ms", res.DurationMs)
	}
	WriteJSON(w, http.StatusOK, res)
}
