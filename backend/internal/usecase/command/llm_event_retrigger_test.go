package command

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"fx-bot/backend/internal/domain/market"
)

// LLMEventRetrigger: on top of the hourly LLM cycle,
// re-run the SAME all-pairs judgment when a position closes (the freed slot is
// re-examined now, not up to 59min later) or when the price moves hard (the last
// hourly judgment is stale). It never runs a cycle itself — it only calls the
// injected Trigger (the manual-button path: weekend gate + run-guard), so a
// running scheduled cycle always wins. Cooldowns are armed on every fire ATTEMPT
// so a refused trigger cannot be re-hammered every tick.

func retriggerCandles1m(now time.Time, n int, closes func(i int) float64) []market.Candle {
	out := make([]market.Candle, n)
	for i := 0; i < n; i++ {
		out[i] = market.Candle{
			OpenTime: now.Add(-time.Duration(n-i) * time.Minute),
			Close:    closes(i),
		}
	}
	return out
}

func newTestRetrigger(now *time.Time, fired *[]string, triggerErr error) *LLMEventRetrigger {
	return &LLMEventRetrigger{
		OnPositionClose: true,
		MovePips:        25,
		MoveWindow:      30 * time.Minute,
		MoveCooldown:    20 * time.Minute,
		CloseCooldown:   3 * time.Minute,
		Trigger: func(reason string) error {
			*fired = append(*fired, reason)
			return triggerErr
		},
		Now:    func() time.Time { return *now },
		Logger: slog.Default(),
	}
}

func TestLLMEventRetrigger_MoveFiresAboveThreshold(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	var fired []string
	r := newTestRetrigger(&now, &fired, nil)

	// 60 flat 1m candles at 145.00, mid now 145.30 = +30 pips over any window ≥ threshold 25.
	candles := retriggerCandles1m(now, 60, func(int) float64 { return 145.00 })
	r.OnPriceTick("USD_JPY", 145.30, func() []market.Candle { return candles })

	if len(fired) != 1 {
		t.Fatalf("fired = %v, want exactly 1", fired)
	}
	if want := "big_move:USD_JPY"; len(fired[0]) < len(want) || fired[0][:len(want)] != want {
		t.Errorf("reason = %q, want prefix %q", fired[0], want)
	}
}

func TestLLMEventRetrigger_MoveBelowThresholdNoFire(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	var fired []string
	r := newTestRetrigger(&now, &fired, nil)

	candles := retriggerCandles1m(now, 60, func(int) float64 { return 145.00 })
	r.OnPriceTick("USD_JPY", 145.10, func() []market.Candle { return candles }) // +10 pips < 25

	if len(fired) != 0 {
		t.Fatalf("fired = %v, want none", fired)
	}
}

// SELL 方向 (下落) も絶対値で判定する。
func TestLLMEventRetrigger_MoveFiresOnDrop(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	var fired []string
	r := newTestRetrigger(&now, &fired, nil)

	candles := retriggerCandles1m(now, 60, func(int) float64 { return 145.00 })
	r.OnPriceTick("USD_JPY", 144.70, func() []market.Candle { return candles }) // −30 pips

	if len(fired) != 1 {
		t.Fatalf("fired = %v, want exactly 1", fired)
	}
}

// 起動直後など window 分の履歴が無いときは判定しない (誤発火防止・fail closed)。
func TestLLMEventRetrigger_MoveInsufficientHistoryNoFire(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	var fired []string
	r := newTestRetrigger(&now, &fired, nil)

	candles := retriggerCandles1m(now, 10, func(int) float64 { return 145.00 }) // 10m < 30m window
	r.OnPriceTick("USD_JPY", 145.50, func() []market.Candle { return candles })

	if len(fired) != 0 {
		t.Fatalf("fired = %v, want none (insufficient history)", fired)
	}
}

// 1秒ループから毎 tick 呼ばれるので、candle 取得は per-symbol throttle (10s) の
// 間隔でしか行わない — 2 tick 目は candles getter すら呼ばれない。
func TestLLMEventRetrigger_MoveCheckThrottled(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	var fired []string
	r := newTestRetrigger(&now, &fired, nil)
	r.MovePips = 9999 // never fires; we only observe the getter calls

	calls := 0
	getter := func() []market.Candle {
		calls++
		return retriggerCandles1m(now, 60, func(int) float64 { return 145.00 })
	}
	r.OnPriceTick("USD_JPY", 145.00, getter)
	now = now.Add(time.Second)
	r.OnPriceTick("USD_JPY", 145.00, getter)
	if calls != 1 {
		t.Fatalf("candle getter calls = %d, want 1 (second tick throttled)", calls)
	}
	now = now.Add(11 * time.Second)
	r.OnPriceTick("USD_JPY", 145.00, getter)
	if calls != 2 {
		t.Fatalf("candle getter calls = %d, want 2 after throttle window", calls)
	}
}

// クールダウン中は動きっぱなしでも再発火しない。明けたら再発火する。
// Trigger がエラー (run-guard 拒否等) でもクールダウンは腕arm される。
func TestLLMEventRetrigger_MoveCooldown(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	var fired []string
	r := newTestRetrigger(&now, &fired, nil)

	getter := func() []market.Candle {
		return retriggerCandles1m(now, 60, func(int) float64 { return 145.00 })
	}
	r.OnPriceTick("USD_JPY", 145.30, getter)
	now = now.Add(11 * time.Second) // past the check throttle, inside cooldown
	r.OnPriceTick("USD_JPY", 145.35, getter)
	// 別 symbol でもクールダウンは global (直前に全ペア再判断済みだから)。
	r.OnPriceTick("EUR_JPY", 999.99, func() []market.Candle {
		return retriggerCandles1m(now, 60, func(int) float64 { return 900.00 })
	})
	if len(fired) != 1 {
		t.Fatalf("fired = %v, want 1 (cooldown suppresses)", fired)
	}
	now = now.Add(21 * time.Minute) // cooldown (20m) elapsed
	r.OnPriceTick("USD_JPY", 145.35, getter)
	if len(fired) != 2 {
		t.Fatalf("fired = %v, want 2 after cooldown", fired)
	}
}

func TestLLMEventRetrigger_MoveDisabledByZeroPips(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	var fired []string
	r := newTestRetrigger(&now, &fired, nil)
	r.MovePips = 0

	r.OnPriceTick("USD_JPY", 145.30, func() []market.Candle {
		return retriggerCandles1m(now, 60, func(int) float64 { return 145.00 })
	})
	if len(fired) != 0 {
		t.Fatalf("fired = %v, want none (move trigger off)", fired)
	}
}

func TestLLMEventRetrigger_PositionCloseFires(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	var fired []string
	r := newTestRetrigger(&now, &fired, nil)

	r.OnPositionClosed("USD_JPY", "take_profit")
	if len(fired) != 1 {
		t.Fatalf("fired = %v, want 1", fired)
	}
	if want := "position_closed:USD_JPY take_profit"; fired[0] != want {
		t.Errorf("reason = %q, want %q", fired[0], want)
	}
}

// 同時多発決済 (複数ペア OCO 連鎖) は CloseCooldown 内で 1 回に dedupe。
func TestLLMEventRetrigger_PositionCloseCooldown(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	var fired []string
	r := newTestRetrigger(&now, &fired, nil)

	r.OnPositionClosed("USD_JPY", "take_profit")
	now = now.Add(30 * time.Second)
	r.OnPositionClosed("EUR_JPY", "stop_loss")
	if len(fired) != 1 {
		t.Fatalf("fired = %v, want 1 (dedupe within close cooldown)", fired)
	}
	now = now.Add(4 * time.Minute)
	r.OnPositionClosed("GBP_JPY", "stop_loss")
	if len(fired) != 2 {
		t.Fatalf("fired = %v, want 2 after close cooldown", fired)
	}
}

func TestLLMEventRetrigger_PositionCloseDisabled(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	var fired []string
	r := newTestRetrigger(&now, &fired, nil)
	r.OnPositionClose = false

	r.OnPositionClosed("USD_JPY", "take_profit")
	if len(fired) != 0 {
		t.Fatalf("fired = %v, want none", fired)
	}
}

// Trigger 未配線 (nil) でも panic しない — wiring の起動順序都合で一瞬 nil があり得る。
func TestLLMEventRetrigger_NilTriggerSafe(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	r := &LLMEventRetrigger{
		OnPositionClose: true,
		MovePips:        25,
		MoveWindow:      30 * time.Minute,
		Now:             func() time.Time { return now },
		Logger:          slog.Default(),
	}
	r.OnPositionClosed("USD_JPY", "take_profit")
	r.OnPriceTick("USD_JPY", 145.30, func() []market.Candle {
		return retriggerCandles1m(now, 60, func(int) float64 { return 145.00 })
	})
}

// fired/refused ログは WARN で出す: LOG_LEVEL=warn 運用では INFO だとログから
// 消えて「発火したのか拒否されたのか」を後から検証できない。回帰防止に WARN 固定。
type levelRecorder struct {
	slog.Handler
	levels *[]slog.Level
	msgs   *[]string
}

func (h levelRecorder) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (h levelRecorder) Handle(_ context.Context, r slog.Record) error {
	*h.levels = append(*h.levels, r.Level)
	*h.msgs = append(*h.msgs, r.Message)
	return nil
}

func TestLLMEventRetrigger_FiredAndRefusedLogAtWarn(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	var levels []slog.Level
	var msgs []string
	logger := slog.New(levelRecorder{levels: &levels, msgs: &msgs})

	var fired []string
	r := newTestRetrigger(&now, &fired, nil)
	r.Logger = logger
	r.OnPositionClosed("USD_JPY", "take_profit") // → fired

	now = now.Add(4 * time.Minute)
	r.Trigger = func(reason string) error { return errors.New("判断サイクルを実行中です") }
	r.OnPositionClosed("EUR_JPY", "stop_loss") // → refused

	if len(msgs) != 2 || msgs[0] != "llm_event_retrigger_fired" || msgs[1] != "llm_event_retrigger_refused" {
		t.Fatalf("msgs = %v, want [llm_event_retrigger_fired llm_event_retrigger_refused]", msgs)
	}
	for i, l := range levels {
		if l != slog.LevelWarn {
			t.Errorf("log %q level = %v, want WARN (LOG_LEVEL=warn でも可視であること)", msgs[i], l)
		}
	}
}
