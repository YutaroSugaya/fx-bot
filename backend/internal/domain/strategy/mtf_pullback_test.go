package strategy

import (
	"math"
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/ta"
)

// candlesFromCloses builds 5m candles whose Close walks the given values;
// High/Low hug the close (so only-close logic is isolated from wick logic).
func candlesFromCloses(closes []float64) []market.Candle {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]market.Candle, len(closes))
	for i, c := range closes {
		out[i] = market.Candle{
			Symbol:   "USD_JPY",
			Interval: 5 * time.Minute,
			OpenTime: t0.Add(time.Duration(i) * 5 * time.Minute),
			Open:     c,
			High:     c,
			Low:      c,
			Close:    c,
		}
	}
	return out
}

const mtfPip = 0.01

func TestRegressionDir(t *testing.T) {
	up := candlesFromCloses([]float64{150.00, 150.05, 150.10, 150.18, 150.25})
	if d := regressionDir(up, 5, mtfPip); d != "up" {
		t.Fatalf("ascending: expected up, got %q", d)
	}
	down := candlesFromCloses([]float64{150.25, 150.18, 150.10, 150.05, 150.00})
	if d := regressionDir(down, 5, mtfPip); d != "down" {
		t.Fatalf("descending: expected down, got %q", d)
	}
	flat := candlesFromCloses([]float64{150.10, 150.11, 150.10, 150.09, 150.10})
	if d := regressionDir(flat, 5, mtfPip); d != "flat" {
		t.Fatalf("flat: expected flat, got %q", d)
	}
}

func TestRegressionDir_DriftThreshold(t *testing.T) {
	// Total drift ≈ 4 pips over the window. Threshold 5 → flat; threshold 2 → up.
	c := candlesFromCloses([]float64{150.00, 150.01, 150.02, 150.03, 150.04})
	if d := regressionDir(c, 5, mtfPip); d != "flat" {
		t.Fatalf("drift 4 < thresh 5: expected flat, got %q", d)
	}
	if d := regressionDir(c, 2, mtfPip); d != "up" {
		t.Fatalf("drift 4 > thresh 2: expected up, got %q", d)
	}
}

func mkSwingSlice(prices []float64, kind ta.SwingKind) []ta.Swing {
	out := make([]ta.Swing, len(prices))
	for i, p := range prices {
		out[i] = ta.Swing{Index: i * 2, Price: p, Kind: kind}
	}
	return out
}

func TestSwingStructureDir(t *testing.T) {
	// higher highs + higher lows → up
	upH := mkSwingSlice([]float64{150.20, 150.35}, ta.SwingHigh)
	upL := mkSwingSlice([]float64{150.05, 150.15}, ta.SwingLow)
	if d := swingStructureDir(upH, upL); d != "up" {
		t.Fatalf("HH+HL: expected up, got %q", d)
	}
	// lower highs + lower lows → down
	dnH := mkSwingSlice([]float64{150.35, 150.20}, ta.SwingHigh)
	dnL := mkSwingSlice([]float64{150.15, 150.05}, ta.SwingLow)
	if d := swingStructureDir(dnH, dnL); d != "down" {
		t.Fatalf("LH+LL: expected down, got %q", d)
	}
	// mixed (higher high but lower low) → flat
	mixH := mkSwingSlice([]float64{150.20, 150.35}, ta.SwingHigh)
	mixL := mkSwingSlice([]float64{150.15, 150.05}, ta.SwingLow)
	if d := swingStructureDir(mixH, mixL); d != "flat" {
		t.Fatalf("HH+LL: expected flat, got %q", d)
	}
	// not enough swings → flat
	if d := swingStructureDir(mkSwingSlice([]float64{150.20}, ta.SwingHigh), upL); d != "flat" {
		t.Fatalf("1 high: expected flat, got %q", d)
	}
}

func TestCombinedDir(t *testing.T) {
	if d := combinedDir("up", "up"); d != "up" {
		t.Fatalf("agree up: got %q", d)
	}
	if d := combinedDir("down", "down"); d != "down" {
		t.Fatalf("agree down: got %q", d)
	}
	if d := combinedDir("up", "down"); d != "flat" {
		t.Fatalf("disagree: expected flat, got %q", d)
	}
	if d := combinedDir("up", "flat"); d != "flat" {
		t.Fatalf("one flat: expected flat, got %q", d)
	}
}

// barsHLC builds 5m candles from parallel high/low/close slices.
func barsHLC(high, low, close []float64) []market.Candle {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]market.Candle, len(high))
	for i := range high {
		out[i] = market.Candle{
			Symbol:   "USD_JPY",
			Interval: 5 * time.Minute,
			OpenTime: t0.Add(time.Duration(i) * 5 * time.Minute),
			Open:     close[i],
			High:     high[i],
			Low:      low[i],
			Close:    close[i],
		}
	}
	return out
}

// wallPx projects a wall by candle INDEX (the legacy semantics, equivalent to
// the old per-index scan): wallPrices[i] = wall.At(i). Used by the unit tests
// that supply bars in wall-index space directly.
func wallPx(wall ta.Line, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = wall.At(float64(i))
	}
	return out
}

func TestFitWall_DescendingResistanceForUptrend(t *testing.T) {
	// 3 lower swing highs (idx 1,3,5 = 150.30/.25/.20) form a descending wall.
	high := []float64{150.10, 150.30, 150.15, 150.25, 150.12, 150.20, 150.08}
	low := []float64{150.00, 150.20, 150.05, 150.15, 150.02, 150.10, 149.98}
	cl := []float64{150.05, 150.25, 150.10, 150.20, 150.07, 150.15, 150.03}
	wall, ok := fitWall(barsHLC(high, low, cl), "up", 1, 0, 5, mtfPip)
	if !ok {
		t.Fatal("expected a valid descending wall")
	}
	if wall.Slope >= 0 {
		t.Fatalf("descending wall: expected negative slope, got %v", wall.Slope)
	}
}

func TestFitWall_RequiresThreeTouches(t *testing.T) {
	// Only 2 swing highs → not yet a proven wall.
	high := []float64{150.10, 150.30, 150.15, 150.25, 150.12}
	low := []float64{150.00, 150.20, 150.05, 150.15, 150.02}
	cl := []float64{150.05, 150.25, 150.10, 150.20, 150.07}
	if _, ok := fitWall(barsHLC(high, low, cl), "up", 1, 0, 5, mtfPip); ok {
		t.Fatal("2 touches: expected ok=false")
	}
}

func TestWallBreakout_Up_SecondTouchBreaksFires(t *testing.T) {
	// Flat wall at 150.20. Touch→reject (1st), touch→break (2nd) → fire.
	wall := ta.Line{Slope: 0, Intercept: 150.20}
	high := []float64{150.10, 150.11, 150.12, 150.20, 150.12, 150.11, 150.19, 150.30}
	cl := []float64{150.09, 150.10, 150.11, 150.18, 150.10, 150.10, 150.18, 150.25}
	low := make([]float64, len(high))
	for i := range high {
		low[i] = cl[i] - 0.05
	}
	fire, episodes := scanWallBreakout(barsHLC(high, low, cl), wallPx(wall, len(cl)), "up", 3, 1, mtfDExitPips, 3, mtfPip)
	if !fire {
		t.Fatalf("expected fire=true (1 prior rejection + fresh breakout), episodes=%d", episodes)
	}
	if episodes != 1 {
		t.Fatalf("expected 1 prior rejection episode, got %d", episodes)
	}
}

func TestWallBreakout_Up_FirstTouchBreakDoesNotFire(t *testing.T) {
	// Approach once then break immediately — no prior rejection → skip (1st touch).
	wall := ta.Line{Slope: 0, Intercept: 150.20}
	high := []float64{150.10, 150.11, 150.20, 150.30}
	cl := []float64{150.09, 150.10, 150.18, 150.25}
	low := []float64{150.05, 150.06, 150.13, 150.20}
	fire, episodes := scanWallBreakout(barsHLC(high, low, cl), wallPx(wall, len(cl)), "up", 3, 1, mtfDExitPips, 3, mtfPip)
	if fire {
		t.Fatal("1st-touch breakout must not fire")
	}
	if episodes != 0 {
		t.Fatalf("expected 0 prior rejections, got %d", episodes)
	}
}

func TestWallBreakout_Up_TouchWithoutBreakDoesNotFire(t *testing.T) {
	// Two touches but price never closes through the wall → no entry.
	wall := ta.Line{Slope: 0, Intercept: 150.20}
	high := []float64{150.20, 150.12, 150.19, 150.19}
	cl := []float64{150.18, 150.10, 150.17, 150.17}
	low := []float64{150.13, 150.05, 150.12, 150.12}
	fire, _ := scanWallBreakout(barsHLC(high, low, cl), wallPx(wall, len(cl)), "up", 3, 1, mtfDExitPips, 3, mtfPip)
	if fire {
		t.Fatal("no close beyond wall: must not fire")
	}
}

func TestWallBreakout_Down_SecondTouchBreaksFires(t *testing.T) {
	// Flat support at 150.00. Reject up (1st), then break down (2nd) → fire SELL.
	wall := ta.Line{Slope: 0, Intercept: 150.00}
	low := []float64{150.10, 150.09, 150.00, 150.08, 150.09, 150.01, 149.90}
	cl := []float64{150.12, 150.11, 150.02, 150.10, 150.11, 150.02, 149.92}
	high := make([]float64, len(low))
	for i := range low {
		high[i] = cl[i] + 0.05
	}
	fire, episodes := scanWallBreakout(barsHLC(high, low, cl), wallPx(wall, len(cl)), "down", 3, 1, mtfDExitPips, 3, mtfPip)
	if !fire {
		t.Fatalf("expected fire=true on downward break, episodes=%d", episodes)
	}
	if episodes != 1 {
		t.Fatalf("expected 1 prior rejection, got %d", episodes)
	}
}

func TestIsPullback(t *testing.T) {
	// A pullback requires the 5m leg to be the EXPLICIT opposite of the 1h bias
	// — "not aligned" (flat) is NOT a pullback (only the two opposed
	// cases enter).
	cases := []struct {
		h, l trendDir
		want bool
	}{
		{"up", "down", true},
		{"down", "up", true},
		{"up", "flat", false},
		{"down", "flat", false},
		{"up", "up", false},
		{"down", "down", false},
		{"flat", "down", false},
	}
	for _, c := range cases {
		if got := isPullback(c.h, c.l); got != c.want {
			t.Errorf("isPullback(%q,%q)=%v want %v", c.h, c.l, got, c.want)
		}
	}
}

func TestFitWall_RejectsAscendingWallForUptrend(t *testing.T) {
	// 3 ASCENDING swing highs (price already making higher highs = continuation)
	// is NOT a counter-trend resistance wall → reject for an uptrend.
	high := []float64{150.10, 150.12, 150.20, 150.14, 150.16, 150.30, 150.18, 150.20, 150.40, 150.15}
	low := make([]float64, len(high))
	cl := make([]float64, len(high))
	for i := range high {
		low[i] = high[i] - 0.03
		cl[i] = high[i] - 0.01
	}
	if _, ok := fitWall(barsHLC(high, low, cl), "up", 1, 0, 5, mtfPip); ok {
		t.Fatal("ascending swing highs must not be accepted as an uptrend wall")
	}
}

func TestWallBreakout_Up_WickPokeRejectionCounts(t *testing.T) {
	// Highs pierce ABOVE the band but closes pull back below the wall = textbook
	// false-break rejections. They must COUNT as rejection episodes (old code
	// miscounted them as "broke" and dropped them → under-fire).
	wall := ta.Line{Slope: 0, Intercept: 150.20}
	high := []float64{150.10, 150.28, 150.12, 150.27, 150.35}
	cl := []float64{150.09, 150.10, 150.10, 150.10, 150.25}
	low := make([]float64, len(high))
	for i := range high {
		low[i] = cl[i] - 0.05
	}
	fire, episodes := scanWallBreakout(barsHLC(high, low, cl), wallPx(wall, len(cl)), "up", 3, 1, mtfDExitPips, 3, mtfPip)
	if !fire {
		t.Fatalf("wick-poke rejections then break should fire, got fire=false episodes=%d", episodes)
	}
	if episodes < 1 {
		t.Fatalf("wick-poke rejection must count, got episodes=%d", episodes)
	}
}

func TestWallBreakout_Up_GapThroughWithoutRecentTouchDoesNotFire(t *testing.T) {
	// One early reject, then price drifts far away, then a gap-up closes through
	// the wall with NO fresh re-approach → not the spec's "2回目にブレイク".
	wall := ta.Line{Slope: 0, Intercept: 150.20}
	high := []float64{150.10, 150.20, 150.10, 149.90, 149.85, 149.88, 150.35}
	cl := []float64{150.09, 150.18, 150.09, 149.88, 149.83, 149.86, 150.25}
	low := make([]float64, len(high))
	for i := range high {
		low[i] = cl[i] - 0.05
	}
	fire, _ := scanWallBreakout(barsHLC(high, low, cl), wallPx(wall, len(cl)), "up", 3, 1, mtfDExitPips, 3, mtfPip)
	if fire {
		t.Fatal("gap-through with no recent touch must not fire")
	}
}

// --- Evaluate integration -------------------------------------------------

// upZigzag is a strong 1h uptrend with clean swing structure (ascending swing
// highs AND lows), so the 1h bias resolves to "up".
func upZigzag(bars int) []market.Candle {
	saw := []float64{0, 3, 5, 2} // period-4: peak at pos2, trough at pos0
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]market.Candle, bars)
	for i := 0; i < bars; i++ {
		c := 150.00 + 0.05*float64(i) + 0.10*saw[i%4]
		out[i] = market.Candle{
			Symbol: "USD_JPY", Interval: time.Hour,
			OpenTime: t0.Add(time.Duration(i) * time.Hour),
			Open:     c, High: c + 0.02, Low: c - 0.02, Close: c,
		}
	}
	return out
}

// pullbackWith5mBreakout is a 5m down-leg (the pullback inside the 1h uptrend):
// three descending swing highs (idx 2,5,8) form a descending resistance wall
// that price touches and is rejected from, then the last bar closes through it.
func pullbackWith5mBreakout() []market.Candle {
	high := []float64{150.30, 150.32, 150.40, 150.26, 150.28, 150.34, 150.22, 150.24, 150.28, 150.20, 150.23, 150.22, 150.45}
	cl := []float64{150.28, 150.30, 150.38, 150.24, 150.26, 150.32, 150.20, 150.22, 150.26, 150.18, 150.19, 150.20, 150.35}
	low := make([]float64, len(high))
	for i := range cl {
		low[i] = cl[i] - 0.03
	}
	return barsHLC(high, low, cl)
}

// gen1mBreakout scripts the 1m execution leg against the (5m-fitted) wall
// projected onto each 1m bar's time: away → touch → far-depart (rejection
// episode 1) → away → touch → break on the LAST bar. With trend "up" the wall
// is resistance and the break closes above it. Bars are placed late in the wall
// window (so the projection is the spec's "extrapolate to now").
func gen1mBreakout(wall ta.Line, origin time.Time, trend string, pip float64) []market.Candle {
	modes := []string{
		"away", "away", "away", "away", "away", "away", "away", "away", "away", "away", // 0-9
		"touch",                    // 10
		"far", "far", "far", "far", // 11-14 → completes rejection episode 1
		"away", "away", "away", "away", // 15-18
		"touch", "touch", "touch", "touch", // 19-22 (2nd touch run)
		"break", // 23 (fresh close-through)
	}
	const startMin = 50
	up := trend == "up"
	out := make([]market.Candle, len(modes))
	for k, m := range modes {
		t := origin.Add(time.Duration(startMin+k) * time.Minute)
		x := float64(startMin+k) / mtf5mBarMinutes
		w := wall.At(x)
		var hi, lo, cl float64
		switch m {
		case "touch": // reach the wall but close back on the holding side
			if up {
				cl, hi, lo = w-3*pip, w-0.5*pip, w-4*pip
			} else {
				cl, lo, hi = w+3*pip, w+0.5*pip, w+4*pip
			}
		case "far": // pull beyond D_exit → completes the rejection episode
			if up {
				cl, hi, lo = w-12*pip, w-11*pip, w-13*pip
			} else {
				cl, lo, hi = w+12*pip, w+11*pip, w+13*pip
			}
		case "break": // close through the wall by > margin
			if up {
				cl, hi, lo = w+5*pip, w+6*pip, w-1*pip
			} else {
				cl, lo, hi = w-5*pip, w-6*pip, w+1*pip
			}
		default: // away (clearly off the wall, never reaching it)
			if up {
				cl, hi, lo = w-10*pip, w-9*pip, w-11*pip
			} else {
				cl, lo, hi = w+10*pip, w+9*pip, w+11*pip
			}
		}
		out[k] = market.Candle{
			Symbol: "USD_JPY", Interval: time.Minute,
			OpenTime: t, Open: cl, High: hi, Low: lo, Close: cl,
		}
	}
	return out
}

func mtfBuyInputs(now time.Time) EvalInput {
	cfg := baseConfig(config.StrategyMTFPullback, config.DirectionBoth, now)
	c5 := pullbackWith5mBreakout()
	// Fit the same wall Evaluate will (same slice + params) so the 1m fixture is
	// scripted against the real projected line.
	wall, _ := fitWall(c5, "up", mtfN5m, mtfMinProm5mPips, mtfMaxResid5mPips, mtfPip)
	return EvalInput{
		Now:       now,
		Summary:   mkSummary(150.34, 150.35, 150.50, 150.05, "up"),
		Candles1h: upZigzag(18),
		Candles5m: c5,
		Candles1m: gen1mBreakout(wall, c5[0].OpenTime, "up", mtfPip),
		Config:    cfg,
	}
}

func TestEngine_DispatchMTFPullback(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sig := NewEngine().Evaluate(mtfBuyInputs(now))
	if sig.Decision != DecisionEnter || sig.StrategyName != config.StrategyMTFPullback {
		t.Fatalf("engine did not dispatch to mtf_pullback: %s reason=%q", sig.Decision, sig.Reason)
	}
}

func TestMTFPullback_Name(t *testing.T) {
	if (MTFPullback{}).Name() != config.StrategyMTFPullback {
		t.Fatalf("unexpected name %q", (MTFPullback{}).Name())
	}
}

func TestMTFPullback_EntersBuyOn1hUpPullbackBreakout(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sig := MTFPullback{}.Evaluate(mtfBuyInputs(now))
	if sig.Decision != DecisionEnter {
		t.Fatalf("expected ENTER, got %s (reason=%q)", sig.Decision, sig.Reason)
	}
	if sig.Side != order.SideBuy {
		t.Fatalf("expected BUY side, got %s", sig.Side)
	}
	// Exit is STRUCTURAL: TP1 from the 1m line / ~20pips, SL
	// from the 5m wall (clipped to SL_max), and ratchet is dropped.
	if sig.TakeProfitPips < mtfTP1FloorPips || sig.TakeProfitPips > mtfTP1CapPips {
		t.Fatalf("TP should be structural within [%d,%d], got %v", mtfTP1FloorPips, mtfTP1CapPips, sig.TakeProfitPips)
	}
	if sig.StopLossPips < mtfSLMinPips || sig.StopLossPips > mtfSLMaxPips {
		t.Fatalf("SL should be structural within [%d,%d], got %v", mtfSLMinPips, mtfSLMaxPips, sig.StopLossPips)
	}
	if sig.RatchetArmPips != 0 || sig.RatchetGivebackPips != 0 {
		t.Fatalf("ratchet must be dropped (公開されている裁量手法に無い), got arm=%v give=%v", sig.RatchetArmPips, sig.RatchetGivebackPips)
	}
	if sig.Quantity != 100 || sig.ConfigID != "test-cfg" || sig.StrategyName != config.StrategyMTFPullback {
		t.Fatalf("signal metadata wrong: %+v", sig)
	}
}

func TestMTFPullback_SpreadGate(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	in := mtfBuyInputs(now)
	in.Summary.CurrentRate.SpreadPips = 99 // way over config.MaxSpreadPips
	sig := MTFPullback{}.Evaluate(in)
	if sig.Decision == DecisionEnter {
		t.Fatal("wide spread must not enter")
	}
}

func TestMTFPullback_DirectionBlocked(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	in := mtfBuyInputs(now)
	in.Config.Entry.Direction = config.DirectionSellOnly // signal is BUY → blocked
	sig := MTFPullback{}.Evaluate(in)
	if sig.Decision == DecisionEnter {
		t.Fatal("sell_only must block a BUY signal")
	}
}

func TestMTFPullback_NoTrendWhen1hMissing(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	in := mtfBuyInputs(now)
	in.Candles1h = nil
	sig := MTFPullback{}.Evaluate(in)
	if sig.Decision == DecisionEnter {
		t.Fatal("no 1h data must not enter")
	}
}

func TestMTFPullback_NoBreakoutHolds(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	in := mtfBuyInputs(now)
	// Drop the final 1m bar (the close-through) → no 2nd-touch break on 1m.
	c1 := in.Candles1m
	in.Candles1m = c1[:len(c1)-1]
	sig := MTFPullback{}.Evaluate(in)
	if sig.Decision == DecisionEnter {
		t.Fatalf("without the 1m breakout bar there is no entry, got %s", sig.Decision)
	}
}

func TestMTFPullback_InsufficientCandles1m(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	in := mtfBuyInputs(now)
	in.Candles1m = in.Candles1m[:5] // below mtfMin1mBars → cannot execute on 1m
	sig := MTFPullback{}.Evaluate(in)
	if sig.Decision == DecisionEnter {
		t.Fatalf("too few 1m bars must block entry, got %s", sig.Decision)
	}
}

// --- 1m execution / D_exit hysteresis ------------------------------------

func TestScanWallBreakout_DExitHysteresis(t *testing.T) {
	// Wall flat at 150.20. Touch (held) → a SHALLOW wobble that stays within
	// D_exit (never pulls 4.5pip clear) → re-touch → break. The wobble must NOT
	// count as a completed rejection, so this reads as a 1st-touch break: no fire.
	wall := ta.Line{Slope: 0, Intercept: 150.20}
	w := 150.20
	high := []float64{w - 0.10, w - 0.005, w - 0.016, w - 0.005, w + 0.06}
	cl := []float64{w - 0.10, w - 0.03, w - 0.03, w - 0.03, w + 0.05}
	low := make([]float64, len(high))
	for i := range high {
		low[i] = cl[i] - 0.01
	}
	fire, episodes := scanWallBreakout(barsHLC(high, low, cl), wallPx(wall, len(cl)), "up", 3, 1, mtfDExitPips, 15, mtfPip)
	if episodes != 0 {
		t.Fatalf("a wobble within D_exit must not complete a rejection episode, got %d", episodes)
	}
	if fire {
		t.Fatal("no completed prior rejection (wobble stayed inside D_exit) → must not fire")
	}
}

func TestWallPriceSeries_ProjectsByTime(t *testing.T) {
	origin := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wall := ta.Line{Slope: -0.02, Intercept: 150.44} // per 5m-bar
	c := []market.Candle{
		{OpenTime: origin.Add(50 * time.Minute)}, // 5m-index x = 10
		{OpenTime: origin.Add(55 * time.Minute)}, // x = 11
	}
	got := wallPriceSeries(c, wall, origin, mtf5mBarMinutes)
	if math.Abs(got[0]-wall.At(10)) > 1e-9 || math.Abs(got[1]-wall.At(11)) > 1e-9 {
		t.Fatalf("projection wrong: got %v want [%v %v]", got, wall.At(10), wall.At(11))
	}
}

func TestCandles1mSince(t *testing.T) {
	mk := func(min int) market.Candle {
		return market.Candle{OpenTime: time.Date(2026, 1, 1, 0, min, 0, 0, time.UTC)}
	}
	c := []market.Candle{mk(10), mk(20), mk(30), mk(40)}
	got := candles1mSince(c, time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC))
	if len(got) != 2 || !got[0].OpenTime.Equal(mk(30).OpenTime) {
		t.Fatalf("expected 2 bars at/after 00:30, got %d", len(got))
	}
	if candles1mSince(c, time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)) != nil {
		t.Fatal("nothing at/after origin → nil")
	}
}

// --- structural exit -----------------------------------------------------

func TestStructuralSLPips(t *testing.T) {
	pip := mtfPip
	if got := structuralSLPips(150.20, 150.14, order.SideBuy, 2, 3, 12, pip); math.Abs(got-8) > 1e-9 {
		t.Fatalf("buy SL (6pip+2buffer): want 8, got %v", got)
	}
	if got := structuralSLPips(150.50, 150.14, order.SideBuy, 2, 3, 12, pip); got != 12 {
		t.Fatalf("buy SL far wall must clip to SL_max 12, got %v", got)
	}
	if got := structuralSLPips(150.141, 150.14, order.SideBuy, 2, 3, 12, pip); got != 3 {
		t.Fatalf("buy SL tiny gap must floor to SL_min 3, got %v", got)
	}
	if got := structuralSLPips(150.14, 150.20, order.SideSell, 2, 3, 12, pip); math.Abs(got-8) > 1e-9 {
		t.Fatalf("sell SL (wall above): want 8, got %v", got)
	}
}

func TestStructuralTP1Pips(t *testing.T) {
	pip := mtfPip
	chNear := ta.Channel{Upper: ta.Line{Intercept: 150.30}, Lower: ta.Line{Intercept: 150.00}}
	if got := structuralTP1Pips(150.20, order.SideBuy, chNear, true, 0, 20, 5, pip); math.Abs(got-10) > 1e-9 {
		t.Fatalf("near forward line (10pip): want 10, got %v", got)
	}
	chFar := ta.Channel{Upper: ta.Line{Intercept: 150.60}, Lower: ta.Line{Intercept: 150.00}}
	if got := structuralTP1Pips(150.20, order.SideBuy, chFar, true, 0, 20, 5, pip); got != 20 {
		t.Fatalf("far line must cap at ~20pips, got %v", got)
	}
	if got := structuralTP1Pips(150.20, order.SideBuy, ta.Channel{}, false, 0, 20, 5, pip); got != 20 {
		t.Fatalf("no channel → fall back to cap 20, got %v", got)
	}
	chBehind := ta.Channel{Upper: ta.Line{Intercept: 150.10}, Lower: ta.Line{Intercept: 149.90}}
	if got := structuralTP1Pips(150.20, order.SideBuy, chBehind, true, 0, 20, 5, pip); got != 20 {
		t.Fatalf("line behind price → fall back to cap 20, got %v", got)
	}
	chClose := ta.Channel{Upper: ta.Line{Intercept: 150.22}, Lower: ta.Line{Intercept: 150.00}}
	if got := structuralTP1Pips(150.20, order.SideBuy, chClose, true, 0, 20, 5, pip); got != 5 {
		t.Fatalf("very close line must floor to 5, got %v", got)
	}
	chSell := ta.Channel{Upper: ta.Line{Intercept: 150.40}, Lower: ta.Line{Intercept: 150.12}}
	if got := structuralTP1Pips(150.20, order.SideSell, chSell, true, 0, 20, 5, pip); math.Abs(got-8) > 1e-9 {
		t.Fatalf("sell TP (lower line 8pip below): want 8, got %v", got)
	}
}
