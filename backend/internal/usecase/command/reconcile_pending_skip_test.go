package command

import (
	"context"
	"os"
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

// Entry saga race-window guard.
//
// Entry saga の (PlaceOrder → ResolveExecution → PlaceSettleOCO → DB INSERT) の
// 数秒間、broker には position が見えるが DB にはまだ INSERT されていない。
// この間に reconcile が走ると「裸 broker position」を誤検出して external adopt
// 経路に流れ、resolveActiveConfigIDForExternalAdoption が失敗すると
// emergency_stop trip → advisor が以後 config を更新できなくなる二次障害が
// 起こりうる。
//
// 修正契約: Reconcile に PendingPositionTracker を inject し、handleNakedBroker
// は IsPending な broker_position_id を adopt 候補から除外して skip する。
// 次の reconcile pass (entry saga 完了後) で DB と broker が整合して正常 flow に戻る。

func TestReconcile_LiveRuntime_NakedBrokerInPendingTracker_IsSkipped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()

	// Broker 側に「entry saga 進行中」の position が見える状態。
	// DB 側は空 (= まだ INSERT されていない race-window のシミュレーション)。
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

	// 同じ id を pending として登録 → reconcile は触らないはず。
	tracker := safety.NewPendingPositions()
	tracker.MarkPending("1000002")

	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:     ReconcileModeRuntime,
		LiveMode: config.ModeLiveConfig,
		Closer:   backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		// pending tracker を inject。
		PendingTracker: tracker,
	}
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Adopted != 0 {
		t.Errorf("Adopted: got %d want 0 (pending position must NOT be adopted)", sum.Adopted)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped: got %d want 0 (skip must not trip emergency_stop)", sum.Tripped)
	}
	if _, err := os.Stat(flagPath); err == nil {
		t.Errorf("emergency_stop flag must NOT exist after skip")
	}
}

// tracker から MarkResolved された後 (= entry saga が DB INSERT を完了 / 失敗
// した後) は通常の adopt 経路に戻ること。これで「pending guard が永続化しない」
// 不変条件を担保する。
func TestReconcile_LiveRuntime_NakedBrokerNotPending_StillAdoptsAsExternal(t *testing.T) {
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
				BrokerPositionID: "EXT-1",
				Symbol:           "USD_JPY",
				Side:             order.SideBuy,
				Quantity:         1000,
				EntryPrice:       159.0,
				OpenedAt:         time.Now(),
			}}, nil
		},
	}

	tracker := safety.NewPendingPositions()
	// pending には別 id しか入れていない → EXT-1 は通常経路で処理されるはず。
	tracker.MarkPending("OTHER")

	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:            ReconcileModeRuntime,
		LiveMode:        config.ModeLiveConfig,
		Closer:          backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		StrategyConfigs: configRepo,
		PendingTracker:  tracker,
	}
	sum, _ := r.Run(ctx)
	if sum.Adopted != 1 {
		t.Errorf("Adopted: got %d want 1 (non-pending naked broker must be adopted)", sum.Adopted)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped: got %d want 0", sum.Tripped)
	}
}

// PendingTracker が nil の場合は従来挙動 (skip しない)。既存テストの後方互換。
func TestReconcile_NilPendingTracker_DoesNotPanic(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return nil, nil
		},
	}
	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode: ReconcileModeStartup, LiveMode: config.ModePaperConfig,
		Closer: backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		// PendingTracker: nil 明示
	}
	if _, err := r.Run(ctx); err != nil {
		t.Fatalf("nil tracker must not break Run: %v", err)
	}
}
