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
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/query"
)

// tradesWith seeds InMemoryTradeRepo with the given rows (古典派: real impl).
func tradesWith(rows ...port.TradeRecord) *backtest.InMemoryTradeRepo {
	repo := backtest.NewInMemoryTradeRepo()
	for _, r := range rows {
		_ = repo.Insert(context.Background(), r)
	}
	return repo
}

func mkTradesQuery(repo port.TradeRepository) *query.ListTradesQuery {
	return &query.ListTradesQuery{Trades: repo}
}

func TestTradesHandler_List(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		query      *query.ListTradesQuery
		wantStatus int
		wantRows   int
	}{
		{
			name: "returns seeded trades",
			query: mkTradesQuery(tradesWith(
				port.TradeRecord{Symbol: "USD_JPY", Side: "BUY", Quantity: 100, ProfitLossJPY: 50, OpenedAt: now, ClosedAt: now},
				port.TradeRecord{Symbol: "USD_JPY", Side: "SELL", Quantity: 100, ProfitLossJPY: -20, OpenedAt: now, ClosedAt: now},
			)),
			wantStatus: http.StatusOK, wantRows: 2,
		},
		{
			name:       "empty repo returns empty list",
			query:      mkTradesQuery(backtest.NewInMemoryTradeRepo()),
			wantStatus: http.StatusOK, wantRows: 0,
		},
		{
			name:       "nil query returns 503",
			query:      nil,
			wantStatus: http.StatusServiceUnavailable, wantRows: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &TradesHandler{ListQuery: tc.query}
			rec := httptest.NewRecorder()
			h.List(rec, httptest.NewRequest(http.MethodGet, "/api/trades", nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: %d want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			var got []map[string]any
			_ = json.NewDecoder(rec.Body).Decode(&got)
			if len(got) != tc.wantRows {
				t.Errorf("rows: got %d want %d", len(got), tc.wantRows)
			}
		})
	}
}

func TestTradesHandler_Manual(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		body       string
		hasCommand bool
		wantStatus int
	}{
		{"GET returns 405", http.MethodGet, "", true, http.StatusMethodNotAllowed},
		{"nil command returns 503 even with bad side", http.MethodPost, `{"side":"INVALID","take_profit_pips":20,"stop_loss_pips":15}`, false, http.StatusServiceUnavailable},
		{"POST without command returns 503", http.MethodPost, `{"side":"BUY","take_profit_pips":20,"stop_loss_pips":15}`, false, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &TradesHandler{}
			rec := httptest.NewRecorder()
			h.Manual(rec, httptest.NewRequest(tc.method, "/api/trade/manual", bytes.NewReader([]byte(tc.body))))
			if rec.Code != tc.wantStatus {
				t.Errorf("status: %d want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}
