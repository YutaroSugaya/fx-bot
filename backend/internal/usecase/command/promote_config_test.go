package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/adapter/artifact"
	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

func validHardLimits() *config.HardLimits {
	return &config.HardLimits{
		AllowedSymbols:         []string{"USD_JPY"},
		Quantity:               config.IntRange{Min: 100, Max: 100},
		TakeProfitPips:         config.FloatRange{Min: 15.0, Max: 50.0},
		StopLossPips:           config.FloatRange{Min: 15.0, Max: 30.0},
		MaxHoldMinutes:         config.IntRange{Min: 240, Max: 360},
		MaxTradesInThisWindow:  config.IntRange{Min: 0, Max: 3},
		MaxLossInThisWindowJPY: config.IntRange{Min: 0, Max: 5000},
		MaxSpreadPips:          config.FloatRange{Min: 0.3, Max: 1.0},
		ConfigTTLMinutes:       config.IntRange{Min: 60, Max: 120},
	}
}

func validYAML(configID string, validFrom time.Time) []byte {
	return []byte(fmt.Sprintf(`
config_id: "%s"
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
  take_profit_pips: 30.0
  stop_loss_pips: 20.0
  max_hold_minutes: 240
  early_exit_window_minutes: 30
  early_exit_target_pips: -2.0
  ratchet_arm_pips: 20.0
  ratchet_giveback_pips: 3.0
risk:
  quantity: 100
  max_open_positions: 1
  max_trades_in_this_window: 2
  max_loss_in_this_window_jpy: 3000
no_trade:
  enabled: false
  reason: ""
`, configID,
		validFrom.Format(time.RFC3339),
		validFrom.Format(time.RFC3339),
		validFrom.Add(60*time.Minute).Format(time.RFC3339)))
}

func newPromoter(t *testing.T, now time.Time) (*Promoter, *backtest.InMemoryStrategyConfigRepo, *backtest.InMemoryValidationEventRepo, string, string) {
	t.Helper()
	dir := t.TempDir()
	nextPath := filepath.Join(dir, "next.yaml")
	activePath := filepath.Join(dir, "active.yaml")

	v := config.NewValidator(validHardLimits())
	v.Now = func() time.Time { return now }

	strat := backtest.NewInMemoryStrategyConfigRepo()
	val := backtest.NewInMemoryValidationEventRepo()
	// test wires the real file adapter pointed at the tempdir so the
	// existing assertions on active.yaml / next.yaml file presence still work.
	store := artifact.NewFileStrategyConfigStore(nextPath, activePath)
	p := NewPromoter(v, strat, val, store, config.ModePaperConfig, "auto")
	p.Clock = func() time.Time { return now }
	return p, strat, val, nextPath, activePath
}

func TestPromote_AllPass_RenamesNextToActive(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	p, strat, val, nextPath, activePath := newPromoter(t, now)

	raw := validYAML("cfg-1", now)
	if err := os.WriteFile(nextPath, raw, 0o600); err != nil {
		t.Fatalf("write next.yaml: %v", err)
	}

	res, err := p.PromoteFromYAML(context.Background(), raw, config.AccountState{
		MaxDailyLossJPY:      1000,
		MaxConsecutiveLosses: 3,
		MaxOpenPositions:     2,
	}, "")
	if err != nil {
		t.Fatalf("PromoteFromYAML: %v", err)
	}
	if !res.Promoted {
		t.Fatalf("expected promoted, got reason=%s errs=%s", res.RejectReason, res.Errors.Summary())
	}
	if res.ConfigID != "cfg-1" {
		t.Errorf("ConfigID: %s", res.ConfigID)
	}
	// active.yaml should now hold the YAML
	body, err := os.ReadFile(activePath)
	if err != nil || !strings.Contains(string(body), "cfg-1") {
		t.Errorf("active.yaml: err=%v body=%s", err, body)
	}
	// next.yaml should be cleaned up
	if _, err := os.Stat(nextPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("next.yaml should be removed after promotion, got err=%v", err)
	}
	// strategy_configs got an active row.
	// 旧版は MarkActive を別 SQL で呼んでいたが、ConfigPromoter に統合
	// された。Insert 内で status='active' と同時に activated_at が埋まる。
	// テスト fallback (ConfigPromoter=nil) では Insert のみが呼ばれる。
	rows := strat.Rows()
	if len(rows) != 1 || rows[0].Status != port.StrategyConfigStatusActive {
		t.Errorf("strategy rows: %+v", rows)
	}
	// validation events: schema/hard_limit/risk all pass
	for _, typ := range []string{"schema", "hard_limit", "risk"} {
		if len(val.ByType(typ, "pass")) == 0 {
			t.Errorf("expected %s pass event", typ)
		}
	}
}

func TestPromote_SchemaFail_KeepsActive(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	p, strat, val, _, activePath := newPromoter(t, now)

	// Pre-stage an existing active.yaml.
	prior := []byte("# prior active\n")
	if err := os.WriteFile(activePath, prior, 0o600); err != nil {
		t.Fatalf("seed active.yaml: %v", err)
	}

	bad := []byte("not a yaml: : :")
	res, err := p.PromoteFromYAML(context.Background(), bad, config.AccountState{}, "")
	if err != nil {
		t.Fatalf("PromoteFromYAML: %v", err)
	}
	if res.Promoted {
		t.Fatalf("should not promote")
	}
	body, _ := os.ReadFile(activePath)
	if string(body) != string(prior) {
		t.Errorf("active.yaml mutated: %s", body)
	}
	rows := strat.Rows()
	if len(rows) != 1 || rows[0].Status != port.StrategyConfigStatusRejected {
		t.Errorf("expected rejected row, got %+v", rows)
	}
	if len(val.ByType("schema", "fail")) == 0 {
		t.Errorf("expected schema fail event")
	}
}

func TestPromote_HardLimitFail_KeepsActive(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	p, strat, val, _, activePath := newPromoter(t, now)
	prior := []byte("# prior active\n")
	_ = os.WriteFile(activePath, prior, 0o600)

	// Build a YAML with quantity=999 (outside hard limits 100-100).
	raw := []byte(strings.Replace(string(validYAML("cfg-bad", now)), "quantity: 100", "quantity: 999", 1))
	res, _ := p.PromoteFromYAML(context.Background(), raw, config.AccountState{
		MaxDailyLossJPY:      1000,
		MaxConsecutiveLosses: 3,
		MaxOpenPositions:     2,
	}, "")
	if res.Promoted {
		t.Fatalf("hard-limit violation should not promote")
	}
	body, _ := os.ReadFile(activePath)
	if string(body) != string(prior) {
		t.Errorf("active.yaml should be untouched")
	}
	if len(val.ByType("hard_limit", "fail")) == 0 {
		t.Errorf("expected hard_limit fail event")
	}
	rows := strat.Rows()
	if len(rows) != 1 || rows[0].Status != port.StrategyConfigStatusRejected {
		t.Errorf("expected rejected row, got %+v", rows)
	}
}

// noTradeYAMLWithDirtyDirection reproduces a known LLM output quirk: a textbook
// no_trade decision (enabled=false, strategy.name=no_trade, no_trade.enabled=true,
// all exit/risk zeroed) where Claude left a single vestigial entry.direction:both.
func noTradeYAMLWithDirtyDirection(configID string, validFrom time.Time) []byte {
	return []byte(fmt.Sprintf(`
config_id: "%s"
generated_at: %s
valid_from: %s
valid_until: %s
symbol: USD_JPY
enabled: false
market_regime:
  type: unclear
  confidence: 0.28
  reason: regime unclear
strategy:
  name: no_trade
entry:
  max_spread_pips: 1.5
  require_breakout: false
  direction: both
  max_chase_pips: 0
exit:
  take_profit_pips: 0
  stop_loss_pips: 0
  max_hold_minutes: 0
risk:
  quantity: 0
  max_open_positions: 1
  max_trades_in_this_window: 0
  max_loss_in_this_window_jpy: 0
no_trade:
  enabled: true
  reason: confidence 0.28 < 0.35, all lanes no_trade
`, configID,
		validFrom.Format(time.RFC3339),
		validFrom.Format(time.RFC3339),
		validFrom.Add(60*time.Minute).Format(time.RFC3339)))
}

// Regression: a no_trade decision with a leftover
// entry.direction must NOT be rejected. Before the CanonicalizeNoTrade fix this
// promote returned "[schema] entry.direction: must be 'none' when no_trade",
// the previous config stayed active past its valid_until, and the dashboard
// froze. After the fix the no_trade judgment promotes cleanly.
func TestPromote_NoTradeWithDirtyDirection_Promotes(t *testing.T) {
	now := time.Date(2026, 6, 3, 10, 40, 0, 0, time.UTC)
	p, strat, _, _, _ := newPromoter(t, now)

	raw := noTradeYAMLWithDirtyDirection("nt-1", now)
	res, err := p.PromoteFromYAML(context.Background(), raw, config.AccountState{
		MaxDailyLossJPY:      1000,
		MaxConsecutiveLosses: 3,
		MaxOpenPositions:     2,
	}, "")
	if err != nil {
		t.Fatalf("PromoteFromYAML: %v", err)
	}
	if !res.Promoted {
		t.Fatalf("no_trade config with vestigial direction should promote, got reason=%q errs=%s",
			res.RejectReason, res.Errors.Summary())
	}
	rows := strat.Rows()
	if len(rows) != 1 || rows[0].Status != port.StrategyConfigStatusActive {
		t.Fatalf("expected one active row, got %+v", rows)
	}
	if rows[0].StrategyName != string(config.StrategyNoTrade) || rows[0].Enabled {
		t.Errorf("promoted row should be no_trade/disabled, got name=%q enabled=%v",
			rows[0].StrategyName, rows[0].Enabled)
	}
}

func TestPromote_RiskFail_EmergencyStop(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	p, _, val, _, activePath := newPromoter(t, now)
	prior := []byte("# prior\n")
	_ = os.WriteFile(activePath, prior, 0o600)

	res, _ := p.PromoteFromYAML(context.Background(), validYAML("cfg-2", now), config.AccountState{
		EmergencyStop:        true,
		MaxDailyLossJPY:      1000,
		MaxConsecutiveLosses: 3,
		MaxOpenPositions:     2,
	}, "")
	if res.Promoted {
		t.Fatalf("emergency stop should block")
	}
	if len(val.ByType("risk", "fail")) == 0 {
		t.Errorf("expected risk fail event")
	}
	body, _ := os.ReadFile(activePath)
	if string(body) != string(prior) {
		t.Errorf("active.yaml should be untouched")
	}
}

func TestPromote_ExpiresPreviousActive(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	p, strat, _, nextPath, _ := newPromoter(t, now)

	// Seed an active row directly in the in-memory repo.
	strat.SeedActive("cfg-prev", "USD_JPY", string(config.ModePaperConfig))

	raw := validYAML("cfg-new", now)
	_ = os.WriteFile(nextPath, raw, 0o600)

	res, err := p.PromoteFromYAML(context.Background(), raw, config.AccountState{
		MaxDailyLossJPY: 1000, MaxConsecutiveLosses: 3, MaxOpenPositions: 2,
	}, "")
	if err != nil || !res.Promoted {
		t.Fatalf("expected promoted, err=%v", err)
	}
	expired := strat.ExpiredIDs()
	if len(expired) != 1 || expired[0] != "cfg-prev" {
		t.Errorf("expected cfg-prev expired, got %v", expired)
	}
}

func TestLoadActive_Missing(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	p, _, _, _, _ := newPromoter(t, now)
	got, err := p.LoadActive()
	if err != nil {
		t.Fatalf("LoadActive: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing active.yaml")
	}
}

func TestLoadActive_ReturnsParsedConfig(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	p, _, _, _, activePath := newPromoter(t, now)
	if err := os.WriteFile(activePath, validYAML("cfg-x", now), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := p.LoadActive()
	if err != nil {
		t.Fatalf("LoadActive: %v", err)
	}
	if got.ConfigID != "cfg-x" || got.Symbol != "USD_JPY" {
		t.Errorf("loaded: %+v", got)
	}
}

// DB is authoritative for startup loads. LoadActiveFromDB parses the
// raw_yaml off the strategy_configs row with status='active'. YAML on disk is
// irrelevant for this path — even if it's stale or missing, the DB wins.
func TestLoadActiveFromDB_ReturnsParsedFromDBNotYAML(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	p, strat, _, _, activePath := newPromoter(t, now)

	// DB has cfg-from-db active; YAML on disk has cfg-from-yaml (= stale).
	_ = strat.Insert(context.Background(), port.StrategyConfigRecord{
		ConfigID: "cfg-from-db", Source: "auto", Mode: string(config.ModePaperConfig),
		Symbol: "USD_JPY", Enabled: true, StrategyName: "momentum_pullback",
		ValidFrom: now, ValidUntil: now.Add(time.Hour),
		RawYAML: string(validYAML("cfg-from-db", now)),
		Status:  port.StrategyConfigStatusActive,
	})
	if err := os.WriteFile(activePath, validYAML("cfg-from-yaml", now), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	got, err := p.LoadActiveFromDB(context.Background(), "USD_JPY")
	if err != nil {
		t.Fatalf("LoadActiveFromDB: %v", err)
	}
	if got == nil || got.ConfigID != "cfg-from-db" {
		t.Errorf("DB must win; got %+v", got)
	}
}

func TestLoadActiveFromDB_NilWhenNoActiveRow(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	p, _, _, _, _ := newPromoter(t, now)
	got, err := p.LoadActiveFromDB(context.Background(), "USD_JPY")
	if err != nil {
		t.Fatalf("LoadActiveFromDB: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil; got %+v", got)
	}
}

// --- ExpectedSymbol guard ---
// Multi-symbol bundles each pin their Promoter to one symbol so Claude
// returning a YAML for the wrong symbol (hallucination / context mix-up)
// gets rejected before mutating active config.

func TestPromote_ExpectedSymbol_PromotionGate(t *testing.T) {
	cases := []struct {
		name         string
		expectedSym  string // "" = guard disabled
		wantPromoted bool
	}{
		{"match → promoted", "USD_JPY", true},
		{"empty pin → guard skipped, promoted", "", true},
		{"mismatch → blocked", "EUR_JPY", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
			p, _, _, nextPath, _ := newPromoter(t, now)
			p.ExpectedSymbol = tc.expectedSym

			raw := validYAML("cfg-"+tc.name, now)
			_ = os.WriteFile(nextPath, raw, 0o600)
			res, err := p.PromoteFromYAML(context.Background(), raw, config.AccountState{
				MaxDailyLossJPY: 1000, MaxConsecutiveLosses: 3, MaxOpenPositions: 2,
			}, "")
			if err != nil {
				t.Fatalf("PromoteFromYAML: %v", err)
			}
			if res.Promoted != tc.wantPromoted {
				t.Fatalf("promoted=%v, want %v (reason=%q)",
					res.Promoted, tc.wantPromoted, res.RejectReason)
			}
		})
	}
}

func TestPromote_ExpectedSymbol_MismatchAuditAndArtifactSafety(t *testing.T) {
	// mismatch ケースは promotion blocked に加え、(1) reject reason に
	// symbol_mismatch を含む (2) 監査用 rejected row が立つ (3) active.yaml
	// が触られない — の 3 点を担保する必要がある。
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	p, strat, _, _, activePath := newPromoter(t, now)
	p.ExpectedSymbol = "EUR_JPY" // YAML は USD_JPY

	prior := []byte("# prior active\n")
	_ = os.WriteFile(activePath, prior, 0o600)

	raw := validYAML("cfg-wrong-sym", now)
	res, _ := p.PromoteFromYAML(context.Background(), raw, config.AccountState{
		MaxDailyLossJPY: 1000, MaxConsecutiveLosses: 3, MaxOpenPositions: 2,
	}, "")

	if !strings.Contains(res.RejectReason, "symbol_mismatch") {
		t.Errorf("RejectReason: %q", res.RejectReason)
	}
	rows := strat.Rows()
	if len(rows) != 1 || rows[0].Status != port.StrategyConfigStatusRejected {
		t.Errorf("rejected row: %+v", rows)
	}
	if body, _ := os.ReadFile(activePath); string(body) != string(prior) {
		t.Errorf("active.yaml mutated on symbol mismatch: %s", body)
	}
}
