package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

// --- AdvisorCycle.Symbol caller-side early mismatch guard ---
//
// Promoter.ExpectedSymbol stays as the source-of-truth reject point, but
// AdvisorCycle.Run should short-circuit BEFORE PromoteFromYAML when it can
// tell the YAML targets a different symbol. This saves a full
// parse + 4-pass validate + rejected-row write on every Claude mistake.

type fakeAdvisor struct {
	yaml []byte
}

func (f *fakeAdvisor) Generate(_ context.Context, _ *market.MarketSummary) (*port.AdvisorRun, error) {
	return &port.AdvisorRun{
		RunID:      "run-test",
		Status:     port.AdvisorRunStatusSuccess,
		ParsedYAML: f.yaml,
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	}, nil
}

func newAdvisorCycleForTest(t *testing.T, symbol string, yaml []byte) (*AdvisorCycle, *backtest.InMemoryStrategyConfigRepo) {
	t.Helper()
	strat := backtest.NewInMemoryStrategyConfigRepo()
	val := backtest.NewInMemoryValidationEventRepo()
	validator := config.NewValidator(validHardLimits())
	now := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	validator.Now = func() time.Time { return now }
	p := NewPromoter(validator, strat, val, nil, config.ModePaperConfig, "auto")
	p.Clock = func() time.Time { return now }
	p.ExpectedSymbol = symbol
	return &AdvisorCycle{
		Symbol:       symbol,
		Advisor:      &fakeAdvisor{yaml: yaml},
		Promoter:     p,
		BotConfig:    &config.BotConfig{Bot: config.BotSection{Mode: config.ModePaperConfig}},
		Logger:       silentLogger(),
		BuildSummary: func() *market.MarketSummary { return &market.MarketSummary{} },
		GetAccountState: func() (config.AccountState, error) {
			return config.AccountState{MaxDailyLossJPY: 1000, MaxConsecutiveLosses: 3, MaxOpenPositions: 2}, nil
		},
	}, strat
}

// ① dead-market guard end-to-end: an enabled trade whose TP is unreachable for
// the recent 1h range must be promoted as no_trade (downgraded, not rejected).
// In a dead market a TP far beyond the recent range is never reached, so every
// trade just pays the cost floor.
func TestAdvisorCycle_UnreachableTP_DowngradedToNoTrade(t *testing.T) {
	now := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	yaml := validYAML("cfg-dead", now) // enabled momentum_pullback, TP 30
	u, strat := newAdvisorCycleForTest(t, "USD_JPY", yaml)
	// 1h range 10 → max reachable TP = 1.3×10 = 13 < 30 → must downgrade.
	u.BuildSummary = func() *market.MarketSummary {
		s := &market.MarketSummary{Symbol: "USD_JPY"}
		s.Summary1h.RangePips = 10
		return s
	}

	res, err := u.Run(context.Background(), port.AdvisorRunSourceAuto)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil || !res.Promoted {
		t.Fatalf("expected the downgraded no_trade to promote, got %+v", res)
	}
	if !res.Parsed.IsNoTradeDecision() || res.Parsed.Strategy.Name != config.StrategyNoTrade {
		t.Errorf("expected promoted config to be no_trade, got strategy=%q enabled=%v",
			res.Parsed.Strategy.Name, res.Parsed.Enabled)
	}
	// raw_yaml persisted must also be no_trade so a startup re-parse agrees.
	rows := strat.Rows()
	if len(rows) != 1 {
		t.Fatalf("expected 1 active row, got %d", len(rows))
	}
	if !strings.Contains(rows[0].RawYAML, "no_trade") {
		t.Errorf("persisted raw_yaml must reflect the no_trade downgrade, got:\n%s", rows[0].RawYAML)
	}
}

// Reachable TP must NOT be downgraded — the guard only blocks dead markets.
func TestAdvisorCycle_ReachableTP_PromotesTradeUnchanged(t *testing.T) {
	now := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	yaml := validYAML("cfg-live", now) // TP 30
	u, _ := newAdvisorCycleForTest(t, "USD_JPY", yaml)
	// 1h range 30 → max reachable = 1.3×30 = 39 ≥ 30 → reachable, keep the trade.
	u.BuildSummary = func() *market.MarketSummary {
		s := &market.MarketSummary{Symbol: "USD_JPY"}
		s.Summary1h.RangePips = 30
		return s
	}

	res, err := u.Run(context.Background(), port.AdvisorRunSourceAuto)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil || !res.Promoted {
		t.Fatalf("expected promoted, got %+v", res)
	}
	if res.Parsed.Strategy.Name != config.StrategyMomentumPullback {
		t.Errorf("reachable TP must stay a trade, got strategy=%q", res.Parsed.Strategy.Name)
	}
}

func TestAdvisorCycle_SymbolMismatch_RejectsBeforePromoter(t *testing.T) {
	// AdvisorCycle pinned to EUR_JPY; Claude returns a YAML for USD_JPY.
	// Run() must return an error AND skip Promoter entirely (no rejected
	// row written to strategy_configs).
	now := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	yaml := validYAML("cfg-wrong-sym", now) // validYAML default symbol = USD_JPY
	u, strat := newAdvisorCycleForTest(t, "EUR_JPY", yaml)

	res, err := u.Run(context.Background(), port.AdvisorRunSourceAuto)
	if err == nil {
		t.Fatalf("expected mismatch error, got nil")
	}
	if res != nil {
		t.Errorf("res must be nil on mismatch, got %+v", res)
	}
	if !errors.Is(err, ErrAdvisorSymbolMismatch) {
		t.Errorf("error should wrap ErrAdvisorSymbolMismatch: %v", err)
	}
	// Crucially: Promoter must not have written ANY row (no rejected,
	// no active). If it had, strat.Rows() would be non-empty.
	if rows := strat.Rows(); len(rows) != 0 {
		t.Errorf("Promoter must be skipped on early-guard mismatch; got %d strategy_configs rows", len(rows))
	}
}

func TestAdvisorCycle_SymbolMatch_PromotesNormally(t *testing.T) {
	// Sanity check the happy path: symbol matches → Promoter runs, row written.
	now := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	yaml := validYAML("cfg-ok", now)
	u, strat := newAdvisorCycleForTest(t, "USD_JPY", yaml)

	res, err := u.Run(context.Background(), port.AdvisorRunSourceAuto)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil || !res.Promoted {
		t.Fatalf("expected promoted, got %+v", res)
	}
	if rows := strat.Rows(); len(rows) != 1 {
		t.Errorf("expected 1 active row, got %d", len(rows))
	}
}

func TestAdvisorCycle_EmptyExpectedSymbol_SkipsEarlyGuard(t *testing.T) {
	// AdvisorCycle.Symbol="" disables the early guard (back-compat with
	// pre-multi-symbol wiring). Promoter still runs as before.
	now := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	yaml := validYAML("cfg-any", now)
	u, _ := newAdvisorCycleForTest(t, "", yaml)
	// Promoter.ExpectedSymbol also empty (mirrors single-symbol setup).
	u.Promoter.ExpectedSymbol = ""

	res, err := u.Run(context.Background(), port.AdvisorRunSourceAuto)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || !res.Promoted {
		t.Fatalf("expected promoted, got %+v", res)
	}
}

// Sanity: when Claude returns malformed YAML, early guard must NOT swallow
// the parse error — control falls through to Promoter so the parse-failure
// audit trail (rejected row + parse_failures junction) still gets written.
func TestAdvisorCycle_MalformedYAML_FallsThroughToPromoter(t *testing.T) {
	bad := []byte("not yaml: : :")
	u, strat := newAdvisorCycleForTest(t, "USD_JPY", bad)

	_, err := u.Run(context.Background(), port.AdvisorRunSourceAuto)
	if err != nil {
		// Promoter swallows the parse failure into res.RejectReason, returns nil err.
		t.Fatalf("Run should not propagate parse failure: %v", err)
	}
	rows := strat.Rows()
	if len(rows) != 1 || rows[0].Status != port.StrategyConfigStatusRejected {
		t.Errorf("expected one rejected row from Promoter parse-failure path, got %+v", rows)
	}
	_ = fmt.Sprintf // appease unused import linters if needed
}
