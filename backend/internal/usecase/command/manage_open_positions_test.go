package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/port"
)

// ---------- evaluateExit ----------

func TestEvaluateExit_PaperMode_TPHit_ReturnsTakeProfit(t *testing.T) {
	u := &ManageOpenPositions{Symbol: "USD_JPY", PipSize: 0.01, Mode: config.ModePaperConfig, Clock: time.Now}
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.00, TakeProfitPips: 20, StopLossPips: 20,
		MaxHoldMinutes: 240, OpenedAt: time.Now(),
	}
	tk := market.Ticker{Bid: 100.21, Ask: 100.22}
	got := u.evaluateExit(rec, tk, time.Now())
	if got != "take_profit" {
		t.Fatalf("paper mode TP should trigger; got %q", got)
	}
}

func TestEvaluateExit_LiveMode_TPHit_SkippedReturnsEmpty(t *testing.T) {
	u := &ManageOpenPositions{Symbol: "USD_JPY", PipSize: 0.01, Mode: config.ModeLiveConfig, Clock: time.Now}
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.00, TakeProfitPips: 20, StopLossPips: 20,
		MaxHoldMinutes: 240, OpenedAt: time.Now(),
	}
	tk := market.Ticker{Bid: 100.21, Ask: 100.22}
	got := u.evaluateExit(rec, tk, time.Now())
	if got != "" {
		t.Fatalf("live mode must NOT re-evaluate TP/SL (delegated to GMO OCO); got %q", got)
	}
}

func TestEvaluateExit_LiveMode_MaxHoldExpired_ReturnsMaxHold(t *testing.T) {
	u := &ManageOpenPositions{Symbol: "USD_JPY", PipSize: 0.01, Mode: config.ModeLiveConfig, Clock: time.Now}
	opened := time.Now().Add(-5 * time.Hour)
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.00, TakeProfitPips: 20, StopLossPips: 20,
		MaxHoldMinutes: 240, OpenedAt: opened,
	}
	tk := market.Ticker{Bid: 100.00, Ask: 100.01}
	got := u.evaluateExit(rec, tk, time.Now())
	if got != "max_hold" {
		t.Fatalf("live mode must still trigger max_hold; got %q", got)
	}
}

// MaxHold extension policy (asymmetric): within the extension
// window, evaluateExit returns "" (hold) as long as the position is NOT losing
// beyond the threshold — i.e. flat OR winning positions keep running so a
// trend can develop (ratchet handles the eventual winner exit). Only a loss
// worse than -threshold closes early at the soft deadline. The hard deadline
// (soft+extension) always closes.
func TestEvaluateExit_MaxHoldExtension(t *testing.T) {
	u := &ManageOpenPositions{Symbol: "USD_JPY", PipSize: 0.01, Mode: config.ModePaperConfig, Clock: time.Now}
	base := time.Now()

	cases := []struct {
		name       string
		elapsedMin int
		bid, ask   float64
		extMin     int
		extThr     float64
		want       string
	}{
		// Soft deadline only (no extension configured) — close on hit
		{"no extension at soft deadline closes", 240, 100.00, 100.01, 0, 0, "max_hold"},
		// Extension configured, within window, flat → wait
		{"flat at soft + 10min: wait", 250, 100.01, 100.02, 60, 5, ""},
		// ⑧: winning during extension → hold (let the trend run). BUY mid≈100.075 = +7.5 pips.
		{"winning during extension: hold (let it run)", 250, 100.07, 100.08, 60, 5, ""},
		// ⑧: losing beyond -threshold during extension → close. BUY mid≈99.925 = -7.5 pips < -5.
		{"losing beyond threshold during extension: close", 250, 99.92, 99.93, 60, 5, "max_hold"},
		// Past hard deadline regardless → close
		{"past hard deadline (soft+extension) closes regardless", 320, 100.01, 100.02, 60, 5, "max_hold"},
		// Before soft deadline → keep open (no max_hold yet)
		{"before soft deadline returns no max_hold", 230, 100.00, 100.01, 60, 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := port.PositionRecord{
				Side: "BUY", EntryPrice: 100.00,
				TakeProfitPips: 20, StopLossPips: 20, MaxHoldMinutes: 240,
				ExtensionMaxMinutes:              tc.extMin,
				ExtensionUnrealizedPipsThreshold: tc.extThr,
				OpenedAt:                         base,
			}
			now := base.Add(time.Duration(tc.elapsedMin) * time.Minute)
			tk := market.Ticker{Bid: tc.bid, Ask: tc.ask}
			got := u.evaluateExit(rec, tk, now)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// Early-exit policy: in a small window BEFORE
// the soft MaxHold deadline, lock in the "least bad" moment by closing as
// soon as unrealized PnL meets a target (e.g. -2 pips). The intent is to
// avoid the worst-case scenario where, within the final minutes, price
// drifts further into the red right before the deadline forces a worse
// MARKET close.
//
// Semantics:
//   - EarlyExitWindowMinutes > 0 enables the feature. Window starts at
//     OpenedAt + (MaxHoldMinutes - EarlyExitWindowMinutes).
//   - In the window, if PnL_pips >= EarlyExitTargetPips → close as
//     "early_exit" (distinct close_reason from "max_hold" so analytics can
//     separate early-window salvage exits from genuine soft/hard deadline
//     closes).
//   - Outside the window (too early), the feature is inert.
//   - At or after soft deadline, existing soft/hard/extension logic
//     unchanged.
func TestEvaluateExit_EarlyExit(t *testing.T) {
	u := &ManageOpenPositions{Symbol: "USD_JPY", PipSize: 0.01, Mode: config.ModePaperConfig, Clock: time.Now}
	base := time.Now()

	cases := []struct {
		name       string
		side       string
		elapsedMin int
		bid, ask   float64
		windowMin  int
		targetPips float64
		want       string
	}{
		// In window (last 10 min before soft=240), PnL=-1 ≥ target=-2 → close
		{"BUY in window, PnL=-1 meets target=-2: close",
			"BUY", 235, 99.985, 99.995, 10, -2.0, "early_exit"},
		// In window, PnL=-5 < target=-2 → hold (not good enough)
		{"BUY in window, PnL=-5 below target=-2: hold",
			"BUY", 235, 99.945, 99.955, 10, -2.0, ""},
		// Outside window (too early), even with great PnL → hold
		{"BUY before window starts: no early exit",
			"BUY", 225, 99.985, 99.995, 10, -2.0, ""},
		// Disabled (window=0) → feature inert
		{"window=0 disables early exit",
			"BUY", 235, 99.985, 99.995, 0, -2.0, ""},
		// At target boundary (PnL == target) → close (inclusive ≥)
		{"BUY in window, PnL=-2 exactly equals target=-2: close",
			"BUY", 235, 99.975, 99.985, 10, -2.0, "early_exit"},
		// In window, positive PnL → close (take what you can)
		{"BUY in window, PnL=+3: close",
			"BUY", 235, 100.025, 100.035, 10, -2.0, "early_exit"},
		// SELL side mirror
		{"SELL in window, PnL=-1 meets target=-2: close",
			"SELL", 235, 100.005, 100.015, 10, -2.0, "early_exit"},
		{"SELL in window, PnL=-5 below target: hold",
			"SELL", 235, 100.045, 100.055, 10, -2.0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := port.PositionRecord{
				Side: tc.side, EntryPrice: 100.00,
				TakeProfitPips: 30, StopLossPips: 20, MaxHoldMinutes: 240,
				EarlyExitWindowMinutes: tc.windowMin,
				EarlyExitTargetPips:    tc.targetPips,
				OpenedAt:               base,
			}
			now := base.Add(time.Duration(tc.elapsedMin) * time.Minute)
			tk := market.Ticker{Bid: tc.bid, Ask: tc.ask}
			got := u.evaluateExit(rec, tk, now)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// ---------- closeOne live mode ----------

func newLiveManager(t *testing.T, br *fakeLiveBroker, closer port.PositionCloser, flagPath string) *ManageOpenPositions {
	t.Helper()
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	mu := &sync.Mutex{}
	return &ManageOpenPositions{
		Broker:            br,
		Positions:         posRepo,
		Trades:            tradeRepo,
		Closer:            closer,
		Symbol:            "USD_JPY",
		PipSize:           0.01,
		Mode:              config.ModeLiveConfig,
		EmergencyFlagPath: flagPath,
		CloseMutex:        mu,
		Logger:            silentLogger(),
		Clock:             time.Now,
	}
}

// newLiveManagerWithRepo lets callers re-use a pre-seeded position repo so
// the saga's ClaimForClose (OPEN→CLOSING) sees the row inserted by the test.
func newLiveManagerWithRepo(t *testing.T, br *fakeLiveBroker, posRepo *backtest.InMemoryPositionRepo, closer port.PositionCloser, flagPath string) *ManageOpenPositions {
	t.Helper()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	mu := &sync.Mutex{}
	return &ManageOpenPositions{
		Broker:            br,
		Positions:         posRepo,
		Trades:            tradeRepo,
		Closer:            closer,
		Symbol:            "USD_JPY",
		PipSize:           0.01,
		Mode:              config.ModeLiveConfig,
		EmergencyFlagPath: flagPath,
		CloseMutex:        mu,
		Logger:            silentLogger(),
		Clock:             time.Now,
	}
}

// insertLiveOpenPosition inserts a Live OPEN position with pre-recorded
// settle leg ids (required for the close saga to cancel by
// orderId rather than mass-cancel the symbol).
func insertLiveOpenPosition(t *testing.T, repo *backtest.InMemoryPositionRepo, tpID, slID string) port.PositionRecord {
	t.Helper()
	rec := port.PositionRecord{
		Symbol:           "USD_JPY",
		Side:             "BUY",
		Quantity:         100,
		EntryPrice:       100.00,
		TakeProfitPips:   20,
		StopLossPips:     20,
		MaxHoldMinutes:   240,
		StrategyConfigID: "live-cfg",
		Status:           port.PositionStatusOpen,
		OpenedAt:         time.Now().Add(-time.Hour),
	}
	id, err := repo.Insert(context.Background(), port.PositionInsertInput{
		Position: rec,
		Live: &port.PositionLive{
			BrokerPositionID: "gmo-pos-1",
			TPOrderID:        tpID,
			SLOrderID:        slID,
		},
	})
	if err != nil {
		t.Fatalf("setup insert: %v", err)
	}
	rec.ID = id
	return rec
}

func tempFlagPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "emergency_stop.flag")
}

// Ratchet TP (trailing take-profit). evaluateExit は呼ぶ側 (OnTick) が
// PeakUnrealizedPips / RatchetArmed を current tick 値で更新済みの rec を
// 渡す前提で、armed && peak から GivebackPips 戻ったら "ratchet_takeprofit"
// を返す。Live / Paper どちらでも動く (TP/SL と違い broker OCO に依存しない)。
func TestEvaluateExit_Ratchet(t *testing.T) {
	base := time.Now()

	cases := []struct {
		name      string
		mode      config.Mode
		side      string
		entry     float64
		armPips   float64
		givePips  float64
		peakPips  float64 // OnTick が更新済みの peak (この tick で再計算しない)
		armed     bool    // 同上
		bid, ask  float64
		wantClose string
	}{
		// disabled — ratchet 0/0
		{"disabled: no fire", config.ModePaperConfig, "BUY", 100.0, 0, 0, 0, false, 100.10, 100.11, ""},

		// 未 arm: peak が arm 未満なので、戻ってきても fire しない
		{"not yet armed: no fire even if retraced", config.ModePaperConfig, "BUY", 100.0, 5, 3, 4.0, false, 100.00, 100.01, ""},

		// armed + giveback ちょうど: 発火
		// peak=11pips, current bid=100.08 → unrealized=8pips, retrace=3pips==giveback → fire
		{"armed + retrace == giveback: fires (BUY)", config.ModePaperConfig, "BUY", 100.0, 5, 3, 11.0, true, 100.08, 100.09, "ratchet_takeprofit"},

		// armed + 戻り過ぎ: 発火
		{"armed + retrace > giveback: fires (BUY)", config.ModePaperConfig, "BUY", 100.0, 5, 3, 11.0, true, 100.05, 100.06, "ratchet_takeprofit"},

		// armed + 戻り不足: 発火しない (peak はまだ更新中の可能性)
		// peak=11pips, current bid=100.09 → unrealized=9pips, retrace=2pips<3 → no fire
		{"armed + retrace < giveback: no fire", config.ModePaperConfig, "BUY", 100.0, 5, 3, 11.0, true, 100.09, 100.10, ""},

		// SELL 対称: SELL は entry より下げると profit。peak=11pips, current ask=99.92 → unrealized=8pips, retrace=3pips → fire
		{"SELL: armed + retrace == giveback: fires", config.ModePaperConfig, "SELL", 100.0, 5, 3, 11.0, true, 99.91, 99.92, "ratchet_takeprofit"},

		// **Live モードでも fire** (TP/SL と違い ratchet は OnTick で評価する)
		{"LIVE + armed + retrace: fires (NOT skipped like TP/SL)", config.ModeLiveConfig, "BUY", 100.0, 5, 3, 11.0, true, 100.08, 100.09, "ratchet_takeprofit"},

		// 既存 TP より ratchet が先に評価される (peak=20, TP=20, current bid=100.17 → unrealized=17pips, retrace=3pips → fire)
		{"ratchet fires before TP when both eligible", config.ModePaperConfig, "BUY", 100.0, 5, 3, 20.0, true, 100.17, 100.18, "ratchet_takeprofit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &ManageOpenPositions{Symbol: "USD_JPY", PipSize: 0.01, Mode: tc.mode, Clock: time.Now}
			rec := port.PositionRecord{
				Side:                tc.side,
				EntryPrice:          tc.entry,
				TakeProfitPips:      20, // TP は有効にしておくが ratchet が先に発火する想定
				StopLossPips:        20,
				MaxHoldMinutes:      240,
				OpenedAt:            base,
				RatchetArmPips:      tc.armPips,
				RatchetGivebackPips: tc.givePips,
				PeakUnrealizedPips:  tc.peakPips,
				RatchetArmed:        tc.armed,
			}
			tk := market.Ticker{Bid: tc.bid, Ask: tc.ask}
			got := u.evaluateExit(rec, tk, base.Add(time.Minute))
			if got != tc.wantClose {
				t.Errorf("evaluateExit: got %q want %q", got, tc.wantClose)
			}
		})
	}
}

// OnTick が ratchet state (peak / armed) を DB (UpdateRatchetState) に
// 永続化することを確認する。peak は monotonic increasing: 最初の tick で
// peak=8, 次に peak=11 に更新、retrace tick では更新しない (前回値以上で
// なければ DB call しない)。
func TestOnTick_RatchetStateGetsPersisted(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()
	base := time.Now()

	// open position with ratchet enabled
	rec := port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 158.918,
		TakeProfitPips: 30, StopLossPips: 20, MaxHoldMinutes: 240,
		RatchetArmPips: 5, RatchetGivebackPips: 3,
		StrategyConfigID: "ratchet-cfg",
		Status:           port.PositionStatusOpen,
		OpenedAt:         base.Add(-30 * time.Minute),
	}
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{Position: rec})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	u := &ManageOpenPositions{
		Broker:    &fakeBroker{},
		Positions: posRepo,
		Trades:    backtest.NewInMemoryTradeRepo(),
		Closer:    backtest.NewInMemoryPositionCloser(posRepo, backtest.NewInMemoryTradeRepo()),
		Symbol:    "USD_JPY",
		PipSize:   0.01,
		Mode:      config.ModePaperConfig,
		Logger:    silentLogger(),
		Clock:     func() time.Time { return base },
	}

	// tick 1: bid=158.998 → unrealized=+8pips. peak should become 8, armed=true (>=5)
	if err := u.OnTick(ctx, market.Ticker{Bid: 158.998, Ask: 159.008}); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	got, err := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if err != nil || len(got) != 1 {
		t.Fatalf("list after tick1: err=%v len=%d", err, len(got))
	}
	if got[0].ID != id {
		t.Fatalf("unexpected position id")
	}
	// IEEE 754 で entry/bid 引き算は 1e-12 級の drift が出るので近似比較。
	if diff := got[0].PeakUnrealizedPips - 8.0; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("peak after tick1: got %v want ~8.0", got[0].PeakUnrealizedPips)
	}
	if !got[0].RatchetArmed {
		t.Errorf("armed after tick1 (peak=8 >= arm=5): got false want true")
	}

	// tick 2: bid=159.028 → unrealized=+11pips. peak should advance to 11
	if err := u.OnTick(ctx, market.Ticker{Bid: 159.028, Ask: 159.038}); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	got, _ = posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if diff := got[0].PeakUnrealizedPips - 11.0; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("peak after tick2: got %v want ~11.0", got[0].PeakUnrealizedPips)
	}
}

// 損切り側 ratchet の end-to-end: 含み損が深くなって trough が -arm に達し
// (loss_armed=true)、その後 giveback だけ戻したら "ratchet_stoploss" で close。
// 満額 SL(-20)を待たずに浅い傷で撤退することを確認する。
func TestOnTick_LossRatchet_ClosesOnRecoveryFromTrough(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	base := time.Now()

	rec := port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 158.918,
		TakeProfitPips: 30, StopLossPips: 20, MaxHoldMinutes: 240,
		RatchetArmPips: 14, RatchetGivebackPips: 8, // mirror: arm 14 / give 8
		StrategyConfigID: "loss-ratchet-cfg",
		Status:           port.PositionStatusOpen,
		OpenedAt:         base.Add(-30 * time.Minute),
	}
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{Position: rec})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	u := &ManageOpenPositions{
		Broker:    &fakeBroker{},
		Positions: posRepo,
		Trades:    tradeRepo,
		Closer:    backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		Symbol:    "USD_JPY",
		PipSize:   0.01,
		Mode:      config.ModePaperConfig,
		Logger:    silentLogger(),
		Clock:     func() time.Time { return base },
	}

	// tick 1: bid=158.758 → unrealized=-16pips (< SL 20, まだ切れない)。
	// trough=-16 <= -arm14 → loss_armed=true。giveback 未達なので close しない。
	if err := u.OnTick(ctx, market.Ticker{Bid: 158.758, Ask: 158.768}); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	got, err := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if err != nil || len(got) != 1 {
		t.Fatalf("after tick1 expected still-open 1, err=%v len=%d", err, len(got))
	}
	if diff := got[0].TroughUnrealizedPips - (-16.0); diff > 1e-6 || diff < -1e-6 {
		t.Errorf("trough after tick1: got %v want ~-16.0", got[0].TroughUnrealizedPips)
	}
	if !got[0].LossRatchetArmed {
		t.Errorf("loss_armed after tick1 (trough=-16 <= -arm14): got false want true")
	}

	// tick 2: bid=158.838 → unrealized=-8pips = trough(-16) から +8 回復
	// → giveback 8 到達で "ratchet_stoploss" close。position は閉じる。
	if err := u.OnTick(ctx, market.Ticker{Bid: 158.838, Ask: 158.848}); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	openAfter, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if len(openAfter) != 0 {
		t.Fatalf("after tick2 expected closed (0 open), got %d", len(openAfter))
	}
	trades, err := tradeRepo.ListSince(ctx, base.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("list trades: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("expected 1 trade recorded, got %d", len(trades))
	}
	if trades[0].CloseReason != "ratchet_stoploss" {
		t.Errorf("close_reason: got %q want ratchet_stoploss", trades[0].CloseReason)
	}
	if trades[0].PositionID != id {
		t.Errorf("trade position id: got %d want %d", trades[0].PositionID, id)
	}
	// NOTE: 「満額 SL(-20)より浅い傷で切れる」= ratchet が -8pips の tick で
	// 発火する、という fire 閾値そのものは TestEvaluateLossRatchetExit_* が
	// pips 単位で厳密に検証している。ここで記録 pnl の絶対値は検証しない
	// (paper close saga の fill-price 解決に依存し、この test の主眼ではない)。
}

func TestCloseOne_LiveMode_CancelsSettleLegsBeforeBrokerClose(t *testing.T) {
	br := &fakeLiveBroker{
		// Live close MUST cancel only the recorded TP/SL legs
		// of this specific position — not every active order on the symbol.
		// resolveFillPx is the price ResolveExecution will return.
		fakeBroker:    fakeBroker{closePosResult: &order.Order{OrderID: "close-1", Price: 0, Status: "ACCEPTED"}},
		resolveFillPx: 100.10,
		resolvePosID:  "gmo-pos-1",
	}
	posRepo := backtest.NewInMemoryPositionRepo()
	rec := insertLiveOpenPosition(t, posRepo, "tp-leg-1", "sl-leg-1")
	closer := backtest.NewInMemoryPositionCloser(posRepo, backtest.NewInMemoryTradeRepo())
	u := newLiveManagerWithRepo(t, br, posRepo, closer, tempFlagPath(t))

	u.closeOne(context.Background(), rec, market.Ticker{Bid: 100.0, Ask: 100.01}, "max_hold", time.Now())

	// GetActiveOrders is NEVER called during close — we use the
	// pre-recorded TPOrderID / SLOrderID instead.
	if br.getActiveOrdersCalls != 0 {
		t.Errorf("GetActiveOrders must NOT be called during close (use recorded leg ids); got %d", br.getActiveOrdersCalls)
	}
	if len(br.cancelledOrderIDs) != 2 || br.cancelledOrderIDs[0] != "tp-leg-1" || br.cancelledOrderIDs[1] != "sl-leg-1" {
		t.Errorf("expected TP then SL cancelled by recorded ids; got %v", br.cancelledOrderIDs)
	}
	if br.closePosCalls != 1 {
		t.Errorf("broker.ClosePosition should be called once; got %d", br.closePosCalls)
	}
}

// TestCloseOne_EmergencyPaths は live モードで closeOne が emergency_stop
// を発火する全経路を 1 table に統合。
//
// GetActiveOrders is not called during close (the saga
// uses recorded TP/SL leg ids). Failure modes therefore shift:
//   - "missing recorded legs" → reject before any broker call
//   - "CancelOrder fails"     → still reject before broker.ClosePosition
//   - "broker close fails"    → naked position → critical
//   - "CloseAndRecord errs"   → DB/broker divergence → critical
//   - "CloseAndRecord ok=false" → race detected → critical
func TestCloseOne_EmergencyPaths(t *testing.T) {
	cases := []struct {
		name              string
		broker            *fakeLiveBroker
		tpID, slID        string              // recorded settle leg ids on the position
		closer            port.PositionCloser // saga-compatible closer
		wantClosePosCalls int                 // broker.ClosePosition が呼ばれるべき回数
		check             func(t *testing.T, fc *fakeCloser, ic *backtest.InMemoryPositionCloser)
	}{
		// The old "missing recorded settle legs aborts saga" case was removed.
		// Under the current contract, missing legs trigger discovery + soft skip
		// (close proceeds; GMO auto-cancels OCO orphans on close). The
		// happy-path version of that contract is verified separately in
		// TestCloseOne_LiveMode_MissingLegs_StillClosesViaMarketCloseFallback
		// below.
		{
			name: "CancelOrder fails before broker close",
			broker: &fakeLiveBroker{
				fakeBroker: fakeBroker{cancelOrderErr: errors.New("order not found")},
			},
			tpID:              "tp-1",
			slID:              "sl-1",
			closer:            &fakeCloser{ok: true},
			wantClosePosCalls: 0,
		},
		{
			name: "ClosePosition fails after settle legs cancelled (H1 audit)",
			broker: &fakeLiveBroker{
				fakeBroker: fakeBroker{closePosErr: errors.New("broker 500")},
			},
			tpID:              "tp-1",
			slID:              "sl-1",
			closer:            &fakeCloser{ok: true},
			wantClosePosCalls: 1,
		},
		{
			name: "DB error from CloseAndRecord (broker CLOSED, DB stuck CLOSING)",
			broker: &fakeLiveBroker{
				fakeBroker:    fakeBroker{closePosResult: &order.Order{OrderID: "close-1", Price: 0, Status: "ACCEPTED"}},
				resolveFillPx: 100.10,
				resolvePosID:  "gmo-pos-1",
			},
			tpID:              "tp-1",
			slID:              "sl-1",
			closer:            &fakeCloser{err: errors.New("db down")},
			wantClosePosCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flag := tempFlagPath(t)
			posRepo := backtest.NewInMemoryPositionRepo()
			rec := insertLiveOpenPosition(t, posRepo, tc.tpID, tc.slID)
			u := newLiveManagerWithRepo(t, tc.broker, posRepo, tc.closer, flag)

			u.closeOne(context.Background(), rec, market.Ticker{Bid: 100, Ask: 100.01}, "max_hold", time.Now())

			if tc.broker.closePosCalls != tc.wantClosePosCalls {
				t.Errorf("broker.ClosePosition calls: got %d want %d", tc.broker.closePosCalls, tc.wantClosePosCalls)
			}
			if _, err := os.Stat(flag); err != nil {
				t.Errorf("emergency_stop flag must be written; stat err=%v", err)
			}
		})
	}
}

// Contract: when a position has no recorded TP/SL leg ids (=
// external_broker adoption or bot orphan recovered by reconcile), the close
// saga must STILL proceed to market-close. Discovery may or may not turn
// up legs (depends on whether the user attached OCO via the GMO app); the
// invariant is "close fires + no emergency_stop trip".
func TestCloseOne_LiveMode_MissingLegs_StillClosesViaMarketCloseFallback(t *testing.T) {
	flag := tempFlagPath(t)
	posRepo := backtest.NewInMemoryPositionRepo()
	rec := insertLiveOpenPosition(t, posRepo, "", "") // empty leg ids
	br := &fakeLiveBroker{
		fakeBroker: fakeBroker{
			closePosResult: &order.Order{OrderID: "cls-1", Price: 0, Status: "ACCEPTED"},
		},
		resolveFillPx: 100.10,
		resolvePosID:  "gmo-pos-1",
		// Whatever the SettleLegResolver returns, the close must still fire
		// — exercise the discovery branch (default fakeLiveBroker hands back
		// "tp-leg" / "sl-leg" placeholders, which the saga then cancels).
	}
	closer := backtest.NewInMemoryPositionCloser(posRepo, backtest.NewInMemoryTradeRepo())
	u := newLiveManagerWithRepo(t, br, posRepo, closer, flag)

	u.closeOne(context.Background(), rec, market.Ticker{Bid: 100, Ask: 100.01}, "manual", time.Now())

	if br.closePosCalls != 1 {
		t.Errorf("market close must still fire when legs are missing; got %d ClosePosition calls", br.closePosCalls)
	}
	if _, err := os.Stat(flag); err == nil {
		t.Errorf("emergency_stop must NOT be tripped on the no-legs-but-close-ok path")
	}
}

// In Live mode, max_hold close must use ResolveExecution to
// get the actual fill price — not the local ticker. Test that the recorded
// trade carries the resolver's price, not Bid/Ask.
func TestCloseOne_LiveMode_MaxHoldUsesResolveExecutionExitPrice(t *testing.T) {
	br := &fakeLiveBroker{
		fakeBroker:    fakeBroker{closePosResult: &order.Order{OrderID: "close-1", Price: 0, Status: "ACCEPTED"}},
		resolveFillPx: 100.07, // intentionally different from ticker Bid=100.10
		resolvePosID:  "gmo-pos-1",
	}
	closer := &fakeCloser{ok: true}
	posRepo := backtest.NewInMemoryPositionRepo()
	rec := insertLiveOpenPosition(t, posRepo, "tp-1", "sl-1")
	u := newLiveManagerWithRepo(t, br, posRepo, closer, tempFlagPath(t))

	u.closeOne(context.Background(), rec, market.Ticker{Bid: 100.10, Ask: 100.11}, "max_hold", time.Now())

	if closer.gotTrade.ExitPrice != 100.07 {
		t.Errorf("Live close exit price must come from ResolveExecution; got %v want 100.07", closer.gotTrade.ExitPrice)
	}
	if closer.gotTrade.CloseReason != "max_hold" {
		t.Errorf("close reason mismatch; got %q", closer.gotTrade.CloseReason)
	}
}

// --- shadow 時間ストップ計測 (発動しない計測のみ) ---
//
// 「長時間 hold は早期手仕舞いすべきか」をデータで決めるため、建玉が
// 6h / 8h を跨いだ最初の tick で mid 基準含み pips を journal に 1 回だけ記録する
// (event=shadow_timestop / stage=mark_6h・mark_8h)。ポジション操作は一切しない。
func TestOnTick_ShadowTimestopMarks_JournaledOncePerThreshold(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()
	base := time.Now()

	rec := port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 158.918,
		TakeProfitPips: 30, StopLossPips: 15, MaxHoldMinutes: 1440,
		StrategyConfigID: "v83-cfg",
		Status:           port.PositionStatusOpen,
		OpenedAt:         base.Add(-6*time.Hour - time.Minute), // 6h1m 経過
	}
	if _, err := posRepo.Insert(ctx, port.PositionInsertInput{Position: rec}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	j := &recordingJournal{}
	u := &ManageOpenPositions{
		Broker:    &fakeBroker{},
		Positions: posRepo,
		Trades:    backtest.NewInMemoryTradeRepo(),
		Closer:    backtest.NewInMemoryPositionCloser(posRepo, backtest.NewInMemoryTradeRepo()),
		Symbol:    "USD_JPY",
		PipSize:   0.01,
		Mode:      config.ModePaperConfig,
		Logger:    silentLogger(),
		Clock:     func() time.Time { return base },
		Journal:   j,
	}

	// tick 1 (6h1m): mark_6h を 1 回記録。mid = (158.968+158.978)/2 = 158.973 → +5.5p
	if err := u.OnTick(ctx, market.Ticker{Bid: 158.968, Ask: 158.978}); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if len(j.entries) != 1 {
		t.Fatalf("want 1 shadow entry after tick1, got %d: %+v", len(j.entries), j.entries)
	}
	e := j.entries[0]
	if e.Event != "shadow_timestop" || e.Stage != "mark_6h" || e.Symbol != "USD_JPY" || e.Side != "BUY" {
		t.Errorf("entry: %+v", e)
	}
	if !strings.Contains(e.Reason, "+5.5") {
		t.Errorf("Reason must carry mid-based unrealized pips (+5.5), got %q", e.Reason)
	}

	// tick 2 (同 6h 帯): 二重記録しない。
	if err := u.OnTick(ctx, market.Ticker{Bid: 158.968, Ask: 158.978}); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if len(j.entries) != 1 {
		t.Fatalf("mark_6h must be journaled once, got %d entries", len(j.entries))
	}

	// tick 3 (8h2m 経過相当に時計を進める): mark_8h を追加で 1 回。
	u.Clock = func() time.Time { return base.Add(2*time.Hour + time.Minute) }
	if err := u.OnTick(ctx, market.Ticker{Bid: 158.968, Ask: 158.978}); err != nil {
		t.Fatalf("tick3: %v", err)
	}
	if len(j.entries) != 2 || j.entries[1].Stage != "mark_8h" {
		t.Fatalf("want mark_8h as 2nd entry, got %+v", j.entries)
	}
}
