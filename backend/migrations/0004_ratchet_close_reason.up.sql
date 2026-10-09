-- Ratchet TP (0003_ratchet_takeprofit.up.sql で導入) が close 時に返す
-- close_reason="ratchet_takeprofit" を trades.close_reason の CHECK 制約に追加。
-- 0003 で列追加のみして CHECK を拡張し忘れていたため、本番で ratchet TP が
-- 発火すると trades INSERT が CHECK 違反で失敗し、close saga が止まる状態だった。
-- (manage_open_positions_exits.go の ratchet 利確判定が "ratchet_takeprofit" を返す)

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
