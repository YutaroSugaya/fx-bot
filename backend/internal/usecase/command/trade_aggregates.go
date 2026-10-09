package command

import (
	"time"

	"fx-bot/backend/internal/port"
)

// TradeAggregates groups the 1-pass derivations that both Worker.accountSnapshot
// and EntryAdmission.snapshot need from a closed_at DESC trade slice.
//
// Fields map 1:1 to risk.AccountSnapshot graduated-cooldown / direction-block
// inputs:
//   - ConsecutiveLosses    : binary cap (daily block) + 3-loss freeze
//   - LastLossClosedAt     : freeze 起点 (streak head loss's closed_at)
//   - BuyStopLossesToday   : BUY direction block after 2 SLs
//   - SellStopLossesToday  : SELL direction block after 2 SLs
type TradeAggregates struct {
	ConsecutiveLosses   int
	LastLossClosedAt    time.Time
	BuyStopLossesToday  int
	SellStopLossesToday int
	// Last{Buy,Sell}ClosedAt: side 別の直近 closed_at(勝ち負け・close reason 問わず)。
	// reentry cooldown(同 symbol 同 side は決済後 N 分新規禁止)の入力。
	// 入力 slice が当日分 (06:00 JST〜) なので日境界を跨ぐ持ち越しはしない仕様。
	LastBuyClosedAt  time.Time
	LastSellClosedAt time.Time
}

// DeriveTradeAggregates walks `trades` (closed_at DESC) once and returns the
// graduated-cooldown / direction-block inputs together.
//
// streakBroken flag splits the two concerns: the loss streak stops on the
// first non-loss, but the per-side SL counts must keep going past it (a SL
// today still counts against the side even if a TP landed between).
func DeriveTradeAggregates(trades []port.TradeRecord) TradeAggregates {
	agg := TradeAggregates{}
	streakBroken := false
	for i, t := range trades {
		if t.CloseReason == "stop_loss" {
			switch t.Side {
			case "BUY":
				agg.BuyStopLossesToday++
			case "SELL":
				agg.SellStopLossesToday++
			}
		}
		// side 別の直近決済 (closed_at DESC なので初出が最新)。close reason 不問 —
		// reentry cooldown は勝ち直後の即再 IN も塞ぐのが狙い。
		switch t.Side {
		case "BUY":
			if agg.LastBuyClosedAt.IsZero() {
				agg.LastBuyClosedAt = t.ClosedAt
			}
		case "SELL":
			if agg.LastSellClosedAt.IsZero() {
				agg.LastSellClosedAt = t.ClosedAt
			}
		}
		if streakBroken {
			continue
		}
		if t.ProfitLossJPY < 0 {
			if i == 0 {
				agg.LastLossClosedAt = t.ClosedAt
			}
			agg.ConsecutiveLosses++
			continue
		}
		streakBroken = true
	}
	return agg
}
