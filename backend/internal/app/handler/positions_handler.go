package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"fx-bot/backend/internal/usecase/command"
	"fx-bot/backend/internal/usecase/query"
)

// PositionsHandler exposes:
//
//	GET  /api/positions         — delegated to ListOpenPositionsQuery
//	POST /api/positions/close   — dispatched per symbol via CloseCommands map
//	POST /api/positions/extend  — max_hold_minutes 延長 (延長ボタン)
//
// CloseCommands is keyed by bundle symbol. /api/positions/close reads
// ClosePositionRequest.Symbol from the body and routes to
// CloseCommands[body.Symbol]; missing / unknown → 400. Empty / nil → 503.
//
// ExtendCommand is symbol-agnostic (positions are addressed by global id, and
// the op is a pure DB update with no broker side-effect), so it is a single
// command rather than a per-symbol map.
type PositionsHandler struct {
	ListQuery     *query.ListOpenPositionsQuery
	CloseCommands map[string]*command.ClosePositionCommand
	ExtendCommand *command.ExtendMaxHoldCommand
	Logger        *slog.Logger
}

// List は GET /api/positions[?symbol=X]。
//   - ?symbol=USD_JPY → その symbol のみ返す
//   - no query        → 全 symbol を返す (port.PositionRepository 契約で
//     空 string は no-filter として扱われる)
func (h *PositionsHandler) List(w http.ResponseWriter, r *http.Request) {
	if h.ListQuery == nil {
		WriteError(w, http.StatusServiceUnavailable, "list positions not configured")
		return
	}
	views, err := h.ListQuery.Execute(r.Context(), query.ListOpenPositionsInput{
		Symbol: r.URL.Query().Get("symbol"),
	})
	if err != nil {
		WriteError(w, http.StatusInternalServerError, fmt.Sprintf("positions: %v", err))
		return
	}
	WriteJSON(w, http.StatusOK, views)
}

// Close は POST /api/positions/close。
//
// Body の symbol で CloseCommands map を引いて該当 bundle に dispatch する。
// Symbol 必須 / 未知の symbol は 400。
func (h *PositionsHandler) Close(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	if len(h.CloseCommands) == 0 {
		WriteError(w, http.StatusServiceUnavailable, "close position not configured")
		return
	}
	var body ClosePositionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID <= 0 {
		WriteError(w, http.StatusBadRequest, "id is required")
		return
	}
	if body.Symbol == "" {
		WriteError(w, http.StatusBadRequest, "symbol is required")
		return
	}
	cmd, ok := h.CloseCommands[body.Symbol]
	if !ok || cmd == nil {
		WriteError(w, http.StatusBadRequest, "unknown symbol: "+body.Symbol)
		return
	}

	out, err := cmd.Execute(r.Context(), command.ClosePositionInput{PositionID: body.ID})
	if err != nil {
		if errors.Is(err, command.ErrPositionNotFound) {
			WriteJSON(w, http.StatusNotFound, ClosePositionResponse{Error: err.Error()})
			return
		}
		if h.Logger != nil {
			h.Logger.Error("close_position_failed", "err", err)
		}
		WriteJSON(w, http.StatusInternalServerError, ClosePositionResponse{Error: err.Error()})
		return
	}
	if h.Logger != nil {
		h.Logger.Info("manual_close_position",
			"position_id", out.PositionID, "exit", out.ExitPrice, "pnl_jpy", out.ProfitLossJPY)
	}
	WriteJSON(w, http.StatusOK, ClosePositionResponse{
		PositionID:     out.PositionID,
		Side:           out.Side,
		EntryPrice:     out.EntryPrice,
		ExitPrice:      out.ExitPrice,
		ProfitLossPips: out.ProfitLossPips,
		ProfitLossJPY:  out.ProfitLossJPY,
	})
}

// Extend は POST /api/positions/extend。指定 position の max_hold_minutes に
// add_minutes を加算する (= 保有上限の延長)。
//
//   - 範囲外 add_minutes        → 400 (ErrInvalidExtendMinutes)
//   - 未知 / CLOSING / CLOSED id → 404 (ErrPositionNotFound)
//
// symbol はフロントの行から添えられるが dispatch には使わない (id で一意)。
func (h *PositionsHandler) Extend(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	if h.ExtendCommand == nil {
		WriteError(w, http.StatusServiceUnavailable, "extend position not configured")
		return
	}
	var body ExtendPositionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID <= 0 {
		WriteError(w, http.StatusBadRequest, "id is required")
		return
	}

	out, err := h.ExtendCommand.Execute(r.Context(), command.ExtendMaxHoldInput{
		PositionID: body.ID,
		AddMinutes: body.AddMinutes,
	})
	if err != nil {
		if errors.Is(err, command.ErrInvalidExtendMinutes) {
			WriteJSON(w, http.StatusBadRequest, ExtendPositionResponse{Error: err.Error()})
			return
		}
		if errors.Is(err, command.ErrPositionNotFound) {
			WriteJSON(w, http.StatusNotFound, ExtendPositionResponse{Error: err.Error()})
			return
		}
		if h.Logger != nil {
			h.Logger.Error("extend_position_failed", "err", err)
		}
		WriteJSON(w, http.StatusInternalServerError, ExtendPositionResponse{Error: err.Error()})
		return
	}
	if h.Logger != nil {
		h.Logger.Info("extend_position_maxhold",
			"position_id", out.PositionID, "added_minutes", out.AddedMinutes,
			"max_hold_minutes", out.MaxHoldMinutes, "deadline", out.DeadlineAt)
	}
	WriteJSON(w, http.StatusOK, ExtendPositionResponse{
		PositionID:       out.PositionID,
		MaxHoldMinutes:   out.MaxHoldMinutes,
		AddedMinutes:     out.AddedMinutes,
		DeadlineAt:       out.DeadlineAt,
		RemainingMinutes: out.RemainingMinutes,
	})
}
