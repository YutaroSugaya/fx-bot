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

// MAPullback is the 200-MA push/pull strategy distilled from a publicly
// described discretionary MA-pullback method, mechanised with the ta.SMA /
// ta.EMA primitives.
//
// MULTI-TIMEFRAME: the TREND (direction) is read off the 1h 200SMA — the big
// flow the reference method reads on a higher timeframe and names as an entry level — while the
// EXECUTION (pullback target, swings, structural stop) stays on the 5m 200SMA/EMA.
// Price rides the 1h trend, dips back to the 5m 200MA, and bounces; the entry is
// timed on 5m. (Reading the trend off the SAME 5m MA — a ≈16.7h slope — whipsaws
// and loses big-trend alignment; the 1h gives the runner exit a real trend behind it.)
//
//   - Trend H (maTrendDir): the 1h 200SMA SLOPE decides direction — rising ⇒ up,
//     falling ⇒ down, near-flat ⇒ no_trade ("大局は上位足の流れ"). The 5m 200EMA is NOT
//     used for direction; it serves as the other edge of the 5m pullback zone and a
//     defended line for the stop.
//   - Location (inMAZone): price must have pulled back to within k×ATR of EITHER
//     200MA ("待ち" — if it hasn't returned to the MA we don't trade).
//   - Side (priceOnTrendSide): price must hold the trend side of the SMA
//     (above for a long / below for a short) — the reference method reads "the big flow AND
//     price above/below the MA"; this enforces the "price > MA" half.
//   - 厳選 (hasConfluence): that zone must coincide with a recent swing high/low
//     OR a day level — 直近高安・節目, plus その日の高値/安値/起点 (confluenceLevels).
//   - Trigger (reboundConfirmed): the last 5m bar closes back in the trend
//     direction (陽線 up / 陰線 down) — the single mechanical 反発確認 rule.
//   - Exit (daytrade-runner profile; mechanised via the existing Signal path, no
//     exit-engine change): a TRAILING RATCHET is the primary exit — it arms at
//     +maPBRatchetArmPips and closes on a giveback from the peak, letting a trending
//     winner RUN while locking gains. SL defends the 200MA the pullback leans on
//     ("重要ライン抜けで損切り"; a fuller 全戻し(V字) stateful stop is not implemented). TP
//     is a FAR backstop ceiling (the next swing CLUSTER far edge via clusterTPPips,
//     floored ABOVE the ratchet arm so the broker OCO TP never preempts the trail). A
//     daytrade time stop (maPBMaxHoldMinutes) recycles a stalled position before it
//     becomes a dead naked carry. (A scalp-shaped exit — ~20-pip cap, no trailing,
//     no time stop — would structurally cap winners and make the strategy "enter like
//     a trend trade, exit like a scalp".)
type MAPullback struct {
	// P は walk-forward sweep が注入するチューニング
	// パラメータ。**zero-value は DefaultMAPullbackParams() と完全等価**
	// (registry が MAPullback{} を登録しているため。pin: ma_pullback_params_test)。
	// live は常に zero-value = 固定デフォルト値 — 非デフォルト値の注入はオフライン
	// (backtest / sweep) 専用。retune の live 投入は必ず新 config_id で行う。
	P MAPullbackParams
}

func (MAPullback) Name() config.StrategyName { return config.StrategyMAPullback }

// MAPullbackParams は ma_pullback のスイープ対象パラメータ
// (slope閾値 / zone幅 / confluence tol / SL・TP 形状 / arm / give / MaxHold)。
// MA 周期 (200) / ATR window (14) / swing fractal 幅 / day window は戦略の
// アイデンティティとして定数のまま固定 (スイープ面を広げすぎると多重比較で
// 暗記が混入する)。
type MAPullbackParams struct {
	TrendSlopeLookback1h int     // maPB1hSlopeLB:      1h 200SMA 傾き測定の遡り本数
	TrendMinSlopePips    float64 // maPB1hMinSlopePips: トレンド認定の最小傾き (pips/lookback)
	ZoneATR              float64 // maPBZoneATR:        pullback ゾーン幅 (×ATR)
	ConfluenceATR        float64 // maPBConfluenceATR:  節目重なり許容 (×ATR)
	SwingLookback        int     // maPBSwingLookback:  swing/confluence を読む 5m 本数
	SwingPromPips        float64 // maPBSwingPromPips:  swing prominence ノイズゲート
	SLBufferPips         float64 // maPBSLBufferPips
	SLMinPips            float64 // maPBSLMinPips
	SLMaxPips            float64 // maPBSLMaxPips
	TPCapPips            float64 // maPBTPCapPips
	TPFloorPips          float64 // maPBTPFloorPips
	ClusterBandATR       float64 // maPBClusterBandATR
	RatchetArmPips       float64 // maPBRatchetArmPips
	RatchetGivebackPips  float64 // maPBRatchetGivebackPips
	MaxHoldMinutes       int     // maPBMaxHoldMinutes
}

// DefaultMAPullbackParams は固定デフォルト値 (= 下の定数群)。live の実効値。
func DefaultMAPullbackParams() MAPullbackParams {
	return MAPullbackParams{
		TrendSlopeLookback1h: maPB1hSlopeLB,
		TrendMinSlopePips:    maPB1hMinSlopePips,
		ZoneATR:              maPBZoneATR,
		ConfluenceATR:        maPBConfluenceATR,
		SwingLookback:        maPBSwingLookback,
		SwingPromPips:        maPBSwingPromPips,
		SLBufferPips:         maPBSLBufferPips,
		SLMinPips:            maPBSLMinPips,
		SLMaxPips:            maPBSLMaxPips,
		TPCapPips:            maPBTPCapPips,
		TPFloorPips:          maPBTPFloorPips,
		ClusterBandATR:       maPBClusterBandATR,
		RatchetArmPips:       maPBRatchetArmPips,
		RatchetGivebackPips:  maPBRatchetGivebackPips,
		MaxHoldMinutes:       maPBMaxHoldMinutes,
	}
}

// effective は zero-value (= live の registry 登録形) をデフォルトへ正規化する。
func (p MAPullbackParams) effective() MAPullbackParams {
	if p == (MAPullbackParams{}) {
		return DefaultMAPullbackParams()
	}
	return p
}

// min1hBars は 1h 200SMA の傾き計算に必要な 1h 本数 (period + lookback)。
func (p MAPullbackParams) min1hBars() int { return maPB1hPeriod + p.TrendSlopeLookback1h }

// MAPullbackAuditExits returns the strategy-computed exit values that a frozen
// ma_pullback config's "honest audit row" must mirror (equivalence pin). The
// trailing ratchet (arm/give) and the daytrade time stop are fixed constants;
// the structural TP/SL are computed per-entry so they are NOT part of the fixed
// audit set. A test asserts the frozen yaml stays in sync
// with these constants so a constant change can't silently rot the audit rows.
func MAPullbackAuditExits() (armPips, givebackPips float64, maxHoldMinutes int) {
	return maPBRatchetArmPips, maPBRatchetGivebackPips, maPBMaxHoldMinutes
}

// Tunable defaults. Pip-denominated thresholds are
// scale-invariant via market.PipSize. Kept as constants; a walk-forward
// sweep (MAPullbackParams) explores alternatives offline.
const (
	// 1h 200SMA — the TREND source (MTF). the reference method reads the big flow on a higher
	// timeframe and times entries on the trading chart, so the trend is read off the
	// 1h 200SMA rather than the 5m 200MA. The 5m chart (below) stays the EXECUTION chart
	// (pullback target, swings, structural stop). 200 1h bars ≈ 8.3 days of context.
	maPB1hPeriod       = 200                          // 200-bar SMA on 1h
	maPB1hSlopeLB      = 20                           // 1h bars back to measure the 1h 200SMA slope (≈20h)
	maPB1hMinSlopePips = 5.0                          // the 1h 200SMA must drift ≥ this over slopeLB bars to be a trend
	maPB1hMinBars      = maPB1hPeriod + maPB1hSlopeLB // 220 1h bars to compute the slope

	// 200MA on the trading (5m) chart — the pullback TARGET (zone) + the structural
	// stop line. NOT the trend anymore (the 1h 200SMA above decides direction).
	maPB5mPeriod  = 200          // 200-bar SMA/EMA on 5m (≈16.7h)
	maPBMin5mBars = maPB5mPeriod // bars needed to fit the 5m 200MA

	maPBATRWindow     = 14  // recent 5m bars the ATR scale is measured over
	maPBZoneATR       = 0.8 // price within this×ATR of a 200MA = "in the MA zone"
	maPBConfluenceATR = 1.2 // the zone must also sit within this×ATR of a swing S/R
	maPBSwingLookback = 96  // recent 5m bars (≈8h) confluence / TP swings are read over
	maPBSwingN        = 2   // fractal half-width for the 5m swings
	maPBSwingPromPips = 3.0 // swing prominence gate (noise)
	maPBDayWindow     = 288 // last ~24h of 5m bars = "today's" high/low/origin

	// Structural exit, daytrade-runner profile (entry-time pip distances → existing
	// Signal path). The trailing ratchet (below) is the PRIMARY exit; the TP is a far
	// backstop ceiling and the SL defends the 200MA the pullback leans on.
	maPBSLBufferPips   = 2.0  // SL sits this far beyond the defended 200MA
	maPBSLMinPips      = 8.0  // floor: wide enough that 5m noise doesn't pre-stop a young leg
	maPBSLMaxPips      = 20.0 // 安全弁 clip when the MA is far
	maPBTPCapPips      = 80.0 // far runner ceiling: the OCO TP is a backstop, the ratchet runs the exit
	maPBTPFloorPips    = 30.0 // floor ABOVE the ratchet arm so the OCO TP never preempts the trail
	maPBClusterBandATR = 1.5  // swing levels within this×ATR of each other form a 密集地帯

	// Trailing ratchet — the PRIMARY runner exit (bot-side, evaluated every tick;
	// GMO OCO can't trail). Arms once unrealized profit reaches arm, then closes if
	// price gives back giveback from the peak. arm>giveback>0 so an armed trade always
	// locks a positive amount, and arm < maPBTPFloorPips so the trail engages before
	// the far OCO TP can fire.
	maPBRatchetArmPips      = 16.0 // arm the trail once +this in profit
	maPBRatchetGivebackPips = 8.0  // close after giving back this much from the peak
	// Daytrade time stop: recycle a stalled position before it becomes a dead naked
	// carry (~8h ≈ a day-trade horizon; long enough not to cut a trending winner).
	maPBMaxHoldMinutes = 480
)

func (m MAPullback) Evaluate(in EvalInput) Signal {
	name := config.StrategyMAPullback
	p := m.P.effective()
	none := func(reason string) Signal {
		return Signal{Decision: DecisionNone, StrategyName: name, Reason: reason, CreatedAt: in.Now}
	}
	if in.Config == nil || in.Summary == nil {
		return Signal{Decision: DecisionNone, StrategyName: name, CreatedAt: in.Now}
	}
	// 不正注入ガード: 負/0 の lookback は下の slice 演算で panic する。
	// live は常に defaults (>0) なので到達しないが、sweep 等の注入呼び手に対し
	// panic-safe に no-trade へ退避する。
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

	// --- 1h trend H (the big flow): direction is read off the 1h 200SMA slope — the
	// higher timeframe the reference method names ("大局は上位足"). 5m below stays execution. ---
	if len(in.Candles1h) < p.min1hBars() {
		return none("insufficient_1h_candles")
	}
	closes1h := closesOf(in.Candles1h)
	sma1h, ok1h := ta.SMA(closes1h, maPB1hPeriod)
	sma1hPrev, ok1hP := ta.SMA(closes1h[:len(closes1h)-p.TrendSlopeLookback1h], maPB1hPeriod)
	if !ok1h || !ok1hP {
		return none("insufficient_1h_candles")
	}
	H := maTrendDir(sma1h, sma1hPrev, p.TrendMinSlopePips, pip)
	if H == trendFlat {
		return none("no_trend")
	}

	// --- 5m 200MAs: the pullback TARGET (zone) + the structural stop line ---
	if len(in.Candles5m) < maPBMin5mBars {
		return none("insufficient_5m_candles")
	}
	closes := closesOf(in.Candles5m)
	sma, ok1 := ta.SMA(closes, maPB5mPeriod)
	ema, ok2 := ta.EMA(closes, maPB5mPeriod)
	if !ok1 || !ok2 {
		return none("insufficient_5m_candles")
	}

	// ATR sets the zone / confluence / TP-cluster widths. When it is 0 (a dead-
	// flat window) every width collapses to 0, so the zone and confluence gates
	// below decline to trade on their own — no separate guard needed.
	atrPips := atrPipsOf(in.Candles5m, maPBATRWindow, pip)
	zoneTol := p.ZoneATR * atrPips * pip
	if !inMAZone(price, sma, ema, zoneTol) {
		return none("not_in_ma_zone") // ★MAまで戻らなければ見送る (無理に追わない)
	}

	// --- price must hold the trend side of the 5m execution MA ---
	// Direction comes from the 1h trend (H above); this is the EXECUTION-quality half
	// of the trend-side check — the reference method buys only while price holds ABOVE the MA it pulled back to
	// (戻り売りは価格が MA の下)。A pullback that has slipped to the wrong side of the 5m
	// 200SMA is the MA breaking, not a clean push, so we decline even in a 1h uptrend.
	if !priceOnTrendSide(price, sma, H) {
		return none("wrong_side_of_ma")
	}

	// --- 重なり厳選: the zone must coincide with a recent swing S/R OR a day level
	// (直近高安・節目・その日の高値/安値/起点) ---
	cWin := lastN(in.Candles5m, p.SwingLookback)
	highs := ta.SwingHighs(cWin, maPBSwingN, pip, p.SwingPromPips)
	lows := ta.SwingLows(cWin, maPBSwingN, pip, p.SwingPromPips)
	confTol := p.ConfluenceATR * atrPips * pip
	if !hasConfluence(price, confluenceLevels(in.Candles5m, highs, lows), confTol) {
		return none("no_confluence")
	}

	// --- 反発確認: the last 5m bar closes back in the trend direction ---
	if !reboundConfirmed(in.Candles5m[len(in.Candles5m)-1], H) {
		return none("no_rebound")
	}

	// --- enter in the trend direction (continuation after the pullback) ---
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

	// --- structural exit (pip distances at entry → existing Signal path) ---
	// SL defends the 200MA the pullback leans on: the lower MA for a long, the
	// upper MA for a short (price must hold the MA — "重要ライン抜けで損切り").
	maForSL := math.Min(sma, ema)
	if side == order.SideSell {
		maForSL = math.Max(sma, ema)
	}
	slPips := structuralSLPips(entry, maForSL, side, p.SLBufferPips, p.SLMinPips, p.SLMaxPips, pip)
	clusterBand := p.ClusterBandATR * atrPips * pip
	tpPips := clusterTPPips(entry, side, highs, lows, clusterBand, p.TPCapPips, p.TPFloorPips, pip)

	return Signal{
		Decision:       DecisionEnter,
		Side:           side,
		EntryPrice:     entry,
		TakeProfitPips: tpPips,
		StopLossPips:   slPips,
		// Daytrade-runner exits (mechanised, bot-side; the SAME Signal path as the
		// other strategies — no exit-engine change). The trailing ratchet is the
		// PRIMARY exit: it arms at +maPBRatchetArmPips and closes on a giveback from
		// the peak (manage_open_positions_exits.go evaluateRatchetExit), letting a
		// trending winner run while locking gains — the far TP above is only a backstop
		// ceiling. A daytrade time stop recycles a stalled position before it becomes a
		// dead naked carry (evaluateMaxHoldExit); extension/early-exit stay OFF.
		// (All-0s here would mimic a no-management scalp, capping winners at ~20 pips.)
		MaxHoldMinutes:                   p.MaxHoldMinutes,
		ExtensionMaxMinutes:              0,
		ExtensionUnrealizedPipsThreshold: 0,
		EarlyExitWindowMinutes:           0,
		EarlyExitTargetPips:              0,
		RatchetArmPips:                   p.RatchetArmPips,
		RatchetGivebackPips:              p.RatchetGivebackPips,
		Quantity:                         in.Config.Risk.Quantity,
		Reason:                           fmt.Sprintf("%s 200MA pullback TP=%.0f SL=%.0f ratchet=%.0f/%.0f", H, tpPips, slPips, p.RatchetArmPips, p.RatchetGivebackPips),
		ConfigID:                         in.Config.ConfigID,
		StrategyName:                     name,
		CreatedAt:                        in.Now,
	}
}

// closesOf extracts the Close series from candles (order preserved).
func closesOf(candles []market.Candle) []float64 {
	out := make([]float64, len(candles))
	for i, c := range candles {
		out[i] = c.Close
	}
	return out
}

// atrPipsOf returns the mean True Range (in pips) over the last `window` candles,
// reusing market.BuildWindowSummary's ATR so the vol scale matches the rest of
// the codebase. 0 when there are no candles / no pip scale.
func atrPipsOf(candles []market.Candle, window int, pipSize float64) float64 {
	return market.BuildWindowSummary(lastN(candles, window), pipSize).ATRPips
}

// maTrendDir classifies the trend from the 200SMA SLOPE — "200MAが上向き＝上昇,
// 下向き＝下降" (the reference method reads the trend off the MA's direction). The 200SMA
// must drift ≥ minSlopePips over the lookback to count as a trend, else flat.
//
// Slope of the heavily-smoothed 200SMA is the robust signal; the 200EMA is NOT
// used for direction because a SAME-period EMA and SMA nearly coincide on a
// steady trend (their difference flips sign on transients), so EMA-vs-SMA is not
// a reliable trend discriminator. The 200EMA still earns its keep as the other
// edge of the pullback zone (inMAZone) and a defended line for the stop. "price
// vs MA" is intentionally NOT part of this — at the pullback entry price is AT
// the MA, so the trend is read from the MA geometry, not the spot price.
func maTrendDir(sma, smaPrev, minSlopePips, pipSize float64) trendDir {
	if pipSize <= 0 {
		return trendFlat
	}
	slopePips := (sma - smaPrev) / pipSize
	if slopePips > minSlopePips {
		return trendUp
	}
	if slopePips < -minSlopePips {
		return trendDown
	}
	return trendFlat
}

// inMAZone reports whether price has pulled back to within tolPrice of EITHER
// 200MA — the "push-to-MA" location.
func inMAZone(price, sma, ema, tolPrice float64) bool {
	return math.Abs(price-sma) <= tolPrice || math.Abs(price-ema) <= tolPrice
}

// priceOnTrendSide reports whether price sits on the trend side of the trend SMA:
// for an uptrend price must be at/above the SMA (押し目買い while price holds
// above the rising MA), for a downtrend at/below it (戻り売りは MA の下). Equality
// passes — at the pullback price is AT the MA. "flat" has no trend side.
func priceOnTrendSide(price, sma float64, trend trendDir) bool {
	switch trend {
	case trendUp:
		return price >= sma
	case trendDown:
		return price <= sma
	}
	return false
}

// hasConfluence reports whether any swing pivot sits within tolPrice of price —
// the method's 重要レート(節目)との重なり厳選.
func hasConfluence(price float64, swings []ta.Swing, tolPrice float64) bool {
	for _, s := range swings {
		if math.Abs(price-s.Price) <= tolPrice {
			return true
		}
	}
	return false
}

// dayLevels returns the recent ~24h reference prices the reference method watches as
// 意識されるレート: [day high, day low, day origin] — the high and low of the
// last `window` 5m bars and the open of the oldest bar in that window. These
// augment the swing-pivot confluence with the levels the market anchored to
// today. Rolling-window proxy for "その日"; a session-anchored day is a future
// refinement. nil when there are no candles.
func dayLevels(candles []market.Candle, window int) []float64 {
	win := lastN(candles, window)
	if len(win) == 0 {
		return nil
	}
	high, low := win[0].High, win[0].Low
	for _, c := range win {
		if c.High > high {
			high = c.High
		}
		if c.Low < low {
			low = c.Low
		}
	}
	return []float64{high, low, win[0].Open}
}

// confluenceLevels is the full set of 意識されるレート the entry厳選 checks against:
// the recent swing highs/lows PLUS the day high/low/origin, so Evaluate and
// Gates share one definition and cannot drift. Day levels carry only a Price
// (Kind/Index are irrelevant to hasConfluence).
func confluenceLevels(candles []market.Candle, highs, lows []ta.Swing) []ta.Swing {
	levels := append(append([]ta.Swing{}, highs...), lows...)
	for _, p := range dayLevels(candles, maPBDayWindow) {
		levels = append(levels, ta.Swing{Price: p})
	}
	return levels
}

// reboundConfirmed reports whether the last 5m bar confirms a turn back in the
// trend direction (陽線 in an uptrend / 陰線 in a downtrend) — the single
// mechanical "反発確認" rule replacing the discretionary read.
func reboundConfirmed(last market.Candle, trend trendDir) bool {
	switch trend {
	case trendUp:
		return last.Close > last.Open
	case trendDown:
		return last.Close < last.Open
	}
	return false
}

// clusterTPPips returns the take-profit distance (pips) toward the next 密集地帯
// (the reference method's "次の安値・高値の密集地帯まで"). Walking the levels ahead of entry
// nearest-first, it finds the FIRST level that has ≥1 other level within
// bandPrice (i.e. the nearest level that is part of a ≥2-level pack), then takes
// profit at that cluster's FAR edge — the member farthest from entry within the
// band. the reference method exits AFTER price breaks through the dense band and runs
// ("密集ラインをブレイクして走ったタイミングで決済"), so the far edge (past the
// cluster) is the faithful target, not the near edge (which would exit into the
// resistance before the break). For a long the levels are swing HIGHS above
// entry, for a short the swing LOWS below. Falls back to the nearest single level
// when no pack forms, then to capPips when no level lies ahead. Result clamped to
// [floorPips, capPips].
func clusterTPPips(entry float64, side order.Side, highs, lows []ta.Swing, bandPrice, capPips, floorPips, pipSize float64) float64 {
	// Levels ahead of entry, as plain prices.
	var levels []float64
	if side == order.SideBuy {
		for _, s := range highs {
			if s.Price > entry {
				levels = append(levels, s.Price)
			}
		}
	} else {
		for _, s := range lows {
			if s.Price < entry {
				levels = append(levels, s.Price)
			}
		}
	}
	if len(levels) == 0 {
		return capPips
	}
	// Sort by distance from entry (nearest first). For a long that is ascending
	// price; for a short, descending.
	sort.Slice(levels, func(i, j int) bool {
		if side == order.SideBuy {
			return levels[i] < levels[j]
		}
		return levels[i] > levels[j]
	})

	// Find the NEAREST cluster: walk levels nearest-first and pick the first one
	// that has ≥1 other level within bandPrice (a 密集地帯 of ≥2). Take profit at
	// that cluster's FAR edge — the member farthest from entry within the band —
	// so the target sits PAST the dense band (where price "走った" after breaking
	// through), per the reference method's break-and-run exit. If no cluster forms, use the
	// single nearest level. Since levels are sorted nearest-first, the farthest
	// member within band of the anchor is simply the last such level we scan.
	target := levels[0]
	for _, lv := range levels {
		var farEdge float64
		members := 0
		for _, other := range levels {
			if math.Abs(other-lv) <= bandPrice {
				members++
				farEdge = other // levels are nearest-first → last match = far edge
			}
		}
		if members >= 2 {
			target = farEdge
			break
		}
	}

	var distPips float64
	if side == order.SideBuy {
		distPips = (target - entry) / pipSize
	} else {
		distPips = (entry - target) / pipSize
	}
	return clampF(distPips, floorPips, capPips)
}
