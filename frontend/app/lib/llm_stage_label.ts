// LLM判断ジャーナルの stage → 日本語ラベル(ダッシュボード表示)。
// バックエンドの LLMDecisionResult.Stage(backend/internal/usecase/command/llm_decision_cycle.go)
// と1:1。未知の stage は raw のまま表示する(情報を隠さない)。
const LLM_STAGE_LABELS: Record<string, string> = {
  no_trade: '見送り中 (no_trade)',
  submitted: 'エントリー発注',
  wide_spread: 'スプレッド拡大で見送り',
  no_active_config: '設定未解決でスキップ',
  max_concurrent: '保有上限でスキップ',
  pyramid_not_armed: 'ピラミッド条件未達で見送り',
  decider_error: '判断エラー → 安全側で見送り',
  no_decider: '判断器なし',
  emergency_stop: '🛑 緊急停止中でスキップ(LLM を呼ばない)',
  invalid_side: '不正方向 → 見送り',
  skipped: 'スキップ',
  judging: '🤔 判断中…',
  timeout: '⏳ 判断に時間切れ(次サイクルで再試行)',
  market_closed: '🌙 マーケット休場中(土日)',
  error: '⚠️ エラー',
  excluded_hour: '時間帯除外でスキップ',
  htf_trend_veto: '上位足トレンド逆行で見送り (コードveto)',
  // 通貨別エントリー規律
  night_buy_veto: '深夜帯BUY禁止で見送り (コードveto)',
  chase_buy_veto: '高値圏チェイスBUY禁止で見送り (コードveto)',
  sell_low_veto: '安値圏への売り追い禁止で見送り (コードveto)',
  // 統一レーンの HARD 禁止
  exhaustion_veto: '伸び切り追撃禁止で見送り (コードveto)',
  spike_veto: '急変クールダウンで見送り (コードveto)',
  daily_loss_stop: '同日2敗で当日打ち止め (コードveto)',
  admission_rejected: 'リスクゲートが拒否',
  // シナリオ武装 (条件付き注文)
  armed: '⏳ シナリオ武装中(条件付き注文を監視)',
  armed_fired: '🎯 シナリオ発火→発注',
  armed_expired: 'シナリオ期限切れ(次サイクルで再判断)',
  lane_recheck_failed: '発火時に土俵消失で見送り (コードveto)',
  slot_taken: '既存ポジありで発火見送り',
  fire_error: '⚠️ シナリオ発火エラー',
}

export function llmStageLabel(stage: string | undefined): string {
  if (!stage) return '—'
  return LLM_STAGE_LABELS[stage] ?? stage
}
