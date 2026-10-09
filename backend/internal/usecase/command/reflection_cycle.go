package command

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"fx-bot/backend/internal/port"
)

// ReflectFunc is the Reflexion brain: given a win/loss summary + the current playbook, it returns a
// revised playbook (newRules) and whether to apply it. nil / update=false / empty rules = keep the
// current playbook (a safe no-op). Transport error fails safe to no-op. Adapter is fake-injectable.
type ReflectFunc = func(ctx context.Context, tradesSummary, currentPlaybook string) (newRules string, update bool, err error)

// ReflectionCycle is the autonomous learning loop. On a LOW cadence it reviews
// recent closed trades, asks the LLM to reflect on win/loss patterns, and — if the update is valid
// AND there is enough evidence — replaces the playbook the decision cycle reads.
//
// It NEVER touches the order path, never calls OnSignal, never writes strategy_configs. Its only
// effect is updating the advisory playbook text. MinTrades is an overfitting guard: a playbook
// rewritten from a handful of noisy trades would just chase noise, so updates require a
// minimum sample.
type ReflectionCycle struct {
	Symbol    string
	MinTrades int // do not rewrite the playbook from fewer than this many closed trades

	RecentTrades    func(ctx context.Context) ([]port.TradeRecord, error)
	CurrentPlaybook func() string
	Reflect         ReflectFunc
	SavePlaybook    func(rules string) error // persist + hot-swap (holder.Set + file append)
	Logger          *slog.Logger
}

// ReflectionResult reports the outcome for logging / status / tests.
type ReflectionResult struct {
	Stage      string // no_reflector | insufficient_n | reflector_error | no_update | updated
	Updated    bool
	TradeCount int
}

func (c *ReflectionCycle) log(msg string, args ...any) {
	if c.Logger != nil {
		c.Logger.Info(msg, args...)
	}
}

// Run executes one reflection cycle. A non-nil error is a transport/IO failure; a "no update" is the
// normal common outcome reported via Stage.
func (c *ReflectionCycle) Run(ctx context.Context) (ReflectionResult, error) {
	if c.Reflect == nil || c.SavePlaybook == nil {
		return ReflectionResult{Stage: "no_reflector"}, nil
	}
	trades, err := c.RecentTrades(ctx)
	if err != nil {
		return ReflectionResult{Stage: "reflector_error"}, fmt.Errorf("recent trades: %w", err)
	}
	if len(trades) < c.MinTrades {
		c.log("reflection_insufficient_n", "symbol", c.Symbol, "n", len(trades), "min", c.MinTrades)
		return ReflectionResult{Stage: "insufficient_n", TradeCount: len(trades)}, nil
	}

	summary := summarizeTradesForReflection(c.Symbol, trades)
	cur := ""
	if c.CurrentPlaybook != nil {
		cur = c.CurrentPlaybook()
	}
	newRules, update, rerr := c.Reflect(ctx, summary, cur)
	if rerr != nil {
		return ReflectionResult{Stage: "reflector_error", TradeCount: len(trades)}, fmt.Errorf("reflect: %w", rerr)
	}
	if !update || strings.TrimSpace(newRules) == "" {
		c.log("reflection_no_update", "symbol", c.Symbol, "n", len(trades))
		return ReflectionResult{Stage: "no_update", TradeCount: len(trades)}, nil
	}
	if err := c.SavePlaybook(newRules); err != nil {
		return ReflectionResult{Stage: "reflector_error", TradeCount: len(trades)}, fmt.Errorf("save playbook: %w", err)
	}
	c.log("reflection_playbook_updated", "symbol", c.Symbol, "n", len(trades))
	return ReflectionResult{Stage: "updated", Updated: true, TradeCount: len(trades)}, nil
}

// summarizeTradesForReflection renders a compact win/loss digest for the reflection
// prompt. Pure. Win/loss is judged on NET P&L (gross − GMO fee + swap), NOT gross —
// the ~¥8 round-trip fee is a real edge drag, so a gross-positive trade that the fee
// eats must count as a loss, or the loop over-estimates the edge.
func summarizeTradesForReflection(symbol string, trades []port.TradeRecord) string {
	var wins, losses int
	var netJPY, grossJPY, feeJPY, netPips float64
	var b strings.Builder
	for _, t := range trades {
		net := t.ProfitLossJPY - t.FeeJPY + t.SwapJPY
		netJPY += net
		grossJPY += t.ProfitLossJPY
		feeJPY += t.FeeJPY
		netPips += t.ProfitLossPips
		if net > 0 {
			wins++
		} else if net < 0 {
			losses++
		}
	}
	winRate := 0.0
	if n := len(trades); n > 0 {
		winRate = 100 * float64(wins) / float64(n)
	}
	fmt.Fprintf(&b, "symbol=%s n=%d win=%d loss=%d win_rate=%.1f%% net_jpy=%.1f gross_jpy=%.1f fee_jpy=%.1f net_pips=%.1f (win/loss & net_jpy are NET of GMO fee)\n",
		symbol, len(trades), wins, losses, winRate, netJPY, grossJPY, feeJPY, netPips)
	b.WriteString("recent trades (entry_jst, hold, side, net_jpy, fee, pips, close_reason):\n")
	max := len(trades)
	if max > 30 {
		max = 30
	}
	// Entry time in JST + hold duration: the panel is asked to stratify
	// by session/time-of-day, and losses can be time-shaped (e.g. JST 0-5 chase BUYs) —
	// impossible to see without
	// timestamps. JST because every veto/rule is written in JST. Zero times (old
	// rows) omit the fields rather than rendering the 0001-01-01 zero value.
	jst := time.FixedZone("JST", 9*60*60)
	for _, t := range trades[:max] {
		net := t.ProfitLossJPY - t.FeeJPY + t.SwapJPY
		when := ""
		if !t.OpenedAt.IsZero() {
			when = t.OpenedAt.In(jst).Format("01-02 15:04") + "JST "
			if !t.ClosedAt.IsZero() {
				when += fmt.Sprintf("hold=%.1fh ", t.ClosedAt.Sub(t.OpenedAt).Hours())
			}
		}
		fmt.Fprintf(&b, "- %s%s net=%.1fJPY(fee%.1f) %.1fpip close=%q\n", when, t.Side, net, t.FeeJPY, t.ProfitLossPips, t.CloseReason)
	}
	return b.String()
}
