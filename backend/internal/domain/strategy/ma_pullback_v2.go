package strategy

import (
	"fmt"
	"math"
	"sort"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/ta"
)

// MAPullbackV2 is a deliberately SIMPLE trend-following daytrade derived
// from ma_pullback, changing exactly the two loss levers a 2.4-year backtest
// diagnosis of ma_pullback exposed (PF 0.72 over the whole pool):
//
//   - ENTRY quality (ma_pullback hit a 65% stop-out rate in backtest, buying
//     pullbacks that broke structure in weak/false trends). ma_pullback_v2:
//
//   - reads the TREND off the 1h 200EMA (faster than the SMA) with a HIGHER
//     slope gate (maPBV2_1hMinSlopePips) → demand a clear trend, skip chop;
//
//   - adds a higher-low / lower-high STRUCTURE gate (higherLowIntact) → only
//     buy a pullback whose structure is still intact, not a falling knife.
//     (ma_pullback's confluence gate is intentionally dropped — the structure gate
//     targets the diagnosed failure more directly and keeps the rule set simple.)
//
//   - EXIT payoff (ma_pullback's trailing ratchet capped winners at ≈+13 pips while the
//     rare structural TP ran ≈+30; break-even needed payoff 1.90 vs the backtested
//     1.52). ma_pullback_v2 uses a SINGLE fixed broker-OCO exit: structural SL + a fixed
//     TP = maPBV2TPRMultiple × SL. NO trailing ratchet, NO early-exit. The
//     structural SL sits broker-side as the worst-case floor (bot-death-proof),
//     so the exit is fully reproducible in live (the ma_pullback ratchet is bot-side).
//     A shortened daytrade time stop (maPBV2MaxHoldMinutes) recycles a staller.
//
// This is a SEPARATE strategy: ma_pullback.go is untouched.
// It REUSES ma_pullback's pure helpers (inMAZone / priceOnTrendSide / maTrendDir /
// structuralSLPips / swings / ATR) so the two share one definition of the shared
// mechanics and cannot drift. ma_pullback_v2 must pass the full offline gauntlet before any
// live consideration (new config_id, sample count reset).
type MAPullbackV2 struct {
	// P is the (offline) tuning-parameter injection point. zero-value ==
	// DefaultMAPullbackV2Params() because the registry registers MAPullbackV2{}.
	P MAPullbackV2Params
}

func (MAPullbackV2) Name() config.StrategyName { return config.StrategyMAPullbackV2 }

// MAPullbackV2Params holds the ma_pullback_v2 tunables. MA periods (200) / ATR window (14)
// / swing fractal width stay fixed constants (strategy identity — keeping the
// sweep surface small avoids multiple-comparison memorization).
type MAPullbackV2Params struct {
	TrendSlopeLookback1h int     // 1h 200EMA slope lookback (bars)
	TrendMinSlopePips    float64 // min 1h 200EMA drift over the lookback to call a trend
	ZoneATR              float64 // pullback zone half-width (×ATR)
	SwingLookback        int     // 5m bars the structure swings are read over
	SwingPromPips        float64 // swing prominence noise gate
	SLBufferPips         float64 // SL sits this far beyond the defended 5m 200MA
	SLMinPips            float64 // SL floor
	SLMaxPips            float64 // SL clip
	TPRMultiple          float64 // TP = this × SL (fixed R-multiple, the payoff fix)
	MaxHoldMinutes       int     // daytrade time stop
}

// ma_pullback_v2 defaults. Trend gate raised vs ma_pullback (8 vs 5 pips) and read off the EMA;
// MaxHold shortened to a daytrade horizon (240 vs 480); ratchet removed entirely.
const (
	maPBV2_1hMinSlopePips = 8.0 // 1h 200EMA must drift ≥ this over the lookback (ma_pullback: 5.0 SMA)
	maPBV2SLBufferPips    = 2.0
	maPBV2SLMinPips       = 8.0
	maPBV2SLMaxPips       = 20.0
	maPBV2TPRMultiple     = 2.0 // TP = 2 × SL → take-profit becomes the primary exit
	maPBV2MaxHoldMinutes  = 240 // 4h daytrade time stop (ma_pullback: 480)
)

// DefaultMAPullbackV2Params is ma_pullback_v2's effective live value. Shared 5m/zone/swing
// settings reuse the ma_pullback constants (same execution chart) so the two families
// stay comparable; only the v2-specific levers differ.
func DefaultMAPullbackV2Params() MAPullbackV2Params {
	return MAPullbackV2Params{
		TrendSlopeLookback1h: maPB1hSlopeLB, // reuse 20-bar slope window
		TrendMinSlopePips:    maPBV2_1hMinSlopePips,
		ZoneATR:              maPBZoneATR, // reuse 0.8
		SwingLookback:        maPBSwingLookback,
		SwingPromPips:        maPBSwingPromPips,
		SLBufferPips:         maPBV2SLBufferPips,
		SLMinPips:            maPBV2SLMinPips,
		SLMaxPips:            maPBV2SLMaxPips,
		TPRMultiple:          maPBV2TPRMultiple,
		MaxHoldMinutes:       maPBV2MaxHoldMinutes,
	}
}

func (p MAPullbackV2Params) effective() MAPullbackV2Params {
	if p == (MAPullbackV2Params{}) {
		return DefaultMAPullbackV2Params()
	}
	return p
}

func (m MAPullbackV2) Evaluate(in EvalInput) Signal {
	name := config.StrategyMAPullbackV2
	p := m.P.effective()
	none := func(reason string) Signal {
		return Signal{Decision: DecisionNone, StrategyName: name, Reason: reason, CreatedAt: in.Now}
	}
	if in.Config == nil || in.Summary == nil {
		return Signal{Decision: DecisionNone, StrategyName: name, CreatedAt: in.Now}
	}
	// panic-safe guard (negative/0 lookback breaks the slice math below).
	if p.TrendSlopeLookback1h < 1 || p.SwingLookback < 1 {
		return none("invalid_params")
	}
	dir := in.Config.Entry.Direction
	if dir == config.DirectionNone {
		return Signal{Decision: DecisionNoTrade, StrategyName: name, Reason: "direction=none", CreatedAt: in.Now}
	}
	if in.Summary.CurrentRate.SpreadPips > in.Config.Entry.MaxSpreadPips {
		return none("spread_too_wide")
	}
	pip := market.PipSize(in.Config.Symbol)
	price := (in.Summary.CurrentRate.Bid + in.Summary.CurrentRate.Ask) / 2
	if price <= 0 || pip <= 0 {
		return none("no_price")
	}

	// --- 1h trend H: direction from the 1h 200EMA SLOPE (EMA, faster than ma_pullback's SMA) ---
	if len(in.Candles1h) < maPB1hPeriod+p.TrendSlopeLookback1h {
		return none("insufficient_1h_candles")
	}
	closes1h := closesOf(in.Candles1h)
	ema1h, ok1h := ta.EMA(closes1h, maPB1hPeriod)
	ema1hPrev, ok1hP := ta.EMA(closes1h[:len(closes1h)-p.TrendSlopeLookback1h], maPB1hPeriod)
	if !ok1h || !ok1hP {
		return none("insufficient_1h_candles")
	}
	H := maTrendDir(ema1h, ema1hPrev, p.TrendMinSlopePips, pip)
	if H == trendFlat {
		return none("no_trend")
	}

	// --- 5m 200MAs: pullback TARGET (zone) + structural stop line ---
	if len(in.Candles5m) < maPBMin5mBars {
		return none("insufficient_5m_candles")
	}
	closes := closesOf(in.Candles5m)
	sma, ok1 := ta.SMA(closes, maPB5mPeriod)
	ema, ok2 := ta.EMA(closes, maPB5mPeriod)
	if !ok1 || !ok2 {
		return none("insufficient_5m_candles")
	}

	atrPips := atrPipsOf(in.Candles5m, maPBATRWindow, pip)
	zoneTol := p.ZoneATR * atrPips * pip
	if !inMAZone(price, sma, ema, zoneTol) {
		return none("not_in_ma_zone") // 待ち: MA まで戻らなければ追わない
	}
	if !priceOnTrendSide(price, sma, H) {
		return none("wrong_side_of_ma")
	}

	// --- STRUCTURE gate: higher-low (up) / lower-high (down) intact.
	// Don't buy a pullback that has already broken structure (falling knife). ---
	cWin := lastN(in.Candles5m, p.SwingLookback)
	highs := ta.SwingHighs(cWin, maPBSwingN, pip, p.SwingPromPips)
	lows := ta.SwingLows(cWin, maPBSwingN, pip, p.SwingPromPips)
	if !higherLowIntact(lows, highs, H) {
		return none("structure_broken")
	}

	// --- 反発確認: last 5m bar closes back in the trend direction ---
	if !reboundConfirmed(in.Candles5m[len(in.Candles5m)-1], H) {
		return none("no_rebound")
	}

	// --- enter with the trend (continuation after the pullback) ---
	side := order.SideBuy
	if H == trendDown {
		side = order.SideSell
	}
	if !directionAllows(dir, side) {
		return none("direction_blocked")
	}
	entry := in.Summary.CurrentRate.Ask
	if side == order.SideSell {
		entry = in.Summary.CurrentRate.Bid
	}

	// --- single fixed broker-OCO exit: structural SL + TP = R × SL ---
	maForSL := math.Min(sma, ema)
	if side == order.SideSell {
		maForSL = math.Max(sma, ema)
	}
	slPips := structuralSLPips(entry, maForSL, side, p.SLBufferPips, p.SLMinPips, p.SLMaxPips, pip)
	tpPips := rMultipleTPPips(slPips, p.TPRMultiple)

	return Signal{
		Decision:       DecisionEnter,
		Side:           side,
		EntryPrice:     entry,
		TakeProfitPips: tpPips,
		StopLossPips:   slPips,
		MaxHoldMinutes: p.MaxHoldMinutes,
		// ratchet / extension / early-exit all OFF: a single fixed broker OCO
		// (SL + TP = R×SL) IS the entire exit. Live-faithful (the SL/TP both sit
		// broker-side so bot death can't strip the protection — ma_pullback's bot-side
		// ratchet could).
		RatchetArmPips:      0,
		RatchetGivebackPips: 0,
		Quantity:            in.Config.Risk.Quantity,
		Reason:              fmt.Sprintf("%s ma_pullback_v2 200EMA pullback TP=%.0f(=%.1fR) SL=%.0f", H, tpPips, p.TPRMultiple, slPips),
		ConfigID:            in.Config.ConfigID,
		StrategyName:        name,
		CreatedAt:           in.Now,
	}
}

// higherLowIntact reports whether the pullback respects trend structure: in an
// uptrend the LATEST swing low (highest Index) must sit ABOVE the prior swing
// low (a higher-low), in a downtrend the latest swing high BELOW the prior (a
// lower-high). Fewer than 2 relevant swings → false: structure is unconfirmed
// and we decline rather than catch a falling knife on an unproven pullback.
func higherLowIntact(lows, highs []ta.Swing, trend trendDir) bool {
	var s []ta.Swing
	switch trend {
	case trendUp:
		s = append([]ta.Swing{}, lows...)
	case trendDown:
		s = append([]ta.Swing{}, highs...)
	default:
		return false
	}
	if len(s) < 2 {
		return false
	}
	sort.Slice(s, func(i, j int) bool { return s[i].Index > s[j].Index }) // latest first
	latest, prior := s[0], s[1]
	if trend == trendUp {
		return latest.Price > prior.Price
	}
	return latest.Price < prior.Price
}

// rMultipleTPPips returns the take-profit distance as a fixed multiple of the
// stop distance — the payoff fix that makes a real structural take-profit
// (not a trailing give-back) the primary exit.
func rMultipleTPPips(slPips, mult float64) float64 { return slPips * mult }
