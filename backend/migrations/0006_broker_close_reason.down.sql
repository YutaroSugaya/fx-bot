-- Roll back 0006_broker_close_reason.up.sql.
-- Note: 既存に close_reason='broker_close' の trades 行が残っていると
-- ADD CONSTRAINT が失敗する。down 適用時は事前に該当行を 'manual' に
-- 書き換えるか archive 済みであることが前提 (broker-side 手動 close は
-- 'manual' へ寄せるのが意味的に近い)。

ALTER TABLE trades
    DROP CONSTRAINT trades_close_reason_check;

ALTER TABLE trades
    ADD CONSTRAINT trades_close_reason_check
    CHECK (close_reason IN (
        'take_profit',
        'stop_loss',
        'max_hold',
        'early_exit',
        'manual',
        'reconcile_cold_close',
        'ratchet_takeprofit'
    ));
