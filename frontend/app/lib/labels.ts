// 表示ラベル定数の集約先。page.tsx と各 component から共通参照する。
//
// 命名規約: 全て snake_case のキーを backend JSON 契約と一致させる。

export const MODE_LABEL: Record<string, string> = {
  paper_config: 'ペーパー (paper_config)',
  live_config: 'ライブ (live_config)',
  disabled: '無効 (disabled)',
}

export const SIDE_LABEL: Record<string, string> = {
  BUY: '買い',
  SELL: '売り',
}

export const CLOSE_REASON_LABEL: Record<string, string> = {
  take_profit: '利確',
  stop_loss: '損切り',
  max_hold: '保有上限',
  early_exit: '早期利確',
  manual: '手動',
  reconcile_cold_close: '冷起動補正',
  ratchet_takeprofit: 'トレーリング利確',
  ratchet_stoploss: 'トレーリング損切り',
  broker_close: 'ブローカー決済',
  // 毎朝 05:30 JST の強制手仕舞い (05:45 前後のスプレッド急拡大+週末ギャップ対策)
  session_flatten: '朝クローズ',
}

// Origin of a trade's entry. Bot trades are intentionally absent (they fall
// through to the config_id display); only the operator's own discretionary
// trades get a distinct badge so they are not counted as bot performance.
// SoT: port.TradeOrigin (backend/internal/port/repository.go).
export const TRADE_ORIGIN_LABEL: Record<string, string> = {
  external: '外部',
  manual: '手動発注',
}

export const STRATEGY_LABEL: Record<string, string> = {
  momentum_pullback: 'モメンタム押し目',
  mtf_pullback: 'マルチTF押し目 (1h×5m)',
  ma_pullback: '200MA押し目 (5m)',
  breakout_follow: 'ブレイクアウト追随',
  range_breakout_probe: 'レンジブレイク試行',
  no_trade: '取引見送り',
}
