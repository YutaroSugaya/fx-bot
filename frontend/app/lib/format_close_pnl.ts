// Pure formatter for POST /api/positions/close responses.
//
// The dashboard calls this from closePosition() to render the result
// alert. Manual close is operator-driven so the formatter must never
// throw on missing numeric fields — it surfaces "N/A" instead. See
// format_close_pnl.test.ts for the contract.

export type CloseResponseLike = {
  profit_loss_jpy?: number | null
  profit_loss_pips?: number | null
  exit_price?: number | null
  error?: string
}

export type FormatResult = {
  ok: boolean
  message: string
}

function fixedOrNA(v: number | null | undefined, digits: number): string {
  return typeof v === 'number' ? v.toFixed(digits) : 'N/A'
}

function signedJPY(v: number | null | undefined): string {
  if (typeof v !== 'number') return 'N/A'
  return v > 0 ? `+${v.toFixed(0)}` : v.toFixed(0)
}

export function formatClosePnL(body: CloseResponseLike): FormatResult {
  if (body.error) {
    return { ok: false, message: `決済エラー: ${body.error}` }
  }
  const jpy = signedJPY(body.profit_loss_jpy)
  const pips = fixedOrNA(body.profit_loss_pips, 1)
  const exit = fixedOrNA(body.exit_price, 3)
  return {
    ok: true,
    message: `決済完了\n損益: ${jpy} 円 / ${pips} pips\n決済価格: ${exit}`,
  }
}
