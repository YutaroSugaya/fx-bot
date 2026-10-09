package command

import (
	"context"
	"errors"
	"testing"

	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/safety"
)

// LiveExitProtector の reconcile race 対策: ResolveExecution が positionId を
// 取った直後に PendingTracker.MarkPending を呼び、reconcile race-window を
// ガードする。Resolve / DB INSERT が完了するまで reconcile は当該 positionId を
// adopt 対象から除外する。

func TestLiveExitProtector_HappyPath_MarksPositionPending(t *testing.T) {
	br := &fakeLiveBroker{
		resolvePosID:  "1000001",
		resolveFillPx: 150.05,
	}
	tracker := safety.NewPendingPositions()
	p, _ := newProtectorForTest(t, "manual", br)
	p.PendingTracker = tracker

	_, err := p.Attach(context.Background(), LiveExitProtectionInput{
		Order:    &order.Order{OrderID: "ord-1"},
		Side:     order.SideBuy,
		Quantity: 100, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	// happy path: caller がまだ MarkResolved を呼んでいない状態を再現するため、
	// Attach 戻り直後の時点で IsPending が true であることを要求。
	if !tracker.IsPending("1000001") {
		t.Errorf("Attach must MarkPending(POS-A); IsPending=false")
	}
}

func TestLiveExitProtector_PlaceSettleOCOFails_MarksThenResolves(t *testing.T) {
	// OCO 失敗 → compensating close path。emergency_stop は焚かないが、
	// positionId は broker 側で既に close 済みなので、tracker からも解除する必要がある。
	br := &fakeLiveBroker{
		resolvePosID:  "1000002",
		resolveFillPx: 150.05,
		settleOCOErr:  errors.New("gmo oco rejected"),
	}
	tracker := safety.NewPendingPositions()
	p, _ := newProtectorForTest(t, "manual", br)
	p.PendingTracker = tracker

	_, err := p.Attach(context.Background(), LiveExitProtectionInput{
		Order:    &order.Order{OrderID: "ord-1"},
		Side:     order.SideBuy,
		Quantity: 100, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err == nil {
		t.Fatal("OCO failure should produce rolled-back error")
	}
	if tracker.IsPending("1000002") {
		t.Errorf("compensated entry: tracker must be cleared (IsPending=true is leak)")
	}
}

func TestLiveExitProtector_ResolveExecutionFails_TrackerNotTouched(t *testing.T) {
	// ResolveExecution 失敗 = positionId 未取得 → MarkPending は呼ばれてはいけない。
	br := &fakeLiveBroker{resolveErr: errors.New("resolve timeout")}
	tracker := safety.NewPendingPositions()
	p, _ := newProtectorForTest(t, "manual", br)
	p.PendingTracker = tracker

	_, err := p.Attach(context.Background(), LiveExitProtectionInput{
		Order:    &order.Order{OrderID: "ord-1"},
		Side:     order.SideBuy,
		Quantity: 100, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err == nil {
		t.Fatal("expected resolve failure")
	}
	// positionId が無いので tracker には何も入っていないはず (空文字 no-op が
	// MarkPending に渡っても IsPending("") は false なので一貫)。
	if tracker.IsPending("") {
		t.Errorf("empty id must never be IsPending=true")
	}
}

func TestLiveExitProtector_NilTracker_NoPanic(t *testing.T) {
	// PendingTracker が未配線でも従来動作を維持する。
	br := &fakeLiveBroker{resolvePosID: "1000003", resolveFillPx: 150.05}
	p, _ := newProtectorForTest(t, "manual", br)
	// p.PendingTracker = nil 明示

	_, err := p.Attach(context.Background(), LiveExitProtectionInput{
		Order:    &order.Order{OrderID: "ord-1"},
		Side:     order.SideBuy,
		Quantity: 100, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err != nil {
		t.Fatalf("nil tracker should not break happy path: %v", err)
	}
}
