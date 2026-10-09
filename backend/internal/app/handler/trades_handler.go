package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/usecase/command"
	"fx-bot/backend/internal/usecase/query"
)

// TradesHandler exposes:
//
//	GET  /api/trades?limit=50  — delegated to ListTradesQuery
//	POST /api/trade/manual     — dispatched per symbol via ManualCommands map
//
// ManualCommands keys are bundle symbols (= entries in bot_config.symbols).
// /api/trade/manual reads ManualTradeRequest.Symbol from the request body
// and routes to ManualCommands[body.Symbol]; unknown / missing → 400.
// Empty / nil map → /api/trade/manual returns 503.
type TradesHandler struct {
	ListQuery      *query.ListTradesQuery
	ManualCommands map[string]*command.ManualTradeCommand
	Logger         *slog.Logger
}

// List は GET /api/trades?limit=N。
func (h *TradesHandler) List(w http.ResponseWriter, r *http.Request) {
	if h.ListQuery == nil {
		WriteError(w, http.StatusServiceUnavailable, "list trades not configured")
		return
	}
	limit := 0
	if q := r.URL.Query().Get("limit"); q != "" {
		if v, err := strconv.Atoi(q); err == nil {
			limit = v
		}
	}
	trades, err := h.ListQuery.Execute(r.Context(), query.ListTradesInput{Limit: limit})
	if err != nil {
		WriteError(w, http.StatusInternalServerError, fmt.Sprintf("trades: %v", err))
		return
	}
	WriteJSON(w, http.StatusOK, trades)
}

// Manual は POST /api/trade/manual。
//
// Body の symbol で TradesHandler.ManualCommands map を引き、その symbol の
// bundle に dispatch する。Symbol 必須 / 未知の symbol は 400。
func (h *TradesHandler) Manual(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	if len(h.ManualCommands) == 0 {
		WriteError(w, http.StatusServiceUnavailable, "manual trade not configured")
		return
	}
	var req ManualTradeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Symbol == "" {
		WriteError(w, http.StatusBadRequest, "symbol is required")
		return
	}
	cmd, ok := h.ManualCommands[req.Symbol]
	if !ok || cmd == nil {
		WriteError(w, http.StatusBadRequest, fmt.Sprintf("unknown symbol: %s", req.Symbol))
		return
	}
	if req.Side != "BUY" && req.Side != "SELL" {
		WriteError(w, http.StatusBadRequest, `side must be "BUY" or "SELL"`)
		return
	}
	if req.TakeProfitPips <= 0 || req.StopLossPips <= 0 {
		WriteError(w, http.StatusBadRequest, "take_profit_pips and stop_loss_pips must be > 0")
		return
	}

	side := order.SideBuy
	if req.Side == "SELL" {
		side = order.SideSell
	}

	start := time.Now()
	out, err := cmd.Execute(r.Context(), command.ManualTradeInput{
		Side: side, TakeProfitPips: req.TakeProfitPips, StopLossPips: req.StopLossPips,
		MaxHoldMinutes: req.MaxHoldMinutes, Quantity: req.Quantity,
		AllowOverride: req.AllowOverride,
	})
	durationMs := time.Since(start).Milliseconds()
	if err != nil {
		if h.Logger != nil {
			h.Logger.Error("manual_trade_failed", "symbol", req.Symbol, "err", err)
		}
		WriteJSON(w, http.StatusInternalServerError, ManualTradeResponse{
			Error: err.Error(), DurationMs: durationMs,
		})
		return
	}
	if h.Logger != nil {
		h.Logger.Info("manual_trade_placed",
			"symbol", req.Symbol, "side", req.Side, "entry", out.EntryPrice,
			"tp", out.TPPrice, "sl", out.SLPrice, "qty", out.Quantity)
	}
	WriteJSON(w, http.StatusOK, ManualTradeResponse{
		PositionID: out.PositionID, Side: string(out.Side), Quantity: out.Quantity,
		EntryPrice: out.EntryPrice, TPPrice: out.TPPrice, SLPrice: out.SLPrice,
		DurationMs: durationMs,
	})
}
