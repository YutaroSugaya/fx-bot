// Package livesignal holds the most recent live strategy evaluation per symbol
// so the dashboard can show what the bot ACTUALLY decided this tick (strategy
// outcome + risk-gate outcome), as opposed to a frontend reproduction.
//
// It is a leaf package (stdlib only) so both the worker (writer) and the HTTP
// handler (reader) can depend on it without an import cycle.
package livesignal

import (
	"sync"
	"time"
)

// Snapshot is one tick's live evaluation result for a symbol. It is the bot's
// real decision, not an estimate: Decision/Side/Reason come from the strategy,
// Executed/GateBlocked/GateReason from the risk gate + executor.
type Snapshot struct {
	Symbol       string    `json:"symbol"`
	EvaluatedAt  time.Time `json:"evaluated_at"`
	StrategyName string    `json:"strategy_name"`

	// Decision: "enter" | "none" | "no_trade".
	Decision string `json:"decision"`
	Side     string `json:"side"` // "BUY" | "SELL" | ""
	// Reason is the strategy's own reason (e.g. "no_pullback",
	// "trend=up pullback"). Empty on a clean enter.
	Reason string `json:"reason"`

	// Executed is true when an order was actually placed this tick.
	Executed bool `json:"executed"`
	// GateBlocked is true when the strategy wanted to enter but the risk gate
	// stopped it. GateReason then explains which gate (cooldown, open_positions,
	// consecutive_losses_freeze, spread, …).
	GateBlocked bool   `json:"gate_blocked"`
	GateReason  string `json:"gate_reason"`

	// Indicator context the bot saw (from the live MarketSummary / config).
	Trend6h       string  `json:"trend_6h"`
	Trend24h      string  `json:"trend_24h"`
	SpreadPips    float64 `json:"spread_pips"`
	MaxSpreadPips float64 `json:"max_spread_pips"`
	Atr6hPips     float64 `json:"atr_6h_pips"`

	EntryPrice     float64 `json:"entry_price,omitempty"`
	TakeProfitPips float64 `json:"take_profit_pips,omitempty"`
	StopLossPips   float64 `json:"stop_loss_pips,omitempty"`

	// Price is the live mid (bid+ask)/2 the bot saw this tick — so the dashboard
	// can show where price sits relative to the 200MAs below.
	Price float64 `json:"price,omitempty"`

	// The two 200-period MAs the ma_pullback strategy watches on the 5m
	// chart, computed from the same candle window the bot evaluated. 0 when
	// there aren't yet 200 bars. The dashboard shows them (and where the live
	// price sits relative to them) so the operator sees the bot's MA view.
	Sma5m200 float64 `json:"sma_5m_200,omitempty"`
	Ema5m200 float64 `json:"ema_5m_200,omitempty"`

	// Gates is the ma_pullback entry funnel (one step per condition, in
	// evaluation order) so the dashboard can show "現在どこまで通過し、次に何が
	// 必要か". Empty for other strategies. The first OK=false is the active blocker.
	Gates []Gate `json:"gates,omitempty"`
}

// Gate is one step of the entry funnel: a condition + whether it's currently met
// + the live numbers behind it. Mirrors strategy.Gate (mapped at the app layer to
// keep this package a stdlib-only leaf).
type Gate struct {
	Key    string `json:"key"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Holder is a concurrency-safe latest-snapshot-per-symbol store. The worker
// goroutine writes via Set each tick; the HTTP handler reads via Get.
type Holder struct {
	mu sync.RWMutex
	m  map[string]*Snapshot
}

func NewHolder() *Holder { return &Holder{m: map[string]*Snapshot{}} }

func (h *Holder) Set(s *Snapshot) {
	if h == nil || s == nil || s.Symbol == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m[s.Symbol] = s
}

func (h *Holder) Get(symbol string) *Snapshot {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.m[symbol]
}
