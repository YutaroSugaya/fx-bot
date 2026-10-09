// Shared frontend types that mirror the backend JSON contract.
// SoT for these shapes is in:
//   - backend/internal/usecase/query/get_bot_status.go (BotStatusView)
//   - backend/internal/usecase/query/list_trades.go    (TradeView)
//   - backend/internal/usecase/query/list_open_positions.go  (OpenPositionView)
// Field names MUST stay snake_case to match the json:"..." struct tags.
// Optional (?) on a field means the backend uses `omitempty` and/or
// a pointer type (e.g. *int64, *time.Time, *bool), so the JSON key
// can be absent — the FE type must allow that.

// EdgeMetrics mirrors backend EdgeMetricsView (計測パネル). 直近 closed trade
// から算出。profit_factor / reward_risk は loss_count==0 のとき 0 (= N/A; ∞ / — 表示)。
// reward_risk < 1 = 逆RR (勝ち pips < 負け pips)。
export type EdgeMetrics = {
  trade_count: number
  win_count: number
  loss_count: number
  win_rate_pct: number
  profit_factor: number
  avg_win_pips: number
  avg_loss_pips: number
  reward_risk: number
  // gross / fee / net 分離。expectancy_jpy / net_pnl_jpy
  // は NET (= gross − fee + swap)。エッジ判定は net を見る。
  expectancy_jpy: number
  gross_pnl_jpy: number
  fee_jpy: number
  swap_jpy: number
  net_pnl_jpy: number
  fee_estimated_count: number
  max_consecutive_losses: number
  window_days: number
}

// SymbolStatus mirrors backend SymbolStatus (per-symbol breakdown carried
// in Status.symbols). Field naming is snake_case to match the json tag.
export type SymbolStatus = {
  symbol: string
  open_positions: number
  active_config_id?: string
  valid_until?: string
  strategy?: string
  enabled?: boolean
  pnl_24h_jpy?: number
  early_exit_count_24h?: number
  edge_metrics?: EdgeMetrics
}

export type Status = {
  mode: string
  uptime: string
  emergency_stop: boolean
  // Multi-symbol breakdown. Always present (1+ entries). Iterate this
  // instead of the legacy top-level fields below when rendering the
  // symbol-tab UI.
  symbols: SymbolStatus[]
  account_open_positions: number
  // Legacy single-symbol top-level fields (= primary symbol values).
  // Kept while the dashboard migrates fully to symbols[].
  symbol: string
  open_positions: number
  active_config_id?: string
  valid_until?: string
  strategy?: string
  enabled?: boolean
  // observability (pointer + omitempty → optional)
  ticker_errors?: number
  counters?: {
    ticker_errors: number
    emergency_trips: number
    resolve_timeouts: number
    close_races: number
    naked_positions: number
    trading_cycle_missing: number
  }
  // Dashboard 拡張: 24h ウィンドウの観察指標
  pnl_24h_jpy?: number
  reject_count_24h?: number
  early_exit_count_24h?: number
  last_advisor_duration_ms?: number
  // 計測パネル: primary symbol の edge 指標 (per-symbol は symbols[] 内)。
  edge_metrics?: EdgeMetrics
}

export type Trade = {
  position_id: number
  signal_id?: string
  strategy_config_id: string
  symbol: string
  side: string
  quantity: number
  entry_price: number
  exit_price: number
  profit_loss_pips: number
  profit_loss_jpy: number
  close_reason: string
  opened_at: string
  closed_at: string
  // "bot" (default) | "external" (GMO-app entry) | "manual" (manual_trade cmd).
  // Optional so a cached/legacy payload without the key is treated as bot.
  origin?: string
}
