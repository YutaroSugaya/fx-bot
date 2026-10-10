package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// recordingJournal captures the entries a cycle writes, for assertions.
type recordingJournal struct{ entries []port.LLMDecisionLogEntry }

func (r *recordingJournal) Record(e port.LLMDecisionLogEntry) error {
	r.entries = append(r.entries, e)
	return nil
}

func baseLLMCycle(decide LLMDecideFunc, submitted *[]strategy.Signal) *LLMDecisionCycle {
	return &LLMDecisionCycle{
		Symbol: "USD_JPY", Pip: 0.01, Quantity: 1000,
		MaxHoldMinutes: 1440, MaxSpreadPips: 3.0, MaxConcurrent: 1,
		ActiveConfigID: func() string { return "trend-v4-usdjpy" },
		Playbook:       func() string { return "rules" },
		Decide:         decide,
		GetTicker:      func(_ context.Context) (*market.Ticker, error) { return &market.Ticker{Ask: 161.20, Bid: 161.19}, nil },
		OpenPositions:  func(_ context.Context) ([]port.PositionRecord, error) { return nil, nil },
		Submit: func(_ context.Context, sig strategy.Signal, _ *market.Ticker) error {
			*submitted = append(*submitted, sig)
			return nil
		},
	}
}

func TestLLMDecisionCycle_NoDecider_NoSubmit(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(nil, &sub)
	r, err := c.Run(context.Background())
	if err != nil || r.Stage != "no_decider" || len(sub) != 0 {
		t.Fatalf("stage=%q err=%v submits=%d", r.Stage, err, len(sub))
	}
}

func TestLLMDecisionCycle_NoTrade_NoSubmit(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		return strategy.LLMTradeDecision{Go: false}, nil
	}, &sub)
	r, _ := c.Run(context.Background())
	if r.Stage != "no_trade" || len(sub) != 0 {
		t.Fatalf("stage=%q submits=%d", r.Stage, len(sub))
	}
}

// richer per-trade knowledge: every journalled cycle must pair the LLM's prose
// reason with the OBJECTIVE decision-time market context (price/spread/ATR/trend),
// so the knowledge base is analyzable ("this kind of setup → this outcome").
func TestLLMDecisionCycle_JournalsMarketContext(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		return strategy.LLMTradeDecision{Go: false, Reason: "range, no edge"}, nil
	}, &sub)
	c.BuildSummary = func() *market.MarketSummary {
		return &market.MarketSummary{
			CurrentRate: market.CurrentRate{Bid: 149.95, Ask: 149.96, SpreadPips: 1.0},
			Summary5m:   market.WindowSummary{ATRPips: 1.4},
			Summary1h:   market.WindowSummary{ATRPips: 5.6, TrendDirection: "up"},
		}
	}
	j := &recordingJournal{}
	c.Journal = j
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(j.entries) != 1 {
		t.Fatalf("want 1 journal entry, got %d", len(j.entries))
	}
	e := j.entries[0]
	if e.Price != 149.95 || e.SpreadPips != 1.0 || e.ATR5mPips != 1.4 || e.ATR1hPips != 5.6 || e.Trend1h != "up" {
		t.Errorf("journal must capture decision-time market context, got %+v", e)
	}
}

// Hour partition: the LLM loop cedes specific JST hours on a symbol
// to a deterministic strategy (exhaustion_fade owns USD_JPY in JST {4,10,11}) so
// the two entry paths never overlap. In an excluded hour the cycle must skip
// BEFORE calling the LLM (no wasted decide), and trade normally otherwise.
func TestLLMDecisionCycle_ExcludedHourJST_SkipsBeforeDeciding(t *testing.T) {
	var sub []strategy.Signal
	decided := false
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		decided = true
		return strategy.LLMTradeDecision{Go: true, Side: order.SideBuy, TPPips: 10, SLPips: 10}, nil
	}, &sub)
	c.ExcludeHoursJST = []int{4, 10, 11}

	// 01:30 UTC = 10:30 JST → excluded → skip before deciding.
	c.Now = func() time.Time { return time.Date(2026, 6, 24, 1, 30, 0, 0, time.UTC) }
	r, _ := c.Run(context.Background())
	if r.Stage != "excluded_hour" || len(sub) != 0 || decided {
		t.Fatalf("excluded hour must skip pre-decide: stage=%q submits=%d decided=%v", r.Stage, len(sub), decided)
	}

	// 00:30 UTC = 09:30 JST → NOT excluded → decides and submits.
	sub, decided = nil, false
	c.Now = func() time.Time { return time.Date(2026, 6, 24, 0, 30, 0, 0, time.UTC) }
	r, _ = c.Run(context.Background())
	if !decided || len(sub) != 1 {
		t.Fatalf("non-excluded hour must trade: decided=%v submits=%d stage=%q", decided, len(sub), r.Stage)
	}
}

func TestLLMDecisionCycle_DeciderError_NoSubmit(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		return strategy.LLMTradeDecision{}, errors.New("cli boom")
	}, &sub)
	r, err := c.Run(context.Background())
	if err == nil || r.Stage != "decider_error" || len(sub) != 0 {
		t.Fatalf("want decider_error+err+no submit; stage=%q err=%v submits=%d", r.Stage, err, len(sub))
	}
}

func TestLLMDecisionCycle_ValidBuy_SubmitsQty1000(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		return strategy.LLMTradeDecision{Go: true, Side: order.SideBuy, TPPips: 30, SLPips: 15, Reason: "trend"}, nil
	}, &sub)
	r, err := c.Run(context.Background())
	if err != nil || r.Stage != "submitted" || len(sub) != 1 {
		t.Fatalf("want one submit; stage=%q err=%v submits=%d", r.Stage, err, len(sub))
	}
	s := sub[0]
	if s.Side != order.SideBuy || s.Quantity != 1000 || s.TakeProfitPips != 30 || s.StopLossPips != 15 {
		t.Errorf("signal: side=%v qty=%d tp=%v sl=%v", s.Side, s.Quantity, s.TakeProfitPips, s.StopLossPips)
	}
	if s.ConfigID != "trend-v4-usdjpy" || s.StrategyName != "llm_decision" {
		t.Errorf("cfgID=%q name=%q", s.ConfigID, s.StrategyName)
	}
	if s.EntryPrice != 161.20 { // BUY enters at ask
		t.Errorf("entry should be ask 161.20, got %v", s.EntryPrice)
	}
}

func TestLLMDecisionCycle_WideSpread_NoSubmit(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		return strategy.LLMTradeDecision{Go: true, Side: order.SideBuy, TPPips: 30, SLPips: 15}, nil
	}, &sub)
	c.GetTicker = func(_ context.Context) (*market.Ticker, error) { return &market.Ticker{Ask: 161.30, Bid: 161.19}, nil } // 11 pip spread > 3
	r, _ := c.Run(context.Background())
	if r.Stage != "wide_spread" || len(sub) != 0 {
		t.Fatalf("stage=%q submits=%d", r.Stage, len(sub))
	}
}

func TestLLMDecisionCycle_NoActiveConfig_NoSubmit(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		return strategy.LLMTradeDecision{Go: true, Side: order.SideSell, TPPips: 25, SLPips: 18}, nil
	}, &sub)
	c.ActiveConfigID = func() string { return "" }
	r, _ := c.Run(context.Background())
	if r.Stage != "no_active_config" || len(sub) != 0 {
		t.Fatalf("stage=%q submits=%d", r.Stage, len(sub))
	}
}

// Observability: a SUBMITTED cycle writes exactly one "cycle" journal entry carrying the full
// outcome (stage + side + TP/SL + reason + the cycle clock), so the operator can review what the
// loop did over time instead of only the latest overwritten snapshot.
func TestLLMDecisionCycle_JournalsSubmittedOutcome(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		return strategy.LLMTradeDecision{Go: true, Side: order.SideBuy, TPPips: 30, SLPips: 15, Reason: "trend"}, nil
	}, &sub)
	j := &recordingJournal{}
	c.Journal = j
	at := time.Date(2026, 6, 23, 2, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return at }

	if _, err := c.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(j.entries) != 1 {
		t.Fatalf("want exactly one journal entry per cycle, got %d", len(j.entries))
	}
	e := j.entries[0]
	if e.Event != "cycle" || e.Symbol != "USD_JPY" || e.Stage != "submitted" || !e.Go ||
		e.Side != "BUY" || e.TPPips != 30 || e.SLPips != 15 || e.Reason != "trend" || !e.Time.Equal(at) {
		t.Errorf("journal entry wrong: %+v", e)
	}
}

// A parse failure surfaces as a no_trade whose reason is the parse error CODE. Journaling that
// reason is exactly what lets the operator tell a HEALTHY no_trade from a SILENT parse failure
// (otherwise a parser regression can go unnoticed for days).
func TestLLMDecisionCycle_JournalsNoTradeReason(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		return strategy.LLMTradeDecision{Go: false, Reason: "yaml_unmarshal_error"}, nil
	}, &sub)
	j := &recordingJournal{}
	c.Journal = j

	if _, err := c.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(j.entries) != 1 || j.entries[0].Stage != "no_trade" ||
		j.entries[0].Reason != "yaml_unmarshal_error" || j.entries[0].Go {
		t.Fatalf("want one no_trade entry carrying the parse reason, got %+v", j.entries)
	}
}

// A transport error still journals exactly one entry (decider_error) carrying the error text, so
// a recurring API/CLI failure is visible in the history, not just on a transient stdout line.
func TestLLMDecisionCycle_JournalsDeciderErrorWithText(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		return strategy.LLMTradeDecision{}, errors.New("cli boom")
	}, &sub)
	j := &recordingJournal{}
	c.Journal = j

	if _, err := c.Run(context.Background()); err == nil {
		t.Fatal("want a transport error")
	}
	if len(j.entries) != 1 || j.entries[0].Stage != "decider_error" ||
		!strings.Contains(j.entries[0].Error, "cli boom") {
		t.Fatalf("want one decider_error entry carrying err text, got %+v", j.entries)
	}
}

// MTF directional veto: the LLM loop
// must NOT enter AGAINST the last day's move — a counter-trend BUY into a falling day /
// SELL into a rising day is where losses concentrate.
// It is judged on the SAME yardstick the playbook advertises: summary_24h.change_pips
// (was: a 1h-close slope, which could drift from the advertised rule across weekend gaps).
// ALL pairs are in scope (unified lanes); OFF when HTFTrendVetoPips == 0.
func TestLLMDecisionCycle_HTFTrendVeto(t *testing.T) {
	cases := []struct {
		name      string
		symbol    string
		side      order.Side
		vetoPips  float64
		chg24     float64
		wantStage string
	}{
		{"BUY into a −30pip day → veto", "EUR_JPY", order.SideBuy, 20, -30, "htf_trend_veto"},
		{"SELL into a +30pip day → veto", "GBP_JPY", order.SideSell, 20, 30, "htf_trend_veto"},
		{"BUY into a +30pip day → submitted (aligned)", "EUR_JPY", order.SideBuy, 20, 30, "submitted"},
		{"BUY at −10 (below threshold 20) → submitted", "EUR_JPY", order.SideBuy, 20, -10, "submitted"},
		// The lanes are unified across ALL pairs — USD-quote pairs are covered too.
		{"non-JPY BUY into a −30pip day → veto (all pairs)", "EUR_USD", order.SideBuy, 20, -30, "htf_trend_veto"},
		{"non-JPY SELL into a −30pip day → submitted (aligned)", "EUR_USD", order.SideSell, 20, -30, "submitted"},
		{"veto disabled (0) → submitted even counter-trend", "EUR_JPY", order.SideBuy, 0, -30, "submitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sub []strategy.Signal
			c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
				return strategy.LLMTradeDecision{Go: true, Side: tc.side, TPPips: 30, SLPips: 25, Reason: "x"}, nil
			}, &sub)
			c.Symbol = tc.symbol
			c.HTFTrendVetoPips = tc.vetoPips
			c.BuildSummary = summaryChange(tc.chg24, 0, 200)
			j := &recordingJournal{}
			c.Journal = j
			r, err := c.Run(context.Background())
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if r.Stage != tc.wantStage {
				t.Fatalf("stage=%q want %q", r.Stage, tc.wantStage)
			}
			wantSubmits := 0
			if tc.wantStage == "submitted" {
				wantSubmits = 1
			}
			if len(sub) != wantSubmits {
				t.Fatalf("submits=%d want %d", len(sub), wantSubmits)
			}
			// Every outcome — including a veto — is journalled exactly once for audit.
			if len(j.entries) != 1 || j.entries[0].Stage != tc.wantStage {
				t.Fatalf("journal must record one %q entry, got %+v", tc.wantStage, j.entries)
			}
		})
	}
}

// veto must FAIL-OPEN: with no trustworthy 24h window (no summary at all, or
// zero candles behind it) the cycle must NOT block — it proceeds to the normal risk Gate +
// broker OCO (a missing-data veto would silently halt all trading). Counter-trend BUY here would be vetoed IF the window were measurable.
func TestLLMDecisionCycle_HTFTrendVeto_FailsOpen(t *testing.T) {
	cases := []struct {
		name    string
		summary func() *market.MarketSummary
	}{
		{"no summary at all → submit", nil},
		{"24h window with zero candles → submit", summaryChange(-30, 0, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sub []strategy.Signal
			c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
				return strategy.LLMTradeDecision{Go: true, Side: order.SideBuy, TPPips: 10, SLPips: 8}, nil
			}, &sub)
			c.Symbol = "EUR_JPY"
			c.HTFTrendVetoPips = 20
			c.BuildSummary = tc.summary
			r, err := c.Run(context.Background())
			if err != nil || r.Stage != "submitted" || len(sub) != 1 {
				t.Fatalf("veto must fail-open to submit; stage=%q err=%v submits=%d", r.Stage, err, len(sub))
			}
		})
	}
}

// summaryChgRpos builds a decision-time summary carrying BOTH the 24h net move (the veto
// yardstick) and a range position (the exemption's evidence), with a real current rate.
func summaryChgRpos(chg24, rpos float64, numCandles int) func() *market.MarketSummary {
	return func() *market.MarketSummary {
		return &market.MarketSummary{
			CurrentRate: market.CurrentRate{Bid: 161.19, Ask: 161.20, SpreadPips: 1.0},
			Summary24h:  market.WindowSummary{ChangePips: chg24, RangePositionPct: rpos, NumCandles: numCandles},
		}
	}
}

// summaryChgRposNoTicker is summaryChgRpos WITHOUT a current rate — the fabricated-rpos case.
func summaryChgRposNoTicker(chg24, rpos float64, numCandles int) func() *market.MarketSummary {
	return func() *market.MarketSummary {
		return &market.MarketSummary{
			Summary24h: market.WindowSummary{ChangePips: chg24, RangePositionPct: rpos, NumCandles: numCandles},
		}
	}
}

// discipline exemption (off in the default config, but the MECHANISM stays for
// per-currency lane configs): a POSITIONED
// counter-trend entry — SELL from the top of the 24h range (rpos ≥ Sell bound) or BUY from a
// genuine pullback (rpos ≤ Buy bound) — is exempt from the veto; mid-range counter-trend is
// still refused. Config-gated (0 = no exemption = default behaviour) and requires a
// trustworthy 24h window WITH a real rate — with no data the veto stands (the exemption
// fail-closes; only the veto itself fails open).
func TestLLMDecisionCycle_HTFTrendVeto_DisciplineExempt(t *testing.T) {
	cases := []struct {
		name       string
		side       order.Side
		chg24      float64 // counter-trend vs side when it exceeds the veto threshold
		exemptSell float64
		exemptBuy  float64
		summary    func() *market.MarketSummary
		wantStage  string
	}{
		{"SELL into a rising day FROM the top (rpos≥exempt) → submitted", order.SideSell, 30, 0.60, 0, summaryChgRpos(30, 0.75, 200), "submitted"},
		{"SELL into a rising day mid-range (rpos<exempt) → veto", order.SideSell, 30, 0.60, 0, summaryChgRpos(30, 0.40, 200), "htf_trend_veto"},
		{"BUY into a falling day FROM a pullback (rpos≤exempt) → submitted", order.SideBuy, -30, 0, 0.50, summaryChgRpos(-30, 0.35, 200), "submitted"},
		{"BUY into a falling day mid-range (rpos>exempt) → veto", order.SideBuy, -30, 0, 0.50, summaryChgRpos(-30, 0.70, 200), "htf_trend_veto"},
		{"exemption OFF (0) → veto even from the top (default behaviour)", order.SideSell, 30, 0, 0, summaryChgRpos(30, 0.75, 200), "htf_trend_veto"},
		// Ticker-fetch failure: build_market_summary FABRICATES a neutral rpos of 0.5 (with
		// NumCandles>0!) when it has no mid — the fabricated 0.5 sits exactly ON the inclusive
		// BUY bound (0.5 ≤ 0.50), so without a real-rate witness a counter-trend BUY would be
		// exempted on made-up data. The exemption must treat a missing CurrentRate as "rpos is
		// not real" and keep the veto.
		{"fabricated neutral rpos (no ticker) → BUY exemption fail-closes, veto stands", order.SideBuy, -30, 0, 0.50, summaryChgRposNoTicker(-30, 0.5, 200), "htf_trend_veto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sub []strategy.Signal
			c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
				return strategy.LLMTradeDecision{Go: true, Side: tc.side, TPPips: 30, SLPips: 25, Reason: "x"}, nil
			}, &sub)
			c.Symbol = "GBP_JPY"
			c.Pip = 0.01
			c.HTFTrendVetoPips = 20
			c.HTFTrendVetoExemptSellRpos = tc.exemptSell
			c.HTFTrendVetoExemptBuyRpos = tc.exemptBuy
			c.BuildSummary = tc.summary
			c.GetTicker = func(_ context.Context) (*market.Ticker, error) {
				return &market.Ticker{Ask: 215.0 + 0.5*0.01, Bid: 215.0}, nil
			}
			j := &recordingJournal{}
			c.Journal = j
			r, err := c.Run(context.Background())
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if r.Stage != tc.wantStage {
				t.Fatalf("stage=%q want %q", r.Stage, tc.wantStage)
			}
			wantSubmits := 0
			if tc.wantStage == "submitted" {
				wantSubmits = 1
			}
			if len(sub) != wantSubmits {
				t.Fatalf("submits=%d want %d", len(sub), wantSubmits)
			}
			// An exempted trade must be identifiable in the DURABLE journal (not only in
			// slog): re-verifying exempt trades after the fact is impossible if the
			// flag lives only in rotated stdout logs.
			if len(j.entries) != 1 {
				t.Fatalf("journal entries=%d want 1", len(j.entries))
			}
			wantExempt := tc.wantStage == "submitted"
			if j.entries[0].HTFVetoExempt != wantExempt {
				t.Fatalf("journal HTFVetoExempt=%v want %v", j.entries[0].HTFVetoExempt, wantExempt)
			}
			if s := tc.summary(); s.Summary24h.NumCandles > 0 && s.CurrentRate.Bid > 0 {
				if j.entries[0].RangePos24h != s.Summary24h.RangePositionPct {
					t.Fatalf("journal RangePos24h=%v want %v", j.entries[0].RangePos24h, s.Summary24h.RangePositionPct)
				}
			}
		})
	}
}

// ratchet wiring: the LLM loop must FREEZE its configured
// trailing ratchet (arm/giveback) onto every submitted signal so OnTick can lock
// in profit on a runner that reverses — the "夜に乗った利益を吐き出す" leak. With
// arm/give left at 0 the ratchet never fires; this locks the propagation so a
// future refactor can't silently drop it.
func TestLLMDecisionCycle_SubmitsRatchetFromConfig(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		return strategy.LLMTradeDecision{Go: true, Side: order.SideBuy, TPPips: 10, SLPips: 8}, nil
	}, &sub)
	c.RatchetArmPips = 4
	c.RatchetGivebackPips = 2
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sub) != 1 {
		t.Fatalf("want 1 submit, got %d", len(sub))
	}
	if sub[0].RatchetArmPips != 4 || sub[0].RatchetGivebackPips != 2 {
		t.Errorf("signal must carry ratchet arm/give; got arm=%v give=%v",
			sub[0].RatchetArmPips, sub[0].RatchetGivebackPips)
	}
}

// Night-BUY veto: BUY entries during the configured JST night hours are refused in CODE, not
// prompt — late-night JST BUYs (0:00-5:59) tend to be thin-liquidity chases of an overnight move.
// SELL passes (downside can continue through the night). Empty hours = OFF (back-compat).
func TestLLMDecisionCycle_NightBuyVeto(t *testing.T) {
	nightHours := []int{0, 1, 2, 3, 4, 5}
	cases := []struct {
		name      string
		side      order.Side
		utc       time.Time // JST = UTC+9
		hours     []int
		wantStage string
	}{
		{"BUY at JST 02 → veto", order.SideBuy, time.Date(2026, 7, 1, 17, 5, 0, 0, time.UTC), nightHours, "night_buy_veto"},
		{"BUY at JST 05 → veto", order.SideBuy, time.Date(2026, 7, 1, 20, 30, 0, 0, time.UTC), nightHours, "night_buy_veto"},
		{"SELL at JST 02 → submitted (night SELL allowed)", order.SideSell, time.Date(2026, 7, 1, 17, 5, 0, 0, time.UTC), nightHours, "submitted"},
		{"BUY at JST 09 → submitted (outside night)", order.SideBuy, time.Date(2026, 7, 2, 0, 5, 0, 0, time.UTC), nightHours, "submitted"},
		{"BUY at JST 02 with veto OFF → submitted", order.SideBuy, time.Date(2026, 7, 1, 17, 5, 0, 0, time.UTC), nil, "submitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sub []strategy.Signal
			c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
				return strategy.LLMTradeDecision{Go: true, Side: tc.side, TPPips: 30, SLPips: 25, Reason: "x"}, nil
			}, &sub)
			c.NightBuyVetoHoursJST = tc.hours
			c.Now = func() time.Time { return tc.utc }
			j := &recordingJournal{}
			c.Journal = j
			r, err := c.Run(context.Background())
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if r.Stage != tc.wantStage {
				t.Fatalf("stage=%q want %q", r.Stage, tc.wantStage)
			}
			wantSubmits := 0
			if tc.wantStage == "submitted" {
				wantSubmits = 1
			}
			if len(sub) != wantSubmits {
				t.Fatalf("submits=%d want %d", len(sub), wantSubmits)
			}
			// Every veto is journalled for audit (the operator watches the loop via the journal).
			if len(j.entries) != 1 || j.entries[0].Stage != tc.wantStage {
				t.Fatalf("journal must record one %q entry, got %+v", tc.wantStage, j.entries)
			}
		})
	}
}

// summaryNoTicker24h mimics build_market_summary's ticker-fetch-failure output: candles exist
// (NumCandles>0) but there was no mid, so RangePositionPct is the FABRICATED neutral and
// CurrentRate is the zero value (never populated without a real ticker).
func summaryNoTicker24h(rpos float64, numCandles int) func() *market.MarketSummary {
	return func() *market.MarketSummary {
		return &market.MarketSummary{
			Summary24h: market.WindowSummary{RangePositionPct: rpos, NumCandles: numCandles},
		}
	}
}

// summary24h builds a decision-time summary whose 24h window carries the given range position.
func summary24h(rpos float64, numCandles int) func() *market.MarketSummary {
	return func() *market.MarketSummary {
		return &market.MarketSummary{
			CurrentRate: market.CurrentRate{Bid: 161.19, Ask: 161.20, SpreadPips: 1.0},
			Summary24h:  market.WindowSummary{RangePositionPct: rpos, NumCandles: numCandles},
		}
	}
}

// High-chase BUY veto: a BUY with the price already in the top of the trailing 24h
// range is a chase into an extended move. Above the per-pair ceiling the BUY is refused in code —
// a prompt-only "buy pullbacks" rule is not reliable (the LLM can call a 15-pip dip off the high a
// "押し目").
// 0 = OFF (back-compat). Missing/empty 24h summary fails OPEN (a data gap must not halt trading).
func TestLLMDecisionCycle_ChaseBuyVeto_RangePos24h(t *testing.T) {
	cases := []struct {
		name      string
		side      order.Side
		ceiling   float64
		summary   func() *market.MarketSummary
		wantStage string
	}{
		{"BUY above ceiling → veto", order.SideBuy, 0.85, summary24h(0.92, 200), "chase_buy_veto"},
		{"BUY below ceiling → submitted", order.SideBuy, 0.85, summary24h(0.70, 200), "submitted"},
		{"SELL above ceiling → submitted (BUY-only veto)", order.SideSell, 0.85, summary24h(0.92, 200), "submitted"},
		{"veto OFF (0) → submitted", order.SideBuy, 0, summary24h(0.92, 200), "submitted"},
		{"tighter per-pair ceiling (GBP_JPY 0.5) → veto at 0.6", order.SideBuy, 0.5, summary24h(0.60, 200), "chase_buy_veto"},
		{"no 24h candles → fail-open submitted", order.SideBuy, 0.85, summary24h(0.92, 0), "submitted"},
		{"nil summary → fail-open submitted", order.SideBuy, 0.85, nil, "submitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sub []strategy.Signal
			c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
				return strategy.LLMTradeDecision{Go: true, Side: tc.side, TPPips: 30, SLPips: 25, Reason: "x"}, nil
			}, &sub)
			c.MaxRangePos24hBuy = tc.ceiling
			if tc.summary != nil {
				c.BuildSummary = tc.summary
			}
			r, err := c.Run(context.Background())
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if r.Stage != tc.wantStage {
				t.Fatalf("stage=%q want %q", r.Stage, tc.wantStage)
			}
			wantSubmits := 0
			if tc.wantStage == "submitted" {
				wantSubmits = 1
			}
			if len(sub) != wantSubmits {
				t.Fatalf("submits=%d want %d", len(sub), wantSubmits)
			}
		})
	}
}

// Sell-low veto: chasing a SELL into the bottom of the 24h range is banned per-pair for pairs
// that tend to REBOUND off their lows. Pairs where sell-low continuation holds keep it OFF.
// 0 = OFF. Missing 24h data fails open.
func TestLLMDecisionCycle_SellLowVeto_RangePos24h(t *testing.T) {
	cases := []struct {
		name      string
		side      order.Side
		floor     float64
		summary   func() *market.MarketSummary
		wantStage string
	}{
		{"SELL below floor → veto", order.SideSell, 0.15, summary24h(0.05, 200), "sell_low_veto"},
		{"SELL above floor → submitted", order.SideSell, 0.15, summary24h(0.30, 200), "submitted"},
		{"BUY below floor → submitted (SELL-only veto)", order.SideBuy, 0.15, summary24h(0.05, 200), "submitted"},
		{"veto OFF (0) → submitted (USD_JPY keeps sell-low)", order.SideSell, 0, summary24h(0.05, 200), "submitted"},
		{"no 24h candles → fail-open submitted", order.SideSell, 0.15, summary24h(0.05, 0), "submitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sub []strategy.Signal
			c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
				return strategy.LLMTradeDecision{Go: true, Side: tc.side, TPPips: 30, SLPips: 25, Reason: "x"}, nil
			}, &sub)
			c.MinRangePos24hSell = tc.floor
			c.BuildSummary = tc.summary
			r, err := c.Run(context.Background())
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if r.Stage != tc.wantStage {
				t.Fatalf("stage=%q want %q", r.Stage, tc.wantStage)
			}
			wantSubmits := 0
			if tc.wantStage == "submitted" {
				wantSubmits = 1
			}
			if len(sub) != wantSubmits {
				t.Fatalf("submits=%d want %d", len(sub), wantSubmits)
			}
		})
	}
}

// A nil journal must be a safe no-op (journaling is never on the trade-critical path).
func TestLLMDecisionCycle_NilJournal_NoOp(t *testing.T) {
	var sub []strategy.Signal
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		return strategy.LLMTradeDecision{Go: true, Side: order.SideBuy, TPPips: 30, SLPips: 15}, nil
	}, &sub)
	c.Journal = nil
	if r, err := c.Run(context.Background()); err != nil || r.Stage != "submitted" || len(sub) != 1 {
		t.Fatalf("nil journal must not change behaviour; stage=%q err=%v submits=%d", r.Stage, err, len(sub))
	}
}

// summaryChange builds a decision-time summary with the given 24h/15m net moves (veto tests).
func summaryChange(chg24, chg15m float64, numCandles int) func() *market.MarketSummary {
	return func() *market.MarketSummary {
		return &market.MarketSummary{
			CurrentRate: market.CurrentRate{Bid: 161.19, Ask: 161.20, SpreadPips: 1.0},
			Summary15m:  market.WindowSummary{ChangePips: chg15m, NumCandles: numCandles},
			Summary24h:  market.WindowSummary{ChangePips: chg24, RangePositionPct: 0.5, NumCandles: numCandles},
		}
	}
}

// Exhaustion veto: entering in the SAME direction a move has ALREADY
// travelled ≥ ExhaustionVetoPips over 24h is a chase into a spent move (e.g. repeatedly
// selling after the day has already fallen −140+). Counter-side
// entries are the htf veto's job, not this one's. 0 = OFF; missing 24h data fails OPEN.
func TestLLMDecisionCycle_ExhaustionVeto(t *testing.T) {
	cases := []struct {
		name      string
		side      order.Side
		vetoPips  float64
		summary   func() *market.MarketSummary
		wantStage string
	}{
		{"SELL after −150 in 24h → veto (chasing a spent fall)", order.SideSell, 120, summaryChange(-150, 0, 200), "exhaustion_veto"},
		{"BUY after +150 in 24h → veto (chasing a spent rise)", order.SideBuy, 120, summaryChange(150, 0, 200), "exhaustion_veto"},
		{"SELL at −80 in 24h → submitted (fresh move)", order.SideSell, 120, summaryChange(-80, 0, 200), "submitted"},
		{"SELL after +150 in 24h → submitted here (counter-side is htf veto's job)", order.SideSell, 120, summaryChange(150, 0, 200), "submitted"},
		{"veto disabled (0) → submitted", order.SideSell, 0, summaryChange(-150, 0, 200), "submitted"},
		{"no 24h window → fail-open submitted", order.SideSell, 120, summaryChange(-150, 0, 0), "submitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sub []strategy.Signal
			c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
				return strategy.LLMTradeDecision{Go: true, Side: tc.side, TPPips: 30, SLPips: 25, Reason: "x"}, nil
			}, &sub)
			c.ExhaustionVetoPips = tc.vetoPips
			c.BuildSummary = tc.summary
			j := &recordingJournal{}
			c.Journal = j
			r, err := c.Run(context.Background())
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if r.Stage != tc.wantStage {
				t.Fatalf("stage=%q want %q", r.Stage, tc.wantStage)
			}
			if len(j.entries) != 1 || j.entries[0].Stage != tc.wantStage {
				t.Fatalf("journal must record one %q entry, got %+v", tc.wantStage, j.entries)
			}
		})
	}
}

// Spike-cooldown veto: |15m net move| ≥ SpikeVetoPips15m means the price
// is mid-spike — entering EITHER side right after a vertical move is a known losing pattern
// (the LLM may even note it, e.g. "5m急落直後の低位売りは割引だが入る", and enter anyway). Wait a cycle.
// 0 = OFF; missing 15m data fails OPEN.
func TestLLMDecisionCycle_SpikeVeto(t *testing.T) {
	cases := []struct {
		name      string
		side      order.Side
		vetoPips  float64
		summary   func() *market.MarketSummary
		wantStage string
	}{
		{"SELL right after a −20pip 15m plunge → veto", order.SideSell, 15, summaryChange(-40, -20, 200), "spike_veto"},
		{"BUY right after a +20pip 15m spike → veto", order.SideBuy, 15, summaryChange(40, 20, 200), "spike_veto"},
		{"BUY right after a −20pip 15m plunge → veto (no knife-catching either)", order.SideBuy, 15, summaryChange(-40, -20, 200), "spike_veto"},
		{"calm 15m (−4) → submitted", order.SideSell, 15, summaryChange(-40, -4, 200), "submitted"},
		{"veto disabled (0) → submitted", order.SideSell, 0, summaryChange(-40, -20, 200), "submitted"},
		{"no 15m window → fail-open submitted", order.SideSell, 15, summaryChange(-40, -20, 0), "submitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sub []strategy.Signal
			c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
				return strategy.LLMTradeDecision{Go: true, Side: tc.side, TPPips: 30, SLPips: 25, Reason: "x"}, nil
			}, &sub)
			c.SpikeVetoPips15m = tc.vetoPips
			c.BuildSummary = tc.summary
			r, err := c.Run(context.Background())
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if r.Stage != tc.wantStage {
				t.Fatalf("stage=%q want %q", r.Stage, tc.wantStage)
			}
		})
	}
}

// The cycle feeds TODAY's closed trades (06:00 JST trading-day boundary, fetched by the
// wiring) into the summary BEFORE deciding, so the playbook's 同日2敗打ち止め checklist box has
// its evidence (today_closed_trades in the trimmed prompt payload). A fetch error fails open
// (empty list, cycle continues) — a DB blip must never halt trading.
func TestLLMDecisionCycle_TodayClosedTrades_FedToDecider(t *testing.T) {
	var got *market.MarketSummary
	decide := func(_ context.Context, s *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		got = s
		return strategy.LLMTradeDecision{Go: false, Side: "none"}, nil
	}

	t.Run("records mapped into summary.RecentTrades", func(t *testing.T) {
		var sub []strategy.Signal
		c := baseLLMCycle(decide, &sub)
		c.BuildSummary = summaryChange(-40, 0, 200)
		c.TodayClosedTrades = func(_ context.Context) ([]port.TradeRecord, error) {
			return []port.TradeRecord{
				{Side: "SELL", ProfitLossPips: -16.1, ProfitLossJPY: -1610, CloseReason: "ratchet_stoploss"},
				{Side: "BUY", ProfitLossPips: 30, ProfitLossJPY: 300, CloseReason: "take_profit"},
			}, nil
		}
		if _, err := c.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got == nil || len(got.RecentTrades) != 2 {
			t.Fatalf("decider must see 2 today-trades, got %+v", got)
		}
		if got.RecentTrades[0].CloseReason != "ratchet_stoploss" || got.RecentTrades[0].ProfitLossPips != -16.1 {
			t.Fatalf("today-trade mapping wrong: %+v", got.RecentTrades[0])
		}
	})

	t.Run("fetch error fails open (no trades, cycle continues)", func(t *testing.T) {
		var sub []strategy.Signal
		got = nil
		c := baseLLMCycle(decide, &sub)
		c.BuildSummary = summaryChange(-40, 0, 200)
		c.TodayClosedTrades = func(_ context.Context) ([]port.TradeRecord, error) {
			return nil, errors.New("db blip")
		}
		r, err := c.Run(context.Background())
		if err != nil || r.Stage != "no_trade" {
			t.Fatalf("fetch error must fail open: stage=%q err=%v", r.Stage, err)
		}
		if got == nil || len(got.RecentTrades) != 0 {
			t.Fatalf("on fetch error the decider sees no today-trades, got %+v", got.RecentTrades)
		}
	})
}

// Daily-loss stop: the playbook's 同日2敗打ち止め must not be prompt-only —
// the cycle already fetches today's closed trades, so ≥N gross losses today (06:00 JST day) stops
// the symbol BEFORE the LLM call (deterministic, cheaper, journalled as daily_loss_stop). Fetch
// error keeps failing open (no false stop from a DB blip); 0 = OFF.
func TestLLMDecisionCycle_DailyLossStop(t *testing.T) {
	losses := func(n int, win bool) func(_ context.Context) ([]port.TradeRecord, error) {
		return func(_ context.Context) ([]port.TradeRecord, error) {
			out := make([]port.TradeRecord, 0, n+1)
			for i := 0; i < n; i++ {
				out = append(out, port.TradeRecord{Side: "SELL", ProfitLossJPY: -250, ProfitLossPips: -25, CloseReason: "stop_loss"})
			}
			if win {
				out = append(out, port.TradeRecord{Side: "BUY", ProfitLossJPY: 300, ProfitLossPips: 30, CloseReason: "take_profit"})
			}
			return out, nil
		}
	}
	cases := []struct {
		name       string
		stopCount  int
		today      func(_ context.Context) ([]port.TradeRecord, error)
		wantStage  string
		wantDecide bool
	}{
		{"2 losses at cap 2 → stop BEFORE deciding", 2, losses(2, true), "daily_loss_stop", false},
		{"3 losses at cap 2 → stop", 2, losses(3, false), "daily_loss_stop", false},
		{"1 loss at cap 2 → proceeds", 2, losses(1, true), "no_trade", true},
		{"wins only → proceeds", 2, losses(0, true), "no_trade", true},
		{"disabled (0) → proceeds even at 3 losses", 0, losses(3, false), "no_trade", true},
		{"fetch error → fail-open proceeds", 2, func(_ context.Context) ([]port.TradeRecord, error) { return nil, errors.New("db blip") }, "no_trade", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decided := false
			var sub []strategy.Signal
			c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
				decided = true
				return strategy.LLMTradeDecision{Go: false, Side: "none"}, nil
			}, &sub)
			c.BuildSummary = summaryChange(-40, 0, 200)
			c.DailyLossStopCount = tc.stopCount
			c.TodayClosedTrades = tc.today
			j := &recordingJournal{}
			c.Journal = j
			r, err := c.Run(context.Background())
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if r.Stage != tc.wantStage || decided != tc.wantDecide {
				t.Fatalf("stage=%q decided=%v; want %q/%v", r.Stage, decided, tc.wantStage, tc.wantDecide)
			}
			if len(j.entries) != 1 || j.entries[0].Stage != tc.wantStage {
				t.Fatalf("journal must record one %q entry, got %+v", tc.wantStage, j.entries)
			}
		})
	}
}

// Arms: a no_trade decision may pre-place conditional plans.
// The cycle validates them (feature flag / trustworthy current rate / trigger distance / the
// lane tag matching the side), stores them via StoreArms, and journals stage=armed. Every
// VALID decision replaces the previous hour's arms wholesale (ClearArms first); a decider
// error keeps the old arms (they expire on their own).
func TestLLMDecisionCycle_Arms(t *testing.T) {
	sellPlan := strategy.ArmedPlan{Side: order.SideSell, TriggerPrice: 161.00, BreakAbove: false, TPPips: 30, SLPips: 25, Reason: "[L1] 戻り安値割れ"}
	buyPlanFarAway := strategy.ArmedPlan{Side: order.SideBuy, TriggerPrice: 165.00, BreakAbove: true, TPPips: 30, SLPips: 25, Reason: "[L4] 上抜け"}
	buyPlanWrongTag := strategy.ArmedPlan{Side: order.SideBuy, TriggerPrice: 161.40, BreakAbove: true, TPPips: 30, SLPips: 25, Reason: "[L1] タグ不一致"}
	mkDecide := func(arms ...strategy.ArmedPlan) LLMDecideFunc {
		return func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
			return strategy.LLMTradeDecision{Go: false, Side: "none", Reason: "[arm] 土俵成立・再転換待ち", Arms: arms}, nil
		}
	}
	// summaryChange sets CurrentRate Bid 161.19 / Ask 161.20 (mid 161.195).
	base := func(decide LLMDecideFunc) (*LLMDecisionCycle, *[]strategy.ArmedPlan, *int, *int) {
		var stored []strategy.ArmedPlan
		storeCalls, clearCalls := 0, 0
		var sub []strategy.Signal
		c := baseLLMCycle(decide, &sub)
		c.BuildSummary = summaryChange(-40, 0, 200)
		c.ArmEnabled = true
		c.StoreArms = func(plans []strategy.ArmedPlan) { stored = plans; storeCalls++ }
		c.ClearArms = func() { clearCalls++ }
		return c, &stored, &storeCalls, &clearCalls
	}

	t.Run("valid sell plan → stored + stage armed + journalled", func(t *testing.T) {
		c, stored, storeCalls, clearCalls := base(mkDecide(sellPlan))
		j := &recordingJournal{}
		c.Journal = j
		r, err := c.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if r.Stage != "armed" || *storeCalls != 1 || len(*stored) != 1 || (*stored)[0].TriggerPrice != 161.00 {
			t.Fatalf("stage=%q store=%d stored=%+v", r.Stage, *storeCalls, *stored)
		}
		if *clearCalls != 1 {
			t.Fatalf("valid decision must first clear old arms, clear=%d", *clearCalls)
		}
		if len(j.entries) != 1 || j.entries[0].Stage != "armed" || j.entries[0].Arms == "" {
			t.Fatalf("journal must record armed with arms summary, got %+v", j.entries)
		}
	})
	t.Run("feature flag off → plain no_trade, nothing stored", func(t *testing.T) {
		c, _, storeCalls, _ := base(mkDecide(sellPlan))
		c.ArmEnabled = false
		r, _ := c.Run(context.Background())
		if r.Stage != "no_trade" || *storeCalls != 0 {
			t.Fatalf("stage=%q store=%d", r.Stage, *storeCalls)
		}
	})
	t.Run("trigger too far from current rate → dropped → no_trade", func(t *testing.T) {
		c, _, storeCalls, _ := base(mkDecide(buyPlanFarAway)) // 165.00 vs mid 161.195 = 380pips ≫ 30
		r, _ := c.Run(context.Background())
		if r.Stage != "no_trade" || *storeCalls != 0 {
			t.Fatalf("stage=%q store=%d", r.Stage, *storeCalls)
		}
	})
	t.Run("lane tag not matching side → dropped", func(t *testing.T) {
		c, _, storeCalls, _ := base(mkDecide(buyPlanWrongTag))
		r, _ := c.Run(context.Background())
		if r.Stage != "no_trade" || *storeCalls != 0 {
			t.Fatalf("stage=%q store=%d", r.Stage, *storeCalls)
		}
	})
	t.Run("no trustworthy current rate → all dropped", func(t *testing.T) {
		c, _, storeCalls, _ := base(mkDecide(sellPlan))
		c.BuildSummary = func() *market.MarketSummary {
			return &market.MarketSummary{Summary24h: market.WindowSummary{ChangePips: -40, NumCandles: 200}}
		}
		r, _ := c.Run(context.Background())
		if r.Stage != "no_trade" || *storeCalls != 0 {
			t.Fatalf("stage=%q store=%d", r.Stage, *storeCalls)
		}
	})
	t.Run("go:true clears old arms and submits", func(t *testing.T) {
		var sub []strategy.Signal
		clearCalls := 0
		c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
			return strategy.LLMTradeDecision{Go: true, Side: order.SideSell, TPPips: 30, SLPips: 25, Reason: "[L1] 即"}, nil
		}, &sub)
		c.BuildSummary = summaryChange(-40, 0, 200)
		c.ArmEnabled = true
		c.ClearArms = func() { clearCalls++ }
		r, err := c.Run(context.Background())
		if err != nil || r.Stage != "submitted" || len(sub) != 1 || clearCalls != 1 {
			t.Fatalf("stage=%q err=%v subs=%d clear=%d", r.Stage, err, len(sub), clearCalls)
		}
	})
	t.Run("decider error keeps old arms (no clear)", func(t *testing.T) {
		var sub []strategy.Signal
		clearCalls := 0
		c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
			return strategy.LLMTradeDecision{}, errors.New("cli boom")
		}, &sub)
		c.BuildSummary = summaryChange(-40, 0, 200)
		c.ArmEnabled = true
		c.ClearArms = func() { clearCalls++ }
		_, err := c.Run(context.Background())
		if err == nil || clearCalls != 0 {
			t.Fatalf("decider error must keep old arms, err=%v clear=%d", err, clearCalls)
		}
	})
}

// セッションガード: NoEntryHoursJST の JST 時間帯は全 side の新規を
// 止める。早朝 05:45 前後のスプレッド拡大 (平常時の 10 倍超になりうる) までの滑走路が
// 3.5h 未満しかない建玉を構造排除する。excluded_hour と
// 同じく LLM 呼び出しの前で skip する (API 節約 + 判断ごと止める)。
func TestLLMDecisionCycle_NoEntryHourJST_SkipsBeforeDeciding(t *testing.T) {
	var sub []strategy.Signal
	decided := false
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		decided = true
		return strategy.LLMTradeDecision{Go: true, Side: order.SideSell, TPPips: 30, SLPips: 15}, nil
	}, &sub)
	c.NoEntryHoursJST = []int{2, 3, 4, 5}

	// 17:30 UTC = 02:30 JST → 禁止帯 → LLM を呼ばず skip (SELL も止まる点が night_buy_veto と違う)。
	c.Now = func() time.Time { return time.Date(2026, 7, 16, 17, 30, 0, 0, time.UTC) }
	r, _ := c.Run(context.Background())
	if r.Stage != "no_entry_hour" || len(sub) != 0 || decided {
		t.Fatalf("no-entry hour must skip pre-decide: stage=%q submits=%d decided=%v", r.Stage, len(sub), decided)
	}

	// 21:30 UTC = 06:30 JST → 帯の外 → 通常どおり判断・発注。
	sub, decided = nil, false
	c.Now = func() time.Time { return time.Date(2026, 7, 16, 21, 30, 0, 0, time.UTC) }
	r, _ = c.Run(context.Background())
	if !decided || len(sub) != 1 {
		t.Fatalf("outside no-entry hours must trade: decided=%v submits=%d stage=%q", decided, len(sub), r.Stage)
	}
}

// emergency_stop 中はサイクル冒頭で止め、LLM を呼ばない(新規は admission でも止まるが、
// 止まると分かっている判断のために CLI を呼んで利用枠を使わない)。解除後は通常どおり判断する。
func TestLLMDecisionCycle_EmergencyStop_SkipsBeforeDeciding(t *testing.T) {
	var sub []strategy.Signal
	decided := false
	c := baseLLMCycle(func(_ context.Context, _ *market.MarketSummary, _ string) (strategy.LLMTradeDecision, error) {
		decided = true
		return strategy.LLMTradeDecision{Go: true, Side: order.SideSell, TPPips: 30, SLPips: 15}, nil
	}, &sub)
	halted := true
	c.EmergencyActive = func() bool { return halted }

	r, _ := c.Run(context.Background())
	if r.Stage != "emergency_stop" || len(sub) != 0 || decided {
		t.Fatalf("emergency stop must skip pre-decide: stage=%q submits=%d decided=%v", r.Stage, len(sub), decided)
	}

	halted = false
	r, _ = c.Run(context.Background())
	if !decided || len(sub) != 1 {
		t.Fatalf("after resume the cycle must decide and trade: decided=%v submits=%d stage=%q", decided, len(sub), r.Stage)
	}
}
