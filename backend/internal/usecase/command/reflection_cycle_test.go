package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

// The reflection loop must learn from NET P&L (gross − GMO fee + swap), not gross —
// otherwise a trade that is gross-positive but eaten by the ~¥8 round-trip fee is
// mislabelled a "win" and the loop over-estimates the edge.
func TestSummarizeTradesForReflection_NetOfFees(t *testing.T) {
	trades := []port.TradeRecord{
		{Side: "BUY", ProfitLossJPY: 5, FeeJPY: 8, ProfitLossPips: 0.5},   // gross +5, net −3 → LOSS
		{Side: "SELL", ProfitLossJPY: -60, FeeJPY: 7, ProfitLossPips: -6}, // net −67 LOSS
		{Side: "BUY", ProfitLossJPY: 40, FeeJPY: 6, ProfitLossPips: 4},    // net +34 WIN
	}
	out := summarizeTradesForReflection("USD_JPY", trades)
	if !strings.Contains(out, "win=1 loss=2") {
		t.Errorf("win/loss must be NET of fee (gross +5 eaten by ¥8 fee = loss); got:\n%s", out)
	}
	// net = (5−8)+(−60−7)+(40−6) = −36; fee total = 21.
	if !strings.Contains(out, "net_jpy=-36.0") || !strings.Contains(out, "fee_jpy=21.0") {
		t.Errorf("digest must surface net_jpy + fee_jpy drag; got:\n%s", out)
	}
}

func mkTrades(n int) []port.TradeRecord {
	out := make([]port.TradeRecord, n)
	for i := range out {
		out[i] = port.TradeRecord{Symbol: "USD_JPY", Side: "BUY", ProfitLossPips: 1, ProfitLossJPY: 10}
	}
	return out
}

func TestReflectionCycle_InsufficientN_NoUpdate(t *testing.T) {
	saved := 0
	c := &ReflectionCycle{
		Symbol: "USD_JPY", MinTrades: 20,
		RecentTrades:    func(_ context.Context) ([]port.TradeRecord, error) { return mkTrades(5), nil },
		CurrentPlaybook: func() string { return "cur" },
		Reflect: func(_ context.Context, _, _ string) (string, bool, error) {
			t.Fatal("Reflect must not be called below MinTrades")
			return "", false, nil
		},
		SavePlaybook: func(string) error { saved++; return nil },
	}
	r, err := c.Run(context.Background())
	if err != nil || r.Stage != "insufficient_n" || saved != 0 {
		t.Fatalf("stage=%q err=%v saved=%d", r.Stage, err, saved)
	}
}

func TestReflectionCycle_NoUpdate_DoesNotSave(t *testing.T) {
	saved := 0
	c := &ReflectionCycle{
		Symbol: "USD_JPY", MinTrades: 10,
		RecentTrades: func(_ context.Context) ([]port.TradeRecord, error) { return mkTrades(20), nil },
		Reflect:      func(_ context.Context, _, _ string) (string, bool, error) { return "", false, nil },
		SavePlaybook: func(string) error { saved++; return nil },
	}
	r, _ := c.Run(context.Background())
	if r.Stage != "no_update" || saved != 0 {
		t.Fatalf("stage=%q saved=%d", r.Stage, saved)
	}
}

func TestReflectionCycle_ValidUpdate_Saves(t *testing.T) {
	var savedRules string
	saved := 0
	c := &ReflectionCycle{
		Symbol: "USD_JPY", MinTrades: 10,
		RecentTrades: func(_ context.Context) ([]port.TradeRecord, error) { return mkTrades(25), nil },
		Reflect: func(_ context.Context, summary, _ string) (string, bool, error) {
			if summary == "" {
				t.Error("summary should be non-empty")
			}
			return "- 上昇トレンドの押し目だけ買う\n- 急騰直後の飛び乗りはやめる", true, nil
		},
		SavePlaybook: func(r string) error { saved++; savedRules = r; return nil },
	}
	r, err := c.Run(context.Background())
	if err != nil || r.Stage != "updated" || !r.Updated || saved != 1 {
		t.Fatalf("stage=%q updated=%v saved=%d err=%v", r.Stage, r.Updated, saved, err)
	}
	if savedRules == "" {
		t.Error("saved rules should carry the reflected playbook")
	}
}

// The reflection panel is ASKED to stratify by session/time-of-day (reflection-regime.md),
// and losses can be strongly time-of-day shaped (e.g. late-night chase BUYs) — but the
// digest carried no timestamps, so the panel could only
// hallucinate time patterns. Each trade line must carry entry time in JST (the tz every
// rule/veto is written in) + hold duration; missing timestamps must not render "0001-01-01".
func TestSummarizeTradesForReflection_EntryTimeJST(t *testing.T) {
	opened := time.Date(2026, 6, 30, 15, 5, 0, 0, time.UTC) // = 07-01 00:05 JST
	closed := opened.Add(90 * time.Minute)
	trades := []port.TradeRecord{
		{Side: "BUY", ProfitLossJPY: -250, ProfitLossPips: -25, CloseReason: "stop_loss", OpenedAt: opened, ClosedAt: closed},
		{Side: "SELL", ProfitLossJPY: 100, ProfitLossPips: 10, CloseReason: "take_profit"}, // no timestamps (old rows)
	}
	out := summarizeTradesForReflection("GBP_JPY", trades)
	if !strings.Contains(out, "07-01 00:05JST") {
		t.Errorf("digest must show entry time in JST; got:\n%s", out)
	}
	if !strings.Contains(out, "hold=1.5h") {
		t.Errorf("digest must show hold duration; got:\n%s", out)
	}
	if strings.Contains(out, "0001-01-01") {
		t.Errorf("zero timestamps must be omitted, not rendered; got:\n%s", out)
	}
}
