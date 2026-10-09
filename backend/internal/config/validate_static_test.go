package config

import "testing"

// ValidateStatic runs the config-quality passes
// (schema + hard_limit + semantic) WITHOUT the runtime ValidateRisk pass, so it
// can run at startup (LoadActiveFromDB) to fail-close on a structurally-invalid
// active config independent of the live account state (emergency_stop /
// daily_loss legitimately vary across restarts and must NOT brick startup).
func TestValidateStatic_FrozenStrategyComputedConfigPasses(t *testing.T) {
	v := NewValidator(testHardLimits(t))
	cfg := strategyComputedCfg() // ma_pullback, valid_until 2030, max_trades 0, TP14 placeholder
	if res := v.ValidateStatic(cfg, "ma_pullback", "no_trade"); !res.OK() {
		t.Fatalf("frozen strategy_computed config must pass ValidateStatic; got: %s", res.Summary())
	}
}

func TestValidateStatic_ExcludesRuntimeRiskPass(t *testing.T) {
	// Even if (hypothetically) account state were bad, ValidateStatic must not
	// see it — it takes no AccountState. A config that is structurally fine
	// passes regardless of emergency_stop being active live.
	v := NewValidator(testHardLimits(t))
	cfg := strategyComputedCfg()
	res := v.ValidateStatic(cfg, "ma_pullback")
	for _, e := range res.Errors {
		if e.Type == ValidationRisk {
			t.Errorf("ValidateStatic must not run the runtime risk pass; got risk error %s", e.Error())
		}
	}
}

func TestValidateStatic_CatchesStructurallyInvalid(t *testing.T) {
	v := NewValidator(testHardLimits(t))
	cfg := strategyComputedCfg()
	cfg.Symbol = "" // schema: required field missing
	if res := v.ValidateStatic(cfg, "ma_pullback"); res.OK() {
		t.Error("ValidateStatic must reject a config missing a required field")
	}
}
