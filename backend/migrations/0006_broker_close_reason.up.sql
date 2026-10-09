-- reconcile が GMO 側の実約定から復元する close_reason="broker_close" を
-- trades.close_reason の CHECK 制約に追加する。
--
-- 背景: findFillByPositionLookup は GMO /v1/
-- latestExecutions から実 close fill を復元できていたが、その exit price が
-- TP/SL の理論価格に一致しない close (GMO アプリ手動決済・建値付近の broker
-- 都合 close 等) を classifyCloseReason が "broker_close" と分類する。ところが
-- この値が CHECK 制約に無かったため CloseAndRecord が制約違反で失敗し、
-- resolveAndRecordClose がフォールバックして synthetic 0-PnL (reconcile_cold_
-- close) で記録 → 実際の損益が帳簿に乗らない状態になっていた。
-- 0003→0004 / 0005 と同じ「close_reason を制約に入れ忘れ」不具合クラス。
-- 本制約を拡張することで、TP/SL 非一致の broker-side close も実 fill で記録される。

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
