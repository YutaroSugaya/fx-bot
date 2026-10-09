package command

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
)

// TestExecuteOrder_OnSignal_PlacesPaperPosition: opens via the paper broker
// and ensures the DB record matches the signal.
func TestExecuteOrder_OnSignal_PlacesPaperPosition(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	pb := broker.NewPaperBroker(broker.PaperBrokerConfig{
		Clock: func() time.Time { return now },
		Pricer: func(_ context.Context, _ string) (float64, float64, error) {
			return 150.10, 150.13, nil
		},
	})
	repo := backtest.NewInMemoryPositionRepo()
	u := NewExecuteOrder(pb, repo, config.ModePaperConfig, "USD_JPY", silentLogger())
	u.Clock = func() time.Time { return now }

	sig := strategy.Signal{
		Decision:       strategy.DecisionEnter,
		Side:           order.SideBuy,
		EntryPrice:     150.13,
		TakeProfitPips: 2.0,
		StopLossPips:   2.5,
		MaxHoldMinutes: 20,
		Quantity:       100,
		ConfigID:       "cfg-test",
		StrategyName:   "momentum_pullback",
		SignalID:       "sig-1",
	}
	if err := u.OnSignal(context.Background(), sig, &market.Ticker{Bid: 150.10, Ask: 150.13}); err != nil {
		t.Fatalf("OnSignal: %v", err)
	}
	open, _ := repo.ListOpenOrClosing(context.Background(), "USD_JPY")
	if len(open) != 1 || open[0].EntryPrice != 150.13 || open[0].Side != "BUY" {
		t.Fatalf("position record: %+v", open)
	}
	if open[0].StrategyConfigID != "cfg-test" {
		t.Errorf("config id: %s", open[0].StrategyConfigID)
	}
}

// TestManageOpenPositions_TPHit closes at TP when the bid moves past entry+TP.
func TestManageOpenPositions_TPHit_ClosesAndRecordsTrade(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	pb := broker.NewPaperBroker(broker.PaperBrokerConfig{
		Clock: func() time.Time { return now },
		Pricer: func(_ context.Context, _ string) (float64, float64, error) {
			return 150.10, 150.13, nil
		},
	})
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()

	exec := NewExecuteOrder(pb, posRepo, config.ModePaperConfig, "USD_JPY", silentLogger())
	exec.Clock = func() time.Time { return now }

	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy,
		TakeProfitPips: 2.0, StopLossPips: 2.5, MaxHoldMinutes: 20,
		Quantity: 100,
		ConfigID: "cfg-test", SignalID: "sig-1",
	}
	_ = exec.OnSignal(context.Background(), sig, &market.Ticker{Bid: 150.10, Ask: 150.13})

	pb.SwapPricer(func(_ context.Context, _ string) (float64, float64, error) {
		return 150.16, 150.19, nil
	})

	mgr := NewManageOpenPositions(pb, posRepo, tradeRepo, "USD_JPY", config.ModePaperConfig, silentLogger())
	mgr.Closer = backtest.NewInMemoryPositionCloser(posRepo, tradeRepo)
	mgr.Clock = func() time.Time { return now.Add(time.Minute) }

	if err := mgr.OnTick(context.Background(), market.Ticker{Bid: 150.16, Ask: 150.19, Timestamp: now.Add(time.Minute)}); err != nil {
		t.Fatalf("OnTick: %v", err)
	}
	trades, _ := tradeRepo.ListSince(context.Background(), time.Time{}, 100)
	if len(trades) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(trades))
	}
	if trades[0].CloseReason != "take_profit" {
		t.Errorf("close reason: %s", trades[0].CloseReason)
	}
	if trades[0].ProfitLossJPY <= 0 {
		t.Errorf("expected positive PnL, got %v", trades[0].ProfitLossJPY)
	}
}

func TestManageOpenPositions_SLHit(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	pb := broker.NewPaperBroker(broker.PaperBrokerConfig{
		Clock:  func() time.Time { return now },
		Pricer: func(_ context.Context, _ string) (float64, float64, error) { return 150.10, 150.13, nil },
	})
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	exec := NewExecuteOrder(pb, posRepo, config.ModePaperConfig, "USD_JPY", silentLogger())
	exec.Clock = func() time.Time { return now }
	_ = exec.OnSignal(context.Background(), strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy,
		TakeProfitPips: 2.0, StopLossPips: 2.5,
		Quantity: 100,
		ConfigID: "c", SignalID: "s",
	}, &market.Ticker{Bid: 150.10, Ask: 150.13})

	pb.SwapPricer(func(_ context.Context, _ string) (float64, float64, error) {
		return 150.10, 150.13, nil
	})
	mgr := NewManageOpenPositions(pb, posRepo, tradeRepo, "USD_JPY", config.ModePaperConfig, silentLogger())
	mgr.Closer = backtest.NewInMemoryPositionCloser(posRepo, tradeRepo)
	mgr.Clock = func() time.Time { return now.Add(time.Minute) }
	if err := mgr.OnTick(context.Background(), market.Ticker{Bid: 150.10, Ask: 150.13, Timestamp: now.Add(time.Minute)}); err != nil {
		t.Fatalf("OnTick: %v", err)
	}
	trades, _ := tradeRepo.ListSince(context.Background(), time.Time{}, 100)
	if len(trades) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(trades))
	}
	if trades[0].CloseReason != "stop_loss" {
		t.Errorf("close reason: %s", trades[0].CloseReason)
	}
	if trades[0].ProfitLossJPY >= 0 {
		t.Errorf("expected negative PnL, got %v", trades[0].ProfitLossJPY)
	}
}

func TestManageOpenPositions_MaxHold(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	pb := broker.NewPaperBroker(broker.PaperBrokerConfig{
		Clock:  func() time.Time { return now },
		Pricer: func(_ context.Context, _ string) (float64, float64, error) { return 150.10, 150.13, nil },
	})
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	exec := NewExecuteOrder(pb, posRepo, config.ModePaperConfig, "USD_JPY", silentLogger())
	exec.Clock = func() time.Time { return now }
	_ = exec.OnSignal(context.Background(), strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy,
		TakeProfitPips: 99, StopLossPips: 99, MaxHoldMinutes: 5,
		Quantity: 100,
		ConfigID: "c", SignalID: "s",
	}, &market.Ticker{Bid: 150.10, Ask: 150.13})

	mgr := NewManageOpenPositions(pb, posRepo, tradeRepo, "USD_JPY", config.ModePaperConfig, silentLogger())
	mgr.Closer = backtest.NewInMemoryPositionCloser(posRepo, tradeRepo)
	mgr.Clock = func() time.Time { return now.Add(6 * time.Minute) }

	if err := mgr.OnTick(context.Background(), market.Ticker{Bid: 150.10, Ask: 150.13, Timestamp: now.Add(6 * time.Minute)}); err != nil {
		t.Fatalf("OnTick: %v", err)
	}
	trades, _ := tradeRepo.ListSince(context.Background(), time.Time{}, 100)
	if len(trades) != 1 || trades[0].CloseReason != "max_hold" {
		t.Errorf("expected max_hold close, got %+v", trades)
	}
}

// Confirms EntryMutex actually serialises concurrent OnSignal calls. With a
// 50ms delay inside the fake broker's PlaceOrder, two concurrent goroutines
// should take ≥100ms in total when the mutex is wired in (vs ~50ms in
// parallel without it).
func TestExecuteOrder_EntryMutex_SerialisesConcurrentCallers(t *testing.T) {
	br := &fakeBroker{placeOrderDelay: 50 * time.Millisecond}
	pos := backtest.NewInMemoryPositionRepo()
	var entryMu sync.Mutex
	exec := NewExecuteOrder(br, pos, config.ModePaperConfig, "USD_JPY", silentLogger())
	exec.EntryMutex = &entryMu

	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy,
		TakeProfitPips: 10, StopLossPips: 10, MaxHoldMinutes: 5,
		Quantity: 100,
		ConfigID: "c", SignalID: "s",
	}
	tk := &market.Ticker{Bid: 150.10, Ask: 150.13}

	var wg sync.WaitGroup
	wg.Add(2)
	start := time.Now()
	for i := 0; i < 2; i++ {
		go func() { defer wg.Done(); _ = exec.OnSignal(context.Background(), sig, tk) }()
	}
	wg.Wait()
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("two concurrent calls took %v; expected ≥100ms (= serialised by mutex)", elapsed)
	}
	if br.placeOrderCalls != 2 {
		t.Errorf("placeOrderCalls: got %d want 2", br.placeOrderCalls)
	}
}

// TestExecuteOrder_UsesSignalQuantity pins that OnSignal forwards sig.Quantity
// to the broker (no more hard-coded 100 in defaultQty).
func TestExecuteOrder_UsesSignalQuantity(t *testing.T) {
	br := &fakeBroker{}
	pos := backtest.NewInMemoryPositionRepo()
	exec := NewExecuteOrder(br, pos, config.ModePaperConfig, "USD_JPY", silentLogger())

	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy,
		TakeProfitPips: 10, StopLossPips: 10, MaxHoldMinutes: 5,
		Quantity: 5000,
		ConfigID: "c", SignalID: "s",
	}
	if err := exec.OnSignal(context.Background(), sig, &market.Ticker{Bid: 150.10, Ask: 150.13}); err != nil {
		t.Fatalf("OnSignal: %v", err)
	}
	if br.placeOrderReq.Quantity != 5000 {
		t.Errorf("broker request quantity: got %d want 5000", br.placeOrderReq.Quantity)
	}
	open, _ := pos.ListOpenOrClosing(context.Background(), "USD_JPY")
	if len(open) != 1 || open[0].Quantity != 5000 {
		t.Errorf("position record quantity: %+v", open)
	}
}

// TestExecuteOrder_PropagatesEarlyExitFromSignal pins that OnSignal copies
// EarlyExitWindowMinutes / EarlyExitTargetPips from the signal into the
// inserted PositionRecord. Without this, the early-exit feature can never
// fire even when the strategy/config sets the fields.
func TestExecuteOrder_PropagatesEarlyExitFromSignal(t *testing.T) {
	br := &fakeBroker{}
	pos := backtest.NewInMemoryPositionRepo()
	exec := NewExecuteOrder(br, pos, config.ModePaperConfig, "USD_JPY", silentLogger())

	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy,
		TakeProfitPips: 25, StopLossPips: 16, MaxHoldMinutes: 270,
		Quantity:               1000,
		EarlyExitWindowMinutes: 30,
		EarlyExitTargetPips:    -2.0,
		ConfigID:               "c", SignalID: "s",
	}
	if err := exec.OnSignal(context.Background(), sig, &market.Ticker{Bid: 159.13, Ask: 159.14}); err != nil {
		t.Fatalf("OnSignal: %v", err)
	}
	open, _ := pos.ListOpenOrClosing(context.Background(), "USD_JPY")
	if len(open) != 1 {
		t.Fatalf("expected 1 open position, got %d", len(open))
	}
	got := open[0]
	if got.EarlyExitWindowMinutes != 30 {
		t.Errorf("EarlyExitWindowMinutes: got %d want 30", got.EarlyExitWindowMinutes)
	}
	if got.EarlyExitTargetPips != -2.0 {
		t.Errorf("EarlyExitTargetPips: got %v want -2.0", got.EarlyExitTargetPips)
	}
}

// E2E: 2 連敗を seed しても admission verdict 経由の OnSignal は qty を halve しない。
// MinQuantity=1000 (= GMO 最低)、sig.Quantity=3000 → broker request は 3000 のまま。
// 注: 同方向 SL 2 回の direction block を避けるため side を混ぜる。
func TestExecuteOrder_OnSignal_NoQtyHalveOnTwoConsecutiveLossesViaAdmission(t *testing.T) {
	br := &fakeBroker{}
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	// Fixed clock so seeded SLs and the snapshot's startOfDay share one instant
	// — otherwise this flakes in the ~1h after midnight (bot TZ).
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	seedAlternatingSLs(t, tradeRepo, now)

	mu := &sync.Mutex{}
	admission := &EntryAdmission{
		Mutex:     mu,
		Positions: posRepo,
		Trades:    tradeRepo,
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
			Risk:   config.RiskSection{MaxConsecutiveLosses: 4},
		},
		EmergencyFlagPath: filepath.Join(t.TempDir(), "emergency_stop.flag"),
		Logger:            silentLogger(),
		Clock:             func() time.Time { return now },
	}
	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	exec := NewExecuteOrder(br, posRepo, config.ModePaperConfig, "USD_JPY", silentLogger())
	exec.Admission = admission
	exec.ActiveConfig = func() *config.StrategyConfig { return cfg }
	exec.MinQuantity = 1000

	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy,
		TakeProfitPips: 10, StopLossPips: 10, MaxHoldMinutes: 5,
		Quantity: 3000,
		ConfigID: "c", SignalID: "s",
	}
	if err := exec.OnSignal(context.Background(), sig, &market.Ticker{Bid: 150.10, Ask: 150.13}); err != nil {
		t.Fatalf("OnSignal: %v", err)
	}
	// 連敗時の qty 半減は廃止: 2 連敗でも full qty で発注する。
	if br.placeOrderReq.Quantity != 3000 {
		t.Errorf("broker request quantity: got %d want 3000 (halve retired)", br.placeOrderReq.Quantity)
	}
	open, _ := posRepo.ListOpenOrClosing(context.Background(), "USD_JPY")
	if len(open) != 1 || open[0].Quantity != 3000 {
		t.Errorf("position record quantity: %+v", open)
	}
}

// 連敗時の qty 半減は廃止: 2 連敗でも sig.Quantity=1000 がそのまま
// 発注される (mul は常に 1.0 = no-op)。applyQtyMultiplier の floor と合わせ
// broker min を割らないことを担保。side を混ぜて同方向 SL block を回避。
func TestExecuteOrder_OnSignal_KeepsOriginalQtyOnTwoConsecutiveLosses(t *testing.T) {
	br := &fakeBroker{}
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	now := time.Now()
	seedAlternatingSLs(t, tradeRepo, now)
	mu := &sync.Mutex{}
	admission := &EntryAdmission{
		Mutex:     mu,
		Positions: posRepo,
		Trades:    tradeRepo,
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
			Risk:   config.RiskSection{MaxConsecutiveLosses: 4},
		},
		EmergencyFlagPath: filepath.Join(t.TempDir(), "emergency_stop.flag"),
		Logger:            silentLogger(),
		Clock:             time.Now,
	}
	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	exec := NewExecuteOrder(br, posRepo, config.ModePaperConfig, "USD_JPY", silentLogger())
	exec.Admission = admission
	exec.ActiveConfig = func() *config.StrategyConfig { return cfg }
	exec.MinQuantity = 1000

	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy,
		TakeProfitPips: 10, StopLossPips: 10, MaxHoldMinutes: 5,
		Quantity: 1000, // halved=500 < MinQty=1000 → keep original
		ConfigID: "c", SignalID: "s",
	}
	if err := exec.OnSignal(context.Background(), sig, &market.Ticker{Bid: 150.10, Ask: 150.13}); err != nil {
		t.Fatalf("OnSignal: %v", err)
	}
	if br.placeOrderReq.Quantity != 1000 {
		t.Errorf("broker request quantity: got %d want 1000 (halve floored to original)", br.placeOrderReq.Quantity)
	}
}

// TestExecuteOrder_RejectsZeroQuantityEntry pins the safety guard: an Enter
// signal with Quantity<=0 (= strategy forgot to populate it) must not reach
// the broker. Fails loud rather than silently sending qty=0.
func TestExecuteOrder_RejectsZeroQuantityEntry(t *testing.T) {
	br := &fakeBroker{}
	pos := backtest.NewInMemoryPositionRepo()
	exec := NewExecuteOrder(br, pos, config.ModePaperConfig, "USD_JPY", silentLogger())

	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy,
		TakeProfitPips: 10, StopLossPips: 10, MaxHoldMinutes: 5,
		Quantity: 0, // bug: strategy didn't populate
		ConfigID: "c", SignalID: "s",
	}
	err := exec.OnSignal(context.Background(), sig, &market.Ticker{Bid: 150.10, Ask: 150.13})
	if err == nil {
		t.Errorf("expected error on zero-quantity entry, got nil")
	}
	if br.placeOrderCalls != 0 {
		t.Errorf("expected no broker call, got %d", br.placeOrderCalls)
	}
	open, _ := pos.ListOpenOrClosing(context.Background(), "USD_JPY")
	if len(open) != 0 {
		t.Errorf("expected no position record, got %d", len(open))
	}
}
