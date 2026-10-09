-- name: InsertAdvisorRun :exec
INSERT INTO ai_advisor_runs (
  run_id, provider, mode, prompt_path,
  status, source, started_at, finished_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: InsertAdvisorRunIO :exec
INSERT INTO advisor_run_io (run_id, input_json, output_yaml)
VALUES ($1, $2, $3);

-- name: InsertAdvisorRunError :exec
INSERT INTO advisor_run_errors (run_id, message)
VALUES ($1, $2);

-- name: GetLastAdvisorDurationMs :one
-- Dashboard 用: 直近の成功 advisor run の duration_ms。
SELECT (EXTRACT(EPOCH FROM (finished_at - started_at)) * 1000)::int8 AS duration_ms
FROM ai_advisor_runs
WHERE status = 'success'
ORDER BY finished_at DESC
LIMIT 1;
