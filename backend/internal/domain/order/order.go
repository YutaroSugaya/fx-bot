// Package order holds order- and execution-related value types.
package order

import "time"

// Side is "BUY" or "SELL".
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

func (s Side) Valid() bool {
	return s == SideBuy || s == SideSell
}

// Opposite returns the side that closes a position of this side.
//
// 反対サイドを決める関数が不正値 ("" や "FOO") に対して silently BUY を返すと、
// 不正入力がそのまま「live の BUY 成行」として下流に流れる危険がある。
// そのため BUY/SELL 以外は empty Side ("") を返す。empty は Valid()==false で、
// 下流の PlaceOrder などが弾く設計 (gmo_fx.go の `if !req.Side.Valid()` チェック)。
func (s Side) Opposite() Side {
	switch s {
	case SideBuy:
		return SideSell
	case SideSell:
		return SideBuy
	default:
		return Side("")
	}
}

// OrderType enumerates the broker order types we issue.
type OrderType string

const (
	OrderTypeMarket OrderType = "MARKET"
	OrderTypeLimit  OrderType = "LIMIT"
	OrderTypeStop   OrderType = "STOP"
	OrderTypeOCO    OrderType = "OCO"
	OrderTypeIFD    OrderType = "IFD"
	OrderTypeIFDOCO OrderType = "IFDOCO"
)

// PlaceOrderRequest is the brokerage-agnostic order intent the usecase layer
// passes to a Broker.
type PlaceOrderRequest struct {
	Symbol        string
	Side          Side
	Type          OrderType
	Quantity      int
	Price         float64 // 0 for market
	TakeProfit    float64 // 0 if absent
	StopLoss      float64 // 0 if absent
	ExecutionType string  // for IFDOCO compatibility; usually "" for MARKET
	ClientTag     string  // optional dedupe / tracing tag
}

// Order is the broker-side order record (what came back from PlaceOrder /
// what we polled).
type Order struct {
	OrderID   string
	Symbol    string
	Side      Side
	Type      OrderType
	Quantity  int
	Price     float64
	Status    string
	CreatedAt time.Time
}

// Execution is a fill record.
type Execution struct {
	ExecutionID string
	OrderID     string
	PositionID  string
	Symbol      string
	Side        Side
	Quantity    int
	Price       float64
	Timestamp   time.Time

	// Per-fill costs reported by GMO. FeeJPY = 約定金額×0.002% commission;
	// SettledSwapJPY = swap/interest realised at close; LossGainJPY = broker-
	// reported realised P&L on a close fill. Optional (0 when GMO omits them).
	// Persisting these is the prerequisite for net-of-cost edge judgment.
	//
	// 符号規約: FeeJPY は「正 = コスト」に正規化済み (GMO wire は
	// キャッシュフロー符号で徴収が負 — gmo_fx.go decodeExecutionList が反転する)。
	// SettledSwapJPY は wire のまま「受取 = 正」。
	FeeJPY         float64
	SettledSwapJPY float64
	LossGainJPY    float64
}

// AccountMargin is the broker account summary used by risk checks.
type AccountMargin struct {
	AvailableJPY float64
	MarginRatio  float64
	Equity       float64
}
