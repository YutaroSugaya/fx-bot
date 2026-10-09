package query

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/port"
)

// Query must not return port.TradeRecord directly. Adapt rows
// into a TradeView DTO with JSON-friendly snake_case field names so the API
// payload is decoupled from the storage schema.

func TestListTradesQuery_ReturnsTradeView_NotPortRecord(t *testing.T) {
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	repo := backtest.NewInMemoryTradeRepo()
	_ = repo.Insert(context.Background(), port.TradeRecord{
		PositionID: 42, SignalID: "sig-1", StrategyConfigID: "cfg-x",
		Symbol: "USD_JPY", Side: "BUY", Quantity: 100,
		EntryPrice: 150.10, ExitPrice: 150.30,
		ProfitLossPips: 2.0, ProfitLossJPY: 200,
		CloseReason: "take_profit",
		OpenedAt:    now.Add(-time.Hour), ClosedAt: now.Add(-30 * time.Minute),
	})

	q := &ListTradesQuery{Trades: repo, Clock: func() time.Time { return now }}
	got, err := q.Execute(context.Background(), ListTradesInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	// Compile-time guarantee: result is []TradeView, not []port.TradeRecord.
	var _ []TradeView = got

	tv := got[0]
	if tv.PositionID != 42 {
		t.Errorf("PositionID: got %d want 42", tv.PositionID)
	}
	if tv.CloseReason != "take_profit" {
		t.Errorf("CloseReason: got %q want take_profit", tv.CloseReason)
	}
	if tv.ProfitLossJPY != 200 {
		t.Errorf("ProfitLossJPY: got %v want 200", tv.ProfitLossJPY)
	}

	// JSON encoding must use snake_case keys (= the API contract).
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	js := string(raw)
	for _, want := range []string{
		`"position_id":42`,
		`"profit_loss_jpy":200`,
		`"close_reason":"take_profit"`,
		`"opened_at":`,
		`"closed_at":`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON missing %q; got %s", want, js)
		}
	}
	// Should NOT expose Go-style PascalCase keys.
	if strings.Contains(js, `"PositionID"`) || strings.Contains(js, `"CloseReason"`) {
		t.Errorf("JSON should not expose PascalCase keys; got %s", js)
	}
}
