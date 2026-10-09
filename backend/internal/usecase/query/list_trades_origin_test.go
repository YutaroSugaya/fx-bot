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

// /api/trades must expose each trade's origin so the dashboard can label the
// operator's own discretionary trades ("外部" / "手動発注") distinctly from bot
// trades instead of showing them under the borrowed bot config_id.
func TestListTradesQuery_ExposesOrigin(t *testing.T) {
	now := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	repo := backtest.NewInMemoryTradeRepo()
	mk := func(id int64, origin string) port.TradeRecord {
		return port.TradeRecord{
			PositionID: id, StrategyConfigID: "trend-v4-usdjpy",
			Symbol: "USD_JPY", Side: "BUY", Quantity: 1000,
			EntryPrice: 157.10, ExitPrice: 157.20,
			ProfitLossPips: 1.0, ProfitLossJPY: 100, CloseReason: "take_profit",
			Origin:   origin,
			OpenedAt: now.Add(-time.Hour), ClosedAt: now.Add(-30 * time.Minute),
		}
	}
	_ = repo.Insert(context.Background(), mk(1, port.TradeOriginExternal))
	_ = repo.Insert(context.Background(), mk(2, port.TradeOriginBot))

	q := &ListTradesQuery{Trades: repo, Clock: func() time.Time { return now }}
	got, err := q.Execute(context.Background(), ListTradesInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	byID := map[int64]TradeView{}
	for _, tv := range got {
		byID[tv.PositionID] = tv
	}
	if byID[1].Origin != port.TradeOriginExternal {
		t.Errorf("pos 1 Origin: got %q want %q", byID[1].Origin, port.TradeOriginExternal)
	}
	if byID[2].Origin != port.TradeOriginBot {
		t.Errorf("pos 2 Origin: got %q want %q", byID[2].Origin, port.TradeOriginBot)
	}

	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"origin":"external"`) {
		t.Errorf("JSON must carry snake_case origin; got %s", raw)
	}
}
