package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
)

// 無言凍結の再発防止 pin。
// admission が entry を拒否したとき、従来 OnSignal は nil を返していたため
// LLM journal / dashboard は stage="submitted"(成功)と記録し、拒否は
// LOG_LEVEL=error 下で完全に不可視だった(loss_in_window が発火し続けても
// 誰も気付けない)。このファイルは「拒否は typed rejection として全呼び出し元に
// 見える」ことを 4 経路で固定する。

// rejectingAdmission returns an EntryAdmission whose gate ALWAYS rejects
// (emergency_stop flag present) — the simplest deterministic rejection.
func rejectingAdmission(t *testing.T, posRepo *backtest.InMemoryPositionRepo) *EntryAdmission {
	t.Helper()
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	if err := os.WriteFile(flag, []byte("test"), 0o644); err != nil {
		t.Fatalf("write flag: %v", err)
	}
	return &EntryAdmission{
		Mutex:     &sync.Mutex{},
		Positions: posRepo,
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
		},
		EmergencyFlagPath: flag,
		Logger:            silentLogger(),
		Clock:             time.Now,
	}
}

// OnSignal must surface the rejection as *AdmissionRejectedError (reason
// included) and must NOT touch the broker.
func TestExecuteOrder_OnSignal_AdmissionRejected_ReturnsTypedRejection(t *testing.T) {
	br := &fakeBroker{}
	posRepo := backtest.NewInMemoryPositionRepo()
	exec := NewExecuteOrder(br, posRepo, config.ModePaperConfig, "USD_JPY", silentLogger())
	exec.Admission = rejectingAdmission(t, posRepo)
	exec.ActiveConfig = func() *config.StrategyConfig {
		return &config.StrategyConfig{
			Risk:  config.ConfigRiskSection{MaxOpenPositions: 1},
			Entry: config.EntrySection{Direction: config.DirectionBoth},
		}
	}

	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy,
		TakeProfitPips: 10, StopLossPips: 10, MaxHoldMinutes: 5,
		Quantity: 1000, ConfigID: "c", SignalID: "s",
	}
	err := exec.OnSignal(context.Background(), sig, &market.Ticker{Bid: 150.10, Ask: 150.13})
	if err == nil {
		t.Fatal("admission rejection must NOT return nil (nil = journal records 'submitted' for an order that never existed)")
	}
	var rej *AdmissionRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("want *AdmissionRejectedError, got %T: %v", err, err)
	}
	if rej.Reason != "emergency_stop" {
		t.Errorf("Reason = %q, want emergency_stop", rej.Reason)
	}
	if br.placeOrderReq.Quantity != 0 {
		t.Errorf("broker must not be called on rejection, got PlaceOrder req %+v", br.placeOrderReq)
	}
	open, _ := posRepo.ListOpenOrClosing(context.Background(), "USD_JPY")
	if len(open) != 0 {
		t.Errorf("no position may be recorded on rejection, got %d", len(open))
	}
}

// The LLM cycle must journal the rejection truthfully: stage=admission_rejected
// + the gate's reason, with NO error (a refusal is a normal outcome).
func TestLLMDecisionCycle_AdmissionRejected_JournalsStageAndReason(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		return strategy.LLMTradeDecision{Go: true, Side: order.SideBuy, TPPips: 10, SLPips: 10, Reason: "test setup"}, nil
	}, &sub)
	c.Submit = func(_ context.Context, _ strategy.Signal, _ *market.Ticker) error {
		return &AdmissionRejectedError{Reason: "loss_in_window 2081 >= cap 2000"}
	}
	j := &recordingJournal{}
	c.Journal = j

	r, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("a gate refusal is a normal outcome, not an error: %v", err)
	}
	if r.Stage != "admission_rejected" {
		t.Fatalf("stage = %q, want admission_rejected (NOT the lying 'submitted')", r.Stage)
	}
	if len(j.entries) != 1 {
		t.Fatalf("want 1 journal entry, got %d", len(j.entries))
	}
	e := j.entries[0]
	if e.Stage != "admission_rejected" {
		t.Errorf("journal stage = %q, want admission_rejected", e.Stage)
	}
	if e.RejectReason != "loss_in_window 2081 >= cap 2000" {
		t.Errorf("journal reject_reason = %q, want the gate reason", e.RejectReason)
	}
	if !e.Go || e.Side != "BUY" {
		t.Errorf("journal must keep the LLM decision (go/side), got go=%v side=%q", e.Go, e.Side)
	}
}

// The signature (advisor v2) cycle gets the same truthful stage.
func TestSignatureCycle_AdmissionRejected_NotAnError(t *testing.T) {
	var submitted []strategy.Signal
	c := baseCycle(risingDaily(), goVerdict(order.SideBuy), nil, &submitted)
	c.Submit = func(_ context.Context, _ strategy.Signal, _ *market.Ticker) error {
		return &AdmissionRejectedError{Reason: "loss_in_window 2081 >= cap 2000"}
	}
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("a gate refusal is a normal outcome, not an error: %v", err)
	}
	if res.Stage != "admission_rejected" {
		t.Fatalf("stage = %q, want admission_rejected", res.Stage)
	}
}

// The engine tick path must treat the rejection as "not executed", NOT as an
// execution failure (no ExecuteErr, no error-level noise), and record why.
func TestTradingCycle_AdmissionRejected_NoExecuteErrNoPosition(t *testing.T) {
	now := time.Now()
	pos := backtest.NewInMemoryPositionRepo()
	eng := strategy.NewEngine()
	eng.Register(alwaysBuy{})
	executor := NewExecuteOrder(&fakeBroker{}, pos, config.ModePaperConfig, "USD_JPY", silentLogger())
	executor.Admission = rejectingAdmission(t, pos)
	executor.ActiveConfig = func() *config.StrategyConfig { return mkInput(now).ActiveConfig }
	tc := &TradingCycle{Evaluator: &EvaluateEntry{Engine: eng}, Executor: executor, Logger: silentLogger()}

	res, err := tc.Execute(context.Background(), mkInput(now))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Executed {
		t.Error("Executed must be false when admission rejected (previously reported true!)")
	}
	if res.ExecuteErr != nil {
		t.Errorf("ExecuteErr must be nil for a normal gate refusal, got %v", res.ExecuteErr)
	}
	if res.GateReason != "emergency_stop" {
		t.Errorf("GateReason = %q, want the admission reason for observability", res.GateReason)
	}
	open, _ := pos.ListOpenOrClosing(context.Background(), "USD_JPY")
	if len(open) != 0 {
		t.Errorf("no position may exist, got %d", len(open))
	}
}
