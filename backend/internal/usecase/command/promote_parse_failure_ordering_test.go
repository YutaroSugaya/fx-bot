package command

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// Two regressions pinned here:
//
//   (a) event ordering: on parse failure, Promoter called recordValidationEvent BEFORE
//       insertRejectedRow. config_validation_events.config_id has a NOT
//       NULL FK to strategy_configs(config_id), so in production this
//       inserted nothing (FK violation) — and the error was swallowed by
//       `_ =`. The audit event for parse errors was effectively lost.
//
//   (b) raw input: insertRejectedRow did not set ParseFailureRaw, so the
//       strategy_config_parse_failures junction was never populated even
//       though the schema dedicates a column for raw_input.
//
// fkAwareValidationRepo enforces the same FK behaviour the production
// DB has, so the bug surfaces in the test (Insert errors out before the
// parent row exists) — the in-memory repo used in existing tests does
// not, which is why the regression slipped through.

type fkAwareValidationRepo struct {
	parent *backtest.InMemoryStrategyConfigRepo
	rows   []port.ConfigValidationEvent
}

var errFKViolation = errors.New("FK violation: config_id has no matching strategy_configs row")

func (r *fkAwareValidationRepo) Insert(_ context.Context, ev port.ConfigValidationEvent) error {
	recs, _ := r.parent.ListRecent(context.Background(), 1000)
	for _, rec := range recs {
		if rec.ConfigID == ev.ConfigID {
			r.rows = append(r.rows, ev)
			return nil
		}
	}
	return errFKViolation
}

// CountFailSince: satisfies the Dashboard-extension interface method.
// This fake doesn't track timestamps so the `since` arg is unused.
func (r *fkAwareValidationRepo) CountFailSince(_ context.Context, _ time.Time) (int, error) {
	n := 0
	for _, ev := range r.rows {
		if ev.Status == "fail" {
			n++
		}
	}
	return n, nil
}

func TestPromoter_ParseFailure_PersistsValidationEventAndParseFailureJunction(t *testing.T) {
	strategyRepo := backtest.NewInMemoryStrategyConfigRepo()
	validationRepo := &fkAwareValidationRepo{parent: strategyRepo}

	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	v := config.NewValidator(validHardLimits())
	v.Now = func() time.Time { return now }
	p := NewPromoter(v, strategyRepo, validationRepo, nil, config.ModePaperConfig, "auto")
	p.Clock = func() time.Time { return now }

	res, err := p.PromoteFromYAML(context.Background(),
		[]byte("this is not yaml: at all: ::"),
		config.AccountState{}, "manual")
	if err != nil {
		t.Fatalf("PromoteFromYAML returned err: %v", err)
	}
	if res.RejectReason == "" {
		t.Fatalf("expected reject_reason for parse error")
	}

	// 1. strategy_configs row inserted with status=rejected + ParseFailureRaw set.
	recsWithMeta, _ := strategyRepo.ListRecent(context.Background(), 10)
	if len(recsWithMeta) != 1 {
		t.Fatalf("expected exactly 1 rejected row inserted; got %d", len(recsWithMeta))
	}
	rec := recsWithMeta[0].StrategyConfigRecord
	if rec.Status != port.StrategyConfigStatusRejected {
		t.Errorf("status: got %q want rejected", rec.Status)
	}
	if rec.ParseFailureRaw == "" {
		t.Errorf("ParseFailureRaw must be set so the strategy_config_parse_failures junction is populated (regression b)")
	}

	// 2. validation event persisted (= inserted AFTER the parent row; regression a).
	if len(validationRepo.rows) == 0 {
		t.Fatalf("expected at least one config_validation_events row for parse failure; got 0 (FK violation swallowed?)")
	}
	found := false
	for _, ev := range validationRepo.rows {
		if ev.ValidationType == "schema" && ev.Status == "fail" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected schema/fail validation event; rows=%+v", validationRepo.rows)
	}
}

// Validation-failure path (parses fine but a hard_limit / semantic / risk
// check fails) had the SAME bug pattern as the parse path: promoter
// called persistValidationEvents BEFORE insertRejectedRow. The first fix
// for regression (a) only covered the parse-error branch;
// here we pin the validate-fail branch too.
func TestPromoter_ValidationFailure_PersistsAllFourValidationEvents(t *testing.T) {
	strategyRepo := backtest.NewInMemoryStrategyConfigRepo()
	validationRepo := &fkAwareValidationRepo{parent: strategyRepo}

	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	v := config.NewValidator(validHardLimits())
	v.Now = func() time.Time { return now }
	p := NewPromoter(v, strategyRepo, validationRepo, nil, config.ModePaperConfig, "auto")
	p.Clock = func() time.Time { return now }

	// take_profit_pips=999 is well above hard_limits Max=50 → hard_limit fail.
	// The rest of the YAML is structurally valid so parse + schema pass.
	bad := []byte(fmt.Sprintf(`
config_id: "cfg-validate-fail"
generated_at: %s
valid_from: %s
valid_until: %s
symbol: USD_JPY
enabled: true
market_regime:
  type: trend_up
  confidence: 0.68
  reason: test
strategy:
  name: momentum_pullback
  timeframe: 5m
  trend_timeframe: 1h
entry:
  max_spread_pips: 0.5
  min_volatility_pips_5m: 1.5
  max_volatility_pips_5m: 6.0
  require_breakout: false
  direction: both
exit:
  take_profit_pips: 999.0
  stop_loss_pips: 20.0
  max_hold_minutes: 240
risk:
  quantity: 100
  max_open_positions: 1
  max_trades_in_this_window: 2
  max_loss_in_this_window_jpy: 3000
no_trade:
  enabled: false
  reason: ""
`,
		now.Format(time.RFC3339),
		now.Format(time.RFC3339),
		now.Add(time.Hour).Format(time.RFC3339),
	))

	res, err := p.PromoteFromYAML(context.Background(), bad, config.AccountState{}, "manual")
	if err != nil {
		t.Fatalf("PromoteFromYAML returned err: %v", err)
	}
	if res.RejectReason == "" {
		t.Fatalf("expected reject_reason for hard_limit fail")
	}

	// 1. The strategy_configs row must exist (status=rejected) — otherwise
	// the FK-aware validation repo would refuse every subsequent event insert.
	recs, _ := strategyRepo.ListRecent(context.Background(), 10)
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 rejected row; got %d", len(recs))
	}
	if recs[0].Status != port.StrategyConfigStatusRejected {
		t.Errorf("status: got %q want rejected", recs[0].Status)
	}
	if recs[0].ConfigID != "cfg-validate-fail" {
		t.Errorf("config_id: got %q want cfg-validate-fail", recs[0].ConfigID)
	}

	// 2. All four validation events (schema, hard_limit, semantic, risk)
	// must be persisted (each with pass / fail). FK violation would drop
	// every one of them on the floor before the fix.
	if len(validationRepo.rows) != 4 {
		t.Fatalf("expected 4 validation events (one per type) inserted AFTER parent row; got %d: %+v",
			len(validationRepo.rows), validationRepo.rows)
	}
	byType := map[string]string{}
	for _, ev := range validationRepo.rows {
		byType[ev.ValidationType] = ev.Status
	}
	if byType["hard_limit"] != "fail" {
		t.Errorf("expected hard_limit/fail event; byType=%+v", byType)
	}
	for _, typ := range []string{"schema", "semantic", "risk"} {
		if _, ok := byType[typ]; !ok {
			t.Errorf("expected %s event (pass or fail) persisted; byType=%+v", typ, byType)
		}
	}
}
