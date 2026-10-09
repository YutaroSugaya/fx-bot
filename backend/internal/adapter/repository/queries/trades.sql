-- name: InsertTrade :one
-- fee_jpy / swap_jpy / fee_estimated は migration 0007 列。
-- profit_loss_jpy は GROSS のまま — net は導出側 (edge_metrics) で計算する。
INSERT INTO trades (
  position_id, strategy_config_id, symbol, side, quantity,
  entry_price, exit_price, profit_loss_pips, profit_loss_jpy,
  close_reason, fee_jpy, swap_jpy, fee_estimated, opened_at, closed_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING id;

-- name: InsertTradeSignal :exec
INSERT INTO trade_signals (trade_id, signal_id) VALUES ($1, $2);

-- name: ListTradesSinceOpened :many
-- origin discriminates bot trades from the operator's own discretionary
-- trades (manual_trade command = manual_positions; GMO-app entry adopted by
-- reconcile = recovered_positions.external_broker_adoption). The trades row's
-- strategy_config_id only borrows the active config as an FK target, so this
-- read-time JOIN is what keeps the dashboard label / edge metrics honest.
SELECT t.position_id, COALESCE(ts.signal_id,'')::text AS signal_id,
       t.strategy_config_id, t.symbol, t.side, t.quantity,
       t.entry_price, t.exit_price, t.profit_loss_pips, t.profit_loss_jpy,
       t.close_reason, t.fee_jpy, t.swap_jpy, t.fee_estimated,
       t.opened_at, t.closed_at,
       (CASE
          WHEN mp.position_id IS NOT NULL THEN 'manual'
          WHEN rp.recovery_reason = 'external_broker_adoption' THEN 'external'
          ELSE 'bot'
        END)::text AS origin
FROM trades t
LEFT JOIN trade_signals ts ON ts.trade_id = t.id
LEFT JOIN manual_positions mp ON mp.position_id = t.position_id
LEFT JOIN recovered_positions rp ON rp.position_id = t.position_id
WHERE t.opened_at >= $1
ORDER BY t.opened_at DESC
LIMIT $2;

-- name: SumOpenedLossJPYSince :one
-- NET of GMO fee + swap (= real money lost): a gross-positive trade eaten by the fee counts as a loss.
SELECT COALESCE(SUM(CASE WHEN (profit_loss_jpy - fee_jpy + swap_jpy) < 0 THEN -(profit_loss_jpy - fee_jpy + swap_jpy) ELSE 0 END), 0)::float8 AS loss_jpy
FROM trades
WHERE opened_at >= $1;

-- name: CountOpenedSince :one
SELECT COUNT(*)::int8 AS count
FROM trades
WHERE opened_at >= $1;

-- name: ListTradesClosedSince :many
SELECT t.position_id, COALESCE(ts.signal_id,'')::text AS signal_id,
       t.strategy_config_id, t.symbol, t.side, t.quantity,
       t.entry_price, t.exit_price, t.profit_loss_pips, t.profit_loss_jpy,
       t.close_reason, t.fee_jpy, t.swap_jpy, t.fee_estimated,
       t.opened_at, t.closed_at,
       (CASE
          WHEN mp.position_id IS NOT NULL THEN 'manual'
          WHEN rp.recovery_reason = 'external_broker_adoption' THEN 'external'
          ELSE 'bot'
        END)::text AS origin
FROM trades t
LEFT JOIN trade_signals ts ON ts.trade_id = t.id
LEFT JOIN manual_positions mp ON mp.position_id = t.position_id
LEFT JOIN recovered_positions rp ON rp.position_id = t.position_id
WHERE t.closed_at >= $1
ORDER BY t.closed_at DESC
LIMIT $2;

-- name: SumClosedLossJPYSince :one
-- NET of GMO fee + swap (= real money lost): a gross-positive trade eaten by the fee counts as a loss.
SELECT COALESCE(SUM(CASE WHEN (profit_loss_jpy - fee_jpy + swap_jpy) < 0 THEN -(profit_loss_jpy - fee_jpy + swap_jpy) ELSE 0 END), 0)::float8 AS loss_jpy
FROM trades
WHERE closed_at >= $1;

-- name: CountClosedSince :one
SELECT COUNT(*)::int8 AS count
FROM trades
WHERE closed_at >= $1;

-- name: SumPnLJPYClosedSinceBySymbol :one
-- Dashboard 用: symbol-scoped 直近 24h 実現 PnL。NET of GMO fee + swap.
SELECT COALESCE(SUM(profit_loss_jpy - fee_jpy + swap_jpy), 0)::float8 AS pnl_jpy
FROM trades
WHERE symbol = $1 AND closed_at >= $2;

-- name: ListTradesClosedBySymbolSince :many
-- Per-symbol close ledger for the risk gate's per-symbol caps. Mirrors
-- ListTradesClosedSince but filters by symbol so a sibling symbol's
-- trades cannot pollute this bundle's MaxLossInThisWindow / MaxTradesInWindow.
SELECT t.position_id, COALESCE(ts.signal_id,'')::text AS signal_id,
       t.strategy_config_id, t.symbol, t.side, t.quantity,
       t.entry_price, t.exit_price, t.profit_loss_pips, t.profit_loss_jpy,
       t.close_reason, t.fee_jpy, t.swap_jpy, t.fee_estimated,
       t.opened_at, t.closed_at,
       (CASE
          WHEN mp.position_id IS NOT NULL THEN 'manual'
          WHEN rp.recovery_reason = 'external_broker_adoption' THEN 'external'
          ELSE 'bot'
        END)::text AS origin
FROM trades t
LEFT JOIN trade_signals ts ON ts.trade_id = t.id
LEFT JOIN manual_positions mp ON mp.position_id = t.position_id
LEFT JOIN recovered_positions rp ON rp.position_id = t.position_id
WHERE t.symbol = $1 AND t.closed_at >= $2
ORDER BY t.closed_at DESC
LIMIT $3;

-- name: SumClosedLossJPYBySymbolSince :one
-- Per-symbol loss aggregate for MaxLossInThisWindowJPY / MaxDailyLossJPY
-- per-symbol caps.
-- NET of GMO fee + swap (= real money lost): a gross-positive trade eaten by the fee counts as a loss.
SELECT COALESCE(SUM(CASE WHEN (profit_loss_jpy - fee_jpy + swap_jpy) < 0 THEN -(profit_loss_jpy - fee_jpy + swap_jpy) ELSE 0 END), 0)::float8 AS loss_jpy
FROM trades
WHERE symbol = $1 AND closed_at >= $2;

-- name: CountClosedBySymbolSince :one
-- Per-symbol closed-trade count for MaxTradesInThisWindow per-symbol cap.
SELECT COUNT(*)::int8 AS count
FROM trades
WHERE symbol = $1 AND closed_at >= $2;

-- name: CountEarlyExitTradesSinceBySymbol :one
-- Dashboard: early-exit window 発火で close したトレード数。
-- close_reason='early_exit' を直接カウントする (ラベルは migration 0005 で追加)。
-- ラベル追加前の early-exit は 'max_hold' に混ざっているため遡及されない (= go-forward
-- 集計)。
SELECT COUNT(*)::int8 AS count
FROM trades t
WHERE t.symbol = $1
  AND t.closed_at >= $2
  AND t.close_reason = 'early_exit';
