package port

import "time"

// LLMDecisionLogEntry is one durable record of the autonomous LLM trade loop's per-cycle
// behaviour. The loop already logs each outcome to stdout and to an OVERWRITTEN status snapshot,
// but neither survives as history — so a slow drawdown ("じわ負け") or a silent regression (e.g.
// a recurring yaml_unmarshal_error) cannot be reviewed after the fact. The journal is the
// append-only history that closes that gap.
//
// Event distinguishes a normal cycle outcome ("cycle": Stage/Go/Side/TP/SL/Reason) from the
// adapter's parse-fallback diagnostic ("parse_fallback": RawStdout — the raw claude output,
// without which the cause of a parse failure cannot be diagnosed).
type LLMDecisionLogEntry struct {
	Time      time.Time `json:"time"`
	Symbol    string    `json:"symbol"`
	Event     string    `json:"event"`                // "cycle" | "parse_fallback"
	Stage     string    `json:"stage,omitempty"`      // cycle: no_trade | submitted | decider_error | ...
	Go        bool      `json:"go"`                   // cycle: did the LLM choose to trade
	Side      string    `json:"side,omitempty"`       // BUY | SELL
	TPPips    float64   `json:"tp_pips,omitempty"`    //
	SLPips    float64   `json:"sl_pips,omitempty"`    //
	Reason    string    `json:"reason,omitempty"`     // cycle: LLM reason / parse error code; parse_fallback: go-yaml error
	Error     string    `json:"error,omitempty"`      // cycle: transport/IO error text, if any
	RawStdout string    `json:"raw_stdout,omitempty"` // parse_fallback: truncated raw claude stdout for diagnosis
	// Arms is the compact human-readable summary of the conditional (armed) plans this cycle
	// placed (stage=armed) or the plan that fired (event=arm_fire) — e.g.
	// "SELL@161.950↓ TP30/SL25 [L1]". Empty on ordinary cycles.
	Arms string `json:"arms,omitempty"`

	// RejectReason is the risk-gate refusal behind stage=admission_rejected (e.g.
	// "loss_in_window 2500 >= cap 2000"). Distinct from Error (a refusal is a normal
	// outcome, not a failure) and from Reason (the LLM's own prose). Without it a
	// rejection would be journalled as "submitted" with no reason anywhere, which
	// makes a silent entry freeze undiagnosable.
	RejectReason string `json:"reject_reason,omitempty"`

	// Decision-time market CONTEXT (objective state that drove the call) — makes
	// the journal an analyzable knowledge base, not just prose: each record pairs
	// "why we decided" (Reason) with the numbers we saw. The reflection loop / a
	// human can mine "this kind of setup → this outcome" over time. 0/"" = absent.
	Price      float64 `json:"price,omitempty"`
	SpreadPips float64 `json:"spread_pips,omitempty"`
	ATR5mPips  float64 `json:"atr_5m_pips,omitempty"`
	ATR1hPips  float64 `json:"atr_1h_pips,omitempty"`
	Trend1h    string  `json:"trend_1h,omitempty"`
	// RangePos24h is the trailing-24h range position (0=low, 1=high) the entry rules
	// judged on; recorded only when it was actually measured (24h candles + a real
	// ticker — on a ticker-fetch failure the summary fabricates a neutral 0.5, which
	// must not be archived as fact). 0 = absent.
	RangePos24h float64 `json:"range_pos_24h,omitempty"`
	// HTFVetoExempt marks a submitted entry that would have been refused by the MTF
	// directional veto but was waived as a positioned pattern (the htf_trend_veto
	// range-position exemption). Durable ON PURPOSE: re-verifying the exempt trades
	// later needs to find them here, not in rotated stdout logs.
	HTFVetoExempt bool `json:"htf_veto_exempt,omitempty"`
}

// LLMDecisionJournal persists LLMDecisionLogEntry records durably (append-only). A nil journal is
// a valid no-op — journaling is observability and must NEVER sit on the trade-critical path: a
// Record error must not fail a decision cycle; callers log it and continue.
type LLMDecisionJournal interface {
	Record(e LLMDecisionLogEntry) error
}
