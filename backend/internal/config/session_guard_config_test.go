package config

import (
	"strings"
	"testing"
)

// セッションガードの config 配線を固定する。
//   - llm_decision.no_entry_hours_jst : 全 side の新規/arm_fire 禁止時間帯 (02-05 = 05:45 スプレッド壁への滑走路なし建玉の排除)
//   - llm_decision.session_flatten_jst: 毎朝の全玉強制 flatten 時刻 ("05:30"、壁の 15 分前)
//   - risk.reentry_cooldown_minutes   : 同 symbol 同 side の決済後クールダウン (利確直後の同方向即再 IN を防ぐ)
func TestLoadBotConfig_ParsesSessionGuardFields(t *testing.T) {
	body := validBotConfigYAML + `
llm_decision:
  enabled: true
  symbols: [USD_JPY]
  no_entry_hours_jst: [2, 3, 4, 5]
  session_flatten_jst: "05:30"
`
	body = strings.Replace(body, "risk:\n", "risk:\n  reentry_cooldown_minutes: 60\n", 1)
	path := writeTempYAML(t, body)
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.LLMDecision.NoEntryHoursJST; len(got) != 4 || got[0] != 2 || got[3] != 5 {
		t.Errorf("NoEntryHoursJST = %v, want [2 3 4 5]", got)
	}
	if got := cfg.Risk.ReentryCooldownMinutes; got != 60 {
		t.Errorf("ReentryCooldownMinutes = %d, want 60", got)
	}
	minutes, ok := cfg.LLMDecision.SessionFlattenMinutesJST()
	if !ok || minutes != 5*60+30 {
		t.Errorf("SessionFlattenMinutesJST() = (%d,%v), want (330,true)", minutes, ok)
	}
}

// 未設定 = 全部 OFF (後方互換: 旧 yaml がそのまま起動できる)。
func TestLoadBotConfig_SessionGuardDefaultsOff(t *testing.T) {
	path := writeTempYAML(t, validBotConfigYAML)
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.LLMDecision.NoEntryHoursJST; len(got) != 0 {
		t.Errorf("NoEntryHoursJST default = %v, want empty (off)", got)
	}
	if got := cfg.Risk.ReentryCooldownMinutes; got != 0 {
		t.Errorf("ReentryCooldownMinutes default = %d, want 0 (off)", got)
	}
	if _, ok := cfg.LLMDecision.SessionFlattenMinutesJST(); ok {
		t.Errorf("SessionFlattenMinutesJST() must be off when unset")
	}
}

// typo は起動時に LOUD に落とす (night_buy_veto と同じ原則: 黙って無効化される
// 「効いてるつもりの保護」を作らない)。
func TestLoadBotConfig_RejectsInvalidSessionGuardValues(t *testing.T) {
	cases := []struct {
		name    string
		block   string
		wantErr string
	}{
		{
			name: "no_entry_hour_out_of_range",
			block: `
llm_decision:
  enabled: true
  symbols: [USD_JPY]
  no_entry_hours_jst: [2, 24]
`,
			wantErr: "no_entry_hours_jst",
		},
		{
			name: "flatten_bad_format",
			block: `
llm_decision:
  enabled: true
  symbols: [USD_JPY]
  session_flatten_jst: "0530"
`,
			wantErr: "session_flatten_jst",
		},
		{
			name: "flatten_bad_minute",
			block: `
llm_decision:
  enabled: true
  symbols: [USD_JPY]
  session_flatten_jst: "05:75"
`,
			wantErr: "session_flatten_jst",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeTempYAML(t, validBotConfigYAML+c.block)
			_, err := LoadBotConfig(path)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("want boot error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestLoadBotConfig_RejectsNegativeReentryCooldown(t *testing.T) {
	body := strings.Replace(validBotConfigYAML, "risk:\n", "risk:\n  reentry_cooldown_minutes: -5\n", 1)
	path := writeTempYAML(t, body)
	_, err := LoadBotConfig(path)
	if err == nil || !strings.Contains(err.Error(), "reentry_cooldown_minutes") {
		t.Errorf("want boot error containing reentry_cooldown_minutes, got %v", err)
	}
}
