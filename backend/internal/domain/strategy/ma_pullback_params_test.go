package strategy

import (
	"reflect"
	"testing"
)

// walk-forward sweep のための parameterization。
// 絶対条件 = live 挙動不変: zero-value MAPullback{} は DefaultMAPullbackParams()
// と完全等価 (registry は MAPullback{} を登録している)。値の変更はオフライン専用。

// zero-value と explicit defaults が同一 Signal を返すこと (live 不変 pin)。
func TestMAPullback_ZeroValueEqualsDefaults(t *testing.T) {
	in, _ := maReadyInput()
	got := MAPullback{}.Evaluate(in)
	want := MAPullback{P: DefaultMAPullbackParams()}.Evaluate(in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zero-value MAPullback{} must behave as defaults:\n got=%+v\nwant=%+v", got, want)
	}
	if got.Decision != DecisionEnter {
		t.Fatalf("fixture must produce an entry; got %s (%s)", got.Decision, got.Reason)
	}
}

// defaults が凍結済みの定数と一致すること (audit 定数との等値 pin)。
func TestDefaultMAPullbackParams_MatchAuditConstants(t *testing.T) {
	p := DefaultMAPullbackParams()
	arm, give, hold := MAPullbackAuditExits()
	if p.RatchetArmPips != arm || p.RatchetGivebackPips != give || p.MaxHoldMinutes != hold {
		t.Errorf("defaults must equal audit constants: got arm=%v give=%v hold=%d want %v/%v/%d",
			p.RatchetArmPips, p.RatchetGivebackPips, p.MaxHoldMinutes, arm, give, hold)
	}
	if p.TrendMinSlopePips != 5.0 || p.ZoneATR != 0.8 || p.ConfluenceATR != 1.2 {
		t.Errorf("frozen default values drifted: slope=%v zone=%v conf=%v", p.TrendMinSlopePips, p.ZoneATR, p.ConfluenceATR)
	}
}

// パラメータ注入が実際に挙動を変えること (sweep の前提)。
func TestMAPullback_ParamInjection_ChangesBehaviour(t *testing.T) {
	in, _ := maReadyInput()

	t.Run("huge slope threshold kills the trend gate", func(t *testing.T) {
		p := DefaultMAPullbackParams()
		p.TrendMinSlopePips = 10000
		sig := MAPullback{P: p}.Evaluate(in)
		if sig.Decision == DecisionEnter {
			t.Fatalf("want no entry with impossible slope threshold; got entry")
		}
		if sig.Reason != "no_trend" {
			t.Errorf("reason: got %q want no_trend", sig.Reason)
		}
	})

	t.Run("ratchet and maxhold flow into the Signal", func(t *testing.T) {
		p := DefaultMAPullbackParams()
		p.RatchetArmPips = 12
		p.RatchetGivebackPips = 6
		p.MaxHoldMinutes = 240
		sig := MAPullback{P: p}.Evaluate(in)
		if sig.Decision != DecisionEnter {
			t.Fatalf("want entry; got %s (%s)", sig.Decision, sig.Reason)
		}
		if sig.RatchetArmPips != 12 || sig.RatchetGivebackPips != 6 || sig.MaxHoldMinutes != 240 {
			t.Errorf("injected exits must reach the Signal: arm=%v give=%v hold=%d",
				sig.RatchetArmPips, sig.RatchetGivebackPips, sig.MaxHoldMinutes)
		}
	})

	t.Run("SL clamp injection reaches the structural SL", func(t *testing.T) {
		p := DefaultMAPullbackParams()
		p.SLMinPips = 13
		p.SLMaxPips = 13 // clamp を 1 点に固定 → SL は必ず 13
		sig := MAPullback{P: p}.Evaluate(in)
		if sig.Decision != DecisionEnter {
			t.Fatalf("want entry; got %s (%s)", sig.Decision, sig.Reason)
		}
		if sig.StopLossPips != 13 {
			t.Errorf("StopLossPips: got %v want 13 (injected clamp)", sig.StopLossPips)
		}
	})
}

// 不正注入 (負の lookback) は panic ではなく no-trade に
// 退避する (sweep 側で展開時に弾くが、domain は任意の呼び手に対し panic-safe であるべき)。
func TestMAPullback_InvalidLookback_NoPanic(t *testing.T) {
	in, _ := maReadyInput()
	p := DefaultMAPullbackParams()
	p.TrendSlopeLookback1h = -5
	sig := MAPullback{P: p}.Evaluate(in) // panic しないこと
	if sig.Decision == DecisionEnter {
		t.Fatalf("invalid params must not enter; got entry")
	}
	if sig.Reason != "invalid_params" {
		t.Errorf("reason: got %q want invalid_params", sig.Reason)
	}
	_ = MAPullback{P: p}.Gates(in) // こちらも panic しないこと
}
