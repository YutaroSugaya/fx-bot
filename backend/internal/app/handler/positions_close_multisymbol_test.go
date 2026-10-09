package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/command"
)

// --- POST /api/positions/close per-symbol dispatch ---
//
// Pre-fix vulnerability: handler held a single ClosePositionCommand
// (= primary bundle), and ClosePositionCommand.Execute filtered
// ListOpenOrClosing by its own Symbol, so non-primary positions surfaced
// in the dashboard but 404'd on close.
//
// Fix: PositionsHandler.CloseCommands map[string]*ClosePositionCommand,
// keyed by bundle symbol. Body carries `symbol` from the row's OpenPositionView.

func TestPositionsHandler_Close_DispatchesByBodySymbol(t *testing.T) {
	// Two bundles, each with its own repo. Closing a EUR_JPY position must
	// hit the EUR_JPY command, not USD_JPY. The assertion targets the
	// position-lookup outcome: USD_JPY's repo is intentionally empty, so
	// a misdispatch surfaces as ErrPositionNotFound (404). A correct
	// dispatch finds the row and proceeds into ExecuteCloseSaga (which we
	// don't drive end-to-end — broker is nil in the stub).
	usdRepo := backtest.NewInMemoryPositionRepo()
	eurRepo := backtest.NewInMemoryPositionRepo()
	id, _ := eurRepo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: "EUR_JPY", Side: "BUY", Quantity: 100, EntryPrice: 163.40,
		Status: port.PositionStatusOpen, OpenedAt: time.Now(),
	}})

	h := &PositionsHandler{
		CloseCommands: map[string]*command.ClosePositionCommand{
			"USD_JPY": newCloseCmd("USD_JPY", usdRepo),
			"EUR_JPY": newCloseCmd("EUR_JPY", eurRepo),
		},
	}
	body, _ := json.Marshal(ClosePositionRequest{ID: id, Symbol: "EUR_JPY"})

	// ExecuteCloseSaga panics when broker is nil; recover and inspect what
	// the handler did before the panic to assert the dispatch outcome.
	var rec *httptest.ResponseRecorder
	func() {
		defer func() { _ = recover() }()
		rec = httptest.NewRecorder()
		h.Close(rec, httptest.NewRequest(http.MethodPost, "/api/positions/close", bytes.NewReader(body)))
	}()

	if rec == nil {
		t.Fatal("recorder unexpectedly nil — handler panicked before writing anything")
	}
	if rec.Code == http.StatusNotFound {
		t.Fatalf("dispatch went to wrong bundle (404 = USD_JPY's repo didn't have the EUR_JPY id)")
	}
}

func TestPositionsHandler_Close_MissingSymbolRejected(t *testing.T) {
	h := &PositionsHandler{
		CloseCommands: map[string]*command.ClosePositionCommand{"USD_JPY": newCloseCmd("USD_JPY", backtest.NewInMemoryPositionRepo())},
	}
	body, _ := json.Marshal(map[string]any{"id": 1}) // no symbol
	rec := httptest.NewRecorder()
	h.Close(rec, httptest.NewRequest(http.MethodPost, "/api/positions/close", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing symbol should 400, got %d", rec.Code)
	}
}

func TestPositionsHandler_Close_UnknownSymbolRejected(t *testing.T) {
	h := &PositionsHandler{
		CloseCommands: map[string]*command.ClosePositionCommand{"USD_JPY": newCloseCmd("USD_JPY", backtest.NewInMemoryPositionRepo())},
	}
	body, _ := json.Marshal(ClosePositionRequest{ID: 1, Symbol: "GBP_JPY"})
	rec := httptest.NewRecorder()
	h.Close(rec, httptest.NewRequest(http.MethodPost, "/api/positions/close", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown symbol should 400, got %d", rec.Code)
	}
}

func TestPositionsHandler_Close_EmptyMapReturns503(t *testing.T) {
	h := &PositionsHandler{CloseCommands: nil}
	body, _ := json.Marshal(ClosePositionRequest{ID: 1, Symbol: "USD_JPY"})
	rec := httptest.NewRecorder()
	h.Close(rec, httptest.NewRequest(http.MethodPost, "/api/positions/close", bytes.NewReader(body)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("empty map should 503, got %d", rec.Code)
	}
}

func newCloseCmd(symbol string, repo port.PositionRepository) *command.ClosePositionCommand {
	return &command.ClosePositionCommand{
		Mode:      config.ModePaperConfig,
		Symbol:    symbol,
		Positions: repo,
	}
}
