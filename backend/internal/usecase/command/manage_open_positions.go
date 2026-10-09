package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/port"
)

// ManageOpenPositions handles TP/SL/MaxHold monitoring. The price-loop calls
// OnTick on every ticker update.
//
// Paper mode: the broker has no OCO. This usecase evaluates TP/SL/MaxHold
//
//	and exits trades by calling Broker.ClosePosition.
//
// Live mode:  TP/SL are delegated to GMO-side OCO settle orders.
//
//	Bot does NOT re-evaluate TP/SL (would race with GMO and risk
//	naked positions). Only MaxHold is monitored, and on trigger
//	the GMO settle legs are cancelled before MARKET close via
//	ExecuteCloseSaga.
type ManageOpenPositions struct {
	Broker            port.Broker
	Positions         port.PositionRepository
	Trades            port.TradeRepository
	Closer            port.PositionCloser // 決済 (MarkClosed + Trade Insert) を 1 Tx で行う
	Symbol            string
	PipSize           float64
	Mode              config.Mode
	EmergencyFlagPath string      // Live モードで cancel/close/DB 失敗時に発火
	CloseMutex        *sync.Mutex // priceLoop と API 経路の二重決済を防ぐ
	Logger            *slog.Logger
	Clock             func() time.Time
	// OnClosed は決済確定後に 1 回呼ばれるフック (CloseSagaInput.OnClosed へ貫通)。
	// nil = no-op。event_retrigger 用。
	OnClosed func(symbol, reason string)

	// Session flatten: 毎朝 05:45 JST の GMO スプレッド壁
	// (SL/ratchet が広スプレッドへ機械発火する帯) の前に、残っている OPEN 玉を通常
	// スプレッドで market close する。窓 = [StartMinuteJST, +30min)。Enabled=false
	// (zero-value) では絶対に発火しない。config: llm_decision.session_flatten_jst。
	SessionFlattenEnabled        bool
	SessionFlattenStartMinuteJST int // JST minutes-of-day (330 = 05:30)

	// Journal は shadow 計測 (時間ストップ反実仮想) の記録先。nil = no-op。
	// 建玉が 6h / 8h を跨いだ最初の tick で mid 基準含み pips を 1 回だけ記録する
	// (event=shadow_timestop)。発動はしない — 採否を後で判定するための計測のみ。
	Journal port.LLMDecisionJournal
	// shadowMarked は「この position id はどの mark を記録済みか」の bitmask
	// (bit0=6h / bit1=8h)。priceLoop 単一 goroutine からのみ触る。再起動で消える =
	// 稀に二重記録になるが、分析側で (position_id, stage) dedupe すれば無害。
	shadowMarked map[int64]uint8
}

func NewManageOpenPositions(br port.Broker, pos port.PositionRepository, tr port.TradeRepository, symbol string, mode config.Mode, logger *slog.Logger) *ManageOpenPositions {
	if logger == nil {
		logger = slog.Default()
	}
	return &ManageOpenPositions{
		Broker:    br,
		Positions: pos,
		Trades:    tr,
		Symbol:    symbol,
		PipSize:   market.PipSize(symbol),
		Mode:      mode,
		Logger:    logger,
		Clock:     time.Now,
	}
}

// OnTick scans open positions and closes any whose TP/SL is hit by the
// current ticker or whose MaxHold has elapsed.
func (u *ManageOpenPositions) OnTick(ctx context.Context, t market.Ticker) error {
	open, err := u.Positions.ListOpenOrClosing(ctx, u.Symbol)
	if err != nil {
		return fmt.Errorf("list open: %w", err)
	}
	now := u.Clock()
	for _, rec := range open {
		// Skip positions already being closed by another saga — the saga
		// will reach CLOSED on its own; re-evaluating here would race.
		if rec.Status != port.PositionStatusOpen {
			continue
		}
		// External positions (opened by the user directly in the GMO
		// app) are display-only. The bot must not silently close them on
		// a TP/SL/MaxHold match — the user owns those lifecycles on the
		// broker side. The Source field is populated from
		// recovered_positions.recovery_reason at list time.
		if rec.Source.IsExternal() {
			continue
		}
		// Ratchet TP runtime state を tick 値で更新 → DB に永続化。
		// evaluateExit はこの updated rec を見て fire 判定する。failure は
		// non-fatal: 次の tick で再試行 (close でも再起動でも整合する)。
		if updated, changed := u.advanceRatchetState(rec, t); changed {
			if err := u.Positions.UpdateRatchetState(ctx, rec.ID,
				updated.PeakUnrealizedPips, updated.RatchetArmed,
				updated.TroughUnrealizedPips, updated.LossRatchetArmed); err != nil {
				u.Logger.Warn("ratchet_state_update_failed",
					"id", rec.ID, "err", err.Error(),
					"peak", updated.PeakUnrealizedPips, "armed", updated.RatchetArmed,
					"trough", updated.TroughUnrealizedPips, "loss_armed", updated.LossRatchetArmed)
			}
			rec.PeakUnrealizedPips = updated.PeakUnrealizedPips
			rec.RatchetArmed = updated.RatchetArmed
			rec.TroughUnrealizedPips = updated.TroughUnrealizedPips
			rec.LossRatchetArmed = updated.LossRatchetArmed
		}
		// shadow 計測は exit 判定より先: 6h/8h 到達 tick で決済が同時に起きても
		// mark は必ず残る (計測が主目的・ポジション操作は一切しない)。
		u.recordShadowMarks(rec, t, now)
		reason := u.evaluateExit(rec, t, now)
		if reason == "" {
			continue
		}
		u.closeOne(ctx, rec, t, reason, now)
	}
	return nil
}

// shadow 時間ストップ計測の閾値と bitmask。
// 「長時間 hold の早期手仕舞い」は証拠不足のため発動させず、
// 6h/8h 時点の含み (mid 基準) を journal に記録だけして、採否はデータが溜まってから決める。
var shadowTimestopMarks = []struct {
	after time.Duration
	bit   uint8
	stage string
}{
	{6 * time.Hour, 1 << 0, "mark_6h"},
	{8 * time.Hour, 1 << 1, "mark_8h"},
}

// recordShadowMarks は建玉が 6h/8h を跨いだ最初の tick で、mid 基準含み pips を
// journal (event=shadow_timestop) に 1 回だけ記録する。priceLoop 単一 goroutine 前提。
// 再起動で bitmask は消える = 稀な二重記録は分析側 (position_id×stage) の dedupe で無害。
func (u *ManageOpenPositions) recordShadowMarks(rec port.PositionRecord, t market.Ticker, now time.Time) {
	if u.Journal == nil || rec.OpenedAt.IsZero() {
		return
	}
	elapsed := now.Sub(rec.OpenedAt)
	for _, m := range shadowTimestopMarks {
		if elapsed < m.after || u.shadowMarked[rec.ID]&m.bit != 0 {
			continue
		}
		if u.shadowMarked == nil {
			u.shadowMarked = make(map[int64]uint8)
		}
		u.shadowMarked[rec.ID] |= m.bit
		pnl, ok := unrealizedPipsMid(rec, t, u.PipSize)
		if !ok {
			continue
		}
		if err := u.Journal.Record(port.LLMDecisionLogEntry{
			Time: now, Symbol: u.Symbol, Event: "shadow_timestop", Stage: m.stage,
			Side:  rec.Side,
			Price: (t.Bid + t.Ask) / 2,
			Reason: fmt.Sprintf("pos %d %s: unrealized %+.1fp at %s (時間ストップ反実仮想・発動なし)",
				rec.ID, rec.Side, pnl, m.stage),
		}); err != nil {
			u.Logger.Warn("shadow_timestop_journal_failed", "id", rec.ID, "stage", m.stage, "err", err)
		}
	}
	// 掃除: closed 玉の bitmask が溜まり続けないよう、時々 open にない id を落とす。
	if len(u.shadowMarked) > 16 {
		u.pruneShadowMarks(rec.ID)
	}
}

// pruneShadowMarks は「直近 tick で見た id 以外」を全部落とす雑な GC。
// 単一ポジ運用 (max_open_positions=1) では map は実質 1 エントリで、この枝は
// ほぼ走らない — 万一の膨張へのバックストップ。
func (u *ManageOpenPositions) pruneShadowMarks(keep int64) {
	for id := range u.shadowMarked {
		if id != keep {
			delete(u.shadowMarked, id)
		}
	}
}

// advanceRatchetState は現 tick の unrealized から peak/armed を 1 段進める。
// 副作用なし: caller (OnTick) が DB と local rec の両方に書き戻す。peak は
// monotonic increasing で、現値 ≤ 前回 peak なら更新しない (DB call 不要)。
// armed は false→true の単方向で、一度 true になったら再 false にしない。
//
// changed=false のときは DB/local 更新は skip。ratchet OFF (ArmPips==0) や
// PipSize==0、side が BUY/SELL 以外なら必ず changed=false。
func (u *ManageOpenPositions) advanceRatchetState(rec port.PositionRecord, t market.Ticker) (port.PositionRecord, bool) {
	if rec.RatchetArmPips <= 0 || u.PipSize <= 0 {
		return rec, false
	}
	unrealized, ok := unrealizedPipsExit(rec, t, u.PipSize)
	if !ok {
		return rec, false
	}
	changed := false
	// 利確側: peak (最良益) を monotonic increasing で追う。
	if unrealized > rec.PeakUnrealizedPips {
		rec.PeakUnrealizedPips = unrealized
		changed = true
	}
	if !rec.RatchetArmed && rec.PeakUnrealizedPips >= rec.RatchetArmPips {
		rec.RatchetArmed = true
		changed = true
	}
	// 損切り側 (mirror): trough (最悪損) を monotonic decreasing で追う。
	// trough が -RatchetArmPips に達したら LossRatchetArmed を立てる。
	if unrealized < rec.TroughUnrealizedPips {
		rec.TroughUnrealizedPips = unrealized
		changed = true
	}
	if !rec.LossRatchetArmed && rec.TroughUnrealizedPips <= -rec.RatchetArmPips {
		rec.LossRatchetArmed = true
		changed = true
	}
	return rec, changed
}

// unrealizedPipsExit は「いま MARKET 決済したら何 pips の損益か」を返す。
// BUY は bid (売って閉じる)、SELL は ask (買い戻して閉じる) を使う。
// TP/SL の判定と同じ exit-side 価格基準。ok=false は side が BUY/SELL
// 以外 (= positions テーブルの CHECK 制約に反する状態) で防御的に止める。
func unrealizedPipsExit(rec port.PositionRecord, t market.Ticker, pipSize float64) (float64, bool) {
	switch rec.Side {
	case string(order.SideBuy):
		return (t.Bid - rec.EntryPrice) / pipSize, true
	case string(order.SideSell):
		return (rec.EntryPrice - t.Ask) / pipSize, true
	default:
		return 0, false
	}
}

// unrealizedPipsMid は MID 価格 ((bid+ask)/2) 基準の含み損益 pips を返す。
// max_hold / early_exit の deadline 判定で使う (exit-side でなく mid を見る)。
// ok=false は side が BUY/SELL 以外。unrealizedPipsExit (exit-side 基準) とは
// 価格基準が異なるため別関数。
func unrealizedPipsMid(rec port.PositionRecord, t market.Ticker, pipSize float64) (float64, bool) {
	mid := (t.Bid + t.Ask) / 2
	switch rec.Side {
	case string(order.SideBuy):
		return (mid - rec.EntryPrice) / pipSize, true
	case string(order.SideSell):
		return (rec.EntryPrice - mid) / pipSize, true
	default:
		return 0, false
	}
}

// closeOne performs the full close sequence for one position via the shared
// ExecuteCloseSaga (claim → cancel → broker close → resolve → record). The
// saga handles emergency_stop on any failure post-claim; this function only
// needs to translate "already CLOSING" into a debug log and continue.
func (u *ManageOpenPositions) closeOne(ctx context.Context, rec port.PositionRecord, t market.Ticker, reason string, now time.Time) {
	if u.CloseMutex != nil {
		u.CloseMutex.Lock()
		defer u.CloseMutex.Unlock()
	}

	// Paper fallback price (only used when broker returns ord.Price==0).
	// Live mode requires ResolveExecution and ignores this value.
	var paperExit float64
	if rec.Side == string(order.SideBuy) {
		paperExit = t.Bid
	} else {
		paperExit = t.Ask
	}

	res, err := ExecuteCloseSaga(ctx, CloseSagaInput{
		Mode:              u.Mode,
		Symbol:            u.Symbol,
		Broker:            u.Broker,
		Positions:         u.Positions,
		Closer:            u.Closer,
		EmergencyFlagPath: u.EmergencyFlagPath,
		Logger:            u.Logger,
		OnClosed:          u.OnClosed,
	}, rec, reason, paperExit, now)
	if err != nil {
		if errors.Is(err, ErrPositionAlreadyClosing) {
			u.Logger.Debug("close_skipped_already_in_flight", "id", rec.ID, "reason", reason)
			return
		}
		if errors.Is(err, ErrCloseRateUnavailable) {
			u.Logger.Info("close_deferred_quote_rate_unavailable", "id", rec.ID, "reason", reason,
				"note", "USD/JPY rate fetch failed; close deferred to next tick — position stays OPEN, protected by broker OCO")
			return
		}
		if errors.Is(err, ErrCloseRejectedRearmed) {
			u.Logger.Info("close_rejected_oco_rearmed", "id", rec.ID, "reason", reason,
				"note", "broker refused the close; OCO re-armed — position stays protected, reconcile records the eventual fill")
			return
		}
		if errors.Is(err, ErrCloseRejectedLegsIntact) {
			u.Logger.Info("close_rejected_legs_intact", "id", rec.ID, "reason", reason,
				"note", "broker refused the close; original OCO never removed — position stays protected, reconcile records the eventual fill")
			return
		}
		// Saga already tripped emergency_stop + logged; do not propagate.
		return
	}

	// PnL is computed inside the saga (権威ある trade record も saga 内で記録済み)。
	u.Logger.Info("position_closed",
		"id", rec.ID,
		"reason", reason,
		"side", rec.Side,
		"entry", rec.EntryPrice,
		"exit", res.ExitPrice,
		"pnl_jpy", res.ProfitLossJPY,
		"pnl_pips", res.ProfitLossPips,
	)
}

// evaluateExit は終了条件を優先順位付きで評価する dispatcher。
// 実ロジックは manage_open_positions_exits.go の純粋関数に分離している。
//
// 優先順位 (上位が発火したら即 return):
//  1. ratchet_takeprofit: 全モード共通。trailing TP 発火検出
//  2. ratchet_stoploss: 全モード共通。trailing STOP (損切り側 ratchet)。利確側が
//     armed なら 1 で return するので、ここは利確未 arm の玉だけが対象
//  3. max_hold / early_exit: 全モード共通。早期 exit window 発火は "early_exit"、
//     soft/hard deadline・extension 強制 close は "max_hold"
//  4. paper TP/SL: paper モードのみ。Live は GMO OCO がサーバ側で処理するので
//     bot 側で判定すると OCO と race してしまうため skip
//
// 各 evaluator の詳細コメントは manage_open_positions_exits.go を参照。
func (u *ManageOpenPositions) evaluateExit(rec port.PositionRecord, t market.Ticker, now time.Time) string {
	// Session flatten は最優先: 窓内では ratchet より先に発火させ、close_reason を
	// 「セッション掃除」として集計可能にする (壁直前 tick の ratchet 誤発火ラベルを防ぐ)。
	if u.SessionFlattenEnabled {
		if reason := evaluateSessionFlattenExit(rec, now, u.SessionFlattenStartMinuteJST); reason != "" {
			return reason
		}
	}
	if reason := evaluateRatchetExit(rec, t, u.PipSize); reason != "" {
		return reason
	}
	// 損切り側 ratchet (trailing stop)。利確側と同じく全モードで発火する
	// (Live は broker OCO の満額 SL より先に浅い傷で撤退させる)。利確側が
	// armed のときは上で先に return するので、ここに来るのは利確未 arm の玉
	// = まさに「深追いして満額 SL まで往復する」対象。
	if reason := evaluateLossRatchetExit(rec, t, u.PipSize); reason != "" {
		return reason
	}
	if reason := evaluateMaxHoldExit(rec, t, now, u.PipSize); reason != "" {
		return reason
	}
	if u.Mode.IsLive() {
		return ""
	}
	return evaluatePaperTPSLExit(rec, t, u.PipSize)
}
