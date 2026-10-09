package strategy

import (
	"fmt"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/ta"
)

// daily_trend probe constants — FIXED, NOT swept (pre-registered before the backtest).
// Classic Turtle-style daily trend: 200-day SMA slope = trend, Donchian-55 breakout = entry,
// ATR-scaled stop + ratchet. The point vs trend_follow (1h) is the LOWER FREQUENCY: a daily
// position spans weeks, so the per-trade move (100-400 pips) dwarfs the 1.7-3pip cost floor.
const (
	dtSMAPeriod       = 200    // 200-day SMA = the trend filter
	dtSlopeLB         = 20     // bars back to measure SMA slope (sign only, no pip threshold)
	dtDonchianBars    = 55     // continuation entry: break the 55-day high/low (Turtle)
	dtATRWindow       = 14     // daily ATR for volatility-scaled exits
	dtSLATRMult       = 3.0    // structural SL = 3.0 × ATR(daily)
	dtRatchetArmMult  = 3.0    // ratchet arms at 3.0 × ATR of profit
	dtRatchetGiveMult = 1.5    // then trails, closing on a 1.5 × ATR giveback
	dtMaxHoldMin      = 129600 // 90 days — let a trend run; backstop only
	dtTPPipsOff       = 1e7    // TP effectively OFF: ratchet banks the trend
)

// DailyTrend (probe) runs on DAILY bars (the engine feeds them via in.Candles1m in daily-replay
// mode). Restricted in config to EUR/USD + GBP/USD — the confound-free pairs (no yen-trend), so a
// surviving edge here would be a clean, holdout-robust signal. Exit is
// strategy-computed. FALSIFIER, not a validation (pre-registered).
type DailyTrend struct{}

func (DailyTrend) Name() config.StrategyName { return config.StrategyDailyTrend }

func (DailyTrend) Evaluate(in EvalInput) Signal {
	name := config.StrategyDailyTrend
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
	if in.Summary.CurrentRate.SpreadPips > in.Config.Entry.MaxSpreadPips {
		return none("spread_too_wide")
	}
	pip := market.PipSize(in.Config.Symbol)
	if pip <= 0 {
		return none("no_pip")
	}

	// Daily bars arrive in in.Candles1m (the engine's replay-tf series). Need enough for the
	// 200-day SMA + its slope lookback and the Donchian window.
	bars := in.Candles1m
	n := len(bars)
	if n < dtSMAPeriod+dtSlopeLB || n < dtDonchianBars {
		return none("insufficient_daily_history")
	}
	closes := closesOf(bars)
	sma, ok1 := ta.SMA(closes, dtSMAPeriod)
	smaPrev, ok2 := ta.SMA(closes[:len(closes)-dtSlopeLB], dtSMAPeriod)
	if !ok1 || !ok2 {
		return none("sma_unavailable")
	}
	// Trend = sign of the 200-day SMA slope (no pip threshold: avoids a tuned knob and is
	// pair-agnostic across EUR/USD & GBP/USD which have different pip scales).
	var up bool
	switch {
	case sma > smaPrev:
		up = true
	case sma < smaPrev:
		up = false
	default:
		return none("flat_trend")
	}

	atrPips := atrPipsOf(bars, dtATRWindow, pip)
	if atrPips <= 0 {
		return none("no_atr")
	}

	win := lastN(bars, dtDonchianBars)
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
	case up && mid >= donHi:
		side = order.SideBuy
	case !up && mid <= donLo:
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
		TakeProfitPips:      dtTPPipsOff,
		StopLossPips:        dtSLATRMult * atrPips,
		RatchetArmPips:      dtRatchetArmMult * atrPips,
		RatchetGivebackPips: dtRatchetGiveMult * atrPips,
		MaxHoldMinutes:      dtMaxHoldMin,
		Quantity:            in.Config.Risk.Quantity,
		Reason:              fmt.Sprintf("daily trend %s atr=%.1fpips SL=%.0f", side, atrPips, dtSLATRMult*atrPips),
		ConfigID:            in.Config.ConfigID,
		StrategyName:        name,
		CreatedAt:           in.Now,
	}
}
