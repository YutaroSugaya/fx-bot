'use client'

import { useEffect, useRef, useState } from 'react'
import { formatClosePnL, type CloseResponseLike } from './lib/format_close_pnl'
import { fmtJPY, fmtPips, pnlColor } from './lib/format_pnl'
import { aggregateBySymbol, aggregateByDay, tradingDayKey, filterSinceEpoch } from './lib/pnl_breakdown'
import { fmtJST } from './lib/format_time'
import { MODE_LABEL, SIDE_LABEL, CLOSE_REASON_LABEL, STRATEGY_LABEL } from './lib/labels'
import { LOT_SIZE_USDJPY, TRADES_DISPLAY_EPOCH } from './lib/constants'
import { formatPrice } from './lib/price_format'
import { llmStageLabel } from './lib/llm_stage_label'
import { isDisplayedSymbol } from './lib/display_symbols'
import { pickActiveSymbol } from './lib/pick_active_symbol'
import { TradeHistory } from './components/TradeHistory'
import { TradingChart } from './components/TradingChart'
import type { Status, Trade } from './lib/types'

type SymbolTriggerInfo = {
  promoted: boolean
  config_id?: string
  strategy?: string
  enabled?: boolean
  reject_reason?: string
  error?: string
}

type TriggerResult = {
  symbol?: string
  promoted: boolean
  config_id?: string
  strategy?: string
  enabled?: boolean
  reject_reason?: string
  duration_ms: number
  error?: string
  per_symbol?: Record<string, SymbolTriggerInfo>
}

type AskClaudeResult = {
  answer?: string
  duration_ms: number
  error?: string
}

// advisor v2 (署名ブレイク) の発火条件スナップショット (/api/advisor-v2)。
// scheduler が毎サイクル runtime/advisor_v2_status.json に書く内容そのまま。
type V2Step = {
  key: string       // trend | trigger | confirm
  label: string
  status: string    // done | active | todo
  detail: string
}

type V2SymbolState = {
  stage: string
  trend?: string
  buy_trigger?: number
  sell_trigger?: number
  dist_to_trigger_pips?: number
  atr_pips?: number
  current_price?: number
  slope_pips?: number
  next_step?: string
  reason?: string
  error?: string
  // entry-condition checklist + intraday firing state
  steps?: V2Step[]
  armed?: boolean
  broke_pips?: number
  min_slope_pips?: number
  body_need_pips?: number
  min_rr?: number
}

type AdvisorV2Status = {
  updated_at?: string
  interval?: string
  by_symbol?: Record<string, V2SymbolState>
}

// 自律LLMトレードループの最新判断スナップショット (/api/llm-decision)。
// scheduler が毎サイクル runtime/llm_decision_status.json に書く内容そのまま。
type LLMSymbolState = {
  stage?: string      // no_trade | submitted | wide_spread | no_active_config | max_concurrent | error ...
  go?: boolean
  side?: string       // BUY | SELL | none
  tp_pips?: number
  sl_pips?: number
  reason?: string
  playbook?: string   // 現在の「やり方」(反省ループが育てる)
  error?: string
}

type LLMDecisionStatus = {
  updated_at?: string
  interval?: string
  by_symbol?: Record<string, LLMSymbolState>
}

type ManualTradeResult = {
  position_id?: number
  side?: string
  quantity?: number
  entry_price?: number
  tp_price?: number
  sl_price?: number
  duration_ms: number
  error?: string
}

type OpenPosition = {
  id: number
  symbol: string
  side: string
  quantity: number
  entry_price: number
  current_price: number
  unrealized_pips: number
  unrealized_jpy: number
  tp_price: number
  sl_price: number
  opened_at: string
  elapsed_minutes: number
  max_hold_minutes: number
  deadline_at: string
  remaining_minutes: number
  strategy_config_id: string
  is_manual: boolean
  // origin: "bot" (default) | "external_broker" | "paper_recovered"
  source?: string
}

type CandleView = {
  time?: number // UNIX seconds (UTC), bar open time
  open: number
  high: number
  low: number
  close: number
}

type TimeframeView = {
  name: string
  open: number
  high: number
  low: number
  close: number
  range_pips: number
  change_pct: number
  direction: 'up' | 'down' | 'flat'
  sparkline: number[]
  candles?: CandleView[]
  ma_sma_200?: number[]
  ma_ema_200?: number[]
}

type MarketState = {
  symbol: string
  now_jst: string
  current: { bid: number; ask: number; spread_pips: number }
  timeframes: TimeframeView[]
}

type AdvisorDecision = {
  config_id: string
  status: string                  // "active" | "expired" | "rejected" | "generated"
  source: string
  strategy_name: string
  enabled: boolean
  market_regime_type: string
  market_regime_confidence: number
  market_regime_reason?: string
  direction?: string
  take_profit_pips?: number
  stop_loss_pips?: number
  max_hold_minutes?: number
  max_spread_pips?: number
  no_trade_reason?: string
  next_advisor_run_in_minutes?: number
  generated_at: string
  valid_from: string
  valid_until: string
  created_at: string
  activated_at?: string | null
  reject_reason?: string
}

// GMO 外為 FX (USD_JPY) の 1 lot = 10,000 通貨。
// 入力は lot、API には通貨単位で送る。
// 将来 multi-symbol になったら symbol 別に lot size を引く lookup を導入する。
// LOT_SIZE_USDJPY の定義は lib/constants.ts。

// trigger source → 表示ラベルと色。"claude_cli" / "fallback" は旧データ互換。
const SOURCE_LABEL: Record<string, { text: string; color: string; bg: string }> = {
  auto:       { text: '自動',   color: '#9ad',  bg: '#1a2530' },
  manual:     { text: '手動',   color: '#f5c542', bg: '#2a2418' },
  event:      { text: 'イベント', color: '#f08aa0', bg: '#2a1820' },
  claude_cli: { text: '旧:自動', color: '#888', bg: '#1a1d22' },
  fallback:   { text: 'フォールバック', color: '#888', bg: '#1a1d22' },
}

const DIRECTION_LABEL: Record<string, string> = {
  buy_only: '買いのみ',
  sell_only: '売りのみ',
  both: '両方向',
  none: 'なし (no_trade)',
}

const REGIME_LABEL: Record<string, string> = {
  range: 'レンジ',
  trend_up: '上昇トレンド',
  trend_down: '下降トレンド',
  volatile: '高ボラ',
  unclear: '不明',
}

const STATUS_LABEL: Record<string, { text: string; color: string; icon: string }> = {
  active:    { text: '採用中',  color: '#7fd17f', icon: '✓' },
  expired:   { text: '期限切れ', color: '#888',    icon: '·' },
  rejected:  { text: '却下',    color: 'tomato',  icon: '✗' },
  generated: { text: '生成のみ', color: '#888',    icon: '·' },
}

// 戦略名 + パラメータから「これでbotがどう動くか」を 1 文で説明
function describeImpact(d: AdvisorDecision): string {
  if (d.status === 'rejected') {
    return '却下されたため、以前の設定が継続中。bot 挙動は変わらない。'
  }
  if (d.strategy_name === 'no_trade' || !d.enabled) {
    return '新規エントリーを停止。既存ポジションの TP/SL/MaxHold 管理のみ継続。'
  }
  const dirLabel = DIRECTION_LABEL[d.direction ?? ''] ?? d.direction ?? ''
  const strat = STRATEGY_LABEL[d.strategy_name] ?? d.strategy_name
  const tp = d.take_profit_pips ?? 0
  const sl = d.stop_loss_pips ?? 0
  const hold = d.max_hold_minutes ?? 0
  const spread = d.max_spread_pips ?? 0
  return `${strat} で ${dirLabel} エントリー。TP ${tp.toFixed(1)}pips / SL ${sl.toFixed(1)}pips / 最大保有 ${hold}分。スプレッド ${spread.toFixed(1)}pips 以下のときのみ発注。`
}

// fmtJST と jstFormatter は lib/format_time.ts。

// Go の time.Duration の文字列 (例 "1h50m56s" / "47m12s" / "59s") を
// "1時間50分56秒" 形式に変換する。秒のみのときは「秒」だけ、分のみのときは
// 「分秒」だけ表示。冗長な "0時間" は出さない。
// ms (milliseconds) を「N秒」「N分M秒」のような日本語表記に。
// 60秒未満は小数1桁の秒。60秒以上は分秒。3600秒以上は時分秒。
function fmtDurationMs(ms: number): string {
  if (!isFinite(ms) || ms < 0) return `${ms} ms`
  if (ms < 1000) return `${ms}ミリ秒`
  const totalSec = ms / 1000
  if (totalSec < 60) return `${totalSec.toFixed(1)}秒`
  const h = Math.floor(totalSec / 3600)
  const m = Math.floor((totalSec % 3600) / 60)
  const s = Math.floor(totalSec % 60)
  const parts: string[] = []
  if (h > 0) parts.push(`${h}時間`)
  if (h > 0 || m > 0) parts.push(`${m}分`)
  parts.push(`${s}秒`)
  return parts.join('')
}

function fmtUptime(s: string): string {
  if (!s) return ''
  const m = s.match(/^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$/)
  if (!m) return s
  const h = m[1] ? parseInt(m[1], 10) : 0
  const min = m[2] ? parseInt(m[2], 10) : 0
  const sec = m[3] ? Math.floor(parseFloat(m[3])) : 0
  const parts: string[] = []
  if (h > 0) parts.push(`${h}時間`)
  if (h > 0 || min > 0) parts.push(`${min}分`)
  parts.push(`${sec}秒`)
  return parts.join('')
}

// fmtJST / MODE_LABEL / SIDE_LABEL / CLOSE_REASON_LABEL / STRATEGY_LABEL は
// lib/format_time.ts と lib/labels.ts にある。

// 注: "1MIN" = 1分足 (short), "1M" = 1ヶ月 (long)。分と月の取り違え防止に別キー。
const MARKET_TF_ORDER = ['1MIN', '5M', '30M', '1H', '1D', '1M', '3M']

const MARKET_TF_LABEL: Record<string, string> = {
  '1MIN': '1分',
  '5M': '5分',
  '30M': '30分',
  '1H': '1時間',
  '1D': '1日',
  '1M': '1ヶ月',
  '3M': '3ヶ月',
}

// グリッドで同時表示する短期 4 足 (6H 廃止 → 1分を追加)、残りはタブ切替の 1 枚チャート。
const MARKET_GRID_TFS = ['1MIN', '5M', '30M', '1H']
const MARKET_TAB_TFS = ['1D', '1M', '3M']

// Claude 判断トリガー / Claude 質問の UI は使わないため非表示。再表示は true に。
// (: boolean 注釈で literal-false 扱いを避け、内側の型ナローイングを保つ)
const SHOW_CLAUDE_TOOLS: boolean = false

function marketTFRank(name: string): number {
  const idx = MARKET_TF_ORDER.indexOf(name)
  return idx >= 0 ? idx : MARKET_TF_ORDER.length
}

function candlesForTimeframe(tf: TimeframeView): CandleView[] {
  if (tf.candles && tf.candles.length > 0) return tf.candles
  return [{ open: tf.open, high: tf.high, low: tf.low, close: tf.close }]
}


async function fetchJSON<T>(path: string): Promise<T | null> {
  const res = await fetch(path, { cache: 'no-store' })
  if (!res.ok) return null
  return (await res.json()) as T
}

// 取引履歴からPnLサマリを算出する。Goサイドに /api/pnl は無いので
// フロントで集計する。trades の上限は /api/trades?limit=300 に依存
// (通貨ペア別 / 日別の内訳が直近30日ぶんを拾えるよう 50→300 に拡張)。
function computePnL(trades: Trade[]) {
  const now = new Date()
  // 「本日」は GMO の本日損益に合わせ 6:00 JST 始まりの営業日で判定する。
  // 深夜 0〜6 時 JST はまだ前営業日 = GMO と同じ挙動。
  // tradingDayKey を「1日ごとの戦績」テーブルと共用するので、本日の損益は
  // その最上段 (= 現在の営業日) と必ず一致する。
  const todayStr = tradingDayKey(now.toISOString())

  const closed = trades.filter((t) => t.closed_at && t.exit_price > 0)
  const wins = closed.filter((t) => t.profit_loss_jpy > 0)
  const losses = closed.filter((t) => t.profit_loss_jpy < 0)

  const sumJPY = closed.reduce((acc, t) => acc + t.profit_loss_jpy, 0)
  const sumPips = closed.reduce((acc, t) => acc + t.profit_loss_pips, 0)

  const todayTrades = closed.filter((t) => tradingDayKey(t.closed_at) === todayStr)
  const todayJPY = todayTrades.reduce((acc, t) => acc + t.profit_loss_jpy, 0)
  const todayPips = todayTrades.reduce((acc, t) => acc + t.profit_loss_pips, 0)

  const bestJPY = closed.length === 0 ? 0 : Math.max(...closed.map((t) => t.profit_loss_jpy))
  const worstJPY = closed.length === 0 ? 0 : Math.min(...closed.map((t) => t.profit_loss_jpy))
  const avgWinJPY = wins.length === 0 ? 0 : wins.reduce((a, t) => a + t.profit_loss_jpy, 0) / wins.length
  const avgLossJPY = losses.length === 0 ? 0 : losses.reduce((a, t) => a + t.profit_loss_jpy, 0) / losses.length
  const winRate = closed.length === 0 ? 0 : (wins.length / closed.length) * 100

  return {
    total: closed.length,
    wins: wins.length,
    losses: losses.length,
    winRate,
    sumJPY,
    sumPips,
    todayCount: todayTrades.length,
    todayJPY,
    todayPips,
    bestJPY,
    worstJPY,
    avgWinJPY,
    avgLossJPY,
  }
}

// Fallback strategy constants (= DefaultSignatureParams in Go) used to derive the checklist on the
// client when the backend status predates the steps field (old binary). Display-only.
const V2_MIN_SLOPE = 50
const V2_DISP_MULT = 0.5

// deriveV2View builds the entry-condition checklist + armed state for one symbol. It prefers the
// backend-provided fields (new binary); otherwise it derives them from the primitive snapshot so the
// panel is clear even before a backend restart. Mirrors strategy.buildSignatureSteps (Go) 1:1.
function deriveV2View(sym: string, st: V2SymbolState): { steps: V2Step[]; armed: boolean; brokePips: number } {
  const pip = sym.endsWith('JPY') ? 0.01 : 0.0001
  const isUp = st.trend === 'up'
  const isDown = st.trend === 'down'
  const trendOn = isUp || isDown
  const dir = isUp ? '上昇' : isDown ? '下降' : '—'
  const arrow = isUp ? '上抜け' : '下抜け'
  const candle = isUp ? '陽線' : isDown ? '陰線' : '日足'
  const verb = isUp ? '成行買い' : isDown ? '成行売り' : '成行'
  const dist = st.dist_to_trigger_pips
  const broke = trendOn && typeof dist === 'number' && dist <= 0
  const brokePips = st.broke_pips ?? (broke && typeof dist === 'number' ? -dist : 0)
  const armed = st.armed ?? (trendOn && broke)
  // 表示は常にフロントで具体価格まで整形する (backend の steps 文字列には依存しない=
  // 旧バイナリでも・再起動後でも一貫して "具体的な数字" を出すため)。scalar (armed/broke/閾値) は
  // backend 値があれば優先 (?? fallback)。
  const minSlope = st.min_slope_pips ?? V2_MIN_SLOPE
  const bodyNeed = Math.round(st.body_need_pips ?? (typeof st.atr_pips === 'number' ? V2_DISP_MULT * st.atr_pips : 0))
  const slope = typeof st.slope_pips === 'number' ? st.slope_pips : 0
  const sg = (n: number) => (n >= 0 ? '+' : '')

  // 損切り/目標/RR を、露出済みの水準から具体価格で逆算 (表示用の射影=検出器の幾何と一致)。
  // SL = トリガー ∓ 1×ATR / 目標 = トリガー ± レンジ高 (= buy_trigger − sell_trigger)。
  const trig = isUp ? st.buy_trigger : st.sell_trigger
  const atr = st.atr_pips
  const haveLevels =
    trendOn && typeof trig === 'number' && typeof atr === 'number' &&
    typeof st.buy_trigger === 'number' && typeof st.sell_trigger === 'number'
  let confirmDetail: string
  if (!trendOn) {
    confirmDetail = 'トレンド点灯後に判定'
  } else if (haveLevels) {
    const rangePips = Math.round((st.buy_trigger! - st.sell_trigger!) / pip)
    const riskPips = Math.round(atr!)
    const slPrice = isUp ? trig! - atr! * pip : trig! + atr! * pip
    const tpPrice = isUp ? trig! + (st.buy_trigger! - st.sell_trigger!) : trig! - (st.buy_trigger! - st.sell_trigger!)
    const rr = riskPips > 0 ? (rangePips / riskPips).toFixed(1) : '—'
    const head = armed ? `確定すれば${verb}` : `確定ブレイクで${verb}`
    confirmDetail = `${candle}で実体≥${bodyNeed}pips確定 → ${head}。損切 ${formatPrice(sym, slPrice)}(−${riskPips}pips) / 目標 ${formatPrice(sym, tpPrice)}(+${rangePips}pips)・RR ${rr}`
  } else {
    confirmDetail = `${candle}で実体≥${bodyNeed}pips確定 → ${verb}`
  }

  const steps: V2Step[] = [
    {
      key: 'trend',
      label: '① トレンド点灯 (200日線の傾き)',
      status: trendOn ? 'done' : 'active',
      detail: trendOn
        ? `${dir} ${sg(slope)}${slope.toFixed(0)}pips(≥${minSlope}で点灯)✓`
        : `横ばい ${sg(slope)}${slope.toFixed(0)}pips。±${minSlope}pips超で点灯`,
    },
    {
      key: 'trigger',
      label: '② トリガー突破 (直近20日の極値)',
      status: !trendOn ? 'todo' : broke ? 'done' : 'active',
      detail: !trendOn
        ? 'トレンド点灯後に判定'
        : broke
          ? `${typeof trig === 'number' ? formatPrice(sym, trig) + ' を' : ''}${arrow}済み(+${brokePips.toFixed(0)}pips)✓`
          : `終値が ${typeof trig === 'number' ? formatPrice(sym, trig) : ''} を${arrow}待ち(あと${(dist ?? 0).toFixed(0)}pips)`,
    },
    {
      key: 'confirm',
      label: '③ 確定日足で本確認 → 成行',
      status: trendOn && broke ? 'active' : 'todo',
      detail: confirmDetail,
    },
  ]
  return { steps, armed, brokePips }
}

// SignatureFiringPanel: advisor v2 (署名=日足ブレイク) の「いま各条件がどうで、次に何が
// 起こればエントリーするか」を per-symbol で TODO リスト表示する。データは /api/advisor-v2。
function SignatureFiringPanel({ v2 }: { v2: AdvisorV2Status | null }) {
  const bySym = v2?.by_symbol ?? {}
  // 表示スコープ: v2 が無効でも status ファイルの残留があり得るため同じ絞りを掛ける。
  const syms = Object.keys(bySym).filter((s) => isDisplayedSymbol(s)).sort()
  const card = { background: '#0f1115', border: '1px solid #262a33', borderRadius: 6, padding: '10px 12px' } as const
  const trendLabel = (t?: string) =>
    t === 'up' ? '上昇 ▲' : t === 'down' ? '下降 ▼' : t?.startsWith('flat') ? '横ばい —' : (t ?? '—')
  // 価格の方向色は日本式: 上昇 = 赤, 下降 = 青。
  const trendColor = (t?: string) => (t === 'up' ? '#ef5350' : t === 'down' ? '#3b82f6' : '#888')
  const stageLabel = (s: string) =>
    ({
      submitted: 'エントリー送信',
      no_setup: '待機 (条件未成立)',
      no_go: '見送り (判定no-go)',
      side_mismatch: '見送り (方向不一致)',
      wide_spread: '見送り (スプレッド拡大)',
      stale_breakout: '見送り (走りすぎ/RR割れ)',
      max_concurrent: '見送り (建玉上限)',
      pyramid_not_armed: '待機 (1本目が利益未達=2本目不可)',
      no_active_config: '見送り (active config 無)',
      error: 'エラー',
    } as Record<string, string>)[s] ?? s
  const stepIcon = (s: string) => (s === 'done' ? '✓' : s === 'active' ? '▶' : '○')
  const stepColor = (s: string) => (s === 'done' ? '#7fd17f' : s === 'active' ? '#f5c542' : '#667')

  return (
    <div style={{ marginBottom: 16 }}>
      <div style={{ fontSize: 13, opacity: 0.7, marginBottom: 8 }}>
        この条件が揃うと自動エントリー(日足ブレイク){v2?.updated_at ? ` · 更新 ${fmtJST(v2.updated_at)}` : ''}
      </div>
      {syms.length === 0 ? (
        <div style={{ ...card, fontSize: 13, opacity: 0.7 }}>
          まだスナップショットがありません (起動直後 / v2 無効)。対象ペアは bot_config の advisor_v2.symbols で決まります。
        </div>
      ) : (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))', gap: 10 }}>
          {syms.map((sym) => {
            const st = bySym[sym]
            const view = deriveV2View(sym, st)
            const isUp = st.trend === 'up'
            const trig = isUp ? st.buy_trigger : st.trend === 'down' ? st.sell_trigger : undefined
            const active = view.steps.find((s) => s.status === 'active')
            const skipStages = ['no_go', 'side_mismatch', 'wide_spread', 'stale_breakout', 'max_concurrent', 'pyramid_not_armed', 'no_active_config', 'error']
            const waitShort: Record<string, string> = { trend: 'トレンド待ち', trigger: 'ブレイク待ち', confirm: '確定足待ち' }
            return (
              <div key={sym} style={card}>
                {/* header: 状態を一語で (発火待ち / 見送り / ○○待ち) */}
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 8 }}>
                  <b style={{ fontSize: 15 }}>{sym}</b>
                  {view.armed ? (
                    <span style={{ fontSize: 11.5, fontWeight: 700, color: '#1a1206', background: '#f5c542', borderRadius: 4, padding: '2px 7px' }}>🟡 発火待ち</span>
                  ) : skipStages.includes(st.stage) ? (
                    <span style={{ fontSize: 12, color: '#d98a8a' }}>{stageLabel(st.stage)}</span>
                  ) : (
                    <span style={{ fontSize: 12, opacity: 0.85 }}>{active ? waitShort[active.key] ?? '待機' : '待機'}</span>
                  )}
                </div>

                {/* TODO チェックリスト: ✓=達成 / ▶=今ここ / ○=これから。各行に具体的な数字 */}
                <div style={{ display: 'flex', flexDirection: 'column', gap: 7, marginBottom: 8 }}>
                  {view.steps.map((step) => (
                    <div key={step.key} style={{ display: 'flex', gap: 8, alignItems: 'flex-start' }}>
                      <span style={{ fontSize: 13, lineHeight: '17px', color: stepColor(step.status), width: 14, textAlign: 'center', flexShrink: 0 }}>{stepIcon(step.status)}</span>
                      <div style={{ flex: 1 }}>
                        <div style={{ fontSize: 12.5, color: step.status === 'todo' ? '#7a808c' : '#dde', fontWeight: step.status === 'active' ? 700 : 400 }}>{step.label}</div>
                        <div style={{ fontSize: 11.5, color: step.status === 'active' ? '#f5c542' : '#8b93a0', lineHeight: 1.45, marginTop: 1 }}>{step.detail}</div>
                      </div>
                    </div>
                  ))}
                </div>

                {/* compact footer: 生の数値 */}
                <div style={{ borderTop: '1px solid #262a33', paddingTop: 7, fontSize: 11.5, color: '#9aa', display: 'flex', flexWrap: 'wrap', gap: 10 }}>
                  <b style={{ color: trendColor(st.trend) }}>{trendLabel(st.trend)}</b>
                  <span>トリガー {typeof trig === 'number' ? formatPrice(sym, trig) : '—'}</span>
                  <span>現在 {typeof st.current_price === 'number' ? formatPrice(sym, st.current_price) : '—'}</span>
                  <span>ATR {typeof st.atr_pips === 'number' ? `${st.atr_pips.toFixed(0)}pips` : '—'}</span>
                </div>

                {st.error && <div style={{ marginTop: 6, fontSize: 12, color: 'tomato' }}>err: {st.error}</div>}
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}

export default function Page() {
  const [status, setStatus] = useState<Status | null>(null)
  const [trades, setTrades] = useState<Trade[]>([])
  const [decisions, setDecisions] = useState<AdvisorDecision[]>([])
  const [openPositions, setOpenPositions] = useState<OpenPosition[]>([])
  const [err, setErr] = useState<string | null>(null)
  const [advisorBusy, setAdvisorBusy] = useState(false)
  const [advisorResult, setAdvisorResult] = useState<TriggerResult | null>(null)
  // 自律LLM判断パネルの手動「全ペア再判断」ボタン用。
  // 判断は裏で数分走るので、ボタンは「押した瞬間〜新しい結果が届くまで」disable のままにする。
  // llmPendingRef = 押した時点の updated_at。新しい updated_at が来たら完了とみなす。
  const [llmBusy, setLlmBusy] = useState(false)
  const [llmMsg, setLlmMsg] = useState<string | null>(null)
  const llmPendingRef = useRef<string | null>(null)
  const llmTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  // Active symbol for the per-symbol panes (positions / market state /
  // active config card). Null while the first /api/status is in flight.
  // pickActiveSymbol below promotes the first known symbol from
  // status.symbols[] once status arrives, and keeps a manually-selected
  // symbol stable across reloads when it's still in the list.
  const [selectedSymbol, setSelectedSymbol] = useState<string | null>(null)

  // Claude 質問
  const [question, setQuestion] = useState('')
  const [askBusy, setAskBusy] = useState(false)
  const [askResult, setAskResult] = useState<AskClaudeResult | null>(null)

  // ポジション決済
  const [closingId, setClosingId] = useState<number | null>(null)
  // ポジション保有上限の延長
  const [extendingId, setExtendingId] = useState<number | null>(null)

  // 手動売買フォームの state (executeTrade 用。現在の UI には手動売買ボタンは無い)
  const [manualSide, setManualSide] = useState<'BUY' | 'SELL'>('BUY')
  const [manualTP, setManualTP] = useState('30')
  const [manualSL, setManualSL] = useState('20')
  const [manualHold, setManualHold] = useState('240')
  // 入力は lot 単位、API には currency units 単位で送る (LOT_SIZE_USDJPY = 10,000)。
  // default 0.1 lot (= 1,000 通貨) は GMO API minimum クリアかつ証拠金負担が
  // 軽い「最小テスト」サイズ。0.1 単位の小数 OK。
  const [manualLot, setManualLot] = useState('0.1')
  const [tradeBusy, setTradeBusy] = useState(false)
  const [tradeResult, setTradeResult] = useState<ManualTradeResult | null>(null)
  const [marketState, setMarketState] = useState<MarketState | null>(null)
  const [selectedTF, setSelectedTF] = useState<string>('1D')
  const [advisorV2, setAdvisorV2] = useState<AdvisorV2Status | null>(null)
  const [llm, setLlm] = useState<LLMDecisionStatus | null>(null)

  async function reload() {
    try {
      // オープンポジションは選択中の通貨ペアでフィルタせず、常に全 symbol を
      // まとめて取得する。USD/EUR どちらのタブを開いていても、保有中の
      // ポジションは両方まとめて最上段のカードに表示する。
      const [s, t, d, pos, v2, l] = await Promise.all([
        fetchJSON<Status>('/api/status'),
        fetchJSON<Trade[]>('/api/trades?limit=300'),
        fetchJSON<AdvisorDecision[]>('/api/advisor/recent?limit=3'),
        fetchJSON<OpenPosition[]>('/api/positions'),
        fetchJSON<AdvisorV2Status>('/api/advisor-v2'),
        fetchJSON<LLMDecisionStatus>('/api/llm-decision'),
      ])
      setStatus(s)
      // 戦績表示は「今動かしている戦略」の期間だけに絞る (TRADES_DISPLAY_EPOCH)。
      // 累計損益・勝率・通貨別/日別テーブル・履歴すべてこの trades を共有するので、
      // ここで1回絞れば下流すべてが同じ期間になる。DB は無変更 (表示専用)。
      setTrades(filterSinceEpoch(t ?? [], TRADES_DISPLAY_EPOCH))
      setDecisions(d ?? [])
      setOpenPositions(pos ?? [])
      setAdvisorV2(v2)
      setLlm(l)
      // 表示スコープ: symbol 選択も DISPLAY_SYMBOLS(+OPEN玉ペア)に絞った候補から選ぶ。
      // 絞った結果が空(設定ミス)なら全 symbol へフォールバックして画面を空にしない。
      const openSyms = (pos ?? []).map((p) => p.symbol)
      const scopedSyms = (s?.symbols ?? []).filter((x) => isDisplayedSymbol(x.symbol, openSyms))
      const scoped = s && scopedSyms.length > 0 ? { ...s, symbols: scopedSyms } : s
      const next = pickActiveSymbol(selectedSymbol, scoped)
      if (next !== selectedSymbol) setSelectedSymbol(next)
      setErr(null)
    } catch (e) {
      setErr(String(e))
    }
  }

  // Claude に判断させる (manual trigger of one advisor cycle)
  //
  // symbol=undefined (or 'all') → 全 symbol を並列発火、結果は per_symbol に。
  // symbol='USD_JPY' などの具体値 → その symbol のみ発火。
  async function triggerAdvisor(symbol?: string) {
    if (advisorBusy) return
    const label = symbol ? `${symbol} の判断のみを` : '全 symbol の判断を並列で'
    if (!confirm(`Claude に ${label}生成させます。サブエージェント並列検証のため数分〜10 分程度かかります。実行しますか?`)) return
    setAdvisorBusy(true)
    setAdvisorResult(null)
    try {
      const res = await fetch('/api/advisor/trigger', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ symbol: symbol ?? '' }),
      })
      const body = (await res.json()) as TriggerResult
      setAdvisorResult(body)
      reload() // 結果反映後にステータス・取引履歴も更新
    } catch (e) {
      setAdvisorResult({
        promoted: false,
        duration_ms: 0,
        error: String(e),
      })
    } finally {
      setAdvisorBusy(false)
    }
  }

  // 自律LLM判断を手動で全ペア再実行する (LLM がエラーになった時の復旧ボタン)。
  // バックエンドは即 202 を返し、判断は裏で数分走る。ボタンは「新しい結果(=updated_at の更新)
  // が届く」または「安全タイムアウト」までずっと disable のままにして、二度押し/連打を防ぐ。
  function clearLlmPending() {
    llmPendingRef.current = null
    if (llmTimeoutRef.current) {
      clearTimeout(llmTimeoutRef.current)
      llmTimeoutRef.current = null
    }
  }
  async function triggerLLM() {
    if (llmBusy) return
    const since = llm?.updated_at ?? '' // 完了判定の基準 (押した時点の最終判断時刻)
    setLlmBusy(true)
    setLlmMsg('🔄 全ペアを再判断中… 完了まで数分かかります(自動で反映されます)。')
    try {
      const res = await fetch('/api/llm-decision/trigger', { method: 'POST' })
      const body = (await res.json().catch(() => ({}))) as { started?: boolean; error?: string }
      if (res.status === 202 && body.started) {
        // 受理。裏で実行中 → ボタンは disable のまま。updated_at が進んだら下の useEffect で解除。
        llmPendingRef.current = since
        // 安全タイムアウト (万一 status が更新されなくても永久ロックしない)。
        llmTimeoutRef.current = setTimeout(() => {
          setLlmBusy(false)
          setLlmMsg('完了が確認できませんでした。パネルの「最終判断」時刻をご確認ください。')
          clearLlmPending()
        }, 13 * 60 * 1000)
        return // ここでは disable を解除しない
      }
      // 開始されなかった (409 実行中 / 休場 / その他エラー) → すぐ再有効化。
      setLlmMsg(res.status === 409 ? `⚠️ ${body.error ?? 'すでに実行中です'}` : `エラー: ${body.error ?? `HTTP ${res.status}`}`)
      setLlmBusy(false)
    } catch (e) {
      setLlmMsg(`エラー: ${e}`)
      setLlmBusy(false)
    }
  }

  // 手動再判断の完了検知: 押した時点より新しい状態が届き、かつ「判断中」のペアが
  // 1つも無くなったら(=全ペア確定)disable を解除する。サイクル開始時に出る "judging"
  // 中間スナップショットでは解除しない(updated_at は進むが判断中ペアが残っているため)。
  useEffect(() => {
    if (!llmBusy || llmPendingRef.current === null) return
    const cur = llm?.updated_at ?? ''
    const anyJudging = Object.values(llm?.by_symbol ?? {}).some((v) => v?.stage === 'judging')
    if (cur && cur !== llmPendingRef.current && !anyJudging) {
      setLlmBusy(false)
      setLlmMsg('✅ 再判断が完了しました。')
      clearLlmPending()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [llm, llmBusy])

  // Refetch on symbol switch so the per-symbol cards (市場の状態 / bot 判断 /
  // 稼働状況) update immediately without waiting up to 5s.
  // オープンポジションは全 symbol まとめて表示なので symbol 切替の影響は受けない。
  useEffect(() => {
    reload()
    const id = setInterval(reload, 5000)
    return () => clearInterval(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedSymbol])

  // Market state refreshes every 5s so the displayed spread/price stays
  // fresh. Backend caches with 5s TTL so this does not hammer GMO klines.
  // Market state follows the selected symbol so the spread/price the
  // operator sees always belongs to the selected tab. Refetch on symbol switch; clear stale
  // state immediately so the panel never shows the previous symbol's
  // numbers under the new tab.
  useEffect(() => {
    let cancelled = false
    setMarketState(null)
    if (!selectedSymbol) return
    async function loadMarket() {
      const sym = encodeURIComponent(selectedSymbol!)
      // エントリー条件は advisor v2 パネル (/api/advisor-v2) に一本化。ここは市場の状態のみ取得。
      const m = await fetchJSON<MarketState>(`/api/market/state?symbol=${sym}`)
      if (cancelled) return
      setMarketState(m)
    }
    loadMarket()
    const id = setInterval(loadMarket, 5000)
    return () => { cancelled = true; clearInterval(id) }
  }, [selectedSymbol])

  async function submitQuestion() {
    if (askBusy || !question.trim()) return
    setAskBusy(true)
    setAskResult(null)
    try {
      const res = await fetch('/api/ask-claude', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ question: question.trim() }),
      })
      const body = (await res.json()) as AskClaudeResult
      setAskResult(body)
    } catch (e) {
      setAskResult({ duration_ms: 0, error: String(e) })
    } finally {
      setAskBusy(false)
    }
  }

  // POST /api/trade/manual の呼び出し。現在の UI からは呼ばれていない (手動売買ボタンは無い)。
  async function executeTrade(side: 'BUY' | 'SELL') {
    if (tradeBusy) return
    setManualSide(side)
    const tp = parseFloat(manualTP)
    const sl = parseFloat(manualSL)
    const hold = parseInt(manualHold, 10)
    const lot = parseFloat(manualLot)
    if (!lot || lot <= 0) { alert('数量 (lot) を入力してください'); return }
    const qty = Math.round(lot * LOT_SIZE_USDJPY) // 通貨単位に変換して API に送る
    if (!tp || !sl) { alert('TP/SL を入力してください'); return }
    if (!confirm(`手動${side === 'BUY' ? '買い' : '売り'}を発注します。\nTP: ${tp} pips / SL: ${sl} pips / 数量: ${lot} lot (= ${qty.toLocaleString()} 通貨)\n\nactive strategy の direction (no_trade を含む) / open_positions / cooldown / 連敗 / window 上限 / spread は operator override として通します。\nemergency_stop / daily_loss / 経済指標 freeze / 同方向の積み増し (ナンピン禁止) / 再エントリー冷却 / 口座の建玉上限は引き続き enforce。\nよろしいですか?`)) return
    setTradeBusy(true)
    setTradeResult(null)
    try {
      const sym = selectedSymbol ?? status?.symbol ?? ''
      if (!sym) { alert('symbol が解決できません (status 未取得)'); return }
      const res = await fetch('/api/trade/manual', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ symbol: sym, side, take_profit_pips: tp, stop_loss_pips: sl, max_hold_minutes: hold, quantity: qty, allow_override: true }),
      })
      const body = (await res.json()) as ManualTradeResult
      setTradeResult(body)
      if (!body.error) reload()
    } catch (e) {
      setTradeResult({ duration_ms: 0, error: String(e) })
    } finally {
      setTradeBusy(false)
    }
  }

  async function closePosition(id: number, symbol: string) {
    if (closingId !== null) return
    if (!confirm(`ポジション ID#${id} (${symbol}) を現在価格で強制決済します。よろしいですか?`)) return
    setClosingId(id)
    try {
      const res = await fetch('/api/positions/close', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id, symbol }),
      })
      const text = await res.text()
      let body: CloseResponseLike
      try {
        body = JSON.parse(text)
      } catch {
        alert(`決済エラー: サーバー応答が不正です (HTTP ${res.status})\n${text.slice(0, 200)}`)
        return
      }
      const result = formatClosePnL(body)
      alert(result.message)
      if (result.ok) reload()
    } catch (e) {
      alert(`決済エラー: ${e}`)
    } finally {
      setClosingId(null)
    }
  }

  // 保有上限 (max_hold_minutes) を延長する。bot は OnTick で毎回 DB の
  // max_hold_minutes を読み直すので、この更新は次 tick から効く (close 不要)。
  // TP/SL は GMO 側 OCO に乗っているので延長しても下方向の歯止めは維持される。
  async function extendPosition(id: number, symbol: string, currentMaxHold: number) {
    if (extendingId !== null) return
    const raw = prompt(
      `ポジション ID#${id} (${symbol}) の保有上限を延長します。\n追加する分数を入力してください (現在の上限: ${currentMaxHold}分)。`,
      '60',
    )
    if (raw === null) return
    const add = parseInt(raw.trim(), 10)
    if (!Number.isFinite(add) || add <= 0) {
      alert('1 以上の分数を入力してください。')
      return
    }
    setExtendingId(id)
    try {
      const res = await fetch('/api/positions/extend', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id, symbol, add_minutes: add }),
      })
      const text = await res.text()
      let body: { max_hold_minutes?: number; added_minutes?: number; deadline_at?: string; error?: string }
      try {
        body = JSON.parse(text)
      } catch {
        alert(`延長エラー: サーバー応答が不正です (HTTP ${res.status})\n${text.slice(0, 200)}`)
        return
      }
      if (!res.ok || body.error) {
        alert(`延長エラー: ${body.error ?? `HTTP ${res.status}`}`)
        return
      }
      const deadline = body.deadline_at ? fmtJST(body.deadline_at) : '—'
      alert(`延長しました。\n新しい上限: ${body.max_hold_minutes}分 (+${body.added_minutes}分)\n締切: ${deadline}`)
      reload()
    } catch (e) {
      alert(`延長エラー: ${e}`)
    } finally {
      setExtendingId(null)
    }
  }

  async function triggerStop() {
    if (!confirm('Bot を停止します (新規エントリーを止め、既存ポジションの管理のみ継続)。よろしいですか?')) return
    await fetch('/api/emergency-stop', { method: 'POST' })
    reload()
  }
  async function triggerResume() {
    if (!confirm('Bot を再開します。よろしいですか?')) return
    await fetch('/api/emergency-resume', { method: 'POST' })
    reload()
  }

  const cardStyle: React.CSSProperties = {
    background: '#1a1d24',
    border: '1px solid #262a33',
    borderRadius: 8,
    padding: 16,
    marginBottom: 16,
  }

  return (
    <main style={{ maxWidth: 1100, margin: '0 auto' }}>
      <style>{`
        @media (max-width: 700px) {
          .market-grid {
            grid-template-columns: 1fr !important;
            max-width: calc(100vw - 80px);
            width: calc(100vw - 80px);
          }
        }
      `}</style>
      <h1 style={{ marginTop: 0 }}>fx-bot ダッシュボード</h1>
      {err && <div style={{ color: 'tomato' }}>{err}</div>}

      {/* ── ヘルスバー (最上部・稼働チェック) ── */}
      <section style={{ ...cardStyle, marginBottom: 12 }}>
        {!status ? <p style={{ margin: 0 }}>読み込み中…</p> : (() => {
          const liveOK = !status.emergency_stop
          const llmMs = llm?.updated_at ? Date.parse(llm.updated_at) : 0
          const ageMin = llmMs ? Math.round((Date.now() - llmMs) / 60000) : null
          const stale = ageMin !== null && ageMin > 130
          return (
            <div style={{ display: 'flex', gap: 16, alignItems: 'center', flexWrap: 'wrap', fontSize: 13 }}>
              <span style={{ fontSize: 16, fontWeight: 800, color: liveOK ? '#34d399' : 'tomato' }}>
                {liveOK ? '● 稼働中' : '■ 停止中 (emergency_stop)'}
              </span>
              <span>mode: {MODE_LABEL[status.mode] ?? status.mode}</span>
              <span>稼働: {fmtUptime(status.uptime)}</span>
              <span>保有: {status.account_open_positions ?? 0} 玉</span>
              <span style={{ color: stale ? 'tomato' : undefined, fontWeight: stale ? 700 : undefined }}>
                最終LLM判断: {ageMin !== null ? `${ageMin}分前` : '—'}{stale ? ' ⚠ 古い(動いてない可能性)' : ''}
              </span>
              <span style={{ color: (status.ticker_errors ?? 0) > 0 ? '#f5c542' : undefined }}>
                ticker err: {status.ticker_errors ?? 0}
              </span>
              <span style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
                {status.emergency_stop
                  ? <button onClick={triggerResume}>再開</button>
                  : <button onClick={triggerStop}>稼働停止</button>}
                <button onClick={reload}>再読込</button>
              </span>
            </div>
          )
        })()}
      </section>

      {/* ── 自律LLM判断 (全ペア・最上部) ── */}
      <section style={cardStyle}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, flexWrap: 'wrap' }}>
          <h2 style={{ margin: 0 }}>🤖 自律LLM判断(全ペア)</h2>
          <button
            onClick={triggerLLM}
            disabled={llmBusy}
            title="全ペアの判断を今すぐ実行し直します(LLM がエラーになった時の手動更新)。完了まで数分。実行中は押せません。"
            style={{
              padding: '6px 14px',
              fontWeight: 600,
              cursor: llmBusy ? 'not-allowed' : 'pointer',
              opacity: llmBusy ? 0.55 : 1,
              pointerEvents: llmBusy ? 'none' : 'auto',
            }}
          >
            {llmBusy ? '⏳ 再判断中…' : '🔄 全ペア再判断'}
          </button>
        </div>
        {llmMsg && <p style={{ margin: '8px 0 0', fontSize: 12, color: '#9ad' }}>{llmMsg}</p>}
        {(() => {
          const bs = llm?.by_symbol ?? {}
          // 表示スコープ: 稼働ペア(DISPLAY_SYMBOLS)+OPEN玉ペアのみ表示。
          // status ファイルに残る過去の他ペア判断もここで消える。
          const openSyms = openPositions.map((p) => p.symbol)
          const syms = Object.keys(bs).filter((sym) => isDisplayedSymbol(sym, openSyms)).sort()
          if (syms.length === 0) return <p style={{ opacity: 0.6 }}>まだ判断がありません(起動直後の warmup 中、または無効)。</p>
          // stage → 日本語ラベルは lib/llm_stage_label に集約(未登録の stage は raw 英語で
          // 表示されてしまうので新 stage は必ずそこへ足す)。ステージ別の色。判断中=青 / 発注=緑 / 時間切れ=橙 / エラー=赤 / その他=灰。
          const stageColor = (stage?: string): string =>
            stage === 'submitted' ? '#34d399'
              : stage === 'judging' ? '#60a5fa'
              : stage === 'timeout' ? '#fbbf24'
              : stage === 'error' || stage === 'decider_error' ? '#f87171'
              : '#cbd5e1'
          return (
            <div>
              <p style={{ fontSize: 12, opacity: 0.7, marginTop: 0 }}>
                最終判断: {llm?.updated_at ? fmtJST(llm.updated_at) : '—'} / チェック間隔: {llm?.interval ?? '—'} / {syms.length}ペア
              </p>
              {syms.map((sym) => {
                const st = bs[sym]
                const isEntry = st.stage === 'submitted'
                return (
                  <div key={sym} style={{ borderTop: '1px solid #262a33', padding: '8px 0' }}>
                    <div style={{ display: 'flex', alignItems: 'baseline', gap: 10, flexWrap: 'wrap' }}>
                      <span style={{ fontWeight: 700, minWidth: 72 }}>{sym}</span>
                      <span style={{ fontSize: 15, fontWeight: 700, color: stageColor(st.stage) }}>
                        {isEntry
                          ? `▶ ${st.side} 発注 (TP${st.tp_pips}/SL${st.sl_pips})`
                          : llmStageLabel(st.stage)}
                      </span>
                    </div>
                    {st.reason ? <p style={{ margin: '4px 0 0', fontSize: 12, opacity: 0.85 }}>理由: {st.reason}</p> : null}
                    {st.playbook && st.playbook.trim() ? (
                      <details style={{ marginTop: 4 }}>
                        <summary style={{ cursor: 'pointer', fontSize: 12, opacity: 0.7 }}>戦略ポートフォリオ</summary>
                        <pre style={{ whiteSpace: 'pre-wrap', fontSize: 11, opacity: 0.85, margin: '4px 0 0', padding: '8px 10px', background: '#0b0d11', border: '1px solid #262a33', borderRadius: 4 }}>{st.playbook}</pre>
                      </details>
                    ) : null}
                  </div>
                )
              })}
            </div>
          )
        })()}
      </section>

      {/* ── オープンポジション (全 symbol まとめて) ── */}
      <section style={cardStyle}>
        <h2>オープンポジション</h2>
        {openPositions.length === 0 ? (
          <p style={{ opacity: 0.6 }}>現在オープンポジションはありません</p>
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
            {/* 通貨ペアでグループ化して表示 (USD/EUR が混在しても並んで見えるように) */}
            {[...openPositions]
              .sort((a, b) => a.symbol.localeCompare(b.symbol) || a.id - b.id)
              .map((pos) => {
              const isBuy = pos.side === 'BUY'
              const sideColor = isBuy ? '#7fd17f' : 'tomato'
              const pnlColor2 = pos.unrealized_pips > 0 ? '#7fd17f' : pos.unrealized_pips < 0 ? 'tomato' : undefined
              const isExternal = pos.source === 'external_broker'
              const remainPct = pos.max_hold_minutes > 0
                ? Math.max(0, Math.min(100, (pos.remaining_minutes / pos.max_hold_minutes) * 100))
                : 0
              const isOverdue = pos.remaining_minutes < 0
              return (
                <div
                  key={pos.id}
                  style={{
                    padding: 12,
                    background: '#0f1115',
                    border: `1px solid ${sideColor}44`,
                    borderLeft: `3px solid ${sideColor}`,
                    borderRadius: 6,
                    fontSize: 13,
                  }}
                >
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 8 }}>
                    <span style={{ fontWeight: 700, color: sideColor, fontSize: 15 }}>
                      <span style={{
                        display: 'inline-block', marginRight: 8, padding: '2px 8px',
                        fontSize: 12, fontWeight: 700, color: '#cdd3e0',
                        background: '#1c2230', border: '1px solid #3a4a66', borderRadius: 4,
                        verticalAlign: 'middle',
                      }}>{pos.symbol}</span>
                      {isBuy ? '▲ 買い (BUY)' : '▼ 売り (SELL)'}
                      <span style={{ fontWeight: 400, color: '#e0e4ef', marginLeft: 8, fontSize: 13 }}>
                        {(pos.quantity / LOT_SIZE_USDJPY).toLocaleString(undefined, { maximumFractionDigits: 3 })} lot ({pos.quantity.toLocaleString()} 通貨)
                      </span>
                      {isExternal && (
                        <span style={{
                          marginLeft: 8, fontSize: 11, fontWeight: 700,
                          color: '#f5c542', background: '#3a2d10', border: '1px solid #f5c54255',
                          padding: '2px 6px', borderRadius: 3,
                        }} title="GMO アプリ等で開いた position、または bot 発注後の通信エラーで bot DB が record を失ったポジション。reconcile が後付け採用したため bot が自動 TP/SL/MaxHold 管理しない。operator による手動 close は可能。">
                          管理外 (broker のみ)
                        </span>
                      )}
                    </span>
                    <span style={{ fontSize: 11, opacity: 0.6 }}>
                      {pos.is_manual ? '手動発注' : pos.strategy_config_id} · ID#{pos.id}
                    </span>
                  </div>

                  <div style={{ display: 'flex', flexWrap: 'wrap', gap: '4px 24px', marginBottom: 8 }}>
                    <span>エントリー: <strong>{formatPrice(pos.symbol, pos.entry_price)}</strong></span>
                    {pos.current_price > 0 && (
                      <span>現在値: <strong>{formatPrice(pos.symbol, pos.current_price)}</strong></span>
                    )}
                    {!isExternal && (
                      <>
                        <span style={{ color: '#7fd17f' }}>TP: {formatPrice(pos.symbol, pos.tp_price)}</span>
                        <span style={{ color: 'tomato' }}>SL: {formatPrice(pos.symbol, pos.sl_price)}</span>
                      </>
                    )}
                  </div>

                  {pos.current_price > 0 && (
                    <div style={{ marginBottom: 8, fontSize: 14, fontWeight: 700, color: pnlColor2 }}>
                      未実現損益: {pos.unrealized_pips > 0 ? '+' : ''}{pos.unrealized_pips.toFixed(1)} pips
                      {' / '}
                      {pos.unrealized_jpy > 0 ? '+' : ''}{pos.unrealized_jpy.toFixed(0)} 円
                    </div>
                  )}

                  {isExternal ? (
                    <div style={{ fontSize: 12, opacity: 0.75 }}>
                      <span>開始: {fmtJST(pos.opened_at)}</span>
                      <span style={{ marginLeft: 12, color: '#f5c542' }}>
                        bot 自動管理対象外 — operator が dashboard or GMO アプリで操作してください
                      </span>
                    </div>
                  ) : (
                    <>
                      <div style={{ fontSize: 12, opacity: 0.75 }}>
                        <span>経過: {Math.floor(pos.elapsed_minutes)}分</span>
                        {' · '}
                        <span style={{ color: isOverdue ? 'tomato' : undefined }}>
                          残り: {isOverdue ? `超過 ${Math.abs(Math.floor(pos.remaining_minutes))}分` : `${Math.floor(pos.remaining_minutes)}分`}
                          {' / '}最大 {pos.max_hold_minutes}分
                        </span>
                        {' · '}
                        <span>開始: {fmtJST(pos.opened_at)}</span>
                      </div>

                      {/* 残り時間バー */}
                      <div style={{ marginTop: 6, height: 4, background: '#262a33', borderRadius: 2, overflow: 'hidden' }}>
                        <div style={{
                          height: '100%',
                          width: `${remainPct}%`,
                          background: remainPct > 30 ? '#7fd17f' : remainPct > 10 ? '#f5c542' : 'tomato',
                          borderRadius: 2,
                          transition: 'width 0.5s',
                        }} />
                      </div>
                    </>
                  )}

                  <div style={{ marginTop: 10, display: 'flex', gap: 8 }}>
                    {/* 延長: bot 管理対象 (外部ポジは bot が時間決済しないので非表示) */}
                    {!isExternal && (
                      <button
                        onClick={() => extendPosition(pos.id, pos.symbol, pos.max_hold_minutes)}
                        disabled={extendingId !== null}
                        title="保有上限 (max_hold) を延長。TP/SL はそのまま、bot の時間決済だけ先送りします。"
                        style={{
                          padding: '6px 16px',
                          fontWeight: 700,
                          fontSize: 13,
                          background: extendingId === pos.id ? '#1a2a3a' : '#1a2f4a',
                          color: '#7fb8ff',
                          border: '1px solid #4a90d944',
                          borderRadius: 4,
                          cursor: extendingId !== null ? 'wait' : 'pointer',
                          opacity: extendingId !== null && extendingId !== pos.id ? 0.4 : 1,
                        }}
                      >
                        {extendingId === pos.id ? '延長中…' : '⏱ 延長'}
                      </button>
                    )}
                    <button
                      onClick={() => closePosition(pos.id, pos.symbol)}
                      disabled={closingId !== null}
                      style={{
                        padding: '6px 16px',
                        fontWeight: 700,
                        fontSize: 13,
                        background: closingId === pos.id ? '#3a1a1a' : '#4a1a1a',
                        color: 'tomato',
                        border: '1px solid #ff6b6b44',
                        borderRadius: 4,
                        cursor: closingId !== null ? 'wait' : 'pointer',
                        opacity: closingId !== null && closingId !== pos.id ? 0.4 : 1,
                      }}
                    >
                      {closingId === pos.id ? '決済中…' : isExternal ? '強制決済 (外部ポジ)' : '強制決済'}
                    </button>
                  </div>
                </div>
              )
            })}
          </div>
        )}
      </section>

      {(() => {
        if (!status || !status.symbols) return null
        // Show tabs for the pairs actually in play: DISPLAY_SYMBOLS plus any pair that holds a
        // position. The llm/v2 by_symbol sets are not used for this because stale entries linger in
        // the status files, so the display-scope constant is the source of truth. If the filter
        // yields nothing, fall back to all symbols so the tab bar never goes empty by mistake.
        const posSyms = openPositions.map((p) => p.symbol)
        const filtered = status.symbols.filter((s) => isDisplayedSymbol(s.symbol, posSyms))
        const tabs = filtered.length > 0 ? filtered : status.symbols
        if (tabs.length <= 1) return null // single active pair → no tab bar needed
        return (
          <section style={{ marginBottom: 16, display: 'flex', gap: 6, flexWrap: 'wrap', alignItems: 'center' }}>
            <span style={{ fontSize: 12, opacity: 0.7, marginRight: 4 }}>通貨ペア</span>
            {tabs.map((s) => {
              const active = s.symbol === selectedSymbol
              return (
                <button
                  key={s.symbol}
                  onClick={() => setSelectedSymbol(s.symbol)}
                  style={{
                    padding: '6px 14px',
                    background: active ? '#2c4a78' : '#1a1d24',
                    color: active ? '#fff' : '#9ad',
                    border: '1px solid ' + (active ? '#3d6298' : '#262a33'),
                    borderRadius: 6,
                    cursor: 'pointer',
                    fontWeight: active ? 600 : 400,
                  }}
                >
                  {s.symbol}
                  {s.open_positions > 0 && (
                    <span style={{ marginLeft: 6, fontSize: 11, opacity: 0.85 }}>
                      ({s.open_positions})
                    </span>
                  )}
                </button>
              )
            })}
            <span style={{ marginLeft: 'auto', fontSize: 12, opacity: 0.7 }}>
              account合計 open: <strong>{status.account_open_positions}</strong>
            </span>
          </section>
        )
      })()}

      {/* ── 市場の状態 ── */}
      <section style={cardStyle}>
        <h2>市場の状態 {marketState ? `(${marketState.symbol})` : ''}</h2>
        {marketState ? (
          (() => {
            const tfs = [...marketState.timeframes].sort((a, b) => marketTFRank(a.name) - marketTFRank(b.name))
            const byName = (n: string) => tfs.find((t) => t.name === n)
            const gridTfs = MARKET_GRID_TFS.map(byName).filter((t): t is TimeframeView => !!t)
            const tabTfs = MARKET_TAB_TFS.map(byName).filter((t): t is TimeframeView => !!t)
            const activeTabTf = tabTfs.find((t) => t.name === selectedTF) ?? tabTfs[0]
            return (
              <>
                {/* 現在値 */}
                <div style={{ fontSize: 13, opacity: 0.85, marginBottom: 10 }}>
                  現在値 <b style={{ color: '#e0e4ef' }}>{formatPrice(marketState.symbol, marketState.current.bid)}</b> / <b style={{ color: '#e0e4ef' }}>{formatPrice(marketState.symbol, marketState.current.ask)}</b>
                  <span style={{ opacity: 0.6, marginLeft: 8 }}>スプレッド {marketState.current.spread_pips.toFixed(2)} pips</span>
                </div>

                {/* 短期足 2×2 グリッド (1分/5分/30分/1時間 を同時表示) */}
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(380px, 1fr))', gap: 10 }}>
                  {gridTfs.map((tf) => {
                    const candles = candlesForTimeframe(tf)
                    const dirColor = tf.direction === 'up' ? '#ef5350' : tf.direction === 'down' ? '#3b82f6' : '#888'
                    const arrow = tf.direction === 'up' ? '▲' : tf.direction === 'down' ? '▼' : '─'
                    return (
                      <div key={tf.name} style={{ background: '#0f1115', border: '1px solid #262a33', borderRadius: 6, overflow: 'hidden' }}>
                        <div style={{ display: 'flex', alignItems: 'baseline', gap: 8, padding: '6px 10px 2px' }}>
                          <span style={{ fontWeight: 700, fontSize: 13 }}>{MARKET_TF_LABEL[tf.name] ?? tf.name}</span>
                          <span style={{ fontSize: 14, fontWeight: 700, color: '#e0e4ef', fontVariantNumeric: 'tabular-nums' }}>{formatPrice(marketState.symbol, tf.close)}</span>
                          <span style={{ color: dirColor, fontSize: 11, fontWeight: 700, whiteSpace: 'nowrap' }}>
                            {arrow} {tf.change_pct >= 0 ? '+' : ''}{tf.change_pct.toFixed(3)}%
                          </span>
                        </div>
                        <TradingChart candles={candles} tfName={tf.name} symbol={marketState.symbol} height={230} maSma={tf.ma_sma_200} maEma={tf.ma_ema_200} />
                      </div>
                    )
                  })}
                </div>

                {/* 長期足タブ (1日/1ヶ月/3ヶ月) */}
                {activeTabTf && (
                  <div style={{ marginTop: 16 }}>
                    <div style={{ display: 'flex', gap: 4, marginBottom: 8 }}>
                      {tabTfs.map((tf) => {
                        const on = tf.name === activeTabTf.name
                        return (
                          <button
                            key={tf.name}
                            onClick={() => setSelectedTF(tf.name)}
                            style={{
                              padding: '4px 12px',
                              fontSize: 12,
                              fontWeight: on ? 700 : 500,
                              color: on ? '#e0e4ef' : '#8b909c',
                              background: on ? '#1c2230' : 'transparent',
                              border: `1px solid ${on ? '#3a4a66' : '#262a33'}`,
                              borderRadius: 4,
                              cursor: 'pointer',
                            }}
                          >
                            {MARKET_TF_LABEL[tf.name] ?? tf.name}
                          </button>
                        )
                      })}
                    </div>
                    <div style={{ background: '#0f1115', border: '1px solid #262a33', borderRadius: 6, overflow: 'hidden' }}>
                      <TradingChart candles={candlesForTimeframe(activeTabTf)} tfName={activeTabTf.name} symbol={marketState.symbol} height={320} maSma={activeTabTf.ma_sma_200} maEma={activeTabTf.ma_ema_200} />
                    </div>
                  </div>
                )}

                <p style={{ margin: '8px 0 0 0', fontSize: 11, opacity: 0.5 }}>
                  5 秒ごとに自動更新。時刻軸は JST。短期 (1分/5分/30分/1時間) はローカル DB、長期 (1日/1ヶ月/3ヶ月) は GMO klines から。
                </p>
              </>
            )
          })()
        ) : (
          <p style={{ opacity: 0.6, fontSize: 13 }}>loading market state…</p>
        )}
      </section>

      <section style={cardStyle}>
        <h2>現在の収益</h2>
        {(() => {
          const p = computePnL(trades)
          if (p.total === 0) {
            return <p>まだ確定した取引がありません</p>
          }
          const rowStyle: React.CSSProperties = { padding: '4px 12px 4px 0' }
          const bySymbol = aggregateBySymbol(trades)
          const byDay = aggregateByDay(trades)
          const thStyle: React.CSSProperties = { textAlign: 'left', padding: '4px 12px 4px 0', opacity: 0.7, fontWeight: 600, borderBottom: '1px solid rgba(255,255,255,0.15)' }
          const tdStyle: React.CSSProperties = { padding: '4px 12px 4px 0' }
          const numTd: React.CSSProperties = { ...tdStyle, textAlign: 'right' }
          return (
            <>
            <table>
              <tbody>
                <tr>
                  <td style={rowStyle}>本日の損益 (6時始まり)</td>
                  <td style={{ ...rowStyle, color: pnlColor(p.todayJPY), fontWeight: 600 }}>
                    {fmtJPY(p.todayJPY)} ({fmtPips(p.todayPips)}) · {p.todayCount} 件
                  </td>
                </tr>
                <tr>
                  <td style={rowStyle}>累計損益 (直近 {p.total} 件)</td>
                  <td style={{ ...rowStyle, color: pnlColor(p.sumJPY), fontWeight: 600 }}>
                    {fmtJPY(p.sumJPY)} ({fmtPips(p.sumPips)})
                  </td>
                </tr>
                <tr>
                  <td style={rowStyle}>勝率</td>
                  <td style={rowStyle}>
                    {p.winRate.toFixed(1)}% (勝 {p.wins} / 負 {p.losses})
                  </td>
                </tr>
                <tr>
                  <td style={rowStyle}>平均利益 / 平均損失</td>
                  <td style={rowStyle}>
                    <span style={{ color: pnlColor(p.avgWinJPY) }}>{fmtJPY(p.avgWinJPY)}</span>
                    {' / '}
                    <span style={{ color: pnlColor(p.avgLossJPY) }}>{fmtJPY(p.avgLossJPY)}</span>
                  </td>
                </tr>
                <tr>
                  <td style={rowStyle}>最大利益 / 最大損失</td>
                  <td style={rowStyle}>
                    <span style={{ color: pnlColor(p.bestJPY) }}>{fmtJPY(p.bestJPY)}</span>
                    {' / '}
                    <span style={{ color: pnlColor(p.worstJPY) }}>{fmtJPY(p.worstJPY)}</span>
                  </td>
                </tr>
              </tbody>
            </table>

            {/* 通貨ペアごとの戦績 (合計損益の降順)。closed トレードのみ。 */}
            <h3 style={{ marginTop: 20, marginBottom: 6, fontSize: '1.0em' }}>通貨ペアごとの戦績</h3>
            {bySymbol.length === 0 ? (
              <p style={{ opacity: 0.6, fontSize: 13 }}>データなし</p>
            ) : (
              <table style={{ borderCollapse: 'collapse' }}>
                <thead>
                  <tr>
                    <th style={thStyle}>通貨ペア</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>件数</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>勝率</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>損益</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>pips</th>
                  </tr>
                </thead>
                <tbody>
                  {bySymbol.map((s) => (
                    <tr key={s.symbol}>
                      <td style={tdStyle}>{s.symbol}</td>
                      <td style={numTd}>{s.count}</td>
                      <td style={numTd}>{s.winRate.toFixed(0)}% ({s.wins}/{s.losses})</td>
                      <td style={{ ...numTd, color: pnlColor(s.sumJPY), fontWeight: 600 }}>{fmtJPY(s.sumJPY)}</td>
                      <td style={{ ...numTd, color: pnlColor(s.sumPips) }}>{fmtPips(s.sumPips)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}

            {/* 1営業日 (6:00 JST 始まり = GMO の日切り) ごとの戦績。新しい日が先。closed トレードのみ。 */}
            <h3 style={{ marginTop: 20, marginBottom: 6, fontSize: '1.0em' }}>1日ごとの戦績 (6時始まり)</h3>
            <p style={{ marginTop: 0, marginBottom: 6, fontSize: 12, opacity: 0.6 }}>
              GMO の本日損益に合わせ、毎朝 6:00 JST で日を区切る (深夜 0〜6 時の決済は前日に集計)。
            </p>
            {byDay.length === 0 ? (
              <p style={{ opacity: 0.6, fontSize: 13 }}>データなし</p>
            ) : (
              <table style={{ borderCollapse: 'collapse' }}>
                <thead>
                  <tr>
                    <th style={thStyle}>日付</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>件数</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>勝率</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>損益</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>pips</th>
                  </tr>
                </thead>
                <tbody>
                  {byDay.map((d) => (
                    <tr key={d.date}>
                      <td style={tdStyle}>{d.date}</td>
                      <td style={numTd}>{d.count}</td>
                      <td style={numTd}>{d.winRate.toFixed(0)}% ({d.wins}/{d.losses})</td>
                      <td style={{ ...numTd, color: pnlColor(d.sumJPY), fontWeight: 600 }}>{fmtJPY(d.sumJPY)}</td>
                      <td style={{ ...numTd, color: pnlColor(d.sumPips) }}>{fmtPips(d.sumPips)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            </>
          )
        })()}
      </section>

      <section style={cardStyle}>
        <details>
        <summary style={{ cursor: 'pointer', fontWeight: 700, fontSize: '1.1em' }}>稼働状況(詳細・クリックで展開)</summary>
        {!status ? (
          <p>読み込み中…</p>
        ) : (() => {
          // For multi-symbol setups the "active config / open positions"
          // block reflects the SELECTED symbol from the tab strip above,
          // not the primary one. Falls back to the legacy top-level
          // fields (= primary symbol) when symbols[] is empty / not yet
          // loaded.
          const selected =
            (status.symbols ?? []).find((s) => s.symbol === selectedSymbol) ?? null
          const symLabel = selected?.symbol ?? status.symbol
          const symOpen = selected?.open_positions ?? status.open_positions
          const symActiveID = selected?.active_config_id ?? status.active_config_id
          const symStrategy = selected?.strategy ?? status.strategy
          const symEnabled = selected?.enabled ?? status.enabled
          const symValidUntil = selected?.valid_until ?? status.valid_until
          const symPnl24h = selected?.pnl_24h_jpy ?? status.pnl_24h_jpy
          const symEarlyExit = selected?.early_exit_count_24h ?? status.early_exit_count_24h
          const symEdge = selected?.edge_metrics ?? status.edge_metrics
          return (
            <table>
              <tbody>
                <tr><td>モード</td><td>{MODE_LABEL[status.mode] ?? status.mode}</td></tr>
                <tr><td>通貨ペア</td><td>{symLabel}</td></tr>
                <tr><td>稼働時間</td><td>{fmtUptime(status.uptime)}</td></tr>
                <tr><td>保有ポジション数</td><td>{symOpen}</td></tr>
                <tr><td>ティッカー取得エラー</td><td>{status.ticker_errors}</td></tr>
                <tr>
                  <td>稼働状態</td>
                  <td style={{ color: status.emergency_stop ? 'tomato' : '#7fd17f', fontWeight: 600 }}>
                    {status.emergency_stop ? '停止中 (新規エントリー停止)' : '稼働中'}
                  </td>
                </tr>
                {symPnl24h !== undefined && (
                  <tr>
                    <td>直近 24h PnL</td>
                    <td style={{ color: symPnl24h >= 0 ? '#7fd17f' : 'tomato', fontWeight: 600 }}>
                      {symPnl24h >= 0 ? '+' : ''}{symPnl24h.toFixed(1)} JPY
                    </td>
                  </tr>
                )}
                {symEarlyExit !== undefined && (
                  <tr><td>早期 exit 発火 (24h)</td><td>{symEarlyExit} 件</td></tr>
                )}
                {status.reject_count_24h !== undefined && (
                  <tr><td>Config reject (24h)</td><td>{status.reject_count_24h} 件</td></tr>
                )}
                {status.last_advisor_duration_ms !== undefined && (
                  <tr><td>直近 advisor 実行時間</td><td>{(status.last_advisor_duration_ms / 1000).toFixed(1)} 秒</td></tr>
                )}
                {symEdge && symEdge.trade_count > 0 && (() => {
                  const hasRR = symEdge.loss_count > 0 && symEdge.win_count > 0
                  const invertedRR = hasRR && symEdge.reward_risk < 1
                  const pf = symEdge.loss_count > 0
                    ? symEdge.profit_factor.toFixed(2)
                    : (symEdge.win_count > 0 ? '∞' : '—')
                  return (
                    <>
                      <tr><td colSpan={2} style={{ paddingTop: 12, fontWeight: 600, opacity: 0.85 }}>
                        ── edge 品質 (直近 {symEdge.trade_count} trade / 最大 {symEdge.window_days}日) ──
                      </td></tr>
                      <tr><td>勝率</td><td>{symEdge.win_rate_pct.toFixed(1)}% ({symEdge.win_count}勝 {symEdge.loss_count}敗)</td></tr>
                      <tr><td>Profit Factor</td><td>{pf}</td></tr>
                      <tr>
                        <td>RR (平均勝pips / 平均負pips)</td>
                        <td style={{ color: invertedRR ? 'tomato' : (hasRR ? '#7fd17f' : undefined), fontWeight: 600 }}>
                          {hasRR ? symEdge.reward_risk.toFixed(2) : '—'}
                          {invertedRR && ' ⚠ 逆RR'}
                        </td>
                      </tr>
                      <tr><td>平均 勝ち / 負け pips</td><td>{symEdge.avg_win_pips.toFixed(1)} / {symEdge.avg_loss_pips.toFixed(1)}</td></tr>
                      {/* gross / fee / net 分離。エッジ判定は net。 */}
                      {symEdge.gross_pnl_jpy !== undefined && (
                        <tr>
                          <td>PnL 内訳 (gross − fee + swap)</td>
                          <td>
                            {symEdge.gross_pnl_jpy >= 0 ? '+' : ''}{symEdge.gross_pnl_jpy.toFixed(0)}
                            {' − '}{symEdge.fee_jpy.toFixed(0)}
                            {' + '}{symEdge.swap_jpy.toFixed(0)}
                            {' = '}
                            <span style={{ color: symEdge.net_pnl_jpy >= 0 ? '#7fd17f' : 'tomato', fontWeight: 600 }}>
                              {symEdge.net_pnl_jpy >= 0 ? '+' : ''}{symEdge.net_pnl_jpy.toFixed(0)} JPY (net)
                            </span>
                            {symEdge.fee_estimated_count > 0 && (
                              <span style={{ opacity: 0.7 }}> (fee 推定 {symEdge.fee_estimated_count} 件)</span>
                            )}
                          </td>
                        </tr>
                      )}
                      <tr>
                        <td>期待値 / trade (net)</td>
                        <td style={{ color: symEdge.expectancy_jpy >= 0 ? '#7fd17f' : 'tomato', fontWeight: 600 }}>
                          {symEdge.expectancy_jpy >= 0 ? '+' : ''}{symEdge.expectancy_jpy.toFixed(0)} JPY
                        </td>
                      </tr>
                      <tr><td>最大連敗</td><td>{symEdge.max_consecutive_losses}</td></tr>
                    </>
                  )
                })()}
                {symActiveID && (() => {
                  const isTradeable = symStrategy !== 'no_trade' && !!symEnabled
                  const stratLabel = STRATEGY_LABEL[symStrategy ?? ''] ?? symStrategy
                  return (
                    <>
                      <tr><td>アクティブ設定 ID</td><td>{symActiveID}</td></tr>
                      <tr>
                        <td>戦略</td>
                        <td style={{ color: isTradeable ? '#7fd17f' : 'tomato', fontWeight: 600 }}>
                          {isTradeable ? '⭕' : '❌'} {stratLabel}
                          {!isTradeable && (
                            <span style={{ opacity: 0.7, fontWeight: 400, marginLeft: 8 }}>
                              (新規エントリー停止中)
                            </span>
                          )}
                        </td>
                      </tr>
                      <tr>
                        <td>新規エントリー</td>
                        <td style={{ color: isTradeable ? '#7fd17f' : 'tomato', fontWeight: 600 }}>
                          {isTradeable ? '⭕ 取引中' : '❌ 取引なし'}
                        </td>
                      </tr>
                      <tr><td>有効期限</td><td>{fmtJST(symValidUntil ?? '')}</td></tr>
                    </>
                  )
                })()}
              </tbody>
            </table>
          )
        })()}
        </details>
        <div style={{ marginTop: 8 }}>
          {status?.emergency_stop ? (
            <button onClick={triggerResume}>再開</button>
          ) : (
            <button onClick={triggerStop}>稼働停止</button>
          )}
          <button onClick={reload} style={{ marginLeft: 8 }}>再読み込み</button>
        </div>
      </section>

      {/* 手動売買フォームは置かない(自律LLM運用のため不要) */}

      {SHOW_CLAUDE_TOOLS && (<>
      <section style={cardStyle}>
        <h2>Claude に判断させる</h2>
        <p style={{ margin: '0 0 8px 0', opacity: 0.75, fontSize: 13 }}>
          ai_advisor を有効にすると interval_minutes ごとに自動実行されます。下のボタンを押すと、その場で Claude を呼び出して
          次の 1 時間の方針 (買う / 売る / 見送る) を生成します。所要時間は symbol あたり 10〜30 秒。
          通貨ごとに判断する場合は左、全 symbol を一気に動かす場合は右。
        </p>
        <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
          <button
            onClick={() => triggerAdvisor(selectedSymbol ?? undefined)}
            disabled={advisorBusy || !selectedSymbol}
            style={{
              padding: '8px 16px',
              fontWeight: 600,
              cursor: advisorBusy ? 'wait' : 'pointer',
              opacity: advisorBusy || !selectedSymbol ? 0.6 : 1,
            }}
          >
            {advisorBusy
              ? '判断中…'
              : `${selectedSymbol ?? '(symbol 未選択)'} のみ判断`}
          </button>
          <button
            onClick={() => triggerAdvisor()}
            disabled={advisorBusy}
            style={{
              padding: '8px 16px',
              fontWeight: 600,
              cursor: advisorBusy ? 'wait' : 'pointer',
              opacity: advisorBusy ? 0.6 : 1,
            }}
          >
            {advisorBusy ? '判断中… (Claude 応答待ち)' : '全 symbol 並列で判断'}
          </button>
        </div>

        {advisorResult && (
          <div
            style={{
              marginTop: 12,
              padding: 12,
              background: '#0f1115',
              border: '1px solid #262a33',
              borderRadius: 6,
            }}
          >
            {advisorResult.error ? (
              <p style={{ color: 'tomato', margin: 0 }}>
                エラー: {advisorResult.error}
              </p>
            ) : advisorResult.per_symbol && Object.keys(advisorResult.per_symbol).length > 0 ? (
              <p style={{ margin: 0, opacity: 0.85, fontSize: 13 }}>
                全 symbol 並列発火が完了しました ({fmtDurationMs(advisorResult.duration_ms)})。
                下の表で symbol 別の結果を確認してください。
              </p>
            ) : advisorResult.promoted ? (
              <>
                <p style={{ margin: '0 0 4px 0', color: '#7fd17f', fontWeight: 600 }}>
                  ✓ 新しい設定が反映されました ({fmtDurationMs(advisorResult.duration_ms)})
                </p>
                {(() => {
                  const isTradeable = advisorResult.strategy !== 'no_trade' && !!advisorResult.enabled
                  return (
                    <table>
                      <tbody>
                        <tr>
                          <td style={{ paddingRight: 12 }}>戦略</td>
                          <td style={{ color: isTradeable ? '#7fd17f' : 'tomato', fontWeight: 600 }}>
                            {isTradeable ? '⭕' : '❌'}{' '}
                            {STRATEGY_LABEL[advisorResult.strategy ?? ''] ?? advisorResult.strategy}
                          </td>
                        </tr>
                        <tr>
                          <td style={{ paddingRight: 12 }}>新規エントリー</td>
                          <td style={{ color: isTradeable ? '#7fd17f' : 'tomato', fontWeight: 600 }}>
                            {isTradeable ? '⭕ 取引可 (条件成立で発注)' : '❌ 見送り (no_trade)'}
                          </td>
                        </tr>
                        <tr>
                          <td style={{ paddingRight: 12 }}>設定 ID</td>
                          <td style={{ fontSize: 11, opacity: 0.7 }}>{advisorResult.config_id}</td>
                        </tr>
                      </tbody>
                    </table>
                  )
                })()}
              </>
            ) : (
              <p style={{ margin: 0, color: 'tomato' }}>
                ✗ 設定が却下されました ({fmtDurationMs(advisorResult.duration_ms)})
                <br />
                <span style={{ fontSize: 12, opacity: 0.8 }}>
                  理由: {advisorResult.reject_reason || '(不明)'}
                </span>
              </p>
            )}

            {advisorResult.per_symbol && Object.keys(advisorResult.per_symbol).length > 0 && (
              <div style={{ marginTop: 12, paddingTop: 12, borderTop: '1px dashed #262a33' }}>
                <p style={{ margin: '0 0 6px 0', fontSize: 12, opacity: 0.75 }}>
                  symbol 別の結果 (全 symbol 並列発火):
                </p>
                <table style={{ fontSize: 12 }}>
                  <thead>
                    <tr style={{ opacity: 0.65 }}>
                      <th style={{ textAlign: 'left', paddingRight: 12 }}>symbol</th>
                      <th style={{ textAlign: 'left', paddingRight: 12 }}>結果</th>
                      <th style={{ textAlign: 'left', paddingRight: 12 }}>戦略</th>
                      <th style={{ textAlign: 'left', paddingRight: 12 }}>config_id / 理由</th>
                    </tr>
                  </thead>
                  <tbody>
                    {Object.entries(advisorResult.per_symbol).map(([sym, info]) => {
                      const tradeable = info.promoted && info.strategy !== 'no_trade' && !!info.enabled
                      return (
                        <tr key={sym}>
                          <td style={{ paddingRight: 12, fontWeight: 600 }}>{sym}</td>
                          <td style={{
                            paddingRight: 12,
                            color: info.error ? 'tomato' : info.promoted ? '#7fd17f' : 'tomato',
                          }}>
                            {info.error
                              ? '✗ エラー'
                              : info.promoted
                                ? (tradeable ? '⭕ promote' : '⚪ promote (no_trade)')
                                : '✗ 却下'}
                          </td>
                          <td style={{ paddingRight: 12 }}>
                            {STRATEGY_LABEL[info.strategy ?? ''] ?? info.strategy ?? '-'}
                          </td>
                          <td style={{ fontSize: 11, opacity: 0.7 }}>
                            {info.error || info.reject_reason || info.config_id || '-'}
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        )}
      </section>

      {/* ── Claude に質問する ── */}
      <section style={cardStyle}>
        <h2>Claude に質問する</h2>
        <p style={{ margin: '0 0 8px 0', opacity: 0.75, fontSize: 13 }}>
          現在の市場サマリーを添付した上で Claude に自由質問できます。
          「今日のイベントは？」「USD/JPY の方向感は？」など。
        </p>
        <div style={{ marginBottom: 8, display: 'flex', flexWrap: 'wrap', gap: 6 }}>
          {['今日の重要経済指標は何かある？', 'USD/JPY の現在の市場感を教えて', '今のトレンド方向と注意点は？', '直近の reject 原因を分析して'].map((q) => (
            <button
              key={q}
              onClick={() => setQuestion(q)}
              style={{ fontSize: 11, padding: '3px 8px', opacity: 0.8 }}
            >
              {q}
            </button>
          ))}
        </div>
        <div style={{ display: 'flex', gap: 8, marginBottom: 8 }}>
          <textarea
            value={question}
            onChange={(e) => setQuestion(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submitQuestion() }}
            rows={3}
            placeholder="質問を入力… (Cmd/Ctrl+Enter で送信)"
            style={{ flex: 1, resize: 'vertical', background: '#0f1115', color: '#e0e4ef', border: '1px solid #262a33', borderRadius: 4, padding: 8, fontSize: 13 }}
          />
        </div>
        <button
          onClick={submitQuestion}
          disabled={askBusy || !question.trim()}
          style={{ padding: '8px 16px', fontWeight: 600, cursor: askBusy ? 'wait' : 'pointer', opacity: askBusy || !question.trim() ? 0.5 : 1 }}
        >
          {askBusy ? '回答中… (Claude 応答待ち)' : '質問する'}
        </button>
        {askResult && (
          <div style={{ marginTop: 12, padding: 12, background: '#0f1115', border: '1px solid #262a33', borderRadius: 6 }}>
            {askResult.error ? (
              <p style={{ color: 'tomato', margin: 0 }}>エラー: {askResult.error}</p>
            ) : (
              <>
                <div style={{ whiteSpace: 'pre-wrap', fontSize: 13, lineHeight: 1.7 }}>{askResult.answer}</div>
                <div style={{ marginTop: 8, fontSize: 11, opacity: 0.5 }}>
                  所要時間: {fmtDurationMs(askResult.duration_ms)}
                </div>
              </>
            )}
          </div>
        )}
      </section>
      </>)}

      {/* 使わないため非表示。再表示は SHOW_CLAUDE_TOOLS=true に。 */}
      {SHOW_CLAUDE_TOOLS && (
      <section style={cardStyle}>
        <h2>Claude の直近の判断結果 (直近 3 件)</h2>
        <p style={{ margin: '0 0 8px 0', opacity: 0.75, fontSize: 13 }}>
          自動 (定時)・手動 (ボタン)・イベント (Claude が短間隔を要求) の 3 源で生成された設定の履歴。
          「採用中」が現在 bot が実行している判断。
        </p>
        {decisions.length === 0 ? (
          <p>まだ判断履歴がありません</p>
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
            {decisions.map((d) => {
              const st = STATUS_LABEL[d.status] ?? { text: d.status, color: '#888', icon: '·' }
              const isNoTrade = d.strategy_name === 'no_trade' || !d.enabled
              const regimeLabel = REGIME_LABEL[d.market_regime_type] ?? d.market_regime_type
              const stratLabel = STRATEGY_LABEL[d.strategy_name] ?? d.strategy_name
              return (
                <div
                  key={d.config_id}
                  style={{
                    padding: 10,
                    background: '#0f1115',
                    border: '1px solid #262a33',
                    borderLeft: `3px solid ${st.color}`,
                    borderRadius: 6,
                    fontSize: 13,
                  }}
                >
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 4 }}>
                    <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                      <span style={{ color: st.color, fontWeight: 600 }}>
                        {st.icon} {st.text}
                      </span>
                      {(() => {
                        const src = SOURCE_LABEL[d.source] ?? { text: d.source, color: '#888', bg: '#1a1d22' }
                        return (
                          <span
                            style={{
                              fontSize: 10,
                              fontWeight: 600,
                              padding: '2px 6px',
                              borderRadius: 3,
                              background: src.bg,
                              color: src.color,
                              border: `1px solid ${src.color}33`,
                            }}
                          >
                            {src.text}
                          </span>
                        )
                      })()}
                    </span>
                    <span style={{ fontSize: 11, opacity: 0.6 }}>
                      {fmtJST(d.created_at)}
                    </span>
                  </div>
                  <div style={{ marginBottom: 4 }}>
                    <strong>判断:</strong> {stratLabel}
                    {!isNoTrade && (
                      <span style={{ opacity: 0.75 }}>
                        {' '}· 市況: {regimeLabel} (信頼度 {(d.market_regime_confidence * 100).toFixed(0)}%)
                      </span>
                    )}
                  </div>
                  {d.market_regime_reason && (
                    <div style={{ marginBottom: 4, opacity: 0.85 }}>
                      <strong>Claudeの理由:</strong>{' '}
                      <span style={{ fontStyle: 'italic' }}>「{d.market_regime_reason}」</span>
                    </div>
                  )}
                  {isNoTrade && d.no_trade_reason && (
                    <div style={{ marginBottom: 4, opacity: 0.85 }}>
                      <strong>見送り理由:</strong>{' '}
                      <span style={{ fontStyle: 'italic' }}>「{d.no_trade_reason}」</span>
                    </div>
                  )}
                  <div style={{ marginTop: 6, paddingTop: 6, borderTop: '1px dashed #262a33' }}>
                    <strong style={{ color: '#9ad' }}>→ bot への影響:</strong>{' '}
                    {describeImpact(d)}
                  </div>
                  {d.status === 'rejected' && d.reject_reason && (
                    <div style={{ marginTop: 4, color: 'tomato', fontSize: 12 }}>
                      却下理由: {d.reject_reason}
                    </div>
                  )}
                  <div style={{ marginTop: 4, fontSize: 10, opacity: 0.5 }}>
                    {d.config_id} · 有効期間 {fmtJST(d.valid_from)} 〜 {fmtJST(d.valid_until)}
                    {d.next_advisor_run_in_minutes ? ` · 次回再評価 ${d.next_advisor_run_in_minutes}分後` : ''}
                  </div>
                </div>
              )
            })}
          </div>
        )}
      </section>
      )}

      <TradeHistory trades={trades.slice(0, 50)} cardStyle={cardStyle} />

      <footer style={{ opacity: 0.6, fontSize: 12 }}>
        5 秒ごとに更新 · Go API: {process.env.NEXT_PUBLIC_API_BASE || 'http://127.0.0.1:8080'}
      </footer>
    </main>
  )
}
