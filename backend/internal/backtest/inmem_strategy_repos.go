package backtest

import (
	"context"
	"sync"
	"time"

	"fx-bot/backend/internal/port"
)

// InMemoryStrategyConfigRepo is the test/backtest in-memory implementation of
// port.StrategyConfigRepository. It also exposes a couple of test-only
// observability accessors (ExpiredIDs, ActivatedAt) so usecase tests can
// assert side-effects without reaching into private fields of a hand-rolled
// fake. Replaces former `fakeStrategyRepo` / `fakeStrategyConfigsRepo` /
// `stubActiveConfigRepo` in test files.
type InMemoryStrategyConfigRepo struct {
	mu          sync.Mutex
	rows        []port.StrategyConfigRecord
	expired     []string
	activatedAt map[string]time.Time
	insertedAt  map[string]time.Time
}

func NewInMemoryStrategyConfigRepo() *InMemoryStrategyConfigRepo {
	return &InMemoryStrategyConfigRepo{
		activatedAt: map[string]time.Time{},
		insertedAt:  map[string]time.Time{},
	}
}

func (r *InMemoryStrategyConfigRepo) Insert(_ context.Context, rec port.StrategyConfigRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, rec)
	r.insertedAt[rec.ConfigID] = time.Now()
	return nil
}

func (r *InMemoryStrategyConfigRepo) MarkExpired(_ context.Context, configID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expired = append(r.expired, configID)
	for i := range r.rows {
		if r.rows[i].ConfigID == configID && r.rows[i].Status == port.StrategyConfigStatusActive {
			r.rows[i].Status = port.StrategyConfigStatusExpired
		}
	}
	return nil
}

func (r *InMemoryStrategyConfigRepo) MarkActive(_ context.Context, configID string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activatedAt[configID] = at
	for i := range r.rows {
		if r.rows[i].ConfigID == configID {
			r.rows[i].Status = port.StrategyConfigStatusActive
		}
	}
	return nil
}

// GetActive returns the most-recently-inserted active row matching (symbol,
// mode). Insertion order is preserved by appending; rows are scanned in
// reverse so the latest active wins (matches prod repo's ORDER BY created_at
// DESC LIMIT 1).
func (r *InMemoryStrategyConfigRepo) GetActive(_ context.Context, symbol, mode string) (*port.StrategyConfigRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.rows) - 1; i >= 0; i-- {
		rec := r.rows[i]
		if rec.Status != port.StrategyConfigStatusActive {
			continue
		}
		if symbol != "" && rec.Symbol != symbol {
			continue
		}
		if mode != "" && rec.Mode != mode {
			continue
		}
		out := rec
		return &out, nil
	}
	return nil, nil
}

func (r *InMemoryStrategyConfigRepo) ListRecent(_ context.Context, limit int) ([]port.StrategyConfigRecordWithMeta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]port.StrategyConfigRecordWithMeta, 0, len(r.rows))
	// DESC by insertion order (= prod repo's ORDER BY created_at DESC).
	for i := len(r.rows) - 1; i >= 0; i-- {
		rec := r.rows[i]
		meta := port.StrategyConfigRecordWithMeta{StrategyConfigRecord: rec}
		if ts, ok := r.activatedAt[rec.ConfigID]; ok {
			t := ts
			meta.ActivatedAt = &t
		}
		if ts, ok := r.insertedAt[rec.ConfigID]; ok {
			meta.CreatedAt = ts
		}
		out = append(out, meta)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SeedActive is a test helper that bypasses Insert: it appends a record with
// Status=active so callers don't have to spell out all fields. Returns the
// repo for fluent setup.
func (r *InMemoryStrategyConfigRepo) SeedActive(configID, symbol, mode string) *InMemoryStrategyConfigRepo {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, port.StrategyConfigRecord{
		ConfigID: configID, Symbol: symbol, Mode: mode,
		Status: port.StrategyConfigStatusActive,
	})
	r.insertedAt[configID] = time.Now()
	return r
}

// ExpiredIDs returns the list of config_ids passed to MarkExpired, in call
// order. Test-only observability.
func (r *InMemoryStrategyConfigRepo) ExpiredIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.expired...)
}

// ActivatedAt returns the timestamp recorded by MarkActive for the given
// config_id. Test-only observability.
func (r *InMemoryStrategyConfigRepo) ActivatedAt(configID string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ts, ok := r.activatedAt[configID]
	return ts, ok
}

// Rows returns a snapshot of all inserted records in insertion order.
// Test-only observability — replaces direct `.rows` access on the former fake.
func (r *InMemoryStrategyConfigRepo) Rows() []port.StrategyConfigRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]port.StrategyConfigRecord{}, r.rows...)
}

// InMemoryValidationEventRepo is the in-memory port.ConfigValidationEventRepository
// for tests. Replaces former `fakeValidationRepo`.
type InMemoryValidationEventRepo struct {
	mu     sync.Mutex
	events []port.ConfigValidationEvent
}

func NewInMemoryValidationEventRepo() *InMemoryValidationEventRepo {
	return &InMemoryValidationEventRepo{}
}

func (r *InMemoryValidationEventRepo) Insert(_ context.Context, ev port.ConfigValidationEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

// CountFailSince returns the total fail-status events. The in-memory DTO
// has no created_at field so the `since` arg is ignored — production repo
// filters by created_at properly. Used by the dashboard.
func (r *InMemoryValidationEventRepo) CountFailSince(_ context.Context, _ time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.events {
		if ev.Status == "fail" {
			n++
		}
	}
	return n, nil
}

// ByType returns events matching (validation_type, status). Test-only
// observability accessor.
func (r *InMemoryValidationEventRepo) ByType(validationType, status string) []port.ConfigValidationEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []port.ConfigValidationEvent{}
	for _, e := range r.events {
		if e.ValidationType == validationType && e.Status == status {
			out = append(out, e)
		}
	}
	return out
}

// All returns a snapshot of all events. Test-only observability.
func (r *InMemoryValidationEventRepo) All() []port.ConfigValidationEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]port.ConfigValidationEvent{}, r.events...)
}
