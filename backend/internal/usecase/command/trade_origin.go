package command

import "fx-bot/backend/internal/port"

// BotTrades returns only the trades the bot itself decided, dropping the
// operator's own discretionary trades — entries opened directly in the GMO app
// (adopted by reconcile as external_broker) and entries opened via the
// manual_trade command. An empty Origin is treated as bot for back-compat with
// rows recorded before the origin classification existed.
//
// Used by the dashboard edge metrics (計測パネル) and the ops daily summary
// so they measure the bot's edge, not the human's. Risk-gate consumers
// (worker / cooldown) intentionally do NOT call this — a discretionary trade
// still consumes the shared account, so it must still count toward the loss /
// consecutive-loss guards.
func BotTrades(trades []port.TradeRecord) []port.TradeRecord {
	out := make([]port.TradeRecord, 0, len(trades))
	for _, t := range trades {
		if t.Origin == "" || t.Origin == port.TradeOriginBot {
			out = append(out, t)
		}
	}
	return out
}
