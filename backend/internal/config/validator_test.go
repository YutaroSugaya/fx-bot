package config

import (
	"strings"
	"testing"
	"time"
)

// fixture helpers --------------------------------------------------------

func validHardLimits() *HardLimits {
	return &HardLimits{
		AllowedSymbols:         []string{"USD_JPY"},
		Quantity:               IntRange{Min: 100, Max: 100},
		TakeProfitPips:         FloatRange{Min: 15.0, Max: 50.0},
		StopLossPips:           FloatRange{Min: 15.0, Max: 30.0},
		MaxHoldMinutes:         IntRange{Min: 240, Max: 360},
		MaxTradesInThisWindow:  IntRange{Min: 0, Max: 3},
		MaxLossInThisWindowJPY: IntRange{Min: 0, Max: 5000},
		MaxSpreadPips:          FloatRange{Min: 0.3, Max: 1.0},
		ConfigTTLMinutes:       IntRange{Min: 60, Max: 120},
	}
}

func validStrategyConfig(now time.Time) *StrategyConfig {
	return &StrategyConfig{
		ConfigID:    "20260515-100000-usdjpy",
		GeneratedAt: now,
		ValidFrom:   now,
		ValidUntil:  now.Add(60 * time.Minute),
		Symbol:      "USD_JPY",
		Enabled:     true,
		MarketRegime: MarketRegime{
			Type:       RegimeRange,
			Confidence: 0.7,
			Reason:     "test",
		},
		Strategy: StrategySection{
			Name: StrategyMomentumPullback,
		},
		Entry: EntrySection{
			MaxSpreadPips:   0.5,
			RequireBreakout: false,
			Direction:       DirectionBoth,
		},
		Exit: ExitSection{
			TakeProfitPips: 30.0,
			StopLossPips:   20.0,
			MaxHoldMinutes: 240,
			// 下落の歯止め + ratchet 必須 + RR floor: enabled config では early_exit /
			// ratchet を必ず埋め、locked = arm-giveback ≥ TP×0.5 を満たす。
			// TP=30 なので locked ≥ 15 が必要 → arm=20/give=3 (locked 17)。
			EarlyExitWindowMinutes: 30,
			EarlyExitTargetPips:    -2,
			RatchetArmPips:         20,
			RatchetGivebackPips:    3,
		},
		Risk: ConfigRiskSection{
			Quantity:               100,
			MaxOpenPositions:       1,
			MaxTradesInThisWindow:  2,
			MaxLossInThisWindowJPY: 3000,
		},
		NoTrade: NoTradeSection{Enabled: false},
	}
}

func validNoTradeConfig(now time.Time) *StrategyConfig {
	c := validStrategyConfig(now)
	c.Enabled = false
	c.Strategy.Name = StrategyNoTrade
	c.Entry.Direction = DirectionNone
	c.Exit = ExitSection{}
	c.Risk.Quantity = 0
	c.NoTrade = NoTradeSection{Enabled: true, Reason: "uncertain"}
	return c
}

func newValidator(now time.Time) *Validator {
	v := NewValidator(validHardLimits())
	v.Now = func() time.Time { return now }
	return v
}

func errorsContain(errs []ValidationError, needle string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), needle) {
			return true
		}
	}
	return false
}

// schema -----------------------------------------------------------------

func TestValidateSchema_OK(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	// 下落の歯止め + ratchet 必須 + RR floor: enabled fixture は early_exit / ratchet を
	// 埋め、locked = arm-give ≥ TP×0.5 (TP30 → 15) も満たすこと。
	c.Exit.EarlyExitWindowMinutes = 30
	c.Exit.EarlyExitTargetPips = -2
	c.Exit.RatchetArmPips = 20
	c.Exit.RatchetGivebackPips = 3
	r := v.ValidateSchema(c)
	if !r.OK() {
		t.Fatalf("expected ok, got: %s", r.Summary())
	}
}

// 低確度 trade を no_trade に倒す Go-side guard。
// skill 02 で「confidence < 0.35 で no_trade」と書いてあるが、Claude が
// 違反したときの sanity check として Go validator でも reject する。
func TestValidateSchema_A7_RejectsLowConfidenceEnabled(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.MarketRegime.Confidence = 0.30 // < 0.35
	c.Exit.EarlyExitWindowMinutes = 30
	c.Exit.EarlyExitTargetPips = -2
	c.Exit.RatchetArmPips = 5
	c.Exit.RatchetGivebackPips = 3
	r := v.ValidateSchema(c)
	if r.OK() {
		t.Fatalf("confidence 0.30 < 0.35 で enabled config は reject されるべき")
	}
	if !errorsContain(r.Errors, "confidence") {
		t.Errorf("reason should mention confidence: %s", r.Summary())
	}
}

// no_trade config は confidence 低くても OK (= 既に no_trade している)。
func TestValidateSchema_A7_NoTradeExemptFromConfidenceFloor(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validNoTradeConfig(now)
	c.MarketRegime.Confidence = 0.10 // 超低確度
	r := v.ValidateSchema(c)
	if !r.OK() {
		t.Errorf("no_trade config should not be rejected for low confidence: %s", r.Summary())
	}
}

// 下落の歯止め: early_exit OFF は ratchet ON なら許可する。
// trend tier は early_exit を切って ratchet 主体で利を伸ばす設計のため。
// 下落の歯止めは early_exit / ratchet の少なくとも一方があれば足りる。
func TestValidateSchema_A4_AllowsEarlyExitOffWhenRatchetOn(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.MarketRegime.Confidence = 0.7
	c.Exit.EarlyExitWindowMinutes = 0 // OFF
	c.Exit.EarlyExitTargetPips = 0
	// ratchet ON かつ RR floor (locked = arm-give ≥ TP×0.5 = 15) を満たす。
	c.Exit.RatchetArmPips = 20
	c.Exit.RatchetGivebackPips = 3
	r := v.ValidateSchema(c)
	if !r.OK() {
		t.Fatalf("early_exit OFF + ratchet ON は許可されるべき: %s", r.Summary())
	}
}

// early_exit と ratchet が両方 OFF = 下落の歯止めゼロ → reject。
func TestValidateSchema_A4_RejectsBothDownsideCapsOff(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.MarketRegime.Confidence = 0.7
	c.Exit.EarlyExitWindowMinutes = 0
	c.Exit.EarlyExitTargetPips = 0
	c.Exit.RatchetArmPips = 0
	c.Exit.RatchetGivebackPips = 0
	r := v.ValidateSchema(c)
	if r.OK() {
		t.Fatalf("early_exit / ratchet 両 OFF は reject されるべき")
	}
}

// no_trade は early_exit OFF 必須 (= 既存仕様)。
func TestValidateSchema_A4_NoTradeStillRequiresEarlyExitZero(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validNoTradeConfig(now)
	// no_trade config は exit すべて 0 で OK
	r := v.ValidateSchema(c)
	if !r.OK() {
		t.Errorf("no_trade with early_exit=0 should pass: %s", r.Summary())
	}
}

// ratchet 0/0 OFF 禁止 (enabled config のみ)。
// peak から戻ったら確定で「利益取り逃し」を減らす方針を強制。
func TestValidateSchema_B4_RejectsRatchetOffOnEnabledConfig(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.MarketRegime.Confidence = 0.7
	c.Exit.EarlyExitWindowMinutes = 30
	c.Exit.EarlyExitTargetPips = -2
	c.Exit.RatchetArmPips = 0 // OFF — should reject
	c.Exit.RatchetGivebackPips = 0
	r := v.ValidateSchema(c)
	if r.OK() {
		t.Fatalf("ratchet 0/0 (OFF) は enabled config では reject されるべき")
	}
	if !errorsContain(r.Errors, "ratchet") {
		t.Errorf("reason should mention ratchet: %s", r.Summary())
	}
}

// exhaustion_fade is a structural-exit strategy (構造的 SL = 急騰高値の外 +
// 固定 TP = 走り幅の give-back + タイムストップ、ratchet/early_exit なし) like
// daily_trend. A live config with ratchet 0/0 + early_exit 0 must VALIDATE so it
// is promotable.
func TestValidateSchema_ExhaustionFade_StructuralExitsAllowed(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyExhaustionFade
	c.Exit.ExitPolicy = ExitPolicyStrategyComputed
	c.Entry.AllowedHoursJST = []int{4, 10, 11}
	c.Exit.EarlyExitWindowMinutes = 0
	c.Exit.EarlyExitTargetPips = 0
	c.Exit.RatchetArmPips = 0
	c.Exit.RatchetGivebackPips = 0
	r := v.ValidateSchema(c)
	if !r.OK() {
		t.Fatalf("exhaustion_fade strategy_computed (ratchet/early_exit OFF) should validate: %s", r.Summary())
	}
}

func TestValidateSchema_NilConfig(t *testing.T) {
	v := newValidator(time.Now())
	r := v.ValidateSchema(nil)
	if r.OK() {
		t.Fatalf("nil config should fail")
	}
	if !errorsContain(r.Errors, "config is nil") {
		t.Errorf("got: %v", r.Errors)
	}
}

func TestValidateSchema_MissingRequiredFields(t *testing.T) {
	now := time.Now()
	cases := map[string]func(*StrategyConfig){
		"config_id":   func(c *StrategyConfig) { c.ConfigID = "" },
		"symbol":      func(c *StrategyConfig) { c.Symbol = "" },
		"valid_from":  func(c *StrategyConfig) { c.ValidFrom = time.Time{} },
		"valid_until": func(c *StrategyConfig) { c.ValidUntil = time.Time{} },
	}
	for field, mutate := range cases {
		t.Run(field, func(t *testing.T) {
			c := validStrategyConfig(now)
			mutate(c)
			v := newValidator(now)
			r := v.ValidateSchema(c)
			if r.OK() {
				t.Fatalf("expected fail for missing %s", field)
			}
			if !errorsContain(r.Errors, field) {
				t.Errorf("expected error to mention %s, got: %s", field, r.Summary())
			}
		})
	}
}

func TestValidateSchema_ValidUntilBeforeFrom(t *testing.T) {
	now := time.Now()
	c := validStrategyConfig(now)
	c.ValidUntil = c.ValidFrom.Add(-1 * time.Minute)
	v := newValidator(now)
	r := v.ValidateSchema(c)
	if r.OK() || !errorsContain(r.Errors, "must be after valid_from") {
		t.Fatalf("expected valid_until error, got: %s", r.Summary())
	}
}

func TestValidateSchema_InvalidEnums(t *testing.T) {
	now := time.Now()
	cases := map[string]func(*StrategyConfig){
		"market_regime.type": func(c *StrategyConfig) { c.MarketRegime.Type = "bullish" },
		"strategy.name":      func(c *StrategyConfig) { c.Strategy.Name = "moon_shot" },
		"entry.direction":    func(c *StrategyConfig) { c.Entry.Direction = "all_in" },
	}
	for field, mutate := range cases {
		t.Run(field, func(t *testing.T) {
			c := validStrategyConfig(now)
			mutate(c)
			v := newValidator(now)
			r := v.ValidateSchema(c)
			if r.OK() || !errorsContain(r.Errors, field) {
				t.Errorf("expected %s error, got: %s", field, r.Summary())
			}
		})
	}
}

func TestValidateSchema_AllowedHoursJST(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name    string
		hours   []int
		wantOK  bool
		wantMsg string // substring expected in error if !wantOK
	}{
		{"nil = back-compat ok", nil, true, ""},
		{"empty slice = ok", []int{}, true, ""},
		{"valid subset", []int{0, 2, 8, 17, 18, 19}, true, ""},
		{"all 24 ok", []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23}, true, ""},
		{"out of range high", []int{24}, false, "outside [0,23]"},
		{"out of range negative", []int{-1, 5}, false, "outside [0,23]"},
		{"duplicate", []int{8, 8}, false, "duplicate hour 8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validStrategyConfig(now)
			c.Entry.AllowedHoursJST = tc.hours
			v := newValidator(now)
			r := v.ValidateSchema(c)
			if tc.wantOK && !r.OK() {
				t.Errorf("expected ok, got: %s", r.Summary())
			}
			if !tc.wantOK {
				if r.OK() {
					t.Fatal("expected validation failure")
				}
				if !errorsContain(r.Errors, tc.wantMsg) {
					t.Errorf("want msg %q, got: %s", tc.wantMsg, r.Summary())
				}
			}
		})
	}
}

// 追いかけ防止フィルタの sanity 範囲。両方 0 = 無効 (back-compat)。
func TestValidateSchema_ChaseFilter(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name     string
		maxChase float64
		lookback int
		wantOK   bool
		wantMsg  string
	}{
		{"both 0 = disabled ok", 0, 0, true, ""},
		{"valid 12/12", 12, 12, true, ""},
		{"negative max_chase", -1, 12, false, "max_chase_pips"},
		{"negative lookback", 12, -1, false, "chase_lookback_candles"},
		{"max_chase too large (typo guard)", 200, 12, false, "max_chase_pips"},
		{"lookback too large", 12, 1000, false, "chase_lookback_candles"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validStrategyConfig(now)
			c.Entry.MaxChasePips = tc.maxChase
			c.Entry.ChaseLookbackCandles = tc.lookback
			v := newValidator(now)
			r := v.ValidateSchema(c)
			if tc.wantOK && !r.OK() {
				t.Errorf("expected ok, got: %s", r.Summary())
			}
			if !tc.wantOK {
				if r.OK() {
					t.Fatal("expected validation failure")
				}
				if !errorsContain(r.Errors, tc.wantMsg) {
					t.Errorf("want msg %q, got: %s", tc.wantMsg, r.Summary())
				}
			}
		})
	}
}

// next_advisor_run_in_minutes の下限は 10。急変・指標・
// volatile 帯で 10 分間隔の高頻度再評価を許可するため。
func TestValidateSchema_NextAdvisorRunMinutes(t *testing.T) {
	now := time.Now()
	cases := []struct {
		min    int
		wantOK bool
	}{
		{0, true},   // fallback
		{10, true},  // 新下限 (急変帯)
		{15, true},  // 指標帯
		{30, true},  // 平時
		{480, true}, // 上限
		{9, false},  // 下限未満
		{481, false},
	}
	for _, tc := range cases {
		c := validStrategyConfig(now)
		c.NextAdvisorRunInMinutes = tc.min
		v := newValidator(now)
		r := v.ValidateSchema(c)
		if got := r.OK(); got != tc.wantOK {
			t.Errorf("min=%d: OK=%v want %v (%s)", tc.min, got, tc.wantOK, r.Summary())
		}
	}
}

func TestValidateSchema_NoTradeConsistency(t *testing.T) {
	now := time.Now()

	t.Run("no_trade.enabled but strategy.name != no_trade", func(t *testing.T) {
		c := validNoTradeConfig(now)
		c.Strategy.Name = StrategyMomentumPullback
		v := newValidator(now)
		r := v.ValidateSchema(c)
		if r.OK() || !errorsContain(r.Errors, "strategy.name") {
			t.Errorf("got: %s", r.Summary())
		}
	})

	t.Run("no_trade but direction != none", func(t *testing.T) {
		c := validNoTradeConfig(now)
		c.Entry.Direction = DirectionBoth
		v := newValidator(now)
		r := v.ValidateSchema(c)
		if r.OK() || !errorsContain(r.Errors, "entry.direction") {
			t.Errorf("got: %s", r.Summary())
		}
	})

	t.Run("no_trade but quantity != 0", func(t *testing.T) {
		c := validNoTradeConfig(now)
		c.Risk.Quantity = 100
		v := newValidator(now)
		r := v.ValidateSchema(c)
		if r.OK() || !errorsContain(r.Errors, "risk.quantity") {
			t.Errorf("got: %s", r.Summary())
		}
	})

	t.Run("no_trade but non-zero exit", func(t *testing.T) {
		c := validNoTradeConfig(now)
		c.Exit.TakeProfitPips = 1.0
		v := newValidator(now)
		r := v.ValidateSchema(c)
		if r.OK() || !errorsContain(r.Errors, "exit") {
			t.Errorf("got: %s", r.Summary())
		}
	})

	t.Run("enabled=false implicitly no_trade but strategy.name not no_trade", func(t *testing.T) {
		c := validStrategyConfig(now)
		c.Enabled = false
		v := newValidator(now)
		r := v.ValidateSchema(c)
		if r.OK() || !errorsContain(r.Errors, "strategy.name") {
			t.Errorf("got: %s", r.Summary())
		}
	})

	t.Run("pure no_trade config is valid", func(t *testing.T) {
		c := validNoTradeConfig(now)
		v := newValidator(now)
		r := v.ValidateSchema(c)
		if !r.OK() {
			t.Errorf("pure no_trade should be valid, got: %s", r.Summary())
		}
	})
}

// Early-exit window validation. Default 0/0 = disabled; valid
// non-zero combos must be inside sane bounds so a typo can't lock in
// unbounded early-exit behavior.
func TestValidateSchema_EarlyExit(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name       string
		windowMin  int
		targetPips float64
		maxHold    int
		wantOK     bool
		errSubstr  string // checked only when wantOK=false
	}{
		// window=0 (OFF) は ratchet ON なら許可 (下落の歯止めは ratchet が担う)。
		// 本テストの fixture (validStrategyConfig) は ratchet arm20/give3 ON なので OK。
		{"disabled (window=0) with ratchet ON: OK", 0, 0, 240, true, ""},
		{"window=30 target=-2: OK", 30, -2.0, 240, true, ""},
		{"window=1 target=0: OK (minimal)", 1, 0, 240, true, ""},
		{"window=max_hold (240): OK boundary", 240, -2.0, 240, true, ""},
		{"target > 0 (positive — take small win): OK", 30, 5.0, 240, true, ""},

		{"negative window: rejected", -10, -2.0, 240, false, "early_exit_window_minutes"},
		{"window > max_hold: rejected", 300, -2.0, 240, false, "early_exit_window_minutes"},
		{"window > cap (240): rejected", 250, -2.0, 500, false, "early_exit_window_minutes"},
		{"target way too negative (typo): rejected", 30, -200.0, 240, false, "early_exit_target_pips"},
		{"target way too positive (typo): rejected", 30, 200.0, 240, false, "early_exit_target_pips"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validStrategyConfig(now)
			c.Exit.MaxHoldMinutes = tc.maxHold
			c.Exit.EarlyExitWindowMinutes = tc.windowMin
			c.Exit.EarlyExitTargetPips = tc.targetPips
			v := newValidator(now)
			r := v.ValidateSchema(c)
			if tc.wantOK {
				if !r.OK() {
					t.Errorf("expected OK, got: %s", r.Summary())
				}
			} else {
				if r.OK() {
					t.Errorf("expected error containing %q, got OK", tc.errSubstr)
				} else if !errorsContain(r.Errors, tc.errSubstr) {
					t.Errorf("expected error containing %q, got: %s", tc.errSubstr, r.Summary())
				}
			}
		})
	}
}

// Ratchet TP (trailing take-profit). 0 / 0 = OFF。有効化したら arm >= 3,
// give >= 1, arm > give, それぞれ 50pips 以下、部分指定 (片方だけ > 0) は禁止。
func TestValidateSchema_Ratchet(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name     string
		armPips  float64
		givePips float64
		wantOK   bool
		errField string // checked only when wantOK=false
	}{
		// enabled config では 0/0 (OFF) を禁止。
		{"disabled (0/0): rejected (enabled config)", 0, 0, false, "ratchet"},
		// OK cases は RR floor (locked = arm-give ≥ TP×0.5 = 15 @ fixture TP30)
		// も満たす値にする。floor 自体の検証は TestValidateSchema_RatchetRRFloor が担当。
		{"arm=20 give=3: OK", 20, 3, true, ""},
		{"arm=20 give=1: aggressive ratchet OK", 20, 1, true, ""},
		{"arm=50 give=35: upper arm boundary OK", 50, 35, true, ""},
		{"arm=16 give=1: low-arm floor-min OK", 16, 1, true, ""},

		// 部分指定: 片方だけ設定するのは禁止 (どっちも 0 か、どっちも > 0)
		{"arm only: rejected", 5, 0, false, "ratchet"},
		{"giveback only: rejected", 0, 3, false, "ratchet"},

		// 範囲外
		{"negative arm: rejected", -1, 3, false, "ratchet_arm_pips"},
		{"negative giveback: rejected", 5, -1, false, "ratchet_giveback_pips"},
		{"arm < 3 (too sensitive): rejected", 2, 1, false, "ratchet_arm_pips"},
		{"giveback < 1 (no buffer): rejected", 5, 0.5, false, "ratchet_giveback_pips"},
		{"arm > 50 (sanity cap): rejected", 60, 5, false, "ratchet_arm_pips"},
		{"giveback > 50 (sanity cap): rejected", 60, 55, false, "ratchet_giveback_pips"},

		// 論理: arm <= give は ratchet が即発火するので無意味
		{"arm == giveback: rejected", 5, 5, false, "ratchet_giveback_pips"},
		{"arm < giveback: rejected", 3, 5, false, "ratchet_giveback_pips"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validStrategyConfig(now)
			c.Exit.RatchetArmPips = tc.armPips
			c.Exit.RatchetGivebackPips = tc.givePips
			v := newValidator(now)
			r := v.ValidateSchema(c)
			if tc.wantOK {
				if !r.OK() {
					t.Errorf("expected OK, got: %s", r.Summary())
				}
			} else {
				if r.OK() {
					t.Errorf("expected error containing %q, got OK", tc.errField)
				} else if !errorsContain(r.Errors, tc.errField) {
					t.Errorf("expected error containing %q, got: %s", tc.errField, r.Summary())
				}
			}
		})
	}
}

// RR floor: ratchet が逆RRを生むのを防ぐ。
// 例: arm5/give3 だと +2〜4pips で利確する一方 SL は -15〜20pips = 実効RR≈0.2 になる。
// ratchet は本来 TP 手前のトレーリング (skill 03) なので、利確するなら最低
// take_profit_pips × 0.5 の含み益を確保してから (= locked = arm-giveback ≥ TP×0.5)。
func TestValidateSchema_RatchetRRFloor(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		tp     float64
		arm    float64
		give   float64
		wantOK bool
	}{
		{"locked 2 (arm5/give3) on TP30: reject 逆RR", 30, 5, 3, false},
		{"locked 14 (arm15/give1) on TP30: just under floor reject", 30, 15, 1, false},
		{"locked 15 (arm16/give1) on TP30: exactly floor OK", 30, 16, 1, true},
		{"locked 17 (arm20/give3) on TP30: OK", 30, 20, 3, true},
		{"locked 19 (arm24/give5) on TP40: under floor(20) reject", 40, 24, 5, false},
		{"locked 20 (arm25/give5) on TP40: floor OK", 40, 25, 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validStrategyConfig(now)
			c.Exit.TakeProfitPips = tc.tp
			c.Exit.RatchetArmPips = tc.arm
			c.Exit.RatchetGivebackPips = tc.give
			v := newValidator(now)
			r := v.ValidateSchema(c)
			if tc.wantOK && !r.OK() {
				t.Errorf("expected OK, got: %s", r.Summary())
			}
			if !tc.wantOK {
				if r.OK() {
					t.Errorf("expected reject (locked < TP×0.5), got OK")
				} else if !errorsContain(r.Errors, "locked profit") {
					t.Errorf("expected 'locked profit' floor error, got: %s", r.Summary())
				}
			}
		})
	}
}

// RR floor は no_trade では適用しない (ratchet 0/0 = exempt)。
func TestValidateSchema_RatchetRRFloor_NoTradeExempt(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validNoTradeConfig(now) // ratchet 0/0, enabled=false
	r := v.ValidateSchema(c)
	if !r.OK() {
		t.Fatalf("no_trade config should pass (RR floor exempt): %s", r.Summary())
	}
}

// hard_limit -------------------------------------------------------------

func TestValidateHardLimit_OK(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	r := v.ValidateHardLimit(validStrategyConfig(now))
	if !r.OK() {
		t.Fatalf("expected ok, got: %s", r.Summary())
	}
}

func TestValidateHardLimit_SymbolNotAllowed(t *testing.T) {
	now := time.Now()
	c := validStrategyConfig(now)
	c.Symbol = "BTC_JPY"
	v := newValidator(now)
	r := v.ValidateHardLimit(c)
	if r.OK() || !errorsContain(r.Errors, "symbol") {
		t.Fatalf("got: %s", r.Summary())
	}
}

func TestValidateHardLimit_NumericBoundaries(t *testing.T) {
	now := time.Now()
	type tweak struct {
		name   string
		mutate func(*StrategyConfig)
		field  string
	}
	cases := []tweak{
		{"quantity too low", func(c *StrategyConfig) { c.Risk.Quantity = 50 }, "risk.quantity"},
		{"quantity too high", func(c *StrategyConfig) { c.Risk.Quantity = 200 }, "risk.quantity"},
		{"tp too low", func(c *StrategyConfig) { c.Exit.TakeProfitPips = 10 }, "take_profit_pips"},
		{"tp too high", func(c *StrategyConfig) { c.Exit.TakeProfitPips = 60 }, "take_profit_pips"},
		{"sl too low", func(c *StrategyConfig) { c.Exit.StopLossPips = 10 }, "stop_loss_pips"},
		{"sl too high", func(c *StrategyConfig) { c.Exit.StopLossPips = 35 }, "stop_loss_pips"},
		{"max_hold too low", func(c *StrategyConfig) { c.Exit.MaxHoldMinutes = 200 }, "max_hold_minutes"},
		{"max_hold too high", func(c *StrategyConfig) { c.Exit.MaxHoldMinutes = 400 }, "max_hold_minutes"},
		{"spread too low", func(c *StrategyConfig) { c.Entry.MaxSpreadPips = 0.2 }, "max_spread_pips"},
		{"spread too high", func(c *StrategyConfig) { c.Entry.MaxSpreadPips = 1.5 }, "max_spread_pips"},
		{"max_trades too high", func(c *StrategyConfig) { c.Risk.MaxTradesInThisWindow = 99 }, "max_trades_in_this_window"},
		{"max_loss too high", func(c *StrategyConfig) { c.Risk.MaxLossInThisWindowJPY = 99999 }, "max_loss_in_this_window_jpy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validStrategyConfig(now)
			tc.mutate(c)
			v := newValidator(now)
			r := v.ValidateHardLimit(c)
			if r.OK() || !errorsContain(r.Errors, tc.field) {
				t.Errorf("expected %s error, got: %s", tc.field, r.Summary())
			}
		})
	}
}

func TestValidateHardLimit_TTLOutOfRange(t *testing.T) {
	now := time.Now()

	t.Run("ttl too short", func(t *testing.T) {
		c := validStrategyConfig(now)
		c.ValidUntil = c.ValidFrom.Add(30 * time.Minute) // < 60
		v := newValidator(now)
		r := v.ValidateHardLimit(c)
		if r.OK() || !errorsContain(r.Errors, "valid_until") {
			t.Errorf("got: %s", r.Summary())
		}
	})

	t.Run("ttl too long", func(t *testing.T) {
		c := validStrategyConfig(now)
		c.ValidUntil = c.ValidFrom.Add(180 * time.Minute) // > 120
		v := newValidator(now)
		r := v.ValidateHardLimit(c)
		if r.OK() || !errorsContain(r.Errors, "valid_until") {
			t.Errorf("got: %s", r.Summary())
		}
	})

	// TTL is a lifecycle constraint and must apply
	// even to no_trade configs — otherwise a no_trade with TTL=0 or TTL=99h
	// breaks the scheduler's re-evaluation cadence.
	t.Run("no_trade with bad TTL still fails", func(t *testing.T) {
		c := validNoTradeConfig(now)
		c.ValidUntil = c.ValidFrom.Add(30 * time.Minute) // < 60
		v := newValidator(now)
		r := v.ValidateHardLimit(c)
		if r.OK() || !errorsContain(r.Errors, "valid_until") {
			t.Errorf("no_trade should still be TTL-checked; got: %s", r.Summary())
		}
	})
}

func TestValidateHardLimit_NoTradeSkipsNumericChecks(t *testing.T) {
	now := time.Now()
	c := validNoTradeConfig(now)
	// Even though quantity=0 (would normally violate min=100), no_trade exempts.
	v := newValidator(now)
	r := v.ValidateHardLimit(c)
	if !r.OK() {
		t.Errorf("no_trade should be exempt from numeric bounds, got: %s", r.Summary())
	}
}

// risk -------------------------------------------------------------------

func TestValidateRisk_OK(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	state := AccountState{
		MaxDailyLossJPY:      1000,
		MaxConsecutiveLosses: 3,
		MaxOpenPositions:     2,
	}
	r := v.ValidateRisk(validStrategyConfig(now), state)
	if !r.OK() {
		t.Fatalf("expected ok, got: %s", r.Summary())
	}
}

func TestValidateRisk_EmergencyStop(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	r := v.ValidateRisk(validStrategyConfig(now), AccountState{EmergencyStop: true})
	if r.OK() || !errorsContain(r.Errors, "emergency_stop") {
		t.Errorf("got: %s", r.Summary())
	}
}

func TestValidateRisk_DailyLossExceeded(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	state := AccountState{DailyLossJPY: 1500, MaxDailyLossJPY: 1000}
	r := v.ValidateRisk(validStrategyConfig(now), state)
	if r.OK() || !errorsContain(r.Errors, "daily_loss") {
		t.Errorf("got: %s", r.Summary())
	}
}

func TestValidateRisk_ConsecutiveLosses(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	state := AccountState{ConsecutiveLosses: 4, MaxConsecutiveLosses: 3}
	r := v.ValidateRisk(validStrategyConfig(now), state)
	if r.OK() || !errorsContain(r.Errors, "consecutive_losses") {
		t.Errorf("got: %s", r.Summary())
	}
}

func TestValidateRisk_UnknownPosition(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	r := v.ValidateRisk(validStrategyConfig(now), AccountState{UnknownPositionExists: true})
	if r.OK() || !errorsContain(r.Errors, "positions") {
		t.Errorf("got: %s", r.Summary())
	}
}

func TestValidateRisk_UnresolvedOrder(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	r := v.ValidateRisk(validStrategyConfig(now), AccountState{UnresolvedOrderExists: true})
	if r.OK() || !errorsContain(r.Errors, "orders") {
		t.Errorf("got: %s", r.Summary())
	}
}

func TestValidateRisk_MaxOpenPositionsExceeded(t *testing.T) {
	now := time.Now()
	c := validStrategyConfig(now)
	c.Risk.MaxOpenPositions = 5
	v := newValidator(now)
	r := v.ValidateRisk(c, AccountState{MaxOpenPositions: 1})
	if r.OK() || !errorsContain(r.Errors, "max_open_positions") {
		t.Errorf("got: %s", r.Summary())
	}
}

func TestValidateRisk_AlreadyExpired(t *testing.T) {
	now := time.Now()
	c := validStrategyConfig(now)
	c.ValidFrom = now.Add(-2 * time.Hour)
	c.ValidUntil = now.Add(-1 * time.Hour)
	v := newValidator(now)
	r := v.ValidateRisk(c, AccountState{})
	if r.OK() || !errorsContain(r.Errors, "valid_until") {
		t.Errorf("got: %s", r.Summary())
	}
}

func TestValidateRisk_FarFuture(t *testing.T) {
	now := time.Now()
	c := validStrategyConfig(now)
	c.ValidFrom = now.Add(3 * time.Hour)
	c.ValidUntil = now.Add(4 * time.Hour)
	v := newValidator(now)
	r := v.ValidateRisk(c, AccountState{})
	if r.OK() || !errorsContain(r.Errors, "valid_from") {
		t.Errorf("got: %s", r.Summary())
	}
}

// validate all -----------------------------------------------------------

func TestValidateAll_OK(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	r := v.ValidateAll(validStrategyConfig(now), AccountState{
		MaxDailyLossJPY: 1000, MaxConsecutiveLosses: 3, MaxOpenPositions: 2,
	})
	if !r.OK() {
		t.Fatalf("expected ok, got: %s", r.Summary())
	}
}

func TestValidateAll_AggregatesMultipleTypes(t *testing.T) {
	now := time.Now()
	c := validStrategyConfig(now)
	c.Strategy.Name = "moon_shot" // schema fail
	c.Risk.Quantity = 999         // hard_limit fail
	v := newValidator(now)
	r := v.ValidateAll(c, AccountState{EmergencyStop: true}) // risk fail
	if r.OK() {
		t.Fatalf("expected fail")
	}
	gotSchema, gotHard, gotRisk := false, false, false
	for _, e := range r.Errors {
		switch e.Type {
		case ValidationSchema:
			gotSchema = true
		case ValidationHardLimit:
			gotHard = true
		case ValidationRisk:
			gotRisk = true
		}
	}
	if !gotSchema || !gotHard || !gotRisk {
		t.Errorf("expected schema/hard/risk all present, got: %s", r.Summary())
	}
}

// strategy_limits override semantics ------------------------------------

func hardLimitsWithStrategyOverrides() *HardLimits {
	h := validHardLimits()
	// breakout は TP を 60 まで (= グローバル max 50 を超えて) 許容。
	// momentum は TP を 20-40 (= グローバル 15-50 より狭く) 制限。
	h.StrategyLimits = map[string]StrategyLimit{
		"momentum_pullback": {
			TakeProfitPips: FloatRange{Min: 20.0, Max: 40.0},
			StopLossPips:   FloatRange{Min: 15.0, Max: 25.0},
		},
		"breakout_follow": {
			TakeProfitPips: FloatRange{Min: 30.0, Max: 60.0},
			StopLossPips:   FloatRange{Min: 15.0, Max: 30.0},
		},
	}
	return h
}

func TestValidateHardLimit_StrategyOverride_TighterRangeRejects(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	v := &Validator{Limits: hardLimitsWithStrategyOverrides(), Now: func() time.Time { return now }}
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyMomentumPullback
	c.Exit.TakeProfitPips = 45.0 // global OK (15-50) だが momentum は max=40 で reject
	r := v.ValidateHardLimit(c)
	if r.OK() {
		t.Fatalf("expected reject by momentum_pullback strategy_limits, got ok")
	}
	if !strings.Contains(r.Summary(), "strategy_limits[momentum_pullback]") {
		t.Errorf("expected error to cite strategy_limits source, got: %s", r.Summary())
	}
}

func TestValidateHardLimit_StrategyOverride_LooserRangeAccepts(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	v := &Validator{Limits: hardLimitsWithStrategyOverrides(), Now: func() time.Time { return now }}
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyBreakoutFollow
	c.Entry.RequireBreakout = true // breakout_follow の整合性
	c.Exit.TakeProfitPips = 55.0   // global max=50 で reject されるが breakout は max=60 で通る
	c.Exit.StopLossPips = 25.0     // breakout SL 範囲 (15-30) 内
	r := v.ValidateHardLimit(c)
	if !r.OK() {
		t.Fatalf("expected ok via breakout_follow override, got: %s", r.Summary())
	}
}

// ValidateSemantic catches Claude configs that pass schema
// + hard_limits but are internally contradictory (e.g. trend_up + sell_only),
// and enforces the strategy whitelist (no unknown strategy names).

func TestValidateSemantic_OK_PassesValidConfig(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	r := v.ValidateSemantic(validStrategyConfig(now), []string{"momentum_pullback", "breakout_follow", "range_breakout_probe", "no_trade"})
	if !r.OK() {
		t.Errorf("valid config should pass semantic: %s", r.Summary())
	}
}

func TestValidateSemantic_TrendUpSellOnly_Rejects(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.MarketRegime.Type = RegimeTrendUp
	c.Entry.Direction = DirectionSellOnly
	r := v.ValidateSemantic(c, nil)
	if r.OK() || !errorsContain(r.Errors, "direction") {
		t.Errorf("trend_up + sell_only should fail; got: %s", r.Summary())
	}
}

func TestValidateSemantic_TrendDownBuyOnly_Rejects(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.MarketRegime.Type = RegimeTrendDown
	c.Entry.Direction = DirectionBuyOnly
	r := v.ValidateSemantic(c, nil)
	if r.OK() || !errorsContain(r.Errors, "direction") {
		t.Errorf("trend_down + buy_only should fail; got: %s", r.Summary())
	}
}

func TestValidateSemantic_MomentumPullbackRequireBreakout_Rejects(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyMomentumPullback
	c.Entry.RequireBreakout = true
	r := v.ValidateSemantic(c, nil)
	if r.OK() || !errorsContain(r.Errors, "require_breakout") {
		t.Errorf("momentum_pullback + require_breakout=true should fail; got: %s", r.Summary())
	}
}

func TestValidateSemantic_BreakoutFollowRequireBreakoutFalse_Rejects(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyBreakoutFollow
	c.Entry.RequireBreakout = false
	c.Entry.Direction = DirectionBuyOnly
	r := v.ValidateSemantic(c, nil)
	if r.OK() || !errorsContain(r.Errors, "require_breakout") {
		t.Errorf("breakout_follow + require_breakout=false should fail; got: %s", r.Summary())
	}
}

func TestValidateSemantic_RangeBreakoutProbeRequireBreakoutTrue_Rejects(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyRangeBreakoutProbe
	c.Entry.RequireBreakout = true
	c.Entry.Direction = DirectionBoth
	r := v.ValidateSemantic(c, nil)
	if r.OK() || !errorsContain(r.Errors, "require_breakout") {
		t.Errorf("range_breakout_probe + require_breakout=true should fail; got: %s", r.Summary())
	}
}

func TestValidateSemantic_RangeBreakoutProbeRequireBreakoutFalse_OK(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyRangeBreakoutProbe
	c.MarketRegime.Type = RegimeRange
	c.Entry.RequireBreakout = false
	c.Entry.Direction = DirectionBoth
	r := v.ValidateSemantic(c, []string{"range_breakout_probe"})
	if !r.OK() {
		t.Errorf("range_breakout_probe + require_breakout=false should pass; got: %s", r.Summary())
	}
}

func TestValidateSemantic_MTFPullbackRequireBreakout_Rejects(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyMTFPullback
	c.Entry.RequireBreakout = true
	r := v.ValidateSemantic(c, nil)
	if r.OK() || !errorsContain(r.Errors, "require_breakout") {
		t.Errorf("mtf_pullback + require_breakout=true should fail; got: %s", r.Summary())
	}
}

func TestValidateSemantic_MTFPullbackRequireBreakoutFalse_OK(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyMTFPullback
	c.Entry.RequireBreakout = false
	c.Entry.Direction = DirectionBoth
	r := v.ValidateSemantic(c, []string{"mtf_pullback"})
	if !r.OK() {
		t.Errorf("mtf_pullback + require_breakout=false should pass; got: %s", r.Summary())
	}
}

func TestValidateSemantic_MAPullbackRequireBreakout_Rejects(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyMAPullback
	c.Entry.RequireBreakout = true
	r := v.ValidateSemantic(c, nil)
	if r.OK() || !errorsContain(r.Errors, "require_breakout") {
		t.Errorf("ma_pullback + require_breakout=true should fail; got: %s", r.Summary())
	}
}

func TestValidateSemantic_MAPullbackRequireBreakoutFalse_OK(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyMAPullback
	c.Entry.RequireBreakout = false
	c.Entry.Direction = DirectionBoth
	r := v.ValidateSemantic(c, []string{"ma_pullback"})
	if !r.OK() {
		t.Errorf("ma_pullback + require_breakout=false should pass; got: %s", r.Summary())
	}
}

// Ratchet-required exemption: like mtf_pullback, ma_pullback drops ratchet/trailing
// and puts the exit in a structural TP/SL (computed at entry, ridden as pip
// distances). The downside floor is the structural SL + early_exit, so ratchet
// OFF (0/0) must be allowed for this strategy when early_exit is ON.
func TestValidateSchema_B4_AllowsRatchetOffForMAPullback(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyMAPullback
	c.MarketRegime.Confidence = 0.7
	c.Exit.EarlyExitWindowMinutes = 15 // early_exit ON so the downside floor still holds
	c.Exit.EarlyExitTargetPips = -3
	c.Exit.RatchetArmPips = 0 // ratchet OFF — allowed for ma_pullback
	c.Exit.RatchetGivebackPips = 0
	r := v.ValidateSchema(c)
	if !r.OK() {
		t.Fatalf("ma_pullback + ratchet OFF + early_exit ON should pass (ratchet-required exempt): %s", r.Summary())
	}
}

func TestValidateSemantic_StrategyNotInWhitelist_Rejects(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validStrategyConfig(now)
	c.Strategy.Name = StrategyMomentumPullback // valid enum, but not in whitelist
	allowed := []string{"breakout_follow", "no_trade"}
	r := v.ValidateSemantic(c, allowed)
	if r.OK() || !errorsContain(r.Errors, "strategy.name") {
		t.Errorf("strategy not in whitelist should fail; got: %s", r.Summary())
	}
}

func TestValidateSemantic_EmptyWhitelist_SkipsCheck(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	r := v.ValidateSemantic(validStrategyConfig(now), nil)
	if !r.OK() {
		t.Errorf("nil whitelist should skip the check: %s", r.Summary())
	}
}

func TestValidateSemantic_NoTradeIgnoresDirectionRegimeMismatch(t *testing.T) {
	now := time.Now()
	v := newValidator(now)
	c := validNoTradeConfig(now)
	c.MarketRegime.Type = RegimeTrendUp // would normally conflict with direction!=buy
	r := v.ValidateSemantic(c, []string{"no_trade", "momentum_pullback", "breakout_follow", "range_breakout_probe"})
	if !r.OK() {
		t.Errorf("no_trade should bypass direction/regime check: %s", r.Summary())
	}
}

func TestValidateHardLimit_StrategyOverride_FallbackToGlobal(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	// strategy_limits に無いキーは global にフォールバックする (将来の戦略追加時の安全網)。
	v := &Validator{Limits: hardLimitsWithStrategyOverrides(), Now: func() time.Time { return now }}
	c := validStrategyConfig(now)
	c.Strategy.Name = "future_unmapped_strategy" // not in overrides → falls back to global
	c.Exit.TakeProfitPips = 45.0                 // global TP max=50 内
	c.Exit.StopLossPips = 20.0
	r := v.ValidateHardLimit(c)
	if !r.OK() {
		t.Fatalf("expected ok via global fallback, got: %s", r.Summary())
	}
}
