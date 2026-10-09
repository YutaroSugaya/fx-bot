package command

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/safety"
)

// pending TTL/stale 検出の reconcile 配線。
//
// entry saga がクラッシュして終端 defer (MarkResolved) が呼ばれないと、その
// broker_position_id は永久 pending になり、reconcile_pending_skip_test.go の
// skip ガードが「裸 broker position を無期限に隠す」穴に反転する。
//
// 修正契約: MarkPending から PendingTTL (デフォルト 10 分。saga 実時間は数秒) を
// 超えても resolve されない pending は stale とみなし、skip せず通常の
// naked-broker 経路 (Live=external adopt) に戻す。stale entry は tracker から
// MarkResolved で除去し、以後のループで再判定させない。

func TestReconcile_LiveRuntime_StalePendingBeyondTTL_IsAdoptedNotSkipped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	configRepo := backtest.NewInMemoryStrategyConfigRepo().
		SeedActive("cfg-active", "USD_JPY", "live_config")

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return []position.Position{{
				BrokerPositionID: "1000002",
				Symbol:           "USD_JPY",
				Side:             order.SideSell,
				Quantity:         1000,
				EntryPrice:       159.201,
				OpenedAt:         time.Now(),
			}}, nil
		},
	}

	// MarkPending した後、clock を TTL (10 分) を超えて進める = saga crash で
	// 終端 defer が呼ばれなかった状況のシミュレーション。
	now := time.Now()
	tracker := safety.NewPendingPositionsWithClock(func() time.Time { return now })
	tracker.MarkPending("1000002")
	now = now.Add(15 * time.Minute)

	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:            ReconcileModeRuntime,
		LiveMode:        config.ModeLiveConfig,
		Closer:          backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		StrategyConfigs: configRepo,
		PendingTracker:  tracker,
	}
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Adopted != 1 {
		t.Errorf("Adopted: got %d want 1 (stale pending must NOT hide a naked broker position)", sum.Adopted)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped: got %d want 0", sum.Tripped)
	}
	if tracker.IsPending("1000002") {
		t.Errorf("stale pending entry must be cleared (MarkResolved) once handled")
	}
}

// TTL 内 (fresh) の pending は従来どおり skip されること — stale 検出の追加で
// entry saga の race-window guard を壊さない regression 固定。
func TestReconcile_LiveRuntime_FreshPendingWithinTTL_IsStillSkipped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return []position.Position{{
				BrokerPositionID: "1000003",
				Symbol:           "USD_JPY",
				Side:             order.SideBuy,
				Quantity:         1000,
				EntryPrice:       159.0,
				OpenedAt:         time.Now(),
			}}, nil
		},
	}

	now := time.Now()
	tracker := safety.NewPendingPositionsWithClock(func() time.Time { return now })
	tracker.MarkPending("1000003")
	now = now.Add(30 * time.Second) // saga 実時間相当 — TTL 内

	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:           ReconcileModeRuntime,
		LiveMode:       config.ModeLiveConfig,
		Closer:         backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		PendingTracker: tracker,
	}
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Adopted != 0 || sum.Tripped != 0 {
		t.Errorf("fresh pending must still be skipped: Adopted=%d Tripped=%d", sum.Adopted, sum.Tripped)
	}
	if !tracker.IsPending("1000003") {
		t.Errorf("fresh pending entry must remain pending")
	}
}
