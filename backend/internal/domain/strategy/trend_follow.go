package strategy

import (
	"fmt"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/ta"
)

// trend_follow probe constants — FIXED and NOT swept (sweeping these over the data is the exact
// overfit the pre-registration forbids). Defaults are standard trend-following choices (200-MA
// trend filter, Donchian breakout entry, ATR stops à la Turtle), not tuned to this dataset.
const (
	tf1hPeriod        = 200   // 1h 200SMA = the trend filter (≈8.3 days of context)
	tf1hSlopeLB       = 20    // bars back to measure the 200SMA slope (≈20h)
	tf1hMinSlopePips  = 5.0   // the 200SMA must drift ≥ this over slopeLB to count as a trend
	tfDonchian1hBars  = 48    // continuation trigger: break the high/low of the last 48×1h (≈2 days)
	tfATRWindow       = 14    // 1h ATR for volatility-scaled exits
	tfSLATRMult       = 2.0   // structural SL = 2.0 × ATR(1h)
	tfRatchetArmMult  = 3.0   // ratchet arms at 3.0 × ATR(1h) of profit
	tfRatchetGiveMult = 1.5   // then trails, closing on a 1.5 × ATR(1h) giveback
	tfMaxHoldMin      = 43200 // 30 days — let a trend run; force-close is a backstop, not the exit
	tfTPPipsOff       = 1e6   // TP effectively OFF: the ratchet (not a fixed target) banks the trend
)

// TrendFollow (probe) is the "change the game, not the chart pattern" candidate from
// strategy research: intraday chart patterns die AT the cost floor because their
// per-trade edge ≈ the floor. A trend follower's edge is a whole trend LEG (tens–hundreds of
// pips), so the ~1.7pip floor is negligible, and it holds for days so few trades = low total
// cost. Mechanism (time-series momentum / under-reaction to flow) is a documented premium, not
// a chart shape everyone arbitrages instantly. CRUCIALLY it is BOTH-direction, so unlike the
// JPY-cross breakout it cannot merely harvest the 2023-26 yen-weakness trend — it is tested on
// 2015-2023 (which contains yen STRENGTH) to demand regime robustness.
//
// Rule: 1h 200SMA slope = trend; a Donchian-48 break in that direction = entry; SL = 2×ATR(1h);
// no fixed TP — a wide ATR ratchet rides the move; 30-day MaxHold backstop. Exit is
// strategy-computed (config tp/sl are validator placeholders). FALSIFIER, not a validation.
type TrendFollow struct{}

func (TrendFollow) Name() config.StrategyName { return config.StrategyTrendFollow }

func (TrendFollow) Evaluate(in EvalInput) Signal {
	name := config.StrategyTrendFollow
	none := func(reason string) Signal {
		return Signal{Decision: DecisionNone, StrategyName: name, Reason: reason, CreatedAt: in.Now}
	}
	if in.Config == nil || in.Summary == nil {
		return Signal{Decision: DecisionNone, StrategyName: name, CreatedAt: in.Now}
	}
	dir := in.Config.Entry.Direction
	if dir == config.DirectionNone {
		return Signal{Decision: DecisionNoTrade, StrategyName: name, Reason: "direction=none", CreatedAt: in.Now}
	}
	// Live spread guard. Frictionless in backtest (cost from the CLI cost flags).
	if in.Summary.CurrentRate.SpreadPips > in.Config.Entry.MaxSpreadPips {
		return none("spread_too_wide")
	}
	pip := market.PipSize(in.Config.Symbol)
	if pip <= 0 {
		return none("no_pip")
	}

	// Need enough 1h bars for the 200SMA + its slope lookback, and for the Donchian window.
	n1 := len(in.Candles1h)
	if n1 < tf1hPeriod+tf1hSlopeLB || n1 < tfDonchian1hBars+1 {
		return none("insufficient_1h_history")
	}
	closes1h := closesOf(in.Candles1h)
	sma1h, ok1 := ta.SMA(closes1h, tf1hPeriod)
	sma1hPrev, ok2 := ta.SMA(closes1h[:len(closes1h)-tf1hSlopeLB], tf1hPeriod)
	if !ok1 || !ok2 {
		return none("sma_unavailable")
	}
	H := maTrendDir(sma1h, sma1hPrev, tf1hMinSlopePips, pip)
	if H == trendFlat {
		return none("no_trend")
	}

	atrPips := atrPipsOf(in.Candles1h, tfATRWindow, pip)
	if atrPips <= 0 {
		return none("no_atr")
	}

	// Volatility-floor gate (config MinEntryATRPips; 0 = OFF, live default). Offline-only filter to
	// test the "only follow the trend when 1h ATR ≥ floor" hypothesis — skip low-vol regimes that
	// whipsaw. Swept on in-sample, validated FIXED on OOS; never tuned on live.
	if floor := in.Config.Entry.MinEntryATRPips; floor > 0 && atrPips < floor {
		return none(fmt.Sprintf("atr_below_floor(%.1f<%.1f)", atrPips, floor))
	}

	// Donchian-48 continuation: break the high/low of the last tfDonchian1hBars completed 1h bars
	// in the trend direction. Live price = current mid; the window excludes nothing special since
	// in.Candles1h are completed bars and the break is judged against the live quote.
	win := lastN(in.Candles1h, tfDonchian1hBars)
	donHi, donLo := win[0].High, win[0].Low
	for _, c := range win {
		if c.High > donHi {
			donHi = c.High
		}
		if c.Low < donLo {
			donLo = c.Low
		}
	}
	mid := (in.Summary.CurrentRate.Bid + in.Summary.CurrentRate.Ask) / 2

	var side order.Side
	switch {
	case H == trendUp && mid >= donHi:
		side = order.SideBuy
	case H == trendDown && mid <= donLo:
		side = order.SideSell
	default:
		return none("no_continuation")
	}
	if !directionAllows(dir, side) {
		return none("direction_blocked")
	}

	entry := in.Summary.CurrentRate.Ask
	if side == order.SideSell {
		entry = in.Summary.CurrentRate.Bid
	}
	return Signal{
		Decision:            DecisionEnter,
		Side:                side,
		EntryPrice:          entry,
		TakeProfitPips:      tfTPPipsOff,
		StopLossPips:        tfSLATRMult * atrPips,
		RatchetArmPips:      tfRatchetArmMult * atrPips,
		RatchetGivebackPips: tfRatchetGiveMult * atrPips,
		MaxHoldMinutes:      tfMaxHoldMin,
		Quantity:            in.Config.Risk.Quantity,
		Reason:              fmt.Sprintf("trend follow %s atr=%.0fpips SL=%.0f", side, atrPips, tfSLATRMult*atrPips),
		ConfigID:            in.Config.ConfigID,
		StrategyName:        name,
		CreatedAt:           in.Now,
	}
}
