package risk

import (
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
)

func validCfg(dir config.Direction, now time.Time) *config.StrategyConfig {
	return &config.StrategyConfig{
		ConfigID:   "rgcfg",
		ValidFrom:  now,
		ValidUntil: now.Add(time.Hour),
		Enabled:    true,
		Symbol:     "USD_JPY",
		Strategy:   config.StrategySection{Name: config.StrategyMomentumPullback},
		Entry:      config.EntrySection{MaxSpreadPips: 0.5, Direction: dir},
		Exit:       config.ExitSection{TakeProfitPips: 2.0, StopLossPips: 2.5, MaxHoldMinutes: 20},
		Risk: config.ConfigRiskSection{
			Quantity: 100, MaxOpenPositions: 1, MaxTradesInThisWindow: 3, MaxLossInThisWindowJPY: 300,
		},
	}
}

func entrySig(side order.Side) strategy.Signal {
	return strategy.Signal{
		Decision: strategy.DecisionEnter,
		Side:     side,
	}
}

func mkSummary(spreadPips float64) *market.MarketSummary {
	return &market.MarketSummary{
		CurrentRate: market.CurrentRate{SpreadPips: spreadPips},
	}
}

func TestEvaluateSignal_AllowsValidEntry(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		MaxDailyLossJPY: 1000, MaxConsecutiveLosses: 3,
	}, mkSummary(0.3))
	if !d.Allowed {
		t.Errorf("expected allowed, got reason=%s", d.Reason)
	}
}

func TestEvaluateSignal_NoEntrySignal_TrivialAllow(t *testing.T) {
	d := EvaluateSignal(strategy.Signal{Decision: strategy.DecisionNone}, nil, AccountSnapshot{}, nil)
	if !d.Allowed {
		t.Errorf("non-entry should be allowed (no-op)")
	}
}

func TestEvaluateSignal_NoConfig(t *testing.T) {
	d := EvaluateSignal(entrySig(order.SideBuy), nil, AccountSnapshot{}, nil)
	if d.Allowed || !strings.Contains(d.Reason, "no_active_config") {
		t.Errorf("got: %+v", d)
	}
}

func TestEvaluateSignal_EmergencyStop(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{EmergencyStop: true}, mkSummary(0.3))
	if d.Allowed || !strings.Contains(d.Reason, "emergency_stop") {
		t.Errorf("got: %+v", d)
	}
}

func TestEvaluateSignal_DailyLossCap(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		DailyLossJPY: 1100, MaxDailyLossJPY: 1000,
	}, mkSummary(0.3))
	if d.Allowed || !strings.Contains(d.Reason, "daily_loss") {
		t.Errorf("got: %+v", d)
	}
}

func TestEvaluateSignal_ConsecutiveLossesCap(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses: 3, MaxConsecutiveLosses: 3,
	}, mkSummary(0.3))
	if d.Allowed || !strings.Contains(d.Reason, "consecutive_losses") {
		t.Errorf("got: %+v", d)
	}
}

// --- graduated consecutive-loss cooldown
//
// New behavior on top of the binary MaxConsecutiveLosses cap:
//   - >=4 consecutive losses → hard daily block (handled by existing cap
//     once bot_config.risk.max_consecutive_losses is lowered to 4)
//   - >=3 consecutive losses → 30-minute freeze starting at the most-recent
//     loss's closed_at
//
// The freeze uses snap.Now and snap.LastLossClosedAt so the gate stays a
// pure function (no time.Now() inside).

func TestEvaluateSignal_ConsecutiveLosses3_FreezeWithin30Min(t *testing.T) {
	now := time.Date(2026, 5, 28, 10, 30, 0, 0, time.UTC)
	lastLoss := now.Add(-15 * time.Minute)
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses: 3,
		LastLossClosedAt:  lastLoss,
		Now:               now,
	}, mkSummary(0.3))
	if d.Allowed {
		t.Fatalf("expected freeze 15min after the most recent loss (cap=3)")
	}
	if !strings.Contains(d.Reason, "consecutive_losses") {
		t.Errorf("reason should start with consecutive_losses: %q", d.Reason)
	}
	if !strings.Contains(d.Reason, "freeze") {
		t.Errorf("reason should mention freeze: %q", d.Reason)
	}
}

func TestEvaluateSignal_ConsecutiveLosses3_AllowAfter30Min(t *testing.T) {
	now := time.Date(2026, 5, 28, 11, 30, 0, 0, time.UTC)
	lastLoss := now.Add(-31 * time.Minute) // freeze window elapsed
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses:    3,
		LastLossClosedAt:     lastLoss,
		Now:                  now,
		MaxConsecutiveLosses: 4, // binary cap won't trigger at 3
	}, mkSummary(0.3))
	if !d.Allowed {
		t.Fatalf("expected allow after 30min freeze elapsed: %s", d.Reason)
	}
	// 連敗時の qty 半減は廃止: freeze が明けたら full qty で再開する。
	if d.QtyMultiplier != 1.0 {
		t.Errorf("expected QtyMultiplier=1.0 after freeze elapses (halve retired): got %v", d.QtyMultiplier)
	}
}

func TestEvaluateSignal_ConsecutiveLosses2_NoHalveFullQty(t *testing.T) {
	// 連敗時の qty 半減は廃止。2 連敗でも full qty。
	// cap / 30分freeze / 同方向ブロックは残す (ロットを減らさず止める系)。
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses:    2,
		Now:                  now,
		MaxConsecutiveLosses: 4,
	}, mkSummary(0.3))
	if !d.Allowed {
		t.Fatalf("expected allow at 2 consecutive losses: %s", d.Reason)
	}
	if d.QtyMultiplier != 1.0 {
		t.Errorf("expected QtyMultiplier=1.0 with 2 consec (halve retired): got %v", d.QtyMultiplier)
	}
}

func TestEvaluateSignal_NoConsecutiveLosses_FullQty(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses:    0,
		Now:                  now,
		MaxConsecutiveLosses: 4,
	}, mkSummary(0.3))
	if !d.Allowed {
		t.Fatalf("expected allow with zero consecutive losses: %s", d.Reason)
	}
	if d.QtyMultiplier != 1.0 {
		t.Errorf("expected QtyMultiplier=1.0 with 0 consec: got %v", d.QtyMultiplier)
	}
}

// Post-loss freeze: after ANY single losing close on this
// symbol, block new entries for PostLossFreeze — per-symbol, threshold=1, and
// INDEPENDENT of ConsecutiveLossGuardsDisabled (cap / same-direction ban stay
// off, this stays on). Guards against a whipsaw where the bot re-enters minutes
// after a ratchet_stoploss. LastLossClosedAt is the reuse point: it is non-zero
// only when the symbol's most-recent closed trade is a loss.
func TestEvaluateSignal_PostLossFreeze_BlocksWithinWindow(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 30, 0, 0, time.UTC)
	lastLoss := now.Add(-4 * time.Minute) // 4 min ago
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses:             1,
		LastLossClosedAt:              lastLoss,
		Now:                           now,
		PostLossFreeze:                30 * time.Minute,
		ConsecutiveLossGuardsDisabled: true, // cap/same-direction ban/30-min freeze OFF; this must still fire
	}, mkSummary(0.3))
	if d.Allowed {
		t.Fatalf("expected post_loss_freeze to block entry 4min after a loss")
	}
	if !strings.Contains(d.Reason, "post_loss_freeze") {
		t.Errorf("reason should mention post_loss_freeze: %q", d.Reason)
	}
}

func TestEvaluateSignal_PostLossFreeze_AllowAfterWindow(t *testing.T) {
	now := time.Date(2026, 7, 8, 13, 30, 0, 0, time.UTC)
	lastLoss := now.Add(-31 * time.Minute) // window elapsed
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses:             1,
		LastLossClosedAt:              lastLoss,
		Now:                           now,
		PostLossFreeze:                30 * time.Minute,
		ConsecutiveLossGuardsDisabled: true,
	}, mkSummary(0.3))
	if !d.Allowed {
		t.Fatalf("expected allow after post_loss_freeze window elapsed: %s", d.Reason)
	}
	if d.QtyMultiplier != 1.0 {
		t.Errorf("expected QtyMultiplier=1.0 after freeze (halve retired): got %v", d.QtyMultiplier)
	}
}

func TestEvaluateSignal_PostLossFreeze_DisabledWhenZero(t *testing.T) {
	// PostLossFreeze=0 → gate off. A recent loss must NOT block (this is the
	// default until the knob is turned on).
	now := time.Date(2026, 7, 8, 14, 30, 0, 0, time.UTC)
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses:             1,
		LastLossClosedAt:              now.Add(-1 * time.Minute),
		Now:                           now,
		PostLossFreeze:                0,
		ConsecutiveLossGuardsDisabled: true,
	}, mkSummary(0.3))
	if !d.Allowed {
		t.Fatalf("expected allow when PostLossFreeze=0: %s", d.Reason)
	}
}

func TestEvaluateSignal_PostLossFreeze_NoLossNoFreeze(t *testing.T) {
	// LastLossClosedAt zero (most-recent trade was a win / no trades) → the
	// freeze never arms even with the knob on.
	now := time.Date(2026, 7, 8, 15, 30, 0, 0, time.UTC)
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses:             0,
		LastLossClosedAt:              time.Time{},
		Now:                           now,
		PostLossFreeze:                30 * time.Minute,
		ConsecutiveLossGuardsDisabled: true,
	}, mkSummary(0.3))
	if !d.Allowed {
		t.Fatalf("expected allow when no recent loss (LastLossClosedAt zero): %s", d.Reason)
	}
}

func TestEvaluateSignal_ConsecutiveLosses4_DailyBlockViaCap(t *testing.T) {
	// With bot_config.risk.max_consecutive_losses=4、
	// 4 連敗目で binary cap が daily block を発火する。
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses:    4,
		MaxConsecutiveLosses: 4,
		Now:                  now,
	}, mkSummary(0.3))
	if d.Allowed {
		t.Fatalf("expected reject at 4 consecutive losses via cap")
	}
	if !strings.Contains(d.Reason, "consecutive_losses") {
		t.Errorf("reason should mention consecutive_losses: %q", d.Reason)
	}
}

// --- 経済指標カレンダー freeze
//
// EventCalendar.InFreezeWindow の判定結果を snap.InEventFreeze に詰めて
// もらい、risk.Gate はそれを見て reject するだけ。reason は "event_freeze"
// で hard gate (operator override 不可、EvaluateHardSafety にも含める)。
func TestEvaluateSignal_EventFreeze_Rejects(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{InEventFreeze: true, Now: now}, mkSummary(0.3))
	if d.Allowed {
		t.Fatalf("event_freeze should reject entry")
	}
	if !strings.Contains(d.Reason, "event_freeze") {
		t.Errorf("reason should be event_freeze: %q", d.Reason)
	}
}

// hard gate: EvaluateHardSafety (operator override 用パス) でも reject。
// 「経済指標直撃で爆損」を構造的に防ぐため operator が override しても止める。
func TestEvaluateHardSafety_EventFreeze_BlocksOverride(t *testing.T) {
	now := time.Now()
	d := EvaluateHardSafety(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{InEventFreeze: true, Now: now}, mkSummary(0.3))
	if d.Allowed {
		t.Fatalf("HardSafety must block on event_freeze even under override")
	}
	if !strings.Contains(d.Reason, "event_freeze") {
		t.Errorf("reason should be event_freeze: %q", d.Reason)
	}
}

// --- same-direction SL retry ban
//
// 当日 BUY で SL を 2 回踏んだら、その日の BUY エントリーは no_trade。SELL 同様。
// SL 限定 (= CloseReason "stop_loss")。早期 exit の小損は数えない。
// reason は "direction_" prefix で operator override 可能。

func TestEvaluateSignal_A3_TwoBuySLs_BlocksBuySignal(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			BuyStopLossesToday: 2,
			Now:                now,
		}, mkSummary(0.3))
	if d.Allowed {
		t.Fatalf("expected reject for BUY after 2 BUY SLs today")
	}
	if !strings.Contains(d.Reason, "direction_") {
		t.Errorf("reason should start with direction_ (operator-overridable): %q", d.Reason)
	}
	if !strings.Contains(d.Reason, "buy") {
		t.Errorf("reason should mention buy: %q", d.Reason)
	}
}

func TestEvaluateSignal_A3_TwoBuySLs_AllowsSellSignal(t *testing.T) {
	// 同日に BUY SL を 2 回食らっても、SELL 方向はまだ生きている。
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideSell), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			BuyStopLossesToday: 2,
			Now:                now,
		}, mkSummary(0.3))
	if !d.Allowed {
		t.Errorf("SELL should be allowed when only BUY direction is blocked: %s", d.Reason)
	}
}

func TestEvaluateSignal_A3_TwoSellSLs_BlocksSellSignal(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideSell), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			SellStopLossesToday: 2,
			Now:                 now,
		}, mkSummary(0.3))
	if d.Allowed {
		t.Fatalf("expected reject for SELL after 2 SELL SLs today")
	}
	if !strings.Contains(d.Reason, "direction_") || !strings.Contains(d.Reason, "sell") {
		t.Errorf("reason should be direction_..._sell: %q", d.Reason)
	}
}

func TestEvaluateSignal_A3_OneBuySL_AllowsBuy(t *testing.T) {
	// 1 SL では制限なし — 2 連 SL が trigger。
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			BuyStopLossesToday: 1,
			Now:                now,
		}, mkSummary(0.3))
	if !d.Allowed {
		t.Errorf("BUY should still be allowed after only 1 BUY SL: %s", d.Reason)
	}
}

// --- ConsecutiveLossGuardsDisabled
//
// 連敗系スロットルを一括で無効化する
// knob (bot_config.risk.disable_consecutive_loss_guards 由来)。対象:
//   - MaxConsecutiveLosses binary cap
//   - 連敗後 30 分 freeze
//   - 同方向 2-SL daily block
// daily_loss / account_daily_loss cap は爆損ブレーキとして残す (下の sanity)。

func TestEvaluateSignal_GuardsDisabled_A3DirectionBlockNeutralized(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			BuyStopLossesToday:            2,
			Now:                           now,
			ConsecutiveLossGuardsDisabled: true,
		}, mkSummary(0.3))
	if !d.Allowed {
		t.Errorf("same-direction SL block must be neutralized when guards disabled: %s", d.Reason)
	}
}

func TestEvaluateSignal_GuardsDisabled_ConsecutiveCapNeutralized(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			ConsecutiveLosses:             4,
			MaxConsecutiveLosses:          4,
			Now:                           now,
			ConsecutiveLossGuardsDisabled: true,
		}, mkSummary(0.3))
	if !d.Allowed {
		t.Errorf("MaxConsecutiveLosses cap must be neutralized when guards disabled: %s", d.Reason)
	}
}

func TestEvaluateSignal_GuardsDisabled_FreezeNeutralized(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	lastLoss := now.Add(-10 * time.Minute)
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses:             3,
		LastLossClosedAt:              lastLoss,
		Now:                           now,
		MaxConsecutiveLosses:          4,
		ConsecutiveLossGuardsDisabled: true,
	}, mkSummary(0.3))
	if !d.Allowed {
		t.Errorf("consecutive-loss freeze must be neutralized when guards disabled: %s", d.Reason)
	}
}

func TestEvaluateSignal_GuardsDisabled_QtyHalveNeutralized(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses:             2,
		Now:                           now,
		MaxConsecutiveLosses:          4,
		ConsecutiveLossGuardsDisabled: true,
	}, mkSummary(0.3))
	if !d.Allowed {
		t.Fatalf("expected allow at 2 consecutive losses with guards disabled: %s", d.Reason)
	}
	if d.QtyMultiplier != 1.0 {
		t.Errorf("qty halve must be neutralized when guards disabled: got QtyMultiplier=%v", d.QtyMultiplier)
	}
}

func TestEvaluateSignal_GuardsDisabled_DailyLossCapStillFires(t *testing.T) {
	// 連敗系を無効化しても daily_loss cap は残る (唯一の爆損ブレーキ)。
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		DailyLossJPY:                  1100,
		MaxDailyLossJPY:               1000,
		ConsecutiveLosses:             5,
		Now:                           now,
		ConsecutiveLossGuardsDisabled: true,
	}, mkSummary(0.3))
	if d.Allowed || !strings.Contains(d.Reason, "daily_loss") {
		t.Errorf("daily_loss cap must still fire even with guards disabled: %+v", d)
	}
}

// 3-freeze の precedence check: freeze の前に binary cap が当たれば cap が出る。
// 言い換えると freeze 表記は cap 未満の連敗数 (= 3 if cap=4) でしか出ない。
func TestEvaluateSignal_ConsecutiveLosses3_FreezeReasonShape(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	lastLoss := now.Add(-10 * time.Minute)
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		ConsecutiveLosses:    3,
		LastLossClosedAt:     lastLoss,
		Now:                  now,
		MaxConsecutiveLosses: 4,
	}, mkSummary(0.3))
	if d.Allowed {
		t.Fatalf("expected freeze rejection")
	}
	// Reason must contain "consecutive_losses_freeze" so admission's
	// isOverridableReason ("consecutive_losses" prefix) still allows
	// operator override and dashboards can group it.
	if !strings.Contains(d.Reason, "consecutive_losses_freeze") {
		t.Errorf("reason should embed consecutive_losses_freeze: %q", d.Reason)
	}
}

func TestEvaluateSignal_MaxOpenPositions(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		OpenPositions: 1, // cap is 1
	}, mkSummary(0.3))
	if d.Allowed || !strings.Contains(d.Reason, "open_positions") {
		t.Errorf("got: %+v", d)
	}
}

func TestEvaluateSignal_MaxTradesInWindow(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		TradesInWindow: 3, // cap is 3
	}, mkSummary(0.3))
	if d.Allowed || !strings.Contains(d.Reason, "trades_in_window") {
		t.Errorf("got: %+v", d)
	}
}

func TestEvaluateSignal_MaxLossInWindow(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		LossInWindowJPY: 350, // cap is 300
	}, mkSummary(0.3))
	if d.Allowed || !strings.Contains(d.Reason, "loss_in_window") {
		t.Errorf("got: %+v", d)
	}
}

func TestEvaluateSignal_DirectionBuyOnlyBlocksShort(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideSell), validCfg(config.DirectionBuyOnly, now), AccountSnapshot{}, mkSummary(0.3))
	if d.Allowed || !strings.Contains(d.Reason, "direction_buy_only") {
		t.Errorf("got: %+v", d)
	}
}

func TestEvaluateSignal_DirectionSellOnlyBlocksLong(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionSellOnly, now), AccountSnapshot{}, mkSummary(0.3))
	if d.Allowed || !strings.Contains(d.Reason, "direction_sell_only") {
		t.Errorf("got: %+v", d)
	}
}

func TestEvaluateSignal_SpreadGate(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{}, mkSummary(0.9))
	if d.Allowed || !strings.Contains(d.Reason, "spread") {
		t.Errorf("got: %+v", d)
	}
}

// Cooldown gate. The worker pre-computes InCooldown/CooldownUntil/
// CooldownKind from the last closed trade; the gate just rejects.
func TestEvaluateSignal_Cooldown_ActiveRejects(t *testing.T) {
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			InCooldown:    true,
			CooldownUntil: now.Add(30 * time.Second),
			CooldownKind:  "after_loss",
		}, mkSummary(0.3))
	if d.Allowed || !strings.Contains(d.Reason, "cooldown") {
		t.Errorf("expected cooldown rejection; got: %+v", d)
	}
	if !strings.Contains(d.Reason, "after_loss") {
		t.Errorf("rejection should cite cooldown kind; got: %s", d.Reason)
	}
}

func TestEvaluateSignal_Cooldown_InactiveAllows(t *testing.T) {
	// InCooldown=false (= worker decided not in cooldown) → entry allowed.
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			MaxDailyLossJPY: 1000, MaxConsecutiveLosses: 3,
			InCooldown: false,
		}, mkSummary(0.3))
	if !d.Allowed {
		t.Errorf("expected allowed; got reason=%s", d.Reason)
	}
}

// --- account-wide caps (multi-symbol risk gate) ---

func TestEvaluateSignal_AccountDailyLossCap(t *testing.T) {
	// Per-symbol DailyLoss is well under its cap, but account-wide aggregate
	// breached. Must reject with an account_daily_loss reason.
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			DailyLossJPY:           1000,  // per-symbol fine
			MaxDailyLossJPY:        8000,  // per-symbol cap not reached
			AccountDailyLossJPY:    13000, // account total breached
			AccountMaxDailyLossJPY: 12000,
		}, mkSummary(0.3))
	if d.Allowed {
		t.Fatalf("account daily loss must block entry; got Allowed")
	}
	if !strings.Contains(d.Reason, "account_daily_loss") {
		t.Errorf("reason should mention account_daily_loss: %q", d.Reason)
	}
}

func TestEvaluateSignal_AccountOpenPositionsCap(t *testing.T) {
	// Per-symbol OpenPositions=0 (this bundle has no open) but account
	// already at cap due to a sibling symbol holding a position.
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			OpenPositions:           0, // per-symbol slot empty
			AccountOpenPositions:    2, // account cap reached across symbols
			AccountMaxOpenPositions: 2,
		}, mkSummary(0.3))
	if d.Allowed {
		t.Fatalf("account open positions cap must block; got Allowed")
	}
	if !strings.Contains(d.Reason, "account_open_positions") {
		t.Errorf("reason should mention account_open_positions: %q", d.Reason)
	}
}

func TestEvaluateSignal_AccountCapsZeroDisabled(t *testing.T) {
	// AccountMax* = 0 means the account-wide gate is disabled even if the
	// snapshot reports non-zero totals (= single-symbol back-compat path).
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			MaxDailyLossJPY:         8000,
			MaxConsecutiveLosses:    3,
			AccountOpenPositions:    99,
			AccountMaxOpenPositions: 0, // disabled
			AccountDailyLossJPY:     99000,
			AccountMaxDailyLossJPY:  0, // disabled
		}, mkSummary(0.3))
	if !d.Allowed {
		t.Errorf("account caps disabled (0) should not block; got reason=%s", d.Reason)
	}
}

func TestEvaluateSignal_AccountCapDoesNotFireOnSymbolFigures(t *testing.T) {
	// Regression: per-symbol DailyLoss above its own cap must reject with
	// the per-symbol reason ("daily_loss"), NOT the account one — even when
	// account snapshot is also populated.
	now := time.Now()
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			DailyLossJPY:           9000, // per-symbol breach
			MaxDailyLossJPY:        8000,
			AccountDailyLossJPY:    9000,
			AccountMaxDailyLossJPY: 12000,
		}, mkSummary(0.3))
	if d.Allowed {
		t.Fatalf("expected reject")
	}
	if strings.Contains(d.Reason, "account_daily_loss") {
		t.Errorf("reason should be per-symbol, not account_daily_loss: %q", d.Reason)
	}
	if !strings.Contains(d.Reason, "daily_loss") {
		t.Errorf("reason should be daily_loss (per-symbol): %q", d.Reason)
	}
}

func TestEvaluateHardSafety_AccountDailyLossBlocksOverride(t *testing.T) {
	// Operator override must NOT be able to bypass account_daily_loss —
	// it joins emergency_stop / per-symbol daily_loss as a hard gate.
	now := time.Now()
	d := EvaluateHardSafety(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now),
		AccountSnapshot{
			DailyLossJPY:           0,    // per-symbol is fine
			MaxDailyLossJPY:        8000, // ...
			AccountDailyLossJPY:    13000,
			AccountMaxDailyLossJPY: 12000,
		}, mkSummary(0.3))
	if d.Allowed {
		t.Fatalf("HardSafety must block on account_daily_loss; got Allowed")
	}
	if !strings.Contains(d.Reason, "account_daily_loss") {
		t.Errorf("reason: %q", d.Reason)
	}
}

// ---- reentry cooldown ----
//
// 同 symbol 同 side は「任意の決済(勝ち負け・理由問わず)」から ReentryCooldown の間
// 新規を拒否する。狙い: 利確直後の event_retrigger による同方向の即再 IN を防ぐ
// (一度伸び切った動きの再エントリーは SL を踏みやすい)。反対 side は影響なし・期限切れ/無効(0)は素通し。

func TestEvaluateSignal_ReentryCooldown_BlocksSameSideWithinWindow(t *testing.T) {
	now := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	d := EvaluateSignal(entrySig(order.SideSell), validCfg(config.DirectionBoth, now), AccountSnapshot{
		Now:              now,
		ReentryCooldown:  60 * time.Minute,
		LastSellClosedAt: now.Add(-10 * time.Minute), // 利確 10 分後の同方向再 IN
	}, mkSummary(0.3))
	if d.Allowed || !strings.Contains(d.Reason, "reentry_cooldown") {
		t.Errorf("same-side re-entry within cooldown must be rejected, got: %+v", d)
	}
}

func TestEvaluateSignal_ReentryCooldown_OppositeSidePasses(t *testing.T) {
	now := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		Now:              now,
		ReentryCooldown:  60 * time.Minute,
		LastSellClosedAt: now.Add(-10 * time.Minute), // SELL を閉じた直後の BUY は妨げない
	}, mkSummary(0.3))
	if !d.Allowed {
		t.Errorf("opposite side must pass, got reason=%s", d.Reason)
	}
}

func TestEvaluateSignal_ReentryCooldown_ExpiredPasses(t *testing.T) {
	now := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	d := EvaluateSignal(entrySig(order.SideSell), validCfg(config.DirectionBoth, now), AccountSnapshot{
		Now:              now,
		ReentryCooldown:  60 * time.Minute,
		LastSellClosedAt: now.Add(-61 * time.Minute), // 61 分後は通る
	}, mkSummary(0.3))
	if !d.Allowed {
		t.Errorf("expired cooldown must pass, got reason=%s", d.Reason)
	}
}

func TestEvaluateSignal_ReentryCooldown_DisabledPasses(t *testing.T) {
	now := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	d := EvaluateSignal(entrySig(order.SideSell), validCfg(config.DirectionBoth, now), AccountSnapshot{
		Now:              now,
		ReentryCooldown:  0, // 無効
		LastSellClosedAt: now.Add(-1 * time.Minute),
	}, mkSummary(0.3))
	if !d.Allowed {
		t.Errorf("disabled cooldown must pass, got reason=%s", d.Reason)
	}
}

func TestEvaluateSignal_ReentryCooldown_NotOperatorOverridable(t *testing.T) {
	// 手動 override の allowlist に載せない(自動経路の穴も塞ぐ)。
	// ここでは EvaluateHardSafety ではなく reason 文字列の前方一致仕様だけ固定する:
	// admission の isOverridableReason は明示 allowlist なので "reentry_cooldown" は
	// デフォルト非 override(このテストは reason prefix の契約を守る回帰ガード)。
	now := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), AccountSnapshot{
		Now:             now,
		ReentryCooldown: 60 * time.Minute,
		LastBuyClosedAt: now.Add(-5 * time.Minute),
	}, mkSummary(0.3))
	if d.Allowed || !strings.HasPrefix(d.Reason, "reentry_cooldown") {
		t.Errorf("reason must start with reentry_cooldown, got: %+v", d)
	}
}
