// PnL / pips formatters used by the dashboard table cells and summary rows.
//
// All helpers tolerate undefined / null / NaN inputs without throwing —
// the original page.tsx versions called `v.toFixed(...)` unguarded, and
// any malformed JSON or backend field rename crashed the whole page.

function isFiniteNumber(v: number | null | undefined): v is number {
  return typeof v === 'number' && Number.isFinite(v)
}

export function fmtJPY(v: number | null | undefined): string {
  if (!isFiniteNumber(v)) return 'N/A 円'
  const s = v.toFixed(0)
  return v > 0 ? `+${s} 円` : `${s} 円`
}

export function fmtPips(v: number | null | undefined): string {
  if (!isFiniteNumber(v)) return 'N/A pips'
  const s = v.toFixed(1)
  return v > 0 ? `+${s} pips` : `${s} pips`
}

export function pnlColor(v: number | null | undefined): string | undefined {
  if (!isFiniteNumber(v)) return undefined
  if (v > 0) return '#7fd17f'
  if (v < 0) return 'tomato'
  return undefined
}
