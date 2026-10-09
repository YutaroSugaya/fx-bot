package command

import (
	"context"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/clock"
	"fx-bot/backend/internal/domain/order"
)

// usecase 層が time.Now() を直に呼ぶと clock mock
// できずテスト不能。Clock interface を注入できるようにし、close 時刻を
// FakeClock で制御できることを保証する。
//
// 旧実装: ClosePositionCommand.Execute() は `now := time.Now()` を直書き。
// 新実装: Clock フィールドを持ち、nil なら time.Now (= 互換)、設定済みなら
// Clock.Now() を呼ぶ。
func TestClosePositionCommand_UsesInjectedClockForCloseTimestamp(t *testing.T) {
	pos := backtest.NewInMemoryPositionRepo()
	rec := openPosition(t, pos, "BUY", 100.0)
	br := &fakeBroker{closePosResult: &order.Order{OrderID: "cls-1", Price: 100.50, Status: "FILLED"}}
	closer := &fakeCloser{ok: true}

	fixedNow := time.Date(2026, 5, 26, 12, 34, 56, 0, time.UTC)
	cmd := &ClosePositionCommand{
		Mode: config.ModePaperConfig, Symbol: "USD_JPY", PipSize: 0.01,
		Broker:    br,
		Positions: pos,
		Closer:    closer,
		Clock:     clock.NewFake(fixedNow),
		Mutex:     &sync.Mutex{}, EmergencyFlagPath: tempFlag(t), Logger: silentLogger(),
	}
	if _, err := cmd.Execute(context.Background(), ClosePositionInput{PositionID: rec.ID}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// ExecuteCloseSaga が CloseAndRecord に渡す closedAt が injected clock 由来か。
	if got := closer.gotTrade.ClosedAt; !got.Equal(fixedNow) {
		t.Errorf("trade.ClosedAt: got %v want %v", got, fixedNow)
	}
}

func TestClosePositionCommand_NilClockFallsBackToSystem(t *testing.T) {
	// 既存呼び出し (Clock 未設定) が壊れていないか — Execute は成功し、
	// ClosedAt が今近辺の値を持つこと (±5s で十分)。
	pos := backtest.NewInMemoryPositionRepo()
	rec := openPosition(t, pos, "BUY", 100.0)
	br := &fakeBroker{closePosResult: &order.Order{OrderID: "cls-1", Price: 100.50, Status: "FILLED"}}
	closer := &fakeCloser{ok: true}
	cmd := &ClosePositionCommand{
		Mode: config.ModePaperConfig, Symbol: "USD_JPY", PipSize: 0.01,
		Broker: br, Positions: pos, Closer: closer,
		// Clock: nil — 既存挙動 fallback
		Mutex: &sync.Mutex{}, EmergencyFlagPath: tempFlag(t), Logger: silentLogger(),
	}
	before := time.Now()
	if _, err := cmd.Execute(context.Background(), ClosePositionInput{PositionID: rec.ID}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	after := time.Now()
	got := closer.gotTrade.ClosedAt
	if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Errorf("ClosedAt (nil-clock fallback) %v not within [%v, %v]", got, before, after)
	}
}
