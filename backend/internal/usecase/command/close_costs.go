package command

import (
	"fx-bot/backend/internal/port"
)

// gmoFeeRatePerLeg は GMO 外為 API の片道手数料率 (約定金額×0.002%)。
// trades.fee_jpy の推定補完 (entry leg 未捕捉の建玉 / close fill 不明の
// reconcile 推定 close) にのみ使う。broker 実報告値が取れる経路では使わない。
// backtest 側の同じ係数は CostModel.FeeRatePct (= 0.002, % 単位) が持つ。
const gmoFeeRatePerLeg = 0.00002

// closeCosts は close 経路が trades 行へ運ぶ per-trade 実コスト。
// FeeJPY = 往復 (entry leg + close leg)。SwapJPY = close fill の settledSwap。
// FeeEstimated = いずれかの leg を 0.002% 推定で補完したら true。
type closeCosts struct {
	FeeJPY       float64
	SwapJPY      float64
	FeeEstimated bool
}

// estimateLegFeeJPY は片道手数料の 0.002% 推定。約定金額 (quote 通貨建て
// price×qty) × rate × quote→JPY 換算。JPY-quote pair は rate=1.0。
func estimateLegFeeJPY(price float64, qty int, quoteJPYRate float64) float64 {
	if quoteJPYRate <= 0 {
		quoteJPYRate = 1.0
	}
	return price * float64(qty) * gmoFeeRatePerLeg * quoteJPYRate
}

// composePaperCloseCosts は paper close の往復手数料を両 leg とも 0.002% で
// 推定する (paper の損益にも実コストを載せるため)。broker 実報告が存在しないため
// 常に FeeEstimated=true。swap は session_flatten 運用でロール跨ぎが無い
// 前提のためモデル化しない (0)。fee を trades.fee_jpy へ分離計上することで
// profit_loss_jpy の GROSS 不変条件と net 集計式 (gross − fee) を Live と
// 共通のまま paper の net 集計 (net/件) が実コスト床を持つ。
func composePaperCloseCosts(p port.PositionRecord, exitPrice float64, quoteJPYRate float64) closeCosts {
	return closeCosts{
		FeeJPY: estimateLegFeeJPY(p.EntryPrice, p.Quantity, quoteJPYRate) +
			estimateLegFeeJPY(exitPrice, p.Quantity, quoteJPYRate),
		FeeEstimated: true,
	}
}

// composeLiveCloseCosts は entry leg (positions.entry_fee_jpy) と close leg
// (broker 実報告 or 推定) を合成する。
//
//   - p.EntryFeeJPY != nil → broker 実報告 (0 含む。手数料無料期間の 0 は実値)。
//   - p.EntryFeeJPY == nil → migration 0008 以前の建玉。0.002% 推定 + estimated flag。
//   - closeLegReported=false (reconcile の推定 close 等) → close leg も exitPrice
//     から 0.002% 推定 + estimated flag。swap は不明なので呼出側が 0 を渡す。
func composeLiveCloseCosts(
	p port.PositionRecord,
	exitPrice float64,
	closeFeeJPY, closeSwapJPY float64,
	closeLegReported bool,
	quoteJPYRate float64,
) closeCosts {
	out := closeCosts{SwapJPY: closeSwapJPY}
	if p.EntryFeeJPY != nil {
		out.FeeJPY += *p.EntryFeeJPY
	} else {
		out.FeeJPY += estimateLegFeeJPY(p.EntryPrice, p.Quantity, quoteJPYRate)
		out.FeeEstimated = true
	}
	if closeLegReported {
		out.FeeJPY += closeFeeJPY
	} else {
		out.FeeJPY += estimateLegFeeJPY(exitPrice, p.Quantity, quoteJPYRate)
		out.FeeEstimated = true
	}
	return out
}
