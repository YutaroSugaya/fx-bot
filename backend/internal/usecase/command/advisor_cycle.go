package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"gopkg.in/yaml.v3"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase"
)

// ErrAdvisorSymbolMismatch indicates the advisor returned a YAML for a
// symbol other than the one this AdvisorCycle is pinned to. Wrapped in the
// error returned by Run so callers (scheduler tick / manual trigger) can
// match it with errors.Is for routing.
var ErrAdvisorSymbolMismatch = errors.New("advisor symbol mismatch")

// AdvisorCycle runs one Claude advisor invocation followed by config
// promotion. It returns the promotion result so callers can log/notify on
// outcome.
//
// Workflow:
//  1. Build a MarketSummary from the aggregator + ticker + repos
//  2. advisor.Generate(summary) → AdvisorRun
//  3. Persist the AdvisorRun (DB)
//  4. If status != success → return (promotion skipped)
//  5. Promoter.PromoteFromYAML(run.ParsedYAML, accountState)
//
// The caller (app layer) plugs this into the hourly scheduler.
type AdvisorCycle struct {
	// Symbol identifies which symbol bundle this cycle belongs to. Used as
	// log context so multi-symbol parallel cycles produce attributable logs.
	// The Promoter (pinned via Promoter.ExpectedSymbol) is the authoritative
	// reject point for symbol mismatch; this field is for observability.
	Symbol          string
	Advisor         port.Advisor
	Promoter        *Promoter
	Notifier        port.Notifier
	AdvisorRuns     port.AdvisorRunRepository
	MarketSummaries port.MarketSummaryRepository
	BotConfig       *config.BotConfig
	HardLimits      *config.HardLimits
	BuildSummary    func() *market.MarketSummary
	// GetAccountState returns the live account state for the risk-validation
	// pass. Callers MUST return a non-nil error
	// when any underlying DB query fails so promotion can reject instead of
	// validating against zero-valued open_positions / daily_loss (a
	// func() AccountState signature would silently swallow DB errors).
	GetAccountState func() (config.AccountState, error)
	Logger          *slog.Logger

	// OnRunComplete は AdvisorRun を Insert した直後に毎回 fire される
	// 観測フック。cmd/bot が symbol を closure で持って
	// app.ParseErrorMonitor.Record(symbol, status, usageLimited) に転送し、
	// scheduler の nextScheduleInterval が「失敗中なら 10 分後再走」する判断を
	// する。usageLimited=true (session/usage limit) のときは fast-retry しない
	// (数時間戻らないので叩くだけ無駄)。nil 許容 (テスト/legacy 配線で未設定)。
	OnRunComplete func(status port.AdvisorRunStatus, usageLimited bool)

	// parseErrorStreak は連続 parse_error の計数。Run() で状況を見て増減する:
	//   - status=parse_error  → streak++
	//   - status=success      → streak=0 (回復で reset)
	//   - その他 failure      → streak=0 (parse_error 限定の streak)
	// streak が parseErrorAlertThreshold に達した瞬間に Error 通知を 1 度だけ
	// 出す (streak がさらに伸びても重複通知はしない、threshold に到達した時点で
	// 既に運用者は気づいている前提)。
	parseErrorStreak int
}

// parseErrorAlertThreshold: parse_error が連続でこの回数到達すると
// notifier に Error-level alert を送る。初回失敗後は parseErrorRetryInterval (10 分)
// 間隔で再走するため数十分以内に検知 (parse_error の連続は dashboard だけでは気づきにくいため)。
const parseErrorAlertThreshold = 3

// Run executes exactly one cycle. source は発火源 (auto/manual/event)。
// 空文字は auto 扱い (テスト互換)。
func (u *AdvisorCycle) Run(ctx context.Context, source port.AdvisorRunSource) (*PromotionResult, error) {
	if source == "" {
		source = port.AdvisorRunSourceAuto
	}
	u.Logger.Info("advisor_cycle_start", "source", string(source), "symbol", u.Symbol)
	summary := u.BuildSummary()
	// スプレッド 2 倍以上を観測したら
	// 必ず Warn ログを残す。Claude がこのとき no_trade を出してくれているかを
	// 後で AdvisorRun の output_yaml と突き合わせて検証できるようにする
	// (= 「通り抜け疑い」可視化)。
	if market.IsSpreadSpike(summary) {
		u.Logger.Warn("spread_spike_observed",
			"symbol", u.Symbol,
			"current_spread_pips", summary.CurrentRate.SpreadPips,
			"avg_1h_spread_pips", summary.Summary1h.AvgSpreadPips,
			"ratio", summary.CurrentRate.SpreadPips/summary.Summary1h.AvgSpreadPips)
	}
	if u.MarketSummaries != nil {
		_ = usecase.SaveMarketSummaryDB(ctx, u.MarketSummaries, summary, "1h")
	}

	run, err := u.Advisor.Generate(ctx, summary)
	if err != nil {
		u.Logger.Error("advisor_generate_failed", "err", err)
		return nil, err
	}
	if u.AdvisorRuns != nil {
		_ = u.AdvisorRuns.Insert(ctx, port.AdvisorRunRecord{
			RunID:        run.RunID,
			Provider:     "claude_cli",
			Mode:         string(u.BotConfig.Bot.Mode),
			PromptPath:   u.BotConfig.AIAdvisor.PromptPath,
			InputJSON:    run.InputJSON,
			OutputYAML:   run.OutputYAML,
			Status:       run.Status,
			ErrorMessage: run.ErrorMsg,
			Source:       source,
			StartedAt:    run.StartedAt,
			FinishedAt:   run.FinishedAt,
		})
	}
	// OnRunComplete hook: 状態に関係なく毎回 fire (success / parse_error / timeout 等)。
	// 後段の早期 return path で呼び忘れないようここで集約する。
	if u.OnRunComplete != nil {
		u.OnRunComplete(run.Status, run.UsageLimited)
	}

	if run.Status != port.AdvisorRunStatusSuccess {
		u.Logger.Warn("advisor_non_success", "status", string(run.Status), "err", run.ErrorMsg)
		u.notify(ctx, port.LevelWarn, "advisor_failed",
			"Claude advisor returned status="+string(run.Status),
			map[string]any{"run_id": run.RunID, "err": run.ErrorMsg})
		// parse_error streak の集計 + 閾値到達時に Error alert。
		if run.Status == port.AdvisorRunStatusParseError {
			u.parseErrorStreak++
			if u.parseErrorStreak == parseErrorAlertThreshold {
				u.notify(ctx, port.LevelError, "advisor_parse_error_streak",
					fmt.Sprintf("Claude advisor parse_error が %d 連続", u.parseErrorStreak),
					map[string]any{"run_id": run.RunID, "streak": u.parseErrorStreak})
			}
		} else {
			// timeout / cli_error は parse_error 限定 streak をリセット。
			u.parseErrorStreak = 0
		}
		return nil, nil
	}
	// 成功で streak リセット (回復シグナル)。
	u.parseErrorStreak = 0

	// Early symbol-mismatch guard (multi-symbol safety). When this bundle
	// is pinned to a symbol and the YAML parses cleanly to a different
	// symbol, short-circuit before invoking GetAccountState + Promoter —
	// saving the validate cascade and a rejected_row write. Promoter.
	// ExpectedSymbol stays as the source-of-truth reject point covering
	// races / future callers that bypass this layer. Malformed YAML falls
	// through to Promoter so its parse-failure audit path still records.
	if u.Symbol != "" && len(run.ParsedYAML) > 0 {
		if cfg, perr := config.ParseStrategyConfig(run.ParsedYAML); perr == nil && cfg.Symbol != u.Symbol {
			u.Logger.Error("advisor_symbol_mismatch",
				"expected", u.Symbol, "got", cfg.Symbol, "run_id", run.RunID)
			u.notify(ctx, port.LevelWarn, "advisor_symbol_mismatch",
				"advisor returned wrong symbol",
				map[string]any{"expected": u.Symbol, "got": cfg.Symbol, "run_id": run.RunID})
			return nil, fmt.Errorf("%w: expected %s, got %s", ErrAdvisorSymbolMismatch, u.Symbol, cfg.Symbol)
		}
	}

	// dead-market guard: if the advisor returned an enabled
	// trade whose take_profit is unreachable for the recent 1h range, rewrite
	// it to no_trade BEFORE promotion. We mutate the YAML bytes (not just the
	// parsed struct) so every downstream materialization — raw_yaml audit row,
	// live holder.Set(res.Parsed), and startup re-parse via LoadActiveFromDB —
	// consistently sees the no_trade decision. Downgrade, not reject: rejecting
	// would freeze the stale active config past its valid_until. Why: in a dead
	// market a TP larger than the recent range is never reached, so the config only
	// churns small losing trades.
	if len(run.ParsedYAML) > 0 {
		if cfg, perr := config.ParseStrategyConfig(run.ParsedYAML); perr == nil {
			if reason, downgrade := config.UnreachableTPReason(cfg, summary.Summary1h.RangePips); downgrade {
				cfg.DowngradeToNoTrade(reason)
				if nb, merr := yaml.Marshal(cfg); merr == nil {
					run.ParsedYAML = nb
					u.Logger.Warn("dead_market_downgrade_no_trade",
						"config_id", cfg.ConfigID, "symbol", cfg.Symbol,
						"range_1h_pips", summary.Summary1h.RangePips, "reason", reason)
					u.notify(ctx, port.LevelInfo, "dead_market_no_trade",
						"TP unreachable for 1h range — downgraded to no_trade",
						map[string]any{"config_id": cfg.ConfigID, "reason": reason})
				} else {
					// Marshal should never fail for a struct we just parsed;
					// if it somehow does, fall through with the original YAML
					// (the strict Validator still backstops downstream).
					u.Logger.Error("dead_market_downgrade_marshal_failed",
						"config_id", cfg.ConfigID, "err", merr)
				}
			}
		}
	}

	state, stateErr := u.GetAccountState()
	if stateErr != nil {
		// Refuse to promote when we can't read live state. The
		// risk-validation pass would otherwise see 0 open_positions / 0
		// daily_loss and let a config slip past caps it should hit.
		u.Logger.Error("get_account_state_failed_rejecting_promote", "err", stateErr)
		u.notify(ctx, port.LevelWarn, "promote_state_read_failed",
			"refusing to promote: account state read failed",
			map[string]any{"err": stateErr.Error()})
		return nil, stateErr
	}
	res, err := u.Promoter.PromoteFromYAML(ctx, run.ParsedYAML, state, string(source))
	if err != nil {
		u.Logger.Error("promotion_error", "err", err)
		return nil, err
	}
	if res.Promoted {
		u.Logger.Info("config_promoted", "config_id", res.ConfigID, "strategy", string(res.Parsed.Strategy.Name))
		u.notify(ctx, port.LevelInfo, "config_promoted",
			"new active strategy_config "+res.ConfigID,
			map[string]any{"strategy": string(res.Parsed.Strategy.Name)})
	} else {
		u.Logger.Warn("config_rejected", "config_id", res.ConfigID, "reason", res.RejectReason)
		u.notify(ctx, port.LevelWarn, "config_rejected",
			"strategy_config rejected: "+res.RejectReason,
			map[string]any{"config_id": res.ConfigID})
	}
	return res, nil
}

func (u *AdvisorCycle) notify(ctx context.Context, lv port.Level, title, body string, meta map[string]any) {
	if u.Notifier == nil {
		return
	}
	_ = u.Notifier.Notify(ctx, port.Event{Level: lv, Title: title, Body: body, Meta: meta})
}
