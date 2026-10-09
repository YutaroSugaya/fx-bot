import { describe, it, expect } from 'vitest'
import { priceDecimals, formatPrice } from './price_format'

// Scenario: a GBP_USD position card that hard-codes `price.toFixed(3)` shows
//   エントリー: 1.338 / 現在値: 1.338 (identical) next to a correct
//   未実現損益 of -2.9 pips (entry 1.33824, current 1.33795;
//   ×0.0001×1000×160.35 ≈ -46円). toFixed(3) is right for JPY-quote pairs
//   (160.346) but collapses USD-quote 5-decimal prices, so the prices look
//   equal while the P&L shows a loss → the card looks wrong.
//
// Rule: precision must follow the quote currency.
//   _JPY quote (pip=0.01)  → 3 decimals (0.1-pip resolution)
//   USD quote (pip=0.0001) → 5 decimals (0.1-pip resolution)

describe('priceDecimals', () => {
  it('JPY-quote pairs → 3 decimals', () => {
    expect(priceDecimals('USD_JPY')).toBe(3)
    expect(priceDecimals('EUR_JPY')).toBe(3)
    expect(priceDecimals('GBP_JPY')).toBe(3)
  })

  it('USD-quote pairs → 5 decimals', () => {
    expect(priceDecimals('EUR_USD')).toBe(5)
    expect(priceDecimals('GBP_USD')).toBe(5)
  })
})

describe('formatPrice', () => {
  it('GBP_USD renders all 5 decimals (no collapse)', () => {
    expect(formatPrice('GBP_USD', 1.33824)).toBe('1.33824')
    expect(formatPrice('GBP_USD', 1.33795)).toBe('1.33795')
  })

  it('the exact bug: entry and current no longer render identically', () => {
    const entry = formatPrice('GBP_USD', 1.33824)
    const current = formatPrice('GBP_USD', 1.33795)
    expect(entry).not.toBe(current)
    // sanity: the old hard-coded toFixed(3) DID collapse them
    expect((1.33824).toFixed(3)).toBe((1.33795).toFixed(3))
  })

  it('JPY-quote pairs stay at 3 decimals', () => {
    expect(formatPrice('USD_JPY', 160.295)).toBe('160.295')
    expect(formatPrice('GBP_JPY', 205.123)).toBe('205.123')
  })

  it('undefined / null / NaN → "N/A" (no crash)', () => {
    expect(formatPrice('GBP_USD', undefined as unknown as number)).toBe('N/A')
    expect(formatPrice('GBP_USD', null as unknown as number)).toBe('N/A')
    expect(formatPrice('GBP_USD', NaN)).toBe('N/A')
  })
})
