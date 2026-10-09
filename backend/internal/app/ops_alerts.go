package app

import (
	"fmt"
	"time"

	"fx-bot/backend/internal/port"
)

// ops_alerts.go — 放置運用のための日次サマリ / 監視 alert を組み立てる
// 純粋関数群。判定だけを行い、配信 (Notifier) と頻度制御 (1日1回 / dedup) は
// 呼び出し側 (cmd/bot の ops loop) が担う。
//
// 注意: 現状の port.Notifier 実体は stdout (slog) のみ。これらの Event は
// ログに出る。実際にスマホへ push するには LINE / Slack webhook の Notifier
// アダプタを別途足す (本ファイルはアダプタ非依存)。

// OpsSummary は per-symbol の日次サマリ入力。呼び出し側が repo から集計した
// primitives を渡す (app は usecase/command に依存しないため)。
type OpsSummary struct {
	Symbol        string
	TradeCount    int // edge 集計窓内の closed trade 数
	WinRatePct    float64
	ProfitFactor  float64 // 0 = N/A
	RewardRisk    float64 // 0 = N/A; < 1 = 逆RR
	ExpectancyJPY float64
	NetPnLJPY     float64 // edge 窓の純損益
	DailyPnLJPY   float64 // 当日 (bot TZ) 実現損益
}

// DailyDDAlert は当日 NET 実現損が日次ロス上限に到達したら warn Event を返す。
//
// entry gate (唯一の live ブレーキ) と alert は同一定義。「日次ロス」は NET 実現損
// (手数料込・risk.AccountSnapshot.DailyLossJPY = SumClosedLossJPYSince = Σ|net-負の決済 PnL
// = gross − GMO手数料 + swap|)。勝ちは相殺せず手数料を反映するので gross より大きい
// =より保守的(=安全側にブレーキが早まる)。
//
//	dailyLossJPY: 正の値 (= 当日いくら失ったか・手数料込・勝ちは相殺しない)。0 = 損失なし。
//	maxDailyLossJPY <= 0: 無効 (nil)。
//
// ⚠️「未実現 + SL 想定損」まで含める完全形は、サイズアップ時の拡張余地。
func DailyDDAlert(symbol string, dailyLossJPY float64, maxDailyLossJPY int) *port.Event {
	if maxDailyLossJPY <= 0 {
		return nil
	}
	if dailyLossJPY < float64(maxDailyLossJPY) {
		return nil
	}
	return &port.Event{
		Level: port.LevelWarn,
		Title: "daily_dd_breached",
		Body:  fmt.Sprintf("%s 当日NET実現損(手数料込) %.0f JPY が日次上限 %d JPY に到達 (entry gate と同一定義)", symbol, dailyLossJPY, maxDailyLossJPY),
		Meta: map[string]any{
			"symbol": symbol, "daily_loss_jpy": dailyLossJPY, "max_daily_loss_jpy": maxDailyLossJPY,
		},
	}
}

// NoTradeAlert は最後の close からの経過 ago が threshold 以上なら info Event を返す。
// ago < 0 (= まだ一度も約定なし) や threshold <= 0 は無効 (nil)。
func NoTradeAlert(symbol string, ago, threshold time.Duration) *port.Event {
	if threshold <= 0 || ago < 0 {
		return nil
	}
	if ago < threshold {
		return nil
	}
	return &port.Event{
		Level: port.LevelInfo,
		Title: "no_trade_for_hours",
		Body:  fmt.Sprintf("%s 最後の trade から %.1fh 約定なし (閾値 %.0fh)", symbol, ago.Hours(), threshold.Hours()),
		Meta:  map[string]any{"symbol": symbol, "hours_since_last_trade": ago.Hours()},
	}
}

// DailySummaryEvent は日次サマリ Event を 1 行で組み立てる。RR<1 (逆RR) や
// N/A (PF/RR=0) を body に明示する。
func DailySummaryEvent(s OpsSummary) port.Event {
	rr := "—"
	if s.RewardRisk > 0 {
		rr = fmt.Sprintf("%.2f", s.RewardRisk)
		if s.RewardRisk < 1 {
			rr += "⚠逆RR"
		}
	}
	pf := "—"
	if s.ProfitFactor > 0 {
		pf = fmt.Sprintf("%.2f", s.ProfitFactor)
	}
	return port.Event{
		Level: port.LevelInfo,
		Title: "daily_summary",
		Body: fmt.Sprintf("%s 当日 %.0f JPY | 直近%dtrade 勝率%.0f%% PF%s RR%s 期待値%.0f JPY/trade",
			s.Symbol, s.DailyPnLJPY, s.TradeCount, s.WinRatePct, pf, rr, s.ExpectancyJPY),
		Meta: map[string]any{
			"symbol": s.Symbol, "daily_pnl_jpy": s.DailyPnLJPY, "trade_count": s.TradeCount,
			"win_rate_pct": s.WinRatePct, "profit_factor": s.ProfitFactor,
			"reward_risk": s.RewardRisk, "expectancy_jpy": s.ExpectancyJPY,
		},
	}
}
