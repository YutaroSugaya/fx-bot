package config

import (
	"strings"
	"testing"
)

// Per-currency entry discipline: the three loss-pattern vetoes (night BUY / high-chase BUY /
// sell-low) are config-driven so the operator can tune or disable them per pair without a rebuild. This locks the yaml wiring: the
// fields must round-trip from bot_config.yaml into LLMDecisionSection.
func TestLoadBotConfig_ParsesPerCurrencyEntryVetoes(t *testing.T) {
	body := validBotConfigYAML + `
llm_decision:
  enabled: true
  symbols: [USD_JPY, GBP_JPY, GBP_USD]
  night_buy_veto_hours_jst: [0, 1, 2, 3, 4, 5]
  max_range_position_24h_buy:
    USD_JPY: 0.85
    GBP_JPY: 0.5
  min_range_position_24h_sell:
    GBP_USD: 0.15
`
	path := writeTempYAML(t, body)
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	lc := cfg.LLMDecision
	if got := lc.NightBuyVetoHoursJST; len(got) != 6 || got[0] != 0 || got[5] != 5 {
		t.Errorf("NightBuyVetoHoursJST = %v, want [0 1 2 3 4 5]", got)
	}
	if got := lc.MaxRangePos24hBuy["GBP_JPY"]; got != 0.5 {
		t.Errorf("MaxRangePos24hBuy[GBP_JPY] = %v, want 0.5", got)
	}
	if got := lc.MaxRangePos24hBuy["USD_JPY"]; got != 0.85 {
		t.Errorf("MaxRangePos24hBuy[USD_JPY] = %v, want 0.85", got)
	}
	// Unset pair → zero value = veto OFF for that pair.
	if got := lc.MaxRangePos24hBuy["EUR_USD"]; got != 0 {
		t.Errorf("MaxRangePos24hBuy[EUR_USD] = %v, want 0 (off)", got)
	}
	if got := lc.MinRangePos24hSell["GBP_USD"]; got != 0.15 {
		t.Errorf("MinRangePos24hSell[GBP_USD] = %v, want 0.15", got)
	}
	// USD_JPY sell-low must stay OFF unless explicitly configured (its sell-low
	// continuation was the least-bad pattern in the trade-history analysis — never default-ban it).
	if got := lc.MinRangePos24hSell["USD_JPY"]; got != 0 {
		t.Errorf("MinRangePos24hSell[USD_JPY] = %v, want 0 (off)", got)
	}
}

// Load-time validation: a typo'd veto config must fail LOUDLY at boot, not silently disable the
// gate (an hour of 24 never matches jstHourIn; an rpos ceiling of 1.5 can never trip — either
// typo would leave the operator believing a protection is on when it is a no-op).
func TestLoadBotConfig_RejectsOutOfRangeEntryVetoes(t *testing.T) {
	cases := []struct {
		name    string
		block   string
		wantErr string
	}{
		{
			"night hour out of 0-23",
			"\nllm_decision:\n  enabled: true\n  symbols: [USD_JPY]\n  night_buy_veto_hours_jst: [0, 24]\n",
			"night_buy_veto_hours_jst",
		},
		{
			"BUY rpos ceiling above 1",
			"\nllm_decision:\n  enabled: true\n  symbols: [USD_JPY]\n  max_range_position_24h_buy:\n    USD_JPY: 1.5\n",
			"max_range_position_24h_buy",
		},
		{
			"BUY rpos ceiling negative",
			"\nllm_decision:\n  enabled: true\n  symbols: [USD_JPY]\n  max_range_position_24h_buy:\n    USD_JPY: -0.1\n",
			"max_range_position_24h_buy",
		},
		{
			"SELL rpos floor above 1",
			"\nllm_decision:\n  enabled: true\n  symbols: [GBP_USD]\n  min_range_position_24h_sell:\n    GBP_USD: 15\n",
			"min_range_position_24h_sell",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempYAML(t, validBotConfigYAML+tc.block)
			_, err := LoadBotConfig(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want load error mentioning %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// A 0 value in the rpos maps is an explicit OFF and must stay loadable (back-compat with the
// "0 = OFF" convention used by every other optional guard).
func TestLoadBotConfig_EntryVetoZeroIsExplicitOff(t *testing.T) {
	body := validBotConfigYAML + `
llm_decision:
  enabled: true
  symbols: [USD_JPY]
  max_range_position_24h_buy:
    USD_JPY: 0
`
	if _, err := LoadBotConfig(writeTempYAML(t, body)); err != nil {
		t.Fatalf("explicit 0 (= OFF) must load: %v", err)
	}
}

// Omitting the new fields must stay fully back-compatible (all vetoes OFF).
func TestLoadBotConfig_EntryVetoesDefaultOff(t *testing.T) {
	body := validBotConfigYAML + `
llm_decision:
  enabled: true
  symbols: [USD_JPY]
`
	path := writeTempYAML(t, body)
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	lc := cfg.LLMDecision
	if len(lc.NightBuyVetoHoursJST) != 0 || len(lc.MaxRangePos24hBuy) != 0 || len(lc.MinRangePos24hSell) != 0 {
		t.Errorf("omitted veto fields must default off, got %+v %+v %+v",
			lc.NightBuyVetoHoursJST, lc.MaxRangePos24hBuy, lc.MinRangePos24hSell)
	}
}

// ① MTF veto discipline exemption: the htf_trend_veto would otherwise structurally block
// the per-currency discipline's own "やる" patterns (positioned 戻りSELL
// from the 24h top / genuine pullback BUY). The exemption bounds are PER-PAIR maps (same
// shape as max_range_position_24h_buy) because the discipline defines different pullback zones per
// pair — GBP_JPY's verified BUY zone is rpos<0.4 while USD_JPY's is ≤0.5; a single global
// bound would open GBP_JPY's unverified (0.4, 0.5] band. Must round-trip from yaml; a
// typo'd bound must fail loudly at boot (an exempt rpos of 60 instead of 0.60 would waive
// the veto for EVERY sell); omitted pair = 0 = no exemption there (pre-change veto).
func TestLoadBotConfig_ParsesHTFVetoDisciplineExemption(t *testing.T) {
	body := validBotConfigYAML + `
llm_decision:
  enabled: true
  symbols: [USD_JPY, GBP_JPY]
  htf_trend_veto_pips: 12
  htf_trend_veto_exempt_sell_rpos:
    USD_JPY: 0.6
    GBP_JPY: 0.6
  htf_trend_veto_exempt_buy_rpos:
    USD_JPY: 0.5
    GBP_JPY: 0.4
`
	cfg, err := LoadBotConfig(writeTempYAML(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	lc := cfg.LLMDecision
	if got := lc.HTFTrendVetoExemptSellRpos["USD_JPY"]; got != 0.6 {
		t.Errorf("ExemptSellRpos[USD_JPY] = %v, want 0.6", got)
	}
	if got := lc.HTFTrendVetoExemptBuyRpos["GBP_JPY"]; got != 0.4 {
		t.Errorf("ExemptBuyRpos[GBP_JPY] = %v, want 0.4 (the pair's verified zone, NOT 0.5)", got)
	}
	// Unset pair → zero value = no exemption for that pair (the veto stands).
	if got := lc.HTFTrendVetoExemptBuyRpos["EUR_JPY"]; got != 0 {
		t.Errorf("ExemptBuyRpos[EUR_JPY] = %v, want 0 (off)", got)
	}
}

func TestLoadBotConfig_RejectsOutOfRangeHTFVetoExemption(t *testing.T) {
	cases := []struct {
		name    string
		block   string
		wantErr string
	}{
		{
			"sell exempt above 1",
			"\nllm_decision:\n  enabled: true\n  symbols: [USD_JPY]\n  htf_trend_veto_exempt_sell_rpos:\n    USD_JPY: 60\n",
			"htf_trend_veto_exempt_sell_rpos",
		},
		{
			"buy exempt negative",
			"\nllm_decision:\n  enabled: true\n  symbols: [USD_JPY]\n  htf_trend_veto_exempt_buy_rpos:\n    USD_JPY: -0.1\n",
			"htf_trend_veto_exempt_buy_rpos",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBotConfig(writeTempYAML(t, validBotConfigYAML+tc.block))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want load error mentioning %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoadBotConfig_HTFVetoExemptionDefaultsOff(t *testing.T) {
	body := validBotConfigYAML + `
llm_decision:
  enabled: true
  symbols: [USD_JPY]
`
	cfg, err := LoadBotConfig(writeTempYAML(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.LLMDecision.HTFTrendVetoExemptSellRpos) != 0 || len(cfg.LLMDecision.HTFTrendVetoExemptBuyRpos) != 0 {
		t.Errorf("omitted exemption must default empty (off), got %+v", cfg.LLMDecision)
	}
}

// Lane-rulebook vetoes: the playbook's HARD bans enforced in code — exhaustion
// (same-direction chase after the 24h move already ran ≥ N pips) and spike cooldown (any
// entry while |15m move| ≥ N pips). Config-driven; 0/omitted = OFF (back-compat).
func TestLoadBotConfig_ParsesV8Vetoes(t *testing.T) {
	body := validBotConfigYAML + `
llm_decision:
  enabled: true
  symbols: [USD_JPY]
  exhaustion_veto_pips: 120
  spike_veto_pips_15m: 15
  daily_loss_stop_count: 2
  arm_enabled: true
  arm_max_distance_pips: 30
`
	path := writeTempYAML(t, body)
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.LLMDecision.ExhaustionVetoPips; got != 120 {
		t.Errorf("ExhaustionVetoPips = %v, want 120", got)
	}
	if got := cfg.LLMDecision.SpikeVetoPips15m; got != 15 {
		t.Errorf("SpikeVetoPips15m = %v, want 15", got)
	}
	if got := cfg.LLMDecision.DailyLossStopCount; got != 2 {
		t.Errorf("DailyLossStopCount = %v, want 2", got)
	}
	if !cfg.LLMDecision.ArmEnabled || cfg.LLMDecision.ArmMaxDistancePips != 30 {
		t.Errorf("arm config: enabled=%v dist=%v", cfg.LLMDecision.ArmEnabled, cfg.LLMDecision.ArmMaxDistancePips)
	}
	// Omitted → 0 = OFF.
	body2 := validBotConfigYAML + "\nllm_decision:\n  enabled: true\n  symbols: [USD_JPY]\n"
	cfg2, err := LoadBotConfig(writeTempYAML(t, body2))
	if err != nil {
		t.Fatalf("load2: %v", err)
	}
	if cfg2.LLMDecision.ArmEnabled {
		t.Error("arm_enabled must default OFF")
	}
	if cfg2.LLMDecision.ExhaustionVetoPips != 0 || cfg2.LLMDecision.SpikeVetoPips15m != 0 || cfg2.LLMDecision.DailyLossStopCount != 0 {
		t.Errorf("omitted v8 vetoes must default OFF, got %v/%v/%d",
			cfg2.LLMDecision.ExhaustionVetoPips, cfg2.LLMDecision.SpikeVetoPips15m, cfg2.LLMDecision.DailyLossStopCount)
	}
}
