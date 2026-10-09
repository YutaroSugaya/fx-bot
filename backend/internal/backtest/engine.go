package backtest

import (
	"context"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/safety"
	"fx-bot/backend/internal/usecase"
)

// Engine drives a fixed-config replay over a 1-minute candle stream.
// Stateful so future Mode B (advisor-replay) can call Replay multiple times
// with different StrategyConfigs while keeping accumulated state.
//
// Strategies is exposed so tests can register synthetic strategies (e.g. an
// always-buy fixture) without depending on the production momentum_pullback /
// breakout_follow signal generation logic.
type Engine struct {
	cfg        EngineConfig
	Strategies *strategy.Engine
}

// NewEngine constructs an Engine. cfg.StrategyConfig is fixed for the Engine's
// lifetime in Mode A. Construct a new Engine for each scenario.
func NewEngine(cfg EngineConfig) *Engine {
	return &Engine{
		cfg:        cfg,
		Strategies: strategy.NewEngine(),
	}
}

// openPosition is the engine's internal tracking for a single open position.
// Encapsulates everything needed to evaluate TP/SL/MaxHold on subsequent bars.
// maxHistoryBars caps the 1m history window fed to the strategy each bar
// for performance. ~20,000 1m bars ≈ 14 days, covering ma_pullback's
// deepest lookback (1h 200SMA slope ≈ 13,200 1m bars) + the 24h summary bucket
// with margin. Makes the replay O(n) instead of O(n²).
const maxHistoryBars = 20000

type openPosition struct {
	side           order.Side
	entryPrice     float64
	tpPips         float64
	slPips         float64
	maxHoldMinutes int
	openedAt       time.Time
	quantity       int

	// Trailing ratchet state. armPips/givePips come
	// from the Signal (0/0 = ratchet off). peakUnrealizedPips accumulates the
	// favorable excursion across bars; ratchetArmed flips once peak ≥ armPips.
	// Once armed, a giveback of givePips from peak closes the position
	// ("ratchet_takeprofit"). Bar-resolution + pessimistic (see evaluateExit).
	armPips            float64
	givePips           float64
	peakUnrealizedPips float64
	ratchetArmed       bool
}

// Replay streams the candles through the strategy engine + risk gate, opens
// positions on entry signals, and resolves exits (TP/SL/MaxHold) per bar.
// Returns the trade list, equity curve, and computed metrics.
//
// Bar convention:
//   - "now" for signal evaluation = bar.OpenTime + 1m (= bar close)
//   - Entry fills at bar close (signal bar's Close price + slippage)
//   - Exits are checked starting at the NEXT bar (avoids same-bar look-ahead)
//   - TP/SL hit detection uses bar.High / bar.Low
//   - When both levels are touched in one bar, ConflictPolicy decides
func (e *Engine) Replay(ctx context.Context, candles []market.Candle) (Result, error) {
	pip := market.PipSize(e.cfg.Symbol)
	// Resolve the quote→JPY multiplier once. JPY-quote pairs → 1.0 (unchanged).
	// USD-quote pairs use AssumedUSDJPYRate; if unset, factor stays 1.0 and the
	// JPY column is in quote currency (pips remains the currency-agnostic truth).
	costs := e.cfg.Costs
	if mul, err := market.QuoteJPYRate(e.cfg.Symbol, e.cfg.AssumedUSDJPYRate); err == nil {
		costs.quoteJPYRate = mul
	}
	// makeTrade の SwapTable 参照用に symbol を解決しておく
	// (quoteJPYRate と同じ「Replay で一度だけ解決」パターン)。
	costs.symbol = e.cfg.Symbol
	result := Result{}
	var open *openPosition

	for i, bar := range candles {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}
		now := bar.OpenTime.Add(time.Minute) // bar close

		// History EXCLUDES the current bar — at bar close in Live, the just-
		// finished bar is included in the aggregator, but the strategy is
		// re-evaluated continuously between bars too. Including the current
		// bar in Summary while comparing CurrentRate.Ask to Summary.High
		// makes breakout impossible (Ask ≤ High by definition). Excluding
		// it matches the realistic comparison: "this bar's close broke
		// above the prior 6h range".
		// Cap history to the last maxHistoryBars 1m bars instead
		// of the full prefix candles[:i]. The full prefix made each iteration
		// O(i) (BuildMarketSummary + Resample copy the whole slice), so a full
		// replay was O(n²) — a 3-month 1m run never finished. Strategies only
		// read recent bars: ma_pullback's deepest lookback is the 1h 200SMA slope
		// = (200+20) 1h bars ≈ 13,200 1m bars; the 24h summary bucket = 1,440.
		// 20,000 1m bars (~14 days) covers every lookback with margin, making the
		// replay O(n). Behaviour-preserving once i ≥ the deepest lookback.
		maxHist := e.cfg.MaxHistoryBars
		if maxHist <= 0 {
			maxHist = maxHistoryBars
		}
		lo := 0
		if i > maxHist {
			lo = i - maxHist
		}
		history := candles[lo:i]

		// 1) Resolve exits on any open position FIRST. Same-bar entry then exit
		//    is disallowed by the next-bar exit rule, so exits only run for
		//    positions opened on a prior bar.
		if open != nil {
			tr, hitAmbiguous := evaluateExit(open, bar, now, pip, e.cfg.ConflictPolicy, costs)
			if hitAmbiguous {
				result.AmbiguousBars++
			}
			if tr != nil {
				result.Trades = append(result.Trades, *tr)
				open = nil
			}
		}

		// 2) If still flat, evaluate new entry. Rebuild MarketSummary from
		//    `history` so production strategies see real window stats
		//    (Summary1h/6h/24h). Frictionless ticker: bid = ask = bar.Close.
		if open == nil {
			ticker := &market.Ticker{
				Symbol: e.cfg.Symbol, Bid: bar.Close, Ask: bar.Close, Timestamp: now,
			}
			summary := usecase.BuildMarketSummary(usecase.BuildMarketSummaryInput{
				Symbol:    e.cfg.Symbol,
				Mode:      "backtest_mode_a",
				Now:       now,
				Candles1m: history,
				Ticker:    ticker,
			})
			in := strategy.EvalInput{
				Now:       now,
				Config:    e.cfg.StrategyConfig,
				Summary:   summary,
				Candles1m: history,
				Candles5m: market.Resample(history, 5*time.Minute),
				// 1h series for the MTF pullback strategy. Existing
				// strategies ignore it; without it mtf_pullback would always
				// short-circuit on insufficient_1h_candles and silently trade 0.
				Candles1h: market.Resample(history, time.Hour),
			}
			sig := e.Strategies.Evaluate(in)
			if sig.Decision == strategy.DecisionEnter && sig.Side.Valid() {
				entry := bar.Close
				// Entry adverse fill = per-leg slip (fixed SlippagePips + half the
				// modeled time-of-day spread at `now`), same basis as the exit legs.
				if slip := costs.legSlipPips(now); slip != 0 {
					if sig.Side == order.SideBuy {
						entry += slip * pip
					} else {
						entry -= slip * pip
					}
				}
				// Quantity comes from the signal (populated by strategy from
				// config.Risk.Quantity at evaluation time). Matches production
				// defaultQty(sig) in execute_order.go. Fall back to config-level
				// quantity for resilience if a strategy emits an Enter without
				// populating sig.Quantity (test fixtures, future strategies).
				// Ultimate fallback = safety.DefaultQuantity (実証 min = 1,000)
				// so a strategy bug doesn't silently produce sub-min trades.
				qty := sig.Quantity
				if qty == 0 {
					qty = e.cfg.StrategyConfig.Risk.Quantity
				}
				if qty == 0 {
					qty = safety.DefaultQuantity
				}
				open = &openPosition{
					side:           sig.Side,
					entryPrice:     entry,
					tpPips:         sig.TakeProfitPips,
					slPips:         sig.StopLossPips,
					maxHoldMinutes: sig.MaxHoldMinutes,
					openedAt:       now,
					quantity:       qty,
					armPips:        sig.RatchetArmPips,
					givePips:       sig.RatchetGivebackPips,
				}
			}
		}
	}

	result.Metrics = ComputeMetrics(result.Trades)
	return result, nil
}

// evaluateExit checks whether bar's OHLC crosses TP, SL, or MaxHold for the
// given open position. Returns (closed Trade, ambiguousBar) — when the bar
// is ambiguous (both TP and SL touched) and policy == SkipAmbiguous, the
// trade is nil and ambiguous is true (carry to next bar).
func evaluateExit(p *openPosition, bar market.Candle, now time.Time, pip float64,
	policy ConflictPolicy, costs CostModel) (*Trade, bool) {

	var tpPrice, slPrice float64
	switch p.side {
	case order.SideBuy:
		tpPrice = p.entryPrice + p.tpPips*pip
		slPrice = p.entryPrice - p.slPips*pip
	case order.SideSell:
		tpPrice = p.entryPrice - p.tpPips*pip
		slPrice = p.entryPrice + p.slPips*pip
	}

	tpHit := false
	slHit := false
	if p.side == order.SideBuy {
		tpHit = bar.High >= tpPrice
		slHit = bar.Low <= slPrice
	} else {
		tpHit = bar.Low <= tpPrice
		slHit = bar.High >= slPrice
	}

	// Conflict resolution (structural TP vs SL on the same bar).
	if tpHit && slHit {
		switch policy {
		case OptimisticTPFirst:
			return makeTrade(*p, applyExitSlippage(tpPrice, p.side, pip, costs, now), now, "take_profit", pip, costs), true
		case SkipAmbiguous:
			return nil, true
		default: // PessimisticSLFirst
			return makeTrade(*p, applyExitSlippage(slPrice, p.side, pip, costs, now), now, "stop_loss", pip, costs), true
		}
	}
	if tpHit {
		return makeTrade(*p, applyExitSlippage(tpPrice, p.side, pip, costs, now), now, "take_profit", pip, costs), false
	}

	// Trailing ratchet — the PRIMARY exit of ratchet-driven runner strategies. Checked
	// BEFORE the structural SL because, once armed, the trailing stop (peak −
	// giveback, in profit) sits ABOVE the SL (in loss), so a retrace hits it
	// first. PESSIMISTIC + bar-resolution: armed-ness and peak come from PRIOR
	// bars (this bar's high cannot inflate the peak before the retrace check),
	// and we model the fill AT the trailing-stop level. GMO OCO can't trail, so
	// this only reproduces the bot-side (process-alive) exit — interpret it as
	// downward-biased (pessimistic) by construction.
	if p.ratchetArmed && p.givePips > 0 {
		ratchetStopPips := p.peakUnrealizedPips - p.givePips
		var stopPrice float64
		hit := false
		if p.side == order.SideBuy {
			stopPrice = p.entryPrice + ratchetStopPips*pip
			hit = bar.Low <= stopPrice
		} else {
			stopPrice = p.entryPrice - ratchetStopPips*pip
			hit = bar.High >= stopPrice
		}
		if hit {
			return makeTrade(*p, applyExitSlippage(stopPrice, p.side, pip, costs, now), now, "ratchet_takeprofit", pip, costs), false
		}
	}

	if slHit {
		return makeTrade(*p, applyExitSlippage(slPrice, p.side, pip, costs, now), now, "stop_loss", pip, costs), false
	}

	// Update the ratchet peak from THIS bar's favorable excursion, then arm.
	// Done AFTER the retrace check so a position armed this bar can only
	// ratchet-close from the next bar onward (pessimistic).
	if p.armPips > 0 {
		var favPips float64
		if p.side == order.SideBuy {
			favPips = (bar.High - p.entryPrice) / pip
		} else {
			favPips = (p.entryPrice - bar.Low) / pip
		}
		if favPips > p.peakUnrealizedPips {
			p.peakUnrealizedPips = favPips
		}
		if p.peakUnrealizedPips >= p.armPips {
			p.ratchetArmed = true
		}
	}

	// MaxHold check (time-based, evaluated at bar close).
	if p.maxHoldMinutes > 0 {
		deadline := p.openedAt.Add(time.Duration(p.maxHoldMinutes) * time.Minute)
		if !now.Before(deadline) {
			exit := applyExitSlippage(bar.Close, p.side, pip, costs, now)
			return makeTrade(*p, exit, now, "max_hold", pip, costs), false
		}
	}
	return nil, false
}

// applyExitSlippage moves the exit price in the adverse direction. For a BUY
// close (= sell) the bid is hit at exit - slip; for SELL close (= buy back)
// the ask is hit at exit + slip. Zero-cost passes the price through unchanged.
func applyExitSlippage(price float64, side order.Side, pip float64, costs CostModel, now time.Time) float64 {
	slip := costs.legSlipPips(now) // SlippagePips + half the modeled spread at `now`
	if slip == 0 {
		return price
	}
	if side == order.SideBuy {
		return price - slip*pip
	}
	return price + slip*pip
}

// makeTrade builds a Trade from the closing facts. Pure function; ApplyCosts
// (fee deduction) folded in here.
func makeTrade(p openPosition, exitPrice float64, closedAt time.Time, reason string,
	pip float64, costs CostModel) *Trade {

	var pips float64
	switch p.side {
	case order.SideBuy:
		pips = (exitPrice - p.entryPrice) / pip
	case order.SideSell:
		pips = (p.entryPrice - exitPrice) / pip
	}
	// JPY PnL. For JPY-quote pairs the quote IS JPY so pip*quantity is already
	// JPY. For USD-quote pairs (EUR_USD) pip*quantity is USD; quoteJPYRate (the
	// resolved USD/JPY multiplier, 1.0 for JPY quote) converts it to JPY.
	mul := costs.quoteJPYRate
	if mul <= 0 {
		mul = 1.0 // unset (e.g. USD-quote backtest with no AssumedUSDJPYRate): leave in quote currency.
	}
	jpy := pips * pip * float64(p.quantity) * mul
	jpy -= costs.FeeJPYPerTrade
	// GMO 約定金額×rate% 手数料 (往復 = entry + exit notional)。
	if costs.FeeRatePct > 0 {
		notionalQuote := (p.entryPrice + exitPrice) * float64(p.quantity)
		jpy -= notionalQuote * (costs.FeeRatePct / 100.0) * mul
	}
	// スワップ (円/晩テーブル × 21:00 UTC 跨ぎ夜数、水曜 3 倍)。
	// 符号付き (負 = 支払い) なので加算。nil テーブルは 0 (back-compat)。
	// ProfitLossJPY は fee/slippage 同様 NET を維持し、内訳は SwapJPY に透明記録。
	swap := costs.SwapTable.SwapJPY(costs.symbol, string(p.side), p.openedAt, closedAt, p.quantity)
	jpy += swap
	return &Trade{
		Side:           string(p.side),
		EntryPrice:     p.entryPrice,
		ExitPrice:      exitPrice,
		OpenedAt:       p.openedAt,
		ClosedAt:       closedAt,
		ProfitLossPips: pips,
		ProfitLossJPY:  jpy,
		SwapJPY:        swap,
		CloseReason:    reason,
	}
}
