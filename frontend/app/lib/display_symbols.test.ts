import { describe, expect, it } from 'vitest'
import { isDisplayedSymbol } from './display_symbols'

// 表示スコープの契約:
//  - scope にあるペアだけ表示
//  - ただし OPEN ポジションを持つペアはスコープ外でも必ず表示(可視性原則)
//  - scope 空 = 全ペア表示(旧挙動へのワンライン復帰)
describe('isDisplayedSymbol', () => {
  it('shows only scoped symbols', () => {
    expect(isDisplayedSymbol('USD_JPY', [], ['USD_JPY'])).toBe(true)
    expect(isDisplayedSymbol('EUR_JPY', [], ['USD_JPY'])).toBe(false)
    expect(isDisplayedSymbol('GBP_USD', [], ['USD_JPY'])).toBe(false)
  })

  it('never hides a symbol holding an open position (visibility principle)', () => {
    expect(isDisplayedSymbol('EUR_JPY', ['EUR_JPY'], ['USD_JPY'])).toBe(true)
    // 玉のない別ペアは隠れたまま
    expect(isDisplayedSymbol('GBP_JPY', ['EUR_JPY'], ['USD_JPY'])).toBe(false)
  })

  it('empty scope shows everything (legacy behaviour)', () => {
    expect(isDisplayedSymbol('GBP_USD', [], [])).toBe(true)
  })

  it('defaults openSymbols to empty', () => {
    expect(isDisplayedSymbol('USD_JPY', undefined, ['USD_JPY'])).toBe(true)
  })
})
