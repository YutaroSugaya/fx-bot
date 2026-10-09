package command

import (
	"context"
	"errors"
	"testing"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// ArmedFire: the deterministic watcher that executes the LLM's pre-placed conditional
// plans. Fires ONLY when (a) the price actually crosses the trigger, and (b) EVERY veto plus
// the lane 土俵 (chg24 band + 6h agreement, keyed by the plan's [L1]/[L4] tag) re-validates on
// FRESH market state at fire time. Anything else keeps the plans armed (they expire on their
// own) and journals why — with a cooldown so a persistent veto doesn't spam the journal.

func sellPlanL1(trigger float64) strategy.ArmedPlan {
	return strategy.ArmedPlan{Side: order.SideSell, TriggerPrice: trigger, BreakAbove: false, TPPips: 30, SLPips: 25, Reason: "[L1] 戻り安値割れ"}
}

func buyPlanL4(trigger float64) strategy.ArmedPlan {
	return strategy.ArmedPlan{Side: order.SideBuy, TriggerPrice: trigger, BreakAbove: true, TPPips: 30, SLPips: 25, Reason: "[L4] 揉み合い上抜け"}
}

type fireFixture struct {
	f       *ArmedFire
	sub     *[]strategy.Signal
	journal *recordingJournal
	cleared *int
}

// summaryFire builds fire-time market state: chg24/chg6/chg15m + rpos with a real rate.
func summaryFire(chg24, chg6, chg15m, rpos float64) func(*market.Ticker) *market.MarketSummary {
	return func(_ *market.Ticker) *market.MarketSummary {
		return &market.MarketSummary{
			CurrentRate: market.CurrentRate{Bid: 161.00, Ask: 161.01, SpreadPips: 1.0},
			Summary15m:  market.WindowSummary{ChangePips: chg15m, NumCandles: 15},
			Summary6h:   market.WindowSummary{ChangePips: chg6, NumCandles: 360},
			Summary24h:  market.WindowSummary{ChangePips: chg24, RangePositionPct: rpos, NumCandles: 1440},
		}
	}
}

func newFireFixture(plans []strategy.ArmedPlan, expiresAt time.Time) *fireFixture {
	var sub []strategy.Signal
	cleared := 0
	j := &recordingJournal{}
	f := &ArmedFire{
		Symbol: "USD_JPY", Pip: 0.01, Quantity: 1000,
		MaxHoldMinutes: 1440, RatchetArmPips: 14, RatchetGivebackPips: 8,
		MaxSpreadPips: 3.0, MaxConcurrent: 1,
		HTFTrendVetoPips: 20, ExhaustionVetoPips: 120, SpikeVetoPips15m: 15,
		NightBuyVetoHoursJST: []int{0, 1, 2, 3, 4, 5},
		MaxRangePos24hBuy:    0.85,
		GetArms: func() ([]strategy.ArmedPlan, time.Time, bool) {
			return plans, expiresAt, len(plans) > 0
		},
		ClearArms:      func() { cleared++ },
		ActiveConfigID: func() string { return "llm-v8-usdjpy" },
		BuildSummary:   summaryFire(-40, -10, -2, 0.4), // healthy L1 土俵 by default
		OpenPositions:  func(_ context.Context) ([]port.PositionRecord, error) { return nil, nil },
		Submit: func(_ context.Context, sig strategy.Signal, _ *market.Ticker) error {
			sub = append(sub, sig)
			return nil
		},
		Journal: j,
		Now:     func() time.Time { return time.Date(2026, 7, 9, 5, 0, 0, 0, time.UTC) }, // 14:00 JST
	}
	return &fireFixture{f: f, sub: &sub, journal: j, cleared: &cleared}
}

func farFuture() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }

func TestArmedFire_SellPlanFiresOnBreakBelow(t *testing.T) {
	fx := newFireFixture([]strategy.ArmedPlan{sellPlanL1(161.00)}, farFuture())
	// bid 160.99 ≤ trigger 161.00 → cross
	r, err := fx.f.OnTick(context.Background(), 160.99, 161.00)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stage != "armed_fired" || len(*fx.sub) != 1 {
		t.Fatalf("stage=%q subs=%d", r.Stage, len(*fx.sub))
	}
	sig := (*fx.sub)[0]
	if sig.Side != order.SideSell || sig.EntryPrice != 160.99 || sig.TakeProfitPips != 30 || sig.StopLossPips != 25 || sig.Quantity != 1000 {
		t.Fatalf("signal: %+v", sig)
	}
	if *fx.cleared != 1 {
		t.Fatalf("arms must be cleared after firing, cleared=%d", *fx.cleared)
	}
	if len(fx.journal.entries) != 1 || fx.journal.entries[0].Stage != "armed_fired" || fx.journal.entries[0].Event != "arm_fire" {
		t.Fatalf("journal: %+v", fx.journal.entries)
	}
}

func TestArmedFire_BuyPlanFiresOnBreakAbove(t *testing.T) {
	fx := newFireFixture([]strategy.ArmedPlan{buyPlanL4(161.01)}, farFuture())
	fx.f.BuildSummary = summaryFire(+45, +12, 3, 0.7)           // healthy L4 土俵
	r, err := fx.f.OnTick(context.Background(), 161.00, 161.01) // ask 161.01 ≥ trigger
	if err != nil {
		t.Fatal(err)
	}
	if r.Stage != "armed_fired" || len(*fx.sub) != 1 || (*fx.sub)[0].Side != order.SideBuy || (*fx.sub)[0].EntryPrice != 161.01 {
		t.Fatalf("stage=%q subs=%+v", r.Stage, *fx.sub)
	}
}

func TestArmedFire_NoCross_Idle(t *testing.T) {
	fx := newFireFixture([]strategy.ArmedPlan{sellPlanL1(160.50)}, farFuture())
	r, err := fx.f.OnTick(context.Background(), 160.99, 161.00) // bid above trigger
	if err != nil || r.Stage != "idle" || len(*fx.sub) != 0 || len(fx.journal.entries) != 0 {
		t.Fatalf("stage=%q err=%v subs=%d journal=%d", r.Stage, err, len(*fx.sub), len(fx.journal.entries))
	}
}

func TestArmedFire_Expired_ClearsAndJournals(t *testing.T) {
	past := time.Date(2026, 7, 9, 4, 0, 0, 0, time.UTC)
	fx := newFireFixture([]strategy.ArmedPlan{sellPlanL1(161.00)}, past)
	r, err := fx.f.OnTick(context.Background(), 160.99, 161.00)
	if err != nil || r.Stage != "armed_expired" || len(*fx.sub) != 0 || *fx.cleared != 1 {
		t.Fatalf("stage=%q err=%v subs=%d cleared=%d", r.Stage, err, len(*fx.sub), *fx.cleared)
	}
	if len(fx.journal.entries) != 1 || fx.journal.entries[0].Stage != "armed_expired" {
		t.Fatalf("journal: %+v", fx.journal.entries)
	}
}

// Fire-time re-validation. Each veto keeps the plans armed (retry after the cooldown; the next
// cycle replaces them anyway) and journals its stage once — not per tick.
func TestArmedFire_FireTimeRevalidation(t *testing.T) {
	cases := []struct {
		name      string
		plan      strategy.ArmedPlan
		summary   func(*market.Ticker) *market.MarketSummary
		bid, ask  float64
		nowUTC    time.Time
		wantStage string
	}{
		{"lane 土俵 broken (chg24 no longer a falling day) → lane_recheck_failed",
			sellPlanL1(161.00), summaryFire(-10, -5, -2, 0.4), 160.99, 161.00,
			time.Date(2026, 7, 9, 5, 0, 0, 0, time.UTC), "lane_recheck_failed"},
		{"6h disagreement → lane_recheck_failed",
			sellPlanL1(161.00), summaryFire(-40, +6, -2, 0.4), 160.99, 161.00,
			time.Date(2026, 7, 9, 5, 0, 0, 0, time.UTC), "lane_recheck_failed"},
		{"missing 24h window → lane check fail-CLOSES (a pre-placed order needs positive confirmation)",
			sellPlanL1(161.00), func(_ *market.Ticker) *market.MarketSummary {
				return &market.MarketSummary{CurrentRate: market.CurrentRate{Bid: 161.00, Ask: 161.01}}
			}, 160.99, 161.00,
			time.Date(2026, 7, 9, 5, 0, 0, 0, time.UTC), "lane_recheck_failed"},
		{"spike in progress → spike_veto",
			sellPlanL1(161.00), summaryFire(-40, -10, -18, 0.4), 160.99, 161.00,
			time.Date(2026, 7, 9, 5, 0, 0, 0, time.UTC), "spike_veto"},
		{"counter-trend at fire time → htf_trend_veto",
			sellPlanL1(161.00), summaryFire(+30, -5, -2, 0.4), 160.99, 161.00,
			time.Date(2026, 7, 9, 5, 0, 0, 0, time.UTC), "htf_trend_veto"},
		{"exhausted move at fire time → exhaustion_veto",
			sellPlanL1(161.00), summaryFire(-130, -30, -2, 0.4), 160.99, 161.00,
			time.Date(2026, 7, 9, 5, 0, 0, 0, time.UTC), "exhaustion_veto"},
		{"night BUY → night_buy_veto",
			buyPlanL4(161.01), summaryFire(+45, +12, 3, 0.7), 161.00, 161.01,
			time.Date(2026, 7, 8, 16, 30, 0, 0, time.UTC), "night_buy_veto"}, // 01:30 JST
		{"chase BUY (rpos over cap) → chase_buy_veto",
			buyPlanL4(161.01), summaryFire(+45, +12, 3, 0.95), 161.00, 161.01,
			time.Date(2026, 7, 9, 5, 0, 0, 0, time.UTC), "chase_buy_veto"},
		{"wide spread → wide_spread",
			sellPlanL1(161.00), summaryFire(-40, -10, -2, 0.4), 160.99, 161.06, // 7 pips
			time.Date(2026, 7, 9, 5, 0, 0, 0, time.UTC), "wide_spread"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFireFixture([]strategy.ArmedPlan{tc.plan}, farFuture())
			fx.f.BuildSummary = tc.summary
			fx.f.Now = func() time.Time { return tc.nowUTC }
			r, err := fx.f.OnTick(context.Background(), tc.bid, tc.ask)
			if err != nil {
				t.Fatal(err)
			}
			if r.Stage != tc.wantStage || len(*fx.sub) != 0 {
				t.Fatalf("stage=%q want %q subs=%d", r.Stage, tc.wantStage, len(*fx.sub))
			}
			if *fx.cleared != 0 {
				t.Fatalf("a veto must keep the plans armed, cleared=%d", *fx.cleared)
			}
			if len(fx.journal.entries) != 1 || fx.journal.entries[0].Stage != tc.wantStage {
				t.Fatalf("journal: %+v", fx.journal.entries)
			}
			// Same tick again inside the cooldown: no duplicate journal, no submit.
			r2, _ := fx.f.OnTick(context.Background(), tc.bid, tc.ask)
			if r2.Stage != "cooldown" || len(fx.journal.entries) != 1 {
				t.Fatalf("cooldown: stage=%q journal=%d", r2.Stage, len(fx.journal.entries))
			}
		})
	}
}

func TestArmedFire_SlotTaken_NoFire(t *testing.T) {
	fx := newFireFixture([]strategy.ArmedPlan{sellPlanL1(161.00)}, farFuture())
	fx.f.OpenPositions = func(_ context.Context) ([]port.PositionRecord, error) {
		return []port.PositionRecord{{Side: "BUY"}}, nil
	}
	r, err := fx.f.OnTick(context.Background(), 160.99, 161.00)
	if err != nil || r.Stage != "slot_taken" || len(*fx.sub) != 0 {
		t.Fatalf("stage=%q err=%v subs=%d", r.Stage, err, len(*fx.sub))
	}
}

func TestArmedFire_AdmissionRejected_KeepsPlans(t *testing.T) {
	fx := newFireFixture([]strategy.ArmedPlan{sellPlanL1(161.00)}, farFuture())
	fx.f.Submit = func(_ context.Context, _ strategy.Signal, _ *market.Ticker) error {
		return &AdmissionRejectedError{Reason: "daily cap"}
	}
	r, err := fx.f.OnTick(context.Background(), 160.99, 161.00)
	if err != nil || r.Stage != "admission_rejected" || *fx.cleared != 0 {
		t.Fatalf("stage=%q err=%v cleared=%d", r.Stage, err, *fx.cleared)
	}
	if len(fx.journal.entries) != 1 || fx.journal.entries[0].RejectReason == "" {
		t.Fatalf("journal must carry the reject reason: %+v", fx.journal.entries)
	}
}

func TestArmedFire_SubmitTransportError_Surfaced(t *testing.T) {
	fx := newFireFixture([]strategy.ArmedPlan{sellPlanL1(161.00)}, farFuture())
	fx.f.Submit = func(_ context.Context, _ strategy.Signal, _ *market.Ticker) error {
		return errors.New("broker down")
	}
	r, err := fx.f.OnTick(context.Background(), 160.99, 161.00)
	if err == nil || r.Stage != "fire_error" {
		t.Fatalf("stage=%q err=%v", r.Stage, err)
	}
}

// セッションガード: 発火時刻が NoEntryHoursJST に入っていたら
// トリガー到達でも発火拒否 (SELL 含む全 side)。深夜帯に建てた玉が早朝の
// スプレッド拡大で SL を刈られる型を、arm_fire 経路でも構造的に塞ぐ。
func TestArmedFire_NoEntryHourVeto_BlocksAllSides(t *testing.T) {
	fx := newFireFixture([]strategy.ArmedPlan{sellPlanL1(161.00)}, farFuture())
	fx.f.NoEntryHoursJST = []int{2, 3, 4, 5}
	// 17:30 UTC = 02:30 JST → 禁止帯。
	fx.f.Now = func() time.Time { return time.Date(2026, 7, 16, 17, 30, 0, 0, time.UTC) }
	r, err := fx.f.OnTick(context.Background(), 160.99, 161.00) // trigger は crossed
	if err != nil {
		t.Fatal(err)
	}
	if r.Stage != "no_entry_hour" || len(*fx.sub) != 0 {
		t.Fatalf("stage=%q subs=%d (SELL も禁止帯では発火しない)", r.Stage, len(*fx.sub))
	}
	if len(fx.journal.entries) != 1 || fx.journal.entries[0].Stage != "no_entry_hour" {
		t.Fatalf("journal: %+v", fx.journal.entries)
	}
	// plans は保持 (帯明けの再判断は次サイクルの仕事; ここでは Clear しない)。
	if *fx.cleared != 0 {
		t.Fatalf("plans must stay armed, cleared=%d", *fx.cleared)
	}
}
