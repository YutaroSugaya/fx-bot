package risk

import (
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
)

// 手動 override は EvaluateSignal の最初の理由が allowlist(open_positions / cooldown / post_loss_freeze …)
// なら、その後ろのゲートを評価しない。だから「override できない」ゲートは EvaluateHardSafety でも
// 再検査しなければならない: 同 symbol 同 side の建玉(外部建玉を含む)があれば新規を拒否する
// ナンピン禁止、決済直後の同方向の再エントリー冷却、口座全体の建玉上限。

func hardSafetyCfg() *config.StrategyConfig {
	return &config.StrategyConfig{
		Entry: config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 5},
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 1},
	}
}

func hardSafetyEntry(side order.Side) strategy.Signal {
	return strategy.Signal{Decision: strategy.DecisionEnter, Side: side, TakeProfitPips: 10, StopLossPips: 10, ConfigID: "c1"}
}

func TestEvaluateHardSafety_BlocksSameSidePyramidingInclExternal(t *testing.T) {
	snap := AccountSnapshot{OpenPositions: 1, OpenBuyInclExternal: 1}

	d := EvaluateHardSafety(hardSafetyEntry(order.SideBuy), hardSafetyCfg(), snap, nil)
	if d.Allowed || !strings.HasPrefix(d.Reason, "pyramiding_blocked_same_side_buy") {
		t.Fatalf("same-side BUY must stay blocked in the hard-safety pass; got %+v", d)
	}

	// Opposite side is not pyramiding.
	if d := EvaluateHardSafety(hardSafetyEntry(order.SideSell), hardSafetyCfg(), snap, nil); !d.Allowed {
		t.Fatalf("opposite-side entry must pass the hard-safety pass; got %+v", d)
	}
}

func TestEvaluateHardSafety_PyramidingHonorsMaxConcurrent(t *testing.T) {
	sig := hardSafetyEntry(order.SideSell)
	sig.MaxConcurrent = 2

	if d := EvaluateHardSafety(sig, hardSafetyCfg(), AccountSnapshot{OpenSellInclExternal: 1}, nil); !d.Allowed {
		t.Fatalf("a strategy allowing 2 same-side positions must pass with 1 open; got %+v", d)
	}
	if d := EvaluateHardSafety(sig, hardSafetyCfg(), AccountSnapshot{OpenSellInclExternal: 2}, nil); d.Allowed {
		t.Fatal("the (N+1)th same-side position must be blocked")
	}
}

func TestEvaluateHardSafety_BlocksReentryCooldown(t *testing.T) {
	now := time.Date(2020, 1, 6, 10, 0, 0, 0, time.UTC)
	snap := AccountSnapshot{Now: now, ReentryCooldown: time.Hour, LastBuyClosedAt: now.Add(-10 * time.Minute)}

	d := EvaluateHardSafety(hardSafetyEntry(order.SideBuy), hardSafetyCfg(), snap, nil)
	if d.Allowed || !strings.HasPrefix(d.Reason, "reentry_cooldown") {
		t.Fatalf("same-side re-entry inside the cooldown must be blocked; got %+v", d)
	}
	if d := EvaluateHardSafety(hardSafetyEntry(order.SideSell), hardSafetyCfg(), snap, nil); !d.Allowed {
		t.Fatalf("the cooldown is per side; got %+v", d)
	}
}

func TestEvaluateHardSafety_BlocksAccountOpenPositionsCap(t *testing.T) {
	snap := AccountSnapshot{AccountMaxOpenPositions: 3, AccountOpenPositions: 3}

	d := EvaluateHardSafety(hardSafetyEntry(order.SideBuy), hardSafetyCfg(), snap, nil)
	if d.Allowed || !strings.HasPrefix(d.Reason, "account_open_positions") {
		t.Fatalf("the account-wide cap must hold in the hard-safety pass; got %+v", d)
	}
}
