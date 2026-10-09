import { describe, it, expect } from 'vitest'
import type { Status } from './types'

// Contract test: the Frontend Status type must accept every legitimate
// payload shape produced by /api/status. Backend BotStatusView declares
//   TickerErrors *int64 `json:"ticker_errors,omitempty"`
// which means the field can be:
//   - present as a number          (pointer set, value 0 / non-zero)
//   - omitted entirely             (pointer nil → omitempty drops it)
//
// Earlier the Frontend type had `ticker_errors: number` (required),
// so destructuring a payload without that key gave undefined and
// any code expecting a real number (e.g. arithmetic) silently broke.
// This test pins the optional contract so a future tightening cannot
// reintroduce the mismatch.

describe('Status type ↔ backend BotStatusView contract', () => {
  it('accepts payload with ticker_errors present', () => {
    const sample: Status = {
      mode: 'paper_config',
      symbol: 'USD_JPY',
      uptime: '1h2m',
      open_positions: 0,
      emergency_stop: false,
      symbols: [{ symbol: 'USD_JPY', open_positions: 0 }],
      account_open_positions: 0,
      ticker_errors: 3,
    }
    expect(sample.ticker_errors).toBe(3)
  })

  it('accepts payload with ticker_errors omitted (= backend pointer was nil + omitempty)', () => {
    const sample: Status = {
      mode: 'paper_config',
      symbol: 'USD_JPY',
      uptime: '1h2m',
      open_positions: 0,
      emergency_stop: false,
      symbols: [{ symbol: 'USD_JPY', open_positions: 0 }],
      account_open_positions: 0,
      // no ticker_errors here — the type must allow this
    }
    expect(sample.ticker_errors).toBeUndefined()
  })

  it('accepts a minimal status (only required fields)', () => {
    const sample: Status = {
      mode: 'paper_config',
      symbol: 'USD_JPY',
      uptime: '0s',
      open_positions: 0,
      emergency_stop: false,
      symbols: [{ symbol: 'USD_JPY', open_positions: 0 }],
      account_open_positions: 0,
    }
    // Type-only test — if this compiles + assigns, the optional contract is honored.
    expect(sample.mode).toBe('paper_config')
  })

  it('accepts a full status with all optional fields populated', () => {
    const sample: Status = {
      mode: 'live_config',
      symbol: 'USD_JPY',
      uptime: '3h27m12s',
      open_positions: 1,
      emergency_stop: false,
      symbols: [{ symbol: 'USD_JPY', open_positions: 1, active_config_id: 'u', enabled: true }],
      account_open_positions: 1,
      active_config_id: '20260521-070000-usdjpy',
      valid_until: '2026-05-21T08:00:00Z',
      strategy: 'momentum_pullback',
      enabled: true,
      ticker_errors: 0,
    }
    expect(sample.active_config_id).toMatch(/usdjpy$/)
  })

  it('accepts a multi-symbol payload with symbols[] array + account_open_positions', () => {
    const sample: Status = {
      mode: 'paper_config',
      symbol: 'USD_JPY',
      uptime: '2h',
      open_positions: 1,
      emergency_stop: false,
      symbols: [
        { symbol: 'USD_JPY', open_positions: 1, active_config_id: 'u', pnl_24h_jpy: 1500 },
        { symbol: 'EUR_JPY', open_positions: 2, active_config_id: 'e', pnl_24h_jpy: -800 },
      ],
      account_open_positions: 3,
    }
    expect(sample.symbols.length).toBe(2)
    expect(sample.symbols[1].pnl_24h_jpy).toBe(-800)
    expect(sample.account_open_positions).toBe(3)
  })

  it('accepts edge_metrics on both top-level and per-symbol (計測パネル)', () => {
    const em = {
      trade_count: 8, win_count: 7, loss_count: 1, win_rate_pct: 87.5,
      profit_factor: 2.27, avg_win_pips: 4.87, avg_loss_pips: 15,
      reward_risk: 0.32, expectancy_jpy: 23.88, net_pnl_jpy: 191,
      gross_pnl_jpy: 191, fee_jpy: 0, swap_jpy: 0, fee_estimated_count: 0,
      max_consecutive_losses: 1, window_days: 90,
    }
    const sample: Status = {
      mode: 'live_config',
      symbol: 'USD_JPY',
      uptime: '5h',
      open_positions: 0,
      emergency_stop: false,
      symbols: [{ symbol: 'USD_JPY', open_positions: 0, edge_metrics: em }],
      account_open_positions: 0,
      edge_metrics: em,
    }
    expect(sample.edge_metrics?.reward_risk).toBeLessThan(1) // 逆RR
    expect(sample.symbols[0].edge_metrics?.trade_count).toBe(8)
  })
})
