package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// --- Worker.accountSnapshot external-position policy ---
//
// Invariant: external (= source=external_broker) positions are
// display-only and must NOT consume per-symbol bot capacity. EntryAdmission
// already excludes them under the admission lock; if the pre-tick gate in
// Worker.accountSnapshot counted them via len(open), auto entry would be
// stopped at the worker level before reaching admission.
//
// This test pins the worker side to the same rule.

func seedExternalAndBot(t *testing.T, repo *backtest.InMemoryPositionRepo, sym string) (botID, extID int64) {
	t.Helper()
	t0 := time.Now()
	bot, err := repo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: sym, Side: "BUY", Quantity: 100, EntryPrice: 100,
		Status: port.PositionStatusOpen, OpenedAt: t0,
	}})
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	ext, err := repo.Insert(context.Background(), port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: sym, Side: "BUY", Quantity: 100, EntryPrice: 100,
			Status: port.PositionStatusOpen, OpenedAt: t0,
		},
		Recovered: &port.RecoveredPositionMeta{
			Reason:      port.RecoveryReasonExternalBrokerAdoption,
			RecoveredAt: t0,
		},
	})
	if err != nil {
		t.Fatalf("seed external: %v", err)
	}
	return bot, ext
}

func TestWorkerAccountSnapshot_ExternalPositionsExcludedFromOpenCount(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	_, _ = seedExternalAndBot(t, repo, "USD_JPY")

	w := &Worker{
		Symbol:    "USD_JPY",
		Positions: repo,
		Trades:    backtest.NewInMemoryTradeRepo(),
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
		},
		EmergencyFlagPath: filepath.Join(t.TempDir(), "emergency_stop.flag"),
	}
	snap, err := w.accountSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatalf("accountSnapshot: %v", err)
	}
	if snap.OpenPositions != 1 {
		t.Errorf("OpenPositions: got %d want 1 (external must be skipped)", snap.OpenPositions)
	}
}

// Loss-freeze wiring: accountSnapshot must populate Now (= clock) and
// LastLossClosedAt (= closed_at of the head trade if it's a loss). The risk
// gate's 30-minute freeze depends on both being non-zero in the streak.
func TestWorkerAccountSnapshot_PopulatesLastLossClosedAtAndNow(t *testing.T) {
	tradeRepo := backtest.NewInMemoryTradeRepo()
	// Fixed clock: pin startOfDay to a mid-day instant so the now-relative
	// fixtures don't straddle midnight (bot TZ) when the suite runs ~00:00-01:00.
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// Seed two consecutive losses; most-recent loss closed 10 minutes ago.
	mustInsertTrade(t, tradeRepo, port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", ProfitLossJPY: -50,
		CloseReason: "stop_loss",
		OpenedAt:    now.Add(-30 * time.Minute),
		ClosedAt:    now.Add(-20 * time.Minute),
	})
	mostRecentClose := now.Add(-10 * time.Minute)
	mustInsertTrade(t, tradeRepo, port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", ProfitLossJPY: -30,
		CloseReason: "stop_loss",
		OpenedAt:    now.Add(-15 * time.Minute),
		ClosedAt:    mostRecentClose,
	})

	w := &Worker{
		Symbol:    "USD_JPY",
		Positions: backtest.NewInMemoryPositionRepo(),
		Trades:    tradeRepo,
		Clock:     func() time.Time { return now },
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
		},
		EmergencyFlagPath: filepath.Join(t.TempDir(), "emergency_stop.flag"),
	}
	snap, err := w.accountSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatalf("accountSnapshot: %v", err)
	}
	if snap.ConsecutiveLosses != 2 {
		t.Errorf("ConsecutiveLosses: got %d want 2", snap.ConsecutiveLosses)
	}
	if snap.LastLossClosedAt.IsZero() {
		t.Fatalf("LastLossClosedAt should be populated (head trade is a loss)")
	}
	if diff := snap.LastLossClosedAt.Sub(mostRecentClose).Abs(); diff > time.Second {
		t.Errorf("LastLossClosedAt: got %v want ~%v (diff=%v)", snap.LastLossClosedAt, mostRecentClose, diff)
	}
	if snap.Now.IsZero() {
		t.Errorf("Now: should be populated from worker clock")
	}
}

// 直近 trade が win なら streak が切れているので LastLossClosedAt はゼロ。
// 30-min freeze は 3 連敗 streak が前提なので、ここで populate しないことで
// 古い loss の closed_at が将来の freeze を誤発火させるのを防ぐ。
func TestWorkerAccountSnapshot_LastLossClosedAtZeroWhenStreakBroken(t *testing.T) {
	tradeRepo := backtest.NewInMemoryTradeRepo()
	now := time.Now()
	// 古い loss → 直近 win の順 (DESC head が win)。streak は 0。
	mustInsertTrade(t, tradeRepo, port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", ProfitLossJPY: -40,
		CloseReason: "stop_loss",
		OpenedAt:    now.Add(-40 * time.Minute),
		ClosedAt:    now.Add(-30 * time.Minute),
	})
	mustInsertTrade(t, tradeRepo, port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", ProfitLossJPY: 80,
		CloseReason: "take_profit",
		OpenedAt:    now.Add(-15 * time.Minute),
		ClosedAt:    now.Add(-5 * time.Minute),
	})

	w := &Worker{
		Symbol:    "USD_JPY",
		Positions: backtest.NewInMemoryPositionRepo(),
		Trades:    tradeRepo,
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
		},
		EmergencyFlagPath: filepath.Join(t.TempDir(), "emergency_stop.flag"),
	}
	snap, err := w.accountSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatalf("accountSnapshot: %v", err)
	}
	if snap.ConsecutiveLosses != 0 {
		t.Errorf("ConsecutiveLosses: got %d want 0 (head=win)", snap.ConsecutiveLosses)
	}
	if !snap.LastLossClosedAt.IsZero() {
		t.Errorf("LastLossClosedAt should be zero when head trade is a win, got %v", snap.LastLossClosedAt)
	}
}

// The worker pre-tick snapshot must populate the side-aware
// counts INCLUDING external positions (OpenBuyInclExternal / OpenSellInclExternal)
// so the worker-path pyramiding pre-gate sees the same picture as EntryAdmission.
// Regression: if the counts are computed locally but dropped from the returned
// struct, the pre-gate always sees zero and never blocks same-side stacking.
// (The authoritative admission gate still blocks — this pins the pre-gate too.)
func TestWorkerAccountSnapshot_PopulatesSideCountsInclExternal(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	// seedExternalAndBot inserts 1 bot BUY + 1 external BUY.
	_, _ = seedExternalAndBot(t, repo, "USD_JPY")
	// add 1 bot SELL.
	if _, err := repo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: "USD_JPY", Side: "SELL", Quantity: 100, EntryPrice: 100,
		Status: port.PositionStatusOpen, OpenedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("seed sell: %v", err)
	}

	w := &Worker{
		Symbol:    "USD_JPY",
		Positions: repo,
		Trades:    backtest.NewInMemoryTradeRepo(),
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
		},
		EmergencyFlagPath: filepath.Join(t.TempDir(), "emergency_stop.flag"),
	}
	snap, err := w.accountSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatalf("accountSnapshot: %v", err)
	}
	if snap.OpenBuyInclExternal != 2 {
		t.Errorf("OpenBuyInclExternal: got %d want 2 (bot BUY + external BUY; external INCLUDED for the pyramiding block)", snap.OpenBuyInclExternal)
	}
	if snap.OpenSellInclExternal != 1 {
		t.Errorf("OpenSellInclExternal: got %d want 1", snap.OpenSellInclExternal)
	}
	// Sanity: bot-only open count still excludes the external BUY.
	if snap.OpenPositions != 2 {
		t.Errorf("OpenPositions: got %d want 2 (1 bot BUY + 1 bot SELL; external excluded)", snap.OpenPositions)
	}
}

func mustInsertTrade(t *testing.T, repo *backtest.InMemoryTradeRepo, rec port.TradeRecord) {
	t.Helper()
	if err := repo.Insert(context.Background(), rec); err != nil {
		t.Fatalf("insert trade: %v", err)
	}
}

// Same-side SL-count wiring: accountSnapshot must populate BuyStopLossesToday /
// SellStopLossesToday from today's CloseReason="stop_loss" trades. Counts
// per-side so the gate can block same-direction retries after 2 SL hits.
func TestWorkerAccountSnapshot_CountsTodayStopLossesBySide(t *testing.T) {
	tradeRepo := backtest.NewInMemoryTradeRepo()
	// Fixed clock: pin startOfDay to a mid-day instant (see other snapshot tests).
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// Seed mixed trades today: 2 BUY SL, 1 SELL SL, 1 BUY TP (should NOT count
	// against BuyStopLossesToday — TP is not an SL hit).
	mustInsertTrade(t, tradeRepo, port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", ProfitLossJPY: -50,
		CloseReason: "stop_loss",
		OpenedAt:    now.Add(-60 * time.Minute),
		ClosedAt:    now.Add(-50 * time.Minute),
	})
	mustInsertTrade(t, tradeRepo, port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", ProfitLossJPY: -50,
		CloseReason: "stop_loss",
		OpenedAt:    now.Add(-45 * time.Minute),
		ClosedAt:    now.Add(-35 * time.Minute),
	})
	mustInsertTrade(t, tradeRepo, port.TradeRecord{
		Symbol: "USD_JPY", Side: "SELL", ProfitLossJPY: -50,
		CloseReason: "stop_loss",
		OpenedAt:    now.Add(-30 * time.Minute),
		ClosedAt:    now.Add(-20 * time.Minute),
	})
	mustInsertTrade(t, tradeRepo, port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", ProfitLossJPY: 80,
		CloseReason: "take_profit",
		OpenedAt:    now.Add(-15 * time.Minute),
		ClosedAt:    now.Add(-5 * time.Minute),
	})

	w := &Worker{
		Symbol:    "USD_JPY",
		Positions: backtest.NewInMemoryPositionRepo(),
		Trades:    tradeRepo,
		Clock:     func() time.Time { return now },
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
		},
		EmergencyFlagPath: filepath.Join(t.TempDir(), "emergency_stop.flag"),
	}
	snap, err := w.accountSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatalf("accountSnapshot: %v", err)
	}
	if snap.BuyStopLossesToday != 2 {
		t.Errorf("BuyStopLossesToday: got %d want 2 (TP must not count)", snap.BuyStopLossesToday)
	}
	if snap.SellStopLossesToday != 1 {
		t.Errorf("SellStopLossesToday: got %d want 1", snap.SellStopLossesToday)
	}
}

// 経済指標 freeze: worker.accountSnapshot は EventCalendar が
// セットされていれば現在時刻で InFreezeWindow を問い合わせて
// InEventFreeze flag を埋める。
func TestWorkerAccountSnapshot_FillsInEventFreezeFromCalendar(t *testing.T) {
	now := time.Now()
	cal := &config.EventCalendar{
		Events: []config.CalendarEvent{
			{Name: "test event", At: now, PreMinutes: 30, PostMinutes: 30},
		},
	}
	w := &Worker{
		Symbol:    "USD_JPY",
		Positions: backtest.NewInMemoryPositionRepo(),
		Trades:    backtest.NewInMemoryTradeRepo(),
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
		},
		EventCalendar:     cal,
		EmergencyFlagPath: filepath.Join(t.TempDir(), "emergency_stop.flag"),
	}
	snap, err := w.accountSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatalf("accountSnapshot: %v", err)
	}
	if !snap.InEventFreeze {
		t.Errorf("InEventFreeze should be true when event window covers now")
	}
}

// nil EventCalendar (= 設定無し) でも snapshot は失敗しない、freeze=false。
func TestWorkerAccountSnapshot_NilEventCalendar_NoFreeze(t *testing.T) {
	w := &Worker{
		Symbol:    "USD_JPY",
		Positions: backtest.NewInMemoryPositionRepo(),
		Trades:    backtest.NewInMemoryTradeRepo(),
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
		},
		EmergencyFlagPath: filepath.Join(t.TempDir(), "emergency_stop.flag"),
	}
	snap, err := w.accountSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatalf("accountSnapshot: %v", err)
	}
	if snap.InEventFreeze {
		t.Errorf("nil EventCalendar should keep InEventFreeze=false")
	}
}

// Sanity: bot-source-only setup keeps full count.
func TestWorkerAccountSnapshot_BotPositionsStillCounted(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	t0 := time.Now()
	for i := 0; i < 3; i++ {
		_, _ = repo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100,
			Status: port.PositionStatusOpen, OpenedAt: t0,
		}})
	}
	w := &Worker{
		Symbol:    "USD_JPY",
		Positions: repo,
		Trades:    backtest.NewInMemoryTradeRepo(),
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
		},
		EmergencyFlagPath: filepath.Join(t.TempDir(), "emergency_stop.flag"),
	}
	snap, err := w.accountSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatalf("accountSnapshot: %v", err)
	}
	if snap.OpenPositions != 3 {
		t.Errorf("OpenPositions: got %d want 3", snap.OpenPositions)
	}
}

// reentry cooldown 配線: accountSnapshot は side 別の直近決済時刻
// (勝ち決済含む) と risk.reentry_cooldown_minutes を snapshot に載せる。
// gate 側 (risk.EvaluateSignal) はこの 3 値だけで判定する。
func TestWorkerAccountSnapshot_PopulatesReentryCooldownInputs(t *testing.T) {
	tradeRepo := backtest.NewInMemoryTradeRepo()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	sellClose := now.Add(-9 * time.Minute) // 利確 9 分前 (利確直後の再 IN 型)
	mustInsertTrade(t, tradeRepo, port.TradeRecord{
		Symbol: "USD_JPY", Side: "SELL", ProfitLossJPY: 95,
		CloseReason: "ratchet_takeprofit",
		OpenedAt:    now.Add(-60 * time.Minute),
		ClosedAt:    sellClose,
	})
	w := &Worker{
		Symbol:    "USD_JPY",
		Positions: backtest.NewInMemoryPositionRepo(),
		Trades:    tradeRepo,
		Clock:     func() time.Time { return now },
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
			Risk:   config.RiskSection{ReentryCooldownMinutes: 60},
		},
		EmergencyFlagPath: filepath.Join(t.TempDir(), "emergency_stop.flag"),
	}
	snap, err := w.accountSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatalf("accountSnapshot: %v", err)
	}
	if snap.ReentryCooldown != 60*time.Minute {
		t.Errorf("ReentryCooldown: got %v want 60m", snap.ReentryCooldown)
	}
	if diff := snap.LastSellClosedAt.Sub(sellClose).Abs(); diff > time.Second {
		t.Errorf("LastSellClosedAt: got %v want ~%v", snap.LastSellClosedAt, sellClose)
	}
	if !snap.LastBuyClosedAt.IsZero() {
		t.Errorf("LastBuyClosedAt: got %v want zero (BUY 決済なし)", snap.LastBuyClosedAt)
	}
}
