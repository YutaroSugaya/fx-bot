package command

import (
	"testing"

	"fx-bot/backend/internal/port"
)

// BotTrades must keep only bot-decided trades so the dashboard edge metrics
// (計測パネル) and the ops daily summary measure the bot's edge, not the
// operator's own discretionary (external GMO-app / manual_trade) trades.
func TestBotTrades(t *testing.T) {
	in := []port.TradeRecord{
		{PositionID: 1, Origin: port.TradeOriginBot},
		{PositionID: 2, Origin: port.TradeOriginExternal},
		{PositionID: 3, Origin: ""}, // legacy row: empty == bot
		{PositionID: 4, Origin: port.TradeOriginManual},
		{PositionID: 5, Origin: port.TradeOriginBot},
	}
	got := BotTrades(in)

	wantIDs := []int64{1, 3, 5}
	if len(got) != len(wantIDs) {
		t.Fatalf("BotTrades len = %d, want %d (%+v)", len(got), len(wantIDs), got)
	}
	for i, id := range wantIDs {
		if got[i].PositionID != id {
			t.Errorf("got[%d].PositionID = %d, want %d (order must be preserved)", i, got[i].PositionID, id)
		}
	}
}

func TestBotTrades_Empty(t *testing.T) {
	if got := BotTrades(nil); len(got) != 0 {
		t.Errorf("BotTrades(nil) = %+v, want empty", got)
	}
}

func TestBotTrades_AllExcluded(t *testing.T) {
	in := []port.TradeRecord{
		{PositionID: 1, Origin: port.TradeOriginExternal},
		{PositionID: 2, Origin: port.TradeOriginManual},
	}
	if got := BotTrades(in); len(got) != 0 {
		t.Errorf("BotTrades(all non-bot) = %+v, want empty", got)
	}
}
