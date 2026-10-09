import { describe, it, expect } from 'vitest'
import { fmtJST } from './format_time'

describe('fmtJST', () => {
  it('formats a valid ISO string to JST', () => {
    // 2026-05-26T03:00:00Z → JST 12:00
    const got = fmtJST('2026-05-26T03:00:00Z')
    expect(got).toContain('2026')
    expect(got).toContain('12:00')
    expect(got).toContain('JST')
  })

  it('returns empty string for empty input', () => {
    expect(fmtJST('')).toBe('')
  })

  it('returns input for unparseable strings', () => {
    expect(fmtJST('not-a-date')).toBe('not-a-date')
  })
})
