import { describe, expect, it } from 'vitest'
import { parseDisplayEpoch, parseDisplaySymbols } from './display_scope'

// 表示スコープ(戦績の期間・表示するペア)は env で決める。未設定なら絞らない。
//  - NEXT_PUBLIC_TRADES_DISPLAY_EPOCH: opened_at がこの時刻以降の trade だけ戦績に出す
//  - NEXT_PUBLIC_DISPLAY_SYMBOLS: カンマ区切りのペア(空 = 全ペア)
describe('parseDisplayEpoch', () => {
  it('returns empty (no filter) when unset or blank', () => {
    expect(parseDisplayEpoch(undefined)).toBe('')
    expect(parseDisplayEpoch('')).toBe('')
    expect(parseDisplayEpoch('   ')).toBe('')
  })

  it('passes a valid ISO timestamp through (trimmed)', () => {
    expect(parseDisplayEpoch(' 2020-01-06T07:00:00+09:00 ')).toBe('2020-01-06T07:00:00+09:00')
  })

  it('ignores an unparsable value instead of hiding every trade', () => {
    expect(parseDisplayEpoch('not-a-date')).toBe('')
  })
})

describe('parseDisplaySymbols', () => {
  it('returns empty (all symbols) when unset or blank', () => {
    expect(parseDisplaySymbols(undefined)).toEqual([])
    expect(parseDisplaySymbols('')).toEqual([])
    expect(parseDisplaySymbols(' , ')).toEqual([])
  })

  it('splits, trims and upper-cases a comma separated list', () => {
    expect(parseDisplaySymbols('usd_jpy, EUR_JPY ,')).toEqual(['USD_JPY', 'EUR_JPY'])
  })
})
