package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// Lane 土俵 bounds re-checked at fire time, keyed by the plan's tag. MUST stay in sync with
// the playbook's L1/L4 boxes (chg24 band + 6h agreement) and configs/bot_config.live.yaml's
// htf/exhaustion thresholds — the fire-time re-check is the code half of the arm promise
// ("発火時に土俵を再確認するので古いシナリオは撃たれない").
const (
	laneL1MinDropPips = 20.0 // [L1] SELL fires only while chg24 ≤ −20 (fresh falling day)
	laneL4MinRisePips = 30.0 // [L4] BUY fires only while chg24 ≥ +30 (clear rising day)

	// fireCooldown throttles re-attempts after ANY failed fire (veto/reject): the trigger
	// stays crossed tick after tick, so without a cooldown a persistent veto would hammer
	// the journal and the summary builder. One distinct journal entry per veto state.
	fireCooldown = 30 * time.Second
)

// ArmedFireResult reports where one tick's fire attempt stopped, for tests/logs.
type ArmedFireResult struct {
	Stage string // idle | cooldown | armed_expired | lane_recheck_failed | no_entry_hour | night_buy_veto | chase_buy_veto | sell_low_veto | exhaustion_veto | spike_veto | htf_trend_veto | wide_spread | slot_taken | no_active_config | admission_rejected | fire_error | armed_fired
	Plan  *strategy.ArmedPlan
}

// ArmedFire is the deterministic executor of the LLM's pre-placed conditional plans.
// It watches ticks; when the price crosses a plan's trigger it
// RE-VALIDATES everything on fresh market state — spread, night/chase/sell-low, exhaustion,
// spike, htf counter-trend, and the plan's lane 土俵 ([L1]/[L4] chg24 band + 6h agreement,
// fail-CLOSED: a pre-placed order needs positive confirmation) — then submits through the
// SAME OnSignal path as every other entry (risk Gate + broker OCO). No LLM call at fire time.
type ArmedFire struct {
	Symbol   string
	Pip      float64
	Quantity int

	MaxHoldMinutes      int
	RatchetArmPips      float64
	RatchetGivebackPips float64
	MaxSpreadPips       float64
	MaxConcurrent       int

	// Fire-time veto thresholds — wired from the SAME config values as LLMDecisionCycle.
	HTFTrendVetoPips     float64
	NightBuyVetoHoursJST []int
	// NoEntryHoursJST (session guard): ALL-side fire ban in these JST hours —
	// the cycle skips its decisions there, and a plan armed earlier must not fire into
	// the band either (深夜建ての玉が早朝 05:45 JST 前後のスプレッド拡大で SL を刈られる型)。Plans stay
	// armed (band 明けの扱いは次サイクルの総入替に委ねる)。Empty = OFF.
	NoEntryHoursJST    []int
	MaxRangePos24hBuy  float64
	MinRangePos24hSell float64
	ExhaustionVetoPips float64
	SpikeVetoPips15m   float64

	GetArms        func() ([]strategy.ArmedPlan, time.Time, bool)
	ClearArms      func()
	ActiveConfigID func() string
	// BuildSummary builds the FIRE-TIME market snapshot from the given live ticker — wired to
	// the worker's in-memory aggregator windows (no network, no DB) so a fire attempt cannot
	// stall the 1s price loop on I/O.
	BuildSummary  func(ticker *market.Ticker) *market.MarketSummary
	OpenPositions func(ctx context.Context) ([]port.PositionRecord, error)
	Submit        func(ctx context.Context, sig strategy.Signal, ticker *market.Ticker) error
	Journal       port.LLMDecisionJournal
	Now           func() time.Time
	Logger        *slog.Logger

	mu          sync.Mutex
	nextAttempt time.Time
	lastVeto    string
}

func (f *ArmedFire) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *ArmedFire) log(msg string, args ...any) {
	if f.Logger != nil {
		f.Logger.Info(msg, args...)
	}
}

// journal records one fire-path outcome (Event=arm_fire). Observability only — never blocks.
func (f *ArmedFire) journal(stage string, plan *strategy.ArmedPlan, entry float64, spread float64, rejectReason, errText string) {
	if f.Journal == nil {
		return
	}
	e := port.LLMDecisionLogEntry{
		Time: f.now(), Symbol: f.Symbol, Event: "arm_fire", Stage: stage,
		Price: entry, SpreadPips: spread, RejectReason: rejectReason, Error: errText,
	}
	if plan != nil {
		e.Go = stage == "armed_fired"
		e.Side = string(plan.Side)
		e.TPPips = plan.TPPips
		e.SLPips = plan.SLPips
		e.Reason = plan.Reason
		e.Arms = armsSummary([]strategy.ArmedPlan{*plan})
	}
	if rerr := f.Journal.Record(e); rerr != nil {
		f.log("armed_fire_journal_failed", "symbol", f.Symbol, "err", rerr)
	}
}

// veto finalizes a failed fire attempt: cooldown armed, journalled ONCE per distinct veto state.
func (f *ArmedFire) veto(stage string, plan *strategy.ArmedPlan, entry, spread float64, rejectReason string) ArmedFireResult {
	f.nextAttempt = f.now().Add(fireCooldown)
	if f.lastVeto != stage {
		f.lastVeto = stage
		f.journal(stage, plan, entry, spread, rejectReason, "")
	}
	f.log("armed_fire_veto", "symbol", f.Symbol, "stage", stage)
	return ArmedFireResult{Stage: stage, Plan: plan}
}

// OnTick evaluates the current tick against the armed plans. Called from the per-symbol price
// loop; cheap when idle (one holder read + float compares). Never fires the LLM.
func (f *ArmedFire) OnTick(ctx context.Context, bid, ask float64) (ArmedFireResult, error) {
	if f.GetArms == nil || f.Submit == nil || bid <= 0 || ask <= 0 {
		return ArmedFireResult{Stage: "idle"}, nil
	}
	plans, expiresAt, ok := f.GetArms()
	if !ok || len(plans) == 0 {
		return ArmedFireResult{Stage: "idle"}, nil
	}
	now := f.now()
	if now.After(expiresAt) {
		if f.ClearArms != nil {
			f.ClearArms()
		}
		f.journal("armed_expired", &plans[0], 0, 0, "", "")
		f.log("armed_expired", "symbol", f.Symbol)
		return ArmedFireResult{Stage: "armed_expired"}, nil
	}
	// Trigger crossing: SELL fires when the BID falls through the level (that is the price a
	// market sell fills at); BUY when the ASK rises through it.
	var hit *strategy.ArmedPlan
	for i := range plans {
		p := &plans[i]
		if p.Side == order.SideSell && !p.BreakAbove && bid <= p.TriggerPrice {
			hit = p
			break
		}
		if p.Side == order.SideBuy && p.BreakAbove && ask >= p.TriggerPrice {
			hit = p
			break
		}
	}
	if hit == nil {
		return ArmedFireResult{Stage: "idle"}, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if now.Before(f.nextAttempt) {
		return ArmedFireResult{Stage: "cooldown", Plan: hit}, nil
	}

	entry := ask
	if hit.Side == order.SideSell {
		entry = bid
	}
	spread := 0.0
	if f.Pip > 0 {
		spread = (ask - bid) / f.Pip
	}

	// ---- fire-time re-validation (fresh state; vetoes mirror LLMDecisionCycle) ----
	// Session guard: no fire (any side) inside the no-entry band — the pure clock
	// check runs first (cheapest, and the band bans the hour regardless of market state).
	if jstHourIn(now, f.NoEntryHoursJST) {
		return f.veto("no_entry_hour", hit, entry, spread, ""), nil
	}
	if f.MaxSpreadPips > 0 && spread > f.MaxSpreadPips {
		return f.veto("wide_spread", hit, entry, spread, ""), nil
	}
	if hit.Side == order.SideBuy && jstHourIn(now, f.NightBuyVetoHoursJST) {
		return f.veto("night_buy_veto", hit, entry, spread, ""), nil
	}
	tickerNow := &market.Ticker{Bid: bid, Ask: ask, Timestamp: now}
	summary := (*market.MarketSummary)(nil)
	if f.BuildSummary != nil {
		summary = f.BuildSummary(tickerNow)
	}
	if summary != nil && summary.Summary24h.NumCandles > 0 {
		rpos := summary.Summary24h.RangePositionPct
		if hit.Side == order.SideBuy && f.MaxRangePos24hBuy > 0 && rpos > f.MaxRangePos24hBuy {
			return f.veto("chase_buy_veto", hit, entry, spread, ""), nil
		}
		if hit.Side == order.SideSell && f.MinRangePos24hSell > 0 && rpos < f.MinRangePos24hSell {
			return f.veto("sell_low_veto", hit, entry, spread, ""), nil
		}
		chg := summary.Summary24h.ChangePips
		if f.ExhaustionVetoPips > 0 &&
			((hit.Side == order.SideSell && chg <= -f.ExhaustionVetoPips) ||
				(hit.Side == order.SideBuy && chg >= f.ExhaustionVetoPips)) {
			return f.veto("exhaustion_veto", hit, entry, spread, ""), nil
		}
		if f.HTFTrendVetoPips > 0 &&
			((hit.Side == order.SideBuy && chg <= -f.HTFTrendVetoPips) ||
				(hit.Side == order.SideSell && chg >= f.HTFTrendVetoPips)) {
			return f.veto("htf_trend_veto", hit, entry, spread, ""), nil
		}
	}
	if summary != nil && summary.Summary15m.NumCandles > 0 && f.SpikeVetoPips15m > 0 {
		if chg := summary.Summary15m.ChangePips; chg >= f.SpikeVetoPips15m || chg <= -f.SpikeVetoPips15m {
			return f.veto("spike_veto", hit, entry, spread, ""), nil
		}
	}
	// Lane 土俵 re-check — FAIL-CLOSED: a pre-placed order only fires on positive confirmation
	// that the lane's premise still holds (missing/blind data = no fire, unlike the vetoes).
	if !laneStillHolds(hit, summary) {
		return f.veto("lane_recheck_failed", hit, entry, spread, ""), nil
	}
	// Single position slot: any open/closing position on this symbol blocks a pre-placed fire
	// (the entry paths share one slot; external/manual positions count too).
	if f.OpenPositions != nil {
		opens, oerr := f.OpenPositions(ctx)
		if oerr != nil {
			f.nextAttempt = now.Add(fireCooldown)
			return ArmedFireResult{Stage: "fire_error", Plan: hit}, fmt.Errorf("open positions: %w", oerr)
		}
		if len(opens) > 0 {
			return f.veto("slot_taken", hit, entry, spread, ""), nil
		}
	}
	cfgID := ""
	if f.ActiveConfigID != nil {
		cfgID = f.ActiveConfigID()
	}
	if cfgID == "" {
		return f.veto("no_active_config", hit, entry, spread, ""), nil
	}

	sig := strategy.BuildLLMSignal(hit.Side, entry, hit.TPPips, hit.SLPips, f.Quantity,
		f.MaxHoldMinutes, f.RatchetArmPips, f.RatchetGivebackPips, config.StrategyLLMDecision, now)
	sig.ConfigID = cfgID
	sig.Reason = hit.Reason
	maxConc := f.MaxConcurrent
	if maxConc < 1 {
		maxConc = 1
	}
	sig.MaxConcurrent = maxConc
	if !sig.IsEntry() {
		return f.veto("fire_error", hit, entry, spread, ""), nil
	}

	if err := f.Submit(ctx, sig, tickerNow); err != nil {
		var rej *AdmissionRejectedError
		if errors.As(err, &rej) {
			// Risk-gate refusal = normal outcome; plans stay armed (cap may free up, or the
			// next cycle replaces them). Journalled with the gate's own reason.
			f.nextAttempt = now.Add(fireCooldown)
			f.lastVeto = "admission_rejected"
			f.journal("admission_rejected", hit, entry, spread, rej.Reason, "")
			return ArmedFireResult{Stage: "admission_rejected", Plan: hit}, nil
		}
		f.nextAttempt = now.Add(fireCooldown)
		f.journal("fire_error", hit, entry, spread, "", err.Error())
		return ArmedFireResult{Stage: "fire_error", Plan: hit}, fmt.Errorf("armed fire submit: %w", err)
	}

	if f.ClearArms != nil {
		f.ClearArms()
	}
	f.lastVeto = ""
	f.journal("armed_fired", hit, entry, spread, "", "")
	f.log("armed_fired", "symbol", f.Symbol, "side", string(hit.Side),
		"trigger", hit.TriggerPrice, "entry", entry, "tp", hit.TPPips, "sl", hit.SLPips)
	return ArmedFireResult{Stage: "armed_fired", Plan: hit}, nil
}

// laneStillHolds re-confirms the plan's lane 土俵 on fresh state, keyed by its tag:
//
//	[L1] SELL: chg24 ≤ −laneL1MinDropPips AND 6h falling (agreement)
//	[L4] BUY : chg24 ≥ +laneL4MinRisePips AND 6h rising
//
// No tag / no trustworthy windows → false (fail-closed).
func laneStillHolds(p *strategy.ArmedPlan, s *market.MarketSummary) bool {
	if s == nil || s.Summary24h.NumCandles <= 0 || s.Summary6h.NumCandles <= 0 {
		return false
	}
	chg24 := s.Summary24h.ChangePips
	chg6 := s.Summary6h.ChangePips
	switch laneTagOf(p.Reason) {
	case "[L1]":
		return p.Side == order.SideSell && chg24 <= -laneL1MinDropPips && chg6 < 0
	case "[L4]":
		return p.Side == order.SideBuy && chg24 >= laneL4MinRisePips && chg6 > 0
	}
	return false
}
