package command

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

// fakeBroker は port.Broker を満たすテスト用 stub。
// 必要な戻り値を field に設定してから Command の Execute に渡す。
// ExecutionResolver は実装しない (= type assertion 失敗ケースのテスト用)。
//
// MOCK rationale (TESTING.md §2 古典派 3 用途):
//   - §1 システム境界: broker は GMO API 越し / Paper broker でも置けるが、
//     ここでは call count / 引数記録 / placeOrderDelay 等の観察可能性が必要なので
//     real broker では足りない (= happy path だけなら PaperBroker に置換可能)。
//   - §2 失敗注入: tickerErr / placeOrderErr / closePosErr / cancelOrderErr /
//     getActiveOrdersErr で個別の失敗パスをテストする。
type fakeBroker struct {
	mu sync.Mutex

	ticker      *market.Ticker
	tickerErr   error
	tickerCalls int

	placeOrderResult *order.Order
	placeOrderErr    error
	placeOrderReq    order.PlaceOrderRequest
	placeOrderCalls  int
	// placeOrderDelay is held while inside PlaceOrder so concurrency tests can
	// observe whether two callers are serialised by EntryMutex.
	placeOrderDelay time.Duration

	closePosResult *order.Order
	closePosErr    error
	closePosCalls  int
	closePosArg    position.Position

	// for ManageOpenPositions tests (live mode cancel sequence)
	activeOrders         []order.Order
	getActiveOrdersErr   error
	getActiveOrdersCalls int
	cancelOrderErr       error
	cancelledOrderIDs    []string
}

func (b *fakeBroker) GetTicker(ctx context.Context, symbol string) (*market.Ticker, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tickerCalls++
	if b.tickerErr != nil {
		return nil, b.tickerErr
	}
	if b.ticker == nil {
		return &market.Ticker{Bid: 100.00, Ask: 100.01}, nil
	}
	return b.ticker, nil
}
func (b *fakeBroker) GetKlines(ctx context.Context, sym, iv, date string) ([]market.Kline, error) {
	return nil, nil
}
func (b *fakeBroker) GetAccountMargin(ctx context.Context) (*order.AccountMargin, error) {
	return &order.AccountMargin{}, nil
}
func (b *fakeBroker) GetOpenPositions(ctx context.Context, sym string) ([]position.Position, error) {
	return nil, nil
}
func (b *fakeBroker) GetActiveOrders(ctx context.Context, sym string) ([]order.Order, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.getActiveOrdersCalls++
	if b.getActiveOrdersErr != nil {
		return nil, b.getActiveOrdersErr
	}
	return append([]order.Order{}, b.activeOrders...), nil
}
func (b *fakeBroker) GetExecutions(ctx context.Context, oid string) ([]order.Execution, error) {
	return nil, nil
}
func (b *fakeBroker) PlaceOrder(ctx context.Context, req order.PlaceOrderRequest) (*order.Order, error) {
	b.mu.Lock()
	b.placeOrderCalls++
	b.placeOrderReq = req
	delay := b.placeOrderDelay
	res := b.placeOrderResult
	herr := b.placeOrderErr
	b.mu.Unlock()
	// Sleep OUTSIDE the broker's own mu so the broker mutex isn't what serialises
	// concurrent callers — that would mask whether ExecuteOrder.EntryMutex actually
	// works.
	if delay > 0 {
		time.Sleep(delay)
	}
	if herr != nil {
		return nil, herr
	}
	if res != nil {
		return res, nil
	}
	return &order.Order{OrderID: "ord-1", Price: 100.0, Status: "FILLED"}, nil
}
func (b *fakeBroker) ClosePosition(ctx context.Context, pos position.Position) (*order.Order, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closePosCalls++
	b.closePosArg = pos
	if b.closePosErr != nil {
		return nil, b.closePosErr
	}
	if b.closePosResult != nil {
		return b.closePosResult, nil
	}
	return &order.Order{OrderID: "close-1", Price: 100.0, Status: "FILLED"}, nil
}
func (b *fakeBroker) CancelOrder(ctx context.Context, oid string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelledOrderIDs = append(b.cancelledOrderIDs, oid)
	return b.cancelOrderErr
}

// fakeLiveBroker は port.LiveBroker
// (= port.Broker + ExecutionResolver + SettleLegResolver + OCOCloseOrderPlacer)
// を満たす。
//
// MOCK rationale (TESTING.md §2 古典派 3 用途):
//   - §1 システム境界: Live broker は GMO API 越しで test では本物を叩けない。
//   - §2 失敗注入: resolveErr / settleLegsErr / settleOCOErr で MARKET+OCO
//     entry flow の各分岐を網羅する。
type fakeLiveBroker struct {
	fakeBroker

	resolvePosID  string
	resolveFillPx float64
	// broker-reported costs returned by ResolveExecution。
	// close saga / entry protector がこれを trades.fee_jpy / swap_jpy と
	// positions.entry_fee_jpy へ運ぶことをテストで検証する。
	resolveFeeJPY  float64
	resolveSwapJPY float64
	resolveErr     error
	resolveCalls   int

	// SettleLegResolver — Live entry path queries these after PlaceSettleOCO.
	// Defaults to "tp-leg" / "sl-leg" if unset.
	settleTPOrderID string
	settleSLOrderID string
	settleLegsErr   error
	settleLegsCalls int

	// OCOCloseOrderPlacer — Live entry path posts the OCO settle pair AFTER
	// ResolveExecution. Records calls so tests can verify the TP/SL prices
	// were derived from the *actual* fill price (not the request estimate).
	settleOCOErr      error
	settleOCOCalls    int
	settleOCOLastArgs port.OCOCloseOrderInput
	settleOCORootID   string // defaults to "oco-root" if empty
}

func (b *fakeLiveBroker) ResolveExecution(ctx context.Context, orderID string) (port.ResolvedExecution, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resolveCalls++
	if b.resolveErr != nil {
		return port.ResolvedExecution{}, b.resolveErr
	}
	return port.ResolvedExecution{
		PositionID:     b.resolvePosID,
		Price:          b.resolveFillPx,
		FeeJPY:         b.resolveFeeJPY,
		SettledSwapJPY: b.resolveSwapJPY,
	}, nil
}

func (b *fakeLiveBroker) ResolveSettleLegs(ctx context.Context, brokerPosID, symbol string) (string, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settleLegsCalls++
	if b.settleLegsErr != nil {
		return "", "", b.settleLegsErr
	}
	tp := b.settleTPOrderID
	sl := b.settleSLOrderID
	if tp == "" {
		tp = "tp-leg"
	}
	if sl == "" {
		sl = "sl-leg"
	}
	return tp, sl, nil
}

func (b *fakeLiveBroker) PlaceSettleOCO(ctx context.Context, in port.OCOCloseOrderInput) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settleOCOCalls++
	b.settleOCOLastArgs = in
	if b.settleOCOErr != nil {
		return "", b.settleOCOErr
	}
	if b.settleOCORootID != "" {
		return b.settleOCORootID, nil
	}
	return "oco-root", nil
}

// 注意: PositionRepository / TradeRepository の test 用 in-memory 実装は
// `backtest.NewInMemoryPositionRepo()` / `backtest.NewInMemoryTradeRepo()` を
// 利用する (古典派ルール §4: 実装をそのまま動かす)。fake は廃止。

// fakeCloser は port.PositionCloser の test 実装。
//
// MOCK rationale (TESTING.md §2 古典派 3 用途):
//   - §2 失敗注入: `err: errors.New(...)` で DB 障害を、`ok: false` で
//     CLOSING→CLOSED transition の競合 (= 別 saga が先に閉じた) を再現する。
//   - 観察用 (`calls`, `gotTrade`, `gotID`) で「何が trade として記録されたか」を
//     直接アサートできる。
//
// happy path だけのテストは [backtest.InMemoryPositionCloser] を使う方が望ましいが、
// 既存 callsite は失敗注入と happy path が混在しているため、ここでは fake を
// 残置し、各 callsite が `ok: true` 単独で済むものは段階的に InMemory に
// 置換する方針 (= 次回 cycle の Refactor 候補)。
type fakeCloser struct {
	mu       sync.Mutex
	calls    int
	ok       bool
	err      error
	gotTrade port.TradeRecord
	gotID    int64
}

func (c *fakeCloser) CloseAndRecord(ctx context.Context, posID int64, t time.Time, trade port.TradeRecord) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.gotTrade = trade
	c.gotID = posID
	if c.err != nil {
		return false, c.err
	}
	return c.ok, nil
}

// errResolverFailure は ExecutionResolver タイムアウトを模擬する sentinel。
var errResolverFailure = errors.New("resolve timeout")

// silentLogger は test 中に出力が混ざらないようにするための discard logger。
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
