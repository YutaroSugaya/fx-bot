package strategy

import (
	"math"
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

func efCandle(o, h, l, c float64) market.Candle {
	return market.Candle{Symbol: "USD_JPY", Open: o, High: h, Low: l, Close: c}
}

func efNow() time.Time { return time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC) }

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// testFadeParams: small/clean knobs so a short hand-built series exercises the
// detector. ERLookback=60 means the Kaufman ER is unavailable on the ~13-bar
// fixture (ok=false) → the trend kill-switch does not fire here; it is tested
// separately with ERLookback=10.
func testFadeParams() ExhaustionFadeParams {
	return ExhaustionFadeParams{
		MAPeriod: 20, ATRWindow: 14, // used by the Strategy wrapper, not the pure detector
		MinDeviationATR: 1.5, MaxDeviationATR: 3.0,
		MinRunPips: 8, RunLookback: 10,
		LevelTolerancePips: 3, RoundStep: 0.50, SwingN: 2, SwingMinPromPips: 0,
		NewHighLookback: 3, MinWickToBody: 2.0,
		ERLookback: 60, MaxEfficiencyRatio: 0.65,
		SLBufferATR: 0.5, TPRevertFrac: 0.5, MinTPPips: 2,
		MaxHoldMinutes: 60,
	}
}

// upSpikeStallSeries: 3 flat bars at 149.70, a clean run up that pokes the
// 150.00 round number (climax bar H=150.01), then two stall bars that fail to
// make a new high, ending in a bearish shooting-star (body 1p, upper wick 2.5p).
// refMA 149.70 / refATR 12p → price sits ~2.2 ATR above the mean. The textbook
// exhaustion-fade SHORT.
func upSpikeStallSeries() []market.Candle {
	return []market.Candle{
		efCandle(149.70, 149.70, 149.70, 149.70),
		efCandle(149.70, 149.70, 149.70, 149.70),
		efCandle(149.70, 149.70, 149.70, 149.70),
		efCandle(149.70, 149.75, 149.70, 149.74),
		efCandle(149.74, 149.79, 149.74, 149.78),
		efCandle(149.78, 149.83, 149.78, 149.82),
		efCandle(149.82, 149.87, 149.82, 149.86),
		efCandle(149.86, 149.91, 149.86, 149.90),
		efCandle(149.90, 149.95, 149.90, 149.94),
		efCandle(149.94, 150.01, 149.94, 149.99),   // climax, pokes 150.00
		efCandle(149.99, 149.99, 149.95, 149.98),   // stall (no new high)
		efCandle(149.98, 149.99, 149.95, 149.97),   // stall
		efCandle(149.975, 150.00, 149.96, 149.965), // shooting star, closes down
	}
}

func TestDetectExhaustionFade_FiresSellOnExhaustion(t *testing.T) {
	p := testFadeParams()
	prop := DetectExhaustionFade(upSpikeStallSeries(), 149.70, 12, 0.01, p)
	if !prop.Found {
		t.Fatalf("expected a fade SELL, got not-found: %s", prop.Reason)
	}
	if prop.Side != order.SideSell {
		t.Fatalf("up-spike → SELL, got %s", prop.Side)
	}
	// Stop sits ABOVE the spike high (150.01) + 0.5*12p buffer = 150.07 → 10.5p.
	if !near(prop.StopPips, 10.5, 0.2) {
		t.Errorf("StopPips expected ~10.5, got %v (stop=%.3f)", prop.StopPips, prop.Stop)
	}
	if prop.Stop <= prop.SpikeExtreme {
		t.Errorf("stop %.3f must be beyond spike high %.3f", prop.Stop, prop.SpikeExtreme)
	}
	// TP = 0.5 × run(26.5p) ≈ 13.25p.
	if !near(prop.RewardPips, 13.25, 0.3) {
		t.Errorf("RewardPips expected ~13.25, got %v", prop.RewardPips)
	}
}

func TestDetectExhaustionFade_NoFire_StillMakingNewHighs(t *testing.T) {
	cs := upSpikeStallSeries()
	cs[len(cs)-1] = efCandle(149.975, 150.05, 149.96, 150.04) // last bar makes a new high → momentum intact
	if prop := DetectExhaustionFade(cs, 149.70, 12, 0.01, testFadeParams()); prop.Found {
		t.Fatalf("still making new highs must NOT fade, got %s", prop.Reason)
	}
}

func TestDetectExhaustionFade_NoFire_NotStretched(t *testing.T) {
	// Huge refATR → deviation falls below MinDeviationATR.
	if prop := DetectExhaustionFade(upSpikeStallSeries(), 149.70, 40, 0.01, testFadeParams()); prop.Found {
		t.Fatalf("not stretched must NOT fade, got %s", prop.Reason)
	}
}

func TestDetectExhaustionFade_NoFire_TooStretched(t *testing.T) {
	// Small refATR → deviation exceeds MaxDeviationATR (3σ+ chase zone).
	if prop := DetectExhaustionFade(upSpikeStallSeries(), 149.70, 6, 0.01, testFadeParams()); prop.Found {
		t.Fatalf("over-stretched (chase) must NOT fade, got %s", prop.Reason)
	}
}

func TestDetectExhaustionFade_NoFire_NoLevel(t *testing.T) {
	// Shift the whole series +0.20 so the spike high (150.21) is far from any
	// .00/.50 round number and there is no prior swing → no level to lean on.
	cs := upSpikeStallSeries()
	for i := range cs {
		cs[i] = efCandle(cs[i].Open+0.20, cs[i].High+0.20, cs[i].Low+0.20, cs[i].Close+0.20)
	}
	if prop := DetectExhaustionFade(cs, 149.90, 12, 0.01, testFadeParams()); prop.Found {
		t.Fatalf("no level near the spike must NOT fade, got %s", prop.Reason)
	}
}

func TestDetectExhaustionFade_KillSwitch_EfficiencyRatio(t *testing.T) {
	// Same exhaustion setup, but with a SHORT ER window (10) the run is measured
	// as a clean, efficient trend (ER ~0.84) → the trend kill-switch blocks the fade.
	p := testFadeParams()
	p.ERLookback = 10
	p.MaxEfficiencyRatio = 0.65
	if prop := DetectExhaustionFade(upSpikeStallSeries(), 149.70, 12, 0.01, p); prop.Found {
		t.Fatalf("high efficiency (strong trend) must NOT fade, got %s", prop.Reason)
	}
}

func TestDetectExhaustionFade_FiresBuyOnDownSpike(t *testing.T) {
	// Mirror: a down-spike into 149.00 that stalls with a bullish hammer (long lower wick).
	cs := []market.Candle{
		efCandle(150.30, 150.30, 150.30, 150.30),
		efCandle(150.30, 150.30, 150.30, 150.30),
		efCandle(150.30, 150.30, 150.30, 150.30),
		efCandle(150.30, 150.30, 150.25, 150.26),
		efCandle(150.26, 150.26, 150.21, 150.22),
		efCandle(150.22, 150.22, 150.17, 150.18),
		efCandle(150.18, 150.18, 150.13, 150.14),
		efCandle(150.14, 150.14, 150.09, 150.10),
		efCandle(150.10, 150.10, 150.05, 150.06),
		efCandle(150.06, 150.06, 149.99, 150.01),   // climax low pokes 150.00
		efCandle(150.01, 150.05, 150.01, 150.02),   // stall (no new low)
		efCandle(150.02, 150.06, 150.01, 150.03),   // stall
		efCandle(150.025, 150.04, 150.00, 150.035), // hammer, closes up
	}
	prop := DetectExhaustionFade(cs, 150.30, 12, 0.01, testFadeParams())
	if !prop.Found || prop.Side != order.SideBuy {
		t.Fatalf("down-spike → BUY fade, got found=%v side=%s reason=%s", prop.Found, prop.Side, prop.Reason)
	}
	if prop.Stop >= prop.SpikeExtreme {
		t.Errorf("buy stop %.3f must be below spike low %.3f", prop.Stop, prop.SpikeExtreme)
	}
}

func TestExhaustionFade_Evaluate_FiresThroughGates(t *testing.T) {
	// 25 1h reference bars: flat closes (SMA20=149.70) with a 12-pip range each
	// (ATR(14)=12p) — the stretch anchor the detector test used as scalars.
	h1 := make([]market.Candle, 25)
	for i := range h1 {
		h1[i] = efCandle(149.70, 149.76, 149.64, 149.70)
	}
	cfg := &config.StrategyConfig{
		Symbol:   "USD_JPY",
		Enabled:  true,
		Strategy: config.StrategySection{Name: config.StrategyExhaustionFade},
		Entry:    config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 3},
		Risk:     config.ConfigRiskSection{Quantity: 1000},
	}
	in := EvalInput{
		Now:       efNow(),
		Config:    cfg,
		Summary:   &market.MarketSummary{CurrentRate: market.CurrentRate{Bid: 149.965, Ask: 149.966, SpreadPips: 0.1}},
		Candles1m: upSpikeStallSeries(),
		Candles1h: h1,
	}
	sig := ExhaustionFade{Params: testFadeParams()}.Evaluate(in)
	if sig.Decision != DecisionEnter || sig.Side != order.SideSell {
		t.Fatalf("expected ENTER/SELL, got %s/%s reason=%s", sig.Decision, sig.Side, sig.Reason)
	}
	if sig.Quantity != 1000 || sig.StopLossPips <= 0 || sig.TakeProfitPips <= 0 || sig.MaxHoldMinutes <= 0 {
		t.Errorf("signal fields off: qty=%d sl=%.1f tp=%.1f hold=%d", sig.Quantity, sig.StopLossPips, sig.TakeProfitPips, sig.MaxHoldMinutes)
	}
	if sig.EntryPrice != 149.965 { // SELL fills at bid
		t.Errorf("SELL entry should be bid 149.965, got %v", sig.EntryPrice)
	}
	// direction gate: buy_only must block a SELL fade.
	cfg.Entry.Direction = config.DirectionBuyOnly
	if s := (ExhaustionFade{Params: testFadeParams()}).Evaluate(in); s.Decision == DecisionEnter {
		t.Errorf("buy_only must block the SELL fade, got %s", s.Reason)
	}
}

func TestExhaustionFade_Evaluate_SLCap(t *testing.T) {
	h1 := make([]market.Candle, 25)
	for i := range h1 {
		h1[i] = efCandle(149.70, 149.76, 149.64, 149.70)
	}
	mkCfg := func(slCap float64) *config.StrategyConfig {
		return &config.StrategyConfig{
			Symbol:   "USD_JPY",
			Enabled:  true,
			Strategy: config.StrategySection{Name: config.StrategyExhaustionFade},
			Entry:    config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 3},
			Exit:     config.ExitSection{StopLossPips: slCap},
			Risk:     config.ConfigRiskSection{Quantity: 1000},
		}
	}
	mkIn := func(cfg *config.StrategyConfig) EvalInput {
		return EvalInput{
			Now: efNow(), Config: cfg,
			Summary:   &market.MarketSummary{CurrentRate: market.CurrentRate{Bid: 149.965, Ask: 149.966, SpreadPips: 0.1}},
			Candles1m: upSpikeStallSeries(), Candles1h: h1,
		}
	}
	// High cap (100) does not bind → structural SL (~10.5p) flows through.
	hi := ExhaustionFade{Params: testFadeParams()}.Evaluate(mkIn(mkCfg(100)))
	if hi.Decision != DecisionEnter || !near(hi.StopLossPips, 10.5, 0.3) {
		t.Fatalf("cap=100 should not bind, expected SL~10.5, got %.2f (%s)", hi.StopLossPips, hi.Reason)
	}
	// Tight cap (6) binds → SL is capped to 6.
	cap6 := ExhaustionFade{Params: testFadeParams()}.Evaluate(mkIn(mkCfg(6)))
	if cap6.Decision != DecisionEnter || cap6.StopLossPips != 6 {
		t.Fatalf("cap=6 should cap SL to 6, got %.2f (%s)", cap6.StopLossPips, cap6.Reason)
	}
}

func TestExhaustionFade_Evaluate_HTFTrendGate(t *testing.T) {
	mkIn := func(h1 []market.Candle, htfMax float64) EvalInput {
		return EvalInput{
			Now: efNow(),
			Config: &config.StrategyConfig{
				Symbol: "USD_JPY", Enabled: true,
				Strategy: config.StrategySection{Name: config.StrategyExhaustionFade},
				Entry:    config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 3, HTFEfficiencyMax: htfMax},
				Risk:     config.ConfigRiskSection{Quantity: 1000},
			},
			Summary:   &market.MarketSummary{CurrentRate: market.CurrentRate{Bid: 149.965, Ask: 149.966, SpreadPips: 0.1}},
			Candles1m: upSpikeStallSeries(), Candles1h: h1,
		}
	}
	// Flat 1h closes (efficiency ratio undefined) → gate cannot apply → fires.
	flat := make([]market.Candle, 25)
	for i := range flat {
		flat[i] = efCandle(149.70, 149.76, 149.64, 149.70)
	}
	if sig := (ExhaustionFade{Params: testFadeParams()}).Evaluate(mkIn(flat, 0.40)); sig.Decision != DecisionEnter {
		t.Fatalf("flat 1h (ER undefined) should still fire, got %s/%s", sig.Decision, sig.Reason)
	}
	// Clean rising 1h trend (ER≈1 >= 0.40) → "don't fade a strong trend" → blocked.
	trend := make([]market.Candle, 25)
	for i := range trend {
		base := 149.00 + float64(i)*0.05
		trend[i] = efCandle(base, base+0.06, base-0.02, base+0.05)
	}
	if sig := (ExhaustionFade{Params: testFadeParams()}).Evaluate(mkIn(trend, 0.40)); sig.Decision == DecisionEnter {
		t.Fatalf("strong 1h uptrend must block the fade, got ENTER (%s)", sig.Reason)
	}
	// Same strong trend but gate OFF (htfMax=0) → fires (gate is opt-in).
	if sig := (ExhaustionFade{Params: testFadeParams()}).Evaluate(mkIn(trend, 0)); sig.Decision != DecisionEnter {
		t.Fatalf("gate OFF should fire even in a trend, got %s/%s", sig.Decision, sig.Reason)
	}
}

func TestDetectExhaustionFade_BandPass_RunEfficiencyFloor(t *testing.T) {
	// The clean monotonic up-spike has a run-window Kaufman ER ≈ 0.82.
	p := testFadeParams()
	p.MinRunEfficiencyRatio = 0.5 // floor below 0.82 → sharp enough → still fades.
	if prop := DetectExhaustionFade(upSpikeStallSeries(), 149.70, 12, 0.01, p); !prop.Found {
		t.Fatalf("an efficient spike must pass a 0.5 run-efficiency floor, got %s", prop.Reason)
	}
	p.MinRunEfficiencyRatio = 0.9 // floor above 0.82 → too choppy relative to the floor → blocked.
	if prop := DetectExhaustionFade(upSpikeStallSeries(), 149.70, 12, 0.01, p); prop.Found {
		t.Fatalf("a run below the efficiency floor must NOT fade, got found (%s)", prop.Reason)
	}
}

// divergenceFadeSeries: a FIRST swing high at 149.93 reached cleanly (RSI 100),
// a pullback, then a choppy grind to a HIGHER spike high 150.01 that pokes
// 150.00 with weaker momentum (RSI < 100) and a closing rejection wick — the
// textbook bearish regular divergence on top of a fadeable exhausted spike.
func divergenceFadeSeries() []market.Candle {
	return []market.Candle{
		efCandle(149.85, 149.86, 149.84, 149.85),
		efCandle(149.85, 149.86, 149.84, 149.85),
		efCandle(149.85, 149.86, 149.84, 149.85),
		efCandle(149.85, 149.93, 149.85, 149.92), // first swing high (clean → RSI 100)
		efCandle(149.92, 149.92, 149.87, 149.88),
		efCandle(149.88, 149.89, 149.85, 149.86), // pullback
		efCandle(149.86, 149.90, 149.86, 149.89),
		efCandle(149.89, 149.93, 149.89, 149.92),
		efCandle(149.92, 149.92, 149.88, 149.89), // choppy dip
		efCandle(149.89, 149.95, 149.89, 149.94),
		efCandle(149.94, 149.97, 149.93, 149.94), // choppy dip
		efCandle(149.94, 149.98, 149.94, 149.97),
		efCandle(149.97, 149.99, 149.96, 149.98),
		efCandle(149.98, 150.01, 149.98, 150.00),   // higher spike high, pokes 150.00 (weaker RSI)
		efCandle(150.00, 150.00, 149.96, 149.98),   // stall (no new high)
		efCandle(149.975, 150.00, 149.96, 149.965), // rejection wick, closes down
	}
}

func TestDetectExhaustionFade_Divergence(t *testing.T) {
	p := testFadeParams()
	p.RSIPeriod = 3

	// (a) require divergence, but the clean single-spike series has only one
	// swing high → nothing to diverge against → blocked.
	p.RequireDivergence = true
	if prop := DetectExhaustionFade(upSpikeStallSeries(), 149.70, 12, 0.01, p); prop.Found {
		t.Fatalf("require-divergence with a single swing high must block, got found (%s)", prop.Reason)
	}
	// (b) a two-high series (HH price + lower RSI = bearish divergence) passes.
	if prop := DetectExhaustionFade(divergenceFadeSeries(), 149.70, 12, 0.01, p); !prop.Found || prop.Side != order.SideSell {
		t.Fatalf("bearish divergence must allow the SELL fade, got found=%v side=%s reason=%s", prop.Found, prop.Side, prop.Reason)
	}
	// (c) sanity: gate OFF, the two-high series still fires — so (a) blocked on
	// divergence, not on some unrelated condition.
	p.RequireDivergence = false
	if prop := DetectExhaustionFade(divergenceFadeSeries(), 149.70, 12, 0.01, p); !prop.Found {
		t.Fatalf("divergence series must fire with the gate off, got %s", prop.Reason)
	}
}

func TestRoundTPPips(t *testing.T) {
	cases := []struct {
		name    string
		entry   float64
		side    order.Side
		step    float64
		offset  float64
		minTP   float64
		wantPip float64
	}{
		// SELL: nearest round below 149.62 is 149.50; exit 2p above it → 10p.
		{"sell_round_below", 149.62, order.SideSell, 0.50, 2, 8, 10},
		// SELL: 149.55 → round 149.50 gives only 3p (< floor 8) → step down to
		// 149.00 → 53p.
		{"sell_step_down_to_floor", 149.55, order.SideSell, 0.50, 2, 8, 53},
		// BUY: nearest round above 149.38 is 149.50; exit 2p below it → 10p.
		{"buy_round_above", 149.38, order.SideBuy, 0.50, 2, 8, 10},
		// On the round exactly → skip it, use the next one out.
		{"sell_on_round", 149.50, order.SideSell, 0.50, 2, 8, 48},
	}
	for _, c := range cases {
		got, ok := roundTPPips(c.entry, c.side, c.step, c.offset, c.minTP, 0.01)
		if !ok || !near(got, c.wantPip, 0.001) {
			t.Errorf("%s: roundTPPips=%v ok=%v want %v", c.name, got, ok, c.wantPip)
		}
	}
}

func TestDetectExhaustionFade_RoundTP(t *testing.T) {
	p := testFadeParams()
	// baseline revert TP ≈ 13.25 (0.5 × 26.5p run).
	base := DetectExhaustionFade(upSpikeStallSeries(), 149.70, 12, 0.01, p)
	if !base.Found || base.RewardPips < 12 {
		t.Fatalf("baseline revert TP expected ~13, got found=%v rw=%.2f", base.Found, base.RewardPips)
	}
	// round-TP on a 0.10 grid caps the exit just above the 149.90 minor level
	// (≈4.5p) — tighter than the full revert (price stalls at the round on the
	// way down). The fade still fires; only the TP is tightened.
	p.RoundTP = true
	p.RoundTPStep = 0.10
	p.RoundTPOffsetPips = 2
	r := DetectExhaustionFade(upSpikeStallSeries(), 149.70, 12, 0.01, p)
	if !r.Found {
		t.Fatalf("round-TP setup must still fade, got %s", r.Reason)
	}
	if !near(r.RewardPips, 4.5, 0.2) {
		t.Errorf("round-TP should cap TP ~4.5 (just above 149.90), got %.2f", r.RewardPips)
	}
	if r.Target <= 149.90 {
		t.Errorf("SELL round-TP target %.3f must sit just ABOVE the 149.90 round", r.Target)
	}
}

func TestHTFTrendBlocksFade(t *testing.T) {
	up := []float64{149.00, 149.10, 149.20, 149.30}   // +30p over 3 bars
	down := []float64{149.30, 149.20, 149.10, 149.00} // -30p
	mild := []float64{149.00, 149.02, 149.05, 149.07} // +7p

	// SELL fade fights a strong UP trend → block; a BUY fade ALIGNS with it → allow.
	if !htfTrendBlocksFade(up, 3, 20, 0.01, order.SideSell) {
		t.Error("SELL into a strong uptrend must be blocked")
	}
	if htfTrendBlocksFade(up, 3, 20, 0.01, order.SideBuy) {
		t.Error("BUY aligned with the uptrend must be allowed")
	}
	// Mirror for a downtrend.
	if !htfTrendBlocksFade(down, 3, 20, 0.01, order.SideBuy) {
		t.Error("BUY into a strong downtrend must be blocked")
	}
	if htfTrendBlocksFade(down, 3, 20, 0.01, order.SideSell) {
		t.Error("SELL aligned with the downtrend must be allowed")
	}
	// Slope below the threshold, veto off, and short history → never block.
	if htfTrendBlocksFade(mild, 3, 20, 0.01, order.SideSell) {
		t.Error("a mild slope below the threshold must not block")
	}
	if htfTrendBlocksFade(up, 3, 0, 0.01, order.SideSell) {
		t.Error("veto off (0 pips) must not block")
	}
	if htfTrendBlocksFade(up, 10, 20, 0.01, order.SideSell) {
		t.Error("insufficient history must not block")
	}
}

// mtf1hUptrendAnchor: 25 1h bars whose SMA20 / ATR(14) are the same flat
// 149.70 / 12p anchor the detector tests use (so the fade still fires), but
// whose 24-bar net move is a strong +30p uptrend (the MTF veto trigger).
func mtf1hUptrendAnchor() []market.Candle {
	h := make([]market.Candle, 25)
	for i := range h {
		h[i] = efCandle(149.70, 149.76, 149.64, 149.70)
	}
	h[0] = efCandle(149.40, 149.46, 149.34, 149.40) // 24-bar net = +30p
	return h
}

func TestExhaustionFade_Evaluate_MTFDirectionalVeto(t *testing.T) {
	mkIn := func(vetoPips float64) EvalInput {
		return EvalInput{
			Now: efNow(),
			Config: &config.StrategyConfig{
				Symbol: "USD_JPY", Enabled: true,
				Strategy: config.StrategySection{Name: config.StrategyExhaustionFade},
				Entry:    config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 3, HTFTrendVetoPips: vetoPips},
				Risk:     config.ConfigRiskSection{Quantity: 1000},
			},
			Summary:   &market.MarketSummary{CurrentRate: market.CurrentRate{Bid: 149.965, Ask: 149.966, SpreadPips: 0.1}},
			Candles1m: upSpikeStallSeries(), Candles1h: mtf1hUptrendAnchor(),
		}
	}
	// veto OFF → the SELL fade fires despite the 1h uptrend.
	if sig := (ExhaustionFade{Params: testFadeParams()}).Evaluate(mkIn(0)); sig.Decision != DecisionEnter || sig.Side != order.SideSell {
		t.Fatalf("veto off should fire SELL, got %s/%s (%s)", sig.Decision, sig.Side, sig.Reason)
	}
	// veto ON (20p) → the SELL fade fights the +30p 1h uptrend → blocked.
	if sig := (ExhaustionFade{Params: testFadeParams()}).Evaluate(mkIn(20)); sig.Decision == DecisionEnter {
		t.Fatalf("SELL fade into a strong 1h uptrend must be vetoed, got ENTER (%s)", sig.Reason)
	}
}

func TestExhaustionFade_Evaluate_ConfigRefinementsMapping(t *testing.T) {
	h1 := make([]market.Candle, 25)
	for i := range h1 {
		h1[i] = efCandle(149.70, 149.76, 149.64, 149.70)
	}
	base := func() *config.StrategyConfig {
		return &config.StrategyConfig{
			Symbol: "USD_JPY", Enabled: true,
			Strategy: config.StrategySection{Name: config.StrategyExhaustionFade},
			Entry:    config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 3},
			Risk:     config.ConfigRiskSection{Quantity: 1000},
		}
	}
	mkIn := func(cfg *config.StrategyConfig) EvalInput {
		return EvalInput{
			Now: efNow(), Config: cfg,
			Summary:   &market.MarketSummary{CurrentRate: market.CurrentRate{Bid: 149.965, Ask: 149.966, SpreadPips: 0.1}},
			Candles1m: upSpikeStallSeries(), Candles1h: h1,
		}
	}
	// baseline (no refinements) fires SELL with the revert TP (~13p).
	bsig := ExhaustionFade{Params: testFadeParams()}.Evaluate(mkIn(base()))
	if bsig.Decision != DecisionEnter || bsig.TakeProfitPips < 12 {
		t.Fatalf("baseline must fire with revert TP ~13, got %s tp=%.1f (%s)", bsig.Decision, bsig.TakeProfitPips, bsig.Reason)
	}

	// require_rsi_divergence → blocked (clean single-spike has no divergence).
	div := base()
	div.Entry.RequireRSIDivergence = true
	if s := (ExhaustionFade{Params: testFadeParams()}).Evaluate(mkIn(div)); s.Decision == DecisionEnter {
		t.Errorf("require_rsi_divergence config must block the single-spike fade, got ENTER")
	}

	// min_run_efficiency above the run's ~0.82 → blocked.
	bp := base()
	bp.Entry.MinRunEfficiency = 0.9
	if s := (ExhaustionFade{Params: testFadeParams()}).Evaluate(mkIn(bp)); s.Decision == DecisionEnter {
		t.Errorf("min_run_efficiency config must block the sub-floor run, got ENTER")
	}

	// round_number_tp (10-pip grid) caps TP just above 149.90 (~4.5p).
	rt := base()
	rt.Exit.RoundNumberTP = true
	rt.Exit.RoundNumberTPStepPips = 10
	rt.Exit.RoundNumberTPOffsetPips = 2
	if s := (ExhaustionFade{Params: testFadeParams()}).Evaluate(mkIn(rt)); s.Decision != DecisionEnter || !near(s.TakeProfitPips, 4.5, 0.3) {
		t.Errorf("round_number_tp config should cap TP ~4.5, got %s tp=%.2f", s.Decision, s.TakeProfitPips)
	}
}

func TestDefaultExhaustionFadeParams_Sane(t *testing.T) {
	p := DefaultExhaustionFadeParams()
	if p.MinDeviationATR <= 0 || p.MaxDeviationATR <= p.MinDeviationATR {
		t.Errorf("deviation band invalid: %v..%v", p.MinDeviationATR, p.MaxDeviationATR)
	}
	if p.RunLookback < 2 || p.NewHighLookback < 1 || p.MinTPPips <= 0 {
		t.Errorf("default params not sane: %+v", p)
	}
}
