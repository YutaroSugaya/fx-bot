package query

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// GetBotStatusInput は GetBotStatusQuery.Execute の引数。
type GetBotStatusInput struct{}

// EdgeMetricsView は per-symbol の「edge の質」サマリ (計測パネル)。直近の
// closed trade から command.DeriveEdgeMetrics で算出した値を JSON 契約 (snake_case)
// に写したもの。「勝率は高いが逆RR」を dashboard / 日次サマリで一目で見るため。
// N/A 表現: loss_count==0 のとき profit_factor / reward_risk は 0 (frontend が ∞/— 表示)。
type EdgeMetricsView struct {
	TradeCount   int     `json:"trade_count"`
	WinCount     int     `json:"win_count"`
	LossCount    int     `json:"loss_count"`
	WinRatePct   float64 `json:"win_rate_pct"`
	ProfitFactor float64 `json:"profit_factor"`
	AvgWinPips   float64 `json:"avg_win_pips"`
	AvgLossPips  float64 `json:"avg_loss_pips"`
	RewardRisk   float64 `json:"reward_risk"`
	// gross / fee / net 分離。
	// expectancy_jpy / net_pnl_jpy は NET (= gross − fee + swap)。
	// fee/swap 未記録の row では net == gross (後方互換)。
	ExpectancyJpy        float64 `json:"expectancy_jpy"`
	GrossPnlJpy          float64 `json:"gross_pnl_jpy"`
	FeeJpy               float64 `json:"fee_jpy"`
	SwapJpy              float64 `json:"swap_jpy"`
	NetPnlJpy            float64 `json:"net_pnl_jpy"`
	FeeEstimatedCount    int     `json:"fee_estimated_count"`
	MaxConsecutiveLosses int     `json:"max_consecutive_losses"`
	WindowDays           int     `json:"window_days"`
}

// SymbolStatus is the per-symbol breakdown carried in BotStatusView.Symbols.
// One entry per bot_config.symbols (stable order = config order). Fields
// mirror the legacy top-level BotStatusView fields per-symbol so a future
// frontend can iterate Symbols and ignore the top-level entirely.
type SymbolStatus struct {
	Symbol            string           `json:"symbol"`
	OpenPositions     int              `json:"open_positions"`
	ActiveConfigID    string           `json:"active_config_id,omitempty"`
	ValidUntil        *time.Time       `json:"valid_until,omitempty"`
	Strategy          string           `json:"strategy,omitempty"`
	Enabled           *bool            `json:"enabled,omitempty"`
	Pnl24hJpy         *float64         `json:"pnl_24h_jpy,omitempty"`
	EarlyExitCount24h *int             `json:"early_exit_count_24h,omitempty"`
	EdgeMetrics       *EdgeMetricsView `json:"edge_metrics,omitempty"`
}

// BotStatusView は GET /api/status の DTO。
type BotStatusView struct {
	Mode          string `json:"mode"`
	Uptime        string `json:"uptime"`
	EmergencyStop bool   `json:"emergency_stop"`

	// Multi-symbol breakdown (additive). Per-symbol metrics + active config;
	// iterate this instead of the legacy top-level fields below.
	Symbols              []SymbolStatus `json:"symbols"`
	AccountOpenPositions int            `json:"account_open_positions"`

	// Legacy single-symbol top-level fields. Reflect bot_config.symbols[0]
	// (the primary symbol) so existing dashboards keep parsing while the
	// frontend migrates to the Symbols array.
	Symbol         string     `json:"symbol"`
	OpenPositions  int        `json:"open_positions"`
	ActiveConfigID string     `json:"active_config_id,omitempty"`
	ValidUntil     *time.Time `json:"valid_until,omitempty"`
	Strategy       string     `json:"strategy,omitempty"`
	Enabled        *bool      `json:"enabled,omitempty"`
	TickerErrors   *int64     `json:"ticker_errors,omitempty"`
	Counters       any        `json:"counters,omitempty"`

	// Dashboard 拡張: 改善ループの観察コストを下げるための 4 指標。
	// 各 fn が nil の場合は omitempty で省略される。
	// Pnl24hJpy / EarlyExitCount24h は primary-symbol 値 (per-symbol は Symbols[] 内)。
	Pnl24hJpy             *float64 `json:"pnl_24h_jpy,omitempty"`
	RejectCount24h        *int     `json:"reject_count_24h,omitempty"`         // account-wide
	EarlyExitCount24h     *int     `json:"early_exit_count_24h,omitempty"`     // primary symbol
	LastAdvisorDurationMs *int     `json:"last_advisor_duration_ms,omitempty"` // account-wide

	// 計測パネル: primary symbol の edge 指標 (per-symbol は Symbols[] 内)。
	EdgeMetrics *EdgeMetricsView `json:"edge_metrics,omitempty"`
}

// EmergencyStopReader は flag ファイル存在で稼働停止状態を判定する関数型。
type EmergencyStopReader func() bool

// CountersSnapshotFn は observability カウンタの Snapshot を返す関数型。
type CountersSnapshotFn func() any

// TickerErrorsFn は累積 ticker エラー回数を返す関数型 (legacy)。
type TickerErrorsFn func() int64

// Dashboard 拡張: 4 指標を /api/status に乗せるための fn-typed 依存性。
// Query 層が repo を直接持たず、wiring 層 (api_wire.go) で repo を closure capture
// して渡す。fn が nil の場合は対応フィールドが BotStatusView から省略される。

// Pnl24hJpyFn は直近 24h の実現 PnL (JPY) を返す。
type Pnl24hJpyFn func(ctx context.Context, symbol string) (float64, error)

// RejectCount24hFn は直近 24h の config_validation_events status='fail' 件数を返す。
type RejectCount24hFn func(ctx context.Context) (int, error)

// EarlyExitCount24hFn は直近 24h で早期 exit (held < max_hold) 発火した trade 数を返す。
type EarlyExitCount24hFn func(ctx context.Context, symbol string) (int, error)

// LastAdvisorDurationMsFn は直近 advisor run の duration_ms を返す (= finished_at - started_at)。
type LastAdvisorDurationMsFn func(ctx context.Context) (int, error)

// EdgeMetricsFn は直近 closed trade の edge 指標 (PF/RR/期待値等) を返す (計測パネル)。
type EdgeMetricsFn func(ctx context.Context, symbol string) (EdgeMetricsView, error)

// GetBotStatusQuery is a read-only CQRS Query.
//
// GET /api/status を裏で支える。bot mode / 稼働時間 / OPEN ポジ数 / アクティ
// ブ config の要約 / observability counters を 1 つの DTO に集約する。
type GetBotStatusQuery struct {
	BotConfig *config.BotConfig
	Positions port.PositionRepository
	// GetActiveConfigs returns every symbol's active config (multi-symbol
	// source). When wired, the Symbols breakdown is populated AND the
	// legacy top-level active_config_* fields are sourced from the primary
	// symbol slot. nil → no active config information at all.
	GetActiveConfigs func() map[string]*config.StrategyConfig
	// GetActiveConfig is the legacy single-symbol shim. Used only when
	// GetActiveConfigs is nil (= callers not yet migrated).
	GetActiveConfig func() *config.StrategyConfig
	EmergencyActive EmergencyStopReader
	CountersFn      CountersSnapshotFn // nil 可
	TickerErrors    TickerErrorsFn     // nil 可
	StartedAt       time.Time
	Clock           func() time.Time // テスト用

	// Dashboard 拡張: 全て nil 可。nil の場合は対応フィールドが省略される。
	Pnl24hJpyFn             Pnl24hJpyFn
	RejectCount24hFn        RejectCount24hFn
	EarlyExitCount24hFn     EarlyExitCount24hFn
	LastAdvisorDurationMsFn LastAdvisorDurationMsFn
	EdgeMetricsFn           EdgeMetricsFn // nil 可 (計測パネル)
	// Logger は Dashboard 拡張 fn の err を warn で出すために使う。nil 可。
	Logger *slog.Logger
}

// Execute returns the current BotStatusView.
func (q *GetBotStatusQuery) Execute(ctx context.Context, _ GetBotStatusInput) (BotStatusView, error) {
	now := time.Now()
	if q.Clock != nil {
		now = q.Clock()
	}
	uptime := now.Sub(q.StartedAt).Round(time.Second).String()

	emergencyActive := false
	if q.EmergencyActive != nil {
		emergencyActive = q.EmergencyActive()
	}

	symbols := q.BotConfig.ResolveSymbols()
	if len(symbols) == 0 {
		// Defensive: BotConfig.validate() rejects this, but Execute can
		// be called from tests with a hand-built BotConfig. Fall back to
		// legacy Symbol to keep the response shape valid.
		symbols = []string{q.BotConfig.Symbol}
	}

	activeConfigs := q.resolveActiveConfigs()

	perSymbol := make([]SymbolStatus, 0, len(symbols))
	accountOpen := 0
	for _, sym := range symbols {
		open, err := q.Positions.ListOpenOrClosing(ctx, sym)
		if err != nil {
			return BotStatusView{}, fmt.Errorf("list open %s: %w", sym, err)
		}
		s := SymbolStatus{Symbol: sym, OpenPositions: len(open)}
		if cfg := activeConfigs[sym]; cfg != nil {
			s.ActiveConfigID = cfg.ConfigID
			vu := cfg.ValidUntil
			s.ValidUntil = &vu
			s.Strategy = string(cfg.Strategy.Name)
			en := cfg.Enabled
			s.Enabled = &en
		}
		if q.Pnl24hJpyFn != nil {
			if pnl, err := q.Pnl24hJpyFn(ctx, sym); err == nil {
				s.Pnl24hJpy = &pnl
			} else if q.Logger != nil {
				q.Logger.Warn("dashboard_pnl_24h_failed", "symbol", sym, "err", err)
			}
		}
		if q.EarlyExitCount24hFn != nil {
			if n, err := q.EarlyExitCount24hFn(ctx, sym); err == nil {
				s.EarlyExitCount24h = &n
			} else if q.Logger != nil {
				q.Logger.Warn("dashboard_early_exit_count_24h_failed", "symbol", sym, "err", err)
			}
		}
		if q.EdgeMetricsFn != nil {
			if em, err := q.EdgeMetricsFn(ctx, sym); err == nil {
				emCopy := em
				s.EdgeMetrics = &emCopy
			} else if q.Logger != nil {
				q.Logger.Warn("dashboard_edge_metrics_failed", "symbol", sym, "err", err)
			}
		}
		perSymbol = append(perSymbol, s)
		accountOpen += s.OpenPositions
	}

	// Legacy top-level fields = primary symbol's slice of the per-symbol
	// computation we just did. Saves a second repo round-trip and
	// guarantees the legacy fields match Symbols[0] exactly.
	primary := perSymbol[0]
	v := BotStatusView{
		Mode:                 string(q.BotConfig.Bot.Mode),
		Uptime:               uptime,
		EmergencyStop:        emergencyActive,
		Symbols:              perSymbol,
		AccountOpenPositions: accountOpen,
		Symbol:               primary.Symbol,
		OpenPositions:        primary.OpenPositions,
		ActiveConfigID:       primary.ActiveConfigID,
		ValidUntil:           primary.ValidUntil,
		Strategy:             primary.Strategy,
		Enabled:              primary.Enabled,
		Pnl24hJpy:            primary.Pnl24hJpy,
		EarlyExitCount24h:    primary.EarlyExitCount24h,
		EdgeMetrics:          primary.EdgeMetrics,
	}
	if q.TickerErrors != nil {
		n := q.TickerErrors()
		v.TickerErrors = &n
	}
	if q.CountersFn != nil {
		v.Counters = q.CountersFn()
	}
	if q.RejectCount24hFn != nil {
		if n, err := q.RejectCount24hFn(ctx); err == nil {
			v.RejectCount24h = &n
		} else if q.Logger != nil {
			q.Logger.Warn("dashboard_reject_count_24h_failed", "err", err)
		}
	}
	if q.LastAdvisorDurationMsFn != nil {
		if ms, err := q.LastAdvisorDurationMsFn(ctx); err == nil {
			v.LastAdvisorDurationMs = &ms
		} else if q.Logger != nil {
			q.Logger.Warn("dashboard_last_advisor_duration_ms_failed", "err", err)
		}
	}
	return v, nil
}

// resolveActiveConfigs returns the symbol→config map, preferring the
// multi-symbol GetActiveConfigs source and falling back to the legacy
// single-symbol GetActiveConfig shim when only that is wired.
func (q *GetBotStatusQuery) resolveActiveConfigs() map[string]*config.StrategyConfig {
	if q.GetActiveConfigs != nil {
		return q.GetActiveConfigs()
	}
	if q.GetActiveConfig != nil {
		if cfg := q.GetActiveConfig(); cfg != nil {
			return map[string]*config.StrategyConfig{q.BotConfig.Symbol: cfg}
		}
	}
	return map[string]*config.StrategyConfig{}
}
