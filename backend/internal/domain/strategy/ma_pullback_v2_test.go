package strategy

import (
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/ta"
)

// ---------------------------------------------------------------------------
// ma_pullback_v2 tests. ma_pullback_v2 is a SEPARATE strategy from ma_pullback:
// it reuses ma_pullback's building blocks but changes exactly two loss levers:
//   - ENTRY quality: trend off the 1h 200EMA with a HIGHER slope gate, plus a
//     higher-low/lower-high STRUCTURE gate (buying pullbacks that already broke
//     structure stops out far more often).
//   - EXIT payoff: a single broker-OCO exit = structural SL + fixed TP=2×SL,
//     ratchet OFF (a tight trailing ratchet caps winners well below the
//     structural TP). MaxHold shortened to a daytrade horizon.
// ma_pullback.go is left unchanged.
// ---------------------------------------------------------------------------

// --- pure helper unit tests --------------------------------------------------

func TestHigherLowIntact(t *testing.T) {
	// Uptrend: latest swing low (higher Index) must sit ABOVE the prior one.
	lowsHL := []ta.Swing{
		{Index: 210, Price: 150.10, Kind: ta.SwingLow},
		{Index: 230, Price: 150.20, Kind: ta.SwingLow}, // latest = higher → intact
	}
	if !higherLowIntact(lowsHL, nil, trendUp) {
		t.Fatal("uptrend higher-low should be intact")
	}
	// Uptrend with a LOWER latest low = structure broken (falling knife).
	lowsBroken := []ta.Swing{
		{Index: 210, Price: 150.20, Kind: ta.SwingLow},
		{Index: 230, Price: 150.10, Kind: ta.SwingLow}, // latest = lower → broken
	}
	if higherLowIntact(lowsBroken, nil, trendUp) {
		t.Fatal("uptrend lower-low should be REJECTED (structure broken)")
	}
	// Downtrend: latest swing high must sit BELOW the prior (lower-high).
	highsLH := []ta.Swing{
		{Index: 210, Price: 150.90, Kind: ta.SwingHigh},
		{Index: 230, Price: 150.80, Kind: ta.SwingHigh}, // latest = lower → intact
	}
	if !higherLowIntact(nil, highsLH, trendDown) {
		t.Fatal("downtrend lower-high should be intact")
	}
	// Fewer than 2 relevant swings → no confirmed structure → false.
	if higherLowIntact([]ta.Swing{{Index: 230, Price: 150.20, Kind: ta.SwingLow}}, nil, trendUp) {
		t.Fatal("single swing low → structure NOT confirmed")
	}
	// flat trend has no structure side.
	if higherLowIntact(lowsHL, highsLH, trendFlat) {
		t.Fatal("flat trend → false")
	}
	// Order independence: unsorted input must still pick the highest-Index swing.
	lowsUnsorted := []ta.Swing{
		{Index: 230, Price: 150.20, Kind: ta.SwingLow},
		{Index: 210, Price: 150.10, Kind: ta.SwingLow},
	}
	if !higherLowIntact(lowsUnsorted, nil, trendUp) {
		t.Fatal("unsorted swings: latest is Index 230 (higher) → intact")
	}
}

func TestRMultipleTPPips(t *testing.T) {
	if got := rMultipleTPPips(10, 2.0); !approxF(got, 20) {
		t.Fatalf("TP = %v, want 20 (= 2.0 × 10 SL)", got)
	}
	if got := rMultipleTPPips(8, 2.5); !approxF(got, 20) {
		t.Fatalf("TP = %v, want 20 (= 2.5 × 8 SL)", got)
	}
}

func TestMAPullbackV2_Name(t *testing.T) {
	if got := (MAPullbackV2{}).Name(); got != config.StrategyMAPullbackV2 {
		t.Fatalf("Name() = %q, want %q", got, config.StrategyMAPullbackV2)
	}
}

// --- Evaluate integration ----------------------------------------------------

// maV2Input builds an EvalInput primed for a ma_pullback_v2 ENTER: a 1h uptrend/downtrend
// (read off the 1h 200EMA slope), a 5m pullback to the 5m 200MA with a confirmed
// higher-low (uptrend) / lower-high (downtrend) STRUCTURE, and a rebound bar.
// Mutations touch only High/Low/Open (never Close) so the close-based MAs match
// the values computed here. up=true → BUY scenario, up=false → SELL.
func maV2Input(up bool) (EvalInput, float64) {
	symbol, pip, base := "USD_JPY", maPip, 150.00
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sign := 1.0
	if !up {
		sign = -1.0
	}
	// 240-bar 5m ramp drifting 0.3 pip/bar → 5m 200SMA slopes, EMA on trend side.
	n := 240
	c5 := make([]market.Candle, n)
	for i := 0; i < n; i++ {
		c5[i] = maCandleP(t0, i, base+float64(i)*sign*0.3*pip, symbol, pip)
	}
	sma, _ := ta.SMA(closesOf(c5), maPB5mPeriod)

	// Plant TWO swing pivots so the STRUCTURE gate has ≥2 to compare. For an
	// uptrend: an earlier LOWER swing low (j1) and a later HIGHER swing low (j2)
	// = higher-low intact. For a downtrend: an earlier HIGHER high and a later
	// LOWER high = lower-high intact. Edit High/Low only (Close untouched).
	j1, j2 := 208, 230
	if up {
		c5[j1].Low = c5[j1].Close - 10*pip // deeper earlier low
		c5[j2].Low = c5[j2].Close - 5*pip  // later low; ramp rise makes its PRICE higher → higher-low
	} else {
		c5[j1].High = c5[j1].Close + 10*pip // higher earlier high
		c5[j2].High = c5[j2].Close + 5*pip  // later high; ramp fall makes its PRICE lower → lower-high
	}

	// Last 5m bar confirms the rebound in the trend direction (edit Open only).
	last := n - 1
	if up {
		c5[last].Open = c5[last].Close - pip // bullish
	} else {
		c5[last].Open = c5[last].Close + pip // bearish
	}

	// Live rate: mid sits on the 5m SMA (pulled back to the MA), spread 1 pip.
	mid := sma
	cur := market.CurrentRate{Bid: mid - 0.5*pip, Ask: mid + 0.5*pip, SpreadPips: 1.0}

	in := EvalInput{
		Now:       t0.Add(time.Duration(n) * 5 * time.Minute),
		Summary:   &market.MarketSummary{Symbol: symbol, CurrentRate: cur},
		Candles5m: c5,
		// 1h trend source: ramp 0.5 pip/bar → 1h 200EMA slope ≈ 10 pips/20 bars
		// (> the 8-pip slope gate). 230 bars > the 220 the slope window needs.
		Candles1h: ma1hSeries(symbol, pip, base, up, 230),
		Config: &config.StrategyConfig{
			ConfigID: "frozen-mapb-v3-2",
			Symbol:   symbol,
			Enabled:  true,
			Strategy: config.StrategySection{Name: config.StrategyMAPullbackV2},
			Entry:    config.EntrySection{MaxSpreadPips: 1.0, Direction: config.DirectionBoth},
			Risk:     config.ConfigRiskSection{Quantity: 1000},
		},
	}
	return in, sma
}

func TestMAPullbackV2_Evaluate_HappyBuy(t *testing.T) {
	in, _ := maV2Input(true)
	sig := MAPullbackV2{}.Evaluate(in)
	if sig.Decision != DecisionEnter {
		t.Fatalf("expected ENTER, got %s (reason=%s)", sig.Decision, sig.Reason)
	}
	if sig.Side != order.SideBuy {
		t.Fatalf("expected BUY (uptrend), got %s", sig.Side)
	}
	// Single broker-OCO exit: TP is a FIXED multiple of SL (the payoff fix).
	if !approxF(sig.TakeProfitPips, maPBV2TPRMultiple*sig.StopLossPips) {
		t.Errorf("TP = %.2f, want %.2f (= %.1f × SL %.2f)",
			sig.TakeProfitPips, maPBV2TPRMultiple*sig.StopLossPips, maPBV2TPRMultiple, sig.StopLossPips)
	}
	if sig.StopLossPips < maPBV2SLMinPips-1e-9 {
		t.Errorf("SL = %.2f, want ≥ floor %.0f", sig.StopLossPips, maPBV2SLMinPips)
	}
	// Ratchet OFF — ma_pullback_v2 does NOT trail (single fixed OCO, live-faithful).
	if sig.RatchetArmPips != 0 || sig.RatchetGivebackPips != 0 {
		t.Errorf("ratchet must be OFF, got arm %.1f give %.1f", sig.RatchetArmPips, sig.RatchetGivebackPips)
	}
	// Daytrade time stop (shorter than ma_pullback's 480).
	if sig.MaxHoldMinutes != maPBV2MaxHoldMinutes {
		t.Errorf("max_hold = %d, want %d", sig.MaxHoldMinutes, maPBV2MaxHoldMinutes)
	}
	// Extension/early-exit OFF.
	if sig.ExtensionMaxMinutes != 0 || sig.EarlyExitWindowMinutes != 0 || sig.EarlyExitTargetPips != 0 {
		t.Errorf("extension/early-exit must be OFF")
	}
	if sig.Quantity != 1000 {
		t.Errorf("quantity = %d, want 1000", sig.Quantity)
	}
}

func TestMAPullbackV2_Evaluate_HappySell(t *testing.T) {
	in, _ := maV2Input(false)
	sig := MAPullbackV2{}.Evaluate(in)
	if sig.Decision != DecisionEnter || sig.Side != order.SideSell {
		t.Fatalf("expected ENTER SELL (downtrend), got %s/%s (reason=%s)", sig.Decision, sig.Side, sig.Reason)
	}
}

// A near-flat 1h trend (slope below the gate) must NOT trade.
func TestMAPullbackV2_Evaluate_FlatTrend_NoTrade(t *testing.T) {
	in, _ := maV2Input(true)
	// Flatten the 1h series: all closes equal → zero slope → no trend.
	flat := make([]market.Candle, len(in.Candles1h))
	for i := range in.Candles1h {
		c := in.Candles1h[i]
		c.Open, c.High, c.Low, c.Close = 150.00, 150.01, 149.99, 150.00
		flat[i] = c
	}
	in.Candles1h = flat
	sig := MAPullbackV2{}.Evaluate(in)
	if sig.Decision == DecisionEnter {
		t.Fatalf("flat trend must not ENTER, got ENTER (reason=%s)", sig.Reason)
	}
}

// A pullback that breaks structure (a LOWER low in an uptrend) must be declined.
func TestMAPullbackV2_Evaluate_StructureBroken_Declines(t *testing.T) {
	in, _ := maV2Input(true)
	// Make the latest swing low (j2=230) LOWER than the earlier one (j1=208) →
	// higher-low broken (falling knife). The 22-bar ramp rises ≈6.6 pips between
	// them, so j2's dip must be >6.6 pips deeper than j1's to actually invert the
	// price order. Edit Low only (Close untouched → MAs unchanged).
	in.Candles5m[208].Low = in.Candles5m[208].Close - 5*maPip  // earlier, shallower
	in.Candles5m[230].Low = in.Candles5m[230].Close - 16*maPip // latest, deep → lower low → broken
	sig := MAPullbackV2{}.Evaluate(in)
	if sig.Decision == DecisionEnter {
		t.Fatalf("broken higher-low must not ENTER, got ENTER (reason=%s)", sig.Reason)
	}
}
