package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/query"
)

// --- GET /api/positions?symbol= dispatch ---
//
// Multi-symbol contract:
//   - ?symbol=USD_JPY → only USD_JPY rows
//   - no query        → every symbol's open positions
//   - unknown symbol  → 200 with empty array (matches "no positions for symbol")

func seedPosition(t *testing.T, repo *backtest.InMemoryPositionRepo, sym string) {
	t.Helper()
	_, err := repo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: sym, Side: "BUY", Quantity: 100, EntryPrice: 100, Status: port.PositionStatusOpen,
		OpenedAt: time.Now(),
	}})
	if err != nil {
		t.Fatalf("seed %s: %v", sym, err)
	}
}

func TestPositionsHandler_List_SymbolQueryFilters(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	seedPosition(t, repo, "USD_JPY")
	seedPosition(t, repo, "EUR_JPY")
	seedPosition(t, repo, "EUR_JPY")

	h := &PositionsHandler{ListQuery: &query.ListOpenPositionsQuery{
		PipSize: 0.01, Positions: repo,
	}}
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/api/positions?symbol=EUR_JPY", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	var views []map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&views)
	if len(views) != 2 {
		t.Errorf("?symbol=EUR_JPY filter: got %d rows, want 2", len(views))
	}
}

func TestPositionsHandler_List_NoQueryReturnsAllSymbols(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	seedPosition(t, repo, "USD_JPY")
	seedPosition(t, repo, "EUR_JPY")

	h := &PositionsHandler{ListQuery: &query.ListOpenPositionsQuery{
		PipSize: 0.01, Positions: repo,
	}}
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/api/positions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var views []map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&views)
	if len(views) != 2 {
		t.Errorf("no-query: got %d rows, want 2 (all symbols)", len(views))
	}
}

func TestPositionsHandler_List_UnknownSymbolReturnsEmpty(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	seedPosition(t, repo, "USD_JPY")
	h := &PositionsHandler{ListQuery: &query.ListOpenPositionsQuery{
		PipSize: 0.01, Positions: repo,
	}}
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/api/positions?symbol=GBP_JPY", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var views []map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&views)
	if len(views) != 0 {
		t.Errorf("unknown symbol: got %d rows, want 0", len(views))
	}
}
