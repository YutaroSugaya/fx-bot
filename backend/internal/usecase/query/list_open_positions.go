package query

import (
	"context"
	"fmt"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

// resolvePip returns the pip size to use for a given position. q.PipSize
// stays as the back-compat single-symbol override; when zero (multi-symbol
// wiring), pip is derived from the position's own symbol via market.PipSize.
func (q *ListOpenPositionsQuery) resolvePip(sym string) float64 {
	if q.PipSize > 0 {
		return q.PipSize
	}
	return market.PipSize(sym)
}

// GetTickerFn は GET の際に現在価格を取得する関数。symbol-aware: 各 row の
// symbol を渡して、その symbol の現在価格を返す。nil なら未実現損益は 0 で
// 返す (entry_price と TP/SL absolute 価格は計算可能)。Execute は 1 回の
// 呼び出し内で symbol ごとに 1 度だけ評価する (= 同 symbol N rows でも
// 呼び出し回数は 1 — GMO rate limit に対する基本配慮)。
type GetTickerFn func(ctx context.Context, symbol string) (*market.Ticker, error)

// ListOpenPositionsInput は ListOpenPositionsQuery.Execute の引数。
// Symbol が空のときは Query.Symbol (legacy single-symbol wiring) → 空 ("")
// にフォールバックする。空 "" は PositionRepository.ListOpenOrClosing が
// 「全 symbol」として扱う契約。
type ListOpenPositionsInput struct {
	Symbol string
}

// OpenPositionView は GET /api/positions の各行用 DTO。
//
// 未実現損益と TP/SL absolute 価格、デッドラインまでの残り時間など
// derived field を含む。Query 層で計算することで handler は薄くなる。
//
// `source` discriminates bot-owned vs externally-opened rows
// ("bot" | "external_broker" | "paper_recovered"). The dashboard uses
// this to badge external rows so the operator can see at a glance which
// positions the bot is managing vs which the user opened in the GMO app.
type OpenPositionView struct {
	ID               int64     `json:"id"`
	Symbol           string    `json:"symbol"`
	Side             string    `json:"side"`
	Quantity         int       `json:"quantity"`
	EntryPrice       float64   `json:"entry_price"`
	CurrentPrice     float64   `json:"current_price"`
	UnrealizedPips   float64   `json:"unrealized_pips"`
	UnrealizedJPY    float64   `json:"unrealized_jpy"`
	TPPrice          float64   `json:"tp_price"`
	SLPrice          float64   `json:"sl_price"`
	OpenedAt         time.Time `json:"opened_at"`
	ElapsedMinutes   float64   `json:"elapsed_minutes"`
	MaxHoldMinutes   int       `json:"max_hold_minutes"`
	DeadlineAt       time.Time `json:"deadline_at"`
	RemainingMinutes float64   `json:"remaining_minutes"`
	StrategyConfigID string    `json:"strategy_config_id"`
	IsManual         bool      `json:"is_manual"`
	Source           string    `json:"source"`
}

// ListOpenPositionsQuery is a read-only CQRS Query.
//
// GET /api/positions を裏で支える。port.PositionRepository から OPEN
// ポジションを引き、現在価格 (ticker) と組み合わせて未実現損益・TP/SL
// 絶対価格・経過/残り時間を計算する。
//
// 状態変更しない (Tx 不要)。
type ListOpenPositionsQuery struct {
	Symbol    string
	PipSize   float64
	Positions port.PositionRepository
	GetTicker GetTickerFn // nil 可
	Clock     func() time.Time
}

// Execute returns one OpenPositionView per OPEN row in the configured symbol.
//
// Filters to status=OPEN only. CLOSING rows are mid-saga or
// stuck-from-crashed-saga state that the operator should not see in the
// position list — reconcile resolves them (real fill / estimated close;
// synthetic only on paper startup) in the background. Showing CLOSING in the
// dashboard list was misleading because the row would still display live
// PnL as if it were an active position.
func (q *ListOpenPositionsQuery) Execute(ctx context.Context, in ListOpenPositionsInput) ([]OpenPositionView, error) {
	sym := in.Symbol
	if sym == "" {
		sym = q.Symbol // legacy single-symbol wiring; "" → all symbols
	}
	all, err := q.Positions.ListOpenOrClosing(ctx, sym)
	if err != nil {
		return nil, fmt.Errorf("list open: %w", err)
	}
	open := make([]port.PositionRecord, 0, len(all))
	for _, p := range all {
		if p.Status == port.PositionStatusOpen {
			open = append(open, p)
		}
	}

	// Per-symbol ticker cache: one GetTicker call per unique symbol in the
	// result set (rate-limit care — a /api/positions poll with N rows that
	// share a symbol still hits GMO once). Failures (nil ticker / non-nil
	// err) are silently degraded — the row just shows CurrentPrice=0 instead
	// of failing the whole listing. We cache the FULL ticker (not a derived
	// price) so each side can pick its own honest close price below.
	tickerBySymbol := map[string]*market.Ticker{}
	tickerFetched := map[string]bool{}
	tickerFor := func(sym string) *market.Ticker {
		if q.GetTicker == nil {
			return nil
		}
		if tickerFetched[sym] {
			return tickerBySymbol[sym]
		}
		tk, terr := q.GetTicker(ctx, sym)
		if terr != nil {
			tk = nil
		}
		tickerBySymbol[sym] = tk
		tickerFetched[sym] = true
		return tk
	}
	// closePriceFor is the price the position would be CLOSED at right now —
	// the honest "if I exited this instant" valuation that matches GMO's
	// 評価損益. A BUY is closed by SELLING at the BID; a SELL is closed by
	// BUYING back at the ASK. Using MID for both would overstate
	// profit by half the spread on every open row.
	closePriceFor := func(sym, side string) float64 {
		tk := tickerFor(sym)
		if tk == nil {
			return 0
		}
		if side == "SELL" {
			return tk.Ask
		}
		return tk.Bid
	}
	// midFor is used only as the USD→JPY CONVERSION rate for USD-quote pairs
	// (a rate, not a fill), where mid is the right neutral choice.
	midFor := func(sym string) float64 {
		tk := tickerFor(sym)
		if tk == nil {
			return 0
		}
		return (tk.Bid + tk.Ask) / 2
	}

	now := time.Now()
	if q.Clock != nil {
		now = q.Clock()
	}

	out := make([]OpenPositionView, 0, len(open))
	for _, p := range open {
		pip := q.resolvePip(p.Symbol)
		// CurrentPrice is the close-now price for THIS side (BUY→bid, SELL→ask),
		// so the displayed price and the unrealized PnL agree and both match GMO.
		currentPrice := closePriceFor(p.Symbol, p.Side)
		tpPrice, slPrice := position.ComputeTPSLPrices(
			order.Side(p.Side), p.EntryPrice, p.TakeProfitPips, p.StopLossPips, pip,
		)
		var unrealizedPips, unrealizedJPY float64
		if currentPrice > 0 {
			priceDiff := currentPrice - p.EntryPrice
			if p.Side == "SELL" {
				priceDiff = -priceDiff
			}
			unrealizedPips = priceDiff / pip
			// priceDiff*qty is in the QUOTE currency. For JPY-quote pairs that is
			// already JPY (rate 1.0). For USD-quote pairs (EUR_USD) it is USD, so
			// multiply by USD/JPY (reusing the per-symbol ticker cache). If USD/JPY
			// is unavailable, leave 0 rather than show a misleading quote-currency
			// figure — UnrealizedPips remains the currency-agnostic truth.
			rate := 1.0
			if market.QuoteCurrency(p.Symbol) != "JPY" {
				rate = 0
				if mul, rerr := market.QuoteJPYRate(p.Symbol, midFor("USD_JPY")); rerr == nil {
					rate = mul
				}
			}
			unrealizedJPY = priceDiff * float64(p.Quantity) * rate
		}
		deadline := p.OpenedAt.Add(time.Duration(p.MaxHoldMinutes) * time.Minute)
		// "Manual entry" は manual_positions junction の存在で判定する
		// (strategy_config_id='manual' の sentinel は使わない)。
		// IsManual lookup が失敗しても view 自体は返す — UI の警告は
		// 別軸で扱う。
		isManual, _ := q.Positions.IsManual(ctx, p.ID)
		source := string(p.Source)
		if source == "" {
			source = string(port.PositionSourceBot)
		}
		out = append(out, OpenPositionView{
			ID:               p.ID,
			Symbol:           p.Symbol,
			Side:             p.Side,
			Quantity:         p.Quantity,
			EntryPrice:       p.EntryPrice,
			CurrentPrice:     currentPrice,
			UnrealizedPips:   unrealizedPips,
			UnrealizedJPY:    unrealizedJPY,
			TPPrice:          tpPrice,
			SLPrice:          slPrice,
			OpenedAt:         p.OpenedAt,
			ElapsedMinutes:   now.Sub(p.OpenedAt).Minutes(),
			MaxHoldMinutes:   p.MaxHoldMinutes,
			DeadlineAt:       deadline,
			RemainingMinutes: deadline.Sub(now).Minutes(),
			StrategyConfigID: p.StrategyConfigID,
			IsManual:         isManual,
			Source:           source,
		})
	}
	return out, nil
}
