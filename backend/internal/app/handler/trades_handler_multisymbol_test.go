package handler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"fx-bot/backend/internal/usecase/command"
)

// --- POST /api/trade/manual symbol dispatch ---
//
// Multi-symbol contract:
//   - body.Symbol is REQUIRED. Missing → 400.
//   - body.Symbol must match a key in TradesHandler.ManualCommands. Unknown → 400.
//   - Matching symbol routes to that bundle's ManualTradeCommand.
//
// The legacy single ManualCommand field is replaced by ManualCommands map.

func TestTradesHandler_Manual_UnknownSymbolRejected(t *testing.T) {
	h := &TradesHandler{
		ManualCommands: map[string]*command.ManualTradeCommand{
			"USD_JPY": {}, // map populated but no entry for EUR_JPY
		},
		Logger: slog.Default(),
	}
	body, _ := json.Marshal(ManualTradeRequest{
		Symbol: "EUR_JPY", Side: "BUY",
		TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240, Quantity: 100,
	})
	rec := httptest.NewRecorder()
	h.Manual(rec, httptest.NewRequest(http.MethodPost, "/api/trade/manual", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: %d want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestTradesHandler_Manual_MissingSymbolRejected(t *testing.T) {
	h := &TradesHandler{
		ManualCommands: map[string]*command.ManualTradeCommand{"USD_JPY": {}},
		Logger:         slog.Default(),
	}
	body, _ := json.Marshal(ManualTradeRequest{
		// Symbol intentionally absent
		Side: "BUY", TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240, Quantity: 100,
	})
	rec := httptest.NewRecorder()
	h.Manual(rec, httptest.NewRequest(http.MethodPost, "/api/trade/manual", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: %d want 400", rec.Code)
	}
}

func TestTradesHandler_Manual_EmptyMapReturns503(t *testing.T) {
	// No bundles wired → endpoint reports unavailable rather than 400.
	h := &TradesHandler{ManualCommands: nil, Logger: slog.Default()}
	body, _ := json.Marshal(ManualTradeRequest{
		Symbol: "USD_JPY", Side: "BUY",
		TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240, Quantity: 100,
	})
	rec := httptest.NewRecorder()
	h.Manual(rec, httptest.NewRequest(http.MethodPost, "/api/trade/manual", bytes.NewReader(body)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: %d want 503", rec.Code)
	}
}
