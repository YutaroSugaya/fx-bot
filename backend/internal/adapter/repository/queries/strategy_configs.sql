-- name: InsertStrategyConfig :exec
-- market_regime_type / strategy_name accept *string (nil = NULL) so the
-- wrapper can map "" to NULL without sqlc losing the column type.
INSERT INTO strategy_configs (
  config_id, source, mode, symbol, enabled,
  market_regime_type, market_regime_confidence, strategy_name,
  valid_from, valid_until, raw_yaml, status
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- name: MarkStrategyConfigExpired :exec
UPDATE strategy_configs
SET status = 'expired'
WHERE config_id = $1 AND status = 'active';

-- name: MarkStrategyConfigActive :exec
UPDATE strategy_configs SET status = 'active' WHERE config_id = $1;

-- name: ExpireOtherActiveConfigs :exec
UPDATE strategy_configs
SET status = 'expired'
WHERE symbol = $1 AND mode = $2 AND status = 'active' AND config_id <> $3;

-- name: GetActiveStrategyConfig :one
SELECT sc.config_id, sc.source, sc.mode, sc.symbol, sc.enabled,
       COALESCE(sc.market_regime_type,'')::text AS market_regime_type,
       sc.market_regime_confidence,
       COALESCE(sc.strategy_name,'')::text AS strategy_name,
       sc.valid_from, sc.valid_until, sc.raw_yaml,
       sc.status, COALESCE(sr.reason,'')::text AS reject_reason
FROM strategy_configs sc
LEFT JOIN strategy_config_rejections   sr ON sr.config_id = sc.config_id
LEFT JOIN strategy_config_activations  sa ON sa.config_id = sc.config_id
WHERE sc.status = 'active' AND sc.symbol = $1 AND sc.mode = $2
ORDER BY sa.activated_at DESC NULLS LAST, sc.id DESC
LIMIT 1;

-- name: ListRecentStrategyConfigs :many
SELECT sc.config_id, sc.source, sc.mode, sc.symbol, sc.enabled,
       COALESCE(sc.market_regime_type,'')::text AS market_regime_type,
       sc.market_regime_confidence,
       COALESCE(sc.strategy_name,'')::text AS strategy_name,
       sc.valid_from, sc.valid_until, sc.raw_yaml,
       sc.status, COALESCE(sr.reason,'')::text AS reject_reason,
       sc.created_at, sa.activated_at
FROM strategy_configs sc
LEFT JOIN strategy_config_rejections   sr ON sr.config_id = sc.config_id
LEFT JOIN strategy_config_activations  sa ON sa.config_id = sc.config_id
ORDER BY sc.created_at DESC
LIMIT $1;

-- name: UpsertStrategyConfigRejection :exec
INSERT INTO strategy_config_rejections (config_id, reason)
VALUES ($1, $2)
ON CONFLICT (config_id) DO UPDATE SET reason = EXCLUDED.reason;

-- name: UpsertStrategyConfigActivation :exec
INSERT INTO strategy_config_activations (config_id, activated_at)
VALUES ($1, $2)
ON CONFLICT (config_id) DO UPDATE SET activated_at = EXCLUDED.activated_at;

-- name: UpsertStrategyConfigParseFailure :exec
INSERT INTO strategy_config_parse_failures (config_id, raw_input)
VALUES ($1, $2)
ON CONFLICT (config_id) DO UPDATE SET raw_input = EXCLUDED.raw_input;
