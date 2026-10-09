-- Revert the trade-cost columns.
ALTER TABLE trades DROP COLUMN IF EXISTS fee_estimated;
ALTER TABLE trades DROP COLUMN IF EXISTS swap_jpy;
ALTER TABLE trades DROP COLUMN IF EXISTS fee_jpy;
