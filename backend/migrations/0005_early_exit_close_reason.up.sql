-- Early-exit window が close 時に返す close_reason="early_exit" を
-- trades.close_reason の CHECK 制約に追加する。
--
-- 背景: evaluateMaxHoldExit の early-exit 分岐は従来 "max_hold" を返しており、
-- 本来の soft/hard deadline close と区別できず集計を歪めていた。
-- manage_open_positions_exits.go の early-exit 分岐を "early_exit"
-- に変更したため、本制約を拡張しないと early-exit 発火時の trades INSERT が
-- CHECK 違反で失敗し close saga が止まる (0003→0004 と同じ不具合クラス)。

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
