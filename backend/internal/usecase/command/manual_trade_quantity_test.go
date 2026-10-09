package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// ManualTradeCommand must reject quantities that exceed
// HardLimits.Quantity.Max.
// Previously the command only defaulted Quantity≤0 to safety.DefaultQuantity
// and forwarded any positive value to the broker untouched — operator could
// fat-finger 10000 and the order would go through.

func makeHardLimits(qtyMin, qtyMax int) *config.HardLimits {
	return &config.HardLimits{
		AllowedSymbols:         []string{"USD_JPY"},
		Quantity:               config.IntRange{Min: qtyMin, Max: qtyMax},
		TakeProfitPips:         config.FloatRange{Min: 15, Max: 50},
		StopLossPips:           config.FloatRange{Min: 15, Max: 30},
		MaxHoldMinutes:         config.IntRange{Min: 60, Max: 360},
		MaxTradesInThisWindow:  config.IntRange{Min: 0, Max: 3},
		MaxLossInThisWindowJPY: config.IntRange{Min: 0, Max: 5000},
		MaxSpreadPips:          config.FloatRange{Min: 0.3, Max: 1.0},
		ConfigTTLMinutes:       config.IntRange{Min: 60, Max: 120},
	}
}

func TestManualTradeCommand_QuantityExceedsHardLimit_RejectsWithoutBrokerCall(t *testing.T) {
	br := &fakeBroker{
		ticker:           &market.Ticker{Bid: 100.00, Ask: 100.01},
		placeOrderResult: &order.Order{OrderID: "should-not-be-called", Price: 100.01, Status: "FILLED"},
	}
	cmd := makeManualCmd(t, br, config.ModePaperConfig, tempFlag(t))
	cmd.HardLimits = makeHardLimits(100, 100) // cap = 100

	_, err := cmd.Execute(context.Background(), ManualTradeInput{
		Side:           order.SideBuy,
		TakeProfitPips: 20,
		StopLossPips:   15,
		MaxHoldMinutes: 240,
		Quantity:       999, // > cap
	})
	if err == nil {
		t.Fatal("expected reject; quantity 999 > cap 100")
	}
	if !errors.Is(err, ErrQuantityOutOfRange) && !strings.Contains(err.Error(), "quantity") {
		t.Errorf("expected ErrQuantityOutOfRange-style error; got %v", err)
	}
	if br.placeOrderCalls != 0 {
		t.Errorf("broker should NOT be called when quantity rejected; got %d calls", br.placeOrderCalls)
	}
}

func TestManualTradeCommand_QuantityBelowMin_Rejects(t *testing.T) {
	br := &fakeBroker{
		ticker: &market.Ticker{Bid: 100.00, Ask: 100.01},
	}
	cmd := makeManualCmd(t, br, config.ModePaperConfig, tempFlag(t))
	cmd.HardLimits = makeHardLimits(50, 200)

	_, err := cmd.Execute(context.Background(), ManualTradeInput{
		Side:           order.SideBuy,
		TakeProfitPips: 20,
		StopLossPips:   15,
		MaxHoldMinutes: 240,
		Quantity:       30, // < min 50
	})
	if err == nil {
		t.Fatal("expected reject; quantity 30 < min 50")
	}
	if br.placeOrderCalls != 0 {
		t.Errorf("broker should NOT be called when quantity rejected; got %d calls", br.placeOrderCalls)
	}
}

func TestManualTradeCommand_QuantityZero_DefaultsToHardLimitMin(t *testing.T) {
	// When the operator passes Quantity=0 (no
	// preference), the command must default to the SMALLEST safe size
	// (HardLimits.Quantity.Min, e.g. 1000), NOT the cap (Max, e.g. 100000).
	// The old behaviour defaulted a fat-fingered omission to the maximum
	// approved size — a 100x risk amplifier with no second wall (the old
	// LIVE_MAX_QUANTITY env guard was removed). Min is fail-safe: an operator
	// who wants more must say so explicitly.
	br := &fakeBroker{
		ticker:           &market.Ticker{Bid: 100.00, Ask: 100.01},
		placeOrderResult: &order.Order{OrderID: "ord-1", Price: 100.01, Status: "FILLED"},
	}
	cmd := makeManualCmd(t, br, config.ModePaperConfig, tempFlag(t))
	cmd.HardLimits = makeHardLimits(100, 200)

	out, err := cmd.Execute(context.Background(), ManualTradeInput{
		Side: order.SideBuy, TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
		Quantity: 0, // → default
	})
	if err != nil {
		t.Fatalf("expected pass with default qty; got %v", err)
	}
	if out.Quantity != cmd.HardLimits.Quantity.Min {
		t.Errorf("default qty = %d; want min %d (safest default, not max)", out.Quantity, cmd.HardLimits.Quantity.Min)
	}
}

func TestManualTradeCommand_NoHardLimits_FallsBackToOldBehavior(t *testing.T) {
	// Regression: existing test scenarios (HardLimits=nil) must still work
	// for backwards compat; the only change is "if HardLimits set, validate".
	br := &fakeBroker{
		ticker:           &market.Ticker{Bid: 100.00, Ask: 100.01},
		placeOrderResult: &order.Order{OrderID: "ord-1", Price: 100.01, Status: "FILLED"},
	}
	cmd := makeManualCmd(t, br, config.ModePaperConfig, tempFlag(t))
	// HardLimits left nil

	_, err := cmd.Execute(context.Background(), ManualTradeInput{
		Side: order.SideBuy, TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240, Quantity: 10000,
	})
	if err != nil {
		t.Fatalf("nil HardLimits should not gate quantity; got %v", err)
	}
}
