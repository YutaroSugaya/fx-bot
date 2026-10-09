import { describe, it, expect } from 'vitest'
import { formatClosePnL } from './format_close_pnl'

// Regression guard for:
//   "TypeError: Cannot read properties of undefined (reading 'toFixed')"
// fired on POST /api/positions/close. The old inline code did
//   const jpy = body.profit_loss_jpy as number
//   jpy.toFixed(0)
// which crashes when the field is missing / null / undefined (e.g. the
// backend short-circuited on a path that didn't populate it, or the
// response was an unexpected shape). The dashboard then surfaced a
// blocking JS error instead of a useful message.
//
// formatClosePnL is a pure formatter that takes the parsed response body
// and returns a stable alert string + an "ok" flag, never throwing on
// missing fields. Manual close is operator-driven so the result panel
// must always render — even when the backend doesn't echo every number.

describe('formatClosePnL', () => {
  it('positive PnL: shows + prefix on JPY and 1-decimal pips with full exit price', () => {
    const result = formatClosePnL({
      profit_loss_jpy: 1234.56,
      profit_loss_pips: 8.7,
      exit_price: 150.123,
    })
    expect(result.ok).toBe(true)
    expect(result.message).toBe('決済完了\n損益: +1235 円 / 8.7 pips\n決済価格: 150.123')
  })

  it('negative PnL: shows minus prefix natively, no leading +', () => {
    const result = formatClosePnL({
      profit_loss_jpy: -500,
      profit_loss_pips: -3.2,
      exit_price: 149.876,
    })
    expect(result.ok).toBe(true)
    expect(result.message).toBe('決済完了\n損益: -500 円 / -3.2 pips\n決済価格: 149.876')
  })

  it('zero PnL: rendered as 0, not undefined', () => {
    const result = formatClosePnL({
      profit_loss_jpy: 0,
      profit_loss_pips: 0,
      exit_price: 150.0,
    })
    expect(result.ok).toBe(true)
    expect(result.message).toBe('決済完了\n損益: 0 円 / 0.0 pips\n決済価格: 150.000')
  })

  it('missing profit_loss_jpy: does NOT throw, surfaces N/A instead', () => {
    // This is the bug-repro case: previous code called undefined.toFixed(0).
    const result = formatClosePnL({
      profit_loss_pips: 5,
      exit_price: 150,
    } as unknown as { profit_loss_jpy: number; profit_loss_pips: number; exit_price: number })
    expect(result.ok).toBe(true)
    expect(result.message).toBe('決済完了\n損益: N/A 円 / 5.0 pips\n決済価格: 150.000')
  })

  it('all numeric fields missing: each falls back to N/A independently', () => {
    const result = formatClosePnL({} as unknown as { profit_loss_jpy: number; profit_loss_pips: number; exit_price: number })
    expect(result.ok).toBe(true)
    expect(result.message).toBe('決済完了\n損益: N/A 円 / N/A pips\n決済価格: N/A')
  })

  it('null fields (json null): treated the same as missing', () => {
    const result = formatClosePnL({
      profit_loss_jpy: null,
      profit_loss_pips: null,
      exit_price: null,
    } as unknown as { profit_loss_jpy: number; profit_loss_pips: number; exit_price: number })
    expect(result.ok).toBe(true)
    expect(result.message).toBe('決済完了\n損益: N/A 円 / N/A pips\n決済価格: N/A')
  })

  it('error in body: ok=false and message is the server-side error', () => {
    const result = formatClosePnL({
      error: 'position not found or already closed',
    })
    expect(result.ok).toBe(false)
    expect(result.message).toBe('決済エラー: position not found or already closed')
  })

  it('error with empty string treated as no-error (falsy)', () => {
    const result = formatClosePnL({
      error: '',
      profit_loss_jpy: 0,
      profit_loss_pips: 0,
      exit_price: 150,
    })
    expect(result.ok).toBe(true)
    expect(result.message).toContain('決済完了')
  })
})
