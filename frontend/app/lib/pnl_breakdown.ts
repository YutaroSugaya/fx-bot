// 通貨ペアごと / 1日ごとの戦績集計。UI の #現在の収益 セクションで
// per-pair / per-day テーブルを出すための純粋関数群。computePnL (page.tsx)
// と同じく closed (closed_at あり & exit_price>0) のみを対象にする。
//
// 「1日」の境界は GMO の本日損益に合わせ 6:00 JST (NY クローズ = 営業日境界) で切る。
// 0:00 JST 暦日ではない。ダッシュボードの「本日損益」を
// GMO 画面の本日損益と一致させるのが目的。境界の唯一の実装点は tradingDayKey。

import type { Trade } from './types'

// PnLBucket は1グループ (1ペア or 1日) の戦績サマリ。
export type PnLBucket = {
  count: number
  wins: number
  losses: number
  winRate: number // %
  sumJPY: number
  sumPips: number
}

export type SymbolPnL = PnLBucket & { symbol: string }
export type DayPnL = PnLBucket & { date: string } // date = 営業日の開始日 (YYYY-MM-DD, 6:00 JST 始まり)

// GMO の本日損益が切り替わる時刻 (JST)。毎朝 6:00 固定。
// GMO / 国内 FX の営業日境界 (NY クローズ) に相当。冬時間は 7:00 になるが、
// 単純さを優先して夏冬を区別せず 6:00 固定とする (冬は 1 時間ずれる)。
export const TRADING_DAY_BOUNDARY_HOUR_JST = 6

const jstDateFormatter = new Intl.DateTimeFormat('en-CA', {
  timeZone: 'Asia/Tokyo',
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
})

// jstDateKey は ISO 文字列を JST の暦日 (YYYY-MM-DD) に変換する。
// en-CA ロケールは YYYY-MM-DD 形式を返すのでソート可能なキーになる。
// parse 不能 / 空文字は空文字を返す (UI を壊さないため)。
export function jstDateKey(iso: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (isNaN(d.getTime())) return ''
  return jstDateFormatter.format(d)
}

// tradingDayKey は ISO 文字列を「GMO 営業日」キー (YYYY-MM-DD) に変換する。
// 営業日 D = D 06:00 JST 〜 (D+1) 05:59:59 JST で、キーは営業日開始日 (D)。
// 6:00 JST より前の時刻は前営業日に属する (例: 7/8 03:00 JST → "2026-07-07")。
// 実装: 瞬間を境界時間ぶん巻き戻してから JST 暦日を取る。こうすると 6:00 が
// ちょうど暦日の 0:00 に落ちるので、あとは jstDateKey と同じ暦日ロジックで済む。
// parse 不能 / 空文字は空文字を返す (UI を壊さないため)。
export function tradingDayKey(iso: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (isNaN(d.getTime())) return ''
  const shifted = new Date(d.getTime() - TRADING_DAY_BOUNDARY_HOUR_JST * 60 * 60 * 1000)
  return jstDateFormatter.format(shifted)
}

function isClosed(t: Trade): boolean {
  return Boolean(t.closed_at) && t.exit_price > 0
}

// filterSinceEpoch は opened_at (エントリー時刻) が epoch 以降 (境界含む) の trade だけ残す
// 表示専用フィルタ。ダッシュボードの戦績を「今動かしている戦略」の期間だけに絞るために使う。
// 判定が closed_at でなく opened_at なのは、トレードは「建てた時点の戦略」に帰属する
// (config は建玉時に凍結保存) ため。epoch より前に建てた持ち越し玉が epoch 後に決済
// されても、旧戦略の成績を新戦略の戦績に混ぜない。
// DB もバックエンドの反省ループ epoch (reflection_start_at) も一切変更しない — 表示だけ。
// epoch が空文字 / parse 不能 → フィルタしない (全件返す)。
// opened_at が空 / parse 不能な trade は epoch 指定時は「期間内と確認できない」ので除外する。
export function filterSinceEpoch(trades: Trade[], epochISO: string): Trade[] {
  if (!epochISO) return trades
  const floor = new Date(epochISO).getTime()
  if (isNaN(floor)) return trades
  return trades.filter((t) => {
    if (!t.opened_at) return false
    const opened = new Date(t.opened_at).getTime()
    if (isNaN(opened)) return false
    return opened >= floor
  })
}

function emptyBucket(): PnLBucket {
  return { count: 0, wins: 0, losses: 0, winRate: 0, sumJPY: 0, sumPips: 0 }
}

function accumulate(b: PnLBucket, t: Trade): void {
  b.count += 1
  if (t.profit_loss_jpy > 0) b.wins += 1
  else if (t.profit_loss_jpy < 0) b.losses += 1
  b.sumJPY += t.profit_loss_jpy
  b.sumPips += t.profit_loss_pips
}

function finalize(b: PnLBucket): void {
  b.winRate = b.count === 0 ? 0 : (b.wins / b.count) * 100
}

// aggregateBySymbol は通貨ペアごとに戦績を集計し、合計損益 (sumJPY) 降順で返す。
export function aggregateBySymbol(trades: Trade[]): SymbolPnL[] {
  const groups = new Map<string, PnLBucket>()
  for (const t of trades) {
    if (!isClosed(t)) continue
    let b = groups.get(t.symbol)
    if (!b) {
      b = emptyBucket()
      groups.set(t.symbol, b)
    }
    accumulate(b, t)
  }
  const rows: SymbolPnL[] = []
  for (const [symbol, b] of groups) {
    finalize(b)
    rows.push({ symbol, ...b })
  }
  rows.sort((a, b) => b.sumJPY - a.sumJPY)
  return rows
}

// aggregateByDay は GMO 営業日 (6:00 JST 始まり) ごとに戦績を集計し、日付降順 (新しい日が先) で返す。
export function aggregateByDay(trades: Trade[]): DayPnL[] {
  const groups = new Map<string, PnLBucket>()
  for (const t of trades) {
    if (!isClosed(t)) continue
    const date = tradingDayKey(t.closed_at)
    if (!date) continue
    let b = groups.get(date)
    if (!b) {
      b = emptyBucket()
      groups.set(date, b)
    }
    accumulate(b, t)
  }
  const rows: DayPnL[] = []
  for (const [date, b] of groups) {
    finalize(b)
    rows.push({ date, ...b })
  }
  rows.sort((a, b) => (a.date < b.date ? 1 : a.date > b.date ? -1 : 0))
  return rows
}
