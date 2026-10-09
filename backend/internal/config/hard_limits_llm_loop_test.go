package config

import "testing"

// An LLM-loop playbook (runtime/playbook_*.jsonl, not tracked) may prescribe geometries up to
// TP40/SL20 (e.g. pullback TP30/SL15, breakout TP40/SL20, range TP20/SL15).
//
// build_market_summary advertises hard_limits.TakeProfitPips/StopLossPips to the LLM as the range it
// may choose within. A scalp-tier ceiling (TP≤20 / SL≤15) would
// CONTRADICT the playbook: the LLM would be told "TP max 20" while its own playbook prescribes
// TP40 for strategy B, so it could never confidently emit strategy A/B. The advertised range MUST
// cover the playbook's widest geometry (B: TP40/SL20) so the LLM is free to follow the playbook.
//
// This is the *advertised* range only. The per-trade catastrophe rail is order_boundary, guarded
// separately by TestHardLimitsYAML_OrderBoundaryRailIntact below — widening guidance must not loosen it.
func TestHardLimitsYAML_AdvertisedRangeCoversPlaybookGeometry(t *testing.T) {
	h, err := LoadHardLimits("../../../configs/hard_limits.yaml")
	if err != nil {
		t.Fatalf("load hard_limits.yaml: %v", err)
	}
	const playbookMaxTP, playbookMaxSL = 40.0, 20.0 // strategy B is the widest
	if h.TakeProfitPips.Max < playbookMaxTP {
		t.Errorf("advertised take_profit_pips.max = %.1f, must be >= %.1f so the LLM can emit playbook strategy B (TP40)",
			h.TakeProfitPips.Max, playbookMaxTP)
	}
	if h.StopLossPips.Max < playbookMaxSL {
		t.Errorf("advertised stop_loss_pips.max = %.1f, must be >= %.1f so the LLM can emit playbook strategy B (SL20)",
			h.StopLossPips.Max, playbookMaxSL)
	}
}

// Guard: widening the advertised range must NOT touch the catastrophe rail. order_boundary is the gate
// the LLM signal actually hits (ValidateSignalBoundaries → OrderBoundaryFor); the SL sanity cap (≤50)
// and per-trade JPY loss cap (≤¥8000 global) are what truly bound worst-case loss as qty scales.
// If a future edit loosens these, this fails — an early warning on a live-money safety net.
func TestHardLimitsYAML_OrderBoundaryRailIntact(t *testing.T) {
	h, err := LoadHardLimits("../../../configs/hard_limits.yaml")
	if err != nil {
		t.Fatalf("load hard_limits.yaml: %v", err)
	}
	ob := h.OrderBoundaryFor("") // global rail (no per-strategy override)
	if ob == nil {
		t.Fatal("global order_boundary must be set (the per-trade catastrophe rail)")
	}
	if ob.MaxLossPerTradeJPY > 8000 {
		t.Errorf("global per-trade loss cap = %d JPY, must stay <= 8000 (catastrophe rail must not loosen)", ob.MaxLossPerTradeJPY)
	}
	if ob.MaxStopLossPips > 50 {
		t.Errorf("global SL sanity cap = %.1f, must stay <= 50 pips", ob.MaxStopLossPips)
	}
}
