-- name: InsertMarketSummary :exec
INSERT INTO market_summaries (symbol, summary_window, raw_json)
VALUES ($1, $2, $3);
