-- 0008 down: entry-time cost capture 列を落とす。較正データ (entry_spread_pips /
-- entry_slippage_pips) と entry_fee_jpy の実測値は失われる (down は data loss 許容)。
ALTER TABLE positions DROP COLUMN IF EXISTS entry_slippage_pips;
ALTER TABLE positions DROP COLUMN IF EXISTS entry_spread_pips;
ALTER TABLE positions DROP COLUMN IF EXISTS entry_fee_jpy;
