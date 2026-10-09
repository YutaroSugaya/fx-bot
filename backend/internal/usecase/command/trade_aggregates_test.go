package command

import (
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

// DeriveTradeAggregates は closed_at DESC のスライスを 1-pass で歩いて
// 連敗 cooldown / 同方向 SL block / reentry cooldown が使う集計値を返す純粋関数。
func TestDeriveTradeAggregates(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		trades []port.TradeRecord
		want   TradeAggregates
	}{
		{
			name:   "empty",
			trades: nil,
			want:   TradeAggregates{},
		},
		{
			name: "head_loss_two_consec_then_win",
			trades: []port.TradeRecord{
				{Side: "BUY", ProfitLossJPY: -50, CloseReason: "stop_loss", ClosedAt: now.Add(-5 * time.Minute)},
				{Side: "SELL", ProfitLossJPY: -30, CloseReason: "stop_loss", ClosedAt: now.Add(-15 * time.Minute)},
				{Side: "BUY", ProfitLossJPY: 80, CloseReason: "take_profit", ClosedAt: now.Add(-25 * time.Minute)},
			},
			want: TradeAggregates{
				ConsecutiveLosses:   2,
				LastLossClosedAt:    now.Add(-5 * time.Minute),
				BuyStopLossesToday:  1,
				SellStopLossesToday: 1,
				LastBuyClosedAt:     now.Add(-5 * time.Minute),
				LastSellClosedAt:    now.Add(-15 * time.Minute),
			},
		},
		{
			name: "head_win_streak_zero_but_sl_counts_continue",
			trades: []port.TradeRecord{
				{Side: "BUY", ProfitLossJPY: 80, CloseReason: "take_profit", ClosedAt: now.Add(-3 * time.Minute)},
				{Side: "BUY", ProfitLossJPY: -50, CloseReason: "stop_loss", ClosedAt: now.Add(-15 * time.Minute)},
				{Side: "BUY", ProfitLossJPY: -50, CloseReason: "stop_loss", ClosedAt: now.Add(-30 * time.Minute)},
			},
			want: TradeAggregates{
				ConsecutiveLosses:   0,
				LastLossClosedAt:    time.Time{},
				BuyStopLossesToday:  2, // streak broken でも当日 SL は数え続ける
				SellStopLossesToday: 0,
				LastBuyClosedAt:     now.Add(-3 * time.Minute),
			},
		},
		{
			name: "early_exit_loss_does_not_count_as_sl",
			trades: []port.TradeRecord{
				{Side: "BUY", ProfitLossJPY: -5, CloseReason: "early_exit", ClosedAt: now.Add(-5 * time.Minute)},
				{Side: "SELL", ProfitLossJPY: -50, CloseReason: "stop_loss", ClosedAt: now.Add(-20 * time.Minute)},
			},
			want: TradeAggregates{
				ConsecutiveLosses:   2,
				LastLossClosedAt:    now.Add(-5 * time.Minute),
				BuyStopLossesToday:  0, // early_exit は SL カウントしない
				SellStopLossesToday: 1,
				LastBuyClosedAt:     now.Add(-5 * time.Minute),
				LastSellClosedAt:    now.Add(-20 * time.Minute),
			},
		},
		{
			name: "all_wins",
			trades: []port.TradeRecord{
				{Side: "BUY", ProfitLossJPY: 50, CloseReason: "take_profit", ClosedAt: now.Add(-5 * time.Minute)},
				{Side: "SELL", ProfitLossJPY: 30, CloseReason: "take_profit", ClosedAt: now.Add(-15 * time.Minute)},
			},
			// 勝ち決済も Last*ClosedAt には載る (reentry cooldown は勝ち直後の再 IN が本丸)
			want: TradeAggregates{
				LastBuyClosedAt:  now.Add(-5 * time.Minute),
				LastSellClosedAt: now.Add(-15 * time.Minute),
			},
		},
		{
			name: "breakeven_at_head_breaks_streak",
			trades: []port.TradeRecord{
				{Side: "BUY", ProfitLossJPY: 0, CloseReason: "early_exit", ClosedAt: now.Add(-2 * time.Minute)},
				{Side: "BUY", ProfitLossJPY: -50, CloseReason: "stop_loss", ClosedAt: now.Add(-15 * time.Minute)},
			},
			want: TradeAggregates{
				ConsecutiveLosses:   0, // breakeven は loss ではない (旧 worker 仕様と整合)
				LastLossClosedAt:    time.Time{},
				BuyStopLossesToday:  1,
				SellStopLossesToday: 0,
				LastBuyClosedAt:     now.Add(-2 * time.Minute),
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeriveTradeAggregates(c.trades)
			if got != c.want {
				t.Errorf("DeriveTradeAggregates() = %+v, want %+v", got, c.want)
			}
		})
	}
}

// ---- reentry cooldown 入力: side 別の直近決済時刻 ----
//
// LastBuyClosedAt / LastSellClosedAt は「任意の close reason」(勝ち負け問わず)の
// side 別最新 closed_at。trades は closed_at DESC 前提なので side ごとの初出を採る。
func TestDeriveTradeAggregates_LastClosedAtPerSide(t *testing.T) {
	t1 := time.Date(2026, 7, 17, 0, 40, 0, 0, time.UTC) // 最新: SELL 利確
	t2 := time.Date(2026, 7, 17, 0, 20, 0, 0, time.UTC) // BUY SL
	t3 := time.Date(2026, 7, 17, 0, 5, 0, 0, time.UTC)  // SELL SL (t1 より古い → 無視)
	agg := DeriveTradeAggregates([]port.TradeRecord{
		{Side: "SELL", CloseReason: "ratchet_takeprofit", ProfitLossJPY: 95, ClosedAt: t1},
		{Side: "BUY", CloseReason: "stop_loss", ProfitLossJPY: -100, ClosedAt: t2},
		{Side: "SELL", CloseReason: "stop_loss", ProfitLossJPY: -101, ClosedAt: t3},
	})
	if !agg.LastSellClosedAt.Equal(t1) {
		t.Errorf("LastSellClosedAt: got %v want %v (勝ち決済も対象)", agg.LastSellClosedAt, t1)
	}
	if !agg.LastBuyClosedAt.Equal(t2) {
		t.Errorf("LastBuyClosedAt: got %v want %v", agg.LastBuyClosedAt, t2)
	}
}

func TestDeriveTradeAggregates_LastClosedAtPerSide_EmptyIsZero(t *testing.T) {
	agg := DeriveTradeAggregates(nil)
	if !agg.LastBuyClosedAt.IsZero() || !agg.LastSellClosedAt.IsZero() {
		t.Errorf("empty input must leave zero times: %+v", agg)
	}
}
