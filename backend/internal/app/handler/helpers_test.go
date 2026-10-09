package handler

import (
	"context"
	"time"

	"fx-bot/backend/internal/port"
)

// errPositions は InMemoryPositionRepo を内包し、ListOpenOrClosing に err
// 注入できる薄い wrapper。古典派ルール §4 「失敗注入は許容」に従い、DB
// ダウン系のテストで使う。Insert / MarkClosed は埋め込み側にそのまま委譲。
type errPositions struct {
	port.PositionRepository
	listOpenErr error
}

func (r *errPositions) ListOpenOrClosing(ctx context.Context, sym string) ([]port.PositionRecord, error) {
	if r.listOpenErr != nil {
		return nil, r.listOpenErr
	}
	return r.PositionRepository.ListOpenOrClosing(ctx, sym)
}

// errTrades は InMemoryTradeRepo を内包し、ListSince / CountSince / SumLossJPYSince
// にエラーを注入できる薄い wrapper。
type errTrades struct {
	port.TradeRepository
	listSinceErr error
}

func (r *errTrades) ListSince(ctx context.Context, since time.Time, limit int) ([]port.TradeRecord, error) {
	if r.listSinceErr != nil {
		return nil, r.listSinceErr
	}
	return r.TradeRepository.ListSince(ctx, since, limit)
}
