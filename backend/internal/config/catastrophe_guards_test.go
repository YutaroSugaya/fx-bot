package config

import "testing"

// TestCatastropheGuards_NotSilentlyWeakened pins the blow-up brakes in the REAL
// configs/hard_limits.yaml so a future edit (e.g. loosening the TP floor for more
// aggressive trading) can never quietly RAISE a catastrophe cap. These caps
// are the floor that lets the bot "trade aggressively and lose some" WITHOUT risking
// ruin — the catastrophe defences in CLAUDE.md (残すカタストロフ防御) keep exactly these.
func TestCatastropheGuards_NotSilentlyWeakened(t *testing.T) {
	h, err := LoadHardLimits("../../../configs/hard_limits.yaml")
	if err != nil {
		t.Fatalf("load hard_limits: %v", err)
	}
	if h.OrderBoundary == nil {
		t.Fatal("order_boundary (per-trade blow-up brake) must be set")
	}
	if h.OrderBoundary.MaxLossPerTradeJPY <= 0 || h.OrderBoundary.MaxLossPerTradeJPY > 8000 {
		t.Errorf("global per-trade loss cap must stay in (0, 8000] JPY, got %d", h.OrderBoundary.MaxLossPerTradeJPY)
	}
	if h.OrderBoundary.MaxStopLossPips <= 0 || h.OrderBoundary.MaxStopLossPips > 50 {
		t.Errorf("per-trade SL sanity cap must stay in (0, 50] pips, got %v", h.OrderBoundary.MaxStopLossPips)
	}
	if h.MaxLossInThisWindowJPY.Max <= 0 || h.MaxLossInThisWindowJPY.Max > 5000 {
		t.Errorf("window (daily) loss cap ceiling must stay in (0, 5000] JPY, got %d", h.MaxLossInThisWindowJPY.Max)
	}
	if h.Quantity.Max > 100000 {
		t.Errorf("quantity ceiling must stay <= 100000 units, got %d", h.Quantity.Max)
	}
}
