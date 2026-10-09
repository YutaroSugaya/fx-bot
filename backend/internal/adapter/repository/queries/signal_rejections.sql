-- name: InsertSignalRejection :exec
INSERT INTO signal_rejections (strategy_config_id, reason, detail)
VALUES ($1, $2, $3);
