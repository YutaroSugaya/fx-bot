-- session flatten (セッションガード) が close 時に返す
-- close_reason="session_flatten" を trades.close_reason の CHECK 制約に追加。
-- 0010 と同じ不具合クラス対策: evaluator が新 reason を返すのに制約に入れ忘れると
-- close saga の trade INSERT が CHECK 違反で止まる。
-- (manage_open_positions_exits.go の evaluateSessionFlattenExit が返す。
--  毎朝 05:30 JST に残玉を強制手仕舞い = 05:45 スプレッド壁 + 週末ギャップ対策)

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
        'broker_close',
        'session_flatten'
    ));
