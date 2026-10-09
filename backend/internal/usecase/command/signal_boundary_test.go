package command

import (
	"testing"

	"fx-bot/backend/internal/config"
)

// hard_limits must be enforced at the ORDER boundary on the REAL values the
// strategy emits (Signal), not the config placeholders. Strategy-computed
// TP / MaxHold can fall outside the config range (or above the MaxHold cap),
// and validation that only sees the placeholder config never notices. The
// per-trade loss cap is the real capital control as qty scales.

func obLimits() *config.HardLimits {
	return &config.HardLimits{
		Quantity:       config.IntRange{Min: 1000, Max: 100000},
		MaxHoldMinutes: config.IntRange{Min: 30, Max: 600},
		OrderBoundary: &config.OrderBoundary{
			MaxLossPerTradeJPY: 8000,
			MaxStopLossPips:    50,
			MaxTakeProfitPips:  120,
		},
	}
}

// obLimitsV2 mirrors hard_limits with the per-strategy split: the GLOBAL
// order_boundary stays TIGHT (50/120, intraday), and signature_breakout gets a WIDE per-strategy
// override (SL 200 / TP 1500) for daily structural stops/measured-move targets. The per-trade JPY
// loss cap (8000) is unchanged on both = the real capital control. MaxHold cap raised for the runner.
func obLimitsV2() *config.HardLimits {
	return &config.HardLimits{
		Quantity:       config.IntRange{Min: 1000, Max: 100000},
		MaxHoldMinutes: config.IntRange{Min: 30, Max: 43200},
		OrderBoundary: &config.OrderBoundary{ // GLOBAL stays tight (scalp/intraday)
			MaxLossPerTradeJPY: 8000,
			MaxStopLossPips:    50,
			MaxTakeProfitPips:  120,
		},
		StrategyOrderBoundaries: map[string]*config.OrderBoundary{
			"signature_breakout": {MaxLossPerTradeJPY: 8000, MaxStopLossPips: 200, MaxTakeProfitPips: 1500},
		},
	}
}

func TestValidateSignalBoundaries_V2DailyBreakoutPasses(t *testing.T) {
	// advisor v2 (signature_breakout) profile: daily structural SL ~150pips, measured-move TP ~400pips, 30-day hold,
	// qty 3000. Worst loss = 150*0.01*3000 = 4500 JPY ≤ 8000 cap → must pass under the per-strategy cap.
	if err := ValidateSignalBoundaries("signature_breakout", "USD_JPY", 150, 400, 43200, 3000, 1.0, obLimitsV2()); err != nil {
		t.Fatalf("v2 daily breakout (SL150/TP400/qty3000) must pass; got %v", err)
	}
}

func TestValidateSignalBoundaries_V2LossCapIsTheRealControl(t *testing.T) {
	// SL 150pips < 200 sanity cap, but at qty6000 the worst loss = 150*0.01*6000 = 9000 JPY > 8000.
	// The per-trade JPY loss cap (capital control) rejects it even though the pip sanity passes.
	if err := ValidateSignalBoundaries("signature_breakout", "USD_JPY", 150, 400, 43200, 6000, 1.0, obLimitsV2()); err == nil {
		t.Fatal("expected per-trade loss rejection (9000 JPY > 8000) — capital cap is the real control")
	}
}

func TestValidateSignalBoundaries_V2SanityRejectsDecimalTypo(t *testing.T) {
	// A decimal typo (600pip SL) is still caught by the per-strategy sanity cap (600 > 200).
	if err := ValidateSignalBoundaries("signature_breakout", "USD_JPY", 600, 400, 43200, 1000, 1.0, obLimitsV2()); err == nil {
		t.Fatal("expected SL sanity rejection (600 > 200; decimal typo)")
	}
}

func TestValidateSignalBoundaries_GlobalStaysTightForOtherStrategies(t *testing.T) {
	// A non-signature strategy (no per-strategy override) must still hit the TIGHT global cap (50):
	// a 100-pip SL that would be fine for v2 is rejected for ma_pullback. This is the defense-in-depth
	// the per-strategy split restores.
	if err := ValidateSignalBoundaries("ma_pullback", "USD_JPY", 100, 80, 480, 1000, 1.0, obLimitsV2()); err == nil {
		t.Fatal("non-signature strategy must be capped by the tight global SL (100 > 50)")
	}
	// Manual trades (strategyName "") likewise use the tight global cap.
	if err := ValidateSignalBoundaries("", "USD_JPY", 100, 80, 480, 1000, 1.0, obLimitsV2()); err == nil {
		t.Fatal("manual/global path must be capped by the tight global SL (100 > 50)")
	}
}

func TestValidateSignalBoundaries_FrozenRunnerPasses(t *testing.T) {
	// Intraday runner profile: SL 8-20, TP 30-80, MaxHold 480, qty 1000.
	// Must NEVER false-block a legitimate runner trade.
	if err := ValidateSignalBoundaries("momentum_pullback", "USD_JPY", 20, 80, 480, 1000, 1.0, obLimits()); err != nil {
		t.Fatalf("legit JPY-pair runner trade must pass; got %v", err)
	}
	if err := ValidateSignalBoundaries("momentum_pullback", "EUR_USD", 20, 80, 480, 1000, 150.0, obLimits()); err != nil {
		t.Fatalf("legit USD-pair runner trade must pass; got %v", err)
	}
	// Larger lot (qty 10000, SL 20): JPY = 2000 JPY ≤ 8000 — must pass.
	if err := ValidateSignalBoundaries("momentum_pullback", "USD_JPY", 20, 80, 480, 10000, 1.0, obLimits()); err != nil {
		t.Fatalf("qty 10000 must still pass; got %v", err)
	}
}

func TestValidateSignalBoundaries_RejectsPerTradeLossOverCap(t *testing.T) {
	// qty 100000 (100x fat finger), SL 20, JPY pair → 20*0.01*100000 = 20000 JPY > 8000.
	if err := ValidateSignalBoundaries("momentum_pullback", "USD_JPY", 20, 80, 480, 100000, 1.0, obLimits()); err == nil {
		t.Fatal("expected per-trade loss rejection (20000 JPY > 8000 cap)")
	}
}

func TestValidateSignalBoundaries_RejectsMaxHoldOverCap(t *testing.T) {
	if err := ValidateSignalBoundaries("momentum_pullback", "USD_JPY", 20, 80, 780, 1000, 1.0, obLimits()); err == nil {
		t.Fatal("expected max_hold rejection (780 > 600)")
	}
}

func TestValidateSignalBoundaries_RejectsSLSanity(t *testing.T) {
	if err := ValidateSignalBoundaries("momentum_pullback", "USD_JPY", 200, 80, 480, 1000, 1.0, obLimits()); err == nil {
		t.Fatal("expected SL sanity rejection (200 > 50; decimal typo)")
	}
}

func TestValidateSignalBoundaries_RejectsQtyOutOfRange(t *testing.T) {
	if err := ValidateSignalBoundaries("momentum_pullback", "USD_JPY", 20, 80, 480, 500, 1.0, obLimits()); err == nil {
		t.Fatal("expected qty rejection (500 < min 1000)")
	}
}

func TestValidateSignalBoundaries_NilLimitsSkips(t *testing.T) {
	if err := ValidateSignalBoundaries("momentum_pullback", "USD_JPY", 999, 999, 9999, 999999, 1.0, nil); err != nil {
		t.Fatalf("nil limits must skip (back-compat); got %v", err)
	}
}

func TestValidateSignalBoundaries_RateUnavailableSkipsLossCapOnly(t *testing.T) {
	// rate=0 (USD-pair rate fetch failed) → skip the JPY loss cap but still
	// enforce the non-FX bounds. qty 100000 is in [1000,100000] so it passes.
	if err := ValidateSignalBoundaries("momentum_pullback", "EUR_USD", 20, 80, 480, 100000, 0, obLimits()); err != nil {
		t.Fatalf("rate unavailable must skip only the loss cap; got %v", err)
	}
	if err := ValidateSignalBoundaries("momentum_pullback", "EUR_USD", 20, 80, 780, 1000, 0, obLimits()); err == nil {
		t.Fatal("max_hold must still be enforced when rate unavailable")
	}
}
