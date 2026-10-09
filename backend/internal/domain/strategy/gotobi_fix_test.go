package strategy

import (
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// isGotobiTradingDay is the pure heart of the gotobi probe: which JST dates the
// importer-fixing flow should be traded. Gotobi calendar days are 5/10/15/20/25
// and month-end; a gotobi day on a weekend shifts back to the prior business day
// (Friday). Weekends never trade. (JP bank holidays are out of scope.)
func TestIsGotobiTradingDay(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	at := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 9, 0, 0, 0, jst) }
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"Tue 5th = gotobi", at(2024, 3, 5), true},
		{"Wed 20th = gotobi", at(2024, 3, 20), true},
		{"Fri 15th = gotobi (itself)", at(2024, 3, 15), true},
		{"Fri 31 May = month-end gotobi", at(2024, 5, 31), true},
		{"Sun 10th = weekend, no trade", at(2024, 3, 10), false},
		{"Fri 8th = 10th(Sun) shifted back", at(2024, 3, 8), true},
		{"Sat 25th = weekend, no trade", at(2023, 11, 25), false},
		{"Fri 24th = 25th(Sat) shifted back", at(2023, 11, 24), true},
		{"Sun 31 Mar = weekend month-end, no trade", at(2024, 3, 31), false},
		{"Fri 29 Mar = month-end(Sun 31) shifted back", at(2024, 3, 29), true},
		{"Tue 12th = normal day", at(2024, 3, 12), false},
		{"Fri 22nd = normal Friday (no weekend gotobi)", at(2024, 3, 22), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGotobiTradingDay(tc.t); got != tc.want {
				t.Errorf("isGotobiTradingDay(%s) = %v, want %v", tc.t.Format("2006-01-02 Mon"), got, tc.want)
			}
		})
	}
}

func TestGotobiFix_Evaluate(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	cfg := &config.StrategyConfig{
		ConfigID: "test-gotobi",
		Symbol:   "USD_JPY",
		Entry:    config.EntrySection{Direction: config.DirectionBuyOnly, MaxSpreadPips: 1.0},
		Risk:     config.ConfigRiskSection{Quantity: 1000},
	}
	sum := &market.MarketSummary{CurrentRate: market.CurrentRate{Bid: 150.0, Ask: 150.0, SpreadPips: 0}}
	eval := func(now time.Time, c *config.StrategyConfig) Signal {
		return GotobiFix{}.Evaluate(EvalInput{Now: now, Config: c, Summary: sum})
	}

	// Fires on a gotobi day at exactly 09:00 JST: buy, MaxHold to the fix, protective SL.
	sig := eval(time.Date(2024, 3, 5, 9, 0, 0, 0, jst), cfg)
	if sig.Decision != DecisionEnter || sig.Side != order.SideBuy {
		t.Fatalf("gotobi 09:00 must ENTER buy; got %v %v (%s)", sig.Decision, sig.Side, sig.Reason)
	}
	if sig.MaxHoldMinutes != gotobiMaxHoldMin || sig.StopLossPips != gotobiSLPips {
		t.Errorf("exit plan = MaxHold %d / SL %.0f; want %d / %.0f", sig.MaxHoldMinutes, sig.StopLossPips, gotobiMaxHoldMin, gotobiSLPips)
	}
	if sig.Quantity != 1000 {
		t.Errorf("quantity = %d, want 1000", sig.Quantity)
	}

	// Off the entry minute → no entry (engine only opens once per day at 09:00).
	if s := eval(time.Date(2024, 3, 5, 10, 0, 0, 0, jst), cfg); s.Decision == DecisionEnter {
		t.Errorf("gotobi 10:00 must not enter; got %v", s.Decision)
	}
	// Not a gotobi day → no entry.
	if s := eval(time.Date(2024, 3, 12, 9, 0, 0, 0, jst), cfg); s.Decision == DecisionEnter {
		t.Errorf("non-gotobi 09:00 must not enter; got %v", s.Decision)
	}
	// sell_only blocks the long.
	sellCfg := *cfg
	sellCfg.Entry.Direction = config.DirectionSellOnly
	if s := eval(time.Date(2024, 3, 5, 9, 0, 0, 0, jst), &sellCfg); s.Decision == DecisionEnter {
		t.Errorf("sell_only must block the gotobi buy; got %v", s.Decision)
	}
}
