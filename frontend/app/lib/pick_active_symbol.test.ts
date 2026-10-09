import { describe, it, expect } from 'vitest'
import { pickActiveSymbol } from './pick_active_symbol'
import type { Status } from './types'

function statusWith(symbols: string[]): Status {
  return {
    mode: 'paper_config',
    symbol: symbols[0] ?? '',
    uptime: '0s',
    open_positions: 0,
    emergency_stop: false,
    symbols: symbols.map((s) => ({ symbol: s, open_positions: 0 })),
    account_open_positions: 0,
  }
}

describe('pickActiveSymbol', () => {
  it('returns first symbol on initial paint (currentSelection null)', () => {
    expect(pickActiveSymbol(null, statusWith(['USD_JPY', 'EUR_JPY']))).toBe('USD_JPY')
  })

  it('keeps user-selected symbol stable across reloads while still known', () => {
    expect(pickActiveSymbol('EUR_JPY', statusWith(['USD_JPY', 'EUR_JPY']))).toBe('EUR_JPY')
  })

  it('falls back to first symbol when current selection has dropped out of config', () => {
    expect(pickActiveSymbol('GBP_JPY', statusWith(['USD_JPY', 'EUR_JPY']))).toBe('USD_JPY')
  })

  it('returns current selection unchanged when status is null (fetch failed)', () => {
    expect(pickActiveSymbol('EUR_JPY', null)).toBe('EUR_JPY')
  })

  it('returns current selection unchanged when symbols[] is empty (no fresh data)', () => {
    expect(pickActiveSymbol('EUR_JPY', statusWith([]))).toBe('EUR_JPY')
  })

  it('single-symbol setup always lands on that symbol', () => {
    expect(pickActiveSymbol(null, statusWith(['USD_JPY']))).toBe('USD_JPY')
    expect(pickActiveSymbol('USD_JPY', statusWith(['USD_JPY']))).toBe('USD_JPY')
  })
})
