package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/command"
	"fx-bot/backend/internal/usecase/query"
)

// mkListQuery composes a real ListOpenPositionsQuery (古典派ルール §4) — handler
// テストはハンドラ層に閉じず end-to-end の挙動を見る。
func mkListQuery(repo port.PositionRepository, getTicker query.GetTickerFn) *query.ListOpenPositionsQuery {
	return &query.ListOpenPositionsQuery{
		Symbol: "USD_JPY", PipSize: 0.01,
		Positions: repo, GetTicker: getTicker,
	}
}

func TestPositionsHandler_List(t *testing.T) {
	now := time.Now()
	openedAt := now.Add(-30 * time.Minute)
	rec := port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100.00,
		TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
		StrategyConfigID: "cfg-test",
		Status:           port.PositionStatusOpen, OpenedAt: openedAt,
	}

	cases := []struct {
		name             string
		seed             *port.PositionRecord
		getTicker        query.GetTickerFn
		wantStatus       int
		wantLen          int
		checkCurrentPx   bool
		wantCurrentPx    float64
		wantUnrealizedOk bool // sign check: true if want > 0
	}{
		{
			name: "with ticker computes current price and unrealized PnL",
			seed: &rec,
			getTicker: func(ctx context.Context, _ string) (*market.Ticker, error) {
				return &market.Ticker{Bid: 100.10, Ask: 100.11}, nil
			},
			wantStatus:       http.StatusOK,
			wantLen:          1,
			checkCurrentPx:   true,
			wantCurrentPx:    100.10, // BUY closes at bid
			wantUnrealizedOk: true,
		},
		{
			name:           "no ticker keeps current price zero",
			seed:           &rec,
			getTicker:      nil,
			wantStatus:     http.StatusOK,
			wantLen:        1,
			checkCurrentPx: true,
			wantCurrentPx:  0,
		},
		{
			name:       "empty repo returns 200 with 0 rows",
			seed:       nil,
			getTicker:  nil,
			wantStatus: http.StatusOK,
			wantLen:    0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := backtest.NewInMemoryPositionRepo()
			if tc.seed != nil {
				_, _ = repo.Insert(context.Background(), port.PositionInsertInput{Position: *tc.seed})
			}
			h := &PositionsHandler{ListQuery: mkListQuery(repo, tc.getTicker)}
			rec := httptest.NewRecorder()
			h.List(rec, httptest.NewRequest(http.MethodGet, "/api/positions", nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status: %d want %d", rec.Code, tc.wantStatus)
			}
			var got []query.OpenPositionView
			_ = json.NewDecoder(rec.Body).Decode(&got)
			if len(got) != tc.wantLen {
				t.Fatalf("len: got %d want %d", len(got), tc.wantLen)
			}
			if tc.wantLen > 0 && tc.checkCurrentPx {
				if !approx(got[0].CurrentPrice, tc.wantCurrentPx) {
					t.Errorf("CurrentPrice: got %v want %v", got[0].CurrentPrice, tc.wantCurrentPx)
				}
			}
			if tc.wantLen > 0 && tc.wantUnrealizedOk && got[0].UnrealizedJPY <= 0 {
				t.Errorf("UnrealizedJPY should be positive; got %v", got[0].UnrealizedJPY)
			}
		})
	}
}

func TestPositionsHandler_List_RepoError_Returns500(t *testing.T) {
	repo := &errPositions{
		PositionRepository: backtest.NewInMemoryPositionRepo(),
		listOpenErr:        errors.New("db down"),
	}
	h := &PositionsHandler{ListQuery: mkListQuery(repo, nil)}
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/api/positions", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: %d want 500", rec.Code)
	}
}

func TestPositionsHandler_List_NilQuery_Returns503(t *testing.T) {
	h := &PositionsHandler{ListQuery: nil}
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/api/positions", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: %d want 503", rec.Code)
	}
}

func TestPositionsHandler_Close(t *testing.T) {
	// 405 / 503 / 400 までは handler 層で完結し Execute に降りない。
	// 実 Execute の挙動は command/close_position_test.go の責務。
	// Multi-symbol dispatch coverage は positions_close_multisymbol_test.go.
	emptyMap := map[string]*command.ClosePositionCommand{"USD_JPY": {}}
	cases := []struct {
		name       string
		method     string
		body       string
		hasMap     bool
		wantStatus int
	}{
		{"GET returns 405", http.MethodGet, "", true, http.StatusMethodNotAllowed},
		{"nil map returns 503", http.MethodPost, `{"id":1,"symbol":"USD_JPY"}`, false, http.StatusServiceUnavailable},
		{"id=0 returns 400 when map wired", http.MethodPost, `{"id":0,"symbol":"USD_JPY"}`, true, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &PositionsHandler{}
			if tc.hasMap {
				h.CloseCommands = emptyMap
			}
			rec := httptest.NewRecorder()
			h.Close(rec, httptest.NewRequest(tc.method, "/api/positions/close", bytes.NewReader([]byte(tc.body))))
			if rec.Code != tc.wantStatus {
				t.Errorf("status: %d want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }
