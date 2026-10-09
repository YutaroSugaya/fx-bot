// 「取引履歴 (直近 50 件)」セクションのコンポーネント。
// 同セクションは backend の TradeView (lib/types.ts) を読むだけで状態を持たない
// presentational component なので props は trades のみ。card styling は呼出側から渡す。

import type { CSSProperties } from 'react'
import type { Trade } from '../lib/types'
import { fmtJPY, fmtPips, pnlColor } from '../lib/format_pnl'
import { fmtJST } from '../lib/format_time'
import { SIDE_LABEL, CLOSE_REASON_LABEL } from '../lib/labels'
import { LOT_SIZE_USDJPY } from '../lib/constants'
import { tradeOriginLabel, isDiscretionaryOrigin } from '../lib/trade_origin'

type Props = {
  trades: Trade[]
  cardStyle: CSSProperties
}

export function TradeHistory({ trades, cardStyle }: Props) {
  return (
    <section style={cardStyle}>
      <h2>取引履歴 (直近 50 件)</h2>
      {trades.length === 0 ? (
        <p>取引履歴はまだありません</p>
      ) : (
        <table style={{ width: '100%', fontSize: 12, borderCollapse: 'collapse' }}>
          <thead>
            <tr>
              <th align="left">決済時刻</th>
              <th align="left">方向</th>
              <th align="right">数量 (lot)</th>
              <th align="right">エントリー</th>
              <th align="right">決済価格</th>
              <th align="right">損益 (pips)</th>
              <th align="right">損益 (円)</th>
              <th align="left">決済理由</th>
              <th align="left">設定 ID</th>
            </tr>
          </thead>
          <tbody>
            {trades.map((t, i) => (
              <tr key={i} style={{ borderTop: '1px solid #262a33' }}>
                <td>{fmtJST(t.closed_at)}</td>
                <td>{SIDE_LABEL[t.side] ?? t.side}</td>
                <td align="right">
                  {(t.quantity / LOT_SIZE_USDJPY).toLocaleString(undefined, { maximumFractionDigits: 3 })}
                </td>
                <td align="right">{t.entry_price}</td>
                <td align="right">{t.exit_price}</td>
                <td align="right" style={{ color: pnlColor(t.profit_loss_pips) }}>
                  {fmtPips(t.profit_loss_pips)}
                </td>
                <td align="right" style={{ color: pnlColor(t.profit_loss_jpy) }}>
                  {fmtJPY(t.profit_loss_jpy)}
                </td>
                <td>{CLOSE_REASON_LABEL[t.close_reason] ?? t.close_reason}</td>
                <td>
                  {isDiscretionaryOrigin(t.origin) ? (
                    <span
                      title="bot ではなく裁量(手動/外部)で建てた玉。bot の成績には含めていません"
                      style={{
                        padding: '1px 6px',
                        borderRadius: 4,
                        border: '1px solid #6b7280',
                        color: '#cbd5e1',
                        fontSize: 11,
                      }}
                    >
                      {tradeOriginLabel(t.origin, t.strategy_config_id)}
                    </span>
                  ) : (
                    t.strategy_config_id
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}
