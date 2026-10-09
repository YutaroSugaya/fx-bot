package command

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"fx-bot/backend/internal/domain/clock"
	"fx-bot/backend/internal/port"
)

// 延長幅の許容レンジ。1 回の操作で動かせる上限を MaxExtendMinutes (12h) に
// 抑え、fat-finger な一発巨大延長を防ぐ。さらに伸ばしたい場合は再度叩く。
const (
	MinExtendMinutes = 1
	MaxExtendMinutes = 720
)

// ErrInvalidExtendMinutes は add_minutes が [MinExtendMinutes, MaxExtendMinutes]
// の外のときに返る。
var ErrInvalidExtendMinutes = fmt.Errorf("add_minutes must be between %d and %d", MinExtendMinutes, MaxExtendMinutes)

// ExtendMaxHoldInput は ExtendMaxHoldCommand.Execute の引数。
type ExtendMaxHoldInput struct {
	PositionID int64
	AddMinutes int
}

// ExtendMaxHoldOutput は延長後の状態。フロントが新しい締切を表示するのに使う。
type ExtendMaxHoldOutput struct {
	PositionID       int64
	MaxHoldMinutes   int // 加算後の合計
	AddedMinutes     int
	DeadlineAt       time.Time // OpenedAt + MaxHoldMinutes
	RemainingMinutes float64   // DeadlineAt - now
}

// ExtendMaxHoldCommand は開いている position の保有上限 (max_hold_minutes) を
// 延長する usecase。UI/API の「延長ボタン」(POST /api/positions/extend) から
// 呼ばれる。
//
// 設計メモ:
//   - close と違い broker/pip/mutex を持たない。純粋に positions 行の
//     max_hold_minutes を加算するだけで、TP/SL (GMO 側 OCO) には触れない。
//     OnTick の max_hold 判定と extend の UPDATE は両方とも status='OPEN' を
//     ゲートにするので、締切ギリギリで close saga が CLOSING に倒した直後の
//     extend は 0 行ヒット (= 対象なし) になり race しない。
//   - max_hold_minutes は per-position の凍結値なので config 変更では動かせ
//     ない (open 玉の保護: 建玉時の値を凍結保存)。これが唯一の延長経路。
type ExtendMaxHoldCommand struct {
	Positions port.PositionRepository
	Clock     clock.Clock
	Logger    *slog.Logger
}

func (c *ExtendMaxHoldCommand) now() time.Time {
	if c.Clock != nil {
		return c.Clock.Now()
	}
	return time.Now()
}

// Execute は add_minutes を検証し、OPEN position の max_hold_minutes を加算して
// 新しい deadline / remaining を返す。対象が無ければ ErrPositionNotFound。
func (c *ExtendMaxHoldCommand) Execute(ctx context.Context, in ExtendMaxHoldInput) (ExtendMaxHoldOutput, error) {
	if in.AddMinutes < MinExtendMinutes || in.AddMinutes > MaxExtendMinutes {
		return ExtendMaxHoldOutput{}, ErrInvalidExtendMinutes
	}

	ext, err := c.Positions.ExtendMaxHold(ctx, in.PositionID, in.AddMinutes)
	if err != nil {
		return ExtendMaxHoldOutput{}, fmt.Errorf("extend max hold: %w", err)
	}
	if ext == nil {
		return ExtendMaxHoldOutput{}, ErrPositionNotFound
	}

	deadline := ext.OpenedAt.Add(time.Duration(ext.MaxHoldMinutes) * time.Minute)
	out := ExtendMaxHoldOutput{
		PositionID:       in.PositionID,
		MaxHoldMinutes:   ext.MaxHoldMinutes,
		AddedMinutes:     in.AddMinutes,
		DeadlineAt:       deadline,
		RemainingMinutes: deadline.Sub(c.now()).Minutes(),
	}
	if c.Logger != nil {
		c.Logger.Info("extend_maxhold",
			"position_id", out.PositionID,
			"added_minutes", out.AddedMinutes,
			"max_hold_minutes", out.MaxHoldMinutes,
			"deadline", out.DeadlineAt)
	}
	return out, nil
}
