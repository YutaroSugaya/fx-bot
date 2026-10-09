package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// LLMDecideFunc is the autonomous trade-decider brain. It reads the market
// summary + the current playbook (accumulated lessons + rules the reflection loop maintains) and
// returns a domain decision (side + TP/SL, or no-trade). nil = no decider wired → always no-trade.
// A transport error fails safe to no-trade (the cycle logs/retries); the adapter is fake-injectable.
type LLMDecideFunc = func(ctx context.Context, summary *market.MarketSummary, playbook string) (strategy.LLMTradeDecision, error)

// LLMDecisionCycle is the autonomous LLM trade loop's per-symbol cycle. It mirrors SignatureCycle's
// SAFETY wiring exactly — the LLM only chooses go/side/TP/SL; every submitted Signal flows through
// `Submit = ExecuteOrder.OnSignal`, which applies the risk Gate (emergency_stop / daily-loss /
// no-nanpin / per-trade cap / spread / qty) and places broker-side OCO. The LLM has strictly LESS
// authority than a human manual trade (AllowOverride=false downstream), and qty is forced to config.
type LLMDecisionCycle struct {
	Symbol   string
	Pip      float64
	Quantity int // forced order size (= bot_config.llm_decision.quantity, default 1000)

	MaxHoldMinutes      int
	RatchetArmPips      float64
	RatchetGivebackPips float64
	MaxSpreadPips       float64 // pre-submit defer (0 = no cap); Gate is the backstop
	MaxConcurrent       int     // same-symbol same-side cap (0/1 = no nanpin)

	// HTFTrendVetoPips drives the ① MTF directional veto: an entry AGAINST the
	// last day's move (counter-trend BUY into a falling day / SELL into a rising day) is
	// refused. 0 = OFF. Applies to ALL pairs (the playbook lanes contain no counter-trend
	// entry). Judged on Summary24h.ChangePips — the same yardstick the playbook and
	// prompt advertise — not a 1h-close slope.
	HTFTrendVetoPips float64
	// HTFTrendVetoExempt{Sell,Buy}Rpos waive the ① veto for a POSITIONED counter-trend
	// entry: without it the veto would structurally block the discipline's
	// own "やる" patterns — a 戻りSELL from the top of the trailing 24h range (rpos ≥ Sell
	// bound) and a genuine
	// pullback BUY (rpos ≤ Buy bound). A counter-trend entry WITHOUT location (mid-range) is
	// still vetoed. 0 = no exemption. The exemption needs a
	// trustworthy 24h window: with no summary/candles it fail-CLOSES (veto stands) — only
	// the veto itself fails open.
	HTFTrendVetoExemptSellRpos float64
	HTFTrendVetoExemptBuyRpos  float64

	// ExcludeHoursJST cedes these JST hours-of-day on this symbol to a deterministic
	// strategy (e.g. exhaustion_fade owns USD_JPY in JST {4,10,11}) so the hourly LLM
	// loop and the per-tick engine never compete for the single position slot. Empty =
	// no exclusion. Set per-symbol from bot_config.llm_decision.exclude_hours_jst.
	ExcludeHoursJST []int

	// NoEntryHoursJST (session guard): ALL-side new-entry ban in
	// these JST hours — unlike ExcludeHoursJST nothing else owns them; the hours are simply
	// not tradeable (早朝 05:45 JST 前後にスプレッドが一桁 pips 以上に跳ね、その手前で建てた
	// 玉は SL を機械的に刈られやすい)。The cycle skips BEFORE the
	// LLM call (saves the API spend; event_retrigger re-runs land here too). The armed-plan
	// fire path enforces the same hours (ArmedFire.NoEntryHoursJST). Empty = OFF.
	NoEntryHoursJST []int

	// Per-currency entry discipline.
	// A trade-history analysis (with 1m candles) found the
	// losses concentrated in three behaviours; these vetoes stop them in CODE, because a prompt
	// rule alone is not reliable (an LLM can call a 15-pip dip off the high a "押し目"). All fail OPEN on
	// missing data and journal their stage, so a data gap can never silently halt trading.
	//
	// NightBuyVetoHoursJST refuses BUY entries in these JST hours (SELL passes — downside
	// continues through the night). JST 0-5 (0:00-5:59) BUY was consistently negative in the
	// trade-history analysis. Empty = OFF.
	NightBuyVetoHoursJST []int
	// MaxRangePos24hBuy refuses a BUY when Summary24h.RangePositionPct exceeds it (price already
	// at the top of the trailing 24h range = a high-chase, negative in the trade-history
	// analysis on JPY crosses). 0 = OFF. Per-pair from bot_config.
	MaxRangePos24hBuy float64
	// MinRangePos24hSell refuses a SELL when Summary24h.RangePositionPct is below it — banned only
	// on pairs that rebound off lows (e.g. GBP_USD). Keep 0 (OFF) on pairs whose lanes
	// include sell-low continuation (e.g. USD_JPY [L1]). 0 = OFF.
	MinRangePos24hSell float64

	// Playbook vetoes (the lane rulebook's HARD bans, enforced in code).
	//
	// ExhaustionVetoPips refuses an entry in the SAME direction the 24h window has ALREADY
	// travelled ≥ this many pips (Summary24h.ChangePips): chasing a spent move. Entries opened
	// after the day had already moved very far tended to lose, while fresh-move entries did
	// not. Counter-direction entries are the
	// htf veto's job. 0 = OFF; missing 24h window fails OPEN.
	ExhaustionVetoPips float64
	// SpikeVetoPips15m refuses ANY entry while |Summary15m.ChangePips| ≥ this: the price is
	// mid-spike, where both chasing it and knife-catching it are poor entries. The next cycle
	// re-evaluates on a calm window. 0 = OFF; missing 15m window fails OPEN.
	SpikeVetoPips15m float64
	// DailyLossStopCount stops the symbol once TODAY's closed trades
	// (06:00 JST day, via TodayClosedTrades) contain this many gross losses — the playbook's
	// 同日2敗打ち止め enforced in code, BEFORE the LLM call (deterministic + saves the API
	// spend). 0 = OFF; a trades-fetch error fails open (empty list → no stop).
	DailyLossStopCount int

	// TodayClosedTrades fetches THIS trading day's closed trades (06:00 JST boundary — the
	// wiring passes market.TradingDayStartJST as the floor). The cycle maps them into
	// summary.RecentTrades so the trimmed decision payload's today_closed_trades carries the
	// evidence for the playbook's 同日2敗打ち止め checklist box. nil = not wired; a fetch
	// error fails OPEN (empty list — a DB blip must never halt trading).
	TodayClosedTrades func(ctx context.Context) ([]port.TradeRecord, error)

	// Arms: a no_trade decision may pre-place conditional
	// plans ("if price breaks LEVEL, enter SIDE"); a deterministic watcher fires them with
	// full fire-time re-validation. The cycle only VALIDATES and STORES them:
	//   - ArmEnabled gates the whole feature (config llm_decision.arm_enabled; default off).
	//   - ArmMaxDistancePips caps |trigger − current mid| (0 → 30) — no far-fetched scenarios.
	//   - plans need a trustworthy current rate (summary CurrentRate.Bid > 0) and the lane tag
	//     matching their side ([L1]=SELL / [L4]=BUY) — the fire-time lane re-check keys off it.
	// Every VALID decision replaces the previous arms wholesale (ClearArms first); a decider
	// error keeps the old plans (they expire on their own). nil funcs = feature not wired.
	ArmEnabled         bool
	ArmMaxDistancePips float64
	StoreArms          func(plans []strategy.ArmedPlan)
	ClearArms          func()

	// EmergencyActive reports runtime/emergency_stop.flag. When it returns true the cycle
	// stops before building the summary or calling the LLM (nil → not checked here; the
	// risk Gate still rejects new entries at admission).
	EmergencyActive func() bool

	ActiveConfigID func() string                // real strategy_configs FK (empty → skip)
	Playbook       func() string                // current playbook rules text (nil → "")
	Decide         LLMDecideFunc                // the LLM brain (nil → no-trade)
	BuildSummary   func() *market.MarketSummary // market features for the prompt
	GetTicker      func(ctx context.Context) (*market.Ticker, error)
	OpenPositions  func(ctx context.Context) ([]port.PositionRecord, error) // pyramid/no-nanpin policy
	Submit         func(ctx context.Context, sig strategy.Signal, ticker *market.Ticker) error
	Now            func() time.Time
	Logger         *slog.Logger
	// Journal durably records each cycle's outcome (append-only history). Optional: nil = no-op.
	// Observability ONLY — a Record failure never aborts the cycle (see journal()).
	Journal port.LLMDecisionJournal
}

// LLMDecisionResult reports where a cycle stopped, for logging / status / tests.
type LLMDecisionResult struct {
	Stage    string // no_decider | emergency_stop | excluded_hour | no_entry_hour | daily_loss_stop | no_trade | armed | invalid_side | night_buy_veto | chase_buy_veto | sell_low_veto | exhaustion_veto | spike_veto | htf_trend_veto | wide_spread | no_active_config | max_concurrent | pyramid_not_armed | skipped | admission_rejected | submitted | decider_error
	Decision strategy.LLMTradeDecision
	// RejectReason carries the risk gate's refusal for stage=admission_rejected
	// (e.g. "loss_in_window 2081 >= cap 2000") so journal/status show WHY.
	RejectReason string
	// Summary is the decision-time market snapshot, captured so the journal can
	// record the objective context alongside the LLM's reasoning. nil before the
	// summary is built (e.g. excluded_hour / no_decider).
	Summary *market.MarketSummary
	// HTFVetoExempt: this outcome passed the ① MTF veto only via the positioned
	// per-currency exemption — journalled so exempt trades stay auditable as a cohort.
	HTFVetoExempt bool
	// Arms is the compact summary of the plans this cycle placed (stage=armed).
	Arms string
}

// htfVetoDisciplineExempt reports whether a counter-trend entry that the ① MTF veto
// would refuse is POSITIONED per the per-currency discipline and therefore exempt: a SELL from the top of the trailing 24h range
// (rpos ≥ HTFTrendVetoExemptSellRpos — the 戻りSELL zone) or a BUY from a
// genuine pullback (rpos ≤ HTFTrendVetoExemptBuyRpos). Unlike the vetoes, the
// exemption fail-CLOSES: no trustworthy 24h window (nil summary / 0 candles / no
// real current rate) or an unset bound (0) means no exemption and the veto stands.
// The CurrentRate.Bid witness matters: on a ticker-fetch failure build_market_summary
// FABRICATES a neutral rpos of 0.5 even with NumCandles>0, which sits exactly on the
// inclusive BUY bound (0.5 ≤ 0.50) — only a summary built from a real ticker (Bid>0;
// CurrentRate stays zero-valued otherwise) proves the rpos was actually measured.
func (c *LLMDecisionCycle) htfVetoDisciplineExempt(summary *market.MarketSummary, side order.Side) bool {
	if summary == nil || summary.Summary24h.NumCandles <= 0 || summary.CurrentRate.Bid <= 0 {
		return false
	}
	rpos := summary.Summary24h.RangePositionPct
	switch side {
	case order.SideSell:
		return c.HTFTrendVetoExemptSellRpos > 0 && rpos >= c.HTFTrendVetoExemptSellRpos
	case order.SideBuy:
		return c.HTFTrendVetoExemptBuyRpos > 0 && rpos <= c.HTFTrendVetoExemptBuyRpos
	}
	return false
}

func (c *LLMDecisionCycle) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *LLMDecisionCycle) log(msg string, args ...any) {
	if c.Logger != nil {
		c.Logger.Info(msg, args...)
	}
}

// journal appends this cycle's final outcome to the durable decision journal. Observability only:
// a nil journal or a Record error never affects trading — we just log and move on. Invoked via a
// single defer so every return path (no_trade / submitted / errors) is captured exactly once.
func (c *LLMDecisionCycle) journal(result *LLMDecisionResult, runErr *error) {
	if c.Journal == nil {
		return
	}
	e := port.LLMDecisionLogEntry{
		Time:   c.now(),
		Symbol: c.Symbol,
		Event:  "cycle",
		Stage:  result.Stage,
		Go:     result.Decision.Go,
		Side:   string(result.Decision.Side),
		TPPips: result.Decision.TPPips,
		SLPips: result.Decision.SLPips,
		Reason: result.Decision.Reason,
	}
	if runErr != nil && *runErr != nil {
		e.Error = (*runErr).Error()
	}
	e.RejectReason = result.RejectReason
	// Pair the reasoning with the objective decision-time context (③ richer
	// per-trade knowledge): price/spread/ATR(5m)/ATR(1h)/1h-trend.
	if s := result.Summary; s != nil {
		e.Price = s.CurrentRate.Bid
		e.SpreadPips = s.CurrentRate.SpreadPips
		e.ATR5mPips = s.Summary5m.ATRPips
		e.ATR1hPips = s.Summary1h.ATRPips
		e.Trend1h = s.Summary1h.TrendDirection
		// Archive rpos only when it was actually measured — on a ticker-fetch failure
		// build_market_summary fabricates a neutral 0.5 (see htfVetoDisciplineExempt).
		if s.Summary24h.NumCandles > 0 && s.CurrentRate.Bid > 0 {
			e.RangePos24h = s.Summary24h.RangePositionPct
		}
	}
	e.HTFVetoExempt = result.HTFVetoExempt
	e.Arms = result.Arms
	if rerr := c.Journal.Record(e); rerr != nil {
		c.log("llm_journal_record_failed", "symbol", c.Symbol, "err", rerr)
	}
}

// Run executes one autonomous LLM decision cycle. A non-nil error is a transport/IO failure
// (decider / ticker / submit) — never a "no trade" outcome (the normal, common result via Stage).
func (c *LLMDecisionCycle) Run(ctx context.Context) (result LLMDecisionResult, err error) {
	defer c.journal(&result, &err)
	// summary is captured by res so EVERY outcome (no_trade / submitted / error)
	// records the decision-time market context in the journal, not just the prose.
	var summary *market.MarketSummary
	htfVetoExempt := false // set iff the ① veto fired and the positioned exemption waived it
	res := func(stage string, d strategy.LLMTradeDecision) LLMDecisionResult {
		return LLMDecisionResult{Stage: stage, Decision: d, Summary: summary, HTFVetoExempt: htfVetoExempt}
	}
	if c.Decide == nil {
		return res("no_decider", strategy.LLMTradeDecision{}), nil
	}
	if c.EmergencyActive != nil && c.EmergencyActive() {
		c.log("llm_emergency_stop", "symbol", c.Symbol)
		return res("emergency_stop", strategy.LLMTradeDecision{}), nil
	}

	// Hour partition: cede these JST hours on this symbol to a deterministic
	// strategy (e.g. exhaustion_fade on USD_JPY in JST {4,10,11}) so the two entry
	// paths never compete for the single position slot. JST = UTC+9 (no DST).
	// Skip BEFORE BuildSummary/Decide to avoid a wasted LLM call.
	if jstHourIn(c.now(), c.ExcludeHoursJST) {
		c.log("llm_excluded_hour", "symbol", c.Symbol)
		return res("excluded_hour", strategy.LLMTradeDecision{}), nil
	}

	// Session guard: no new entries (any side) in these JST hours — skip the
	// whole decision (and the LLM call) like excluded_hour, but journal a distinct
	// stage so the journal 集計 can tell "ceded to another strategy" from "banned band".
	if jstHourIn(c.now(), c.NoEntryHoursJST) {
		c.log("llm_no_entry_hour", "symbol", c.Symbol)
		return res("no_entry_hour", strategy.LLMTradeDecision{}), nil
	}

	if c.BuildSummary != nil {
		summary = c.BuildSummary()
	}
	// Feed TODAY's closed trades (06:00 JST trading day) to the decider — the evidence for
	// the playbook's 同日2敗打ち止め box. Fail-open: a fetch error means an empty list, never a halt.
	if summary != nil && c.TodayClosedTrades != nil {
		trs, terr := c.TodayClosedTrades(ctx)
		if terr != nil {
			c.log("llm_today_trades_fetch_failed", "symbol", c.Symbol, "err", terr)
			trs = nil
		}
		today := make([]market.RecentTrade, 0, len(trs))
		for _, tr := range trs {
			today = append(today, market.RecentTrade{
				Side:           tr.Side,
				ProfitLossPips: tr.ProfitLossPips,
				ProfitLossJPY:  tr.ProfitLossJPY,
				CloseReason:    tr.CloseReason,
			})
		}
		summary.RecentTrades = today
	}
	// Daily-loss stop: the playbook's 同日2敗打ち止め, enforced in code BEFORE the LLM
	// call. Gross-loss count over today's closed trades; fail-open (a fetch error above left
	// the list empty → losses 0 → no stop).
	if c.DailyLossStopCount > 0 && summary != nil {
		lossesToday := 0
		for _, tr := range summary.RecentTrades {
			if tr.ProfitLossJPY < 0 {
				lossesToday++
			}
		}
		if lossesToday >= c.DailyLossStopCount {
			c.log("llm_daily_loss_stop", "symbol", c.Symbol, "losses_today", lossesToday, "cap", c.DailyLossStopCount)
			return res("daily_loss_stop", strategy.LLMTradeDecision{}), nil
		}
	}
	playbook := ""
	if c.Playbook != nil {
		playbook = c.Playbook()
	}

	d, err := c.Decide(ctx, summary, playbook)
	if err != nil {
		// Transport error → fail safe to no-trade, surface for retry/log. The PREVIOUS hour's
		// armed plans are kept (they expire on their own) — a CLI blip must not disarm a
		// deliberately placed scenario.
		return res("decider_error", strategy.LLMTradeDecision{}), fmt.Errorf("llm decide: %w", err)
	}
	// Every VALID decision supersedes last hour's conditional plans wholesale — whatever
	// the outcome below (enter / arm / no_trade / veto), stale scenarios never outlive the
	// decision that should have replaced them.
	if c.ClearArms != nil {
		c.ClearArms()
	}
	if !d.Go {
		if plans := c.acceptedArms(d.Arms, summary); len(plans) > 0 {
			c.StoreArms(plans)
			r := res("armed", d)
			r.Arms = armsSummary(plans)
			c.log("llm_armed", "symbol", c.Symbol, "arms", r.Arms)
			return r, nil
		}
		c.log("llm_no_trade", "symbol", c.Symbol, "reason", d.Reason)
		return res("no_trade", d), nil
	}
	if !d.Side.Valid() {
		// Defensive: the parser already guards this; never trade on an invalid side.
		c.log("llm_invalid_side", "symbol", c.Symbol, "side", string(d.Side))
		return res("invalid_side", d), nil
	}

	// Per-currency entry discipline — the three
	// veto rules run BEFORE the (network) ticker/candle fetches: they are pure checks on the
	// clock and the already-built summary. Each is independently config-gated and fail-open.

	// Night-BUY veto: JST 0-5 (0:00-5:59) BUY was consistently negative in the trade-history
	// analysis (overnight chases). SELL passes — downside continues through the night.
	if d.Side == order.SideBuy && jstHourIn(c.now(), c.NightBuyVetoHoursJST) {
		c.log("llm_night_buy_veto", "symbol", c.Symbol)
		return res("night_buy_veto", d), nil
	}

	// Range-position vetoes need a trustworthy 24h window; without one they fail open
	// (RangePositionPct defaults to 0.5 on empty data, which must not trip either bound).
	if summary != nil && summary.Summary24h.NumCandles > 0 {
		rpos := summary.Summary24h.RangePositionPct
		// High-chase BUY veto: price already at the top of the trailing 24h range.
		if d.Side == order.SideBuy && c.MaxRangePos24hBuy > 0 && rpos > c.MaxRangePos24hBuy {
			c.log("llm_chase_buy_veto", "symbol", c.Symbol, "range_pos_24h", rpos, "cap", c.MaxRangePos24hBuy)
			return res("chase_buy_veto", d), nil
		}
		// Sell-low veto: chasing a SELL into the bottom of the range, on pairs that rebound.
		if d.Side == order.SideSell && c.MinRangePos24hSell > 0 && rpos < c.MinRangePos24hSell {
			c.log("llm_sell_low_veto", "symbol", c.Symbol, "range_pos_24h", rpos, "floor", c.MinRangePos24hSell)
			return res("sell_low_veto", d), nil
		}
		// Exhaustion veto: the 24h window has ALREADY travelled ≥ the cap in the entry's
		// direction — a chase into a spent move. Same-direction
		// only; counter-direction entries are the ① htf veto's job below.
		if chg := summary.Summary24h.ChangePips; c.ExhaustionVetoPips > 0 &&
			((d.Side == order.SideSell && chg <= -c.ExhaustionVetoPips) ||
				(d.Side == order.SideBuy && chg >= c.ExhaustionVetoPips)) {
			c.log("llm_exhaustion_veto", "symbol", c.Symbol, "side", string(d.Side),
				"change_pips_24h", chg, "cap", c.ExhaustionVetoPips)
			return res("exhaustion_veto", d), nil
		}
	}
	// Spike-cooldown veto: the last 15 minutes moved ≥ the cap in EITHER direction — the
	// price is mid-spike, so neither chase it nor knife-catch it; the next cycle re-evaluates.
	if summary != nil && summary.Summary15m.NumCandles > 0 && c.SpikeVetoPips15m > 0 {
		if chg := summary.Summary15m.ChangePips; chg >= c.SpikeVetoPips15m || chg <= -c.SpikeVetoPips15m {
			c.log("llm_spike_veto", "symbol", c.Symbol, "side", string(d.Side),
				"change_pips_15m", chg, "cap", c.SpikeVetoPips15m)
			return res("spike_veto", d), nil
		}
	}

	// ① MTF directional veto: refuse an entry AGAINST the last
	// day's move — counter-trend BUY into a falling day / SELL into a rising day (where the
	// trade-history analysis found losses concentrated). It is judged
	// on the SAME yardstick the playbook advertises — summary_24h.change_pips — instead of
	// a 1h-close slope, so the LLM's evidence and the code's trigger can never drift.
	// ALL pairs (the playbook lanes contain no counter-trend entry). A move-ALIGNED entry passes;
	// |change| below the threshold passes both ways. 0 = OFF; no 24h window → fail-open.
	if c.HTFTrendVetoPips > 0 && summary != nil && summary.Summary24h.NumCandles > 0 {
		chg := summary.Summary24h.ChangePips
		if (d.Side == order.SideBuy && chg <= -c.HTFTrendVetoPips) ||
			(d.Side == order.SideSell && chg >= c.HTFTrendVetoPips) {
			if c.htfVetoDisciplineExempt(summary, d.Side) {
				// Positioned per the per-currency discipline (SELL from the 24h top / BUY from a
				// genuine pullback). The live config sets no exemption bounds; the mechanism
				// stays for per-currency lane configs.
				htfVetoExempt = true
				c.log("llm_htf_trend_veto_exempt", "symbol", c.Symbol, "side", string(d.Side),
					"range_pos_24h", summary.Summary24h.RangePositionPct)
			} else {
				c.log("llm_htf_trend_veto", "symbol", c.Symbol, "side", string(d.Side),
					"change_pips_24h", chg, "cap", c.HTFTrendVetoPips)
				return res("htf_trend_veto", d), nil
			}
		}
	}

	ticker, err := c.GetTicker(ctx)
	if err != nil {
		return res("submitted", d), fmt.Errorf("ticker: %w", err)
	}
	// Wide-spread guard (pre-submit defer; the next cycle retries once liquidity normalizes).
	if c.MaxSpreadPips > 0 && c.Pip > 0 {
		if spreadPips := (ticker.Ask - ticker.Bid) / c.Pip; spreadPips > c.MaxSpreadPips {
			c.log("llm_wide_spread", "symbol", c.Symbol, "spread_pips", spreadPips, "cap", c.MaxSpreadPips)
			return res("wide_spread", d), nil
		}
	}
	entry := ticker.Ask
	if d.Side == order.SideSell {
		entry = ticker.Bid
	}

	// Resolve the position's FK to a REAL active config_id (without it the position INSERT FK-fails →
	// orphan with no exit management). Skip entirely — never place an order we cannot record.
	cfgID := ""
	if c.ActiveConfigID != nil {
		cfgID = c.ActiveConfigID()
	}
	if cfgID == "" {
		c.log("llm_no_active_config", "symbol", c.Symbol)
		return res("no_active_config", d), nil
	}

	// Concurrency / pyramid policy (identical doctrine to SignatureCycle): default cap 1 (no nanpin);
	// a 2nd same-side entry only when every existing same-side position is already ratchet-ARMED.
	maxConc := c.MaxConcurrent
	if maxConc < 1 {
		maxConc = 1
	}
	sameSide := 0
	if c.OpenPositions != nil {
		opens, oerr := c.OpenPositions(ctx)
		if oerr != nil {
			return res("submitted", d), fmt.Errorf("open positions: %w", oerr)
		}
		for _, p := range opens {
			if p.Side != string(d.Side) {
				continue
			}
			sameSide++
			if !p.RatchetArmed {
				c.log("llm_pyramid_not_armed", "symbol", c.Symbol, "open_same_side", sameSide)
				return res("pyramid_not_armed", d), nil
			}
		}
	}
	if sameSide >= maxConc {
		c.log("llm_max_concurrent", "symbol", c.Symbol, "open_same_side", sameSide, "cap", maxConc)
		return res("max_concurrent", d), nil
	}

	sig := strategy.BuildLLMSignal(d.Side, entry, d.TPPips, d.SLPips, c.Quantity, c.MaxHoldMinutes,
		c.RatchetArmPips, c.RatchetGivebackPips, config.StrategyLLMDecision, c.now())
	sig.ConfigID = cfgID
	sig.MaxConcurrent = maxConc
	if !sig.IsEntry() {
		c.log("llm_signal_not_entry", "symbol", c.Symbol, "reason", sig.Reason)
		return res("skipped", d), nil
	}
	if err := c.Submit(ctx, sig, ticker); err != nil {
		var rej *AdmissionRejectedError
		if errors.As(err, &rej) {
			// Risk-gate refusal = a NORMAL outcome. Journal it truthfully (recording it as
			// "submitted" would hide a stuck risk cap behind apparent successful submits).
			c.log("llm_admission_rejected", "symbol", c.Symbol, "side", string(d.Side), "reason", rej.Reason)
			r := res("admission_rejected", d)
			r.RejectReason = rej.Reason
			return r, nil
		}
		return res("submitted", d), fmt.Errorf("submit: %w", err)
	}
	c.log("llm_submitted", "symbol", c.Symbol, "side", string(d.Side),
		"tp_pips", sig.TakeProfitPips, "sl_pips", sig.StopLossPips, "qty", sig.Quantity)
	return res("submitted", d), nil
}

// laneTagOf extracts the playbook lane tag a plan's reason carries ("[L1]" / "[L4]"), or "".
func laneTagOf(reason string) string {
	if strings.Contains(reason, "[L1]") {
		return "[L1]"
	}
	if strings.Contains(reason, "[L4]") {
		return "[L4]"
	}
	return ""
}

// acceptedArms applies the CYCLE-side validation to the parser-normalized plans:
// feature wired+enabled, a trustworthy current rate (no arming on fabricated data), trigger
// within ArmMaxDistancePips of the mid, and the lane tag matching the side (L1=SELL / L4=BUY —
// the fire-time lane re-check keys off the tag). Invalid plans are dropped individually.
func (c *LLMDecisionCycle) acceptedArms(plans []strategy.ArmedPlan, summary *market.MarketSummary) []strategy.ArmedPlan {
	if !c.ArmEnabled || c.StoreArms == nil || len(plans) == 0 || c.Pip <= 0 {
		return nil
	}
	if summary == nil || summary.CurrentRate.Bid <= 0 || summary.CurrentRate.Ask <= 0 {
		return nil // no trustworthy live rate — never arm on fabricated data
	}
	mid := (summary.CurrentRate.Bid + summary.CurrentRate.Ask) / 2
	maxDist := c.ArmMaxDistancePips
	if maxDist <= 0 {
		maxDist = 30
	}
	out := make([]strategy.ArmedPlan, 0, len(plans))
	for _, p := range plans {
		if dist := (p.TriggerPrice - mid) / c.Pip; dist > maxDist || dist < -maxDist {
			c.log("llm_arm_dropped_distance", "symbol", c.Symbol, "trigger", p.TriggerPrice, "mid", mid)
			continue
		}
		tag := laneTagOf(p.Reason)
		if (p.Side == order.SideSell && tag != "[L1]") || (p.Side == order.SideBuy && tag != "[L4]") {
			c.log("llm_arm_dropped_lane_tag", "symbol", c.Symbol, "side", string(p.Side), "reason", p.Reason)
			continue
		}
		out = append(out, p)
	}
	return out
}

// armsSummary renders plans compactly for the journal/status ("SELL@161.950↓ TP30/SL25 [L1]").
func armsSummary(plans []strategy.ArmedPlan) string {
	parts := make([]string, 0, len(plans))
	for _, p := range plans {
		arrow := "↓"
		if p.BreakAbove {
			arrow = "↑"
		}
		parts = append(parts, fmt.Sprintf("%s@%.3f%s TP%.0f/SL%.0f %s",
			string(p.Side), p.TriggerPrice, arrow, p.TPPips, p.SLPips, laneTagOf(p.Reason)))
	}
	return strings.Join(parts, " | ")
}
