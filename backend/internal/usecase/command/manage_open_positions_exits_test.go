package command

import (
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

// evaluateExit() の 124 行ネストを 3 つの
// 純粋関数 (ratchet / maxhold / tpsl) に Strategy パターンで分離する。
//
// 旧 evaluateExit の全 case は既存 TestEvaluateExit_* (manage_open_positions_test.go)
// が網羅しているのでそちらを behavior-preserving の safety net として使う。
// このファイルは各 evaluator が「単体で呼べる純粋関数」になっていることだけを
// 確認する最小テスト。

func TestEvaluateRatchetExit_FiresWhenArmedAndRetraced(t *testing.T) {
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0, Quantity: 1000,
		RatchetArmPips: 5, RatchetGivebackPips: 3, RatchetArmed: true,
		PeakUnrealizedPips: 11.0,
	}
	tk := market.Ticker{Bid: 100.08, Ask: 100.09}

	if got := evaluateRatchetExit(rec, tk, 0.01); got != "ratchet_takeprofit" {
		t.Errorf("got %q want ratchet_takeprofit", got)
	}
}

func TestEvaluateRatchetExit_NotArmed_NoFire(t *testing.T) {
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0, Quantity: 1000,
		RatchetArmPips: 5, RatchetGivebackPips: 3, RatchetArmed: false,
		PeakUnrealizedPips: 11.0,
	}
	tk := market.Ticker{Bid: 100.08, Ask: 100.09}

	if got := evaluateRatchetExit(rec, tk, 0.01); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

// --- Loss-side ratchet (trailing stop, mirror of profit ratchet) ---
// 含み損の trough を追い、arm 到達後に giveback だけ戻ったら "ratchet_stoploss"。

func TestEvaluateLossRatchetExit_FiresWhenArmedAndRecovered(t *testing.T) {
	// BUY entry 100.00, pip 0.01. trough -20pips (bid 99.80), 現在 bid 99.88
	// = -12pips。trough から +8pips 戻った → giveback 8 到達で損切り。
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0, Quantity: 1000,
		RatchetArmPips: 14, RatchetGivebackPips: 8, LossRatchetArmed: true,
		TroughUnrealizedPips: -20.0,
	}
	tk := market.Ticker{Bid: 99.88, Ask: 99.89} // BUY exit=bid → -12pips
	if got := evaluateLossRatchetExit(rec, tk, 0.01); got != "ratchet_stoploss" {
		t.Errorf("got %q want ratchet_stoploss", got)
	}
}

func TestEvaluateLossRatchetExit_NotArmed_NoFire(t *testing.T) {
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0, Quantity: 1000,
		RatchetArmPips: 14, RatchetGivebackPips: 8, LossRatchetArmed: false,
		TroughUnrealizedPips: -20.0,
	}
	tk := market.Ticker{Bid: 99.88, Ask: 99.89}
	if got := evaluateLossRatchetExit(rec, tk, 0.01); got != "" {
		t.Errorf("got %q want empty (not armed)", got)
	}
}

func TestEvaluateLossRatchetExit_ArmedButStillDeep_NoFire(t *testing.T) {
	// trough -20, 現在 -15 = +5pips 戻り (< giveback 8) → まだ我慢。
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0, Quantity: 1000,
		RatchetArmPips: 14, RatchetGivebackPips: 8, LossRatchetArmed: true,
		TroughUnrealizedPips: -20.0,
	}
	tk := market.Ticker{Bid: 99.85, Ask: 99.86} // -15pips, recovered only +5
	if got := evaluateLossRatchetExit(rec, tk, 0.01); got != "" {
		t.Errorf("got %q want empty (recovered < giveback)", got)
	}
}

func TestEvaluateLossRatchetExit_SellSide(t *testing.T) {
	// SELL entry 100.00. loss = ask above entry. trough -20 (ask 100.20),
	// 現在 ask 100.12 = -12pips → +8 recovered → fire.
	rec := port.PositionRecord{
		Side: "SELL", EntryPrice: 100.0, Quantity: 1000,
		RatchetArmPips: 14, RatchetGivebackPips: 8, LossRatchetArmed: true,
		TroughUnrealizedPips: -20.0,
	}
	tk := market.Ticker{Bid: 100.11, Ask: 100.12} // SELL exit=ask → -12pips
	if got := evaluateLossRatchetExit(rec, tk, 0.01); got != "ratchet_stoploss" {
		t.Errorf("got %q want ratchet_stoploss", got)
	}
}

func TestEvaluateLossRatchetExit_FeatureOff_NoFire(t *testing.T) {
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0, Quantity: 1000,
		RatchetArmPips: 0, RatchetGivebackPips: 0, LossRatchetArmed: true,
		TroughUnrealizedPips: -20.0,
	}
	tk := market.Ticker{Bid: 99.88, Ask: 99.89}
	if got := evaluateLossRatchetExit(rec, tk, 0.01); got != "" {
		t.Errorf("got %q want empty (feature off)", got)
	}
}

// Early-exit window fires with a DISTINCT "early_exit" close_reason so it is
// separable from genuine soft/hard deadline "max_hold" closes.
func TestEvaluateMaxHoldExit_EarlyExitWindow_ReturnsEarlyExit(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0,
		MaxHoldMinutes:         60,
		EarlyExitWindowMinutes: 10, // window opens at elapsed >= 50min
		EarlyExitTargetPips:    -2,
		OpenedAt:               now.Add(-55 * time.Minute), // inside window, before soft
	}
	tk := market.Ticker{Bid: 99.99, Ask: 100.0} // mid=99.995 → -0.5 pips >= -2 target
	if got := evaluateMaxHoldExit(rec, tk, now, 0.01); got != "early_exit" {
		t.Errorf("early-exit window: got %q want early_exit", got)
	}
}

func TestEvaluateMaxHoldExit_HardDeadlineExpired(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0,
		MaxHoldMinutes:      60,
		ExtensionMaxMinutes: 30, // hard = 90min
		OpenedAt:            now.Add(-91 * time.Minute),
	}
	tk := market.Ticker{Bid: 100.0, Ask: 100.01}
	if got := evaluateMaxHoldExit(rec, tk, now, 0.01); got != "max_hold" {
		t.Errorf("hard deadline: got %q want max_hold", got)
	}
}

func TestEvaluateMaxHoldExit_NotYetExpired(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0,
		MaxHoldMinutes: 60,
		OpenedAt:       now.Add(-30 * time.Minute), // half-way
	}
	tk := market.Ticker{Bid: 100.0, Ask: 100.01}
	if got := evaluateMaxHoldExit(rec, tk, now, 0.01); got != "" {
		t.Errorf("still in window: got %q want empty", got)
	}
}

func TestEvaluatePaperTPSLExit_TPHit(t *testing.T) {
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0,
		TakeProfitPips: 20, StopLossPips: 15,
	}
	tk := market.Ticker{Bid: 100.20, Ask: 100.21} // bid >= entry + 20*0.01
	if got := evaluatePaperTPSLExit(rec, tk, 0.01); got != "take_profit" {
		t.Errorf("got %q want take_profit", got)
	}
}

func TestEvaluatePaperTPSLExit_SLHit(t *testing.T) {
	rec := port.PositionRecord{
		Side: "SELL", EntryPrice: 100.0,
		TakeProfitPips: 20, StopLossPips: 15,
	}
	tk := market.Ticker{Bid: 100.14, Ask: 100.15}
	if got := evaluatePaperTPSLExit(rec, tk, 0.01); got != "stop_loss" {
		t.Errorf("got %q want stop_loss", got)
	}
}

func TestEvaluatePaperTPSLExit_NoHit(t *testing.T) {
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0,
		TakeProfitPips: 20, StopLossPips: 15,
	}
	tk := market.Ticker{Bid: 100.05, Ask: 100.06}
	if got := evaluatePaperTPSLExit(rec, tk, 0.01); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

// --- session flatten ---
//
// 毎朝 05:45 JST 前後に GMO のスプレッドが 10pips 超へ開く「壁」(bid が建値付近でも
// ask ジャンプで SELL の SL が刈られうる) の 15 分前に、
// 残っている OPEN 玉を通常スプレッドで手仕舞う。窓 = [start, start+30min)。
// 土曜のこの回が金曜カットオフ (週末ギャップ回避) の実装を兼ねる。

const flattenStart0530 = 5*60 + 30 // 05:30 JST

func TestEvaluateSessionFlattenExit_FiresInsideWindow(t *testing.T) {
	rec := port.PositionRecord{
		Side: "SELL", EntryPrice: 218.772, Quantity: 1000,
		// 02:01 JST 建て。UTC では前日 17:01。
		OpenedAt: time.Date(2026, 7, 16, 17, 1, 0, 0, time.UTC),
	}
	// 05:35 JST = 20:35 UTC (前日)
	now := time.Date(2026, 7, 16, 20, 35, 0, 0, time.UTC)
	if got := evaluateSessionFlattenExit(rec, now, flattenStart0530); got != "session_flatten" {
		t.Errorf("got %q want session_flatten", got)
	}
}

func TestEvaluateSessionFlattenExit_BeforeWindow_NoFire(t *testing.T) {
	rec := port.PositionRecord{
		Side: "SELL", EntryPrice: 218.772, Quantity: 1000,
		OpenedAt: time.Date(2026, 7, 16, 17, 1, 0, 0, time.UTC),
	}
	// 05:29 JST → まだ窓の外
	now := time.Date(2026, 7, 16, 20, 29, 0, 0, time.UTC)
	if got := evaluateSessionFlattenExit(rec, now, flattenStart0530); got != "" {
		t.Errorf("got %q want empty (before window)", got)
	}
}

func TestEvaluateSessionFlattenExit_AfterWindow_NoFire(t *testing.T) {
	// 窓 (30 分) を過ぎたら掃除しない: bot が窓の間ずっと死んでいた場合は
	// broker OCO が守る (見つけた状態で余計なことをしない)。06:01 JST。
	rec := port.PositionRecord{
		Side: "SELL", EntryPrice: 218.772, Quantity: 1000,
		OpenedAt: time.Date(2026, 7, 16, 17, 1, 0, 0, time.UTC),
	}
	now := time.Date(2026, 7, 16, 21, 1, 0, 0, time.UTC)
	if got := evaluateSessionFlattenExit(rec, now, flattenStart0530); got != "" {
		t.Errorf("got %q want empty (after window)", got)
	}
}

func TestEvaluateSessionFlattenExit_OpenedInsideWindow_NoFire(t *testing.T) {
	// 窓の中で建った玉は掃除しない (防御的: 02-06 時新規禁止で本来起きないが、
	// paper/手動経路で起きても「建てた直後に強制 close」の自食いを防ぐ)。
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0, Quantity: 1000,
		OpenedAt: time.Date(2026, 7, 16, 20, 32, 0, 0, time.UTC), // 05:32 JST
	}
	now := time.Date(2026, 7, 16, 20, 40, 0, 0, time.UTC) // 05:40 JST
	if got := evaluateSessionFlattenExit(rec, now, flattenStart0530); got != "" {
		t.Errorf("got %q want empty (opened inside window)", got)
	}
}

// evaluateExit 優先順位: 窓内では ratchet より先に session_flatten が勝つ
// (壁の直前 tick で ratchet が広スプレッドへ market close する事故ラベルを避け、
// 集計が「セッション掃除」と読めるようにする)。
func TestEvaluateExit_SessionFlattenWinsOverRatchet(t *testing.T) {
	u := &ManageOpenPositions{
		Symbol: "GBP_JPY", PipSize: 0.01, Mode: config.ModeLiveConfig,
		SessionFlattenEnabled:        true,
		SessionFlattenStartMinuteJST: flattenStart0530,
	}
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0, Quantity: 1000, Status: port.PositionStatusOpen,
		OpenedAt:       time.Date(2026, 7, 16, 17, 1, 0, 0, time.UTC),
		RatchetArmPips: 5, RatchetGivebackPips: 3, RatchetArmed: true, PeakUnrealizedPips: 20,
	}
	tk := market.Ticker{Bid: 100.08, Ask: 100.09}         // ratchet giveback も発火する状態
	now := time.Date(2026, 7, 16, 20, 35, 0, 0, time.UTC) // 05:35 JST
	if got := u.evaluateExit(rec, tk, now); got != "session_flatten" {
		t.Errorf("got %q want session_flatten (highest priority in window)", got)
	}
}

func TestEvaluateExit_SessionFlattenDisabled_ZeroValueOff(t *testing.T) {
	// zero-value (フラグ未設定) では絶対に発火しない — 既存テスト/既存挙動の保護。
	u := &ManageOpenPositions{Symbol: "USD_JPY", PipSize: 0.01, Mode: config.ModeLiveConfig}
	rec := port.PositionRecord{
		Side: "BUY", EntryPrice: 100.0, Quantity: 1000, Status: port.PositionStatusOpen,
		OpenedAt: time.Date(2026, 7, 16, 17, 1, 0, 0, time.UTC),
	}
	tk := market.Ticker{Bid: 100.00, Ask: 100.01}
	now := time.Date(2026, 7, 16, 20, 35, 0, 0, time.UTC)
	if got := u.evaluateExit(rec, tk, now); got != "" {
		t.Errorf("got %q want empty (feature off by default)", got)
	}
}
