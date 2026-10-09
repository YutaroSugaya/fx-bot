package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/clock"
	"fx-bot/backend/internal/port"
)

// ClosePositionInput は ClosePositionCommand.Execute の引数。
type ClosePositionInput struct {
	PositionID int64
}

// ClosePositionOutput は ClosePositionCommand.Execute の戻り値。
type ClosePositionOutput struct {
	PositionID     int64
	Side           string
	EntryPrice     float64
	ExitPrice      float64
	ProfitLossPips float64
	ProfitLossJPY  float64
}

// ErrPositionNotFound は指定 ID の OPEN ポジションが存在しないときに返される。
var ErrPositionNotFound = errors.New("position not found or already closed")

// ClosePositionCommand は API 経由 (= 手動) のポジション強制決済 usecase。
//
// Delegates the full close sequence to
// ExecuteCloseSaga so manual close and scheduled close share the same
// safety envelope (claim → cancel by recorded leg id → close → resolve →
// record).
type ClosePositionCommand struct {
	Mode              config.Mode
	Symbol            string
	PipSize           float64
	Broker            port.Broker
	Positions         port.PositionRepository
	Closer            port.PositionCloser
	Mutex             *sync.Mutex // priceLoop と直列化するための共有 mutex
	EmergencyFlagPath string
	Logger            *slog.Logger

	// Clock は close 時刻 (saga に渡す `now`) のソース。
	// nil の場合は time.Now() に fallback (既存呼び出し互換)。
	// Clock 注入の足場 (domain/clock 参照)。
	Clock clock.Clock

	// OnClosed は決済確定後に 1 回呼ばれるフック (CloseSagaInput.OnClosed へ貫通)。
	// nil = no-op。event_retrigger 用。
	OnClosed func(symbol, reason string)
}

// now は Clock が設定されていればそれを、なければ time.Now() を返す。
// 既存の Clock 未設定の callsite を壊さないための互換ヘルパ。
func (c *ClosePositionCommand) now() time.Time {
	if c.Clock != nil {
		return c.Clock.Now()
	}
	return time.Now()
}

// Execute は手動決済の全フローを 1 関数で実行する。
func (c *ClosePositionCommand) Execute(ctx context.Context, in ClosePositionInput) (ClosePositionOutput, error) {
	if c.Mutex != nil {
		c.Mutex.Lock()
		defer c.Mutex.Unlock()
	}

	open, err := c.Positions.ListOpenOrClosing(ctx, c.Symbol)
	if err != nil {
		return ClosePositionOutput{}, fmt.Errorf("list open: %w", err)
	}
	// Accepts CLOSING in addition to OPEN. CLOSING rows are
	// usually the residue of a crashed saga (e.g. previous force-close
	// failed after ClaimForClose succeeded but before broker-close). The
	// saga treats CLOSING as "resume", skipping the redundant claim.
	var rec *port.PositionRecord
	for i := range open {
		if open[i].ID == in.PositionID &&
			(open[i].Status == port.PositionStatusOpen ||
				open[i].Status == port.PositionStatusClosing) {
			rec = &open[i]
			break
		}
	}
	if rec == nil {
		return ClosePositionOutput{}, ErrPositionNotFound
	}

	now := c.now()
	res, err := ExecuteCloseSaga(ctx, CloseSagaInput{
		Mode:              c.Mode,
		Symbol:            c.Symbol,
		Broker:            c.Broker,
		Positions:         c.Positions,
		Closer:            c.Closer,
		EmergencyFlagPath: c.EmergencyFlagPath,
		Logger:            c.Logger,
		OnClosed:          c.OnClosed,
	}, *rec, "manual", 0, now)
	if err != nil {
		// Manual close racing with scheduled close: silently report
		// "already closed" rather than emergency_stop.
		if errors.Is(err, ErrPositionAlreadyClosing) {
			return ClosePositionOutput{}, ErrPositionNotFound
		}
		return ClosePositionOutput{}, err
	}

	// PnL is computed inside the saga (quote→JPY rate resolved once, up front).
	return ClosePositionOutput{
		PositionID:     rec.ID,
		Side:           rec.Side,
		EntryPrice:     rec.EntryPrice,
		ExitPrice:      res.ExitPrice,
		ProfitLossPips: res.ProfitLossPips,
		ProfitLossJPY:  res.ProfitLossJPY,
	}, nil
}
