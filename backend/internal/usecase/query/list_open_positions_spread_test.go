package query

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

// 含み損益は「いま手仕舞いしたら」の評価。BUY は BID で(売って閉じる)、SELL は
// ASK で(買い戻して閉じる)評価しなければ GMO の評価損益と一致しない。
//
// 不具合: ダッシュの含み益が GMO 表示より高い。原因は現在値に
// MID=(bid+ask)/2 を使い、スプレッドの半分だけ常にプラス方向へ過大表示していたこと
// (BUY も SELL も利益を過大/損失を過小に見せる)。このテストは side-aware な
// close 価格評価を pin する。
func TestListOpenPositions_ClosePriceIsSideAware(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	openedAt := now.Add(-30 * time.Minute)
	// spread = 0.10 (= 10 pips @ pip 0.01)。entry 150.00。MID は 150.15。
	tk := &market.Ticker{Symbol: "USD_JPY", Bid: 150.10, Ask: 150.20}
	mk := func(side string) *ListOpenPositionsQuery {
		repo := backtest.NewInMemoryPositionRepo()
		_, _ = repo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: side, Quantity: 1000, EntryPrice: 150.00,
			Status: port.PositionStatusOpen, OpenedAt: openedAt,
		}})
		return &ListOpenPositionsQuery{
			Symbol: "USD_JPY", PipSize: 0.01, Positions: repo,
			GetTicker: func(context.Context, string) (*market.Ticker, error) { return tk, nil },
			Clock:     func() time.Time { return now },
		}
	}

	// BUY は bid 150.10 で評価 → +10 pips → +100 JPY。
	// (MID 150.15 だと +15 pips/+150 JPY に過大表示される = 旧バグ)。
	buy, err := mk("BUY").Execute(context.Background(), ListOpenPositionsInput{})
	if err != nil {
		t.Fatalf("BUY execute: %v", err)
	}
	if !nearly(buy[0].CurrentPrice, 150.10) {
		t.Errorf("BUY CurrentPrice: got %v want 150.10 (bid)", buy[0].CurrentPrice)
	}
	if !nearly(buy[0].UnrealizedPips, 10) {
		t.Errorf("BUY UnrealizedPips: got %v want 10 (bid valuation)", buy[0].UnrealizedPips)
	}
	if !nearly(buy[0].UnrealizedJPY, 100) {
		t.Errorf("BUY UnrealizedJPY: got %v want 100", buy[0].UnrealizedJPY)
	}

	// SELL は ask 150.20 で評価 → -20 pips → -200 JPY。
	// (MID 150.15 だと -15 pips/-150 JPY に過小損失で表示される = 旧バグ)。
	sell, err := mk("SELL").Execute(context.Background(), ListOpenPositionsInput{})
	if err != nil {
		t.Fatalf("SELL execute: %v", err)
	}
	if !nearly(sell[0].CurrentPrice, 150.20) {
		t.Errorf("SELL CurrentPrice: got %v want 150.20 (ask)", sell[0].CurrentPrice)
	}
	if !nearly(sell[0].UnrealizedPips, -20) {
		t.Errorf("SELL UnrealizedPips: got %v want -20 (ask valuation)", sell[0].UnrealizedPips)
	}
	if !nearly(sell[0].UnrealizedJPY, -200) {
		t.Errorf("SELL UnrealizedJPY: got %v want -200", sell[0].UnrealizedJPY)
	}
}
