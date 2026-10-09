import { describe, it, expect } from 'vitest'
import { fmtJPY, fmtPips } from './format_pnl'
import type { Trade } from './types'

// Contract test: the frontend Trade type must read the actual snake_case
// JSON shape produced by backend/internal/usecase/query/list_trades.go.
//
// Previously the frontend type used PascalCase (ProfitLossPips,
// ProfitLossJPY, etc.), so reading t.ProfitLossPips against the real
// JSON returned undefined → fmtPips(undefined).toFixed(1) crash.
// This test pins the snake_case contract so the regression cannot
// reappear silently. SoT: TradeView struct (backend list_trades.go).

const SAMPLE_BACKEND_TRADE_JSON = `{
  "position_id": 42,
  "signal_id": "sig-abc",
  "strategy_config_id": "20260521-070000-usdjpy",
  "symbol": "USD_JPY",
  "side": "BUY",
  "quantity": 100,
  "entry_price": 150.123,
  "exit_price": 150.456,
  "profit_loss_pips": 33.3,
  "profit_loss_jpy": 3330,
  "close_reason": "take_profit",
  "opened_at": "2026-05-21T07:00:00Z",
  "closed_at": "2026-05-21T07:42:00Z",
  "origin": "bot"
}`

describe('Trade type ↔ backend TradeView contract', () => {
  it('parses snake_case JSON without producing undefined numeric fields', () => {
    const t = JSON.parse(SAMPLE_BACKEND_TRADE_JSON) as Trade
    // The crash signature was: any of these being undefined.
    expect(typeof t.profit_loss_pips).toBe('number')
    expect(typeof t.profit_loss_jpy).toBe('number')
    expect(typeof t.entry_price).toBe('number')
    expect(typeof t.exit_price).toBe('number')
  })

  it('parsed trade can be rendered through fmtPips/fmtJPY without crashing', () => {
    const t = JSON.parse(SAMPLE_BACKEND_TRADE_JSON) as Trade
    expect(fmtPips(t.profit_loss_pips)).toBe('+33.3 pips')
    expect(fmtJPY(t.profit_loss_jpy)).toBe('+3330 円')
  })
})
