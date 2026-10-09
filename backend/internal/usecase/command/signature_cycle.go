package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// chaseRRFloorMult relaxes the entry-time RR>=MinRR gate when re-validated at the LIVE fill.
// The detector gates RR on the breakout-bar CLOSE; we enter at the live price on a later cycle,
// which is normally near the close (low-frequency daily breakout entered promptly). A small fill
// drift is allowed (×0.8), but a genuine chase that pushes realized RR well below MinRR is skipped.
const chaseRRFloorMult = 0.8

// SignatureCycle is the advisor v2 orchestration. It runs
// PERIODICALLY (not per-tick — LLM stays out of the realtime order path) for one symbol:
//
//  1. deterministic DetectSignatureBreakout on daily candles -> a proposal (geometry only)
//  2. if (and only if) a setup exists, ask the LLM judge (breakout-advisor) to grade the 8 axes
//  3. default no_trade: act only when go=true, label=trend_continuation, and the LLM agrees with
//     the deterministic side
//  4. build an ENTER Signal from the DETERMINISTIC geometry (LLM never sets the stop) and submit
//     it through the EXISTING order path (Submit = ExecuteOrder.OnSignal), which applies the risk
//     Gate (single-position / no-nanpin / daily-loss / spread / qty) and places broker-side OCO.
//
// All collaborators are injected as funcs (same style as AdvisorCycle) so the orchestration is
// unit-testable with fakes and the cmd/bot composition root wires the real adapters.
// SignatureJudgeFunc is the advisor v2 LLM go/no-go gate. nil = deterministic mode (auto-enter).
type SignatureJudgeFunc = func(ctx context.Context, p strategy.BreakoutProposal, summary *market.MarketSummary) (strategy.AdvisorVerdict, error)

type SignatureCycle struct {
	Symbol   string
	Pip      float64
	Quantity int
	// ActiveConfigID returns the symbol's active config_id — a REAL strategy_configs row — used as
	// the position's FK. v2 entries MUST reference a real config or the position INSERT FK-fails
	// (broker order fills but DB has no row → orphan → no ratchet management). "" → skip entry
	// (no_active_config). In exclusive mode the engine config (e.g. trend-v4) does not itself trade,
	// so positions recorded under that id are advisor v2's.
	ActiveConfigID func() string
	Params         strategy.SignatureParams
	// MaxSpreadPips skips entry while the live spread is wider than this (0 = no cap). Defers fills
	// out of the wide-spread window (e.g. 06:00-07:00 JST daily roll / news); the next hourly cycle
	// retries, so entry lands once liquidity normalizes. The broker-side risk Gate is a backstop.
	MaxSpreadPips float64

	// MaxConcurrent caps simultaneous SAME-SYMBOL same-side v2 positions. 0/1 = strict single-position
	// (no nanpin). 2 = "add to a winner": the 2nd opens ONLY when the existing same-side position is
	// already ratchet-ARMED (in strong profit and trailing in profit), so it can no longer lose while
	// the bot is alive → aggregate risk stays ≈ one unit. The risk Gate (Submit) enforces the hard cap.
	MaxConcurrent int
	// OpenPositions returns the symbol's OPEN/CLOSING positions (bot + external) so the cycle can apply
	// the concurrency / pyramid policy. nil = treat as no open positions (single-shot; tests).
	OpenPositions func(ctx context.Context) ([]port.PositionRecord, error)

	DailyCandles func(ctx context.Context) ([]market.Candle, error)
	// Judge is the LLM go/no-go gate. nil = deterministic mode (a found setup auto-enters).
	Judge        SignatureJudgeFunc
	GetTicker    func(ctx context.Context) (*market.Ticker, error)
	BuildSummary func() *market.MarketSummary
	Submit       func(ctx context.Context, sig strategy.Signal, ticker *market.Ticker) error
	Now          func() time.Time // nil -> time.Now
	Logger       *slog.Logger
}

// SignatureCycleResult reports where a cycle stopped (for logging/audit/tests).
type SignatureCycleResult struct {
	Stage    string // no_setup | no_go | side_mismatch | wide_spread | stale_breakout | max_concurrent | pyramid_not_armed | no_active_config | skipped | submitted
	Proposal strategy.BreakoutProposal
	Verdict  strategy.AdvisorVerdict
	// State is the human-facing firing-condition snapshot (trend / trigger / distance / ATR),
	// surfaced in runtime/advisor_v2_status.json + the API so the UI shows when/why it fires.
	State strategy.SignatureState
}

func (c *SignatureCycle) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *SignatureCycle) log(msg string, args ...any) {
	if c.Logger != nil {
		c.Logger.Info(msg, args...)
	}
}

// Run executes one advisor v2 cycle for the symbol. It returns the stage reached; a non-nil
// error is a transport/IO failure (data, ticker, submit) — never a "no trade" outcome, which is
// the normal, common result and is reported via Stage.
func (c *SignatureCycle) Run(ctx context.Context) (SignatureCycleResult, error) {
	daily, err := c.DailyCandles(ctx)
	if err != nil {
		return SignatureCycleResult{}, fmt.Errorf("daily candles: %w", err)
	}
	prop := strategy.DetectSignatureBreakout(daily, c.Pip, c.Params)
	// Display snapshot of the firing conditions (trend / trigger level / distance / ATR), surfaced
	// in the status file + API so the entry conditions are visible from the UI. Best-effort price
	// (last completed daily close; the trigger/distance are what matter for "how close to firing").
	state := strategy.DescribeSignatureState(daily, 0, c.Pip, c.Params)
	res := func(stage string, verdict strategy.AdvisorVerdict) SignatureCycleResult {
		return SignatureCycleResult{Stage: stage, Proposal: prop, Verdict: verdict, State: state}
	}

	if !prop.Found {
		c.log("v2_no_setup", "symbol", c.Symbol, "reason", prop.Reason)
		return res("no_setup", strategy.AdvisorVerdict{}), nil
	}

	var verdict strategy.AdvisorVerdict
	if c.Judge == nil {
		// Deterministic mode (advisor_v2.deterministic): no LLM — a found setup auto-enters.
		// Safety stays deterministic (risk Gate in Submit: spread / event-freeze / single-position /
		// daily-loss).
		verdict = strategy.AdvisorVerdict{
			Go: true, Label: prop.Label, Side: prop.Side,
			Level: prop.Level, ATRPips: prop.ATRPips, Invalidation: prop.Invalidation, Reason: "deterministic_auto",
		}
	} else {
		var summary *market.MarketSummary
		if c.BuildSummary != nil {
			summary = c.BuildSummary()
		}
		v, jerr := c.Judge(ctx, prop, summary)
		if jerr != nil {
			// A transport error from the judge: fail safe to no-trade, surface the error for retry.
			return res("no_go", strategy.AdvisorVerdict{}), fmt.Errorf("judge: %w", jerr)
		}
		verdict = v
	}
	// Default no_trade: act ONLY on an explicit, consistent go.
	if !verdict.Go || verdict.Label != strategy.BreakoutTrendCont {
		c.log("v2_no_go", "symbol", c.Symbol, "label", string(verdict.Label), "reason", verdict.Reason)
		return res("no_go", verdict), nil
	}
	if verdict.Side != prop.Side {
		// LLM disagreed with the deterministic breakout direction — safe skip (never override geometry).
		c.log("v2_side_mismatch", "symbol", c.Symbol, "llm_side", string(verdict.Side), "det_side", string(prop.Side))
		return res("side_mismatch", verdict), nil
	}

	ticker, err := c.GetTicker(ctx)
	if err != nil {
		return res("submitted", verdict), fmt.Errorf("ticker: %w", err)
	}
	// Wide-spread guard: skip while spread is abnormal (06:00-07:00 JST roll / news). The next
	// hourly cycle retries, so the confirmed breakout still gets entered once spread normalizes.
	if c.MaxSpreadPips > 0 && c.Pip > 0 {
		if spreadPips := (ticker.Ask - ticker.Bid) / c.Pip; spreadPips > c.MaxSpreadPips {
			c.log("v2_wide_spread", "symbol", c.Symbol, "spread_pips", spreadPips, "cap", c.MaxSpreadPips)
			return res("wide_spread", verdict), nil
		}
	}
	entry := ticker.Ask
	if prop.Side == order.SideSell {
		entry = ticker.Bid
	}
	// Chase guard (re-validate the edge at the LIVE fill). The detector gated RR>=MinRR on the
	// breakout-bar CLOSE, but we enter at the live price on a later cycle and a daily breakout stays
	// "the setup" for ~24h. If price has chased far from the level, the REALIZED reward:risk against
	// the same structural invalidation/target can fall below the threshold — skip rather than enter a
	// stale breakout. (Raising the SL sanity cap for daily stops removed the implicit pip-cap that
	// used to reject chases at the order boundary, so this is now the primary chase protection.)
	liveStopPips := math.Abs(entry-prop.Invalidation) / c.Pip
	liveRewardPips := math.Abs(prop.TargetRef-entry) / c.Pip
	if liveStopPips <= 0 || liveRewardPips <= 0 {
		c.log("v2_stale_breakout", "symbol", c.Symbol, "reason", "no_room", "entry", entry)
		return res("stale_breakout", verdict), nil
	}
	if liveRR := liveRewardPips / liveStopPips; liveRR < c.Params.MinRR*chaseRRFloorMult {
		c.log("v2_stale_breakout", "symbol", c.Symbol, "live_rr", liveRR,
			"floor", c.Params.MinRR*chaseRRFloorMult, "entry", entry, "level", prop.Level)
		return res("stale_breakout", verdict), nil
	}
	// Resolve the position's FK to a REAL active config_id. Without it OnSignal's position INSERT
	// FK-fails (broker order would fill but leave an orphan with no ratchet management). Skip the
	// entry entirely — never place an order we cannot record.
	cfgID := ""
	if c.ActiveConfigID != nil {
		cfgID = c.ActiveConfigID()
	}
	if cfgID == "" {
		c.log("v2_no_active_config", "symbol", c.Symbol)
		return res("no_active_config", verdict), nil
	}
	// Concurrency / pyramid policy. Default cap = 1 (strict single-position, no nanpin). When
	// MaxConcurrent > 1 ("add to a winner"), a 2nd same-side entry is allowed ONLY when every
	// existing same-side position is already ratchet-ARMED — i.e. the prior trade is in strong
	// profit and trailing in profit, so it can no longer produce a loss while the bot is alive and
	// aggregate risk stays ≈ one unit. The risk Gate (Submit) enforces the hard ceiling too.
	maxConc := c.MaxConcurrent
	if maxConc < 1 {
		maxConc = 1
	}
	sameSide := 0
	if c.OpenPositions != nil {
		opens, oerr := c.OpenPositions(ctx)
		if oerr != nil {
			return res("submitted", verdict), fmt.Errorf("open positions: %w", oerr)
		}
		for _, p := range opens {
			if p.Side != string(prop.Side) {
				continue
			}
			sameSide++
			if !p.RatchetArmed {
				// Existing same-side position not yet winning enough to add on top of — never nanpin.
				c.log("v2_pyramid_not_armed", "symbol", c.Symbol, "open_same_side", sameSide)
				return res("pyramid_not_armed", verdict), nil
			}
		}
	}
	if sameSide >= maxConc {
		c.log("v2_max_concurrent", "symbol", c.Symbol, "open_same_side", sameSide, "cap", maxConc)
		return res("max_concurrent", verdict), nil
	}
	// Build the ENTER from the DETERMINISTIC proposal geometry (LLM never sets the stop). TP is the
	// measured-move target (broker-side OCO, like SL); the ratchet trails on top for early exit.
	sig := strategy.BuildSignatureSignal(prop.Side, entry, prop.Invalidation, prop.TargetRef, prop.ATRPips, c.Pip, prop.Label, c.Quantity, config.StrategySignatureBreakout, c.now())
	sig.ConfigID = cfgID
	sig.MaxConcurrent = maxConc // let the risk Gate permit up to maxConc same-side (default 1 = no nanpin)
	if !sig.IsEntry() {
		c.log("v2_signal_not_entry", "symbol", c.Symbol, "reason", sig.Reason)
		return res("skipped", verdict), nil
	}
	if err := c.Submit(ctx, sig, ticker); err != nil {
		var rej *AdmissionRejectedError
		if errors.As(err, &rej) {
			// Risk-gate refusal = a normal outcome, not a transport failure.
			c.log("v2_admission_rejected", "symbol", c.Symbol, "side", string(prop.Side), "reason", rej.Reason)
			return res("admission_rejected", verdict), nil
		}
		return res("submitted", verdict), fmt.Errorf("submit: %w", err)
	}
	c.log("v2_submitted", "symbol", c.Symbol, "side", string(prop.Side), "sl_pips", sig.StopLossPips, "rr", prop.RR)
	return res("submitted", verdict), nil
}
