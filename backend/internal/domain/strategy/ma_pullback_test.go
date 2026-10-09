package strategy

import (
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/ta"
)

const maPip = 0.01

func approxF(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}

// --- pure helper unit tests --------------------------------------------------

func TestMATrendDir(t *testing.T) {
	// up: 200SMA rising by 6 pips over the lookback (> the 3-pip gate).
	if d := maTrendDir(150.50, 150.44, 3, maPip); d != "up" {
		t.Fatalf("rising SMA: expected up, got %q", d)
	}
	// down: 200SMA falling by 6 pips.
	if d := maTrendDir(150.44, 150.50, 3, maPip); d != "down" {
		t.Fatalf("falling SMA: expected down, got %q", d)
	}
	// SMA barely sloping (1 pip < 3 gate) → flat.
	if d := maTrendDir(150.50, 150.49, 3, maPip); d != "flat" {
		t.Fatalf("flat slope up: expected flat, got %q", d)
	}
	if d := maTrendDir(150.49, 150.50, 3, maPip); d != "flat" {
		t.Fatalf("flat slope down: expected flat, got %q", d)
	}
	// Exactly at the gate (3.0 pips): the comparison is strict (>), so flat.
	// Use pip=1.0 + integer diffs so the slope is exactly 3.0 in float64.
	if d := maTrendDir(13, 10, 3, 1.0); d != "flat" {
		t.Fatalf("slope exactly +3 (strict gate): expected flat, got %q", d)
	}
	if d := maTrendDir(10, 13, 3, 1.0); d != "flat" {
		t.Fatalf("slope exactly -3 (strict gate): expected flat, got %q", d)
	}
	// pipSize <= 0 cannot scale the slope → flat.
	if d := maTrendDir(150.50, 150.44, 3, 0); d != "flat" {
		t.Fatalf("pipSize 0: expected flat, got %q", d)
	}
}

func TestInMAZone(t *testing.T) {
	tol := 0.05 // 5 pips in price units
	if !inMAZone(150.32, 150.30, 150.50, tol) {
		t.Fatal("near SMA: expected in zone")
	}
	if !inMAZone(150.48, 150.10, 150.50, tol) {
		t.Fatal("near EMA: expected in zone")
	}
	if inMAZone(150.80, 150.30, 150.50, tol) {
		t.Fatal("far from both: expected NOT in zone")
	}
}

func TestHasConfluence(t *testing.T) {
	swings := []ta.Swing{
		{Index: 5, Price: 150.20, Kind: ta.SwingLow},
		{Index: 9, Price: 150.55, Kind: ta.SwingHigh},
	}
	if !hasConfluence(150.205, swings, 0.02) {
		t.Fatal("price near a swing low should be confluent")
	}
	if hasConfluence(150.40, swings, 0.02) {
		t.Fatal("price between swings (no level near) should NOT be confluent")
	}
}

func TestReboundConfirmed(t *testing.T) {
	bull := market.Candle{Open: 150.10, Close: 150.20}
	bear := market.Candle{Open: 150.20, Close: 150.10}
	if !reboundConfirmed(bull, "up") || reboundConfirmed(bear, "up") {
		t.Fatal("up rebound: bullish bar confirms, bearish does not")
	}
	if !reboundConfirmed(bear, "down") || reboundConfirmed(bull, "down") {
		t.Fatal("down rebound: bearish bar confirms, bullish does not")
	}
}

func TestClusterTPPips(t *testing.T) {
	band := 0.05 // 5 pips: levels within this of each other cluster (密集地帯)
	// BUY: 150.62 & 150.64 cluster (2 pips apart); 150.80 isolated. the reference method exits
	// AFTER price breaks through the dense band and runs, so we target the FAR
	// edge of the nearest cluster = 150.64 → 14 pips (not the near edge 150.62).
	highs := []ta.Swing{{Price: 150.62, Kind: ta.SwingHigh}, {Price: 150.64, Kind: ta.SwingHigh}, {Price: 150.80, Kind: ta.SwingHigh}}
	if got := clusterTPPips(150.50, order.SideBuy, highs, nil, band, 20, 6, maPip); !approxF(got, 14) {
		t.Fatalf("buy cluster: expected far edge 14, got %v", got)
	}
	// SELL: 150.38 & 150.36 cluster below entry → far edge 150.36 → 14 pips.
	lows := []ta.Swing{{Price: 150.38, Kind: ta.SwingLow}, {Price: 150.36, Kind: ta.SwingLow}, {Price: 150.20, Kind: ta.SwingLow}}
	if got := clusterTPPips(150.50, order.SideSell, nil, lows, band, 20, 6, maPip); !approxF(got, 14) {
		t.Fatalf("sell cluster: expected far edge 14, got %v", got)
	}
	// No cluster (isolated levels) → fall back to the nearest single level (12).
	iso := []ta.Swing{{Price: 150.62, Kind: ta.SwingHigh}, {Price: 150.80, Kind: ta.SwingHigh}}
	if got := clusterTPPips(150.50, order.SideBuy, iso, nil, band, 20, 6, maPip); !approxF(got, 12) {
		t.Fatalf("no cluster → nearest: expected 12, got %v", got)
	}
	// No level ahead → cap (20).
	if got := clusterTPPips(150.90, order.SideBuy, highs, nil, band, 20, 6, maPip); !approxF(got, 20) {
		t.Fatalf("no level ahead: expected cap 20, got %v", got)
	}
	// Nearest level very close → floored to 6.
	near := []ta.Swing{{Price: 150.52, Kind: ta.SwingHigh}}
	if got := clusterTPPips(150.50, order.SideBuy, near, nil, band, 20, 6, maPip); !approxF(got, 6) {
		t.Fatalf("near level: expected floor 6, got %v", got)
	}
}

func TestDayLevels(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 5 bars; oldest Open=150.00, a spike high at bar 2, a dip low at bar 3.
	c := []market.Candle{
		{OpenTime: t0, Open: 150.00, High: 150.10, Low: 149.95, Close: 150.05},
		{OpenTime: t0.Add(5 * time.Minute), Open: 150.05, High: 150.20, Low: 150.00, Close: 150.15},
		{OpenTime: t0.Add(10 * time.Minute), Open: 150.15, High: 150.60, Low: 150.10, Close: 150.40}, // day high 150.60
		{OpenTime: t0.Add(15 * time.Minute), Open: 150.40, High: 150.45, Low: 149.80, Close: 150.00}, // day low 149.80
		{OpenTime: t0.Add(20 * time.Minute), Open: 150.00, High: 150.10, Low: 149.90, Close: 150.05},
	}
	lv := dayLevels(c, 288)
	if len(lv) != 3 {
		t.Fatalf("expected 3 day levels (high/low/open), got %d", len(lv))
	}
	if !approxF(lv[0], 150.60) {
		t.Errorf("day high = %v, want 150.60", lv[0])
	}
	if !approxF(lv[1], 149.80) {
		t.Errorf("day low = %v, want 149.80", lv[1])
	}
	if !approxF(lv[2], 150.00) {
		t.Errorf("day open (oldest bar) = %v, want 150.00", lv[2])
	}
	// window smaller than the slice → only the most recent bars; open is the
	// oldest WITHIN the window (bar index 2 here).
	lv2 := dayLevels(c, 3)
	if !approxF(lv2[2], 150.15) {
		t.Errorf("windowed day open = %v, want 150.15 (oldest in last 3)", lv2[2])
	}
	if dayLevels(nil, 288) != nil {
		t.Error("no candles → nil")
	}
}

// The reference method anchors entries to その日の高値・安値・起点, not only recent swings.
// When the swing confluence is absent but the price sits on a day level (here the
// day origin = the oldest bar's open), confluence must still pass.
func TestMAPullback_Evaluate_ConfluenceViaDayLevel(t *testing.T) {
	in, sma := maReadyInput()
	// Remove the planted swing low → swing confluence alone would fail.
	in.Candles5m[230].Low = in.Candles5m[230].Close - maPip
	// Plant the day origin (oldest bar's Open) right at the entry price (= sma).
	// Open is not used by the close-based MAs, so the trend is unchanged.
	in.Candles5m[0].Open = sma
	sig := MAPullback{}.Evaluate(in)
	if sig.Decision != DecisionEnter {
		t.Fatalf("expected ENTER via day-level confluence, got %s (reason=%s)", sig.Decision, sig.Reason)
	}
}

// --- Evaluate integration (wiring) -------------------------------------------

// maCandleP builds one 5m candle: High/Low hug close by ±1 pip, Open at close.
func maCandleP(t0 time.Time, i int, closePx float64, symbol string, pip float64) market.Candle {
	return market.Candle{
		Symbol:   symbol,
		Interval: 5 * time.Minute,
		OpenTime: t0.Add(time.Duration(i) * 5 * time.Minute),
		Open:     closePx,
		High:     closePx + pip,
		Low:      closePx - pip,
		Close:    closePx,
	}
}

// ma1hSeries builds an `n`-bar 1h series ramping 0.5 pip/bar in the trend direction
// (up=true rising / false falling) → the 1h 200SMA slopes ≈10 pips over the 20-bar
// slope window (> the maPB1hMinSlopePips gate). Only Close matters (the 1h trend is
// close-based); High/Low/Open hug it. This is the higher-timeframe trend source the
// ma_pullback MTF runner reads — execution (zone/confluence/rebound/exit) stays on 5m.
func ma1hSeries(symbol string, pip, base float64, up bool, n int) []market.Candle {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sign := 1.0
	if !up {
		sign = -1.0
	}
	c := make([]market.Candle, n)
	for i := 0; i < n; i++ {
		px := base + float64(i)*sign*0.5*pip
		c[i] = market.Candle{
			Symbol:   symbol,
			Interval: time.Hour,
			OpenTime: t0.Add(time.Duration(i) * time.Hour),
			Open:     px, High: px + pip, Low: px - pip, Close: px,
		}
	}
	return c
}

// maInput builds an MTF EvalInput primed to ENTER: the 1h 200SMA gives the TREND
// (up/down), the 5m chart gives the pullback-to-MA + confluence + rebound that times
// the entry. parameterised
// by symbol/pip and direction. up=true → 1h 200SMA rising (uptrend) with 5m price
// pulled back to the 5m 200MA, a confluence swing LOW and a bullish rebound bar
// (→ BUY); up=false is the mirror (→ SELL). Mutations touch only High/Low/Open
// (never Close) so the strategy's close-based SMA/EMA equal the values computed
// here. Returns the input plus the computed 5m SMA.
func maInput(symbol string, pip, base float64, up bool) (EvalInput, float64) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sign := 1.0
	if !up {
		sign = -1.0
	}
	// 240-bar 5m ramp drifting 0.3 pip/bar → 200SMA slope ≈ 6 pips over the
	// 20-bar slope window (> the 3-pip gate), and EMA on the trend side of SMA.
	n := 240
	c5 := make([]market.Candle, n)
	for i := 0; i < n; i++ {
		c5[i] = maCandleP(t0, i, base+float64(i)*sign*0.3*pip, symbol, pip)
	}
	sma, _ := ta.SMA(closesOf(c5), maPB5mPeriod)

	// Plant a confluence swing near the SMA in the recent window (edit High/Low
	// only — Close untouched so the MAs are unchanged). j has ≥2 neighbours.
	j := 230
	if up {
		c5[j].Low = sma - 0.5*pip
	} else {
		c5[j].High = sma + 0.5*pip
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
		Now: t0.Add(time.Duration(n) * 5 * time.Minute),
		Summary: &market.MarketSummary{
			Symbol:      symbol,
			CurrentRate: cur,
		},
		Candles5m: c5,
		// 1h trend source (MTF): rising for up / falling for down. 230 bars > the
		// 220 the 1h 200SMA + slope window needs. base is irrelevant (only the slope
		// drives the trend; the side gate uses the 5m SMA).
		Candles1h: ma1hSeries(symbol, pip, base, up, 230),
		Config: &config.StrategyConfig{
			ConfigID: "frozen-mapb-v3",
			Symbol:   symbol,
			Enabled:  true,
			Strategy: config.StrategySection{Name: config.StrategyMAPullback},
			Entry:    config.EntrySection{MaxSpreadPips: 1.0, Direction: config.DirectionBoth},
			Exit:     config.ExitSection{MaxHoldMinutes: 90},
			Risk:     config.ConfigRiskSection{Quantity: 1000},
		},
	}
	return in, sma
}

func maReadyInput() (EvalInput, float64)     { return maInput("USD_JPY", maPip, 150.00, true) }
func maReadyInputDown() (EvalInput, float64) { return maInput("USD_JPY", maPip, 150.00, false) }

func TestMAPullback_Evaluate_HappyBuy(t *testing.T) {
	in, _ := maReadyInput()
	sig := MAPullback{}.Evaluate(in)
	if sig.Decision != DecisionEnter {
		t.Fatalf("expected ENTER, got %s (reason=%s)", sig.Decision, sig.Reason)
	}
	if sig.Side != order.SideBuy {
		t.Fatalf("expected BUY (uptrend), got %s", sig.Side)
	}
	// Exact computed exits for this fixture: entry≈MA → SL floored to the min; only
	// the planted swing LOW exists (no swing HIGH ahead) → TP falls back to the cap
	// (which is now the daytrade-runner ceiling, not a scalp cap).
	if !approxF(sig.StopLossPips, maPBSLMinPips) {
		t.Errorf("SL = %.2f, want %.0f (entry≈MA → floored)", sig.StopLossPips, maPBSLMinPips)
	}
	if !approxF(sig.TakeProfitPips, maPBTPCapPips) {
		t.Errorf("TP = %.2f, want %.0f (no level ahead → cap)", sig.TakeProfitPips, maPBTPCapPips)
	}
	// Daytrade-runner retune: the trailing ratchet is now the PRIMARY exit (was OFF).
	if sig.RatchetArmPips != maPBRatchetArmPips || sig.RatchetGivebackPips != maPBRatchetGivebackPips {
		t.Errorf("ratchet = arm %.1f/give %.1f, want %.1f/%.1f",
			sig.RatchetArmPips, sig.RatchetGivebackPips, maPBRatchetArmPips, maPBRatchetGivebackPips)
	}
	if sig.Quantity != 1000 {
		t.Errorf("quantity = %d, want 1000", sig.Quantity)
	}
	// Daytrade-runner retune: a daytrade time stop now recycles a stalled position
	// (was 0 = no deadline, which left dead naked carries). Extension/early-exit stay
	// OFF — the trailing ratchet + far TP ceiling + structural SL + time stop are the
	// only exits.
	if sig.MaxHoldMinutes != maPBMaxHoldMinutes {
		t.Errorf("max_hold = %d, want %d (daytrade time stop)", sig.MaxHoldMinutes, maPBMaxHoldMinutes)
	}
	if sig.ExtensionMaxMinutes != 0 || sig.EarlyExitWindowMinutes != 0 || sig.EarlyExitTargetPips != 0 {
		t.Errorf("extension/early-exit must be OFF, got ext=%d ewin=%d etgt=%.1f",
			sig.ExtensionMaxMinutes, sig.EarlyExitWindowMinutes, sig.EarlyExitTargetPips)
	}
}

// Daytrade-runner retune (replaces the old scalp-shaped "20-pip cap / no trailing /
// no time stop"): the strategy now emits a trailing ratchet as the PRIMARY runner
// exit, a daytrade time stop, and a far TP ceiling. The ratchet must satisfy
// arm>giveback>0, and — critically for coherence — the TP ceiling must sit ABOVE the
// ratchet arm, else the broker OCO TP would fire before the trail ever engages and
// re-create the scalp exit we are removing.
func TestMAPullback_Evaluate_RunnerExitsEmitted(t *testing.T) {
	in, _ := maReadyInput()
	sig := MAPullback{}.Evaluate(in)
	if sig.Decision != DecisionEnter {
		t.Fatalf("expected ENTER, got %s (reason=%s)", sig.Decision, sig.Reason)
	}
	if sig.RatchetArmPips != maPBRatchetArmPips || sig.RatchetGivebackPips != maPBRatchetGivebackPips {
		t.Errorf("ratchet = arm %.1f/give %.1f, want %.1f/%.1f",
			sig.RatchetArmPips, sig.RatchetGivebackPips, maPBRatchetArmPips, maPBRatchetGivebackPips)
	}
	if !(maPBRatchetArmPips > maPBRatchetGivebackPips && maPBRatchetGivebackPips > 0) {
		t.Errorf("ratchet must satisfy arm>giveback>0, got arm %.1f give %.1f", maPBRatchetArmPips, maPBRatchetGivebackPips)
	}
	if sig.MaxHoldMinutes != maPBMaxHoldMinutes {
		t.Errorf("max_hold = %d, want %d (daytrade time stop)", sig.MaxHoldMinutes, maPBMaxHoldMinutes)
	}
	if sig.TakeProfitPips <= maPBRatchetArmPips {
		t.Errorf("TP ceiling %.1f must exceed ratchet arm %.1f so the trail can engage",
			sig.TakeProfitPips, maPBRatchetArmPips)
	}
	// Runner SL floor is wide enough that 5m noise doesn't pre-stop a young leg.
	if maPBSLMinPips < 8 {
		t.Errorf("runner SL floor %.1f too tight (want >= 8)", maPBSLMinPips)
	}
}

func TestMAPullback_Evaluate_HappySell(t *testing.T) {
	in, _ := maReadyInputDown()
	sig := MAPullback{}.Evaluate(in)
	if sig.Decision != DecisionEnter || sig.Side != order.SideSell {
		t.Fatalf("expected ENTER/SELL (downtrend), got %s/%s (reason=%s)", sig.Decision, sig.Side, sig.Reason)
	}
	if !approxF(sig.StopLossPips, maPBSLMinPips) {
		t.Errorf("SL = %.2f, want %.0f", sig.StopLossPips, maPBSLMinPips)
	}
	if !approxF(sig.TakeProfitPips, maPBTPCapPips) {
		t.Errorf("TP = %.2f, want %.0f", sig.TakeProfitPips, maPBTPCapPips)
	}
}

func TestMAPullback_Evaluate_HappyBuy_EURUSD(t *testing.T) {
	// Same geometry on a USD-quote pair (pip=0.0001): proves pip-scaling holds
	// end-to-end and yields the SAME pip values (SL floor 8 / TP cap 80).
	in, _ := maInput("EUR_USD", 0.0001, 1.0800, true)
	sig := MAPullback{}.Evaluate(in)
	if sig.Decision != DecisionEnter || sig.Side != order.SideBuy {
		t.Fatalf("EUR_USD: expected ENTER/BUY, got %s/%s (reason=%s)", sig.Decision, sig.Side, sig.Reason)
	}
	if !approxF(sig.StopLossPips, maPBSLMinPips) {
		t.Errorf("EUR_USD SL = %.2f, want %.0f", sig.StopLossPips, maPBSLMinPips)
	}
	if !approxF(sig.TakeProfitPips, maPBTPCapPips) {
		t.Errorf("EUR_USD TP = %.2f, want %.0f", sig.TakeProfitPips, maPBTPCapPips)
	}
}

func TestMAPullback_Evaluate_HappySell_EURUSD(t *testing.T) {
	in, _ := maInput("EUR_USD", 0.0001, 1.0800, false)
	sig := MAPullback{}.Evaluate(in)
	if sig.Decision != DecisionEnter || sig.Side != order.SideSell {
		t.Fatalf("EUR_USD down: expected ENTER/SELL, got %s/%s (reason=%s)", sig.Decision, sig.Side, sig.Reason)
	}
}

func TestMAPullback_Evaluate_DirectionBlocked(t *testing.T) {
	in, _ := maReadyInput() // uptrend → wants BUY
	in.Config.Entry.Direction = config.DirectionSellOnly
	if r := (MAPullback{}).Evaluate(in).Reason; r != "direction_blocked" {
		t.Errorf("up + sell_only: expected direction_blocked, got %q", r)
	}
}

func TestMAPullback_Evaluate_Insufficient5mCandles(t *testing.T) {
	in, _ := maReadyInput()
	in.Candles5m = in.Candles5m[:maPBMin5mBars-1]
	if r := (MAPullback{}).Evaluate(in).Reason; r != "insufficient_5m_candles" {
		t.Errorf("short 5m: expected insufficient_5m_candles, got %q", r)
	}
}

func TestMAPullback_Evaluate_NoTrendIsNone(t *testing.T) {
	in, _ := maReadyInput() // 5m still ramps UP, but the trend is read off the 1h.
	// Flatten the 1h series → no 1h 200SMA slope → no_trend (even though the 5m trends
	// up, proving the trend source is the 1h, NOT the 5m).
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	flat := make([]market.Candle, len(in.Candles1h))
	for i := range flat {
		flat[i] = market.Candle{Symbol: "USD_JPY", Interval: time.Hour,
			OpenTime: t0.Add(time.Duration(i) * time.Hour),
			Open:     150.00, High: 150.00, Low: 150.00, Close: 150.00}
	}
	in.Candles1h = flat
	if r := (MAPullback{}).Evaluate(in).Reason; r != "no_trend" {
		t.Errorf("flat 1h (trending 5m): expected no_trend, got %q", r)
	}
}

func TestMAPullback_Evaluate_Insufficient1hCandles(t *testing.T) {
	in, _ := maReadyInput()
	in.Candles1h = in.Candles1h[:maPB1hMinBars-1]
	if r := (MAPullback{}).Evaluate(in).Reason; r != "insufficient_1h_candles" {
		t.Errorf("short 1h: expected insufficient_1h_candles, got %q", r)
	}
}

func TestMAPullback_Evaluate_OutOfZoneIsNone(t *testing.T) {
	in, sma5 := maReadyInput()
	mid := sma5 + 1.0 // 100 pips above the MA — no pullback
	in.Summary.CurrentRate = market.CurrentRate{Bid: mid - 0.005, Ask: mid + 0.005, SpreadPips: 1.0}
	if r := (MAPullback{}).Evaluate(in).Reason; r != "not_in_ma_zone" {
		t.Errorf("far above MA: expected not_in_ma_zone, got %q", r)
	}
}

func TestMAPullback_Evaluate_OutOfZone_EURUSD(t *testing.T) {
	in, sma5 := maInput("EUR_USD", 0.0001, 1.0800, true)
	mid := sma5 + 100*0.0001 // 100 pips above (pip-scaled)
	in.Summary.CurrentRate = market.CurrentRate{Bid: mid - 0.00005, Ask: mid + 0.00005, SpreadPips: 1.0}
	if r := (MAPullback{}).Evaluate(in).Reason; r != "not_in_ma_zone" {
		t.Errorf("EUR_USD far above MA: expected not_in_ma_zone, got %q", r)
	}
}

func TestPriceOnTrendSide(t *testing.T) {
	sma := 150.50
	// up: price must be on/above the trend SMA; below it = MA broken = wrong side.
	if !priceOnTrendSide(150.55, sma, "up") || !priceOnTrendSide(sma, sma, "up") {
		t.Fatal("up: price at/above SMA should be on the trend side")
	}
	if priceOnTrendSide(150.45, sma, "up") {
		t.Fatal("up: price below SMA should NOT be on the trend side")
	}
	// down: price must be on/below the trend SMA.
	if !priceOnTrendSide(150.45, sma, "down") || !priceOnTrendSide(sma, sma, "down") {
		t.Fatal("down: price at/below SMA should be on the trend side")
	}
	if priceOnTrendSide(150.55, sma, "down") {
		t.Fatal("down: price above SMA should NOT be on the trend side")
	}
	if priceOnTrendSide(150.55, sma, "flat") {
		t.Fatal("flat: no trend side")
	}
}

// In an uptrend a pullback that has slipped BELOW the 200SMA (still inside the
// ATR zone) is the wrong side of the MA — the reference method buys only while price holds
// above the rising MA. Evaluate must block it with reason wrong_side_of_ma.
func TestMAPullback_Evaluate_WrongSideOfMA(t *testing.T) {
	in, sma := maReadyInput() // uptrend → wants BUY, price normally AT the MA
	mid := sma - 1.0*maPip    // 1 pip below the SMA: inside the zone but below it
	in.Summary.CurrentRate = market.CurrentRate{Bid: mid - 0.5*maPip, Ask: mid + 0.5*maPip, SpreadPips: 1.0}
	if r := (MAPullback{}).Evaluate(in).Reason; r != "wrong_side_of_ma" {
		t.Errorf("price below MA in uptrend: expected wrong_side_of_ma, got %q", r)
	}
}

func TestMAPullback_Evaluate_NoConfluenceIsNone(t *testing.T) {
	in, _ := maReadyInput()
	in.Candles5m[230].Low = in.Candles5m[230].Close - maPip // remove the planted swing low
	if r := (MAPullback{}).Evaluate(in).Reason; r != "no_confluence" {
		t.Errorf("no nearby swing: expected no_confluence, got %q", r)
	}
}

func TestMAPullback_Evaluate_NoConfluence_Down(t *testing.T) {
	in, _ := maReadyInputDown()
	in.Candles5m[230].High = in.Candles5m[230].Close + maPip // remove the planted swing high
	if r := (MAPullback{}).Evaluate(in).Reason; r != "no_confluence" {
		t.Errorf("down no_confluence: got %q", r)
	}
}

func TestMAPullback_Evaluate_NoReboundIsNone(t *testing.T) {
	in, _ := maReadyInput()
	last := len(in.Candles5m) - 1
	in.Candles5m[last].Open = in.Candles5m[last].Close + maPip // bearish in an uptrend
	if r := (MAPullback{}).Evaluate(in).Reason; r != "no_rebound" {
		t.Errorf("bearish last bar in uptrend: expected no_rebound, got %q", r)
	}
}

func TestMAPullback_Evaluate_NoRebound_Down(t *testing.T) {
	in, _ := maReadyInputDown()
	last := len(in.Candles5m) - 1
	in.Candles5m[last].Open = in.Candles5m[last].Close - maPip // bullish in a downtrend
	if r := (MAPullback{}).Evaluate(in).Reason; r != "no_rebound" {
		t.Errorf("bullish last bar in downtrend: expected no_rebound, got %q", r)
	}
}

func TestMAPullback_Evaluate_SpreadGuard(t *testing.T) {
	in, _ := maReadyInput()
	in.Summary.CurrentRate.SpreadPips = 2.0 // > max 1.0
	if r := (MAPullback{}).Evaluate(in).Reason; r != "spread_too_wide" {
		t.Errorf("expected spread_too_wide, got %q", r)
	}
}

func TestMAPullback_Evaluate_NoPrice(t *testing.T) {
	in, _ := maReadyInput()
	in.Summary.CurrentRate = market.CurrentRate{Bid: 0, Ask: 0, SpreadPips: 0}
	if r := (MAPullback{}).Evaluate(in).Reason; r != "no_price" {
		t.Errorf("zero bid/ask: expected no_price, got %q", r)
	}
}

func TestMAPullback_Name(t *testing.T) {
	if (MAPullback{}).Name() != config.StrategyMAPullback {
		t.Fatalf("Name() = %q, want ma_pullback", (MAPullback{}).Name())
	}
}
