-- Roll back 0004_ratchet_close_reason.up.sql.
-- Note: 既存に close_reason='ratchet_takeprofit' の trades 行が残っていると
-- ALTER TABLE ... ADD CONSTRAINT が失敗する。down 適用時は事前に該当行を
-- 別 reason に書き換えるか archive 済みであることが前提。

ALTER TABLE trades
    DROP CONSTRAINT trades_close_reason_check;

ALTER TABLE trades
    ADD CONSTRAINT trades_close_reason_check
    CHECK (close_reason IN (
        'take_profit',
        'stop_loss',
        'max_hold',
        'manual',
        'reconcile_cold_close'
    ));
