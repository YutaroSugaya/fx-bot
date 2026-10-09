package port

import (
	"context"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
)

// Broker is the abstraction the usecase layer uses for all market data and
// trading operations. Concrete implementations live under
// internal/adapter/broker/.
//
// MVP scope: Paper implementation handles every method. Gmo implementation
// handles Public + Private API reads, with Private POST write paths fully
// implemented but gated by mode=live_config in the calling code.
type Broker interface {
	// Public market data
	GetTicker(ctx context.Context, symbol string) (*market.Ticker, error)
	GetKlines(ctx context.Context, symbol, interval, dateYYYYMMDD string) ([]market.Kline, error)

	// Private account
	GetAccountMargin(ctx context.Context) (*order.AccountMargin, error)
	GetOpenPositions(ctx context.Context, symbol string) ([]position.Position, error)
	GetActiveOrders(ctx context.Context, symbol string) ([]order.Order, error)
	GetExecutions(ctx context.Context, orderID string) ([]order.Execution, error)

	// Private trading
	PlaceOrder(ctx context.Context, req order.PlaceOrderRequest) (*order.Order, error)
	// ClosePosition closes the given position at MARKET. Caller passes the
	// full Position (not just an ID) because GMO requires symbol/side/size
	// for the close request; Paper / Mock benefit from the same context.
	ClosePosition(ctx context.Context, pos position.Position) (*order.Order, error)
	CancelOrder(ctx context.Context, orderID string) error
}

// ResolvedExecution is the aggregate view of an order's fills that
// ResolveExecution returns. PositionID / Price come from the first fill
// (existing semantics: GMO 1,000-currency MARKET orders fill in one
// execution); the cost fields are SUMMED across every fill of the order so
// partial fills cannot under-report fees.
//
// FeeJPY / SettledSwapJPY / LossGainJPY は broker 実報告コストで、
// trades.fee_jpy / swap_jpy への永続化の運搬役。
type ResolvedExecution struct {
	PositionID string
	Price      float64
	// FeeJPY は内部規約「正 = コスト」(GMO wire はキャッシュフロー符号で徴収が負 —
	// adapter の decodeExecutionList が反転して正規化する)。約定金額×0.002%/leg を
	// fill 合算した値。SettledSwapJPY は「受取 = 正」の raw 符号 (net = gross −
	// fee + swap と整合)。LossGainJPY は broker 報告の実現損益 (raw)。
	FeeJPY         float64
	SettledSwapJPY float64
	LossGainJPY    float64
}

// ExecutionResolver is an optional interface implemented by live brokers.
// After PlaceOrder returns an orderId, the caller resolves the actual
// positionId, fill price and broker-reported costs from the broker's
// execution feed. GmoBroker implements this; PaperBroker does not need to.
type ExecutionResolver interface {
	ResolveExecution(ctx context.Context, orderID string) (ResolvedExecution, error)
}

// SettleLegResolver resolves the TP/SL settle leg orderIds attached to a
// broker position. Implemented by Live brokers (GMO OCO settle orders create
// two children whose IDs are not returned by the closeOrder call — they must
// be discovered via /v1/activeOrders).
//
// Returns (tpOrderID, slOrderID, nil) when both legs are found. If either
// leg is missing the call returns an error so the caller can trip
// emergency_stop (a Live position without recorded protection is critical).
type SettleLegResolver interface {
	ResolveSettleLegs(ctx context.Context, brokerPositionID, symbol string) (tpOrderID, slOrderID string, err error)
}

// OCOCloseOrderPlacer places a one-cancels-other settle order against an
// existing broker position. Used for the MARKET+OCO-close two-step entry
// flow (replacing the IFDOCO single-call path that had a 10,000-currency
// minimum on GMO Forex).
//
// Returns the rootOrderId of the OCO container order. The individual leg
// orderIds are discovered separately via SettleLegResolver because GMO
// does not return them in the closeOrder POST response.
//
// Live (Gmo) implements this; Paper does not need to — paper TP/SL is
// monitored by the bot's ManageOpenPositions loop, not pushed to the
// broker.
type OCOCloseOrderPlacer interface {
	PlaceSettleOCO(ctx context.Context, in OCOCloseOrderInput) (rootOrderID string, err error)
}

// OCOCloseOrderInput is the brokerage-agnostic OCO-close request shape.
// `Side` is the CLOSE side (opposite of the position being protected).
// `TPPrice` is the limit-leg price; `SLPrice` is the stop-leg price.
type OCOCloseOrderInput struct {
	Symbol           string
	BrokerPositionID int64
	Side             order.Side
	Size             int
	TPPrice          float64
	SLPrice          float64
}

// LatestExecutionsLookup は「symbol 指定で直近の約定一覧を取りに行く」
// オプショナル interface。Reconcile が leg orderId を持たない (= ResolveSettleLegs
// soft-fail 経路で記録漏れした、もしくは外部建玉として adopt した) live ポジを
// resolve する fallback として使う。
//
// 動機: leg id が記録されていないと、GMO 側 SL 約定後の reconcile が synthetic
// 0-PnL close に倒れ、実際の損益が画面で 0 円表示になる。leg id が無くても
// broker_position_id は必ず記録されているので、positionId 一致 + 反対 side +
// opened_at 以降の execution を引いて実 exit price を復元する。
//
// 戻り値の Execution は positionId / side / price / timestamp が信頼可能で
// あれば足りる (caller 側で filter する)。実装は最新 N 件 (GMO は 100 件) を
// 返せばよく、ページネーション義務は caller には課されていない。
//
// GMO Forex 実装: /v1/latestExecutions?symbol=...
// Paper / 単体テスト: 不要 (オプショナル interface — type assertion で判定)。
type LatestExecutionsLookup interface {
	GetLatestExecutionsBySymbol(ctx context.Context, symbol string) ([]order.Execution, error)
}

// LiveBroker は live_config モードで必須となるブローカ契約。
// Broker + ExecutionResolver + SettleLegResolver + OCOCloseOrderPlacer を
// 組み合わせ、「Live で使うなら必ず約定解決・settle leg 取得・OCO 後付け
// が出来る」ことを型レベルで保証する。
//
// LatestExecutionsLookup は **任意** (recovery fallback 用) なので LiveBroker
// には含めない。GmoBroker は実装しているが、欠落しても Live 通常運用は壊れない。
type LiveBroker interface {
	Broker
	ExecutionResolver
	SettleLegResolver
	OCOCloseOrderPlacer
}
