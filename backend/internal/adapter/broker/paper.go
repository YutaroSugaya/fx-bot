package broker

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
)

// PricerFunc returns a (bid, ask) pair for the symbol at the current moment.
// PaperBroker calls it whenever a fill decision is needed.
type PricerFunc func(ctx context.Context, symbol string) (bid, ask float64, err error)

// PaperBroker is an in-memory simulated broker for paper_config mode.
// Fills are immediate and deterministic:
//
//	BUY  at current ASK
//	SELL at current BID
//	Close BUY at current BID   (spread cost)
//	Close SELL at current ASK
//
// It implements port.Broker; the read endpoints that touch a real exchange
// (GetTicker, GetKlines, GetAccountMargin) are delegated to a Pricer or
// return placeholder values — the bot uses the GMO Public API for ticker/
// kline in mixed mode anyway.
type PaperBroker struct {
	mu             sync.Mutex
	nextID         int64
	positions      map[string]position.Position
	orders         map[string]order.Order
	closePrices    map[string]float64 // brokerPositionID → realized close price
	pricer         PricerFunc
	slippagePips   float64 // adverse fill offset
	feeJPYPerTrade float64 // subtracted from PnL on close
	clock          func() time.Time
}

// PaperBrokerConfig configures a PaperBroker.
//
// SlippagePips and FeeJPYPerTrade default to 0 (frictionless simulation).
// Set to non-zero to approximate Live execution cost so Paper PnL converges
// toward Live PnL (Live 実測 slippage を Paper に反映 — HardLimits.Paper も参照)。
//
// Pip size is resolved per-call via market.PipSize(symbol) so one broker
// instance can serve every supported symbol. There is no broker-level
// PipSize override.
type PaperBrokerConfig struct {
	Pricer         PricerFunc
	SlippagePips   float64
	FeeJPYPerTrade float64
	Clock          func() time.Time
}

func NewPaperBroker(cfg PaperBrokerConfig) *PaperBroker {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &PaperBroker{
		positions:      map[string]position.Position{},
		orders:         map[string]order.Order{},
		closePrices:    map[string]float64{},
		pricer:         cfg.Pricer,
		slippagePips:   cfg.SlippagePips,
		feeJPYPerTrade: cfg.FeeJPYPerTrade,
		clock:          cfg.Clock,
	}
}

// roundPips rounds a pip count to one decimal place so absolute prices like
// 150.33 don't yield 19.999999 due to float subtraction error.
func roundPips(p float64) float64 {
	// 1 decimal place
	return float64(int64(p*10+0.5)) / 10
}

// SwapPricer replaces the pricer function. Useful in tests to simulate price
// moves without rebuilding the broker.
func (b *PaperBroker) SwapPricer(p PricerFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pricer = p
}

func (b *PaperBroker) nextOrderID() string {
	b.nextID++
	return "paper-ord-" + strconv.FormatInt(b.nextID, 10)
}

func (b *PaperBroker) nextPositionID() string {
	b.nextID++
	return "paper-pos-" + strconv.FormatInt(b.nextID, 10)
}

// Restore は bot 再起動時に呼ぶ。DB に残っている OPEN ポジションを
// PaperBroker のメモリマップに復元することで、再起動後も ClosePosition で
// 「unknown position」エラーが出ないようにする。
//
// 渡す positions は port.PositionRepository.ListOpenOrClosing() の結果を想定。
// nextID は復元する broker_position_id の "paper-pos-N" の N 最大値より大きく
// 進めて、新規約定時の ID 衝突を防ぐ。
func (b *PaperBroker) Restore(positions []position.Position) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range positions {
		if p.BrokerPositionID == "" {
			continue
		}
		b.positions[p.BrokerPositionID] = p
		// "paper-pos-123" / "paper-ord-456" 形式から数値を抜き、nextID を進める。
		if n, ok := parsePaperIDSuffix(p.BrokerPositionID); ok && n > b.nextID {
			b.nextID = n
		}
	}
}

// parsePaperIDSuffix は "paper-pos-42" / "paper-ord-42" の末尾数値を返す。
// マッチしなければ ok=false。
func parsePaperIDSuffix(id string) (int64, bool) {
	// 末尾の数字部分だけ抜く。シンプルに最後のハイフン以降をパース。
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '-' {
			if v, err := strconv.ParseInt(id[i+1:], 10, 64); err == nil {
				return v, true
			}
			return 0, false
		}
	}
	return 0, false
}

// GetTicker delegates to the configured Pricer. Returning the spread as a
// real ticker keeps strategy evaluation realistic.
func (b *PaperBroker) GetTicker(ctx context.Context, symbol string) (*market.Ticker, error) {
	if b.pricer == nil {
		return nil, fmt.Errorf("paper: no pricer configured")
	}
	bid, ask, err := b.pricer(ctx, symbol)
	if err != nil {
		return nil, err
	}
	return &market.Ticker{
		Symbol:    symbol,
		Bid:       bid,
		Ask:       ask,
		Timestamp: b.clock(),
	}, nil
}

// GetKlines returns nothing — paper mode pulls klines from the public API,
// not the broker simulator.
func (b *PaperBroker) GetKlines(ctx context.Context, symbol, interval, dateYYYYMMDD string) ([]market.Kline, error) {
	return nil, nil
}

// GetAccountMargin reports a stub margin record.
func (b *PaperBroker) GetAccountMargin(ctx context.Context) (*order.AccountMargin, error) {
	return &order.AccountMargin{
		AvailableJPY: 1_000_000,
		MarginRatio:  100.0,
		Equity:       1_000_000,
	}, nil
}

func (b *PaperBroker) GetOpenPositions(ctx context.Context, symbol string) ([]position.Position, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]position.Position, 0, len(b.positions))
	for _, p := range b.positions {
		if p.Status != position.StatusOpen {
			continue
		}
		if symbol == "" || p.Symbol == symbol {
			out = append(out, p)
		}
	}
	return out, nil
}

func (b *PaperBroker) GetActiveOrders(ctx context.Context, symbol string) ([]order.Order, error) {
	// In paper mode every order fills instantly, so "active" is always empty.
	return nil, nil
}

func (b *PaperBroker) GetExecutions(ctx context.Context, orderID string) ([]order.Execution, error) {
	// Paper only stores positions; if needed, future expansion can record
	// per-fill executions. For MVP this returns empty.
	return nil, nil
}

// PlaceOrder opens a paper position immediately at the current ask/bid.
// TP and SL from the request are stored on the position; the order manager
// monitors ticks and calls ClosePosition when a level is hit.
func (b *PaperBroker) PlaceOrder(ctx context.Context, req order.PlaceOrderRequest) (*order.Order, error) {
	if b.pricer == nil {
		return nil, fmt.Errorf("paper: pricer not set")
	}
	if !req.Side.Valid() {
		return nil, fmt.Errorf("paper: invalid side %q", req.Side)
	}
	if req.Quantity <= 0 {
		return nil, fmt.Errorf("paper: quantity must be > 0")
	}
	bid, ask, err := b.pricer(ctx, req.Symbol)
	if err != nil {
		return nil, fmt.Errorf("paper: pricer: %w", err)
	}
	pip := market.PipSize(req.Symbol)
	// Adverse-direction slippage: worsens the fill so Paper PnL is
	// closer to Live. Zero by default → existing fills unchanged.
	slip := b.slippagePips * pip
	var fill float64
	if req.Side == order.SideBuy {
		fill = ask + slip
	} else {
		fill = bid - slip
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Use the same id for the order and the resulting position so the
	// caller (ExecuteOrder) can pass ord.OrderID back into ClosePosition.
	pid := b.nextPositionID()
	oid := pid
	now := b.clock()

	// Translate TP/SL absolute prices back to pips so the Position holds the
	// same semantics as live entry fills. If req.TakeProfit/StopLoss are 0
	// the position carries 0 pips and the manager won't auto-close on them.
	var tpPips, slPips float64
	if req.TakeProfit > 0 {
		tpPips = roundPips(absDiff(req.TakeProfit, fill) / pip)
	}
	if req.StopLoss > 0 {
		slPips = roundPips(absDiff(req.StopLoss, fill) / pip)
	}

	pos := position.Position{
		BrokerPositionID: pid,
		Symbol:           req.Symbol,
		Side:             req.Side,
		Quantity:         req.Quantity,
		EntryPrice:       fill,
		TakeProfitPips:   tpPips,
		StopLossPips:     slPips,
		Status:           position.StatusOpen,
		OpenedAt:         now,
	}
	b.positions[pid] = pos

	ord := order.Order{
		OrderID:   oid,
		Symbol:    req.Symbol,
		Side:      req.Side,
		Type:      req.Type,
		Quantity:  req.Quantity,
		Price:     fill,
		Status:    "FILLED",
		CreatedAt: now,
	}
	b.orders[oid] = ord
	return &ord, nil
}

// ClosePosition closes the given paper position at the current opposite price.
func (b *PaperBroker) ClosePosition(ctx context.Context, pos position.Position) (*order.Order, error) {
	if pos.BrokerPositionID == "" {
		return nil, fmt.Errorf("paper: missing broker position id")
	}
	b.mu.Lock()
	cur, ok := b.positions[pos.BrokerPositionID]
	if !ok {
		b.mu.Unlock()
		return nil, fmt.Errorf("paper: unknown position %q", pos.BrokerPositionID)
	}
	if cur.Status != position.StatusOpen {
		b.mu.Unlock()
		return nil, fmt.Errorf("paper: position %q already closed", pos.BrokerPositionID)
	}
	b.mu.Unlock()

	bid, ask, err := b.pricer(ctx, cur.Symbol)
	if err != nil {
		return nil, fmt.Errorf("paper: pricer: %w", err)
	}
	// Adverse slippage on close too: BUY close = sell at bid - slip,
	// SELL close = buy at ask + slip.
	slip := b.slippagePips * market.PipSize(cur.Symbol)
	var fill float64
	if cur.Side == order.SideBuy {
		fill = bid - slip
	} else {
		fill = ask + slip
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	cur.Status = position.StatusClosed
	cur.ClosedAt = &now
	b.positions[pos.BrokerPositionID] = cur
	b.closePrices[pos.BrokerPositionID] = fill

	oid := b.nextOrderID()
	ord := order.Order{
		OrderID:   oid,
		Symbol:    cur.Symbol,
		Side:      cur.Side.Opposite(),
		Type:      order.OrderTypeMarket,
		Quantity:  cur.Quantity,
		Price:     fill,
		Status:    "FILLED",
		CreatedAt: now,
	}
	b.orders[oid] = ord
	return &ord, nil
}

// CancelOrder is a no-op in paper mode (orders fill instantly).
func (b *PaperBroker) CancelOrder(ctx context.Context, orderID string) error {
	return nil
}

// PnLJPY computes the realized profit/loss in JPY for a closed position.
// Useful for test assertions.
//
// For USDJPY: pnl_jpy = (exit - entry) * quantity * (BUY→+1, SELL→-1).
func (b *PaperBroker) PnLJPY(brokerPositionID string) (float64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pos, ok := b.positions[brokerPositionID]
	if !ok {
		return 0, fmt.Errorf("paper: no position %q", brokerPositionID)
	}
	if pos.Status != position.StatusClosed {
		return 0, fmt.Errorf("paper: position not closed")
	}
	closePx, ok := b.closePrices[brokerPositionID]
	if !ok {
		return 0, fmt.Errorf("paper: close price not recorded for %q", brokerPositionID)
	}
	dir := 1.0
	if pos.Side == order.SideSell {
		dir = -1.0
	}
	gross := (closePx - pos.EntryPrice) * float64(pos.Quantity) * dir
	return gross - b.feeJPYPerTrade, nil
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}
