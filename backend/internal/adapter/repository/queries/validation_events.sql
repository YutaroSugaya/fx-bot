-- name: InsertConfigValidationEvent :exec
-- message is nullable; pass *string (nil = NULL) from the wrapper.
INSERT INTO config_validation_events (config_id, validation_type, status, message)
VALUES ($1, $2, $3, $4);

-- name: CountValidationFailSince :one
-- Dashboard 用: 直近 24h の reject 件数。
SELECT COUNT(*)::int8 AS count
FROM config_validation_events
WHERE status = 'fail' AND created_at >= $1;
