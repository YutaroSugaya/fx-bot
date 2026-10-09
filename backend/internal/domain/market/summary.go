package market

import "time"

// MarketSummary is the JSON payload sent to Claude as the input for hourly
// strategy config generation. Field names match prompts/generate_strategy_config.md.
//
// IMPORTANT: This struct (and everything below it) must never embed API keys,
// DSNs, or other secrets — it is serialized to runtime/ai_input/latest_summary.json
// and forwarded to a subprocess (claude -p). Builder tests assert no secret
// leakage.
type MarketSummary struct {
	Symbol        string      `json:"symbol"`
	Time          time.Time   `json:"time"`
	NextValidFrom time.Time   `json:"next_valid_from"` // valid_from に Claude が必ず使う時刻 (= 次の minute boundary)
	CurrentRate   CurrentRate `json:"current_rate"`
	// Summary5m aggregates the genuine 5-minute candle stream. Its atr_pips is the
	// trader's "ATR(5m)" (mean True Range over 5m candles, in pips) — distinct from the
	// 1m-derived atr_pips on the windows below. It exists so the
	// playbook's ATR(5m) conditions reference a correctly-scaled value (the 1m-derived
	// ATR is ~1/5 the size, so ATR(5m) thresholds checked against it never fire). Zero value
	// when no 5m candles are available.
	Summary5m         WindowSummary      `json:"summary_5m"`
	Summary15m        WindowSummary      `json:"summary_15m"`
	Summary1h         WindowSummary      `json:"summary_1h"`
	Summary6h         WindowSummary      `json:"summary_6h"`
	Summary24h        WindowSummary      `json:"summary_24h"`
	BotState          BotState           `json:"bot_state"`
	RecentTrades      []RecentTrade      `json:"recent_trades"`
	RecentRejections  []RecentRejection  `json:"recent_rejections"`
	HardLimits        *HardLimitsForJSON `json:"hard_limits"`
	AllowedStrategies []string           `json:"allowed_strategies,omitempty"`

	// RecentDecisions は直近の advisor 判断 (= 自分が前回までに出した config) の
	// 要約。Claude に「過去の自分の判断」を見せて、イベント中に前回方針を延長すべきか
	// 作り直すべきかを判断させるため。空/nil = 履歴なし。
	RecentDecisions []RecentDecision `json:"recent_decisions,omitempty"`

	// EventContext は now に最も関連する経済イベント (event_calendar 由来)。
	// nil = 近接イベントなし。Claude が「今はイベント前/中」を認識して
	// breakout 仕込み・cadence 短縮・方針延長を判断するための文脈。
	EventContext *EventContext `json:"event_context,omitempty"`
}

// RecentDecision は過去 1 回分の advisor 判断の compact view。
// 元の port.StrategyConfigRecord に direction / next_advisor_run の列が無く
// 埋められない (常に空送り = ノイズ) ため出さない。regime_type / strategy_name /
// age_minutes だけで「過去の自分の判断」の連続性は伝わる。
type RecentDecision struct {
	RegimeType   string `json:"regime_type"`
	StrategyName string `json:"strategy_name"`
	AgeMinutes   int    `json:"age_minutes"` // 何分前の判断か
}

// EventContext は now 近傍の経済イベント文脈。
type EventContext struct {
	Name         string `json:"name"`
	Policy       string `json:"policy"`        // "freeze" | "breakout"
	MinutesUntil int    `json:"minutes_until"` // at までの分数 (窓内で at 後なら負)
	InWindow     bool   `json:"in_window"`     // [at-pre, at+post] 内か
}

// CurrentRate is the most-recent observable bid/ask + computed spread.
type CurrentRate struct {
	Bid        float64   `json:"bid"`
	Ask        float64   `json:"ask"`
	SpreadPips float64   `json:"spread_pips"`
	Timestamp  time.Time `json:"timestamp,omitempty"`
}

// WindowSummary aggregates a time-window of candles for Claude consumption.
type WindowSummary struct {
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	RangePips float64 `json:"range_pips"`
	// ChangePips is the window's NET directional move: first open → last close, in pips
	// (negative = the window fell). The LLM playbook's lane/veto yardstick — range_position
	// says WHERE price sits in the range, ChangePips says HOW FAR it has already travelled,
	// which is what separates a fresh move (tradeable) from an exhausted one (chase).
	ChangePips         float64 `json:"change_pips"`
	RealizedVolatility float64 `json:"realized_volatility"`
	// ATRPips is the mean True Range over the window's candles, in pips.
	// A vol measure better suited to stop/target sizing than
	// realized_volatility. 0 when no candles.
	ATRPips        float64 `json:"atr_pips,omitempty"`
	TrendDirection string  `json:"trend_direction"` // "up" | "down" | "flat"
	AvgSpreadPips  float64 `json:"avg_spread_pips,omitempty"`
	MaxSpreadPips  float64 `json:"max_spread_pips,omitempty"`
	Support        float64 `json:"support,omitempty"`
	Resistance     float64 `json:"resistance,omitempty"`
	// RangePositionPct is where the current price sits within [low, high]:
	// 0 = at the window low (support zone), 1 = at the high
	// (resistance zone), 0.5 = mid / no range. Set by BuildMarketSummary from
	// the live mid price (BuildWindowSummary alone leaves it 0). NOT omitempty
	// because 0.0 ("at the low") is the most actionable value.
	RangePositionPct float64 `json:"range_position_pct"`
	NumCandles       int     `json:"num_candles,omitempty"`
}

// BotState communicates the bot's own runtime state to Claude so it can
// throttle / disable trading appropriately.
type BotState struct {
	Mode                  string  `json:"mode"`
	EmergencyStop         bool    `json:"emergency_stop"`
	CurrentPosition       *string `json:"current_position"` // null when flat
	OpenPositionsCount    int     `json:"open_positions_count"`
	DailyPnLJPY           float64 `json:"daily_pnl_jpy"`
	ConsecutiveLosses     int     `json:"consecutive_losses"`
	TradesToday           int     `json:"trades_today"`
	TradesInCurrentWindow int     `json:"trades_in_current_window"`
}

// SpreadSample is one timestamped spread observation, used to compute
// per-window avg/max spread in MarketSummary.
type SpreadSample struct {
	Time time.Time
	Pips float64
}

// RecentTrade is a compact record for the last N trades in summary feed.
type RecentTrade struct {
	Side           string  `json:"side"`
	ProfitLossPips float64 `json:"profit_loss_pips"`
	ProfitLossJPY  float64 `json:"profit_loss_jpy"`
	CloseReason    string  `json:"close_reason"`
}

// RecentRejection is a compact (reason, count) view.
type RecentRejection struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// HardLimitsForJSON is the subset of hard limits passed back to Claude so it
// generates configs inside our bounds. We re-declare it here to keep this
// package self-contained (domain → no upward import of config).
type HardLimitsForJSON struct {
	Quantity               IntBound   `json:"quantity"`
	MaxSpreadPips          float64    `json:"max_spread_pips"`
	MaxTradesInThisWindow  int        `json:"max_trades_in_this_window"`
	MaxLossInThisWindowJPY int        `json:"max_loss_in_this_window_jpy"`
	MaxDailyLossJPY        int        `json:"max_daily_loss_jpy"`
	MaxConsecutiveLosses   int        `json:"max_consecutive_losses"`
	StopLossPips           FloatBound `json:"stop_loss_pips"`
	TakeProfitPips         FloatBound `json:"take_profit_pips"`
}

type IntBound struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type FloatBound struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}
