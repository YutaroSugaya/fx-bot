package command

import "fx-bot/backend/internal/port"

// EdgeMetrics は閉じた trade 群から計算する「edge の質」指標。
// dashboard / 日次サマリで「勝率は高いが逆RR」のような罠を可視化するために使う。
// 純粋関数 DeriveEdgeMetrics で算出する。
//
// N/A の表現:
//   - 損失 trade が無いと ProfitFactor / RewardRisk は数学的に未定義 (∞)。
//     ここでは 0 を返し、呼び出し側が LossCount==0 を見て "∞" / "—" を出し分ける。
//   - trade が 0 件なら全フィールド 0。
type EdgeMetrics struct {
	TradeCount int
	WinCount   int
	LossCount  int

	WinRatePct float64 // 0..100 (TradeCount==0 なら 0)

	GrossProfitJPY float64 // 勝ち trade の PnL 合計 (>= 0)
	GrossLossJPY   float64 // 負け trade の PnL 合計の絶対値 (>= 0)

	// gross / fee / net の分離 (profit_loss_jpy 単独は GROSS であって net ではない)。
	//   GrossPnLJPY  = Σ profit_loss_jpy (broker 手数料控除前)
	//   FeeTotalJPY  = Σ trades.fee_jpy (往復手数料)
	//   SwapTotalJPY = Σ trades.swap_jpy (符号付き)
	//   NetPnLJPY    = Gross − Fee + Swap (真の net)。**エッジ判定は net を使う**。
	// fee/swap 未記録の row は 0 なので net == gross (後方互換)。
	GrossPnLJPY       float64
	FeeTotalJPY       float64
	SwapTotalJPY      float64
	NetPnLJPY         float64
	FeeEstimatedCount int // fee_estimated=true の trade 数 (推定 backfill 行の混入量)

	// ProfitFactor / RewardRisk / 勝敗分類は GROSS ベースのまま (入口の質の
	// 指標としての連続性維持)。コスト床を跨ぐかは net 系 (NetPnLJPY /
	// ExpectancyJPY) で判定する。
	ProfitFactor float64 // GrossProfit / GrossLoss。0 = N/A (損失なし or trade なし)

	AvgWinPips  float64 // 勝ち trade の平均 pips (>= 0)
	AvgLossPips float64 // 負け trade の平均 pips の絶対値 (>= 0)
	RewardRisk  float64 // AvgWinPips / AvgLossPips。0 = N/A。逆RR は < 1

	ExpectancyJPY float64 // 1 trade あたり NET 期待値 = NetPnL / TradeCount

	MaxConsecutiveLosses int
}

// DeriveEdgeMetrics は閉じた trade スライスを 1-pass で歩いて EdgeMetrics を返す。
// 入力順は問わない (連敗 streak の最大値は昇順/降順で不変)。
// win = ProfitLossJPY > 0、loss = < 0、breakeven (== 0) は TradeCount のみに数え、
// 勝敗いずれにも含めず連敗 streak をリセットする。
func DeriveEdgeMetrics(trades []port.TradeRecord) EdgeMetrics {
	m := EdgeMetrics{TradeCount: len(trades)}
	var winPipsSum, lossPipsSum float64
	curLossStreak := 0

	for _, t := range trades {
		m.GrossPnLJPY += t.ProfitLossJPY
		m.FeeTotalJPY += t.FeeJPY
		m.SwapTotalJPY += t.SwapJPY
		if t.FeeEstimated {
			m.FeeEstimatedCount++
		}
		switch {
		case t.ProfitLossJPY > 0:
			m.WinCount++
			m.GrossProfitJPY += t.ProfitLossJPY
			winPipsSum += t.ProfitLossPips
			curLossStreak = 0
		case t.ProfitLossJPY < 0:
			m.LossCount++
			m.GrossLossJPY += -t.ProfitLossJPY
			lossPipsSum += -t.ProfitLossPips
			curLossStreak++
			if curLossStreak > m.MaxConsecutiveLosses {
				m.MaxConsecutiveLosses = curLossStreak
			}
		default: // breakeven
			curLossStreak = 0
		}
	}

	m.NetPnLJPY = m.GrossPnLJPY - m.FeeTotalJPY + m.SwapTotalJPY
	if m.TradeCount > 0 {
		m.WinRatePct = 100 * float64(m.WinCount) / float64(m.TradeCount)
		m.ExpectancyJPY = m.NetPnLJPY / float64(m.TradeCount)
	}
	if m.WinCount > 0 {
		m.AvgWinPips = winPipsSum / float64(m.WinCount)
	}
	if m.LossCount > 0 {
		m.AvgLossPips = lossPipsSum / float64(m.LossCount)
		if m.GrossLossJPY > 0 {
			m.ProfitFactor = m.GrossProfitJPY / m.GrossLossJPY
		}
		if m.AvgLossPips > 0 {
			m.RewardRisk = m.AvgWinPips / m.AvgLossPips
		}
	}
	return m
}
