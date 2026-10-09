package backtest

import (
	"math"
	"testing"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// ma_pullback's PRIMARY exit is the trailing ratchet (arm16/give8); a backtest
// engine that ignores it cannot validate the strategy offline at all. This pins
// a pessimistic,
// bar-resolution ratchet: peak is taken from the favorable excursion of PRIOR
// bars (this bar's high can't inflate the peak before the retrace is checked),
// and an armed position closes when price gives back `give` pips from peak.
func TestEvaluateExit_RatchetTrailingClose(t *testing.T) {
	pip := 0.01
	costs := CostModel{quoteJPYRate: 1.0}
	p := &openPosition{
		side: order.SideBuy, entryPrice: 100.00,
		tpPips: 80, slPips: 20, // far structural backstops
		armPips: 16, givePips: 8,
		openedAt: time.Unix(0, 0),
	}
	t0 := time.Unix(60, 0)

	// bar1: rises to +20 pips (High 100.20). Arms (peak 20 ≥ 16). No close yet.
	bar1 := market.Candle{High: 100.20, Low: 100.05, Close: 100.18}
	if tr, _ := evaluateExit(p, bar1, t0, pip, PessimisticSLFirst, costs); tr != nil {
		t.Fatalf("bar1 must not close; got %s @ %.3f", tr.CloseReason, tr.ExitPrice)
	}
	if !p.ratchetArmed {
		t.Fatal("must arm after peak +20 >= arm 16")
	}

	// bar2: retraces to +10 pips (Low 100.10). peak 20 − give 8 = ratchet stop +12
	// (price 100.12); Low 100.10 ≤ 100.12 → ratchet close at +12.
	bar2 := market.Candle{High: 100.19, Low: 100.10, Close: 100.11}
	tr2, _ := evaluateExit(p, bar2, t0.Add(time.Minute), pip, PessimisticSLFirst, costs)
	if tr2 == nil || tr2.CloseReason != "ratchet_takeprofit" {
		t.Fatalf("bar2 must ratchet-close; got %+v", tr2)
	}
	if math.Abs(tr2.ProfitLossPips-12) > 0.5 {
		t.Errorf("ratchet exit pips = %.2f, want ~12 (peak20 − give8)", tr2.ProfitLossPips)
	}
}

func TestEvaluateExit_RatchetNotArmedBelowArm(t *testing.T) {
	pip := 0.01
	costs := CostModel{quoteJPYRate: 1.0}
	p := &openPosition{side: order.SideBuy, entryPrice: 100.00, tpPips: 80, slPips: 20, armPips: 16, givePips: 8, openedAt: time.Unix(0, 0)}
	// Peaks at +10 (< arm 16): never arms. A retrace must NOT ratchet-close.
	bar1 := market.Candle{High: 100.10, Low: 100.05, Close: 100.08}
	evaluateExit(p, bar1, time.Unix(60, 0), pip, PessimisticSLFirst, costs)
	if p.ratchetArmed {
		t.Fatal("must NOT arm when peak +10 < arm 16")
	}
	bar2 := market.Candle{High: 100.09, Low: 100.01, Close: 100.02} // retrace, still no ratchet
	if tr, _ := evaluateExit(p, bar2, time.Unix(120, 0), pip, PessimisticSLFirst, costs); tr != nil {
		t.Fatalf("unarmed ratchet must not close; got %s", tr.CloseReason)
	}
}

func TestEvaluateExit_RatchetOffWhenArmZero(t *testing.T) {
	// arm/give 0 = ratchet disabled (back-compat with TP/SL-only configs).
	pip := 0.01
	costs := CostModel{quoteJPYRate: 1.0}
	p := &openPosition{side: order.SideBuy, entryPrice: 100.00, tpPips: 5, slPips: 5, openedAt: time.Unix(0, 0)}
	bar := market.Candle{High: 100.20, Low: 100.18, Close: 100.19} // +20, TP 5 hit
	tr, _ := evaluateExit(p, bar, time.Unix(60, 0), pip, PessimisticSLFirst, costs)
	if tr == nil || tr.CloseReason != "take_profit" {
		t.Fatalf("ratchet off → normal TP exit; got %+v", tr)
	}
}
