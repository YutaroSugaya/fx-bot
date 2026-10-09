import { describe, it, expect } from 'vitest'
import { tradeOriginLabel, isDiscretionaryOrigin } from './trade_origin'

// The 「設定 ID」 column must NOT show a bot config_id for the operator's own
// discretionary trades — those borrow the active config as a mere FK target,
// so showing "trend-v4-usdjpy" makes a manual trade look like bot performance.
describe('tradeOriginLabel', () => {
  it('shows the bot config_id for bot trades', () => {
    expect(tradeOriginLabel('bot', 'trend-v4-usdjpy')).toBe('trend-v4-usdjpy')
  })

  it('treats missing origin as bot (legacy rows)', () => {
    expect(tradeOriginLabel(undefined, 'trend-v4-usdjpy')).toBe('trend-v4-usdjpy')
  })

  it('labels external GMO-app trades distinctly', () => {
    expect(tradeOriginLabel('external', 'trend-v4-usdjpy')).toBe('外部')
  })

  it('labels manual_trade-command trades distinctly', () => {
    expect(tradeOriginLabel('manual', 'trend-v4-usdjpy')).toBe('手動発注')
  })
})

describe('isDiscretionaryOrigin', () => {
  it.each([
    ['external', true],
    ['manual', true],
    ['bot', false],
    [undefined, false],
  ])('origin=%s → %s', (origin, want) => {
    expect(isDiscretionaryOrigin(origin as string | undefined)).toBe(want)
  })
})
