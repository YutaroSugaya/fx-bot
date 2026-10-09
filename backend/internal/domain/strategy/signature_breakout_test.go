package strategy

import (
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// buildSeries makes daily candles from a close series. bullish bars: small up body;
// bearish bars: small down body. High/Low wrap the body by a fixed sleeve.
func buildSeries(closes []float64, bullish bool) []market.Candle {
	out := make([]market.Candle, len(closes))
	for i, c := range closes {
		var o, h, l float64
		if bullish {
			o = c - 0.10
			h = c + 0.05
			l = o - 0.05
		} else {
			o = c + 0.10
			h = o + 0.05
			l = c - 0.05
		}
		out[i] = market.Candle{Open: o, High: h, Low: l, Close: c}
	}
	return out
}

func testParams() SignatureParams {
	return SignatureParams{
		SMAPeriod: 5, SMASlopeLB: 3, MinSlopePips: 20,
		DonchianBars: 5, ATRWindow: 5, DispMultATR: 0.2, SLBufferATR: 1.0, MinRR: 2.0,
	}
}

func risingCloses() []float64 {
	cs := make([]float64, 12)
	for i := range cs {
		cs[i] = 98.00 + float64(i)*0.25 // monotone up -> clear uptrend
	}
	return cs
}

func TestSignatureBreakout_UpBreakoutFound(t *testing.T) {
	daily := buildSeries(risingCloses(), true)
	got := DetectSignatureBreakout(daily, 0.01, testParams())
	if !got.Found {
		t.Fatalf("expected a found breakout, got notFound: %s", got.Reason)
	}
	if got.Label != BreakoutTrendCont {
		t.Errorf("label = %q, want %q", got.Label, BreakoutTrendCont)
	}
	if got.Side != order.SideBuy {
		t.Errorf("side = %q, want BUY", got.Side)
	}
	if got.RR < testParams().MinRR {
		t.Errorf("RR = %.2f, want >= %.2f", got.RR, testParams().MinRR)
	}
	if !(got.Invalidation < got.Entry && got.Entry < got.TargetRef) {
		t.Errorf("geometry off: inval=%.3f entry=%.3f target=%.3f (want inval<entry<target)", got.Invalidation, got.Entry, got.TargetRef)
	}
}

func TestSignatureBreakout_DownBreakoutFound(t *testing.T) {
	cs := make([]float64, 12)
	for i := range cs {
		cs[i] = 102.75 - float64(i)*0.25 // monotone down
	}
	daily := buildSeries(cs, false)
	got := DetectSignatureBreakout(daily, 0.01, testParams())
	if !got.Found {
		t.Fatalf("expected found, got notFound: %s", got.Reason)
	}
	if got.Side != order.SideSell {
		t.Errorf("side = %q, want SELL", got.Side)
	}
	if !(got.TargetRef < got.Entry && got.Entry < got.Invalidation) {
		t.Errorf("geometry off: target=%.3f entry=%.3f inval=%.3f (want target<entry<inval)", got.TargetRef, got.Entry, got.Invalidation)
	}
}

func TestSignatureBreakout_FlatNoTrade(t *testing.T) {
	cs := make([]float64, 12)
	for i := range cs {
		cs[i] = 100.00 // flat -> no trend
	}
	got := DetectSignatureBreakout(buildSeries(cs, true), 0.01, testParams())
	if got.Found {
		t.Fatalf("flat market should not produce a setup, got %+v", got)
	}
	if got.Reason != "no_trend" {
		t.Errorf("reason = %q, want no_trend", got.Reason)
	}
}

func TestSignatureBreakout_RRTooLow(t *testing.T) {
	p := testParams()
	p.MinRR = 5.0 // raise the bar above the ~2.5 this setup yields -> reject
	got := DetectSignatureBreakout(buildSeries(risingCloses(), true), 0.01, p)
	if got.Found {
		t.Fatalf("RR should be below 5.0 and rejected, got RR=%.2f", got.RR)
	}
	if got.Label != BreakoutNoTrade {
		t.Errorf("label = %q, want no_trade", got.Label)
	}
}

func TestSignatureBreakout_InsufficientHistory(t *testing.T) {
	got := DetectSignatureBreakout(buildSeries([]float64{99, 99.5, 100, 100.5}, true), 0.01, testParams())
	if got.Found || got.Reason != "insufficient_history" {
		t.Errorf("want notFound insufficient_history, got %+v", got)
	}
}

func TestCompletedDailyBars_DropsFormingBar(t *testing.T) {
	now := time.Date(2026, 6, 16, 8, 0, 0, 0, time.UTC)
	bars := []market.Candle{
		{OpenTime: now.Add(-72 * time.Hour), Close: 100},
		{OpenTime: now.Add(-48 * time.Hour), Close: 101},
		{OpenTime: now.Add(-26 * time.Hour), Close: 102}, // completed (>24h)
		{OpenTime: now.Add(-3 * time.Hour), Close: 103},  // forming (<24h) -> drop
	}
	got := CompletedDailyBars(bars, now)
	if len(got) != 3 {
		t.Fatalf("want 3 (forming dropped), got %d", len(got))
	}
	if got[len(got)-1].Close != 102 {
		t.Errorf("last kept bar should be the completed one (102), got %.0f", got[len(got)-1].Close)
	}
}

func TestCompletedDailyBars_KeepsWhenLastCompleted(t *testing.T) {
	now := time.Date(2026, 6, 16, 8, 0, 0, 0, time.UTC)
	bars := []market.Candle{
		{OpenTime: now.Add(-48 * time.Hour), Close: 101},
		{OpenTime: now.Add(-25 * time.Hour), Close: 102}, // completed; no forming bar present
	}
	if got := CompletedDailyBars(bars, now); len(got) != 2 {
		t.Errorf("no forming bar -> keep all, got %d", len(got))
	}
}

func TestDescribeSignatureState_Uptrend(t *testing.T) {
	daily := buildSeries(risingCloses(), true)
	cur := daily[len(daily)-1].Close - 0.05 // just below the latest close
	st := DescribeSignatureState(daily, cur, 0.01, testParams())
	if st.Reason != "ok" {
		t.Fatalf("reason = %q, want ok", st.Reason)
	}
	if st.Trend != "up" {
		t.Errorf("trend = %q, want up", st.Trend)
	}
	if st.BuyTrigger <= 0 || st.ATRPips <= 0 {
		t.Errorf("missing geometry: %+v", st)
	}
	// dist to the buy trigger should equal (buyTrigger - current)/pip
	want := (st.BuyTrigger - cur) / 0.01
	if d := st.DistToTrigPips - want; d > 0.1 || d < -0.1 {
		t.Errorf("DistToTrigPips = %.1f, want ~%.1f", st.DistToTrigPips, want)
	}
	// NextStep is the human firing-condition sentence surfaced on the UI; must be populated.
	if st.NextStep == "" {
		t.Error("NextStep must be populated for the dashboard (what must happen next to enter)")
	}
}

func TestDescribeSignatureState_FlatNextStep(t *testing.T) {
	cs := make([]float64, 12)
	for i := range cs {
		cs[i] = 100.00 // flat -> no trend
	}
	st := DescribeSignatureState(buildSeries(cs, true), 100.00, 0.01, testParams())
	if st.Trend != "flat(no_trade)" {
		t.Fatalf("trend = %q, want flat", st.Trend)
	}
	if st.NextStep == "" {
		t.Error("flat state must still explain what's needed (trend to appear)")
	}
}

func TestDescribeSignatureState_InsufficientHistory(t *testing.T) {
	st := DescribeSignatureState(buildSeries([]float64{99, 100, 101}, true), 100, 0.01, testParams())
	if st.Reason != "insufficient_history" {
		t.Errorf("reason = %q, want insufficient_history", st.Reason)
	}
}

func stepsByKey(st SignatureState) map[string]SignatureStep {
	m := map[string]SignatureStep{}
	for _, s := range st.Steps {
		m[s.Key] = s
	}
	return m
}

// Checklist: uptrend + price ALREADY past the buy trigger = armed, now waiting for the daily close.
func TestDescribeSignatureState_StepsArmed(t *testing.T) {
	daily := buildSeries(risingCloses(), true)
	st := DescribeSignatureState(daily, daily[len(daily)-1].High+0.20, 0.01, testParams())
	if st.Reason != "ok" {
		t.Fatalf("reason = %q, want ok", st.Reason)
	}
	if !st.Armed {
		t.Errorf("Armed = false, want true (uptrend + price past trigger)")
	}
	if st.BrokePips <= 0 {
		t.Errorf("BrokePips = %.1f, want > 0 when price is past the trigger", st.BrokePips)
	}
	if len(st.Steps) < 3 {
		t.Fatalf("want >=3 checklist steps, got %d", len(st.Steps))
	}
	by := stepsByKey(st)
	if by["trend"].Status != "done" {
		t.Errorf("trend step = %q, want done", by["trend"].Status)
	}
	if by["trigger"].Status != "done" {
		t.Errorf("trigger step = %q, want done", by["trigger"].Status)
	}
	if by["confirm"].Status != "active" {
		t.Errorf("confirm step = %q, want active (now waiting for the daily close)", by["confirm"].Status)
	}
	for _, s := range st.Steps {
		if s.Label == "" || s.Detail == "" {
			t.Errorf("step %q missing label/detail: %+v", s.Key, s)
		}
	}
	// thresholds surfaced so the UI doesn't hardcode them
	if st.MinSlopePips != testParams().MinSlopePips {
		t.Errorf("MinSlopePips = %.0f, want %.0f", st.MinSlopePips, testParams().MinSlopePips)
	}
	if st.MinRR != testParams().MinRR {
		t.Errorf("MinRR = %.1f, want %.1f", st.MinRR, testParams().MinRR)
	}
	if wantBody := testParams().DispMultATR * st.ATRPips; st.BodyNeedPips-wantBody > 0.1 || st.BodyNeedPips-wantBody < -0.1 {
		t.Errorf("BodyNeedPips = %.1f, want ~%.1f", st.BodyNeedPips, wantBody)
	}
}

// Checklist: uptrend but price BELOW the trigger = trend done, trigger is the active wait, not armed.
func TestDescribeSignatureState_StepsNotBroke(t *testing.T) {
	daily := buildSeries(risingCloses(), true)
	st := DescribeSignatureState(daily, daily[len(daily)-1].Close-1.00, 0.01, testParams())
	if st.Armed {
		t.Errorf("Armed = true, want false (price not past trigger)")
	}
	if st.BrokePips != 0 {
		t.Errorf("BrokePips = %.1f, want 0 when not broken", st.BrokePips)
	}
	by := stepsByKey(st)
	if by["trend"].Status != "done" {
		t.Errorf("trend step = %q, want done", by["trend"].Status)
	}
	if by["trigger"].Status != "active" {
		t.Errorf("trigger step = %q, want active (the current wait)", by["trigger"].Status)
	}
	if by["confirm"].Status != "todo" {
		t.Errorf("confirm step = %q, want todo", by["confirm"].Status)
	}
}

// Checklist: flat = the trend step is the active wait, downstream steps are todo, never armed.
func TestDescribeSignatureState_StepsFlat(t *testing.T) {
	cs := make([]float64, 12)
	for i := range cs {
		cs[i] = 100.00
	}
	st := DescribeSignatureState(buildSeries(cs, true), 100.00, 0.01, testParams())
	if st.Armed {
		t.Errorf("Armed = true, want false (flat)")
	}
	by := stepsByKey(st)
	if by["trend"].Status != "active" {
		t.Errorf("trend step = %q, want active (waiting for a trend)", by["trend"].Status)
	}
	if by["trigger"].Status != "todo" {
		t.Errorf("trigger step = %q, want todo when flat", by["trigger"].Status)
	}
	if st.MinSlopePips != testParams().MinSlopePips {
		t.Errorf("MinSlopePips = %.0f, want %.0f", st.MinSlopePips, testParams().MinSlopePips)
	}
}

func TestBuildSignatureSignal_TrendBuy(t *testing.T) {
	name := config.StrategyName("signature_breakout")
	sig := BuildSignatureSignal(order.SideBuy, 150.25, 149.45, 152.25, 80, 0.01, BreakoutTrendCont, 1000, name, time.Now())
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("want entry buy, got %+v", sig)
	}
	if d := sig.StopLossPips - 80; d > 0.1 || d < -0.1 { // (150.25-149.45)/0.01 ≈ 80
		t.Errorf("SL = %.4f, want ~80 (structural)", sig.StopLossPips)
	}
	if d := sig.TakeProfitPips - 200; d > 0.1 || d < -0.1 { // (152.25-150.25)/0.01 = 200 (measured-move target)
		t.Errorf("TP = %.4f, want ~200 (measured-move target, broker-side OCO)", sig.TakeProfitPips)
	}
	if sig.RatchetArmPips != tfRatchetArmMult*80 || sig.RatchetGivebackPips != tfRatchetGiveMult*80 {
		t.Errorf("ratchet mismatch: arm=%.0f give=%.0f", sig.RatchetArmPips, sig.RatchetGivebackPips)
	}
	if sig.Quantity != 1000 {
		t.Errorf("qty = %d, want 1000", sig.Quantity)
	}
}

func TestBuildSignatureSignal_TrendSell(t *testing.T) {
	sig := BuildSignatureSignal(order.SideSell, 1.2500, 1.2560, 1.2380, 60, 0.0001, BreakoutTrendCont, 1000, "signature_breakout", time.Now())
	if !sig.IsEntry() || sig.Side != order.SideSell {
		t.Fatalf("want entry sell, got %+v", sig)
	}
	if d := sig.StopLossPips - 60; d > 0.1 || d < -0.1 { // (1.2560-1.2500)/0.0001 ≈ 60
		t.Errorf("SL = %.4f, want ~60", sig.StopLossPips)
	}
	if d := sig.TakeProfitPips - 120; d > 0.1 || d < -0.1 { // (1.2500-1.2380)/0.0001 = 120 (measured-move target)
		t.Errorf("TP = %.4f, want ~120 (measured-move target, broker-side OCO)", sig.TakeProfitPips)
	}
}

func TestBuildSignatureSignal_CountertrendDeferred(t *testing.T) {
	sig := BuildSignatureSignal(order.SideBuy, 150, 149, 152, 50, 0.01, BreakoutCountertrend, 1000, "signature_breakout", time.Now())
	if sig.IsEntry() {
		t.Errorf("countertrend is deferred in v2; must be NONE, got %+v", sig)
	}
}

func TestBuildSignatureSignal_BadInvalidation(t *testing.T) {
	// BUY but invalidation above entry -> negative SL -> NONE
	sig := BuildSignatureSignal(order.SideBuy, 150.00, 150.50, 152.00, 50, 0.01, BreakoutTrendCont, 1000, "signature_breakout", time.Now())
	if sig.IsEntry() {
		t.Errorf("bad invalidation must be NONE, got %+v", sig)
	}
}

func TestBuildSignatureSignal_PastTargetNone(t *testing.T) {
	// Live entry has chased to/above the measured-move target -> no reward room -> NONE
	// (the cycle's chase guard normally catches this earlier; this is the structural backstop).
	sig := BuildSignatureSignal(order.SideBuy, 152.30, 149.45, 152.25, 80, 0.01, BreakoutTrendCont, 1000, "signature_breakout", time.Now())
	if sig.IsEntry() {
		t.Errorf("entry at/above target must be NONE, got %+v", sig)
	}
}

func TestSignatureBreakout_NoChaseWeakBody(t *testing.T) {
	// Strong prior uptrend but the breakout bar closes only a hair above the level with a
	// doji-ish body -> weak/unconfirmed breakout should be rejected (not a conviction close).
	cs := risingCloses()
	daily := buildSeries(cs, true)
	// overwrite the breakout bar with a tiny body that still closes just above the window high
	last := len(daily) - 1
	daily[last] = market.Candle{Open: 100.74, Close: 100.745, High: 100.80, Low: 100.30}
	got := DetectSignatureBreakout(daily, 0.01, testParams())
	if got.Found {
		t.Errorf("weak-body breakout should be rejected, got %+v", got)
	}
}
