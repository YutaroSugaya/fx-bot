import { TRADE_ORIGIN_LABEL } from './labels'

// isDiscretionaryOrigin reports whether a trade was opened by the operator
// (directly in the GMO app = "external", or via the manual_trade command =
// "manual") rather than by the bot. Used to badge those rows distinctly.
export function isDiscretionaryOrigin(origin: string | undefined): boolean {
  return origin === 'external' || origin === 'manual'
}

// tradeOriginLabel returns what the 「設定 ID」 column should show: bot trades
// show their config_id, but the operator's own discretionary trades show a
// distinct origin badge ("外部" / "手動発注") so a manual trade is never
// mistaken for bot performance. Missing origin (legacy rows) = bot.
export function tradeOriginLabel(origin: string | undefined, strategyConfigId: string): string {
  if (isDiscretionaryOrigin(origin)) {
    return TRADE_ORIGIN_LABEL[origin as string] ?? strategyConfigId
  }
  return strategyConfigId
}
