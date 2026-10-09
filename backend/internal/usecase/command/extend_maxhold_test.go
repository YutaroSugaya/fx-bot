package command

import (
	"context"
	"errors"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/domain/clock"
	"fx-bot/backend/internal/port"
)

// openForExtend は ExtendMaxHold テスト用に OPEN position を 1 件 seed する。
// OpenedAt を呼び出し側で固定できるよう引数に取る (deadline/remaining の
// 期待値を決め打ちするため)。
func openForExtend(t *testing.T, repo *backtest.InMemoryPositionRepo, maxHold int, openedAt time.Time) port.PositionRecord {
	t.Helper()
	rec := port.PositionRecord{
		Symbol:           "USD_JPY",
		Side:             "BUY",
		Quantity:         1000,
		EntryPrice:       159.996,
		TakeProfitPips:   20,
		StopLossPips:     15,
		MaxHoldMinutes:   maxHold,
		StrategyConfigID: "cfg-test",
		Status:           port.PositionStatusOpen,
		OpenedAt:         openedAt,
	}
	id, err := repo.Insert(context.Background(), port.PositionInsertInput{Position: rec, Manual: true})
	if err != nil {
		t.Fatalf("setup insert: %v", err)
	}
	rec.ID = id
	return rec
}

// TestExtendMaxHoldCommand_HappyPath: OPEN position を +60 分延長すると
// max_hold_minutes が加算され、deadline/remaining が新値で再計算される。
func TestExtendMaxHoldCommand_HappyPath(t *testing.T) {
	now := time.Date(2026, 6, 3, 3, 22, 0, 0, time.UTC)
	repo := backtest.NewInMemoryPositionRepo()
	// 100 分前に開いた、上限 240 分の position。+60 → 上限 300、
	// deadline = OpenedAt+300 = now+200、remaining = 200。
	rec := openForExtend(t, repo, 240, now.Add(-100*time.Minute))

	cmd := &ExtendMaxHoldCommand{Positions: repo, Clock: clock.NewFake(now)}
	out, err := cmd.Execute(context.Background(), ExtendMaxHoldInput{PositionID: rec.ID, AddMinutes: 60})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out.PositionID != rec.ID {
		t.Errorf("PositionID: got %d want %d", out.PositionID, rec.ID)
	}
	if out.MaxHoldMinutes != 300 {
		t.Errorf("MaxHoldMinutes: got %d want 300", out.MaxHoldMinutes)
	}
	if out.AddedMinutes != 60 {
		t.Errorf("AddedMinutes: got %d want 60", out.AddedMinutes)
	}
	wantDeadline := rec.OpenedAt.Add(300 * time.Minute)
	if !out.DeadlineAt.Equal(wantDeadline) {
		t.Errorf("DeadlineAt: got %v want %v", out.DeadlineAt, wantDeadline)
	}
	if out.RemainingMinutes != 200 {
		t.Errorf("RemainingMinutes: got %v want 200", out.RemainingMinutes)
	}

	// DB 側にも反映されていること (次 tick が読む値)。
	got, _ := repo.ListOpenOrClosing(context.Background(), "USD_JPY")
	if len(got) != 1 || got[0].MaxHoldMinutes != 300 {
		t.Errorf("persisted max_hold: got %+v want 300", got)
	}
}

// TestExtendMaxHoldCommand_NotFound: 存在しない ID は ErrPositionNotFound。
func TestExtendMaxHoldCommand_NotFound(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	cmd := &ExtendMaxHoldCommand{Positions: repo, Clock: clock.NewFake(time.Now())}
	_, err := cmd.Execute(context.Background(), ExtendMaxHoldInput{PositionID: 999, AddMinutes: 60})
	if !errors.Is(err, ErrPositionNotFound) {
		t.Fatalf("got %v want ErrPositionNotFound", err)
	}
}

// TestExtendMaxHoldCommand_ClosingIsNotFound: CLOSING 中の position は
// 延長対象外 (status='OPEN' のみ) → ErrPositionNotFound。
func TestExtendMaxHoldCommand_ClosingIsNotFound(t *testing.T) {
	now := time.Now()
	repo := backtest.NewInMemoryPositionRepo()
	rec := openForExtend(t, repo, 240, now)
	if ok, err := repo.ClaimForClose(context.Background(), rec.ID, now); err != nil || !ok {
		t.Fatalf("claim for close: ok=%v err=%v", ok, err)
	}
	cmd := &ExtendMaxHoldCommand{Positions: repo, Clock: clock.NewFake(now)}
	_, err := cmd.Execute(context.Background(), ExtendMaxHoldInput{PositionID: rec.ID, AddMinutes: 60})
	if !errors.Is(err, ErrPositionNotFound) {
		t.Fatalf("got %v want ErrPositionNotFound", err)
	}
}

// TestExtendMaxHoldCommand_InvalidMinutes: 範囲外の add_minutes は弾く。
func TestExtendMaxHoldCommand_InvalidMinutes(t *testing.T) {
	now := time.Now()
	for _, add := range []int{0, -10, MaxExtendMinutes + 1} {
		repo := backtest.NewInMemoryPositionRepo()
		rec := openForExtend(t, repo, 240, now)
		cmd := &ExtendMaxHoldCommand{Positions: repo, Clock: clock.NewFake(now)}
		_, err := cmd.Execute(context.Background(), ExtendMaxHoldInput{PositionID: rec.ID, AddMinutes: add})
		if !errors.Is(err, ErrInvalidExtendMinutes) {
			t.Errorf("add=%d: got %v want ErrInvalidExtendMinutes", add, err)
		}
		// 範囲外なら DB は不変であること。
		got, _ := repo.ListOpenOrClosing(context.Background(), "USD_JPY")
		if got[0].MaxHoldMinutes != 240 {
			t.Errorf("add=%d: max_hold mutated to %d, want unchanged 240", add, got[0].MaxHoldMinutes)
		}
	}
}
