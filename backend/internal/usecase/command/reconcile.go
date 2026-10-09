package command

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
)

// ReconcileMode controls what Reconcile does when the broker and DB disagree.
//
// Startup mode (ReconcileModeStartup):
//   - naked_broker_position (Paper): ADOPT into DB (INSERT a recovery PositionRecord)
//   - naked_broker_position (Live):  ADOPT as external_broker. The
//     user is allowed to open positions
//     directly in the GMO app — bot is
//     display-only for those rows. Source is
//     set so EntryAdmission and
//     ManageOpenPositions skip them.
//   - stale_db_position (Paper):     MarkClosed in DB with a synthetic
//     zero-PnL trade (no broker-side TP/SL
//     fills to consult in paper mode)
//   - stale_db_position (Live):      try to
//     RESOLVE the fill via the position's
//     recorded TP/SL settle leg executions
//     (same path as runtime). On success,
//     record the real trade. On failure,
//     DEFER to the runtime reconcile loop
//     (no trip, no synthetic zero-PnL trade).
//
// Runtime mode (ReconcileModeRuntime):
//   - naked_broker_position (Live):  ADOPT as external_broker. The
//     user can open positions in the GMO app
//     even while the bot is running.
//   - naked_broker_position (Paper): trip emergency_stop — in paper mode
//     the broker IS the PaperBroker so a
//     new naked position is a bot bug.
//   - stale_db_position:     try to RESOLVE the fill via the position's
//     recorded TP/SL settle leg executions. On
//     success, atomically Claim → CloseAndRecord with
//     the real fill price + close reason. On failure
//     (Live): defer (StaleGracePeriod) → estimated
//     close if price is clearly past SL/TP → keep
//     deferring → trip only after StaleHardTripPeriod.
//     Never books a 0-PnL trade.
//
// Default zero-value is Startup so callers explicitly opt into the stricter
// behaviour for periodic reconciliation.
type ReconcileMode int

const (
	ReconcileModeStartup ReconcileMode = iota
	ReconcileModeRuntime
)

// OpsCounters is the subset of Live observability counters the trading-path
// commands bump. *app.Counters satisfies it; defined here so usecase/command
// need not import the app layer. nil = no-op (tests / unwired paths).
type OpsCounters interface {
	IncrNakedPositions()
	IncrCloseRaces()
	IncrResolveTimeouts()
}

// Reconcile compares the broker's view of open positions with the local DB.
//
// Goal: the bot should recover gracefully when the DB has
// lost track of positions (DB crash, app restart, manual DB ops, etc.). The
// broker is the source of truth for "what's actually open" — Reconcile pulls
// that view in and updates the local DB to match.
type Reconcile struct {
	Broker    port.Broker
	Positions port.PositionRepository
	Notifier  port.Notifier
	Symbol    string
	Mode      ReconcileMode
	Logger    *slog.Logger

	// LiveMode tells the runtime reconciler whether to invoke the fill-
	// resolution path (Live OCO settle orders have TP/SL legs to query). For
	// paper mode runtime reconcile, stale_db_position trips emergency_stop
	// directly because we have no broker-side execution feed to consult.
	LiveMode config.Mode

	// Closer is needed by runtime mode to atomically transition a resolved
	// stale_db_position from OPEN/CLOSING → CLOSED + insert the trade row.
	// nil in startup mode (MarkClosed is used instead).
	Closer port.PositionCloser

	// StrategyConfigs is consulted on EVERY external-position adoption path to
	// resolve the current active config_id (positions.strategy_config_id is
	// now a NOT NULL FK after Step B). Required by BOTH the paper-startup
	// adoption path AND the Live runtime external-adoption path (a user opens
	// a trade in the GMO app mid-run). If nil and adoption is attempted, the
	// adoption cannot resolve a config_id → returns extNoConfig → the position
	// is deferred then trips emergency_stop instead of being adopted (so the
	// runtime builder must wire this, not only the startup one).
	StrategyConfigs port.StrategyConfigRepository

	// EmergencyFlagPath is the path to runtime/emergency_stop.flag.
	// Empty disables the trip (tests / one-shot diagnostics).
	EmergencyFlagPath string

	// StaleGracePeriod defers the runtime stale_db_position fallback+trip.
	//
	// Why: when GMO settles a position via its OCO SL leg, the runtime reconcile
	// can run in the ~30s window before the GMO executions feed returns that
	// fill. resolveAndRecordClose then finds no fill; tripping emergency_stop
	// there is premature, because one reconcile cycle later the real fill is
	// resolvable and can be recorded cleanly.
	//
	// Within this window of first observing a stale, unresolvable Live runtime
	// position, handleStaleDB DEFERS — neither estimated/synthetic-closing nor
	// tripping — so the real fill has a chance to resolve on a later pass. Once
	// the window elapses, the fallback→trip chain runs. Zero disables
	// the grace (trip on first detection; existing tests and
	// the paper/non-live paths rely on this).
	StaleGracePeriod time.Duration

	// StaleHardTripPeriod bounds how long a Live runtime stale_db_position may
	// keep DEFERRING before it escalates to an emergency_stop trip.
	//
	// Why: a freshly-opened position can be omitted from the broker's
	// GetOpenPositions feed for ~90s (feed lag on the open), so reconcile deems it
	// stale while its close fill is not resolvable (it is still genuinely OPEN at
	// the broker) and the price is inside the OCO band (ambiguous). Booking a 0-PnL
	// synthetic close then would destroy the real fill that resolves minutes
	// later. So there is NO synthetic 0-PnL fallback on the
	// live-runtime path: after grace, reconcile keeps DEFERRING (so a later pass
	// records the real fill) until StaleHardTripPeriod elapses, at which point a
	// genuine orphan trips emergency_stop (alert a human) — still WITHOUT ever
	// fabricating PnL. Should be >= StaleGracePeriod. Zero disables the hard trip
	// (defer indefinitely; used by tests that never reach this branch).
	StaleHardTripPeriod time.Duration

	// staleFirstSeen records, per position id, the first Clock time it was
	// observed stale & unresolvable in Live runtime mode. Cleared once the
	// position resolves. Guarded by staleMu. Lazily initialised. Shared by the
	// StaleGracePeriod and StaleHardTripPeriod thresholds (both measured from
	// this first-observation time).
	staleFirstSeen map[int64]time.Time
	staleMu        sync.Mutex

	// ExternalAdoptGrace bounds how long a Live naked broker position that cannot be adopted (no
	// active config resolvable for its symbol) may keep DEFERRING before it trips emergency_stop.
	// A transient broker artifact (e.g. a settle leg right after a close, gone by the next pass) is
	// never tripped; only a position that PERSISTS unadoptable past this window halts the bot. This
	// prevents spurious emergency_stops from transient external positions that are gone within
	// minutes. Zero = trip on first
	// detection (relied on by tests and the non-runtime paths).
	ExternalAdoptGrace time.Duration
	// externalFirstSeen records, per broker_position_id, the first Clock time a Live naked position
	// was observed unadoptable. Cleared on adoption or when the position disappears. Guarded by externalMu.
	externalFirstSeen map[string]time.Time
	externalMu        sync.Mutex

	// PendingTracker は entry saga 進行中 (broker fill 済みだが DB INSERT 未完) の
	// broker_position_id を追跡する in-memory tracker。handleNakedBroker は
	// IsPending==true の id を adopt 候補から除外し、次の reconcile pass で
	// DB と broker が整合してから処理する。
	//
	// entry saga 中の数秒〜十数秒の race-window で reconcile が自分のポジを
	// external adopt 経路に流して emergency_stop を trip させないための対策。
	// nil 許容 (テスト互換)。
	PendingTracker port.PendingPositionTracker

	// PendingTTL は pending entry を「saga 進行中」とみなす上限。
	// saga crash で MarkResolved が呼ばれないと pending が永久化し、naked broker
	// position を無期限に隠すため、TTL 超過の pending は skip 対象から外して通常の
	// naked-broker 経路に戻す。saga 実時間は数秒なので 10 分は十分保守的。
	// Zero は defaultPendingTTL。
	PendingTTL time.Duration

	// Clock is injected for testability; defaults to time.Now.
	Clock func() time.Time

	// Counters surfaces reconcile-detected anomalies (naked_broker_position) on
	// /api/status. nil = no-op. Observability only — never affects control flow.
	Counters OpsCounters

	// OnPositionClosed is called exactly once per position AFTER a close is fully
	// settled by reconcile (real fill resolved via resolveAndRecordClose, or the
	// estimated-close ladder) — never on defer/trip/adoption paths. nil = no-op.
	// event_retrigger: wired to the LLM re-judgment coordinator; the
	// broker-side OCO fill is THE normal live close, and reconcile is the only
	// place the bot learns about it. Observability/trigger only — must never
	// affect reconcile control flow.
	OnPositionClosed func(symbol, reason string)
}

// defaultPendingTTL は PendingTTL 未指定時の既定値。entry saga の実時間は数秒
// なので、これを超えて pending のままの id は saga crash 由来とみなしてよい。
const defaultPendingTTL = 10 * time.Minute

func (u *Reconcile) pendingTTL() time.Duration {
	if u.PendingTTL > 0 {
		return u.PendingTTL
	}
	return defaultPendingTTL
}

// isPendingStale は brokerPositionID が TTL 超過の pending (= saga crash で
// MarkResolved されなかった entry) かを返す。呼び出し側で PendingTracker の
// nil チェック済みであること。
func (u *Reconcile) isPendingStale(brokerPositionID string) bool {
	for _, id := range u.PendingTracker.StaleIDs(u.pendingTTL()) {
		if id == brokerPositionID {
			return true
		}
	}
	return false
}

// ReconcileSummary returns counts of what happened so callers can log a
// one-liner and operators can grep for it.
type ReconcileSummary struct {
	Adopted     int // broker had a position we now record in DB
	MarkedDone  int // DB had OPEN that broker has closed (startup: blind MarkClosed)
	Resolved    int // runtime: stale_db_position resolved via settle-leg execution
	Deferred    int // runtime: stale_db_position held within grace period (no trip yet)
	Tripped     int // discrepancies that tripped emergency_stop (Runtime mode)
	BrokerCount int
	DBCount     int
}

// Run executes one reconciliation pass.
func (u *Reconcile) Run(ctx context.Context) (ReconcileSummary, error) {
	now := time.Now()
	if u.Clock != nil {
		now = u.Clock()
	}

	brokerPositions, err := u.Broker.GetOpenPositions(ctx, u.Symbol)
	if err != nil {
		return ReconcileSummary{}, fmt.Errorf("broker open positions: %w", err)
	}
	dbPositions, err := u.Positions.ListOpenOrClosing(ctx, u.Symbol)
	if err != nil {
		return ReconcileSummary{}, fmt.Errorf("db open positions: %w", err)
	}
	summary := ReconcileSummary{BrokerCount: len(brokerPositions), DBCount: len(dbPositions)}

	// Broker-side metadata lives in positions_live now. Look up per row.
	dbLiveByID := map[int64]*port.PositionLive{}
	dbByBrokerID := map[string]port.PositionRecord{}
	for _, p := range dbPositions {
		live, lerr := u.Positions.GetLive(ctx, p.ID)
		if lerr != nil {
			return ReconcileSummary{}, fmt.Errorf("db get_live position %d: %w", p.ID, lerr)
		}
		dbLiveByID[p.ID] = live
		if live != nil && live.BrokerPositionID != "" {
			dbByBrokerID[live.BrokerPositionID] = p
		}
	}
	brokerByID := map[string]struct{}{}

	// Pass 1: naked_broker_position — broker にあるが DB にない
	// 分岐ロジックは reconcile_handlers.go の handleNakedBroker に分離。
	for _, p := range brokerPositions {
		brokerByID[p.BrokerPositionID] = struct{}{}
		if _, known := dbByBrokerID[p.BrokerPositionID]; known {
			continue
		}
		u.handleNakedBroker(ctx, p, now, &summary)
	}
	// Forget unadoptable first-seen marks for broker positions that have disappeared, so a
	// transient artifact (gone this pass) is never later mistaken for a persistent one.
	u.pruneExternalUnadoptable(brokerByID)

	// Pass 2: stale_db_position — DB が OPEN/CLOSING だが broker にない
	// 分岐ロジックは reconcile_handlers.go の handleStaleDB に分離。
	for _, p := range dbPositions {
		live := dbLiveByID[p.ID]
		if live == nil || live.BrokerPositionID == "" {
			// positions_live 行がない (legacy paper 行) → 比較対象なし、skip
			continue
		}
		if _, ok := brokerByID[live.BrokerPositionID]; ok {
			continue
		}
		u.handleStaleDB(ctx, p, live, now, &summary)
	}

	return summary, nil
}

// resolveAndRecordClose attempts to determine why a position disappeared
// from the broker (TP fill vs SL fill) and atomically records the trade.
// buildCloseTrade assembles the trades-row payload from a position record plus
// the resolved exit price / PnL / reason / close time / per-trade costs
// (fee_jpy / swap_jpy / fee_estimated). Shared by every close
// path (reconcile resolve & estimated, reconcile cold-close, close saga) so the
// TradeRecord mapping has a single source of truth.
func buildCloseTrade(p port.PositionRecord, exitPrice, pnlPips, pnlJPY float64, reason string, closedAt time.Time, costs closeCosts) port.TradeRecord {
	return port.TradeRecord{
		PositionID:       p.ID,
		StrategyConfigID: p.StrategyConfigID,
		Symbol:           p.Symbol,
		Side:             p.Side,
		Quantity:         p.Quantity,
		EntryPrice:       p.EntryPrice,
		ExitPrice:        exitPrice,
		ProfitLossPips:   pnlPips,
		ProfitLossJPY:    pnlJPY, // GROSS のまま (net は導出側)
		CloseReason:      reason,
		FeeJPY:           costs.FeeJPY,
		SwapJPY:          costs.SwapJPY,
		FeeEstimated:     costs.FeeEstimated,
		OpenedAt:         p.OpenedAt,
		ClosedAt:         closedAt,
	}
}

// Returns (true, nil) on successful close + trade insert.
// Returns (false, nil) when no fill can be found yet — caller decides
// whether to retry next pass or trip emergency_stop.
// Returns (false, err) for true errors (broker API failure, DB errors).
func (u *Reconcile) resolveAndRecordClose(ctx context.Context, p port.PositionRecord, live *port.PositionLive, now time.Time) (bool, error) {
	if live == nil {
		return false, fmt.Errorf("no positions_live row for position %d", p.ID)
	}

	fill := closeFill{}
	// Path 1: leg orderId が記録されている場合は /v1/executions で TP/SL を直接引く
	if live.TPOrderID != "" || live.SLOrderID != "" {
		f, err := u.findFillForLegs(ctx, live.TPOrderID, live.SLOrderID)
		if err != nil {
			return false, err
		}
		fill = f
	}
	// Path 2: leg id 無し or leg fill 未発見 → broker_position_id 経由で
	// /v1/latestExecutions を引いて positionId 一致の close fill を探す。
	// ResolveSettleLegs が soft-fail して leg id が空のまま GMO 側で SL が約定しても、
	// synthetic 0 PnL に倒さず実約定を拾うための経路。
	if fill.Reason == "" && live.BrokerPositionID != "" {
		f, err := u.findFillByPositionLookup(ctx, p, live.BrokerPositionID)
		if err != nil {
			// lookup 自体の失敗 (broker API エラー) は WARN にとどめて「fill 未発見」
			// 扱いで続行 — 以降は caller (handleStaleDB) が DEFER / 推定 close /
			// trip を判定する (Live では 0-PnL を捏造しない)。
			u.Logger.Warn("reconcile_position_lookup_failed",
				"db_position_id", p.ID, "broker_position_id", live.BrokerPositionID, "err", err)
		} else if f.Reason != "" {
			fill = f
		}
	}
	exitPrice, reason := fill.Price, fill.Reason
	if reason == "" {
		// 経路 1/2 とも fill 不明 — caller (handleStaleDB) が DEFER / 推定 close /
		// trip を判定する (synthetic は Paper startup のみ)。元の挙動互換のため、leg id が完全に無い場合は
		// 明示エラーで返して、leg id 有りで実 fill 未発見な場合は (false, nil) で返す。
		if live.TPOrderID == "" && live.SLOrderID == "" {
			return false, fmt.Errorf("no recorded settle legs for position %d (positionId lookup also yielded no match)", p.ID)
		}
		return false, nil
	}

	// Claim OPEN → CLOSING so the close saga and reconcile cannot collide.
	// A row already in CLOSING (saga crashed
	// before CloseAndRecord) is still resolvable — fall through to
	// CloseAndRecord which flips CLOSING → CLOSED. We only skip when the
	// row is already CLOSED (= someone else finalised it).
	if p.Status == port.PositionStatusOpen {
		claimed, cerr := u.Positions.ClaimForClose(ctx, p.ID, now)
		if cerr != nil {
			return false, fmt.Errorf("claim_for_close: %w", cerr)
		}
		if !claimed {
			// Already CLOSED, or another path finalised between our list
			// query and the claim. Skip.
			return false, nil
		}
	} else if p.Status != port.PositionStatusClosing {
		// Defensive: ListOpenOrClosing filters to OPEN|CLOSING, so we
		// shouldn't see anything else. Bail without action.
		return false, nil
	}

	quoteJPYRate, qerr := resolveQuoteJPYRate(ctx, u.Broker, p.Symbol)
	if qerr != nil {
		return false, fmt.Errorf("resolve_quote_jpy_rate: %w", qerr)
	}
	pnlPips, pnlJPY, _ := position.ComputeClosePnL(
		p.EntryPrice, exitPrice, order.Side(p.Side), p.Quantity, p.Symbol, quoteJPYRate,
	)
	// err は p.Side が DB の OPEN/CLOSING 行から来る値なので invariant 上
	// BUY/SELL のみ。万一壊れていても silent 0,0 になるだけ。

	// live のみコスト合成 (paper reconcile は手数料が存在しない)。
	costs := closeCosts{}
	if u.LiveMode.IsLive() {
		costs = composeLiveCloseCosts(p, exitPrice, fill.FeeJPY, fill.SwapJPY, fill.Reported, quoteJPYRate)
	}
	trade := buildCloseTrade(p, exitPrice, pnlPips, pnlJPY, reason, now, costs)
	ok, derr := u.Closer.CloseAndRecord(ctx, p.ID, now, trade)
	if derr != nil {
		return false, fmt.Errorf("close_and_record: %w", derr)
	}
	if !ok {
		// Someone else flipped state between Claim and CloseAndRecord — exit
		// without tripping (the other path will trip if it must).
		return false, nil
	}
	u.Logger.Info("reconcile_resolved_stale_db_position",
		"db_position_id", p.ID, "broker_position_id", live.BrokerPositionID,
		"reason", reason, "exit", exitPrice, "pnl_jpy", pnlJPY)
	if u.OnPositionClosed != nil {
		u.OnPositionClosed(p.Symbol, reason)
	}
	return true, nil
}

// NOTE: there is deliberately no live-runtime synthetic 0-PnL close path here.
// Booking exit=entry/0-PnL for an unresolvable stale position would destroy the
// real fill of a position that is merely missing from the broker feed
// transiently (still open) — see StaleHardTripPeriod. The live-runtime path
// DEFERS until the real fill resolves, then trips after a hard window for
// genuine orphans, and NEVER fabricates PnL. The only remaining
// synthetic-close is Paper startup (handleStaleDB case 3, inline), where there
// is no execution feed to consult at all.

// estimateStaleClose は broker が既に閉じた stale ポジの実約定が解決できないとき、
// 現在値 (current) が OCO の SL/TP 水準を「明確に超えている」場合だけ、その水準で
// 約定したとみなして (exit, reason, true) を返す純関数。SL と TP の間 (どちらが
// 約定したか不明) は (0, "", false) を返し、live-runtime の caller は 0-PnL を
// 捏造せず DEFER する (後の pass で実約定を拾う)。pip>0 前提。
func estimateStaleClose(side order.Side, entry, current, tpPips, slPips, pip float64) (exit float64, reason string, ok bool) {
	if pip <= 0 {
		return 0, "", false
	}
	tpPrice, slPrice := position.ComputeTPSLPrices(side, entry, tpPips, slPips, pip)
	switch side {
	case order.SideBuy:
		if slPips > 0 && current <= slPrice {
			return slPrice, "stop_loss", true
		}
		if tpPips > 0 && current >= tpPrice {
			return tpPrice, "take_profit", true
		}
	case order.SideSell:
		if slPips > 0 && current >= slPrice {
			return slPrice, "stop_loss", true
		}
		if tpPips > 0 && current <= tpPrice {
			return tpPrice, "take_profit", true
		}
	}
	return 0, "", false
}

// recordEstimatedClose は Live で stale ポジの実約定が解決できなかったときに、
// grace 後に試す改善パス。現在値を取得し、OCO の SL/TP 水準を明確に超えていれば
// その水準の実 PnL で trade を記録する。曖昧 / ticker 取得不可 / paper では false
// を返し、live-runtime の caller は 0-PnL を捏造せず DEFER する (後の pass で実約定を
// 拾い、genuine orphan は hard window 超過で trip)。再起動と broker SL 約定が同時刻に
// 重なったとき、実際の損益が 0 として帳簿から消えるのを防ぐ。
func (u *Reconcile) recordEstimatedClose(ctx context.Context, p port.PositionRecord, now time.Time) bool {
	if !u.LiveMode.IsLive() {
		return false
	}
	tk, err := u.Broker.GetTicker(ctx, p.Symbol)
	if err != nil || tk == nil {
		return false
	}
	pip := market.PipSize(p.Symbol)
	exit, reason, ok := estimateStaleClose(order.Side(p.Side), p.EntryPrice, tk.Mid(), p.TakeProfitPips, p.StopLossPips, pip)
	if !ok {
		return false
	}
	// Claim OPEN→CLOSING (tolerate already-CLOSING), 同 recordSyntheticClose。
	if p.Status == port.PositionStatusOpen {
		claimed, cerr := u.Positions.ClaimForClose(ctx, p.ID, now)
		if cerr != nil {
			u.Logger.Error("reconcile_estimated_claim_failed", "db_position_id", p.ID, "err", cerr)
			return false
		}
		if !claimed {
			return false
		}
	}
	quoteJPYRate, qerr := resolveQuoteJPYRate(ctx, u.Broker, p.Symbol)
	if qerr != nil {
		u.Logger.Error("reconcile_estimated_quote_rate_failed", "db_position_id", p.ID, "symbol", p.Symbol, "err", qerr)
		return false
	}
	pnlPips, pnlJPY, _ := position.ComputeClosePnL(p.EntryPrice, exit, order.Side(p.Side), p.Quantity, p.Symbol, quoteJPYRate)
	// 実約定が解決できない推定 close — close leg fee は 0.002% 推定、
	// swap は不明 (0)。fee_estimated=true で後の backfill 対象として判別可能にする。
	costs := composeLiveCloseCosts(p, exit, 0, 0, false, quoteJPYRate)
	trade := buildCloseTrade(p, exit, pnlPips, pnlJPY, reason, now, costs)
	rok, derr := u.Closer.CloseAndRecord(ctx, p.ID, now, trade)
	if derr != nil || !rok {
		u.Logger.Error("reconcile_estimated_close_and_record_failed", "db_position_id", p.ID, "err", derr, "ok", rok)
		return false
	}
	u.Logger.Warn("reconcile_estimated_close_recorded",
		"db_position_id", p.ID, "reason", reason, "entry", p.EntryPrice, "exit", exit, "pnl_jpy", pnlJPY,
		"note", "real fill unresolved; exit estimated from OCO SL/TP level vs current market (not the exact broker fill)")
	if u.OnPositionClosed != nil {
		u.OnPositionClosed(p.Symbol, reason)
	}
	return true
}

// findFillByPositionLookup は broker_position_id をキーに /v1/latestExecutions
// (port.LatestExecutionsLookup) を引き、対象 position の決済 fill を探す。
//
// 経路 1 (findFillForLegs) で leg orderId が記録されていない / 解決できなかった
// ケースで使う fallback。filter ルール:
//   - PositionID == brokerPositionID
//   - Side != position.Side (反対 side = close)
//   - Timestamp >= position.OpenedAt (建て約定を誤って拾わない安全装置)
//
// 複数 match なら最も新しい timestamp を採用 (部分決済は最後の fill が close)。
// close_reason は exit price が TP/SL の理論価格にマッチするかで分類:
//   - TP price ± 0.5 pip → "take_profit"
//   - SL price ± 0.5 pip → "stop_loss"
//   - その他 → "broker_close" (GMO アプリ手動決済など)
//
// Broker が LatestExecutionsLookup 未実装なら (0, "", nil) を返す (caller は
// fill 未発見として扱う)。
func (u *Reconcile) findFillByPositionLookup(ctx context.Context, p port.PositionRecord, brokerPositionID string) (closeFill, error) {
	lookup, ok := u.Broker.(port.LatestExecutionsLookup)
	if !ok {
		return closeFill{}, nil
	}
	execs, err := lookup.GetLatestExecutionsBySymbol(ctx, p.Symbol)
	if err != nil {
		return closeFill{}, fmt.Errorf("latest_executions(%s): %w", p.Symbol, err)
	}
	openSide := order.Side(p.Side)
	closeSide := openSide.Opposite()
	var best *order.Execution
	// fee / settledSwap は対象 position の close fill 全件で合算する
	// (部分決済が複数 fill に割れていても過少報告しない)。price は最新 fill。
	feeSum, swapSum := 0.0, 0.0
	for i := range execs {
		e := &execs[i]
		if string(e.PositionID) != brokerPositionID {
			continue
		}
		if e.Side != closeSide {
			// entry 約定 (= 同じ side) は除外
			continue
		}
		if !p.OpenedAt.IsZero() && e.Timestamp.Before(p.OpenedAt) {
			continue
		}
		feeSum += e.FeeJPY
		swapSum += e.SettledSwapJPY
		if best == nil || e.Timestamp.After(best.Timestamp) {
			best = e
		}
	}
	if best == nil {
		return closeFill{}, nil
	}
	return closeFill{
		Price:    best.Price,
		FeeJPY:   feeSum,
		SwapJPY:  swapSum,
		Reason:   classifyCloseReason(openSide, p.EntryPrice, best.Price, p.TakeProfitPips, p.StopLossPips, p.Symbol),
		Reported: true,
	}, nil
}

// classifyCloseReason は exit price を OCO の執行原理で方向付き分類する。
//   - SL は逆指値 (トリガー後は成行) → SL 価格より不利側の fill は全て stop_loss
//     (週末 gap で大きく滑っても SL fill は SL)。
//   - TP は指値 → TP 価格より有利側の fill は全て take_profit。
//   - OCO が生きている限り TP より有利 / SL より不利な価格の手動決済は成立しない
//     (先に OCO が約定する) ので、無制限側は安全。中間帯だけが broker_close。
//   - 境界の float 誤差・微小滑りは 0.5 pip の許容で吸収する
//     (「±0.5pip 一致」判定だと 0.5pip ちょうどの滑りが float64 境界で
//     broker_close に化けるため、片側は無制限にしている)。
func classifyCloseReason(side order.Side, entry, exit, tpPips, slPips float64, symbol string) string {
	pip := market.PipSize(symbol)
	if pip <= 0 {
		return "broker_close"
	}
	tpPrice, slPrice := position.ComputeTPSLPrices(side, entry, tpPips, slPips, pip)
	tol := 0.5 * pip
	favorableSign := 1.0 // BUY: 価格が高いほど有利
	if side == order.SideSell {
		favorableSign = -1.0
	}
	if tpPips > 0 && favorableSign*(exit-tpPrice) >= -tol {
		return "take_profit"
	}
	if slPips > 0 && favorableSign*(exit-slPrice) <= tol {
		return "stop_loss"
	}
	return "broker_close"
}

// closeFill は reconcile が broker 約定履歴から復元した close fill。
// Reason=="" は「fill 未発見」。Reported=true は fee/swap が broker 実報告値
// であることを示す (composeLiveCloseCosts の closeLegReported へ渡す)。
type closeFill struct {
	Price    float64
	FeeJPY   float64
	SwapJPY  float64
	Reason   string
	Reported bool
}

// legFill は 1 leg (TP or SL) の約定一覧を closeFill に畳む。price は先頭 fill、
// fee / settledSwap は全 fill の合算 (部分約定対応)。
func legFill(execs []order.Execution, reason string) closeFill {
	out := closeFill{Price: execs[0].Price, Reason: reason, Reported: true}
	for _, e := range execs {
		out.FeeJPY += e.FeeJPY
		out.SwapJPY += e.SettledSwapJPY
	}
	return out
}

// findFillForLegs queries GMO executions for the recorded TP and SL settle
// legs. Returns a closeFill whose Reason is "take_profit", "stop_loss", or
// "" if neither leg has a fill.
func (u *Reconcile) findFillForLegs(ctx context.Context, tpOrderID, slOrderID string) (closeFill, error) {
	if tpOrderID != "" {
		execs, err := u.Broker.GetExecutions(ctx, tpOrderID)
		if err != nil {
			return closeFill{}, fmt.Errorf("get executions (tp=%s): %w", tpOrderID, err)
		}
		if len(execs) > 0 {
			return legFill(execs, "take_profit"), nil
		}
	}
	if slOrderID != "" {
		execs, err := u.Broker.GetExecutions(ctx, slOrderID)
		if err != nil {
			return closeFill{}, fmt.Errorf("get executions (sl=%s): %w", slOrderID, err)
		}
		if len(execs) > 0 {
			return legFill(execs, "stop_loss"), nil
		}
	}
	return closeFill{}, nil
}

// resolvePaperActiveConfigID returns the config_id of the current active
// strategy_configs row for (symbol, paper_mode). Adopted paper positions
// FK to this row (since `strategy_config_id='recovered'` sentinel was
// dropped in Step B). Returns an error when StrategyConfigs is not wired
// or when no active row exists — callers trip emergency_stop in both
// cases.
func (u *Reconcile) resolvePaperActiveConfigID(ctx context.Context, symbol string) (string, error) {
	if u.StrategyConfigs == nil {
		return "", fmt.Errorf("StrategyConfigs not wired into Reconcile; cannot FK adopt")
	}
	rec, err := u.StrategyConfigs.GetActive(ctx, symbol, string(config.ModePaperConfig))
	if err != nil {
		return "", fmt.Errorf("get active paper config: %w", err)
	}
	if rec == nil {
		return "", fmt.Errorf("no active paper config for %s; refusing to adopt", symbol)
	}
	return rec.ConfigID, nil
}

// resolveActiveConfigIDForExternalAdoption returns a config_id suitable as
// FK target for an externally-opened position. The Source field (set via
// recovered_positions.recovery_reason="external_broker_adoption") is what
// downstream consumers actually read; the FK is just satisfied here.
//
// Preference order: active live_config → active paper_config. The fallback
// is what unblocks the very first live restart, when no advisor cycle has
// produced a live_config yet but the bot was previously running in paper
// mode (so an active paper_config still exists).
func (u *Reconcile) resolveActiveConfigIDForExternalAdoption(ctx context.Context, symbol string) (string, error) {
	if u.StrategyConfigs == nil {
		return "", fmt.Errorf("StrategyConfigs not wired into Reconcile; cannot FK adopt")
	}
	if rec, err := u.StrategyConfigs.GetActive(ctx, symbol, string(config.ModeLiveConfig)); err != nil {
		return "", fmt.Errorf("get active live config: %w", err)
	} else if rec != nil {
		return rec.ConfigID, nil
	}
	if rec, err := u.StrategyConfigs.GetActive(ctx, symbol, string(config.ModePaperConfig)); err != nil {
		return "", fmt.Errorf("get active paper config (fallback): %w", err)
	} else if rec != nil {
		return rec.ConfigID, nil
	}
	return "", fmt.Errorf("no active live or paper config for %s; cannot adopt external position without FK target", symbol)
}

// externalAdoptResult is the outcome of attempting to adopt a Live naked broker position.
// The caller (handleNakedBroker) decides whether to trip emergency_stop or DEFER — so the
// ExternalAdoptGrace window can absorb transient broker artifacts without halting the bot.
type externalAdoptResult int

const (
	extAdopted      externalAdoptResult = iota // recorded in DB as external_broker
	extNoConfig                                // no active config resolvable for the symbol (grace-eligible)
	extInsertFailed                            // DB insert error (real fault — caller trips)
)

// adoptExternalLivePosition records an externally-opened broker position in DB with
// recovery_reason=external_broker_adoption. It does NOT trip emergency_stop itself — it returns
// the outcome so the caller can apply the ExternalAdoptGrace policy. Bot treats the resulting row
// as display-only: EntryAdmission and ManageOpenPositions skip it via PositionRecord.Source.
func (u *Reconcile) adoptExternalLivePosition(ctx context.Context, p position.Position, now time.Time) externalAdoptResult {
	cfgID, err := u.resolveActiveConfigIDForExternalAdoption(ctx, p.Symbol)
	if err != nil {
		u.Logger.Error("reconcile_external_adopt_no_active_config",
			"broker_position_id", p.BrokerPositionID, "err", err)
		return extNoConfig
	}
	rec := port.PositionRecord{
		Symbol: p.Symbol, Side: string(p.Side), Quantity: p.Quantity,
		EntryPrice: p.EntryPrice,
		// Bot does not manage external positions — leave TP/SL/MaxHold zero
		// so the values can never be misinterpreted as bot-owned targets.
		TakeProfitPips:   0,
		StopLossPips:     0,
		MaxHoldMinutes:   0,
		StrategyConfigID: cfgID,
		Status:           port.PositionStatusOpen,
		OpenedAt:         coalesceTime(p.OpenedAt, now),
	}
	in := port.PositionInsertInput{
		Position: rec,
		Live:     &port.PositionLive{BrokerPositionID: p.BrokerPositionID},
		Recovered: &port.RecoveredPositionMeta{
			Reason:      port.RecoveryReasonExternalBrokerAdoption,
			RecoveredAt: now,
		},
	}
	if _, ierr := u.Positions.Insert(ctx, in); ierr != nil {
		u.Logger.Error("reconcile_external_adopt_insert_failed",
			"broker_position_id", p.BrokerPositionID, "err", ierr)
		return extInsertFailed
	}
	u.Logger.Info("reconcile_adopted_external_broker_position",
		"broker_position_id", p.BrokerPositionID,
		"side", string(p.Side), "qty", p.Quantity, "entry", p.EntryPrice,
		"strategy_config_id", cfgID)
	return extAdopted
}

// externalUnadoptableFirstSeen returns the first Clock time the given broker_position_id was seen
// unadoptable, recording `now` if this is the first sighting.
func (u *Reconcile) externalUnadoptableFirstSeen(brokerPositionID string, now time.Time) time.Time {
	u.externalMu.Lock()
	defer u.externalMu.Unlock()
	if u.externalFirstSeen == nil {
		u.externalFirstSeen = make(map[string]time.Time)
	}
	if t, ok := u.externalFirstSeen[brokerPositionID]; ok {
		return t
	}
	u.externalFirstSeen[brokerPositionID] = now
	return now
}

// clearExternalUnadoptable forgets a broker_position_id once it is adopted or disappears.
func (u *Reconcile) clearExternalUnadoptable(brokerPositionID string) {
	u.externalMu.Lock()
	defer u.externalMu.Unlock()
	delete(u.externalFirstSeen, brokerPositionID)
}

// pruneExternalUnadoptable drops first-seen entries for broker positions no longer present, so a
// transient position that vanished does not later be mistaken for a persistent one.
func (u *Reconcile) pruneExternalUnadoptable(present map[string]struct{}) {
	u.externalMu.Lock()
	defer u.externalMu.Unlock()
	for id := range u.externalFirstSeen {
		if _, ok := present[id]; !ok {
			delete(u.externalFirstSeen, id)
		}
	}
}

// coalesceTime returns t if non-zero else fallback. Adopted positions may not
// carry an OpenedAt — fall back to "now" so MaxHoldMinutes starts ticking.
func coalesceTime(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}

// staleElapsed reports whether `threshold` has elapsed since the first time
// position `id` was observed stale & unresolvable in Live runtime mode. On the
// first observation it records `now` (shared staleFirstSeen map) and returns
// false (defer). threshold <= 0 disables the check and returns disabledReturn
// without touching the map. Shared by staleGraceElapsed / staleHardTripElapsed,
// which differ only in their threshold and disabled-return value.
func (u *Reconcile) staleElapsed(id int64, now time.Time, threshold time.Duration, disabledReturn bool) bool {
	if threshold <= 0 {
		return disabledReturn
	}
	u.staleMu.Lock()
	defer u.staleMu.Unlock()
	if u.staleFirstSeen == nil {
		u.staleFirstSeen = map[int64]time.Time{}
	}
	first, seen := u.staleFirstSeen[id]
	if !seen {
		u.staleFirstSeen[id] = now
		return false
	}
	return now.Sub(first) >= threshold
}

// staleGraceElapsed reports whether the grace period for a stale, unresolvable
// runtime position has elapsed (=> proceed to fallback/trip). With
// StaleGracePeriod <= 0 it always returns true (legacy immediate behaviour).
func (u *Reconcile) staleGraceElapsed(id int64, now time.Time) bool {
	return u.staleElapsed(id, now, u.StaleGracePeriod, true)
}

// staleHardTripElapsed reports whether a Live runtime stale, unresolvable
// position has been observed long enough to escalate from "keep deferring" to
// an emergency_stop trip. It reads the same first-seen timestamp recorded by
// staleGraceElapsed (which runs first on the live-runtime path). With
// StaleHardTripPeriod <= 0 the hard trip is disabled — the position defers
// indefinitely (it is never 0-PnL synthetic-closed regardless).
func (u *Reconcile) staleHardTripElapsed(id int64, now time.Time) bool {
	return u.staleElapsed(id, now, u.StaleHardTripPeriod, false)
}

// clearStaleGrace forgets any deferred-trip bookkeeping for a position id,
// called once it resolves so the map does not grow unbounded and a future
// stale episode starts its grace window fresh.
func (u *Reconcile) clearStaleGrace(id int64) {
	u.staleMu.Lock()
	defer u.staleMu.Unlock()
	if u.staleFirstSeen != nil {
		delete(u.staleFirstSeen, id)
	}
}

// tripEmergencyStop は reconcile 由来の理由を "reconcile:" プレフィックス付きで
// flag に書き込む。
// 注: 他の subsystem (manual_ / execute_) は underscore プレフィックスを使っており
// 形式が違うが、既存 observability (dashboard / log filter) との後方互換のため
// "reconcile:" の colon 形式を意図的に維持する。
func (u *Reconcile) tripEmergencyStop(reason string) {
	if err := safety.Trip(u.EmergencyFlagPath, "reconcile:"+reason); err != nil {
		u.Logger.Error("emergency_stop_write_failed", "err", err)
	}
}

func (u *Reconcile) warn(ctx context.Context, title, body string, meta map[string]any) {
	if u.Logger != nil {
		args := []any{"title", title}
		for k, v := range meta {
			args = append(args, k, v)
		}
		u.Logger.Warn(body, args...)
	}
	if u.Notifier != nil {
		_ = u.Notifier.Notify(ctx, port.Event{
			Level: port.LevelWarn,
			Title: title,
			Body:  body,
			Meta:  meta,
		})
	}
}
