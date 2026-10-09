-- Revert to the 0006 close_reason set (drop 'ratchet_stoploss').
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
        'broker_close'
    ));
