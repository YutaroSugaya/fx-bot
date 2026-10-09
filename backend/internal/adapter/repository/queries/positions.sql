-- name: InsertPosition :one
-- ratchet_arm_pips / ratchet_giveback_pips は snapshot for trailing
-- take-profit (migration 0003). peak_unrealized_pips / ratchet_armed は
-- runtime state なので INSERT 時は DEFAULT (0/false) のまま。OnTick で
-- UpdatePositionRatchetState が更新する。
-- entry_fee_jpy / entry_spread_pips / entry_slippage_pips は entry 時点の
-- 実コスト捕捉 (migration 0008)。NULL = 未捕捉 (0 と区別)。
INSERT INTO positions (
  symbol, side, quantity, entry_price,
  take_profit_pips, stop_loss_pips, max_hold_minutes,
  extension_max_minutes, extension_unrealized_pips_threshold,
  early_exit_window_minutes, early_exit_target_pips,
  ratchet_arm_pips, ratchet_giveback_pips,
  entry_fee_jpy, entry_spread_pips, entry_slippage_pips,
  strategy_config_id, status, opened_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
RETURNING id;

-- name: InsertPositionLive :exec
INSERT INTO positions_live (position_id, broker_position_id, tp_order_id, sl_order_id)
VALUES ($1, $2, $3, $4);

-- name: InsertManualPosition :exec
INSERT INTO manual_positions (position_id) VALUES ($1);

-- name: InsertRecoveredPosition :exec
INSERT INTO recovered_positions (position_id, recovery_reason, recovered_at)
VALUES ($1, $2, $3);

-- name: InsertPositionStateEvent :exec
INSERT INTO position_state_events (position_id, state, transitioned_at)
VALUES ($1, $2, $3);

-- name: ClaimPositionForClose :execrows
UPDATE positions
SET status = 'CLOSING', updated_at = now()
WHERE id = $1 AND status = 'OPEN';

-- name: MarkPositionClosedBlind :execrows
-- Paper-startup-only blind close path. Lossy: no trade row is written.
-- Live MUST go through MarkPositionClosed (CLOSING → CLOSED) inside the
-- PositionCloser Tx so the trade ledger stays complete.
UPDATE positions
SET status = 'CLOSED', updated_at = now()
WHERE id = $1 AND status IN ('OPEN','CLOSING');

-- name: MarkPositionClosed :execrows
UPDATE positions
SET status = 'CLOSED', updated_at = now()
WHERE id = $1 AND status = 'CLOSING';

-- name: ListOpenOrClosingPositions :many
-- LEFT JOIN recovered_positions so the row carries the recovery_reason
-- (empty string when the row has no recovered_positions junction). The
-- repo wrapper maps reason → port.PositionSource so usecase code never
-- branches on the raw string.
SELECT p.id, p.symbol, p.side, p.quantity, p.entry_price,
       p.take_profit_pips, p.stop_loss_pips, p.max_hold_minutes,
       p.extension_max_minutes, p.extension_unrealized_pips_threshold,
       p.early_exit_window_minutes, p.early_exit_target_pips,
       p.ratchet_arm_pips, p.ratchet_giveback_pips,
       p.peak_unrealized_pips, p.ratchet_armed,
       p.trough_unrealized_pips, p.loss_ratchet_armed,
       p.entry_fee_jpy, p.entry_spread_pips, p.entry_slippage_pips,
       p.strategy_config_id, p.status, p.opened_at,
       COALESCE(r.recovery_reason, '')::text AS recovery_reason
FROM positions p
LEFT JOIN recovered_positions r ON r.position_id = p.id
WHERE p.status IN ('OPEN','CLOSING') AND ($1::text = '' OR p.symbol = $1)
ORDER BY p.opened_at ASC;

-- name: UpdatePositionRatchetState :execrows
-- OnTick で利確側 (peak/armed) と損切り側 (trough/loss_armed) の ratchet
-- runtime state を 1 write で更新する。armed/loss_armed は monotonic
-- (false→true)、peak は monotonic increasing・trough は monotonic decreasing
-- で usecase 側が保証する。status='OPEN' の row のみ更新 (CLOSING 中の
-- position は close saga が値を確定させているので state update は無視)。
UPDATE positions
SET peak_unrealized_pips = $2,
    ratchet_armed = $3,
    trough_unrealized_pips = $4,
    loss_ratchet_armed = $5,
    updated_at = now()
WHERE id = $1 AND status = 'OPEN';

-- name: ExtendPositionMaxHold :one
-- 延長ボタン (UI: POST /api/positions/extend) から呼ぶ。max_hold_minutes に
-- add_minutes を加算し、新しい合計 + opened_at を返す。status='OPEN' の row
-- のみ対象。CLOSING/CLOSED/未知 id は 0 rows → :one が pgx.ErrNoRows を返し、
-- wrapper が nil で「対象なし」を表す。bot の OnTick は毎 tick で
-- max_hold_minutes を読み直すので、UPDATE 後の次 tick から新 deadline が効く
-- (build/close 不要)。max_hold_minutes は per-position の凍結値なので config
-- 変更では動かせない (建玉時に凍結保存した値を使う設計)。
UPDATE positions
SET max_hold_minutes = max_hold_minutes + sqlc.arg(add_minutes),
    updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'OPEN'
RETURNING max_hold_minutes, opened_at;

-- name: CountOpenPositionsAllSymbols :one
-- Account-wide AccountOpenPositions cap reads this. Counts every
-- OPEN+CLOSING position across all symbols, including external (manually-
-- opened in the GMO app) positions — the account cap is a margin-protection
-- gate so external positions consume the same margin pool.
SELECT COUNT(*)::int8 AS count
FROM positions
WHERE status IN ('OPEN','CLOSING');

-- name: GetPositionLive :one
SELECT position_id, broker_position_id, tp_order_id, sl_order_id
FROM positions_live
WHERE position_id = $1;

-- name: IsManualPosition :one
SELECT EXISTS (SELECT 1 FROM manual_positions WHERE position_id = $1) AS is_manual;
