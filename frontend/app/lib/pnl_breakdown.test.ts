import { describe, it, expect } from 'vitest'
import { jstDateKey, tradingDayKey, aggregateBySymbol, aggregateByDay, filterSinceEpoch } from './pnl_breakdown'
import type { Trade } from './types'

// 通貨ペアごと / 1日ごとの戦績集計 (UI #現在の収益 の per-pair / per-day テーブル用)。
// computePnL と同じく closed (closed_at あり & exit_price>0) のみ対象。
// 「1日」の境界は GMO の本日損益に合わせ 6:00 JST (NY クローズ = 営業日境界) で切る。
// 0:00 JST 暦日ではない。tradingDayKey がその唯一の実装点。

function trade(over: Partial<Trade>): Trade {
  return {
    position_id: 1,
    strategy_config_id: 'cfg',
    symbol: 'USD_JPY',
    side: 'BUY',
    quantity: 1000,
    entry_price: 150,
    exit_price: 150.1,
    profit_loss_pips: 10,
    profit_loss_jpy: 100,
    close_reason: 'take_profit',
    opened_at: '2026-06-24T00:00:00Z',
    closed_at: '2026-06-24T01:00:00Z',
    ...over,
  }
}

describe('jstDateKey', () => {
  it('UTC を JST (+9) の YYYY-MM-DD に変換する', () => {
    // 2026-06-24 16:00 UTC = 2026-06-25 01:00 JST → 日付が進む
    expect(jstDateKey('2026-06-24T16:00:00Z')).toBe('2026-06-25')
  })

  it('JST 同日内は同じキー', () => {
    expect(jstDateKey('2026-06-24T00:00:00Z')).toBe('2026-06-24') // 09:00 JST
    expect(jstDateKey('2026-06-24T14:59:00Z')).toBe('2026-06-24') // 23:59 JST
  })

  it('parse 不能は空文字を返す (UI を壊さない)', () => {
    expect(jstDateKey('')).toBe('')
    expect(jstDateKey('not-a-date')).toBe('')
  })
})

describe('tradingDayKey', () => {
  // GMO の本日損益は毎朝 6:00 JST で日を切り替える。
  // 営業日 D = D 06:00 JST 〜 (D+1) 05:59:59 JST。ラベルは営業日の開始日 (D)。
  it('6:00 JST ちょうどはその営業日の始まり', () => {
    expect(tradingDayKey('2026-07-07T06:00:00+09:00')).toBe('2026-07-07')
  })

  it('6:00 JST 直前 (5:59:59) は前営業日に属する', () => {
    expect(tradingDayKey('2026-07-07T05:59:59+09:00')).toBe('2026-07-06')
  })

  it('深夜 0〜6 時 JST はまだ前営業日 (GMO の「本日」が前日を指す時間帯)', () => {
    // 7/8 03:00 JST は GMO では前日 (7/7) の「本日損益」に入っている
    expect(tradingDayKey('2026-07-08T03:00:00+09:00')).toBe('2026-07-07')
  })

  it('同営業日内 (昼〜深夜手前) は同じキー', () => {
    expect(tradingDayKey('2026-07-07T12:00:00+09:00')).toBe('2026-07-07')
    expect(tradingDayKey('2026-07-07T23:59:00+09:00')).toBe('2026-07-07')
  })

  it('parse 不能 / 空文字は空文字を返す (UI を壊さない)', () => {
    expect(tradingDayKey('')).toBe('')
    expect(tradingDayKey('not-a-date')).toBe('')
  })
})

describe('aggregateBySymbol', () => {
  it('通貨ペアごとに件数 / 勝敗 / 損益を集計し sumJPY 降順で返す', () => {
    const trades: Trade[] = [
      trade({ symbol: 'USD_JPY', profit_loss_jpy: 100, profit_loss_pips: 10 }),
      trade({ symbol: 'USD_JPY', profit_loss_jpy: -40, profit_loss_pips: -4 }),
      trade({ symbol: 'EUR_JPY', profit_loss_jpy: 300, profit_loss_pips: 30 }),
    ]
    const rows = aggregateBySymbol(trades)
    expect(rows).toHaveLength(2)
    // EUR_JPY (+300) が USD_JPY (+60) より上
    expect(rows[0].symbol).toBe('EUR_JPY')
    expect(rows[0].sumJPY).toBe(300)
    expect(rows[0].count).toBe(1)
    expect(rows[0].wins).toBe(1)
    expect(rows[0].losses).toBe(0)
    expect(rows[1].symbol).toBe('USD_JPY')
    expect(rows[1].sumJPY).toBe(60)
    expect(rows[1].sumPips).toBe(6)
    expect(rows[1].count).toBe(2)
    expect(rows[1].wins).toBe(1)
    expect(rows[1].losses).toBe(1)
    expect(rows[1].winRate).toBe(50)
  })

  it('未確定 (closed_at 無 / exit_price 0) は除外する', () => {
    const trades: Trade[] = [
      trade({ symbol: 'USD_JPY', closed_at: '', exit_price: 0 }),
      trade({ symbol: 'USD_JPY', profit_loss_jpy: 100 }),
    ]
    const rows = aggregateBySymbol(trades)
    expect(rows).toHaveLength(1)
    expect(rows[0].count).toBe(1)
  })

  it('空配列 → 空配列', () => {
    expect(aggregateBySymbol([])).toEqual([])
  })
})

describe('aggregateByDay', () => {
  it('営業日ごとに集計し日付降順 (新しい日が先) で返す', () => {
    const trades: Trade[] = [
      trade({ closed_at: '2026-06-23T01:00:00Z', profit_loss_jpy: 50, profit_loss_pips: 5 }), // 6/23 10:00 JST
      trade({ closed_at: '2026-06-24T01:00:00Z', profit_loss_jpy: 100, profit_loss_pips: 10 }), // 6/24 10:00 JST
      trade({ closed_at: '2026-06-24T02:00:00Z', profit_loss_jpy: -30, profit_loss_pips: -3 }), // 6/24 11:00 JST
    ]
    const rows = aggregateByDay(trades)
    expect(rows).toHaveLength(2)
    expect(rows[0].date).toBe('2026-06-24')
    expect(rows[0].sumJPY).toBe(70)
    expect(rows[0].sumPips).toBe(7)
    expect(rows[0].count).toBe(2)
    expect(rows[0].wins).toBe(1)
    expect(rows[0].losses).toBe(1)
    expect(rows[1].date).toBe('2026-06-23')
    expect(rows[1].sumJPY).toBe(50)
  })

  it('6:00 JST の営業日境界でまとまる (深夜決済は前営業日・GMO の日切りに一致)', () => {
    const trades: Trade[] = [
      // 7/7 23:00 JST (= 7/7 14:00 UTC) → 営業日 7/7
      trade({ closed_at: '2026-07-07T14:00:00Z', profit_loss_jpy: 100 }),
      // 7/8 02:00 JST (= 7/7 17:00 UTC) → まだ 6 時前 → 営業日は 7/7
      trade({ closed_at: '2026-07-07T17:00:00Z', profit_loss_jpy: 50 }),
      // 7/8 07:00 JST (= 7/7 22:00 UTC) → 6 時を過ぎた → 営業日 7/8
      trade({ closed_at: '2026-07-07T22:00:00Z', profit_loss_jpy: -30 }),
    ]
    const rows = aggregateByDay(trades)
    expect(rows).toHaveLength(2)
    expect(rows[0].date).toBe('2026-07-08')
    expect(rows[0].sumJPY).toBe(-30)
    expect(rows[0].count).toBe(1)
    expect(rows[1].date).toBe('2026-07-07')
    expect(rows[1].sumJPY).toBe(150) // 100 + 50 が同じ営業日にまとまる
    expect(rows[1].count).toBe(2)
  })

  it('空配列 → 空配列', () => {
    expect(aggregateByDay([])).toEqual([])
  })
})

describe('filterSinceEpoch', () => {
  // ダッシュボードを「今動かしている戦略」の期間だけに絞る表示フィルタ。
  // 判定は opened_at (エントリー時刻) 基準。
  // トレードは「建てた時点の戦略」に帰属する (config は建玉時に凍結保存される) ので、
  // epoch より前に建てた玉が epoch 後に決済されても旧戦略の成績 = 表示しない。
  // epoch = 2026-07-01 00:00 JST (= 2026-06-30T15:00:00Z)。
  const epoch = '2026-07-01T00:00:00+09:00'

  it('epoch 以降にエントリーした trade だけ残す', () => {
    const trades: Trade[] = [
      trade({ symbol: 'GBP_JPY', opened_at: '2026-06-26T04:00:00Z', closed_at: '2026-06-26T05:00:00Z' }), // 旧戦略 → 除外
      trade({ symbol: 'USD_JPY', opened_at: '2026-07-01T00:08:00Z', closed_at: '2026-07-01T01:08:00Z' }), // epoch 後エントリー → 残す
    ]
    const rows = filterSinceEpoch(trades, epoch)
    expect(rows).toHaveLength(1)
    expect(rows[0].symbol).toBe('USD_JPY')
  })

  it('epoch 前に建てた持ち越し玉は epoch 後に close されても除外する', () => {
    // 戦略切替をまたぐ建玉: 旧戦略でエントリー → 新 epoch 後に決済。
    // closed_at 基準だと混入するのが opened_at 基準へ変えた理由。
    const trades: Trade[] = [
      trade({ opened_at: '2026-06-30T14:00:00Z', closed_at: '2026-07-01T05:00:00Z' }),
    ]
    expect(filterSinceEpoch(trades, epoch)).toHaveLength(0)
  })

  it('境界 (opened_at == epoch) は含める', () => {
    const trades: Trade[] = [trade({ opened_at: '2026-06-30T15:00:00Z', closed_at: '2026-07-01T01:00:00Z' })] // = epoch ちょうど
    expect(filterSinceEpoch(trades, epoch)).toHaveLength(1)
  })

  it('epoch が空文字ならフィルタしない (全件返す = 表示無効化)', () => {
    const trades: Trade[] = [
      trade({ opened_at: '2026-06-26T04:00:00Z' }),
      trade({ opened_at: '2026-07-01T00:08:00Z' }),
    ]
    expect(filterSinceEpoch(trades, '')).toHaveLength(2)
  })

  it('opened_at が空 / parse 不能な trade は epoch 指定時に除外する', () => {
    const trades: Trade[] = [
      trade({ opened_at: '' }),
      trade({ opened_at: 'not-a-date' }),
      trade({ opened_at: '2026-07-01T00:08:00Z' }),
    ]
    expect(filterSinceEpoch(trades, epoch)).toHaveLength(1)
  })

  it('空配列 → 空配列', () => {
    expect(filterSinceEpoch([], epoch)).toEqual([])
  })
})
