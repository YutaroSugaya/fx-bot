-- Trailing STOP (0009_loss_ratchet.up.sql で導入) が close 時に返す
-- close_reason="ratchet_stoploss" を trades.close_reason の CHECK 制約に追加。
-- 0003→0004 / 0005 / 0006 と同じ「close_reason を制約に入れ忘れると close saga
-- が CHECK 違反で止まる」不具合クラスを繰り返さないため、列追加と同時に拡張する。
-- (manage_open_positions_exits.go の evaluateLossRatchetExit が "ratchet_stoploss" を返す)

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
        'ratchet_takeprofit',
        'ratchet_stoploss',
        'broker_close'
    ));
