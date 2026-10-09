package handler

import "time"

// ManualTradeRequest は POST /api/trade/manual の JSON ボディ。
//
// AllowOverride is the explicit operator-override flag.
// Default (false / absent) means manual entries hit exactly the same gates
// as auto entries — emergency_stop / daily_loss / direction / spread /
// max_open_positions / cooldown / no_active_config are all enforced. Set
// true only when the operator intentionally wants to override the
// allowlisted soft gates (see command.isOverridableReason: open_positions,
// cooldown, consecutive_losses, post_loss_freeze, trades_in_window,
// loss_in_window, direction_*, spread); hard safety gates remain enforced.
type ManualTradeRequest struct {
	Symbol         string  `json:"symbol"`           // required for multi-symbol bundles
	Side           string  `json:"side"`             // "BUY" or "SELL"
	TakeProfitPips float64 `json:"take_profit_pips"` // e.g. 30.0
	StopLossPips   float64 `json:"stop_loss_pips"`   // e.g. 20.0
	MaxHoldMinutes int     `json:"max_hold_minutes"` // e.g. 240
	Quantity       int     `json:"quantity"`         // 0 → default
	AllowOverride  bool    `json:"allow_override"`   // default false; operator-explicit only
}

// ManualTradeResponse は POST /api/trade/manual の JSON レスポンス。
type ManualTradeResponse struct {
	PositionID int64   `json:"position_id"`
	Side       string  `json:"side"`
	Quantity   int     `json:"quantity"`
	EntryPrice float64 `json:"entry_price"`
	TPPrice    float64 `json:"tp_price"`
	SLPrice    float64 `json:"sl_price"`
	DurationMs int64   `json:"duration_ms"`
	Error      string  `json:"error,omitempty"`
}

// ClosePositionRequest は POST /api/positions/close の JSON ボディ。
// Symbol は multi-symbol bundle の dispatch key (= 該当 position の
// OpenPositionView.Symbol)。frontend は表示中の row の symbol を必ず添える。
type ClosePositionRequest struct {
	ID     int64  `json:"id"`
	Symbol string `json:"symbol"`
}

// ClosePositionResponse は POST /api/positions/close の JSON レスポンス。
type ClosePositionResponse struct {
	PositionID     int64   `json:"position_id"`
	Side           string  `json:"side"`
	EntryPrice     float64 `json:"entry_price"`
	ExitPrice      float64 `json:"exit_price"`
	ProfitLossPips float64 `json:"profit_loss_pips"`
	ProfitLossJPY  float64 `json:"profit_loss_jpy"`
	Error          string  `json:"error,omitempty"`
}

// ExtendPositionRequest は POST /api/positions/extend の JSON ボディ。
// AddMinutes は max_hold_minutes に加算する分数 (= 延長幅)。Symbol は表示中
// の行から添えられるが、dispatch には使わない (position は id で一意)。
type ExtendPositionRequest struct {
	ID         int64  `json:"id"`
	Symbol     string `json:"symbol"`
	AddMinutes int    `json:"add_minutes"`
}

// ExtendPositionResponse は POST /api/positions/extend の JSON レスポンス。
// MaxHoldMinutes は加算後の合計、DeadlineAt = OpenedAt + MaxHoldMinutes。
type ExtendPositionResponse struct {
	PositionID       int64     `json:"position_id"`
	MaxHoldMinutes   int       `json:"max_hold_minutes"`
	AddedMinutes     int       `json:"added_minutes"`
	DeadlineAt       time.Time `json:"deadline_at"`
	RemainingMinutes float64   `json:"remaining_minutes"`
	Error            string    `json:"error,omitempty"`
}

// OpenPositionView / AdvisorDecisionView は usecase/query/ 配下に置く
// (handler/types.go には置かない)。

// TriggerResult は POST /api/advisor/trigger の JSON レスポンス。
// Trigger は Command 寄り (state-changing) のため handler 側に残す。
//
// Multi-symbol fan-out: symbol を指定しない fire (空 body, or
// {"symbol":""}) は全 bundle を並列に動かし、結果を PerSymbol に詰める。
// 後方互換のため top-level (Promoted/ConfigID/...) は primary symbol (= 全
// fan-out なら最初の symbol、symbol 指定 fire ならその symbol) の結果。
type TriggerResult struct {
	Symbol       string                       `json:"symbol,omitempty"`
	Promoted     bool                         `json:"promoted"`
	ConfigID     string                       `json:"config_id,omitempty"`
	Strategy     string                       `json:"strategy,omitempty"`
	Enabled      bool                         `json:"enabled"`
	RejectReason string                       `json:"reject_reason,omitempty"`
	DurationMs   int64                        `json:"duration_ms"`
	Error        string                       `json:"error,omitempty"`
	PerSymbol    map[string]SymbolTriggerInfo `json:"per_symbol,omitempty"`
}

// SymbolTriggerInfo は PerSymbol fan-out の 1 symbol 分の結果。
type SymbolTriggerInfo struct {
	Promoted     bool   `json:"promoted"`
	ConfigID     string `json:"config_id,omitempty"`
	Strategy     string `json:"strategy,omitempty"`
	Enabled      bool   `json:"enabled"`
	RejectReason string `json:"reject_reason,omitempty"`
	Error        string `json:"error,omitempty"`
}

// TriggerAdvisorRequest は POST /api/advisor/trigger の JSON ボディ。
// Symbol は省略可。空欄なら全 bundle 並列発火。
type TriggerAdvisorRequest struct {
	Symbol string `json:"symbol"`
}

// AskClaudeRequest は POST /api/ask-claude の JSON ボディ。
type AskClaudeRequest struct {
	Question string `json:"question"`
}

// AskClaudeResponse は POST /api/ask-claude の JSON レスポンス。
type AskClaudeResponse struct {
	Answer     string `json:"answer,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}
