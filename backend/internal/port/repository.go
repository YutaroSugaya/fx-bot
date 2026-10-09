// Package port defines the interfaces that connect the usecase layer to its
// external dependencies (drivers/adapters). Implementations live under
// internal/adapter/.
package port

import (
	"context"
	"errors"
	"time"
)

// ErrDuplicateConfigID is returned by StrategyConfigRepository.Insert when the
// config_id already exists (UNIQUE violation). Usecase layer detects this and
// retries with a suffixed config_id so manual re-triggers within the same hour
// don't fail (Claude tends to align config_id to the next valid_from).
var ErrDuplicateConfigID = errors.New("duplicate config_id")

// ErrTradePositionIDMismatch is returned by PositionCloser.CloseAndRecord
// when the function arg `positionID` does not match trade.PositionID.
// Defensive guard: the FK on trades.position_id resolves to ANY real
// position, so a mismatch would otherwise close one row and write the
// trade against another silently.
var ErrTradePositionIDMismatch = errors.New("position_id mismatch: trade.PositionID must equal closer arg positionID")

// ErrBrokerPositionNotFound is returned (wrapped) by a Broker when the broker
// reports the position no longer exists (GMO ERR-254 "Not found position").
// During a close this is BENIGN, not a naked position: the settle leg (TP/SL
// OCO) filled first and already closed the position, racing our MaxHold/manual
// close. The close saga detects this via errors.Is and skips emergency_stop —
// reconcile records the broker-side fill instead of halting the bot.
var ErrBrokerPositionNotFound = errors.New("broker position not found")

// ---------------------------------------------------------------------------
// strategy_configs + 3 junction tables
// ---------------------------------------------------------------------------

// StrategyConfigStatus is the lifecycle status of a generated config row.
type StrategyConfigStatus string

const (
	StrategyConfigStatusGenerated StrategyConfigStatus = "generated"
	StrategyConfigStatusRejected  StrategyConfigStatus = "rejected"
	StrategyConfigStatusActive    StrategyConfigStatus = "active"
	StrategyConfigStatusExpired   StrategyConfigStatus = "expired"
)

// StrategyConfigRecord is the row layout of the strategy_configs main table
// plus the optional rejection / parse-failure metadata that the repository
// writes to junction tables in the SAME Tx as the main row.
//
// RejectReason: non-empty iff Status == rejected. Writes a
// strategy_config_rejections row in the same Tx.
//
// ParseFailureRaw: non-empty iff the row originated from an advisor parse
// failure. Writes a strategy_config_parse_failures row in the same Tx.
//
// activated_at is handled by MarkActive / PromoteActive — they write a
// strategy_config_activations row when promoting to active.
type StrategyConfigRecord struct {
	ConfigID               string
	Source                 string // "auto" | "manual" | "event" | "fallback"
	Mode                   string // "paper_config" | "live_config" | "disabled"
	Symbol                 string
	Enabled                bool
	MarketRegimeType       string // empty when parse failed (audit via ParseFailureRaw)
	MarketRegimeConfidence float64
	StrategyName           string // empty when parse failed (audit via ParseFailureRaw)
	ValidFrom              time.Time
	ValidUntil             time.Time
	RawYAML                string
	Status                 StrategyConfigStatus
	RejectReason           string // optional — Status==rejected only
	ParseFailureRaw        string // optional — set on parse-failure inserts
}

type StrategyConfigRepository interface {
	Insert(ctx context.Context, rec StrategyConfigRecord) error
	MarkExpired(ctx context.Context, configID string) error
	// MarkActive flips status to 'active' AND writes a
	// strategy_config_activations row (activated_at) in one Tx.
	MarkActive(ctx context.Context, configID string, at time.Time) error

	// GetActive returns the single active config for the given (symbol,
	// mode) tuple, or (nil, nil) when none exists.
	GetActive(ctx context.Context, symbol, mode string) (*StrategyConfigRecord, error)

	// ListRecent returns the most-recent N rows ordered by created_at desc.
	// Includes rejected/expired/active alike — caller filters via Status.
	// ActivatedAt is populated by JOINing strategy_config_activations.
	ListRecent(ctx context.Context, limit int) ([]StrategyConfigRecordWithMeta, error)
}

// StrategyConfigRecordWithMeta extends StrategyConfigRecord with timestamps
// not returned by GetActive (created_at, activated_at).
type StrategyConfigRecordWithMeta struct {
	StrategyConfigRecord
	CreatedAt   time.Time
	ActivatedAt *time.Time
}

// ConfigValidationEvent is one row in config_validation_events.
type ConfigValidationEvent struct {
	ConfigID       string
	ValidationType string // "schema" | "hard_limit" | "semantic" | "risk"
	Status         string // "pass" | "fail"
	Message        string
}

type ConfigValidationEventRepository interface {
	Insert(ctx context.Context, ev ConfigValidationEvent) error
	// Dashboard 用: 直近 24h の reject 件数。
	CountFailSince(ctx context.Context, since time.Time) (int, error)
}

// ---------------------------------------------------------------------------
// market_summaries / ai_advisor_runs + 2 junction tables
// ---------------------------------------------------------------------------

type MarketSummaryRecord struct {
	Symbol        string
	SummaryWindow string // "1h" | "6h" | "24h"
	RawJSON       []byte
}

type MarketSummaryRepository interface {
	Insert(ctx context.Context, rec MarketSummaryRecord) error
}

type AdvisorRunStatus string

const (
	AdvisorRunStatusSuccess    AdvisorRunStatus = "success"
	AdvisorRunStatusTimeout    AdvisorRunStatus = "timeout"
	AdvisorRunStatusParseError AdvisorRunStatus = "parse_error"
	AdvisorRunStatusCLIError   AdvisorRunStatus = "cli_error"
)

// AdvisorRunSource は advisor cycle がどの発火源で走ったかを示す。
type AdvisorRunSource string

const (
	AdvisorRunSourceAuto   AdvisorRunSource = "auto"   // 通常の scheduler fire
	AdvisorRunSourceManual AdvisorRunSource = "manual" // dashboard ボタン
	AdvisorRunSourceEvent  AdvisorRunSource = "event"  // Claude が短間隔を要求した結果の fire
)

// AdvisorRunRecord holds the main ai_advisor_runs row. Large IO payloads
// (input_json, output_yaml) and error messages now live in dedicated
// junction tables — Insert writes the appropriate junction row in the
// same Tx as the main row.
type AdvisorRunRecord struct {
	RunID        string
	Provider     string
	Mode         string
	PromptPath   string
	InputJSON    []byte // status=success → goes to advisor_run_io
	OutputYAML   []byte // status=success → goes to advisor_run_io
	Status       AdvisorRunStatus
	ErrorMessage string // status in (timeout/parse_error/cli_error) → goes to advisor_run_errors
	Source       AdvisorRunSource
	StartedAt    time.Time
	FinishedAt   time.Time
}

// AdvisorRunRepository writes the ai_advisor_runs row + the matching
// junction row (advisor_run_io for success, advisor_run_errors for the
// failure statuses) atomically in one Tx.
type AdvisorRunRepository interface {
	Insert(ctx context.Context, rec AdvisorRunRecord) error
	// Dashboard 用: 直近の成功 advisor run の duration_ms。
	GetLastDurationMs(ctx context.Context) (int, error)
}

// ---------------------------------------------------------------------------
// positions (main) + 3 junction tables + position_state_events
// ---------------------------------------------------------------------------

type PositionStatus string

const (
	PositionStatusOpen    PositionStatus = "OPEN"
	PositionStatusClosing PositionStatus = "CLOSING"
	PositionStatusClosed  PositionStatus = "CLOSED"
	PositionStatusUnknown PositionStatus = "UNKNOWN"
)

// PositionSource discriminates how a position came to be in the DB.
//   - PositionSourceBot:            bot-driven entry (strategy signal or manual
//     entry from the dashboard). Bot owns its
//     TP/SL/MaxHold lifecycle.
//   - PositionSourceExternalBroker: position was discovered at the broker side
//     (e.g. user opened it directly in the GMO
//     app). Bot is display-only — does NOT
//     manage TP/SL/MaxHold, does NOT count it
//     toward `max_open_positions`.
//   - PositionSourcePaperRecovered: paper-mode broker had a position the DB
//     didn't know about at startup (DB crash,
//     tests). Bot manages it normally with the
//     active config's parameters.
//
// The default zero value is "" which the adapters/usecases treat as Bot for
// backwards-compatibility with the many call sites that still construct
// PositionRecord literals without a Source value.
type PositionSource string

const (
	PositionSourceBot            PositionSource = "bot"
	PositionSourceExternalBroker PositionSource = "external_broker"
	PositionSourcePaperRecovered PositionSource = "paper_recovered"
)

// IsExternal reports whether the position originated outside the bot and must
// NOT be managed by the bot's TP/SL/MaxHold loop nor counted toward entry caps.
// Treats the empty default ("") as Bot.
func (s PositionSource) IsExternal() bool {
	return s == PositionSourceExternalBroker
}

// PositionRecord is the row layout of the positions main table.
// Live-only fields (broker_position_id, tp_order_id, sl_order_id) are
// held in PositionLive — see PositionLiveRepository.
// State-dependent timestamps (closing_at, closed_at) are derived from
// position_state_events — see PositionStateEventRepository.
type PositionRecord struct {
	ID             int64
	Symbol         string
	Side           string // "BUY" | "SELL"
	Quantity       int
	EntryPrice     float64
	TakeProfitPips float64
	StopLossPips   float64
	MaxHoldMinutes int
	// ExtensionMaxMinutes / ExtensionUnrealizedPipsThreshold are snapshot
	// from the active config at entry time (positions freeze their exit
	// parameters at entry; later config switches never affect open
	// positions). Together with MaxHoldMinutes
	// they form a soft/hard deadline: the bot may wait past the soft
	// deadline (OpenedAt + MaxHoldMinutes) up to the hard deadline
	// (+ExtensionMaxMinutes) IF |unrealized_pips| ≤ threshold. 0 = disabled.
	ExtensionMaxMinutes              int
	ExtensionUnrealizedPipsThreshold float64
	// EarlyExitWindowMinutes / EarlyExitTargetPips form a "least bad"
	// early close policy: in the final EarlyExitWindowMinutes before the
	// soft MaxHold deadline, evaluateExit closes the position as soon as
	// unrealized_pips >= EarlyExitTargetPips. This caps the worst-case
	// downside that a fixed deadline force-close can lock in. 0 window =
	// disabled (back-compat). Snapshot from active config at entry time
	// (same freeze-at-entry rule as above).
	EarlyExitWindowMinutes int
	EarlyExitTargetPips    float64
	// RatchetArmPips / RatchetGivebackPips は trailing take-profit の snapshot
	// (migration 0003)。peak が ArmPips に達して以降、peak から
	// GivebackPips だけ戻ったら MARKET close する。0 / 0 = OFF。Insert 時に
	// active config の Exit セクションから凍結保存し、以後 advisor が config
	// を切り替えても既存 position の動作は変えない (config_snapshot rule)。
	RatchetArmPips      float64
	RatchetGivebackPips float64
	// PeakUnrealizedPips / RatchetArmed は ratchet の runtime state。OnTick で
	// UpdateRatchetState が更新する。snapshot ではない (config 由来でない)。
	// 新規 position は DB DEFAULT で 0.0 / false から始まる。
	PeakUnrealizedPips float64
	RatchetArmed       bool
	// TroughUnrealizedPips / LossRatchetArmed は trailing STOP (損切り側 ratchet)
	// の runtime state (migration 0009)。利確 ratchet の鏡像で、
	// 含み損の trough (最悪値) を OnTick で追い、trough が -RatchetArmPips に
	// 達して以降 (LossRatchetArmed=true)、trough から RatchetGivebackPips だけ
	// 戻ったら浅い傷で損切り close する ("ratchet_stoploss")。arm/giveback は
	// 利確側と同じ RatchetArmPips / RatchetGivebackPips を共用する (同 pips の
	// mirror)。RatchetArmPips=0 なら両側 OFF。新規 position は DB DEFAULT で
	// 0.0 / false から始まる。
	TroughUnrealizedPips float64
	LossRatchetArmed     bool
	// Entry-time cost capture (migration 0008)。
	// nil = 未捕捉 (0008 以前に建った行 / ticker 不在等)。0 と NULL を区別するため
	// pointer — EntryFeeJPY の 0 は「broker が 0 と報告 (手数料無料期間)」を意味する。
	//   EntryFeeJPY:       entry fill の broker 実報告手数料。close 時に trades.fee_jpy へ合成。
	//   EntrySpreadPips:   発注直前 ticker の実測スプレッド (backtest スプレッドモデルの較正源)。
	//   EntrySlippagePips: 符号付き adverse slippage = BUY: (fill−ask)/pip, SELL: (bid−fill)/pip。
	EntryFeeJPY       *float64
	EntrySpreadPips   *float64
	EntrySlippagePips *float64
	StrategyConfigID  string
	Status            PositionStatus
	OpenedAt          time.Time
	// Source is populated by repositories when listing positions; it is
	// derived from the recovered_positions junction (reason field). Default
	// "" is treated as Bot. It is NOT a column on positions — it is a
	// computed discriminator. See PositionSource for values.
	Source PositionSource
}

// PositionLive holds broker-side metadata that used to be nullable
// columns on positions. Live mode populates all three fields (broker
// position id + TP/SL leg order ids at GMO). Paper mode populates
// BrokerPositionID only (TP/SL order ids are empty strings) — the
// PaperBroker still needs the broker-assigned id to find the position
// at close time. Naming is historical: every persisted position with a
// broker id has a positions_live row.
type PositionLive struct {
	PositionID       int64
	BrokerPositionID string
	TPOrderID        string
	SLOrderID        string
}

// PositionInsertInput bundles everything a caller wants to persist
// atomically with a position Insert: the main row, optionally the live
// (broker-side) metadata, optionally a manual_positions marker, optionally
// a recovered_positions marker, and always the initial OPEN state event.
//
// Live carries the broker-assigned id (and TP/SL leg ids when in live mode).
// It can coexist with Manual or Recovered — the origin marker (Manual /
// Recovered) and the broker metadata (Live) are orthogonal. Manual and
// Recovered are mutually exclusive: a position is either manually
// initiated or recovered, never both. The repository enforces this.
type PositionInsertInput struct {
	Position  PositionRecord
	Live      *PositionLive
	Manual    bool
	Recovered *RecoveredPositionMeta
}

// RecoveredPositionMeta is the reason+timestamp pair recorded in the
// recovered_positions junction when reconcile inserts a Paper-mode
// position from broker state OR adopts an externally-opened live position.
type RecoveredPositionMeta struct {
	Reason      string
	RecoveredAt time.Time
}

// Canonical recovery_reason values written to recovered_positions. The list
// is small on purpose — ListOpenOrClosing maps these to PositionSource and
// the rest of the codebase decides behaviour from PositionSource, not from
// the raw reason string.
const (
	// RecoveryReasonBrokerNakedAtStartup: paper mode startup found a broker
	// position the DB had lost track of (DB crash, tests). Bot manages it.
	RecoveryReasonBrokerNakedAtStartup = "broker_naked_at_startup"
	// RecoveryReasonExternalBrokerAdoption: an externally-opened position
	// (user trading directly in the GMO app/web) was discovered by
	// reconcile. Bot does NOT manage it (display-only).
	RecoveryReasonExternalBrokerAdoption = "external_broker_adoption"
)

// RecoveryReasonToSource maps a recovered_positions.recovery_reason value to
// the PositionSource that the rest of the codebase reads. Unknown reasons
// default to PaperRecovered to preserve historical behaviour (= bot manages).
func RecoveryReasonToSource(reason string) PositionSource {
	switch reason {
	case RecoveryReasonExternalBrokerAdoption:
		return PositionSourceExternalBroker
	case RecoveryReasonBrokerNakedAtStartup:
		return PositionSourcePaperRecovered
	case "":
		return PositionSourceBot
	default:
		return PositionSourcePaperRecovered
	}
}

type PositionRepository interface {
	// Insert writes the positions main row, the requested junction row
	// (live/manual/recovered), AND a position_state_events('OPEN') row
	// in one Tx. The returned ID is positions.id.
	Insert(ctx context.Context, in PositionInsertInput) (int64, error)

	// ListOpenOrClosing returns positions in OPEN or CLOSING state for
	// the given symbol. Reconcile uses this — a position stuck in
	// CLOSING after a saga crash is still visible. Pass "" for all symbols.
	ListOpenOrClosing(ctx context.Context, symbol string) ([]PositionRecord, error)

	// CountOpenAllSymbols returns the count of positions in OPEN or
	// CLOSING state across every symbol, including external (manually
	// opened in the GMO app) positions. Used by the account-wide
	// AccountOpenPositions cap (multi-symbol risk gate). External positions
	// are counted because the account cap is a margin-protection gate, and
	// margin is consumed by external positions too.
	CountOpenAllSymbols(ctx context.Context) (int, error)

	// ClaimForClose atomically transitions a position from OPEN to CLOSING
	// AND inserts a position_state_events('CLOSING') row in one Tx.
	// Returns ok=true on success, ok=false (no err) when the row was not
	// in OPEN — another saga is in flight or already finalised.
	ClaimForClose(ctx context.Context, id int64, claimedAt time.Time) (ok bool, err error)

	// GetLive returns the positions_live row for the given position, or
	// (nil, nil) when none exists (= Paper position).
	GetLive(ctx context.Context, positionID int64) (*PositionLive, error)

	// IsManual reports whether the position has a manual_positions row.
	IsManual(ctx context.Context, positionID int64) (bool, error)

	// MarkClosed flips status → CLOSED and appends a CLOSED state event
	// WITHOUT writing a trade row. Lossy on purpose — only paper-mode
	// startup recovery (the broker says the position is gone, we have
	// no fill price to record) is allowed to call this. Live mode MUST
	// go through ClaimForClose + PositionCloser.CloseAndRecord so the
	// trade ledger stays complete.
	MarkClosed(ctx context.Context, id int64, closedAt time.Time) error

	// UpdateRatchetState persists the OnTick-tracked ratchet runtime state
	// for the given position: 利確側 (peak_unrealized_pips / ratchet_armed) と
	// 損切り側 (trough_unrealized_pips / loss_ratchet_armed) を 1 write で更新する。
	// status='OPEN' の row のみ更新 — CLOSING / CLOSED の position 宛は
	// silently no-op (close saga が既に値を確定させているため)。
	// 呼ぶ側は peak を monotonic increasing・trough を monotonic decreasing で
	// 保つこと (変化が無ければ呼ばない)。
	UpdateRatchetState(ctx context.Context, id int64, peakUnrealizedPips float64, armed bool, troughUnrealizedPips float64, lossArmed bool) error

	// ExtendMaxHold は max_hold_minutes に addMinutes を加算し、新しい合計と
	// opened_at を返す。UI/API の「延長ボタン」(POST /api/positions/extend)
	// から呼ばれる。status='OPEN' の row のみ対象で、CLOSING / CLOSED / 未知
	// id には (nil, nil) を返す (= 延長対象なし)。max_hold_minutes は per-
	// position の凍結値 (建玉時に config から凍結保存) で config 変更
	// では動かせないため、開いた position の保有上限を伸ばす唯一の経路。bot の
	// OnTick は毎 tick で max_hold_minutes を読み直すので、UPDATE 後の次 tick
	// から新しい soft deadline が効く (close/再 build 不要)。
	ExtendMaxHold(ctx context.Context, id int64, addMinutes int) (*MaxHoldExtended, error)
}

// MaxHoldExtended は PositionRepository.ExtendMaxHold の戻り値。新しい
// max_hold_minutes (加算後の合計) と、deadline を再計算するための opened_at
// を持つ。deadline = OpenedAt + MaxHoldMinutes 分。
type MaxHoldExtended struct {
	MaxHoldMinutes int
	OpenedAt       time.Time
}

// PositionCloser atomically marks a position CLOSED and inserts the
// corresponding trade row (+ optional trade_signals junction for auto
// trades) in one DB transaction.
//
// Returns ok=false (without error) when the position was not in CLOSING
// state at UPDATE time — caller must NOT assume the broker close it just
// performed was the original one. Caller is expected to trip
// emergency_stop and let the reconciler clean up.
//
// Contract: the saga must transition OPEN → CLOSING via ClaimForClose
// BEFORE any broker call. CloseAndRecord only flips CLOSING → CLOSED.
type PositionCloser interface {
	CloseAndRecord(ctx context.Context, positionID int64, closedAt time.Time, trade TradeRecord) (ok bool, err error)
}

// StrategyConfigPromoter atomically expires the previous active config row
// (if any) and inserts the new active row in a single DB transaction.
//
// On 23505 unique violation during INSERT, the Tx is rolled back (so the
// prior active stays active) and ErrDuplicateConfigID is returned —
// caller retries with a suffixed config_id.
type StrategyConfigPromoter interface {
	PromoteActive(ctx context.Context, prevConfigID string, newRec StrategyConfigRecord) error
}

// TradeRecord is the row layout of the trades main table. SignalID is
// passed in for auto trades; the repository writes it to the
// trade_signals junction. Manual close trades leave SignalID="" and the
// junction row is skipped.
type TradeRecord struct {
	PositionID       int64
	SignalID         string
	StrategyConfigID string
	Symbol           string
	Side             string
	Quantity         int
	EntryPrice       float64
	ExitPrice        float64
	ProfitLossPips   float64
	ProfitLossJPY    float64 // GROSS のまま維持 (既存集計との互換)。net = gross − FeeJPY + SwapJPY は導出側で計算
	CloseReason      string  // "take_profit" | "stop_loss" | "max_hold" | "early_exit" | "ratchet_takeprofit" | "ratchet_stoploss" | "manual" | "reconcile_cold_close" | "broker_close" (TP/SL 非一致の broker-side close を reconcile が実 fill から復元; migration 0006)。ratchet_stoploss = 損切り側 trailing (migration 0009/0010)。値を増やすときは必ず trades.close_reason の CHECK 制約 (migrations) も同時に拡張すること
	// per-trade 実コスト (migration 0007)。
	//   FeeJPY:       往復手数料 = entry leg (positions.entry_fee_jpy) + close leg (close fill の実報告 or 0.002% 推定)
	//   SwapJPY:      close fill の settledSwap (跨ぎスワップ。符号付き)
	//   FeeEstimated: true = いずれかの leg を 0.002% 推定で補完 / false = 両 leg とも broker 実報告値
	FeeJPY       float64
	SwapJPY      float64
	FeeEstimated bool
	OpenedAt     time.Time
	ClosedAt     time.Time
	// Origin classifies who opened the position behind this trade. It is
	// DERIVED at read time from the manual_positions / recovered_positions
	// junctions (NOT a stored trades column), so existing rows classify
	// retroactively. Empty string from older code paths is treated as
	// TradeOriginBot by consumers. See TradeOrigin constants.
	Origin string
}

// TradeOrigin classifies the entry behind a TradeRecord so the bot's edge
// metrics and the trade-history label can be kept free of the operator's own
// discretionary trades. The fact lives in junction tables (the trades row's
// strategy_config_id only borrows the active config as an FK target), so the
// classification is a read-time JOIN, not stored state.
const (
	// TradeOriginBot: bot strategy signal (default; also the empty-string fallback).
	TradeOriginBot = "bot"
	// TradeOriginExternal: opened directly in the GMO app, adopted by reconcile
	// (recovered_positions.recovery_reason = "external_broker_adoption").
	TradeOriginExternal = "external"
	// TradeOriginManual: opened via the bot's manual_trade command (manual_positions junction).
	TradeOriginManual = "manual"
)

type TradeRepository interface {
	Insert(ctx context.Context, rec TradeRecord) error

	// ListSince and CountSince filter by opened_at — used by UI listings.
	ListSince(ctx context.Context, since time.Time, limit int) ([]TradeRecord, error)
	SumLossJPYSince(ctx context.Context, since time.Time) (int, error)
	CountSince(ctx context.Context, since time.Time) (int, error)

	// closed_at-based account-wide aggregates. Used for the
	// account-wide AccountDailyLossJPY snapshot field in multi-symbol
	// setups; single-symbol code retains them as the per-symbol path.
	ListClosedSince(ctx context.Context, since time.Time, limit int) ([]TradeRecord, error)
	SumClosedLossJPYSince(ctx context.Context, since time.Time) (int, error)
	CountClosedSince(ctx context.Context, since time.Time) (int, error)

	// Per-symbol closed_at aggregates. Risk gate per-symbol caps
	// (MaxTradesInThisWindow / MaxLossInThisWindowJPY / MaxDailyLossJPY /
	// MaxConsecutiveLosses) read these so a sibling symbol's trade does
	// not pollute this symbol's gate.
	ListClosedBySymbolSince(ctx context.Context, symbol string, since time.Time, limit int) ([]TradeRecord, error)
	SumClosedLossJPYBySymbolSince(ctx context.Context, symbol string, since time.Time) (int, error)
	CountClosedBySymbolSince(ctx context.Context, symbol string, since time.Time) (int, error)

	// Dashboard 用: symbol-scoped 符号付き PnL と早期 exit 発火数。
	SumPnLJPYClosedSinceBySymbol(ctx context.Context, symbol string, since time.Time) (float64, error)
	CountEarlyExitTradesSinceBySymbol(ctx context.Context, symbol string, since time.Time) (int, error)
}

type SignalRejection struct {
	StrategyConfigID string
	Reason           string
	Detail           []byte
}

type SignalRejectionRepository interface {
	Insert(ctx context.Context, rec SignalRejection) error
}

// CandleRecord is one OHLCV bar persisted to the candles table.
// Timeframe is "1m" | "5m" | "15m" | "1h" — matches market.Aggregator keys.
type CandleRecord struct {
	ID        int64
	Symbol    string
	Timeframe string
	OpenedAt  time.Time
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MarketSummaryArtifactStore persists/reads the latest MarketSummary
// snapshot as opaque raw bytes — typically JSON, but the port doesn't
// commit to that. The port keeps file I/O for this artifact out of the
// usecase layer ("usecase doesn't touch file I/O",
// docs/architecture/layers/usecase.md); the adapter/artifact/ adapter
// implements it.
//
// Read returns (nil, nil) when the artifact does not yet exist
// (= startup-before-first-publish). Permission / IO errors propagate as
// non-nil err.
type MarketSummaryArtifactStore interface {
	WriteLatestSummary(ctx context.Context, raw []byte) error
	ReadLatestSummary(ctx context.Context) ([]byte, error)
}

// StrategyConfigArtifactStore persists the active strategy config YAML as a
// human-readable artifact (consumed by operators / external tooling). DB is
// the runtime SoT — startup loads via Promoter.LoadActiveFromDB — so this
// is best-effort: write failures after a successful DB promote are logged
// but never roll the promote back.
//
// PromoteActive performs the staging→active swap atomically (tempfile +
// rename under the hood) and cleans up the next-staging artifact if any.
//
// ReadActive returns the current on-disk active YAML, or (nil, nil) when
// no artifact exists. Used by legacy `Promoter.LoadActive` fallback (= CLI
// tools that don't have DB access). Bot runtime uses LoadActiveFromDB and
// does not call this.
type StrategyConfigArtifactStore interface {
	PromoteActive(ctx context.Context, raw []byte) error
	ReadActive(ctx context.Context) ([]byte, error)
}

// CandleRepository persists OHLCV bars so the bot can:
//   - survive restarts without re-fetching from GMO (rate-limit friendly)
//   - replay historical bars for the backtest engine
//   - audit which bars the advisor actually saw
//
// Upsert key: (Symbol, Timeframe, OpenedAt).
type CandleRepository interface {
	Upsert(ctx context.Context, rec CandleRecord) error
	UpsertBatch(ctx context.Context, recs []CandleRecord) error
	// ListSince returns bars whose OpenedAt >= since, ordered most-recent-first.
	// limit <= 0 means no limit.
	ListSince(ctx context.Context, symbol, timeframe string, since time.Time, limit int) ([]CandleRecord, error)
}

// ---------------------------------------------------------------------------
// Repositories aggregate — passed around by usecase wiring.
// ---------------------------------------------------------------------------

type Repositories struct {
	StrategyConfigs  StrategyConfigRepository
	ValidationEvents ConfigValidationEventRepository
	MarketSummaries  MarketSummaryRepository
	AdvisorRuns      AdvisorRunRepository
	Positions        PositionRepository
	Trades           TradeRepository
	SignalRejections SignalRejectionRepository
	Candles          CandleRepository
	Closer           PositionCloser         // 決済 (CLOSING→CLOSED + trade + trade_signals) を 1 Tx で行う
	ConfigPromoter   StrategyConfigPromoter // active 切替 (旧 expire + 新 insert + activation 行) を 1 Tx で行う
}
