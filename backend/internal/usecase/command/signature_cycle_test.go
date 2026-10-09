package command

import (
	"context"
	"errors"
	"testing"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

func openBuys(n int, armed bool) func(context.Context) ([]port.PositionRecord, error) {
	return func(context.Context) ([]port.PositionRecord, error) {
		out := make([]port.PositionRecord, n)
		for i := range out {
			out[i] = port.PositionRecord{Side: "BUY", RatchetArmed: armed}
		}
		return out, nil
	}
}

func sigTestParams() strategy.SignatureParams {
	return strategy.SignatureParams{
		SMAPeriod: 5, SMASlopeLB: 3, MinSlopePips: 20,
		DonchianBars: 5, ATRWindow: 5, DispMultATR: 0.2, SLBufferATR: 1.0, MinRR: 2.0,
	}
}

func risingDaily() []market.Candle {
	out := make([]market.Candle, 12)
	for i := range out {
		c := 98.00 + float64(i)*0.25
		out[i] = market.Candle{Open: c - 0.10, High: c + 0.05, Low: c - 0.15, Close: c}
	}
	return out
}

func flatDaily() []market.Candle {
	out := make([]market.Candle, 12)
	for i := range out {
		out[i] = market.Candle{Open: 100, High: 100.02, Low: 99.98, Close: 100}
	}
	return out
}

func baseCycle(daily []market.Candle, verdict strategy.AdvisorVerdict, judgeErr error, submitted *[]strategy.Signal) *SignatureCycle {
	return &SignatureCycle{
		Symbol: "USD_JPY", Pip: 0.01, Quantity: 1000, ActiveConfigID: func() string { return "trend-v4-usdjpy" }, Params: sigTestParams(),
		DailyCandles: func(ctx context.Context) ([]market.Candle, error) { return daily, nil },
		Judge: func(ctx context.Context, p strategy.BreakoutProposal, s *market.MarketSummary) (strategy.AdvisorVerdict, error) {
			return verdict, judgeErr
		},
		GetTicker: func(ctx context.Context) (*market.Ticker, error) {
			return &market.Ticker{Bid: 100.74, Ask: 100.76}, nil
		},
		BuildSummary: func() *market.MarketSummary { return nil },
		Submit: func(ctx context.Context, sig strategy.Signal, t *market.Ticker) error {
			*submitted = append(*submitted, sig)
			return nil
		},
	}
}

func goVerdict(side order.Side) strategy.AdvisorVerdict {
	return strategy.AdvisorVerdict{Go: true, Label: strategy.BreakoutTrendCont, Side: side}
}

func TestSignatureCycle_NoSetup(t *testing.T) {
	var submitted []strategy.Signal
	c := baseCycle(flatDaily(), goVerdict(order.SideBuy), nil, &submitted)
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Stage != "no_setup" {
		t.Errorf("stage = %q, want no_setup", res.Stage)
	}
	if len(submitted) != 0 {
		t.Errorf("must not submit on no setup, got %d", len(submitted))
	}
}

func TestSignatureCycle_NoGo(t *testing.T) {
	var submitted []strategy.Signal
	c := baseCycle(risingDaily(), strategy.AdvisorVerdict{Go: false, Label: strategy.BreakoutNoTrade}, nil, &submitted)
	res, _ := c.Run(context.Background())
	if res.Stage != "no_go" || len(submitted) != 0 {
		t.Errorf("want no_go + no submit, got stage=%q submitted=%d", res.Stage, len(submitted))
	}
}

func TestSignatureCycle_Submitted(t *testing.T) {
	var submitted []strategy.Signal
	c := baseCycle(risingDaily(), goVerdict(order.SideBuy), nil, &submitted)
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Stage != "submitted" {
		t.Fatalf("stage = %q, want submitted", res.Stage)
	}
	if len(submitted) != 1 {
		t.Fatalf("want 1 submit, got %d", len(submitted))
	}
	sig := submitted[0]
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Errorf("submitted signal not a buy entry: %+v", sig)
	}
	if sig.StopLossPips <= 0 {
		t.Errorf("SL must be positive (structural), got %.2f", sig.StopLossPips)
	}
	if sig.Quantity != 1000 || sig.ConfigID != "trend-v4-usdjpy" {
		t.Errorf("qty/config not set: qty=%d config=%s (config must be the REAL active id for the FK)", sig.Quantity, sig.ConfigID)
	}
	if sig.StrategyName != "signature_breakout" {
		t.Errorf("strategy name = %q", sig.StrategyName)
	}
}

func TestSignatureCycle_SideMismatch(t *testing.T) {
	var submitted []strategy.Signal
	// deterministic proposal is BUY (rising); LLM says SELL -> safe skip.
	c := baseCycle(risingDaily(), goVerdict(order.SideSell), nil, &submitted)
	res, _ := c.Run(context.Background())
	if res.Stage != "side_mismatch" || len(submitted) != 0 {
		t.Errorf("want side_mismatch + no submit, got stage=%q submitted=%d", res.Stage, len(submitted))
	}
}

func TestSignatureCycle_JudgeError(t *testing.T) {
	var submitted []strategy.Signal
	c := baseCycle(risingDaily(), strategy.AdvisorVerdict{}, errors.New("cli timeout"), &submitted)
	_, err := c.Run(context.Background())
	if err == nil {
		t.Errorf("judge transport error must surface")
	}
	if len(submitted) != 0 {
		t.Errorf("must not submit when judge errs, got %d", len(submitted))
	}
}

func TestSignatureCycle_DeterministicAutoEnter(t *testing.T) {
	// Judge == nil → deterministic mode: a found setup auto-enters without any LLM call.
	var submitted []strategy.Signal
	c := baseCycle(risingDaily(), strategy.AdvisorVerdict{}, nil, &submitted)
	c.Judge = nil
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Stage != "submitted" || len(submitted) != 1 {
		t.Fatalf("deterministic mode must auto-enter: stage=%q submitted=%d", res.Stage, len(submitted))
	}
	if !submitted[0].IsEntry() || submitted[0].Side != order.SideBuy {
		t.Errorf("expected buy entry, got %+v", submitted[0])
	}
}

func TestSignatureCycle_NoActiveConfigSkips(t *testing.T) {
	// No active config -> must NOT place an order (an order with a non-existent config_id would
	// FK-fail on insert and leave an orphan broker position). Skip with stage no_active_config.
	var submitted []strategy.Signal
	c := baseCycle(risingDaily(), goVerdict(order.SideBuy), nil, &submitted)
	c.ActiveConfigID = func() string { return "" }
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Stage != "no_active_config" || len(submitted) != 0 {
		t.Fatalf("no active config must skip (no order): stage=%q submitted=%d", res.Stage, len(submitted))
	}
}

func TestSignatureCycle_ChaseRejected(t *testing.T) {
	// Setup found (rising daily), but the live price has chased far ABOVE the breakout level so the
	// realized reward:risk against the same target/invalidation collapses below MinRR*floor -> skip.
	var submitted []strategy.Signal
	c := baseCycle(risingDaily(), goVerdict(order.SideBuy), nil, &submitted)
	c.GetTicker = func(ctx context.Context) (*market.Ticker, error) {
		return &market.Ticker{Bid: 101.49, Ask: 101.50}, nil // way past the ~100.55 level / 101.75 target
	}
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Stage != "stale_breakout" || len(submitted) != 0 {
		t.Fatalf("chased entry must skip (stale_breakout, no order): stage=%q submitted=%d", res.Stage, len(submitted))
	}
}

func TestSignatureCycle_Pyramid_AddsWhenFirstArmed(t *testing.T) {
	// MaxConcurrent=2 + one existing same-side position that is ratchet-ARMED -> the 2nd is allowed.
	var submitted []strategy.Signal
	c := baseCycle(risingDaily(), goVerdict(order.SideBuy), nil, &submitted)
	c.MaxConcurrent = 2
	c.OpenPositions = openBuys(1, true) // 1 BUY, armed (in strong profit)
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Stage != "submitted" || len(submitted) != 1 {
		t.Fatalf("armed 1st must allow the 2nd: stage=%q submitted=%d", res.Stage, len(submitted))
	}
	if submitted[0].MaxConcurrent != 2 {
		t.Errorf("signal MaxConcurrent = %d, want 2 (so the gate permits the 2nd)", submitted[0].MaxConcurrent)
	}
}

func TestSignatureCycle_Pyramid_SkipsWhenFirstNotArmed(t *testing.T) {
	// MaxConcurrent=2 but the existing position is NOT armed yet (not winning enough) -> never nanpin.
	var submitted []strategy.Signal
	c := baseCycle(risingDaily(), goVerdict(order.SideBuy), nil, &submitted)
	c.MaxConcurrent = 2
	c.OpenPositions = openBuys(1, false) // 1 BUY, NOT armed
	res, _ := c.Run(context.Background())
	if res.Stage != "pyramid_not_armed" || len(submitted) != 0 {
		t.Fatalf("unarmed 1st must block the add: stage=%q submitted=%d", res.Stage, len(submitted))
	}
}

func TestSignatureCycle_Pyramid_CapReached(t *testing.T) {
	// MaxConcurrent=2 with 2 same-side already open -> the 3rd is blocked (hard ceiling).
	var submitted []strategy.Signal
	c := baseCycle(risingDaily(), goVerdict(order.SideBuy), nil, &submitted)
	c.MaxConcurrent = 2
	c.OpenPositions = openBuys(2, true)
	res, _ := c.Run(context.Background())
	if res.Stage != "max_concurrent" || len(submitted) != 0 {
		t.Fatalf("cap reached must block: stage=%q submitted=%d", res.Stage, len(submitted))
	}
}

func TestSignatureCycle_DefaultSinglePosition(t *testing.T) {
	// Default (MaxConcurrent unset=1): any existing same-side position blocks a new one (no nanpin).
	var submitted []strategy.Signal
	c := baseCycle(risingDaily(), goVerdict(order.SideBuy), nil, &submitted)
	c.OpenPositions = openBuys(1, true) // even armed, default cap=1 blocks
	res, _ := c.Run(context.Background())
	if res.Stage != "max_concurrent" || len(submitted) != 0 {
		t.Fatalf("default single-position must block the 2nd: stage=%q submitted=%d", res.Stage, len(submitted))
	}
}

func TestSignatureCycle_WideSpreadSkips(t *testing.T) {
	var submitted []strategy.Signal
	c := baseCycle(risingDaily(), goVerdict(order.SideBuy), nil, &submitted)
	c.MaxSpreadPips = 1.0 // baseCycle ticker spread = 2.0 pips -> skip
	res, _ := c.Run(context.Background())
	if res.Stage != "wide_spread" || len(submitted) != 0 {
		t.Errorf("wide spread must skip, got stage=%q submitted=%d", res.Stage, len(submitted))
	}
}
