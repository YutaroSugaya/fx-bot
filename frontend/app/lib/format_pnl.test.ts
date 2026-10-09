import { describe, it, expect } from 'vitest'
import { fmtJPY, fmtPips, pnlColor } from './format_pnl'

// Regression guard for:
//   "TypeError: Cannot read properties of undefined (reading 'toFixed')"
//   thrown from fmtPips.
// Root cause: the frontend Trade type used PascalCase (ProfitLossPips)
// but the backend TradeView returns snake_case JSON
// (profit_loss_pips). t.ProfitLossPips was therefore undefined and
// fmtPips(undefined).toFixed(1) threw.
//
// fmtJPY / fmtPips are now hardened to handle undefined / null without
// throwing — but the real fix is the Trade type rename. This test
// guarantees the helpers themselves stay safe (a regression would
// reproduce the crash).

describe('fmtJPY', () => {
  it('positive number → +<n> 円', () => {
    expect(fmtJPY(1234.7)).toBe('+1235 円')
  })

  it('negative number → -<n> 円 (no leading +)', () => {
    expect(fmtJPY(-500)).toBe('-500 円')
  })

  it('zero → 0 円', () => {
    expect(fmtJPY(0)).toBe('0 円')
  })

  it('undefined: returns "N/A 円" (does NOT throw)', () => {
    // Reproduces the original crash signature.
    expect(fmtJPY(undefined as unknown as number)).toBe('N/A 円')
  })

  it('null: also returns "N/A 円"', () => {
    expect(fmtJPY(null as unknown as number)).toBe('N/A 円')
  })

  it('NaN: returns "N/A 円" (not "+NaN 円")', () => {
    expect(fmtJPY(NaN)).toBe('N/A 円')
  })
})

describe('fmtPips', () => {
  it('positive number → +<n.x> pips', () => {
    expect(fmtPips(8.74)).toBe('+8.7 pips')
  })

  it('negative number → -<n.x> pips', () => {
    expect(fmtPips(-3.2)).toBe('-3.2 pips')
  })

  it('zero → 0.0 pips', () => {
    expect(fmtPips(0)).toBe('0.0 pips')
  })

  it('undefined: returns "N/A pips" (does NOT throw — the original bug)', () => {
    expect(fmtPips(undefined as unknown as number)).toBe('N/A pips')
  })

  it('null: also returns "N/A pips"', () => {
    expect(fmtPips(null as unknown as number)).toBe('N/A pips')
  })

  it('NaN: returns "N/A pips"', () => {
    expect(fmtPips(NaN)).toBe('N/A pips')
  })
})

describe('pnlColor', () => {
  it('positive → green', () => {
    expect(pnlColor(10)).toBe('#7fd17f')
  })

  it('negative → red (tomato)', () => {
    expect(pnlColor(-1)).toBe('tomato')
  })

  it('zero → undefined (default color)', () => {
    expect(pnlColor(0)).toBeUndefined()
  })

  it('undefined input → undefined (no color, no crash)', () => {
    expect(pnlColor(undefined as unknown as number)).toBeUndefined()
  })
})
