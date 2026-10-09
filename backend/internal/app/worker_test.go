package app

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// 連敗 streak の純粋テストは usecase/command.TestDeriveTradeAggregates に移動。
// app 側の責務は accountSnapshot の DB 集計組み立て (worker_account_snapshot_test.go)。

// computeCooldown derives (InCooldown, until, kind) from HardLimits
// + the most-recent closed trade. Pure-ish — uses Trades repo (= InMemory in
// these tests) and an injected `now`.
func TestComputeCooldown_NoConfig_NotInCooldown(t *testing.T) {
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	in, _, _ := ComputeCooldown(now, &config.HardLimits{}, backtest.NewInMemoryTradeRepo(), context.Background())
	if in {
		t.Errorf("expected not in cooldown when HardLimits.Cooldown is nil")
	}
}

func TestComputeCooldown_NoRecentTrade_NotInCooldown(t *testing.T) {
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	hl := &config.HardLimits{Cooldown: &config.CooldownConfig{AfterEntrySeconds: 60}}
	in, _, _ := ComputeCooldown(now, hl, backtest.NewInMemoryTradeRepo(), context.Background())
	if in {
		t.Errorf("expected not in cooldown with empty trade repo")
	}
}

func TestComputeCooldown_AfterLoss_TakesLongerOfEntryAndLoss(t *testing.T) {
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	repo := backtest.NewInMemoryTradeRepo()
	// Loss closed 100s ago; after_entry=60 elapsed, but after_loss=300 NOT.
	_ = repo.Insert(context.Background(), port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", ProfitLossJPY: -50,
		CloseReason: "stop_loss",
		OpenedAt:    now.Add(-200 * time.Second),
		ClosedAt:    now.Add(-100 * time.Second),
	})
	hl := &config.HardLimits{Cooldown: &config.CooldownConfig{
		AfterEntrySeconds: 60, AfterLossSeconds: 300, AfterTakeProfitSeconds: 120,
	}}
	in, until, kind := ComputeCooldown(now, hl, repo, context.Background())
	if !in {
		t.Fatalf("expected in cooldown 100s after a loss with after_loss=300")
	}
	if kind != "after_loss" {
		t.Errorf("kind: got %q want after_loss", kind)
	}
	wantUntil := now.Add(200 * time.Second) // -100 + 300 = +200
	if !until.Equal(wantUntil) {
		t.Errorf("until: got %v want %v", until, wantUntil)
	}
}

func TestComputeCooldown_AfterTakeProfit(t *testing.T) {
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	repo := backtest.NewInMemoryTradeRepo()
	// TP closed 30s ago; after_take_profit=120 → still in cooldown.
	_ = repo.Insert(context.Background(), port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", ProfitLossJPY: 100,
		CloseReason: "take_profit",
		OpenedAt:    now.Add(-90 * time.Second),
		ClosedAt:    now.Add(-30 * time.Second),
	})
	hl := &config.HardLimits{Cooldown: &config.CooldownConfig{
		AfterEntrySeconds: 60, AfterTakeProfitSeconds: 120,
	}}
	in, _, kind := ComputeCooldown(now, hl, repo, context.Background())
	if !in || kind != "after_take_profit" {
		t.Errorf("expected after_take_profit cooldown; got in=%v kind=%s", in, kind)
	}
}

func TestComputeCooldown_Elapsed_NotInCooldown(t *testing.T) {
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	repo := backtest.NewInMemoryTradeRepo()
	// Loss closed 10 minutes ago — past every cooldown.
	_ = repo.Insert(context.Background(), port.TradeRecord{
		Symbol: "USD_JPY", ProfitLossJPY: -10,
		CloseReason: "stop_loss",
		OpenedAt:    now.Add(-15 * time.Minute),
		ClosedAt:    now.Add(-10 * time.Minute),
	})
	hl := &config.HardLimits{Cooldown: &config.CooldownConfig{
		AfterEntrySeconds: 60, AfterLossSeconds: 300,
	}}
	in, _, _ := ComputeCooldown(now, hl, repo, context.Background())
	if in {
		t.Errorf("cooldown should be expired 10min after close")
	}
}

// Regression: 古い長時間ポジが直後にクローズし、新しい短時間ポジが
// 先にクローズしているケースで、cooldown 起点は「最新 close」(= 古い方) で
// なければならない。
// 旧実装は ORDER BY opened_at DESC LIMIT 1 で「最新 open」を取っていたため
// 古いトレード A の close (= 最新の close 時刻) を見逃していた。
func TestComputeCooldown_MostRecentClosedNotMostRecentOpened(t *testing.T) {
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	repo := backtest.NewInMemoryTradeRepo()
	// Trade A: opened 200s ago, closed 10s ago (loss, long-running)
	_ = repo.Insert(context.Background(), port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", ProfitLossJPY: -50,
		CloseReason: "stop_loss",
		OpenedAt:    now.Add(-200 * time.Second),
		ClosedAt:    now.Add(-10 * time.Second),
	})
	// Trade B: opened 50s ago, closed 30s ago (loss, short-running, opened more recently)
	_ = repo.Insert(context.Background(), port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", ProfitLossJPY: -30,
		CloseReason: "stop_loss",
		OpenedAt:    now.Add(-50 * time.Second),
		ClosedAt:    now.Add(-30 * time.Second),
	})
	hl := &config.HardLimits{Cooldown: &config.CooldownConfig{
		AfterEntrySeconds: 0, AfterLossSeconds: 20,
	}}
	in, until, _ := ComputeCooldown(now, hl, repo, context.Background())
	// Based on A (the most-recent CLOSED): cooldown until -10 + 20 = +10s → STILL active
	// Based on B (the most-recent OPENED, current bug): cooldown until -30 + 20 = -10s → expired (wrong)
	if !in {
		t.Fatalf("expected still in cooldown 10s after A's close (the most-recent close)")
	}
	wantUntil := now.Add(10 * time.Second)
	if !until.Equal(wantUntil) {
		t.Errorf("cooldown until: got %v want %v (= A.ClosedAt + 20s)", until, wantUntil)
	}
}

func TestComputeCooldown_AllZeros_Disabled(t *testing.T) {
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	repo := backtest.NewInMemoryTradeRepo()
	_ = repo.Insert(context.Background(), port.TradeRecord{
		Symbol: "USD_JPY", ProfitLossJPY: -10,
		CloseReason: "stop_loss",
		OpenedAt:    now.Add(-2 * time.Second),
		ClosedAt:    now.Add(-1 * time.Second),
	})
	hl := &config.HardLimits{Cooldown: &config.CooldownConfig{}} // all zeros
	in, _, _ := ComputeCooldown(now, hl, repo, context.Background())
	if in {
		t.Errorf("all-zero cooldown should mean disabled, got InCooldown=true")
	}
}
