package app

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// advisor promotion 用の AccountState は entry gate と同じ集計を使う必要がある。
// 特に ConsecutiveLosses はゼロ値のままだと validator の consecutive loss cap が
// 実質無効化されるため、worker.accountSnapshot と同じく closed_at DESC で算出する。
//
// 本テストは BuildPromotionAccountState ヘルパが ConsecutiveLosses を埋めることを
// 失敗テストで先に固定する (strict TDD Red)。
func TestBuildPromotionAccountState_FillsConsecutiveLosses(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// 直近 3 件が連敗、その前は勝ち。closed_at DESC で読まれることに注意。
	closeTimes := []time.Time{
		now.Add(-1 * time.Hour),
		now.Add(-2 * time.Hour),
		now.Add(-3 * time.Hour),
		now.Add(-4 * time.Hour),
	}
	pnl := []float64{-100, -200, -50, +300}
	for i := range closeTimes {
		_ = tradeRepo.Insert(ctx, port.TradeRecord{
			Symbol: "USD_JPY", Side: "BUY",
			ProfitLossJPY: pnl[i],
			OpenedAt:      closeTimes[i].Add(-time.Minute),
			ClosedAt:      closeTimes[i],
		})
	}

	got, err := BuildPromotionAccountState(ctx, posRepo, tradeRepo, BuildPromotionAccountStateInput{
		Symbol:               "USD_JPY",
		EmergencyFlagPath:    t.TempDir() + "/never",
		Timezone:             "UTC",
		Now:                  func() time.Time { return now },
		MaxDailyLossJPY:      1000,
		MaxConsecutiveLosses: 5,
		MaxOpenPositions:     2,
	})
	if err != nil {
		t.Fatalf("BuildPromotionAccountState: %v", err)
	}
	if got.ConsecutiveLosses != 3 {
		t.Errorf("ConsecutiveLosses: got %d, want 3 (latest 3 trades are losses)", got.ConsecutiveLosses)
	}
	// daily_loss は bot timezone の day-start 基準で sum of |negatives|.
	// 4 trade はすべて today (UTC base) なので 100+200+50 = 350。
	if got.DailyLossJPY != 350 {
		t.Errorf("DailyLossJPY: got %d, want 350", got.DailyLossJPY)
	}
}

// BuildPromotionAccountState は worker.accountSnapshot と同じ aggregate を
// 用いるはず — worker_test.go の TestCountConsecutiveLosses をそのまま満たすこと。
func TestBuildPromotionAccountState_NoTrades_ZeroConsecutiveLosses(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	got, err := BuildPromotionAccountState(ctx, posRepo, tradeRepo, BuildPromotionAccountStateInput{
		Symbol:               "USD_JPY",
		EmergencyFlagPath:    t.TempDir() + "/never",
		Timezone:             "UTC",
		Now:                  func() time.Time { return now },
		MaxConsecutiveLosses: 3,
	})
	if err != nil {
		t.Fatalf("BuildPromotionAccountState: %v", err)
	}
	if got.ConsecutiveLosses != 0 {
		t.Errorf("ConsecutiveLosses should be 0 with no trades; got %d", got.ConsecutiveLosses)
	}
	if got.DailyLossJPY != 0 {
		t.Errorf("DailyLossJPY should be 0 with no trades; got %d", got.DailyLossJPY)
	}
}

// AccountState は config パッケージ由来であることを型レベルで保証する
// (regression: 過去に struct を再定義してドリフトが発生した経緯)。
var _ = func() config.AccountState {
	return config.AccountState{}
}
