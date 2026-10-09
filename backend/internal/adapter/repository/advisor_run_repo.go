package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/repository/dbgen"
	"fx-bot/backend/internal/port"
)

// AdvisorRunRepo persists the main ai_advisor_runs row + (in the same Tx)
// the matching junction row:
//
//   - status='success'                              → advisor_run_io
//   - status in (timeout, parse_error, cli_error)   → advisor_run_errors
//
// This split keeps the main row small (no JSONB in the hot path) while
// preserving the audit trail for failures and the full IO for successes.
type AdvisorRunRepo struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

func NewAdvisorRunRepo(pool *pgxpool.Pool) *AdvisorRunRepo {
	return &AdvisorRunRepo{pool: pool, q: dbgen.New(pool)}
}

func (r *AdvisorRunRepo) Insert(ctx context.Context, rec port.AdvisorRunRecord) error {
	source := string(rec.Source)
	if source == "" {
		source = string(port.AdvisorRunSourceAuto)
	}
	return withTx(ctx, r.pool, r.q, func(q *dbgen.Queries) error {
		if err := q.InsertAdvisorRun(ctx, dbgen.InsertAdvisorRunParams{
			RunID:      rec.RunID,
			Provider:   rec.Provider,
			Mode:       rec.Mode,
			PromptPath: rec.PromptPath,
			Status:     string(rec.Status),
			Source:     source,
			StartedAt:  pgts(rec.StartedAt),
			FinishedAt: pgts(rec.FinishedAt),
		}); err != nil {
			return fmt.Errorf("ai_advisor_runs insert: %w", err)
		}

		if rec.Status == port.AdvisorRunStatusSuccess {
			// Success path: always have input + output. Fall back to empty
			// JSON / string if the caller didn't populate them.
			input := rec.InputJSON
			if len(input) == 0 {
				input = []byte("{}")
			}
			output := string(rec.OutputYAML)
			if err := q.InsertAdvisorRunIO(ctx, dbgen.InsertAdvisorRunIOParams{
				RunID:      rec.RunID,
				InputJson:  input,
				OutputYaml: output,
			}); err != nil {
				return fmt.Errorf("advisor_run_io insert: %w", err)
			}
			return nil
		}

		msg := rec.ErrorMessage
		if msg == "" {
			msg = string(rec.Status)
		}
		if err := q.InsertAdvisorRunError(ctx, dbgen.InsertAdvisorRunErrorParams{
			RunID:   rec.RunID,
			Message: msg,
		}); err != nil {
			return fmt.Errorf("advisor_run_errors insert: %w", err)
		}
		return nil
	})
}

// GetLastDurationMs は直近の成功 advisor run の duration_ms (= finished_at - started_at)
// を返す (Dashboard 用)。1 件も無い場合は (0, pgx.ErrNoRows)。
func (r *AdvisorRunRepo) GetLastDurationMs(ctx context.Context) (int, error) {
	v, err := r.q.GetLastAdvisorDurationMs(ctx)
	if err != nil {
		return 0, fmt.Errorf("advisor_runs get last duration: %w", err)
	}
	return int(v), nil
}
