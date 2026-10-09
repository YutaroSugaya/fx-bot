-- name: UpsertCandle :exec
INSERT INTO candles (symbol, timeframe, opened_at, open, high, low, close, volume)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (symbol, timeframe, opened_at) DO UPDATE SET
    open       = EXCLUDED.open,
    high       = EXCLUDED.high,
    low        = EXCLUDED.low,
    close      = EXCLUDED.close,
    volume     = EXCLUDED.volume,
    updated_at = now();

-- name: ListCandlesSince :many
SELECT id, symbol, timeframe, opened_at, open, high, low, close, volume, created_at, updated_at
FROM candles
WHERE symbol = $1 AND timeframe = $2 AND opened_at >= $3
ORDER BY opened_at DESC
LIMIT $4;
