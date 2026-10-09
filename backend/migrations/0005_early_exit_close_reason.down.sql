-- Roll back 0005_early_exit_close_reason.up.sql.
-- Note: 既存に close_reason='early_exit' の trades 行が残っていると
-- ADD CONSTRAINT が失敗する。down 適用時は事前に該当行を 'max_hold' に
-- 書き換えるか archive 済みであることが前提 (early_exit は元々 max_hold
-- 扱いだったので 'max_hold' へ戻すのが意味的に正しい)。

ALTER TABLE trades
    DROP CONSTRAINT trades_close_reason_check;

ALTER TABLE trades
    ADD CONSTRAINT trades_close_reason_check
    CHECK (close_reason IN (
        'take_profit',
        'stop_loss',
        'max_hold',
        'manual',
        'reconcile_cold_close',
        'ratchet_takeprofit'
    ));
