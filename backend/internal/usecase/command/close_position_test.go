package command

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/port"
)

func tempFlag(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "emergency_stop.flag")
}

func openPosition(t *testing.T, repo *backtest.InMemoryPositionRepo, side string, entry float64) port.PositionRecord {
	t.Helper()
	rec := port.PositionRecord{
		Symbol:           "USD_JPY",
		Side:             side,
		Quantity:         100,
		EntryPrice:       entry,
		TakeProfitPips:   20,
		StopLossPips:     15,
		MaxHoldMinutes:   240,
		StrategyConfigID: "cfg-test",
		Status:           port.PositionStatusOpen,
		OpenedAt:         time.Now().Add(-time.Hour),
	}
	id, err := repo.Insert(context.Background(), port.PositionInsertInput{
		Position: rec,
		Manual:   true,
	})
	if err != nil {
		t.Fatalf("setup insert: %v", err)
	}
	rec.ID = id
	return rec
}

// TestClosePositionCommand_PaperHappyPath: BUY/SELL の PnL 計算軸を 1 table に。
func TestClosePositionCommand_PaperHappyPath(t *testing.T) {
	cases := []struct {
		name     string
		side     string
		entry    float64
		exit     float64
		wantPnL  float64
		wantExit float64
	}{
		{"BUY profits when exit > entry", "BUY", 100.0, 101.0, 100.0, 101.0},
		{"SELL profits when exit < entry", "SELL", 100.0, 99.5, 50.0, 99.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := backtest.NewInMemoryPositionRepo()
			rec := openPosition(t, pos, tc.side, tc.entry)
			br := &fakeBroker{closePosResult: &order.Order{OrderID: "cls-1", Price: tc.exit, Status: "FILLED"}}
			cmd := &ClosePositionCommand{
				Mode: config.ModePaperConfig, Symbol: "USD_JPY", PipSize: 0.01,
				Broker: br, Positions: pos, Closer: &fakeCloser{ok: true},
				Mutex: &sync.Mutex{}, EmergencyFlagPath: tempFlag(t), Logger: silentLogger(),
			}
			out, err := cmd.Execute(context.Background(), ClosePositionInput{PositionID: rec.ID})
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if !nearly(out.ExitPrice, tc.wantExit) {
				t.Errorf("ExitPrice: got %v want %v", out.ExitPrice, tc.wantExit)
			}
			if !nearly(out.ProfitLossJPY, tc.wantPnL) {
				t.Errorf("PnL JPY: got %v want %v", out.ProfitLossJPY, tc.wantPnL)
			}
		})
	}
}

// TestClosePositionCommand_EmergencyPaths: 4 つの emergency-trip 経路を統合。
func TestClosePositionCommand_EmergencyPaths(t *testing.T) {
	cases := []struct {
		name           string
		mode           config.Mode
		brokerSetup    func() port.Broker
		closer         *fakeCloser
		insertWithLegs bool // when true, seed positions_live.tp_order_id/sl_order_id so the close saga has something to cancel (required for cases that exercise the cancel-fails path)
	}{
		{
			name: "live resolve timeout",
			mode: config.ModeLiveConfig,
			brokerSetup: func() port.Broker {
				return &fakeLiveBroker{
					fakeBroker: fakeBroker{closePosResult: &order.Order{OrderID: "cls-1", Price: 0}},
					resolveErr: errResolverFailure,
				}
			},
			closer: &fakeCloser{ok: true},
		},
		{
			name: "live no resolver (broker doesn't implement ExecutionResolver)",
			mode: config.ModeLiveConfig,
			brokerSetup: func() port.Broker {
				return &fakeBroker{closePosResult: &order.Order{OrderID: "cls-1", Price: 0}}
			},
			closer: &fakeCloser{ok: true},
		},
		{
			// cancelLiveSettleLegsForPosition only attempts cancel
			// when leg IDs are recorded. The H2 critical path requires a
			// position WITH leg IDs that CancelOrder fails on (insertion is
			// done below by overriding the position via insertWithLegs).
			name: "live cancel settle leg fails before close (H2)",
			mode: config.ModeLiveConfig,
			brokerSetup: func() port.Broker {
				return &fakeLiveBroker{
					fakeBroker: fakeBroker{
						activeOrders:   []order.Order{{OrderID: "sl-1"}},
						cancelOrderErr: errors.New("rate limited"),
					},
				}
			},
			closer:         &fakeCloser{ok: true},
			insertWithLegs: true,
		},
		{
			name: "live close fails after settle legs cancelled (H2)",
			mode: config.ModeLiveConfig,
			brokerSetup: func() port.Broker {
				return &fakeLiveBroker{
					fakeBroker: fakeBroker{
						activeOrders: []order.Order{{OrderID: "tp-1"}, {OrderID: "sl-1"}},
						closePosErr:  errors.New("broker 500"),
					},
				}
			},
			closer: &fakeCloser{ok: true},
		},
		{
			name:        "race: CloseAndRecord returns ok=false",
			mode:        config.ModePaperConfig,
			brokerSetup: func() port.Broker { return &fakeBroker{} },
			closer:      &fakeCloser{ok: false},
		},
		{
			name:        "DB error from CloseAndRecord",
			mode:        config.ModePaperConfig,
			brokerSetup: func() port.Broker { return &fakeBroker{} },
			closer:      &fakeCloser{err: errors.New("db down")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := backtest.NewInMemoryPositionRepo()
			rec := openPosition(t, pos, "BUY", 100.0)
			if tc.insertWithLegs {
				// Overwrite the positions_live row with non-empty leg ids so
				// the close saga has something to cancel (= the CancelOrder
				// failure path is reachable). Without this, the helper would
				// no-op the cancel step on missing legs.
				_, _ = pos.Insert(context.Background(), port.PositionInsertInput{
					Position: port.PositionRecord{
						Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100.0,
						TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
						StrategyConfigID: "cfg-test", Status: port.PositionStatusOpen,
						OpenedAt: time.Now().Add(-time.Hour),
					},
					Live: &port.PositionLive{
						BrokerPositionID: "broker-pos-X",
						TPOrderID:        "tp-leg-1",
						SLOrderID:        "sl-leg-1",
					},
				})
				// Refresh rec to point at the position-with-legs row.
				open, _ := pos.ListOpenOrClosing(context.Background(), "USD_JPY")
				for _, p := range open {
					if live, _ := pos.GetLive(context.Background(), p.ID); live != nil && live.TPOrderID == "tp-leg-1" {
						rec = p
						break
					}
				}
			}
			flag := tempFlag(t)
			cmd := &ClosePositionCommand{
				Mode: tc.mode, Symbol: "USD_JPY", PipSize: 0.01,
				Broker: tc.brokerSetup(), Positions: pos, Closer: tc.closer,
				EmergencyFlagPath: flag, Logger: silentLogger(),
			}
			_, err := cmd.Execute(context.Background(), ClosePositionInput{PositionID: rec.ID})
			if err == nil {
				t.Fatal("expected critical error")
			}
			if _, ferr := os.Stat(flag); ferr != nil {
				t.Errorf("emergency_stop flag must be written; stat err=%v", ferr)
			}
		})
	}
}

func TestClosePositionCommand_PositionNotFound(t *testing.T) {
	pos := backtest.NewInMemoryPositionRepo()
	cmd := &ClosePositionCommand{
		Mode: config.ModePaperConfig, Symbol: "USD_JPY", PipSize: 0.01,
		Broker: &fakeBroker{}, Positions: pos, Closer: &fakeCloser{ok: true},
		EmergencyFlagPath: tempFlag(t), Logger: silentLogger(),
	}
	_, err := cmd.Execute(context.Background(), ClosePositionInput{PositionID: 999})
	if !errors.Is(err, ErrPositionNotFound) {
		t.Errorf("expected ErrPositionNotFound, got %v", err)
	}
}

// Live で ResolveExecution が成功 → ExitPrice が fillPx になることを別軸で確認。
// Insert with Live = TP/SL leg ids so the saga's cancelLiveSettleLegsForPosition
// finds them via PositionRepository.GetLive.
func TestClosePositionCommand_LiveMode_ResolveSucceeds_UsesActualFillPrice(t *testing.T) {
	pos := backtest.NewInMemoryPositionRepo()
	rec := port.PositionRecord{
		Symbol:           "USD_JPY",
		Side:             "BUY",
		Quantity:         100,
		EntryPrice:       100.0,
		TakeProfitPips:   20,
		StopLossPips:     15,
		MaxHoldMinutes:   240,
		StrategyConfigID: "cfg-test",
		Status:           port.PositionStatusOpen,
		OpenedAt:         time.Now().Add(-time.Hour),
	}
	id, err := pos.Insert(context.Background(), port.PositionInsertInput{
		Position: rec,
		Live: &port.PositionLive{
			BrokerPositionID: "gmo-pos-1",
			TPOrderID:        "tp-1",
			SLOrderID:        "sl-1",
		},
	})
	if err != nil {
		t.Fatalf("insert live position: %v", err)
	}
	rec.ID = id
	br := &fakeLiveBroker{
		fakeBroker:    fakeBroker{closePosResult: &order.Order{OrderID: "cls-1", Price: 0, Status: "ACCEPTED"}},
		resolveFillPx: 100.50,
		resolvePosID:  "gmo-pos-1",
	}
	cmd := &ClosePositionCommand{
		Mode: config.ModeLiveConfig, Symbol: "USD_JPY", PipSize: 0.01,
		Broker: br, Positions: pos, Closer: &fakeCloser{ok: true},
		EmergencyFlagPath: tempFlag(t), Logger: silentLogger(),
	}
	out, err := cmd.Execute(context.Background(), ClosePositionInput{PositionID: rec.ID})
	if err != nil {
		t.Fatalf("expected success, got err=%v", err)
	}
	if !nearly(out.ExitPrice, 100.50) {
		t.Errorf("ExitPrice should be from ResolveExecution; got %v want 100.50", out.ExitPrice)
	}
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 0.01 }
