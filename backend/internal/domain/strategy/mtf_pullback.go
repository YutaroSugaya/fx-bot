package strategy

import (
	"fmt"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/ta"
)

// MTFPullback is the multi-timeframe trend-pullback strategy distilled
// from a publicly described discretionary method, with the hand-drawn lines replaced by ta geometry primitives:
//
//   - 1h bias H: the higher-timeframe direction (regression slope + swing
//     structure must agree). Environment recognition only — never a location
//     gate.
//   - 5m leg L: the short-timeframe direction. We only act when L is NOT
//     aligned with H (price is pulling back against the 1h trend).
//   - 5m wall: the pullback's lower-highs (uptrend) / higher-lows (downtrend)
//     form a trendline price has respected >=3 times (ta.TouchedLine). This is
//     the LOCATION only.
//   - Trigger (on 1m): the 5m wall is projected onto each 1m bar's time; the
//     1st touch is skipped and we enter the moment a 1m bar closes back through
//     the wall in the 1h-trend direction ("1回目スルー・2回目にブレイク").
//   - Exit (structural): SL is derived from the wall ("逆側に
//     ブレイクしたら損切り", clipped to SL_max), TP1 from the 1m channel line
//     (min(1m line, ~20pips)). Both are computed at entry and expressed as pip
//     distances so they ride the existing Signal → broker-OCO / position path
//     with no exit-engine change. ratchet/trailing is dropped (公開されている裁量手法に無い).
//
// Not implemented (deferred): the SECOND position + mid-line TP2 (scale-out),
// which need the cap and the close-saga reworked. The touch state the method
// would keep in an engine-side store is RECOMPUTED from the candle window each tick,
// keeping Evaluate pure; the D_exit hysteresis is honoured in the
// recompute.
type MTFPullback struct{}

func (MTFPullback) Name() config.StrategyName { return config.StrategyMTFPullback }

// Tunable defaults (未決パラメータ). Pip-denominated
// thresholds are scale-invariant via market.PipSize, so they read the same on
// JPY and USD-quote pairs. Kept as constants (not config), mirroring
// momentum_pullback's local constants; a future walk-forward sweep promotes the
// winners.
const (
	mtfLookback1h     = 48 // bars of 1h history to read (≈2 days)
	mtfN1h            = 2  // fractal half-width for 1h swings
	mtfMinProm1hPips  = 8  // 1h swing must protrude ≥ this (noise gate)
	mtfMinDrift1hPips = 15 // 1h regression must drift ≥ this to be a trend
	mtfMin1hBars      = 12 // need at least this many 1h bars to judge the trend

	mtfLookback5m     = 36 // bars of 5m history (≈3h) the wall is fit over
	mtfN5m            = 2  // fractal half-width for 5m swings
	mtfMinProm5mPips  = 3  // 5m swing prominence gate
	mtfMinDrift5mPips = 4  // 5m regression drift threshold
	mtfMaxResid5mPips = 5  // wall touches must lie within this RMS of a line
	mtfMin5mBars      = 10 // need at least this many 5m bars
	mtf5mBarMinutes   = 5  // 5m wall bar width (denominator projecting onto 1m)

	mtfN1m           = 2  // fractal half-width for 1m swings (1m channel for TP1)
	mtfMinProm1mPips = 2  // 1m swing prominence gate
	mtfMin1mBars     = 20 // need at least this many 1m bars in the wall window to execute

	mtfTouchEpsPips         = 3   // how close a bar must come to "touch" the wall
	mtfBreakoutMarginPips   = 1   // close must clear the wall by this to "break"
	mtfDExitPips            = 4.5 // D_exit hysteresis (1.5×eps): a touch ends only once price pulls this far past the wall
	mtfBreakoutWithin1mBars = 15  // the 1m break must follow a touch within this many 1m bars
	mtfWallSlopeTolPips     = 0.5 // a wall sloping WITH the trend by more than this (pips/bar) is not a 逆トレンド wall

	// Structural exit, expressed as entry-time pip distances so
	// they flow through the existing Signal → broker-OCO / position-snapshot
	// path. 未決パラメータ — defaults pending a walk-forward sweep.
	mtfSLBufferPips = 2  // SL sits this far beyond the wall (建値からラインの逆側+バッファ)
	mtfSLMinPips    = 3  // floor so a fresh-break stop isn't absurdly tight
	mtfSLMaxPips    = 12 // 安全弁 SL_max: clip when the wall is far
	mtfTP1CapPips   = 20 // TP1 = min(1m line, ~20pips) — the ~20pip leg
	mtfTP1FloorPips = 5  // floor so TP1 isn't degenerate
)

func (MTFPullback) Evaluate(in EvalInput) Signal {
	name := config.StrategyMTFPullback
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

	// --- 1h bias H (environment recognition) ---
	if len(in.Candles1h) < mtfMin1hBars {
		return none("insufficient_1h_candles")
	}
	c1h := lastN(in.Candles1h, mtfLookback1h)
	H := combinedDir(
		regressionDir(c1h, mtfMinDrift1hPips, pip),
		swingStructureDir(
			ta.SwingHighs(c1h, mtfN1h, pip, mtfMinProm1hPips),
			ta.SwingLows(c1h, mtfN1h, pip, mtfMinProm1hPips),
		),
	)
	if H == trendFlat {
		return none("no_1h_trend")
	}

	// --- 5m leg L: must be a pullback (not aligned with the 1h trend) ---
	if len(in.Candles5m) < mtfMin5mBars {
		return none("insufficient_5m_candles")
	}
	c5 := lastN(in.Candles5m, mtfLookback5m)
	L := combinedDir(
		regressionDir(c5, mtfMinDrift5mPips, pip),
		swingStructureDir(
			ta.SwingHighs(c5, mtfN5m, pip, mtfMinProm5mPips),
			ta.SwingLows(c5, mtfN5m, pip, mtfMinProm5mPips),
		),
	)
	if !isPullback(H, L) {
		// Need a genuinely OPPOSED 5m leg (the pullback against the 1h trend).
		// "not aligned" (L flat) is not enough — that is chop/continuation, the
		// 別物 case the method warns about.
		return none("no_pullback")
	}

	// --- 5m wall (location only) ---
	wall, ok := fitWall(c5, H, mtfN5m, mtfMinProm5mPips, mtfMaxResid5mPips, pip)
	if !ok {
		return none("no_valid_wall")
	}

	// --- execution on 1m: 1回目スルー・2回目にブレイク ---
	// The 5m wall supplies only the location; the touch/break trigger is watched
	// on 1m bars (finer timing), projecting the wall onto each 1m bar's time.
	origin := c5[0].OpenTime
	c1 := candles1mSince(in.Candles1m, origin)
	if len(c1) < mtfMin1mBars {
		return none("insufficient_1m_candles")
	}
	wallPrices := wallPriceSeries(c1, wall, origin, mtf5mBarMinutes)
	fire, _ := scanWallBreakout(c1, wallPrices, H, mtfTouchEpsPips, mtfBreakoutMarginPips, mtfDExitPips, mtfBreakoutWithin1mBars, pip)
	if !fire {
		return none("no_breakout")
	}

	// --- enter in the 1h-trend direction (continuation after pullback) ---
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

	// --- structural exit: TP/SL derived from the lines at entry,
	// expressed as pip distances so they ride the existing Signal → broker-OCO /
	// position-snapshot path with no exit-engine change. ratchet is dropped
	// (公開されている裁量手法に無い; 既定では使わず構造的TPを主とする).
	wallNow := wallPrices[len(wallPrices)-1]
	slPips := structuralSLPips(entry, wallNow, side, mtfSLBufferPips, mtfSLMinPips, mtfSLMaxPips, pip)
	ch1m, chOK := ta.ParallelChannel(c1, mtfN1m, pip, mtfMinProm1mPips)
	tpPips := structuralTP1Pips(entry, side, ch1m, chOK, float64(len(c1)-1), mtfTP1CapPips, mtfTP1FloorPips, pip)

	return Signal{
		Decision:                         DecisionEnter,
		Side:                             side,
		EntryPrice:                       entry,
		TakeProfitPips:                   tpPips,
		StopLossPips:                     slPips,
		MaxHoldMinutes:                   in.Config.Exit.MaxHoldMinutes,
		ExtensionMaxMinutes:              in.Config.Exit.ExtensionMaxMinutes,
		ExtensionUnrealizedPipsThreshold: in.Config.Exit.ExtensionUnrealizedPipsThreshold,
		EarlyExitWindowMinutes:           in.Config.Exit.EarlyExitWindowMinutes,
		EarlyExitTargetPips:              in.Config.Exit.EarlyExitTargetPips,
		RatchetArmPips:                   0, // ratchet dropped — structural TP is primary
		RatchetGivebackPips:              0,
		Quantity:                         in.Config.Risk.Quantity,
		Reason:                           fmt.Sprintf("1h=%s pullback 1m-breakout TP=%.0f SL=%.0f", H, tpPips, slPips),
		ConfigID:                         in.Config.ConfigID,
		StrategyName:                     name,
		CreatedAt:                        in.Now,
	}
}

// lastN returns the last n elements of s (or all of s when shorter).
func lastN(s []market.Candle, n int) []market.Candle {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// --- direction helpers (environment recognition) -------------------------
//
// Both the 1h bias (H) and the 5m leg (L) are classified by AGREEMENT of two
// independent views: the sign of the regression
// slope over closes, and the swing structure (higher-highs+higher-lows = up).
// Requiring both to agree avoids calling a noisy drift a "trend".

// regressionDir returns "up"/"down"/"flat" from the OLS slope of closes. The
// total drift across the window (slope × span) must exceed minDriftPips in
// magnitude, else "flat". This rejects a near-flat regression as a trend.
func regressionDir(candles []market.Candle, minDriftPips, pipSize float64) trendDir {
	if len(candles) < 2 || pipSize <= 0 {
		return trendFlat
	}
	pts := make([]ta.Point, len(candles))
	for i, c := range candles {
		pts[i] = ta.Point{X: float64(i), Y: c.Close}
	}
	line, ok := ta.FitLine(pts)
	if !ok {
		return trendFlat
	}
	driftPips := line.Slope * float64(len(candles)-1) / pipSize
	if driftPips > minDriftPips {
		return trendUp
	}
	if driftPips < -minDriftPips {
		return trendDown
	}
	return trendFlat
}

// swingStructureDir classifies trend from the last two swing highs and lows:
// higher-high AND higher-low = up; lower-high AND lower-low = down; anything
// mixed (or fewer than two of either) = flat.
func swingStructureDir(highs, lows []ta.Swing) trendDir {
	if len(highs) < 2 || len(lows) < 2 {
		return trendFlat
	}
	lastH, prevH := highs[len(highs)-1].Price, highs[len(highs)-2].Price
	lastL, prevL := lows[len(lows)-1].Price, lows[len(lows)-2].Price
	if lastH > prevH && lastL > prevL {
		return trendUp
	}
	if lastH < prevH && lastL < prevL {
		return trendDown
	}
	return trendFlat
}

// combinedDir returns the shared direction only when both views agree and
// neither is flat; otherwise flat.
func combinedDir(regDir, structDir trendDir) trendDir {
	if regDir == structDir && (regDir == trendUp || regDir == trendDown) {
		return regDir
	}
	return trendFlat
}

// isPullback reports whether the 5m leg L is the genuine OPPOSITE of the 1h
// bias H (the only configurations the method enters: 1h up × 5m down, 1h down ×
// 5m up). L==flat is NOT a pullback — that is chop/continuation.
func isPullback(h, l trendDir) bool {
	return (h == trendUp && l == trendDown) || (h == trendDown && l == trendUp)
}

// --- the 5m "wall" (壁) + execution trigger ------------------------------
//
// In an uptrend the pullback is a short down-leg whose lower swing HIGHS form a
// descending resistance line; in a downtrend the bounce's higher swing LOWS
// form an ascending support line. fitWall fits that line (a TouchedLine, so it
// needs >=3 touches and must actually be straight). The line lives in 5m-index
// space; wallBreakout scans the SAME slice against it.

// fitWall returns the 5m trendline price has repeatedly respected. For trend
// "up" it fits the swing highs (descending resistance), for "down" the swing
// lows (ascending support). ok=false unless >=3 touches lie on a clean line
// AND the wall is genuinely COUNTER-trend: a wall sloping with the trend (an
// ascending "resistance" in an uptrend / descending "support" in a downtrend)
// is continuation, not the 逆トレンド壁 the method trades, so it is rejected.
func fitWall(candles []market.Candle, trend trendDir, n int, minPromPips, maxResidPips, pipSize float64) (ta.Line, bool) {
	var line ta.Line
	var ok bool
	switch trend {
	case trendUp:
		line, ok = ta.TouchedLine(ta.SwingHighs(candles, n, pipSize, minPromPips), 3, maxResidPips, pipSize)
	case trendDown:
		line, ok = ta.TouchedLine(ta.SwingLows(candles, n, pipSize, minPromPips), 3, maxResidPips, pipSize)
	default:
		return ta.Line{}, false
	}
	if !ok {
		return ta.Line{}, false
	}
	tol := mtfWallSlopeTolPips * pipSize // per-bar slope tolerance (allows a near-flat wall)
	if trend == trendUp && line.Slope > tol {
		return ta.Line{}, false // resistance must descend (or be ~flat), not ascend
	}
	if trend == trendDown && line.Slope < -tol {
		return ta.Line{}, false // support must ascend (or be ~flat), not descend
	}
	return line, true
}

// candles1mSince returns the candles at or after originTime — the 1m bars that
// fall within the 5m wall's domain, so the projection never reaches behind the
// window the wall was fit over. Assumes ascending OpenTime (as the aggregator
// emits).
func candles1mSince(candles []market.Candle, originTime time.Time) []market.Candle {
	for i, c := range candles {
		if !c.OpenTime.Before(originTime) {
			return candles[i:]
		}
	}
	return nil
}

// wallPriceSeries projects the 5m-fitted wall onto each candle's own time. The
// wall lives in 5m-bar-index space (index 0 = the 5m bar at originTime, one
// index per barMinutes). For a candle opening at time t the wall's x-coordinate
// is (t-originTime)/barMinutes — fractional, so 1m bars falling between or after
// the 5m bars are the method's "extrapolate to now" (Line.At).
func wallPriceSeries(candles []market.Candle, wall ta.Line, originTime time.Time, barMinutes float64) []float64 {
	out := make([]float64, len(candles))
	for i, c := range candles {
		x := c.OpenTime.Sub(originTime).Minutes() / barMinutes
		out[i] = wall.At(x)
	}
	return out
}

// scanWallBreakout decides the "1回目スルー・2回目にブレイクで執行" trigger by
// replaying the touch/break state machine over `candles` against the per-bar
// projected wall price `wallPrices` (len must match). The method implies this state
// lives in an engine-side per-(symbol,line) store; we instead recompute it from the
// supplied bars each tick, keeping Evaluate pure. It runs on 1m bars (finer
// execution timing) — the 5m wall only supplies the location.
//
// Whether the wall HELD or BROKE on a bar is decided by the bar's CLOSE, not its
// wick: a high that pierces a resistance but closes back below is a rejection
// (false-break), not a break.
//
// dExitPips is the D_exit hysteresis ("一度 ε を D_exit 以上離れて初めて離脱"):
// a touch episode only completes once the bar's approaching extreme has pulled
// at least D_exit beyond the wall. A shallow wobble that stays within D_exit is
// the SAME touch, so it is not miscounted as a fresh rejection.
//
// fire=true requires all three: (1) >=1 completed prior rejection episode (the
// 1st touch, skipped); (2) the latest bar freshly closed THROUGH the wall (the
// previous bar had not); and (3) that break followed a touch within
// maxBarsSinceTouch bars — a genuine 2nd-touch break, not a gap-through after
// price had drifted far from the wall.
func scanWallBreakout(candles []market.Candle, wallPrices []float64, trend trendDir, epsPips, marginPips, dExitPips float64, maxBarsSinceTouch int, pipSize float64) (bool, int) {
	n := len(candles)
	if n < 2 || len(wallPrices) != n {
		return false, 0
	}
	up := trend == trendUp
	eps := epsPips * pipSize
	margin := marginPips * pipSize
	dExit := dExitPips * pipSize

	episodes := 0
	state := "away"
	lastTouchIdx := -1
	for i := 0; i < n; i++ {
		w := wallPrices[i]
		var reached, brokeByClose, departedFar bool
		if up {
			reached = candles[i].High >= w-eps         // extreme came up to (or through) the wall
			brokeByClose = candles[i].Close > w+margin // genuine break on the close
			departedFar = candles[i].High <= w-dExit   // pulled clearly away below resistance
		} else {
			reached = candles[i].Low <= w+eps
			brokeByClose = candles[i].Close < w-margin
			departedFar = candles[i].Low >= w+dExit
		}
		held := reached && !brokeByClose // tested the wall but it held (incl wick-poke rejection)
		if held {
			if state == "away" {
				state = "touching"
			}
			lastTouchIdx = i
			continue
		}
		if brokeByClose {
			state = "away" // broke through — not a rejection
			continue
		}
		// Moved away without breaking. Only a move beyond D_exit ends the episode
		// (hysteresis); a shallow wobble stays the same pending touch.
		if state == "touching" && departedFar {
			episodes++
			state = "away"
		}
	}

	last := n - 1
	var brokeNow, brokePrev bool
	if up {
		brokeNow = candles[last].Close > wallPrices[last]+margin
		brokePrev = candles[last-1].Close > wallPrices[last-1]+margin
	} else {
		brokeNow = candles[last].Close < wallPrices[last]-margin
		brokePrev = candles[last-1].Close < wallPrices[last-1]-margin
	}
	fresh := brokeNow && !brokePrev
	recentTouch := lastTouchIdx >= 0 && last-lastTouchIdx <= maxBarsSinceTouch
	return episodes >= 1 && fresh && recentTouch, episodes
}

// structuralSLPips returns the stop distance derived from the wall ("SL=5mライン
// を逆側にブレイクしたら損切り"): the gap from entry back to the wall on the
// protective side plus a buffer, clamped to [minPips, maxPips]. maxPips is the
// safety clip for when the wall is far; minPips keeps a fresh-break
// stop tradeable.
func structuralSLPips(entry, wallNow float64, side order.Side, bufferPips, minPips, maxPips, pipSize float64) float64 {
	var distPips float64
	if side == order.SideBuy {
		distPips = (entry-wallNow)/pipSize + bufferPips // wall sits below a long
	} else {
		distPips = (wallNow-entry)/pipSize + bufferPips // wall sits above a short
	}
	return clampF(distPips, minPips, maxPips)
}

// structuralTP1Pips returns TP1 = min(distance to the forward 1m channel line,
// capPips), floored at floorPips. The forward line is the channel side in the
// trade direction (Upper for a long, Lower for a short), projected to projIndex
// (the latest 1m bar). When no valid channel exists or the line sits behind
// price, it falls back to capPips (the method's "~20pips" leg of the min).
func structuralTP1Pips(entry float64, side order.Side, ch ta.Channel, chOK bool, projIndex, capPips, floorPips, pipSize float64) float64 {
	if !chOK {
		return capPips
	}
	var distPips float64
	if side == order.SideBuy {
		distPips = (ch.Upper.At(projIndex) - entry) / pipSize
	} else {
		distPips = (entry - ch.Lower.At(projIndex)) / pipSize
	}
	if distPips <= 0 {
		return capPips // line is behind price — use the ~20pip leg
	}
	if distPips > capPips {
		distPips = capPips
	}
	return clampF(distPips, floorPips, capPips)
}

// clampF bounds v to [lo, hi].
func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
