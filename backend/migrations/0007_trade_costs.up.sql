-- Per-trade cost columns on trades.
-- profit_loss_jpy は GROSS のまま維持し (既存集計との互換を守る)、
-- net = gross - fee_jpy + swap_jpy を導出側で計算する。
--   fee_jpy:       GMO 手数料 (約定金額×0.002%×往復)。close 時に Execution.FeeJPY から焼く。
--   swap_jpy:      跨ぎスワップ (Execution.SettledSwapJPY)。
--   fee_estimated: true = 0.002% 推定 backfill 行 / false = broker 実報告値。
-- ⚠️ 列追加のみ (NOT NULL + DEFAULT 0/false)。既存行は 0/false のまま・稼働 bot へは無影響。
--    値は close saga が close 時に書く (close_saga.go の composeLiveCloseCosts)。
ALTER TABLE trades ADD COLUMN IF NOT EXISTS fee_jpy DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE trades ADD COLUMN IF NOT EXISTS swap_jpy DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE trades ADD COLUMN IF NOT EXISTS fee_estimated BOOLEAN NOT NULL DEFAULT false;
