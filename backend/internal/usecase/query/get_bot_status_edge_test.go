package query

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
)

// EdgeMetricsFn が wire されたら per-symbol と primary top-level の両方に
// edge_metrics が乗る (計測パネル)。
func TestGetBotStatus_EdgeMetricsFn_PopulatesPerSymbolAndPrimary(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	seedOpen(t, repo, "USD_JPY", 1)

	cfg := &config.BotConfig{Symbol: "USD_JPY"}
	cfg.Normalize()
	cfg.Bot.Mode = config.ModePaperConfig

	q := &GetBotStatusQuery{
		BotConfig:       cfg,
		Positions:       repo,
		EmergencyActive: func() bool { return false },
		StartedAt:       time.Now().Add(-time.Minute),
		EdgeMetricsFn: func(_ context.Context, _ string) (EdgeMetricsView, error) {
			return EdgeMetricsView{
				TradeCount: 8, WinCount: 7, LossCount: 1,
				WinRatePct: 87.5, ProfitFactor: 2.27,
				AvgWinPips: 4.87, AvgLossPips: 15, RewardRisk: 0.32,
				ExpectancyJpy: 23.88, NetPnlJpy: 191,
				MaxConsecutiveLosses: 1, WindowDays: 90,
			}, nil
		},
	}
	v, err := q.Execute(context.Background(), GetBotStatusInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(v.Symbols) != 1 || v.Symbols[0].EdgeMetrics == nil {
		t.Fatalf("per-symbol EdgeMetrics not populated: %+v", v.Symbols)
	}
	if v.Symbols[0].EdgeMetrics.RewardRisk != 0.32 || v.Symbols[0].EdgeMetrics.TradeCount != 8 {
		t.Errorf("edge metrics values: %+v", v.Symbols[0].EdgeMetrics)
	}
	if v.EdgeMetrics == nil || v.EdgeMetrics.RewardRisk != 0.32 {
		t.Errorf("top-level EdgeMetrics (primary) not mirrored: %+v", v.EdgeMetrics)
	}
}

// EdgeMetricsFn が nil なら edge_metrics は省略される。
func TestGetBotStatus_EdgeMetricsFn_NilOmits(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	seedOpen(t, repo, "USD_JPY", 0)

	cfg := &config.BotConfig{Symbol: "USD_JPY"}
	cfg.Normalize()
	cfg.Bot.Mode = config.ModePaperConfig

	q := &GetBotStatusQuery{
		BotConfig:       cfg,
		Positions:       repo,
		EmergencyActive: func() bool { return false },
		StartedAt:       time.Now(),
	}
	v, err := q.Execute(context.Background(), GetBotStatusInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if v.EdgeMetrics != nil {
		t.Errorf("top-level EdgeMetrics should be nil when fn nil")
	}
	if len(v.Symbols) > 0 && v.Symbols[0].EdgeMetrics != nil {
		t.Errorf("per-symbol EdgeMetrics should be nil when fn nil")
	}
}
