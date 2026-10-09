-- Per-position entry-time cost capture.
--   entry_fee_jpy:       GMO が entry fill で報告した手数料 (約定金額×0.002%/leg)。
--                        NULL = 未捕捉 (この migration 以前に建った行 / resolve 不能)。
--                        0 = broker が 0 と報告 (API 手数料無料期間など) — NULL と区別する。
--   entry_spread_pips:   admission 時点の実測スプレッド (ticker bid/ask)。NULL = 未捕捉。
--                        live フォワードが backtest スプレッドモデルの較正データ源になる。
--   entry_slippage_pips: 符号付き adverse slippage = BUY: (fill-ask)/pip, SELL: (bid-fill)/pip。
--                        正 = 不利方向。NULL = 未捕捉 (ticker 無し等)。
-- ⚠️ 列追加のみ (全て nullable・default なし)。既存行・稼働 bot へは無影響。
--    値を埋めるのは execute_order / manual_trade の entry 経路 (要 bot 再起動)。
-- close 時の roundtrip fee 合成は trades.fee_jpy 側 (= entry_fee_jpy + close leg fee)。
ALTER TABLE positions ADD COLUMN IF NOT EXISTS entry_fee_jpy DOUBLE PRECISION;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS entry_spread_pips DOUBLE PRECISION;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS entry_slippage_pips DOUBLE PRECISION;
