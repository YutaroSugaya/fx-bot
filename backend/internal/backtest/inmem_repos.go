package backtest

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"fx-bot/backend/internal/port"
)

// InMemoryPositionRepo is a goroutine-safe port.PositionRepository
// implementation backed by a slice + mutex. It also keeps in-memory
// copies of the positions_live / manual_positions / recovered_positions /
// position_state_events junctions so usecase tests can verify Insert
// wrote the expected variant.
type InMemoryPositionRepo struct {
	mu          sync.Mutex
	nextID      int64
	rows        []port.PositionRecord
	live        map[int64]port.PositionLive
	manual      map[int64]bool
	recovered   map[int64]port.RecoveredPositionMeta
	stateEvents map[int64][]positionStateEvent
}

type positionStateEvent struct {
	state          port.PositionStatus
	transitionedAt time.Time
}

func NewInMemoryPositionRepo() *InMemoryPositionRepo {
	return &InMemoryPositionRepo{
		live:        map[int64]port.PositionLive{},
		manual:      map[int64]bool{},
		recovered:   map[int64]port.RecoveredPositionMeta{},
		stateEvents: map[int64][]positionStateEvent{},
	}
}

func (r *InMemoryPositionRepo) Insert(_ context.Context, in port.PositionInsertInput) (int64, error) {
	if err := validateInMemVariant(in); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	rec := in.Position
	rec.ID = r.nextID
	if rec.Status == "" {
		rec.Status = port.PositionStatusOpen
	}
	r.rows = append(r.rows, rec)
	if in.Live != nil {
		live := *in.Live
		live.PositionID = rec.ID
		r.live[rec.ID] = live
	}
	if in.Manual {
		r.manual[rec.ID] = true
	}
	if in.Recovered != nil {
		r.recovered[rec.ID] = *in.Recovered
	}
	r.stateEvents[rec.ID] = append(r.stateEvents[rec.ID], positionStateEvent{
		state:          rec.Status,
		transitionedAt: rec.OpenedAt,
	})
	return rec.ID, nil
}

func validateInMemVariant(in port.PositionInsertInput) error {
	if in.Manual && in.Recovered != nil {
		return errVariantConflict
	}
	return nil
}

var errVariantConflict = positionInsertVariantErr("position insert: Manual and Recovered are mutually exclusive")

type positionInsertVariantErr string

func (e positionInsertVariantErr) Error() string { return string(e) }

func (r *InMemoryPositionRepo) ListOpenOrClosing(_ context.Context, symbol string) ([]port.PositionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]port.PositionRecord, 0, 2)
	for _, rec := range r.rows {
		if rec.Symbol != symbol && symbol != "" {
			continue
		}
		if rec.Status != port.PositionStatusOpen && rec.Status != port.PositionStatusClosing {
			continue
		}
		// Mirror the SQL adapter: derive Source from the recovered_positions
		// junction so usecase tests see the same view they would in
		// production. A literal Source set by the test wins (mirrors the
		// "you can override the join" reality only in tests — fine).
		if rec.Source == "" {
			if meta, ok := r.recovered[rec.ID]; ok {
				rec.Source = port.RecoveryReasonToSource(meta.Reason)
			} else {
				rec.Source = port.PositionSourceBot
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

// CountOpenAllSymbols returns OPEN+CLOSING count across every symbol.
// External (manually-opened) positions are counted because the account-wide
// cap is a margin-protection gate; margin is a shared pool external
// positions also draw from.
func (r *InMemoryPositionRepo) CountOpenAllSymbols(_ context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.rows {
		if rec.Status == port.PositionStatusOpen || rec.Status == port.PositionStatusClosing {
			n++
		}
	}
	return n, nil
}

func (r *InMemoryPositionRepo) ClaimForClose(_ context.Context, id int64, claimedAt time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.rows {
		if r.rows[i].ID != id {
			continue
		}
		if r.rows[i].Status != port.PositionStatusOpen {
			return false, nil
		}
		r.rows[i].Status = port.PositionStatusClosing
		r.stateEvents[id] = append(r.stateEvents[id], positionStateEvent{
			state:          port.PositionStatusClosing,
			transitionedAt: claimedAt,
		})
		return true, nil
	}
	return false, nil
}

func (r *InMemoryPositionRepo) GetLive(_ context.Context, positionID int64) (*port.PositionLive, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if live, ok := r.live[positionID]; ok {
		copy := live
		return &copy, nil
	}
	return nil, nil
}

func (r *InMemoryPositionRepo) IsManual(_ context.Context, positionID int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.manual[positionID], nil
}

// MarkClosed mirrors PositionRepo.MarkClosed — paper-startup-only blind
// close. Flips status → CLOSED and appends a CLOSED state event WITHOUT
// writing a trade row. Silent no-op on unknown id (matches prod repo).
func (r *InMemoryPositionRepo) MarkClosed(_ context.Context, id int64, closedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.rows {
		if r.rows[i].ID != id {
			continue
		}
		if r.rows[i].Status != port.PositionStatusOpen && r.rows[i].Status != port.PositionStatusClosing {
			return nil
		}
		r.rows[i].Status = port.PositionStatusClosed
		r.stateEvents[id] = append(r.stateEvents[id], positionStateEvent{
			state:          port.PositionStatusClosed,
			transitionedAt: closedAt,
		})
		return nil
	}
	return nil
}

// UpdateRatchetState mirrors PositionRepo.UpdateRatchetState — updates
// 利確側 (peak/armed) + 損切り側 (trough/loss_armed) runtime state on OPEN
// positions only. Silent no-op for CLOSING / CLOSED / unknown ids (matches
// prod repo's `WHERE status='OPEN'`).
func (r *InMemoryPositionRepo) UpdateRatchetState(_ context.Context, id int64, peakUnrealizedPips float64, armed bool, troughUnrealizedPips float64, lossArmed bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.rows {
		if r.rows[i].ID != id {
			continue
		}
		if r.rows[i].Status != port.PositionStatusOpen {
			return nil
		}
		r.rows[i].PeakUnrealizedPips = peakUnrealizedPips
		r.rows[i].RatchetArmed = armed
		r.rows[i].TroughUnrealizedPips = troughUnrealizedPips
		r.rows[i].LossRatchetArmed = lossArmed
		return nil
	}
	return nil
}

// ExtendMaxHold mirrors PositionRepo.ExtendMaxHold — adds addMinutes to
// max_hold_minutes on OPEN positions only, returning the new total + opened_at.
// Returns (nil, nil) for CLOSING / CLOSED / unknown id (matches prod repo's
// `WHERE status='OPEN'` returning 0 rows).
func (r *InMemoryPositionRepo) ExtendMaxHold(_ context.Context, id int64, addMinutes int) (*port.MaxHoldExtended, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.rows {
		if r.rows[i].ID != id {
			continue
		}
		if r.rows[i].Status != port.PositionStatusOpen {
			return nil, nil
		}
		r.rows[i].MaxHoldMinutes += addMinutes
		return &port.MaxHoldExtended{
			MaxHoldMinutes: r.rows[i].MaxHoldMinutes,
			OpenedAt:       r.rows[i].OpenedAt,
		}, nil
	}
	return nil, nil
}

// InMemoryPositionCloser wires an in-memory PositionRepo + TradeRepo into
// a port.PositionCloser. Used by tests and the backtest engine so the
// the close saga (CLOSING → CLOSED + trade + trade_signals) works
// without DB.
type InMemoryPositionCloser struct {
	Positions *InMemoryPositionRepo
	Trades    *InMemoryTradeRepo
}

func NewInMemoryPositionCloser(p *InMemoryPositionRepo, t *InMemoryTradeRepo) *InMemoryPositionCloser {
	return &InMemoryPositionCloser{Positions: p, Trades: t}
}

func (c *InMemoryPositionCloser) CloseAndRecord(ctx context.Context, posID int64, closedAt time.Time, trade port.TradeRecord) (bool, error) {
	// Refuse to close one row and book the
	// trade against a different position. The trades FK resolves to ANY
	// real position so the corruption is silent without this guard.
	if trade.PositionID != posID {
		return false, fmt.Errorf("inmem closer: %w (closer=%d trade=%d)", port.ErrTradePositionIDMismatch, posID, trade.PositionID)
	}
	c.Positions.mu.Lock()
	found := false
	for i := range c.Positions.rows {
		if c.Positions.rows[i].ID != posID {
			continue
		}
		found = true
		if c.Positions.rows[i].Status != port.PositionStatusClosing {
			c.Positions.mu.Unlock()
			return false, nil
		}
		c.Positions.rows[i].Status = port.PositionStatusClosed
		c.Positions.stateEvents[posID] = append(c.Positions.stateEvents[posID], positionStateEvent{
			state:          port.PositionStatusClosed,
			transitionedAt: closedAt,
		})
		break
	}
	c.Positions.mu.Unlock()
	if !found {
		return false, nil
	}
	if err := c.Trades.Insert(ctx, trade); err != nil {
		return false, err
	}
	return true, nil
}

// InMemoryTradeRepo is a goroutine-safe port.TradeRepository.
// ListSince returns rows sorted most-recent-first (DESC by OpenedAt) to
// match the prod repo contract used by worker.accountSnapshot.
type InMemoryTradeRepo struct {
	mu   sync.Mutex
	rows []port.TradeRecord
}

func NewInMemoryTradeRepo() *InMemoryTradeRepo {
	return &InMemoryTradeRepo{}
}

func (r *InMemoryTradeRepo) Insert(_ context.Context, rec port.TradeRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, rec)
	return nil
}

func (r *InMemoryTradeRepo) ListSince(_ context.Context, since time.Time, limit int) ([]port.TradeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]port.TradeRecord, 0, len(r.rows))
	for _, rec := range r.rows {
		if !rec.OpenedAt.Before(since) {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].OpenedAt.After(out[j].OpenedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *InMemoryTradeRepo) SumLossJPYSince(_ context.Context, since time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var loss float64
	for _, rec := range r.rows {
		if rec.OpenedAt.Before(since) {
			continue
		}
		if rec.ProfitLossJPY < 0 {
			loss += -rec.ProfitLossJPY
		}
	}
	return int(loss), nil
}

func (r *InMemoryTradeRepo) CountSince(_ context.Context, since time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.rows {
		if !rec.OpenedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

// closed_at-based account-wide aggregates.
func (r *InMemoryTradeRepo) ListClosedSince(_ context.Context, since time.Time, limit int) ([]port.TradeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]port.TradeRecord, 0, len(r.rows))
	for _, rec := range r.rows {
		if rec.ClosedAt.IsZero() {
			continue
		}
		if !rec.ClosedAt.Before(since) {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ClosedAt.After(out[j].ClosedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *InMemoryTradeRepo) SumClosedLossJPYSince(_ context.Context, since time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var loss float64
	for _, rec := range r.rows {
		if rec.ClosedAt.IsZero() || rec.ClosedAt.Before(since) {
			continue
		}
		if rec.ProfitLossJPY < 0 {
			loss += -rec.ProfitLossJPY
		}
	}
	return int(loss), nil
}

func (r *InMemoryTradeRepo) CountClosedSince(_ context.Context, since time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.rows {
		if rec.ClosedAt.IsZero() {
			continue
		}
		if !rec.ClosedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

// Per-symbol closed_at aggregates so the risk gate's per-symbol caps
// do not get polluted by sibling-symbol trades.

func (r *InMemoryTradeRepo) ListClosedBySymbolSince(_ context.Context, symbol string, since time.Time, limit int) ([]port.TradeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]port.TradeRecord, 0, len(r.rows))
	for _, rec := range r.rows {
		if rec.Symbol != symbol || rec.ClosedAt.IsZero() || rec.ClosedAt.Before(since) {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ClosedAt.After(out[j].ClosedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *InMemoryTradeRepo) SumClosedLossJPYBySymbolSince(_ context.Context, symbol string, since time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var loss float64
	for _, rec := range r.rows {
		if rec.Symbol != symbol || rec.ClosedAt.IsZero() || rec.ClosedAt.Before(since) {
			continue
		}
		if rec.ProfitLossJPY < 0 {
			loss += -rec.ProfitLossJPY
		}
	}
	return int(loss), nil
}

func (r *InMemoryTradeRepo) CountClosedBySymbolSince(_ context.Context, symbol string, since time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.rows {
		if rec.Symbol != symbol || rec.ClosedAt.IsZero() || rec.ClosedAt.Before(since) {
			continue
		}
		n++
	}
	return n, nil
}

// Dashboard 用。In-memory impl は positions テーブルと結合できないので、
// 早期 exit カウントは「close_reason=max_hold + ClosedAt - OpenedAt < MaxHoldMinutes」で代用。
// PnL は signed sum。

func (r *InMemoryTradeRepo) SumPnLJPYClosedSinceBySymbol(_ context.Context, symbol string, since time.Time) (float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sum float64
	for _, rec := range r.rows {
		if rec.Symbol != symbol || rec.ClosedAt.IsZero() || rec.ClosedAt.Before(since) {
			continue
		}
		sum += rec.ProfitLossJPY
	}
	return sum, nil
}

func (r *InMemoryTradeRepo) CountEarlyExitTradesSinceBySymbol(_ context.Context, symbol string, since time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.rows {
		if rec.Symbol != symbol || rec.ClosedAt.IsZero() || rec.ClosedAt.Before(since) {
			continue
		}
		if rec.CloseReason != "max_hold" {
			continue
		}
		// In-memory layer doesn't know about positions.early_exit_window_minutes /
		// max_hold_minutes; approximate by treating any max_hold close as a
		// potential early-exit candidate. Production repo (TradeRepo) joins
		// the positions table for the proper filter.
		n++
	}
	return n, nil
}

// InMemoryCandleRepo implements port.CandleRepository.
type InMemoryCandleRepo struct {
	mu   sync.Mutex
	rows []port.CandleRecord
}

func NewInMemoryCandleRepo() *InMemoryCandleRepo {
	return &InMemoryCandleRepo{}
}

func (r *InMemoryCandleRepo) Upsert(_ context.Context, rec port.CandleRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.rows {
		if r.rows[i].Symbol == rec.Symbol &&
			r.rows[i].Timeframe == rec.Timeframe &&
			r.rows[i].OpenedAt.Equal(rec.OpenedAt) {
			if rec.ID == 0 {
				rec.ID = r.rows[i].ID
			}
			if rec.CreatedAt.IsZero() {
				rec.CreatedAt = r.rows[i].CreatedAt
			}
			r.rows[i] = rec
			return nil
		}
	}
	r.rows = append(r.rows, rec)
	return nil
}

func (r *InMemoryCandleRepo) UpsertBatch(ctx context.Context, recs []port.CandleRecord) error {
	for _, rec := range recs {
		if err := r.Upsert(ctx, rec); err != nil {
			return err
		}
	}
	return nil
}

func (r *InMemoryCandleRepo) ListSince(_ context.Context, symbol, timeframe string, since time.Time, limit int) ([]port.CandleRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]port.CandleRecord, 0, len(r.rows))
	for _, rec := range r.rows {
		if rec.Symbol == symbol && rec.Timeframe == timeframe && !rec.OpenedAt.Before(since) {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].OpenedAt.After(out[j].OpenedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
