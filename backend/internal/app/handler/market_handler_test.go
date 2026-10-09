package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/query"
)

// stubBroker is a minimal port.Broker for handler tests. Only the methods
// used by GetMarketStateQuery are meaningful; the rest return zero values.
//
// MOCK rationale (TESTING.md §2 古典派 3 用途):
//   - §1 システム境界: GMO API は test で本物を叩けない。
//   - §2 失敗注入: failTkr=true で ticker fetch 失敗時の HTTP error 経路を確認する。
type stubBroker struct {
	ticker  *market.Ticker
	klines  []market.Kline
	calls   int
	failTkr bool
}

func (b *stubBroker) GetTicker(context.Context, string) (*market.Ticker, error) {
	b.calls++
	if b.failTkr {
		return nil, http.ErrServerClosed
	}
	return b.ticker, nil
}
func (b *stubBroker) GetKlines(_ context.Context, _, _, _ string) ([]market.Kline, error) {
	return b.klines, nil
}
func (b *stubBroker) GetAccountMargin(context.Context) (*order.AccountMargin, error) {
	return nil, nil
}
func (b *stubBroker) GetOpenPositions(context.Context, string) ([]position.Position, error) {
	return nil, nil
}
func (b *stubBroker) GetActiveOrders(context.Context, string) ([]order.Order, error) {
	return nil, nil
}
func (b *stubBroker) GetExecutions(context.Context, string) ([]order.Execution, error) {
	return nil, nil
}
func (b *stubBroker) PlaceOrder(context.Context, order.PlaceOrderRequest) (*order.Order, error) {
	return nil, nil
}
func (b *stubBroker) ClosePosition(context.Context, position.Position) (*order.Order, error) {
	return nil, nil
}
func (b *stubBroker) CancelOrder(context.Context, string) error { return nil }

func mkMarketHandler(t *testing.T, tickerOK bool) (*MarketHandler, *stubBroker) {
	t.Helper()
	repo := backtest.NewInMemoryCandleRepo()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	// Seed enough 5m bars so at least 5M TF survives even without GMO klines.
	for i := 0; i < 30; i++ {
		op := 159.10 + 0.005*float64(i)
		_ = repo.Upsert(context.Background(), port.CandleRecord{
			Symbol: "USD_JPY", Timeframe: "5m",
			OpenedAt: now.Add(-time.Duration(30-i) * 5 * time.Minute),
			Open:     op, High: op + 0.04, Low: op - 0.03, Close: op + 0.02,
		})
	}
	br := &stubBroker{
		ticker:  &market.Ticker{Symbol: "USD_JPY", Bid: 159.230, Ask: 159.232, Timestamp: now},
		failTkr: !tickerOK,
	}
	q := &query.GetMarketStateQuery{
		Candles: repo, Broker: br,
		Clock: func() time.Time { return now },
	}
	return &MarketHandler{Query: q, Symbol: "USD_JPY"}, br
}

func TestMarketHandler_State_ReturnsView(t *testing.T) {
	cases := []struct {
		name   string
		setup  func() *MarketHandler
		url    string
		status int
		check  func(t *testing.T, body []byte)
	}{
		{
			name:   "happy path with default symbol",
			setup:  func() *MarketHandler { h, _ := mkMarketHandler(t, true); return h },
			url:    "/api/market/state",
			status: 200,
			check: func(t *testing.T, body []byte) {
				var v query.MarketStateView
				if err := json.Unmarshal(body, &v); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if v.Symbol != "USD_JPY" {
					t.Errorf("symbol: %q", v.Symbol)
				}
				if len(v.Timeframes) == 0 {
					t.Error("no timeframes")
				}
			},
		},
		{
			name:   "explicit symbol via query param",
			setup:  func() *MarketHandler { h, _ := mkMarketHandler(t, true); return h },
			url:    "/api/market/state?symbol=USD_JPY",
			status: 200,
			check: func(t *testing.T, body []byte) {
				if len(body) < 10 {
					t.Errorf("tiny body: %q", body)
				}
			},
		},
		{
			name:   "503 when query is nil",
			setup:  func() *MarketHandler { return &MarketHandler{Symbol: "USD_JPY"} },
			url:    "/api/market/state",
			status: http.StatusServiceUnavailable,
			check:  func(t *testing.T, body []byte) {},
		},
		{
			name:   "400 when symbol missing and no default",
			setup:  func() *MarketHandler { h, _ := mkMarketHandler(t, true); h.Symbol = ""; return h },
			url:    "/api/market/state",
			status: http.StatusBadRequest,
			check:  func(t *testing.T, body []byte) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.setup()
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			h.State(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status: got %d want %d (body=%s)", rr.Code, tc.status, rr.Body.String())
			}
			tc.check(t, rr.Body.Bytes())
		})
	}
}

func TestMarketHandler_State_CachesWithinTTL(t *testing.T) {
	h, br := mkMarketHandler(t, true)
	h.CacheTTL = time.Hour

	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/market/state", nil)
		h.State(rr, req)
		if rr.Code != 200 {
			t.Fatalf("iter %d: status %d", i, rr.Code)
		}
	}
	if br.calls != 1 {
		t.Errorf("broker GetTicker calls: got %d want 1 (cache should serve iterations 2+3)", br.calls)
	}
}
