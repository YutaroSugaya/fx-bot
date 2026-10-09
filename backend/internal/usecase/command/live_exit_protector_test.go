package command

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"fx-bot/backend/internal/domain/order"
)

// LiveExitProtector は Live エントリの「MARKET 約定後」フェーズ:
//   1. ResolveExecution → 実 positionId + 実 fill price
//   2. ComputeTPSLPrices → TP/SL の絶対価格
//   3. PlaceSettleOCO → OCO 親注文
//   4. ResolveSettleLegs → tp/sl leg orderIds (soft fail OK)
//
// を 1 つに集約する。manual_trade.go と execute_order.go で同一ロジックが
// 重複していた。本テストはそれぞれの分岐を網羅する。
//
// 命名: prefix は "live_exit_<...>" reason を発行。manual / auto の呼び出し側は
// `Source` で識別ラベルを付与 (= "manual_..." / "execute_..." reason prefix が
// 旧 emergency_stop ログと一致するため互換)。

func newProtectorForTest(t *testing.T, source string, br *fakeLiveBroker) (*LiveExitProtector, string) {
	t.Helper()
	flag := tempFlag(t)
	return &LiveExitProtector{
		Broker:            br,
		Symbol:            "USD_JPY",
		PipSize:           0.01,
		Source:            source,
		EmergencyFlagPath: flag,
		Logger:            silentLogger(),
	}, flag
}

func TestLiveExitProtector_HappyPath(t *testing.T) {
	br := &fakeLiveBroker{
		resolvePosID:  "100",
		resolveFillPx: 150.05,
	}
	p, flag := newProtectorForTest(t, "manual", br)

	got, err := p.Attach(context.Background(), LiveExitProtectionInput{
		Order:          &order.Order{OrderID: "ord-1"},
		Side:           order.SideBuy,
		Quantity:       100,
		TakeProfitPips: 20,
		StopLossPips:   15,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.EntryPrice != 150.05 {
		t.Errorf("EntryPrice: got %v want 150.05", got.EntryPrice)
	}
	if got.BrokerPositionID != "100" {
		t.Errorf("BrokerPositionID: got %q want 100", got.BrokerPositionID)
	}
	if got.TPOrderID != "tp-leg" || got.SLOrderID != "sl-leg" {
		t.Errorf("leg ids: tp=%q sl=%q (want tp-leg/sl-leg)", got.TPOrderID, got.SLOrderID)
	}
	if _, statErr := os.Stat(flag); statErr == nil {
		t.Errorf("emergency flag should NOT be written on happy path")
	}
	if br.settleOCOCalls != 1 {
		t.Errorf("expected 1 PlaceSettleOCO call; got %d", br.settleOCOCalls)
	}
	// TP/SL absolute prices must be derived from fill price (150.05), not the
	// pre-trade estimate. BUY: TP = 150.05 + 20*0.01 = 150.25, SL = 150.05 - 15*0.01 = 149.90
	if br.settleOCOLastArgs.TPPrice < 150.249 || br.settleOCOLastArgs.TPPrice > 150.251 {
		t.Errorf("TP from fill: got %v want ~150.25", br.settleOCOLastArgs.TPPrice)
	}
	if br.settleOCOLastArgs.SLPrice < 149.899 || br.settleOCOLastArgs.SLPrice > 149.901 {
		t.Errorf("SL from fill: got %v want ~149.90", br.settleOCOLastArgs.SLPrice)
	}
}

func TestLiveExitProtector_ResolveExecutionFails_Critical(t *testing.T) {
	br := &fakeLiveBroker{resolveErr: errors.New("resolve timeout")}
	p, flag := newProtectorForTest(t, "manual", br)

	_, err := p.Attach(context.Background(), LiveExitProtectionInput{
		Order:    &order.Order{OrderID: "ord-1"},
		Side:     order.SideBuy,
		Quantity: 100, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err == nil {
		t.Fatal("expected critical error from resolve failure")
	}
	if !strings.Contains(err.Error(), "resolve_execution") {
		t.Errorf("error should mention resolve_execution; got %v", err)
	}
	if _, statErr := os.Stat(flag); statErr != nil {
		t.Errorf("emergency flag must be written on critical failure: %v", statErr)
	}
	if br.settleOCOCalls != 0 {
		t.Errorf("must NOT call PlaceSettleOCO after resolve failure; got %d calls", br.settleOCOCalls)
	}
}

func TestLiveExitProtector_PlaceSettleOCOFails_CompensatesByMarketClose(t *testing.T) {
	br := &fakeLiveBroker{
		resolvePosID:  "100",
		resolveFillPx: 150.05,
		settleOCOErr:  errors.New("gmo oco rejected"),
	}
	p, flag := newProtectorForTest(t, "manual", br)

	_, err := p.Attach(context.Background(), LiveExitProtectionInput{
		Order:    &order.Order{OrderID: "ord-1"},
		Side:     order.SideBuy,
		Quantity: 100, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err == nil {
		t.Fatal("expected error after OCO failure + compensation")
	}
	if !strings.Contains(err.Error(), "rolled back") && !strings.Contains(err.Error(), "compensat") {
		t.Errorf("error should indicate rollback/compensation; got %v", err)
	}
	if br.closePosCalls != 1 {
		t.Errorf("expected 1 compensating ClosePosition call; got %d", br.closePosCalls)
	}
	// Compensating close succeeded → emergency flag must NOT be tripped
	// (broker side is clean, no protection lost).
	if _, statErr := os.Stat(flag); statErr == nil {
		t.Errorf("compensation success: flag should NOT be written")
	}
}

func TestLiveExitProtector_PlaceSettleOCOFails_AndCompensateFails_Critical(t *testing.T) {
	br := &fakeLiveBroker{
		resolvePosID:  "100",
		resolveFillPx: 150.05,
		settleOCOErr:  errors.New("oco rejected"),
	}
	br.fakeBroker.closePosErr = errors.New("close also failed")
	p, flag := newProtectorForTest(t, "manual", br)

	_, err := p.Attach(context.Background(), LiveExitProtectionInput{
		Order:    &order.Order{OrderID: "ord-1"},
		Side:     order.SideBuy,
		Quantity: 100, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err == nil {
		t.Fatal("expected critical error when both OCO and compensate fail")
	}
	if !strings.Contains(err.Error(), "compensate_close_failed") {
		t.Errorf("error should mention compensate_close_failed; got %v", err)
	}
	if _, statErr := os.Stat(flag); statErr != nil {
		t.Errorf("double-failure: emergency flag must be written; stat err=%v", statErr)
	}
}

func TestLiveExitProtector_ResolveSettleLegsSoftFails_EmptyIDsReturned(t *testing.T) {
	br := &fakeLiveBroker{
		resolvePosID:  "100",
		resolveFillPx: 150.05,
		settleLegsErr: errors.New("legs lookup timeout"),
	}
	p, flag := newProtectorForTest(t, "manual", br)

	got, err := p.Attach(context.Background(), LiveExitProtectionInput{
		Order:    &order.Order{OrderID: "ord-1"},
		Side:     order.SideBuy,
		Quantity: 100, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err != nil {
		t.Fatalf("leg resolve soft failure must NOT be a hard error; got %v", err)
	}
	if got.TPOrderID != "" || got.SLOrderID != "" {
		t.Errorf("soft failure should yield empty leg ids; got tp=%q sl=%q", got.TPOrderID, got.SLOrderID)
	}
	if got.EntryPrice != 150.05 || got.BrokerPositionID != "100" {
		t.Errorf("entry data must still propagate; got %+v", got)
	}
	if _, statErr := os.Stat(flag); statErr == nil {
		t.Errorf("soft failure should NOT trip emergency_stop")
	}
}

func TestLiveExitProtector_BrokerNotLiveCapable_Critical(t *testing.T) {
	// Plain fakeBroker (not fakeLiveBroker) → no ExecutionResolver interface
	br := &fakeBroker{}
	flag := tempFlag(t)
	p := &LiveExitProtector{
		Broker:            br,
		Symbol:            "USD_JPY",
		PipSize:           0.01,
		Source:            "manual",
		EmergencyFlagPath: flag,
		Logger:            silentLogger(),
	}
	_, err := p.Attach(context.Background(), LiveExitProtectionInput{
		Order:    &order.Order{OrderID: "ord-1"},
		Side:     order.SideBuy,
		Quantity: 100, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err == nil {
		t.Fatal("plain broker should fail (no ExecutionResolver)")
	}
	if !strings.Contains(err.Error(), "resolver") {
		t.Errorf("error should mention missing resolver; got %v", err)
	}
	if _, statErr := os.Stat(flag); statErr != nil {
		t.Errorf("emergency flag must be written: %v", statErr)
	}
}

func TestLiveExitProtector_NonNumericBrokerPositionID_Critical(t *testing.T) {
	br := &fakeLiveBroker{
		resolvePosID:  "not-a-number",
		resolveFillPx: 150.05,
	}
	p, flag := newProtectorForTest(t, "manual", br)

	_, err := p.Attach(context.Background(), LiveExitProtectionInput{
		Order:    &order.Order{OrderID: "ord-1"},
		Side:     order.SideBuy,
		Quantity: 100, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err == nil {
		t.Fatal("expected critical error for non-numeric broker position id")
	}
	if !strings.Contains(err.Error(), "numeric") && !strings.Contains(err.Error(), "broker_position_id") {
		t.Errorf("error should mention numeric or broker_position_id; got %v", err)
	}
	if _, statErr := os.Stat(flag); statErr != nil {
		t.Errorf("emergency flag must be written: %v", statErr)
	}
}

func TestLiveExitProtector_SourceAffectsReasonPrefix(t *testing.T) {
	// "execute" source → reason prefix "execute_..." (= ExecuteOrder の旧ログと一致)
	br := &fakeLiveBroker{resolveErr: errors.New("boom")}
	p, _ := newProtectorForTest(t, "execute", br)

	_, err := p.Attach(context.Background(), LiveExitProtectionInput{
		Order:    &order.Order{OrderID: "ord-1"},
		Side:     order.SideBuy,
		Quantity: 100, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err == nil || !strings.Contains(err.Error(), "execute_") {
		t.Errorf("execute source should prefix reason with execute_; got %v", err)
	}
}
