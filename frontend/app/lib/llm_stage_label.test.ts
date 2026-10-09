import { describe, expect, it } from 'vitest'
import { llmStageLabel } from './llm_stage_label'

// LLM判断ジャーナルの stage → 日本語ラベル。全判断・全約定をログ/ダッシュボードに出す方針:
// 運用者が raw の stage 文字列(night_buy_veto 等)を英語のまま見せられると、じわ負けの原因を
// 一目で判断できない。バックエンドの LLMDecisionResult.Stage(llm_decision_cycle.go)に新しい
// stage が増えたらここに追従する。
describe('llmStageLabel', () => {
  it('maps the per-currency discipline veto stages', () => {
    expect(llmStageLabel('night_buy_veto')).toBe('深夜帯BUY禁止で見送り (コードveto)')
    expect(llmStageLabel('chase_buy_veto')).toBe('高値圏チェイスBUY禁止で見送り (コードveto)')
    expect(llmStageLabel('sell_low_veto')).toBe('安値圏への売り追い禁止で見送り (コードveto)')
  })

  it('maps the pre-existing veto/skip stages that were missing from the dashboard', () => {
    expect(llmStageLabel('htf_trend_veto')).toBe('上位足トレンド逆行で見送り (コードveto)')
    expect(llmStageLabel('excluded_hour')).toBe('時間帯除外でスキップ')
  })

  it('keeps the existing stage labels', () => {
    expect(llmStageLabel('no_trade')).toBe('見送り中 (no_trade)')
    expect(llmStageLabel('submitted')).toBe('エントリー発注')
    expect(llmStageLabel('wide_spread')).toBe('スプレッド拡大で見送り')
    expect(llmStageLabel('decider_error')).toBe('判断エラー → 安全側で見送り')
    expect(llmStageLabel('market_closed')).toBe('🌙 マーケット休場中(土日)')
  })

  it('maps the emergency-stop skip stage (the cycle stops before calling the LLM)', () => {
    expect(llmStageLabel('emergency_stop')).toBe('🛑 緊急停止中でスキップ(LLM を呼ばない)')
  })

  it('falls back to the raw stage string for unknown stages (never hides information)', () => {
    expect(llmStageLabel('some_future_stage')).toBe('some_future_stage')
    expect(llmStageLabel(undefined)).toBe('—')
  })
})
