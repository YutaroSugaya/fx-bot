package broker

import (
	"context"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

// MockBroker is a programmable port.Broker for tests. Every method delegates
// to the corresponding func field if set, else returns zero/empty values.
//
// MOCK rationale (TESTING.md §2 古典派 3 用途):
//   - §1 システム境界: GMO API は test で本物を叩けない (= PaperBroker は内部状態を
//     持つため reconcile 差分テストには向かない)。
//   - §2 失敗注入: 各 *Fn フィールドで「broker にこの position が見える」「ticker が
//     特定の bid/ask を返す」等の差分を強制し、reconcile のシナリオ別経路を網羅する。
type MockBroker struct {
	GetTickerFn        func(ctx context.Context, symbol string) (*market.Ticker, error)
	GetKlinesFn        func(ctx context.Context, symbol, interval, date string) ([]market.Kline, error)
	GetAccountMarginFn func(ctx context.Context) (*order.AccountMargin, error)
	GetOpenPositionsFn func(ctx context.Context, symbol string) ([]position.Position, error)
	GetActiveOrdersFn  func(ctx context.Context, symbol string) ([]order.Order, error)
	GetExecutionsFn    func(ctx context.Context, orderID string) ([]order.Execution, error)
	// GetLatestExecutionsBySymbolFn は port.LatestExecutionsLookup の mock。
	// reconcile の positionId-lookup fallback を駆動するテスト用。
	GetLatestExecutionsBySymbolFn func(ctx context.Context, symbol string) ([]order.Execution, error)
	PlaceOrderFn                  func(ctx context.Context, req order.PlaceOrderRequest) (*order.Order, error)
	ClosePositionFn               func(ctx context.Context, pos position.Position) (*order.Order, error)
	CancelOrderFn                 func(ctx context.Context, orderID string) error
}

func (m *MockBroker) GetTicker(ctx context.Context, symbol string) (*market.Ticker, error) {
	if m.GetTickerFn != nil {
		return m.GetTickerFn(ctx, symbol)
	}
	return nil, nil
}

func (m *MockBroker) GetKlines(ctx context.Context, symbol, interval, date string) ([]market.Kline, error) {
	if m.GetKlinesFn != nil {
		return m.GetKlinesFn(ctx, symbol, interval, date)
	}
	return nil, nil
}

func (m *MockBroker) GetAccountMargin(ctx context.Context) (*order.AccountMargin, error) {
	if m.GetAccountMarginFn != nil {
		return m.GetAccountMarginFn(ctx)
	}
	return nil, nil
}

func (m *MockBroker) GetOpenPositions(ctx context.Context, symbol string) ([]position.Position, error) {
	if m.GetOpenPositionsFn != nil {
		return m.GetOpenPositionsFn(ctx, symbol)
	}
	return nil, nil
}

func (m *MockBroker) GetActiveOrders(ctx context.Context, symbol string) ([]order.Order, error) {
	if m.GetActiveOrdersFn != nil {
		return m.GetActiveOrdersFn(ctx, symbol)
	}
	return nil, nil
}

func (m *MockBroker) GetExecutions(ctx context.Context, orderID string) ([]order.Execution, error) {
	if m.GetExecutionsFn != nil {
		return m.GetExecutionsFn(ctx, orderID)
	}
	return nil, nil
}

// GetLatestExecutionsBySymbol は port.LatestExecutionsLookup を満たす。
// 既存テストで Fn 未設定なら空 list を返し、type assertion は成立しても
// fallback ロジックは「該当なし → synthetic close」に流れる (既存挙動維持)。
func (m *MockBroker) GetLatestExecutionsBySymbol(ctx context.Context, symbol string) ([]order.Execution, error) {
	if m.GetLatestExecutionsBySymbolFn != nil {
		return m.GetLatestExecutionsBySymbolFn(ctx, symbol)
	}
	return nil, nil
}

func (m *MockBroker) PlaceOrder(ctx context.Context, req order.PlaceOrderRequest) (*order.Order, error) {
	if m.PlaceOrderFn != nil {
		return m.PlaceOrderFn(ctx, req)
	}
	return nil, nil
}

func (m *MockBroker) ClosePosition(ctx context.Context, pos position.Position) (*order.Order, error) {
	if m.ClosePositionFn != nil {
		return m.ClosePositionFn(ctx, pos)
	}
	return nil, nil
}

func (m *MockBroker) CancelOrder(ctx context.Context, orderID string) error {
	if m.CancelOrderFn != nil {
		return m.CancelOrderFn(ctx, orderID)
	}
	return nil
}

// MockLiveBroker は MockBroker + ExecutionResolver + SettleLegResolver
// + OCOCloseOrderPlacer。port.LiveBroker を満たす。
// Live モードのテストで「PlaceOrder 後に positionId を resolve」「OCO close
// で TP/SL を後付け」「settle leg orderIds を活性注文一覧から拾う」一連の
// セマンティクスを検証する用途。MockBroker 単体はあえて Resolver 未実装に
// 保ち、Live 必須シナリオが「no resolver」だったときの fail-loud 経路を
// 別途テストできる。
type MockLiveBroker struct {
	MockBroker
	ResolveExecutionFn  func(ctx context.Context, orderID string) (port.ResolvedExecution, error)
	ResolveSettleLegsFn func(ctx context.Context, brokerPositionID, symbol string) (string, string, error)
	PlaceSettleOCOFn    func(ctx context.Context, in port.OCOCloseOrderInput) (string, error)
}

func (m *MockLiveBroker) ResolveExecution(ctx context.Context, orderID string) (port.ResolvedExecution, error) {
	if m.ResolveExecutionFn != nil {
		return m.ResolveExecutionFn(ctx, orderID)
	}
	return port.ResolvedExecution{}, nil
}

func (m *MockLiveBroker) ResolveSettleLegs(ctx context.Context, brokerPositionID, symbol string) (string, string, error) {
	if m.ResolveSettleLegsFn != nil {
		return m.ResolveSettleLegsFn(ctx, brokerPositionID, symbol)
	}
	return "", "", nil
}

func (m *MockLiveBroker) PlaceSettleOCO(ctx context.Context, in port.OCOCloseOrderInput) (string, error) {
	if m.PlaceSettleOCOFn != nil {
		return m.PlaceSettleOCOFn(ctx, in)
	}
	return "", nil
}
