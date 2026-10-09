package command

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// ナンピン禁止は override できない: 手動 override で open_positions の cap を越えられても、
// 同 symbol 同 side の建玉があれば admission は拒否する。逆 side なら cap の override は効く。
func TestEntryAdmission_ManualOverride_NeverStacksSameSide(t *testing.T) {
	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 1},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	for _, tc := range []struct {
		openSide string
		wantOK   bool
	}{
		{openSide: "BUY", wantOK: false},
		{openSide: "SELL", wantOK: true},
	} {
		t.Run("open_"+tc.openSide, func(t *testing.T) {
			flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
			posRepo := backtest.NewInMemoryPositionRepo()
			tradeRepo := backtest.NewInMemoryTradeRepo()
			_, _ = posRepo.Insert(context.Background(), port.PositionInsertInput{
				Position: port.PositionRecord{
					Symbol: "USD_JPY", Side: tc.openSide, Quantity: 1000, EntryPrice: 100,
					StrategyConfigID: "cfg-seed",
					Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
				},
			})
			a, _ := newAdmission(t, posRepo, tradeRepo, flag, 1)

			sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
			verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
				Signal:        sig,
				ActiveConfig:  cfg,
				AllowOverride: true,
				Source:        "manual",
			})
			if err != nil {
				t.Fatalf("admission: %v", err)
			}
			if release != nil {
				release()
			}
			if verdict.Allowed != tc.wantOK {
				t.Fatalf("open %s + manual BUY override: allowed=%v want %v (reason=%q)",
					tc.openSide, verdict.Allowed, tc.wantOK, verdict.Reason)
			}
			if !tc.wantOK && !strings.HasPrefix(verdict.Reason, "pyramiding_blocked_same_side_buy") {
				t.Errorf("reason=%q, want pyramiding_blocked_same_side_buy", verdict.Reason)
			}
		})
	}
}
